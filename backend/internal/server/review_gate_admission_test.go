package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/approval"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// --- E54.53 / #3041: human-executor review-gate admission ---
//
// One behavioral case per named reviewGateAdmitReason. Every non-OK reason is
// a fail-closed branch that keeps today's ADR-018 409, so each gets its own
// assertion rather than the happy path plus a subset.

// groomingReviewSpec is the shape that MUST be admitted: a review stage with a
// human executor and NO pull_request input, mirroring backlog_grooming's
// `confirm` stage.
const groomingReviewSpec = `
version: "1.0"
roles:
  operator:
    members: ["@kuhlman-labs"]
workflows:
  backlog_grooming:
    stages:
      - id: confirm
        type: review
        executor:
          human: true
        gates:
          - type: approval
            sla: 24_hours
            approvers:
              any_of: [operator]
`

// featureChangeReviewSpec is the ADR-018 shape spelled with `artifact:
// pull_request` — this repository's feature_change review stage.
const featureChangeReviewSpec = `
version: "1.0"
workflows:
  feature_change:
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
      - id: review
        type: review
        executor:
          human: true
        inputs:
          - artifact: pull_request
            from_stage: implement
`

// routineChangeReviewSpec is the SAME ADR-018 shape spelled with `source:
// pull_request` — this repository's routine_change review stage. Both
// spellings ship, so leg 3 must recognize both.
const routineChangeReviewSpec = `
version: "1.0"
workflows:
  routine_change:
    stages:
      - id: review
        type: review
        executor:
          human: true
        inputs:
          - source: pull_request
            required: true
`

// agentReviewSpec pairs a human ROW (seeded by construction) with an
// AGENT-executor review SPEC stage, so leg 2's row/spec disagreement is
// reachable without the fixture itself calling the predicate under test.
const agentReviewSpec = `
version: "1.0"
workflows:
  backlog_grooming:
    stages:
      - id: confirm
        type: review
        executor:
          agent: claude-code
`

// noReviewSpec declares no review stage at all.
const noReviewSpec = `
version: "1.0"
workflows:
  backlog_grooming:
    stages:
      - id: groom
        type: plan
        executor:
          agent: claude-code
`

// twoReviewSpec declares TWO review stages — refused outright (operator
// binding condition 3) so admission and the first-match-by-type quorum
// readers can never resolve different spec stages.
const twoReviewSpec = `
version: "1.0"
workflows:
  backlog_grooming:
    stages:
      - id: confirm
        type: review
        executor:
          human: true
      - id: confirm_again
        type: review
        executor:
          human: true
`

// seedReviewStage seeds a review-typed stage row plus its run row. executor is
// the PERSISTED executor_kind (leg 1); specYAML is the run's cached workflow
// spec. A nil specYAML pointer is impossible here — pass "" for the
// no-workflow-spec case, and pass seedRun=false to leave the run row absent.
func seedReviewStage(rr *approvalRunRepo, workflowID, specYAML string, executor run.ExecutorKind, seedRun bool) *run.Stage {
	runID := uuid.New()
	st := &run.Stage{
		ID:               uuid.New(),
		RunID:            runID,
		Sequence:         1,
		Type:             run.StageTypeReview,
		ExecutorKind:     executor,
		ExecutorRef:      "",
		State:            run.StageStateAwaitingApproval,
		RequiresApproval: true,
		CreatedAt:        time.Now().UTC(),
		UpdatedAt:        time.Now().UTC(),
	}
	rr.mu.Lock()
	rr.stages[st.ID] = st
	rr.mu.Unlock()
	if seedRun {
		rr.seedRun(&run.Run{
			ID:           runID,
			Repo:         "kuhlman-labs/example",
			WorkflowID:   workflowID,
			WorkflowSHA:  "sha",
			WorkflowSpec: []byte(specYAML),
		})
	}
	return st
}

