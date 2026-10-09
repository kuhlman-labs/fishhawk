package mcpserver

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// --- fixture helpers ---

// seedChildWithSlice seeds a child run row carrying the authoritative
// slice_index + slice_depends_on the ChildrenStatus snapshot reads, plus its
// implement stage.
func seedChildWithSlice(fb *fakeBackend, childID uuid.UUID, runState, stageState string, sliceIndex int, dependsOn []int) uuid.UUID {
	stageID := uuid.New()
	fb.mu.Lock()
	defer fb.mu.Unlock()
	si := sliceIndex
	fb.getRunByID[childID] = Run{
		ID: childID.String(), State: runState, Repo: "x/y",
		SliceIndex: &si, SliceDependsOn: dependsOn,
	}
	fb.stagesByRun[childID] = []Stage{{ID: stageID.String(), RunID: childID.String(), Type: "implement", State: stageState}}
	return stageID
}

// seedSlicesIntegrated appends a slices_integrated audit entry to the parent
// carrying the given merged child run ids.
//
// KEY PROVENANCE (binding condition 3): the payload keys are NOT hand-written
// here. They are taken from slicesIntegratedPayload's json tags — the SAME
// declaration decodeIntegratedChildRunIDs / decodeConsolidatedBranch read —
// which TestSlicesIntegratedPayloadKeysMatchEmitter separately proves match
// orchestrator.emitSlicesIntegrated's real payload literal on disk. So a
// renamed key on either side reddens instead of staying green in every mode
// test while the real verb never releases.
func seedSlicesIntegrated(t *testing.T, fb *fakeBackend, parent uuid.UUID, branch string, childRunIDs []string) {
	t.Helper()
	payload := slicesIntegratedPayloadMap(t, branch, childRunIDs)
	fb.mu.Lock()
	defer fb.mu.Unlock()
	seq := int64(len(fb.perRunAuditByRun[parent]) + 1)
	fb.perRunAuditByRun[parent] = append(fb.perRunAuditByRun[parent], AuditEntry{
		ID:       uuid.NewString(),
		Sequence: seq,
		RunID:    parent.String(),
		Category: "slices_integrated",
		Payload:  payload,
	})
}

// seedFillerAudit appends n benign audit entries (category "stage_advanced",
// none of the fan-in kinds) to the parent so a subsequent fan-in marker lands
// BEYOND the endpoint's first 500-entry page — the #2695 long-history situation.
func seedFillerAudit(fb *fakeBackend, parent uuid.UUID, n int) {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	for i := 0; i < n; i++ {
		seq := int64(len(fb.perRunAuditByRun[parent]) + 1)
		fb.perRunAuditByRun[parent] = append(fb.perRunAuditByRun[parent], AuditEntry{
			ID:       uuid.NewString(),
			Sequence: seq,
			RunID:    parent.String(),
			Category: "stage_advanced",
		})
	}
}

// seedFillerFanIn appends n slices_integrated markers that cover NOTHING
// relevant (each a throwaway child run id), so a later covering marker of the
// SAME category lands beyond the endpoint's first 500-entry page. This is the
// situation seedFillerAudit + a single fan-in marker cannot create: there the
// fillers are a DIFFERENT category (stage_advanced), so the category-filtered
// fan-in read collapses to one page and the paginate-to-last-page walk never
// runs to a second page. Here the fan-in category itself spans multiple pages.
func seedFillerFanIn(t *testing.T, fb *fakeBackend, parent uuid.UUID, n int) {
	t.Helper()
	payload := slicesIntegratedPayloadMap(t, "fishhawk/filler", []string{uuid.NewString()})
	fb.mu.Lock()
	defer fb.mu.Unlock()
	for i := 0; i < n; i++ {
		seq := int64(len(fb.perRunAuditByRun[parent]) + 1)
		fb.perRunAuditByRun[parent] = append(fb.perRunAuditByRun[parent], AuditEntry{
			ID:       uuid.NewString(),
			Sequence: seq,
			RunID:    parent.String(),
			Category: "slices_integrated",
			Payload:  payload,
		})
	}
}

func newAwaitResolver(t *testing.T) (*fakeBackend, *runResolver) {
	t.Helper()
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	r.reviewPollInterval = time.Millisecond
	return fb, r
}

func awaitIn(parent uuid.UUID) AwaitChildrenInput {
	return AwaitChildrenInput{RunID: parent.String(), TimeoutSeconds: 2}
}

// --- release condition (2): children_dispatchable ---

// TestAwaitChildren_DispatchableReleasesOnFirstPoll pins the NO-BASELINE
// property. A covered, awaiting-host-dispatch child that already exists when
// the call is armed must release on the FIRST poll — a transition-keyed release
// would wait forever for a change that already happened.
func TestAwaitChildren_DispatchableReleasesOnFirstPoll(t *testing.T) {
	fb, r := newAwaitResolver(t)
	parent, a := uuid.New(), uuid.New()
	seedChildWithSlice(fb, a, "pending", "awaiting_host_dispatch", 0, nil)
	seedPlanDecomposed(fb, parent, []string{a.String()}, 0)

	start := time.Now()
	_, out, err := r.awaitChildren(context.Background(), nil, awaitIn(parent))
	if err != nil {
		t.Fatalf("awaitChildren: %v", err)
	}
	if out.Status != "children_dispatchable" {
		t.Fatalf("status = %q, want children_dispatchable", out.Status)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("released after %v — the first-poll evaluation did not fire", elapsed)
	}
	if len(out.DispatchableChildRunIDs) != 1 || out.DispatchableChildRunIDs[0] != a.String() {
		t.Errorf("dispatchable = %v, want [%s]", out.DispatchableChildRunIDs, a)
	}
	if out.NextStep == nil || out.NextStep.Action != "fishhawk_run_children" {
		t.Errorf("next_step = %+v, want fishhawk_run_children", out.NextStep)
	}
	if out.Children == nil {
		t.Error("release carried no ChildrenStatus snapshot")
	}
}

