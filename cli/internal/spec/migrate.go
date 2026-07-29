package spec

// workflow-v1 -> workflow-v2 migration codemod (E52.8 / #2220).
//
// MigrateBytes translates a workflow-v1 document into the workflow-v2
// grammar E52.2-E52.10 landed, by editing yaml.v3 NODES rather than
// round-tripping through a typed struct — the approach preset.go's
// Generate already proves — so comments and key ordering survive the
// rewrite. A governance file is read by humans; a codemod that strips
// its rationale comments has destroyed the thing being migrated.
//
// FIXED ORDER OF OPERATIONS, and each step is load-bearing:
//
//	1. parse            — a document that is not YAML is a *ParseError.
//	2. version routing  — major 2 is a NO-OP (nothing to migrate is not
//	                      an error); major 0 REFUSES (R9); anything else
//	                      falls through to step 3.
//	3. R0 source gate   — the SOURCE must pass ValidateBytes. Migrating an
//	                      already-invalid document produces garbage, and
//	                      the schema subsumes the impossibilities this
//	                      file would otherwise re-check by hand (a gate
//	                      declaring both approvers and approvals, a role
//	                      with an empty members list).
//	4. analyze          — a READ-ONLY pass that resolves roles, computes
//	                      each approval gate's before/after predicate, and
//	                      collects every refusal. It runs before any
//	                      mutation so the eligibility report is emitted on
//	                      the REFUSAL path too, not just on success.
//	5. transform        — node edits, using step 4's computed predicates.
//	6. R7 output gate   — the emitted document must pass ValidateBytes.
//	                      Fail closed: no bytes are returned.
//
// WHAT IS NEVER FABRICATED. `limit_usd`, `min_permission`, `member_of`
// and `not:` are never invented. The legacy grammar carries no source for
// any of them, and there is no token-to-USD rate anywhere in this repo —
// a guessed number in a governance file is worse than an absent one. The
// eligibility report says so per gate, including the WIDENING note when a
// migrated gate emits no `not:` exclusion.
//
// REFUSALS. Ten named branches, each aborting the WHOLE migration: a
// non-empty Refusals list means no bytes are produced under any flag.
// Every branch names a legacy form that has no faithful v2 equivalent —
// refusing is the point, because a guessed translation of an approval
// predicate is an authorization change nobody reviewed.

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// The ten refusal branches. Each is a stable identifier so a caller (and
// a test) can assert WHICH branch fired, not merely that something did.
const (
	// RefusalSourceInvalid (R0): the source document does not pass
	// ValidateBytes against its own major's schema.
	RefusalSourceInvalid = "R0_source_invalid"
	// RefusalAllOfMultiMemberRole (R1): `all_of` names a role that
	// resolves to more than one member. v2's approvals predicate carries
	// no per-role quorum — its only cardinality field is one
	// document-wide `count` over a flat `members` list — so "one member
	// from EACH of two multi-member roles" is inexpressible.
	RefusalAllOfMultiMemberRole = "R1_all_of_multi_member_role"
	// RefusalUndefinedRole (R2): an approvers form names a role the
	// `roles` map does not define.
	RefusalUndefinedRole = "R2_undefined_role"
	// RefusalTeamMemberRef (R3): a role member is an @org/team reference.
	// v2 `members` is a list of per-subject identifiers and `member_of`
	// is a SINGLE group string, so neither has a faithful encoding.
	RefusalTeamMemberRef = "R3_team_member_ref"
	// RefusalReviewersAgentCount (R4): the bare `reviewers.agent: N`
	// count v2 removed. Each agents[] entry REQUIRES a provider and a
	// bare integer carries none.
	RefusalReviewersAgentCount = "R4_reviewers_agent_count"
	// RefusalDuplicateConstraint (R5): a constraints LIST declares the
	// same kind twice. An object cannot hold a duplicate key, and
	// dropping one would silently discard a declared constraint.
	RefusalDuplicateConstraint = "R5_duplicate_constraint_kind"
	// RefusalUnknownPageEvent (R6): a must_page_human token outside v2's
	// page_human_on enum after the reviewer_reject rewrite.
	RefusalUnknownPageEvent = "R6_unknown_page_event"
	// RefusalOutputInvalid (R7): the emitted document fails ValidateBytes.
	RefusalOutputInvalid = "R7_output_invalid"
	// RefusalUnknownDelegationKnob (R8): an operator_agent knob, condition
	// value, or model_policy field the v2 action grammar does not carry.
	RefusalUnknownDelegationKnob = "R8_unknown_delegation_knob"
	// RefusalSourceVersionMajor0 (R9): a version-major-0 source. Routing
	// source validation through the v0 schema is a statement about what
	// PARSES, not about what is faithfully translatable — the v0 and v1
	// constraint folds diverge — so major 1 is the accepted input and a
	// v0 path is added deliberately if anyone asks for it.
	RefusalSourceVersionMajor0 = "R9_source_version_major_0"
)

