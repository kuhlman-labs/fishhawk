package main

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
