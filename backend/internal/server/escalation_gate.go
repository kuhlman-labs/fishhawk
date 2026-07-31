package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/appliesto"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/delegation"
	"github.com/kuhlman-labs/fishhawk/backend/internal/escalation"
	"github.com/kuhlman-labs/fishhawk/backend/internal/identity"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// Per-path escalation enforcement — THE server seam (E53.4 / #2227).
//
// A workflow's `escalations` block declares `{match: <predicate>, require:
// {...}}` rules that RAISE requirements for a change the predicate matches.
// The declaration side (grammar, composition, only-ever-raise validation, the
// pure clamp) lives in backend/internal/spec; the pure firing walk lives in
// backend/internal/escalation. THIS file is where the run's change is BUILT
// and the firing decision is turned into enforcement.
//
// ONE RESOLVER, TWO SEAMS, ONE AUDIT EMIT POINT. Both consumers —
// approveStageAs / checkApprovalPredicates on the approval gate, and
// delegation.Evaluate on the delegation gate — call resolveEscalations. The
// `escalation_fired` audit entry is written HERE, inside it, rather than at
// each consumer, which is what makes the "audited at every firing seam"
// guarantee structural: a `max_autonomy`-only escalation on a workflow with NO
// approval gate — a rule that changes behaviour purely through the delegation
// clamp — is audited exactly as an approvals escalation is, and a future third
// consumer inherits the audit rather than having to remember it.
//
// NO TWO-PHASE SPLIT, unlike applies_to. That file defers `paths` to the plan
// gate because no authoritative path set exists at admission. Escalations are
// evaluated only at GATE time, by which point all three producers exist — the
// approved plan's scope union, the run's immutable issue-label snapshot and its
// trigger source — so the full predicate is evaluated in ONE pass. That is a
// deliberate difference, not an inconsistency.
//
// FAIL CLOSED on both named modes. An unreadable/absent plan while a
// paths-bearing escalation is declared, and a Match error, each return an error
// the caller turns into a refusal (a retryable 503 at the approval gate, an
// omitted delegation surface at the delegation seam) rather than proceeding
// UNESCALATED. Proceeding unescalated is indistinguishable from the control
// being absent, which is the whole failure this gate exists to prevent.

// CategoryEscalationFired is the audit category recording that one or more
// escalations fired for a run's change at a gate. Registered in
// audit.KnownCategories so an operator can await it
// (categories_completeness_test.go's AST sweep fails the build otherwise).
const CategoryEscalationFired = "escalation_fired"

// errEscalationPlanUnreadable is the fail-closed sentinel for "a paths-bearing
// escalation is declared but this run's approved plan could not be read". It
// is a sentinel so the approval gate can name the mode in its 503 without
// string-matching.
var errEscalationPlanUnreadable = errors.New("escalations declare a paths criterion but the run's approved plan could not be read")

// escalationResolverFor adapts *Server to delegation.EscalationResolver. The
// adapter is a distinct type rather than a method set on *Server so the
// delegation package's required-at-construction contract is satisfied by an
// explicit, greppable wiring expression at each of the three construction
// sites.
type escalationResolverFor struct{ s *Server }

// escalationResolver returns the resolver every delegation.NewEvaluator call
// site in this package passes. It is never nil: NewEvaluator rejects a nil
// resolver, and the no-escalations case is answered INSIDE resolveEscalations
// (which returns the zero requirement), so "inert" is a code path rather than
// an unset field.
func (s *Server) escalationResolver() delegation.EscalationResolver {
	return escalationResolverFor{s: s}
}

// ResolveEscalations implements delegation.EscalationResolver: it returns only
// the COMPOSED requirement, because the delegation seam consumes exactly one
// dimension of it (the `max_autonomy` ceiling). The firing detail and the
// audit emit are handled inside resolveEscalations, shared with the approval
// seam.
func (r escalationResolverFor) ResolveEscalations(ctx context.Context, runRow *run.Run, wf *spec.Workflow, stageID uuid.UUID) (spec.ComposedRequirements, error) {
	res, err := r.s.resolveEscalations(ctx, runRow, wf, stageID)
	if err != nil {
		return spec.ComposedRequirements{}, err
	}
	return res.Requirements, nil
}

