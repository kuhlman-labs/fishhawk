package account

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	accountdb "github.com/kuhlman-labs/fishhawk/backend/internal/account/db"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
)

// fakeMemberQueries is an in-memory MemberQueries for the branches that need no
// database. getAccountErr forces GetAccountByKey to return a specific error
// (e.g. pgx.ErrNoRows) so the unknown-account refusal is reachable without a live
// composite FK, and upsertCalls records whether the invited write was ever
// reached — the counterfactual signal for the validation/fail-closed guards.
type fakeMemberQueries struct {
	getAccountErr error
	account       accountdb.Account
	upsertCalls   int
}

func (f *fakeMemberQueries) GetAccountByKey(_ context.Context, _ accountdb.GetAccountByKeyParams) (accountdb.Account, error) {
	if f.getAccountErr != nil {
		return accountdb.Account{}, f.getAccountErr
	}
	return f.account, nil
}

func (f *fakeMemberQueries) UpsertInvitedAccountMember(_ context.Context, arg accountdb.UpsertInvitedAccountMemberParams) (accountdb.UpsertInvitedAccountMemberRow, error) {
	f.upsertCalls++
	role := arg.Role
	return accountdb.UpsertInvitedAccountMemberRow{
		ID:        arg.ID,
		AccountID: arg.AccountID,
		Provider:  arg.Provider,
		MemberRef: arg.MemberRef,
		Role:      role,
		Origin:    "invited",
	}, nil
}

func (f *fakeMemberQueries) ListAccountMembersWithAccountKey(_ context.Context) ([]accountdb.ListAccountMembersWithAccountKeyRow, error) {
	return nil, nil
}

