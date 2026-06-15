package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// Run-lifecycle event names the board-sync hook maps onto canonical work-item
// states through the repo conventions' `transitions` map (#1012). They are the
// keys a conventions config declares under `transitions`, so the strings must
// match the schema's transition-event enum.
const (
	lifecycleRunStarted = "run_started"
	lifecyclePROpened   = "pr_opened"
	lifecycleRunFailed  = "run_failed"
	lifecycleRunMerged  = "run_merged"
)

// categoryWorkItemTransitioned is the audit category written for every
// board-state move the run-lifecycle hook attempts — both the moves it lands
// and the never-fight-the-human skips (#1012). Documented in
// docs/issue-comment-surfaces.md. It is an internal audit-only category, not
// an issue-comment surface.
const categoryWorkItemTransitioned = "work_item_transitioned"

// lifecyclePredecessors names, for each lifecycle event, the prior lifecycle
// events whose configured target states are the expected source for this
// event's move. The board-sync hook resolves these through the conventions'
// `transitions` map to build the never-fight-the-human expected-source set:
// a card is advanced only from a status a prior lifecycle edge would have left
// it in. run_started has no predecessor edge — its expected source is the
// backlog/unset entry state, added explicitly by expectedSourceStates.
var lifecyclePredecessors = map[string][]string{
	lifecycleRunStarted: nil,
	lifecyclePROpened:   {lifecycleRunStarted},
	lifecycleRunFailed:  {lifecycleRunStarted, lifecyclePROpened},
	lifecycleRunMerged:  {lifecyclePROpened, lifecycleRunStarted},
}

// NotifyBoardTransition is the exported webhook.BoardSyncer entrypoint the
// dispatcher calls on the run_started edge (#1012). It delegates to the
// unexported best-effort hook used by the in-process lifecycle call sites.
func (s *Server) NotifyBoardTransition(ctx context.Context, runID uuid.UUID, event string) {
	s.notifyBoardTransition(ctx, runID, event)
}

// notifyBoardTransition is the best-effort board-state-sync hook (#1012),
// modelled on notifyStatusUpdate: it advances the work item backing an
// issue-triggered run along a run-lifecycle edge and NEVER unwinds the run.
// Every exit is a no-op-or-log:
//   - no run repo / run lookup failure / non-issue trigger / unmapped event /
//     unconfigured states map => silent no-op (nothing to move).
//   - a provider that does not implement the Transitioner capability => no-op.
//   - a genuine provider error => WARN log, no audit, no unwind.
//   - a move OR a deliberate skip => a work_item_transitioned audit entry on
//     the run (condition (4): audit every move AND every skip).
//
// The never-fight-the-human guard lives in the provider (it only advances a
// card whose current status is in ExpectedSourceStates); this hook supplies the
// expected-source set derived from the configured transitions.
func (s *Server) notifyBoardTransition(ctx context.Context, runID uuid.UUID, event string) {
	if s.cfg.RunRepo == nil {
		return
	}
	rn, err := s.cfg.RunRepo.GetRun(ctx, runID)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "board transition: get run failed",
			slog.String("event", event),
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()))
		return
	}
	s.boardTransitionForRun(ctx, rn, event)
}

