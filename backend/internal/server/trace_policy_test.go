package server

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
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
func (r *policyRunRepo) RetryRun(context.Context, uuid.UUID, run.State) (*run.Run, error) {
	return nil, errors.New("not used")
}
func (r *policyRunRepo) SetRunPullRequestURL(context.Context, uuid.UUID, string) (*run.Run, error) {
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

func (r *policyRunRepo) ListStagesAwaitingChildren(context.Context) ([]*run.Stage, error) {
	return nil, errors.New("not used")
}

func (r *policyRunRepo) ListStagesDispatched(context.Context) ([]*run.Stage, error) {
	return nil, nil
}

func (r *policyRunRepo) RetryStage(_ context.Context, id uuid.UUID, to run.StageState) (*run.Stage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stage != nil && r.stage.ID == id {
		r.stage.State = to
		r.stage.FailureCategory = nil
		r.stage.FailureReason = nil
		return r.stage, nil
	}
	return nil, run.ErrNotFound
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
version: "0.3"
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

// testWorkflowSpecRequireTests adds a tests_added_or_updated
// required-outcome so an empty (present-but-zero-file) diff fails the
// outcome — the only policy path an empty implement diff can fail
// (#692). Used by the empty-diff → category-C reclassification test.
const testWorkflowSpecRequireTests = `
version: "0.3"
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
          - required_outcomes:
              - tests_added_or_updated
        gates:
          - type: approval
            approvers:
              any_of: [eng_team]
            sla: 4_hours
`

// testWorkflowSpecInvalidGlob carries a malformed forbidden_paths
// pattern (`[bad` — an unterminated character class). policy.matchAny
// validates the glob against an empty path before the per-file loop,
// so this emits an `invalid pattern` violation even on an empty diff.
// Used to prove the noDiffCaptured carve-out keeps a genuine
// spec-config category-B from being reclassified to retryable C.
const testWorkflowSpecInvalidGlob = `
version: "0.3"
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
              - "[bad"
        gates:
          - type: approval
            approvers:
              any_of: [eng_team]
            sla: 4_hours
`

// newPolicyTraceServer wires a Server with the full policy
// re-eval stack: signing, tracestore, audit, run repo (with stage
// + run rows). The run row's WorkflowSpec is populated from
// testWorkflowSpec so the trace handler's policy re-eval reads
// constraints from the cache (#283) rather than refetching from
// GitHub.
func newPolicyTraceServer(t *testing.T, files []map[string]string) (
	*Server, *signingFake, *policyRunRepo, *auditFake,
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
		// Tests in this file assert awaiting_approval as the
		// post-policy-pass state. Per #207's gate-aware transition,
		// that requires the stage to be marked as gated. The stage
		// TYPE here is informational; what drives the trace
		// handler's terminal state is RequiresApproval.
		RequiresApproval: true,
	}
	runRow := &run.Run{
		ID:             runID,
		Repo:           "kuhlman-labs/example",
		WorkflowID:     "feature_change",
		WorkflowSHA:    "abc123",
		InstallationID: &installation,
		// Cache the spec on the run row (#283). The trace handler's
		// policy re-eval reads constraints from here instead of
		// refetching from GitHub.
		WorkflowSpec: []byte(testWorkflowSpec),
	}
	repo := newPolicyRunRepo(stage, runRow)

	s, sf, _, au := newTraceServer(t)
	s.cfg.RunRepo = repo
	_ = files
	return s, sf, repo, au
}

func TestShipTrace_PolicyReEval_CleanDiff_AwaitingApproval(t *testing.T) {
	clean := []map[string]string{
		{"path": "backend/main.go", "status": "M"},
		{"path": "backend/main_test.go", "status": "A"},
	}
	s, sf, repo, au := newPolicyTraceServer(t, clean)
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
}

func TestShipTrace_PolicyReEval_ForbiddenPath_FailedB(t *testing.T) {
	violating := []map[string]string{
		{"path": "infra/terraform.tf", "status": "M"},
		{"path": "backend/main.go", "status": "M"},
	}
	s, sf, repo, au := newPolicyTraceServer(t, violating)
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

func TestShipTrace_PolicyReEval_EmptyDiff_FailedC_Retryable(t *testing.T) {
	// #691/#692: an implement stage whose bundle carries a
	// PRESENT-but-empty git_diff event (num_files:0) is the staging-bug
	// signature. With a tests_added_or_updated required-outcome the
	// empty diff fails policy, but the failure is a capture miss, not a
	// constraint breach — so the trace handler stamps a retryable
	// category-C with a no_diff_captured reason rather than an
	// un-redrivable category-B. The seam check below then drives
	// run.RetryStage across the trace-handler → run-repo boundary and
	// asserts failed → pending, proving the recovery path the issue
	// says is missing works end to end.
	s, sf, repo, _ := newPolicyTraceServer(t, nil)
	repo.runRow.WorkflowSpec = []byte(testWorkflowSpecRequireTests)
	// An empty non-nil slice emits a git_diff event with num_files:0,
	// distinct from the nil/no-event case (no_diff_in_bundle skip→pass).
	bundle := makeTestBundle(t, []map[string]string{})
	priv, _ := sf.issue(t, repo.runRow.ID)

	w := shipRequest(t, s, repo.runRow.ID, repo.stage.ID, "raw", priv, bundle, "")
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
	}

	if repo.stage.State != run.StageStateFailed {
		t.Fatalf("stage state = %q, want failed", repo.stage.State)
	}
	if repo.stage.FailureCategory == nil || *repo.stage.FailureCategory != run.FailureC {
		t.Fatalf("FailureCategory = %v, want C", repo.stage.FailureCategory)
	}
	if repo.stage.FailureReason == nil || !strings.HasPrefix(*repo.stage.FailureReason, "no_diff_captured:") {
		t.Errorf("FailureReason = %v, want a no_diff_captured reason", repo.stage.FailureReason)
	}

	// Seam: the resulting category-C failed stage must be retryable —
	// failed → pending, NOT ErrRetryNotApplicable (which is what a
	// category-B would yield).
	dec, err := run.RetryStage(context.Background(), repo, repo.stage.ID, run.RetryOptions{})
	if err != nil {
		t.Fatalf("RetryStage on category-C failed stage = %v, want nil", err)
	}
	if dec.Stage.State != run.StageStatePending {
		t.Errorf("post-retry state = %q, want pending", dec.Stage.State)
	}
}

func TestShipTrace_PolicyReEval_EmptyDiff_InvalidPattern_StaysFailedB(t *testing.T) {
	// Carve-out (correctness): an empty diff combined with a malformed
	// forbidden_paths glob emits an `invalid pattern` violation. That's
	// a genuine spec-config category-B, so noDiffCaptured must NOT
	// reclassify it to retryable C even though the diff is empty.
	s, sf, repo, _ := newPolicyTraceServer(t, nil)
	repo.runRow.WorkflowSpec = []byte(testWorkflowSpecInvalidGlob)
	bundle := makeTestBundle(t, []map[string]string{})
	priv, _ := sf.issue(t, repo.runRow.ID)

	w := shipRequest(t, s, repo.runRow.ID, repo.stage.ID, "raw", priv, bundle, "")
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
	}

	if repo.stage.State != run.StageStateFailed {
		t.Fatalf("stage state = %q, want failed", repo.stage.State)
	}
	if repo.stage.FailureCategory == nil || *repo.stage.FailureCategory != run.FailureB {
		t.Errorf("FailureCategory = %v, want B (invalid-pattern config error must not be reclassified)", repo.stage.FailureCategory)
	}
}

