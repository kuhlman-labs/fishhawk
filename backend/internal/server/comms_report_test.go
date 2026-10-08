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
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/userreport"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// comms_report ingest tests (#4015, slice 2). Every case drives a SIGNED POST
// through the real router (shipPlanRequest): request → DetectArtifactKind →
// the upkeep and comms guards → handleCommsReport → binding from the run's
// cached user-report-scan spec → ParseCommsReport (embedded schema +
// semantics) → latestCommsScanGathered over seeded audit rows → the charter
// checks → commsPreviewDrafts through a registered fake provider → artifact +
// comms_report_recorded → stage state, all read back from the fakes.

// commsExampleBody is the shipped canonical example: one draft over
// UR-issue-12 + UR-issue-40 citing U1, one n_drift (N2, UR-issue-41), one
// not_drafted (UR-comment-12-7).
func commsExampleBody(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("../../../docs/spec/examples/comms-report-v1-example.json")
	if err != nil {
		t.Fatalf("read comms example: %v", err)
	}
	return b
}

// commsExampleMutated decodes the example, applies mutate and re-encodes.
func commsExampleMutated(t *testing.T, mutate func(m map[string]any)) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(commsExampleBody(t), &m); err != nil {
		t.Fatal(err)
	}
	mutate(m)
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func commsTestDraft(m map[string]any) map[string]any {
	return m["drafts"].([]any)[0].(map[string]any)
}

// userReportScanSpec is the SHIPPED user-report-scan declaration: one plan
// stage `scan` declaring produces: comms_report.
func userReportScanSpec(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("../../../docs/spec/examples/workflow-v2-user-report-scan.yaml")
	if err != nil {
		t.Fatalf("read user-report-scan example: %v", err)
	}
	return b
}

// commsExampleGather is a gather whose shown set and charter ids match the
// example, plus UR-issue-99, which the example cites nowhere.
func commsExampleGather() commsScanGatheredPayload {
	at := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)
	shown := func(id, kind string, issue int, comment int64) commsShownReport {
		return commsShownReport{ID: id, Kind: kind, IssueNumber: issue, CommentID: comment,
			ContentHash: strings.Repeat("c", 64), UpdatedAt: at}
	}
	return commsScanGatheredPayload{
		StageAttempt: "1", Repo: upkeepTestRepo,
		Shown: []commsShownReport{
			shown("UR-issue-12", "issue", 12, 0),
			shown("UR-issue-40", "issue", 40, 0),
			shown("UR-issue-41", "issue", 41, 0),
			shown("UR-comment-12-7", "comment", 12, 7),
			shown("UR-issue-99", "issue", 99, 0),
		},
		Charter: commsCharterRecord{Path: "docs/CHARTER.md", ContentHash: strings.Repeat("d", 64),
			RubricIDs: []string{"U1", "U2"}, NonGoalIDs: []string{"N1", "N2"}},
	}
}

type commsIngestFixture struct {
	s         *Server
	ar        *fakeArtifactRepo
	au        *auditFake
	rr        *upkeepRunRepo
	cursors   *csCursors
	runRow    *run.Run
	planStage *run.Stage
	implStage *run.Stage
	priv      ed25519.PrivateKey
}

// newCommsIngestFixture seeds a user-report-scan run (cached wfSpec +
// workflowID) with a plan stage (sequence 0, the only stage the spec maps)
// and an implement stage row outside the stage list, wires the default
// preview conventions and a fake provider, and installs a recording cursor
// store the ingest must never touch.
func newCommsIngestFixture(t *testing.T, wfSpec []byte, workflowID string) *commsIngestFixture {
	t.Helper()
	return newCommsIngestFixtureWith(t, wfSpec, workflowID, nil)
}

// newCommsIngestFixtureWith is newCommsIngestFixture with the server's run
// repository optionally wrapped (wrap nil = the plain fake).
func newCommsIngestFixtureWith(t *testing.T, wfSpec []byte, workflowID string, wrap func(*upkeepRunRepo) run.Repository) *commsIngestFixture {
	t.Helper()
	registerFakeProvider(t, &fakeWorkProvider{})
	installConventions(t, commsPreviewConventions(), nil)
	rr := newUpkeepRunRepo()
	inst := int64(4242)
	runRow := &run.Run{ID: uuid.New(), Repo: upkeepTestRepo, InstallationID: &inst,
		WorkflowID: workflowID, WorkflowSpec: wfSpec, State: run.StateRunning}
	rr.seedRun(runRow)
	planStage := &run.Stage{ID: uuid.New(), RunID: runRow.ID, Sequence: 0, Type: run.StageTypePlan,
		State: run.StageStateRunning, RequiresApproval: true}
	implStage := &run.Stage{ID: uuid.New(), RunID: runRow.ID, Sequence: 1, Type: run.StageTypeImplement,
		State: run.StageStateRunning}
	rr.getStages[planStage.ID] = planStage
	rr.getStages[implStage.ID] = implStage
	rr.stagesByRunID = map[uuid.UUID][]*run.Stage{runRow.ID: {planStage}}
	f := &commsIngestFixture{
		ar: newFakeArtifactRepo(), au: newAuditFake(), rr: rr,
		cursors: &csCursors{vals: map[string]time.Time{}},
		runRow:  runRow, planStage: planStage, implStage: implStage,
	}
	sf := newSigningFake()
	var runRepo run.Repository = rr
	if wrap != nil {
		runRepo = wrap(rr)
	}
	f.s = New(Config{
		Addr: "127.0.0.1:0", SigningRepo: sf, ArtifactRepo: f.ar, AuditRepo: f.au, RunRepo: runRepo,
		UserReportCursors: f.cursors,
		Logger:            slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
	})
	f.priv, _ = sf.issue(t, runRow.ID)
	return f
}

