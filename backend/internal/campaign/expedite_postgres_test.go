package campaign_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/campaign"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// holdNow is microsecond-precision so a Postgres TIMESTAMPTZ round-trip is
// exact and read-back can be compared with Equal.
var holdNow = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

func hotfix(hold bool) *campaign.ExpediteParams {
	e := &campaign.ExpediteParams{Kind: campaign.ExpediteKindHotfix, Workflow: "hotfix_change", Reason: "prod down"}
	if hold {
		e.Hold = &campaign.DispatchHoldParams{HeldAt: holdNow, ExpiresAt: holdNow.Add(campaign.MaxDispatchHold)}
	}
	return e
}

func appendHotfix(t *testing.T, repo campaign.Repository, campaignID uuid.UUID, ref string, e *campaign.ExpediteParams) *campaign.AppendOutcome {
	t.Helper()
	out, err := repo.AppendCampaignItems(context.Background(), campaign.AppendCampaignItemsParams{
		CampaignID: campaignID,
		NewItems:   []campaign.CreateCampaignItemParams{{IssueRef: ref}},
		Expedite:   e,
		Record:     func(context.Context, campaign.AppendOutcome) error { return nil },
	})
	if err != nil {
		t.Fatalf("expedited append %s: %v", ref, err)
	}
	return out
}

