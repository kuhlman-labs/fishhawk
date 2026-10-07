package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/apitoken"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concurrency"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	runpkg "github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/server"
	"github.com/kuhlman-labs/fishhawk/backend/internal/timescale"
)

// markerAnswer is one scripted host-dispatch marker answer.
type markerAnswer struct {
	res *HostDispatchResult
	err error
}

// seqMarker is the in-file sequenced fake of slotMarker:
// HostDispatchStageWithNonce consumes answers in order (the last one
// repeats), and every call is recorded under mu (the waiter runs on its own
// goroutine).
type seqMarker struct {
	mu        sync.Mutex
	answers   []markerAnswer
	calls     int
	nonces    []string // the nonce each marker call carried
	reports   []string // expected_state|category|reason per ReportStageFailureFrom
	reportErr error
	// stageWait / stageWaitErr answer GetRunStageWait (the lost-response read).
	stageWait      *RunStageWait
	stageWaitErr   error
	stageWaitCalls int
	// block, when non-nil, makes HostDispatchStage wait on it and then answer
	// a transport error if its ctx is done (the cap-vs-in-flight pin).
	block <-chan struct{}
}

func (m *seqMarker) HostDispatchStageWithNonce(ctx context.Context, _, _ uuid.UUID, nonce string) (*HostDispatchResult, error) {
	if m.block != nil {
		<-m.block
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	i := m.calls
	m.calls++
	m.nonces = append(m.nonces, nonce)
	if i >= len(m.answers) {
		i = len(m.answers) - 1
	}
	a := m.answers[i]
	return a.res, a.err
}

func (m *seqMarker) ReportStageFailureFrom(_ context.Context, _, _ uuid.UUID, expectedState, category, reason, _ string, _ int) (*ReapFailureResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reports = append(m.reports, expectedState+"|"+category+"|"+reason)
	if m.reportErr != nil {
		return nil, m.reportErr
	}
	return &ReapFailureResult{Transitioned: true, StageState: "failed"}, nil
}

func (m *seqMarker) GetRunStageWait(_ context.Context, _, _ uuid.UUID, _ int) (*RunStageWait, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stageWaitCalls++
	return m.stageWait, m.stageWaitErr
}

func (m *seqMarker) snapshot() (calls int, reports []string, stageWaitCalls int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls, append([]string(nil), m.reports...), m.stageWaitCalls
}

// slotSpawnRecorder records waiter spawns (the "fake runners").
type slotSpawnRecorder struct {
	mu       sync.Mutex
	binaries []string
	stages   []string
	err      error
	errFor   map[string]bool // stage ids whose spawn fails
}

func (s *slotSpawnRecorder) spawn(binary string, _, _ []string, _, stageID string, _ detachedFailureReporter, _ detachedStageStateProbe) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil || s.errFor[stageID] {
		return "", errors.New("spawn boom")
	}
	s.binaries = append(s.binaries, binary)
	s.stages = append(s.stages, stageID)
	return "/dev/null", nil
}

func (s *slotSpawnRecorder) spawned() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.stages...)
}

func queuedAnswer(enq time.Time) markerAnswer {
	return queuedAnswerContended(enq, false)
}

func queuedAnswerContended(enq time.Time, contended bool) markerAnswer {
	return markerAnswer{err: newConcurrencySlotQueuedError(&apiError{
		StatusCode: 409, Code: concurrencySlotQueuedCode,
		Details: map[string]any{
			"group": "local-implement:h1", "limit": 1, "position": 1,
			"holders": []any{}, "enqueued_at": enq.Format(time.RFC3339Nano),
			"contended": contended,
		},
	})}
}

var (
	admittedAnswer   = markerAnswer{res: &HostDispatchResult{Transitioned: true, StageState: "dispatched"}}
	otherWinsAnswer  = markerAnswer{res: &HostDispatchResult{Transitioned: false, StageState: "dispatched"}}
	transportAnswer  = markerAnswer{err: errors.New("dial tcp 127.0.0.1:8080: connect: connection refused")}
	unavailableAnswr = markerAnswer{err: &apiError{StatusCode: 503, Code: "internal_error"}}
	notAdmissibleAns = markerAnswer{err: &apiError{StatusCode: 409, Code: "dispatch_not_admissible"}}
)

// testSlotNonce is the admission nonce testSlotSpec gives its waiter.
const testSlotNonce = "nonce-own"

// testSlotSpec builds a fast waiter spec over m and rec.
func testSlotSpec(m *seqMarker, rec *slotSpawnRecorder) slotWaiterSpec {
	return slotWaiterSpec{
		runID:   uuid.New(),
		stageID: uuid.New(),
		nonce:   testSlotNonce,
		marker:  m,
		prepare: func() (slotSpawnInputs, error) { return slotSpawnInputs{binary: "/bin/runner"}, nil },
		spawn:   rec.spawn,
		cap:     time.Minute,
		poll:    time.Millisecond,
		logf:    func(string, ...any) {},
	}
}

// waitDone fails the test if done does not close within a bounded wait.
func waitDone(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(timescale.D(10 * time.Second)):
		t.Fatal("slot waiter did not exit")
	}
}

func TestSlotWaiter_SpawnsOnceOnAdmit(t *testing.T) {
	enq := time.Now()
	m := &seqMarker{answers: []markerAnswer{queuedAnswer(enq), queuedAnswer(enq), admittedAnswer}}
	rec := &slotSpawnRecorder{}
	status, done := slotWaiters.start(testSlotSpec(m, rec))
	if status != slotWaiterStarted {
		t.Fatalf("status = %q, want started", status)
	}
	waitDone(t, done)
	if got := rec.spawned(); len(got) != 1 {
		t.Fatalf("spawns = %v, want exactly one on admission", got)
	}
	if calls, reports, _ := m.snapshot(); calls != 3 || len(reports) != 0 {
		t.Fatalf("marker calls = %d (want 3), reports = %v (want none)", calls, reports)
	}
}

