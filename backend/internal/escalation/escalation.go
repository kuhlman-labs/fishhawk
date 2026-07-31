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
