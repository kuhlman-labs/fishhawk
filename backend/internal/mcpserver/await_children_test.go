package mcpserver

import (
	"context"
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
	seedChildWithSlice(fb, b, "pending", "awaiting_host_dispatch", 1, []int{0})
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

// --- release condition (3): children_settled ---

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
	if out.Status != "children_settled" {
		t.Fatalf("status = %q, want children_settled", out.Status)
	}
	if out.NextStep == nil || out.NextStep.Action != "fishhawk_consolidate_slices" {
		t.Errorf("next_step = %+v, want fishhawk_consolidate_slices", out.NextStep)
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
// owns.
func TestAwaitChildrenDispatchableIsPure(t *testing.T) {
	cs := &ChildrenStatus{
		Children: []ChildStatus{
			{RunID: "r0", SliceIndex: 0, State: "succeeded"},
			{RunID: "r1", SliceIndex: 1, State: "pending", DependsOn: []int{0}},
			{RunID: "r2", SliceIndex: 2, State: "pending", DependsOn: []int{5}}, // not minted
			{RunID: "r3", SliceIndex: 3, State: "running"},                      // in flight
		},
		IntegratedChildRunIDs: []string{"r0"},
	}
	got := awaitChildrenDispatchable(cs)
	if len(got) != 1 || got[0] != "r1" {
		t.Fatalf("dispatchable = %v, want [r1] (r2's dependency slice is not minted; r3 is in flight)", got)
	}
	cs.IntegratedChildRunIDs = nil
	if got := awaitChildrenDispatchable(cs); len(got) != 0 {
		t.Errorf("dispatchable with NO integration = %v, want none", got)
	}
}
