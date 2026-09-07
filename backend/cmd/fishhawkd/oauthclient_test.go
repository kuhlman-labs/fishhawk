package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
)

// dummyDBURL is a syntactically-valid postgres URL that is NEVER dialed: every
// test using it exercises a guard that fires BEFORE postgres.Connect, so a real
// connection is never attempted. Reused across the DB-free register cases.
const dummyDBURL = "postgres://x:y@nowhere/db"

// mustRegisterOAuthClient registers a client via the shipped verb and fails on a
// non-OK exit, so the DB-backed tests do not hand-roll an INSERT.
func mustRegisterOAuthClient(t *testing.T, url, clientID string, extra ...string) {
	t.Helper()
	args := append([]string{"register", "--db", url, "--client-id", clientID, "--redirect-uri", "http://127.0.0.1:8765/callback"}, extra...)
	var log bytes.Buffer
	if got := runOAuthClient(args, &log); got != exitOK {
		t.Fatalf("register %s exit = %d; log:\n%s", clientID, got, log.String())
	}
}

// TestRunOAuthClient_UnknownSubcommand pins that a bare/unknown oauth-client verb
// reaches its own usage banner, not a dial.
func TestRunOAuthClient_UnknownSubcommand(t *testing.T) {
	var out strings.Builder
	if got := runOAuthClient([]string{"frobnicate"}, &out); got != exitUsage {
		t.Fatalf("exit = %d, want %d", got, exitUsage)
	}
	if !strings.Contains(out.String(), "unknown subcommand") {
		t.Errorf("output missing usage error: %s", out.String())
	}
}

// TestRunOAuth_UnknownSubcommandPrintsUsageIncludingTokenRevoke pins the
// `oauth` group dispatcher: an unknown verb reaches the group's usage banner,
// which must list BOTH verb groups — the `client` trio and `token revoke`
// (E66.5 / #2393) — so an operator who mistypes discovers the revocation verb.
func TestRunOAuth_UnknownSubcommandPrintsUsageIncludingTokenRevoke(t *testing.T) {
	var out strings.Builder
	if got := runOAuth([]string{"frobnicate"}, &out); got != exitUsage {
		t.Fatalf("exit = %d, want %d", got, exitUsage)
	}
	usage := out.String()
	if !strings.Contains(usage, "unknown subcommand") {
		t.Errorf("output missing usage error: %s", usage)
	}
	for _, want := range []string{
		"fishhawkd oauth client register",
		"fishhawkd oauth client list",
		"fishhawkd oauth client remove",
		"fishhawkd oauth token revoke --subject <s> [--client-id <c>]",
	} {
		if !strings.Contains(usage, want) {
			t.Errorf("oauth usage does not list %q:\n%s", want, usage)
		}
	}
}

// TestRunOAuth_DispatchesTokenGroup pins that `oauth token` routes to the token
// verb group rather than the group-level unknown-subcommand banner.
func TestRunOAuth_DispatchesTokenGroup(t *testing.T) {
	var out strings.Builder
	if got := runOAuth([]string{"token", "frobnicate"}, &out); got != exitUsage {
		t.Fatalf("exit = %d, want %d", got, exitUsage)
	}
	if !strings.Contains(out.String(), "fishhawkd oauth token: unknown subcommand") {
		t.Errorf("`oauth token frobnicate` did not reach the token group's banner: %s", out.String())
	}
}

// TestRunOAuthClientRegister_MissingFlags covers the flag-presence guards that
// return exitUsage BEFORE any database dial (the dummy --db is never connected).
func TestRunOAuthClientRegister_MissingFlags(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		wantSub string
	}{
		{"missing db", []string{"register", "--client-id", "cli", "--redirect-uri", "http://127.0.0.1:8765/cb"}, "--db"},
		{"missing client-id", []string{"register", "--db", dummyDBURL, "--redirect-uri", "http://127.0.0.1:8765/cb"}, "--client-id required"},
		{"whitespace client-id", []string{"register", "--db", dummyDBURL, "--client-id", " cli ", "--redirect-uri", "http://127.0.0.1:8765/cb"}, "surrounding whitespace"},
		{"zero redirect-uri", []string{"register", "--db", dummyDBURL, "--client-id", "cli"}, "at least one --redirect-uri required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out strings.Builder
			if got := runOAuthClient(tc.args, &out); got != exitUsage {
				t.Fatalf("exit = %d, want %d:\n%s", got, exitUsage, out.String())
			}
			if !strings.Contains(out.String(), tc.wantSub) {
				t.Errorf("output missing %q: %s", tc.wantSub, out.String())
			}
		})
	}
}

