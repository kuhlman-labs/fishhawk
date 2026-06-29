package campaign_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/campaign"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// TestAssemble_MultiWaveDAG drives a small DAG (41,42 in wave 0; 43 depends on
// 41; 44 depends on 42 and 43) end-to-end from a synthesized
// workmgmt.EpicChildrenResult through Assemble — the integration test across
// the workmgmt→campaign seam — asserting the computed wave order and per-item
// depends_on refs.
func TestAssemble_MultiWaveDAG(t *testing.T) {
	res := &workmgmt.EpicChildrenResult{
		Children: []workmgmt.EpicChild{
			{Number: 41}, {Number: 42}, {Number: 43}, {Number: 44},
		},
		// 43->41, 44->42, 44->43 (From depends on To).
		Edges: []workmgmt.DependsEdge{
			{From: 43, To: 41}, {From: 44, To: 42}, {From: 44, To: 43},
		},
	}

	a, err := campaign.Assemble("issue:40", res)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if a.EpicRef != "issue:40" {
		t.Errorf("epic ref = %q, want issue:40", a.EpicRef)
	}

	wantWaves := [][]string{
		{"issue:41", "issue:42"},
		{"issue:43"},
		{"issue:44"},
	}
	if !reflect.DeepEqual(a.Waves, wantWaves) {
		t.Errorf("waves = %v, want %v", a.Waves, wantWaves)
	}

	wantItems := []campaign.AssembledItem{
		{IssueRef: "issue:41", DependsOn: nil, Wave: 0},
		{IssueRef: "issue:42", DependsOn: nil, Wave: 0},
		{IssueRef: "issue:43", DependsOn: []string{"issue:41"}, Wave: 1},
		{IssueRef: "issue:44", DependsOn: []string{"issue:42", "issue:43"}, Wave: 2},
	}
	if !reflect.DeepEqual(a.Items, wantItems) {
		t.Errorf("items = %+v, want %+v", a.Items, wantItems)
	}
}

// TestAssemble_CycleRejected asserts a cyclic depends_on graph fails closed
// with ErrCycle.
func TestAssemble_CycleRejected(t *testing.T) {
	res := &workmgmt.EpicChildrenResult{
		Children: []workmgmt.EpicChild{{Number: 41}, {Number: 42}},
		// 41->42 and 42->41: a 2-cycle.
		Edges: []workmgmt.DependsEdge{{From: 41, To: 42}, {From: 42, To: 41}},
	}
	_, err := campaign.Assemble("issue:40", res)
	if !errors.Is(err, campaign.ErrCycle) {
		t.Fatalf("Assemble(cycle) err = %v, want ErrCycle", err)
	}
}

// TestAssemble_DanglingDependencyRejected asserts that a mis-targeted edge
// surfaced by the provider as a DroppedEdge fails assembly closed with
// ErrDanglingDependency — the body-authoritative "a missing dependency fails
// assembly closed" choice.
func TestAssemble_DanglingDependencyRejected(t *testing.T) {
	res := &workmgmt.EpicChildrenResult{
		Children:     []workmgmt.EpicChild{{Number: 41}},
		DroppedEdges: []workmgmt.DependsEdge{{From: 41, To: 999}},
	}
	_, err := campaign.Assemble("issue:40", res)
	if !errors.Is(err, campaign.ErrDanglingDependency) {
		t.Fatalf("Assemble(dangling) err = %v, want ErrDanglingDependency", err)
	}
}

// TestAssemble_NilResult covers the defensive nil-result guard.
func TestAssemble_NilResult(t *testing.T) {
	if _, err := campaign.Assemble("issue:40", nil); err == nil {
		t.Fatal("Assemble(nil) err = nil, want error")
	}
}

