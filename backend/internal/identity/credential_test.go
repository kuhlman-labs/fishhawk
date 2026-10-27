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
