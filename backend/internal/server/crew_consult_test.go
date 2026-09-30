package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/crewmessage"
	"github.com/kuhlman-labs/fishhawk/backend/internal/timescale"
)

// fakeResponder is a deterministic CrewResponder. When gate is non-nil it
// blocks until gate closes (or ctx ends); err makes it fail.
type fakeResponder struct {
	answer CrewConsultAnswer
	err    error
	gate   chan struct{}
	seen   chan CrewConsultRequest
}

func (f *fakeResponder) Respond(ctx context.Context, req CrewConsultRequest) (CrewConsultAnswer, error) {
	if f.seen != nil {
		select {
		case f.seen <- req:
		default:
		}
	}
	if f.gate != nil {
		select {
		case <-f.gate:
		case <-ctx.Done():
			// Deliberately keep blocking past ctx: the dispatcher must not
			// rely on the responder honouring its deadline.
			<-f.gate
		}
	}
	return f.answer, f.err
}

// (C3) the registry refuses an implementer registration at construction.
func TestCrewResponderRegistry_RefusesImplementer(t *testing.T) {
	_, err := NewCrewResponderRegistry(map[crewmessage.Role]CrewResponder{
		crewmessage.RoleHistorian:   &fakeResponder{},
		crewmessage.RoleImplementer: &fakeResponder{},
	})
	if err == nil || !strings.Contains(err.Error(), "implement stage") {
		t.Fatalf("err = %v, want an implementer refusal", err)
	}
}

func TestCrewResponderRegistry_RefusesUnknownRoleAndNilResponder(t *testing.T) {
	if _, err := NewCrewResponderRegistry(map[crewmessage.Role]CrewResponder{"claude-opus-5": &fakeResponder{}}); err == nil ||
		!strings.Contains(err.Error(), "not a crew role") {
		t.Fatalf("unknown role err = %v, want not-a-crew-role", err)
	}
	if _, err := NewCrewResponderRegistry(map[crewmessage.Role]CrewResponder{crewmessage.RoleHistorian: nil}); err == nil ||
		!strings.Contains(err.Error(), "nil responder") {
		t.Fatalf("nil responder err = %v, want nil-responder refusal", err)
	}
}

func TestCrewResponderRegistry_LookupIsClosed(t *testing.T) {
	h := &fakeResponder{}
	in := map[crewmessage.Role]CrewResponder{crewmessage.RoleHistorian: h}
	reg, err := NewCrewResponderRegistry(in)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	// A caller mutation after construction cannot widen the registry.
	in[crewmessage.RoleSecurity] = &fakeResponder{}
	if got, ok := reg.Lookup(crewmessage.RoleHistorian); !ok || got != h {
		t.Fatalf("historian lookup = %v, %v", got, ok)
	}
	if _, ok := reg.Lookup(crewmessage.RoleSecurity); ok {
		t.Fatal("security resolved after a post-construction mutation of the input map")
	}
	var zero CrewResponderRegistry
	if _, ok := zero.Lookup(crewmessage.RoleHistorian); ok {
		t.Fatal("the zero-value (production) registry resolved a responder")
	}
	empty, err := NewCrewResponderRegistry(nil)
	if err != nil {
		t.Fatalf("empty registry: %v", err)
	}
	if _, ok := empty.Lookup(crewmessage.RoleHistorian); ok {
		t.Fatal("the empty registry resolved a responder")
	}
}

// A sender may ask for LESS time than the window, never more.
func TestCrewConsultDeadline(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	window := now.Add(crewConsultWindow)
	for name, tc := range map[string]struct {
		deadline string
		want     time.Time
	}{
		"absent":          {"", window},
		"shorter":         {"2026-09-30T12:05:00Z", now.Add(5 * time.Minute)},
		"shorter_lower_z": {"2026-09-30t12:05:00z", now.Add(5 * time.Minute)},
		"longer_clamped":  {"2026-09-30T13:00:00Z", window},
		"unparseable":     {"not-a-date", window},
	} {
		t.Run(name, func(t *testing.T) {
			got := crewConsultDeadline(&crewmessage.Message{Deadline: tc.deadline}, now)
			if !got.Equal(tc.want) {
				t.Fatalf("deadline = %v, want %v", got, tc.want)
			}
		})
	}
}

// --- E77.14 / #3876: the consult dispatcher's concurrency + fault scaffolding.