func TestShipTrace_PolicyReEval_NoDiffEvent_EmitsSkippedAndAdvances(t *testing.T) {
	// Older-runner case (or stage that doesn't produce a diff): no
	// git_diff event in the bundle. The trace handler emits a
	// policy_evaluated audit entry with `skip_reason =
	// no_diff_in_bundle` (#283 — was previously rendered as a
	// generic pass via #247; #283 made the skip reason structured
	// so the SPA can render it precisely).
	s, sf, repo, au := newPolicyTraceServer(t, nil)
	bundle := makeTestBundle(t, nil) // no files = no git_diff event
	priv, _ := sf.issue(t, repo.runRow.ID)

	w := shipRequest(t, s, repo.runRow.ID, repo.stage.ID, "raw", priv, bundle, "")
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d", w.Code)
	}
	if repo.stage.State != run.StageStateAwaitingApproval {
		t.Errorf("stage state = %q, want awaiting_approval", repo.stage.State)
	}
	au.mu.Lock()
	defer au.mu.Unlock()
	var policyEntry *audit.ChainAppendParams
	for i := range au.appended {
		if au.appended[i].Category == "policy_evaluated" {
			policyEntry = &au.appended[i]
			break
		}
	}
	if policyEntry == nil {
		t.Fatalf("expected policy_evaluated audit entry; got %v", categoryNames(au.appended))
	}
	if !strings.Contains(string(policyEntry.Payload), `"skip_reason":"no_diff_in_bundle"`) {
		t.Errorf("payload missing skip_reason=no_diff_in_bundle:\n%s", policyEntry.Payload)
	}
}

