package oauthas

import "strings"

// SupportedScopes is the ratified operator scope vocabulary (#2391: one scope
// language in the product, no parallel MCP-only vocabulary). It is a fixed-order
// read-only MIRROR of operatorDefaultScopes in backend/cmd/fishhawkd/token.go,
// which is the single source of truth.
//
// The two lists cannot share a symbol: operatorDefaultScopes lives in the main
// package backend/cmd/fishhawkd and is unimportable here. Rather than assert the
// mirror with a hardcoded duplicate (a false drift detector), the drift test
// TestSupportedScopes_MatchesOperatorDefault AST-parses token.go and compares the
// parsed literal against this slice, so real drift fails the in-loop verify gate.
//
// Publishing scopes_supported makes later narrowing additive.
var SupportedScopes = []string{
	"read:runs",
	"read:audit",
	"write:runs",
	"write:approvals",
	"write:stages",
	"write:deploy",
	"write:campaigns",
	"read:audit-export",
}

// IsSupportedScope reports whether s is a member of SupportedScopes.
func IsSupportedScope(s string) bool {
	for _, sc := range SupportedScopes {
		if sc == s {
			return true
		}
	}
	return false
}

// ParseScope parses an RFC 6749 §3.3 space-delimited scope request. It splits on
// the ASCII space only, rejects an empty request and any unknown scope with
// invalid_scope, and de-duplicates while preserving first-seen order.
func ParseScope(raw string) ([]string, error) {
	if raw == "" {
		return nil, newError(ErrCodeInvalidScope, "scope request is empty")
	}
	parts := strings.Split(raw, " ")
	seen := make(map[string]bool, len(parts))
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p == "" {
			continue
		}
		if !IsSupportedScope(p) {
			return nil, newError(ErrCodeInvalidScope, "unsupported scope %q", p)
		}
		if seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	if len(out) == 0 {
		return nil, newError(ErrCodeInvalidScope, "scope request is empty")
	}
	return out, nil
}

// ScopeString joins scopes with the RFC 6749 §3.3 ASCII-space delimiter.
func ScopeString(scopes []string) string {
	return strings.Join(scopes, " ")
}
