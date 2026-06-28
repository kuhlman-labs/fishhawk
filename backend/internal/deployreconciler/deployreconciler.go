// Package deployreconciler runs the background ticker that polls a
// delegating deploy stage's external GitHub Actions run to a terminal
// outcome and resolves the stage (#1386 / E23.6, ADR-038).
//
// The deploy executor is server-side, NOT a blocking runner subprocess
// (operator-ratified architecture, #1386): the awaiting_deployment park
// state is explicitly non-settled (run.IsSettled excludes it), and the
// dispatch/poll primitives plus the App installation token are
// backend-only. So the deploy stage parks at awaiting_deployment after
// slice-1's trigger fires the external pipeline, and THIS ticker — the
// deploy-side analogue of the mergereconciler — walks each parked stage,
// reads the external run handle slice-1 stored in the deployment_dispatched
// audit entry, polls the GitHub Actions run, and on a terminal conclusion
// resolves the stage through the server's ResolveDeploymentFromPollState.
//
// ONLY github_actions targets are polled here. A webhook delegate target
// has no standard status API, so its terminal outcome arrives via the
// external pipeline calling back into POST /v0/runs/{run_id}/deployment
// (#1395) — the reconciler skips webhook stages.
//
// Each tick runs TWO distinct scans. The first (above) walks
// awaiting_deployment stages for the FORWARD deploy. The second (#1398 /
// #1386 binding condition 2) walks deploy stages with a pending ROLLBACK —
// those carrying a deployment_rollback_initiated audit entry with no matching
// deployment_rollback_completed. A rolled-back deploy stage is ALREADY terminal
// (succeeded/failed), so it never appears in the awaiting_deployment scan; the
// rollback scan is keyed on the rollback HANDLE (audit), polls the rollback run
// via the slice-1 fishhawk_rollback-extended correlation token (so it never
// mis-associates the forward deploy run), and on a terminal conclusion records
// rolled_back + deployment_rollback_completed through
// ResolveDeploymentRollbackFromPollState. This closes the gap where a
// github_actions rollback pipeline that never calls back would leave the
// rollback un-finalized.
//
// Correlation safety (binding condition 1, #1386): when slice-1's
// best-effort run-id resolution came up empty (GitHub's run listing is
// eventually consistent), the handle's gha_run_id is 0. The reconciler
// re-resolves via ResolveDispatchedRun, whose PRIMARY match is the
// fishhawk_run_id+fishhawk_stage_id correlation token echoed in the
// workflow_dispatch inputs. An ambiguous fallback (multiple concurrent
// dispatch runs on the same branch with no echoed inputs) resolves to
// INDETERMINATE — the stage stays parked and NO outcome is recorded
// rather than a wrong external run being associated.
//
// Per-stage errors WARN-log and never abort the tick; a transient poll
// error leaves the stage parked and is retried next tick (NOT a
// terminal-fail). Mirrors the mergereconciler ticker shape: Run() blocks
// until ctx is cancelled.
package deployreconciler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// DefaultInterval is the tick period fishhawkd applies when the caller
// leaves Interval zero.
const DefaultInterval = 60 * time.Second

// categoryDeploymentDispatched is the audit category slice-1's trigger
// writes the external run handle into; the reconciler reads it back. Kept
// as a local const (not imported from the server package) so this package
// stays free of a server dependency, mirroring the mergereconciler's
// posture. It MUST match server.CategoryDeploymentDispatched.
const categoryDeploymentDispatched = "deployment_dispatched"

// categoryDeploymentRollbackInitiated / categoryDeploymentRollbackCompleted
// are the rollback handle's audit categories (#1398, #1386 binding condition
// 2). The rollback scan reads the initiated entry's handle (the DISTINCT
// rollback run, separate from the forward deploy's deployment_dispatched) and
// the completed entry is what marks a rollback terminal. Local consts for the
// same no-server-dependency reason; they MUST match
// server.CategoryDeploymentRollbackInitiated / …Completed.
const (
	categoryDeploymentRollbackInitiated = "deployment_rollback_initiated"
	categoryDeploymentRollbackCompleted = "deployment_rollback_completed"
)