// Refusal is one named reason the migration cannot proceed. Branch is one
// of the R0-R9 constants; Path is the JSON-pointer-ish location of the
// offending construct; Message names it and says why no faithful
// translation exists.
type Refusal struct {
	Branch  string
	Path    string
	Message string
}

func (r Refusal) String() string {
	if r.Path == "" {
		return fmt.Sprintf("[%s] %s", r.Branch, r.Message)
	}
	return fmt.Sprintf("[%s] %s: %s", r.Branch, r.Path, r.Message)
}

// MigrationResult is MigrateBytes' whole output.
type MigrationResult struct {
	// Migrated is the workflow-v2 document. Empty when NoOp is true or
	// Refusals is non-empty — those paths produce no bytes at all.
	Migrated []byte
	// Report is the approval-eligibility report. ALWAYS non-nil,
	// including on the no-op and refusal paths: the operator's decision
	// is about who can approve what, so the report is the product even
	// when the bytes are not.
	Report *EligibilityReport
	// Refusals is the named reasons the migration stopped. Non-empty
	// means Migrated is empty.
	Refusals []Refusal
	// NoOp is true when the source is already at version major 2.
	NoOp bool
}

// Refused reports whether any refusal branch fired.
func (r *MigrationResult) Refused() bool { return len(r.Refusals) > 0 }

// v2PageEvents is workflow-v2's page_human_on enum. The bare
// reviewer_reject token v0/v1 accept is deliberately ABSENT: it is
// rewritten to gating_reviewer_reject (the sense v0/v1 always resolved it
// to) before this set is consulted.
var v2PageEvents = map[string]bool{
	"advisory_reviewer_reject": true,
	"gating_reviewer_reject":   true,
	"plan_rejection":           true,
	"scope_amendment":          true,
	"budget_override":          true,
	"policy_override":          true,
	"exception_request":        true,
	"requirement_arbitration":  true,
	"clarification_request":    true,
}

// v2ModelPolicyFields is workflow-v2's model_policy property set. The
// block is carried forward verbatim; this is the R8 gate on it.
var v2ModelPolicyFields = map[string]bool{
	"strategy": true,
	"defaults": true,
	"allowed":  true,
}

// knobClass binds one v0/v1 operator_agent may_* knob to the v2 action
// class it becomes and to that class's single backend-evaluable
// condition. The v1 schema pins each knob's value enum to exactly this
// condition, so a knob carrying anything else is an R8 refusal rather
// than a `when` the backend could never answer.
type knobClass struct {
	Knob      string
	Class     string
	Condition string
}

// knobClasses is the translation table, in the canonical class order the
// emitted `actions` matrix uses (matching the backend's knownActionOrder,
// so a migrated document and a hand-written one read the same).
var knobClasses = []knobClass{
	{Knob: "may_approve", Class: "approve", Condition: "clean_dual_approval"},
	{Knob: "may_route_fixup", Class: "fixup", Condition: "convergent_concerns"},
	{Knob: "may_waive", Class: "waive", Condition: "solo_low"},
	{Knob: "may_retry", Class: "retry", Condition: "infra_flake"},
	{Knob: "may_merge", Class: "merge", Condition: "gates_resolved_ci_green"},
}

// legacyReviewerRejectToken is the bare page event v2 removed; it is
// rewritten to gatingReviewerRejectToken, the class v0/v1 always resolved
// it to.
const (
	legacyReviewerRejectToken = "reviewer_reject"
	gatingReviewerRejectToken = "gating_reviewer_reject"
)

// tierPageEvents is the seven-event page list the medium and high tiers
// expand to (workflow-v2 $defs/autonomy_tier). The low tier expands to
// none — with nothing delegated there is nothing to except.
var tierPageEvents = []string{
	gatingReviewerRejectToken,
	"plan_rejection",
	"scope_amendment",
	"budget_override",
	"policy_override",
	"exception_request",
	"requirement_arbitration",
}

// postTranslateHook is a TEST-ONLY seam. R7 — the output-validation gate —
// is unreachable from any schema-valid v1 source by construction, which
// is exactly why it must still be PROVEN: it is the last line between a
// codemod bug and a governance file that no longer validates. A test sets
// this to corrupt the translated tree, then asserts the gate refuses and
// returns no bytes. Nil in production; never set outside a test.
var postTranslateHook func(root *yaml.Node)

