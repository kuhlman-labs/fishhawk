package mcptoken_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/kuhlman-labs/fishhawk/backend/internal/mcptoken"
	"github.com/kuhlman-labs/fishhawk/backend/internal/postgres"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// startContainer mirrors the helpers in apitoken / signing tests.
// MCP tokens FK runs(id) so the repo can't function without a real
// run row; helpers below cover both.
func startContainer(t *testing.T) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	c, err := tcpostgres.Run(ctx,
		"postgres:16-alpine",
		tcpostgres.WithDatabase("fishhawk"),
		tcpostgres.WithUsername("fishhawk"),
		tcpostgres.WithPassword("fishhawk"),
		testcontainers.WithWaitStrategy(
			wait.ForAll(
				wait.ForLog("database system is ready to accept connections").
					WithOccurrence(2).
					WithStartupTimeout(60*time.Second),
				wait.ForListeningPort("5432/tcp"),
			),
		),
	)
	if err != nil {
		if isDockerUnavailable(err) {
			t.Skipf("Docker not available; skipping integration test: %v", err)
		}
		t.Fatalf("start postgres: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = c.Terminate(ctx)
	})
	url, err := c.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}
	return url
}

func isDockerUnavailable(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "Cannot connect to the Docker daemon") ||
		strings.Contains(msg, "docker not available") ||
		strings.Contains(msg, "no such file or directory") &&
			strings.Contains(msg, "docker.sock")
}

