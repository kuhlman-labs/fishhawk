package auth

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/google/uuid"

	accountdb "github.com/kuhlman-labs/fishhawk/backend/internal/account/db"
)

// Workspace-membership login gate (E44.3 / ADR-057 Amendment A2).
//
// The admission SOURCE is account_members rows, not a live forge match:
//
//   - origin='invited' rows admit DB-ONLY — no forge call on that path,
//     so forge-API availability can never lock out an invited member.
//   - origin='auto_join' rows are minted at login by the auto-join
//     bootstrap and RE-VERIFIED against their policy predicate at every
//     subsequent login; a row whose predicate no longer holds stops
//     admitting but is kept for audit.
//
// The auto-join bootstrap is the ONLY live-forge read: the user's org
// list (GET /user/orgs with the user's OAuth token) is intersected with
// organization-granularity accounts whose auto_join_role policy is set;
// a match with no existing grant mints an origin='auto_join' row and
// admits. A forge error fails auto-join eval CLOSED without touching
// invited admission. Providers with no lister implementation (gitlab
// today) deny.

// Member-grant origins (account_members.origin, migration 0056).
const (
	MemberOriginInvited  = "invited"
	MemberOriginAutoJoin = "auto_join"
)

// providerGitHub is the only provider the resolver implements today.
const providerGitHub = "github"

// granularityOrganization is the sole granularity auto-join policies
// anchor to: App installations are org-scoped, so org membership is
// what a user OAuth token can actually verify. Enterprise tenants are
// workspaces owning several org installations, admitted via invited
// rows or org-scoped auto-join — never an enterprise-membership API.
const granularityOrganization = "organization"

// MembershipResolver decides which workspace account(s) admit a
// logging-in forge user. Consulted by the OAuth callback BEFORE
// SignIn; an empty result denies the sign-in (no session), an error
// fails closed.
type MembershipResolver interface {
	ResolveAccounts(ctx context.Context, provider, accessToken string, profile GitHubProfile) ([]uuid.UUID, error)
}

// ForgeMembershipLister is the live-forge slice of the resolver:
// the authenticated user's org membership keys. *GitHubOAuth
// implements it via ListUserOrgKeys.
type ForgeMembershipLister interface {
	ListUserOrgKeys(ctx context.Context, accessToken string) ([]string, error)
}

// MemberGrant is one account_members row joined with the admission
// fields of its account, as the resolver consumes it.
type MemberGrant struct {
	AccountID   uuid.UUID
	Origin      string // MemberOriginInvited | MemberOriginAutoJoin
	AccountKey  string
	Granularity string
	// AutoJoinRole is the account's auto-join policy role; nil means
	// the account has no auto-join policy.
	AutoJoinRole *string
}

// AutoJoinAccount is an organization-granularity account whose
// auto_join_role policy is set and whose account_key matched the
// user's live org list.
type AutoJoinAccount struct {
	AccountID    uuid.UUID
	AccountKey   string
	AutoJoinRole string
}

// MembershipStore is the persistence slice the resolver needs.
// Backed by account/db in production (NewAccountMembershipStore).
type MembershipStore interface {
	// ListMemberGrants returns every account_members row for
	// (provider, memberRef), joined with each account's admission
	// fields.
	ListMemberGrants(ctx context.Context, provider, memberRef string) ([]MemberGrant, error)

	// ListAutoJoinAccounts returns the accounts whose auto-join
	// policy predicate matches the given live membership keys:
	// provider match, organization granularity, non-NULL
	// auto_join_role, account_key in keys.
	ListAutoJoinAccounts(ctx context.Context, provider string, keys []string) ([]AutoJoinAccount, error)

	// UpsertAutoJoinGrant mints (or refreshes) an origin='auto_join'
	// account_members row for the member.
	UpsertAutoJoinGrant(ctx context.Context, id, accountID uuid.UUID, provider, memberRef, role string) error
}

type forgeMembershipResolver struct {
	store  MembershipStore
	lister ForgeMembershipLister
	newID  func() uuid.UUID
}

// NewMembershipResolver builds the production resolver from the
// persistence store and the live forge lister.
func NewMembershipResolver(store MembershipStore, lister ForgeMembershipLister) MembershipResolver {
	return &forgeMembershipResolver{store: store, lister: lister, newID: uuid.New}
}

