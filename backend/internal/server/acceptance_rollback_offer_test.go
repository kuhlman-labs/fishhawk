package server

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// postDeployTriageFixture is a release-shaped run (E35.3 / #1600): one stage
// per entry of the shape, at Sequence == index, every stage succeeded, the run
// still running, its cached spec carrying a github_actions deploy delegate and
// an installation id — so the rollback dispatch WOULD succeed against the
// wired GitHub stub if triage ever called it (the never-auto-fires control).
type postDeployTriageFixture struct {
	s      *Server
	rr     *promptRunRepo
	ar     *fakeArtifactRepo
	au     *auditFake
	stub   *deployTriggerGitHub
	runID  uuid.UUID
	stages []*run.Stage
}

func newPostDeployTriageFixture(t *testing.T, shape ...run.StageType) *postDeployTriageFixture {
	t.Helper()
	f := &postDeployTriageFixture{
		rr:    newPromptRunRepo(),
		ar:    newFakeArtifactRepo(),
		au:    newAuditFake(),
		runID: uuid.New(),
	}
	f.rr.getRuns[f.runID] = &run.Run{
		ID: f.runID, Repo: "kuhlman-labs/example", WorkflowID: "release", WorkflowSHA: "sha",
		WorkflowSpec: []byte(deploySpecNoConstraints), InstallationID: instID(99), State: run.StateRunning,
	}
	for i, typ := range shape {
		st := &run.Stage{ID: uuid.New(), RunID: f.runID, Sequence: i, Type: typ, State: run.StageStateSucceeded}
		f.rr.getStages[st.ID] = st
		f.stages = append(f.stages, st)
	}
	f.rr.stagesByRunID = map[uuid.UUID][]*run.Stage{f.runID: f.stages}
	stub, gh := newDeployTriggerGitHub(t)
	f.stub = stub
	f.s = New(Config{Addr: "127.0.0.1:0", RunRepo: f.rr, ArtifactRepo: f.ar, AuditRepo: f.au, GitHub: gh})
	return f
}

// stageOf returns the fixture's i-th stage (by sequence).
func (f *postDeployTriageFixture) stageOf(i int) *run.Stage { return f.stages[i] }

// last returns the highest-sequence stage — the acceptance stage in every
// shape these tests build.
func (f *postDeployTriageFixture) last() *run.Stage { return f.stages[len(f.stages)-1] }

// storeHandle persists a forward deployment record carrying handle on stage.
func (f *postDeployTriageFixture) storeHandle(t *testing.T, stage *run.Stage, handle string) *artifact.Artifact {
	t.Helper()
	a := deploymentArtifact(t, stage.ID, time.Now().UTC(), 0, forwardDeployment(handle))
	f.ar.all = append(f.ar.all, a)
	return a
}

// triage runs triageAcceptanceFailure for the fixture's acceptance stage and
// returns the realized disposition plus the decoded acceptance_triage_decided
// payload.
func (f *postDeployTriageFixture) triage(t *testing.T, acc acceptanceBody) (string, map[string]any) {
	t.Helper()
	disposition := f.s.triageAcceptanceFailure(context.Background(), f.runID, f.last(), acc, uuid.NewString())
	e := findAppendedByCategory(t, f.au, CategoryAcceptanceTriageDecided)
	var p map[string]any
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		t.Fatalf("decode triage payload: %v\n%s", err, e.Payload)
	}
	if p["disposition"] != disposition {
		t.Fatalf("payload disposition %v != returned %q", p["disposition"], disposition)
	}
	return disposition, p
}

var (
	class1ErrorVerdict      = acceptanceBody{Verdict: "failed", FailureMode: acceptanceFailureError}
	class4UnitemizedVerdict = acceptanceBody{Verdict: "failed", FailureMode: "assertion_fail"}
)

