package modeloracle

import (
	"context"
	"testing"
)

// TestNoData_UniversalFailOpen asserts the shipped production oracle reports no
// snapshot (ok=false, fresh=false) for an arbitrary set of providers — the
// universal fail-open contract the validation layer relies on so no hard
// rejection can fire in production today.
func TestNoData_UniversalFailOpen(t *testing.T) {
	o := NewNoData()
	for _, provider := range []string{"anthropic", "codex", "claudecode", "", "made-up-provider"} {
		models, fresh, ok := o.Snapshot(context.Background(), provider)
		if ok {
			t.Errorf("provider %q: ok = true, want false (no snapshot)", provider)
		}
		if fresh {
			t.Errorf("provider %q: fresh = true, want false", provider)
		}
		if models != nil {
			t.Errorf("provider %q: models = %v, want nil", provider, models)
		}
	}
}

// TestStatic_FreshMembership asserts a fresh Static fixture reports present /
// absent membership with ok=true for a configured provider and honors the Fresh
// flag.
func TestStatic_FreshMembership(t *testing.T) {
	o := Static{
		Models: map[string][]string{"anthropic": {"claude-opus-4-8", "claude-sonnet-4-6"}},
		Fresh:  true,
	}
	models, fresh, ok := o.Snapshot(context.Background(), "anthropic")
	if !ok {
		t.Fatal("ok = false, want true for a configured provider")
	}
	if !fresh {
		t.Error("fresh = false, want true")
	}
	if !contains(models, "claude-opus-4-8") {
		t.Errorf("models = %v, want it to contain claude-opus-4-8", models)
	}
	if contains(models, "claude-typo-9") {
		t.Errorf("models = %v, want it to NOT contain claude-typo-9", models)
	}
}

// TestStatic_StaleHonorsFreshFlag asserts a stale Static fixture (Fresh=false)
// returns fresh=false even for a populated provider, so the caller fails open.
func TestStatic_StaleHonorsFreshFlag(t *testing.T) {
	o := Static{
		Models: map[string][]string{"anthropic": {"claude-opus-4-8"}},
		Fresh:  false,
	}
	_, fresh, ok := o.Snapshot(context.Background(), "anthropic")
	if !ok {
		t.Fatal("ok = false, want true for a configured provider")
	}
	if fresh {
		t.Error("fresh = true, want false (stale fixture)")
	}
}

// TestStatic_UnknownProvider asserts a provider absent from Models has no
// snapshot (ok=false), like NoData for that provider.
func TestStatic_UnknownProvider(t *testing.T) {
	o := Static{Models: map[string][]string{"anthropic": {"claude-opus-4-8"}}, Fresh: true}
	models, _, ok := o.Snapshot(context.Background(), "codex")
	if ok {
		t.Error("ok = true, want false for an unconfigured provider")
	}
	if models != nil {
		t.Errorf("models = %v, want nil", models)
	}
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
