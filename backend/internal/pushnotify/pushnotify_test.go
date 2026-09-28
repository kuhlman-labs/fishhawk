package pushnotify

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/timescale"
)

// ---- shared fakes (every fake guards its bookkeeping with a mutex: the
// dispatcher calls them from worker goroutines, #3226/#2586) ----

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

func captureLogger() (*slog.Logger, *syncBuffer) {
	buf := &syncBuffer{}
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})), buf
}

type outcomeRecorder struct {
	mu       sync.Mutex
	outcomes []Outcome
}

func (r *outcomeRecorder) record(ctx context.Context, o Outcome) {
	if ctx.Err() != nil {
		panic("outcome callback received a done context")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.outcomes = append(r.outcomes, o)
}

func (r *outcomeRecorder) all() []Outcome {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Outcome(nil), r.outcomes...)
}

func (r *outcomeRecorder) forSeq(seq int64) []Outcome {
	var out []Outcome
	for _, o := range r.all() {
		if o.Event.SourceSequence == seq {
			out = append(out, o)
		}
	}
	return out
}

type countingSink struct {
	name string
	mu   sync.Mutex
	seqs []int64
}

func (s *countingSink) Name() string { return s.name }

func (s *countingSink) Deliver(_ context.Context, e Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seqs = append(s.seqs, e.SourceSequence)
	return nil
}

func (s *countingSink) delivered() []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]int64(nil), s.seqs...)
}

type failingSink struct {
	name string
	err  error
}

func (s *failingSink) Name() string                         { return s.name }
func (s *failingSink) Deliver(context.Context, Event) error { return s.err }

// blockingSink ignores its context entirely and blocks on release, signalling
// entered once per call — the worst case a context timeout cannot cancel.
type blockingSink struct {
	name    string
	entered chan int64
	release chan struct{}
}

func newBlockingSink(t *testing.T, name string) *blockingSink {
	s := &blockingSink{name: name, entered: make(chan int64, 64), release: make(chan struct{})}
	t.Cleanup(func() { close(s.release) })
	return s
}

func (s *blockingSink) Name() string { return s.name }

func (s *blockingSink) Deliver(_ context.Context, e Event) error {
	s.entered <- e.SourceSequence
	<-s.release
	return nil
}

type panicSink struct{}

func (panicSink) Name() string                         { return "boom" }
func (panicSink) Deliver(context.Context, Event) error { panic("secret-ish panic value PANICCANARY") }

func ev(seq int64) Event {
	return Event{SchemaVersion: SchemaVersion, Event: "plan_awaiting_approval", SourceSequence: seq, RunID: "run-1", Repo: "o/r"}
}

func drain(t *testing.T, d *Dispatcher) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timescale.D(3*time.Second))
	defer cancel()
	if err := d.Drain(ctx); err != nil {
		t.Fatalf("Drain did not complete: %v", err)
	}
}

// closeOnCleanup closes d at test end under a bounded deadline, so a
// counterfactual that pins a worker fails fast instead of hanging cleanup.
func closeOnCleanup(t *testing.T, d *Dispatcher) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), timescale.D(time.Second))
		defer cancel()
		_ = d.Close(ctx)
	})
}

// withRegistered registers a test factory and removes it on cleanup.
func withRegistered(t *testing.T, kind string, f Factory) {
	t.Helper()
	Register(kind, f)
	t.Cleanup(func() {
		registryMu.Lock()
		delete(registry, kind)
		registryMu.Unlock()
	})
}

// ---- registry ----

func TestRegistry_BuiltinSinksSelfRegister(t *testing.T) {
	got := Registered()
	for _, want := range []string{WebhookKind, SlackKind} {
		found := false
		for _, k := range got {
			found = found || k == want
		}
		if !found {
			t.Errorf("Registered() = %v, missing %q", got, want)
		}
	}
}

func TestRegister_RejectsDuplicateAndEmpty(t *testing.T) {
	for name, fn := range map[string]func(){
		"duplicate": func() { Register(WebhookKind, webhookFromEnv) },
		"empty":     func() { Register("", webhookFromEnv) },
		"nil":       func() { Register("zz-nil", nil) },
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("Register did not panic")
				}
			}()
			fn()
		})
	}
}

func TestSinksFromEnv_EmptyEnvYieldsNoSinks(t *testing.T) {
	sinks, err := SinksFromEnv(func(string) string { return "" })
	if err != nil || len(sinks) != 0 {
		t.Fatalf("SinksFromEnv(empty) = %v, %v; want no sinks, nil", sinks, err)
	}
}

