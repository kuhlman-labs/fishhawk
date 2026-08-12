package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/approval"
	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// deploySpecMultiEnv is a deploy stage declaring a multi-environment allow-list
// [staging, prod]. deployEnvironmentFromSpecStage's first-wins fallback reports
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
// [prod]. Since #2642 (E23.19) the gate, the record, and the trigger each key on
// the deploy stage that MATCHES the stage row being acted on (by its deploy
// ordinal), so the second deploy stage gates and labels against [prod], not the
// first stage's [staging].
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
// deploySpecStageForStage cannot resolve the deploy stage at record time — here
// an ABSENT (empty) cached spec, which trips resolveDeploySpecStage's
// len(WorkflowSpec)==0 guard — the
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
	// An ABSENT cached spec: deploySpecStageForStage returns ok=false at
	// resolveDeploySpecStage's len(WorkflowSpec)==0 guard, so the membership
	// re-check cannot run.
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

// TestDeployEnvironment_TwoDeployStages_GateAndRecordAgree pins the #2642 (E23.19)
// contract on a spec with TWO deploy stages carrying DIFFERENT
// allowed_environments: the gate and the record agree ON THE ROW BEING GATED,
// for BOTH rows. This REPLACES the earlier first-match characterization — the
// point was inverted by acceptance criteria 1-2, which require each stage to
// gate and label against its OWN allow-list. Because both sides reach the single
// resolveDeploySpecStage chokepoint keyed on the row's deploy ordinal, the record
// cannot key on a different stage than the gate that admitted the approval.
func TestDeployEnvironment_TwoDeployStages_GateAndRecordAgree(t *testing.T) {
	// FIRST row: gate admits staging (its own allow-list), record labels staging.
	t.Run("first_row_staging", func(t *testing.T) {
		s, _, rr, au := newApprovalServer(t)
		first, runRow := seedDeployRun(rr, "release", twoDeployStagesSpec)
		seedSecondDeployStage(rr, first, run.StageStateAwaitingDeployApproval)

		wProd := submitApproval(t, s, first.ID, `{"decision":"approve","comment":"--environment=prod"}`)
		assertDeployRefused(t, wProd, rr, au, first, "deploy_environment_not_allowed")

		wStaging := submitApproval(t, s, first.ID, `{"decision":"approve","comment":"--environment=staging"}`)
		if wStaging.Code != http.StatusOK {
			t.Fatalf("staging approval status = %d, want 200 (first deploy stage permits staging):\n%s", wStaging.Code, wStaging.Body.String())
		}
		if got := s.deployEnvironmentForStage(context.Background(), runRow.ID, first.ID); got != "staging" {
			t.Errorf("deployEnvironmentForStage(first) = %q, want %q (gate and record agree on the first row)", got, "staging")
		}
	})

	// SECOND row: gate admits prod (its own allow-list), record labels prod.
	t.Run("second_row_prod", func(t *testing.T) {
		s, _, rr, au := newApprovalServer(t)
		first, runRow := seedDeployRun(rr, "release", twoDeployStagesSpec)
		second := seedSecondDeployStage(rr, first, run.StageStateAwaitingDeployApproval)

		wStaging := submitApproval(t, s, second.ID, `{"decision":"approve","comment":"--environment=staging"}`)
		assertDeployRefused(t, wStaging, rr, au, second, "deploy_environment_not_allowed")

		wProd := submitApproval(t, s, second.ID, `{"decision":"approve","comment":"--environment=prod"}`)
		if wProd.Code != http.StatusOK {
			t.Fatalf("prod approval status = %d, want 200 (second deploy stage permits prod):\n%s", wProd.Code, wProd.Body.String())
		}
		if got := s.deployEnvironmentForStage(context.Background(), runRow.ID, second.ID); got != "prod" {
			t.Errorf("deployEnvironmentForStage(second) = %q, want %q (gate and record agree on the second row)", got, "prod")
		}
	})
}