// resolveEscalations evaluates the run's `escalations` declaration and returns
// what fired, emitting the single `escalation_fired` audit entry as a side
// effect.
//
// ZERO EXTRA READS FOR A WORKFLOW THAT DECLARES NONE: the short-circuit is the
// FIRST statement, before any repository call, so every run on a workflow with
// no escalations follows a byte-identical code path to today.
func (s *Server) resolveEscalations(ctx context.Context, runRow *run.Run, wf *spec.Workflow, stageID uuid.UUID) (escalation.Result, error) {
	if runRow == nil || wf == nil || len(wf.Escalations) == 0 {
		return escalation.Result{}, nil
	}

	change, err := s.escalationChange(ctx, runRow, wf.Escalations)
	if err != nil {
		return escalation.Result{}, err
	}
	res, err := escalation.Evaluate(wf.Escalations, change)
	if err != nil {
		// A Match error (a malformed glob that reached the gate) is a
		// declaration we cannot evaluate. Fail closed — see the file header.
		return escalation.Result{}, fmt.Errorf("evaluate escalations: %w", err)
	}
	if res.Any() {
		s.writeEscalationFiredAudit(ctx, runRow, stageID, res)
	}
	return res, nil
}

// escalationChange builds the spec.Change the declarations are matched
// against, from the three producers that all exist at gate time.
//
// PATHS are the UNION of the approved plan's top-level scope.files, every
// decomposition sub_plan scope and every split_proposal phase scope — the same
// union planGateScopePaths computes for applies_to (#2226) and scopedPaths
// computes for scope regression (#1257). The union, not the top-level list, is
// load-bearing: a decomposed run's fan-out child runs bounded to its SLICE
// scope, so checking only the top level would let a slice touch an escalated
// path without escalating.
//
// The plan is loaded ONLY when some declaration actually carries a `paths`
// criterion. That keeps the fail-closed branch honest — a label-only or
// trigger-only escalation set is not refused because the run happens to have
// no plan yet — while still refusing a paths-bearing set we cannot evaluate.
//
// LABELS come from the run row's IMMUTABLE IssueContext.Labels snapshot, not a
// fresh forge read, so a control that fires at two seams cannot reach two
// answers about one run. TRIGGER comes from the run's trigger_source through
// the shared appliesto mapping.
func (s *Server) escalationChange(ctx context.Context, runRow *run.Run, escalations []spec.Escalation) (spec.Change, error) {
	var labels []string
	if runRow.IssueContext != nil {
		labels = runRow.IssueContext.Labels
	}
	change := appliesto.AdmissionChange(string(runRow.TriggerSource), labels)

	if !escalationsDeclarePaths(escalations) {
		return change, nil
	}
	approvedPlan, err := s.loadApprovedPlanForRun(ctx, runRow.ID)
	if err != nil {
		return spec.Change{}, fmt.Errorf("%w: %v", errEscalationPlanUnreadable, err)
	}
	if approvedPlan == nil {
		return spec.Change{}, errEscalationPlanUnreadable
	}
	change.Paths = planGateScopePaths(approvedPlan)
	return change, nil
}

// escalationsDeclarePaths reports whether any declaration carries a `paths`
// criterion — the condition under which the approved plan must be readable.
func escalationsDeclarePaths(escalations []spec.Escalation) bool {
	for i := range escalations {
		if len(escalations[i].Match.Paths) > 0 {
			return true
		}
	}
	return false
}

// escalationFiredPayload is the `escalation_fired` audit payload. Summary
// renders through escalation.RenderFired — the SAME helper the run-status
// escalations block renders through — so the audit row and the API surface can
// never describe one firing differently.
type escalationFiredPayload struct {
	Summary     string   `json:"summary"`
	Fingerprint string   `json:"fingerprint"`
	StageID     string   `json:"stage_id,omitempty"`
	Fired       []int    `json:"fired"`
	Count       *int     `json:"required_count,omitempty"`
	MemberOf    []string `json:"required_member_of,omitempty"`
	MinPerm     string   `json:"required_min_permission,omitempty"`
	MaxAutonomy string   `json:"max_autonomy,omitempty"`
}

