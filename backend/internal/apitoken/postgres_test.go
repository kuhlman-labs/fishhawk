package apitoken_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/apitoken"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/postgres"
)

func newRepo(t *testing.T) apitoken.Repository {
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
	return apitoken.NewPostgresRepository(pool)
}

func TestPostgres_Issue_ReturnsPlaintextOnce(t *testing.T) {
	r := newRepo(t)
	tok, err := r.Issue(context.Background(), "github:42", []string{"runs:read"})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if tok.PlainText == "" {
		t.Error("Issue returned empty PlainText")
	}
	if !strings.HasPrefix(tok.PlainText, apitoken.TokenPrefix) {
		t.Errorf("PlainText = %q, want prefix %q", tok.PlainText, apitoken.TokenPrefix)
	}
	if tok.ID == uuid.Nil {
		t.Error("ID is zero")
	}
	if tok.Subject != "github:42" {
		t.Errorf("Subject = %q", tok.Subject)
	}
	if len(tok.Scopes) != 1 || tok.Scopes[0] != "runs:read" {
		t.Errorf("Scopes = %v", tok.Scopes)
	}
	if tok.IsRevoked() {
		t.Errorf("freshly-issued token is marked revoked")
	}

	// GetByID should NOT carry the plaintext.
	got, err := r.GetByID(context.Background(), tok.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.PlainText != "" {
		t.Errorf("PlainText leaked from repository on read: %q", got.PlainText)
	}
}

func TestPostgres_Authenticate_HappyPath(t *testing.T) {
	r := newRepo(t)
	tok, err := r.Issue(context.Background(), "github:42", []string{"runs:read"})
	if err != nil {
		t.Fatal(err)
	}

	got, err := r.Authenticate(context.Background(), tok.PlainText)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if got.ID != tok.ID || got.Subject != tok.Subject {
		t.Errorf("Authenticate returned wrong row: %+v", got)
	}
	// last_used_at should now be set.
	got2, _ := r.GetByID(context.Background(), tok.ID)
	if got2.LastUsedAt == nil {
		t.Errorf("Authenticate did not stamp last_used_at")
	}
}

func TestPostgres_Authenticate_Malformed(t *testing.T) {
	r := newRepo(t)
	_, err := r.Authenticate(context.Background(), "not-a-token")
	if !errors.Is(err, apitoken.ErrMalformedToken) {
		t.Errorf("err = %v, want ErrMalformedToken", err)
	}
}

func TestPostgres_Authenticate_NotFound(t *testing.T) {
	r := newRepo(t)
	// A well-formed prefix but never-issued token.
	_, err := r.Authenticate(context.Background(),
		"fhk_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
	if !errors.Is(err, apitoken.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestPostgres_Authenticate_RevokedTokenRejected(t *testing.T) {
	r := newRepo(t)
	tok, _ := r.Issue(context.Background(), "github:42", nil)
	if _, err := r.Revoke(context.Background(), tok.ID, "github:42"); err != nil {
		t.Fatal(err)
	}
	_, err := r.Authenticate(context.Background(), tok.PlainText)
	if !errors.Is(err, apitoken.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound (revoked token must not authenticate)", err)
	}
}

func TestPostgres_Revoke_Idempotent(t *testing.T) {
	r := newRepo(t)
	tok, _ := r.Issue(context.Background(), "github:42", nil)

	r1, err := r.Revoke(context.Background(), tok.ID, "github:42")
	if err != nil {
		t.Fatalf("first Revoke: %v", err)
	}
	if !r1.IsRevoked() {
		t.Errorf("first Revoke didn't set RevokedAt")
	}
	first := *r1.RevokedAt

	// Second revoke keeps the original timestamp (COALESCE).
	r2, err := r.Revoke(context.Background(), tok.ID, "github:42")
	if err != nil {
		t.Errorf("second Revoke: %v", err)
	}
	if r2.RevokedAt == nil || !r2.RevokedAt.Equal(first) {
		t.Errorf("second Revoke changed timestamp: %v vs %v", r2.RevokedAt, first)
	}
}

func TestPostgres_Revoke_OwnershipChecked(t *testing.T) {
	r := newRepo(t)
	tok, _ := r.Issue(context.Background(), "github:42", nil)
	_, err := r.Revoke(context.Background(), tok.ID, "github:99")
	if !errors.Is(err, apitoken.ErrForbidden) {
		t.Errorf("err = %v, want ErrForbidden", err)
	}
}

func TestPostgres_Revoke_NotFound(t *testing.T) {
	r := newRepo(t)
	_, err := r.Revoke(context.Background(), uuid.New(), "x")
	if !errors.Is(err, apitoken.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestPostgres_ListForSubject_FiltersRevoked(t *testing.T) {
	r := newRepo(t)
	const me = "github:42"
	const other = "github:99"

	a, _ := r.Issue(context.Background(), me, nil)
	b, _ := r.Issue(context.Background(), me, nil)
	_, _ = r.Issue(context.Background(), other, nil)

	// Revoke b; only a should remain in the list.
	if _, err := r.Revoke(context.Background(), b.ID, me); err != nil {
		t.Fatal(err)
	}
	got, err := r.ListForSubject(context.Background(), me)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != a.ID {
		t.Errorf("ListForSubject = %+v, want only token a", got)
	}
}

func TestPostgres_ListForSubject_NewestFirst(t *testing.T) {
	r := newRepo(t)
	a, _ := r.Issue(context.Background(), "github:42", nil)
	time.Sleep(10 * time.Millisecond) // ensure created_at differs
	b, _ := r.Issue(context.Background(), "github:42", nil)

	got, err := r.ListForSubject(context.Background(), "github:42")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].ID != b.ID || got[1].ID != a.ID {
		t.Errorf("order = [%s, %s], want newest-first [%s, %s]",
			got[0].ID, got[1].ID, b.ID, a.ID)
	}
}

func TestPostgres_Issue_RejectsEmptySubject(t *testing.T) {
	r := newRepo(t)
	_, err := r.Issue(context.Background(), "", nil)
	if err == nil || !strings.Contains(err.Error(), "subject") {
		t.Errorf("err = %v, want subject error", err)
	}
}
