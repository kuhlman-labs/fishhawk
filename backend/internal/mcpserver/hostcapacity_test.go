package mcpserver

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/concurrency"
)

// The test binary never reads the developer's real FISHHAWK_HOST_CAPACITY:
// NewServer-driven tests reach the host-dispatch marker, and a value set in
// the shell would change every marker body or fail it closed. Config.internal
// still wires the processHostCapacity variable, which is what
// TestConfigInternal_WiresLazyProcessHostCapacity pins.
func init() {
	processHostCapacity = fixedHostCapacity(hostCapacityResolution{Capacity: defaultHostCapacity, Source: hostCapacitySourceDefault}, nil)
}

// fixedHostCapacity is a resolver answering res and err.
func fixedHostCapacity(res hostCapacityResolution, err error) func() (hostCapacityResolution, error) {
	return func() (hostCapacityResolution, error) { return res, err }
}

// envLookup is a lookupEnv seam answering FISHHAWK_HOST_CAPACITY=value when
// set is true, and unset otherwise.
func envLookup(value string, set bool) func(string) (string, bool) {
	return func(key string) (string, bool) {
		if key == hostCapacityEnv && set {
			return value, true
		}
		return "", false
	}
}

// TestResolveHostCapacity pins every env state: unset is the default (sent as
// nothing), an integer in 1..64 (trimmed, sign allowed) is source env, and
// every other value, including set-but-empty, is an error wrapping
// errInvalidHostCapacity that names the variable and the value.
func TestResolveHostCapacity(t *testing.T) {
	ok := []struct {
		name, raw string
		set       bool
		want      int
		source    string
	}{
		{"unset", "", false, 3, hostCapacitySourceDefault},
		{"two", "2", true, 2, hostCapacitySourceEnv},
		{"padded", " 2 ", true, 2, hostCapacitySourceEnv},
		{"plus sign", "+2", true, 2, hostCapacitySourceEnv},
		{"lower bound", "1", true, 1, hostCapacitySourceEnv},
		{"upper bound", "64", true, 64, hostCapacitySourceEnv},
	}
	for _, tc := range ok {
		t.Run(tc.name, func(t *testing.T) {
			res, err := resolveHostCapacity(envLookup(tc.raw, tc.set))
			if err != nil {
				t.Fatalf("resolveHostCapacity: %v", err)
			}
			if res.Capacity != tc.want || res.Source != tc.source {
				t.Errorf("resolution = %+v, want capacity %d source %s", res, tc.want, tc.source)
			}
			n, sent := res.declared()
			if wantSent := tc.source == hostCapacitySourceEnv; sent != wantSent || (sent && n != tc.want) {
				t.Errorf("declared() = %d, %v; want sent=%v", n, sent, wantSent)
			}
		})
	}
	for _, raw := range []string{"", "  ", "0", "-1", "65", "abc", "2.5", "0x2", "1e1"} {
		t.Run("invalid "+raw, func(t *testing.T) {
			res, err := resolveHostCapacity(envLookup(raw, true))
			if !errors.Is(err, errInvalidHostCapacity) {
				t.Fatalf("err = %v, want errInvalidHostCapacity", err)
			}
			if !strings.Contains(err.Error(), hostCapacityEnv) || !strings.Contains(err.Error(), `"`+raw+`"`) || !strings.Contains(err.Error(), "1..64") {
				t.Errorf("err = %q, want it to name %s, the value %q and the range 1..64", err, hostCapacityEnv, raw)
			}
			if res.Source != hostCapacitySourceInvalid {
				t.Errorf("source = %q, want invalid", res.Source)
			}
			if _, sent := res.declared(); sent {
				t.Error("an invalid value declares a capacity")
			}
		})
	}
}

// TestValidateHostCapacityEnv is the startup entry point: nil for unset and
// valid values, the resolver's error otherwise.
func TestValidateHostCapacityEnv(t *testing.T) {
	if err := ValidateHostCapacityEnv(envLookup("", false)); err != nil {
		t.Errorf("unset: %v", err)
	}
	if err := ValidateHostCapacityEnv(envLookup("4", true)); err != nil {
		t.Errorf("4: %v", err)
	}
	if err := ValidateHostCapacityEnv(envLookup("0", true)); !errors.Is(err, errInvalidHostCapacity) {
		t.Errorf("0: err = %v, want errInvalidHostCapacity", err)
	}
}

