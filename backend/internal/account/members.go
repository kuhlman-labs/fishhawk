package account

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	accountdb "github.com/kuhlman-labs/fishhawk/backend/internal/account/db"
)

// The operator membership surface (E44.34 / #2924). It is the supported write
// path for admitting the first human on a fresh self-hosted install — an
// account_members row with origin='invited' that the login gate's admission walk
// already honors DB-only (ADR-057 Amendment A2), replacing the hand-written SQL
// docs/deploy/self-hosted.md pointed operators at. InviteMember / ListMembers
// back the `fishhawkd member invite|list` subcommands; the CLI stays a thin
// flag-parsing shell over this domain surface so the logic is unit-testable
// against a fake.
//
// It composes with the shipped `fishhawkd account create` verb (registry.go):
// the operator creates the account, then invites the member into it. InviteMember
// FAILS CLOSED naming the account-create remedy when the account does not exist
// (ErrAccountNotFound, reused from registry.go), mirroring RegisterInstallation —
// admitting a human must be an operator decision recorded server-side, never one
// an admission path materializes.

// memberRoles is the allow-list InviteMemberRequest.Validate enforces.
// account_members.role is nullable TEXT with NO database CHECK (migration 0055),
// and Store.MemberRole (roles.go) resolves EVERY non-"admin" value — including a
// typo like "admn" — to member-tier (least privilege). So a mis-typed --role
// would be accepted by the database and silently produce a member-tier grant
// with no error at any layer. This Go-side allow-list is the ONLY guard against
// that and must reject out-of-set values.
var memberRoles = []string{RoleAdmin, RoleMember}

// DefaultInviteRole is the role an invite resolves to when --role is omitted:
// member-tier, least privilege.
const DefaultInviteRole = RoleMember

// MemberQueries is the query surface the operator membership verbs need.
// *accountdb.Queries (accountdb.New(pool)) satisfies it; tests inject a fake for
// the branches that need no database.
type MemberQueries interface {
	GetAccountByKey(ctx context.Context, arg accountdb.GetAccountByKeyParams) (accountdb.Account, error)
	UpsertInvitedAccountMember(ctx context.Context, arg accountdb.UpsertInvitedAccountMemberParams) (accountdb.UpsertInvitedAccountMemberRow, error)
	ListAccountMembersWithAccountKey(ctx context.Context) ([]accountdb.ListAccountMembersWithAccountKeyRow, error)
}

var _ MemberQueries = (*accountdb.Queries)(nil)

// InviteMemberRequest is the input to InviteMember.
type InviteMemberRequest struct {
	Provider   string
	AccountKey string
	MemberRef  string
	Role       string
}

// resolveDefaults trims every field and fills Role with DefaultInviteRole when
// empty. Trimming happens BEFORE validation so a whitespace-only member ref is
// rejected as empty and a padded role still matches the allow-list.
func (r InviteMemberRequest) resolveDefaults() InviteMemberRequest {
	out := InviteMemberRequest{
		Provider:   strings.TrimSpace(r.Provider),
		AccountKey: strings.TrimSpace(r.AccountKey),
		MemberRef:  strings.TrimSpace(r.MemberRef),
		Role:       strings.TrimSpace(r.Role),
	}
	if out.Role == "" {
		out.Role = DefaultInviteRole
	}
	return out
}

// Validate checks a RESOLVED request, each failure wrapping ErrValidation
// (reused from registry.go) and naming the offending value + flag: empty member
// ref, provider not in accountProviders, role not in memberRoles.
func (r InviteMemberRequest) Validate() error {
	if r.MemberRef == "" {
		return fmt.Errorf("account: member ref must be non-empty (--member-ref): %w", ErrValidation)
	}
	if !slices.Contains(accountProviders, r.Provider) {
		return fmt.Errorf("account: provider %q is not one of %s (--provider): %w",
			r.Provider, strings.Join(accountProviders, ", "), ErrValidation)
	}
	if !slices.Contains(memberRoles, r.Role) {
		return fmt.Errorf("account: role %q is not one of %s (--role): %w",
			r.Role, strings.Join(memberRoles, ", "), ErrValidation)
	}
	return nil
}

// InviteMember resolves defaults, validates, resolves the owning account by
// (provider, account_key), and upserts one origin='invited' account_members row
// pointing at it.
//
// It FAILS CLOSED when the named account does not exist (ErrAccountNotFound),
// naming the `fishhawkd account create` remedy, rather than materializing the
// account — the same load-bearing control RegisterInstallation records. Every
// input validation failure wraps ErrValidation. The upsert is idempotent on
// (account_id, provider, member_ref); a re-invite with a different role
// overwrites the role AND (for a prior auto_join grant) the origin.
func InviteMember(ctx context.Context, q MemberQueries, req InviteMemberRequest) (accountdb.UpsertInvitedAccountMemberRow, error) {
	resolved := req.resolveDefaults()
	if err := resolved.Validate(); err != nil {
		return accountdb.UpsertInvitedAccountMemberRow{}, err
	}

	acct, err := q.GetAccountByKey(ctx, accountdb.GetAccountByKeyParams{
		Provider:   resolved.Provider,
		AccountKey: resolved.AccountKey,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return accountdb.UpsertInvitedAccountMemberRow{}, fmt.Errorf(
				"account: no %s account with account_key %q; create it first with: fishhawkd account create --provider %s --account-key %s: %w",
				resolved.Provider, resolved.AccountKey, resolved.Provider, resolved.AccountKey, ErrAccountNotFound)
		}
		return accountdb.UpsertInvitedAccountMemberRow{}, err
	}

	role := resolved.Role
	return q.UpsertInvitedAccountMember(ctx, accountdb.UpsertInvitedAccountMemberParams{
		ID:        uuid.New(),
		AccountID: acct.ID,
		Provider:  resolved.Provider,
		MemberRef: resolved.MemberRef,
		Role:      &role,
	})
}

// ListMembers returns every membership grant JOINed with its owning account's
// account_key. A passthrough so the CLI depends on this domain package, not on
// accountdb, for both verbs.
func ListMembers(ctx context.Context, q MemberQueries) ([]accountdb.ListAccountMembersWithAccountKeyRow, error) {
	return q.ListAccountMembersWithAccountKey(ctx)
}