// TestPersist_AssembleThenReadBack is the pgtest-backed happy-path for the
// persistence helper (opus LOW(1) binding condition): assemble an epic, persist
// the campaign + items via the Repository, and read them back to confirm the
// helper writes the campaign and one item per assembled item with the right
// epic/issue refs and depends_on edges.
func TestPersist_AssembleThenReadBack(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := campaign.NewPostgresRepository(pool)
	ctx := context.Background()

	res := &workmgmt.EpicChildrenResult{
		Children: []workmgmt.EpicChild{{Number: 41}, {Number: 42}, {Number: 43}},
		Edges:    []workmgmt.DependsEdge{{From: 43, To: 41}, {From: 43, To: 42}},
	}
	a, err := campaign.Assemble("issue:40", res)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}

	c, err := campaign.Persist(ctx, repo, "kuhlman-labs/fishhawk", a)
	if err != nil {
		t.Fatalf("Persist: %v", err)
	}
	if c.EpicRef != "issue:40" || c.Repo != "kuhlman-labs/fishhawk" {
		t.Errorf("persisted campaign = %+v, want epic issue:40 repo kuhlman-labs/fishhawk", c)
	}
	if c.State != campaign.StatePending {
		t.Errorf("persisted campaign state = %q, want pending", c.State)
	}

	// Read the campaign back independently.
	got, err := repo.GetCampaign(ctx, c.ID)
	if err != nil {
		t.Fatalf("GetCampaign: %v", err)
	}
	if got.EpicRef != "issue:40" {
		t.Errorf("read-back epic ref = %q, want issue:40", got.EpicRef)
	}

	// Read the items back: insertion order is ascending issue number.
	items, err := repo.ListCampaignItemsForCampaign(ctx, c.ID)
	if err != nil {
		t.Fatalf("ListCampaignItemsForCampaign: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("persisted %d items, want 3", len(items))
	}
	wantRefs := []string{"issue:41", "issue:42", "issue:43"}
	for i, it := range items {
		if it.IssueRef != wantRefs[i] {
			t.Errorf("item[%d] ref = %q, want %q", i, it.IssueRef, wantRefs[i])
		}
		if it.CampaignID != c.ID {
			t.Errorf("item[%d] campaign id = %v, want %v", i, it.CampaignID, c.ID)
		}
	}
	// The third item carries both depends_on edges.
	if !reflect.DeepEqual(items[2].DependsOn, []string{"issue:41", "issue:42"}) {
		t.Errorf("item issue:43 depends_on = %v, want [issue:41 issue:42]", items[2].DependsOn)
	}
}

// TestPersist_NilAssembly covers the defensive nil-assembly guard.
func TestPersist_NilAssembly(t *testing.T) {
	if _, err := campaign.Persist(context.Background(), campaign.BaseFake{}, "repo", nil); err == nil {
		t.Fatal("Persist(nil) err = nil, want error")
	}
}

// TestPersist_CreateCampaignError covers the create-campaign error branch:
// BaseFake.CreateCampaign returns ErrNotFound, which Persist must wrap and
// return rather than proceeding to create items.
func TestPersist_CreateCampaignError(t *testing.T) {
	a := &campaign.Assembly{EpicRef: "issue:40", Items: []campaign.AssembledItem{{IssueRef: "issue:41"}}}
	_, err := campaign.Persist(context.Background(), campaign.BaseFake{}, "repo", a)
	if !errors.Is(err, campaign.ErrNotFound) {
		t.Fatalf("Persist err = %v, want wrapped ErrNotFound", err)
	}
}

// capturingFake records the CreateCampaignParams Persist builds so a test can
// assert the PausePolicy normalization without a database. It creates the
// campaign successfully; tests pass an Assembly with no items so the item loop
// is a no-op (BaseFake.CreateCampaignItem would otherwise return ErrNotFound).
type capturingFake struct {
	campaign.BaseFake
	got campaign.CreateCampaignParams
}

func (f *capturingFake) CreateCampaign(_ context.Context, p campaign.CreateCampaignParams) (*campaign.Campaign, error) {
	f.got = p
	return &campaign.Campaign{EpicRef: p.EpicRef, PausePolicy: p.PausePolicy}, nil
}

// TestPersist_NormalizesZeroPausePolicy is the backward-compat done-means: an
// Assembly with a ZERO PausePolicy (what the existing, unchanged server call
// site produces under slice 1) must persist as the block-the-campaign default
// pause_campaign — never an empty string. Tested via the captured params so
// the normalization is asserted in Persist itself.
func TestPersist_NormalizesZeroPausePolicy(t *testing.T) {
	f := &capturingFake{}
	a := &campaign.Assembly{EpicRef: "issue:40"} // zero PausePolicy, no items
	if _, err := campaign.Persist(context.Background(), f, "kuhlman-labs/fishhawk", a); err != nil {
		t.Fatalf("Persist: %v", err)
	}
	if f.got.PausePolicy != campaign.PausePolicyPauseCampaign {
		t.Errorf("zero PausePolicy persisted as %q, want %q", f.got.PausePolicy, campaign.PausePolicyPauseCampaign)
	}
}