// TestDeployGate_SecondDeployStage_UsesOwnAllowList is the primary #2642 gate
// proof: the SECOND deploy row is gated against its OWN allow-list ([prod]), not
// the first stage's [staging]. --environment=prod is admitted and advances the
// stage; --environment=staging is refused 422 deploy_environment_not_allowed
// with a deploy_preflight_refused audit. This is counterfactual (a)'s vehicle:
// restoring first-match inside deployStageForRunStage reddens it (prod refused,
// staging admitted).
func TestDeployGate_SecondDeployStage_UsesOwnAllowList(t *testing.T) {
	t.Run("prod_admitted", func(t *testing.T) {
		s, _, rr, _ := newApprovalServer(t)
		first, _ := seedDeployRun(rr, "release", twoDeployStagesSpec)
		second := seedSecondDeployStage(rr, first, run.StageStateAwaitingDeployApproval)
		w := submitApproval(t, s, second.ID, `{"decision":"approve","comment":"--environment=prod"}`)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (second deploy stage permits prod):\n%s", w.Code, w.Body.String())
		}
		cur, _ := rr.GetStage(context.Background(), second.ID)
		if cur.State == run.StageStateAwaitingDeployApproval {
			t.Errorf("second deploy stage still parked at %q; the approval should have advanced it", cur.State)
		}
	})
	t.Run("staging_refused", func(t *testing.T) {
		s, _, rr, au := newApprovalServer(t)
		first, _ := seedDeployRun(rr, "release", twoDeployStagesSpec)
		second := seedSecondDeployStage(rr, first, run.StageStateAwaitingDeployApproval)
		w := submitApproval(t, s, second.ID, `{"decision":"approve","comment":"--environment=staging"}`)
		assertDeployRefused(t, w, rr, au, second, "deploy_environment_not_allowed")
	})
}

// TestDeployGate_FirstDeployStage_UnchangedUnderTwoDeployStages proves the fix is
// stage-scoped, not "last match wins": with the second row present, the FIRST row
// still admits staging and refuses prod.
func TestDeployGate_FirstDeployStage_UnchangedUnderTwoDeployStages(t *testing.T) {
	t.Run("staging_admitted", func(t *testing.T) {
		s, _, rr, _ := newApprovalServer(t)
		first, _ := seedDeployRun(rr, "release", twoDeployStagesSpec)
		seedSecondDeployStage(rr, first, run.StageStateAwaitingDeployApproval)
		w := submitApproval(t, s, first.ID, `{"decision":"approve","comment":"--environment=staging"}`)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (first deploy stage permits staging):\n%s", w.Code, w.Body.String())
		}
	})
	t.Run("prod_refused", func(t *testing.T) {
		s, _, rr, au := newApprovalServer(t)
		first, _ := seedDeployRun(rr, "release", twoDeployStagesSpec)
		seedSecondDeployStage(rr, first, run.StageStateAwaitingDeployApproval)
		w := submitApproval(t, s, first.ID, `{"decision":"approve","comment":"--environment=prod"}`)
		assertDeployRefused(t, w, rr, au, first, "deploy_environment_not_allowed")
	})
}