// MigrateBytes translates a workflow-v1 document into workflow-v2.
//
// It returns a *ParseError when src is empty or not a YAML mapping
// document. Every other failure mode is a REFUSAL carried on the result
// (see the R0-R9 constants), not a Go error: a refusal is an answer about
// the document, and the caller still wants the eligibility report that
// comes with it. The result's Report is always non-nil.
func MigrateBytes(src []byte) (*MigrationResult, error) {
	if len(bytes.TrimSpace(src)) == 0 {
		return nil, &ParseError{Msg: "empty document"}
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return nil, &ParseError{Msg: err.Error()}
	}
	root := documentRoot(&doc)
	if root == nil {
		return nil, &ParseError{Msg: "document is not a mapping"}
	}

	// Step 2: version routing. Already-v2 is a no-op, not an error;
	// major 0 refuses outright (R9).
	if major, ok := sourceVersionMajor(root); ok {
		switch major {
		case 2:
			return &MigrationResult{NoOp: true, Report: reportForV2(root)}, nil
		case 0:
			return &MigrationResult{
				Report: reportForV2(root),
				Refusals: []Refusal{{
					Branch: RefusalSourceVersionMajor0,
					Path:   "/version",
					Message: "workflow-v0 source documents are not migratable: this codemod accepts version major 1 only. " +
						"The v0 and v1 grammars diverge (notably the constraint fold), so a v0 document would be MISTRANSLATED rather than migrated. " +
						"Migrate the document to workflow-v1 first, or open an issue asking for a v0 path.",
				}},
			}, nil
		}
	}

	// Step 3 (R0): the source must validate against its own major's
	// schema before anything is translated.
	if err := ValidateBytes(src); err != nil {
		return &MigrationResult{
			Report: reportForV2(root),
			Refusals: []Refusal{{
				Branch:  RefusalSourceInvalid,
				Path:    "/",
				Message: "source document does not validate against its own workflow schema; migrate only a valid document:\n" + err.Error(),
			}},
		}, nil
	}

	// Step 4: read-only analysis — roles, per-gate predicates, refusals.
	an := analyzeSource(root)
	result := &MigrationResult{Report: an.report, Refusals: an.refusals}
	if result.Refused() {
		return result, nil
	}

	// Step 5: the node edits.
	transform(root, an)
	if postTranslateHook != nil {
		postTranslateHook(root)
	}

	out, err := encodeDocument(&doc)
	if err != nil {
		return nil, err
	}

	// Step 6 (R7): fail closed on an output that no longer validates.
	if verr := ValidateBytes(out); verr != nil {
		result.Refusals = append(result.Refusals, Refusal{
			Branch:  RefusalOutputInvalid,
			Path:    "/",
			Message: "the migrated document does not validate against the workflow-v2 schema; nothing was written:\n" + verr.Error(),
		})
		return result, nil
	}
	result.Migrated = out
	return result, nil
}

// encodeDocument re-encodes an edited node tree at the canonical 2-space
// indent the shipped specs use.
func encodeDocument(doc *yaml.Node) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return nil, fmt.Errorf("spec: re-encode migrated document: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("spec: close encoder for migrated document: %w", err)
	}
	return buf.Bytes(), nil
}

// sourceVersionMajor reads the document's version major. ok is false when
// the key is absent or its value does not parse as an integer major — the
// R0 gate reports that case with the schema's own required-version error
// rather than a bespoke one.
func sourceVersionMajor(root *yaml.Node) (int, bool) {
	v := mapValue(root, "version")
	if v == nil || v.Kind != yaml.ScalarNode {
		return 0, false
	}
	part := v.Value
	if idx := strings.IndexByte(part, '.'); idx >= 0 {
		part = part[:idx]
	}
	major, err := strconv.Atoi(part)
	if err != nil {
		return 0, false
	}
	return major, true
}

// approvalPredicate is a gate's approval predicate in plain Go, shared by
// the analysis pass (which renders it into the eligibility report) and
// the transform pass (which emits it as nodes). Keeping ONE computation
// is what makes the report and the emitted document provably agree.
type approvalPredicate struct {
	Count int
	// Members are @-stripped per-subject identifiers.
	Members []string
	// MemberOf and MinPermission are only ever populated by READING an
	// already-v2 `approvals` block. The codemod never synthesizes them —
	// the legacy grammar carries no source for either.
	MemberOf      string
	MinPermission string
	Not           []string
}

// analysis is the read-only pass's output.
type analysis struct {
	report   *EligibilityReport
	refusals []Refusal
	// gateApprovals maps a gate node to the predicate its legacy
	// `approvers` block translates to. A gate already carrying
	// `approvals` is absent from this map — it is carried through
	// verbatim.
	gateApprovals map[*yaml.Node]approvalPredicate
	// autonomy maps a workflow or gate node to the delegation block its
	// `operator_agent` translates to.
	autonomy map[*yaml.Node]*delegationBlock
}

func (a *analysis) refuse(branch, path, msg string) {
	a.refusals = append(a.refusals, Refusal{Branch: branch, Path: path, Message: msg})
}

// analyzeSource walks the source document without mutating it, resolving
// roles, computing every gate's predicate, translating every
// operator_agent block, and collecting refusals. It builds the
// eligibility report as it goes, so the report survives a refusal.
func analyzeSource(root *yaml.Node) *analysis {
	an := &analysis{
		report:        &EligibilityReport{},
		gateApprovals: map[*yaml.Node]approvalPredicate{},
		autonomy:      map[*yaml.Node]*delegationBlock{},
	}

	roles := an.collectRoles(root)

	workflows := mappingNode(mapValue(root, "workflows"))
	for _, pair := range mapPairs(workflows) {
		wfName, wf := pair[0].Value, mappingNode(pair[1])
		if wf == nil {
			continue
		}
		wfPath := "/workflows/" + wfName
		an.analyzeDelegation(wf, wfPath)

		stages := mapValue(wf, "stages")
		if stages == nil || stages.Kind != yaml.SequenceNode {
			continue
		}
		for i, stRaw := range stages.Content {
			st := mappingNode(stRaw)
			if st == nil {
				continue
			}
			stPath := fmt.Sprintf("%s/stages/%d", wfPath, i)
			an.analyzeStage(st, stPath, roles)
		}
	}
	return an
}

