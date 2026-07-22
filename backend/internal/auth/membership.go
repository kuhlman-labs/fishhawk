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
//   - origin='invited' rows admit DB-ONLY — the resolver checks for an
//     invited grant FIRST and, if one exists, admits its account(s) and
//     returns WITHOUT making any forge call at all. Invited admission is
//     forge-independent: immune to forge latency, hang, availability, or
//     egress, not merely tolerant of a forge error.
//   - origin='auto_join' rows are minted at login by the auto-join
//     bootstrap and RE-VERIFIED against their policy predicate at every
//     subsequent login; a row whose predicate no longer holds stops
//     admitting but is kept for audit.
//
// The live forge read is reached ONLY on the auto_join path — when no
// invited grant admits. It is intersected with accounts whose
// auto_join_role policy is set; a match with no existing grant mints an
// origin='auto_join' row and admits. A forge error there fails the
// auto_join eval CLOSED (and, with no invited grant, the whole sign-in).
// A provider with NO registered lister denies.
//
// E44.8 (#1832) generalizes the auto_join sources from one to three,
// all behind this same seam:
//
//   - organization (github) — GET /user/orgs with the user's OAuth token.
//   - enterprise (github, EMU posture only) — the enterprise short code
//     derived from the EMU login itself (see emu.go). No forge call.
//   - group (gitlab) — GET /api/v4/groups with the user's OAuth token
//     (see gitlab_membership.go). SEAM-FIRST: no GitLab browser sign-in
//     flow exists yet, so provider="gitlab" is not reachable in
//     production until that flow lands (operator-filed follow-up).
//
// Every derived membership key stays BOUND to the granularity it was
// derived from — the admission set is a list of (key, granularity)
// PAIRS, never a cartesian product of a key set and a granularity set.
// An org key "acme" must not admit an enterprise account keyed "acme",
// and a derived enterprise short code must not admit an organization
// account of the same key.

// Member-grant origins (account_members.origin, migration 0056).
const (
	MemberOriginInvited  = "invited"
	MemberOriginAutoJoin = "auto_join"
)

// Providers the resolver can dispatch. A provider is only actually
// resolvable when a lister is registered for it (serve.go wiring).
const (
	providerGitHub = "github"
	providerGitLab = "gitlab"
)

// Auto-join granularities (accounts.granularity, migration 0052).
const (
	// granularityOrganization: GitHub org membership, the granularity
	// a user OAuth token verifies via GET /user/orgs.
	granularityOrganization = "organization"
	// granularityEnterprise: GitHub Enterprise Cloud, derived from an
	// EMU login's short code under EMU posture only.
	granularityEnterprise = "enterprise"
	// granularityGroup: GitLab group membership (full_path).
	granularityGroup = "group"
)

// MembershipKey is one derived membership fact: a forge key BOUND to
// the granularity it was derived from. The pairing is load-bearing —
// see the cartesian-product note above.
type MembershipKey struct {
	Key         string
	Granularity string
}

// MembershipResolver decides which workspace account(s) admit a
// logging-in forge user. Consulted by the OAuth callback BEFORE
// SignIn; an empty result denies the sign-in (no session), an error
// fails closed.
type MembershipResolver interface {
	ResolveAccounts(ctx context.Context, provider, accessToken string, profile GitHubProfile) ([]uuid.UUID, error)
}

// ForgeMembershipLister is the live-forge slice of the resolver: the
// authenticated user's membership keys for that forge's auto-join
// granularity. *GitHubOAuth implements it via ListUserOrgKeys (org
// logins); *GitLabMembershipLister implements it via the groups API
// (group full_paths).
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

// AutoJoinAccount is an account whose auto_join_role policy is set and
// whose (account_key, granularity) PAIR matched one of the user's
// derived membership keys.
type AutoJoinAccount struct {
	AccountID    uuid.UUID
	AccountKey   string
	Granularity  string
	AutoJoinRole string
}

// MembershipStore is the persistence slice the resolver needs.
// Backed by account/db in production (NewAccountMembershipStore).
type MembershipStore interface {
	// ListMemberGrants returns every account_members row for
	// (provider, memberRef), joined with each account's admission
	// fields.
	ListMemberGrants(ctx context.Context, provider, memberRef string) ([]MemberGrant, error)

	// ListAutoJoinAccounts returns the accounts whose auto-join policy
	// predicate matches the given derived membership pairs: provider
	// match, non-NULL auto_join_role, and (account_key, granularity)
	// equal to one of the pairs. Pair-wise — NOT the cartesian product
	// of the keys and the granularities.
	ListAutoJoinAccounts(ctx context.Context, provider string, pairs []MembershipKey) ([]AutoJoinAccount, error)

	// UpsertAutoJoinGrant mints (or refreshes) an origin='auto_join'
	// account_members row for the member.
	UpsertAutoJoinGrant(ctx context.Context, id, accountID uuid.UUID, provider, memberRef, role string) error
}

type forgeMembershipResolver struct {
	store        MembershipStore
	listers      map[string]ForgeMembershipLister
	emuOAuthHost string
	newID        func() uuid.UUID
}

// ResolverOption configures the membership resolver.
type ResolverOption func(*forgeMembershipResolver)

// WithEMUOAuthHost carries the deployment's configured GitHub OAuth
// base URL so the resolver can decide EMU posture (IsEMUOAuthHost).
// Enterprise-granularity auto-join is derived ONLY under that posture:
// on github.com a login cannot contain an underscore, so treating one
// as an enterprise short code would let a crafted login claim an
// enterprise.
func WithEMUOAuthHost(oauthBaseURL string) ResolverOption {
	return func(r *forgeMembershipResolver) { r.emuOAuthHost = oauthBaseURL }
}