// TestDeployEnvironment_SecondDeployStage_RecordLabelsFromOwnStage is binding
// condition 2's record-WRITING-boundary proof (AC2): it drives the REAL deploy
// record path (ResolveDeploymentFromPollState) for a deploy of the SECOND stage
// and asserts on the SHIPPED record — the persisted deployment artifact content
// AND the deployment_outcome_recorded audit payload — that the label is "prod"
// (the second stage's own environment), never "staging". A unit on the resolver
// would only be the setup; this asserts the stageID threading and record
// publication end to end.
//
// It is ALSO the AC3 shared-helper counterfactual vehicle: deleting the
// record-side resolveDeploySpecStage call and re-deriving the stage locally by
// first-match reddens both the artifact and the audit assertion (the label
// becomes "staging").
func TestDeployEnvironment_SecondDeployStage_RecordLabelsFromOwnStage(t *testing.T) {
	rr := newApprovalRunRepo()
	first, runRow := seedDeployRun(rr, "release", twoDeployStagesSpec)
	second := seedSecondDeployStage(rr, first, run.StageStateAwaitingDeployment)
	ar := newFakeArtifactRepo()
	au := newApprovalAuditFake()
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, ArtifactRepo: ar, AuditRepo: au})

	// No explicit --environment= approval → the record derives the label from
	// the SECOND stage's own allow-list ([prod]) via the spec-derivation
	// fallback. Under first-match it would derive "staging" from the first stage.
	if err := s.ResolveDeploymentFromPollState(context.Background(), runRow.ID, second.ID,
		run.DeployOutcomeSucceeded, "deadbeef", nil); err != nil {
		t.Fatalf("ResolveDeploymentFromPollState: %v", err)
	}

	// (AC2) the SHIPPED deployment artifact carries "prod".
	arts, err := ar.ListForStage(context.Background(), second.ID)
	if err != nil {
		t.Fatalf("ListForStage: %v", err)
	}
	var body deploymentBody
	found := false
	for _, a := range arts {
		if a.Kind == artifact.KindDeployment {
			if uerr := json.Unmarshal(a.Content, &body); uerr != nil {
				t.Fatalf("unmarshal deployment artifact: %v", uerr)
			}
			found = true
		}
	}
	if !found {
		t.Fatal("no deployment artifact persisted for the second stage")
	}
	if body.Environment != "prod" {
		t.Errorf("deployment artifact environment = %q, want %q (second stage's own env)", body.Environment, "prod")
	}

	// (AC2) the deployment_outcome_recorded audit payload carries "prod".
	p := auditPayload(t, au, CategoryDeploymentOutcomeRecorded)
	if p["environment"] != "prod" {
		t.Errorf("deployment_outcome_recorded environment = %v, want %q (second stage's own env)", p["environment"], "prod")
	}
}

// TestDeployEnvironmentForStage_MissingRunOrStage_ReturnsEmpty covers
// deploySpecStageForStage's best-effort lookup-failure arms: an absent run
// (GetRun errors) and an absent stage row (GetStage errors) each resolve to ""
// rather than a label or a panic.
func TestDeployEnvironmentForStage_MissingRunOrStage_ReturnsEmpty(t *testing.T) {
	// (1) GetRun errors: no run seeded at all.
	t.Run("missing_run", func(t *testing.T) {
		rr := newApprovalRunRepo()
		s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr})
		if got := s.deployEnvironmentForStage(context.Background(), uuid.New(), uuid.New()); got != "" {
			t.Errorf("deployEnvironmentForStage = %q, want %q (missing run → empty)", got, "")
		}
	})
	// (2) GetStage errors: the run is seeded but no stage row exists for the id.
	t.Run("missing_stage", func(t *testing.T) {
		rr := newApprovalRunRepo()
		_, runRow := seedDeployRun(rr, "release", twoDeployStagesSpec)
		s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr})
		if got := s.deployEnvironmentForStage(context.Background(), runRow.ID, uuid.New()); got != "" {
			t.Errorf("deployEnvironmentForStage = %q, want %q (missing stage → empty)", got, "")
		}
	})
}

