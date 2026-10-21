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

// TestProgressTee_TokenSourceRefreshesBearer (#3255): a tee whose source holds
// an EXPIRED token POSTs the refreshed bearer; a nil source POSTs the
// constructor token byte-for-byte (the no-regression control).
func TestProgressTee_TokenSourceRefreshesBearer(t *testing.T) {
	var sink syncBuf
	rep := &fakeReporter{}
	tee := newProgressTee(&sink, rep, "run-1", "stage-1", "fhm_expired", &sink)
	src, _ := newExpiredTokenSource()
	tee.tokens = src
	_, _ = tee.Write([]byte(testHeartbeat))
	tee.waitForReports()
	if rep.callCount() != 1 || rep.calls[0].MCPToken != "fhm_refreshed_1" {
		t.Fatalf("posted = %+v, want one POST carrying fhm_refreshed_1", rep.calls)
	}

	rep2 := &fakeReporter{}
	tee2 := newProgressTee(&sink, rep2, "run-1", "stage-1", "fhm_tok", &sink)
	_, _ = tee2.Write([]byte(testHeartbeat))
	tee2.waitForReports()
	if rep2.callCount() != 1 || rep2.calls[0].MCPToken != "fhm_tok" {
		t.Fatalf("nil-source posted = %+v, want constructor token fhm_tok", rep2.calls)
	}
}

// TestProgressTee_HeartbeatCarriesPhaseSnapshot (#4023): with a stage clock
// attached, a heartbeat POST carries the clock's snapshot read at Write time —
// agent-phase fields only in the agent phase, all five in the gate phase, and
// a negative extrapolated remainder clamped to 0. Without a clock the five
// fields stay unset (the byte-identical legacy body).
func TestProgressTee_HeartbeatCarriesPhaseSnapshot(t *testing.T) {
	var sink syncBuf
	rep := &fakeReporter{}
	tee := newProgressTee(&sink, rep, "run-1", "stage-1", "fhm_tok", &sink)

	_, _ = tee.Write([]byte(testHeartbeat))
	tee.waitForReports()
	if got := rep.calls[0]; got.Phase != "" || got.PhaseElapsedSeconds != nil || got.GateBudgetSeconds != nil ||
		got.GateRemainingSeconds != nil || got.GateVerifying {
		t.Errorf("clock-less heartbeat carried phase fields: %+v", got)
	}

	clock, mc := newTestPhaseClock(time.Hour, time.Minute)
	tee.phases = clock
	clock.beginAgent()
	mc.advance(90 * time.Second)
	_, _ = tee.Write([]byte(testHeartbeat))
	tee.waitForReports()
	got := rep.calls[1]
	if got.Phase != stagePhaseAgent || got.PhaseElapsedSeconds == nil || *got.PhaseElapsedSeconds != 90 {
		t.Errorf("agent heartbeat = %+v, want phase=agent phase_elapsed=90", got)
	}
	if got.GateBudgetSeconds != nil || got.GateRemainingSeconds != nil || got.GateVerifying {
		t.Errorf("agent heartbeat carried gate fields: %+v", got)
	}
	if got.LastEvent != "assistant" || got.TurnsThisAttempt != 9 || got.TokensThisAttempt != 13402 {
		t.Errorf("agent heartbeat lost its counters: %+v", got)
	}

	clock.beginGate()
	clock.startVerify()
	mc.advance(clock.gateBudget + time.Minute) // overdrawn while in flight
	_, _ = tee.Write([]byte(testHeartbeat))
	tee.waitForReports()
	got = rep.calls[2]
	if got.Phase != stagePhaseGate || !got.GateVerifying || got.GateBudgetSeconds == nil || *got.GateBudgetSeconds != 17*60 {
		t.Errorf("gate heartbeat = %+v, want phase=gate verifying budget=1020", got)
	}
	if got.GateRemainingSeconds == nil || *got.GateRemainingSeconds != 0 {
		t.Errorf("gate heartbeat remaining = %v, want clamped to 0 (the ingest rejects negatives)", got.GateRemainingSeconds)
	}
	if got.PhaseElapsedSeconds == nil || *got.PhaseElapsedSeconds != 18*60 {
		t.Errorf("gate heartbeat phase_elapsed = %v, want 1080", got.PhaseElapsedSeconds)
	}
}

// orderReporter records the order in which reports COMPLETE and blocks the
// FIRST call until released, flagging any call that ENTERS while that first
// call is still in flight.
type orderReporter struct {
	mu          sync.Mutex
	calls       []upload.ReportStageProgressArgs
	first       chan struct{} // released by the test
	firstActive bool
	overlapped  bool
	entered     chan struct{} // closed when a second call enters
	n           int
}

