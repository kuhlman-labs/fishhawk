package identity

import "testing"

// TestForgeVerifiedCredential pins the credential-kind allow-list (#4633).
// CF2 vehicle: with the default branch returning true, every false row flips.
func TestForgeVerifiedCredential(t *testing.T) {
	for _, tc := range []struct {
		kind string
		want bool
	}{
		{CredentialSession, true},
		{CredentialDeviceFlow, true},
		{CredentialOAuthAS, true},
		{CredentialStatic, false},
		{CredentialUnrecorded, false},
		{CredentialMCPRun, false},
		{CredentialUnknown, false},
		{"", false},
		{"OAUTH", false},
		{"bogus", false},
	} {
		if got := ForgeVerifiedCredential(tc.kind); got != tc.want {
			t.Errorf("ForgeVerifiedCredential(%q) = %v, want %v", tc.kind, got, tc.want)
		}
	}
	for _, k := range ForgeVerifiedKinds {
		if !ForgeVerifiedCredential(k) {
			t.Errorf("ForgeVerifiedKinds carries %q, which ForgeVerifiedCredential refuses", k)
		}
	}
	if len(ForgeVerifiedKinds) != 3 {
		t.Errorf("ForgeVerifiedKinds = %v, want exactly the three verified kinds", ForgeVerifiedKinds)
	}
}

// TestCredentialKind pins one row per classifier branch and the branch order
// (#4633, ADR-123 D1). CF vehicle: mapping an empty or a "static" auth_method
// to the device flow flips the unrecorded / static rows; moving the oauth_as
// branch below the auth_method switch flips the audience-wins row.
func TestCredentialKind(t *testing.T) {
	const run = "mcp:run:0b8e7f8a-0000-0000-0000-000000000001"
	for _, tc := range []struct {
		name                                                   string
		sessionID, tokenID, oauthAudience, subject, authMethod string
		want                                                   string
	}{
		{"cookie session", "s1", "", "", "github:alice", "", CredentialSession},
		{"AS token", "", "t1", "https://fh.example/mcp", "github:alice", "", CredentialOAuthAS},
		{"device-flow token", "", "t1", "", "github:alice", "oauth", CredentialDeviceFlow},
		{"static token", "", "t1", "", "github:alice", "static", CredentialStatic},
		{"empty auth_method", "", "t1", "", "github:alice", "", CredentialUnrecorded},
		{"unknown auth_method", "", "t1", "", "github:alice", "ldap", CredentialUnknown},
		{"differently-cased auth_method", "", "t1", "", "github:alice", "OAUTH", CredentialUnknown},
		{"mcp:run token", "", "t1", "", run, "oauth", CredentialMCPRun},
		{"no credential marker", "", "", "", "github:alice", "", CredentialUnknown},
		{"no credential marker with an auth_method", "", "", "", "github:alice", "oauth", CredentialUnknown},
		{"AS audience wins over static auth_method", "", "t1", "https://fh.example/mcp", "github:alice", "static", CredentialOAuthAS},
		{"AS audience wins over an mcp:run subject", "", "t1", "https://fh.example/mcp", run, "", CredentialOAuthAS},
		{"session with a token id is not a session", "s1", "t1", "", "github:alice", "static", CredentialStatic},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := CredentialKind(tc.sessionID, tc.tokenID, tc.oauthAudience, tc.subject, tc.authMethod); got != tc.want {
				t.Errorf("CredentialKind(%q, %q, %q, %q, %q) = %q, want %q",
					tc.sessionID, tc.tokenID, tc.oauthAudience, tc.subject, tc.authMethod, got, tc.want)
			}
		})
	}
}

// TestAuthMethodCredentialKind pins the auth_method mapping both
// CredentialKind and `fishhawkd approver-members` share.
func TestAuthMethodCredentialKind(t *testing.T) {
	for _, tc := range []struct{ authMethod, want string }{
		{"oauth", CredentialDeviceFlow},
		{"static", CredentialStatic},
		{"", CredentialUnrecorded},
		{"ldap", CredentialUnknown},
		{"Static", CredentialUnknown},
	} {
		if got := AuthMethodCredentialKind(tc.authMethod); got != tc.want {
			t.Errorf("AuthMethodCredentialKind(%q) = %q, want %q", tc.authMethod, got, tc.want)
		}
	}
}
