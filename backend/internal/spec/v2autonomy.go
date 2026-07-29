package spec

import (
	"encoding/json"
	"fmt"
	"sort"
)

// workflow-v2 unified autonomy grammar (ADR-066 / E52.10 / #2222).
//
// v2 REMOVES the v0/v1 `operator_agent` block and replaces it with two
// surfaces that say the same thing more legibly:
//
//	autonomy: low | medium | high     tier shorthand — expands to a matrix
//	actions:  {<class>: {mode, when}} the per-action-class matrix
//
// `mode` is the whole grammar: `gated` (the human acts — the fail-closed
// default and exactly what an absent may_* knob meant), `auto` (the
// operator agent may act, and only then is a `when` condition required),
// and `report` (the operator agent surfaces a proposal without acting).
//
// THE LOAD-BEARING DECISION is the DERIVED-OperatorAgent bridge below.
// Rather than teaching delegation.Evaluate, the four checkDelegation
// action handlers, AutoDriveRunGate and the campaign driver a second
// grammar, a v2 workflow's RESOLVED matrix is projected back onto the
// existing *OperatorAgent struct. Every enforcement site keeps reading
// the field it already reads and stays byte-identical. Both non-auto
// modes project to an EMPTY knob — today's knob-absence — so nothing an
// author can write in the new grammar widens agent authority beyond what
// a may_* knob could. A consumer this file's authors missed keeps
// reading a nil/empty block and REFUSES: fail-closed, never a widening.
//
// The class-name set is deliberately OPEN (ADR-065 per-workflow-type
// extensibility). An unknown class is safe BY CONSTRUCTION rather than by
// enumeration: it maps to no knob, so at `mode: gated` / `mode: report`
// it delegates nothing, and at `mode: auto` it is REJECTED because no
// backend-evaluable condition exists for it.
//
// ORDERING (see ParseBytes, and backend/internal/spec/README.md):
//
//	checkV2RemovedForms   -> before schema validation (actionable message
//	                         for a v2 document still carrying operator_agent)
//	schema.Validate       -> the structural gate
//	normalizeV2Shapes     -> after validation, before the typed decode
//	deriveV2Autonomy      -> HERE: AFTER the typed decode, BEFORE Validate
//
// This pass runs after the typed decode because it needs ActionMatrix's
// custom unmarshaller's OUTPUT (the partitioned Classes map), not the raw
// document; it runs before Validate so the derived block is in place for
// every semantic rule and every consumer of the returned *Spec.

// AutonomyTier is the v2 tier shorthand: one word that expands to a full
// action matrix. The three values are METHODOLOGY.md's autonomy tiers and
// reproduce the shipped workflow-preset-<tier>.yaml delegation blocks.
type AutonomyTier string

// The three autonomy tiers. The schema enum is the closed set, so an
// unrecognized tier never reaches a parsed Spec.
const (
	// TierLow delegates nothing: every action class is gated and every
	// judgment pages the human. It implies no page_human_on list — with
	// nothing delegated there is nothing to except.
	TierLow AutonomyTier = "low"
	// TierMedium delegates approve, fixup and retry under their named
	// conditions; waive and merge stay gated.
	TierMedium AutonomyTier = "medium"
	// TierHigh is medium plus waive and merge at mode: auto, so a clean
	// change can be carried through to merge.
	TierHigh AutonomyTier = "high"
)

// ActionMode says WHO acts on an action class.
type ActionMode string

// The three action modes. The schema enum is the closed set.
const (
	// ModeReport surfaces a proposal for the human without acting. It
	// projects to an EMPTY knob — report never delegates.
	ModeReport ActionMode = "report"
	// ModeGated means the human acts. This is the fail-closed default an
	// absent class entry resolves to, and it projects to an EMPTY knob —
	// byte-identically to v0/v1 knob-absence.
	ModeGated ActionMode = "gated"
	// ModeAuto means the operator agent may act without paging. It is the
	// only mode that widens authority, and the only one that requires a
	// `when` condition.
	ModeAuto ActionMode = "auto"
)

// The five action classes the backend knows how to evaluate. The
// class-name set is open (see the file header), so these are the KNOWN
// classes, not the legal ones.
const (
	ActionApprove = "approve"
	ActionFixup   = "fixup"
	ActionWaive   = "waive"
	ActionRetry   = "retry"
	ActionMerge   = "merge"
)

