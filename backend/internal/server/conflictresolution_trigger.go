package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// conflictResolutionCeiling is the absolute number of conflict-resolution
// passes ONE implement stage may ever consume (E64.62 / #3202). It is
// deliberately 1 and deliberately NOT operator-overridable: the pass resolves
// a merge conflict MECHANICALLY inside the hunks git itself marked, so a
// second attempt against the same conflict has no new information and would
// only widen the blast radius. When it is spent the verb returns to today's
// fail-closed 422 naming the resolve-push-vouch route.
//
// The counter is the run's stage_conflict_resolution_triggered entries for
// that stage. It is a DISTINCT category from stage_fixup_triggered, which is
// what makes the budget separation STRUCTURAL rather than asserted: the fix-up
// counter never reads this category and this counter never reads the fix-up
// one, so a conflict-resolution pass can never spend a fix-up slot.
const conflictResolutionCeiling = 1

// conflictResolutionTriggeredNote is the constant sentence shipped on every
// 202. It states the two facts an operator reading a 202 must not have to
// infer — that NOTHING was written to the branch by this call, and what the
// route back is — so a 202 is never mistaken for a completed advance.
const conflictResolutionTriggeredNote = "The base merge CONFLICTED, so NOTHING was written to the run branch by this call — no merge commit, no audit-complete re-post. A bounded, agent-driven conflict-resolution pass (ceiling 1) has been authorized instead: the implement stage is re-opened and the runner performs the merge LOCALLY on the run branch, resolves only inside the conflicted hunks, and pushes the merge commit through the App installation (ADR-035 sole writer). Await the stage with fishhawk_await_stage, then re-invoke fishhawk_rebase_run_branch: on success the behind-probe short-circuits and the fishhawk_audit_complete check is re-posted at the resulting head. If the pass is REFUSED the run is restored to its pre-pass review gate and the next invocation fails closed naming the refusal."

// conflictResolutionRoute is the shipped fallback route named by every
// fail-closed conflict refusal. It is a constant because both the
// budget-spent arm and the cannot-start arm must name the SAME route — an
// operator who reads one and not the other must not learn a different answer.
const conflictResolutionRoute = "Resolve the conflict in a worktree, push, then admit the pushed head with fishhawk_vouch_commit. (fishhawk_reset_run_branch is the sibling verb for a FOREIGN COMMIT pushed ON TOP of the run's commits — a different problem.)"

// conflictResolutionStart is what a SUCCESSFULLY started pass reports back to
// the rebase handler: the re-opened implement stage and which pass ordinal
// this was.
type conflictResolutionStart struct {
	StageID uuid.UUID
	Pass    int
}

// conflictResolutionRefusal is the negative result: no pass could be started,
// and WHY. Reason is always populated. BudgetSpent distinguishes the
// ceiling arm — the one that must name the FAILED pass and its refusal reason
// — from every "no pass is startable at all" arm.
type conflictResolutionRefusal struct {
	Reason       string
	BudgetSpent  bool
	FailedPass   int
	FailedReason string
}

