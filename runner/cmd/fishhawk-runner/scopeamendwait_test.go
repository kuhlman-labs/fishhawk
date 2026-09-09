package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/runner/internal/upload"
)

// --- #3320 scope-amendment settle wait ---
//
// TIMING: this module has no timescale package, so every deadline-competing
// duration here derives from the ONE module-local factor via scaledD /
// lockTestScale (lockholder_test.go), per the #1984 rule. Poll intervals stay
// unscaled.

// withShrunkSettleTuning shrinks the settle-wait tuning vars for the duration
// of one test and restores them. The budget is SCALED; the poll interval is
// deliberately not (it is not deadline-competing).
func withShrunkSettleTuning(t *testing.T, budget time.Duration) {
	t.Helper()
	oldBudget, oldWait, oldPoll := scopeAmendmentSettleBudget, scopeAmendmentSettleWaitSeconds, scopeAmendmentSettlePollInterval
	scopeAmendmentSettleBudget = budget
	scopeAmendmentSettleWaitSeconds = 30
	scopeAmendmentSettlePollInterval = 5 * time.Millisecond
	t.Cleanup(func() {
		scopeAmendmentSettleBudget = oldBudget
		scopeAmendmentSettleWaitSeconds = oldWait
		scopeAmendmentSettlePollInterval = oldPoll
	})
}

func settleCfg() config {
	return config{runID: "run-1", stageID: "stage-1"}
}

func settlePendingRow(id, stageID string) upload.ScopeAmendment {
	return upload.ScopeAmendment{
		ID:      id,
		RunID:   "run-1",
		StageID: stageID,
		Status:  "pending",
		Paths:   []upload.ScopeAmendmentPath{{Path: "a.go", Operation: "modify"}},
	}
}

func settleDecidedRow(id, stageID, status string) upload.ScopeAmendment {
	r := settlePendingRow(id, stageID)
	r.Status = status
	return r
}

// settleLogLine returns the first JSONL object in log whose "event" equals
// event, or nil.
func settleLogLine(t *testing.T, log string, event string) map[string]any {
	t.Helper()
	for _, line := range strings.Split(log, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			continue
		}
		if m["event"] == event {
			return m
		}
	}
	return nil
}

// TestAwaitPendingScopeAmendmentSettle_NoOpGuards pins every no-op guard:
// each performs ZERO fetches (asserted on the fake's call counter, not just on
// the nil return) and returns nil.
func TestAwaitPendingScopeAmendmentSettle_NoOpGuards(t *testing.T) {
	cases := []struct {
		name      string
		nilClient bool
		token     string
		stageType string
		stageID   string
		budget    time.Duration
	}{
		{name: "nil client", nilClient: true, token: "fhm_x", stageType: "implement", stageID: "stage-1", budget: scaledD(200 * time.Millisecond)},
		{name: "empty token", token: "", stageType: "implement", stageID: "stage-1", budget: scaledD(200 * time.Millisecond)},
		{name: "non-implement stage", token: "fhm_x", stageType: "plan", stageID: "stage-1", budget: scaledD(200 * time.Millisecond)},
		{name: "empty stage id", token: "fhm_x", stageType: "implement", stageID: "", budget: scaledD(200 * time.Millisecond)},
		{name: "non-positive budget", token: "fhm_x", stageType: "implement", stageID: "stage-1", budget: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withShrunkSettleTuning(t, tc.budget)
			fake := &fakeUploader{amendments: []upload.ScopeAmendment{settlePendingRow("a1", "stage-1")}}
			var client uploadClient = fake
			if tc.nilClient {
				client = nil
			}
			cfg := settleCfg()
			cfg.stageID = tc.stageID
			var log bytes.Buffer

			got := awaitPendingScopeAmendmentSettle(context.Background(), client, cfg, tc.token, tc.stageType, &log)

			if got != nil {
				t.Errorf("events = %v, want nil", got)
			}
			if fake.amendmentCalls != 0 {
				t.Errorf("FetchScopeAmendments calls = %d, want 0 (guard must not fetch)", fake.amendmentCalls)
			}
			if log.Len() != 0 {
				t.Errorf("log = %q, want empty", log.String())
			}
		})
	}
}

