package server

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
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
	// (nil, nil) = the run has no decomposition children, so run.RetryStage's
	// decomposed-parent detection (#1891) treats a failed implement stage as a
	// standalone retry (→ pending), not an awaiting_children restore. Returning
	// an error here would fail the retry closed (the fail-closed detection
	// path), breaking the category-C retryability seam this file asserts.
	return nil, nil
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
func (r *policyRunRepo) ListReviewStagesAwaitingApproval(context.Context) ([]*run.Stage, error) {
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

// gateEvidenceSpec describes the runner-shaped `gate_evidence` event a
// bundle should carry. Nil verifyRuns + nil summary means "emit no
// gate_evidence event at all" — the no-evidence case.
type gateEvidenceSpec struct {
	verifyRuns    []map[string]any
	verifySummary map[string]any
	// rawPayload, when non-empty, is written as the event's `data`
	// VERBATIM instead of composing one from the fields above. Used to
	// feed a structurally malformed payload — valid JSON at the event
	// line level, undecodable into bundle.GateEvidence — through the
	// extractor's error path (#1886 fix-up).
	rawPayload json.RawMessage
}

// makeTestBundleWithGateEvidence extends makeTestBundle with a
// runner-shaped `gate_evidence` event (#963). The payload is written as
// raw maps, NOT via bundle.GateEvidence, so the test exercises the
// actual json tags of the runner↔backend wire contract: a tag drift
// between the runner's composer and the backend's extractor shows up
// here as a missing signal rather than silently reading a zero value.
func makeTestBundleWithGateEvidence(t *testing.T, files []map[string]string, ge *gateEvidenceSpec) []byte {
	t.Helper()
	base := makeTestBundle(t, files)
	if ge == nil {
		return base
	}

	// Unpack the gz produced above, append the event, re-pack.
	zr, err := gzip.NewReader(bytes.NewReader(base))
	if err != nil {
		t.Fatalf("gunzip base bundle: %v", err)
	}
	raw, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("read base bundle: %v", err)
	}

	payload := ge.rawPayload
	if len(payload) == 0 {
		payloadMap := map[string]any{}
		if ge.verifyRuns != nil {
			payloadMap["verify_runs"] = ge.verifyRuns
		}
		if ge.verifySummary != nil {
			payloadMap["verify_summary"] = ge.verifySummary
		}
		var err error
		payload, err = json.Marshal(payloadMap)
		if err != nil {
			t.Fatalf("marshal gate_evidence payload: %v", err)
		}
	}
	line, err := json.Marshal(map[string]any{
		"seq": 3, "kind": "gate_evidence", "data": json.RawMessage(payload),
	})
	if err != nil {
		t.Fatalf("marshal gate_evidence line: %v", err)
	}
	raw = append(raw, line...)
	raw = append(raw, '\n')

	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	if _, err := w.Write(raw); err != nil {
		t.Fatalf("gzip: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return gz.Bytes()
}

// testWorkflowSpecRequireVerification opts the implement stage into the
// workflow-v1.5 `verification_reported` required outcome (#1886 /
// ADR-059). Pinned at version "1.5" — the outcome is workflow-v1-only.
const testWorkflowSpecRequireVerification = `
version: "1.5"
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
          verify:
            command: scripts/test verify
        constraints:
          - required_outcomes:
              - verification_reported
        gates:
          - type: approval
            approvers:
              any_of: [eng_team]
            sla: 4_hours
`

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

// GetRunAccountID satisfies the REQUIRED run.AccountGetter portion of
// run.Repository (E44.11 / #2074). Untenanted: this fake's runs carry no
// tenant account, matching its pre-promotion effective behavior.
func (*policyRunRepo) GetRunAccountID(_ context.Context, _ uuid.UUID) (string, error) {
	return "", nil
}

// decodePolicyPayload returns the first appended policy_evaluated
// audit payload, decoded. Fails the test when none was written.
func decodePolicyPayload(t *testing.T, au *auditFake) policy.EvaluationPayload {
	t.Helper()
	au.mu.Lock()
	defer au.mu.Unlock()
	for i := range au.appended {
		if au.appended[i].Category != "policy_evaluated" {
			continue
		}
		var pl policy.EvaluationPayload
		if err := json.Unmarshal(au.appended[i].Payload, &pl); err != nil {
			t.Fatalf("decode policy_evaluated payload: %v", err)
		}
		return pl
	}
	t.Fatalf("expected policy_evaluated audit entry; got %v", categoryNames(au.appended))
	return policy.EvaluationPayload{}
}

// TestShipTrace_PolicyReEval_VerificationReported is the CROSS-BOUNDARY
// end-to-end assertion for #1886 / ADR-059: it drives a real trace
// upload carrying a runner-shaped `gate_evidence` event through the
// bundle extractor, the signal derivation in trace.go, and the policy
// evaluator, and asserts the emitted `policy_evaluated` audit payload.
// scope.files for this change spans all four layers, so THIS test —
// not the per-layer units — is what fails if the gate_evidence field
// names drift between the runner and the backend.
func TestShipTrace_PolicyReEval_VerificationReported(t *testing.T) {
	changed := []map[string]string{{"path": "backend/main.go", "status": "M"}}

	cases := []struct {
		name            string
		evidence        *gateEvidenceSpec
		wantPassed      bool
		wantOutcome     string // expected applied_constraints.verification.outcome ("" = nil signal)
		wantViolationIs string
		wantState       run.StageState
	}{
		{
			name: "verify_summary passed satisfies",
			evidence: &gateEvidenceSpec{
				verifySummary: map[string]any{"outcome": "passed", "iterations": 1, "max_iterations": 3},
				verifyRuns: []map[string]any{
					{"command": "scripts/test verify", "exit_code": 0, "outcome": "passed"},
				},
			},
			wantPassed:  true,
			wantOutcome: "passed",
			wantState:   run.StageStateAwaitingApproval,
		},
		{
			name: "verify_summary failed violates",
			evidence: &gateEvidenceSpec{
				verifySummary: map[string]any{"outcome": "failed", "iterations": 3, "max_iterations": 3},
				verifyRuns: []map[string]any{
					{"command": "scripts/test verify", "exit_code": 1, "outcome": "failed"},
				},
			},
			wantPassed:      false,
			wantOutcome:     "failed",
			wantViolationIs: "scripts/test verify",
			wantState:       run.StageStateFailed,
		},
		{
			// A skipped verify gate is not a passed gate.
			name: "verify_summary skipped violates",
			evidence: &gateEvidenceSpec{
				verifySummary: map[string]any{"outcome": "skipped"},
			},
			wantPassed:      false,
			wantOutcome:     "skipped",
			wantViolationIs: `"skipped"`,
			wantState:       run.StageStateFailed,
		},
		{
			// Superseded case (#1205): the verify-fix loop's first
			// iteration FAILED and was absorbed, the terminal run
			// passed, and no summary was emitted. The last
			// non-superseded run is authoritative → satisfied.
			name: "superseded failing run then passing terminal run satisfies",
			evidence: &gateEvidenceSpec{
				verifyRuns: []map[string]any{
					{"command": "scripts/test verify", "exit_code": 1, "outcome": "failed", "superseded": true},
					{"command": "scripts/test verify", "exit_code": 0, "outcome": "passed"},
				},
			},
			wantPassed:  true,
			wantOutcome: "passed",
			wantState:   run.StageStateAwaitingApproval,
		},
		{
			// No summary, single-shot committed gate that failed.
			name: "last verify_run failed violates",
			evidence: &gateEvidenceSpec{
				verifyRuns: []map[string]any{
					{"command": "scripts/test verify", "exit_code": 2, "outcome": "failed"},
				},
			},
			wantPassed:      false,
			wantOutcome:     "failed",
			wantViolationIs: "scripts/test verify",
			wantState:       run.StageStateFailed,
		},
		{
			// No gate_evidence event at all (older runner, or a stage
			// that ran no gates). Fail-closed: nil signal → violation.
			name:            "no gate_evidence violates",
			evidence:        nil,
			wantPassed:      false,
			wantOutcome:     "",
			wantViolationIs: "no verification evidence in trace",
			wantState:       run.StageStateFailed,
		},
		{
			// gate_evidence present but carrying neither a summary nor
			// any verify run — nothing to assert on, so still nil.
			name:            "empty gate_evidence violates",
			evidence:        &gateEvidenceSpec{},
			wantPassed:      false,
			wantOutcome:     "",
			wantViolationIs: "no verification evidence in trace",
			wantState:       run.StageStateFailed,
		},
		{
			// The OTHER flavor of the extractor's error branch: the
			// event is present and its `data` is valid JSON, but it
			// does not decode into bundle.GateEvidence (verify_runs is
			// a number, not an array). ExtractGateEvidence returns a
			// parse error rather than ErrNoGateEvidence, and the
			// derivation must fail closed exactly the same way — a
			// garbled payload is never a passed gate.
			name:            "malformed gate_evidence violates",
			evidence:        &gateEvidenceSpec{rawPayload: json.RawMessage(`{"verify_runs":42}`)},
			wantPassed:      false,
			wantOutcome:     "",
			wantViolationIs: "no verification evidence in trace",
			wantState:       run.StageStateFailed,
		},
		{
			// Summary precedence, conflict direction 1: the terminal
			// non-superseded run PASSED but the once-per-stage summary
			// says failed. The summary wins, so this violates — a
			// last-run-wins implementation would pass it.
			name: "failed summary beats passing terminal run",
			evidence: &gateEvidenceSpec{
				verifySummary: map[string]any{"outcome": "failed", "iterations": 3, "max_iterations": 3},
				verifyRuns: []map[string]any{
					{"command": "scripts/test verify", "exit_code": 0, "outcome": "passed"},
				},
			},
			wantPassed:  false,
			wantOutcome: "failed",
			// No non-passing command to name, so the detail falls back
			// to the bare outcome — itself proof the summary, not the
			// run list, produced the verdict.
			wantViolationIs: `verification outcome "failed", want "passed"`,
			wantState:       run.StageStateFailed,
		},
		{
			// Summary precedence, conflict direction 2: the terminal
			// non-superseded run FAILED but the summary says passed
			// (the verify-fix loop's last iteration is recorded as the
			// summary). The summary wins, so this satisfies.
			name: "passing summary beats failing terminal run",
			evidence: &gateEvidenceSpec{
				verifySummary: map[string]any{"outcome": "passed", "iterations": 2, "max_iterations": 3},
				verifyRuns: []map[string]any{
					{"command": "scripts/test verify", "exit_code": 1, "outcome": "failed"},
				},
			},
			wantPassed:  true,
			wantOutcome: "passed",
			wantState:   run.StageStateAwaitingApproval,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, sf, repo, au := newPolicyTraceServer(t, changed)
			repo.runRow.WorkflowSpec = []byte(testWorkflowSpecRequireVerification)
			b := makeTestBundleWithGateEvidence(t, changed, tc.evidence)
			priv, _ := sf.issue(t, repo.runRow.ID)

			w := shipRequest(t, s, repo.runRow.ID, repo.stage.ID, "raw", priv, b, "")
			if w.Code != http.StatusAccepted {
				t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
			}

			pl := decodePolicyPayload(t, au)
			if pl.Passed != tc.wantPassed {
				t.Errorf("Passed = %v, want %v (violations %+v)", pl.Passed, tc.wantPassed, pl.Violations)
			}
			if tc.wantOutcome == "" {
				if pl.Applied.Verification != nil {
					t.Errorf("Applied.Verification = %+v, want nil", pl.Applied.Verification)
				}
			} else {
				if pl.Applied.Verification == nil {
					t.Fatalf("Applied.Verification = nil, want outcome %q", tc.wantOutcome)
				}
				if pl.Applied.Verification.Outcome != tc.wantOutcome {
					t.Errorf("Verification.Outcome = %q, want %q",
						pl.Applied.Verification.Outcome, tc.wantOutcome)
				}
			}
			if tc.wantViolationIs == "" {
				if len(pl.Violations) != 0 {
					t.Errorf("Violations = %+v, want none", pl.Violations)
				}
			} else {
				if len(pl.Violations) != 1 {
					t.Fatalf("Violations = %+v, want exactly 1", pl.Violations)
				}
				if pl.Violations[0].Constraint != "required_outcomes" ||
					!strings.Contains(pl.Violations[0].Detail, tc.wantViolationIs) {
					t.Errorf("Violation = %+v, want required_outcomes containing %q",
						pl.Violations[0], tc.wantViolationIs)
				}
			}
			// verification_reported is never deferred.
			if len(pl.DeferredOutcomes) != 0 {
				t.Errorf("DeferredOutcomes = %v, want empty", pl.DeferredOutcomes)
			}
			if repo.stage.State != tc.wantState {
				t.Errorf("stage state = %q, want %q", repo.stage.State, tc.wantState)
			}
		})
	}
}

// TestShipTrace_PolicyReEval_VerificationSignal_NotDerivedWithoutOptIn
// asserts the derivation is inert for a workflow that did NOT opt in:
// the signal is still recorded on the audit payload (it is cheap and
// useful evidence), but it produces no violation and cannot change the
// verdict of a spec declaring only tests_added_or_updated.
func TestShipTrace_PolicyReEval_VerificationSignal_NotDerivedWithoutOptIn(t *testing.T) {
	changed := []map[string]string{{"path": "backend/main.go", "status": "M"}}
	s, sf, repo, au := newPolicyTraceServer(t, changed)
	// testWorkflowSpec declares forbidden_paths + max_files_changed
	// only — no required_outcomes at all.
	b := makeTestBundleWithGateEvidence(t, changed, &gateEvidenceSpec{
		verifySummary: map[string]any{"outcome": "failed"},
	})
	priv, _ := sf.issue(t, repo.runRow.ID)

	w := shipRequest(t, s, repo.runRow.ID, repo.stage.ID, "raw", priv, b, "")
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
	}

	pl := decodePolicyPayload(t, au)
	if !pl.Passed || len(pl.Violations) != 0 {
		t.Errorf("Passed = %v violations = %+v, want a clean pass (outcome not declared)",
			pl.Passed, pl.Violations)
	}
	if repo.stage.State != run.StageStateAwaitingApproval {
		t.Errorf("stage state = %q, want awaiting_approval", repo.stage.State)
	}
	// The documented side behavior: reEvaluatePolicy sets
	// constraints.Verification unconditionally, so the failed signal is
	// still recorded as evidence on the payload even though the
	// workflow never declared the outcome.
	if pl.Applied.Verification == nil {
		t.Fatal("Applied.Verification = nil; the signal should still be recorded for a non-opted-in workflow")
	}
	if pl.Applied.Verification.Outcome != "failed" {
		t.Errorf("Applied.Verification.Outcome = %q, want failed",
			pl.Applied.Verification.Outcome)
	}
}

