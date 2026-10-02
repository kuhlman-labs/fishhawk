// Package escalation is the shared, pure evaluation core for a workflow's
// per-path `escalations` declaration (E53.4 / #2227). It carries no I/O, no
// HTTP shapes and no persistence — only the firing walk, the composition
// hand-off to spec, the ONE operator-facing renderer, and the fingerprint the
// audit de-duplication keys on.
//
// It exists for the SAME reason backend/internal/appliesto does: two
// enforcement seams in different packages consume the firing decision, and a
// seam that grew its own copy of the walk would drift from the other. The
// seams are:
//
//   - the APPROVAL GATE (backend/internal/server/quorum.go + approvals.go) —
//     raises the approval count, the group membership and the minimum
//     permission at BOTH the quorum count and the pre-Submit 403;
//   - DELEGATION RESOLUTION (backend/internal/delegation) — clamps the
//     resolved action matrix with the composed `max_autonomy` CEILING, LAST,
//     after the workflow tier and after every explicit `actions` override.
//
// A third consumer reads the FIRED set rather than the composed requirements:
// the plan- and implement-review loops attach every persona a fired
// escalation names in `require.reviewers` (ADR-084 D2(c) / E55.9 / #3754)
// through PersonaAttachments. That dimension is deliberately NOT part of
// spec.ComposedRequirements — a reviewers-only firing composes to the zero
// value, so the two seams above see "nothing raised" for it — which is why
// the attachment is derived here from Result.Fired instead.
//
// Both reach this package through the ONE server-side resolver
// (backend/internal/server/escalation_gate.go), which is also the single
// `escalation_fired` audit emit point — so a `max_autonomy`-only escalation on
// a workflow with no approval gate, which changes behaviour purely through the
// delegation clamp, is audited exactly as an approvals escalation is.
//
// FAIL-CLOSED IS UNIFORM, and deliberately asymmetric to the host packages'
// advisory sweeps — the same asymmetry appliesto's header names. Evaluate
// RETURNS a Match error rather than swallowing it: an escalation declaration
// that cannot be evaluated must never resolve to "nothing raised", because
// "nothing raised" is indistinguishable from the control being absent. Every
// caller turns that error into a refusal (a retryable 503 at the approval
// gate, an omitted delegation surface at the delegation seam). Writing
// `if err != nil { return Result{}, nil }` here by analogy with the server's
// runScopePrecheck / runSurfaceSweep fail-OPEN sweeps is the tempting bug this
// comment exists to name.
//
// The dependency direction is verified by compilation: this package imports
// only `spec`, and `spec` does not import it — a cycle fails `go build`.
package escalation

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// Fired is one escalation that matched the change, carrying its DECLARATION
// INDEX so an operator-facing message can name the rule by the position it
// occupies in the workflow document — the same coordinate the validation
// errors report at (/workflows/<name>/escalations/<i>).
type Fired struct {
	Index      int
	Escalation spec.Escalation
}

// Result is one evaluation: which escalations fired, and the STRICTEST
// requirement composed across them.
//
// The zero value is "nothing fired, nothing raised" — what a workflow
// declaring no escalations, and a change matching none of the ones it
// declares, both evaluate to. The no-escalation case is therefore a VALUE
// rather than a special case, which is what lets the delegation seam demand a
// resolver unconditionally instead of tolerating a nil one.
type Result struct {
	Fired        []Fired
	Requirements spec.ComposedRequirements
}

// Any reports whether at least one escalation fired.
func (r Result) Any() bool { return len(r.Fired) > 0 }

// Evaluate walks the declarations IN ORDER, matches each one's predicate
// against the change, and composes the strictest requirement over the hits.
//
// ORDER of the walk affects only the reported Fired slice (which is kept in
// declaration order so the rendering is stable); it CANNOT affect
// Requirements, because spec.ComposeEscalations is max / min / set-union per
// dimension — commutative and associative. That is what makes "never
// last-match-wins" a structural property rather than a convention.
//
// A Match error is RETURNED with the zero Result. See the package header: the
// caller fails closed on it, never treats it as no-match.
func Evaluate(escalations []spec.Escalation, change spec.Change) (Result, error) {
	var out Result
	var fired []spec.Escalation
	for i := range escalations {
		ok, err := escalations[i].Match.Match(change)
		if err != nil {
			return Result{}, fmt.Errorf("escalation %d: match: %w", i, err)
		}
		if !ok {
			continue
		}
		out.Fired = append(out.Fired, Fired{Index: i, Escalation: escalations[i]})
		fired = append(fired, escalations[i])
	}
	out.Requirements = spec.ComposeEscalations(fired)
	return out, nil
}

