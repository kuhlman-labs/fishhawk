package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	accountdb "github.com/kuhlman-labs/fishhawk/backend/internal/account/db"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
)

// captureStdout runs fn with os.Stdout redirected to a pipe and returns
// everything written to it — the list/register verbs print their
// machine-consumable output to stdout (the token.go convention).
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.String()
	}()
	fn()
	_ = w.Close()
	os.Stdout = orig
	out := <-done
	_ = r.Close()
	return out
}

// TestRunInstallationRegister_MissingFlags covers the flag-presence guards,
// which return exitUsage BEFORE any database dial — so a dummy --db is never
// connected to. (Shape validation, which needs the pool, is covered DB-backed
// below.)
func TestRunInstallationRegister_MissingFlags(t *testing.T) {
	const dummyDB = "postgres://x:y@nowhere/db"
	cases := []struct {
		name    string
		args    []string
		wantSub string
	}{
		{"missing db", []string{"register", "--provider", "gitlab", "--account-key", "acme", "--installation-ref", "gitlab:4242"}, "--db"},
		{"missing provider", []string{"register", "--db", dummyDB, "--account-key", "acme", "--installation-ref", "gitlab:4242"}, "--provider required"},
		{"missing account-key", []string{"register", "--db", dummyDB, "--provider", "gitlab", "--installation-ref", "gitlab:4242"}, "--account-key required"},
		{"missing installation-ref", []string{"register", "--db", dummyDB, "--provider", "gitlab", "--account-key", "acme"}, "--installation-ref required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			got := runInstallation(tc.args, &out)
			if got != exitUsage {
				t.Fatalf("exit = %d, want %d; log:\n%s", got, exitUsage, out.String())
			}
			if !strings.Contains(out.String(), tc.wantSub) {
				t.Errorf("log %q does not name %q", out.String(), tc.wantSub)
			}
		})
	}
}

