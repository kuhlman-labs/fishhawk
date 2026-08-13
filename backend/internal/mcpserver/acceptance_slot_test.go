package mcpserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

// shrinkSlotProbeTimeout shrinks acceptanceSlotProbeTimeout for the duration of a
// test so an unreachable/black-hole probe never waits on wall clock, restoring it
// after. Not parallel-safe (it mutates a package var), so callers must not
// t.Parallel().
func shrinkSlotProbeTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	prev := acceptanceSlotProbeTimeout
	acceptanceSlotProbeTimeout = d
	t.Cleanup(func() { acceptanceSlotProbeTimeout = prev })
}

// --- resolveAcceptanceSlotHost (env → default ladder) ---

func TestResolveAcceptanceSlotHost(t *testing.T) {
	t.Run("env override wins", func(t *testing.T) {
		host, source := resolveAcceptanceSlotHost(envFuncFromMap(map[string]string{
			acceptancePreviewAddrEnv: "preview.example:9000",
		}))
		if host != "preview.example:9000" || source != "env" {
			t.Errorf("host,source = %q,%q; want preview.example:9000,env", host, source)
		}
	})
	t.Run("whitespace-only env falls to default", func(t *testing.T) {
		host, source := resolveAcceptanceSlotHost(envFuncFromMap(map[string]string{
			acceptancePreviewAddrEnv: "   ",
		}))
		if host != acceptanceSlotDefaultHost || source != "default" {
			t.Errorf("host,source = %q,%q; want %s,default", host, source, acceptanceSlotDefaultHost)
		}
	})
	t.Run("unset env falls to the localhost:8090 default", func(t *testing.T) {
		host, source := resolveAcceptanceSlotHost(envFuncFromMap(map[string]string{}))
		if host != "localhost:8090" || source != "default" {
			t.Errorf("host,source = %q,%q; want localhost:8090,default", host, source)
		}
	})
	t.Run("nil getenv falls to default (no panic)", func(t *testing.T) {
		host, source := resolveAcceptanceSlotHost(nil)
		if host != acceptanceSlotDefaultHost || source != "default" {
			t.Errorf("host,source = %q,%q; want %s,default", host, source, acceptanceSlotDefaultHost)
		}
	})
}

// --- probeAcceptanceSlotHealth (one case per named branch) ---

func TestProbeAcceptanceSlotHealth_Serving(t *testing.T) {
	ts := healthzServer(t, http.StatusOK, `{"git_sha":"abc1234def"}`)
	outcome, sha, detail := probeAcceptanceSlotHealth(context.Background(), hostOf(ts.URL))
	if outcome != slotProbeServing {
		t.Fatalf("outcome = %v, want serving (%s)", outcome, detail)
	}
	if sha != "abc1234def" {
		t.Errorf("gitSHA = %q, want abc1234def", sha)
	}
}

// TestProbeAcceptanceSlotHealth_ConnectionRefused pins the ONE positive free
// signal (condition 1): a connection refused from a REACHABLE host (a closed
// loopback port) classifies as slotProbeRefused. A closed httptest server gives a
// deterministic ECONNREFUSED on loopback.
func TestProbeAcceptanceSlotHealth_ConnectionRefused(t *testing.T) {
	shrinkSlotProbeTimeout(t, 200*time.Millisecond)
	ts := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	host := hostOf(ts.URL)
	ts.Close() // now the loopback port is reachable-but-closed → connection refused
	outcome, _, detail := probeAcceptanceSlotHealth(context.Background(), host)
	if outcome != slotProbeRefused {
		t.Fatalf("outcome = %v, want refused (a connection refused is the positive free signal); detail=%s", outcome, detail)
	}
}

// TestProbeAcceptanceSlotHealth_BlackholeDialFailure pins condition 1's fail-shut
// half: a dial failure that is NOT a connection refused (a black-holed /
// unroutable target) classifies slotProbeUnreachable, so the state mapping reports
// unverifiable and never free. 192.0.2.1 is TEST-NET-1 (RFC 5737), guaranteed
// unroutable; the shrunk timeout bounds the wait.
func TestProbeAcceptanceSlotHealth_BlackholeDialFailure(t *testing.T) {
	shrinkSlotProbeTimeout(t, 150*time.Millisecond)
	outcome, _, detail := probeAcceptanceSlotHealth(context.Background(), "192.0.2.1:8090")
	if outcome != slotProbeUnreachable {
		t.Fatalf("outcome = %v, want unreachable (a non-refused dial failure cannot decide free); detail=%s", outcome, detail)
	}
}

