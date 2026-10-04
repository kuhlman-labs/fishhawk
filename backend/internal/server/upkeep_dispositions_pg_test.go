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

	"github.com/kuhlman-labs/fishhawk/backend/internal/apitoken"
	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// upkeep_dispositions_pg_test.go drives the upkeep capture END TO END against
// REAL Postgres through s.Handler(): the auth middleware, requireRunAccount,
// the handler, and the production audit repository's ATOMIC
// UpkeepWindowAppender path (run-row lock, binding re-check, watermark scan).
// Every refusal is proven by reading audit_entries AFTER the call.

const ukPGBearer = "fhk_uk_operator"

type ukPGFixture struct {
	s       *Server
	runID   uuid.UUID
	stageID uuid.UUID
	arts    artifact.Repository
	audit   audit.Repository
	art     *artifact.Artifact
}

// newUKPGFixture: a real run, a plan stage, a kind upkeep_report artifact and
// its upkeep_report_recorded row. wrap, when non-nil, decorates the audit repo
// the server sees (the race test's hook).
func newUKPGFixture(t *testing.T, wrap func(audit.Repository) audit.Repository) *ukPGFixture {
	t.Helper()
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	runRepo := run.NewPostgresRepository(pool)
	f := &ukPGFixture{arts: artifact.NewPostgresRepository(pool), audit: audit.NewPostgresRepository(pool)}
	rn, err := runRepo.CreateRun(ctx, run.CreateRunParams{
		Repo: "x/y", WorkflowID: "upkeep_scan", WorkflowSHA: "abc", TriggerSource: run.TriggerCLI,
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
	f.art = f.record(t, ukReportBody(t, ukFlake, ukDrift, ukDeprec))

	auditForServer := f.audit
	if wrap != nil {
		auditForServer = wrap(f.audit)
	}
	tokens := &stubAPITokenRepo{tok: &apitoken.Token{
		ID: uuid.New(), Subject: "github:ops", Scopes: []string{"read:runs", "write:approvals"}, PlainText: ukPGBearer,
	}}
	f.s = New(Config{RunRepo: runRepo, ArtifactRepo: f.arts, AuditRepo: auditForServer, APITokenRepo: tokens})
	return f
}

// record creates an upkeep_report artifact and appends its recorded row, as
// the ingest does.
func (f *ukPGFixture) record(t *testing.T, body []byte) *artifact.Artifact {
	t.Helper()
	ctx := context.Background()
	sv := plan.UpkeepReportVersion
	a, err := f.arts.Create(ctx, artifact.CreateParams{
		StageID: f.stageID, Kind: artifact.KindUpkeepReport, SchemaVersion: &sv,
		Content: body, ContentHash: sha256Hex(body),
	})
	if err != nil {
		t.Fatalf("create upkeep_report artifact: %v", err)
	}
	payload, _ := json.Marshal(map[string]any{"run_id": f.runID.String(), "stage_id": f.stageID.String(), "artifact_id": a.ID.String()})
	sid := f.stageID
	if _, err := f.audit.AppendChained(ctx, audit.ChainAppendParams{
		RunID: f.runID, StageID: &sid, Timestamp: time.Now().UTC(),
		Category: CategoryUpkeepReportRecorded, Payload: payload,
	}); err != nil {
		t.Fatalf("append upkeep_report_recorded: %v", err)
	}
	return a
}

func (f *ukPGFixture) post(t *testing.T, raw string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v0/runs/"+f.runID.String()+"/upkeep-dispositions", strings.NewReader(raw))
	req.Header.Set("Authorization", "Bearer "+ukPGBearer)
	w := httptest.NewRecorder()
	f.s.Handler().ServeHTTP(w, req)
	return w
}

func (f *ukPGFixture) dispositionRows(t *testing.T) int {
	t.Helper()
	rows, err := f.audit.ListForRunByCategory(context.Background(), f.runID, CategoryUpkeepDispositionRecorded)
	if err != nil {
		t.Fatalf("list dispositions: %v", err)
	}
	return len(rows)
}

// TestUpkeepDispositionsPG_AtomicCaptureThenWindowClosed: a POST records two
// rows on the ATOMIC path; after AppendChainedUpkeepWindowClose the next POST
// is 409 upkeep_window_closed and the row count is unchanged.
//
// COUNTERFACTUAL (the handler's errors.As(&closed) mapping): delete it — the
// request returns 500 instead of 409. The row count after the call is what
// proves nothing committed.
func TestUpkeepDispositionsPG_AtomicCaptureThenWindowClosed(t *testing.T) {
	f := newUKPGFixture(t, nil)
	if _, ok := f.audit.(audit.UpkeepWindowAppender); !ok {
		t.Fatal("production audit repo must carry UpkeepWindowAppender")
	}
	w := f.post(t, ukBatch(ukEntry(ukFlake, "approved"), ukEntry(ukDrift, "rejected")))
	got := decodeUK(t, w)
	if got.ArtifactID != f.art.ID.String() || len(got.Dispositions) != 2 || got.WindowClosed {
		t.Fatalf("POST = %+v", got)
	}
	if n := f.dispositionRows(t); n != 2 {
		t.Fatalf("committed rows = %d, want 2", n)
	}

	sid := f.stageID
	wmPayload, _ := json.Marshal(map[string]any{"run_id": f.runID.String(), "artifact_id": f.art.ID.String(), "settlement": "approved"})
	wm, consumed, err := f.audit.(audit.UpkeepWindowAppender).AppendChainedUpkeepWindowClose(context.Background(),
		audit.ChainAppendParams{RunID: f.runID, StageID: &sid, Timestamp: time.Now().UTC(), Category: audit.UpkeepApplyWindowClosedCategory, Payload: wmPayload},
		f.art.ID.String())
	if err != nil {
		t.Fatalf("close window: %v", err)
	}
	if len(consumed) != 2 {
		t.Fatalf("consumed = %d, want 2", len(consumed))
	}

	w = f.post(t, ukBatch(ukEntry(ukDeprec, "approved")))
	requireGDError(t, w, http.StatusConflict, "upkeep_window_closed")
	if n := f.dispositionRows(t); n != 2 {
		t.Fatalf("committed rows after a closed-window POST = %d, want 2 (unchanged)", n)
	}
	if !strings.Contains(w.Body.String(), `"watermark_sequence":`) || !strings.Contains(w.Body.String(), f.art.ID.String()) {
		t.Errorf("409 details missing watermark facts: %s", w.Body.String())
	}
	_ = wm
}

// ukRaceAudit is the appender HOOK for the rebind race (#3923 approval
// condition 1): it runs between the handler's resolution of report A and the
// atomic append, recording report B exactly where a concurrent ingest would.
type ukRaceAudit struct {
	audit.Repository
	inner audit.UpkeepWindowAppender
	hook  func()
}

func (a *ukRaceAudit) AppendChainedUpkeepDispositionBatch(ctx context.Context, artifactID string, ps []audit.ChainAppendParams) ([]*audit.Entry, error) {
	if a.hook != nil {
		a.hook()
	}
	return a.inner.AppendChainedUpkeepDispositionBatch(ctx, artifactID, ps)
}

func (a *ukRaceAudit) AppendChainedUpkeepWindowClose(ctx context.Context, p audit.ChainAppendParams, artifactID string) (*audit.Entry, []*audit.Entry, error) {
	return a.inner.AppendChainedUpkeepWindowClose(ctx, p, artifactID)
}

// TestUpkeepDispositionsPG_RebindRaceRefusedSuperseded: report B is recorded
// AFTER the capture resolved report A and BEFORE its append. The in-transaction
// binding re-check refuses 409 upkeep_report_superseded naming B, and ZERO
// disposition rows commit.
//
// COUNTERFACTUAL (audit checkReportBinding): make it return nil — the request
// returns 200 and the rows land against the superseded A.
func TestUpkeepDispositionsPG_RebindRaceRefusedSuperseded(t *testing.T) {
	var race *ukRaceAudit
	f := newUKPGFixture(t, func(inner audit.Repository) audit.Repository {
		race = &ukRaceAudit{Repository: inner, inner: inner.(audit.UpkeepWindowAppender)}
		return race
	})
	var b *artifact.Artifact
	race.hook = func() {
		race.hook = nil // once
		b = f.record(t, ukReportBody(t, ukFlake, ukDeprec))
	}

	w := f.post(t, ukBatch(ukEntry(ukFlake, "approved")))
	requireGDError(t, w, http.StatusConflict, "upkeep_report_superseded")
	if b == nil {
		t.Fatal("the hook never ran: the race was not exercised")
	}
	if n := f.dispositionRows(t); n != 0 {
		t.Fatalf("committed rows = %d, want 0 on a superseded capture", n)
	}
	var env struct {
		Error struct {
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &env)
	if env.Error.Details["artifact_id"] != f.art.ID.String() || env.Error.Details["current_artifact_id"] != b.ID.String() {
		t.Errorf("details = %v, want artifact %s current %s", env.Error.Details, f.art.ID, b.ID)
	}

	// Re-capture now binds to B.
	got := decodeUK(t, f.post(t, ukBatch(ukEntry(ukFlake, "approved"))))
	if got.ArtifactID != b.ID.String() || f.dispositionRows(t) != 1 {
		t.Fatalf("re-capture bound to %s with %d rows, want B %s with 1", got.ArtifactID, f.dispositionRows(t), b.ID)
	}
}