// W5 (generalized — the email sink is a sibling slice): a factory reporting a
// half-configuration makes SinksFromEnv return a NAMED error and NO sinks,
// even though another sink is fully configured.
func TestSinksFromEnv_HalfConfiguredSinkFailsClosed(t *testing.T) {
	withRegistered(t, "zz-half", func(getenv func(string) string) (Sink, error) {
		if getenv("ZZ_ADDR") != "" && getenv("ZZ_FROM") == "" {
			return nil, errors.New("ZZ_FROM: required when ZZ_ADDR is set")
		}
		if getenv("ZZ_ADDR") == "" {
			return nil, nil
		}
		return &countingSink{name: "zz-half"}, nil
	})
	env := map[string]string{
		EnvWebhookURL: "https://hooks.example.com/x", EnvWebhookSecret: "s",
		"ZZ_ADDR": "smtp.example.com:587",
	}
	sinks, err := SinksFromEnv(func(k string) string { return env[k] })
	if err == nil || !strings.Contains(err.Error(), "ZZ_FROM") || !strings.Contains(err.Error(), `"zz-half"`) {
		t.Fatalf("err = %v; want a named ZZ_FROM error for zz-half", err)
	}
	if sinks != nil {
		t.Fatalf("sinks = %v; want none on a configuration error", sinks)
	}

	env["ZZ_FROM"] = "fishhawk@example.com"
	sinks, err = SinksFromEnv(func(k string) string { return env[k] })
	if err != nil {
		t.Fatalf("fully configured: %v", err)
	}
	var names []string
	for _, s := range sinks {
		names = append(names, s.Name())
	}
	if !reflect.DeepEqual(names, []string{WebhookKind, "zz-half"}) {
		t.Fatalf("sink order = %v; want sorted [webhook zz-half]", names)
	}
}

// ---- L4: SanitizeTransportError ----

const canary = "LEAKCANARY0001"

func TestSanitizeTransportError_KeepsHostDropsCredentials(t *testing.T) {
	leakyURL := "https://user:" + canary + "@hooks.example.com:8443/services/" + canary + "?token=" + canary
	refused := &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}
	cases := []struct {
		name       string
		err        error
		wantHost   string
		wantReason string
	}{
		{"url refused", &url.Error{Op: "Post", URL: leakyURL, Err: refused}, "hooks.example.com", ReasonConnectionRefused},
		{"url dns", &url.Error{Op: "Post", URL: leakyURL, Err: &net.DNSError{Err: "no such host " + canary, Name: "hooks.example.com"}}, "hooks.example.com", ReasonDNS},
		{"url tls", &url.Error{Op: "Post", URL: leakyURL, Err: x509.UnknownAuthorityError{}}, "hooks.example.com", ReasonTLS},
		{"url timeout", &url.Error{Op: "Post", URL: leakyURL, Err: context.DeadlineExceeded}, "hooks.example.com", ReasonTimeout},
		{"url reset", &url.Error{Op: "Post", URL: leakyURL, Err: os.NewSyscallError("read", syscall.ECONNRESET)}, "hooks.example.com", ReasonConnectionReset},
		{"url canceled", &url.Error{Op: "Post", URL: leakyURL, Err: context.Canceled}, "hooks.example.com", ReasonCanceled},
		{"unparseable url", &url.Error{Op: "Post", URL: "::" + canary, Err: errors.New(canary)}, "", ReasonFailed},
		{"status at host", AtHost("hooks.example.com", &HTTPStatusError{StatusCode: 503}), "hooks.example.com", "http status 503"},
		{"raw text", fmt.Errorf("smtp: 535 auth failed for password %s", canary), "", ReasonFailed},
		{"raw text at host", AtHost("smtp.example.com", fmt.Errorf("535 %s", canary)), "smtp.example.com", ReasonFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := SanitizeTransportError("slack", tc.err)
			var de *DeliveryError
			if !errors.As(got, &de) {
				t.Fatalf("got %T, want *DeliveryError", got)
			}
			if de.Host != tc.wantHost || de.Reason != tc.wantReason || de.Sink != "slack" {
				t.Fatalf("got %+v; want host %q reason %q", de, tc.wantHost, tc.wantReason)
			}
			for _, s := range []string{got.Error(), fmt.Sprintf("%+v", got), fmt.Sprintf("%#v", got)} {
				if strings.Contains(s, canary) {
					t.Fatalf("sanitized error leaks the canary: %s", s)
				}
				if !strings.Contains(s, "slack") {
					t.Fatalf("sanitized error does not name the sink: %s", s)
				}
			}
			if errors.Unwrap(got) != nil {
				t.Fatal("sanitized error retains a cause")
			}
		})
	}
	if SanitizeTransportError("x", nil) != nil {
		t.Fatal("nil in must be nil out")
	}
	pre := &DeliveryError{Sink: "a", Host: "h", Reason: ReasonTLS}
	if got := SanitizeTransportError("b", pre); got != pre {
		t.Fatalf("an already-sanitized error must pass through unchanged, got %v", got)
	}
}

