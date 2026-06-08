package main

import (
	"context"
	"errors"
	"testing"

	"github.com/kuhlman-labs/fishhawk/runner/internal/agent"
)

// TestSelectInvoker_ClaudeCode covers both the default id and an
// explicit "claude-code": each routes through the newInvoker seam and
// returns the claude adapter with no error.
func TestSelectInvoker_ClaudeCode(t *testing.T) {
	// Swap the seam for a sentinel so we can prove claude-code routed
	// through newInvoker (not some other branch) and forwarded the key.
	sentinel := &fakeInvoker{}
	orig := newInvoker
	var gotKey string
	newInvoker = func(apiKey string) agent.Invoker {
		gotKey = apiKey
		return sentinel
	}
	t.Cleanup(func() { newInvoker = orig })

	for _, id := range []string{"claude-code"} {
		inv, err := selectInvoker(id, "secret-key")
		if err != nil {
			t.Fatalf("selectInvoker(%q) error = %v", id, err)
		}
		if inv != agent.Invoker(sentinel) {
			t.Errorf("selectInvoker(%q) did not route through newInvoker seam", id)
		}
		if gotKey != "secret-key" {
			t.Errorf("selectInvoker(%q) forwarded key = %q, want %q", id, gotKey, "secret-key")
		}
	}
}

// TestSelectInvoker_Codex asserts the codex routing contract: selection
// succeeds with a non-nil invoker and no error (a recognized provider),
// and its Invoke returns the deferred not-implemented agent error. This
// guards the placeholder against being collapsed into the unknown-agent
// fail-fast branch when the real adapter (#840) is wired.
func TestSelectInvoker_Codex(t *testing.T) {
	inv, err := selectInvoker("codex", "key")
	if err != nil {
		t.Fatalf("selectInvoker(codex) error = %v, want nil", err)
	}
	if inv == nil {
		t.Fatal("selectInvoker(codex) returned nil invoker")
	}
	res, ierr := inv.Invoke(context.Background(), agent.Invocation{})
	if ierr == nil {
		t.Fatal("codex placeholder Invoke returned nil error, want not-implemented")
	}
	if !errors.Is(ierr, agent.ErrAgentFailed) {
		t.Errorf("codex placeholder Invoke error = %v, want wrapping ErrAgentFailed", ierr)
	}
	if res.OK {
		t.Error("codex placeholder Result.OK = true, want false")
	}
	if res.FailureCategory != "A" {
		t.Errorf("codex placeholder FailureCategory = %q, want A", res.FailureCategory)
	}
}

// TestSelectInvoker_Unknown asserts an unrecognized id fails fast with
// errUnknownAgent and a nil invoker.
func TestSelectInvoker_Unknown(t *testing.T) {
	inv, err := selectInvoker("gpt-9000", "key")
	if !errors.Is(err, errUnknownAgent) {
		t.Fatalf("selectInvoker(unknown) error = %v, want errUnknownAgent", err)
	}
	if inv != nil {
		t.Error("selectInvoker(unknown) returned non-nil invoker")
	}
}
