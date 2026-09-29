package mcpserver

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kuhlman-labs/fishhawk/backend/internal/delegationview"
)

// fishhawk_delegation (E76.1 / #3747) is the MCP half of the per-workflow
// delegation read: for each workflow a repository's spec declares, the resolved
// autonomy matrix with per-class provenance, the pageable-event set, and each
// escalation's match criteria plus the matrix its ceiling would produce — read
// from the spec at a ref, WITHOUT a run.
//
// It is READ-ONLY and mints no audit entry. The projection is the DECLARATION,
// not an evaluation: `matrix` is the workflow-level matrix, and `ceiling_matrix`
// is what an escalation's ceiling WOULD produce, not one in force. Whether an
// escalation fires depends on an approved plan's scope.files, which does not
// exist before a run.
//
// The response is bounded per ADR-077 through the SHARED machinery
// (elisionLedger, attachAndMeasureOut, marshalledLen, pointerREST) in a
// three-tier ladder — see boundDelegationOutput.

// DelegationInput is the tool's input schema.
type DelegationInput struct {
	Repo     string `json:"repo" jsonschema:"owner/name of the repository whose workflow spec to read"`
	Ref      string `json:"ref,omitempty" jsonschema:"the git ref to read .fishhawk/workflows.yaml at under source=ref; omit for the repository default branch head"`
	Source   string `json:"source,omitempty" jsonschema:"where to read the spec from: ref (default, fetched through the forge at ref) or run_cache (the spec cached on this repository's newest run). Always echoed on the response and never switched silently"`
	Workflow string `json:"workflow,omitempty" jsonschema:"narrow the response to one workflow id"`
}

// DelegationOutput is the tool's output schema: the projected view plus the
// bounded-surface Elisions block.
type DelegationOutput struct {
	delegationview.View
	Elisions *Elisions `json:"elisions,omitempty"`
}

// delegationFloorListCap bounds each escalation match list the B1 tier keeps.
//
// It is not a readability choice: an escalation may declare an arbitrary number
// of globs, so keeping them uncapped would make B1's size a function of the
// SPEC rather than of the answer and the ladder could not narrow predictably.
const delegationFloorListCap = 8

// delegationSurface is the unbounded retrieval surface every delegation elision
// names: the same read over REST, which the MCP byte budget does not bound.
func delegationSurface() unboundedPointer {
	return pointerREST("/v0/repos/{owner}/{name}/delegation")
}

// registerDelegation wires the fishhawk_delegation tool.
func registerDelegation(srv *mcp.Server, resolver *runResolver) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "fishhawk_delegation",
		Description: strings.TrimSpace(`
WHEN: before starting work in a repository, or while reviewing its governance,
and you want to know what the crew may decide on its own — per workflow, from
the spec at a ref, with no run required.

ELIGIBILITY: any authenticated caller. Read-only — it writes nothing, records
no audit entry, and grants NO authority. Reading that a class is delegated is
not permission to act: the run's own delegation block (GET /v0/runs/{id}) is
what evaluates a class's condition against live state.

Per workflow it reports:
  - autonomy: the declared tier shorthand (low / medium / high), empty when the
    workflow declares only an actions block, and empty with an EMPTY matrix when
    it declares no autonomy at all — the fail-closed reading, nothing delegated.
  - matrix: every action class with its mode (gated / auto / report) and the
    PROVENANCE of that mode — tier, explicit (an actions entry named the class),
    default (nothing named it, so it fell to the fail-closed gated), or
    escalation.
  - must_page_human: the events that page a human regardless of any class's
    mode — the same list a run's delegation block surfaces.
  - escalations[]: each declared escalation's match criteria (paths, labels,
    trigger), the approvals it raises, its max_autonomy ceiling, and
    ceiling_matrix — the matrix that ceiling WOULD produce.

TWO CONTRACTS NOT TO MISREAD. matrix is the WORKFLOW-level matrix: a gate
declaring its own autonomy block overrides it wholesale AT THAT GATE, and this
read shows the workflow-level one. ceiling_matrix is declarative — whether an
escalation fires depends on an approved plan's scope.files, which does not exist
before a run, so this read never asks; the workflow's own matrix is never
clamped here.

source is ALWAYS echoed and never switched silently: source=ref (the default)
reads through the forge at ref, and an unconfigured forge is a named refusal
pointing at source=run_cache rather than a fallback.

content_hash covers the projected delegation content ONLY — never the repo, ref,
workflow_sha or source — so identical delegation read at two refs hashes
identically, and a later confirmation can bind to exactly what was shown. Bind a
single-workflow confirmation to that workflow's OWN content_hash.
`),
	}, resolver.delegation)
}

