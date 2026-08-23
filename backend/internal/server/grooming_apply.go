package server

// On-approval grooming apply (E54.19 / #2822).
//
// THIS FILE IS ApplyGrooming's PRODUCTION CALLER. #2237 shipped the apply
// layer — the pure derivation, the seven containment rules, the
// continue-and-report executor — with its seam deliberately unwired. This
// hook wires it, and the wiring point is deliberate: the SERVER applies on the
// groom stage's approval gate, with NO agent in the loop and no new operator
// surface.
//
// WHY THE GATE AND NOT AN AGENT STAGE. The apply layer's containment rules ARE
// the authorization. Routing the call through an agent stage would put an
// agent between the operator's gate decision and the tracker write — the exact
// inversion the backlog-grooming workflow exists to prevent — and would need an
// implement-stage runner path that produces no diff. The gate approval is
// already the ratification signal (campaign_grooming_source.go reads the same
// predicate to build a campaign from a report); this makes it the apply trigger
// too, so the two surfaces agree on what "ratified" means by construction.
//
// WHAT IT AUTHORIZES, AND WHAT IT DOES NOT. Only HYGIENE-CLASS entries receive
// a decision: the report's hygiene defects and dependency edges, both of which
// workmgmt's action-class map routes to the `hygiene` action class — labels,
// epic links, board placement, field writes. Objective, reversible, and the one
// class this repository's workflow declares `mode: auto`. Every ordering,
// duplicate, decomposition and vision-drift entry receives NO decision at all,
// so the apply layer records it skipped with `no_decision` and dispatches
// nothing: the destructive classes stay PROPOSALS pending a per-entry
// decision-capture surface. Delegation is untouched — this change edits no
// spec or autonomy-registry file, so ordering/dedup/scoping remain refused at
// `mode: auto` by the two existing parse-time controls.
//
// THE LADDER IS THREE RUNGS, each an explicitly deletable control:
//
//	C1 DECISION     a decision other than approve applies NOTHING. The call site
//	                sits in finishApprovalAdvance's TYPE-only plan block and is
//	                passed the decision, rather than nesting inside the
//	                approve-only block, precisely so this guard's removal is
//	                observable on the reject path.
//	C2 REPORT       (an early-out, not a control) no grooming_report artifact on
//	                the stage means an ordinary plan approval: return having
//	                written nothing.
//	C3 RATIFICATION re-read the stage's approval rows and require >= 1 grant and
//	                ZERO rejections — the identical predicate
//	                campaign_grooming_source.go's approvedGroomingReport ships.
//	                A contested gate, or one this submission did not satisfy,
//	                applies nothing.
//
// Only past all three does anything resolve a provider. Every subsequent
// failure degrades to a NAMED reason with no dispatch and one server-authored
// grooming_apply_completed row, so an operator reading the audit trail can tell
// "the apply did not happen, and why" from "the apply happened and applied
// nothing".
//
// BEST-EFFORT, like every sibling in that block (fileSplitProposalChildren,
// fileOrLinkLiveValidationWalk, recordPlanPredictedRuntime): the gate already
// passed and its approval row is in place, so nothing here ever unwinds the
// approval.

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/approval"
	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// Capability seams, mirroring conventionsLoader: package-level vars so a test
// substitutes a fake mutator/reader WITHOUT mutating the process-wide provider
// registry every other server test shares.
var (
	groomingMutatorFor = workmgmt.MutatorFor
	groomingReaderFor  = workmgmt.ReaderFor
)

// groomingApplyBudget bounds the detached apply. The hook runs SYNCHRONOUSLY on
// the operator's approve request, so a wedged forge would otherwise hold that
// request open; the apply is continue-and-report, so a candidate whose dispatch
// exceeds the budget is recorded rather than retried.
// It is a var, not a const, ONLY so a test can shrink it: the bounded-context
// behaviour is a real failure mode (a wedged forge on the operator's approve
// request) and a three-minute constant is untestable in a unit test. Production
// never reassigns it.
var groomingApplyBudget = 3 * time.Minute

