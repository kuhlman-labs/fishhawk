package server

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

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/timescale"
)

// rtSteps returns n copies of d.
func rtSteps(n int, d time.Duration) []time.Duration {
	out := make([]time.Duration, n)
	for i := range out {
		out[i] = d
	}
	return out
}

// The package default is replaced ONCE, before any test goroutine starts, so a
// test exercising the real review loops never waits on the production
// schedule. Tests needing another schedule pass a cfg to the internal variants.
func init() {
	defaultReviewTerminalWriteConfig = reviewTerminalWriteConfig{
		backoff:   rtSteps(12, time.Millisecond),
		settle:    rtSteps(50, time.Millisecond),
		opTimeout: timescale.D(2 * time.Second),
	}
}

// flakyAction is what one flakyReviewAudit operation does.
type flakyAction int

const (
	flakyOK flakyAction = iota
	flakyFail
	flakyCommitThenFail
	flakyBlock
	flakyCommitThenBlock
)

// flakyReviewAudit wraps seqAuditFake with injectable append / list failures.
// The action hooks are set before the code under test runs and never change;
// recovered flips every operation back to flakyOK. The list path returns the
// sequence-stamped entries (real sequences + timestamps), not auditFake's
// unsequenced copies.
type flakyReviewAudit struct {
	*seqAuditFake
	appendAction func(n int, p audit.ChainAppendParams) flakyAction
	listAction   func(n int, category string) flakyAction
	recovered    atomic.Bool
	committed    chan struct{}

	cmu         sync.Mutex
	appendCalls int
	listCalls   int
}

func newFlakyReviewAudit() *flakyReviewAudit {
	return &flakyReviewAudit{seqAuditFake: newSeqAuditFake(), committed: make(chan struct{}, 1)}
}

func (f *flakyReviewAudit) counts() (appends, lists int) {
	f.cmu.Lock()
	defer f.cmu.Unlock()
	return f.appendCalls, f.listCalls
}

