package main

import (
	"errors"
	"testing"

	"github.com/kuhlman-labs/fishhawk/runner/internal/agent"
	"github.com/kuhlman-labs/fishhawk/runner/internal/agent/claudecode"
	"github.com/kuhlman-labs/fishhawk/runner/internal/agent/codex"
)

// TestSelectInvoker_ClaudeCode covers both the default id and an
// explicit "claude-code": each routes through the newInvoker seam and
// returns the claude adapter with no error, forwarding both the API key
// and the binary override.
func TestSelectInvoker_ClaudeCode(t *testing.T) {
	// Swap the seam for a sentinel so we can prove claude-code routed
	// through newInvoker (not some other branch) and forwarded the key
	// and binary override.
	sentinel := &fakeInvoker{}
	orig := newInvoker
	var gotKey, gotBinary string
	newInvoker = func(apiKey, binary string) agent.Invoker {
		gotKey = apiKey
		gotBinary = binary
		return sentinel
	}
	t.Cleanup(func() { newInvoker = orig })

	for _, id := range []string{"claude-code"} {
		inv, err := selectInvoker(id, "secret-key", "/opt/claude")
		if err != nil {
			t.Fatalf("selectInvoker(%q) error = %v", id, err)
		}
		if inv != agent.Invoker(sentinel) {
			t.Errorf("selectInvoker(%q) did not route through newInvoker seam", id)
		}
		if gotKey != "secret-key" {
			t.Errorf("selectInvoker(%q) forwarded key = %q, want %q", id, gotKey, "secret-key")
		}
		if gotBinary != "/opt/claude" {
			t.Errorf("selectInvoker(%q) forwarded binary = %q, want %q", id, gotBinary, "/opt/claude")
		}
	}
}

// TestSelectInvoker_Codex asserts the codex routing contract: selection
// succeeds with a non-nil invoker and no error (a recognized provider),
// routing through the newCodexInvoker seam with the key and binary
// forwarded. This is now the REAL codex adapter (#840), no longer a
// not-implemented placeholder; it must still be distinct from the
// unknown-agent fail-fast branch.
func TestSelectInvoker_Codex(t *testing.T) {
	sentinel := &fakeInvoker{}
	orig := newCodexInvoker
	var gotKey, gotBinary string
	newCodexInvoker = func(apiKey, binary string) agent.Invoker {
		gotKey = apiKey
		gotBinary = binary
		return sentinel
	}
	t.Cleanup(func() { newCodexInvoker = orig })

	inv, err := selectInvoker("codex", "openai-secret", "/opt/codex")
	if err != nil {
		t.Fatalf("selectInvoker(codex) error = %v, want nil", err)
	}
	if inv != agent.Invoker(sentinel) {
		t.Error("selectInvoker(codex) did not route through newCodexInvoker seam")
	}
	if gotKey != "openai-secret" {
		t.Errorf("selectInvoker(codex) forwarded key = %q, want openai-secret", gotKey)
	}
	if gotBinary != "/opt/codex" {
		t.Errorf("selectInvoker(codex) forwarded binary = %q, want /opt/codex", gotBinary)
	}
}

// TestSelectInvoker_CodexDefault asserts the default newCodexInvoker seam
// constructs the real codex adapter (a non-nil invoker), guarding against
// a regression back to a nil / placeholder branch.
func TestSelectInvoker_CodexDefault(t *testing.T) {
	inv, err := selectInvoker("codex", "key", "")
	if err != nil {
		t.Fatalf("selectInvoker(codex) error = %v, want nil", err)
	}
	if inv == nil {
		t.Fatal("selectInvoker(codex) returned nil invoker")
	}
}

// TestSelectInvoker_StampsBinary pins the binary-override threading
// through the DEFAULT (unswapped) seams onto the concrete adapters: a
// non-empty override sets the invoker's .Binary, and an empty override
// leaves .Binary "" so the adapter resolves its DefaultBinary
// (preserving historical PATH resolution). Both providers are covered.
func TestSelectInvoker_StampsBinary(t *testing.T) {
	t.Run("claude-code override sets .Binary", func(t *testing.T) {
		inv, err := selectInvoker("claude-code", "key", "/opt/claude-good")
		if err != nil {
			t.Fatalf("selectInvoker(claude-code) error = %v", err)
		}
		cc, ok := inv.(*claudecode.Invoker)
		if !ok {
			t.Fatalf("invoker type = %T, want *claudecode.Invoker", inv)
		}
		if cc.Binary != "/opt/claude-good" {
			t.Errorf("claudecode .Binary = %q, want /opt/claude-good", cc.Binary)
		}
	})
	t.Run("claude-code empty override leaves .Binary empty", func(t *testing.T) {
		inv, err := selectInvoker("claude-code", "key", "")
		if err != nil {
			t.Fatalf("selectInvoker(claude-code) error = %v", err)
		}
		cc, ok := inv.(*claudecode.Invoker)
		if !ok {
			t.Fatalf("invoker type = %T, want *claudecode.Invoker", inv)
		}
		if cc.Binary != "" {
			t.Errorf("claudecode .Binary = %q, want empty (adapter resolves DefaultBinary)", cc.Binary)
		}
	})
	t.Run("codex override sets .Binary", func(t *testing.T) {
		inv, err := selectInvoker("codex", "key", "/opt/codex-good")
		if err != nil {
			t.Fatalf("selectInvoker(codex) error = %v", err)
		}
		cx, ok := inv.(*codex.Invoker)
		if !ok {
			t.Fatalf("invoker type = %T, want *codex.Invoker", inv)
		}
		if cx.Binary != "/opt/codex-good" {
			t.Errorf("codex .Binary = %q, want /opt/codex-good", cx.Binary)
		}
	})
	t.Run("codex empty override leaves .Binary empty", func(t *testing.T) {
		inv, err := selectInvoker("codex", "key", "")
		if err != nil {
			t.Fatalf("selectInvoker(codex) error = %v", err)
		}
		cx, ok := inv.(*codex.Invoker)
		if !ok {
			t.Fatalf("invoker type = %T, want *codex.Invoker", inv)
		}
		if cx.Binary != "" {
			t.Errorf("codex .Binary = %q, want empty (adapter resolves DefaultBinary)", cx.Binary)
		}
	})
}

// TestAPIKeyForAgent pins the per-provider key sourcing: codex reads
// OPENAI_API_KEY, everything else (including the claude-code default and
// an unknown id) reads ANTHROPIC_API_KEY.
func TestAPIKeyForAgent(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "anthropic-key")
	t.Setenv("OPENAI_API_KEY", "openai-key")

	if got := apiKeyForAgent("codex"); got != "openai-key" {
		t.Errorf("apiKeyForAgent(codex) = %q, want openai-key", got)
	}
	if got := apiKeyForAgent("claude-code"); got != "anthropic-key" {
		t.Errorf("apiKeyForAgent(claude-code) = %q, want anthropic-key", got)
	}
	if got := apiKeyForAgent("unknown"); got != "anthropic-key" {
		t.Errorf("apiKeyForAgent(unknown) = %q, want anthropic-key (fallback)", got)
	}
}

// TestSelectInvoker_Unknown asserts an unrecognized id fails fast with
// errUnknownAgent and a nil invoker.
func TestSelectInvoker_Unknown(t *testing.T) {
	inv, err := selectInvoker("gpt-9000", "key", "")
	if !errors.Is(err, errUnknownAgent) {
		t.Fatalf("selectInvoker(unknown) error = %v, want errUnknownAgent", err)
	}
	if inv != nil {
		t.Error("selectInvoker(unknown) returned non-nil invoker")
	}
}