// RenderFired is THE operator-facing summary of an evaluation: which
// predicates fired and what they raised. The audit payload AND the run-status
// `escalations` block both render through it, so the two surfaces cannot
// describe the same firing differently — a run read saying the gate needs two
// approvals while the audit row says three is a governance-chain defect, not a
// cosmetic one.
//
// An empty result renders the explicit "no escalation fired" sentence rather
// than the empty string, so a payload carrying it is never ambiguous between
// "evaluated, nothing fired" and "never evaluated".
func RenderFired(res Result) string {
	if !res.Any() {
		return "no escalation fired"
	}
	var b strings.Builder
	parts := make([]string, 0, len(res.Fired))
	for _, f := range res.Fired {
		parts = append(parts, fmt.Sprintf("escalation %d (%s)", f.Index, renderPredicate(f.Escalation.Match)))
	}
	fmt.Fprintf(&b, "%s fired: %s", pluralFired(len(res.Fired)), strings.Join(parts, "; "))
	if raised := renderRequirements(res.Requirements); raised != "" {
		fmt.Fprintf(&b, ". Raised: %s", raised)
	}
	return b.String()
}

func pluralFired(n int) string {
	if n == 1 {
		return "1 escalation"
	}
	return fmt.Sprintf("%d escalations", n)
}

// renderPredicate names a predicate's declared criteria in a fixed order, so
// the same declaration always renders the same string (the property
// Fingerprint's stability rests on).
func renderPredicate(p spec.Predicate) string {
	var parts []string
	if len(p.Paths) > 0 {
		parts = append(parts, "paths="+strings.Join(p.Paths, ","))
	}
	if len(p.Labels) > 0 {
		parts = append(parts, "labels="+strings.Join(p.Labels, ","))
	}
	if len(p.ChangeKinds) > 0 {
		parts = append(parts, "change_kind="+strings.Join(p.ChangeKinds, ","))
	}
	if len(p.Triggers) > 0 {
		forms := make([]string, len(p.Triggers))
		for i, t := range p.Triggers {
			forms[i] = string(t)
		}
		parts = append(parts, "trigger="+strings.Join(forms, ","))
	}
	if len(parts) == 0 {
		return "no criterion"
	}
	return strings.Join(parts, " ")
}

// renderRequirements names each raised dimension in a fixed order. Empty when
// nothing was raised.
func renderRequirements(req spec.ComposedRequirements) string {
	var parts []string
	if req.Count != nil {
		parts = append(parts, fmt.Sprintf("approvals.count=%d", *req.Count))
	}
	if len(req.MemberOf) > 0 {
		// Already sorted + de-duplicated by ComposeEscalations; sorted again
		// here so this renderer does not depend on that being true.
		groups := append([]string(nil), req.MemberOf...)
		sort.Strings(groups)
		parts = append(parts, "approvals.member_of="+strings.Join(groups, "+"))
	}
	if req.MinPermission != "" {
		parts = append(parts, "approvals.min_permission="+req.MinPermission)
	}
	if req.MaxAutonomy != "" {
		parts = append(parts, "max_autonomy="+string(req.MaxAutonomy))
	}
	return strings.Join(parts, ", ")
}

// Fingerprint is a stable hash over the RENDERED fired set and composed
// requirements — the key the audit de-duplication compares.
//
// It hashes the render rather than the structs deliberately: RenderFired is
// already the surface an operator reads, so two evaluations with the same
// fingerprint are two evaluations an operator could not tell apart, which is
// exactly the equivalence the de-duplication wants. A genuinely CHANGED fired
// set or raised value (a scope amendment moving the plan's files into an
// escalated path, say) renders differently and therefore fingerprints
// differently — and that second entry is precisely what an operator must see.
func Fingerprint(res Result) string {
	sum := sha256.Sum256([]byte(RenderFired(res)))
	return hex.EncodeToString(sum[:])
}

