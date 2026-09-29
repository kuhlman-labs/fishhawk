package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/decisionindex"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
)

// precedent_pg_test.go is the CROSS-BOUNDARY test the per-layer units cannot
// stand in for (E75.3 / #3731). GET /v0/precedent spans request parsing, the
// derived-context resolver, persistence and the ranked render; the handler unit
// tests use a fake index, and the store tests never go through HTTP. This one
// drives the REAL handler over the REAL decisionindex.Store on a REAL Postgres,
// for BOTH input modes, with two accounts and two repositories seeded so the
// isolation is proven end to end rather than at one layer.

type precedentPGFixture struct {
	pool  *pgxpool.Pool
	s     *Server
	audit audit.Repository

	accountA, accountB uuid.UUID
	runA, runB, runC   uuid.UUID
	planStage, implStg uuid.UUID
}

// newPrecedentPGFixture seeds:
//
//	account A: runA (acme/widgets, plan + implement stages, a plan artifact
//	           carrying scope.files, a same-stage AND a different-stage
//	           escalation_fired), runB (other/repo)
//	account B: runC (acme/widgets — the SAME repository under a FOREIGN account,
//	           which is what makes the account arm the only thing that can drop
//	           its rows)
func newPrecedentPGFixture(t *testing.T) *precedentPGFixture {
	t.Helper()
	pool := pgtest.NewPool(t)
	f := &precedentPGFixture{
		pool:      pool,
		audit:     audit.NewPostgresRepository(pool),
		accountA:  uuid.MustParse(testOperatorAccountID),
		accountB:  uuid.New(),
		runA:      uuid.New(),
		runB:      uuid.New(),
		runC:      uuid.New(),
		planStage: uuid.New(),
		implStg:   uuid.New(),
	}
	f.s = New(Config{Addr: "127.0.0.1:0",
		PrecedentIndex: decisionindex.NewStore(pool),
		AuditRepo:      f.audit,
	})
	f.exec(t, `INSERT INTO accounts (id, account_key) VALUES ($1, 'pcd-a'), ($2, 'pcd-b')`,
		f.accountA, f.accountB)
	f.seedRun(t, f.runA, "acme/widgets", &f.accountA)
	f.seedRun(t, f.runB, "other/repo", &f.accountA)
	f.seedRun(t, f.runC, "acme/widgets", &f.accountB)
	f.seedStage(t, f.planStage, f.runA, 0, "plan")
	f.seedStage(t, f.implStg, f.runA, 1, "implement")
	return f
}

func (f *precedentPGFixture) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("seed %q: %v", sql, err)
	}
}

func (f *precedentPGFixture) seedRun(t *testing.T, id uuid.UUID, repo string, acct *uuid.UUID) {
	t.Helper()
	f.exec(t, `INSERT INTO runs (id, repo, workflow_id, workflow_sha, trigger_source, state, runner_kind, account_id)
	           VALUES ($1, $2, 'feature_change', 'sha-pcd', 'cli', 'pending', 'local', $3)`, id, repo, acct)
}

func (f *precedentPGFixture) seedStage(t *testing.T, id, runID uuid.UUID, seq int, kind string) {
	t.Helper()
	f.exec(t, `INSERT INTO stages (id, run_id, sequence, stage_type, executor_kind, executor_ref, state)
	           VALUES ($1, $2, $3, $4, 'agent', 'claude-code', 'pending')`, id, runID, seq, kind)
}

func (f *precedentPGFixture) seedPlanArtifact(t *testing.T, stageID uuid.UUID, paths ...string) {
	t.Helper()
	files := make([]map[string]string, 0, len(paths))
	for _, p := range paths {
		files = append(files, map[string]string{"path": p, "operation": "modify"})
	}
	content, err := json.Marshal(map[string]any{"scope": map[string]any{"files": files}})
	if err != nil {
		t.Fatal(err)
	}
	f.exec(t, `INSERT INTO artifacts (id, stage_id, kind, content, content_hash, created_at)
	           VALUES ($1, $2, 'plan', $3, $4, now())`, uuid.New(), stageID, content, "h-"+uuid.NewString())
}

// appendEntry appends through the REAL chained audit repository, so the reason
// excerpt is read back from a genuine chain entry.
func (f *precedentPGFixture) appendEntry(t *testing.T, runID uuid.UUID, stageID *uuid.UUID, category string, payload map[string]any) *audit.Entry {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	kind := audit.ActorUser
	subject := "operator"
	e, err := f.audit.AppendChained(context.Background(), audit.ChainAppendParams{
		RunID: runID, StageID: stageID, Timestamp: time.Now().UTC(), Category: category,
		ActorKind: &kind, ActorSubject: &subject, Payload: raw,
	})
	if err != nil {
		t.Fatalf("append %s: %v", category, err)
	}
	return e
}