// ---- Dispatcher ----

// A3: a Deliver that IGNORES ctx cannot wedge the caller or the worker: the
// enqueue returns at once, and the worker abandons the delivery at the
// per-sink timeout (approval condition 1), so Drain completes while the sink
// is still blocked.
func TestDispatcher_IgnoredContextCannotWedgeCaller(t *testing.T) {
	sink := newBlockingSink(t, "stuck")
	rec := &outcomeRecorder{}
	log, logs := captureLogger()
	d := NewDispatcher([]Sink{sink}, Options{Workers: 1, SinkTimeout: timescale.D(50 * time.Millisecond), OnOutcome: rec.record, Logger: log})
	closeOnCleanup(t, d)

	start := time.Now()
	if !d.Enqueue(ev(7)) {
		t.Fatal("Enqueue refused")
	}
	if el := time.Since(start); el > timescale.D(100*time.Millisecond) {
		t.Fatalf("Enqueue took %v; it must not block", el)
	}
	<-sink.entered
	drain(t, d) // RED if the worker called Deliver inline
	outs := rec.forSeq(7)
	if len(outs) != 1 || !outs[0].TimedOut {
		t.Fatalf("outcomes = %+v; want one timed-out outcome", outs)
	}
	var de *DeliveryError
	if !errors.As(outs[0].Err, &de) || !de.Timeout() {
		t.Fatalf("outcome err = %v; want a timeout DeliveryError", outs[0].Err)
	}
	if d.Abandoned() != 1 {
		t.Fatalf("Abandoned() = %d, want 1", d.Abandoned())
	}
	if !strings.Contains(logs.String(), "abandoned_deliveries=1") {
		t.Fatalf("WARN log lacks the abandoned gauge: %s", logs.String())
	}
}

// ctxSink blocks the event with blockSeq until its ctx is done (it honours
// ctx) and records every other event.
type ctxSink struct {
	countingSink
	blockSeq int64
}

func (s *ctxSink) Deliver(ctx context.Context, e Event) error {
	if e.SourceSequence == s.blockSeq {
		<-ctx.Done()
		return ctx.Err()
	}
	return s.countingSink.Deliver(ctx, e)
}

// A5: the per-sink timeout bounds the worker, so a later Event on the SAME
// single worker is still delivered.
func TestDispatcher_PerSinkTimeoutBoundsWorker(t *testing.T) {
	sink := &ctxSink{countingSink: countingSink{name: "slow"}, blockSeq: 1}
	rec := &outcomeRecorder{}
	d := NewDispatcher([]Sink{sink}, Options{Workers: 1, SinkTimeout: timescale.D(50 * time.Millisecond), OnOutcome: rec.record})
	closeOnCleanup(t, d)

	d.Enqueue(ev(1))
	d.Enqueue(ev(2))
	drain(t, d)
	if got := sink.delivered(); !reflect.DeepEqual(got, []int64{2}) {
		t.Fatalf("delivered = %v; want the sibling event 2 delivered", got)
	}
	outs := rec.forSeq(1)
	var de *DeliveryError
	if len(outs) != 1 || !errors.As(outs[0].Err, &de) || !de.Timeout() {
		t.Fatalf("event 1 outcome = %+v; want a timeout", outs)
	}
}

// A6: a failing sink does not suppress a sibling sink registered after it.
func TestDispatcher_FailingSinkDoesNotSuppressSiblings(t *testing.T) {
	counting := &countingSink{name: "counting"}
	rec := &outcomeRecorder{}
	d := NewDispatcher([]Sink{&failingSink{name: "bad", err: errors.New("nope")}, counting}, Options{OnOutcome: rec.record})
	closeOnCleanup(t, d)
	d.Enqueue(ev(3))
	drain(t, d)
	if got := counting.delivered(); !reflect.DeepEqual(got, []int64{3}) {
		t.Fatalf("counting sink delivered %v; want [3]", got)
	}
	outs := rec.forSeq(3)
	if len(outs) != 2 || outs[0].Sink != "bad" || outs[0].Err == nil || outs[1].Sink != "counting" || outs[1].Err != nil {
		t.Fatalf("outcomes = %+v; want bad=failed, counting=ok", outs)
	}
}

