package campaign

import (
	"errors"
	"reflect"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

func TestValidDependencyRef(t *testing.T) {
	cases := map[string]bool{
		"issue:1":                  true,
		"issue:4358":               true,
		"issue:0":                  false,
		"issue:-3":                 false,
		"issue:05":                 false,
		"#5":                       false,
		"5":                        false,
		"issue:#5":                 false,
		" issue:5":                 false,
		"issue:5 ":                 false,
		"unparsable:abc:\"x\"":     false,
		"unparsable:deadbeef":      false,
		"":                         false,
		"issue:":                   false,
		"issue:issue:5":            false,
		"issue:99999999999999999x": false,
	}
	for ref, want := range cases {
		if got := ValidDependencyRef(ref); got != want {
			t.Errorf("ValidDependencyRef(%q) = %v, want %v", ref, got, want)
		}
	}
}

func TestMergeDependsOn_NeverDropsPersisted(t *testing.T) {
	got := MergeDependsOn([]string{"issue:1", "issue:2"}, []string{"issue:3", "issue:1", "issue:3"})
	want := []string{"issue:1", "issue:2", "issue:3"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("merge = %v, want %v", got, want)
	}
	// A narrower addition retains every persisted edge (monotone).
	if got := MergeDependsOn([]string{"issue:1", "issue:2"}, []string{"issue:2"}); !reflect.DeepEqual(got, []string{"issue:1", "issue:2"}) {
		t.Fatalf("narrower add = %v, want [issue:1 issue:2]", got)
	}
	if got := MergeDependsOn([]string{"issue:1"}, nil); !reflect.DeepEqual(got, []string{"issue:1"}) {
		t.Fatalf("empty add = %v, want [issue:1]", got)
	}
}

func TestCheckDuplicates(t *testing.T) {
	existing := []*Item{{IssueRef: "issue:10"}}
	if err := CheckDuplicates(existing, []string{"issue:20", "issue:21"}); err != nil {
		t.Fatalf("no dup: %v", err)
	}
	err := CheckDuplicates(existing, []string{"issue:10", "issue:20", "issue:20"})
	var dup *DuplicateItemError
	if !errors.As(err, &dup) || !errors.Is(err, ErrDuplicateItem) {
		t.Fatalf("err = %v, want *DuplicateItemError", err)
	}
	if !reflect.DeepEqual(dup.Refs, []string{"issue:10", "issue:20"}) {
		t.Errorf("dup refs = %v, want [issue:10 issue:20]", dup.Refs)
	}
	if dup.Error() == "" {
		t.Error("empty error text")
	}
}

func TestValidateAppendClosure(t *testing.T) {
	t.Run("new-new cycle refused", func(t *testing.T) {
		err := ValidateAppendClosure(nil, []CreateCampaignItemParams{
			{IssueRef: "issue:2", DependsOn: []string{"issue:3"}},
			{IssueRef: "issue:3", DependsOn: []string{"issue:2"}},
		}, nil)
		var ce *CycleError
		if !errors.As(err, &ce) || !errors.Is(err, ErrCycle) {
			t.Fatalf("err = %v, want *CycleError", err)
		}
		if !reflect.DeepEqual(ce.Cycle, []string{"issue:2", "issue:3", "issue:2"}) {
			t.Errorf("cycle = %v", ce.Cycle)
		}
		if ce.Error() == "" {
			t.Error("empty error text")
		}
	})
	t.Run("new-existing cycle refused (seeded by construction)", func(t *testing.T) {
		// A's persisted depends_on already names B, which is not yet a member.
		existing := []*Item{{ID: uuid.New(), IssueRef: "issue:1", DependsOn: []string{"issue:2"}}}
		err := ValidateAppendClosure(existing, []CreateCampaignItemParams{
			{IssueRef: "issue:2", DependsOn: []string{"issue:1"}},
		}, nil)
		if !errors.Is(err, ErrCycle) {
			t.Fatalf("err = %v, want ErrCycle", err)
		}
	})
	t.Run("cycle closed by an addition through a new node refused", func(t *testing.T) {
		a := &Item{ID: uuid.New(), IssueRef: "issue:1"}
		err := ValidateAppendClosure([]*Item{a}, []CreateCampaignItemParams{
			{IssueRef: "issue:2", DependsOn: []string{"issue:1"}},
		}, map[uuid.UUID][]string{a.ID: {"issue:2"}})
		if !errors.Is(err, ErrCycle) {
			t.Fatalf("err = %v, want ErrCycle", err)
		}
	})
	t.Run("pre-existing member-only cycle tolerated", func(t *testing.T) {
		existing := []*Item{
			{ID: uuid.New(), IssueRef: "issue:1", DependsOn: []string{"issue:2"}},
			{ID: uuid.New(), IssueRef: "issue:2", DependsOn: []string{"issue:1"}},
		}
		if err := ValidateAppendClosure(existing, []CreateCampaignItemParams{
			{IssueRef: "issue:3", DependsOn: []string{"issue:1"}},
		}, nil); err != nil {
			t.Fatalf("unrelated append over a held member cycle: %v", err)
		}
	})
	t.Run("edges to non-members ignored", func(t *testing.T) {
		if err := ValidateAppendClosure(nil, []CreateCampaignItemParams{
			{IssueRef: "issue:3", DependsOn: []string{"issue:999"}},
		}, nil); err != nil {
			t.Fatalf("external edge treated as cycle: %v", err)
		}
	})
	t.Run("diamond is not a cycle", func(t *testing.T) {
		if err := ValidateAppendClosure([]*Item{{ID: uuid.New(), IssueRef: "issue:1"}}, []CreateCampaignItemParams{
			{IssueRef: "issue:2", DependsOn: []string{"issue:1"}},
			{IssueRef: "issue:3", DependsOn: []string{"issue:1"}},
			{IssueRef: "issue:4", DependsOn: []string{"issue:2", "issue:3"}},
		}, nil); err != nil {
			t.Fatalf("diamond: %v", err)
		}
	})
}

func TestCheckExternal_NewNodesOnly(t *testing.T) {
	existing := []*Item{{IssueRef: "issue:10", DependsOn: []string{"issue:999"}}}
	newItems := []CreateCampaignItemParams{
		{IssueRef: "issue:20", DependsOn: []string{"issue:10"}},
		{IssueRef: "issue:21", DependsOn: []string{"issue:20", "issue:777"}},
	}
	err := CheckExternal(newItems, MemberRefs(existing, newItems))
	var ee *ExternalDependencyError
	if !errors.As(err, &ee) || !errors.Is(err, ErrExternalDependency) {
		t.Fatalf("err = %v, want *ExternalDependencyError", err)
	}
	// Only the NEW node's external edge; existing issue:10 -> issue:999 is not flagged.
	if !reflect.DeepEqual(ee.Edges, []ExternalEdge{{From: "issue:21", To: "issue:777"}}) {
		t.Errorf("edges = %v", ee.Edges)
	}
	if ee.Error() == "" {
		t.Error("empty error text")
	}
	if err := CheckExternal(newItems[:1], MemberRefs(existing, newItems[:1])); err != nil {
		t.Errorf("member-only edges: %v", err)
	}
}

func TestValidateAppend_GuardOrder(t *testing.T) {
	a := &Item{ID: uuid.New(), IssueRef: "issue:1"}
	existing := []*Item{a}
	cases := []struct {
		name string
		p    AppendCampaignItemsParams
		want error
	}{
		{"malformed new ref", AppendCampaignItemsParams{NewItems: []CreateCampaignItemParams{{IssueRef: "#2"}}}, ErrInvalidDependencyRef},
		{"malformed dep", AppendCampaignItemsParams{NewItems: []CreateCampaignItemParams{{IssueRef: "issue:2", DependsOn: []string{"unparsable:x"}}}}, ErrInvalidDependencyRef},
		{"malformed addition", AppendCampaignItemsParams{AddDependencies: map[uuid.UUID][]string{a.ID: {"issue:0"}}}, ErrInvalidDependencyRef},
		{"duplicate", AppendCampaignItemsParams{NewItems: []CreateCampaignItemParams{{IssueRef: "issue:1"}}}, ErrDuplicateItem},
		{"addition to non-member", AppendCampaignItemsParams{AddDependencies: map[uuid.UUID][]string{uuid.New(): {"issue:1"}}}, ErrCampaignItemNotFound},
		{"external", AppendCampaignItemsParams{NewItems: []CreateCampaignItemParams{{IssueRef: "issue:2", DependsOn: []string{"issue:9"}}}}, ErrExternalDependency},
		{"cycle", AppendCampaignItemsParams{
			NewItems:        []CreateCampaignItemParams{{IssueRef: "issue:2", DependsOn: []string{"issue:1"}}},
			AddDependencies: map[uuid.UUID][]string{a.ID: {"issue:2"}},
		}, ErrCycle},
	}
	for _, tc := range cases {
		if err := ValidateAppend(existing, tc.p); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
	}
	ok := AppendCampaignItemsParams{
		NewItems:      []CreateCampaignItemParams{{IssueRef: "issue:2", DependsOn: []string{"issue:9"}}},
		AllowExternal: true,
	}
	if err := ValidateAppend(existing, ok); err != nil {
		t.Errorf("allow external: %v", err)
	}
}

func TestPlanAppend_Classification(t *testing.T) {
	existing := []*Item{{IssueRef: "issue:10"}}
	res := &workmgmt.EpicChildrenResult{
		Children: []workmgmt.EpicChild{{Number: 20, Autonomy: "low"}, {Number: 21, Autonomy: "bogus"}},
		Edges:    []workmgmt.DependsEdge{{From: 21, To: 20}},
		DroppedEdges: []workmgmt.DependsEdge{
			{From: 20, To: 10, Reason: workmgmt.DropNotChild},  // existing member → depends_on
			{From: 21, To: 999, Reason: workmgmt.DropNotChild}, // non-member → external
			{From: 21, To: 999, Reason: workmgmt.DropNotChild}, // duplicate edge deduped
		},
		SatisfiedEdges: []workmgmt.SatisfiedEdge{
			{From: 20, To: 500, State: "closed", StateReason: "completed"}, // non-member → elided
			{From: 21, To: 10, State: "closed", StateReason: "completed"},  // member → depends_on
		},
	}
	plan, err := PlanAppend(existing, []string{"issue:20", "issue:21"}, res)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	want := []CreateCampaignItemParams{
		{IssueRef: "issue:20", DependsOn: []string{"issue:10"}, Autonomy: "low"},
		{IssueRef: "issue:21", DependsOn: []string{"issue:20", "issue:999", "issue:10"}, Autonomy: ""},
	}
	if !reflect.DeepEqual(plan.Items, want) {
		t.Errorf("items = %+v\nwant %+v", plan.Items, want)
	}
	if !reflect.DeepEqual(plan.External, map[string][]string{"issue:21": {"issue:999"}}) {
		t.Errorf("external = %v", plan.External)
	}
	if len(plan.Satisfied) != 1 || plan.Satisfied[0].To != 500 {
		t.Errorf("satisfied = %+v, want only the non-member 500", plan.Satisfied)
	}
}

func TestPlanAppend_DuplicateAndNil(t *testing.T) {
	if _, err := PlanAppend([]*Item{{IssueRef: "issue:10"}}, []string{"issue:10"}, &workmgmt.EpicChildrenResult{}); !errors.Is(err, ErrDuplicateItem) {
		t.Errorf("dup err = %v", err)
	}
	if _, err := PlanAppend(nil, []string{"issue:1"}, nil); err == nil {
		t.Error("nil result accepted")
	}
}

func TestPlanAppend_UnparsableTargetIsRefusedDownstream(t *testing.T) {
	res := &workmgmt.EpicChildrenResult{
		Children:     []workmgmt.EpicChild{{Number: 20}},
		DroppedEdges: []workmgmt.DependsEdge{{From: 20, ToRef: "foo", ToRefDigest: "abcd"}},
	}
	plan, err := PlanAppend(nil, []string{"issue:20"}, res)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	err = ValidateAppend(nil, AppendCampaignItemsParams{NewItems: plan.Items, AllowExternal: true})
	if !errors.Is(err, ErrInvalidDependencyRef) {
		t.Fatalf("unparsable target err = %v, want ErrInvalidDependencyRef", err)
	}
}

// TestValidateAppend_RunsValidateExpedite pins that the shared append guard
// (the one the Postgres adapter and every fake call under the lock) applies
// the expedite rule, so no repository path admits an invalid expedite.
func TestValidateAppend_RunsValidateExpedite(t *testing.T) {
	p := AppendCampaignItemsParams{
		NewItems: []CreateCampaignItemParams{{IssueRef: "issue:1"}, {IssueRef: "issue:2"}},
		Expedite: &ExpediteParams{Kind: ExpediteKindHotfix, Workflow: "w"},
	}
	if err := ValidateAppend(nil, p); !errors.Is(err, ErrExpediteInvalid) {
		t.Fatalf("ValidateAppend(two expedited items) = %v, want ErrExpediteInvalid", err)
	}
	p.NewItems = p.NewItems[:1]
	if err := ValidateAppend(nil, p); err != nil {
		t.Fatalf("ValidateAppend(one expedited item) = %v, want nil", err)
	}
}
