package server

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/intakegroom"
	"github.com/kuhlman-labs/fishhawk/backend/internal/orchestrator"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/upkeep"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// upkeep_report ingest tests (#3921, slice 2). Every case drives a SIGNED POST
// through the real router (shipPlanRequest): request → DetectArtifactKind →
// guard / handleUpkeepReport → binding from the run row's cached spec →
// ParseUpkeepReport → run-ref check → dedupe through intakeCandidates and a
// registered fake reader → artifact row + chained audit row → stage state, all
// read back from the fakes after the call.
//
// The RunRepo is the slice-1 multi-run wrapper (upkeepRunRepo), never an edit
// to the shared promptRunRepo (approval condition 4).

// The shipped example's two cited runs.
const (
	upkeepCitedRunA = "6f1c2a3e-8b4d-4e5f-9a6b-7c8d9e0f1a2b"
	upkeepCitedRunB = "9b8a7c6d-5e4f-4a3b-8c2d-1e0f9a8b7c6d"
)

// upkeepRereadFailRepo fails GetRun for the reporting run only AFTER the
// binding has listed the run's stages, so the binding (and the router's
// ownership middleware before it) read the run normally and only the
// handler's own re-read fails. Keyed on ListStagesForRun rather than a call
// count, which the middleware's read would shift.
type upkeepRereadFailRepo struct {
	*upkeepRunRepo

	id  uuid.UUID
	err error
}

func (r *upkeepRereadFailRepo) GetRun(ctx context.Context, id uuid.UUID) (*run.Run, error) {
	if id == r.id && r.listCalls() > 0 {
		return nil, r.err
	}
	return r.upkeepRunRepo.GetRun(ctx, id)
}

// upkeepNthGetRunFailRepo fails GetRun for id from its from-th call on
// (1-based) and counts every call for id (#3924 carried item 5). The router's
// run-ownership middleware (requireRunAccount → resolveRunForAuthz) reads the
// run FIRST, so from = 2 lets that read SUCCEED and fails only the handler's
// later read — the realistic transient, with the account check exercised. An
// id-keyed injection reaches the handler too, but only because that
// middleware falls through on a load error. Either way the keying is not what
// proves the branch: the tests assert the 500's message names the step.
type upkeepNthGetRunFailRepo struct {
	*upkeepRunRepo

	id   uuid.UUID
	from int
	err  error

	callMu sync.Mutex
	calls  int
}

func (r *upkeepNthGetRunFailRepo) GetRun(ctx context.Context, id uuid.UUID) (*run.Run, error) {
	if id == r.id {
		r.callMu.Lock()
		r.calls++
		n := r.calls
		r.callMu.Unlock()
		if n >= r.from {
			return nil, r.err
		}
	}
	return r.upkeepRunRepo.GetRun(ctx, id)
}

func (r *upkeepNthGetRunFailRepo) getRunCalls() int {
	r.callMu.Lock()
	defer r.callMu.Unlock()
	return r.calls
}

// newUpkeepLateGetRunFailFixture is the ingest fixture with the reporting
// run's GetRun failing from its second call on.
func newUpkeepLateGetRunFailFixture(t *testing.T) (*upkeepIngestFixture, *upkeepNthGetRunFailRepo) {
	t.Helper()
	var repo *upkeepNthGetRunFailRepo
	f := newUpkeepIngestFixtureWith(t, upkeepScanSpec(t), "upkeep_scan", nil, func(rr *upkeepRunRepo) run.Repository {
		repo = &upkeepNthGetRunFailRepo{upkeepRunRepo: rr, from: 2, err: errors.New("connection reset")}
		return repo
	})
	repo.id = f.runRow.ID
	return f, repo
}

// upkeepErrorMessage returns the error envelope's message.
func upkeepErrorMessage(resp map[string]any) string {
	errObj, _ := resp["error"].(map[string]any)
	msg, _ := errObj["message"].(string)
	return msg
}

type upkeepIngestFixture struct {
	s         *Server
	sf        *signingFake
	ar        *fakeArtifactRepo
	au        *auditFake
	rr        *upkeepRunRepo
	runRow    *run.Run
	planStage *run.Stage
	implStage *run.Stage
	priv      ed25519.PrivateKey
	reader    *igReadProvider
}

// upkeepExampleBody is the shipped canonical example — valid, three findings,
// two cited runs.
func upkeepExampleBody(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("../../../docs/spec/examples/upkeep-report-v1-example.json")
	if err != nil {
		t.Fatalf("read upkeep example: %v", err)
	}
	return b
}

// newUpkeepIngestRepo seeds the reporting run (cached wfSpec + workflowID), its
// plan (sequence 0) and implement (sequence 1) stages, and the example's two
// cited runs under the same repo.
func newUpkeepIngestRepo(wfSpec []byte, workflowID string, runID, planStageID uuid.UUID) (*upkeepRunRepo, *run.Run, *run.Stage, *run.Stage) {
	rr := newUpkeepRunRepo()
	inst := int64(4242)
	runRow := &run.Run{
		// Untenanted: the run-ownership middleware 403s a tenanted run for the
		// test's identity-less caller. The account arm of the run-ref check is
		// unit-tested in upkeep_binding_test.go.
		ID: runID, Repo: upkeepTestRepo, InstallationID: &inst,
		WorkflowID: workflowID, WorkflowSpec: wfSpec, State: run.StateRunning,
	}
	rr.seedRun(runRow)
	planStage := &run.Stage{ID: planStageID, RunID: runID, Sequence: 0, Type: run.StageTypePlan,
		State: run.StageStateRunning, RequiresApproval: true}
	implStage := &run.Stage{ID: uuid.New(), RunID: runID, Sequence: 1, Type: run.StageTypeImplement,
		State: run.StageStateRunning}
	rr.getStages[planStage.ID] = planStage
	rr.getStages[implStage.ID] = implStage
	rr.stagesByRunID = map[uuid.UUID][]*run.Stage{runID: {implStage, planStage}}
	for _, id := range []string{upkeepCitedRunA, upkeepCitedRunB} {
		rr.seedRun(&run.Run{ID: uuid.MustParse(id), Repo: upkeepTestRepo})
	}
	return rr, runRow, planStage, implStage
}

// newUpkeepSettleServer is the settle-row hook for
// TestShipPlan_EverySiblingSettlesItsStage: the multi-run wrapper installed as
// the server's RunRepo, seeded with an upkeep-scan run whose plan stage is
// stageID, plus a registered empty fake reader so no real forge is reached.
// It returns the wrapper's embedded promptRunRepo, whose getStages the settle
// table reads. It installs the default conventions itself (#3924 carried
// item 2) rather than relying on the caller's, so the dedupe's conventions
// load never reaches the real forge whichever test builds it.
func newUpkeepSettleServer(t *testing.T, runID, stageID uuid.UUID) (*Server, *signingFake, *promptRunRepo) {
	t.Helper()
	igRegisterReadProvider(t, &igReadProvider{})
	installConventions(t, workmgmt.Default(), nil)
	rr, _, _, _ := newUpkeepIngestRepo(upkeepScanSpec(t), "upkeep_scan", runID, stageID)
	sf := newSigningFake()
	s := New(Config{
		Addr: "127.0.0.1:0", SigningRepo: sf, ArtifactRepo: newFakeArtifactRepo(),
		AuditRepo: newAuditFake(), RunRepo: rr,
	})
	return s, sf, rr.promptRunRepo
}

