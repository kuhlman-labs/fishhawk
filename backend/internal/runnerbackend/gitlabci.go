package runnerbackend

import (
	"context"
	"log/slog"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/gitlabclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// PipelineTrigger is the narrow slice of the GitLab forge the GitLabCI backend
// uses — just the pipeline-create call. Extracting an interface (the same
// local-interface convention as githubactions.go's DispatchClient) lets tests
// substitute a stub, and keeps the backend from depending on the whole
// *gitlab.Forge surface. *gitlab.Forge satisfies it via its TriggerPipeline
// wrapper (forge/gitlab/gitlab.go), resolved by the orchestrator/dispatcher
// through forge.Get("gitlab") so no authenticated client is wired into the
// dispatch path.
type PipelineTrigger interface {
	TriggerPipeline(ctx context.Context, scope forge.CredentialScope, ref string,
		vars []gitlabclient.PipelineVariable) error
}

// GitLabPipelineTrigger resolves the registered GitLab forge (forge.Get) to a
// PipelineTrigger, or nil when GitLab is unconfigured / not registered / does
// not satisfy the interface. Nil-safe by design: a nil return leaves
// GitLabCI.Trigger nil so TriggerStage warn+skips rather than dispatching
// against a nil forge. This is the single resolution the orchestrator and
// webhook dispatcher both build their gitlab_ci backend from, keeping the
// forge.Get("gitlab") lookup out of the serve.go token-wiring path.
func GitLabPipelineTrigger() PipelineTrigger {
	f, err := forge.Get("gitlab")
	if err != nil {
		return nil
	}
	if t, ok := f.(PipelineTrigger); ok {
		return t
	}
	return nil
}

// GitLabCI triggers a stage by creating a GitLab CI/CD pipeline against the
// run's branch (#1861, ADR-058). It is the backend behind runner_kind=gitlab_ci.
//
// DISPATCH ONLY: TriggerStage creates the pipeline and does NOT write a commit
// status. Publishing a gitlab_ci run's stage status is exclusively
// auditcheckpublisher's job (mirroring the GitHub check-run division), which is
// why the PipelineTrigger interface carries no status method — the backend
// structurally cannot write one.
//
// DORMANT: no gitlab_ci run is created until go-live enablement (#2043), so this
// backend fires only under unit/wire tests today. Trigger is nil-safe: an
// unconfigured GitLab (forge.Get("gitlab") failing or not satisfying
// PipelineTrigger) leaves it nil and TriggerStage warn+skips, mirroring the
// github_actions nil-client skip.
type GitLabCI struct {
	Trigger PipelineTrigger
	Logger  *slog.Logger
}

// Kind reports runner_kind=gitlab_ci.
func (*GitLabCI) Kind() string { return run.RunnerKindGitLabCI }

// HostDispatched is false: fishhawkd fires the pipeline trigger itself (gitlab_ci
// is not a host-spawned channel like the local runner).
func (*GitLabCI) HostDispatched() bool { return false }

// TriggerStage creates the GitLab pipeline for the stage described by p. It
// warn+skips (returns nil, fires NO pipeline) when GitLab is unconfigured
// (Trigger == nil) or the credential scope is the zero/unwired scope
// (p.Scope.IsZero()) — the fail-closed guards mirroring github_actions. On the
// happy path it creates a pipeline against p.Ref (the run branch) carrying
// run_id/stage_id/workflow_id/stage as CI/CD variables, plus parent_run_id iff
// the run is a decomposed child (#1227). It NEVER writes a commit status.
func (g *GitLabCI) TriggerStage(ctx context.Context, p TriggerParams) error {
	if g.Trigger == nil {
		g.logger().LogAttrs(ctx, slog.LevelWarn, "orchestrator: GitLab not configured; skipping pipeline trigger",
			slog.String("run_id", p.RunID.String()),
		)
		return nil
	}
	if p.Scope.IsZero() {
		g.logger().LogAttrs(ctx, slog.LevelWarn, "orchestrator: run has no credential scope; skipping pipeline trigger",
			slog.String("run_id", p.RunID.String()),
		)
		return nil
	}

	// Ordered slice (not a map) so the CI/CD variable order is deterministic —
	// the run-provenance keys the customer pipeline reads. run_id/stage_id/
	// workflow_id/stage mirror the github_actions workflow_dispatch inputs.
	vars := []gitlabclient.PipelineVariable{
		{Key: "run_id", Value: p.RunID.String()},
		{Key: "stage_id", Value: p.StageID.String()},
		{Key: "workflow_id", Value: p.WorkflowID},
		{Key: "stage", Value: p.StageExecutorRef},
	}
	// #1227: a decomposed child carries its decomposition-parent id so the
	// customer pipeline can key a resource group on the run FAMILY. DecomposedFrom
	// (the fan-out parent), NOT ParentRunID, is the sibling family.
	if p.DecomposedFrom != nil {
		vars = append(vars, gitlabclient.PipelineVariable{Key: "parent_run_id", Value: p.DecomposedFrom.String()})
	}

	// E24.5: annotate a decomposed child's pipeline trigger with its slice
	// provenance so the per-slice fan-out is observable.
	if p.DecomposedFrom != nil && p.SliceIndex != nil {
		g.logger().LogAttrs(ctx, slog.LevelInfo, "orchestrator dispatching decomposed-child pipeline trigger",
			slog.String("run_id", p.RunID.String()),
			slog.String("stage_id", p.StageID.String()),
			slog.Int("slice_index", *p.SliceIndex),
			slog.String("decomposed_from", p.DecomposedFrom.String()),
			slog.String("ref", p.Ref),
		)
	}

	return g.Trigger.TriggerPipeline(ctx, p.Scope, p.Ref, vars)
}

func (g *GitLabCI) logger() *slog.Logger {
	if g.Logger != nil {
		return g.Logger
	}
	return slog.Default()
}
