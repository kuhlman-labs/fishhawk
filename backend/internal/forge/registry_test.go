package forge_test

import (
	"errors"
	"reflect"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
)

// fakeForge is a minimal forge.Forge for registry tests. Only Name is
// exercised here; the rest of the surface is embedded from a nil pointer
// so the type satisfies the interface without hand-writing 20 stubs (no
// method is called).
type fakeForge struct {
	forge.Forge
	name string
}

func (f fakeForge) Name() string { return f.name }

// TestRegisterAndGet round-trips a forge through the registry.
func TestRegisterAndGet(t *testing.T) {
	f := fakeForge{name: "test-register-get"}
	forge.Register(f)

	got, err := forge.Get("test-register-get")
	if err != nil {
		t.Fatalf("Get after Register: %v", err)
	}
	if got.Name() != "test-register-get" {
		t.Errorf("Get returned forge %q, want %q", got.Name(), "test-register-get")
	}
}

// TestRegisterReplaces confirms a second Register under the same id wins,
// matching the workmgmt registry contract.
func TestRegisterReplaces(t *testing.T) {
	forge.Register(fakeForge{name: "test-replace"})
	forge.Register(fakeForge{name: "test-replace"})
	got, err := forge.Get("test-replace")
	if err != nil {
		t.Fatalf("Get after double Register: %v", err)
	}
	if got.Name() != "test-replace" {
		t.Errorf("Get returned %q, want %q", got.Name(), "test-replace")
	}
}

// TestGetUnknownForgeFailsClosed is the fail-closed guard: Get on an
// unregistered id returns an *UnknownForgeError (never a nil forge for a
// caller to dispatch against), and the error names the unknown id and
// the sorted registered set.
func TestGetUnknownForgeFailsClosed(t *testing.T) {
	forge.Register(fakeForge{name: "aaa-known"})
	forge.Register(fakeForge{name: "zzz-known"})

	got, err := forge.Get("nope-unregistered")
	if got != nil {
		t.Fatalf("Get(unregistered) returned non-nil forge %v; must fail closed", got)
	}
	var unknown *forge.UnknownForgeError
	if !errors.As(err, &unknown) {
		t.Fatalf("Get(unregistered) error = %v (%T), want *UnknownForgeError", err, err)
	}
	if unknown.ID != "nope-unregistered" {
		t.Errorf("UnknownForgeError.ID = %q, want %q", unknown.ID, "nope-unregistered")
	}
	msg := err.Error()
	for _, want := range []string{"nope-unregistered", "aaa-known", "zzz-known"} {
		if !contains(msg, want) {
			t.Errorf("error message %q does not name %q", msg, want)
		}
	}
}

// TestUnknownForgeErrorNoForgesRegistered exercises the empty-registry
// branch of UnknownForgeError.Error, which prints a distinct message.
// The global registry is never empty once other tests run, so the
// message is asserted directly on a constructed value.
func TestUnknownForgeErrorNoForgesRegistered(t *testing.T) {
	e := &forge.UnknownForgeError{ID: "x"}
	msg := e.Error()
	if !contains(msg, "no forges registered") {
		t.Errorf("empty-registry error = %q, want it to mention %q", msg, "no forges registered")
	}
	if !contains(msg, `"x"`) {
		t.Errorf("empty-registry error = %q, want it to name the id", msg)
	}
}

// TestRegisteredSorted asserts Registered returns ids in sorted order —
// the property startup logging and the unknown-forge error depend on for
// stable output.
func TestRegisteredSorted(t *testing.T) {
	forge.Register(fakeForge{name: "sort-ccc"})
	forge.Register(fakeForge{name: "sort-aaa"})
	forge.Register(fakeForge{name: "sort-bbb"})

	ids := forge.Registered()
	// Filter to this test's ids so concurrently-registered fakes from
	// other tests don't perturb the ordering assertion.
	var mine []string
	for _, id := range ids {
		if len(id) >= 5 && id[:5] == "sort-" {
			mine = append(mine, id)
		}
	}
	want := []string{"sort-aaa", "sort-bbb", "sort-ccc"}
	if !reflect.DeepEqual(mine, want) {
		t.Errorf("Registered (sort- subset) = %v, want %v", mine, want)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