func newUpkeepIngestFixtureWith(t *testing.T, wfSpec []byte, workflowID string, reader *igReadProvider, wrap func(*upkeepRunRepo) run.Repository) *upkeepIngestFixture {
	t.Helper()
	if reader == nil {
		reader = &igReadProvider{}
	}
	igRegisterReadProvider(t, reader)
	installConventions(t, workmgmt.Default(), nil)
	rr, runRow, planStage, implStage := newUpkeepIngestRepo(wfSpec, workflowID, uuid.New(), uuid.New())
	var repo run.Repository = rr
	if wrap != nil {
		repo = wrap(rr)
	}
	f := &upkeepIngestFixture{
		sf: newSigningFake(), ar: newFakeArtifactRepo(), au: newAuditFake(), rr: rr,
		runRow: runRow, planStage: planStage, implStage: implStage, reader: reader,
	}
	f.s = New(Config{
		Addr: "127.0.0.1:0", SigningRepo: f.sf, ArtifactRepo: f.ar, AuditRepo: f.au, RunRepo: repo,
		Logger: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
	})
	f.priv, _ = f.sf.issue(t, runRow.ID)
	return f
}

func newUpkeepIngestFixture(t *testing.T, reader *igReadProvider) *upkeepIngestFixture {
	t.Helper()
	return newUpkeepIngestFixtureWith(t, upkeepScanSpec(t), "upkeep_scan", reader, nil)
}

func (f *upkeepIngestFixture) post(t *testing.T, stageID uuid.UUID, body []byte) (int, map[string]any) {
	t.Helper()
	w := shipPlanRequest(t, f.s, f.runRow.ID, stageID, f.priv, body, "")
	var m map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &m)
	return w.Code, m
}

// recorded returns every upkeep_report_recorded payload, decoded raw.
func (f *upkeepIngestFixture) recorded(t *testing.T) []map[string]json.RawMessage {
	t.Helper()
	f.au.mu.Lock()
	defer f.au.mu.Unlock()
	var out []map[string]json.RawMessage
	for _, e := range f.au.appended {
		if e.Category != CategoryUpkeepReportRecorded {
			continue
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(e.Payload, &m); err != nil {
			t.Fatalf("decode recorded payload: %v", err)
		}
		out = append(out, m)
	}
	return out
}

func (f *upkeepIngestFixture) artifacts(kind artifact.Kind) int {
	f.ar.mu.Lock()
	defer f.ar.mu.Unlock()
	n := 0
	for _, a := range f.ar.all {
		if a.Kind == kind {
			n++
		}
	}
	return n
}

func (f *upkeepIngestFixture) stageState(id uuid.UUID) run.StageState {
	return f.rr.getStages[id].State
}

// upkeepErrorDetails returns the 400's details object.
func upkeepErrorDetails(t *testing.T, resp map[string]any) map[string]any {
	t.Helper()
	errObj, _ := resp["error"].(map[string]any)
	d, _ := errObj["details"].(map[string]any)
	if d == nil {
		t.Fatalf("response carries no error.details: %v", resp)
	}
	return d
}

func upkeepErrorCode(resp map[string]any) string {
	errObj, _ := resp["error"].(map[string]any)
	code, _ := errObj["code"].(string)
	return code
}

// upkeepDuplicatesOf decodes a recorded row's duplicates, asserting it is a
// JSON ARRAY (never absent or null — #3924 reads it).
func upkeepDuplicatesOf(t *testing.T, row map[string]json.RawMessage) []upkeep.Duplicate {
	t.Helper()
	raw, ok := row["duplicates"]
	if !ok || !bytes.HasPrefix(bytes.TrimSpace(raw), []byte("[")) {
		t.Fatalf("duplicates = %s (present=%v), want a JSON array", raw, ok)
	}
	var d []upkeep.Duplicate
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatalf("decode duplicates: %v", err)
	}
	return d
}

// (s1) happy path.
func TestUpkeepReportIngest_HappyPath(t *testing.T) {
	f := newUpkeepIngestFixture(t, nil)
	code, resp := f.post(t, f.planStage.ID, upkeepExampleBody(t))
	if code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %v", code, resp)
	}
	if resp["schema_version"] != plan.UpkeepReportVersion || resp["idempotent"] != false {
		t.Errorf("response = %v, want schema_version %s, idempotent false", resp, plan.UpkeepReportVersion)
	}
	if n := f.artifacts(artifact.KindUpkeepReport); n != 1 {
		t.Fatalf("upkeep_report artifacts = %d, want 1", n)
	}
	if got := *f.ar.all[0].SchemaVersion; got != plan.UpkeepReportVersion {
		t.Errorf("artifact schema_version = %q, want %q", got, plan.UpkeepReportVersion)
	}
	rows := f.recorded(t)
	if len(rows) != 1 {
		t.Fatalf("upkeep_report_recorded rows = %d, want 1", len(rows))
	}
	var counts map[string]int
	if err := json.Unmarshal(rows[0]["entry_counts"], &counts); err != nil {
		t.Fatalf("decode entry_counts: %v", err)
	}
	want := map[string]int{"findings": 3, "flake": 1, "toolchain_drift": 1, "deprecation": 1, "advisory": 0}
	for k, v := range want {
		if got, ok := counts[k]; !ok || got != v {
			t.Errorf("entry_counts[%s] = %d (present=%v), want %d", k, got, ok, v)
		}
	}
	// #3750: a report with no advisory finding records an empty covered
	// array, no coverage degrade, and an empty source_degrades array.
	for k, wantRaw := range map[string]string{"covered": "[]", "source_degrades": "[]", "coverage_degraded": "false",
		"coverage_scanned_pulls": "0", "coverage_window_truncated": "false"} {
		if got := string(rows[0][k]); got != wantRaw {
			t.Errorf("%s = %s, want %s", k, got, wantRaw)
		}
	}
	if _, ok := rows[0]["coverage_degrade_reason"]; ok {
		t.Error("coverage_degrade_reason present on a healthy coverage read")
	}
	if d := upkeepDuplicatesOf(t, rows[0]); len(d) != 0 {
		t.Errorf("duplicates = %+v, want [] over an empty window", d)
	}
	if string(rows[0]["dedupe_degraded"]) != "false" {
		t.Errorf("dedupe_degraded = %s, want false", rows[0]["dedupe_degraded"])
	}
	if _, ok := rows[0]["dedupe_degrade_reason"]; ok {
		t.Error("dedupe_degrade_reason present on a healthy dedupe")
	}
	for _, k := range []string{"artifact_id", "content_hash", "size_bytes", "dedupe_scanned_items", "dedupe_window_truncated"} {
		if _, ok := rows[0][k]; !ok {
			t.Errorf("recorded row missing %q", k)
		}
	}
	if got := f.stageState(f.planStage.ID); got != run.StageStateAwaitingApproval {
		t.Errorf("stage = %q, want awaiting_approval", got)
	}
}

