// Package fixupobligation classifies the operator-authored instructions routed
// with an implement-stage fix-up pass (#2737) into REPORTING obligations — an
// instruction that asks the agent to RECORD something on a report surface (the
// PR body's `## Notes`, a run log) rather than to change code.
//
// It exists because a fix-up pass has no sanctioned transport for such an
// instruction: the slim fix-up prompt deliberately renders NO PR-description
// block (the pull request already exists, so the pass must never clobber its
// title/body — backend/internal/prompt/prompt.go's FixupCommitMessagePath
// contract), while the verify gate certifies only the tree and the implement
// review is diff-only. A routed reporting obligation could therefore be
// declined silently, with nothing marking the omission.
//
// This package is the PURE, dependency-free half of the fix: both the fix-up
// prompt renderer (which names each obligation to the agent) and the
// implement-review re-derivation (which subtracts the agent's `met` reports and
// signals the remainder) call Detect on the SAME stage_fixup_triggered audit
// payload, so the declared set is identical at both sites. Sameness rests on a
// pinned IDENTITY, not on a shared predicate: the prompt serve records which
// entry it rendered from (a fixup_report_obligations_declared anchor carrying
// that entry's audit sequence) and the review resolves that exact entry, so a
// second fix-up triggered for the same stage in between cannot make the review
// report ids the agent never saw. The anchor stores a pointer, not the derived
// set, so there is still no persisted duplicate to drift. See
// backend/internal/server/prompt.go::resolveFixupReportObligations.
//
// The signal this package feeds is EVIDENCE ONLY: it never fails, re-opens, or
// re-budgets a pass. Long-form contract: backend/internal/fixupobligation/README.md.
package fixupobligation

import (
	"strings"
	"unicode/utf8"
)

// SourceKind names which routed channel an obligation's text came from. The
// three kinds are exactly the operator-authored free-text fields the fix-up
// trigger records on its stage_fixup_triggered audit entry
// (backend/internal/server/fixup.go::writeFixupAudit).
type SourceKind string

const (
	// SourceConcern is a routed implement-review concern note.
	SourceConcern SourceKind = "concern"
	// SourceOperatorConcern is the free-text `operator_concern` instruction
	// (#1311). Since #2623 it is ALSO minted as a durable concern, so its text
	// normally appears as a concern note too; Detect dedupes that overlap.
	SourceOperatorConcern SourceKind = "operator_concern"
	// SourceReason is the operator's fix-up `reason`.
	SourceReason SourceKind = "reason"
)

// Status literals for an agent's per-obligation self-report.
const (
	// StatusMet claims the obligation was carried out. The runner requires the
	// sidecar entry to carry a non-empty record holding the attestation text —
	// and then discards that text rather than transmitting it (see Report).
	StatusMet = "met"
	// StatusDeclined honestly declines the obligation. The runner requires a
	// non-empty reason on the same terms.
	StatusDeclined = "declined"
)

// Status literals for an UNDELIVERED finding (what the review-time join
// reports about a declared obligation that carries no valid `met` report).
const (
	// StatusUnreported means the agent said nothing at all about this
	// obligation — no report, or one that failed the runner's fail-closed
	// validation and was dropped.
	StatusUnreported = "unreported"
	// StatusUnsatisfiable means the obligation named the pull-request body —
	// a surface a fix-up pass has NO mechanism to write (the slim fix-up
	// prompt renders no PR-description block, and the pass pushes commits
	// only). Such an obligation is returned as a Finding REGARDLESS of the
	// agent's self-report — even a `met` claim — because that claim cannot be
	// true: the pass could not have written the PR body. Reporting it as
	// unsatisfiable (rather than dropping it on a `met`, or accusing the agent
	// of an omission) is the heart of #2782 — it is what stops a rich
	// counterfactual table reading as complete while documenting only the
	// first pass (PR #2775), without falsely blaming the agent for a surface
	// it never controlled.
	StatusUnsatisfiable = "unsatisfiable"
)

// MaxExcerptBytes bounds every stored obligation excerpt (the operator's own
// instruction text — an agent's report carries no text at all, see Report).
// Operator-authored text can be up to maxOperatorConcernBytes
// (4000) and a routed concern note is unbounded in practice; without a cap a
// single instruction could swamp the fix-up prompt, the gate-evidence payload,
// and the audit entry.
const MaxExcerptBytes = 400