// (C7) TestTriageAcceptanceFailure_PostDeploy_OffersRollback: a class-1
// (failure_mode=error) and a class-4 (unitemized) failed verdict on a
// release-shaped run [deploy, acceptance] record rollback_offered, and the
// payload's rollback_offer names the deploy stage, the deployment artifact and
// the stored handle. Without the branch, class 1 reaches routeAcceptanceClass1,
// finds no implement stage and records fixup_unavailable_paged; class 4 records
// paged; neither carries rollback_offer.
func TestTriageAcceptanceFailure_PostDeploy_OffersRollback(t *testing.T) {
	for _, tc := range []struct {
		name  string
		acc   acceptanceBody
		class string
	}{
		{"class1_error", class1ErrorVerdict, acceptanceClass1},
		{"class4_unitemized", class4UnitemizedVerdict, acceptanceClass4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPostDeployTriageFixture(t, run.StageTypeDeploy, run.StageTypeAcceptance)
			deploy := f.stageOf(0)
			stored := f.storeHandle(t, deploy, "rev-abc")

			disposition, p := f.triage(t, tc.acc)
			if disposition != acceptanceDispositionRollbackOffered {
				t.Fatalf("disposition = %q, want rollback_offered (payload %v)", disposition, p)
			}
			if p["class"] != tc.class {
				t.Errorf("class = %v, want %s", p["class"], tc.class)
			}
			offer, ok := p["rollback_offer"].(map[string]any)
			if !ok {
				t.Fatalf("payload carries no rollback_offer object: %v", p)
			}
			if offer["deploy_stage_id"] != deploy.ID.String() ||
				offer["deployment_artifact_id"] != stored.ID.String() ||
				offer["rollback_handle"] != "rev-abc" {
				t.Errorf("rollback_offer = %v, want {deploy_stage_id:%s deployment_artifact_id:%s rollback_handle:rev-abc}",
					offer, deploy.ID, stored.ID)
			}
			if !acceptanceDispositionPages(disposition) {
				t.Errorf("acceptanceDispositionPages(%q) = false; the offer must page a human", disposition)
			}
		})
	}
}

// (C8) TestTriageAcceptanceFailure_PostDeploy_NeverDispatches is the
// done-means operator gate: the offer NEVER fires the rollback. The fixture
// wires a GitHub stub, a github_actions delegate and an installation id, so a
// branch that called dispatchRollback would succeed and be observed here as a
// dispatch hit. State is read back AFTER triage (the control's effect is the
// ABSENCE of committed state): no dispatch, no deployment_rollback_initiated,
// no stage transition, deploy still succeeded.
func TestTriageAcceptanceFailure_PostDeploy_NeverDispatches(t *testing.T) {
	f := newPostDeployTriageFixture(t, run.StageTypeDeploy, run.StageTypeAcceptance)
	deploy := f.stageOf(0)
	f.storeHandle(t, deploy, "rev-abc")

	disposition, _ := f.triage(t, class1ErrorVerdict)
	if disposition != acceptanceDispositionRollbackOffered {
		t.Fatalf("disposition = %q, want rollback_offered", disposition)
	}
	if f.stub.dispatchHits != 0 {
		t.Errorf("GitHub dispatch hits = %d, want 0 — triage must never auto-fire the rollback", f.stub.dispatchHits)
	}
	if n := countAppendedByCategory(f.au, CategoryDeploymentRollbackInitiated); n != 0 {
		t.Errorf("deployment_rollback_initiated entries = %d, want 0", n)
	}
	if n := len(f.rr.transitionStageCalls); n != 0 {
		t.Errorf("stage transitions = %d (%+v), want 0 — the offer takes no state transition", n, f.rr.transitionStageCalls)
	}
	cur, err := f.rr.GetStage(context.Background(), deploy.ID)
	if err != nil {
		t.Fatalf("read back deploy: %v", err)
	}
	if cur.State != run.StageStateSucceeded {
		t.Errorf("deploy stage state = %q, want succeeded (unchanged)", cur.State)
	}
}

// (C9) TestTriageAcceptanceFailure_MultiDeploy_Pages: on [deployA, deployB,
// acceptance] the verified deploy (B, nearest before the acceptance) is not the
// stage the run-scoped rollback endpoint resolves (A, #2642), so triage pages
// instead of offering a rollback that would revert a different deploy. Both
// deploys carry a stored handle so the mismatch check is the only thing
// between this fixture and an offer naming B.
func TestTriageAcceptanceFailure_MultiDeploy_Pages(t *testing.T) {
	f := newPostDeployTriageFixture(t, run.StageTypeDeploy, run.StageTypeDeploy, run.StageTypeAcceptance)
	f.storeHandle(t, f.stageOf(0), "rev-a")
	f.storeHandle(t, f.stageOf(1), "rev-b")

	disposition, p := f.triage(t, class4UnitemizedVerdict)
	if disposition != acceptanceDispositionPaged {
		t.Fatalf("disposition = %q, want paged on a multi-deploy run (payload %v)", disposition, p)
	}
	if _, has := p["rollback_offer"]; has {
		t.Errorf("payload carries rollback_offer on a multi-deploy run: %v", p)
	}
	if reason, _ := p["reason"].(string); !strings.Contains(reason, "#2642") {
		t.Errorf("reason = %q, want it to name the run-scoped rollback limitation (#2642)", reason)
	}
	if f.stub.dispatchHits != 0 {
		t.Errorf("GitHub dispatch hits = %d, want 0", f.stub.dispatchHits)
	}
}