// knownActionOrder is the canonical order known classes are resolved and
// surfaced in, so a ResolvedMatrix is deterministic across runs (Go map
// iteration is randomized). Extension classes follow, sorted by name.
var knownActionOrder = []string{ActionApprove, ActionFixup, ActionWaive, ActionRetry, ActionMerge}

// classConditions is the action-class condition registry: the single
// backend-evaluable condition legal for each known class. A class absent
// from this map is an EXTENSION class — it has no backend-evaluable
// condition at all, which is precisely why `mode: auto` on it is rejected.
var classConditions = map[string]DelegationCondition{
	ActionApprove: ConditionCleanDualApproval,
	ActionFixup:   ConditionConvergentConcerns,
	ActionWaive:   ConditionSoloLow,
	ActionRetry:   ConditionInfraFlake,
	ActionMerge:   ConditionGatesResolvedCIGreen,
}

// reservedMatrixKeys names the `actions` properties that are NOT action
// classes. It is the ONE list consulted by the unmarshaller (which routes
// these into typed fields), the marshaller (which re-emits them as
// siblings) and the validator, so a future reserved key cannot be added
// to one and missed by the others.
var reservedMatrixKeys = map[string]bool{
	"page_human_on": true,
	"model_policy":  true,
}

// ActionEntry is one action class's declared setting.
type ActionEntry struct {
	Mode ActionMode          `json:"mode" yaml:"mode"`
	When DelegationCondition `json:"when,omitempty" yaml:"when,omitempty"`
	// MinSeverity is accepted on the `fixup` class only. It is v0/v1's
	// operator_agent.route_fixup_min_severity, moved onto the class it
	// governs.
	MinSeverity string `json:"min_severity,omitempty" yaml:"min_severity,omitempty"`
}

// ActionMatrix is the parsed `actions` block: an OPEN map of action
// classes plus the two reserved keys.
//
// The Classes map is populated by the custom UnmarshalJSON below, NOT by
// ordinary struct decoding. That is load-bearing: `actions` is one JSON
// object mixing arbitrary class names with two reserved keys, and no
// struct-tag arrangement routes an arbitrary sibling property into a
// named field. Without the partitioning, `actions: {approve: {mode:
// gated}}` would decode to an EMPTY Classes map — the explicit entry
// would vanish before validation AND before resolution, and an author
// tightening one class on an `autonomy: medium` workflow would silently
// get medium's `approve: auto`.
type ActionMatrix struct {
	// Classes maps each declared ACTION CLASS to its entry. Reserved keys
	// are never present here.
	Classes map[string]ActionEntry
	// PageHumanOn is the reserved `page_human_on` key: events that always
	// page the human regardless of any class's mode (v0/v1's
	// must_page_human).
	PageHumanOn []string
	// ModelPolicy is the reserved `model_policy` key (#1421), moved here
	// from the removed operator_agent block.
	ModelPolicy *ModelPolicy
}

// UnmarshalJSON partitions the `actions` object: the reserved keys into
// their typed fields, EVERY other property into Classes.
//
// Implementing json.Unmarshaler takes over decoding of this value
// entirely, so the outer decoder's DisallowUnknownFields setting does not
// reject the arbitrary sibling properties routed into Classes — which is
// exactly what an open class-name set needs. Unknown keys INSIDE an entry
// are still rejected: the schema's action_entry is additionalProperties:
// false, and that gate runs before the typed decode.
func (m *ActionMatrix) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*m = ActionMatrix{}
	for k, v := range raw {
		switch k {
		case "page_human_on":
			if err := json.Unmarshal(v, &m.PageHumanOn); err != nil {
				return fmt.Errorf("actions.page_human_on: %w", err)
			}
		case "model_policy":
			if err := json.Unmarshal(v, &m.ModelPolicy); err != nil {
				return fmt.Errorf("actions.model_policy: %w", err)
			}
		default:
			var entry ActionEntry
			if err := json.Unmarshal(v, &entry); err != nil {
				return fmt.Errorf("actions.%s: %w", k, err)
			}
			if m.Classes == nil {
				m.Classes = make(map[string]ActionEntry, len(raw))
			}
			m.Classes[k] = entry
		}
	}
	return nil
}

