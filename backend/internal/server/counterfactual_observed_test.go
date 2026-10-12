package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/bundle"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/delegation"
	"github.com/kuhlman-labs/fishhawk/backend/internal/drive"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// Runner-observed counterfactuals (E85.3 / #4143), server half.
//
// COUNTERFACTUAL CONVENTION: every arm guarding a control names the mutation
// that removes it and the fixture state that makes the removal observable as
// COMMITTED STATE (a stored audit row, a stored concern row and its state, a
// merge dispatch) rather than as a log line or an error identity.

// cfGoldenObservations reads the SHARED cross-module golden
// testdata/wire/counterfactual_observation.json — the exact bytes the runner
// folds into gate_evidence.counterfactual_observations. Its four members are,
// in order, green (index 0), red (1), timeout (2) and error/build_failed (3).
func cfGoldenObservations(t *testing.T) []json.RawMessage {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(pkgSrcDir, "..", "..", "..", "testdata", "wire", "counterfactual_observation.json"))
	if err != nil {
		t.Fatalf("read shared wire fixture: %v", err)
	}
	var m struct {
		Observations []json.RawMessage `json:"counterfactual_observations"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("decode shared wire fixture: %v", err)
	}
	if len(m.Observations) != 4 {
		t.Fatalf("shared wire fixture carries %d observations, want 4 (green, red, timeout, error)", len(m.Observations))
	}
	wantObserved := []string{"green", "red", "timeout", "error"}
	for i, raw := range m.Observations {
		var o bundle.CounterfactualObservationEvidence
		if err := json.Unmarshal(raw, &o); err != nil {
			t.Fatalf("decode golden observation %d: %v", i, err)
		}
		if o.Index != i || o.Observed != wantObserved[i] {
			t.Fatalf("golden observation %d = (index %d, observed %q), want (%d, %q) — the fixture no longer exercises this file's arms",
				i, o.Index, o.Observed, i, wantObserved[i])
		}
	}
	return m.Observations
}

// cfEvidence composes a gate_evidence payload carrying the given observations.
func cfEvidence(t *testing.T, obs ...json.RawMessage) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{"counterfactual_observations": obs})
	if err != nil {
		t.Fatalf("marshal gate_evidence: %v", err)
	}
	return string(b)
}

// cfObservationAt returns golden observation 0 re-keyed to index i with the
// given observed value (and no reason) — a well-formed observation the
// closed-vocabulary rules admit.
func cfObservationAt(t *testing.T, i int, observed string) json.RawMessage {
	t.Helper()
	return cfObservationEdit(t, func(o *bundle.CounterfactualObservationEvidence) {
		o.Index, o.Observed, o.Reason = i, observed, ""
	})
}

// cfObservationEdit returns golden observation 0 with edit applied.
func cfObservationEdit(t *testing.T, edit func(*bundle.CounterfactualObservationEvidence)) json.RawMessage {
	t.Helper()
	var o bundle.CounterfactualObservationEvidence
	if err := json.Unmarshal(cfGoldenObservations(t)[0], &o); err != nil {
		t.Fatalf("decode golden observation: %v", err)
	}
	edit(&o)
	b, err := json.Marshal(o)
	if err != nil {
		t.Fatalf("marshal observation: %v", err)
	}
	return b
}

// cfSeedPlanArtifact stores a standard_v1 plan artifact on planStageID
// declaring n counterfactuals — all the ingest's membership read needs.
func cfSeedPlanArtifact(t *testing.T, arts artifact.Repository, planStageID uuid.UUID, n int) {
	t.Helper()
	cfs := make([]map[string]any, n)
	for i := range cfs {
		cfs[i] = map[string]any{
			"control_path": "a/b.go",
			"mutation":     map[string]any{"find": "x", "replace": "y"},
			"tests":        map[string]any{"run": "^TestX$", "package": "./a"},
			"expect":       "red",
		}
	}
	content, err := json.Marshal(map[string]any{"counterfactuals": cfs})
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	v := "standard_v1"
	if _, err := arts.Create(context.Background(), artifact.CreateParams{
		StageID: planStageID, Kind: artifact.KindPlan, SchemaVersion: &v,
		Content: content, ContentHash: sha256Hex(content),
	}); err != nil {
		t.Fatalf("seed plan artifact: %v", err)
	}
}

// cfPlanRunRepo is the run-store half of the ingest's approved-plan read for
// direct recorder calls: ListStagesForRun names one plan stage (or none, or
// fails) and GetRun ends the parent walk. Nothing else is reachable from
// recordCounterfactualObservations.
type cfPlanRunRepo struct {
	run.Repository
	planStageID uuid.UUID
	listErr     error
}

func (r *cfPlanRunRepo) ListStagesForRun(_ context.Context, runID uuid.UUID) ([]*run.Stage, error) {
	if r.listErr != nil {
		return nil, r.listErr
	}
	if r.planStageID == uuid.Nil {
		return nil, nil
	}
	return []*run.Stage{{ID: r.planStageID, RunID: runID, Type: run.StageTypePlan}}, nil
}

func (r *cfPlanRunRepo) GetRun(_ context.Context, id uuid.UUID) (*run.Run, error) {
	return &run.Run{ID: id}, nil
}

// cfBundle packs a raw trace bundle whose gate_evidence carries obs.
func cfBundle(t *testing.T, obs ...json.RawMessage) []byte {
	t.Helper()
	return gateIsolationBundle(t, nil, cfEvidence(t, obs...))
}

// cfRows returns every appended counterfactual_observed row.
func cfRows(au *auditFake) []audit.ChainAppendParams {
	au.mu.Lock()
	defer au.mu.Unlock()
	var out []audit.ChainAppendParams
	for _, p := range au.appended {
		if p.Category == CategoryCounterfactualObserved {
			out = append(out, p)
		}
	}
	return out
}

// controlUnpinnedRows returns every control_unpinned row on the run.
func controlUnpinnedRows(t *testing.T, cr concern.Repository, runID uuid.UUID) []*concern.Concern {
	t.Helper()
	all, err := cr.ListByRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("ListByRun: %v", err)
	}
	var out []*concern.Concern
	for _, c := range all {
		if c.Category == controlUnpinnedConcernCategory {
			out = append(out, c)
		}
	}
	return out
}

// newCFTraceServer is newTraceServer plus a fake concern store.
func newCFTraceServer(t *testing.T) (*Server, *signingFake, *auditFake, *fakeConcernRepo) {
	t.Helper()
	s, sf, _, au := newTraceServer(t)
	cr := newFakeConcernRepo()
	s.cfg.ConcernRepo = cr
	return s, sf, au, cr
}

// --- cross-boundary, real Postgres ------------------------------------------

// TestCounterfactualObserved_PGTraceToGateViewAndWaive is the CROSS-BOUNDARY
// test: a raw trace bundle carrying the shared golden's four observations
// (plus a green one at index 4, which the stored approved plan does not
// declare) travels the REAL handleShipTrace (registered route, signature
// verified) into REAL Postgres audit + concern + artifact stores, then out
// through the gate view and the waive handler. It asserts on STORED rows: four system-actor, stage-anchored
// counterfactual_observed rows preserving each observed value, and exactly one
// control_unpinned concern whose stored provenance is server_check and whose
// origin sequence is the green row's chain sequence.
//
// COUNTERFACTUALS: (1) delete the recordCounterfactualObservations call in
// trace.go → zero rows → RED. (2) delete the `obs.Observed == green` guard →
// the red, timeout and error observations also mint concerns (4, not 1) → RED.
// (2b) make declaredCounterfactualCount return known=false → the undeclared
// green at index 4 is recorded and raises → 5 rows, 2 concerns → RED.
// (3) delete the Provenance stamp in raiseControlUnpinnedConcern → the run-bound
// mcp:run: token (which passes ordinary waive auth on its OWN run) waives the
// row: the read-back state is waived, not raised → RED.
func TestCounterfactualObserved_PGTraceToGateViewAndWaive(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	runs := run.NewPostgresRepository(pool)
	au := audit.NewPostgresRepository(pool)
	cr := concern.NewPostgresRepository(pool)
	arts := artifact.NewPostgresRepository(pool)
	sf := newSigningFake()
	s := New(Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      runs,
		AuditRepo:    au,
		ConcernRepo:  cr,
		ArtifactRepo: arts,
		SigningRepo:  sf,
		TraceStore:   newTraceStoreFake(),
	})
	rn, err := runs.CreateRun(ctx, run.CreateRunParams{
		Repo: "x/y", WorkflowID: "feature_change", WorkflowSHA: "abc", TriggerSource: run.TriggerCLI,
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	// The approved plan the ingest reads index membership from: it declares
	// the golden's four counterfactuals, so index 4 below is undeclared.
	planStage, err := runs.CreateStage(ctx, run.CreateStageParams{
		RunID: rn.ID, Sequence: 1, Type: run.StageTypePlan,
		ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code",
	})
	if err != nil {
		t.Fatalf("create plan stage: %v", err)
	}
	cfSeedPlanArtifact(t, arts, planStage.ID, 4)
	st, err := runs.CreateStage(ctx, run.CreateStageParams{
		RunID: rn.ID, Sequence: 2, Type: run.StageTypeImplement,
		ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code",
	})
	if err != nil {
		t.Fatalf("create stage: %v", err)
	}
	priv, _ := sf.issue(t, rn.ID)
	body := cfBundle(t, append(cfGoldenObservations(t), cfObservationAt(t, 4, "green"))...)

	if w := shipRequest(t, s, rn.ID, st.ID, "raw", priv, body, ""); w.Code != http.StatusAccepted {
		t.Fatalf("ship status = %d, want 202:\n%s", w.Code, w.Body.String())
	}
	s.waitBackgroundReviews()

	entries, err := au.ListForRunByCategory(ctx, rn.ID, CategoryCounterfactualObserved)
	if err != nil {
		t.Fatalf("list counterfactual_observed: %v", err)
	}
	if len(entries) != 4 {
		t.Fatalf("stored counterfactual_observed rows = %d, want 4 (the undeclared index 4 is rejected against the stored plan)", len(entries))
	}
	var greenSeq int64
	wantObserved := []string{"green", "red", "timeout", "error"}
	for i, e := range entries {
		if e.StageID == nil || *e.StageID != st.ID {
			t.Errorf("row %d stage = %v, want stage-anchored %s", i, e.StageID, st.ID)
		}
		if e.ActorKind == nil || *e.ActorKind != audit.ActorKind("system") {
			t.Errorf("row %d actor kind = %v, want system", i, e.ActorKind)
		}
		var p counterfactualObservedPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("decode row %d: %v", i, err)
		}
		if p.Index != i || p.Observed != wantObserved[i] {
			t.Errorf("row %d = (index %d, observed %q), want (%d, %q)", i, p.Index, p.Observed, i, wantObserved[i])
		}
		if p.Source != "runner" || p.StageID != st.ID.String() || p.ContentHash != sha256Hex(body) {
			t.Errorf("row %d coordinates = (source %q, stage %q, hash %q)", i, p.Source, p.StageID, p.ContentHash)
		}
		if p.Index == 0 {
			greenSeq = e.Sequence
		}
		if p.Observed == "error" && p.Reason != "build_failed" {
			t.Errorf("error row reason = %q, want build_failed", p.Reason)
		}
	}

	rows := controlUnpinnedRows(t, cr, rn.ID)
	if len(rows) != 1 {
		t.Fatalf("control_unpinned concerns = %d, want exactly 1 (only the GREEN observation raises)", len(rows))
	}
	row := rows[0]
	if row.Provenance != concern.ProvenanceServerCheck || !row.IsServerCheck() {
		t.Errorf("stored provenance = %q, want %q", row.Provenance, concern.ProvenanceServerCheck)
	}
	if row.StageID != st.ID || row.StageKind != concern.StageKindImplement || row.Severity != "medium" ||
		row.State != concern.StateRaised || row.ReviewerModel != nil {
		t.Errorf("stored row = %+v, want an implement/medium/raised row with no reviewer model", row)
	}
	if row.OriginReviewSequence != greenSeq || greenSeq == 0 {
		t.Errorf("origin_review_sequence = %d, want the green row's sequence %d", row.OriginReviewSequence, greenSeq)
	}
	if row.CheckKey != "control_unpinned|0|backend/internal/plan/validate.go" {
		t.Errorf("check_key = %q", row.CheckKey)
	}
	for _, want := range []string{"#0", "backend/internal/plan/validate.go", "^TestValidate_Counterfactuals$", "./backend/internal/plan", "3f9a1c7e5b2d"} {
		if !strings.Contains(row.Note, want) {
			t.Errorf("note missing %q: %s", want, row.Note)
		}
	}

	// Gate-visible: the gate view lists it in open[] with its category.
	w := callGateView(s, rn.ID, "stage_kind=implement", gateViewReadIdentity())
	if w.Code != http.StatusOK {
		t.Fatalf("gate-view = %d:\n%s", w.Code, w.Body.String())
	}
	var gv gateViewResponse
	if err := json.Unmarshal(w.Body.Bytes(), &gv); err != nil {
		t.Fatalf("decode gate-view: %v", err)
	}
	if !slices.ContainsFunc(gv.Open, func(c gateViewConcern) bool {
		return c.ID == row.ID && c.Category == controlUnpinnedConcernCategory && c.StageKind == concern.StageKindImplement
	}) {
		t.Errorf("gate-view open[] = %+v, want the control_unpinned concern %s", gv.Open, row.ID)
	}

	readState := func() concern.State {
		t.Helper()
		got, err := cr.GetByIDs(ctx, []uuid.UUID{row.ID})
		if err != nil || len(got) != 1 {
			t.Fatalf("read back concern: %v (%d rows)", err, len(got))
		}
		return got[0].State
	}

	// Human-only: a run-bound agent token on its OWN run is refused...
	w = postWaiveAs(t, s, row.ID.String(), waiveConcernRequest{Reason: "agent says the control is fine"}, withRunBoundAuth(rn.ID))
	assertRequiresHuman(t, w, row, clearVerbWaive, refusedActorAgentToken)
	if got := readState(); got != concern.StateRaised {
		t.Fatalf("stored state after an agent waive = %q, want raised", got)
	}
	// ...a delegated waive is refused...
	w = postWaiveAs(t, s, row.ID.String(), waiveConcernRequest{Reason: "delegated", Delegated: true}, withAuth)
	assertRequiresHuman(t, w, row, clearVerbWaive, refusedActorDelegated)
	if got := readState(); got != concern.StateRaised {
		t.Fatalf("stored state after a delegated waive = %q, want raised", got)
	}
	// ...and a human waive with a reason clears it.
	if w := postWaiveAs(t, s, row.ID.String(), waiveConcernRequest{Reason: "pinned by an integration test outside the selector"}, withAuth); w.Code != http.StatusOK {
		t.Fatalf("human waive = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	if got := readState(); got != concern.StateWaived {
		t.Fatalf("stored state after a human waive = %q, want waived", got)
	}
}

// --- delegated settlement ----------------------------------------------------

// TestCounterfactualObserved_GreenHoldsDelegatedMerge: on a run otherwise
// eligible for the delegated may_merge arm (stages succeeded, PR open, latest
// drive advance checks_green_awaiting_merge — the TestAutoDriveRunGate_Merge
// fixture), a GREEN observation's concern keeps AutoDriveRunGate from merging;
// a human waive releases it.
//
// COUNTERFACTUAL: delete the InsertRaised call in raiseControlUnpinnedConcern →
// the fixture has zero open concerns, the arm is eligible and the merger is
// called on the FIRST evaluation → RED.
func TestCounterfactualObserved_GreenHoldsDelegatedMerge(t *testing.T) {
	ctx := context.Background()
	s, repo, au, cr := newAutoDriveServer(t)
	runID, stages := startAutoDriveRun(t, s, repo)
	stages[0].State = run.StageStateSucceeded
	stages[1].State = run.StageStateSucceeded
	if _, err := repo.TransitionRun(ctx, runID, run.StateRunning); err != nil {
		t.Fatalf("TransitionRun -> running: %v", err)
	}
	if _, err := repo.SetRunPullRequestURL(ctx, runID, "https://github.com/x/y/pull/7"); err != nil {
		t.Fatalf("SetRunPullRequestURL: %v", err)
	}
	seedReviewEntry(t, au, runID, 5, drive.Category, drive.Advance{Rule: drive.RuleChecksGreenAwaitingMerge})

	obs := cfGoldenObservations(t)
	s.recordCounterfactualObservations(ctx, runID, stages[1].ID, "h-green", cfBundle(t, obs[0]))

	merger := &fakeMerger{}
	out, err := s.AutoDriveRunGate(ctx, getRun(t, repo, runID), campaignOperatorIdentity(), merger, nil)
	if err != nil {
		t.Fatalf("AutoDriveRunGate: %v", err)
	}
	if merger.called != 0 || (out.Acted && out.Action == delegation.ActionMerge) {
		t.Fatalf("delegated merge fired with a control_unpinned concern open: outcome %+v, merger called %d", out, merger.called)
	}

	rows := controlUnpinnedRows(t, cr, runID)
	if len(rows) != 1 {
		t.Fatalf("control_unpinned concerns = %d, want 1", len(rows))
	}
	if w := postWaiveAs(t, s, rows[0].ID.String(), waiveConcernRequest{Reason: "human: pinned elsewhere"}, withAuth); w.Code != http.StatusOK {
		t.Fatalf("human waive = %d:\n%s", w.Code, w.Body.String())
	}
	out, err = s.AutoDriveRunGate(ctx, getRun(t, repo, runID), campaignOperatorIdentity(), merger, nil)
	if err != nil {
		t.Fatalf("AutoDriveRunGate after waive: %v", err)
	}
	if merger.called != 1 || !out.Acted || out.Action != delegation.ActionMerge {
		t.Fatalf("after the human waive: outcome %+v, merger called %d, want one delegated merge", out, merger.called)
	}
}

// --- trace-handler placement -------------------------------------------------

// TestCounterfactualObserved_RawVariantOnly: a REDACTED upload of an
// observation-bearing bundle records nothing. The redacted POST is the only
// upload, so no dedup can mask a hoisted call. COUNTERFACTUAL: move the call
// outside the raw guard → 4 rows → RED.
func TestCounterfactualObserved_RawVariantOnly(t *testing.T) {
	s, sf, au, cr := newCFTraceServer(t)
	runID, stageID := uuid.New(), uuid.New()
	priv, _ := sf.issue(t, runID)
	if w := shipRequest(t, s, runID, stageID, "redacted", priv, cfBundle(t, cfGoldenObservations(t)...), ""); w.Code != http.StatusAccepted {
		t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
	}
	if n := len(cfRows(au)); n != 0 {
		t.Fatalf("redacted upload recorded %d rows, want 0", n)
	}
	if n := len(controlUnpinnedRows(t, cr, runID)); n != 0 {
		t.Fatalf("redacted upload raised %d concerns, want 0", n)
	}
}

// TestCounterfactualObserved_RecordedBeforeAgentFailure: an agent-failed
// bundle still records its observations, and they are on the chain BEFORE the
// stage is failed. COUNTERFACTUAL: move the call below the agent-failed branch
// → the snapshot taken at the failed transition lacks the category → RED.
func TestCounterfactualObserved_RecordedBeforeAgentFailure(t *testing.T) {
	s, sf, repo, au := newPolicyTraceServer(t, nil)
	snoop := &failSnoopRepo{policyRunRepo: repo, au: au}
	s.cfg.RunRepo = snoop
	priv, _ := sf.issue(t, repo.runRow.ID)
	body := gateIsolationBundle(t, map[string]any{
		"agent_failed":         true,
		"agent_failure_reason": "agent exited non-zero",
	}, cfEvidence(t, cfGoldenObservations(t)[1]))

	if w := shipRequest(t, s, repo.runRow.ID, repo.stage.ID, "raw", priv, body, ""); w.Code != http.StatusAccepted {
		t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
	}
	if repo.stage.State != run.StageStateFailed {
		t.Fatalf("stage state = %q, want failed (the agent-failed branch ran)", repo.stage.State)
	}
	if !slices.Contains(snoop.atFailed, CategoryCounterfactualObserved) {
		t.Fatalf("chain at the failed transition = %v; counterfactual_observed must already be on it", snoop.atFailed)
	}
}

// --- recorder failure modes --------------------------------------------------

// TestCounterfactualObserved_PayloadCarriesGoldenFields pins the bundle-to-audit
// boundary: each row's payload carries every golden key with the runner's exact
// bytes at its top level, plus stage_id, content_hash and source.
func TestCounterfactualObserved_PayloadCarriesGoldenFields(t *testing.T) {
	s, sf, au, _ := newCFTraceServer(t)
	runID, stageID := uuid.New(), uuid.New()
	priv, _ := sf.issue(t, runID)
	obs := cfGoldenObservations(t)
	if w := shipRequest(t, s, runID, stageID, "raw", priv, cfBundle(t, obs...), ""); w.Code != http.StatusAccepted {
		t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
	}
	rows := cfRows(au)
	if len(rows) != len(obs) {
		t.Fatalf("rows = %d, want %d", len(rows), len(obs))
	}
	for i, r := range rows {
		var got, want map[string]json.RawMessage
		if err := json.Unmarshal(r.Payload, &got); err != nil {
			t.Fatalf("decode row %d: %v", i, err)
		}
		if err := json.Unmarshal(obs[i], &want); err != nil {
			t.Fatalf("decode golden %d: %v", i, err)
		}
		for k, v := range want {
			if !bytes.Equal(got[k], v) {
				t.Errorf("row %d %s = %s, want the golden's %s", i, k, got[k], v)
			}
		}
		if string(got["source"]) != `"runner"` || string(got["stage_id"]) != `"`+stageID.String()+`"` {
			t.Errorf("row %d source/stage_id = %s/%s", i, got["source"], got["stage_id"])
		}
		if r.StageID == nil || *r.StageID != stageID || r.ActorKind == nil || *r.ActorKind != audit.ActorKind("system") {
			t.Errorf("row %d anchor/actor = %v/%v", i, r.StageID, r.ActorKind)
		}
	}
}

// TestCounterfactualObserved_NonGreenRaisesNothing: red, timeout and error are
// recorded and raise nothing. COUNTERFACTUAL: delete the green guard → 3
// concerns → RED.
func TestCounterfactualObserved_NonGreenRaisesNothing(t *testing.T) {
	s, sf, au, cr := newCFTraceServer(t)
	runID, stageID := uuid.New(), uuid.New()
	priv, _ := sf.issue(t, runID)
	obs := cfGoldenObservations(t)
	if w := shipRequest(t, s, runID, stageID, "raw", priv, cfBundle(t, obs[1], obs[2], obs[3]), ""); w.Code != http.StatusAccepted {
		t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
	}
	if n := len(cfRows(au)); n != 3 {
		t.Fatalf("rows = %d, want 3", n)
	}
	if n := len(controlUnpinnedRows(t, cr, runID)); n != 0 {
		t.Fatalf("non-green observations raised %d concerns, want 0", n)
	}
}

// TestCounterfactualObserved_RePostAndReobservationDeduplicated: a re-POST of
// the SAME raw bundle appends no rows (COUNTERFACTUAL: delete the
// (stage_id, content_hash, index) skip → 8 rows → RED), and a second bundle
// with a different content hash re-observing the same green records its rows
// but raises no second concern (COUNTERFACTUAL: delete the check_key dedup → 2
// concerns → RED). Another stage's green is a distinct concern, and one bundle
// repeating an index records it once.
func TestCounterfactualObserved_RePostAndReobservationDeduplicated(t *testing.T) {
	s, sf, au, cr := newCFTraceServer(t)
	runID, stageID := uuid.New(), uuid.New()
	priv, _ := sf.issue(t, runID)
	obs := cfGoldenObservations(t)
	body := cfBundle(t, obs...)
	for i := 0; i < 2; i++ {
		if w := shipRequest(t, s, runID, stageID, "raw", priv, body, ""); w.Code != http.StatusAccepted {
			t.Fatalf("POST %d status = %d:\n%s", i, w.Code, w.Body.String())
		}
	}
	if n := len(cfRows(au)); n != 4 {
		t.Fatalf("rows after a re-POST = %d, want 4 (deduplicated on stage_id + content_hash + index)", n)
	}
	if n := len(controlUnpinnedRows(t, cr, runID)); n != 1 {
		t.Fatalf("concerns after a re-POST = %d, want 1", n)
	}

	second := cfBundle(t, obs[0])
	if w := shipRequest(t, s, runID, stageID, "raw", priv, second, ""); w.Code != http.StatusAccepted {
		t.Fatalf("second bundle status = %d", w.Code)
	}
	if n := len(cfRows(au)); n != 5 {
		t.Fatalf("rows after a different bundle = %d, want 5 (a new content hash is a new record)", n)
	}
	if n := len(controlUnpinnedRows(t, cr, runID)); n != 1 {
		t.Fatalf("concerns after a re-observation = %d, want 1 (check_key dedup)", n)
	}

	if w := shipRequest(t, s, runID, uuid.New(), "raw", priv, second, ""); w.Code != http.StatusAccepted {
		t.Fatalf("other-stage status = %d", w.Code)
	}
	if n := len(controlUnpinnedRows(t, cr, runID)); n != 2 {
		t.Fatalf("concerns after another stage's green = %d, want 2 (dedup is stage-scoped)", n)
	}

	// One raw bundle repeating an index records it ONCE (concern 46e2c40c):
	// the bundle's index is the join key. COUNTERFACTUAL: delete
	// `recorded[obs.Index] = true` after the append → both copies append (8
	// rows, not 7) → RED. The repeated observation is red, so the check_key
	// concern dedup cannot mask the second append.
	before := len(cfRows(au))
	if w := shipRequest(t, s, runID, uuid.New(), "raw", priv, cfBundle(t, obs[1], obs[1]), ""); w.Code != http.StatusAccepted {
		t.Fatalf("repeated-index bundle status = %d", w.Code)
	}
	if n := len(cfRows(au)) - before; n != 1 {
		t.Fatalf("rows from one bundle repeating index 1 = %d, want 1 (intra-bundle dedup)", n)
	}
}

// TestCounterfactualObserved_SettledStatesSuppressReraise: a waived or deferred
// row with the same check key suppresses a re-raise; an addressed one does not.
// Rows are seeded BY CONSTRUCTION. COUNTERFACTUAL: drop the waived/deferred
// clause from the dedup → the waived and deferred arms raise a second concern
// → RED; drop the state test entirely (any row suppresses) → the addressed arm
// raises nothing → RED.
func TestCounterfactualObserved_SettledStatesSuppressReraise(t *testing.T) {
	cases := []struct {
		state     concern.State
		wantRaise bool
	}{
		{concern.StateWaived, false},
		{concern.StateDeferred, false},
		{concern.StateAddressed, true},
	}
	for _, c := range cases {
		t.Run(string(c.state), func(t *testing.T) {
			s, _, au, cr := newCFTraceServer(t)
			runID, stageID := uuid.New(), uuid.New()
			obs := cfGoldenObservations(t)
			s.recordCounterfactualObservations(context.Background(), runID, stageID, "h1", cfBundle(t, obs[0]))
			rows := controlUnpinnedRows(t, cr, runID)
			if len(rows) != 1 {
				t.Fatalf("seed: concerns = %d, want 1", len(rows))
			}
			cr.mu.Lock()
			rows[0].State = c.state
			cr.mu.Unlock()

			s.recordCounterfactualObservations(context.Background(), runID, stageID, "h2", cfBundle(t, obs[0]))
			if n := len(cfRows(au)); n != 2 {
				t.Fatalf("rows = %d, want 2", n)
			}
			got := len(controlUnpinnedRows(t, cr, runID)) - 1
			if c.wantRaise && got != 1 {
				t.Errorf("a %s row suppressed the re-raise; want a fresh concern", c.state)
			}
			if !c.wantRaise && got != 0 {
				t.Errorf("a %s row did not suppress the re-raise: %d new concerns", c.state, got)
			}
		})
	}
}

// TestCounterfactualObserved_ConcernListErrorFailsOpen: a failed ListByRun
// raises anyway. COUNTERFACTUAL: return on the list error → 0 concerns → RED.
func TestCounterfactualObserved_ConcernListErrorFailsOpen(t *testing.T) {
	s, _, _, cr := newCFTraceServer(t)
	cr.listErr = errors.New("concern store outage")
	runID, stageID := uuid.New(), uuid.New()
	s.recordCounterfactualObservations(context.Background(), runID, stageID, "h", cfBundle(t, cfGoldenObservations(t)[0]))
	cr.listErr = nil
	if n := len(controlUnpinnedRows(t, cr, runID)); n != 1 {
		t.Fatalf("concerns with a failed dedup read = %d, want 1 (fail open)", n)
	}
}

// TestCounterfactualObserved_DedupReadErrorStillRecords: a failed audit dedup
// read appends every observation. COUNTERFACTUAL: return on the read error →
// 0 rows → RED.
func TestCounterfactualObserved_DedupReadErrorStillRecords(t *testing.T) {
	s, sf, au, _ := newCFTraceServer(t)
	au.listByCategoryErrCategory = CategoryCounterfactualObserved
	runID, stageID := uuid.New(), uuid.New()
	priv, _ := sf.issue(t, runID)
	if w := shipRequest(t, s, runID, stageID, "raw", priv, cfBundle(t, cfGoldenObservations(t)...), ""); w.Code != http.StatusAccepted {
		t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
	}
	if n := len(cfRows(au)); n != 4 {
		t.Fatalf("rows with a failed dedup read = %d, want 4", n)
	}
}

// TestCounterfactualObserved_AppendFailureRaisesNothing: a failed append never
// fails the upload and raises no concern (no concern without its origin row).
// COUNTERFACTUAL: drop the `continue` after the append error → the nil entry is
// dereferenced and the upload panics → RED.
func TestCounterfactualObserved_AppendFailureRaisesNothing(t *testing.T) {
	s, sf, au, cr := newCFTraceServer(t)
	au.appendErrCategory = CategoryCounterfactualObserved
	runID, stageID := uuid.New(), uuid.New()
	priv, _ := sf.issue(t, runID)
	if w := shipRequest(t, s, runID, stageID, "raw", priv, cfBundle(t, cfGoldenObservations(t)...), ""); w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 despite the failed append:\n%s", w.Code, w.Body.String())
	}
	if n := len(cfRows(au)); n != 0 {
		t.Fatalf("rows = %d, want 0", n)
	}
	if n := len(controlUnpinnedRows(t, cr, runID)); n != 0 {
		t.Fatalf("concerns = %d, want 0 (no concern without its origin row)", n)
	}
}

// TestCounterfactualObserved_InsertFailureKeepsRow: a failed InsertRaised
// leaves the audit row as the record.
func TestCounterfactualObserved_InsertFailureKeepsRow(t *testing.T) {
	s, _, au, cr := newCFTraceServer(t)
	cr.insertErr = errors.New("concern store outage")
	runID, stageID := uuid.New(), uuid.New()
	s.recordCounterfactualObservations(context.Background(), runID, stageID, "h", cfBundle(t, cfGoldenObservations(t)[0]))
	if n := len(cfRows(au)); n != 1 {
		t.Fatalf("rows = %d, want 1", n)
	}
	cr.insertErr = nil
	if n := len(controlUnpinnedRows(t, cr, runID)); n != 0 {
		t.Fatalf("concerns = %d, want 0", n)
	}
}

func TestCounterfactualObserved_NothingWithoutObservations(t *testing.T) {
	cases := map[string]string{
		"no gate_evidence event":           "",
		"gate_evidence without the member": `{"verify_runs":[{"command":"scripts/test verify","exit_code":0,"outcome":"passed"}]}`,
		"empty member":                     `{"counterfactual_observations":[]}`,
		"unparsable gate_evidence":         `{"counterfactual_observations":"not-a-list"}`,
	}
	for name, ge := range cases {
		t.Run(name, func(t *testing.T) {
			s, _, au, cr := newCFTraceServer(t)
			runID := uuid.New()
			s.recordCounterfactualObservations(context.Background(), runID, uuid.New(), "h", gateIsolationBundle(t, nil, ge))
			if n := len(cfRows(au)); n != 0 {
				t.Fatalf("rows = %d, want 0", n)
			}
			if n := len(controlUnpinnedRows(t, cr, runID)); n != 0 {
				t.Fatalf("concerns = %d, want 0", n)
			}
		})
	}
}

func TestCounterfactualObserved_NilStoresAreSafe(t *testing.T) {
	s, _, au, _ := newCFTraceServer(t)
	s.cfg.ConcernRepo = nil
	s.recordCounterfactualObservations(context.Background(), uuid.New(), uuid.New(), "h", cfBundle(t, cfGoldenObservations(t)[0]))
	if n := len(cfRows(au)); n != 1 {
		t.Fatalf("rows with no concern store = %d, want 1", n)
	}
	s.cfg.AuditRepo = nil
	// Must not panic.
	s.recordCounterfactualObservations(context.Background(), uuid.New(), uuid.New(), "h", cfBundle(t, cfGoldenObservations(t)[0]))
}

// --- ingest admission (fix-up A) ---------------------------------------------

// TestCounterfactualOutcomes_MatchSharedFixture pins the backend's closed
// vocabulary against testdata/wire/counterfactual_outcomes.json, which the
// runner's tests pin its cfOutcome* / cfReason* constants against too.
// COUNTERFACTUAL: add or drop a value in counterfactualObservedValues,
// counterfactualBaselineValues or counterfactualReasonValues → RED.
func TestCounterfactualOutcomes_MatchSharedFixture(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(pkgSrcDir, "..", "..", "..", "testdata", "wire", "counterfactual_outcomes.json"))
	if err != nil {
		t.Fatalf("read shared fixture: %v", err)
	}
	var f struct {
		Observed []string `json:"observed"`
		Baseline []string `json:"baseline"`
		Reason   []string `json:"reason"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatalf("decode shared fixture: %v", err)
	}
	for name, pair := range map[string][2][]string{
		"observed": {f.Observed, counterfactualObservedValues},
		"baseline": {f.Baseline, counterfactualBaselineValues},
		"reason":   {f.Reason, counterfactualReasonValues},
	} {
		if len(pair[0]) == 0 || !slices.Equal(pair[0], pair[1]) {
			t.Errorf("%s vocabulary = %v, shared fixture = %v", name, pair[1], pair[0])
		}
	}
}

