package opsmonitor

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/timescale"
)

// --- fixtures -----------------------------------------------------------------

type stubProbe struct {
	name      string
	hostLocal bool
	check     func(ctx context.Context) []Result
	calls     *atomic.Int32
}

func (s stubProbe) Name() string    { return s.name }
func (s stubProbe) HostLocal() bool { return s.hostLocal }
func (s stubProbe) Check(ctx context.Context) []Result {
	if s.calls != nil {
		s.calls.Add(1)
	}
	return s.check(ctx)
}

func fixed(rs ...Result) func(context.Context) []Result {
	return func(context.Context) []Result { return rs }
}

func failingResult(probe, condition, subject string) Result {
	return Result{Probe: probe, Condition: condition, Subject: subject, Status: StatusFailing, Observed: "o", Threshold: "t"}
}

type panicMetaProbe struct{}

func (panicMetaProbe) Name() string                   { panic("boom") }
func (panicMetaProbe) HostLocal() bool                { return false }
func (panicMetaProbe) Check(context.Context) []Result { return nil }

// fakeAudit records global-chain appends and serves ListAll.
type fakeAudit struct {
	audit.BaseFake
	mu          sync.Mutex
	appends     []audit.GlobalChainAppendParams
	appendErr   error
	appendPanic bool
	listErr     error
	listPanic   bool
	listed      []*audit.Entry
	listCalls   int
	listParams  []audit.ListAllParams
}

func (f *fakeAudit) AppendGlobalChained(_ context.Context, p audit.GlobalChainAppendParams) (*audit.Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.appendPanic {
		panic("audit store exploded")
	}
	f.appends = append(f.appends, p)
	if f.appendErr != nil {
		return nil, f.appendErr
	}
	return &audit.Entry{Category: p.Category, Timestamp: p.Timestamp, Payload: p.Payload}, nil
}

func (f *fakeAudit) ListAll(_ context.Context, p audit.ListAllParams) ([]*audit.Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCalls++
	f.listParams = append(f.listParams, p)
	if f.listPanic {
		panic("audit list exploded")
	}
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.listed, nil
}

func (f *fakeAudit) appendCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.appends)
}

func (f *fakeAudit) payload(t *testing.T, i int) map[string]any {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if i >= len(f.appends) {
		t.Fatalf("append %d not recorded (have %d)", i, len(f.appends))
	}
	var m map[string]any
	if err := json.Unmarshal(f.appends[i].Payload, &m); err != nil {
		t.Fatalf("payload: %v", err)
	}
	return m
}

type countingNotifier struct {
	mu    sync.Mutex
	calls []Result
	err   error
	panic bool
}

func (n *countingNotifier) Notify(_ context.Context, r Result, _ Deployment) error {
	n.mu.Lock()
	n.calls = append(n.calls, r)
	n.mu.Unlock()
	if n.panic {
		panic("notifier exploded")
	}
	return n.err
}

func (n *countingNotifier) count() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.calls)
}

// captureHandler records every slog record.
type captureHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r)
	return nil
}
func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(string) slog.Handler      { return h }

func (h *captureHandler) count(level slog.Level, msgContains string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, r := range h.records {
		if r.Level == level && strings.Contains(r.Message, msgContains) {
			n++
		}
	}
	return n
}

func (h *captureHandler) atOrAbove(level slog.Level) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, r := range h.records {
		if r.Level >= level {
			n++
		}
	}
	return n
}

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func newClock() *fakeClock { return &fakeClock{now: time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)} }

func newTicker(probes []Probe, au audit.Repository, n Notifier, clk *fakeClock, h *captureHandler) *Ticker {
	t := &Ticker{
		Probes:      probes,
		Deployment:  DeploymentLocal,
		Audit:       au,
		QuietWindow: time.Hour,
		Now:         clk.Now,
		Logger:      slog.New(h),
	}
	if n != nil {
		t.Notifier = n
	}
	return t
}

// --- Sweep --------------------------------------------------------------------

