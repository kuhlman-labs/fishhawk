package account

import (
	"context"
	"strings"

	accountdb "github.com/kuhlman-labs/fishhawk/backend/internal/account/db"
)

// KeyLister is the query surface Resolver needs. *accountdb.Queries
// (constructed via accountdb.New(pool)) satisfies it; tests inject a fake.
type KeyLister interface {
	ListAccountsByAccountKey(ctx context.Context, accountKey string) ([]accountdb.Account, error)
}

var _ KeyLister = (*accountdb.Queries)(nil)

// Resolver resolves the ADR-057/ADR-058 tenancy provider discriminator for a
// repo: which forge ("github", "gitlab", …) the repo's owner is registered
// under in the accounts table. It is the sole out-of-file hint the per-repo
// conventions loader (E45.16 / #2022) consults before falling back to the
// deployment override / workmgmt.Default().
type Resolver struct {
	q KeyLister
}

// NewResolver wraps an account-key lister (accountdb.New(pool)) into a
// Resolver. A nil lister is tolerated: ResolveProvider then reports not-found —
// the no-database posture degrades to the loader's fallback chain.
func NewResolver(q KeyLister) *Resolver {
	return &Resolver{q: q}
}

// ResolveProvider looks up the provider for repo ("owner/name") by using the
// owner segment as the accounts.account_key. Multi-row semantics are explicit:
// exactly one row resolves (provider, true, nil); zero rows report not-found;
// and because accounts.UNIQUE(provider, account_key) (migration 0052) permits
// the SAME account_key under BOTH providers, more than one row is AMBIGUOUS and
// also reports not-found — never an arbitrary first row, so an ambiguous key
// falls through cleanly to the loader's override/Default chain. A query error
// is propagated so the caller can fail closed rather than silently selecting a
// different provider on a transient DB fault.
//
// A repo with no "/" or an empty owner segment is malformed and reports
// not-found without a query (defensive — callers supply "owner/name").
func (r *Resolver) ResolveProvider(ctx context.Context, repo string) (provider string, found bool, err error) {
	if r == nil || r.q == nil {
		return "", false, nil
	}
	owner, _, ok := strings.Cut(repo, "/")
	if !ok || owner == "" {
		return "", false, nil
	}
	rows, err := r.q.ListAccountsByAccountKey(ctx, owner)
	if err != nil {
		return "", false, err
	}
	if len(rows) != 1 {
		return "", false, nil
	}
	return rows[0].Provider, true, nil
}
