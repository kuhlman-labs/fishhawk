package server

import (
	"strings"

	"github.com/kuhlman-labs/fishhawk/backend/internal/identity"
)

// approvalCredentialKind classifies the credential that authenticated the
// request into the identity.Credential* vocabulary (#4633), for the
// approvals.members credential leg in checkApprovalPredicates. It reads only
// the fields bearerAuth and the cookie path already set:
//
//   - a cookie session (SessionID set, no TokenID) is CredentialSession;
//   - an AS-issued `fho_` token (OAuthAudience set, which bearerAuth does only
//     after audience validation) is CredentialOAuthAS, whatever AuthMethod says;
//   - an identity with no TokenID carries no credential marker and is
//     CredentialUnknown (fail closed);
//   - a run-bound MCP token (subject mcp:run:<uuid>) is CredentialMCPRun;
//   - otherwise an api token, classified by its auth_method: "oauth" is the
//     device flow, "static" an operator-minted token, "" unrecorded, and any
//     other value unknown.
//
// The allow-list decision itself is identity.ForgeVerifiedCredential; this
// function only names the kind.
func approvalCredentialKind(id Identity) string {
	switch {
	case id.SessionID != "" && id.TokenID == "":
		return identity.CredentialSession
	case id.OAuthAudience != "":
		return identity.CredentialOAuthAS
	case id.TokenID == "":
		return identity.CredentialUnknown
	case strings.HasPrefix(id.Subject, "mcp:run:"):
		return identity.CredentialMCPRun
	}
	switch id.AuthMethod {
	case identity.CredentialDeviceFlow:
		return identity.CredentialDeviceFlow
	case "static":
		return identity.CredentialStatic
	case "":
		return identity.CredentialUnrecorded
	default:
		return identity.CredentialUnknown
	}
}