// delegation is the tool handler.
func (r *runResolver) delegation(ctx context.Context, req *mcp.CallToolRequest, in DelegationInput) (*mcp.CallToolResult, DelegationOutput, error) {
	// DelegationInput and RepoDelegationParams are field-identical by
	// construction: the tool input IS the query, so a conversion keeps them from
	// drifting (the fishhawk_precedent precedent).
	res, err := r.api.GetRepoDelegation(ctx, RepoDelegationParams(in))
	if err != nil {
		// A backend refusal surfaces as a TOOL ERROR, never an empty view: an
		// empty workflows array reads as "this repository delegates nothing",
		// which is the opposite of "we could not tell you".
		return nil, DelegationOutput{}, fmt.Errorf("get repo delegation: %w", err)
	}
	bounded, berr := boundDelegationOutput(DelegationOutput{View: *res}, r.responseBudget(req))
	if berr != nil {
		return nil, DelegationOutput{}, berr
	}
	return nil, bounded, nil
}

// boundDelegationOutput is the ADR-077 ladder for fishhawk_delegation. Every
// tier MARKS its truncation, so the tool never returns a silently partial
// delegation posture.
//
//	B1    drop every escalation's ceiling_matrix and cap every escalation match
//	      list to a fixed head. ceiling_matrix is a DERIVED projection — it is
//	      stored nowhere, so no pointer retrieves it and only the REST re-query
//	      recomputes it; it is recorded as a COMPUTED elision for that reason.
//	B2    drop workflows from the TAIL of the id-ordered list under a stored
//	      elision naming the REST surface.
//	FLOOR keep, per workflow, only id + autonomy + content_hash, with matrix and
//	      escalations elided under ONE aggregate entry. That body is
//	      CONSTANT-SIZE per workflow, which is what makes the ladder converge —
//	      and the floor itself is measured and shrunk (see delegationFloor).
func boundDelegationOutput(out DelegationOutput, budget responseBudget) (DelegationOutput, error) {
	set := func(o *DelegationOutput, e *Elisions) { o.Elisions = e }

	n, err := marshalledLen(out)
	if err != nil {
		return out, err
	}
	if n <= budget.bytes {
		return out, nil
	}

	led := &elisionLedger{budget: budget.bytes, source: budget.source,
		note: "response reduced to fit the tool-result byte budget"}

	// B1: the derived ceiling matrices and the oversized match lists.
	led.tier = "B1"
	ceilings, globs := 0, 0
	for wi := range out.Workflows {
		for ei := range out.Workflows[wi].Escalations {
			e := &out.Workflows[wi].Escalations[ei]
			if len(e.CeilingMatrix) > 0 {
				e.CeilingMatrix = nil
				ceilings++
			}
			e.Match.Paths, globs = capDelegationList(e.Match.Paths, globs)
			e.Match.Labels, globs = capDelegationList(e.Match.Labels, globs)
			e.Match.ChangeKind, globs = capDelegationList(e.Match.ChangeKind, globs)
			e.Match.Trigger, globs = capDelegationList(e.Match.Trigger, globs)
		}
	}
	if ceilings > 0 {
		led.add(newComputedElision("workflows[].escalations[].ceiling_matrix", fmt.Sprintf(
			"the ceiling matrix was dropped for %d escalations; each still reports its max_autonomy, so WHICH ceiling applies is retained and only its per-class effect is omitted. The matrix is DERIVED by clamping the workflow matrix at query time and is stored nowhere, so no pointer can retrieve it — re-running the read recomputes it",
			ceilings), ceilings))
	}
	if globs > 0 {
		led.add(newStoredElision("workflows[].escalations[].match", fmt.Sprintf(
			"%d match criteria beyond the first %d of each list were dropped to fit the byte budget; the surface below returns every criterion un-elided",
			globs, delegationFloorListCap), delegationSurface().retrievalPointer, globs))
	}
	fits, err := attachAndMeasureOut(&out, led, budget.bytes, set)
	if err != nil {
		return out, err
	}
	if fits {
		return out, nil
	}

	// B2: drop workflows from the tail of the id-ordered list.
	base := append([]elidedField(nil), led.entries...)
	total := len(out.Workflows)
	for len(out.Workflows) > 1 {
		keep := len(out.Workflows) / 2
		out.Workflows = out.Workflows[:keep]
		led.tier = "B2"
		led.entries = append(append([]elidedField(nil), base...),
			newStoredElision("workflows", fmt.Sprintf(
				"%d trailing workflows were dropped to fit the byte budget. The list is id-ASCENDING, so the drop is positional rather than by relevance — narrow the read with the workflow parameter, or use the surface below, to see a specific one",
				total-keep), delegationSurface().retrievalPointer, total-keep))
		fits, ferr := attachAndMeasureOut(&out, led, budget.bytes, set)
		if ferr != nil {
			return out, ferr
		}
		if fits {
			return out, nil
		}
	}

	return delegationFloor(out, budget, total)
}

