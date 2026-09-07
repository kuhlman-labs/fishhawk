package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/oauthas"
	"github.com/kuhlman-labs/fishhawk/backend/internal/oauthstore"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
)

// TestRunOAuthToken_UnknownSubcommand pins that a bare/unknown oauth-token verb
// reaches its own usage banner naming `revoke`, not a dial.
func TestRunOAuthToken_UnknownSubcommand(t *testing.T) {
	var out strings.Builder
	if got := runOAuthToken([]string{"frobnicate"}, &out); got != exitUsage {
		t.Fatalf("exit = %d, want %d", got, exitUsage)
	}
	if !strings.Contains(out.String(), "unknown subcommand") || !strings.Contains(out.String(), "oauth token revoke --subject") {
		t.Errorf("output missing usage error naming revoke: %s", out.String())
	}
}

// TestOAuthTokenRevoke_RefusesMissingDB pins the --db guard: it fires before
// input validation and before any dial.
func TestOAuthTokenRevoke_RefusesMissingDB(t *testing.T) {
	t.Setenv("FISHHAWKD_DATABASE_URL", "")
	var out strings.Builder
	if got := runOAuthToken([]string{"revoke", "--subject", "github:octocat"}, &out); got != exitUsage {
		t.Fatalf("exit = %d, want %d:\n%s", got, exitUsage, out.String())
	}
	if !strings.Contains(out.String(), "--db (or FISHHAWKD_DATABASE_URL) required") {
		t.Errorf("output missing --db guard: %s", out.String())
	}
}

// TestOAuthTokenRevoke_RefusesEmptySubject pins the required-subject refusal on
// the SHIPPED verb path: an absent --subject, and a whitespace-only one, both
// exit usage naming --subject specifically, with the dummy --db never dialed
// (the guard fires before postgres.Connect — a dial would fail on the
// unresolvable host with a DIFFERENT exit code and message).
func TestOAuthTokenRevoke_RefusesEmptySubject(t *testing.T) {
	for _, args := range [][]string{
		{"revoke", "--db", dummyDBURL},
		{"revoke", "--db", dummyDBURL, "--subject", ""},
		{"revoke", "--db", dummyDBURL, "--subject", "   "},
	} {
		var out strings.Builder
		if got := runOAuthToken(args, &out); got != exitUsage {
			t.Fatalf("%v: exit = %d, want %d:\n%s", args, got, exitUsage, out.String())
		}
		if !strings.Contains(out.String(), "--subject required") {
			t.Errorf("%v: output does not name --subject as required: %s", args, out.String())
		}
	}
}

// TestOAuthTokenRevoke_RefusesWhitespacePaddedSubject pins that a padded
// subject is REFUSED naming --subject rather than silently trimmed into a
// different subject. The padded string is compared against ITSELF inside the
// guard (TrimSpace(s) != s), so the refusal cannot be satisfied by an unrelated
// mismatch; the message must carry the flag name.
func TestOAuthTokenRevoke_RefusesWhitespacePaddedSubject(t *testing.T) {
	for _, padded := range []string{" github:octocat", "github:octocat ", "\tgithub:octocat\n"} {
		var out strings.Builder
		got := runOAuthToken([]string{"revoke", "--db", dummyDBURL, "--subject", padded}, &out)
		if got != exitUsage {
			t.Fatalf("%q: exit = %d, want %d:\n%s", padded, got, exitUsage, out.String())
		}
		if !strings.Contains(out.String(), "--subject") || !strings.Contains(out.String(), "surrounding whitespace") {
			t.Errorf("%q: output does not name --subject's whitespace rule: %s", padded, out.String())
		}
		if strings.Contains(out.String(), "--client-id") {
			t.Errorf("%q: refusal blamed --client-id, want --subject: %s", padded, out.String())
		}
	}
}

// TestOAuthTokenRevoke_RefusesWhitespacePaddedClientID pins the same rule on the
// optional filter: a padded --client-id is refused naming --client-id (and NOT
// --subject, which is clean here), before any dial.
func TestOAuthTokenRevoke_RefusesWhitespacePaddedClientID(t *testing.T) {
	var out strings.Builder
	got := runOAuthToken([]string{"revoke", "--db", dummyDBURL, "--subject", "github:octocat", "--client-id", " https://client.example.com/c "}, &out)
	if got != exitUsage {
		t.Fatalf("exit = %d, want %d:\n%s", got, exitUsage, out.String())
	}
	if !strings.Contains(out.String(), "--client-id") || !strings.Contains(out.String(), "surrounding whitespace") {
		t.Errorf("output does not name --client-id's whitespace rule: %s", out.String())
	}
	if strings.Contains(out.String(), "--subject") {
		t.Errorf("refusal blamed --subject, want --client-id: %s", out.String())
	}
}

// TestResolveOAuthTokenRevocation_PassesCleanInputUnchanged pins the pure
// validator's happy path: clean inputs come back byte-identical (no trimming,
// no normalization), and an empty --client-id is preserved as "every client".
func TestResolveOAuthTokenRevocation_PassesCleanInputUnchanged(t *testing.T) {
	in := oauthTokenRevocation{Subject: "github:octocat", ClientID: ""}
	got, err := resolveOAuthTokenRevocation(in)
	if err != nil || got != in {
		t.Fatalf("resolve(%+v) = (%+v, %v), want the input unchanged", in, got, err)
	}
	in.ClientID = "https://client.example.com/oauth/client"
	if got, err := resolveOAuthTokenRevocation(in); err != nil || got != in {
		t.Fatalf("resolve(%+v) = (%+v, %v), want the input unchanged", in, got, err)
	}
}

