package routing

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// env returns a getenv function over a literal map, so no test mutates the
// process environment.
func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func validEnv() map[string]string {
	return map[string]string{
		EnvRegions:       "us=https://us.cell.example,eu=https://eu.cell.example",
		EnvHandoffSecret: "s3cret",
		EnvAdminToken:    "operator-token",
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	cfg, err := LoadConfig(env(validEnv()))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got, want := len(cfg.Regions), 2; got != want {
		t.Fatalf("regions = %d, want %d", got, want)
	}
	if got, ok := cfg.CellURL("eu"); !ok || got != "https://eu.cell.example" {
		t.Fatalf("CellURL(eu) = %q, %v", got, ok)
	}
	if _, ok := cfg.CellURL("ap"); ok {
		t.Fatal("CellURL(ap) resolved a region that is not configured")
	}
	if cfg.HandoffTTL != DefaultHandoffTTL {
		t.Fatalf("HandoffTTL = %s, want %s", cfg.HandoffTTL, DefaultHandoffTTL)
	}
	// Condition (2): the OAuth login/callback pair is deliberately NOT
	// routed by default — it carries no account identity.
	if got, want := cfg.RoutedPaths, []string{DefaultRoutedPath}; len(got) != 1 || got[0] != want[0] {
		t.Fatalf("RoutedPaths = %v, want %v", got, want)
	}
}

func TestLoadConfigOverrides(t *testing.T) {
	e := validEnv()
	// An explicit list is honoured, but only over surfaces the cell actually
	// verifies — see TestLoadConfigFailsClosed's unsupported-path case.
	e[EnvRoutedPaths] = " /v0/onboarding/start "
	e[EnvHandoffTTL] = "90s"

	cfg, err := LoadConfig(env(e))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.HandoffTTL != 90*time.Second {
		t.Fatalf("HandoffTTL = %s, want 90s", cfg.HandoffTTL)
	}
	if len(cfg.RoutedPaths) != 1 || cfg.RoutedPaths[0] != DefaultRoutedPath {
		t.Fatalf("RoutedPaths = %v", cfg.RoutedPaths)
	}
}

// TestSupportedRoutedPathsMatchCellMiddleware is the lockstep guard: the
// directory may only route a path the cell mounts its handoff-verifying
// middleware on. The cell's constant is server.RoutedOnboardingPath; the
// directory module cannot import backend (the dependency is one-way), so the
// literal is asserted here and named in both files' comments.
func TestSupportedRoutedPathsMatchCellMiddleware(t *testing.T) {
	want := map[string]bool{"/v0/onboarding/start": true}
	if len(supportedRoutedPaths) != len(want) {
		t.Fatalf("supportedRoutedPaths = %v, want %v — a new entry needs a matching cell mount", supportedRoutedPaths, want)
	}
	for p := range want {
		if !supportedRoutedPaths[p] {
			t.Errorf("supportedRoutedPaths is missing %q", p)
		}
	}
}

// Every fail-closed startup branch gets its own case: a defect must abort
// startup, never degrade to a warning that leaves a half-configured router
// listening.
func TestLoadConfigFailsClosed(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(map[string]string)
		wantSub string
	}{
		{
			name:    "unparsable region map",
			mutate:  func(e map[string]string) { e[EnvRegions] = "us-https://us.cell.example" },
			wantSub: "is not region=url",
		},
		{
			name:    "region named twice",
			mutate:  func(e map[string]string) { e[EnvRegions] = "us=https://a.example,us=https://b.example" },
			wantSub: "twice",
		},
		{
			name:    "empty region set",
			mutate:  func(e map[string]string) { e[EnvRegions] = "" },
			wantSub: EnvRegions + " is required",
		},
		{
			name:    "non-absolute cell URL",
			mutate:  func(e map[string]string) { e[EnvRegions] = "us=/cell" },
			wantSub: "must be absolute http(s)",
		},
		{
			name:    "cell URL without host",
			mutate:  func(e map[string]string) { e[EnvRegions] = "us=https://" },
			wantSub: "has no host",
		},
		{
			name:    "cell URL carrying a query",
			mutate:  func(e map[string]string) { e[EnvRegions] = "us=https://us.cell.example?a=1" },
			wantSub: "must not carry a query",
		},
		{
			name:    "empty handoff secret",
			mutate:  func(e map[string]string) { e[EnvHandoffSecret] = "" },
			wantSub: EnvHandoffSecret + " is required",
		},
		{
			name:    "unparsable handoff TTL",
			mutate:  func(e map[string]string) { e[EnvHandoffTTL] = "soon" },
			wantSub: "is not a duration",
		},
		{
			name:    "non-positive handoff TTL",
			mutate:  func(e map[string]string) { e[EnvHandoffTTL] = "-1s" },
			wantSub: "must be positive",
		},
		{
			name:    "relative routed path",
			mutate:  func(e map[string]string) { e[EnvRoutedPaths] = "v0/onboarding/start" },
			wantSub: "must be absolute",
		},
		{
			name: "duplicate routed path",
			mutate: func(e map[string]string) {
				e[EnvRoutedPaths] = DefaultRoutedPath + "," + DefaultRoutedPath
			},
			wantSub: "listed twice",
		},
		{
			// A path the cell does not mount withRegionPin on would receive a
			// signed redirect that verifies nothing and pins nothing.
			name:    "unsupported routed path",
			mutate:  func(e map[string]string) { e[EnvRoutedPaths] = "/v0/onboarding/start,/v0/onboarding/resume" },
			wantSub: "is not a cell surface that verifies a handoff",
		},
		{
			name:    "unsupported routed path alone",
			mutate:  func(e map[string]string) { e[EnvRoutedPaths] = "/v0/auth/github/callback" },
			wantSub: "is not a cell surface that verifies a handoff",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := validEnv()
			tc.mutate(e)
			_, err := LoadConfig(env(e))
			if err == nil {
				t.Fatal("LoadConfig succeeded, want a startup error")
			}
			if !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("error %v does not wrap ErrInvalidConfig", err)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("error %q does not mention %q", err, tc.wantSub)
			}
		})
	}
}

// An unset operator credential is deliberately NOT a startup error: it is a
// runtime 503 on both surfaces (see TestRoutedRefusesWhenAdminTokenUnset
// and TestAssignRefusesWhenAdminTokenUnset). Unset must mean closed, and a
// startup abort would push an operator toward unsetting it to "turn auth
// off".
func TestLoadConfigAllowsUnsetAdminToken(t *testing.T) {
	e := validEnv()
	e[EnvAdminToken] = ""
	cfg, err := LoadConfig(env(e))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.AdminToken != "" {
		t.Fatalf("AdminToken = %q, want empty", cfg.AdminToken)
	}
}