// countExpedites reads the committed campaign_item_expedites rows.
func countExpedites(t *testing.T, pool *pgxpool.Pool, campaignID uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM campaign_item_expedites WHERE campaign_id = $1`, campaignID).Scan(&n); err != nil {
		t.Fatalf("count expedites: %v", err)
	}
	return n
}

func readHold(t *testing.T, repo campaign.Repository, campaignID uuid.UUID) *campaign.DispatchHold {
	t.Helper()
	h, err := repo.GetDispatchHold(context.Background(), campaignID)
	if err != nil {
		t.Fatalf("get dispatch hold: %v", err)
	}
	return h
}

// TestAppendCampaignItems_ExpeditedHeadsQueue pins the head position: on a
// roster of two pending items (positions 0 and 1) the expedited item lands at
// -1 — a NEGATIVE queue_position, which the column admits — is listed first,
// and is the engine's Eligible[0]. A second expedite goes one lower still, and
// on an empty roster the head is 0.
func TestAppendCampaignItems_ExpeditedHeadsQueue(t *testing.T) {
	repo, _, c, _ := appendFixture(t,
		campaign.CreateCampaignItemParams{IssueRef: "issue:10"},
		campaign.CreateCampaignItemParams{IssueRef: "issue:11"},
	)
	out := appendHotfix(t, repo, c.ID, "issue:20", hotfix(false))
	if len(out.Created) != 1 || out.Created[0].Position != -1 {
		t.Fatalf("created = %+v, want one item at position -1", out.Created)
	}
	if out.Hold != nil {
		t.Errorf("outcome hold = %+v, want nil (no hold asked)", out.Hold)
	}
	items, err := repo.ListCampaignItemsForCampaign(context.Background(), c.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 3 || items[0].IssueRef != "issue:20" || items[0].Position != -1 {
		t.Fatalf("listing head = %+v, want issue:20 at -1", items[0])
	}
	if el := campaign.NextEligible(items).Eligible; len(el) != 3 || el[0] != "issue:20" {
		t.Fatalf("Eligible = %v, want issue:20 first", el)
	}

	appendHotfix(t, repo, c.ID, "issue:21", hotfix(false))
	if got := readItems(t, repo, c.ID)["issue:21"].Position; got != -2 {
		t.Errorf("second expedite position = %d, want -2", got)
	}

	empty := makeCampaign(t, repo)
	if out := appendHotfix(t, repo, empty.ID, "issue:30", hotfix(false)); out.Created[0].Position != 0 {
		t.Errorf("expedite on an empty roster position = %d, want 0", out.Created[0].Position)
	}
}

// TestAppendCampaignItems_ExpediteAttachedOnEveryReadPath pins that the
// outcome, GetCampaignItem, ListCampaignItemsForCampaign and
// ListCampaignItemsForRun all carry the expedite record, and that an ordinary
// item carries none.
func TestAppendCampaignItems_ExpediteAttachedOnEveryReadPath(t *testing.T) {
	repo, pool, c, seeded := appendFixture(t, campaign.CreateCampaignItemParams{IssueRef: "issue:10"})
	ctx := context.Background()
	out := appendHotfix(t, repo, c.ID, "issue:20", hotfix(false))
	hot := out.Created[0]
	want := func(where string, e *campaign.Expedite) {
		t.Helper()
		if e == nil || e.Kind != campaign.ExpediteKindHotfix || e.Workflow != "hotfix_change" || e.Reason != "prod down" || e.CreatedAt.IsZero() {
			t.Errorf("%s: expedite = %+v, want {hotfix, hotfix_change, prod down, created_at set}", where, e)
		}
	}
	want("outcome", hot.Expedite)

	got, err := repo.GetCampaignItem(ctx, hot.ID)
	if err != nil {
		t.Fatalf("get item: %v", err)
	}
	want("GetCampaignItem", got.Expedite)
	plain, err := repo.GetCampaignItem(ctx, seeded["issue:10"].ID)
	if err != nil {
		t.Fatalf("get plain item: %v", err)
	}
	if plain.Expedite != nil {
		t.Errorf("ordinary item expedite = %+v, want nil", plain.Expedite)
	}

	listed := readItems(t, repo, c.ID)
	want("ListCampaignItemsForCampaign", listed["issue:20"].Expedite)
	if listed["issue:10"].Expedite != nil {
		t.Errorf("listed ordinary item expedite = %+v, want nil", listed["issue:10"].Expedite)
	}

	r, err := run.NewPostgresRepository(pool).CreateRun(ctx, run.CreateRunParams{
		Repo: "kuhlman-labs/fishhawk", WorkflowID: "hotfix_change", WorkflowSHA: "deadbeef", TriggerSource: run.TriggerCLI,
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	if _, err := repo.SetCampaignItemRun(ctx, hot.ID, &r.ID); err != nil {
		t.Fatalf("link run: %v", err)
	}
	byRun, err := repo.ListCampaignItemsForRun(ctx, r.ID)
	if err != nil {
		t.Fatalf("list for run: %v", err)
	}
	if len(byRun) != 1 {
		t.Fatalf("items for run = %d, want 1", len(byRun))
	}
	want("ListCampaignItemsForRun", byRun[0].Expedite)
}

// TestAppendCampaignItems_HoldPlacedAndRead pins the hold write: the outcome
// and GetDispatchHold both name the expedited item with the caller's clock
// stamps, and a campaign with no hold reads (nil, nil).
func TestAppendCampaignItems_HoldPlacedAndRead(t *testing.T) {
	repo, _, c, _ := appendFixture(t, campaign.CreateCampaignItemParams{IssueRef: "issue:10"})
	if h := readHold(t, repo, c.ID); h != nil {
		t.Fatalf("hold before any hotfix = %+v, want nil", h)
	}
	var recorded campaign.AppendOutcome
	out, err := repo.AppendCampaignItems(context.Background(), campaign.AppendCampaignItemsParams{
		CampaignID: c.ID,
		NewItems:   []campaign.CreateCampaignItemParams{{IssueRef: "issue:20"}},
		Expedite:   hotfix(true),
		Record:     func(_ context.Context, o campaign.AppendOutcome) error { recorded = o; return nil },
	})
	if err != nil {
		t.Fatalf("append with hold: %v", err)
	}
	hotID := out.Created[0].ID
	for where, h := range map[string]*campaign.DispatchHold{"outcome": out.Hold, "recorder": recorded.Hold, "GetDispatchHold": readHold(t, repo, c.ID)} {
		if h == nil || h.CampaignID != c.ID || h.HotfixItemID != hotID || !h.HeldAt.Equal(holdNow) || !h.ExpiresAt.Equal(holdNow.Add(campaign.MaxDispatchHold)) {
			t.Errorf("%s hold = %+v, want campaign %s item %s held %s..%s", where, h, c.ID, hotID, holdNow, holdNow.Add(campaign.MaxDispatchHold))
		}
	}
	if len(recorded.Created) != 1 || recorded.Created[0].Expedite == nil {
		t.Errorf("recorder saw created = %+v, want the expedited item with its expedite record", recorded.Created)
	}
}

// TestAppendCampaignItems_SecondHoldRefused pins the active-hold refusal: a
// second expedite asking for a hold is ErrDispatchHoldActive and writes no
// item, no expedite row and no change to the hold; a second expedite WITHOUT a
// hold is admitted during the first one's hold.
func TestAppendCampaignItems_SecondHoldRefused(t *testing.T) {
	repo, pool, c, _ := appendFixture(t, campaign.CreateCampaignItemParams{IssueRef: "issue:10"})
	first := appendHotfix(t, repo, c.ID, "issue:20", hotfix(true))

	second := hotfix(true)
	second.Hold.HeldAt = holdNow.Add(time.Minute)
	second.Hold.ExpiresAt = holdNow.Add(time.Hour)
	_, err := repo.AppendCampaignItems(context.Background(), campaign.AppendCampaignItemsParams{
		CampaignID: c.ID,
		NewItems:   []campaign.CreateCampaignItemParams{{IssueRef: "issue:21"}},
		Expedite:   second,
		Record:     func(context.Context, campaign.AppendOutcome) error { return nil },
	})
	if !errors.Is(err, campaign.ErrDispatchHoldActive) {
		t.Fatalf("second hold err = %v, want ErrDispatchHoldActive", err)
	}
	if n := len(readItems(t, repo, c.ID)); n != 2 {
		t.Errorf("items after the refusal = %d, want 2 (nothing written)", n)
	}
	if n := countExpedites(t, pool, c.ID); n != 1 {
		t.Errorf("expedite rows after the refusal = %d, want 1", n)
	}
	if h := readHold(t, repo, c.ID); h == nil || h.HotfixItemID != first.Created[0].ID || !h.ExpiresAt.Equal(holdNow.Add(campaign.MaxDispatchHold)) {
		t.Errorf("hold after the refusal = %+v, want the first hotfix's hold unchanged", h)
	}

	if out := appendHotfix(t, repo, c.ID, "issue:22", hotfix(false)); out.Hold != nil || out.Created[0].Expedite == nil {
		t.Errorf("second expedite without a hold = %+v, want an expedited item and no new hold", out)
	}
}

// TestAppendCampaignItems_ExpediteRecordFailureWritesNothing pins
// record-before-commit on the expedited append: a failing recorder leaves no
// item, no expedite row and no hold in COMMITTED state.
func TestAppendCampaignItems_ExpediteRecordFailureWritesNothing(t *testing.T) {
	repo, pool, c, _ := appendFixture(t, campaign.CreateCampaignItemParams{IssueRef: "issue:10"})
	boom := errors.New("audit down")
	_, err := repo.AppendCampaignItems(context.Background(), campaign.AppendCampaignItemsParams{
		CampaignID: c.ID,
		NewItems:   []campaign.CreateCampaignItemParams{{IssueRef: "issue:20"}},
		Expedite:   hotfix(true),
		Record:     func(context.Context, campaign.AppendOutcome) error { return boom },
	})
	if !errors.Is(err, campaign.ErrAppendUnrecorded) || !errors.Is(err, boom) {
		t.Fatalf("err = %v, want ErrAppendUnrecorded wrapping the record error", err)
	}
	if n := len(readItems(t, repo, c.ID)); n != 1 {
		t.Errorf("items = %d, want 1 (the expedited item rolled back)", n)
	}
	if n := countExpedites(t, pool, c.ID); n != 0 {
		t.Errorf("expedite rows = %d, want 0", n)
	}
	if h := readHold(t, repo, c.ID); h != nil {
		t.Errorf("hold = %+v, want nil (rolled back)", h)
	}
}

// TestAppendCampaignItems_InvalidExpediteWritesNothing pins that the adapter
// applies ValidateExpedite under the lock: two expedited items refuse with
// ErrExpediteInvalid and nothing is written.
func TestAppendCampaignItems_InvalidExpediteWritesNothing(t *testing.T) {
	repo, pool, c, _ := appendFixture(t, campaign.CreateCampaignItemParams{IssueRef: "issue:10"})
	_, err := repo.AppendCampaignItems(context.Background(), campaign.AppendCampaignItemsParams{
		CampaignID: c.ID,
		NewItems:   []campaign.CreateCampaignItemParams{{IssueRef: "issue:20"}, {IssueRef: "issue:21"}},
		Expedite:   hotfix(true),
	})
	if !errors.Is(err, campaign.ErrExpediteInvalid) {
		t.Fatalf("err = %v, want ErrExpediteInvalid", err)
	}
	if n := len(readItems(t, repo, c.ID)); n != 1 {
		t.Errorf("items = %d, want 1", n)
	}
	if n := countExpedites(t, pool, c.ID); n != 0 || readHold(t, repo, c.ID) != nil {
		t.Errorf("expedite rows = %d / hold present, want none", n)
	}
}

func releaseParams(c, item uuid.UUID, rec func(context.Context, campaign.DispatchHold) error) campaign.ReleaseDispatchHoldParams {
	return campaign.ReleaseDispatchHoldParams{CampaignID: c, HotfixItemID: item, Reason: campaign.HoldReleasedHotfixTerminal, Record: rec}
}

// TestReleaseDispatchHold_DeletesAndRecords pins the happy path: the recorder
// sees the hold, the call returns it, and the row is gone afterwards.
func TestReleaseDispatchHold_DeletesAndRecords(t *testing.T) {
	repo, _, c, _ := appendFixture(t, campaign.CreateCampaignItemParams{IssueRef: "issue:10"})
	hot := appendHotfix(t, repo, c.ID, "issue:20", hotfix(true)).Created[0]
	var seen []campaign.DispatchHold
	got, err := repo.ReleaseDispatchHold(context.Background(), releaseParams(c.ID, hot.ID, func(_ context.Context, h campaign.DispatchHold) error {
		seen = append(seen, h)
		return nil
	}))
	if err != nil {
		t.Fatalf("release: %v", err)
	}
	if got == nil || got.HotfixItemID != hot.ID || len(seen) != 1 || seen[0] != *got {
		t.Fatalf("released = %+v, recorder saw %+v, want the hold once", got, seen)
	}
	if h := readHold(t, repo, c.ID); h != nil {
		t.Errorf("hold after release = %+v, want nil", h)
	}
}

// TestListDispatchHolds_AcrossCampaigns pins the cross-campaign read the
// host-dispatch guard's unresolved-ancestry arm uses: no rows lists none, two
// campaigns' holds list oldest held_at first, and a released hold drops out.
func TestListDispatchHolds_AcrossCampaigns(t *testing.T) {
	repo, _, c, _ := appendFixture(t, campaign.CreateCampaignItemParams{IssueRef: "issue:10"})
	ctx := context.Background()
	if hs, err := repo.ListDispatchHolds(ctx); err != nil || len(hs) != 0 {
		t.Fatalf("holds on an empty table = %+v, %v; want none", hs, err)
	}
	hot := appendHotfix(t, repo, c.ID, "issue:20", hotfix(true)).Created[0]
	other := makeCampaign(t, repo)
	early := hotfix(true)
	early.Hold.HeldAt = holdNow.Add(-time.Minute)
	early.Hold.ExpiresAt = early.Hold.HeldAt.Add(campaign.MaxDispatchHold)
	otherHot := appendHotfix(t, repo, other.ID, "issue:30", early).Created[0]
	hs, err := repo.ListDispatchHolds(ctx)
	if err != nil {
		t.Fatalf("list holds: %v", err)
	}
	if len(hs) != 2 || hs[0].CampaignID != other.ID || hs[0].HotfixItemID != otherHot.ID ||
		!hs[0].HeldAt.Equal(early.Hold.HeldAt) || hs[1].CampaignID != c.ID || hs[1].HotfixItemID != hot.ID {
		t.Fatalf("holds = %+v, want %s's then %s's", hs, other.ID, c.ID)
	}
	if _, err := repo.ReleaseDispatchHold(ctx, releaseParams(other.ID, otherHot.ID,
		func(context.Context, campaign.DispatchHold) error { return nil })); err != nil {
		t.Fatalf("release: %v", err)
	}
	if hs, err := repo.ListDispatchHolds(ctx); err != nil || len(hs) != 1 || hs[0].CampaignID != c.ID {
		t.Fatalf("holds after a release = %+v, %v; want only %s's", hs, err, c.ID)
	}
}

// TestReleaseDispatchHold_NoOps pins the idempotent no-ops: a hold naming a
// different item, a campaign with no hold, and a missing campaign return
// (nil, nil) without calling the recorder, and the mismatched hold survives.
func TestReleaseDispatchHold_NoOps(t *testing.T) {
	repo, _, c, seeded := appendFixture(t, campaign.CreateCampaignItemParams{IssueRef: "issue:10"})
	hot := appendHotfix(t, repo, c.ID, "issue:20", hotfix(true)).Created[0]
	unheld := makeCampaign(t, repo)
	calls := 0
	rec := func(context.Context, campaign.DispatchHold) error { calls++; return nil }
	for name, p := range map[string]campaign.ReleaseDispatchHoldParams{
		"mismatched hotfix item": releaseParams(c.ID, seeded["issue:10"].ID, rec),
		"no hold":                releaseParams(unheld.ID, hot.ID, rec),
		"missing campaign":       releaseParams(uuid.New(), hot.ID, rec),
	} {
		got, err := repo.ReleaseDispatchHold(context.Background(), p)
		if err != nil || got != nil {
			t.Errorf("%s: release = (%+v, %v), want (nil, nil)", name, got, err)
		}
	}
	if calls != 0 {
		t.Errorf("recorder calls = %d, want 0", calls)
	}
	if h := readHold(t, repo, c.ID); h == nil || h.HotfixItemID != hot.ID {
		t.Errorf("hold after the mismatched release = %+v, want it intact", h)
	}
}

// TestReleaseDispatchHold_RecordFailureKeepsHold pins record-before-commit on
// the release: a failing recorder, a nil recorder and an unknown reason all
// leave the hold row in COMMITTED state.
func TestReleaseDispatchHold_RecordFailureKeepsHold(t *testing.T) {
	repo, _, c, _ := appendFixture(t, campaign.CreateCampaignItemParams{IssueRef: "issue:10"})
	hot := appendHotfix(t, repo, c.ID, "issue:20", hotfix(true)).Created[0]
	boom := errors.New("audit down")

	_, err := repo.ReleaseDispatchHold(context.Background(), releaseParams(c.ID, hot.ID, func(context.Context, campaign.DispatchHold) error { return boom }))
	if !errors.Is(err, campaign.ErrHoldReleaseUnrecorded) || !errors.Is(err, boom) {
		t.Fatalf("record failure err = %v, want ErrHoldReleaseUnrecorded wrapping it", err)
	}
	if h := readHold(t, repo, c.ID); h == nil || h.HotfixItemID != hot.ID {
		t.Fatalf("hold after a failed record = %+v, want it intact (rolled back)", h)
	}

	if _, err := repo.ReleaseDispatchHold(context.Background(), releaseParams(c.ID, hot.ID, nil)); !errors.Is(err, campaign.ErrHoldReleaseUnrecorded) {
		t.Errorf("nil recorder err = %v, want ErrHoldReleaseUnrecorded", err)
	}
	bad := releaseParams(c.ID, hot.ID, func(context.Context, campaign.DispatchHold) error { return nil })
	bad.Reason = "operator"
	if _, err := repo.ReleaseDispatchHold(context.Background(), bad); !errors.Is(err, campaign.ErrExpediteInvalid) {
		t.Errorf("unknown reason err = %v, want ErrExpediteInvalid", err)
	}
	if h := readHold(t, repo, c.ID); h == nil {
		t.Error("hold after a nil-recorder / unknown-reason release = nil, want it intact")
	}
}

// TestReleaseDispatchHold_ConcurrentReleaseOnce pins the lost-race no-op:
// concurrent releases of one hold record and return it exactly once.
func TestReleaseDispatchHold_ConcurrentReleaseOnce(t *testing.T) {
	repo, _, c, _ := appendFixture(t, campaign.CreateCampaignItemParams{IssueRef: "issue:10"})
	hot := appendHotfix(t, repo, c.ID, "issue:20", hotfix(true)).Created[0]
	const n = 6
	var (
		mu       sync.Mutex
		recorded int
		released int
		wg       sync.WaitGroup
	)
	rec := func(context.Context, campaign.DispatchHold) error {
		mu.Lock()
		recorded++
		mu.Unlock()
		return nil
	}
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := repo.ReleaseDispatchHold(context.Background(), releaseParams(c.ID, hot.ID, rec))
			if err != nil {
				errs <- err
				return
			}
			if got != nil {
				mu.Lock()
				released++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent release: %v", err)
	}
	if recorded != 1 || released != 1 {
		t.Errorf("recorded = %d, released = %d, want 1/1", recorded, released)
	}
}

// TestPostgres_ExpediteReadErrorFailsClosed pins that an unreadable expedite
// or hold table FAILS the read rather than reporting an ordinary item or no
// hold: a hold-exemption decision must never rest on a swallowed error. The
// table is renamed away in this test's own database clone.
func TestPostgres_ExpediteReadErrorFailsClosed(t *testing.T) {
	repo, pool, c, seeded := appendFixture(t, campaign.CreateCampaignItemParams{IssueRef: "issue:10"})
	ctx := context.Background()
	appendHotfix(t, repo, c.ID, "issue:20", hotfix(true))
	r, err := run.NewPostgresRepository(pool).CreateRun(ctx, run.CreateRunParams{
		Repo: "kuhlman-labs/fishhawk", WorkflowID: "feature_change", WorkflowSHA: "deadbeef", TriggerSource: run.TriggerCLI,
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	if _, err := repo.SetCampaignItemRun(ctx, seeded["issue:10"].ID, &r.ID); err != nil {
		t.Fatalf("link run: %v", err)
	}

	if _, err := pool.Exec(ctx, `ALTER TABLE campaign_item_expedites RENAME TO campaign_item_expedites_gone`); err != nil {
		t.Fatalf("rename expedites: %v", err)
	}
	if got, err := repo.GetCampaignItem(ctx, seeded["issue:10"].ID); err == nil {
		t.Errorf("GetCampaignItem with the expedite table gone = %+v, want an error", got)
	}
	if got, err := repo.ListCampaignItemsForCampaign(ctx, c.ID); err == nil {
		t.Errorf("ListCampaignItemsForCampaign with the expedite table gone = %d items, want an error", len(got))
	}
	if got, err := repo.ListCampaignItemsForRun(ctx, r.ID); err == nil {
		t.Errorf("ListCampaignItemsForRun with the expedite table gone = %d items, want an error", len(got))
	}
	if _, err := repo.AppendCampaignItems(ctx, campaign.AppendCampaignItemsParams{
		CampaignID: c.ID,
		NewItems:   []campaign.CreateCampaignItemParams{{IssueRef: "issue:30"}},
		Expedite:   hotfix(false),
		Record:     func(context.Context, campaign.AppendOutcome) error { return nil },
	}); err == nil {
		t.Error("expedited append with the expedite table gone succeeded, want an error")
	}

	if _, err := pool.Exec(ctx, `ALTER TABLE campaign_dispatch_holds RENAME TO campaign_dispatch_holds_gone`); err != nil {
		t.Fatalf("rename holds: %v", err)
	}
	if h, err := repo.GetDispatchHold(ctx, c.ID); err == nil {
		t.Errorf("GetDispatchHold with the hold table gone = %+v, want an error (never \"no hold\")", h)
	}
	if _, err := repo.ReleaseDispatchHold(ctx, releaseParams(c.ID, uuid.New(), func(context.Context, campaign.DispatchHold) error { return nil })); err == nil {
		t.Error("ReleaseDispatchHold with the hold table gone succeeded, want an error")
	}
}
