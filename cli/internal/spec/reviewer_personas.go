package spec

import "fmt"

// Reviewer personas — `reviewer_personas` (ADR-084 / E55.8 / #3753).
//
// The CLI half of the reviewer_personas validation family: a raw-map port of
// backend/internal/spec/reviewer_personas.go, so `fishhawk validate` and
// `fishhawk doctor` refuse locally exactly what the backend refuses at run
// creation. The two Go modules cannot share a package, so the rules are
// re-implemented here over the yaml.v3 tree and the rejection texts below are
// BYTE-IDENTICAL copies of the backend's constants. Two tests hold the pair
// together: TestReviewerPersonasMessageParity reads both source files over the
// repo root and requires every const declaration verbatim, and the shared
// corpus docs/spec/review-conventions-fixtures.json (which also carries the
// reviewer_personas rows) is run through ValidateBytes here and ParseBytes
// there, each row asserting the same path and message on both sides.
//
// RULE ORDER mirrors the backend's — each persona rung immediately after its
// review_conventions sibling — so the CLI's error list leads with the entry
// the backend returns first:
//
//  1. checkReviewerPersonaDeclarations, after checkReviewConventionDeclarations
//     and BEFORE the workflow loop, over the persona names in sorted order:
//     the remit path rule (the review-conventions rule and reason strings,
//     reviewConventionPathReason), then the decision_record.index path rule
//     when a decision_record is declared (the same rule, E78.5 / #3756), then
//     the persona agent's agent_version range syntax.
//  2. checkStageReviewerPersonas, inside the sorted stage loop after
//     checkStageReviewConventions: the stage type must be plan or implement,
//     then every attached name must resolve to a declared persona, then the
//     stage must configure an agent reviewer.
//  3. checkReviewerPersonasReferenced, after checkReviewConventionsReferenced:
//     a declared persona no stage attaches AND no workflow escalation requires
//     (require.reviewers, ADR-084 D2(c) / E55.9 / #3754) is refused.
//
// The escalation route's own rungs (an undeclared name, no agent-reviewing
// plan / implement stage to join, a no-op attachment) run inside
// checkEscalations (validate.go — checkEscalatedReviewers), but their texts
// are declared HERE, mirroring the backend, so the one parity test covers
// every persona constant.
//
// Each rung reports at most ONE entry per persona (rung 1), per stage (rung 2)
// or per unreferenced name (rung 3) — the backend's first-error-per-site
// shape — so a document violating exactly one rule yields exactly one entry.
//
// THE REUSE ASYMMETRY (identical to review_conventions.go's): the backend's
// resolver DELETES the root and workflow `defaults` blocks after folding them
// into the stages, while this package's resolveV2Reuse KEEPS them. Rung 3
// therefore counts attachments on workflows/*/stages/*/reviewers/personas
// (plus workflows/*/escalations/*/require/reviewers, a workflow member with
// no defaults route) ONLY, never inside a `defaults` block: an attachment
// inherited from defaults.reviewers has already been folded onto the
// inheriting stage and is
// counted THERE, and one made only in a defaults block every stage overrides
// counts nowhere — on both validators. The corpus rows
// persona-defaults-inherited and persona-attached-only-in-unused-defaults pin
// that agreement.
//
// Shape mismatches are skipped, as everywhere in this package: the sweep runs
// AFTER schema validation, so a node that is not the shape the schema requires
// was already rejected upstream.
//
// What stays backend-only is SelectReviewerPersonas and the
// DeclarationSiteFmtReviewerPersonaRemit /
// DeclarationSiteFmtReviewerPersonaDecisionRecord provenance strings — the
// CLI validates the grammar and runs no reviewer.

// PathFmtReviewerPersona is the reported path for a declaration or reference rejection (persona name).
const PathFmtReviewerPersona = "/reviewer_personas/%s"

// PathFmtStageReviewerPersonas is the reported path for a stage-type or no-agents rejection (workflow name, stage index).
const PathFmtStageReviewerPersonas = "/workflows/%s/stages/%d/reviewers/personas"

// PathFmtStageReviewerPersonaItem is the reported path for an undeclared-name rejection (workflow name, stage index, attachment index).
const PathFmtStageReviewerPersonaItem = "/workflows/%s/stages/%d/reviewers/personas/%d"

// MsgFmtReviewerPersonaRemitPathInvalid rejects a persona remit path that is not a canonical repo-relative path (name, path, reason — one of the review-convention path reasons).
const MsgFmtReviewerPersonaRemitPathInvalid = "reviewer_personas.%s: remit.path %q is not a canonical repo-relative path: %s; name the remit document relative to the repository root, slash-separated, with no empty, \".\" or \"..\" segment (e.g. docs/review/security-remit.md)"