// TestAwaitChildren_SucceededButNotCoveredDoesNotRelease is the load-bearing
// discrimination: predecessor run state flips to succeeded BEFORE the
// between-wave integration runs, so a Blocked-keyed (state-keyed) release would
// announce a dispatch the server refuses 409 wave_not_integrated. The dependent
// child must stay unreleased until the integration set is EXTENDED to cover it.
func TestAwaitChildren_SucceededButNotCoveredDoesNotRelease(t *testing.T) {
	fb, r := newAwaitResolver(t)
	parent, a, b := uuid.New(), uuid.New(), uuid.New()
	seedChildWithSlice(fb, a, "succeeded", "succeeded", 0, nil)
	// B is parked by RuleChildrenDispatch: its RUN state is 'running' (#1237)
	// while its implement STAGE waits at awaiting_host_dispatch. The release
	// keys on the stage state, so it must still become dispatchable once the
	// integration covers A — a run-state predicate would skip B forever.
	seedChildWithSlice(fb, b, "running", "awaiting_host_dispatch", 1, []int{0})
	seedPlanDecomposed(fb, parent, []string{a.String(), b.String()}, 0)
	// A slices_integrated entry EXISTS but covers nothing relevant — the stale
	// case. Blocked would be false here (slice 0 succeeded); coverage is not.
	seedSlicesIntegrated(t, fb, parent, "fishhawk/consolidated", []string{uuid.NewString()})

	in := awaitIn(parent)
	in.TimeoutSeconds = 1
	_, out, err := r.awaitChildren(context.Background(), nil, in)
	if err != nil {
		t.Fatalf("awaitChildren: %v", err)
	}
	if out.Status != "timeout" {
		t.Fatalf("status = %q, want timeout — a succeeded-but-unmerged predecessor must NOT release children_dispatchable", out.Status)
	}

	// Extend the integration to cover slice 0 and the SAME predicate releases.
	seedSlicesIntegrated(t, fb, parent, "fishhawk/consolidated", []string{a.String()})
	_, out2, err := r.awaitChildren(context.Background(), nil, awaitIn(parent))
	if err != nil {
		t.Fatalf("awaitChildren (post-integration): %v", err)
	}
	if out2.Status != "children_dispatchable" {
		t.Fatalf("status after integration = %q, want children_dispatchable", out2.Status)
	}
	if len(out2.DispatchableChildRunIDs) != 1 || out2.DispatchableChildRunIDs[0] != b.String() {
		t.Errorf("dispatchable = %v, want [%s]", out2.DispatchableChildRunIDs, b)
	}
}

// --- release condition (4): children_failed / integration_pending / children_settled ---

// TestAwaitChildren_AllTerminalReleasesSettled: a succeeded + failed fan-out is
// terminal but NOT settled (#4080) — it releases children_failed naming the
// failed child, with next_step get_run_status on it. Before #4080 this released
// children_settled and pointed at fishhawk_consolidate_slices, which cannot
// integrate a failed slice.
func TestAwaitChildren_AllTerminalReleasesSettled(t *testing.T) {
	fb, r := newAwaitResolver(t)
	parent, a, b := uuid.New(), uuid.New(), uuid.New()
	seedChildWithSlice(fb, a, "succeeded", "succeeded", 0, nil)
	seedChildWithSlice(fb, b, "failed", "failed", 1, nil)
	seedPlanDecomposed(fb, parent, []string{a.String(), b.String()}, 0)

	_, out, err := r.awaitChildren(context.Background(), nil, awaitIn(parent))
	if err != nil {
		t.Fatalf("awaitChildren: %v", err)
	}
	if out.Status != "children_failed" {
		t.Fatalf("status = %q, want children_failed", out.Status)
	}
	if len(out.FailedChildRunIDs) != 1 || out.FailedChildRunIDs[0] != b.String() {
		t.Errorf("failed_child_run_ids = %v, want [%s]", out.FailedChildRunIDs, b)
	}
	if out.NextStep == nil || out.NextStep.Action != "fishhawk_get_run_status" || out.NextStep.Params["run_id"] != b.String() {
		t.Errorf("next_step = %+v, want fishhawk_get_run_status on the failed child %s", out.NextStep, b)
	}
	if !strings.Contains(out.Message, b.String()) {
		t.Errorf("message %q does not name the failed child", out.Message)
	}
}

// --- release condition (4): timeout ---

func TestAwaitChildren_TimeoutIsResumable(t *testing.T) {
	fb, r := newAwaitResolver(t)
	parent, a := uuid.New(), uuid.New()
	// running: not dispatchable, not terminal, no amendment.
	seedChildWithSlice(fb, a, "running", "running", 0, nil)
	seedPlanDecomposed(fb, parent, []string{a.String()}, 0)

	in := awaitIn(parent)
	in.TimeoutSeconds = 1
	_, out, err := r.awaitChildren(context.Background(), nil, in)
	if err != nil {
		t.Fatalf("awaitChildren: %v", err)
	}
	if out.Status != "timeout" {
		t.Fatalf("status = %q, want timeout", out.Status)
	}
	if out.PollIntervalSeconds != suggestedStageWaitPollIntervalSeconds {
		t.Errorf("poll_interval_seconds = %d, want %d", out.PollIntervalSeconds, suggestedStageWaitPollIntervalSeconds)
	}
	// The contract requires EVERY release — timeout included — to carry the
	// ChildrenStatus snapshot and a re-arm next_step.
	if out.Children == nil {
		t.Error("timeout release carried no ChildrenStatus snapshot")
	}
	if out.NextStep == nil || out.NextStep.Action != "fishhawk_await_children" {
		t.Errorf("next_step = %+v, want a fishhawk_await_children re-arm", out.NextStep)
	} else if out.NextStep.Params["run_id"] != parent.String() {
		t.Errorf("re-arm run_id = %q, want the parent %s", out.NextStep.Params["run_id"], parent)
	}
}

// --- release condition (1): amendment_pending, and its two negative pins ---