// testWorkflowSpecRequireDiffCoverage opts the implement stage into the
// workflow-v1.6 `diff_coverage` constraint (#1888 / ADR-059) at an 80%
// threshold. Pinned at version "1.6" — the kind is workflow-v1-only.
const testWorkflowSpecRequireDiffCoverage = `
version: "1.6"
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
          - diff_coverage:
              command: make coverage
              report_path: coverage.lcov
              min_new_line_coverage: 80
`

// TestShipTrace_PolicyReEval_DiffCoverage is the CROSS-BOUNDARY
// end-to-end assertion for #1888 / ADR-059. Each case packs a bundle
// carrying the RUNNER's literal `gate_evidence` json — field names spelled
// exactly as runner/cmd/fishhawk-runner/gateevidence.go emits them, NOT
// composed via bundle.GateEvidence — and drives it through the real trace
// upload: the bundle extractor, diffCoverageSignalFromBundle, the spec
// constraint load, and the policy evaluator, asserting the emitted
// `policy_evaluated` payload and the resulting stage state.
//
// This change spans payload, evidence, persistence and evaluation layers,
// so THIS test — not the per-layer units, which all pass through a tag
// drift — is what fails when the runner and backend mirror structs
// disagree.
func TestShipTrace_PolicyReEval_DiffCoverage(t *testing.T) {
	changed := []map[string]string{{"path": "backend/main.go", "status": "M"}}

	cases := []struct {
		name string
		// runnerJSON is the runner's literal gate_evidence payload.
		runnerJSON      string
		wantPassed      bool
		wantOutcome     string // applied_constraints.diff_coverage_signal.outcome ("" = nil signal)
		wantViolationIs string
		wantState       run.StageState
	}{
		{
			// The POSITIVE criterion: coverage above the threshold PASSES.
			name: "above threshold passes",
			runnerJSON: `{"diff_coverage":{"outcome":"measured","command":"make coverage",
				"exit_code":0,"report_path":"coverage.lcov","base_ref":"main",
				"new_lines":10,"covered_new_lines":10,"percent":100}}`,
			wantPassed:  true,
			wantOutcome: "measured",
			wantState:   run.StageStateAwaitingApproval,
		},
		{
			// Boundary: exactly AT the threshold passes (>= comparison).
			name: "exactly at threshold passes",
			runnerJSON: `{"diff_coverage":{"outcome":"measured","command":"make coverage",
				"exit_code":0,"report_path":"coverage.lcov","base_ref":"main",
				"new_lines":5,"covered_new_lines":4,"percent":80}}`,
			wantPassed:  true,
			wantOutcome: "measured",
			wantState:   run.StageStateAwaitingApproval,
		},
		{
			// One below the boundary fails.
			name: "one below threshold fails",
			runnerJSON: `{"diff_coverage":{"outcome":"measured","command":"make coverage",
				"exit_code":0,"report_path":"coverage.lcov","base_ref":"main",
				"new_lines":100,"covered_new_lines":79,"percent":79,
				"uncovered_files":["src/app.go"]}}`,
			wantPassed:      false,
			wantOutcome:     "measured",
			wantViolationIs: "below the required 80%",
			wantState:       run.StageStateFailed,
		},
		{
			// Condition 1: a configured stage whose diff added no coverable
			// lines receives the documented VACUOUS PASS, carried as an
			// explicit measured-with-zero signal rather than as silence.
			name: "configured stage with no new lines passes",
			runnerJSON: `{"diff_coverage":{"outcome":"measured","command":"make coverage",
				"exit_code":0,"report_path":"coverage.lcov","base_ref":"main",
				"new_lines":0,"covered_new_lines":0,"percent":0,
				"reason":"no added lines against \"main\"; nothing to measure"}}`,
			wantPassed:  true,
			wantOutcome: "measured",
			wantState:   run.StageStateAwaitingApproval,
		},
		{
			// Fail-closed: no diff-coverage record in the evidence at all.
			name:            "gate evidence without a diff-coverage record violates",
			runnerJSON:      `{"verify_summary":{"outcome":"passed"}}`,
			wantPassed:      false,
			wantOutcome:     "",
			wantViolationIs: "no diff-coverage evidence in trace",
			wantState:       run.StageStateFailed,
		},
		{
			name: "measurement failure violates with the runner's reason",
			runnerJSON: `{"diff_coverage":{"outcome":"failed","command":"make coverage",
				"exit_code":2,"report_path":"coverage.lcov",
				"reason":"coverage command exited 2: FAIL ./pkg"}}`,
			wantPassed:      false,
			wantOutcome:     "failed",
			wantViolationIs: "FAIL ./pkg",
			wantState:       run.StageStateFailed,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, sf, repo, au := newPolicyTraceServer(t, changed)
			repo.runRow.WorkflowSpec = []byte(testWorkflowSpecRequireDiffCoverage)
			b := makeTestBundleWithGateEvidence(t, changed, &gateEvidenceSpec{
				rawPayload: json.RawMessage(tc.runnerJSON),
			})
			priv, _ := sf.issue(t, repo.runRow.ID)

			w := shipRequest(t, s, repo.runRow.ID, repo.stage.ID, "raw", priv, b, "")
			if w.Code != http.StatusAccepted {
				t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
			}

			pl := decodePolicyPayload(t, au)
			if pl.Passed != tc.wantPassed {
				t.Errorf("Passed = %v, want %v (violations %+v)", pl.Passed, tc.wantPassed, pl.Violations)
			}
			// The DECLARED constraint must round-trip onto the payload, or
			// the post-CI re-evaluation would lose the gate entirely.
			if pl.Applied.DiffCoverage == nil {
				t.Fatalf("Applied.DiffCoverage = nil, want the declared constraint")
			}
			if pl.Applied.DiffCoverage.MinNewLineCoverage != 80 {
				t.Errorf("Applied.DiffCoverage.MinNewLineCoverage = %d, want 80",
					pl.Applied.DiffCoverage.MinNewLineCoverage)
			}
			if tc.wantOutcome == "" {
				if pl.Applied.DiffCoverageSignal != nil {
					t.Errorf("Applied.DiffCoverageSignal = %+v, want nil", pl.Applied.DiffCoverageSignal)
				}
			} else {
				if pl.Applied.DiffCoverageSignal == nil {
					t.Fatalf("Applied.DiffCoverageSignal = nil, want outcome %q — the runner's json field names did not survive the wire contract",
						tc.wantOutcome)
				}
				if pl.Applied.DiffCoverageSignal.Outcome != tc.wantOutcome {
					t.Errorf("DiffCoverageSignal.Outcome = %q, want %q",
						pl.Applied.DiffCoverageSignal.Outcome, tc.wantOutcome)
				}
			}
			if tc.wantViolationIs == "" {
				if len(pl.Violations) != 0 {
					t.Errorf("Violations = %+v, want none", pl.Violations)
				}
			} else {
				if len(pl.Violations) != 1 {
					t.Fatalf("Violations = %+v, want exactly 1", pl.Violations)
				}
				if pl.Violations[0].Constraint != "diff_coverage" ||
					!strings.Contains(pl.Violations[0].Detail, tc.wantViolationIs) {
					t.Errorf("Violation = %+v, want diff_coverage containing %q",
						pl.Violations[0], tc.wantViolationIs)
				}
			}
			// diff_coverage is never deferred.
			if len(pl.DeferredOutcomes) != 0 {
				t.Errorf("DeferredOutcomes = %v, want empty", pl.DeferredOutcomes)
			}
			if repo.stage.State != tc.wantState {
				t.Errorf("stage state = %q, want %q", repo.stage.State, tc.wantState)
			}
		})
	}
}

