package prompt

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
)

// MaxRevisionBasePlanBytes caps the revision BASE plan blob rendered into the
// plan-gate `revise` prompt (#3087). Raised from the historical inline 4000
// that BOTH renderers — buildPlan's and buildGroomingPropose's `### Revision
// constraint` blocks — sliced independently with a bare `...[truncated]`
// marker, mid-sentence and mid-step.
//
// The observed live loss is run 6059c011 (#3048): an eleven-step plan was cut
// inside step 2, so the agent told to "REVISE the prior plan below" and to
// "not discard the parts of the plan the constraint does not touch" could see
// two of eleven steps. Nothing in the prompt said which nine were missing.
//
// 60000 is chosen to sit far ABOVE realistic plan size rather than at a size a
// real plan can reach: a 45-file, 15-step standard_v1 plan carrying acceptance
// criteria marshals to roughly 15-25 KB under json.MarshalIndent, so this
// leaves ~2.5x headroom and every realistic revise now inlines the WHOLE prior
// plan. It is a judgement, not a bound that exists — nothing prevents a plan
// artifact from exceeding it — which is why exceeding it is no longer silent:
// renderRevisionBase falls to a STEP-COMPLETE, self-describing digest.
const MaxRevisionBasePlanBytes = 60000

// maxRevisionBaseFieldBytes caps one individually-variable body inside the
// over-cap digest (a step description, a summary, a test strategy, one
// acceptance-criterion statement). A cut here draws a NAMED elision carrying
// the field it belongs to and its byte accounting, so a shortened body always
// says which body it is.
const maxRevisionBaseFieldBytes = 2000

// revisionBaseShrinkFieldBytes is maxRevisionBaseFieldBytes' replacement in
// the digest's SECOND pass — the shrink pass that fires only when the first
// pass still overflows MaxRevisionBasePlanBytes.
const revisionBaseShrinkFieldBytes = 120

// maxRevisionBaseStepIdentities bounds how many approach-step IDENTITY lines
// the digest renders, and maxRevisionBaseListItems bounds every other list it
// renders (acceptance criteria, decomposition sub-plans, split phases, risks).
//
// These bounds are what make the digest BOUNDED BY CONSTRUCTION, which is the
// honest form of the step-completeness guarantee (#3087 approval condition 1):
// the digest guarantees that no step is SILENTLY absent, NOT that every step
// identity is always rendered. Past the budget the renderer emits one
// structured line stating exactly how many further steps exist and their
// position range, so listed + counted == total holds arithmetically and an
// unbounded plan still yields a bounded digest whose omission is named.
//
// The resulting worst case for the shrink pass: a ~1500-byte header, plus
// maxRevisionBaseStepIdentities lines of the form
// "Step 999999: [approach step body elided — 999999 bytes withheld]" (~60
// bytes each, ~12000 total), plus maxRevisionBaseListItems criterion-id lines
// capped at revisionBaseShrinkFieldBytes (~300 bytes each, ~30000 total), plus
// a handful of counted-remainder lines. That is comfortably under
// MaxRevisionBasePlanBytes, so the shrink pass terminates under the cap.
const (
	maxRevisionBaseStepIdentities = 200
	maxRevisionBaseListItems      = 100
)

// revisionBaseRetrievalPointer names the concrete recovery path for a revision
// base that did not fit whole: the run's own plan artifact, which IS
// addressable — unlike a truncated free-text channel, the base is a stored
// document a verb can fetch. Worded so it does not DEPEND on that verb being
// wired into a given plan agent's tool set (the same best-effort caveat
// priorRejectionRetrievalPointer and revisionConstraintRetrievalPointer carry,
// ADR-021), which is why revisionBaseElidedNotice ALSO instructs the planner to
// declare the gap in risks_and_assumptions regardless.
const revisionBaseRetrievalPointer = "To recover what is not shown: read this run's plan artifact in full (via fishhawk_get_plan if it is available to you), or ask the operator for the prior plan."

