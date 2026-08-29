package auth_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/account"
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

func newMembershipPool(t *testing.T) *pgxpool.Pool {
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
	return pool
}

func newMembershipFixture(t *testing.T) (*pgxpool.Pool, *fakeOrgLister, auth.MembershipResolver) {
	t.Helper()
	pool := newMembershipPool(t)
	lister := &fakeOrgLister{}
	resolver := auth.NewMembershipResolver(
		auth.NewAccountMembershipStore(accountdb.New(pool)),
		map[string]auth.ForgeMembershipLister{"github": lister})
	return pool, lister, resolver
}

// newEMUFixture is newMembershipFixture under EMU posture: the
// deployment's OAuth host is a data-resident <slug>.ghe.com endpoint,
// so an underscore-bearing login yields an enterprise short code.
func newEMUFixture(t *testing.T) (*pgxpool.Pool, *fakeOrgLister, auth.MembershipResolver) {
	t.Helper()
	pool := newMembershipPool(t)
	lister := &fakeOrgLister{}
	resolver := auth.NewMembershipResolver(
		auth.NewAccountMembershipStore(accountdb.New(pool)),
		map[string]auth.ForgeMembershipLister{"github": lister},
		auth.WithEMUOAuthHost("https://acme.ghe.com/login/oauth/authorize"))
	return pool, lister, resolver
}

// newGitLabFixture registers a gitlab group lister alongside github.
func newGitLabFixture(t *testing.T) (*pgxpool.Pool, *fakeOrgLister, auth.MembershipResolver) {
	t.Helper()
	pool := newMembershipPool(t)
	lister := &fakeOrgLister{}
	resolver := auth.NewMembershipResolver(
		auth.NewAccountMembershipStore(accountdb.New(pool)),
		map[string]auth.ForgeMembershipLister{"gitlab": lister})
	return pool, lister, resolver
}

// seedGitHubAccount inserts a github account with the given
// granularity and auto-join policy role (nil = no policy).
func seedGitHubAccount(t *testing.T, pool *pgxpool.Pool, key, granularity string, autoJoinRole *string) uuid.UUID {
	t.Helper()
	return seedProviderAccount(t, pool, "github", key, granularity, autoJoinRole)
}