// cfAdmitCase records one bundle of [valid, bad] observations through the
// recorder on a minimal-config server (index membership unknown, so only the
// rule under test can refuse bad) and returns the stored rows plus the
// admission's rejections for the same input.
func cfAdmitCase(t *testing.T, bad json.RawMessage) ([]audit.ChainAppendParams, []error) {
	t.Helper()
	s, _, au, _ := newCFTraceServer(t)
	valid := cfGoldenObservations(t)[1]
	s.recordCounterfactualObservations(context.Background(), uuid.New(), uuid.New(), "h", cfBundle(t, valid, bad))
	var decoded []bundle.CounterfactualObservationEvidence
	for _, raw := range []json.RawMessage{valid, bad} {
		var o bundle.CounterfactualObservationEvidence
		if err := json.Unmarshal(raw, &o); err != nil {
			t.Fatalf("decode: %v", err)
		}
		decoded = append(decoded, o)
	}
	_, rejected := admitCounterfactualObservations(decoded, 0, false)
	return cfRows(au), rejected
}

// cfWantRejected asserts exactly the valid observation was stored and the bad
// one was refused with want.
func cfWantRejected(t *testing.T, rows []audit.ChainAppendParams, rejected []error, want error) {
	t.Helper()
	if len(rows) != 1 {
		t.Fatalf("stored rows = %d, want 1 (only the valid observation)", len(rows))
	}
	if len(rejected) != 1 || !errors.Is(rejected[0], want) {
		t.Fatalf("rejections = %v, want exactly one wrapping %q", rejected, want)
	}
}

