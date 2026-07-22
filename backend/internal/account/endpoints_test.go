package account

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	accountdb "github.com/kuhlman-labs/fishhawk/backend/internal/account/db"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
)

// The set-column / NULL-column / not-found branches are exercised against a
// REAL migrated database via pgtest.NewPool and the production accountdb.Queries
// (binding condition 2), so the (provider, installation_ref) → row mapping and
// the *string NULL semantics are covered end-to-end, not merely at a fake seam.

// seedInstallation inserts a github account plus one installation carrying the
// given endpoint columns (nil → SQL NULL), and returns the installation_ref.
func seedInstallation(t *testing.T, pool *pgxpool.Pool, ref string, forgeBase, oauthBase *string) {
	t.Helper()
	ctx := context.Background()
	q := accountdb.New(pool)
	acct, err := q.UpsertAccount(ctx, accountdb.UpsertAccountParams{
		ID:          uuid.New(),
		Provider:    "github",
		AccountKey:  "acme-" + ref,
		Granularity: "enterprise",
	})
	if err != nil {
		t.Fatalf("UpsertAccount: %v", err)
	}
	if _, err := q.UpsertInstallation(ctx, accountdb.UpsertInstallationParams{
		ID:              uuid.New(),
		AccountID:       acct.ID,
		Provider:        "github",
		InstallationRef: ref,
		ForgeBaseUrl:    forgeBase,
		OauthBaseUrl:    oauthBase,
	}); err != nil {
		t.Fatalf("UpsertInstallation: %v", err)
	}
}

func TestEndpointResolver_SetColumns(t *testing.T) {
	pool := pgtest.NewPool(t)
	r := NewEndpointResolver(accountdb.New(pool))

	forge := "https://acme.ghe.com/api/v3"
	oauth := "https://acme.ghe.com"
	seedInstallation(t, pool, "1001", &forge, &oauth)

	gotForge, gotOAuth, err := r.ResolveInstallationEndpoints(context.Background(), "github", "1001")
	if err != nil {
		t.Fatalf("ResolveInstallationEndpoints: %v", err)
	}
	if gotForge != forge {
		t.Errorf("forgeBaseURL = %q, want %q", gotForge, forge)
	}
	if gotOAuth != oauth {
		t.Errorf("oauthBaseURL = %q, want %q", gotOAuth, oauth)
	}
}

func TestEndpointResolver_NullColumns(t *testing.T) {
	pool := pgtest.NewPool(t)
	r := NewEndpointResolver(accountdb.New(pool))

	// Installation exists but both endpoint columns are NULL → the intentional
	// absence of an override → deployment default (empty, empty, no error).
	seedInstallation(t, pool, "1002", nil, nil)

	gotForge, gotOAuth, err := r.ResolveInstallationEndpoints(context.Background(), "github", "1002")
	if err != nil {
		t.Fatalf("ResolveInstallationEndpoints: %v", err)
	}
	if gotForge != "" || gotOAuth != "" {
		t.Errorf("NULL columns → (%q, %q), want empty (deployment default)", gotForge, gotOAuth)
	}
}

func TestEndpointResolver_SetForge_NullOAuth(t *testing.T) {
	pool := pgtest.NewPool(t)
	r := NewEndpointResolver(accountdb.New(pool))

	// Mixed boundary: forge_base_url SET, oauth_base_url NULL. Each column is
	// honored independently — the set forge is returned, the NULL oauth is the
	// deployment default (empty).
	forge := "https://acme.ghe.com/api/v3"
	seedInstallation(t, pool, "1003", &forge, nil)

	gotForge, gotOAuth, err := r.ResolveInstallationEndpoints(context.Background(), "github", "1003")
	if err != nil {
		t.Fatalf("ResolveInstallationEndpoints: %v", err)
	}
	if gotForge != forge {
		t.Errorf("forgeBaseURL = %q, want %q", gotForge, forge)
	}
	if gotOAuth != "" {
		t.Errorf("oauthBaseURL = %q, want empty (NULL oauth → deployment default)", gotOAuth)
	}
}

func TestEndpointResolver_NullForge_SetOAuth(t *testing.T) {
	pool := pgtest.NewPool(t)
	r := NewEndpointResolver(accountdb.New(pool))

	// Mixed boundary: forge_base_url NULL, oauth_base_url SET. The NULL forge is
	// the deployment default (empty); the set oauth is returned independently.
	oauth := "https://acme.ghe.com"
	seedInstallation(t, pool, "1004", nil, &oauth)

	gotForge, gotOAuth, err := r.ResolveInstallationEndpoints(context.Background(), "github", "1004")
	if err != nil {
		t.Fatalf("ResolveInstallationEndpoints: %v", err)
	}
	if gotForge != "" {
		t.Errorf("forgeBaseURL = %q, want empty (NULL forge → deployment default)", gotForge)
	}
	if gotOAuth != oauth {
		t.Errorf("oauthBaseURL = %q, want %q", gotOAuth, oauth)
	}
}

func TestEndpointResolver_NotFound(t *testing.T) {
	pool := pgtest.NewPool(t)
	r := NewEndpointResolver(accountdb.New(pool))

	// No row for this ref (pgx.ErrNoRows) → deployment default, not an error.
	gotForge, gotOAuth, err := r.ResolveInstallationEndpoints(context.Background(), "github", "does-not-exist")
	if err != nil {
		t.Fatalf("not-found should not error, got: %v", err)
	}
	if gotForge != "" || gotOAuth != "" {
		t.Errorf("not-found → (%q, %q), want empty (deployment default)", gotForge, gotOAuth)
	}
}

// fakeInstallationGetter returns a programmed error, exercising the real-DB-
// error branch without needing to induce a live fault.
type fakeInstallationGetter struct{ err error }

func (f fakeInstallationGetter) GetInstallationByRef(context.Context, accountdb.GetInstallationByRefParams) (accountdb.Installation, error) {
	return accountdb.Installation{}, f.err
}

func TestEndpointResolver_DBError_FailsClosed(t *testing.T) {
	sentinel := errors.New("connection refused")
	r := NewEndpointResolver(fakeInstallationGetter{err: sentinel})

	_, _, err := r.ResolveInstallationEndpoints(context.Background(), "github", "1001")
	if err == nil {
		t.Fatal("a real DB error must be surfaced, got nil (fail-closed contract, binding condition 1)")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("error = %v, want it to wrap the DB error", err)
	}
}

func TestEndpointResolver_NilGetter(t *testing.T) {
	// A nil getter (no database) reports the deployment default without a query.
	r := NewEndpointResolver(nil)
	gotForge, gotOAuth, err := r.ResolveInstallationEndpoints(context.Background(), "github", "1001")
	if err != nil || gotForge != "" || gotOAuth != "" {
		t.Errorf("nil getter → (%q, %q, %v), want empty defaults with no error", gotForge, gotOAuth, err)
	}
}