// TestAwaitPendingScopeAmendmentSettle_NoPendingForStage_ReturnsImmediately:
// the probe succeeds with nothing pending, so exactly ONE fetch happens, no
// events are returned, and the ordinary stage log is untouched.
func TestAwaitPendingScopeAmendmentSettle_NoPendingForStage_ReturnsImmediately(t *testing.T) {
	withShrunkSettleTuning(t, scaledD(2*time.Second))
	fake := &fakeUploader{amendments: []upload.ScopeAmendment{settleDecidedRow("a1", "stage-1", "approved")}}
	var log bytes.Buffer

	got := awaitPendingScopeAmendmentSettle(context.Background(), fake, settleCfg(), "fhm_x", "implement", &log)

	if got != nil {
		t.Errorf("events = %v, want nil", got)
	}
	if fake.amendmentCalls != 1 {
		t.Errorf("FetchScopeAmendments calls = %d, want exactly 1 (probe only)", fake.amendmentCalls)
	}
	if log.Len() != 0 {
		t.Errorf("log = %q, want empty (no wait_started line)", log.String())
	}
	if fake.gotAmendmentArgs == nil || fake.gotAmendmentArgs.WaitSeconds != 0 {
		t.Errorf("probe WaitSeconds = %v, want 0", fake.gotAmendmentArgs)
	}
}

// TestAwaitPendingScopeAmendmentSettle_PendingBelongsToAnotherStage_NoWait is
// the stage-filter counterfactual vehicle: a pending row owned by an EARLIER
// stage of the same run must never make THIS stage wait.
func TestAwaitPendingScopeAmendmentSettle_PendingBelongsToAnotherStage_NoWait(t *testing.T) {
	withShrunkSettleTuning(t, scaledD(2*time.Second))
	fake := &fakeUploader{amendments: []upload.ScopeAmendment{settlePendingRow("a1", "stage-OTHER")}}
	var log bytes.Buffer

	got := awaitPendingScopeAmendmentSettle(context.Background(), fake, settleCfg(), "fhm_x", "implement", &log)

	if got != nil {
		t.Errorf("events = %v, want nil (another stage's pending row must not make this stage wait)", got)
	}
	if fake.amendmentCalls != 1 {
		t.Errorf("FetchScopeAmendments calls = %d, want exactly 1 (probe only, no wait loop)", fake.amendmentCalls)
	}
	if log.Len() != 0 {
		t.Errorf("log = %q, want empty", log.String())
	}
}

// TestAwaitPendingScopeAmendmentSettle_ProbeError_FailsOpenUnavailable: a plain
// probe error logs scope_amendment_settle_check_failed with outcome=unavailable
// and returns nil, never entering the loop.
func TestAwaitPendingScopeAmendmentSettle_ProbeError_FailsOpenUnavailable(t *testing.T) {
	withShrunkSettleTuning(t, scaledD(2*time.Second))
	fake := &fakeUploader{amendmentsErr: errors.New("boom")}
	var log bytes.Buffer

	got := awaitPendingScopeAmendmentSettle(context.Background(), fake, settleCfg(), "fhm_x", "implement", &log)

	if got != nil {
		t.Errorf("events = %v, want nil", got)
	}
	line := settleLogLine(t, log.String(), "scope_amendment_settle_check_failed")
	if line == nil {
		t.Fatalf("no scope_amendment_settle_check_failed line in %q", log.String())
	}
	if line["outcome"] != "unavailable" {
		t.Errorf("outcome = %v, want unavailable", line["outcome"])
	}
	if settleLogLine(t, log.String(), "scope_amendment_settle_wait_started") != nil {
		t.Error("wait_started logged, want none (probe failed before loop entry)")
	}
	if fake.amendmentCalls != 1 {
		t.Errorf("FetchScopeAmendments calls = %d, want exactly 1", fake.amendmentCalls)
	}
}