func TestAwaitChildren_AmendmentPendingReleasesWithPrefilledDecide(t *testing.T) {
	fb, r := newAwaitResolver(t)
	parent, a := uuid.New(), uuid.New()
	stageID := seedChildWithSlice(fb, a, "running", "running", 0, nil)
	seedPlanDecomposed(fb, parent, []string{a.String()}, 0)
	amendmentID := uuid.NewString()
	fb.mu.Lock()
	fb.amendmentsByRun[a] = []ScopeAmendmentItem{{
		ID: amendmentID, RunID: a.String(), StageID: stageID.String(),
		Status: "pending", Reason: "needs a coupled test",
		Paths: []ScopeAmendmentPath{{Path: "x/y_test.go", Operation: "create"}},
	}}
	fb.mu.Unlock()

	_, out, err := r.awaitChildren(context.Background(), nil, awaitIn(parent))
	if err != nil {
		t.Fatalf("awaitChildren: %v", err)
	}
	if out.Status != "amendment_pending" {
		t.Fatalf("status = %q, want amendment_pending", out.Status)
	}
	if out.PendingAmendmentChildRunID != a.String() {
		t.Errorf("child = %q, want %s", out.PendingAmendmentChildRunID, a)
	}
	if out.PendingAmendment == nil || out.PendingAmendment.ID != amendmentID {
		t.Fatalf("pending_amendment = %+v, want the seeded row", out.PendingAmendment)
	}
	if out.NextStep == nil || out.NextStep.Action != "fishhawk_decide_scope_amendment" {
		t.Fatalf("next_step = %+v, want fishhawk_decide_scope_amendment", out.NextStep)
	}
	if out.NextStep.Params["run_id"] != a.String() || out.NextStep.Params["amendment_id"] != amendmentID {
		t.Errorf("next_step params = %v, want the CHILD run id + amendment id", out.NextStep.Params)
	}
}

// TestAwaitChildren_StrictAmendmentPredicate is the pair of NEGATIVE pins the
// strict #2588 predicate exists for: an amendment on a DIFFERENT stage id, and
// one whose status is not exactly "pending", must NOT release. A re-derivation
// that loosened either half would go green on the positive test above.
func TestAwaitChildren_StrictAmendmentPredicate(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(item *ScopeAmendmentItem)
		wantErr string
	}{
		{"different_stage_id", func(i *ScopeAmendmentItem) { i.StageID = uuid.NewString() }, ""},
		{"already_decided", func(i *ScopeAmendmentItem) { i.Status = "approved" }, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fb, r := newAwaitResolver(t)
			parent, a := uuid.New(), uuid.New()
			stageID := seedChildWithSlice(fb, a, "running", "running", 0, nil)
			seedPlanDecomposed(fb, parent, []string{a.String()}, 0)
			item := ScopeAmendmentItem{
				ID: uuid.NewString(), RunID: a.String(), StageID: stageID.String(), Status: "pending",
			}
			tc.mutate(&item)
			fb.mu.Lock()
			fb.amendmentsByRun[a] = []ScopeAmendmentItem{item}
			fb.mu.Unlock()

			in := awaitIn(parent)
			in.TimeoutSeconds = 1
			_, out, err := r.awaitChildren(context.Background(), nil, in)
			if err != nil {
				t.Fatalf("awaitChildren: %v", err)
			}
			if out.Status == "amendment_pending" {
				t.Fatalf("released amendment_pending on a %s row — the strict predicate is not enforced", tc.name)
			}
		})
	}
}

// TestAwaitChildren_TwoPendingAmendments_DeterministicSelectionAndReArm pins
// the concurrent-amendment contract: with pending amendments on BOTH a slice-0
// and a slice-1 child, the first await releases on the SLICE-0 child (ascending
// slice index), deciding it and re-invoking releases on the slice-1 child's
// remaining amendment (the re-arm), and a third await no longer releases on
// amendment_pending.
func TestAwaitChildren_TwoPendingAmendments_DeterministicSelectionAndReArm(t *testing.T) {
	fb, r := newAwaitResolver(t)
	parent, a, b := uuid.New(), uuid.New(), uuid.New()
	// Seed slice 1 FIRST so plan_decomposed order does NOT coincide with slice
	// order — a positional implementation would pick b and fail here.
	stageB := seedChildWithSlice(fb, b, "running", "running", 1, nil)
	stageA := seedChildWithSlice(fb, a, "running", "running", 0, nil)
	seedPlanDecomposed(fb, parent, []string{b.String(), a.String()}, 0)

	amendA, amendB := uuid.New(), uuid.New()
	fb.mu.Lock()
	fb.decideFlipsListStatus = true
	fb.amendmentsByRun[a] = []ScopeAmendmentItem{{ID: amendA.String(), RunID: a.String(), StageID: stageA.String(), Status: "pending"}}
	fb.amendmentsByRun[b] = []ScopeAmendmentItem{{ID: amendB.String(), RunID: b.String(), StageID: stageB.String(), Status: "pending"}}
	fb.mu.Unlock()

	_, first, err := r.awaitChildren(context.Background(), nil, awaitIn(parent))
	if err != nil {
		t.Fatalf("first await: %v", err)
	}
	if first.Status != "amendment_pending" || first.PendingAmendmentChildRunID != a.String() {
		t.Fatalf("first release = %q on %s, want amendment_pending on the SLICE-0 child %s",
			first.Status, first.PendingAmendmentChildRunID, a)
	}

	// Decide it through the real handler with the next_step's OWN arguments.
	if _, _, err := r.decideScopeAmendment(context.Background(), nil, DecideScopeAmendmentInput{
		RunID:       first.NextStep.Params["run_id"],
		AmendmentID: first.NextStep.Params["amendment_id"],
		Decision:    "approve",
	}); err != nil {
		t.Fatalf("decide: %v", err)
	}

	_, second, err := r.awaitChildren(context.Background(), nil, awaitIn(parent))
	if err != nil {
		t.Fatalf("second await: %v", err)
	}
	if second.Status != "amendment_pending" || second.PendingAmendmentChildRunID != b.String() {
		t.Fatalf("second release = %q on %s, want the re-arm on slice-1 child %s",
			second.Status, second.PendingAmendmentChildRunID, b)
	}
	if _, _, err := r.decideScopeAmendment(context.Background(), nil, DecideScopeAmendmentInput{
		RunID:       second.NextStep.Params["run_id"],
		AmendmentID: second.NextStep.Params["amendment_id"],
		Decision:    "approve",
	}); err != nil {
		t.Fatalf("decide 2: %v", err)
	}

	in := awaitIn(parent)
	in.TimeoutSeconds = 1
	_, third, err := r.awaitChildren(context.Background(), nil, in)
	if err != nil {
		t.Fatalf("third await: %v", err)
	}
	if third.Status == "amendment_pending" {
		t.Fatalf("third await still released amendment_pending (%s) — the decisions did not clear the queue",
			third.PendingAmendmentChildRunID)
	}
}

