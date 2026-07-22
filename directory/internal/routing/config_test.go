package routing_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/directory/internal/routing"
)

// env builds a getenv func over a map so no test mutates process state.
func env(kv map[string]string) func(string) string {
	return func(k string) string { return kv[k] }
}

func validEnv() map[string]string {
	return map[string]string{
		routing.EnvSupportedRegions: "us,eu,au",
		routing.EnvCellBaseURLs:     "us=https://us.app.fishhawk.test,eu=https://eu.app.fishhawk.test,au=https://au.app.fishhawk.test",
		routing.EnvHandoffSecret:    "shared-secret",
	}
}

func TestLoadConfigParsesCommaSplitEnv(t *testing.T) {
	cfg, err := routing.LoadConfig(env(validEnv()))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got, want := strings.Join(cfg.SupportedRegions, ","), "us,eu,au"; got != want {
		t.Fatalf("SupportedRegions: got %q want %q", got, want)
	}
	if got, want := cfg.CellBaseURLs["eu"], "https://eu.app.fishhawk.test"; got != want {
		t.Fatalf("eu cell: got %q want %q", got, want)
	}
	if cfg.HandoffTTL != routing.DefaultHandoffTTL {
		t.Fatalf("HandoffTTL: got %s want %s", cfg.HandoffTTL, routing.DefaultHandoffTTL)
	}
}

func TestLoadConfigToleratesWhitespaceAndCaseAndTrailingSlash(t *testing.T) {
	kv := validEnv()
	kv[routing.EnvSupportedRegions] = " US , eu ,, AU "
	kv[routing.EnvCellBaseURLs] = " US = https://us.app.fishhawk.test/ , eu=https://eu.app.fishhawk.test , au=https://au.app.fishhawk.test "
	cfg, err := routing.LoadConfig(env(kv))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got, want := cfg.CellBaseURLs["us"], "https://us.app.fishhawk.test"; got != want {
		t.Fatalf("us cell: got %q want %q (trailing slash should be trimmed)", got, want)
	}
	if !cfg.Supports("au") {
		t.Fatal("Supports(au) = false")
	}
}

func TestLoadConfigCustomTTL(t *testing.T) {
	kv := validEnv()
	kv[routing.EnvHandoffTTL] = "45s"
	cfg, err := routing.LoadConfig(env(kv))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.HandoffTTL != 45*time.Second {
		t.Fatalf("HandoffTTL: got %s want 45s", cfg.HandoffTTL)
	}
}

// One case per fail-closed branch in LoadConfig.
func TestLoadConfigFailsClosed(t *testing.T) {
	cases := map[string]struct {
		mutate  func(map[string]string)
		wantSub string
	}{
		"no supported regions": {
			func(kv map[string]string) { kv[routing.EnvSupportedRegions] = "" },
			routing.EnvSupportedRegions,
		},
		"blank-only supported regions": {
			func(kv map[string]string) { kv[routing.EnvSupportedRegions] = " , , " },
			routing.EnvSupportedRegions,
		},
		"no cell base urls": {
			func(kv map[string]string) { kv[routing.EnvCellBaseURLs] = "" },
			routing.EnvCellBaseURLs,
		},
		"malformed pair": {
			func(kv map[string]string) { kv[routing.EnvCellBaseURLs] = "https://us.app.fishhawk.test" },
			"is not a region=url pair",
		},
		"pair with empty url": {
			func(kv map[string]string) { kv[routing.EnvCellBaseURLs] = "us=" },
			"is not a region=url pair",
		},
		"url for unsupported region": {
			func(kv map[string]string) {
				kv[routing.EnvSupportedRegions] = "us"
				kv[routing.EnvCellBaseURLs] = "us=https://us.app.fishhawk.test,jp=https://jp.app.fishhawk.test"
			},
			"not in " + routing.EnvSupportedRegions,
		},
		"supported region with no cell": {
			func(kv map[string]string) {
				kv[routing.EnvCellBaseURLs] = "us=https://us.app.fishhawk.test,eu=https://eu.app.fishhawk.test"
			},
			"have no cell base URL",
		},
		"relative base url": {
			func(kv map[string]string) {
				kv[routing.EnvSupportedRegions] = "us"
				kv[routing.EnvCellBaseURLs] = "us=/cell"
			},
			"absolute http(s) URL",
		},
		"non-http scheme": {
			func(kv map[string]string) {
				kv[routing.EnvSupportedRegions] = "us"
				kv[routing.EnvCellBaseURLs] = "us=ftp://us.app.fishhawk.test"
			},
			"absolute http(s) URL",
		},
		"no host": {
			func(kv map[string]string) {
				kv[routing.EnvSupportedRegions] = "us"
				kv[routing.EnvCellBaseURLs] = "us=https:///cell"
			},
			"has no host",
		},
		"no handoff secret": {
			func(kv map[string]string) { kv[routing.EnvHandoffSecret] = "  " },
			routing.EnvHandoffSecret,
		},
		"unparseable ttl": {
			func(kv map[string]string) { kv[routing.EnvHandoffTTL] = "soon" },
			routing.EnvHandoffTTL,
		},
		"non-positive ttl": {
			func(kv map[string]string) { kv[routing.EnvHandoffTTL] = "0s" },
			"must be positive",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			kv := validEnv()
			tc.mutate(kv)
			cfg, err := routing.LoadConfig(env(kv))
			if err == nil {
				t.Fatalf("expected failure, got config %+v", cfg)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("error %q does not mention %q", err, tc.wantSub)
			}
		})
	}
}

func TestResolveFailsClosedForUnconfiguredRegion(t *testing.T) {
	cfg, err := routing.LoadConfig(env(validEnv()))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	base, err := cfg.Resolve("jp")
	if !errors.Is(err, routing.ErrNoCellForRegion) {
		t.Fatalf("got (%q, %v) want ErrNoCellForRegion", base, err)
	}
	if base != "" {
		// The fall-through hazard: never hand back some other region's cell.
		t.Fatalf("Resolve returned a base URL %q for an unconfigured region", base)
	}
}

func TestResolveIsCaseInsensitive(t *testing.T) {
	cfg, err := routing.LoadConfig(env(validEnv()))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	base, err := cfg.Resolve(" EU ")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if base != "https://eu.app.fishhawk.test" {
		t.Fatalf("got %q", base)
	}
}

func TestSupportsRejectsUnknownRegion(t *testing.T) {
	cfg, err := routing.LoadConfig(env(validEnv()))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Supports("jp") {
		t.Fatal("Supports(jp) = true")
	}
}
