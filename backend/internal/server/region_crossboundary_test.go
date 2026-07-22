package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/account"
	accountdb "github.com/kuhlman-labs/fishhawk/backend/internal/account/db"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/directory/pkg/handoff"
	"github.com/kuhlman-labs/fishhawk/directory/pkg/routing"
)

// This file is the cross-boundary integration test the plan requires: the
// directory's REAL router (directory/pkg/routing over a real directory
// database) emits a redirect, and the Location header it produced is replayed
// BYTE FOR BYTE against a real cell server backed by a real Postgres accounts
// row. Nothing here hand-builds a routed path — hand-building the target is
// exactly the defect ADR-062 A2.1 records, because it lets the redirect and
// the verifier drift apart while both halves' unit tests stay green.

const (
	crossAdminToken = "operator-credential"
	crossSecret     = "cross-boundary-shared-secret"
	crossRegion     = "us"
)

// newDirectoryDB creates a FRESH database on the shared test Postgres and
// applies the directory's own migrations to it.
//
// The directory gets its own database rather than sharing the cell's: both
// planes' migrations use golang-migrate's default schema_migrations table, so
// stacking them in one database would make each think the other's version had
// already been applied.
func newDirectoryDB(t *testing.T) string {
	t.Helper()
	base := pgtest.NewURL(t)

	ctx := context.Background()
	conn, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatalf("connect to seed database: %v", err)
	}
	name := "directory_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	_, err = conn.Exec(ctx, fmt.Sprintf("CREATE DATABASE %q", name))
	closeErr := conn.Close(ctx)
	if err != nil {
		t.Fatalf("create directory database: %v", err)
	}
	if closeErr != nil {
		t.Fatalf("close seed connection: %v", closeErr)
	}

	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse base url: %v", err)
	}
	u.Path = "/" + name
	dirURL := u.String()

	// Drop it again at the end. The shared test Postgres outlives a single
	// package process (pgtest reuses one container), so a per-test database
	// left behind accumulates across runs until the postmaster gives out —
	// which a `-count=20` loop reaches quickly. Registered BEFORE the caller's
	// pool.Close cleanup so LIFO ordering closes the pool first; a still-open
	// connection would make DROP DATABASE fail.
	t.Cleanup(func() {
		ctx := context.Background()
		c, err := pgx.Connect(ctx, base)
		if err != nil {
			return // The container is gone; nothing to clean up.
		}
		defer func() { _ = c.Close(ctx) }()
		_, _ = c.Exec(ctx, fmt.Sprintf("DROP DATABASE IF EXISTS %q WITH (FORCE)", name))
	})

	if err := routing.MigrateSchema(dirURL); err != nil {
		t.Fatalf("migrate directory schema: %v", err)
	}
	return dirURL
}

// newDirectoryServer stands up the REAL directory router over its own
// database, routing crossRegion at cellURL.
func newDirectoryServer(t *testing.T, cellURL string) *httptest.Server {
	t.Helper()
	dirURL := newDirectoryDB(t)

	// A deliberately tiny pool: this test drives a handful of sequential
	// requests, and the shared test Postgres is a bounded resource every
	// package process in the suite competes for. pgxpool's default (one
	// connection per CPU) would spend that budget for nothing.
	poolCfg, err := pgxpool.ParseConfig(dirURL)
	if err != nil {
		t.Fatalf("parse directory pool config: %v", err)
	}
	poolCfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(context.Background(), poolCfg)
	if err != nil {
		t.Fatalf("open directory pool: %v", err)
	}
	t.Cleanup(pool.Close)

	rt, err := routing.NewPostgres(routing.Config{
		Regions:       map[string]string{crossRegion: cellURL},
		RoutedPaths:   []string{RoutedOnboardingPath},
		HandoffSecret: crossSecret,
		HandoffTTL:    5 * time.Minute,
		AdminToken:    crossAdminToken,
	}, pool)
	if err != nil {
		t.Fatalf("build directory router: %v", err)
	}

	srv := httptest.NewServer(rt)
	t.Cleanup(srv.Close)
	return srv
}