// TestRunOAuthClientRegister_RejectsInvalidRedirectURIs drives one case per
// component rule the registered-side validator enforces, each URI SELF-PAIRED
// through MatchRedirectURI(u, u) so the byte-equality branch cannot refuse it for
// an unrelated reason — the RED must land on the component check. Every case
// asserts exitUsage AND that the log names the offending URI.
func TestRunOAuthClientRegister_RejectsInvalidRedirectURIs(t *testing.T) {
	cases := []struct {
		name string
		uri  string
	}{
		{"fragment", "http://127.0.0.1:8765/cb#frag"},
		{"bare trailing hash", "http://127.0.0.1:8765/cb#"},
		{"query", "http://127.0.0.1:8765/cb?x=1"},
		{"userinfo", "http://user@127.0.0.1:8765/cb"},
		{"scheme-less relative", "/callback"},
		{"empty host", "https:///callback"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out strings.Builder
			got := runOAuthClient([]string{"register", "--db", dummyDBURL, "--client-id", "cli", "--redirect-uri", tc.uri}, &out)
			if got != exitUsage {
				t.Fatalf("exit = %d, want %d:\n%s", got, exitUsage, out.String())
			}
			if !strings.Contains(out.String(), tc.uri) {
				t.Errorf("output does not name the offending URI %q: %s", tc.uri, out.String())
			}
		})
	}
}

// TestRunOAuthClientRegister_RejectsUnsupportedAuthMethod pins the none-only
// refusal.
func TestRunOAuthClientRegister_RejectsUnsupportedAuthMethod(t *testing.T) {
	var out strings.Builder
	got := runOAuthClient([]string{"register", "--db", dummyDBURL, "--client-id", "cli",
		"--redirect-uri", "http://127.0.0.1:8765/cb", "--token-endpoint-auth-method", "client_secret_basic"}, &out)
	if got != exitUsage {
		t.Fatalf("exit = %d, want %d:\n%s", got, exitUsage, out.String())
	}
	if !strings.Contains(out.String(), "client_secret_basic") || !strings.Contains(out.String(), "only 'none'") {
		t.Errorf("output missing the unsupported-auth-method refusal naming client_secret_basic: %s", out.String())
	}
}

// TestRunOAuthClientRegister_RejectsGrantTypesWithoutAuthorizationCode pins the
// authorization_code-membership check. --grant-type refresh_token alone REPLACES
// the default set (which would have carried authorization_code), so the check has
// something to reject.
func TestRunOAuthClientRegister_RejectsGrantTypesWithoutAuthorizationCode(t *testing.T) {
	var out strings.Builder
	got := runOAuthClient([]string{"register", "--db", dummyDBURL, "--client-id", "cli",
		"--redirect-uri", "http://127.0.0.1:8765/cb", "--grant-type", "refresh_token"}, &out)
	if got != exitUsage {
		t.Fatalf("exit = %d, want %d:\n%s", got, exitUsage, out.String())
	}
	if !strings.Contains(out.String(), "authorization_code") {
		t.Errorf("output does not name the required authorization_code grant: %s", out.String())
	}
}

// TestRunOAuthClientRegister_TenantPairMustMatch pins that --provider and
// --account-key must be supplied together or not at all.
func TestRunOAuthClientRegister_TenantPairMustMatch(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"provider without account-key", []string{"register", "--db", dummyDBURL, "--client-id", "cli", "--redirect-uri", "http://127.0.0.1:8765/cb", "--provider", "github"}},
		{"account-key without provider", []string{"register", "--db", dummyDBURL, "--client-id", "cli", "--redirect-uri", "http://127.0.0.1:8765/cb", "--account-key", "acme"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out strings.Builder
			if got := runOAuthClient(tc.args, &out); got != exitUsage {
				t.Fatalf("exit = %d, want %d:\n%s", got, exitUsage, out.String())
			}
			if !strings.Contains(out.String(), "--provider and --account-key") {
				t.Errorf("output missing the tenant-pair refusal: %s", out.String())
			}
		})
	}
}

// TestRunOAuthClientRemove_MissingClientID pins the remove flag guard.
func TestRunOAuthClientRemove_MissingClientID(t *testing.T) {
	var out strings.Builder
	if got := runOAuthClient([]string{"remove", "--db", dummyDBURL}, &out); got != exitUsage {
		t.Fatalf("exit = %d, want %d:\n%s", got, exitUsage, out.String())
	}
	if !strings.Contains(out.String(), "--client-id required") {
		t.Errorf("output missing --client-id guard: %s", out.String())
	}
}

// --- DB-backed cases -------------------------------------------------------

