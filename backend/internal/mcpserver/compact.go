package mcpserver

import (
	"encoding/json"
	"fmt"
	"reflect"
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

// elidedReviewProseMarker is the visible stand-in stripReviewProse writes in
// place of a NON-EMPTY review free_form or concern note on the compact-default
// get_run_status path (#3043). Before, the field was blanked to "",
// INDISTINGUISHABLE from a concern that genuinely carries no note — the elision
// the operator on #3043 mistook for an empty backend note. The marker makes the
// elision legible and names the surfaces that restore the full text. It is set
// ONLY when the field was non-empty, so a genuinely-empty note still reads as
// empty: the two states an operator must be able to tell apart stay distinct.
const elidedReviewProseMarker = "…(elided; full text via fishhawk_get_gate_view, or include_review_prose=true)"

// concernNoteCapBytes is the ENCODED-length cap a concern note is truncated to
// on a DEDUPED get_run_status response (E45.92 / #3627). It is a floor on
// usefulness — roughly two sentences of a concern note — not a derived optimum;
// the byte-breakdown test records the actual totals so a later tuning pass has
// evidence. Capping is on the escape-exact encoded length via bound.go's
// capJSONString, never a raw-byte cut.
const concernNoteCapBytes = 240

// cappedNoteMarker is appended to a note whose full text exceeded the cap on a
// deduped response. It names the two full-prose surfaces, mirroring
// elidedReviewProseMarker. A note WITHIN the cap is returned byte-identical and
// carries NO marker, so a capped note and an uncapped one are distinguishable.
const cappedNoteMarker = "…(capped; full note via fishhawk_get_gate_view, or include_review_prose=true)"

// implementReviewsElidedNote is the fixed wire note dedupImplementReviews sets on
// GetRunStatusOutput.ImplementReviewsElided when it drops the redundant flat
// implement_reviews listing (E45.92 / #3627). It explains the omission so it is
// legible rather than silent (the #3043 lesson). The full contract — including
// that a deduped response carries capped rather than content-free notes ONLY
// when the freed bytes cover the prefixes — lives in the field's jsonschema
// description and the package README; the wire note stays concise so it does not
// itself eat the bytes the dedup frees.
const implementReviewsElidedNote = "omitted; every row is present in implement_review_status.reviews (full prose via fishhawk_get_gate_view or include_review_prose=true)"

// projectedConcernNote returns the projected form of a NON-EMPTY concern note on
// the compact-default path. When capNotes is false it is today's content-free
// marker. When capNotes is true a note within concernNoteCapBytes of encoded
// length is returned byte-identical (no marker); a longer note is capped to a
// rune-boundary encoded prefix plus cappedNoteMarker. capNotes true is reached
// ONLY on a deduped implement_review_status.reviews render whose freed bytes
// cover the added prefixes (dedupImplementReviews decides), so no response grows.
func projectedConcernNote(note string, capNotes bool) string {
	if !capNotes {
		return elidedReviewProseMarker
	}
	if jsonEncodedLen(note) <= concernNoteCapBytes {
		return note
	}
	return capJSONString(note, concernNoteCapBytes) + cappedNoteMarker
}

// stripReviewProse replaces the free-text fields of each typed review IN PLACE:
// the review-level free_form and every concern's note. free_form is ALWAYS
// replaced by elidedReviewProseMarker when non-empty (unaffected by capNotes).
// For a concern note, capNotes selects the projection: false is today's
// content-free marker (byte-for-byte the pre-#3627 behaviour); true is a capped
// prefix (via projectedConcernNote), used ONLY at the deduped
// implement_review_status.reviews call site. A field that was ALREADY empty is
// left empty on both paths — the marker/prefix is written only over non-empty
// text, so an elided note and a genuinely-empty one remain distinguishable
// (#3043). Everything an operator gates on — verdict, authority, reviewer_kind,
// reviewer_model, reason, and each concern's severity/category (the "concern
// keys") — is left intact.
func stripReviewProse(reviews []PlanReview, capNotes bool) {
	for i := range reviews {
		if reviews[i].FreeForm != "" {
			reviews[i].FreeForm = elidedReviewProseMarker
		}
		for j := range reviews[i].Concerns {
			if reviews[i].Concerns[j].Note != "" {
				reviews[i].Concerns[j].Note = projectedConcernNote(reviews[i].Concerns[j].Note, capNotes)
			}
		}
	}
}

// implementReviewsContained reports whether every element of flat has a DISTINCT
// DeepEqual partner in rich (MULTISET containment): two byte-identical flat rows
// each need their own partner, and ordering differences between
// loadImplementReviews' (reviewed+skipped) order and reviewRound.LandedRows'
// (reviewed+failed+skipped) order do not defeat the match. It returns FALSE for
// an empty rich slice — a none/pending status carries no rows, so nothing is
// duplicated and the flat listing is the only verdict surface there. (An empty
// flat slice matches vacuously here; dedupImplementReviews's size guard then
// refuses the fire, so an empty flat listing never gains a spurious elision.)
func implementReviewsContained(flat, rich []PlanReview) bool {
	if len(rich) == 0 {
		return false
	}
	used := make([]bool, len(rich))
	for i := range flat {
		matched := false
		for k := range rich {
			if used[k] {
				continue
			}
			if reflect.DeepEqual(flat[i], rich[k]) {
				used[k] = true
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

// jsonFieldBytes returns the marshalled wire cost of ONE response field —
// `"key":value` without the surrounding object braces or a separating comma. It
// is used by dedupImplementReviews's size accounting to compare the bytes a
// dropped field frees against the bytes the projection adds.
func jsonFieldBytes(key string, value any) int {
	raw, err := json.Marshal(map[string]any{key: value})
	if err != nil {
		return 0
	}
	return len(raw) - 2 // drop the `{` and `}`
}

// dedupImplementReviews drops the flat implement_reviews listing when it is
// provably CONTAINED in implement_review_status.reviews — the exact condition
// under which it carries nothing new (E45.92 / #3627) — replacing it with the
// legible ImplementReviewsElided note. It ALSO decides, by an ENFORCED size
// accounting, whether the deduped implement_review_status.reviews notes may
// carry capped prefixes instead of the content-free marker: the returned
// capNotes is true ONLY when the bytes freed by dropping the flat listing cover
// BOTH the elision note AND the added prefixes, so no response can ever grow.
//
// It fires (returns fired=true) only when ALL hold:
//   - out.ImplementReviewStatus != nil          (nil-deref guard)
//   - implementReviewsContained(flat, rich)      (rich carries every flat row;
//     false for an empty rich, so a none/pending status never fires; an empty
//     flat matches vacuously and is refused by the size guard below)
//   - saved >= costElided                        (the freed bytes cover even the
//     omission explanation; otherwise the response would grow, so leave the tiny
//     duplication — or an empty listing — byte-for-byte in place)
//
// On a fire it sets out.ImplementReviews = nil (json omitempty drops it from the
// wire) and out.ImplementReviewsElided, and returns capNotes.
func dedupImplementReviews(out *GetRunStatusOutput) (fired bool, capNotes bool) {
	if out.ImplementReviewStatus == nil {
		return false, false
	}
	flat := out.ImplementReviews
	rich := out.ImplementReviewStatus.Reviews
	// An empty flat listing needs no dedicated guard: implementReviewsContained
	// matches it vacuously, but the size guard below then measures a freed size of
	// ~22 bytes (an empty array under its key) against the ~150-byte elision note
	// and refuses to fire — so an empty implement_reviews never gains a spurious
	// elision note. (Deleting the size guard reddens both the minimal-row and the
	// empty-flat cases in TestDedupImplementReviews.)
	if !implementReviewsContained(flat, rich) {
		return false, false
	}

	// saved = today's wire contribution of the flat listing, which ships
	// marker-elided. Measured on a marker-elided DEEP COPY so the saving is the
	// real freed bytes, never the (larger) unstripped size — using the unstripped
	// size would let the cap look affordable when it is not.
	elidedFlat := deepCopyReviews(flat)
	stripReviewProse(elidedFlat, false)
	saved := jsonFieldBytes("implement_reviews", elidedFlat)

	// costElided = the bytes the ImplementReviewsElided field adds (+1 for its
	// field separator, so the added side is not under-counted).
	costElided := jsonFieldBytes("implement_reviews_elided", implementReviewsElidedNote) + 1
	if saved < costElided {
		return false, false
	}

	out.ImplementReviews = nil
	out.ImplementReviewsElided = implementReviewsElidedNote

	// costCap = the extra bytes capping the rich notes adds over today's
	// content-free markers. Enable the prefixes only when the freed bytes cover
	// the explanation AND the prefixes, so the deduped response is never larger
	// than today's.
	costCap := richNoteCapDelta(rich)
	return true, saved >= costElided+costCap
}

// deepCopyReviews copies a review slice giving each review its OWN Concerns
// slice, so stripping the copy cannot mutate the original's shared backing array.
func deepCopyReviews(in []PlanReview) []PlanReview {
	out := make([]PlanReview, len(in))
	for i := range in {
		out[i] = in[i]
		if in[i].Concerns != nil {
			out[i].Concerns = make([]PlanReviewConcern, len(in[i].Concerns))
			copy(out[i].Concerns, in[i].Concerns)
		}
	}
	return out
}

// richNoteCapDelta is the total added encoded bytes if every non-empty rich
// concern note were capped (projectedConcernNote with capNotes=true) instead of
// marker-elided. It can be negative — a note shorter than the marker costs fewer
// bytes capped — which correctly makes the cap even more affordable.
func richNoteCapDelta(rich []PlanReview) int {
	markerLen := jsonEncodedLen(elidedReviewProseMarker)
	delta := 0
	for i := range rich {
		for j := range rich[i].Concerns {
			note := rich[i].Concerns[j].Note
			if note == "" {
				continue
			}
			delta += jsonEncodedLen(projectedConcernNote(note, true)) - markerLen
		}
	}
	return delta
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
// (#1749): post-#1727/#1744 the oversized free-text payload strings (a review
// free_form, an issue body/comment, a routed-concern note) are the residual
// per-entry bulk this lever removes. Sized to keep the audit_limit=10 default
// snapshot under the ~7KB done-means target (the
// TestGetRunStatus_CompactDefault_UnderSizeBudget backstop); lowered from the
// initial ~256, then to 96, to bite harder on genuine prose payloads. The full,
// untruncated value always remains available via fishhawk_list_audit and is
// restored on get_run_status by include_audit_hashes.
//
// NOTE (scope boundary): this cap only shrinks oversized *string* values. A
// run whose recent 10 audit entries are small-payload structural categories
// (cost_recorded, trace_uploaded, policy_evaluated, *_precheck/_sweep) carries
// almost no cappable string bulk, so its default response floor is set by (a)
// the retained operator-playbook non-audit fields (next_actions, stages,
// drive_status, cache_efficiency, cost, budget — ~6.5KB, retained by the
// compaction contract) plus (b) the irreducible per-entry audit structure
// (id/run_id/ts/sequence/actor ≈ 280B/entry). Those two floors are outside the
// recent_audit-string remit, so on such a run no cap value reaches ~7KB;
// compacting them is a separate follow-up (see #1749 acceptance notes).
const auditPayloadStringCap = 96

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
