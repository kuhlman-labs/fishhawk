package runnerbackend

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
)

// DispatchClient is the narrow slice of *githubclient.Client the GitHubActions
// backend uses — just the workflow_dispatch call. Extracting an interface (the
// orchestrator's local-interface convention) lets tests substitute a stub. The
// orchestrator's and webhook's own GitHubAPI interfaces both satisfy it.
type DispatchClient interface {
	DispatchWorkflow(ctx context.Context, scope forge.CredentialScope,
		repo forge.RepoRef, workflowFile, ref string,
		inputs githubclient.DispatchInputs) error
}

// GitHubActions triggers a stage by firing a GitHub Actions workflow_dispatch.
// It is the backend behind runner_kind=github_actions and the resolver's
// legacy auto-resolve / fire-through channel. DefaultRef defaults to "main" and
// ActionsWorkflowFile to "fishhawk.yml" at use time — the same defaults the
// orchestrator.fireDispatch and webhook CI-retry dispatch sites carried.
type GitHubActions struct {
	Client              DispatchClient
	DefaultRef          string
	ActionsWorkflowFile string
	Logger              *slog.Logger
}

// Kind reports runner_kind=github_actions.
func (*GitHubActions) Kind() string { return "github_actions" }

// HostDispatched is false: fishhawkd fires the workflow_dispatch itself.
func (*GitHubActions) HostDispatched() bool { return false }

// TriggerStage fires the workflow_dispatch. It reproduces fireDispatch
// byte-for-byte: warn+nil skip when the client is unwired or the credential
// scope is the zero (unwired) scope, malformed-repo error, ref/actions-file
// defaults, the decomposed-child slice provenance log, and the
// run_id/stage_id/workflow_id/stage inputs plus parent_run_id iff the run is a
// decomposed child (#1227). p.Ref is ignored: the workflow file lives on the
// default branch, so github_actions always dispatches on DefaultRef ("main").
func (g *GitHubActions) TriggerStage(ctx context.Context, p TriggerParams) error {
	if g.Client == nil {
		g.logger().LogAttrs(ctx, slog.LevelWarn, "orchestrator: GitHub not configured; skipping workflow_dispatch",
			slog.String("run_id", p.RunID.String()),
		)
		return nil
	}
	if p.Scope.IsZero() {
		g.logger().LogAttrs(ctx, slog.LevelWarn, "orchestrator: run has no installation_id; skipping workflow_dispatch",
			slog.String("run_id", p.RunID.String()),
		)
		return nil
	}

	repo, err := parseRepo(p.Repo)
	if err != nil {
		return fmt.Errorf("orchestrator: parse repo %q: %w", p.Repo, err)
	}

	ref := g.DefaultRef
	if ref == "" {
		ref = "main"
	}
	actionsFile := g.ActionsWorkflowFile
	if actionsFile == "" {
		actionsFile = "fishhawk.yml"
	}

	// E24.5: annotate a decomposed child's workflow_dispatch with its slice
	// provenance so the per-slice Actions fan-out is observable.
	if p.DecomposedFrom != nil && p.SliceIndex != nil {
		g.logger().LogAttrs(ctx, slog.LevelInfo, "orchestrator dispatching decomposed-child workflow_dispatch",
			slog.String("run_id", p.RunID.String()),
			slog.String("stage_id", p.StageID.String()),
			slog.Int("slice_index", *p.SliceIndex),
			slog.String("decomposed_from", p.DecomposedFrom.String()),
			slog.String("ref", ref),
		)
	}

	inputs := githubclient.DispatchInputs{
		"run_id":      p.RunID.String(),
		"stage_id":    p.StageID.String(),
		"workflow_id": p.WorkflowID,
		"stage":       p.StageExecutorRef,
	}
	// #1227: a decomposed child carries its decomposition-parent id so the
	// customer workflow can key an Actions `concurrency:` group on the run
	// FAMILY. DecomposedFrom (the fan-out parent), NOT ParentRunID, is the
	// sibling family. Empty when DecomposedFrom is nil.
	if p.DecomposedFrom != nil {
		inputs["parent_run_id"] = p.DecomposedFrom.String()
	}
	return g.Client.DispatchWorkflow(ctx, p.Scope, repo, actionsFile, ref, inputs)
}

func (g *GitHubActions) logger() *slog.Logger {
	if g.Logger != nil {
		return g.Logger
	}
	return slog.Default()
}

// parseRepo splits "owner/name". Kept local to the runnerbackend package (the
// dispatch seam's only owner/name splitter); orchestrator and webhook retain
// their own copies for their non-dispatch callers (enableAutoMerge, etc.), a
// consolidation the orchestrator already flags as a v0.x cleanup.
func parseRepo(s string) (forge.RepoRef, error) {
	for i := 0; i < len(s); i++ {
		if s[i] == '/' {
			if i == 0 || i == len(s)-1 {
				return forge.RepoRef{}, fmt.Errorf("malformed repo %q", s)
			}
			return forge.RepoRef{Owner: s[:i], Name: s[i+1:]}, nil
		}
	}
	return forge.RepoRef{}, fmt.Errorf("malformed repo %q", s)
}