// TestRunOAuthClientRegister_WritesRowWithDefaults reads the persisted row back
// through raw SQL: token_endpoint_auth_method='none', grant_types carrying BOTH
// authorization_code AND refresh_token (the read-time default is
// authorization_code ONLY — a dropped register-time default silently breaks
// refresh), and response_types=['code'].
func TestRunOAuthClientRegister_WritesRowWithDefaults(t *testing.T) {
	url := pgtest.NewURL(t)
	const clientID = "https://client.example.com/oauth/client"
	mustRegisterOAuthClient(t, url, clientID)

	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	var authMethod string
	var grantTypes, responseTypes, redirectURIs []string
	if err := pool.QueryRow(context.Background(),
		`SELECT token_endpoint_auth_method, grant_types, response_types, redirect_uris FROM oauth_clients WHERE client_id = $1`,
		clientID).Scan(&authMethod, &grantTypes, &responseTypes, &redirectURIs); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if authMethod != "none" {
		t.Errorf("token_endpoint_auth_method = %q, want none", authMethod)
	}
	if !containsString(grantTypes, "authorization_code") || !containsString(grantTypes, "refresh_token") {
		t.Errorf("grant_types = %v, want both authorization_code and refresh_token", grantTypes)
	}
	if len(responseTypes) != 1 || responseTypes[0] != "code" {
		t.Errorf("response_types = %v, want [code]", responseTypes)
	}
	if len(redirectURIs) != 1 || redirectURIs[0] != "http://127.0.0.1:8765/callback" {
		t.Errorf("redirect_uris = %v, want the registered pair", redirectURIs)
	}
}

// TestRunOAuthClientRegister_ReportsCreatedThenRefreshed asserts the first
// register prints created and a second of the same client_id prints refreshed —
// read from the returned row's first_seen_at/updated_at.
func TestRunOAuthClientRegister_ReportsCreatedThenRefreshed(t *testing.T) {
	url := pgtest.NewURL(t)
	const clientID = "https://client.example.com/oauth/client"

	first := captureStdout(t, func() {
		var log bytes.Buffer
		if got := runOAuthClient([]string{"register", "--db", url, "--client-id", clientID, "--redirect-uri", "http://127.0.0.1:8765/callback"}, &log); got != exitOK {
			t.Fatalf("first register exit; log:\n%s", log.String())
		}
	})
	if !strings.Contains(first, "created") {
		t.Errorf("first register output = %q, want it to say created", first)
	}

	second := captureStdout(t, func() {
		var log bytes.Buffer
		if got := runOAuthClient([]string{"register", "--db", url, "--client-id", clientID, "--redirect-uri", "http://127.0.0.1:9999/callback"}, &log); got != exitOK {
			t.Fatalf("second register exit; log:\n%s", log.String())
		}
	})
	if !strings.Contains(second, "refreshed") {
		t.Errorf("second register output = %q, want it to say refreshed", second)
	}
}

// TestRunOAuthClientRegister_UnknownAccountFailsClosed pins the fail-closed
// tenant resolution: an --account-key naming no account exits failure and names
// the account-create remedy.
func TestRunOAuthClientRegister_UnknownAccountFailsClosed(t *testing.T) {
	url := pgtest.NewURL(t)
	var out strings.Builder
	got := runOAuthClient([]string{"register", "--db", url, "--client-id", "https://client.example.com/oauth/client",
		"--redirect-uri", "http://127.0.0.1:8765/callback", "--provider", "github", "--account-key", "ghost"}, &out)
	if got != exitFailure {
		t.Fatalf("exit = %d, want %d:\n%s", got, exitFailure, out.String())
	}
	if !strings.Contains(out.String(), "account create") {
		t.Errorf("output does not name the `account create` remedy: %s", out.String())
	}
}

// TestRunOAuthClientRegister_AccountIDIsStickyAcrossTenants pins UpsertClient's
// COALESCE stickiness at the CLI level: registering the same client_id first
// under account A then under account B leaves account_id on A. Reads the column
// back through raw SQL — error identity alone cannot express a committed-state
// claim.
func TestRunOAuthClientRegister_AccountIDIsStickyAcrossTenants(t *testing.T) {
	url := pgtest.NewURL(t)
	mustCreateAccount(t, url, "github", "tenant-a")
	mustCreateAccount(t, url, "github", "tenant-b")
	const clientID = "https://client.example.com/oauth/client"

	mustRegisterOAuthClient(t, url, clientID, "--provider", "github", "--account-key", "tenant-a")
	mustRegisterOAuthClient(t, url, clientID, "--provider", "github", "--account-key", "tenant-b")

	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	var accountAID, persisted string
	if err := pool.QueryRow(context.Background(),
		`SELECT id::text FROM accounts WHERE provider = 'github' AND account_key = 'tenant-a'`).Scan(&accountAID); err != nil {
		t.Fatalf("read account A id: %v", err)
	}
	if err := pool.QueryRow(context.Background(),
		`SELECT account_id::text FROM oauth_clients WHERE client_id = $1`, clientID).Scan(&persisted); err != nil {
		t.Fatalf("read persisted account_id: %v", err)
	}
	if persisted != accountAID {
		t.Errorf("persisted account_id = %q, want tenant A %q — a re-register under tenant B must not move the registration", persisted, accountAID)
	}
}

