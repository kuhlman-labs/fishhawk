package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/apitoken"
	"github.com/kuhlman-labs/fishhawk/backend/internal/approval"
	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// comms_apply_pg_test.go pins the operator's preview property (#4017) against
// REAL Postgres: the comms_report_recorded row's previews are stored as JSONB
// (key order and whitespace normalized), served by GET /comms-dispositions
// re-encoded, and the apply files a body the captain actually reviewed.
//
// Every hop is the production one: the run, stage, artifacts, audit chain and
// approvals live in Postgres; the report is ingested through the SIGNED POST
// /v0/runs/{id}/plan (shipPlanRequest, the real router), whose previews run
// through the registered recording provider; the read and the disposition go
// through s.Handler() (the capture takes the ATOMIC FamilyWindowAppender
// path); the apply settles the window through the same atomic path.
//
// THE PRECONDITION (approval condition 2): filing_body_digest covers the
// renderer output BEFORE the intake advisory section, and
// intakegroom.RenderBody leaves a body untouched ONLY for DEGRADED signals
// with no findings (a successful, empty evaluation still appends a section).
// The recording provider is File-only, so the intake hook degrades
// reader_unavailable with no findings; the test asserts that on the decoded
// preview before it asserts the digest equality.
//
// The test is NON-parallel: it swaps conventionsLoader.

const cmaPGBearer = "fhk_cma_operator"