// TestRunInstallationRegister_ValidationErrors is DATABASE-BACKED on purpose
// (binding condition 2): the shape validators live inside RegisterInstallation,
// which runs AFTER the pool is opened, so a no-DB case would only ever reach a
// connect failure and the claimed "malformed ref is rejected" transition would
// be unobservable. Each case seeds a real account so the write COULD land if
// validation were bypassed, then asserts exitUsage AND that installations stays
// EMPTY — which is exactly what makes the malformed-gitlab-ref case a true
// counterfactual vehicle: delete the gitlab branch of ValidateInstallationRef
// and this case writes a row and exits OK instead.
func TestRunInstallationRegister_ValidationErrors(t *testing.T) {
	cases := []struct {
		name    string
		args    []string // appended after --db url
		wantSub string
	}{
		{"unknown provider", []string{"--provider", "bitbucket", "--account-key", "acme", "--installation-ref", "gitlab:4242"}, `"bitbucket"`},
		{"malformed gitlab ref", []string{"--provider", "gitlab", "--account-key", "acme", "--installation-ref", "gitlab:abc"}, `"gitlab:abc"`},
		{"github ref wrong prefix", []string{"--provider", "github", "--account-key", "acme", "--installation-ref", "github:42"}, `"github:42"`},
		{"non-https forge base url", []string{"--provider", "gitlab", "--account-key", "acme", "--installation-ref", "gitlab:4242", "--forge-base-url", "http://insecure.example"}, "https"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			url := pgtest.NewURL(t)
			// Seed BOTH providers' accounts so a bypassed validator would find
			// an owning account and actually write, rather than failing on an
			// unknown account for an unrelated reason.
			mustCreateAccount(t, url, "gitlab", "acme")
			mustCreateAccount(t, url, "github", "acme")

			var out bytes.Buffer
			got := runInstallation(append([]string{"register", "--db", url}, tc.args...), &out)
			if got != exitUsage {
				t.Fatalf("exit = %d, want %d (validation → usage); log:\n%s", got, exitUsage, out.String())
			}
			if !strings.Contains(out.String(), tc.wantSub) {
				t.Errorf("log %q does not name %q", out.String(), tc.wantSub)
			}

			pool, err := pgxpool.New(context.Background(), url)
			if err != nil {
				t.Fatalf("pool: %v", err)
			}
			defer pool.Close()
			var count int
			if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM installations`).Scan(&count); err != nil {
				t.Fatalf("count installations: %v", err)
			}
			if count != 0 {
				t.Errorf("installations row count = %d after a rejected register, want 0", count)
			}
		})
	}
}

// TestRunInstallation_UnknownSubcommand pins the dispatch usage path.
func TestRunInstallation_UnknownSubcommand(t *testing.T) {
	var out bytes.Buffer
	if got := runInstallation([]string{"banana"}, &out); got != exitUsage {
		t.Fatalf("exit = %d, want %d", got, exitUsage)
	}
	if !strings.Contains(out.String(), "unknown subcommand") {
		t.Errorf("log missing usage error: %s", out.String())
	}
}

// TestRunInstallationRegister_WritesRowAndIsIdempotent asserts the committed
// state: register writes exactly one installations row pointing at the named
// account, and a re-run leaves exactly one (ON CONFLICT idempotence).
func TestRunInstallationRegister_WritesRowAndIsIdempotent(t *testing.T) {
	url := pgtest.NewURL(t)
	mustCreateAccount(t, url, "gitlab", "acme")

	var out bytes.Buffer
	if got := runInstallation([]string{"register", "--db", url, "--provider", "gitlab", "--account-key", "acme", "--installation-ref", "gitlab:4242"}, &out); got != exitOK {
		t.Fatalf("register exit = %d, want %d; log:\n%s", got, exitOK, out.String())
	}

	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	var count int
	var accountKey string
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*), max(a.account_key)
		   FROM installations i JOIN accounts a ON a.id = i.account_id
		  WHERE i.provider = $1 AND i.installation_ref = $2`,
		"gitlab", "gitlab:4242").Scan(&count, &accountKey); err != nil {
		t.Fatalf("read back installation: %v", err)
	}
	if count != 1 {
		t.Fatalf("installation row count = %d, want 1", count)
	}
	if accountKey != "acme" {
		t.Errorf("installation owning account_key = %q, want acme", accountKey)
	}

	out.Reset()
	if got := runInstallation([]string{"register", "--db", url, "--provider", "gitlab", "--account-key", "acme", "--installation-ref", "gitlab:4242"}, &out); got != exitOK {
		t.Fatalf("second register exit = %d; log:\n%s", got, out.String())
	}
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM installations WHERE provider = $1 AND installation_ref = $2`,
		"gitlab", "gitlab:4242").Scan(&count); err != nil {
		t.Fatalf("recount: %v", err)
	}
	if count != 1 {
		t.Errorf("after idempotent re-run, installation row count = %d, want 1", count)
	}
}

// TestRunInstallationRegister_RefusesUnknownAccount is the database-backed
// counterfactual vehicle (binding condition 2 + 4): registering under an account
// that does NOT exist must exit non-OK, name the `fishhawkd account create`
// remedy, AND leave the installations table with ZERO rows — a control that
// fired and rolled back would be byte-indistinguishable from one that never
// fired, so the state read is what discriminates. The exit is exitFailure (not
// exitUsage): an unknown account is not a mis-typed flag.
func TestRunInstallationRegister_RefusesUnknownAccount(t *testing.T) {
	url := pgtest.NewURL(t)

	var out bytes.Buffer
	got := runInstallation([]string{"register", "--db", url, "--provider", "gitlab", "--account-key", "ghost", "--installation-ref", "gitlab:4242"}, &out)
	if got != exitFailure {
		t.Fatalf("exit = %d, want %d (unknown account → failure); log:\n%s", got, exitFailure, out.String())
	}
	if !strings.Contains(out.String(), "fishhawkd account create") {
		t.Errorf("log %q does not name the `fishhawkd account create` remedy", out.String())
	}

	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	var count int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM installations`).Scan(&count); err != nil {
		t.Fatalf("count installations: %v", err)
	}
	if count != 0 {
		t.Errorf("installations row count = %d after a refused register, want 0", count)
	}
}

