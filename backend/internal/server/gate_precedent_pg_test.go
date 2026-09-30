package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/decisionindex"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/precedent"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// gate_precedent_pg_test.go is the E75.4 / #3732 cross-boundary test: the REAL
// decision index, the REAL chained audit repository and the REAL gate-view /
// run-detail handlers on a REAL Postgres, so the seam between the ranking, the
// index read, the chain write and the HTTP payload is proven end to end.
//
// Every seed goes through helpers DEFINED IN THIS FILE (or exported package
// constructors) — no shared fixture file is touched.

const gpPGRepo = "acme/gate-precedent"

type gpPG struct {
	pool  *pgxpool.Pool
	audit audit.Repository
	srv   *Server
	// bare is the same wiring WITHOUT a precedent index — the pre-E75.4 server.
	bare *Server
}

func newGPPG(t *testing.T) *gpPG {
	t.Helper()
	pool := pgtest.NewPool(t)
	au := audit.NewPostgresRepository(pool)
	f := &gpPG{pool: pool, audit: au}
	f.srv = New(Config{Addr: "127.0.0.1:0",
		RunRepo:        run.NewPostgresRepository(pool),
		ConcernRepo:    concern.NewPostgresRepository(pool),
		AuditRepo:      au,
		PrecedentIndex: decisionindex.NewStore(pool),
	})
	f.bare = New(Config{Addr: "127.0.0.1:0",
		RunRepo:     run.NewPostgresRepository(pool),
		ConcernRepo: concern.NewPostgresRepository(pool),
		AuditRepo:   au,
	})
	return f
}

func (f *gpPG) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("seed %q: %v", sql, err)
	}
}

func (f *gpPG) seedRun(t *testing.T, acct *uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.New()
	f.exec(t, `INSERT INTO runs (id, repo, workflow_id, workflow_sha, trigger_source, state, runner_kind, account_id)
	           VALUES ($1, $2, 'feature_change', 'sha-gp', 'cli', 'running', 'local', $3)`, id, gpPGRepo, acct)
	return id
}

func (f *gpPG) seedStage(t *testing.T, runID uuid.UUID, seq int, kind, state string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	f.exec(t, `INSERT INTO stages (id, run_id, sequence, stage_type, executor_kind, executor_ref, state)
	           VALUES ($1, $2, $3, $4, 'agent', 'claude-code', $5)`, id, runID, seq, kind, state)
	return id
}

// seedDecision records a real approval_submitted entry on a prior run's plan
// stage and indexes it through the real Extract + Store. acct, when non-nil,
// tenants the index row.
func (f *gpPG) seedDecision(t *testing.T, acct *uuid.UUID, outcome, reason string) int64 {
	t.Helper()
	runID := f.seedRun(t, acct)
	stageID := f.seedStage(t, runID, 0, "plan", "succeeded")
	raw, err := json.Marshal(map[string]any{"decision": outcome, "rejection_comment": reason})
	if err != nil {
		t.Fatal(err)
	}
	kind := audit.ActorUser
	subject := "operator"
	e, err := f.audit.AppendChained(context.Background(), audit.ChainAppendParams{
		RunID: runID, StageID: &stageID, Timestamp: time.Now().UTC(), Category: "approval_submitted",
		ActorKind: &kind, ActorSubject: &subject, Payload: raw,
	})
	if err != nil {
		t.Fatalf("append approval_submitted: %v", err)
	}
	row, err := decisionindex.Extract(e, decisionindex.RowContext{
		Repo: gpPGRepo, WorkflowID: "feature_change", DoctrineVersion: "sha-gp", StageKind: "plan",
	})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	row.AccountID = acct
	if err := decisionindex.NewStore(f.pool).Upsert(context.Background(), *row); err != nil {
		t.Fatalf("index: %v", err)
	}
	return e.Sequence
}

// seedGateRun parks a fresh untenanted run at its plan gate.
func (f *gpPG) seedGateRun(t *testing.T) (runID, planStage uuid.UUID) {
	t.Helper()
	runID = f.seedRun(t, nil)
	planStage = f.seedStage(t, runID, 0, "plan", "awaiting_approval")
	f.seedStage(t, runID, 1, "implement", "pending")
	return runID, planStage
}

func gpGateView(t *testing.T, s *Server, runID uuid.UUID) (gateViewResponse, []byte) {
	t.Helper()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v0/runs/"+runID.String()+"/gate-view", nil)
	req.SetPathValue("run_id", runID.String())
	req = withIdentity(req, Identity{Subject: "github:op", TokenID: "tok-gp", Scopes: []string{scopeGateViewRead}})
	s.handleGetRunGateView(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("gate-view = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp gateViewResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode gate-view: %v", err)
	}
	return resp, w.Body.Bytes()
}

