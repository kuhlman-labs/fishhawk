// Package splitfiling holds the pure logic behind on-approval child filing for
// an approved plan that carries a split_proposal (#2057, E50.5). It is
// deliberately dependency-light — it imports only backend/internal/plan for the
// SplitProposal shape and owns neutral input/output types (PhaseEvidence,
// ChildSpec, ContractClassification, CapExceptionDraft) so the server approval
// hook (backend/internal/server/split_filing.go, a sibling slice) can drive it
// without this package taking a server dependency.
//
// Three pure operations:
//
//   - BuildChildSpecs turns a plan.SplitProposal into an ordered []ChildSpec —
//     one per phase, each carrying a symbol-set scope statement (prose, never a
//     stale file list), 0-based depends_on edges, parent+design issue references,
//     a bounded "Parent plan context" block carrying the approved parent plan's
//     summary and numbered approach (#5223), and a contract flag on the terminal
//     phase. The contract child additionally
//     carries the parent's acceptance criteria and, by construction, NO
//     "Closes #<parent>" line (a Closes line in a child ISSUE body is functionless
//     — GitHub auto-closes only from a PR/commit and only the enclosing issue; the
//     live parent-close mechanism is deferred to follow-up #2062).
//   - Classify (classify.go) decides the contract phase is delete-only by default
//     and governed-exception only when reachability evidence proves an atomic
//     rename overflows the resolved implement cap.
//   - DraftCapException (classify.go) renders the in-memory-only cap-exception
//     spec diff + PR body for the governed-exception case — never written to disk.
package splitfiling

import (
	"fmt"
	"path"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
)

// designIssueRef and deferralIssueRef name the issues every child body and the
// cap-exception draft reference: #2008 is the E50 design issue the split-proposal
// work descends from, and #2062 (E50.6) is the deferred close-parent watcher that
// will close the parent once the contract child lands. They are exported so the
// server hook and its tests can assert on the exact numbers.
const (
	// DesignIssue is the E50 design issue every filed child body references.
	DesignIssue = 2008
	// DeferralIssue is the E50.6 close-parent-watcher follow-up (#2062) that the
	// contract child and the parent acceptance-carrier comment defer parent
	// closure to. This slice ships no close-parent watcher.
	DeferralIssue = 2062
)

// BuildInput bundles the neutral inputs BuildChildSpecs needs: the approved
// plan's split proposal, the parent (run) issue number every child references,
// and the parent's acceptance criteria the terminal contract child carries.
// DesignIssue defaults to the package DesignIssue constant when zero.
//
// ParentSummary and ParentApproach are the approved parent plan's summary and
// numbered approach (#5223). Every child renders them as a bounded "Parent plan
// context" block (see renderParentPlanContext); both empty omits the block, so
// the Proposal is byte-identical to the pre-#5223 render.
type BuildInput struct {
	Proposal         plan.SplitProposal
	ParentIssue      int
	DesignIssue      int
	ParentAcceptance []string
	ParentSummary    string
	ParentApproach   []plan.ApproachStep
}

// ChildSpec is one conventions-neutral child issue the server hook files. It
// carries the fields the hook maps onto a refinement.ChildDraft (Title→Summary,
// Proposal, DoneMeans, AcceptanceCriteria, DependsOn) plus the classification
// metadata (PhaseIndex, IsContract) and issue references the hook renders into
// the body and depends_on edges.
type ChildSpec struct {
	// PhaseIndex is the child's 0-based phase index within the split proposal.
	PhaseIndex int
	// Title is the phase title, used as the child issue summary line.
	Title string
	// ScopeStatement is the symbol-set scope prose synthesized from the phase's
	// scope_hint (never a stale file list). It is also embedded in Proposal.
	ScopeStatement string
	// Proposal is the child issue body prose. It references the parent (run)
	// issue and the design issue and, for the contract child, states the deferral
	// to #2062 — but NEVER contains a "Closes #<parent>" line.
	Proposal string
	// DoneMeans is the child's done-means body section.
	DoneMeans string
	// DependsOn holds the 0-based phase indices this phase depends on, copied
	// verbatim from the split phase. The server hook resolves these to sibling
	// #N at filing time in wave order.
	DependsOn []int
	// ParentIssue is the parent (run) issue number every child references.
	ParentIssue int
	// DesignIssue is the design issue number every child body references (#2008).
	DesignIssue int
	// IsContract marks the terminal (last) phase — the contract child.
	IsContract bool
	// AcceptanceCriteria carries the parent's acceptance criteria, populated ONLY
	// on the contract child (the acceptance carrier). Non-contract children leave
	// it nil.
	AcceptanceCriteria []string
}