func newRepoWithRun(t *testing.T) (mcptoken.Repository, uuid.UUID) {
	t.Helper()
	url := startContainer(t)
	if err := postgres.MigrateUp(url); err != nil {
		t.Fatalf("MigrateUp: %v", err)
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	runRepo := run.NewPostgresRepository(pool)
	r, err := runRepo.CreateRun(context.Background(), run.CreateRunParams{
		Repo:          "kuhlman-labs/fishhawk",
		WorkflowID:    "feature_change",
		WorkflowSHA:   "deadbeef",
		TriggerSource: run.TriggerCLI,
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	return mcptoken.NewPostgresRepository(pool), r.ID
}

func TestPostgres_Issue_ReturnsPlaintextOnce(t *testing.T) {
	r, runID := newRepoWithRun(t)
	tok, err := r.Issue(context.Background(), mcptoken.IssueParams{RunID: runID})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if tok.PlainText == "" {
		t.Error("Issue returned empty PlainText")
	}
	if !strings.HasPrefix(tok.PlainText, mcptoken.TokenPrefix) {
		t.Errorf("PlainText = %q, want prefix %q", tok.PlainText, mcptoken.TokenPrefix)
	}
	if tok.ID == uuid.Nil {
		t.Error("ID is zero")
	}
	if tok.RunID != runID {
		t.Errorf("RunID = %s, want %s", tok.RunID, runID)
	}
	if tok.IsRevoked() {
		t.Error("freshly-issued token marked revoked")
	}
	if tok.ExpiresAt.IsZero() {
		t.Error("ExpiresAt zero on issued token")
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

func TestPostgres_Issue_RejectsNilRunID(t *testing.T) {
	r, _ := newRepoWithRun(t)
	_, err := r.Issue(context.Background(), mcptoken.IssueParams{})
	if err == nil {
		t.Fatal("Issue with nil RunID should error")
	}
}

func TestPostgres_Issue_DefaultTTLApplied(t *testing.T) {
	r, runID := newRepoWithRun(t)
	before := time.Now().UTC()
	tok, _ := r.Issue(context.Background(), mcptoken.IssueParams{RunID: runID})
	after := time.Now().UTC()

	// ExpiresAt should be ~DefaultTTL from now.
	lo := before.Add(mcptoken.DefaultTTL - time.Minute)
	hi := after.Add(mcptoken.DefaultTTL + time.Minute)
	if tok.ExpiresAt.Before(lo) || tok.ExpiresAt.After(hi) {
		t.Errorf("ExpiresAt %v outside expected range [%v, %v]",
			tok.ExpiresAt, lo, hi)
	}
}

func TestPostgres_Issue_CustomTTLApplied(t *testing.T) {
	r, runID := newRepoWithRun(t)
	before := time.Now().UTC()
	tok, _ := r.Issue(context.Background(), mcptoken.IssueParams{
		RunID: runID,
		TTL:   5 * time.Minute,
	})
	after := time.Now().UTC()
	lo := before.Add(4 * time.Minute)
	hi := after.Add(6 * time.Minute)
	if tok.ExpiresAt.Before(lo) || tok.ExpiresAt.After(hi) {
		t.Errorf("ExpiresAt %v outside expected 5-minute range", tok.ExpiresAt)
	}
}

func TestPostgres_Authenticate_HappyPath(t *testing.T) {
	r, runID := newRepoWithRun(t)
	tok, err := r.Issue(context.Background(), mcptoken.IssueParams{RunID: runID})
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.Authenticate(context.Background(), tok.PlainText)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if got.ID != tok.ID || got.RunID != tok.RunID {
		t.Errorf("Authenticate returned wrong row: %+v", got)
	}
	// last_used_at should now be set.
	got2, _ := r.GetByID(context.Background(), tok.ID)
	if got2.LastUsedAt == nil {
		t.Error("Authenticate did not stamp last_used_at")
	}
}

func TestPostgres_Authenticate_Malformed(t *testing.T) {
	r, _ := newRepoWithRun(t)
	_, err := r.Authenticate(context.Background(), "not-a-token")
	if !errors.Is(err, mcptoken.ErrMalformedToken) {
		t.Errorf("err = %v, want ErrMalformedToken", err)
	}
}

func TestPostgres_Authenticate_NotFound(t *testing.T) {
	r, _ := newRepoWithRun(t)
	// Well-formed prefix but never-issued.
	_, err := r.Authenticate(context.Background(),
		mcptoken.TokenPrefix+"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
	if !errors.Is(err, mcptoken.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestPostgres_Authenticate_RevokedRejected(t *testing.T) {
	r, runID := newRepoWithRun(t)
	tok, _ := r.Issue(context.Background(), mcptoken.IssueParams{RunID: runID})
	if _, err := r.Revoke(context.Background(), tok.ID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	_, err := r.Authenticate(context.Background(), tok.PlainText)
	if !errors.Is(err, mcptoken.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound on revoked token", err)
	}
}

func TestPostgres_Authenticate_ExpiredRejected(t *testing.T) {
	r, runID := newRepoWithRun(t)
	// Issue with a tiny TTL so it lapses immediately.
	tok, err := r.Issue(context.Background(), mcptoken.IssueParams{
		RunID: runID,
		TTL:   1 * time.Nanosecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Sleep past the TTL boundary.
	time.Sleep(10 * time.Millisecond)
	_, err = r.Authenticate(context.Background(), tok.PlainText)
	if !errors.Is(err, mcptoken.ErrExpired) {
		t.Errorf("err = %v, want ErrExpired", err)
	}
}

func TestPostgres_Revoke_Idempotent(t *testing.T) {
	r, runID := newRepoWithRun(t)
	tok, _ := r.Issue(context.Background(), mcptoken.IssueParams{RunID: runID})

	first, err := r.Revoke(context.Background(), tok.ID)
	if err != nil {
		t.Fatalf("first Revoke: %v", err)
	}
	if !first.IsRevoked() {
		t.Error("first Revoke didn't mark revoked")
	}
	firstAt := *first.RevokedAt

	second, err := r.Revoke(context.Background(), tok.ID)
	if err != nil {
		t.Fatalf("second Revoke: %v", err)
	}
	if !second.RevokedAt.Equal(firstAt) {
		t.Errorf("second Revoke changed RevokedAt: %v vs %v",
			*second.RevokedAt, firstAt)
	}
}

func TestPostgres_Revoke_NotFound(t *testing.T) {
	r, _ := newRepoWithRun(t)
	_, err := r.Revoke(context.Background(), uuid.New())
	if !errors.Is(err, mcptoken.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestPostgres_RevokeForRun_RevokesEveryActive(t *testing.T) {
	r, runID := newRepoWithRun(t)
	// Issue three tokens for the same run; revoke one manually
	// (so it stays revoked but the bulk call shouldn't count it).
	tok1, _ := r.Issue(context.Background(), mcptoken.IssueParams{RunID: runID})
	tok2, _ := r.Issue(context.Background(), mcptoken.IssueParams{RunID: runID})
	_, _ = r.Issue(context.Background(), mcptoken.IssueParams{RunID: runID})
	if _, err := r.Revoke(context.Background(), tok1.ID); err != nil {
		t.Fatal(err)
	}

	n, err := r.RevokeForRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("RevokeForRun count = %d, want 2 (the two unrevoked tokens)", n)
	}
	// Both remaining tokens should now reject on Authenticate.
	if _, err := r.Authenticate(context.Background(), tok2.PlainText); !errors.Is(err, mcptoken.ErrNotFound) {
		t.Errorf("tok2 still authenticates: %v", err)
	}
}

func TestPostgres_GetByID_NotFound(t *testing.T) {
	r, _ := newRepoWithRun(t)
	_, err := r.GetByID(context.Background(), uuid.New())
	if !errors.Is(err, mcptoken.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}