func gpGetRun(t *testing.T, s *Server, runID uuid.UUID) map[string]json.RawMessage {
	t.Helper()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v0/runs/"+runID.String(), nil)
	req.SetPathValue("run_id", runID.String())
	s.handleGetRun(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("get run = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode run: %v", err)
	}
	return m
}

func (f *gpPG) surfacedEntries(t *testing.T, runID uuid.UUID) []*audit.Entry {
	t.Helper()
	es, err := f.audit.ListForRunByCategory(context.Background(), runID, CategoryPrecedentSurfaced)
	if err != nil {
		t.Fatalf("list precedent_surfaced: %v", err)
	}
	return es
}

// TestGatePrecedent_SurfacesAndRecordsOnce: three prior plan_approval decisions
// in the repository, a fourth run parked at its plan gate.
func TestGatePrecedent_SurfacesAndRecordsOnce(t *testing.T) {
	f := newGPPG(t)
	seqs := map[int64]bool{}
	for i, outcome := range []string{"approve", "approve", "reject"} {
		seqs[f.seedDecision(t, nil, outcome, fmt.Sprintf("SENTINEL-gp-reason-%d", i))] = true
	}
	runID, planStage := f.seedGateRun(t)

	// (a) the gate view surfaces all three with scores, keys and a version.
	view, _ := gpGateView(t, f.srv, runID)
	b := view.Precedent
	if b == nil {
		t.Fatal("gate view carries no precedent block at an open plan gate")
	}
	if b.DecisionClass != "plan_approval" || b.StageID != planStage.String() || b.IndexVersion != precedent.IndexVersion {
		t.Errorf("block header = %s/%s/%s", b.DecisionClass, b.StageID, b.IndexVersion)
	}
	if len(b.Items) != 3 {
		t.Fatalf("cited %d items, want all 3 prior decisions", len(b.Items))
	}
	for _, it := range b.Items {
		if !seqs[it.SourceSequence] || it.SourceEntryHash == "" {
			t.Errorf("unexpected citation %d/%q", it.SourceSequence, it.SourceEntryHash)
		}
		if !strings.HasPrefix(it.ReasonExcerpt, "SENTINEL-gp-reason-") {
			t.Errorf("item %d excerpt = %q, want the chain reason (read at query time)", it.SourceSequence, it.ReasonExcerpt)
		}
	}
	if b.Summary.AgreementRatio <= 0 || b.Summary.ModalOutcome != "approve" {
		t.Errorf("summary = %+v, want a non-zero agreement ratio on approve", b.Summary)
	}

	// (b) exactly one entry, citing those three and the fingerprint.
	es := f.surfacedEntries(t, runID)
	if len(es) != 1 {
		t.Fatalf("precedent_surfaced entries = %d, want exactly 1", len(es))
	}
	var p precedentSurfacedPayload
	if err := json.Unmarshal(es[0].Payload, &p); err != nil {
		t.Fatal(err)
	}
	if p.Fingerprint != b.Fingerprint || len(p.Cited) != 3 || p.StageID != planStage.String() {
		t.Errorf("payload = %+v", p)
	}
	for _, c := range p.Cited {
		if !seqs[c.SourceSequence] {
			t.Errorf("payload cites %d, not a seeded decision", c.SourceSequence)
		}
	}

	// (c) re-read the gate view AND the run detail: still exactly one entry,
	// and the run detail carries the same block.
	gpGateView(t, f.srv, runID)
	runPayload := gpGetRun(t, f.srv, runID)
	var runBlock gatePrecedentBlock
	if err := json.Unmarshal(runPayload["precedent"], &runBlock); err != nil {
		t.Fatalf("run detail carries no decodable precedent block: %v", err)
	}
	if runBlock.Fingerprint != b.Fingerprint {
		t.Errorf("run-detail fingerprint %s != gate-view %s — the two surfaces disagree", runBlock.Fingerprint, b.Fingerprint)
	}
	if got := len(f.surfacedEntries(t, runID)); got != 1 {
		t.Fatalf("precedent_surfaced entries after 3 reads = %d, want STILL 1", got)
	}

	// (d) the payload carries NO reason prose; the surfaced block does.
	if strings.Contains(string(es[0].Payload), "SENTINEL-gp-reason-") {
		t.Errorf("the precedent_surfaced payload copied reason prose: %s", es[0].Payload)
	}
}