// TestShipTrace_PolicyReEval_DiffCoverage_SignalInertWithoutOptIn asserts
// the derivation is inert for a workflow that did NOT declare the
// constraint: a bundle carrying a catastrophic 0%-coverage measurement
// produces no violation at all, so an opt-out workflow takes no new code
// path (the revert-safety and no-regression property).
func TestShipTrace_PolicyReEval_DiffCoverage_SignalInertWithoutOptIn(t *testing.T) {
	changed := []map[string]string{{"path": "backend/main.go", "status": "M"}}
	s, sf, repo, au := newPolicyTraceServer(t, changed)
	// testWorkflowSpec declares forbidden_paths + max_files_changed only.
	b := makeTestBundleWithGateEvidence(t, changed, &gateEvidenceSpec{
		rawPayload: json.RawMessage(`{"diff_coverage":{"outcome":"measured",
			"new_lines":100,"covered_new_lines":0,"percent":0}}`),
	})
	priv, _ := sf.issue(t, repo.runRow.ID)

	w := shipRequest(t, s, repo.runRow.ID, repo.stage.ID, "raw", priv, b, "")
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
	}

	pl := decodePolicyPayload(t, au)
	if !pl.Passed || len(pl.Violations) != 0 {
		t.Errorf("Passed = %v violations = %+v, want a clean pass (constraint not declared)",
			pl.Passed, pl.Violations)
	}
	if pl.Applied.DiffCoverage != nil {
		t.Errorf("Applied.DiffCoverage = %+v, want nil when not declared", pl.Applied.DiffCoverage)
	}
	if repo.stage.State != run.StageStateAwaitingApproval {
		t.Errorf("stage state = %q, want awaiting_approval", repo.stage.State)
	}
}