// TestCounterfactualObserved_UnknownObservedRejected. COUNTERFACTUAL: delete
// the counterfactualObservedValues case in admitCounterfactualObservations →
// "purple" is stored (2 rows) and nothing is rejected → RED.
func TestCounterfactualObserved_UnknownObservedRejected(t *testing.T) {
	rows, rejected := cfAdmitCase(t, cfObservationEdit(t, func(o *bundle.CounterfactualObservationEvidence) { o.Index, o.Observed = 2, "purple" }))
	cfWantRejected(t, rows, rejected, errCounterfactualObservedUnknown)
}

// TestCounterfactualObserved_UnknownBaselineRejected. COUNTERFACTUAL: delete
// the counterfactualBaselineValues case → "maybe" is stored → RED.
func TestCounterfactualObserved_UnknownBaselineRejected(t *testing.T) {
	rows, rejected := cfAdmitCase(t, cfObservationEdit(t, func(o *bundle.CounterfactualObservationEvidence) { o.Index, o.Baseline = 2, "maybe" }))
	cfWantRejected(t, rows, rejected, errCounterfactualBaselineUnknown)
}

// TestCounterfactualObserved_UnknownReasonRejected. COUNTERFACTUAL: delete the
// counterfactualReasonValues case → "made_up" is stored → RED. An EMPTY reason
// (red and green carry none) stays admitted: the valid observation has one.
func TestCounterfactualObserved_UnknownReasonRejected(t *testing.T) {
	rows, rejected := cfAdmitCase(t, cfObservationEdit(t, func(o *bundle.CounterfactualObservationEvidence) {
		o.Index, o.Observed, o.Reason = 2, "error", "made_up"
	}))
	cfWantRejected(t, rows, rejected, errCounterfactualReasonUnknown)
}

