package account

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	accountdb "github.com/kuhlman-labs/fishhawk/backend/internal/account/db"
)

// fakeAccountKeyLister serves seeded rows keyed by account_key (real lookup
// semantics — a query for an unseeded key returns zero rows), records the key
// it was queried with, counts calls, and can be programmed to fail.
type fakeAccountKeyLister struct {
	rows    map[string][]accountdb.Account
	err     error
	calls   int
	lastKey string
}

func (f *fakeAccountKeyLister) ListAccountsByAccountKey(_ context.Context, accountKey string) ([]accountdb.Account, error) {
	f.calls++
	f.lastKey = accountKey
	if f.err != nil {
		return nil, f.err
	}
	return f.rows[accountKey], nil
}

func acctRow(provider, key string) accountdb.Account {
	return accountdb.Account{ID: uuid.New(), Provider: provider, AccountKey: key, Granularity: "organization"}
}

// Exactly one row: the provider resolves. Also the assumption check the plan
// names — account_key holds the forge owner login, so seeding account_key=owner
// and resolving "<owner>/<name>" must query BY that owner segment and return
// the seeded provider; mismatched key semantics fail here.
func TestResolveProvider_ExactlyOneRowResolves(t *testing.T) {
	for _, tc := range []struct{ provider, owner string }{
		{"github", "kuhlman-labs"},
		{"gitlab", "some-group"},
	} {
		f := &fakeAccountKeyLister{rows: map[string][]accountdb.Account{
			tc.owner: {acctRow(tc.provider, tc.owner)},
		}}
		r := NewResolver(f)
		provider, found, err := r.ResolveProvider(context.Background(), tc.owner+"/repo")
		if err != nil {
			t.Fatalf("ResolveProvider error: %v", err)
		}
		if !found || provider != tc.provider {
			t.Errorf("ResolveProvider(%s/repo) = (%q,%v), want (%q,true)", tc.owner, provider, found, tc.provider)
		}
		if f.lastKey != tc.owner {
			t.Errorf("queried account_key = %q, want owner segment %q", f.lastKey, tc.owner)
		}
	}
}

func TestResolveProvider_ZeroRowsNotFound(t *testing.T) {
	f := &fakeAccountKeyLister{rows: map[string][]accountdb.Account{}}
	r := NewResolver(f)
	provider, found, err := r.ResolveProvider(context.Background(), "unregistered/repo")
	if err != nil {
		t.Fatalf("ResolveProvider error: %v", err)
	}
	if found || provider != "" {
		t.Errorf("ResolveProvider = (%q,%v), want (\"\",false)", provider, found)
	}
	if f.calls != 1 {
		t.Errorf("lister calls = %d, want 1", f.calls)
	}
}

// The ambiguity case the plan requires: accounts.UNIQUE(provider, account_key)
// permits the SAME account_key under BOTH providers. Seeding "acme" under both
// github AND gitlab must resolve found=false — a clean fall-through, never an
// arbitrary first row.
func TestResolveProvider_SameKeyUnderBothProvidersIsAmbiguous(t *testing.T) {
	f := &fakeAccountKeyLister{rows: map[string][]accountdb.Account{
		"acme": {acctRow("github", "acme"), acctRow("gitlab", "acme")},
	}}
	r := NewResolver(f)
	provider, found, err := r.ResolveProvider(context.Background(), "acme/widget")
	if err != nil {
		t.Fatalf("ResolveProvider error: %v", err)
	}
	if found || provider != "" {
		t.Errorf("ambiguous key resolved (%q,%v), want (\"\",false) — must never pick an arbitrary row", provider, found)
	}
}

// A query error is propagated (found=false, err non-nil) so the caller can
// fail closed rather than silently selecting a different provider.
func TestResolveProvider_QueryErrorPropagates(t *testing.T) {
	boom := errors.New("connection refused")
	f := &fakeAccountKeyLister{err: boom}
	r := NewResolver(f)
	provider, found, err := r.ResolveProvider(context.Background(), "acme/widget")
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v propagated", err, boom)
	}
	if found || provider != "" {
		t.Errorf("ResolveProvider = (%q,%v) on error, want (\"\",false)", provider, found)
	}
}

// A malformed repo (no "/", or an empty owner segment) reports not-found
// WITHOUT a query.
func TestResolveProvider_MalformedRepoNotFoundNoQuery(t *testing.T) {
	f := &fakeAccountKeyLister{rows: map[string][]accountdb.Account{
		"": {acctRow("github", "")},
	}}
	r := NewResolver(f)
	for _, repo := range []string{"no-slash", "/leading-slash", ""} {
		provider, found, err := r.ResolveProvider(context.Background(), repo)
		if err != nil || found || provider != "" {
			t.Errorf("ResolveProvider(%q) = (%q,%v,%v), want (\"\",false,nil)", repo, provider, found, err)
		}
	}
	if f.calls != 0 {
		t.Errorf("lister calls = %d, want 0 (malformed repo must not query)", f.calls)
	}
}

// A nil Resolver / nil lister is the no-database posture: not-found without a
// panic, degrading to the loader's fallback chain.
func TestResolveProvider_NilResolverNotFound(t *testing.T) {
	var r *Resolver
	provider, found, err := r.ResolveProvider(context.Background(), "acme/widget")
	if err != nil || found || provider != "" {
		t.Errorf("nil Resolver = (%q,%v,%v), want (\"\",false,nil)", provider, found, err)
	}
	provider, found, err = NewResolver(nil).ResolveProvider(context.Background(), "acme/widget")
	if err != nil || found || provider != "" {
		t.Errorf("nil-lister Resolver = (%q,%v,%v), want (\"\",false,nil)", provider, found, err)
	}
}
