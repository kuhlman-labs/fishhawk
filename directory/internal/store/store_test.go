package store_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/kuhlman-labs/fishhawk/directory/internal/routing"
	"github.com/kuhlman-labs/fishhawk/directory/internal/store"
	"github.com/kuhlman-labs/fishhawk/directory/pkg/handoff"
)

// ---- test harness ------------------------------------------------------
//
// The directory module cannot import backend/internal/pgtest (internal
// visibility), so it carries this minimal equivalent: it ATTACHES to the
// same shared, reused fishhawk-test-postgres container by name and hands
// each test its own ephemeral database. The attach-retry contract mirrors
// pgtest's — retry on the first-start name conflict and on a stale reuse
// reference (a daemon-evicted container) — so the two harnesses can race
// each other for the shared container without either failing.
//
// Unlike pgtest there is no TEMPLATE database: the directory schema is a
// single tiny migration, so each test just creates an empty database and
// migrates it, avoiding template contention entirely.

const (
	containerName = "fishhawk-test-postgres"
	baseDB        = "fishhawk"
	baseUser      = "fishhawk"
	basePass      = "fishhawk"

	attachAttempts   = 20
	attachRetryDelay = 500 * time.Millisecond
	startTimeout     = 180 * time.Second
)

var (
	sharedOnce sync.Once
	sharedBase string
	sharedErr  error
)

// newPool returns a connected pool against a freshly-migrated ephemeral
// database, dropped via t.Cleanup. Skips when Docker is unavailable.
func newPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if os.Getenv("FISHHAWK_SKIP_INTEGRATION") != "" {
		t.Skip("FISHHAWK_SKIP_INTEGRATION set; skipping integration test")
	}
	sharedOnce.Do(func() { sharedBase, sharedErr = startSharedContainer() })
	if sharedErr != nil {
		if isDockerUnavailable(sharedErr) {
			t.Skipf("Docker not available; skipping integration test: %v", sharedErr)
		}
		t.Fatalf("start shared postgres: %v", sharedErr)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	admin, err := pgx.Connect(ctx, sharedBase)
	if err != nil {
		t.Fatalf("connect base db: %v", err)
	}
	defer func() { _ = admin.Close(ctx) }()

	dbName := "fhdir_" + randomSuffix(t)
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{dbName}.Sanitize()); err != nil {
		t.Fatalf("create per-test db: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		c, err := pgx.Connect(ctx, sharedBase)
		if err != nil {
			return // best-effort drop
		}
		defer func() { _ = c.Close(ctx) }()
		_, _ = c.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{dbName}.Sanitize()+" WITH (FORCE)")
	})

	dbURL := replaceDBName(sharedBase, dbName)
	if err := store.MigrateUp(dbURL); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.Connect(context.Background(), dbURL)
	if err != nil {
		t.Fatalf("connect per-test pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func startSharedContainer() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), startTimeout)
	defer cancel()

	var container *tcpostgres.PostgresContainer
	var lastErr error
	for i := 0; i < attachAttempts; i++ {
		c, err := tcpostgres.Run(ctx,
			"postgres:16-alpine",
			tcpostgres.WithDatabase(baseDB),
			tcpostgres.WithUsername(baseUser),
			tcpostgres.WithPassword(basePass),
			tcpostgres.BasicWaitStrategies(),
			testcontainers.WithReuseByName(containerName),
		)
		if err == nil {
			container = c
			break
		}
		if !isContainerNameConflict(err) && !isStaleContainerRef(err) {
			return "", err
		}
		lastErr = err
		time.Sleep(attachRetryDelay)
	}
	if container == nil {
		return "", fmt.Errorf("attach to shared container after %d attempts: %w", attachAttempts, lastErr)
	}
	// Intentionally NOT terminated: the container is shared across package
	// processes (ryuk disabled) and reaped by scripts/test's lease-refcounted
	// EXIT trap.
	base, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		return "", fmt.Errorf("connection string: %w", err)
	}
	return base, nil
}

