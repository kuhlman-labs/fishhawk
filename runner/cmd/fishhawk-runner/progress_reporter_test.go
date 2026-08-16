package main

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/runner/internal/upload"
)

const testHeartbeat = `{"event":"stage_progress","elapsed_seconds":42,"turns":9,"tokens_so_far":13402,"last_event_kind":"assistant"}` + "\n"

// fakeReporter is a stageProgressReporter test double. It records calls, can
// block on a gate, and can return a canned error — enough to drive every tee
// guard. Concurrency-safe: the tee reports from an async goroutine.
type fakeReporter struct {
	mu       sync.Mutex
	calls    []upload.ReportStageProgressArgs
	err      error
	block    chan struct{} // when non-nil, ReportStageProgress waits on it
	inFlight atomic.Int32
	maxSeen  atomic.Int32
}

func (f *fakeReporter) ReportStageProgress(_ context.Context, args upload.ReportStageProgressArgs) error {
	n := f.inFlight.Add(1)
	for {
		m := f.maxSeen.Load()
		if n <= m || f.maxSeen.CompareAndSwap(m, n) {
			break
		}
	}
	defer f.inFlight.Add(-1)
	if f.block != nil {
		<-f.block
	}
	f.mu.Lock()
	f.calls = append(f.calls, args)
	f.mu.Unlock()
	return f.err
}

func (f *fakeReporter) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// syncBuf is a concurrency-safe writer used as BOTH the tee's forward sink and
// diag sink in tests, mirroring how main.go passes one mutex-guarded syncWriter
// to both — so a report goroutine's diag write and a heartbeat forward never
// race on the underlying buffer (approval condition 1).
type syncBuf struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// TestProgressTee_ForwardsHeartbeatAndPosts is the happy path: the heartbeat is
// forwarded byte-identically AND one POST lands with the parsed counters.
func TestProgressTee_ForwardsHeartbeatAndPosts(t *testing.T) {
	var sink syncBuf
	rep := &fakeReporter{}
	tee := newProgressTee(&sink, rep, "run-1", "stage-1", "fhm_tok", &sink)

	n, err := tee.Write([]byte(testHeartbeat))
	if err != nil || n != len(testHeartbeat) {
		t.Fatalf("Write = (%d, %v), want (%d, nil)", n, err, len(testHeartbeat))
	}
	tee.waitForReports()
	if sink.String() != testHeartbeat {
		t.Errorf("forwarded bytes = %q, want the verbatim heartbeat", sink.String())
	}
	if rep.callCount() != 1 {
		t.Fatalf("POST count = %d, want 1", rep.callCount())
	}
	got := rep.calls[0]
	if got.LastEvent != "assistant" || got.TurnsThisAttempt != 9 || got.TokensThisAttempt != 13402 {
		t.Errorf("posted = %+v", got)
	}
}

// TestProgressTee_NilClientIsPassThrough: a nil client forwards and never posts.
func TestProgressTee_NilClientIsPassThrough(t *testing.T) {
	var sink syncBuf
	tee := newProgressTee(&sink, nil, "run-1", "stage-1", "fhm_tok", &sink)
	n, err := tee.Write([]byte(testHeartbeat))
	if err != nil || n != len(testHeartbeat) {
		t.Fatalf("Write = (%d, %v)", n, err)
	}
	tee.waitForReports()
	if sink.String() != testHeartbeat {
		t.Errorf("forwarded bytes = %q, want verbatim", sink.String())
	}
}

