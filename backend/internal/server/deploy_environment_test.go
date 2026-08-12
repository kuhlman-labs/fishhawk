package server

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/approval"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// deploySpecMultiEnv is a deploy stage declaring a multi-environment allow-list
// [staging, prod]. deployEnvironmentForRun's first-wins fallback reports
// "staging" on it, so every fallback assertion below expects "staging" and the
// approved-environment path is proven by a result of "prod" instead.
const deploySpecMultiEnv = `
version: "1.0"
workflows:
  release:
    stages:
      - id: deploy
        type: deploy
        executor:
          delegate:
            target: github_actions
            workflow_ref: deploy.yml
        constraints:
          - allowed_environments: [staging, prod]
        produces:
          - artifact: deployment
`

// twoDeployStagesSpec declares TWO deploy stages with DIFFERENT
// allowed_environments: the FIRST permits only [staging], the SECOND only
// [prod]. First-stage keying is the shared, pre-existing behavior of the gate
// and the record alike; this fixture characterizes that both sides key on the
// SAME (first) stage — it is NOT an endorsement of first-stage keying, which is
// a latent multi-deploy-stage defect tracked separately and out of #2324 scope.
const twoDeployStagesSpec = `
version: "1.0"
workflows:
  release:
    stages:
      - id: deploy_staging
        type: deploy
        executor:
          delegate:
            target: github_actions
            workflow_ref: deploy-staging.yml
        constraints:
          - allowed_environments: [staging]
        produces:
          - artifact: deployment
      - id: deploy_prod
        type: deploy
        executor:
          delegate:
            target: github_actions
            workflow_ref: deploy-prod.yml
        constraints:
          - allowed_environments: [prod]
        produces:
          - artifact: deployment
`

// seedStageEnvApproval appends an approval row straight onto the fake repo so
// the bad/edge states below are seeded BY CONSTRUCTION — the RED of a
// counterfactual lands on the behavioral assertion, not on a fixture-setup
// guard. An empty comment seeds a nil Comment (the no-comment approval case).
func seedStageEnvApproval(ar *fakeApprovalRepo, stageID uuid.UUID, dec approval.Decision, comment string) {
	ar.mu.Lock()
	defer ar.mu.Unlock()
	var cptr *string
	if comment != "" {
		c := comment
		cptr = &c
	}
	ar.all = append(ar.all, &approval.Approval{
		ID:          uuid.New(),
		StageID:     stageID,
		Decision:    dec,
		Comment:     cptr,
		Surface:     approval.SurfaceAPI,
		SubmittedAt: time.Now().UTC(),
	})
}

// newStageEnvServer stands up a server whose RunRepo carries deploySpecMultiEnv
// on the seeded deploy run and whose ApprovalRepo is ar (pass nil to exercise
// the unwired-repo branch). Returns the deploy stage and run so tests drive
// deployEnvironmentForStage directly.
func newStageEnvServer(t *testing.T, ar approval.Repository) (*Server, *run.Stage, *run.Run) {
	t.Helper()
	rr := newApprovalRunRepo()
	stage, runRow := seedDeployRun(rr, "release", deploySpecMultiEnv)
	s := New(Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      rr,
		ApprovalRepo: ar,
	})
	return s, stage, runRow
}

// errListApprovalRepo is an approval.Repository whose ListForStage always
// errors, exercising deployApprovedEnvironment's best-effort read-failure
// fallback.
type errListApprovalRepo struct{ err error }

func (e errListApprovalRepo) Submit(context.Context, approval.SubmitParams) (*approval.SubmitResult, error) {
	return nil, e.err
}

func (e errListApprovalRepo) ListForStage(context.Context, uuid.UUID) ([]*approval.Approval, error) {
	return nil, e.err
}