// collectRoles resolves the top-level roles map to @-stripped member
// lists, refusing (R3) on any team reference.
func (an *analysis) collectRoles(root *yaml.Node) map[string][]string {
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
				an.refuse(RefusalTeamMemberRef, "/roles/"+name,
					fmt.Sprintf("role %q names the TEAM reference %q, which has no faithful workflow-v2 encoding: "+
						"v2 `approvals.members` is a list of per-subject identifiers and `approvals.member_of` is a SINGLE group string, "+
						"so a team (and a role mixing users and teams) cannot be expressed as either. "+
						"Rewrite the gate by hand as approvals: {count: N, member_of: \"%s\"} or enumerate the team's members.", name, m.Value, ref))
				continue
			}
			resolved = append(resolved, ref)
		}
		out[name] = resolved
	}
	return out
}

// analyzeStage collects the stage-level refusals (R4, R5) and walks its
// gates.
func (an *analysis) analyzeStage(st *yaml.Node, stPath string, roles map[string][]string) {
	stageID := ""
	if id := mapValue(st, "id"); id != nil {
		stageID = id.Value
	}

	// R4: the bare reviewers.agent count.
	if reviewers := mappingNode(mapValue(st, "reviewers")); reviewers != nil {
		if agent := mapValue(reviewers, "agent"); agent != nil {
			an.refuse(RefusalReviewersAgentCount, stPath+"/reviewers/agent",
				fmt.Sprintf("stage %q declares the bare reviewer count `agent: %s`, which workflow-v2 removed and this codemod cannot translate: "+
					"each v2 `reviewers.agents[]` entry REQUIRES a `provider`, and a bare integer carries none. "+
					"Declare the providers explicitly, e.g. reviewers: {agents: [{provider: claudecode}], human: 1}.", stageID, agent.Value))
		}
	}

	// R5: a constraints LIST declaring the same kind twice.
	if constraints := mapValue(st, "constraints"); constraints != nil && constraints.Kind == yaml.SequenceNode {
		seen := map[string]bool{}
		for _, entry := range constraints.Content {
			for _, pair := range mapPairs(mappingNode(entry)) {
				kind := pair[0].Value
				if seen[kind] {
					an.refuse(RefusalDuplicateConstraint, stPath+"/constraints",
						fmt.Sprintf("stage %q declares the constraint kind %q more than once. "+
							"workflow-v2 spells constraints as ONE object, whose keys are unique, so the two declarations cannot both survive — "+
							"and dropping one would silently discard a declared constraint. Merge them by hand into a single value first.", stageID, kind))
				}
				seen[kind] = true
			}
		}
	}

	gates := mapValue(st, "gates")
	if gates == nil || gates.Kind != yaml.SequenceNode {
		return
	}
	for j, gRaw := range gates.Content {
		gate := mappingNode(gRaw)
		if gate == nil {
			continue
		}
		gatePath := fmt.Sprintf("%s/gates/%d", stPath, j)
		an.analyzeDelegation(gate, gatePath)
		an.analyzeGate(gate, gatePath, stageID, roles)
	}
}

// analyzeGate computes one approval gate's before/after predicate and
// appends its eligibility block. A check gate carries no predicate and is
// skipped.
func (an *analysis) analyzeGate(gate *yaml.Node, gatePath, stageID string, roles map[string][]string) {
	typ := mapValue(gate, "type")
	if typ == nil || typ.Value != "approval" {
		return
	}

	// A gate already carrying `approvals` is carried through verbatim —
	// and still reported, because the operator's question is about every
	// gate's eligible set, not only the ones that changed.
	if existing := mappingNode(mapValue(gate, "approvals")); existing != nil {
		p := readApprovals(existing)
		an.report.Gates = append(an.report.Gates, GateEligibility{
			Path:       gatePath,
			StageID:    stageID,
			Before:     renderPredicate(p),
			After:      renderPredicate(p),
			Enumerable: p.Members,
			Unresolved: unresolvedOf(p),
			Change:     changeNone,
		})
		return
	}

	approvers := mappingNode(mapValue(gate, "approvers"))
	if approvers == nil {
		return
	}
	pred, before, ref := translateApprovers(approvers, roles, gatePath)
	if ref != nil {
		an.refusals = append(an.refusals, *ref)
		an.report.Gates = append(an.report.Gates, GateEligibility{
			Path:    gatePath,
			StageID: stageID,
			Before:  before,
			After:   "(refused — see the refusal above; nothing was written)",
			Refused: true,
			Change:  changeRefused,
		})
		return
	}
	an.gateApprovals[gate] = pred
	an.report.Gates = append(an.report.Gates, GateEligibility{
		Path:       gatePath,
		StageID:    stageID,
		Before:     before,
		After:      renderPredicate(pred),
		Enumerable: pred.Members,
		Unresolved: unresolvedOf(pred),
		Change:     changeWidenedNoExclusion,
	})
}