// newCellServer stands up a real cell over a real Postgres accounts row.
func newCellServer(t *testing.T) (*httptest.Server, *pgxpool.Pool) {
	t.Helper()
	pool := pgtest.NewPool(t)
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO accounts (id, provider, account_key, granularity) VALUES ($1, 'github', 'acme', 'organization')`,
		uuid.New()); err != nil {
		t.Fatalf("seed cell account: %v", err)
	}

	s := New(Config{
		HandoffSecret: crossSecret,
		RegionPinner:  account.NewRegionPinner(accountdb.New(pool), crossRegion),
	})
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return srv, pool
}

// noRedirectClient never follows a redirect — the test needs the Location the
// directory emitted, not the response the cell would give if net/http
// followed it and dropped the original bytes.
func noRedirectClient() *http.Client {
	return &http.Client{
		Timeout:       10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func assignRegion(t *testing.T, dirURL string) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{
		"provider": "github", "account_key": "acme", "region": crossRegion,
	})
	req, err := http.NewRequest(http.MethodPost, dirURL+routing.AssignPath, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build assign request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+crossAdminToken)
	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatalf("assign: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("assign status = %d, want 200", resp.StatusCode)
	}
}

// routedLocation drives the routed GET through the real router and returns
// the Location header verbatim.
func routedLocation(t *testing.T, dirURL string) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet,
		dirURL+RoutedOnboardingPath+"?provider=github&account_key=acme&state=caller-state", nil)
	if err != nil {
		t.Fatalf("build routed request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+crossAdminToken)
	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatalf("routed GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("routed GET status = %d, want 302", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if loc == "" {
		t.Fatal("routed GET emitted no Location header")
	}
	return loc
}

// replay issues a GET for the EXACT url string given.
func replay(t *testing.T, target string) (int, string) {
	t.Helper()
	resp, err := noRedirectClient().Get(target)
	if err != nil {
		t.Fatalf("replay %s: %v", target, err)
	}
	defer func() { _ = resp.Body.Close() }()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		t.Fatalf("read replay body: %v", err)
	}
	return resp.StatusCode, buf.String()
}

func homeRegion(t *testing.T, pool *pgxpool.Pool) *string {
	t.Helper()
	var region *string
	if err := pool.QueryRow(context.Background(),
		`SELECT home_region FROM accounts WHERE provider = 'github' AND account_key = 'acme'`).Scan(&region); err != nil {
		t.Fatalf("read home_region: %v", err)
	}
	return region
}

// TestCrossBoundary_DirectoryRedirectPinsCell walks the whole seam:
// assign -> route -> 302 -> replay the emitted Location -> the cell's
// accounts row is stamped.
func TestCrossBoundary_DirectoryRedirectPinsCell(t *testing.T) {
	cell, pool := newCellServer(t)
	dir := newDirectoryServer(t, cell.URL)

	assignRegion(t, dir.URL)
	loc := routedLocation(t, dir.URL)

	// The redirect must land on the cell, at the SAME path, with the
	// caller's own query preserved and the handoff appended.
	target, err := url.Parse(loc)
	if err != nil {
		t.Fatalf("parse Location %q: %v", loc, err)
	}
	if !strings.HasPrefix(loc, cell.URL) {
		t.Fatalf("Location %q does not target the cell %q", loc, cell.URL)
	}
	if target.Path != RoutedOnboardingPath {
		t.Fatalf("Location path = %q, want the original %q", target.Path, RoutedOnboardingPath)
	}
	q := target.Query()
	if q.Get("state") != "caller-state" {
		t.Fatalf("caller state = %q, want it preserved across the redirect", q.Get("state"))
	}
	if q.Get(handoff.ParamSignature) == "" || q.Get(handoff.ParamRegion) != crossRegion {
		t.Fatalf("Location carries no usable handoff: %v", q)
	}

	if got := homeRegion(t, pool); got != nil {
		t.Fatalf("home_region = %v before the replay, want NULL", *got)
	}

	// Replay the Location EXACTLY as emitted.
	status, body := replay(t, loc)
	if status != http.StatusOK {
		t.Fatalf("replay status = %d, want 200:\n%s", status, body)
	}
	got := homeRegion(t, pool)
	if got == nil || *got != crossRegion {
		t.Fatalf("home_region = %v after the replay, want %q", got, crossRegion)
	}

	var resp onboardingStartResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decode cell response: %v\n%s", err, body)
	}
	if !resp.Pinned || resp.HomeRegion != crossRegion || resp.AccountKey != "acme" {
		t.Fatalf("cell response = %+v, want a pinned acme in %q", resp, crossRegion)
	}
}

// Approval condition 3: with no nonce store, the replay bound is the
// conditional UPDATE. Replaying the SAME Location verbatim is a harmless
// no-op — the region does not move and nothing errors — while a Location
// naming a DIFFERENT region is refused with the typed conflict.
func TestCrossBoundary_ReplayIsNoOpAndForeignRegionIsRefused(t *testing.T) {
	cell, pool := newCellServer(t)
	dir := newDirectoryServer(t, cell.URL)

	assignRegion(t, dir.URL)
	loc := routedLocation(t, dir.URL)

	if status, body := replay(t, loc); status != http.StatusOK {
		t.Fatalf("first replay status = %d, want 200:\n%s", status, body)
	}
	// The same Location again: a no-op, not a refusal and not a re-home.
	if status, body := replay(t, loc); status != http.StatusOK {
		t.Fatalf("second replay status = %d, want 200 (a verbatim replay is a no-op):\n%s", status, body)
	}
	if got := homeRegion(t, pool); got == nil || *got != crossRegion {
		t.Fatalf("home_region = %v after two replays, want a stable %q", got, crossRegion)
	}

	// (a) Tampering the region WITHOUT re-signing breaks the MAC.
	tampered := mutateRegion(t, loc, "eu", false)
	if status, body := replay(t, tampered); status != http.StatusForbidden {
		t.Fatalf("unsigned region tamper status = %d, want 403:\n%s", status, body)
	}

	// (b) Re-signing with the shared secret produces an AUTHENTIC handoff
	// naming another region — a misrouted caller, or a directory bug. The
	// cell's residency self-check refuses it with the typed conflict rather
	// than recording another region's account.
	resigned := mutateRegion(t, loc, "eu", true)
	status, body := replay(t, resigned)
	if status != http.StatusConflict {
		t.Fatalf("foreign-region replay status = %d, want 409:\n%s", status, body)
	}
	var env errorEnvelope
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("decode error envelope: %v\n%s", err, body)
	}
	if env.Error.Code != "region_mismatch" {
		t.Fatalf("error code = %q, want region_mismatch", env.Error.Code)
	}

	if got := homeRegion(t, pool); got == nil || *got != crossRegion {
		t.Fatalf("home_region = %v after the refusals, want it unchanged at %q", got, crossRegion)
	}
}

// mutateRegion rewrites the fh_region parameter of an emitted Location. With
// resign=false the signature is left stale (the tamper case); with
// resign=true the handoff is re-signed with the shared secret, which is what
// a genuinely misrouted-but-authentic handoff looks like.
func mutateRegion(t *testing.T, loc, region string, resign bool) string {
	t.Helper()
	u, err := url.Parse(loc)
	if err != nil {
		t.Fatalf("parse Location: %v", err)
	}
	q := u.Query()
	q.Set(handoff.ParamRegion, region)

	if resign {
		expires, err := time.Parse(time.RFC3339, q.Get(handoff.ParamExpiresAt))
		if err != nil {
			t.Fatalf("parse emitted expiry: %v", err)
		}
		signed, err := handoff.Sign(crossSecret, handoff.Params{
			Provider:   q.Get(handoff.ParamProvider),
			AccountKey: q.Get(handoff.ParamAccountKey),
			HomeRegion: region,
			ExpiresAt:  expires,
			Nonce:      q.Get(handoff.ParamNonce),
		})
		if err != nil {
			t.Fatalf("re-sign handoff: %v", err)
		}
		for k, vs := range signed {
			q[k] = vs
		}
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// The directory refuses BOTH surfaces without the operator credential
// (ADR-062 A2.5 / approval condition 4), so an unauthorized caller never even
// reaches a handoff.
func TestCrossBoundary_RoutedSurfaceRequiresOperatorCredential(t *testing.T) {
	cell, _ := newCellServer(t)
	dir := newDirectoryServer(t, cell.URL)
	assignRegion(t, dir.URL)

	resp, err := noRedirectClient().Get(dir.URL + RoutedOnboardingPath + "?provider=github&account_key=acme")
	if err != nil {
		t.Fatalf("unauthenticated routed GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated routed GET status = %d, want 401", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "" {
		t.Fatalf("refused request still emitted a Location: %q", loc)
	}
}