// (a) multi-env spec + an approve carrying --environment=prod → "prod": the
// operator's actual choice, not the schema-order-derived first entry.
func TestDeployEnvironmentForStage_ApprovedEnvironment(t *testing.T) {
	ar := newFakeApprovalRepo()
	s, stage, runRow := newStageEnvServer(t, ar)
	seedStageEnvApproval(ar, stage.ID, approval.DecisionApprove, "--environment=prod")
	if got := s.deployEnvironmentForStage(context.Background(), runRow.ID, stage.ID); got != "prod" {
		t.Errorf("deployEnvironmentForStage = %q, want %q (the approved environment)", got, "prod")
	}
}

// (b) no approval recorded → fallback "staging" (single-environment correctness
// preserved: the derivation still labels the deploy).
func TestDeployEnvironmentForStage_NoApproval_FallsBack(t *testing.T) {
	ar := newFakeApprovalRepo()
	s, stage, runRow := newStageEnvServer(t, ar)
	if got := s.deployEnvironmentForStage(context.Background(), runRow.ID, stage.ID); got != "staging" {
		t.Errorf("deployEnvironmentForStage = %q, want %q (spec-derivation fallback)", got, "staging")
	}
}

// (c) an approve with no --environment= flag → fallback (the flag-less approve
// records no explicit choice).
func TestDeployEnvironmentForStage_NoEnvironmentFlag_FallsBack(t *testing.T) {
	ar := newFakeApprovalRepo()
	s, stage, runRow := newStageEnvServer(t, ar)
	seedStageEnvApproval(ar, stage.ID, approval.DecisionApprove, "lgtm")
	if got := s.deployEnvironmentForStage(context.Background(), runRow.ID, stage.ID); got != "staging" {
		t.Errorf("deployEnvironmentForStage = %q, want %q (no flag → fallback)", got, "staging")
	}
}

// (d) a REJECT decision carrying --environment=prod plus a flag-less approve →
// fallback, never "prod". The Decision != approve filter is the control; a
// rejected environment must never label the deploy.
func TestDeployEnvironmentForStage_RejectCommentIgnored(t *testing.T) {
	ar := newFakeApprovalRepo()
	s, stage, runRow := newStageEnvServer(t, ar)
	// Ascending order: the reject (carrying the flag) precedes a flag-less
	// approve, so only the Decision filter — not iteration order — keeps "prod"
	// out.
	seedStageEnvApproval(ar, stage.ID, approval.DecisionReject, "--environment=prod")
	seedStageEnvApproval(ar, stage.ID, approval.DecisionApprove, "lgtm")
	if got := s.deployEnvironmentForStage(context.Background(), runRow.ID, stage.ID); got != "staging" {
		t.Errorf("deployEnvironmentForStage = %q, want %q (reject comment must be ignored)", got, "staging")
	}
}

// (e) an approve naming an env NOT in allowed_environments (seeded by
// construction — the gate would refuse it) → fallback, never the bogus value.
// The sliceContains membership re-check is the control.
func TestDeployEnvironmentForStage_DisallowedApprovalEnv_FallsBack(t *testing.T) {
	ar := newFakeApprovalRepo()
	s, stage, runRow := newStageEnvServer(t, ar)
	seedStageEnvApproval(ar, stage.ID, approval.DecisionApprove, "--environment=canary")
	if got := s.deployEnvironmentForStage(context.Background(), runRow.ID, stage.ID); got != "staging" {
		t.Errorf("deployEnvironmentForStage = %q, want %q (a non-member env must fall back, never publish %q)", got, "staging", "canary")
	}
}