// (s2) a byte-identical re-POST is idempotent.
func TestUpkeepReportIngest_IdempotentRepost(t *testing.T) {
	f := newUpkeepIngestFixture(t, nil)
	body := upkeepExampleBody(t)
	if code, resp := f.post(t, f.planStage.ID, body); code != http.StatusCreated {
		t.Fatalf("first status = %d: %v", code, resp)
	}
	code, resp := f.post(t, f.planStage.ID, body)
	if code != http.StatusOK || resp["idempotent"] != true {
		t.Fatalf("re-POST = %d %v, want 200 idempotent", code, resp)
	}
	if n := f.artifacts(artifact.KindUpkeepReport); n != 1 {
		t.Errorf("artifacts = %d, want 1", n)
	}
	if n := len(f.recorded(t)); n != 1 {
		t.Errorf("recorded rows = %d, want 1", n)
	}
}

// (s3) the audit append fails after Create: 500 with the artifact durable and
// the stage still running; the retry HEALS the row and settles.
func TestUpkeepReportIngest_HealsMissingRecordedRow(t *testing.T) {
	f := newUpkeepIngestFixture(t, nil)
	body := upkeepExampleBody(t)
	f.au.appendErrCategory = CategoryUpkeepReportRecorded
	if code, resp := f.post(t, f.planStage.ID, body); code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %v", code, resp)
	}
	if n := f.artifacts(artifact.KindUpkeepReport); n != 1 {
		t.Fatalf("artifacts after the failed append = %d, want 1 (durable)", n)
	}
	if got := f.stageState(f.planStage.ID); got != run.StageStateRunning {
		t.Fatalf("stage = %q after a 500, want running", got)
	}
	f.au.appendErrCategory = ""
	code, resp := f.post(t, f.planStage.ID, body)
	if code != http.StatusOK || resp["idempotent"] != true {
		t.Fatalf("retry = %d %v, want 200 idempotent", code, resp)
	}
	if n := len(f.recorded(t)); n != 1 {
		t.Errorf("recorded rows after the heal = %d, want exactly 1", n)
	}
	if got := f.stageState(f.planStage.ID); got != run.StageStateAwaitingApproval {
		t.Errorf("stage = %q, want awaiting_approval", got)
	}
}

// The heal's own existence check failing is a 500, never a gapped 200.
func TestUpkeepReportIngest_HealReadFailure500(t *testing.T) {
	f := newUpkeepIngestFixture(t, nil)
	body := upkeepExampleBody(t)
	if code, _ := f.post(t, f.planStage.ID, body); code != http.StatusCreated {
		t.Fatalf("first status = %d", code)
	}
	f.au.listByCategoryErrCategory = CategoryUpkeepReportRecorded
	if code, resp := f.post(t, f.planStage.ID, body); code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %v", code, resp)
	}
}

// (s4) Create fails: 500, the stage still running, nothing recorded.
func TestUpkeepReportIngest_CreateFailure500(t *testing.T) {
	f := newUpkeepIngestFixture(t, nil)
	f.ar.createErr = errors.New("disk full")
	if code, resp := f.post(t, f.planStage.ID, upkeepExampleBody(t)); code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %v", code, resp)
	}
	if got := f.stageState(f.planStage.ID); got != run.StageStateRunning {
		t.Errorf("stage = %q, want running", got)
	}
	if n := len(f.recorded(t)); n != 0 {
		t.Errorf("recorded rows = %d, want 0", n)
	}
}

// A GetByHash error that is not ErrNotFound is a 500 with nothing written.
func TestUpkeepReportIngest_GetByHashFailure500(t *testing.T) {
	f := newUpkeepIngestFixture(t, nil)
	f.ar.getByHashErr = errors.New("db down")
	if code, resp := f.post(t, f.planStage.ID, upkeepExampleBody(t)); code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %v", code, resp)
	}
	if n := f.artifacts(artifact.KindUpkeepReport); n != 0 {
		t.Errorf("artifacts = %d, want 0", n)
	}
	if got := f.stageState(f.planStage.ID); got != run.StageStateRunning {
		t.Errorf("stage = %q, want running", got)
	}
}

// (s5) the dedupe AC: an OPEN marker issue and an OPEN title restatement mark
// their findings; a CLOSED issue carrying the drift marker marks nothing.
func TestUpkeepReportIngest_DedupeMarksOpenDuplicates(t *testing.T) {
	reader := &igReadProvider{items: []workmgmt.WorkItemRecord{
		{Number: 43, Title: "Unrelated sweep", Body: upkeep.FindingMarker("toolchain_drift:go"), State: "closed", URL: "https://example.test/43"},
		{Number: 42, Title: "Replace deprecated io/ioutil calls", URL: "https://example.test/42"},
		{Number: 41, Title: "Unrelated invoices rollup", Body: "filed\n" + upkeep.FindingMarker("flake:TestWidgetSync"), URL: "https://example.test/41"},
	}}
	f := newUpkeepIngestFixture(t, reader)
	if code, resp := f.post(t, f.planStage.ID, upkeepExampleBody(t)); code != http.StatusCreated {
		t.Fatalf("status = %d: %v", code, resp)
	}
	rows := f.recorded(t)
	if len(rows) != 1 {
		t.Fatalf("recorded rows = %d, want 1", len(rows))
	}
	got := map[string]upkeep.Duplicate{}
	for _, d := range upkeepDuplicatesOf(t, rows[0]) {
		got[d.FindingID] = d
	}
	if d, ok := got["flake:TestWidgetSync"]; !ok || d.IssueNumber != 41 || d.Basis != upkeep.BasisMarker {
		t.Errorf("flake duplicate = %+v (present=%v), want #41 by marker", d, ok)
	}
	if d, ok := got["deprecation:io/ioutil"]; !ok || d.IssueNumber != 42 || d.Basis != upkeep.BasisSimilarity {
		t.Errorf("deprecation duplicate = %+v (present=%v), want #42 by similarity", d, ok)
	}
	if d, ok := got["toolchain_drift:go"]; ok {
		t.Errorf("drift finding marked duplicate of %+v; a CLOSED issue must never mark", d)
	}
	if len(got) != 2 {
		t.Errorf("duplicates = %+v, want exactly the two open matches", got)
	}
}

// (s6) the reader fails: the report is still ingested, the degrade is named,
// duplicates is [].
func TestUpkeepReportIngest_DedupeDegradedStillIngests(t *testing.T) {
	f := newUpkeepIngestFixture(t, &igReadProvider{listErr: errors.New("forge said no")})
	if code, resp := f.post(t, f.planStage.ID, upkeepExampleBody(t)); code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %v", code, resp)
	}
	rows := f.recorded(t)
	if len(rows) != 1 {
		t.Fatalf("recorded rows = %d, want 1", len(rows))
	}
	if string(rows[0]["dedupe_degraded"]) != "true" {
		t.Errorf("dedupe_degraded = %s, want true", rows[0]["dedupe_degraded"])
	}
	var reason string
	_ = json.Unmarshal(rows[0]["dedupe_degrade_reason"], &reason)
	if reason != string(intakegroom.DegradeReasonReaderError) {
		t.Errorf("dedupe_degrade_reason = %q, want reader_error", reason)
	}
	if d := upkeepDuplicatesOf(t, rows[0]); len(d) != 0 {
		t.Errorf("duplicates = %+v, want [] on a degrade", d)
	}
	if got := f.stageState(f.planStage.ID); got != run.StageStateAwaitingApproval {
		t.Errorf("stage = %q, want awaiting_approval", got)
	}
}

