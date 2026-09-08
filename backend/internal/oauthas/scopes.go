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
// SupportedScopes is the VOCABULARY, not the POSTURE. It is what ParseScope
// validates against and what `fishhawkd token issue` mints; DefaultScopes below
// is what this server ADVERTISES and DEFAULTS TO. The two are deliberately
// distinct — see DefaultScopes for why (#2477).
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

// DefaultScopes is the ADVERTISED and DEFAULTED scope POSTURE (#2477): what the
// PRM and AS metadata publish as scopes_supported, and what an authorization
// request carrying NO scope token is granted when the client's registration pins
// nothing. It is SupportedScopes in its existing order MINUS write:deploy.
//
// WHY IT IS A SECOND LIST rather than a narrowing of SupportedScopes: the
// vocabulary and the posture answer different questions and must be free to
// differ. SupportedScopes is the one scope language of the product (#2391) and an
// order-exact mirror of operatorDefaultScopes in backend/cmd/fishhawkd/token.go;
// narrowing IT would change what `fishhawkd token issue` mints on an operator
// token and would delete a scope from the language. DefaultScopes answers only
// "what should the least-effort path for a first-time MCP client grant?", and the
// answer is: not authority to ship (#2471 — a client that requests
// scopes_supported verbatim, or omits scope entirely, was getting write:deploy).
//
// INVARIANT: DefaultScopes is a STRICT SUBSET of SupportedScopes. A member absent
// from SupportedScopes is a programming error — it would be advertised and
// defaulted while ParseScope refuses it on the explicit path, so a client could
// not re-request what it was granted. TestDefaultScopes_IsStrictSubsetOfSupportedScopes
// holds that up.
var DefaultScopes = []string{
	"read:runs",
	"read:audit",
	"write:runs",
	"write:approvals",
	"write:stages",
	"write:campaigns",
	"read:audit-export",
}

// PreRegistrationOnlyScopes names the scopes that are UNREACHABLE through an
// ordinary authorization request. This is the ONE place the advertised set and
// the mintable set intentionally disagree AT REQUEST TIME: a scope named here
// stays in SupportedScopes — fully mintable, fully carryable on an operator
// token issued by `fishhawkd token issue` — but an authorization request naming
// it is refused invalid_scope UNLESS the client's registration PINS it.
//
// FORWARD RULE: a future scope needing the same treatment is added to THIS list.
// Never reuse DefaultScopes as an enforcement bound — DefaultScopes is posture
// (what we advertise and default to), not a boundary; conflating them would mean
// that advertising a scope more narrowly silently made it unrequestable.
//
// RESIDUAL, stated plainly: "the client's registration" is whatever
// registeredScopeSet reads off the resolved client, and resolveOAuthClient
// resolves STORE-FIRST then falls through to the client's own CIMD document
// (backend/internal/server/oauthas.go). A store row is an operator act — the
// `fishhawkd oauth client register --scope` write path (E66.21 / #2438), which
// has SHIPPED. A CIMD document is authored by the CLIENT, and its `scope` member
// is unvalidated passthrough. So for a CIMD-resolved client this bound is NOT an
// operator gate: such a client can self-declare write:deploy in its own metadata
// and pin itself. Narrowing the pin to store-resolved registrations only is a
// deliberate follow-up, not part of #2477's ratified posture.
var PreRegistrationOnlyScopes = []string{"write:deploy"}

// isPreRegistrationOnlyScope reports whether s is a member of
// PreRegistrationOnlyScopes.
func isPreRegistrationOnlyScope(s string) bool {
	for _, sc := range PreRegistrationOnlyScopes {
		if sc == s {
			return true
		}
	}
	return false
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
//     particular `scope=%20%20` still fails invalid_scope. ONLY AFTER ParseScope
//     succeeds — so no error identity moves — each resolved scope is checked
//     against PreRegistrationOnlyScopes and refused invalid_scope unless the
//     registration pins it. A MIXED request naming such a scope alongside valid
//     ones is refused WHOLE, never silently stripped: stripping would mint a
//     grant narrower than the one the client asked for and the consent page
//     displayed, which is a consent/grant divergence. RFC 6749 §3.3 explicitly
//     permits failing invalid_scope on an invalid scope value.
//   - requested == "" — CARRIES NO SCOPE TOKEN, and defaults. url.Values.Get
//     cannot distinguish an ABSENT `scope` key from a present-but-empty
//     `scope=`, and this server deliberately does not try to: an empty value
//     carries zero scope tokens, so it is semantically identical to omission
//     and takes the same default. Making a client that serializes an empty list
//     as `scope=` fail while one that omits the key succeeds would be exactly
//     the onboarding trap #2466 exists to remove.
//
// The default is the client's REGISTERED scope when the registration pins one,
// otherwise DefaultScopes — the advertised POSTURE, which is what the PRM and AS
// metadata publish as scopes_supported. It is deliberately NOT the whole
// SupportedScopes vocabulary (#2477): the least-effort path for a first-time
// client must not be authority to ship.
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
		scopes, err := ParseScope(requested)
		if err != nil {
			return nil, err
		}
		// The pre-registration bound. Applied AFTER ParseScope so an unknown or
		// whitespace-only request keeps its existing error identity, and applied
		// to the WHOLE request so a mixed one is refused rather than stripped.
		for _, s := range scopes {
			if isPreRegistrationOnlyScope(s) && !containsScope(registered, s) {
				return nil, newError(ErrCodeInvalidScope,
					"scope %q is reachable only through a client registration that pins it, not through an authorization request", s)
			}
		}
		return scopes, nil
	}
	if len(registered) == 0 {
		// A defensive COPY: the caller stores this on the code row, and handing
		// out the package slice's backing array is a mutation hazard.
		out := make([]string, len(DefaultScopes))
		copy(out, DefaultScopes)
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

// containsScope reports whether ss contains want.
func containsScope(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// ScopeString joins scopes with the RFC 6749 §3.3 ASCII-space delimiter.
func ScopeString(scopes []string) string {
	return strings.Join(scopes, " ")
}
