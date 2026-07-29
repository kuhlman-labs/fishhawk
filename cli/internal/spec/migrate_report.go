package spec

// The approval-eligibility report (E52.8 / #2220).
//
// Migrating a governance file changes WHO CAN APPROVE WHAT. That is the
// only question an operator actually has to answer before committing the
// rewrite, so the report — not the migrated bytes — is the product of
// `fishhawk migrate-spec`, and it is emitted on every path including the
// refusal and already-v2 no-op ones.
//
// THE RULE THAT SHAPES THIS FILE: the eligible-principal set is split
// into two EXPLICITLY LABELLED parts, and a predicate this document
// cannot resolve is rendered SYMBOLICALLY as itself rather than dropped.
// `min_permission: write` and `member_of: acme/reviewers` are resolved by
// the forge at run time; a report that silently shows an empty set for a
// gate governed only by member_of is worse than one that says the set is
// not resolvable here, because an empty set reads as "nobody can approve
// this" and invites exactly the wrong edit.

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// The three `change:` verdicts a gate block closes with.
const (
	changeNone = "none — the gate already declares an `approvals` block; it is carried through verbatim."

	changeRefused = "not computed — the translation refused, so no eligibility change can be stated."

	// changeWidenedNoExclusion is the standing note for EVERY gate
	// migrated out of the legacy grammar. The legacy `approvers` form has
	// no source for an exclusion, so the migrated gate emits no `not:`
	// and the change's own author can satisfy a gate the shipped presets'
	// `not: [author, agent]` default would bar them from. Adding the
	// exclusion silently would be exactly the guess this codemod exists
	// not to make; reporting it is the alternative.
	changeWidenedNoExclusion = "WIDENED — the migrated gate declares no `not:` exclusion, because the legacy `approvers` form " +
		"carries no source for one. Relative to the shipped presets' `not: [author, agent]` default this is a widening: " +
		"the change's own author (and an agent identity) can now satisfy this gate. Add `not: [author, agent]` by hand if that is not intended."
)

// UnresolvedPredicate is one approval predicate this document cannot
// enumerate — the forge resolves it at run time.
type UnresolvedPredicate struct {
	// Kind is the predicate key, e.g. "min_permission" or "member_of".
	Kind string
	// Rendered is the predicate written as ITSELF, e.g. "min_permission
	// >= write".
	Rendered string
}

// GateEligibility is one approval gate's before/after eligibility block.
type GateEligibility struct {
	// Path is the gate's YAML path, e.g.
	// /workflows/feature_change/stages/0/gates/0.
	Path string
	// StageID is the owning stage's id, so the gate is nameable in prose.
	StageID string
	// Before and After render the source and migrated predicates.
	Before string
	After  string
	// Enumerable are the principals resolvable FROM THIS DOCUMENT.
	Enumerable []string
	// Unresolved are the predicates only the forge can resolve. Rendered
	// symbolically, never as an empty or fabricated set.
	Unresolved []UnresolvedPredicate
	// Refused is true when this gate's translation refused.
	Refused bool
	// Change is the `change:` line — one of the three verdicts above.
	Change string
}

// EligibilityReport is the whole document's report.
type EligibilityReport struct {
	Gates []GateEligibility
	// BudgetStages names the budget-bearing stages (stage id, or a path
	// when unnamed) for which no `limit_usd` ceiling was derivable. There
	// is no token-to-USD rate anywhere in this repo, so the codemod never
	// fabricates one; the report notes per such stage that a ceiling must
	// be added by hand.
	BudgetStages []string
}

// forgeResolvedMarker labels every symbolically-rendered predicate, so a
// reader never mistakes one for an enumerated principal.
const forgeResolvedMarker = "(forge-resolved at run time)"

