package server

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/policy"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// stubWorkflowSpecFetcher returns a canned workflow spec.
type stubWorkflowSpecFetcher struct {
	content []byte
	sha     string
	getErr  error
	calls   int
	mu      sync.Mutex
	gotInst int64
	gotRepo githubclient.RepoRef
	gotRef  string
}

func (s *stubWorkflowSpecFetcher) GetWorkflowSpec(_ context.Context, installationID int64, repo githubclient.RepoRef, ref string) (*githubclient.FileContent, error) {
	s.mu.Lock()
	s.calls++
	s.gotInst = installationID
	s.gotRepo = repo
	s.gotRef = ref
	s.mu.Unlock()
	if s.getErr != nil {
		return nil, s.getErr
	}
	return &githubclient.FileContent{
		Path:    "workflows.yaml",
		Content: s.content,
		SHA:     s.sha,
	}, nil
}

// policyRunRepo is the test fake for the trace handler's policy
// re-eval path. Combines GetStage + GetRun + TransitionStage with
// recorded transitions.
type policyRunRepo struct {
	mu          sync.Mutex
	stage       *run.Stage
	runRow      *run.Run
	transitions []approvalTransition
}

func newPolicyRunRepo(stage *run.Stage, runRow *run.Run) *policyRunRepo {
	return &policyRunRepo{stage: stage, runRow: runRow}
}

func (r *policyRunRepo) GetStage(_ context.Context, id uuid.UUID) (*run.Stage, error) {
	if r.stage != nil && r.stage.ID == id {
		return r.stage, nil
	}
	return nil, run.ErrNotFound
}

func (r *policyRunRepo) GetRun(_ context.Context, id uuid.UUID) (*run.Run, error) {
	if r.runRow != nil && r.runRow.ID == id {
		return r.runRow, nil
	}
	return nil, run.ErrNotFound
}

func (r *policyRunRepo) TransitionStage(_ context.Context, id uuid.UUID, to run.StageState, c *run.StageCompletion) (*run.Stage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.transitions = append(r.transitions, approvalTransition{StageID: id, To: to, Completion: c})
	if r.stage != nil && r.stage.ID == id {
		r.stage.State = to
		if c != nil {
			r.stage.FailureCategory = c.FailureCategory
			r.stage.FailureReason = c.FailureReason
		}
	}
	return r.stage, nil
}

func (r *policyRunRepo) CreateRun(context.Context, run.CreateRunParams) (*run.Run, error) {
	return nil, errors.New("not used")
}
func (r *policyRunRepo) GetRunByIdempotencyKey(context.Context, string, string) (*run.Run, error) {
	return nil, run.ErrNotFound
}
func (r *policyRunRepo) ListRuns(context.Context, run.ListRunsFilter) ([]*run.Run, error) {
	return nil, errors.New("not used")
}
func (r *policyRunRepo) TransitionRun(context.Context, uuid.UUID, run.State) (*run.Run, error) {
	return nil, errors.New("not used")
}
func (r *policyRunRepo) CreateStage(context.Context, run.CreateStageParams) (*run.Stage, error) {
	return nil, errors.New("not used")
}
func (r *policyRunRepo) ListStagesForRun(context.Context, uuid.UUID) ([]*run.Stage, error) {
	return nil, errors.New("not used")
}
func (r *policyRunRepo) ListStagesAwaitingApproval(context.Context) ([]*run.Stage, error) {
	return nil, errors.New("not used")
}

func (r *policyRunRepo) ListStagesDispatched(context.Context) ([]*run.Stage, error) {
	return nil, nil
}