func randomSuffix(t *testing.T) string {
	t.Helper()
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("random: %v", err)
	}
	return hex.EncodeToString(b)
}

func replaceDBName(baseURL, dbName string) string {
	u, err := url.Parse(baseURL)
	if err != nil {
		return baseURL
	}
	u.Path = "/" + dbName
	return u.String()
}

func isContainerNameConflict(err error) bool {
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "container name") && strings.Contains(msg, "already in use") {
		return true
	}
	return strings.Contains(msg, "status code 409") && strings.Contains(msg, "conflict")
}

func isStaleContainerRef(err error) bool {
	return strings.Contains(strings.ToLower(err.Error()), "no such container")
}

func isDockerUnavailable(err error) bool {
	msg := strings.ToLower(err.Error())
	for _, marker := range []string{
		"cannot connect to the docker daemon",
		"docker: not found",
		"executable file not found",
		"dial unix /var/run/docker.sock",
		"is the docker daemon running",
	} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

// ---- store tests -------------------------------------------------------

func TestAssignRegionRecordsRow(t *testing.T) {
	s := store.New(newPool(t))
	ctx := context.Background()

	got, err := s.AssignRegion(ctx, "github", "kuhlman-labs", "eu")
	if err != nil {
		t.Fatalf("AssignRegion: %v", err)
	}
	if got.Provider != "github" || got.AccountKey != "kuhlman-labs" || got.HomeRegion != "eu" {
		t.Fatalf("assignment: %+v", got)
	}

	read, err := s.LookupRegion(ctx, "github", "kuhlman-labs")
	if err != nil {
		t.Fatalf("LookupRegion: %v", err)
	}
	if read.HomeRegion != "eu" {
		t.Fatalf("home_region: got %q want eu", read.HomeRegion)
	}
}

// First-write-wins: a second assignment naming a different region must
// NOT move the account.
func TestAssignRegionIsFirstWriteWins(t *testing.T) {
	s := store.New(newPool(t))
	ctx := context.Background()

	if _, err := s.AssignRegion(ctx, "github", "acme", "eu"); err != nil {
		t.Fatalf("first assign: %v", err)
	}
	got, err := s.AssignRegion(ctx, "github", "acme", "us")
	if err != nil {
		t.Fatalf("second assign: %v", err)
	}
	if got.HomeRegion != "eu" {
		t.Fatalf("region moved: got %q want eu", got.HomeRegion)
	}
	read, err := s.LookupRegion(ctx, "github", "acme")
	if err != nil {
		t.Fatalf("LookupRegion: %v", err)
	}
	if read.HomeRegion != "eu" {
		t.Fatalf("persisted region moved: got %q want eu", read.HomeRegion)
	}
}

func TestAssignRegionNormalizesAndRejectsEmptyInput(t *testing.T) {
	s := store.New(newPool(t))
	ctx := context.Background()

	got, err := s.AssignRegion(ctx, " GitHub ", " acme ", " EU ")
	if err != nil {
		t.Fatalf("AssignRegion: %v", err)
	}
	if got.Provider != "github" || got.AccountKey != "acme" || got.HomeRegion != "eu" {
		t.Fatalf("normalization: %+v", got)
	}

	for name, args := range map[string][3]string{
		"no provider":    {"", "acme", "eu"},
		"no account_key": {"github", "  ", "eu"},
		"no region":      {"github", "acme", ""},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := s.AssignRegion(ctx, args[0], args[1], args[2]); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestLookupRegionNotFound(t *testing.T) {
	s := store.New(newPool(t))
	_, err := s.LookupRegion(context.Background(), "github", "never-onboarded")
	if !errors.Is(err, routing.ErrNotFound) {
		t.Fatalf("got %v want routing.ErrNotFound", err)
	}
}

func TestInstallStateRoundTrip(t *testing.T) {
	s := store.New(newPool(t))
	ctx := context.Background()
	want := routing.InstallState{
		Nonce: "n-1", Provider: "github", AccountKey: "acme",
		HomeRegion: "eu", ExpiresAt: time.Now().Add(time.Minute).UTC().Truncate(time.Millisecond),
	}
	if err := s.PutInstallState(ctx, want); err != nil {
		t.Fatalf("PutInstallState: %v", err)
	}
	got, err := s.ConsumeInstallState(ctx, "n-1")
	if err != nil {
		t.Fatalf("ConsumeInstallState: %v", err)
	}
	if got.Provider != want.Provider || got.AccountKey != want.AccountKey || got.HomeRegion != want.HomeRegion {
		t.Fatalf("round trip: got %+v want %+v", got, want)
	}
	if !got.ExpiresAt.Equal(want.ExpiresAt) {
		t.Fatalf("expires_at: got %s want %s", got.ExpiresAt, want.ExpiresAt)
	}
}

func TestPutInstallStateRejectsEmptyNonce(t *testing.T) {
	s := store.New(newPool(t))
	if err := s.PutInstallState(context.Background(), routing.InstallState{Provider: "github"}); err == nil {
		t.Fatal("expected an error")
	}
}

func TestConsumeInstallStateIsSingleUse(t *testing.T) {
	s := store.New(newPool(t))
	ctx := context.Background()
	if err := s.PutInstallState(ctx, routing.InstallState{
		Nonce: "n-2", Provider: "github", AccountKey: "acme",
		HomeRegion: "eu", ExpiresAt: time.Now().Add(time.Minute),
	}); err != nil {
		t.Fatalf("PutInstallState: %v", err)
	}
	if _, err := s.ConsumeInstallState(ctx, "n-2"); err != nil {
		t.Fatalf("first consume: %v", err)
	}
	if _, err := s.ConsumeInstallState(ctx, "n-2"); !errors.Is(err, routing.ErrNotFound) {
		t.Fatalf("replay: got %v want routing.ErrNotFound", err)
	}
}

func TestConsumeInstallStateUnknownAndEmptyNonce(t *testing.T) {
	s := store.New(newPool(t))
	ctx := context.Background()
	for name, nonce := range map[string]string{"unknown": "never-minted", "empty": "  "} {
		t.Run(name, func(t *testing.T) {
			if _, err := s.ConsumeInstallState(ctx, nonce); !errors.Is(err, routing.ErrNotFound) {
				t.Fatalf("got %v want routing.ErrNotFound", err)
			}
		})
	}
}

func TestConsumeInstallStateExpired(t *testing.T) {
	s := store.New(newPool(t))
	ctx := context.Background()
	if err := s.PutInstallState(ctx, routing.InstallState{
		Nonce: "n-3", Provider: "github", AccountKey: "acme",
		HomeRegion: "eu", ExpiresAt: time.Now().Add(-time.Minute),
	}); err != nil {
		t.Fatalf("PutInstallState: %v", err)
	}
	if _, err := s.ConsumeInstallState(ctx, "n-3"); !errors.Is(err, routing.ErrExpired) {
		t.Fatalf("got %v want routing.ErrExpired", err)
	}
	// Expired rows are consumed too, so a stale nonce cannot be retried.
	if _, err := s.ConsumeInstallState(ctx, "n-3"); !errors.Is(err, routing.ErrNotFound) {
		t.Fatalf("retry after expiry: got %v want routing.ErrNotFound", err)
	}
}

func TestPruneExpiredInstallStates(t *testing.T) {
	s := store.New(newPool(t))
	ctx := context.Background()
	for nonce, exp := range map[string]time.Duration{"live": time.Minute, "dead-1": -time.Minute, "dead-2": -time.Hour} {
		if err := s.PutInstallState(ctx, routing.InstallState{
			Nonce: nonce, Provider: "github", AccountKey: "acme",
			HomeRegion: "eu", ExpiresAt: time.Now().Add(exp),
		}); err != nil {
			t.Fatalf("PutInstallState(%s): %v", nonce, err)
		}
	}
	n, err := s.PruneExpiredInstallStates(ctx)
	if err != nil {
		t.Fatalf("PruneExpiredInstallStates: %v", err)
	}
	if n != 2 {
		t.Fatalf("pruned: got %d want 2", n)
	}
	if _, err := s.ConsumeInstallState(ctx, "live"); err != nil {
		t.Fatalf("live nonce was pruned: %v", err)
	}
}

func TestMigrateDownRollsBack(t *testing.T) {
	// A dedicated database so the rollback does not disturb siblings.
	pool := newPool(t)
	cfg := pool.Config().ConnConfig
	dbURL := fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable",
		cfg.User, cfg.Password, cfg.Host, cfg.Port, cfg.Database)
	pool.Close()

	if err := store.MigrateDown(dbURL); err != nil {
		t.Fatalf("MigrateDown: %v", err)
	}
	// Re-applying must succeed, proving the down migration left a clean slate.
	if err := store.MigrateUp(dbURL); err != nil {
		t.Fatalf("MigrateUp after down: %v", err)
	}
}

// ---- integration: store → routing → config ----------------------------

// The cross-boundary test the plan calls for: a real Postgres-backed
// store, the real router, and the real config, asserting a 302 whose
// Location preserves the original path and query AND carries a handoff
// pin that verifies with the shared secret.
func TestDirectoryEndToEndAssignsRecordsAndRedirects(t *testing.T) {
	s := store.New(newPool(t))
	cfg, err := routing.LoadConfig(func(k string) string {
		return map[string]string{
			routing.EnvSupportedRegions: "us,eu",
			routing.EnvCellBaseURLs:     "us=https://us.app.fishhawk.test,eu=https://eu.app.fishhawk.test",
			routing.EnvHandoffSecret:    "integration-secret",
		}[k]
	})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	h := routing.New(s, cfg).Handler()

	// 1. Onboarding assigns + records + redirects.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		routing.PathOnboardingStart+"?provider=github&account_key=kuhlman-labs&region=eu&code=abc&state=oauth-state", nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("onboarding status: got %d body %q", rec.Code, rec.Body.String())
	}

	// The row is really in Postgres.
	got, err := s.LookupRegion(context.Background(), "github", "kuhlman-labs")
	if err != nil {
		t.Fatalf("LookupRegion after onboarding: %v", err)
	}
	if got.HomeRegion != "eu" {
		t.Fatalf("persisted home_region: got %q want eu", got.HomeRegion)
	}

	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse Location: %v", err)
	}
	if loc.Host != "eu.app.fishhawk.test" || loc.Path != routing.PathOnboardingStart {
		t.Fatalf("location: %s", loc)
	}
	if loc.Query().Get("code") != "abc" || loc.Query().Get("state") != "oauth-state" {
		t.Fatalf("original query not preserved: %s", loc.RawQuery)
	}
	pin, err := handoff.Verify(loc.Query(), "integration-secret", time.Now())
	if err != nil {
		t.Fatalf("Verify pin: %v", err)
	}
	if pin.HomeRegion != "eu" || pin.AccountKey != "kuhlman-labs" {
		t.Fatalf("pin: %+v", pin)
	}

	// 2. A later login routes the same account by the persisted region.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		routing.PathLogin+"?provider=github&account_key=kuhlman-labs", nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("login status: got %d body %q", rec.Code, rec.Body.String())
	}
	loc, err = url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse Location: %v", err)
	}
	if loc.Host != "eu.app.fishhawk.test" {
		t.Fatalf("login routed to %q want eu.app.fishhawk.test", loc.Host)
	}

	// 3. An unonboarded account fails closed against the real store.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		routing.PathLogin+"?provider=github&account_key=stranger", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unonboarded login: got %d want 404", rec.Code)
	}
}
