package runnerbackend

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
)

// gitLabScopePrefix is the credential-scope ref prefix a GitLab run carries:
// "gitlab:<project_id>". The GitLabCI backend parses the numeric project id out
// of it to address the pipelines API, mirroring how the forge GitLab adapter
// parses the same ref.
const gitLabScopePrefix = "gitlab:"

// PipelineTriggerClient is the narrow slice of the GitLab client the GitLabCI
// backend needs — just the pipeline-create call. Extracting an interface (the
// dispatch seam's local-interface convention, matching DispatchClient) lets
// tests substitute a stub. *gitlabclient.Client satisfies it.
type PipelineTriggerClient interface {
	// CreatePipeline creates a GitLab pipeline for projectID at ref, passing
	// variables as CI/CD variables. ref selects BOTH the .gitlab-ci.yml
	// evaluated AND the commit the pipeline runs against.
	CreatePipeline(ctx context.Context, projectID int, ref string, variables map[string]string) error
}

// GitLabCI triggers a stage by creating a GitLab pipeline via the pipelines
// API. It is the backend behind runner_kind=gitlab_ci (E45.8 / #1861), the
// second RunnerBackend the dispatch seam exists to admit.
//
// Like GitHubActions and UNLIKE Local, it is NOT host-dispatched: fishhawkd
// fires the pipeline itself, so its agent stages dispatch rather than park.
// TriggerStage does DISPATCH ONLY — it creates the pipeline against p.Ref and
// writes NO commit status. Commit-status publishing is EXCLUSIVELY
// auditcheckpublisher's job (the fishhawk-gate status posted as the run
// progresses), mirroring the GitHub division where workflow_dispatch fires the
// CI and the check-run publisher posts the gate — no two code paths write the
// same status.
type GitLabCI struct {
	Client PipelineTriggerClient
	Logger *slog.Logger
}

// Kind reports runner_kind=gitlab_ci.
func (*GitLabCI) Kind() string { return "gitlab_ci" }

// HostDispatched is false: fishhawkd creates the pipeline itself, so a
// gitlab_ci stage dispatches (like github_actions) rather than parking.
func (*GitLabCI) HostDispatched() bool { return false }

// TriggerStage creates a GitLab pipeline for the stage described by p. It
// warn+skips (returns nil) when the client is unwired or the credential scope
// is zero — the direct analogue of the github_actions unwired/zero-id skip.
// The pipeline is created against p.Ref (the run branch); an empty ref is a
// hard error because GitLab's pipelines API requires one to select the
// .gitlab-ci.yml and the target commit.
//
// The run_id/stage_id/workflow_id/stage identifiers ride as CI/CD variables so
// the backend-agnostic runner keys its identity off them, plus parent_run_id
// iff the run is a decomposed child (#1227 parity with the Actions path, which
// keys an Actions concurrency: group on the run family).
func (g *GitLabCI) TriggerStage(ctx context.Context, p TriggerParams) error {
	if g.Client == nil {
		g.logger().LogAttrs(ctx, slog.LevelWarn, "orchestrator: GitLab not configured; skipping pipeline create",
			slog.String("run_id", p.RunID.String()),
		)
		return nil
	}
	if p.Scope.IsZero() {
		g.logger().LogAttrs(ctx, slog.LevelWarn, "orchestrator: run has no gitlab scope; skipping pipeline create",
			slog.String("run_id", p.RunID.String()),
		)
		return nil
	}

	projectID, err := gitLabProjectID(p.Scope)
	if err != nil {
		return fmt.Errorf("orchestrator: gitlab_ci trigger: %w", err)
	}
	if p.Ref == "" {
		return fmt.Errorf("orchestrator: gitlab_ci trigger: empty ref for run %s (the run branch is required as the pipeline ref)", p.RunID)
	}

	// E24.5 parity: a decomposed child's pipeline create carries its slice
	// provenance so the per-slice fan-out is observable.
	if p.DecomposedFrom != nil && p.SliceIndex != nil {
		g.logger().LogAttrs(ctx, slog.LevelInfo, "orchestrator dispatching decomposed-child gitlab pipeline",
			slog.String("run_id", p.RunID.String()),
			slog.String("stage_id", p.StageID.String()),
			slog.Int("slice_index", *p.SliceIndex),
			slog.String("decomposed_from", p.DecomposedFrom.String()),
			slog.String("ref", p.Ref),
		)
	}

	variables := map[string]string{
		"run_id":      p.RunID.String(),
		"stage_id":    p.StageID.String(),
		"workflow_id": p.WorkflowID,
		"stage":       p.StageExecutorRef,
	}
	// #1227: a decomposed child carries its decomposition-parent id so the
	// customer .gitlab-ci.yml can key a concurrency group on the run FAMILY.
	if p.DecomposedFrom != nil {
		variables["parent_run_id"] = p.DecomposedFrom.String()
	}
	return g.Client.CreatePipeline(ctx, projectID, p.Ref, variables)
}

func (g *GitLabCI) logger() *slog.Logger {
	if g.Logger != nil {
		return g.Logger
	}
	return slog.Default()
}

// gitLabProjectID parses the numeric GitLab project id out of a
// "gitlab:<project_id>" credential scope ref. It fails closed — never a silent
// 0 — on a missing prefix or a non-numeric id.
func gitLabProjectID(scope forge.CredentialScope) (int, error) {
	ref := scope.Ref()
	if !strings.HasPrefix(ref, gitLabScopePrefix) {
		return 0, fmt.Errorf("credential scope ref %q is not a gitlab project scope", ref)
	}
	rest := strings.TrimPrefix(ref, gitLabScopePrefix)
	id, err := strconv.Atoi(rest)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("credential scope ref %q has no valid gitlab project id", ref)
	}
	return id, nil
}