func TestSlotWaiter_NoSpawnWhenAnotherSessionWins(t *testing.T) {
	enq := time.Now()
	m := &seqMarker{answers: []markerAnswer{queuedAnswer(enq), otherWinsAnswer}}
	rec := &slotSpawnRecorder{}
	_, done := slotWaiters.start(testSlotSpec(m, rec))
	waitDone(t, done)
	if got := rec.spawned(); len(got) != 0 {
		t.Fatalf("spawns = %v, want none: transitioned:false means another session admitted the stage", got)
	}
	if _, _, reads := m.snapshot(); reads != 0 {
		t.Fatalf("stage reads = %d, want 0: with no lost response there is nothing to disambiguate", reads)
	}
}

func TestSlotWaiter_RetriesTransient(t *testing.T) {
	for name, transient := range map[string]markerAnswer{"5xx": unavailableAnswr, "transport": transportAnswer} {
		t.Run(name, func(t *testing.T) {
			m := &seqMarker{answers: []markerAnswer{transient, transient, admittedAnswer}}
			rec := &slotSpawnRecorder{}
			_, done := slotWaiters.start(testSlotSpec(m, rec))
			waitDone(t, done)
			if got := rec.spawned(); len(got) != 1 {
				t.Fatalf("spawns = %v, want one after the transient failures", got)
			}
			if calls, _, _ := m.snapshot(); calls != 3 {
				t.Fatalf("marker calls = %d, want 3", calls)
			}
		})
	}
}

func TestSlotWaiter_StopsOnTerminal4xx(t *testing.T) {
	m := &seqMarker{answers: []markerAnswer{notAdmissibleAns, admittedAnswer}}
	rec := &slotSpawnRecorder{}
	spec := testSlotSpec(m, rec)
	_, done := slotWaiters.start(spec)
	waitDone(t, done)
	if calls, _, _ := m.snapshot(); calls != 1 {
		t.Fatalf("marker calls = %d, want 1 (a terminal 4xx stops the waiter)", calls)
	}
	if got := rec.spawned(); len(got) != 0 {
		t.Fatalf("spawns = %v, want none", got)
	}
	// The registry entry was cleared: the same stage can start again.
	m2 := &seqMarker{answers: []markerAnswer{admittedAnswer}}
	spec.marker = m2
	status, done2 := slotWaiters.start(spec)
	if status != slotWaiterStarted {
		t.Fatalf("restart status = %q, want started (registry cleared on exit)", status)
	}
	waitDone(t, done2)
}

// TestSlotWaiter_LostResponse pins approval condition 3: a transport error
// followed by 200 transitioned:false is resolved from the stage's holding
// block, by the admission nonce the server recorded — never by the host label,
// which two sessions on one host share.
func TestSlotWaiter_LostResponse(t *testing.T) {
	enq := time.Now().UTC()
	holding := func(nonce, state string) *RunStageWait {
		return &RunStageWait{State: state, Concurrency: &StageConcurrency{
			Status: concurrencyStatusHolding, Host: "h1", HeldDispatchedAt: &enq, AdmissionNonce: nonce,
		}}
	}
	cases := []struct {
		name      string
		stageWait *RunStageWait
		readErr   error
		wantSpawn int
	}{
		{"own admission spawns once", holding(testSlotNonce, "dispatched"), nil, 1},
		{"another session on the SAME host spawns nothing", holding("nonce-other-session", "dispatched"), nil, 0},
		{"a direct dispatch_stage admission (no nonce) spawns nothing", holding("", "dispatched"), nil, 0},
		{"a running stage already has a runner", holding(testSlotNonce, "running"), nil, 0},
		{"no holding block spawns nothing", &RunStageWait{State: "dispatched"}, nil, 0},
		{"a stage read error spawns nothing", nil, errors.New("read boom"), 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := &seqMarker{
				answers:      []markerAnswer{queuedAnswer(enq), transportAnswer, otherWinsAnswer},
				stageWait:    tc.stageWait,
				stageWaitErr: tc.readErr,
			}
			rec := &slotSpawnRecorder{}
			_, done := slotWaiters.start(testSlotSpec(m, rec))
			waitDone(t, done)
			if got := rec.spawned(); len(got) != tc.wantSpawn {
				t.Fatalf("spawns = %v, want %d", got, tc.wantSpawn)
			}
			if _, _, reads := m.snapshot(); reads != 1 {
				t.Fatalf("stage reads = %d, want 1 (the lost-response disambiguation)", reads)
			}
		})
	}

	// A waiter with no nonce never claims a lost admission, not even one
	// whose recorded nonce is also empty (a direct dispatch's).
	t.Run("an empty nonce never claims", func(t *testing.T) {
		m := &seqMarker{
			answers:   []markerAnswer{transportAnswer, otherWinsAnswer},
			stageWait: holding("", "dispatched"),
		}
		rec := &slotSpawnRecorder{}
		spec := testSlotSpec(m, rec)
		spec.nonce = ""
		_, done := slotWaiters.start(spec)
		waitDone(t, done)
		if got := rec.spawned(); len(got) != 0 {
			t.Fatalf("spawns = %v, want none", got)
		}
	})

	// A definitive queued answer after the lost request clears it: a later
	// transitioned:false is another session's, even with an own-looking block.
	t.Run("queued after the loss clears it", func(t *testing.T) {
		m := &seqMarker{
			answers:   []markerAnswer{transportAnswer, queuedAnswer(enq), otherWinsAnswer},
			stageWait: holding(testSlotNonce, "dispatched"),
		}
		rec := &slotSpawnRecorder{}
		_, done := slotWaiters.start(testSlotSpec(m, rec))
		waitDone(t, done)
		if got := rec.spawned(); len(got) != 0 {
			t.Fatalf("spawns = %v, want none", got)
		}
	})
}

