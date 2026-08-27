package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
)

// TestRunAccountCreate_UsageErrors covers each missing-required / validation
// branch with NO database: every case must exit exitUsage AND name the offending
// input in the log, so a command failing for an unrelated reason cannot green a
// case. The dummy --db URL is never connected to because the guards fire first.
func TestRunAccountCreate_UsageErrors(t *testing.T) {
	const dummyDB = "postgres://x:y@nowhere/db"
	cases := []struct {
		name    string
		args    []string
		wantSub string
	}{
		{"missing db", []string{"create", "--provider", "gitlab", "--account-key", "acme"}, "--db"},
		{"missing provider", []string{"create", "--db", dummyDB, "--account-key", "acme"}, "--provider required"},
		{"missing account-key", []string{"create", "--db", dummyDB, "--provider", "gitlab"}, "--account-key required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			got := runAccount(tc.args, &out)
			if got != exitUsage {
				t.Fatalf("exit = %d, want %d; log:\n%s", got, exitUsage, out.String())
			}
			if !strings.Contains(out.String(), tc.wantSub) {
				t.Errorf("log %q does not name %q", out.String(), tc.wantSub)
			}
		})
	}
}

// TestRunAccount_UnknownSubcommand pins the dispatch usage path.
func TestRunAccount_UnknownSubcommand(t *testing.T) {
	var out bytes.Buffer
	if got := runAccount([]string{"banana"}, &out); got != exitUsage {
		t.Fatalf("exit = %d, want %d", got, exitUsage)
	}
	if !strings.Contains(out.String(), "unknown subcommand") {
		t.Errorf("log missing usage error: %s", out.String())
	}
}

// TestRunAccountCreate_InvalidGranularityIsUsage asserts a validation failure
// reaching the domain layer (bad granularity, which passes the flag-presence
// guards) is mapped to exitUsage via errors.Is(ErrValidation), against a real
// database so the case is not short-circuited by a connect failure.
func TestRunAccountCreate_InvalidGranularityIsUsage(t *testing.T) {
	url := pgtest.NewURL(t)
	var out bytes.Buffer
	got := runAccount([]string{"create", "--db", url, "--provider", "gitlab", "--account-key", "acme", "--granularity", "org"}, &out)
	if got != exitUsage {
		t.Fatalf("exit = %d, want %d (validation → usage); log:\n%s", got, exitUsage, out.String())
	}
	if !strings.Contains(out.String(), `"org"`) {
		t.Errorf("log %q does not name the rejected granularity", out.String())
	}
}

// TestRunAccountCreate_WritesRowAndIsIdempotent asserts the committed state: one
// accounts row with the resolved (provider, account_key, granularity), and a
// re-run leaves exactly one row (ON CONFLICT idempotence). The granularity is
// read back from the persisted row — the shipped-behavior test for the
// per-provider default no compiler enforces.
func TestRunAccountCreate_WritesRowAndIsIdempotent(t *testing.T) {
	url := pgtest.NewURL(t)

	var out bytes.Buffer
	if got := runAccount([]string{"create", "--db", url, "--provider", "gitlab", "--account-key", "acme", "--display-name", "Acme"}, &out); got != exitOK {
		t.Fatalf("first create exit = %d, want %d; log:\n%s", got, exitOK, out.String())
	}

	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	var count int
	var granularity, displayName string
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*), max(granularity), max(display_name) FROM accounts WHERE provider = $1 AND account_key = $2`,
		"gitlab", "acme").Scan(&count, &granularity, &displayName); err != nil {
		t.Fatalf("read back account: %v", err)
	}
	if count != 1 {
		t.Fatalf("account row count = %d, want 1", count)
	}
	if granularity != "group" {
		t.Errorf("granularity = %q, want group (gitlab default)", granularity)
	}
	if displayName != "Acme" {
		t.Errorf("display_name = %q, want Acme", displayName)
	}

	// Idempotent re-run: still exactly one row.
	out.Reset()
	if got := runAccount([]string{"create", "--db", url, "--provider", "gitlab", "--account-key", "acme"}, &out); got != exitOK {
		t.Fatalf("second create exit = %d, want %d; log:\n%s", got, exitOK, out.String())
	}
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM accounts WHERE provider = $1 AND account_key = $2`, "gitlab", "acme").Scan(&count); err != nil {
		t.Fatalf("recount: %v", err)
	}
	if count != 1 {
		t.Errorf("after idempotent re-run, account row count = %d, want 1", count)
	}
}

// TestRunAccountList_FiltersAndRendersAccountKey asserts the list surface: it
// renders the joined columns and honors the --provider filter. Covers binding
// condition 1 (real integration coverage for the list verb).
func TestRunAccountList_FiltersAndRendersAccountKey(t *testing.T) {
	url := pgtest.NewURL(t)

	// Empty-result path on a fresh database.
	empty := captureStdout(t, func() {
		var log bytes.Buffer
		if got := runAccount([]string{"list", "--db", url}, &log); got != exitOK {
			t.Fatalf("list on empty db exit = %d; log:\n%s", got, log.String())
		}
	})
	if !strings.Contains(empty, "no accounts registered") {
		t.Errorf("empty list output %q missing the no-accounts line", empty)
	}

	seed := func(provider, key string) {
		var out bytes.Buffer
		if got := runAccount([]string{"create", "--db", url, "--provider", provider, "--account-key", key}, &out); got != exitOK {
			t.Fatalf("seed create %s/%s exit = %d; log:\n%s", provider, key, got, out.String())
		}
	}
	seed("gitlab", "acme")
	seed("github", "widgets")

	all := captureAccountList(t, []string{"list", "--db", url})
	if !strings.Contains(all, "acme") || !strings.Contains(all, "widgets") {
		t.Errorf("unfiltered list %q missing a seeded account_key", all)
	}
	filtered := captureAccountList(t, []string{"list", "--db", url, "--provider", "gitlab"})
	if !strings.Contains(filtered, "acme") {
		t.Errorf("gitlab-filtered list %q missing acme", filtered)
	}
	if strings.Contains(filtered, "widgets") {
		t.Errorf("gitlab-filtered list %q leaked the github account widgets", filtered)
	}
}

// captureAccountList runs `account list` capturing stdout (where the rendered
// table goes) and returns it.
func captureAccountList(t *testing.T, args []string) string {
	t.Helper()
	return captureStdout(t, func() {
		var log bytes.Buffer
		if got := runAccount(args, &log); got != exitOK {
			t.Fatalf("account list exit = %d; log:\n%s", got, log.String())
		}
	})
}
