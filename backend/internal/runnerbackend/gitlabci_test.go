package runnerbackend

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// --- GitLabCI fake ---

type pipelineCall struct {
	ProjectID int
	Ref       string
	Variables map[string]string
}

type fakePipelineClient struct {
	calls []pipelineCall
	err   error
}

func (f *fakePipelineClient) CreatePipeline(_ context.Context, projectID int, ref string, variables map[string]string) error {
	if f.err != nil {
		return f.err
	}
	f.calls = append(f.calls, pipelineCall{ProjectID: projectID, Ref: ref, Variables: variables})
	return nil
}

func gitLabScope(projectID string) forge.CredentialScope {
	return forge.FromRef("gitlab:" + projectID)
}

// --- backend identity ---

func TestGitLabCI_KindAndHostDispatched(t *testing.T) {
	g := &GitLabCI{}
	if g.Kind() != run.RunnerKindGitLabCI {
		t.Errorf("Kind() = %q, want gitlab_ci", g.Kind())
	}
	if g.HostDispatched() {
		t.Error("GitLabCI.HostDispatched() = true, want false (fishhawkd fires the pipeline)")
	}
}

// --- TriggerStage happy paths (ref/variable assertions — the CI-observable proxy) ---

func TestGitLabCI_TriggerStage_TopLevel(t *testing.T) {
	fc := &fakePipelineClient{}
	g := &GitLabCI{Client: fc}
	runID, stageID := uuid.New(), uuid.New()
	err := g.TriggerStage(context.Background(), TriggerParams{
		RunID: runID, StageID: stageID, WorkflowID: "feature_change",
		StageExecutorRef: "claude-code", Repo: "group/proj",
		Scope: gitLabScope("123"), Ref: "fishhawk/run-abcd1234",
	})
	if err != nil {
		t.Fatalf("TriggerStage: %v", err)
	}
	if len(fc.calls) != 1 {
		t.Fatalf("pipeline calls = %d, want 1", len(fc.calls))
	}
	call := fc.calls[0]
	if call.ProjectID != 123 {
		t.Errorf("project id = %d, want 123", call.ProjectID)
	}
	if call.Ref != "fishhawk/run-abcd1234" {
		t.Errorf("ref = %q, want the top-level run branch", call.Ref)
	}
	want := map[string]string{
		"run_id":      runID.String(),
		"stage_id":    stageID.String(),
		"workflow_id": "feature_change",
		"stage":       "claude-code",
	}
	for k, v := range want {
		if call.Variables[k] != v {
			t.Errorf("variables[%q] = %q, want %q", k, call.Variables[k], v)
		}
	}
	if _, ok := call.Variables["parent_run_id"]; ok {
		t.Errorf("top-level run must omit parent_run_id, got %q", call.Variables["parent_run_id"])
	}
}

func TestGitLabCI_TriggerStage_DecomposedChild(t *testing.T) {
	fc := &fakePipelineClient{}
	g := &GitLabCI{Client: fc}
	parent := uuid.New()
	err := g.TriggerStage(context.Background(), TriggerParams{
		RunID: uuid.New(), StageID: uuid.New(), WorkflowID: "feature_change",
		StageExecutorRef: "claude-code", Repo: "group/proj",
		Scope: gitLabScope("77"), Ref: "fishhawk/run-abcd1234/slice-2",
		DecomposedFrom: uuidPtr(parent), SliceIndex: intPtr(2),
	})
	if err != nil {
		t.Fatalf("TriggerStage: %v", err)
	}
	call := fc.calls[0]
	if call.Ref != "fishhawk/run-abcd1234/slice-2" {
		t.Errorf("ref = %q, want the decomposed-child slice branch", call.Ref)
	}
	if got := call.Variables["parent_run_id"]; got != parent.String() {
		t.Errorf("parent_run_id = %q, want %q", got, parent.String())
	}
}

// --- fail-closed / defensive branches ---

func TestGitLabCI_TriggerStage_NilClientSkips(t *testing.T) {
	g := &GitLabCI{Client: nil}
	if err := g.TriggerStage(context.Background(), TriggerParams{
		RunID: uuid.New(), Scope: gitLabScope("1"), Ref: "fishhawk/run-x",
	}); err != nil {
		t.Fatalf("nil-client skip must return nil, got %v", err)
	}
}

func TestGitLabCI_TriggerStage_ZeroScopeSkips(t *testing.T) {
	fc := &fakePipelineClient{}
	g := &GitLabCI{Client: fc}
	if err := g.TriggerStage(context.Background(), TriggerParams{
		RunID: uuid.New(), Ref: "fishhawk/run-x", // zero Scope
	}); err != nil {
		t.Fatalf("zero-scope skip must return nil, got %v", err)
	}
	if len(fc.calls) != 0 {
		t.Errorf("zero scope must fire no pipeline, got %d", len(fc.calls))
	}
}

func TestGitLabCI_TriggerStage_EmptyRefErrors(t *testing.T) {
	fc := &fakePipelineClient{}
	g := &GitLabCI{Client: fc}
	err := g.TriggerStage(context.Background(), TriggerParams{
		RunID: uuid.New(), Scope: gitLabScope("1"), // no Ref
	})
	if err == nil {
		t.Fatal("empty ref must error (the run branch is the required pipeline ref)")
	}
	if len(fc.calls) != 0 {
		t.Errorf("empty ref must fire no pipeline, got %d", len(fc.calls))
	}
}

func TestGitLabCI_TriggerStage_BadScopeErrors(t *testing.T) {
	fc := &fakePipelineClient{}
	g := &GitLabCI{Client: fc}
	for _, ref := range []string{"gitlab:notanumber", "github:5", "5"} {
		err := g.TriggerStage(context.Background(), TriggerParams{
			RunID: uuid.New(), Scope: forge.FromRef(ref), Ref: "fishhawk/run-x",
		})
		if err == nil {
			t.Errorf("scope %q must error", ref)
		}
	}
	if len(fc.calls) != 0 {
		t.Errorf("bad scope must fire no pipeline, got %d", len(fc.calls))
	}
}

func TestGitLabCI_TriggerStage_PropagatesError(t *testing.T) {
	sentinel := errors.New("boom")
	fc := &fakePipelineClient{err: sentinel}
	g := &GitLabCI{Client: fc}
	err := g.TriggerStage(context.Background(), TriggerParams{
		RunID: uuid.New(), Scope: gitLabScope("1"), Ref: "fishhawk/run-x",
	})
	if !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want sentinel propagated", err)
	}
}

// --- Resolver routes a resolved gitlab_ci run to the GitLabCI backend ---

func TestResolver_RoutesResolvedGitLabCI(t *testing.T) {
	rr := &Resolver{
		Registry: Registry{
			run.RunnerKindGitHubActions: &GitHubActions{},
			run.RunnerKindLocal:         &Local{},
			run.RunnerKindGitLabCI:      &GitLabCI{},
		},
	}
	r := &run.Run{ID: uuid.New(), RunnerKind: run.RunnerKindGitLabCI, RunnerKindResolved: true}
	got := rr.Resolve(context.Background(), r)
	if got.Kind() != run.RunnerKindGitLabCI {
		t.Errorf("Resolve() backend kind = %q, want gitlab_ci", got.Kind())
	}
}