func TestSweep_NonLocalDeploymentMarksHostProbesNotApplicable(t *testing.T) {
	for _, dep := range []Deployment{DeploymentKubernetes, DeploymentRemote} {
		t.Run(string(dep), func(t *testing.T) {
			var calls, remoteCalls atomic.Int32
			var probes []Probe
			for _, name := range []string{"disk", "gocache", "load", "docker", "runners"} {
				probes = append(probes, stubProbe{name: name, hostLocal: true, calls: &calls,
					check: fixed(failingResult(name, ConditionDiskFreeLow, ""))})
			}
			probes = append(probes, stubProbe{name: "postgres", calls: &remoteCalls,
				check: fixed(Result{Status: StatusOK})})
			res := Sweep(context.Background(), probes, dep, time.Second)
			if calls.Load() != 0 {
				t.Errorf("host-local Check calls = %d, want 0", calls.Load())
			}
			if remoteCalls.Load() != 1 {
				t.Errorf("non-host-local Check calls = %d, want 1", remoteCalls.Load())
			}
			if len(res) != 6 {
				t.Fatalf("results = %+v, want 6", res)
			}
			for _, r := range res[:5] {
				if r.Status != StatusNotApplicable || !strings.Contains(r.Reason, string(dep)) {
					t.Errorf("%s = %+v, want not_applicable naming %s", r.Probe, r, dep)
				}
			}
			if res[5].Status != StatusOK || res[5].Probe != "postgres" {
				t.Errorf("postgres = %+v, want ok", res[5])
			}
		})
	}
}

func TestSweep_LocalDeploymentRunsHostProbes(t *testing.T) {
	var calls atomic.Int32
	res := Sweep(context.Background(), []Probe{stubProbe{name: "disk", hostLocal: true, calls: &calls,
		check: fixed(Result{Status: StatusOK})}}, DeploymentLocal, time.Second)
	if calls.Load() != 1 || res[0].Status != StatusOK || res[0].Probe != "disk" {
		t.Errorf("calls %d, results %+v; want the probe run and its name filled", calls.Load(), res)
	}
}

func TestSweep_ProbePanicReportsUnknown(t *testing.T) {
	res := Sweep(context.Background(), []Probe{stubProbe{name: "p", check: func(context.Context) []Result {
		panic("kaboom")
	}}}, DeploymentLocal, time.Second)
	if len(res) != 1 || res[0].Status != StatusUnknown || res[0].Reason != "probe panicked" {
		t.Fatalf("results = %+v, want one unknown 'probe panicked'", res)
	}
}

func TestSweep_ProbeTimeoutReportsUnknown(t *testing.T) {
	timeout := timescale.D(50 * time.Millisecond)
	res := Sweep(context.Background(), []Probe{stubProbe{name: "late", check: func(ctx context.Context) []Result {
		<-ctx.Done()
		return []Result{{Status: StatusOK}}
	}}}, DeploymentLocal, timeout)
	if len(res) != 1 || res[0].Status != StatusUnknown || !strings.Contains(res[0].Reason, "timed out") {
		t.Fatalf("results = %+v, want one unknown timeout", res)
	}
}

// TestSweep_ProbeIgnoringContextCannotStall: a probe that never returns still
// yields unknown within the deadline.
func TestSweep_ProbeIgnoringContextCannotStall(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	timeout := timescale.D(50 * time.Millisecond)
	start := time.Now()
	res := Sweep(context.Background(), []Probe{stubProbe{name: "wedged", check: func(context.Context) []Result {
		<-release
		return []Result{{Status: StatusOK}}
	}}}, DeploymentLocal, timeout)
	if el := time.Since(start); el > timeout+timescale.D(2*time.Second) {
		t.Errorf("sweep took %s, want about %s", el, timeout)
	}
	if res[0].Status != StatusUnknown {
		t.Errorf("result = %+v, want unknown", res[0])
	}
}

func TestSweep_ParentCancelledReason(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res := Sweep(ctx, []Probe{stubProbe{name: "p", check: func(ctx context.Context) []Result {
		<-ctx.Done()
		return []Result{{Status: StatusOK}}
	}}}, DeploymentLocal, time.Second)
	if res[0].Status != StatusUnknown || !strings.Contains(res[0].Reason, "cancelled") {
		t.Errorf("result = %+v, want unknown 'sweep cancelled'", res[0])
	}
}

