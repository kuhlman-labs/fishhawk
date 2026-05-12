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

	startResp Run
	getResp   Run
	listResp  ListResp

	startStatus int
	getStatus   int
	listStatus  int
}

// Local copies — main.go doesn't import these from the httpclient
// package because they're test-only response shapes.
type Run = httpclient.Run
type ListResp = httpclient.ListRunsResult

func newFakeBackend(t *testing.T) (*fakeBackend, *httptest.Server) {
	t.Helper()
	fb := &fakeBackend{
		startStatus: http.StatusCreated,
		getStatus:   http.StatusOK,
		listStatus:  http.StatusOK,
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