// RuleKeyBytes is the hex length of a RuleKey: the leading 8 bytes of the
// sha256 over the canonical rendering, which is short enough to read in an
// audit payload and wide enough that a collision between two declarations in
// one workflow document is not a practical concern.
const RuleKeyBytes = 16

// RuleKey is the STABLE, content-derived key for ONE escalation declaration —
// the join key a decision index uses to follow a rule across workflow-document
// edits, where the positional Index cannot.
//
// It is derived from the DECLARATION'S OWN CONTENT ALONE and carries NO
// positional index, which gives it exactly these properties:
//
//   - REORDERING unrelated declarations leaves a matched rule's key unchanged
//     (the defect the positional index has, and the reason this is not built
//     on RenderFired or Fingerprint — both are RESULT-level and both embed
//     "escalation <i>", so a key derived from either would move on an
//     unrelated reorder);
//   - editing ANOTHER rule leaves this rule's key unchanged;
//   - editing THIS rule's globs, labels, change kinds, triggers or `require`
//     clamp CHANGES its key — a rule whose meaning moved is a different rule.
//     `require.reviewers` (E55.9 / #3754) is part of the clamp: it is rendered
//     ONLY when non-empty, so the key of every rule that declares no
//     reviewers is byte-identical to its pre-#3754 key (pinned by
//     TestRuleKey_ReviewersLessRuleKeyIsStable), and it is rendered as a
//     sorted list, so permuting it does not move the key.
//
// Within one rule each match criterion's list is SORTED before hashing,
// because spec.Predicate evaluates each list as an unordered OR: permuting
// globs inside one rule is not a change to what the rule matches, so it must
// not move the key. The sort operates on copies, so RuleKey never mutates its
// argument (the parsed spec.Workflow is shared across gate evaluations). Each
// element is additionally LENGTH-PREFIXED before it is joined, so a value
// carrying the renderer's own delimiters cannot make two DIFFERENT
// declarations key identically — see renderRuleForKey.
//
// RuleKey is NOT Fingerprint. Fingerprint is the RESULT-level de-duplication
// key over a whole evaluation (what fired plus what was composed); RuleKey is
// the RULE-level content key for one declaration.
func RuleKey(e spec.Escalation) string {
	sum := sha256.Sum256([]byte(renderRuleForKey(e)))
	return hex.EncodeToString(sum[:])[:RuleKeyBytes]
}

// renderRuleForKey is RuleKey's canonical rendering of one declaration. It is
// deliberately NOT renderPredicate: that renderer is operator-facing and
// preserves the author's declared ORDER (a reader wants to see what they
// wrote), while this one canonicalizes the order away, renders the `require`
// clamp, and — unlike the operator-facing renderer — is INJECTIVE. Keeping
// them separate is what lets RenderFired stay byte-identical while the key
// gains its stability contract.
//
// INJECTIVITY is the property the key rests on, and a plain delimiter-joined
// rendering does not have it: `paths: ["a,b"]` and `paths: ["a", "b"]` are two
// DIFFERENT declarations (the first matches one glob containing a comma, the
// second matches either of two globs) that both render `paths=a,b` under a
// bare comma join, and would therefore share a RuleKey — a decision index
// following one rule would silently follow the other. The same hazard exists
// one level up, where a value carrying a space and a ` labels=` prefix could
// forge a second field in a space-joined part list, and again on the scalar
// `require.approvals.member_of`, which is free-form operator text.
//
// So EVERY atom — each element of every criterion list, and each rendered
// field — is LENGTH-PREFIXED as `<byte-len>:<value>` before it is joined. A
// length prefix is self-delimiting: the decoder (which does not exist; only
// the hash consumes this) could recover the exact atom boundaries regardless
// of what bytes an atom contains, which is precisely the statement that two
// distinct declarations cannot render identically. The joining delimiters are
// retained purely for legibility when debugging a key.
func renderRuleForKey(e spec.Escalation) string {
	var parts []string
	field := func(name, value string) {
		parts = append(parts, encodeAtom(name+"="+value))
	}
	if len(e.Match.Paths) > 0 {
		field("paths", joinSorted(e.Match.Paths))
	}
	if len(e.Match.Labels) > 0 {
		field("labels", joinSorted(e.Match.Labels))
	}
	if len(e.Match.ChangeKinds) > 0 {
		field("change_kind", joinSorted(e.Match.ChangeKinds))
	}
	if len(e.Match.Triggers) > 0 {
		forms := make([]string, len(e.Match.Triggers))
		for i, t := range e.Match.Triggers {
			forms[i] = string(t)
		}
		field("trigger", joinSorted(forms))
	}
	if len(parts) == 0 {
		parts = append(parts, encodeAtom("no criterion"))
	}
	if a := e.Require.Approvals; a != nil {
		if a.Count != nil {
			field("require.approvals.count", fmt.Sprintf("%d", *a.Count))
		}
		if a.MemberOf != "" {
			field("require.approvals.member_of", a.MemberOf)
		}
		if a.MinPermission != "" {
			field("require.approvals.min_permission", a.MinPermission)
		}
	}
	if e.Require.MaxAutonomy != "" {
		field("require.max_autonomy", string(e.Require.MaxAutonomy))
	}
	// Appended LAST and only when non-empty, so a reviewers-less rule renders
	// exactly as it did before the dimension existed and keeps its key.
	if len(e.Require.Reviewers) > 0 {
		field("require.reviewers", joinSorted(e.Require.Reviewers))
	}
	return strings.Join(parts, " ")
}

