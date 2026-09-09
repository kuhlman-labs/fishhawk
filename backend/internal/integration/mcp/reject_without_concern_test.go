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

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/bundle"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	runpkg "github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/server"
	"github.com/kuhlman-labs/fishhawk/backend/internal/signing"
	"github.com/kuhlman-labs/fishhawk/backend/internal/tracestore"
)

// ---------------------------------------------------------------------------
// #3319 cross-boundary: the PREDICATE SPLIT, end to end.
//
// This is the seam the per-layer units cannot cover. The verdict the server
// ingests, the concern row it refuses to move, the audit entry it writes, the
// gate-view read model and the MCP next_actions surface are five different
// representations that agree only by convention (a json tag, a payload field,
// a classifier fold). Two verdict SHAPES are driven, and the assertion that
// matters is the DIFFERENCE between them:
//
//	SHAPE 1 (bare reject): reject, concerns[] empty, ONE blank-note `reopened`.
//	  → B vetoed; advisory PRESENT (the verdict named nothing anywhere).
//	SHAPE 2 (mixed):       reject, concerns[] empty, NOTED `confirmed` for A
//	                       plus blank-note `reopened` for B.
//	  → B vetoed identically; A's confirm APPLIES; advisory ABSENT, because
//	    the VERDICT-LEVEL predicate is false while the PER-RESOLUTION one
//	    still refuses B.
//
// Shape 2 is what pins the split at the cross-boundary layer: a single shared
// predicate produces the same answer for both shapes and cannot distinguish
// them.
//
// HARNESS NOTE (operator condition 4): concern_evidence_test.go's
// `evidenceReviewer` returns ONE fixed verdict, which cannot express two shapes
// in one journey. Rather than edit that unscoped harness file or collapse the
// two shapes to fit it, this file declares its own equivalent fake and drives
// each shape as its own independent run. No scope amendment was needed.
// ---------------------------------------------------------------------------

// rwcReviewer is a PlanReviewer returning one fixed verdict, like
// concern_evidence_test.go's evidenceReviewer. Declared here so this file can
// stand up TWO servers with TWO different verdicts without touching that file.
type rwcReviewer struct {
	verdict *planreview.ReviewVerdict
	model   string
}

func (r rwcReviewer) Review(context.Context, string) (*planreview.ReviewVerdict, string, error) {
	return r.verdict, r.model, nil
}

