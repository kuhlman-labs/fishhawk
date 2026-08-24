package github

// Tests for the optional workmgmt.GroomingMutator capability (E54.5 / #2237).
//
// Structure mirrors reader_test.go: the per-kind dispatch table asserting the
// forge calls each kind emits, ONE behavioral test per defensive branch
// (each asserting the TYPED error AND that nothing was written), and a
// CROSS-BOUNDARY test driving the REAL workmgmt.ApplyGrooming core through the
// REAL *Provider over a REAL *githubclient.Client against an httptest forge.
//
// Every assertion reads COMMITTED STATE — which calls the fake or the fixture
// server actually received — rather than the returned error alone, because a
// containment check that fired and a mutation that silently did nothing return
// the same envelope.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// groomingStates is the canonical -> board-option map the grooming tests
// resolve through. It carries Up Next, which canonicalStates does not, because
// the icebox expected-source set is {backlog, up_next}.
var groomingStates = map[string]string{
	workmgmt.CanonicalStateBacklog:    "Backlog",
	workmgmt.CanonicalStateUpNext:     "Up Next",
	workmgmt.CanonicalStateInProgress: "In Progress",
	workmgmt.CanonicalStateInReview:   "In Review",
	workmgmt.CanonicalStateBlocked:    "Blocked",
	workmgmt.CanonicalStateDone:       "Done",
}

// groomingBoardOptions is the fixture board: every canonical column plus the
// Icebox column an icebox move targets.
var groomingBoardOptions = map[string]string{
	"Backlog": "OPT_BACKLOG", "Up Next": "OPT_UPNEXT", "In Progress": "OPT_IP",
	"In Review": "OPT_IR", "Blocked": "OPT_BLOCKED", "Done": "OPT_DONE",
	"Icebox": "OPT_ICEBOX", "3": "OPT_RANK_3", "P1": "OPT_P1", "5": "OPT_EST_5",
}

// groomingAPI returns a fakeAPI primed for a grooming mutation: the board
// resolves with a projects token configured, the issue resolves to a node id,
// and its card sits at currentStatus.
func groomingAPI(currentStatus string, onBoard bool) *fakeAPI {
	return &fakeAPI{
		parentNode: "ISSUE_NODE",
		// DISTINCT ids for the child (#2237) and the parent epic (#1437), so
		// the epic-link direction assertion has something to discriminate on:
		// with one shared id, swapping AddSubIssue's parent and child
		// arguments would satisfy every assertion (#2237 review).
		nodeIDs:                 map[int]string{2237: "CHILD_NODE", 1437: "PARENT_EPIC_NODE"},
		meta:                    &githubclient.ProjectMeta{ProjectID: "PROJ", FieldID: "FIELD", StatusOptions: groomingBoardOptions},
		itemStatus:              &githubclient.ProjectItemStatus{OnBoard: onBoard, ItemID: "ITEM", Status: currentStatus},
		itemID:                  "ITEM",
		projectsTokenConfigured: true,
		getIssues: map[int]*githubclient.Issue{
			2237: {Number: 2237, Title: "t", Body: "## Summary\n\nbody", State: "open", Labels: []string{"type:feature"}},
		},
	}
}

// groomingRequest builds a cleared mutation request for kind against #2237.
func groomingRequest(kind workmgmt.GroomingMutationKind, after workmgmt.GroomingValue) workmgmt.GroomingMutationRequest {
	return workmgmt.GroomingMutationRequest{
		Target: workmgmt.Target{
			Scope:   forge.FromGitHubInstallationID(99),
			Repo:    workmgmt.Repo{Owner: "kuhlman-labs", Name: "fishhawk"},
			Project: &workmgmt.Project{Owner: "kuhlman-labs", OwnerType: "user", Number: 7},
		},
		EntryID: "hygiene:github:kuhlman-labs/fishhawk%232237:x",
		Kind:    kind,
		ItemRef: "#2237",
		After:   after,
		States:  groomingStates,
	}
}

// ---------------------------------------------------------------------------
// Per-kind dispatch
// ---------------------------------------------------------------------------

// TestApplyGroomingMutation_LabelSetUsesTheAdditiveEndpoint is the
// lost-update guard (#2237 review). The label write must go through
// AddIssueLabels (POST .../labels), which merges SERVER-SIDE, and must NOT go
// through a read-union-PATCH — the payload therefore carries ONLY the new
// label, never the full set.
//
// Two committed-state assertions carry it: the additive call was made with
// exactly the added label, and ZERO UpdateIssue PATCHes were sent. A provider
// that reverted to the union PATCH fails the second even though its final
// label set would look identical in a single-writer test.
func TestApplyGroomingMutation_LabelSetUsesTheAdditiveEndpoint(t *testing.T) {
	api := groomingAPI("Backlog", true)
	res, err := New(api).ApplyGroomingMutation(context.Background(),
		groomingRequest(workmgmt.GroomingKindLabelSet, workmgmt.GroomingValue{List: []string{"area:backend"}}))
	if err != nil {
		t.Fatalf("ApplyGroomingMutation: %v", err)
	}
	if !res.Applied {
		t.Fatalf("result = %+v, want applied", res)
	}
	if len(api.addLabelsCalls) != 1 {
		t.Fatalf("AddIssueLabels calls = %d, want 1", len(api.addLabelsCalls))
	}
	call := api.addLabelsCalls[0]
	if call.number != 2237 {
		t.Errorf("number = %d, want 2237", call.number)
	}
	// ONLY the added label. A union payload here would mean the caller is
	// still transmitting the whole set, which is the clobbering shape.
	if got := strings.Join(call.labels, ","); got != "area:backend" {
		t.Errorf("labels sent = %q, want ONLY the added area:backend — a union payload is the lost-update shape", got)
	}
	if len(api.updateIssueCalls) != 0 {
		t.Errorf("a label mutation sent %d wholesale UpdateIssue PATCH(es): %+v",
			len(api.updateIssueCalls), api.updateIssueCalls)
	}
	if got := strings.Join(res.Observed.List, ","); got != "type:feature" {
		t.Errorf("Observed = %v, want the pre-write label set", res.Observed.List)
	}
}

// concurrentLabelAPI models the two candidate write shapes over one mutable
// label set, with a BARRIER that forces both racers to complete their reads
// before either writes — the interleaving that produces a lost update, made
// deterministic instead of hoped for.
//
//   - AddIssueLabels UNIONS into the stored set (GitHub's server-side merge).
//   - UpdateIssue REPLACES it (GitHub's PATCH semantics).
//
// Both are modelled so the test discriminates: the additive path keeps both
// racers' labels, the replacing path keeps only the last writer's union.
type concurrentLabelAPI struct {
	*fakeAPI
	mu      sync.Mutex
	labels  []string
	barrier *sync.WaitGroup
}

func (c *concurrentLabelAPI) GetIssue(_ context.Context, _ forge.CredentialScope, _ githubclient.RepoRef,
	number int) (*githubclient.Issue, error) {
	c.mu.Lock()
	snapshot := append([]string(nil), c.labels...)
	c.mu.Unlock()
	// Both racers read BEFORE either writes. Without this the test would pass
	// under either shape whenever the goroutines happened to serialize.
	c.barrier.Done()
	c.barrier.Wait()
	return &githubclient.Issue{Number: number, State: "open", Labels: snapshot}, nil
}

func (c *concurrentLabelAPI) AddIssueLabels(_ context.Context, _ forge.CredentialScope, _ githubclient.RepoRef,
	_ int, labels []string) ([]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, l := range labels {
		if !slices.Contains(c.labels, l) {
			c.labels = append(c.labels, l)
		}
	}
	return append([]string(nil), c.labels...), nil
}

func (c *concurrentLabelAPI) UpdateIssue(_ context.Context, _ forge.CredentialScope, _ githubclient.RepoRef,
	number int, p githubclient.UpdateIssueParams) (*githubclient.Issue, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if p.Labels != nil {
		c.labels = append([]string(nil), *p.Labels...)
	}
	return &githubclient.Issue{Number: number, Labels: append([]string(nil), c.labels...)}, nil
}