// ResolveAccounts implements the Amendment A2 admission walk. The
// returned IDs are sorted (deterministic-first: the callback binds the
// session to index 0). Empty result = deny; error = fail closed.
func (r *forgeMembershipResolver) ResolveAccounts(ctx context.Context, provider, accessToken string, profile GitHubProfile) ([]uuid.UUID, error) {
	if provider != providerGitHub {
		// No resolver implementation for this provider yet (GitLab is
		// a documented additive follow-on): deny.
		return nil, nil
	}
	if profile.Login == "" {
		return nil, errors.New("auth: membership resolution requires a login")
	}

	grants, err := r.store.ListMemberGrants(ctx, provider, profile.Login)
	if err != nil {
		return nil, fmt.Errorf("auth: list member grants: %w", err)
	}

	admitted := map[uuid.UUID]bool{}
	for _, g := range grants {
		if g.Origin == MemberOriginInvited {
			// Invited rows admit DB-only — deliberately decided before
			// any forge call so forge availability can't affect them.
			admitted[g.AccountID] = true
		}
	}

	// Auto-join evaluation — the ONLY live-forge read. An error here
	// fails auto-join eval closed: invited admissions above stand,
	// and if none exist the whole sign-in fails closed.
	keys, forgeErr := r.lister.ListUserOrgKeys(ctx, accessToken)
	if forgeErr != nil {
		if len(admitted) == 0 {
			return nil, fmt.Errorf("auth: forge membership list failed and no invited grant admits: %w", forgeErr)
		}
		return sortedAccountIDs(admitted), nil
	}

	keySet := make(map[string]bool, len(keys))
	for _, k := range keys {
		keySet[k] = true
	}

	// Re-verify existing auto_join grants against their predicate: the
	// account's policy must still be set and the user's live org list
	// must still contain the account's org. A failed predicate stops
	// admitting; the row is kept for audit (suspended, not deleted).
	haveGrant := make(map[uuid.UUID]bool, len(grants))
	for _, g := range grants {
		haveGrant[g.AccountID] = true
		if g.Origin == MemberOriginAutoJoin &&
			g.AutoJoinRole != nil &&
			g.Granularity == granularityOrganization &&
			keySet[g.AccountKey] {
			admitted[g.AccountID] = true
		}
	}

	// Bootstrap: a matching policy with NO existing grant (of any
	// origin) mints an audited origin='auto_join' row and admits.
	if len(keys) > 0 {
		policies, err := r.store.ListAutoJoinAccounts(ctx, provider, keys)
		if err != nil {
			return nil, fmt.Errorf("auth: list auto-join accounts: %w", err)
		}
		for _, p := range policies {
			if haveGrant[p.AccountID] {
				continue
			}
			if err := r.store.UpsertAutoJoinGrant(ctx, r.newID(), p.AccountID, provider, profile.Login, p.AutoJoinRole); err != nil {
				// The minted row IS the audit record of the admission;
				// if it can't be written the admission doesn't happen.
				return nil, fmt.Errorf("auth: mint auto-join grant: %w", err)
			}
			admitted[p.AccountID] = true
		}
	}

	return sortedAccountIDs(admitted), nil
}

// sortedAccountIDs returns the admitted set in a stable order so the
// callback's "first account" pick is deterministic across logins.
func sortedAccountIDs(set map[uuid.UUID]bool) []uuid.UUID {
	ids := make([]uuid.UUID, 0, len(set))
	for id := range set {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i].String() < ids[j].String() })
	return ids
}

// accountMembershipStore adapts the account/db sqlc queries to
// MembershipStore.
type accountMembershipStore struct {
	q *accountdb.Queries
}

// NewAccountMembershipStore wraps the account/db query layer as the
// resolver's MembershipStore.
func NewAccountMembershipStore(q *accountdb.Queries) MembershipStore {
	return &accountMembershipStore{q: q}
}

func (s *accountMembershipStore) ListMemberGrants(ctx context.Context, provider, memberRef string) ([]MemberGrant, error) {
	rows, err := s.q.ListMemberGrantsByRef(ctx, accountdb.ListMemberGrantsByRefParams{
		Provider:  provider,
		MemberRef: memberRef,
	})
	if err != nil {
		return nil, err
	}
	out := make([]MemberGrant, 0, len(rows))
	for _, row := range rows {
		out = append(out, MemberGrant{
			AccountID:    row.AccountID,
			Origin:       row.Origin,
			AccountKey:   row.AccountKey,
			Granularity:  row.Granularity,
			AutoJoinRole: row.AutoJoinRole,
		})
	}
	return out, nil
}

func (s *accountMembershipStore) ListAutoJoinAccounts(ctx context.Context, provider string, keys []string) ([]AutoJoinAccount, error) {
	rows, err := s.q.ListAutoJoinAccountsByKeys(ctx, accountdb.ListAutoJoinAccountsByKeysParams{
		Provider:    provider,
		AccountKeys: keys,
	})
	if err != nil {
		return nil, err
	}
	out := make([]AutoJoinAccount, 0, len(rows))
	for _, row := range rows {
		role := ""
		if row.AutoJoinRole != nil {
			role = *row.AutoJoinRole
		}
		out = append(out, AutoJoinAccount{
			AccountID:    row.ID,
			AccountKey:   row.AccountKey,
			AutoJoinRole: role,
		})
	}
	return out, nil
}

func (s *accountMembershipStore) UpsertAutoJoinGrant(ctx context.Context, id, accountID uuid.UUID, provider, memberRef, role string) error {
	return s.q.UpsertAccountMemberWithOrigin(ctx, accountdb.UpsertAccountMemberWithOriginParams{
		ID:        id,
		AccountID: accountID,
		Provider:  provider,
		MemberRef: memberRef,
		Role:      &role,
		Origin:    MemberOriginAutoJoin,
	})
}

// Compile-time checks.
var (
	_ ForgeMembershipLister = (*GitHubOAuth)(nil)
	_ MembershipStore       = (*accountMembershipStore)(nil)
)
