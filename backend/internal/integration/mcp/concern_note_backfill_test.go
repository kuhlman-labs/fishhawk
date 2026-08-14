package mcpe2e_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	runpkg "github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/server"
	"github.com/kuhlman-labs/fishhawk/backend/internal/signing"
)

// concernNoteBackfillPlanJSON is a minimal valid standard_v1 plan for the
// signed plan upload that drives the real review path.
func concernNoteBackfillPlanJSON() []byte {
	body, _ := json.Marshal(map[string]any{
		"plan_version":     "standard_v1",
		"ticket_reference": map[string]any{"type": "github_issue", "url": "https://github.com/x/y/issues/2555", "id": "x/y#2555"},
		"generated_by":     map[string]any{"agent": "claude-code", "model": "claude-opus-5", "timestamp": "2026-08-14T00:00:00Z"},
		"summary":          "never persist a review concern with an empty note",
		"scope":            map[string]any{"files": []map[string]any{{"path": "backend/internal/server/trace.go", "operation": "modify"}}},
		"approach":         []map[string]any{{"step": 1, "description": "Backfill a blank concern note before InsertRaised."}},
		"verification":     map[string]any{"test_strategy": "Run the tests.", "rollback_plan": "Revert the PR."},

		"predicted_runtime_minutes":    34,
		"predicted_runtime_confidence": "medium",
	})
	return body
}