func newCommsScanFixture(t *testing.T) *commsIngestFixture {
	t.Helper()
	return newCommsIngestFixture(t, userReportScanSpec(t), "user_report_scan")
}

// seedGather records p as a comms_scan_gathered row for stageID at sequence
// seq (stage column, payload stage_id and digest stamped as the gather does),
// timestamped an hour before now so it precedes any artifact the test then
// creates, and returns its digest.
func (f *commsIngestFixture) seedGather(t *testing.T, stageID uuid.UUID, seq int64, p commsScanGatheredPayload) string {
	t.Helper()
	return f.seedGatherAt(t, stageID, seq, time.Now().UTC().Add(-time.Hour), p)
}

// seedGatherAt is seedGather with the row's timestamp at.
func (f *commsIngestFixture) seedGatherAt(t *testing.T, stageID uuid.UUID, seq int64, at time.Time, p commsScanGatheredPayload) string {
	t.Helper()
	p = p.normalized()
	p.StageID = stageID
	p.GatherDigest = commsGatherDigest(p)
	e := commsSeed(t, CategoryCommsScanGathered, f.runRow.ID, nil, p)
	e.StageID, e.Sequence, e.Timestamp = &stageID, seq, at
	f.au.mu.Lock()
	f.au.seeded = append(f.au.seeded, e)
	f.au.mu.Unlock()
	return p.GatherDigest
}

func (f *commsIngestFixture) post(t *testing.T, stageID uuid.UUID, body []byte) (int, map[string]any) {
	t.Helper()
	w := shipPlanRequest(t, f.s, f.runRow.ID, stageID, f.priv, body, "")
	var m map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &m)
	return w.Code, m
}

