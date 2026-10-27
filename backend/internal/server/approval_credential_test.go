package server

import (
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/identity"
)

// TestApprovalCredentialKind pins one row per classifier branch (#4633). CF3 /
// CF4 vehicle: mapping an empty or a "static" auth_method to the device flow
// flips the unrecorded / static rows.
func TestApprovalCredentialKind(t *testing.T) {
	for _, tc := range []struct {
		name string
		id   Identity
		want string
	}{
		{"cookie session", Identity{Subject: "github:alice", UserID: "u1", SessionID: "s1"}, identity.CredentialSession},
		{"AS token", Identity{Subject: "github:alice", TokenID: "t1", OAuthAudience: "https://fh.example/mcp"}, identity.CredentialOAuthAS},
		{"device-flow token", Identity{Subject: "github:alice", TokenID: "t1", AuthMethod: "oauth"}, identity.CredentialDeviceFlow},
		{"static token", Identity{Subject: "github:alice", TokenID: "t1", AuthMethod: "static"}, identity.CredentialStatic},
		{"empty auth_method", Identity{Subject: "github:alice", TokenID: "t1"}, identity.CredentialUnrecorded},
		{"unknown auth_method", Identity{Subject: "github:alice", TokenID: "t1", AuthMethod: "ldap"}, identity.CredentialUnknown},
		{"mcp:run token", Identity{Subject: "mcp:run:0b8e7f8a-0000-0000-0000-000000000001", TokenID: "t1", AuthMethod: "oauth"}, identity.CredentialMCPRun},
		{"no credential marker", Identity{Subject: "github:alice"}, identity.CredentialUnknown},
		{"AS audience wins over static auth_method", Identity{Subject: "github:alice", TokenID: "t1", AuthMethod: "static", OAuthAudience: "https://fh.example/mcp"}, identity.CredentialOAuthAS},
		{"session with a token id is not a session", Identity{Subject: "github:alice", SessionID: "s1", TokenID: "t1", AuthMethod: "static"}, identity.CredentialStatic},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := approvalCredentialKind(tc.id); got != tc.want {
				t.Errorf("approvalCredentialKind(%+v) = %q, want %q", tc.id, got, tc.want)
			}
		})
	}
}