// assertUpkeepRefused asserts a 400 with code and details.reason, the stage
// failed, and nothing stored or recorded.
func assertUpkeepRefused(t *testing.T, f *upkeepIngestFixture, stageID uuid.UUID, code int, resp map[string]any, wantCode, wantReason string) {
	t.Helper()
	if code != http.StatusBadRequest || upkeepErrorCode(resp) != wantCode {
		t.Fatalf("response = %d %v, want 400 %s", code, resp, wantCode)
	}
	if wantReason != "" {
		if got := upkeepErrorDetails(t, resp)["reason"]; got != wantReason {
			t.Errorf("details.reason = %v, want %q", got, wantReason)
		}
	}
	if got := f.stageState(stageID); got != run.StageStateFailed {
		t.Errorf("stage = %q, want failed (category-B)", got)
	}
	if got := upkeepPersistedFailureCategory(f, stageID); got != run.FailureB {
		t.Errorf("persisted failure category = %q, want %q", got, run.FailureB)
	}
	if n := f.artifacts(artifact.KindUpkeepReport); n != 0 {
		t.Errorf("upkeep_report artifacts = %d, want 0", n)
	}
	if n := len(f.recorded(t)); n != 0 {
		t.Errorf("recorded rows = %d, want 0", n)
	}
}

// upkeepPersistedFailureCategory returns the failure category the LAST
// transition of stageID to failed persisted, or "" when there was none. The
// fake has no compare-and-swap capability, so run.FailStage writes the
// category through TransitionStage's completion (#3924 carried item 4).
func upkeepPersistedFailureCategory(f *upkeepIngestFixture, stageID uuid.UUID) run.FailureCategory {
	var got run.FailureCategory
	for _, c := range f.rr.transitionStageCalls {
		if c.StageID != stageID || c.To != run.StageStateFailed {
			continue
		}
		got = ""
		if c.Completion != nil && c.Completion.FailureCategory != nil {
			got = *c.Completion.FailureCategory
		}
	}
	return got
}

// (s7) an implement-typed stage. The REASON isolates the type guard: without
// it the binding refuses with stage_does_not_declare_upkeep_report instead.
func TestUpkeepReportIngest_NonPlanStageRefused(t *testing.T) {
	f := newUpkeepIngestFixture(t, nil)
	code, resp := f.post(t, f.implStage.ID, upkeepExampleBody(t))
	assertUpkeepRefused(t, f, f.implStage.ID, code, resp, "upkeep_report_stage_invalid", upkeepRefusalStageTypeNotPlan)
}

// (s8) carried #3920 note 1: an ordinary workflow's plan stage, which declares
// only `plan`, cannot ship an upkeep_report.
func TestUpkeepReportIngest_UndeclaringStageRefused(t *testing.T) {
	f := newUpkeepIngestFixtureWith(t, []byte(appliesToSpec("")), "guarded", nil, nil)
	code, resp := f.post(t, f.planStage.ID, upkeepExampleBody(t))
	assertUpkeepRefused(t, f, f.planStage.ID, code, resp, "upkeep_report_stage_invalid", upkeepRefusalStageDoesNotDeclare)
}

// (s9) no cached spec: the binding is undecidable and the ingest fails CLOSED.
func TestUpkeepReportIngest_NoCachedSpecRefused(t *testing.T) {
	f := newUpkeepIngestFixtureWith(t, nil, "upkeep_scan", nil, nil)
	code, resp := f.post(t, f.planStage.ID, upkeepExampleBody(t))
	assertUpkeepRefused(t, f, f.planStage.ID, code, resp, "upkeep_report_stage_invalid", upkeepRefusalStageBindingUndecidable)
}

// (s10) an evidence-less finding fails the SCHEMA. Semantic rule (f) would
// also refuse an evidence-less flake, so the assertion pins the SCHEMA layer's
// error ("plan: schema:"), not just the pointer — otherwise rule (f) masks a
// deleted schema step.
func TestUpkeepReportIngest_InvalidReportRefused(t *testing.T) {
	f := newUpkeepIngestFixture(t, nil)
	var m map[string]any
	if err := json.Unmarshal(upkeepExampleBody(t), &m); err != nil {
		t.Fatal(err)
	}
	m["findings"].([]any)[0].(map[string]any)["evidence"] = []any{}
	body, _ := json.Marshal(m)
	code, resp := f.post(t, f.planStage.ID, body)
	assertUpkeepRefused(t, f, f.planStage.ID, code, resp, "upkeep_report_invalid", "")
	if e, _ := upkeepErrorDetails(t, resp)["error"].(string); !strings.HasPrefix(e, "plan: schema:") || !strings.Contains(e, "/findings/0/evidence") {
		t.Errorf("details.error = %q, want a schema error naming /findings/0/evidence", e)
	}
}

// (s11) a cited run under another repo, and an unknown cited run: the same
// code and the same details keys, so the tenant cannot tell them apart.
func TestUpkeepReportIngest_RunRefRefused(t *testing.T) {
	cases := map[string]func(rr *upkeepRunRepo){
		"foreign repo": func(rr *upkeepRunRepo) {
			rr.seedRun(&run.Run{ID: uuid.MustParse(upkeepCitedRunA), Repo: "someone-else/other"})
		},
		"unknown run": func(rr *upkeepRunRepo) { delete(rr.getRuns, uuid.MustParse(upkeepCitedRunA)) },
	}
	var keySets []string
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := newUpkeepIngestFixture(t, nil)
			mutate(f.rr)
			code, resp := f.post(t, f.planStage.ID, upkeepExampleBody(t))
			assertUpkeepRefused(t, f, f.planStage.ID, code, resp, "upkeep_report_invalid", upkeepRunRefInvalid)
			d := upkeepErrorDetails(t, resp)
			if d["run_id"] != upkeepCitedRunA {
				t.Errorf("details.run_id = %v, want %s", d["run_id"], upkeepCitedRunA)
			}
			var keys []string
			for k := range d {
				keys = append(keys, k)
			}
			keySets = append(keySets, strings.Join(sortedStrings(keys), ","))
		})
	}
	if len(keySets) == 2 && keySets[0] != keySets[1] {
		t.Errorf("details keys differ between unknown and foreign: %q vs %q", keySets[0], keySets[1])
	}
}