// (f) last-approve-wins: whichever later APPROVE carries a flag wins. Two
// sub-cases — a later flag beating an earlier flag-less approve, and two flagged
// approves where the last wins.
func TestDeployEnvironmentForStage_LastApproveWins(t *testing.T) {
	t.Run("later_flag_beats_earlier_flagless", func(t *testing.T) {
		ar := newFakeApprovalRepo()
		s, stage, runRow := newStageEnvServer(t, ar)
		// An empty comment seeds a NIL Comment — exercises the a.Comment == nil
		// guard (a nil-comment approve is skipped, not deref-panicked).
		seedStageEnvApproval(ar, stage.ID, approval.DecisionApprove, "")
		seedStageEnvApproval(ar, stage.ID, approval.DecisionApprove, "--environment=prod")
		if got := s.deployEnvironmentForStage(context.Background(), runRow.ID, stage.ID); got != "prod" {
			t.Errorf("deployEnvironmentForStage = %q, want %q (later flagged approve wins)", got, "prod")
		}
	})
	t.Run("last_of_two_flags_wins", func(t *testing.T) {
		ar := newFakeApprovalRepo()
		s, stage, runRow := newStageEnvServer(t, ar)
		seedStageEnvApproval(ar, stage.ID, approval.DecisionApprove, "--environment=staging")
		seedStageEnvApproval(ar, stage.ID, approval.DecisionApprove, "--environment=prod")
		if got := s.deployEnvironmentForStage(context.Background(), runRow.ID, stage.ID); got != "prod" {
			t.Errorf("deployEnvironmentForStage = %q, want %q (last approve wins)", got, "prod")
		}
	})
}

// (g) a nil ApprovalRepo → fallback, no panic (the resolve path is reachable in
// server constructions that wire no ApprovalRepo).
func TestDeployEnvironmentForStage_NilApprovalRepo_FallsBack(t *testing.T) {
	s, stage, runRow := newStageEnvServer(t, nil)
	if got := s.deployEnvironmentForStage(context.Background(), runRow.ID, stage.ID); got != "staging" {
		t.Errorf("deployEnvironmentForStage = %q, want %q (nil ApprovalRepo → fallback)", got, "staging")
	}
}

// (h) a ListForStage error → fallback, no error propagated (best-effort read).
func TestDeployEnvironmentForStage_ListError_FallsBack(t *testing.T) {
	s, stage, runRow := newStageEnvServer(t, errListApprovalRepo{err: context.DeadlineExceeded})
	if got := s.deployEnvironmentForStage(context.Background(), runRow.ID, stage.ID); got != "staging" {
		t.Errorf("deployEnvironmentForStage = %q, want %q (ListForStage error → fallback)", got, "staging")
	}
}

// (i) lastWinsAllowedEnvironments folds a duplicate-kind stage to its LAST
// non-empty entry ([prod]) and an absent-constraint stage to nil — the fold the
// gate admits against and the record re-checks membership against.
func TestLastWinsAllowedEnvironments(t *testing.T) {
	parsed, err := spec.ParseBytes([]byte(deploySpecLegacyDuplicateEnvs))
	if err != nil {
		t.Fatalf("ParseBytes(deploySpecLegacyDuplicateEnvs): %v", err)
	}
	dup := parsed.Workflows["release"].Stages[0]
	if got := lastWinsAllowedEnvironments(dup); len(got) != 1 || got[0] != "prod" {
		t.Errorf("lastWinsAllowedEnvironments(duplicate-kind) = %v, want [prod] (last-wins)", got)
	}
	if got := lastWinsAllowedEnvironments(spec.Stage{}); got != nil {
		t.Errorf("lastWinsAllowedEnvironments(no constraints) = %v, want nil", got)
	}
}