// seedDecision appends a real decision-bearing entry and indexes it through the
// real Extract + Store, so the row under test is the product's own.
func (f *precedentPGFixture) seedDecision(t *testing.T, runID uuid.UUID, stageID *uuid.UUID, repo, stageKind, outcome, reason string, paths, keys []string) int64 {
	t.Helper()
	e := f.appendEntry(t, runID, stageID, "approval_submitted",
		map[string]any{"decision": outcome, "rejection_comment": reason})
	row, err := decisionindex.Extract(e, decisionindex.RowContext{
		Repo: repo, WorkflowID: "feature_change", DoctrineVersion: "sha-pcd",
		StageKind: stageKind, TouchedPaths: paths, EscalationKeys: keys,
	})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if err := decisionindex.NewStore(f.pool).Upsert(context.Background(), *row); err != nil {
		t.Fatalf("index: %v", err)
	}
	return e.Sequence
}

// TestPrecedentPG_ExplicitContextRanksAcrossTheRealBoundary drives the explicit
// mode: the ranked order, each item's matched keys and cited
// source_sequence/entry_hash, the query-time reason excerpt read from the real
// chain, and that NEITHER the other repository's NOR the other account's rows
// appear.
func TestPrecedentPG_ExplicitContextRanksAcrossTheRealBoundary(t *testing.T) {
	f := newPrecedentPGFixture(t)

	strongSeq := f.seedDecision(t, f.runA, &f.planStage, "acme/widgets", "plan", "approve",
		"same package, approved before", []string{"backend/internal/server/foo.go"}, []string{"scope_cap_exceeded"})
	weakSeq := f.seedDecision(t, f.runA, &f.planStage, "acme/widgets", "plan", "reject",
		"unrelated area", []string{"frontend/src/App.tsx"}, nil)
	otherRepoSeq := f.seedDecision(t, f.runB, nil, "other/repo", "plan", "approve",
		"different repository", []string{"backend/internal/server/foo.go"}, nil)
	foreignAcctSeq := f.seedDecision(t, f.runC, nil, "acme/widgets", "plan", "approve",
		"foreign account", []string{"backend/internal/server/foo.go"}, nil)

	rec := precedentGET(t, f.s, "?repo=acme/widgets&decision_class=plan_approval&stage_kind=plan"+
		"&paths=backend/internal/server/bar.go&escalation_keys=scope_cap_exceeded")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (body %s)", rec.Code, rec.Body.String())
	}
	resp := decodePrecedent(t, rec)

	if len(resp.Results) != 2 {
		t.Fatalf("results = %d, want exactly 2 (the other repository's and the other account's rows must be absent): %+v",
			len(resp.Results), resp.Results)
	}
	for _, it := range resp.Results {
		if it.SourceSequence == otherRepoSeq {
			t.Errorf("a row from other/repo (sequence %d) leaked into an acme/widgets query", otherRepoSeq)
		}
		if it.SourceSequence == foreignAcctSeq {
			t.Errorf("a row from ANOTHER ACCOUNT (sequence %d) leaked into the query", foreignAcctSeq)
		}
		if it.SourceEntryHash == "" {
			t.Errorf("item %d cites no entry hash", it.SourceSequence)
		}
	}

	top := resp.Results[0]
	if top.SourceSequence != strongSeq {
		t.Errorf("highest-ranked = %d, want the same-package + shared-escalation decision %d", top.SourceSequence, strongSeq)
	}
	if top.Score.TouchedPaths <= 0 || top.Score.EscalationKeys <= 0 {
		t.Errorf("top item's components = %+v, want both the path and escalation signals to have contributed", top.Score)
	}
	if len(top.MatchedKeys.EscalationKeys) != 1 || top.MatchedKeys.EscalationKeys[0] != "scope_cap_exceeded" {
		t.Errorf("matched escalation keys = %v, want [scope_cap_exceeded]", top.MatchedKeys.EscalationKeys)
	}
	if len(top.MatchedKeys.TouchedPathPrefixes) == 0 {
		t.Error("the sibling-path match named no shared prefix")
	}
	// The excerpt comes from the REAL chain entry, at query time.
	if top.ReasonExcerpt != "same package, approved before" {
		t.Errorf("reason excerpt = %q, want the chain entry's own reason", top.ReasonExcerpt)
	}
	if resp.Results[1].SourceSequence != weakSeq {
		t.Errorf("second item = %d, want the weaker %d", resp.Results[1].SourceSequence, weakSeq)
	}
	if resp.Summary.Count != 2 || resp.Summary.ModalOutcome == "" {
		t.Errorf("summary = %+v, want 2 items and a named modal outcome", resp.Summary)
	}
}

