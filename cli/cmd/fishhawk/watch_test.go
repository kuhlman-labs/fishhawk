package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/cli/internal/httpclient"
)

// watchFake is a configurable backend for the `run watch` loop + command
// tests. It serves GET stages (stage-id resolution), GET stage-wait (the
// {state, terminal, failure_*} envelope), and GET scope-amendments.
type watchFake struct {
	mu sync.Mutex

	runID     uuid.UUID
	stageID   uuid.UUID
	stageType string

	// stage-wait envelope fields.
	state           string
	terminal        bool
	failureCategory *string

	// stage-wait / list HTTP status: 0 → 200, >=400 → error envelope.
	stageWaitStatus int
	stagesStatus    int

	amendments []httpclient.ScopeAmendment
}

func newWatchFake(t *testing.T) (*watchFake, *httptest.Server) {
	t.Helper()
	fb := &watchFake{
		runID:     uuid.New(),
		stageID:   uuid.New(),
		stageType: "implement",
		state:     "running",
	}
	mux := http.NewServeMux()
	writeJSON := func(w http.ResponseWriter, status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	writeErr := func(w http.ResponseWriter, status int, code string) {
		writeJSON(w, status, map[string]any{"error": map[string]any{"code": code, "message": code}})
	}

	mux.HandleFunc("GET /v0/runs/{run_id}/stages", func(w http.ResponseWriter, _ *http.Request) {
		fb.mu.Lock()
		status := fb.stagesStatus
		fb.mu.Unlock()
		if status >= 400 {
			writeErr(w, status, "internal")
			return
		}
		writeJSON(w, http.StatusOK, httpclient.ListStagesResult{Items: []httpclient.Stage{
			{ID: uuid.New(), RunID: fb.runID, Sequence: 1, Type: "plan", State: "succeeded"},
			{ID: fb.stageID, RunID: fb.runID, Sequence: 2, Type: fb.stageType, State: "running"},
		}})
	})
	mux.HandleFunc("GET /v0/runs/{run_id}/stages/{stage_id}", func(w http.ResponseWriter, _ *http.Request) {
		fb.mu.Lock()
		status := fb.stageWaitStatus
		env := httpclient.RunStageWait{
			ID: fb.stageID, RunID: fb.runID, State: fb.state,
			Terminal: fb.terminal, FailureCategory: fb.failureCategory,
		}
		fb.mu.Unlock()
		if status >= 400 {
			writeErr(w, status, "internal")
			return
		}
		writeJSON(w, http.StatusOK, env)
	})
	mux.HandleFunc("GET /v0/runs/{run_id}/scope-amendments", func(w http.ResponseWriter, _ *http.Request) {
		fb.mu.Lock()
		items := append([]httpclient.ScopeAmendment(nil), fb.amendments...)
		fb.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"items": items})
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return fb, srv
}

func (fb *watchFake) addPendingAmendment() {
	fb.mu.Lock()
	fb.amendments = append(fb.amendments, httpclient.ScopeAmendment{
		ID: uuid.New(), RunID: fb.runID, StageID: fb.stageID, Status: "pending",
		Paths: []httpclient.ScopeAmendmentPath{{Path: "pkg/foo.go", Operation: "modify"}},
	})
	fb.mu.Unlock()
}

// parseWatchSummary decodes the single JSON summary line the watcher must
// emit to stdout, asserting exactly one line is present.
func parseWatchSummary(t *testing.T, stdout string) watchSummary {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) != 1 || strings.TrimSpace(lines[0]) == "" {
		t.Fatalf("want exactly one summary line, got %d: %q", len(lines), stdout)
	}
	var s watchSummary
	if err := json.Unmarshal([]byte(lines[0]), &s); err != nil {
		t.Fatalf("summary is not valid JSON: %v (%q)", err, lines[0])
	}
	return s
}