func (f *flakyReviewAudit) AppendChained(ctx context.Context, p audit.ChainAppendParams) (*audit.Entry, error) {
	f.cmu.Lock()
	f.appendCalls++
	n := f.appendCalls
	f.cmu.Unlock()
	act := flakyOK
	if !f.recovered.Load() && f.appendAction != nil {
		act = f.appendAction(n, p)
	}
	switch act {
	case flakyFail:
		return nil, errors.New("flaky: injected append failure")
	case flakyBlock:
		<-ctx.Done()
		return nil, ctx.Err()
	}
	e, err := f.seqAuditFake.AppendChained(ctx, p)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	e.Timestamp = p.Timestamp
	out := *e
	f.mu.Unlock()
	switch act {
	case flakyCommitThenFail:
		return nil, errors.New("flaky: committed but the ack was lost")
	case flakyCommitThenBlock:
		select {
		case f.committed <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return &out, nil
}

func (f *flakyReviewAudit) ListForRunByCategory(ctx context.Context, runID uuid.UUID, category string) ([]*audit.Entry, error) {
	f.cmu.Lock()
	f.listCalls++
	n := f.listCalls
	f.cmu.Unlock()
	act := flakyOK
	if !f.recovered.Load() && f.listAction != nil {
		act = f.listAction(n, category)
	}
	switch act {
	case flakyFail:
		return nil, errors.New("flaky: injected list failure")
	case flakyBlock:
		<-ctx.Done()
		return nil, ctx.Err()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*audit.Entry
	for _, e := range f.seeded {
		if e.RunID != nil && *e.RunID == runID && e.Category == category {
			cp := *e
			out = append(out, &cp)
		}
	}
	for _, e := range f.entries {
		if e.RunID != nil && *e.RunID == runID && e.Category == category {
			cp := *e
			out = append(out, &cp)
		}
	}
	return out, nil
}

// rows returns the run's stored entries of one category.
func (f *flakyReviewAudit) rows(t *testing.T, runID uuid.UUID, category string) []*audit.Entry {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*audit.Entry
	for _, e := range f.entries {
		if e.RunID != nil && *e.RunID == runID && e.Category == category {
			cp := *e
			out = append(out, &cp)
		}
	}
	return out
}

// namedFailures returns the run's stored rows of category whose reason starts
// with verdict_write_failed.
func (f *flakyReviewAudit) namedFailures(t *testing.T, runID uuid.UUID, category string) []planreview.ReviewFailedPayload {
	t.Helper()
	var out []planreview.ReviewFailedPayload
	for _, e := range f.rows(t, runID, category) {
		var p planreview.ReviewFailedPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("decode %s: %v", category, err)
		}
		if strings.HasPrefix(p.Reason, reviewVerdictWriteFailedPrefix) {
			out = append(out, p)
		}
	}
	return out
}

func (f *flakyReviewAudit) seedStarted(runID, stageID uuid.UUID, seq int64, ts time.Time, category string, configured int) {
	b, _ := json.Marshal(planreview.ReviewStartedPayload{ConfiguredAgents: configured, Authority: planreview.AuthorityAdvisory})
	rid, sid := runID, stageID
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seeded = append(f.seeded, &audit.Entry{RunID: &rid, StageID: &sid, Sequence: seq, Timestamp: ts, Category: category, Payload: b})
}

func newRTServer(t *testing.T, f *flakyReviewAudit) (*Server, *syncBuffer) {
	t.Helper()
	buf := &syncBuffer{}
	logger := slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return New(Config{Addr: "127.0.0.1:0", AuditRepo: f, Logger: logger}), buf
}

func rtCfg(backoff, settle []time.Duration, op time.Duration) reviewTerminalWriteConfig {
	return reviewTerminalWriteConfig{backoff: backoff, settle: settle, opTimeout: op}
}

func planReviewedParams(runID, stageID uuid.UUID) audit.ChainAppendParams {
	b, _ := json.Marshal(planreview.PlanReviewedPayload{
		ReviewerKind: "agent", ReviewerModel: "m", Authority: planreview.AuthorityAdvisory,
		Verdict:  planreview.VerdictApproveWithConcerns,
		Concerns: []planreview.Concern{{Severity: planreview.SeverityMedium, Category: "scope", Note: "a"}, {Severity: planreview.SeverityLow, Category: "scope", Note: "b"}},
	})
	sk := audit.ActorKind("system")
	sid := stageID
	return audit.ChainAppendParams{RunID: runID, StageID: &sid, Category: "plan_reviewed", ActorKind: &sk, Payload: b}
}

func implementReviewedParams(runID, stageID uuid.UUID) audit.ChainAppendParams {
	p := planReviewedParams(runID, stageID)
	p.Category = "implement_reviewed"
	return p
}

func registered(runID uuid.UUID, label string) int {
	return len(lostReviewTerminalsFor(lostReviewTerminalKey{runID: runID, stage: label}))
}

func failAll(int, audit.ChainAppendParams) flakyAction { return flakyFail }

// (1)
func TestAppendReviewTerminal_TransientOutageThenRecovers(t *testing.T) {
	f := newFlakyReviewAudit()
	f.appendAction = func(n int, _ audit.ChainAppendParams) flakyAction {
		if n <= 3 {
			return flakyFail
		}
		return flakyOK
	}
	s, _ := newRTServer(t, f)
	runID, stageID := uuid.New(), uuid.New()
	e, err := s.appendReviewTerminalWith(context.Background(), planReviewedParams(runID, stageID), defaultReviewTerminalWriteConfig)
	if err != nil || e == nil {
		t.Fatalf("append = (%v, %v), want an entry", e, err)
	}
	if got := len(f.rows(t, runID, "plan_reviewed")); got != 1 {
		t.Fatalf("plan_reviewed rows = %d, want 1", got)
	}
}

// (2) C1 of the plan: the retry probe finds the lost-ack commit.
func TestAppendReviewTerminal_AmbiguousCommitNotDuplicated(t *testing.T) {
	f := newFlakyReviewAudit()
	f.appendAction = func(n int, _ audit.ChainAppendParams) flakyAction {
		if n == 1 {
			return flakyCommitThenFail
		}
		return flakyOK
	}
	s, _ := newRTServer(t, f)
	runID, stageID := uuid.New(), uuid.New()
	e, err := s.appendReviewTerminalWith(context.Background(), planReviewedParams(runID, stageID), defaultReviewTerminalWriteConfig)
	if err != nil || e == nil {
		t.Fatalf("append = (%v, %v), want the committed entry", e, err)
	}
	rows := f.rows(t, runID, "plan_reviewed")
	if len(rows) != 1 {
		t.Fatalf("plan_reviewed rows = %d, want exactly 1 (no duplicate after a lost ack)", len(rows))
	}
	if e.Sequence != rows[0].Sequence || e.Sequence == 0 {
		t.Errorf("returned Sequence = %d, stored = %d (C6: must be the stored row's real sequence)", e.Sequence, rows[0].Sequence)
	}
}

// (3)
func TestAppendReviewTerminal_ProbeReadErrorSkipsAppend(t *testing.T) {
	f := newFlakyReviewAudit()
	f.appendAction = failAll
	f.listAction = func(int, string) flakyAction { return flakyFail }
	s, _ := newRTServer(t, f)
	runID, stageID := uuid.New(), uuid.New()
	cfg := rtCfg(rtSteps(4, time.Millisecond), rtSteps(1, timescale.D(500*time.Millisecond)), timescale.D(time.Second))
	if _, err := s.appendReviewTerminalWith(context.Background(), planReviewedParams(runID, stageID), cfg); err == nil {
		t.Fatal("append succeeded under a total outage")
	}
	appends, lists := f.counts()
	if appends != 1 {
		t.Errorf("append calls = %d, want 1 (every probe failed, so no retry append and no named attempt)", appends)
	}
	if lists != 5 {
		t.Errorf("list calls = %d, want 5 (4 retry probes + the final probe)", lists)
	}
	s.waitBackgroundReviews()
}

// (4)
func TestAppendReviewTerminal_ContextCancelStopsRetryingAndRegisters(t *testing.T) {
	f := newFlakyReviewAudit()
	f.appendAction = failAll
	f.listAction = func(int, string) flakyAction { return flakyFail }
	s, _ := newRTServer(t, f)
	runID, stageID := uuid.New(), uuid.New()
	cfg := rtCfg(rtSteps(3, time.Hour), rtSteps(3, time.Millisecond), timescale.D(time.Second))
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	start := time.Now()
	if _, err := s.appendReviewTerminalWith(ctx, planReviewedParams(runID, stageID), cfg); err == nil {
		t.Fatal("append succeeded under a total outage")
	}
	if el := time.Since(start); el > timescale.D(2*time.Second) {
		t.Fatalf("append returned after %s, want within %s of the cancel", el, timescale.D(2*time.Second))
	}
	s.waitBackgroundReviews()
	if n := len(f.namedFailures(t, runID, "plan_review_failed")); n != 0 {
		t.Errorf("named failures = %d, want 0 (the outage never clears)", n)
	}
	if n := registered(runID, "plan"); n != 1 {
		t.Errorf("registered = %d, want 1 (a cancel must register, never drop)", n)
	}
}

// (5)
func TestAppendReviewTerminal_HungAttemptBoundedByPerAttemptTimeout(t *testing.T) {
	f := newFlakyReviewAudit()
	f.appendAction = func(n int, _ audit.ChainAppendParams) flakyAction {
		if n == 1 {
			return flakyBlock
		}
		return flakyOK
	}
	s, _ := newRTServer(t, f)
	runID, stageID := uuid.New(), uuid.New()
	cfg := rtCfg(rtSteps(2, time.Millisecond), rtSteps(1, time.Millisecond), 20*time.Millisecond)
	done := make(chan struct{})
	var err error
	go func() {
		_, err = s.appendReviewTerminalWith(context.WithoutCancel(context.Background()), planReviewedParams(runID, stageID), cfg)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timescale.D(2 * time.Second)):
		t.Fatal("a hung append was not bounded by the per-attempt timeout")
	}
	if err != nil {
		t.Fatalf("append err = %v, want success on the retry", err)
	}
	if n := len(f.rows(t, runID, "plan_reviewed")); n != 1 {
		t.Errorf("plan_reviewed rows = %d, want 1", n)
	}
}

// rejectCategory rejects every append of one category.
func rejectCategory(cat string) func(int, audit.ChainAppendParams) flakyAction {
	return func(_ int, p audit.ChainAppendParams) flakyAction {
		if p.Category == cat {
			return flakyFail
		}
		return flakyOK
	}
}

// (6)
func TestAppendReviewTerminal_ExhaustedPayloadRejected_NamedFailureLands(t *testing.T) {
	f := newFlakyReviewAudit()
	f.appendAction = rejectCategory("plan_reviewed")
	s, logs := newRTServer(t, f)
	runID, stageID := uuid.New(), uuid.New()
	cfg := rtCfg(rtSteps(3, time.Millisecond), rtSteps(1, time.Hour), timescale.D(time.Second))
	p := planReviewedParams(runID, stageID)
	if _, err := s.appendReviewTerminalWith(context.Background(), p, cfg); err == nil {
		t.Fatal("append succeeded although plan_reviewed is rejected")
	}
	named := f.namedFailures(t, runID, "plan_review_failed")
	if len(named) != 1 {
		t.Fatalf("named failures = %d, want exactly 1 on return", len(named))
	}
	if !strings.Contains(named[0].Reason, "verdict approve_with_concerns, 2 concerns") {
		t.Errorf("reason = %q, want the verdict and concern count", named[0].Reason)
	}
	if n := registered(runID, "plan"); n != 0 {
		t.Errorf("registered = %d, want 0", n)
	}
	if !strings.Contains(logs.String(), "review terminal write exhausted its retry budget") || !strings.Contains(logs.String(), `\"verdict\":\"approve_with_concerns\"`) {
		t.Errorf("ERROR log lacks the exhaustion line or the payload JSON:\n%s", logs.String())
	}
}

// (7)
func TestAppendReviewTerminal_ExhaustedOutage_DetachedSettleLandsAfterRecovery(t *testing.T) {
	f := newFlakyReviewAudit()
	f.appendAction = failAll
	s, _ := newRTServer(t, f)
	runID, stageID := uuid.New(), uuid.New()
	cfg := rtCfg(rtSteps(3, time.Millisecond), rtSteps(200, timescale.D(5*time.Millisecond)), timescale.D(time.Second))
	if _, err := s.appendReviewTerminalWith(context.Background(), planReviewedParams(runID, stageID), cfg); err == nil {
		t.Fatal("append succeeded under a total outage")
	}
	f.recovered.Store(true)
	s.waitBackgroundReviews()
	if n := len(f.namedFailures(t, runID, "plan_review_failed")); n != 1 {
		t.Errorf("named failures = %d, want 1 (the settle lands it after recovery)", n)
	}
	if n := registered(runID, "plan"); n != 0 {
		t.Errorf("registered = %d, want 0", n)
	}
}

// (8)
func TestSettleLostReviewTerminal_BoundedLeavesEntryRegistered(t *testing.T) {
	f := newFlakyReviewAudit()
	f.appendAction = failAll
	s, logs := newRTServer(t, f)
	runID, stageID := uuid.New(), uuid.New()
	cfg := rtCfg(rtSteps(1, time.Millisecond), rtSteps(3, time.Millisecond), timescale.D(time.Second))
	_, _ = s.appendReviewTerminalWith(context.Background(), planReviewedParams(runID, stageID), cfg)
	s.waitBackgroundReviews()
	if n := registered(runID, "plan"); n != 1 {
		t.Errorf("registered = %d, want 1 after the bounded settle ends", n)
	}
	if !strings.Contains(logs.String(), "fishhawk_reconcile_reviews") {
		t.Errorf("settle exhaustion ERROR does not name fishhawk_reconcile_reviews:\n%s", logs.String())
	}
}

// registerParked registers a lost plan terminal for (runID, stageID) with a
// settle that cannot run before the test drives the flush itself.
func registerParked(t *testing.T, s *Server, runID, stageID uuid.UUID, origTS time.Time) *lostReviewTerminal {
	t.Helper()
	p := planReviewedParams(runID, stageID)
	p.Timestamp = origTS
	named, stage, ok := buildVerdictWriteFailedParams(p, 1, time.Millisecond, errors.New("down"))
	if !ok {
		t.Fatal("buildVerdictWriteFailedParams rejected plan_reviewed")
	}
	e := &lostReviewTerminal{stageID: stageID, originalCategory: p.Category, originalTimestamp: origTS, named: named}
	s.registerLostReviewTerminal(context.Background(), e, runID, stage, rtCfg(nil, rtSteps(1, timescale.D(30*time.Second)), time.Second))
	return e
}

// (9)
func TestFlushLostReviewTerminals_StaleRoundDropped(t *testing.T) {
	f := newFlakyReviewAudit()
	s, _ := newRTServer(t, f)
	runID, stageID := uuid.New(), uuid.New()
	orig := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	registerParked(t, s, runID, stageID, orig)
	f.seedStarted(runID, stageID, 10, orig.Add(time.Minute), "plan_review_started", 1)
	appended, _, _ := s.flushLostReviewTerminals(context.Background(), runID, "plan", rtCfg(nil, nil, time.Second))
	if appended != 0 || len(f.rows(t, runID, "plan_review_failed")) != 0 {
		t.Errorf("flush appended %d to a newer round, want 0", appended)
	}
	if n := registered(runID, "plan"); n != 0 {
		t.Errorf("registered = %d, want 0 (stale entry dropped)", n)
	}
}

// (10)
func TestFlushLostReviewTerminals_OriginalLandedDropsNamedFailure(t *testing.T) {
	f := newFlakyReviewAudit()
	s, _ := newRTServer(t, f)
	runID, stageID := uuid.New(), uuid.New()
	f.seedStarted(runID, stageID, 10, time.Now().UTC().Add(-time.Hour), "plan_review_started", 1)
	p := planReviewedParams(runID, stageID)
	p.Timestamp = nextReviewTerminalTimestamp()
	if _, err := f.AppendChained(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	registerParked(t, s, runID, stageID, p.Timestamp)
	appended, confirmed, _ := s.flushLostReviewTerminals(context.Background(), runID, "plan", rtCfg(nil, nil, time.Second))
	if appended != 0 || len(f.rows(t, runID, "plan_review_failed")) != 0 {
		t.Errorf("flush appended %d beside the landed original, want 0", appended)
	}
	if !confirmed || registered(runID, "plan") != 0 {
		t.Errorf("confirmed = %v registered = %d, want true / 0", confirmed, registered(runID, "plan"))
	}
}

// (11)
func TestFlushLostReviewTerminals_CapsAtConfiguredRemainder(t *testing.T) {
	f := newFlakyReviewAudit()
	s, _ := newRTServer(t, f)
	runID, stageID := uuid.New(), uuid.New()
	f.seedStarted(runID, stageID, 10, time.Now().UTC().Add(-time.Hour), "plan_review_started", 1)
	other := planReviewedParams(runID, stageID)
	other.Timestamp = nextReviewTerminalTimestamp()
	if _, err := f.AppendChained(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	registerParked(t, s, runID, stageID, nextReviewTerminalTimestamp())
	appended, _, _ := s.flushLostReviewTerminals(context.Background(), runID, "plan", rtCfg(nil, nil, time.Second))
	if appended != 0 || len(f.rows(t, runID, "plan_review_failed")) != 0 {
		t.Errorf("flush appended %d into a settled round, want 0", appended)
	}
	if n := registered(runID, "plan"); n != 0 {
		t.Errorf("registered = %d, want 0", n)
	}
}

// (12)
func TestReviewTerminalTimestamp_StrictlyIncreasing(t *testing.T) {
	prev := nextReviewTerminalTimestamp()
	for i := 0; i < 1000; i++ {
		ts := nextReviewTerminalTimestamp()
		if !ts.After(prev) || !ts.Equal(ts.Truncate(time.Microsecond)) {
			t.Fatalf("call %d: %s not strictly after %s at µs precision", i, ts, prev)
		}
		prev = ts
	}
}

// (13)
func TestProductionReviewTerminalWriteConfig_RidesOutMinutes(t *testing.T) {
	cfg := productionReviewTerminalWriteConfig()
	var b, st time.Duration
	for _, d := range cfg.backoff {
		b += d
	}
	for _, d := range cfg.settle {
		st += d
	}
	if b < 5*time.Minute || st < 20*time.Minute || cfg.opTimeout <= 0 {
		t.Errorf("backoff %s settle %s opTimeout %s; want >= 5m, >= 20m, > 0", b, st, cfg.opTimeout)
	}
}

// (14)
func TestPlanReviewLoop_TransientAppendOutage_VerdictAndConcernsLand(t *testing.T) {
	f := newFlakyReviewAudit()
	var failed atomic.Int32
	f.appendAction = func(_ int, p audit.ChainAppendParams) flakyAction {
		if p.Category == "plan_reviewed" && failed.Add(1) <= 3 {
			return flakyFail
		}
		return flakyOK
	}
	cr := newFakeConcernRepo()
	s := New(Config{Addr: "127.0.0.1:0", AuditRepo: f, ConcernRepo: cr})
	runID, stageID := uuid.New(), uuid.New()
	rev := &fakePlanReviewer{verdict: &planreview.ReviewVerdict{
		Verdict:  planreview.VerdictApproveWithConcerns,
		Concerns: []planreview.Concern{{Severity: planreview.SeverityMedium, Category: "scope", Note: "n"}},
	}, model: "claude-opus-4-8"}
	s.runPlanReviewLoop(context.Background(), runID, stageID, []reviewerInvocation{{reviewer: rev}}, planreview.AuthorityAdvisory, "prompt", "author", planreview.DefaultReviewBudget, "")
	rows := f.rows(t, runID, "plan_reviewed")
	if len(rows) != 1 {
		t.Fatalf("plan_reviewed rows = %d, want 1", len(rows))
	}
	if n := len(f.rows(t, runID, "plan_review_failed")); n != 0 {
		t.Errorf("plan_review_failed rows = %d, want 0", n)
	}
	concerns, _ := cr.ListByRun(context.Background(), runID)
	if len(concerns) != 1 || concerns[0].OriginReviewSequence != rows[0].Sequence {
		t.Errorf("concerns = %d (want 1) stamped with the stored row's sequence %d", len(concerns), rows[0].Sequence)
	}
}

// (15)
func TestImplementReviewLoop_TransientAppendOutage_VerdictLands(t *testing.T) {
	f := newFlakyReviewAudit()
	var failed atomic.Int32
	f.appendAction = func(_ int, p audit.ChainAppendParams) flakyAction {
		if p.Category == "implement_reviewed" && failed.Add(1) <= 3 {
			return flakyFail
		}
		return flakyOK
	}
	cr := newFakeConcernRepo()
	s := New(Config{Addr: "127.0.0.1:0", AuditRepo: f, ConcernRepo: cr})
	runID, stageID := uuid.New(), uuid.New()
	rev := &fakePlanReviewer{verdict: &planreview.ReviewVerdict{
		Verdict:  planreview.VerdictApproveWithConcerns,
		Concerns: []planreview.Concern{{Severity: planreview.SeverityMedium, Category: "scope", Note: "n"}},
	}, model: "claude-opus-4-8"}
	s.runImplementReviewInvocations(context.Background(), runID, stageID,
		[]reviewerInvocation{{reviewer: rev}}, planreview.AuthorityAdvisory, "prompt", "author", "", "", planreview.DefaultReviewBudget, "", 0)
	rows := f.rows(t, runID, "implement_reviewed")
	if len(rows) != 1 {
		t.Fatalf("implement_reviewed rows = %d, want 1", len(rows))
	}
	if n := len(f.rows(t, runID, "implement_review_failed")); n != 0 {
		t.Errorf("implement_review_failed rows = %d, want 0", n)
	}
	concerns, _ := cr.ListByRun(context.Background(), runID)
	if len(concerns) != 1 || concerns[0].OriginReviewSequence != rows[0].Sequence {
		t.Errorf("concerns = %d (want 1) stamped with the stored row's sequence %d", len(concerns), rows[0].Sequence)
	}
}

// (16)
func TestAppendReviewTerminal_FinalAttemptAmbiguousCommit_NoNamedFailure(t *testing.T) {
	f := newFlakyReviewAudit()
	f.appendAction = func(n int, p audit.ChainAppendParams) flakyAction {
		if p.Category != "plan_reviewed" {
			return flakyOK
		}
		if n == 4 { // the LAST phase-1 attempt (1 initial + 3 retries)
			return flakyCommitThenFail
		}
		return flakyFail
	}
	s, _ := newRTServer(t, f)
	runID, stageID := uuid.New(), uuid.New()
	cfg := rtCfg(rtSteps(3, time.Millisecond), rtSteps(1, time.Hour), timescale.D(time.Second))
	e, err := s.appendReviewTerminalWith(context.Background(), planReviewedParams(runID, stageID), cfg)
	if err != nil || e == nil {
		t.Fatalf("append = (%v, %v), want the committed entry from the final probe", e, err)
	}
	if n := len(f.rows(t, runID, "plan_reviewed")); n != 1 {
		t.Errorf("plan_reviewed rows = %d, want 1", n)
	}
	if n := len(f.namedFailures(t, runID, "plan_review_failed")); n != 0 {
		t.Errorf("verdict_write_failed rows = %d, want 0", n)
	}
	if n := registered(runID, "plan"); n != 0 {
		t.Errorf("registered = %d, want 0", n)
	}
}

// (17)
func TestAppendReviewTerminal_CancelAfterAmbiguousCommit_NoNamedFailure(t *testing.T) {
	f := newFlakyReviewAudit()
	f.appendAction = func(n int, _ audit.ChainAppendParams) flakyAction {
		if n == 1 {
			return flakyCommitThenBlock
		}
		return flakyOK
	}
	s, _ := newRTServer(t, f)
	runID, stageID := uuid.New(), uuid.New()
	cfg := rtCfg(rtSteps(3, time.Hour), rtSteps(1, time.Hour), timescale.D(10*time.Second))
	ctx, cancel := context.WithCancel(context.Background())
	go func() { <-f.committed; cancel() }()
	e, err := s.appendReviewTerminalWith(ctx, planReviewedParams(runID, stageID), cfg)
	if err != nil || e == nil {
		t.Fatalf("append = (%v, %v), want the committed entry", e, err)
	}
	if n := len(f.rows(t, runID, "plan_reviewed")); n != 1 {
		t.Errorf("plan_reviewed rows = %d, want 1", n)
	}
	if n := len(f.namedFailures(t, runID, "plan_review_failed")); n != 0 {
		t.Errorf("verdict_write_failed rows = %d, want 0", n)
	}
	if n := registered(runID, "plan"); n != 0 {
		t.Errorf("registered = %d, want 0", n)
	}
}

// (18) Under binding condition C1 a failed final probe skips the immediate
// named attempt, so the only append is the initial one.
func TestAppendReviewTerminal_HungProbeCountsAsFailedAttempt(t *testing.T) {
	f := newFlakyReviewAudit()
	f.appendAction = failAll
	f.listAction = func(int, string) flakyAction { return flakyBlock }
	s, _ := newRTServer(t, f)
	runID, stageID := uuid.New(), uuid.New()
	cfg := rtCfg(rtSteps(3, time.Millisecond), rtSteps(3, time.Millisecond), 20*time.Millisecond)
	start := time.Now()
	_, _ = s.appendReviewTerminalWith(context.Background(), planReviewedParams(runID, stageID), cfg)
	if el := time.Since(start); el > timescale.D(2*time.Second) {
		t.Fatalf("append returned after %s; a hung probe was not bounded", el)
	}
	if appends, _ := f.counts(); appends != 1 {
		t.Errorf("append calls = %d, want 1 (no retry after a failed probe, no named attempt after a failed final probe)", appends)
	}
	if n := registered(runID, "plan"); n != 1 {
		t.Errorf("registered = %d, want 1", n)
	}
	s.waitBackgroundReviews()
}

// (19)
func TestFlushLostReviewTerminals_BlockedAppendReleasesLockWithinBound(t *testing.T) {
	f := newFlakyReviewAudit()
	var blocked atomic.Bool
	f.appendAction = func(_ int, p audit.ChainAppendParams) flakyAction {
		if p.Category == "plan_review_failed" && !blocked.Swap(true) {
			return flakyBlock
		}
		return flakyOK
	}
	s, _ := newRTServer(t, f)
	runID, stageID := uuid.New(), uuid.New()
	registerParked(t, s, runID, stageID, nextReviewTerminalTimestamp())
	cfg := rtCfg(nil, nil, 20*time.Millisecond)
	done := make(chan int, 1)
	go func() { a, _, _ := s.flushLostReviewTerminals(context.Background(), runID, "plan", cfg); done <- a }()
	select {
	case a := <-done:
		if a != 0 {
			t.Fatalf("first flush appended %d, want 0", a)
		}
	case <-time.After(timescale.D(2 * time.Second)):
		t.Fatal("a blocked flush append held the stripe lock past its bound")
	}
	if registered(runID, "plan") != 1 {
		t.Fatal("entry dropped after a failed flush append")
	}
	lock := reconcileEmitLockFor(runID)
	if !lock.TryLock() {
		t.Fatal("stripe lock still held after the flush returned")
	}
	lock.Unlock()
	if a, _, _ := s.flushLostReviewTerminals(context.Background(), runID, "plan", cfg); a != 1 {
		t.Errorf("second flush appended %d, want 1", a)
	}
	if n := registered(runID, "plan"); n != 0 {
		t.Errorf("registered = %d, want 0", n)
	}
}

// (20)
func TestAppendReviewTerminal_CancelledDuringPayloadRejection_NamedFailureLands(t *testing.T) {
	f := newFlakyReviewAudit()
	f.appendAction = rejectCategory("plan_reviewed")
	s, _ := newRTServer(t, f)
	runID, stageID := uuid.New(), uuid.New()
	cfg := rtCfg(rtSteps(3, time.Hour), rtSteps(1, time.Hour), timescale.D(time.Second))
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	_, _ = s.appendReviewTerminalWith(ctx, planReviewedParams(runID, stageID), cfg)
	if n := len(f.namedFailures(t, runID, "plan_review_failed")); n != 1 {
		t.Errorf("named failures = %d, want 1 (cancellation shares the terminal path)", n)
	}
	if n := registered(runID, "plan"); n != 0 {
		t.Errorf("registered = %d, want 0", n)
	}
}

// recorder captures reconcileRecomputeAuditComplete calls with a snapshot of
// the implement_review_failed rows at call time.
type recomputeRecorder struct {
	mu    sync.Mutex
	calls []int
}

func (r *recomputeRecorder) hook(f *flakyReviewAudit, t *testing.T) func(context.Context, uuid.UUID) {
	return func(_ context.Context, runID uuid.UUID) {
		n := len(f.rows(t, runID, "implement_review_failed"))
		r.mu.Lock()
		r.calls = append(r.calls, n)
		r.mu.Unlock()
	}
}

func (r *recomputeRecorder) snapshot() []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]int(nil), r.calls...)
}

