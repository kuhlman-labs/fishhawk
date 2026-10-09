// Package planreview defines the verdict types, authority resolution
// logic, and audit payload for plan-review agents (ADR-027).
//
// Authority modes control whether agent verdicts can block stage
// advancement (gating) or are surfaced for human consumption only
// (advisory). The three modes are derived from the stage's
// ReviewersConfig via ResolveAuthority.
package planreview

import (
	"strings"

	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// Verdict is the review agent's conclusion on a plan artifact.
// The closed set matches the verdict JSON schema emitted by the
// review-agent prompt.
type Verdict string

// Closed verdict set per ADR-027.
const (
	VerdictApprove             Verdict = "approve"
	VerdictApproveWithConcerns Verdict = "approve_with_concerns"
	VerdictReject              Verdict = "reject"
)

// AllVerdicts is the single source of truth for the closed Verdict set
// (#1324). It is the ONE list both VerdictSchema()'s `verdict` enum array and
// the Verdict.Valid validation path derive from — every reviewer adapter
// validates through Verdict.Valid rather than a private hand-maintained map, so
// adding a Verdict constant without also listing it here is impossible to do
// silently: the new constant would be absent from BOTH the schema enum and the
// accepted set together, and the schema drift test asserts this list equals the
// schema enum. The ordering is the canonical approve→reject ordering and is the
// order emitted into the schema enum.
var AllVerdicts = []Verdict{
	VerdictApprove,
	VerdictApproveWithConcerns,
	VerdictReject,
}

// Valid reports whether v is a member of the closed AllVerdicts set. It is the
// canonical verdict-validation path the reviewer adapters consume (#1324),
// replacing their previously per-adapter validVerdicts maps so there is exactly
// one accepted-verdict list, shared with VerdictSchema().
func (v Verdict) Valid() bool {
	for _, known := range AllVerdicts {
		if v == known {
			return true
		}
	}
	return false
}

// ConcernSeverity classifies the weight of a reviewer concern.
type ConcernSeverity string

// Concern severity levels per ADR-027 / #560 issue body.
const (
	SeverityHigh   ConcernSeverity = "high"
	SeverityMedium ConcernSeverity = "medium"
	SeverityLow    ConcernSeverity = "low"
)

// AllConcernSeverities is the single source of truth for the closed
// ConcernSeverity set (#1324), consumed by VerdictSchema()'s concern `severity`
// enum for the same no-drift reason as AllVerdicts. The ordering is the
// canonical high→low ordering and is the order emitted into the schema enum.
var AllConcernSeverities = []ConcernSeverity{
	SeverityHigh,
	SeverityMedium,
	SeverityLow,
}

// Valid reports whether s is a member of the closed AllConcernSeverities set.
func (s ConcernSeverity) Valid() bool {
	for _, known := range AllConcernSeverities {
		if s == known {
			return true
		}
	}
	return false
}

// Concern is one flagged issue within a review verdict.
// Severity drives how the concern is surfaced to the operator;
// Category is a short classifier (e.g. "scope", "security");
// Note is the reviewer's free-form explanation.
type Concern struct {
	Severity ConcernSeverity `json:"severity"`
	Category string          `json:"category"`
	Note     string          `json:"note"`

	// SuggestedPatch optionally carries a unified diff that applies to the
	// PR branch and mechanically resolves the concern (E22.X / #1165).
	// Implement reviewers populate it ONLY for mechanical concerns whose
	// fix is a small self-contained diff; it is the input to the
	// near-deterministic fix-up apply path. omitempty keeps reviewer
	// output predating the field byte-identical, and encoding/json ignores
	// the absent member in both directions so old verdicts decode unchanged.
	SuggestedPatch string `json:"suggested_patch,omitempty"`

	// Provenance is a SERVER-INTERNAL marker recording that this concern was
	// synthesized from an ADR-050-attacker-influenced source rather than
	// authored by an operator or a review agent (ADR-050 / E31.8 / #1613).
	// When set to ConcernProvenanceAcceptance the fix-up prompt renderer
	// routes the concern's free-text through the sanitizeUntrustedComment
	// quarantine envelope (structure-neutralized, DATA-not-instructions
	// framing) instead of the trusted MANDATORY / win-on-conflict framing,
	// closing the injected-acceptance-agent -> binding-implement-instruction
	// chain. It is NEVER populated by a review agent — VerdictSchema()
	// deliberately omits `provenance`, so a reviewer cannot smuggle it in via
	// the closed (additionalProperties:false) verdict schema; it is stamped
	// only server-side at synthesis. omitempty keeps every reviewer-emitted
	// verdict and every already-persisted concern byte-identical, and
	// encoding/json ignores the absent member in both directions so a concern
	// without provenance decodes with the zero value (empty string) and
	// renders on the unchanged trusted path.
	Provenance string `json:"provenance,omitempty"`

	// SettledRef optionally echoes the stable id of a concern listed in the
	// implement-review prompt's "Settled concerns" ledger (#1913) that this
	// concern re-raises. It is the lineage tag the deterministic server-side
	// re-litigation guard keys on: a concern whose SettledRef resolves to a
	// same-run/same-stage WAIVED or DEFERRED concern and whose NewEvidence is
	// empty is recorded as a concern_relitigation_suppressed audit entry rather
	// than minted as a fresh open concern row — closing the round-over-round
	// churn where a reviewer re-raises an operator-arbitrated finding reworded.
	// A re-raise of an ADDRESSED or SUPERSEDED ledger entry stays insertable
	// (a genuine regression of an addressed fix must reach the operator); the
	// tag is still requested for lineage. omitempty keeps reviewer output
	// predating the field byte-identical, and encoding/json ignores the absent
	// member in both directions so old verdicts decode with an empty value.
	SettledRef string `json:"settled_ref,omitempty"`

	// NewEvidence optionally carries the reviewer's justification that a
	// re-raise of a settled concern (SettledRef) rests on genuinely new
	// evidence rather than re-litigating the operator's arbitration (#1913).
	// A non-empty NewEvidence disarms the re-litigation guard for that
	// concern — it falls open to the normal insert so a real regression is
	// never discarded. omitempty keeps pre-#1913 verdicts byte-identical.
	NewEvidence string `json:"new_evidence,omitempty"`

	// Convention names the repository review convention (E55.3 / #2244) a
	// RepoConventionConcernCategory concern was derived from. It is
	// REVIEWER-EMITTABLE (registered in VerdictSchema()) and is the key
	// ClampConventionSeverities looks the convention's severity_cap up by; an
	// empty or unknown name fails closed to the most restrictive rendered cap.
	// omitempty keeps every convention-free verdict byte-identical, and an old
	// stored concern decodes with an empty value.
	Convention string `json:"convention,omitempty"`

	// SeverityClampedFrom is a SERVER-INTERNAL marker recording the severity a
	// reviewer originally assigned before ClampConventionSeverities lowered it
	// to a convention's severity_cap (E55.3 / #2244). It is stamped ONLY by
	// the clamp and is deliberately absent from VerdictSchema(), so a
	// schema-constrained reviewer cannot emit it. omitempty keeps every
	// unclamped concern byte-identical.
	SeverityClampedFrom ConcernSeverity `json:"severity_clamped_from,omitempty"`

	// QuotedPassage and DocumentRef (E55.10 / #3755) are REVIEWER-EMITTABLE
	// (registered in VerdictSchema()): the exact passage a reviewer persona
	// quotes from a document injected into its prompt, and that document's
	// Source path. The server verifies the quote at ingest against the text it
	// actually injected into that invocation (VerifyQuotedPassages); an
	// unverified quote demotes the concern to low. omitempty keeps every
	// quote-free verdict byte-identical.
	QuotedPassage string `json:"quoted_passage,omitempty"`
	DocumentRef   string `json:"document_ref,omitempty"`

	// QuoteUnverified, QuoteVerifiedContentHash, PersonaSeverityCap and
	// ReviewerRole are SERVER-INTERNAL ingest markers (E55.10 / #3755),
	// deliberately absent from VerdictSchema() and zeroed on every verdict by
	// ClearPersonaIngestMarkers before the server stamps them, so a reviewer on
	// the unconstrained decode path can never pre-set them:
	//
	//   - QuoteUnverified: the quoted passage was NOT found in the named
	//     injected document (VerifyQuotedPassages demoted the concern).
	//   - QuoteVerifiedContentHash: the content_hash of the injected document
	//     the quote WAS found in.
	//   - PersonaSeverityCap: the persona remit's severity_cap the concern was
	//     clamped to (ClampPersonaSeverities).
	//   - ReviewerRole: the persona name, or the standard role, of the reviewer
	//     that raised the concern.
	//
	// omitempty keeps every untouched concern byte-identical.
	QuoteUnverified          bool            `json:"quote_unverified,omitempty"`
	QuoteVerifiedContentHash string          `json:"quote_verified_content_hash,omitempty"`
	PersonaSeverityCap       ConcernSeverity `json:"persona_severity_cap,omitempty"`
	ReviewerRole             string          `json:"reviewer_role,omitempty"`
}

// ConcernProvenanceAcceptance marks a Concern synthesized from the acceptance
// agent's attacker-influenceable free-text verdict (the E31.8 class-1
// acceptance-failure triage path). A concern carrying this provenance is
// rendered by the implement fix-up prompt through the untrusted-comment
// quarantine envelope rather than as trusted binding fix-up text (#1613).
const ConcernProvenanceAcceptance = "acceptance"

// Review-convention concern categories (E55.3 / #2244). They are reviewer
// CONCERN categories carried inside plan_reviewed / implement_reviewed verdict
// payloads — not audit categories. The *ConcernCategory name suffix is
// load-bearing: it is what exempts these bindings from the audit-category
// registry sweep (backend/internal/audit TestKnownCategoriesCoversEmittedCategories).
const (
	// RepoConventionConcernCategory marks a concern derived from a repository
	// review convention rendered into the review prompt. Its severity is
	// bounded by that convention's severity_cap at ingest
	// (ClampConventionSeverities), keyed by Concern.Convention.
	RepoConventionConcernCategory = "repo_convention"

	// ConventionsOverrideAttemptConcernCategory marks a reviewer report that a
	// conventions file tried to remove, weaken, reorder or override a standard
	// review criterion. It reports an instruction in a file read at the run's
	// pinned base — not a defect in the change — so the fix-up surface refuses
	// to route it (backend/internal/server/fixup.go).
	ConventionsOverrideAttemptConcernCategory = "conventions_override_attempt"

	// ConventionsFileModifiedConcernCategory marks a change that edits a
	// declared conventions file. The implement-review site synthesizes exactly
	// one per round when no reviewer raised it
	// (AddSynthesizedConventionsFileModifiedConcern).
	ConventionsFileModifiedConcernCategory = "conventions_file_modified"
)

// conventionCategory reports which review-convention category a concern's
// free-text category names, matched on its trimmed, lower-cased form so a
// cosmetic variant ("Repo_Convention ", "REPO_CONVENTION") cannot slip past the
// clamp. It returns "" for every other category.
func conventionCategory(category string) string {
	switch c := strings.ToLower(strings.TrimSpace(category)); c {
	case RepoConventionConcernCategory,
		ConventionsOverrideAttemptConcernCategory,
		ConventionsFileModifiedConcernCategory:
		return c
	}
	return ""
}

// IsConventionsOverrideAttempt reports whether a concern category names
// ConventionsOverrideAttemptConcernCategory, using the same normalized match the
// clamp applies. The fix-up surface consults it to refuse routing such a
// concern as a code obligation.
func IsConventionsOverrideAttempt(category string) bool {
	return conventionCategory(category) == ConventionsOverrideAttemptConcernCategory
}

// ConventionCaps is the set of review conventions RENDERED into one review
// round's prompt, keyed by convention name, each mapped to its declared
// severity_cap ("" = uncapped; spec.ReviewConventionSeverityCapLow /
// spec.ReviewConventionSeverityCapMedium otherwise). The key set IS the
// rendered-name set: a name absent from the map was not rendered. A nil or
// empty ConventionCaps means ZERO conventions were rendered for the round,
// which ClampConventionSeverities treats as the most restrictive case of all
// (every convention-category concern clamps to low).
type ConventionCaps map[string]ConcernSeverity

// severityRank orders severities for the clamp: low < medium < high. An
// unknown or empty CONCERN severity ranks as high so it is lowered whenever a
// cap applies (fail closed); capRank handles the cap side separately.
func severityRank(s ConcernSeverity) int {
	switch s {
	case SeverityLow:
		return 1
	case SeverityMedium:
		return 2
	default:
		return 3
	}
}

// capRank orders a declared severity_cap: "" (uncapped) ranks as high (no
// bound); low/medium/high rank as themselves; any other value is not a member
// of the closed cap set and fails closed to low.
func capRank(c ConcernSeverity) int {
	switch c {
	case "", SeverityHigh:
		return 3
	case SeverityMedium:
		return 2
	default:
		return 1
	}
}

// rankSeverity maps a rank back to its severity.
func rankSeverity(r int) ConcernSeverity {
	switch r {
	case 1:
		return SeverityLow
	case 2:
		return SeverityMedium
	default:
		return SeverityHigh
	}
}

// ClampedConcern records one concern ClampConventionSeverities lowered, so the
// ingest site can WARN-log each clamp with the run id and category.
type ClampedConcern struct {
	// Index is the concern's position in ReviewVerdict.Concerns.
	Index int
	// Category is the concern's normalized review-convention category.
	Category string
	// Convention is the concern's (trimmed) Convention name, possibly empty.
	Convention string
	// From / To are the reviewer-assigned and clamped severities.
	From, To ConcernSeverity
	// NoConventionsRendered is true when the clamp fired because ZERO
	// conventions were rendered for the round (approval condition 2 of #2244).
	NoConventionsRendered bool
}

// ClampResult is what ClampConventionSeverities changed.
type ClampResult struct {
	// Clamped lists every lowered concern, in concern order.
	Clamped []ClampedConcern
	// VerdictClampedFrom is VerdictReject when the clamp downgraded a reject
	// to approve_with_concerns, "" otherwise. It is the value the ingest site
	// stamps on the payload's verdict_clamped_from.
	VerdictClampedFrom Verdict
}

// ClampConventionSeverities enforces review-convention severity caps on a
// reviewer verdict at INGEST (E55.3 / #2244), mutating v in place (its Concerns
// slice is copied before the first write, so a caller-shared backing array is
// never written). It is the server-side half of the convention contract: the
// prompt tells the reviewer the caps, this makes them hold whatever the model
// emits.
//
// Per concern, matched on the normalized category:
//
//   - caps EMPTY (zero conventions rendered for the round): a repo_convention,
//     conventions_override_attempt or conventions_file_modified concern clamps
//     to low — no rendered convention can justify any weight, so the most
//     restrictive cap applies. It is never left unclamped.
//   - caps non-empty, repo_convention: the cap is caps[Convention] when that
//     name was rendered, else (empty or unknown name) the MOST restrictive
//     rendered cap — fail closed rather than letting a mislabelled finding
//     escape its cap.
//   - caps non-empty, any other category (including override-attempt and
//     file-modified): untouched.
//
// A concern whose severity exceeds its cap is lowered to the cap and stamped
// SeverityClampedFrom with the original severity. A REJECT verdict is
// downgraded to approve_with_concerns only when at least one concern was
// lowered FROM high and no high concern remains — a reject resting on an
// unclamped high, or on no high at all, stands. The clamp is idempotent: a
// second call over its own output lowers nothing.
//
// The synthesized conventions_file_modified concern is server-authored, not a
// reviewer emission: callers MUST clamp BEFORE calling
// AddSynthesizedConventionsFileModifiedConcern so the empty-caps rule never
// lowers it.
//
// It is ClampConventionSeveritiesForRound with conventionsFileModified false.
func ClampConventionSeverities(v *ReviewVerdict, caps ConventionCaps) ClampResult {
	return ClampConventionSeveritiesForRound(v, caps, false)
}

// ClampConventionSeveritiesForRound is ClampConventionSeverities for a review
// round that KNOWS whether the reviewed change modifies a declared conventions
// file (E55.10 / #3755, the #2244 seam). With conventionsFileModified true the
// empty-caps rule EXEMPTS conventions_file_modified concerns: the round has
// independent, server-observed evidence the finding is real (the diff edits a
// declared conventions file), so a reviewer raising it at medium keeps that
// weight even when zero conventions were rendered. Every other rule, and every
// other category, is unchanged; with conventionsFileModified false it is
// byte-identical to the original clamp.
func ClampConventionSeveritiesForRound(v *ReviewVerdict, caps ConventionCaps, conventionsFileModified bool) ClampResult {
	var res ClampResult
	if v == nil || len(v.Concerns) == 0 {
		return res
	}
	noneRendered := len(caps) == 0
	// The most restrictive rendered cap — the fail-closed bound for a
	// repo_convention concern naming no (or an unrendered) convention.
	mostRestrictive := 3
	for _, c := range caps {
		if r := capRank(c); r < mostRestrictive {
			mostRestrictive = r
		}
	}

	copied := false
	loweredFromHigh := false
	for i, c := range v.Concerns {
		cat := conventionCategory(c.Category)
		if cat == "" {
			continue
		}
		name := strings.TrimSpace(c.Convention)
		var bound int
		switch {
		case noneRendered && conventionsFileModified && cat == ConventionsFileModifiedConcernCategory:
			continue
		case noneRendered:
			bound = 1
		case cat != RepoConventionConcernCategory:
			continue
		default:
			if capSev, ok := caps[name]; ok && name != "" {
				bound = capRank(capSev)
			} else {
				bound = mostRestrictive
			}
		}
		if severityRank(c.Severity) <= bound {
			continue
		}
		if !copied {
			v.Concerns = append([]Concern(nil), v.Concerns...)
			copied = true
		}
		to := rankSeverity(bound)
		if c.Severity == SeverityHigh {
			loweredFromHigh = true
		}
		v.Concerns[i].SeverityClampedFrom = c.Severity
		v.Concerns[i].Severity = to
		res.Clamped = append(res.Clamped, ClampedConcern{
			Index:                 i,
			Category:              cat,
			Convention:            name,
			From:                  c.Severity,
			To:                    to,
			NoConventionsRendered: noneRendered,
		})
	}

	res.VerdictClampedFrom = downgradeRejectWithoutHigh(v, loweredFromHigh)
	return res
}

// downgradeRejectWithoutHigh is the shared reject-downgrade rule every ingest
// clamp applies: a REJECT verdict becomes approve_with_concerns only when the
// clamp lowered at least one concern FROM high and no high concern remains. It
// returns VerdictReject when it downgraded (the value stamped on the payload's
// verdict_clamped_from), "" otherwise — a reject resting on an unlowered high,
// or on no high at all, stands.
func downgradeRejectWithoutHigh(v *ReviewVerdict, loweredFromHigh bool) Verdict {
	if v.Verdict != VerdictReject || !loweredFromHigh {
		return ""
	}
	for _, c := range v.Concerns {
		if c.Severity == SeverityHigh {
			return ""
		}
	}
	v.Verdict = VerdictApproveWithConcerns
	return VerdictReject
}

// ClearPersonaIngestMarkers zeroes the four SERVER-INTERNAL persona ingest
// markers — QuoteUnverified, QuoteVerifiedContentHash, PersonaSeverityCap and
// ReviewerRole — on every concern of v (E55.10 / #3755), returning how many
// concerns carried at least one. The ingest site calls it on EVERY verdict
// before stamping, so a reviewer on the unconstrained decode path (which does
// not reject unknown keys) cannot pre-set a marker the server alone may write.
// v.Concerns is copied before the first write, so a caller-shared backing
// array is never written. A nil verdict is a no-op returning 0.
func ClearPersonaIngestMarkers(v *ReviewVerdict) (cleared int) {
	if v == nil {
		return 0
	}
	copied := false
	for i, c := range v.Concerns {
		if !c.QuoteUnverified && c.QuoteVerifiedContentHash == "" && c.PersonaSeverityCap == "" && c.ReviewerRole == "" {
			continue
		}
		if !copied {
			v.Concerns = append([]Concern(nil), v.Concerns...)
			copied = true
		}
		v.Concerns[i].QuoteUnverified = false
		v.Concerns[i].QuoteVerifiedContentHash = ""
		v.Concerns[i].PersonaSeverityCap = ""
		v.Concerns[i].ReviewerRole = ""
		cleared++
	}
	return cleared
}

// QuotedDocument is one document whose text was injected into a reviewer
// invocation's prompt, as VerifyQuotedPassages checks a quote against it
// (E55.10 / #3755). Path is the document's Source path (what a reviewer names
// in document_ref); Commit and ContentHash are the attribution its
// document_injected audit entry recorded; Text is the exact document text the
// reviewer was shown (repodoc.InjectedContent), never the server's framing.
type QuotedDocument struct {
	Path        string
	Commit      string
	ContentHash string
	Text        string
}

// Quote-verification failure modes (E55.10 / #3755): why VerifyQuotedPassages
// demoted a concern. Each is logged by the ingest site with the run id,
// persona and document_ref.
const (
	// QuoteFailureDocumentRefMissing: the concern quotes a passage but names
	// no document_ref.
	QuoteFailureDocumentRefMissing = "document_ref_missing"
	// QuoteFailureDocumentUnknown: document_ref names no document injected
	// into this invocation (including one injected into a DIFFERENT
	// invocation, or withheld).
	QuoteFailureDocumentUnknown = "document_unknown"
	// QuoteFailureDocumentTextUnavailable: the named document was injected but
	// the server holds no text for it (e.g. a withheld notice), so nothing can
	// be verified.
	QuoteFailureDocumentTextUnavailable = "document_text_unavailable"
	// QuoteFailurePassageNotFound: the quote is not a substring of the named
	// document's injected text (whitespace-collapsed, case-sensitive).
	QuoteFailurePassageNotFound = "passage_not_found"
)

// QuoteVerified records one concern whose quote VerifyQuotedPassages found.
type QuoteVerified struct {
	// Index is the concern's position in ReviewVerdict.Concerns.
	Index int
	// DocumentRef is the normalized document_ref the quote was matched in.
	DocumentRef string
	// Commit / ContentHash are the matched document's attribution.
	Commit, ContentHash string
}

// QuoteDemotion records one concern VerifyQuotedPassages marked unverified.
type QuoteDemotion struct {
	// Index is the concern's position in ReviewVerdict.Concerns.
	Index int
	// DocumentRef is the normalized document_ref ("" when missing).
	DocumentRef string
	// Failure is one of the QuoteFailure* modes.
	Failure string
	// From / To are the severities before and after; equal when the concern
	// was already low (marked, not lowered).
	From, To ConcernSeverity
}

// QuoteVerificationResult is what VerifyQuotedPassages found and changed.
type QuoteVerificationResult struct {
	// Verified lists every concern whose quote was found, in concern order.
	Verified []QuoteVerified
	// Demoted lists every concern whose quote was NOT found, in concern order.
	Demoted []QuoteDemotion
	// VerdictClampedFrom is VerdictReject when a demotion downgraded a reject
	// to approve_with_concerns, "" otherwise.
	VerdictClampedFrom Verdict
}

// normalizeDocumentRef trims surrounding whitespace and any leading "./" so a
// reviewer naming "./docs/x.md" or " docs/x.md" matches the Source path
// "docs/x.md".
func normalizeDocumentRef(ref string) string {
	ref = strings.TrimSpace(ref)
	for strings.HasPrefix(ref, "./") {
		ref = strings.TrimPrefix(ref, "./")
	}
	return ref
}

// collapseWhitespace replaces every run of whitespace with one space and trims
// the ends, so a quote re-wrapped across lines still matches.
func collapseWhitespace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// VerifyQuotedPassages checks every concern's quoted_passage against the text
// the server actually injected into this reviewer invocation (E55.10 / #3755,
// ADR-084 D3 / rule 3), mutating v in place (its Concerns slice is copied
// before the first write). docs is that invocation's injected document set.
//
// A concern whose whitespace-trimmed QuotedPassage is empty is untouched. For
// every other concern the quote is VERIFIED iff its DocumentRef (trimmed,
// leading "./" stripped) names a document in docs with non-blank Text and the
// whitespace-collapsed quote is a substring of the whitespace-collapsed Text —
// case-sensitive, so one altered word fails. When several docs share the
// path, a match in any of them verifies (with that doc's hash).
//
//   - verified: QuoteVerifiedContentHash is stamped with the matched
//     document's ContentHash and QuoteUnverified cleared; severity untouched.
//   - unverified: QuoteUnverified is set, QuoteVerifiedContentHash cleared,
//     and the severity lowered to low, stamping SeverityClampedFrom with the
//     original severity when it changed and none was already recorded. The
//     failure mode (document_ref_missing | document_unknown |
//     document_text_unavailable | passage_not_found) is reported per concern.
//
// The shared reject-downgrade rule then applies: a REJECT becomes
// approve_with_concerns only when a high was lowered and no high remains.
// The function is idempotent: a second call over its own output changes
// nothing.
//
// It bounds FABRICATED passages, not trivially-true ones: a very short quote
// (a single common word) can verify; no minimum length is enforced.
func VerifyQuotedPassages(v *ReviewVerdict, docs []QuotedDocument) QuoteVerificationResult {
	var res QuoteVerificationResult
	if v == nil || len(v.Concerns) == 0 {
		return res
	}
	copied := false
	write := func() {
		if !copied {
			v.Concerns = append([]Concern(nil), v.Concerns...)
			copied = true
		}
	}
	loweredFromHigh := false
	for i, c := range v.Concerns {
		quote := collapseWhitespace(c.QuotedPassage)
		if quote == "" {
			continue
		}
		ref := normalizeDocumentRef(c.DocumentRef)
		failure := QuoteFailureDocumentRefMissing
		var matched *QuotedDocument
		if ref != "" {
			failure = QuoteFailureDocumentUnknown
			for j := range docs {
				if normalizeDocumentRef(docs[j].Path) != ref {
					continue
				}
				text := collapseWhitespace(docs[j].Text)
				if text == "" {
					if failure == QuoteFailureDocumentUnknown {
						failure = QuoteFailureDocumentTextUnavailable
					}
					continue
				}
				failure = QuoteFailurePassageNotFound
				if strings.Contains(text, quote) {
					matched = &docs[j]
					break
				}
			}
		}
		if matched != nil {
			if c.QuoteUnverified || c.QuoteVerifiedContentHash != matched.ContentHash {
				write()
				v.Concerns[i].QuoteUnverified = false
				v.Concerns[i].QuoteVerifiedContentHash = matched.ContentHash
			}
			res.Verified = append(res.Verified, QuoteVerified{
				Index: i, DocumentRef: ref, Commit: matched.Commit, ContentHash: matched.ContentHash,
			})
			continue
		}
		from := c.Severity
		if !c.QuoteUnverified || c.QuoteVerifiedContentHash != "" || from != SeverityLow {
			write()
			v.Concerns[i].QuoteUnverified = true
			v.Concerns[i].QuoteVerifiedContentHash = ""
			if from != SeverityLow {
				if from == SeverityHigh {
					loweredFromHigh = true
				}
				if c.SeverityClampedFrom == "" {
					v.Concerns[i].SeverityClampedFrom = from
				}
				v.Concerns[i].Severity = SeverityLow
			}
		}
		res.Demoted = append(res.Demoted, QuoteDemotion{
			Index: i, DocumentRef: ref, Failure: failure, From: from, To: SeverityLow,
		})
	}
	res.VerdictClampedFrom = downgradeRejectWithoutHigh(v, loweredFromHigh)
	return res
}

// PersonaClampedConcern records one concern ClampPersonaSeverities lowered.
type PersonaClampedConcern struct {
	// Index is the concern's position in ReviewVerdict.Concerns.
	Index int
	// Category is the concern's category, verbatim.
	Category string
	// From / To are the reviewer-assigned and clamped severities.
	From, To ConcernSeverity
}

// PersonaClampResult is what ClampPersonaSeverities changed.
type PersonaClampResult struct {
	// Clamped lists every lowered concern, in concern order.
	Clamped []PersonaClampedConcern
	// VerdictClampedFrom is VerdictReject when the clamp downgraded a reject
	// to approve_with_concerns, "" otherwise.
	VerdictClampedFrom Verdict
}

// ClampPersonaSeverities enforces a reviewer persona remit's severity_cap on
// that persona's verdict at INGEST (E55.10 / #3755), mutating v in place (its
// Concerns slice is copied before the first write). capSev "" (and "high") is
// UNCAPPED — a no-op; "medium" and "low" bound as themselves; any other value
// is outside the closed cap set and fails closed to low.
//
// Every concern ranked above the cap — whatever its category — is lowered to
// it, stamped SeverityClampedFrom with the original severity (unless one is
// already recorded, e.g. by an unverified-quote demotion) and
// PersonaSeverityCap with the effective cap. The shared reject-downgrade rule
// then applies. Idempotent: a second call over its own output lowers nothing.
func ClampPersonaSeverities(v *ReviewVerdict, capSev ConcernSeverity) PersonaClampResult {
	var res PersonaClampResult
	if v == nil || len(v.Concerns) == 0 {
		return res
	}
	bound := capRank(capSev)
	if bound >= 3 {
		return res
	}
	to := rankSeverity(bound)
	copied := false
	loweredFromHigh := false
	for i, c := range v.Concerns {
		if severityRank(c.Severity) <= bound {
			continue
		}
		if !copied {
			v.Concerns = append([]Concern(nil), v.Concerns...)
			copied = true
		}
		if c.Severity == SeverityHigh {
			loweredFromHigh = true
		}
		if c.SeverityClampedFrom == "" {
			v.Concerns[i].SeverityClampedFrom = c.Severity
		}
		v.Concerns[i].Severity = to
		v.Concerns[i].PersonaSeverityCap = to
		res.Clamped = append(res.Clamped, PersonaClampedConcern{
			Index: i, Category: c.Category, From: c.Severity, To: to,
		})
	}
	res.VerdictClampedFrom = downgradeRejectWithoutHigh(v, loweredFromHigh)
	return res
}

// AddSynthesizedConventionsFileModifiedConcern appends the ONE server-authored
// conventions_file_modified concern a review round carries when the reviewed
// diff modifies a declared conventions file and no reviewer of the round raised
// it (E55.3 / #2244). The concern is medium and names every (deduplicated,
// non-blank) path in order. When v's verdict is a plain approve it is raised to
// approve_with_concerns — an approve cannot carry an open concern — and the
// function returns VerdictApprove as the raisedFrom value the caller stamps on
// verdict_raised_from; approve_with_concerns and reject keep their verdict and
// return "". A nil verdict or a path list with no non-blank entry is a no-op
// returning "" (there is nothing to name). v.Concerns is copied before the
// append so a caller-shared backing array is never written.
//
// Call it AFTER ClampConventionSeverities: the clamp's empty-caps rule targets
// reviewer emissions and would otherwise lower this server-authored concern.
func AddSynthesizedConventionsFileModifiedConcern(v *ReviewVerdict, paths []string) (raisedFrom Verdict) {
	if v == nil {
		return ""
	}
	seen := make(map[string]bool, len(paths))
	named := make([]string, 0, len(paths))
	for _, p := range paths {
		p = strings.TrimSpace(p)
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		named = append(named, p)
	}
	if len(named) == 0 {
		return ""
	}
	concerns := make([]Concern, 0, len(v.Concerns)+1)
	concerns = append(concerns, v.Concerns...)
	concerns = append(concerns, Concern{
		Severity: SeverityMedium,
		Category: ConventionsFileModifiedConcernCategory,
		Note: "This change modifies a repository review-conventions file declared in the workflow spec: " +
			strings.Join(named, ", ") +
			". A conventions file changes the review criteria applied to later runs, so a human must confirm the edit is intended before merge.",
	})
	v.Concerns = concerns
	if v.Verdict == VerdictApprove {
		v.Verdict = VerdictApproveWithConcerns
		return VerdictApprove
	}
	return ""
}

// Usage is the token usage a reviewer backend reports for one review
// invocation (#681). It is captured at the reviewer CONTRACT boundary so
// the server can record advisory reviewer agent cost backend-agnostically
// at the plan_reviewed / implement_reviewed call site, never branching on
// which adapter (local subprocess, SDK, future) actually ran.
//
// Known is the graceful-degradation marker: a backend that cannot report
// usage leaves Known false (with zero-value token counts), and the server
// records the cost at usd=0 with known_usage=false rather than guessing —
// mirroring the cost/pricing unknown-model ok=false contract.
//
// Normalized accounting invariant (#1010, supersedes the #995 asymmetry):
// InputTokens is the cache-EXCLUSIVE fresh input-token count for EVERY
// adapter, and the cache-served portion is always ADDITIONAL to
// InputTokens — total input-side tokens = InputTokens + cache read +
// cache write, uniformly. Each adapter converts its backend's raw
// reporting to this contract at the boundary: codex's per-turn raw
// `input_tokens` INCLUDES `cached_input_tokens` (pinned against codex-cli
// 0.137.0), so the codex adapter subtracts the cached sum (clamped at 0);
// the Anthropic-side `input_tokens` already EXCLUDES cache reads/writes
// and passes through unchanged. Turns makes a multi-turn agentic blowup
// (many turns each re-sending the growing conversation) visible instead
// of a single opaque sum.
//
// Cache split (ADR-044 / #1343): the single former CachedInputTokens
// field is split into the three-bucket model — fresh input
// (InputTokens), cache READ (cache-served, cheaper), and cache WRITE
// (cache-creation, premium) — so the reviewer cost path can price the
// read/write portions at their separate vendor rates. The
// CachedInputTokens() accessor returns read+write so the existing
// cost_recorded `cached_input_tokens` total stays computable (back-compat).
type Usage struct {
	InputTokens  int
	OutputTokens int
	// CacheReadInputTokens is the cache-served (read) portion of the
	// input side, ADDITIONAL to InputTokens. Priced at the cheaper
	// cache-read rate. claudecode surfaces it from the envelope's
	// cache_read_input_tokens; codex maps its single cached_input_tokens
	// here; the anthropic adapter reads it from the SDK Usage block.
	CacheReadInputTokens int
	// CacheWriteInputTokens is the cache-creation (write) portion of the
	// input side, ADDITIONAL to InputTokens. Priced at the premium
	// cache-write rate. claudecode surfaces it from the envelope's
	// cache_creation_input_tokens; the anthropic adapter reads it from the
	// SDK Usage block; codex has no cache-write signal and leaves it 0.
	CacheWriteInputTokens int
	// Turns is the number of model turns the invocation took: summed
	// turn.completed lines for codex, 1 for the single-shot adapters
	// (claudecode --print, anthropic Messages). 0 when unknown.
	Turns int
	Known bool
}

// CachedInputTokens returns the total cache-served portion of the input
// side — the sum of the cache READ and cache WRITE buckets. It replaces
// the former CachedInputTokens field (#1343): every prior reader of that
// summed total keeps working through this accessor while the read/write
// split is recorded additively. Usage stays a comparable struct (all
// int/bool fields), so zero-value equality comparisons still hold.
func (u Usage) CachedInputTokens() int {
	return u.CacheReadInputTokens + u.CacheWriteInputTokens
}

// ConcernResolution is one reviewer judgment on a PRIOR concern listed
// in the implement-review prompt's delta-verification section (E22.X /
// #984). ID is the concern's stable UUID echoed back from the prompt;
// Resolution is one of "confirmed" (the diff resolves it), "reopened"
// (it does not), or "superseded" (overtaken by a different change). The
// server tolerantly maps these onto the concern state machine —
// unknown IDs and resolution strings are warn-and-skipped, never a
// gate failure.
type ConcernResolution struct {
	ID         string `json:"id"`
	Resolution string `json:"resolution"`
	Note       string `json:"note,omitempty"`
}

// ReviewVerdict is the structured response emitted by a review agent.
// The review-agent prompt instructs agents to return only this shape
// as a JSON object — no prose, no re-planning.
type ReviewVerdict struct {
	Verdict  Verdict   `json:"verdict"`
	Concerns []Concern `json:"concerns,omitempty"`
	FreeForm string    `json:"free_form,omitempty"`

	// ConcernResolutions carries the reviewer's per-concern verdicts on
	// the prior concerns threaded into a re-review prompt (#984). Absent
	// (nil) on a first review and on output from reviewers predating the
	// field — encoding/json ignores unknown members in both directions,
	// so old reviewer output stays valid.
	ConcernResolutions []ConcernResolution `json:"concern_resolutions,omitempty"`

	// Usage is the reviewer backend's token usage for this invocation,
	// populated by the adapter AFTER it decodes the verdict JSON — usage
	// comes from the API/CLI envelope, not the model-emitted verdict body.
	// The `json:"-"` tag isolates it from the agent-JSON decode so a model
	// that echoes a "usage" key in its response cannot spoof the recorded
	// cost figure. Zero-value with Known=false when the backend cannot
	// report usage (graceful degradation).
	Usage Usage `json:"-"`
}

// RejectSubstantiatesResolution answers the PER-RESOLUTION question the
// server-side reopen veto asks (#3319): may THIS resolution be applied on the
// evidence THIS verdict carries for it? It returns false — the resolution is
// UNSUBSTANTIATED and must be refused — when the verdict is `reject`, the
// verdict raises no concern of its own (len(v.Concerns) == 0), and this
// resolution's OWN Note is blank after TrimSpace. It returns true otherwise.
//
// It reads ONLY res.Note. A sibling resolution's note is evidence for that
// SIBLING, never for this one — which is exactly why this predicate exists
// separately from RejectNamesNoConcern below and MUST be the one the veto
// consults. Worked example, the mixed-resolution verdict:
//
//	verdict = reject, concerns[] = empty
//	  resolution A = confirmed, note "looks good"   (a DIFFERENT concern)
//	  resolution B = reopened,  note ""
//
// RejectNamesNoConcern is FALSE here (A carries a note), so a veto keyed on it
// would apply B's unsubstantiated reopen using A's evidence — concern A's
// evidence authorising concern B's reopen, the defect #3319 exists to close.
// RejectSubstantiatesResolution(v, B) is false and refuses B, while
// RejectSubstantiatesResolution(v, A) is true and A applies normally.
//
// v.FreeForm is deliberately NOT consulted: free-form prose is unmatched to any
// concern id, and treating it as substantiation is precisely the shape #3319
// reports. The narrowing is deliberate in both directions — a one-word note
// ("still broken") is NOT refused, because a broader semantic judgement is not
// machine-decidable and would silently discard genuine findings; and a reject
// that DOES raise a new concern is an engaged review, so its blank-note reopens
// still apply.
func RejectSubstantiatesResolution(v ReviewVerdict, res ConcernResolution) bool {
	if v.Verdict != VerdictReject {
		return true
	}
	if len(v.Concerns) != 0 {
		return true
	}
	return strings.TrimSpace(res.Note) != ""
}

// RejectNamesNoConcern answers the VERDICT-LEVEL question the display-only
// advisory asks (#3319): did this reject name NOTHING, ANYWHERE? It returns
// true when the verdict is `reject`, it raises no concern of its own, and no
// entry in v.ConcernResolutions carries a non-blank (TrimSpace) note.
//
// This is intentionally a DIFFERENT question from
// RejectSubstantiatesResolution above, and it MUST NOT be consulted by the
// veto. It aggregates over ALL resolutions, which is correct for an advisory
// whose job is to tell the operator the whole verdict carried no evidence, and
// wrong for a per-resolution refusal: on the mixed-resolution verdict in that
// function's doc comment it is FALSE, so a veto keyed on it would apply B's
// unsubstantiated reopen on A's note. Do not helpfully re-unify the two.
//
// v.FreeForm is likewise not consulted — a reject whose entire assertion lives
// in free_form names no concern, which is the reported #3319 shape and reads
// true here.
func RejectNamesNoConcern(v ReviewVerdict) bool {
	if v.Verdict != VerdictReject || len(v.Concerns) != 0 {
		return false
	}
	for _, res := range v.ConcernResolutions {
		if strings.TrimSpace(res.Note) != "" {
			return false
		}
	}
	return true
}

// AuthorityMode determines whether agent verdicts gate stage advancement.
type AuthorityMode string

// Authority modes per ADR-027 §3 decision table.
const (
	// AuthorityGating means an agent rejection blocks the plan stage
	// from advancing to awaiting_approval. Applies when agent>0 and
	// human==0: no human approver is present to override the agent.
	AuthorityGating AuthorityMode = "gating"

	// AuthorityAdvisory means agent verdicts are recorded and surfaced
	// but cannot block stage advancement. Applies when agent>0 and
	// human>0: the human approver is the authoritative gate.
	AuthorityAdvisory AuthorityMode = "advisory"

	// AuthorityGateless means no review agents are configured and the
	// plan stage proceeds without agent review. Applies when agent==0.
	AuthorityGateless AuthorityMode = "gateless"
)

// AuthoritySource records HOW an AuthorityMode was arrived at (E53.2 /
// #2225): SourceDeclared when the spec's reviewers.authority field spelled
// it out, SourceDerived when it fell back to the ADR-027 count-derived rule.
// It is surfaced per stage on GET /v0/runs/{id} so an operator reads the
// mode and its provenance instead of re-deriving either.
type AuthoritySource string

// Authority provenance markers (E53.2 / #2225).
const (
	// SourceDeclared marks an authority resolved from an explicit
	// reviewers.authority declaration in the spec.
	SourceDeclared AuthoritySource = "declared"

	// SourceDerived marks an authority resolved from the ADR-027
	// count-derived default (reviewers.authority was absent or, on the
	// zero-agent defence branch, could not be honoured).
	SourceDerived AuthoritySource = "derived"
)

// ResolveAuthority maps a ReviewersConfig to the applicable authority
// mode. It is the single-value entry point every existing call site uses;
// its behaviour is UNCHANGED for any Authority-empty config — the
// count-derived ADR-027 §3 decision table:
//
//	agent>0 && human==0 → gating
//	agent>0 && human>0  → advisory
//	agent==0            → gateless
//
// An explicit reviewers.authority (E53.2 / #2225) WINS over that table.
// The agent count is the effective count (ReviewersConfig.AgentCount):
// a heterogeneous `agents` list (#955) supersedes the bare integer, so
// heterogeneity changes who reviews, never the gating semantics.
//
// This is a thin wrapper over ResolveAuthorityWithSource so authority
// resolution stays behind ONE chokepoint — nothing downstream re-derives
// the mode a second way.
func ResolveAuthority(r spec.ReviewersConfig) AuthorityMode {
	mode, _ := ResolveAuthorityWithSource(r)
	return mode
}

// ResolveAuthorityWithSource resolves the authority mode AND its provenance
// (E53.2 / #2225). Resolution order:
//
//  1. AgentCount()==0 → (gateless, derived). With zero agent reviewers there
//     is nothing to gate on or to surface, so a declared authority cannot be
//     honoured; authority governs whether a verdict blocks, never which gates
//     exist. This branch is UNREACHABLE for a schema+semantically-validated
//     spec (spec.Validate rejects a declaration with no agents in both the
//     backend and the CLI); it is defence in depth for campaign-override bytes
//     that bypass validation.
//  2. Authority is exactly "gating" or "advisory" → (that mode, declared).
//  3. Any other non-empty Authority (out-of-enum; reachable only by bypassing
//     the schema) falls through to (4) rather than being honoured.
//  4. The ADR-027 count-derived table → (mode, derived).
func ResolveAuthorityWithSource(r spec.ReviewersConfig) (AuthorityMode, AuthoritySource) {
	if r.AgentCount() == 0 {
		return AuthorityGateless, SourceDerived
	}
	switch AuthorityMode(r.Authority) {
	case AuthorityGating:
		return AuthorityGating, SourceDeclared
	case AuthorityAdvisory:
		return AuthorityAdvisory, SourceDeclared
	}
	// Authority empty or out-of-enum: fall back to the count-derived rule.
	if r.Human == 0 {
		return AuthorityGating, SourceDerived
	}
	return AuthorityAdvisory, SourceDerived
}

// Settled reports whether a stage's configured agent reviews have all
// reached a terminal state: at least configuredAgents terminal review
// entries (plan_reviewed/implement_reviewed, *_review_failed,
// *_review_skipped each count, so a timed-out or skipped reviewer
// never strands the detection). Zero configured agents never settle —
// there is nothing to wait for, and the caller's gateless branch owns
// that case. This is the N-of-N detection the drive engine's
// reviews_settled_gate rule (#1023) and the ADR-036 plan-approval
// completion gate share.
func Settled(configuredAgents, terminalEntries int) bool {
	return configuredAgents > 0 && terminalEntries >= configuredAgents
}

// ReasonReviewerNotConfigured is the ReviewSkippedPayload.Reason for the
// coarse degradation: the stage requested agent review but NO reviewer
// backend was wired at all on the deployment (#574). It is distinct from
// ReasonReviewerUnavailable, which is the per-reviewer capability gap.
const ReasonReviewerNotConfigured = "reviewer_not_configured"

// ReasonReviewerUnavailable is the ReviewSkippedPayload.Reason for the
// per-reviewer capability gap (#1495): a deployment that HAS a reviewer
// backend but cannot run THIS spec-declared reviewer's provider (its
// FISHHAWKD_ENABLE_* / API-key capability gate is off). It is deliberately
// distinct from a genuine reviewer error (plan_review_failed /
// implement_review_failed) — the reviewer never ran because the deployment
// lacks the capability, not because the invocation failed.
const ReasonReviewerUnavailable = "reviewer_unavailable"

// ReasonPersonaRemitUnavailable is the ReviewSkippedPayload.Reason for a
// reviewer PERSONA (ADR-084 / E55.8 / #3753) whose remit document could not be
// resolved, rendered or attributed: the persona FAILS CLOSED for itself only —
// it never runs on a remit-less prompt — while the stage's standard reviewers
// still run. ReviewSkippedPayload.Detail names which step failed. It is a skip
// REASON value on the existing *_review_skipped categories, not a new audit
// category.
const ReasonPersonaRemitUnavailable = "persona_remit_unavailable"

// ReasonPersonaAttachmentUnresolvable is the ReviewSkippedPayload.Reason for a
// reviewer-persona ATTACHMENT SET that could not be resolved (ADR-084 / E55.9 /
// #3754, carrying #3753): the spec declares reviewer_personas, but the
// reviewed stage could not be mapped onto its spec stage (Detail
// persona_stage_unresolvable) or the escalations that attach personas could
// not be evaluated (Detail escalation_unevaluable). Which personas would have
// run is unknown, so ONE terminal skip per failed source is recorded instead
// of silently running none; it is counted in configured_agents so the round
// still settles exactly. Persona is empty — no persona was identified. A skip
// REASON value on the existing *_review_skipped categories, not a new audit
// category.
const ReasonPersonaAttachmentUnresolvable = "persona_attachment_unresolvable"

// ReviewSkippedPayload is the JSON payload stored in an audit
// entry with category "plan_review_skipped" / "implement_review_skipped"
// (#574). It records that an agent review the spec requested did not run.
// Three degradation reasons share this payload:
//   - ReasonReviewerNotConfigured: no reviewer backend wired at all (#574).
//   - ReasonReviewerUnavailable: this specific spec-declared reviewer's
//     provider is unavailable on the deployment — the capability-gate
//     degradation honoring the per-reviewer optional flag (#1495).
//   - ReasonPersonaRemitUnavailable: a reviewer persona's remit document could
//     not be resolved or attributed, so that persona alone did not run
//     (ADR-084 / #3753); Persona and Detail name it and the failed step.
//   - ReasonPersonaAttachmentUnresolvable: the round's persona attachment set
//     could not be resolved at all (ADR-084 / E55.9 / #3754); Persona is empty
//     and Detail names the failed source (persona_stage_unresolvable /
//     escalation_unevaluable).
//
// Authority captures whether the skip degraded a gating or advisory gate;
// in advisory mode the human gate remains authoritative.
type ReviewSkippedPayload struct {
	Reason           string        `json:"reason"`
	ConfiguredAgents int           `json:"configured_agents"`
	Authority        AuthorityMode `json:"authority"`

	// Provider is the spec-declared reviewer provider that could not be run
	// (#1495). Populated only on the ReasonReviewerUnavailable per-reviewer
	// capability skip; empty (omitempty) on the coarse not-configured skip,
	// which has no single provider. Keeps pre-#1495 payloads byte-identical.
	Provider string `json:"provider,omitempty"`

	// Optional records the per-reviewer optional flag the skip honored
	// (#1495): true for a graceful quiet advisory-skip, false (default) for a
	// loud surface (the deployment SHOULD have run it). omitempty keeps
	// optional:false (the common case) and pre-#1495 payloads byte-identical.
	Optional bool `json:"optional,omitempty"`

	// Persona names the reviewer persona (ADR-084 / E55.8 / #3753) this skip
	// belongs to — set on a ReasonPersonaRemitUnavailable skip and on a
	// ReasonReviewerUnavailable skip of a persona whose provider is
	// unavailable. Empty for every standard reviewer; omitempty keeps those
	// payloads byte-identical to pre-#3753 entries.
	Persona string `json:"persona,omitempty"`

	// Detail is the machine-readable step a ReasonPersonaRemitUnavailable skip
	// failed at (e.g. remit_missing, run_base_commit_unrecorded,
	// document_resolver_unconfigured, remit_unresolvable,
	// persona_prompt_build_failed, remit_unattributed) or the source a
	// ReasonPersonaAttachmentUnresolvable skip failed at
	// (persona_stage_unresolvable, escalation_unevaluable). Empty on every
	// other skip; omitempty keeps those payloads byte-identical.
	Detail string `json:"detail,omitempty"`
}

// ReviewStartedPayload is the JSON payload stored in an audit entry with
// category "plan_review_started" / "implement_review_started" (#600). It
// marks that a review agent was actually dispatched — emitted once per
// stage at dispatch, only when agent>0 AND a PlanReviewer is wired (never
// for the agent==0 "none" or nil-reviewer "skipped" branches). It is the
// MCP-readable proxy that distinguishes a configured-and-running review
// ('pending') from no review configured ('none'): the started entry is
// appended before the per-reviewer loop writes the terminal *_reviewed
// entries, so its audit sequence always precedes them under both gating
// (synchronous) and advisory (detached) authority.
type ReviewStartedPayload struct {
	ConfiguredAgents int           `json:"configured_agents"`
	Authority        AuthorityMode `json:"authority"`

	// Personas lists, in attachment order, the reviewer personas (ADR-084 /
	// E55.8 / #3753) counted in ConfiguredAgents: each persona is one extra
	// reviewer invocation that produces exactly one terminal entry (verdict,
	// failed or skipped), so the round settles at ConfiguredAgents terminal
	// entries. Empty when the reviewed stage attaches no persona; omitempty
	// keeps those payloads byte-identical to pre-#3753 entries.
	Personas []string `json:"personas,omitempty"`

	// HeadSHA is the implement-review idempotency key (#797): the bundle's
	// verify_run committed-tree head_sha, recorded on the
	// implement_review_started entry so a retried raw upload (transient 5xx
	// after the review already dispatched) is deduped on (stage_id,
	// head_sha) before re-dispatching a second review. omitempty keeps the
	// plan path byte-identical — plan_review_started has no diff/head_sha
	// and passes "", so its payload is unchanged.
	HeadSHA string `json:"head_sha,omitempty"`

	// TreeSHA is the round's REVIEWED-TREE identity (#3655): the tree object
	// hash of the committed tree the bundle's authoritative (last) verify_run
	// certified, i.e. the tree this implement-review round judged. It is
	// DISTINCT from HeadSHA, which stays the #797 dedup key and is a throwaway
	// WIP-commit SHA the runner soft-resets away — so the pushed commit SHA
	// always differs from it, while the pushed TREE equals it on a normal ship.
	// The success PR ship RETAINS it as a human coordinate on a
	// review_head_mismatch row; since #3665 the comparison that fires the row is
	// ChangeID below, not this tree.
	// omitempty keeps plan_review_started (which passes "") and every
	// pre-change implement_review_started payload byte-identical; an old
	// stored payload decodes "", which disables the comparison for that round.
	TreeSHA string `json:"tree_sha,omitempty"`

	// ChangeID is the round's REVIEWED-CHANGE identity (#3665): the
	// `git patch-id --stable` sum the runner stamped on the bundle's
	// authoritative verify_run for the gated commit whose tree TreeSHA names —
	// i.e. the CHANGE this implement-review round judged, read from that SAME
	// event (bundle.ExtractVerifyIdentity) so the pair can never describe two
	// different commits. This, not TreeSHA, is what the success PR ship compares
	// against the runner-reported verified_change_id: patch-id ignores line
	// numbers and both sides base the diff on the commit's own parent, so a
	// change re-staged onto an advanced base yields an IDENTICAL id (no row)
	// while a genuinely different re-landed change yields a different one.
	// omitempty keeps plan_review_started (which passes "") and every
	// pre-change implement_review_started payload byte-identical; an old stored
	// payload decodes "", which the ship-side check treats as undecidable and
	// records nothing.
	ChangeID string `json:"change_id,omitempty"`

	// RoundOrigin records WHERE an implement-review round's diff came from
	// (#4077), so a round orphaned by a daemon restart can be re-dispatched
	// against the same input: "trace" (the stage's uploaded trace bundle),
	// "fixup_push" (ComparePatch(RoundBaseSHA, HeadSHA) after a fix-up push)
	// or "consolidated" (a decomposed parent's ComparePatch(RoundBaseSHA,
	// HeadSHA)). Empty on plan_review_started and on every implement round
	// written before #4077 — a reader treats empty as an unknown source.
	// omitempty keeps those payloads byte-identical.
	RoundOrigin string `json:"round_origin,omitempty"`

	// RoundBaseSHA is the compare base of a fixup_push / consolidated round
	// (#4077): the re-dispatch rebuilds the diff as ComparePatch(RoundBaseSHA,
	// HeadSHA). Empty for a trace round (its diff lives in the stored bundle)
	// and for plan rounds. omitempty keeps those payloads byte-identical.
	RoundBaseSHA string `json:"round_base_sha,omitempty"`

	// RedispatchOf is the audit sequence of the ORPHANED round's
	// *_review_started entry this round re-dispatches after a daemon restart
	// (#4077). 0 (omitted) on an ordinary round.
	RedispatchOf int64 `json:"redispatch_of,omitempty"`

	// RedispatchDepth counts how many re-dispatches separate this round from
	// the round that was originally dispatched (#4077): 1 for the first
	// re-dispatch, the orphaned round's depth + 1 thereafter. The boot sweep
	// caps it so a crash loop cannot re-dispatch forever. 0 (omitted) on an
	// ordinary round.
	RedispatchDepth int `json:"redispatch_depth,omitempty"`
}

// ReviewFailedPayload is the JSON payload stored in an audit entry with
// category "plan_review_failed" / "implement_review_failed" (#664). It is
// the terminal entry written when a wired reviewer invocation errors or
// times out (a reviewer killed at FISHHAWKD_PLAN_REVIEW_TIMEOUT surfaces as
// a Review error). One entry is appended per failed reviewer invocation.
//
// It is kept deliberately distinct from plan_reviewed / implement_reviewed
// (which carry only the closed approve / approve_with_concerns / reject
// verdict set) so a timeout or transport failure is never decoded as a real
// verdict. Authority records whether the failure degraded a gating or
// advisory gate; this entry is observability-only and does NOT change
// gating advance/degrade semantics (#574). ReviewerModel is best-effort —
// it is empty when the adapter failed before reporting which model ran.
type ReviewFailedPayload struct {
	Reason        string        `json:"reason"`
	ReviewerModel string        `json:"reviewer_model,omitempty"`
	Authority     AuthorityMode `json:"authority"`

	// Timeout distinguishes a per-invocation budget kill from any other
	// failure (#747): true when the reviewer was killed by the size-aware
	// budget deadline (context.DeadlineExceeded at the call site), false for
	// transport/decode/other errors. It is the additive discriminator the
	// issue's "distinguish a timeout" requirement asks for — the audit
	// category stays plan_review_failed / implement_review_failed so existing
	// #664 and MCP await-review readers are unaffected. omitempty keeps the
	// payload byte-identical to pre-#747 entries on the non-timeout path.
	Timeout bool `json:"timeout,omitempty"`
}

// PlanReviewedPayload is the JSON payload stored in an audit entry
// with category "plan_reviewed". One entry is appended per review
// agent invocation.
type PlanReviewedPayload struct {
	ReviewerKind  string        `json:"reviewer_kind"`
	ReviewerModel string        `json:"reviewer_model,omitempty"`
	Authority     AuthorityMode `json:"authority"`
	Verdict       Verdict       `json:"verdict"`
	Concerns      []Concern     `json:"concerns,omitempty"`
	FreeForm      string        `json:"free_form,omitempty"`

	// InputTokens / OutputTokens surface the reviewer invocation's token
	// usage on the review audit surface itself (#995), so a context-assembly
	// blowup is visible where operators already read verdicts — not only in
	// the cost_recorded ledger. InputTokens is the fresh (cache-exclusive)
	// input count per the normalized Usage contract (#1010). omitempty keeps
	// usage-free payloads byte-identical to pre-#995 entries.
	InputTokens  int `json:"input_tokens,omitempty"`
	OutputTokens int `json:"output_tokens,omitempty"`

	// ReviewerVersion / ReviewerBinary record the resolved reviewer CLI's
	// version and binary-path provenance for this invocation (#1768,
	// completing the both-CLIs half of #1741 leg (a) for the REVIEWER
	// surface). ReviewerVersion is the CLI's free-form version string (e.g.
	// "0.30.5 (codex-cli)"), or "unknown" when the CLI is present but
	// unprobeable; ReviewerBinary is the resolved binary name/path (e.g.
	// "codex", or a FISHHAWKD_CODEX_BINARY override path). Both are populated
	// only for a reviewer whose adapter implements the version-probe /
	// binary-report capabilities (the codex subprocess adapter); the
	// anthropic SDK and claudecode adapters implement neither, so both fields
	// stay empty and omitempty keeps every pre-#1768 payload byte-identical —
	// mirroring the InputTokens/OutputTokens additive-field posture above.
	ReviewerVersion string `json:"reviewer_version,omitempty"`
	ReviewerBinary  string `json:"reviewer_binary,omitempty"`

	// Persona names the reviewer persona (ADR-084 / E55.8 / #3753) that
	// produced this verdict. Empty for a standard reviewer; omitempty keeps
	// every standard-reviewer payload byte-identical to pre-#3753 entries.
	Persona string `json:"persona,omitempty"`

	// VerdictClampedFrom records that the ingest-time review-convention clamp
	// (ClampConventionSeverities, E55.3 / #2244) downgraded the reviewer's
	// verdict: it holds the ORIGINAL verdict (reject) while Verdict holds the
	// clamped one (approve_with_concerns). omitempty keeps every unclamped
	// payload byte-identical, and an old stored payload decodes "".
	VerdictClampedFrom Verdict `json:"verdict_clamped_from,omitempty"`
}

// ImplementReviewedPayload is the JSON payload stored in an audit entry
// with category "implement_reviewed" (ADR-027 impl 2/2). It records one
// implement-review agent invocation against the implement-stage diff. The
// shape is identical to PlanReviewedPayload — the verdict, authority, and
// concern semantics are the same; only the reviewed artifact (diff vs.
// plan) differs. scope.files drift is surfaced as a {category:"scope"}
// concern here rather than an auto-reject (ADR-027 Decision Q6).
//
// The companion "implement_review_skipped" category reuses
// ReviewSkippedPayload — same reviewer-not-configured degradation story
// as the plan stage.
type ImplementReviewedPayload struct {
	ReviewerKind  string        `json:"reviewer_kind"`
	ReviewerModel string        `json:"reviewer_model,omitempty"`
	Authority     AuthorityMode `json:"authority"`
	Verdict       Verdict       `json:"verdict"`
	Concerns      []Concern     `json:"concerns,omitempty"`
	FreeForm      string        `json:"free_form,omitempty"`

	// ConcernResolutions records the reviewer's delta-verification
	// verdicts on prior concerns (#984) on the authoritative audit
	// payload — the concern store applies them as a derived index.
	// omitempty keeps resolution-free payloads byte-identical to
	// pre-#984 entries, and old stored payloads decode unchanged.
	ConcernResolutions []ConcernResolution `json:"concern_resolutions,omitempty"`

	// InputTokens / OutputTokens mirror PlanReviewedPayload (#995): the
	// reviewer invocation's token usage on the review audit surface.
	// InputTokens is fresh (cache-exclusive) input per the normalized
	// Usage contract (#1010).
	InputTokens  int `json:"input_tokens,omitempty"`
	OutputTokens int `json:"output_tokens,omitempty"`

	// ReviewerVersion / ReviewerBinary mirror PlanReviewedPayload (#1768):
	// the resolved reviewer CLI's version string and binary-path provenance
	// for this invocation. Populated only for a capability-bearing reviewer
	// (the codex subprocess adapter); empty for the anthropic/claudecode
	// adapters, so omitempty keeps every pre-#1768 payload byte-identical.
	ReviewerVersion string `json:"reviewer_version,omitempty"`
	ReviewerBinary  string `json:"reviewer_binary,omitempty"`

	// RejectWithoutConcern marks a `reject` verdict that named NO concern
	// anywhere (#3319) — no concerns[] entry and no concern_resolutions entry
	// carrying a non-blank note. ADVISORY and DISPLAY-ONLY: it gates nothing and
	// is derived from RejectNamesNoConcern, the VERDICT-LEVEL predicate — never
	// from RejectSubstantiatesResolution, which is the per-resolution question
	// the reopen veto asks. omitempty keeps every pre-#3319 payload
	// byte-identical, and an old stored payload decodes false.
	RejectWithoutConcern bool `json:"reject_without_concern,omitempty"`

	// ReviewRoundSequence is the audit Sequence of the
	// implement_review_started row that opened the review round THIS verdict
	// belongs to (#3593). It is RECORDED by the writer at round-open time —
	// runImplementReviews captures emitReviewStarted's returned sequence and
	// threads it into runImplementReviewInvocations — never derived from the
	// verdict's position in the chain. It is the fixed anchor the retry
	// supersession predicate and the PR relay both key on: a stage retry whose
	// sequence exceeds this value discarded the tree this round reviewed. 0
	// (absent) means a legacy row (predating #3593) or an emit failure
	// (emitReviewStarted returned ok=false), and ONLY then do consumers fall
	// back to the legacy "newest implement_review_started below the verdict"
	// derivation. omitempty keeps pre-#3593 payloads byte-identical, and an
	// old stored payload decodes 0.
	ReviewRoundSequence int64 `json:"review_round_sequence,omitempty"`

	// SupersededByRetry marks a verdict written AFTER a same-stage
	// stage_retried / stage_override_retried row whose Sequence exceeds
	// ReviewRoundSequence (#3593): the verdict reviewed a tree the retry has
	// since discarded, so it must never be posted to the retry's PR. Recorded
	// at VERDICT-BUILD time from a fresh retry-sequence read. The
	// retry-lands-during-persistence window MAY leave a genuinely-superseded
	// verdict UNMARKED (the retry landed after this payload was written); that
	// residual is safe because the concern is still superseded by the
	// post-persist re-check (or the retry-time sweep) AND the PR relay's rule
	// compares retry rows against ReviewRoundSequence, not this flag alone.
	// omitempty keeps every pre-#3593 payload byte-identical, and an old stored
	// payload decodes false.
	SupersededByRetry bool `json:"superseded_by_retry,omitempty"`

	// Origin marks a non-first-review provenance for the verdict (#1250).
	// Empty on the first review and the parent-decomposition consolidated
	// review (byte-identical). The base-rebase re-invoke supplemental pass
	// sets Origin="base_rebase_reinvoke" so this additive verdict is
	// labelable and so the pre-dispatch idempotency dedup can key on
	// (stage_id, Origin, HeadSHA). omitempty keeps origin-free payloads
	// byte-identical to pre-#1250 entries.
	Origin string `json:"origin,omitempty"`

	// HeadSHA is the re-landed head SHA the supplemental verdict is anchored
	// to (#1250): the pushed tree the base-rebase re-invoke produced. Paired
	// with Origin it forms the (stage_id, Origin, HeadSHA) idempotency key
	// that dedups a retried PR-upload. Empty on every non-supplemental
	// review; omitempty keeps those payloads byte-identical.
	HeadSHA string `json:"head_sha,omitempty"`

	// Persona names the reviewer persona (ADR-084 / E55.8 / #3753) that
	// produced this verdict. Empty for a standard reviewer; omitempty keeps
	// every standard-reviewer payload byte-identical to pre-#3753 entries.
	Persona string `json:"persona,omitempty"`

	// VerdictClampedFrom records that the ingest-time review-convention clamp
	// (ClampConventionSeverities, E55.3 / #2244) downgraded the reviewer's
	// verdict: it holds the ORIGINAL verdict (reject) while Verdict holds the
	// clamped one (approve_with_concerns). omitempty keeps every unclamped
	// payload byte-identical, and an old stored payload decodes "".
	VerdictClampedFrom Verdict `json:"verdict_clamped_from,omitempty"`

	// VerdictRaisedFrom records that the once-per-round conventions_file_modified
	// synthesis (AddSynthesizedConventionsFileModifiedConcern, E55.3 / #2244)
	// raised a plain approve to approve_with_concerns: it holds the ORIGINAL
	// verdict (approve). omitempty keeps every other payload byte-identical.
	VerdictRaisedFrom Verdict `json:"verdict_raised_from,omitempty"`

	// ConventionsFileModifiedSynthesized marks the ONE verdict of a round that
	// carries the server-synthesized conventions_file_modified concern (E55.3 /
	// #2244), distinguishing it from a reviewer-raised one. omitempty keeps
	// every other payload byte-identical, and an old stored payload decodes
	// false.
	ConventionsFileModifiedSynthesized bool `json:"conventions_file_modified_synthesized,omitempty"`
}

// OriginBaseRebaseReinvoke is the ImplementReviewedPayload.Origin marker
// for the base-rebase re-invoke supplemental implement-review pass (#1250).
// The first review and the parent-decomposition consolidated review leave
// Origin empty; only runSupplementalReinvokeReview stamps this value, which
// the pre-dispatch idempotency dedup keys on together with HeadSHA.
const OriginBaseRebaseReinvoke = "base_rebase_reinvoke"
