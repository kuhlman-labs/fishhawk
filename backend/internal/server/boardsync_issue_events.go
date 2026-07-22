package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/webhook"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// Issue-lifecycle event names the board-sync reconciler maps onto canonical
// work-item states through the repo conventions' `transitions` map (#1817).
// They are the keys a conventions config declares under `transitions`, so the
// strings must match the schema's transition-event enum. Unlike the run- and
// campaign-lifecycle edges these fire from the `issues` webhook (closed /
// reopened) with no run behind them — a hand-closed epic/ADR/throwaway has
// never been driven by a Fishhawk run — so they are driven from this separate
// entry point, auditing on the GLOBAL chain (there is no run to chain onto).
const (
	lifecycleIssueClosed   = "issue_closed"
	lifecycleIssueReopened = "issue_reopened"
)

// issueStateReasonNotPlanned / issueStateReasonDuplicate are the GitHub
// issues.closed `state_reason` values that mean "not done" — the reconciler
// deliberately LEAVES the card in place (never fights the human triage) and
// records an audited skip. Any other value (including "completed" and the
// null/absent REST default, which closes issues as completed) advances the
// card. See docs.github.com issues webhook `state_reason`.
const (
	issueStateReasonNotPlanned = "not_planned"
	issueStateReasonDuplicate  = "duplicate"
)

// issueLifecyclePayload is the subset of the `issues` webhook body the
// reconciler needs: the issue number and its close state_reason.
type issueLifecyclePayload struct {
	Issue struct {
		Number      int    `json:"number"`
		StateReason string `json:"state_reason"`
	} `json:"issue"`
}

// handleIssueLifecycleBoardSync is the ISSUE-lifecycle board-sync entry point
// (#1817): advance a hand-closed, never-run-driven issue (epic/ADR/throwaway)
// to Done on the board when it is closed, and pull a reopened card back. It is
// the sibling of boardTransitionForRun / boardTransitionForCampaignItem,
// separate because a closed issue has NO run and NO campaign — so the
// installation comes straight off the webhook Event and it audits on the
// GLOBAL chain (the repo + issue number + state_reason travel in the payload).
//
// Like the run/campaign hooks it is best-effort and every exit is a no-op-or-log:
//   - unmapped action / event not in conv.Transitions / empty conv.States =>
//     silent no-op (nothing to move).
//   - a closed-as-not_planned/duplicate issue => the provider is NOT called;
//     the card is left where it is and the deliberate leave-in-place is
//     recorded as an audited skip naming the state_reason (the NOT_PLANNED
//     disposition, #1817).
//   - a provider that does not implement Transitioner => no-op.
//   - a malformed payload / unsplittable repo => silent no-op.
//   - a genuine provider error => WARN log, no audit, no unwind (never a 5xx).
//   - a move OR a deliberate provider skip => a work_item_transitioned audit on
//     the GLOBAL chain (audit every move AND every skip, #1012).
func (s *Server) handleIssueLifecycleBoardSync(ctx context.Context, ev webhook.Event) {
	var event string
	switch ev.Action {
	case "closed":
		event = lifecycleIssueClosed
	case "reopened":
		event = lifecycleIssueReopened
	default:
		return // only closed/reopened drive the reconciler.
	}

	var p issueLifecyclePayload
	if err := json.Unmarshal(ev.RawBody, &p); err != nil || p.Issue.Number <= 0 {
		return // malformed payload or no issue number: nothing to board.
	}
	issueNum := p.Issue.Number
	stateReason := p.Issue.StateReason

	conv, err := conventionsLoader(ctx, ev.Repo)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "issue board transition: load conventions failed",
			slog.String("event", event),
			slog.String("repo", ev.Repo),
			slog.Int("issue_number", issueNum),
			slog.String("error", err.Error()))
		return
	}
	canonical, ok := conv.Transitions[event]
	if !ok || len(conv.States) == 0 {
		return // event not mapped, or no states map: no transition configured.
	}

	// NOT_PLANNED disposition (#1817): a not_planned / duplicate close is human
	// triage saying "this is not done". Leave the card wherever it is and record
	// the deliberate leave-in-place as an audited skip — never call the provider.
	if event == lifecycleIssueClosed && (stateReason == issueStateReasonNotPlanned || stateReason == issueStateReasonDuplicate) {
		s.auditIssueBoardTransition(ctx, event, ev.Repo, issueNum, stateReason, canonical, &workmgmt.TransitionResult{
			Skipped:    true,
			SkipReason: "issue closed as " + stateReason + ": left in place",
		})
		return
	}

	provider, err := workmgmt.Get(conv.Provider)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "issue board transition: resolve provider failed",
			slog.String("event", event),
			slog.String("repo", ev.Repo),
			slog.Int("issue_number", issueNum),
			slog.String("provider", conv.Provider),
			slog.String("error", err.Error()))
		return
	}
	transitioner, ok := provider.(workmgmt.Transitioner)
	if !ok {
		return // provider does not board work (e.g. jira is interface-only in v0).
	}

	owner, name, ok := splitRepoFullName(ev.Repo)
	if !ok {
		return
	}
	target := workmgmt.Target{
		Repo:    workmgmt.Repo{Owner: owner, Name: name},
		Scope:   forge.FromGitHubInstallationID(ev.InstallationID),
		Project: conv.Project,
	}

	res, err := transitioner.Transition(ctx, workmgmt.TransitionRequest{
		IssueNumber:          issueNum,
		Trigger:              event,
		Target:               target,
		CanonicalState:       canonical,
		ExpectedSourceStates: issueExpectedSourceStates(event, conv),
		States:               conv.States,
	})
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "issue board transition failed",
			slog.String("event", event),
			slog.String("repo", ev.Repo),
			slog.Int("issue_number", issueNum),
			slog.String("canonical_state", canonical),
			slog.String("error", err.Error()))
		return
	}
	s.auditIssueBoardTransition(ctx, event, ev.Repo, issueNum, stateReason, canonical, res)
}

