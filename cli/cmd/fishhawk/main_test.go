package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/cli/internal/httpclient"
)

func TestRun_Help(t *testing.T) {
	for _, arg := range []string{"--help", "-h", "help"} {
		t.Run(arg, func(t *testing.T) {
			var stdout, stderr strings.Builder
			got := run([]string{arg}, &stdout, &stderr)
			if got != exitOK {
				t.Errorf("run(%q) = %d, want exitOK", arg, got)
			}
			if !strings.Contains(stdout.String(), "Usage: fishhawk") {
				t.Errorf("usage missing for %q:\n%s", arg, stdout.String())
			}
		})
	}
}

func TestRun_Version(t *testing.T) {
	var stdout strings.Builder
	got := run([]string{"version"}, &stdout, io.Discard)
	if got != exitOK {
		t.Errorf("status = %d, want exitOK", got)
	}
	if strings.TrimSpace(stdout.String()) == "" {
		t.Error("version output empty")
	}
}

func TestRun_NoArgs(t *testing.T) {
	got := run(nil, io.Discard, io.Discard)
	if got != exitUsage {
		t.Errorf("status = %d, want exitUsage", got)
	}
}

func TestRun_UnknownCommand(t *testing.T) {
	var stderr strings.Builder
	got := run([]string{"nope"}, io.Discard, &stderr)
	if got != exitUsage {
		t.Errorf("status = %d, want exitUsage", got)
	}
	if !strings.Contains(stderr.String(), "unknown subcommand") {
		t.Errorf("missing 'unknown subcommand': %s", stderr.String())
	}
}

func TestRun_NoSubcommand(t *testing.T) {
	var stderr strings.Builder
	got := run([]string{"run"}, io.Discard, &stderr)
	if got != exitUsage {
		t.Errorf("status = %d, want exitUsage", got)
	}
}

// fakeBackend replicates the bits of the API the CLI calls. Each
// handler stores the last request so tests can assert wiring.
type fakeBackend struct {
	mu sync.Mutex

	startedRun  *httpclient.CreateRunInput
	cancelledID string
	listQuery   string

	// E18.1 / #332 — captures the last plan-approve request.
	stagesForRun map[uuid.UUID][]httpclient.Stage
	approvedID   string
	approvalBody httpclient.SubmitApprovalInput

	// E18.3 / #334 — captures the last retry request.
	retriedID string

	// E18.4 / #335 — captures the last audit-list request.
	auditQuery  string
	auditRunID  string
	auditResp   httpclient.ListRunAuditResult
	auditStatus int

	startResp Run
	getResp   Run
	listResp  ListResp

	// approvalResp returned by POST /v0/stages/{id}/approvals
	approvalResp httpclient.Stage
	// approvalRawResp, when set, is written verbatim on the approvals
	// 200 — serves the #986 duplicate-labeled shape with the literal
	// duplicate_submission/prior_decision/prior_submitted_at keys.
	approvalRawResp string
	// retryResp returned by POST /v0/stages/{id}/retry
	retryResp httpclient.Stage
	// retryErrCode lets a test request a 4xx response with a typed
	// API code; empty + retryStatus<400 → happy path.
	retryErrCode string

	startStatus    int
	getStatus      int
	listStatus     int
	stagesStatus   int
	approvalStatus int
	retryStatus    int
}

// Local copies — main.go doesn't import these from the httpclient
// package because they're test-only response shapes.
type Run = httpclient.Run
type ListResp = httpclient.ListRunsResult

func newFakeBackend(t *testing.T) (*fakeBackend, *httptest.Server) {
	t.Helper()
	fb := &fakeBackend{
		startStatus:    http.StatusCreated,
		getStatus:      http.StatusOK,
		listStatus:     http.StatusOK,
		stagesStatus:   http.StatusOK,
		approvalStatus: http.StatusOK,
		retryStatus:    http.StatusOK,
		auditStatus:    http.StatusOK,
		stagesForRun:   map[uuid.UUID][]httpclient.Stage{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v0/runs", func(w http.ResponseWriter, r *http.Request) {
		var in httpclient.CreateRunInput
		_ = json.NewDecoder(r.Body).Decode(&in)
		fb.mu.Lock()
		fb.startedRun = &in
		fb.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(fb.startStatus)
		_ = json.NewEncoder(w).Encode(fb.startResp)
	})
	mux.HandleFunc("GET /v0/runs/{run_id}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(fb.getStatus)
		_ = json.NewEncoder(w).Encode(fb.getResp)
	})
	mux.HandleFunc("GET /v0/runs", func(w http.ResponseWriter, r *http.Request) {
		fb.mu.Lock()
		fb.listQuery = r.URL.RawQuery
		fb.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(fb.listStatus)
		_ = json.NewEncoder(w).Encode(fb.listResp)
	})
	mux.HandleFunc("POST /v0/runs/{run_id}/cancel", func(w http.ResponseWriter, r *http.Request) {
		fb.mu.Lock()
		fb.cancelledID = r.PathValue("run_id")
		fb.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(httpclient.Run{ID: uuid.MustParse(r.PathValue("run_id")), State: "cancelled"})
	})
	mux.HandleFunc("GET /v0/runs/{run_id}/stages", func(w http.ResponseWriter, r *http.Request) {
		id, err := uuid.Parse(r.PathValue("run_id"))
		w.Header().Set("Content-Type", "application/json")
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": "validation_failed"}})
			return
		}
		w.WriteHeader(fb.stagesStatus)
		fb.mu.Lock()
		items := fb.stagesForRun[id]
		fb.mu.Unlock()
		_ = json.NewEncoder(w).Encode(httpclient.ListStagesResult{Items: items})
	})
	mux.HandleFunc("GET /v0/runs/{run_id}/audit", func(w http.ResponseWriter, r *http.Request) {
		fb.mu.Lock()
		fb.auditRunID = r.PathValue("run_id")
		fb.auditQuery = r.URL.RawQuery
		fb.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(fb.auditStatus)
		if fb.auditStatus >= 400 {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]any{
					"code":    "internal_error",
					"message": "boom",
				},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(fb.auditResp)
	})
	mux.HandleFunc("POST /v0/stages/{stage_id}/retry", func(w http.ResponseWriter, r *http.Request) {
		fb.mu.Lock()
		fb.retriedID = r.PathValue("stage_id")
		fb.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(fb.retryStatus)
		if fb.retryStatus >= 400 {
			code := fb.retryErrCode
			if code == "" {
				code = "internal_error"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]any{
					"code":    code,
					"message": "retry rejected",
				},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(fb.retryResp)
	})
	mux.HandleFunc("POST /v0/stages/{stage_id}/approvals", func(w http.ResponseWriter, r *http.Request) {
		var in httpclient.SubmitApprovalInput
		_ = json.NewDecoder(r.Body).Decode(&in)
		fb.mu.Lock()
		fb.approvedID = r.PathValue("stage_id")
		fb.approvalBody = in
		fb.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(fb.approvalStatus)
		if fb.approvalStatus >= 400 {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]any{
					"code":    "invalid_state_transition",
					"message": "stage is not awaiting_approval",
				},
			})
			return
		}
		if fb.approvalRawResp != "" {
			_, _ = w.Write([]byte(fb.approvalRawResp))
			return
		}
		_ = json.NewEncoder(w).Encode(fb.approvalResp)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return fb, srv
}