func TestResolveReviewGateAdmission_Reasons(t *testing.T) {
	cases := []struct {
		name       string
		workflowID string
		specYAML   string
		executor   run.ExecutorKind
		seedRun    bool
		nilStage   bool
		want       reviewGateAdmitReason
	}{
		{
			name:       "grooming shape is admitted",
			workflowID: "backlog_grooming",
			specYAML:   groomingReviewSpec,
			executor:   run.ExecutorHuman,
			seedRun:    true,
			want:       reviewGateAdmitOK,
		},
		{
			name:       "agent-executor row is refused (leg 1)",
			workflowID: "backlog_grooming",
			specYAML:   groomingReviewSpec,
			executor:   run.ExecutorAgent,
			seedRun:    true,
			want:       reviewGateAdmitNotHumanRow,
		},
		{
			name:     "nil stage is refused",
			nilStage: true,
			want:     reviewGateAdmitNotHumanRow,
		},
		{
			name:       "unreadable run row is refused",
			workflowID: "backlog_grooming",
			specYAML:   groomingReviewSpec,
			executor:   run.ExecutorHuman,
			seedRun:    false,
			want:       reviewGateAdmitRunUnavailable,
		},
		{
			name:       "empty cached spec is refused",
			workflowID: "backlog_grooming",
			specYAML:   "",
			executor:   run.ExecutorHuman,
			seedRun:    true,
			want:       reviewGateAdmitNoSpec,
		},
		{
			name:       "unparseable cached spec is refused",
			workflowID: "backlog_grooming",
			specYAML:   "version: \"1.0\"\nworkflows: [this is not a map",
			executor:   run.ExecutorHuman,
			seedRun:    true,
			want:       reviewGateAdmitSpecParse,
		},
		{
			name:       "workflow absent from spec is refused",
			workflowID: "not_declared",
			specYAML:   groomingReviewSpec,
			executor:   run.ExecutorHuman,
			seedRun:    true,
			want:       reviewGateAdmitWorkflowMissing,
		},
		{
			name:       "workflow declaring no review stage is refused",
			workflowID: "backlog_grooming",
			specYAML:   noReviewSpec,
			executor:   run.ExecutorHuman,
			seedRun:    true,
			want:       reviewGateAdmitNoReviewSpecStage,
		},
		{
			name:       "workflow declaring two review stages is refused",
			workflowID: "backlog_grooming",
			specYAML:   twoReviewSpec,
			executor:   run.ExecutorHuman,
			seedRun:    true,
			want:       reviewGateAdmitMultipleReviewStages,
		},
		{
			name:       "human row with agent spec stage is refused (leg 2)",
			workflowID: "backlog_grooming",
			specYAML:   agentReviewSpec,
			executor:   run.ExecutorHuman,
			seedRun:    true,
			want:       reviewGateAdmitSpecNotHuman,
		},
		{
			name:       "artifact: pull_request is refused (leg 3)",
			workflowID: "feature_change",
			specYAML:   featureChangeReviewSpec,
			executor:   run.ExecutorHuman,
			seedRun:    true,
			want:       reviewGateAdmitPullRequestManaged,
		},
		{
			name:       "source: pull_request is refused (leg 3)",
			workflowID: "routine_change",
			specYAML:   routineChangeReviewSpec,
			executor:   run.ExecutorHuman,
			seedRun:    true,
			want:       reviewGateAdmitPullRequestManaged,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, _, rr, _ := newApprovalServer(t)
			var st *run.Stage
			if !tc.nilStage {
				st = seedReviewStage(rr, tc.workflowID, tc.specYAML, tc.executor, tc.seedRun)
			}
			got := s.resolveReviewGateAdmission(context.Background(), st)
			if got != tc.want {
				t.Fatalf("admission reason = %s (%d), want %s (%d)",
					got, got, tc.want, tc.want)
			}
		})
	}
}

// An unwired RunRepo cannot resolve the run row, so admission fails closed
// rather than panicking. The handler never reaches this branch (its own
// approvals_unconfigured 503 fires first), but the predicate is reachable
// directly and must not admit.
func TestResolveReviewGateAdmission_NilRunRepo_FailsClosed(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0"})
	st := &run.Stage{ID: uuid.New(), RunID: uuid.New(), Type: run.StageTypeReview, ExecutorKind: run.ExecutorHuman}
	if got := s.resolveReviewGateAdmission(context.Background(), st); got != reviewGateAdmitRunUnavailable {
		t.Fatalf("admission reason = %s, want %s", got, reviewGateAdmitRunUnavailable)
	}
}

func TestSoleReviewSpecStage(t *testing.T) {
	cases := []struct {
		name       string
		stages     []spec.Stage
		wantReason reviewGateAdmitReason
		wantID     string
	}{
		{
			name:       "no review stage",
			stages:     []spec.Stage{{ID: "groom", Type: spec.StageTypePlan}},
			wantReason: reviewGateAdmitNoReviewSpecStage,
		},
		{
			name: "exactly one review stage resolves",
			stages: []spec.Stage{
				{ID: "groom", Type: spec.StageTypePlan},
				{ID: "confirm", Type: spec.StageTypeReview},
			},
			wantReason: reviewGateAdmitOK,
			wantID:     "confirm",
		},
		{
			name: "two review stages refuse",
			stages: []spec.Stage{
				{ID: "confirm", Type: spec.StageTypeReview},
				{ID: "confirm_again", Type: spec.StageTypeReview},
			},
			wantReason: reviewGateAdmitMultipleReviewStages,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := soleReviewSpecStage(spec.Workflow{Stages: tc.stages})
			if reason != tc.wantReason {
				t.Fatalf("reason = %s, want %s", reason, tc.wantReason)
			}
			if got.ID != tc.wantID {
				t.Fatalf("resolved stage id = %q, want %q", got.ID, tc.wantID)
			}
		})
	}
}