// MsgFmtReviewerPersonaDecisionRecordIndexPathInvalid rejects a persona decision_record.index path that is not a canonical repo-relative path (name, path, reason — one of the review-convention path reasons).
const MsgFmtReviewerPersonaDecisionRecordIndexPathInvalid = "reviewer_personas.%s: decision_record.index %q is not a canonical repo-relative path: %s; name the decision-record index relative to the repository root, slash-separated, with no empty, \".\" or \"..\" segment (e.g. docs/adr/index.json)"

// MsgFmtReviewerPersonaStageType rejects reviewers.personas on a stage type other than plan / implement (stage id, stage type).
const MsgFmtReviewerPersonaStageType = "stage %q: reviewers.personas is valid only on a plan or implement stage, not a %q stage: only those two stages run the agent-review loop a persona joins, so this attachment would run no reviewer; move it onto a plan or implement stage (a file-level defaults.reviewers block carrying personas lands on every stage that inherits it — give this stage its own reviewers block)"

// MsgFmtReviewerPersonaUnknown rejects an attached name that resolves to no declared persona (stage id, name, name).
const MsgFmtReviewerPersonaUnknown = "stage %q: reviewers.personas names %q, which matches no entry in the top-level reviewer_personas map; declare it there (reviewer_personas.%s: {agent: {provider: ...}, remit: {path: ...}}) or remove it from the attachment — a persona is declared, never auto-discovered"

// MsgFmtReviewerPersonaNoAgents rejects an attachment on a stage that configures no agent reviewers (stage id).
const MsgFmtReviewerPersonaNoAgents = "stage %q: reviewers.personas attaches reviewer personas but the stage configures no agent reviewers; a persona reviews IN ADDITION to the stage's standard reviewers and inherits their authority, so declare at least one entry under reviewers.agents, or remove reviewers.personas"

// MsgFmtReviewerPersonaUnreferenced rejects a declared persona that no resolved stage attaches and no escalation requires (name).
const MsgFmtReviewerPersonaUnreferenced = "reviewer_personas.%s is declared but no stage attaches it through reviewers.personas and no workflow escalation requires it through require.reviewers, so it would run no review — it is refused rather than silently accepted as a control that does something; attach it on a plan or implement stage, require it from an escalation, or remove the declaration"

// PathFmtEscalationReviewers is the reported path for a require.reviewers no-agent-review rejection (workflow name, escalation index).
const PathFmtEscalationReviewers = "/workflows/%s/escalations/%d/require/reviewers"

// PathFmtEscalationReviewerItem is the reported path for a require.reviewers undeclared-name or no-op rejection (workflow name, escalation index, item index).
const PathFmtEscalationReviewerItem = "/workflows/%s/escalations/%d/require/reviewers/%d"

// MsgFmtEscalationReviewerPersonaUnknown rejects a require.reviewers name that resolves to no declared persona (workflow name, escalation index, name, name).
const MsgFmtEscalationReviewerPersonaUnknown = "workflow %q escalation %d: require.reviewers names %q, which matches no entry in the top-level reviewer_personas map; declare it there (reviewer_personas.%s: {agent: {provider: ...}, remit: {path: ...}}) or remove it from require.reviewers — a persona is declared, never auto-discovered"

// MsgFmtEscalationReviewersNoAgentReview rejects require.reviewers on a workflow with no plan / implement stage configuring agent reviewers (workflow name, escalation index, workflow name).
const MsgFmtEscalationReviewersNoAgentReview = "workflow %q escalation %d: require.reviewers attaches reviewer personas, but workflow %q has no plan or implement stage configuring agent reviewers, so there is no agent-review loop for an escalated persona to join and it would run no review wherever the escalation fires — it is refused rather than silently accepted as a control that does something; declare reviewers.agents on a plan or implement stage of this workflow, or drop require.reviewers"

// MsgFmtEscalationReviewerNoRaiseBaseline is the baseline clause of the require.reviewers no-op rejection, rendered into MsgFmtEscalationNoRaise (persona name).
const MsgFmtEscalationReviewerNoRaiseBaseline = "the workflow's baseline: every plan or implement stage that runs agent review already attaches %q through reviewers.personas, and an escalated persona composes with the static attachment as a de-duplicated union, so the reviewed set is identical to the baseline"

// MsgFmtEscalationReviewerNoRaiseFix is the fix clause of the require.reviewers no-op rejection, rendered into MsgFmtEscalationNoRaise (persona name, persona name).
const MsgFmtEscalationReviewerNoRaiseFix = "Drop %q from the static reviewers.personas of some agent-reviewing stage so the escalation attaches it only where a matching change warrants it, or drop %q from require.reviewers."

