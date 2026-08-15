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
// A second, INDEPENDENT class rides the same scan (#2494): a cost row
// whose model id is a bracket-wrapped placeholder ("<synthetic>") records
// a request that never reached a model. Those rows are routed out of the
// unpriced sets entirely and reported as agent_request_failed_alert with
// the token-ratio evidence, so unpriced_model_alert means only what it
// says — a real model with no price.
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
	"strings"
	"time"
)

// Window is how far back the detector looks. A model that recorded an
// unpriced or no-usage cost row within [now-Window, now] trips the
// alert, subject to the once-per-window dedup below. 24h mirrors
// spendalert.Window so both ground-truth ledger checks share one horizon.
const Window = 24 * time.Hour

// Sample is one cost observation read back from a cost_recorded audit
// entry: when it happened, the reported model id, the two ground-truth
// coverage flags the pricer stamped, and the four token counts that ride
// the same payload. KnownModel=false means the model id was absent from
// the pricing table; KnownUsage=false means the backend reported no
// usable token split.
//
// The token counts are carried so a FAILED-REQUEST classification (see
// IsFailedRequestModel) can report the token ratio that makes the
// diagnosis legible — a request that never reached a model shows a large
// cache read against near-zero output. InputTokens is the FRESH
// (cache-exclusive) input; the cache read/write portions are separate.
type Sample struct {
	Time       time.Time
	Model      string
	KnownModel bool
	KnownUsage bool

	InputTokens           int
	CacheReadInputTokens  int
	CacheWriteInputTokens int
	OutputTokens          int
}

// Alert represents a prior emitted alert for one model, used to suppress
// a re-alarm within the same window. The backend expands each prior alert
// payload's model arrays into one Alert per model id.
//
// FailedRequest names WHICH alert stream the prior entry came from, and
// the two streams dedup INDEPENDENTLY (#2494): a prior
// unpriced_model_alert (FailedRequest=false) suppresses only the unpriced
// / unknown-usage sets, and a prior agent_request_failed_alert
// (FailedRequest=true) suppresses only the failed-request set. See
// Evaluate for why the classes deliberately do not cross-suppress.
type Alert struct {
	Time  time.Time
	Model string
	// FailedRequest is true when this prior alert came from the
	// agent_request_failed_alert stream. The zero value (false) is the
	// pre-#2494 unpriced_model_alert stream.
	FailedRequest bool
}

// IsFailedRequestModel reports whether a model id is a PLACEHOLDER for a
// request that never reached a model, rather than a model identifier.
//
// Claude Code stamps a bracket-wrapped pseudo-id (the observed value is
// "<synthetic>") on a message it synthesized locally because the API
// request failed before any model ran. Such a row must never be reported
// as an unpriced model: there is no model to price, and treating it as a
// pricing-coverage gap sends the operator hunting for a missing pricing
// entry instead of a failed request (#2494).
//
// The predicate keys on the bracket WRAPPER shape alone — no allow-list of
// specific placeholder ids — so a genuinely-unpriced REAL model id (a
// freshly released claude-fable-5, a gpt-5.6-*) is unaffected and keeps
// flowing to the unpriced set.
func IsFailedRequestModel(model string) bool {
	m := strings.TrimSpace(model)
	return len(m) >= 2 && strings.HasPrefix(m, "<") && strings.HasSuffix(m, ">")
}

