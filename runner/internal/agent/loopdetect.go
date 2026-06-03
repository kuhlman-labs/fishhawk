package agent

// DefaultLoopThreshold is the number of identical CONSECUTIVE tool-call
// signatures that trips the loop detector when a caller does not set its
// own threshold. It is deliberately conservative: a real agent doing real
// work varies its actions (read a file, edit it, run a test, read the
// result), so an unbroken run of this many identical calls is a strong
// no-progress signal, while the handful of legitimate repeats — re-reading
// the same file, retrying a flaky command two or three times — stay well
// under it and never false-abort.
const DefaultLoopThreshold = 8

// LoopDetector flags a no-progress / duplicate-action loop by watching the
// stream of tool-call signatures an agent emits during a single stage
// invocation. It trips when the SAME signature recurs in an unbroken
// consecutive run of length >= threshold.
//
// It is conservative by construction. ONLY an unbroken streak of identical
// signatures trips it — any differing signature in between resets the
// streak to one. So an agent that re-reads a file once, or retries a flaky
// command a couple of times interleaved with other work, never trips it;
// the agent has to do the exact same thing threshold-times-in-a-row with
// nothing else between. The signature granularity (tool name + arguments,
// see the adapter that builds it) means "Read file X" and "Read file Y"
// are distinct calls and do not accumulate against each other.
//
// The detector is pure: it holds only the running streak, has no I/O, and
// no dependency on the agent backend. The zero value is usable and behaves
// as if constructed with DefaultLoopThreshold; prefer NewLoopDetector for
// clarity.
type LoopDetector struct {
	threshold int
	last      string
	streak    int
	tripped   bool
}

// NewLoopDetector returns a detector that trips after threshold identical
// consecutive signatures. A threshold <= 0 falls back to
// DefaultLoopThreshold so callers can pass an unset config value through
// unchanged.
func NewLoopDetector(threshold int) *LoopDetector {
	if threshold <= 0 {
		threshold = DefaultLoopThreshold
	}
	return &LoopDetector{threshold: threshold}
}

// Observe feeds one tool-call signature and reports whether the detector
// has now tripped. Once tripped it stays tripped — subsequent calls keep
// returning true — so an observe-then-check caller cannot miss the edge.
//
// An empty signature is a no-op: it neither advances nor resets the streak
// and returns the current tripped state. Non-tool trace events carry no
// signature and must not break an otherwise-identical run of tool calls.
func (d *LoopDetector) Observe(sig string) bool {
	if d.tripped {
		return true
	}
	if sig == "" {
		return false
	}
	if d.threshold <= 0 {
		d.threshold = DefaultLoopThreshold
	}
	if sig == d.last {
		d.streak++
	} else {
		d.last = sig
		d.streak = 1
	}
	if d.streak >= d.threshold {
		d.tripped = true
	}
	return d.tripped
}

// Tripped reports whether a loop has been detected.
func (d *LoopDetector) Tripped() bool { return d.tripped }

// Streak returns the length of the current identical-signature run. At the
// moment Observe trips, this equals the configured threshold — useful for
// naming the figure in an audit reason.
func (d *LoopDetector) Streak() int { return d.streak }

// Signature returns the most recently observed signature — the one whose
// repetition tripped the detector. Empty before the first Observe.
func (d *LoopDetector) Signature() string { return d.last }
