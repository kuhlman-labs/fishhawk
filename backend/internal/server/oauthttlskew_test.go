package server

import (
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/credstore"
)

// TestDefaultAccessTokenTTLAndRefreshSkewAgree is the cross-module pin
// for #2393 / ADR-076: the authorization server's shipped access-token
// default, and the proactive-refresh margin the shared credstore
// DERIVES from it, must agree so a future TTL change cannot leave the
// margin thinner than a refresh round-trip — that fails HERE, not in
// production as a mid-session 401. It also pins credstore's fallback
// lifetime (used when a stored credential carries no issued_at) equal
// to the server default (binding condition 6), so a credential whose
// lifetime cannot be measured is refreshed on the same margin the AS
// actually issues.
//
// Sites to update when either half moves: backend/internal/server
// /oauthas.go (defaultOAuthAccessTokenTTL), backend/cmd/fishhawkd
// /serve.go (the --oauth-access-token-ttl default), credstore
// /credstore.go (DefaultLifetime + RefreshSkew), credstore/README.md,
// backend/cmd/fishhawk-mcp/README.md, backend/cmd/fishhawkd/README.md.
func TestDefaultAccessTokenTTLAndRefreshSkewAgree(t *testing.T) {
	if defaultOAuthAccessTokenTTL != 15*time.Minute {
		t.Fatalf("defaultOAuthAccessTokenTTL = %v, want 15m (the #2393 short-lived default)", defaultOAuthAccessTokenTTL)
	}
	if got := credstore.RefreshSkew(defaultOAuthAccessTokenTTL); got != 2*time.Minute {
		t.Fatalf("credstore.RefreshSkew(%v) = %v, want 2m — the derived margin no longer matches the shipped TTL", defaultOAuthAccessTokenTTL, got)
	}
	if credstore.DefaultLifetime != defaultOAuthAccessTokenTTL {
		t.Fatalf("credstore.DefaultLifetime = %v, want the server default %v (binding condition 6: the issued_at-absent fallback must equal the AS default)", credstore.DefaultLifetime, defaultOAuthAccessTokenTTL)
	}
}