// checkReviewerPersonaDeclarations runs rung 1 over every declared persona in
// sorted name order, reporting at most one entry per persona: the remit path
// rule, then the decision_record.index path rule, then the persona agent's
// agent_version range.
func checkReviewerPersonaDeclarations(root map[string]any, errs *[]ValidationErrorEntry) {
	declared, ok := root["reviewer_personas"].(map[string]any)
	if !ok {
		return
	}
	for _, name := range sortedKeys(declared) {
		entry, ok := declared[name].(map[string]any)
		if !ok {
			continue
		}
		ptr := fmt.Sprintf(PathFmtReviewerPersona, name)
		if remit, ok := entry["remit"].(map[string]any); ok {
			if p, ok := remit["path"].(string); ok {
				if reason := reviewConventionPathReason(p); reason != "" {
					*errs = append(*errs, ValidationErrorEntry{
						Path:    ptr + "/remit/path",
						Message: fmt.Sprintf(MsgFmtReviewerPersonaRemitPathInvalid, name, p, reason),
					})
					continue
				}
			}
		}
		if dr, ok := entry["decision_record"].(map[string]any); ok {
			if idx, ok := dr["index"].(string); ok {
				if reason := reviewConventionPathReason(idx); reason != "" {
					*errs = append(*errs, ValidationErrorEntry{
						Path:    ptr + "/decision_record/index",
						Message: fmt.Sprintf(MsgFmtReviewerPersonaDecisionRecordIndexPathInvalid, name, idx, reason),
					})
					continue
				}
			}
		}
		if agent, ok := entry["agent"].(map[string]any); ok {
			appendRangeError(agent["agent_version"], ptr+"/agent/agent_version", errs)
		}
	}
}

// checkStageReviewerPersonas runs rung 2 for stage index i of workflow
// wfName, reporting at most one entry for the stage: stage type, then the
// FIRST attached name with no declared persona, then the agent-reviewer rule.
// declared is the root reviewer_personas map (nil when absent, so every
// attached name is undeclared). The agent-reviewer rule reads
// reviewers.agents only, for the reason checkStageReviewConventions gives
// (the legacy integer reviewers.agent does not exist at v2).
func checkStageReviewerPersonas(stage map[string]any, wfName string, i int, declared map[string]any, errs *[]ValidationErrorEntry) {
	reviewers, ok := stage["reviewers"].(map[string]any)
	if !ok {
		return
	}
	names := rawStringList(reviewers["personas"])
	if len(names) == 0 {
		return
	}
	stageID, _ := stage["id"].(string)
	stageType, _ := stage["type"].(string)
	if !reviewConventionStageType(stageType) {
		*errs = append(*errs, ValidationErrorEntry{
			Path:    fmt.Sprintf(PathFmtStageReviewerPersonas, wfName, i),
			Message: fmt.Sprintf(MsgFmtReviewerPersonaStageType, stageID, stageType),
		})
		return
	}
	for j, name := range names {
		if _, ok := declared[name]; !ok {
			*errs = append(*errs, ValidationErrorEntry{
				Path:    fmt.Sprintf(PathFmtStageReviewerPersonaItem, wfName, i, j),
				Message: fmt.Sprintf(MsgFmtReviewerPersonaUnknown, stageID, name, name),
			})
			return
		}
	}
	if agents, ok := reviewers["agents"].([]any); !ok || len(agents) == 0 {
		*errs = append(*errs, ValidationErrorEntry{
			Path:    fmt.Sprintf(PathFmtStageReviewerPersonas, wfName, i),
			Message: fmt.Sprintf(MsgFmtReviewerPersonaNoAgents, stageID),
		})
	}
}

// checkReviewerPersonasReferenced runs rung 3: every declared persona must be
// attached by some stage or required by some escalation. Attachments are read
// from workflows/*/stages/*/reviewers/personas and
// workflows/*/escalations/*/require/reviewers ONLY — never a `defaults` block,
// which this package's resolver keeps and the backend's deletes (see the
// REUSE ASYMMETRY note above).
func checkReviewerPersonasReferenced(root map[string]any, errs *[]ValidationErrorEntry) {
	declared, ok := root["reviewer_personas"].(map[string]any)
	if !ok || len(declared) == 0 {
		return
	}
	attached := make(map[string]bool)
	workflows, _ := root["workflows"].(map[string]any)
	for _, wfRaw := range workflows {
		wf, ok := wfRaw.(map[string]any)
		if !ok {
			continue
		}
		escalations, _ := wf["escalations"].([]any)
		for _, escRaw := range escalations {
			esc, ok := escRaw.(map[string]any)
			if !ok {
				continue
			}
			if require, ok := esc["require"].(map[string]any); ok {
				for _, name := range rawStringList(require["reviewers"]) {
					attached[name] = true
				}
			}
		}
		stages, _ := wf["stages"].([]any)
		for _, stRaw := range stages {
			st, ok := stRaw.(map[string]any)
			if !ok {
				continue
			}
			reviewers, ok := st["reviewers"].(map[string]any)
			if !ok {
				continue
			}
			for _, name := range rawStringList(reviewers["personas"]) {
				attached[name] = true
			}
		}
	}
	for _, name := range sortedKeys(declared) {
		if !attached[name] {
			*errs = append(*errs, ValidationErrorEntry{
				Path:    fmt.Sprintf(PathFmtReviewerPersona, name),
				Message: fmt.Sprintf(MsgFmtReviewerPersonaUnreferenced, name),
			})
		}
	}
}