// capDelegationList truncates one match list to delegationFloorListCap,
// accumulating how many entries it dropped so the elision can report a total.
func capDelegationList(in []string, dropped int) ([]string, int) {
	if len(in) <= delegationFloorListCap {
		return in, dropped
	}
	return in[:delegationFloorListCap], dropped + len(in) - delegationFloorListCap
}

// delegationFloor keeps, per workflow, ONLY id + autonomy + content_hash.
//
// THE FIT IS MEASURED, NOT INFERRED FROM THE PER-WORKFLOW SIZE. Three capped
// scalars per workflow is a constant-size ENTRY, but the number of ENTRIES is a
// function of the spec, so a spec declaring hundreds of workflows would put even
// the stripped list above the budget. So the floor is BUILT, MEASURED, and
// rebuilt with the retained workflow count HALVED until it fits, which converges
// by construction: at zero retained workflows the floor is the view's scalars
// plus the aggregate elision prose. The retained total and the truncation marks
// survive every step, and the prose names the count ACTUALLY retained rather
// than the starting one.
func delegationFloor(out DelegationOutput, budget responseBudget, total int) (DelegationOutput, error) {
	bound := max(budget.bytes, mcpConvergenceFloorBytes)
	keep := len(out.Workflows)
	for {
		floor, err := buildDelegationFloor(out, budget, total, keep)
		if err != nil {
			return out, err
		}
		n, err := marshalledLen(floor)
		if err != nil {
			return out, err
		}
		if n <= bound || keep == 0 {
			return floor, nil
		}
		keep /= 2
	}
}

// buildDelegationFloor renders one candidate floor retaining `keep` workflow
// summaries. Split out of delegationFloor so the measured shrink re-renders the
// elision prose with the count it actually applied.
func buildDelegationFloor(out DelegationOutput, budget responseBudget, total, keep int) (DelegationOutput, error) {
	summaries := make([]delegationview.WorkflowDelegation, 0, keep)
	for i := 0; i < keep && i < len(out.Workflows); i++ {
		w := out.Workflows[i]
		summaries = append(summaries, delegationview.WorkflowDelegation{
			ID:          capJSONString(w.ID, floorFieldCap),
			Autonomy:    capJSONString(w.Autonomy, floorFieldCap),
			Matrix:      []delegationview.Action{},
			ContentHash: capJSONString(w.ContentHash, floorFieldCap),
		})
	}
	floor := DelegationOutput{View: delegationview.View{
		Repo:        capJSONString(out.Repo, floorFieldCap),
		Source:      capJSONString(out.Source, floorFieldCap),
		Ref:         capJSONString(out.Ref, floorFieldCap),
		WorkflowSHA: capJSONString(out.WorkflowSHA, floorFieldCap),
		SpecVersion: capJSONString(out.SpecVersion, floorFieldCap),
		SchemaMajor: out.SchemaMajor,
		ContentHash: capJSONString(out.ContentHash, floorFieldCap),
		Workflows:   summaries,
	}}
	led := &elisionLedger{
		budget: budget.bytes,
		source: budget.source,
		tier:   floorTierName,
		note:   "reduced to the constant-size floor: per workflow only its id, its declared autonomy tier and its content hash. The entry below is an AGGREGATE — the floor tier's explicit exception to per-field itemisation",
	}
	led.add(aggregateStoredElision("*", fmt.Sprintf(
		"every resolved matrix, page list, model policy and escalation was omitted, and %d of %d workflows were dropped entirely. Each RETAINED workflow keeps its id, its autonomy tier and its content_hash — so a confirmation can still be bound to exactly what was projected, and the tier still says roughly how much is delegated — while the per-class modes, their provenance and the escalation ceilings are not shown. The view-level content_hash is retained and still covers the UNFILTERED projected set. The surface below returns the full delegation read",
		total-len(summaries), total),
		[]string{delegationSurface().String()}))
	w, err := led.wire()
	if err != nil {
		return out, err
	}
	floor.Elisions = w
	return floor, nil
}
