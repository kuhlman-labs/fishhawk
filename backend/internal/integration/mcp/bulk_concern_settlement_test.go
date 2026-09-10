package mcpe2e_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/approval"
	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/bundle"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/mcpserver"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	runpkg "github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/server"
	"github.com/kuhlman-labs/fishhawk/backend/internal/signing"
	"github.com/kuhlman-labs/fishhawk/backend/internal/tracestore"
)

// ---------------------------------------------------------------------------
// E64.77 / #3318 cross-boundary: the claim SHORTHAND, the run-status MARKER and
// the BULK waive verb, on ONE journey.
//
// The seam per-layer units cannot cover: the approve-time EXPANSION, the audit
// payload it writes, the review-time RESOLUTION that reads it back, the
// run-status render and the MCP consumer's decode are five representations that
// agree only by convention (an audit key, a json tag, a state name). Each layer
// passes in isolation while the expansion and the resolution disagree about
// which ids were claimed, and while the marker is computed server-side but
// dropped on the wire.
//
// Four phases:
//
//	(i)   approve the plan with claims_all_open_plan_concerns, and assert the
//	      audit payload carries exactly the run's open PLAN ids (the implement
//	      concerns are NOT claimed);
//	(ii)  BEFORE any implement review lands, while every claimed plan concern is
//	      STILL OPEN, drive the real GET /v0/runs/{id} serialization and decode
//	      it through the MCP consumer type, asserting claimed_by_approval. That
//	      still-open state is the ONLY state in which the marker is useful — its
//	      whole purpose is telling an operator which concerns will self-settle
//	      BEFORE they do — so checking it only after settlement would leave the
//	      feature untested where it is used;
//	(iii) land a CONFIRMING implement review and assert each plan concern reached
//	      addressed_by_condition with a concern_addressed_by_condition audit row
//	      naming BOTH the claiming approval's sequence AND the confirming
//	      review's sequence, the implement concerns still open, and the marker
//	      gone (the settled concerns left the open set);
//	(iv)  BULK waive the remaining implement ids and assert one concern_waived
//	      row each and an empty open set.
//
// HARNESS NOTE (operator condition 3): newFixture is used READ-ONLY. Following
// the established pattern in this package (concern_evidence_test.go,
// reject_without_concern_test.go), the run, its stages, its mixed plan- and
// implement-stage concerns and a second server carrying ConcernRepo +
// ApprovalRepo are all stood up HERE. No shared fixture extension was needed,
// so no scope amendment was requested for e2e_test.go.
//
// COVERAGE NOTE: this drives the real HTTP approvals endpoint rather than the
// fishhawk_approve_plan MCP tool. The tool -> client -> request-body hop is
// pinned separately and specifically by mcpserver's
// TestApprovePlan_ClaimsAllOpenPlanConcerns_PlumbedToSubmitApproval and
// TestSubmitApproval_SendsClaimsAllOpenPlanConcernsBody; what only an
// integration test can cover — and what this covers — is everything downstream
// of the request body, plus the RETURN leg decoded through the real MCP
// consumer type in phase (ii).
// ---------------------------------------------------------------------------

// bcsReviewer returns one fixed implement-review verdict.
type bcsReviewer struct {
	verdict *planreview.ReviewVerdict
	model   string
}

func (r bcsReviewer) Review(context.Context, string) (*planreview.ReviewVerdict, string, error) {
	return r.verdict, r.model, nil
}

// bcsWorkflowSpec declares ONE agent reviewer on the implement stage and an
// approval gate on the plan stage.
var bcsWorkflowSpec = []byte(`version: "0.3"
workflows:
  feature_change:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
      - id: implement
        type: implement
        executor:
          agent: claude-code
        reviewers:
          agent: 1
`)