// startConflictResolutionPass is the rebase verb's 202 arm (E64.62 / #3202).
//
// It replaces the outright rebase_conflict refusal with a BUDGETED
// authorization: under the ceiling it records the durable trigger, re-opens
// the implement stage, and reports back so the handler can answer 202. At or
// over the ceiling — and on every case in which no pass can be started at all
// — it returns a refusal and the handler keeps TODAY'S fail-closed 422.
//
// ORDERING IS LOAD-BEARING, and it is APPEND-THEN-REOPEN rather than the
// reverse. The trigger entry is BOTH the durable budget counter and the
// runner's instruction; the re-open is what makes the local loop dispatch the
// stage. Of the two possible half-failures:
//
//   - append-then-reopen: a failed re-open leaves a SPENT budget slot and no
//     dispatch. The stage is untouched, nothing runs, and the operator's next
//     invocation takes the budget-spent 422 and the resolve-push-vouch route.
//     Loud, fail-closed, and costs one wasted slot.
//   - reopen-then-append: a failed append leaves the implement stage PENDING
//     with no instruction, so the loop dispatches an ORDINARY full implement
//     re-run against the run branch — an unauthorized agent pass doing
//     arbitrary work, which is strictly worse than a wasted slot. There is
//     also no undo: run.RestoreFixupStage only accepts a `failed` stage, so a
//     successful re-open cannot be walked back.
//
// So the append runs first, and the payload is therefore made COMPLETE before
// any mutation: PriorState and the review stage to re-park are derived here
// (mirroring run.FixupStage's own non-acceptance-mode selection) rather than
// read out of the FixupStage decision afterwards.
func (s *Server) startConflictResolutionPass(r *http.Request, runID uuid.UUID,
	branch, baseRef, headSHA string) (*conflictResolutionStart, *conflictResolutionRefusal) {
	ctx := r.Context()
	if s.cfg.RunRepo == nil || s.cfg.AuditRepo == nil {
		return nil, &conflictResolutionRefusal{Reason: "no run/audit repository is wired, so no conflict-resolution pass can be authorized"}
	}
	// Every anchor the runner needs must be resolvable BEFORE the budget is
	// spent — a half-populated trigger would have the runner merge nothing and
	// refuse naming the wrong cause, burning the ceiling-of-one on a serve bug.
	if branch == "" || baseRef == "" || headSHA == "" {
		return nil, &conflictResolutionRefusal{Reason: "the run branch, base ref or head sha could not be resolved, so no conflict-resolution pass can be anchored"}
	}

	impl := s.findImplementStage(ctx, runID)
	if impl == nil {
		return nil, &conflictResolutionRefusal{Reason: "the run has no implement stage to re-open, so no conflict-resolution pass can be started"}
	}

	// BUDGET. Count every prior trigger for this stage — a trigger CONSUMED by
	// a later failure still counts, because the pass it authorized ran. The
	// failure entry only supplies the wording.
	prior, ok := s.countStageEntries(ctx, runID, impl.ID, CategoryStageConflictResolutionTriggered)
	if !ok {
		return nil, &conflictResolutionRefusal{Reason: "the conflict-resolution budget could not be read from the audit chain; refusing to authorize a pass on an uncertain count"}
	}
	if prior >= conflictResolutionCeiling {
		return nil, s.conflictResolutionBudgetSpent(ctx, runID, impl.ID, prior)
	}

	// PRE-MUTATION payload derivation, so the appended trigger is complete.
	priorState := impl.State
	if priorState != run.StageStateSucceeded && priorState != run.StageStateAwaitingApproval {
		return nil, &conflictResolutionRefusal{Reason: fmt.Sprintf(
			"the implement stage is in state %q; only a stage parked at its review gate (succeeded or awaiting_approval) can be re-opened for a conflict-resolution pass", priorState)}
	}
	reviewStageID := ""
	if priorState == run.StageStateSucceeded {
		// push_and_open_pr flow: the human gate is a SEPARATE review stage that
		// FixupStage will re-park. Record it so the failure recovery restores it.
		if review := s.openReviewStageFor(ctx, runID); review != nil {
			reviewStageID = review.ID.String()
		}
	}

	pass := prior + 1
	trigger := conflictResolutionTrigger{
		Branch:                branch,
		BaseRef:               baseRef,
		ExpectedHeadSHA:       headSHA,
		Pass:                  pass,
		PriorState:            string(priorState),
		ReparkedReviewStageID: reviewStageID,
	}
	if !trigger.populated() {
		return nil, &conflictResolutionRefusal{Reason: "the conflict-resolution trigger payload could not be fully populated, so no pass was authorized"}
	}
	if err := s.writeConflictResolutionTriggeredAudit(r, runID, impl.ID, trigger); err != nil {
		return nil, &conflictResolutionRefusal{Reason: "recording the conflict-resolution trigger failed (" + err.Error() + "), so no pass was authorized and nothing was written to the branch"}
	}

	// RE-OPEN. run.FixupStage owns the state-machine edge (implement →
	// pending, plus the review re-park on the push_and_open_pr flow) and the
	// terminal-run guard. The pass counts we hand it are the CONFLICT-
	// RESOLUTION counts, never the fix-up ones, so this call can neither read
	// nor spend the fix-up budget.
	if _, err := run.FixupStage(ctx, s.cfg.RunRepo, impl.ID, run.FixupOptions{
		PriorPassCount: prior,
		MaxPasses:      conflictResolutionCeiling,
		HardCeiling:    conflictResolutionCeiling,
	}); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"conflict resolution: re-opening the implement stage failed; the trigger is recorded and the budget slot is spent",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", impl.ID.String()),
			slog.String("error", err.Error()))
		return nil, &conflictResolutionRefusal{Reason: "re-opening the implement stage for the conflict-resolution pass failed (" + err.Error() +
			"); the trigger is recorded, so the ceiling-of-one budget is now SPENT and no pass will run"}
	}

	s.notifyStatusUpdate(ctx, runID, CategoryStageConflictResolutionTriggered)
	return &conflictResolutionStart{StageID: impl.ID, Pass: pass}, nil
}