func TestProbeAcceptanceSlotHealth_ReachableNoIdentity(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"non-200", http.StatusServiceUnavailable, `{"git_sha":"abc1234def"}`},
		{"non-JSON body", http.StatusOK, `not json`},
		{"missing git_sha", http.StatusOK, `{"status":"ok"}`},
		{"unknown git_sha", http.StatusOK, `{"git_sha":"unknown"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := healthzServer(t, tc.status, tc.body)
			outcome, _, detail := probeAcceptanceSlotHealth(context.Background(), hostOf(ts.URL))
			if outcome != slotProbeReachableNoIdentity {
				t.Fatalf("outcome = %v, want reachableNoIdentity (%s)", outcome, detail)
			}
		})
	}
}

// --- acceptanceSlotFor classification (stage-state → holder/waiter) ---

// newSlotResolver builds a runResolver whose api points at fb's server and whose
// getenv resolves the preview host from env (the getenv seam, condition 4 — no
// t.Setenv, so these tests stay parallel-safe modulo the probe-timeout var).
func newSlotResolver(srv *httptest.Server, env map[string]string) *runResolver {
	return newResolver(srv, env)
}

// seedItemWithAcceptanceStage adds a running/pending campaign item to fb: it mints
// a run id, seeds an acceptance stage in that run at stageState, and returns the
// CampaignItem to hand to acceptanceSlotFor.
func seedItemWithAcceptanceStage(t *testing.T, fb *fakeBackend, issueRef, itemState, stageState string) (CampaignItem, uuid.UUID) {
	t.Helper()
	runID := uuid.New()
	fb.stagesByRun[runID] = []Stage{
		{ID: uuid.NewString(), RunID: runID.String(), Sequence: 1, Type: "plan", State: "succeeded"},
		{ID: uuid.NewString(), RunID: runID.String(), Sequence: 3, Type: "acceptance", State: stageState},
	}
	return CampaignItem{ID: uuid.NewString(), IssueRef: issueRef, RunID: runID.String(), State: itemState}, runID
}

func TestAcceptanceSlotFor_Classification(t *testing.T) {
	cases := []struct {
		name       string
		stageState string
		wantHeld   bool
		wantWait   bool
	}{
		{"dispatched → holds", "dispatched", true, false},
		{"running → holds", "running", true, false},
		{"awaiting_host_dispatch → waits", "awaiting_host_dispatch", false, true},
		{"pending → waits", "pending", false, true},
		{"succeeded → neither", "succeeded", false, false},
		{"failed → neither", "failed", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			shrinkSlotProbeTimeout(t, 200*time.Millisecond)
			fb, srv := newFakeBackend(t)
			// A live /healthz serving a git_sha so a "holds" case reports held; a
			// "neither" case short-circuits before probing regardless.
			hz := healthzServer(t, http.StatusOK, `{"git_sha":"abc1234def"}`)
			item, _ := seedItemWithAcceptanceStage(t, fb, "#26", "running", tc.stageState)
			r := newSlotResolver(srv, map[string]string{acceptancePreviewAddrEnv: hostOf(hz.URL)})

			slot := r.acceptanceSlotFor(context.Background(), []CampaignItem{item})

			if !tc.wantHeld && !tc.wantWait {
				if slot != nil {
					t.Fatalf("non-participant stage %q should omit the block, got %+v", tc.stageState, slot)
				}
				return
			}
			if slot == nil {
				t.Fatalf("participant stage %q should assemble a block, got nil", tc.stageState)
			}
			if tc.wantHeld {
				if slot.HeldBy == nil || slot.HeldBy.IssueRef != "#26" {
					t.Errorf("HeldBy = %+v, want issue #26", slot.HeldBy)
				}
				if slot.HeldBy != nil && slot.HeldBy.StageState != tc.stageState {
					t.Errorf("HeldBy.StageState = %q, want %q", slot.HeldBy.StageState, tc.stageState)
				}
			}
			if tc.wantWait {
				if len(slot.Waiting) != 1 || slot.Waiting[0].IssueRef != "#26" {
					t.Errorf("Waiting = %+v, want one entry for #26", slot.Waiting)
				}
			}
		})
	}
}

// TestAcceptanceSlotFor_NoAcceptanceStage_OmitsBlock proves a candidate whose run
// has NO acceptance stage is not a participant, so the block is omitted.
func TestAcceptanceSlotFor_NoAcceptanceStage_OmitsBlock(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	fb.stagesByRun[runID] = []Stage{
		{ID: uuid.NewString(), RunID: runID.String(), Sequence: 1, Type: "plan", State: "running"},
	}
	item := CampaignItem{ID: uuid.NewString(), IssueRef: "#26", RunID: runID.String(), State: "running"}
	r := newSlotResolver(srv, nil)
	if slot := r.acceptanceSlotFor(context.Background(), []CampaignItem{item}); slot != nil {
		t.Fatalf("a run with no acceptance stage should omit the block, got %+v", slot)
	}
}

// --- State-mapping counterfactuals (conditions 1, 2, 6) ---

// TestAcceptanceSlotFor_HeldWithoutLiveStage covers the #2488 evidence case: the
// probe serves a git_sha (slot bound) but no live acceptance stage holds it (a
// settled sibling still binds the preview). State is held with a nil HeldBy.
func TestAcceptanceSlotFor_HeldWithoutLiveStage(t *testing.T) {
	shrinkSlotProbeTimeout(t, 200*time.Millisecond)
	fb, srv := newFakeBackend(t)
	hz := healthzServer(t, http.StatusOK, `{"git_sha":"cafebabe123"}`)
	// One waiter keeps the campaign a participant (so we probe) while no item holds.
	item, _ := seedItemWithAcceptanceStage(t, fb, "#26", "running", "pending")
	r := newSlotResolver(srv, map[string]string{acceptancePreviewAddrEnv: hostOf(hz.URL)})

	slot := r.acceptanceSlotFor(context.Background(), []CampaignItem{item})
	if slot == nil {
		t.Fatal("expected a block (a waiter is present)")
	}
	if slot.State != "held" || slot.ServingGitSHA != "cafebabe123" {
		t.Fatalf("state,sha = %q,%q; want held,cafebabe123", slot.State, slot.ServingGitSHA)
	}
	if slot.HeldBy != nil {
		t.Errorf("HeldBy = %+v, want nil (no live acceptance stage holds the served slot)", slot.HeldBy)
	}
}

// TestAcceptanceSlotFor_DialFailure_IsUnverifiableNotFree is condition 6's first
// counterfactual: a black-holed target (a non-refused dial failure) yields
// unverifiable, NEVER free. Deleting the slotProbeUnreachable branch in
// acceptanceSlotProbeOnce (folding it into slotProbeRefused) reddens this on the
// state assertion.
func TestAcceptanceSlotFor_DialFailure_IsUnverifiableNotFree(t *testing.T) {
	shrinkSlotProbeTimeout(t, 150*time.Millisecond)
	fb, srv := newFakeBackend(t)
	// One waiter makes the campaign a participant so it probes; the probe target
	// is unroutable (TEST-NET-1) so the dial fails WITHOUT a connection refused.
	item, _ := seedItemWithAcceptanceStage(t, fb, "#26", "running", "pending")
	r := newSlotResolver(srv, map[string]string{acceptancePreviewAddrEnv: "192.0.2.1:8090"})

	slot := r.acceptanceSlotFor(context.Background(), []CampaignItem{item})
	if slot == nil {
		t.Fatal("expected a block (a waiter is present)")
	}
	if slot.State != "unverifiable" {
		t.Fatalf("state = %q, want unverifiable (a dial failure must not report free); detail=%s", slot.State, slot.Detail)
	}
}

// TestAcceptanceSlotFor_ProbeVsStageDisagreement_IsUnverifiable is condition 6's
// second counterfactual: the probe says free (connection refused) while stage
// state names a live holder — a self-contradiction resolved to unverifiable,
// never free (condition 2). Deleting the `if heldBy != nil` guard in the
// slotProbeRefused arm reddens this on the state assertion.
func TestAcceptanceSlotFor_ProbeVsStageDisagreement_IsUnverifiable(t *testing.T) {
	shrinkSlotProbeTimeout(t, 200*time.Millisecond)
	fb, srv := newFakeBackend(t)
	// A reachable-but-closed loopback port → connection refused → probe says free.
	closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	refusedHost := hostOf(closed.URL)
	closed.Close()
	// Stage state names a live holder (running acceptance stage).
	item, _ := seedItemWithAcceptanceStage(t, fb, "#26", "running", "running")
	r := newSlotResolver(srv, map[string]string{acceptancePreviewAddrEnv: refusedHost})

	slot := r.acceptanceSlotFor(context.Background(), []CampaignItem{item})
	if slot == nil {
		t.Fatal("expected a block (a holder is present)")
	}
	if slot.State != "unverifiable" {
		t.Fatalf("state = %q, want unverifiable (probe free vs stage-named holder is a contradiction, never free); detail=%s", slot.State, slot.Detail)
	}
	if slot.HeldBy == nil || slot.HeldBy.IssueRef != "#26" {
		t.Errorf("HeldBy = %+v, want the stage-named holder #26 preserved for the operator", slot.HeldBy)
	}
}

// TestAcceptanceSlotFor_Free is the positive free path: a connection refused from
// a reachable host with NO stage-named holder reports free.
func TestAcceptanceSlotFor_Free(t *testing.T) {
	shrinkSlotProbeTimeout(t, 200*time.Millisecond)
	fb, srv := newFakeBackend(t)
	closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	refusedHost := hostOf(closed.URL)
	closed.Close()
	// A waiter (not a holder) keeps the campaign a participant; no holder exists.
	item, _ := seedItemWithAcceptanceStage(t, fb, "#26", "running", "pending")
	r := newSlotResolver(srv, map[string]string{acceptancePreviewAddrEnv: refusedHost})

	slot := r.acceptanceSlotFor(context.Background(), []CampaignItem{item})
	if slot == nil {
		t.Fatal("expected a block (a waiter is present)")
	}
	if slot.State != "free" {
		t.Fatalf("state = %q, want free (connection refused, no stage-named holder); detail=%s", slot.State, slot.Detail)
	}
}

// --- Bound / degrade controls (plan counterfactuals) ---

// slotHostFromCountingHealthz stands up a /healthz that counts requests and serves
// a git_sha, returning the host and the counter pointer. Used by the no-probe
// short-circuit counterfactual: the target is a REACHABLE in-test server so
// deleting the short-circuit cannot fail for the same reason the control would.
func slotHostFromCountingHealthz(t *testing.T, count *int64) string {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(count, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"git_sha":"abc1234def"}`))
	}))
	t.Cleanup(ts.Close)
	return hostOf(ts.URL)
}