// withBackend sets FISHHAWK_BACKEND_URL so the subcommands pick up
// the fake server's URL via envOr.
func withBackend(t *testing.T, srv *httptest.Server) {
	t.Setenv("FISHHAWK_BACKEND_URL", srv.URL)
	t.Setenv("FISHHAWK_TOKEN", "")
}

func TestRunStart_HappyPath(t *testing.T) {
	fb, srv := newFakeBackend(t)
	withBackend(t, srv)
	id := uuid.New()
	fb.startResp = httpclient.Run{
		ID: id, Repo: "x/y", WorkflowID: "w", WorkflowSHA: "abc",
		TriggerSource: "cli", State: "pending",
		CreatedAt: time.Now().UTC(),
	}

	var stdout strings.Builder
	got := run([]string{
		"run", "start",
		"--repo", "x/y", "--workflow", "w", "--workflow-sha", "abc",
	}, &stdout, io.Discard)
	if got != exitOK {
		t.Fatalf("status = %d, want exitOK", got)
	}
	if fb.startedRun.Repo != "x/y" {
		t.Errorf("Repo = %q", fb.startedRun.Repo)
	}
	if fb.startedRun.TriggerSource != "cli" {
		t.Errorf("TriggerSource = %q", fb.startedRun.TriggerSource)
	}
	if !strings.Contains(stdout.String(), id.String()) {
		t.Errorf("stdout missing run ID: %s", stdout.String())
	}
}

func TestRunStart_TriggerRefForwarded(t *testing.T) {
	fb, srv := newFakeBackend(t)
	withBackend(t, srv)
	fb.startResp = httpclient.Run{ID: uuid.New(), State: "pending"}

	if rc := run([]string{
		"run", "start",
		"--repo", "x/y", "--workflow", "w", "--workflow-sha", "abc",
		"--trigger-ref", "issue:1247",
	}, io.Discard, io.Discard); rc != exitOK {
		t.Fatalf("status = %d", rc)
	}
	if fb.startedRun.TriggerRef == nil || *fb.startedRun.TriggerRef != "issue:1247" {
		t.Errorf("TriggerRef = %v, want issue:1247", fb.startedRun.TriggerRef)
	}
}

func TestRunStart_RunnerKindForwarded(t *testing.T) {
	// E22.7 / #410: --runner-kind local should land in the wire
	// body. Empty (omitted) leaves the field unset → backend
	// applies its github_actions default.
	fb, srv := newFakeBackend(t)
	withBackend(t, srv)
	fb.startResp = httpclient.Run{ID: uuid.New(), State: "pending", RunnerKind: "local"}

	if rc := run([]string{
		"run", "start",
		"--repo", "x/y", "--workflow", "w", "--workflow-sha", "abc",
		"--runner-kind", "local",
	}, io.Discard, io.Discard); rc != exitOK {
		t.Fatalf("status = %d", rc)
	}
	if fb.startedRun.RunnerKind != "local" {
		t.Errorf("RunnerKind = %q, want local", fb.startedRun.RunnerKind)
	}
}

func TestRunStart_RunnerKindOmitted_DefaultsAtBackend(t *testing.T) {
	// When --runner-kind isn't set the field is empty; the
	// httpclient's `omitempty` tag drops it from the wire JSON
	// entirely so the backend applies its default. This test
	// asserts the CLI layer doesn't fill in a default itself.
	fb, srv := newFakeBackend(t)
	withBackend(t, srv)
	fb.startResp = httpclient.Run{ID: uuid.New(), State: "pending"}

	if rc := run([]string{
		"run", "start",
		"--repo", "x/y", "--workflow", "w", "--workflow-sha", "abc",
	}, io.Discard, io.Discard); rc != exitOK {
		t.Fatalf("status = %d", rc)
	}
	if fb.startedRun.RunnerKind != "" {
		t.Errorf("RunnerKind = %q, want empty (backend defaults)", fb.startedRun.RunnerKind)
	}
}

func TestRunStart_RunnerKindShownInTextOutput(t *testing.T) {
	// printRun surfaces runner_kind so operators see the tag on
	// the response. Empty omits the line (legacy parity).
	fb, srv := newFakeBackend(t)
	withBackend(t, srv)
	fb.startResp = httpclient.Run{ID: uuid.New(), State: "pending", RunnerKind: "local"}

	var stdout strings.Builder
	if rc := run([]string{
		"run", "start",
		"--repo", "x/y", "--workflow", "w", "--workflow-sha", "abc",
		"--runner-kind", "local",
	}, &stdout, io.Discard); rc != exitOK {
		t.Fatalf("status = %d", rc)
	}
	if !strings.Contains(stdout.String(), "runner_kind:    local") {
		t.Errorf("text output missing runner_kind line:\n%s", stdout.String())
	}
}

func TestRunStart_MissingRequiredFlags(t *testing.T) {
	_, srv := newFakeBackend(t)
	withBackend(t, srv)
	var stderr strings.Builder
	got := run([]string{
		"run", "start", "--repo", "x/y",
	}, io.Discard, &stderr)
	if got != exitUsage {
		t.Errorf("status = %d, want exitUsage", got)
	}
	if !strings.Contains(stderr.String(), "required") {
		t.Errorf("missing 'required' in stderr: %s", stderr.String())
	}
}

func TestRunStart_BackendError(t *testing.T) {
	fb, srv := newFakeBackend(t)
	withBackend(t, srv)
	fb.startStatus = http.StatusBadRequest
	var stderr strings.Builder
	got := run([]string{
		"run", "start",
		"--repo", "x/y", "--workflow", "w", "--workflow-sha", "abc",
	}, io.Discard, &stderr)
	if got != exitFailure {
		t.Errorf("status = %d, want exitFailure", got)
	}
}

func TestRunStatus_HappyPath(t *testing.T) {
	fb, srv := newFakeBackend(t)
	withBackend(t, srv)
	id := uuid.New()
	fb.getResp = httpclient.Run{ID: id, State: "running"}

	var stdout strings.Builder
	got := run([]string{"run", "status", id.String()}, &stdout, io.Discard)
	if got != exitOK {
		t.Fatalf("status = %d", got)
	}
	if !strings.Contains(stdout.String(), "running") {
		t.Errorf("missing state: %s", stdout.String())
	}
}

func TestRunStatus_BadUUID(t *testing.T) {
	_, srv := newFakeBackend(t)
	withBackend(t, srv)
	got := run([]string{"run", "status", "not-a-uuid"}, io.Discard, io.Discard)
	if got != exitUsage {
		t.Errorf("status = %d, want exitUsage", got)
	}
}

func TestRunStatus_MissingArg(t *testing.T) {
	_, srv := newFakeBackend(t)
	withBackend(t, srv)
	got := run([]string{"run", "status"}, io.Discard, io.Discard)
	if got != exitUsage {
		t.Errorf("status = %d, want exitUsage", got)
	}
}