// (j) FAIL-CLOSED on an unresolvable spec (E23.18 / #2324 high/security). When
// deploySpecStageForRun cannot resolve the deploy stage at record time — here an
// ABSENT (empty) cached spec, which trips its len(WorkflowSpec)==0 guard — the
// membership re-check cannot run. The operator-supplied --environment= value is
// operator-controlled input on an audit surface, so a validation that cannot run
// MUST fail closed: deployApprovedEnvironment returns "" rather than publishing
// the unchecked "prod". The seeded approve is gate-admitted BY CONSTRUCTION (in
// production it can only exist because checkDeployPreflight validated it against
// the immutable cached spec), so the RED of the counterfactual lands on the
// behavioral assertion, not on a fixture-setup guard.
//
// COUNTERFACTUAL: deleting the `if !ok { return "" }` guard in
// deployApprovedEnvironment reddens the deployApprovedEnvironment assertion below
// — the fail-open path publishes "prod" (the unchecked approved value) instead of
// "". The end-to-end deployEnvironmentForStage assertion then confirms no
// unverified environment reaches the deploy record.
func TestDeployApprovedEnvironment_UnresolvableSpec_FailsClosed(t *testing.T) {
	rr := newApprovalRunRepo()
	// An ABSENT cached spec: deploySpecStageForRun returns ok=false at its
	// len(WorkflowSpec)==0 guard, so the membership re-check cannot run.
	stage, runRow := seedDeployRun(rr, "release", "")
	ar := newFakeApprovalRepo()
	s := New(Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      rr,
		ApprovalRepo: ar,
	})
	seedStageEnvApproval(ar, stage.ID, approval.DecisionApprove, "--environment=prod")

	// deployApprovedEnvironment fails closed: the unchecked value is not published.
	if got := s.deployApprovedEnvironment(context.Background(), runRow.ID, stage.ID); got != "" {
		t.Errorf("deployApprovedEnvironment = %q, want %q (unresolvable spec must fail closed, never publish the unchecked approved value)", got, "")
	}
	// End-to-end: the published audit-surface label is empty too (the derivation
	// fallback also cannot resolve the absent spec), so no unverified environment
	// reaches the deploy record.
	if got := s.deployEnvironmentForStage(context.Background(), runRow.ID, stage.ID); got != "" {
		t.Errorf("deployEnvironmentForStage = %q, want %q (no unverified environment reaches the record)", got, "")
	}
}

// TestDeployEnvironment_TwoDeployStages_GateAndRecordAgree pins binding
// condition 2: on a spec with TWO deploy stages carrying DIFFERENT
// allowed_environments, the approval gate and the deploy record resolve from the
// SAME (first) stage, so they AGREE about which allow-list applies. First-stage
// keying is the shared, pre-existing behavior this test CHARACTERIZES — it is
// NOT an endorsement of first-stage keying (a latent multi-deploy-stage defect
// tracked separately, out of #2324 scope). The point is only that after the
// selection was extracted into one helper the gate and the record cannot key on
// different stages.
func TestDeployEnvironment_TwoDeployStages_GateAndRecordAgree(t *testing.T) {
	s, ar, rr, au := newApprovalServer(t)
	stage, runRow := seedDeployRun(rr, "release", twoDeployStagesSpec)

	// The gate keys on the FIRST deploy stage, whose allow-list is [staging].
	// "prod" belongs only to the SECOND deploy stage, so the gate REFUSES it —
	// proving the gate is not keying on the second stage. A pre-Submit refusal
	// records no approval row.
	wProd := submitApproval(t, s, stage.ID, `{"decision":"approve","comment":"--environment=prod"}`)
	assertDeployRefused(t, wProd, rr, au, stage, "deploy_environment_not_allowed")

	// "staging" (the first stage's allow-list) is admitted and recorded.
	wStaging := submitApproval(t, s, stage.ID, `{"decision":"approve","comment":"--environment=staging"}`)
	if wStaging.Code != http.StatusOK {
		t.Fatalf("staging approval status = %d, want 200 (first deploy stage permits staging):\n%s", wStaging.Code, wStaging.Body.String())
	}

	// The record reads the SAME recorded approval and re-checks it against the
	// SAME first-stage allow-list, so it reports "staging" — agreeing with the
	// gate that just admitted it. It cannot report "prod" (the second stage's
	// env), because there is one implementation of the stage selection.
	if got := s.deployEnvironmentForStage(context.Background(), runRow.ID, stage.ID); got != "staging" {
		t.Errorf("deployEnvironmentForStage = %q, want %q (gate and record resolve from the SAME first stage)", got, "staging")
	}
	_ = ar
}
