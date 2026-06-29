package main

import "testing"

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