func TestCommsApplyPG_PreviewJSONEqualAndFiledBodyMatches(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	runRepo := run.NewPostgresRepository(pool)
	arts := artifact.NewPostgresRepository(pool)
	au := audit.NewPostgresRepository(pool)
	approvals := approval.NewPostgresRepository(pool)

	provider := &cmaProvider{name: "comms-apply-pg-fake-" + uuid.NewString()}
	workmgmt.Register(provider)
	// The conventions KEEP their defaulted autonomy tier: the preview shows it,
	// the apply suppresses it (approval condition 4), so the body is the only
	// thing this test claims is reviewed-equals-filed.
	installConventions(t, ukApplyConventions(provider.name, true), nil)

	install := int64(4242)
	rn, err := runRepo.CreateRun(ctx, run.CreateRunParams{
		Repo: cmaRepo, WorkflowID: "user_report_scan", WorkflowSHA: "abc", TriggerSource: run.TriggerCLI,
		WorkflowSpec: userReportScanSpec(t), InstallationID: &install,
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	st, err := runRepo.CreateStage(ctx, run.CreateStageParams{
		RunID: rn.ID, Sequence: 0, Type: run.StageTypePlan,
		ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code", RequiresApproval: true,
	})
	if err != nil {
		t.Fatalf("create stage: %v", err)
	}
	for _, to := range []run.StageState{run.StageStateDispatched, run.StageStateRunning} {
		if _, err := runRepo.TransitionStage(ctx, st.ID, to, nil); err != nil {
			t.Fatalf("transition stage to %s: %v", to, err)
		}
	}

	sf := newSigningFake()
	tokens := &stubAPITokenRepo{tok: &apitoken.Token{
		ID: uuid.New(), Subject: "github:ops", Scopes: []string{"read:runs", "write:approvals"}, PlainText: cmaPGBearer,
	}}
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: runRepo, ArtifactRepo: arts, AuditRepo: au,
		ApprovalRepo: approvals, SigningRepo: sf, APITokenRepo: tokens})
	priv, _ := sf.issue(t, rn.ID)

	if _, _, err := s.recordCommsScanGathered(ctx, rn.ID, st.ID, cmaGather(cmaOpts{})); err != nil {
		t.Fatalf("record gather: %v", err)
	}
	// One draft whose body carries `&`; the server's draft marker adds `<`/`>`.
	if w := shipPlanRequest(t, s, rn.ID, st.ID, priv, cmaReportBody(t, []cmaDraft{cmaD1}, false), ""); w.Code != http.StatusCreated {
		t.Fatalf("ingest status = %d, want 201:\n%s", w.Code, w.Body.String())
	}

	do := func(method, raw string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/v0/runs/"+rn.ID.String()+"/comms-dispositions", strings.NewReader(raw))
		req.Header.Set("Authorization", "Bearer "+cmaPGBearer)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, req)
		return w
	}
	read := decodeCMD(t, do(http.MethodGet, ""))

	// JSON-EQUAL: the served previews decode to the same value as the stored
	// row's previews (byte equality is not claimed: JSONB and encoding/json
	// both re-encode).
	recRows, err := au.ListForRunByCategory(ctx, rn.ID, CategoryCommsReportRecorded)
	if err != nil || len(recRows) != 1 {
		t.Fatalf("comms_report_recorded rows = %d (err %v), want 1", len(recRows), err)
	}
	var stored struct {
		Previews json.RawMessage `json:"previews"`
	}
	if err := json.Unmarshal(recRows[0].Payload, &stored); err != nil {
		t.Fatal(err)
	}
	var servedAny, storedAny any
	if json.Unmarshal(read.Previews, &servedAny) != nil || json.Unmarshal(stored.Previews, &storedAny) != nil ||
		!reflect.DeepEqual(servedAny, storedAny) {
		t.Fatalf("served previews are not JSON-equal to the stored row:\nserved %s\nstored %s", read.Previews, stored.Previews)
	}

	var previews []commsDraftPreview
	if err := json.Unmarshal(read.Previews, &previews); err != nil {
		t.Fatalf("decode served previews: %v", err)
	}
	if len(previews) != 1 || previews[0].DraftID != cmaD1.id() || previews[0].CommsRenderedPreview == nil {
		t.Fatalf("served previews = %s, want one rendered preview for %s", read.Previews, cmaD1.id())
	}
	pv := previews[0]
	// The precondition: the intake hook DEGRADED with no findings, so the
	// preview body carries no intake section.
	if !pv.Intake.Degraded || pv.Intake.HasFindings() {
		t.Fatalf("precondition: preview intake = degraded %v findings %v, want degraded with none", pv.Intake.Degraded, pv.Intake.HasFindings())
	}
	if !strings.Contains(pv.Body, "&") || !strings.Contains(pv.Body, "<") || !strings.Contains(pv.Body, ">") {
		t.Fatalf("precondition: the preview body carries no `&`/`<`/`>` to re-encode:\n%s", pv.Body)
	}
	sum := sha256.Sum256([]byte(pv.Body))
	if got := hex.EncodeToString(sum[:]); got != pv.FilingBodyDigest || got == "" {
		t.Fatalf("sha256(decoded preview body) = %s, filing_body_digest = %s", got, pv.FilingBodyDigest)
	}

	// The captain approves D1 through the real capture (atomic path), the gate
	// is granted, and the apply files.
	decodeCMD(t, do(http.MethodPost, cmdBatch(cmdEntry(cmaD1.id(), commsVerdictApproved))))
	if _, err := approvals.Submit(ctx, approval.SubmitParams{
		StageID: st.ID, ApproverSubject: "github:ops", Decision: approval.DecisionApprove, Surface: approval.SurfaceAPI,
	}); err != nil {
		t.Fatalf("seed grant: %v", err)
	}
	stage, err := runRepo.GetStage(ctx, st.ID)
	if err != nil {
		t.Fatal(err)
	}
	s.applyApprovedComms(ctx, stage, approval.DecisionApprove)
	s.waitReportApply()

	reqs := provider.requests()
	if len(reqs) != 1 {
		t.Fatalf("File calls = %d, want 1", len(reqs))
	}
	if reqs[0].Item.Body != pv.Body {
		t.Fatalf("filed body differs from the decoded preview body:\nfiled   %q\npreview %q", reqs[0].Item.Body, pv.Body)
	}
	filedRows, err := au.ListForRunByCategory(ctx, rn.ID, CategoryCommsDraftFiled)
	if err != nil || len(filedRows) != 1 {
		t.Fatalf("comms_draft_filed rows = %d (err %v), want 1", len(filedRows), err)
	}
	var filed commsDraftFiledPayload
	if err := json.Unmarshal(filedRows[0].Payload, &filed); err != nil {
		t.Fatal(err)
	}
	if filed.FilingBodyDigest != pv.FilingBodyDigest {
		t.Errorf("filed row digest = %s, want the reviewed %s", filed.FilingBodyDigest, pv.FilingBodyDigest)
	}
	// Approval condition 4: the preview showed the defaulted autonomy label;
	// the filing suppressed exactly it.
	var defaultedAutonomy []string
	for _, l := range pv.DefaultedLabels {
		if commsIsAutonomyLabel(l) {
			defaultedAutonomy = append(defaultedAutonomy, l)
		}
	}
	if len(defaultedAutonomy) == 0 || !reflect.DeepEqual(filed.SuppressedDefaultLabels, defaultedAutonomy) {
		t.Errorf("suppressed_default_labels = %v, want the preview's defaulted autonomy %v", filed.SuppressedDefaultLabels, defaultedAutonomy)
	}
	for _, l := range reqs[0].Item.Classification.Labels {
		if commsIsAutonomyLabel(l) {
			t.Errorf("filed labels %v carry %s", reqs[0].Item.Classification.Labels, l)
		}
	}
}