func TestRunStatus_JSONOutput(t *testing.T) {
	fb, srv := newFakeBackend(t)
	withBackend(t, srv)
	id := uuid.New()
	fb.getResp = httpclient.Run{
		ID:           id,
		Repo:         "x/y",
		State:        "running",
		RetryAttempt: 2,
	}

	var stdout strings.Builder
	got := run([]string{"run", "status", "--output", "json", id.String()}, &stdout, io.Discard)
	if got != exitOK {
		t.Fatalf("status = %d, want exitOK", got)
	}
	var decoded httpclient.Run
	if err := json.Unmarshal([]byte(stdout.String()), &decoded); err != nil {
		t.Fatalf("unmarshal stdout: %v\nstdout: %q", err, stdout.String())
	}
	if decoded.ID != id {
		t.Errorf("ID = %s, want %s", decoded.ID, id)
	}
	if decoded.State != "running" {
		t.Errorf("State = %q, want running", decoded.State)
	}
	if decoded.RetryAttempt != 2 {
		t.Errorf("RetryAttempt = %d, want 2", decoded.RetryAttempt)
	}
}

func TestRunStatus_BadOutputValue(t *testing.T) {
	var hits atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v0/runs/{run_id}", func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(httpclient.Run{})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	t.Setenv("FISHHAWK_BACKEND_URL", srv.URL)
	t.Setenv("FISHHAWK_TOKEN", "")

	var stderr strings.Builder
	got := run([]string{"run", "status", "--output", "xml", uuid.New().String()}, io.Discard, &stderr)
	if got != exitUsage {
		t.Errorf("status = %d, want exitUsage", got)
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("backend hit %d times; want 0 (bad --output must short-circuit before the network call)", n)
	}
	if !strings.Contains(stderr.String(), "invalid --output") {
		t.Errorf("stderr missing 'invalid --output': %s", stderr.String())
	}
}

func TestRunList_HappyPath(t *testing.T) {
	fb, srv := newFakeBackend(t)
	withBackend(t, srv)
	fb.listResp = httpclient.ListRunsResult{
		Items: []httpclient.Run{
			{ID: uuid.New(), Repo: "x/y", WorkflowID: "w", State: "pending", CreatedAt: time.Now().UTC()},
			{ID: uuid.New(), Repo: "a/b", WorkflowID: "w", State: "running", CreatedAt: time.Now().UTC()},
		},
		NextCursor: "abc",
	}
	var stdout strings.Builder
	got := run([]string{"run", "list", "--repo", "x/y"}, &stdout, io.Discard)
	if got != exitOK {
		t.Fatalf("status = %d", got)
	}
	out := stdout.String()
	if !strings.Contains(out, "x/y") || !strings.Contains(out, "a/b") {
		t.Errorf("missing items in output:\n%s", out)
	}
	if !strings.Contains(out, "More:") {
		t.Errorf("missing pagination hint:\n%s", out)
	}
	if !strings.Contains(fb.listQuery, "repo=x%2Fy") {
		t.Errorf("query missing repo filter: %s", fb.listQuery)
	}
}

func TestRunList_Empty(t *testing.T) {
	fb, srv := newFakeBackend(t)
	withBackend(t, srv)
	fb.listResp = httpclient.ListRunsResult{}
	var stdout strings.Builder
	if rc := run([]string{"run", "list"}, &stdout, io.Discard); rc != exitOK {
		t.Fatalf("status = %d", rc)
	}
	if !strings.Contains(stdout.String(), "(no runs)") {
		t.Errorf("expected (no runs) on empty list: %s", stdout.String())
	}
}

func TestRunCancel_HappyPath(t *testing.T) {
	fb, srv := newFakeBackend(t)
	withBackend(t, srv)
	id := uuid.New()

	var stdout strings.Builder
	got := run([]string{"run", "cancel", id.String()}, &stdout, io.Discard)
	if got != exitOK {
		t.Fatalf("status = %d", got)
	}
	if fb.cancelledID != id.String() {
		t.Errorf("cancelledID = %q, want %q", fb.cancelledID, id)
	}
	if !strings.Contains(stdout.String(), "cancelled") {
		t.Errorf("missing state in output: %s", stdout.String())
	}
}

func TestRunCancel_BadUUID(t *testing.T) {
	_, srv := newFakeBackend(t)
	withBackend(t, srv)
	got := run([]string{"run", "cancel", "not-a-uuid"}, io.Discard, io.Discard)
	if got != exitUsage {
		t.Errorf("status = %d, want exitUsage", got)
	}
}

func TestRunOpen_PrintURL(t *testing.T) {
	id := uuid.New()
	t.Setenv("FISHHAWK_BACKEND_URL", "https://app.fishhawk.test")
	var stdout strings.Builder
	got := run([]string{"run", "open", "--print-url", id.String()}, &stdout, io.Discard)
	if got != exitOK {
		t.Fatalf("status = %d", got)
	}
	want := fmt.Sprintf("https://app.fishhawk.test/runs/%s", id)
	if !strings.Contains(stdout.String(), want) {
		t.Errorf("output = %q, want contains %q", stdout.String(), want)
	}
}

func TestRunOpen_BrowserStub(t *testing.T) {
	// Substitute openBrowser so the test doesn't actually launch
	// a browser. Verify the URL we'd open is the canonical UI URL
	// and the success path returns exitOK.
	id := uuid.New()
	t.Setenv("FISHHAWK_BACKEND_URL", "https://app.fishhawk.test")
	called := ""
	orig := openBrowser
	openBrowser = func(url string) error {
		called = url
		return nil
	}
	t.Cleanup(func() { openBrowser = orig })

	var stdout strings.Builder
	got := run([]string{"run", "open", id.String()}, &stdout, io.Discard)
	if got != exitOK {
		t.Fatalf("status = %d", got)
	}
	if !strings.Contains(called, id.String()) {
		t.Errorf("openBrowser was called with %q, want URL containing %s", called, id)
	}
}

func TestRunOpen_BadUUID(t *testing.T) {
	got := run([]string{"run", "open", "not-a-uuid"}, io.Discard, io.Discard)
	if got != exitUsage {
		t.Errorf("status = %d, want exitUsage", got)
	}
}

func TestRunOpen_BrowserFailure(t *testing.T) {
	id := uuid.New()
	t.Setenv("FISHHAWK_BACKEND_URL", "https://app.fishhawk.test")
	orig := openBrowser
	openBrowser = func(url string) error { return fmt.Errorf("no browser available") }
	t.Cleanup(func() { openBrowser = orig })

	var stderr strings.Builder
	got := run([]string{"run", "open", id.String()}, io.Discard, &stderr)
	if got != exitFailure {
		t.Errorf("status = %d, want exitFailure", got)
	}
	if !strings.Contains(stderr.String(), "no browser") {
		t.Errorf("stderr missing browser error: %s", stderr.String())
	}
}

func TestRun_UnknownRunSubcommand(t *testing.T) {
	var stderr strings.Builder
	got := run([]string{"run", "frobnicate"}, io.Discard, &stderr)
	if got != exitUsage {
		t.Errorf("status = %d, want exitUsage", got)
	}
}

// --- audit list (E18.4 / #335) ---

