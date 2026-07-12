// Package unpricedmodel detects a ground-truth pricing-coverage gap: a
// model that was dispatched and ran but is not in the shared pricing
// table (known_model=false) or reported no usage the pricer could act on
// (known_usage=false). The cost ledger already stamps known_model /
// known_usage on every cost_recorded audit entry, but until now nothing
// read them back, so a valid-but-unpriced model (e.g. a freshly released
// claude-fable-5 or gpt-5.6-*) silently recorded $0 across many rows.
//
// This is a warn-only signal, deliberately modeled on
// backend/internal/spendalert: an unpriced-model window emits a
// unpriced_model_alert audit entry naming the offending model id(s); it
// never blocks or fails a trace upload. Per ADR-044 the pricing table
// stays human-authoritative — this alarms, it never auto-prices.
//
// The detector is a pure function over cost samples (the backend reads
// them from the cost_recorded audit entries) plus the prior emitted
// alerts (for once-per-window dedup). Keeping it free of any repository
// dependency makes the trip condition trivially testable and means the
// wiring in the trace handler only has to shuttle samples in and an
// audit entry out — exactly the shape checkSpendAlert already proved.
package unpricedmodel

import (
	"sort"
	"time"
)

// Window is how far back the detector looks. A model that recorded an
// unpriced or no-usage cost row within [now-Window, now] trips the
// alert, subject to the once-per-window dedup below. 24h mirrors
// spendalert.Window so both ground-truth ledger checks share one horizon.
const Window = 24 * time.Hour

// Sample is one cost observation read back from a cost_recorded audit
// entry: when it happened, the reported model id, and the two
// ground-truth coverage flags the pricer stamped. KnownModel=false means
// the model id was absent from the pricing table; KnownUsage=false means
// the backend reported no usable token split.
type Sample struct {
	Time       time.Time
	Model      string
	KnownModel bool
	KnownUsage bool
}

// Alert represents a prior emitted unpriced_model_alert for one model,
// used to suppress a re-alarm within the same window. The backend
// expands each prior alert payload's unpriced_models / unknown_usage_models
// arrays into one Alert per model id.
type Alert struct {
	Time  time.Time
	Model string
}

// Decision is the outcome of an Evaluate call. It is fully populated
// whether or not the alert tripped so the caller can log the figures
// either way; Tripped is the only field that gates emission. The two
// model sets are deduped and sorted for deterministic payloads.
type Decision struct {
	// Tripped is true when at least one in-window model is unpriced or
	// reported no usage AND has not already been alarmed this window. It
	// is the sole emit gate.
	Tripped bool
	// UnpricedModels is the sorted set of in-window model ids that
	// recorded a cost row with KnownModel=false, minus any already
	// alarmed this window.
	UnpricedModels []string
	// UnknownUsageModels is the sorted set of in-window model ids that
	// recorded a cost row with KnownUsage=false, minus any already
	// alarmed this window.
	UnknownUsageModels []string
	// Window is the horizon that was applied (after defaulting).
	Window time.Duration
	// WindowStart is now-Window: the inclusive lower bound of the sample
	// and prior-alert scan.
	WindowStart time.Time
}

// Evaluate scans samples in [now-window, now], collecting the set of
// models that recorded an unpriced (KnownModel=false) or no-usage
// (KnownUsage=false) cost row, then suppresses any model already alarmed
// within the window (present in priorAlerts with Time >= windowStart) so
// a persistently-unpriced model alarms once per window rather than once
// per invocation.
//
// A non-positive window falls back to Window. Empty model ids are
// skipped. Both model sets are deduped and sorted so the emitted payload
// is deterministic. This is warn-only: Evaluate never returns an error
// and always populates Decision; Tripped is the caller's emit gate.
func Evaluate(samples []Sample, priorAlerts []Alert, now time.Time, window time.Duration) Decision {
	if window <= 0 {
		window = Window
	}
	now = now.UTC()
	windowStart := now.Add(-window)

	d := Decision{Window: window, WindowStart: windowStart}

	// alarmed collects the models already alerted within this window;
	// they are suppressed from both trip sets. Best-effort dedup: this is
	// noise-reduction on a warn-only path, not a correctness invariant —
	// a concurrent recordCost racing the ListAll->Evaluate->AppendChained
	// sequence can still emit a rare duplicate alert, which is acceptable
	// (mirrors checkSpendAlert's un-serialized best-effort shape).
	alarmed := make(map[string]struct{})
	for _, a := range priorAlerts {
		if a.Model == "" {
			continue
		}
		if a.Time.UTC().Before(windowStart) {
			continue
		}
		alarmed[a.Model] = struct{}{}
	}

	unpriced := make(map[string]struct{})
	unknownUsage := make(map[string]struct{})
	for _, s := range samples {
		if s.Model == "" {
			continue
		}
		t := s.Time.UTC()
		if t.Before(windowStart) || t.After(now) {
			continue
		}
		if _, seen := alarmed[s.Model]; seen {
			continue
		}
		if !s.KnownModel {
			unpriced[s.Model] = struct{}{}
		}
		if !s.KnownUsage {
			unknownUsage[s.Model] = struct{}{}
		}
	}

	d.UnpricedModels = sortedKeys(unpriced)
	d.UnknownUsageModels = sortedKeys(unknownUsage)
	d.Tripped = len(d.UnpricedModels) > 0 || len(d.UnknownUsageModels) > 0
	return d
}

// sortedKeys returns the map's keys as a sorted slice (nil when empty so
// an untripped Decision carries nil, not an empty non-nil slice).
func sortedKeys(set map[string]struct{}) []string {
	if len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