// MarshalJSON is UnmarshalJSON's symmetric inverse: class entries are
// re-emitted as siblings of the reserved keys, so a matrix round-trips.
func (m ActionMatrix) MarshalJSON() ([]byte, error) {
	out := make(map[string]any, len(m.Classes)+len(reservedMatrixKeys))
	for k, v := range m.Classes {
		if reservedMatrixKeys[k] {
			// Unreachable through UnmarshalJSON (it never routes a reserved
			// key into Classes), but a hand-built matrix could collide; the
			// reserved key wins so the emitted document stays decodable.
			continue
		}
		out[k] = v
	}
	// Emit an explicit EMPTY page list too: nil vs non-nil-empty is
	// load-bearing. A non-nil empty page_human_on is a deliberate "page on
	// NOTHING" override that resolveBlock honours (it overrides the tier's
	// list); guarding on len() > 0 would drop it, so a round trip would decode
	// back to nil and silently fall through to the tier's page list — losing
	// the override. Guard on nil so the round trip is symmetric.
	if m.PageHumanOn != nil {
		out["page_human_on"] = m.PageHumanOn
	}
	if m.ModelPolicy != nil {
		out["model_policy"] = m.ModelPolicy
	}
	return json.Marshal(out)
}

// ResolutionSource records WHICH input decided an action class's mode —
// the provenance #2267's min(workflow tier, issue label, escalation
// ceiling) composition needs a field to record its clamp in. The value
// set is deliberately open-ended so a later rung can add its own source
// without reshaping the carrier.
type ResolutionSource string

const (
	// SourceExplicit means an `actions` entry named this class directly.
	SourceExplicit ResolutionSource = "explicit"
	// SourceTier means the class took its value from the `autonomy` tier
	// expansion.
	SourceTier ResolutionSource = "tier"
	// SourceDefault means nothing named the class, so it fell to the
	// fail-closed `mode: gated` default.
	SourceDefault ResolutionSource = "default"
)

// ResolvedAction is one action class after resolution, carrying its
// provenance.
type ResolvedAction struct {
	Action      string              `json:"action"`
	Mode        ActionMode          `json:"mode"`
	Condition   DelegationCondition `json:"condition,omitempty"`
	MinSeverity string              `json:"min_severity,omitempty"`
	Source      ResolutionSource    `json:"source"`
}

// ResolvedMatrix is a fully resolved autonomy block: every known class
// plus every declared extension class, each with its provenance.
type ResolvedMatrix struct {
	// Tier is the declared shorthand, empty when the block declared only
	// `actions`.
	Tier        AutonomyTier     `json:"tier,omitempty"`
	Actions     []ResolvedAction `json:"actions"`
	PageHumanOn []string         `json:"page_human_on,omitempty"`
	ModelPolicy *ModelPolicy     `json:"model_policy,omitempty"`
}

// expandTier returns the action matrix a tier shorthand stands for.
//
// The three expansions reproduce the shipped docs/spec/workflow-preset-
// <tier>.yaml delegation blocks with ONE deliberate vocabulary change:
// the page list emits the v2 spelling gating_reviewer_reject, never the
// bare reviewer_reject that E52.3 / #2215 removed from v2's must_page_human
// enum. A tier must not expand to a value undeclarable under the grammar
// it belongs to.
//
// An unrecognized tier returns the zero matrix, so resolution falls
// through to the fail-closed `mode: gated` default for every class. The
// schema enum is the real gate (a bad tier is a *SchemaError long before
// this runs); this is the belt to that braces.
func expandTier(t AutonomyTier) ActionMatrix {
	pageList := []string{
		PageEventGatingReviewerReject,
		PageEventPlanRejection,
		PageEventScopeAmendment,
		PageEventBudgetOverride,
		PageEventPolicyOverride,
		PageEventExceptionRequest,
		PageEventRequirementArbitration,
	}
	switch t {
	case TierLow:
		// Human-led: nothing delegated. The low preset declares no
		// operator_agent block at all, so it implies no page list either.
		return ActionMatrix{Classes: map[string]ActionEntry{
			ActionApprove: {Mode: ModeGated},
			ActionFixup:   {Mode: ModeGated},
			ActionWaive:   {Mode: ModeGated},
			ActionRetry:   {Mode: ModeGated},
			ActionMerge:   {Mode: ModeGated},
		}}
	case TierMedium:
		return ActionMatrix{
			Classes: map[string]ActionEntry{
				ActionApprove: {Mode: ModeAuto, When: ConditionCleanDualApproval},
				ActionFixup:   {Mode: ModeAuto, When: ConditionConvergentConcerns},
				ActionRetry:   {Mode: ModeAuto, When: ConditionInfraFlake},
				ActionWaive:   {Mode: ModeGated},
				ActionMerge:   {Mode: ModeGated},
			},
			PageHumanOn: pageList,
		}
	case TierHigh:
		return ActionMatrix{
			Classes: map[string]ActionEntry{
				ActionApprove: {Mode: ModeAuto, When: ConditionCleanDualApproval},
				ActionFixup:   {Mode: ModeAuto, When: ConditionConvergentConcerns},
				ActionRetry:   {Mode: ModeAuto, When: ConditionInfraFlake},
				ActionWaive:   {Mode: ModeAuto, When: ConditionSoloLow},
				ActionMerge:   {Mode: ModeAuto, When: ConditionGatesResolvedCIGreen},
			},
			PageHumanOn: pageList,
		}
	}
	return ActionMatrix{}
}