// TestPersist_PreservesExplicitPausePolicy asserts the other half: an explicit
// pause_item policy survives Persist unchanged (slice 3 sets it from the create
// request), so normalization defaults only the zero value.
func TestPersist_PreservesExplicitPausePolicy(t *testing.T) {
	f := &capturingFake{}
	a := &campaign.Assembly{EpicRef: "issue:40", PausePolicy: campaign.PausePolicyPauseItem}
	if _, err := campaign.Persist(context.Background(), f, "kuhlman-labs/fishhawk", a); err != nil {
		t.Fatalf("Persist: %v", err)
	}
	if f.got.PausePolicy != campaign.PausePolicyPauseItem {
		t.Errorf("explicit PausePolicy persisted as %q, want %q", f.got.PausePolicy, campaign.PausePolicyPauseItem)
	}
}

// TestPersist_ThreadsOperatorAgent asserts the campaign-level operator_agent
// override (E25.12) is threaded straight through Persist onto the
// CreateCampaignParams, byte-for-byte. A nil override stays nil (the
// unchanged-behavior path); a non-nil block passes through unchanged.
func TestPersist_ThreadsOperatorAgent(t *testing.T) {
	// Nil override → nil params.
	fNil := &capturingFake{}
	if _, err := campaign.Persist(context.Background(), fNil, "kuhlman-labs/fishhawk",
		&campaign.Assembly{EpicRef: "issue:1451"}); err != nil {
		t.Fatalf("Persist (nil override): %v", err)
	}
	if fNil.got.OperatorAgent != nil {
		t.Errorf("nil override persisted as %q, want nil", fNil.got.OperatorAgent)
	}

	// Non-nil override passes through byte-for-byte.
	override := []byte(`{"may_approve":"solo_low"}`)
	f := &capturingFake{}
	if _, err := campaign.Persist(context.Background(), f, "kuhlman-labs/fishhawk",
		&campaign.Assembly{EpicRef: "issue:1451", OperatorAgent: override}); err != nil {
		t.Fatalf("Persist (override): %v", err)
	}
	if string(f.got.OperatorAgent) != string(override) {
		t.Errorf("override persisted as %q, want %q", f.got.OperatorAgent, override)
	}
}

// TestPersist_ThreadsIdempotencyKey asserts the optional create idempotency key
// (E25.13 / #1455) is threaded straight through Persist onto the
// CreateCampaignParams. A nil key stays nil (the unchanged-behavior path); a
// non-nil key passes through by value.
func TestPersist_ThreadsIdempotencyKey(t *testing.T) {
	// Nil key → nil params.
	fNil := &capturingFake{}
	if _, err := campaign.Persist(context.Background(), fNil, "kuhlman-labs/fishhawk",
		&campaign.Assembly{EpicRef: "issue:1455"}); err != nil {
		t.Fatalf("Persist (nil key): %v", err)
	}
	if fNil.got.IdempotencyKey != nil {
		t.Errorf("nil key persisted as %v, want nil", fNil.got.IdempotencyKey)
	}

	// Non-nil key passes through by value.
	key := "campaign-key-1"
	f := &capturingFake{}
	if _, err := campaign.Persist(context.Background(), f, "kuhlman-labs/fishhawk",
		&campaign.Assembly{EpicRef: "issue:1455", IdempotencyKey: &key}); err != nil {
		t.Fatalf("Persist (key): %v", err)
	}
	if f.got.IdempotencyKey == nil || *f.got.IdempotencyKey != key {
		t.Errorf("key persisted as %v, want %q", f.got.IdempotencyKey, key)
	}
}

// persistItemErrFake creates a campaign successfully but fails every item
// insert, so Persist reaches the create-item error branch.
type persistItemErrFake struct{ campaign.BaseFake }

func (persistItemErrFake) CreateCampaign(_ context.Context, _ campaign.CreateCampaignParams) (*campaign.Campaign, error) {
	return &campaign.Campaign{}, nil
}

// TestPersist_CreateItemError covers the create-item error branch.
func TestPersist_CreateItemError(t *testing.T) {
	a := &campaign.Assembly{EpicRef: "issue:40", Items: []campaign.AssembledItem{{IssueRef: "issue:41"}}}
	_, err := campaign.Persist(context.Background(), persistItemErrFake{}, "repo", a)
	if !errors.Is(err, campaign.ErrNotFound) {
		t.Fatalf("Persist err = %v, want wrapped ErrNotFound from item insert", err)
	}
}