// rollbackCorrelationMarker is the workflow_dispatch input slice-1's rollback
// trigger (deploy_rollback.go::dispatchRollbackGitHubActions) injects to
// distinguish the rollback run from the forward deploy run. Both echo
// fishhawk_run_id + fishhawk_stage_id, so the rollback re-resolve MUST include
// this marker or ResolveDispatchedRun's primary match could associate the
// wrong (forward) run. MUST match server.rollbackDispatchInput.
const rollbackCorrelationMarker = "fishhawk_rollback"

// WorkflowRunPoller reads a dispatched GitHub Actions run's live state and
// re-resolves a dispatched run from its correlation token. Satisfied by
// *githubclient.Client. Tests inject a stub returning canned run states.
type WorkflowRunPoller interface {
	GetWorkflowRun(ctx context.Context, installationID int64, repo githubclient.RepoRef, runID int64) (*githubclient.WorkflowRun, error)
	ResolveDispatchedRun(ctx context.Context, installationID int64, repo githubclient.RepoRef, branch string, correlation map[string]string, createdAfter time.Time) (*githubclient.WorkflowRun, error)
}

// AuditReader reads the deployment_dispatched handle back for a parked
// deploy stage. Satisfied by audit.Repository.
type AuditReader interface {
	ListForRunByCategory(ctx context.Context, runID uuid.UUID, category string) ([]*audit.Entry, error)
}

// DeployStageSource lists deploy stages parked at awaiting_deployment and
// reads their run row. A NARROW capability interface, deliberately NOT the
// broad run.Repository: the awaiting-deployment listing is a method only the
// deploy executor needs, so keeping it off run.Repository avoids forcing a
// stub into every run.Repository test fake across the backend. Satisfied by
// the production *run.postgresRepo (which carries the method) and by
// run.BaseFake-based test fakes; serve.go type-asserts cfg.RunRepo to it.
type DeployStageSource interface {
	ListDeployStagesAwaitingDeployment(ctx context.Context) ([]*run.Stage, error)
	// ListDeployStagesRollbackPending lists deploy stages with a
	// deployment_rollback_initiated audit entry and NO matching
	// deployment_rollback_completed (#1398). The rollback-side analogue of
	// ListDeployStagesAwaitingDeployment, keyed on the rollback handle (audit)
	// not stage state — a rolled-back deploy stage is already terminal.
	ListDeployStagesRollbackPending(ctx context.Context) ([]*run.Stage, error)
	GetRun(ctx context.Context, id uuid.UUID) (*run.Run, error)
}

// Resolver records a terminal deploy outcome: it persists the deployment
// artifact, writes the deployment_outcome_recorded + deploy_run audit
// entries, transitions the stage awaiting_deployment → succeeded/failed,
// and advances the run. Satisfied by *server.Server
// (ResolveDeploymentFromPollState). Keeping the artifact/audit/trace
// writes in the server package mirrors the mergereconciler delegating to
// ResolveReviewFromPollState — this package stays a thin GitHub poller.
type Resolver interface {
	ResolveDeploymentFromPollState(ctx context.Context, runID, stageID uuid.UUID, outcome run.DeployOutcome, gitRef string, wr *githubclient.WorkflowRun) error
	// ResolveDeploymentRollbackFromPollState records a rolled_back disposition
	// once the reconciler has polled the rollback run to terminal (#1398): it
	// persists a rolled_back deployment artifact and writes
	// deployment_outcome_recorded + deploy_run + deployment_rollback_completed.
	// The deploy stage is ALREADY terminal, so this does NOT transition the
	// stage or advance the run — the rolled_back outcome rides the artifact +
	// audit (DeployOutcome is in-memory only, no column per migration 0038).
	// Satisfied by *server.Server.
	ResolveDeploymentRollbackFromPollState(ctx context.Context, runID, stageID uuid.UUID, gitRef string, wr *githubclient.WorkflowRun) error
}

