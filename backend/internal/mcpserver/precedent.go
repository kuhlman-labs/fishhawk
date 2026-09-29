package mcpserver

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kuhlman-labs/fishhawk/backend/internal/precedent"
)

// fishhawk_precedent (E75.3 / #3731, ADR-082 #3728 decision (b)) is the MCP
// half of the precedent query: prior decisions of the same class from the SAME
// repository, ranked with an explained score, each citing its chain entry.
//
// It is READ-ONLY and mints no audit entry. A precedent item is evidence, not
// authority: it reports how a decision like this one went before, and the
// caller still decides.
//
// The response is bounded per ADR-077 through the SHARED machinery
// (elisionLedger, attachAndMeasureOut, marshalledLen, capJSONString) in a
// three-tier ladder — see boundPrecedentOutput.

// PrecedentInput is the tool's input schema. Two modes: an explicit context, or
// a gate reference (run_id + optional stage_id) the backend derives repo, stage
// kind, touched paths and escalation keys from. DecisionClass is required in
// BOTH modes.
type PrecedentInput struct {
	DecisionClass   string   `json:"decision_class" jsonschema:"the decision class to find precedent for: plan_approval, concern_waive, concern_defer, concern_addressed_by_condition, scope_amendment, acceptance_arbitration, merge_verdict, clarification or grooming_disposition"`
	Repo            string   `json:"repo,omitempty" jsonschema:"owner/name of the repository to search; required unless run_id is supplied"`
	RunID           string   `json:"run_id,omitempty" jsonschema:"resolve the context from this run's plan artifact and escalations instead of naming it field by field"`
	StageID         string   `json:"stage_id,omitempty" jsonschema:"the stage within run_id whose fired escalation keys to rank against"`
	StageKind       string   `json:"stage_kind,omitempty" jsonschema:"narrow to decisions made on this stage kind (plan, implement, acceptance)"`
	Paths           []string `json:"paths,omitempty" jsonschema:"the paths this decision touches; matched prefix-aware against each prior decision's plan scope"`
	ConcernCategory string   `json:"concern_category,omitempty" jsonschema:"the reviewer concern category; normalized server-side to its canonical key"`
	Severity        string   `json:"severity,omitempty" jsonschema:"the concern severity"`
	EscalationKeys  []string `json:"escalation_keys,omitempty" jsonschema:"the escalation rule keys currently fired"`
	Limit           int      `json:"limit,omitempty" jsonschema:"maximum ranked items to return (default 20, max 50)"`
}

// PrecedentOutput is the tool's output schema. It mirrors the REST body and adds
// the bounded-surface Elisions block.
type PrecedentOutput struct {
	ResolvedContext PrecedentResolvedContext `json:"resolved_context"`
	Summary         precedent.Summary        `json:"summary"`
	Results         []precedent.Item         `json:"results"`
	Truncated       bool                     `json:"truncated"`
	Degraded        []PrecedentDegraded      `json:"degraded,omitempty"`
	Elisions        *Elisions                `json:"elisions,omitempty"`
}

// precedentFloorListCap bounds each resolved-context list the FLOOR tier keeps.
// The backend already caps its echo, but the floor's bound must not DEPEND on
// that: a resolved context echoing thousands of paths would put the
// summary-only floor above the byte budget and the ladder could not converge
// (#3731 binding condition 2). Capped here, the floor has a fixed maximum.
const precedentFloorListCap = 20

// precedentSurface is the unbounded retrieval surface every precedent elision
// names: the same query over REST, which the MCP byte budget does not bound.
func precedentSurface() unboundedPointer { return pointerREST("/v0/precedent") }