// TestAwaitPendingScopeAmendmentSettle_ProbeBlocksPastBudget_BoundedAndFailsOpen
// is the REVISION-1 pin: the settle budget bounds the WHOLE function, the
// INITIAL PROBE INCLUDED. The fake's first FetchScopeAmendments honors its
// context and returns only when that context expires; if the deadline were
// established only AFTER the probe, the probe would run on the bare parent
// context and the call would outlast the bound.
func TestAwaitPendingScopeAmendmentSettle_ProbeBlocksPastBudget_BoundedAndFailsOpen(t *testing.T) {
	budget := scaledD(150 * time.Millisecond)
	withShrunkSettleTuning(t, budget)
	// The hold is far longer than the budget: only the entry-computed deadline
	// can end it.
	hold := scaledD(5 * time.Second)
	fake := &fakeUploader{
		amendments: []upload.ScopeAmendment{settlePendingRow("a1", "stage-1")},
		amendmentsHook: func(ctx context.Context, _ upload.FetchScopeAmendmentsArgs) error {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(hold):
				return nil
			}
		},
	}
	var log bytes.Buffer

	start := time.Now()
	got := awaitPendingScopeAmendmentSettle(context.Background(), fake, settleCfg(), "fhm_x", "implement", &log)
	elapsed := time.Since(start)

	// Upper bound derived from the SAME scale factor as the budget, so the
	// discrimination ratio (bound/budget = 4x) holds at any factor.
	upper := scaledD(600 * time.Millisecond)
	if elapsed > upper {
		t.Errorf("call took %s, want <= %s (budget %s must bound the probe too)", elapsed, upper, budget)
	}
	if got != nil {
		t.Errorf("events = %v, want nil (fail-open)", got)
	}
	line := settleLogLine(t, log.String(), "scope_amendment_settle_check_failed")
	if line == nil {
		t.Fatalf("no scope_amendment_settle_check_failed line in %q", log.String())
	}
	if line["outcome"] == "decided" {
		t.Errorf("outcome = decided, want a NON-decided outcome")
	}
	if line["outcome"] != "timeout" {
		t.Errorf("outcome = %v, want timeout (our derived deadline ended the probe)", line["outcome"])
	}
	if settleLogLine(t, log.String(), "scope_amendment_settle_wait_started") != nil {
		t.Error("wait_started logged, want none")
	}
	if settleLogLine(t, log.String(), "scope_amendment_settle_wait_ended") != nil {
		t.Error("wait_ended logged, want none")
	}
}

// TestAwaitPendingScopeAmendmentSettle_ApprovedMidWait_OutcomeDecided: pending
// on the probe, approved on the next poll → outcome=decided with the
// wait_started/wait_ended pair and a policy_event.
func TestAwaitPendingScopeAmendmentSettle_ApprovedMidWait_OutcomeDecided(t *testing.T) {
	withShrunkSettleTuning(t, scaledD(5*time.Second))
	fake := &fakeUploader{amendmentsSeq: [][]upload.ScopeAmendment{
		{settlePendingRow("a1", "stage-1")},
		{settleDecidedRow("a1", "stage-1", "approved")},
	}}
	var log bytes.Buffer

	got := awaitPendingScopeAmendmentSettle(context.Background(), fake, settleCfg(), "fhm_x", "implement", &log)

	if len(got) != 1 {
		t.Fatalf("events = %v, want exactly 1 policy_event", got)
	}
	if settleLogLine(t, log.String(), "scope_amendment_settle_wait_started") == nil {
		t.Errorf("no wait_started line in %q", log.String())
	}
	ended := settleLogLine(t, log.String(), "scope_amendment_settle_wait_ended")
	if ended == nil {
		t.Fatalf("no wait_ended line in %q", log.String())
	}
	if ended["outcome"] != "decided" {
		t.Errorf("outcome = %v, want decided", ended["outcome"])
	}
}

