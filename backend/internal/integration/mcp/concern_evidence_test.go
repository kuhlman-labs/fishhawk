package mcpe2e_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	runpkg "github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/server"
	"github.com/kuhlman-labs/fishhawk/backend/internal/signing"
)

// concernEvidenceWorkflowSpec declares ONE agent reviewer on the plan stage —
// the precondition runPlanReviews resolves before it invokes a reviewer at all.
var concernEvidenceWorkflowSpec = []byte(`version: "0.3"
workflows:
  feature_change:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        reviewers:
          agent: 1
        produces:
          - artifact: plan
            schema: standard_v1
      - id: implement
        type: implement
        executor:
          agent: claude-code
`)

// evidenceReviewer is a PlanReviewer that returns one fixed verdict. The
// reviewer backend is the only faked hop: everything downstream of it — the
// audit append, the concern persistence, Postgres, the gate-view handler and
// its JSON serialization — is the real production code.
type evidenceReviewer struct{ verdict *planreview.ReviewVerdict }

func (r evidenceReviewer) Review(context.Context, string) (*planreview.ReviewVerdict, string, error) {
	return r.verdict, "gpt-5.6-sol", nil
}

// concernEvidencePlanJSON is a minimal valid standard_v1 plan.
func concernEvidencePlanJSON() []byte {
	body, _ := json.Marshal(map[string]any{
		"plan_version":     "standard_v1",
		"ticket_reference": map[string]any{"type": "github_issue", "url": "https://github.com/x/y/issues/2353", "id": "x/y#2353"},
		"generated_by":     map[string]any{"agent": "claude-code", "model": "claude-opus-5", "timestamp": "2026-08-12T00:00:00Z"},
		"summary":          "surface reviewer evidence on every operator read path",
		"scope":            map[string]any{"files": []map[string]any{{"path": "backend/internal/server/gateview.go", "operation": "modify"}}},
		"approach":         []map[string]any{{"step": 1, "description": "Thread new_evidence and settled_ref end to end."}},
		"verification":     map[string]any{"test_strategy": "Run the tests.", "rollback_plan": "Revert the PR."},

		"predicted_runtime_minutes":    34,
		"predicted_runtime_confidence": "medium",
	})
	return body
}