// --- DB-backed cases -------------------------------------------------------

// mintGrantFor mints one AS grant for subject/clientID straight through the
// store, so the verb under test has real rows to revoke.
func mintGrantFor(t *testing.T, pool *pgxpool.Pool, subject, clientID string) *oauthstore.IssuedGrant {
	t.Helper()
	repo := oauthstore.NewPostgresRepository(pool)
	now := time.Now().UTC()
	code, err := repo.CreateAuthorizationCode(context.Background(), oauthstore.NewAuthorizationCode{
		ClientID:            clientID,
		RedirectURI:         "http://127.0.0.1:8765/callback",
		CodeChallenge:       oauthas.DeriveS256Challenge("verifier-verifier-verifier-verifier"),
		CodeChallengeMethod: oauthas.CodeChallengeMethodS256,
		Scopes:              []string{"runs:read"},
		Subject:             subject,
		Provider:            "github",
		ExpiresAt:           now.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("CreateAuthorizationCode: %v", err)
	}
	grant, err := repo.RedeemAuthorizationCode(context.Background(), code.PlainText, nil, oauthstore.RedemptionRequest{
		AccessTokenExpiry:  now.Add(15 * time.Minute),
		RefreshTokenExpiry: now.Add(24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("RedeemAuthorizationCode: %v", err)
	}
	return grant
}

// TestRunOAuthTokenRevoke_RevokesSubjectAndReportsCounts drives the SHIPPED verb
// against the real store: it revokes exactly the named subject's pair, prints
// both counts and the resolved filter, leaves another subject live, and reports
// (0, 0) on a second invocation — read back through the store's own consuming
// paths, not inferred from the exit code.
func TestRunOAuthTokenRevoke_RevokesSubjectAndReportsCounts(t *testing.T) {
	url := pgtest.NewURL(t)
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()
	const clientID = "https://client.example.com/oauth/client"
	target := mintGrantFor(t, pool, "github:octocat", clientID)
	bystander := mintGrantFor(t, pool, "github:hubot", clientID)

	out := captureStdout(t, func() {
		var log bytes.Buffer
		if got := runOAuthToken([]string{"revoke", "--db", url, "--subject", "github:octocat"}, &log); got != exitOK {
			t.Fatalf("revoke exit = %d; log:\n%s", got, log.String())
		}
	})
	for _, want := range []string{"subject=github:octocat", "client_id=*", "access_revoked=1", "refresh_revoked=1"} {
		if !strings.Contains(out, want) {
			t.Errorf("revoke output = %q, want it to carry %s", out, want)
		}
	}

	repo := oauthstore.NewPostgresRepository(pool)
	if _, err := repo.AuthenticateAccessToken(context.Background(), target.Access.PlainText); !errors.Is(err, oauthstore.ErrRevoked) {
		t.Errorf("target access token after the verb = %v, want ErrRevoked", err)
	}
	if _, err := repo.RotateRefreshToken(context.Background(), target.Refresh.PlainText, nil, oauthstore.RotationRequest{
		AccessTokenExpiry: time.Now().Add(time.Minute), RefreshTokenExpiry: time.Now().Add(time.Hour),
	}); !errors.Is(err, oauthstore.ErrRevoked) {
		t.Errorf("target refresh token after the verb = %v, want ErrRevoked", err)
	}
	if _, err := repo.AuthenticateAccessToken(context.Background(), bystander.Access.PlainText); err != nil {
		t.Errorf("the OTHER subject's access token was caught by the verb: %v", err)
	}

	again := captureStdout(t, func() {
		var log bytes.Buffer
		if got := runOAuthToken([]string{"revoke", "--db", url, "--subject", "github:octocat"}, &log); got != exitOK {
			t.Fatalf("second revoke exit = %d; log:\n%s", got, log.String())
		}
	})
	if !strings.Contains(again, "access_revoked=0") || !strings.Contains(again, "refresh_revoked=0") {
		t.Errorf("second revoke output = %q, want zero counts (already revoked is a successful no-op)", again)
	}
}

// TestRunOAuthTokenRevoke_ClientFilterRendersAndNarrows pins the optional
// --client-id filter end to end: only the named client's pair for the subject is
// revoked, the other client's pair stays live, and the output echoes the filter.
func TestRunOAuthTokenRevoke_ClientFilterRendersAndNarrows(t *testing.T) {
	url := pgtest.NewURL(t)
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()
	const clientA = "https://a.example.com/oauth/client"
	const clientB = "https://b.example.com/oauth/client"
	underA := mintGrantFor(t, pool, "github:octocat", clientA)
	underB := mintGrantFor(t, pool, "github:octocat", clientB)

	out := captureStdout(t, func() {
		var log bytes.Buffer
		if got := runOAuthToken([]string{"revoke", "--db", url, "--subject", "github:octocat", "--client-id", clientA}, &log); got != exitOK {
			t.Fatalf("revoke exit = %d; log:\n%s", got, log.String())
		}
	})
	if !strings.Contains(out, "client_id="+clientA) || !strings.Contains(out, "access_revoked=1") {
		t.Errorf("filtered revoke output = %q, want the filter echoed and one access row", out)
	}
	repo := oauthstore.NewPostgresRepository(pool)
	if _, err := repo.AuthenticateAccessToken(context.Background(), underA.Access.PlainText); !errors.Is(err, oauthstore.ErrRevoked) {
		t.Errorf("client A's access token = %v, want ErrRevoked", err)
	}
	if _, err := repo.AuthenticateAccessToken(context.Background(), underB.Access.PlainText); err != nil {
		t.Errorf("client B's access token was caught by the filtered verb: %v", err)
	}
}