// TestResolveProcessHostCapacity runs the PRODUCTION read (#3969 carry-forward
// item 3): resolveProcessHostCapacity over the real process environment, which
// processHostCapacity wraps in sync.OnceValues. The test binary's init pins
// processHostCapacity, so this is the only test that reaches os.LookupEnv.
func TestResolveProcessHostCapacity(t *testing.T) {
	t.Run("set valid", func(t *testing.T) {
		t.Setenv(hostCapacityEnv, "5")
		res, err := resolveProcessHostCapacity()
		if err != nil || res.Capacity != 5 || res.Source != hostCapacitySourceEnv || res.Raw != "5" {
			t.Fatalf("resolveProcessHostCapacity() = %+v, %v; want capacity 5 from the env", res, err)
		}
	})
	t.Run("set invalid", func(t *testing.T) {
		t.Setenv(hostCapacityEnv, "0")
		res, err := resolveProcessHostCapacity()
		if !errors.Is(err, errInvalidHostCapacity) || res.Source != hostCapacitySourceInvalid {
			t.Fatalf("resolveProcessHostCapacity() = %+v, %v; want the invalid-capacity error", res, err)
		}
	})
	t.Run("unset", func(t *testing.T) {
		t.Setenv(hostCapacityEnv, "") // restores the caller's value on cleanup
		if err := os.Unsetenv(hostCapacityEnv); err != nil {
			t.Fatalf("unsetenv: %v", err)
		}
		res, err := resolveProcessHostCapacity()
		if err != nil || res.Capacity != defaultHostCapacity || res.Source != hostCapacitySourceDefault {
			t.Fatalf("resolveProcessHostCapacity() = %+v, %v; want the default %d", res, err, defaultHostCapacity)
		}
	})
}

// TestHostCapacityParity pins the mirrored constants to
// backend/internal/concurrency's.
func TestHostCapacityParity(t *testing.T) {
	if defaultHostCapacity != concurrency.DefaultHostCapacity {
		t.Errorf("defaultHostCapacity = %d, want concurrency.DefaultHostCapacity %d", defaultHostCapacity, concurrency.DefaultHostCapacity)
	}
	if maxHostCapacity != concurrency.MaxLimit {
		t.Errorf("maxHostCapacity = %d, want concurrency.MaxLimit %d", maxHostCapacity, concurrency.MaxLimit)
	}
	if capacityGroupName != concurrency.CapacityGroupName {
		t.Errorf("capacityGroupName = %q, want %q", capacityGroupName, concurrency.CapacityGroupName)
	}
	for _, label := range []string{"h1", ""} {
		if got, want := capacityGroupKeyFor(label), concurrency.CapacityGroupKey(label); got != want {
			t.Errorf("capacityGroupKeyFor(%q) = %q, want concurrency.CapacityGroupKey's %q", label, got, want)
		}
	}
}

// TestConfigInternal_WiresLazyProcessHostCapacity: the NewServer path wires
// processHostCapacity, and a config{} literal (nil resolver) sends no capacity.
func TestConfigInternal_WiresLazyProcessHostCapacity(t *testing.T) {
	c := Config{BackendURL: "http://x"}.internal()
	if c.hostCapacity == nil {
		t.Fatal("Config.internal().hostCapacity is nil, want the lazy processHostCapacity")
	}
	saved := processHostCapacity
	t.Cleanup(func() { processHostCapacity = saved })
	processHostCapacity = fixedHostCapacity(hostCapacityResolution{Capacity: 7, Source: hostCapacitySourceEnv}, nil)
	if res, _ := newAPIClient(Config{BackendURL: "http://x"}.internal()).hostCapacityResolution(); res.Capacity != 7 {
		t.Errorf("NewServer-path client capacity = %+v, want the process value 7", res)
	}
	bare := newAPIClient(config{backendURL: "http://x"})
	if res, err := bare.hostCapacityResolution(); err != nil || res.Source != hostCapacitySourceDefault || res.Capacity != defaultHostCapacity {
		t.Errorf("nil resolver = %+v, %v; want the default, sent as nothing", res, err)
	}
}

// TestNewServer_DoesNotResolveHostCapacity pins the laziness: building the
// tool registry (fishhawkd's /mcp route does so per request) never reads the
// variable.
func TestNewServer_DoesNotResolveHostCapacity(t *testing.T) {
	saved := processHostCapacity
	t.Cleanup(func() { processHostCapacity = saved })
	calls := 0
	processHostCapacity = func() (hostCapacityResolution, error) {
		calls++
		return hostCapacityResolution{Source: hostCapacitySourceDefault, Capacity: defaultHostCapacity}, nil
	}
	_ = NewServer(Config{BackendURL: "http://x", APIToken: "tok"})
	if calls != 0 {
		t.Errorf("NewServer resolved the host capacity %d times, want 0 (lazy)", calls)
	}
}

// TestHostDispatchStage_NilCapacityResolverIsOldClient: a client with no
// capacity resolver is byte-identical to a pre-#4241 client.
func TestHostDispatchStage_NilCapacityResolverIsOldClient(t *testing.T) {
	var gotBody string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_, _ = w.Write([]byte(`{"transitioned":true,"stage_state":"dispatched"}`))
	}))
	defer ts.Close()
	c := newAPIClient(config{backendURL: ts.URL, apiToken: "tok", hostLabel: fixedHostLabel("h1")})
	if _, err := c.HostDispatchStage(context.Background(), uuid.New(), uuid.New()); err != nil {
		t.Fatalf("HostDispatchStage: %v", err)
	}
	if gotBody != `{"host":"h1"}` {
		t.Errorf("body = %q, want the pre-#4241 {\"host\":\"h1\"}", gotBody)
	}
}