// TestAcceptanceSlot_NoParticipant_IssuesNoProbe pins the no-candidates
// short-circuit control: a campaign whose items carry NO acceptance stage issues
// ZERO probe requests and omits the block. The bad state is seeded BY
// CONSTRUCTION (an item run with only a plan stage), and the /healthz target is a
// REACHABLE counting server, so deleting the short-circuit reddens the request
// counter — not a fixture-setup failure.
func TestAcceptanceSlot_NoParticipant_IssuesNoProbe(t *testing.T) {
	shrinkSlotProbeTimeout(t, 200*time.Millisecond)
	fb, srv := newFakeBackend(t)
	var probeCount int64
	host := slotHostFromCountingHealthz(t, &probeCount)

	// An item whose run has only a non-acceptance stage → not a participant.
	runID := uuid.New()
	fb.stagesByRun[runID] = []Stage{
		{ID: uuid.NewString(), RunID: runID.String(), Sequence: 1, Type: "plan", State: "running"},
	}
	item := CampaignItem{ID: uuid.NewString(), IssueRef: "#26", RunID: runID.String(), State: "running"}
	r := newSlotResolver(srv, map[string]string{acceptancePreviewAddrEnv: host})

	slot := r.acceptanceSlotFor(context.Background(), []CampaignItem{item})
	if slot != nil {
		t.Errorf("no participant should omit the block, got %+v", slot)
	}
	if got := atomic.LoadInt64(&probeCount); got != 0 {
		t.Fatalf("probe requests = %d, want 0 (a campaign nowhere near acceptance must issue no probe)", got)
	}
}

