package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/orchestrator"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// delegation_shadow_blind_test.go pins the BLINDNESS half of the E82.1 /
// #3778 delegation shadow stamp (ADR-085 rule 4): the stamp is never rendered
// to the deciding human. A stamp whose payload carries a unique marker is
// seeded BY CONSTRUCTION, its presence is proved through the reads that are
// meant to return it (the explicit-category read and the chain=true read —
// positive controls, so absence elsewhere is not vacuous), and then neither
// the category string nor the marker may appear on any gate-facing surface:
// the unfiltered per-run audit list, GET /v0/audit?run_id&limit=5 (the read
// fishhawk_get_run_status's recent_audit makes), the run read incl. its
// delegation block, the gate view, the auto-drive hand-off and the status
// comment (anchor) render.

// shadowBlindMarker is the unique string the seeded stamp's reason carries.
const shadowBlindMarker = "SHADOW-MARKER-3778"

// chainReadingAudit is auditFake with a ChainsByParent that returns the run's
// full chain (auditFake's returns nil), so the chain=true positive control
// reads the same rows the list reads do.
type chainReadingAudit struct{ *auditFake }

func (a chainReadingAudit) ChainsByParent(ctx context.Context, runID uuid.UUID, _ bool) ([]*audit.Entry, error) {
	return a.ListForRun(ctx, runID)
}

// shadowBlindFixture is a report-mode approve run parked at the plan gate
// with a clean two-reviewer round, five ordinary rows, and ONE marker-bearing
// stamp seeded in the NEWEST slot of the time-descending global feed (index 0
// of the fake's seed order, which ListAll returns verbatim).
type shadowBlindFixture struct {
	s     *Server
	au    *auditFake
	runID uuid.UUID
}

func newShadowBlindFixture(t *testing.T) shadowBlindFixture {
	t.Helper()
	repo := &autoDriveRepo{driveE2ERepo: &driveE2ERepo{fakeRepo: newFakeRepo()}}
	au := newAuditFake()
	s := New(Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      repo,
		AuditRepo:    chainReadingAudit{au},
		ConcernRepo:  newFakeConcernRepo(),
		ApprovalRepo: newFakeApprovalRepo(),
		Orchestrator: &orchestrator.Orchestrator{Runs: repo},
	})
	runID, stages := startAutoDriveRunWithSpec(t, s, repo, v2ReportSpecYAML(`    autonomy: medium
    actions:
      approve:
        mode: report`))
	planStageID := stages[0].ID

	stampPayload, err := json.Marshal(delegationShadowPayload{
		ShadowVersion:    delegationShadowVersion,
		Action:           "approve",
		Class:            "approve",
		Condition:        "clean_dual_approval",
		Verdict:          "unmet",
		Reason:           shadowBlindMarker + ": 1 of 2 approve verdicts",
		Mode:             "report",
		ModeSource:       "tier",
		Anchored:         true,
		HumanDecision:    "approve",
		DecisionCategory: "approval_submitted",
		ActorKind:        "user",
		ActorSubject:     "github:test-operator",
		StageID:          planStageID.String(),
		EscalationKeys:   []string{},
	})
	if err != nil {
		t.Fatalf("marshal stamp: %v", err)
	}
	rid := runID
	stamp := &audit.Entry{
		ID: uuid.New(), RunID: &rid, StageID: &planStageID, Sequence: 99,
		Timestamp: time.Now().UTC(), Category: CategoryDelegationShadowEvaluated,
		Payload: stampPayload,
	}
	// The stamp leads the seed order (the newest slot of GET /v0/audit's
	// descending feed) so a withholding applied AFTER pagination costs the
	// limit=5 read a visible row.
	au.seeded = append([]*audit.Entry{stamp}, au.seeded...)
	seedCleanPlanApproval(t, au, runID)
	for i, cat := range []string{"stage_dispatched", "stage_succeeded"} {
		seedReviewEntry(t, au, runID, int64(10+i), cat, map[string]any{"stage_id": planStageID.String()})
	}
	return shadowBlindFixture{s: s, au: au, runID: runID}
}

// get drives a read handler directly with the session operator identity and
// the run_id path value the mux would supply.
func (f shadowBlindFixture) get(t *testing.T, h http.HandlerFunc, path string) string {
	t.Helper()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.SetPathValue("run_id", f.runID.String())
	h(w, withAuth(req))
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s: status = %d, want 200:\n%s", path, w.Code, w.Body.String())
	}
	return w.Body.String()
}

