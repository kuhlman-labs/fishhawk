package server

import (
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/identity"
)

// TestApprovalCredentialKind pins the adapter's field wiring onto
// identity.CredentialKind (#4633): one row per Identity field the classifier
// reads, so a swapped or dropped argument flips a row. The per-branch table
// lives in identity's TestCredentialKind.
func TestApprovalCredentialKind(t *testing.T) {
	for _, tc := range []struct {
		name string
		id   Identity
		want string
	}{
		{"SessionID", Identity{Subject: "github:alice", UserID: "u1", SessionID: "s1"}, identity.CredentialSession},
		{"OAuthAudience", Identity{Subject: "github:alice", TokenID: "t1", AuthMethod: "static", OAuthAudience: "https://fh.example/mcp"}, identity.CredentialOAuthAS},
		{"TokenID absent", Identity{Subject: "github:alice"}, identity.CredentialUnknown},
		{"Subject", Identity{Subject: "mcp:run:0b8e7f8a-0000-0000-0000-000000000001", TokenID: "t1", AuthMethod: "oauth"}, identity.CredentialMCPRun},
		{"AuthMethod static", Identity{Subject: "github:alice", TokenID: "t1", AuthMethod: "static"}, identity.CredentialStatic},
		{"AuthMethod oauth", Identity{Subject: "github:alice", TokenID: "t1", AuthMethod: "oauth"}, identity.CredentialDeviceFlow},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := approvalCredentialKind(tc.id); got != tc.want {
				t.Errorf("approvalCredentialKind(%+v) = %q, want %q", tc.id, got, tc.want)
			}
		})
	}
}