// TestAcceptanceSlot_PerItemStageReadFailure_Degrades pins the best-effort degrade
// control: a per-item ListRunStages 500 skips that item while the block still
// assembles from the surviving items. Deleting the `if lerr != nil { continue }`
// degrade makes the read error propagate and reddens this.
func TestAcceptanceSlot_PerItemStageReadFailure_Degrades(t *testing.T) {
	shrinkSlotProbeTimeout(t, 200*time.Millisecond)
	fb, srv := newFakeBackend(t)
	hz := healthzServer(t, http.StatusOK, `{"git_sha":"abc1234def"}`)

	// Item A: its stage read 500s (degraded away).
	failItem, failRun := seedItemWithAcceptanceStage(t, fb, "#26", "running", "running")
	fb.stagesStatusByRun[failRun] = http.StatusInternalServerError
	// Item B: a healthy running acceptance stage — the surviving holder.
	okItem, _ := seedItemWithAcceptanceStage(t, fb, "#27", "running", "running")

	r := newSlotResolver(srv, map[string]string{acceptancePreviewAddrEnv: hostOf(hz.URL)})
	slot := r.acceptanceSlotFor(context.Background(), []CampaignItem{failItem, okItem})
	if slot == nil {
		t.Fatal("the block must still assemble from the surviving item")
	}
	if slot.HeldBy == nil || slot.HeldBy.IssueRef != "#27" {
		t.Fatalf("HeldBy = %+v, want the surviving item #27 (the failed read must degrade, not propagate)", slot.HeldBy)
	}
	// The degraded item is recorded in read_failures, so the block is honestly
	// PARTIAL rather than silently dropping it. This is the observable effect of
	// the degrade guard: deleting the guard (the append+continue) removes the ref
	// here (the failed read then falls through to the acc==nil skip, unrecorded).
	if len(slot.ReadFailures) != 1 || slot.ReadFailures[0] != "#26" {
		t.Fatalf("ReadFailures = %+v, want [#26] (the degraded item is recorded, not silently dropped)", slot.ReadFailures)
	}
}