func TestSpecStageDeclaresPullRequestInput(t *testing.T) {
	cases := []struct {
		name  string
		stage spec.Stage
		want  bool
	}{
		{"no inputs", spec.Stage{ID: "confirm"}, false},
		{
			"github_issue input only",
			spec.Stage{Inputs: []spec.Input{{Source: spec.InputSourceGitHubIssue, Required: true}}},
			false,
		},
		{
			"source: pull_request",
			spec.Stage{Inputs: []spec.Input{{Source: spec.InputSourcePullRequest, Required: true}}},
			true,
		},
		{
			"artifact: pull_request",
			spec.Stage{Inputs: []spec.Input{{Artifact: "pull_request", FromStage: "implement"}}},
			true,
		},
		{
			"pull_request among several inputs",
			spec.Stage{Inputs: []spec.Input{
				{Source: spec.InputSourceGitHubIssue},
				{Artifact: "pull_request", FromStage: "implement"},
			}},
			true,
		},
		{
			"a non-pull_request artifact",
			spec.Stage{Inputs: []spec.Input{{Artifact: "plan", FromStage: "plan"}}},
			false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := specStageDeclaresPullRequestInput(tc.stage); got != tc.want {
				t.Fatalf("specStageDeclaresPullRequestInput = %v, want %v", got, tc.want)
			}
		})
	}
}

// The 409's details.admission_reason is a stable wire string, so pin every
// constant's spelling — a renamed reason is an API change, not a refactor.
func TestReviewGateAdmitReason_String(t *testing.T) {
	cases := map[reviewGateAdmitReason]string{
		reviewGateAdmitOK:                   "admitted",
		reviewGateAdmitNotHumanRow:          "not_human_executor_row",
		reviewGateAdmitNoSpec:               "no_workflow_spec",
		reviewGateAdmitRunUnavailable:       "run_unavailable",
		reviewGateAdmitSpecParse:            "workflow_spec_unparseable",
		reviewGateAdmitWorkflowMissing:      "workflow_missing",
		reviewGateAdmitNoReviewSpecStage:    "no_review_spec_stage",
		reviewGateAdmitMultipleReviewStages: "multiple_review_spec_stages",
		reviewGateAdmitSpecNotHuman:         "spec_stage_not_human_executor",
		reviewGateAdmitPullRequestManaged:   "pull_request_managed",
	}
	for reason, want := range cases {
		if got := reason.String(); got != want {
			t.Errorf("reason %d String() = %q, want %q", reason, got, want)
		}
	}
	if got := reviewGateAdmitReason(999).String(); got != "unknown" {
		t.Errorf("out-of-range String() = %q, want \"unknown\"", got)
	}
}

// requireReviewGateAttestation is decision-scoped: an approve without an
// attestation is refused, a reject without one is not.
func TestRequireReviewGateAttestation_DecisionScoped(t *testing.T) {
	cases := []struct {
		name     string
		decision approval.Decision
		comment  string
		wantOK   bool
	}{
		{"approve with attestation", approval.DecisionApprove, "checked the forge; 8 labels landed", true},
		{"approve with empty comment", approval.DecisionApprove, "", false},
		{"approve with whitespace comment", approval.DecisionApprove, "   \t\n ", false},
		{"reject with empty comment", approval.DecisionReject, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, _, rr, _ := newApprovalServer(t)
			st := seedReviewStage(rr, "backlog_grooming", groomingReviewSpec, run.ExecutorHuman, true)
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodPost, "/v0/stages/"+st.ID.String()+"/approvals", nil)
			got := s.requireReviewGateAttestation(w, r, st, tc.decision, tc.comment)
			if got != tc.wantOK {
				t.Fatalf("requireReviewGateAttestation = %v, want %v (body: %s)", got, tc.wantOK, w.Body.String())
			}
			if !tc.wantOK && w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", w.Code)
			}
		})
	}
}