// --- item 2 (#2695): the omitted-timeout 600s default ---

// TestClampAwaitChildrenTimeout pins the boundary set: a non-positive input
// resolves to the 600s default (NOT the shared 360s review default), a positive
// input clamps against effectiveAwaitCap, and long_wait unlocks 7200s.
func TestClampAwaitChildrenTimeout(t *testing.T) {
	cases := []struct {
		n         int
		heartbeat bool
		longWait  bool
		want      int
	}{
		{0, false, false, 600},    // omitted → the 600s default
		{-5, false, false, 600},   // negative → the 600s default
		{1, false, false, 1},      // small positive passes through
		{600, false, false, 600},  // at the default cap
		{601, false, false, 600},  // over the 600s cap with no opt-in → clamped down
		{601, false, true, 601},   // long_wait raises the cap → passes through
		{7201, false, true, 7200}, // over the raised cap → clamped to 7200
		{7201, true, false, 7200}, // progressToken raises the cap likewise
	}
	for _, c := range cases {
		if got := clampAwaitChildrenTimeout(c.n, c.heartbeat, c.longWait); got != c.want {
			t.Errorf("clampAwaitChildrenTimeout(%d, heartbeat=%v, longWait=%v) = %d, want %d",
				c.n, c.heartbeat, c.longWait, got, c.want)
		}
	}
}

// TestAwaitChildren_OmittedTimeoutDefaultsTo600s is the BEHAVIORAL wiring test:
// with timeout_seconds OMITTED against a parent that can never release, the
// resolved timeout the message interpolates must be the 600s default. A wiring
// regression that left the shared 360s clampAwaitTimeoutHeartbeat in place would
// interpolate 360 and redden here — the done-means test for a default-VALUE change
// compilation cannot enforce. The caller ctx is cancelled after a few poll
// intervals to force the timeout path deterministically.
func TestAwaitChildren_OmittedTimeoutDefaultsTo600s(t *testing.T) {
	fb, r := newAwaitResolver(t)
	parent, a := uuid.New(), uuid.New()
	// running: not dispatchable, not terminal, no amendment → never releases.
	seedChildWithSlice(fb, a, "running", "running", 0, nil)
	seedPlanDecomposed(fb, parent, []string{a.String()}, 0)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()

	in := AwaitChildrenInput{RunID: parent.String()} // timeout_seconds OMITTED
	_, out, err := r.awaitChildren(ctx, nil, in)
	if err != nil {
		t.Fatalf("awaitChildren: %v", err)
	}
	if out.Status != "timeout" {
		t.Fatalf("status = %q, want timeout", out.Status)
	}
	if !strings.Contains(out.Message, "600s") {
		t.Errorf("timeout message = %q, want it to name the 600s default (a 360s-clamp wiring regression reddens here)", out.Message)
	}
	if out.TimeoutCapSeconds != 600 {
		t.Errorf("timeout_cap_seconds = %d, want 600", out.TimeoutCapSeconds)
	}
}

// --- item 1 (#2695): the fan-in marker beyond the first audit page ---

// TestAwaitChildren_IntegrationMarkerBeyondFirstAuditPage is the item-1 control.
// The slices_integrated marker that covers slice 0 sits AFTER 600 filler entries,
// i.e. beyond the endpoint's first 500-entry page. The old single unfiltered
// ListRunAudit{Limit:500} read never saw it and every await timed out; the
// category-filtered, paginated fan-in read finds it regardless of history length,
// so the dependent child releases children_dispatchable. The counterfactual
// (restore the single read, observe status "timeout") is recorded in the PR body.
func TestAwaitChildren_IntegrationMarkerBeyondFirstAuditPage(t *testing.T) {
	fb, r := newAwaitResolver(t)
	parent, a, b := uuid.New(), uuid.New(), uuid.New()
	// Wave-0 child a succeeded; dependent child b parked at awaiting_host_dispatch
	// (run 'running', #1237) depending on slice 0.
	seedChildWithSlice(fb, a, "succeeded", "succeeded", 0, nil)
	seedChildWithSlice(fb, b, "running", "awaiting_host_dispatch", 1, []int{0})
	seedPlanDecomposed(fb, parent, []string{a.String(), b.String()}, 0)
	// Bury the fan-in marker beyond the first 500-entry page.
	seedFillerAudit(fb, parent, 600)
	// THEN the slices_integrated entry that covers slice 0 (the newest entry).
	seedSlicesIntegrated(t, fb, parent, "fishhawk/consolidated", []string{a.String()})

	_, out, err := r.awaitChildren(context.Background(), nil, awaitIn(parent))
	if err != nil {
		t.Fatalf("awaitChildren: %v", err)
	}
	if out.Status != "children_dispatchable" {
		t.Fatalf("status = %q, want children_dispatchable — the integration marker beyond page one must still be found", out.Status)
	}
	if len(out.DispatchableChildRunIDs) != 1 || out.DispatchableChildRunIDs[0] != b.String() {
		t.Errorf("dispatchable = %v, want [%s]", out.DispatchableChildRunIDs, b)
	}
}

