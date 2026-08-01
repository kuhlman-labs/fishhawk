package mcpserver

import "testing"

// TestNewServer_HandshakeReady is the moved handshake-readiness guard
// (was TestBuildServer_HandshakeReady in the cmd package): NewServer must
// construct cleanly through the exported seam and return a non-nil server
// the transport can run. The protocol-level handshake itself is tested by
// the SDK; this locks in that the extracted constructor does not panic.
func TestNewServer_HandshakeReady(t *testing.T) {
	srv := NewServer(Config{
		BackendURL: "http://localhost:8080",
		APIToken:   "tok",
	})
	if srv == nil {
		t.Fatal("NewServer returned nil")
	}
}

// TestBuildServer_HandshakeReady pins the unexported shell constructor
// directly: buildServer must return a non-nil empty server every test can
// start from, independent of tool registration.
func TestBuildServer_HandshakeReady(t *testing.T) {
	srv := buildServer(Config{
		BackendURL: "http://localhost:8080",
		APIToken:   "tok",
	})
	if srv == nil {
		t.Fatal("buildServer returned nil")
	}
}

// TestHandshakeVersion is the moved handshake-version table: unstamped
// builds (GitSHA "unknown" or empty) advertise the bare base so the
// handshake string is unchanged from pre-stamping behavior; stamped builds
// append "+<sha>" including any -dirty suffix.
func TestHandshakeVersion(t *testing.T) {
	for _, tc := range []struct {
		sha  string
		want string
	}{
		{"unknown", serverVersion},
		{"", serverVersion},
		{"abc1234", serverVersion + "+abc1234"},
		{"abc1234-dirty", serverVersion + "+abc1234-dirty"},
	} {
		if got := handshakeVersion(tc.sha); got != tc.want {
			t.Errorf("handshakeVersion(%q) = %q, want %q", tc.sha, got, tc.want)
		}
	}
}