// BuildChildSpecs walks the split proposal's phases in order and returns one
// ChildSpec per phase (empty when the proposal has no phases). The terminal
// phase is flagged IsContract and carries the parent's acceptance criteria; no
// child carries a "Closes #<parent>" line.
func BuildChildSpecs(in BuildInput) []ChildSpec {
	design := in.DesignIssue
	if design == 0 {
		design = DesignIssue
	}
	phases := in.Proposal.Phases
	specs := make([]ChildSpec, 0, len(phases))
	// The parent plan context is the same for every child: render it once.
	parentContext := renderParentPlanContext(in.ParentIssue, in.ParentSummary, in.ParentApproach)
	last := len(phases) - 1
	for i, ph := range phases {
		isContract := i == last
		spec := ChildSpec{
			PhaseIndex:     i,
			Title:          ph.Title,
			ScopeStatement: scopeStatement(ph),
			DependsOn:      append([]int(nil), ph.DependsOn...),
			ParentIssue:    in.ParentIssue,
			DesignIssue:    design,
			IsContract:     isContract,
		}
		spec.Proposal = buildProposal(spec, isContract, parentContext)
		spec.DoneMeans = buildDoneMeans(isContract)
		if isContract {
			spec.AcceptanceCriteria = append([]string(nil), in.ParentAcceptance...)
		}
		specs = append(specs, spec)
	}
	return specs
}

// scopeStatement synthesizes the child's symbol-set scope prose from the phase's
// scope_hint. When the hint is present it is used verbatim (the planner authored
// it as a symbol-set statement). When absent it falls back to prose naming the
// affected packages derived from the phase's declared files — never the raw file
// list itself, which goes stale as the migration proceeds.
func scopeStatement(ph plan.SplitPhase) string {
	if hint := strings.TrimSpace(ph.ScopeHint); hint != "" {
		return "Symbol-set scope: " + hint
	}
	if pkgs := uniquePackages(ph.Scope); len(pkgs) > 0 {
		return "Symbol-set scope: migrate the consumers within " + strings.Join(pkgs, ", ") + "."
	}
	return "Symbol-set scope: migrate the consumers declared for this phase."
}

// uniquePackages returns the sorted unique directory of each declared file in
// the phase scope — the package-level projection of the file list.
func uniquePackages(scope *plan.Scope) []string {
	if scope == nil {
		return nil
	}
	seen := map[string]struct{}{}
	for _, f := range scope.Files {
		dir := path.Dir(strings.TrimSpace(f.Path))
		if dir == "" || dir == "." {
			continue
		}
		seen[dir] = struct{}{}
	}
	if len(seen) == 0 {
		return nil
	}
	out := make([]string, 0, len(seen))
	for d := range seen {
		out = append(out, d)
	}
	sort.Strings(out)
	return out
}

// buildProposal renders the child body prose. It references the parent (run)
// issue and the design issue and embeds the symbol-set scope statement, followed
// by the pre-rendered parent plan context block when it is non-empty. The
// contract child states the parent-close deferral to #2062 WITHOUT a "Closes #"
// line; a non-contract child states plainly that it references but does not close
// the parent.
func buildProposal(c ChildSpec, isContract bool, parentContext string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Phase %d of the approved split proposal for the over-cap change tracked on #%d", c.PhaseIndex+1, c.ParentIssue)
	if c.DesignIssue > 0 {
		fmt.Fprintf(&b, " (design: #%d)", c.DesignIssue)
	}
	b.WriteString(".\n\n")
	b.WriteString(c.ScopeStatement)
	b.WriteString("\n\n")
	if parentContext != "" {
		b.WriteString(parentContext)
		b.WriteString("\n\n")
	}
	if isContract {
		fmt.Fprintf(&b, "This is the terminal (contract) phase and the acceptance carrier for parent #%d: it carries the parent's acceptance criteria. It does NOT close the parent — GitHub auto-closes an issue only from a PR/commit and only the enclosing issue, so a close line here would be functionless. The parent is closed when this contract child lands, automated by follow-up #%d (E50.6) once it ships.", c.ParentIssue, DeferralIssue)
	} else {
		fmt.Fprintf(&b, "This is a transitional phase of the split for #%d; it references but does not close the parent.", c.ParentIssue)
	}
	b.WriteString("\n\n")
	b.WriteString(LandabilityCaveat)
	return b.String()
}

// MaxParentPlanContextChars bounds the rendered "Parent plan context" block
// (#5223), counted in characters (runes) and INCLUDING its heading line and any
// truncation note. The same block rides every one of the N child bodies, so a
// long plan must not inflate them all. The value is a body-size judgement, not
// a forge limit; TestBuildChildSpecs_ParentPlanContextBounded pins it.
const MaxParentPlanContextChars = 8000

// ParentPlanContextHeading opens the parent plan context block. Exported so the
// server hook's seam test asserts on the same literal the renderer writes.
const ParentPlanContextHeading = "**Parent plan context.**"

