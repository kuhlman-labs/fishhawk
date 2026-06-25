// Price drift-check (#1335, ADR-044 decision 1).
//
// familyRates above is the single source of truth for cost, but vendors
// reprice over time and neither offers a programmatic rate card, so a stale
// or missing price can sit in the table silently (the #1334 evidence: opus
// at 15/75 for ~3 releases after the real 5/25). This file compares the
// table against the community LiteLLM cross-vendor price dataset and reports
// drift.
//
// Per ADR-044 the LiteLLM dataset is an ALARM, not authority: the manual
// table stays source of truth, the check WARNS and never fails a normal
// build, and a daily scheduled job turns a high-severity report into an
// issue. The dataset is pinned to an immutable commit (AGENTS.md pin-tools
// rule); see pricing/cmd/price-drift. CheckDrift itself takes the dataset
// bytes so it is pure and deterministic — the network fetch lives in the cmd.
//
// This is distinct from TestCost_PricesLiveModelIDs (the internal
// completeness invariant — every live model id must be priced — which stays
// a hard CI FAIL). Drift against an external dataset is operational
// pressure, not a build break.

package pricing

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
)

// DriftSeverity classifies a single field comparison.
type DriftSeverity string

const (
	// SeverityOK means within the ignore band (|delta| < warnThresholdPct).
	SeverityOK DriftSeverity = "ok"
	// SeverityWarn means a noticeable drift (warn..high band).
	SeverityWarn DriftSeverity = "warn"
	// SeverityHigh means a large drift (> highThresholdPct) — the daily job
	// opens/updates an issue on any high finding.
	SeverityHigh DriftSeverity = "high"
	// SeverityNoReference means the family's LiteLLM reference id (or a
	// specific priced field) is absent from the dataset, so the price could
	// not be cross-checked. Not a drift, but a provenance gap worth surfacing
	// so the operator knows the alarm is blind for that family/field.
	SeverityNoReference DriftSeverity = "no_reference"
)

// Tolerance bands (ADR-044 decision 1): ignore sub-2% float/rounding noise,
// warn above 2%, escalate above 10%.
const (
	warnThresholdPct = 2.0
	highThresholdPct = 10.0
)

// familyToLiteLLM maps each familyRates family prefix to the LiteLLM dataset
// key used as its price reference — the current-flagship bare id for that
// family (no bedrock/azure/vertex provider prefix). OPERATOR-MAINTAINED: when
// a family's flagship id advances (e.g. claude-opus-4-7 -> claude-opus-4-8),
// point it at the new id so the alarm keeps tracking the live price. A family
// whose reference id is absent from the pinned dataset reports SeverityNoReference
// rather than a false drift.
//
// Every key here MUST be a key in familyRates; a missing/extra key fails
// TestDriftReferenceMapMatchesFamilies.
var familyToLiteLLM = map[string]string{
	"claude-opus":   "claude-opus-4-7",
	"claude-sonnet": "claude-sonnet-4-6",
	"claude-haiku":  "claude-haiku-4-5",
	"gpt-5.5":       "gpt-5.5",
}

// DriftFinding is one priced-field comparison between our table and LiteLLM.
type DriftFinding struct {
	Family           string        `json:"family"`
	LiteLLMID        string        `json:"litellm_id"`
	Field            string        `json:"field"` // input | output | cache_read | cache_write
	OursPerMillion   float64       `json:"ours_per_million"`
	TheirsPerMillion float64       `json:"theirs_per_million"` // 0 when SeverityNoReference
	PctDelta         float64       `json:"pct_delta"`          // (ours-theirs)/theirs*100; signed
	Severity         DriftSeverity `json:"severity"`
	Direction        string        `json:"direction"` // ours_lower (under-billing risk) | ours_higher (over-reporting) | n/a
}

// DriftReport is the full cross-check outcome.
type DriftReport struct {
	SourceSHA      string         `json:"source_sha"`       // pinned LiteLLM commit (provenance)
	GeneratedAtUTC string         `json:"generated_at_utc"` // stamp from the caller (tests/cmd pass it; package never reads the clock)
	PricingAsOf    string         `json:"pricing_as_of"`    // pricing.AsOf at check time
	CheckedFields  int            `json:"checked_fields"`
	OKFields       int            `json:"ok_fields"`
	Findings       []DriftFinding `json:"findings"` // warn / high / no_reference only (ok omitted)
}