// rwcWorkflowSpec declares ONE agent reviewer on the implement stage — the
// precondition resolveStageReviewers checks before a reviewer is invoked at all.
var rwcWorkflowSpec = []byte(`version: "0.3"
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

// rwcJourney is one driven shape: a fresh run carrying rwcWorkflowSpec, an
// approved plan artifact, an implement stage in `running`, and two implement
// concerns routed to addressed_pending — then the reviewer's fixed verdict
// ingested through the REAL signed trace-upload path.
type rwcJourney struct {
	runID    uuid.UUID
	stageID  uuid.UUID
	concernA *concern.Concern
	concernB *concern.Concern
	srvURL   string
}

// rwcPlanJSON is a minimal valid standard_v1 plan; loadApprovedPlanForRun must
// find one or runImplementReviews returns before invoking any reviewer.
func rwcPlanJSON() []byte {
	body, _ := json.Marshal(map[string]any{
		"plan_version":     "standard_v1",
		"ticket_reference": map[string]any{"type": "github_issue", "url": "https://github.com/x/y/issues/3319", "id": "x/y#3319"},
		"generated_by":     map[string]any{"agent": "claude-code", "model": "claude-opus-5", "timestamp": "2026-09-09T00:00:00Z"},
		"summary":          "a reopen must be substantiated for the concern it names",
		"scope":            map[string]any{"files": []map[string]any{{"path": "backend/internal/server/trace.go", "operation": "modify"}}},
		"approach":         []map[string]any{{"step": 1, "description": "Split the predicate."}},
		"verification":     map[string]any{"test_strategy": "Run the tests.", "rollback_plan": "Revert the PR."},

		"predicted_runtime_minutes":    30,
		"predicted_runtime_confidence": "medium",
	})
	return body
}

// rwcBundle returns a gzipped JSONL trace bundle with a git_diff event and NO
// push_fixup marker, so the raw-variant trace hook dispatches the implement
// review rather than deferring it to a push report.
func rwcBundle(t *testing.T) []byte {
	t.Helper()
	type line struct {
		Seq  int             `json:"seq"`
		TS   time.Time       `json:"ts"`
		Kind string          `json:"kind"`
		Data json.RawMessage `json:"data,omitempty"`
	}
	t0 := time.Date(2026, 9, 9, 10, 0, 0, 0, time.UTC)
	mdata, err := json.Marshal(bundle.Manifest{BundleSchema: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	diffData, err := json.Marshal(map[string]any{
		"kind":     "git_diff",
		"base_ref": "main",
		"files": []map[string]string{
			{"path": "backend/internal/server/trace.go", "status": "modified"},
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

// driveRWCShape stands up one journey and ingests `verdict` through the real
// trace-upload path. resolutions is built from the two seeded concern ids by
// the caller-supplied builder, so a shape can name A, B, or both.
func driveRWCShape(t *testing.T, ctx context.Context, fx *e2eFixture,
	build func(idA, idB string) *planreview.ReviewVerdict) rwcJourney {
	t.Helper()

	auditRepo := audit.NewPostgresRepository(fx.pool)
	signingRepo := signing.NewPostgresRepository(fx.pool)
	artifactRepo := artifact.NewPostgresRepository(fx.pool)
	concernRepo := concern.NewPostgresRepository(fx.pool)

	r, err := fx.runRepo.CreateRun(ctx, runpkg.CreateRunParams{
		Repo:          "kuhlman-labs/fishhawk",
		WorkflowID:    "feature_change",
		WorkflowSHA:   "deadbeef",
		TriggerSource: runpkg.TriggerCLI,
		WorkflowSpec:  rwcWorkflowSpec,
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	// The approved plan artifact on a plan stage.
	planStage, err := fx.runRepo.CreateStage(ctx, runpkg.CreateStageParams{
		RunID:        r.ID,
		Sequence:     1,
		Type:         runpkg.StageTypePlan,
		ExecutorKind: runpkg.ExecutorAgent,
		ExecutorRef:  "fishhawk/runner@v1",
	})
	if err != nil {
		t.Fatalf("CreateStage(plan): %v", err)
	}
	planContent := rwcPlanJSON()
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

	// The implement stage, in `running` — the state the raw trace upload
	// completes from.
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
	for _, to := range []runpkg.StageState{runpkg.StageStateDispatched, runpkg.StageStateRunning} {
		if _, err := fx.runRepo.TransitionStage(ctx, implStage.ID, to, nil); err != nil {
			t.Fatalf("TransitionStage(implement → %s): %v", to, err)
		}
	}

	// TWO real implement concerns, both routed back to addressed_pending
	// through the real repository — seeded BY CONSTRUCTION, so a veto deletion
	// reddens on the behavioural assertion and not on fixture setup.
	rows, err := concernRepo.InsertRaised(ctx, concern.InsertRaisedParams{
		RunID:                r.ID,
		StageID:              implStage.ID,
		StageKind:            concern.StageKindImplement,
		ReviewerModel:        "gpt-5.6-sol",
		OriginReviewSequence: 1,
		Concerns: []concern.RaisedConcern{
			{Severity: "medium", Category: "correctness", Note: "concern A: the projection drops the verdict"},
			{Severity: "high", Category: "authz", Note: "concern B: the handler still trusts the caller subject"},
		},
	})
	if err != nil {
		t.Fatalf("InsertRaised: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("seeded concerns = %d, want 2", len(rows))
	}
	rowA, rowB := rows[0], rows[1]
	if err := concernRepo.MarkAddressedPending(ctx, []uuid.UUID{rowA.ID}, rwcRoutingReasonA); err != nil {
		t.Fatalf("route A: %v", err)
	}
	if err := concernRepo.MarkAddressedPending(ctx, []uuid.UUID{rowB.ID}, rwcRoutingReasonB); err != nil {
		t.Fatalf("route B: %v", err)
	}

	srv := server.New(server.Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      fx.runRepo,
		AuditRepo:    auditRepo,
		SigningRepo:  signingRepo,
		ArtifactRepo: artifactRepo,
		ConcernRepo:  concernRepo,
		APITokenRepo: fx.apitokenRepo,
		TraceStore:   tracestore.NewMem(),
		GitHub:       githubclient.New(nil),
		PlanReviewer: rwcReviewer{verdict: build(rowA.ID.String(), rowB.ID.String()), model: "fable-5"},
	})
	httpSrv := httptest.NewServer(srv.Handler())
	t.Cleanup(httpSrv.Close)

	// The run needs its OWN signing key: the fixture's key is bound to the
	// fixture's run, and the trace handler verifies per-run.
	issued, err := signingRepo.Issue(ctx, r.ID, time.Hour)
	if err != nil {
		t.Fatalf("Issue signing key: %v", err)
	}

	// Ingest the verdict through the REAL signed raw-variant trace upload —
	// the production path that calls runImplementReviews.
	body := rwcBundle(t)
	signature := ed25519.Sign(issued.PrivateKey, signing.ComputeMessage(body))
	url := fmt.Sprintf("%s/v0/runs/%s/trace?stage_id=%s&variant=raw", httpSrv.URL, r.ID, implStage.ID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build trace request: %v", err)
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("X-Fishhawk-Signature", hex.EncodeToString(signature))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("trace POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusAccepted {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("trace status %d: %s", resp.StatusCode, raw)
	}

	return rwcJourney{runID: r.ID, stageID: implStage.ID, concernA: rowA, concernB: rowB, srvURL: httpSrv.URL}
}

const (
	rwcRoutingReasonA = "routing reason A: fix the projection"
	rwcRoutingReasonB = "routing reason B: fix the authz check"
)

// rwcConcernState re-reads one concern row from the real repository.
func rwcConcernState(t *testing.T, ctx context.Context, fx *e2eFixture, id uuid.UUID) *concern.Concern {
	t.Helper()
	rows, err := concern.NewPostgresRepository(fx.pool).GetByIDs(ctx, []uuid.UUID{id})
	if err != nil {
		t.Fatalf("GetByIDs: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("GetByIDs returned %d rows, want 1", len(rows))
	}
	return rows[0]
}

// rwcVetoes decodes every concern_resolution_vetoed audit entry on the run.
func rwcVetoes(t *testing.T, ctx context.Context, fx *e2eFixture, runID uuid.UUID) []struct {
	ConcernID  string `json:"concern_id"`
	Resolution string `json:"resolution"`
	VetoReason string `json:"veto_reason"`
} {
	t.Helper()
	entries, err := audit.NewPostgresRepository(fx.pool).ListForRunByCategory(ctx, runID, "concern_resolution_vetoed")
	if err != nil {
		t.Fatalf("ListForRunByCategory: %v", err)
	}
	var out []struct {
		ConcernID  string `json:"concern_id"`
		Resolution string `json:"resolution"`
		VetoReason string `json:"veto_reason"`
	}
	for _, e := range entries {
		var p struct {
			ConcernID  string `json:"concern_id"`
			Resolution string `json:"resolution"`
			VetoReason string `json:"veto_reason"`
		}
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("decode veto payload: %v", err)
		}
		out = append(out, p)
	}
	return out
}

// rwcGateViewConcern reads one open concern off the REAL gate-view HTTP
// surface, decoded from the raw body so a json-tag regression goes RED.
func rwcGateViewConcern(t *testing.T, ctx context.Context, fx *e2eFixture, j rwcJourney, id uuid.UUID) struct {
	ID       string `json:"id"`
	State    string `json:"state"`
	Disputed bool   `json:"disputed"`
	Disputes []struct {
		VetoReason string `json:"veto_reason"`
		Resolution string `json:"resolution"`
	} `json:"disputes"`
} {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, j.srvURL+"/v0/runs/"+j.runID.String()+"/gate-view", nil)
	if err != nil {
		t.Fatalf("build gate-view request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+fx.operatorTok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("gate-view request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read gate-view body: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("gate-view status %d: %s", resp.StatusCode, raw)
	}
	var gv struct {
		Open []struct {
			ID       string `json:"id"`
			State    string `json:"state"`
			Disputed bool   `json:"disputed"`
			Disputes []struct {
				VetoReason string `json:"veto_reason"`
				Resolution string `json:"resolution"`
			} `json:"disputes"`
		} `json:"open"`
	}
	if err := json.Unmarshal(raw, &gv); err != nil {
		t.Fatalf("decode gate-view body: %v\n%s", err, raw)
	}
	for _, c := range gv.Open {
		if c.ID == id.String() {
			return c
		}
	}
	t.Fatalf("concern %s not present in the gate view's open set:\n%s", id, raw)
	return gv.Open[0]
}

// TestE2E_RejectWithoutConcern_PredicateSplit is the cross-boundary done-means
// for #3319, driving BOTH verdict shapes (binding correction 1) so the
// DIFFERENCE between the per-resolution veto and the verdict-level advisory is
// observed end to end.
func TestE2E_RejectWithoutConcern_PredicateSplit(t *testing.T) {
	fx := newFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	session := connectMCPClient(t, ctx, fx.mcpBinary, fx.operatorTok, fx.url)

	// --- SHAPE 1: a BARE reject with one blank-note reopen for B. ---------
	shape1 := driveRWCShape(t, ctx, fx, func(_, idB string) *planreview.ReviewVerdict {
		return &planreview.ReviewVerdict{
			Verdict: planreview.VerdictReject,
			// A NON-BLANK free_form, deliberately: it is the reported #3319
			// shape (the whole assertion living in unmatched prose) AND it makes
			// the free-form counterfactual reachable from this fixture — a
			// predicate mutated to treat free_form as substantiation would stop
			// vetoing B here, turning this journey RED.
			FreeForm:           "this is still broken in three places and must not merge",
			ConcernResolutions: []planreview.ConcernResolution{{ID: idB, Resolution: "reopened"}},
		}
	})

	gotB := rwcConcernState(t, ctx, fx, shape1.concernB.ID)
	if gotB.State != concern.StateAddressedPending {
		t.Errorf("shape 1: B state = %q, want addressed_pending — an unsubstantiated reopen must leave the ledger unchanged", gotB.State)
	}
	if gotB.StateReason != rwcRoutingReasonB {
		t.Errorf("shape 1: B state_reason = %q, want the routing reason byte-identical", gotB.StateReason)
	}
	v1 := rwcVetoes(t, ctx, fx, shape1.runID)
	if len(v1) != 1 {
		t.Fatalf("shape 1: concern_resolution_vetoed entries = %d, want 1: %+v", len(v1), v1)
	}
	if v1[0].ConcernID != shape1.concernB.ID.String() || v1[0].VetoReason != "reopen_without_named_concern" || v1[0].Resolution != "reopened" {
		t.Errorf("shape 1: veto = %+v, want reopen_without_named_concern naming B", v1[0])
	}
	gv1 := rwcGateViewConcern(t, ctx, fx, shape1, shape1.concernB.ID)
	if gv1.Disputed {
		t.Error("shape 1: gate view reports B disputed=true, want false — a refused REOPEN is not a confirmation that failed to settle the concern")
	}
	if len(gv1.Disputes) != 1 || gv1.Disputes[0].VetoReason != "reopen_without_named_concern" {
		t.Errorf("shape 1: disputes = %+v, want exactly one row carrying the refusal as detail", gv1.Disputes)
	}
	na1 := getNextActions(t, ctx, session, shape1.runID)
	if !rwcHasAdvisory(na1) {
		t.Errorf("shape 1: next_actions must carry the review_reject_without_concern advisory (this reject named nothing anywhere); got %+v", na1)
	}

	// --- SHAPE 2: the MIXED verdict — noted confirm for A, blank reopen B. -
	// The split's discriminating case: B is vetoed exactly as in shape 1,
	// A's confirm applies, and the advisory is ABSENT because the
	// VERDICT-LEVEL predicate is false (A carries a note).
	shape2 := driveRWCShape(t, ctx, fx, func(idA, idB string) *planreview.ReviewVerdict {
		return &planreview.ReviewVerdict{
			Verdict: planreview.VerdictReject,
			ConcernResolutions: []planreview.ConcernResolution{
				{ID: idA, Resolution: "confirmed", Note: "looks good"},
				{ID: idB, Resolution: "reopened"},
			},
		}
	})

	gotB2 := rwcConcernState(t, ctx, fx, shape2.concernB.ID)
	if gotB2.State != concern.StateAddressedPending {
		t.Errorf("shape 2: B state = %q, want addressed_pending — concern A's note must not substantiate concern B's reopen", gotB2.State)
	}
	if gotB2.StateReason != rwcRoutingReasonB {
		t.Errorf("shape 2: B state_reason = %q, want the routing reason byte-identical", gotB2.StateReason)
	}
	if gotA2 := rwcConcernState(t, ctx, fx, shape2.concernA.ID); gotA2.State != concern.StateAddressed {
		t.Errorf("shape 2: A state = %q, want addressed — a sibling's veto must not suppress a well-noted confirm", gotA2.State)
	}
	v2 := rwcVetoes(t, ctx, fx, shape2.runID)
	if len(v2) != 1 {
		t.Fatalf("shape 2: concern_resolution_vetoed entries = %d, want exactly 1 (B only): %+v", len(v2), v2)
	}
	if v2[0].ConcernID != shape2.concernB.ID.String() || v2[0].VetoReason != "reopen_without_named_concern" || v2[0].Resolution != "reopened" {
		t.Errorf("shape 2: veto = %+v, want reopen_without_named_concern naming B", v2[0])
	}
	gv2 := rwcGateViewConcern(t, ctx, fx, shape2, shape2.concernB.ID)
	if gv2.Disputed {
		t.Error("shape 2: gate view reports B disputed=true, want false")
	}
	if len(gv2.Disputes) != 1 || gv2.Disputes[0].VetoReason != "reopen_without_named_concern" {
		t.Errorf("shape 2: disputes = %+v, want exactly one row", gv2.Disputes)
	}
	// THE POINT OF THE SPLIT, observed at the cross-boundary layer: the same
	// per-resolution refusal, the OPPOSITE verdict-level advisory.
	na2 := getNextActions(t, ctx, session, shape2.runID)
	if rwcHasAdvisory(na2) {
		t.Errorf("shape 2: next_actions must NOT carry the review_reject_without_concern advisory — resolution A carries a note, so this reject DID name something; got %+v", na2)
	}
}

func rwcHasAdvisory(na *nextActionsView) bool {
	if na == nil {
		return false
	}
	for _, a := range na.Actions {
		if a.Action == "review_reject_without_concern" {
			return true
		}
	}
	return false
}
