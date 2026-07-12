package main

import (
	"fmt"
	"unicode/utf8"
)

// This file holds the pure projection helpers shared by
// fishhawk_get_run_status and fishhawk_await_audit to make their default
// responses compact (#1727). Both surfaces carry oversized free-text —
// the issue body + every comment (issue_context) and reviewer free-text
// prose (free_form + per-concern notes) — that dominates the tool-result
// token footprint while contributing nothing an operator acts on. The
// projection is applied server-side BEFORE serialization (mirroring the
// fishhawk_list_runs include_issue_context idiom), so the heavy text is
// stripped from the marshalled payload, not merely hidden client-side.
// Two opt-in flags restore today's full shape.

// stripReviewProse clears the free-text fields from each typed implement
// review IN PLACE: the review-level free_form and every concern's note.
// Everything an operator gates on — verdict, authority, reviewer_kind,
// reviewer_model, reason, and each concern's severity/category (the
// "concern keys") — is left intact. Called on the default (no
// include_review_prose) get_run_status path.
func stripReviewProse(reviews []PlanReview) {
	for i := range reviews {
		reviews[i].FreeForm = ""
		for j := range reviews[i].Concerns {
			reviews[i].Concerns[j].Note = ""
		}
	}
}

// compactFreeTextKeys is the narrow, documented denylist of oversized
// free-text keys compactAuditPayload removes from an untyped audit
// payload. It is deliberately small: only reviewer free-text prose
// ("free_form") and the issue-context body + comments ("body",
// "comments"). Verdict / severity / category / concern keys are never in
// the denylist, so they always survive the projection. Each key is gated
// by its own flag below.
const (
	freeTextKeyFreeForm = "free_form"
	freeTextKeyBody     = "body"
	freeTextKeyComments = "comments"
)

// compactAuditPayload returns a copy of an untyped audit payload with the
// oversized free-text keys projected out. When dropReviewProse is set the
// "free_form" key is removed; when dropIssueContext is set the "body" and
// "comments" keys are removed. It descends recursively into nested maps
// and slices of maps so a review payload's nested prose and an
// issue-context payload's body/comments are both stripped no matter how
// deep they are nested (a shallow top-level-only strip would leave prose
// behind). The input is returned unchanged when it is nil, not a map, or
// when neither flag is set (nothing to strip). Verdict/severity/category
// keys are never in the denylist, so they always survive.
func compactAuditPayload(payload any, dropIssueContext, dropReviewProse bool) any {
	if !dropIssueContext && !dropReviewProse {
		return payload
	}
	return compactValue(payload, dropIssueContext, dropReviewProse)
}

// compactValue recursively strips the denylisted keys from a decoded-JSON
// value: on a map it drops the flagged keys and recurses into the
// remaining values; on a slice it recurses into each element; any other
// value (string, number, bool, nil) is returned unchanged.
func compactValue(v any, dropIssueContext, dropReviewProse bool) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			if dropReviewProse && k == freeTextKeyFreeForm {
				continue
			}
			if dropIssueContext && (k == freeTextKeyBody || k == freeTextKeyComments) {
				continue
			}
			out[k] = compactValue(val, dropIssueContext, dropReviewProse)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i := range t {
			out[i] = compactValue(t[i], dropIssueContext, dropReviewProse)
		}
		return out
	default:
		return v
	}
}

// auditPayloadStringCap is the default byte cap on a single string value in a
// recent_audit entry's payload. A value longer than this is truncated by
// truncateAuditPayloadStrings on the compact-by-default get_run_status path
// (#1749): post-#1727/#1744 the surviving payload objects (untruncated) are the
// residual bulk that dominates the default response. Sized to keep the
// audit_limit=10 default snapshot under the ~7KB done-means target (the
// TestGetRunStatus_CompactDefault_UnderSizeBudget backstop); lowered from the
// initial ~256 so the representative fixture clears the target. The full,
// untruncated value always remains available via fishhawk_list_audit and is
// restored on get_run_status by include_audit_hashes.
const auditPayloadStringCap = 160

// truncateAuditPayloadStrings returns a copy of a decoded-JSON audit payload
// with every oversized string value truncated to a rune-safe prefix plus a
// stable marker pointing at fishhawk_list_audit. It descends recursively into
// nested maps and slices (mirroring compactValue's descent shape) so a string
// buried under any nesting is truncated. The input is returned unchanged when
// cap <= 0 or when it is nil / a bare scalar / a bare string (payloads are
// always maps, so a bare top-level string is never truncated — only values
// reached by descending into a map or slice are). Non-string scalars pass
// through untouched.
func truncateAuditPayloadStrings(payload any, cap int) any {
	if cap <= 0 {
		return payload
	}
	switch payload.(type) {
	case map[string]any, []any:
		return truncateValue(payload, cap)
	default:
		return payload
	}
}

// truncateValue is the recursive worker for truncateAuditPayloadStrings: it
// truncates any oversized string it reaches, recurses into maps and slices,
// and returns every other value (number, bool, nil) unchanged.
func truncateValue(v any, cap int) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = truncateValue(val, cap)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i := range t {
			out[i] = truncateValue(t[i], cap)
		}
		return out
	case string:
		return truncateOversizedString(t, cap)
	default:
		return v
	}
}

// truncateOversizedString truncates s to at most cap bytes on a UTF-8 rune
// boundary (backing off with utf8.DecodeLastRuneInString so the result is never
// invalid UTF-8) and appends a stable marker reporting the elided byte count and
// where to read the full value. A string already within cap is returned as-is.
func truncateOversizedString(s string, cap int) string {
	if len(s) <= cap {
		return s
	}
	prefix := s[:cap]
	// Back off to a valid rune boundary: a cut that landed mid-rune leaves
	// DecodeLastRuneInString returning RuneError with size 1 — trim those
	// stray bytes until the final rune decodes cleanly.
	for len(prefix) > 0 {
		r, size := utf8.DecodeLastRuneInString(prefix)
		if r == utf8.RuneError && size <= 1 {
			prefix = prefix[:len(prefix)-1]
			continue
		}
		break
	}
	elided := len(s) - len(prefix)
	return prefix + fmt.Sprintf("…(+%d bytes; full value via fishhawk_list_audit)", elided)
}

// collapseCacheEfficiencyStages drops the per-stage breakdown from a
// cache_efficiency block IN PLACE, leaving only the run-level rollup scalars.
// Called on the default get_run_status path (#1749); the Stages field carries
// json:"stages,omitempty", so a nil slice drops the field from the wire. Nil-safe.
func collapseCacheEfficiencyStages(ce *CacheEfficiency) {
	if ce == nil {
		return
	}
	ce.Stages = nil
}