// (21)
func TestSettleLostReviewTerminal_ImplementRound_RepublishesAuditComplete(t *testing.T) {
	for _, tc := range []struct {
		name   string
		params func(uuid.UUID, uuid.UUID) audit.ChainAppendParams
		want   int
	}{
		{"implement_round_republishes", implementReviewedParams, 1},
		{"plan_round_does_not_republish", planReviewedParams, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFlakyReviewAudit()
			f.appendAction = failAll
			s, _ := newRTServer(t, f)
			rec := &recomputeRecorder{}
			s.reconcileRecomputeAuditComplete = rec.hook(f, t)
			runID, stageID := uuid.New(), uuid.New()
			cfg := rtCfg(rtSteps(2, time.Millisecond), rtSteps(200, timescale.D(5*time.Millisecond)), timescale.D(time.Second))
			_, _ = s.appendReviewTerminalWith(context.Background(), tc.params(runID, stageID), cfg)
			f.recovered.Store(true)
			s.waitBackgroundReviews()
			calls := rec.snapshot()
			if len(calls) != tc.want {
				t.Fatalf("republish calls = %d, want %d", len(calls), tc.want)
			}
			if tc.want == 1 && calls[0] != 1 {
				t.Errorf("implement_review_failed rows at republish = %d, want 1 (republish after the row landed)", calls[0])
			}
		})
	}
}