// registerPrecedent wires the fishhawk_precedent tool.
func registerPrecedent(srv *mcp.Server, resolver *runResolver) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "fishhawk_precedent",
		Description: strings.TrimSpace(`
WHEN: before making a gate decision (approve/reject a plan, waive or defer a
concern, decide a scope amendment, arbitrate acceptance) and you want to know
how decisions of that kind have gone before in THIS repository.

ELIGIBILITY: any authenticated caller. Read-only — it writes nothing, records
no audit entry, and grants NO authority. A precedent item is evidence that a
human or a delegation rule already weighed something similar; it is not
permission to decide the same way, and it never substitutes for the gate.

Two ways to name the context:
  - EXPLICIT: repo + decision_class (+ stage_kind, paths, concern_category,
    severity, escalation_keys).
  - GATE REFERENCE: run_id (+ stage_id) and decision_class. The backend derives
    repo, stage kind, touched paths and fired escalation keys from that run's
    own plan artifact and escalations, so the query ranks against the same keys
    the prior decisions were indexed with. decision_class is still REQUIRED: a
    gate does not say which CLASS of decision you are asking about.

Each result explains itself: matched_keys names which keys matched, score
reports what each signal contributed, and source_sequence / source_entry_hash
cite the audit-chain entry. reason_excerpt is read from that entry at query
time and capped; the full reason is on the chain.

summary carries the agreement of the returned set (count, human vs delegated,
the modal outcome and its share, and the doctrine versions spanned).
hard_filter_only marks an item that matched the repository and class and
nothing else — a weak precedent, reported rather than hidden.
`),
	}, resolver.precedent)
}

// precedent is the tool handler.
func (r *runResolver) precedent(ctx context.Context, req *mcp.CallToolRequest, in PrecedentInput) (*mcp.CallToolResult, PrecedentOutput, error) {
	// PrecedentInput and PrecedentParams are field-identical by construction:
	// the tool input IS the query, so a conversion keeps them from drifting.
	res, err := r.api.GetPrecedent(ctx, PrecedentParams(in))
	if err != nil {
		return nil, PrecedentOutput{}, fmt.Errorf("get precedent: %w", err)
	}
	out := PrecedentOutput{
		ResolvedContext: res.ResolvedContext,
		Summary:         res.Summary,
		Results:         res.Results,
		Truncated:       res.Truncated,
		Degraded:        res.Degraded,
	}
	bounded, berr := boundPrecedentOutput(out, r.responseBudget(req))
	if berr != nil {
		return nil, PrecedentOutput{}, berr
	}
	return nil, bounded, nil
}

// boundPrecedentOutput is the ADR-077 ladder for fishhawk_precedent. Every tier
// MARKS its truncation, so the tool never returns a silently short ranking.
//
//	B1    drop every reason excerpt and the per-signal score breakdown, keeping
//	      each item's matched keys, outcome, total score and cited sequence.
//	B2    drop items from the TAIL of the ranked list. The tail is the WEAKEST
//	      precedent, so the retained PREFIX is the answer's substance — the one
//	      direction in which dropping is not arbitrary.
//	FLOOR summary + a length-and-byte-capped resolved_context, results[]
//	      entirely elided under one aggregate entry. This is what makes the
//	      bound hold even when ONE result plus the summary exceeds the budget.
func boundPrecedentOutput(out PrecedentOutput, budget responseBudget) (PrecedentOutput, error) {
	set := func(o *PrecedentOutput, e *Elisions) { o.Elisions = e }

	n, err := marshalledLen(out)
	if err != nil {
		return out, err
	}
	if n <= budget.bytes {
		return out, nil
	}

	led := &elisionLedger{budget: budget.bytes, source: budget.source,
		note: "response reduced to fit the tool-result byte budget"}

	// B1.
	led.tier = "B1"
	excerpts := 0
	for i := range out.Results {
		if out.Results[i].ReasonExcerpt != "" {
			out.Results[i].ReasonExcerpt = ""
			excerpts++
		}
		out.Results[i].Score = precedent.ScoreComponents{Total: out.Results[i].Score.Total}
	}
	if excerpts > 0 {
		led.add(newStoredElision("results[].reason_excerpt", fmt.Sprintf(
			"%d reason excerpts were dropped to fit the byte budget; each item still cites its chain entry by reason_sequence and reason_key, and the surface below returns the excerpts un-elided",
			excerpts), precedentSurface().retrievalPointer, excerpts))
	}
	led.add(newComputedElision("results[].score", fmt.Sprintf(
		"the per-signal score breakdown (touched_paths, escalation_keys, concern_category, severity) was dropped for %d items; score.total is retained. The components are DERIVED at query time from the ranking context and are stored nowhere, so no pointer can retrieve them — re-running the query recomputes them",
		len(out.Results)), len(out.Results)))
	fits, err := attachAndMeasureOut(&out, led, budget.bytes, set)
	if err != nil {
		return out, err
	}
	if fits {
		return out, nil
	}

	// B2.
	base := append([]elidedField(nil), led.entries...)
	total := len(out.Results)
	for len(out.Results) > 1 {
		keep := len(out.Results) / 2
		out.Results = out.Results[:keep]
		led.tier = "B2"
		led.entries = append(append([]elidedField(nil), base...),
			newStoredElision("results", fmt.Sprintf(
				"%d trailing items were dropped to fit the byte budget. The list is score-DESCENDING, so the dropped items are the WEAKEST precedent and the retained prefix is the ranking's substance; the surface below returns the full ranking",
				total-keep), precedentSurface().retrievalPointer, total-keep))
		fits, ferr := attachAndMeasureOut(&out, led, budget.bytes, set)
		if ferr != nil {
			return out, ferr
		}
		if fits {
			return out, nil
		}
	}

	return precedentFloor(out, budget, total)
}

