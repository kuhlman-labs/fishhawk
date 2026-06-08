package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/kuhlman-labs/fishhawk/runner/internal/agent"
)

// errUnknownAgent is the sentinel a caller switches on when the
// requested agent id matches no known provider. selectInvoker wraps it
// with the offending id; the run() entrypoint maps it to a category-A
// runner/agent failure and exits before any agent is invoked.
var errUnknownAgent = errors.New("agent: unknown provider")

// newCodexInvoker is the seam for the codex provider. It returns a
// minimal placeholder whose Invoke fails not-implemented; the real
// codex adapter (tracked separately, #840) replaces this branch. Kept
// as a var so a future adapter can be wired in one place and tests can
// assert routing without standing up the real binary.
var newCodexInvoker = func(_ string) agent.Invoker {
	return codexPlaceholder{}
}

// codexPlaceholder is the deferred codex provider. Selection succeeds
// (selectInvoker returns it with no error), but Invoke returns a
// category-A not-implemented agent error until the real adapter lands.
// This keeps codex a recognized provider — distinct from an unknown id
// that fails fast at selection — so #840 can drop in the concrete
// adapter without touching the selector's routing contract.
type codexPlaceholder struct{}

func (codexPlaceholder) Invoke(_ context.Context, _ agent.Invocation) (agent.Result, error) {
	return agent.Result{
		OK:              false,
		FailureCategory: "A",
		FailureReason:   "codex agent provider not implemented",
	}, fmt.Errorf("%w: codex provider not implemented", agent.ErrAgentFailed)
}

// selectInvoker maps an agent id to a concrete agent.Invoker.
//
//	claude-code → the existing claudecode adapter (via the newInvoker seam)
//	codex       → a deferred placeholder (via the newCodexInvoker seam)
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
