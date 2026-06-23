package agent

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

func TestMakePayload_RoundTrips(t *testing.T) {
	p := MakePayload(map[string]any{"k": "v", "n": 7})
	var got map[string]any
	if err := json.Unmarshal(p, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["k"] != "v" {
		t.Errorf(`got["k"] = %v, want "v"`, got["k"])
	}
	// JSON numbers decode to float64; check via formatted equal.
	if n, ok := got["n"].(float64); !ok || n != 7 {
		t.Errorf(`got["n"] = %v, want 7`, got["n"])
	}
}

func TestMakePayload_PanicsOnUnmarshalable(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on unmarshalable input")
		}
	}()
	// Channels can't be marshaled — any panic is fine.
	MakePayload(make(chan int))
}

func TestAppendEnvOverride(t *testing.T) {
	t.Run("removes_conflicting_entry_and_appends_sole_match", func(t *testing.T) {
		env := []string{"PATH=/bin", "OPENAI_API_KEY=inherited-wrong", "HOME=/root"}
		got := AppendEnvOverride(env, "OPENAI_API_KEY", "configured-right")
		want := []string{"PATH=/bin", "HOME=/root", "OPENAI_API_KEY=configured-right"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %q, want %q", got, want)
		}
		// Exactly one entry for the key — a subprocess reads the first
		// match, so a lingering duplicate would re-shadow the override.
		var n int
		for _, kv := range got {
			if kv == "OPENAI_API_KEY=configured-right" || kv == "OPENAI_API_KEY=inherited-wrong" {
				n++
			}
		}
		if n != 1 {
			t.Errorf("OPENAI_API_KEY entries = %d, want exactly 1", n)
		}
	})

	t.Run("appends_when_no_prior_entry", func(t *testing.T) {
		env := []string{"PATH=/bin"}
		got := AppendEnvOverride(env, "OPENAI_API_KEY", "v")
		want := []string{"PATH=/bin", "OPENAI_API_KEY=v"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("leaves_unrelated_entries_order_stable", func(t *testing.T) {
		// A key that is a prefix of an unrelated one must not be stripped:
		// matching is on the full "KEY=" prefix, so FOO= leaves FOOBAR=.
		env := []string{"A=1", "FOOBAR=keep", "B=2", "FOO=old"}
		got := AppendEnvOverride(env, "FOO", "new")
		want := []string{"A=1", "FOOBAR=keep", "B=2", "FOO=new"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("does_not_mutate_or_alias_input", func(t *testing.T) {
		env := []string{"PATH=/bin", "FOO=old", "HOME=/root"}
		orig := append([]string(nil), env...)
		got := AppendEnvOverride(env, "FOO", "new")
		if !reflect.DeepEqual(env, orig) {
			t.Errorf("input slice mutated: got %q, want %q", env, orig)
		}
		// The result must not share backing storage with the input: writing
		// through one must not be visible through the other.
		if len(got) > 0 {
			got[0] = "MUTATED=1"
			if env[0] == "MUTATED=1" {
				t.Error("result aliases the caller's backing array")
			}
		}
	})
}

// TestStructuredOutput_ZeroValues pins the feature-gate default (#1325): an
// Invocation with no JSONSchema and a Result with no StructuredOutput are the
// zero values, so the structured-output path is inert unless explicitly wired —
// every existing caller keeps today's byte-for-byte behavior.
func TestStructuredOutput_ZeroValues(t *testing.T) {
	if (Invocation{}).JSONSchema != "" {
		t.Error("zero-value Invocation.JSONSchema should be empty (no --json-schema flag)")
	}
	if (Result{}).StructuredOutput != nil {
		t.Error("zero-value Result.StructuredOutput should be nil")
	}
}

func TestErrors_AreDistinct(t *testing.T) {
	pairs := []struct {
		a, b error
	}{
		{ErrAgentFailed, ErrBudgetExceeded},
		{ErrAgentFailed, ErrTimeout},
		{ErrAgentFailed, ErrBinaryNotFound},
		{ErrBudgetExceeded, ErrTimeout},
		{ErrBudgetExceeded, ErrBinaryNotFound},
		{ErrTimeout, ErrBinaryNotFound},
		{ErrAgentThinkingBlock, ErrAgentFailed},
		{ErrAgentThinkingBlock, ErrBudgetExceeded},
		{ErrAgentThinkingBlock, ErrTimeout},
		{ErrAgentThinkingBlock, ErrBinaryNotFound},
	}
	for _, p := range pairs {
		if errors.Is(p.a, p.b) {
			t.Errorf("errors.Is(%v, %v) = true, want false", p.a, p.b)
		}
	}
}