// TestSlotWaiter_SendsItsNonceOnEveryPoll: the nonce rides every marker POST,
// so whichever poll is admitted records it.
func TestSlotWaiter_SendsItsNonceOnEveryPoll(t *testing.T) {
	enq := time.Now()
	m := &seqMarker{answers: []markerAnswer{queuedAnswer(enq), transportAnswer, admittedAnswer}}
	rec := &slotSpawnRecorder{}
	_, done := slotWaiters.start(testSlotSpec(m, rec))
	waitDone(t, done)
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.nonces) != 3 {
		t.Fatalf("marker calls = %d, want 3", len(m.nonces))
	}
	for i, n := range m.nonces {
		if n != testSlotNonce {
			t.Fatalf("poll %d carried nonce %q, want %q", i, n, testSlotNonce)
		}
	}
}

func TestNewAdmissionNonce(t *testing.T) {
	a, b := newAdmissionNonce(), newAdmissionNonce()
	if a == b || len(a) != 36 {
		t.Fatalf("nonces %q, %q; want two distinct UUIDs", a, b)
	}
}

// TestJitteredPoll pins the ±20% bounds of one poll wait.
func TestJitteredPoll(t *testing.T) {
	base := 5 * time.Second
	if got := jitteredPoll(base, 0); got != 4*time.Second {
		t.Fatalf("jitteredPoll(r=0) = %v, want 4s (-20%%)", got)
	}
	if got := jitteredPoll(base, 0.5); got != base {
		t.Fatalf("jitteredPoll(r=0.5) = %v, want 5s", got)
	}
	if got := jitteredPoll(base, 0.999999); got < 5999*time.Millisecond || got >= 6*time.Second {
		t.Fatalf("jitteredPoll(r→1) = %v, want just under 6s (+20%%)", got)
	}
	for i := 0; i < 100; i++ {
		if got := randomJitteredPoll(base); got < 4*time.Second || got >= 6*time.Second {
			t.Fatalf("randomJitteredPoll = %v, want within [4s, 6s)", got)
		}
	}
}

// seqJitter answers its waits in order, repeating the last.
func seqJitter(waits ...time.Duration) func(time.Duration) time.Duration {
	var mu sync.Mutex
	i := 0
	return func(time.Duration) time.Duration {
		mu.Lock()
		defer mu.Unlock()
		w := waits[min(i, len(waits)-1)]
		i++
		return w
	}
}

// TestSlotWaiter_ContendedAnswerRetriesOnceAtOnce: a contended queued answer
// is retried immediately, once; every other wait is a full jittered poll
// (here an hour, far past the cap, so only an immediate retry can poll again).
func TestSlotWaiter_ContendedAnswerRetriesOnceAtOnce(t *testing.T) {
	enq := time.Now()
	cases := []struct {
		name          string
		contendedSpec bool
		answers       []markerAnswer
		jitter        []time.Duration
		wantCalls     int
		wantSpawn     int
	}{
		{"a contended answer is retried at once", false,
			[]markerAnswer{queuedAnswerContended(enq, true), admittedAnswer}, []time.Duration{time.Millisecond, time.Hour}, 2, 1},
		{"the immediate retry is not retried again", false,
			[]markerAnswer{queuedAnswerContended(enq, true), queuedAnswerContended(enq, true), admittedAnswer}, []time.Duration{time.Millisecond, time.Hour}, 2, 0},
		{"an uncontended answer waits a full poll", false,
			[]markerAnswer{queuedAnswer(enq), admittedAnswer}, []time.Duration{time.Millisecond, time.Hour}, 1, 0},
		{"the dispatch call's contended answer makes the first poll immediate", true,
			[]markerAnswer{admittedAnswer}, []time.Duration{time.Hour}, 1, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := &seqMarker{answers: tc.answers}
			rec := &slotSpawnRecorder{}
			spec := testSlotSpec(m, rec)
			spec.contended = tc.contendedSpec
			spec.jitter = seqJitter(tc.jitter...)
			spec.cap = timescale.D(300 * time.Millisecond)
			_, done := slotWaiters.start(spec)
			waitDone(t, done)
			if calls, _, _ := m.snapshot(); calls != tc.wantCalls {
				t.Fatalf("marker calls = %d, want %d", calls, tc.wantCalls)
			}
			if got := rec.spawned(); len(got) != tc.wantSpawn {
				t.Fatalf("spawns = %v, want %d", got, tc.wantSpawn)
			}
		})
	}
}

func TestSlotWaiter_RebuildsSpawnInputsAtAdmission(t *testing.T) {
	enq := time.Now()
	gate := make(chan struct{})
	m := &seqMarker{answers: []markerAnswer{queuedAnswer(enq), queuedAnswer(enq), admittedAnswer}}
	rec := &slotSpawnRecorder{}
	spec := testSlotSpec(m, rec)
	var mu sync.Mutex
	binary := "/old/fishhawk-runner"
	prepared := 0
	spec.prepare = func() (slotSpawnInputs, error) {
		mu.Lock()
		defer mu.Unlock()
		prepared++
		return slotSpawnInputs{binary: binary}, nil
	}
	// Hold the waiter before its first poll until the binary changed.
	inner := spec.marker
	spec.marker = gatedMarker{slotMarker: inner, gate: gate}
	_, done := slotWaiters.start(spec)
	mu.Lock()
	binary = "/new/fishhawk-runner"
	if prepared != 0 {
		t.Errorf("prepare ran %d times while queued, want 0", prepared)
	}
	mu.Unlock()
	close(gate)
	waitDone(t, done)
	if prepared != 1 {
		t.Fatalf("prepare ran %d times, want exactly 1 (at admission)", prepared)
	}
	if rec.binaries[0] != "/new/fishhawk-runner" {
		t.Fatalf("spawned binary = %q, want the binary current at admission", rec.binaries[0])
	}
}

