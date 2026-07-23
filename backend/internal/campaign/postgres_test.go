package campaign_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/campaign"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// jsonEqual reports whether two raw JSON blobs are semantically equal,
// tolerating the whitespace + key-order normalization Postgres applies when it
// stores a value in a JSONB column (the override survives by VALUE, not bytes).
func jsonEqual(t *testing.T, a, b []byte) bool {
	t.Helper()
	var av, bv any
	if err := json.Unmarshal(a, &av); err != nil {
		t.Fatalf("unmarshal %q: %v", a, err)
	}
	if err := json.Unmarshal(b, &bv); err != nil {
		t.Fatalf("unmarshal %q: %v", b, err)
	}
	return reflect.DeepEqual(av, bv)
}

// makeCampaign creates a campaign with sensible defaults.
func makeCampaign(t *testing.T, repo campaign.Repository) *campaign.Campaign {
	t.Helper()
	c, err := repo.CreateCampaign(context.Background(), campaign.CreateCampaignParams{
		Repo:    "kuhlman-labs/fishhawk",
		EpicRef: "issue:1439",
	})
	if err != nil {
		t.Fatalf("create campaign: %v", err)
	}
	return c
}

func TestPostgres_CreateAndGetCampaign(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := campaign.NewPostgresRepository(pool)

	created := makeCampaign(t, repo)
	if created.State != campaign.StatePending {
		t.Errorf("initial state = %q, want pending", created.State)
	}
	if created.EpicRef != "issue:1439" {
		t.Errorf("epic_ref = %q, want issue:1439", created.EpicRef)
	}

	got, err := repo.GetCampaign(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("get campaign: %v", err)
	}
	if got.ID != created.ID || got.Repo != "kuhlman-labs/fishhawk" {
		t.Errorf("read-back mismatch: %+v", got)
	}
}

func TestPostgres_GetCampaign_NotFound(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := campaign.NewPostgresRepository(pool)

	_, err := repo.GetCampaign(context.Background(), uuid.New())
	if !errors.Is(err, campaign.ErrNotFound) {
		t.Errorf("GetCampaign(missing) err = %v, want ErrNotFound", err)
	}
}

func TestPostgres_ListCampaigns(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := campaign.NewPostgresRepository(pool)

	c := makeCampaign(t, repo)

	// Repo + state filter narrows to the created campaign.
	got, err := repo.ListCampaigns(context.Background(), campaign.ListCampaignsFilter{
		Repo:  "kuhlman-labs/fishhawk",
		State: string(campaign.StatePending),
		Limit: 10,
	})
	if err != nil {
		t.Fatalf("list campaigns: %v", err)
	}
	var found bool
	for _, item := range got {
		if item.ID == c.ID {
			found = true
		}
	}
	if !found {
		t.Errorf("created campaign %s not in filtered list (%d rows)", c.ID, len(got))
	}

	// A non-matching state filter excludes it.
	none, err := repo.ListCampaigns(context.Background(), campaign.ListCampaignsFilter{
		State: string(campaign.StateSucceeded),
		Limit: 10,
	})
	if err != nil {
		t.Fatalf("list campaigns (succeeded): %v", err)
	}
	for _, item := range none {
		if item.ID == c.ID {
			t.Errorf("pending campaign %s leaked into succeeded filter", c.ID)
		}
	}
}

// TestPostgres_GetCampaignAccountID exercises the AccountGetter method — a
// REQUIRED part of campaign.Repository since E44.11 / #2074, so interface
// satisfaction is COMPILER-enforced and the call goes straight through the
// Repository value (ADR-057 / #1830). A tenanted campaign yields its account
// UUID string, an untenanted (NULL account_id) one yields "", and a missing id
// yields ErrNotFound. Also pins that GetCampaign still round-trips a tenanted row
// (the added account_id scan handles non-NULL values).
func TestPostgres_GetCampaignAccountID(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := campaign.NewPostgresRepository(pool)
	ctx := context.Background()

	acct := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO accounts (id, account_key) VALUES ($1, $2)`,
		acct, "acct-"+acct.String()[:8]); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	tenanted, plain := makeCampaign(t, repo), makeCampaign(t, repo)
	if _, err := pool.Exec(ctx, `UPDATE campaigns SET account_id=$1 WHERE id=$2`, acct, tenanted.ID); err != nil {
		t.Fatalf("bind account: %v", err)
	}

	got, err := repo.GetCampaignAccountID(ctx, tenanted.ID)
	if err != nil {
		t.Fatalf("GetCampaignAccountID: %v", err)
	}
	if got != acct.String() {
		t.Errorf("GetCampaignAccountID = %q, want %q", got, acct.String())
	}
	gotPlain, err := repo.GetCampaignAccountID(ctx, plain.ID)
	if err != nil {
		t.Fatalf("GetCampaignAccountID(untenanted): %v", err)
	}
	if gotPlain != "" {
		t.Errorf("untenanted GetCampaignAccountID = %q, want empty", gotPlain)
	}
	if _, err := repo.GetCampaignAccountID(ctx, uuid.New()); !errors.Is(err, campaign.ErrNotFound) {
		t.Errorf("GetCampaignAccountID(missing) err = %v, want ErrNotFound", err)
	}

	// The tenanted row still round-trips through GetCampaign (non-NULL
	// account_id scan).
	rt, err := repo.GetCampaign(ctx, tenanted.ID)
	if err != nil {
		t.Fatalf("get tenanted campaign: %v", err)
	}
	if rt.ID != tenanted.ID {
		t.Errorf("round-trip mismatch: %+v", rt)
	}
}

// TestPostgres_ListCampaigns_AccountFilter exercises
// ListCampaignsFilter.AccountID (ADR-057 / #1830): a set filter keeps
// same-account campaigns PLUS untenanted (NULL account_id) campaigns and
// excludes other accounts' campaigns; an empty filter is no constraint; a
// malformed non-empty value degrades to no constraint (accountIDArg's
// defensive nil mapping — the handler validates the account source).
func TestPostgres_ListCampaigns_AccountFilter(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := campaign.NewPostgresRepository(pool)
	ctx := context.Background()

	acctA, acctB := uuid.New(), uuid.New()
	for _, id := range []uuid.UUID{acctA, acctB} {
		if _, err := pool.Exec(ctx, `INSERT INTO accounts (id, account_key) VALUES ($1, $2)`,
			id, "acct-"+id.String()[:8]); err != nil {
			t.Fatalf("insert account: %v", err)
		}
	}
	campA, campB, campU := makeCampaign(t, repo), makeCampaign(t, repo), makeCampaign(t, repo)
	if _, err := pool.Exec(ctx, `UPDATE campaigns SET account_id=$1 WHERE id=$2`, acctA, campA.ID); err != nil {
		t.Fatalf("bind A: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE campaigns SET account_id=$1 WHERE id=$2`, acctB, campB.ID); err != nil {
		t.Fatalf("bind B: %v", err)
	}

	ids := func(cs []*campaign.Campaign) map[uuid.UUID]bool {
		m := map[uuid.UUID]bool{}
		for _, c := range cs {
			m[c.ID] = true
		}
		return m
	}

	// Account A: A's campaign + the untenanted one visible, B's excluded.
	got, err := repo.ListCampaigns(ctx, campaign.ListCampaignsFilter{AccountID: acctA.String(), Limit: 100})
	if err != nil {
		t.Fatalf("list A: %v", err)
	}
	m := ids(got)
	if !m[campA.ID] || !m[campU.ID] || m[campB.ID] {
		t.Errorf("account A listing = %v; want campA+campU, not campB", m)
	}

	// Empty filter: no constraint — all three visible.
	got, err = repo.ListCampaigns(ctx, campaign.ListCampaignsFilter{Limit: 100})
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	m = ids(got)
	if !m[campA.ID] || !m[campB.ID] || !m[campU.ID] {
		t.Errorf("unfiltered listing = %v; want all three campaigns", m)
	}

	// Malformed non-empty filter: degrades to no constraint rather than
	// erroring (defensive — the handler owns validating the source).
	got, err = repo.ListCampaigns(ctx, campaign.ListCampaignsFilter{AccountID: "not-a-uuid", Limit: 100})
	if err != nil {
		t.Fatalf("list malformed: %v", err)
	}
	if m = ids(got); !m[campA.ID] || !m[campB.ID] || !m[campU.ID] {
		t.Errorf("malformed-filter listing = %v; want all three campaigns (no constraint)", m)
	}
}

