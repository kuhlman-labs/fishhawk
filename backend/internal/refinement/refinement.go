// Package refinement is the intake-drafting half of ADR-052 option A (E34
// wave 0): it turns a natural-language brief into a structured epic/children
// draft, validates the draft's dependency graph, renders a byte-compatible
// filing preview, and persists the draft as a JSONB row keyed by a refinement
// session id.
//
// The load-bearing invariant is that NOTHING here files (ADR-052 decision 1):
// no provider write, no HTTP/MCP surface. The drafting agent proposes, the
// preview renderer shows what filing WOULD produce, and the repository stores
// the draft — but the actual provider filing is E34.3's job, gated behind the
// E34.2 preview. This package owns the model + agent + render + persistence
// and never reaches a provider.
//
// Dependency edges between children are expressed as 1-based sibling ordinals
// (children carry no issue numbers at draft time; the E34.3 filing executor
// remaps ordinals to real #numbers). Draft-time validation reuses
// campaign.Assemble's dangling/cycle rules over a synthetic 1..N numbering so
// the draft graph is checked with exactly the same semantics the campaign
// assembler enforces at filing time.
package refinement

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/kuhlman-labs/fishhawk/backend/internal/campaign"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// EpicDraft is the structured decomposition an agent drafts from a brief: the
// epic itself plus its children. It is the JSON contract the drafting agent
// emits (a closed field set — see decode.go), the value persisted as JSONB,
// and the input the preview renderer routes through the filing conventions.
type EpicDraft struct {
	Epic     EpicSpec     `json:"epic"`
	Children []ChildDraft `json:"children"`
}

// EpicSpec is the epic half of a draft. Scope and OutOfScope are prose; the
// renderer folds OutOfScope into the epic's Scope section (the epic skeleton
// carries no dedicated out-of-scope section).
type EpicSpec struct {
	Summary    string `json:"summary"`
	Scope      string `json:"scope"`
	OutOfScope string `json:"out_of_scope"`
}

// ChildDraft is one child of an epic draft. DependsOn entries are 1-based
// sibling ordinals into the parent EpicDraft.Children slice (ordinal 1 is the
// first child), NOT issue numbers — children have no issue numbers at draft
// time. The E34.3 filing executor remaps the ordinals to real #numbers once
// the children are filed.
type ChildDraft struct {
	Summary            string   `json:"summary"`
	Proposal           string   `json:"proposal"`
	DoneMeans          string   `json:"done_means"`
	AcceptanceCriteria []string `json:"acceptance_criteria"`
	Labels             []string `json:"labels"`
	DependsOn          []int    `json:"depends_on"`
}

// Validate enforces the draft's structural and dependency-graph invariants,
// returning the first violation as an error. Semantic shape (unknown fields,
// malformed JSON) is DecodeDraft's job; Validate operates on an
// already-decoded draft.
//
// Structural rules: non-empty epic summary and scope; at least one child; per
// child a non-empty summary and proposal and at least one acceptance
// criterion (the issue's 'criteria per child' done-means — E34.5's precheck
// adds the quality screen later).
//
// Dependency rules reuse campaign.Assemble over a synthetic 1..N ordinal
// numbering, so a draft edge is checked with exactly the semantics the
// campaign assembler applies at filing time: an out-of-range or self ordinal
// surfaces a wrapped campaign.ErrDanglingDependency naming the edge, a cyclic
// set surfaces campaign.ErrCycle. errors.Is against those sentinels works
// through the wrap.
func (d EpicDraft) Validate() error {
	if strings.TrimSpace(d.Epic.Summary) == "" {
		return fmt.Errorf("refinement: epic summary is required")
	}
	if strings.TrimSpace(d.Epic.Scope) == "" {
		return fmt.Errorf("refinement: epic scope is required")
	}
	if len(d.Children) == 0 {
		return fmt.Errorf("refinement: draft must have at least one child")
	}
	for i, c := range d.Children {
		ordinal := i + 1
		if strings.TrimSpace(c.Summary) == "" {
			return fmt.Errorf("refinement: child %d has an empty summary", ordinal)
		}
		if strings.TrimSpace(c.Proposal) == "" {
			return fmt.Errorf("refinement: child %d (%q) has an empty proposal", ordinal, c.Summary)
		}
		if len(c.AcceptanceCriteria) == 0 {
			return fmt.Errorf("refinement: child %d (%q) has no acceptance criteria", ordinal, c.Summary)
		}
	}
	return d.validateDependencies()
}