// recorded returns every comms_report_recorded payload, decoded raw.
func (f *commsIngestFixture) recorded(t *testing.T) []map[string]json.RawMessage {
	t.Helper()
	f.au.mu.Lock()
	defer f.au.mu.Unlock()
	var out []map[string]json.RawMessage
	for _, e := range f.au.appended {
		if e.Category != CategoryCommsReportRecorded {
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

func (f *commsIngestFixture) artifacts(kind artifact.Kind) int {
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

func (f *commsIngestFixture) stageState(id uuid.UUID) run.StageState {
	return f.rr.getStages[id].State
}

func commsJSONString(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("decode %s as string: %v", raw, err)
	}
	return s
}

// assertCommsRefused asserts a 400 with code and reason (reason "" = the key
// is absent), the stage failed category-B, and nothing stored.
func assertCommsRefused(t *testing.T, f *commsIngestFixture, stageID uuid.UUID, code int, resp map[string]any, wantCode, wantReason string) map[string]any {
	t.Helper()
	if code != http.StatusBadRequest || upkeepErrorCode(resp) != wantCode {
		t.Fatalf("response = %d %v, want 400 %s", code, resp, wantCode)
	}
	d := upkeepErrorDetails(t, resp)
	if got, ok := d["reason"]; wantReason == "" && ok || wantReason != "" && got != wantReason {
		t.Errorf("details.reason = %v (present=%v), want %q", got, ok, wantReason)
	}
	if got := f.stageState(stageID); got != run.StageStateFailed {
		t.Errorf("stage = %q, want failed (category-B)", got)
	}
	var cat run.FailureCategory
	for _, c := range f.rr.transitionStageCalls {
		if c.StageID == stageID && c.To == run.StageStateFailed && c.Completion != nil && c.Completion.FailureCategory != nil {
			cat = *c.Completion.FailureCategory
		}
	}
	if cat != run.FailureB {
		t.Errorf("persisted failure category = %q, want %q", cat, run.FailureB)
	}
	if n := f.artifacts(artifact.KindCommsReport); n != 0 {
		t.Errorf("comms_report artifacts = %d, want 0", n)
	}
	if n := len(f.recorded(t)); n != 0 {
		t.Errorf("recorded rows = %d, want 0", n)
	}
	return d
}

func TestCommsReportIngest_HappyPath(t *testing.T) {
	f := newCommsScanFixture(t)
	digest := f.seedGather(t, f.planStage.ID, 5, commsExampleGather())

	code, resp := f.post(t, f.planStage.ID, commsExampleBody(t))
	if code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %v", code, resp)
	}
	if resp["schema_version"] != plan.CommsReportVersion || resp["idempotent"] != false {
		t.Errorf("response = %v, want schema_version %s, idempotent false", resp, plan.CommsReportVersion)
	}
	if n := f.artifacts(artifact.KindCommsReport); n != 1 {
		t.Fatalf("comms_report artifacts = %d, want 1", n)
	}
	if got := *f.ar.all[0].SchemaVersion; got != plan.CommsReportVersion {
		t.Errorf("artifact schema_version = %q", got)
	}
	if got := f.stageState(f.planStage.ID); got != run.StageStateAwaitingApproval {
		t.Errorf("stage = %q, want awaiting_approval", got)
	}
	rows := f.recorded(t)
	if len(rows) != 1 {
		t.Fatalf("comms_report_recorded rows = %d, want 1", len(rows))
	}
	row := rows[0]
	if got := commsJSONString(t, row["gather_digest"]); got != digest {
		t.Errorf("gather_digest = %q, want the seeded gather's %q", got, digest)
	}
	if string(row["gather_sequence"]) != "5" {
		t.Errorf("gather_sequence = %s, want 5", row["gather_sequence"])
	}
	if got := commsJSONString(t, row["charter_content_hash"]); got != strings.Repeat("d", 64) {
		t.Errorf("charter_content_hash = %q", got)
	}
	if string(row["unaccounted_report_ids"]) != `["UR-issue-99"]` {
		t.Errorf("unaccounted_report_ids = %s, want [\"UR-issue-99\"]", row["unaccounted_report_ids"])
	}
	if string(row["entry_counts"]) != `{"drafts":1,"n_drift":1,"not_drafted":1}` {
		t.Errorf("entry_counts = %s", row["entry_counts"])
	}
	if string(row["preview_degraded"]) != "false" {
		t.Errorf("preview_degraded = %s, want false", row["preview_degraded"])
	}
	if _, ok := row["preview_degrade_reason"]; ok {
		t.Error("an undegraded row carries preview_degrade_reason")
	}
	for _, k := range []string{"run_id", "stage_id", "artifact_id", "content_hash", "schema_version", "size_bytes", "charter_text"} {
		if _, ok := row[k]; !ok {
			t.Errorf("recorded row lacks %q", k)
		}
	}
	var previews []commsDraftPreview
	if err := json.Unmarshal(row["previews"], &previews); err != nil {
		t.Fatalf("decode previews: %v", err)
	}
	if len(previews) != 1 || previews[0].DraftID != "draft:UR-issue-12+UR-issue-40" || previews[0].CommsRenderedPreview == nil {
		t.Fatalf("previews = %+v, want one rendered preview of the example draft", previews)
	}
	if !strings.Contains(previews[0].Body, "<!-- fishhawk-comms:v1") || strings.Join(previews[0].SourceRefs, ",") != "#12,#40" {
		t.Errorf("preview = %+v, want the server-rendered marker and #12,#40 source refs", previews[0])
	}
}

// The ingest NEVER advances the user-report cursor: a regression guard, not a
// counterfactual (the ingest carries no cursor code to delete).
func TestCommsReportIngest_NeverTouchesCursors(t *testing.T) {
	f := newCommsScanFixture(t)
	f.seedGather(t, f.planStage.ID, 1, commsExampleGather())
	if code, resp := f.post(t, f.planStage.ID, commsExampleBody(t)); code != http.StatusCreated {
		t.Fatalf("status = %d: %v", code, resp)
	}
	f.cursors.mu.Lock()
	defer f.cursors.mu.Unlock()
	if f.cursors.advances != 0 || f.cursors.inits != 0 || len(f.cursors.vals) != 0 {
		t.Errorf("cursor store touched: advances=%d inits=%d vals=%v", f.cursors.advances, f.cursors.inits, f.cursors.vals)
	}
}

func TestCommsReportIngest_StageRefusals(t *testing.T) {
	t.Run("stage_type_not_plan", func(t *testing.T) {
		f := newCommsScanFixture(t)
		f.seedGather(t, f.implStage.ID, 1, commsExampleGather())
		code, resp := f.post(t, f.implStage.ID, commsExampleBody(t))
		assertCommsRefused(t, f, f.implStage.ID, code, resp, "comms_report_stage_invalid", commsRefusalStageTypeNotPlan)
	})
	t.Run("stage_does_not_declare_comms_report", func(t *testing.T) {
		f := newCommsIngestFixture(t, []byte(appliesToSpec("")), "guarded")
		f.seedGather(t, f.planStage.ID, 1, commsExampleGather())
		code, resp := f.post(t, f.planStage.ID, commsExampleBody(t))
		assertCommsRefused(t, f, f.planStage.ID, code, resp, "comms_report_stage_invalid", commsRefusalStageDoesNotDeclare)
	})
	t.Run("stage_binding_undecidable unmappable", func(t *testing.T) {
		f := newCommsScanFixture(t)
		stray := &run.Stage{ID: uuid.New(), RunID: f.runRow.ID, Type: run.StageTypePlan, State: run.StageStateRunning}
		f.rr.getStages[stray.ID] = stray
		f.seedGather(t, stray.ID, 1, commsExampleGather())
		code, resp := f.post(t, stray.ID, commsExampleBody(t))
		assertCommsRefused(t, f, stray.ID, code, resp, "comms_report_stage_invalid", commsRefusalStageBindingUndecidable)
	})
	t.Run("stage_binding_undecidable no cached spec", func(t *testing.T) {
		f := newCommsIngestFixture(t, nil, "user_report_scan")
		f.seedGather(t, f.planStage.ID, 1, commsExampleGather())
		code, resp := f.post(t, f.planStage.ID, commsExampleBody(t))
		assertCommsRefused(t, f, f.planStage.ID, code, resp, "comms_report_stage_invalid", commsRefusalStageBindingUndecidable)
	})
}

// scan_context_absent: the run holds a gather row for ANOTHER stage, so only
// the stage filter and the sentinel mapping refuse; without the mapping the
// error would be a 500.
func TestCommsReportIngest_ScanContextAbsent(t *testing.T) {
	f := newCommsScanFixture(t)
	f.seedGather(t, f.implStage.ID, 1, commsExampleGather())
	code, resp := f.post(t, f.planStage.ID, commsExampleBody(t))
	assertCommsRefused(t, f, f.planStage.ID, code, resp, "comms_report_stage_invalid", commsRefusalScanContextAbsent)
}

// A gather LIST failure is a 500 with the stage left running.
func TestCommsReportIngest_GatherListFailure500(t *testing.T) {
	f := newCommsScanFixture(t)
	f.au.listByCategoryErrCategory = CategoryCommsScanGathered
	code, resp := f.post(t, f.planStage.ID, commsExampleBody(t))
	if code != http.StatusInternalServerError || upkeepErrorCode(resp) != "internal_error" {
		t.Fatalf("response = %d %v, want 500", code, resp)
	}
	if got := f.stageState(f.planStage.ID); got != run.StageStateRunning {
		t.Errorf("stage = %q, want running", got)
	}
	if n := f.artifacts(artifact.KindCommsReport); n != 0 {
		t.Errorf("artifacts = %d, want 0", n)
	}
}

func TestCommsReportIngest_InvalidReportRefused(t *testing.T) {
	cases := map[string]func(m map[string]any){
		"schema extra property": func(m map[string]any) { m["extra"] = true },
		"semantic autonomy label": func(m map[string]any) {
			commsTestDraft(m)["proposed_issue"].(map[string]any)["labels"] = []any{"autonomy:high"}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := newCommsScanFixture(t)
			f.seedGather(t, f.planStage.ID, 1, commsExampleGather())
			code, resp := f.post(t, f.planStage.ID, commsExampleMutated(t, mutate))
			assertCommsRefused(t, f, f.planStage.ID, code, resp, "comms_report_invalid", "")
		})
	}
}

// Each charter-anchored check: the fixture cites a shape-valid id ABSENT from
// the gathered row, so the check is the only thing refusing it.
func TestCommsReportIngest_CharterRefRefusals(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(m map[string]any)
		reason  string
		details map[string]string
	}{
		{"report_ref_invalid", func(m map[string]any) {
			m["not_drafted"].([]any)[0].(map[string]any)["report_id"] = "UR-issue-999"
		}, commsRefReportRefInvalid, map[string]string{"report_id": "UR-issue-999"}},
		{"rubric_id_unknown", func(m map[string]any) {
			commsTestDraft(m)["rubric_citations"] = []any{map[string]any{"rubric_id": "Z9"}}
		}, commsRefRubricIDUnknown, map[string]string{"rubric_id": "Z9", "draft_id": "draft:UR-issue-12+UR-issue-40"}},
		{"non_goal_id_unknown", func(m map[string]any) {
			nd := m["n_drift"].([]any)[0].(map[string]any)
			nd["non_goal_id"], nd["id"] = "N99", "ndrift:N99:UR-issue-41"
		}, commsRefNonGoalIDUnknown, map[string]string{"non_goal_id": "N99"}},
		// The draft cites UR-comment-12-7 (on issue 12) but NOT UR-issue-12, so
		// only the comment report's issue-number derivation can refuse #12.
		{"parent_epic_is_source via a comment report", func(m map[string]any) {
			d := commsTestDraft(m)
			d["source_report_ids"] = []any{"UR-comment-12-7", "UR-issue-40"}
			d["id"] = "draft:UR-comment-12-7+UR-issue-40"
			d["proposed_issue"].(map[string]any)["parent_epic"] = "#12"
			m["not_drafted"] = []any{map[string]any{"report_id": "UR-issue-12", "reason": "question"}}
		}, commsRefParentEpicIsSource, map[string]string{"draft_id": "draft:UR-comment-12-7+UR-issue-40", "parent_epic": "#12"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := commsExampleMutated(t, tc.mutate)
			if _, err := plan.ParseCommsReport(body); err != nil {
				t.Fatalf("fixture must be schema- and semantically valid: %v", err)
			}
			f := newCommsScanFixture(t)
			f.seedGather(t, f.planStage.ID, 1, commsExampleGather())
			code, resp := f.post(t, f.planStage.ID, body)
			d := assertCommsRefused(t, f, f.planStage.ID, code, resp, "comms_report_invalid", tc.reason)
			for k, v := range tc.details {
				if d[k] != v {
					t.Errorf("details.%s = %v, want %q", k, d[k], v)
				}
			}
		})
	}
}

