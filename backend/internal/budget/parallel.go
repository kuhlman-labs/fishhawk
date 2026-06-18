package budget

// This file adds the decomposition concurrency-cap decision core (E24.6 /
// #1146). It is the fourth pure decision in the budget package, alongside
// Evaluate (periodic per-workflow budgets) and EvaluateRun (the per-run
// tripwire): where those gate spend, ParallelDecision gates how many
// decomposed child runs may dispatch at once.
//
// Like the other two it is a storage-free pure function with NO repository
// dependency: the caller supplies the requested fan-out width and the
// resolved cap (from spec.EffectiveMaxParallel) and ParallelDecision
// returns a fully-populated ParallelResult. This is the CONTRACT the
// concurrency throttle in E24.3 (#1143) consumes; #1146 ships the contract
// and resolution but does not yet call it from the dispatch loop.

// ParallelResult is the outcome of a ParallelDecision call. It is fully
// populated regardless of whether the cap throttled the fan-out so the
// caller can log every figure; Allowed is the number the caller should
// dispatch and Capped reports whether the cap reduced it below Requested.
type ParallelResult struct {
	// Requested echoes the fan-out width the caller asked for (the number
	// of decomposed children to dispatch concurrently).
	Requested int
	// Cap echoes the resolved cap the caller passed in. A non-positive Cap
	// means "unlimited" — no throttle.
	Cap int
	// Allowed is the number the caller may dispatch concurrently: Requested
	// when the cap is unlimited or not binding, else Cap.
	Allowed int
	// Capped is true when the cap reduced Allowed below Requested.
	Capped bool
}

// ParallelDecision resolves how many of `requested` decomposed children may
// dispatch concurrently under `cap`. Semantics (0 = unlimited, kept
// consistent with spec.EffectiveMaxParallel):
//
//   - cap <= 0   → unlimited: Allowed = requested, Capped = false.
//   - cap >= requested → cap not binding: Allowed = requested, Capped = false.
//   - cap < requested  → throttle: Allowed = cap, Capped = true.
//
// It never queries and has no side effects; the caller logs the result and
// (in E24.3 / #1143) dispatches Allowed children at a time.
func ParallelDecision(requested, cap int) ParallelResult {
	r := ParallelResult{
		Requested: requested,
		Cap:       cap,
		Allowed:   requested,
	}
	if cap <= 0 {
		return r
	}
	if cap < requested {
		r.Allowed = cap
		r.Capped = true
	}
	return r
}