// translateApprovers turns a legacy `approvers` block into a v2 approvals
// predicate, or returns the refusal explaining why no faithful
// translation exists. The second return is the rendering of the SOURCE
// predicate for the report.
//
// any_of means "one approval from anyone in the union", which is exactly
// approvals: {count: 1, members: <union>}.
//
// all_of means "one approver from EACH named role". v2 has no per-role
// quorum, so this translates only when every named role resolves to
// exactly one member (R1 otherwise). `count` is then the number of
// DISTINCT members in the deduplicated union, NOT the number of named
// roles: two singleton roles naming the SAME person are one human, and
// emitting count 2 over a one-member list would ship a gate that can
// never clear. Counting distinct members is faithful in both sub-cases —
// disjoint singletons still yield count = N, overlapping ones collapse.
func translateApprovers(approvers *yaml.Node, roles map[string][]string, gatePath string) (approvalPredicate, string, *Refusal) {
	form, names := approversForm(approvers)
	before := fmt.Sprintf("approvers: {%s: [%s]}", form, strings.Join(names, ", "))

	var union []string
	seen := map[string]bool{}
	for _, name := range names {
		members, ok := roles[name]
		if !ok {
			return approvalPredicate{}, before, &Refusal{
				Branch: RefusalUndefinedRole,
				Path:   gatePath + "/approvers/" + form,
				Message: fmt.Sprintf("the gate names the role %q, which the document's top-level `roles` map does not define. "+
					"There is nothing to resolve the approver set from, and workflow-v2 removed the roles map entirely, "+
					"so define the role first or write the gate's approvals block by hand.", name),
			}
		}
		if form == "all_of" && len(members) != 1 {
			return approvalPredicate{}, before, &Refusal{
				Branch: RefusalAllOfMultiMemberRole,
				Path:   gatePath + "/approvers/all_of",
				Message: fmt.Sprintf("`all_of` names the role %q, which resolves to %d members. "+
					"`all_of` means one approval from EACH named role, and workflow-v2's approvals predicate has NO per-role quorum — "+
					"its only cardinality field is a single document-wide `count` over a flat `members` list. "+
					"Neither candidate encoding preserves the meaning (count = number of roles lets two members of the SAME role satisfy it; "+
					"count = size of the union demands every listed human), so there is no faithful translation. "+
					"Write this gate's approvals block by hand.", name, len(members)),
			}
		}
		for _, m := range members {
			if seen[m] {
				continue
			}
			seen[m] = true
			union = append(union, m)
		}
	}

	count := 1
	if form == "all_of" {
		count = len(union)
	}
	return approvalPredicate{Count: count, Members: union}, before, nil
}

// approversForm reports which of any_of / all_of a legacy approvers block
// declares and the role names it lists. The v0/v1 schema pins the block
// to exactly one of the two keys.
func approversForm(approvers *yaml.Node) (string, []string) {
	for _, form := range []string{"any_of", "all_of"} {
		seq := mapValue(approvers, form)
		if seq == nil || seq.Kind != yaml.SequenceNode {
			continue
		}
		names := make([]string, 0, len(seq.Content))
		for _, n := range seq.Content {
			names = append(names, n.Value)
		}
		return form, names
	}
	return "any_of", nil
}

// readApprovals decodes an already-v2 approvals block into the shared
// predicate shape so the report can render it identically.
func readApprovals(node *yaml.Node) approvalPredicate {
	p := approvalPredicate{}
	if c := mapValue(node, "count"); c != nil {
		p.Count, _ = strconv.Atoi(c.Value)
	}
	if m := mapValue(node, "members"); m != nil && m.Kind == yaml.SequenceNode {
		for _, n := range m.Content {
			p.Members = append(p.Members, n.Value)
		}
	}
	if g := mapValue(node, "member_of"); g != nil {
		p.MemberOf = g.Value
	}
	if mp := mapValue(node, "min_permission"); mp != nil {
		p.MinPermission = mp.Value
	}
	if n := mapValue(node, "not"); n != nil && n.Kind == yaml.SequenceNode {
		for _, e := range n.Content {
			p.Not = append(p.Not, e.Value)
		}
	}
	return p
}

// delegationBlock is a translated operator_agent block: either an exact
// tier match (Tier non-empty) or the explicit action matrix.
type delegationBlock struct {
	// Tier is non-empty ONLY on an exact match against a tier's full
	// expansion — all five classes AND the page list. Any partial match
	// emits the explicit matrix instead, because rounding to a tier would
	// widen or narrow agent authority without the operator noticing.
	Tier string
	// Entries are the classes the source actually declared, in canonical
	// order. An ABSENT knob is omitted entirely: absence already resolves
	// to the fail-closed `mode: gated`.
	Entries []actionEntry
	// PageEvents is the rewritten must_page_human list (nil when the
	// source declared none).
	PageEvents []string
	// ModelPolicy is the source's model_policy node, carried verbatim.
	ModelPolicy *yaml.Node
}