// Ticker scans deploy stages parked in awaiting_deployment and resolves
// any whose external GitHub Actions run has reached a terminal
// conclusion. Run() blocks until ctx is done.
type Ticker struct {
	// Runs lists awaiting-deployment deploy stages (via
	// ListDeployStagesAwaitingDeployment) and reads the run row (for
	// installation_id + repo). Required.
	Runs DeployStageSource

	// GH polls the external GitHub Actions run and re-resolves a
	// dispatched run from its correlation token. Required.
	GH WorkflowRunPoller

	// Audit reads the deployment_dispatched handle back. Required.
	Audit AuditReader

	// Resolver records the terminal outcome (artifact + audit + trace +
	// transition + run advance) through the server. Required.
	Resolver Resolver

	// Logger receives structured warnings about transient errors.
	// nil → slog.Default().
	Logger *slog.Logger

	// Interval is the tick period. Defaults to DefaultInterval when
	// zero. The ticker fires immediately on Run() start; the interval
	// gates subsequent ticks.
	Interval time.Duration

	// Now sources the current time for the re-resolve created-after
	// window. nil → time.Now.
	Now func() time.Time
}

// Run drives the ticker until ctx is cancelled. Each tick lists
// awaiting-deployment deploy stages and reconciles any whose external run
// has reached a terminal conclusion. Per-stage errors log but don't abort
// the loop.
func (t *Ticker) Run(ctx context.Context) error {
	if t.Runs == nil {
		return errors.New("deployreconciler: ticker requires Runs")
	}
	if t.GH == nil {
		return errors.New("deployreconciler: ticker requires GH")
	}
	if t.Audit == nil {
		return errors.New("deployreconciler: ticker requires Audit")
	}
	if t.Resolver == nil {
		return errors.New("deployreconciler: ticker requires Resolver")
	}
	interval := t.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}

	t.Tick(ctx)

	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
			t.Tick(ctx)
		}
	}
}

// Tick performs one pass over awaiting-deployment deploy stages. Exposed
// for tests so the deterministic scenarios can drive the ticker
// step-by-step without spinning real timers.
func (t *Ticker) Tick(ctx context.Context) {
	logger := t.Logger
	if logger == nil {
		logger = slog.Default()
	}

	stages, err := t.Runs.ListDeployStagesAwaitingDeployment(ctx)
	if err != nil {
		logger.LogAttrs(ctx, slog.LevelWarn, "deployreconciler: list awaiting-deployment stages failed",
			slog.String("error", err.Error()))
	} else {
		for _, s := range stages {
			if s.Type != run.StageTypeDeploy {
				// Defense-in-depth: the query already filters to
				// stage_type = 'deploy'. A fake or future listing that returns a
				// non-deploy stage can't reach reconcileStage.
				continue
			}
			t.reconcileStage(ctx, logger, s)
		}
	}

	// Second, DISTINCT scan: deploy stages with a pending rollback (#1398 /
	// #1386 binding condition 2). A rolled-back deploy stage is already terminal
	// (succeeded/failed), so it never appears in the awaiting_deployment scan
	// above; the rollback handle lives in a deployment_rollback_initiated audit
	// entry. Records rolled_back + deployment_rollback_completed for a
	// github_actions rollback pipeline that never calls back. A list error here
	// does NOT abort the forward scan above (and vice versa).
	rollbacks, err := t.Runs.ListDeployStagesRollbackPending(ctx)
	if err != nil {
		logger.LogAttrs(ctx, slog.LevelWarn, "deployreconciler: list rollback-pending stages failed",
			slog.String("error", err.Error()))
		return
	}
	for _, s := range rollbacks {
		if s.Type != run.StageTypeDeploy {
			// Defense-in-depth: the query already filters to stage_type =
			// 'deploy'. A fake returning a non-deploy stage can't reach
			// reconcileRollback.
			continue
		}
		t.reconcileRollback(ctx, logger, s)
	}
}

// dispatchHandle is the slice-1 deployment_dispatched audit payload the
// reconciler reads back. Only the github_actions fields are required here;
// a webhook target carries no gha_run_id and is skipped.
type dispatchHandle struct {
	Target         string `json:"target"`
	GHARunID       int64  `json:"gha_run_id"`
	ExternalRunURL string `json:"external_run_url"`
	GitRef         string `json:"git_ref"`
	DispatchedAt   string `json:"dispatched_at"`
}