// TestE2E_ConcernEvidence_ReachesGateView is the cross-boundary done-means for
// E60.8 / #2353, and it carries BOTH fields on ONE journey (binding condition
// 1). settled_ref crosses exactly the same four boundaries as new_evidence, so
// asserting one end-to-end and the other per-layer would leave a severed seam
// on the second field invisible — which is the whole reason the cross-boundary
// rule exists.
//
// The journey, all real code past the reviewer adapter:
//
//	reviewer verdict  (planreview.Concern carrying new_evidence + settled_ref)
//	→ audit payload   (the plan_reviewed entry appended to real Postgres)
//	→ domain          (persistReviewConcerns' RaisedConcern mapping — the exact
//	                   two-field drop this change fixes)
//	→ persistence     (migration 0069's columns, real Postgres, real repository)
//	→ HTTP            (GET /v0/runs/{id}/gate-view, the surface behind
//	                   fishhawk_get_gate_view)
//
// Per-layer units each pass while any one of those seams is severed: the
// verdict decode, the concern repository round-trip and the gate-view renderer
// are all green today with trace.go's literal dropping both fields.
func TestE2E_ConcernEvidence_ReachesGateView(t *testing.T) {
	fx := newFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	const newEvidence = "backend/internal/server/trace.go:4557 builds concern.RaisedConcern with severity/category/note/suggested_patch only; both evidence fields are dropped before InsertRaised"
	settledRef := uuid.New().String()

	auditRepo := audit.NewPostgresRepository(fx.pool)
	signingRepo := signing.NewPostgresRepository(fx.pool)
	artifactRepo := artifact.NewPostgresRepository(fx.pool)
	concernRepo := concern.NewPostgresRepository(fx.pool)
	srv := server.New(server.Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      fx.runRepo,
		AuditRepo:    auditRepo,
		SigningRepo:  signingRepo,
		ArtifactRepo: artifactRepo,
		ConcernRepo:  concernRepo,
		APITokenRepo: fx.apitokenRepo,
		GitHub:       githubclient.New(nil),
		PlanReviewer: evidenceReviewer{verdict: &planreview.ReviewVerdict{
			Verdict: planreview.VerdictReject,
			Concerns: []planreview.Concern{{
				Severity:    planreview.SeverityHigh,
				Category:    "correctness",
				Note:        "the evidence for this concern is not reaching the gate",
				NewEvidence: newEvidence,
				// A settled_ref that resolves to nothing: the re-litigation guard
				// falls OPEN on an unknown ref, so the concern mints normally and
				// both fields must survive onto the row.
				SettledRef: settledRef,
			}},
			FreeForm: "reject pending the evidence surfacing",
		}},
	})
	httpSrv := httptest.NewServer(srv.Handler())
	t.Cleanup(httpSrv.Close)

	r, err := fx.runRepo.CreateRun(ctx, runpkg.CreateRunParams{
		Repo:          "kuhlman-labs/fishhawk",
		WorkflowID:    "feature_change",
		WorkflowSHA:   "deadbeef",
		TriggerSource: runpkg.TriggerCLI,
		WorkflowSpec:  concernEvidenceWorkflowSpec,
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	planStage, err := fx.runRepo.CreateStage(ctx, runpkg.CreateStageParams{
		RunID:            r.ID,
		Sequence:         1,
		Type:             runpkg.StageTypePlan,
		ExecutorKind:     runpkg.ExecutorAgent,
		ExecutorRef:      "fishhawk/runner@v1",
		RequiresApproval: true,
	})
	if err != nil {
		t.Fatalf("CreateStage(plan): %v", err)
	}
	for _, to := range []runpkg.StageState{runpkg.StageStateDispatched, runpkg.StageStateRunning} {
		if _, err := fx.runRepo.TransitionStage(ctx, planStage.ID, to, nil); err != nil {
			t.Fatalf("TransitionStage(plan → %s): %v", to, err)
		}
	}
	// The run needs its own signing key: the fixture's key is bound to the
	// fixture's run, and the plan-upload handler verifies per-run.
	issued, err := signingRepo.Issue(ctx, r.ID, time.Hour)
	if err != nil {
		t.Fatalf("Issue signing key: %v", err)
	}

	// Ship the plan through the real signed upload path. That handler invokes
	// runPlanReviews, which appends the plan_reviewed audit entry and calls
	// persistReviewConcerns with the decoded verdict.
	resp := shipPlanSigned(t, ctx, httpSrv.URL, r.ID, planStage.ID, issued.PrivateKey, concernEvidencePlanJSON())
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("ship plan status %d: %s", resp.StatusCode, raw)
	}

	// Persistence leg, read through the REAL repository against real Postgres:
	// both fields landed on the durable row via migration 0069's columns.
	rows, err := concernRepo.ListByRun(ctx, r.ID)
	if err != nil {
		t.Fatalf("ListByRun: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("persisted concerns = %d, want 1: %+v", len(rows), rows)
	}
	if rows[0].NewEvidence != newEvidence {
		t.Errorf("persisted NewEvidence = %q, want %q verbatim — dropped between the verdict and the row",
			rows[0].NewEvidence, newEvidence)
	}
	if rows[0].SettledRef != settledRef {
		t.Errorf("persisted SettledRef = %q, want %q verbatim — dropped between the verdict and the row",
			rows[0].SettledRef, settledRef)
	}

	// Read leg: the surface the operator actually reads. Raw body, so a json-tag
	// regression (which yields an empty field, never an error) goes RED.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		httpSrv.URL+"/v0/runs/"+r.ID.String()+"/gate-view", nil)
	if err != nil {
		t.Fatalf("build gate-view request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+fx.operatorTok)
	gvResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("gate-view request: %v", err)
	}
	defer func() { _ = gvResp.Body.Close() }()
	rawBody, err := io.ReadAll(gvResp.Body)
	if err != nil {
		t.Fatalf("read gate-view body: %v", err)
	}
	if gvResp.StatusCode != http.StatusOK {
		t.Fatalf("gate-view status %d: %s", gvResp.StatusCode, rawBody)
	}

	var gv struct {
		Open []struct {
			Note        string `json:"note"`
			NewEvidence string `json:"new_evidence"`
			SettledRef  string `json:"settled_ref"`
		} `json:"open"`
	}
	if err := json.Unmarshal(rawBody, &gv); err != nil {
		t.Fatalf("decode gate-view body: %v\n%s", err, rawBody)
	}
	if len(gv.Open) != 1 {
		t.Fatalf("gate-view open concerns = %d, want 1:\n%s", len(gv.Open), rawBody)
	}
	if gv.Open[0].NewEvidence != newEvidence {
		t.Errorf("gate-view new_evidence = %q, want %q verbatim — the operator reads this surface\nbody: %s",
			gv.Open[0].NewEvidence, newEvidence, rawBody)
	}
	if gv.Open[0].SettledRef != settledRef {
		t.Errorf("gate-view settled_ref = %q, want %q verbatim\nbody: %s",
			gv.Open[0].SettledRef, settledRef, rawBody)
	}
}
