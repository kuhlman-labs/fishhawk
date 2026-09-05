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
// WHAT IT AUTHORIZES, AND WHAT IT DOES NOT (E54.48 / #2991). It CONSUMES the
// per-entry dispositions #2843 captures and merges them with an undispositioned
// default (mergeGroomingDecisions): an undispositioned HYGIENE-action-class entry
// (hygiene defects + dependency edges) still auto-applies — objective, reversible,
// the one class this repository's workflow declares `mode: auto` — while an
// undispositioned entry of a GATED class receives NO decision and is recorded
// skipped with `no_decision`. An explicit recorded `approved` is the ONLY thing
// that populates GateApproved, which is what unlocks a gated destructive class
// (workmgmt rule 4) and rule 8's delegation-tier label; `rejected` skips and
// `amended` is NOT an approval path. Delegation is untouched — this change edits
// no spec or autonomy-registry file, so `mode: auto` on ordering/dedup/scoping
// stays refused at parse time. NOTE the honest asymmetry with the issue's
// headline: dedup is `mode: gated`, so an approved duplicate now DISPATCHES; but
// scoping is `mode: report`, and workmgmt rule 3 short-circuits report mode
// BEFORE any gate-approval check, so an approved decomposition stays
// surface-only in THIS repo — "scoping icebox" does NOT become dispatchable here.
//
// THE CAPTURE/APPLY WINDOW (#2991). On approval, AFTER ratification and BEFORE
// provider resolution, the hook settles the artifact-bound capture window: it
// appends a `grooming_apply_window_closed` watermark and reads the consumable
// dispositions in ONE transaction (audit.GroomingWindowAppender). A reject
// settles the window `rejected`. The FIRST watermark is permanent. A
// PRE-ratification degrade does NOT close the window (nothing decided); a
// POST-ratification degrade leaves it CLOSED (the dispositions were consumed,
// and reopening on retry is the failure the protocol prevents).
//
// THE LADDER, each rung an explicitly deletable control, in evaluation order:
//
//	C2 REPORT       (an early-out, not a control) EVALUATED FIRST: no
//	                grooming_report artifact on the stage means an ordinary plan
//	                approval OR reject — return having written nothing (so a
//	                reject on an ordinary plan stage settles no window).
//	C1 DECISION     a decision other than approve applies NOTHING and settles the
//	                window `rejected`. The decision is PASSED (not implied by
//	                nesting) precisely so this guard's removal is observable on the
//	                reject path.
//	C3 RATIFICATION re-read the stage's approval rows and require >= 1 grant and
//	                ZERO rejections — the identical predicate
//	                campaign_grooming_source.go's approvedGroomingReport ships.
//	                A contested or ungranted gate applies nothing and does NOT
//	                close the window.
//
// Only past all three (and the settlement) does anything resolve a provider.
// Every subsequent
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
	"sort"
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
	// groomingApplyWindowUnsettled names a POST-RATIFICATION settlement failure:
	// C3 passed but appending the capture-window watermark (and reading the
	// consumed dispositions) failed, so nothing was consumed and the window was
	// NOT closed. Nothing is dispatched; a later approve can settle it (#2991).
	groomingApplyWindowUnsettled = "grooming_apply_window_unsettled"
)

// groomingApplyDegradePayload is the server-authored grooming_apply_completed
// row written when the apply did NOT run. It is a SUPERSET of
// workmgmt.GroomingApplySummary's count fields, so one category filter returns
// both the ran and the did-not-run cases and a reader can tell them apart by
// `degraded`. `refused` (#2860) is part of that superset and serializes as its
// zero value on a degrade, which is the correct fact: nothing was dispatched,
// so nothing was refused. No grooming_mutation_applied row is ever written on a degrade
// path, so the churn guard's disposition baseline is unaffected and every entry
// correctly resurfaces on the next grooming run.
type groomingApplyDegradePayload struct {
	Applied       int    `json:"applied"`
	Failed        int    `json:"failed"`
	Skipped       int    `json:"skipped"`
	Refused       int    `json:"refused"`
	Degraded      bool   `json:"degraded"`
	DegradeReason string `json:"degrade_reason"`
}

// consumedDisposition is one collapsed operator verdict consumed from the
// closed capture window (#2991): the last-wins verdict and its close_target for
// one entry id.
type consumedDisposition struct {
	Verdict     string
	CloseTarget string
}

