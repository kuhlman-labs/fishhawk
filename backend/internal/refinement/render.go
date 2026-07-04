package refinement

import (
	"strconv"
	"strings"

	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// Draft-time render sentinels. A preview render happens BEFORE anything is
// filed, so three facts that only exist at filing time are supplied as
// sentinels the E34.3 filing executor replaces with real values:
//
//   - the feature title's {epic} placeholder (the parent epic number),
//   - the required feature epic_link parent-epic relation, and
//   - the epic type's fail-closed sequential numbering seed (workmgmt.Apply
//     rejects an empty ExistingNumbers for a numbered type, #1265).
//
// RenderOptions carries overrides for all three; the zero value renders a
// preview with these sentinels.
const (
	sentinelEpicNumber = "X"  // {epic} in a child title -> "[EX.n] ..."
	sentinelParentEpic = "#0" // the feature's required parent-epic relation
)

// sentinelEpicExistingNumbers seeds the epic's sequential numbering so a
// preview render allocates epic number 1 (max(0)+1). The filing executor
// re-renders with the real discovered numbers.
var sentinelEpicExistingNumbers = []int{0}

// RenderOptions carries the draft-time filing sentinels (see the package
// sentinel constants) so a preview render is byte-identical to the real filing
// given the same inputs, and E34.3 can re-render with real values. The zero
// value is the pure-preview default.
type RenderOptions struct {
	// EpicNumber overrides the {epic} title placeholder for children; empty
	// uses sentinelEpicNumber.
	EpicNumber string
	// ParentEpicRef overrides the child's parent-epic relation; empty uses
	// sentinelParentEpic.
	ParentEpicRef string
	// EpicExistingNumbers overrides the epic numbering seed; empty uses
	// sentinelEpicExistingNumbers.
	EpicExistingNumbers []int
}

func (o RenderOptions) epicNumber() string {
	if o.EpicNumber != "" {
		return o.EpicNumber
	}
	return sentinelEpicNumber
}

func (o RenderOptions) parentEpicRef() string {
	if o.ParentEpicRef != "" {
		return o.ParentEpicRef
	}
	return sentinelParentEpic
}

func (o RenderOptions) epicExistingNumbers() []int {
	if len(o.EpicExistingNumbers) > 0 {
		return o.EpicExistingNumbers
	}
	return sentinelEpicExistingNumbers
}

// RenderDraft renders the whole draft into a preview slice for E34.2's preview
// gate: the epic first, then each child in ordinal order. It routes every item
// through the SAME workmgmt.Apply conventions pipeline the filing path uses,
// so a preview body is byte-compatible with a hand-filed item by construction.
// The first Apply error (unknown type, missing field, unresolved placeholder)
// stops and is returned.
func RenderDraft(draft EpicDraft, opts RenderOptions, conv workmgmt.Conventions) ([]workmgmt.WorkItem, error) {
	items := make([]workmgmt.WorkItem, 0, len(draft.Children)+1)

	epic, err := RenderEpic(draft.Epic, opts, conv)
	if err != nil {
		return nil, err
	}
	items = append(items, epic)

	for i, child := range draft.Children {
		item, err := RenderChild(child, i+1, opts, conv)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, nil
}

// RenderChild renders one child (at its 1-based ordinal) into a
// conventions-complete WorkItem, routed through workmgmt.Apply as a feature
// (the v0 default — the drafting schema exposes no per-child type). The body is
// Apply's assembled skeleton (Summary/Proposal/Done-means/Acceptance criteria)
// plus, ONLY when the child has depends_on edges, the depends_on marker line in
// the provider's exact `Depends on: #N` format appended after Apply — using
// DRAFT ordinals as refs (placeholders the filing executor replaces with real
// #numbers). A child with no edges renders a body byte-identical to a direct
// Apply call (the never-fold-the-marker byte-compat contract).
func RenderChild(child ChildDraft, ordinal int, opts RenderOptions, conv workmgmt.Conventions) (workmgmt.WorkItem, error) {
	// Preview: the sentinel epic number + parent-epic ref, and NO real
	// depends_on refs on the request (the ordinal-placeholder marker is
	// appended AFTER Apply, unchanged, below).
	req := FilingRequestForChild(child, ordinal, opts.epicNumber(), opts.parentEpicRef(), nil)
	item, _, err := workmgmt.Apply(req, conv)
	if err != nil {
		return workmgmt.WorkItem{}, err
	}
	item.Body = appendDependsOnMarker(item.Body, child.DependsOn)
	return item, nil
}

// FilingRequestForChild builds the conventions FilingRequest for one child at
// its 1-based ordinal. It is the single seam shared by the E34.2 preview
// (RenderChild, which passes the non-empty sentinel epicNumber "X" and nil
// dependsOnRefs) and the E34.3 filing executor (which passes epicNumber "" and
// the child's depends_on refs resolved to real `#N` numbers).
//
// The {epic} title var is set ONLY when epicNumber is non-empty. When epicNumber
// == "" the "epic" key is OMITTED entirely, which is the contract that lets the
// server-side deriveEpicTitleVar (which short-circuits when title_vars already
// carries "epic") derive the epic's DISCOVERED ordinal from the parent epic's
// leading [E<n>] title at File time (#1644). The executor passes "" so the
// ordinal is DERIVED rather than injected as the epic's ISSUE number; the E34.2
// preview (RenderChild) passes the non-empty sentinel "X" and so still renders
// [EX.n] directly through workmgmt.Apply (preview never runs deriveEpicTitleVar).
//
// dependsOnRefs sets Relations.DependsOn so the github provider's
// ensureDependsOnMarker renders the marker in its exact `Depends on: #N` format
// at File time. The preview passes nil (Relations.DependsOn empty) and appends
// the draft-ordinal placeholder marker itself after Apply, so a preview body is
// byte-identical whether or not this builder is used.
func FilingRequestForChild(child ChildDraft, ordinal int, epicNumber, parentEpicRef string, dependsOnRefs []string) workmgmt.FilingRequest {
	titleVars := map[string]string{"n": strconv.Itoa(ordinal)}
	if epicNumber != "" {
		titleVars["epic"] = epicNumber
	}
	return workmgmt.FilingRequest{
		Type:    "feature",
		Summary: child.Summary,
		Sections: map[string]string{
			"Proposal":            child.Proposal,
			"Done-means":          child.DoneMeans,
			"Acceptance criteria": bulletize(child.AcceptanceCriteria),
		},
		Labels:    child.Labels,
		TitleVars: titleVars,
		Relations: workmgmt.Relations{ParentEpic: parentEpicRef, DependsOn: dependsOnRefs},
	}
}

// RenderEpic renders the epic into a conventions-complete WorkItem via
// workmgmt.Apply. The epic skeleton is Summary/Scope/Notes and Apply fails
// closed on an unknown section key, so OutOfScope is FOLDED into the Scope
// section content (under an `### Out of scope` sub-heading) rather than filed
// as its own section. The epic is a numbered type, so ExistingNumbers is
// seeded (sentinel [0] -> epic number 1 by default) or Apply fails closed.
func RenderEpic(epic EpicSpec, opts RenderOptions, conv workmgmt.Conventions) (workmgmt.WorkItem, error) {
	req := FilingRequestForEpic(epic, opts.epicExistingNumbers())
	item, _, err := workmgmt.Apply(req, conv)
	if err != nil {
		return workmgmt.WorkItem{}, err
	}
	return item, nil
}

// FilingRequestForEpic builds the conventions FilingRequest for the epic. It is
// the single seam shared by the E34.2 preview (RenderEpic, which passes the
// sentinel existingNumbers seed [0]) and the E34.3 filing executor (which
// passes nil so applyAndFileWorkItem's server-side #1269 discovery allocates
// against the real tracker). OutOfScope is folded into the Scope section under
// an `### Out of scope` sub-heading because the epic skeleton carries no
// dedicated out-of-scope section and Apply fails closed on an unknown section
// key.
func FilingRequestForEpic(epic EpicSpec, existingNumbers []int) workmgmt.FilingRequest {
	scope := epic.Scope
	if strings.TrimSpace(epic.OutOfScope) != "" {
		scope = strings.TrimRight(epic.Scope, "\n") + "\n\n### Out of scope\n\n" + strings.TrimSpace(epic.OutOfScope)
	}
	return workmgmt.FilingRequest{
		Type:    "epic",
		Summary: epic.Summary,
		Sections: map[string]string{
			"Summary": epic.Summary,
			"Scope":   scope,
		},
		ExistingNumbers: existingNumbers,
	}
}

// bulletize renders acceptance criteria as a markdown bullet list — one
// `- <criterion>` per line — for the child's Acceptance criteria section. An
// empty slice yields the empty string (an unpopulated section).
func bulletize(items []string) string {
	var b strings.Builder
	for i, it := range items {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString("- ")
		b.WriteString(it)
	}
	return b.String()
}

// appendDependsOnMarker appends the depends_on marker line for the given draft
// ordinals to body, matching the github provider's renderDependsOnMarker /
// ensureDependsOnMarker format EXACTLY (`Depends on: #X, #Y`, separated from
// the body by a blank line). Returns body unchanged when there are no edges,
// so a no-dependency child's body is byte-identical to a bare Apply output.
// The marker is NEVER folded or dropped to force byte-equality — it is the
// campaign-assembly contract (ADR-052), so a child WITH edges renders exactly
// the Apply body PLUS the marker.
func appendDependsOnMarker(body string, ordinals []int) string {
	if len(ordinals) == 0 {
		return body
	}
	parts := make([]string, 0, len(ordinals))
	for _, o := range ordinals {
		parts = append(parts, "#"+strconv.Itoa(o))
	}
	marker := "Depends on: " + strings.Join(parts, ", ")
	if strings.TrimSpace(body) == "" {
		return marker
	}
	return strings.TrimRight(body, "\n") + "\n\n" + marker
}