// HighSeverity reports whether any finding is SeverityHigh — the daily job's
// open-an-issue trigger.
func (r DriftReport) HighSeverity() bool {
	for _, f := range r.Findings {
		if f.Severity == SeverityHigh {
			return true
		}
	}
	return false
}

// HasDrift reports whether any finding is actual drift (warn or high),
// EXCLUDING no_reference provenance gaps. The healthy baseline carries a
// persistent no_reference finding (e.g. gpt-5.5 cache_write, which LiteLLM
// leaves null), so the daily job gates its "comment on drift" path on this
// rather than on len(Findings) > 0 to avoid a daily false alarm.
func (r DriftReport) HasDrift() bool {
	for _, f := range r.Findings {
		if f.Severity == SeverityWarn || f.Severity == SeverityHigh {
			return true
		}
	}
	return false
}

// litellmEntry is the subset of a LiteLLM model record we price-check.
// Pointers distinguish "field absent/null" (no reference) from "0".
type litellmEntry struct {
	InputCostPerToken           *float64 `json:"input_cost_per_token"`
	OutputCostPerToken          *float64 `json:"output_cost_per_token"`
	CacheReadInputTokenCost     *float64 `json:"cache_read_input_token_cost"`
	CacheCreationInputTokenCost *float64 `json:"cache_creation_input_token_cost"`
}

// CheckDrift compares the familyRates table against the LiteLLM price dataset
// and returns a report. datasetJSON is the raw model_prices_and_context_window.json
// body; sourceSHA and generatedAtUTC are recorded for provenance (the package
// never reads the clock — the caller stamps the time). It never errors on a
// drift; it returns an error only when datasetJSON is unparseable.
func CheckDrift(datasetJSON []byte, sourceSHA, generatedAtUTC string) (DriftReport, error) {
	// LiteLLM's file has non-model keys (e.g. "sample_spec") and entries with
	// no cost fields; decoding into our subset tolerates them (absent → nil).
	var dataset map[string]litellmEntry
	if err := json.Unmarshal(datasetJSON, &dataset); err != nil {
		return DriftReport{}, fmt.Errorf("pricing: parse LiteLLM dataset: %w", err)
	}

	report := DriftReport{
		SourceSHA:      sourceSHA,
		GeneratedAtUTC: generatedAtUTC,
		PricingAsOf:    AsOf,
	}

	// Deterministic order (map iteration is randomized).
	families := make([]string, 0, len(familyRates))
	for fam := range familyRates {
		families = append(families, fam)
	}
	sort.Strings(families)

	for _, fam := range families {
		ours := familyRates[fam]
		refID := familyToLiteLLM[fam]
		entry, found := dataset[refID]
		if refID == "" || !found {
			report.Findings = append(report.Findings, DriftFinding{
				Family:    fam,
				LiteLLMID: refID,
				Field:     "*",
				Severity:  SeverityNoReference,
				Direction: "n/a",
			})
			continue
		}
		for _, fc := range []struct {
			field  string
			ours   float64  // per-token
			theirs *float64 // per-token, nil = absent
		}{
			{"input", ours.inputPerToken, entry.InputCostPerToken},
			{"output", ours.outputPerToken, entry.OutputCostPerToken},
			{"cache_read", ours.cacheReadPerToken, entry.CacheReadInputTokenCost},
			{"cache_write", ours.cacheWritePerToken, entry.CacheCreationInputTokenCost},
		} {
			f := compareField(fam, refID, fc.field, fc.ours, fc.theirs)
			switch f.Severity {
			case SeverityOK:
				report.CheckedFields++
				report.OKFields++
			case SeverityNoReference:
				// A field we price but LiteLLM does not (e.g. gpt-5.5 cache_write,
				// which LiteLLM leaves null). Surface only when we actually price
				// it (ours > 0); an unpriced-both field is silently skipped.
				if fc.ours > 0 {
					report.Findings = append(report.Findings, f)
				}
			default:
				report.CheckedFields++
				report.Findings = append(report.Findings, f)
			}
		}
	}
	return report, nil
}