// mergeGroomingDecisions is the PURE merge of the report's undispositioned
// defaults with the operator's recorded dispositions (#2991). It returns the
// GroomingDecision set AND the per-entry GateApproved map ApplyGrooming keys on.
//
// PURE — no I/O, no provider, no clock — so the policy is unit-testable and a
// widening regression fails on an assertion rather than a forge fixture.
//
// BASE LAYER: one synthesized `approved` decision per HYGIENE-ACTION-CLASS entry
// (hygiene defects + dependency edges), routed through
// workmgmt.GroomingActionClassFor so a future remap of `dependency` out of the
// hygiene action class cannot silently widen what auto-applies. This is the
// settled undispositioned-hygiene default (reversible, already authorized by the
// stage approval). An undispositioned entry of a GATED class gets NO base
// decision, so it reaches workmgmt rule 2 as GroomingSkipNoDecision.
//
// OVERLAY: for each disposition whose entry id the report DECLARES, set the
// decision's verdict to the recorded one (approved/rejected/amended, carrying
// close_target verbatim) for ANY class, adding an entry that has no base
// decision. A disposition naming an entry the report does NOT declare is DROPPED
// — workmgmt rule 1 would refuse the WHOLE apply on an unjoined decision id.
//
// GATE APPROVAL: GateApproved[id]=true is populated ONLY by an explicit recorded
// `approved` — never by a synthesized hygiene approval and never by `amended`.
// So `amended` cannot become an approval path, and a whole-report stage approval
// alone never authorizes a destructive-class entry (that needs an explicit
// per-entry approved disposition, which is exactly what unlocks workmgmt rule 4
// and rule 8).
func mergeGroomingDecisions(report *plan.GroomingReport, dispositions map[string]consumedDisposition) ([]workmgmt.GroomingDecision, map[string]bool) {
	if report == nil {
		return nil, nil
	}
	// declared maps every entry's OWN id (the domain ApplyGrooming's join and
	// deriveGroomingMutations key on) to its report class.
	declared := groomingReportEntryClasses(report)

	byEntry := map[string]workmgmt.GroomingDecision{}
	for id, reportClass := range declared {
		if strings.TrimSpace(id) == "" {
			// A schema-validated report always carries an id; an empty one would
			// fail the apply layer's join check and refuse the WHOLE apply, so
			// dropping it is the fail-safe direction — no decision, resurfaces.
			continue
		}
		// BASE LAYER: only a HYGIENE-action-class entry auto-applies undispositioned.
		// This filter is load-bearing — deleting it would synthesize an approval
		// for every gated class.
		if workmgmt.GroomingActionClassFor(reportClass) != spec.ActionGroomHygiene {
			continue
		}
		byEntry[id] = workmgmt.GroomingDecision{EntryID: id, Verdict: workmgmt.GroomingApproved}
	}

	gateApproved := map[string]bool{}
	for id, d := range dispositions {
		if _, ok := declared[id]; !ok {
			// Unjoinable: dropping it here keeps ApplyGrooming's rule-1 join from
			// refusing the whole apply on a decision the report never declared.
			continue
		}
		byEntry[id] = workmgmt.GroomingDecision{
			EntryID:     id,
			Verdict:     workmgmt.GroomingVerdict(d.Verdict),
			CloseTarget: d.CloseTarget,
		}
		if workmgmt.GroomingVerdict(d.Verdict) == workmgmt.GroomingApproved {
			gateApproved[id] = true
		}
	}

	ids := make([]string, 0, len(byEntry))
	for id := range byEntry {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]workmgmt.GroomingDecision, 0, len(ids))
	for _, id := range ids {
		out = append(out, byEntry[id])
	}
	if len(gateApproved) == 0 {
		gateApproved = nil
	}
	return out, gateApproved
}

// groomingReportEntryClasses maps every report entry's OWN id field to its report
// class — the SAME domain workmgmt.groomingReportEntryIDs / deriveGroomingMutations
// key ApplyGrooming's join on, so a synthesized or overlaid decision joins to its
// candidate. (plan.GroomingEntryClasses keys on the DERIVED id instead; in a
// schema-valid report the two coincide, but keying the merge on e.ID keeps it
// aligned with the layer that consumes the decisions.)
func groomingReportEntryClasses(report *plan.GroomingReport) map[string]string {
	out := map[string]string{}
	for _, e := range report.HygieneDefects {
		out[e.ID] = plan.GroomingClassHygiene
	}
	for _, e := range report.DependencyEdges {
		out[e.ID] = plan.GroomingClassDependency
	}
	for _, e := range report.Ordering {
		out[e.ID] = plan.GroomingClassOrdering
	}
	for _, e := range report.Duplicates {
		out[e.ID] = plan.GroomingClassDuplicate
	}
	for _, e := range report.DecompositionSuggestions {
		out[e.ID] = plan.GroomingClassDecomposition
	}
	for _, e := range report.VisionDrift {
		out[e.ID] = plan.GroomingClassVisionDrift
	}
	return out
}

