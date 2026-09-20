package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/stagecheck"
	"github.com/kuhlman-labs/fishhawk/backend/internal/webhook"
)

// gitLabPipelinePayload is the slice of a GitLab Pipeline Hook payload the
// ingester reads
// (https://docs.gitlab.com/user/project/integrations/webhook_events/#pipeline-events).
// object_attributes.id is the pipeline's INSTANCE-GLOBAL id (distinct from
// the project-local iid, https://docs.gitlab.com/api/pipelines/) and is
// monotonically increasing, so a higher id is a newer pipeline for the same
// sha. merge_request is present only on merge-request pipelines; a branch
// pipeline carries no iid and matches the run by sha alone.
type gitLabPipelinePayload struct {
	Project struct {
		ID                int64  `json:"id"`
		PathWithNamespace string `json:"path_with_namespace"`
	} `json:"project"`
	ObjectAttributes struct {
		ID         int64  `json:"id"`
		Ref        string `json:"ref"`
		SHA        string `json:"sha"`
		Status     string `json:"status"`
		Source     string `json:"source"`
		CreatedAt  string `json:"created_at"`
		FinishedAt string `json:"finished_at"`
	} `json:"object_attributes"`
	MergeRequest struct {
		IID int `json:"iid"`
	} `json:"merge_request"`
}

// gitLabPipelineTimestampLayouts are the layouts a Pipeline Hook timestamp
// is tried against, in order. GitLab's documented example carries
// created_at / finished_at as "2016-08-12 15:23:28 UTC" — NOT RFC 3339 —
// while newer instances emit RFC 3339; both are accepted so a layout change
// on GitLab's side degrades to the now() fallback rather than a parse
// error. Since precedence within a check_name is decided by
// gitlab_pipeline_id (see ingestGitLabPipeline), a wrong parse can no
// longer flip which row is latest — it only mis-stamps ts.
var gitLabPipelineTimestampLayouts = []string{
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02 15:04:05 MST",
}

// gitLabPipelineTimestamp picks the row timestamp for a pipeline event:
// finished_at when present and parseable (the terminal instant), else
// created_at, else now() with a WARN. The bool reports whether a payload
// timestamp was used (false = fallback), so a test can pin the degrade.
func gitLabPipelineTimestamp(ctx context.Context, logger *slog.Logger, finishedAt, createdAt string) (time.Time, bool) {
	for _, raw := range []string{finishedAt, createdAt} {
		if raw == "" {
			continue
		}
		for _, layout := range gitLabPipelineTimestampLayouts {
			if t, err := time.Parse(layout, raw); err == nil {
				return t.UTC(), true
			}
		}
	}
	if logger != nil {
		logger.LogAttrs(ctx, slog.LevelWarn, "gitlab pipeline: no parseable timestamp; using now()",
			slog.String("finished_at", finishedAt),
			slog.String("created_at", createdAt),
		)
	}
	return time.Now().UTC(), false
}

// gitLabPipelineCheckState maps a GitLab pipeline status onto the
// stage_checks (status, conclusion) vocabulary stagecheck.DeriveState
// reads. Documented statuses
// (https://docs.gitlab.com/api/pipelines/): created, waiting_for_resource,
// preparing, pending, running, success, failed, canceled, skipped, manual,
// scheduled.
//
//   - success            → completed / success           (pass)
//   - failed             → completed / failure           (fail)
//   - canceled|cancelled → completed / cancelled         (fail, superseded)
//   - skipped            → completed / skipped           (pass) ONLY when the
//     run's snapshot carries allow_merge_on_skipped_pipeline; otherwise
//     completed / skipped_not_allowed (fail — GitLab itself would refuse
//     the merge, so the gate must not borrow GitHub's skipped-is-pass rule)
//   - created|waiting_for_resource|preparing|pending|scheduled → queued
//   - running|manual|canceling → in_progress
//   - anything else      → in_progress, so a status GitLab adds later can
//     never clear the gate by accident.
func gitLabPipelineCheckState(status string, allowSkipped bool) (checkStatus string, conclusion *string) {
	completed := func(c string) (string, *string) { return "completed", &c }
	switch status {
	case "success":
		return completed("success")
	case "failed":
		return completed("failure")
	case "canceled", "cancelled":
		return completed("cancelled")
	case "skipped":
		if allowSkipped {
			return completed("skipped")
		}
		return completed("skipped_not_allowed")
	case "created", "waiting_for_resource", "preparing", "pending", "scheduled":
		return "queued", nil
	case "running", "manual", "canceling":
		return "in_progress", nil
	default:
		return "in_progress", nil
	}
}