func (c *concurrentLabelAPI) current() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.labels...)
}

// TestApplyGroomingMutation_CompetingLabelAddsBothSurvive is the concurrency
// control the review asked for (#2237 review): two applies adding DIFFERENT
// labels to the SAME issue, forced to read the same pre-state, must both
// survive.
//
// This is a genuine counterfactual vehicle rather than an assertion of intent:
// the fake models BOTH write shapes, so swapping groomingSetLabels back to a
// read-union-PATCH makes the later writer replace the earlier one's label and
// this test goes red on committed state — the stored label set — not on an
// error value.
func TestApplyGroomingMutation_CompetingLabelAddsBothSurvive(t *testing.T) {
	var barrier sync.WaitGroup
	barrier.Add(2)
	api := &concurrentLabelAPI{
		fakeAPI: groomingAPI("Backlog", true),
		labels:  []string{"type:feature"},
		barrier: &barrier,
	}
	provider := New(api)

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, label := range []string{"area:backend", "area:cli"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = provider.ApplyGroomingMutation(context.Background(),
				groomingRequest(workmgmt.GroomingKindLabelSet, workmgmt.GroomingValue{List: []string{label}}))
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent apply %d: %v", i, err)
		}
	}

	got := api.current()
	for _, want := range []string{"type:feature", "area:backend", "area:cli"} {
		if !slices.Contains(got, want) {
			t.Errorf("label %q was lost; final set = %v — a concurrent add clobbered it", want, got)
		}
	}
}

// TestApplyGroomingMutation_LabelSetAlreadyPresentSkips pins the provider-side
// no-op: a proposed label the issue already carries writes NOTHING.
func TestApplyGroomingMutation_LabelSetAlreadyPresentSkips(t *testing.T) {
	api := groomingAPI("Backlog", true)
	res, err := New(api).ApplyGroomingMutation(context.Background(),
		groomingRequest(workmgmt.GroomingKindLabelSet, workmgmt.GroomingValue{List: []string{"type:feature"}}))
	if err != nil {
		t.Fatalf("ApplyGroomingMutation: %v", err)
	}
	if !res.Skipped || res.Applied {
		t.Errorf("result = %+v, want skipped", res)
	}
	if len(api.updateIssueCalls) != 0 {
		t.Errorf("UpdateIssue calls = %d, want 0 — an already-present label must not be written",
			len(api.updateIssueCalls))
	}
}

// noLabelAdderAPI satisfies API by embedding it, and deliberately does NOT
// implement the optional labelAdder extension — the promoted method set is
// exactly API's, and AddIssueLabels is no longer a member. The embedded value
// is nil because no method is ever reached: the refusal is decided before the
// pre-read.
type noLabelAdderAPI struct{ API }

// TestApplyGroomingMutation_LabelSetWithoutTheAdditivePrimitiveIsRefused pins
// the optional-capability refusal. AddIssueLabels is an OPTIONAL extension of
// API, so an implementation can lack it — and when it does, the label mutation
// must fail LOUD with a typed not-implemented UnavailableError. The outcome
// that must not be possible is a silent fallback to a wholesale
// UpdateIssue(labels) PATCH, which is the lost-update shape
// TestApplyGroomingMutation_CompetingLabelAddsBothSurvive guards against.
func TestApplyGroomingMutation_LabelSetWithoutTheAdditivePrimitiveIsRefused(t *testing.T) {
	res, err := New(&noLabelAdderAPI{}).ApplyGroomingMutation(context.Background(),
		groomingRequest(workmgmt.GroomingKindLabelSet, workmgmt.GroomingValue{List: []string{"area:backend"}}))
	if err == nil {
		t.Fatalf("ApplyGroomingMutation = %+v, want a refusal", res)
	}
	if res != nil {
		t.Errorf("result = %+v, want nil alongside the error", res)
	}
	var unavailable *workmgmt.UnavailableError
	if !errors.As(err, &unavailable) {
		t.Fatalf("error = %v (%T), want *workmgmt.UnavailableError", err, err)
	}
	if unavailable.Reason != workmgmt.ReasonNotImplemented {
		t.Errorf("Reason = %q, want %q", unavailable.Reason, workmgmt.ReasonNotImplemented)
	}
	if unavailable.Capability != workmgmt.GroomingCapability {
		t.Errorf("Capability = %q, want %q", unavailable.Capability, workmgmt.GroomingCapability)
	}
}

// TestApplyGroomingMutation_CloseKinds pins the state_reason vocabulary for
// both destructive close kinds: a de-duplicated issue must close as
// `duplicate` and a descoped one as `not_planned` — never as `completed`,
// which would misreport it as delivered work.
func TestApplyGroomingMutation_CloseKinds(t *testing.T) {
	for _, tc := range []struct {
		kind       workmgmt.GroomingMutationKind
		wantReason string
	}{
		{workmgmt.GroomingKindCloseDuplicate, "duplicate"},
		{workmgmt.GroomingKindCloseNotPlanned, "not_planned"},
	} {
		t.Run(string(tc.kind), func(t *testing.T) {
			api := groomingAPI("Backlog", true)
			res, err := New(api).ApplyGroomingMutation(context.Background(),
				groomingRequest(tc.kind, workmgmt.GroomingValue{Scalar: "closed"}))
			if err != nil {
				t.Fatalf("ApplyGroomingMutation: %v", err)
			}
			if !res.Applied {
				t.Fatalf("result = %+v, want applied", res)
			}
			if len(api.updateIssueCalls) != 1 {
				t.Fatalf("UpdateIssue calls = %d, want 1", len(api.updateIssueCalls))
			}
			p := api.updateIssueCalls[0].params
			if p.State == nil || *p.State != "closed" {
				t.Errorf("State = %v, want closed", p.State)
			}
			if p.StateReason == nil || *p.StateReason != tc.wantReason {
				t.Errorf("StateReason = %v, want %q", p.StateReason, tc.wantReason)
			}
			if p.Labels != nil {
				t.Errorf("a close must not touch the label set: %+v", p.Labels)
			}
		})
	}
}

// TestApplyGroomingMutation_EpicLinkPersistsBothTheEdgeAndTheMarker pins the
// epic link to the IssueNodeID + AddSubIssue path File's linkEpic uses AND to
// the `Parent epic: #N` body marker.
//
// BOTH, because the write and the pre-dispatch read must observe the same
// persisted relationship (#2237 review). workmgmt.WorkItemRecord exposes no
// parent edge, so the idempotence diff reads the body marker; a write that
// persisted the link only through AddSubIssue is invisible to the next
// apply's read and re-dispatches. The marker assertion is the proof that the
// two agree.
func TestApplyGroomingMutation_EpicLinkPersistsBothTheEdgeAndTheMarker(t *testing.T) {
	api := groomingAPI("Backlog", true)
	res, err := New(api).ApplyGroomingMutation(context.Background(),
		groomingRequest(workmgmt.GroomingKindEpicLink, workmgmt.GroomingValue{Scalar: "#1437"}))
	if err != nil {
		t.Fatalf("ApplyGroomingMutation: %v", err)
	}
	if !res.Applied {
		t.Fatalf("result = %+v, want applied", res)
	}
	// DIRECTION, not merely presence: the fixture gives the parent epic
	// (#1437) and the child (#2237) DISTINCT node ids, so a provider that
	// passed them the other way round fails here. With one shared id — as this
	// fixture had — the reversed call was indistinguishable (#2237 review).
	if api.subParent != "PARENT_EPIC_NODE" {
		t.Errorf("AddSubIssue parent = %q, want PARENT_EPIC_NODE (#1437's node); the parent epic must be passed as PARENT", api.subParent)
	}
	if api.subChild != "CHILD_NODE" {
		t.Errorf("AddSubIssue child = %q, want CHILD_NODE (#2237's node); the target issue must be passed as CHILD", api.subChild)
	}
	if api.nodeIDNumber != 1437 {
		t.Errorf("last IssueNodeID number = %d, want the parent epic 1437", api.nodeIDNumber)
	}
	if len(api.updateIssueCalls) != 1 {
		t.Fatalf("UpdateIssue calls = %d, want 1 — the marker write the next read observes", len(api.updateIssueCalls))
	}
	call := api.updateIssueCalls[0]
	if call.params.Body == nil {
		t.Fatal("Body param is nil; the epic link must persist the marker the read observes")
	}
	if !strings.Contains(*call.params.Body, "Parent epic: #1437") {
		t.Errorf("body written = %q, want it to carry `Parent epic: #1437`", *call.params.Body)
	}
	// The pointer-omission invariant: a marker write must not touch labels or
	// state.
	if call.params.Labels != nil || call.params.State != nil || call.params.StateReason != nil {
		t.Errorf("epic link also set labels/state/state_reason: %+v", call.params)
	}
}