// writeEscalationFiredAudit appends the governance-chain entry recording that
// an escalation fired.
//
// BEST-EFFORT, NOT A PRECONDITION OF ENFORCEMENT — and the asymmetry is a
// deliberate rule, not an inconsistency with the applies_to override (#2361,
// re-ratified for this change). An escalation firing moves in the SAFE
// direction: the gate gets STRICTER. Failing the gate because a log line could
// not be written would convert an audit-store outage into a total governance
// outage, which is strictly worse than an unrecorded raise. The rule the next
// reader should carry away:
//
//	a REFUSAL or a RAISE is best-effort — the safe outcome has already happened;
//	a GRANT (the applies_to override) is a PRECONDITION — it records an
//	exception being MADE.
//
// So the escalation is enforced whether or not this entry lands; the entry is
// written when the audit repository is available, and a failure is warn-logged
// NAMING the escalation summary so the raise is still recoverable from the
// application log.
//
// DE-DUPLICATION is read-then-append on (run, stage, fingerprint), which
// handles the common SEQUENTIAL case: repeated evaluations of one unchanged
// gate write exactly one entry. It is deliberately NOT atomic — two CONCURRENT
// evaluations can both read "absent" and both append, so a duplicate entry is
// possible. That is accepted as hygiene rather than a control gap (the same
// class as #2366): a duplicate governance-chain entry over-reports a raise that
// genuinely happened, and a uniqueness constraint or upsert to prevent it would
// be a schema change out of proportion to the defect. Do NOT read this as a
// per-(run, stage) uniqueness guarantee — it is not one.
//
// The fingerprint (not the stage alone) is the key so a genuinely CHANGED
// firing on the same stage — a scope amendment moving the plan's files into an
// escalated path, raising the requirement again — writes a SECOND entry, which
// is precisely what an operator must see.
//
// A READ failure during de-duplication EMITS anyway (fail toward visibility): a
// duplicate entry is benign, a missing one defeats the surface this audit
// exists to provide.
func (s *Server) writeEscalationFiredAudit(ctx context.Context, runRow *run.Run, stageID uuid.UUID, res escalation.Result) {
	if s.cfg.AuditRepo == nil {
		return
	}
	summary := escalation.RenderFired(res)
	fingerprint := escalation.Fingerprint(res)
	if s.escalationAlreadyAudited(ctx, runRow.ID, stageID, fingerprint) {
		return
	}

	indices := make([]int, 0, len(res.Fired))
	for _, f := range res.Fired {
		indices = append(indices, f.Index)
	}
	p := escalationFiredPayload{
		Summary:     summary,
		Fingerprint: fingerprint,
		Fired:       indices,
		Count:       res.Requirements.Count,
		MemberOf:    res.Requirements.MemberOf,
		MinPerm:     res.Requirements.MinPermission,
		MaxAutonomy: string(res.Requirements.MaxAutonomy),
	}
	var stagePtr *uuid.UUID
	if stageID != uuid.Nil {
		p.StageID = stageID.String()
		sid := stageID
		stagePtr = &sid
	}
	payload, err := json.Marshal(p)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "escalation: marshal escalation_fired payload failed; the escalation is still enforced",
			slog.String("run_id", runRow.ID.String()),
			slog.String("escalation", summary),
			slog.String("error", err.Error()))
		return
	}
	actorKind := audit.ActorSystem
	if _, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     runRow.ID,
		StageID:   stagePtr,
		Timestamp: time.Now().UTC(),
		Category:  CategoryEscalationFired,
		ActorKind: &actorKind,
		Payload:   payload,
	}); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "escalation: escalation_fired audit append failed; the escalation is still enforced",
			slog.String("run_id", runRow.ID.String()),
			slog.String("escalation", summary),
			slog.String("error", err.Error()))
	}
}

