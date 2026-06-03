package budget

// This file adds the WHOLE-RUN budget tripwire's decision core (ADR-030 /
// #653), the third axis of ADR-030's budget story alongside the periodic
// per-workflow budgets (Evaluate, above) and the rate-anomaly detector
// (internal/spendalert). Where Evaluate gates a calendar period across many
// runs, EvaluateRun is a per-run safety rail: it compares a SINGLE run's
// accumulated spend — US dollars and tokens — against operator-configured
// ceilings so a runaway run can be halted before it overruns silently.
//
// Like Evaluate it is a pure function with NO repository dependency: the
// caller supplies the run's already-rolled cost (runs.cost_usd_total, #649)
// and a cumulative token figure, and EvaluateRun returns a fully-populated
// RunDecision. Keeping it storage-free makes the threshold logic trivially
// testable and means the trace-handler wiring only has to shuttle two
// figures in and a decision out.

// Per-run budget dimensions, recorded as the `dimension` field in the
// run_budget_exceeded audit entry so an operator can see which ceiling the
// run breached.
const (
	DimensionUSD    = "usd"
	DimensionTokens = "tokens"
)

// RunDecision is the outcome of an EvaluateRun call. It is fully populated
// regardless of whether a ceiling was crossed so the caller can log or
// surface the figures either way; Over (with Dimension) is the gate the
// caller keys the halt off.
type RunDecision struct {
	// Over is true when either configured ceiling has been reached or
	// exceeded (exact equality counts as over). It is the halt gate.
	Over bool
	// Dimension names the breached axis: DimensionUSD or DimensionTokens.
	// Empty when Over is false. When both ceilings are breached, US$ wins
	// (it is the more operator-legible figure) — EvaluateRun checks it
	// first and returns the US$ dimension.
	Dimension string
	// CostUSD / MaxUSD echo the US$ figures the caller passed in.
	CostUSD float64
	MaxUSD  float64
	// Tokens / MaxTokens echo the token figures the caller passed in.
	Tokens    int64
	MaxTokens int64
}

// EvaluateRun compares a run's accumulated cost against the per-run US$ and
// token ceilings and reports whether either has been breached. The caller
// supplies costUSD (the run's rolled cost_usd_total) and tokens (the run's
// cumulative input+output tokens); EvaluateRun never queries.
//
// A non-positive ceiling means "no ceiling on this dimension" — the default
// 0 disables the tripwire so a misconfigured (or unset) budget never halts a
// run. US$ is checked before tokens so it is the reported dimension when both
// are over.
func EvaluateRun(costUSD float64, tokens int64, maxUSD float64, maxTokens int64) RunDecision {
	d := RunDecision{
		CostUSD:   costUSD,
		MaxUSD:    maxUSD,
		Tokens:    tokens,
		MaxTokens: maxTokens,
	}
	if maxUSD > 0 && costUSD >= maxUSD {
		d.Over = true
		d.Dimension = DimensionUSD
		return d
	}
	if maxTokens > 0 && tokens >= maxTokens {
		d.Over = true
		d.Dimension = DimensionTokens
		return d
	}
	return d
}