// TestApplyGroomingMutation_EpicLinkReapplyIsANoOp is the REAL-PROVIDER
// re-apply the review demanded (#2237 review). The first call's marker write
// is fed back as the issue's body — a genuine round trip through the shipped
// code, not a fake that appends a marker production never writes — and the
// second call must dispatch NOTHING.
//
// The previous idempotence coverage lived only in workmgmt's stateful fake,
// which stamped the marker itself. That did not merely miss the defect; it
// manufactured the evidence that the defect was absent. This test cannot: it
// reads the body the provider ACTUALLY wrote.
func TestApplyGroomingMutation_EpicLinkReapplyIsANoOp(t *testing.T) {
	api := groomingAPI("Backlog", true)
	provider := New(api)
	req := groomingRequest(workmgmt.GroomingKindEpicLink, workmgmt.GroomingValue{Scalar: "#1437"})

	first, err := provider.ApplyGroomingMutation(context.Background(), req)
	if err != nil {
		t.Fatalf("first apply: %v", err)
	}
	if !first.Applied {
		t.Fatalf("first apply = %+v, want applied", first)
	}
	if len(api.updateIssueCalls) != 1 || api.updateIssueCalls[0].params.Body == nil {
		t.Fatalf("first apply wrote no body: %+v", api.updateIssueCalls)
	}

	// Persist what the provider wrote — the round trip.
	api.getIssues[2237].Body = *api.updateIssueCalls[0].params.Body
	api.updateIssueCalls = nil
	api.subParent, api.subChild = "", ""

	second, err := provider.ApplyGroomingMutation(context.Background(), req)
	if err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if !second.Skipped || second.Applied {
		t.Errorf("second apply = %+v, want skipped — the relationship is already persisted", second)
	}
	if len(api.updateIssueCalls) != 0 {
		t.Errorf("re-apply PATCHed the issue again: %+v", api.updateIssueCalls)
	}
	if api.subParent != "" || api.subChild != "" {
		t.Errorf("re-apply called AddSubIssue again (parent=%q child=%q); the link already exists",
			api.subParent, api.subChild)
	}
	if second.Observed.Scalar != "#1437" {
		t.Errorf("Observed = %+v, want the already-persisted parent #1437", second.Observed)
	}
}

// TestApplyGroomingMutation_EpicLinkAlreadyMarkedSkips seeds the
// already-linked state BY CONSTRUCTION — the body carries the marker before
// the first call — so the assertion that reddens when the pre-read skip is
// removed is the BEHAVIOURAL one (nothing was written) rather than a fixture
// step. It is the read half of the same write-and-read-must-agree property
// TestApplyGroomingMutation_EpicLinkReapplyIsANoOp proves end to end.
func TestApplyGroomingMutation_EpicLinkAlreadyMarkedSkips(t *testing.T) {
	api := groomingAPI("Backlog", true)
	api.getIssues[2237].Body = "## Summary\n\nbody\n\nParent epic: #1437"

	res, err := New(api).ApplyGroomingMutation(context.Background(),
		groomingRequest(workmgmt.GroomingKindEpicLink, workmgmt.GroomingValue{Scalar: "#1437"}))
	if err != nil {
		t.Fatalf("ApplyGroomingMutation: %v", err)
	}
	if !res.Skipped || res.Applied {
		t.Errorf("result = %+v, want skipped — the relationship is already recorded", res)
	}
	if len(api.updateIssueCalls) != 0 {
		t.Errorf("an already-linked epic still PATCHed the issue: %+v", api.updateIssueCalls)
	}
	if api.subParent != "" || api.subChild != "" {
		t.Errorf("an already-linked epic still called AddSubIssue (parent=%q child=%q)", api.subParent, api.subChild)
	}
	if res.Observed.Scalar != "#1437" {
		t.Errorf("Observed = %+v, want the already-persisted parent #1437", res.Observed)
	}
}

// TestApplyGroomingMutation_EpicLinkRefusesADifferentExistingParent is the
// conflicting-parent gap (#2237 review).
//
// ensureParentEpicMarker is idempotent on the MARKER, not on the PARENT: it
// returns ANY marker-bearing body unchanged. Reading that unchanged return as
// "the requested relationship exists" made the provider report a body naming
// parent #999 as already linked to the proposed #1437 — and #999 is exactly
// the state the apply layer dispatches ON, because its pre-dispatch read diffs
// the marker VALUE. So the one case where a correction was genuinely requested
// was the one case reported as needing no correction, on every re-apply.
//
// The defined behaviour is an explicit typed REFUSAL, not a silent skip and
// not an overwrite: this provider has no primitive to re-parent with
// (AddSubIssue only ADDS an edge; there is no removal and no replace-parent),
// so rewriting the marker would leave the body claiming one parent while the
// sub-issue graph held another. The candidate fails loud, naming both, and a
// human decides.
//
// The seeded body IS the conflict, by construction — the test never calls the
// control to set up its own fixture — so removing the discrimination reddens
// the behavioural assertions below, not a setup step.
func TestApplyGroomingMutation_EpicLinkRefusesADifferentExistingParent(t *testing.T) {
	api := groomingAPI("Backlog", true)
	api.getIssues[2237].Body = "## Summary\n\nbody\n\nParent epic: #999"

	res, err := New(api).ApplyGroomingMutation(context.Background(),
		groomingRequest(workmgmt.GroomingKindEpicLink, workmgmt.GroomingValue{Scalar: "#1437"}))

	// THE CONCERN'S OWN ASSERTION FIRST, and not behind a Fatalf on the error
	// type: the defect is that a requested CORRECTION was reported as already
	// present. With the discrimination removed the provider returns
	// Skipped/"parent epic marker already present" and writes nothing — so the
	// zero-write assertions below stay green and only this one reddens.
	if res != nil {
		t.Fatalf("result = %+v, want NO result — a requested correction must not be reported as already present", res)
	}
	var conflict *ParentEpicConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("err = %v (%T), want *ParentEpicConflictError", err, err)
	}
	if conflict.Current != "#999" || conflict.Proposed != "#1437" {
		t.Errorf("conflict = %+v, want current=#999 proposed=#1437 — both parents must be named", conflict)
	}
	// Committed state: nothing was written on either surface.
	if len(api.updateIssueCalls) != 0 {
		t.Errorf("a refused epic link still PATCHed the body: %+v", api.updateIssueCalls)
	}
	if api.subParent != "" || api.subChild != "" {
		t.Errorf("a refused epic link still called AddSubIssue (parent=%q child=%q); re-parenting is not available",
			api.subParent, api.subChild)
	}
}

// TestApplyGroomingMutation_EpicLinkMatchesTheParentAcrossRefShapes is the
// CONTROL for the refusal above: the discrimination must be on the parent's
// VALUE, normalized, not on the marker's raw text. A body written `Parent
// epic: 1437` (no hash — the shape a suggested_fix may carry) names the SAME
// parent as a proposed `#1437`, so it is an already-present SKIP, not a
// conflict. Without this, a normalization regression would turn every
// unhashed marker into a permanent failure.
func TestApplyGroomingMutation_EpicLinkMatchesTheParentAcrossRefShapes(t *testing.T) {
	api := groomingAPI("Backlog", true)
	api.getIssues[2237].Body = "## Summary\n\nbody\n\nParent epic: 1437"

	res, err := New(api).ApplyGroomingMutation(context.Background(),
		groomingRequest(workmgmt.GroomingKindEpicLink, workmgmt.GroomingValue{Scalar: "#1437"}))
	if err != nil {
		t.Fatalf("ApplyGroomingMutation: %v", err)
	}
	if !res.Skipped || res.Applied {
		t.Errorf("result = %+v, want skipped — `1437` and `#1437` are the same parent", res)
	}
	if len(api.updateIssueCalls) != 0 || api.subParent != "" {
		t.Errorf("an already-linked epic still wrote: patches=%+v subParent=%q", api.updateIssueCalls, api.subParent)
	}
}