// TestCounterfactualObserved_RejectedReasonQuoteTruncated: a 4 KiB unknown
// reason is quoted in its rejection error cut to 1 KiB plus the marker, so a
// hostile reason cannot balloon the WARN log. COUNTERFACTUAL: make
// quoteCounterfactualValue return v unchanged → the error carries the whole
// 4 KiB and no marker → RED.
func TestCounterfactualObserved_RejectedReasonQuoteTruncated(t *testing.T) {
	long := strings.Repeat("é", 2048) // 4096 bytes of two-byte runes
	_, rejected := admitCounterfactualObservations([]bundle.CounterfactualObservationEvidence{{
		Index: 0, Baseline: "green", Observed: "error", Reason: long,
	}}, 0, false)
	if len(rejected) != 1 || !errors.Is(rejected[0], errCounterfactualReasonUnknown) {
		t.Fatalf("rejections = %v, want one errCounterfactualReasonUnknown", rejected)
	}
	msg := rejected[0].Error()
	if !strings.Contains(msg, counterfactualTruncatedMarker) || strings.Contains(msg, long) {
		t.Fatalf("rejection quotes the reason untruncated (%d bytes): %.120s…", len(msg), msg)
	}
	if got := quoteCounterfactualValue(long); len(got) > counterfactualReasonQuoteMaxBytes+len(counterfactualTruncatedMarker) || !utf8.ValidString(got) {
		t.Fatalf("quoted reason = %d bytes (valid UTF-8 %v), want ≤ %d + marker", len(got), utf8.ValidString(got), counterfactualReasonQuoteMaxBytes)
	}
	if got := quoteCounterfactualValue("build_failed"); got != "build_failed" {
		t.Fatalf("a short value is quoted as %q, want it verbatim", got)
	}
}