// TestAwaitChildren_MultiPageFanInWalkKeepsLastPage is the item-1 SUCCESS-branch
// control (#2695). TestAwaitChildren_IntegrationMarkerBeyondFirstAuditPage buries
// the marker after 600 stage_advanced fillers, but those are a DIFFERENT category:
// the category-filtered fan-in read of slices_integrated returns the single marker
// on page one, so lastFanInAuditPage's cursor-follow-and-keep-last-page loop never
// runs to a second page and its success branch is never exercised green. Here the
// fan-in CATEGORY itself has 501 entries: page one holds 500 markers covering
// nothing, page two holds the ONE covering marker (the newest). The walk must keep
// the LAST page, so dependent child b releases children_dispatchable. A regression
// returning the FIRST page instead (assigning `last` once, or breaking out early)
// would keep marker #500 — which covers nothing — and b would never release
// (status "timeout"). That counterfactual (return page one instead of the last
// page → observed "timeout") is recorded in the PR body.
func TestAwaitChildren_MultiPageFanInWalkKeepsLastPage(t *testing.T) {
	fb, r := newAwaitResolver(t)
	parent, a, b := uuid.New(), uuid.New(), uuid.New()
	seedChildWithSlice(fb, a, "succeeded", "succeeded", 0, nil)
	seedChildWithSlice(fb, b, "running", "awaiting_host_dispatch", 1, []int{0})
	seedPlanDecomposed(fb, parent, []string{a.String(), b.String()}, 0)
	// 500 slices_integrated markers covering nothing relevant fill page one...
	seedFillerFanIn(t, fb, parent, 500)
	// ...and the 501st — newest, on page two — covers slice 0's child a.
	seedSlicesIntegrated(t, fb, parent, "fishhawk/consolidated", []string{a.String()})

	_, out, err := r.awaitChildren(context.Background(), nil, awaitIn(parent))
	if err != nil {
		t.Fatalf("awaitChildren: %v", err)
	}
	if out.Status != "children_dispatchable" {
		t.Fatalf("status = %q, want children_dispatchable — the covering marker on the LAST fan-in page must be kept, not page one's stale one", out.Status)
	}
	if len(out.DispatchableChildRunIDs) != 1 || out.DispatchableChildRunIDs[0] != b.String() {
		t.Errorf("dispatchable = %v, want [%s]", out.DispatchableChildRunIDs, b)
	}
}

// TestAwaitChildren_FanInHistoryExceedsPageCapFailsLoud pins binding condition 1's
// CAP-EXHAUSTION fault: an unbounded/pathological fan-in history (the endpoint
// keeps returning an advancing cursor) is a READ FAILURE naming its own cause —
// never a silently stale last-fetched page. Deleting the page-cap return reddens
// this (the walk would spin forever / hang the test).
func TestAwaitChildren_FanInHistoryExceedsPageCapFailsLoud(t *testing.T) {
	fb, r := newAwaitResolver(t)
	parent := uuid.New()
	// A real decomposed parent: fanInChildrenStatus probes plan_decomposed FIRST
	// (approval condition C1) and only then walks the fan-in categories. The
	// plan_decomposed probe is a single read, so the never-ending cursor below
	// reaches only the fan-in walk this test pins.
	seedPlanDecomposed(fb, parent, []string{uuid.NewString()}, 0)
	fb.mu.Lock()
	fb.perRunAuditNeverEndByRun[parent] = true // advancing cursor that never terminates
	fb.mu.Unlock()

	in := awaitIn(parent)
	in.TimeoutSeconds = 1
	_, out, err := r.awaitChildren(context.Background(), nil, in)
	if err == nil {
		t.Fatalf("expected a loud read error on cap exhaustion, got status %q", out.Status)
	}
	if !strings.Contains(err.Error(), "read parent audit") || !strings.Contains(err.Error(), "page walk cap") {
		t.Errorf("error = %v, want a wrapped 'read parent audit' cap-exhaustion failure", err)
	}
	// It must be DISTINCT from the non-progressing-cursor diagnosis.
	if strings.Contains(err.Error(), "did not advance") {
		t.Errorf("cap-exhaustion error collapsed into the non-progressing-cursor message: %v", err)
	}
}

// TestAwaitChildren_FanInCursorNonProgressingFailsLoud pins binding condition 1's
// NON-PROGRESSING-CURSOR fault, kept DISTINCT from cap exhaustion: the endpoint
// hands back the SAME cursor it was given, which is detected as its own fault
// rather than spun on. Deleting the next==cursor guard reddens this (the walk
// would run to the cap and report the wrong cause).
func TestAwaitChildren_FanInCursorNonProgressingFailsLoud(t *testing.T) {
	fb, r := newAwaitResolver(t)
	parent := uuid.New()
	// See the cap-exhaustion test: plan_decomposed is probed first (C1).
	seedPlanDecomposed(fb, parent, []string{uuid.NewString()}, 0)
	fb.mu.Lock()
	// A FIXED (non-advancing) next cursor on every page — the back-compat literal
	// branch returns it verbatim, so the walk sees next == the cursor it just sent.
	fb.perRunAuditNextByRun[parent] = "stuck-cursor"
	fb.mu.Unlock()

	in := awaitIn(parent)
	in.TimeoutSeconds = 1
	_, out, err := r.awaitChildren(context.Background(), nil, in)
	if err == nil {
		t.Fatalf("expected a loud read error on a non-progressing cursor, got status %q", out.Status)
	}
	if !strings.Contains(err.Error(), "read parent audit") || !strings.Contains(err.Error(), "did not advance") {
		t.Errorf("error = %v, want a wrapped 'read parent audit' non-progressing-cursor failure", err)
	}
	// It must be DISTINCT from the cap-exhaustion diagnosis.
	if strings.Contains(err.Error(), "page walk cap") {
		t.Errorf("non-progressing error collapsed into the cap-exhaustion message: %v", err)
	}
}

// TestAwaitChildren_NotDecomposedErrors pins the caller-error branch.
func TestAwaitChildren_NotDecomposedErrors(t *testing.T) {
	_, r := newAwaitResolver(t)
	if _, _, err := r.awaitChildren(context.Background(), nil, awaitIn(uuid.New())); err == nil {
		t.Fatal("expected an error for a run with no plan_decomposed entry")
	}
	if _, _, err := r.awaitChildren(context.Background(), nil, AwaitChildrenInput{RunID: "nope"}); err == nil {
		t.Fatal("expected an error for a non-UUID run_id")
	}
}