// The shown set is authoritative: an id in shown AND omitted is citable.
func TestCommsReportIngest_ShownAndOmittedIDIsCitable(t *testing.T) {
	f := newCommsScanFixture(t)
	g := commsExampleGather()
	g.Omitted, g.OmittedCount = []string{"UR-issue-41"}, 1
	f.seedGather(t, f.planStage.ID, 1, g)
	if code, resp := f.post(t, f.planStage.ID, commsExampleBody(t)); code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %v", code, resp)
	}
}

func TestCheckCommsCharterRefs_OmittedOnlyIDRefused(t *testing.T) {
	r, err := plan.ParseCommsReport(commsExampleBody(t))
	if err != nil {
		t.Fatal(err)
	}
	g := commsExampleGather()
	g.Shown = g.Shown[1:] // UR-issue-12 no longer shown...
	g.Omitted = []string{"UR-issue-12"}
	if ref := checkCommsCharterRefs(r, &g); ref == nil || ref.Reason != commsRefReportRefInvalid || ref.Details["report_id"] != "UR-issue-12" {
		t.Errorf("refusal = %+v, want report_ref_invalid for an omitted-only id", ref)
	}
}

// Approval condition 2: an identical retry takes the idempotent path BEFORE
// any gather is read, so a later gather that omits a cited report cannot fail
// the already-committed stage.
func TestCommsReportIngest_IdempotentRetryIgnoresNewerGather(t *testing.T) {
	f := newCommsScanFixture(t)
	digestA := f.seedGather(t, f.planStage.ID, 5, commsExampleGather())
	if code, resp := f.post(t, f.planStage.ID, commsExampleBody(t)); code != http.StatusCreated {
		t.Fatalf("first ingest = %d: %v", code, resp)
	}
	gB := commsExampleGather()
	gB.Shown = gB.Shown[:2] // omits UR-issue-41 and UR-comment-12-7, which the report cites
	gB.StageAttempt = "2"
	f.seedGather(t, f.planStage.ID, 9, gB)

	code, resp := f.post(t, f.planStage.ID, commsExampleBody(t))
	if code != http.StatusOK || resp["idempotent"] != true {
		t.Fatalf("retry = %d %v, want 200 idempotent", code, resp)
	}
	if got := f.stageState(f.planStage.ID); got != run.StageStateAwaitingApproval {
		t.Errorf("stage = %q, want awaiting_approval (NOT failed)", got)
	}
	rows := f.recorded(t)
	if len(rows) != 1 || commsJSONString(t, rows[0]["gather_digest"]) != digestA {
		t.Errorf("recorded rows = %d, want the one row naming gather A", len(rows))
	}
	if n := f.artifacts(artifact.KindCommsReport); n != 1 {
		t.Errorf("artifacts = %d, want 1", n)
	}
}