// TestRunOAuthClientList_RendersSourceAndColumns covers both the empty-table
// message and, after seeding, that the rendered table carries the client_id, the
// literal pre-registered SOURCE, the redirect URI, and BOTH timestamps
// (first-seen and last-updated).
func TestRunOAuthClientList_RendersSourceAndColumns(t *testing.T) {
	url := pgtest.NewURL(t)

	empty := captureStdout(t, func() {
		var log bytes.Buffer
		if got := runOAuthClient([]string{"list", "--db", url}, &log); got != exitOK {
			t.Fatalf("list on empty exit; log:\n%s", log.String())
		}
	})
	if !strings.Contains(empty, "no oauth client registrations") {
		t.Errorf("empty list output = %q, want the no-registrations line", empty)
	}

	const clientID = "https://client.example.com/oauth/client"
	// Register twice so updated_at moves past first_seen_at and the second
	// registration's 9999 redirect URI proves the refresh landed.
	mustRegisterOAuthClient(t, url, clientID)
	mustRegisterOAuthClient(t, url, clientID, "--redirect-uri", "http://127.0.0.1:9999/callback")

	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	// Two back-to-back registrations stamp first_seen_at and updated_at within the
	// same wall-clock second, and both columns render to SECOND precision — so the
	// two would format IDENTICALLY and a SINGLE rendered timestamp would satisfy
	// both Contains assertions below (the vacuity the review names). Age
	// first_seen_at an hour behind updated_at so the two are DISTINCT to the
	// second, then require distinctness explicitly: each Contains assertion then
	// discriminates its OWN column.
	if _, err := pool.Exec(context.Background(),
		`UPDATE oauth_clients SET first_seen_at = updated_at - interval '1 hour' WHERE client_id = $1`, clientID); err != nil {
		t.Fatalf("age first_seen_at: %v", err)
	}
	var firstSeen, updated string
	if err := pool.QueryRow(context.Background(),
		`SELECT to_char(first_seen_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
		        to_char(updated_at    AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')
		   FROM oauth_clients WHERE client_id = $1`, clientID).Scan(&firstSeen, &updated); err != nil {
		t.Fatalf("read timestamps: %v", err)
	}
	if firstSeen == updated {
		t.Fatalf("first_seen_at and updated_at rendered identically (%q); the two timestamp assertions would be vacuous", firstSeen)
	}

	listed := captureStdout(t, func() {
		var log bytes.Buffer
		if got := runOAuthClient([]string{"list", "--db", url}, &log); got != exitOK {
			t.Fatalf("list exit; log:\n%s", log.String())
		}
	})
	// The SOURCE column is asserted against the LITERAL "pre-registered", not the
	// production sourcePreRegistered constant: a wrong constant would move actual
	// and expected together and pass vacuously, so the expected value is pinned
	// independent of the code under test. firstSeen and updated are now distinct
	// (guarded above), so requiring BOTH discriminates each timestamp column.
	for _, want := range []string{clientID, "pre-registered", "http://127.0.0.1:9999/callback", firstSeen, updated} {
		if !strings.Contains(listed, want) {
			t.Errorf("list output missing %q:\n%s", want, listed)
		}
	}
}

// TestRunOAuthClientRemove_DeletesThenUnknownFails removes a seeded row (read
// back: zero rows) and asserts a second remove of the same id exits failure
// naming the id.
func TestRunOAuthClientRemove_DeletesThenUnknownFails(t *testing.T) {
	url := pgtest.NewURL(t)
	const clientID = "https://client.example.com/oauth/client"
	mustRegisterOAuthClient(t, url, clientID)

	var log bytes.Buffer
	if got := runOAuthClient([]string{"remove", "--db", url, "--client-id", clientID}, &log); got != exitOK {
		t.Fatalf("remove exit = %d; log:\n%s", got, log.String())
	}

	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()
	var count int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM oauth_clients WHERE client_id = $1`, clientID).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Errorf("row count after remove = %d, want 0", count)
	}

	var out strings.Builder
	if got := runOAuthClient([]string{"remove", "--db", url, "--client-id", clientID}, &out); got != exitFailure {
		t.Fatalf("second remove exit = %d, want %d:\n%s", got, exitFailure, out.String())
	}
	if !strings.Contains(out.String(), clientID) {
		t.Errorf("second remove output does not name the id %q: %s", clientID, out.String())
	}
}