// TestCounterfactualObserved_CountCapped: a bundle of nine distinct-index
// observations records the first plan.MaxCounterfactuals (8) and refuses the
// tail with errCounterfactualTooMany. Minimal config, so no plan membership
// read can mask the cap. COUNTERFACTUAL: delete the `len(obs) >
// plan.MaxCounterfactuals` block → 9 rows → RED.
func TestCounterfactualObserved_CountCapped(t *testing.T) {
	s, _, au, _ := newCFTraceServer(t)
	var raws []json.RawMessage
	var decoded []bundle.CounterfactualObservationEvidence
	for i := range plan.MaxCounterfactuals + 1 {
		raw := cfObservationAt(t, i, "red")
		raws = append(raws, raw)
		var o bundle.CounterfactualObservationEvidence
		_ = json.Unmarshal(raw, &o)
		decoded = append(decoded, o)
	}
	s.recordCounterfactualObservations(context.Background(), uuid.New(), uuid.New(), "h", cfBundle(t, raws...))
	if n := len(cfRows(au)); n != plan.MaxCounterfactuals {
		t.Fatalf("rows = %d, want %d (the cap)", n, plan.MaxCounterfactuals)
	}
	admitted, rejected := admitCounterfactualObservations(decoded, 0, false)
	if len(admitted) != plan.MaxCounterfactuals || len(rejected) != 1 || !errors.Is(rejected[0], errCounterfactualTooMany) {
		t.Fatalf("admitted %d, rejected %v; want %d and one errCounterfactualTooMany", len(admitted), rejected, plan.MaxCounterfactuals)
	}
}