// TestInviteMemberRequestValidate covers one behavioral case per named failure
// mode: each must return a non-nil error that wraps ErrValidation AND names the
// offending value.
func TestInviteMemberRequestValidate(t *testing.T) {
	cases := []struct {
		name    string
		req     InviteMemberRequest
		wantSub string
	}{
		{"empty member ref", InviteMemberRequest{Provider: "github", AccountKey: "acme", Role: "member"}, "member ref must be non-empty"},
		{"whitespace member ref", InviteMemberRequest{Provider: "github", AccountKey: "acme", MemberRef: "   ", Role: "member"}, "member ref must be non-empty"},
		{"unknown provider", InviteMemberRequest{Provider: "bitbucket", AccountKey: "acme", MemberRef: "octocat", Role: "member"}, `"bitbucket"`},
		{"unknown role", InviteMemberRequest{Provider: "github", AccountKey: "acme", MemberRef: "octocat", Role: "admn"}, `"admn"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.req.resolveDefaults().Validate()
			if err == nil {
				t.Fatalf("Validate() = nil, want error wrapping ErrValidation")
			}
			if !errors.Is(err, ErrValidation) {
				t.Errorf("error %v does not wrap ErrValidation", err)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error %q does not name %q", err.Error(), tc.wantSub)
			}
		})
	}
}

// TestInviteMemberRequestResolveDefaults pins that an omitted role resolves to
// member-tier (least privilege) and that every field is trimmed.
func TestInviteMemberRequestResolveDefaults(t *testing.T) {
	got := InviteMemberRequest{Provider: " github ", AccountKey: " acme ", MemberRef: " octocat ", Role: ""}.resolveDefaults()
	if got.Role != RoleMember {
		t.Errorf("resolved role = %q, want %q (member default)", got.Role, RoleMember)
	}
	if got.Provider != "github" || got.AccountKey != "acme" || got.MemberRef != "octocat" {
		t.Errorf("resolveDefaults did not trim: %+v", got)
	}
	// An explicit role is preserved (and trimmed).
	if r := (InviteMemberRequest{Role: " admin "}).resolveDefaults().Role; r != RoleAdmin {
		t.Errorf("explicit role = %q, want %q", r, RoleAdmin)
	}
}

// TestInviteMember_UnknownRoleIsValidationError is the counterfactual vehicle for
// the role allow-list branch in Validate(): an out-of-set role must be refused as
// ErrValidation and never reach the write. Deleting that branch makes err nil and
// upsertCalls 1 — RED on both assertions. The fake returns a valid account so the
// refusal cannot be attributed to a missing-account path.
func TestInviteMember_UnknownRoleIsValidationError(t *testing.T) {
	fake := &fakeMemberQueries{account: accountdb.Account{ID: uuid.New(), Provider: "github", AccountKey: "acme"}}
	_, err := InviteMember(context.Background(), fake, InviteMemberRequest{
		Provider:   "github",
		AccountKey: "acme",
		MemberRef:  "octocat",
		Role:       "admn", // a typo roles.Store would silently resolve to member-tier
	})
	if err == nil {
		t.Fatalf("InviteMember with out-of-set role = nil error, want ErrValidation")
	}
	if !errors.Is(err, ErrValidation) {
		t.Errorf("error %v does not wrap ErrValidation", err)
	}
	if !strings.Contains(err.Error(), `"admn"`) {
		t.Errorf("error %q does not name the rejected role", err.Error())
	}
	if fake.upsertCalls != 0 {
		t.Errorf("UpsertInvitedAccountMember was called %d times on an invalid role, want 0", fake.upsertCalls)
	}
}

// TestInviteMember_UnknownAccountRefuses is the unit-level twin of the pgtest
// fail-closed counterfactual: when GetAccountByKey reports no row, InviteMember
// must refuse with an error wrapping ErrAccountNotFound that names the
// `fishhawkd account create` remedy and the missing account key, must NOT be an
// ErrValidation (a different CLI exit code), and must NEVER reach the write.
func TestInviteMember_UnknownAccountRefuses(t *testing.T) {
	fake := &fakeMemberQueries{getAccountErr: pgx.ErrNoRows}
	_, err := InviteMember(context.Background(), fake, InviteMemberRequest{
		Provider:   "github",
		AccountKey: "acme",
		MemberRef:  "octocat",
	})
	if err == nil {
		t.Fatalf("InviteMember for unknown account = nil error, want refusal")
	}
	if !errors.Is(err, ErrAccountNotFound) {
		t.Errorf("error %v does not wrap ErrAccountNotFound", err)
	}
	if errors.Is(err, ErrValidation) {
		t.Errorf("unknown-account refusal must NOT be a validation error (drives a different CLI exit code): %v", err)
	}
	if !strings.Contains(err.Error(), "fishhawkd account create") {
		t.Errorf("error %q does not name the `fishhawkd account create` remedy", err.Error())
	}
	if !strings.Contains(err.Error(), `"acme"`) {
		t.Errorf("error %q does not name the missing account_key \"acme\"", err.Error())
	}
	if fake.upsertCalls != 0 {
		t.Errorf("UpsertInvitedAccountMember was called %d times on an unknown account, want 0", fake.upsertCalls)
	}
}

// TestInviteMember_PropagatesDBFault asserts a non-ErrNoRows fault from
// GetAccountByKey is propagated verbatim (not remapped to ErrAccountNotFound), so
// the CLI maps it to a failure exit rather than swallowing a transient fault as a
// missing account.
func TestInviteMember_PropagatesDBFault(t *testing.T) {
	boom := errors.New("connection reset")
	fake := &fakeMemberQueries{getAccountErr: boom}
	_, err := InviteMember(context.Background(), fake, InviteMemberRequest{
		Provider:   "github",
		AccountKey: "acme",
		MemberRef:  "octocat",
	})
	if !errors.Is(err, boom) {
		t.Errorf("error = %v, want the propagated DB fault", err)
	}
	if errors.Is(err, ErrValidation) || errors.Is(err, ErrAccountNotFound) {
		t.Errorf("a raw DB fault must not be classified as validation/not-found: %v", err)
	}
	if fake.upsertCalls != 0 {
		t.Errorf("UpsertInvitedAccountMember was called %d times on a DB fault, want 0", fake.upsertCalls)
	}
}

// TestInviteMember_WritesInvitedGrantAndIsIdempotent asserts committed state
// against a real migrated database: the invite writes exactly one grant with
// origin='invited' and the resolved role, a role-omitted invite resolves to
// 'member' read back from the row, a re-invite with a different role leaves
// exactly one grant carrying the NEW role, and an invite over a seeded
// origin='auto_join' row flips the origin to 'invited' (the documented upgrade).
func TestInviteMember_WritesInvitedGrantAndIsIdempotent(t *testing.T) {
	pool := pgtest.NewPool(t)
	q := accountdb.New(pool)
	ctx := context.Background()

	acct, err := CreateAccount(ctx, q, CreateAccountRequest{Provider: "github", AccountKey: "acme", Granularity: "organization"})
	if err != nil {
		t.Fatalf("seed account: %v", err)
	}

	// Role omitted → default 'member', origin 'invited'.
	row, err := InviteMember(ctx, q, InviteMemberRequest{Provider: "github", AccountKey: "acme", MemberRef: "octocat"})
	if err != nil {
		t.Fatalf("first invite: %v", err)
	}
	if row.Origin != "invited" {
		t.Errorf("origin = %q, want invited", row.Origin)
	}

	assertGrant := func(wantRole, wantOrigin string, wantCount int) {
		t.Helper()
		var count int
		var role, origin string
		if err := pool.QueryRow(ctx,
			`SELECT count(*), max(role), max(origin) FROM account_members WHERE account_id = $1 AND provider = 'github' AND member_ref = 'octocat'`,
			acct.ID).Scan(&count, &role, &origin); err != nil {
			t.Fatalf("read back grant: %v", err)
		}
		if count != wantCount {
			t.Fatalf("grant count = %d, want %d", count, wantCount)
		}
		if role != wantRole {
			t.Errorf("role = %q, want %q", role, wantRole)
		}
		if origin != wantOrigin {
			t.Errorf("origin = %q, want %q", origin, wantOrigin)
		}
	}
	assertGrant("member", "invited", 1)

	// Re-invite with an explicit admin role: exactly one row, new role.
	if _, err := InviteMember(ctx, q, InviteMemberRequest{Provider: "github", AccountKey: "acme", MemberRef: "octocat", Role: "admin"}); err != nil {
		t.Fatalf("second invite: %v", err)
	}
	assertGrant("admin", "invited", 1)

	// Seed a DIFFERENT member as origin='auto_join' via raw INSERT, then invite
	// them: the operator invite must OVERWRITE origin to 'invited' (the upgrade
	// to DB-only admission), still exactly one row.
	autoID := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO account_members (id, account_id, provider, member_ref, role, origin) VALUES ($1, $2, 'github', 'hubber', 'member', 'auto_join')`,
		autoID, acct.ID); err != nil {
		t.Fatalf("seed auto_join grant: %v", err)
	}
	if _, err := InviteMember(ctx, q, InviteMemberRequest{Provider: "github", AccountKey: "acme", MemberRef: "hubber", Role: "member"}); err != nil {
		t.Fatalf("invite over auto_join: %v", err)
	}
	var count int
	var origin string
	if err := pool.QueryRow(ctx,
		`SELECT count(*), max(origin) FROM account_members WHERE account_id = $1 AND provider = 'github' AND member_ref = 'hubber'`,
		acct.ID).Scan(&count, &origin); err != nil {
		t.Fatalf("read back upgraded grant: %v", err)
	}
	if count != 1 {
		t.Errorf("hubber grant count = %d, want 1 (upgrade in place, not a duplicate)", count)
	}
	if origin != "invited" {
		t.Errorf("hubber origin = %q, want invited (auto_join upgraded by operator invite)", origin)
	}
}

// TestInviteMember_FailsClosedOnUnknownAccountCommittedState asserts the
// fail-closed control as committed state against a real database: inviting into
// an account key that was never created refuses with ErrAccountNotFound AND
// leaves zero account_members rows — a control that errored AFTER writing could
// not green this. The unknown key is seeded BY CONSTRUCTION (a fresh random key
// never passed to CreateAccount), so the RED lands on the behavioral assertion.
func TestInviteMember_FailsClosedOnUnknownAccountCommittedState(t *testing.T) {
	pool := pgtest.NewPool(t)
	q := accountdb.New(pool)
	ctx := context.Background()

	unknownKey := "never-created-" + uuid.NewString()
	_, err := InviteMember(ctx, q, InviteMemberRequest{Provider: "github", AccountKey: unknownKey, MemberRef: "octocat"})
	if !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("error = %v, want ErrAccountNotFound", err)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM account_members`).Scan(&count); err != nil {
		t.Fatalf("count members: %v", err)
	}
	if count != 0 {
		t.Errorf("account_members row count = %d after a fail-closed invite, want 0 (no implicit write)", count)
	}
}

// TestListMembers_RendersRosterWithAccountKey asserts the passthrough returns
// each grant JOINed with its owning account_key and origin.
func TestListMembers_RendersRosterWithAccountKey(t *testing.T) {
	pool := pgtest.NewPool(t)
	q := accountdb.New(pool)
	ctx := context.Background()

	if _, err := CreateAccount(ctx, q, CreateAccountRequest{Provider: "github", AccountKey: "acme", Granularity: "organization"}); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	if _, err := InviteMember(ctx, q, InviteMemberRequest{Provider: "github", AccountKey: "acme", MemberRef: "octocat", Role: "admin"}); err != nil {
		t.Fatalf("invite: %v", err)
	}

	members, err := ListMembers(ctx, q)
	if err != nil {
		t.Fatalf("ListMembers: %v", err)
	}
	if len(members) != 1 {
		t.Fatalf("member count = %d, want 1", len(members))
	}
	m := members[0]
	if m.AccountKey != "acme" || m.MemberRef != "octocat" || m.Origin != "invited" {
		t.Errorf("grant = %+v, want acme/octocat/invited", m)
	}
	if m.Role == nil || *m.Role != "admin" {
		t.Errorf("role = %v, want admin", m.Role)
	}
}