// commsFailFirstIngestAppend ingests the example with the comms_report_recorded
// append failing after the artifact Create (500, artifact stored, no row,
// stage running), and returns the stored artifact's creation time.
func commsFailFirstIngestAppend(t *testing.T, f *commsIngestFixture) time.Time {
	t.Helper()
	f.au.appendErrCategory = CategoryCommsReportRecorded
	if code, resp := f.post(t, f.planStage.ID, commsExampleBody(t)); code != http.StatusInternalServerError {
		t.Fatalf("first ingest = %d %v, want 500 (append failed)", code, resp)
	}
	f.au.appendErrCategory = ""
	if n := f.artifacts(artifact.KindCommsReport); n != 1 || len(f.recorded(t)) != 0 {
		t.Fatalf("artifacts = %d rows = %d, want 1 and 0 after the failed append", n, len(f.recorded(t)))
	}
	if got := f.stageState(f.planStage.ID); got != run.StageStateRunning {
		t.Fatalf("stage = %q, want running after a 500", got)
	}
	f.ar.mu.Lock()
	defer f.ar.mu.Unlock()
	return f.ar.all[0].CreatedAt
}

// A retry after a lost append HEALS the missing row bound to the gather the
// FIRST ingest validated against (A): the newest gather recorded at or before
// the artifact's creation. A gather B recorded AFTER the creation is never the
// binding — neither when it omits a cited report nor when it keeps every
// cited id but carries different content hashes, where B validates and only
// the creation-time ordering keeps the heal (and the preview's filing marker)
// on A's hashes.
func TestCommsReportIngest_HealBindsToOriginalGather(t *testing.T) {
	cases := []struct {
		name   string
		gather func() commsScanGatheredPayload
	}{
		{name: "B omits cited reports", gather: func() commsScanGatheredPayload {
			g := commsExampleGather()
			g.Shown = g.Shown[:2]
			return g
		}},
		{name: "B keeps the ids with different hashes", gather: func() commsScanGatheredPayload {
			g := commsExampleGather()
			for i := range g.Shown {
				g.Shown[i].ContentHash = strings.Repeat("e", 64)
			}
			return g
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newCommsScanFixture(t)
			digestA := f.seedGather(t, f.planStage.ID, 5, commsExampleGather())
			created := commsFailFirstIngestAppend(t, f)
			gB := tc.gather()
			gB.StageAttempt = "2"
			// Anchored to the artifact's own creation stamp: recorded after it.
			digestB := f.seedGatherAt(t, f.planStage.ID, 9, created.Add(time.Minute), gB)
			if digestB == digestA {
				t.Fatal("fixture: the two gathers must differ")
			}

			code, resp := f.post(t, f.planStage.ID, commsExampleBody(t))
			if code != http.StatusOK || resp["idempotent"] != true {
				t.Fatalf("retry = %d %v, want 200 idempotent", code, resp)
			}
			rows := f.recorded(t)
			if len(rows) != 1 {
				t.Fatalf("recorded rows = %d, want 1 healed row", len(rows))
			}
			if got := commsJSONString(t, rows[0]["gather_digest"]); got != digestA {
				t.Errorf("healed gather_digest = %q, want gather A's %q (B is %q)", got, digestA, digestB)
			}
			if string(rows[0]["gather_sequence"]) != "5" {
				t.Errorf("healed gather_sequence = %s, want 5", rows[0]["gather_sequence"])
			}
			var previews []commsDraftPreview
			if err := json.Unmarshal(rows[0]["previews"], &previews); err != nil {
				t.Fatalf("decode previews: %v", err)
			}
			if len(previews) != 1 || previews[0].CommsRenderedPreview == nil {
				t.Fatalf("previews = %+v, want one rendered preview", previews)
			}
			got, malformed := userreport.ParseDraftMarkers(previews[0].Body)
			want := []userreport.MarkedReport{
				{ID: "UR-issue-12", ContentHash: strings.Repeat("c", 64)},
				{ID: "UR-issue-40", ContentHash: strings.Repeat("c", 64)},
			}
			if malformed != 0 || len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
				t.Errorf("preview filing marker = %+v (malformed %d), want gather A's hashes %+v", got, malformed, want)
			}
			if got := f.stageState(f.planStage.ID); got != run.StageStateAwaitingApproval {
				t.Errorf("stage = %q, want awaiting_approval", got)
			}
		})
	}
}