// TestAwaitPendingScopeAmendmentSettle_DeniedMidWait_OutcomeDecided: a DENIAL
// is a decision — it ends the wait promptly rather than burning the budget.
func TestAwaitPendingScopeAmendmentSettle_DeniedMidWait_OutcomeDecided(t *testing.T) {
	budget := scaledD(5 * time.Second)
	withShrunkSettleTuning(t, budget)
	fake := &fakeUploader{amendmentsSeq: [][]upload.ScopeAmendment{
		{settlePendingRow("a1", "stage-1")},
		{settleDecidedRow("a1", "stage-1", "denied")},
	}}
	var log bytes.Buffer

	start := time.Now()
	got := awaitPendingScopeAmendmentSettle(context.Background(), fake, settleCfg(), "fhm_x", "implement", &log)
	elapsed := time.Since(start)

	if len(got) != 1 {
		t.Fatalf("events = %v, want exactly 1 policy_event", got)
	}
	ended := settleLogLine(t, log.String(), "scope_amendment_settle_wait_ended")
	if ended == nil {
		t.Fatalf("no wait_ended line in %q", log.String())
	}
	if ended["outcome"] != "decided" {
		t.Errorf("outcome = %v, want decided (a denial IS a decision)", ended["outcome"])
	}
	// It must not have burned the budget waiting on an answer it already has.
	if elapsed >= budget {
		t.Errorf("call took %s, want well under the %s budget", elapsed, budget)
	}
}

// TestAwaitPendingScopeAmendmentSettle_BudgetExpires_OutcomeTimeout: the row
// stays pending on every fetch, so the budget expires — outcome=timeout, and
// the call returns within a scaled upper bound.
func TestAwaitPendingScopeAmendmentSettle_BudgetExpires_OutcomeTimeout(t *testing.T) {
	budget := scaledD(150 * time.Millisecond)
	withShrunkSettleTuning(t, budget)
	fake := &fakeUploader{amendments: []upload.ScopeAmendment{settlePendingRow("a1", "stage-1")}}
	var log bytes.Buffer

	start := time.Now()
	got := awaitPendingScopeAmendmentSettle(context.Background(), fake, settleCfg(), "fhm_x", "implement", &log)
	elapsed := time.Since(start)

	upper := scaledD(600 * time.Millisecond)
	if elapsed > upper {
		t.Errorf("call took %s, want <= %s", elapsed, upper)
	}
	if len(got) != 1 {
		t.Fatalf("events = %v, want exactly 1 policy_event", got)
	}
	ended := settleLogLine(t, log.String(), "scope_amendment_settle_wait_ended")
	if ended == nil {
		t.Fatalf("no wait_ended line in %q", log.String())
	}
	if ended["outcome"] != "timeout" {
		t.Errorf("outcome = %v, want timeout", ended["outcome"])
	}
}

// TestAwaitPendingScopeAmendmentSettle_MidWaitFetchError_OutcomeUnavailable: a
// fetch error INSIDE the loop (the probe succeeded) classifies unavailable and
// returns promptly, well inside the budget.
func TestAwaitPendingScopeAmendmentSettle_MidWaitFetchError_OutcomeUnavailable(t *testing.T) {
	budget := scaledD(5 * time.Second)
	withShrunkSettleTuning(t, budget)
	fake := &fakeUploader{amendments: []upload.ScopeAmendment{settlePendingRow("a1", "stage-1")}}
	fake.amendmentsHook = func(_ context.Context, _ upload.FetchScopeAmendmentsArgs) error {
		if fake.amendmentCalls >= 2 {
			return errors.New("backend down")
		}
		return nil
	}
	var log bytes.Buffer

	start := time.Now()
	got := awaitPendingScopeAmendmentSettle(context.Background(), fake, settleCfg(), "fhm_x", "implement", &log)
	elapsed := time.Since(start)

	if len(got) != 1 {
		t.Fatalf("events = %v, want exactly 1 policy_event", got)
	}
	ended := settleLogLine(t, log.String(), "scope_amendment_settle_wait_ended")
	if ended == nil {
		t.Fatalf("no wait_ended line in %q", log.String())
	}
	if ended["outcome"] != "unavailable" {
		t.Errorf("outcome = %v, want unavailable", ended["outcome"])
	}
	if elapsed >= budget {
		t.Errorf("call took %s, want well under the %s budget (an error returns promptly)", elapsed, budget)
	}
}