// escalationAlreadyAudited reports whether an `escalation_fired` entry with
// this stage AND this fingerprint already exists on the run. A read failure
// returns false — emit anyway, per writeEscalationFiredAudit's
// fail-toward-visibility note.
func (s *Server) escalationAlreadyAudited(ctx context.Context, runID, stageID uuid.UUID, fingerprint string) bool {
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, CategoryEscalationFired)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "escalation: escalation_fired de-duplication read failed; emitting anyway",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()))
		return false
	}
	want := ""
	if stageID != uuid.Nil {
		want = stageID.String()
	}
	for _, e := range entries {
		var p escalationFiredPayload
		if json.Unmarshal(e.Payload, &p) != nil {
			continue
		}
		if p.Fingerprint == fingerprint && p.StageID == want {
			return true
		}
	}
	return false
}

// escalatedApprovals is the RAISED approval requirement the gate enforces: the
// strictest of the gate's declared baseline and everything the fired
// escalations composed to.
//
// memberOf is a CONJUNCTION — an approver must belong to EVERY listed group —
// which is why membership can only ever narrow. The baseline group (when the
// gate declares one) is folded in with the escalated ones for exactly that
// reason: an escalation must not be able to REPLACE the gate's own group.
type escalatedApprovals struct {
	count         int
	memberOf      []string
	minPermission string
}

// effectiveApprovals composes a gate's baseline *spec.Approvals with the fired
// escalations' requirement, taking the STRICTEST value per dimension. It
// returns a COPY-shaped value and never mutates base: the parsed spec is
// shared and cached on the run row, so mutating it would clamp another run.
//
// RUNTIME LOWERING IS STRUCTURALLY IMPOSSIBLE HERE, independent of validation.
// Every dimension takes max / strictest / union, so an escalated value BELOW
// the baseline yields the BASELINE. That matters because validation is a
// DECLARATION-time check on the binary that parsed the spec: a spec that
// bypassed it — an older binary's cached bytes, a hand-built struct — still
// cannot weaken a gate at runtime.
func effectiveApprovals(base *spec.Approvals, req spec.ComposedRequirements) escalatedApprovals {
	out := escalatedApprovals{count: 1}
	if base != nil {
		if base.Count != nil {
			out.count = *base.Count
		}
		out.minPermission = base.MinPermission
		if base.MemberOf != "" {
			out.memberOf = append(out.memberOf, base.MemberOf)
		}
	}
	if req.Count != nil && *req.Count > out.count {
		out.count = *req.Count
	}
	if permissionAtLeast(req.MinPermission, out.minPermission) {
		out.minPermission = req.MinPermission
	}
	seen := make(map[string]struct{}, len(out.memberOf))
	for _, g := range out.memberOf {
		seen[g] = struct{}{}
	}
	for _, g := range req.MemberOf {
		if g == "" {
			continue
		}
		if _, dup := seen[g]; dup {
			continue
		}
		seen[g] = struct{}{}
		out.memberOf = append(out.memberOf, g)
	}
	return out
}

// permissionAtLeast reports whether candidate is a permission tier at least as
// strict as baseline, using the forge-neutral identity ladder rather than a
// second one. An empty/unparseable CANDIDATE never wins (there is nothing to
// raise to); an empty/unparseable BASELINE is no constraint, so any parseable
// candidate raises it.
func permissionAtLeast(candidate, baseline string) bool {
	c, ok := identity.ParsePermission(candidate)
	if !ok {
		return false
	}
	b, ok := identity.ParsePermission(baseline)
	if !ok {
		return true
	}
	return c.AtLeast(b)
}

// renderMemberOf serializes a membership CONJUNCTION for a surface whose
// pre-#2227 shape was a bare string: one group renders as that string
// (byte-identical to today — the overwhelmingly common case, since only a
// fired escalation can add a second), and a genuine conjunction renders as the
// list. Nothing renders as nil, so an absent requirement stays absent rather
// than becoming an empty string.
//
// This keeps the E9 Export v1 hash chain and every strict decoder unaffected
// for a run whose workflow declares no escalations, which is the additive
// posture every other enrichment on these payloads has taken.
func renderMemberOf(groups []string) any {
	switch len(groups) {
	case 0:
		return nil
	case 1:
		return groups[0]
	default:
		return groups
	}
}