// truncationMarker is appended to a bounded excerpt so a reader can tell the
// text was cut rather than authored short.
const truncationMarker = "… (truncated)"

// Source is one routed instruction, tagged with the channel it arrived on and
// with its TRUST provenance.
type Source struct {
	Kind SourceKind
	Text string
	// Untrusted marks text that is NOT operator-authored: today, a routed
	// concern synthesized from the acceptance agent's attacker-influenceable
	// free-text verdict (ADR-050 / E31.8 / #1613). Detect carries the flag onto
	// the Obligation so both render sites can quarantine the excerpt instead of
	// rendering it inline as trusted operator instruction. The flag is the
	// caller's to set: this package classifies text, it cannot know provenance.
	Untrusted bool
}

// Obligation is one DECLARED reporting obligation: a stable id, the channel it
// came from, and the bounded excerpt of the instruction text.
type Obligation struct {
	// ID is `ob-1`..`ob-N` in Detect's fixed order. It is the join key between
	// the prompt block the agent reads and the report it writes back.
	ID string
	// Source is the channel the instruction arrived on.
	Source SourceKind
	// Text is the bounded excerpt of the instruction.
	Text string
	// Untrusted mirrors the originating Source's trust provenance. An
	// obligation detected in an acceptance-derived concern note carries text
	// that an attacker-influenceable validator authored, so it must reach the
	// fix-up agent and the reviewer only inside a quarantine envelope — the
	// same trust boundary the concern itself is rendered on. Losing this flag
	// would launder untrusted text into trusted prompt content, which is the
	// bypass this field exists to close.
	Untrusted bool
	// PRBody marks an obligation whose instruction names the pull-request body
	// — a surface a fix-up pass has NO mechanism to write (#2782). Derived at
	// the single Detect site from NamesPRBodySurface(text), so the flag rides
	// every consumer (the routing-time warning, the advisory audit entry, the
	// fix-up prompt, and the review-time join) without any of them re-running
	// the classifier. Undelivered treats a PRBody obligation as unsatisfiable
	// regardless of the agent's report.
	PRBody bool
}

// Report is one agent self-report entry for a declared obligation, as
// validated by the runner and carried through gate_evidence.
//
// It carries NO agent-authored free text, deliberately. The fix-up agent runs
// arbitrary repository commands, so every byte of an attestation or decline
// reason it writes is agent-controlled; carrying that text over the trace
// bundle into the reviewer's prompt would be an egress path for repository
// content that never appears in the committed diff, and quarantining it at the
// render site limits its AUTHORITY without limiting what it can carry. So the
// runner validates the text locally (a `met` needs a non-empty record, a
// `declined` needs a non-empty reason) and transmits only the two fields the
// join needs: the id and the status literal.
type Report struct {
	// ID must match a declared Obligation.ID; an unknown id is ignored.
	ID string
	// Status is StatusMet or StatusDeclined (the runner drops anything else).
	Status string
}

// Finding is one UNDELIVERED declared obligation: the obligation itself plus
// why it is undelivered.
//
// There is no decline-reason field: the agent's stated reason is validated on
// the runner and never crosses the upload boundary (see Report), so a Finding
// carries only backend-derived text — the operator's own instruction excerpt.
type Finding struct {
	Obligation
	// Status is StatusUnreported (nothing valid came back for this id),
	// StatusDeclined (the agent reported it and honestly declined), or
	// StatusUnsatisfiable (the obligation named the PR body, which the pass
	// cannot write — returned regardless of the agent's report).
	Status string
}

// recordingVerbs is the first half of the deliberately CONJUNCTIVE classifier.
// A reporting obligation asks the agent to write something down, so the text
// must carry a recording verb. Matched case-folded on a plain substring, so
// inflections ("recorded", "reporting", "documenting") match their stem.
var recordingVerbs = []string{
	"record",
	"report",
	"document",
	"attest",
	"attestation",
	"write up",
	"list out",
}