// A heal whose original gather (the newest recorded at or before the
// artifact's creation) no longer validates the stored report is a named 500
// — never a binding to it — with the stage left running.
func TestCommsReportIngest_HealWithoutValidatingGather500(t *testing.T) {
	f := newCommsScanFixture(t)
	f.seedGather(t, f.planStage.ID, 5, commsExampleGather())
	commsFailFirstIngestAppend(t, f)
	f.au.mu.Lock()
	f.au.seeded = nil // the only validating gather is gone
	f.au.mu.Unlock()
	gB := commsExampleGather()
	gB.Shown = gB.Shown[:2]
	f.seedGather(t, f.planStage.ID, 9, gB) // recorded before the artifact

	code, resp := f.post(t, f.planStage.ID, commsExampleBody(t))
	if code != http.StatusInternalServerError {
		t.Fatalf("retry = %d %v, want 500", code, resp)
	}
	if msg := upkeepErrorMessage(resp); msg != commsNoGatherValidates {
		t.Errorf("message = %q, want %q", msg, commsNoGatherValidates)
	}
	if got := upkeepErrorDetails(t, resp)["reason"]; got != commsHealReasonGatherInvalid {
		t.Errorf("details.reason = %v, want %q", got, commsHealReasonGatherInvalid)
	}
	if len(f.recorded(t)) != 0 {
		t.Error("a heal bound to a non-validating gather")
	}
	if got := f.stageState(f.planStage.ID); got != run.StageStateRunning {
		t.Errorf("stage = %q, want running", got)
	}
}

// A heal with NO gather recorded at or before the artifact's creation cannot
// determine the original binding: a named 500, even though a LATER gather
// validates the stored report, with the stage left running.
func TestCommsReportIngest_HealOriginalGatherUndecidable500(t *testing.T) {
	f := newCommsScanFixture(t)
	f.seedGather(t, f.planStage.ID, 5, commsExampleGather())
	created := commsFailFirstIngestAppend(t, f)
	f.au.mu.Lock()
	f.au.seeded = nil // the original gather is gone
	f.au.mu.Unlock()
	gB := commsExampleGather() // validates the stored report
	gB.StageAttempt = "2"
	f.seedGatherAt(t, f.planStage.ID, 9, created.Add(time.Minute), gB)

	code, resp := f.post(t, f.planStage.ID, commsExampleBody(t))
	if code != http.StatusInternalServerError {
		t.Fatalf("retry = %d %v, want 500", code, resp)
	}
	if msg := upkeepErrorMessage(resp); msg != commsOriginalGatherUndecidable {
		t.Errorf("message = %q, want %q", msg, commsOriginalGatherUndecidable)
	}
	if got := upkeepErrorDetails(t, resp)["reason"]; got != commsHealReasonGatherUndecidable {
		t.Errorf("details.reason = %v, want %q", got, commsHealReasonGatherUndecidable)
	}
	if len(f.recorded(t)) != 0 {
		t.Error("a heal bound to a gather recorded after the artifact")
	}
	if got := f.stageState(f.planStage.ID); got != run.StageStateRunning {
		t.Errorf("stage = %q, want running", got)
	}
}

// The recorded binding survives a second gather for the same stage: the row
// still names the first, which commsScanGatheredByDigest returns, while
// latestCommsScanGathered returns the second.
func TestCommsReportIngest_BindingSurvivesSecondGather(t *testing.T) {
	f := newCommsScanFixture(t)
	digestA := f.seedGather(t, f.planStage.ID, 5, commsExampleGather())
	if code, resp := f.post(t, f.planStage.ID, commsExampleBody(t)); code != http.StatusCreated {
		t.Fatalf("ingest = %d: %v", code, resp)
	}
	gB := commsExampleGather()
	gB.StageAttempt = "2"
	digestB := f.seedGather(t, f.planStage.ID, 9, gB)
	if digestA == digestB {
		t.Fatal("fixture: the two gathers must differ")
	}
	rows := f.recorded(t)
	if len(rows) != 1 {
		t.Fatalf("rows = %d", len(rows))
	}
	bound := commsJSONString(t, rows[0]["gather_digest"])
	if bound != digestA {
		t.Fatalf("recorded gather_digest = %q, want gather A's", bound)
	}
	ctx := context.Background()
	_, pa, err := f.s.commsScanGatheredByDigest(ctx, f.runRow.ID, bound)
	if err != nil || pa.GatherDigest != digestA {
		t.Errorf("by digest = %+v err=%v, want gather A", pa, err)
	}
	_, pl, err := f.s.latestCommsScanGathered(ctx, f.runRow.ID, f.planStage.ID)
	if err != nil || pl.GatherDigest != digestB {
		t.Errorf("latest = %+v err=%v, want gather B", pl, err)
	}
}