// collapseGroomingConsumed collapses the consumed disposition rows LAST-WINS per
// entry id. The rows are already artifact-scoped and below-watermark by the
// settlement (audit layer or the fallback), so this only decodes and collapses.
func collapseGroomingConsumed(entries []*audit.Entry) map[string]consumedDisposition {
	type ranked struct {
		d consumedDisposition
		s int64
	}
	latest := map[string]ranked{}
	for _, e := range entries {
		if e == nil {
			continue
		}
		var p groomingDispositionPayload
		if json.Unmarshal(e.Payload, &p) != nil || p.EntryID == "" {
			continue
		}
		if cur, ok := latest[p.EntryID]; ok && cur.s > e.Sequence {
			continue
		}
		latest[p.EntryID] = ranked{consumedDisposition{Verdict: p.Verdict, CloseTarget: p.CloseTarget}, e.Sequence}
	}
	out := make(map[string]consumedDisposition, len(latest))
	for id, r := range latest {
		out[id] = r.d
	}
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
	if stage == nil || s.cfg.ArtifactRepo == nil || s.cfg.ApprovalRepo == nil ||
		s.cfg.RunRepo == nil || s.cfg.AuditRepo == nil {
		return
	}

	// C2 (early-out, NOT a control), EVALUATED BEFORE THE DECISION BRANCH: no
	// grooming_report on this stage means an ordinary plan approval OR reject.
	// Return having written nothing — a reject on an ordinary plan stage settles
	// no window (approval condition: C2 before the decision branch).
	report, art, found, degrade := s.groomingReportForStage(ctx, stage.ID)
	if !found && degrade == "" {
		return
	}

	sink := &groomingApplyAuditSink{s: s, runID: stage.RunID, stageID: stage.ID}
	if degrade != "" {
		s.degradeGroomingApply(ctx, sink, stage, degrade, "grooming report could not be parsed")
		return
	}

	// C1 (issue AC4): a decision other than approve applies NOTHING and settles
	// the window as `rejected` — as decisively as approval closes it. Passing the
	// decision (rather than nesting in the approve-only block) is what makes this
	// guard's deletion observable on the reject path.
	if decision != approval.DecisionApprove {
		if _, err := s.settleGroomingWindow(ctx, stage, art.ID.String(), "rejected"); err != nil {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "grooming apply: reject-path window settlement failed",
				slog.String("run_id", stage.RunID.String()),
				slog.String("stage_id", stage.ID.String()),
				slog.String("error", err.Error()))
		}
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
		// PRE-RATIFICATION DEGRADE: nothing was decided, so the window is NOT
		// closed — the gate may still settle, and closing here would permanently
		// refuse a legitimate later capture (#2991).
		s.degradeGroomingApply(ctx, sink, stage, groomingApplyNotRatified,
			"the grooming gate is contested or ungranted; nothing applied")
		return
	}

	// Ratification passed. SETTLE THE WINDOW IMMEDIATELY, before any provider
	// resolution: append the artifact-bound `approved` watermark and read back the
	// consumable dispositions in ONE transaction (#2991). From here on any degrade
	// is POST-RATIFICATION and leaves the window CLOSED — the dispositions were
	// consumed, and reopening on a retry is exactly the failure this protocol
	// prevents.
	consumed, serr := s.settleGroomingWindow(ctx, stage, art.ID.String(), "approved")
	if serr != nil {
		// The watermark did not land, so nothing was consumed and the window is
		// still open; a later approve can settle it.
		s.degradeGroomingApply(ctx, sink, stage, groomingApplyWindowUnsettled, serr.Error())
		return
	}
	decisions, gateApproved := mergeGroomingDecisions(report, consumed)

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
		Decisions: decisions,
		Modes:     s.groomingModesForRun(ctx, rn),
		// GateApproved carries the explicit per-entry `approved` dispositions
		// (#2991) — the ONLY thing that unlocks a gated destructive class or
		// rule 8's delegation-tier label. Nil when no entry was explicitly
		// approved, so a whole-report gate approval alone authorizes nothing
		// destructive.
		GateApproved: gateApproved,
		States:       conv.States,
		// IceboxColumn stays empty: no conventions source declares one, and the
		// issue forbids touching the work-management schema — so an approved
		// decomposition entry records GroomingSkipIceboxColumnUnavailable rather
		// than misrouting.
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
		slog.Int("refused", result.Summary.Refused),
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
func (s *Server) groomingReportForStage(ctx context.Context, stageID uuid.UUID) (*plan.GroomingReport, *artifact.Artifact, bool, string) {
	arts, err := s.cfg.ArtifactRepo.ListForStage(ctx, stageID)
	if err != nil {
		// An unreadable artifact list is indistinguishable from "no report" for
		// a stage that never had one, and this hook must not degrade an
		// ordinary plan approval. Silence is the fail-safe direction: no
		// mutation, and the entries resurface next run.
		return nil, nil, false, ""
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
		return nil, nil, false, ""
	}
	report, perr := plan.ParseGroomingReport(newest.Content)
	if perr != nil {
		return nil, newest, true, groomingApplyReportUnparseable
	}
	return report, newest, true, ""
}