// autonomyBlock is one declared (tier, matrix) pair — the unit that wins
// or loses WHOLESALE at a resolution rung.
type autonomyBlock struct {
	tier   AutonomyTier
	matrix *ActionMatrix
}

// declaredAutonomy returns the block a level declares, or nil when it
// declares neither surface. Declaring EITHER `autonomy` or `actions`
// counts as declaring the block: that is what makes a gate's override
// wholesale even when it names one class.
func declaredAutonomy(tier AutonomyTier, matrix *ActionMatrix) *autonomyBlock {
	if tier == "" && matrix == nil {
		return nil
	}
	return &autonomyBlock{tier: tier, matrix: matrix}
}

// ResolveAutonomy resolves the autonomy block governing a gate into a
// fully populated matrix, or nil when no level declares one (fail-closed:
// nothing delegated, every judgment pages the human).
//
// LADDER: a gate declaring EITHER `autonomy` or `actions` supplies the
// WHOLE block; otherwise the workflow's own block applies. Entries are
// NEVER merged across levels — a gate declaring `actions: {merge: …}` on
// an `autonomy: high` workflow has NO tier, so every class it does not
// name resolves to the fail-closed default, not to high's value. That is
// the same wholesale-override semantics the operator_agent block had.
//
// WITHIN the chosen block: start from the tier expansion (Source=tier),
// overlay each explicit class entry (Source=explicit), which wins for
// THAT CLASS ONLY, then give every known class still unaccounted for
// `mode: gated` (Source=default). page_human_on and model_policy come
// from the block when it declares them, else from the tier expansion.
//
// g may be nil for callers resolving outside any gate context.
func ResolveAutonomy(w *Workflow, g *Gate) *ResolvedMatrix {
	var block *autonomyBlock
	if g != nil {
		block = declaredAutonomy(g.Autonomy, g.Actions)
	}
	if block == nil && w != nil {
		block = declaredAutonomy(w.Autonomy, w.Actions)
	}
	if block == nil {
		return nil
	}
	return resolveBlock(block)
}

// resolveBlock is ResolveAutonomy's per-block resolution, split out so the
// ladder above reads as the ladder and nothing else.
func resolveBlock(b *autonomyBlock) *ResolvedMatrix {
	expanded := expandTier(b.tier)
	entries := make(map[string]ResolvedAction, len(knownActionOrder))
	for name, e := range expanded.Classes {
		entries[name] = resolvedFrom(name, e, SourceTier)
	}
	if b.matrix != nil {
		for name, e := range b.matrix.Classes {
			entries[name] = resolvedFrom(name, e, SourceExplicit)
		}
	}
	for _, name := range knownActionOrder {
		if _, ok := entries[name]; !ok {
			entries[name] = ResolvedAction{Action: name, Mode: ModeGated, Source: SourceDefault}
		}
	}

	out := &ResolvedMatrix{Tier: b.tier, Actions: orderActions(entries)}
	out.PageHumanOn = expanded.PageHumanOn
	out.ModelPolicy = expanded.ModelPolicy
	if b.matrix != nil {
		if b.matrix.PageHumanOn != nil {
			out.PageHumanOn = b.matrix.PageHumanOn
		}
		if b.matrix.ModelPolicy != nil {
			out.ModelPolicy = b.matrix.ModelPolicy
		}
	}
	return out
}