func sortedStrings(in []string) []string {
	out := append([]string(nil), in...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// A cited run whose lookup does not ANSWER is a 500, stage untouched.
func TestUpkeepReportIngest_RunRefTransportError500(t *testing.T) {
	f := newUpkeepIngestFixture(t, nil)
	f.rr.getRunErrs[uuid.MustParse(upkeepCitedRunB)] = errors.New("connection reset")
	code, resp := f.post(t, f.planStage.ID, upkeepExampleBody(t))
	if code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %v", code, resp)
	}
	if msg := upkeepErrorMessage(resp); msg != "check cited run refs failed" {
		t.Errorf("message = %q, want the run-ref step's", msg)
	}
	if got := f.stageState(f.planStage.ID); got != run.StageStateRunning {
		t.Errorf("stage = %q, want running", got)
	}
	if n := f.artifacts(artifact.KindUpkeepReport); n != 0 {
		t.Errorf("artifacts = %d, want 0", n)
	}
}

// (s12) a GetRun transport error during the binding: 500, nothing stored. The
// error is injected on the read AFTER the ownership middleware's (#3924
// carried item 5), and the message names the binding step, so the 500 proves
// the binding's branch was reached rather than the middleware's.
func TestUpkeepReportIngest_BindingTransportError500(t *testing.T) {
	f, repo := newUpkeepLateGetRunFailFixture(t)
	// An upkeep_report is outside the comms guard's allowlist, so that guard
	// (#4015) reads the run too: the third read is the handler's binding.
	repo.from = 3
	code, resp := f.post(t, f.planStage.ID, upkeepExampleBody(t))
	if code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %v", code, resp)
	}
	if msg := upkeepErrorMessage(resp); msg != "resolve the stage's upkeep_report declaration failed" {
		t.Errorf("message = %q, want the binding step's", msg)
	}
	if n := repo.getRunCalls(); n != 3 {
		t.Errorf("reporting-run GetRun calls = %d, want 3 (the middleware's, the comms guard's, then the failed binding read)", n)
	}
	if got := f.stageState(f.planStage.ID); got != run.StageStateRunning {
		t.Errorf("stage = %q, want running", got)
	}
	if n := f.artifacts(artifact.KindUpkeepReport); n != 0 {
		t.Errorf("artifacts = %d, want 0", n)
	}
}

// The handler's own read of the reporting run (after the binding resolved it):
// a vanished row refuses as stage_binding_undecidable; a transport error 500s.
func TestUpkeepReportIngest_ReportingRunReread(t *testing.T) {
	t.Run("vanished", func(t *testing.T) {
		f := newUpkeepIngestFixtureWith(t, upkeepScanSpec(t), "upkeep_scan", nil, func(rr *upkeepRunRepo) run.Repository {
			return &upkeepRereadFailRepo{upkeepRunRepo: rr, err: run.ErrNotFound}
		})
		f.s.cfg.RunRepo.(*upkeepRereadFailRepo).id = f.runRow.ID
		code, resp := f.post(t, f.planStage.ID, upkeepExampleBody(t))
		assertUpkeepRefused(t, f, f.planStage.ID, code, resp, "upkeep_report_stage_invalid", upkeepRefusalStageBindingUndecidable)
		if f.rr.listCalls() != 1 {
			t.Errorf("ListStagesForRun calls = %d, want 1 (the binding resolved; the RE-READ refused)", f.rr.listCalls())
		}
	})
	t.Run("transport error", func(t *testing.T) {
		f := newUpkeepIngestFixtureWith(t, upkeepScanSpec(t), "upkeep_scan", nil, func(rr *upkeepRunRepo) run.Repository {
			return &upkeepRereadFailRepo{upkeepRunRepo: rr, err: errors.New("connection reset")}
		})
		f.s.cfg.RunRepo.(*upkeepRereadFailRepo).id = f.runRow.ID
		if code, resp := f.post(t, f.planStage.ID, upkeepExampleBody(t)); code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500: %v", code, resp)
		}
		if got := f.stageState(f.planStage.ID); got != run.StageStateRunning {
			t.Errorf("stage = %q, want running", got)
		}
		if n := f.artifacts(artifact.KindUpkeepReport); n != 0 {
			t.Errorf("artifacts = %d, want 0", n)
		}
	})
}

// --- plan-path guard ---------------------------------------------------------

// TestUpkeepStageGuard_EveryKindClassified is approval condition 1's guard
// test: it iterates plan.AllArtifactKinds() against an upkeep-declaring stage
// and asserts each kind's outcome, t.Fatalf'ing on a kind with no row so a new
// sibling fails here until it is classified on the allowlist or refused.
func TestUpkeepStageGuard_EveryKindClassified(t *testing.T) {
	type outcome struct {
		body       func(*testing.T) []byte
		status     int
		code       string // error code on a refusal
		storedKind artifact.Kind
		stored     int
		state      run.StageState
	}
	rows := map[plan.ArtifactKind]outcome{
		// (g1) a plan from the upkeep stage: refused through the plan_invalid
		// tail, never stored as a plan.
		plan.ArtifactKindPlan: {validPlanBytes, http.StatusBadRequest, "plan_invalid", artifact.KindPlan, 0, run.StageStateFailed},
		// (g2) a grooming_report: refused, never ingested.
		plan.ArtifactKindGroomingReport: {validGroomingReportBytes, http.StatusBadRequest, "grooming_report_stage_invalid", artifact.KindGroomingReport, 0, run.StageStateFailed},
		// Allowlisted: parks, writes nothing approvable.
		plan.ArtifactKindClarificationRequest: {validClarificationBytes, http.StatusCreated, "", artifact.KindPlan, 0, run.StageStateAwaitingInput},
		// Allowlisted: the declared artifact.
		plan.ArtifactKindUpkeepReport: {upkeepExampleBody, http.StatusCreated, "", artifact.KindUpkeepReport, 1, run.StageStateAwaitingApproval},
		// (g5) a comms_report (#4015): refused through the plan_invalid tail
		// before the comms handler is reached, never stored.
		plan.ArtifactKindCommsReport: {commsExampleBody, http.StatusBadRequest, "plan_invalid", artifact.KindCommsReport, 0, run.StageStateFailed},
	}
	for _, kind := range plan.AllArtifactKinds() {
		row, ok := rows[kind]
		if !ok {
			t.Fatalf("ArtifactKind %q is unclassified for an upkeep-declaring stage — add it to upkeepStageAllowedKinds deliberately, or assert its refusal here (#3921 condition 1)", kind)
		}
		t.Run(string(kind), func(t *testing.T) {
			f := newUpkeepIngestFixture(t, nil)
			code, resp := f.post(t, f.planStage.ID, row.body(t))
			if code != row.status {
				t.Fatalf("status = %d, want %d: %v", code, row.status, resp)
			}
			if row.code != "" && upkeepErrorCode(resp) != row.code {
				t.Errorf("error code = %q, want %q", upkeepErrorCode(resp), row.code)
			}
			if n := f.artifacts(row.storedKind); n != row.stored {
				t.Errorf("%s artifacts = %d, want %d", row.storedKind, n, row.stored)
			}
			if got := f.stageState(f.planStage.ID); got != row.state {
				t.Errorf("stage = %q, want %q", got, row.state)
			}
		})
	}
}

// (g1 detail) the refusal names the declaration.
func TestUpkeepStageGuard_PlanRefusalNamesDeclaration(t *testing.T) {
	f := newUpkeepIngestFixture(t, nil)
	code, resp := f.post(t, f.planStage.ID, validPlanBytes(t))
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d: %v", code, resp)
	}
	if e, _ := upkeepErrorDetails(t, resp)["error"].(string); !strings.Contains(e, "produces: upkeep_report") {
		t.Errorf("details.error = %q, want it to name produces: upkeep_report", e)
	}
}