func TestPostgres_ListCampaigns_BadPagination(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := campaign.NewPostgresRepository(pool)

	if _, err := repo.ListCampaigns(context.Background(), campaign.ListCampaignsFilter{Limit: 0}); err == nil {
		t.Error("ListCampaigns with Limit=0 should error")
	}
	if _, err := repo.ListCampaigns(context.Background(), campaign.ListCampaignsFilter{Limit: 10, Offset: -1}); err == nil {
		t.Error("ListCampaigns with Offset=-1 should error")
	}
}

func TestPostgres_TransitionCampaign(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := campaign.NewPostgresRepository(pool)
	ctx := context.Background()

	c := makeCampaign(t, repo)

	// Valid edge: pending → running.
	running, err := repo.TransitionCampaign(ctx, c.ID, campaign.StateRunning)
	if err != nil {
		t.Fatalf("transition pending→running: %v", err)
	}
	if running.State != campaign.StateRunning {
		t.Errorf("state after transition = %q, want running", running.State)
	}

	// Idempotent same-state no-op returns the unchanged row.
	same, err := repo.TransitionCampaign(ctx, c.ID, campaign.StateRunning)
	if err != nil {
		t.Fatalf("idempotent running→running: %v", err)
	}
	if same.State != campaign.StateRunning {
		t.Errorf("idempotent transition state = %q, want running", same.State)
	}

	// Invalid edge: running → pending is refused with InvalidTransitionError.
	_, err = repo.TransitionCampaign(ctx, c.ID, campaign.StatePending)
	var ite campaign.InvalidTransitionError
	if !errors.As(err, &ite) {
		t.Fatalf("running→pending err = %v, want InvalidTransitionError", err)
	}
	if ite.Kind != "campaign" || ite.From != "running" || ite.To != "pending" {
		t.Errorf("InvalidTransitionError = %+v, want campaign running→pending", ite)
	}

	// Terminal transition then any further move is refused.
	if _, err := repo.TransitionCampaign(ctx, c.ID, campaign.StateSucceeded); err != nil {
		t.Fatalf("transition running→succeeded: %v", err)
	}
	if _, err := repo.TransitionCampaign(ctx, c.ID, campaign.StateFailed); !errors.As(err, &ite) {
		t.Errorf("succeeded→failed err = %v, want InvalidTransitionError", err)
	}

	// A missing campaign is ErrNotFound.
	if _, err := repo.TransitionCampaign(ctx, uuid.New(), campaign.StateRunning); !errors.Is(err, campaign.ErrNotFound) {
		t.Errorf("transition(missing) err = %v, want ErrNotFound", err)
	}
}

// TestPostgres_ConcurrentTransitionCampaign_ExactlyOneWins exercises the
// SELECT … FOR UPDATE serialization that repository.go documents as a
// load-bearing contract ("two concurrent transition calls observing the same
// prior state cannot both succeed"). Two goroutines race two DIFFERENT
// terminal targets from running; exactly one must win and the loser must
// observe the now-terminal post-transition state and fail with
// InvalidTransitionError (terminal → terminal is forbidden). Mirrors
// run.TestPostgres_ConcurrentDifferentTargets_ExactlyOneWins.
func TestPostgres_ConcurrentTransitionCampaign_ExactlyOneWins(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := campaign.NewPostgresRepository(pool)
	ctx := context.Background()

	c := makeCampaign(t, repo)
	if _, err := repo.TransitionCampaign(ctx, c.ID, campaign.StateRunning); err != nil {
		t.Fatalf("→running: %v", err)
	}

	results := make([]error, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, err := repo.TransitionCampaign(ctx, c.ID, campaign.StateSucceeded)
		results[0] = err
	}()
	go func() {
		defer wg.Done()
		_, err := repo.TransitionCampaign(ctx, c.ID, campaign.StateFailed)
		results[1] = err
	}()
	wg.Wait()

	winners, losers := 0, 0
	for _, err := range results {
		if err == nil {
			winners++
			continue
		}
		var ite campaign.InvalidTransitionError
		if !errors.As(err, &ite) {
			t.Errorf("non-winner err = %v, want InvalidTransitionError", err)
			continue
		}
		losers++
	}
	if winners != 1 || losers != 1 {
		t.Errorf("winners=%d losers=%d, want 1 of each", winners, losers)
	}

	// The campaign settled in exactly one terminal state.
	final, err := repo.GetCampaign(ctx, c.ID)
	if err != nil {
		t.Fatalf("get final campaign: %v", err)
	}
	if !final.State.IsTerminal() {
		t.Errorf("final state = %q, want a terminal state", final.State)
	}
}