// (C10 / approval condition 1) TestTriageAcceptanceFailure_MixedImplementDeploy_KeepsFixupRoute:
// a mixed [plan, implement, deploy, review, acceptance] workflow — an implement
// stage ahead of the deploy — keeps today's routeAcceptanceClass1 fix-up route
// (fixup_dispatched, implement re-opened) and records NO rollback_offered. The
// offer fires only on the release shape (no implement stage), where class 1
// would otherwise degrade to fixup_unavailable_paged.
func TestTriageAcceptanceFailure_MixedImplementDeploy_KeepsFixupRoute(t *testing.T) {
	s, rr, ar, au, _, runID, implementStageID, reviewStageID, acceptanceStageID, priv := newAcceptanceTriageServer(t)
	deploy := &run.Stage{ID: uuid.New(), RunID: runID, Type: run.StageTypeDeploy, State: run.StageStateSucceeded}
	rr.getStages[deploy.ID] = deploy
	order := []*run.Stage{
		rr.stagesByRunID[runID][0], rr.getStages[implementStageID], deploy,
		rr.getStages[reviewStageID], rr.getStages[acceptanceStageID],
	}
	for i, st := range order {
		st.Sequence = i
	}
	rr.stagesByRunID[runID] = order
	ar.all = append(ar.all, deploymentArtifact(t, deploy.ID, time.Now().UTC(), 0, forwardDeployment("rev-abc")))

	body := failedAcceptanceBytes(t, "error", []acceptanceCriterionResult{{ID: "ac-create", Result: "failed", Observed: "500"}})
	w := shipAcceptanceRequest(t, s, runID, acceptanceStageID, priv, body, "")
	if w.Code != 201 {
		t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
	}
	payload := triagePayload(t, au)
	if !strings.Contains(payload, `"disposition":"fixup_dispatched"`) {
		t.Errorf("triage payload = %s, want the class-1 fix-up route kept (fixup_dispatched)", payload)
	}
	if strings.Contains(payload, "rollback_offer") {
		t.Errorf("triage payload records a rollback offer on a mixed implement+deploy workflow: %s", payload)
	}
	if got := rr.getStages[implementStageID].State; got != run.StageStatePending {
		t.Errorf("implement state = %q, want pending (the fix-up re-opened it)", got)
	}
}

// (C10) TestTriageAcceptanceFailure_NoDeploy_Unchanged: a run with NO deploy
// stage ahead of the acceptance keeps every existing disposition. The
// no-implement [review, acceptance] shape isolates the "no earlier deploy"
// check from the implement-stage check: class 1 still records the existing
// fixup_unavailable_paged and class 4 the existing paged, neither with an offer.
func TestTriageAcceptanceFailure_NoDeploy_Unchanged(t *testing.T) {
	for _, tc := range []struct {
		name string
		acc  acceptanceBody
		want string
	}{
		{"class1", class1ErrorVerdict, acceptanceDispositionFixupUnavailable},
		{"class4", class4UnitemizedVerdict, acceptanceDispositionPaged},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPostDeployTriageFixture(t, run.StageTypeReview, run.StageTypeAcceptance)
			disposition, p := f.triage(t, tc.acc)
			if disposition != tc.want {
				t.Errorf("disposition = %q, want %q", disposition, tc.want)
			}
			if _, has := p["rollback_offer"]; has {
				t.Errorf("payload carries rollback_offer without a deploy stage: %v", p)
			}
		})
	}
}