func bcsPlanJSON() []byte {
	body, _ := json.Marshal(map[string]any{
		"plan_version":     "standard_v1",
		"ticket_reference": map[string]any{"type": "github_issue", "url": "https://github.com/x/y/issues/3318", "id": "x/y#3318"},
		"generated_by":     map[string]any{"agent": "claude-code", "model": "claude-opus-5", "timestamp": "2026-09-10T00:00:00Z"},
		"summary":          "close the merge-gate concern-waiver toil",
		"scope":            map[string]any{"files": []map[string]any{{"path": "backend/internal/server/bulk_waive.go", "operation": "create"}}},
		"approach":         []map[string]any{{"step": 1, "description": "Add the shorthand, the marker and the bulk verb."}},
		"verification":     map[string]any{"test_strategy": "Run the tests.", "rollback_plan": "Revert the PR."},

		"predicted_runtime_minutes":    10,
		"predicted_runtime_confidence": "medium",
	})
	return body
}

// bcsBundle is a gzipped JSONL trace bundle with a git_diff event and no
// push_fixup marker, so the raw-variant trace hook dispatches the implement
// review rather than deferring it to a push report.
func bcsBundle(t *testing.T) []byte {
	t.Helper()
	type line struct {
		Seq  int             `json:"seq"`
		TS   time.Time       `json:"ts"`
		Kind string          `json:"kind"`
		Data json.RawMessage `json:"data,omitempty"`
	}
	t0 := time.Date(2026, 9, 10, 10, 0, 0, 0, time.UTC)
	mdata, err := json.Marshal(bundle.Manifest{BundleSchema: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	diffData, err := json.Marshal(map[string]any{
		"kind":     "git_diff",
		"base_ref": "main",
		"files": []map[string]string{
			{"path": "backend/internal/server/bulk_waive.go", "status": "added"},
		},
		"num_files": 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	lines := []line{
		{Seq: 1, TS: t0.Add(-time.Second), Kind: bundle.EventKindManifest, Data: mdata},
		{Seq: 2, TS: t0, Kind: bundle.EventKindGitDiff, Data: diffData},
		{Seq: 3, TS: t0.Add(time.Minute), Kind: "agent_end", Data: json.RawMessage(`{}`)},
		{Seq: 4, TS: t0.Add(time.Minute + time.Second), Kind: "trailer", Data: json.RawMessage(`{}`)},
	}
	var raw bytes.Buffer
	for _, l := range lines {
		b, err := json.Marshal(l)
		if err != nil {
			t.Fatal(err)
		}
		raw.Write(b)
		raw.WriteByte('\n')
	}
	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	if _, err := w.Write(raw.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return gz.Bytes()
}

// bcsPost issues an operator-authenticated JSON POST and returns the status +
// body.
func bcsPost(t *testing.T, ctx context.Context, url, token string, body any) (int, []byte) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, out
}

// bcsRunConcerns drives the REAL GET /v0/runs/{id} handler and decodes the
// concerns block through the MCP CONSUMER type (mcpserver.RunConcerns), so a
// json-tag drift on either side of that hand-maintained wire mirror — which
// yields a silent zero value, never an error — goes red here.
func bcsRunConcerns(t *testing.T, ctx context.Context, srvURL, token string, runID uuid.UUID) (*mcpserver.RunConcerns, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srvURL+"/v0/runs/"+runID.String(), nil)
	if err != nil {
		t.Fatalf("build run-status request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("run-status request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	rawBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read run-status body: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("run-status status %d: %s", resp.StatusCode, rawBody)
	}
	var decoded struct {
		Concerns *mcpserver.RunConcerns `json:"concerns"`
	}
	if err := json.Unmarshal(rawBody, &decoded); err != nil {
		t.Fatalf("decode run-status body through the MCP consumer type: %v\n%s", err, rawBody)
	}
	if decoded.Concerns == nil {
		t.Fatalf("concerns block ABSENT — presence is authoritative (#3043):\n%s", rawBody)
	}
	return decoded.Concerns, rawBody
}

func TestE2E_BulkConcernSettlement_ShorthandMarkerAndBulkWaive(t *testing.T) {
	fx := newFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	auditRepo := audit.NewPostgresRepository(fx.pool)
	signingRepo := signing.NewPostgresRepository(fx.pool)
	artifactRepo := artifact.NewPostgresRepository(fx.pool)
	concernRepo := concern.NewPostgresRepository(fx.pool)
	approvalRepo := approval.NewPostgresRepository(fx.pool)

	r, err := fx.runRepo.CreateRun(ctx, runpkg.CreateRunParams{
		Repo:          "kuhlman-labs/fishhawk",
		WorkflowID:    "feature_change",
		WorkflowSHA:   "deadbeef",
		TriggerSource: runpkg.TriggerCLI,
		WorkflowSpec:  bcsWorkflowSpec,
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
	planContent := bcsPlanJSON()
	sv := "standard_v1"
	sum := sha256.Sum256(planContent)
	if _, err := artifactRepo.Create(ctx, artifact.CreateParams{
		StageID:       planStage.ID,
		Kind:          artifact.KindPlan,
		SchemaVersion: &sv,
		Content:       planContent,
		ContentHash:   hex.EncodeToString(sum[:]),
	}); err != nil {
		t.Fatalf("Create plan artifact: %v", err)
	}
	parkAtGate(t, ctx, fx.runRepo, planStage.ID)

	implStage, err := fx.runRepo.CreateStage(ctx, runpkg.CreateStageParams{
		RunID:        r.ID,
		Sequence:     2,
		Type:         runpkg.StageTypeImplement,
		ExecutorKind: runpkg.ExecutorAgent,
		ExecutorRef:  "fishhawk/runner@v1",
	})
	if err != nil {
		t.Fatalf("CreateStage(implement): %v", err)
	}

	// MIXED concerns, seeded BY CONSTRUCTION through the real repository: TWO
	// open PLAN-stage concerns (the shorthand must claim exactly these) and TWO
	// open IMPLEMENT-stage concerns (it must claim NEITHER — they are the bulk
	// waive's subjects in phase (iv)).
	planRows, err := concernRepo.InsertRaised(ctx, concern.InsertRaisedParams{
		RunID: r.ID, StageID: planStage.ID, StageKind: concern.StageKindPlan,
		ReviewerModel: "gpt-5.6-sol", OriginReviewSequence: 1,
		Concerns: []concern.RaisedConcern{
			{Severity: "medium", Category: "correctness", Note: "plan concern A: the refusal shape is contradictory"},
			{Severity: "low", Category: "coverage", Note: "plan concern B: the criterion is only half-asserted"},
		},
	})
	if err != nil {
		t.Fatalf("InsertRaised(plan): %v", err)
	}
	implRows, err := concernRepo.InsertRaised(ctx, concern.InsertRaisedParams{
		RunID: r.ID, StageID: implStage.ID, StageKind: concern.StageKindImplement,
		ReviewerModel: "gpt-5.6-sol", OriginReviewSequence: 2,
		Concerns: []concern.RaisedConcern{
			{Severity: "medium", Category: "scope", Note: "implement concern A: an out-of-scope edit"},
			{Severity: "low", Category: "style", Note: "implement concern B: a naming nit"},
		},
	})
	if err != nil {
		t.Fatalf("InsertRaised(implement): %v", err)
	}
	planA, planB := planRows[0], planRows[1]
	implA, implB := implRows[0], implRows[1]

	srv := server.New(server.Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      fx.runRepo,
		AuditRepo:    auditRepo,
		SigningRepo:  signingRepo,
		ArtifactRepo: artifactRepo,
		ConcernRepo:  concernRepo,
		ApprovalRepo: approvalRepo,
		APITokenRepo: fx.apitokenRepo,
		TraceStore:   tracestore.NewMem(),
		GitHub:       githubclient.New(nil),
		PlanReviewer: bcsReviewer{model: "fable-5", verdict: &planreview.ReviewVerdict{
			// A CONFIRMING (non-reject) implement verdict raising nothing fresh:
			// one confirming review settles the operator's claims.
			Verdict:  planreview.VerdictApprove,
			FreeForm: "the conditions were delivered",
		}},
	})
	httpSrv := httptest.NewServer(srv.Handler())
	t.Cleanup(httpSrv.Close)

	// ---- Phase (i): approve with the SHORTHAND. -----------------------------
	status, body := bcsPost(t, ctx, httpSrv.URL+"/v0/stages/"+planStage.ID.String()+"/approvals", fx.operatorTok,
		map[string]any{
			"decision":                      "approve",
			"comment":                       "both reviews read; my conditions answer the whole open plan ledger",
			"claims_all_open_plan_concerns": true,
		})
	if status != http.StatusOK {
		t.Fatalf("approve status %d: %s", status, body)
	}

	// The EXPANSION landed on the approval_submitted payload as the expanded id
	// set, under the SAME key the #1956 resolution path reads back.
	approvalEntries, err := auditRepo.ListForRunByCategory(ctx, r.ID, "approval_submitted")
	if err != nil {
		t.Fatalf("list approval_submitted: %v", err)
	}
	if len(approvalEntries) != 1 {
		t.Fatalf("approval_submitted entries = %d, want 1", len(approvalEntries))
	}
	var approvalPayload struct {
		ClaimsConcernIDs []string `json:"claims_concern_ids"`
		ClaimsAll        bool     `json:"claims_all_open_plan_concerns"`
		Expanded         int      `json:"claims_all_open_plan_concerns_expanded"`
	}
	if err := json.Unmarshal(approvalEntries[0].Payload, &approvalPayload); err != nil {
		t.Fatalf("decode approval payload: %v", err)
	}
	approvalSeq := approvalEntries[0].Sequence
	if !approvalPayload.ClaimsAll || approvalPayload.Expanded != 2 {
		t.Errorf("intent keys = all %v / expanded %d, want true / 2: %s",
			approvalPayload.ClaimsAll, approvalPayload.Expanded, approvalEntries[0].Payload)
	}
	gotClaims := map[string]bool{}
	for _, id := range approvalPayload.ClaimsConcernIDs {
		gotClaims[id] = true
	}
	if len(gotClaims) != 2 || !gotClaims[planA.ID.String()] || !gotClaims[planB.ID.String()] {
		t.Fatalf("claims_concern_ids = %v, want exactly the two open PLAN ids [%s %s]",
			approvalPayload.ClaimsConcernIDs, planA.ID, planB.ID)
	}
	if gotClaims[implA.ID.String()] || gotClaims[implB.ID.String()] {
		t.Errorf("the expansion claimed an IMPLEMENT concern: %v", approvalPayload.ClaimsConcernIDs)
	}

	// ---- Phase (ii): the MARKER, while the claimed concerns are STILL OPEN. --
	concerns, rawBody := bcsRunConcerns(t, ctx, httpSrv.URL, fx.operatorTok, r.ID)
	if concerns.Open != 4 {
		t.Fatalf("open concerns = %d, want 4 before any review lands:\n%s", concerns.Open, rawBody)
	}
	marked := map[string]bool{}
	for _, item := range concerns.Items {
		marked[item.ID] = item.ClaimedByApproval
	}
	if !marked[planA.ID.String()] || !marked[planB.ID.String()] {
		t.Errorf("claimed_by_approval = A %v / B %v on the STILL-OPEN claimed plan concerns, want both true — the marker is dropped somewhere between the server and the MCP consumer:\n%s",
			marked[planA.ID.String()], marked[planB.ID.String()], rawBody)
	}
	if marked[implA.ID.String()] || marked[implB.ID.String()] {
		t.Errorf("claimed_by_approval set on an IMPLEMENT concern (A %v / B %v), want both false:\n%s",
			marked[implA.ID.String()], marked[implB.ID.String()], rawBody)
	}

	// ---- Phase (iii): a CONFIRMING implement review settles the claims. -----
	for _, to := range []runpkg.StageState{runpkg.StageStateDispatched, runpkg.StageStateRunning} {
		if _, err := fx.runRepo.TransitionStage(ctx, implStage.ID, to, nil); err != nil {
			t.Fatalf("TransitionStage(implement → %s): %v", to, err)
		}
	}
	issued, err := signingRepo.Issue(ctx, r.ID, time.Hour)
	if err != nil {
		t.Fatalf("Issue signing key: %v", err)
	}
	traceBody := bcsBundle(t)
	signature := ed25519.Sign(issued.PrivateKey, signing.ComputeMessage(traceBody))
	traceURL := fmt.Sprintf("%s/v0/runs/%s/trace?stage_id=%s&variant=raw", httpSrv.URL, r.ID, implStage.ID)
	traceReq, err := http.NewRequestWithContext(ctx, http.MethodPost, traceURL, bytes.NewReader(traceBody))
	if err != nil {
		t.Fatalf("build trace request: %v", err)
	}
	traceReq.Header.Set("Content-Type", "application/octet-stream")
	traceReq.Header.Set("X-Fishhawk-Signature", hex.EncodeToString(signature))
	traceResp, err := http.DefaultClient.Do(traceReq)
	if err != nil {
		t.Fatalf("trace POST: %v", err)
	}
	traceRaw, _ := io.ReadAll(traceResp.Body)
	_ = traceResp.Body.Close()
	if traceResp.StatusCode != http.StatusAccepted {
		t.Fatalf("trace status %d: %s", traceResp.StatusCode, traceRaw)
	}

	settled, err := concernRepo.GetByIDs(ctx, []uuid.UUID{planA.ID, planB.ID, implA.ID, implB.ID})
	if err != nil {
		t.Fatalf("GetByIDs after the confirming review: %v", err)
	}
	for i, want := range []concern.State{
		concern.StateAddressedByCondition, concern.StateAddressedByCondition,
		concern.StateRaised, concern.StateRaised,
	} {
		if settled[i].State != want {
			t.Errorf("concern %s state = %q, want %q", settled[i].ID, settled[i].State, want)
		}
	}

	// Each settled concern carries its OWN concern_addressed_by_condition row
	// naming BOTH lineage endpoints: the CLAIMING approval's audit sequence and
	// the CONFIRMING review's sequence. Both halves are asserted — a row naming
	// only one end does not make the lineage reconstructable from the chain,
	// which is the whole point of the entry.
	reviewEntries, err := auditRepo.ListForRunByCategory(ctx, r.ID, "implement_reviewed")
	if err != nil {
		t.Fatalf("list implement_reviewed: %v", err)
	}
	if len(reviewEntries) != 1 {
		t.Fatalf("implement_reviewed entries = %d, want 1", len(reviewEntries))
	}
	reviewSeq := reviewEntries[0].Sequence

	settleEntries, err := auditRepo.ListForRunByCategory(ctx, r.ID, server.CategoryConcernAddressedByCondition)
	if err != nil {
		t.Fatalf("list concern_addressed_by_condition: %v", err)
	}
	if len(settleEntries) != 2 {
		t.Fatalf("concern_addressed_by_condition entries = %d, want 2 (one per settled plan concern)", len(settleEntries))
	}
	sawSettled := map[string]bool{}
	for _, e := range settleEntries {
		var payload struct {
			ConcernID                string `json:"concern_id"`
			ApprovalSequence         int64  `json:"approval_sequence"`
			ConfirmingReviewSequence int64  `json:"confirming_review_sequence"`
		}
		if err := json.Unmarshal(e.Payload, &payload); err != nil {
			t.Fatalf("decode concern_addressed_by_condition payload: %v", err)
		}
		sawSettled[payload.ConcernID] = true
		if payload.ApprovalSequence != approvalSeq {
			t.Errorf("entry for %s names approval_sequence %d, want the CLAIMING approval's %d",
				payload.ConcernID, payload.ApprovalSequence, approvalSeq)
		}
		if payload.ConfirmingReviewSequence != reviewSeq {
			t.Errorf("entry for %s names confirming_review_sequence %d, want the CONFIRMING review's %d",
				payload.ConcernID, payload.ConfirmingReviewSequence, reviewSeq)
		}
	}
	if !sawSettled[planA.ID.String()] || !sawSettled[planB.ID.String()] {
		t.Errorf("concern_addressed_by_condition rows = %v, want one per claimed plan concern", sawSettled)
	}

	// The settled plan concerns have LEFT the open set, so the marker is no
	// longer emitted for them; only the implement concerns remain.
	afterReview, afterRaw := bcsRunConcerns(t, ctx, httpSrv.URL, fx.operatorTok, r.ID)
	if afterReview.Open != 2 {
		t.Fatalf("open concerns after the confirming review = %d, want 2 (only the implement pair):\n%s",
			afterReview.Open, afterRaw)
	}
	for _, item := range afterReview.Items {
		if item.ID == planA.ID.String() || item.ID == planB.ID.String() {
			t.Errorf("settled plan concern %s is still in the open set:\n%s", item.ID, afterRaw)
		}
		if item.ClaimedByApproval {
			t.Errorf("open implement concern %s carries claimed_by_approval:\n%s", item.ID, afterRaw)
		}
	}

	// ---- Phase (iv): BULK waive the remaining implement concerns. -----------
	const waiveReason = "both are accepted trade-offs; recorded and not blocking the merge"
	status, body = bcsPost(t, ctx, httpSrv.URL+"/v0/runs/"+r.ID.String()+"/concerns/waive", fx.operatorTok,
		map[string]any{
			"concern_ids": []string{implA.ID.String(), implB.ID.String()},
			"reason":      waiveReason,
		})
	if status != http.StatusOK {
		t.Fatalf("bulk waive status %d: %s", status, body)
	}
	var waiveResp struct {
		Waived  int `json:"waived"`
		Failed  int `json:"failed"`
		Results []struct {
			ConcernID string `json:"concern_id"`
			Applied   bool   `json:"applied"`
			State     string `json:"state"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body, &waiveResp); err != nil {
		t.Fatalf("decode bulk waive body: %v\n%s", err, body)
	}
	if waiveResp.Waived != 2 || waiveResp.Failed != 0 {
		t.Errorf("waived/failed = %d/%d, want 2/0: %s", waiveResp.Waived, waiveResp.Failed, body)
	}

	waived, err := concernRepo.GetByIDs(ctx, []uuid.UUID{implA.ID, implB.ID})
	if err != nil {
		t.Fatalf("GetByIDs after the bulk waive: %v", err)
	}
	for _, row := range waived {
		if row.State != concern.StateWaived {
			t.Errorf("concern %s state = %q, want waived", row.ID, row.State)
		}
		if row.StateReason != waiveReason {
			t.Errorf("concern %s state_reason = %q, want the shared batch reason", row.ID, row.StateReason)
		}
	}
	waiveEntries, err := auditRepo.ListForRunByCategory(ctx, r.ID, server.CategoryConcernWaived)
	if err != nil {
		t.Fatalf("list concern_waived: %v", err)
	}
	if len(waiveEntries) != 2 {
		t.Fatalf("concern_waived entries = %d, want 2 (one per concern in the batch)", len(waiveEntries))
	}
	sawWaived := map[string]bool{}
	for _, e := range waiveEntries {
		var payload struct {
			ConcernID string `json:"concern_id"`
			Reason    string `json:"reason"`
			BulkWaive bool   `json:"bulk_waive"`
		}
		if err := json.Unmarshal(e.Payload, &payload); err != nil {
			t.Fatalf("decode concern_waived payload: %v", err)
		}
		sawWaived[payload.ConcernID] = true
		if payload.Reason != waiveReason || !payload.BulkWaive {
			t.Errorf("entry for %s = reason %q / bulk_waive %v, want the shared reason and the bulk marker",
				payload.ConcernID, payload.Reason, payload.BulkWaive)
		}
	}
	if !sawWaived[implA.ID.String()] || !sawWaived[implB.ID.String()] {
		t.Errorf("concern_waived rows = %v, want one per batched implement concern", sawWaived)
	}

	// The run reaches its merge gate with an EMPTY open set — the toil this
	// change removes, end to end.
	final, finalRaw := bcsRunConcerns(t, ctx, httpSrv.URL, fx.operatorTok, r.ID)
	if final.Open != 0 || len(final.Items) != 0 {
		t.Errorf("open concerns after the bulk waive = %d (%d items), want 0:\n%s",
			final.Open, len(final.Items), finalRaw)
	}
}