// revisionBaseElidedNotice is the renderer-emitted instruction that fires when
// and only when the base did not fit whole. Same shape and same placement rule
// as revisionConstraintElidedNotice (#2871): it is written by the RENDERER
// into the stable scaffolding OUTSIDE and BEFORE the elidable text, so a cut
// that removes the base's tail cannot also remove the instruction to notice it.
const revisionBaseElidedNotice = "IMPORTANT: the revision base below did NOT fit whole and was ELIDED — what follows is a digest or a cut prefix, NOT the entire prior document. You MUST record in the plan's risks_and_assumptions that the revision base arrived incomplete, naming what you could not see. The elision manifest and byte accounting inside it state exactly what is missing and how to fetch it.\n\n"

// renderRevisionBase is the pure entry point for the revision-base channel: it
// returns the text to render and whether the base was elided.
//
// The boundary is strictly `>`, like every sibling channel (CapText,
// CapTextWithRetrieval): a base of exactly MaxRevisionBasePlanBytes bytes
// returns VERBATIM with elided=false, so the at-cap case is byte-unchanged.
//
// Over cap the fallback ORDER is load-bearing. It first tries the structured
// digest (revisionBaseDigest), which is step-complete and self-describing. Only
// when the blob does not decode as a plan carrying approach steps — a grooming
// report, which is what the buildGroomingPropose branch's base actually is, or
// a malformed blob — does it fall to CapTextWithRetrieval: the ADR-077 elision
// marker with byte accounting, the INCOMPLETE statement and the retrieval
// pointer. NEVER CapText's bare "...[truncated]", which severed the prior plan
// mid-sentence and told the reader nothing about what was lost.
func renderRevisionBase(base string) (string, bool) {
	if len(base) <= MaxRevisionBasePlanBytes {
		return base, false
	}
	if digest, ok := revisionBaseDigest(base); ok {
		return digest, true
	}
	capped, _ := CapTextWithRetrieval(base, MaxRevisionBasePlanBytes, revisionBaseRetrievalPointer)
	return capped, true
}

// writeRevisionBase renders the revision-base block shared by buildPlan and
// buildGroomingPropose — one renderer for both call sites, in the same spirit
// as writeOperatorConstraint owning the constraint channel, so the two cannot
// drift apart. leadLine is the noun-specific lead ("Prior plan (the revision
// base):" vs "Prior report (the revision base):").
//
// When it elides it writes revisionBaseElidedNotice FIRST — outside the
// elidable text, the #2871 placement rule — and returns true so the caller can
// stop asserting a truncation that no longer happens on a base that fit.
func writeRevisionBase(b *strings.Builder, base, leadLine string) bool {
	rendered, elided := renderRevisionBase(base)
	if elided {
		b.WriteString(revisionBaseElidedNotice)
	}
	b.WriteString(leadLine)
	b.WriteString("\n\n")
	b.WriteString(rendered)
	b.WriteString("\n\n")
	return elided
}