// precedentFloor keeps the summary and a capped resolved_context with results[]
// entirely elided, guaranteeing convergence under
// max(budget, mcpConvergenceFloorBytes). Every retained string is capped
// escape-aware and every retained list is length-capped, so the floor's size is
// a CONSTANT rather than a function of the request.
func precedentFloor(out PrecedentOutput, budget responseBudget, total int) (PrecedentOutput, error) {
	floor := PrecedentOutput{
		ResolvedContext: capPrecedentResolvedContext(out.ResolvedContext),
		Summary:         out.Summary,
		Results:         []precedent.Item{},
		Truncated:       true,
	}
	led := &elisionLedger{
		budget: budget.bytes,
		source: budget.source,
		tier:   floorTierName,
		note:   "reduced to the constant-size floor: the agreement summary plus the capped resolved context, with every ranked item omitted. The entry below is an AGGREGATE — the floor tier's explicit exception to per-field itemisation",
	}
	led.add(aggregateStoredElision("*", fmt.Sprintf(
		"every ranked item was omitted (%d dropped) along with every reason excerpt and score breakdown, and the resolved context's lists were capped to %d entries each; truncated is set. The summary is retained because it is the one part that describes the WHOLE result set rather than any one item. The surface below returns the full ranking",
		total, precedentFloorListCap),
		[]string{precedentSurface().String()}))
	w, err := led.wire()
	if err != nil {
		return out, err
	}
	floor.Elisions = w
	return floor, nil
}

// capPrecedentResolvedContext bounds the echoed context: every scalar capped
// escape-aware, every list capped in LENGTH and each retained element capped in
// BYTES. The untruncated totals the backend reported are preserved, so the
// caller still learns how much there was.
func capPrecedentResolvedContext(rc PrecedentResolvedContext) PrecedentResolvedContext {
	out := PrecedentResolvedContext{
		Repo:                    capJSONString(rc.Repo, floorFieldCap),
		DecisionClass:           capJSONString(rc.DecisionClass, floorFieldCap),
		StageKind:               capJSONString(rc.StageKind, floorFieldCap),
		ConcernCategory:         capJSONString(rc.ConcernCategory, floorFieldCap),
		Severity:                capJSONString(rc.Severity, floorFieldCap),
		RunID:                   capJSONString(rc.RunID, floorFieldCap),
		StageID:                 capJSONString(rc.StageID, floorFieldCap),
		TouchedPathsTotal:       rc.TouchedPathsTotal,
		TouchedPathsTruncated:   rc.TouchedPathsTruncated,
		EscalationKeysTotal:     rc.EscalationKeysTotal,
		EscalationKeysTruncated: rc.EscalationKeysTruncated,
	}
	out.TouchedPaths, out.TouchedPathsTruncated = capPrecedentList(rc.TouchedPaths, rc.TouchedPathsTruncated)
	out.EscalationKeys, out.EscalationKeysTruncated = capPrecedentList(rc.EscalationKeys, rc.EscalationKeysTruncated)
	if out.TouchedPathsTotal < len(rc.TouchedPaths) {
		out.TouchedPathsTotal = len(rc.TouchedPaths)
	}
	if out.EscalationKeysTotal < len(rc.EscalationKeys) {
		out.EscalationKeysTotal = len(rc.EscalationKeys)
	}
	return out
}

func capPrecedentList(in []string, alreadyTruncated bool) ([]string, bool) {
	truncated := alreadyTruncated
	kept := in
	if len(kept) > precedentFloorListCap {
		kept = kept[:precedentFloorListCap]
		truncated = true
	}
	out := make([]string, 0, len(kept))
	for _, v := range kept {
		out = append(out, capJSONString(v, floorFieldCap))
	}
	return out, truncated
}
