package repoacl_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/identity"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/postgres"
	"github.com/kuhlman-labs/fishhawk/backend/internal/repoacl"
	repoacldb "github.com/kuhlman-labs/fishhawk/backend/internal/repoacl/db"
	"github.com/kuhlman-labs/fishhawk/backend/internal/timescale"
)

// newStore stands up migration 0059 against a real Postgres. This round-trip is
// what proves the hand-written queries.sql.go shapes agree with the SQL — the
// db package is not sqlc-regenerated locally (established repo convention), so
// a column-order or type mismatch fails HERE rather than at compile time.
func newStore(t *testing.T) (repoacl.Store, *pgxpool.Pool) {
	t.Helper()
	url := pgtest.NewURL(t)
	if err := postgres.MigrateUp(url); err != nil {
		t.Fatalf("MigrateUp: %v", err)
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return repoacl.NewPostgresStore(pool), pool
}

// ensureAndUpsert mirrors what Mirror.Permission does on a miss: it creates the
// lockable purge-watermark row (EnsurePurgeGeneration) and captures the current
// generation, THEN performs the guarded upsert with that generation. The guarded
// upsert's INSERT ... SELECT FROM repo_acl_purge_watermarks is a no-op when the
// watermark row is absent (FOR SHARE on an absent row locks nothing and the
// SELECT yields zero rows), so every direct-store test path must ensure the row
// first.
func ensureAndUpsert(t *testing.T, store repoacl.Store, provider, subject, repo string, perm identity.Permission) {
	t.Helper()
	ctx := context.Background()
	gen, err := store.EnsurePurgeGeneration(ctx, provider, subject)
	if err != nil {
		t.Fatalf("EnsurePurgeGeneration(%s/%s): %v", provider, subject, err)
	}
	if err := store.Upsert(ctx, provider, subject, repo, perm, gen); err != nil {
		t.Fatalf("Upsert(%s/%s/%s): %v", provider, subject, repo, err)
	}
}

func TestRepoACLPostgres_UpsertGetRoundTrip(t *testing.T) {
	store, _ := newStore(t)
	ctx := context.Background()

	// A miss is found=false with NO error — never confused with a fault.
	if _, found, err := store.Get(ctx, "github", "octocat", "acme/app"); err != nil || found {
		t.Fatalf("Get(miss) = (found=%v, err=%v), want (false, nil)", found, err)
	}

	before := time.Now()
	ensureAndUpsert(t, store, "github", "octocat", "acme/app", identity.PermissionRead)
	entry, found, err := store.Get(ctx, "github", "octocat", "acme/app")
	if err != nil || !found {
		t.Fatalf("Get after upsert = (found=%v, err=%v), want (true, nil)", found, err)
	}
	if entry.Permission != identity.PermissionRead {
		t.Errorf("permission = %q, want %q", entry.Permission, identity.PermissionRead)
	}
	if entry.CheckedAt.Before(before.Add(-time.Minute)) || entry.CheckedAt.IsZero() {
		t.Errorf("checked_at = %v, want a now()-ish stamp", entry.CheckedAt)
	}
}

// The ON CONFLICT path: re-upserting the natural key updates the permission and
// REFRESHES checked_at rather than inserting a duplicate.
func TestRepoACLPostgres_UpsertConflictRefreshesPermissionAndCheckedAt(t *testing.T) {
	store, pool := newStore(t)
	ctx := context.Background()

	ensureAndUpsert(t, store, "github", "octocat", "acme/app", identity.PermissionAdmin)
	first, _, err := store.Get(ctx, "github", "octocat", "acme/app")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	// Postgres now() is transaction-start time, so a same-instant re-upsert can
	// tie. Sleep past the timestamp resolution the assertion depends on.
	time.Sleep(5 * time.Millisecond)

	ensureAndUpsert(t, store, "github", "octocat", "acme/app", identity.PermissionNone)
	second, found, err := store.Get(ctx, "github", "octocat", "acme/app")
	if err != nil || !found {
		t.Fatalf("Get after conflict = (found=%v, err=%v)", found, err)
	}
	if second.Permission != identity.PermissionNone {
		t.Errorf("permission = %q, want %q (conflict must update)", second.Permission, identity.PermissionNone)
	}
	if !second.CheckedAt.After(first.CheckedAt) {
		t.Errorf("checked_at %v not after %v — the TTL clock must be refreshed on every upsert", second.CheckedAt, first.CheckedAt)
	}

	var count int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM repo_acl_entries WHERE provider = 'github' AND subject = 'octocat' AND repo = 'acme/app'`,
	).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Errorf("row count = %d, want 1 (UNIQUE(provider, subject, repo))", count)
	}
}

// The login purge deletes exactly one identity's rows — not another subject's,
// and not the same subject under a different provider.
func TestRepoACLPostgres_DeleteForSubjectIsScoped(t *testing.T) {
	store, _ := newStore(t)
	ctx := context.Background()

	seed := []struct{ provider, subject, repo string }{
		{"github", "octocat", "acme/app"},
		{"github", "octocat", "acme/other"},
		{"github", "hubot", "acme/app"},
		{"gitlab", "octocat", "acme/app"},
	}
	for _, s := range seed {
		ensureAndUpsert(t, store, s.provider, s.subject, s.repo, identity.PermissionRead)
	}

	if err := store.DeleteForSubject(ctx, "github", "octocat"); err != nil {
		t.Fatalf("DeleteForSubject: %v", err)
	}

	for _, tc := range []struct {
		provider, subject, repo string
		wantFound               bool
	}{
		{"github", "octocat", "acme/app", false},
		{"github", "octocat", "acme/other", false},
		{"github", "hubot", "acme/app", true},
		{"gitlab", "octocat", "acme/app", true},
	} {
		_, found, err := store.Get(ctx, tc.provider, tc.subject, tc.repo)
		if err != nil {
			t.Fatalf("Get %s/%s/%s: %v", tc.provider, tc.subject, tc.repo, err)
		}
		if found != tc.wantFound {
			t.Errorf("Get(%s, %s, %s) found = %v, want %v", tc.provider, tc.subject, tc.repo, found, tc.wantFound)
		}
	}
}

// The Mirror driving a REAL store: a miss resolves through the forge and is
// memoized, and the second read is served from the mirror with no forge call.
func TestRepoACLPostgres_MirrorMemoizesAcrossReads(t *testing.T) {
	store, _ := newStore(t)
	res := &fakeResolver{perm: identity.PermissionWrite}
	m := repoacl.NewMirror(store, res, repoacl.DefaultTTL, testLogger())
	ctx := context.Background()

	for i := range 2 {
		visible, err := m.Visible(ctx, "github", "octocat", "acme/app")
		if err != nil {
			t.Fatalf("Visible #%d: %v", i, err)
		}
		if !visible {
			t.Fatalf("Visible #%d = false, want true", i)
		}
	}
	if res.calls != 1 {
		t.Errorf("forge calls = %d, want 1 (the second read must hit the mirror)", res.calls)
	}

	// After a purge the next read re-resolves.
	if err := m.InvalidateSubject(ctx, "github", "octocat"); err != nil {
		t.Fatalf("InvalidateSubject: %v", err)
	}
	if _, err := m.Visible(ctx, "github", "octocat", "acme/app"); err != nil {
		t.Fatalf("Visible after purge: %v", err)
	}
	if res.calls != 2 {
		t.Errorf("forge calls = %d after purge, want 2", res.calls)
	}
}

// TestRepoACLPostgres_PurgeOverlapRejectsInFlightGuardedInsert is the binding
// race-B proof (#2116), exercising the OVERLAP window — NOT the
// purge-completes-first case. A purge (T1) BUMPS the watermark and HOLDS its row
// lock uncommitted while an in-flight resolution's guarded upsert (T2), carrying
// a generation captured BEFORE the bump, BLOCKS on FOR SHARE. Binding condition
// 2: we assert POSITIVE blocked-state evidence — T2's backend waiting on a Lock
// in pg_stat_activity — so a non-blocking lock mode (FOR KEY SHARE) fails this
// test LOUDLY. The bump+delete then commit; T2 unblocks, re-reads the BUMPED
// generation via EvalPlanQual, is REJECTED (pgx.ErrNoRows), and the stale grant
// does NOT survive the purge (Get → found=false).
func TestRepoACLPostgres_PurgeOverlapRejectsInFlightGuardedInsert(t *testing.T) {
	store, pool := newStore(t)
	ctx := context.Background()
	const provider, subject, repo = "github", "octocat", "acme/app"

	// Resolution start: create the lockable watermark row and capture gen 0
	// (BEFORE the racing purge bumps it).
	capturedGen, err := store.EnsurePurgeGeneration(ctx, provider, subject)
	if err != nil {
		t.Fatalf("EnsurePurgeGeneration: %v", err)
	}
	if capturedGen != 0 {
		t.Fatalf("captured generation = %d, want 0", capturedGen)
	}

	// T1: a transaction that bumps the watermark and holds the FOR NO KEY UPDATE
	// row lock UNCOMMITTED.
	conn1, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire conn1: %v", err)
	}
	defer conn1.Release()
	tx1, err := conn1.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx1: %v", err)
	}
	defer func() { _ = tx1.Rollback(ctx) }() // no-op after commit
	q1 := repoacldb.New(tx1)
	if err := q1.BumpRepoACLPurgeWatermark(ctx, repoacldb.BumpRepoACLPurgeWatermarkParams{
		Provider: provider, Subject: subject,
	}); err != nil {
		t.Fatalf("bump in tx1: %v", err)
	}

	// T2: a dedicated connection whose backend pid we watch. Run the guarded
	// upsert with the PRE-bump generation on a goroutine; it must BLOCK on FOR
	// SHARE against tx1's uncommitted bump.
	conn2, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire conn2: %v", err)
	}
	defer conn2.Release()
	var conn2pid int
	if err := conn2.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&conn2pid); err != nil {
		t.Fatalf("conn2 backend pid: %v", err)
	}
	q2 := repoacldb.New(conn2)

	type upsertResult struct {
		row repoacldb.RepoAclEntry
		err error
	}
	done := make(chan upsertResult, 1)
	go func() {
		row, err := q2.UpsertRepoACLEntryGuarded(ctx, repoacldb.UpsertRepoACLEntryGuardedParams{
			ID:         uuid.New(),
			Provider:   provider,
			Subject:    subject,
			Repo:       repo,
			Permission: string(identity.PermissionAdmin),
			Generation: capturedGen,
		})
		done <- upsertResult{row, err}
	}()

	// POSITIVE blocked-state evidence: poll pg_stat_activity until conn2's
	// backend is waiting on a Lock. If the upsert returns FIRST (never blocked),
	// the lock mode is non-blocking and race B is open — fail loudly.
	blockDeadline := time.Now().Add(timescale.D(2 * time.Second))
	blocked := false
	for time.Now().Before(blockDeadline) {
		select {
		case res := <-done:
			t.Fatalf("guarded upsert returned (err=%v) before the bump committed — it did NOT block on the watermark row lock; the lock mode is non-blocking (FOR KEY SHARE?) and race B is OPEN", res.err)
		default:
		}
		var waitEventType string
		if err := pool.QueryRow(ctx,
			`SELECT coalesce(wait_event_type, '') FROM pg_stat_activity WHERE pid = $1`, conn2pid,
		).Scan(&waitEventType); err != nil {
			t.Fatalf("poll pg_stat_activity: %v", err)
		}
		if waitEventType == "Lock" {
			blocked = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !blocked {
		t.Fatalf("conn2 never entered a Lock wait within %v — the guarded upsert did not block on the watermark row; the lock mode fails to serialize against the bump (race B OPEN)", timescale.D(2*time.Second))
	}

	// The purge completes while T2 is blocked: commit the bump AND the delete.
	if err := q1.DeleteRepoACLEntriesForSubject(ctx, repoacldb.DeleteRepoACLEntriesForSubjectParams{
		Provider: provider, Subject: subject,
	}); err != nil {
		t.Fatalf("delete in tx1: %v", err)
	}
	if err := tx1.Commit(ctx); err != nil {
		t.Fatalf("commit tx1: %v", err)
	}

	// T2 unblocks, re-reads the BUMPED generation (1 > capturedGen 0), and is
	// REJECTED: zero rows → pgx.ErrNoRows.
	select {
	case res := <-done:
		if !errors.Is(res.err, pgx.ErrNoRows) {
			t.Fatalf("guarded upsert err = %v, want pgx.ErrNoRows (a purge that bumped after capture must reject the in-flight write)", res.err)
		}
	case <-time.After(timescale.D(5 * time.Second)):
		t.Fatal("guarded upsert did not return after the bump committed")
	}

	// The stale grant did NOT survive the purge — had the guard failed, the
	// rejected INSERT's row would be present here.
	if _, found, err := store.Get(ctx, provider, subject, repo); err != nil {
		t.Fatalf("Get after purge: %v", err)
	} else if found {
		t.Fatal("the rejected grant is present in the mirror — the guard did not hold across the delete (race B OPEN)")
	}

	// The store's Upsert maps the guarded rejection (pgx.ErrNoRows) to a BENIGN
	// nil, not a store fault: a stale-generation write through the production
	// path returns nil and memoizes nothing. The watermark is now at generation
	// 1, so capturedGen 0 is rejected.
	if err := store.Upsert(ctx, provider, subject, repo, identity.PermissionAdmin, capturedGen); err != nil {
		t.Fatalf("store.Upsert with a stale generation = %v, want nil (a guarded rejection is benign, not a fault)", err)
	}
	if _, found, err := store.Get(ctx, provider, subject, repo); err != nil {
		t.Fatalf("Get after benign rejection: %v", err)
	} else if found {
		t.Fatal("a stale-generation store.Upsert memoized a row — the benign rejection must write nothing")
	}
}

// TestRepoACLPostgres_ResolveAfterPurgeMemoizes: a resolution that captures the
// POST-bump generation writes successfully and is served from the mirror on the
// next read. This is the correctly-allowed direction — a resolution whose forge
// read is post-purge must memoize.
func TestRepoACLPostgres_ResolveAfterPurgeMemoizes(t *testing.T) {
	store, _ := newStore(t)
	res := &fakeResolver{perm: identity.PermissionWrite}
	m := repoacl.NewMirror(store, res, repoacl.DefaultTTL, testLogger())
	ctx := context.Background()

	// A purge bumps the watermark to generation 1.
	if err := m.InvalidateSubject(ctx, "github", "octocat"); err != nil {
		t.Fatalf("InvalidateSubject: %v", err)
	}
	// A resolution AFTER the purge captures generation 1 and writes.
	if visible, err := m.Visible(ctx, "github", "octocat", "acme/app"); err != nil || !visible {
		t.Fatalf("Visible = (%v, %v), want (true, nil)", visible, err)
	}
	// Served from the mirror on the next read (memoized).
	if visible, err := m.Visible(ctx, "github", "octocat", "acme/app"); err != nil || !visible {
		t.Fatalf("Visible #2 = (%v, %v), want (true, nil)", visible, err)
	}
	if res.calls != 1 {
		t.Errorf("forge calls = %d, want 1 (post-purge resolution memoized; second read hits the mirror)", res.calls)
	}
}

// TestRepoACLPostgres_NeverPurgedResolveMemoizes: a subject with NO prior purge
// resolves and memoizes — EnsurePurgeGeneration creates the watermark at
// generation 0, the guard passes (0 >= 0), and the write lands. Covers the
// first-purge / never-purged / cache-miss-no-entry case (m3).
func TestRepoACLPostgres_NeverPurgedResolveMemoizes(t *testing.T) {
	store, _ := newStore(t)
	res := &fakeResolver{perm: identity.PermissionRead}
	m := repoacl.NewMirror(store, res, repoacl.DefaultTTL, testLogger())
	ctx := context.Background()

	if visible, err := m.Visible(ctx, "github", "octocat", "acme/app"); err != nil || !visible {
		t.Fatalf("Visible = (%v, %v), want (true, nil)", visible, err)
	}
	if visible, err := m.Visible(ctx, "github", "octocat", "acme/app"); err != nil || !visible {
		t.Fatalf("Visible #2 = (%v, %v), want (true, nil)", visible, err)
	}
	if res.calls != 1 {
		t.Errorf("forge calls = %d, want 1 (never-purged resolution memoized; second read hits the mirror)", res.calls)
	}
}
