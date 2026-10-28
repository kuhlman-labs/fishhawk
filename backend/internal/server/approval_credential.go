package server

import "github.com/kuhlman-labs/fishhawk/backend/internal/identity"

// approvalCredentialKind adapts the request Identity to
// identity.CredentialKind (#4633, ADR-123 D1), for the approvals.members
// credential leg in checkApprovalPredicates. It passes only the fields
// bearerAuth and the cookie path already set; the classification and its
// order live in identity.
func approvalCredentialKind(id Identity) string {
	return identity.CredentialKind(id.SessionID, id.TokenID, id.OAuthAudience, id.Subject, id.AuthMethod)
}