// C2: the named append commits but returns an error; the next flush finds it
// by probe and the republish still fires.
func TestSettleLostReviewTerminal_NamedAmbiguousCommit_RepublishesAuditComplete(t *testing.T) {
	f := newFlakyReviewAudit()
	f.appendAction = func(_ int, p audit.ChainAppendParams) flakyAction {
		if p.Category == "implement_review_failed" {
			return flakyCommitThenFail
		}
		return flakyFail
	}
	s, _ := newRTServer(t, f)
	rec := &recomputeRecorder{}
	s.reconcileRecomputeAuditComplete = rec.hook(f, t)
	runID, stageID := uuid.New(), uuid.New()
	cfg := rtCfg(rtSteps(1, time.Millisecond), rtSteps(5, time.Millisecond), timescale.D(time.Second))
	_, _ = s.appendReviewTerminalWith(context.Background(), implementReviewedParams(runID, stageID), cfg)
	s.waitBackgroundReviews()
	if n := len(f.namedFailures(t, runID, "implement_review_failed")); n != 1 {
		t.Fatalf("named failures = %d, want 1", n)
	}
	if calls := rec.snapshot(); len(calls) != 1 {
		t.Errorf("republish calls = %d, want 1 (a probe-confirmed landing must republish)", len(calls))
	}
	if n := registered(runID, "implement"); n != 0 {
		t.Errorf("registered = %d, want 0", n)
	}
}