// The report-surface nouns are the second half of the conjunctive classifier:
// a recording verb alone is ordinary ("record the retry count in a metric"),
// so the text must ALSO name a report surface. This narrowness is the
// anti-noise property — a signal that fires on every pass is one operators
// learn to ignore, which is the failure mode this package must not create.
//
// The nouns split into two disjoint sets by whether a fix-up pass CAN write
// the named surface (#2782):
//
//   - prBodySurfaceNouns name the pull-request body, which a fix-up pass has
//     NO mechanism to write: the slim fix-up prompt renders no PR-description
//     block and the pass pushes commits only, so the PR title/body composed at
//     PR-open by the first implement attempt is never re-composed. An
//     obligation naming one of these is UNSATISFIABLE by construction.
//     `## notes` / `notes section` are PR-body surfaces because in this
//     workflow the `## Notes` heading IS a pull-request-body convention
//     (AGENTS.md's git-flow PR-body template) — and it is the exact phrasing
//     the #2782 occurrence used.
//   - otherReportSurfaceNouns name a surface the pass CAN still write (a run
//     log), so an obligation naming one stays satisfiable and keeps today's
//     `met`-suppresses-the-finding behaviour.
//
// IsObligation keeps its exact prior semantics by testing the UNION of the two
// lists, so the detected obligation SET — and therefore every `ob-N` id — is
// unchanged; only the per-member classification is new.
var prBodySurfaceNouns = []string{
	"pr body",
	"pull request body",
	"pull-request body",
	"pr description",
	"pull request description",
	"pull-request description",
	"## notes",
	"notes section",
}

// otherReportSurfaceNouns name report surfaces a fix-up pass CAN write, so an
// obligation naming one is satisfiable and keeps today's behaviour.
var otherReportSurfaceNouns = []string{
	"run log",
}

// IsObligation reports whether text carries a reporting obligation: it must
// contain BOTH a recording verb AND a report-surface noun (case-folded),
// testing the UNION of the PR-body and other-surface noun lists so the
// detected set is identical to before the #2782 split. This is a lexical,
// English-only heuristic over operator-authored text — a reporting obligation
// phrased without any listed term is a false NEGATIVE, which is today's exact
// behavior, so nothing regresses when it misses.
func IsObligation(text string) bool {
	folded := strings.ToLower(text)
	if !containsAny(folded, recordingVerbs) {
		return false
	}
	return containsAny(folded, prBodySurfaceNouns) || containsAny(folded, otherReportSurfaceNouns)
}

// NamesPRBodySurface reports whether text names the pull-request body — a
// surface a fix-up pass cannot write (#2782). It is deliberately NOT
// conjunctive with the recording verbs: it answers a different question than
// IsObligation. IsObligation asks "is this a reporting obligation at all"
// (verb AND surface); NamesPRBodySurface asks "does this instruction name the
// surface this pass structurally cannot write" (surface only), which is what
// decides whether an already-detected obligation is unsatisfiable. Case-folded
// substring over prBodySurfaceNouns only.
func NamesPRBodySurface(text string) bool {
	return containsAny(strings.ToLower(text), prBodySurfaceNouns)
}

func containsAny(folded string, needles []string) bool {
	for _, n := range needles {
		if strings.Contains(folded, n) {
			return true
		}
	}
	return false
}