// (1) terminal-ok: a settled-succeeded stage returns exit 0 / terminal_ok.
func TestWatchLoop_TerminalOK(t *testing.T) {
	fb, srv := newWatchFake(t)
	fb.state = "succeeded"
	fb.terminal = true

	c := httpclient.New(srv.URL, "op-tok")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var stdout strings.Builder
	rc := watchLoop(ctx, c, fb.runID, fb.stageID, "implement", watchUntilAny, 1, &stdout, io.Discard)
	if rc != exitWatchTerminalOK {
		t.Fatalf("rc = %d, want exitWatchTerminalOK", rc)
	}
	s := parseWatchSummary(t, stdout.String())
	if s.Outcome != "terminal_ok" || s.ExitCode != exitWatchTerminalOK || s.State != "succeeded" {
		t.Errorf("summary = %+v, want terminal_ok/0/succeeded", s)
	}
}

// (2a) failed via state==failed.
func TestWatchLoop_Failed_ByState(t *testing.T) {
	fb, srv := newWatchFake(t)
	fb.state = "failed"
	fb.terminal = true

	c := httpclient.New(srv.URL, "op-tok")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var stdout strings.Builder
	rc := watchLoop(ctx, c, fb.runID, fb.stageID, "implement", watchUntilTerminal, 1, &stdout, io.Discard)
	if rc != exitWatchFailed {
		t.Fatalf("rc = %d, want exitWatchFailed", rc)
	}
	s := parseWatchSummary(t, stdout.String())
	if s.Outcome != "failed" || s.ExitCode != exitWatchFailed || s.State != "failed" {
		t.Errorf("summary = %+v, want failed/1/failed", s)
	}
}

// (2b) failed via a non-nil failure_category even when the state is a
// parked/awaiting_* state (terminal=true because IsSettled).
func TestWatchLoop_Failed_ByFailureCategory(t *testing.T) {
	fb, srv := newWatchFake(t)
	cat := "category_b"
	fb.state = "awaiting_approval"
	fb.terminal = true
	fb.failureCategory = &cat

	c := httpclient.New(srv.URL, "op-tok")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var stdout strings.Builder
	rc := watchLoop(ctx, c, fb.runID, fb.stageID, "implement", watchUntilAny, 1, &stdout, io.Discard)
	if rc != exitWatchFailed {
		t.Fatalf("rc = %d, want exitWatchFailed", rc)
	}
	s := parseWatchSummary(t, stdout.String())
	if s.Outcome != "failed" || s.ExitCode != exitWatchFailed {
		t.Errorf("summary = %+v, want failed/1", s)
	}
}

// (3a) amendment-pending under --until any.
func TestWatchLoop_AmendmentPending_UntilAny(t *testing.T) {
	fb, srv := newWatchFake(t)
	fb.addPendingAmendment() // stage stays non-terminal

	c := httpclient.New(srv.URL, "op-tok")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var stdout strings.Builder
	rc := watchLoop(ctx, c, fb.runID, fb.stageID, "implement", watchUntilAny, 1, &stdout, io.Discard)
	if rc != exitWatchAmendmentPending {
		t.Fatalf("rc = %d, want exitWatchAmendmentPending", rc)
	}
	s := parseWatchSummary(t, stdout.String())
	if s.Outcome != "amendment_pending" || s.ExitCode != exitWatchAmendmentPending {
		t.Errorf("summary = %+v, want amendment_pending/3", s)
	}
}

// (3b) amendment-pending under --until amendment.
func TestWatchLoop_AmendmentPending_UntilAmendment(t *testing.T) {
	fb, srv := newWatchFake(t)
	fb.addPendingAmendment()

	c := httpclient.New(srv.URL, "op-tok")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var stdout strings.Builder
	rc := watchLoop(ctx, c, fb.runID, fb.stageID, "implement", watchUntilAmendment, 1, &stdout, io.Discard)
	if rc != exitWatchAmendmentPending {
		t.Fatalf("rc = %d, want exitWatchAmendmentPending", rc)
	}
	s := parseWatchSummary(t, stdout.String())
	if s.Outcome != "amendment_pending" {
		t.Errorf("summary = %+v, want amendment_pending", s)
	}
}

