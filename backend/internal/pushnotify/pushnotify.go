// Package pushnotify is Fishhawk's outbound push-notification seam (#2292):
// the one-screen decision payload (Event), the Sink interface every
// destination implements, a self-registering sink registry configured from
// the environment, the single credential-scrubbing error helper, and the
// asynchronous Dispatcher that delivers Events off the request path.
//
// The package has no Fishhawk-internal consumers of its own: the caller
// (issuecomment's push channel) decides WHEN an Event exists and hands it to
// Dispatcher.Enqueue, which never blocks and never touches the network.
// Long-form contract: README.md in this directory.
package pushnotify

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// SchemaVersion is the Event payload's schema version. Additive fields keep
// the version; a removed or re-typed field bumps it (docs/notifications.md).
const SchemaVersion = 1

// Event is the one-screen decision payload a sink delivers: enough context
// for an operator to decide whether to act without opening the issue thread.
type Event struct {
	SchemaVersion int `json:"schema_version"`
	// Event is the page-class kind token (plan_awaiting_approval,
	// implement_review_rejected, scope_amendment, ...).
	Event string `json:"event"`
	// SourceSequence is the originating audit entry's sequence number. It is
	// the dedup key and, with RunID, the X-Fishhawk-Delivery identifier.
	SourceSequence int64       `json:"source_sequence"`
	RunID          string      `json:"run_id"`
	RunShortID     string      `json:"run_short_id"`
	Repo           string      `json:"repo"`
	Issue          *Issue      `json:"issue,omitempty"`
	WorkflowID     string      `json:"workflow_id,omitempty"`
	Stage          *Stage      `json:"stage,omitempty"`
	Decision       string      `json:"decision"`
	Verdicts       []Verdict   `json:"verdicts"`
	GateLatency    GateLatency `json:"gate_latency"`
	Links          Links       `json:"links"`
	OccurredAt     time.Time   `json:"occurred_at"`
}

// Issue identifies the originating issue of an issue-anchored run. It is
// omitted from the payload for a CLI- or PR-triggered run.
type Issue struct {
	Number int    `json:"number"`
	URL    string `json:"url,omitempty"`
}

// Stage names the stage the decision concerns.
type Stage struct {
	Type  string `json:"type"`
	State string `json:"state,omitempty"`
}

// Verdict is one reviewer's verdict for a review-derived event.
type Verdict struct {
	ReviewerModel string `json:"reviewer_model"`
	Verdict       string `json:"verdict"`
}

// GateLatency is the accumulated wait-on-human context for the run.
type GateLatency struct {
	TotalWaitOnHumanSeconds int64      `json:"total_wait_on_human_seconds"`
	Gates                   []GateWait `json:"gates"`
}

// GateWait is one gate's accumulated wait.
type GateWait struct {
	Gate        string `json:"gate"`
	WaitSeconds int64  `json:"wait_seconds"`
}

// Links carries deep links; each is omitted when the base URL that would
// produce it is unset.
type Links struct {
	Run         string `json:"run,omitempty"`
	Issue       string `json:"issue,omitempty"`
	PullRequest string `json:"pull_request,omitempty"`
}

// MarshalEvent renders e as compact JSON with nil slices normalized to empty
// arrays, so a consumer always sees `"verdicts": []` rather than null. It is
// the exact byte sequence a signing sink signs.
func MarshalEvent(e Event) ([]byte, error) {
	if e.Verdicts == nil {
		e.Verdicts = []Verdict{}
	}
	if e.GateLatency.Gates == nil {
		e.GateLatency.Gates = []GateWait{}
	}
	return json.Marshal(e)
}

// Sink is one push destination.
type Sink interface {
	// Name is the sink KIND ("webhook", "slack", "email"). It is the only
	// identifier ever surfaced outside the sink (logs, /healthz, audit rows).
	Name() string
	// Deliver sends e. It must bound its own I/O independently of ctx and
	// must return errors only via SanitizeTransportError.
	Deliver(ctx context.Context, e Event) error
}

// Destination is optionally implemented by a sink to expose the destination
// HOST (never a URL, path, query or userinfo) for error rendering.
type Destination interface {
	DestinationHost() string
}

// Factory builds a sink from the environment. It returns (nil, nil) when the
// sink is not configured at all, and a non-nil error NAMING THE ENV VAR (never
// its value) when the sink is half- or mis-configured.
type Factory func(getenv func(string) string) (Sink, error)

var (
	registryMu sync.RWMutex
	registry   = map[string]Factory{}
)