// escalationsForRun parses the run's cached workflow spec and returns the
// workflow's escalation declarations. Returns (nil, nil) when the run declares
// none — including every legacy run with no cached spec, an unparseable spec,
// or a workflow missing from its own cached spec.
//
// A spec that will not parse is NOT a fail-closed case here: the run's OTHER
// gates already fail their own way on it (fetchApprovalsForStage falls open to
// the legacy path, checkDelegation refuses), and treating an unparseable spec
// as "escalations unknown, refuse" would convert a legacy-row read into a 503
// on a path that has nothing to do with escalations. The fail-closed modes this
// gate owns are the two the plan names: a Match error, and an unreadable plan
// while a paths-bearing escalation IS declared.
// It is a *Server method rather than a free function purely so every caller
// reads as one seam (`s.escalationsForRun` beside `s.resolveEscalations`); it
// touches no server state.
func (*Server) escalationsForRun(runRow *run.Run) (*spec.Workflow, error) {
	if runRow == nil || len(runRow.WorkflowSpec) == 0 {
		return nil, nil
	}
	parsed, err := spec.ParseBytes(runRow.WorkflowSpec)
	if err != nil {
		return nil, nil //nolint:nilerr // see the doc comment: an unparseable spec is not this gate's fail-closed mode.
	}
	wf, ok := parsed.Workflows[runRow.WorkflowID]
	if !ok || len(wf.Escalations) == 0 {
		return nil, nil
	}
	return &wf, nil
}

// resolveStageEscalations is the APPROVAL seam's entry point: it resolves the
// run row for a stage, evaluates the escalations, and returns the composed
// requirement. A run whose workflow declares none yields the zero requirement
// with ZERO extra reads beyond the run row the caller needs anyway.
func (s *Server) resolveStageEscalations(ctx context.Context, stage *run.Stage) (spec.ComposedRequirements, error) {
	if s.cfg.RunRepo == nil {
		return spec.ComposedRequirements{}, nil
	}
	runRow, err := s.cfg.RunRepo.GetRun(ctx, stage.RunID)
	if err != nil {
		if errors.Is(err, run.ErrNotFound) {
			// DEGRADES on a MISSING/legacy row only, and the reason is
			// specific rather than a general tolerance: a workflow's
			// `escalations` block is declared in the CACHED SPEC BYTES, which
			// live ON this run row. A row that does not exist means a spec
			// that does not exist, which escalationsForRun would answer with
			// "declares none" anyway — so refusing here would not enforce an
			// escalation we could otherwise have found; it would only convert
			// every legacy / manual / spec-less run's approval into a 503, on
			// a path where fetchApprovalsForStage already degrades to the
			// legacy first-vote gate on the SAME read.
			return spec.ComposedRequirements{}, nil //nolint:nilerr // see comment: a missing row's declaration is unrecoverable, so degrading is correct.
		}
		// A TRANSIENT repository error is NOT the legacy-row case (#2227
		// fixup): the run row — and the escalation declaration on it — may
		// well exist, so degrading to the baseline here would be exactly the
		// "proceed unescalated" outcome the file header forbids, on a run
		// whose spec can demonstrably declare an escalation. fetchApprovalsForStage
		// can succeed off a cached read while this second GetRun fails
		// transiently, so the two must not silently disagree. Fail closed,
		// reusing the refusal posture the two named modes below take.
		//
		// The three fail-CLOSED modes this gate owns are the ones where a
		// declaration IS (or may be) visible and cannot be evaluated: a
		// transient run-row read error, a Match error, and an unreadable plan
		// under a paths-bearing escalation. The last two are below.
		return spec.ComposedRequirements{}, fmt.Errorf("resolve escalations: read run row: %w", err)
	}
	wf, err := s.escalationsForRun(runRow)
	if err != nil || wf == nil {
		return spec.ComposedRequirements{}, err
	}
	res, err := s.resolveEscalations(ctx, runRow, wf, stage.ID)
	if err != nil {
		return spec.ComposedRequirements{}, err
	}
	return res.Requirements, nil
}