// (3c) a pending amendment is NOT triggered under --until terminal: the
// watcher ignores amendments and times out on the never-settling stage.
func TestWatchLoop_AmendmentIgnored_UntilTerminal(t *testing.T) {
	fb, srv := newWatchFake(t)
	fb.addPendingAmendment() // stage never settles

	c := httpclient.New(srv.URL, "op-tok")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	var stdout strings.Builder
	rc := watchLoop(ctx, c, fb.runID, fb.stageID, "implement", watchUntilTerminal, 1, &stdout, io.Discard)
	if rc != exitWatchTimeout {
		t.Fatalf("rc = %d, want exitWatchTimeout (amendment ignored under terminal)", rc)
	}
	s := parseWatchSummary(t, stdout.String())
	if s.Outcome != "timeout" {
		t.Errorf("summary = %+v, want timeout", s)
	}
}

// Binding condition 1 (#1550): --until amendment must NOT hang when the
// stage settles terminal before any amendment appears — it returns the
// terminal outcome.
func TestWatchLoop_UntilAmendment_TerminalBeforeAmendment(t *testing.T) {
	fb, srv := newWatchFake(t)
	fb.state = "succeeded"
	fb.terminal = true // no amendments ever filed

	c := httpclient.New(srv.URL, "op-tok")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var stdout strings.Builder
	rc := watchLoop(ctx, c, fb.runID, fb.stageID, "implement", watchUntilAmendment, 1, &stdout, io.Discard)
	if rc != exitWatchTerminalOK {
		t.Fatalf("rc = %d, want exitWatchTerminalOK (terminal ends an amendment wait)", rc)
	}
	s := parseWatchSummary(t, stdout.String())
	if s.Outcome != "terminal_ok" || s.State != "succeeded" {
		t.Errorf("summary = %+v, want terminal_ok/succeeded", s)
	}
}

// (4) timeout: max-duration elapses before the stage settles.
func TestWatchLoop_Timeout(t *testing.T) {
	fb, srv := newWatchFake(t)
	// stage stays running, no amendments → loop until deadline.

	c := httpclient.New(srv.URL, "op-tok")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	var stdout strings.Builder
	rc := watchLoop(ctx, c, fb.runID, fb.stageID, "implement", watchUntilAny, 1, &stdout, io.Discard)
	if rc != exitWatchTimeout {
		t.Fatalf("rc = %d, want exitWatchTimeout", rc)
	}
	s := parseWatchSummary(t, stdout.String())
	if s.Outcome != "timeout" || s.ExitCode != exitWatchTimeout {
		t.Errorf("summary = %+v, want timeout/4", s)
	}
}

// (5) a transport/API error on the stage-wait call returns exit 1 with
// outcome=error and still emits exactly one summary line.
func TestWatchLoop_TransportError(t *testing.T) {
	fb, srv := newWatchFake(t)
	fb.stageWaitStatus = http.StatusInternalServerError

	c := httpclient.New(srv.URL, "op-tok")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var stdout strings.Builder
	// --until terminal so the loop goes straight to the failing stage-wait.
	rc := watchLoop(ctx, c, fb.runID, fb.stageID, "implement", watchUntilTerminal, 1, &stdout, io.Discard)
	if rc != exitWatchFailed {
		t.Fatalf("rc = %d, want exitWatchFailed", rc)
	}
	s := parseWatchSummary(t, stdout.String())
	if s.Outcome != "error" || s.ExitCode != exitWatchFailed {
		t.Errorf("summary = %+v, want error/1", s)
	}
}

// (5b) an amendment-list transport error under --until any also returns
// error/exit 1 with a single summary line.
func TestWatchLoop_AmendmentListError(t *testing.T) {
	runID := uuid.New()
	stageID := uuid.New()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v0/runs/{run_id}/scope-amendments", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":{"code":"internal","message":"boom"}}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := httpclient.New(srv.URL, "op-tok")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var stdout strings.Builder
	rc := watchLoop(ctx, c, runID, stageID, "implement", watchUntilAny, 1, &stdout, io.Discard)
	if rc != exitWatchFailed {
		t.Fatalf("rc = %d, want exitWatchFailed", rc)
	}
	s := parseWatchSummary(t, stdout.String())
	if s.Outcome != "error" {
		t.Errorf("summary = %+v, want error", s)
	}
}

