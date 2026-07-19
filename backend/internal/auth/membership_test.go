package auth_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	accountdb "github.com/kuhlman-labs/fishhawk/backend/internal/account/db"
	"github.com/kuhlman-labs/fishhawk/backend/internal/auth"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/postgres"
)

// The Amendment A2 admission matrix is driven against a REAL migrated
// database (the account_members rows are the authoritative source)
// with a FAKE forge lister (the auto-join bootstrap's only live read)
// — never a pre-seeded shortcut around the resolver.

type fakeOrgLister struct {
	keys  []string
	err   error
	calls int
}

func (f *fakeOrgLister) ListUserOrgKeys(context.Context, string) ([]string, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.keys, nil
}

func newMembershipFixture(t *testing.T) (*pgxpool.Pool, *fakeOrgLister, auth.MembershipResolver) {
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
	lister := &fakeOrgLister{}
	resolver := auth.NewMembershipResolver(
		auth.NewAccountMembershipStore(accountdb.New(pool)), lister)
	return pool, lister, resolver
}

// seedGitHubAccount inserts a github account with the given
// granularity and auto-join policy role (nil = no policy).
func seedGitHubAccount(t *testing.T, pool *pgxpool.Pool, key, granularity string, autoJoinRole *string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO accounts (id, provider, account_key, granularity, auto_join_role)
		 VALUES ($1, 'github', $2, $3, $4)`,
		id, key, granularity, autoJoinRole,
	); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	return id
}

// seedMember inserts an account_members row with an explicit origin.
func seedMember(t *testing.T, pool *pgxpool.Pool, accountID uuid.UUID, memberRef, origin string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO account_members (id, account_id, provider, member_ref, origin)
		 VALUES ($1, $2, 'github', $3, $4)`,
		uuid.New(), accountID, memberRef, origin,
	); err != nil {
		t.Fatalf("seed member: %v", err)
	}
}

func resolve(t *testing.T, r auth.MembershipResolver, provider string) ([]uuid.UUID, error) {
	t.Helper()
	return r.ResolveAccounts(context.Background(), provider, "gho_tok",
		auth.GitHubProfile{ID: 42, Login: "octocat"})
}

// (a) An invited row admits with the forge lister ERRORING — the
// DB-only path: forge availability can never lock out an invited
// member.
func TestMembership_InvitedRowAdmits_ForgeErroring(t *testing.T) {
	pool, lister, r := newMembershipFixture(t)
	accountID := seedGitHubAccount(t, pool, "acme-corp", "organization", nil)
	seedMember(t, pool, accountID, "octocat", auth.MemberOriginInvited)
	lister.err = errors.New("github is down")

	got, err := resolve(t, r, "github")
	if err != nil {
		t.Fatalf("ResolveAccounts: %v", err)
	}
	if len(got) != 1 || got[0] != accountID {
		t.Errorf("admitted = %v, want [%s]", got, accountID)
	}
}

// (b) No row + a matching auto-join policy: admits AND mints an
// audited origin='auto_join' row bound to the right account with the
// policy role.
func TestMembership_AutoJoinBootstrap_MintsAuditedRow(t *testing.T) {
	pool, lister, r := newMembershipFixture(t)
	role := "member"
	accountID := seedGitHubAccount(t, pool, "acme-corp", "organization", &role)
	lister.keys = []string{"acme-corp", "unrelated-org"}

	got, err := resolve(t, r, "github")
	if err != nil {
		t.Fatalf("ResolveAccounts: %v", err)
	}
	if len(got) != 1 || got[0] != accountID {
		t.Fatalf("admitted = %v, want [%s]", got, accountID)
	}

	var origin, memberRef string
	var gotRole *string
	if err := pool.QueryRow(context.Background(),
		`SELECT origin, member_ref, role FROM account_members WHERE account_id = $1`,
		accountID,
	).Scan(&origin, &memberRef, &gotRole); err != nil {
		t.Fatalf("read minted grant: %v", err)
	}
	if origin != auth.MemberOriginAutoJoin {
		t.Errorf("origin = %q, want auto_join", origin)
	}
	if memberRef != "octocat" {
		t.Errorf("member_ref = %q, want octocat", memberRef)
	}
	if gotRole == nil || *gotRole != "member" {
		t.Errorf("role = %v, want policy role 'member'", gotRole)
	}
}