// actionEntry is one action class's translated setting.
type actionEntry struct {
	Class       string
	Mode        string
	When        string
	MinSeverity string
}

// analyzeDelegation translates a level's operator_agent block, recording
// the result against that level's node. An ABSENT block records NOTHING:
// it is deliberately not rounded to `autonomy: low`, which would
// additionally assert an empty page list the source never declared.
func (an *analysis) analyzeDelegation(level *yaml.Node, path string) {
	oa := mappingNode(mapValue(level, "operator_agent"))
	if oa == nil {
		return
	}
	block, refusals := translateOperatorAgent(oa, path+"/operator_agent")
	an.refusals = append(an.refusals, refusals...)
	if len(refusals) == 0 {
		an.autonomy[level] = block
	}
}

// translateOperatorAgent maps each v0/v1 delegation knob onto its v2
// action class, rewrites the page list, and collapses the result to a
// tier shorthand only on an EXACT match.
func translateOperatorAgent(oa *yaml.Node, path string) (*delegationBlock, []Refusal) {
	var refusals []Refusal
	block := &delegationBlock{}

	known := map[string]bool{"route_fixup_min_severity": true, "must_page_human": true, "model_policy": true}
	for _, kc := range knobClasses {
		known[kc.Knob] = true
	}
	for _, pair := range mapPairs(oa) {
		if !known[pair[0].Value] {
			refusals = append(refusals, Refusal{
				Branch: RefusalUnknownDelegationKnob,
				Path:   path + "/" + pair[0].Value,
				Message: fmt.Sprintf("operator_agent knob %q has no workflow-v2 action class: the v2 grammar carries the five classes "+
					"approve/fixup/waive/retry/merge plus the reserved page_human_on and model_policy keys. Translate it by hand.", pair[0].Value),
			})
		}
	}

	minSeverity := ""
	if ms := mapValue(oa, "route_fixup_min_severity"); ms != nil {
		minSeverity = ms.Value
	}
	for _, kc := range knobClasses {
		knob := mapValue(oa, kc.Knob)
		if knob == nil {
			continue // absence already resolves to the fail-closed `gated`
		}
		if knob.Value != kc.Condition {
			refusals = append(refusals, Refusal{
				Branch: RefusalUnknownDelegationKnob,
				Path:   path + "/" + kc.Knob,
				Message: fmt.Sprintf("knob %s declares the condition %q, but workflow-v2 binds the %q action class to exactly one "+
					"backend-evaluable condition (%q). A `when` the backend cannot answer would make the entry unreachable, so this is not translated.",
					kc.Knob, knob.Value, kc.Class, kc.Condition),
			})
			continue
		}
		entry := actionEntry{Class: kc.Class, Mode: "auto", When: kc.Condition}
		if kc.Class == "fixup" {
			entry.MinSeverity = minSeverity
		}
		block.Entries = append(block.Entries, entry)
	}

	if events := mapValue(oa, "must_page_human"); events != nil && events.Kind == yaml.SequenceNode {
		block.PageEvents = make([]string, 0, len(events.Content))
		for i, ev := range events.Content {
			token, ok := translatePageEvent(ev.Value)
			if !ok {
				refusals = append(refusals, Refusal{
					Branch: RefusalUnknownPageEvent,
					Path:   fmt.Sprintf("%s/must_page_human/%d", path, i),
					Message: fmt.Sprintf("page event %q is not a member of workflow-v2's page_human_on enum (after the reviewer_reject rewrite). "+
						"Translate it by hand or drop it.", ev.Value),
				})
				continue
			}
			block.PageEvents = append(block.PageEvents, token)
		}
	}

	if mp := mappingNode(mapValue(oa, "model_policy")); mp != nil {
		for _, pair := range mapPairs(mp) {
			if !v2ModelPolicyFields[pair[0].Value] {
				refusals = append(refusals, Refusal{
					Branch:  RefusalUnknownDelegationKnob,
					Path:    path + "/model_policy/" + pair[0].Value,
					Message: fmt.Sprintf("model_policy field %q is not carried by workflow-v2's model_policy block.", pair[0].Value),
				})
			}
		}
		block.ModelPolicy = mp
	}

	if len(refusals) > 0 {
		return nil, refusals
	}
	block.Tier = matchTier(block)
	return block, nil
}

// translatePageEvent rewrites the bare reviewer_reject token to the
// gating spelling v0/v1 always resolved it to, and reports whether the
// result is a member of v2's page_human_on enum.
func translatePageEvent(token string) (string, bool) {
	if token == legacyReviewerRejectToken {
		token = gatingReviewerRejectToken
	}
	return token, v2PageEvents[token]
}