// syncBuffer is a mutex-guarded io.Writer over a bytes.Buffer. The background
// consult goroutines write the slog handler while the test reads it, so the
// bookkeeping a fake reachable from a concurrent product path owns must be
// guarded (#3226) — an unguarded buffer makes a -race RED attributable to the
// FAKE rather than to the control under test.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// faultSink is the fault-injecting crewConsultSink: it records every call
// under a mutex and returns the injected results, so the two branches no real
// mailbox reaches on demand become drivable. Counters are mutex-guarded
// because the consult goroutines call it concurrently with the test's reads
// (#3226).
type faultSink struct {
	mu sync.Mutex

	// respondReply / respondAnswered / respondErr are what Respond returns.
	// A NON-NIL respondReply with a NON-NIL respondErr is the
	// answered-but-not-disposed pairing.
	respondReply    *crewmessage.Row
	respondAnswered *crewmessage.Row
	respondErr      error
	// disposeErr is what Dispose returns.
	disposeErr error

	respondCalls  int
	disposeCalls  int
	lastDisposeIn crewmessage.DisposeParams
}

func (k *faultSink) Respond(ctx context.Context, p RespondParams) (*crewmessage.Row, *crewmessage.Row, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.respondCalls++
	return k.respondReply, k.respondAnswered, k.respondErr
}

func (k *faultSink) Dispose(ctx context.Context, p crewmessage.DisposeParams) (*crewmessage.Row, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.disposeCalls++
	k.lastDisposeIn = p
	return nil, k.disposeErr
}

func (k *faultSink) counts() (respond, dispose int) {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.respondCalls, k.disposeCalls
}

func (k *faultSink) disposeParams() crewmessage.DisposeParams {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.lastDisposeIn
}

// newConsultServer builds a Server whose logging is captured and whose consult
// sink is sink. NO database is needed: the seam intercepts every mailbox
// interaction the dispatcher performs. server_test.go carries no reusable
// Server-construction or Shutdown-drain helper to reuse (it calls New(Config{})
// inline at each site and asserts Shutdown directly), so this helper collides
// with nothing there.
func newConsultServer(t *testing.T, sink crewConsultSink) (*Server, *syncBuffer) {
	t.Helper()
	buf := &syncBuffer{}
	s := New(Config{Logger: slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))})
	s.crewConsultSinkOverride = sink
	return s, buf
}

// consultReq is a minimal CrewConsultRequest for the hermetic arms.
func consultReq() CrewConsultRequest {
	return CrewConsultRequest{
		SentSequence:  7,
		RunID:         uuid.New(),
		StageID:       uuid.New(),
		SenderRole:    crewmessage.RolePlanner,
		RecipientRole: crewmessage.RoleHistorian,
		Question:      "did we decide this before?",
	}
}

// expiredCtx is a context whose deadline is already in the past, so
// runCrewConsult's outer select can only take the ctx.Done() branch.
func expiredCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	t.Cleanup(cancel)
	return ctx
}

// The counterfactual vehicle for the inner-goroutine tracking control: the
// responder is PROVABLY still inside Respond (it has signalled `seen` and is
// blocked on an unclosed gate) when runCrewConsult has already returned
// through its deadline branch. A TRACKED inner goroutine keeps s.bgReviews
// non-zero across that window; an untracked one does not. Deleting the inner
// s.bgReviews.Add(1)/Done() makes the drain complete during the settle window.
func TestCrewConsult_InnerResponderGoroutineTracked(t *testing.T) {
	sink := &faultSink{}
	s, _ := newConsultServer(t, sink)
	gate := make(chan struct{})
	seen := make(chan CrewConsultRequest, 1)
	resp := &fakeResponder{gate: gate, seen: seen}

	// runCrewConsult is driven DIRECTLY (not through dispatchCrewConsult), so
	// the inner responder goroutine is the ONLY thing bgReviews can hold.
	s.runCrewConsult(expiredCtx(t), resp, consultReq())

	// The responder is inside Respond ...
	select {
	case <-seen:
	case <-time.After(timescale.D(5 * time.Second)):
		t.Fatal("responder never entered Respond")
	}
	// ... and the outer path has already closed the consult.
	if _, dispose := sink.counts(); dispose != 1 {
		t.Fatalf("dispose calls = %d after the deadline branch, want 1", dispose)
	}

	drained := make(chan struct{})
	go func() {
		s.waitBackgroundReviews()
		close(drained)
	}()
	select {
	case <-drained:
		t.Fatal("drain completed while the responder was still running: the inner responder goroutine is not tracked by s.bgReviews")
	case <-time.After(timescale.D(300 * time.Millisecond)):
	}

	close(gate)
	select {
	case <-drained:
	case <-time.After(timescale.D(5 * time.Second)):
		t.Fatal("drain did not complete after the responder returned")
	}
}

