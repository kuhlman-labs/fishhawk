package spec

import (
	"errors"
	"fmt"
	"sort"
)

// Reviewer personas — `reviewer_personas` (ADR-084 / E55.8 / #3753).
//
// ADR-084 binding rule 1: "A named persona declares a model configuration and
// a remit document." A workflow-v2 document may declare a top-level map of
// NAMED personas, each pairing an agent-reviewer model configuration (the
// SAME shape as one reviewers.agents[] entry — $defs/agent_reviewer) with an
// inline remit {path, severity_cap?}, and a plan or implement stage ATTACHES
// them by name through `reviewers.personas`. A persona is a SEPARATE reviewer
// invocation with its own prompt; the stage's standard reviewers and their
// prompt are unchanged. This file owns the typed shape, the semantic
// validation family Validate wires in, and the pure selection function the
// review-loop consumer (backend/internal/server) calls.
//
// The remit REUSES the E55.2 review-conventions machinery rather than naming a
// review_conventions entry: the same canonical repo-relative path rule
// (reviewConventionPathReason, so the reported reason strings are the
// conventions' own), the same severity_cap closed set (grammar only here;
// clamping is E55.10 #3755) and a declaration-site string the repo-document
// resolver echoes. A convention's `required` (default true = fail the review
// stage) and `applies_to` semantics are deliberately NOT carried: a missing
// remit degrades only the persona, never the stage — except that an
// escalation-attached persona that cannot run fails a GATING stage (#3913).
//
// RULE ORDER IS A CONTRACT, mirrored by the CLI's raw-map port so the
// backend's FIRST error is the CLI list's first entry. Each persona rung runs
// immediately AFTER its review_conventions sibling:
//
//  1. DECLARATION checks (validateReviewerPersonaDeclarations), over the
//     persona names in sorted order, after validateReviewConventionDeclarations
//     and BEFORE the workflow loop: the remit path rule, then the
//     decision_record.index path rule (when declared), then the persona
//     agent's agent_version range syntax.
//  2. STAGE checks (validateStageReviewerPersonas), inside validateWorkflow's
//     reviewers block after validateStageReviewConventions: the stage type
//     must be plan or implement, then every attached name must resolve to a
//     declared persona, then the stage must configure an agent reviewer.
//  3. REFERENCE check (validateReviewerPersonasReferenced), after
//     validateReviewConventionsReferenced: a declared persona that no resolved
//     stage attaches AND no workflow escalation requires (require.reviewers,
//     ADR-084 D2(c) / E55.9 / #3754) is refused.
//
// The escalation route's own rungs — an undeclared name, no agent-reviewing
// plan / implement stage to join, and a no-op attachment — run inside
// validateEscalations (escalation.go, rungs 8-10), but their texts are
// declared HERE so the one parity test covers every persona constant.
//
// Every rule reads the RESOLVED document: parse.go folds workflow-v2
// `defaults` / `extends` before Validate runs, so an attachment inherited
// through defaults.reviewers counts on every inheriting stage and is subject
// to the stage-type rule there; an attachment made only in a defaults block no
// stage inherits attaches nothing.
//
// The rejection texts below are single-line `const … = "…"` declarations so
// cli/internal/spec can carry byte-identical copies (the two Go modules cannot
// share a package); keep them single-line. TestReviewerPersonasMessageParity
// (cli/internal/spec) holds the pair together.

// ReviewerPersona is one named entry of the top-level reviewer_personas map.
// The schema's $defs/reviewer_persona is additionalProperties:false and this
// struct round-trips through ParseBytes' DisallowUnknownFields decode, so the
// two MUST stay in lockstep.
type ReviewerPersona struct {
	// Agent is the persona's model configuration — the same shape (and the
	// same schema $defs/agent_reviewer) as one reviewers.agents[] entry.
	Agent AgentReviewer `json:"agent" yaml:"agent"`
	// Remit names the document the persona reviews against.
	Remit PersonaRemit `json:"remit" yaml:"remit"`
	// DecisionRecord optionally opts the persona into the decision-record
	// selection (ADR-084 D4(b) / E78.5 / #3756); nil means the persona reads
	// no decision record and its prompt carries the remit only.
	DecisionRecord *PersonaDecisionRecord `json:"decision_record,omitempty" yaml:"decision_record,omitempty"`
}

