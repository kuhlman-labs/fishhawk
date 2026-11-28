package campaign_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/approval"
	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/campaign"
	campaigndb "github.com/kuhlman-labs/fishhawk/backend/internal/campaign/db"
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
// created with no autonomy, and one created with an out-of-set value ("bogus"),
// are both NORMALIZED on write to the built-in medium — never "" and never an
// invalid value (ADR-069 decision 4, #5316). The column CHECK itself still
// fails closed: a direct-SQL write of an invalid value is REJECTED by
// campaign_items_autonomy_check, and the legacy empty tier it still admits is readable.
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

	// No autonomy → normalized to the built-in medium on write, never "" or NULL.
	bare, err := repo.CreateCampaignItem(ctx, campaign.CreateCampaignItemParams{
		CampaignID: c.ID,
		IssueRef:   "issue:1533",
	})
	if err != nil {
		t.Fatalf("create bare item: %v", err)
	}
	if bare.Autonomy != "medium" {
		t.Errorf("bare item autonomy = %q, want medium (normalized on write)", bare.Autonomy)
	}
	assertAutonomyColumn(t, pool, bare.ID, "medium")

	// An out-of-set tier is normalized to medium on write — it neither trips the
	// CHECK nor persists as "" or the invalid value.
	bogus, err := repo.CreateCampaignItem(ctx, campaign.CreateCampaignItemParams{
		CampaignID: c.ID,
		IssueRef:   "issue:1534",
		Autonomy:   "bogus",
	})
	if err != nil {
		t.Fatalf("create item with autonomy='bogus' errored (normalize missing?): %v", err)
	}
	if bogus.Autonomy != "medium" {
		t.Errorf("out-of-set item autonomy = %q, want medium (normalized on write)", bogus.Autonomy)
	}
	assertAutonomyColumn(t, pool, bogus.ID, "medium")

	// The column CHECK still fails closed: a direct-SQL write that bypasses the
	// repository's normalization is REJECTED for an invalid value...
	if _, err := pool.Exec(ctx, `UPDATE campaign_items SET autonomy = 'bogus' WHERE id = $1`, bogus.ID); err == nil {
		t.Error("direct-SQL autonomy='bogus' write succeeded, want campaign_items_autonomy_check rejection")
	}
	assertAutonomyColumn(t, pool, bogus.ID, "medium")
	// ...while the legacy '' it still admits stays writable, so a pre-#5316 row
	// remains readable until reconcile-on-read re-resolves it (no migration).
	if _, err := pool.Exec(ctx, `UPDATE campaign_items SET autonomy = '' WHERE id = $1`, bare.ID); err != nil {
		t.Fatalf("direct-SQL legacy autonomy='' write: %v", err)
	}
	legacy, err := repo.GetCampaignItem(ctx, bare.ID)
	if err != nil {
		t.Fatalf("get legacy item: %v", err)
	}
	if legacy.Autonomy != "" {
		t.Errorf("legacy item autonomy = %q, want the stored legacy empty tier", legacy.Autonomy)
	}
}

// assertAutonomyColumn reads the raw campaign_items.autonomy column for id, so
// the assertion covers the stored value rather than only the row mapper.
func assertAutonomyColumn(t *testing.T, pool *pgxpool.Pool, id uuid.UUID, want string) {
	t.Helper()
	var col string
	if err := pool.QueryRow(context.Background(),
		`SELECT autonomy FROM campaign_items WHERE id = $1`, id,
	).Scan(&col); err != nil {
		t.Fatalf("read autonomy column: %v", err)
	}
	if col != want {
		t.Errorf("campaign_items.autonomy column = %q, want %q", col, want)
	}
}

// TestSetCampaignItemAutonomy_RoundTrip drives the new persistence seam (#2355):
// a stored autonomy:low item is overwritten to medium and the RETURNING row plus
// a fresh GetCampaignItem read both carry the new tier.
func TestSetCampaignItemAutonomy_RoundTrip(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := campaign.NewPostgresRepository(pool)
	ctx := context.Background()

	c := makeCampaign(t, repo)
	item, err := repo.CreateCampaignItem(ctx, campaign.CreateCampaignItemParams{
		CampaignID: c.ID,
		IssueRef:   "issue:2355",
		Autonomy:   "low",
	})
	if err != nil {
		t.Fatalf("create item: %v", err)
	}

	updated, err := repo.SetCampaignItemAutonomy(ctx, item.ID, "medium")
	if err != nil {
		t.Fatalf("SetCampaignItemAutonomy: %v", err)
	}
	if updated.Autonomy != "medium" {
		t.Errorf("returned autonomy = %q, want medium", updated.Autonomy)
	}
	got, err := repo.GetCampaignItem(ctx, item.ID)
	if err != nil {
		t.Fatalf("get item: %v", err)
	}
	if got.Autonomy != "medium" {
		t.Errorf("read-back autonomy = %q, want medium", got.Autonomy)
	}
}

// TestSetCampaignItemAutonomy_NormalizesOutOfSetTier is the counterfactual for
// the normalizeAutonomy call in postgresRepo.SetCampaignItemAutonomy: a tier
// outside {low,medium,high} would otherwise trip the migration-0049 column CHECK
// (SQLSTATE 23514). The item is seeded BY CONSTRUCTION with a valid "low" tier;
// the bad tier is passed only to the control under test, so the RED lands on the
// behavioral assertion (the write succeeds and persists the built-in medium,
// #5316) rather than a fixture-setup failure. Delete the normalizeAutonomy call and this test goes
// RED with a CHECK violation.
func TestSetCampaignItemAutonomy_NormalizesOutOfSetTier(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := campaign.NewPostgresRepository(pool)
	ctx := context.Background()

	c := makeCampaign(t, repo)
	item, err := repo.CreateCampaignItem(ctx, campaign.CreateCampaignItemParams{
		CampaignID: c.ID,
		IssueRef:   "issue:2356",
		Autonomy:   "low",
	})
	if err != nil {
		t.Fatalf("create item: %v", err)
	}

	updated, err := repo.SetCampaignItemAutonomy(ctx, item.ID, "critical")
	if err != nil {
		t.Fatalf("SetCampaignItemAutonomy with out-of-set tier errored (normalize missing?): %v", err)
	}
	if updated.Autonomy != "medium" {
		t.Errorf("out-of-set tier persisted as %q, want medium (normalized, never \"\")", updated.Autonomy)
	}
}

// TestSetCampaignItemAutonomy_Idempotent asserts a same-value write is a
// harmless no-op that returns the unchanged tier.
func TestSetCampaignItemAutonomy_Idempotent(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := campaign.NewPostgresRepository(pool)
	ctx := context.Background()

	c := makeCampaign(t, repo)
	item, err := repo.CreateCampaignItem(ctx, campaign.CreateCampaignItemParams{
		CampaignID: c.ID,
		IssueRef:   "issue:2357",
		Autonomy:   "high",
	})
	if err != nil {
		t.Fatalf("create item: %v", err)
	}
	updated, err := repo.SetCampaignItemAutonomy(ctx, item.ID, "high")
	if err != nil {
		t.Fatalf("idempotent SetCampaignItemAutonomy: %v", err)
	}
	if updated.Autonomy != "high" {
		t.Errorf("autonomy = %q, want high", updated.Autonomy)
	}
}

// TestSetCampaignItemAutonomy_NotFound asserts a missing item id maps to
// ErrNotFound (pgx.ErrNoRows translation), mirroring SetCampaignItemRun.
func TestSetCampaignItemAutonomy_NotFound(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := campaign.NewPostgresRepository(pool)
	ctx := context.Background()

	if _, err := repo.SetCampaignItemAutonomy(ctx, uuid.New(), "medium"); !errors.Is(err, campaign.ErrNotFound) {
		t.Fatalf("SetCampaignItemAutonomy(unknown) err = %v, want ErrNotFound", err)
	}
}