// An unmappable stage inside an upkeep workflow refuses too (fail closed).
func TestUpkeepStageGuard_UnmappableStageRefused(t *testing.T) {
	f := newUpkeepIngestFixture(t, nil)
	stray := &run.Stage{ID: uuid.New(), RunID: f.runRow.ID, Type: run.StageTypePlan, State: run.StageStateRunning}
	f.rr.getStages[stray.ID] = stray
	code, resp := f.post(t, stray.ID, validPlanBytes(t))
	if code != http.StatusBadRequest || upkeepErrorCode(resp) != "plan_invalid" {
		t.Fatalf("response = %d %v, want 400 plan_invalid", code, resp)
	}
	if n := f.artifacts(artifact.KindPlan); n != 0 {
		t.Errorf("plan artifacts = %d, want 0", n)
	}
}

// (g3) a plan from an ordinary workflow is unaffected, and the guard never
// lists stages for it.
func TestUpkeepStageGuard_OrdinaryWorkflowUnaffected(t *testing.T) {
	f := newUpkeepIngestFixtureWith(t, []byte(appliesToSpec("")), "guarded", nil, nil)
	f.rr.stagesByRunID = nil // any ListStagesForRun is loud
	code, resp := f.post(t, f.planStage.ID, validPlanBytes(t))
	if code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %v", code, resp)
	}
	if n := f.artifacts(artifact.KindPlan); n != 1 {
		t.Errorf("plan artifacts = %d, want 1", n)
	}
	if n := f.rr.listCalls(); n != 0 {
		t.Errorf("ListStagesForRun calls = %d, want 0 for a workflow declaring no upkeep_report", n)
	}
}

// (g4) the guard's GetRun does not answer: 500, nothing stored, stage running.
// As in (s12) the error lands on the read after the middleware's, and the
// message names the guard's binding step.
func TestUpkeepStageGuard_TransportError500(t *testing.T) {
	f, repo := newUpkeepLateGetRunFailFixture(t)
	code, resp := f.post(t, f.planStage.ID, validPlanBytes(t))
	if code != http.StatusInternalServerError || upkeepErrorCode(resp) != "internal_error" {
		t.Fatalf("response = %d %v, want 500 internal_error", code, resp)
	}
	if msg := upkeepErrorMessage(resp); msg != "resolve the stage's upkeep_report declaration failed" {
		t.Errorf("message = %q, want the guard's binding step's", msg)
	}
	if n := repo.getRunCalls(); n != 2 {
		t.Errorf("reporting-run GetRun calls = %d, want 2 (the middleware's, then the failed guard read)", n)
	}
	if n := f.artifacts(artifact.KindPlan); n != 0 {
		t.Errorf("plan artifacts = %d, want 0", n)
	}
	if got := f.stageState(f.planStage.ID); got != run.StageStateRunning {
		t.Errorf("stage = %q, want running", got)
	}
}

// --- plan-path guard: undetectable bodies (#3922, the carried #3921 concern) --

// upkeepGuardBody returns the shipped example with mutate applied, re-encoded.
func upkeepGuardBody(t *testing.T, mutate func(m map[string]any)) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(upkeepExampleBody(t), &m); err != nil {
		t.Fatal(err)
	}
	mutate(m)
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// upkeepGuardLongKind is a 100-byte unknown kind: the echo must be cut to
// upkeepGuardMaxKindBytes (approval condition 6).
var upkeepGuardLongKind = "upkeep_reprot_" + strings.Repeat("x", 86)

// upkeepGuardUndetectableRows are the three ways a body on an upkeep-declaring
// stage reads as kind plan. want is the row's detail; absent must NOT appear.
func upkeepGuardUndetectableRows(t *testing.T) []struct {
	name   string
	body   []byte
	want   []string
	absent []string
} {
	return []struct {
		name   string
		body   []byte
		want   []string
		absent []string
	}{
		{
			name: "truncated JSON",
			body: []byte(`{"kind":"upkeep_report","report_version":`),
			want: []string{"unexpected end of JSON input", "not a parseable upkeep_report"},
			// The pre-#3922 message claimed a malformed upkeep_report was a plan.
			absent: []string{"not a plan"},
		},
		{
			name: "kind-less body",
			body: upkeepGuardBody(t, func(m map[string]any) { delete(m, "kind") }),
			want: []string{`carries no top-level "kind"`},
		},
		{
			name: "unknown kind",
			body: upkeepGuardBody(t, func(m map[string]any) { m["kind"] = upkeepGuardLongKind }),
			want: []string{
				`its top-level kind "` + upkeepGuardLongKind[:upkeepGuardMaxKindBytes] + `...[truncated]" is not a recognized artifact kind`,
			},
			absent: []string{upkeepGuardLongKind},
		},
	}
}

// TestUpkeepStageGuard_UndetectableBodyKeepsParseError pins the refusal text
// for a body the discriminator reads as a plan: the parse error, the missing
// kind or the unknown kind is named, alongside the declaration and
// upkeep_report_v1. The code stays plan_invalid, nothing is stored and the
// stage fails category-B (no orchestrator, so no schema retry).
func TestUpkeepStageGuard_UndetectableBodyKeepsParseError(t *testing.T) {
	for _, row := range upkeepGuardUndetectableRows(t) {
		t.Run(row.name, func(t *testing.T) {
			f := newUpkeepIngestFixture(t, nil)
			code, resp := f.post(t, f.planStage.ID, row.body)
			if code != http.StatusBadRequest || upkeepErrorCode(resp) != "plan_invalid" {
				t.Fatalf("response = %d %v, want 400 plan_invalid", code, resp)
			}
			e, _ := upkeepErrorDetails(t, resp)["error"].(string)
			for _, want := range append([]string{"produces: upkeep_report", plan.UpkeepReportVersion}, row.want...) {
				if !strings.Contains(e, want) {
					t.Errorf("details.error = %q, want it to contain %q", e, want)
				}
			}
			for _, absent := range row.absent {
				if strings.Contains(e, absent) {
					t.Errorf("details.error = %q, must not contain %q", e, absent)
				}
			}
			if got := f.stageState(f.planStage.ID); got != run.StageStateFailed {
				t.Errorf("stage = %q, want failed", got)
			}
			for _, k := range []artifact.Kind{artifact.KindPlan, artifact.KindUpkeepReport} {
				if n := f.artifacts(k); n != 0 {
					t.Errorf("%s artifacts = %d, want 0", k, n)
				}
			}
		})
	}
}

// upkeepRetryRunRepo lets the schema retry re-open the stage: the shared
// promptRunRepo's RetryStage is a stub that errors.
type upkeepRetryRunRepo struct{ *upkeepRunRepo }

func (r *upkeepRetryRunRepo) RetryStage(_ context.Context, id uuid.UUID, to run.StageState) (*run.Stage, error) {
	st, ok := r.getStages[id]
	if !ok {
		return nil, run.ErrNotFound
	}
	st.State = to
	return st, nil
}