func TestSweep_EmptyOrUnknownStatusIsUnknown(t *testing.T) {
	for _, st := range []Status{"", "bogus"} {
		res := Sweep(context.Background(), []Probe{stubProbe{name: "p", check: fixed(Result{Status: st, Observed: "x"})}},
			DeploymentLocal, time.Second)
		if len(res) != 1 || res[0].Status != StatusUnknown || !strings.Contains(res[0].Reason, "unrecognized status") {
			t.Errorf("status %q: results = %+v, want unknown", st, res)
		}
	}
}

func TestSweep_NoResultIsUnknown(t *testing.T) {
	res := Sweep(context.Background(), []Probe{stubProbe{name: "p", check: fixed()}}, DeploymentLocal, time.Second)
	if len(res) != 1 || res[0].Status != StatusUnknown || res[0].Reason != "probe returned no result" {
		t.Fatalf("results = %+v, want one unknown 'no result'", res)
	}
}

func TestSweep_UnknownWithoutReasonGetsReason(t *testing.T) {
	res := Sweep(context.Background(), []Probe{stubProbe{name: "p", check: fixed(Result{Status: StatusUnknown})}},
		DeploymentLocal, time.Second)
	if res[0].Reason == "" {
		t.Errorf("result = %+v, want a filled reason", res[0])
	}
}

func TestSweep_FailingWithoutRemedyGetsTableRemedy(t *testing.T) {
	res := Sweep(context.Background(), []Probe{
		stubProbe{name: "gocache", check: fixed(failingResult("", ConditionGoCacheOverThreshold, ""))},
		stubProbe{name: "x", check: fixed(failingResult("", "unlisted_condition", ""))},
		stubProbe{name: "y", check: fixed(Result{Status: StatusFailing, Condition: ConditionDiskFreeLow, Remedy: "custom"})},
	}, DeploymentLocal, time.Second)
	if res[0].Remedy != remedies[ConditionGoCacheOverThreshold] {
		t.Errorf("gocache remedy = %q, want the table remedy", res[0].Remedy)
	}
	if res[1].Remedy != genericRemedy {
		t.Errorf("unlisted remedy = %q, want the generic remedy", res[1].Remedy)
	}
	if res[2].Remedy != "custom" {
		t.Errorf("explicit remedy overwritten: %q", res[2].Remedy)
	}
}

func TestSweep_NilAndPanickingMetadataReadUnknown(t *testing.T) {
	res := Sweep(context.Background(), []Probe{nil, panicMetaProbe{}, stubProbe{check: fixed(Result{Status: StatusOK})}},
		DeploymentLocal, time.Second)
	if len(res) != 3 {
		t.Fatalf("results = %+v", res)
	}
	if res[0].Status != StatusUnknown || res[0].Reason != "probe is nil" {
		t.Errorf("nil probe = %+v", res[0])
	}
	if res[1].Status != StatusUnknown || res[1].Reason != "probe panicked" {
		t.Errorf("panicking Name = %+v", res[1])
	}
	if res[2].Probe != "unnamed" || res[2].Status != StatusOK {
		t.Errorf("empty name = %+v, want 'unnamed'", res[2])
	}
}

func TestSweep_DefaultTimeoutApplied(t *testing.T) {
	var deadline time.Time
	Sweep(context.Background(), []Probe{stubProbe{name: "p", check: func(ctx context.Context) []Result {
		deadline, _ = ctx.Deadline()
		return []Result{{Status: StatusOK}}
	}}}, DeploymentLocal, 0)
	if left := time.Until(deadline); left < DefaultProbeTimeout-5*time.Second || left > DefaultProbeTimeout {
		t.Errorf("deadline in %s, want about %s", left, DefaultProbeTimeout)
	}
}

func TestRemediesCoverEveryCondition(t *testing.T) {
	for _, c := range Conditions {
		if strings.TrimSpace(remedies[c]) == "" {
			t.Errorf("condition %s has no remedy", c)
		}
		if RemedyFor(c) != remedies[c] {
			t.Errorf("RemedyFor(%s) diverges from the table", c)
		}
	}
	if !strings.Contains(remedies[ConditionGoCacheOverThreshold], "scripts/dev gocache --trim") ||
		!strings.Contains(remedies[ConditionGoCacheOverThreshold], "go clean -cache") {
		t.Errorf("gocache remedy must name the trim and warn off go clean -cache")
	}
}