// matchTier returns the tier shorthand a translated block collapses to,
// or "" when it does not match one EXACTLY.
//
// The comparison is over the RESOLVED matrix (every unlisted class reads
// as `gated`) plus the page list as a SET. Any partial match returns ""
// so the explicit matrix is emitted instead: rounding a
// may_approve-ONLY block up to `autonomy: medium` would silently hand the
// operator agent fixup and retry authority the source never granted.
func matchTier(block *delegationBlock) string {
	if block.ModelPolicy != nil {
		return "" // no tier expands to a model_policy
	}
	resolved := map[string]actionEntry{}
	for _, kc := range knobClasses {
		resolved[kc.Class] = actionEntry{Class: kc.Class, Mode: "gated"}
	}
	for _, e := range block.Entries {
		if e.MinSeverity != "" {
			return "" // no tier expands to a min_severity
		}
		resolved[e.Class] = e
	}
	for _, tier := range []string{"low", "medium", "high"} {
		want, wantPages := tierExpansion(tier)
		if !sameMatrix(resolved, want) || !sameSet(block.PageEvents, wantPages) {
			continue
		}
		return tier
	}
	return ""
}

// tierExpansion is the CLI-side copy of workflow-v2 $defs/autonomy_tier's
// three expansions, kept in the vocabulary the schema declares (the page
// list uses gating_reviewer_reject, never the bare token v2 removed).
func tierExpansion(tier string) (map[string]actionEntry, []string) {
	gated := func() map[string]actionEntry {
		m := map[string]actionEntry{}
		for _, kc := range knobClasses {
			m[kc.Class] = actionEntry{Class: kc.Class, Mode: "gated"}
		}
		return m
	}
	auto := func(m map[string]actionEntry, classes ...string) map[string]actionEntry {
		for _, c := range classes {
			for _, kc := range knobClasses {
				if kc.Class == c {
					m[c] = actionEntry{Class: c, Mode: "auto", When: kc.Condition}
				}
			}
		}
		return m
	}
	switch tier {
	case "low":
		// Nothing delegated, and no page list implied — with nothing
		// delegated there is nothing to except.
		return gated(), nil
	case "medium":
		return auto(gated(), "approve", "fixup", "retry"), tierPageEvents
	case "high":
		return auto(gated(), "approve", "fixup", "retry", "waive", "merge"), tierPageEvents
	}
	return nil, nil
}