// C1: the original commits then errors, the final probe errors; once the store
// recovers there is exactly one terminal and no verdict_write_failed.
func TestAppendReviewTerminal_FinalProbeFails_NoNamedAttempt_SingleTerminal(t *testing.T) {
	f := newFlakyReviewAudit()
	f.appendAction = func(n int, _ audit.ChainAppendParams) flakyAction {
		if n == 1 {
			return flakyCommitThenFail
		}
		return flakyOK
	}
	f.listAction = func(int, string) flakyAction { return flakyFail }
	s, _ := newRTServer(t, f)
	runID, stageID := uuid.New(), uuid.New()
	cfg := rtCfg(nil, rtSteps(200, timescale.D(5*time.Millisecond)), timescale.D(time.Second))
	if _, err := s.appendReviewTerminalWith(context.Background(), planReviewedParams(runID, stageID), cfg); err == nil {
		t.Fatal("append reported success although the final probe failed")
	}
	f.recovered.Store(true)
	s.waitBackgroundReviews()
	if n := len(f.rows(t, runID, "plan_reviewed")); n != 1 {
		t.Errorf("plan_reviewed rows = %d, want 1", n)
	}
	if n := len(f.rows(t, runID, "plan_review_failed")); n != 0 {
		t.Errorf("plan_review_failed rows = %d, want 0 (two terminals for one reviewer)", n)
	}
	if n := registered(runID, "plan"); n != 0 {
		t.Errorf("registered = %d, want 0", n)
	}
}