// parentPlanElision marks every place the bounded block cut text. It is one rune
// on purpose: a truncated description of L runes then renders as L+1 runes, so
// the rendered length is monotone in L and the largest-fit search is exact.
const parentPlanElision = "…"

// approachLine is one parent approach step prepared for rendering: its number
// and its description flattened onto a single line.
type approachLine struct {
	num  int
	desc []rune
}

// renderParentPlanContext renders the bounded block that carries the approved
// parent plan's summary and numbered approach onto every split child (#5223).
//
// WHY THE WHOLE PARENT APPROACH: plan-standard-v1 split phases declare no
// approach of their own (plan.SplitPhase carries title, scope_hint, scope and
// depends_on only), so "each child carries its phase's approach" is read as
// "each child carries the whole parent approach", labelled as context beside
// the phase's own symbol-set scope. That reading is inferred, not stated by the
// schema; the operator accepted it at the #5223 plan gate.
//
// It returns "" when the summary is blank and there are no steps, so a plan
// without either renders the pre-#5223 Proposal byte-for-byte. Otherwise the
// block is never longer than MaxParentPlanContextChars, and it degrades in a
// fixed order so the step list stays complete for as long as possible:
//
//  1. Everything fits: summary and every step in full, no note.
//  2. Summary first: the summary is shortened (or omitted) and every step is
//     still shown in full.
//  3. Steps next: the summary is omitted, EVERY step number is kept, and each
//     description longer than a common per-step limit is cut with an elision.
//  4. Not even one line per step fits: the block says so explicitly, naming the
//     step count, rather than listing a silently partial approach.
//
// Every truncated form ends with a note pointing at the parent's run plan.
func renderParentPlanContext(parentIssue int, summary string, steps []plan.ApproachStep) string {
	summary = strings.TrimSpace(summary)
	lines := parentApproachLines(steps)
	if summary == "" && len(lines) == 0 {
		return ""
	}
	header := ParentPlanContextHeading + " The approved parent plan's summary and full approach, carried for context when planning this phase on its own. This phase's own scope is the symbol-set scope above: a split phase declares no approach of its own, so every child carries the whole parent approach."
	parentRef := "the parent issue"
	if parentIssue > 0 {
		parentRef = fmt.Sprintf("parent #%d", parentIssue)
	}
	note := func(summaryPart, stepsPart string) string {
		parts := make([]string, 0, 2)
		for _, p := range []string{summaryPart, stepsPart} {
			if p != "" {
				parts = append(parts, p)
			}
		}
		return fmt.Sprintf("_Parent plan context truncated to fit the %d-character bound: %s. The full approved plan is on %s's Fishhawk run._",
			MaxParentPlanContextChars, strings.Join(parts, "; "), parentRef)
	}
	fullSteps := renderApproachLines(lines, -1)

	// 1. Everything fits.
	if full := assembleParentPlanContext(header, summary, fullSteps, ""); fitsParentPlanBound(full) {
		return full
	}

	stepsFullPart := ""
	if len(lines) > 0 {
		stepsFullPart = "every approach step is shown in full"
	}

	// 2. Shorten the summary first, keeping every step whole.
	if summary != "" {
		sumRunes := []rune(summary)
		shortenedNote := note("the summary is shortened (marked "+parentPlanElision+")", stepsFullPart)
		render := func(k int) string {
			cut := strings.TrimRightFunc(string(sumRunes[:k]), unicode.IsSpace) + parentPlanElision
			return assembleParentPlanContext(header, cut, fullSteps, shortenedNote)
		}
		if k, ok := largestFitting(1, len(sumRunes)-1, func(k int) bool { return fitsParentPlanBound(render(k)) }); ok {
			return render(k)
		}
	}
	summaryOmittedPart := ""
	if summary != "" {
		summaryOmittedPart = "the summary is omitted"
	}
	if summary != "" {
		if out := assembleParentPlanContext(header, "", fullSteps, note(summaryOmittedPart, stepsFullPart)); fitsParentPlanBound(out) {
			return out
		}
	}

	// 3. Summary gone and the full steps still overflow: keep EVERY step number
	// and shorten each description to the largest common per-step limit that fits.
	maxDesc := 0
	for _, l := range lines {
		if len(l.desc) > maxDesc {
			maxDesc = len(l.desc)
		}
	}
	stepsNote := note(summaryOmittedPart, "every approach step is kept, with long step descriptions shortened (marked "+parentPlanElision+")")
	render := func(limit int) string {
		return assembleParentPlanContext(header, "", renderApproachLines(lines, limit), stepsNote)
	}
	if limit, ok := largestFitting(0, maxDesc-1, func(limit int) bool { return fitsParentPlanBound(render(limit)) }); ok {
		return render(limit)
	}

	// 4. Not even one line per step fits: say so rather than list a partial
	// approach as if it were whole.
	return header + "\n\n" + fmt.Sprintf("_The parent approach has %d steps, too many to list even one line per step within the %d-character bound, so no step is listed here. The full approved plan is on %s's Fishhawk run._",
		len(lines), MaxParentPlanContextChars, parentRef)
}

