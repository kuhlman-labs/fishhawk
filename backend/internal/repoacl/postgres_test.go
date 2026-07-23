package repoacl_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/identity"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/postgres"
	"github.com/kuhlman-labs/fishhawk/backend/internal/repoacl"
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

func TestRepoACLPostgres_UpsertGetRoundTrip(t *testing.T) {
	store, _ := newStore(t)
	ctx := context.Background()

	// A miss is found=false with NO error — never confused with a fault.
	if _, found, err := store.Get(ctx, "github", "octocat", "acme/app"); err != nil || found {
		t.Fatalf("Get(miss) = (found=%v, err=%v), want (false, nil)", found, err)
	}

	before := time.Now()
	if err := store.Upsert(ctx, "github", "octocat", "acme/app", identity.PermissionRead); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
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

	if err := store.Upsert(ctx, "github", "octocat", "acme/app", identity.PermissionAdmin); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	first, _, err := store.Get(ctx, "github", "octocat", "acme/app")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	// Postgres now() is transaction-start time, so a same-instant re-upsert can
	// tie. Sleep past the timestamp resolution the assertion depends on.
	time.Sleep(5 * time.Millisecond)

	if err := store.Upsert(ctx, "github", "octocat", "acme/app", identity.PermissionNone); err != nil {
		t.Fatalf("Upsert (conflict): %v", err)
	}
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
		if err := store.Upsert(ctx, s.provider, s.subject, s.repo, identity.PermissionRead); err != nil {
			t.Fatalf("Upsert %v: %v", s, err)
		}
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