// conflictResolutionBudgetSpent builds the ceiling refusal. It reads the
// NEWEST same-stage stage_conflict_resolution_failed entry so the 422 can name
// the FAILED pass and the runner's own refusal reason — the operator asked
// whether a mechanical resolution was possible and is owed the answer, not a
// bare "budget spent". An unreadable or undecodable failure entry degrades to
// the count alone; it never suppresses the refusal.
func (s *Server) conflictResolutionBudgetSpent(ctx context.Context, runID, stageID uuid.UUID, prior int) *conflictResolutionRefusal {
	ref := &conflictResolutionRefusal{
		BudgetSpent: true,
		FailedPass:  prior,
		Reason: fmt.Sprintf("the bounded conflict-resolution budget is SPENT (%d of %d passes used)",
			prior, conflictResolutionCeiling),
	}
	failure, ok := s.newestStageEntry(ctx, runID, stageID, CategoryStageConflictResolutionFailed)
	if !ok || failure == nil {
		return ref
	}
	var payload struct {
		Pass   int    `json:"pass"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(failure.Payload, &payload); err != nil {
		return ref
	}
	if payload.Pass > 0 {
		ref.FailedPass = payload.Pass
	}
	ref.FailedReason = payload.Reason
	return ref
}

// message renders the refusal into the sentence shipped on the 422. The
// budget-spent arm names the failed pass, its refusal reason and the
// resolve-push-vouch route; every other arm names why no pass was startable
// and the same route.
func (ref *conflictResolutionRefusal) message() string {
	if !ref.BudgetSpent {
		return "the run branch conflicts with the advanced base; the merge was REFUSED and NOTHING was written (no merge commit, no audit entry, no check re-post). No conflict-resolution pass could be started: " +
			ref.Reason + ". " + conflictResolutionRoute
	}
	msg := fmt.Sprintf("the run branch conflicts with the advanced base; the merge was REFUSED and NOTHING was written (no merge commit, no audit entry, no check re-post). Conflict-resolution pass %d already ran and was REFUSED", ref.FailedPass)
	if ref.FailedReason != "" {
		msg += " (" + ref.FailedReason + ")"
	}
	msg += ", so " + ref.Reason + " and no further pass will be authorized. " + conflictResolutionRoute
	return msg
}

// details is the structured half of the 422, so a machine reader does not have
// to parse the sentence.
func (ref *conflictResolutionRefusal) details(branch, baseRef string, mergeErr error) map[string]any {
	d := map[string]any{
		"branch":                        branch,
		"base_ref":                      baseRef,
		"conflict_resolution_available": false,
		"conflict_resolution_reason":    ref.Reason,
	}
	if mergeErr != nil {
		d["error"] = mergeErr.Error()
	}
	if ref.BudgetSpent {
		d["conflict_resolution_budget_spent"] = true
		d["conflict_resolution_failed_pass"] = ref.FailedPass
		if ref.FailedReason != "" {
			d["conflict_resolution_failure_reason"] = ref.FailedReason
		}
	}
	return d
}

// countStageEntries counts the run's entries of a category bound to stageID.
// The bool reports whether the READ succeeded: a listing error must never be
// read as a count of zero, because the caller treats zero as budget headroom.
func (s *Server) countStageEntries(ctx context.Context, runID, stageID uuid.UUID, category string) (int, bool) {
	if s.cfg.AuditRepo == nil {
		return 0, false
	}
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, category)
	if err != nil {
		return 0, false
	}
	n := 0
	for _, e := range entries {
		if e.StageID != nil && *e.StageID == stageID {
			n++
		}
	}
	return n, true
}

// openReviewStageFor returns the run's review stage while it is still parked
// at awaiting_approval — the stage run.FixupStage re-parks on the
// push_and_open_pr flow, and therefore the one the failure recovery must
// restore. Nil when there is none (the commit-yourself flow, or a review gate
// that has already closed); FixupStage itself refuses that case, so this
// helper deliberately does not.
func (s *Server) openReviewStageFor(ctx context.Context, runID uuid.UUID) *run.Stage {
	stages, err := s.cfg.RunRepo.ListStagesForRun(ctx, runID)
	if err != nil {
		return nil
	}
	for _, st := range stages {
		if st.Type == run.StageTypeReview && st.State == run.StageStateAwaitingApproval {
			return st
		}
	}
	return nil
}

// writeConflictResolutionTriggeredAudit appends the durable trigger entry.
// Unlike most audit writes in this package it is NOT best-effort: the entry IS
// the budget counter and the runner's instruction, so the error is returned
// and the caller refuses rather than authorizing a pass no reader can justify.
// The actor is the OPERATOR — the pass exists only because an operator invoked
// the rebase verb.
func (s *Server) writeConflictResolutionTriggeredAudit(r *http.Request, runID, stageID uuid.UUID,
	trigger conflictResolutionTrigger) error {
	id := IdentityFrom(r.Context())
	subject := id.Subject
	if subject == "" {
		subject = "anonymous"
	}
	actorKind := audit.ActorUser
	payload, err := json.Marshal(trigger)
	if err != nil {
		return err
	}
	if _, err := s.cfg.AuditRepo.AppendChained(r.Context(), audit.ChainAppendParams{
		RunID:        runID,
		StageID:      &stageID,
		Timestamp:    time.Now().UTC(),
		Category:     CategoryStageConflictResolutionTriggered,
		ActorKind:    &actorKind,
		ActorSubject: &subject,
		Payload:      payload,
	}); err != nil {
		s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn,
			"conflict resolution: append stage_conflict_resolution_triggered audit entry failed; no pass authorized",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("error", err.Error()))
		return err
	}
	return nil
}

// writeRebaseConflictRefusal ships the fail-closed 422 — today's
// rebase_conflict behaviour, now reached only when NO pass could be started.
// Because the conflict arm otherwise returns 202, this refusal is normally
// observable on the NEXT rebase invocation rather than the first.
func (s *Server) writeRebaseConflictRefusal(w http.ResponseWriter, r *http.Request,
	ref *conflictResolutionRefusal, branch, baseRef string, mergeErr error) {
	s.writeError(w, r, http.StatusUnprocessableEntity, "rebase_conflict",
		ref.message(), ref.details(branch, baseRef, mergeErr))
}