// The closed set of degrade reasons. Each names ONE rung of the resolution
// ladder that could not produce an input, and each is carried on the
// server-authored grooming_apply_completed row so the absence of an apply is
// itself auditable.
const (
	groomingApplyReportUnparseable      = "grooming_apply_report_unparseable"
	groomingApplyRunUnreadable          = "grooming_apply_run_unreadable"
	groomingApplyConventionsUnavailable = "grooming_apply_conventions_unavailable"
	groomingApplyMutatorUnavailable     = "grooming_apply_mutator_unavailable"
	groomingApplyReaderUnavailable      = "grooming_apply_reader_unavailable"
	groomingApplyNotRatified            = "grooming_apply_not_ratified"
	groomingApplyRepoUnresolvable       = "grooming_apply_repo_unresolvable"
)

// groomingApplyDegradePayload is the server-authored grooming_apply_completed
// row written when the apply did NOT run. It is a SUPERSET of
// workmgmt.GroomingApplySummary's count fields, so one category filter returns
// both the ran and the did-not-run cases and a reader can tell them apart by
// `degraded`. No grooming_mutation_applied row is ever written on a degrade
// path, so the churn guard's disposition baseline is unaffected and every entry
// correctly resurfaces on the next grooming run.
type groomingApplyDegradePayload struct {
	Applied       int    `json:"applied"`
	Failed        int    `json:"failed"`
	Skipped       int    `json:"skipped"`
	Degraded      bool   `json:"degraded"`
	DegradeReason string `json:"degrade_reason"`
}

// hygieneOnlyGroomingDecisions synthesizes the approved decision set: one
// approval per HYGIENE-CLASS entry, nothing else.
//
// PURE — no I/O, no provider, no clock — so the class boundary is unit-testable
// on its own and a widening regression fails on an assertion rather than on a
// forge fixture. The hygiene test goes through workmgmt.GroomingActionClassFor,
// the SAME map the derivation keys on, so a future remap of `dependency` out of
// the hygiene action class cannot silently widen what this hook auto-applies.
//
// CloseTarget is never populated: no hygiene kind is a duplicate close, and the
// field is the destructive dedup surface this slice grants nothing on.
//
// An entry with an EMPTY id is dropped rather than decided. A schema-validated
// report always carries one; an empty id would fail the apply layer's join
// check and refuse the WHOLE apply, so dropping it is the fail-safe direction —
// that entry simply receives no decision and resurfaces next run.
func hygieneOnlyGroomingDecisions(report *plan.GroomingReport) []workmgmt.GroomingDecision {
	if report == nil {
		return nil
	}
	var out []workmgmt.GroomingDecision
	add := func(id, reportClass string) {
		if strings.TrimSpace(id) == "" {
			return
		}
		if workmgmt.GroomingActionClassFor(reportClass) != spec.ActionGroomHygiene {
			return
		}
		out = append(out, workmgmt.GroomingDecision{EntryID: id, Verdict: workmgmt.GroomingApproved})
	}
	for _, e := range report.HygieneDefects {
		add(e.ID, plan.GroomingClassHygiene)
	}
	for _, e := range report.DependencyEdges {
		add(e.ID, plan.GroomingClassDependency)
	}
	// Ordering, Duplicates, DecompositionSuggestions and VisionDrift are
	// deliberately absent: NO decision means the apply layer records each
	// skipped with GroomingSkipNoDecision and dispatches nothing.
	return out
}

// groomingApplyAuditSink is the server's workmgmt.GroomingAuditSink: it writes
// the apply layer's records onto the run's audit chain.
//
// THE PAYLOAD SHAPE IS LOAD-BEARING (#2813). Both methods marshal the workmgmt
// record BARE — its own json tags, NOT wrapped in a run/stage envelope —
// because grooming_report.go's priorGroomingDispositions decodes entry_id /
// outcome / skip_reason directly off the payload, and the churn guard's whole
// idempotence baseline reads it. The run and stage identity ride the audit
// row's own columns, which is where they belong.
type groomingApplyAuditSink struct {
	s       *Server
	runID   uuid.UUID
	stageID uuid.UUID
}