// parentApproachLines prepares each approach step for rendering. A step's own
// number is kept (falling back to its 1-based position when unset) and its
// description's whitespace runs are collapsed so every step is exactly one line.
func parentApproachLines(steps []plan.ApproachStep) []approachLine {
	out := make([]approachLine, 0, len(steps))
	for i, st := range steps {
		num := st.Step
		if num <= 0 {
			num = i + 1
		}
		out = append(out, approachLine{num: num, desc: []rune(strings.Join(strings.Fields(st.Description), " "))})
	}
	return out
}

// renderApproachLines renders "N. description" per step. A negative limit keeps
// every description whole; otherwise a description longer than limit runes is
// cut to limit runes plus the elision marker.
func renderApproachLines(lines []approachLine, limit int) []string {
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		desc := string(l.desc)
		if limit >= 0 && len(l.desc) > limit {
			desc = strings.TrimRightFunc(string(l.desc[:limit]), unicode.IsSpace) + parentPlanElision
		}
		out = append(out, strings.TrimRight(fmt.Sprintf("%d. %s", l.num, desc), " "))
	}
	return out
}

// assembleParentPlanContext joins the block's present sections with blank lines.
func assembleParentPlanContext(header, summary string, stepLines []string, note string) string {
	parts := []string{header}
	if summary != "" {
		parts = append(parts, "Summary: "+summary)
	}
	if len(stepLines) > 0 {
		parts = append(parts, "Approach:\n\n"+strings.Join(stepLines, "\n"))
	}
	if note != "" {
		parts = append(parts, note)
	}
	return strings.Join(parts, "\n\n")
}

func fitsParentPlanBound(s string) bool {
	return utf8.RuneCountInString(s) <= MaxParentPlanContextChars
}

// largestFitting returns the largest n in [lo, hi] for which ok(n) holds,
// searching on the assumption that ok is true up to some n and false after it.
// It only ever returns an n it has seen ok accept, so a predicate that is not
// perfectly monotone can cost a little length but never the bound. It reports
// false when ok(lo) fails or the range is empty.
func largestFitting(lo, hi int, ok func(int) bool) (int, bool) {
	if hi < lo || !ok(lo) {
		return 0, false
	}
	best := lo
	for lo++; lo <= hi; {
		mid := lo + (hi-lo)/2
		if ok(mid) {
			best, lo = mid, mid+1
		} else {
			hi = mid - 1
		}
	}
	return best, true
}

// LandabilityCaveat is the honest statement carried on EVERY filed child body
// and mirrored into the parent comment (#2412): split-filing checks each phase's
// declared file count against the implement cap and, when the runner shipped a
// reachability sweep, Go symbol reachability across phase boundaries — but it
// does NOT verify independent landability, and specifically does NOT detect
// PATH-KEYED couplings. It names the exact coupling class that broke run
// 1d94db6f so the phase author verifies the tree builds before landing the phase
// alone. Exported so the server hook mirrors ONE sentence into the parent
// acceptance-carrier comment rather than hand-copying drift-prone prose.
const LandabilityCaveat = "**Landability is not verified.** Split-filing checks this phase's declared file count against the implement cap and, when the runner shipped a reachability sweep, Go symbol reachability across phase boundaries. It does NOT verify that this phase lands independently, and specifically does NOT detect PATH-KEYED couplings — literal file-path strings in surface-sweep tables, path-keyed maps in tests, and build/rebuild matrices — so a phase that moves or renames files may leave such a coupling dangling and may not be individually green. Verify the tree builds before landing this phase alone (this is the coupling class that broke run 1d94db6f)."

// buildDoneMeans renders the child done-means section. The non-contract phase
// deliberately no longer asserts "the resulting tree compiles as an
// at-or-under-cap intermediate" (#2412) — that claim is unsatisfiable on its
// face for a change like #2410, since split-filing does not verify independent
// landability (see the landabilityCaveat on every child body). It states only
// what the phase actually guarantees: its symbol-set scope is migrated.
func buildDoneMeans(isContract bool) string {
	if isContract {
		return "The contract phase deletes the transitional names and the intermediate compiles; the parent's acceptance criteria (carried below) are satisfied."
	}
	return "The phase's symbol-set scope is migrated; independent landability of this phase is unverified (see the landability caveat in the body — verify the tree builds before landing this phase alone)."
}