// TestAcceptanceSlot_CandidateCap_TruncatesAndReports pins the cap control: with
// more candidate items than acceptanceSlotMaxItemReads, exactly the cap many stage
// reads are issued, truncated is true, and items_inspected equals the cap.
// Deleting the cap makes all reads issue and truncated stay false — reddening both
// the read-count and the truncated assertions.
func TestAcceptanceSlot_CandidateCap_TruncatesAndReports(t *testing.T) {
	shrinkSlotProbeTimeout(t, 200*time.Millisecond)
	fb, srv := newFakeBackend(t)
	hz := healthzServer(t, http.StatusOK, `{"git_sha":"abc1234def"}`)

	extra := 3
	total := acceptanceSlotMaxItemReads + extra
	items := make([]CampaignItem, 0, total)
	runIDs := make([]uuid.UUID, 0, total)
	for i := 0; i < total; i++ {
		// All pending (waiters) so the block assembles but the read count is what
		// matters; distinct run ids so the cap governs the fan-out.
		it, runID := seedItemWithAcceptanceStage(t, fb, "#"+uuid.NewString()[:4], "running", "pending")
		items = append(items, it)
		runIDs = append(runIDs, runID)
	}
	r := newSlotResolver(srv, map[string]string{acceptancePreviewAddrEnv: hostOf(hz.URL)})

	slot := r.acceptanceSlotFor(context.Background(), items)
	if slot == nil {
		t.Fatal("expected a block")
	}
	if !slot.Truncated {
		t.Errorf("Truncated = false, want true (more candidates than the cap)")
	}
	if slot.ItemsInspected != acceptanceSlotMaxItemReads {
		t.Errorf("ItemsInspected = %d, want %d", slot.ItemsInspected, acceptanceSlotMaxItemReads)
	}
	if slot.InspectionLimit != acceptanceSlotMaxItemReads {
		t.Errorf("InspectionLimit = %d, want %d", slot.InspectionLimit, acceptanceSlotMaxItemReads)
	}

	// Exactly the cap many stage reads were issued: count the run ids whose
	// GET /v0/runs/{id}/stages was called. The items beyond the cap must be
	// untouched.
	fb.mu.Lock()
	defer fb.mu.Unlock()
	called := 0
	for _, id := range runIDs {
		if fb.stagesCalledByID[id] > 0 {
			called++
		}
	}
	if called != acceptanceSlotMaxItemReads {
		t.Errorf("stage reads issued = %d, want exactly the cap %d", called, acceptanceSlotMaxItemReads)
	}
}

