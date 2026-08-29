package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	accountdb "github.com/kuhlman-labs/fishhawk/backend/internal/account/db"
	"github.com/kuhlman-labs/fishhawk/backend/internal/auth"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
)

// TestRunMemberInvite_UsageErrors covers each missing-required flag branch with
// NO database: every case must exit exitUsage AND name the offending flag in the
// log, so a command failing for an unrelated reason cannot green a case. The
// dummy --db URL is never connected to because the guards fire first.
func TestRunMemberInvite_UsageErrors(t *testing.T) {
	const dummyDB = "postgres://x:y@nowhere/db"
	cases := []struct {
		name    string
		args    []string
		wantSub string
	}{
		{"missing db", []string{"invite", "--provider", "github", "--account-key", "acme", "--member-ref", "octocat"}, "--db"},
		{"missing provider", []string{"invite", "--db", dummyDB, "--account-key", "acme", "--member-ref", "octocat"}, "--provider required"},
		{"missing account-key", []string{"invite", "--db", dummyDB, "--provider", "github", "--member-ref", "octocat"}, "--account-key required"},
		{"missing member-ref", []string{"invite", "--db", dummyDB, "--provider", "github", "--account-key", "acme"}, "--member-ref required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			got := runMember(tc.args, &out)
			if got != exitUsage {
				t.Fatalf("exit = %d, want %d; log:\n%s", got, exitUsage, out.String())
			}
			if !strings.Contains(out.String(), tc.wantSub) {
				t.Errorf("log %q does not name %q", out.String(), tc.wantSub)
			}
		})
	}
}

// TestRunMemberList_MissingDBIsUsage covers the list verb's own --db guard.
func TestRunMemberList_MissingDBIsUsage(t *testing.T) {
	var out bytes.Buffer
	if got := runMember([]string{"list"}, &out); got != exitUsage {
		t.Fatalf("exit = %d, want %d; log:\n%s", got, exitUsage, out.String())
	}
	if !strings.Contains(out.String(), "--db") {
		t.Errorf("log %q does not name --db", out.String())
	}
}

// TestRunMember_UnknownSubcommand pins the dispatch usage path.
func TestRunMember_UnknownSubcommand(t *testing.T) {
	var out bytes.Buffer
	if got := runMember([]string{"banana"}, &out); got != exitUsage {
		t.Fatalf("exit = %d, want %d", got, exitUsage)
	}
	if !strings.Contains(out.String(), "unknown subcommand") {
		t.Errorf("log missing usage error: %s", out.String())
	}
}

// TestRunMemberInvite_InvalidRoleIsUsage asserts a validation failure reaching
// the domain layer (an out-of-set --role, which passes the flag-presence guards)
// is mapped to exitUsage via errors.Is(ErrValidation), against a real database so
// the case is not short-circuited by a connect failure. This is the CLI half of
// the role allow-list counterfactual: deleting the allow-list branch persists the
// bad role and returns exitOK, reddening this case.
func TestRunMemberInvite_InvalidRoleIsUsage(t *testing.T) {
	url := pgtest.NewURL(t)
	// The account must exist so the role check (not the missing-account check) is
	// what refuses.
	seedAccount(t, url, "github", "acme")

	var out bytes.Buffer
	got := runMember([]string{"invite", "--db", url, "--provider", "github", "--account-key", "acme", "--member-ref", "octocat", "--role", "superuser"}, &out)
	if got != exitUsage {
		t.Fatalf("exit = %d, want %d (validation → usage); log:\n%s", got, exitUsage, out.String())
	}
	if !strings.Contains(out.String(), `"superuser"`) {
		t.Errorf("log %q does not name the rejected role", out.String())
	}
}