// TestAwaitChildrenDispatchableIsPure exercises the release predicate directly
// across its branches, including the not-minted fail-closed case wavecoverage
// owns. It keys on ImplementStageState, NOT the run-level State: the parked
// children r1/r2 carry run state 'running' (the RuleChildrenDispatch reality,
// #1237) with their implement stage at awaiting_host_dispatch, so a predicate
// that read the run state would find NONE dispatchable and this would fail.
func TestAwaitChildrenDispatchableIsPure(t *testing.T) {
	cs := &ChildrenStatus{
		Children: []ChildStatus{
			{RunID: "r0", SliceIndex: 0, State: "succeeded", ImplementStageState: "succeeded"},
			{RunID: "r1", SliceIndex: 1, State: "running", ImplementStageState: "awaiting_host_dispatch", DependsOn: []int{0}},
			{RunID: "r2", SliceIndex: 2, State: "running", ImplementStageState: "awaiting_host_dispatch", DependsOn: []int{5}}, // not minted
			{RunID: "r3", SliceIndex: 3, State: "running", ImplementStageState: "running"},                                     // in flight
		},
		IntegratedChildRunIDs: []string{"r0"},
	}
	got := awaitChildrenDispatchable(cs)
	if len(got) != 1 || got[0] != "r1" {
		t.Fatalf("dispatchable = %v, want [r1] (r2's dependency slice is not minted; r3's stage is in flight)", got)
	}
	cs.IntegratedChildRunIDs = nil
	if got := awaitChildrenDispatchable(cs); len(got) != 0 {
		t.Errorf("dispatchable with NO integration = %v, want none", got)
	}
}

// --- #4080: coverage-aware releases ---

// seedParentImplementStage seeds the decomposed parent's implement stage at
// state, so the integration_pending arm can read whether the parent is still
// awaiting_children (approval condition C4) and the C3 arm whether it already
// succeeded.
func seedParentImplementStage(fb *fakeBackend, parent uuid.UUID, state string) {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	fb.stagesByRun[parent] = []Stage{
		{ID: uuid.NewString(), RunID: parent.String(), Sequence: 1, Type: "plan", State: "succeeded"},
		{ID: uuid.NewString(), RunID: parent.String(), Sequence: 2, Type: "implement", State: state},
	}
}

// seedFanInEntry appends a fan-in audit entry of category to the parent. The
// payload is a DECODER struct (sliceHeadMissingPayload, …) marshalled through
// encoding/json, so its keys come from the decoder's own json tags — which the
// payload-key-tie tests separately bind to the real emitters on disk. No test
// hand-writes a key that could agree with a wrong decoder.
func seedFanInEntry(t *testing.T, fb *fakeBackend, parent uuid.UUID, category string, payload any) {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal %s payload: %v", category, err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("re-decode %s payload: %v", category, err)
	}
	fb.mu.Lock()
	defer fb.mu.Unlock()
	seq := int64(len(fb.perRunAuditByRun[parent]) + 1)
	fb.perRunAuditByRun[parent] = append(fb.perRunAuditByRun[parent], AuditEntry{
		ID:       uuid.NewString(),
		Sequence: seq,
		RunID:    parent.String(),
		Category: category,
		Payload:  m,
	})
}

// seedFourSucceeded seeds a decomposed parent with four SUCCEEDED, terminal
// children at slices 0..3.
func seedFourSucceeded(fb *fakeBackend, parent uuid.UUID) []uuid.UUID {
	kids := []uuid.UUID{uuid.New(), uuid.New(), uuid.New(), uuid.New()}
	ids := make([]string, len(kids))
	for i, k := range kids {
		seedChildWithSlice(fb, k, "succeeded", "succeeded", i, nil)
		ids[i] = k.String()
	}
	seedPlanDecomposed(fb, parent, ids, 0)
	return kids
}

// TestAwaitChildren_FourSucceededThreeIntegrated_IntegrationPendingNotSettled is
// the DONE-MEANS test (#4080). All four children are succeeded and terminal, so
// the pre-change awaitChildrenAllSettled arm WOULD release children_settled; the
// newest slices_integrated names only three. The verb must release
// integration_pending naming the fourth, with next_step consolidate (the parent
// is still awaiting_children, C4), and only release children_settled once an
// entry covering all four lands.
func TestAwaitChildren_FourSucceededThreeIntegrated_IntegrationPendingNotSettled(t *testing.T) {
	fb, r := newAwaitResolver(t)
	parent := uuid.New()
	kids := seedFourSucceeded(fb, parent)
	seedParentImplementStage(fb, parent, "awaiting_children")
	seedSlicesIntegrated(t, fb, parent, "fishhawk/run-x", []string{kids[0].String(), kids[1].String(), kids[2].String()})

	_, out, err := r.awaitChildren(context.Background(), nil, awaitIn(parent))
	if err != nil {
		t.Fatalf("awaitChildren: %v", err)
	}
	if out.Status != "integration_pending" {
		t.Fatalf("status = %q, want integration_pending — 3 of 4 slices integrated must NOT read as settled", out.Status)
	}
	if len(out.UnintegratedChildRunIDs) != 1 || out.UnintegratedChildRunIDs[0] != kids[3].String() {
		t.Errorf("unintegrated_child_run_ids = %v, want [%s]", out.UnintegratedChildRunIDs, kids[3])
	}
	if out.NextStep == nil || out.NextStep.Action != "fishhawk_consolidate_slices" || out.NextStep.Params["run_id"] != parent.String() {
		t.Errorf("next_step = %+v, want fishhawk_consolidate_slices on the parent", out.NextStep)
	}
	if !strings.Contains(out.Message, kids[3].String()) || !strings.Contains(out.Message, "Acceptance and review must wait") {
		t.Errorf("message %q must name the uncovered child and hold acceptance/review", out.Message)
	}
	if out.Children == nil || out.Children.IntegrationPhase != integrationPhaseReadyToIntegrate {
		t.Errorf("children.integration_phase = %+v, want ready_to_integrate", out.Children)
	}

	// A covering entry lands: the SAME absolute predicate now releases settled.
	all := []string{kids[0].String(), kids[1].String(), kids[2].String(), kids[3].String()}
	seedSlicesIntegrated(t, fb, parent, "fishhawk/run-x", all)
	_, out2, err := r.awaitChildren(context.Background(), nil, awaitIn(parent))
	if err != nil {
		t.Fatalf("awaitChildren (covered): %v", err)
	}
	if out2.Status != "children_settled" {
		t.Fatalf("status after full coverage = %q, want children_settled", out2.Status)
	}
	if out2.NextStep == nil || out2.NextStep.Action != "fishhawk_consolidate_slices" {
		t.Errorf("settled next_step = %+v, want fishhawk_consolidate_slices", out2.NextStep)
	}
}