// NewMembershipResolver builds the production resolver from the
// persistence store and a provider-keyed map of live forge listers. A
// provider absent from the map denies (fail-closed: an unconfigured
// forge never admits).
func NewMembershipResolver(store MembershipStore, listers map[string]ForgeMembershipLister, opts ...ResolverOption) MembershipResolver {
	r := &forgeMembershipResolver{store: store, listers: listers, newID: uuid.New}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// ResolveAccounts implements the Amendment A2 admission walk. The
// returned IDs are sorted (deterministic-first: the callback binds the
// session to index 0). Empty result = deny; error = fail closed.
func (r *forgeMembershipResolver) ResolveAccounts(ctx context.Context, provider, accessToken string, profile GitHubProfile) ([]uuid.UUID, error) {
	lister := r.listers[provider]
	if lister == nil {
		// No lister registered for this provider (an unconfigured
		// forge): deny, without touching the store.
		return nil, nil
	}
	if profile.Login == "" {
		return nil, errors.New("auth: membership resolution requires a login")
	}

	grants, err := r.store.ListMemberGrants(ctx, provider, profile.Login)
	if err != nil {
		return nil, fmt.Errorf("auth: list member grants: %w", err)
	}

	// Invited rows admit DB-ONLY. Check for an invited grant FIRST and,
	// if any exists, admit its account(s) and return WITHOUT any forge
	// call — invited admission must be immune to forge latency, hang,
	// availability, and egress, not merely tolerant of a forge error
	// (ADR-057 Amendment A2). The live-forge read below is reached only
	// when no invited grant admits, scoping it to the auto_join path.
	invited := map[uuid.UUID]bool{}
	for _, g := range grants {
		if g.Origin == MemberOriginInvited {
			invited[g.AccountID] = true
		}
	}
	if len(invited) > 0 {
		return sortedAccountIDs(invited), nil
	}

	// No invited grant: the auto_join path, the ONLY live-forge read. An
	// error here fails the auto_join eval — and, since no invited grant
	// admits, the whole sign-in — CLOSED.
	liveKeys, forgeErr := lister.ListUserOrgKeys(ctx, accessToken)
	if forgeErr != nil {
		return nil, fmt.Errorf("auth: forge membership list failed and no invited grant admits: %w", forgeErr)
	}

	pairs := r.derivePairs(provider, profile.Login, liveKeys)
	pairSet := make(map[MembershipKey]bool, len(pairs))
	for _, p := range pairs {
		pairSet[p] = true
	}

	admitted := map[uuid.UUID]bool{}

	// Re-verify existing auto_join grants against their predicate: the
	// account's policy must still be set and its (account_key,
	// granularity) PAIR must still be one the user's memberships derive
	// — never a cross-granularity key match. A failed predicate stops
	// admitting; the row is kept for audit (suspended, not deleted).
	haveGrant := make(map[uuid.UUID]bool, len(grants))
	for _, g := range grants {
		haveGrant[g.AccountID] = true
		if g.Origin == MemberOriginAutoJoin &&
			g.AutoJoinRole != nil &&
			pairSet[MembershipKey{Key: g.AccountKey, Granularity: g.Granularity}] {
			admitted[g.AccountID] = true
		}
	}

	// Bootstrap: a matching policy with NO existing grant (of any
	// origin) mints an audited origin='auto_join' row and admits.
	if len(pairs) > 0 {
		policies, err := r.store.ListAutoJoinAccounts(ctx, provider, pairs)
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

// derivePairs turns a provider's live membership listing (plus, for
// GitHub under EMU posture, the login itself) into the (key,
// granularity) pairs the auto-join predicate matches on. Each key is
// bound to the granularity it was derived from; duplicates are dropped
// so a repeated key never mints twice.
func (r *forgeMembershipResolver) derivePairs(provider, login string, liveKeys []string) []MembershipKey {
	granularity := granularityOrganization
	if provider == providerGitLab {
		granularity = granularityGroup
	}

	pairs := make([]MembershipKey, 0, len(liveKeys)+1)
	seen := make(map[MembershipKey]bool, len(liveKeys)+1)
	add := func(p MembershipKey) {
		if p.Key == "" || seen[p] {
			return
		}
		seen[p] = true
		pairs = append(pairs, p)
	}
	for _, k := range liveKeys {
		add(MembershipKey{Key: k, Granularity: granularity})
	}

	// EMU enterprise membership: derived from the login's short code,
	// and ONLY under EMU posture. On github.com posture no enterprise
	// key is derived at all — a public login cannot contain an
	// underscore, so an ungated derivation would be a spoofing surface.
	if provider == providerGitHub && IsEMUOAuthHost(r.emuOAuthHost) {
		if code, ok := EnterpriseShortCode(login); ok {
			add(MembershipKey{Key: code, Granularity: granularityEnterprise})
		}
	}
	return pairs
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

func (s *accountMembershipStore) ListAutoJoinAccounts(ctx context.Context, provider string, pairs []MembershipKey) ([]AutoJoinAccount, error) {
	// Two POSITIONALLY PAIRED arrays: the query unnests them together,
	// so index i's key only ever matches index i's granularity.
	keys := make([]string, 0, len(pairs))
	granularities := make([]string, 0, len(pairs))
	for _, p := range pairs {
		keys = append(keys, p.Key)
		granularities = append(granularities, p.Granularity)
	}
	rows, err := s.q.ListAutoJoinAccountsByKeys(ctx, accountdb.ListAutoJoinAccountsByKeysParams{
		Provider:      provider,
		AccountKeys:   keys,
		Granularities: granularities,
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
			Granularity:  row.Granularity,
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