// A refused preview is recorded and the ingest still succeeds.
func TestCommsReportIngest_PreviewFailureStillRecords(t *testing.T) {
	f := newCommsScanFixture(t)
	f.seedGather(t, f.planStage.ID, 1, commsExampleGather())
	body := commsExampleMutated(t, func(m map[string]any) {
		commsTestDraft(m)["proposed_issue"].(map[string]any)["type"] = "no_such_type"
	})
	code, resp := f.post(t, f.planStage.ID, body)
	if code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %v", code, resp)
	}
	rows := f.recorded(t)
	if len(rows) != 1 {
		t.Fatalf("rows = %d", len(rows))
	}
	var previews []commsDraftPreview
	if err := json.Unmarshal(rows[0]["previews"], &previews); err != nil {
		t.Fatal(err)
	}
	if len(previews) != 1 || previews[0].Error == nil || previews[0].Error.Code == "" || previews[0].FilingBodyDigest == "" {
		t.Errorf("previews = %+v, want the refused preview recorded with its digest", previews)
	}
}

// A run-wide preview degrade (conventions unavailable) is recorded with its
// reason and the ingest still succeeds.
func TestCommsReportIngest_PreviewDegradeRecorded(t *testing.T) {
	f := newCommsScanFixture(t)
	installConventions(t, workmgmt.Conventions{}, errors.New("conventions read failed"))
	f.seedGather(t, f.planStage.ID, 1, commsExampleGather())
	if code, resp := f.post(t, f.planStage.ID, commsExampleBody(t)); code != http.StatusCreated {
		t.Fatalf("status = %d: %v", code, resp)
	}
	row := f.recorded(t)[0]
	if string(row["preview_degraded"]) != "true" || commsJSONString(t, row["preview_degrade_reason"]) != commsPreviewDegradeConventionsUnavailable {
		t.Errorf("degrade = %s / %s", row["preview_degraded"], row["preview_degrade_reason"])
	}
}

func TestCommsReportIngest_ConcurrentSameBody(t *testing.T) {
	f := newCommsScanFixture(t)
	f.seedGather(t, f.planStage.ID, 1, commsExampleGather())
	// installConventions' counting loader is not goroutine-safe; a plain one
	// keeps -race on the ingest, not the fixture.
	prev := conventionsLoader
	conventionsLoader = func(context.Context, string) (workmgmt.Conventions, error) {
		return commsPreviewConventions(), nil
	}
	t.Cleanup(func() { conventionsLoader = prev })
	body := commsExampleBody(t)
	var wg sync.WaitGroup
	codes := make([]int, 4)
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i], _ = f.post(t, f.planStage.ID, body)
		}(i)
	}
	wg.Wait()
	created := 0
	for _, c := range codes {
		switch c {
		case http.StatusCreated:
			created++
		case http.StatusOK:
		default:
			t.Errorf("status = %d, want 201 or 200", c)
		}
	}
	if created != 1 {
		t.Errorf("201s = %d, want exactly 1", created)
	}
	if n := f.artifacts(artifact.KindCommsReport); n != 1 {
		t.Errorf("artifacts = %d, want 1", n)
	}
	if n := len(f.recorded(t)); n != 1 {
		t.Errorf("recorded rows = %d, want 1", n)
	}
}

// --- plan-path guard ---------------------------------------------------------