// settleGroomingWindow closes the disposition-capture window for artifactID with
// the given settlement, atomically appending the watermark and reading back the
// consumed dispositions ({this artifact, below the watermark}) — the #2991
// TOCTOU close. It returns the consumed dispositions collapsed last-wins per
// entry id. It drives the audit-layer capability when present (production
// postgresRepo) and a non-atomic, permanence-aware read-then-append fallback for
// in-memory repositories that do not implement it.
func (s *Server) settleGroomingWindow(ctx context.Context, stage *run.Stage, artifactID, settlement string) (map[string]consumedDisposition, error) {
	now := time.Now().UTC()
	payload, err := json.Marshal(groomingWindowPayload{
		RunID: stage.RunID.String(), StageID: stage.ID.String(),
		ArtifactID: artifactID, Settlement: settlement,
		ClosedAt: now.Format(time.RFC3339Nano),
	})
	if err != nil {
		return nil, err
	}
	systemKind := audit.ActorSystem
	stageID := stage.ID
	params := audit.ChainAppendParams{
		RunID: stage.RunID, StageID: &stageID, Timestamp: now,
		Category: audit.GroomingApplyWindowClosedCategory, ActorKind: &systemKind, Payload: payload,
	}

	if appender, ok := s.cfg.AuditRepo.(audit.GroomingWindowAppender); ok {
		_, consumed, aerr := appender.AppendChainedGroomingWindowClose(ctx, params, artifactID)
		if aerr != nil {
			return nil, aerr
		}
		return collapseGroomingConsumed(consumed), nil
	}

	// FALLBACK (in-memory repos): permanence-aware read-then-append. Not atomic —
	// the protocol's guarantees hold only on the production repository, which is
	// why the interleaving tests are pgtest tests.
	wins, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, stage.RunID, audit.GroomingApplyWindowClosedCategory)
	if err != nil {
		return nil, err
	}
	var existing *audit.Entry
	for _, e := range wins {
		if e == nil {
			continue
		}
		var wp groomingWindowPayload
		if json.Unmarshal(e.Payload, &wp) != nil || wp.ArtifactID != artifactID {
			continue
		}
		if existing == nil || e.Sequence < existing.Sequence {
			existing = e
		}
	}
	var belowSeq int64
	if existing != nil {
		belowSeq = existing.Sequence // PERMANENCE: never extend the bound.
	} else {
		wm, aerr := s.cfg.AuditRepo.AppendChained(ctx, params)
		if aerr != nil {
			return nil, aerr
		}
		belowSeq = wm.Sequence
	}
	disp, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, stage.RunID, CategoryGroomingDispositionRecorded)
	if err != nil {
		return nil, err
	}
	scoped := make([]*audit.Entry, 0, len(disp))
	for _, e := range disp {
		if e == nil || e.Sequence >= belowSeq {
			continue
		}
		var dp groomingDispositionPayload
		if json.Unmarshal(e.Payload, &dp) != nil || dp.ArtifactID != artifactID {
			continue
		}
		scoped = append(scoped, e)
	}
	return collapseGroomingConsumed(scoped), nil
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