// FailedRequestEvidence is the token-ratio evidence attached to a
// failed-request alert: the summed token counts across the in-window
// failed-request samples the alert REPORTS (a sample suppressed by the
// once-per-window dedup contributes nothing, so the evidence always
// describes the models the payload names), plus the derived cache-read
// ratio.
//
// The ratio is what made the original diagnosis possible: a request that
// died before reaching a model still replays a large cached prompt, so a
// cache-read share near 1.0 against near-zero output is the signature of a
// failed request rather than real work.
type FailedRequestEvidence struct {
	InputTokens           int
	CacheReadInputTokens  int
	CacheWriteInputTokens int
	OutputTokens          int
	// CacheReadRatio is cache_read / (fresh_input + cache_read +
	// cache_write). It is 0 when the denominator is 0 (a manifest that
	// reported no usage at all) — never a NaN.
	CacheReadRatio float64
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
	// FailedRequestTripped is the SECOND, independent emit gate: true when
	// at least one in-window sample carried a failed-request placeholder id
	// that has not already been alarmed on the failed-request stream this
	// window. It is deliberately NOT folded into Tripped so the two alerts
	// emit independently.
	FailedRequestTripped bool
	// FailedRequestModels is the sorted set of in-window placeholder model
	// ids (IsFailedRequestModel) minus any already alarmed on the
	// failed-request stream this window. These ids are EXCLUDED from
	// UnpricedModels and UnknownUsageModels.
	FailedRequestModels []string
	// FailedRequestEvidence carries the summed token counts + derived
	// cache-read ratio across the reported failed-request samples.
	FailedRequestEvidence FailedRequestEvidence
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
// A sample whose model id is a FAILED-REQUEST placeholder
// (IsFailedRequestModel) is routed into FailedRequestModels instead and
// EXCLUDED from both unpriced sets (#2494): a request that never reached a
// model is not a pricing-coverage gap.
//
// The two classes are STRICTLY INDEPENDENT — deliberately chosen over
// cross-suppression (#2494 approval condition 2). Historical placeholder
// occurrences were recorded under unpriced_model_alert, so on the first
// window after this ships a prior unpriced_model_alert naming "<synthetic>"
// does NOT suppress the first agent_request_failed_alert, and the operator
// sees ONE duplicate report at that single upgrade boundary. That is
// cheaper than a cross-class suppression rule which would have to be
// reasoned about on every window forever, and after the boundary each
// class dedups against its own history exactly as before.
//
// A non-positive window falls back to Window. Empty model ids are
// skipped. All model sets are deduped and sorted so the emitted payload
// is deterministic. This is warn-only: Evaluate never returns an error
// and always populates Decision; Tripped and FailedRequestTripped are the
// caller's two independent emit gates.
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
	// alarmed / failedAlarmed are the two INDEPENDENT prior-alert sets: a
	// prior alert suppresses only its own class (see the strict-independence
	// rationale on Evaluate's doc comment).
	alarmed := make(map[string]struct{})
	failedAlarmed := make(map[string]struct{})
	for _, a := range priorAlerts {
		if a.Model == "" {
			continue
		}
		if a.Time.UTC().Before(windowStart) {
			continue
		}
		if a.FailedRequest {
			failedAlarmed[a.Model] = struct{}{}
			continue
		}
		alarmed[a.Model] = struct{}{}
	}

	unpriced := make(map[string]struct{})
	unknownUsage := make(map[string]struct{})
	failedRequest := make(map[string]struct{})
	var ev FailedRequestEvidence
	for _, s := range samples {
		if s.Model == "" {
			continue
		}
		t := s.Time.UTC()
		if t.Before(windowStart) || t.After(now) {
			continue
		}
		// Failed-request placeholders never reach the unpriced sets: the id
		// names a request that died before any model ran, so known_model=false
		// on such a row is a tautology, not a pricing-coverage gap.
		if IsFailedRequestModel(s.Model) {
			if _, seen := failedAlarmed[s.Model]; seen {
				continue
			}
			failedRequest[s.Model] = struct{}{}
			ev.InputTokens += s.InputTokens
			ev.CacheReadInputTokens += s.CacheReadInputTokens
			ev.CacheWriteInputTokens += s.CacheWriteInputTokens
			ev.OutputTokens += s.OutputTokens
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

	if denom := ev.InputTokens + ev.CacheReadInputTokens + ev.CacheWriteInputTokens; denom > 0 {
		ev.CacheReadRatio = float64(ev.CacheReadInputTokens) / float64(denom)
	}

	d.UnpricedModels = sortedKeys(unpriced)
	d.UnknownUsageModels = sortedKeys(unknownUsage)
	d.Tripped = len(d.UnpricedModels) > 0 || len(d.UnknownUsageModels) > 0
	d.FailedRequestModels = sortedKeys(failedRequest)
	d.FailedRequestEvidence = ev
	d.FailedRequestTripped = len(d.FailedRequestModels) > 0
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