// reconcileStage polls one parked deploy stage's external run and, on a
// terminal conclusion, hands off to the server resolver. Skips cleanly
// (no transition) when the run has no installation, when no dispatch handle
// was recorded, when the target is webhook, or when the run is still
// in-flight. Per-row errors log but don't propagate.
func (t *Ticker) reconcileStage(ctx context.Context, logger *slog.Logger, s *run.Stage) {
	runRow, err := t.Runs.GetRun(ctx, s.RunID)
	if err != nil {
		logger.LogAttrs(ctx, slog.LevelWarn, "deployreconciler: get run failed",
			slog.String("run_id", s.RunID.String()),
			slog.String("error", err.Error()))
		return
	}
	if runRow.InstallationID == nil || *runRow.InstallationID == 0 {
		// No installation_id → no GitHub creds to poll with. Leave parked.
		return
	}
	repo, err := parseRepoRef(runRow.Repo)
	if err != nil {
		logger.LogAttrs(ctx, slog.LevelWarn, "deployreconciler: malformed run repo",
			slog.String("run_id", s.RunID.String()),
			slog.String("repo", runRow.Repo),
			slog.String("error", err.Error()))
		return
	}

	handle, ok := t.latestHandle(ctx, logger, s, categoryDeploymentDispatched)
	if !ok {
		// No dispatch handle recorded yet (slice-1's trigger has not fired,
		// or the audit append failed). Nothing to poll; leave parked.
		return
	}
	if handle.Target == "webhook" {
		// Webhook targets report terminal via the deployment callback, not
		// the reconciler (slice-1 / #1395). Leave parked.
		return
	}

	correlation := map[string]string{
		"fishhawk_run_id":   s.RunID.String(),
		"fishhawk_stage_id": s.ID.String(),
	}
	wr := t.resolveRun(ctx, logger, s, repo, *runRow.InstallationID, handle, correlation)
	if wr == nil {
		// Indeterminate / not-yet-resolvable / transient poll error — leave
		// parked and retry next tick. NEVER terminal-fail on a poll miss.
		return
	}

	// Terminal only when the run has completed. A queued/in_progress/
	// requested run carries an empty conclusion — leave parked.
	if !strings.EqualFold(wr.Status, "completed") {
		return
	}
	outcome, terminal := mapConclusion(wr.Conclusion)
	if !terminal {
		// Completed but with a conclusion we don't map to a terminal deploy
		// outcome (e.g. an empty or unexpected value) — leave parked rather
		// than guess a wrong disposition.
		logger.LogAttrs(ctx, slog.LevelWarn,
			"deployreconciler: completed run with unmapped conclusion; left parked",
			slog.String("run_id", s.RunID.String()),
			slog.String("conclusion", wr.Conclusion))
		return
	}

	if err := t.Resolver.ResolveDeploymentFromPollState(ctx, s.RunID, s.ID, outcome, handle.GitRef, wr); err != nil {
		logger.LogAttrs(ctx, slog.LevelWarn, "deployreconciler: resolve deployment failed",
			slog.String("run_id", s.RunID.String()),
			slog.String("stage_id", s.ID.String()),
			slog.String("error", err.Error()))
		return
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "deployreconciler: resolved deployment from poll",
		slog.String("run_id", s.RunID.String()),
		slog.String("stage_id", s.ID.String()),
		slog.String("conclusion", wr.Conclusion),
		slog.String("outcome", string(outcome)))
}