// TestUpkeepStageGuard_UndetectableBodySchemaRetryCarriesParseError: with an
// orchestrator and audit repo wired, the guard's plan_invalid refusal of a
// truncated upkeep_report schedules the bounded schema retry, and the
// plan_schema_retry row's validation_error — the next prompt's feedback —
// carries the parse error and names upkeep_report_v1.
func TestUpkeepStageGuard_UndetectableBodySchemaRetryCarriesParseError(t *testing.T) {
	f := newUpkeepIngestFixtureWith(t, upkeepScanSpec(t), "upkeep_scan", nil, func(rr *upkeepRunRepo) run.Repository {
		return &upkeepRetryRunRepo{upkeepRunRepo: rr}
	})
	f.s.cfg.Orchestrator = &orchestrator.Orchestrator{Runs: f.s.cfg.RunRepo}
	code, resp := f.post(t, f.planStage.ID, []byte(`{"kind":"upkeep_report","report_version":`))
	if code != http.StatusBadRequest || upkeepErrorCode(resp) != "plan_invalid" {
		t.Fatalf("response = %d %v, want 400 plan_invalid", code, resp)
	}
	if got := upkeepErrorDetails(t, resp)["retry_scheduled"]; got != true {
		t.Fatalf("details.retry_scheduled = %v, want true: %v", got, resp)
	}
	var rows []string
	f.au.mu.Lock()
	for _, e := range f.au.appended {
		if e.Category != "plan_schema_retry" {
			continue
		}
		var p struct {
			ValidationError string `json:"validation_error"`
		}
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Errorf("decode plan_schema_retry payload: %v", err)
		}
		rows = append(rows, p.ValidationError)
	}
	f.au.mu.Unlock()
	if len(rows) != 1 {
		t.Fatalf("plan_schema_retry rows = %d, want 1", len(rows))
	}
	for _, want := range []string{"unexpected end of JSON input", plan.UpkeepReportVersion, "produces: upkeep_report"} {
		if !strings.Contains(rows[0], want) {
			t.Errorf("validation_error = %q, want it to contain %q", rows[0], want)
		}
	}
	if n := f.artifacts(artifact.KindUpkeepReport); n != 0 {
		t.Errorf("upkeep_report artifacts = %d, want 0", n)
	}
}

// TestUpkeepGuardBodyDetail covers the pure helper's branches directly,
// including a multi-byte kind cut mid-rune (the echo stays valid UTF-8) and a
// kind exactly at the cap (not truncated).
func TestUpkeepGuardBodyDetail(t *testing.T) {
	atCap := strings.Repeat("k", upkeepGuardMaxKindBytes)
	runes := strings.Repeat("é", 40) // after the leading "x", the byte-64 cut splits the 32nd é
	cases := []struct {
		name string
		body string
		derr error
		want string
	}{
		{"parse error", `{`, &plan.ParseError{Msg: "boom"}, "the body is not a parseable upkeep_report (plan: parse: boom)"},
		{"no kind", `{"summary":"x"}`, nil, `the body carries no top-level "kind", so it was read as a plan`},
		{"empty kind", `{"kind":""}`, nil, `the body carries no top-level "kind", so it was read as a plan`},
		{"kind at cap", `{"kind":"` + atCap + `"}`, nil, `its top-level kind "` + atCap + `" is not a recognized artifact kind`},
		{"multi-byte kind", `{"kind":"x` + runes + `"}`, nil, `its top-level kind "x` + strings.Repeat("é", 31) + `...[truncated]" is not a recognized artifact kind`},
		{"recognized plan", `{"kind":"plan"}`, nil, upkeepGuardDisallowedDetail("plan")},
		{"recognized grooming_report", `{"kind":"grooming_report"}`, nil, upkeepGuardDisallowedDetail("grooming_report")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := upkeepGuardBodyDetail([]byte(tc.body), tc.derr); got != tc.want {
				t.Errorf("upkeepGuardBodyDetail = %q, want %q", got, tc.want)
			}
		})
	}
}

// upkeepGuardDisallowedDetail is the guard detail for a recognized kind the
// upkeep allowlist does not admit.
func upkeepGuardDisallowedDetail(kind string) string {
	return `its top-level kind "` + kind + `" is a recognized artifact kind but is not allowed on a stage declaring produces: upkeep_report`
}

// TestUpkeepStageGuard_RecognizedButDisallowedKind (#3924 carried item 9): a
// body whose top-level kind is an explicit "plan" — a kind
// plan.AllArtifactKinds recognizes — shipped to an upkeep-declaring stage is
// refused with a detail saying the kind is recognized but not allowed, never
// that it is not a recognized artifact kind.
//
// Counterfactual: dropping the recognized-status branch from
// upkeepGuardBodyDetail reverts the detail to `... "plan" is not a recognized
// artifact kind` — RED on both the want and the absent assertion.
func TestUpkeepStageGuard_RecognizedButDisallowedKind(t *testing.T) {
	var m map[string]any
	if err := json.Unmarshal(validPlanBytes(t), &m); err != nil {
		t.Fatal(err)
	}
	m["kind"] = "plan"
	body, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if k, derr := plan.DetectArtifactKind(body); derr != nil || k != plan.ArtifactKindPlan {
		t.Fatalf("fixture: DetectArtifactKind = %q, %v; want plan, nil", k, derr)
	}

	f := newUpkeepIngestFixture(t, nil)
	code, resp := f.post(t, f.planStage.ID, body)
	if code != http.StatusBadRequest || upkeepErrorCode(resp) != "plan_invalid" {
		t.Fatalf("response = %d %v, want 400 plan_invalid", code, resp)
	}
	e, _ := upkeepErrorDetails(t, resp)["error"].(string)
	for _, want := range []string{upkeepGuardDisallowedDetail("plan"), "produces: upkeep_report", plan.UpkeepReportVersion} {
		if !strings.Contains(e, want) {
			t.Errorf("details.error = %q, want it to contain %q", e, want)
		}
	}
	if strings.Contains(e, "not a recognized artifact kind") {
		t.Errorf("details.error = %q, must not call a recognized kind unrecognized", e)
	}
	if got := f.stageState(f.planStage.ID); got != run.StageStateFailed {
		t.Errorf("stage = %q, want failed", got)
	}
	if got := upkeepPersistedFailureCategory(f, f.planStage.ID); got != run.FailureB {
		t.Errorf("persisted failure category = %q, want %q", got, run.FailureB)
	}
	if n := f.artifacts(artifact.KindPlan); n != 0 {
		t.Errorf("plan artifacts = %d, want 0", n)
	}
}

// TestUpkeepReportIngest_ConcurrentSameBody (#3924 carried item 3) is the
// atomicity proof for upkeepIngestMu: eight signed POSTs of identical bytes,
// released from one barrier, must commit exactly ONE upkeep_report artifact
// and ONE upkeep_report_recorded row, with one 201 and the rest idempotent
// 200s. Like the grooming sibling it asserts COMMITTED STATE, because every
// racing request returns a success status whether or not the writes forked.
//
// The conventions loader is replaced by a non-counting one: the shared
// installConventions counter is unguarded, and the dedupe runs OUTSIDE the
// mutex, so it would race under -race and make a RED the fake's, not the
// control's.
//
// Counterfactual vehicle: deleting the upkeepIngestMu Lock/Unlock pair opens
// the GetByHash → Create window, so several requests see no artifact and each
// creates one. Natural scheduling made that RED intermittent (1 of 3 runs
// observed), so the artifact repo is wrapped in a rendezvous that holds the
// first GetByHash miss open until a second miss arrives — deterministic
// without the mutex, a single bounded wait with it.
func TestUpkeepReportIngest_ConcurrentSameBody(t *testing.T) {
	const n = 8
	f := newUpkeepIngestFixture(t, nil)
	f.s.cfg.ArtifactRepo = &upkeepRendezvousArtifactRepo{fakeArtifactRepo: f.ar, second: make(chan struct{})}
	prev := conventionsLoader
	conventionsLoader = func(context.Context, string) (workmgmt.Conventions, error) {
		return workmgmt.Default(), nil
	}
	t.Cleanup(func() { conventionsLoader = prev })

	body := upkeepExampleBody(t)
	codes := make([]int, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			codes[i] = shipPlanRequest(t, f.s, f.runRow.ID, f.planStage.ID, f.priv, body, "").Code
		}(i)
	}
	close(start)
	wg.Wait()

	created := 0
	for i, c := range codes {
		switch c {
		case http.StatusCreated:
			created++
		case http.StatusOK:
		default:
			t.Fatalf("request %d status = %d, want 201 or 200", i, c)
		}
	}
	if created != 1 {
		t.Errorf("201 responses = %d, want exactly 1 (the rest must dedup to 200)", created)
	}
	if got := f.artifacts(artifact.KindUpkeepReport); got != 1 {
		t.Errorf("upkeep_report artifacts = %d, want 1 — concurrent identical POSTs must not fork the artifact row", got)
	}
	if got := len(f.recorded(t)); got != 1 {
		t.Errorf("upkeep_report_recorded rows = %d, want 1 — the existence check and the append must be atomic", got)
	}
}