func (k *groomingApplyAuditSink) append(ctx context.Context, category string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	systemKind := audit.ActorSystem
	stageID := k.stageID
	_, aerr := k.s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     k.runID,
		StageID:   &stageID,
		Timestamp: time.Now().UTC(),
		Category:  category,
		ActorKind: &systemKind,
		Payload:   body,
	})
	return aerr
}

func (k *groomingApplyAuditSink) RecordGroomingMutation(ctx context.Context, rec workmgmt.GroomingMutationRecord) error {
	return k.append(ctx, workmgmt.GroomingMutationAppliedCategory, rec)
}

func (k *groomingApplyAuditSink) RecordGroomingApplyCompleted(ctx context.Context, sum workmgmt.GroomingApplySummary) error {
	return k.append(ctx, workmgmt.GroomingApplyCompletedCategory, sum)
}

// applyApprovedGrooming is the on-approval hook. See the file header for the
// three-rung ladder and the best-effort contract.
func (s *Server) applyApprovedGrooming(ctx context.Context, stage *run.Stage, decision approval.Decision) {
	// C1 (issue AC4): a rejected report applies NOTHING. This is the control
	// the call-site placement exists to make observable — see the header.
	if decision != approval.DecisionApprove {
		return
	}
	if stage == nil || s.cfg.ArtifactRepo == nil || s.cfg.ApprovalRepo == nil ||
		s.cfg.RunRepo == nil || s.cfg.AuditRepo == nil {
		return
	}

	// C2 (early-out, NOT a control): no grooming_report on this stage means an
	// ordinary plan approval. Return having written nothing.
	report, found, degrade := s.groomingReportForStage(ctx, stage.ID)
	if !found && degrade == "" {
		return
	}

	sink := &groomingApplyAuditSink{s: s, runID: stage.RunID, stageID: stage.ID}
	if degrade != "" {
		s.degradeGroomingApply(ctx, sink, stage, degrade, "grooming report could not be parsed")
		return
	}

	// C3: re-ratify from the stage's approval rows. THE SUBMISSION IS NOT THE
	// GATE. approval.SubmitResult carries only {Approval, Inserted} and no
	// gate-satisfied flag, so satisfaction is not inferable from this one
	// approve — and a gate contested by a rejection must apply nothing even
	// when the latest submission is a grant. The predicate (>= 1 grant, 0
	// rejections) is the one campaign_grooming_source.go's approvedGroomingReport
	// already ships for the same report, so the two ratification surfaces stay
	// consistent rather than diverging.
	//
	// RESIDUAL, stated: under this repo's `count: 1` gate, grant-and-no-rejection
	// and gate-satisfied coincide. Under a `count: 2` gate a single grant would
	// satisfy this predicate before the gate itself is satisfied; tightening both
	// surfaces to a real count check is a follow-up, not a divergence to
	// introduce here.
	approvals, aerr := s.cfg.ApprovalRepo.ListForStage(ctx, stage.ID)
	if aerr != nil {
		s.degradeGroomingApply(ctx, sink, stage, groomingApplyNotRatified, aerr.Error())
		return
	}
	grants, rejections := 0, 0
	for _, ap := range approvals {
		if ap == nil {
			continue
		}
		switch ap.Decision {
		case approval.DecisionApprove:
			grants++
		case approval.DecisionReject:
			rejections++
		}
	}
	if grants == 0 || rejections > 0 {
		s.degradeGroomingApply(ctx, sink, stage, groomingApplyNotRatified,
			"the grooming gate is contested or ungranted; nothing applied")
		return
	}

	// Past every rung. Resolve the apply inputs; each failure degrades to a
	// named reason with NO dispatch.
	rn, rerr := s.cfg.RunRepo.GetRun(ctx, stage.RunID)
	if rerr != nil {
		s.degradeGroomingApply(ctx, sink, stage, groomingApplyRunUnreadable, rerr.Error())
		return
	}
	owner, name, ok := splitRepoFullName(rn.Repo)
	if !ok {
		s.degradeGroomingApply(ctx, sink, stage, groomingApplyRepoUnresolvable, rn.Repo)
		return
	}
	conv, cerr := conventionsLoader(ctx, rn.Repo)
	if cerr != nil {
		s.degradeGroomingApply(ctx, sink, stage, groomingApplyConventionsUnavailable, cerr.Error())
		return
	}
	mutator, merr := groomingMutatorFor(conv.Provider)
	if merr != nil {
		s.degradeGroomingApply(ctx, sink, stage, groomingApplyMutatorUnavailable, merr.Error())
		return
	}
	reader, rderr := groomingReaderFor(conv.Provider)
	if rderr != nil {
		// A nil reader would fail every observable candidate CLOSED one by one;
		// refusing the whole apply here is the same verdict, named once.
		s.degradeGroomingApply(ctx, sink, stage, groomingApplyReaderUnavailable, rderr.Error())
		return
	}

	target := workmgmt.Target{
		Repo:    workmgmt.Repo{Owner: owner, Name: name},
		Project: conv.Project,
		Jira:    conv.Jira,
		GitLab:  conv.GitLab,
	}
	if rn.InstallationID != nil {
		target.Scope = forge.FromGitHubInstallationID(*rn.InstallationID)
	}
	if target.Scope.IsZero() && s.cfg.GitHub != nil {
		// The defer_concern.go ladder: fall back to resolving the App
		// installation for the target repo when the run carries none. A
		// resolution failure is NOT fatal — the provider fails closed with its
		// own typed error and the candidate is recorded failed, which is the
		// continue-and-report posture this layer holds throughout.
		if scope, serr := s.resolveRepoScope(ctx, owner, name); serr == nil {
			target.Scope = scope
		}
	}

	req := workmgmt.GroomingApplyRequest{
		Target:    target,
		Report:    report,
		Decisions: hygieneOnlyGroomingDecisions(report),
		Modes:     s.groomingModesForRun(ctx, rn),
		// GateApproved is deliberately nil: it is the PER-ENTRY destructive
		// override, and this slice grants none.
		GateApproved: nil,
		States:       conv.States,
		// IceboxColumn stays empty: icebox is a scoping kind, this slice
		// authorizes no scoping entry, and there is nothing for it to route.
	}

	// DETACHED and BOUNDED (the acceptance_admission.go precedent).
	// context.WithoutCancel keeps the request's values but drops its
	// cancellation, so an operator's client disconnect cannot strand a
	// half-applied report mid-loop; the timeout bounds a wedged forge so it
	// cannot hold the approve request open.
	applyCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), groomingApplyBudget)
	defer cancel()

	result, err := workmgmt.ApplyGrooming(applyCtx, mutator, reader, sink, req)
	var auditErr *workmgmt.GroomingAuditError
	switch {
	case err == nil:
	case asGroomingAuditError(err, &auditErr):
		// Everything RAN; some audit writes failed. Loud, because an
		// unrecorded mutation is the one outcome AC3 exists to prevent.
		s.cfg.Logger.LogAttrs(applyCtx, slog.LevelError,
			"grooming apply completed with audit write failures",
			slog.String("run_id", stage.RunID.String()),
			slog.String("stage_id", stage.ID.String()),
			slog.String("error", auditErr.Error()),
		)
	default:
		// A join error or an argument error means NOTHING ran.
		s.cfg.Logger.LogAttrs(applyCtx, slog.LevelWarn,
			"grooming apply refused; nothing was dispatched",
			slog.String("run_id", stage.RunID.String()),
			slog.String("stage_id", stage.ID.String()),
			slog.String("error", err.Error()),
		)
		return
	}
	if result == nil {
		return
	}
	s.cfg.Logger.LogAttrs(applyCtx, slog.LevelInfo, "grooming apply completed",
		slog.String("run_id", stage.RunID.String()),
		slog.String("stage_id", stage.ID.String()),
		slog.Int("applied", result.Summary.Applied),
		slog.Int("failed", result.Summary.Failed),
		slog.Int("skipped", result.Summary.Skipped),
	)
}