// (22)
func TestAppendReviewTerminal_ExhaustedNonVerdictOriginal_ReasonNamesOriginal(t *testing.T) {
	for _, tc := range []struct {
		category, reason string
		label            string
	}{
		{"implement_review_skipped", planreview.ReasonPersonaRemitUnavailable, "implement"},
		{"plan_review_failed", "reviewer timeout", "plan"},
	} {
		t.Run(tc.category, func(t *testing.T) {
			f := newFlakyReviewAudit()
			f.appendAction = func(_ int, p audit.ChainAppendParams) flakyAction {
				if strings.Contains(string(p.Payload), reviewVerdictWriteFailedPrefix) {
					return flakyOK
				}
				return flakyFail
			}
			s, _ := newRTServer(t, f)
			runID, stageID := uuid.New(), uuid.New()
			b, _ := json.Marshal(planreview.ReviewFailedPayload{Reason: tc.reason, Authority: planreview.AuthorityAdvisory})
			sk := audit.ActorKind("system")
			p := audit.ChainAppendParams{RunID: runID, StageID: &stageID, Category: tc.category, ActorKind: &sk, Payload: b}
			cfg := rtCfg(rtSteps(1, time.Millisecond), rtSteps(1, time.Hour), timescale.D(time.Second))
			_, _ = s.appendReviewTerminalWith(context.Background(), p, cfg)
			failedCat := tc.label + "_review_failed"
			named := f.namedFailures(t, runID, failedCat)
			if len(named) != 1 {
				t.Fatalf("named failures = %d, want 1", len(named))
			}
			r := named[0].Reason
			if !strings.Contains(r, tc.category) || !strings.Contains(r, tc.reason) {
				t.Errorf("reason %q does not name the original category and reason", r)
			}
			if strings.Contains(r, "verdict ") || strings.Contains(r, "concerns") {
				t.Errorf("reason %q carries a verdict summary for a non-verdict original", r)
			}
		})
	}
}