func auditEntryFixture(seq int64, runID uuid.UUID, category, actor string, payload map[string]any) httpclient.AuditEntry {
	body, _ := json.Marshal(payload)
	var actorPtr *string
	if actor != "" {
		s := actor
		actorPtr = &s
	}
	return httpclient.AuditEntry{
		ID: uuid.New(), Sequence: seq, RunID: runID,
		Timestamp:    time.Date(2026, 5, 14, 12, 0, int(seq), 0, time.UTC),
		Category:     category,
		ActorSubject: actorPtr,
		Payload:      body,
		EntryHash:    fmt.Sprintf("hash-%d", seq),
	}
}

func TestAuditList_HappyPath_TextOutput(t *testing.T) {
	fb, srv := newFakeBackend(t)
	withBackend(t, srv)
	runID := uuid.New()
	fb.auditResp = httpclient.ListRunAuditResult{
		Items: []httpclient.AuditEntry{
			auditEntryFixture(1, runID, "run_dispatched", "", map[string]any{"kind": "issue"}),
			auditEntryFixture(2, runID, "plan_generated", "system", map[string]any{"summary": "add a feature"}),
			auditEntryFixture(3, runID, "approval_submitted", "alice", map[string]any{"decision": "approve"}),
		},
	}

	var stdout strings.Builder
	got := run([]string{"audit", "list", runID.String()}, &stdout, io.Discard)
	if got != exitOK {
		t.Fatalf("status = %d, want exitOK", got)
	}
	if fb.auditRunID != runID.String() {
		t.Errorf("audit ran against %s, want %s", fb.auditRunID, runID)
	}
	out := stdout.String()
	for _, want := range []string{"SEQ", "CATEGORY", "ACTOR", "WHEN", "SUMMARY",
		"run_dispatched", "plan_generated", "approval_submitted",
		"system", "alice", "approve", "add a feature"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing %q\n---\n%s", want, out)
		}
	}
}

func TestAuditList_FiltersForwarded(t *testing.T) {
	fb, srv := newFakeBackend(t)
	withBackend(t, srv)
	runID := uuid.New()
	stageID := uuid.New()
	fb.auditResp = httpclient.ListRunAuditResult{Items: nil}

	got := run([]string{
		"audit", "list",
		"--category", "approval_submitted",
		"--stage", stageID.String(),
		"--limit", "25",
		"--cursor", "abc",
		runID.String(),
	}, io.Discard, io.Discard)
	if got != exitOK {
		t.Fatalf("status = %d, want exitOK", got)
	}
	q := fb.auditQuery
	for _, want := range []string{
		"category=approval_submitted",
		"stage_id=" + stageID.String(),
		"limit=25",
		"cursor=abc",
	} {
		if !strings.Contains(q, want) {
			t.Errorf("query missing %q: %s", want, q)
		}
	}
}

func TestAuditList_EmptyPage(t *testing.T) {
	fb, srv := newFakeBackend(t)
	withBackend(t, srv)
	fb.auditResp = httpclient.ListRunAuditResult{Items: nil}

	var stdout strings.Builder
	got := run([]string{"audit", "list", uuid.New().String()}, &stdout, io.Discard)
	if got != exitOK {
		t.Fatalf("status = %d", got)
	}
	if !strings.Contains(stdout.String(), "(no audit entries)") {
		t.Errorf("stdout missing empty-page placeholder: %s", stdout.String())
	}
}

func TestAuditList_NextCursorRendered(t *testing.T) {
	fb, srv := newFakeBackend(t)
	withBackend(t, srv)
	runID := uuid.New()
	fb.auditResp = httpclient.ListRunAuditResult{
		Items:      []httpclient.AuditEntry{auditEntryFixture(1, runID, "run_dispatched", "", nil)},
		NextCursor: "tok-42",
	}

	var stdout strings.Builder
	got := run([]string{"audit", "list", runID.String()}, &stdout, io.Discard)
	if got != exitOK {
		t.Fatalf("status = %d", got)
	}
	if !strings.Contains(stdout.String(), "--cursor tok-42") {
		t.Errorf("stdout missing cursor hint: %s", stdout.String())
	}
}

func TestAuditList_JSONOutputIsNDJSON(t *testing.T) {
	fb, srv := newFakeBackend(t)
	withBackend(t, srv)
	runID := uuid.New()
	fb.auditResp = httpclient.ListRunAuditResult{
		Items: []httpclient.AuditEntry{
			auditEntryFixture(1, runID, "run_dispatched", "system", map[string]any{"kind": "issue"}),
			auditEntryFixture(2, runID, "plan_generated", "system", map[string]any{"summary": "x"}),
		},
	}

	var stdout strings.Builder
	got := run([]string{"audit", "list", "--output", "json", runID.String()}, &stdout, io.Discard)
	if got != exitOK {
		t.Fatalf("status = %d", got)
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 NDJSON lines; got %d\n%s", len(lines), stdout.String())
	}
	// Each line must round-trip through the AuditEntry shape.
	for i, line := range lines {
		var e httpclient.AuditEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("line %d not valid AuditEntry json: %v\n%s", i, err, line)
		}
		if e.RunID != runID {
			t.Errorf("line %d run_id = %s, want %s", i, e.RunID, runID)
		}
	}
}

func TestAuditList_BadUUID(t *testing.T) {
	_, srv := newFakeBackend(t)
	withBackend(t, srv)
	var stderr strings.Builder
	got := run([]string{"audit", "list", "not-a-uuid"}, io.Discard, &stderr)
	if got != exitUsage {
		t.Errorf("status = %d, want exitUsage", got)
	}
	if !strings.Contains(stderr.String(), "not a UUID") {
		t.Errorf("stderr missing 'not a UUID': %s", stderr.String())
	}
}

func TestAuditList_BadStageUUID(t *testing.T) {
	// --stage parses locally; surface a clean error without hitting
	// the backend.
	fb, srv := newFakeBackend(t)
	withBackend(t, srv)
	var stderr strings.Builder
	got := run([]string{
		"audit", "list",
		"--stage", "nope",
		uuid.New().String(),
	}, io.Discard, &stderr)
	if got != exitUsage {
		t.Errorf("status = %d, want exitUsage", got)
	}
	if !strings.Contains(stderr.String(), "--stage") {
		t.Errorf("stderr missing --stage diagnostic: %s", stderr.String())
	}
	if fb.auditRunID != "" {
		t.Errorf("backend hit despite local --stage validation failure")
	}
}

func TestAuditList_MissingArg(t *testing.T) {
	_, srv := newFakeBackend(t)
	withBackend(t, srv)
	var stderr strings.Builder
	got := run([]string{"audit", "list"}, io.Discard, &stderr)
	if got != exitUsage {
		t.Errorf("status = %d, want exitUsage", got)
	}
	if !strings.Contains(stderr.String(), "<run-id> required") {
		t.Errorf("stderr missing diagnostic: %s", stderr.String())
	}
}

func TestAuditList_ServerError(t *testing.T) {
	fb, srv := newFakeBackend(t)
	withBackend(t, srv)
	fb.auditStatus = http.StatusInternalServerError

	var stderr strings.Builder
	got := run([]string{"audit", "list", uuid.New().String()}, io.Discard, &stderr)
	if got != exitFailure {
		t.Errorf("status = %d, want exitFailure", got)
	}
	if !strings.Contains(stderr.String(), "internal_error") {
		t.Errorf("stderr missing api code: %s", stderr.String())
	}
}