// TestMergeConstraints_DiffCoverage pins the fold: the constraint is
// carried onto policy.Constraints with its declared values, the most
// restrictive threshold wins when declared twice, and a stage whose ONLY
// constraint is diff_coverage is not treated as constraint-free.
func TestMergeConstraints_DiffCoverage(t *testing.T) {
	got := mergeConstraints([]spec.Constraint{
		{DiffCoverage: &spec.DiffCoverageConstraint{
			Command: "a", ReportPath: "a.lcov", MinNewLineCoverage: 60,
		}},
		{DiffCoverage: &spec.DiffCoverageConstraint{
			Command: "b", ReportPath: "b.lcov", MinNewLineCoverage: 90, BaseRef: "release",
		}},
	})
	if got.DiffCoverage == nil {
		t.Fatal("DiffCoverage = nil, want the folded constraint")
	}
	if got.DiffCoverage.MinNewLineCoverage != 90 {
		t.Errorf("MinNewLineCoverage = %d, want 90 (most restrictive)", got.DiffCoverage.MinNewLineCoverage)
	}
	if got.DiffCoverage.Command != "b" || got.DiffCoverage.BaseRef != "release" {
		t.Errorf("folded constraint = %+v, want the winning entry's fields", got.DiffCoverage)
	}
	if isEmptyConstraints(got) {
		t.Error("a stage whose only constraint is diff_coverage was treated as constraint-free")
	}
}