// (23)
func TestAppendReviewTerminalSingle_TotalOutageReturnsWithinBound(t *testing.T) {
	t.Run("total_outage", func(t *testing.T) {
		f := newFlakyReviewAudit()
		f.appendAction = failAll
		f.listAction = func(int, string) flakyAction { return flakyFail }
		s, _ := newRTServer(t, f)
		runID, stageID := uuid.New(), uuid.New()
		cfg := rtCfg(rtSteps(3, time.Hour), rtSteps(1, timescale.D(300*time.Millisecond)), 20*time.Millisecond)
		start := time.Now()
		_, err := s.appendReviewTerminalSingleWith(context.Background(), planReviewedParams(runID, stageID), cfg)
		appends, lists := f.counts()
		if err == nil {
			t.Fatal("single append succeeded under a total outage")
		}
		if el := time.Since(start); el > timescale.D(2*time.Second) {
			t.Fatalf("single append returned after %s", el)
		}
		if appends != 1 || lists != 1 {
			t.Errorf("appends = %d lists = %d, want exactly 1 and 1", appends, lists)
		}
		if n := registered(runID, "plan"); n != 1 {
			t.Errorf("registered = %d, want 1", n)
		}
		s.waitBackgroundReviews()
	})
	t.Run("ambiguous_commit", func(t *testing.T) {
		f := newFlakyReviewAudit()
		f.appendAction = func(n int, _ audit.ChainAppendParams) flakyAction {
			if n == 1 {
				return flakyCommitThenFail
			}
			return flakyOK
		}
		s, _ := newRTServer(t, f)
		runID, stageID := uuid.New(), uuid.New()
		e, err := s.appendReviewTerminalSingleWith(context.Background(), planReviewedParams(runID, stageID), rtCfg(nil, rtSteps(1, time.Hour), timescale.D(time.Second)))
		if err != nil || e == nil {
			t.Fatalf("single = (%v, %v), want the probe-found entry", e, err)
		}
		if n := len(f.namedFailures(t, runID, "plan_review_failed")); n != 0 {
			t.Errorf("verdict_write_failed rows = %d, want 0", n)
		}
		if n := registered(runID, "plan"); n != 0 {
			t.Errorf("registered = %d, want 0", n)
		}
	})
}