// (6) invalid --until returns exitUsage (a parse error, no summary line).
func TestRunWatch_InvalidUntil(t *testing.T) {
	var stderr strings.Builder
	rc := run([]string{"run", "watch", "--until", "bogus", uuid.New().String()}, io.Discard, &stderr)
	if rc != exitUsage {
		t.Fatalf("rc = %d, want exitUsage", rc)
	}
	if !strings.Contains(stderr.String(), "invalid --until") {
		t.Errorf("stderr missing diagnostic: %s", stderr.String())
	}
}

func TestRunWatch_BadUUID(t *testing.T) {
	var stderr strings.Builder
	rc := run([]string{"run", "watch", "not-a-uuid"}, io.Discard, &stderr)
	if rc != exitUsage {
		t.Fatalf("rc = %d, want exitUsage", rc)
	}
	if !strings.Contains(stderr.String(), "not a UUID") {
		t.Errorf("stderr missing diagnostic: %s", stderr.String())
	}
}

func TestRunWatch_MissingArg(t *testing.T) {
	var stderr strings.Builder
	rc := run([]string{"run", "watch"}, io.Discard, &stderr)
	if rc != exitUsage {
		t.Fatalf("rc = %d, want exitUsage", rc)
	}
	if !strings.Contains(stderr.String(), "<run-id> required") {
		t.Errorf("stderr missing diagnostic: %s", stderr.String())
	}
}

// (7) stage-type-not-found: a non-terminal failure path that emits a
// single JSON summary with outcome=error AND returns exitFailure, and
// prints the available stage types to stderr (binding condition 2).
func TestRunWatch_StageTypeNotFound(t *testing.T) {
	fb, srv := newWatchFake(t)
	fb.stageType = "implement" // list serves plan + implement

	t.Setenv("FISHHAWK_BACKEND_URL", srv.URL)
	t.Setenv("FISHHAWK_TOKEN", "op-tok")

	var stdout, stderr strings.Builder
	rc := run([]string{"run", "watch", "--stage", "deploy", fb.runID.String()}, &stdout, &stderr)
	if rc != exitWatchFailed {
		t.Fatalf("rc = %d, want exitWatchFailed", rc)
	}
	if !strings.Contains(stderr.String(), "no stage of type \"deploy\"") ||
		!strings.Contains(stderr.String(), "available:") {
		t.Errorf("stderr missing available-types diagnostic: %s", stderr.String())
	}
	s := parseWatchSummary(t, stdout.String())
	if s.Outcome != "error" || s.ExitCode != exitWatchFailed || s.StageType != "deploy" {
		t.Errorf("summary = %+v, want error/1/deploy", s)
	}
}

// End-to-end through the command dispatcher: resolve the stage id by type
// then settle terminal-ok, exercising the transport + resolution + loop
// together.
func TestRunWatch_EndToEnd_TerminalOK(t *testing.T) {
	fb, srv := newWatchFake(t)
	fb.state = "succeeded"
	fb.terminal = true

	t.Setenv("FISHHAWK_BACKEND_URL", srv.URL)
	t.Setenv("FISHHAWK_TOKEN", "op-tok")

	var stdout strings.Builder
	rc := run([]string{"run", "watch", "--stage", "implement", "--poll", "1", fb.runID.String()}, &stdout, io.Discard)
	if rc != exitWatchTerminalOK {
		t.Fatalf("rc = %d, want exitWatchTerminalOK", rc)
	}
	s := parseWatchSummary(t, stdout.String())
	if s.Outcome != "terminal_ok" || s.StageID != fb.stageID.String() {
		t.Errorf("summary = %+v, want terminal_ok with resolved stage id %s", s, fb.stageID)
	}
}