func sameMatrix(a, b map[string]actionEntry) bool {
	if len(a) != len(b) {
		return false
	}
	for k, av := range a {
		if bv, ok := b[k]; !ok || av != bv {
			return false
		}
	}
	return true
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	set := make(map[string]bool, len(a))
	for _, s := range a {
		set[s] = true
	}
	for _, s := range b {
		if !set[s] {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------
// transform: the node edits.
// ---------------------------------------------------------------------

// transform applies every mechanical translation to the parsed tree,
// using the analysis pass's computed predicates and delegation blocks. It
// runs only when analysis produced no refusals.
func transform(root *yaml.Node, an *analysis) {
	// (a) version collapses to the single token "2" — v2 has no minor
	// chain, and its enum rejects the dotted forms.
	if v := mapValue(root, "version"); v != nil {
		v.Tag = "!!str"
		v.Value = "2"
	}

	workflows := mappingNode(mapValue(root, "workflows"))
	for _, pair := range mapPairs(workflows) {
		wf := mappingNode(pair[1])
		if wf == nil {
			continue
		}
		// (b) drive -> auto_advance, renamed IN PLACE so the key's
		// leading comment and its position both survive.
		renameKey(wf, "drive", "auto_advance")
		applyDelegation(wf, an)

		stages := mapValue(wf, "stages")
		if stages == nil || stages.Kind != yaml.SequenceNode {
			continue
		}
		for _, stRaw := range stages.Content {
			st := mappingNode(stRaw)
			if st == nil {
				continue
			}
			transformStage(st, an)
		}
	}

	// (f) the roles map is deleted LAST, once the approvals translation
	// has consumed it. v2's root is additionalProperties:false over
	// [version, workflows, defaults, test_conventions], so a retained
	// roles map would fail the R7 output gate.
	removeMapKey(root, "roles")
}

func transformStage(st *yaml.Node, an *analysis) {
	// (c) budget.max_runtime_minutes: N -> max_runtime: "Nm". max_tokens
	// is CARRIED FORWARD (v2 keeps it as an optional secondary lever) and
	// no limit_usd is fabricated: there is no token-to-USD rate in this
	// repo, so inventing one would be a guess.
	if budget := mappingNode(mapValue(st, "budget")); budget != nil {
		if v := mapValue(budget, "max_runtime_minutes"); v != nil {
			renameKey(budget, "max_runtime_minutes", "max_runtime")
			v.Tag = "!!str"
			v.Style = 0
			v.Value += "m"
		}
	}

	// (d) the constraints SEQUENCE of single-key maps folds into one v2
	// MAPPING, preserving declaration order and each entry's comments.
	foldConstraints(st)

	for _, gRaw := range valueSeq(st, "gates") {
		gate := mappingNode(gRaw)
		if gate == nil {
			continue
		}
		applyDelegation(gate, an)
		pred, ok := an.gateApprovals[gate]
		if !ok {
			continue
		}
		// (g) approvers -> approvals, renamed in place so the gate's key
		// order and any comment on the approvers key survive.
		renameKey(gate, "approvers", "approvals")
		replaceValue(gate, "approvals", approvalsNode(pred))
	}
}

// foldConstraints rewrites a stage's constraints LIST into the v2 OBJECT
// form in place. Each list entry is a single-key mapping; its key/value
// nodes are REUSED, so per-kind comments ride along. A head comment
// attached to the list ITEM (the common shape for a comment written above
// a `- kind:` entry) is moved onto the key it documents.
func foldConstraints(st *yaml.Node) {
	c := mapValue(st, "constraints")
	if c == nil || c.Kind != yaml.SequenceNode {
		return
	}
	var folded []*yaml.Node
	for _, entry := range c.Content {
		m := mappingNode(entry)
		if m == nil {
			continue
		}
		if m.HeadComment != "" && len(m.Content) > 0 && m.Content[0].HeadComment == "" {
			m.Content[0].HeadComment = m.HeadComment
		}
		folded = append(folded, m.Content...)
	}
	c.Kind = yaml.MappingNode
	c.Tag = "!!map"
	c.Style = 0
	c.Content = folded
}

// applyDelegation replaces a level's operator_agent block with either the
// `autonomy` tier shorthand or the explicit `actions` matrix, renaming in
// place so the block's leading comment and position survive.
func applyDelegation(level *yaml.Node, an *analysis) {
	block, ok := an.autonomy[level]
	if !ok {
		return
	}
	if block.Tier != "" {
		renameKey(level, "operator_agent", "autonomy")
		replaceValue(level, "autonomy", scalar(block.Tier))
		return
	}
	renameKey(level, "operator_agent", "actions")
	replaceValue(level, "actions", actionsNode(block))
}

// actionsNode renders a delegation block as the explicit v2 `actions`
// matrix: the declared classes in canonical order, then the reserved
// page_human_on and model_policy keys.
func actionsNode(block *delegationBlock) *yaml.Node {
	out := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	for _, e := range block.Entries {
		entry := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		entry.Content = append(entry.Content, scalar("mode"), scalar(e.Mode))
		if e.When != "" {
			entry.Content = append(entry.Content, scalar("when"), scalar(e.When))
		}
		if e.MinSeverity != "" {
			entry.Content = append(entry.Content, scalar("min_severity"), scalar(e.MinSeverity))
		}
		out.Content = append(out.Content, scalar(e.Class), entry)
	}
	if len(block.PageEvents) > 0 {
		seq := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		for _, ev := range block.PageEvents {
			seq.Content = append(seq.Content, scalar(ev))
		}
		out.Content = append(out.Content, scalar("page_human_on"), seq)
	}
	if block.ModelPolicy != nil {
		out.Content = append(out.Content, scalar("model_policy"), block.ModelPolicy)
	}
	return out
}

// approvalsNode renders a translated predicate as the v2 approvals block.
// It emits `count` and `members` ONLY: min_permission, member_of and
// `not` have no source in the legacy grammar and are never fabricated.
func approvalsNode(p approvalPredicate) *yaml.Node {
	out := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	count := scalar(strconv.Itoa(p.Count))
	count.Tag = "!!int"
	out.Content = append(out.Content, scalar("count"), count)
	if len(p.Members) > 0 {
		seq := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Style: yaml.FlowStyle}
		for _, m := range p.Members {
			seq.Content = append(seq.Content, scalar(m))
		}
		out.Content = append(out.Content, scalar("members"), seq)
	}
	return out
}

// ---------------------------------------------------------------------
// small node helpers
// ---------------------------------------------------------------------

func scalar(v string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v}
}

// mapPairs returns a mapping node's [key, value] pairs in document order.
func mapPairs(m *yaml.Node) [][2]*yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	out := make([][2]*yaml.Node, 0, len(m.Content)/2)
	for i := 0; i+1 < len(m.Content); i += 2 {
		out = append(out, [2]*yaml.Node{m.Content[i], m.Content[i+1]})
	}
	return out
}

// valueSeq returns the sequence entries of a mapping's key, or nil.
func valueSeq(m *yaml.Node, key string) []*yaml.Node {
	v := mapValue(m, key)
	if v == nil || v.Kind != yaml.SequenceNode {
		return nil
	}
	return v.Content
}

// renameKey rewrites a mapping key's scalar value in place, preserving
// its position in the mapping and every comment attached to it.
func renameKey(m *yaml.Node, from, to string) {
	if m == nil || m.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == from {
			m.Content[i].Value = to
			return
		}
	}
}

// replaceValue swaps a mapping key's value node, carrying the old value's
// comments onto the replacement so a rationale comment written on the
// block survives the rewrite.
func replaceValue(m *yaml.Node, key string, val *yaml.Node) {
	if m == nil || m.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			old := m.Content[i+1]
			val.HeadComment, val.LineComment, val.FootComment = old.HeadComment, old.LineComment, old.FootComment
			m.Content[i+1] = val
			return
		}
	}
}