func seedProviderAccount(t *testing.T, pool *pgxpool.Pool, provider, key, granularity string, autoJoinRole *string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO accounts (id, provider, account_key, granularity, auto_join_role)
		 VALUES ($1, $2, $3, $4, $5)`,
		id, provider, key, granularity, autoJoinRole,
	); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	return id
}

// seedMember inserts an account_members row with an explicit origin.
func seedMember(t *testing.T, pool *pgxpool.Pool, accountID uuid.UUID, memberRef, origin string) {
	t.Helper()
	seedMemberFor(t, pool, "github", accountID, memberRef, origin)
}

func seedMemberFor(t *testing.T, pool *pgxpool.Pool, provider string, accountID uuid.UUID, memberRef, origin string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO account_members (id, account_id, provider, member_ref, origin)
		 VALUES ($1, $2, $3, $4, $5)`,
		uuid.New(), accountID, provider, memberRef, origin,
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

// An invited row admits WITHOUT the forge lister being called at all —
// the stronger 'not called' assertion (mirrors the GitLab-deny check)
// proving invited admission is forge-INDEPENDENT, not merely
// forge-error-tolerant (ADR-057 Amendment A2). The lister here would
// SUCCEED; admission must still make no forge call.
func TestMembership_InvitedRowAdmits_ForgeNeverCalled(t *testing.T) {
	pool, lister, r := newMembershipFixture(t)
	accountID := seedGitHubAccount(t, pool, "acme-corp", "organization", nil)
	seedMember(t, pool, accountID, "octocat", auth.MemberOriginInvited)
	lister.keys = []string{"acme-corp"} // a healthy forge would return this

	got, err := resolve(t, r, "github")
	if err != nil {
		t.Fatalf("ResolveAccounts: %v", err)
	}
	if len(got) != 1 || got[0] != accountID {
		t.Errorf("admitted = %v, want [%s]", got, accountID)
	}
	if lister.calls != 0 {
		t.Errorf("forge lister called %d times on the invited-admit path, want 0", lister.calls)
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
// (b) already covers minting an auto_join row; here both origins coexist
// for one user across different accounts and read back distinctly. They
// are seeded directly because the invited short-circuit (A2) means a
// single login with an invited grant never also mints an auto_join row.
func TestMembership_OriginsPersistDistinctly(t *testing.T) {
	pool, _, _ := newMembershipFixture(t)
	invitedID := seedGitHubAccount(t, pool, "invited-org", "organization", nil)
	seedMember(t, pool, invitedID, "octocat", auth.MemberOriginInvited)
	role := "admin"
	autoID := seedGitHubAccount(t, pool, "auto-org", "organization", &role)
	seedMember(t, pool, autoID, "octocat", auth.MemberOriginAutoJoin)

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

// A provider with NO registered lister (gitlab when
// FISHHAWKD_GITLAB_BASE_URL is unset) denies when no grant admits: the
// auto_join eval cannot run, and the github lister is never called for
// another provider's login.
func TestMembership_UnregisteredProvider_Denies(t *testing.T) {
	_, lister, r := newMembershipFixture(t)
	got, err := r.ResolveAccounts(context.Background(), "gitlab", "glpat-tok",
		auth.GitHubProfile{ID: 7, Login: "gl-user"})
	if err != nil {
		t.Fatalf("ResolveAccounts: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("admitted = %v, want deny (no gitlab lister registered)", got)
	}
	if lister.calls != 0 {
		t.Errorf("forge lister called %d times for an unregistered provider, want 0", lister.calls)
	}
}

// CONDITION (1) — the key/granularity product must NOT admit across
// granularities. Both directions asserted, both DENIALS.
func TestMembership_KeyGranularityPairing_NoCrossGranularityAdmission(t *testing.T) {
	// (a) A live ORG key "acme" must not admit an ENTERPRISE account
	// keyed "acme": the user is merely an org member.
	t.Run("org key does not admit an enterprise account of the same key", func(t *testing.T) {
		pool, lister, r := newEMUFixture(t)
		role := "member"
		seedGitHubAccount(t, pool, "acme", "enterprise", &role)
		lister.keys = []string{"acme"} // org membership only

		got, err := r.ResolveAccounts(context.Background(), "github", "gho_tok",
			// A login with no underscore derives NO enterprise key, so
			// the only candidate pair is ("acme", organization).
			auth.GitHubProfile{ID: 42, Login: "octocat"})
		if err != nil {
			t.Fatalf("ResolveAccounts: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("admitted = %v, want deny (org key must not admit an enterprise account)", got)
		}
	})
	// (b) A derived ENTERPRISE short code "acme" must not admit an
	// ORGANIZATION account keyed "acme".
	t.Run("enterprise short code does not admit an organization account of the same key", func(t *testing.T) {
		pool, lister, r := newEMUFixture(t)
		role := "member"
		seedGitHubAccount(t, pool, "acme", "organization", &role)
		lister.keys = nil // NOT an org member of acme

		got, err := r.ResolveAccounts(context.Background(), "github", "gho_tok",
			auth.GitHubProfile{ID: 42, Login: "alice_acme"})
		if err != nil {
			t.Fatalf("ResolveAccounts: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("admitted = %v, want deny (enterprise short code must not admit an organization account)", got)
		}
	})
	// The same pairing governs RE-VERIFICATION of an existing
	// auto_join grant: an enterprise-granularity grant must not
	// re-admit off a same-named live ORG key.
	t.Run("reverification does not cross granularities", func(t *testing.T) {
		pool, lister, r := newEMUFixture(t)
		role := "member"
		accountID := seedGitHubAccount(t, pool, "acme", "enterprise", &role)
		seedMember(t, pool, accountID, "octocat", auth.MemberOriginAutoJoin)
		lister.keys = []string{"acme"} // org membership only

		got, err := r.ResolveAccounts(context.Background(), "github", "gho_tok",
			auth.GitHubProfile{ID: 42, Login: "octocat"})
		if err != nil {
			t.Fatalf("ResolveAccounts: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("admitted = %v, want deny (grant re-verification must stay granularity-bound)", got)
		}
		assertMemberRowCount(t, pool, accountID, 1) // kept for audit
	})
}

// EMU posture: an enterprise-granularity policy account keyed by the
// login's short code ADMITS and mints an audited grant. A no-op change
// leaving the query at granularity='organization' fails this.
func TestMembership_EMUEnterpriseAutoJoin_Admits(t *testing.T) {
	pool, lister, r := newEMUFixture(t)
	role := "member"
	accountID := seedGitHubAccount(t, pool, "acme", "enterprise", &role)
	lister.keys = nil // no org membership at all

	got, err := r.ResolveAccounts(context.Background(), "github", "gho_tok",
		auth.GitHubProfile{ID: 42, Login: "alice_acme"})
	if err != nil {
		t.Fatalf("ResolveAccounts: %v", err)
	}
	if len(got) != 1 || got[0] != accountID {
		t.Fatalf("admitted = %v, want [%s]", got, accountID)
	}
	var origin, memberRef string
	if err := pool.QueryRow(context.Background(),
		`SELECT origin, member_ref FROM account_members WHERE account_id = $1`,
		accountID).Scan(&origin, &memberRef); err != nil {
		t.Fatalf("read minted grant: %v", err)
	}
	if origin != auth.MemberOriginAutoJoin {
		t.Errorf("origin = %q, want auto_join", origin)
	}
	// The FULL login (short code included) stays the identity key.
	if memberRef != "alice_acme" {
		t.Errorf("member_ref = %q, want the full login alice_acme", memberRef)
	}
}

// SPOOFING GUARD: on github.com posture NO enterprise key is derived,
// so a crafted underscore-bearing login cannot claim an enterprise.
func TestMembership_GitHubDotComPosture_UnderscoreLogin_DerivesNoEnterpriseKey(t *testing.T) {
	pool, lister, r := newMembershipFixture(t) // no WithEMUOAuthHost
	role := "member"
	seedGitHubAccount(t, pool, "acme", "enterprise", &role)
	lister.keys = nil

	got, err := r.ResolveAccounts(context.Background(), "github", "gho_tok",
		auth.GitHubProfile{ID: 42, Login: "alice_acme"})
	if err != nil {
		t.Fatalf("ResolveAccounts: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("admitted = %v, want deny (no EMU posture ⇒ no enterprise derivation)", got)
	}
}

// EMU posture + a login with NO underscore contributes no enterprise
// key; org auto-join is unaffected.
func TestMembership_EMUPosture_NoUnderscoreLogin_OrgAutoJoinUnaffected(t *testing.T) {
	pool, lister, r := newEMUFixture(t)
	role := "member"
	entID := seedGitHubAccount(t, pool, "octocat", "enterprise", &role)
	orgID := seedGitHubAccount(t, pool, "acme-corp", "organization", &role)
	lister.keys = []string{"acme-corp"}

	got, err := r.ResolveAccounts(context.Background(), "github", "gho_tok",
		auth.GitHubProfile{ID: 42, Login: "octocat"})
	if err != nil {
		t.Fatalf("ResolveAccounts: %v", err)
	}
	if len(got) != 1 || got[0] != orgID {
		t.Fatalf("admitted = %v, want only the org account [%s] (enterprise %s must not admit)",
			got, orgID, entID)
	}
}

// EMU posture + a derived short code with no matching policy account:
// deny (empty, nil error).
func TestMembership_EMUPosture_NoMatchingEnterpriseAccount_Denies(t *testing.T) {
	pool, lister, r := newEMUFixture(t)
	role := "member"
	seedGitHubAccount(t, pool, "other-ent", "enterprise", &role)
	lister.keys = nil

	got, err := r.ResolveAccounts(context.Background(), "github", "gho_tok",
		auth.GitHubProfile{ID: 42, Login: "alice_acme"})
	if err != nil {
		t.Fatalf("ResolveAccounts: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("admitted = %v, want deny", got)
	}
}

// An existing ENTERPRISE auto_join grant whose short code no longer
// matches stops admitting; the row is kept for audit.
func TestMembership_EMUReverify_ShortCodeChanged_Denies(t *testing.T) {
	pool, lister, r := newEMUFixture(t)
	role := "member"
	accountID := seedGitHubAccount(t, pool, "acme", "enterprise", &role)
	seedMember(t, pool, accountID, "alice_other", auth.MemberOriginAutoJoin)
	lister.keys = nil

	got, err := r.ResolveAccounts(context.Background(), "github", "gho_tok",
		auth.GitHubProfile{ID: 42, Login: "alice_other"})
	if err != nil {
		t.Fatalf("ResolveAccounts: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("admitted = %v, want deny (short code no longer matches)", got)
	}
	assertMemberRowCount(t, pool, accountID, 1)
}

// GitLab: a group-granularity policy account admits a member whose
// live group list carries its full_path.
func TestMembership_GitLabGroupAutoJoin_Admits(t *testing.T) {
	pool, lister, r := newGitLabFixture(t)
	role := "member"
	accountID := seedProviderAccount(t, pool, "gitlab", "acme/platform", "group", &role)
	lister.keys = []string{"acme/platform", "unrelated/group"}

	got, err := r.ResolveAccounts(context.Background(), "gitlab", "gl-tok",
		auth.GitHubProfile{ID: 7, Login: "gl-user"})
	if err != nil {
		t.Fatalf("ResolveAccounts: %v", err)
	}
	if len(got) != 1 || got[0] != accountID {
		t.Fatalf("admitted = %v, want [%s]", got, accountID)
	}
	var origin string
	if err := pool.QueryRow(context.Background(),
		`SELECT origin FROM account_members WHERE account_id = $1`, accountID).Scan(&origin); err != nil {
		t.Fatalf("read minted grant: %v", err)
	}
	if origin != auth.MemberOriginAutoJoin {
		t.Errorf("origin = %q, want auto_join", origin)
	}
}

// GitLab lister error with NO invited grant fails CLOSED: error, and
// no grant minted.
func TestMembership_GitLabListerError_NoInvitedRow_FailsClosed(t *testing.T) {
	pool, lister, r := newGitLabFixture(t)
	role := "member"
	accountID := seedProviderAccount(t, pool, "gitlab", "acme/platform", "group", &role)
	lister.err = errors.New("gitlab is down")

	got, err := r.ResolveAccounts(context.Background(), "gitlab", "gl-tok",
		auth.GitHubProfile{ID: 7, Login: "gl-user"})
	if err == nil {
		t.Fatalf("ResolveAccounts = %v, want fail-closed error", got)
	}
	assertMemberRowCount(t, pool, accountID, 0)
}

// GitLab invited grant admits DB-ONLY: the group lister is never
// called (the invariant the GitHub path already holds).
func TestMembership_GitLabInvitedRow_ListerNeverCalled(t *testing.T) {
	pool, lister, r := newGitLabFixture(t)
	accountID := seedProviderAccount(t, pool, "gitlab", "acme/platform", "group", nil)
	seedMemberFor(t, pool, "gitlab", accountID, "gl-user", auth.MemberOriginInvited)
	lister.keys = []string{"acme/platform"}

	got, err := r.ResolveAccounts(context.Background(), "gitlab", "gl-tok",
		auth.GitHubProfile{ID: 7, Login: "gl-user"})
	if err != nil {
		t.Fatalf("ResolveAccounts: %v", err)
	}
	if len(got) != 1 || got[0] != accountID {
		t.Fatalf("admitted = %v, want [%s]", got, accountID)
	}
	if lister.calls != 0 {
		t.Errorf("gitlab lister called %d times on the invited-admit path, want 0", lister.calls)
	}
}

// An invited grant admits for a provider with NO registered lister.
// Invited admission is DB-only and forge-INDEPENDENT, so it cannot be
// conditioned on the forge being configured at all: here only github
// has a lister, yet a gitlab invited grant still admits. (The lister
// lookup is on the auto_join path only.)
func TestMembership_InvitedRow_UnregisteredProviderLister_StillAdmits(t *testing.T) {
	// The github-only fixture: no gitlab lister is registered.
	pool, ghLister, r := newMembershipFixture(t)
	accountID := seedProviderAccount(t, pool, "gitlab", "acme/platform", "group", nil)
	seedMemberFor(t, pool, "gitlab", accountID, "gl-user", auth.MemberOriginInvited)

	got, err := r.ResolveAccounts(context.Background(), "gitlab", "gl-tok",
		auth.GitHubProfile{ID: 7, Login: "gl-user"})
	if err != nil {
		t.Fatalf("ResolveAccounts: %v", err)
	}
	if len(got) != 1 || got[0] != accountID {
		t.Fatalf("admitted = %v, want [%s] (invited grants are forge-independent)", got, accountID)
	}
	if ghLister.calls != 0 {
		t.Errorf("github lister called %d times resolving a gitlab login, want 0", ghLister.calls)
	}
}

// The same provider with no lister and NO invited grant still denies:
// an auto_join evaluation is impossible without a live membership read.
func TestMembership_UnregisteredProviderLister_NoInvitedRow_Denies(t *testing.T) {
	pool, _, r := newMembershipFixture(t)
	role := "member"
	seedProviderAccount(t, pool, "gitlab", "acme/platform", "group", &role)

	got, err := r.ResolveAccounts(context.Background(), "gitlab", "gl-tok",
		auth.GitHubProfile{ID: 7, Login: "gl-user"})
	if err != nil {
		t.Fatalf("ResolveAccounts: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("admitted = %v, want deny (no gitlab lister ⇒ no auto_join eval)", got)
	}
}

// A failed grant mint aborts the admission: the minted row IS the
// audit record, so if it can't be written the admission doesn't happen.
func TestMembership_MintGrantError_FailsClosed(t *testing.T) {
	lister := &fakeOrgLister{keys: []string{"acme-corp"}}
	role := "member"
	accountID := uuid.New()
	store := &fakeMembershipStore{
		policies: []auth.AutoJoinAccount{{
			AccountID: accountID, AccountKey: "acme-corp",
			Granularity: "organization", AutoJoinRole: role,
		}},
		upsertErr: errors.New("write failed"),
	}
	r := auth.NewMembershipResolver(store,
		map[string]auth.ForgeMembershipLister{"github": lister})

	got, err := resolve(t, r, "github")
	if err == nil {
		t.Fatalf("ResolveAccounts = %v, want fail-closed error on mint failure", got)
	}
}

// fakeMembershipStore drives the store-error branches the real SQL
// cannot be made to fail on demand.
type fakeMembershipStore struct {
	grants    []auth.MemberGrant
	policies  []auth.AutoJoinAccount
	upsertErr error
	gotPairs  []auth.MembershipKey
}

func (f *fakeMembershipStore) ListMemberGrants(context.Context, string, string) ([]auth.MemberGrant, error) {
	return f.grants, nil
}

func (f *fakeMembershipStore) ListAutoJoinAccounts(_ context.Context, _ string, pairs []auth.MembershipKey) ([]auth.AutoJoinAccount, error) {
	f.gotPairs = pairs
	return f.policies, nil
}

func (f *fakeMembershipStore) UpsertAutoJoinGrant(context.Context, uuid.UUID, uuid.UUID, string, string, string) error {
	return f.upsertErr
}

// The resolver hands the store PAIRS, each key bound to the
// granularity it was derived from — the in-process half of condition
// (1) (the SQL half is asserted by the DB-backed denial tests above).
func TestMembership_DerivedPairsStayGranularityBound(t *testing.T) {
	store := &fakeMembershipStore{}
	lister := &fakeOrgLister{keys: []string{"acme-corp"}}
	r := auth.NewMembershipResolver(store,
		map[string]auth.ForgeMembershipLister{"github": lister},
		auth.WithEMUOAuthHost("https://acme.ghe.com/login/oauth/authorize"))

	if _, err := r.ResolveAccounts(context.Background(), "github", "gho_tok",
		auth.GitHubProfile{ID: 42, Login: "alice_acme"}); err != nil {
		t.Fatalf("ResolveAccounts: %v", err)
	}
	want := []auth.MembershipKey{
		{Key: "acme-corp", Granularity: "organization"},
		{Key: "acme", Granularity: "enterprise"},
	}
	if len(store.gotPairs) != len(want) {
		t.Fatalf("pairs = %v, want %v", store.gotPairs, want)
	}
	for i, p := range want {
		if store.gotPairs[i] != p {
			t.Errorf("pair[%d] = %v, want %v", i, store.gotPairs[i], p)
		}
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

// ---------------------------------------------------------------------
// E44.35 (#2925): the forge-INDEPENDENT user-granularity tier.
//
// The load-bearing property is that a user-granularity account's
// ADMISSION DOES NOT DEPEND ON the forge listing call — NOT that the
// process never dials the forge (it still does whenever a healthy lister
// is registered, because the OTHER granularities need it, condition 1).
// So M1 (healthy lister) asserts admission + the minted row and does NOT
// assert a zero call-count; the no-call evidence lives in M2 (provider
// absent from the lister map ⇒ no call possible, fake call-count == 0)
// and M3 (lister errors ⇒ its result cannot have been used).
// ---------------------------------------------------------------------

// M1 [real pgtest]: a user-granularity account keyed 'octocat' AND an
// organization account keyed 'acme-corp' with a live org membership BOTH
// admit through a HEALTHY lister — the UNION of the forge-INDEPENDENT user
// admission and the forge-DERIVED org admission (approval condition 1).
// Seeding the org account is load-bearing: it discriminates a resolver
// that short-circuits after the user account admits and drops the
// forge-derived set (the literal no-call remedy condition 1 FORBADE). A
// test that admitted only the user account would still pass under that
// broken short circuit; asserting the org account too catches it. Each
// account mints exactly one origin='auto_join' row and a second resolve
// leaves both at 1 — the shared-haveGrant no-remint guarantee across the
// two evalAutoJoin calls and across resolves. Admission does NOT turn on
// the lister call itself; that no-call independence is what M2/M3 prove,
// so M1 asserts no zero call-count.
func TestMembership_UserGranularityAutoJoin_Admits(t *testing.T) {
	pool, lister, r := newMembershipFixture(t)
	role := "member"
	userID := seedGitHubAccount(t, pool, "octocat", "user", &role)
	orgID := seedGitHubAccount(t, pool, "acme-corp", "organization", &role)
	lister.keys = []string{"acme-corp"} // healthy forge: octocat is an org member

	for i := 1; i <= 2; i++ {
		got, err := resolve(t, r, "github")
		if err != nil {
			t.Fatalf("ResolveAccounts (resolve #%d): %v", i, err)
		}
		if len(got) != 2 || !admits(got, userID) || !admits(got, orgID) {
			t.Fatalf("resolve #%d admitted = %v, want BOTH the user account %s and the forge-derived org account %s (a short circuit after the user admit would drop the org account)",
				i, got, userID, orgID)
		}
	}

	// The user account minted an audited auto_join row bound to the login
	// with the policy role.
	var origin, memberRef string
	var gotRole *string
	if err := pool.QueryRow(context.Background(),
		`SELECT origin, member_ref, role FROM account_members WHERE account_id = $1`,
		userID).Scan(&origin, &memberRef, &gotRole); err != nil {
		t.Fatalf("read minted user grant: %v", err)
	}
	if origin != auth.MemberOriginAutoJoin {
		t.Errorf("user grant origin = %q, want auto_join", origin)
	}
	if memberRef != "octocat" {
		t.Errorf("user grant member_ref = %q, want octocat", memberRef)
	}
	if gotRole == nil || *gotRole != "member" {
		t.Errorf("user grant role = %v, want policy role 'member'", gotRole)
	}
	// Neither account re-minted across the two resolves.
	assertMemberRowCount(t, pool, userID, 1)
	assertMemberRowCount(t, pool, orgID, 1)
}

// M2 [real pgtest]: the SAME admission with the provider ABSENT from the
// lister map — no forge call is possible, and the user-granularity
// account still admits. The fake lister (registered under a DIFFERENT
// provider) records ZERO calls, the executable no-call evidence for
// `user-granularity-admission-does-not-depend-on-the-listing-call`.
func TestMembership_UserGranularity_UnregisteredLister_StillAdmits(t *testing.T) {
	pool := newMembershipPool(t)
	// A fake lister registered under 'gitlab' only; resolving 'github'
	// finds no lister, so no forge call is even reachable for github.
	lister := &fakeOrgLister{keys: []string{"some-org"}}
	r := auth.NewMembershipResolver(
		auth.NewAccountMembershipStore(accountdb.New(pool)),
		map[string]auth.ForgeMembershipLister{"gitlab": lister})
	role := "member"
	accountID := seedGitHubAccount(t, pool, "octocat", "user", &role)

	got, err := resolve(t, r, "github")
	if err != nil {
		t.Fatalf("ResolveAccounts: %v", err)
	}
	if len(got) != 1 || got[0] != accountID {
		t.Fatalf("admitted = %v, want [%s] (a user-granularity account admits with no forge configured)", got, accountID)
	}
	if lister.calls != 0 {
		t.Errorf("forge lister called %d times admitting a user-granularity account with the provider unregistered, want 0", lister.calls)
	}
	assertMemberRowCount(t, pool, accountID, 1)
}

// M3 [real pgtest]: the SAME admission with the lister returning an
// ERROR. The forge failure cannot deny a predicate that never needed the
// forge — the moved fail-closed edge (condition 1). Its result cannot
// have been used, so admission proves independence.
func TestMembership_UserGranularity_ListerError_StillAdmits(t *testing.T) {
	pool, lister, r := newMembershipFixture(t)
	role := "member"
	accountID := seedGitHubAccount(t, pool, "octocat", "user", &role)
	lister.err = errors.New("github is down")

	got, err := resolve(t, r, "github")
	if err != nil {
		t.Fatalf("ResolveAccounts: %v — a forge error must not deny a user-granularity account", err)
	}
	if len(got) != 1 || got[0] != accountID {
		t.Fatalf("admitted = %v, want [%s]", got, accountID)
	}
	assertMemberRowCount(t, pool, accountID, 1)
}

// M4 [real pgtest]: the regression pin for the edge that must NOT be
// loosened. A forge error with NO user-granularity account (and no
// invited grant) still fails CLOSED — an organization account whose
// admission genuinely needed the forge is denied when the forge errors.
func TestMembership_ForgeError_NoUserGranularityAccount_FailsClosed(t *testing.T) {
	pool, lister, r := newMembershipFixture(t)
	role := "member"
	seedGitHubAccount(t, pool, "acme-corp", "organization", &role)
	lister.err = errors.New("github is down")

	got, err := resolve(t, r, "github")
	if err == nil {
		t.Fatalf("ResolveAccounts = %v, want fail-closed error (no user-granularity account admits)", got)
	}
}

// M5 [real pgtest]: a user account keyed 'someone-else' DENIES login
// 'octocat' — the byte-exact `login == account_key` predicate. (Case
// sensitivity is deliberately not normalized: an operator whose account
// key casing differs from the canonical login is denied here too;
// docs/deploy/self-hosted.md states the key must match EXACTLY.)
func TestMembership_UserGranularity_LoginMismatch_Denies(t *testing.T) {
	pool, lister, r := newMembershipFixture(t)
	role := "member"
	seedGitHubAccount(t, pool, "someone-else", "user", &role)
	lister.keys = nil

	got, err := resolve(t, r, "github")
	if err != nil {
		t.Fatalf("ResolveAccounts: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("admitted = %v, want deny (user account keyed 'someone-else' must not admit octocat)", got)
	}
}

// M6 [real pgtest]: the (key, granularity) pairing must not cross
// granularities, BOTH directions.
func TestMembership_UserGranularityPairing_NoCrossGranularityAdmission(t *testing.T) {
	// (a) An ORGANIZATION account keyed 'octocat' is NOT admitted by the
	// login-derived user pair {octocat, user}: the org account needs a
	// live org membership, which the user tier never supplies.
	t.Run("user pair does not admit an organization account of the login name", func(t *testing.T) {
		pool, lister, r := newMembershipFixture(t)
		role := "member"
		seedGitHubAccount(t, pool, "octocat", "organization", &role)
		lister.keys = nil // NOT an org member of 'octocat'

		got, err := resolve(t, r, "github")
		if err != nil {
			t.Fatalf("ResolveAccounts: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("admitted = %v, want deny (the user pair must not admit an organization account)", got)
		}
	})
	// (b) A USER account keyed 'acme-corp' is NOT admitted by the org key
	// 'acme-corp' the lister returns: the user tier binds to the login,
	// not to an org membership of the same name.
	t.Run("org key does not admit a user account of the same key", func(t *testing.T) {
		pool, lister, r := newMembershipFixture(t)
		role := "member"
		seedGitHubAccount(t, pool, "acme-corp", "user", &role)
		lister.keys = []string{"acme-corp"} // an org membership of the same name

		got, err := resolve(t, r, "github")
		if err != nil {
			t.Fatalf("ResolveAccounts: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("admitted = %v, want deny (an org key must not admit a user account of the same key)", got)
		}
	})
}

// M7 [real pgtest]: re-verification stays login-bound. An existing
// origin='auto_join' grant on a user account whose account_key no longer
// equals the login stops admitting, and the row survives for audit.
func TestMembership_UserGranularityReverify_KeyChanged_Denies(t *testing.T) {
	pool, lister, r := newMembershipFixture(t)
	role := "member"
	// The account was re-keyed to 'someone-else' after the grant was minted.
	accountID := seedGitHubAccount(t, pool, "someone-else", "user", &role)
	seedMember(t, pool, accountID, "octocat", auth.MemberOriginAutoJoin)
	lister.keys = nil

	got, err := resolve(t, r, "github")
	if err != nil {
		t.Fatalf("ResolveAccounts: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("admitted = %v, want deny (user account_key no longer equals the login)", got)
	}
	assertMemberRowCount(t, pool, accountID, 1) // kept for audit
}

// M8 [fake store]: a mint failure on the user path fails CLOSED. The fake
// store's upsertErr is the only way to force the write to fail on demand;
// the minted row IS the audit record, so a failed write must abort the
// admission. (M9 covers the no-duplicate-row property, which is only
// observable against the REAL store.)
func TestMembership_UserGranularity_MintGrantError_FailsClosed(t *testing.T) {
	lister := &fakeOrgLister{keys: []string{"some-org"}}
	role := "member"
	accountID := uuid.New()
	store := &fakeMembershipStore{
		policies: []auth.AutoJoinAccount{{
			AccountID: accountID, AccountKey: "octocat",
			Granularity: "user", AutoJoinRole: role,
		}},
		upsertErr: errors.New("write failed"),
	}
	r := auth.NewMembershipResolver(store,
		map[string]auth.ForgeMembershipLister{"github": lister})

	got, err := resolve(t, r, "github")
	if err == nil {
		t.Fatalf("ResolveAccounts = %v, want fail-closed error on user-path mint failure", got)
	}
}

// M9 [real pgtest]: idempotence. A second resolve for an already-granted
// user account admits WITHOUT re-minting the grant. Read against the REAL
// store — a re-mint is committed state, invisible to the fake (condition
// 3). The row COUNT alone is TAUTOLOGICAL here: UpsertAutoJoinGrant's ON
// CONFLICT (account_id, provider, member_ref) DO UPDATE keeps the count at
// 1 even if the resolver re-upserts, so a count assertion cannot
// distinguish one upsert from two (the test_vacuity concern). The system
// transaction id xmin CAN: a second upsert fires DO UPDATE and writes a
// NEW row version with a fresh xmin, whereas the haveGrant bootstrap guard
// (which skips the mint when the persisted grant already exists) leaves
// xmin untouched. So the load-bearing assertion is xmin unchanged across
// the two resolves; the count stays as a belt-and-suspenders check.
func TestMembership_UserGranularity_SecondResolve_NoDuplicateRow(t *testing.T) {
	pool, lister, r := newMembershipFixture(t)
	role := "member"
	accountID := seedGitHubAccount(t, pool, "octocat", "user", &role)
	lister.keys = nil

	// First resolve mints the grant.
	got, err := resolve(t, r, "github")
	if err != nil {
		t.Fatalf("ResolveAccounts (resolve #1): %v", err)
	}
	if len(got) != 1 || got[0] != accountID {
		t.Fatalf("resolve #1 admitted = %v, want [%s]", got, accountID)
	}
	xmin1 := memberRowXmin(t, pool, accountID)

	// Second resolve re-verifies the existing grant and must NOT re-upsert.
	got, err = resolve(t, r, "github")
	if err != nil {
		t.Fatalf("ResolveAccounts (resolve #2): %v", err)
	}
	if len(got) != 1 || got[0] != accountID {
		t.Fatalf("resolve #2 admitted = %v, want [%s]", got, accountID)
	}
	xmin2 := memberRowXmin(t, pool, accountID)

	if xmin2 != xmin1 {
		t.Errorf("account_members row xmin changed %q -> %q: the second resolve re-upserted the grant (haveGrant bootstrap guard bypassed)", xmin1, xmin2)
	}
	assertMemberRowCount(t, pool, accountID, 1) // no duplicate minted row
}

// CROSS-BOUNDARY (condition 2), user tier: single-tenant CONFIG →
// bootstrap CREATE → resolver ADMISSION. The bootstrap writes the one
// implicit account at granularity=user, and the REAL resolver then
// admits its owner login with NO forge membership read — the exact
// personal-namespace path #2925 stands up. Seeding a row by hand would
// pass even if the bootstrap wrote a granularity/account_key the
// resolver's pair binding does not match; driving the real bootstrap
// catches that wiring failure.
func TestMembershipResolver_AdmitsViaSingleTenantBootstrap_UserGranularity(t *testing.T) {
	pool, lister, r := newMembershipFixture(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	accountID, bootstrapped, err := account.EnsureSingleTenantAccount(context.Background(),
		accountdb.New(pool), account.SingleTenantConfig{
			AccountKey:   "octocat",
			Granularity:  "user",
			AutoJoinRole: "member",
		}, logger)
	if err != nil {
		t.Fatalf("EnsureSingleTenantAccount: %v", err)
	}
	if !bootstrapped {
		t.Fatal("bootstrapped = false, want true")
	}

	// The forge is UNREACHABLE — the personal-namespace admission must
	// not need it.
	lister.err = errors.New("forge unreachable")

	got, err := resolve(t, r, "github")
	if err != nil {
		t.Fatalf("ResolveAccounts: %v", err)
	}
	if len(got) != 1 || got[0] != accountID {
		t.Fatalf("admitted = %v, want the bootstrapped user account [%s]", got, accountID)
	}
	var origin string
	var mrole *string
	if err := pool.QueryRow(context.Background(),
		`SELECT origin, role FROM account_members WHERE account_id = $1`, accountID,
	).Scan(&origin, &mrole); err != nil {
		t.Fatalf("read minted grant: %v", err)
	}
	if origin != auth.MemberOriginAutoJoin {
		t.Errorf("origin = %q, want auto_join", origin)
	}
	if mrole == nil || *mrole != "member" {
		t.Errorf("role = %v, want the profile's auto-join role 'member'", mrole)
	}
}

// admits reports whether id is in the admitted set (order-independent).
func admits(got []uuid.UUID, id uuid.UUID) bool {
	for _, g := range got {
		if g == id {
			return true
		}
	}
	return false
}

// memberRowXmin reads the single account_members row's system transaction
// id (xmin), which Postgres rewrites on every UPDATE — including an ON
// CONFLICT DO UPDATE that leaves the row's values unchanged. A stable xmin
// across two resolves proves the second resolve performed no second upsert.
func memberRowXmin(t *testing.T, pool *pgxpool.Pool, accountID uuid.UUID) string {
	t.Helper()
	var xmin string
	if err := pool.QueryRow(context.Background(),
		`SELECT xmin::text FROM account_members WHERE account_id = $1`, accountID,
	).Scan(&xmin); err != nil {
		t.Fatalf("read row xmin: %v", err)
	}
	return xmin
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

// CROSS-BOUNDARY (E44.9 / #1833): deployment config → persistence → auth
// admission. The single-tenant profile's REAL bootstrap writes the one
// implicit account, and the REAL MembershipResolver then admits a member of
// it — the seam per-layer unit tests would both pass while it broke, since
// the bootstrap's whole purpose is to make the resolver's admission set
// non-empty on a fresh install.
func TestMembershipResolver_AdmitsViaSingleTenantBootstrap(t *testing.T) {
	pool, lister, r := newMembershipFixture(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	accountID, bootstrapped, err := account.EnsureSingleTenantAccount(context.Background(),
		accountdb.New(pool), account.SingleTenantConfig{
			AccountKey:   "acme-corp",
			Granularity:  "organization",
			AutoJoinRole: "member",
		}, logger)
	if err != nil {
		t.Fatalf("EnsureSingleTenantAccount: %v", err)
	}
	if !bootstrapped {
		t.Fatal("bootstrapped = false, want true")
	}

	// The signing-in user is a member of the configured org on the forge.
	lister.keys = []string{"acme-corp"}

	got, err := resolve(t, r, "github")
	if err != nil {
		t.Fatalf("ResolveAccounts: %v", err)
	}
	if len(got) != 1 || got[0] != accountID {
		t.Fatalf("admitted = %v, want the bootstrapped account [%s]", got, accountID)
	}

	var origin string
	var role *string
	if err := pool.QueryRow(context.Background(),
		`SELECT origin, role FROM account_members WHERE account_id = $1`, accountID,
	).Scan(&origin, &role); err != nil {
		t.Fatalf("read minted grant: %v", err)
	}
	if origin != auth.MemberOriginAutoJoin {
		t.Errorf("origin = %q, want auto_join", origin)
	}
	if role == nil || *role != "member" {
		t.Errorf("role = %v, want the profile's auto-join role 'member'", role)
	}
}

// The negative twin: the bootstrapped account's granularity must still BIND
// its key. A profile bootstrapped at ENTERPRISE granularity is not admitted by
// an ORGANIZATION key of the same name (no EMU posture here), so the profile
// cannot widen admission across granularities.
func TestMembershipResolver_SingleTenantBootstrap_NonMatchingGranularityDenied(t *testing.T) {
	pool, lister, r := newMembershipFixture(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	accountID, _, err := account.EnsureSingleTenantAccount(context.Background(),
		accountdb.New(pool), account.SingleTenantConfig{
			AccountKey:   "acme-corp",
			Granularity:  "enterprise",
			AutoJoinRole: "member",
		}, logger)
	if err != nil {
		t.Fatalf("EnsureSingleTenantAccount: %v", err)
	}

	lister.keys = []string{"acme-corp"} // an ORG key of the same name

	got, err := resolve(t, r, "github")
	if err != nil {
		t.Fatalf("ResolveAccounts: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("admitted = %v, want none — an organization key must not admit an enterprise-granularity account", got)
	}
	assertMemberRowCount(t, pool, accountID, 0)
}