func (o *orderReporter) ReportStageProgress(_ context.Context, args upload.ReportStageProgressArgs) error {
	o.mu.Lock()
	o.n++
	idx := o.n
	if idx == 1 {
		o.firstActive = true
	} else {
		if o.firstActive {
			o.overlapped = true
		}
		close(o.entered)
	}
	o.mu.Unlock()
	if idx == 1 {
		<-o.first
	}
	o.mu.Lock()
	if idx == 1 {
		o.firstActive = false
	}
	o.calls = append(o.calls, args)
	o.mu.Unlock()
	return nil
}

// TestProgressTee_ReportPhaseWaitsForInFlightHeartbeat (CF-J vehicle): a phase
// report issued while a heartbeat POST is in flight waits for it, so the
// heartbeat cannot land after the transition and flip the stored phase back
// (last-writer-wins). The heartbeat is blocked until the phase report has
// either entered the reporter (the defect) or had ample time to.
func TestProgressTee_ReportPhaseWaitsForInFlightHeartbeat(t *testing.T) {
	var sink syncBuf
	rep := &orderReporter{first: make(chan struct{}), entered: make(chan struct{})}
	tee := newProgressTee(&sink, rep, "run-1", "stage-1", "fhm_tok", &sink)

	_, _ = tee.Write([]byte(testHeartbeat))
	deadline := time.Now().Add(scaledD(5 * time.Second))
	for {
		rep.mu.Lock()
		active := rep.firstActive
		rep.mu.Unlock()
		if active {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("precondition: the heartbeat POST never went in flight")
		}
		time.Sleep(20 * time.Millisecond)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		tee.reportPhase(stagePhaseSnapshot{Phase: stagePhaseGate, GateBudget: time.Hour, GateRemaining: time.Hour})
	}()
	select {
	case <-rep.entered:
	case <-time.After(scaledD(300 * time.Millisecond)):
	}
	close(rep.first)
	<-done
	tee.waitForReports()

	rep.mu.Lock()
	defer rep.mu.Unlock()
	if rep.overlapped {
		t.Error("the phase report POSTed while the heartbeat was still in flight; it must wait for it")
	}
	if len(rep.calls) != 2 || rep.calls[0].LastEvent != "assistant" || rep.calls[1].Phase != stagePhaseGate {
		t.Errorf("completion order = %+v, want [heartbeat, gate phase report]", rep.calls)
	}
}

// TestProgressTee_ReportPhaseNoClientIsNoOp: a nil tee, a nil client and an
// empty token (the acceptance stage's zero-credential posture) all make
// reportPhase a no-op.
func TestProgressTee_ReportPhaseNoClientIsNoOp(t *testing.T) {
	var nilTee *progressTee
	nilTee.reportPhase(stagePhaseSnapshot{Phase: stagePhaseAgent})

	var sink syncBuf
	newProgressTee(&sink, nil, "run-1", "stage-1", "fhm_tok", &sink).reportPhase(stagePhaseSnapshot{Phase: stagePhaseAgent})

	rep := &fakeReporter{}
	newProgressTee(&sink, rep, "run-1", "stage-1", "", &sink).reportPhase(stagePhaseSnapshot{Phase: stagePhaseAgent})
	if rep.callCount() != 0 {
		t.Errorf("POST count = %d, want 0 (no token → no phase report)", rep.callCount())
	}
	if sink.String() != "" {
		t.Errorf("a no-op phase report wrote %q", sink.String())
	}
}

// TestProgressTee_ReportPhaseFailureLoggedAndDropped: a failed phase report is
// one stage_progress_report_failed diag line and is dropped; the tee keeps
// working afterwards (the slot is released).
func TestProgressTee_ReportPhaseFailureLoggedAndDropped(t *testing.T) {
	var sink syncBuf
	rep := &fakeReporter{err: &statusErr{code: 400}}
	tee := newProgressTee(&sink, rep, "run-1", "stage-1", "fhm_tok", &sink)
	tee.reportPhase(stagePhaseSnapshot{Phase: stagePhaseGate})
	if got := strings.Count(sink.String(), "stage_progress_report_failed"); got != 1 {
		t.Errorf("diag carries %d stage_progress_report_failed lines, want 1:\n%s", got, sink.String())
	}
	_, _ = tee.Write([]byte(testHeartbeat))
	tee.waitForReports()
	if rep.callCount() != 2 {
		t.Errorf("POST count = %d, want 2 (the slot must be released after a failed phase report)", rep.callCount())
	}
}