func TestResolveDeployment(t *testing.T) {
	k8s := func(k string) string {
		if k == kubernetesHostEnv {
			return "10.0.0.1"
		}
		return ""
	}
	none := func(string) string { return "" }
	for _, tc := range []struct {
		raw    string
		getenv func(string) string
		want   Deployment
		err    bool
	}{
		{"", none, DeploymentLocal, false},
		{"auto", none, DeploymentLocal, false},
		{"", k8s, DeploymentKubernetes, false},
		{"auto", k8s, DeploymentKubernetes, false},
		{"", nil, DeploymentLocal, false},
		{"local", k8s, DeploymentLocal, false},
		{"kubernetes", none, DeploymentKubernetes, false},
		{"remote", none, DeploymentRemote, false},
		{"docker", none, "", true},
	} {
		got, err := ResolveDeployment(tc.raw, tc.getenv)
		if (err != nil) != tc.err || got != tc.want {
			t.Errorf("ResolveDeployment(%q) = %q, %v; want %q (err %v)", tc.raw, got, err, tc.want, tc.err)
		}
		if err != nil && !strings.Contains(err.Error(), "auto, local, kubernetes or remote") {
			t.Errorf("error %q does not name the accepted set", err)
		}
	}
}

// --- Ticker -------------------------------------------------------------------

func TestTick_FailingConditionEmitsOneRowWithRemedy(t *testing.T) {
	au, n, h, clk := &fakeAudit{}, &countingNotifier{}, &captureHandler{}, newClock()
	tk := newTicker([]Probe{stubProbe{name: "disk", hostLocal: true, check: fixed(failingResult("disk", ConditionDiskFreeLow, ""))}}, au, n, clk, h)
	tk.Tick(context.Background())
	if au.appendCount() != 1 || n.count() != 1 {
		t.Fatalf("appends %d, notifies %d; want 1/1", au.appendCount(), n.count())
	}
	p := au.payload(t, 0)
	if p["condition"] != ConditionDiskFreeLow || p["remedy"] == "" || p["notification"] != "posted" || p["deployment"] != "local" {
		t.Errorf("payload = %v", p)
	}
	if p["quiet_window_seconds"] != float64(3600) {
		t.Errorf("quiet_window_seconds = %v", p["quiet_window_seconds"])
	}
	a := au.appends[0]
	if a.Category != CategoryOpsProbeFailed || a.ActorKind == nil || *a.ActorKind != audit.ActorSystem || a.AccountID != nil || !a.Timestamp.Equal(clk.Now()) {
		t.Errorf("append = %+v", a)
	}
}

func TestTick_QuietWindowSuppressesDuplicate(t *testing.T) {
	au, n, h, clk := &fakeAudit{}, &countingNotifier{}, &captureHandler{}, newClock()
	tk := newTicker([]Probe{stubProbe{name: "gocache", check: fixed(failingResult("gocache", ConditionGoCacheOverThreshold, ""))}}, au, n, clk, h)
	tk.Tick(context.Background())
	clk.Advance(time.Minute)
	tk.Tick(context.Background())
	if n.count() != 1 || au.appendCount() != 1 {
		t.Errorf("notifies %d, appends %d; want 1/1 inside the window", n.count(), au.appendCount())
	}
	clk.Advance(time.Hour)
	tk.Tick(context.Background())
	if n.count() != 2 || au.appendCount() != 2 {
		t.Errorf("after expiry: notifies %d, appends %d; want 2/2", n.count(), au.appendCount())
	}
}

func TestTick_ClockStepBackwardsBeyondWindowReemits(t *testing.T) {
	au, n, h, clk := &fakeAudit{}, &countingNotifier{}, &captureHandler{}, newClock()
	tk := newTicker([]Probe{stubProbe{name: "load", check: fixed(failingResult("load", ConditionHostLoadHigh, ""))}}, au, n, clk, h)
	tk.Tick(context.Background())
	clk.Advance(-2 * time.Hour)
	tk.Tick(context.Background())
	if n.count() != 2 {
		t.Errorf("notifies = %d, want 2 (a backwards step past the window must not suppress)", n.count())
	}
}