// A4: with the worker pinned and the queue full, Enqueue returns false
// without blocking and logs a WARN.
func TestDispatcher_FullQueueDropsWithoutBlocking(t *testing.T) {
	sink := newBlockingSink(t, "stuck")
	log, logs := captureLogger()
	d := NewDispatcher([]Sink{sink}, Options{QueueSize: 1, Workers: 1, SinkTimeout: timescale.D(30 * time.Second), Logger: log})

	if !d.Enqueue(ev(1)) {
		t.Fatal("first Enqueue refused")
	}
	<-sink.entered // the only worker is now pinned on event 1
	if !d.Enqueue(ev(2)) {
		t.Fatal("second Enqueue refused; the queue (size 1) should hold it")
	}
	res := make(chan bool, 1)
	go func() { res <- d.Enqueue(ev(3)) }()
	select {
	case ok := <-res:
		if ok {
			t.Fatal("Enqueue into a full queue returned true")
		}
	case <-time.After(timescale.D(time.Second)):
		t.Fatal("Enqueue blocked on a full queue")
	}
	if !strings.Contains(logs.String(), "queue full") || !strings.Contains(logs.String(), "source_sequence=3") {
		t.Fatalf("missing queue-full WARN: %s", logs.String())
	}
}

func TestDispatcher_EnqueueAfterCloseRefused(t *testing.T) {
	counting := &countingSink{name: "c"}
	d := NewDispatcher([]Sink{counting}, Options{})
	d.Enqueue(ev(1))
	ctx, cancel := context.WithTimeout(context.Background(), timescale.D(3*time.Second))
	defer cancel()
	if err := d.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := counting.delivered(); !reflect.DeepEqual(got, []int64{1}) {
		t.Fatalf("Close did not drain the queue: delivered %v", got)
	}
	if d.Enqueue(ev(2)) {
		t.Fatal("Enqueue after Close returned true")
	}
	if err := d.Close(ctx); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// Close is bounded by ONE overall deadline and never waits on an abandoned
// Deliver.
func TestDispatcher_CloseBoundedByDeadline(t *testing.T) {
	sink := newBlockingSink(t, "stuck")
	d := NewDispatcher([]Sink{sink}, Options{Workers: 1, SinkTimeout: timescale.D(30 * time.Second)})
	d.Enqueue(ev(1))
	<-sink.entered
	ctx, cancel := context.WithTimeout(context.Background(), timescale.D(50*time.Millisecond))
	defer cancel()
	start := time.Now()
	if err := d.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close = %v; want DeadlineExceeded", err)
	}
	if el := time.Since(start); el > timescale.D(time.Second) {
		t.Fatalf("Close took %v past its deadline", el)
	}

	// And with a short per-sink timeout, Close completes without waiting on
	// the abandoned Deliver.
	sink2 := newBlockingSink(t, "stuck2")
	d2 := NewDispatcher([]Sink{sink2}, Options{Workers: 1, SinkTimeout: timescale.D(30 * time.Millisecond)})
	d2.Enqueue(ev(2))
	<-sink2.entered
	ctx2, cancel2 := context.WithTimeout(context.Background(), timescale.D(3*time.Second))
	defer cancel2()
	if err := d2.Close(ctx2); err != nil {
		t.Fatalf("Close with an abandoned Deliver: %v", err)
	}
}

func TestDispatcher_DrainBoundedByContext(t *testing.T) {
	sink := newBlockingSink(t, "stuck")
	d := NewDispatcher([]Sink{sink}, Options{Workers: 1, SinkTimeout: timescale.D(30 * time.Second)})
	d.Enqueue(ev(1))
	ctx, cancel := context.WithTimeout(context.Background(), timescale.D(30*time.Millisecond))
	defer cancel()
	if err := d.Drain(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Drain = %v; want DeadlineExceeded while a delivery is in flight", err)
	}
}

