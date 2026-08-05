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

// ResolveRequestedScope resolves the scope set an authorization request is to be
// granted, taking RFC 6749 §3.3's OTHER branch for a request that CARRIES NO
// SCOPE TOKEN: process it with a pre-defined default instead of failing
// invalid_scope (#2466 — a scope-omitting client, the shape of Claude Code's
// CIMD, could not otherwise get past the first authorize).
//
// The contract, in order:
//
//   - requested != "" — the PRESENT path. Delegated to ParseScope UNCHANGED, so
//     every present-value branch (unknown scope, whitespace-only, dedup,
//     multi-space) keeps its current behaviour and error identity. In
//     particular `scope=%20%20` still fails invalid_scope.
//   - requested == "" — CARRIES NO SCOPE TOKEN, and defaults. url.Values.Get
//     cannot distinguish an ABSENT `scope` key from a present-but-empty
//     `scope=`, and this server deliberately does not try to: an empty value
//     carries zero scope tokens, so it is semantically identical to omission
//     and takes the same default. Making a client that serializes an empty list
//     as `scope=` fail while one that omits the key succeeds would be exactly
//     the onboarding trap #2466 exists to remove.
//
// The default is the client's REGISTERED scope when the registration pins one,
// otherwise the whole SupportedScopes vocabulary (which is what the PRM and AS
// metadata advertise as scopes_supported).
//
// The registered default is INTERSECTED with SupportedScopes rather than taken
// verbatim: a registration may pin scopes outside this server's vocabulary
// (registeredScopeSet splits the raw registration string and never validates its
// members), and such a scope is already UNREQUESTABLE through the explicit path
// because ParseScope rejects it before the registered-scope restriction is ever
// reached. Defaulting to it verbatim would grant through the default what the
// explicit path refuses. A registration whose intersection is EMPTY fails CLOSED
// with invalid_scope rather than minting a code carrying an empty grant.
func ResolveRequestedScope(requested string, registered []string) ([]string, error) {
	if requested != "" {
		return ParseScope(requested)
	}
	if len(registered) == 0 {
		// A defensive COPY: the caller stores this on the code row, and handing
		// out the package slice's backing array is a mutation hazard.
		out := make([]string, len(SupportedScopes))
		copy(out, SupportedScopes)
		return out, nil
	}
	seen := make(map[string]bool, len(registered))
	out := make([]string, 0, len(registered))
	for _, s := range registered {
		if !IsSupportedScope(s) || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil, newError(ErrCodeInvalidScope,
			"the client's registered scope names no scope this authorization server supports, so a scope-less request cannot be defaulted")
	}
	return out, nil
}

// ScopeString joins scopes with the RFC 6749 §3.3 ASCII-space delimiter.
func ScopeString(scopes []string) string {
	return strings.Join(scopes, " ")
}