// asGroomingAuditError is errors.As spelled as a helper so the switch above
// reads as the ladder it is.
func asGroomingAuditError(err error, target **workmgmt.GroomingAuditError) bool {
	ae, ok := err.(*workmgmt.GroomingAuditError)
	if ok {
		*target = ae
	}
	return ok
}

// groomingReportForStage loads the stage's grooming_report artifact. It returns
// (report, found, degradeReason): NOT found with an empty reason is the
// ordinary-plan-stage early-out; a non-empty reason means a report WAS present
// and could not be parsed, which is a degrade the operator must see rather than
// a silent no-op. When a stage carries more than one report (a re-run of the
// groom stage), the LATEST by CreatedAt wins — the same rule
// approvedGroomingReport applies.
func (s *Server) groomingReportForStage(ctx context.Context, stageID uuid.UUID) (*plan.GroomingReport, bool, string) {
	arts, err := s.cfg.ArtifactRepo.ListForStage(ctx, stageID)
	if err != nil {
		// An unreadable artifact list is indistinguishable from "no report" for
		// a stage that never had one, and this hook must not degrade an
		// ordinary plan approval. Silence is the fail-safe direction: no
		// mutation, and the entries resurface next run.
		return nil, false, ""
	}
	var newest *artifact.Artifact
	for _, a := range arts {
		if a == nil || a.Kind != artifact.KindGroomingReport {
			continue
		}
		// FIRST-HIT SENTINEL, not a numeric floor: the zero CreatedAt must not
		// read as an absence (the campaign_grooming_source.go lesson).
		if newest != nil && !a.CreatedAt.After(newest.CreatedAt) {
			continue
		}
		newest = a
	}
	if newest == nil {
		return nil, false, ""
	}
	report, perr := plan.ParseGroomingReport(newest.Content)
	if perr != nil {
		return nil, true, groomingApplyReportUnparseable
	}
	return report, true, ""
}