func TestPostgres_CampaignItem_RoundTripAndDependsOn(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := campaign.NewPostgresRepository(pool)
	ctx := context.Background()

	c := makeCampaign(t, repo)
	item, err := repo.CreateCampaignItem(ctx, campaign.CreateCampaignItemParams{
		CampaignID: c.ID,
		IssueRef:   "issue:1441",
		DependsOn:  []string{"issue:1440", "issue:1437"},
	})
	if err != nil {
		t.Fatalf("create campaign item: %v", err)
	}
	if item.State != campaign.ItemStatePending {
		t.Errorf("initial item state = %q, want pending", item.State)
	}
	if item.RunID != nil {
		t.Errorf("new item RunID = %v, want nil", item.RunID)
	}

	got, err := repo.GetCampaignItem(ctx, item.ID)
	if err != nil {
		t.Fatalf("get campaign item: %v", err)
	}
	if len(got.DependsOn) != 2 || got.DependsOn[0] != "issue:1440" || got.DependsOn[1] != "issue:1437" {
		t.Errorf("depends_on round-trip = %v, want [issue:1440 issue:1437]", got.DependsOn)
	}

	// An item created with no dependencies reads back a nil/empty slice
	// (the column holds '[]', never NULL).
	bare, err := repo.CreateCampaignItem(ctx, campaign.CreateCampaignItemParams{
		CampaignID: c.ID,
		IssueRef:   "issue:1442",
	})
	if err != nil {
		t.Fatalf("create bare item: %v", err)
	}
	if len(bare.DependsOn) != 0 {
		t.Errorf("bare item depends_on = %v, want empty", bare.DependsOn)
	}

	// Listing by campaign returns both items in insertion order.
	list, err := repo.ListCampaignItemsForCampaign(ctx, c.ID)
	if err != nil {
		t.Fatalf("list items for campaign: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("items for campaign = %d, want 2", len(list))
	}
	if list[0].IssueRef != "issue:1441" || list[1].IssueRef != "issue:1442" {
		t.Errorf("item order = [%q %q], want [issue:1441 issue:1442]", list[0].IssueRef, list[1].IssueRef)
	}

	if _, err := repo.GetCampaignItem(ctx, uuid.New()); !errors.Is(err, campaign.ErrNotFound) {
		t.Errorf("GetCampaignItem(missing) err = %v, want ErrNotFound", err)
	}
}

// TestPostgres_CampaignItem_Autonomy_RoundTripAndFailClosed covers both the
// happy path and the fail-closed CHECK for the campaign_items.autonomy column
// (#1551 / E32.4). A known tier ("low") round-trips onto Item.Autonomy; an item
// created with no autonomy reads back the empty (unknown/default) tier; and an
// out-of-set value ("bogus") is REJECTED by campaign_items_autonomy_check rather
// than silently persisting a tier the engine cannot interpret.
func TestPostgres_CampaignItem_Autonomy_RoundTripAndFailClosed(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := campaign.NewPostgresRepository(pool)
	ctx := context.Background()

	c := makeCampaign(t, repo)

	// Known tier round-trips.
	low, err := repo.CreateCampaignItem(ctx, campaign.CreateCampaignItemParams{
		CampaignID: c.ID,
		IssueRef:   "issue:1532",
		Autonomy:   "low",
	})
	if err != nil {
		t.Fatalf("create autonomy:low item: %v", err)
	}
	if low.Autonomy != "low" {
		t.Errorf("created item autonomy = %q, want low", low.Autonomy)
	}
	got, err := repo.GetCampaignItem(ctx, low.ID)
	if err != nil {
		t.Fatalf("get autonomy:low item: %v", err)
	}
	if got.Autonomy != "low" {
		t.Errorf("read-back autonomy = %q, want low", got.Autonomy)
	}

	// No autonomy → empty (unknown/default) tier, never NULL.
	bare, err := repo.CreateCampaignItem(ctx, campaign.CreateCampaignItemParams{
		CampaignID: c.ID,
		IssueRef:   "issue:1533",
	})
	if err != nil {
		t.Fatalf("create bare item: %v", err)
	}
	if bare.Autonomy != "" {
		t.Errorf("bare item autonomy = %q, want empty", bare.Autonomy)
	}

	// Fail closed: an out-of-set tier is rejected by the CHECK constraint.
	if _, err := repo.CreateCampaignItem(ctx, campaign.CreateCampaignItemParams{
		CampaignID: c.ID,
		IssueRef:   "issue:1534",
		Autonomy:   "bogus",
	}); err == nil {
		t.Error("create item with autonomy='bogus' succeeded, want CHECK-constraint rejection")
	}
}

// TestPostgres_Assemble_OutOfSetAutonomy_DegradesNotAborts is the routed
// untested-path seam test (#1551 / E32.4 fix-up): it drives the FULL
// Assemble→Persist path from an EpicChild carrying an OUT-OF-SET autonomy tier
// (a typo'd label such as `autonomy:critical`) and asserts persistence
// SUCCEEDS with the campaign_items.autonomy column stored as "" — the mislabeled
// child degrades to the non-human-led default instead of aborting the entire
// epic campaign with an SQLSTATE 23514 CHECK violation. This pins the campaign
// package's half of the fix; the parse boundary's normalization of the label
// itself is pinned by TestParseAutonomyLabel in the github provider package.
func TestPostgres_Assemble_OutOfSetAutonomy_DegradesNotAborts(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := campaign.NewPostgresRepository(pool)
	ctx := context.Background()

	res := &workmgmt.EpicChildrenResult{
		Children: []workmgmt.EpicChild{
			{Number: 41, Autonomy: "critical"}, // out-of-set typo → must degrade to ""
			{Number: 42, Autonomy: "low"},      // valid tier passes through
		},
	}
	a, err := campaign.Assemble("issue:40", res)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	// Assemble normalizes the out-of-set tier to "" before it can reach the DB.
	if a.Items[0].Autonomy != "" {
		t.Errorf("assembled item[0] autonomy = %q, want empty (normalized)", a.Items[0].Autonomy)
	}
	if a.Items[1].Autonomy != "low" {
		t.Errorf("assembled item[1] autonomy = %q, want low (passthrough)", a.Items[1].Autonomy)
	}

	// Persist must SUCCEED — no CHECK-constraint abort on the mislabeled child.
	c, err := campaign.Persist(ctx, repo, "kuhlman-labs/fishhawk", a)
	if err != nil {
		t.Fatalf("Persist aborted on out-of-set tier (want success): %v", err)
	}

	items, err := repo.ListCampaignItemsForCampaign(ctx, c.ID)
	if err != nil {
		t.Fatalf("ListCampaignItemsForCampaign: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("persisted %d items, want 2", len(items))
	}
	if items[0].IssueRef != "issue:41" || items[0].Autonomy != "" {
		t.Errorf("item issue:41 autonomy = %q, want empty", items[0].Autonomy)
	}
	if items[1].IssueRef != "issue:42" || items[1].Autonomy != "low" {
		t.Errorf("item issue:42 autonomy = %q, want low", items[1].Autonomy)
	}

	// Read the raw column too: prove campaign_items.autonomy itself holds "",
	// satisfying the fail-closed 0049 CHECK rather than the rejected "critical".
	var col string
	if err := pool.QueryRow(ctx,
		`SELECT autonomy FROM campaign_items WHERE id = $1`, items[0].ID,
	).Scan(&col); err != nil {
		t.Fatalf("read autonomy column: %v", err)
	}
	if col != "" {
		t.Errorf("campaign_items.autonomy column = %q, want empty", col)
	}
}

// TestPostgres_CampaignItem_MalformedDependsOn_Tolerated asserts the
// rowToCampaignItem tolerance branch: a depends_on payload that is valid
// JSONB but not a []string (so json.Unmarshal into []string fails) is
// DROPPED to a nil slice rather than failing the read — mirroring
// run.rowToRun's drop-don't-500 posture on its JSONB columns. The bad
// payload is written via raw SQL because the repo write path always
// produces a well-formed array.
func TestPostgres_CampaignItem_MalformedDependsOn_Tolerated(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := campaign.NewPostgresRepository(pool)
	ctx := context.Background()

	c := makeCampaign(t, repo)
	item, err := repo.CreateCampaignItem(ctx, campaign.CreateCampaignItemParams{
		CampaignID: c.ID,
		IssueRef:   "issue:1441",
		DependsOn:  []string{"issue:1440"},
	})
	if err != nil {
		t.Fatalf("create item: %v", err)
	}

	// Overwrite depends_on with valid JSONB that is NOT a string array.
	if _, err := pool.Exec(ctx,
		`UPDATE campaign_items SET depends_on = '{"not":"an-array"}'::jsonb WHERE id = $1`,
		item.ID,
	); err != nil {
		t.Fatalf("write malformed depends_on: %v", err)
	}

	got, err := repo.GetCampaignItem(ctx, item.ID)
	if err != nil {
		t.Fatalf("get item with malformed depends_on should not error: %v", err)
	}
	if got.DependsOn != nil {
		t.Errorf("malformed depends_on = %v, want nil (dropped, not surfaced)", got.DependsOn)
	}
}

func TestPostgres_TransitionCampaignItem(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := campaign.NewPostgresRepository(pool)
	ctx := context.Background()

	c := makeCampaign(t, repo)
	item, err := repo.CreateCampaignItem(ctx, campaign.CreateCampaignItemParams{
		CampaignID: c.ID,
		IssueRef:   "issue:1441",
		DependsOn:  []string{"issue:1440"},
	})
	if err != nil {
		t.Fatalf("create item: %v", err)
	}

	// pending → blocked (dependency open) → running (dependency cleared).
	if _, err := repo.TransitionCampaignItem(ctx, item.ID, campaign.ItemStateBlocked); err != nil {
		t.Fatalf("pending→blocked: %v", err)
	}
	if _, err := repo.TransitionCampaignItem(ctx, item.ID, campaign.ItemStateRunning); err != nil {
		t.Fatalf("blocked→running: %v", err)
	}

	// Invalid: running → blocked is refused.
	_, err = repo.TransitionCampaignItem(ctx, item.ID, campaign.ItemStateBlocked)
	var ite campaign.InvalidTransitionError
	if !errors.As(err, &ite) {
		t.Fatalf("running→blocked err = %v, want InvalidTransitionError", err)
	}
	if ite.Kind != "campaign_item" {
		t.Errorf("error kind = %q, want campaign_item", ite.Kind)
	}

	// running → succeeded, then terminal rejects further moves.
	if _, err := repo.TransitionCampaignItem(ctx, item.ID, campaign.ItemStateSucceeded); err != nil {
		t.Fatalf("running→succeeded: %v", err)
	}
	if _, err := repo.TransitionCampaignItem(ctx, item.ID, campaign.ItemStateFailed); !errors.As(err, &ite) {
		t.Errorf("succeeded→failed err = %v, want InvalidTransitionError", err)
	}

	if _, err := repo.TransitionCampaignItem(ctx, uuid.New(), campaign.ItemStateRunning); !errors.Is(err, campaign.ErrNotFound) {
		t.Errorf("transition(missing) err = %v, want ErrNotFound", err)
	}
}

// TestPostgres_ConcurrentTransitionCampaignItem_ExactlyOneWins is the
// campaign_item analogue of the campaign concurrency test: it covers the
// LockCampaignItemForUpdate serialization path that CI cannot infer from the
// sequential happy-path cases. Two goroutines race succeeded vs failed from
// running; exactly one wins and the loser observes the now-terminal state via
// InvalidTransitionError.
func TestPostgres_ConcurrentTransitionCampaignItem_ExactlyOneWins(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := campaign.NewPostgresRepository(pool)
	ctx := context.Background()

	c := makeCampaign(t, repo)
	item, err := repo.CreateCampaignItem(ctx, campaign.CreateCampaignItemParams{
		CampaignID: c.ID,
		IssueRef:   "issue:1441",
	})
	if err != nil {
		t.Fatalf("create item: %v", err)
	}
	if _, err := repo.TransitionCampaignItem(ctx, item.ID, campaign.ItemStateRunning); err != nil {
		t.Fatalf("→running: %v", err)
	}

	results := make([]error, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, err := repo.TransitionCampaignItem(ctx, item.ID, campaign.ItemStateSucceeded)
		results[0] = err
	}()
	go func() {
		defer wg.Done()
		_, err := repo.TransitionCampaignItem(ctx, item.ID, campaign.ItemStateFailed)
		results[1] = err
	}()
	wg.Wait()

	winners, losers := 0, 0
	for _, err := range results {
		if err == nil {
			winners++
			continue
		}
		var ite campaign.InvalidTransitionError
		if !errors.As(err, &ite) {
			t.Errorf("non-winner err = %v, want InvalidTransitionError", err)
			continue
		}
		losers++
	}
	if winners != 1 || losers != 1 {
		t.Errorf("winners=%d losers=%d, want 1 of each", winners, losers)
	}

	final, err := repo.GetCampaignItem(ctx, item.ID)
	if err != nil {
		t.Fatalf("get final item: %v", err)
	}
	if !final.State.IsTerminal() {
		t.Errorf("final item state = %q, want a terminal state", final.State)
	}
}

// TestPostgres_CreateCampaign_PausePolicy covers the pause_policy column added
// by 0040: makeCampaign (no policy set) defaults to the conservative
// pause_campaign, and an explicit pause_item round-trips through create + read.
func TestPostgres_CreateCampaign_PausePolicy(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := campaign.NewPostgresRepository(pool)
	ctx := context.Background()

	// Zero policy normalizes to the block-the-campaign default.
	def := makeCampaign(t, repo)
	if def.PausePolicy != campaign.PausePolicyPauseCampaign {
		t.Errorf("default PausePolicy = %q, want pause_campaign", def.PausePolicy)
	}

	// Explicit pause_item is preserved end-to-end.
	c, err := repo.CreateCampaign(ctx, campaign.CreateCampaignParams{
		Repo:        "kuhlman-labs/fishhawk",
		EpicRef:     "issue:1439",
		PausePolicy: campaign.PausePolicyPauseItem,
	})
	if err != nil {
		t.Fatalf("create campaign with pause_item: %v", err)
	}
	if c.PausePolicy != campaign.PausePolicyPauseItem {
		t.Errorf("created PausePolicy = %q, want pause_item", c.PausePolicy)
	}
	got, err := repo.GetCampaign(ctx, c.ID)
	if err != nil {
		t.Fatalf("get campaign: %v", err)
	}
	if got.PausePolicy != campaign.PausePolicyPauseItem {
		t.Errorf("read-back PausePolicy = %q, want pause_item", got.PausePolicy)
	}
}

// TestPostgres_CreateCampaign_OperatorAgent covers the operator_agent column
// added by 0041 (E25.12): a campaign created with no override round-trips as
// nil (the unchanged-behavior done-means), and a non-nil raw JSONB block
// survives create + read byte-for-byte (opaque passthrough, the campaign
// package never interprets it).
func TestPostgres_CreateCampaign_OperatorAgent(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := campaign.NewPostgresRepository(pool)
	ctx := context.Background()

	// No override → nil on create AND read-back (the unchanged-behavior path).
	def := makeCampaign(t, repo)
	if def.OperatorAgent != nil {
		t.Errorf("default OperatorAgent = %q, want nil (no override)", def.OperatorAgent)
	}
	gotDef, err := repo.GetCampaign(ctx, def.ID)
	if err != nil {
		t.Fatalf("get campaign (no override): %v", err)
	}
	if gotDef.OperatorAgent != nil {
		t.Errorf("read-back OperatorAgent = %q, want nil (no override)", gotDef.OperatorAgent)
	}

	// Explicit override is preserved end-to-end, byte-for-byte.
	override := []byte(`{"may_approve":"solo_low","must_page_human":["reviewer_reject"]}`)
	c, err := repo.CreateCampaign(ctx, campaign.CreateCampaignParams{
		Repo:          "kuhlman-labs/fishhawk",
		EpicRef:       "issue:1451",
		OperatorAgent: override,
	})
	if err != nil {
		t.Fatalf("create campaign with operator_agent: %v", err)
	}
	if !jsonEqual(t, c.OperatorAgent, override) {
		t.Errorf("created OperatorAgent = %q, want value-equal to %q", c.OperatorAgent, override)
	}
	got, err := repo.GetCampaign(ctx, c.ID)
	if err != nil {
		t.Fatalf("get campaign: %v", err)
	}
	if !jsonEqual(t, got.OperatorAgent, override) {
		t.Errorf("read-back OperatorAgent = %q, want value-equal to %q", got.OperatorAgent, override)
	}
}

// TestPostgres_GetCampaignByIdempotencyKey_HappyPath covers the E25.13 (#1455)
// idempotency lookup + the idempotency_key round-trip through CreateCampaign: a
// campaign created with a key is found by (repo, key) and reads back the same
// key. Mirrors run TestPostgres_GetRunByIdempotencyKey_HappyPath.
func TestPostgres_GetCampaignByIdempotencyKey_HappyPath(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := campaign.NewPostgresRepository(pool)
	ctx := context.Background()

	key := "abc123"
	created, err := repo.CreateCampaign(ctx, campaign.CreateCampaignParams{
		Repo:           "kuhlman-labs/fishhawk",
		EpicRef:        "issue:1455",
		IdempotencyKey: &key,
	})
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	if created.IdempotencyKey == nil || *created.IdempotencyKey != key {
		t.Errorf("created IdempotencyKey = %v, want %q", created.IdempotencyKey, key)
	}
	got, err := repo.GetCampaignByIdempotencyKey(ctx, "kuhlman-labs/fishhawk", key)
	if err != nil {
		t.Fatalf("GetCampaignByIdempotencyKey: %v", err)
	}
	if got.ID != created.ID {
		t.Errorf("got %s, want %s", got.ID, created.ID)
	}
	if got.IdempotencyKey == nil || *got.IdempotencyKey != key {
		t.Errorf("read-back IdempotencyKey round-trip failed: %v", got.IdempotencyKey)
	}
}

// TestPostgres_GetCampaignByIdempotencyKey_NotFound: an unknown key yields
// ErrNotFound (the handler's fall-through-to-create signal).
func TestPostgres_GetCampaignByIdempotencyKey_NotFound(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := campaign.NewPostgresRepository(pool)
	_, err := repo.GetCampaignByIdempotencyKey(context.Background(), "kuhlman-labs/fishhawk", "nope")
	if !errors.Is(err, campaign.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// TestPostgres_DuplicateCampaignIdempotencyKey_ConflictsAtDB pins the DB-level
// guarantee behind the handler's lookup-then-create: the partial unique index
// over (repo, idempotency_key) WHERE idempotency_key IS NOT NULL means a race
// between two callers picking the same key can't both insert. Mirrors run
// TestPostgres_DuplicateIdempotencyKey_ConflictsAtDB.
func TestPostgres_DuplicateCampaignIdempotencyKey_ConflictsAtDB(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := campaign.NewPostgresRepository(pool)
	ctx := context.Background()

	key := "shared"
	if _, err := repo.CreateCampaign(ctx, campaign.CreateCampaignParams{
		Repo:           "kuhlman-labs/fishhawk",
		EpicRef:        "issue:1",
		IdempotencyKey: &key,
	}); err != nil {
		t.Fatalf("first CreateCampaign: %v", err)
	}
	_, err := repo.CreateCampaign(ctx, campaign.CreateCampaignParams{
		Repo:           "kuhlman-labs/fishhawk",
		EpicRef:        "issue:2",
		IdempotencyKey: &key,
	})
	if err == nil {
		t.Fatal("expected duplicate-key error from DB")
	}
}

// TestPostgres_NullCampaignIdempotencyKey_DoesNotCollide: two campaigns with no
// key (nil) both succeed — the partial index excludes NULLs so keyless
// campaigns never conflict.
func TestPostgres_NullCampaignIdempotencyKey_DoesNotCollide(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := campaign.NewPostgresRepository(pool)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if _, err := repo.CreateCampaign(ctx, campaign.CreateCampaignParams{
			Repo:    "kuhlman-labs/fishhawk",
			EpicRef: "issue:1",
		}); err != nil {
			t.Fatalf("CreateCampaign #%d: %v", i, err)
		}
	}
}

// TestPostgres_TransitionCampaign_Paused covers the campaign-level paused
// overlay edges admitted by 0040: running → paused, then resume paused →
// running. The insert succeeding proves the widened campaigns_state_check.
func TestPostgres_TransitionCampaign_Paused(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := campaign.NewPostgresRepository(pool)
	ctx := context.Background()

	c := makeCampaign(t, repo)
	if _, err := repo.TransitionCampaign(ctx, c.ID, campaign.StateRunning); err != nil {
		t.Fatalf("→running: %v", err)
	}
	paused, err := repo.TransitionCampaign(ctx, c.ID, campaign.StatePaused)
	if err != nil {
		t.Fatalf("running→paused: %v", err)
	}
	if paused.State != campaign.StatePaused {
		t.Errorf("state after pause = %q, want paused", paused.State)
	}
	// Resume.
	running, err := repo.TransitionCampaign(ctx, c.ID, campaign.StateRunning)
	if err != nil {
		t.Fatalf("paused→running (resume): %v", err)
	}
	if running.State != campaign.StateRunning {
		t.Errorf("state after resume = %q, want running", running.State)
	}
}

// TestPostgres_PauseCampaignItem is the round-trip done-means for the item
// pause carrier: a running item paused via PauseCampaignItem persists state
// 'paused' (proving the widened campaign_items_state_check) with the
// PauseReason JSONB intact on both the returned row and an independent read.
// A re-pause is an idempotent no-op preserving the first reason, and the item
// resumes paused → running.
func TestPostgres_PauseCampaignItem(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := campaign.NewPostgresRepository(pool)
	ctx := context.Background()

	c := makeCampaign(t, repo)
	item, err := repo.CreateCampaignItem(ctx, campaign.CreateCampaignItemParams{
		CampaignID: c.ID,
		IssueRef:   "issue:1441",
	})
	if err != nil {
		t.Fatalf("create item: %v", err)
	}
	if item.PauseReason != nil {
		t.Errorf("new item PauseReason = %v, want nil", item.PauseReason)
	}
	// Only running → paused is valid; advance the item first.
	if _, err := repo.TransitionCampaignItem(ctx, item.ID, campaign.ItemStateRunning); err != nil {
		t.Fatalf("→running: %v", err)
	}

	runID := uuid.New()
	stageID := uuid.New()
	reason := campaign.PauseReason{
		PageEvent: "campaign_gate_paged",
		RunID:     &runID,
		StageID:   &stageID,
		Gate:      "implement_review",
	}
	paused, err := repo.PauseCampaignItem(ctx, item.ID, reason)
	if err != nil {
		t.Fatalf("pause item: %v", err)
	}
	if paused.State != campaign.ItemStatePaused {
		t.Errorf("state after pause = %q, want paused", paused.State)
	}
	assertReason := func(label string, pr *campaign.PauseReason) {
		t.Helper()
		if pr == nil {
			t.Fatalf("%s: PauseReason = nil, want round-tripped reason", label)
		}
		if pr.PageEvent != "campaign_gate_paged" || pr.Gate != "implement_review" {
			t.Errorf("%s: PauseReason = %+v, want page=campaign_gate_paged gate=implement_review", label, pr)
		}
		if pr.RunID == nil || *pr.RunID != runID {
			t.Errorf("%s: PauseReason.RunID = %v, want %s", label, pr.RunID, runID)
		}
		if pr.StageID == nil || *pr.StageID != stageID {
			t.Errorf("%s: PauseReason.StageID = %v, want %s", label, pr.StageID, stageID)
		}
	}
	assertReason("returned", paused.PauseReason)

	// Independent read-back carries the same reason.
	got, err := repo.GetCampaignItem(ctx, item.ID)
	if err != nil {
		t.Fatalf("get paused item: %v", err)
	}
	assertReason("read-back", got.PauseReason)

	// Idempotent re-pause: unchanged item, first reason preserved.
	re, err := repo.PauseCampaignItem(ctx, item.ID, campaign.PauseReason{PageEvent: "different"})
	if err != nil {
		t.Fatalf("re-pause: %v", err)
	}
	assertReason("idempotent-repause", re.PauseReason)

	// Resume re-engages the item.
	resumed, err := repo.TransitionCampaignItem(ctx, item.ID, campaign.ItemStateRunning)
	if err != nil {
		t.Fatalf("paused→running (resume): %v", err)
	}
	if resumed.State != campaign.ItemStateRunning {
		t.Errorf("state after resume = %q, want running", resumed.State)
	}
}

// TestPostgres_CampaignItem_MalformedPauseReason_Tolerated asserts the
// rowToCampaignItem tolerance branch for pause_reason: a JSONB blob that is
// valid JSONB but not a PauseReason object (so json.Unmarshal fails) is
// DROPPED to a nil *PauseReason rather than failing the read — same posture as
// the depends_on tolerance. Written via raw SQL because the repo write path
// always marshals a well-formed object.
func TestPostgres_CampaignItem_MalformedPauseReason_Tolerated(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := campaign.NewPostgresRepository(pool)
	ctx := context.Background()

	c := makeCampaign(t, repo)
	item, err := repo.CreateCampaignItem(ctx, campaign.CreateCampaignItemParams{
		CampaignID: c.ID,
		IssueRef:   "issue:1441",
	})
	if err != nil {
		t.Fatalf("create item: %v", err)
	}

	// Overwrite pause_reason with valid JSONB that is NOT a PauseReason object.
	if _, err := pool.Exec(ctx,
		`UPDATE campaign_items SET pause_reason = '["not","an-object"]'::jsonb WHERE id = $1`,
		item.ID,
	); err != nil {
		t.Fatalf("write malformed pause_reason: %v", err)
	}

	got, err := repo.GetCampaignItem(ctx, item.ID)
	if err != nil {
		t.Fatalf("get item with malformed pause_reason should not error: %v", err)
	}
	if got.PauseReason != nil {
		t.Errorf("malformed pause_reason = %+v, want nil (dropped, not surfaced)", got.PauseReason)
	}
}

// TestPostgres_PauseCampaignItem_InvalidAndMissing covers the two error
// branches of PauseCampaignItem: pausing a non-running (pending) item is
// refused with InvalidTransitionError (only running → paused is valid), and a
// missing item is ErrNotFound.
func TestPostgres_PauseCampaignItem_InvalidAndMissing(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := campaign.NewPostgresRepository(pool)
	ctx := context.Background()

	c := makeCampaign(t, repo)
	item, err := repo.CreateCampaignItem(ctx, campaign.CreateCampaignItemParams{
		CampaignID: c.ID,
		IssueRef:   "issue:1441",
	})
	if err != nil {
		t.Fatalf("create item: %v", err)
	}

	// pending → paused is refused (item must be running first).
	_, err = repo.PauseCampaignItem(ctx, item.ID, campaign.PauseReason{PageEvent: "x"})
	var ite campaign.InvalidTransitionError
	if !errors.As(err, &ite) {
		t.Fatalf("pause(pending) err = %v, want InvalidTransitionError", err)
	}
	if ite.Kind != "campaign_item" || ite.To != "paused" {
		t.Errorf("InvalidTransitionError = %+v, want campaign_item →paused", ite)
	}

	// A missing item is ErrNotFound.
	if _, err := repo.PauseCampaignItem(ctx, uuid.New(), campaign.PauseReason{}); !errors.Is(err, campaign.ErrNotFound) {
		t.Errorf("pause(missing) err = %v, want ErrNotFound", err)
	}
}

// TestPostgres_RestartCampaignItem is the E32.9 (#1729) round-trip + per-mode
// done-means for the operator restart reset. It drives one item per named mode
// through the reset:
//   - cancelled → pending, run link cleared (the operator-verb path);
//   - failed → pending, run link cleared (the broader repo contract);
//   - succeeded → InvalidTransitionError (not restartable);
//   - running → InvalidTransitionError (not restartable);
//   - missing → ErrNotFound.
//
// A REAL runs row is linked so the run-link-clearing is observable.
func TestPostgres_RestartCampaignItem(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := campaign.NewPostgresRepository(pool)
	runRepo := run.NewPostgresRepository(pool)
	ctx := context.Background()
	c := makeCampaign(t, repo)

	// mkLinkedItem creates an item linked to a fresh run and drives it to the
	// requested state through the valid running-first path.
	mkLinkedItem := func(t *testing.T, ref string, to campaign.ItemState) *campaign.Item {
		t.Helper()
		r, err := runRepo.CreateRun(ctx, run.CreateRunParams{
			Repo:          "kuhlman-labs/fishhawk",
			WorkflowID:    "feature_change",
			WorkflowSHA:   "deadbeef",
			TriggerSource: run.TriggerCLI,
		})
		if err != nil {
			t.Fatalf("create run: %v", err)
		}
		it, err := repo.CreateCampaignItem(ctx, campaign.CreateCampaignItemParams{
			CampaignID: c.ID,
			IssueRef:   ref,
		})
		if err != nil {
			t.Fatalf("create item: %v", err)
		}
		if _, err := repo.SetCampaignItemRun(ctx, it.ID, &r.ID); err != nil {
			t.Fatalf("link run: %v", err)
		}
		if _, err := repo.TransitionCampaignItem(ctx, it.ID, campaign.ItemStateRunning); err != nil {
			t.Fatalf("→running: %v", err)
		}
		if to != campaign.ItemStateRunning {
			if _, err := repo.TransitionCampaignItem(ctx, it.ID, to); err != nil {
				t.Fatalf("→%s: %v", to, err)
			}
		}
		return it
	}

	// cancelled → pending, run link cleared.
	cancelled := mkLinkedItem(t, "issue:cancelled", campaign.ItemStateCancelled)
	restarted, err := repo.RestartCampaignItem(ctx, cancelled.ID)
	if err != nil {
		t.Fatalf("restart(cancelled): %v", err)
	}
	if restarted.State != campaign.ItemStatePending {
		t.Errorf("restart(cancelled) state = %q, want pending", restarted.State)
	}
	if restarted.RunID != nil {
		t.Errorf("restart(cancelled) run_id = %v, want nil (link cleared)", restarted.RunID)
	}
	// Persisted, not just returned.
	if got, _ := repo.GetCampaignItem(ctx, cancelled.ID); got.State != campaign.ItemStatePending || got.RunID != nil {
		t.Errorf("persisted cancelled restart = state %q run_id %v, want pending/nil", got.State, got.RunID)
	}

	// failed → pending, run link cleared (the broader repo contract).
	failed := mkLinkedItem(t, "issue:failed", campaign.ItemStateFailed)
	rf, err := repo.RestartCampaignItem(ctx, failed.ID)
	if err != nil {
		t.Fatalf("restart(failed): %v", err)
	}
	if rf.State != campaign.ItemStatePending || rf.RunID != nil {
		t.Errorf("restart(failed) = state %q run_id %v, want pending/nil", rf.State, rf.RunID)
	}

	// succeeded → InvalidTransitionError (not a restartable state).
	succeeded := mkLinkedItem(t, "issue:succeeded", campaign.ItemStateSucceeded)
	var ite campaign.InvalidTransitionError
	if _, err := repo.RestartCampaignItem(ctx, succeeded.ID); !errors.As(err, &ite) {
		t.Errorf("restart(succeeded) err = %v, want InvalidTransitionError", err)
	} else if ite.Kind != "campaign_item" || ite.From != "succeeded" {
		t.Errorf("restart(succeeded) err = %+v, want campaign_item from succeeded", ite)
	}

	// running → InvalidTransitionError (not a restartable state).
	running := mkLinkedItem(t, "issue:running", campaign.ItemStateRunning)
	if _, err := repo.RestartCampaignItem(ctx, running.ID); !errors.As(err, &ite) {
		t.Errorf("restart(running) err = %v, want InvalidTransitionError", err)
	}

	// missing → ErrNotFound.
	if _, err := repo.RestartCampaignItem(ctx, uuid.New()); !errors.Is(err, campaign.ErrNotFound) {
		t.Errorf("restart(missing) err = %v, want ErrNotFound", err)
	}
}

// TestPostgres_SettleCampaignItemOutOfBand pins the #2029 guard-bypassing
// terminal→succeeded settle: a cancelled or failed item is settled succeeded
// with its run link RETAINED (provenance to the dead run), while every other
// from-state is rejected InvalidTransitionError and a missing item is
// ErrNotFound. Modeled on TestPostgres_RestartCampaignItem's per-mode round
// trip, differing in the target (succeeded, not pending) and the retained link.
func TestPostgres_SettleCampaignItemOutOfBand(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := campaign.NewPostgresRepository(pool)
	runRepo := run.NewPostgresRepository(pool)
	ctx := context.Background()
	c := makeCampaign(t, repo)

	// mkLinkedItem creates an item linked to a fresh run and drives it to the
	// requested state through the valid running-first path, returning the item
	// and the run id it was linked to.
	mkLinkedItem := func(t *testing.T, ref string, to campaign.ItemState) (*campaign.Item, uuid.UUID) {
		t.Helper()
		r, err := runRepo.CreateRun(ctx, run.CreateRunParams{
			Repo:          "kuhlman-labs/fishhawk",
			WorkflowID:    "feature_change",
			WorkflowSHA:   "deadbeef",
			TriggerSource: run.TriggerCLI,
		})
		if err != nil {
			t.Fatalf("create run: %v", err)
		}
		it, err := repo.CreateCampaignItem(ctx, campaign.CreateCampaignItemParams{
			CampaignID: c.ID,
			IssueRef:   ref,
		})
		if err != nil {
			t.Fatalf("create item: %v", err)
		}
		if _, err := repo.SetCampaignItemRun(ctx, it.ID, &r.ID); err != nil {
			t.Fatalf("link run: %v", err)
		}
		if _, err := repo.TransitionCampaignItem(ctx, it.ID, campaign.ItemStateRunning); err != nil {
			t.Fatalf("→running: %v", err)
		}
		if to != campaign.ItemStateRunning {
			if _, err := repo.TransitionCampaignItem(ctx, it.ID, to); err != nil {
				t.Fatalf("→%s: %v", to, err)
			}
		}
		return it, r.ID
	}

	// cancelled → succeeded, run link RETAINED (unlike restart, which clears it).
	cancelled, cancelledRun := mkLinkedItem(t, "issue:cancelled", campaign.ItemStateCancelled)
	settled, err := repo.SettleCampaignItemOutOfBand(ctx, cancelled.ID)
	if err != nil {
		t.Fatalf("settle(cancelled): %v", err)
	}
	if settled.State != campaign.ItemStateSucceeded {
		t.Errorf("settle(cancelled) state = %q, want succeeded", settled.State)
	}
	if settled.RunID == nil || *settled.RunID != cancelledRun {
		t.Errorf("settle(cancelled) run_id = %v, want %s retained", settled.RunID, cancelledRun)
	}
	// Persisted, not just returned — state succeeded AND run link intact.
	if got, _ := repo.GetCampaignItem(ctx, cancelled.ID); got.State != campaign.ItemStateSucceeded ||
		got.RunID == nil || *got.RunID != cancelledRun {
		t.Errorf("persisted cancelled settle = state %q run_id %v, want succeeded/%s", got.State, got.RunID, cancelledRun)
	}

	// failed → succeeded, run link retained.
	failed, failedRun := mkLinkedItem(t, "issue:failed", campaign.ItemStateFailed)
	sf, err := repo.SettleCampaignItemOutOfBand(ctx, failed.ID)
	if err != nil {
		t.Fatalf("settle(failed): %v", err)
	}
	if sf.State != campaign.ItemStateSucceeded {
		t.Errorf("settle(failed) state = %q, want succeeded", sf.State)
	}
	if sf.RunID == nil || *sf.RunID != failedRun {
		t.Errorf("settle(failed) run_id = %v, want %s retained", sf.RunID, failedRun)
	}

	// succeeded → InvalidTransitionError (not a settleable terminal-non-succeeded
	// state; also guards against a concurrent double-settle).
	succeeded, _ := mkLinkedItem(t, "issue:succeeded", campaign.ItemStateSucceeded)
	var ite campaign.InvalidTransitionError
	if _, err := repo.SettleCampaignItemOutOfBand(ctx, succeeded.ID); !errors.As(err, &ite) {
		t.Errorf("settle(succeeded) err = %v, want InvalidTransitionError", err)
	} else if ite.Kind != "campaign_item" || ite.From != "succeeded" || ite.To != "succeeded" {
		t.Errorf("settle(succeeded) err = %+v, want campaign_item succeeded→succeeded", ite)
	}

	// running → InvalidTransitionError (not a terminal state).
	running, _ := mkLinkedItem(t, "issue:running", campaign.ItemStateRunning)
	if _, err := repo.SettleCampaignItemOutOfBand(ctx, running.ID); !errors.As(err, &ite) {
		t.Errorf("settle(running) err = %v, want InvalidTransitionError", err)
	}

	// pending → InvalidTransitionError (a run-less pending item is class A's
	// guarded path, never this bypass).
	pending, err := repo.CreateCampaignItem(ctx, campaign.CreateCampaignItemParams{
		CampaignID: c.ID,
		IssueRef:   "issue:pending",
	})
	if err != nil {
		t.Fatalf("create pending item: %v", err)
	}
	if _, err := repo.SettleCampaignItemOutOfBand(ctx, pending.ID); !errors.As(err, &ite) {
		t.Errorf("settle(pending) err = %v, want InvalidTransitionError", err)
	}

	// missing → ErrNotFound.
	if _, err := repo.SettleCampaignItemOutOfBand(ctx, uuid.New()); !errors.Is(err, campaign.ErrNotFound) {
		t.Errorf("settle(missing) err = %v, want ErrNotFound", err)
	}
}

// TestPostgres_RunLinkage_EndToEnd spans domain → persistence → run linkage:
// it inserts a REAL runs row (via the run repo, exercising the cross-package
// boundary), attaches it to a campaign item, and asserts both forward
// (GetCampaignItem.RunID) and reverse (ListCampaignItemsForRun) discovery
// resolve the link.
func TestPostgres_RunLinkage_EndToEnd(t *testing.T) {
	pool := pgtest.NewPool(t)
	campaignRepo := campaign.NewPostgresRepository(pool)
	runRepo := run.NewPostgresRepository(pool)
	ctx := context.Background()

	r, err := runRepo.CreateRun(ctx, run.CreateRunParams{
		Repo:          "kuhlman-labs/fishhawk",
		WorkflowID:    "feature_change",
		WorkflowSHA:   "deadbeef",
		TriggerSource: run.TriggerCLI,
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}

	c := makeCampaign(t, campaignRepo)
	item, err := campaignRepo.CreateCampaignItem(ctx, campaign.CreateCampaignItemParams{
		CampaignID: c.ID,
		IssueRef:   "issue:1441",
	})
	if err != nil {
		t.Fatalf("create item: %v", err)
	}

	// Attach the run.
	linked, err := campaignRepo.SetCampaignItemRun(ctx, item.ID, &r.ID)
	if err != nil {
		t.Fatalf("set item run: %v", err)
	}
	if linked.RunID == nil || *linked.RunID != r.ID {
		t.Fatalf("SetCampaignItemRun RunID = %v, want %s", linked.RunID, r.ID)
	}

	// Forward discovery.
	got, err := campaignRepo.GetCampaignItem(ctx, item.ID)
	if err != nil {
		t.Fatalf("get item: %v", err)
	}
	if got.RunID == nil || *got.RunID != r.ID {
		t.Errorf("GetCampaignItem.RunID = %v, want %s", got.RunID, r.ID)
	}

	// Reverse discovery: which campaign item owns this run.
	forRun, err := campaignRepo.ListCampaignItemsForRun(ctx, r.ID)
	if err != nil {
		t.Fatalf("list items for run: %v", err)
	}
	if len(forRun) != 1 || forRun[0].ID != item.ID {
		t.Fatalf("ListCampaignItemsForRun = %+v, want one item %s", forRun, item.ID)
	}

	// SetCampaignItemRun on a missing item is ErrNotFound.
	if _, err := campaignRepo.SetCampaignItemRun(ctx, uuid.New(), &r.ID); !errors.Is(err, campaign.ErrNotFound) {
		t.Errorf("SetCampaignItemRun(missing) err = %v, want ErrNotFound", err)
	}
}

// TestPostgres_RunDelete_SetsItemRunNull asserts the ON DELETE SET NULL FK:
// deleting the linked run nulls the item's run_id and preserves the item row
// (campaign history survives a run deletion).
func TestPostgres_RunDelete_SetsItemRunNull(t *testing.T) {
	pool := pgtest.NewPool(t)
	campaignRepo := campaign.NewPostgresRepository(pool)
	runRepo := run.NewPostgresRepository(pool)
	ctx := context.Background()

	r, err := runRepo.CreateRun(ctx, run.CreateRunParams{
		Repo:          "kuhlman-labs/fishhawk",
		WorkflowID:    "feature_change",
		WorkflowSHA:   "deadbeef",
		TriggerSource: run.TriggerCLI,
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	c := makeCampaign(t, campaignRepo)
	item, err := campaignRepo.CreateCampaignItem(ctx, campaign.CreateCampaignItemParams{
		CampaignID: c.ID,
		IssueRef:   "issue:1441",
	})
	if err != nil {
		t.Fatalf("create item: %v", err)
	}
	if _, err := campaignRepo.SetCampaignItemRun(ctx, item.ID, &r.ID); err != nil {
		t.Fatalf("set item run: %v", err)
	}

	// Delete the run row directly (no run-repo delete verb exists).
	if _, err := pool.Exec(ctx, `DELETE FROM runs WHERE id = $1`, r.ID); err != nil {
		t.Fatalf("delete run: %v", err)
	}

	// The item survives with run_id nulled.
	got, err := campaignRepo.GetCampaignItem(ctx, item.ID)
	if err != nil {
		t.Fatalf("get item after run delete: %v", err)
	}
	if got.RunID != nil {
		t.Errorf("item RunID after run delete = %v, want nil (ON DELETE SET NULL)", got.RunID)
	}
}

// TestPostgres_CampaignDelete_CascadesItems asserts the ON DELETE CASCADE FK:
// deleting a campaign removes its items.
func TestPostgres_CampaignDelete_CascadesItems(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := campaign.NewPostgresRepository(pool)
	ctx := context.Background()

	c := makeCampaign(t, repo)
	item, err := repo.CreateCampaignItem(ctx, campaign.CreateCampaignItemParams{
		CampaignID: c.ID,
		IssueRef:   "issue:1441",
	})
	if err != nil {
		t.Fatalf("create item: %v", err)
	}

	if _, err := pool.Exec(ctx, `DELETE FROM campaigns WHERE id = $1`, c.ID); err != nil {
		t.Fatalf("delete campaign: %v", err)
	}

	if _, err := repo.GetCampaignItem(ctx, item.ID); !errors.Is(err, campaign.ErrNotFound) {
		t.Errorf("item after campaign delete err = %v, want ErrNotFound (ON DELETE CASCADE)", err)
	}
}