func TestAuditList_BadOutputValue(t *testing.T) {
	_, srv := newFakeBackend(t)
	withBackend(t, srv)
	var stderr strings.Builder
	got := run([]string{"audit", "list", "--output", "xml", uuid.New().String()}, io.Discard, &stderr)
	if got != exitUsage {
		t.Errorf("status = %d, want exitUsage", got)
	}
	if !strings.Contains(stderr.String(), "invalid --output") {
		t.Errorf("stderr missing diagnostic: %s", stderr.String())
	}
}

func TestAudit_UnknownSubcommand(t *testing.T) {
	var stderr strings.Builder
	got := run([]string{"audit", "frobnicate"}, io.Discard, &stderr)
	if got != exitUsage {
		t.Errorf("status = %d, want exitUsage", got)
	}
	if !strings.Contains(stderr.String(), "unknown subcommand") {
		t.Errorf("stderr missing diagnostic: %s", stderr.String())
	}
}

func TestAudit_NoSubcommand(t *testing.T) {
	var stderr strings.Builder
	got := run([]string{"audit"}, io.Discard, &stderr)
	if got != exitUsage {
		t.Errorf("status = %d, want exitUsage", got)
	}
	if !strings.Contains(stderr.String(), "subcommand required") {
		t.Errorf("stderr missing diagnostic: %s", stderr.String())
	}
}

// --- run retry (E18.3 / #334) ---

func TestRunRetry_HappyPath(t *testing.T) {
	fb, srv := newFakeBackend(t)
	withBackend(t, srv)
	stageID := uuid.New()
	runID := uuid.New()
	// Category-A retry path: state flips to dispatched once the
	// orchestrator hands off workflow_dispatch.
	fb.retryResp = httpclient.Stage{
		ID: stageID, RunID: runID, Sequence: 2, Type: "implement",
		State:    "dispatched",
		Executor: httpclient.StageExecutor{Kind: "agent", Ref: "claude-code"},
	}

	var stdout strings.Builder
	got := run([]string{"run", "retry", stageID.String()}, &stdout, io.Discard)
	if got != exitOK {
		t.Fatalf("status = %d, want exitOK", got)
	}
	if fb.retriedID != stageID.String() {
		t.Errorf("retried stage_id = %s, want %s", fb.retriedID, stageID)
	}
	out := stdout.String()
	if !strings.Contains(out, stageID.String()) {
		t.Errorf("stdout missing stage id: %s", out)
	}
	if !strings.Contains(out, "dispatched") {
		t.Errorf("stdout missing post-retry state: %s", out)
	}
}

func TestRunRetry_NotApplicable_409(t *testing.T) {
	// retry_not_applicable (e.g. category B or gate-rejected D).
	// The CLI surfaces the API error code verbatim so operators
	// can switch on it.
	fb, srv := newFakeBackend(t)
	withBackend(t, srv)
	fb.retryStatus = http.StatusUnprocessableEntity
	fb.retryErrCode = "retry_not_applicable"

	var stderr strings.Builder
	got := run([]string{"run", "retry", uuid.New().String()}, io.Discard, &stderr)
	if got != exitFailure {
		t.Errorf("status = %d, want exitFailure", got)
	}
	if !strings.Contains(stderr.String(), "retry_not_applicable") {
		t.Errorf("stderr missing api code: %s", stderr.String())
	}
}

func TestRunRetry_StageNotFound_404(t *testing.T) {
	fb, srv := newFakeBackend(t)
	withBackend(t, srv)
	fb.retryStatus = http.StatusNotFound
	fb.retryErrCode = "stage_not_found"

	var stderr strings.Builder
	got := run([]string{"run", "retry", uuid.New().String()}, io.Discard, &stderr)
	if got != exitFailure {
		t.Errorf("status = %d, want exitFailure", got)
	}
	if !strings.Contains(stderr.String(), "stage_not_found") {
		t.Errorf("stderr missing api code: %s", stderr.String())
	}
}

func TestRunRetry_BadUUID(t *testing.T) {
	_, srv := newFakeBackend(t)
	withBackend(t, srv)
	var stderr strings.Builder
	got := run([]string{"run", "retry", "not-a-uuid"}, io.Discard, &stderr)
	if got != exitUsage {
		t.Errorf("status = %d, want exitUsage", got)
	}
	if !strings.Contains(stderr.String(), "not a UUID") {
		t.Errorf("stderr missing 'not a UUID': %s", stderr.String())
	}
}

func TestRunRetry_MissingArg(t *testing.T) {
	_, srv := newFakeBackend(t)
	withBackend(t, srv)
	var stderr strings.Builder
	got := run([]string{"run", "retry"}, io.Discard, &stderr)
	if got != exitUsage {
		t.Errorf("status = %d, want exitUsage", got)
	}
	if !strings.Contains(stderr.String(), "<stage-id> required") {
		t.Errorf("stderr missing diagnostic: %s", stderr.String())
	}
}

func TestRunRetry_JSONOutput(t *testing.T) {
	fb, srv := newFakeBackend(t)
	withBackend(t, srv)
	stageID := uuid.New()
	runID := uuid.New()
	fb.retryResp = httpclient.Stage{
		ID: stageID, RunID: runID, Sequence: 2, Type: "implement",
		State:    "dispatched",
		Executor: httpclient.StageExecutor{Kind: "agent", Ref: "claude-code"},
	}

	var stdout strings.Builder
	got := run([]string{"run", "retry", "--output", "json", stageID.String()}, &stdout, io.Discard)
	if got != exitOK {
		t.Fatalf("status = %d", got)
	}
	var decoded httpclient.Stage
	if err := json.NewDecoder(strings.NewReader(stdout.String())).Decode(&decoded); err != nil {
		t.Fatalf("decode json: %v\nstdout: %s", err, stdout.String())
	}
	if decoded.ID != stageID {
		t.Errorf("ID = %s, want %s", decoded.ID, stageID)
	}
	if decoded.State != "dispatched" {
		t.Errorf("State = %q", decoded.State)
	}
}

func TestRunRetry_BadOutputValue(t *testing.T) {
	_, srv := newFakeBackend(t)
	withBackend(t, srv)
	var stderr strings.Builder
	got := run([]string{"run", "retry", "--output", "xml", uuid.New().String()}, io.Discard, &stderr)
	if got != exitUsage {
		t.Errorf("status = %d, want exitUsage", got)
	}
	if !strings.Contains(stderr.String(), "invalid --output") {
		t.Errorf("stderr missing 'invalid --output': %s", stderr.String())
	}
}

// --- plan approve (E18.1 / #332) ---

// planApproveStages builds a stage list with a plan stage in the
// given state plus an implement stage at sequence 2 for shape.
func planApproveStages(runID uuid.UUID, planState string) []httpclient.Stage {
	return []httpclient.Stage{
		{ID: uuid.New(), RunID: runID, Sequence: 1, Type: "plan", State: planState,
			Executor: httpclient.StageExecutor{Kind: "agent", Ref: "claude-code"}},
		{ID: uuid.New(), RunID: runID, Sequence: 2, Type: "implement", State: "pending",
			Executor: httpclient.StageExecutor{Kind: "agent", Ref: "claude-code"}},
	}
}