// upkeepRendezvousWindow bounds how long the first GetByHash miss waits for a
// second. It is the green path's whole added cost (paid once) and only the
// counterfactual's odds depend on it, so it is not timescale-scaled.
const upkeepRendezvousWindow = 500 * time.Millisecond

// upkeepRendezvousArtifactRepo makes the ingest's GetByHash → Create window
// deterministic: the FIRST GetByHash that misses waits until a SECOND miss
// arrives or upkeepRendezvousWindow elapses. Under upkeepIngestMu no second
// request can reach GetByHash while the first holds the lock, so the wait
// times out once; with the mutex deleted a racing request misses too and
// releases both, so both Create.
type upkeepRendezvousArtifactRepo struct {
	*fakeArtifactRepo

	once   sync.Once
	second chan struct{}
	rvMu   sync.Mutex
	misses int
}

func (r *upkeepRendezvousArtifactRepo) GetByHash(ctx context.Context, stageID uuid.UUID, contentHash string) (*artifact.Artifact, error) {
	a, err := r.fakeArtifactRepo.GetByHash(ctx, stageID, contentHash)
	if !errors.Is(err, artifact.ErrNotFound) {
		return a, err
	}
	r.rvMu.Lock()
	r.misses++
	first := r.misses == 1
	r.rvMu.Unlock()
	if !first {
		r.once.Do(func() { close(r.second) })
		return a, err
	}
	select {
	case <-r.second:
	case <-time.After(upkeepRendezvousWindow):
	}
	return a, err
}

// TestUpkeepReportIngest_RecordsAdvisoryCoverage (#3750): the shipped advisory
// example through the real signed handler, with the listing seam serving one
// dependabot[bot] pull request bumping golang.org/x/net past the fix in
// /backend. The PERSISTED upkeep_report_recorded row (read after the call)
// marks exactly GO-2024-2687 covered by that pull request, counts three
// advisory findings, and copies the report's source_degrades verbatim. The
// seam serves a matching pull request, so only the payload wiring decides
// whether the mark lands: record a constant [] instead and this goes red.
func TestUpkeepReportIngest_RecordsAdvisoryCoverage(t *testing.T) {
	var calls atomic.Int32
	swapUpkeepListOpenPulls(t, ukPullsSeam(&calls, ukDependabotPull(3823, "golang.org/x/net", "0.22.0", "0.23.0", "/backend")))
	f := newUpkeepIngestFixture(t, nil)
	if code, resp := f.post(t, f.planStage.ID, upkeepAdvisoryExampleBody(t)); code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %v", code, resp)
	}
	rows := f.recorded(t)
	if len(rows) != 1 {
		t.Fatalf("upkeep_report_recorded rows = %d, want 1", len(rows))
	}
	row := rows[0]
	var covered []upkeep.Covered
	if err := json.Unmarshal(row["covered"], &covered); err != nil {
		t.Fatalf("decode covered %s: %v", row["covered"], err)
	}
	if len(covered) != 1 || covered[0].FindingID != ukAdvNet || covered[0].Package != "golang.org/x/net" ||
		len(covered[0].Pulls) != 1 || covered[0].Pulls[0].Number != 3823 || covered[0].Pulls[0].Directory != "backend" {
		t.Errorf("covered = %+v, want exactly %s by #3823 in backend", covered, ukAdvNet)
	}
	var counts map[string]int
	if err := json.Unmarshal(row["entry_counts"], &counts); err != nil {
		t.Fatal(err)
	}
	if counts["advisory"] != 3 || counts["findings"] != 3 {
		t.Errorf("entry_counts = %v, want advisory 3, findings 3", counts)
	}
	var degrades []plan.UpkeepSourceDegrade
	if err := json.Unmarshal(row["source_degrades"], &degrades); err != nil {
		t.Fatal(err)
	}
	if len(degrades) != 1 || degrades[0].Source != plan.UpkeepSourceDeprecation ||
		degrades[0].Reason != plan.UpkeepDegradeNetworkUnavailable || degrades[0].Detail == "" {
		t.Errorf("source_degrades = %+v, want the report's one {deprecation, network_unavailable} entry", degrades)
	}
	if string(row["coverage_degraded"]) != "false" || string(row["coverage_scanned_pulls"]) != "1" {
		t.Errorf("coverage_degraded = %s, coverage_scanned_pulls = %s, want false, 1",
			row["coverage_degraded"], row["coverage_scanned_pulls"])
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("listing calls = %d, want 1", n)
	}
}

// TestUpkeepReportIngest_CoverageDegradedStillIngests: a failed listing is
// recorded as a named coverage degrade with an empty covered array, and the
// report is still ingested and settled.
func TestUpkeepReportIngest_CoverageDegradedStillIngests(t *testing.T) {
	swapUpkeepListOpenPulls(t, func(context.Context, *Server, *run.Run, forge.RepoRef, int) ([]githubclient.OpenPullRequest, bool, error) {
		return nil, false, errors.New("github said no")
	})
	f := newUpkeepIngestFixture(t, nil)
	if code, resp := f.post(t, f.planStage.ID, upkeepAdvisoryExampleBody(t)); code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %v", code, resp)
	}
	rows := f.recorded(t)
	if len(rows) != 1 {
		t.Fatalf("recorded rows = %d, want 1", len(rows))
	}
	if string(rows[0]["coverage_degraded"]) != "true" || string(rows[0]["covered"]) != "[]" {
		t.Errorf("coverage_degraded = %s, covered = %s, want true, []", rows[0]["coverage_degraded"], rows[0]["covered"])
	}
	var reason string
	_ = json.Unmarshal(rows[0]["coverage_degrade_reason"], &reason)
	if reason != upkeepCoveragePullListFailed {
		t.Errorf("coverage_degrade_reason = %q, want %q", reason, upkeepCoveragePullListFailed)
	}
	if got := f.stageState(f.planStage.ID); got != run.StageStateAwaitingApproval {
		t.Errorf("stage = %q, want awaiting_approval", got)
	}
}
