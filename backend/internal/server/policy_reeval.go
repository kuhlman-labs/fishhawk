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
// sees the stage_checks state ingestCheckRun just wrote. The checks
// are READ from the run's REVIEW stage (findCISignalStage, #3489) —
// the stage ingestCheckRun writes against — while the evaluation
// itself (the prior-payload lookup and the appended
// `policy_evaluated` row) stays anchored to the IMPLEMENT stage.
// The dispatcher's existing CI-retry path stays as-is; this is a
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
	s.reevaluateCIPolicyForRun(ctx, parent, checkName)
}

// reevaluateCIPolicyForRun runs the post-CI re-eval for ONE already-
// resolved run and the check that just terminated. It holds everything
// from the required-check filter onward; reevaluateCIPolicyForPR is the
// GitHub caller (resolving the run by PR URL), and the GitLab pipeline
// ingester (gitlab_pipeline.go) calls it once per run whose review
// stage received a `gitlab/pipeline` row — the ingester already holds
// the run (it read the snapshot flag from it), and a GitLab run's
// pull_request_url is not the lookup key the pipeline hook carries
// (E45.55 / #3490). Same best-effort contract as the caller.
func (s *Server) reevaluateCIPolicyForRun(ctx context.Context, parent *run.Run, checkName string) {
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

	// The CI signal is read from the review stage — the stage
	// ingestCheckRun appends rows to — NOT the implement stage the
	// evaluation is anchored to (#3489).
	ciStage := s.findCISignalStage(ctx, parent.ID)
	if ciStage == nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "policy reeval: run has no review stage to read CI checks from",
			slog.String("run_id", parent.ID.String()))
		return
	}
	checks, err := s.cfg.StageCheckRepo.LatestForStage(ctx, ciStage.ID)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "policy reeval: list stage checks failed",
			slog.String("stage_id", ciStage.ID.String()),
			slog.String("error", err.Error()))
		return
	}
	newCI := aggregateCIGreen(parent.RequiredChecksSnapshot, checks)

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
// nil when none exists. It anchors the policy evaluation (the
// prior-payload lookup and the appended `policy_evaluated` row);
// it is NOT the stage the CI signal is read from — see
// findCISignalStage.
func (s *Server) findImplementStage(ctx context.Context, runID uuid.UUID) *run.Stage {
	return s.findStageOfType(ctx, runID, run.StageTypeImplement)
}

// findCISignalStage returns the stage whose stage_checks rows carry
// the run's GitHub CI signal, or nil when the run has none.
//
// GitHub `check_run` rows are appended by ingestCheckRun against the
// run's REVIEW stage(s): stagecheck/queries.sql's
// FindRunStagesForCheckRun filters `s.stage_type = 'review'` (#254),
// because the review stage is the only one whose gate is tied to
// merge state. Every reader of LatestForStage for the CI signal —
// the deploy gate's ci_green verdict and the post-CI policy re-eval —
// MUST resolve its stage through this helper; reading any other
// stage (the implement stage, as both readers did before #3489)
// reads a stage that never receives a row, so the signal is
// permanently pending.
//
// The FIRST review stage in sequence order is returned. The ingester
// appends to EVERY review stage the query returns, so any of them
// carries the same rows; the first is simply the deterministic pick.
func (s *Server) findCISignalStage(ctx context.Context, runID uuid.UUID) *run.Stage {
	return s.findStageOfType(ctx, runID, run.StageTypeReview)
}

// findStageOfType returns the first stage of type t on the run in
// the repository's sequence order, or nil when none exists or the
// listing fails (logged at WARN).
func (s *Server) findStageOfType(ctx context.Context, runID uuid.UUID, t run.StageType) *run.Stage {
	stages, err := s.cfg.RunRepo.ListStagesForRun(ctx, runID)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "policy reeval: list stages failed",
			slog.String("run_id", runID.String()),
			slog.String("stage_type", string(t)),
			slog.String("error", err.Error()))
		return nil
	}
	for i := range stages {
		if stages[i].Type == t {
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

// aggregateCIGreen folds the latest stage_checks state for a run's
// required-checks snapshot into a single ci_green value.
//
//   - true:  a present snapshot whose every required check has
//     reported and is in the pass bucket. A present-but-EMPTY
//     Contexts list also folds to true — a repository that
//     genuinely declares zero required checks is vacuously green.
//   - false: at least one required check is in the fail bucket
//     (failure is decisive — we don't wait for siblings).
//   - nil:   UNKNOWN. Either the snapshot is absent (snap == nil —
//     "we never looked up branch protection"; an absent snapshot is
//     NOT a pass, #2497), or no fails are recorded yet but at least
//     one declared required check hasn't reported terminally.
//
// The nil-snapshot arm is the #2497 fix: it distinguishes "zero
// required checks are declared" (a present empty snapshot → green)
// from "we never looked" (absent snapshot → unknown). Previously the
// caller passed snap.Contexts, so a nil snapshot folded to the same
// vacuous true as an empty declared list.
//
// `fishhawk_audit_complete` is excluded — it's Fishhawk's own
// derived check (#229) and would create a circular dependency if
// folded into ci_green.
func aggregateCIGreen(snap *run.RequiredChecksSnapshot, checks []*stagecheck.Check) *bool {
	if snap == nil {
		return nil
	}
	latestByName := make(map[string]*stagecheck.Check, len(checks))
	for _, c := range checks {
		latestByName[c.Name] = c
	}
	sawFail := false
	sawPending := false
	for _, name := range snap.Contexts {
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