func TestDispatcher_NilAndEmptyAreNoOps(t *testing.T) {
	var nilD *Dispatcher
	empty := NewDispatcher(nil, Options{})
	for name, d := range map[string]*Dispatcher{"nil": nilD, "empty": empty} {
		t.Run(name, func(t *testing.T) {
			if !d.Enqueue(ev(1)) {
				t.Fatal("no-op Enqueue must accept (nothing to drop)")
			}
			if err := d.Drain(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := d.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
			if names := d.SinkNames(); len(names) != 0 || names == nil {
				t.Fatalf("SinkNames = %#v; want empty non-nil", names)
			}
			if d.Abandoned() != 0 {
				t.Fatal("Abandoned != 0")
			}
		})
	}
}

func TestDispatcher_SinkNamesInConfigOrder(t *testing.T) {
	d := NewDispatcher([]Sink{&countingSink{name: "webhook"}, &countingSink{name: "email"}}, Options{})
	closeOnCleanup(t, d)
	if got := d.SinkNames(); !reflect.DeepEqual(got, []string{"webhook", "email"}) {
		t.Fatalf("SinkNames = %v", got)
	}
}

// A sink that returns a raw, unsanitized error still cannot leak its text
// past the dispatcher: the outcome and the log carry only the sanitized form.
func TestDispatcher_SanitizesUnsanitizedSinkError(t *testing.T) {
	rec := &outcomeRecorder{}
	log, logs := captureLogger()
	d := NewDispatcher([]Sink{&failingSink{name: "raw", err: errors.New("dial https://u:" + canary + "@h/x failed")}}, Options{OnOutcome: rec.record, Logger: log})
	closeOnCleanup(t, d)
	d.Enqueue(ev(4))
	drain(t, d)
	outs := rec.forSeq(4)
	if len(outs) != 1 || outs[0].Err == nil {
		t.Fatalf("outcomes = %+v", outs)
	}
	if strings.Contains(outs[0].Err.Error(), canary) || strings.Contains(logs.String(), canary) {
		t.Fatalf("raw sink error leaked: outcome %q log %q", outs[0].Err, logs.String())
	}
	if !strings.Contains(logs.String(), "sink=raw") {
		t.Fatalf("log does not name the sink (vacuous): %s", logs.String())
	}
}

func TestDispatcher_PanickingSinkIsIsolated(t *testing.T) {
	counting := &countingSink{name: "counting"}
	rec := &outcomeRecorder{}
	log, logs := captureLogger()
	d := NewDispatcher([]Sink{panicSink{}, counting}, Options{Workers: 1, OnOutcome: rec.record, Logger: log})
	closeOnCleanup(t, d)
	d.Enqueue(ev(5))
	drain(t, d)
	if got := counting.delivered(); !reflect.DeepEqual(got, []int64{5}) {
		t.Fatalf("sibling after a panicking sink delivered %v", got)
	}
	outs := rec.forSeq(5)
	var de *DeliveryError
	if len(outs) != 2 || !errors.As(outs[0].Err, &de) || de.Reason != ReasonPanic {
		t.Fatalf("outcomes = %+v; want a panic outcome first", outs)
	}
	if strings.Contains(logs.String(), "PANICCANARY") {
		t.Fatal("panic value leaked to the log")
	}
}

func TestDispatcher_OutcomeCallbackPanicDoesNotKillWorker(t *testing.T) {
	counting := &countingSink{name: "c"}
	calls := 0
	var mu sync.Mutex
	d := NewDispatcher([]Sink{counting}, Options{Workers: 1, OnOutcome: func(context.Context, Outcome) {
		mu.Lock()
		calls++
		mu.Unlock()
		panic("callback")
	}})
	closeOnCleanup(t, d)
	d.Enqueue(ev(1))
	d.Enqueue(ev(2))
	drain(t, d)
	if got := counting.delivered(); !reflect.DeepEqual(got, []int64{1, 2}) {
		t.Fatalf("worker died after a callback panic: delivered %v", got)
	}
}

func TestMarshalEvent_NormalizesNilSlices(t *testing.T) {
	b, err := MarshalEvent(Event{RunID: "r"})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if v, ok := m["verdicts"].([]any); !ok || len(v) != 0 {
		t.Fatalf("verdicts = %#v; want []", m["verdicts"])
	}
	gl := m["gate_latency"].(map[string]any)
	if g, ok := gl["gates"].([]any); !ok || len(g) != 0 {
		t.Fatalf("gates = %#v; want []", gl["gates"])
	}
	if _, ok := m["issue"]; ok {
		t.Fatal("issue must be omitted when nil")
	}
}