// TestAcceptanceSlot_TruncatedNoParticipant_SurfacesUnverifiable pins the #2503
// high-concern edge path: MORE than the cap of non-terminal, parseable-run-id
// candidate items, NONE of which is at the acceptance gate WITHIN the inspected
// prefix, but the reads are capped so a participant could exist BEYOND it. The
// block must still be surfaced with truncated=true and state unverifiable — not
// omitted, which would silently drop BOTH the block and the truncation signal
// despite an item possibly being at the acceptance gate. Deleting the
// `if truncated { ... }` branch (reverting to the bare `return nil`) reddens this
// on the non-nil assertion. No /healthz target is wired because this path issues
// no probe — asserting a nil block cannot be a fixture-setup failure.
func TestAcceptanceSlot_TruncatedNoParticipant_SurfacesUnverifiable(t *testing.T) {
	fb, srv := newFakeBackend(t)
	// cap+extra items, each with ONLY a non-acceptance (plan) stage so none is a
	// participant, all non-terminal with parseable run ids so all qualify as
	// candidates and the cap is hit before any participant is found.
	total := acceptanceSlotMaxItemReads + 3
	items := make([]CampaignItem, 0, total)
	for i := 0; i < total; i++ {
		runID := uuid.New()
		fb.stagesByRun[runID] = []Stage{
			{ID: uuid.NewString(), RunID: runID.String(), Sequence: 1, Type: "plan", State: "running"},
		}
		items = append(items, CampaignItem{ID: uuid.NewString(), IssueRef: "#" + uuid.NewString()[:4], RunID: runID.String(), State: "running"})
	}
	r := newSlotResolver(srv, nil)

	slot := r.acceptanceSlotFor(context.Background(), items)
	if slot == nil {
		t.Fatal("truncated inspection with no found participant must still surface the block (truncation signal), got nil")
	}
	if !slot.Truncated {
		t.Errorf("Truncated = false, want true (more candidates than the cap)")
	}
	if slot.State != "unverifiable" {
		t.Fatalf("state = %q, want unverifiable (a participant may exist beyond the cap; the slot cannot be positively decided); detail=%s", slot.State, slot.Detail)
	}
	if slot.ItemsInspected != acceptanceSlotMaxItemReads {
		t.Errorf("ItemsInspected = %d, want %d", slot.ItemsInspected, acceptanceSlotMaxItemReads)
	}
	if slot.HeldBy != nil {
		t.Errorf("HeldBy = %+v, want nil (no participant was found)", slot.HeldBy)
	}
}

// TestAcceptanceSlot_AllReadsFail_SurfacesUnverifiable pins the #2503 fix-up
// high-concern edge path: a sole candidate — or all candidates — whose stage
// reads FAIL can be AT the acceptance gate, but the classification cannot tell.
// Without this guard the block is omitted entirely (no holder, no waiter, not
// truncated → the bare `return nil`), presenting an incomplete inspection as "no
// acceptance participation" and dropping the read_failures signal — the same
// trustworthy-observability failure shape as the truncation boundary. The block
// must be surfaced with state unverifiable, no probe, and the failed ref in
// read_failures. Deleting the `len(readFailures) == 0` term from the short-circuit
// guard (reverting to `if !truncated`) reddens this on the non-nil assertion. No
// /healthz target is wired because this path issues no probe — asserting a nil
// block cannot be a fixture-setup failure.
func TestAcceptanceSlot_AllReadsFail_SurfacesUnverifiable(t *testing.T) {
	fb, srv := newFakeBackend(t)
	// A sole candidate whose stage read 500s: it might be at the acceptance gate,
	// but we cannot classify it. Non-terminal item state + parseable run id → it
	// qualifies as a candidate before the read fails.
	item, failRun := seedItemWithAcceptanceStage(t, fb, "#26", "running", "running")
	fb.stagesStatusByRun[failRun] = http.StatusInternalServerError
	r := newSlotResolver(srv, nil)

	slot := r.acceptanceSlotFor(context.Background(), []CampaignItem{item})
	if slot == nil {
		t.Fatal("all candidate reads failing must still surface the block (read_failures signal), got nil")
	}
	if slot.State != "unverifiable" {
		t.Fatalf("state = %q, want unverifiable (classification incomplete; a candidate may be at the acceptance gate); detail=%s", slot.State, slot.Detail)
	}
	if len(slot.ReadFailures) != 1 || slot.ReadFailures[0] != "#26" {
		t.Fatalf("ReadFailures = %+v, want [#26] (the failed read is recorded, not silently dropped)", slot.ReadFailures)
	}
	if slot.HeldBy != nil {
		t.Errorf("HeldBy = %+v, want nil (no participant could be classified)", slot.HeldBy)
	}
	if len(slot.Waiting) != 0 {
		t.Errorf("Waiting = %+v, want empty (no participant could be classified)", slot.Waiting)
	}
	if slot.Truncated {
		t.Errorf("Truncated = true, want false (the cap was not hit; the read failed)")
	}
}