// TestRunInstallationList_FiltersAndRendersAccountKey asserts the list verb
// renders the joined account_key and honors --provider (binding condition 1).
func TestRunInstallationList_FiltersAndRendersAccountKey(t *testing.T) {
	url := pgtest.NewURL(t)
	mustCreateAccount(t, url, "gitlab", "acme")
	mustCreateAccount(t, url, "github", "widgets")
	mustRegisterInstallation(t, url, "gitlab", "acme", "gitlab:4242")
	mustRegisterInstallation(t, url, "github", "widgets", "77")

	all := captureStdout(t, func() {
		var log bytes.Buffer
		if got := runInstallation([]string{"list", "--db", url}, &log); got != exitOK {
			t.Fatalf("list exit = %d; log:\n%s", got, log.String())
		}
	})
	if !strings.Contains(all, "gitlab:4242") || !strings.Contains(all, "acme") {
		t.Errorf("list %q missing the gitlab installation + its account_key", all)
	}
	if !strings.Contains(all, "77") || !strings.Contains(all, "widgets") {
		t.Errorf("list %q missing the github installation + its account_key", all)
	}

	filtered := captureStdout(t, func() {
		var log bytes.Buffer
		if got := runInstallation([]string{"list", "--db", url, "--provider", "gitlab"}, &log); got != exitOK {
			t.Fatalf("filtered list exit = %d; log:\n%s", got, log.String())
		}
	})
	if !strings.Contains(filtered, "gitlab:4242") {
		t.Errorf("gitlab-filtered list %q missing the gitlab installation", filtered)
	}
	if strings.Contains(filtered, "widgets") {
		t.Errorf("gitlab-filtered list %q leaked the github installation", filtered)
	}
}

// TestInstallationRegister_AdmitsGitLabProjectThroughTheGate is the primary,
// done-means assertion: it drives the real runAccountCreate + runInstallationRegister
// against a pgtest database and then asserts the SHIPPED production authorizer
// admits the registered project — crossing CLI → domain → sqlc → Postgres →
// webhook authorizer in one test, so a per-layer green over a broken seam cannot
// pass.
func TestInstallationRegister_AdmitsGitLabProjectThroughTheGate(t *testing.T) {
	url := pgtest.NewURL(t)

	var out bytes.Buffer
	if got := runAccount([]string{"create", "--db", url, "--provider", "gitlab", "--account-key", "acme"}, &out); got != exitOK {
		t.Fatalf("account create exit = %d; log:\n%s", got, out.String())
	}
	out.Reset()
	if got := runInstallation([]string{"register", "--db", url, "--provider", "gitlab", "--account-key", "acme", "--installation-ref", "gitlab:4242"}, &out); got != exitOK {
		t.Fatalf("installation register exit = %d; log:\n%s", got, out.String())
	}

	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	authorizer := gitLabProjectRegistry{q: accountdb.New(pool)}
	ctx := context.Background()

	// Registered id + matching namespace → admitted.
	if ok, err := authorizer.AuthorizedGitLabProject(ctx, "gitlab:4242", "acme/widgets"); err != nil || !ok {
		t.Errorf("AuthorizedGitLabProject(gitlab:4242, acme/widgets) = (%v, %v), want (true, nil)", ok, err)
	}
	// Registered id + WRONG namespace → refused (the namespace half of the bind).
	if ok, err := authorizer.AuthorizedGitLabProject(ctx, "gitlab:4242", "other/widgets"); err != nil || ok {
		t.Errorf("AuthorizedGitLabProject(gitlab:4242, other/widgets) = (%v, %v), want (false, nil)", ok, err)
	}
	// Unregistered id + registered namespace → refused (the ref half of the bind).
	if ok, err := authorizer.AuthorizedGitLabProject(ctx, "gitlab:9999", "acme/widgets"); err != nil || ok {
		t.Errorf("AuthorizedGitLabProject(gitlab:9999, acme/widgets) = (%v, %v), want (false, nil)", ok, err)
	}
}

func mustCreateAccount(t *testing.T, url, provider, key string) {
	t.Helper()
	var out bytes.Buffer
	if got := runAccount([]string{"create", "--db", url, "--provider", provider, "--account-key", key}, &out); got != exitOK {
		t.Fatalf("mustCreateAccount %s/%s exit = %d; log:\n%s", provider, key, got, out.String())
	}
}

func mustRegisterInstallation(t *testing.T, url, provider, key, ref string) {
	t.Helper()
	var out bytes.Buffer
	if got := runInstallation([]string{"register", "--db", url, "--provider", provider, "--account-key", key, "--installation-ref", ref}, &out); got != exitOK {
		t.Fatalf("mustRegisterInstallation %s/%s/%s exit = %d; log:\n%s", provider, key, ref, got, out.String())
	}
}