// TestApplyGroomingMutation_BoardWriteUsesTheProjectsToken closes the routing
// gap two reviewers flagged and neither could decide from a diff (#2237
// review): groomingMoveCard passes the ORIGINAL ctx to placeIssueOnBoard while
// threading boardCtx through its reads, which reads as a wrong-credential bug.
//
// It is not one — placeIssueOnBoard re-applies WithProjectsToken itself for a
// user-owned board (#1114) — but the claim was uncovered, so this asserts it
// where it matters: at the WRITE. The fake records the context flag on
// AddProjectItem and SetProjectItemSingleSelect, which the httptest fixture
// (accepting either credential) cannot discriminate.
func TestApplyGroomingMutation_BoardWriteUsesTheProjectsToken(t *testing.T) {
	// Off-board card + empty expected-source set: the placement proceeds, so
	// the write is genuinely reached.
	api := groomingAPI("", false)
	res, err := New(api).ApplyGroomingMutation(context.Background(),
		groomingRequest(workmgmt.GroomingKindBoardPlace, workmgmt.GroomingValue{Scalar: "Backlog"}))
	if err != nil {
		t.Fatalf("ApplyGroomingMutation: %v", err)
	}
	if !res.Applied {
		t.Fatalf("result = %+v, want applied — the write must be reached for this to mean anything", res)
	}
	if !api.addProjectItemProjectsToken {
		t.Error("AddProjectItem ran WITHOUT the projects-token opt-in; a user-owned board cannot be written with the installation token (#1114)")
	}
	if !api.setFieldProjectsToken {
		t.Error("SetProjectItemSingleSelect ran WITHOUT the projects-token opt-in")
	}
}

// TestApplyGroomingMutation_OrgOwnedBoardWriteStaysOnTheInstallationToken is
// the other side of that routing: the projects-token opt-in is scoped to
// USER-owned boards. An org-owned board must stay on the installation token,
// or the opt-in would be unconditional and the assertion above would prove
// nothing about routing.
func TestApplyGroomingMutation_OrgOwnedBoardWriteStaysOnTheInstallationToken(t *testing.T) {
	api := groomingAPI("", false)
	req := groomingRequest(workmgmt.GroomingKindBoardPlace, workmgmt.GroomingValue{Scalar: "Backlog"})
	req.Target.Project.OwnerType = "organization"
	res, err := New(api).ApplyGroomingMutation(context.Background(), req)
	if err != nil {
		t.Fatalf("ApplyGroomingMutation: %v", err)
	}
	if !res.Applied {
		t.Fatalf("result = %+v, want applied", res)
	}
	if api.addProjectItemProjectsToken || api.setFieldProjectsToken {
		t.Error("an ORG-owned board write took the projects-token opt-in; it is scoped to user-owned boards")
	}
}

// TestApplyGroomingMutation_DependsOnAddStampsTheMarker pins the depends_on
// write to the existing ensureDependsOnMarker body convention.
func TestApplyGroomingMutation_DependsOnAddStampsTheMarker(t *testing.T) {
	api := groomingAPI("Backlog", true)
	res, err := New(api).ApplyGroomingMutation(context.Background(),
		groomingRequest(workmgmt.GroomingKindDependsOnAdd, workmgmt.GroomingValue{Scalar: "#2236"}))
	if err != nil {
		t.Fatalf("ApplyGroomingMutation: %v", err)
	}
	if !res.Applied {
		t.Fatalf("result = %+v, want applied", res)
	}
	if len(api.updateIssueCalls) != 1 {
		t.Fatalf("UpdateIssue calls = %d, want 1", len(api.updateIssueCalls))
	}
	body := api.updateIssueCalls[0].params.Body
	if body == nil || !strings.Contains(*body, "Depends on: #2236") {
		t.Errorf("written body = %v, want the depends_on marker appended", body)
	}
	if !strings.Contains(*body, "## Summary") {
		t.Errorf("written body dropped the original content: %q", *body)
	}
}

// TestApplyGroomingMutation_DependsOnAlreadyMarkedSkips pins the idempotent
// arm of ensureDependsOnMarker: a body already carrying a marker writes
// nothing rather than re-PATCHing an identical body.
func TestApplyGroomingMutation_DependsOnAlreadyMarkedSkips(t *testing.T) {
	api := groomingAPI("Backlog", true)
	api.getIssues[2237].Body = "## Summary\n\nbody\n\nDepends on: #2236"
	res, err := New(api).ApplyGroomingMutation(context.Background(),
		groomingRequest(workmgmt.GroomingKindDependsOnAdd, workmgmt.GroomingValue{Scalar: "#2236"}))
	if err != nil {
		t.Fatalf("ApplyGroomingMutation: %v", err)
	}
	if !res.Skipped {
		t.Errorf("result = %+v, want skipped", res)
	}
	if len(api.updateIssueCalls) != 0 {
		t.Errorf("UpdateIssue calls = %d, want 0", len(api.updateIssueCalls))
	}
}

// TestApplyGroomingMutation_BoardMoves is the placement table. It carries an
// EXPLICIT ICEBOX ROW alongside board_place (approval condition I2): icebox is
// a board move and is routed through the same guard, so both must appear.
//
// Each row asserts on COMMITTED STATE — whether SetProjectItemSingleSelect was
// reached and with which option — and the refused rows additionally
// distinguish DID-NOT-TRY from TRIED-AND-WAS-REFUSED by requiring the skip
// reason, so a provider that never attempted the move for an unrelated reason
// does not pass as a working guard.
func TestApplyGroomingMutation_BoardMoves(t *testing.T) {
	for _, tc := range []struct {
		name          string
		kind          workmgmt.GroomingMutationKind
		expectedFrom  []string
		column        string
		currentStatus string
		onBoard       bool
		wantOption    string // "" = no write expected
		wantSkip      string
	}{
		{
			name: "board_place onto an off-board item", kind: workmgmt.GroomingKindBoardPlace,
			column: "Backlog", onBoard: false, wantOption: "OPT_BACKLOG",
		},
		{
			name: "board_place refused: a human already boarded it", kind: workmgmt.GroomingKindBoardPlace,
			column: "Backlog", currentStatus: "In Progress", onBoard: true,
			wantSkip: "manual_placement_preserved",
		},
		{
			name: "icebox from Backlog", kind: workmgmt.GroomingKindIcebox,
			expectedFrom:  []string{workmgmt.CanonicalStateBacklog, workmgmt.CanonicalStateUpNext},
			column:        "Icebox",
			currentStatus: "Backlog", onBoard: true, wantOption: "OPT_ICEBOX",
		},
		{
			name: "icebox from Up Next", kind: workmgmt.GroomingKindIcebox,
			expectedFrom:  []string{workmgmt.CanonicalStateBacklog, workmgmt.CanonicalStateUpNext},
			column:        "Icebox",
			currentStatus: "Up Next", onBoard: true, wantOption: "OPT_ICEBOX",
		},
		{
			// The load-bearing icebox row: a human moved the card to In
			// Progress, so parking it would override their decision.
			name: "icebox refused: a human moved it to In Progress", kind: workmgmt.GroomingKindIcebox,
			expectedFrom:  []string{workmgmt.CanonicalStateBacklog, workmgmt.CanonicalStateUpNext},
			column:        "Icebox",
			currentStatus: "In Progress", onBoard: true, wantSkip: "manual_placement_preserved",
		},
		{
			name: "icebox refused: the item carries no card at all", kind: workmgmt.GroomingKindIcebox,
			expectedFrom:  []string{workmgmt.CanonicalStateBacklog, workmgmt.CanonicalStateUpNext},
			column:        "Icebox",
			currentStatus: "", onBoard: false, wantSkip: "manual_placement_preserved",
		},
		{
			// Reachable only when the card's current column is BOTH an
			// expected source AND the target: the placement guard runs first,
			// so a card already parked outside the source set takes the
			// manual-placement branch above instead.
			name: "board move already at the target column", kind: workmgmt.GroomingKindIcebox,
			expectedFrom:  []string{workmgmt.CanonicalStateBacklog},
			column:        "Backlog",
			currentStatus: "Backlog", onBoard: true, wantSkip: "already at target column",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := groomingAPI(tc.currentStatus, tc.onBoard)
			req := groomingRequest(tc.kind, workmgmt.GroomingValue{Scalar: tc.column})
			req.ExpectedFrom = tc.expectedFrom
			res, err := New(api).ApplyGroomingMutation(context.Background(), req)
			if err != nil {
				t.Fatalf("ApplyGroomingMutation: %v", err)
			}
			if tc.wantSkip != "" {
				if !res.Skipped || res.SkipReason != tc.wantSkip {
					t.Errorf("result = %+v, want skipped %q", res, tc.wantSkip)
				}
				if api.setOptionID != "" {
					t.Errorf("SetProjectItemSingleSelect reached with option %q; the guard must refuse BEFORE the write",
						api.setOptionID)
				}
				return
			}
			if !res.Applied {
				t.Fatalf("result = %+v, want applied", res)
			}
			if api.setOptionID != tc.wantOption {
				t.Errorf("set option = %q, want %q", api.setOptionID, tc.wantOption)
			}
		})
	}
}