// issueExpectedSourceStates derives the never-fight-the-human expected-source
// set for an issue-lifecycle event (#1817):
//
//   - issue_closed: EVERY canonical state declared in conv.States EXCEPT the
//     issue_closed target itself. So a card in any non-terminal column advances
//     to Done, while a card already IN the target (e.g. one a prior run_merged
//     edge already landed) falls outside the set and the provider records an
//     idempotent never-fight-the-human skip — exactly one move across the
//     run-driven-merge overlap, never a second.
//   - issue_reopened: ONLY the issue_closed transition target (falling back to
//     the canonical done state when issue_closed is unconfigured). A reopen
//     pulls a card back from Done ONLY — a card a human parked in some other
//     column is left untouched.
//
// The set is deduplicated; order is not significant to the provider.
func issueExpectedSourceStates(event string, conv workmgmt.Conventions) []string {
	closedTarget := conv.Transitions[lifecycleIssueClosed]
	if closedTarget == "" {
		closedTarget = workmgmt.CanonicalStateDone
	}

	var out []string
	switch event {
	case lifecycleIssueClosed:
		seen := map[string]bool{}
		for state := range conv.States {
			if state == closedTarget || state == "" || seen[state] {
				continue
			}
			seen[state] = true
			out = append(out, state)
		}
	case lifecycleIssueReopened:
		out = append(out, closedTarget)
	}
	return out
}

// auditIssueBoardTransition appends a work_item_transitioned entry recording
// what an issue-scoped board move did — a landed move or a deliberate skip (the
// never-fight-the-human idempotent overlap AND the not_planned/duplicate
// leave-in-place) — on the GLOBAL audit chain (a closed issue has no run; the
// repo + issue number + state_reason travel in the payload). Best-effort: a
// missing audit repo or an append error logs and returns. Audits BOTH a move
// and every skip, matching the run- and campaign-scoped hooks (#1012).
func (s *Server) auditIssueBoardTransition(ctx context.Context, event, repo string, issueNum int, stateReason, canonical string, res *workmgmt.TransitionResult) {
	if s.cfg.AuditRepo == nil || res == nil {
		return
	}
	payload, _ := json.Marshal(map[string]any{
		"trigger":         event,
		"repo":            repo,
		"issue_number":    issueNum,
		"state_reason":    stateReason,
		"canonical_state": canonical,
		"from":            res.From,
		"to":              res.To,
		"moved":           res.Moved,
		"skipped":         res.Skipped,
		"skip_reason":     res.SkipReason,
	})
	kind := audit.ActorSystem
	if _, err := s.cfg.AuditRepo.AppendGlobalChained(ctx, audit.GlobalChainAppendParams{
		Timestamp: time.Now().UTC(),
		Category:  categoryWorkItemTransitioned,
		ActorKind: &kind,
		Payload:   payload,
		// AccountID stays nil (untenanted partition): a hand-closed issue has
		// no run or campaign to carry a tenant, the webhook ctx has no request
		// Identity, and the server has no installation→account resolution
		// surface yet (ADR-057 / #1828 — the webhook Dispatcher's
		// InstallationAccountLookup seam is the wiring point once one exists).
	}); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "append issue work_item_transitioned audit",
			slog.String("event", event),
			slog.String("repo", repo),
			slog.Int("issue_number", issueNum),
			slog.String("error", err.Error()))
	}
}