// TestProgressTee_NoTokenIsPassThrough: an empty bearer (the acceptance stage's
// zero-credential posture) forwards and never posts.
func TestProgressTee_NoTokenIsPassThrough(t *testing.T) {
	var sink syncBuf
	rep := &fakeReporter{}
	tee := newProgressTee(&sink, rep, "run-1", "stage-1", "", &sink)
	if _, err := tee.Write([]byte(testHeartbeat)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	tee.waitForReports()
	if rep.callCount() != 0 {
		t.Errorf("POST count = %d, want 0 (no token → pass-through)", rep.callCount())
	}
	if sink.String() != testHeartbeat {
		t.Errorf("forwarded bytes = %q, want verbatim", sink.String())
	}
}

// TestProgressTee_NonHeartbeatLinePassesThroughUnposted pins the swallow: a line
// that is not a stage_progress object is forwarded and NOT posted. The doc
// comment states the whole-line assumption underneath this behaviour.
func TestProgressTee_NonHeartbeatLinePassesThroughUnposted(t *testing.T) {
	var sink syncBuf
	rep := &fakeReporter{}
	tee := newProgressTee(&sink, rep, "run-1", "stage-1", "fhm_tok", &sink)

	for _, line := range []string{
		`{"event":"runner_started","run_id":"x"}` + "\n",
		"not json at all\n",
		`{"event":"stage_progress"` + "\n", // split/partial write: invalid JSON
	} {
		if _, err := tee.Write([]byte(line)); err != nil {
			t.Fatalf("Write(%q): %v", line, err)
		}
	}
	tee.waitForReports()
	if rep.callCount() != 0 {
		t.Errorf("POST count = %d, want 0 (non-heartbeat lines are not posted)", rep.callCount())
	}
	if !strings.Contains(sink.String(), "runner_started") || !strings.Contains(sink.String(), "not json at all") {
		t.Errorf("non-heartbeat lines not forwarded verbatim: %q", sink.String())
	}
}

// TestProgressTee_Non2xxIsLoggedAndDropped (C4 counterfactual vehicle): a report
// error is written as a single stage_progress_report_failed diag line and
// dropped — Write still returns success. The reporter returns an error DIRECTLY
// (a reachable in-test double), so deleting the tee's error-swallow branch turns
// this RED rather than passing on an unrelated connection error.
func TestProgressTee_Non2xxIsLoggedAndDropped(t *testing.T) {
	var sink syncBuf
	rep := &fakeReporter{err: &statusErr{code: 500}}
	tee := newProgressTee(&sink, rep, "run-1", "stage-1", "fhm_tok", &sink)

	n, err := tee.Write([]byte(testHeartbeat))
	if err != nil || n != len(testHeartbeat) {
		t.Fatalf("Write = (%d, %v), want (%d, nil) — the reporter must never fail the write", n, err, len(testHeartbeat))
	}
	tee.waitForReports()
	if !strings.Contains(sink.String(), "stage_progress_report_failed") {
		t.Errorf("diag missing the swallowed-failure line:\n%s", sink.String())
	}
	// The heartbeat line itself was still forwarded verbatim.
	if !strings.Contains(sink.String(), testHeartbeat) {
		t.Errorf("heartbeat not forwarded despite a failed report:\n%s", sink.String())
	}
}

// TestProgressTee_TransportErrorIsLoggedAndDropped: a transport-shaped error is
// also swallowed to a diag line, never returned.
func TestProgressTee_TransportErrorIsLoggedAndDropped(t *testing.T) {
	var sink syncBuf
	rep := &fakeReporter{err: context.DeadlineExceeded}
	tee := newProgressTee(&sink, rep, "run-1", "stage-1", "fhm_tok", &sink)
	if _, err := tee.Write([]byte(testHeartbeat)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	tee.waitForReports()
	if !strings.Contains(sink.String(), "stage_progress_report_failed") {
		t.Errorf("diag missing swallowed transport error:\n%s", sink.String())
	}
}

// TestProgressTee_SlowBackendDoesNotBlockTheSink: Write returns immediately even
// when the backend blocks — the POST is async — so a slow report cannot stall
// the agent's heartbeat goroutine. Asserts the Write returns well under the 5s
// per-call timeout.
func TestProgressTee_SlowBackendDoesNotBlockTheSink(t *testing.T) {
	var sink syncBuf
	gate := make(chan struct{})
	rep := &fakeReporter{block: gate}
	tee := newProgressTee(&sink, rep, "run-1", "stage-1", "fhm_tok", &sink)

	start := time.Now()
	if _, err := tee.Write([]byte(testHeartbeat)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if elapsed := time.Since(start); elapsed > progressReportTimeout {
		t.Errorf("Write blocked for %v, want << %v (async POST)", elapsed, progressReportTimeout)
	}
	// The heartbeat was forwarded immediately, before the report unblocks.
	if sink.String() != testHeartbeat {
		t.Errorf("forwarded bytes = %q, want verbatim before the report completes", sink.String())
	}
	close(gate) // release the report so the goroutine can exit
	tee.waitForReports()
}

// TestProgressTee_ConcurrentTicksDoNotLeakGoroutines pins the single-in-flight
// cap: a tick issued while a report is outstanding is SKIPPED, not queued, so a
// wedged backend cannot leak goroutines. Drives many concurrent Writes; asserts
// the reporter never sees more than one concurrent call and all goroutines
// return. Run under -race, a report goroutine's diag write overlaps the
// concurrent heartbeat forwards on the shared syncBuf.
func TestProgressTee_ConcurrentTicksDoNotLeakGoroutines(t *testing.T) {
	var sink syncBuf
	// Every report errors immediately, forcing a diag write that OVERLAPS the
	// concurrent forwards (approval condition 1 — the -race target).
	rep := &fakeReporter{err: &statusErr{code: 500}}
	tee := newProgressTee(&sink, rep, "run-1", "stage-1", "fhm_tok", &sink)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 25; j++ {
				if _, err := tee.Write([]byte(testHeartbeat)); err != nil {
					t.Errorf("Write: %v", err)
				}
			}
		}()
	}
	wg.Wait()
	tee.waitForReports()

	if got := rep.maxSeen.Load(); got > 1 {
		t.Errorf("max concurrent reports = %d, want <= 1 (single-in-flight cap)", got)
	}
	if got := rep.inFlight.Load(); got != 0 {
		t.Errorf("in-flight after wait = %d, want 0 (no leaked goroutines)", got)
	}
}

// statusErr is a minimal error double for a non-2xx report result.
type statusErr struct{ code int }

func (e *statusErr) Error() string { return "report failed: status " + itoa(e.code) }

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [12]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