// makeTestBundle builds a *.jsonl.gz with a manifest, an optional
// git_diff event, and a trailer. Used to drive the trace handler
// end-to-end without depending on the runner package.
func makeTestBundle(t *testing.T, files []map[string]string) []byte {
	t.Helper()
	var raw bytes.Buffer
	manifest, _ := json.Marshal(map[string]any{"bundle_schema": "v1"})
	manifestLine, _ := json.Marshal(map[string]any{"seq": 1, "kind": "manifest", "data": json.RawMessage(manifest)})
	raw.Write(manifestLine)
	raw.WriteByte('\n')

	if files != nil {
		payload, _ := json.Marshal(map[string]any{
			"kind":      "name_status",
			"base_ref":  "origin/main",
			"files":     files,
			"num_files": len(files),
		})
		diffLine, _ := json.Marshal(map[string]any{
			"seq": 2, "kind": "git_diff", "data": json.RawMessage(payload),
		})
		raw.Write(diffLine)
		raw.WriteByte('\n')
	}

	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	_, _ = w.Write(raw.Bytes())
	_ = w.Close()
	return gz.Bytes()
}

const testWorkflowSpec = `
version: "0.1"
roles:
  eng_team:
    members: ["@org/eng"]
workflows:
  feature_change:
    description: Test workflow
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        constraints:
          - forbidden_paths:
              - "infra/**"
          - max_files_changed: 10
        gates:
          - type: approval
            approvers:
              any_of: [eng_team]
            sla: 4_hours
`

// newPolicyTraceServer wires a Server with the full policy
// re-eval stack: signing, tracestore, audit, run repo (with stage
// + run rows), and a workflow spec fetcher stub.
func newPolicyTraceServer(t *testing.T, files []map[string]string) (
	*Server, *signingFake, *policyRunRepo, *auditFake, *stubWorkflowSpecFetcher,
) {
	t.Helper()
	stageID := uuid.New()
	runID := uuid.New()
	installation := int64(99)

	stage := &run.Stage{
		ID:           stageID,
		RunID:        runID,
		Type:         run.StageTypeImplement,
		ExecutorKind: run.ExecutorAgent,
		ExecutorRef:  "claude-code",
		State:        run.StageStateDispatched,
	}
	runRow := &run.Run{
		ID:             runID,
		Repo:           "kuhlman-labs/example",
		WorkflowID:     "feature_change",
		WorkflowSHA:    "abc123",
		InstallationID: &installation,
	}
	repo := newPolicyRunRepo(stage, runRow)
	gh := &stubWorkflowSpecFetcher{content: []byte(testWorkflowSpec), sha: "abc123"}

	s, sf, _, au := newTraceServer(t)
	s.cfg.RunRepo = repo
	s.traceWorkflowSpecOverride = gh
	_ = files
	return s, sf, repo, au, gh
}

func TestShipTrace_PolicyReEval_CleanDiff_AwaitingApproval(t *testing.T) {
	clean := []map[string]string{
		{"path": "backend/main.go", "status": "M"},
		{"path": "backend/main_test.go", "status": "A"},
	}
	s, sf, repo, au, gh := newPolicyTraceServer(t, clean)
	bundle := makeTestBundle(t, clean)
	priv, _ := sf.issue(t, repo.runRow.ID)

	w := shipRequest(t, s, repo.runRow.ID, repo.stage.ID, "raw", priv, bundle, "")
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
	}

	if repo.stage.State != run.StageStateAwaitingApproval {
		t.Errorf("stage state = %q, want awaiting_approval", repo.stage.State)
	}
	// Audit appended: trace_uploaded + policy_evaluated.
	au.mu.Lock()
	categories := []string{}
	for _, e := range au.appended {
		categories = append(categories, e.Category)
	}
	au.mu.Unlock()
	hasPolicy := false
	for _, c := range categories {
		if c == "policy_evaluated" {
			hasPolicy = true
		}
	}
	if !hasPolicy {
		t.Errorf("expected policy_evaluated audit entry, got categories %v", categories)
	}
	if gh.calls != 1 {
		t.Errorf("workflow spec fetched %d times, want 1", gh.calls)
	}
}