// TestAwaitPendingScopeAmendmentSettle_ParentContextCancelled_OutcomeUnavailable:
// the parent context is cancelled while a request is genuinely IN FLIGHT (the
// hook holds the way the backend's `?wait` does), which must classify
// unavailable — not timeout, which would report a healthy backend's cancelled
// stage as an expired settle budget.
func TestAwaitPendingScopeAmendmentSettle_ParentContextCancelled_OutcomeUnavailable(t *testing.T) {
	withShrunkSettleTuning(t, scaledD(10*time.Second))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fake := &fakeUploader{amendments: []upload.ScopeAmendment{settlePendingRow("a1", "stage-1")}}
	fake.amendmentsHook = func(hookCtx context.Context, _ upload.FetchScopeAmendmentsArgs) error {
		if fake.amendmentCalls >= 2 {
			cancel()
			<-hookCtx.Done()
			return hookCtx.Err()
		}
		return nil
	}
	var log bytes.Buffer

	got := awaitPendingScopeAmendmentSettle(ctx, fake, settleCfg(), "fhm_x", "implement", &log)

	if len(got) != 1 {
		t.Fatalf("events = %v, want exactly 1 policy_event", got)
	}
	ended := settleLogLine(t, log.String(), "scope_amendment_settle_wait_ended")
	if ended == nil {
		t.Fatalf("no wait_ended line in %q", log.String())
	}
	if ended["outcome"] != "unavailable" {
		t.Errorf("outcome = %v, want unavailable (parent cancellation, not our budget)", ended["outcome"])
	}
}

// TestBoundedWaitSeconds pins the shared `?wait=N` bound at the cap, below the
// cap, at the sub-second floor, and at a non-positive remainder.
func TestBoundedWaitSeconds(t *testing.T) {
	cases := []struct {
		name      string
		remaining time.Duration
		cap       int
		want      int
	}{
		{name: "above the cap clamps to the cap", remaining: 500 * time.Second, cap: 30, want: 30},
		{name: "exactly the cap", remaining: 30 * time.Second, cap: 30, want: 30},
		{name: "below the cap floors to whole seconds", remaining: 7500 * time.Millisecond, cap: 30, want: 7},
		{name: "sub-second floors to zero", remaining: 900 * time.Millisecond, cap: 30, want: 0},
		{name: "zero remainder", remaining: 0, cap: 30, want: 0},
		{name: "negative remainder", remaining: -5 * time.Second, cap: 30, want: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := boundedWaitSeconds(tc.remaining, tc.cap); got != tc.want {
				t.Errorf("boundedWaitSeconds(%s, %d) = %d, want %d", tc.remaining, tc.cap, got, tc.want)
			}
		})
	}
}

