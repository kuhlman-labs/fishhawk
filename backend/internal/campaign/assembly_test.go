package campaign_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
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

	// Position is the DENSE 0-based queue position (migration 0074), stamped from
	// each item's index in res.Children — distinct from Wave, which is the
	// topological depth. Two items can share a wave and never a position.
	wantItems := []campaign.AssembledItem{
		{IssueRef: "issue:41", DependsOn: nil, Wave: 0, Position: 0},
		{IssueRef: "issue:42", DependsOn: nil, Wave: 0, Position: 1},
		{IssueRef: "issue:43", DependsOn: []string{"issue:41"}, Wave: 1, Position: 2},
		{IssueRef: "issue:44", DependsOn: []string{"issue:42", "issue:43"}, Wave: 2, Position: 3},
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

// TestAssemble_DanglingDependency_Categorized asserts the typed
// DanglingDependencyError groups dropped edges by cause and renders one clause
// per non-empty category (#2120): a DropNotChild (or unclassified/zero-reason)
// edge keeps the "not a fellow child of the epic" wording, a
// DropExcludedIncomplete edge names the include/omit remedy, and errors.As
// recovers the categorized edge lists. errors.Is(ErrDanglingDependency) still
// holds through the wrap.
func TestAssemble_DanglingDependency_Categorized(t *testing.T) {
	res := &workmgmt.EpicChildrenResult{
		Children: []workmgmt.EpicChild{{Number: 41}, {Number: 42}},
		DroppedEdges: []workmgmt.DependsEdge{
			{From: 41, To: 999, Reason: workmgmt.DropNotChild},
			{From: 42, To: 43, Reason: workmgmt.DropExcludedIncomplete},
		},
	}
	_, err := campaign.Assemble("issue:40", res)
	if !errors.Is(err, campaign.ErrDanglingDependency) {
		t.Fatalf("err = %v, want wrapped ErrDanglingDependency", err)
	}
	var de *campaign.DanglingDependencyError
	if !errors.As(err, &de) {
		t.Fatalf("err = %v, want *DanglingDependencyError via errors.As", err)
	}
	if len(de.NotChild) != 1 || de.NotChild[0] != (workmgmt.DependsEdge{From: 41, To: 999, Reason: workmgmt.DropNotChild}) {
		t.Errorf("NotChild = %+v, want [{41 999 not_child}]", de.NotChild)
	}
	if len(de.ExcludedIncomplete) != 1 || de.ExcludedIncomplete[0] != (workmgmt.DependsEdge{From: 42, To: 43, Reason: workmgmt.DropExcludedIncomplete}) {
		t.Errorf("ExcludedIncomplete = %+v, want [{42 43 excluded_incomplete}]", de.ExcludedIncomplete)
	}
	msg := err.Error()
	// not_child clause keeps the pre-#2120 wording and names its edge.
	if !strings.Contains(msg, "not a fellow child of the epic") || !strings.Contains(msg, "issue:41->issue:999") {
		t.Errorf("message %q missing not_child clause naming issue:41->issue:999", msg)
	}
	// excluded_incomplete clause names cause + include/omit remedy and its edge.
	if !strings.Contains(msg, "include it in items") || !strings.Contains(msg, "omit items") || !strings.Contains(msg, "issue:42->issue:43") {
		t.Errorf("message %q missing excluded_incomplete remedy clause naming issue:42->issue:43", msg)
	}
}

// TestAssemble_DanglingDependency_ZeroReasonIsNotChild asserts a dropped edge
// with an empty (zero) Reason — a pre-#2120 provider edge or a hand-built
// fixture — defaults to the not_child category, preserving the current wording
// (#2120 zero-value compatibility).
func TestAssemble_DanglingDependency_ZeroReasonIsNotChild(t *testing.T) {
	res := &workmgmt.EpicChildrenResult{
		Children:     []workmgmt.EpicChild{{Number: 41}},
		DroppedEdges: []workmgmt.DependsEdge{{From: 41, To: 999}}, // no Reason
	}
	_, err := campaign.Assemble("issue:40", res)
	var de *campaign.DanglingDependencyError
	if !errors.As(err, &de) {
		t.Fatalf("err = %v, want *DanglingDependencyError", err)
	}
	if len(de.NotChild) != 1 || len(de.ExcludedIncomplete) != 0 {
		t.Errorf("categories = NotChild %+v / ExcludedIncomplete %+v, want the zero-reason edge in NotChild only", de.NotChild, de.ExcludedIncomplete)
	}
	if !strings.Contains(err.Error(), "not a fellow child of the epic") {
		t.Errorf("message %q missing not_child wording", err.Error())
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

// TestPersist_ThreadsAutonomy_SeamReadBack is the BINDING autonomy seam test
// (#1551 / E32.4, operator binding condition 1): it proves the FULL
// label→assemble→persist→read path, not merely a CreateCampaignItem round-trip.
// Starting from an EpicChild carrying Autonomy=="low" (the tier a sibling slice
// parses off the child's `autonomy:low` label), it runs Assemble → Persist
// against a real pgtest pool, then reads the campaign_items row back — both
// through the Repository (asserting Item.Autonomy) AND via a raw column SELECT
// (asserting the campaign_items.autonomy column itself holds "low"). A sibling
// child carrying no autonomy label reads back the empty (unknown/default) tier.
func TestPersist_ThreadsAutonomy_SeamReadBack(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := campaign.NewPostgresRepository(pool)
	ctx := context.Background()

	res := &workmgmt.EpicChildrenResult{
		Children: []workmgmt.EpicChild{
			{Number: 41, Autonomy: "low"}, // human-led
			{Number: 42},                  // no autonomy label → "" default
		},
	}
	a, err := campaign.Assemble("issue:40", res)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	// Assemble must have copied the source tier onto the assembled item before
	// it ever reached the database (the label→domain hop of the seam).
	if a.Items[0].Autonomy != "low" {
		t.Errorf("assembled item[0] autonomy = %q, want low", a.Items[0].Autonomy)
	}
	if a.Items[1].Autonomy != "" {
		t.Errorf("assembled item[1] autonomy = %q, want empty", a.Items[1].Autonomy)
	}

	c, err := campaign.Persist(ctx, repo, "kuhlman-labs/fishhawk", a)
	if err != nil {
		t.Fatalf("Persist: %v", err)
	}

	// Read back through the Repository: the persisted item carries the tier.
	items, err := repo.ListCampaignItemsForCampaign(ctx, c.ID)
	if err != nil {
		t.Fatalf("ListCampaignItemsForCampaign: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("persisted %d items, want 2", len(items))
	}
	if items[0].IssueRef != "issue:41" || items[0].Autonomy != "low" {
		t.Errorf("item issue:41 autonomy = %q, want low", items[0].Autonomy)
	}
	if items[1].IssueRef != "issue:42" || items[1].Autonomy != "" {
		t.Errorf("item issue:42 autonomy = %q, want empty", items[1].Autonomy)
	}

	// Read the raw column too: prove campaign_items.autonomy itself holds the
	// tier, not merely that the row mapper reconstructs it.
	var col string
	if err := pool.QueryRow(ctx,
		`SELECT autonomy FROM campaign_items WHERE id = $1`, items[0].ID,
	).Scan(&col); err != nil {
		t.Fatalf("read autonomy column: %v", err)
	}
	if col != "low" {
		t.Errorf("campaign_items.autonomy column = %q, want low", col)
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

// TestPersist_ThreadsWorkingDir pins the campaign-level checkout binding
// (E48.87 / #2527) crossing Assembly → CreateCampaignParams: an empty binding
// stays empty (the unchanged default, so every existing call site that never
// sets it persists an empty string), and a bound value passes through verbatim — Persist
// neither normalizes nor validates it (absolute-path validation is the REST
// handler's).
func TestPersist_ThreadsWorkingDir(t *testing.T) {
	// Empty binding → empty params (the unchanged-behavior default).
	fEmpty := &capturingFake{}
	if _, err := campaign.Persist(context.Background(), fEmpty, "kuhlman-labs/fishhawk",
		&campaign.Assembly{EpicRef: "issue:2527"}); err != nil {
		t.Fatalf("Persist (no binding): %v", err)
	}
	if fEmpty.got.WorkingDir != "" {
		t.Errorf("unbound campaign persisted WorkingDir %q, want \"\"", fEmpty.got.WorkingDir)
	}

	// A bound value passes through verbatim.
	const wd = "/Users/op/checkouts/fishhawk"
	f := &capturingFake{}
	if _, err := campaign.Persist(context.Background(), f, "kuhlman-labs/fishhawk",
		&campaign.Assembly{EpicRef: "issue:2527", WorkingDir: wd}); err != nil {
		t.Fatalf("Persist (binding): %v", err)
	}
	if f.got.WorkingDir != wd {
		t.Errorf("WorkingDir persisted as %q, want %q", f.got.WorkingDir, wd)
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

// itemCapturingFake records the item params Persist writes, in order, so the
// queue-position threading is asserted at the seam rather than inferred.
type itemCapturingFake struct {
	campaign.BaseFake
	campaignGot campaign.CreateCampaignParams
	itemsGot    []campaign.CreateCampaignItemParams
}

func (f *itemCapturingFake) CreateCampaign(_ context.Context, p campaign.CreateCampaignParams) (*campaign.Campaign, error) {
	f.campaignGot = p
	return &campaign.Campaign{EpicRef: p.EpicRef}, nil
}

func (f *itemCapturingFake) CreateCampaignItem(_ context.Context, p campaign.CreateCampaignItemParams) (*campaign.Item, error) {
	f.itemsGot = append(f.itemsGot, p)
	return &campaign.Item{IssueRef: p.IssueRef, Position: p.Position}, nil
}

// TestPersist_ThreadsQueuePosition asserts the K1 seam: Assemble stamps a dense
// 0-based position from the order of res.Children, and Persist carries it onto
// the item params. The children are handed in a DELIBERATELY non-ascending
// order (what ReorderByPriority produces for a ratified rank order), so a
// Persist that dropped Position would show up as all-zero positions rather than
// as a silently-still-correct sequence.
func TestPersist_ThreadsQueuePosition(t *testing.T) {
	res := &workmgmt.EpicChildrenResult{Children: []workmgmt.EpicChild{
		{Number: 30}, {Number: 10}, {Number: 20},
	}}
	a, err := campaign.Assemble("", res)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	f := &itemCapturingFake{}
	if _, err := campaign.Persist(context.Background(), f, "kuhlman-labs/fishhawk", a); err != nil {
		t.Fatalf("Persist: %v", err)
	}
	wantRefs := []string{"issue:30", "issue:10", "issue:20"}
	if len(f.itemsGot) != len(wantRefs) {
		t.Fatalf("persisted %d items, want %d", len(f.itemsGot), len(wantRefs))
	}
	for i, want := range wantRefs {
		if f.itemsGot[i].IssueRef != want {
			t.Errorf("item %d ref = %q, want %q", i, f.itemsGot[i].IssueRef, want)
		}
		if f.itemsGot[i].Position != i {
			t.Errorf("item %d (%s) Position = %d, want %d", i, f.itemsGot[i].IssueRef, f.itemsGot[i].Position, i)
		}
	}
}

// TestPersist_ThreadsGroomingSource is binding condition K3's seam assertion:
// the provenance block must reach the CAMPAIGN's create params, so it rides the
// campaign row's own single-row INSERT rather than a separate write that could
// be lost. A nil block stays nil — the unchanged epic/items path persists NULL.
func TestPersist_ThreadsGroomingSource(t *testing.T) {
	fNone := &itemCapturingFake{}
	if _, err := campaign.Persist(context.Background(), fNone, "kuhlman-labs/fishhawk",
		&campaign.Assembly{EpicRef: "issue:2238"}); err != nil {
		t.Fatalf("Persist (no grooming source): %v", err)
	}
	if fNone.campaignGot.GroomingSource != nil {
		t.Errorf("non-grooming campaign persisted GroomingSource %s, want nil", fNone.campaignGot.GroomingSource)
	}

	provenance := []byte(`{"source_run_id":"abc","report_content_hash":"sha256:deadbeef"}`)
	f := &itemCapturingFake{}
	if _, err := campaign.Persist(context.Background(), f, "kuhlman-labs/fishhawk",
		&campaign.Assembly{EpicRef: "", GroomingSource: provenance}); err != nil {
		t.Fatalf("Persist (grooming source): %v", err)
	}
	if string(f.campaignGot.GroomingSource) != string(provenance) {
		t.Errorf("GroomingSource persisted as %s, want %s", f.campaignGot.GroomingSource, provenance)
	}
}