// TestProgressTee_ReportPhaseCarriesCountersAndEvent: a phase report carries
// the last heartbeat's turns/tokens, a runner-authored last_event per
// transition, and the snapshot's phase fields.
func TestProgressTee_ReportPhaseCarriesCountersAndEvent(t *testing.T) {
	var sink syncBuf
	rep := &fakeReporter{}
	tee := newProgressTee(&sink, rep, "run-1", "stage-1", "fhm_tok", &sink)

	tee.reportPhase(stagePhaseSnapshot{Phase: stagePhaseAgent, PhaseElapsed: 0})
	_, _ = tee.Write([]byte(testHeartbeat))
	tee.waitForReports()
	tee.reportPhase(stagePhaseSnapshot{Phase: stagePhaseGate, PhaseElapsed: 61 * time.Second, GateBudget: 17 * time.Minute, GateRemaining: 16 * time.Minute, Verifying: true})
	tee.reportPhase(stagePhaseSnapshot{Phase: stagePhaseGate, GateBudget: 17 * time.Minute, GateRemaining: 15 * time.Minute})

	if rep.callCount() != 4 {
		t.Fatalf("POST count = %d, want 4", rep.callCount())
	}
	first := rep.calls[0]
	if first.LastEvent != phaseEventAgentStart || first.TurnsThisAttempt != 0 || first.Phase != stagePhaseAgent ||
		first.PhaseElapsedSeconds == nil || *first.PhaseElapsedSeconds != 0 || first.GateBudgetSeconds != nil {
		t.Errorf("agent_start report = %+v", first)
	}
	verify := rep.calls[2]
	if verify.LastEvent != phaseEventGateVerify || verify.TurnsThisAttempt != 9 || verify.TokensThisAttempt != 13402 ||
		!verify.GateVerifying || *verify.GateBudgetSeconds != 1020 || *verify.GateRemainingSeconds != 960 || *verify.PhaseElapsedSeconds != 61 {
		t.Errorf("gate:verify report = %+v", verify)
	}
	gate := rep.calls[3]
	if gate.LastEvent != phaseEventGate || gate.GateVerifying || *gate.GateRemainingSeconds != 900 {
		t.Errorf("gate report = %+v", gate)
	}
	if got := phaseReportEvent(stagePhaseSnapshot{}); got != "" {
		t.Errorf("phaseReportEvent(zero) = %q, want empty", got)
	}
}

// slowIngestReporter simulates slow progress ingestion on the stage clock's
// manual time source: every report advances it by delay, then returns err.
type slowIngestReporter struct {
	mc    *manualClock
	delay time.Duration
	err   error
	calls atomic.Int32
}

func (s *slowIngestReporter) ReportStageProgress(ctx context.Context, _ upload.ReportStageProgressArgs) error {
	s.calls.Add(1)
	if _, ok := ctx.Deadline(); !ok {
		return context.Canceled // the POST must run under a bounded context
	}
	s.mc.advance(s.delay)
	return s.err
}

// TestProgressTee_SlowPhaseReportsAreNotChargedToGateBudget drives the REAL
// reportPhase as the clock's report hook against delayed and timing-out
// progress ingestion: the gate budget is charged only the verifies' own wall
// time, so repeated slow reports never refuse a later verify.
func TestProgressTee_SlowPhaseReportsAreNotChargedToGateBudget(t *testing.T) {
	for _, tc := range []struct {
		name  string
		delay time.Duration
		err   error
	}{
		{"delayed", 4 * time.Minute, nil},
		{"timing out", 40 * time.Second, context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var sink syncBuf
			clock, mc := newTestPhaseClock(time.Hour, time.Minute) // budget 17m
			rep := &slowIngestReporter{mc: mc, delay: tc.delay, err: tc.err}
			tee := newProgressTee(&sink, rep, "run-1", "stage-1", "fhm_tok", &sink)
			tee.phases = clock
			clock.report = tee.reportPhase

			clock.beginGate()
			for i := 0; i < 5; i++ {
				clock.startVerify()
				mc.advance(30 * time.Second)
				clock.endVerify()
			}
			if got := rep.calls.Load(); got != 11 {
				t.Fatalf("phase reports = %d, want 11 (beginGate + 5 x start/end)", got)
			}
			if got, want := clock.gateRemaining(), clock.gateBudget-150*time.Second; got != want {
				t.Errorf("gateRemaining = %s, want %s (five 30s verifies; report latency uncharged)", got, want)
			}
			if eff, clipped := clock.gateVerifyTimeout(time.Minute); eff != time.Minute || clipped {
				t.Errorf("gateVerifyTimeout(1m) = (%s, %t), want (1m, false)", eff, clipped)
			}
			if clock.gateExhausted() {
				t.Error("gate budget exhausted by slow progress ingestion")
			}
			wantFailures := 0
			if tc.err != nil {
				wantFailures = 11
			}
			if got := strings.Count(sink.String(), "stage_progress_report_failed"); got != wantFailures {
				t.Errorf("stage_progress_report_failed lines = %d, want %d", got, wantFailures)
			}
		})
	}
}