// revisionBaseDigest renders an over-cap revision base as a step-complete
// digest, returning ok=false when the blob is not a plan this digest can speak
// about (any decode error, or a decoded value carrying zero approach steps —
// there is nothing step-complete to say, and an empty digest would be a worse
// lie than a loud cut).
//
// It decodes with a PLAIN json.Unmarshal, deliberately NOT plan.Parse: Parse
// strict-decodes with DisallowUnknownFields, so a plan carrying a field this
// binary does not know would be pushed onto the byte-cut path even though it is
// perfectly structured. The same blob is decoded a SECOND time into a generic
// map so the digest can name every top-level key it does not render — a field
// dropped by the typed decode is then listed in the elision manifest rather
// than vanishing (#3087 approval condition 2).
func revisionBaseDigest(base string) (string, bool) {
	var p plan.Plan
	if err := json.Unmarshal([]byte(base), &p); err != nil {
		return "", false
	}
	if len(p.Approach) == 0 {
		return "", false
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal([]byte(base), &doc); err != nil {
		return "", false
	}
	digest := renderRevisionBaseDigest(base, &p, doc, maxRevisionBaseFieldBytes, false)
	if len(digest) > MaxRevisionBasePlanBytes {
		// Second pass (the shrink pass): drop step and criterion BODIES
		// entirely, keeping every identity line, and replace the auxiliary
		// lists with counted lines. This is what makes the identity contract
		// hold whatever the input size — see maxRevisionBaseStepIdentities.
		digest = renderRevisionBaseDigest(base, &p, doc, revisionBaseShrinkFieldBytes, true)
	}
	return digest, true
}

// revisionBaseRenderedKeys is the set of the prior plan's top-level JSON keys
// the digest DOES speak about. Every other key present in the document — a
// known field the digest omits, or a field this binary's plan.Plan does not
// know at all — is named in the elision manifest. Kept as an explicit set
// rather than derived from the struct so adding a plan field cannot silently
// widen what the manifest claims is rendered.
var revisionBaseRenderedKeys = map[string]bool{
	"plan_version":                 true,
	"summary":                      true,
	"scope":                        true,
	"approach":                     true,
	"verification":                 true,
	"risks_and_assumptions":        true,
	"predicted_runtime_minutes":    true,
	"predicted_runtime_confidence": true,
	"decomposition":                true,
	"split_proposal":               true,
}

// revisionBaseRender accumulates the digest body while tracking how many bytes
// of SOURCE-derived content it actually carried, so the document-level
// accounting can be computed once the body is known and written ahead of it.
type revisionBaseRender struct {
	b strings.Builder
	// sourceBytes counts only bytes drawn from the prior document that
	// survived into the digest. Everything else — JSON punctuation, key
	// names, this renderer's own prose — is deliberately NOT counted, so the
	// reported elision is conservative in the safe direction.
	sourceBytes int
	fieldCap    int
}

// capField renders one variable-length body, accumulating the bytes carried
// and appending a NAMED elision (the field it belongs to plus its byte
// accounting) when it cuts. label names the field so a shortened body always
// says which body it is.
func (r *revisionBaseRender) capField(label, s string) string {
	if len(s) <= r.fieldCap {
		r.sourceBytes += len(s)
		return s
	}
	kept := strings.ToValidUTF8(s[:r.fieldCap], "")
	r.sourceBytes += len(kept)
	return kept + fmt.Sprintf("...[ELIDED — %s: %d of %d bytes shown, %d bytes dropped]",
		label, len(kept), len(s), len(s)-len(kept))
}

// droppedBody renders a body that was withheld ENTIRELY, keeping the identity
// the line exists to carry. It accumulates no source bytes: nothing was
// carried, and the document-level accounting must say so.
func droppedBody(label string, s string) string {
	return fmt.Sprintf("[%s elided — %d bytes withheld]", label, len(s))
}

// listBudget clamps a list length to the per-digest budget, returning how many
// items to render.
func listBudget(total, budget int) int {
	if total > budget {
		return budget
	}
	return total
}

// remainderLine emits the ONE structured line that names and COUNTS the items
// past the budget, so listed + counted == total is arithmetically checkable
// from the rendered text (#3087 approval condition 1).
func (r *revisionBaseRender) remainderLine(kind string, listed, total int) {
	if listed >= total {
		return
	}
	fmt.Fprintf(&r.b, "...[%d further %s NOT rendered: positions %d..%d of %d total. %s]\n",
		total-listed, kind, listed+1, total, total, revisionBaseRetrievalPointer)
}

// renderRevisionBaseDigest is the digest body renderer, parameterized by the
// per-field cap and by whether step/criterion bodies are dropped outright, so
// the first pass and the shrink pass share one implementation and cannot
// disagree about the step-identity invariant.
func renderRevisionBaseDigest(base string, p *plan.Plan, doc map[string]json.RawMessage, fieldCap int, dropBodies bool) string {
	r := &revisionBaseRender{fieldCap: fieldCap}

	// (b) plan-level scalars.
	fmt.Fprintf(&r.b, "plan_version: %s\n", r.capField("plan_version", p.PlanVersion))
	fmt.Fprintf(&r.b, "summary: %s\n", r.capField("summary", p.Summary))
	fmt.Fprintf(&r.b, "predicted_runtime_minutes: %d (confidence: %s)\n\n",
		p.PredictedRuntimeMinutes, p.PredictedRuntimeConfidence)

	// (c) EVERY approach step's identity, in order, up to the identity budget.
	listed := listBudget(len(p.Approach), maxRevisionBaseStepIdentities)
	r.b.WriteString("Approach steps:\n")
	for i := 0; i < listed; i++ {
		s := p.Approach[i]
		body := r.capField(fmt.Sprintf("approach step %d body", s.Step), s.Description)
		if dropBodies {
			body = droppedBody(fmt.Sprintf("approach step %d body", s.Step), s.Description)
		}
		fmt.Fprintf(&r.b, "Step %d: %s\n", s.Step, body)
	}
	r.remainderLine("approach steps", listed, len(p.Approach))
	r.b.WriteString("\n")

	// (d) verification + every acceptance criterion's id (the downstream join
	// key, so ids are never dropped) and statement.
	fmt.Fprintf(&r.b, "verification.test_strategy: %s\n",
		r.capField("verification.test_strategy", p.Verification.TestStrategy))
	fmt.Fprintf(&r.b, "verification.rollback_plan: %s\n",
		r.capField("verification.rollback_plan", p.Verification.RollbackPlan))
	crit := p.Verification.AcceptanceCriteria
	if len(crit) > 0 {
		critListed := listBudget(len(crit), maxRevisionBaseListItems)
		r.b.WriteString("Acceptance criteria:\n")
		for i := 0; i < critListed; i++ {
			c := crit[i]
			id := r.capField("acceptance criterion id", c.ID)
			stmt := r.capField(fmt.Sprintf("acceptance criterion %q statement", c.ID), c.Statement)
			if dropBodies {
				stmt = droppedBody(fmt.Sprintf("acceptance criterion %q statement", c.ID), c.Statement)
			}
			fmt.Fprintf(&r.b, "- %s: %s\n", id, stmt)
		}
		r.remainderLine("acceptance criteria", critListed, len(crit))
	}
	r.b.WriteString("\n")

	// (e) decomposition sub-plan titles and split_proposal phase titles, and
	// (f) risks_and_assumptions. The shrink pass replaces each with a counted
	// line rather than rendering it, which is what bounds the second pass.
	r.writeAux("decomposition sub-plans", subPlanTitles(p), dropBodies)
	r.writeAux("split_proposal phases", splitPhaseTitles(p), dropBodies)
	r.writeAux("risks_and_assumptions entries", p.RisksAndAssumptions, dropBodies)

	body := r.b.String()

	// (a) The header, written LAST and prepended, because the document-level
	// accounting is only known once the body is rendered.
	var h strings.Builder
	fmt.Fprintf(&h, "The prior plan is %d bytes — over the %d-byte revision-base cap — so it is NOT inlined whole. "+
		"What follows is a STEP-COMPLETE DIGEST rendered by Fishhawk from the plan artifact itself: no approach step is silently absent. %s\n\n",
		len(base), MaxRevisionBasePlanBytes, revisionBaseRetrievalPointer)
	elided := len(base) - r.sourceBytes
	fmt.Fprintf(&h, "Revision base accounting (whole document): %d original bytes, %d rendered bytes, %d elided bytes "+
		"(rendered + elided == original; elided is derived as original - rendered, so it accounts for EVERY byte not carried "+
		"forward — JSON punctuation and key names included — and never under-reports).\n\n",
		len(base), r.sourceBytes, elided)
	fmt.Fprintf(&h, "Revision base totals: %d approach steps, %d scope.files paths, %d acceptance criteria, "+
		"%d decomposition sub-plans, %d split_proposal phases, %d risks_and_assumptions entries.\n\n",
		len(p.Approach), len(p.Scope.Files), len(p.Verification.AcceptanceCriteria),
		len(subPlanTitles(p)), len(splitPhaseTitles(p)), len(p.RisksAndAssumptions))
	writeRevisionBaseManifest(&h, doc)

	return h.String() + body
}

// writeAux renders one auxiliary title/entry list, or — in the shrink pass —
// the counted line that stands in for it.
func (r *revisionBaseRender) writeAux(kind string, items []string, dropBodies bool) {
	if len(items) == 0 {
		return
	}
	if dropBodies {
		fmt.Fprintf(&r.b, "...[%d %s NOT rendered in this shrunken digest. %s]\n\n",
			len(items), kind, revisionBaseRetrievalPointer)
		return
	}
	fmt.Fprintf(&r.b, "%s:\n", kind)
	listed := listBudget(len(items), maxRevisionBaseListItems)
	for i := 0; i < listed; i++ {
		fmt.Fprintf(&r.b, "- %s\n", r.capField(kind+" entry", items[i]))
	}
	r.remainderLine(kind, listed, len(items))
	r.b.WriteString("\n")
}

// writeRevisionBaseManifest names every top-level key of the prior document the
// digest does NOT render, so a field the typed decode dropped — including one
// this binary's plan.Plan does not know — is listed rather than vanishing. Key
// names are rendered through sanitizeScopePath: they are document-supplied text
// landing inside a trusted prompt section, and a key carrying a newline would
// otherwise put attacker-chosen text at column 0.
func writeRevisionBaseManifest(h *strings.Builder, doc map[string]json.RawMessage) {
	var missing []string
	for k := range doc {
		if !revisionBaseRenderedKeys[k] {
			missing = append(missing, k)
		}
	}
	if len(missing) == 0 {
		h.WriteString("Elision manifest: every top-level key of the prior plan is represented above.\n\n")
		return
	}
	sort.Strings(missing)
	listed := listBudget(len(missing), maxRevisionBaseListItems)
	h.WriteString("Elision manifest — top-level keys of the prior plan this digest does NOT render:\n")
	for i := 0; i < listed; i++ {
		fmt.Fprintf(h, "- %s\n", sanitizeScopePath(missing[i]))
	}
	if listed < len(missing) {
		fmt.Fprintf(h, "- ...[%d further top-level keys NOT named]\n", len(missing)-listed)
	}
	h.WriteString("\n")
}

// subPlanTitles returns the decomposition sub-plan titles, nil when the plan
// declares no decomposition.
func subPlanTitles(p *plan.Plan) []string {
	if p.Decomposition == nil {
		return nil
	}
	out := make([]string, 0, len(p.Decomposition.SubPlans))
	for _, sp := range p.Decomposition.SubPlans {
		out = append(out, sp.Title)
	}
	return out
}

// splitPhaseTitles returns the split_proposal phase titles, nil when the plan
// declares no split.
func splitPhaseTitles(p *plan.Plan) []string {
	if p.SplitProposal == nil {
		return nil
	}
	out := make([]string, 0, len(p.SplitProposal.Phases))
	for _, ph := range p.SplitProposal.Phases {
		out = append(out, ph.Title)
	}
	return out
}

// The three conditional lead sentences for the #2516 enumerated carry-forward
// block, selected by whether the revision-base blob above it was rendered and
// whether it was elided (#3087). Before this change the block asserted
// unconditionally that the blob "is TRUNCATED at 4000 bytes" — after the cap
// raise that is false on every realistic revise, and it was already false when
// no blob was rendered at all.
//
// Only the truncation CLAIM varies. scopeCarryForwardObligation — the BINDING
// carry-forward instruction and the scope_removals / scope-regression language
// — is shared verbatim by all three branches, so no branch can drift into a
// weaker obligation. And the two non-elided leads keep the list authoritative
// on its own INDEPENDENT ground: it is resolved from the newest
// plan_scope_retry entry, which on a corrective re-dispatch is NOT the newest
// artifact, so it supersedes the blob whether or not the blob is complete.
const (
	scopeCarryForwardElidedLead = "The prior-plan blob above was ELIDED — it did not fit whole, so it may not show the whole scope; " +
		"its elision manifest and byte accounting state exactly what is missing and how to fetch it. " +
		"THIS LIST — not that blob — is the revision base's scope. "

	scopeCarryForwardWholeLead = "The prior-plan blob above is the COMPLETE prior plan — it was delivered whole, not truncated. " +
		"THIS LIST nonetheless remains the authoritative binding scope set: it is derived server-side from the newest " +
		"plan_scope_retry entry, which on a corrective re-dispatch is NOT the newest plan artifact, so it supersedes " +
		"the blob regardless of the blob's completeness. "

	scopeCarryForwardNoBaseLead = "No prior-plan blob is included above. THIS LIST is the revision base's scope, derived server-side " +
		"from the newest plan_scope_retry entry. "

	scopeCarryForwardObligation = "The revised plan MUST carry EVERY path below forward into scope.files (or into the owning " +
		"decomposition sub-plan / split_proposal phase scope), unless it DECLARES the drop in the top-level scope_removals " +
		"array with a reason. A path that simply disappears is a scope regression: the plan gate refuses it and " +
		"re-dispatches this stage.\n\n"
)
