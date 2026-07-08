package main

import (
	"bytes"
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
)

// TestOperatorDefaultScopes_IncludesWriteDeploy pins #1390: the deploy gate
// (deploy approval + deploy ship/rollback operator bearer paths) requires the
// write:deploy scope, so every operator token issued with the default set —
// and every `token migrate --apply` promotion, which reuses this same set as
// the target (token.go runTokenMigrate) — must carry it. Without this, a fresh
// operator token could never approve or roll back a deploy.
func TestOperatorDefaultScopes_IncludesWriteDeploy(t *testing.T) {
	if !containsScope(operatorDefaultScopes, "write:deploy") {
		t.Fatalf("operatorDefaultScopes must contain write:deploy; got %v", operatorDefaultScopes)
	}
}

// TestOperatorDefaultScopes_IncludesWriteCampaigns pins #1474: the campaign
// primitive (POST/GET /v0/campaigns and POST /v0/campaigns/{id}/resume) requires
// the write:campaigns scope, so every operator token issued with the default set
// — and every `token migrate --apply` promotion — must carry it. Without this,
// existing operator tokens 403 on all campaign tools.
func TestOperatorDefaultScopes_IncludesWriteCampaigns(t *testing.T) {
	if !containsScope(operatorDefaultScopes, "write:campaigns") {
		t.Fatalf("operatorDefaultScopes must contain write:campaigns; got %v", operatorDefaultScopes)
	}
}

// TestOperatorDefaultScopes_RetainsBaseline guards against an accidental
// replacement (rather than addition) of the scope set when a new scope is
// added: the pre-existing operator scopes must still be present.
func TestOperatorDefaultScopes_RetainsBaseline(t *testing.T) {
	for _, want := range []string{"read:runs", "read:audit", "write:runs", "write:approvals", "write:stages", "write:deploy"} {
		if !containsScope(operatorDefaultScopes, want) {
			t.Errorf("operatorDefaultScopes missing baseline scope %q; got %v", want, operatorDefaultScopes)
		}
	}
}

func containsScope(scopes []string, want string) bool {
	for _, s := range scopes {
		if s == want {
			return true
		}
	}
	return false
}

// TestRunTokenIssue_DefaultsAuthMethodStatic pins #1708: `fishhawkd token
// issue` mints a token through the static apitoken.Issue path, so the persisted
// row must record auth_method='static' (via the column DEFAULT — condition (1):
// the static path is not changed) with a NULL provider. The OAuth
// (auth_method='oauth') path is minted by the login endpoint, not this command.
func TestRunTokenIssue_DefaultsAuthMethodStatic(t *testing.T) {
	url := pgtest.NewURL(t) // freshly migrated per-test database

	var logSink bytes.Buffer
	code := runTokenIssue([]string{"--db", url, "--subject", "github:42"}, &logSink)
	if code != exitOK {
		t.Fatalf("runTokenIssue exit = %d, want %d; log:\n%s", code, exitOK, logSink.String())
	}

	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	var authMethod string
	var provider *string
	if err := pool.QueryRow(context.Background(),
		`SELECT auth_method, provider FROM api_tokens WHERE subject = $1`, "github:42",
	).Scan(&authMethod, &provider); err != nil {
		t.Fatalf("read back issued token: %v", err)
	}
	if authMethod != "static" {
		t.Errorf("issued token auth_method = %q, want static", authMethod)
	}
	if provider != nil {
		t.Errorf("issued token provider = %q, want NULL for a static token", *provider)
	}
}