// TestConcernNoteBackfill_EndToEnd is the cross-boundary done-means for #2555.
// A reviewer verdict carrying a concern with an EMPTY note (seeded by
// construction — the reviewer adapter is the only faked hop) is driven through
// the real ingestion path, and the synthesized substance is asserted to survive
// every boundary and arrive at BOTH operator read surfaces.
//
// The journey, all real code past the reviewer adapter:
//
//	reviewer verdict  (planreview.Concern with note:"" plus substantive free_form)
//	→ audit payload   (the plan_reviewed entry appended to real Postgres)
//	→ domain          (persistReviewConcerns' backfill + RaisedConcern mapping)
//	→ persistence     (real Postgres, real concern repository)
//	→ HTTP            (GET /v0/runs/{id}/gate-view — the full note; and
//	                   GET /v0/runs/{id} — the short_summary label)
//
// Per-layer units each pass while any one of those seams is severed: the
// verdict decode, the repository round-trip, and each renderer are all green
// today against a blank note that the write side never backfilled.
func TestConcernNoteBackfill_EndToEnd(t *testing.T) {
	fx := newFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	const freeForm = "The new retry loop has no ceiling: a permanently failing dispatch spins until the stage deadline."

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
				Severity: planreview.SeverityHigh,
				Category: "correctness",
				// Empty BY CONSTRUCTION: this is the artifact the issue reports —
				// an unactionable concern holding the gate open with no substance.
				Note: "",
			}},
			FreeForm: freeForm,
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
	issued, err := signingRepo.Issue(ctx, r.ID, time.Hour)
	if err != nil {
		t.Fatalf("Issue signing key: %v", err)
	}

	// Ship the plan through the real signed upload path, which invokes
	// runPlanReviews → persistReviewConcerns with the decoded verdict.
	resp := shipPlanSigned(t, ctx, httpSrv.URL, r.ID, planStage.ID, issued.PrivateKey, concernNoteBackfillPlanJSON())
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("ship plan status %d: %s", resp.StatusCode, raw)
	}

	// Persistence leg, through the REAL repository against real Postgres.
	rows, err := concernRepo.ListByRun(ctx, r.ID)
	if err != nil {
		t.Fatalf("ListByRun: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("persisted concerns = %d, want 1 (a blank-note concern is never dropped): %+v", len(rows), rows)
	}
	if strings.TrimSpace(rows[0].Note) == "" {
		t.Fatal("the durable row holds a BLANK note — the concern wedges the gate carrying nothing")
	}
	if !strings.Contains(rows[0].Note, freeForm) {
		t.Errorf("persisted note = %q, want the review's free_form substance recovered into it", rows[0].Note)
	}

	// The reviewer's ORIGINAL output is untouched on the audit entry: the row is
	// a derived index, so the backfill must never rewrite the authoritative
	// record of what the reviewer actually emitted.
	entries, err := auditRepo.ListForRunByCategory(ctx, r.ID, "plan_reviewed")
	if err != nil {
		t.Fatalf("ListForRunByCategory(plan_reviewed): %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("plan_reviewed entries = %d, want 1", len(entries))
	}
	var reviewed struct {
		Concerns []struct {
			Note string `json:"note"`
		} `json:"concerns"`
	}
	if err := json.Unmarshal(entries[0].Payload, &reviewed); err != nil {
		t.Fatalf("decode plan_reviewed payload: %v", err)
	}
	if len(reviewed.Concerns) != 1 || reviewed.Concerns[0].Note != "" {
		t.Errorf("plan_reviewed payload concerns = %+v, want the reviewer's ORIGINAL empty note preserved",
			reviewed.Concerns)
	}

	// Advisory traceability entry landed durably alongside it.
	backfilled, err := auditRepo.ListForRunByCategory(ctx, r.ID, "concern_note_backfilled")
	if err != nil {
		t.Fatalf("ListForRunByCategory(concern_note_backfilled): %v", err)
	}
	if len(backfilled) != 1 {
		t.Fatalf("concern_note_backfilled entries = %d, want 1", len(backfilled))
	}

	// Read leg A: the gate view, which renders the FULL note. Raw body, so a
	// json-tag regression (an empty field, never an error) goes RED.
	gvBody := getJSON(t, ctx, httpSrv.URL+"/v0/runs/"+r.ID.String()+"/gate-view", fx.operatorTok)
	var gv struct {
		Open []struct {
			Note string `json:"note"`
		} `json:"open"`
	}
	if err := json.Unmarshal(gvBody, &gv); err != nil {
		t.Fatalf("decode gate-view body: %v\n%s", err, gvBody)
	}
	if len(gv.Open) != 1 {
		t.Fatalf("gate-view open concerns = %d, want 1:\n%s", len(gv.Open), gvBody)
	}
	if !strings.Contains(gv.Open[0].Note, freeForm) {
		t.Errorf("gate-view note = %q, want the recovered substance — this is the surface the operator decides from\nbody: %s",
			gv.Open[0].Note, gvBody)
	}
	if !strings.Contains(gv.Open[0].Note, concern.MissingNoteMarker) {
		t.Errorf("gate-view note = %q, want the self-describing synthesized-note marker", gv.Open[0].Note)
	}

	// Read leg B: the run-status surface, which renders the bounded label. The
	// key was OMITTED entirely for a blank note before this change.
	//
	// The label is bounded at 100 bytes, so it structurally CANNOT carry the
	// recovered free_form prose — leg A above is the substance surface. What is
	// load-bearing here is therefore the DERIVATION, not mere non-blankness: the
	// label must carry the self-describing synthesized marker and must be a byte
	// prefix of THIS row's whitespace-collapsed persisted note (the same note leg
	// A pinned to the review's free_form). Both assertions go RED if the
	// cross-boundary note is replaced with arbitrary non-empty text, or if the
	// label is derived from anything other than the recovered note.
	runBody := getJSON(t, ctx, httpSrv.URL+"/v0/runs/"+r.ID.String(), fx.operatorTok)
	var status struct {
		Concerns struct {
			Items []map[string]any `json:"items"`
		} `json:"concerns"`
	}
	if err := json.Unmarshal(runBody, &status); err != nil {
		t.Fatalf("decode run body: %v\n%s", err, runBody)
	}
	if len(status.Concerns.Items) != 1 {
		t.Fatalf("run-status concern items = %d, want 1:\n%s", len(status.Concerns.Items), runBody)
	}
	label, present := status.Concerns.Items[0]["short_summary"]
	if !present {
		t.Fatalf("run-status short_summary key ABSENT for the backfilled concern:\n%s", runBody)
	}
	s, _ := label.(string)
	if strings.TrimSpace(s) == "" {
		t.Fatalf("run-status short_summary = %q, want a non-blank label\nbody: %s", s, runBody)
	}
	if !strings.Contains(s, concern.MissingNoteMarker) {
		t.Errorf("run-status short_summary = %q, want the self-describing synthesized-note marker %q — "+
			"the operator must be able to tell this label was not authored by the reviewer\nbody: %s",
			s, concern.MissingNoteMarker, runBody)
	}
	// The label is the collapsed note cut to the byte bound with a trailing
	// truncation marker (or the whole collapsed note when it already fits), so
	// stripping that marker must leave a byte prefix of the persisted note.
	collapsedNote := strings.Join(strings.Fields(rows[0].Note), " ")
	if head := strings.TrimSuffix(s, "..."); head == "" || !strings.HasPrefix(collapsedNote, head) {
		t.Errorf("run-status short_summary = %q, want a leading slice of the recovered note %q — "+
			"the label must be derived from THIS concern's backfilled note, not from arbitrary text\nbody: %s",
			s, collapsedNote, runBody)
	}
}

// getJSON performs an authenticated GET and returns the raw body, failing on a
// non-200.
func getJSON(t *testing.T, ctx context.Context, url, token string) []byte {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("build request %s: %v", url, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s body: %v", url, err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status %d: %s", url, resp.StatusCode, raw)
	}
	return raw
}