// TestPrecedentPG_GateReferenceDerivesFromTheRealJoins drives the GATE-REFERENCE
// mode: the echoed context must equal the seeded plan's paths and the SAME
// STAGE's fired escalation keys, and the ranking must have used them.
func TestPrecedentPG_GateReferenceDerivesFromTheRealJoins(t *testing.T) {
	f := newPrecedentPGFixture(t)
	f.seedPlanArtifact(t, f.planStage, "backend/internal/server/foo.go", "backend/internal/server/bar.go")
	// A DIFFERENT stage's escalation must not be picked up.
	f.appendEntry(t, f.runA, &f.planStage, "escalation_fired",
		map[string]any{"fired_keys": []string{"other_stage_rule"}})
	f.appendEntry(t, f.runA, &f.implStg, "escalation_fired",
		map[string]any{"fired_keys": []string{"scope_cap_exceeded"}})

	matching := f.seedDecision(t, f.runA, &f.implStg, "acme/widgets", "implement", "approve",
		"prior implement approval", []string{"backend/internal/server/foo.go"}, []string{"scope_cap_exceeded"})

	rec := precedentGET(t, f.s, "?decision_class=plan_approval&run_id="+f.runA.String()+
		"&stage_id="+f.implStg.String())
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (body %s)", rec.Code, rec.Body.String())
	}
	resp := decodePrecedent(t, rec)
	rc := resp.ResolvedContext
	if rc.Repo != "acme/widgets" {
		t.Errorf("derived repo = %q, want acme/widgets", rc.Repo)
	}
	if rc.StageKind != "implement" {
		t.Errorf("derived stage kind = %q, want implement", rc.StageKind)
	}
	if len(rc.TouchedPaths) != 2 ||
		rc.TouchedPaths[0] != "backend/internal/server/bar.go" ||
		rc.TouchedPaths[1] != "backend/internal/server/foo.go" {
		t.Errorf("derived paths = %v, want the seeded plan's sorted scope.files", rc.TouchedPaths)
	}
	if len(rc.EscalationKeys) != 1 || rc.EscalationKeys[0] != "scope_cap_exceeded" {
		t.Errorf("derived keys = %v, want ONLY the SAME stage's fired keys", rc.EscalationKeys)
	}
	if rc.RunID != f.runA.String() || rc.StageID != f.implStg.String() {
		t.Errorf("echoed reference = %s/%s, want %s/%s", rc.RunID, rc.StageID, f.runA, f.implStg)
	}
	if len(resp.Results) != 1 || resp.Results[0].SourceSequence != matching {
		t.Fatalf("results = %+v, want the one implement-stage decision %d", resp.Results, matching)
	}
	if resp.Results[0].Score.TouchedPaths <= 0 || resp.Results[0].Score.EscalationKeys <= 0 {
		t.Errorf("the DERIVED keys were not used for ranking: %+v", resp.Results[0].Score)
	}
}

// TestPrecedentPG_GateReferenceForeignAccountIs404 is the #3731 binding-condition-1
// control END TO END, through the real account-narrowed SQL: runC belongs to
// account B, and the caller is in account A.
//
// COUNTERFACTUAL: delete the account arm from decisionindex's gateContextSQL
// WHERE clause → the run resolves, the response is 200 and carries
// acme/widgets in resolved_context, RED on both assertions.
func TestPrecedentPG_GateReferenceForeignAccountIs404(t *testing.T) {
	f := newPrecedentPGFixture(t)
	f.seedPlanArtifact(t, f.planStage, "backend/internal/server/foo.go")

	rec := precedentGET(t, f.s, "?decision_class=plan_approval&run_id="+f.runC.String())
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for a run in ANOTHER account (body %s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, leak := range []string{"acme/widgets", "resolved_context", "touched_paths", "escalation_keys"} {
		if strings.Contains(body, leak) {
			t.Errorf("the 404 body leaks %q: %s", leak, body)
		}
	}

	// Same 404 for a stage that exists but is NOT on the named run.
	otherRunStage := uuid.New()
	f.seedStage(t, otherRunStage, f.runB, 0, "plan")
	rec = precedentGET(t, f.s, "?decision_class=plan_approval&run_id="+f.runA.String()+
		"&stage_id="+otherRunStage.String())
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for a stage not on the run (body %s)", rec.Code, rec.Body.String())
	}

	// Positive control: the caller's OWN run resolves, so the two 404s above are
	// the predicates biting rather than a broken fixture.
	if ok := precedentGET(t, f.s, "?decision_class=plan_approval&run_id="+f.runA.String()); ok.Code != http.StatusOK {
		t.Errorf("the caller's own run = %d, want 200 (body %s)", ok.Code, ok.Body.String())
	}
}