func (f shadowBlindFixture) runAudit(t *testing.T, query string) string {
	t.Helper()
	return f.get(t, f.s.handleListRunAudit, "/v0/runs/"+f.runID.String()+"/audit"+query)
}

// assertBlind fails when surface carries the stamp's category or marker.
func assertBlind(t *testing.T, surface, body string) {
	t.Helper()
	if strings.Contains(body, CategoryDelegationShadowEvaluated) {
		t.Errorf("%s carries the %s category; the shadow stamp must be withheld:\n%s", surface, CategoryDelegationShadowEvaluated, body)
	}
	if strings.Contains(body, shadowBlindMarker) {
		t.Errorf("%s carries the stamp marker %q; the shadow stamp must be withheld:\n%s", surface, shadowBlindMarker, body)
	}
}

// auditItems decodes a list response's items.
func auditItems(t *testing.T, body string) []auditEntryResponse {
	t.Helper()
	var out struct {
		Items []auditEntryResponse `json:"items"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode audit list: %v\n%s", err, body)
	}
	return out.Items
}

// TestShadowBlind_ExplicitCategoryAndChainReadsReturnStamp is the positive
// control: the explicit-category read (the E82 record's read path) and the
// chain=true read (hash-chain integrity) both return the marker-bearing stamp.
func TestShadowBlind_ExplicitCategoryAndChainReadsReturnStamp(t *testing.T) {
	f := newShadowBlindFixture(t)

	byCat := f.runAudit(t, "?category="+CategoryDelegationShadowEvaluated)
	if items := auditItems(t, byCat); len(items) != 1 || items[0].Category != CategoryDelegationShadowEvaluated {
		t.Fatalf("category read items = %+v, want exactly the stamp", items)
	}
	if !strings.Contains(byCat, shadowBlindMarker) {
		t.Fatalf("category read lacks the marker:\n%s", byCat)
	}

	chain := f.runAudit(t, "?chain=true")
	if !strings.Contains(chain, CategoryDelegationShadowEvaluated) || !strings.Contains(chain, shadowBlindMarker) {
		t.Fatalf("chain=true read must stay complete (carry the stamp):\n%s", chain)
	}

	global := f.get(t, f.s.handleListGlobalAudit, "/v0/audit?run_id="+f.runID.String()+"&category="+CategoryDelegationShadowEvaluated)
	if !strings.Contains(global, shadowBlindMarker) {
		t.Fatalf("global category read lacks the marker:\n%s", global)
	}
}

// TestShadowBlind_UnfilteredRunAuditWithholdsStamp pins the per-run list the
// frontend run view reads: unfiltered and other-category reads omit the stamp
// while still returning every ordinary row.
func TestShadowBlind_UnfilteredRunAuditWithholdsStamp(t *testing.T) {
	f := newShadowBlindFixture(t)

	body := f.runAudit(t, "")
	assertBlind(t, "GET /v0/runs/{id}/audit", body)
	ordinary := 0
	for _, it := range auditItems(t, body) {
		if it.Sequence > 0 {
			ordinary++
		}
	}
	if ordinary != 5 {
		t.Errorf("unfiltered list seeded rows = %d, want the 5 ordinary rows", ordinary)
	}

	assertBlind(t, "GET /v0/runs/{id}/audit?category=plan_reviewed", f.runAudit(t, "?category=plan_reviewed"))
}

// TestShadowBlind_RecentAuditWithholdsBeforePagination pins the exact read
// fishhawk_get_run_status's recent_audit makes. With the stamp in the newest
// slot, limit=5 must still return FIVE visible rows: the withholding runs
// before pageOffset, so cursors walk the filtered set.
func TestShadowBlind_RecentAuditWithholdsBeforePagination(t *testing.T) {
	f := newShadowBlindFixture(t)

	body := f.get(t, f.s.handleListGlobalAudit, "/v0/audit?run_id="+f.runID.String()+"&limit=5")
	assertBlind(t, "GET /v0/audit?run_id&limit=5", body)
	if items := auditItems(t, body); len(items) != 5 {
		t.Errorf("limit=5 items = %d, want 5 (withholding must precede pagination):\n%s", len(items), body)
	}
	assertBlind(t, "GET /v0/audit?run_id", f.get(t, f.s.handleListGlobalAudit, "/v0/audit?run_id="+f.runID.String()))
}

// TestShadowBlind_GateSurfacesNeverRenderStamp pins the surfaces the deciding
// human reads at a gate: the run read (incl. the delegation block), the gate
// view, the auto-drive hand-off and the status-comment (anchor) render.
func TestShadowBlind_GateSurfacesNeverRenderStamp(t *testing.T) {
	f := newShadowBlindFixture(t)

	_, raw := getRunResponse(t, f.s, f.runID)
	runBody, _ := json.Marshal(raw)
	if _, ok := raw["delegation"]; !ok {
		t.Fatalf("run read carries no delegation block; the fixture must exercise it:\n%s", runBody)
	}
	assertBlind(t, "GET /v0/runs/{id}", string(runBody))

	gv := callGateView(f.s, f.runID, "", gateViewReadIdentity())
	if gv.Code != http.StatusOK {
		t.Fatalf("gate view status = %d:\n%s", gv.Code, gv.Body.String())
	}
	assertBlind(t, "GET /v0/runs/{id}/gate-view", gv.Body.String())

	ad := autoDrivePost(t, f.s, f.s.handleAutoDrive, f.runID, "", "{}", autoDriveOperatorIdentity())
	if ad.Code != http.StatusOK {
		t.Fatalf("auto-drive status = %d:\n%s", ad.Code, ad.Body.String())
	}
	if !strings.Contains(ad.Body.String(), `"reported":true`) {
		t.Fatalf("auto-drive did not take the report hand-off; the fixture must exercise it:\n%s", ad.Body.String())
	}
	assertBlind(t, "POST /v0/runs/{id}/auto-drive", ad.Body.String())

	sc := f.get(t, f.s.handleGetStatusComment, "/v0/runs/"+f.runID.String()+"/status-comment")
	assertBlind(t, "GET /v0/runs/{id}/status-comment", sc)
}

// TestShadowBlind_ProductReportBundleWithholdsStamp pins the product-report
// egress: the diagnostic bundle is PUBLISHED into a forge issue, so it must
// carry neither the category nor the marker. The stamp is the plan stage's
// most recent stage-tagged row here, so without the withholding it would
// surface as the failing stage's failing_surface.
func TestShadowBlind_ProductReportBundleWithholdsStamp(t *testing.T) {
	fp := &fakeFeedbackProvider{}
	af := &scAuditFake{}
	s, runID := productReportFixture(t, fp, af)
	stages, err := s.cfg.RunRepo.ListStagesForRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("list stages: %v", err)
	}
	var failID uuid.UUID
	for _, st := range stages {
		if st.State == run.StageStateFailed {
			failID = st.ID
		}
	}
	af.allEntries = append(af.allEntries, &audit.Entry{
		Sequence: 102, StageID: &failID, Category: CategoryDelegationShadowEvaluated,
		Payload: json.RawMessage(`{"reason":"` + shadowBlindMarker + `"}`),
	})

	rec := postProductReport(s, runID, "mcp:run:"+runID.String(), "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d:\n%s", rec.Code, rec.Body.String())
	}
	if !fp.filed {
		t.Fatal("the report was not filed")
	}
	filed := fp.filedReport.Title + "\n" + fp.filedReport.Body
	assertBlind(t, "product-report filed body", filed)
	if !strings.Contains(filed, "policy_evaluated") {
		t.Errorf("filed body lacks the real failing surface policy_evaluated:\n%s", filed)
	}
}

// TestWithholdDelegationShadow pins the helper's contract directly: the
// explicit category keeps every row, any other request drops only the stamp,
// and the input slice is never filtered in place.
func TestWithholdDelegationShadow(t *testing.T) {
	stamp := &audit.Entry{Category: CategoryDelegationShadowEvaluated}
	other := &audit.Entry{Category: "plan_reviewed"}
	in := []*audit.Entry{stamp, other, nil}

	if got := withholdDelegationShadow(in, CategoryDelegationShadowEvaluated); len(got) != 3 {
		t.Errorf("explicit category kept %d rows, want 3", len(got))
	}
	for _, cat := range []string{"", "plan_reviewed"} {
		got := withholdDelegationShadow(in, cat)
		if len(got) != 2 || got[0] != other || got[1] != nil {
			t.Errorf("category %q: got %+v, want [other nil]", cat, got)
		}
	}
	if in[0] != stamp || in[1] != other {
		t.Errorf("input mutated in place: %+v", in)
	}
}