// TestApplyGroomingMutation_ValueSetKindsWriteFields pins the three value-set
// kinds to a board FIELD write on the field their kind names — and, per
// approval condition I3, that a rank_set is a FIELD write: the assertion is on
// the resolved field NAME and option, with no positional primitive anywhere in
// the call log.
func TestApplyGroomingMutation_ValueSetKindsWriteFields(t *testing.T) {
	for _, tc := range []struct {
		kind      workmgmt.GroomingMutationKind
		value     string
		wantField string
		wantOpt   string
	}{
		{workmgmt.GroomingKindRankSet, "3", "Rank", "OPT_RANK_3"},
		{workmgmt.GroomingKindPrioritySet, "P1", "Priority", "OPT_P1"},
		{workmgmt.GroomingKindFieldSet, "5", "Estimate", "OPT_EST_5"},
	} {
		t.Run(string(tc.kind), func(t *testing.T) {
			api := groomingAPI("Backlog", true)
			res, err := New(api).ApplyGroomingMutation(context.Background(),
				groomingRequest(tc.kind, workmgmt.GroomingValue{Scalar: tc.value}))
			if err != nil {
				t.Fatalf("ApplyGroomingMutation: %v", err)
			}
			if !res.Applied {
				t.Fatalf("result = %+v, want applied", res)
			}
			if api.fieldsName != tc.wantField {
				t.Errorf("resolved field = %q, want %q", api.fieldsName, tc.wantField)
			}
			if api.setOptionID != tc.wantOpt {
				t.Errorf("set option = %q, want %q", api.setOptionID, tc.wantOpt)
			}
		})
	}
}

// TestApplyGroomingMutation_FieldWriteOnOffBoardItemSkips pins the field
// write's off-board branch: an item with no card has no field to write, so it
// SKIPS rather than resolving a card that does not exist.
func TestApplyGroomingMutation_FieldWriteOnOffBoardItemSkips(t *testing.T) {
	api := groomingAPI("", false)
	res, err := New(api).ApplyGroomingMutation(context.Background(),
		groomingRequest(workmgmt.GroomingKindRankSet, workmgmt.GroomingValue{Scalar: "3"}))
	if err != nil {
		t.Fatalf("ApplyGroomingMutation: %v", err)
	}
	if !res.Skipped || res.SkipReason != "not on board" {
		t.Errorf("result = %+v, want a not-on-board skip", res)
	}
	if api.setOptionID != "" {
		t.Errorf("SetProjectItemSingleSelect reached for an off-board item (option %q)", api.setOptionID)
	}
}

// ---------------------------------------------------------------------------
// Defensive branches — one behavioral test per branch, each asserting the
// TYPED error AND that nothing reached the forge.
// ---------------------------------------------------------------------------

// TestApplyGroomingMutation_UnhandledKindIsTypedNotSilent is the I5 principle
// generalized: a kind with no wired primitive returns
// *UnsupportedGroomingKindError and writes nothing. A silent no-op here would
// put a fabricated "applied" row in the audit trail.
func TestApplyGroomingMutation_UnhandledKindIsTypedNotSilent(t *testing.T) {
	api := groomingAPI("Backlog", true)
	res, err := New(api).ApplyGroomingMutation(context.Background(),
		groomingRequest(workmgmt.GroomingMutationKind("teleport_to_mars"), workmgmt.GroomingValue{Scalar: "x"}))
	if res != nil {
		t.Errorf("result = %+v, want nil alongside the typed error", res)
	}
	var ue *UnsupportedGroomingKindError
	if !errors.As(err, &ue) {
		t.Fatalf("err = %v (%T), want *UnsupportedGroomingKindError", err, err)
	}
	if ue.Kind != "teleport_to_mars" {
		t.Errorf("Kind = %q, want the offending kind named", ue.Kind)
	}
	if len(api.updateIssueCalls) != 0 || api.setOptionID != "" {
		t.Error("an unhandled kind reached the forge")
	}
}

// TestApplyGroomingMutation_IceboxWithNoColumnIsTypedNotSilent is approval
// condition I5 asserted at the provider layer. ApplyGrooming refuses this
// upstream with GroomingSkipIceboxColumnUnavailable; a caller that reaches the
// provider anyway gets a TYPED refusal — never a silent no-op, and never a
// misroute to some other column, which the assertion on setOptionID pins.
func TestApplyGroomingMutation_IceboxWithNoColumnIsTypedNotSilent(t *testing.T) {
	api := groomingAPI("Backlog", true)
	req := groomingRequest(workmgmt.GroomingKindIcebox, workmgmt.GroomingValue{})
	req.ExpectedFrom = []string{workmgmt.CanonicalStateBacklog}
	res, err := New(api).ApplyGroomingMutation(context.Background(), req)
	if res != nil {
		t.Errorf("result = %+v, want nil alongside the typed error", res)
	}
	var ue *UnsupportedGroomingKindError
	if !errors.As(err, &ue) {
		t.Fatalf("err = %v (%T), want *UnsupportedGroomingKindError", err, err)
	}
	if api.setOptionID != "" {
		t.Errorf("an icebox move with no column reached the board and set option %q — a misroute", api.setOptionID)
	}
}

// TestApplyGroomingMutation_IceboxColumnNotOnBoardIsTypedNotSilent is the
// other half of I5: an icebox column that IS configured but is not an option
// on the project fails loud rather than writing some nearest column.
func TestApplyGroomingMutation_IceboxColumnNotOnBoardIsTypedNotSilent(t *testing.T) {
	api := groomingAPI("Backlog", true)
	api.meta = &githubclient.ProjectMeta{ProjectID: "PROJ", FieldID: "FIELD",
		StatusOptions: map[string]string{"Backlog": "OPT_BACKLOG"}} // no Icebox column
	req := groomingRequest(workmgmt.GroomingKindIcebox, workmgmt.GroomingValue{Scalar: "Icebox"})
	req.ExpectedFrom = []string{workmgmt.CanonicalStateBacklog}
	res, err := New(api).ApplyGroomingMutation(context.Background(), req)
	if res != nil {
		t.Errorf("result = %+v, want nil alongside the typed error", res)
	}
	var ue *UnsupportedGroomingKindError
	if !errors.As(err, &ue) || !strings.Contains(ue.Detail, "not a Status option") {
		t.Fatalf("err = %v (%T), want an unmapped-column *UnsupportedGroomingKindError", err, err)
	}
	if api.setOptionID != "" {
		t.Errorf("a missing icebox column still set option %q", api.setOptionID)
	}
}