// PersonaDecisionRecord is a persona's decision-record opt-in (ADR-084 D4(b)
// / binding rule 4 / E78.5 / #3756). The schema's
// $defs/persona_decision_record is additionalProperties:false, so this struct
// MUST stay in lockstep with it.
type PersonaDecisionRecord struct {
	// Index is the repo-relative, slash-separated path of the decision-record
	// index (adr-index-v1, e.g. docs/adr/index.json), held to the
	// review-conventions canonical path rule. The review loop resolves it at
	// the run's pinned admission commit and selects the records whose
	// applies_to matches the change; that selection lives in
	// backend/internal/decisionrecord, not here.
	Index string `json:"index" yaml:"index"`
}

// PersonaRemit is a persona's remit document. The schema's
// $defs/persona_remit is additionalProperties:false, so this struct MUST stay
// in lockstep with it — and there is deliberately no inline `text` field: a
// remit names a file in the repository under review.
type PersonaRemit struct {
	// Path is the repo-relative, slash-separated remit document path, held to
	// the review-conventions canonical path rule.
	Path string `json:"path" yaml:"path"`
	// SeverityCap optionally bounds the severity the persona may assign;
	// empty means UNCAPPED. Closed set: ReviewConventionSeverityCapLow /
	// ReviewConventionSeverityCapMedium. Grammar only — clamping is E55.10
	// (#3755).
	SeverityCap string `json:"severity_cap,omitempty" yaml:"severity_cap,omitempty"`
}

// SelectedReviewerPersona is one persona SelectReviewerPersonas attached to a
// stage's review — the value the review loop resolves into a separate
// reviewer invocation.
type SelectedReviewerPersona struct {
	// Name is the reviewer_personas map key.
	Name string
	// Agent is the persona's declared model configuration.
	Agent AgentReviewer
	// RemitPath is the declared repo-relative remit document path.
	RemitPath string
	// SeverityCap is the declared remit cap; "" means uncapped.
	SeverityCap string
	// DeclarationSite is the free-form provenance repodoc.Declaration expects
	// (formatted by DeclarationSiteFmtReviewerPersonaRemit), echoed into every
	// resolution error so an operator knows which knob produced it.
	DeclarationSite string
	// DecisionRecordIndex is the declared decision-record index path; ""
	// means the persona declares no decision_record and the review loop
	// performs no decision-record read for it (E78.5 / #3756).
	DecisionRecordIndex string
	// DecisionRecordDeclarationSite is the provenance string for the index
	// and every record resolved through it (formatted by
	// DeclarationSiteFmtReviewerPersonaDecisionRecord); "" exactly when
	// DecisionRecordIndex is "".
	DecisionRecordDeclarationSite string
}

// DeclarationSiteFmtReviewerPersonaRemit formats SelectedReviewerPersona's
// DeclarationSite from the persona name.
const DeclarationSiteFmtReviewerPersonaRemit = "reviewer_personas.%s.remit in .fishhawk/workflows.yaml"

// DeclarationSiteFmtReviewerPersonaDecisionRecord formats
// SelectedReviewerPersona's DecisionRecordDeclarationSite from the persona
// name.
const DeclarationSiteFmtReviewerPersonaDecisionRecord = "reviewer_personas.%s.decision_record.index in .fishhawk/workflows.yaml"

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

// ErrReviewerPersonaStageType is returned (wrapped) by SelectReviewerPersonas
// for a stage attaching personas that is not a plan or implement stage.
var ErrReviewerPersonaStageType = errors.New("reviewer personas are attachable only on a plan or implement stage")

// ErrReviewerPersonaUndeclared is returned (wrapped) by
// SelectReviewerPersonas for an attached name with no declared persona.
var ErrReviewerPersonaUndeclared = errors.New("reviewer persona is not declared in reviewer_personas")