// reconcileRollback polls one deploy stage's pending ROLLBACK run and, on a
// terminal conclusion, hands off to the server's rollback resolver (#1398). The
// deploy stage is already terminal; this scan exists so a github_actions
// rollback pipeline that never calls back into POST /v0/runs/{run_id}/deployment
// still records rolled_back + deployment_rollback_completed. Skips cleanly (no
// record) when the run has no installation, no rollback handle was recorded,
// the target is webhook (callback path), or the rollback run is still in-flight.
// Per-row errors log but don't propagate.
func (t *Ticker) reconcileRollback(ctx context.Context, logger *slog.Logger, s *run.Stage) {
	runRow, err := t.Runs.GetRun(ctx, s.RunID)
	if err != nil {
		logger.LogAttrs(ctx, slog.LevelWarn, "deployreconciler: get run failed (rollback)",
			slog.String("run_id", s.RunID.String()),
			slog.String("error", err.Error()))
		return
	}
	if runRow.InstallationID == nil || *runRow.InstallationID == 0 {
		// No installation_id → no GitHub creds to poll with. Leave pending.
		return
	}
	repo, err := parseRepoRef(runRow.Repo)
	if err != nil {
		logger.LogAttrs(ctx, slog.LevelWarn, "deployreconciler: malformed run repo (rollback)",
			slog.String("run_id", s.RunID.String()),
			slog.String("repo", runRow.Repo),
			slog.String("error", err.Error()))
		return
	}

	handle, ok := t.latestHandle(ctx, logger, s, categoryDeploymentRollbackInitiated)
	if !ok {
		// No rollback handle (the query selected on its presence, but the
		// payload may be unparseable). Nothing to poll; leave pending.
		return
	}
	if handle.Target == "webhook" {
		// Webhook rollbacks report terminal via the deployment callback, not
		// the reconciler. Leave pending.
		return
	}

	// Re-resolve correlation MUST carry the rollback marker: the forward deploy
	// run and the rollback run both echo run_id+stage_id, so without
	// fishhawk_rollback="true" ResolveDispatchedRun could associate the wrong
	// (forward) run (plan risk #1).
	correlation := map[string]string{
		"fishhawk_run_id":         s.RunID.String(),
		"fishhawk_stage_id":       s.ID.String(),
		rollbackCorrelationMarker: "true",
	}
	wr := t.resolveRun(ctx, logger, s, repo, *runRow.InstallationID, handle, correlation)
	if wr == nil {
		// Indeterminate / not-yet-resolvable / transient poll error — leave
		// pending and retry next tick. NEVER mis-associate on an ambiguous
		// correlation.
		return
	}

	if !strings.EqualFold(wr.Status, "completed") {
		// Rollback run still queued/in-progress — leave pending.
		return
	}
	// Any TERMINAL conclusion records rolled_back, mirroring the webhook
	// callback (which sets outcome=rolled_back regardless of the rollback run's
	// conclusion — the actual conclusion is preserved in the deploy_run trace).
	// A non-terminal/unmapped conclusion (e.g. empty) is left pending rather
	// than recording a guessed disposition.
	if _, terminal := mapConclusion(wr.Conclusion); !terminal {
		logger.LogAttrs(ctx, slog.LevelWarn,
			"deployreconciler: completed rollback run with unmapped conclusion; left pending",
			slog.String("run_id", s.RunID.String()),
			slog.String("conclusion", wr.Conclusion))
		return
	}

	if err := t.Resolver.ResolveDeploymentRollbackFromPollState(ctx, s.RunID, s.ID, handle.GitRef, wr); err != nil {
		logger.LogAttrs(ctx, slog.LevelWarn, "deployreconciler: resolve deployment rollback failed",
			slog.String("run_id", s.RunID.String()),
			slog.String("stage_id", s.ID.String()),
			slog.String("error", err.Error()))
		return
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "deployreconciler: resolved deployment rollback from poll",
		slog.String("run_id", s.RunID.String()),
		slog.String("stage_id", s.ID.String()),
		slog.String("conclusion", wr.Conclusion))
}

// latestHandle reads the most-recent audit entry of the given category for the
// stage's run and unmarshals its payload into a dispatchHandle. Returns
// ok=false when none exists or the payload can't be parsed. Shared by the
// forward scan (category deployment_dispatched) and the rollback scan (category
// deployment_rollback_initiated) — both audit payloads carry the same
// target/gha_run_id/git_ref/dispatched_at handle fields.
func (t *Ticker) latestHandle(ctx context.Context, logger *slog.Logger, s *run.Stage, category string) (dispatchHandle, bool) {
	entries, err := t.Audit.ListForRunByCategory(ctx, s.RunID, category)
	if err != nil {
		logger.LogAttrs(ctx, slog.LevelWarn, "deployreconciler: read dispatch handle failed",
			slog.String("run_id", s.RunID.String()),
			slog.String("category", category),
			slog.String("error", err.Error()))
		return dispatchHandle{}, false
	}
	if len(entries) == 0 {
		return dispatchHandle{}, false
	}
	// Entries are ordered ascending by sequence; the last one is the most
	// recent dispatch (a fixup re-dispatch would append a newer handle).
	latest := entries[len(entries)-1]
	var h dispatchHandle
	if err := json.Unmarshal(latest.Payload, &h); err != nil {
		logger.LogAttrs(ctx, slog.LevelWarn, "deployreconciler: dispatch handle payload unparseable",
			slog.String("run_id", s.RunID.String()),
			slog.String("category", category),
			slog.String("error", err.Error()))
		return dispatchHandle{}, false
	}
	return h, true
}

