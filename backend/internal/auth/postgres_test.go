package auth_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/auth"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/postgres"
)

func newRepo(t *testing.T) auth.Repository {
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
	return auth.NewPostgresRepository(pool)
}

func ptrStr(s string) *string { return &s }

var sampleProfile = auth.GitHubProfile{
	ID:    42,
	Login: "octocat",
	Name:  "The Octo Cat",
	Email: ptrStr("octo@example.com"),
}

func TestPostgres_SignIn_CreatesUserAndSession(t *testing.T) {
	r := newRepo(t)
	user, sess, err := r.SignIn(context.Background(), sampleProfile)
	if err != nil {
		t.Fatalf("SignIn: %v", err)
	}
	if user.GitHubLogin != "octocat" || user.GitHubUserID != 42 {
		t.Errorf("user = %+v", user)
	}
	if sess.PlainText == "" {
		t.Error("session PlainText empty")
	}
	if !strings.HasPrefix(sess.PlainText, auth.SessionTokenPrefix) {
		t.Errorf("plaintext missing prefix: %q", sess.PlainText)
	}
	if sess.AbsoluteExpiresAt.Before(time.Now().Add(6 * 24 * time.Hour)) {
		t.Errorf("AbsoluteExpiresAt too soon: %v", sess.AbsoluteExpiresAt)
	}
}

func TestPostgres_SignIn_UpsertsOnSecondSignIn(t *testing.T) {
	r := newRepo(t)
	first, _, err := r.SignIn(context.Background(), sampleProfile)
	if err != nil {
		t.Fatal(err)
	}

	// Second sign-in: same GitHub id, renamed login.
	renamed := sampleProfile
	renamed.Login = "octocat-renamed"
	second, _, err := r.SignIn(context.Background(), renamed)
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID {
		t.Errorf("user ID changed across sign-ins: %s vs %s", first.ID, second.ID)
	}
	if second.GitHubLogin != "octocat-renamed" {
		t.Errorf("login not updated: %q", second.GitHubLogin)
	}
}

func TestPostgres_Authenticate_HappyPath(t *testing.T) {
	r := newRepo(t)
	_, sess, _ := r.SignIn(context.Background(), sampleProfile)

	user, gotSess, err := r.Authenticate(context.Background(), sess.PlainText)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if user.GitHubLogin != "octocat" {
		t.Errorf("user = %+v", user)
	}
	if gotSess.ID != sess.ID {
		t.Errorf("session id mismatch")
	}
	if gotSess.PlainText != "" {
		t.Errorf("PlainText leaked from Authenticate: %q", gotSess.PlainText)
	}
}

func TestPostgres_Authenticate_RevokedSessionRejected(t *testing.T) {
	r := newRepo(t)
	_, sess, _ := r.SignIn(context.Background(), sampleProfile)
	sid, _ := uuid.Parse(sess.ID)
	if err := r.Revoke(context.Background(), sid); err != nil {
		t.Fatal(err)
	}
	_, _, err := r.Authenticate(context.Background(), sess.PlainText)
	if !errors.Is(err, auth.ErrSessionNotFound) {
		t.Errorf("err = %v, want ErrSessionNotFound", err)
	}
}

func TestPostgres_Authenticate_Malformed(t *testing.T) {
	r := newRepo(t)
	_, _, err := r.Authenticate(context.Background(), "not-a-token")
	if !errors.Is(err, auth.ErrMalformedToken) {
		t.Errorf("err = %v, want ErrMalformedToken", err)
	}
}

func TestPostgres_Authenticate_NotFound(t *testing.T) {
	r := newRepo(t)
	_, _, err := r.Authenticate(context.Background(),
		auth.SessionTokenPrefix+"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
	if !errors.Is(err, auth.ErrSessionNotFound) {
		t.Errorf("err = %v, want ErrSessionNotFound", err)
	}
}

func TestPostgres_GetUser_HappyPath(t *testing.T) {
	r := newRepo(t)
	user, _, _ := r.SignIn(context.Background(), sampleProfile)
	uid, _ := uuid.Parse(user.ID)
	got, err := r.GetUser(context.Background(), uid)
	if err != nil {
		t.Fatalf("GetUser: %v", err)
	}
	if got.GitHubLogin != "octocat" {
		t.Errorf("login = %q", got.GitHubLogin)
	}
}

func TestPostgres_Revoke_Idempotent(t *testing.T) {
	r := newRepo(t)
	_, sess, _ := r.SignIn(context.Background(), sampleProfile)
	sid, _ := uuid.Parse(sess.ID)
	if err := r.Revoke(context.Background(), sid); err != nil {
		t.Fatal(err)
	}
	if err := r.Revoke(context.Background(), sid); err != nil {
		t.Errorf("second Revoke errored: %v", err)
	}
}

func TestPostgres_EvictExpired(t *testing.T) {
	r := newRepo(t)
	_, _, _ = r.SignIn(context.Background(), sampleProfile)

	// Cutoff well past the absolute TTL (7 days); should evict
	// the row regardless of when SignIn ran.
	n, err := r.EvictExpired(context.Background(), time.Now().Add(30*24*time.Hour).Unix())
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("EvictExpired returned %d, want 1", n)
	}
}

func TestPostgres_EvictExpired_RespectsCutoff(t *testing.T) {
	r := newRepo(t)
	_, _, _ = r.SignIn(context.Background(), sampleProfile)

	// Cutoff before the session's absolute_expires_at (~7d from
	// now). Nothing should be evicted.
	n, err := r.EvictExpired(context.Background(), time.Now().Add(-time.Hour).Unix())
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("EvictExpired returned %d, want 0", n)
	}
}

func TestPostgres_SignIn_RejectsEmptyProfile(t *testing.T) {
	r := newRepo(t)
	_, _, err := r.SignIn(context.Background(), auth.GitHubProfile{ID: 0, Login: ""})
	if err == nil {
		t.Error("expected error on empty profile")
	}
}