// sortedReviewerPersonaNames returns the declared names in sorted order, so
// both the backend's first error and the CLI's list are deterministic.
func sortedReviewerPersonaNames(s *Spec) []string {
	names := make([]string, 0, len(s.ReviewerPersonas))
	for name := range s.ReviewerPersonas {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// validateReviewerPersonaDeclarations runs the per-persona DECLARATION checks
// (rule-order rung 1): the remit path rule, then the decision_record.index
// path rule when a decision_record is declared (the same
// reviewConventionPathReason, E78.5 / #3756), then the persona agent's
// agent_version range syntax (the same ValidAgentVersionRange check every
// reviewers.agents[] entry gets, at the persona's own path).
func validateReviewerPersonaDeclarations(s *Spec) error {
	for _, name := range sortedReviewerPersonaNames(s) {
		p := s.ReviewerPersonas[name]
		ptr := fmt.Sprintf(PathFmtReviewerPersona, name)
		if reason := reviewConventionPathReason(p.Remit.Path); reason != "" {
			return &ValidationError{
				Path:    ptr + "/remit/path",
				Message: fmt.Sprintf(MsgFmtReviewerPersonaRemitPathInvalid, name, p.Remit.Path, reason),
			}
		}
		if p.DecisionRecord != nil {
			if reason := reviewConventionPathReason(p.DecisionRecord.Index); reason != "" {
				return &ValidationError{
					Path:    ptr + "/decision_record/index",
					Message: fmt.Sprintf(MsgFmtReviewerPersonaDecisionRecordIndexPathInvalid, name, p.DecisionRecord.Index, reason),
				}
			}
		}
		if p.Agent.AgentVersion != "" {
			if err := ValidAgentVersionRange(p.Agent.AgentVersion); err != nil {
				return &ValidationError{Path: ptr + "/agent/agent_version", Message: err.Error()}
			}
		}
	}
	return nil
}

// validateStageReviewerPersonas runs the STAGE checks (rule-order rung 2) for
// stage index i of workflow wf: stage type, then each name resolves (path
// .../personas/<j>), then the stage has an agent reviewer. It shares the
// plan/implement predicate with review conventions — the same agent-review
// loop is what both feed.
func validateStageReviewerPersonas(s *Spec, wf string, i int, st *Stage) error {
	if st.Reviewers == nil || len(st.Reviewers.Personas) == 0 {
		return nil
	}
	if !reviewConventionStageType(st.Type) {
		return &ValidationError{
			Path:    fmt.Sprintf(PathFmtStageReviewerPersonas, wf, i),
			Message: fmt.Sprintf(MsgFmtReviewerPersonaStageType, st.ID, st.Type),
		}
	}
	for j, name := range st.Reviewers.Personas {
		if _, ok := s.ReviewerPersonas[name]; !ok {
			return &ValidationError{
				Path:    fmt.Sprintf(PathFmtStageReviewerPersonaItem, wf, i, j),
				Message: fmt.Sprintf(MsgFmtReviewerPersonaUnknown, st.ID, name, name),
			}
		}
	}
	if st.Reviewers.AgentCount() == 0 {
		return &ValidationError{
			Path:    fmt.Sprintf(PathFmtStageReviewerPersonas, wf, i),
			Message: fmt.Sprintf(MsgFmtReviewerPersonaNoAgents, st.ID),
		}
	}
	return nil
}

// validateReviewerPersonasReferenced runs the REFERENCE check (rule-order rung
// 3): every declared persona must be attached by at least one stage of the
// resolved document OR required by at least one workflow escalation's
// require.reviewers (E55.9 / #3754). Only workflows/*/stages/*/reviewers/personas
// and workflows/*/escalations/*/require/reviewers count — the backend's
// resolver has already deleted `defaults`, so an attachment made only inside a
// defaults.reviewers block no stage inherits attaches nothing (escalations are
// a workflow member with no defaults route at all).
func validateReviewerPersonasReferenced(s *Spec) error {
	if len(s.ReviewerPersonas) == 0 {
		return nil
	}
	attached := make(map[string]bool)
	for _, wf := range s.Workflows {
		for _, e := range wf.Escalations {
			for _, name := range e.Require.Reviewers {
				attached[name] = true
			}
		}
		for _, st := range wf.Stages {
			if st.Reviewers == nil {
				continue
			}
			for _, name := range st.Reviewers.Personas {
				attached[name] = true
			}
		}
	}
	for _, name := range sortedReviewerPersonaNames(s) {
		if !attached[name] {
			return &ValidationError{
				Path:    fmt.Sprintf(PathFmtReviewerPersona, name),
				Message: fmt.Sprintf(MsgFmtReviewerPersonaUnreferenced, name),
			}
		}
	}
	return nil
}

// SelectReviewerPersonas returns the reviewer personas stage st attaches, in
// the order the stage's reviewers.personas lists them — the pure consumer
// contract the plan-review and implement-review loops call to build one extra
// reviewer invocation per persona.
//
// Unlike SelectReviewConventions there is no Change argument: a persona has
// no applies_to, so an attached persona reviews every change the stage
// reviews.
//
// It fails closed for a hand-built Spec that skipped Validate: a stage type
// other than plan / implement returns ErrReviewerPersonaStageType, and a name
// with no declared persona returns ErrReviewerPersonaUndeclared (both
// wrapped). A nil stage, a nil reviewers block, or an empty attachment
// returns (nil, nil).
//
// Whether the remit document exists at the reviewed base is NOT decided here:
// that is the repo-document resolver's ErrMissingDocument at review time, and
// a missing remit degrades only the persona (never the stage — except that an
// escalation-attached persona that cannot run fails a GATING stage, #3913).
func (s *Spec) SelectReviewerPersonas(st *Stage) ([]SelectedReviewerPersona, error) {
	if st == nil || st.Reviewers == nil || len(st.Reviewers.Personas) == 0 {
		return nil, nil
	}
	if !reviewConventionStageType(st.Type) {
		return nil, fmt.Errorf("stage %q (type %q): %w", st.ID, st.Type, ErrReviewerPersonaStageType)
	}
	out := make([]SelectedReviewerPersona, 0, len(st.Reviewers.Personas))
	for _, name := range st.Reviewers.Personas {
		sel, ok := s.selectReviewerPersona(name)
		if !ok {
			return nil, fmt.Errorf("stage %q attaches %q: %w", st.ID, name, ErrReviewerPersonaUndeclared)
		}
		out = append(out, sel)
	}
	return out, nil
}

// SelectNamedReviewerPersonas returns the declared personas named by names —
// the escalation-attachment twin of SelectReviewerPersonas (ADR-084 D2(c) /
// E55.9 / #3754). The review loop calls it with the de-duplicated persona set
// backend/internal/escalation.PersonaAttachments derives from the escalations
// that FIRED for the change under review, so an escalation-attached persona
// resolves into the same SelectedReviewerPersona (and thus the same reviewer
// invocation and remit resolution) a static attachment does.
//
// The result is in INPUT order with duplicates dropped (the first occurrence
// wins). It fails closed for a hand-built Spec that skipped Validate: a name
// with no declared persona returns ErrReviewerPersonaUndeclared (wrapped) and
// NO partial result. An empty or nil names returns (nil, nil). There is no
// stage-type check — the caller already decided which review this is.
func (s *Spec) SelectNamedReviewerPersonas(names []string) ([]SelectedReviewerPersona, error) {
	if len(names) == 0 {
		return nil, nil
	}
	seen := make(map[string]bool, len(names))
	out := make([]SelectedReviewerPersona, 0, len(names))
	for _, name := range names {
		if seen[name] {
			continue
		}
		seen[name] = true
		sel, ok := s.selectReviewerPersona(name)
		if !ok {
			return nil, fmt.Errorf("escalation requires %q: %w", name, ErrReviewerPersonaUndeclared)
		}
		out = append(out, sel)
	}
	return out, nil
}

// selectReviewerPersona is the ONE name -> SelectedReviewerPersona mapping
// both selection functions share, so a static and an escalation attachment of
// the same persona resolve to identical values. ok is false when s is nil or
// declares no persona by that name.
func (s *Spec) selectReviewerPersona(name string) (SelectedReviewerPersona, bool) {
	if s == nil {
		return SelectedReviewerPersona{}, false
	}
	p, ok := s.ReviewerPersonas[name]
	if !ok {
		return SelectedReviewerPersona{}, false
	}
	sel := SelectedReviewerPersona{
		Name:            name,
		Agent:           p.Agent,
		RemitPath:       p.Remit.Path,
		SeverityCap:     p.Remit.SeverityCap,
		DeclarationSite: fmt.Sprintf(DeclarationSiteFmtReviewerPersonaRemit, name),
	}
	if p.DecisionRecord != nil {
		sel.DecisionRecordIndex = p.DecisionRecord.Index
		sel.DecisionRecordDeclarationSite = fmt.Sprintf(DeclarationSiteFmtReviewerPersonaDecisionRecord, name)
	}
	return sel, true
}
