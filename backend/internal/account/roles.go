// Package account carries the tenancy identity persistence surface (accounts,
// installations, account_members; ADR-057 / ADR-058) and the handler-authz
// role reader the server's account-ownership middleware consults (E44.5 /
// #1829). The sqlc-generated query surface lives in the account/db subpackage.
package account

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	accountdb "github.com/kuhlman-labs/fishhawk/backend/internal/account/db"
)

// Role tiers the handler-authz middleware bounds a write against (E44.5 /
// #1829). account_members.role is nullable TEXT with no CHECK (migration
// 0055), so the admin/member interpretation is introduced here in Go: an
// explicit "admin" grants the full surface (destructive + admin routes); every
// other value — "member", an unknown string, a NULL role, or no membership —
// resolves to member-tier (least privilege).
const (
	RoleAdmin  = "admin"
	RoleMember = "member"
)

// RoleReader is the query surface Store needs. *accountdb.Queries (constructed
// via accountdb.New(pool)) satisfies it; tests inject a fake.
type RoleReader interface {
	GetAccountMemberRole(ctx context.Context, arg accountdb.GetAccountMemberRoleParams) (*string, error)
}

// Store resolves a caller's role within a tenant account (E44.5 / #1829). It is
// the forge-agnostic role lookup the server's account-ownership middleware
// consults on the cookie write-tier path: it derives the forge-neutral
// member_ref by stripping the "<provider>:" prefix from the identity subject
// GENERICALLY (never a hard-coded "github:" literal), so github:, gitlab:, and
// any future forge all resolve.
type Store struct {
	q RoleReader
}

// NewStore wraps a role reader (accountdb.New(pool)) into a Store. A nil reader
// is tolerated: MemberRole then returns "" (member-tier) — the untenanted-allow
// posture for a deployment without a database.
func NewStore(q RoleReader) *Store {
	return &Store{q: q}
}

// MemberRole returns the caller's role in accountID, or "" when there is no
// membership grant, the grant carries a NULL role, or the store is unwired —
// all member-tier (least privilege). accountID is the account UUID string;
// provider is the forge discriminator ("github", "gitlab", …); subject is the
// identity subject "<provider>:<member_ref>" the member_ref is stripped from
// generically. An empty accountID/provider or a non-UUID accountID returns ""
// without a query (defensive — the caller supplies a resolved Identity).
//
// A genuine query error (not pgx.ErrNoRows) is surfaced so the middleware can
// fail closed (503) rather than silently granting or denying on a transient DB
// fault.
func (s *Store) MemberRole(ctx context.Context, accountID, provider, subject string) (string, error) {
	if s == nil || s.q == nil {
		return "", nil
	}
	if accountID == "" || provider == "" {
		return "", nil
	}
	aid, err := uuid.Parse(accountID)
	if err != nil {
		return "", nil
	}
	// Forge-agnostic member_ref: strip the "<provider>:" prefix generically.
	// A subject that lacks the prefix (unexpected) is used verbatim.
	memberRef := strings.TrimPrefix(subject, provider+":")
	role, err := s.q.GetAccountMemberRole(ctx, accountdb.GetAccountMemberRoleParams{
		AccountID: aid,
		Provider:  provider,
		MemberRef: memberRef,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if role == nil {
		return "", nil
	}
	return *role, nil
}