// resolveRun returns the live external WorkflowRun for the handle, or nil
// when it can't be resolved this tick (transient error, indeterminate
// correlation, or not yet visible). When the handle carries a concrete
// gha_run_id it is fetched directly; otherwise (slice-1's best-effort
// resolution was empty) it is re-resolved by the correlation token —
// returning nil on an ambiguous fallback rather than associating a wrong
// run (binding condition 1, #1386).
func (t *Ticker) resolveRun(ctx context.Context, logger *slog.Logger, s *run.Stage, repo githubclient.RepoRef, installationID int64, handle dispatchHandle, correlation map[string]string) *githubclient.WorkflowRun {
	if handle.GHARunID > 0 {
		wr, err := t.GH.GetWorkflowRun(ctx, installationID, repo, handle.GHARunID)
		if err != nil {
			logger.LogAttrs(ctx, slog.LevelWarn, "deployreconciler: get workflow run failed; retry next tick",
				slog.String("run_id", s.RunID.String()),
				slog.Int64("gha_run_id", handle.GHARunID),
				slog.String("error", err.Error()))
			return nil
		}
		return wr
	}

	// gha_run_id absent — re-resolve by the correlation token. The created
	// window mirrors slice-1's trigger (dispatched_at minus a minute of
	// slack); a parse failure falls back to the zero time (no created
	// filter), which ResolveDispatchedRun tolerates. The correlation map is
	// supplied by the caller: the forward scan passes run_id+stage_id; the
	// rollback scan ADDS fishhawk_rollback="true" so it never matches the
	// forward deploy run (which echoes the same run_id+stage_id).
	createdAfter := time.Time{}
	if handle.DispatchedAt != "" {
		if ts, perr := time.Parse(time.RFC3339, handle.DispatchedAt); perr == nil {
			createdAfter = ts.Add(-1 * time.Minute)
		}
	}
	wr, err := t.GH.ResolveDispatchedRun(ctx, installationID, repo, handle.GitRef, correlation, createdAfter)
	if err != nil {
		logger.LogAttrs(ctx, slog.LevelWarn, "deployreconciler: re-resolve dispatched run failed; retry next tick",
			slog.String("run_id", s.RunID.String()),
			slog.String("error", err.Error()))
		return nil
	}
	// (nil, nil) is INDETERMINATE or not-yet-visible: leave parked, retry.
	return wr
}

// mapConclusion maps a terminal GitHub Actions workflow_run conclusion to a
// deploy outcome (binding-condition mapping, #1386). terminal=false means
// the conclusion is not one we resolve on (e.g. empty) — the caller leaves
// the stage parked rather than recording a guessed disposition.
//
//   - success                       → succeeded (stage → succeeded)
//   - neutral                       → partial   (stage → failed, the deploy
//     pipeline signals a partial rollout via a neutral conclusion)
//   - failure / cancelled / timed_out / startup_failure / action_required /
//     stale / skipped → failed (stage → failed)
func mapConclusion(conclusion string) (run.DeployOutcome, bool) {
	switch strings.ToLower(strings.TrimSpace(conclusion)) {
	case "success":
		return run.DeployOutcomeSucceeded, true
	case "neutral":
		return run.DeployOutcomePartial, true
	case "failure", "cancelled", "canceled", "timed_out", "startup_failure", "action_required", "stale", "skipped":
		return run.DeployOutcomeFailed, true
	default:
		return "", false
	}
}

// parseRepoRef splits "owner/name" into a githubclient.RepoRef. Local to
// this package (the server's parseRepoRef is unexported in another package).
func parseRepoRef(s string) (githubclient.RepoRef, error) {
	owner, name, ok := strings.Cut(s, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return githubclient.RepoRef{}, fmt.Errorf("malformed repo %q", s)
	}
	return githubclient.RepoRef{Owner: owner, Name: name}, nil
}