// TestEmitScopeAmendmentSettle_SeamContract pins the LITERAL JSONL field sets
// per the #618 both-ends rule: the runner writes them and a log reader (an
// operator, or a future fishhawk-mcp relay) reads them by name, so a rename is
// a silent break.
func TestEmitScopeAmendmentSettle_SeamContract(t *testing.T) {
	withShrunkSettleTuning(t, scaledD(5*time.Second))

	// _started + _ended
	fake := &fakeUploader{amendmentsSeq: [][]upload.ScopeAmendment{
		{settlePendingRow("amd-7", "stage-1")},
		{settleDecidedRow("amd-7", "stage-1", "approved")},
	}}
	var log bytes.Buffer
	awaitPendingScopeAmendmentSettle(context.Background(), fake, settleCfg(), "fhm_x", "implement", &log)

	started := settleLogLine(t, log.String(), "scope_amendment_settle_wait_started")
	if started == nil {
		t.Fatalf("no wait_started line in %q", log.String())
	}
	assertKeys(t, "wait_started", started, []string{"event", "run_id", "stage_id", "amendments", "budget_seconds"})
	if started["run_id"] != "run-1" || started["stage_id"] != "stage-1" {
		t.Errorf("wait_started ids = %v/%v, want run-1/stage-1", started["run_id"], started["stage_id"])
	}
	ids, ok := started["amendments"].([]any)
	if !ok || len(ids) != 1 || ids[0] != "amd-7" {
		t.Errorf("wait_started amendments = %v, want [amd-7]", started["amendments"])
	}
	if _, ok := started["budget_seconds"].(float64); !ok {
		t.Errorf("budget_seconds = %v, want a number", started["budget_seconds"])
	}

	ended := settleLogLine(t, log.String(), "scope_amendment_settle_wait_ended")
	if ended == nil {
		t.Fatalf("no wait_ended line in %q", log.String())
	}
	assertKeys(t, "wait_ended", ended, []string{"event", "run_id", "stage_id", "outcome", "waited_ms", "amendments"})
	if _, ok := ended["waited_ms"].(float64); !ok {
		t.Errorf("waited_ms = %v, want a number", ended["waited_ms"])
	}

	// _check_failed
	failFake := &fakeUploader{amendmentsErr: errors.New("boom")}
	var failLog bytes.Buffer
	awaitPendingScopeAmendmentSettle(context.Background(), failFake, settleCfg(), "fhm_x", "implement", &failLog)
	failed := settleLogLine(t, failLog.String(), "scope_amendment_settle_check_failed")
	if failed == nil {
		t.Fatalf("no check_failed line in %q", failLog.String())
	}
	assertKeys(t, "check_failed", failed, []string{"event", "run_id", "stage_id", "outcome", "detail"})
}

func assertKeys(t *testing.T, label string, got map[string]any, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s field count = %d (%v), want %d (%v)", label, len(got), got, len(want), want)
	}
	for _, k := range want {
		if _, ok := got[k]; !ok {
			t.Errorf("%s missing field %q (got %v)", label, k, got)
		}
	}
}

// TestAwaitPendingScopeAmendmentSettle_PolicyEventShape asserts the returned
// agent.Event decodes to the payload the trace bundle records, so the bundle
// record cannot silently drift.
func TestAwaitPendingScopeAmendmentSettle_PolicyEventShape(t *testing.T) {
	withShrunkSettleTuning(t, scaledD(5*time.Second))
	fake := &fakeUploader{amendmentsSeq: [][]upload.ScopeAmendment{
		{settlePendingRow("amd-7", "stage-1")},
		{settleDecidedRow("amd-7", "stage-1", "approved")},
	}}
	var log bytes.Buffer

	got := awaitPendingScopeAmendmentSettle(context.Background(), fake, settleCfg(), "fhm_x", "implement", &log)

	if len(got) != 1 {
		t.Fatalf("events = %v, want exactly 1", got)
	}
	if got[0].Kind != "policy_event" {
		t.Errorf("Kind = %q, want policy_event", got[0].Kind)
	}
	raw, err := json.Marshal(got[0].Payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	var p struct {
		Check      string   `json:"check"`
		Outcome    string   `json:"outcome"`
		Amendments []string `json:"amendments"`
		WaitedMS   *int64   `json:"waited_ms"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("unmarshal payload %s: %v", raw, err)
	}
	if p.Check != "scope_amendment_settle_wait" {
		t.Errorf("check = %q, want scope_amendment_settle_wait", p.Check)
	}
	if p.Outcome != "decided" {
		t.Errorf("outcome = %q, want decided", p.Outcome)
	}
	if len(p.Amendments) != 1 || p.Amendments[0] != "amd-7" {
		t.Errorf("amendments = %v, want [amd-7]", p.Amendments)
	}
	if p.WaitedMS == nil {
		t.Errorf("waited_ms missing from payload %s", raw)
	}
}