// resolvedFrom projects one declared entry onto its resolved form. The
// condition is carried ONLY for a class whose mode actually consumes it;
// a `when` written next to `mode: gated` is meaningless (the schema
// permits it, the grammar ignores it) and is dropped here rather than
// surfaced as if it governed something.
func resolvedFrom(name string, e ActionEntry, src ResolutionSource) ResolvedAction {
	out := ResolvedAction{Action: name, Mode: e.Mode, Source: src, MinSeverity: e.MinSeverity}
	if e.Mode == ModeAuto || e.Mode == ModeReport {
		out.Condition = e.When
	}
	return out
}

// orderActions renders the resolved entries deterministically: the five
// known classes in canonical order, then extension classes sorted by name.
func orderActions(entries map[string]ResolvedAction) []ResolvedAction {
	out := make([]ResolvedAction, 0, len(entries))
	seen := make(map[string]bool, len(entries))
	for _, name := range knownActionOrder {
		if e, ok := entries[name]; ok {
			out = append(out, e)
			seen[name] = true
		}
	}
	extra := make([]string, 0, len(entries))
	for name := range entries {
		if !seen[name] {
			extra = append(extra, name)
		}
	}
	sort.Strings(extra)
	for _, name := range extra {
		out = append(out, entries[name])
	}
	return out
}

// DerivedOperatorAgent projects a resolved matrix back onto the v0/v1
// *OperatorAgent struct — the bridge that keeps every enforcement site
// already in the tree unchanged (see the file header).
//
// A knob carries its class's condition ONLY when that class resolved to
// `mode: auto`. `gated`, `report` and every extension class leave the knob
// EMPTY — exactly today's knob-absence — so nothing expressible in the new
// grammar widens agent authority beyond what a may_* knob could.
//
// nil in, nil out: a workflow declaring neither surface derives no block,
// so delegation.Configured stays false and the run-status delegation block
// stays omitted, byte-identically to today.
func DerivedOperatorAgent(rm *ResolvedMatrix) *OperatorAgent {
	if rm == nil {
		return nil
	}
	out := &OperatorAgent{MustPageHuman: rm.PageHumanOn, ModelPolicy: rm.ModelPolicy}
	for _, a := range rm.Actions {
		if a.Action == ActionFixup {
			out.RouteFixupMinSeverity = a.MinSeverity
		}
		if a.Mode != ModeAuto {
			continue
		}
		switch a.Action {
		case ActionApprove:
			out.MayApprove = a.Condition
		case ActionFixup:
			out.MayRouteFixup = a.Condition
		case ActionWaive:
			out.MayWaive = a.Condition
		case ActionRetry:
			out.MayRetry = a.Condition
		case ActionMerge:
			out.MayMerge = a.Condition
		}
	}
	return out
}

// deriveV2Autonomy validates every declared autonomy block in a decoded
// v2 spec and projects each onto the derived OperatorAgent field the rest
// of the tree reads. Called from ParseBytes for a routed major >= 2 only,
// after the typed decode and before Validate.
//
// A level declaring NEITHER surface derives nil, leaving OperatorAgent nil
// so an unconfigured v2 workflow behaves byte-identically to today.
func deriveV2Autonomy(s *Spec) error {
	names := make([]string, 0, len(s.Workflows))
	for name := range s.Workflows {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		wf := s.Workflows[name]
		base := "/workflows/" + name
		if err := validateAutonomy(wf.Actions, base); err != nil {
			return err
		}
		wf.OperatorAgent = DerivedOperatorAgent(ResolveAutonomy(&wf, nil))
		for si := range wf.Stages {
			for gi := range wf.Stages[si].Gates {
				g := &wf.Stages[si].Gates[gi]
				gatePath := fmt.Sprintf("%s/stages/%d/gates/%d", base, si, gi)
				if err := validateAutonomy(g.Actions, gatePath); err != nil {
					return err
				}
				if declaredAutonomy(g.Autonomy, g.Actions) == nil {
					continue
				}
				g.OperatorAgent = DerivedOperatorAgent(ResolveAutonomy(&wf, g))
			}
		}
		s.Workflows[name] = wf
	}
	return nil
}