// Render writes the report as plain text for stdout.
func (r *EligibilityReport) Render() string {
	var b strings.Builder
	b.WriteString("Approval-eligibility report\n")
	b.WriteString("===========================\n")
	if r == nil || len(r.Gates) == 0 {
		b.WriteString("\nThis document declares no approval gates.\n")
		return b.String()
	}
	for _, g := range r.Gates {
		b.WriteString("\n")
		fmt.Fprintf(&b, "gate %s (stage: %s)\n", g.Path, displayStageID(g.StageID))
		fmt.Fprintf(&b, "  before: %s\n", g.Before)
		fmt.Fprintf(&b, "  after:  %s\n", g.After)
		b.WriteString("  eligible principals\n")
		b.WriteString("    enumerable from this document:\n")
		if len(g.Enumerable) == 0 {
			b.WriteString("      (none)\n")
		}
		for _, m := range g.Enumerable {
			fmt.Fprintf(&b, "      - %s\n", m)
		}
		b.WriteString("    not resolvable from this document:\n")
		if len(g.Unresolved) == 0 {
			b.WriteString("      (none)\n")
		}
		for _, u := range g.Unresolved {
			fmt.Fprintf(&b, "      - %s   %s\n", u.Rendered, forgeResolvedMarker)
		}
		if len(g.Enumerable) == 0 && len(g.Unresolved) == 0 && !g.Refused {
			b.WriteString("    (no approver constraint beyond count/not)\n")
		}
		fmt.Fprintf(&b, "  change: %s\n", g.Change)
	}
	r.renderBudgetNote(&b)
	b.WriteString("\nServer-side validation remains the authority for cross-stage wiring: this report and the\n")
	b.WriteString("codemod's schema gate cover the spec document, not `needs` / `from_stage` referent resolution.\n")
	return b.String()
}

// renderBudgetNote emits the per-stage limit_usd note the migration is
// required to produce: for every budget-bearing stage it states that no USD
// ceiling was derivable (no token-to-USD rate exists in this repo) and that
// one should be added by hand. Nothing is fabricated — this is advisory
// prose, not a guessed number.
func (r *EligibilityReport) renderBudgetNote(b *strings.Builder) {
	if r == nil || len(r.BudgetStages) == 0 {
		return
	}
	b.WriteString("\nbudget: no `limit_usd` ceiling was derivable — there is no token-to-USD rate in this\n")
	b.WriteString("repository, so none was fabricated. Add `budget.limit_usd` by hand to these\n")
	b.WriteString("budget-bearing stages if you want a spend ceiling:\n")
	for _, s := range r.BudgetStages {
		fmt.Fprintf(b, "  - %s\n", s)
	}
}

func displayStageID(id string) string {
	if id == "" {
		return "(unnamed)"
	}
	return id
}

// renderPredicate renders an approvals predicate as it appears (or will
// appear) in the document.
func renderPredicate(p approvalPredicate) string {
	parts := []string{fmt.Sprintf("count: %d", p.Count)}
	if len(p.Not) > 0 {
		parts = append(parts, fmt.Sprintf("not: [%s]", strings.Join(p.Not, ", ")))
	}
	if p.MinPermission != "" {
		parts = append(parts, "min_permission: "+p.MinPermission)
	}
	if p.MemberOf != "" {
		parts = append(parts, "member_of: "+p.MemberOf)
	}
	if len(p.Members) > 0 {
		parts = append(parts, fmt.Sprintf("members: [%s]", strings.Join(p.Members, ", ")))
	}
	return "approvals: {" + strings.Join(parts, ", ") + "}"
}

// unresolvedOf renders the predicates only the forge can resolve. Both
// are rendered as THEMSELVES; neither is ever expanded to a guessed
// member list nor silently dropped.
func unresolvedOf(p approvalPredicate) []UnresolvedPredicate {
	var out []UnresolvedPredicate
	if p.MinPermission != "" {
		out = append(out, UnresolvedPredicate{
			Kind:     "min_permission",
			Rendered: "min_permission >= " + p.MinPermission,
		})
	}
	if p.MemberOf != "" {
		out = append(out, UnresolvedPredicate{
			Kind:     "member_of",
			Rendered: "member_of = " + p.MemberOf,
		})
	}
	return out
}