// (C11) TestTriageAcceptanceFailure_PostDeploy_OfferNotSuppressedByRerunBudget:
// with the re-run budget already at the cap, a post-deploy class-1 verdict
// still records rollback_offered — the offer is not an auto-route, so the
// budget must not suppress it.
func TestTriageAcceptanceFailure_PostDeploy_OfferNotSuppressedByRerunBudget(t *testing.T) {
	f := newPostDeployTriageFixture(t, run.StageTypeDeploy, run.StageTypeAcceptance)
	f.storeHandle(t, f.stageOf(0), "rev-abc")
	routed, _ := json.Marshal(map[string]any{"disposition": acceptanceDispositionFixupDispatched})
	for i := 0; i < defaultMaxAcceptanceReruns; i++ {
		f.au.seeded = append(f.au.seeded, &audit.Entry{RunID: &f.runID, Category: CategoryAcceptanceTriageDecided, Payload: routed})
	}

	disposition, p := f.triage(t, class1ErrorVerdict)
	if disposition != acceptanceDispositionRollbackOffered {
		t.Fatalf("disposition = %q, want rollback_offered at the re-run cap (payload %v)", disposition, p)
	}
	if p["prior_routed_passes"] != float64(defaultMaxAcceptanceReruns) {
		t.Errorf("prior_routed_passes = %v, want %d", p["prior_routed_passes"], defaultMaxAcceptanceReruns)
	}
}

// (C11) TestTriageAcceptanceFailure_PostDeploy_UnsettledWins: an acceptance
// stage not yet settled still records unsettled_paged — the offer is placed
// AFTER the unsettled check.
func TestTriageAcceptanceFailure_PostDeploy_UnsettledWins(t *testing.T) {
	f := newPostDeployTriageFixture(t, run.StageTypeDeploy, run.StageTypeAcceptance)
	f.storeHandle(t, f.stageOf(0), "rev-abc")
	f.last().State = run.StageStateRunning

	disposition, p := f.triage(t, class1ErrorVerdict)
	if disposition != acceptanceDispositionUnsettled {
		t.Fatalf("disposition = %q, want unsettled_paged (payload %v)", disposition, p)
	}
	if _, has := p["rollback_offer"]; has {
		t.Errorf("unsettled payload carries rollback_offer: %v", p)
	}
}

// (C12) TestTriageAcceptanceFailure_PostDeploy_StageListError_FallsThrough: a
// ListStagesForRun error falls through to the EXISTING routing — class 4 pages,
// class 1 degrades to fixup_unavailable_paged — never an offer. Deleting the
// error branch alone is unobservable here (nil stages behave like "no deploy"
// and fall through to the same routing — a masking case); the counterfactual
// is making that branch return rollback_offered.
func TestTriageAcceptanceFailure_PostDeploy_StageListError_FallsThrough(t *testing.T) {
	for _, tc := range []struct {
		name string
		acc  acceptanceBody
		want string
	}{
		{"class4", class4UnitemizedVerdict, acceptanceDispositionPaged},
		{"class1", class1ErrorVerdict, acceptanceDispositionFixupUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPostDeployTriageFixture(t, run.StageTypeDeploy, run.StageTypeAcceptance)
			f.storeHandle(t, f.stageOf(0), "rev-abc")
			f.rr.stagesByRunID = nil // ListStagesForRun now errors

			disposition, p := f.triage(t, tc.acc)
			if disposition != tc.want {
				t.Errorf("disposition = %q, want %q", disposition, tc.want)
			}
			if _, has := p["rollback_offer"]; has {
				t.Errorf("payload carries rollback_offer after a stage-list error: %v", p)
			}
		})
	}
}

// TestTriageAcceptanceFailure_PostDeploy_Class2And3Unchanged: only classes 1
// and 4 are offered. A class-2 (skip/flake) verdict keeps its re-open, and a
// class-3 (bad criterion — every itemized failure on a plan-less release run)
// keeps its page, both with no offer.
func TestTriageAcceptanceFailure_PostDeploy_Class2And3Unchanged(t *testing.T) {
	t.Run("class2", func(t *testing.T) {
		f := newPostDeployTriageFixture(t, run.StageTypeDeploy, run.StageTypeAcceptance)
		f.storeHandle(t, f.stageOf(0), "rev-abc")
		acc := acceptanceBody{Verdict: "failed", FailureMode: "assertion_fail"}
		acc.normalizedCriteria = []acceptanceCriterionResult{{ID: "ac-1", Result: acceptanceResultSkipped}}
		disposition, p := f.triage(t, acc)
		if p["class"] != acceptanceClass2 {
			t.Fatalf("class = %v, want 2", p["class"])
		}
		if disposition != acceptanceDispositionRetryDispatched {
			t.Errorf("disposition = %q, want retry_dispatched (the existing class-2 re-open route)", disposition)
		}
		if _, has := p["rollback_offer"]; has {
			t.Errorf("class-2 payload carries rollback_offer: %v", p)
		}
	})
	t.Run("class3", func(t *testing.T) {
		f := newPostDeployTriageFixture(t, run.StageTypeDeploy, run.StageTypeAcceptance)
		f.storeHandle(t, f.stageOf(0), "rev-abc")
		acc := acceptanceBody{Verdict: "failed", FailureMode: "assertion_fail"}
		acc.normalizedCriteria = []acceptanceCriterionResult{{ID: "ac-1", Result: acceptanceResultFailed}}
		disposition, p := f.triage(t, acc)
		if p["class"] != acceptanceClass3 {
			t.Fatalf("class = %v, want 3", p["class"])
		}
		if disposition != acceptanceDispositionPaged {
			t.Errorf("disposition = %q, want paged", disposition)
		}
		if _, has := p["rollback_offer"]; has {
			t.Errorf("class-3 payload carries rollback_offer: %v", p)
		}
	})
}