func TestTick_DistinctSubjectsNotSuppressed(t *testing.T) {
	au, n, h, clk := &fakeAudit{}, &countingNotifier{}, &captureHandler{}, newClock()
	tk := newTicker([]Probe{stubProbe{name: "runners", check: fixed(
		failingResult("runners", ConditionOrphanedRunner, "1/a/b"),
		failingResult("runners", ConditionOrphanedRunner, "2/c/d"),
	)}}, au, n, clk, h)
	tk.Tick(context.Background())
	if au.appendCount() != 2 || n.count() != 2 {
		t.Errorf("appends %d, notifies %d; want 2/2", au.appendCount(), n.count())
	}
}

func auditRow(condition, subject string, ts time.Time) *audit.Entry {
	b, _ := json.Marshal(map[string]any{"condition": condition, "subject": subject})
	return &audit.Entry{Category: CategoryOpsProbeFailed, Timestamp: ts, Payload: b}
}

func TestTick_DedupSurvivesRestartViaAudit(t *testing.T) {
	clk := newClock()
	au := &fakeAudit{listed: []*audit.Entry{auditRow(ConditionGoCacheOverThreshold, "", clk.Now().Add(-10*time.Minute))}}
	n, h := &countingNotifier{}, &captureHandler{}
	tk := newTicker([]Probe{stubProbe{name: "gocache", check: fixed(failingResult("gocache", ConditionGoCacheOverThreshold, ""))}}, au, n, clk, h)
	tk.Tick(context.Background())
	if n.count() != 0 || au.appendCount() != 0 {
		t.Errorf("notifies %d, appends %d; want 0/0 (a recent row suppresses a fresh Ticker)", n.count(), au.appendCount())
	}
	if len(au.listParams) != 1 || au.listParams[0].Category == nil || *au.listParams[0].Category != CategoryOpsProbeFailed || au.listParams[0].Limit != 200 {
		t.Errorf("ListAll params = %+v, want the category filter and Limit 200", au.listParams)
	}
	// The found row's timestamp is cached: a second Tick inside the window
	// does not re-read the log.
	tk.Tick(context.Background())
	if au.listCalls != 1 {
		t.Errorf("ListAll calls = %d, want 1", au.listCalls)
	}
}

func TestTick_AuditRowsThatDoNotMatchDoNotSuppress(t *testing.T) {
	clk := newClock()
	bad := &audit.Entry{Category: CategoryOpsProbeFailed, Timestamp: clk.Now(), Payload: []byte("{not json")}
	au := &fakeAudit{listed: []*audit.Entry{
		nil,
		bad,
		auditRow(ConditionGoCacheOverThreshold, "other-subject", clk.Now()),
		auditRow(ConditionDiskFreeLow, "", clk.Now()),
		auditRow(ConditionGoCacheOverThreshold, "", clk.Now().Add(-2*time.Hour)),
		{Category: "something_else", Timestamp: clk.Now(), Payload: []byte(`{"condition":"gocache_over_threshold","subject":""}`)},
	}}
	n, h := &countingNotifier{}, &captureHandler{}
	tk := newTicker([]Probe{stubProbe{name: "gocache", check: fixed(failingResult("gocache", ConditionGoCacheOverThreshold, ""))}}, au, n, clk, h)
	tk.Tick(context.Background())
	if n.count() != 1 {
		t.Errorf("notifies = %d, want 1 (no row matches condition+subject inside the window)", n.count())
	}
}

func TestTick_AuditReadErrorFallsBackToInMemory(t *testing.T) {
	clk := newClock()
	au := &fakeAudit{listErr: errors.New("db down")}
	n, h := &countingNotifier{}, &captureHandler{}
	tk := newTicker([]Probe{stubProbe{name: "disk", check: fixed(failingResult("disk", ConditionDiskFreeLow, ""))}}, au, n, clk, h)
	tk.Tick(context.Background())
	clk.Advance(time.Minute)
	tk.Tick(context.Background())
	if n.count() != 1 {
		t.Errorf("notifies = %d, want 1 (fail open once, then in-memory)", n.count())
	}
	if h.count(slog.LevelWarn, "audit dedup lookup failed") != 1 {
		t.Errorf("want one WARN for the failed lookup")
	}
}