// TestGatePrecedent_AccountIsolated: a TENANTED and an UNTENANTED decision in
// the SAME repository and class; the untenanted gate run cites only the
// untenanted one — the account-scoped read is the only thing separating them.
func TestGatePrecedent_AccountIsolated(t *testing.T) {
	f := newGPPG(t)
	acct := uuid.New()
	f.exec(t, `INSERT INTO accounts (id, account_key) VALUES ($1, 'gp-tenant')`, acct)
	tenanted := f.seedDecision(t, &acct, "approve", "tenant-only reason")
	untenanted := f.seedDecision(t, nil, "approve", "shared reason")
	runID, _ := f.seedGateRun(t)

	view, _ := gpGateView(t, f.srv, runID)
	if view.Precedent == nil {
		t.Fatal("no precedent block")
	}
	if len(view.Precedent.Items) != 1 {
		t.Fatalf("cited %d items, want 1 (the tenanted row must be invisible to an untenanted run)", len(view.Precedent.Items))
	}
	if got := view.Precedent.Items[0].SourceSequence; got != untenanted || got == tenanted {
		t.Errorf("cited %d, want the untenanted %d", got, untenanted)
	}
}

// TestGatePrecedent_NoOpenGatePG: a running run in a repository WITH indexed
// decisions surfaces nothing and records nothing.
func TestGatePrecedent_NoOpenGatePG(t *testing.T) {
	f := newGPPG(t)
	f.seedDecision(t, nil, "approve", "r")
	runID := f.seedRun(t, nil)
	f.seedStage(t, runID, 0, "plan", "running")
	_, raw := gpGateView(t, f.srv, runID)
	if strings.Contains(string(raw), `"precedent"`) {
		t.Errorf("gate view carries a precedent key with no open gate: %s", raw)
	}
	if got := len(f.surfacedEntries(t, runID)); got != 0 {
		t.Errorf("entries = %d, want 0", got)
	}
}

// TestPrecedentChangesNoGateOutcome: the same seeded gate read with the index
// wired and with it nil yields identical run state, stage states, gate-view
// open/settled sets and run payload once the precedent field is dropped.
func TestPrecedentChangesNoGateOutcome(t *testing.T) {
	f := newGPPG(t)
	f.seedDecision(t, nil, "approve", "r1")
	f.seedDecision(t, nil, "reject", "r2")
	runID, _ := f.seedGateRun(t)

	withView, withRaw := gpGateView(t, f.srv, runID)
	if withView.Precedent == nil {
		t.Fatal("setup: the wired server must surface a block")
	}
	withRun := gpGetRun(t, f.srv, runID)

	bareView, bareRaw := gpGateView(t, f.bare, runID)
	bareRun := gpGetRun(t, f.bare, runID)

	if bareView.Precedent != nil || strings.Contains(string(bareRaw), `"precedent"`) {
		t.Fatalf("the index-less server surfaced precedent: %s", bareRaw)
	}
	withView.Precedent = nil
	a, _ := json.Marshal(withView)
	c, _ := json.Marshal(bareView)
	if string(a) != string(c) {
		t.Errorf("gate view differs beyond the precedent field:\nwith: %s\nbare: %s", a, c)
	}
	// RAW wire comparison, not just the typed one: gateViewResponse cannot
	// detect a difference in a field the struct does not declare, so the
	// byte-identical claim is only half-proved by the typed compare above.
	// map[string]json.RawMessage marshals with sorted keys, so both sides are
	// canonical and directly comparable.
	var withMap, bareMap map[string]json.RawMessage
	if err := json.Unmarshal(withRaw, &withMap); err != nil {
		t.Fatalf("unmarshal wired gate view: %v", err)
	}
	if err := json.Unmarshal(bareRaw, &bareMap); err != nil {
		t.Fatalf("unmarshal index-less gate view: %v", err)
	}
	delete(withMap, "precedent")
	wm, _ := json.Marshal(withMap)
	bm, _ := json.Marshal(bareMap)
	if string(wm) != string(bm) {
		t.Errorf("gate view WIRE differs beyond the precedent field:\nwith: %s\nbare: %s", wm, bm)
	}
	delete(withRun, "precedent")
	ra, _ := json.Marshal(withRun)
	rb, _ := json.Marshal(bareRun)
	if string(ra) != string(rb) {
		t.Errorf("run payload differs beyond the precedent field:\nwith: %s\nbare: %s", ra, rb)
	}

	got, err := run.NewPostgresRepository(f.pool).GetRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != run.StateRunning {
		t.Errorf("run state = %s, want running (precedent must not move the run)", got.State)
	}
	stages, err := run.NewPostgresRepository(f.pool).ListStagesForRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range stages {
		want := run.StageStatePending
		if st.Type == run.StageTypePlan {
			want = run.StageStateAwaitingApproval
		}
		if st.State != want {
			t.Errorf("stage %s state = %s, want %s", st.Type, st.State, want)
		}
	}
}
