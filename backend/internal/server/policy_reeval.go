package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/policy"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/stagecheck"
)

// reevaluateCIPolicy re-fires the implement-stage policy evaluator
// after a `check_run.completed` event lands on a Fishhawk-managed
// PR's required check (#300). The trace-upload-time evaluation
// defers `ci_green` because CI hasn't started yet (#297); this
// handler closes the loop by re-emitting `policy_evaluated` with
// the latest aggregate signal once a required check terminates.
//
// Semantics (#300 design pass, per-check completion + dedup):
//
//   - Fires on every terminal `check_run.completed` event for a
//     required check.
//   - The audit row's `ci_green` value reflects the aggregate
//     across all required checks: true when all are pass-bucket,
//     false on the first fail-bucket check (failure is decisive
//     and doesn't wait for siblings), nil while some required
//     checks haven't reported yet.
//   - Dedup against the latest `policy_evaluated` row's ci_green
//     value — re-runs that don't shift the aggregate don't write
//     duplicate rows. Net effect: the audit chain records state
//     transitions, not raw event counts.
//
// Runs server-side (rather than as a dispatcher MatchAction) so it
// sees the stage_checks state ingestCheckRun just wrote. The
// dispatcher's existing CI-retry path stays as-is; this is a
// distinct concern.
//
// Best-effort throughout: failures log but never unwind the webhook
// dispatch. A missed re-eval leaves the SPA showing the prior
// state until a future event re-triggers.
func (s *Server) reevaluateCIPolicy(ctx context.Context, raw []byte) {
	if s.cfg.RunRepo == nil || s.cfg.AuditRepo == nil || s.cfg.StageCheckRepo == nil {
		return
	}

	var p checkRunPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "policy reeval: payload parse failed",
			slog.String("error", err.Error()))
		return
	}
	if p.Action != "completed" || p.CheckRun.Conclusion == nil {
		return
	}
	if p.CheckRun.Name == "" || p.Repository.FullName == "" {
		return
	}

	for _, pr := range p.CheckRun.PullRequests {
		s.reevaluateCIPolicyForPR(ctx, p.Repository.FullName, pr.Number, p.CheckRun.Name)
	}
}

// reevaluateCIPolicyForPR runs the re-eval for a single
// (PR, check) pair. Pulled out so a check_run event with multiple
// pull_requests[] entries doesn't short-circuit on the first match.
func (s *Server) reevaluateCIPolicyForPR(
	ctx context.Context,
	repoFullName string,
	prNumber int,
	checkName string,
) {
	prURL := fmt.Sprintf("https://github.com/%s/pull/%d", repoFullName, prNumber)

	parent := s.findLatestRunForPR(ctx, prURL)
	if parent == nil {
		return
	}
	if !isRequiredCheck(parent.RequiredChecksSnapshot, checkName) {
		return
	}

	implStage := s.findImplementStage(ctx, parent.ID)
	if implStage == nil {
		return
	}

	prior, err := s.latestPolicyEvaluatedPayload(ctx, parent.ID, implStage.ID)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "policy reeval: load prior policy_evaluated failed",
			slog.String("run_id", parent.ID.String()),
			slog.String("error", err.Error()))
		return
	}
	if prior == nil {
		// No prior evaluation. The trace-upload path emits one for
		// every implement stage, including the empty / skipped
		// cases, so this would only fire if the implement stage
		// has never had a trace land. Nothing to update; CI events
		// will re-trigger us once the trace handler runs.
		return
	}
	if prior.SkipReason != "" {
		// Prior evaluation was skipped (spec unavailable, no diff,
		// etc.). The skip reason hasn't changed — a CI signal
		// doesn't unblock a missing spec. Leave the chain alone.
		return
	}

	checks, err := s.cfg.StageCheckRepo.LatestForStage(ctx, implStage.ID)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "policy reeval: list stage checks failed",
			slog.String("stage_id", implStage.ID.String()),
			slog.String("error", err.Error()))
		return
	}
	newCI := aggregateCIGreen(parent.RequiredChecksSnapshot.Contexts, checks)

	if priorCIGreenEqual(prior.Applied.CIGreen, newCI) {
		return
	}

	diff := reconstructPolicyDiff(prior.Diff)
	constraints := prior.Applied
	constraints.CIGreen = newCI

	if _, err := policy.EmitEvaluation(ctx, s.cfg.AuditRepo, parent.ID, implStage.ID,
		prior.StageType, diff, constraints, nil); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "policy reeval: emit failed",
			slog.String("run_id", parent.ID.String()),
			slog.String("stage_id", implStage.ID.String()),
			slog.String("error", err.Error()))
	}
}