// TestPostgres_Assemble_OutOfSetAutonomy_DegradesNotAborts is the routed
// untested-path seam test (#1551 / E32.4 fix-up): it drives the FULL
// Assemble→Persist path from an EpicChild carrying an OUT-OF-SET autonomy tier
// (a typo'd label such as `autonomy:critical`) and asserts persistence
// SUCCEEDS with the campaign_items.autonomy column stored as the built-in
// medium (#5316) — the mislabeled child degrades to the named default, never "",
// instead of aborting the entire
// epic campaign with an SQLSTATE 23514 CHECK violation. This pins the campaign
// package's half of the fix; the parse boundary's normalization of the label
// itself is pinned by TestParseAutonomyLabel in the github provider package.
func TestPostgres_Assemble_OutOfSetAutonomy_DegradesNotAborts(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := campaign.NewPostgresRepository(pool)
	ctx := context.Background()

	res := &workmgmt.EpicChildrenResult{
		Children: []workmgmt.EpicChild{
			{Number: 41, Autonomy: "critical"}, // out-of-set typo → must degrade to medium
			{Number: 42, Autonomy: "low"},      // valid tier passes through
		},
	}
	a, err := campaign.Assemble("issue:40", res)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	// Assemble normalizes the out-of-set tier to medium before it can reach the DB.
	if a.Items[0].Autonomy != "medium" {
		t.Errorf("assembled item[0] autonomy = %q, want medium (normalized)", a.Items[0].Autonomy)
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
	if items[0].IssueRef != "issue:41" || items[0].Autonomy != "medium" {
		t.Errorf("item issue:41 autonomy = %q, want medium", items[0].Autonomy)
	}
	if items[1].IssueRef != "issue:42" || items[1].Autonomy != "low" {
		t.Errorf("item issue:42 autonomy = %q, want low", items[1].Autonomy)
	}

	// Read the raw column too: prove campaign_items.autonomy itself holds medium,
	// satisfying the fail-closed 0049 CHECK rather than the rejected "critical".
	assertAutonomyColumn(t, pool, items[0].ID, "medium")
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

// TestPostgres_TransitionCampaignItem_Replanning is the #5379 round trip
// against the real migration-0120 CHECK: a running item settles to
// replanning and the row persists (the widened campaign_items_state_check
// admits it) with the rejected run still linked; replanning → failed is
// refused by the transition table and commits nothing; replanning →
// cancelled (the operator cancel cascade) is admitted.
func TestPostgres_TransitionCampaignItem_Replanning(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := campaign.NewPostgresRepository(pool)
	runRepo := run.NewPostgresRepository(pool)
	ctx := context.Background()
	c := makeCampaign(t, repo)

	r, err := runRepo.CreateRun(ctx, run.CreateRunParams{
		Repo: "kuhlman-labs/fishhawk", WorkflowID: "feature_change", WorkflowSHA: "deadbeef", TriggerSource: run.TriggerCLI,
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	it, err := repo.CreateCampaignItem(ctx, campaign.CreateCampaignItemParams{CampaignID: c.ID, IssueRef: "issue:5379"})
	if err != nil {
		t.Fatalf("create item: %v", err)
	}
	if _, err := repo.SetCampaignItemRun(ctx, it.ID, &r.ID); err != nil {
		t.Fatalf("link run: %v", err)
	}
	if _, err := repo.TransitionCampaignItem(ctx, it.ID, campaign.ItemStateRunning); err != nil {
		t.Fatalf("→running: %v", err)
	}
	if _, err := repo.TransitionCampaignItem(ctx, it.ID, campaign.ItemStateReplanning); err != nil {
		t.Fatalf("running→replanning: %v", err)
	}
	got, err := repo.GetCampaignItem(ctx, it.ID)
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if got.State != campaign.ItemStateReplanning {
		t.Errorf("persisted state = %q, want replanning", got.State)
	}
	if got.RunID == nil || *got.RunID != r.ID {
		t.Errorf("persisted run_id = %v, want %s (the rejected run stays linked)", got.RunID, r.ID)
	}

	var ite campaign.InvalidTransitionError
	if _, err := repo.TransitionCampaignItem(ctx, it.ID, campaign.ItemStateFailed); !errors.As(err, &ite) {
		t.Errorf("replanning→failed err = %v, want InvalidTransitionError", err)
	}
	if got, _ := repo.GetCampaignItem(ctx, it.ID); got.State != campaign.ItemStateReplanning {
		t.Errorf("state after refused replanning→failed = %q, want replanning (nothing written)", got.State)
	}

	if _, err := repo.TransitionCampaignItem(ctx, it.ID, campaign.ItemStateCancelled); err != nil {
		t.Errorf("replanning→cancelled: %v", err)
	}
	if got, _ := repo.GetCampaignItem(ctx, it.ID); got.State != campaign.ItemStateCancelled {
		t.Errorf("state after replanning→cancelled = %q, want cancelled", got.State)
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

// TestPostgres_CreateCampaign_WorkingDir_RoundTripsEveryReadPath covers the
// campaigns.working_dir column added by 0071 (E48.87 / #2527) against the shared
// pgtest Postgres. The generated sqlc file is HAND-EDITED here (a local `sqlc
// generate` rewrites every package), and the failure mode of a hand edit is a
// column list updated in SOME statements but not all — invisible to inspection.
// So this asserts the binding comes back from EVERY campaign read path, each of
// which has its OWN embedded column list and its OWN row.Scan:
//
//   - CreateCampaign (the INSERT ... RETURNING list)
//   - GetCampaign
//   - GetCampaignByIdempotencyKey
//   - ListCampaigns (the :many scan)
//   - LockCampaignForUpdate — asserted on the generated query's OWN returned
//     row, driven DIRECTLY through campaigndb rather than indirectly through
//     TransitionCampaign (which returns UpdateCampaignState's row, not the
//     locked one, so an assertion there would not exercise this read path at all)
//   - UpdateCampaignState (via TransitionCampaign's returned row)
//
// A statement whose list or Scan call was missed fails here with a scan-arity
// error or an empty value rather than in production. The unbound default is
// pinned too: a campaign created with no binding reads back "" everywhere.
func TestPostgres_CreateCampaign_WorkingDir_RoundTripsEveryReadPath(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := campaign.NewPostgresRepository(pool)
	ctx := context.Background()

	// No binding → "" on create AND read-back (the unchanged-behavior path for
	// every pre-#2527 row).
	def := makeCampaign(t, repo)
	if def.WorkingDir != "" {
		t.Errorf("default WorkingDir = %q, want \"\" (no binding)", def.WorkingDir)
	}
	gotDef, err := repo.GetCampaign(ctx, def.ID)
	if err != nil {
		t.Fatalf("get campaign (no binding): %v", err)
	}
	if gotDef.WorkingDir != "" {
		t.Errorf("read-back WorkingDir = %q, want \"\" (no binding)", gotDef.WorkingDir)
	}

	// An explicit binding survives create + every read path.
	const wd = "/Users/op/checkouts/fishhawk"
	key := "idem-workdir-" + uuid.NewString()
	c, err := repo.CreateCampaign(ctx, campaign.CreateCampaignParams{
		Repo:           "kuhlman-labs/fishhawk",
		EpicRef:        "issue:2527",
		WorkingDir:     wd,
		IdempotencyKey: &key,
	})
	if err != nil {
		t.Fatalf("create campaign with working_dir: %v", err)
	}
	if c.WorkingDir != wd {
		t.Errorf("CreateCampaign returned WorkingDir = %q, want %q", c.WorkingDir, wd)
	}

	got, err := repo.GetCampaign(ctx, c.ID)
	if err != nil {
		t.Fatalf("get campaign: %v", err)
	}
	if got.WorkingDir != wd {
		t.Errorf("GetCampaign WorkingDir = %q, want %q", got.WorkingDir, wd)
	}

	byKey, err := repo.GetCampaignByIdempotencyKey(ctx, "kuhlman-labs/fishhawk", key)
	if err != nil {
		t.Fatalf("get campaign by idempotency key: %v", err)
	}
	if byKey.WorkingDir != wd {
		t.Errorf("GetCampaignByIdempotencyKey WorkingDir = %q, want %q", byKey.WorkingDir, wd)
	}

	listed, err := repo.ListCampaigns(ctx, campaign.ListCampaignsFilter{Repo: "kuhlman-labs/fishhawk", Limit: 100})
	if err != nil {
		t.Fatalf("list campaigns: %v", err)
	}
	var found *campaign.Campaign
	for _, row := range listed {
		if row.ID == c.ID {
			found = row
			break
		}
	}
	if found == nil {
		t.Fatalf("campaign %s absent from ListCampaigns", c.ID)
	}
	if found.WorkingDir != wd {
		t.Errorf("ListCampaigns WorkingDir = %q, want %q", found.WorkingDir, wd)
	}

	// LockCampaignForUpdate, driven DIRECTLY so the assertion is on the row THAT
	// query returned — the load-bearing check for its own column list + Scan.
	locked, err := campaigndb.New(pool).LockCampaignForUpdate(ctx, c.ID)
	if err != nil {
		t.Fatalf("lock campaign for update: %v", err)
	}
	if locked.WorkingDir != wd {
		t.Errorf("LockCampaignForUpdate WorkingDir = %q, want %q", locked.WorkingDir, wd)
	}

	// UpdateCampaignState's RETURNING list, via the transition primitive.
	transitioned, err := repo.TransitionCampaign(ctx, c.ID, campaign.StateRunning)
	if err != nil {
		t.Fatalf("transition campaign: %v", err)
	}
	if transitioned.WorkingDir != wd {
		t.Errorf("TransitionCampaign WorkingDir = %q, want %q", transitioned.WorkingDir, wd)
	}
}

// TestPostgres_CreateCampaign_RunnerKind_RoundTripsEveryReadPath covers the
// campaigns.runner_kind column added by 0113 (#4485) the same way the
// working_dir round-trip above does, and for the same reason: the generated sqlc
// file is HAND-EDITED, and a column list or Scan missed in one statement is
// invisible to inspection. Every query in queries.sql that returns a campaigns
// row is exercised, each with its OWN column list and its OWN row.Scan:
//
//   - CreateCampaign (the unguarded INSERT ... RETURNING list)
//   - CreateCampaignGuardedByGroomingCurrency (the guarded INSERT ... SELECT,
//     whose positional parameters the new column renumbered)
//   - GetCampaign
//   - GetCampaignByIdempotencyKey
//   - ListCampaigns (the :many scan)
//   - LockCampaignForUpdate, driven DIRECTLY through campaigndb
//   - UpdateCampaignState (via TransitionCampaign's returned row)
//
// The not-recorded default is pinned too: a campaign created with no runner
// kind reads back "" everywhere, never NULL.
func TestPostgres_CreateCampaign_RunnerKind_RoundTripsEveryReadPath(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := campaign.NewPostgresRepository(pool)
	ctx := context.Background()
	const repoName = "acme/widgets"

	// Not recorded → "" on create AND read-back (every pre-#4485 row).
	def, err := repo.CreateCampaign(ctx, campaign.CreateCampaignParams{Repo: repoName, EpicRef: "issue:11"})
	if err != nil {
		t.Fatalf("create campaign (no runner kind): %v", err)
	}
	if def.RunnerKind != "" {
		t.Errorf("default RunnerKind = %q, want \"\" (not recorded)", def.RunnerKind)
	}
	gotDef, err := repo.GetCampaign(ctx, def.ID)
	if err != nil {
		t.Fatalf("get campaign (no runner kind): %v", err)
	}
	if gotDef.RunnerKind != "" {
		t.Errorf("read-back RunnerKind = %q, want \"\" (not recorded)", gotDef.RunnerKind)
	}

	// A recorded runner kind survives the unguarded create + every read path.
	const rk = "local"
	key := "idem-runnerkind-" + uuid.NewString()
	c, err := repo.CreateCampaign(ctx, campaign.CreateCampaignParams{
		Repo:           repoName,
		EpicRef:        "issue:12",
		RunnerKind:     rk,
		IdempotencyKey: &key,
	})
	if err != nil {
		t.Fatalf("create campaign with runner_kind: %v", err)
	}
	if c.RunnerKind != rk {
		t.Errorf("CreateCampaign returned RunnerKind = %q, want %q", c.RunnerKind, rk)
	}

	// The guarded INSERT ... SELECT writes and returns it too. No newer
	// grooming candidate is seeded, so the guard admits the row.
	guarded, err := repo.CreateCampaign(ctx, campaign.CreateCampaignParams{
		Repo:       repoName,
		EpicRef:    "",
		RunnerKind: rk,
		WorkingDir: "/srv/checkouts/widgets",
		GroomingGuard: &campaign.GroomingCurrencyGuard{
			SourceRunID:     uuid.New(),
			Repo:            repoName,
			WorkflowID:      "backlog_grooming",
			SourceCreatedAt: time.Now(),
		},
	})
	if err != nil {
		t.Fatalf("create guarded campaign with runner_kind: %v", err)
	}
	if guarded.RunnerKind != rk {
		t.Errorf("guarded CreateCampaign returned RunnerKind = %q, want %q", guarded.RunnerKind, rk)
	}
	if guarded.WorkingDir != "/srv/checkouts/widgets" {
		t.Errorf("guarded CreateCampaign returned WorkingDir = %q, want the bound path (positional parameters shifted?)", guarded.WorkingDir)
	}
	gotGuarded, err := repo.GetCampaign(ctx, guarded.ID)
	if err != nil {
		t.Fatalf("get guarded campaign: %v", err)
	}
	if gotGuarded.RunnerKind != rk {
		t.Errorf("GetCampaign (guarded row) RunnerKind = %q, want %q", gotGuarded.RunnerKind, rk)
	}

	got, err := repo.GetCampaign(ctx, c.ID)
	if err != nil {
		t.Fatalf("get campaign: %v", err)
	}
	if got.RunnerKind != rk {
		t.Errorf("GetCampaign RunnerKind = %q, want %q", got.RunnerKind, rk)
	}

	byKey, err := repo.GetCampaignByIdempotencyKey(ctx, repoName, key)
	if err != nil {
		t.Fatalf("get campaign by idempotency key: %v", err)
	}
	if byKey.RunnerKind != rk {
		t.Errorf("GetCampaignByIdempotencyKey RunnerKind = %q, want %q", byKey.RunnerKind, rk)
	}

	listed, err := repo.ListCampaigns(ctx, campaign.ListCampaignsFilter{Repo: repoName, Limit: 100})
	if err != nil {
		t.Fatalf("list campaigns: %v", err)
	}
	var found *campaign.Campaign
	for _, row := range listed {
		if row.ID == c.ID {
			found = row
			break
		}
	}
	if found == nil {
		t.Fatalf("campaign %s absent from ListCampaigns", c.ID)
	}
	if found.RunnerKind != rk {
		t.Errorf("ListCampaigns RunnerKind = %q, want %q", found.RunnerKind, rk)
	}

	locked, err := campaigndb.New(pool).LockCampaignForUpdate(ctx, c.ID)
	if err != nil {
		t.Fatalf("lock campaign for update: %v", err)
	}
	if locked.RunnerKind != rk {
		t.Errorf("LockCampaignForUpdate RunnerKind = %q, want %q", locked.RunnerKind, rk)
	}

	transitioned, err := repo.TransitionCampaign(ctx, c.ID, campaign.StateRunning)
	if err != nil {
		t.Fatalf("transition campaign: %v", err)
	}
	if transitioned.RunnerKind != rk {
		t.Errorf("TransitionCampaign RunnerKind = %q, want %q", transitioned.RunnerKind, rk)
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

// TestPostgres_TransitionCampaign_AwaitingHuman is the ONLY test that proves
// migration 0087's widened campaigns_state_check actually shipped (E72.33 /
// #3660). A Go-only transition test passes with the migration MISSING and the
// write then fails SQLSTATE 23514 at runtime, so this drives the real column:
// pending -> awaiting_human must SUCCEED and read back `awaiting_human` on an
// independent GetCampaign, and the state must be reversible in BOTH directions
// the transition table admits (-> pending on a relabel-without-start, -> running
// on a start).
func TestPostgres_TransitionCampaign_AwaitingHuman(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := campaign.NewPostgresRepository(pool)
	ctx := context.Background()

	c := makeCampaign(t, repo)
	awaiting, err := repo.TransitionCampaign(ctx, c.ID, campaign.StateAwaitingHuman)
	if err != nil {
		t.Fatalf("pending→awaiting_human (widened campaigns_state_check missing?): %v", err)
	}
	if awaiting.State != campaign.StateAwaitingHuman {
		t.Errorf("returned state = %q, want awaiting_human", awaiting.State)
	}
	// COMMITTED STATE: an independent read, not only the returned row.
	got, err := repo.GetCampaign(ctx, c.ID)
	if err != nil {
		t.Fatalf("re-read campaign: %v", err)
	}
	if got.State != campaign.StateAwaitingHuman {
		t.Errorf("persisted state = %q, want awaiting_human", got.State)
	}
	// REVISION FIX 1's persistence half: the row can follow the re-derivation back
	// to pending (the relabel-without-start corner).
	back, err := repo.TransitionCampaign(ctx, c.ID, campaign.StatePending)
	if err != nil {
		t.Fatalf("awaiting_human→pending: %v", err)
	}
	if back.State != campaign.StatePending {
		t.Errorf("state after relabel re-derive = %q, want pending", back.State)
	}
	// And forward again to running (the relabel-THEN-start corner).
	if _, err := repo.TransitionCampaign(ctx, c.ID, campaign.StateAwaitingHuman); err != nil {
		t.Fatalf("pending→awaiting_human (second time): %v", err)
	}
	running, err := repo.TransitionCampaign(ctx, c.ID, campaign.StateRunning)
	if err != nil {
		t.Fatalf("awaiting_human→running: %v", err)
	}
	if running.State != campaign.StateRunning {
		t.Errorf("state after start = %q, want running", running.State)
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

	// replanning → pending, the rejected run's link cleared (#5379). The
	// replanning row itself proves the widened 0120 CHECK. The COMMITTED row is
	// re-read: the from-guard's effect is the reset, not only a nil error.
	replanning := mkLinkedItem(t, "issue:replanning", campaign.ItemStateReplanning)
	if got, _ := repo.GetCampaignItem(ctx, replanning.ID); got.State != campaign.ItemStateReplanning || got.RunID == nil {
		t.Fatalf("seeded replanning row = state %q run_id %v, want replanning with the rejected run linked", got.State, got.RunID)
	}
	if _, err := repo.RestartCampaignItem(ctx, replanning.ID); err != nil {
		t.Errorf("restart(replanning): %v", err)
	}
	if got, _ := repo.GetCampaignItem(ctx, replanning.ID); got.State != campaign.ItemStatePending || got.RunID != nil {
		t.Errorf("persisted replanning restart = state %q run_id %v, want pending/nil", got.State, got.RunID)
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

// TestPostgres_SettleCampaignItemOutOfBand_StampsResolvedBy pins the #3563
// addition to the #2029 out-of-band settle: it now stamps
// resolved_by=issue_closed WHILE retaining the run link. Both properties are
// asserted on the PERSISTED row, not just the returned one, because the marker
// and the link are what the engine and the operator surface read back.
func TestPostgres_SettleCampaignItemOutOfBand_StampsResolvedBy(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := campaign.NewPostgresRepository(pool)
	runRepo := run.NewPostgresRepository(pool)
	ctx := context.Background()
	c := makeCampaign(t, repo)

	r, err := runRepo.CreateRun(ctx, run.CreateRunParams{
		Repo:          "kuhlman-labs/fishhawk",
		WorkflowID:    "feature_change",
		WorkflowSHA:   "deadbeef",
		TriggerSource: run.TriggerCLI,
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	it, err := repo.CreateCampaignItem(ctx, campaign.CreateCampaignItemParams{CampaignID: c.ID, IssueRef: "issue:oob"})
	if err != nil {
		t.Fatalf("create item: %v", err)
	}
	// A fresh item carries the '' default — the pre-settle baseline.
	if it.ResolvedBy != campaign.ResolvedByUnset {
		t.Fatalf("fresh item resolved_by = %q, want %q", it.ResolvedBy, campaign.ResolvedByUnset)
	}
	if _, err := repo.SetCampaignItemRun(ctx, it.ID, &r.ID); err != nil {
		t.Fatalf("link run: %v", err)
	}
	for _, to := range []campaign.ItemState{campaign.ItemStateRunning, campaign.ItemStateCancelled} {
		if _, err := repo.TransitionCampaignItem(ctx, it.ID, to); err != nil {
			t.Fatalf("→%s: %v", to, err)
		}
	}

	settled, err := repo.SettleCampaignItemOutOfBand(ctx, it.ID)
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	if settled.ResolvedBy != campaign.ResolvedByIssueClosed {
		t.Errorf("settle resolved_by = %q, want %q", settled.ResolvedBy, campaign.ResolvedByIssueClosed)
	}
	got, err := repo.GetCampaignItem(ctx, it.ID)
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if got.State != campaign.ItemStateSucceeded {
		t.Errorf("persisted state = %q, want succeeded", got.State)
	}
	if got.ResolvedBy != campaign.ResolvedByIssueClosed {
		t.Errorf("persisted resolved_by = %q, want %q", got.ResolvedBy, campaign.ResolvedByIssueClosed)
	}
	if got.RunID == nil || *got.RunID != r.ID {
		t.Errorf("persisted run_id = %v, want %s RETAINED", got.RunID, r.ID)
	}
}

// TestPostgres_SettleCampaignItemForClosedIssue pins the #3563 not-yet-run
// issue-closed settle: every admitted (from, to) pair round-trips with the
// resolved_by marker written in the SAME statement as the state, and every
// refusal leaves the row byte-unchanged.
//
// ATOMICITY is asserted on the RETURNED row (one statement, so the returned row
// carries BOTH the new state and the marker) and re-asserted on the persisted
// row: a two-statement settle would be observable as a row whose state moved
// while resolved_by stayed ” — the window in which NextEligible would offer an
// abandoned issue as Restartable.
func TestPostgres_SettleCampaignItemForClosedIssue(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := campaign.NewPostgresRepository(pool)
	runRepo := run.NewPostgresRepository(pool)
	ctx := context.Background()
	c := makeCampaign(t, repo)

	// mkItem creates a run-less item and drives it to `from` (pending is the
	// create default; blocked is one valid transition away).
	mkItem := func(t *testing.T, ref string, from campaign.ItemState) *campaign.Item {
		t.Helper()
		it, err := repo.CreateCampaignItem(ctx, campaign.CreateCampaignItemParams{CampaignID: c.ID, IssueRef: ref})
		if err != nil {
			t.Fatalf("create item %s: %v", ref, err)
		}
		if from != campaign.ItemStatePending {
			if _, err := repo.TransitionCampaignItem(ctx, it.ID, from); err != nil {
				t.Fatalf("%s →%s: %v", ref, from, err)
			}
		}
		return it
	}

	// --- The four admitted (from, to) pairs.
	for _, tc := range []struct {
		name string
		from campaign.ItemState
		to   campaign.ItemState
	}{
		{"pending_to_succeeded", campaign.ItemStatePending, campaign.ItemStateSucceeded},
		{"pending_to_cancelled", campaign.ItemStatePending, campaign.ItemStateCancelled},
		{"blocked_to_succeeded", campaign.ItemStateBlocked, campaign.ItemStateSucceeded},
		{"blocked_to_cancelled", campaign.ItemStateBlocked, campaign.ItemStateCancelled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			it := mkItem(t, "issue:"+tc.name, tc.from)
			settled, err := repo.SettleCampaignItemForClosedIssue(ctx, it.ID, tc.to)
			if err != nil {
				t.Fatalf("settle: %v", err)
			}
			// Atomicity: ONE statement, so the returned row carries both.
			if settled.State != tc.to {
				t.Errorf("returned state = %q, want %q", settled.State, tc.to)
			}
			if settled.ResolvedBy != campaign.ResolvedByIssueClosed {
				t.Errorf("returned resolved_by = %q, want %q", settled.ResolvedBy, campaign.ResolvedByIssueClosed)
			}
			got, err := repo.GetCampaignItem(ctx, it.ID)
			if err != nil {
				t.Fatalf("re-read: %v", err)
			}
			if got.State != tc.to || got.ResolvedBy != campaign.ResolvedByIssueClosed {
				t.Errorf("persisted = state %q resolved_by %q, want %q / %q", got.State, got.ResolvedBy, tc.to, campaign.ResolvedByIssueClosed)
			}
			// The run link is untouched by this arm (a class-A item has none).
			if got.RunID != nil {
				t.Errorf("persisted run_id = %v, want nil (class A is run-less)", got.RunID)
			}
		})
	}

	// --- A replanning item (#5379): its plan was rejected, so it holds a link
	// to the rejected run. Both closure outcomes settle it, stamp the marker,
	// and RETAIN the run link as provenance. The committed row is re-read.
	for _, tc := range []struct {
		name string
		to   campaign.ItemState
	}{
		{"replanning_to_succeeded", campaign.ItemStateSucceeded},
		{"replanning_to_cancelled", campaign.ItemStateCancelled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, err := runRepo.CreateRun(ctx, run.CreateRunParams{
				Repo: "kuhlman-labs/fishhawk", WorkflowID: "feature_change", WorkflowSHA: "deadbeef", TriggerSource: run.TriggerCLI,
			})
			if err != nil {
				t.Fatalf("create run: %v", err)
			}
			it := mkItem(t, "issue:"+tc.name, campaign.ItemStatePending)
			if _, err := repo.SetCampaignItemRun(ctx, it.ID, &r.ID); err != nil {
				t.Fatalf("link run: %v", err)
			}
			if _, err := repo.TransitionCampaignItem(ctx, it.ID, campaign.ItemStateRunning); err != nil {
				t.Fatalf("→running: %v", err)
			}
			if _, err := repo.TransitionCampaignItem(ctx, it.ID, campaign.ItemStateReplanning); err != nil {
				t.Fatalf("→replanning: %v", err)
			}
			if _, err := repo.SettleCampaignItemForClosedIssue(ctx, it.ID, tc.to); err != nil {
				t.Errorf("settle: %v", err)
			}
			got, err := repo.GetCampaignItem(ctx, it.ID)
			if err != nil {
				t.Fatalf("re-read: %v", err)
			}
			if got.State != tc.to || got.ResolvedBy != campaign.ResolvedByIssueClosed {
				t.Errorf("persisted = state %q resolved_by %q, want %q / %q", got.State, got.ResolvedBy, tc.to, campaign.ResolvedByIssueClosed)
			}
			if got.RunID == nil || *got.RunID != r.ID {
				t.Errorf("persisted run_id = %v, want %s (the rejected run, retained as provenance)", got.RunID, r.ID)
			}
		})
	}

	// --- Refused FROM-states. Each is asserted on COMMITTED STATE as well as on
	// error identity: a guard that fired and then rolled back returns the same
	// error as one that never existed, so the row is re-read.
	var ite campaign.InvalidTransitionError
	t.Run("refuses_running_from", func(t *testing.T) {
		r, err := runRepo.CreateRun(ctx, run.CreateRunParams{
			Repo: "kuhlman-labs/fishhawk", WorkflowID: "feature_change", WorkflowSHA: "deadbeef", TriggerSource: run.TriggerCLI,
		})
		if err != nil {
			t.Fatalf("create run: %v", err)
		}
		it := mkItem(t, "issue:refuse-running", campaign.ItemStatePending)
		if _, err := repo.SetCampaignItemRun(ctx, it.ID, &r.ID); err != nil {
			t.Fatalf("link run: %v", err)
		}
		if _, err := repo.TransitionCampaignItem(ctx, it.ID, campaign.ItemStateRunning); err != nil {
			t.Fatalf("→running: %v", err)
		}
		if _, err := repo.SettleCampaignItemForClosedIssue(ctx, it.ID, campaign.ItemStateSucceeded); !errors.As(err, &ite) {
			t.Fatalf("err = %v, want InvalidTransitionError", err)
		} else if ite.Kind != "campaign_item" || ite.From != "running" || ite.To != "succeeded" {
			t.Errorf("err = %+v, want campaign_item running→succeeded", ite)
		}
		if got, _ := repo.GetCampaignItem(ctx, it.ID); got.State != campaign.ItemStateRunning || got.ResolvedBy != campaign.ResolvedByUnset {
			t.Errorf("row after refusal = state %q resolved_by %q, want running / %q (nothing written)", got.State, got.ResolvedBy, campaign.ResolvedByUnset)
		}
	})
	t.Run("refuses_succeeded_from", func(t *testing.T) {
		it := mkItem(t, "issue:refuse-succeeded", campaign.ItemStatePending)
		if _, err := repo.TransitionCampaignItem(ctx, it.ID, campaign.ItemStateSucceeded); err != nil {
			t.Fatalf("→succeeded: %v", err)
		}
		if _, err := repo.SettleCampaignItemForClosedIssue(ctx, it.ID, campaign.ItemStateSucceeded); !errors.As(err, &ite) {
			t.Fatalf("err = %v, want InvalidTransitionError", err)
		}
		if got, _ := repo.GetCampaignItem(ctx, it.ID); got.ResolvedBy != campaign.ResolvedByUnset {
			t.Errorf("resolved_by after refusal = %q, want %q (nothing written)", got.ResolvedBy, campaign.ResolvedByUnset)
		}
	})
	t.Run("refuses_paused_from", func(t *testing.T) {
		r, err := runRepo.CreateRun(ctx, run.CreateRunParams{
			Repo: "kuhlman-labs/fishhawk", WorkflowID: "feature_change", WorkflowSHA: "deadbeef", TriggerSource: run.TriggerCLI,
		})
		if err != nil {
			t.Fatalf("create run: %v", err)
		}
		it := mkItem(t, "issue:refuse-paused", campaign.ItemStatePending)
		if _, err := repo.SetCampaignItemRun(ctx, it.ID, &r.ID); err != nil {
			t.Fatalf("link run: %v", err)
		}
		if _, err := repo.TransitionCampaignItem(ctx, it.ID, campaign.ItemStateRunning); err != nil {
			t.Fatalf("→running: %v", err)
		}
		if _, err := repo.PauseCampaignItem(ctx, it.ID, campaign.PauseReason{PageEvent: "campaign_gate_paged"}); err != nil {
			t.Fatalf("→paused: %v", err)
		}
		if _, err := repo.SettleCampaignItemForClosedIssue(ctx, it.ID, campaign.ItemStateSucceeded); !errors.As(err, &ite) {
			t.Fatalf("err = %v, want InvalidTransitionError", err)
		}
		if got, _ := repo.GetCampaignItem(ctx, it.ID); got.State != campaign.ItemStatePaused || got.ResolvedBy != campaign.ResolvedByUnset {
			t.Errorf("row after refusal = state %q resolved_by %q, want paused / %q", got.State, got.ResolvedBy, campaign.ResolvedByUnset)
		}
	})

	// --- Refused `to`: an out-of-set target. pending→running is a VALID
	// lifecycle edge, so this refusal is the `to` guard's alone and not the
	// transition table's — which is why the row is re-read: an accepted `to`
	// would have moved the item to running AND stamped a marker.
	t.Run("refuses_out_of_set_to", func(t *testing.T) {
		it := mkItem(t, "issue:refuse-to", campaign.ItemStatePending)
		if _, err := repo.SettleCampaignItemForClosedIssue(ctx, it.ID, campaign.ItemStateRunning); !errors.As(err, &ite) {
			t.Fatalf("err = %v, want InvalidTransitionError", err)
		} else if ite.From != "pending" || ite.To != "running" {
			t.Errorf("err = %+v, want campaign_item pending→running", ite)
		}
		if got, _ := repo.GetCampaignItem(ctx, it.ID); got.State != campaign.ItemStatePending || got.ResolvedBy != campaign.ResolvedByUnset {
			t.Errorf("row after refusal = state %q resolved_by %q, want pending / %q (nothing written)", got.State, got.ResolvedBy, campaign.ResolvedByUnset)
		}
	})

	// --- A missing item.
	t.Run("missing_item", func(t *testing.T) {
		if _, err := repo.SettleCampaignItemForClosedIssue(ctx, uuid.New(), campaign.ItemStateSucceeded); !errors.Is(err, campaign.ErrNotFound) {
			t.Errorf("err = %v, want ErrNotFound", err)
		}
	})
}

// TestPostgres_SettleCampaignItemForClosedIssue_NormalizesMarker exercises the
// campaign.normalizeResolvedBy guard against the REAL migration-0085 CHECK.
//
// The normalizer and the CHECK are two layers of the same fail-closed posture:
// the CHECK rejects an out-of-set marker at write time (pinned by
// backend/internal/postgres/postgres_test.go's reversal test, which inserts
// 'bogus' directly), and the normalizer keeps an out-of-set value from ever
// REACHING it and turning a settle into a failed write. Here the settle path is
// driven end to end and the persisted marker asserted to be exactly the
// in-set value — i.e. the write was ACCEPTED by the CHECK, which is only true
// because the value handed to the column is normalized.
func TestPostgres_SettleCampaignItemForClosedIssue_NormalizesMarker(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := campaign.NewPostgresRepository(pool)
	ctx := context.Background()
	c := makeCampaign(t, repo)

	it, err := repo.CreateCampaignItem(ctx, campaign.CreateCampaignItemParams{CampaignID: c.ID, IssueRef: "issue:normalize"})
	if err != nil {
		t.Fatalf("create item: %v", err)
	}
	settled, err := repo.SettleCampaignItemForClosedIssue(ctx, it.ID, campaign.ItemStateCancelled)
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	// The persisted marker is exactly one of the CHECK-permitted values — never
	// a value the column would have rejected, and never silently empty.
	switch settled.ResolvedBy {
	case campaign.ResolvedByIssueClosed:
		// expected
	case campaign.ResolvedByUnset:
		t.Errorf("resolved_by = %q, want %q (an issue-closed settle must be marked)", settled.ResolvedBy, campaign.ResolvedByIssueClosed)
	default:
		t.Errorf("resolved_by = %q, outside the migration-0085 CHECK set", settled.ResolvedBy)
	}
	// Raw read-back through the pool: the column really holds the in-set value,
	// independent of the domain mapping.
	var raw string
	if err := pool.QueryRow(ctx, `SELECT resolved_by FROM campaign_items WHERE id = $1`, it.ID).Scan(&raw); err != nil {
		t.Fatalf("raw select: %v", err)
	}
	if raw != string(campaign.ResolvedByIssueClosed) {
		t.Errorf("column resolved_by = %q, want %q", raw, campaign.ResolvedByIssueClosed)
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

// --- ReopenCampaignForItemRestart (#2681) ---

// reopenFixture builds a campaign driven to the requested state plus a set of
// items, each driven to its requested state through the valid transition path,
// returned by issue ref. It is the shared seed for the
// ReopenCampaignForItemRestart tests: every guard those tests exercise must be
// reached with the rows already IN the target state, seeded by construction,
// so a RED lands on the behavioral assertion rather than on fixture setup.
func reopenFixture(t *testing.T, repo campaign.Repository, cState campaign.State, items map[string]campaign.ItemState) (*campaign.Campaign, map[string]*campaign.Item) {
	t.Helper()
	ctx := context.Background()
	c := makeCampaign(t, repo)

	out := make(map[string]*campaign.Item, len(items))
	for ref, st := range items {
		it, err := repo.CreateCampaignItem(ctx, campaign.CreateCampaignItemParams{CampaignID: c.ID, IssueRef: ref})
		if err != nil {
			t.Fatalf("create item %s: %v", ref, err)
		}
		// running/paused/succeeded-from-running are reached through running; the
		// rest transition straight out of pending.
		switch st {
		case campaign.ItemStatePending:
			// initial state
		case campaign.ItemStatePaused:
			if _, err := repo.TransitionCampaignItem(ctx, it.ID, campaign.ItemStateRunning); err != nil {
				t.Fatalf("item %s →running: %v", ref, err)
			}
			if _, err := repo.TransitionCampaignItem(ctx, it.ID, campaign.ItemStatePaused); err != nil {
				t.Fatalf("item %s →paused: %v", ref, err)
			}
		default:
			if _, err := repo.TransitionCampaignItem(ctx, it.ID, st); err != nil {
				t.Fatalf("item %s →%s: %v", ref, st, err)
			}
		}
		fresh, err := repo.GetCampaignItem(ctx, it.ID)
		if err != nil {
			t.Fatalf("re-read item %s: %v", ref, err)
		}
		out[ref] = fresh
	}

	switch cState {
	case campaign.StatePending:
		// initial state
	case campaign.StatePaused:
		if _, err := repo.TransitionCampaign(ctx, c.ID, campaign.StateRunning); err != nil {
			t.Fatalf("campaign →running: %v", err)
		}
		if _, err := repo.TransitionCampaign(ctx, c.ID, campaign.StatePaused); err != nil {
			t.Fatalf("campaign →paused: %v", err)
		}
	default:
		if _, err := repo.TransitionCampaign(ctx, c.ID, cState); err != nil {
			t.Fatalf("campaign →%s: %v", cState, err)
		}
	}
	fresh, err := repo.GetCampaign(ctx, c.ID)
	if err != nil {
		t.Fatalf("re-read campaign: %v", err)
	}
	return fresh, out
}

// TestReopenCampaignForItemRestart_ReopensAndResetsAtomically is the happy-path
// round trip: a terminal-FAILED campaign whose items are {succeeded, failed
// (run-linked)} is reopened and its failed item reset in one call. It asserts
// BOTH the returned values AND a fresh read of each row, so a method that
// returned the right structs without committing them fails.
func TestReopenCampaignForItemRestart_ReopensAndResetsAtomically(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := campaign.NewPostgresRepository(pool)
	runRepo := run.NewPostgresRepository(pool)
	ctx := context.Background()

	c, items := reopenFixture(t, repo, campaign.StateFailed, map[string]campaign.ItemState{
		"issue:100": campaign.ItemStateSucceeded,
		"issue:101": campaign.ItemStateFailed,
	})
	// Link a REAL run to the failed item so the link-clearing is observable.
	r, err := runRepo.CreateRun(ctx, run.CreateRunParams{
		Repo: "kuhlman-labs/fishhawk", WorkflowID: "feature_change", WorkflowSHA: "deadbeef", TriggerSource: run.TriggerCLI,
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	if _, err := repo.SetCampaignItemRun(ctx, items["issue:101"].ID, &r.ID); err != nil {
		t.Fatalf("link run: %v", err)
	}

	gotC, gotItem, err := repo.ReopenCampaignForItemRestart(ctx, c.ID, items["issue:101"].ID)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if gotC.State != campaign.StateRunning {
		t.Errorf("returned campaign state = %q, want running", gotC.State)
	}
	if gotItem.State != campaign.ItemStatePending {
		t.Errorf("returned item state = %q, want pending", gotItem.State)
	}
	if gotItem.RunID != nil {
		t.Errorf("returned item run_id = %v, want nil (link cleared)", gotItem.RunID)
	}

	// Persisted, not just returned.
	freshC, err := repo.GetCampaign(ctx, c.ID)
	if err != nil {
		t.Fatalf("re-read campaign: %v", err)
	}
	if freshC.State != campaign.StateRunning {
		t.Errorf("persisted campaign state = %q, want running", freshC.State)
	}
	freshItem, err := repo.GetCampaignItem(ctx, items["issue:101"].ID)
	if err != nil {
		t.Fatalf("re-read item: %v", err)
	}
	if freshItem.State != campaign.ItemStatePending || freshItem.RunID != nil {
		t.Errorf("persisted item = state %q run_id %v, want pending/nil", freshItem.State, freshItem.RunID)
	}
	// The untouched sibling is unchanged.
	if sib, _ := repo.GetCampaignItem(ctx, items["issue:100"].ID); sib.State != campaign.ItemStateSucceeded {
		t.Errorf("sibling state = %q, want succeeded (untouched)", sib.State)
	}
}

// TestReopenCampaignForItemRestart_RejectsNonFailedCampaign is the
// campaign-state guard's counterfactual vehicle. Every non-failed campaign state
// is refused InvalidTransitionError{Kind:"campaign"} — and, because a control
// whose real effect is COMMITTED STATE cannot be pinned by error identity alone,
// it re-reads BOTH rows and asserts the item is STILL terminal and the campaign
// state unchanged.
func TestReopenCampaignForItemRestart_RejectsNonFailedCampaign(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := campaign.NewPostgresRepository(pool)
	ctx := context.Background()

	for _, cState := range []campaign.State{
		campaign.StatePending,
		campaign.StateRunning,
		campaign.StatePaused,
		campaign.StateCancelled,
		campaign.StateSucceeded,
	} {
		t.Run(string(cState), func(t *testing.T) {
			c, items := reopenFixture(t, repo, cState, map[string]campaign.ItemState{
				"issue:200": campaign.ItemStateFailed,
			})
			item := items["issue:200"]

			gotC, gotItem, err := repo.ReopenCampaignForItemRestart(ctx, c.ID, item.ID)
			var ite campaign.InvalidTransitionError
			if !errors.As(err, &ite) {
				t.Fatalf("err = %v, want InvalidTransitionError", err)
			}
			if ite.Kind != "campaign" || ite.From != string(cState) {
				t.Errorf("err = %+v, want kind campaign from %s", ite, cState)
			}
			if gotC != nil || gotItem != nil {
				t.Errorf("returned (%v, %v), want (nil, nil) on refusal", gotC, gotItem)
			}
			// COMMITTED STATE: nothing was written.
			freshC, err := repo.GetCampaign(ctx, c.ID)
			if err != nil {
				t.Fatalf("re-read campaign: %v", err)
			}
			if freshC.State != cState {
				t.Errorf("campaign state = %q, want %q (unchanged)", freshC.State, cState)
			}
			freshItem, err := repo.GetCampaignItem(ctx, item.ID)
			if err != nil {
				t.Fatalf("re-read item: %v", err)
			}
			if freshItem.State != campaign.ItemStateFailed {
				t.Errorf("item state = %q, want failed (unchanged)", freshItem.State)
			}
		})
	}
}

// TestReopenCampaignForItemRestart_RejectsNonRestartableItem is BOTH the
// item-state guard's counterfactual vehicle AND the ATOMICITY proof. The
// campaign guard has ALREADY PASSED when the item guard rejects, so a
// non-transactional implementation (two sequential writes) would have committed
// the campaign flip to running before the item guard ran. The assertion that
// carries the atomicity claim is therefore "the campaign is STILL failed" after
// the call — not the error identity, which is byte-identical either way.
func TestReopenCampaignForItemRestart_RejectsNonRestartableItem(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := campaign.NewPostgresRepository(pool)
	ctx := context.Background()

	for _, itemState := range []campaign.ItemState{
		campaign.ItemStateSucceeded,
		campaign.ItemStateRunning,
		campaign.ItemStatePending,
		campaign.ItemStateBlocked,
		campaign.ItemStatePaused,
	} {
		t.Run(string(itemState), func(t *testing.T) {
			c, items := reopenFixture(t, repo, campaign.StateFailed, map[string]campaign.ItemState{
				"issue:300": itemState,
			})
			item := items["issue:300"]

			gotC, gotItem, err := repo.ReopenCampaignForItemRestart(ctx, c.ID, item.ID)
			var ite campaign.InvalidTransitionError
			if !errors.As(err, &ite) {
				t.Fatalf("err = %v, want InvalidTransitionError", err)
			}
			if ite.Kind != "campaign_item" || ite.From != string(itemState) {
				t.Errorf("err = %+v, want kind campaign_item from %s", ite, itemState)
			}
			if gotC != nil || gotItem != nil {
				t.Errorf("returned (%v, %v), want (nil, nil) on refusal", gotC, gotItem)
			}
			// THE ATOMICITY ASSERTION: the campaign guard passed, so a
			// two-writes-no-transaction implementation would already have flipped
			// the campaign to running. One transaction means the rollback erased it.
			freshC, err := repo.GetCampaign(ctx, c.ID)
			if err != nil {
				t.Fatalf("re-read campaign: %v", err)
			}
			if freshC.State != campaign.StateFailed {
				t.Errorf("campaign state = %q, want failed — the item guard rejected, so the campaign flip must have rolled back (atomicity)", freshC.State)
			}
			freshItem, err := repo.GetCampaignItem(ctx, item.ID)
			if err != nil {
				t.Fatalf("re-read item: %v", err)
			}
			if freshItem.State != itemState {
				t.Errorf("item state = %q, want %q (unchanged)", freshItem.State, itemState)
			}
		})
	}
}

// TestReopenCampaignForItemRestart_RejectsItemFromAnotherCampaign pins the
// ownership guard: a real, genuinely-restartable item of campaign B passed with
// campaign A's id is refused ErrCampaignItemNotFound — and NOTHING in either
// campaign is written. Both campaigns are FAILED and both items restartable, so
// the refusal can only come from the ownership check (pairing the foreign item
// with a fixture that would otherwise succeed is what makes the guard's deletion
// observable).
func TestReopenCampaignForItemRestart_RejectsItemFromAnotherCampaign(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := campaign.NewPostgresRepository(pool)
	ctx := context.Background()

	a, aItems := reopenFixture(t, repo, campaign.StateFailed, map[string]campaign.ItemState{
		"issue:400": campaign.ItemStateFailed,
	})
	b, bItems := reopenFixture(t, repo, campaign.StateFailed, map[string]campaign.ItemState{
		"issue:401": campaign.ItemStateFailed,
	})

	gotC, gotItem, err := repo.ReopenCampaignForItemRestart(ctx, a.ID, bItems["issue:401"].ID)
	if !errors.Is(err, campaign.ErrCampaignItemNotFound) {
		t.Fatalf("err = %v, want ErrCampaignItemNotFound", err)
	}
	if !errors.Is(err, campaign.ErrNotFound) {
		t.Errorf("err = %v, want it to also satisfy errors.Is(err, ErrNotFound) (the umbrella sentinel)", err)
	}
	if gotC != nil || gotItem != nil {
		t.Errorf("returned (%v, %v), want (nil, nil) on refusal", gotC, gotItem)
	}
	// Neither campaign nor B's item moved.
	if fresh, _ := repo.GetCampaign(ctx, a.ID); fresh.State != campaign.StateFailed {
		t.Errorf("campaign A state = %q, want failed (unchanged)", fresh.State)
	}
	if fresh, _ := repo.GetCampaign(ctx, b.ID); fresh.State != campaign.StateFailed {
		t.Errorf("campaign B state = %q, want failed (unchanged)", fresh.State)
	}
	if fresh, _ := repo.GetCampaignItem(ctx, bItems["issue:401"].ID); fresh.State != campaign.ItemStateFailed {
		t.Errorf("campaign B item state = %q, want failed (unchanged)", fresh.State)
	}
	if fresh, _ := repo.GetCampaignItem(ctx, aItems["issue:400"].ID); fresh.State != campaign.ItemStateFailed {
		t.Errorf("campaign A item state = %q, want failed (unchanged)", fresh.State)
	}
}

// TestReopenCampaignForItemRestart_NotFound pins the two missing-row guards and
// the sentinel DISTINCTION between them: a missing campaign is
// ErrCampaignNotFound, a missing item under a real failed campaign is
// ErrCampaignItemNotFound, and both satisfy errors.Is(err, ErrNotFound) so no
// existing umbrella caller changes behavior.
func TestReopenCampaignForItemRestart_NotFound(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := campaign.NewPostgresRepository(pool)
	ctx := context.Background()

	c, items := reopenFixture(t, repo, campaign.StateFailed, map[string]campaign.ItemState{
		"issue:500": campaign.ItemStateFailed,
	})

	// Unknown campaign id -> the CAMPAIGN sentinel (never the item-shaped one).
	_, _, err := repo.ReopenCampaignForItemRestart(ctx, uuid.New(), items["issue:500"].ID)
	if !errors.Is(err, campaign.ErrCampaignNotFound) {
		t.Errorf("missing-campaign err = %v, want ErrCampaignNotFound", err)
	}
	if errors.Is(err, campaign.ErrCampaignItemNotFound) {
		t.Errorf("missing-campaign err = %v, must NOT be reported as an item miss", err)
	}
	if !errors.Is(err, campaign.ErrNotFound) {
		t.Errorf("missing-campaign err = %v, want it to satisfy the ErrNotFound umbrella", err)
	}

	// Unknown item id under a real failed campaign -> the ITEM sentinel.
	_, _, err = repo.ReopenCampaignForItemRestart(ctx, c.ID, uuid.New())
	if !errors.Is(err, campaign.ErrCampaignItemNotFound) {
		t.Errorf("missing-item err = %v, want ErrCampaignItemNotFound", err)
	}
	if errors.Is(err, campaign.ErrCampaignNotFound) {
		t.Errorf("missing-item err = %v, must NOT be reported as a campaign miss", err)
	}
	if !errors.Is(err, campaign.ErrNotFound) {
		t.Errorf("missing-item err = %v, want it to satisfy the ErrNotFound umbrella", err)
	}

	// Nothing was written on either miss.
	if fresh, _ := repo.GetCampaign(ctx, c.ID); fresh.State != campaign.StateFailed {
		t.Errorf("campaign state = %q, want failed (unchanged)", fresh.State)
	}
	if fresh, _ := repo.GetCampaignItem(ctx, items["issue:500"].ID); fresh.State != campaign.ItemStateFailed {
		t.Errorf("item state = %q, want failed (unchanged)", fresh.State)
	}
}

// TestPostgres_Persist_QueueOrderSurvivesRealPostgres is THE assertion behind
// binding condition K1, and it must run against REAL Postgres — a fake whose
// map or slice iteration happened to preserve insertion order would pass here
// while production scrambled the queue. It asserts COMMITTED state (a read-back
// through the repository), because a scrambled order still returns a successful
// create.
//
// WHAT THE ORDERING COLUMN ACTUALLY FIXES, stated from what the tree does
// rather than from assumption. campaign.Persist is NOT transactional (one
// CreateCampaign, then a loop of CreateCampaignItem, each its own implicit
// transaction), so the pre-0074 (created_at, id) listing did NOT collapse to
// one transaction timestamp — it depended on each statement's now() being
// strictly increasing. That is a timing accident, not a guarantee: two inserts
// landing in the same clock tick share a created_at and the order is then
// decided by a RANDOM UUID. A campaign queue that is meant to carry a ratified
// priority order cannot rest on that, so the order is written down explicitly.
// TestPostgres_QueueOrderBeatsInsertionOrder below is the discriminating half —
// it constructs positions that CONTRADICT insertion order, so a listing that
// still ordered by (created_at, id) would fail it.
func TestPostgres_Persist_QueueOrderSurvivesRealPostgres(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := campaign.NewPostgresRepository(pool)

	// Ranked in the reverse of numeric order, which is also the order a
	// ReorderByPriority permutation produces from a ratified grooming order.
	res := &workmgmt.EpicChildrenResult{Children: []workmgmt.EpicChild{
		{Number: 903}, {Number: 901}, {Number: 905}, {Number: 902}, {Number: 904},
	}}
	assembly, err := campaign.Assemble("", res)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	created, err := campaign.Persist(context.Background(), repo, "kuhlman-labs/fishhawk", assembly)
	if err != nil {
		t.Fatalf("Persist: %v", err)
	}

	items, err := repo.ListCampaignItemsForCampaign(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	var gotRefs []string
	for _, it := range items {
		gotRefs = append(gotRefs, it.IssueRef)
	}
	want := []string{"issue:903", "issue:901", "issue:905", "issue:902", "issue:904"}
	if !reflect.DeepEqual(gotRefs, want) {
		t.Fatalf("persisted queue order = %v, want %v (the assembled order, read back from Postgres)", gotRefs, want)
	}
	for i, it := range items {
		if it.Position != i {
			t.Fatalf("items[%d].Position = %d, want %d (dense 0-based queue position)", i, it.Position, i)
		}
	}

}

// TestPostgres_QueueOrderBeatsInsertionOrder is the DISCRIMINATING half of K1:
// it writes items whose queue positions CONTRADICT their insertion order, so
// the read-back can only come out right if the listing genuinely orders by
// queue_position. Against the pre-0074 (created_at, id) clause the expected and
// actual orders are exact reverses of each other, so this test cannot pass by
// accident of timing.
func TestPostgres_QueueOrderBeatsInsertionOrder(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := campaign.NewPostgresRepository(pool)
	c := makeCampaign(t, repo)

	// Insert ascending by issue number, assigning DESCENDING queue positions.
	for i, n := range []int{910, 911, 912} {
		if _, err := repo.CreateCampaignItem(context.Background(), campaign.CreateCampaignItemParams{
			CampaignID: c.ID,
			IssueRef:   "issue:" + strconv.Itoa(n),
			Position:   2 - i,
		}); err != nil {
			t.Fatalf("create item %d: %v", n, err)
		}
	}

	items, err := repo.ListCampaignItemsForCampaign(context.Background(), c.ID)
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	var gotRefs []string
	for _, it := range items {
		gotRefs = append(gotRefs, it.IssueRef)
	}
	want := []string{"issue:912", "issue:911", "issue:910"}
	if !reflect.DeepEqual(gotRefs, want) {
		t.Fatalf("listing order = %v, want %v — the listing must order by queue_position, not by insertion (created_at, id)",
			gotRefs, want)
	}
}

// TestPostgres_QueueOrder_EqualPositionsKeepInsertionOrder pins the DEFAULT-0
// compatibility path: every pre-0074 row carries queue_position 0, so the
// retained (created_at, id) tiebreak must still decide their order. Without
// that tiebreak an all-zero campaign would list in an undefined order — a
// silent behaviour change for every campaign that already exists.
func TestPostgres_QueueOrder_EqualPositionsKeepInsertionOrder(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := campaign.NewPostgresRepository(pool)
	c := makeCampaign(t, repo)

	for _, n := range []int{920, 921, 922} {
		if _, err := repo.CreateCampaignItem(context.Background(), campaign.CreateCampaignItemParams{
			CampaignID: c.ID,
			IssueRef:   "issue:" + strconv.Itoa(n),
			// Position left at the zero value: exactly what a pre-0074 row has.
		}); err != nil {
			t.Fatalf("create item %d: %v", n, err)
		}
	}

	items, err := repo.ListCampaignItemsForCampaign(context.Background(), c.ID)
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	var gotRefs []string
	for _, it := range items {
		gotRefs = append(gotRefs, it.IssueRef)
		if it.Position != 0 {
			t.Fatalf("item %s Position = %d, want the 0 default", it.IssueRef, it.Position)
		}
	}
	if want := []string{"issue:920", "issue:921", "issue:922"}; !reflect.DeepEqual(gotRefs, want) {
		t.Fatalf("all-zero-position listing = %v, want insertion order %v", gotRefs, want)
	}
}

// TestPostgres_Campaign_GroomingSourceRoundTrip is binding condition K3's
// COMMITTED-STATE assertion: the provenance must be readable back off the
// campaign ROW, not merely echoed in a create response. It is written by the
// campaigns INSERT itself, so there is no window in which a grooming-sourced
// campaign exists unprovenanced — and no dependency on the best-effort audit
// emit that happens after persistence.
func TestPostgres_Campaign_GroomingSourceRoundTrip(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := campaign.NewPostgresRepository(pool)

	provenance := []byte(`{"source_run_id":"11111111-1111-1111-1111-111111111111","report_content_hash":"sha256:abc","ordered_refs":["issue:903","issue:901"]}`)
	created, err := repo.CreateCampaign(context.Background(), campaign.CreateCampaignParams{
		Repo:           "kuhlman-labs/fishhawk",
		EpicRef:        "",
		GroomingSource: provenance,
	})
	if err != nil {
		t.Fatalf("create campaign: %v", err)
	}
	if !jsonEqual(t, created.GroomingSource, provenance) {
		t.Fatalf("create returned grooming_source %s, want %s", created.GroomingSource, provenance)
	}

	got, err := repo.GetCampaign(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("get campaign: %v", err)
	}
	if !jsonEqual(t, got.GroomingSource, provenance) {
		t.Fatalf("read-back grooming_source = %s, want %s", got.GroomingSource, provenance)
	}

	// A campaign created WITHOUT a grooming source stays nil (NULL column), so
	// the unchanged epic/items paths carry no provenance key at all.
	plainCampaign := makeCampaign(t, repo)
	if plainCampaign.GroomingSource != nil {
		t.Fatalf("non-grooming campaign carries grooming_source %s, want nil", plainCampaign.GroomingSource)
	}
	plainRead, err := repo.GetCampaign(context.Background(), plainCampaign.ID)
	if err != nil {
		t.Fatalf("get plain campaign: %v", err)
	}
	if plainRead.GroomingSource != nil {
		t.Fatalf("read-back non-grooming grooming_source = %s, want nil", plainRead.GroomingSource)
	}
}

// --- Grooming-currency guard (E54.17 / #2817) ---
//
// These cases pin CreateCampaignGuardedByGroomingCurrency against REAL Postgres,
// one behavioral case per NAMED branch of the guard predicate. The SOURCE run is
// described entirely by the campaign.GroomingCurrencyGuard passed to
// CreateCampaign — the guard's subquery only scans for NEWER candidate runs — so
// the fixtures seed only the candidate run(s) + plan stage + grooming_report
// artifact + approval rows BY CONSTRUCTION, never by calling the control. Each
// case reads COMMITTED state (a follow-up ListCampaigns) rather than only the
// returned error, because a guard whose effect is a not-written row cannot be
// pinned by error identity alone.
//
// COUNTERFACTUALS OBSERVED (run, not reasoned — condition 2):
//   - Deleting the whole `WHERE NOT EXISTS (...)` clause from
//     createCampaignGuardedByGroomingCurrency in db/queries.sql.go and re-running
//     TestPostgres_GroomingCurrencyGuard/newer_approved: RED — the campaign row is
//     created (ListCampaigns returns 1, err is nil) instead of the expected
//     ErrGroomingOrderSuperseded + zero rows. Restored byte-identically → GREEN.
//   - Deleting the pgx.ErrNoRows -> ErrGroomingOrderSuperseded translation in
//     postgres.go (case a is the vehicle that traverses the real repository, since
//     the handler-mapping unit injects the sentinel through a fake and cannot see
//     this translation) and re-running the same case: RED — the call returns a
//     bare wrapped "create campaign: no rows in result set" that
//     errors.Is(err, ErrGroomingOrderSuperseded) does not match. Restored → GREEN.
//   - Replacing the `(newer.created_at, newer.id) > ($15, $10)` tuple comparison in
//     db/queries.sql.go with a timestamp-only `newer.created_at > $15` and
//     re-running same_created_at_larger_id: RED — err = <nil>, want
//     ErrGroomingOrderSuperseded (the same-instant larger-id candidate no longer
//     counts as newer). same_created_at_smaller_id stays GREEN either way — proving
//     the id tiebreak, not the timestamp, is what this pair pins. Restored → GREEN.
//   - Widening `newer.account_id IS NOT DISTINCT FROM $13` to
//     `(... OR newer.account_id IS NULL)` (the hypothetical NULL-candidate-as-
//     wildcard predicate) and re-running source_account_set_candidate_null_account:
//     RED — CreateCampaign returned ErrGroomingOrderSuperseded, want created (the
//     set-source guard wrongly matched the NULL-account candidate). Restored → GREEN.

// groomingCandidateSpec describes one seeded NEWER grooming run for the guard
// table. accountID nil = untenanted (NULL account_id); installationID nil = NULL.
type groomingCandidateSpec struct {
	repo           string
	workflowID     string
	accountID      *uuid.UUID
	installationID *int64
	createdAt      time.Time
	kind           artifact.Kind
	approve        bool
	reject         bool
	explicitID     *uuid.UUID // when set, the run row is given this id (id-order cases)
}

// seedGroomingCandidate inserts a candidate run + plan stage + artifact + the
// requested approval rows directly through the real repos, then forces the row's
// created_at (and account_id) via raw SQL — CreateRun stamps now() and a NULL
// account, neither of which the ordering/tenancy cases can control otherwise.
func seedGroomingCandidate(t *testing.T, pool *pgxpool.Pool, runRepo run.Repository, artRepo artifact.Repository, apprRepo approval.Repository, spec groomingCandidateSpec) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	rn, err := runRepo.CreateRun(ctx, run.CreateRunParams{
		Repo: spec.repo, WorkflowID: spec.workflowID, WorkflowSHA: "sha",
		TriggerSource: run.TriggerCLI, InstallationID: spec.installationID,
	})
	if err != nil {
		t.Fatalf("seed candidate run: %v", err)
	}
	runID := rn.ID
	// Force created_at + account_id (and the run id itself for the id-order cases).
	if spec.explicitID != nil {
		if _, err := pool.Exec(ctx, "UPDATE runs SET id=$1 WHERE id=$2", *spec.explicitID, runID); err != nil {
			t.Fatalf("seed candidate id: %v", err)
		}
		runID = *spec.explicitID
	}
	if _, err := pool.Exec(ctx, "UPDATE runs SET created_at=$1, account_id=$2 WHERE id=$3", spec.createdAt, spec.accountID, runID); err != nil {
		t.Fatalf("seed candidate created_at/account: %v", err)
	}
	stage, err := runRepo.CreateStage(ctx, run.CreateStageParams{
		RunID: runID, Sequence: 0, Type: run.StageTypePlan,
		ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code", RequiresApproval: true,
	})
	if err != nil {
		t.Fatalf("seed candidate stage: %v", err)
	}
	kind := spec.kind
	if kind == "" {
		kind = artifact.KindGroomingReport
	}
	body := json.RawMessage(`{"grooming_report":true}`)
	if _, err := artRepo.Create(ctx, artifact.CreateParams{
		StageID: stage.ID, Kind: kind, Content: body, ContentHash: "sha256:candidate",
	}); err != nil {
		t.Fatalf("seed candidate artifact: %v", err)
	}
	if spec.approve {
		if _, err := apprRepo.Submit(ctx, approval.SubmitParams{
			StageID: stage.ID, ApproverSubject: "op-approve", Decision: approval.DecisionApprove, Surface: "cli",
		}); err != nil {
			t.Fatalf("seed candidate approve: %v", err)
		}
	}
	if spec.reject {
		if _, err := apprRepo.Submit(ctx, approval.SubmitParams{
			StageID: stage.ID, ApproverSubject: "op-reject", Decision: approval.DecisionReject, Surface: "cli",
		}); err != nil {
			t.Fatalf("seed candidate reject: %v", err)
		}
	}
	return runID
}

func TestPostgres_GroomingCurrencyGuard(t *testing.T) {
	pool := pgtest.NewPool(t)
	campRepo := campaign.NewPostgresRepository(pool)
	runRepo := run.NewPostgresRepository(pool)
	artRepo := artifact.NewPostgresRepository(pool)
	apprRepo := approval.NewPostgresRepository(pool)
	ctx := context.Background()

	base := time.Now().UTC().Truncate(time.Microsecond)
	const workflow = "backlog_grooming"
	acctA := uuid.New()
	var install7 int64 = 7
	// runs.account_id carries an FK to accounts (migration 0055), so a candidate
	// row bearing acctA needs the account to exist first.
	if _, err := pool.Exec(ctx, "INSERT INTO accounts (id, account_key) VALUES ($1, $2)", acctA, "acct-a"); err != nil {
		t.Fatalf("seed account: %v", err)
	}

	// maxUUID sorts lexically after every candidate id and minUUID before it, so at
	// an EQUAL created_at the (created_at, id) tiebreak is exercised in BOTH
	// directions: with the source id = maxUUID a candidate is lexically SMALLER and
	// does NOT supersede (same_created_at_smaller_id), and with the source id =
	// minUUID a candidate id = maxUUID is lexically LARGER and DOES supersede
	// (same_created_at_larger_id) — proving the tiebreak is a real strict ordering,
	// not a timestamp-only comparison.
	maxUUID := uuid.MustParse("ffffffff-ffff-ffff-ffff-ffffffffffff")
	minUUID := uuid.MustParse("00000000-0000-0000-0000-000000000001")

	type tc struct {
		name        string
		guard       func(repo string) *campaign.GroomingCurrencyGuard // nil = unguarded
		candidates  []groomingCandidateSpec
		wantRefused bool
	}
	cases := []tc{
		{
			name: "newer_approved", // case (a): superseded → refused, zero rows
			guard: func(repo string) *campaign.GroomingCurrencyGuard {
				return &campaign.GroomingCurrencyGuard{SourceRunID: uuid.New(), Repo: repo, WorkflowID: workflow, SourceCreatedAt: base}
			},
			candidates:  []groomingCandidateSpec{{workflowID: workflow, createdAt: base.Add(time.Hour), approve: true}},
			wantRefused: true,
		},
		{
			name: "newer_no_approval", // case (b): report but no grant → created
			guard: func(repo string) *campaign.GroomingCurrencyGuard {
				return &campaign.GroomingCurrencyGuard{SourceRunID: uuid.New(), Repo: repo, WorkflowID: workflow, SourceCreatedAt: base}
			},
			candidates: []groomingCandidateSpec{{workflowID: workflow, createdAt: base.Add(time.Hour)}},
		},
		{
			name: "newer_approve_and_reject", // case (c): contested → not ratified → created
			guard: func(repo string) *campaign.GroomingCurrencyGuard {
				return &campaign.GroomingCurrencyGuard{SourceRunID: uuid.New(), Repo: repo, WorkflowID: workflow, SourceCreatedAt: base}
			},
			candidates: []groomingCandidateSpec{{workflowID: workflow, createdAt: base.Add(time.Hour), approve: true, reject: true}},
		},
		{
			name: "different_repo", // case (d): candidate on another repo → created
			guard: func(repo string) *campaign.GroomingCurrencyGuard {
				return &campaign.GroomingCurrencyGuard{SourceRunID: uuid.New(), Repo: repo, WorkflowID: workflow, SourceCreatedAt: base}
			},
			candidates: []groomingCandidateSpec{{repo: "someone/else", workflowID: workflow, createdAt: base.Add(time.Hour), approve: true}},
		},
		{
			name: "different_account", // case (d): candidate tenanted, source untenanted → created
			guard: func(repo string) *campaign.GroomingCurrencyGuard {
				return &campaign.GroomingCurrencyGuard{SourceRunID: uuid.New(), Repo: repo, WorkflowID: workflow, SourceCreatedAt: base}
			},
			candidates: []groomingCandidateSpec{{workflowID: workflow, accountID: &acctA, createdAt: base.Add(time.Hour), approve: true}},
		},
		{
			name: "different_workflow", // case (d): candidate on another workflow → created
			guard: func(repo string) *campaign.GroomingCurrencyGuard {
				return &campaign.GroomingCurrencyGuard{SourceRunID: uuid.New(), Repo: repo, WorkflowID: workflow, SourceCreatedAt: base}
			},
			candidates: []groomingCandidateSpec{{workflowID: "feature_change", createdAt: base.Add(time.Hour), approve: true}},
		},
		{
			name: "different_installation", // case (d): candidate installed, source nil → created
			guard: func(repo string) *campaign.GroomingCurrencyGuard {
				return &campaign.GroomingCurrencyGuard{SourceRunID: uuid.New(), Repo: repo, WorkflowID: workflow, SourceCreatedAt: base}
			},
			candidates: []groomingCandidateSpec{{workflowID: workflow, installationID: &install7, createdAt: base.Add(time.Hour), approve: true}},
		},
		{
			// case (d) REVERSE of different_account: source tenanted, candidate NULL
			// account → distinct → created. Pins IS NOT DISTINCT FROM in the
			// set-vs-NULL direction — a predicate treating a NULL candidate column as
			// a wildcard when the source side is SET would wrongly refuse here.
			name: "source_account_set_candidate_null_account",
			guard: func(repo string) *campaign.GroomingCurrencyGuard {
				return &campaign.GroomingCurrencyGuard{SourceRunID: uuid.New(), Repo: repo, WorkflowID: workflow, AccountID: acctA.String(), SourceCreatedAt: base}
			},
			candidates: []groomingCandidateSpec{{workflowID: workflow, createdAt: base.Add(time.Hour), approve: true}},
		},
		{
			// case (d) REVERSE of different_installation: source installed, candidate
			// NULL installation → distinct → created. The installation-column mirror
			// of the case above.
			name: "source_installation_set_candidate_null_installation",
			guard: func(repo string) *campaign.GroomingCurrencyGuard {
				return &campaign.GroomingCurrencyGuard{SourceRunID: uuid.New(), Repo: repo, WorkflowID: workflow, SourceInstallationID: &install7, SourceCreatedAt: base}
			},
			candidates: []groomingCandidateSpec{{workflowID: workflow, createdAt: base.Add(time.Hour), approve: true}},
		},
		{
			// (d): NULL IS NOT DISTINCT FROM NULL → superseded. Documentary — shares
			// its setup with newer_approved; the created-direction discrimination of
			// the NULL-vs-set tenancy semantics lives in the two reverse cases above
			// (source_account_set_candidate_null_account /
			// source_installation_set_candidate_null_installation).
			name: "both_null_account_and_installation_match",
			guard: func(repo string) *campaign.GroomingCurrencyGuard {
				return &campaign.GroomingCurrencyGuard{SourceRunID: uuid.New(), Repo: repo, WorkflowID: workflow, SourceCreatedAt: base}
			},
			candidates:  []groomingCandidateSpec{{workflowID: workflow, createdAt: base.Add(time.Hour), approve: true}},
			wantRefused: true,
		},
		{
			name: "both_set_equal_account_and_installation_match", // (d): equal set values match → superseded
			guard: func(repo string) *campaign.GroomingCurrencyGuard {
				return &campaign.GroomingCurrencyGuard{SourceRunID: uuid.New(), Repo: repo, WorkflowID: workflow, AccountID: acctA.String(), SourceInstallationID: &install7, SourceCreatedAt: base}
			},
			candidates:  []groomingCandidateSpec{{workflowID: workflow, accountID: &acctA, installationID: &install7, createdAt: base.Add(time.Hour), approve: true}},
			wantRefused: true,
		},
		{
			name: "older_approved", // case (e): candidate older → not newer → created
			guard: func(repo string) *campaign.GroomingCurrencyGuard {
				return &campaign.GroomingCurrencyGuard{SourceRunID: uuid.New(), Repo: repo, WorkflowID: workflow, SourceCreatedAt: base}
			},
			candidates: []groomingCandidateSpec{{workflowID: workflow, createdAt: base.Add(-time.Hour), approve: true}},
		},
		{
			name: "same_created_at_smaller_id", // case (e): equal instant, candidate id < source id → created
			guard: func(repo string) *campaign.GroomingCurrencyGuard {
				return &campaign.GroomingCurrencyGuard{SourceRunID: maxUUID, Repo: repo, WorkflowID: workflow, SourceCreatedAt: base}
			},
			candidates: []groomingCandidateSpec{{workflowID: workflow, createdAt: base, approve: true, explicitID: uuidPtr(minUUID)}},
		},
		{
			name: "same_created_at_larger_id", // case (a): equal instant, candidate id > source id → newer → refused
			guard: func(repo string) *campaign.GroomingCurrencyGuard {
				return &campaign.GroomingCurrencyGuard{SourceRunID: minUUID, Repo: repo, WorkflowID: workflow, SourceCreatedAt: base}
			},
			candidates:  []groomingCandidateSpec{{workflowID: workflow, createdAt: base, approve: true, explicitID: uuidPtr(maxUUID)}},
			wantRefused: true,
		},
		{
			name:       "nil_guard_with_newer_approved", // case (f): allow_superseded path → created
			guard:      func(string) *campaign.GroomingCurrencyGuard { return nil },
			candidates: []groomingCandidateSpec{{workflowID: workflow, createdAt: base.Add(time.Hour), approve: true}},
		},
		{
			name: "newer_approved_plan_artifact", // case (g): approved but artifact kind=plan → created
			guard: func(repo string) *campaign.GroomingCurrencyGuard {
				return &campaign.GroomingCurrencyGuard{SourceRunID: uuid.New(), Repo: repo, WorkflowID: workflow, SourceCreatedAt: base}
			},
			candidates: []groomingCandidateSpec{{workflowID: workflow, createdAt: base.Add(time.Hour), approve: true, kind: artifact.KindPlan}},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// A unique campaign repo per case isolates both the candidate scan (scoped
			// by source_repo) and the committed-state count (ListCampaigns by repo).
			campaignRepo := "kuhlman-labs/case-" + c.name
			for _, spec := range c.candidates {
				if spec.repo == "" {
					spec.repo = campaignRepo
				}
				seedGroomingCandidate(t, pool, runRepo, artRepo, apprRepo, spec)
			}
			var guard *campaign.GroomingCurrencyGuard
			if g := c.guard(campaignRepo); g != nil {
				guard = g
			}
			created, err := campRepo.CreateCampaign(ctx, campaign.CreateCampaignParams{
				Repo: campaignRepo, EpicRef: "", GroomingGuard: guard,
			})
			listed, lerr := campRepo.ListCampaigns(ctx, campaign.ListCampaignsFilter{Repo: campaignRepo, Limit: 10})
			if lerr != nil {
				t.Fatalf("list campaigns: %v", lerr)
			}
			if c.wantRefused {
				if !errors.Is(err, campaign.ErrGroomingOrderSuperseded) {
					t.Fatalf("err = %v, want ErrGroomingOrderSuperseded", err)
				}
				if created != nil {
					t.Fatalf("created = %+v, want nil on refusal", created)
				}
				if len(listed) != 0 {
					t.Fatalf("committed campaigns = %d, want 0 (no row on refusal)", len(listed))
				}
				return
			}
			if err != nil {
				t.Fatalf("CreateCampaign err = %v, want created", err)
			}
			if created == nil {
				t.Fatalf("created = nil, want a campaign")
			}
			if len(listed) != 1 {
				t.Fatalf("committed campaigns = %d, want 1", len(listed))
			}
		})
	}
}

func uuidPtr(u uuid.UUID) *uuid.UUID { return &u }

// --- AppendCampaignItems (E86.47 / #4358) ---

// appendFixture seeds a campaign with items via the repository and returns the
// repo, campaign and items by ref.
func appendFixture(t *testing.T, items ...campaign.CreateCampaignItemParams) (campaign.Repository, *pgxpool.Pool, *campaign.Campaign, map[string]*campaign.Item) {
	t.Helper()
	pool := pgtest.NewPool(t)
	repo := campaign.NewPostgresRepository(pool)
	c := makeCampaign(t, repo)
	byRef := map[string]*campaign.Item{}
	for i, p := range items {
		p.CampaignID = c.ID
		p.Position = i
		it, err := repo.CreateCampaignItem(context.Background(), p)
		if err != nil {
			t.Fatalf("seed item %s: %v", p.IssueRef, err)
		}
		byRef[it.IssueRef] = it
	}
	return repo, pool, c, byRef
}

// readItems re-reads the committed items keyed by ref.
func readItems(t *testing.T, repo campaign.Repository, id uuid.UUID) map[string]*campaign.Item {
	t.Helper()
	items, err := repo.ListCampaignItemsForCampaign(context.Background(), id)
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	out := map[string]*campaign.Item{}
	for _, it := range items {
		out[it.IssueRef] = it
	}
	return out
}

func TestPostgres_AppendCampaignItems_QueuePositions(t *testing.T) {
	repo, _, c, _ := appendFixture(t,
		campaign.CreateCampaignItemParams{IssueRef: "issue:10"},
		campaign.CreateCampaignItemParams{IssueRef: "issue:11"},
	)
	var recorded *campaign.AppendOutcome
	out, err := repo.AppendCampaignItems(context.Background(), campaign.AppendCampaignItemsParams{
		CampaignID: c.ID,
		NewItems: []campaign.CreateCampaignItemParams{
			{IssueRef: "issue:20", DependsOn: []string{"issue:10"}, Autonomy: "low"},
			{IssueRef: "issue:21", DependsOn: []string{"issue:20"}},
		},
		Record: func(_ context.Context, o campaign.AppendOutcome) error { recorded = &o; return nil },
	})
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if len(out.Created) != 2 || recorded == nil || len(recorded.Created) != 2 {
		t.Fatalf("created = %d, recorded = %v", len(out.Created), recorded)
	}
	got := readItems(t, repo, c.ID)
	if got["issue:20"].Position != 2 || got["issue:21"].Position != 3 {
		t.Errorf("positions = %d,%d, want 2,3", got["issue:20"].Position, got["issue:21"].Position)
	}
	if got["issue:20"].Autonomy != "low" || !reflect.DeepEqual(got["issue:21"].DependsOn, []string{"issue:20"}) {
		t.Errorf("read-back mismatch: %+v %+v", got["issue:20"], got["issue:21"])
	}
}

func TestPostgres_AppendCampaignItems_DuplicateUnderLock(t *testing.T) {
	repo, _, c, _ := appendFixture(t, campaign.CreateCampaignItemParams{IssueRef: "issue:10"})
	_, err := repo.AppendCampaignItems(context.Background(), campaign.AppendCampaignItemsParams{
		CampaignID: c.ID,
		NewItems:   []campaign.CreateCampaignItemParams{{IssueRef: "issue:11"}, {IssueRef: "issue:10"}},
	})
	if !errors.Is(err, campaign.ErrDuplicateItem) {
		t.Fatalf("err = %v, want ErrDuplicateItem", err)
	}
	if n := len(readItems(t, repo, c.ID)); n != 1 {
		t.Errorf("item count = %d, want 1 (nothing written)", n)
	}
}

func TestPostgres_AppendCampaignItems_CycleWritesNothing(t *testing.T) {
	// Seeded BY CONSTRUCTION: A already depends on B, which is not yet a member.
	repo, _, c, _ := appendFixture(t, campaign.CreateCampaignItemParams{IssueRef: "issue:1", DependsOn: []string{"issue:2"}})
	for name, newItems := range map[string][]campaign.CreateCampaignItemParams{
		"new-existing": {{IssueRef: "issue:2", DependsOn: []string{"issue:1"}}},
		"new-new":      {{IssueRef: "issue:3", DependsOn: []string{"issue:4"}}, {IssueRef: "issue:4", DependsOn: []string{"issue:3"}}},
	} {
		_, err := repo.AppendCampaignItems(context.Background(), campaign.AppendCampaignItemsParams{CampaignID: c.ID, NewItems: newItems})
		if !errors.Is(err, campaign.ErrCycle) {
			t.Fatalf("%s: err = %v, want ErrCycle", name, err)
		}
		got := readItems(t, repo, c.ID)
		if len(got) != 1 || !reflect.DeepEqual(got["issue:1"].DependsOn, []string{"issue:2"}) {
			t.Errorf("%s: committed state changed: %d items, deps %v", name, len(got), got["issue:1"].DependsOn)
		}
	}
}

func TestPostgres_AppendCampaignItems_ExternalRefusedUnlessAllowed(t *testing.T) {
	repo, _, c, _ := appendFixture(t, campaign.CreateCampaignItemParams{IssueRef: "issue:10"})
	p := campaign.AppendCampaignItemsParams{
		CampaignID: c.ID,
		NewItems:   []campaign.CreateCampaignItemParams{{IssueRef: "issue:20", DependsOn: []string{"issue:999"}}},
	}
	if _, err := repo.AppendCampaignItems(context.Background(), p); !errors.Is(err, campaign.ErrExternalDependency) {
		t.Fatalf("err = %v, want ErrExternalDependency", err)
	}
	if n := len(readItems(t, repo, c.ID)); n != 1 {
		t.Fatalf("item count = %d, want 1", n)
	}
	p.AllowExternal = true
	if _, err := repo.AppendCampaignItems(context.Background(), p); err != nil {
		t.Fatalf("allowed: %v", err)
	}
	if got := readItems(t, repo, c.ID); !reflect.DeepEqual(got["issue:20"].DependsOn, []string{"issue:999"}) {
		t.Errorf("external edge not persisted: %v", got["issue:20"])
	}
}

func TestPostgres_AppendCampaignItems_AdditionIsMonotone(t *testing.T) {
	repo, _, c, items := appendFixture(t,
		campaign.CreateCampaignItemParams{IssueRef: "issue:1"},
		campaign.CreateCampaignItemParams{IssueRef: "issue:2"},
		campaign.CreateCampaignItemParams{IssueRef: "issue:3", DependsOn: []string{"issue:1"}},
	)
	p3 := items["issue:3"].ID
	out, err := repo.AppendCampaignItems(context.Background(), campaign.AppendCampaignItemsParams{
		CampaignID:      c.ID,
		AddDependencies: map[uuid.UUID][]string{p3: {"issue:2"}},
	})
	if err != nil {
		t.Fatalf("append addition: %v", err)
	}
	if len(out.Added) != 1 || !reflect.DeepEqual(out.Added[0].Refs, []string{"issue:2"}) {
		t.Errorf("added = %+v", out.Added)
	}
	// A NARROWER addition (only issue:2) must retain the old issue:1 edge.
	if _, err := repo.AppendCampaignItems(context.Background(), campaign.AppendCampaignItemsParams{
		CampaignID:      c.ID,
		AddDependencies: map[uuid.UUID][]string{p3: {"issue:2"}},
	}); err != nil {
		t.Fatalf("narrower addition: %v", err)
	}
	if got := readItems(t, repo, c.ID)["issue:3"].DependsOn; !reflect.DeepEqual(got, []string{"issue:1", "issue:2"}) {
		t.Errorf("depends_on = %v, want [issue:1 issue:2] (monotone union)", got)
	}
}

func TestPostgres_AppendCampaignItems_RunningItemSkipped(t *testing.T) {
	repo, _, c, items := appendFixture(t,
		campaign.CreateCampaignItemParams{IssueRef: "issue:1"},
		campaign.CreateCampaignItemParams{IssueRef: "issue:2", DependsOn: []string{"issue:1"}},
	)
	running := items["issue:2"]
	if _, err := repo.TransitionCampaignItem(context.Background(), running.ID, campaign.ItemStateRunning); err != nil {
		t.Fatalf("transition: %v", err)
	}
	before := readItems(t, repo, c.ID)["issue:2"]
	out, err := repo.AppendCampaignItems(context.Background(), campaign.AppendCampaignItemsParams{
		CampaignID:      c.ID,
		AddDependencies: map[uuid.UUID][]string{running.ID: {"issue:3"}},
		NewItems:        []campaign.CreateCampaignItemParams{{IssueRef: "issue:3"}},
	})
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if !reflect.DeepEqual(out.SkippedStarted, []string{"issue:2"}) || len(out.Added) != 0 {
		t.Errorf("skipped = %v added = %v", out.SkippedStarted, out.Added)
	}
	after := readItems(t, repo, c.ID)["issue:2"]
	if !reflect.DeepEqual(after.DependsOn, before.DependsOn) || after.State != campaign.ItemStateRunning || !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Errorf("running item touched: before %+v after %+v", before, after)
	}
}

func TestPostgres_AppendCampaignItems_MalformedRefRefused(t *testing.T) {
	repo, _, c, items := appendFixture(t, campaign.CreateCampaignItemParams{IssueRef: "issue:1"})
	for name, p := range map[string]campaign.AppendCampaignItemsParams{
		"new dep":  {CampaignID: c.ID, AllowExternal: true, NewItems: []campaign.CreateCampaignItemParams{{IssueRef: "issue:2", DependsOn: []string{"unparsable:x"}}}},
		"addition": {CampaignID: c.ID, AddDependencies: map[uuid.UUID][]string{items["issue:1"].ID: {"unparsable:x"}}},
	} {
		if _, err := repo.AppendCampaignItems(context.Background(), p); !errors.Is(err, campaign.ErrInvalidDependencyRef) {
			t.Fatalf("%s: err = %v, want ErrInvalidDependencyRef", name, err)
		}
		got := readItems(t, repo, c.ID)
		if len(got) != 1 || len(got["issue:1"].DependsOn) != 0 {
			t.Errorf("%s: committed state changed: %+v", name, got)
		}
	}
}

func TestPostgres_AppendCampaignItems_RecordFailureWritesNothing(t *testing.T) {
	repo, _, c, items := appendFixture(t,
		campaign.CreateCampaignItemParams{IssueRef: "issue:1"},
		campaign.CreateCampaignItemParams{IssueRef: "issue:2"},
	)
	boom := errors.New("audit down")
	_, err := repo.AppendCampaignItems(context.Background(), campaign.AppendCampaignItemsParams{
		CampaignID:      c.ID,
		NewItems:        []campaign.CreateCampaignItemParams{{IssueRef: "issue:3"}},
		AddDependencies: map[uuid.UUID][]string{items["issue:2"].ID: {"issue:1"}},
		Record:          func(context.Context, campaign.AppendOutcome) error { return boom },
	})
	if !errors.Is(err, campaign.ErrAppendUnrecorded) || !errors.Is(err, boom) {
		t.Fatalf("err = %v, want ErrAppendUnrecorded wrapping the record error", err)
	}
	got := readItems(t, repo, c.ID)
	if len(got) != 2 || len(got["issue:2"].DependsOn) != 0 {
		t.Errorf("record failure left writes: %d items, issue:2 deps %v", len(got), got["issue:2"].DependsOn)
	}
}

func TestPostgres_AppendCampaignItems_TerminalCampaignRefused(t *testing.T) {
	repo, _, c, _ := appendFixture(t)
	if _, err := repo.TransitionCampaign(context.Background(), c.ID, campaign.StateCancelled); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	_, err := repo.AppendCampaignItems(context.Background(), campaign.AppendCampaignItemsParams{
		CampaignID: c.ID, NewItems: []campaign.CreateCampaignItemParams{{IssueRef: "issue:1"}},
	})
	var inv campaign.InvalidTransitionError
	if !errors.As(err, &inv) || inv.Kind != "campaign" || inv.From != string(campaign.StateCancelled) {
		t.Fatalf("err = %v, want InvalidTransitionError{campaign, cancelled}", err)
	}
	if n := len(readItems(t, repo, c.ID)); n != 0 {
		t.Errorf("item count = %d, want 0", n)
	}
	if _, err := repo.AppendCampaignItems(context.Background(), campaign.AppendCampaignItemsParams{CampaignID: uuid.New()}); !errors.Is(err, campaign.ErrCampaignNotFound) {
		t.Errorf("missing campaign err = %v, want ErrCampaignNotFound", err)
	}
}

// --- Item start claims (#5266) ---

var claimT0 = time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)

// fixedClock returns a clock pinned at at.
func fixedClock(at time.Time) func() time.Time { return func() time.Time { return at } }

// newClaimFixture seeds one unlinked item on a fresh database.
func newClaimFixture(t *testing.T) (*pgxpool.Pool, campaign.Repository, run.Repository, *campaign.Item) {
	t.Helper()
	pool := pgtest.NewPool(t)
	repo := campaign.NewPostgresRepository(pool)
	c := makeCampaign(t, repo)
	item, err := repo.CreateCampaignItem(context.Background(), campaign.CreateCampaignItemParams{
		CampaignID: c.ID,
		IssueRef:   "issue:5266",
	})
	if err != nil {
		t.Fatalf("create item: %v", err)
	}
	return pool, repo, run.NewPostgresRepository(pool), item
}

func makeClaimRun(t *testing.T, runs run.Repository) uuid.UUID {
	t.Helper()
	r, err := runs.CreateRun(context.Background(), run.CreateRunParams{
		Repo:          "kuhlman-labs/fishhawk",
		WorkflowID:    "feature_change",
		WorkflowSHA:   "deadbeef",
		TriggerSource: run.TriggerCLI,
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	return r.ID
}

func claimAt(t *testing.T, repo campaign.Repository, itemID uuid.UUID, starter string, at time.Time) *campaign.StartClaim {
	t.Helper()
	c, err := repo.ClaimCampaignItemStart(context.Background(), campaign.ClaimCampaignItemStartParams{
		ItemID: itemID, Starter: starter, Clock: fixedClock(at),
	})
	if err != nil {
		t.Fatalf("claim %s at %s: %v", starter, at, err)
	}
	return c
}

// readClaimRow reads the item's committed claim row, or nil when it has none.
func readClaimRow(t *testing.T, pool *pgxpool.Pool, itemID uuid.UUID) *campaign.StartClaim {
	t.Helper()
	var c campaign.StartClaim
	err := pool.QueryRow(context.Background(),
		`SELECT item_id, claim_id, starter, claimed_at, expires_at FROM campaign_item_start_claims WHERE item_id = $1`,
		itemID).Scan(&c.ItemID, &c.ClaimID, &c.Starter, &c.ClaimedAt, &c.ExpiresAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		t.Fatalf("read claim row: %v", err)
	}
	return &c
}

func wantStartConflict(t *testing.T, err error, reason campaign.StartConflictReason) *campaign.ItemStartConflictError {
	t.Helper()
	var ce *campaign.ItemStartConflictError
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v, want *ItemStartConflictError{%s}", err, reason)
	}
	if ce.Reason != reason {
		t.Fatalf("conflict reason = %q, want %q (err %v)", ce.Reason, reason, err)
	}
	return ce
}

func sameClaim(a, b *campaign.StartClaim) bool {
	return a != nil && b != nil && a.ItemID == b.ItemID && a.ClaimID == b.ClaimID && a.Starter == b.Starter &&
		a.ClaimedAt.Equal(b.ClaimedAt) && a.ExpiresAt.Equal(b.ExpiresAt)
}

func TestClaimCampaignItemStart_UnlinkedItem_ClaimsWithTTL(t *testing.T) {
	pool, repo, _, item := newClaimFixture(t)
	got := claimAt(t, repo, item.ID, campaign.StarterOperator, claimT0)
	if got.ItemID != item.ID || got.ClaimID == uuid.Nil || got.Starter != campaign.StarterOperator {
		t.Errorf("claim = %+v, want item %s, a fresh claim id, starter operator", got, item.ID)
	}
	if !got.ClaimedAt.Equal(claimT0) || !got.ExpiresAt.Equal(claimT0.Add(campaign.ItemStartClaimTTL)) {
		t.Errorf("claim window = [%s, %s], want [%s, +%s]", got.ClaimedAt, got.ExpiresAt, claimT0, campaign.ItemStartClaimTTL)
	}
	if row := readClaimRow(t, pool, item.ID); !sameClaim(row, got) {
		t.Errorf("committed claim row = %+v, want %+v", row, got)
	}
}

func TestClaimCampaignItemStart_LinkedItem_RefusedNamingWinner(t *testing.T) {
	pool, repo, runs, item := newClaimFixture(t)
	winner := makeClaimRun(t, runs)
	if _, err := repo.SetCampaignItemRun(context.Background(), item.ID, &winner); err != nil {
		t.Fatalf("link winner: %v", err)
	}
	got, err := repo.ClaimCampaignItemStart(context.Background(), campaign.ClaimCampaignItemStartParams{
		ItemID: item.ID, Starter: campaign.StarterCampaignDriver, Clock: fixedClock(claimT0),
	})
	ce := wantStartConflict(t, err, campaign.StartConflictLinked)
	if got != nil {
		t.Errorf("claim = %+v, want nil on a refusal", got)
	}
	if ce.WinnerRunID == nil || *ce.WinnerRunID != winner || ce.ItemID != item.ID {
		t.Errorf("conflict = %+v, want item %s winner %s", ce, item.ID, winner)
	}
	if row := readClaimRow(t, pool, item.ID); row != nil {
		t.Errorf("claim row after a linked refusal = %+v, want none written", row)
	}
}

func TestClaimCampaignItemStart_LiveClaimHeld_Refused(t *testing.T) {
	pool, repo, _, item := newClaimFixture(t)
	holder := claimAt(t, repo, item.ID, campaign.StarterCampaignDriver, claimT0)
	_, err := repo.ClaimCampaignItemStart(context.Background(), campaign.ClaimCampaignItemStartParams{
		ItemID: item.ID, Starter: campaign.StarterOperator, Clock: fixedClock(claimT0.Add(time.Minute)),
	})
	ce := wantStartConflict(t, err, campaign.StartConflictClaimed)
	if !sameClaim(ce.HeldClaim, holder) {
		t.Errorf("HeldClaim = %+v, want the holder %+v", ce.HeldClaim, holder)
	}
	if row := readClaimRow(t, pool, item.ID); !sameClaim(row, holder) {
		t.Errorf("claim row after a refused claimant = %+v, want the holder's row unchanged %+v", row, holder)
	}
}

func TestClaimCampaignItemStart_ExpiredClaim_Reclaimed(t *testing.T) {
	pool, repo, _, item := newClaimFixture(t)
	crashed := claimAt(t, repo, item.ID, campaign.StarterCampaignDriver, claimT0)
	// A claimant AT the crashed claim's expires_at reclaims (crash recovery).
	at := crashed.ExpiresAt
	got := claimAt(t, repo, item.ID, campaign.StarterOperator, at)
	if got.ClaimID == crashed.ClaimID || got.Starter != campaign.StarterOperator {
		t.Errorf("reclaim = %+v, want a NEW operator claim (crashed claim %s)", got, crashed.ClaimID)
	}
	if !got.ClaimedAt.Equal(at) || !got.ExpiresAt.Equal(at.Add(campaign.ItemStartClaimTTL)) {
		t.Errorf("reclaim window = [%s, %s], want [%s, +TTL]", got.ClaimedAt, got.ExpiresAt, at)
	}
	if row := readClaimRow(t, pool, item.ID); !sameClaim(row, got) {
		t.Errorf("committed row = %+v, want the reclaim %+v", row, got)
	}
}

func TestClaimCampaignItemStart_MissingItem_NotFound(t *testing.T) {
	_, repo, _, _ := newClaimFixture(t)
	_, err := repo.ClaimCampaignItemStart(context.Background(), campaign.ClaimCampaignItemStartParams{
		ItemID: uuid.New(), Starter: campaign.StarterOperator, Clock: fixedClock(claimT0),
	})
	if !errors.Is(err, campaign.ErrNotFound) {
		t.Errorf("claim on a missing item: err = %v, want ErrNotFound", err)
	}
}

func TestClaimCampaignItemStart_InvalidParams_Refused(t *testing.T) {
	pool, repo, _, item := newClaimFixture(t)
	missing := uuid.New()
	for name, tc := range map[string]campaign.ClaimCampaignItemStartParams{
		// A MISSING item proves the first two are refused before any read:
		// a read would answer ErrNotFound instead.
		"unknown starter": {ItemID: missing, Starter: "human", Clock: fixedClock(claimT0)},
		"empty starter":   {ItemID: missing, Starter: "", Clock: fixedClock(claimT0)},
		"nil clock":       {ItemID: missing, Starter: campaign.StarterOperator},
		"zero reading":    {ItemID: item.ID, Starter: campaign.StarterOperator, Clock: fixedClock(time.Time{})},
	} {
		got, err := repo.ClaimCampaignItemStart(context.Background(), tc)
		if !errors.Is(err, campaign.ErrStartClaimInvalid) || got != nil {
			t.Errorf("%s: claim = %+v, err = %v, want nil, ErrStartClaimInvalid", name, got, err)
		}
	}
	if row := readClaimRow(t, pool, item.ID); row != nil {
		t.Errorf("claim row after invalid claims = %+v, want none", row)
	}
}

// waitForLockWaiter blocks until a backend on this test's database is waiting
// on a lock, failing if done closes first (the claim returned without
// waiting) or the bound elapses.
func waitForLockWaiter(t *testing.T, pool *pgxpool.Pool, done <-chan struct{}) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		select {
		case <-done:
			t.Fatal("the claim returned before the test released the item lock: it did not wait on the item row lock")
		default:
		}
		var waiting int
		if err := pool.QueryRow(context.Background(),
			`SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock' AND pid <> pg_backend_pid()`,
		).Scan(&waiting); err != nil {
			t.Fatalf("read pg_stat_activity: %v", err)
		}
		if waiting > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("no backend began waiting on the item lock within 15s")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestClaimCampaignItemStart_WaitsOnItemLock_SeesCommittedLink pins the T1
// atomicity: a claim blocked behind a transaction that holds the item row lock
// and links a run reads the COMMITTED row once the lock is released and
// refuses linked, rather than claiming an item that is now linked.
func TestClaimCampaignItemStart_WaitsOnItemLock_SeesCommittedLink(t *testing.T) {
	pool, repo, runs, item := newClaimFixture(t)
	winner := makeClaimRun(t, runs)
	ctx := context.Background()

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT id FROM campaign_items WHERE id = $1 FOR UPDATE`, item.ID); err != nil {
		t.Fatalf("lock item: %v", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE campaign_items SET run_id = $2 WHERE id = $1`, item.ID, winner); err != nil {
		t.Fatalf("link inside the open transaction: %v", err)
	}

	done := make(chan struct{})
	var claimErr error
	go func() {
		defer close(done)
		_, claimErr = repo.ClaimCampaignItemStart(ctx, campaign.ClaimCampaignItemStartParams{
			ItemID: item.ID, Starter: campaign.StarterCampaignDriver, Clock: fixedClock(claimT0),
		})
	}()
	waitForLockWaiter(t, pool, done)
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the link: %v", err)
	}
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("claim did not return within 15s of the link committing")
	}
	ce := wantStartConflict(t, claimErr, campaign.StartConflictLinked)
	if ce.WinnerRunID == nil || *ce.WinnerRunID != winner {
		t.Errorf("WinnerRunID = %v, want the committed run %s", ce.WinnerRunID, winner)
	}
	if row := readClaimRow(t, pool, item.ID); row != nil {
		t.Errorf("claim row = %+v, want none: the item was linked when the claim acquired the lock", row)
	}
}

// TestClaimCampaignItemStart_LockWait_StampsFromPostLockClock pins #5266 X4:
// the claim's claimed_at / expires_at come from the clock read AFTER the item
// row lock is granted. The lock wait here consumes 9 of the claim TTL's 10
// minutes; a claim stamped before the lock would have 1 minute left, which no
// longer covers the start and cleanup budgets.
func TestClaimCampaignItemStart_LockWait_StampsFromPostLockClock(t *testing.T) {
	pool, repo, _, item := newClaimFixture(t)
	ctx := context.Background()
	var clockNanos atomicTime
	clockNanos.set(claimT0)

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT id FROM campaign_items WHERE id = $1 FOR UPDATE`, item.ID); err != nil {
		t.Fatalf("lock item: %v", err)
	}

	done := make(chan struct{})
	var got *campaign.StartClaim
	var claimErr error
	go func() {
		defer close(done)
		got, claimErr = repo.ClaimCampaignItemStart(ctx, campaign.ClaimCampaignItemStartParams{
			ItemID: item.ID, Starter: campaign.StarterOperator, Clock: clockNanos.get,
		})
	}()
	waitForLockWaiter(t, pool, done)
	postLock := claimT0.Add(9 * time.Minute)
	clockNanos.set(postLock)
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("release the item lock: %v", err)
	}
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("claim did not return within 15s of the lock release")
	}
	if claimErr != nil {
		t.Fatalf("claim after the lock wait: %v", claimErr)
	}
	if !got.ClaimedAt.Equal(postLock) || !got.ExpiresAt.Equal(postLock.Add(campaign.ItemStartClaimTTL)) {
		t.Errorf("claim window = [%s, %s], want [%s, +TTL] (stamped from the post-lock clock, not %s)",
			got.ClaimedAt, got.ExpiresAt, postLock, claimT0)
	}
	if !got.Covers(postLock, campaign.ItemStartWorstCase) {
		t.Errorf("claim %+v does not cover ItemStartWorstCase (%s) at %s", got, campaign.ItemStartWorstCase, postLock)
	}
	if row := readClaimRow(t, pool, item.ID); !sameClaim(row, got) {
		t.Errorf("committed row = %+v, want %+v", row, got)
	}
}

// atomicTime is a race-safe mutable clock.
type atomicTime struct {
	mu sync.Mutex
	t  time.Time
}

func (a *atomicTime) set(t time.Time) { a.mu.Lock(); a.t = t; a.mu.Unlock() }
func (a *atomicTime) get() time.Time  { a.mu.Lock(); defer a.mu.Unlock(); return a.t }

// TestClaimCampaignItemStart_Concurrent_ExactlyOneWins: two starters claiming
// the same item at the same instant — exactly one gets the claim, the other is
// refused claimed naming the winner.
func TestClaimCampaignItemStart_Concurrent_ExactlyOneWins(t *testing.T) {
	pool, repo, _, item := newClaimFixture(t)
	starters := []string{campaign.StarterOperator, campaign.StarterCampaignDriver}
	claims := make([]*campaign.StartClaim, len(starters))
	errs := make([]error, len(starters))
	var wg sync.WaitGroup
	for i, s := range starters {
		wg.Add(1)
		go func(i int, s string) {
			defer wg.Done()
			claims[i], errs[i] = repo.ClaimCampaignItemStart(context.Background(), campaign.ClaimCampaignItemStartParams{
				ItemID: item.ID, Starter: s, Clock: fixedClock(claimT0),
			})
		}(i, s)
	}
	wg.Wait()
	var winner *campaign.StartClaim
	refused := 0
	for i := range starters {
		if errs[i] != nil {
			wantStartConflict(t, errs[i], campaign.StartConflictClaimed)
			refused++
			continue
		}
		if winner != nil {
			t.Fatalf("both starters claimed: %+v and %+v", winner, claims[i])
		}
		winner = claims[i]
	}
	if winner == nil || refused != 1 {
		t.Fatalf("winner = %+v, refused = %d, want exactly one winner and one refusal", winner, refused)
	}
	if row := readClaimRow(t, pool, item.ID); !sameClaim(row, winner) {
		t.Errorf("committed row = %+v, want the winner %+v", row, winner)
	}
}

func TestLinkClaimedCampaignItemRun_OwnClaim_LinksAndConsumesClaim(t *testing.T) {
	pool, repo, runs, item := newClaimFixture(t)
	claim := claimAt(t, repo, item.ID, campaign.StarterOperator, claimT0)
	runID := makeClaimRun(t, runs)
	got, err := repo.LinkClaimedCampaignItemRun(context.Background(), campaign.LinkClaimedCampaignItemRunParams{
		ItemID: item.ID, ClaimID: claim.ClaimID, RunID: runID,
	})
	if err != nil {
		t.Fatalf("link: %v", err)
	}
	if got.RunID == nil || *got.RunID != runID {
		t.Errorf("returned item RunID = %v, want %s", got.RunID, runID)
	}
	back, err := repo.GetCampaignItem(context.Background(), item.ID)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if back.RunID == nil || *back.RunID != runID {
		t.Errorf("committed RunID = %v, want %s", back.RunID, runID)
	}
	if row := readClaimRow(t, pool, item.ID); row != nil {
		t.Errorf("claim row after the link = %+v, want consumed", row)
	}
}

func TestLinkClaimedCampaignItemRun_ClaimReplaced_RefusedClaimLost(t *testing.T) {
	pool, repo, runs, item := newClaimFixture(t)
	a := claimAt(t, repo, item.ID, campaign.StarterOperator, claimT0)
	b := claimAt(t, repo, item.ID, campaign.StarterCampaignDriver, a.ExpiresAt)
	runID := makeClaimRun(t, runs)
	got, err := repo.LinkClaimedCampaignItemRun(context.Background(), campaign.LinkClaimedCampaignItemRunParams{
		ItemID: item.ID, ClaimID: a.ClaimID, RunID: runID,
	})
	ce := wantStartConflict(t, err, campaign.StartConflictClaimLost)
	if got != nil {
		t.Errorf("item = %+v, want nil on a refusal", got)
	}
	if !sameClaim(ce.HeldClaim, b) {
		t.Errorf("HeldClaim = %+v, want the replacer %+v", ce.HeldClaim, b)
	}
	back, err := repo.GetCampaignItem(context.Background(), item.ID)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if back.RunID != nil {
		t.Errorf("committed RunID = %v, want the item still unlinked", *back.RunID)
	}
	if row := readClaimRow(t, pool, item.ID); !sameClaim(row, b) {
		t.Errorf("claim row = %+v, want B's row intact %+v", row, b)
	}
}

func TestLinkClaimedCampaignItemRun_Released_RefusedClaimLostWithNoHolder(t *testing.T) {
	_, repo, runs, item := newClaimFixture(t)
	a := claimAt(t, repo, item.ID, campaign.StarterOperator, claimT0)
	if err := repo.ReleaseCampaignItemStart(context.Background(), item.ID, a.ClaimID); err != nil {
		t.Fatalf("release: %v", err)
	}
	_, err := repo.LinkClaimedCampaignItemRun(context.Background(), campaign.LinkClaimedCampaignItemRunParams{
		ItemID: item.ID, ClaimID: a.ClaimID, RunID: makeClaimRun(t, runs),
	})
	ce := wantStartConflict(t, err, campaign.StartConflictClaimLost)
	if ce.HeldClaim != nil {
		t.Errorf("HeldClaim = %+v, want nil (no claim holds the item)", ce.HeldClaim)
	}
	back, err := repo.GetCampaignItem(context.Background(), item.ID)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if back.RunID != nil {
		t.Errorf("committed RunID = %v, want unlinked", *back.RunID)
	}
}

// TestLinkClaimedCampaignItemRun_ExpiredUnreplaced_StillLinks: expiry only
// lets ANOTHER claimant replace a claim; the link itself never checks the
// clock, so a starter whose claim expired but was not replaced still links.
func TestLinkClaimedCampaignItemRun_ExpiredUnreplaced_StillLinks(t *testing.T) {
	pool, repo, runs, item := newClaimFixture(t)
	// Claimed long ago: the claim is expired against any present clock.
	claim := claimAt(t, repo, item.ID, campaign.StarterCampaignDriver, claimT0.Add(-24*time.Hour))
	runID := makeClaimRun(t, runs)
	got, err := repo.LinkClaimedCampaignItemRun(context.Background(), campaign.LinkClaimedCampaignItemRunParams{
		ItemID: item.ID, ClaimID: claim.ClaimID, RunID: runID,
	})
	if err != nil {
		t.Fatalf("link through an expired, unreplaced claim: %v", err)
	}
	if got.RunID == nil || *got.RunID != runID {
		t.Errorf("RunID = %v, want %s", got.RunID, runID)
	}
	if row := readClaimRow(t, pool, item.ID); row != nil {
		t.Errorf("claim row = %+v, want consumed", row)
	}
}

func TestLinkClaimedCampaignItemRun_LinkedToOther_RefusedLinked(t *testing.T) {
	pool, repo, runs, item := newClaimFixture(t)
	claim := claimAt(t, repo, item.ID, campaign.StarterOperator, claimT0)
	// An unconditional link (the reconcile relink path) lands first.
	other := makeClaimRun(t, runs)
	if _, err := repo.SetCampaignItemRun(context.Background(), item.ID, &other); err != nil {
		t.Fatalf("link other: %v", err)
	}
	_, err := repo.LinkClaimedCampaignItemRun(context.Background(), campaign.LinkClaimedCampaignItemRunParams{
		ItemID: item.ID, ClaimID: claim.ClaimID, RunID: makeClaimRun(t, runs),
	})
	ce := wantStartConflict(t, err, campaign.StartConflictLinked)
	if ce.WinnerRunID == nil || *ce.WinnerRunID != other {
		t.Errorf("WinnerRunID = %v, want %s", ce.WinnerRunID, other)
	}
	back, err := repo.GetCampaignItem(context.Background(), item.ID)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if back.RunID == nil || *back.RunID != other {
		t.Errorf("committed RunID = %v, want unchanged %s", back.RunID, other)
	}
	if row := readClaimRow(t, pool, item.ID); !sameClaim(row, claim) {
		t.Errorf("claim row = %+v, want untouched %+v (the refusal writes nothing)", row, claim)
	}
}

func TestLinkClaimedCampaignItemRun_SameRun_Idempotent(t *testing.T) {
	pool, repo, runs, item := newClaimFixture(t)
	claim := claimAt(t, repo, item.ID, campaign.StarterOperator, claimT0)
	runID := makeClaimRun(t, runs)
	p := campaign.LinkClaimedCampaignItemRunParams{ItemID: item.ID, ClaimID: claim.ClaimID, RunID: runID}
	if _, err := repo.LinkClaimedCampaignItemRun(context.Background(), p); err != nil {
		t.Fatalf("first link: %v", err)
	}
	got, err := repo.LinkClaimedCampaignItemRun(context.Background(), p)
	if err != nil {
		t.Fatalf("re-link of the same run: %v, want idempotent success", err)
	}
	if got.RunID == nil || *got.RunID != runID {
		t.Errorf("re-link RunID = %v, want %s", got.RunID, runID)
	}
	if row := readClaimRow(t, pool, item.ID); row != nil {
		t.Errorf("claim row = %+v, want none", row)
	}
}

func TestLinkClaimedCampaignItemRun_MissingItemAndInvalidParams(t *testing.T) {
	_, repo, runs, item := newClaimFixture(t)
	runID := makeClaimRun(t, runs)
	if _, err := repo.LinkClaimedCampaignItemRun(context.Background(), campaign.LinkClaimedCampaignItemRunParams{
		ItemID: uuid.New(), ClaimID: uuid.New(), RunID: runID,
	}); !errors.Is(err, campaign.ErrNotFound) {
		t.Errorf("link on a missing item: err = %v, want ErrNotFound", err)
	}
	for name, p := range map[string]campaign.LinkClaimedCampaignItemRunParams{
		"nil claim id": {ItemID: item.ID, RunID: runID},
		"nil run id":   {ItemID: item.ID, ClaimID: uuid.New()},
	} {
		if _, err := repo.LinkClaimedCampaignItemRun(context.Background(), p); !errors.Is(err, campaign.ErrStartClaimInvalid) {
			t.Errorf("%s: err = %v, want ErrStartClaimInvalid", name, err)
		}
	}
	back, err := repo.GetCampaignItem(context.Background(), item.ID)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if back.RunID != nil {
		t.Errorf("RunID = %v, want unlinked after refused links", *back.RunID)
	}
}

func TestReleaseCampaignItemStart_OwnClaim_FreesTheItem(t *testing.T) {
	pool, repo, _, item := newClaimFixture(t)
	a := claimAt(t, repo, item.ID, campaign.StarterOperator, claimT0)
	if err := repo.ReleaseCampaignItemStart(context.Background(), item.ID, a.ClaimID); err != nil {
		t.Fatalf("release: %v", err)
	}
	if row := readClaimRow(t, pool, item.ID); row != nil {
		t.Errorf("claim row after release = %+v, want none", row)
	}
	// A new claim at the SAME instant succeeds: the item is free.
	b := claimAt(t, repo, item.ID, campaign.StarterCampaignDriver, claimT0)
	if b.ClaimID == a.ClaimID {
		t.Errorf("post-release claim reused claim id %s", a.ClaimID)
	}
}

func TestReleaseCampaignItemStart_OtherHoldersClaim_Untouched(t *testing.T) {
	pool, repo, _, item := newClaimFixture(t)
	a := claimAt(t, repo, item.ID, campaign.StarterOperator, claimT0)
	b := claimAt(t, repo, item.ID, campaign.StarterCampaignDriver, a.ExpiresAt)
	// A's stale token released after B's reclaim must not free B's claim.
	if err := repo.ReleaseCampaignItemStart(context.Background(), item.ID, a.ClaimID); err != nil {
		t.Fatalf("stale release: %v", err)
	}
	if row := readClaimRow(t, pool, item.ID); !sameClaim(row, b) {
		t.Errorf("claim row after a stale release = %+v, want B's row intact %+v", row, b)
	}
}

func TestReleaseCampaignItemStart_Idempotent(t *testing.T) {
	pool, repo, _, item := newClaimFixture(t)
	if err := repo.ReleaseCampaignItemStart(context.Background(), item.ID, uuid.New()); err != nil {
		t.Errorf("release with no claim: %v, want nil", err)
	}
	a := claimAt(t, repo, item.ID, campaign.StarterOperator, claimT0)
	for i := 0; i < 2; i++ {
		if err := repo.ReleaseCampaignItemStart(context.Background(), item.ID, a.ClaimID); err != nil {
			t.Errorf("release #%d: %v, want nil", i+1, err)
		}
	}
	if row := readClaimRow(t, pool, item.ID); row != nil {
		t.Errorf("claim row = %+v, want none", row)
	}
}

// TestReleaseCampaignItemStart_DatabaseError_Surfaced: a release whose DELETE
// cannot run returns the wrapped error (the caller logs it; the claim then
// lapses by TTL) and leaves the claim row in place.
func TestReleaseCampaignItemStart_DatabaseError_Surfaced(t *testing.T) {
	pool, repo, _, item := newClaimFixture(t)
	a := claimAt(t, repo, item.ID, campaign.StarterOperator, claimT0)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := repo.ReleaseCampaignItemStart(ctx, item.ID, a.ClaimID); !errors.Is(err, context.Canceled) {
		t.Fatalf("release on a cancelled context: err = %v, want context.Canceled", err)
	}
	if row := readClaimRow(t, pool, item.ID); !sameClaim(row, a) {
		t.Errorf("claim row after a failed release = %+v, want the claim intact %+v", row, a)
	}
}

// holdItemLock opens a transaction holding item's row lock, rolled back at
// test cleanup.
func holdItemLock(t *testing.T, pool *pgxpool.Pool, itemID uuid.UUID) {
	t.Helper()
	tx, err := pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	if _, err := tx.Exec(context.Background(), `SELECT id FROM campaign_items WHERE id = $1 FOR UPDATE`, itemID); err != nil {
		t.Fatalf("lock item: %v", err)
	}
}

// cancelDuringLockWait runs call on a cancellable context, cancels it once the
// call is blocked on the item row lock, and returns the call's error.
func cancelDuringLockWait(t *testing.T, pool *pgxpool.Pool, call func(context.Context) error) error {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	var callErr error
	go func() {
		defer close(done)
		callErr = call(ctx)
	}()
	waitForLockWaiter(t, pool, done)
	cancel()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("call did not return within 15s of its context being cancelled")
	}
	return callErr
}

// TestClaimCampaignItemStart_LockWaitCancelled_ErrorsWithoutClaiming: a claim
// whose context ends while it waits on the item row lock returns the lock
// error — not a conflict — and writes no claim row.
func TestClaimCampaignItemStart_LockWaitCancelled_ErrorsWithoutClaiming(t *testing.T) {
	pool, repo, _, item := newClaimFixture(t)
	holdItemLock(t, pool, item.ID)
	err := cancelDuringLockWait(t, pool, func(ctx context.Context) error {
		_, err := repo.ClaimCampaignItemStart(ctx, campaign.ClaimCampaignItemStartParams{
			ItemID: item.ID, Starter: campaign.StarterOperator, Clock: fixedClock(claimT0),
		})
		return err
	})
	var ce *campaign.ItemStartConflictError
	if err == nil || errors.As(err, &ce) || !strings.Contains(err.Error(), "lock campaign item") {
		t.Fatalf("claim cancelled in the lock wait: err = %v, want a wrapped lock error, not a conflict", err)
	}
	if row := readClaimRow(t, pool, item.ID); row != nil {
		t.Errorf("claim row = %+v, want none after a cancelled claim", row)
	}
}

// TestLinkClaimedCampaignItemRun_LockWaitCancelled_KeepsClaimUnlinked: a link
// whose context ends while it waits on the item row lock returns the lock
// error, consumes no claim and links no run.
func TestLinkClaimedCampaignItemRun_LockWaitCancelled_KeepsClaimUnlinked(t *testing.T) {
	pool, repo, runs, item := newClaimFixture(t)
	runID := makeClaimRun(t, runs)
	a := claimAt(t, repo, item.ID, campaign.StarterOperator, claimT0)
	holdItemLock(t, pool, item.ID)
	err := cancelDuringLockWait(t, pool, func(ctx context.Context) error {
		_, err := repo.LinkClaimedCampaignItemRun(ctx, campaign.LinkClaimedCampaignItemRunParams{
			ItemID: item.ID, ClaimID: a.ClaimID, RunID: runID,
		})
		return err
	})
	var ce *campaign.ItemStartConflictError
	if err == nil || errors.As(err, &ce) || !strings.Contains(err.Error(), "lock campaign item") {
		t.Fatalf("link cancelled in the lock wait: err = %v, want a wrapped lock error, not a conflict", err)
	}
	if row := readClaimRow(t, pool, item.ID); !sameClaim(row, a) {
		t.Errorf("claim row = %+v, want the claim intact %+v", row, a)
	}
	var linked *uuid.UUID
	if err := pool.QueryRow(context.Background(), `SELECT run_id FROM campaign_items WHERE id = $1`, item.ID).Scan(&linked); err != nil {
		t.Fatalf("read run_id: %v", err)
	}
	if linked != nil {
		t.Errorf("run_id = %s, want unlinked after a cancelled link", *linked)
	}
}

// TestItemStartBudgets_WorstCasePostClaimUnderHalfTheClaimTTL pins the claim
// invariant (#5266 U3): everything a live starter does after claiming — the
// start budget plus, on its own context, the cleanup budget — fits in under
// half the claim TTL, so a live starter's claim cannot be stolen.
func TestItemStartBudgets_WorstCasePostClaimUnderHalfTheClaimTTL(t *testing.T) {
	if campaign.ItemStartCleanupBudget <= 0 {
		t.Errorf("ItemStartCleanupBudget = %s, want > 0", campaign.ItemStartCleanupBudget)
	}
	if campaign.ItemStartBudget <= 0 {
		t.Errorf("ItemStartBudget = %s, want > 0", campaign.ItemStartBudget)
	}
	sum := campaign.ItemStartBudget + campaign.ItemStartCleanupBudget
	if campaign.ItemStartWorstCase != sum {
		t.Errorf("ItemStartWorstCase = %s, want ItemStartBudget + ItemStartCleanupBudget = %s", campaign.ItemStartWorstCase, sum)
	}
	if sum >= campaign.ItemStartClaimTTL/2 {
		t.Errorf("ItemStartBudget (%s) + ItemStartCleanupBudget (%s) = %s, want < ItemStartClaimTTL/2 (%s)",
			campaign.ItemStartBudget, campaign.ItemStartCleanupBudget, sum, campaign.ItemStartClaimTTL/2)
	}
}

func TestStartClaim_Covers(t *testing.T) {
	c := &campaign.StartClaim{ClaimedAt: claimT0, ExpiresAt: claimT0.Add(campaign.ItemStartClaimTTL)}
	need := campaign.ItemStartWorstCase
	for name, tc := range map[string]struct {
		claim *campaign.StartClaim
		now   time.Time
		want  bool
	}{
		"fresh claim":                {c, claimT0, true},
		"exactly the need remaining": {c, c.ExpiresAt.Add(-need), true},
		"one ns short":               {c, c.ExpiresAt.Add(-need + time.Nanosecond), false},
		"expired":                    {c, c.ExpiresAt.Add(time.Minute), false},
		"nil claim":                  {nil, claimT0, false},
	} {
		if got := tc.claim.Covers(tc.now, need); got != tc.want {
			t.Errorf("%s: Covers = %v, want %v", name, got, tc.want)
		}
	}
}

func TestItemStartConflictError_NamesItemReasonAndHolder(t *testing.T) {
	item, winner, claimID := uuid.New(), uuid.New(), uuid.New()
	linked := (&campaign.ItemStartConflictError{ItemID: item, Reason: campaign.StartConflictLinked, WinnerRunID: &winner}).Error()
	for _, want := range []string{item.String(), "linked", winner.String()} {
		if !strings.Contains(linked, want) {
			t.Errorf("linked Error() = %q, want it to name %q", linked, want)
		}
	}
	held := (&campaign.ItemStartConflictError{ItemID: item, Reason: campaign.StartConflictClaimed,
		HeldClaim: &campaign.StartClaim{ClaimID: claimID, Starter: campaign.StarterCampaignDriver, ExpiresAt: claimT0}}).Error()
	for _, want := range []string{item.String(), "claimed", claimID.String(), campaign.StarterCampaignDriver} {
		if !strings.Contains(held, want) {
			t.Errorf("claimed Error() = %q, want it to name %q", held, want)
		}
	}
}