// Detect classifies the routed instructions and returns the declared reporting
// obligations, assigning ids `ob-1`..`ob-N` in a FIXED order that does NOT
// depend on how the caller interleaved the sources: every SourceConcern in
// input order, then SourceOperatorConcern, then SourceReason. Determinism here
// is load-bearing — the prompt renderer and the review-time re-derivation must
// mint the same id for the same instruction.
//
// A SourceOperatorConcern whose trimmed text already appeared as a
// SourceConcern is SKIPPED: since #2623 the free-text operator_concern is also
// minted as a durable concern, so it otherwise arrives twice and would draw two
// ids for one instruction. The surviving SourceConcern keeps ITS OWN Untrusted
// flag, so the dedupe can only ever retain the more-quarantined of the two.
//
// Each Obligation carries its Source's Untrusted flag through unchanged, so a
// reporting obligation detected inside an acceptance-derived (untrusted)
// concern note stays marked as untrusted at every downstream render site.
//
// Returns nil when nothing is classified — the common case, and the one that
// keeps the fix-up prompt byte-identical to today.
func Detect(sources []Source) []Obligation {
	concernTexts := make(map[string]struct{}, len(sources))
	for _, s := range sources {
		if s.Kind == SourceConcern {
			concernTexts[strings.TrimSpace(s.Text)] = struct{}{}
		}
	}

	var out []Obligation
	for _, kind := range []SourceKind{SourceConcern, SourceOperatorConcern, SourceReason} {
		for _, s := range sources {
			if s.Kind != kind {
				continue
			}
			trimmed := strings.TrimSpace(s.Text)
			if trimmed == "" {
				continue
			}
			if kind == SourceOperatorConcern {
				if _, dup := concernTexts[trimmed]; dup {
					continue
				}
			}
			if !IsObligation(trimmed) {
				continue
			}
			out = append(out, Obligation{
				ID:     nextID(len(out)),
				Source: kind,
				Text:   Bound(trimmed),
				// Classify the PR-body surface off the SAME trimmed text the id
				// and excerpt derive from, so the flag is minted at exactly one
				// site (#2782) and every downstream consumer inherits it.
				PRBody:    NamesPRBodySurface(trimmed),
				Untrusted: s.Untrusted,
			})
		}
	}
	return out
}

// nextID renders the id for the obligation at index i (0-based) as `ob-<i+1>`.
func nextID(i int) string {
	return "ob-" + itoa(i+1)
}

// itoa avoids a strconv import for the tiny positive-int case.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	for n > 0 {
		pos--
		buf[pos] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[pos:])
}

// Bound caps text at MaxExcerptBytes, appending truncationMarker when it cut.
// The cut lands on a rune boundary so the result is always valid UTF-8.
func Bound(text string) string {
	if len(text) <= MaxExcerptBytes {
		return text
	}
	cut := MaxExcerptBytes
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + truncationMarker
}

// Undelivered joins the declared obligations against the agent's validated
// reports and returns, in DECLARED order, every obligation the operator should
// see about this pass.
//
// A PR-body obligation (PRBody true) is ALWAYS returned, with Status
// StatusUnsatisfiable, short-circuiting BEFORE the report lookup — INCLUDING
// when the agent reported `met`. The pass has no mechanism to write the PR
// body, so a `met` claim for such an obligation cannot be true, and silently
// honouring it is exactly what made PR #2775's rich counterfactual table read
// as complete while documenting only the first pass. Reporting it as
// unsatisfiable — not as an agent omission — tells the operator the
// instruction was structurally impossible without falsely accusing the agent
// of ignoring it.
//
// A non-PR-body obligation keeps the prior behaviour byte-for-byte: it is
// returned only when it carries no report whose Status is StatusMet. A report
// whose id is not in the declared set is ignored (it can satisfy nothing), and
// a declared obligation with a StatusDeclined report is returned with
// StatusDeclined — an honest decline is still an undelivered obligation the
// operator should see, it is simply not a silent one. The agent's stated
// REASON is not part of the finding: it is validated on the runner and never
// transmitted (see Report).
//
// Returns nil when every declared NON-PR-body obligation carries a `met`
// report and no PR-body obligation was declared, or when nothing was declared.
// That nil is acceptance criterion 3's no-noise control: a satisfied ordinary
// pass emits no signal at all.
func Undelivered(declared []Obligation, reports []Report) []Finding {
	byID := make(map[string]Report, len(reports))
	for _, r := range reports {
		// First report for an id wins; a duplicate cannot upgrade a decline.
		if _, seen := byID[r.ID]; !seen {
			byID[r.ID] = r
		}
	}
	var out []Finding
	for _, ob := range declared {
		// A PR-body obligation is unsatisfiable regardless of the report — the
		// pass could not have written the PR body — so it short-circuits ahead
		// of the `met` lookup below. This is the heart of the #2782 fix.
		if ob.PRBody {
			out = append(out, Finding{Obligation: ob, Status: StatusUnsatisfiable})
			continue
		}
		rep, ok := byID[ob.ID]
		if ok && rep.Status == StatusMet {
			continue
		}
		f := Finding{Obligation: ob, Status: StatusUnreported}
		if ok && rep.Status == StatusDeclined {
			f.Status = StatusDeclined
		}
		out = append(out, f)
	}
	return out
}