// (c) An existing auto_join row whose predicate no longer holds stops
// admitting — but the row is KEPT for audit. Both predicate legs are
// exercised: policy revoked, and user no longer in the org.
func TestMembership_AutoJoinReverify_PredicateFailed_Denies(t *testing.T) {
	t.Run("policy revoked", func(t *testing.T) {
		pool, lister, r := newMembershipFixture(t)
		accountID := seedGitHubAccount(t, pool, "acme-corp", "organization", nil) // no policy
		seedMember(t, pool, accountID, "octocat", auth.MemberOriginAutoJoin)
		lister.keys = []string{"acme-corp"} // still a live org member

		got, err := resolve(t, r, "github")
		if err != nil {
			t.Fatalf("ResolveAccounts: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("admitted = %v, want deny (policy revoked)", got)
		}
		assertMemberRowCount(t, pool, accountID, 1) // kept for audit
	})
	t.Run("left the org", func(t *testing.T) {
		pool, lister, r := newMembershipFixture(t)
		role := "member"
		accountID := seedGitHubAccount(t, pool, "acme-corp", "organization", &role)
		seedMember(t, pool, accountID, "octocat", auth.MemberOriginAutoJoin)
		lister.keys = nil // live list no longer contains acme-corp

		got, err := resolve(t, r, "github")
		if err != nil {
			t.Fatalf("ResolveAccounts: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("admitted = %v, want deny (no longer an org member)", got)
		}
		assertMemberRowCount(t, pool, accountID, 1) // kept for audit
	})
}

// A still-valid auto_join row re-verifies and admits again without
// minting a duplicate.
func TestMembership_AutoJoinReverify_PredicateHolds_Admits(t *testing.T) {
	pool, lister, r := newMembershipFixture(t)
	role := "member"
	accountID := seedGitHubAccount(t, pool, "acme-corp", "organization", &role)
	seedMember(t, pool, accountID, "octocat", auth.MemberOriginAutoJoin)
	lister.keys = []string{"acme-corp"}

	got, err := resolve(t, r, "github")
	if err != nil {
		t.Fatalf("ResolveAccounts: %v", err)
	}
	if len(got) != 1 || got[0] != accountID {
		t.Errorf("admitted = %v, want [%s]", got, accountID)
	}
	assertMemberRowCount(t, pool, accountID, 1)
}

// (d) No grant and no matching policy: deny (empty, nil error — the
// callback turns this into the access-denied redirect with no cookie).
func TestMembership_NoRowNoPolicy_Denies(t *testing.T) {
	pool, lister, r := newMembershipFixture(t)
	seedGitHubAccount(t, pool, "acme-corp", "organization", nil)
	lister.keys = []string{"acme-corp"} // org member, but the account has no auto-join policy

	got, err := resolve(t, r, "github")
	if err != nil {
		t.Fatalf("ResolveAccounts: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("admitted = %v, want deny", got)
	}
}

// (e) A forge error during auto-join eval with NO invited row fails
// CLOSED: error, no admission.
func TestMembership_ForgeError_NoInvitedRow_FailsClosed(t *testing.T) {
	pool, lister, r := newMembershipFixture(t)
	role := "member"
	seedGitHubAccount(t, pool, "acme-corp", "organization", &role)
	lister.err = errors.New("github is down")

	got, err := resolve(t, r, "github")
	if err == nil {
		t.Fatalf("ResolveAccounts = %v, want fail-closed error", got)
	}
}

// (f) invited vs auto_join origins persist distinctly on their rows.
func TestMembership_OriginsPersistDistinctly(t *testing.T) {
	pool, lister, r := newMembershipFixture(t)
	invitedID := seedGitHubAccount(t, pool, "invited-org", "organization", nil)
	seedMember(t, pool, invitedID, "octocat", auth.MemberOriginInvited)
	role := "admin"
	autoID := seedGitHubAccount(t, pool, "auto-org", "organization", &role)
	lister.keys = []string{"auto-org"}

	got, err := resolve(t, r, "github")
	if err != nil {
		t.Fatalf("ResolveAccounts: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("admitted = %v, want both accounts", got)
	}
	for _, tc := range []struct {
		accountID  uuid.UUID
		wantOrigin string
	}{
		{invitedID, auth.MemberOriginInvited},
		{autoID, auth.MemberOriginAutoJoin},
	} {
		var origin string
		if err := pool.QueryRow(context.Background(),
			`SELECT origin FROM account_members WHERE account_id = $1 AND member_ref = 'octocat'`,
			tc.accountID,
		).Scan(&origin); err != nil {
			t.Fatalf("read origin for %s: %v", tc.accountID, err)
		}
		if origin != tc.wantOrigin {
			t.Errorf("origin for %s = %q, want %q", tc.accountID, origin, tc.wantOrigin)
		}
	}
}

// A provider with no resolver implementation (gitlab today) denies —
// even with an admitting row present the GitHub walk never runs.
func TestMembership_GitLabProvider_Denies(t *testing.T) {
	_, lister, r := newMembershipFixture(t)
	got, err := r.ResolveAccounts(context.Background(), "gitlab", "glpat-tok",
		auth.GitHubProfile{ID: 7, Login: "gl-user"})
	if err != nil {
		t.Fatalf("ResolveAccounts: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("admitted = %v, want deny (no gitlab impl)", got)
	}
	if lister.calls != 0 {
		t.Errorf("forge lister called %d times for an unimplemented provider, want 0", lister.calls)
	}
}

// Auto-join anchors to ORGANIZATION granularity only: a policy role on
// an enterprise-granularity account never auto-admits (the enterprise
// membership API is not used; ADR-057 Amendment A2).
func TestMembership_EnterpriseGranularityPolicy_NeverAutoJoins(t *testing.T) {
	pool, lister, r := newMembershipFixture(t)
	role := "member"
	seedGitHubAccount(t, pool, "acme-ent", "enterprise", &role)
	lister.keys = []string{"acme-ent"}

	got, err := resolve(t, r, "github")
	if err != nil {
		t.Fatalf("ResolveAccounts: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("admitted = %v, want deny (auto-join is org-granularity only)", got)
	}
}

// Multi-account admission is deterministic: the returned order is
// stable across calls (the callback binds the FIRST id).
func TestMembership_MultiAccount_DeterministicOrder(t *testing.T) {
	pool, lister, r := newMembershipFixture(t)
	a := seedGitHubAccount(t, pool, "org-a", "organization", nil)
	b := seedGitHubAccount(t, pool, "org-b", "organization", nil)
	seedMember(t, pool, a, "octocat", auth.MemberOriginInvited)
	seedMember(t, pool, b, "octocat", auth.MemberOriginInvited)
	lister.keys = nil

	first, err := resolve(t, r, "github")
	if err != nil {
		t.Fatalf("ResolveAccounts: %v", err)
	}
	second, err := resolve(t, r, "github")
	if err != nil {
		t.Fatalf("ResolveAccounts: %v", err)
	}
	if len(first) != 2 || len(second) != 2 {
		t.Fatalf("admitted = %v / %v, want both accounts twice", first, second)
	}
	if first[0] != second[0] || first[1] != second[1] {
		t.Errorf("order not deterministic: %v vs %v", first, second)
	}
	if first[0].String() > first[1].String() {
		t.Errorf("ids not sorted: %v", first)
	}
}

func assertMemberRowCount(t *testing.T, pool *pgxpool.Pool, accountID uuid.UUID, want int) {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM account_members WHERE account_id = $1`, accountID,
	).Scan(&n); err != nil {
		t.Fatalf("count member rows: %v", err)
	}
	if n != want {
		t.Errorf("account_members rows for %s = %d, want %d", accountID, n, want)
	}
}
