package planreview

import "time"

// ReviewBudget computes a per-invocation review timeout from the prompt size
// (#747). A fixed FISHHAWKD_PLAN_REVIEW_TIMEOUT killed the reviewer mid-
// inference on large diffs (e.g. a 13-file / ~600-line implement diff), and
// the verdict was silently dropped, surfacing only as an opaque
// *_review_failed. A size-aware budget gives a large diff proportionally more
// wall-clock while still bounding the worst case, so the implement review
// (larger inputs than plan review) naturally gets a larger budget with no
// separate per-stage default.
//
// The budget is a pure function of the prompt byte length, so it is trivially
// unit-testable and carries no dependencies. It is computed at the server call
// site and applied as a context deadline; the claudecode adapter honours an
// incoming ctx deadline (and falls back to its own Config.Timeout only when the
// caller set none).
type ReviewBudget struct {
	// Floor is the minimum budget, applied for an empty or tiny prompt. It
	// preserves the #606 300s floor so small plans behave exactly as before.
	Floor time.Duration
	// PerKB is the additional allowance granted per kilobyte of prompt
	// (rounded up to the next KB). A zero PerKB degrades the budget to a flat
	// Floor — the operator escape hatch that restores today's fixed behaviour
	// without a redeploy (set FISHHAWKD_REVIEW_BUDGET_PER_KB=0).
	PerKB time.Duration
	// Cap is the hard ceiling that bounds the worst-case loop wait. A non-
	// positive Cap disables the ceiling (the budget then grows unbounded with
	// prompt size, which is not recommended). Floor always wins on conflict: a
	// Cap below Floor still yields at least Floor.
	Cap time.Duration
}

// DefaultReviewBudget is the budget policy used when server.Config.ReviewBudget
// is left zero. Floor 300s preserves the #606 floor; PerKB 10s covers larger
// diffs (a ~25KB implement prompt resolves to ~550s); Cap 1200s (20m) bounds
// the worst-case synchronous gating wait.
var DefaultReviewBudget = ReviewBudget{
	Floor: 300 * time.Second,
	PerKB: 10 * time.Second,
	Cap:   1200 * time.Second,
}

// Budget returns the per-invocation timeout for a prompt of promptLen bytes:
//
//	Floor + PerKB*ceil(promptLen/1024), clamped to [Floor, Cap].
//
// The ceil division rounds any partial kilobyte up to a full KB allowance. The
// Cap is applied only when positive; Floor is applied last so the result is
// never below Floor even if Cap is misconfigured below it.
func (b ReviewBudget) Budget(promptLen int) time.Duration {
	if promptLen < 0 {
		promptLen = 0
	}
	kb := (promptLen + 1023) / 1024 // ceil(promptLen / 1024)
	d := b.Floor + b.PerKB*time.Duration(kb)
	if b.Cap > 0 && d > b.Cap {
		d = b.Cap
	}
	if d < b.Floor {
		d = b.Floor
	}
	return d
}