// boardTransitionForRun is the run-resolved core of the hook, split out so the
// webhook dispatcher's run_started path (which already holds the created run)
// can drive it without a redundant GetRun.
func (s *Server) boardTransitionForRun(ctx context.Context, rn *run.Run, event string) {
	if rn == nil || rn.TriggerRef == nil {
		return // not issue-triggered: nothing to board.
	}
	issueNum, ok := parseIssueTriggerRef(*rn.TriggerRef)
	if !ok {
		return
	}

	conv, err := conventionsLoader(rn.Repo)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "board transition: load conventions failed",
			slog.String("event", event),
			slog.String("run_id", rn.ID.String()),
			slog.String("error", err.Error()))
		return
	}
	canonical, ok := conv.Transitions[event]
	if !ok || len(conv.States) == 0 {
		return // event not mapped, or no states map: no transition configured.
	}

	provider, err := workmgmt.Get(conv.Provider)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "board transition: resolve provider failed",
			slog.String("event", event),
			slog.String("run_id", rn.ID.String()),
			slog.String("provider", conv.Provider),
			slog.String("error", err.Error()))
		return
	}
	transitioner, ok := provider.(workmgmt.Transitioner)
	if !ok {
		return // provider does not board work (e.g. jira is interface-only in v0).
	}

	owner, name, ok := splitRepoFullName(rn.Repo)
	if !ok {
		return
	}
	target := workmgmt.Target{
		Repo:    workmgmt.Repo{Owner: owner, Name: name},
		Project: conv.Project,
	}
	if rn.InstallationID != nil {
		target.InstallationID = *rn.InstallationID
	}

	res, err := transitioner.Transition(ctx, workmgmt.TransitionRequest{
		IssueNumber:          issueNum,
		Trigger:              event,
		Target:               target,
		CanonicalState:       canonical,
		ExpectedSourceStates: expectedSourceStates(event, conv),
		States:               conv.States,
	})
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "board transition failed",
			slog.String("event", event),
			slog.String("run_id", rn.ID.String()),
			slog.Int("issue_number", issueNum),
			slog.String("canonical_state", canonical),
			slog.String("error", err.Error()))
		return
	}
	s.auditBoardTransition(ctx, rn, event, issueNum, canonical, res)
}

// auditBoardTransition appends a work_item_transitioned entry recording what the
// transition did — a landed move or a deliberate skip — onto the run. It is
// best-effort: a missing audit repo or an append error logs and returns. Unlike
// work_item_filed, this is NOT gated on the run being non-terminal: run_merged
// and run_failed fire exactly as the run reaches a terminal state, so gating
// them out would silence the two most meaningful board moves.
func (s *Server) auditBoardTransition(ctx context.Context, rn *run.Run, event string, issueNum int, canonical string, res *workmgmt.TransitionResult) {
	if s.cfg.AuditRepo == nil || rn == nil || res == nil {
		return
	}
	payload, _ := json.Marshal(map[string]any{
		"trigger":         event,
		"issue_number":    issueNum,
		"canonical_state": canonical,
		"from":            res.From,
		"to":              res.To,
		"moved":           res.Moved,
		"skipped":         res.Skipped,
		"skip_reason":     res.SkipReason,
	})
	kind := audit.ActorSystem
	if _, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     rn.ID,
		Timestamp: time.Now().UTC(),
		Category:  categoryWorkItemTransitioned,
		ActorKind: &kind,
		Payload:   payload,
	}); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "append work_item_transitioned audit",
			slog.String("event", event),
			slog.String("run_id", rn.ID.String()),
			slog.String("error", err.Error()))
	}
}

// expectedSourceStates derives the never-fight-the-human expected-source set for
// a lifecycle event from the configured transitions: the canonical states the
// prior lifecycle edges target. run_started additionally accepts the
// backlog/unset entry state (the provider treats an unset Status as Backlog).
// The set is deduplicated; order is not significant to the provider.
func expectedSourceStates(event string, conv workmgmt.Conventions) []string {
	var out []string
	seen := map[string]bool{}
	add := func(state string) {
		if state != "" && !seen[state] {
			seen[state] = true
			out = append(out, state)
		}
	}
	if event == lifecycleRunStarted {
		add(workmgmt.CanonicalStateBacklog)
	}
	for _, pred := range lifecyclePredecessors[event] {
		if target, ok := conv.Transitions[pred]; ok {
			add(target)
		}
	}
	return out
}

// parseIssueTriggerRef pulls the numeric issue number out of an "issue:42"
// TriggerRef (the shape the dispatcher writes for issue triggers). Returns
// (0, false) for any other shape — ad-hoc/CLI runs carry no issue to board.
func parseIssueTriggerRef(ref string) (int, bool) {
	rest, ok := strings.CutPrefix(ref, "issue:")
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(rest)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}