// TestRunMemberInvite_UnknownAccountFailsClosed is THE fail-closed case: inviting
// into an account key with no accounts row exits exitFailure (NOT exitUsage — the
// exit-code split is the contract), the log names `fishhawkd account create`, and
// a direct count of account_members after the call returns 0 — reading committed
// state, because a control that fires and rolls back returns a byte-identical
// error. Deleting the pgx.ErrNoRows→ErrAccountNotFound branch in InviteMember
// reddens both the exit-code and the remedy-message assertion.
func TestRunMemberInvite_UnknownAccountFailsClosed(t *testing.T) {
	url := pgtest.NewURL(t)

	var out bytes.Buffer
	got := runMember([]string{"invite", "--db", url, "--provider", "github", "--account-key", "never-created", "--member-ref", "octocat"}, &out)
	if got != exitFailure {
		t.Fatalf("exit = %d, want %d (unknown account → failure, not usage); log:\n%s", got, exitFailure, out.String())
	}
	if !strings.Contains(out.String(), "fishhawkd account create") {
		t.Errorf("log %q does not name the account-create remedy", out.String())
	}

	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()
	var count int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM account_members`).Scan(&count); err != nil {
		t.Fatalf("count members: %v", err)
	}
	if count != 0 {
		t.Errorf("account_members count = %d after a fail-closed invite, want 0 (no implicit write)", count)
	}
}

// TestRunMemberInvite_PrintsSuccessLineAndSeamAdmitsDBOnly is the cross-boundary
// seam test (CLI → domain → sqlc → Postgres → login gate). It drives the real
// verbs, asserts the invite success line prints every documented field
// (condition 4), then constructs the real membership resolver with an EMPTY
// lister registry — the load-bearing part: it proves the invited grant admits
// from the database ALONE, with no registered provider lister consulted
// (ADR-057 Amendment A2) — and asserts the invited login is admitted to exactly
// the created account while an uninvited login is admitted to nothing.
func TestRunMemberInvite_PrintsSuccessLineAndSeamAdmitsDBOnly(t *testing.T) {
	url := pgtest.NewURL(t)

	out := captureStdout(t, func() {
		var log bytes.Buffer
		if got := runAccount([]string{"create", "--db", url, "--provider", "github", "--account-key", "acme"}, &log); got != exitOK {
			t.Fatalf("account create exit = %d; log:\n%s", got, log.String())
		}
		if got := runMember([]string{"invite", "--db", url, "--provider", "github", "--account-key", "acme", "--member-ref", "octocat", "--role", "admin"}, &log); got != exitOK {
			t.Fatalf("member invite exit = %d; log:\n%s", got, log.String())
		}
	})
	// Condition 4: the success line names id, account_key, provider, member_ref,
	// role and origin=invited.
	for _, want := range []string{"invited member id=", "account_key=acme", "provider=github", "member_ref=octocat", "role=admin", "origin=invited"} {
		if !strings.Contains(out, want) {
			t.Errorf("invite success line %q missing %q", out, want)
		}
	}

	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()
	ctx := context.Background()

	var acctID string
	if err := pool.QueryRow(ctx, `SELECT id FROM accounts WHERE provider = 'github' AND account_key = 'acme'`).Scan(&acctID); err != nil {
		t.Fatalf("read account id: %v", err)
	}

	// The empty lister map is deliberate: an invited grant must admit with NO
	// registered lister, forge-independently.
	resolver := auth.NewMembershipResolver(
		auth.NewAccountMembershipStore(accountdb.New(pool)),
		map[string]auth.ForgeMembershipLister{},
	)

	ids, err := resolver.ResolveAccounts(ctx, "github", "", auth.GitHubProfile{Login: "octocat"})
	if err != nil {
		t.Fatalf("ResolveAccounts(invited): %v", err)
	}
	if len(ids) != 1 || ids[0].String() != acctID {
		t.Errorf("invited login admitted to %v, want exactly [%s]", ids, acctID)
	}

	uninvited, err := resolver.ResolveAccounts(ctx, "github", "", auth.GitHubProfile{Login: "stranger"})
	if err != nil {
		t.Fatalf("ResolveAccounts(uninvited): %v", err)
	}
	if len(uninvited) != 0 {
		t.Errorf("uninvited login admitted to %v, want nothing", uninvited)
	}
}

// TestRunMemberList_RendersOriginAndFilters asserts the list surface: the
// empty-result line on a fresh database, an invited row rendered with its origin
// and owning account_key, and the --provider / --account-key filters (a row under
// a different provider is excluded).
func TestRunMemberList_RendersOriginAndFilters(t *testing.T) {
	url := pgtest.NewURL(t)

	empty := captureStdout(t, func() {
		var log bytes.Buffer
		if got := runMember([]string{"list", "--db", url}, &log); got != exitOK {
			t.Fatalf("list on empty db exit = %d; log:\n%s", got, log.String())
		}
	})
	if !strings.Contains(empty, "no members granted") {
		t.Errorf("empty list output %q missing the no-members line", empty)
	}

	seedAccount(t, url, "github", "acme")
	seedAccount(t, url, "gitlab", "widgets")
	invite := func(provider, key, ref string) {
		var log bytes.Buffer
		if got := runMember([]string{"invite", "--db", url, "--provider", provider, "--account-key", key, "--member-ref", ref}, &log); got != exitOK {
			t.Fatalf("invite %s/%s/%s exit = %d; log:\n%s", provider, key, ref, got, log.String())
		}
	}
	invite("github", "acme", "octocat")
	invite("gitlab", "widgets", "gluser")

	all := captureMemberList(t, []string{"list", "--db", url})
	if !strings.Contains(all, "octocat") || !strings.Contains(all, "invited") || !strings.Contains(all, "acme") {
		t.Errorf("unfiltered list %q missing the invited row / origin / account_key", all)
	}

	byProvider := captureMemberList(t, []string{"list", "--db", url, "--provider", "github"})
	if !strings.Contains(byProvider, "octocat") {
		t.Errorf("github-filtered list %q missing octocat", byProvider)
	}
	if strings.Contains(byProvider, "gluser") {
		t.Errorf("github-filtered list %q leaked the gitlab member gluser", byProvider)
	}

	byKey := captureMemberList(t, []string{"list", "--db", url, "--account-key", "widgets"})
	if !strings.Contains(byKey, "gluser") {
		t.Errorf("widgets-filtered list %q missing gluser", byKey)
	}
	if strings.Contains(byKey, "octocat") {
		t.Errorf("widgets-filtered list %q leaked the acme member octocat", byKey)
	}
}

// seedAccount creates an account via the shipped account verb so the member
// tests do not hand-roll an INSERT.
func seedAccount(t *testing.T, url, provider, key string) {
	t.Helper()
	var out bytes.Buffer
	if got := runAccount([]string{"create", "--db", url, "--provider", provider, "--account-key", key}, &out); got != exitOK {
		t.Fatalf("seed account %s/%s exit = %d; log:\n%s", provider, key, got, out.String())
	}
}

// captureMemberList runs `member list` capturing stdout (where the rendered table
// goes) and returns it.
func captureMemberList(t *testing.T, args []string) string {
	t.Helper()
	return captureStdout(t, func() {
		var log bytes.Buffer
		if got := runMember(args, &log); got != exitOK {
			t.Fatalf("member list exit = %d; log:\n%s", got, log.String())
		}
	})
}