// ingestGitLabPipeline handles a GitLab Pipeline Hook (object_kind
// `pipeline`) by writing one `gitlab/pipeline` stage_checks row per
// matching review stage and re-running the post-CI policy evaluation for
// every run that received a row (E45.55 / #3490). It is the GitLab sibling
// of ingestCheckRun and the SECOND writer of stage_checks.
//
// Routing: the review-stage match is PROJECT-SCOPED —
// stagecheck.FindMatchingStagesForGitLabPipeline requires BOTH runs.repo
// (ev.Repo, the project path) and runs.installation_ref (ev.CredentialRef,
// "gitlab:<project_id>") alongside the head_sha and, when the hook names a
// merge request, its iid — so a fork sharing sha + iid never receives a
// row. An event missing the repo, the credential ref, the sha or a
// positive pipeline id is dropped before any repository call.
//
// Precedence: the SAME head_sha can carry several pipelines (retry, manual
// re-run, merge-request vs branch pipeline) and the NEWEST id is
// authoritative. That is STRUCTURAL, not a guard here: both stagecheck
// latest-row readers order by `gitlab_pipeline_id DESC NULLS LAST, ts
// DESC`, so an older id appended later can never become the latest row.
// The `latest.GitLabPipelineID > incoming → skip` check below is a
// WRITE-AVOIDANCE OPTIMISATION ONLY — correctness does not depend on it,
// so the read-then-append is not a race worth a transaction: a concurrent
// delivery interleaving can at worst write one redundant row the readers
// never surface. Deleting the skip leaves every precedence test green
// (pinned by the counterfactual in gitlab_pipeline_pg_test.go).
//
// Skipped pipelines: `skipped` is recorded as pass ONLY when the run's
// RequiredChecksSnapshot carries GitLabAllowSkippedPipeline; a run with a
// nil snapshot (or the flag off) records the fail-bucket conclusion
// `skipped_not_allowed`. Conservative on purpose — a nil-snapshot run
// refuses snapshot_absent at the deploy gate regardless.
//
// Best-effort throughout: per-row failures log and continue; the webhook
// receiver acknowledges 202 either way, exactly as for ingestCheckRun.
func (s *Server) ingestGitLabPipeline(ctx context.Context, ev webhook.Event) {
	if s.cfg.StageCheckRepo == nil || s.cfg.RunRepo == nil {
		return
	}
	var p gitLabPipelinePayload
	if err := json.Unmarshal(ev.RawBody, &p); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "gitlab pipeline: payload parse failed",
			slog.String("delivery_id", ev.DeliveryID),
			slog.String("error", err.Error()),
		)
		return
	}
	if ev.Repo == "" || ev.CredentialRef == "" || p.ObjectAttributes.SHA == "" || p.ObjectAttributes.ID <= 0 {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo, "gitlab pipeline: event lacks a project-scoped match key; ignored",
			slog.String("delivery_id", ev.DeliveryID),
			slog.String("repo", ev.Repo),
			slog.String("credential_ref", ev.CredentialRef),
			slog.String("sha", p.ObjectAttributes.SHA),
			slog.Int64("pipeline_id", p.ObjectAttributes.ID),
		)
		return
	}

	match := stagecheck.GitLabPipelineMatch{
		Repo:            ev.Repo,
		InstallationRef: ev.CredentialRef,
		HeadSHA:         p.ObjectAttributes.SHA,
		MergeRequestIID: p.MergeRequest.IID,
	}
	refs, err := s.cfg.StageCheckRepo.FindMatchingStagesForGitLabPipeline(ctx, match)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "gitlab pipeline: find matching stages failed",
			slog.String("repo", ev.Repo),
			slog.String("credential_ref", ev.CredentialRef),
			slog.String("sha", p.ObjectAttributes.SHA),
			slog.Int("mr_iid", p.MergeRequest.IID),
			slog.String("error", err.Error()),
		)
		return
	}
	if len(refs) == 0 {
		return
	}

	ts, _ := gitLabPipelineTimestamp(ctx, s.cfg.Logger, p.ObjectAttributes.FinishedAt, p.ObjectAttributes.CreatedAt)
	pipelineID := p.ObjectAttributes.ID

	// One GetRun per distinct run — several review stages can share a run.
	runsByID := map[uuid.UUID]*run.Run{}
	loadRun := func(id uuid.UUID) *run.Run {
		if r, ok := runsByID[id]; ok {
			return r
		}
		r, err := s.cfg.RunRepo.GetRun(ctx, id)
		if err != nil {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "gitlab pipeline: get run failed",
				slog.String("run_id", id.String()),
				slog.String("error", err.Error()),
			)
			r = nil
		}
		runsByID[id] = r
		return r
	}

	// Runs whose review stage received a TERMINAL row, in first-seen
	// order, for the per-run re-eval below.
	var reevalOrder []uuid.UUID
	reevalSeen := map[uuid.UUID]bool{}

	for _, ref := range refs {
		r := loadRun(ref.RunID)
		allowSkipped := r != nil && r.RequiredChecksSnapshot != nil && r.RequiredChecksSnapshot.GitLabAllowSkippedPipeline
		checkStatus, conclusion := gitLabPipelineCheckState(p.ObjectAttributes.Status, allowSkipped)

		// Write-avoidance only — see the doc comment. Correctness lives
		// in the readers' ORDER BY, not here.
		latest, err := s.cfg.StageCheckRepo.LatestForStageAndName(ctx, ref.StageID, webhook.GitLabPipelineCheckContext)
		if err == nil && latest != nil && latest.GitLabPipelineID != nil && *latest.GitLabPipelineID > pipelineID {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo, "gitlab pipeline: superseded by a newer pipeline on this stage; not recorded",
				slog.String("stage_id", ref.StageID.String()),
				slog.Int64("pipeline_id", pipelineID),
				slog.Int64("latest_pipeline_id", *latest.GitLabPipelineID),
			)
			continue
		}

		id := pipelineID
		if _, err := s.cfg.StageCheckRepo.Append(ctx, stagecheck.AppendParams{
			StageID:          ref.StageID,
			Name:             webhook.GitLabPipelineCheckContext,
			Status:           checkStatus,
			Conclusion:       conclusion,
			HeadSHA:          p.ObjectAttributes.SHA,
			GitLabPipelineID: &id,
			Timestamp:        ts,
			Payload:          ev.RawBody,
		}); err != nil {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "gitlab pipeline: append failed",
				slog.String("stage_id", ref.StageID.String()),
				slog.Int64("pipeline_id", pipelineID),
				slog.String("error", err.Error()),
			)
			continue
		}
		if checkStatus == "completed" && r != nil && !reevalSeen[ref.RunID] {
			reevalSeen[ref.RunID] = true
			reevalOrder = append(reevalOrder, ref.RunID)
		}
	}

	// EVERY run that received a terminal row is re-evaluated — not just
	// the first match. Two runs for the same MR (an older run at sha aaa
	// and a newer one at sha bbb) are distinct rows, and a late pipeline
	// for aaa must still flip run A's ci_green.
	for _, runID := range reevalOrder {
		s.reevaluateCIPolicyForRun(ctx, runsByID[runID], webhook.GitLabPipelineCheckContext)
	}
}