// TestDeployPreflight_StageSelectionUnresolvable_FailsClosed asserts the gate
// fails closed (422 deploy_preflight_unevaluable + exactly one
// deploy_preflight_refused audit) on each named can't-evaluate mode. Bad states
// are seeded BY CONSTRUCTION so a counterfactual RED (deleting a refusal arm)
// lands on the behavioral assertion.
func TestDeployPreflight_StageSelectionUnresolvable_FailsClosed(t *testing.T) {
	// (1) ListStagesForRun errors → the run's stage rows cannot be listed.
	t.Run("list_stages_errors", func(t *testing.T) {
		s, _, rr, au := newApprovalServer(t)
		stage, _ := seedDeployRun(rr, "release", twoDeployStagesSpec)
		rr.listStagesErr = errors.New("boom")
		w := submitApproval(t, s, stage.ID, `{"decision":"approve","comment":"--environment=staging"}`)
		assertDeployRefused(t, w, rr, au, stage, "deploy_preflight_unevaluable")
	})
	// (2) the row's deploy ordinal exceeds the spec's deploy-stage count. The
	// single-deploy fixture carries ONE deploy stage; a SECOND deploy row (deploy
	// ordinal 1) has no spec counterpart, so the gate on that row fails closed.
	t.Run("ordinal_exceeds_spec", func(t *testing.T) {
		s, _, rr, au := newApprovalServer(t)
		first, _ := seedDeployRun(rr, "release", deploySpecMultiEnv)
		second := seedSecondDeployStage(rr, first, run.StageStateAwaitingDeployApproval)
		w := submitApproval(t, s, second.ID, `{"decision":"approve","comment":"--environment=staging"}`)
		assertDeployRefused(t, w, rr, au, second, "deploy_preflight_unevaluable")
	})
	// (3) the spec declares no deploy stage at all.
	t.Run("no_deploy_stage_in_spec", func(t *testing.T) {
		s, _, rr, au := newApprovalServer(t)
		stage, _ := seedDeployRun(rr, "release", deploySpecNoDeployStage)
		w := submitApproval(t, s, stage.ID, `{"decision":"approve","comment":"--environment=staging"}`)
		assertDeployRefused(t, w, rr, au, stage, "deploy_preflight_unevaluable")
	})
}

// TestDeployEnvironmentForStage_StageSelectionUnresolvable_ReturnsEmpty asserts
// the RECORD side returns "" (never a label drawn from another stage) on the two
// selection-failure modes, so no operator-supplied environment reaches the deploy
// record when the stage it was gated against cannot be re-confirmed. Each case
// seeds a gate-admitted `--environment=prod` approval BY CONSTRUCTION, so the
// fail-closed control under test is deployApprovedEnvironment's `if !ok` guard —
// the #2324 membership-recheck guard, extended here to the #2642 stage-selection
// failure modes. This is counterfactual (d)'s vehicle: deleting that ok guard
// publishes the unchecked "prod" (the membership recheck folds a zero allow-list
// to nil and admits the value) instead of "".
func TestDeployEnvironmentForStage_StageSelectionUnresolvable_ReturnsEmpty(t *testing.T) {
	// (1) ListStagesForRun errors → the run's stage rows cannot be listed.
	t.Run("list_stages_errors", func(t *testing.T) {
		rr := newApprovalRunRepo()
		stage, runRow := seedDeployRun(rr, "release", twoDeployStagesSpec)
		ar := newFakeApprovalRepo()
		seedStageEnvApproval(ar, stage.ID, approval.DecisionApprove, "--environment=prod")
		rr.listStagesErr = errors.New("boom")
		s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, ApprovalRepo: ar})
		if got := s.deployEnvironmentForStage(context.Background(), runRow.ID, stage.ID); got != "" {
			t.Errorf("deployEnvironmentForStage = %q, want %q (row listing failed → fail closed, never publish the unchecked value)", got, "")
		}
	})
	// (2) the row's deploy ordinal exceeds the spec's deploy-stage count. The
	// single-deploy fixture carries ONE deploy stage; a SECOND deploy row's
	// ordinal (1) has no spec counterpart, so the stage cannot be re-confirmed.
	t.Run("ordinal_exceeds_spec", func(t *testing.T) {
		rr := newApprovalRunRepo()
		first, runRow := seedDeployRun(rr, "release", deploySpecMultiEnv)
		second := seedSecondDeployStage(rr, first, run.StageStateAwaitingDeployment)
		ar := newFakeApprovalRepo()
		seedStageEnvApproval(ar, second.ID, approval.DecisionApprove, "--environment=prod")
		s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, ApprovalRepo: ar})
		if got := s.deployEnvironmentForStage(context.Background(), runRow.ID, second.ID); got != "" {
			t.Errorf("deployEnvironmentForStage = %q, want %q (ordinal beyond spec → fail closed, never publish the unchecked value)", got, "")
		}
	})
}
