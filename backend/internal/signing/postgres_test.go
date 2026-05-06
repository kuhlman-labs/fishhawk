package signing_test

import (
	"context"
	"crypto/ed25519"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/kuhlman-labs/fishhawk/backend/internal/postgres"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/signing"
)

func startContainer(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	c, err := tcpostgres.Run(ctx,
		"postgres:16-alpine",
		tcpostgres.WithDatabase("fishhawk"),
		tcpostgres.WithUsername("fishhawk"),
		tcpostgres.WithPassword("fishhawk"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second),
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
		t.Fatalf("conn string: %v", err)
	}
	if err := postgres.MigrateUp(url); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	pool, err := postgres.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func isDockerUnavailable(err error) bool {
	if err == nil {
		return false
	}
	if os.Getenv("FISHHAWK_SKIP_INTEGRATION") != "" {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, marker := range []string{
		"cannot connect to the docker daemon",
		"docker: not found",
		"executable file not found",
		"dial unix /var/run/docker.sock",
	} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

func makeRun(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	repo := run.NewPostgresRepository(pool)
	r, err := repo.CreateRun(context.Background(), run.CreateRunParams{
		Repo:          "kuhlman-labs/fishhawk",
		WorkflowID:    "feature_change",
		WorkflowSHA:   "deadbeef",
		TriggerSource: run.TriggerCLI,
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	return r.ID
}

// --- pure unit tests of the helpers (no DB) ---

func TestComputeMessage_DeterministicSha256(t *testing.T) {
	a := signing.ComputeMessage([]byte("hello"))
	b := signing.ComputeMessage([]byte("hello"))
	if string(a) != string(b) {
		t.Errorf("ComputeMessage not deterministic")
	}
	if len(a) != 32 {
		t.Errorf("ComputeMessage len = %d, want 32 (sha256 size)", len(a))
	}
	c := signing.ComputeMessage([]byte("hello!"))
	if string(c) == string(a) {
		t.Error("ComputeMessage should differ for different inputs")
	}
}

func TestSignAndVerifyWith_RoundTrip(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	msg := signing.ComputeMessage([]byte("trace bundle bytes"))
	sig := signing.Sign(priv, msg)
	if len(sig) != ed25519.SignatureSize {
		t.Errorf("signature len = %d, want %d", len(sig), ed25519.SignatureSize)
	}
	if err := signing.VerifyWith(pub, msg, sig); err != nil {
		t.Errorf("VerifyWith: %v", err)
	}
}

func TestVerifyWith_RejectsTampering(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	msg := signing.ComputeMessage([]byte("legit"))
	sig := signing.Sign(priv, msg)

	tampered := signing.ComputeMessage([]byte("tampered"))
	if err := signing.VerifyWith(pub, tampered, sig); !errors.Is(err, signing.ErrSignatureInvalid) {
		t.Errorf("err = %v, want ErrSignatureInvalid", err)
	}

	// Flip a bit in the signature.
	sigCopy := append([]byte(nil), sig...)
	sigCopy[0] ^= 0x01
	if err := signing.VerifyWith(pub, msg, sigCopy); !errors.Is(err, signing.ErrSignatureInvalid) {
		t.Errorf("err = %v, want ErrSignatureInvalid for flipped sig", err)
	}
}

// --- integration tests (testcontainers Postgres) ---

func TestPostgres_IssueAndGet(t *testing.T) {
	pool := startContainer(t)
	repo := signing.NewPostgresRepository(pool)
	runID := makeRun(t, pool)

	issued, err := repo.Issue(context.Background(), runID, signing.DefaultTTL)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if len(issued.PublicKey) != ed25519.PublicKeySize {
		t.Errorf("public key len = %d, want %d", len(issued.PublicKey), ed25519.PublicKeySize)
	}
	if len(issued.PrivateKey) != ed25519.PrivateKeySize {
		t.Errorf("private key len = %d, want %d", len(issued.PrivateKey), ed25519.PrivateKeySize)
	}
	if !issued.ExpiresAt.After(issued.IssuedAt) {
		t.Errorf("ExpiresAt %v not after IssuedAt %v", issued.ExpiresAt, issued.IssuedAt)
	}

	stored, err := repo.Get(context.Background(), runID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(stored.PublicKey) != string(issued.PublicKey) {
		t.Error("Get returned a different public key than Issue did")
	}
}

func TestPostgres_Issue_RejectsZeroTTL(t *testing.T) {
	pool := startContainer(t)
	repo := signing.NewPostgresRepository(pool)
	runID := makeRun(t, pool)

	if _, err := repo.Issue(context.Background(), runID, 0); err == nil {
		t.Fatal("Issue with TTL=0 should error")
	}
}

func TestPostgres_Issue_AllowsRotation(t *testing.T) {
	// Per migration 0012 each Issue call inserts a new row so a
	// later stage's runner process can sign with its own key. The
	// second key's public half must differ from the first, and Get
	// returns the latest.
	pool := startContainer(t)
	repo := signing.NewPostgresRepository(pool)
	runID := makeRun(t, pool)

	first, err := repo.Issue(context.Background(), runID, signing.DefaultTTL)
	if err != nil {
		t.Fatalf("first Issue: %v", err)
	}
	second, err := repo.Issue(context.Background(), runID, signing.DefaultTTL)
	if err != nil {
		t.Fatalf("second Issue: %v", err)
	}
	if string(first.PublicKey) == string(second.PublicKey) {
		t.Error("rotation should yield a fresh public key, got the same as the first")
	}
	got, err := repo.Get(context.Background(), runID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got.PublicKey) != string(second.PublicKey) {
		t.Error("Get should return the latest issued key")
	}
}

func TestPostgres_Issue_RunNotFound(t *testing.T) {
	pool := startContainer(t)
	repo := signing.NewPostgresRepository(pool)

	// Run row does not exist; FK violation should bubble up as a
	// non-nil error.
	_, err := repo.Issue(context.Background(), uuid.New(), signing.DefaultTTL)
	if err == nil {
		t.Fatal("Issue against missing run should error")
	}
}

func TestPostgres_Get_NotFound(t *testing.T) {
	pool := startContainer(t)
	repo := signing.NewPostgresRepository(pool)

	_, err := repo.Get(context.Background(), uuid.New())
	if !errors.Is(err, signing.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestPostgres_Verify_HappyPath(t *testing.T) {
	pool := startContainer(t)
	repo := signing.NewPostgresRepository(pool)
	runID := makeRun(t, pool)

	issued, err := repo.Issue(context.Background(), runID, signing.DefaultTTL)
	if err != nil {
		t.Fatal(err)
	}

	bundle := []byte(`{"trace":"bytes go here"}`)
	msg := signing.ComputeMessage(bundle)
	sig := signing.Sign(issued.PrivateKey, msg)

	if err := repo.Verify(context.Background(), runID, msg, sig); err != nil {
		t.Errorf("Verify: %v", err)
	}
}

func TestPostgres_Verify_TamperedMessage(t *testing.T) {
	pool := startContainer(t)
	repo := signing.NewPostgresRepository(pool)
	runID := makeRun(t, pool)
	issued, _ := repo.Issue(context.Background(), runID, signing.DefaultTTL)

	msg := signing.ComputeMessage([]byte("legit"))
	sig := signing.Sign(issued.PrivateKey, msg)
	tampered := signing.ComputeMessage([]byte("tampered"))

	err := repo.Verify(context.Background(), runID, tampered, sig)
	if !errors.Is(err, signing.ErrSignatureInvalid) {
		t.Errorf("err = %v, want ErrSignatureInvalid", err)
	}
}

func TestPostgres_Verify_TamperedSignature(t *testing.T) {
	pool := startContainer(t)
	repo := signing.NewPostgresRepository(pool)
	runID := makeRun(t, pool)
	issued, _ := repo.Issue(context.Background(), runID, signing.DefaultTTL)

	msg := signing.ComputeMessage([]byte("legit"))
	sig := signing.Sign(issued.PrivateKey, msg)
	tampered := append([]byte(nil), sig...)
	tampered[0] ^= 0x01

	err := repo.Verify(context.Background(), runID, msg, tampered)
	if !errors.Is(err, signing.ErrSignatureInvalid) {
		t.Errorf("err = %v, want ErrSignatureInvalid", err)
	}
}

func TestPostgres_Verify_WrongRunRejected(t *testing.T) {
	// A signature from run A's key shouldn't pass for run B even
	// though the bytes are identical and the cipher is the same.
	pool := startContainer(t)
	repo := signing.NewPostgresRepository(pool)

	runA := makeRun(t, pool)
	runB := makeRun(t, pool)
	issuedA, _ := repo.Issue(context.Background(), runA, signing.DefaultTTL)
	_, _ = repo.Issue(context.Background(), runB, signing.DefaultTTL)

	msg := signing.ComputeMessage([]byte("trace"))
	sigA := signing.Sign(issuedA.PrivateKey, msg)

	// Verifying runA's signature under runB's key fails (different
	// public key).
	err := repo.Verify(context.Background(), runB, msg, sigA)
	if !errors.Is(err, signing.ErrSignatureInvalid) {
		t.Errorf("cross-run verify: err = %v, want ErrSignatureInvalid", err)
	}
}

func TestPostgres_Verify_VerifyForUnknownRunReturnsNotFound(t *testing.T) {
	pool := startContainer(t)
	repo := signing.NewPostgresRepository(pool)

	err := repo.Verify(context.Background(), uuid.New(), []byte{1, 2, 3}, []byte{4, 5, 6})
	if !errors.Is(err, signing.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestPostgres_Verify_ExpiredKey(t *testing.T) {
	// Use the test-only constructor with a clock pinned to a fixed
	// instant. Issue the key at T0 with TTL 1 hour, then advance
	// the clock past expiry and verify.
	pool := startContainer(t)
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	repo := signing.NewPostgresRepositoryWithClock(pool, clock)
	runID := makeRun(t, pool)

	issued, err := repo.Issue(context.Background(), runID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	msg := signing.ComputeMessage([]byte("trace"))
	sig := signing.Sign(issued.PrivateKey, msg)

	// Advance the clock 2 hours past issuance.
	now = now.Add(2 * time.Hour)
	err = repo.Verify(context.Background(), runID, msg, sig)
	if !errors.Is(err, signing.ErrExpired) {
		t.Errorf("err = %v, want ErrExpired", err)
	}
}

func TestPostgres_TriggerBlocksUpdate(t *testing.T) {
	// Mirrors the audit_entries trigger test: signing_keys is also
	// append-only, enforced by triggers regardless of the API.
	pool := startContainer(t)
	repo := signing.NewPostgresRepository(pool)
	runID := makeRun(t, pool)
	if _, err := repo.Issue(context.Background(), runID, signing.DefaultTTL); err != nil {
		t.Fatal(err)
	}

	_, err := pool.Exec(context.Background(),
		`UPDATE signing_keys SET public_key = $1 WHERE run_id = $2`,
		make([]byte, 32), runID)
	if err == nil {
		t.Fatal("UPDATE on signing_keys should be blocked by the trigger")
	}
	if !strings.Contains(err.Error(), "append-only") {
		t.Errorf("trigger error = %v, want 'append-only' substring", err)
	}
}

func TestPostgres_TriggerBlocksDelete(t *testing.T) {
	pool := startContainer(t)
	repo := signing.NewPostgresRepository(pool)
	runID := makeRun(t, pool)
	if _, err := repo.Issue(context.Background(), runID, signing.DefaultTTL); err != nil {
		t.Fatal(err)
	}

	_, err := pool.Exec(context.Background(),
		`DELETE FROM signing_keys WHERE run_id = $1`, runID)
	if err == nil {
		t.Fatal("DELETE on signing_keys should be blocked by the trigger")
	}
	if !strings.Contains(err.Error(), "append-only") {
		t.Errorf("trigger error = %v, want 'append-only' substring", err)
	}
}
