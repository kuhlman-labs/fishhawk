package main

import (
	"encoding/json"
	"sort"

	"github.com/kuhlman-labs/fishhawk/runner/internal/agent"
	"github.com/kuhlman-labs/fishhawk/runner/internal/redaction"
)

/*
 * Trace-bundle redaction (E2.4 / closes the redacted-variant gap
 * #218 left). The runner ships every stage's bundle to S3 in two
 * variants:
 *
 *   - raw        — events as captured, gated by S3 Object Lock
 *                  for compliance.
 *   - redacted   — same events with `redaction.RedactDefault`
 *                  applied to each payload (and to the manifest's
 *                  agent_failure_reason); the SPA's transcript
 *                  view reads this one.
 *
 * Redaction happens upstream of `bundle.PackBytes` so the redacted
 * variant is a clean re-pack of redacted source events. That keeps
 * the trailer's content_hash valid (it's computed by Pack from the
 * actual payload bytes), and avoids the post-hoc gunzip/rewrite
 * dance that operating on packed bytes would require.
 *
 * Per MVP_SPEC §4.4 the pattern set is intentionally conservative
 * (low false-positive risk on known credential formats) — the raw
 * bundle exists for the cases the redactor misses.
 */

// redactEvents applies RedactDefault to each event's payload and
// returns a new slice (in order) plus the aggregated hit counts
// across all events. The original slice is not mutated.
//
// Events whose payload is nil pass through untouched. Events whose
// payload is non-empty get the entire payload-bytes slice passed
// through Redact — secrets can appear in any field of the verbatim
// JSON we capture, so per-field redaction would miss anywhere we
// don't think to look.
func redactEvents(events []agent.Event) ([]agent.Event, []redaction.Hit) {
	if len(events) == 0 {
		return events, nil
	}
	out := make([]agent.Event, len(events))
	totals := map[string]int{}
	for i, e := range events {
		out[i] = e
		if len(e.Payload) == 0 {
			continue
		}
		redacted, hits := redaction.RedactDefault([]byte(e.Payload))
		out[i].Payload = json.RawMessage(redacted)
		for _, h := range hits {
			totals[h.Pattern] += h.Count
		}
	}
	return out, hitsFromMap(totals)
}

// redactString runs RedactDefault on a single string and returns the
// redacted form plus the aggregated hits. Used for the manifest's
// AgentFailureReason — process exit messages can carry tokens
// agents printed before crashing.
func redactString(s string) (string, []redaction.Hit) {
	if s == "" {
		return s, nil
	}
	red, hits := redaction.RedactDefault([]byte(s))
	return string(red), hits
}

// hitsFromMap normalizes a counts-by-pattern map into the sorted
// []Hit shape redaction.Redact returns. Sort order is alphabetical
// by pattern name so the resulting telemetry is stable across runs.
func hitsFromMap(m map[string]int) []redaction.Hit {
	if len(m) == 0 {
		return nil
	}
	out := make([]redaction.Hit, 0, len(m))
	for name, n := range m {
		out = append(out, redaction.Hit{Pattern: name, Count: n})
	}
	// Same ordering as redaction.Redact's own output so the two
	// telemetry paths produce comparable lines.
	sort.Slice(out, func(i, j int) bool { return out[i].Pattern < out[j].Pattern })
	return out
}

// mergeHits combines two []Hit slices by summing counts per pattern.
// Used to report a single per-stage redaction line covering both the
// events loop and the manifest-reason call.
func mergeHits(a, b []redaction.Hit) []redaction.Hit {
	if len(a) == 0 {
		return b
	}
	if len(b) == 0 {
		return a
	}
	totals := map[string]int{}
	for _, h := range a {
		totals[h.Pattern] += h.Count
	}
	for _, h := range b {
		totals[h.Pattern] += h.Count
	}
	return hitsFromMap(totals)
}