// gatedMarker holds every marker call until gate closes.
type gatedMarker struct {
	slotMarker
	gate <-chan struct{}
}

func (g gatedMarker) HostDispatchStageWithNonce(ctx context.Context, runID, stageID uuid.UUID, nonce string) (*HostDispatchResult, error) {
	<-g.gate
	return g.slotMarker.HostDispatchStageWithNonce(ctx, runID, stageID, nonce)
}

func TestSlotWaiter_CapExpiryStops(t *testing.T) {
	enq := time.Now()
	m := &seqMarker{answers: []markerAnswer{queuedAnswer(enq)}}
	rec := &slotSpawnRecorder{}
	spec := testSlotSpec(m, rec)
	spec.cap = 50 * time.Millisecond
	var logged []string
	var lmu sync.Mutex
	spec.logf = func(f string, a ...any) {
		lmu.Lock()
		defer lmu.Unlock()
		logged = append(logged, f)
	}
	_, done := slotWaiters.start(spec)
	waitDone(t, done)
	if got := rec.spawned(); len(got) != 0 {
		t.Fatalf("spawns = %v, want none (always queued)", got)
	}
	lmu.Lock()
	if len(logged) != 1 || !strings.Contains(logged[0], "cap") {
		t.Errorf("cap-expiry log = %q, want one line naming the cap", logged)
	}
	lmu.Unlock()
	spec.marker = &seqMarker{answers: []markerAnswer{admittedAnswer}}
	status, done2 := slotWaiters.start(spec)
	if status != slotWaiterStarted {
		t.Fatalf("start after cap expiry = %q, want started", status)
	}
	waitDone(t, done2)
}

// TestSlotWaiter_CapDoesNotCancelInFlightAdmission: the cap stops polling but
// never cancels an in-flight marker POST, whose admission would otherwise be
// lost with no spawn.
func TestSlotWaiter_CapDoesNotCancelInFlightAdmission(t *testing.T) {
	block := make(chan struct{})
	m := &seqMarker{answers: []markerAnswer{admittedAnswer}, block: block}
	rec := &slotSpawnRecorder{}
	spec := testSlotSpec(m, rec)
	spec.cap = 20 * time.Millisecond
	_, done := slotWaiters.start(spec)
	time.Sleep(timescale.D(200 * time.Millisecond)) // the cap expires mid-request
	close(block)
	waitDone(t, done)
	if got := rec.spawned(); len(got) != 1 {
		t.Fatalf("spawns = %v, want 1: an admission answered after the cap still spawns", got)
	}
}

func TestSlotWaiter_DedupesPerStage(t *testing.T) {
	gate := make(chan struct{})
	m := &seqMarker{answers: []markerAnswer{admittedAnswer}}
	rec := &slotSpawnRecorder{}
	spec := testSlotSpec(m, rec)
	spec.marker = gatedMarker{slotMarker: m, gate: gate}
	status1, done1 := slotWaiters.start(spec)
	status2, done2 := slotWaiters.start(spec)
	if status1 != slotWaiterStarted || status2 != slotWaiterAlreadyWaiting {
		t.Fatalf("statuses = %q, %q; want started, already_waiting", status1, status2)
	}
	if done1 != done2 {
		t.Error("already_waiting must hand back the running waiter's done channel")
	}
	close(gate)
	waitDone(t, done1)
	if calls, _, _ := m.snapshot(); calls != 1 {
		t.Fatalf("marker calls = %d, want 1 (one poller)", calls)
	}
	if got := rec.spawned(); len(got) != 1 {
		t.Fatalf("spawns = %v, want 1", got)
	}
}

func TestSlotWaiter_SpawnFailureReleasesSlot(t *testing.T) {
	for name, mutate := range map[string]func(*slotWaiterSpec, *slotSpawnRecorder){
		"spawn error": func(_ *slotWaiterSpec, rec *slotSpawnRecorder) { rec.err = errors.New("boom") },
		"prepare error": func(s *slotWaiterSpec, _ *slotSpawnRecorder) {
			s.prepare = func() (slotSpawnInputs, error) { return slotSpawnInputs{}, errors.New("no runner") }
		},
	} {
		t.Run(name, func(t *testing.T) {
			m := &seqMarker{answers: []markerAnswer{admittedAnswer}}
			rec := &slotSpawnRecorder{}
			spec := testSlotSpec(m, rec)
			mutate(&spec, rec)
			_, done := slotWaiters.start(spec)
			waitDone(t, done)
			_, reports, _ := m.snapshot()
			if len(reports) != 1 || !strings.HasPrefix(reports[0], "dispatched|C|concurrency slot waiter: spawn failed after admission: ") {
				t.Fatalf("reap reports = %q, want one pinned to dispatched, category C", reports)
			}
		})
	}
	// A failed release is logged, not retried forever.
	m := &seqMarker{answers: []markerAnswer{admittedAnswer}, reportErr: errors.New("reap boom")}
	rec := &slotSpawnRecorder{err: errors.New("boom")}
	spec := testSlotSpec(m, rec)
	var logged string
	spec.logf = func(f string, a ...any) { logged = f }
	_, done := slotWaiters.start(spec)
	waitDone(t, done)
	if !strings.Contains(logged, "slot release failed") {
		t.Fatalf("log = %q, want the failed release named", logged)
	}
}

// --- END-TO-END: the real server over pgtest Postgres --------------------

// e2eSlotFixture is a real server.New over pgtest Postgres with the Postgres
// concurrency store, a real operator token, and a resolver against it.
type e2eSlotFixture struct {
	ctx     context.Context
	runRepo runpkg.Repository
	r       *runResolver
	rec     *slotSpawnRecorder
	// baseURL / bearer let a test open a SECOND session's client against the
	// same server (another MCP process on the same host).
	baseURL, bearer string
}

