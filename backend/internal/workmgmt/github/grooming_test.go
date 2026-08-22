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
	"strings"
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
		parentNode:              "ISSUE_NODE",
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

// TestApplyGroomingMutation_LabelSetSendsTheUnion is the wholesale-replace
// guard: GitHub's PATCH REPLACES the label array, so the provider must read
// the current set and send the UNION. The fixture issue already carries
// type:feature; the assertion is on the WRITTEN params, so a provider that
// sent only the proposed label (silently stripping type:feature) fails here.
func TestApplyGroomingMutation_LabelSetSendsTheUnion(t *testing.T) {
	api := groomingAPI("Backlog", true)
	res, err := New(api).ApplyGroomingMutation(context.Background(),
		groomingRequest(workmgmt.GroomingKindLabelSet, workmgmt.GroomingValue{List: []string{"area:backend"}}))
	if err != nil {
		t.Fatalf("ApplyGroomingMutation: %v", err)
	}
	if !res.Applied {
		t.Fatalf("result = %+v, want applied", res)
	}
	if len(api.updateIssueCalls) != 1 {
		t.Fatalf("UpdateIssue calls = %d, want 1", len(api.updateIssueCalls))
	}
	call := api.updateIssueCalls[0]
	if call.number != 2237 {
		t.Errorf("number = %d, want 2237", call.number)
	}
	if call.params.Labels == nil {
		t.Fatal("Labels param is nil; the label mutation must set it")
	}
	if got := strings.Join(*call.params.Labels, ","); got != "type:feature,area:backend" {
		t.Errorf("labels written = %q, want the UNION type:feature,area:backend", got)
	}
	// The pointer-omission invariant at the provider layer: a label mutation
	// must not also rewrite the body or the state.
	if call.params.Body != nil || call.params.State != nil || call.params.StateReason != nil {
		t.Errorf("label mutation also set body/state/state_reason: %+v", call.params)
	}
	if got := strings.Join(res.Observed.List, ","); got != "type:feature" {
		t.Errorf("Observed = %v, want the pre-write label set", res.Observed.List)
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

// TestApplyGroomingMutation_EpicLinkUsesSubIssues pins the epic link to the
// same IssueNodeID + AddSubIssue path File's linkEpic uses.
func TestApplyGroomingMutation_EpicLinkUsesSubIssues(t *testing.T) {
	api := groomingAPI("Backlog", true)
	res, err := New(api).ApplyGroomingMutation(context.Background(),
		groomingRequest(workmgmt.GroomingKindEpicLink, workmgmt.GroomingValue{Scalar: "#1437"}))
	if err != nil {
		t.Fatalf("ApplyGroomingMutation: %v", err)
	}
	if !res.Applied {
		t.Fatalf("result = %+v, want applied", res)
	}
	if api.subParent != "ISSUE_NODE" || api.subChild != "ISSUE_NODE" {
		t.Errorf("AddSubIssue(parent=%q, child=%q), want both resolved node ids", api.subParent, api.subChild)
	}
	if api.nodeIDNumber != 1437 {
		t.Errorf("last IssueNodeID number = %d, want the parent epic 1437", api.nodeIDNumber)
	}
	if len(api.updateIssueCalls) != 0 {
		t.Errorf("an epic link must not PATCH the issue: %+v", api.updateIssueCalls)
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
			func(a *fakeAPI) { a.updateIssueErr = errors.New("boom") }},
		{"close write fails", workmgmt.GroomingKindCloseDuplicate, workmgmt.GroomingValue{Scalar: "closed"}, false,
			func(a *fakeAPI) { a.updateIssueErr = errors.New("boom") }},
		{"depends_on read fails", workmgmt.GroomingKindDependsOnAdd, workmgmt.GroomingValue{Scalar: "#1"}, false,
			func(a *fakeAPI) { a.getIssueErr = errors.New("boom") }},
		{"epic child node resolution fails", workmgmt.GroomingKindEpicLink, workmgmt.GroomingValue{Scalar: "#1"}, false,
			func(a *fakeAPI) { a.nodeIDErr = errors.New("boom") }},
		{"epic sub-issue link fails", workmgmt.GroomingKindEpicLink, workmgmt.GroomingValue{Scalar: "#1"}, false,
			func(a *fakeAPI) { a.subErr = errors.New("boom") }},
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
	patches  []string // "<path> <body>", one per PATCH
	graphql  []string // one operation name per GraphQL call
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
	mux.HandleFunc("POST /graphql", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Query string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(body.Query, "ProjectFields"):
			fx.graphql = append(fx.graphql, "ProjectFields")
			_, _ = io.WriteString(w, `{"data":{"user":{"projectV2":{"id":"PROJ","field":{"id":"FIELD","options":[`+
				`{"id":"OPT_BACKLOG","name":"Backlog"},{"id":"OPT_ICEBOX","name":"Icebox"},{"id":"OPT_RANK_3","name":"3"}]}}}}}`)
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

	srv := httptest.NewServer(mux)
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
// per-layer units cannot cover (#618): the REAL workmgmt.ApplyGrooming core
// drives the REAL *Provider over a REAL *githubclient.Client against an
// httptest forge, and the assertions read the HTTP requests the fixture server
// ACTUALLY received.
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

	hygiene := plan.HygieneDefect{
		ItemRef: groomingItemRef(2237), Defect: "missing_label_namespace",
		Detail: "no area label", SuggestedFix: "area:backend",
	}
	hygiene.ID = plan.GroomingEntryID(plan.GroomingClassHygiene, hygiene.Defect, hygiene.ItemRef)

	ordering := plan.OrderingEntry{ItemRef: groomingItemRef(2238), Rank: 3, Score: 0.9}
	ordering.ID = plan.GroomingEntryID(plan.GroomingClassOrdering, "", ordering.ItemRef)

	dup := plan.DuplicateCandidate{
		Pair:  []plan.ItemRef{groomingItemRef(2239), groomingItemRef(2240)},
		Basis: "same proposal", Confidence: "high",
	}
	dup.ID = plan.GroomingEntryID(plan.GroomingClassDuplicate, "", dup.Pair[0], dup.Pair[1])

	decomp := plan.DecompositionSuggestion{ItemRef: groomingItemRef(2241), Rationale: "too big"}
	decomp.ID = plan.GroomingEntryID(plan.GroomingClassDecomposition, "", decomp.ItemRef)

	report := &plan.GroomingReport{
		Ordering: []plan.OrderingEntry{ordering}, Duplicates: []plan.DuplicateCandidate{dup},
		HygieneDefects: []plan.HygieneDefect{hygiene}, DecompositionSuggestions: []plan.DecompositionSuggestion{decomp},
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
			{EntryID: hygiene.ID, Verdict: workmgmt.GroomingApproved},
			{EntryID: ordering.ID, Verdict: workmgmt.GroomingApproved},
			{EntryID: dup.ID, Verdict: workmgmt.GroomingRejected, CloseTarget: dup.Pair[0].ID},
			{EntryID: decomp.ID, Verdict: workmgmt.GroomingApproved},
		},
		Modes: map[string]workmgmt.GroomingMode{
			"hygiene":  workmgmt.GroomingModeAuto,
			"ordering": workmgmt.GroomingModeGated,
			"dedup":    workmgmt.GroomingModeAuto,
			// The I1 row: report mode on the class carrying the destructive
			// icebox kind, whose entry ALSO holds an explicit gate approval.
			"scoping": workmgmt.GroomingModeReport,
		},
		GateApproved: map[string]bool{decomp.ID: true},
		States:       groomingStates,
		IceboxColumn: "Icebox",
	})
	if err != nil {
		t.Fatalf("ApplyGrooming: %v", err)
	}

	// Exactly ONE PATCH reached the forge: the label union. No close.
	if len(fx.patches) != 1 {
		t.Fatalf("PATCH calls = %v, want exactly the label update", fx.patches)
	}
	if !strings.Contains(fx.patches[0], "/repos/kuhlman-labs/fishhawk/issues/2237") {
		t.Errorf("PATCH path = %q, want the hygiene item", fx.patches[0])
	}
	if !strings.Contains(fx.patches[0], `"labels":["type:feature","area:backend"]`) {
		t.Errorf("PATCH body = %q, want the label UNION", fx.patches[0])
	}
	for _, p := range fx.patches {
		if strings.Contains(p, `"state":"closed"`) {
			t.Errorf("a close reached the forge for a REJECTED duplicate: %q", p)
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
		case decomp.ID:
			sawReportMode = true
			if rec.SkipReason != workmgmt.GroomingSkipReportMode {
				t.Errorf("decomposition skip reason = %q, want %q (report mode beats the gate approval)",
					rec.SkipReason, workmgmt.GroomingSkipReportMode)
			}
		case dup.ID:
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