func TestPlanApprove_HappyPath(t *testing.T) {
	fb, srv := newFakeBackend(t)
	withBackend(t, srv)
	runID := uuid.New()
	stages := planApproveStages(runID, "awaiting_approval")
	planStageID := stages[0].ID
	fb.stagesForRun[runID] = stages
	fb.approvalResp = httpclient.Stage{
		ID: planStageID, RunID: runID, Sequence: 1, Type: "plan",
		State:    "succeeded",
		Executor: httpclient.StageExecutor{Kind: "agent", Ref: "claude-code"},
	}

	var stdout strings.Builder
	got := run([]string{"plan", "approve", "--reason", "looks good", runID.String()}, &stdout, io.Discard)
	if got != exitOK {
		t.Fatalf("status = %d, want exitOK", got)
	}
	if fb.approvedID != planStageID.String() {
		t.Errorf("approved stage_id = %s, want %s", fb.approvedID, planStageID)
	}
	if fb.approvalBody.Decision != httpclient.ApprovalApprove {
		t.Errorf("decision = %q, want approve", fb.approvalBody.Decision)
	}
	if fb.approvalBody.Comment != "looks good" {
		t.Errorf("comment = %q, want 'looks good'", fb.approvalBody.Comment)
	}
	out := stdout.String()
	if !strings.Contains(out, planStageID.String()) {
		t.Errorf("stdout missing stage id: %s", out)
	}
	if !strings.Contains(out, "succeeded") {
		t.Errorf("stdout missing post-approval state: %s", out)
	}
}

func TestPlanApprove_NoAwaitingPlanStage(t *testing.T) {
	// Plan stage already settled — the operator missed the window
	// or someone else approved. Surface a clear, actionable message
	// pointing back at run status.
	fb, srv := newFakeBackend(t)
	withBackend(t, srv)
	runID := uuid.New()
	fb.stagesForRun[runID] = planApproveStages(runID, "succeeded")

	var stderr strings.Builder
	got := run([]string{"plan", "approve", runID.String()}, io.Discard, &stderr)
	if got != exitFailure {
		t.Errorf("status = %d, want exitFailure", got)
	}
	if !strings.Contains(stderr.String(), "no plan stage awaiting approval") {
		t.Errorf("stderr missing diagnostic: %s", stderr.String())
	}
	// Approval endpoint must not be reached.
	if fb.approvedID != "" {
		t.Errorf("approval endpoint reached with no awaiting plan stage; stage_id=%s", fb.approvedID)
	}
}

func TestPlanApprove_BadUUID(t *testing.T) {
	_, srv := newFakeBackend(t)
	withBackend(t, srv)
	var stderr strings.Builder
	got := run([]string{"plan", "approve", "not-a-uuid"}, io.Discard, &stderr)
	if got != exitUsage {
		t.Errorf("status = %d, want exitUsage", got)
	}
	if !strings.Contains(stderr.String(), "not a UUID") {
		t.Errorf("stderr missing 'not a UUID': %s", stderr.String())
	}
}

func TestPlanApprove_MissingArg(t *testing.T) {
	_, srv := newFakeBackend(t)
	withBackend(t, srv)
	var stderr strings.Builder
	got := run([]string{"plan", "approve"}, io.Discard, &stderr)
	if got != exitUsage {
		t.Errorf("status = %d, want exitUsage", got)
	}
	if !strings.Contains(stderr.String(), "<run-id> required") {
		t.Errorf("stderr missing diagnostic: %s", stderr.String())
	}
}

func TestPlanApprove_ServerRejection(t *testing.T) {
	// Server returns 409 invalid_state_transition (e.g. someone else
	// just approved). The CLI surfaces the API error and exits non-zero.
	fb, srv := newFakeBackend(t)
	withBackend(t, srv)
	runID := uuid.New()
	fb.stagesForRun[runID] = planApproveStages(runID, "awaiting_approval")
	fb.approvalStatus = http.StatusConflict

	var stderr strings.Builder
	got := run([]string{"plan", "approve", runID.String()}, io.Discard, &stderr)
	if got != exitFailure {
		t.Errorf("status = %d, want exitFailure", got)
	}
	if !strings.Contains(stderr.String(), "invalid_state_transition") {
		t.Errorf("stderr missing api error code: %s", stderr.String())
	}
}

func TestPlanApprove_JSONOutput(t *testing.T) {
	fb, srv := newFakeBackend(t)
	withBackend(t, srv)
	runID := uuid.New()
	stages := planApproveStages(runID, "awaiting_approval")
	planStageID := stages[0].ID
	fb.stagesForRun[runID] = stages
	fb.approvalResp = httpclient.Stage{
		ID: planStageID, RunID: runID, Sequence: 1, Type: "plan", State: "succeeded",
		Executor: httpclient.StageExecutor{Kind: "agent", Ref: "claude-code"},
	}

	var stdout strings.Builder
	got := run([]string{"plan", "approve", "--output", "json", runID.String()}, &stdout, io.Discard)
	if got != exitOK {
		t.Fatalf("status = %d", got)
	}
	var decoded httpclient.Stage
	if err := json.NewDecoder(strings.NewReader(stdout.String())).Decode(&decoded); err != nil {
		t.Fatalf("decode json: %v\nstdout: %s", err, stdout.String())
	}
	if decoded.ID != planStageID {
		t.Errorf("ID = %s, want %s", decoded.ID, planStageID)
	}
	if decoded.State != "succeeded" {
		t.Errorf("State = %q, want succeeded", decoded.State)
	}
}

func TestPlanApprove_Duplicate_TextNoticeOnStderr(t *testing.T) {
	// #986: a duplicate-labeled 200 prints an explicit stderr notice
	// before the normal stage echo — a no-op must never render as a
	// normal result. Exit stays 0 (the HTTP request succeeded).
	fb, srv := newFakeBackend(t)
	withBackend(t, srv)
	runID := uuid.New()
	stages := planApproveStages(runID, "awaiting_approval")
	planStageID := stages[0].ID
	fb.stagesForRun[runID] = stages
	fb.approvalRawResp = fmt.Sprintf(
		`{"id":%q,"run_id":%q,"sequence":1,"type":"plan","state":"awaiting_approval","executor":{"kind":"agent","ref":"claude-code"},"duplicate_submission":true,"prior_decision":"approve","prior_submitted_at":"2026-06-10T12:00:00Z"}`,
		planStageID, runID)

	var stdout, stderr strings.Builder
	got := run([]string{"plan", "approve", runID.String()}, &stdout, &stderr)
	if got != exitOK {
		t.Fatalf("status = %d, want exitOK (duplicate keeps exit 0)", got)
	}
	if !strings.Contains(stderr.String(), "duplicate submission — prior approve decision (2026-06-10T12:00:00Z) stands; stage state unchanged") {
		t.Errorf("stderr missing duplicate notice: %s", stderr.String())
	}
	// The normal stage echo still follows on stdout.
	if !strings.Contains(stdout.String(), planStageID.String()) {
		t.Errorf("stdout missing stage echo: %s", stdout.String())
	}
}