// TestTriageAcceptanceFailure_PostDeploy_HandleReadErrorStillOffers: a stored
// rollback_handle read error still offers — with an empty handle and a reason
// naming the failure — because the offer has no side effect and the rollback
// endpoint re-reads and fails closed itself.
func TestTriageAcceptanceFailure_PostDeploy_HandleReadErrorStillOffers(t *testing.T) {
	f := newPostDeployTriageFixture(t, run.StageTypeDeploy, run.StageTypeAcceptance)
	f.storeHandle(t, f.stageOf(0), "rev-abc")
	f.ar.listErr = errors.New("artifact store down")

	disposition, p := f.triage(t, class4UnitemizedVerdict)
	if disposition != acceptanceDispositionRollbackOffered {
		t.Fatalf("disposition = %q, want rollback_offered on a handle read error (payload %v)", disposition, p)
	}
	offer, _ := p["rollback_offer"].(map[string]any)
	if offer["deploy_stage_id"] != f.stageOf(0).ID.String() || offer["rollback_handle"] != "" || offer["deployment_artifact_id"] != "" {
		t.Errorf("rollback_offer = %v, want the deploy stage with an empty handle + artifact id", offer)
	}
	if reason, _ := p["reason"].(string); !strings.Contains(reason, "could not be read") {
		t.Errorf("reason = %q, want it to name the unreadable handle", reason)
	}
	if f.stub.dispatchHits != 0 {
		t.Errorf("GitHub dispatch hits = %d, want 0", f.stub.dispatchHits)
	}
}

// TestPostDeployRollbackTarget pins the pure resolver over UNSORTED rows: the
// verified deploy is the nearest one sequenced before the acceptance, the
// run-scoped deploy the lowest-sequence one, and hasImplement any implement row.
func TestPostDeployRollbackTarget(t *testing.T) {
	mk := func(seq int, typ run.StageType) *run.Stage {
		return &run.Stage{ID: uuid.New(), Sequence: seq, Type: typ}
	}
	deployA, deployB, deployAfter := mk(0, run.StageTypeDeploy), mk(1, run.StageTypeDeploy), mk(3, run.StageTypeDeploy)
	acceptance := mk(2, run.StageTypeAcceptance)
	implement := mk(4, run.StageTypeImplement)

	verified, runScoped, hasImplement := postDeployRollbackTarget([]*run.Stage{deployAfter, acceptance, nil, deployB, deployA}, acceptance)
	if verified != deployB || runScoped != deployA || hasImplement {
		t.Errorf("got verified=%v runScoped=%v hasImplement=%v, want deployB / deployA / false", verified, runScoped, hasImplement)
	}
	if _, _, has := postDeployRollbackTarget([]*run.Stage{deployA, acceptance, implement}, acceptance); !has {
		t.Error("hasImplement = false with an implement row present")
	}
	if v, rs, _ := postDeployRollbackTarget([]*run.Stage{mk(0, run.StageTypeReview), acceptance}, acceptance); v != nil || rs != nil {
		t.Errorf("no deploy: verified=%v runScoped=%v, want nil/nil", v, rs)
	}
}

// (C14) TestAcceptanceDispositionPages_RollbackOffered pins that the offer
// pages a human (the immediate page-class hook fires and the arbitration guard
// admits it) and the exact literal the issuecomment and mcpserver copies mirror.
func TestAcceptanceDispositionPages_RollbackOffered(t *testing.T) {
	if acceptanceDispositionRollbackOffered != "rollback_offered" {
		t.Errorf("acceptanceDispositionRollbackOffered = %q, want rollback_offered", acceptanceDispositionRollbackOffered)
	}
	if !acceptanceDispositionPages("rollback_offered") {
		t.Error(`acceptanceDispositionPages("rollback_offered") = false, want true`)
	}
}