// PersonaAttachment is one reviewer persona the fired escalations attach to a
// review, with the escalations that named it.
type PersonaAttachment struct {
	// Persona is the reviewer_personas map key.
	Persona string
	// Fired is every FIRED escalation whose require.reviewers names Persona,
	// in declaration order — the provenance an audit entry records (its
	// Index for the operator, RuleKey(Fired[i].Escalation) for a decision
	// index).
	Fired []Fired
}

// PersonaAttachments derives the reviewer personas an evaluation attaches
// (ADR-084 D2(c) / E55.9 / #3754): the de-duplicated UNION of every fired
// escalation's require.reviewers, sorted by persona name, each carrying the
// fired escalations naming it in declaration order.
//
// Only res.Fired is read. An escalation that did not fire contributes nothing
// — whatever it declares — because Evaluate never put it in Fired; that is the
// "cost only where attached" property the review loop relies on. A result
// with nothing fired, or whose fired escalations declare no reviewers, returns
// nil. Sorting by name (not by first appearance) makes the output a function
// of the fired SET, so reordering the declarations cannot reorder the
// attachments. The result shares no slice with res.
func PersonaAttachments(res Result) []PersonaAttachment {
	byName := make(map[string][]Fired)
	for _, f := range res.Fired {
		seen := make(map[string]bool, len(f.Escalation.Require.Reviewers))
		for _, name := range f.Escalation.Require.Reviewers {
			if seen[name] {
				continue
			}
			seen[name] = true
			byName[name] = append(byName[name], f)
		}
	}
	if len(byName) == 0 {
		return nil
	}
	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]PersonaAttachment, 0, len(names))
	for _, name := range names {
		out = append(out, PersonaAttachment{Persona: name, Fired: byName[name]})
	}
	return out
}

// joinSorted renders one unordered-OR criterion list canonically: a SORTED
// COPY of its LENGTH-PREFIXED elements, joined. The copy is load-bearing — an
// in-place sort would mutate the caller's parsed spec.Workflow. The
// length-prefixing is what makes the join injective: without it ["a,b"] and
// ["a","b"] render identically (see renderRuleForKey's header). Sorting the
// ENCODED elements rather than the raw ones is a different total order than
// sorting the raw ones, which is immaterial — the contract is only that a
// permutation of one rule's list does not move the key, and any total order
// computed from the elements alone satisfies it.
func joinSorted(vals []string) string {
	cp := make([]string, len(vals))
	for i, v := range vals {
		cp[i] = encodeAtom(v)
	}
	sort.Strings(cp)
	return strings.Join(cp, ",")
}

// encodeAtom renders one atom self-delimitingly as `<byte-len>:<value>`. It is
// the single primitive the canonical rule rendering's injectivity rests on: a
// reader that knows the length can recover the atom whatever bytes it holds,
// so no atom can impersonate a delimiter, a field boundary or a neighbouring
// atom.
func encodeAtom(s string) string {
	return fmt.Sprintf("%d:%s", len(s), s)
}