// TestApplyGroomingMutation_TypedDegradations pins each capability-unavailable
// branch to its exact Reason, with a nil result and nothing written.
func TestApplyGroomingMutation_TypedDegradations(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mutate  func(*fakeAPI, *workmgmt.GroomingMutationRequest)
		kind    workmgmt.GroomingMutationKind
		after   workmgmt.GroomingValue
		wantRsn workmgmt.UnavailableReason
	}{
		{
			name:    "no installation scope",
			mutate:  func(_ *fakeAPI, r *workmgmt.GroomingMutationRequest) { r.Target.Scope = forge.CredentialScope{} },
			kind:    workmgmt.GroomingKindLabelSet,
			after:   workmgmt.GroomingValue{List: []string{"area:backend"}},
			wantRsn: workmgmt.ReasonNoInstallation,
		},
		{
			name:    "board move with no project configured",
			mutate:  func(_ *fakeAPI, r *workmgmt.GroomingMutationRequest) { r.Target.Project = nil },
			kind:    workmgmt.GroomingKindBoardPlace,
			after:   workmgmt.GroomingValue{Scalar: "Backlog"},
			wantRsn: workmgmt.ReasonNoProjectConfigured,
		},
		{
			name:    "board move on a user-owned board with no projects token",
			mutate:  func(a *fakeAPI, _ *workmgmt.GroomingMutationRequest) { a.projectsTokenConfigured = false },
			kind:    workmgmt.GroomingKindBoardPlace,
			after:   workmgmt.GroomingValue{Scalar: "Backlog"},
			wantRsn: workmgmt.ReasonNoProjectsToken,
		},
		{
			name:    "field write with no project configured",
			mutate:  func(_ *fakeAPI, r *workmgmt.GroomingMutationRequest) { r.Target.Project = nil },
			kind:    workmgmt.GroomingKindRankSet,
			after:   workmgmt.GroomingValue{Scalar: "3"},
			wantRsn: workmgmt.ReasonNoProjectConfigured,
		},
		{
			name:    "field write on a user-owned board with no projects token",
			mutate:  func(a *fakeAPI, _ *workmgmt.GroomingMutationRequest) { a.projectsTokenConfigured = false },
			kind:    workmgmt.GroomingKindRankSet,
			after:   workmgmt.GroomingValue{Scalar: "3"},
			wantRsn: workmgmt.ReasonNoProjectsToken,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := groomingAPI("Backlog", false)
			req := groomingRequest(tc.kind, tc.after)
			tc.mutate(api, &req)
			res, err := New(api).ApplyGroomingMutation(context.Background(), req)
			if res != nil {
				t.Errorf("result = %+v, want nil alongside the unavailable error", res)
			}
			var ue *workmgmt.UnavailableError
			if !errors.As(err, &ue) {
				t.Fatalf("err = %v (%T), want *workmgmt.UnavailableError", err, err)
			}
			if ue.Reason != tc.wantRsn {
				t.Errorf("Reason = %q, want %q", ue.Reason, tc.wantRsn)
			}
			if ue.Capability != workmgmt.GroomingCapability {
				t.Errorf("Capability = %q, want %q", ue.Capability, workmgmt.GroomingCapability)
			}
			if len(api.updateIssueCalls) != 0 || api.setOptionID != "" {
				t.Error("a degraded mutation still reached the forge")
			}
		})
	}
}

// TestApplyGroomingMutation_PreflightErrors pins the programming-error guards
// and the unparseable item ref.
func TestApplyGroomingMutation_PreflightErrors(t *testing.T) {
	base := groomingRequest(workmgmt.GroomingKindLabelSet, workmgmt.GroomingValue{List: []string{"l"}})

	if _, err := New(nil).ApplyGroomingMutation(context.Background(), base); err == nil ||
		!strings.Contains(err.Error(), "missing API client") {
		t.Errorf("err = %v, want a missing-API-client refusal", err)
	}
	noRepo := base
	noRepo.Target.Repo = workmgmt.Repo{}
	if _, err := New(groomingAPI("Backlog", true)).ApplyGroomingMutation(context.Background(), noRepo); err == nil ||
		!strings.Contains(err.Error(), "repo owner and name required") {
		t.Errorf("err = %v, want a repo refusal", err)
	}
	badRef := base
	badRef.ItemRef = "not-a-number"
	api := groomingAPI("Backlog", true)
	if _, err := New(api).ApplyGroomingMutation(context.Background(), badRef); err == nil ||
		!strings.Contains(err.Error(), "not a numeric issue reference") {
		t.Errorf("err = %v, want an unparseable-ref refusal", err)
	}
	if len(api.updateIssueCalls) != 0 {
		t.Error("an unparseable item ref still reached the forge")
	}
}

// TestApplyGroomingMutation_MissingProposedValues pins the three "the caller
// proposed nothing to write" branches to typed refusals.
func TestApplyGroomingMutation_MissingProposedValues(t *testing.T) {
	for _, kind := range []workmgmt.GroomingMutationKind{
		workmgmt.GroomingKindEpicLink, workmgmt.GroomingKindDependsOnAdd, workmgmt.GroomingKindRankSet,
	} {
		t.Run(string(kind), func(t *testing.T) {
			api := groomingAPI("Backlog", true)
			_, err := New(api).ApplyGroomingMutation(context.Background(),
				groomingRequest(kind, workmgmt.GroomingValue{}))
			var ue *UnsupportedGroomingKindError
			if !errors.As(err, &ue) {
				t.Fatalf("err = %v (%T), want *UnsupportedGroomingKindError", err, err)
			}
			if len(api.updateIssueCalls) != 0 || api.subParent != "" || api.setOptionID != "" {
				t.Error("a mutation with no proposed value still reached the forge")
			}
		})
	}
}