// TestCommsStageGuard_EveryKindClassified iterates plan.AllArtifactKinds()
// against a comms-declaring stage, t.Fatalf'ing on a kind with no row so a new
// sibling fails here until it is classified.
func TestCommsStageGuard_EveryKindClassified(t *testing.T) {
	type outcome struct {
		body       func(*testing.T) []byte
		status     int
		code       string
		reason     string
		storedKind artifact.Kind
		stored     int
		state      run.StageState
	}
	rows := map[plan.ArtifactKind]outcome{
		plan.ArtifactKindPlan:                 {validPlanBytes, http.StatusBadRequest, "plan_invalid", "", artifact.KindPlan, 0, run.StageStateFailed},
		plan.ArtifactKindGroomingReport:       {validGroomingReportBytes, http.StatusBadRequest, "grooming_report_stage_invalid", commsGuardReasonStageDeclaresComms, artifact.KindGroomingReport, 0, run.StageStateFailed},
		plan.ArtifactKindUpkeepReport:         {upkeepExampleBody, http.StatusBadRequest, "plan_invalid", "", artifact.KindUpkeepReport, 0, run.StageStateFailed},
		plan.ArtifactKindClarificationRequest: {validClarificationBytes, http.StatusCreated, "", "", artifact.KindPlan, 0, run.StageStateAwaitingInput},
		plan.ArtifactKindCommsReport:          {commsExampleBody, http.StatusCreated, "", "", artifact.KindCommsReport, 1, run.StageStateAwaitingApproval},
	}
	for _, kind := range plan.AllArtifactKinds() {
		row, ok := rows[kind]
		if !ok {
			t.Fatalf("ArtifactKind %q is unclassified for a comms-declaring stage — add it to commsStageAllowedKinds deliberately, or assert its refusal here (#4015)", kind)
		}
		t.Run(string(kind), func(t *testing.T) {
			f := newCommsScanFixture(t)
			f.seedGather(t, f.planStage.ID, 1, commsExampleGather())
			code, resp := f.post(t, f.planStage.ID, row.body(t))
			if code != row.status {
				t.Fatalf("status = %d, want %d: %v", code, row.status, resp)
			}
			if row.code != "" && upkeepErrorCode(resp) != row.code {
				t.Errorf("error code = %q, want %q", upkeepErrorCode(resp), row.code)
			}
			if row.reason != "" && upkeepErrorDetails(t, resp)["reason"] != row.reason {
				t.Errorf("details.reason = %v, want %q", upkeepErrorDetails(t, resp)["reason"], row.reason)
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

// The plan refusal names the declaration and the comms schema version.
func TestCommsStageGuard_RefusesPlanOnCommsStage(t *testing.T) {
	f := newCommsScanFixture(t)
	code, resp := f.post(t, f.planStage.ID, validPlanBytes(t))
	if code != http.StatusBadRequest || upkeepErrorCode(resp) != "plan_invalid" {
		t.Fatalf("response = %d %v, want 400 plan_invalid", code, resp)
	}
	e, _ := upkeepErrorDetails(t, resp)["error"].(string)
	if !strings.Contains(e, "produces: comms_report") || !strings.Contains(e, plan.CommsReportVersion) {
		t.Errorf("details.error = %q, want it to name produces: comms_report and %s", e, plan.CommsReportVersion)
	}
	if n := f.artifacts(artifact.KindPlan); n != 0 {
		t.Errorf("plan artifacts = %d, want 0", n)
	}
}

// An unmappable stage inside a comms workflow refuses a plan too (fail closed).
func TestCommsStageGuard_UnmappableStageRefused(t *testing.T) {
	f := newCommsScanFixture(t)
	stray := &run.Stage{ID: uuid.New(), RunID: f.runRow.ID, Type: run.StageTypePlan, State: run.StageStateRunning}
	f.rr.getStages[stray.ID] = stray
	code, resp := f.post(t, stray.ID, validPlanBytes(t))
	if code != http.StatusBadRequest || upkeepErrorCode(resp) != "plan_invalid" {
		t.Fatalf("response = %d %v, want 400 plan_invalid", code, resp)
	}
	if e, _ := upkeepErrorDetails(t, resp)["error"].(string); !strings.Contains(e, "undecidable") {
		t.Errorf("details.error = %q, want it to name the undecidable declaration", e)
	}
}

// A plan from an ordinary workflow is unaffected by the comms guard.
func TestCommsStageGuard_OrdinaryWorkflowUnaffected(t *testing.T) {
	f := newCommsIngestFixture(t, []byte(appliesToSpec("")), "guarded")
	if code, resp := f.post(t, f.planStage.ID, validPlanBytes(t)); code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %v", code, resp)
	}
}

// A run read that does not answer inside the guard is a 500 naming the comms
// binding step, with nothing stored and the stage running.
func TestCommsStageGuard_TransportError500(t *testing.T) {
	f := newCommsScanFixture(t)
	f.rr.listStagesErr = errors.New("connection reset")
	code, resp := f.post(t, f.planStage.ID, validPlanBytes(t))
	if code != http.StatusInternalServerError || upkeepErrorMessage(resp) != commsStageBindingResolveFailed {
		t.Fatalf("response = %d %v, want 500 %q", code, resp, commsStageBindingResolveFailed)
	}
	if got := f.stageState(f.planStage.ID); got != run.StageStateRunning {
		t.Errorf("stage = %q, want running", got)
	}
}

// The handler's own binding read failing is a 500 too (the guard passes a
// comms_report through without reading the binding).
func TestCommsReportIngest_BindingTransportError500(t *testing.T) {
	f := newCommsScanFixture(t)
	f.rr.listStagesErr = errors.New("connection reset")
	code, resp := f.post(t, f.planStage.ID, commsExampleBody(t))
	if code != http.StatusInternalServerError || upkeepErrorMessage(resp) != commsStageBindingResolveFailed {
		t.Fatalf("response = %d %v, want 500 %q", code, resp, commsStageBindingResolveFailed)
	}
	if got := f.stageState(f.planStage.ID); got != run.StageStateRunning {
		t.Errorf("stage = %q, want running", got)
	}
}

// The handler's own read of the reporting run, AFTER the binding listed its
// stages (upkeepRereadFailRepo, so the ownership middleware and the binding
// read the run normally), finding the row vanished refuses as
// stage_binding_undecidable: 400, stage failed category-B, nothing stored.
func TestCommsReportIngest_ReportingRunVanished(t *testing.T) {
	var repo *upkeepRereadFailRepo
	f := newCommsIngestFixtureWith(t, userReportScanSpec(t), "user_report_scan", func(rr *upkeepRunRepo) run.Repository {
		repo = &upkeepRereadFailRepo{upkeepRunRepo: rr, err: run.ErrNotFound}
		return repo
	})
	repo.id = f.runRow.ID
	f.seedGather(t, f.planStage.ID, 1, commsExampleGather())
	code, resp := f.post(t, f.planStage.ID, commsExampleBody(t))
	assertCommsRefused(t, f, f.planStage.ID, code, resp, commsReportStageInvalidCode, commsRefusalStageBindingUndecidable)
}

func TestCommsUnaccountedReportIDs_AlwaysArray(t *testing.T) {
	r := &plan.CommsReport{}
	g := &commsScanGatheredPayload{}
	if got := commsUnaccountedReportIDs(r, g); got == nil || len(got) != 0 {
		t.Errorf("unaccounted = %#v, want a non-nil empty slice", got)
	}
	g.Shown = []commsShownReport{{ID: "UR-issue-2"}, {ID: "UR-issue-1"}, {ID: "UR-issue-2"}}
	if got := strings.Join(commsUnaccountedReportIDs(r, g), ","); got != "UR-issue-1,UR-issue-2" {
		t.Errorf("unaccounted = %s, want sorted distinct", got)
	}
}