// TestCounterfactualObserved_IndexMustBeDeclared reads membership from the
// run's approved plan (declaring 2): indexes 0 and 1 are recorded, 3 is
// refused; a wired run with NO approved plan declares none; a failed plan read
// records nothing. COUNTERFACTUALS: delete the `known && o.Index >= declared`
// clause → index 3 is stored (3 rows) → RED; make a nil plan return known=false
// → the no-plan arm stores rows → RED; on the read error, fall through instead
// of returning → the error arm stores rows → RED.
func TestCounterfactualObserved_IndexMustBeDeclared(t *testing.T) {
	newServer := func(t *testing.T, rr *cfPlanRunRepo, declared int) (*Server, *auditFake) {
		t.Helper()
		s, _, au, _ := newCFTraceServer(t)
		arts := newFakeArtifactRepo()
		if rr.planStageID != uuid.Nil {
			cfSeedPlanArtifact(t, arts, rr.planStageID, declared)
		}
		s.cfg.RunRepo, s.cfg.ArtifactRepo = rr, arts
		return s, au
	}
	body := cfBundle(t, cfObservationAt(t, 0, "red"), cfObservationAt(t, 1, "red"), cfObservationAt(t, 3, "green"))

	t.Run("declared", func(t *testing.T) {
		s, au := newServer(t, &cfPlanRunRepo{planStageID: uuid.New()}, 2)
		s.recordCounterfactualObservations(context.Background(), uuid.New(), uuid.New(), "h", body)
		rows := cfRows(au)
		if len(rows) != 2 {
			t.Fatalf("rows = %d, want 2 (index 3 is undeclared)", len(rows))
		}
		for _, r := range rows {
			var p counterfactualObservedPayload
			_ = json.Unmarshal(r.Payload, &p)
			if p.Index > 1 {
				t.Fatalf("stored undeclared index %d", p.Index)
			}
		}
		_, rejected := admitCounterfactualObservations([]bundle.CounterfactualObservationEvidence{{Index: 3, Baseline: "green", Observed: "green"}}, 2, true)
		if len(rejected) != 1 || !errors.Is(rejected[0], errCounterfactualIndexUndeclared) {
			t.Fatalf("rejections = %v, want errCounterfactualIndexUndeclared", rejected)
		}
	})
	t.Run("no approved plan", func(t *testing.T) {
		s, au := newServer(t, &cfPlanRunRepo{}, 0)
		s.recordCounterfactualObservations(context.Background(), uuid.New(), uuid.New(), "h", body)
		if n := len(cfRows(au)); n != 0 {
			t.Fatalf("rows = %d, want 0 (a run with no approved plan declares no counterfactual)", n)
		}
	})
	t.Run("plan read error", func(t *testing.T) {
		s, au := newServer(t, &cfPlanRunRepo{planStageID: uuid.New(), listErr: errors.New("run store outage")}, 2)
		s.recordCounterfactualObservations(context.Background(), uuid.New(), uuid.New(), "h", body)
		if n := len(cfRows(au)); n != 0 {
			t.Fatalf("rows = %d, want 0 (an undecidable membership records nothing)", n)
		}
	})
	t.Run("negative index without a plan read", func(t *testing.T) {
		_, rejected := admitCounterfactualObservations([]bundle.CounterfactualObservationEvidence{{Index: -1, Baseline: "green", Observed: "red"}}, 0, false)
		if len(rejected) != 1 || !errors.Is(rejected[0], errCounterfactualIndexUndeclared) {
			t.Fatalf("rejections = %v, want errCounterfactualIndexUndeclared for index -1", rejected)
		}
	})
}

func TestControlUnpinnedNote(t *testing.T) {
	obs := bundle.CounterfactualObservationEvidence{
		Index: 2, ControlPath: "a/b.go", TestRun: "^TestX$", TestPackage: "./a", HeadSHA: "0123456789abcdef0123",
	}
	note := controlUnpinnedNote(obs)
	for _, want := range []string{"#2", "a/b.go", "^TestX$", "./a", "head 0123456789ab,", "human only"} {
		if !strings.Contains(note, want) {
			t.Errorf("note missing %q: %s", want, note)
		}
	}
	if strings.Contains(note, "0123456789abc") {
		t.Errorf("note carries more than the 12-char short head: %s", note)
	}
	obs.HeadSHA = ""
	if note := controlUnpinnedNote(obs); !strings.Contains(note, "head unknown") {
		t.Errorf("empty head renders %q, want 'head unknown'", note)
	}
}