func newE2ESlotFixture(t *testing.T) *e2eSlotFixture {
	t.Helper()
	return newE2ESlotFixtureWith(t, nil)
}

// newE2ESlotFixtureWith is newE2ESlotFixture with the server's Postgres
// concurrency store passed through wrap (nil: unwrapped), the seam a test uses
// to act INSIDE an in-flight admission.
func newE2ESlotFixtureWith(t *testing.T, wrap func(concurrency.Store) concurrency.Store) *e2eSlotFixture {
	t.Helper()
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	runRepo := runpkg.NewPostgresRepository(pool)
	var store concurrency.Store = concurrency.NewPostgresStore(pool)
	if wrap != nil {
		store = wrap(store)
	}
	const bearer = "fhk_slot_waiter_e2e"
	s := server.New(server.Config{
		RunRepo:     runRepo,
		AuditRepo:   audit.NewPostgresRepository(pool),
		Concurrency: store,
		APITokenRepo: &stubMCPAPITokens{tok: &apitoken.Token{
			ID: uuid.New(), Subject: "github:op", Scopes: []string{"read:runs", "write:runs", "write:approvals"}, PlainText: bearer,
		}},
	})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	api := newAPIClient(config{backendURL: ts.URL, apiToken: bearer})
	api.hostLabel = "e2e-host"
	r := &runResolver{api: api, getenv: func(string) string { return "" }}

	// The two (or more) fake runners: a recorder behind the detached-spawn
	// seam. Swapped serially (no t.Parallel) and restored after every waiter
	// started here has exited.
	rec := &slotSpawnRecorder{errFor: map[string]bool{}}
	savedSpawn, savedLook := dispatchSpawnDetached, runStageLookPath
	savedPoll, savedCap := concurrencySlotPollInterval, concurrencyWaiterCap
	dispatchSpawnDetached = rec.spawn
	runStageLookPath = func(string) (string, error) { return "/fake/fishhawk-runner", nil }
	concurrencySlotPollInterval = 20 * time.Millisecond
	concurrencyWaiterCap = timescale.D(20 * time.Second)
	t.Cleanup(func() {
		dispatchSpawnDetached, runStageLookPath = savedSpawn, savedLook
		concurrencySlotPollInterval, concurrencyWaiterCap = savedPoll, savedCap
	})
	// Runs FIRST (LIFO): a waiter left running by a failed assertion exits by
	// its cap before the seams are restored and the server closes.
	t.Cleanup(func() {
		deadline := time.Now().Add(concurrencyWaiterCap + time.Second)
		for time.Now().Before(deadline) {
			slotWaiters.mu.Lock()
			n := len(slotWaiters.active)
			slotWaiters.mu.Unlock()
			if n == 0 {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	})
	return &e2eSlotFixture{ctx: ctx, runRepo: runRepo, r: r, rec: rec, baseURL: ts.URL, bearer: bearer}
}

// session opens another MCP session's client on the SAME host label.
func (f *e2eSlotFixture) session() *apiClient {
	api := newAPIClient(config{backendURL: f.baseURL, apiToken: f.bearer})
	api.hostLabel = f.r.api.hostLabel
	return api
}

// waiterSpec is a waiter for st over marker with nonce, spawning into f.rec.
func (f *e2eSlotFixture) waiterSpec(st *runpkg.Stage, marker slotMarker, nonce string) slotWaiterSpec {
	return slotWaiterSpec{
		runID: st.RunID, stageID: st.ID, nonce: nonce, marker: marker,
		prepare: func() (slotSpawnInputs, error) { return slotSpawnInputs{binary: "/fake/fishhawk-runner"}, nil },
		spawn:   f.rec.spawn,
		cap:     concurrencyWaiterCap,
		poll:    concurrencySlotPollInterval,
		logf:    func(string, ...any) {},
	}
}

// lossyMarker loses the RESPONSE of its loseAt-th call (1-based): it forwards
// that request to the real server only when send is true (the server then
// processes it), runs during() — another session acting inside the loss — and
// answers a transport error either way. Every other call is forwarded.
type lossyMarker struct {
	slotMarker
	mu     sync.Mutex
	calls  int
	loseAt int
	send   bool
	during func()
}

func (l *lossyMarker) HostDispatchStageWithNonce(ctx context.Context, runID, stageID uuid.UUID, nonce string) (*HostDispatchResult, error) {
	l.mu.Lock()
	l.calls++
	n := l.calls
	l.mu.Unlock()
	if n != l.loseAt {
		return l.slotMarker.HostDispatchStageWithNonce(ctx, runID, stageID, nonce)
	}
	if l.send {
		_, _ = l.slotMarker.HostDispatchStageWithNonce(ctx, runID, stageID, nonce)
	}
	if l.during != nil {
		l.during()
	}
	return nil, errors.New("read tcp 127.0.0.1:8080: connection reset by peer")
}

// runWaiter runs spec's poll loop synchronously under its cap.
func (f *e2eSlotFixture) runWaiter(spec slotWaiterSpec) {
	ctx, cancel := context.WithTimeout(f.ctx, spec.cap)
	defer cancel()
	runSlotWaiter(ctx, spec)
}

func (f *e2eSlotFixture) spawnCount(st *runpkg.Stage) int {
	n := 0
	for _, id := range f.rec.spawned() {
		if id == st.ID.String() {
			n++
		}
	}
	return n
}

// park creates a run with one implement stage at awaiting_host_dispatch.
func (f *e2eSlotFixture) park(t *testing.T, workflowSpec []byte) *runpkg.Stage {
	t.Helper()
	row, err := f.runRepo.CreateRun(f.ctx, runpkg.CreateRunParams{
		Repo: "x/y", WorkflowID: "feature_change", WorkflowSHA: "abc", TriggerSource: runpkg.TriggerCLI,
		WorkflowSpec: workflowSpec,
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	st, err := f.runRepo.CreateStage(f.ctx, runpkg.CreateStageParams{
		RunID: row.ID, Sequence: 1, Type: runpkg.StageTypeImplement,
		ExecutorKind: runpkg.ExecutorAgent, ExecutorRef: "claude-code",
	})
	if err != nil {
		t.Fatalf("create stage: %v", err)
	}
	if st, err = f.runRepo.TransitionStage(f.ctx, st.ID, runpkg.StageStateAwaitingHostDispatch, nil); err != nil {
		t.Fatalf("park stage: %v", err)
	}
	return st
}

func (f *e2eSlotFixture) dispatch(t *testing.T, st *runpkg.Stage) DispatchStageOutput {
	t.Helper()
	_, out, err := f.r.dispatchStage(f.ctx, nil, DispatchStageInput{
		RunID: st.RunID.String(), StageID: st.ID.String(), Workflow: "feature_change", Stage: "implement",
		WorkingDir: t.TempDir(), GitHubRepo: "x/y", PushAndOpenPR: boolPtr(false),
	})
	if err != nil {
		t.Fatalf("dispatchStage(%s): %v", st.ID, err)
	}
	return out
}

// settle moves a dispatched stage through running to succeeded.
func (f *e2eSlotFixture) settle(t *testing.T, st *runpkg.Stage) {
	t.Helper()
	for _, to := range []runpkg.StageState{runpkg.StageStateRunning, runpkg.StageStateSucceeded} {
		if _, err := f.runRepo.TransitionStage(f.ctx, st.ID, to, nil); err != nil {
			t.Fatalf("settle %s -> %s: %v", st.ID, to, err)
		}
	}
}

func (f *e2eSlotFixture) stageState(t *testing.T, st *runpkg.Stage) string {
	t.Helper()
	got, err := f.runRepo.GetStage(f.ctx, st.ID)
	if err != nil {
		t.Fatalf("get stage: %v", err)
	}
	return string(got.State)
}

// slotWaitFor polls cond within a timescale-derived bound.
func slotWaitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timescale.D(15 * time.Second))
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func slotContains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

// TestConcurrencySlot_E2E_TwoFakeRunners drives fishhawk_dispatch_stage
// against the real marker: A spawns, B queues (no spawn) behind A with a live
// waiter, and B spawns when A settles. Then C queues behind B, C's spawn fails
// at admission, C is failed category C and its slot released, so a fresh D is
// admitted at once.
func TestConcurrencySlot_E2E_TwoFakeRunners(t *testing.T) {
	f := newE2ESlotFixture(t)
	a, b := f.park(t, nil), f.park(t, nil)

	outA := f.dispatch(t, a)
	if outA.ConcurrencySlot == nil || outA.ConcurrencySlot.Group != "local-implement:e2e-host" || !slotContains(f.rec.spawned(), a.ID.String()) {
		t.Fatalf("A = %+v (slot %+v), spawns %v; want an admitted spawn in local-implement:e2e-host", outA, outA.ConcurrencySlot, f.rec.spawned())
	}

	outB := f.dispatch(t, b)
	slot := outB.ConcurrencySlot
	if slot == nil || slot.Status != concurrencyStatusAwaitingSlot || slot.Position != 1 || slot.Waiter != slotWaiterStarted ||
		len(slot.Holders) != 1 || slot.Holders[0].StageID != a.ID.String() {
		t.Fatalf("B slot = %+v, want queued at 1 behind A with a started waiter", slot)
	}
	if slotContains(f.rec.spawned(), b.ID.String()) {
		t.Fatal("B spawned while queued")
	}
	if outB.NextStep == nil || outB.NextStep.Action != "fishhawk_await_stage" || !strings.Contains(outB.NextStep.Precondition, "QUEUED") {
		t.Fatalf("B next_step = %+v, want fishhawk_await_stage naming the queue", outB.NextStep)
	}
	if f.stageState(t, b) != "awaiting_host_dispatch" {
		t.Fatalf("B state = %s, want awaiting_host_dispatch while queued", f.stageState(t, b))
	}

	// get_run_status's projection carries B's queue block with a live waiter.
	stagesB, err := f.r.api.ListRunStages(f.ctx, b.RunID)
	if err != nil {
		t.Fatalf("ListRunStages(B): %v", err)
	}
	if ws := stageWaitStatusFor(stagesB, "implement", "running", 0, time.Now().UTC()); ws == nil || ws.Concurrency == nil ||
		ws.Concurrency.Status != concurrencyStatusAwaitingSlot || !ws.Concurrency.waiterLive() {
		t.Fatalf("B wait status = %+v, want the queued block with waiter_live", ws)
	}
	// fishhawk_await_stage holds through the queue: the wait envelope reads
	// settled awaiting_host_dispatch but is held for the slot.
	sw, err := f.r.api.GetRunStageWait(f.ctx, b.RunID, b.ID, 0)
	if err != nil || !stageWaitHeldForSlot(sw) {
		t.Fatalf("B wait envelope = %+v, %v; want held for the slot", sw, err)
	}

	// A settles: B's waiter is admitted and spawns B's fake runner.
	f.settle(t, a)
	slotWaitFor(t, "B's spawn after A settled", func() bool { return slotContains(f.rec.spawned(), b.ID.String()) })
	if got := f.stageState(t, b); got != "dispatched" {
		t.Fatalf("B state = %s, want dispatched", got)
	}

	// Spawn-failure arm: C queues behind B; its spawn fails at admission.
	c := f.park(t, nil)
	f.rec.mu.Lock()
	f.rec.errFor[c.ID.String()] = true
	f.rec.mu.Unlock()
	if out := f.dispatch(t, c); out.ConcurrencySlot == nil || out.ConcurrencySlot.Status != concurrencyStatusAwaitingSlot {
		t.Fatalf("C slot = %+v, want queued behind B", out.ConcurrencySlot)
	}
	f.settle(t, b)
	slotWaitFor(t, "C failed after its spawn failure", func() bool { return f.stageState(t, c) == "failed" })
	gotC, err := f.runRepo.GetStage(f.ctx, c.ID)
	if err != nil || gotC.FailureCategory == nil || string(*gotC.FailureCategory) != "C" {
		t.Fatalf("C = %+v, %v; want failed category C", gotC, err)
	}
	// C's slot was released: a fresh D is admitted at once.
	d := f.park(t, nil)
	outD := f.dispatch(t, d)
	if outD.ConcurrencySlot == nil || outD.ConcurrencySlot.Status == concurrencyStatusAwaitingSlot || !slotContains(f.rec.spawned(), d.ID.String()) {
		t.Fatalf("D slot = %+v, spawns %v; want an immediate admission and spawn", outD.ConcurrencySlot, f.rec.spawned())
	}
	slotWaitFor(t, "every waiter exited", func() bool {
		slotWaiters.mu.Lock()
		defer slotWaiters.mu.Unlock()
		return len(slotWaiters.active) == 0
	})
}

// TestConcurrencySlot_E2E_LostResponseOwnership pins approval condition 3
// against the REAL marker and slot store: waiter W's marker POST is lost (a
// transport error), the stage is admitted during the loss, and W's next POST
// answers 200 transitioned:false. Every arm spawns the stage EXACTLY once. W
// spawns only when the server recorded W's own nonce; another session on the
// SAME host label, or a direct fishhawk_dispatch_stage, is told apart by the
// recorded nonce although the host label is identical.
func TestConcurrencySlot_E2E_LostResponseOwnership(t *testing.T) {
	const wNonce = "nonce-waiter-w"
	cases := []struct {
		name string
		// send forwards W's lost request (the admission was W's own).
		send bool
		// during is the other actor inside the loss (nil: none).
		during      func(t *testing.T, f *e2eSlotFixture, st *runpkg.Stage)
		wantNonce   string
		wantSpawner string
	}{
		{name: "own lost admission spawns once", send: true, wantNonce: wNonce, wantSpawner: "W"},
		{name: "another session on the same host admits: W spawns nothing",
			during: func(t *testing.T, f *e2eSlotFixture, st *runpkg.Stage) {
				f.runWaiter(f.waiterSpec(st, f.session(), "nonce-session-2"))
			},
			wantNonce: "nonce-session-2", wantSpawner: "session 2"},
		{name: "a manual direct re-dispatch admits: W spawns nothing",
			during: func(t *testing.T, f *e2eSlotFixture, st *runpkg.Stage) {
				f.dispatch(t, st)
			},
			wantNonce: "", wantSpawner: "the direct dispatch"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newE2ESlotFixture(t)
			st := f.park(t, nil)
			lossy := &lossyMarker{slotMarker: f.r.api, loseAt: 1, send: tc.send}
			if tc.during != nil {
				lossy.during = func() { tc.during(t, f, st) }
			}
			f.runWaiter(f.waiterSpec(st, lossy, wNonce))

			if n := f.spawnCount(st); n != 1 {
				t.Fatalf("spawns of the stage = %d, want exactly 1 (by %s)", n, tc.wantSpawner)
			}
			sw, err := f.r.api.GetRunStageWait(f.ctx, st.RunID, st.ID, 0)
			if err != nil || sw.State != "dispatched" || sw.Concurrency == nil || sw.Concurrency.Status != concurrencyStatusHolding {
				t.Fatalf("stage = %+v, %v; want dispatched holding", sw, err)
			}
			if c := sw.Concurrency; c.Host != "e2e-host" || c.AdmissionNonce != tc.wantNonce {
				t.Fatalf("holding block host=%q nonce=%q, want host e2e-host (shared by every session) and nonce %q", c.Host, c.AdmissionNonce, tc.wantNonce)
			}
			if lossy.calls != 2 {
				t.Fatalf("W marker calls = %d, want 2 (the lost POST, then transitioned:false)", lossy.calls)
			}
		})
	}
}

// TestConcurrencySlot_E2E_TerminalRunStopsWaiter: B is queued behind A with a
// live waiter, then B's run goes terminal (cancelled, or separately failed).
// When A settles and the slot frees, the marker refuses B's waiter (409
// dispatch_not_admissible) instead of admitting a dead run's stage: B never
// spawns, stays awaiting_host_dispatch, and the waiter registry is cleared.
func TestConcurrencySlot_E2E_TerminalRunStopsWaiter(t *testing.T) {
	for _, terminal := range []runpkg.State{runpkg.StateCancelled, runpkg.StateFailed} {
		t.Run(string(terminal), func(t *testing.T) {
			f := newE2ESlotFixture(t)
			a, b := f.park(t, nil), f.park(t, nil)
			f.dispatch(t, a)
			if out := f.dispatch(t, b); out.ConcurrencySlot == nil || out.ConcurrencySlot.Waiter != slotWaiterStarted {
				t.Fatalf("B slot = %+v, want queued with a started waiter", out.ConcurrencySlot)
			}
			slotWaiters.mu.Lock()
			done, ok := slotWaiters.active[b.ID]
			slotWaiters.mu.Unlock()
			if !ok {
				t.Fatal("no live waiter registered for B")
			}
			if _, err := f.runRepo.TransitionRun(f.ctx, b.RunID, terminal); err != nil {
				t.Fatalf("transition B's run to %s: %v", terminal, err)
			}
			f.settle(t, a) // the slot frees: only the terminal-run refusal keeps B out
			waitDone(t, done)
			if n := f.spawnCount(b); n != 0 {
				t.Fatalf("B spawned %d time(s) for a %s run, want none", n, terminal)
			}
			if got := f.stageState(t, b); got != "awaiting_host_dispatch" {
				t.Fatalf("B state = %s, want awaiting_host_dispatch (never admitted)", got)
			}
			slotWaiters.mu.Lock()
			_, still := slotWaiters.active[b.ID]
			slotWaiters.mu.Unlock()
			if still {
				t.Fatal("B's waiter is still registered after it stopped")
			}
		})
	}
}

// admissionSeamStore runs hook INSIDE one in-flight admission: armed with a
// stage id, the FIRST Admit for exactly that stage (an atomic one-shot — no
// other stage's Admit, and no second Admit of the same stage, can trip it)
// runs hook before delegating to the real store. By then the handler has
// already read the run, so hook acts between the terminal-run pre-read and
// the admission CAS. Hook errors run on the HTTP handler goroutine, so they
// are recorded for the test goroutine rather than failing there.
type admissionSeamStore struct {
	concurrency.Store
	target atomic.Pointer[uuid.UUID]
	hook   func(ctx context.Context) error
	fired  atomic.Int32
	mu     sync.Mutex
	errs   []error
}

func (s *admissionSeamStore) Admit(ctx context.Context, req concurrency.Request) (concurrency.Admission, error) {
	if t := s.target.Load(); t != nil && req.StageID == *t && s.target.CompareAndSwap(t, nil) {
		s.fired.Add(1)
		if err := s.hook(ctx); err != nil {
			s.mu.Lock()
			s.errs = append(s.errs, err)
			s.mu.Unlock()
		}
	}
	return s.Store.Admit(ctx, req)
}

// TestConcurrencySlot_E2E_RunTerminalBetweenGuardAndAdmission reproduces the
// #4035 flake sequence deterministically: B is queued behind A with a live
// waiter; INSIDE B's next in-flight admission — after the handler's
// terminal-run pre-read saw B's run live — B's run goes terminal and A
// settles, freeing the slot. Only the admission's in-transaction run-liveness
// check can refuse B now: the waiter must stop on the 409 without spawning,
// and B must stay awaiting_host_dispatch.
func TestConcurrencySlot_E2E_RunTerminalBetweenGuardAndAdmission(t *testing.T) {
	for _, terminal := range []runpkg.State{runpkg.StateCancelled, runpkg.StateFailed} {
		t.Run(string(terminal), func(t *testing.T) {
			var seam *admissionSeamStore
			f := newE2ESlotFixtureWith(t, func(s concurrency.Store) concurrency.Store {
				seam = &admissionSeamStore{Store: s}
				return seam
			})
			a, b := f.park(t, nil), f.park(t, nil)
			f.dispatch(t, a)
			if out := f.dispatch(t, b); out.ConcurrencySlot == nil || out.ConcurrencySlot.Waiter != slotWaiterStarted {
				t.Fatalf("B slot = %+v, want queued with a started waiter", out.ConcurrencySlot)
			}
			slotWaiters.mu.Lock()
			done, ok := slotWaiters.active[b.ID]
			slotWaiters.mu.Unlock()
			if !ok {
				t.Fatal("no live waiter registered for B")
			}
			seam.hook = func(context.Context) error {
				if _, err := f.runRepo.TransitionRun(f.ctx, b.RunID, terminal); err != nil {
					return fmt.Errorf("transition B's run to %s: %w", terminal, err)
				}
				for _, to := range []runpkg.StageState{runpkg.StageStateRunning, runpkg.StageStateSucceeded} {
					if _, err := f.runRepo.TransitionStage(f.ctx, a.ID, to, nil); err != nil {
						return fmt.Errorf("settle A -> %s: %w", to, err)
					}
				}
				return nil
			}
			bID := b.ID
			seam.target.Store(&bID) // armed: B's waiter's next poll trips it
			waitDone(t, done)

			seam.mu.Lock()
			errs := seam.errs
			seam.mu.Unlock()
			for _, err := range errs {
				t.Errorf("seam hook: %v", err)
			}
			if n := seam.fired.Load(); n != 1 {
				t.Fatalf("seam fired %d time(s), want exactly 1 (the waiter must reach the admission)", n)
			}
			if n := f.spawnCount(b); n != 0 {
				t.Fatalf("B spawned %d time(s) for a %s run, want none", n, terminal)
			}
			if got := f.stageState(t, b); got != "awaiting_host_dispatch" {
				t.Fatalf("B state = %s, want awaiting_host_dispatch (never admitted)", got)
			}
			slotWaiters.mu.Lock()
			_, still := slotWaiters.active[b.ID]
			slotWaiters.mu.Unlock()
			if still {
				t.Fatal("B's waiter is still registered after it stopped")
			}
		})
	}
}

// TestConcurrencySlot_E2E_SpecLimitTwo: a workflow spec declaring implement
// concurrency {limit: 2} admits both dispatches at once.
func TestConcurrencySlot_E2E_SpecLimitTwo(t *testing.T) {
	spec := []byte(`version: "2"
workflows:
  feature_change:
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        concurrency:
          limit: 2
`)
	f := newE2ESlotFixture(t)
	a, b := f.park(t, spec), f.park(t, spec)
	for _, st := range []*runpkg.Stage{a, b} {
		out := f.dispatch(t, st)
		if out.ConcurrencySlot == nil || out.ConcurrencySlot.Status == concurrencyStatusAwaitingSlot || out.ConcurrencySlot.Limit != 2 {
			t.Fatalf("stage %s slot = %+v, want an admission at limit 2", st.ID, out.ConcurrencySlot)
		}
	}
	if got := f.rec.spawned(); len(got) != 2 {
		t.Fatalf("spawns = %v, want both fake runners spawned immediately", got)
	}
}