// validateAutonomy enforces the rules the open class-name set puts out of
// JSON Schema's reach, returning a *ValidationError naming the offending
// class. Three rejections, all of them about `mode: auto` — the only mode
// that widens authority:
//
//	auto on an EXTENSION class  -> no backend-evaluable condition exists
//	auto with no `when`         -> names the class's legal condition
//	auto with the wrong `when`  -> names both conditions
//
// Plus one placement rule: min_severity governs the fixup threshold and is
// accepted on that class only.
//
// And one report rule: a `mode: report` entry that declares a `when` must
// name THIS class's own condition (a foreign or extension-class `when` is
// rejected) — the same per-class binding auto enforces, so a report entry
// the firing arm cannot evaluate is refused at validation rather than
// silently dropped at fire time. A `mode: report` entry with NO `when` is
// always accepted: it fires on gate-live.
//
// `mode: gated` on an unknown class, and a bare `mode: report` (no `when`)
// on an unknown class, are ACCEPTED (ADR-065 extensibility) and are
// fail-closed by construction: neither maps to a knob, so neither delegates
// anything.
func validateAutonomy(m *ActionMatrix, path string) *ValidationError {
	if m == nil {
		return nil
	}
	names := make([]string, 0, len(m.Classes))
	for name := range m.Classes {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		e := m.Classes[name]
		if e.MinSeverity != "" && name != ActionFixup {
			return &ValidationError{
				Path:    fmt.Sprintf("%s/actions/%s/min_severity", path, name),
				Message: fmt.Sprintf("min_severity is accepted on the %q action class only (it tunes convergent_concerns' open-concern threshold); %q declares it", ActionFixup, name),
			}
		}
		// mode: report MAY omit `when` (it fires on gate-live). When it
		// declares one, the per-class binding the schema documents as
		// backend-enforced applies to report as well: the `when` must name
		// THIS class's own condition. A foreign or extension-class `when`
		// names a predicate the report-firing arm cannot evaluate against
		// this gate, and would otherwise be accepted here but silently
		// DROPPED at firing time (never surfacing a proposal). Reject it at
		// validation so report-mode firing is correct for every schema-valid
		// `when` value a document can carry.
		if e.Mode == ModeReport && e.When != "" {
			want, known := classConditions[name]
			if !known {
				return &ValidationError{
					Path:    fmt.Sprintf("%s/actions/%s/when", path, name),
					Message: fmt.Sprintf("action class %q is an extension class with no backend-evaluable condition, so it cannot declare a `when` at mode: report; drop the `when` (a bare mode: report fires on gate-live), or use one of the known classes: %s", name, formatKnownActions()),
				}
			}
			if e.When != want {
				return &ValidationError{
					Path:    fmt.Sprintf("%s/actions/%s/when", path, name),
					Message: fmt.Sprintf("action class %q declares mode: report with when: %s, which is not its condition; %q accepts only when: %s (or no `when`, to fire on gate-live)", name, e.When, name, want),
				}
			}
		}
		if e.Mode != ModeAuto {
			continue
		}
		want, known := classConditions[name]
		if !known {
			return &ValidationError{
				Path:    fmt.Sprintf("%s/actions/%s/mode", path, name),
				Message: fmt.Sprintf("action class %q cannot declare mode: auto — it is an extension class, and no backend-evaluable condition exists for it; declare mode: gated (the human acts) or mode: report (surface a proposal), or use one of the known classes: %s", name, formatKnownActions()),
			}
		}
		if e.When == "" {
			return &ValidationError{
				Path:    fmt.Sprintf("%s/actions/%s", path, name),
				Message: fmt.Sprintf("action class %q declares mode: auto with no `when` condition; mode: auto requires when: %s", name, want),
			}
		}
		if e.When != want {
			return &ValidationError{
				Path:    fmt.Sprintf("%s/actions/%s/when", path, name),
				Message: fmt.Sprintf("action class %q declares mode: auto with when: %s, which is not its condition; %q accepts only when: %s", name, e.When, name, want),
			}
		}
	}
	return nil
}

// formatKnownActions renders the known class names for the extension-class
// rejection message, in the canonical order.
func formatKnownActions() string {
	out := ""
	for i, name := range knownActionOrder {
		if i > 0 {
			out += ", "
		}
		out += name
	}
	return out
}