func TestTick_AppendErrorStillRecordsInMemory(t *testing.T) {
	clk := newClock()
	au := &fakeAudit{appendErr: errors.New("append failed")}
	n, h := &countingNotifier{}, &captureHandler{}
	tk := newTicker([]Probe{stubProbe{name: "disk", check: fixed(failingResult("disk", ConditionDiskFreeLow, ""))}}, au, n, clk, h)
	tk.Tick(context.Background())
	clk.Advance(time.Minute)
	tk.Tick(context.Background())
	if n.count() != 1 {
		t.Errorf("notifies = %d, want 1 (lastEmitted set regardless of append)", n.count())
	}
	if h.count(slog.LevelWarn, "audit append failed") != 1 {
		t.Errorf("want one WARN for the failed append")
	}
}

func TestTick_NotificationOutcomeRecordedInPayload(t *testing.T) {
	for name, tc := range map[string]struct {
		notifier Notifier
		want     string
	}{
		"error":    {&countingNotifier{err: errors.New("403")}, "failed"},
		"panic":    {&countingNotifier{panic: true}, "failed"},
		"disabled": {nil, "disabled"},
	} {
		t.Run(name, func(t *testing.T) {
			au, h, clk := &fakeAudit{}, &captureHandler{}, newClock()
			tk := newTicker([]Probe{stubProbe{name: "disk", check: fixed(failingResult("disk", ConditionDiskFreeLow, ""))}}, au, tc.notifier, clk, h)
			tk.Tick(context.Background())
			if au.appendCount() != 1 {
				t.Fatalf("appends = %d, want 1 (the row lands whatever the notification did)", au.appendCount())
			}
			if got := au.payload(t, 0)["notification"]; got != tc.want {
				t.Errorf("notification = %v, want %s", got, tc.want)
			}
		})
	}
}

func TestTick_NoAuditStillNotifies(t *testing.T) {
	n, h, clk := &countingNotifier{}, &captureHandler{}, newClock()
	tk := newTicker([]Probe{stubProbe{name: "disk", check: fixed(failingResult("disk", ConditionDiskFreeLow, ""))}}, nil, n, clk, h)
	tk.Tick(context.Background())
	tk.Tick(context.Background())
	if n.count() != 1 {
		t.Errorf("notifies = %d, want 1 (in-memory dedup without an audit store)", n.count())
	}
}

// TestTick_EmitPathFailuresAreContained is approval condition (3): a failing
// result whose notifier and audit appender panic or error never escapes Tick,
// and the NEXT failing probe is still run and still emitted.
func TestTick_EmitPathFailuresAreContained(t *testing.T) {
	for name, tc := range map[string]struct {
		au *fakeAudit
		n  *countingNotifier
	}{
		"notifier panics, append errors": {&fakeAudit{appendErr: errors.New("append failed")}, &countingNotifier{panic: true}},
		"notifier errors, append panics": {&fakeAudit{appendPanic: true}, &countingNotifier{err: errors.New("403")}},
		"lookup panics":                  {&fakeAudit{listPanic: true}, &countingNotifier{}},
	} {
		t.Run(name, func(t *testing.T) {
			h, clk := &captureHandler{}, newClock()
			var firstCalls, secondCalls atomic.Int32
			tk := newTicker([]Probe{
				stubProbe{name: "disk", calls: &firstCalls, check: fixed(failingResult("disk", ConditionDiskFreeLow, ""))},
				stubProbe{name: "load", calls: &secondCalls, check: fixed(failingResult("load", ConditionHostLoadHigh, ""))},
			}, tc.au, tc.n, clk, h)
			var res []Result
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("Tick panicked: %v", r)
					}
				}()
				res = tk.Tick(context.Background())
			}()
			if firstCalls.Load() != 1 || secondCalls.Load() != 1 || len(res) != 2 {
				t.Fatalf("calls %d/%d, results %+v; want both probes run", firstCalls.Load(), secondCalls.Load(), res)
			}
			if h.atOrAbove(slog.LevelError) != 0 {
				t.Errorf("an ERROR record was logged")
			}
			if name == "lookup panics" {
				if h.count(slog.LevelWarn, "emit panicked") != 2 {
					t.Errorf("want one 'emit panicked' WARN per failing result")
				}
				return
			}
			if tc.n.count() != 2 {
				t.Errorf("notify attempts = %d, want 2 (the second failing result is still emitted)", tc.n.count())
			}
		})
	}
}