// C3: the pre-round emitter keeps a single best-effort attempt and never
// registers.
func TestEmitPreRoundReviewFailed_SingleAttemptNoRegistry(t *testing.T) {
	f := newFlakyReviewAudit()
	f.appendAction = failAll
	s, _ := newRTServer(t, f)
	runID, stageID := uuid.New(), uuid.New()
	s.emitPreRoundReviewFailed(context.Background(), runID, stageID, "plan_review_failed", planreview.AuthorityGating, "", "document_injection_failed: x", false)
	if appends, lists := f.counts(); appends != 1 || lists != 0 {
		t.Errorf("appends = %d lists = %d, want 1 and 0", appends, lists)
	}
	if n := registered(runID, "plan"); n != 0 {
		t.Errorf("registered = %d, want 0", n)
	}
	s.waitBackgroundReviews()
}

// C4: a 2-reviewer round with BOTH terminals lost settles once the store
// recovers — two named failures, landed == configured_agents.
func TestSettleLostReviewTerminals_TwoReviewersBothLost_RoundSettles(t *testing.T) {
	f := newFlakyReviewAudit()
	f.appendAction = func(_ int, p audit.ChainAppendParams) flakyAction {
		if p.Category == "plan_review_started" {
			return flakyOK
		}
		return flakyFail
	}
	s, _ := newRTServer(t, f)
	runID, stageID := uuid.New(), uuid.New()
	f.seedStarted(runID, stageID, 10, time.Now().UTC().Add(-time.Minute), "plan_review_started", 2)
	cfg := rtCfg(rtSteps(1, time.Millisecond), rtSteps(200, timescale.D(5*time.Millisecond)), timescale.D(time.Second))
	_, _ = s.appendReviewTerminalWith(context.Background(), planReviewedParams(runID, stageID), cfg)
	_, _ = s.appendReviewTerminalWith(context.Background(), planReviewedParams(runID, stageID), cfg)
	if n := registered(runID, "plan"); n != 2 {
		t.Fatalf("registered = %d, want 2 distinct entries", n)
	}
	f.recovered.Store(true)
	s.waitBackgroundReviews()
	if n := len(f.namedFailures(t, runID, "plan_review_failed")); n != 2 {
		t.Errorf("named failures = %d, want 2", n)
	}
	stage, _ := reviewStageKindByLabel("plan")
	landed, err := s.countLandedReviewTerminals(context.Background(), runID, stage, 10)
	if err != nil || landed != 2 {
		t.Errorf("landed = %d (%v), want 2 == configured_agents", landed, err)
	}
	if n := registered(runID, "plan"); n != 0 {
		t.Errorf("registered = %d, want 0", n)
	}
}