func TestPlanApprove_Duplicate_JSONCarriesLabeledFields(t *testing.T) {
	// #986: --output json includes the duplicate fields so scripts can
	// branch on approval EFFECT rather than exit code.
	fb, srv := newFakeBackend(t)
	withBackend(t, srv)
	runID := uuid.New()
	stages := planApproveStages(runID, "awaiting_approval")
	planStageID := stages[0].ID
	fb.stagesForRun[runID] = stages
	fb.approvalRawResp = fmt.Sprintf(
		`{"id":%q,"run_id":%q,"sequence":1,"type":"plan","state":"awaiting_approval","executor":{"kind":"agent","ref":"claude-code"},"duplicate_submission":true,"prior_decision":"reject","prior_submitted_at":"2026-06-10T12:00:00Z"}`,
		planStageID, runID)

	var stdout strings.Builder
	got := run([]string{"plan", "approve", "--output", "json", runID.String()}, &stdout, io.Discard)
	if got != exitOK {
		t.Fatalf("status = %d", got)
	}
	var decoded httpclient.ApprovalResult
	if err := json.NewDecoder(strings.NewReader(stdout.String())).Decode(&decoded); err != nil {
		t.Fatalf("decode json: %v\nstdout: %s", err, stdout.String())
	}
	if !decoded.DuplicateSubmission || decoded.PriorDecision != "reject" || decoded.PriorSubmittedAt == "" {
		t.Errorf("duplicate fields = (%v, %q, %q), want (true, reject, set)",
			decoded.DuplicateSubmission, decoded.PriorDecision, decoded.PriorSubmittedAt)
	}
}

func TestPlanApprove_FirstSubmission_JSONOmitsDuplicateKeys(t *testing.T) {
	// Additive-only contract: a first-submission 200 re-encodes
	// without the #986 keys, so existing json consumers see today's
	// shape unchanged.
	fb, srv := newFakeBackend(t)
	withBackend(t, srv)
	runID := uuid.New()
	stages := planApproveStages(runID, "awaiting_approval")
	fb.stagesForRun[runID] = stages
	fb.approvalResp = httpclient.Stage{
		ID: stages[0].ID, RunID: runID, Sequence: 1, Type: "plan", State: "succeeded",
		Executor: httpclient.StageExecutor{Kind: "agent", Ref: "claude-code"},
	}

	var stdout, stderr strings.Builder
	got := run([]string{"plan", "approve", "--output", "json", runID.String()}, &stdout, &stderr)
	if got != exitOK {
		t.Fatalf("status = %d", got)
	}
	for _, k := range []string{"duplicate_submission", "prior_decision", "prior_submitted_at"} {
		if strings.Contains(stdout.String(), k) {
			t.Errorf("first-submission json must omit %q: %s", k, stdout.String())
		}
	}
	if strings.Contains(stderr.String(), "duplicate submission") {
		t.Errorf("unexpected duplicate notice on a first submission: %s", stderr.String())
	}
}

func TestPlanApprove_BadOutputValue(t *testing.T) {
	_, srv := newFakeBackend(t)
	withBackend(t, srv)
	var stderr strings.Builder
	got := run([]string{"plan", "approve", "--output", "xml", uuid.New().String()}, io.Discard, &stderr)
	if got != exitUsage {
		t.Errorf("status = %d, want exitUsage", got)
	}
	if !strings.Contains(stderr.String(), "invalid --output") {
		t.Errorf("stderr missing diagnostic: %s", stderr.String())
	}
}

// --- plan reject (E18.2 / #333) ---

func TestPlanReject_HappyPath(t *testing.T) {
	fb, srv := newFakeBackend(t)
	withBackend(t, srv)
	runID := uuid.New()
	stages := planApproveStages(runID, "awaiting_approval")
	planStageID := stages[0].ID
	fb.stagesForRun[runID] = stages
	cat := "D"
	reason := "scope too wide"
	fb.approvalResp = httpclient.Stage{
		ID: planStageID, RunID: runID, Sequence: 1, Type: "plan", State: "failed",
		FailureCategory: &cat,
		FailureReason:   &reason,
		Executor:        httpclient.StageExecutor{Kind: "agent", Ref: "claude-code"},
	}

	var stdout, stderr strings.Builder
	got := run([]string{"plan", "reject", "--reason", "scope too wide", runID.String()}, &stdout, &stderr)
	if got != exitOK {
		t.Fatalf("status = %d, want exitOK; stderr=%s", got, stderr.String())
	}
	if fb.approvedID != planStageID.String() {
		t.Errorf("approved stage_id = %s, want %s", fb.approvedID, planStageID)
	}
	if fb.approvalBody.Decision != httpclient.ApprovalReject {
		t.Errorf("decision = %q, want reject", fb.approvalBody.Decision)
	}
	if fb.approvalBody.Comment != "scope too wide" {
		t.Errorf("comment = %q", fb.approvalBody.Comment)
	}
	out := stdout.String()
	// Output should reflect the failed-D state and the rejection
	// reason, otherwise the user runs an extra `run status` to
	// understand what happened.
	if !strings.Contains(out, "failed") {
		t.Errorf("stdout missing 'failed' state: %s", out)
	}
	if !strings.Contains(out, "D") {
		t.Errorf("stdout missing failure category 'D': %s", out)
	}
	// --reason was provided so no warning should fire.
	if strings.Contains(stderr.String(), "warning") {
		t.Errorf("unexpected warning when --reason set: %s", stderr.String())
	}
}

func TestPlanReject_NoReasonWarns(t *testing.T) {
	// Reject without --reason is wire-legal but produces an empty
	// audit comment. The CLI emits a soft warning to stderr and
	// proceeds — exit code stays exitOK.
	fb, srv := newFakeBackend(t)
	withBackend(t, srv)
	runID := uuid.New()
	stages := planApproveStages(runID, "awaiting_approval")
	planStageID := stages[0].ID
	fb.stagesForRun[runID] = stages
	fb.approvalResp = httpclient.Stage{
		ID: planStageID, RunID: runID, Sequence: 1, Type: "plan", State: "failed",
		Executor: httpclient.StageExecutor{Kind: "agent", Ref: "claude-code"},
	}

	var stdout, stderr strings.Builder
	got := run([]string{"plan", "reject", runID.String()}, &stdout, &stderr)
	if got != exitOK {
		t.Fatalf("status = %d, want exitOK", got)
	}
	if !strings.Contains(stderr.String(), "--reason not provided") {
		t.Errorf("stderr missing soft warning: %s", stderr.String())
	}
	if fb.approvalBody.Comment != "" {
		t.Errorf("comment = %q, want empty", fb.approvalBody.Comment)
	}
}

func TestPlanReject_NoAwaitingPlanStage(t *testing.T) {
	fb, srv := newFakeBackend(t)
	withBackend(t, srv)
	runID := uuid.New()
	fb.stagesForRun[runID] = planApproveStages(runID, "succeeded")

	var stderr strings.Builder
	got := run([]string{"plan", "reject", "--reason", "x", runID.String()}, io.Discard, &stderr)
	if got != exitFailure {
		t.Errorf("status = %d, want exitFailure", got)
	}
	if !strings.Contains(stderr.String(), "no plan stage awaiting approval") {
		t.Errorf("stderr missing diagnostic: %s", stderr.String())
	}
	if fb.approvedID != "" {
		t.Errorf("approval endpoint reached with no awaiting plan stage; stage_id=%s", fb.approvedID)
	}
}