// compareField classifies one per-token field pair into a finding (values
// surfaced as $/1M for readability).
func compareField(family, litellmID, field string, oursPerToken float64, theirsPerToken *float64) DriftFinding {
	const perMillion = 1_000_000.0
	f := DriftFinding{
		Family:         family,
		LiteLLMID:      litellmID,
		Field:          field,
		OursPerMillion: oursPerToken * perMillion,
		Direction:      "n/a",
	}
	if theirsPerToken == nil || *theirsPerToken == 0 {
		// No comparable reference value for this field.
		f.Severity = SeverityNoReference
		return f
	}
	f.TheirsPerMillion = *theirsPerToken * perMillion
	f.PctDelta = (oursPerToken - *theirsPerToken) / *theirsPerToken * 100
	abs := math.Abs(f.PctDelta)
	switch {
	case abs > highThresholdPct:
		f.Severity = SeverityHigh
	case abs > warnThresholdPct:
		f.Severity = SeverityWarn
	default:
		f.Severity = SeverityOK
	}
	switch {
	case oursPerToken < *theirsPerToken:
		f.Direction = "ours_lower" // under-billing risk: we charge less than the reference
	case oursPerToken > *theirsPerToken:
		f.Direction = "ours_higher" // over-reporting: we charge more than the reference
	}
	return f
}

// Markdown renders the report as an issue body for the daily drift job.
func (r DriftReport) Markdown() string {
	var b strings.Builder
	// A no_reference finding is a provenance gap, not drift — only warn/high
	// findings count as actual drift in the verdict line.
	driftFindings, noRef := 0, 0
	for _, f := range r.Findings {
		if f.Severity == SeverityNoReference {
			noRef++
		} else {
			driftFindings++
		}
	}
	verdict := "✅ no drift above the warn band"
	switch {
	case r.HighSeverity():
		verdict = "🔴 high-severity drift — review the pricing table"
	case driftFindings > 0:
		verdict = "⚠️ drift in the warn band (no high-severity)"
	case noRef > 0:
		verdict = fmt.Sprintf("✅ no drift; %d field(s) unverifiable (no reference)", noRef)
	}
	fmt.Fprintf(&b, "## Price drift-check — %s\n\n", verdict)
	fmt.Fprintf(&b, "- pricing table `AsOf`: `%s`\n", r.PricingAsOf)
	fmt.Fprintf(&b, "- LiteLLM dataset SHA: `%s`\n", r.SourceSHA)
	fmt.Fprintf(&b, "- generated: `%s`\n", r.GeneratedAtUTC)
	fmt.Fprintf(&b, "- checked %d field(s), %d within tolerance (<%.0f%%)\n\n",
		r.CheckedFields, r.OKFields, warnThresholdPct)
	if len(r.Findings) == 0 {
		b.WriteString("No drift above the warn band. The manual table matches the LiteLLM reference.\n")
		return b.String()
	}
	b.WriteString("| family | litellm id | field | ours $/1M | litellm $/1M | Δ% | severity | direction |\n")
	b.WriteString("|---|---|---|---:|---:|---:|---|---|\n")
	for _, f := range r.Findings {
		theirs := fmt.Sprintf("%.4g", f.TheirsPerMillion)
		delta := fmt.Sprintf("%+.1f%%", f.PctDelta)
		if f.Severity == SeverityNoReference {
			theirs, delta = "—", "—"
		}
		fmt.Fprintf(&b, "| %s | `%s` | %s | %.4g | %s | %s | %s | %s |\n",
			f.Family, f.LiteLLMID, f.Field, f.OursPerMillion, theirs, delta, f.Severity, f.Direction)
	}
	b.WriteString("\n`ours_lower` = under-billing risk (we charge less than the reference); ")
	b.WriteString("`ours_higher` = over-reporting; `no_reference` = the dataset does not price this field, so the alarm is blind there.\n")
	return b.String()
}
