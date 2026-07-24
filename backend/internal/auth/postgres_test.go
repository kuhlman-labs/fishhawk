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

func newRepoWithPool(t *testing.T) (auth.Repository, *pgxpool.Pool) {
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
	return auth.NewPostgresRepository(pool), pool
}

func newRepo(t *testing.T) auth.Repository {
	t.Helper()
	r, _ := newRepoWithPool(t)
	return r
}

// seedAccount inserts an accounts row and returns its id, for tests
// asserting the session->account binding.
func seedAccount(t *testing.T, pool *pgxpool.Pool, provider, key string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO accounts (id, provider, account_key) VALUES ($1, $2, $3)`,
		id, provider, key,
	); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	return id
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
	user, sess, err := r.SignIn(context.Background(), "github", sampleProfile, uuid.Nil)
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
	first, _, err := r.SignIn(context.Background(), "github", sampleProfile, uuid.Nil)
	if err != nil {
		t.Fatal(err)
	}

	// Second sign-in: same GitHub id, renamed login.
	renamed := sampleProfile
	renamed.Login = "octocat-renamed"
	second, _, err := r.SignIn(context.Background(), "github", renamed, uuid.Nil)
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
	_, sess, _ := r.SignIn(context.Background(), "github", sampleProfile, uuid.Nil)

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
	_, sess, _ := r.SignIn(context.Background(), "github", sampleProfile, uuid.Nil)
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
	user, _, _ := r.SignIn(context.Background(), "github", sampleProfile, uuid.Nil)
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
	_, sess, _ := r.SignIn(context.Background(), "github", sampleProfile, uuid.Nil)
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
	_, _, _ = r.SignIn(context.Background(), "github", sampleProfile, uuid.Nil)

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
	_, _, _ = r.SignIn(context.Background(), "github", sampleProfile, uuid.Nil)

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

// TestPostgres_SignIn_BindsAccountID pins the E44.3 session->account
// binding: SignIn persists sessions.account_id and both the returned
// and the re-authenticated Session carry it.
func TestPostgres_SignIn_BindsAccountID(t *testing.T) {
	r, pool := newRepoWithPool(t)
	accountID := seedAccount(t, pool, "github", "acme-corp")

	_, sess, err := r.SignIn(context.Background(), "github", sampleProfile, accountID)
	if err != nil {
		t.Fatalf("SignIn: %v", err)
	}
	if sess.AccountID != accountID.String() {
		t.Errorf("returned Session.AccountID = %q, want %s", sess.AccountID, accountID)
	}

	var persisted *uuid.UUID
	if err := pool.QueryRow(context.Background(),
		`SELECT account_id FROM sessions WHERE id = $1`, uuid.MustParse(sess.ID),
	).Scan(&persisted); err != nil {
		t.Fatalf("read sessions.account_id: %v", err)
	}
	if persisted == nil || *persisted != accountID {
		t.Errorf("sessions.account_id = %v, want %s", persisted, accountID)
	}

	_, got, err := r.Authenticate(context.Background(), sess.PlainText)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if got.AccountID != accountID.String() {
		t.Errorf("Authenticate Session.AccountID = %q, want %s", got.AccountID, accountID)
	}
}

// TestPostgres_SignIn_NilAccountIDStaysNull pins the uuid.Nil -> NULL
// mapping: an unbound sign-in leaves account_id NULL and the Session
// carries an empty AccountID (which /v0/auth/me then refuses).
func TestPostgres_SignIn_NilAccountIDStaysNull(t *testing.T) {
	r, pool := newRepoWithPool(t)
	_, sess, err := r.SignIn(context.Background(), "github", sampleProfile, uuid.Nil)
	if err != nil {
		t.Fatalf("SignIn: %v", err)
	}
	if sess.AccountID != "" {
		t.Errorf("Session.AccountID = %q, want empty", sess.AccountID)
	}
	var persisted *uuid.UUID
	if err := pool.QueryRow(context.Background(),
		`SELECT account_id FROM sessions WHERE id = $1`, uuid.MustParse(sess.ID),
	).Scan(&persisted); err != nil {
		t.Fatalf("read sessions.account_id: %v", err)
	}
	if persisted != nil {
		t.Errorf("sessions.account_id = %v, want NULL", persisted)
	}
}

func TestPostgres_SignIn_RejectsEmptyProfile(t *testing.T) {
	r := newRepo(t)
	_, _, err := r.SignIn(context.Background(), "github", auth.GitHubProfile{ID: 0, Login: ""}, uuid.Nil)
	if err == nil {
		t.Error("expected error on empty profile")
	}
}

// TestPostgres_SignIn_ProviderRoundTrips pins that the provider passed to
// SignIn is persisted and read back on the User (E44.22 / #2109).
func TestPostgres_SignIn_ProviderRoundTrips(t *testing.T) {
	r := newRepo(t)
	glProfile := auth.GitHubProfile{ID: 7, Login: "octo", Name: "Octo", Email: ptrStr("o@ex.com")}
	user, _, err := r.SignIn(context.Background(), "gitlab", glProfile, uuid.Nil)
	if err != nil {
		t.Fatalf("SignIn(gitlab): %v", err)
	}
	if user.Provider != "gitlab" {
		t.Errorf("User.Provider = %q, want gitlab", user.Provider)
	}
	// A GitHub sign-in defaults the discriminator to github.
	ghUser, _, err := r.SignIn(context.Background(), "github", sampleProfile, uuid.Nil)
	if err != nil {
		t.Fatalf("SignIn(github): %v", err)
	}
	if ghUser.Provider != "github" {
		t.Errorf("User.Provider = %q, want github", ghUser.Provider)
	}

	// Defensive default: an empty provider falls back to github rather than
	// writing an empty discriminator (which the CHECK would reject).
	emptyProv := auth.GitHubProfile{ID: 8, Login: "empty-prov", Name: "E"}
	defUser, _, err := r.SignIn(context.Background(), "", emptyProv, uuid.Nil)
	if err != nil {
		t.Fatalf("SignIn(empty provider): %v", err)
	}
	if defUser.Provider != "github" {
		t.Errorf("empty-provider User.Provider = %q, want github (defaulted)", defUser.Provider)
	}
}

// TestPostgres_SignIn_NoCollisionAcrossProviders is the identity-no-collision
// done-means (E44.22 / #2109): a GitLab profile whose numeric id equals a
// pre-existing GitHub user's id upserts a DISTINCT users row under the new
// UNIQUE (provider, github_user_id) — a GitLab id can never overwrite a GitHub
// user of the same numeric id.
func TestPostgres_SignIn_NoCollisionAcrossProviders(t *testing.T) {
	r := newRepo(t)
	ctx := context.Background()

	ghUser, _, err := r.SignIn(ctx, "github",
		auth.GitHubProfile{ID: 100, Login: "gh-user", Name: "GH", Email: ptrStr("gh@ex.com")}, uuid.Nil)
	if err != nil {
		t.Fatalf("SignIn(github): %v", err)
	}
	glUser, _, err := r.SignIn(ctx, "gitlab",
		auth.GitHubProfile{ID: 100, Login: "gl-user", Name: "GL", Email: ptrStr("gl@ex.com")}, uuid.Nil)
	if err != nil {
		t.Fatalf("SignIn(gitlab) with colliding numeric id: %v", err)
	}
	if ghUser.ID == glUser.ID {
		t.Fatalf("GitLab sign-in reused the GitHub user's row (id %s); the composite UNIQUE must keep them distinct", ghUser.ID)
	}
	if glUser.GitHubLogin != "gl-user" {
		t.Errorf("GitLab user login = %q, want gl-user", glUser.GitHubLogin)
	}
	// The GitHub row is untouched — re-fetch it by its id.
	got, err := r.GetUser(ctx, uuid.MustParse(ghUser.ID))
	if err != nil {
		t.Fatalf("GetUser(github): %v", err)
	}
	if got.GitHubLogin != "gh-user" || got.Provider != "github" {
		t.Errorf("github row after gitlab sign-in = %+v, want login=gh-user provider=github (not overwritten)", got)
	}
}