// Waves returns the draft's topological dispatch order as waves of 1-based
// child ordinals: Waves()[w] holds the ordinals of the children whose
// dependencies are all satisfied by earlier waves. It runs campaign.Assemble
// over the SAME synthetic 1..N ordinal numbering validateDependencies uses (so
// the preview wave DAG matches exactly what the campaign assembler produces at
// filing time), then maps Assembly.Waves' synthetic `issue:N` refs back to the
// 1-based ordinals. A dangling or cyclic edge surfaces the wrapped
// campaign.ErrDanglingDependency / campaign.ErrCycle (errors.Is works through
// the wrap), mirroring Validate — a preview of an invalid graph fails closed
// rather than rendering a partial DAG.
func (d EpicDraft) Waves() ([][]int, error) {
	res := &workmgmt.EpicChildrenResult{
		Children: make([]workmgmt.EpicChild, len(d.Children)),
	}
	for i := range d.Children {
		res.Children[i] = workmgmt.EpicChild{Number: i + 1}
	}
	for i, c := range d.Children {
		for _, dep := range c.DependsOn {
			res.Edges = append(res.Edges, workmgmt.DependsEdge{From: i + 1, To: dep})
		}
	}
	asm, err := campaign.Assemble("draft", res)
	if err != nil {
		return nil, fmt.Errorf("refinement: draft wave assembly failed: %w", err)
	}
	waves := make([][]int, len(asm.Waves))
	for w, refs := range asm.Waves {
		ordinals := make([]int, 0, len(refs))
		for _, ref := range refs {
			ord, perr := ordinalFromRef(ref)
			if perr != nil {
				return nil, perr
			}
			ordinals = append(ordinals, ord)
		}
		waves[w] = ordinals
	}
	return waves, nil
}

// ordinalFromRef parses a synthetic `issue:N` campaign ref back into the
// 1-based child ordinal N. The refs come from campaign.Assemble over our own
// synthetic numbering, so a malformed ref is an internal invariant break, not
// user input — it surfaces as an error rather than a silent zero.
func ordinalFromRef(ref string) (int, error) {
	const prefix = "issue:"
	if !strings.HasPrefix(ref, prefix) {
		return 0, fmt.Errorf("refinement: unexpected wave ref %q (want issue:N)", ref)
	}
	n, err := strconv.Atoi(ref[len(prefix):])
	if err != nil {
		return 0, fmt.Errorf("refinement: unparseable wave ref %q: %w", ref, err)
	}
	return n, nil
}

// validateDependencies maps the children onto a synthetic 1..N EpicChild set
// (ordinal i+1 for children[i]) and their DependsOn ordinals onto
// workmgmt.DependsEdge{From, To}, then calls campaign.Assemble to run the
// dangling/cycle rules. An out-of-range ordinal (0, negative, or > N) is not a
// fellow child, so Assemble surfaces a wrapped campaign.ErrDanglingDependency
// naming the edge; a cycle (including a self-edge, which is a length-1 cycle)
// surfaces campaign.ErrCycle. The returned Assembly is discarded — only the
// validation outcome matters here.
func (d EpicDraft) validateDependencies() error {
	res := &workmgmt.EpicChildrenResult{
		Children: make([]workmgmt.EpicChild, len(d.Children)),
	}
	for i := range d.Children {
		res.Children[i] = workmgmt.EpicChild{Number: i + 1}
	}
	for i, c := range d.Children {
		for _, dep := range c.DependsOn {
			res.Edges = append(res.Edges, workmgmt.DependsEdge{From: i + 1, To: dep})
		}
	}
	if _, err := campaign.Assemble("draft", res); err != nil {
		return fmt.Errorf("refinement: draft dependency graph invalid: %w", err)
	}
	return nil
}