// findLatestRunForPR returns the most-recent run whose
// pull_request_url equals prURL. ListRuns is created_at DESC, so
// the first result wins. Returns nil for no match (PR isn't
// Fishhawk-managed, or no implement stage has landed yet).
func (s *Server) findLatestRunForPR(ctx context.Context, prURL string) *run.Run {
	runs, err := s.cfg.RunRepo.ListRuns(ctx, run.ListRunsFilter{
		PullRequestURL: &prURL,
		Limit:          1,
	})
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "policy reeval: list runs failed",
			slog.String("pull_request_url", prURL),
			slog.String("error", err.Error()))
		return nil
	}
	if len(runs) == 0 {
		return nil
	}
	return runs[0]
}

// findImplementStage returns the implement stage for the run, or
// nil when none exists.
func (s *Server) findImplementStage(ctx context.Context, runID uuid.UUID) *run.Stage {
	stages, err := s.cfg.RunRepo.ListStagesForRun(ctx, runID)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "policy reeval: list stages failed",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()))
		return nil
	}
	for i := range stages {
		if stages[i].Type == run.StageTypeImplement {
			return stages[i]
		}
	}
	return nil
}

// latestPolicyEvaluatedPayload walks the run's `policy_evaluated`
// audit rows newest-first and returns the decoded payload of the
// latest row scoped to the given stage. Returns (nil, nil) when
// the stage has no prior evaluation; (nil, err) only on transport
// failure.
func (s *Server) latestPolicyEvaluatedPayload(ctx context.Context, runID, stageID uuid.UUID) (*policy.EvaluationPayload, error) {
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, policy.CategoryPolicyEvaluated)
	if err != nil {
		return nil, err
	}
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		if e.StageID == nil || *e.StageID != stageID {
			continue
		}
		var pl policy.EvaluationPayload
		if err := json.Unmarshal(e.Payload, &pl); err != nil {
			// Defensive: a malformed row shouldn't block the
			// re-eval. Treat as "no prior" and let the trace
			// handler reseed if it ever fires again.
			continue
		}
		return &pl, nil
	}
	return nil, nil
}

// aggregateCIGreen folds the latest stage_checks state for a set
// of required check names into a single ci_green value.
//
//   - true: every required check has reported and every one is in
//     the pass bucket.
//   - false: at least one required check is in the fail bucket
//     (failure is decisive — we don't wait for siblings).
//   - nil:  no fails recorded yet but at least one required check
//     hasn't reported terminally.
//
// `fishhawk_audit_complete` is excluded — it's Fishhawk's own
// derived check (#229) and would create a circular dependency if
// folded into ci_green.
func aggregateCIGreen(required []string, checks []*stagecheck.Check) *bool {
	latestByName := make(map[string]*stagecheck.Check, len(checks))
	for _, c := range checks {
		latestByName[c.Name] = c
	}
	sawFail := false
	sawPending := false
	for _, name := range required {
		if name == auditCompleteCheckName {
			continue
		}
		c, ok := latestByName[name]
		if !ok {
			sawPending = true
			continue
		}
		switch stagecheck.DeriveState(c.Status, c.Conclusion) {
		case stagecheck.StatePass:
			// keep walking
		case stagecheck.StateFail:
			sawFail = true
		default:
			sawPending = true
		}
	}
	if sawFail {
		f := false
		return &f
	}
	if sawPending {
		return nil
	}
	t := true
	return &t
}

// auditCompleteCheckName is the name of Fishhawk's own derived
// check (#229 / #231). Excluded from ci_green aggregation to
// avoid a circular dependency: audit-complete depends on the
// policy evaluation that depends on ci_green.
const auditCompleteCheckName = "fishhawk_audit_complete"

// isRequiredCheck reports whether `name` is listed in the run's
// required-checks snapshot. Returns false for nil snapshot (legacy
// rows pre-#251) — without a snapshot we can't decide what's
// required, so we don't re-evaluate.
func isRequiredCheck(snap *run.RequiredChecksSnapshot, name string) bool {
	if snap == nil {
		return false
	}
	for _, c := range snap.Contexts {
		if c == name {
			return true
		}
	}
	return false
}

// priorCIGreenEqual is a tristate equality check for the dedup
// branch — both nil = equal; one nil = different; both set =
// pointer-deref compare.
func priorCIGreenEqual(a, b *bool) bool {
	switch {
	case a == nil && b == nil:
		return true
	case a == nil || b == nil:
		return false
	default:
		return *a == *b
	}
}

// reconstructPolicyDiff rebuilds a policy.Diff from the prior
// audit row's payload. The prior row is the canonical record of
// the stage's diff (the trace bundle that produced it may have
// been garbage-collected by retention), so reading it back is
// cheaper and consistent with the on-chain history.
func reconstructPolicyDiff(entries []policy.DiffEntry) policy.Diff {
	files := make([]policy.ChangedFile, 0, len(entries))
	for _, e := range entries {
		files = append(files, policy.ChangedFile(e))
	}
	return policy.Diff{ChangedFiles: files}
}