func TestPlanReject_BadUUID(t *testing.T) {
	_, srv := newFakeBackend(t)
	withBackend(t, srv)
	var stderr strings.Builder
	got := run([]string{"plan", "reject", "--reason", "x", "not-a-uuid"}, io.Discard, &stderr)
	if got != exitUsage {
		t.Errorf("status = %d, want exitUsage", got)
	}
	if !strings.Contains(stderr.String(), "not a UUID") {
		t.Errorf("stderr missing 'not a UUID': %s", stderr.String())
	}
}

// TestIntermixedFlagOrder verifies that flags may appear after the positional
// run-id argument. Go's stdlib flag package stops at the first non-flag arg,
// so parseIntermixed is needed to resume parsing after each positional.
func TestIntermixedFlagOrder(t *testing.T) {
	type checkFn func(*testing.T, *fakeBackend, uuid.UUID, string)

	tests := []struct {
		name     string
		wantCode int
		setup    func(*fakeBackend, uuid.UUID)
		args     func(string) []string
		check    checkFn
	}{
		{
			// Reported failure: arg before flag in plan approve.
			name:     "plan approve arg-then-flag",
			wantCode: exitOK,
			setup: func(fb *fakeBackend, runID uuid.UUID) {
				stages := planApproveStages(runID, "awaiting_approval")
				fb.stagesForRun[runID] = stages
				fb.approvalResp = httpclient.Stage{
					ID: stages[0].ID, RunID: runID, Sequence: 1, Type: "plan",
					State:    "succeeded",
					Executor: httpclient.StageExecutor{Kind: "agent", Ref: "claude-code"},
				}
			},
			args: func(id string) []string {
				return []string{"plan", "approve", id, "--reason", "lgtm"}
			},
			check: func(t *testing.T, fb *fakeBackend, _ uuid.UUID, _ string) {
				t.Helper()
				if fb.approvedID == "" {
					t.Error("approval endpoint not reached")
				}
				if fb.approvalBody.Comment != "lgtm" {
					t.Errorf("comment = %q, want lgtm", fb.approvalBody.Comment)
				}
			},
		},
		{
			// Regression guard: flag before arg must still work.
			name:     "plan approve flag-then-arg",
			wantCode: exitOK,
			setup: func(fb *fakeBackend, runID uuid.UUID) {
				stages := planApproveStages(runID, "awaiting_approval")
				fb.stagesForRun[runID] = stages
				fb.approvalResp = httpclient.Stage{
					ID: stages[0].ID, RunID: runID, Sequence: 1, Type: "plan",
					State:    "succeeded",
					Executor: httpclient.StageExecutor{Kind: "agent", Ref: "claude-code"},
				}
			},
			args: func(id string) []string {
				return []string{"plan", "approve", "--reason", "lgtm", id}
			},
			check: func(t *testing.T, fb *fakeBackend, _ uuid.UUID, _ string) {
				t.Helper()
				if fb.approvedID == "" {
					t.Error("approval endpoint not reached")
				}
				if fb.approvalBody.Comment != "lgtm" {
					t.Errorf("comment = %q, want lgtm", fb.approvalBody.Comment)
				}
			},
		},
		{
			// plan reject with arg before flag.
			name:     "plan reject arg-then-flag",
			wantCode: exitOK,
			setup: func(fb *fakeBackend, runID uuid.UUID) {
				stages := planApproveStages(runID, "awaiting_approval")
				fb.stagesForRun[runID] = stages
				cat := "D"
				reason := "too wide"
				fb.approvalResp = httpclient.Stage{
					ID: stages[0].ID, RunID: runID, Sequence: 1, Type: "plan",
					State: "failed", FailureCategory: &cat, FailureReason: &reason,
					Executor: httpclient.StageExecutor{Kind: "agent", Ref: "claude-code"},
				}
			},
			args: func(id string) []string {
				return []string{"plan", "reject", id, "--reason", "too wide"}
			},
			check: func(t *testing.T, fb *fakeBackend, _ uuid.UUID, _ string) {
				t.Helper()
				if fb.approvedID == "" {
					t.Error("approval endpoint not reached")
				}
				if fb.approvalBody.Decision != httpclient.ApprovalReject {
					t.Errorf("decision = %q, want reject", fb.approvalBody.Decision)
				}
				if fb.approvalBody.Comment != "too wide" {
					t.Errorf("comment = %q, want 'too wide'", fb.approvalBody.Comment)
				}
			},
		},
		{
			// run status with arg before flag.
			name:     "run status arg-then-flag",
			wantCode: exitOK,
			setup: func(fb *fakeBackend, runID uuid.UUID) {
				fb.getResp = httpclient.Run{ID: runID, State: "running"}
			},
			args: func(id string) []string {
				return []string{"run", "status", id, "--output", "json"}
			},
			check: func(t *testing.T, _ *fakeBackend, runID uuid.UUID, out string) {
				t.Helper()
				if !strings.Contains(out, runID.String()) {
					t.Errorf("stdout missing run-id %s:\n%s", runID, out)
				}
			},
		},
		{
			// Missing run-id must still return exitUsage.
			name:     "plan approve missing run-id",
			wantCode: exitUsage,
			setup:    nil,
			args: func(_ string) []string {
				return []string{"plan", "approve", "--reason", "x"}
			},
			check: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fb, srv := newFakeBackend(t)
			withBackend(t, srv)
			runID := uuid.New()
			if tc.setup != nil {
				tc.setup(fb, runID)
			}
			var stdout strings.Builder
			got := run(tc.args(runID.String()), &stdout, io.Discard)
			if got != tc.wantCode {
				t.Errorf("exit code = %d, want %d", got, tc.wantCode)
			}
			if tc.check != nil {
				tc.check(t, fb, runID, stdout.String())
			}
		})
	}
}

func TestPlan_UnknownSubcommand(t *testing.T) {
	var stderr strings.Builder
	got := run([]string{"plan", "frobnicate"}, io.Discard, &stderr)
	if got != exitUsage {
		t.Errorf("status = %d, want exitUsage", got)
	}
	if !strings.Contains(stderr.String(), "unknown subcommand") {
		t.Errorf("stderr missing diagnostic: %s", stderr.String())
	}
}

func TestPlan_NoSubcommand(t *testing.T) {
	var stderr strings.Builder
	got := run([]string{"plan"}, io.Discard, &stderr)
	if got != exitUsage {
		t.Errorf("status = %d, want exitUsage", got)
	}
	if !strings.Contains(stderr.String(), "subcommand required") {
		t.Errorf("stderr missing diagnostic: %s", stderr.String())
	}
}

func TestEnvOr(t *testing.T) {
	t.Setenv("X_TEST_KEY", "")
	if got := envOr("X_TEST_KEY", "fallback"); got != "fallback" {
		t.Errorf("got %q, want fallback", got)
	}
	t.Setenv("X_TEST_KEY", "explicit")
	if got := envOr("X_TEST_KEY", "fallback"); got != "explicit" {
		t.Errorf("got %q, want explicit", got)
	}
}
