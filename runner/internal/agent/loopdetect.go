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

// DefaultWaitPollThreshold is the much higher threshold that applies to a
// streak of WAIT-POLL calls — identical read-only inspections an agent
// issues while AWAITING a long-running backgrounded command (#2758). An
// agent that backgrounds `scripts/test verify` and polls its log has no
// instrument other than the repeated poll, and that poll is legitimately
// identical for minutes at a time (verify runs lint first and is silent),
// so DefaultLoopThreshold killed healthy stages at ~8 polls.
//
// This threshold bounds REPETITIONS, not wall-clock. A wait-classed call
// may itself sleep, so no count maps to a fixed duration: at the 20-30s
// poll cadence an agent typically uses while awaiting a gate, 60 polls is
// an illustrative ~20-30 minutes, but 59 repeated `sleep 600` calls would
// sit in this tier far longer. The real time bound is the implement-stage
// budget (ADR-025), which backstops the detector; this tier only bounds a
// PERMANENTLY frozen poller, so the relaxation is a re-tiering of the
// control rather than its removal.
const DefaultWaitPollThreshold = 60

// LoopDetector flags a no-progress / duplicate-action loop by watching the
// stream of tool-call signatures an agent emits during a single stage
// invocation. It trips when the SAME signature recurs in an unbroken
// consecutive run of length >= the threshold in force for that signature's
// CLASS: waitPollThreshold for a wait-classed signature (see
// DefaultWaitPollThreshold), threshold for everything else.
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
// no dependency on the agent backend. In particular it holds no shell
// knowledge — the CALLER classifies each signature and passes the class to
// Observe, so the wait-poll predicate lives in the adapter that already
// understands the tool vocabulary. The zero value is usable and behaves as
// if constructed with the two defaults; prefer NewLoopDetector for clarity.
type LoopDetector struct {
	threshold         int
	waitPollThreshold int
	last              string
	lastWait          bool
	streak            int
	tripped           bool
}

// NewLoopDetector returns a detector that trips after threshold identical
// consecutive non-wait signatures, or waitPollThreshold identical
// consecutive wait-poll signatures. Each argument falls back INDEPENDENTLY
// to its own default when <= 0, so callers can pass unset config values
// through unchanged.
func NewLoopDetector(threshold, waitPollThreshold int) *LoopDetector {
	if threshold <= 0 {
		threshold = DefaultLoopThreshold
	}
	if waitPollThreshold <= 0 {
		waitPollThreshold = DefaultWaitPollThreshold
	}
	return &LoopDetector{threshold: threshold, waitPollThreshold: waitPollThreshold}
}

// Observe feeds one tool-call signature plus its class — waitPoll true for
// an await of a long-running command (#2758) — and reports whether the
// detector has now tripped. Once tripped it stays tripped — subsequent
// calls keep returning true — so an observe-then-check caller cannot miss
// the edge.
//
// An empty signature is a no-op: it neither advances nor resets the streak
// and returns the current tripped state. Non-tool trace events carry no
// signature and must not break an otherwise-identical run of tool calls.
//
// A signature identical to the previous one but observed with a DIFFERENT
// class resets the streak to one and adopts the new class. In practice
// that is unreachable — the class is a pure function of the signature —
// but it is specified so the state machine is total rather than relying on
// the caller's purity.
func (d *LoopDetector) Observe(sig string, waitPoll bool) bool {
	if d.tripped {
		return true
	}
	if sig == "" {
		return false
	}
	d.normalize()
	if sig == d.last && waitPoll == d.lastWait {
		d.streak++
	} else {
		d.last = sig
		d.lastWait = waitPoll
		d.streak = 1
	}
	if d.streak >= d.effectiveThreshold() {
		d.tripped = true
	}
	return d.tripped
}

// normalize applies the <= 0 defaults in place so the zero value behaves
// exactly like one built by NewLoopDetector.
func (d *LoopDetector) normalize() {
	if d.threshold <= 0 {
		d.threshold = DefaultLoopThreshold
	}
	if d.waitPollThreshold <= 0 {
		d.waitPollThreshold = DefaultWaitPollThreshold
	}
}

// effectiveThreshold is the threshold in force for the current streak's
// class. Assumes normalize has run.
func (d *LoopDetector) effectiveThreshold() int {
	if d.lastWait {
		return d.waitPollThreshold
	}
	return d.threshold
}

// Tripped reports whether a loop has been detected.
func (d *LoopDetector) Tripped() bool { return d.tripped }

// Streak returns the length of the current identical-signature run. At the
// moment Observe trips, this equals the effective threshold for the
// tripping signature's class — useful for naming the figure in an audit
// reason.
func (d *LoopDetector) Streak() int { return d.streak }

// Signature returns the most recently observed signature — the one whose
// repetition tripped the detector. Empty before the first Observe.
func (d *LoopDetector) Signature() string { return d.last }

// WaitPoll reports the class of the most recently observed signature — at
// a trip, the class of the signature that tripped the detector. False
// before the first Observe.
func (d *LoopDetector) WaitPoll() bool { return d.lastWait }

// EffectiveThreshold returns the threshold in force for the most recently
// observed signature's class — at a trip, the threshold that was reached.
// Reports the applicable default when the corresponding field is unset, so
// it is meaningful on a zero-value detector.
func (d *LoopDetector) EffectiveThreshold() int {
	d.normalize()
	return d.effectiveThreshold()
}