// TestApplyGroomingMutation_ForgeErrorsSurface pins each forge-failure branch
// to a non-nil error rather than a fabricated applied result.
func TestApplyGroomingMutation_ForgeErrorsSurface(t *testing.T) {
	for _, tc := range []struct {
		name    string
		kind    workmgmt.GroomingMutationKind
		after   workmgmt.GroomingValue
		onBoard bool
		break_  func(*fakeAPI)
	}{
		{"label read fails", workmgmt.GroomingKindLabelSet, workmgmt.GroomingValue{List: []string{"x"}}, false,
			func(a *fakeAPI) { a.getIssueErr = errors.New("boom") }},
		{"label write fails", workmgmt.GroomingKindLabelSet, workmgmt.GroomingValue{List: []string{"x"}}, false,
			func(a *fakeAPI) { a.addLabelsErr = errors.New("boom") }},
		{"close write fails", workmgmt.GroomingKindCloseDuplicate, workmgmt.GroomingValue{Scalar: "closed"}, false,
			func(a *fakeAPI) { a.updateIssueErr = errors.New("boom") }},
		{"depends_on read fails", workmgmt.GroomingKindDependsOnAdd, workmgmt.GroomingValue{Scalar: "#1"}, false,
			func(a *fakeAPI) { a.getIssueErr = errors.New("boom") }},
		{"epic child node resolution fails", workmgmt.GroomingKindEpicLink, workmgmt.GroomingValue{Scalar: "#1"}, false,
			func(a *fakeAPI) { a.nodeIDErr = errors.New("boom") }},
		{"epic sub-issue link fails", workmgmt.GroomingKindEpicLink, workmgmt.GroomingValue{Scalar: "#1"}, false,
			func(a *fakeAPI) { a.subErr = errors.New("boom") }},
		{"epic body read fails", workmgmt.GroomingKindEpicLink, workmgmt.GroomingValue{Scalar: "#1"}, false,
			func(a *fakeAPI) { a.getIssueErr = errors.New("boom") }},
		{"epic marker write fails", workmgmt.GroomingKindEpicLink, workmgmt.GroomingValue{Scalar: "#1"}, false,
			func(a *fakeAPI) { a.updateIssueErr = errors.New("boom") }},
		{"board fields resolution fails", workmgmt.GroomingKindBoardPlace, workmgmt.GroomingValue{Scalar: "Backlog"}, false,
			func(a *fakeAPI) { a.fieldsErr = errors.New("boom") }},
		{"board item read fails", workmgmt.GroomingKindBoardPlace, workmgmt.GroomingValue{Scalar: "Backlog"}, false,
			func(a *fakeAPI) { a.itemStatusErr = errors.New("boom") }},
		{"board write fails", workmgmt.GroomingKindBoardPlace, workmgmt.GroomingValue{Scalar: "Backlog"}, false,
			func(a *fakeAPI) { a.setErr = errors.New("boom") }},
		{"field write fails", workmgmt.GroomingKindRankSet, workmgmt.GroomingValue{Scalar: "3"}, true,
			func(a *fakeAPI) { a.setErr = errors.New("boom") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := groomingAPI("Backlog", tc.onBoard)
			tc.break_(api)
			res, err := New(api).ApplyGroomingMutation(context.Background(), groomingRequest(tc.kind, tc.after))
			if err == nil {
				t.Fatalf("result = %+v, err = nil; a forge failure must never report applied", res)
			}
			if res != nil {
				t.Errorf("result = %+v, want nil alongside the error", res)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// CROSS-BOUNDARY: the real ApplyGrooming core -> the real Provider -> a real
// *githubclient.Client -> an httptest forge.
// ---------------------------------------------------------------------------

// groomingForge is an httptest GitHub that records every request the apply
// makes, so the assertions read the calls the forge ACTUALLY received.
type groomingForge struct {
	patches []string // "<path> <body>", one per PATCH
	posts   []string // "<path> <body>", one per non-GraphQL POST
	graphql []string // one operation name per GraphQL call
	// bodies is EVERY request body the forge received, whatever the method or
	// route, recorded ahead of the mux. It is what lets the cross-boundary
	// test assert a NEGATIVE — that no request anywhere carried the
	// `suggested_fix` prose — instead of only checking the routes it thought
	// to look at.
	bodies   []string
	itemCols map[string]string
}

// newGroomingForge stands up the REST + GraphQL surface an end-to-end grooming
// apply touches: GET/PATCH issue, and the ProjectFields / projectItems /
// AddItem / SetField GraphQL documents.
func newGroomingForge(t *testing.T, currentColumn string) (*githubclient.Client, *groomingForge) {
	t.Helper()
	fx := &groomingForge{itemCols: map[string]string{}}
	mux := http.NewServeMux()

	mux.HandleFunc("GET /repos/{owner}/{repo}/issues/{number}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, fmt.Sprintf(
			`{"number":%s,"node_id":"ISSUE_NODE","title":"t","body":"## Summary","state":"open","labels":[{"name":"type:feature"}]}`,
			r.PathValue("number")))
	})
	mux.HandleFunc("PATCH /repos/{owner}/{repo}/issues/{number}", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		fx.patches = append(fx.patches, r.URL.Path+" "+string(raw))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"number":1,"state":"open","labels":[{"name":"type:feature"}]}`)
	})
	// The ADDITIVE label endpoint. It is a DISTINCT route from the PATCH
	// above, which is what lets the end-to-end assertions tell a server-side
	// merge from a wholesale replacement rather than seeing one blurred
	// "the labels changed".
	mux.HandleFunc("POST /repos/{owner}/{repo}/issues/{number}/labels", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		fx.posts = append(fx.posts, r.URL.Path+" "+string(raw))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `[{"name":"type:feature"},{"name":"area:backend"}]`)
	})
	mux.HandleFunc("POST /graphql", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Query string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(body.Query, "ProjectFields"):
			fx.graphql = append(fx.graphql, "ProjectFields")
			_, _ = io.WriteString(w, `{"data":{"user":{"projectV2":{"id":"PROJ","field":{"id":"FIELD","options":[`+
				`{"id":"OPT_BACKLOG","name":"Backlog"},{"id":"OPT_ICEBOX","name":"Icebox"},`+
				`{"id":"OPT_RANK_1","name":"1"},{"id":"OPT_RANK_3","name":"3"}]}}}}}`)
		case strings.Contains(body.Query, "projectItems"):
			fx.graphql = append(fx.graphql, "ProjectItemStatus")
			_, _ = io.WriteString(w, `{"data":{"node":{"projectItems":{"pageInfo":{"hasNextPage":false},"nodes":[`+
				`{"id":"ITEM","project":{"id":"PROJ"},"fieldValueByName":{"name":"`+currentColumn+`"}}]}}}}`)
		case strings.Contains(body.Query, "AddItem"):
			fx.graphql = append(fx.graphql, "AddItem")
			_, _ = io.WriteString(w, `{"data":{"addProjectV2ItemById":{"item":{"id":"ITEM"}}}}`)
		case strings.Contains(body.Query, "SetField"):
			fx.graphql = append(fx.graphql, "SetField")
			_, _ = io.WriteString(w, `{"data":{"updateProjectV2ItemFieldValue":{"projectV2Item":{"id":"ITEM"}}}}`)
		default:
			fx.graphql = append(fx.graphql, "unknown")
			_, _ = io.WriteString(w, `{"data":{}}`)
		}
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()
		fx.bodies = append(fx.bodies, r.Method+" "+r.URL.Path+" "+string(raw))
		r.Body = io.NopCloser(strings.NewReader(string(raw)))
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	c := githubclient.New(stubTokenProvider{token: "ghs_install"})
	c.BaseURL = srv.URL
	c.HTTP = &http.Client{Timeout: 5 * time.Second}
	c.ProjectsToken = "pat_projects"
	return c, fx
}

// recordingSink collects the audit rows so the end-to-end test can assert
// every candidate was audited.
type recordingSink struct {
	records []workmgmt.GroomingMutationRecord
	summary workmgmt.GroomingApplySummary
}

func (s *recordingSink) RecordGroomingMutation(_ context.Context, rec workmgmt.GroomingMutationRecord) error {
	s.records = append(s.records, rec)
	return nil
}

func (s *recordingSink) RecordGroomingApplyCompleted(_ context.Context, sum workmgmt.GroomingApplySummary) error {
	s.summary = sum
	return nil
}

func groomingItemRef(number int) plan.ItemRef {
	return plan.ItemRef{Type: "github_issue", ID: fmt.Sprintf("kuhlman-labs/fishhawk#%d", number)}
}

// TestApplyGrooming_EndToEndThroughGitHubProvider is the cross-boundary seam
// per-layer units cannot cover (#618, approval condition C2 of #2847): ONE
// CONTINUOUS test from SERIALIZED report bytes through schema ingestion, domain
// decoding, derivation, request resolution and the actual forge request body.
//
// It starts from hand-authored JSON — the bytes a groomer emits — rather than
// from an in-memory struct, because the serialization seam is exactly where a
// new schema field most plausibly gets lost: a missing json tag, an unsynced
// embedded mirror, a decoder that drops the member. Any of those makes
// `hygiene.fix` decode as nil, the entry skip, and the label POST vanish — so
// the POST-body assertion below is the seam's own alarm.
//
// The fixture report carries four entries exercising the four outcomes that
// matter: an APPROVED hygiene label fix (dispatches), an APPROVED gated
// ordering write (dispatches — ordering is non-destructive), a REJECTED
// duplicate (must NOT close), and a REPORT-MODE decomposition suggestion whose
// entry is BOTH approved AND gate-approved (must NOT move the card — approval
// condition I1's report-mode-beats-gate-approval rule, asserted here against
// the real forge rather than only against the core's fake mutator).
//
// WHAT THIS PROVES AND WHAT IT DOES NOT (approval condition I4): it proves the
// emitted request SHAPE and the set of calls made and not made. It does NOT
// prove GitHub accepts that shape — whether the forge honours the `duplicate`
// state_reason on a close needs live validation.
func TestApplyGrooming_EndToEndThroughGitHubProvider(t *testing.T) {
	c, fx := newGroomingForge(t, "Backlog")
	provider := New(c)

	// The `suggested_fix` PROSE is lexically disjoint from the structured
	// label (approval condition C1), so "no request body carries the prose" is
	// both satisfiable for correct behaviour and RED the moment the apply path
	// reads the prose instead of `fix`.
	const prose = "Please attach the ownership marking for platform duties"
	const label = "area:backend"

	hygieneRef := groomingItemRef(2237)
	orderingRef := groomingItemRef(2238)
	dupA, dupB := groomingItemRef(2239), groomingItemRef(2240)
	decompRef := groomingItemRef(2241)

	hygieneID := plan.GroomingEntryID(plan.GroomingClassHygiene, "missing_label_namespace", hygieneRef)
	orderingID := plan.GroomingEntryID(plan.GroomingClassOrdering, "", orderingRef)
	dupID := plan.GroomingEntryID(plan.GroomingClassDuplicate, "", dupA, dupB)
	decompID := plan.GroomingEntryID(plan.GroomingClassDecomposition, "", decompRef)

	ref := func(r plan.ItemRef) string {
		return fmt.Sprintf(`{"type":%q,"id":%q,"url":%q}`, r.Type, r.ID, r.URL)
	}
	// AUTHORED BYTES, not a marshalled struct: `"fix"` is written here exactly
	// as a groomer would emit it, so the json tag, the embedded schema mirror
	// and the strict decoder are all on the path under test.
	doc := []byte(fmt.Sprintf(`{
  "kind": "grooming_report",
  "report_version": "grooming_report_v1",
  "ticket_reference": {"type":"github_issue","url":"https://github.com/kuhlman-labs/fishhawk/issues/2847","id":"kuhlman-labs/fishhawk#2847"},
  "generated_by": {"agent":"claude-code","model":"claude-opus-5","timestamp":"2026-08-23T00:00:00Z"},
  "summary": "One entry per outcome that matters.",
  "ordering": [{"id":%q,"item_ref":%s,"rank":1,"score":9.0,"rubric_citations":[{"rubric_id":"U1"}]}],
  "duplicates": [{"id":%q,"pair":[%s,%s],"basis":"same proposal","confidence":"high"}],
  "hygiene_defects": [{"id":%q,"item_ref":%s,"defect":"missing_label_namespace","detail":"no area label","suggested_fix":%q,"fix":{"labels":[%q]}}],
  "dependency_edges": [],
  "vision_drift": [],
  "decomposition_suggestions": [{"id":%q,"item_ref":%s,"rationale":"too big","proposed_children":[{"title":"a","scope_hint":"x"},{"title":"b","scope_hint":"y"}]}]
}`,
		orderingID, ref(orderingRef),
		dupID, ref(dupA), ref(dupB),
		hygieneID, ref(hygieneRef), prose, label,
		decompID, ref(decompRef)))

	// INGESTION + DOMAIN DECODING — the production parser, not a test decoder.
	report, err := plan.ParseGroomingReport(doc)
	if err != nil {
		t.Fatalf("ParseGroomingReport: %v", err)
	}
	if fix := report.HygieneDefects[0].Fix; fix == nil || len(fix.Labels) != 1 || fix.Labels[0] != label {
		t.Fatalf("the structured fix did not survive serialization: %+v — a json tag or an unsynced schema mirror dropped it",
			report.HygieneDefects[0])
	}

	sink := &recordingSink{}
	res, err := workmgmt.ApplyGrooming(context.Background(), provider, provider, sink, workmgmt.GroomingApplyRequest{
		Target: workmgmt.Target{
			Scope:   forge.FromGitHubInstallationID(99),
			Repo:    workmgmt.Repo{Owner: "kuhlman-labs", Name: "fishhawk"},
			Project: &workmgmt.Project{Owner: "kuhlman-labs", OwnerType: "user", Number: 7},
		},
		Report: report,
		Decisions: []workmgmt.GroomingDecision{
			{EntryID: hygieneID, Verdict: workmgmt.GroomingApproved},
			{EntryID: orderingID, Verdict: workmgmt.GroomingApproved},
			{EntryID: dupID, Verdict: workmgmt.GroomingRejected, CloseTarget: dupA.ID},
			{EntryID: decompID, Verdict: workmgmt.GroomingApproved},
		},
		Modes: map[string]workmgmt.GroomingMode{
			"hygiene":  workmgmt.GroomingModeAuto,
			"ordering": workmgmt.GroomingModeGated,
			"dedup":    workmgmt.GroomingModeAuto,
			// The I1 row: report mode on the class carrying the destructive
			// icebox kind, whose entry ALSO holds an explicit gate approval.
			"scoping": workmgmt.GroomingModeReport,
		},
		GateApproved: map[string]bool{decompID: true},
		States:       groomingStates,
		IceboxColumn: "Icebox",
	})
	if err != nil {
		t.Fatalf("ApplyGrooming: %v", err)
	}

	// THE FORGE REQUEST BODY — the far end of the seam. The label fix went out
	// on the ADDITIVE endpoint carrying EXACTLY the structured label name, and
	// ZERO PATCHes reached the forge (no wholesale label replacement, no
	// close). AC1 requires the assertion be against the labels REQUESTED OF
	// THE PROVIDER, not against the call merely succeeding.
	if len(fx.posts) != 1 {
		t.Fatalf("additive label POSTs = %v, want exactly one", fx.posts)
	}
	if !strings.Contains(fx.posts[0], "/repos/kuhlman-labs/fishhawk/issues/2237/labels") {
		t.Errorf("POST path = %q, want the hygiene item's labels endpoint", fx.posts[0])
	}
	if !strings.Contains(fx.posts[0], `{"labels":["`+label+`"]}`) {
		t.Errorf("POST body = %q, want ONLY the structured label — a union payload is the lost-update shape", fx.posts[0])
	}
	if len(fx.patches) != 0 {
		t.Errorf("PATCH calls = %v, want none: the label write is additive and the duplicate close was REJECTED", fx.patches)
	}

	// THE NEGATIVE ARM (#2847): NO request body the forge received — on any
	// route, by any method — carries the `suggested_fix` prose. This is what
	// makes the seam prove the prose never reaches the forge, rather than only
	// that the label route looked right.
	var proseWords []string
	for _, w := range strings.Fields(prose) {
		if len(w) >= 4 {
			proseWords = append(proseWords, w)
		}
	}
	for _, body := range fx.bodies {
		if strings.Contains(body, prose) {
			t.Errorf("a forge request carries the whole suggested_fix sentence: %s", body)
			continue
		}
		for _, w := range proseWords {
			if strings.Contains(body, w) {
				t.Errorf("a forge request carries the prose word %q — a value derived from suggested_fix reached the forge: %s", w, body)
			}
		}
	}

	// The report-mode entry is the only board move in the report, so ZERO
	// board writes proves report mode beat the gate approval (condition I1).
	if n := countOp(fx.graphql, "SetField"); n != 1 {
		t.Errorf("SetField calls = %d, want exactly 1 (the ordering rank write; the report-mode icebox must not move a card)", n)
	}

	// Outcome bookkeeping: hygiene + ordering applied, duplicate and
	// decomposition skipped, and EVERY candidate audited.
	if len(res.Applied) != 2 {
		t.Errorf("applied = %d (%+v), want 2", len(res.Applied), res.Applied)
	}
	if len(res.Failed) != 0 {
		t.Errorf("failed = %+v, want none", res.Failed)
	}
	if len(sink.records) != 4 {
		t.Errorf("audit records = %d, want one per candidate (4)", len(sink.records))
	}
	var sawReportMode, sawRejected bool
	for _, rec := range sink.records {
		switch rec.EntryID {
		case decompID:
			sawReportMode = true
			if rec.SkipReason != workmgmt.GroomingSkipReportMode {
				t.Errorf("decomposition skip reason = %q, want %q (report mode beats the gate approval)",
					rec.SkipReason, workmgmt.GroomingSkipReportMode)
			}
		case dupID:
			sawRejected = true
			if rec.SkipReason != workmgmt.GroomingSkipNotApproved {
				t.Errorf("duplicate skip reason = %q, want %q", rec.SkipReason, workmgmt.GroomingSkipNotApproved)
			}
		}
	}
	if !sawReportMode || !sawRejected {
		t.Error("the report-mode and rejected candidates were not both audited")
	}
}

func countOp(ops []string, want string) int {
	n := 0
	for _, op := range ops {
		if op == want {
			n++
		}
	}
	return n
}