// TestAcceptanceSlot_TruncatedAndReadFailure_SurfacesBothReasons proves the
// incomplete-inspection block names BOTH reasons when the inspection was truncated
// AND a read failed: read_failures carries the failed ref and Truncated is true,
// so neither signal is dropped.
func TestAcceptanceSlot_TruncatedAndReadFailure_SurfacesBothReasons(t *testing.T) {
	fb, srv := newFakeBackend(t)
	// cap+extra candidates so the inspection truncates; the FIRST inspected item's
	// read 500s so read_failures is also populated. None is a participant (all have
	// only a plan stage except the failing one, whose read never returns).
	total := acceptanceSlotMaxItemReads + 3
	items := make([]CampaignItem, 0, total)
	failItem, failRun := seedItemWithAcceptanceStage(t, fb, "#26", "running", "running")
	fb.stagesStatusByRun[failRun] = http.StatusInternalServerError
	items = append(items, failItem)
	for i := 0; i < total-1; i++ {
		runID := uuid.New()
		fb.stagesByRun[runID] = []Stage{
			{ID: uuid.NewString(), RunID: runID.String(), Sequence: 1, Type: "plan", State: "running"},
		}
		items = append(items, CampaignItem{ID: uuid.NewString(), IssueRef: "#" + uuid.NewString()[:4], RunID: runID.String(), State: "running"})
	}
	r := newSlotResolver(srv, nil)

	slot := r.acceptanceSlotFor(context.Background(), items)
	if slot == nil {
		t.Fatal("expected an incomplete-inspection block, got nil")
	}
	if slot.State != "unverifiable" {
		t.Fatalf("state = %q, want unverifiable; detail=%s", slot.State, slot.Detail)
	}
	if !slot.Truncated {
		t.Errorf("Truncated = false, want true (more candidates than the cap)")
	}
	if len(slot.ReadFailures) != 1 || slot.ReadFailures[0] != "#26" {
		t.Errorf("ReadFailures = %+v, want [#26] (the failed read is recorded alongside truncation)", slot.ReadFailures)
	}
}

// TestAcceptanceSlotFor_TerminalItem_SkippedNoRead proves a terminal item state
// short-circuits in pass 1 with no stage read (the cost bound). A single terminal
// item yields nil (no participant) and issues no read.
func TestAcceptanceSlotFor_TerminalItem_SkippedNoRead(t *testing.T) {
	fb, srv := newFakeBackend(t)
	item, runID := seedItemWithAcceptanceStage(t, fb, "#26", "done", "running")
	r := newSlotResolver(srv, nil)
	if slot := r.acceptanceSlotFor(context.Background(), []CampaignItem{item}); slot != nil {
		t.Fatalf("a terminal item should be skipped, got %+v", slot)
	}
	fb.mu.Lock()
	defer fb.mu.Unlock()
	if fb.stagesCalledByID[runID] != 0 {
		t.Errorf("stage reads for a terminal item = %d, want 0 (pass 1 skips it before any network read)", fb.stagesCalledByID[runID])
	}
}

// TestAcceptanceSlotFor_NoRunID_Skipped proves an item without a run id (never
// started) is not a candidate and issues no read.
func TestAcceptanceSlotFor_NoRunID_Skipped(t *testing.T) {
	_, srv := newFakeBackend(t)
	item := CampaignItem{ID: uuid.NewString(), IssueRef: "#26", State: "eligible"}
	r := newSlotResolver(srv, nil)
	if slot := r.acceptanceSlotFor(context.Background(), []CampaignItem{item}); slot != nil {
		t.Fatalf("an item with no run id should be skipped, got %+v", slot)
	}
}
