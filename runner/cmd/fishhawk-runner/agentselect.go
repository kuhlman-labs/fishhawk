package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/kuhlman-labs/fishhawk/runner/internal/agent"
	"github.com/kuhlman-labs/fishhawk/runner/internal/agent/codex"
)

// apiKeyForAgent resolves the host env var carrying the agent's API key,
// keyed by provider id. claude-code reads ANTHROPIC_API_KEY (the
// historical default, unchanged); codex reads OPENAI_API_KEY. An
// unknown id falls back to ANTHROPIC_API_KEY — harmless, since
// selectInvoker rejects it before the key is used. The customer supplies
// the key via GitHub Secrets (MVP_SPEC §5.3); the selected adapter
// forwards it to the child as the provider's expected env var.
func apiKeyForAgent(agentID string) string {
	switch agentID {
	case "codex":
		return os.Getenv("OPENAI_API_KEY")
	default:
		return os.Getenv("ANTHROPIC_API_KEY")
	}
}

// errUnknownAgent is the sentinel a caller switches on when the
// requested agent id matches no known provider. selectInvoker wraps it
// with the offending id; the run() entrypoint maps it to a category-A
// runner/agent failure and exits before any agent is invoked.
var errUnknownAgent = errors.New("agent: unknown provider")

// newCodexInvoker is the seam for the codex provider, wiring the real
// codex adapter (#840). Kept as a var so the adapter is constructed in
// one place and tests can swap it for a fake (e.g. redirecting to a
// fake-binary seam) without standing up the real `codex` binary.
var newCodexInvoker = func(apiKey string) agent.Invoker {
	return codex.New(apiKey)
}

// selectInvoker maps an agent id to a concrete agent.Invoker.
//
//	claude-code → the claudecode adapter (via the newInvoker seam)
//	codex       → the codex adapter (via the newCodexInvoker seam)
//	(anything)  → errUnknownAgent wrapping the offending id
//
// The default agent id is claude-code (set on the --agent flag), so an
// invocation that omits the flag selects the historical Claude adapter
// and behaves exactly as before.
func selectInvoker(agentID, apiKey string) (agent.Invoker, error) {
	switch agentID {
	case "claude-code":
		return newInvoker(apiKey), nil
	case "codex":
		return newCodexInvoker(apiKey), nil
	default:
		return nil, fmt.Errorf("%w: %q", errUnknownAgent, agentID)
	}
}