// TestAwaitChildren_IntegrationPending_ParentAdvanced (approval condition C4):
// once the parent's implement stage has LEFT awaiting_children,
// fishhawk_consolidate_slices answers 409 not_awaiting_children, so
// integration_pending must not point there. next_step reads the uncovered
// child and the message names the real recovery (re-drive or resume it). An
// UNREADABLE parent stage takes the same non-consolidate arm.
func TestAwaitChildren_IntegrationPending_ParentAdvanced(t *testing.T) {
	cases := []struct {
		name        string
		parentState string // "" = no parent stage seeded (unreadable)
		wantInMsg   string
	}{
		{"parent implement succeeded", "succeeded", "implement stage is succeeded"},
		{"parent implement failed", "failed", "implement stage is failed"},
		{"parent stage unreadable", "", "implement stage is unreadable"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fb, r := newAwaitResolver(t)
			parent := uuid.New()
			kids := seedFourSucceeded(fb, parent)
			if c.parentState != "" {
				seedParentImplementStage(fb, parent, c.parentState)
			}
			seedSlicesIntegrated(t, fb, parent, "fishhawk/run-x", []string{kids[0].String(), kids[1].String(), kids[2].String()})

			_, out, err := r.awaitChildren(context.Background(), nil, awaitIn(parent))
			if err != nil {
				t.Fatalf("awaitChildren: %v", err)
			}
			if out.Status != "integration_pending" {
				t.Fatalf("status = %q, want integration_pending", out.Status)
			}
			if out.NextStep == nil || out.NextStep.Action == "fishhawk_consolidate_slices" {
				t.Fatalf("next_step = %+v — must NOT be fishhawk_consolidate_slices for an advanced parent (409 not_awaiting_children)", out.NextStep)
			}
			if out.NextStep.Action != "fishhawk_get_run_status" || out.NextStep.Params["run_id"] != kids[3].String() {
				t.Errorf("next_step = %+v, want fishhawk_get_run_status on the uncovered child %s", out.NextStep, kids[3])
			}
			for _, want := range []string{"re-drive or resume the uncovered child", "409 not_awaiting_children", c.wantInMsg, kids[3].String()} {
				if !strings.Contains(out.Message, want) {
					t.Errorf("message %q missing %q", out.Message, want)
				}
			}
		})
	}
}

// TestAwaitChildren_NoIntegrationAuthority_FallsBackToSettled (approval
// condition C3): on a deployment with no slice-integration authority the
// server writes NO fan-in record and advances the parent anyway. Requiring
// coverage there would wedge the parent forever, so — exactly where the server
// gate stands down — the verb falls back to the pre-#4080 settled release. The
// negative arm pins that the fallback needs the parent's implement stage to
// have SUCCEEDED: with the parent still awaiting_children the same no-record
// snapshot is integration_pending (the fan-in simply has not run yet).
func TestAwaitChildren_NoIntegrationAuthority_FallsBackToSettled(t *testing.T) {
	t.Run("no record + parent implement succeeded -> settled", func(t *testing.T) {
		fb, r := newAwaitResolver(t)
		parent := uuid.New()
		seedFourSucceeded(fb, parent)
		seedParentImplementStage(fb, parent, "succeeded")

		_, out, err := r.awaitChildren(context.Background(), nil, awaitIn(parent))
		if err != nil {
			t.Fatalf("awaitChildren: %v", err)
		}
		if out.Status != "children_settled" {
			t.Fatalf("status = %q, want children_settled (no integration authority — nothing to require)", out.Status)
		}
		if !strings.Contains(out.Message, "no slice-integration authority") {
			t.Errorf("message %q must say the deployment has no slice-integration authority", out.Message)
		}
	})
	t.Run("no record + parent still awaiting_children -> integration_pending", func(t *testing.T) {
		fb, r := newAwaitResolver(t)
		parent := uuid.New()
		seedFourSucceeded(fb, parent)
		seedParentImplementStage(fb, parent, "awaiting_children")

		_, out, err := r.awaitChildren(context.Background(), nil, awaitIn(parent))
		if err != nil {
			t.Fatalf("awaitChildren: %v", err)
		}
		if out.Status != "integration_pending" {
			t.Fatalf("status = %q, want integration_pending", out.Status)
		}
		if len(out.UnintegratedChildRunIDs) != 4 {
			t.Errorf("unintegrated_child_run_ids = %v, want all four", out.UnintegratedChildRunIDs)
		}
	})
	t.Run("PARTIAL record + parent implement succeeded -> integration_pending", func(t *testing.T) {
		// A record exists, so integration authority demonstrably exists: the
		// fallback must not fire on an advanced parent with partial coverage.
		fb, r := newAwaitResolver(t)
		parent := uuid.New()
		kids := seedFourSucceeded(fb, parent)
		seedParentImplementStage(fb, parent, "succeeded")
		seedSlicesIntegrated(t, fb, parent, "fishhawk/run-x", []string{kids[0].String()})

		_, out, err := r.awaitChildren(context.Background(), nil, awaitIn(parent))
		if err != nil {
			t.Fatalf("awaitChildren: %v", err)
		}
		if out.Status != "integration_pending" {
			t.Fatalf("status = %q, want integration_pending", out.Status)
		}
	})
}