// groomingModesForRun projects the run's resolved autonomy matrix onto the
// action-class -> GroomingMode map ApplyGrooming keys on.
//
// FAIL-CLOSED BY CONSTRUCTION: an unreadable spec, an absent autonomy block, or
// an unrecognized mode all yield an EMPTY or partial map, and
// workmgmt.ResolveGroomingMode normalizes an absent class to `gated`. The
// destructive rule then requires mode auto PLUS an approved entry (or an
// explicit per-entry gate approval), neither of which this slice supplies — so
// the degenerate path authorizes nothing destructive either way.
func (s *Server) groomingModesForRun(ctx context.Context, rn *run.Run) map[string]workmgmt.GroomingMode {
	modes := map[string]workmgmt.GroomingMode{}
	wf, specStage, _, err := resolveSpecStageForRun(rn, run.StageTypePlan)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"grooming apply: workflow spec unreadable; every action class resolves gated",
			slog.String("run_id", rn.ID.String()),
			slog.String("error", err.Error()),
		)
		return modes
	}
	// A gate-level autonomy declaration wins over the workflow-level one, so
	// resolve against the stage's approval gate when it has one.
	var gate *spec.Gate
	for i := range specStage.Gates {
		if specStage.Gates[i].Type == spec.GateTypeApproval {
			gate = &specStage.Gates[i]
			break
		}
	}
	matrix := spec.ResolveAutonomy(&wf, gate)
	if matrix == nil {
		return modes
	}
	for _, a := range matrix.Actions {
		modes[a.Action] = workmgmt.GroomingMode(strings.ToLower(string(a.Mode)))
	}
	return modes
}

// degradeGroomingApply records ONE server-authored grooming_apply_completed row
// naming why the apply did not happen, alongside a Warn log. It writes NO
// grooming_mutation_applied row, so the churn guard's disposition read is
// untouched.
func (s *Server) degradeGroomingApply(ctx context.Context, sink *groomingApplyAuditSink, stage *run.Stage, reason, detail string) {
	s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "grooming apply degraded; nothing was dispatched",
		slog.String("run_id", stage.RunID.String()),
		slog.String("stage_id", stage.ID.String()),
		slog.String("degrade_reason", reason),
		slog.String("detail", detail),
	)
	if err := sink.append(ctx, workmgmt.GroomingApplyCompletedCategory, groomingApplyDegradePayload{
		Degraded: true, DegradeReason: reason,
	}); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelError, "grooming apply: degrade marker not recorded",
			slog.String("run_id", stage.RunID.String()),
			slog.String("stage_id", stage.ID.String()),
			slog.String("error", err.Error()),
		)
	}
}
