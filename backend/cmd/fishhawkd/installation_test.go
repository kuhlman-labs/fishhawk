package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	accountdb "github.com/kuhlman-labs/fishhawk/backend/internal/account/db"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/webhook"
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
		{"missing db", []string{"register", "--provider", "gitlab", "--account-key", "acme", "--installation-ref", "gitlab:4242", "--project-path", "acme/widgets"}, "--db"},
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
		{"unknown provider", []string{"--provider", "bitbucket", "--account-key", "acme", "--installation-ref", "gitlab:4242", "--project-path", "acme/widgets"}, `"bitbucket"`},
		{"malformed gitlab ref", []string{"--provider", "gitlab", "--account-key", "acme", "--installation-ref", "gitlab:abc", "--project-path", "acme/widgets"}, `"gitlab:abc"`},
		{"non-positive gitlab id", []string{"--provider", "gitlab", "--account-key", "acme", "--installation-ref", "gitlab:0", "--project-path", "acme/widgets"}, `"gitlab:0"`},
		{"github ref wrong prefix", []string{"--provider", "github", "--account-key", "acme", "--installation-ref", "github:42"}, `"github:42"`},
		// E45.26 / #2877: --project-path is REQUIRED for a gitlab registration
		// and must live under the owning account_key. Both are DB-backed for
		// the same reason as the rows above — they run after the pool opens —
		// and both assert the installations table stays EMPTY, which is what
		// makes them true counterfactual vehicles.
		{"missing project path", []string{"--provider", "gitlab", "--account-key", "acme", "--installation-ref", "gitlab:4242"}, "--project-path"},
		{"namespaceless project path", []string{"--provider", "gitlab", "--account-key", "acme", "--installation-ref", "gitlab:4242", "--project-path", "widgets"}, "<namespace>/<project>"},
		{"namespace-inconsistent project path", []string{"--provider", "gitlab", "--account-key", "acme", "--installation-ref", "gitlab:4242", "--project-path", "other/widgets"}, `"acme"`},
		{"non-https forge base url", []string{"--provider", "gitlab", "--account-key", "acme", "--installation-ref", "gitlab:4242", "--project-path", "acme/widgets", "--forge-base-url", "http://insecure.example"}, "https"},
		{"non-https oauth base url", []string{"--provider", "gitlab", "--account-key", "acme", "--installation-ref", "gitlab:4242", "--project-path", "acme/widgets", "--oauth-base-url", "http://insecure.example"}, "https"},
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
	if got := runInstallation([]string{"register", "--db", url, "--provider", "gitlab", "--account-key", "acme", "--installation-ref", "gitlab:4242", "--project-path", "acme/widgets"}, &out); got != exitOK {
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
	if got := runInstallation([]string{"register", "--db", url, "--provider", "gitlab", "--account-key", "acme", "--installation-ref", "gitlab:4242", "--project-path", "acme/widgets"}, &out); got != exitOK {
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
	got := runInstallation([]string{"register", "--db", url, "--provider", "gitlab", "--account-key", "ghost", "--installation-ref", "gitlab:4242", "--project-path", "ghost/widgets"}, &out)
	if got != exitFailure {
		t.Fatalf("exit = %d, want %d (unknown account → failure); log:\n%s", got, exitFailure, out.String())
	}
	if !strings.Contains(out.String(), "fishhawkd account create") {
		t.Errorf("log %q does not name the `fishhawkd account create` remedy", out.String())
	}
	// The error must NAME the missing account key, not just a generic remedy —
	// the quoted `"ghost"` appears only in the account_key %q clause, so a
	// regression to a generic message would turn this red.
	if !strings.Contains(out.String(), `"ghost"`) {
		t.Errorf("log %q does not name the missing account_key \"ghost\"", out.String())
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
	// The github account key is deliberately NOT a substring of the gitlab
	// row's project_path ("acme/widgets"): the leak assertion below is a
	// substring match, and a key of "widgets" would make it pass on the
	// gitlab row's own path and stop discriminating.
	mustCreateAccount(t, url, "github", "octo-org")
	mustRegisterInstallation(t, url, "gitlab", "acme", "gitlab:4242", "acme/widgets")
	mustRegisterInstallation(t, url, "github", "octo-org", "77", "")

	all := captureStdout(t, func() {
		var log bytes.Buffer
		if got := runInstallation([]string{"list", "--db", url}, &log); got != exitOK {
			t.Fatalf("list exit = %d; log:\n%s", got, log.String())
		}
	})
	if !strings.Contains(all, "gitlab:4242") || !strings.Contains(all, "acme") {
		t.Errorf("list %q missing the gitlab installation + its account_key", all)
	}
	if !strings.Contains(all, "77") || !strings.Contains(all, "octo-org") {
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
	if strings.Contains(filtered, "octo-org") {
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
	if got := runInstallation([]string{"register", "--db", url, "--provider", "gitlab", "--account-key", "acme", "--installation-ref", "gitlab:4242", "--project-path", "acme/widgets"}, &out); got != exitOK {
		t.Fatalf("installation register exit = %d; log:\n%s", got, out.String())
	}

	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	authorizer := gitLabProjectRegistry{q: accountdb.New(pool)}
	ctx := context.Background()

	// Registered id + the EXACT registered path → admitted.
	if ok, err := authorizer.AuthorizedGitLabProject(ctx, "gitlab:4242", "acme/widgets"); err != nil || !ok {
		t.Errorf("AuthorizedGitLabProject(gitlab:4242, acme/widgets) = (%v, %v), want (true, nil)", ok, err)
	}
	// Registered id + a SIBLING project in the SAME namespace → refused. This
	// is the E45.26 / #2877 tightening: the pre-change namespace-only binding
	// ADMITTED this, leaving the workflow-spec read steerable within the tenant.
	// It travels the whole stack — CLI flag, domain validation, sqlc column,
	// real Postgres, shipped authorizer — so a per-layer green over a broken
	// seam (a transposed Scan order, say) cannot pass it.
	if ok, err := authorizer.AuthorizedGitLabProject(ctx, "gitlab:4242", "acme/other-project"); err != nil || ok {
		t.Errorf("AuthorizedGitLabProject(gitlab:4242, acme/other-project) = (%v, %v), want (false, nil)", ok, err)
	}
	// Registered id + WRONG namespace → refused (the tenancy half of the bind).
	if ok, err := authorizer.AuthorizedGitLabProject(ctx, "gitlab:4242", "other/widgets"); err != nil || ok {
		t.Errorf("AuthorizedGitLabProject(gitlab:4242, other/widgets) = (%v, %v), want (false, nil)", ok, err)
	}
	// A case difference is a refusal: GitLab canonicalises project path case,
	// so this names a different project. Documented in docs/deploy/gitlab.md.
	if ok, err := authorizer.AuthorizedGitLabProject(ctx, "gitlab:4242", "acme/Widgets"); err != nil || ok {
		t.Errorf("AuthorizedGitLabProject(gitlab:4242, acme/Widgets) = (%v, %v), want (false, nil)", ok, err)
	}
	// Unregistered id + registered path → refused (the ref half of the bind).
	if ok, err := authorizer.AuthorizedGitLabProject(ctx, "gitlab:9999", "acme/widgets"); err != nil || ok {
		t.Errorf("AuthorizedGitLabProject(gitlab:9999, acme/widgets) = (%v, %v), want (false, nil)", ok, err)
	}
}

// TestInstallationRegister_NestedGroupAdmitsThroughTheGate is binding condition
// 4 driven end to end: GitLab groups NEST, so a nested-group project must stay
// registerable AND admittable. A validator splitting on every "/" would reject
// the registration outright; an authorizer comparing only two segments would
// refuse the admit. Both halves are exercised against real Postgres.
func TestInstallationRegister_NestedGroupAdmitsThroughTheGate(t *testing.T) {
	url := pgtest.NewURL(t)
	mustCreateAccount(t, url, "gitlab", "acme")
	mustRegisterInstallation(t, url, "gitlab", "acme", "gitlab:4242", "acme/platform/widgets")

	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	authorizer := gitLabProjectRegistry{q: accountdb.New(pool)}
	ctx := context.Background()

	if ok, err := authorizer.AuthorizedGitLabProject(ctx, "gitlab:4242", "acme/platform/widgets"); err != nil || !ok {
		t.Errorf("AuthorizedGitLabProject(gitlab:4242, acme/platform/widgets) = (%v, %v), want (true, nil)", ok, err)
	}
	// A sibling INSIDE the nested group is still refused — the bind is the
	// whole path, not the group prefix.
	if ok, err := authorizer.AuthorizedGitLabProject(ctx, "gitlab:4242", "acme/platform/gadgets"); err != nil || ok {
		t.Errorf("AuthorizedGitLabProject(gitlab:4242, acme/platform/gadgets) = (%v, %v), want (false, nil)", ok, err)
	}
	// So is the parent group's own path.
	if ok, err := authorizer.AuthorizedGitLabProject(ctx, "gitlab:4242", "acme/platform"); err != nil || ok {
		t.Errorf("AuthorizedGitLabProject(gitlab:4242, acme/platform) = (%v, %v), want (false, nil)", ok, err)
	}
}

// TestInstallationGate_UnboundRowRefusesWithTheNamedSentinel is the UPGRADE
// case: a row inserted by raw SQL with project_path NULL — exactly the shape
// every installations row registered before migration 0078 has, and one the CLI
// can no longer produce. It must REFUSE through the shipped authorizer, and
// carry the named sentinel so the dispatcher audits
// gitlab_project_path_unbound rather than a generic lookup failure.
//
// Seeded BY CONSTRUCTION (raw INSERT), never through the register verb, so the
// RED under a deleted unbound guard lands on the behavioral assertion and not
// on fixture setup. The payload path is the one the PRE-#2877 namespace-only
// binding would have ADMITTED, which is what makes "refuses" meaningful here.
func TestInstallationGate_UnboundRowRefusesWithTheNamedSentinel(t *testing.T) {
	url := pgtest.NewURL(t)
	mustCreateAccount(t, url, "gitlab", "acme")

	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()
	ctx := context.Background()

	var acctID string
	if err := pool.QueryRow(ctx,
		`SELECT id FROM accounts WHERE provider = 'gitlab' AND account_key = 'acme'`).Scan(&acctID); err != nil {
		t.Fatalf("read seeded account: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO installations (id, account_id, provider, installation_ref)
		 VALUES (gen_random_uuid(), $1, 'gitlab', 'gitlab:4242')`, acctID); err != nil {
		t.Fatalf("seed pre-0078 installation row: %v", err)
	}

	authorizer := gitLabProjectRegistry{q: accountdb.New(pool)}
	ok, err := authorizer.AuthorizedGitLabProject(ctx, "gitlab:4242", "acme/widgets")
	if ok {
		t.Error("AuthorizedGitLabProject admitted an unbound row, want a refusal (no fallback to namespace-only)")
	}
	if !errors.Is(err, webhook.ErrGitLabProjectPathUnbound) {
		t.Errorf("err = %v, want webhook.ErrGitLabProjectPathUnbound (the audit reason depends on this identity)", err)
	}

	// The operability half: `installation list` marks the row so an operator
	// can ENUMERATE what needs re-registering. Both the upgrade remedy and the
	// rollback recovery in docs/deploy/gitlab.md rest on this rendering.
	listed := captureStdout(t, func() {
		var log bytes.Buffer
		if got := runInstallation([]string{"list", "--db", url}, &log); got != exitOK {
			t.Fatalf("list exit = %d; log:\n%s", got, log.String())
		}
	})
	if !strings.Contains(listed, "PROJECT_PATH") {
		t.Errorf("list %q has no PROJECT_PATH column", listed)
	}
	if !strings.Contains(listed, unboundProjectPathMarker) {
		t.Errorf("list %q does not mark the unbound gitlab row %q", listed, unboundProjectPathMarker)
	}

	// And re-registering REPAIRS the row in place (the documented remedy),
	// after which the same payload admits and the marker is gone.
	mustRegisterInstallation(t, url, "gitlab", "acme", "gitlab:4242", "acme/widgets")
	if ok, err := authorizer.AuthorizedGitLabProject(ctx, "gitlab:4242", "acme/widgets"); err != nil || !ok {
		t.Errorf("after re-registration, AuthorizedGitLabProject = (%v, %v), want (true, nil)", ok, err)
	}
	var count int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM installations WHERE provider = 'gitlab' AND installation_ref = 'gitlab:4242'`).Scan(&count); err != nil {
		t.Fatalf("recount: %v", err)
	}
	if count != 1 {
		t.Errorf("installations row count after repair = %d, want 1 (the upsert repairs in place)", count)
	}
	repaired := captureStdout(t, func() {
		var log bytes.Buffer
		if got := runInstallation([]string{"list", "--db", url}, &log); got != exitOK {
			t.Fatalf("list exit = %d; log:\n%s", got, log.String())
		}
	})
	if strings.Contains(repaired, unboundProjectPathMarker) {
		t.Errorf("list %q still marks the repaired row unbound", repaired)
	}
	if !strings.Contains(repaired, "acme/widgets") {
		t.Errorf("list %q does not render the repaired project_path", repaired)
	}
}

// TestRunInstallationList_RendersNoUnboundMarkerForGitHub is the paired control
// for the marker above: a github installation records no project path BY
// DESIGN, so marking it would manufacture a false alarm and send an operator
// re-registering something that is already correct.
func TestRunInstallationList_RendersNoUnboundMarkerForGitHub(t *testing.T) {
	url := pgtest.NewURL(t)
	mustCreateAccount(t, url, "github", "widgets")
	mustRegisterInstallation(t, url, "github", "widgets", "77", "")

	listed := captureStdout(t, func() {
		var log bytes.Buffer
		if got := runInstallation([]string{"list", "--db", url}, &log); got != exitOK {
			t.Fatalf("list exit = %d; log:\n%s", got, log.String())
		}
	})
	if !strings.Contains(listed, "77") {
		t.Fatalf("list %q missing the github installation", listed)
	}
	if strings.Contains(listed, unboundProjectPathMarker) {
		t.Errorf("list %q marks a github row unbound, want no marker", listed)
	}
}

func mustCreateAccount(t *testing.T, url, provider, key string) {
	t.Helper()
	var out bytes.Buffer
	if got := runAccount([]string{"create", "--db", url, "--provider", provider, "--account-key", key}, &out); got != exitOK {
		t.Fatalf("mustCreateAccount %s/%s exit = %d; log:\n%s", provider, key, got, out.String())
	}
}

// mustRegisterInstallation registers an installation, supplying --project-path
// only when one is given — a github registration takes none, and supplying an
// empty one would exercise a shape the flag does not have.
func mustRegisterInstallation(t *testing.T, url, provider, key, ref, projectPath string) {
	t.Helper()
	args := []string{"register", "--db", url, "--provider", provider, "--account-key", key, "--installation-ref", ref}
	if projectPath != "" {
		args = append(args, "--project-path", projectPath)
	}
	var out bytes.Buffer
	if got := runInstallation(args, &out); got != exitOK {
		t.Fatalf("mustRegisterInstallation %s/%s/%s exit = %d; log:\n%s", provider, key, ref, got, out.String())
	}
}