// TestAwaitChildren_IntegrationFailed_PerCause pins one release per fan-in
// failure cause (#4080). MECHANISM: every child is succeeded, so without the
// failure arm the snapshot would release integration_pending — a different
// status this test catches. Each asserts the cause, the failing child, and
// next_step fishhawk_list_audit category=<cause>.
func TestAwaitChildren_IntegrationFailed_PerCause(t *testing.T) {
	type tc struct {
		name      string
		category  string
		payload   func(kids []uuid.UUID) any
		wantChild func(kids []uuid.UUID) string
		wantInMsg []string
	}
	cases := []tc{
		{
			name:     "slice_head_missing names the child, slice and detail",
			category: auditCategorySliceHeadMissing,
			payload: func(kids []uuid.UUID) any {
				return sliceHeadMissingPayload{ChildRunID: kids[2].String(), SliceIndex: intPtr(2), Branch: "fishhawk/run-p/slice-2", Detail: "ref not found"}
			},
			wantChild: func(kids []uuid.UUID) string { return kids[2].String() },
			wantInMsg: []string{"slice 2", "ref not found", "fishhawk/run-p/slice-2", "push the missing slice branch"},
		},
		{
			name:     "slice_integration_conflict names the conflicting child",
			category: auditCategorySliceIntegrationConflict,
			payload: func(kids []uuid.UUID) any {
				return sliceIntegrationConflictPayload{ConflictingChildRunID: kids[1].String(), ConflictingSliceIndex: intPtr(1)}
			},
			wantChild: func(kids []uuid.UUID) string { return kids[1].String() },
			wantInMsg: []string{"slice 1", "merge conflict", "re-drive the conflicting child"},
		},
		{
			name:     "slice_integration_failed names the cause",
			category: auditCategorySliceIntegrationFailed,
			payload: func([]uuid.UUID) any {
				return sliceIntegrationFailedPayload{Attempts: 5, Error: "github 502"}
			},
			wantChild: func([]uuid.UUID) string { return "" },
			wantInMsg: []string{"after 5 attempts: github 502", "category-B"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fb, r := newAwaitResolver(t)
			parent := uuid.New()
			kids := seedFourSucceeded(fb, parent)
			seedParentImplementStage(fb, parent, "awaiting_children")
			seedSlicesIntegrated(t, fb, parent, "fishhawk/run-x", []string{kids[0].String()})
			seedFanInEntry(t, fb, parent, c.category, c.payload(kids))

			_, out, err := r.awaitChildren(context.Background(), nil, awaitIn(parent))
			if err != nil {
				t.Fatalf("awaitChildren: %v", err)
			}
			if out.Status != "integration_failed" {
				t.Fatalf("status = %q, want integration_failed", out.Status)
			}
			if out.IntegrationFailure == nil || out.IntegrationFailure.Cause != c.category {
				t.Fatalf("integration_failure = %+v, want cause %s", out.IntegrationFailure, c.category)
			}
			if got, want := out.IntegrationFailure.ChildRunID, c.wantChild(kids); got != want {
				t.Errorf("integration_failure.child_run_id = %q, want %q", got, want)
			}
			if out.NextStep == nil || out.NextStep.Action != "fishhawk_list_audit" ||
				out.NextStep.Params["category"] != c.category || out.NextStep.Params["run_id"] != parent.String() {
				t.Errorf("next_step = %+v, want fishhawk_list_audit {run_id: parent, category: %s}", out.NextStep, c.category)
			}
			if !strings.Contains(out.Message, "Do NOT dispatch acceptance or approve the review") {
				t.Errorf("message %q must forbid acceptance/review", out.Message)
			}
			for _, want := range append(c.wantInMsg, c.wantChild(kids)) {
				if !strings.Contains(out.Message, want) {
					t.Errorf("message %q missing %q", out.Message, want)
				}
			}
			if out.Children == nil || out.Children.IntegrationFailure == nil {
				t.Errorf("children block lacks integration_failure: %+v", out.Children)
			}
		})
	}
}

// TestAwaitChildren_IntegrationFailedReleasedMidFanOut: a between-wave
// slice_head_missing blocks the dependent wave indefinitely, so the failure
// releases even while a sibling is still running. Without the arm nothing
// releases (a running child is neither dispatchable nor terminal) and the
// short timeout returns 'timeout'.
func TestAwaitChildren_IntegrationFailedReleasedMidFanOut(t *testing.T) {
	fb, r := newAwaitResolver(t)
	parent, a, b := uuid.New(), uuid.New(), uuid.New()
	seedChildWithSlice(fb, a, "succeeded", "succeeded", 0, nil)
	seedChildWithSlice(fb, b, "running", "running", 1, []int{0})
	seedPlanDecomposed(fb, parent, []string{a.String(), b.String()}, 0)
	seedFanInEntry(t, fb, parent, auditCategorySliceHeadMissing,
		sliceHeadMissingPayload{ChildRunID: a.String(), SliceIndex: intPtr(0), Branch: "fishhawk/run-p/slice-0", Detail: "absent"})

	in := awaitIn(parent)
	in.TimeoutSeconds = 1
	_, out, err := r.awaitChildren(context.Background(), nil, in)
	if err != nil {
		t.Fatalf("awaitChildren: %v", err)
	}
	if out.Status != "integration_failed" {
		t.Fatalf("status = %q, want integration_failed released mid-fan-out", out.Status)
	}
	if out.IntegrationFailure == nil || out.IntegrationFailure.ChildRunID != a.String() {
		t.Errorf("integration_failure = %+v, want child %s", out.IntegrationFailure, a)
	}
}

// TestAwaitChildren_CleanIntegrationNewerThanHeadMissingSettles pins the
// ordering: a full clean integration NEWER than a head-missing supersedes it.
func TestAwaitChildren_CleanIntegrationNewerThanHeadMissingSettles(t *testing.T) {
	fb, r := newAwaitResolver(t)
	parent := uuid.New()
	kids := seedFourSucceeded(fb, parent)
	seedParentImplementStage(fb, parent, "awaiting_children")
	seedFanInEntry(t, fb, parent, auditCategorySliceHeadMissing,
		sliceHeadMissingPayload{ChildRunID: kids[3].String(), SliceIndex: intPtr(3), Branch: "b", Detail: "absent"})
	seedSlicesIntegrated(t, fb, parent, "fishhawk/run-x",
		[]string{kids[0].String(), kids[1].String(), kids[2].String(), kids[3].String()})

	_, out, err := r.awaitChildren(context.Background(), nil, awaitIn(parent))
	if err != nil {
		t.Fatalf("awaitChildren: %v", err)
	}
	if out.Status != "children_settled" {
		t.Fatalf("status = %q, want children_settled (the newer clean integration supersedes the head-missing)", out.Status)
	}
	if out.IntegrationFailure != nil {
		t.Errorf("integration_failure = %+v, want nil once superseded", out.IntegrationFailure)
	}
}
