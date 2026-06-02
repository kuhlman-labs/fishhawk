// Package agent defines the abstraction the runner uses to invoke a
// coding agent (Claude Code in v0; pluggable for v1+) and capture
// its execution as an ordered stream of trace events.
//
// The runner is agent-agnostic: it speaks this package's Invoker
// interface and never imports Claude Code internals directly. The
// concrete adapter for Claude Code lives in
// runner/internal/agent/claudecode.
//
// Trace event vocabulary aligns with docs/ARCHITECTURE.md §5.3. The
// bundling step (E5.3) consumes Result.Events to produce the signed
// trace bundle that ships to the backend.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"time"
)

// Invocation is everything an Invoker needs for a single stage run.
// The runner constructs this from the workflow spec, the stage
// definition, and the resolved inputs (issue body, prior-stage
// artifact, etc.).
type Invocation struct {
	// RunID and Stage are carried through so trace events can be
	// tagged and correlated server-side.
	RunID string
	Stage string

	// Prompt is the constructed text the runner hands the agent.
	// Construction lives outside this package — by the time the
	// Invoker sees the prompt, every variable is already
	// substituted.
	Prompt string

	// WorkingDir is the customer's checked-out repo. The agent runs
	// with this as its CWD; relative paths in tool calls resolve
	// here. The agent must NOT escape this directory; enforcement
	// is via OS sandboxing in v0.x, not v0.
	WorkingDir string

	// Budget bounds the agent's resource consumption. Zero values
	// mean "no limit from this layer" — the runner sets sensible
	// defaults; the workflow spec can override them per stage.
	Budget Budget

	// Env carries additional environment variables to layer onto
	// the child process. The harness seeds the child env from
	// os.Environ() first, then overlays these; later keys win.
	// Used by the runner (E19.8 / #348) to pass the MCP token +
	// backend URL so the agent can call the Fishhawk MCP server
	// mid-execution. Empty / nil is fine — the child inherits the
	// parent env unchanged.
	Env map[string]string

	// ProgressSink receives single-line JSON stage_progress
	// heartbeats emitted at a fixed cadence during the invocation
	// (#580). Each line is a complete `{"event":"stage_progress",...}`
	// JSON object terminated by '\n' carrying coarse, structural
	// liveness metadata (elapsed seconds, turn count, tokens-so-far,
	// last event kind) — never agent payload text, so it has no
	// redaction exposure. The runner wires this to its logSink (the
	// structured stderr stream the fishhawk-mcp relay forwards as
	// progress notifications). Heartbeats are written ONLY here, never
	// appended to Result.Events, so the signed trace bundle is
	// unchanged. A nil ProgressSink disables heartbeats entirely
	// (zero writes), preserving the pre-#580 behavior and tests.
	ProgressSink io.Writer
}

// Budget caps an agent invocation. Per MVP_SPEC §4.2 (`budget`
// block) v0 budgets are advisory. The harness enforces them anyway:
// a hard cap is more useful than a polite warning when it's burning
// money.
type Budget struct {
	// MaxTokens is the cap on total tokens (input + output)
	// reported by the agent. The harness terminates the agent
	// when it exceeds the cap.
	MaxTokens int

	// Timeout is the wall-clock cap on the invocation.
	Timeout time.Duration
}

// Result is what the harness produces for a single Invoke call.
// OK distinguishes clean completion from any kind of failure;
// FailureCategory maps to MVP_SPEC §6 (always 'A' — agent failure —
// for anything originating in this package).
type Result struct {
	OK              bool
	FailureCategory string
	FailureReason   string

	// Events is the captured trace, in order. The first event is
	// always kind=invocation_start; the last is invocation_end.
	// Bundling (E5.3) wraps these with manifest/trailer events.
	Events []Event

	// TokensUsed is the agent-reported token total when known
	// (Claude Code emits a final result event with usage). Zero
	// when the agent didn't report or didn't get that far.
	TokensUsed int

	// InputTokens and OutputTokens split TokensUsed into the
	// prompt-side and completion-side counts the agent reported.
	// They feed the GenAI-semconv observability spans
	// (gen_ai.usage.input_tokens / output_tokens) and the backend
	// cost rollup, which prices input and output at different rates.
	// Both are cumulative across in-driver retries, exactly like
	// TokensUsed, so cost stays honest when a retry doubles spend.
	// Zero when the agent didn't report usage — reporting token usage
	// is part of the Invoker contract (see Invoker), and the backend
	// records a 0/0 split downstream as known_usage=false rather than a
	// silent $0 (#682). claudecode, the sole current backend, always
	// populates these from the result event, so it records
	// known_usage=true.
	InputTokens  int
	OutputTokens int

	// Model is the resolved model id the agent reported (the `model`
	// field on Claude Code's assistant/result events), e.g.
	// "claude-opus-4-8". Pinned for cost pricing and reproducibility
	// (G6): the same model id keys pricing.Cost and is recorded on
	// the run/audit trail. Empty when the agent didn't surface it.
	Model string
}

// Event is one entry in the captured trace. Kind is the discriminant
// for Payload's shape; bundling treats Payload as opaque bytes so
// we can evolve event schemas independently of the trace format.
type Event struct {
	Kind      string          `json:"kind"`
	Timestamp time.Time       `json:"ts"`
	Payload   json.RawMessage `json:"payload,omitempty"`
}

// Invoker runs an agent and produces a Result. Implementations are
// agent-specific; the runner wires whichever Invoker matches the
// workflow stage's `executor.agent` value.
//
// Reporting token usage is part of the contract: an implementation
// that can surface usage MUST populate Result.InputTokens/OutputTokens
// (and Model). A backend that cannot report usage leaves the token
// fields zero, and the backend cost rollup records that bundle as
// known_usage=false at usd=0 rather than a silent $0 (#682). claudecode
// is the only current backend and always reports usage.
type Invoker interface {
	Invoke(ctx context.Context, inv Invocation) (Result, error)
}

// Errors callers may want to switch on. All concrete failures wrap
// one of these.
var (
	ErrAgentFailed    = errors.New("agent: agent failed")
	ErrBudgetExceeded = errors.New("agent: budget exceeded")
	ErrTimeout        = errors.New("agent: timeout")
	ErrBinaryNotFound = errors.New("agent: binary not found")

	// ErrAgentThinkingBlock marks the transient interleaved-thinking
	// API 400 ("thinking/redacted_thinking blocks in the latest
	// assistant message cannot be modified") that kills long agent
	// runs at high turn counts. It is a peer sentinel — it does NOT
	// wrap ErrAgentFailed — so err_class classification stays
	// unambiguous. Downstream category-A handling keys off
	// Result.FailureCategory=="A" (still set on this path), not
	// errors.Is(ErrAgentFailed).
	ErrAgentThinkingBlock = errors.New("agent: api thinking-block 400")
)

// MakePayload marshals v to a json.RawMessage or panics. Helper for
// adapters; payloads are constructed from typed Go values whose
// shapes we control, so a marshal failure is a programmer error.
func MakePayload(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic("agent: payload marshal: " + err.Error())
	}
	return b
}