func TestShipTrace_PolicyReEval_NoCachedSpec_EmitsSkippedAndProceedsToAwaitingApproval(t *testing.T) {
	// Legacy run row (pre-#283 migration) has no cached spec bytes.
	// The trace handler must emit a `policy_evaluated` audit entry
	// with `skip_reason = spec_unavailable` instead of leaving the
	// SPA's <PolicySection> stuck on "pending" (the bug #283 fixed).
	// Pre-#283 this code path tried to refetch the spec from GitHub
	// using `runRow.WorkflowSHA` as the contents-API ref — but the
	// SHA was a blob SHA, not a commit ref, so GitHub returned 404
	// for every call.
	clean := []map[string]string{{"path": "backend/main.go", "status": "M"}}
	s, sf, repo, au := newPolicyTraceServer(t, clean)
	repo.runRow.WorkflowSpec = nil // simulate legacy row
	bundleBytes := makeTestBundle(t, clean)
	priv, _ := sf.issue(t, repo.runRow.ID)

	w := shipRequest(t, s, repo.runRow.ID, repo.stage.ID, "raw", priv, bundleBytes, "")
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
	}
	// Best-effort: a missing cache shouldn't black-hole the stage.
	if repo.stage.State != run.StageStateAwaitingApproval {
		t.Errorf("stage state = %q, want awaiting_approval", repo.stage.State)
	}
	// The audit row STILL lands, with the skip_reason populated so
	// the SPA can render the reason instead of "pending."
	au.mu.Lock()
	defer au.mu.Unlock()
	var policyEntry *audit.ChainAppendParams
	for i := range au.appended {
		if au.appended[i].Category == "policy_evaluated" {
			policyEntry = &au.appended[i]
			break
		}
	}
	if policyEntry == nil {
		t.Fatalf("expected a policy_evaluated audit entry with skip_reason; got categories %v",
			categoryNames(au.appended))
	}
	if !strings.Contains(string(policyEntry.Payload), `"skip_reason":"spec_unavailable"`) {
		t.Errorf("payload missing skip_reason=spec_unavailable:\n%s", policyEntry.Payload)
	}
}

// E8.5 (#163): a bundle whose manifest carries `agent_failed: true`
// flips the stage to failed-A — bypassing both the policy
// re-evaluation and the awaiting_approval advance.
func TestShipTrace_AgentFailed_TransitionsToFailedA(t *testing.T) {
	s, sf, repo, _ := newPolicyTraceServer(t, nil)
	bundleBytes := makeTestBundleAgentFailed(t, "agent process exited 137 (OOM)")
	priv, _ := sf.issue(t, repo.runRow.ID)

	w := shipRequest(t, s, repo.runRow.ID, repo.stage.ID, "raw", priv, bundleBytes, "")
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
	}

	if repo.stage.State != run.StageStateFailed {
		t.Errorf("stage state = %q, want failed", repo.stage.State)
	}
	if repo.stage.FailureCategory == nil || *repo.stage.FailureCategory != run.FailureA {
		t.Errorf("FailureCategory = %v, want A", repo.stage.FailureCategory)
	}
	if repo.stage.FailureReason == nil || *repo.stage.FailureReason != "agent process exited 137 (OOM)" {
		t.Errorf("FailureReason = %v", repo.stage.FailureReason)
	}
}

func TestShipTrace_AgentFailedNoReason_StampsFallbackString(t *testing.T) {
	s, sf, repo, _ := newPolicyTraceServer(t, nil)
	bundleBytes := makeTestBundleAgentFailed(t, "") // no reason supplied
	priv, _ := sf.issue(t, repo.runRow.ID)

	w := shipRequest(t, s, repo.runRow.ID, repo.stage.ID, "raw", priv, bundleBytes, "")
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d", w.Code)
	}
	if repo.stage.FailureReason == nil || *repo.stage.FailureReason == "" {
		t.Errorf("FailureReason should fall back to a non-empty string when reason is omitted")
	}
}

// makeTestBundleAgentFailed builds a *.jsonl.gz with a manifest
// stamped with agent_failed=true (E8.5). No git_diff event — when
// the agent fails, no plan exists to evaluate.
func makeTestBundleAgentFailed(t *testing.T, reason string) []byte {
	t.Helper()
	manifestPayload := map[string]any{
		"bundle_schema": "v1",
		"agent_failed":  true,
	}
	if reason != "" {
		manifestPayload["agent_failure_reason"] = reason
	}
	mp, _ := json.Marshal(manifestPayload)
	manifestLine, _ := json.Marshal(map[string]any{
		"seq": 1, "kind": "manifest", "data": json.RawMessage(mp),
	})
	var raw bytes.Buffer
	raw.Write(manifestLine)
	raw.WriteByte('\n')

	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	_, _ = w.Write(raw.Bytes())
	_ = w.Close()
	return gz.Bytes()
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

// categoryNames is a tiny helper for error messages: list the
// audit categories that landed on the fake so the assertion can
// say "got [x, y, z]" without a manual loop in each test.
func categoryNames(entries []audit.ChainAppendParams) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Category)
	}
	return out
}
