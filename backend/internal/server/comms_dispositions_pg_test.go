package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/apitoken"
	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// comms_dispositions_pg_test.go drives the comms capture END TO END against
// REAL Postgres through s.Handler(): the auth middleware, requireRunAccount,
// the handler, and the production audit repository's ATOMIC
// FamilyWindowAppender path for the comms family (run-row lock, binding
// re-check over comms_report_recorded, comms_apply_window_closed scan). The
// gather and the recorded row are written by the PRODUCTION writers, so the
// recorded row carries its full key set (approval condition 1). Every refusal
// is proven by reading audit_entries AFTER the call.

const cmdPGBearer = "fhk_cmd_operator"

type cmdPGFixture struct {
	s       *Server
	runID   uuid.UUID
	stageID uuid.UUID
	arts    artifact.Repository
	audit   audit.Repository
	gather  *audit.Entry
	art     *artifact.Artifact
}

// newCMDPGFixture: a real run, a plan stage, a recorded gather, a comms_report
// artifact and its comms_report_recorded row. wrap, when non-nil, decorates the
// audit repo the server sees (the superseded test's hook).
func newCMDPGFixture(t *testing.T, wrap func(audit.Repository) audit.Repository) *cmdPGFixture {
	t.Helper()
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	runRepo := run.NewPostgresRepository(pool)
	f := &cmdPGFixture{arts: artifact.NewPostgresRepository(pool), audit: audit.NewPostgresRepository(pool)}
	rn, err := runRepo.CreateRun(ctx, run.CreateRunParams{
		Repo: "x/y", WorkflowID: "user_report_scan", WorkflowSHA: "abc", TriggerSource: run.TriggerCLI,
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
	f.runID, f.stageID = rn.ID, st.ID

	auditForServer := f.audit
	if wrap != nil {
		auditForServer = wrap(f.audit)
	}
	tokens := &stubAPITokenRepo{tok: &apitoken.Token{
		ID: uuid.New(), Subject: "github:ops", Scopes: []string{"read:runs", "write:approvals"}, PlainText: cmdPGBearer,
	}}
	f.s = New(Config{RunRepo: runRepo, ArtifactRepo: f.arts, AuditRepo: auditForServer, APITokenRepo: tokens})

	f.gather, _, err = f.s.recordCommsScanGathered(ctx, f.runID, f.stageID, cmdGather(cmdFixtureOpts{}))
	if err != nil {
		t.Fatalf("record gather: %v", err)
	}
	f.art = f.record(t, cmdReportBody(t, cmdFixtureOpts{}))
	return f
}

// record creates a comms_report artifact and appends its recorded row through
// the production writer, bound to the fixture's gather.
func (f *cmdPGFixture) record(t *testing.T, body []byte) *artifact.Artifact {
	t.Helper()
	ctx := context.Background()
	sv := plan.CommsReportVersion
	a, err := f.arts.Create(ctx, artifact.CreateParams{
		StageID: f.stageID, Kind: artifact.KindCommsReport, SchemaVersion: &sv,
		Content: body, ContentHash: sha256Hex(body),
	})
	if err != nil {
		t.Fatalf("create comms_report artifact: %v", err)
	}
	report, err := plan.ParseCommsReport(body)
	if err != nil {
		t.Fatal(err)
	}
	gathered, err := decodeCommsScanGathered(f.gather.Payload)
	if err != nil {
		t.Fatal(err)
	}
	payload := commsRecordedPayload(f.runID, f.stageID, a.ID.String(), a.ContentHash, len(body), report,
		commsBoundGather{entry: f.gather, payload: gathered}, cmdPreviews(cmdD2))
	if err := f.s.appendCommsRecorded(ctx, f.runID, f.stageID, payload); err != nil {
		t.Fatalf("append comms_report_recorded: %v", err)
	}
	return a
}

func (f *cmdPGFixture) do(t *testing.T, method, raw string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "/v0/runs/"+f.runID.String()+"/comms-dispositions", strings.NewReader(raw))
	req.Header.Set("Authorization", "Bearer "+cmdPGBearer)
	w := httptest.NewRecorder()
	f.s.Handler().ServeHTTP(w, req)
	return w
}

func (f *cmdPGFixture) dispositionRows(t *testing.T) int {
	t.Helper()
	rows, err := f.audit.ListForRunByCategory(context.Background(), f.runID, CategoryCommsDispositionRecorded)
	if err != nil {
		t.Fatalf("list dispositions: %v", err)
	}
	return len(rows)
}

// TestCommsDispositionsPG_HappyPathAndRead: a POST records two rows on the
// ATOMIC path; the GET through the real handler returns them with the recorded
// previews (JSONB-normalized, so compared as JSON), the unaccounted ids and
// the split cluster; the GET equals the POST echo.
func TestCommsDispositionsPG_HappyPathAndRead(t *testing.T) {
	f := newCMDPGFixture(t, nil)
	if _, ok := f.audit.(audit.FamilyWindowAppender); !ok {
		t.Fatal("production audit repo must carry FamilyWindowAppender")
	}
	w := f.do(t, http.MethodPost, cmdBatch(cmdEntryEpic(cmdD1, "approved", "#389"), cmdEntry(cmdD3, "rejected")))
	post := decodeCMD(t, w)
	if n := f.dispositionRows(t); n != 2 {
		t.Fatalf("committed rows = %d, want 2", n)
	}
	g := f.do(t, http.MethodGet, "")
	got := decodeCMD(t, g)
	if g.Body.String() != w.Body.String() {
		t.Errorf("GET differs from the POST echo:\nGET  %s\nPOST %s", g.Body.String(), w.Body.String())
	}
	if got.ArtifactID != f.art.ID.String() || len(got.Dispositions) != 2 || got.Dispositions[0].ParentEpic != "#389" || post.WindowClosed {
		t.Fatalf("read = %+v", got)
	}
	if !reflect.DeepEqual(got.UndecidedDraftIDs, []string{cmdD2}) || !reflect.DeepEqual(got.UnaccountedReportIDs, []string{"UR-issue-99"}) {
		t.Errorf("undecided = %v unaccounted = %v", got.UndecidedDraftIDs, got.UnaccountedReportIDs)
	}
	if !got.ClustersRecorded || len(got.ClusterSplits) != 1 || got.ClusterSplits[0].ReportIDs[0] != "UR-issue-12" {
		t.Errorf("clusters = %v %+v", got.ClustersRecorded, got.ClusterSplits)
	}
	var gotPreviews, wantPreviews any
	_ = json.Unmarshal(got.Previews, &gotPreviews)
	wantRaw, _ := json.Marshal(cmdPreviews(cmdD2).Previews)
	_ = json.Unmarshal(wantRaw, &wantPreviews)
	if !reflect.DeepEqual(gotPreviews, wantPreviews) {
		t.Errorf("previews = %s, want %s", got.Previews, wantRaw)
	}
}

// TestCommsDispositionsPG_WindowClosed: after AppendChainedFamilyWindowClose
// for the comms family, the next POST is 409 comms_window_closed and the row
// count is unchanged.
//
// COUNTERFACTUAL (the errors.As(&closed) mapping): delete it — 500 instead of
// 409; the row count is what proves nothing committed.
func TestCommsDispositionsPG_WindowClosed(t *testing.T) {
	f := newCMDPGFixture(t, nil)
	decodeCMD(t, f.do(t, http.MethodPost, cmdBatch(cmdEntry(cmdD1, "approved"))))
	sid := f.stageID
	wmPayload, _ := json.Marshal(map[string]any{"run_id": f.runID.String(), "artifact_id": f.art.ID.String(), "settlement": "approved"})
	_, consumed, err := f.audit.(audit.FamilyWindowAppender).AppendChainedFamilyWindowClose(context.Background(), audit.WindowFamilyComms,
		audit.ChainAppendParams{RunID: f.runID, StageID: &sid, Timestamp: time.Now().UTC(), Category: audit.CommsApplyWindowClosedCategory, Payload: wmPayload},
		f.art.ID.String())
	if err != nil || len(consumed) != 1 {
		t.Fatalf("close window: consumed %d, err %v", len(consumed), err)
	}
	w := f.do(t, http.MethodPost, cmdBatch(cmdEntry(cmdD3, "approved")))
	requireGDError(t, w, http.StatusConflict, "comms_window_closed")
	if n := f.dispositionRows(t); n != 1 {
		t.Fatalf("committed rows after a closed-window POST = %d, want 1 (unchanged)", n)
	}
	if !strings.Contains(w.Body.String(), `"watermark_sequence":`) || !strings.Contains(w.Body.String(), f.art.ID.String()) {
		t.Errorf("409 details missing watermark facts: %s", w.Body.String())
	}
	if got := decodeCMD(t, f.do(t, http.MethodGet, "")); !got.WindowClosed || got.Settlement == nil || got.Settlement.Settlement != "approved" {
		t.Errorf("read-back window = %v %+v", got.WindowClosed, got.Settlement)
	}
}

// cmdRaceAudit runs hook between the handler's resolution of report A and the
// atomic append, recording report B exactly where a concurrent ingest would.
type cmdRaceAudit struct {
	audit.Repository
	inner audit.FamilyWindowAppender
	hook  func()
}

func (a *cmdRaceAudit) AppendChainedFamilyDispositionBatch(ctx context.Context, family, artifactID string, ps []audit.ChainAppendParams) ([]*audit.Entry, error) {
	if a.hook != nil {
		a.hook()
	}
	return a.inner.AppendChainedFamilyDispositionBatch(ctx, family, artifactID, ps)
}

func (a *cmdRaceAudit) AppendChainedFamilyWindowClose(ctx context.Context, family string, p audit.ChainAppendParams, artifactID string) (*audit.Entry, []*audit.Entry, error) {
	return a.inner.AppendChainedFamilyWindowClose(ctx, family, p, artifactID)
}

// TestCommsDispositionsPG_Superseded: report B is recorded AFTER the capture
// resolved report A and BEFORE its append. The in-transaction binding
// re-check refuses 409 comms_report_superseded naming B, and ZERO disposition
// rows commit. A re-capture then binds to B.
//
// COUNTERFACTUAL (the errors.As(*audit.ReportSupersededError) case): delete it
// — 500 instead of 409. Rows are 0 either way (the audit layer refuses), so
// this arm is pinned on code identity.
func TestCommsDispositionsPG_Superseded(t *testing.T) {
	var race *cmdRaceAudit
	f := newCMDPGFixture(t, func(inner audit.Repository) audit.Repository {
		race = &cmdRaceAudit{Repository: inner, inner: inner.(audit.FamilyWindowAppender)}
		return race
	})
	var b *artifact.Artifact
	race.hook = func() {
		race.hook = nil // once
		// B's bytes must differ from A's (one stage, one content hash per
		// artifact): one trailing newline.
		b = f.record(t, append(cmdReportBody(t, cmdFixtureOpts{}), '\n'))
	}

	w := f.do(t, http.MethodPost, cmdBatch(cmdEntry(cmdD1, "approved")))
	requireGDError(t, w, http.StatusConflict, "comms_report_superseded")
	if b == nil {
		t.Fatal("the hook never ran: the race was not exercised")
	}
	if n := f.dispositionRows(t); n != 0 {
		t.Fatalf("committed rows = %d, want 0 on a superseded capture", n)
	}
	if d := ukErrorDetails(t, w); d["artifact_id"] != f.art.ID.String() || d["current_artifact_id"] != b.ID.String() {
		t.Errorf("details = %v, want artifact %s current %s", d, f.art.ID, b.ID)
	}
	got := decodeCMD(t, f.do(t, http.MethodPost, cmdBatch(cmdEntry(cmdD1, "approved"))))
	if got.ArtifactID != b.ID.String() || f.dispositionRows(t) != 1 {
		t.Fatalf("re-capture bound to %s with %d rows, want B %s with 1", got.ArtifactID, f.dispositionRows(t), b.ID)
	}
}

// TestCommsDispositionsPG_RefusalsWriteNothing: the 422 whole-batch refusal and
// the 400 parent_epic_is_source refusal through the real stack leave
// audit_entries without a disposition row.
func TestCommsDispositionsPG_RefusalsWriteNothing(t *testing.T) {
	f := newCMDPGFixture(t, nil)
	requireGDError(t, f.do(t, http.MethodPost, cmdBatch(cmdEntry(cmdD1, "approved"), cmdEntry("draft:UR-issue-99", "approved"))),
		http.StatusUnprocessableEntity, "comms_draft_unknown")
	requireGDError(t, f.do(t, http.MethodPost, cmdBatch(cmdEntryEpic(cmdD1, "approved", "12"))),
		http.StatusBadRequest, "validation_failed")
	if n := f.dispositionRows(t); n != 0 {
		t.Fatalf("committed rows = %d, want 0", n)
	}
}