func TestTick_AllProbesErrorOrPanicIsFailSafe(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	au := &fakeAudit{appendPanic: true, listPanic: true}
	n, h, clk := &countingNotifier{panic: true}, &captureHandler{}, newClock()
	tk := newTicker([]Probe{
		stubProbe{name: "panics", check: func(context.Context) []Result { panic("x") }},
		stubProbe{name: "hangs", check: func(context.Context) []Result { <-release; return nil }},
		stubProbe{name: "empty", check: fixed()},
		stubProbe{name: "bogus", check: fixed(Result{Status: "weird"})},
		DiskProbe{Path: "/x", Statfs: func(string) (uint64, uint64, error) { return 0, 0, errors.New("eio") }},
		PostgresProbe{},
	}, au, n, clk, h)
	tk.ProbeTimeout = timescale.D(50 * time.Millisecond)
	res := tk.Tick(context.Background())
	if len(res) != 6 {
		t.Fatalf("results = %+v", res)
	}
	for _, r := range res {
		if r.Status != StatusUnknown {
			t.Errorf("%s = %s, want unknown", r.Probe, r.Status)
		}
	}
	if h.atOrAbove(slog.LevelError) != 0 {
		t.Errorf("an ERROR record was logged")
	}
}

func TestTick_UnknownWarnRateLimited(t *testing.T) {
	h, clk := &captureHandler{}, newClock()
	tk := newTicker([]Probe{stubProbe{name: "p", check: fixed(Result{Status: StatusUnknown, Reason: "r"})}}, &fakeAudit{}, &countingNotifier{}, clk, h)
	tk.Tick(context.Background())
	clk.Advance(time.Minute)
	tk.Tick(context.Background())
	if got := h.count(slog.LevelWarn, "probe unknown"); got != 1 {
		t.Errorf("unknown WARNs = %d, want 1 inside the window", got)
	}
	clk.Advance(time.Hour)
	tk.Tick(context.Background())
	if got := h.count(slog.LevelWarn, "probe unknown"); got != 2 {
		t.Errorf("unknown WARNs = %d, want 2 after the window", got)
	}
}

func TestTicker_Defaults(t *testing.T) {
	tk := &Ticker{}
	if tk.quietWindow() != DefaultQuietWindow || tk.logger() == nil || tk.now().IsZero() {
		t.Errorf("defaults not applied")
	}
}

func TestRun_RequiresProbes(t *testing.T) {
	if err := (&Ticker{}).Run(context.Background()); err == nil {
		t.Fatal("Run with no probes returned nil")
	}
}

func TestRun_StopsOnCancel(t *testing.T) {
	var calls atomic.Int32
	h := &captureHandler{}
	tk := &Ticker{
		Probes: []Probe{
			stubProbe{name: "p", calls: &calls, check: fixed(Result{Status: StatusOK})},
			stubProbe{name: "disk", hostLocal: true, check: fixed(Result{Status: StatusOK})},
			nil,
		},
		Deployment: DeploymentRemote,
		Interval:   timescale.D(10 * time.Millisecond),
		Logger:     slog.New(h),
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- tk.Run(ctx) }()
	deadline := time.Now().Add(timescale.D(5 * time.Second))
	for calls.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v, want nil", err)
		}
	case <-time.After(timescale.D(5 * time.Second)):
		t.Fatal("Run did not return after cancel")
	}
	if calls.Load() < 3 {
		t.Errorf("ticks = %d, want >= 3", calls.Load())
	}
	if got := h.count(slog.LevelInfo, "not applicable"); got != 1 {
		t.Errorf("not-applicable INFO lines = %d, want exactly 1", got)
	}
}