// reportForV2 builds a best-effort report from a document that is NOT
// being translated — the already-v2 no-op path, and the R0 / R9 refusal
// paths, where the operator still wants to see what the gates say. A gate
// carrying `approvals` is rendered fully; a gate still carrying the
// legacy `approvers` form is rendered symbolically as its source
// predicate, since no translation was computed for it.
func reportForV2(root *yaml.Node) *EligibilityReport {
	report := &EligibilityReport{}
	// Resolve the roles map best-effort so a gate still carrying the legacy
	// `approvers` form (an R0 / R9 refusal path routes here) names the
	// principals resolvable FROM THIS DOCUMENT on the source side, not an
	// empty set. This is a READ of a document no translation ran on, so it
	// never refuses — a team reference, which has no per-subject encoding,
	// is simply omitted from the enumerable set.
	roles := collectRolesForReport(root)
	workflows := mappingNode(mapValue(root, "workflows"))
	for _, pair := range mapPairs(workflows) {
		wf := mappingNode(pair[1])
		if wf == nil {
			continue
		}
		for i, stRaw := range valueSeq(wf, "stages") {
			st := mappingNode(stRaw)
			if st == nil {
				continue
			}
			stageID := ""
			if id := mapValue(st, "id"); id != nil {
				stageID = id.Value
			}
			for j, gRaw := range valueSeq(st, "gates") {
				gate := mappingNode(gRaw)
				if gate == nil {
					continue
				}
				typ := mapValue(gate, "type")
				if typ == nil || typ.Value != "approval" {
					continue
				}
				path := fmt.Sprintf("/workflows/%s/stages/%d/gates/%d", pair[0].Value, i, j)
				report.Gates = append(report.Gates, untranslatedGate(gate, path, stageID, roles))
			}
		}
	}
	return report
}

// untranslatedGate renders one gate of a document no translation ran on.
func untranslatedGate(gate *yaml.Node, path, stageID string, roles map[string][]string) GateEligibility {
	if existing := mappingNode(mapValue(gate, "approvals")); existing != nil {
		p := readApprovals(existing)
		return GateEligibility{
			Path:       path,
			StageID:    stageID,
			Before:     renderPredicate(p),
			After:      renderPredicate(p),
			Enumerable: p.Members,
			Unresolved: unresolvedOf(p),
			Change:     changeNone,
		}
	}
	approvers := mappingNode(mapValue(gate, "approvers"))
	form, names := approversForm(approvers)
	return GateEligibility{
		Path:    path,
		StageID: stageID,
		Before:  fmt.Sprintf("approvers: {%s: [%s]}", form, strings.Join(names, ", ")),
		After:   "(not translated — the migration did not run on this document)",
		// Even on a path where no translation ran, the mandatory report
		// names who could approve on the source side when it is resolvable
		// from the roles map.
		Enumerable: sourceEnumerable(approvers, roles),
		Refused:    true,
		Change:     changeRefused,
	}
}

// collectRolesForReport resolves the top-level roles map to @-stripped
// per-subject members for reportForV2's best-effort source-side
// enumeration. Unlike (*analysis).collectRoles it never refuses — it is a
// read of a document no translation ran on — so a team reference (which
// has no faithful per-subject encoding) is omitted rather than raising R3.
func collectRolesForReport(root *yaml.Node) map[string][]string {
	out := map[string][]string{}
	roles := mappingNode(mapValue(root, "roles"))
	for _, pair := range mapPairs(roles) {
		name, body := pair[0].Value, mappingNode(pair[1])
		members := mapValue(body, "members")
		if members == nil || members.Kind != yaml.SequenceNode {
			continue
		}
		resolved := make([]string, 0, len(members.Content))
		for _, m := range members.Content {
			ref := strings.TrimPrefix(m.Value, "@")
			if strings.Contains(ref, "/") {
				continue // team ref: not an enumerable per-subject principal
			}
			resolved = append(resolved, ref)
		}
		out[name] = resolved
	}
	return out
}
