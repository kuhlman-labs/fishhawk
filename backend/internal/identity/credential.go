package identity

// Credential kinds (#4633): the forge-neutral vocabulary naming HOW the
// credential that authenticated a request was minted. SubjectListed is a
// SUBJECT-SHAPE rule only; the approvals.members credential leg pairs it with
// ForgeVerifiedCredential so a listed forge subject counts only when the
// presenting credential was itself minted through a forge-verified sign-in.
// The server's approval gate and `fishhawkd approver-members` both classify
// into these constants and consult the one allow-list below, so the gate and
// the impact inventory cannot disagree. The rule is pending ratification as
// ADR-123 (#4750).
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