func TestShipTrace_PolicyReEval_ForbiddenPath_FailedB(t *testing.T) {
	violating := []map[string]string{
		{"path": "infra/terraform.tf", "status": "M"},
		{"path": "backend/main.go", "status": "M"},
	}
	s, sf, repo, au, _ := newPolicyTraceServer(t, violating)
	bundle := makeTestBundle(t, violating)
	priv, _ := sf.issue(t, repo.runRow.ID)

	w := shipRequest(t, s, repo.runRow.ID, repo.stage.ID, "raw", priv, bundle, "")
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
	}

	if repo.stage.State != run.StageStateFailed {
		t.Errorf("stage state = %q, want failed", repo.stage.State)
	}
	if repo.stage.FailureCategory == nil || *repo.stage.FailureCategory != run.FailureB {
		t.Errorf("FailureCategory = %v, want B", repo.stage.FailureCategory)
	}

	au.mu.Lock()
	defer au.mu.Unlock()
	hasPolicy, hasViolations := false, false
	for _, e := range au.appended {
		if e.Category == "policy_evaluated" {
			hasPolicy = true
			if bytes.Contains(e.Payload, []byte("forbidden_paths")) {
				hasViolations = true
			}
		}
	}
	if !hasPolicy {
		t.Errorf("expected policy_evaluated audit entry")
	}
	if !hasViolations {
		t.Errorf("expected forbidden_paths in payload, got %+v", au.appended)
	}
}

func TestShipTrace_PolicyReEval_NoDiffEvent_FallsBackToAwaitingApproval(t *testing.T) {
	// Older-runner case (or stage that doesn't produce a diff): no
	// git_diff event in the bundle. Re-eval is skipped; behavior
	// matches the pre-E3.13 path (advance to awaiting_approval).
	s, sf, repo, _, _ := newPolicyTraceServer(t, nil)
	bundle := makeTestBundle(t, nil) // no files = no git_diff event
	priv, _ := sf.issue(t, repo.runRow.ID)

	w := shipRequest(t, s, repo.runRow.ID, repo.stage.ID, "raw", priv, bundle, "")
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d", w.Code)
	}
	if repo.stage.State != run.StageStateAwaitingApproval {
		t.Errorf("stage state = %q, want awaiting_approval", repo.stage.State)
	}
}

func TestShipTrace_PolicyReEval_SpecFetchFails_ProceedsToAwaitingApproval(t *testing.T) {
	clean := []map[string]string{{"path": "backend/main.go", "status": "M"}}
	s, sf, repo, _, gh := newPolicyTraceServer(t, clean)
	gh.getErr = errors.New("github down")
	bundle := makeTestBundle(t, clean)
	priv, _ := sf.issue(t, repo.runRow.ID)

	w := shipRequest(t, s, repo.runRow.ID, repo.stage.ID, "raw", priv, bundle, "")
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
	}
	// Best-effort: spec fetch failure shouldn't black-hole the
	// stage. Advance to awaiting_approval.
	if repo.stage.State != run.StageStateAwaitingApproval {
		t.Errorf("stage state = %q, want awaiting_approval", repo.stage.State)
	}
}

func TestMergeConstraints(t *testing.T) {
	in := []spec.Constraint{
		{ForbiddenPaths: []string{"infra/**"}},
		{MaxFilesChanged: 10},
		{ForbiddenPaths: []string{".github/**"}},
		{AllowedPaths: []string{"backend/**"}},
		{RequiredOutcomes: []string{"tests_added_or_updated"}},
		{MaxFilesChanged: 5}, // more restrictive — wins
	}
	got := mergeConstraints(in)
	if len(got.ForbiddenPaths) != 2 {
		t.Errorf("ForbiddenPaths = %v, want 2 entries", got.ForbiddenPaths)
	}
	if len(got.AllowedPaths) != 1 {
		t.Errorf("AllowedPaths = %v", got.AllowedPaths)
	}
	if got.MaxFilesChanged != 5 {
		t.Errorf("MaxFilesChanged = %d, want 5 (most restrictive)", got.MaxFilesChanged)
	}
	if len(got.RequiredOutcomes) != 1 {
		t.Errorf("RequiredOutcomes = %v", got.RequiredOutcomes)
	}
}

func TestIsEmptyConstraints(t *testing.T) {
	if !isEmptyConstraints(policy.Constraints{}) {
		t.Error("empty Constraints should be empty")
	}
	if isEmptyConstraints(policy.Constraints{MaxFilesChanged: 1}) {
		t.Error("non-empty Constraints flagged as empty")
	}
}
