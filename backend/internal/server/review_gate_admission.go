package server

import (
	"context"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/approval"
	"github.com/kuhlman-labs/fishhawk/backend/internal/operatorrole"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// reviewGateAdmitReason names the outcome of the review-gate admission
// conjunction (E54.53 / #3041). Exactly one value — reviewGateAdmitOK — admits
// the approval; EVERY other value means NOT ADMITTED, i.e. today's
// unconditional ADR-018 409. The fail-closed direction is therefore
// preserving current behavior: an unresolvable spec can never open a
// PR-merge-managed gate.
type reviewGateAdmitReason int

const (
	// reviewGateAdmitOK — all legs hold: the persisted row is human-executor,
	// the workflow declares exactly one review stage, that stage declares
	// executor.human, and it declares no pull_request input.
	reviewGateAdmitOK reviewGateAdmitReason = iota
	// reviewGateAdmitNotHumanRow — the persisted stage row's executor_kind is
	// not `human`. Written at stage-create time by
	// webhook.CreateStagesFromSpec/mapExecutor directly from that spec stage's
	// own executor, so this leg needs no resolver and cannot be confused by a
	// sibling stage.
	reviewGateAdmitNotHumanRow
	// reviewGateAdmitNoSpec — the run carries no cached workflow spec.
	reviewGateAdmitNoSpec
	// reviewGateAdmitRunUnavailable — the run row could not be read.
	reviewGateAdmitRunUnavailable
	// reviewGateAdmitSpecParse — the cached spec does not parse.
	reviewGateAdmitSpecParse
	// reviewGateAdmitWorkflowMissing — the run's workflow id is absent from the
	// parsed spec.
	reviewGateAdmitWorkflowMissing
	// reviewGateAdmitNoReviewSpecStage — the workflow declares no review stage
	// at all, so there is nothing to resolve this row against.
	reviewGateAdmitNoReviewSpecStage
	// reviewGateAdmitMultipleReviewStages — the workflow declares MORE THAN ONE
	// review stage (operator binding condition 3, #3041). Admission resolves the
	// SOLE review spec stage, while fetchApprovalsForStage / fetchGateForStage
	// still resolve first-match-by-type; for a two-review-stage workflow those
	// could be different stages, splitting one authorization decision across two
	// spec stages. No workflow in this repository declares two review stages, so
	// admission refuses that shape outright rather than carrying the hazard.
	reviewGateAdmitMultipleReviewStages
	// reviewGateAdmitSpecNotHuman — the row says human but the resolved spec
	// stage does not declare executor.human. A row/spec disagreement is a
	// fail-closed anomaly, not a tie to break.
	reviewGateAdmitSpecNotHuman
	// reviewGateAdmitPullRequestManaged — the resolved spec stage declares a
	// pull_request input, so ADR-018's GitHub-managed merge gate owns it. This
	// is the discriminator that keeps feature_change / routine_change closed.
	reviewGateAdmitPullRequestManaged
)

// String is the stable snake_case wire form surfaced as the 409's
// details.admission_reason, so a refused caller learns WHICH leg failed rather
// than only that review stages are managed by GitHub.
func (r reviewGateAdmitReason) String() string {
	switch r {
	case reviewGateAdmitOK:
		return "admitted"
	case reviewGateAdmitNotHumanRow:
		return "not_human_executor_row"
	case reviewGateAdmitNoSpec:
		return "no_workflow_spec"
	case reviewGateAdmitRunUnavailable:
		return "run_unavailable"
	case reviewGateAdmitSpecParse:
		return "workflow_spec_unparseable"
	case reviewGateAdmitWorkflowMissing:
		return "workflow_missing"
	case reviewGateAdmitNoReviewSpecStage:
		return "no_review_spec_stage"
	case reviewGateAdmitMultipleReviewStages:
		return "multiple_review_spec_stages"
	case reviewGateAdmitSpecNotHuman:
		return "spec_stage_not_human_executor"
	case reviewGateAdmitPullRequestManaged:
		return "pull_request_managed"
	default:
		return "unknown"
	}
}

// soleReviewSpecStage resolves the workflow's ONE review-typed spec stage.
//
// Resolution is deliberately all-or-nothing rather than ordinal- or
// index-keyed (operator binding condition 3, #3041). The deploy side resolves
// its spec stage by DEPLOY ORDINAL (deployStageForRunStage, E23.19 / #2642)
// because a workflow may legally declare several deploy stages. The same is
// true of review stages in the schema — but the sibling review-side readers
// fetchApprovalsForStage and fetchGateForStage still resolve first-match-by-
// type, so an ordinal resolver here would let admission read the k-th review
// stage while quorum read the first. Splitting one authorization decision
// across two spec stages is a latent hazard that is cheap to exclude now and
// expensive later, and no workflow in this repository declares two review
// stages, so a multi-review workflow is refused outright instead.
//
// Returns (stage, reviewGateAdmitOK) when exactly one review stage is
// declared; otherwise the naming refusal reason.
func soleReviewSpecStage(wf spec.Workflow) (spec.Stage, reviewGateAdmitReason) {
	var found spec.Stage
	n := 0
	for _, sp := range wf.Stages {
		if sp.Type != spec.StageTypeReview {
			continue
		}
		n++
		if n == 1 {
			found = sp
		}
	}
	switch {
	case n == 0:
		return spec.Stage{}, reviewGateAdmitNoReviewSpecStage
	case n > 1:
		return spec.Stage{}, reviewGateAdmitMultipleReviewStages
	}
	return found, reviewGateAdmitOK
}

// specStageDeclaresPullRequestInput reports whether a spec stage declares a
// pull_request input in EITHER spelling this repository's own workflows use:
// feature_change's review stage declares `artifact: pull_request, from_stage:
// implement`, and routine_change's declares `source: pull_request, required:
// true`. Checking only one form would admit the other workflow's PR-merge
// gate.
func specStageDeclaresPullRequestInput(st spec.Stage) bool {
	for _, in := range st.Inputs {
		if in.Source == spec.InputSourcePullRequest {
			return true
		}
		if in.Artifact == string(spec.ArtifactPullRequest) {
			return true
		}
	}
	return false
}

// resolveReviewGateAdmission is the ONE predicate deciding whether a review
// stage has an in-Fishhawk approval surface (E54.53 / #3041).
//
// ADR-018 (#311, #313) made review-stage approval GitHub's: the PR merge event
// advances the stage and branch protection enforces the approver list. That is
// correct for the PR-merge-managed review stages of feature_change and
// routine_change — and wrong for backlog_grooming's `confirm` stage, which
// declares `executor: human` with `not: [agent]` and takes no pull_request
// input, so an unconditional refusal leaves it with no approvable surface
// anywhere and parks the run in `running` forever.
//
// Admission is a THREE-LEG CONJUNCTION, evaluated in this fixed order so the
// most specific reason is reported:
//
//	LEG 1  the PERSISTED row's executor_kind is `human`
//	LEG 2  the workflow's SOLE review spec stage declares executor.human
//	LEG 3  that spec stage declares NO pull_request input
//
// Every resolution failure returns a non-OK reason, which the caller answers
// with today's 409 — so the fail-closed direction preserves current behavior.
func (s *Server) resolveReviewGateAdmission(ctx context.Context, stage *run.Stage) reviewGateAdmitReason {
	if stage == nil || stage.ExecutorKind != run.ExecutorHuman {
		// LEG 1. The unambiguous leg: mapExecutor persisted this at
		// stage-create from that spec stage's own executor.
		return reviewGateAdmitNotHumanRow
	}
	if s.cfg.RunRepo == nil {
		return reviewGateAdmitRunUnavailable
	}
	runRow, err := s.cfg.RunRepo.GetRun(ctx, stage.RunID)
	if err != nil {
		return reviewGateAdmitRunUnavailable
	}
	if len(runRow.WorkflowSpec) == 0 {
		return reviewGateAdmitNoSpec
	}
	parsed, err := spec.ParseBytes(runRow.WorkflowSpec)
	if err != nil {
		return reviewGateAdmitSpecParse
	}
	wf, ok := parsed.Workflows[runRow.WorkflowID]
	if !ok {
		return reviewGateAdmitWorkflowMissing
	}
	resolved, reason := soleReviewSpecStage(wf)
	if reason != reviewGateAdmitOK {
		return reason
	}
	if !resolved.Executor.Human {
		// LEG 2: a row/spec disagreement fails closed rather than admitting.
		return reviewGateAdmitSpecNotHuman
	}
	if specStageDeclaresPullRequestInput(resolved) {
		// LEG 3: ADR-018 owns this gate.
		return reviewGateAdmitPullRequestManaged
	}
	return reviewGateAdmitOK
}

// humanReviewGateExecutorRefusal is the shared tail of both agent refusals: it
// names the executor constraint the gate declares, so an operator reading the
// message learns WHY the credential was refused rather than only that it was.
const humanReviewGateExecutorRefusal = "this gate declares executor: human with not: [agent]"

// refuseAgentOnHumanReviewGate enforces the admitted gate's `not: [agent]`
// constraint AT THE SURFACE — declared in the workflow spec today but honored
// only because no surface existed, which is not the same as enforced.
//
// It is deliberately placed at the TOP of handleSubmitApproval, AHEAD of
// requireWriteScope (operator binding condition 3041.1). A real run-bound fhm_
// token does NOT carry write:approvals, so behind the scope check the message
// an operator actually sees in production is `insufficient_scope`, which names
// nothing about the executor constraint — only an artificially minted token
// would ever reach the named refusal. Ahead of it, BOTH the scope-less
// (realistic) and scope-bearing run-bound tokens get the named refusal.
//
// Two rungs, mirroring grooming_dispositions.go's G2/G3:
//
//	403 self_decision            — a run-bound `mcp:run:` agent token, even for
//	                               its OWN run.
//	403 operator_agent_forbidden — a DELEGATED operator-agent token, keyed on
//	                               operatorrole.IsTokenSubject, the same
//	                               predicate actor.go uses to classify a
//	                               delegated writer as actor_kind=agent.
//
// Returns true when it wrote a response. A non-agent identity returns false
// immediately, so the common operator path pays no extra repository read; only
// an agent identity triggers the stage lookup, and an unresolvable stage or a
// non-admitted gate also returns false so the ordinary 400/404/409 ladder
// downstream produces the response.
func (s *Server) refuseAgentOnHumanReviewGate(w http.ResponseWriter, r *http.Request) bool {
	id := IdentityFrom(r.Context())
	_, runBound := runBoundTokenRunID(id)
	operatorAgent := operatorrole.IsTokenSubject(id.Subject)
	if !runBound && !operatorAgent {
		return false
	}
	stage := s.lookupStageForPath(r)
	if stage == nil || stage.Type != run.StageTypeReview {
		return false
	}
	if s.resolveReviewGateAdmission(r.Context(), stage) != reviewGateAdmitOK {
		return false
	}
	if runBound {
		s.writeError(w, r, http.StatusForbidden, "self_decision",
			"a run-bound agent token may not approve a human-executor review gate, not even for its own run; "+
				humanReviewGateExecutorRefusal,
			map[string]any{"stage_id": stage.ID.String()})
		return true
	}
	s.writeError(w, r, http.StatusForbidden, "operator_agent_forbidden",
		"a delegated operator-agent token may not approve a human-executor review gate; "+
			humanReviewGateExecutorRefusal+", and an agent recording it would convert the operator gate into a self-approval",
		map[string]any{"stage_id": stage.ID.String(), "subject": id.Subject})
	return true
}

// lookupStageForPath resolves the request's {stage_id} stage row, returning nil
// on any failure. It exists so refuseAgentOnHumanReviewGate can run ahead of
// the handler's own parse/fetch ladder without duplicating its error responses:
// every nil here falls through to that ladder, which writes the real 400 / 404
// / 500.
func (s *Server) lookupStageForPath(r *http.Request) *run.Stage {
	if s.cfg.RunRepo == nil {
		return nil
	}
	stageID, err := uuid.Parse(r.PathValue("stage_id"))
	if err != nil {
		return nil
	}
	stage, err := s.cfg.RunRepo.GetStage(r.Context(), stageID)
	if err != nil {
		return nil
	}
	return stage
}

// requireReviewGateAttestation enforces that an APPROVE of an admitted
// human-executor review gate carries a non-empty comment.
//
// The attestation IS what this gate records: backlog_grooming's `confirm`
// stage exists because an audit row saying `applied` is not the same claim as
// the tracker actually carrying the change (walk #2844 / #2847), so it is where
// an operator records that they checked the FORGE rather than the summary. A
// REJECT needs no attestation — the refusal is itself the judgment — so the
// guard is decision-scoped, not a blanket comment requirement.
//
// Returns true to continue, false after writing a 400.
func (s *Server) requireReviewGateAttestation(w http.ResponseWriter, r *http.Request, stage *run.Stage, decision approval.Decision, comment string) bool {
	if decision != approval.DecisionApprove {
		return true
	}
	if strings.TrimSpace(comment) != "" {
		return true
	}
	s.writeError(w, r, http.StatusBadRequest, "attestation_required",
		"a human-executor review gate records an attestation — state what you checked in `comment`",
		map[string]any{"stage_id": stage.ID.String(), "field": "comment"})
	return false
}