// A RECORDED reply paired with a FAILING disposition is answered, not expired:
// the sender's wait already sees the threaded reply, so the dispatcher logs
// rather than contradicting it. Deleting the `if reply != nil { log; return }`
// block falls through to expireCrewConsult, which disposes once.
func TestCrewConsult_AnsweredButNotDisposed_DoesNotExpire(t *testing.T) {
	sink := &faultSink{
		respondReply: &crewmessage.Row{SentSequence: 8, State: crewmessage.StateOpen},
		respondErr:   errors.New("dispose accepted failed: connection reset"),
	}
	s, buf := newConsultServer(t, sink)
	req := consultReq()
	s.runCrewConsult(context.Background(), &fakeResponder{answer: CrewConsultAnswer{Summary: "we decided in ADR-081"}}, req)

	respond, dispose := sink.counts()
	if respond != 1 {
		t.Fatalf("respond calls = %d, want 1", respond)
	}
	if dispose != 0 {
		t.Fatalf("dispose calls = %d, want 0 — an answered consult must not be expired", dispose)
	}
	logged := buf.String()
	if !strings.Contains(logged, "crew consult answered but not disposed") {
		t.Fatalf("missing the answered-but-not-disposed line: %s", logged)
	}
	for _, want := range []string{req.RunID.String(), `"sent_sequence":7`, `"recipient_role":"historian"`} {
		if !strings.Contains(logged, want) {
			t.Errorf("log line missing %q: %s", want, logged)
		}
	}
	if strings.Contains(logged, "crew consult expired without an answer") {
		t.Errorf("an answered consult logged an expiry: %s", logged)
	}
}

// The negative arm proving the zero-dispose assertion above can observe a
// dispose at all: a nil reply with an error DOES expire, exactly once.
func TestCrewConsult_RespondFailedNoReply_Expires(t *testing.T) {
	sink := &faultSink{respondErr: errors.New("chain append failed")}
	s, buf := newConsultServer(t, sink)
	s.runCrewConsult(context.Background(), &fakeResponder{answer: CrewConsultAnswer{Summary: "an answer"}}, consultReq())

	if _, dispose := sink.counts(); dispose != 1 {
		t.Fatalf("dispose calls = %d, want exactly 1", dispose)
	}
	p := sink.disposeParams()
	if p.Disposition != crewmessage.DispositionExpired {
		t.Fatalf("disposition = %q, want expired", p.Disposition)
	}
	if !strings.Contains(p.Reason, "could not be recorded") || !strings.Contains(p.Reason, "chain append failed") {
		t.Fatalf("reason = %q, want the could-not-be-recorded cause", p.Reason)
	}
	if !strings.Contains(buf.String(), "crew consult expired without an answer") {
		t.Fatalf("missing the expiry WARN: %s", buf.String())
	}
}

// One behavioral row per NAMED tolerance mode of expireCrewConsult. Row
// "unrelated" is the ANTI-VACUITY control: it proves the absence assertions in
// the other three are checking a line this fixture can actually observe. Every
// row also asserts the expiry WARN IS present, so no row can pass because
// expireCrewConsult was never reached.
func TestCrewConsult_ExpiryTolerance(t *testing.T) {
	const notRecorded = "crew consult expiry could not be recorded"
	for name, tc := range map[string]struct {
		disposeErr error
		wantLine   bool
		wantText   string
	}{
		"already_disposed_tolerated": {fmt.Errorf("crewmessage: sequence %d: %w", 7, crewmessage.ErrAlreadyDisposed), false, ""},
		"projection_error_tolerated": {&crewmessage.ProjectionError{Sequence: 7, Err: errors.New("projection lag")}, false, ""},
		"nil_is_silent":              {nil, false, ""},
		"unrelated_error_logged":     {errors.New("dispose: connection reset by peer"), true, "connection reset by peer"},
	} {
		t.Run(name, func(t *testing.T) {
			sink := &faultSink{disposeErr: tc.disposeErr}
			s, buf := newConsultServer(t, sink)
			gate := make(chan struct{})
			seen := make(chan CrewConsultRequest, 1)
			s.runCrewConsult(expiredCtx(t), &fakeResponder{gate: gate, seen: seen}, consultReq())
			t.Cleanup(func() { close(gate); s.waitBackgroundReviews() })

			if _, dispose := sink.counts(); dispose != 1 {
				t.Fatalf("dispose calls = %d, want 1", dispose)
			}
			logged := buf.String()
			// The masking guard: every row must have REACHED expireCrewConsult.
			if !strings.Contains(logged, "crew consult expired without an answer") {
				t.Fatalf("expireCrewConsult was never reached: %s", logged)
			}
			if got := strings.Contains(logged, notRecorded); got != tc.wantLine {
				t.Fatalf("%q present = %v, want %v; log: %s", notRecorded, got, tc.wantLine, logged)
			}
			if tc.wantText != "" && !strings.Contains(logged, tc.wantText) {
				t.Fatalf("log missing the error text %q: %s", tc.wantText, logged)
			}
		})
	}
}