// Register adds a sink factory under kind. Each sink file calls it from its
// own init(), so a new sink needs no edit to this file. A duplicate or empty
// kind, or a nil factory, is a programming error and panics at init.
func Register(kind string, f Factory) {
	if kind == "" || f == nil {
		panic("pushnotify: Register requires a non-empty kind and a non-nil factory")
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, dup := registry[kind]; dup {
		panic(fmt.Sprintf("pushnotify: sink kind %q registered twice", kind))
	}
	registry[kind] = f
}

// Registered returns the sorted registered sink kinds.
func Registered() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	kinds := make([]string, 0, len(registry))
	for k := range registry {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	return kinds
}

// SinksFromEnv constructs every configured sink, in sorted kind order. An
// unconfigured sink is skipped silently; a half- or mis-configured sink is a
// NAMED error (never a silent skip), and on any error no sinks are returned,
// so fishhawkd fails startup instead of running with a partial set. Every
// configuration error is reported, joined.
func SinksFromEnv(getenv func(string) string) ([]Sink, error) {
	registryMu.RLock()
	kinds := make([]string, 0, len(registry))
	for k := range registry {
		kinds = append(kinds, k)
	}
	factories := make(map[string]Factory, len(registry))
	for k, f := range registry {
		factories[k] = f
	}
	registryMu.RUnlock()
	sort.Strings(kinds)

	var sinks []Sink
	var errs []error
	for _, k := range kinds {
		s, err := factories[k](getenv)
		if err != nil {
			errs = append(errs, fmt.Errorf("push sink %q: %w", k, err))
			continue
		}
		if s != nil {
			sinks = append(sinks, s)
		}
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return sinks, nil
}

// DeliveryError is the ONLY error shape a delivery failure takes outside a
// sink: the sink name, the destination host, and a classified reason. It
// deliberately retains no underlying cause, so no nested error text (which
// for net/http embeds the full URL, i.e. a Slack credential) can escape via
// Error(), %+v, or errors.Unwrap.
type DeliveryError struct {
	Sink   string
	Host   string
	Reason string
}

func (e *DeliveryError) Error() string {
	if e.Host == "" {
		return fmt.Sprintf("push sink %q: %s", e.Sink, e.Reason)
	}
	return fmt.Sprintf("push sink %q: %s: %s", e.Sink, e.Host, e.Reason)
}

// Timeout reports whether the delivery failed by timing out.
func (e *DeliveryError) Timeout() bool { return e.Reason == ReasonTimeout }

// Classified delivery-failure reasons.
const (
	ReasonTimeout           = "timeout"
	ReasonCanceled          = "canceled"
	ReasonConnectionRefused = "connection refused"
	ReasonConnectionReset   = "connection reset"
	ReasonDNS               = "dns"
	ReasonTLS               = "tls"
	ReasonPanic             = "sink panicked"
	ReasonFailed            = "delivery failed"
)

// HTTPStatusError is a non-2xx response. It carries the status code only —
// never the response body.
type HTTPStatusError struct {
	StatusCode int
}

func (e *HTTPStatusError) Error() string { return fmt.Sprintf("http status %d", e.StatusCode) }

// hostError attaches a destination host to an error that does not carry one
// (an SMTP protocol reply, a marshal error).
type hostError struct {
	host string
	err  error
}

func (e *hostError) Error() string { return e.err.Error() }
func (e *hostError) Unwrap() error { return e.err }

// AtHost attaches host to err so SanitizeTransportError can render it. host
// must be a bare host name or address, never a URL.
func AtHost(host string, err error) error {
	if err == nil {
		return nil
	}
	return &hostError{host: host, err: err}
}

// SanitizeTransportError is the single credential-scrubbing helper: every
// sink routes every delivery error through it, and its output is the only
// error text that reaches a log line, a returned error or an audit row. It
// renders sink name, destination host only (a *url.Error's URL is parsed and
// reduced to its host — userinfo, path and query are discarded) and a
// classified reason. The raw nested cause text is never included. A nil err
// returns nil; an existing *DeliveryError is returned unchanged.
func SanitizeTransportError(sinkName string, err error) error {
	if err == nil {
		return nil
	}
	var de *DeliveryError
	if errors.As(err, &de) {
		return de
	}
	return &DeliveryError{Sink: sinkName, Host: hostOf(err), Reason: classify(err)}
}

func hostOf(err error) string {
	var he *hostError
	if errors.As(err, &he) {
		return he.host
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		if u, perr := url.Parse(ue.URL); perr == nil {
			return u.Hostname()
		}
		return ""
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return dnsErr.Name
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Addr != nil {
		if h, _, serr := net.SplitHostPort(opErr.Addr.String()); serr == nil {
			return h
		}
	}
	return ""
}

func classify(err error) string {
	var hs *HTTPStatusError
	if errors.As(err, &hs) {
		return hs.Error()
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return ReasonTimeout
	}
	if errors.Is(err, context.Canceled) {
		return ReasonCanceled
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return ReasonDNS
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return ReasonConnectionRefused
	}
	if errors.Is(err, syscall.ECONNRESET) {
		return ReasonConnectionReset
	}
	if isTLS(err) {
		return ReasonTLS
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return ReasonTimeout
	}
	return ReasonFailed
}

func isTLS(err error) bool {
	var (
		cve  *tls.CertificateVerificationError
		rhe  tls.RecordHeaderError
		ae   tls.AlertError
		uae  x509.UnknownAuthorityError
		hne  x509.HostnameError
		cie  x509.CertificateInvalidError
		echo *tls.ECHRejectionError
	)
	return errors.As(err, &cve) || errors.As(err, &rhe) || errors.As(err, &ae) ||
		errors.As(err, &uae) || errors.As(err, &hne) || errors.As(err, &cie) ||
		errors.As(err, &echo)
}

// Outcome is one sink's result for one Event.
type Outcome struct {
	Event Event
	Sink  string
	// Err is nil on success and otherwise always a *DeliveryError.
	Err error
	// TimedOut is true when the worker abandoned a Deliver at the per-sink
	// timeout.
	TimedOut bool
	Elapsed  time.Duration
}

// OutcomeFunc receives every per-sink outcome on a worker goroutine, with a
// fresh detached context bounded by Options.OutcomeTimeout (never a request
// context). Owners use it to record failures.
type OutcomeFunc func(ctx context.Context, o Outcome)

// Options tunes a Dispatcher. Zero values take the documented defaults.
type Options struct {
	QueueSize      int           // default 256
	Workers        int           // default 2
	SinkTimeout    time.Duration // default 10s
	OutcomeTimeout time.Duration // default 10s
	OnOutcome      OutcomeFunc
	Logger         *slog.Logger // default slog.Default()
}

// Dispatcher defaults.
const (
	DefaultQueueSize      = 256
	DefaultWorkers        = 2
	DefaultSinkTimeout    = 10 * time.Second
	DefaultOutcomeTimeout = 10 * time.Second
)

// Dispatcher delivers Events asynchronously: a fixed-size queue drained by a
// worker pool started at construction. Enqueue is the only request-path call
// and never blocks. Each delivery runs under a detached context with a
// per-sink timeout, in its own goroutine, so even a Deliver that ignores its
// context cannot pin a worker past the timeout.
type Dispatcher struct {
	sinks     []Sink
	opts      Options
	log       *slog.Logger
	queue     chan Event
	workersWG sync.WaitGroup
	closeOnce sync.Once

	mu      sync.Mutex
	closed  bool
	pending int
	idle    chan struct{} // closed while pending == 0

	abandoned atomic.Int64
}

// NewDispatcher builds a Dispatcher over sinks and starts its workers. With
// no sinks no workers start and every method is a no-op.
func NewDispatcher(sinks []Sink, opts Options) *Dispatcher {
	if opts.QueueSize <= 0 {
		opts.QueueSize = DefaultQueueSize
	}
	if opts.Workers <= 0 {
		opts.Workers = DefaultWorkers
	}
	if opts.SinkTimeout <= 0 {
		opts.SinkTimeout = DefaultSinkTimeout
	}
	if opts.OutcomeTimeout <= 0 {
		opts.OutcomeTimeout = DefaultOutcomeTimeout
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	idle := make(chan struct{})
	close(idle)
	d := &Dispatcher{
		sinks: append([]Sink(nil), sinks...),
		opts:  opts,
		log:   log,
		idle:  idle,
	}
	if len(d.sinks) == 0 {
		return d
	}
	d.queue = make(chan Event, opts.QueueSize)
	for i := 0; i < opts.Workers; i++ {
		d.workersWG.Add(1)
		go d.worker()
	}
	return d
}

// SinkNames returns the configured sink kinds in configuration order.
func (d *Dispatcher) SinkNames() []string {
	if d == nil {
		return []string{}
	}
	names := make([]string, 0, len(d.sinks))
	for _, s := range d.sinks {
		names = append(names, s.Name())
	}
	return names
}

// Abandoned returns how many Deliver goroutines were abandoned at the
// per-sink timeout and may still be running (bounded by the sink's own
// transport deadline).
func (d *Dispatcher) Abandoned() int64 {
	if d == nil {
		return 0
	}
	return d.abandoned.Load()
}

// Enqueue hands e to the workers without blocking. It returns false when the
// queue is full or the Dispatcher is closed (the Event is dropped and a WARN
// logged; the caller records the drop). A nil or sink-less Dispatcher accepts
// and discards e (nothing to deliver, nothing dropped).
func (d *Dispatcher) Enqueue(e Event) bool {
	if d == nil || len(d.sinks) == 0 {
		return true
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		d.log.Warn("push notification dropped: dispatcher closed",
			"run_id", e.RunID, "source_sequence", e.SourceSequence, "event", e.Event)
		return false
	}
	select {
	case d.queue <- e:
		if d.pending == 0 {
			d.idle = make(chan struct{})
		}
		d.pending++
		return true
	default:
		d.log.Warn("push notification dropped: queue full",
			"run_id", e.RunID, "source_sequence", e.SourceSequence, "event", e.Event,
			"queue_size", d.opts.QueueSize)
		return false
	}
}

// Drain blocks until every accepted Event has been fully processed (every
// sink delivered, failed or timed out) or ctx is done. It is the test seam
// that replaces sleeps, and the first half of shutdown.
func (d *Dispatcher) Drain(ctx context.Context) error {
	if d == nil {
		return nil
	}
	d.mu.Lock()
	idle := d.idle
	d.mu.Unlock()
	select {
	case <-idle:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close stops accepting Events, lets the workers finish the queue, and waits
// for them — bounded by ctx as ONE overall deadline. It never waits on an
// abandoned Deliver: a worker moves on at the per-sink timeout. Close is
// idempotent.
func (d *Dispatcher) Close(ctx context.Context) error {
	if d == nil {
		return nil
	}
	d.closeOnce.Do(func() {
		d.mu.Lock()
		d.closed = true
		d.mu.Unlock()
		if d.queue != nil {
			close(d.queue)
		}
	})
	done := make(chan struct{})
	go func() {
		d.workersWG.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (d *Dispatcher) worker() {
	defer d.workersWG.Done()
	for e := range d.queue {
		for _, s := range d.sinks {
			d.report(d.deliverOne(s, e))
		}
		d.mu.Lock()
		d.pending--
		if d.pending == 0 {
			close(d.idle)
		}
		d.mu.Unlock()
	}
}

type deliverResult struct {
	err error
}

// deliverOne runs one sink's Deliver in its own goroutine under a detached,
// per-sink-timeout context and selects on its result vs the timeout.
func (d *Dispatcher) deliverOne(s Sink, e Event) Outcome {
	name := s.Name()
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), d.opts.SinkTimeout)
	defer cancel()
	done := make(chan deliverResult, 1) // buffered: an abandoned goroutine never blocks on send
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- deliverResult{err: &DeliveryError{Sink: name, Host: hostFor(s), Reason: ReasonPanic}}
			}
		}()
		done <- deliverResult{err: s.Deliver(ctx, e)}
	}()
	o := Outcome{Event: e, Sink: name}
	select {
	case r := <-done:
		if r.err != nil {
			o.Err = sanitizeFor(s, r.err)
		}
	case <-ctx.Done():
		d.abandoned.Add(1)
		o.TimedOut = true
		o.Err = &DeliveryError{Sink: name, Host: hostFor(s), Reason: ReasonTimeout}
	}
	o.Elapsed = time.Since(start)
	return o
}

func hostFor(s Sink) string {
	if dst, ok := s.(Destination); ok {
		return dst.DestinationHost()
	}
	return ""
}

// sanitizeFor sanitizes a sink's returned error, filling the host from the
// sink when the error chain does not carry one. Defense in depth: a sink that
// forgot to sanitize still cannot leak raw cause text past the dispatcher.
func sanitizeFor(s Sink, err error) error {
	se := SanitizeTransportError(s.Name(), err)
	var de *DeliveryError
	if errors.As(se, &de) && de.Host == "" {
		if h := hostFor(s); h != "" {
			de = &DeliveryError{Sink: de.Sink, Host: h, Reason: de.Reason}
			return de
		}
	}
	return se
}

func (d *Dispatcher) report(o Outcome) {
	if o.Err != nil {
		d.log.Warn("push notification delivery failed",
			"sink", o.Sink, "run_id", o.Event.RunID, "source_sequence", o.Event.SourceSequence,
			"event", o.Event.Event, "error", o.Err.Error(), "timed_out", o.TimedOut,
			"abandoned_deliveries", d.abandoned.Load())
	} else {
		d.log.Debug("push notification delivered",
			"sink", o.Sink, "run_id", o.Event.RunID, "source_sequence", o.Event.SourceSequence,
			"event", o.Event.Event, "elapsed", o.Elapsed)
	}
	if d.opts.OnOutcome == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), d.opts.OutcomeTimeout)
	defer cancel()
	defer func() {
		if r := recover(); r != nil {
			d.log.Error("push notification outcome callback panicked", "sink", o.Sink,
				"run_id", o.Event.RunID, "source_sequence", o.Event.SourceSequence)
		}
	}()
	d.opts.OnOutcome(ctx, o)
}

// joinNonEmpty is shared by the renderers.
func joinNonEmpty(sep string, parts ...string) string {
	out := parts[:0:0]
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, sep)
}
