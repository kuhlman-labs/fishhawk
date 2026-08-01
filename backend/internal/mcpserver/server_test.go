package mcpserver

import "testing"

// envFunc returns a func(string) string backed by a literal map. Relocated
// into the package with the moved tests (E66.7 / #2408): it previously lived
// in the command's main_test.go, which stayed with the binary, but
// onboarding_test.go (moved here) still constructs a runResolver with it. We
// don't use os.Setenv in tests because env state would leak across parallel
// runs; the tool constructors take the getter as a parameter for exactly this
// reason.
func envFunc(env map[string]string) func(string) string {
	return func(k string) string {
		return env[k]
	}
}

func TestBuildServer_HandshakeReady(t *testing.T) {
	// The server should construct cleanly with a fully-registered
	// registry. The protocol-level handshake itself is tested by the
	// SDK; this test just locks in that buildServer doesn't panic and
	// returns a non-nil server we can hand to a transport later. Relocated
	// from the command's main_test.go with buildServer (E66.7 / #2408).
	srv := buildServer(config{
		backendURL: "http://localhost:8080",
		apiToken:   "tok",
	})
	if srv == nil {
		t.Fatal("buildServer returned nil")
	}
}

func TestHandshakeVersion(t *testing.T) {
	// Unstamped builds (GitSHA "unknown") advertise the bare base so the
	// handshake string is unchanged from pre-stamping behavior; stamped
	// builds append "+<sha>" including any -dirty suffix. Relocated from
	// the command's main_test.go with handshakeVersion (E66.7 / #2408).
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

// TestNewServer_ReturnsRegisteredServer asserts the exported entry point
// constructs a non-nil server — the coverage gap left when the command's
// newServer closure collapsed into a call to mcpserver.NewServer.
func TestNewServer_ReturnsRegisteredServer(t *testing.T) {
	srv := NewServer(Config{BackendURL: "http://localhost:8080", APIToken: "tok"})
	if srv == nil {
		t.Fatal("NewServer returned nil")
	}
}

// TestConfigInternal_PlumbsTokenToAPIClient pins the Config -> config ->
// apiClient bearer path: the exported Config's APIToken must reach the
// apiClient's token unchanged through the internal() adapter. This is the
// assertion relocated out of the command's main_test.go (which can no
// longer see newAPIClient after the extraction) and is what keeps the
// two-similarly-named-types trade (unexported config alongside exported
// Config) honest.
func TestConfigInternal_PlumbsTokenToAPIClient(t *testing.T) {
	cfg := Config{BackendURL: "http://localhost:8080", APIToken: "fhk_bearer"}
	client := newAPIClient(cfg.internal())
	if client.token != "fhk_bearer" {
		t.Errorf("apiClient bearer = %q, want the Config token fhk_bearer", client.token)
	}
	if client.baseURL != "http://localhost:8080" {
		t.Errorf("apiClient baseURL = %q, want http://localhost:8080", client.baseURL)
	}
}
