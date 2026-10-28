package identity

import "strings"

// Credential kinds (#4633): the forge-neutral vocabulary naming HOW the
// credential that authenticated a request was minted. SubjectListed is a
// SUBJECT-SHAPE rule only; the approvals.members credential leg pairs it with
// ForgeVerifiedCredential so a listed forge subject counts only when the
// presenting credential was itself minted through a forge-verified sign-in.
// Kinds are derived ONCE, by CredentialKind below; the server's approval gate
// and `fishhawkd approver-members` both classify through it and consult the
// one allow-list below, so the gate and the impact inventory cannot disagree.
// The rule's decision record is ADR-123 (#4750), RATIFIED 2026-10-27.
const (
	// CredentialSession is a browser session cookie. auth.SignIn is the
	// only session creator and is called only from the forge OAuth callback,
	// so the session's subject was verified by the forge.
	CredentialSession = "session"
	// CredentialDeviceFlow is an api token minted through the OAuth device
	// flow (IssueOAuth, called only from the device-flow mint). The value
	// matches the api_tokens.auth_method column.
	CredentialDeviceFlow = "oauth"
	// CredentialOAuthAS is an authorization-server (`fho_`) access token.
	// handleOAuthConsent refuses a consent without a forge session, so the
	// token's subject is the session's forge-verified subject.
	CredentialOAuthAS = "oauth_as"
	// CredentialStatic is an operator-minted api token (`fishhawkd token
	// issue`, or a child token from POST /v0/tokens): its subject is
	// whatever the minter wrote, never verified by a forge.
	CredentialStatic = "static"
	// CredentialUnrecorded is an api token whose auth_method is empty, so its
	// minting path cannot be decided.
	CredentialUnrecorded = "unrecorded"
	// CredentialMCPRun is a run-bound MCP token (subject mcp:run:<uuid>).
	CredentialMCPRun = "mcp_run"
	// CredentialUnknown is any other auth_method value, or an identity that
	// carries no credential marker at all.
	CredentialUnknown = "unknown"
)

// ForgeVerifiedKinds lists the credential kinds ForgeVerifiedCredential
// admits, in a stable order, for rendering into refusal details and docs.
var ForgeVerifiedKinds = []string{CredentialSession, CredentialDeviceFlow, CredentialOAuthAS}

// ForgeVerifiedCredential reports whether kind is a forge-verified credential
// kind (#4633): an ALLOW-LIST, so every kind not named here — including the
// empty string, an unrecognised value and a differently-cased spelling — is
// refused. Comparison is byte-exact; no case folding.
func ForgeVerifiedCredential(kind string) bool {
	switch kind {
	case CredentialSession, CredentialDeviceFlow, CredentialOAuthAS:
		return true
	default:
		return false // not a forge-verified credential kind
	}
}

// CredentialKind classifies the credential that authenticated a request into
// the Credential* vocabulary (#4633, ADR-123 D1). Its arguments are the
// trusted markers the auth middleware set on the request identity — the
// cookie session id, the api token id, the validated OAuth audience, the
// subject and the token's auth_method — never request-supplied values. The
// order is load-bearing:
//
//   - a cookie session (sessionID set, no tokenID) is CredentialSession;
//   - an AS-issued `fho_` token (oauthAudience set, which the middleware does
//     only after audience validation) is CredentialOAuthAS, checked BEFORE
//     authMethod because an AS identity carries an empty auth_method;
//   - an identity with no tokenID carries no credential marker and is
//     CredentialUnknown (fail closed);
//   - a run-bound MCP token (subject mcp:run:<uuid>) is CredentialMCPRun;
//   - otherwise an api token, classified by AuthMethodCredentialKind.
//
// The allow-list decision itself is ForgeVerifiedCredential; this function
// only names the kind.
func CredentialKind(sessionID, tokenID, oauthAudience, subject, authMethod string) string {
	switch {
	case sessionID != "" && tokenID == "":
		return CredentialSession
	case oauthAudience != "":
		return CredentialOAuthAS
	case tokenID == "":
		return CredentialUnknown
	case strings.HasPrefix(subject, "mcp:run:"):
		return CredentialMCPRun
	}
	return AuthMethodCredentialKind(authMethod)
}

// AuthMethodCredentialKind classifies an api token by its recorded
// auth_method: "oauth" is the device flow, "static" an operator-minted token,
// "" unrecorded, and any other value unknown. CredentialKind uses it for a
// live api token; `fishhawkd approver-members` uses it for a historical
// approval_submitted row that recorded only auth_method, so both surfaces
// share one mapping.
func AuthMethodCredentialKind(authMethod string) string {
	switch authMethod {
	case CredentialDeviceFlow:
		return CredentialDeviceFlow
	case CredentialStatic:
		return CredentialStatic
	case "":
		return CredentialUnrecorded
	default:
		return CredentialUnknown
	}
}
