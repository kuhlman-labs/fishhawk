package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/decisionindex"
	"github.com/kuhlman-labs/fishhawk/backend/internal/precedent"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// gate_divergence_test.go pins the E75.5 / #3733 detection hook without
// Postgres: the shipped-disabled default on every allow-listed class, the
// call sites (a real waive, bulk waive and defer through their handlers),
// ask-once-per-gate, and one test per named fail-open mode of
// noteGateDivergence. The cross-boundary proof (real index, real chain, a real
// plan reject through HTTP) is gate_divergence_pg_test.go.

const dvDoctrine = "sha-doctrine-1"

// dvEnabled is a firing configuration: 5 human decisions at 80% agreement
// inside 90 days.
func dvEnabled() *precedent.DivergenceConfig {
	return &precedent.DivergenceConfig{Enabled: true, MinDecisions: 5, MinAgreement: 0.8, Window: 90 * 24 * time.Hour}
}

// dvRows builds n unanimous HUMAN index rows of class with outcome, decided
// within the last n days under dvDoctrine.
func dvRows(n int, class decisionindex.DecisionClass, stageKind, outcome string) []decisionindex.Row {
	out := make([]decisionindex.Row, 0, n)
	now := time.Now().UTC()
	for i := 0; i < n; i++ {
		out = append(out, decisionindex.Row{
			SourceSequence: int64(1000 + i), SourceEntryHash: "hash", RunID: uuid.New(),
			Repo: gpRepo, DecisionClass: class, StageKind: stageKind, Outcome: outcome,
			ConcernCategory: "scope", Severity: "medium",
			TouchedPaths: []string{"a/b.go"}, EscalationKeys: []string{},
			DoctrineVersion: dvDoctrine, DecidedAt: now.Add(-time.Duration(i+1) * time.Hour),
		})
	}
	return out
}

func dvIndex(rows []decisionindex.Row) *fakePrecedentIndex {
	return &fakePrecedentIndex{rows: rows, gate: decisionindex.GateContext{
		Repo: gpRepo, DoctrineVersion: dvDoctrine, TouchedPaths: []string{"a/c.go"},
	}}
}

// dvEntries decodes every precedent_divergence entry appended to au.
func dvEntries(t *testing.T, au *auditFake) []precedentDivergencePayload {
	t.Helper()
	au.mu.Lock()
	defer au.mu.Unlock()
	var out []precedentDivergencePayload
	for _, e := range au.appended {
		if e.Category != CategoryPrecedentDivergence {
			continue
		}
		var p precedentDivergencePayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("decode precedent_divergence payload: %v", err)
		}
		out = append(out, p)
	}
	return out
}

// dvWaiveServer wires a real waive path (concern store + audit) plus the run,
// the precedent index and cfg.
func dvWaiveServer(t *testing.T, idx PrecedentIndex, cfg *precedent.DivergenceConfig) (*Server, *auditFake, *fakeConcernRepo, *approvalRunRepo) {
	t.Helper()
	au := newAuditFake()
	cr := newFakeConcernRepo()
	runs := newApprovalRunRepo()
	s := New(Config{
		Addr: "127.0.0.1:0", AuditRepo: au, ConcernRepo: cr, RunRepo: runs,
		PrecedentIndex: idx, DivergenceConfig: cfg,
	})
	return s, au, cr, runs
}

func dvSeedRun(runs *approvalRunRepo) uuid.UUID {
	id := uuid.New()
	runs.seedRun(&run.Run{ID: id, Repo: gpRepo, State: run.StateRunning})
	return id
}

// dvHook builds a server around a gpAudit for direct hook calls.
func dvHook(idx PrecedentIndex, cfg *precedent.DivergenceConfig) (*Server, *gpAudit, *approvalRunRepo) {
	au := &gpAudit{}
	runs := newApprovalRunRepo()
	s := New(Config{Addr: "127.0.0.1:0", AuditRepo: au, RunRepo: runs, PrecedentIndex: idx, DivergenceConfig: cfg})
	return s, au, runs
}

func (a *gpAudit) divergences() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	for _, e := range a.entries {
		if e.Category == CategoryPrecedentDivergence {
			n++
		}
	}
	return n
}

// dvDeferDecision is a concern_defer decision against a unanimous "waived"
// precedent set — a genuine divergence under dvEnabled.
func dvDeferDecision(runID uuid.UUID) gateDivergenceDecision {
	return gateDivergenceDecision{
		RunID: runID, StageID: uuid.New(), Class: decisionindex.ClassConcernDefer, Outcome: "deferred",
		StageKind: concern.StageKindImplement, ConcernCategory: "scope", Severity: "medium",
	}
}

func dvWaivedPrecedent() []decisionindex.Row {
	return dvRows(6, decisionindex.ClassConcernWaive, concern.StageKindImplement, "waived")
}

// TestDivergence_DefaultConfigRecordsNothing is the DONE-MEANS test for the
// shipped default (approval condition 3): with the zero server.Config (nil
// DivergenceConfig) AND with DefaultDivergenceConfig(), a genuinely diverging
// decision on EVERY allow-listed class — a real waive through its handler, a
// concern defer, and a plan reject — records ZERO precedent_divergence
// entries and does not even consult the index. The divergence question is
// derived from those entries (a sibling surface), so zero entries is no
// question.
func TestDivergence_DefaultConfigRecordsNothing(t *testing.T) {
	def := precedent.DefaultDivergenceConfig()
	for name, cfg := range map[string]*precedent.DivergenceConfig{"nil config": nil, "DefaultDivergenceConfig": &def} {
		t.Run(name, func(t *testing.T) {
			// concern_waive, through the real handler, against unanimous defers.
			idx := dvIndex(dvRows(6, decisionindex.ClassConcernDefer, concern.StageKindImplement, "deferred"))
			s, au, cr, runs := dvWaiveServer(t, idx, cfg)
			runID := dvSeedRun(runs)
			row := seedConcernRow(t, cr, runID, uuid.New(), concern.StageKindImplement, 1, "n")
			if w := postWaive(t, s, row.ID.String(), waiveConcernRequest{Reason: "accepted risk"}); w.Code != http.StatusOK {
				t.Fatalf("waive status = %d: %s", w.Code, w.Body.String())
			}
			if n := len(dvEntries(t, au)); n != 0 {
				t.Fatalf("waive: precedent_divergence entries = %d, want 0 under the shipped default", n)
			}

			// concern_defer and plan_approval(reject), each against unanimous
			// contrary precedent.
			hs, hau, hruns := dvHook(dvIndex(append(dvWaivedPrecedent(),
				dvRows(6, decisionindex.ClassPlanApproval, "plan", "approve")...)), cfg)
			rid := dvSeedRun(hruns)
			hs.noteGateDivergence(context.Background(), dvDeferDecision(rid))
			hs.noteGateDivergence(context.Background(), gateDivergenceDecision{
				RunID: rid, StageID: uuid.New(), Class: decisionindex.ClassPlanApproval,
				Outcome: precedent.OutcomeReject, StageKind: "plan",
			})
			if n := hau.divergences(); n != 0 {
				t.Fatalf("defer/reject: precedent_divergence entries = %d, want 0 under the shipped default", n)
			}
			if idx.filterCalls != 0 || hs.cfg.PrecedentIndex.(*fakePrecedentIndex).filterCalls != 0 {
				t.Fatal("the precedent index was queried under the shipped default; want nothing evaluated")
			}
		})
	}
}

// TestDivergence_WaiveAgainstClearPrecedentRecords drives a real waive against
// a unanimous defer precedent with divergence enabled and pins the payload.
func TestDivergence_WaiveAgainstClearPrecedentRecords(t *testing.T) {
	idx := dvIndex(dvRows(6, decisionindex.ClassConcernDefer, concern.StageKindImplement, "deferred"))
	s, au, cr, runs := dvWaiveServer(t, idx, dvEnabled())
	runID := dvSeedRun(runs)
	stageID := uuid.New()
	row := seedConcernRow(t, cr, runID, stageID, concern.StageKindImplement, 1, "n")
	if w := postWaive(t, s, row.ID.String(), waiveConcernRequest{Reason: "accepted risk"}); w.Code != http.StatusOK {
		t.Fatalf("waive status = %d: %s", w.Code, w.Body.String())
	}
	got := dvEntries(t, au)
	if len(got) != 1 {
		t.Fatalf("precedent_divergence entries = %d, want 1", len(got))
	}
	p := got[0]
	if p.DecisionClass != "concern_waive" || p.Outcome != "waived" || p.ModalOutcome != "deferred" ||
		p.StageID != stageID.String() || p.AgreementRatio != 1 || p.HumanCount != 6 ||
		p.Threshold.DoctrineVersion != dvDoctrine || p.Threshold.MinDecisions != 5 ||
		p.IndexVersion != precedent.IndexVersion || p.CitedTotal != 6 || len(p.Cited) != 6 {
		t.Fatalf("payload = %+v", p)
	}
	if got, err := cr.GetByIDs(context.Background(), []uuid.UUID{row.ID}); err != nil || got[0].State != concern.StateWaived {
		t.Fatalf("concern not waived: %v %v", got, err)
	}
}

// TestDivergence_PayloadCarriesNoReasonProse pins ADR-082 rule 1 on the new
// entry: citations only.
func TestDivergence_PayloadCarriesNoReasonProse(t *testing.T) {
	s, au, runs := dvHook(dvIndex(dvWaivedPrecedent()), dvEnabled())
	s.noteGateDivergence(context.Background(), dvDeferDecision(dvSeedRun(runs)))
	if au.divergences() != 1 {
		t.Fatalf("divergences = %d, want 1", au.divergences())
	}
	var raw map[string]any
	_ = json.Unmarshal(au.entries[0].Payload, &raw)
	for _, c := range raw["cited"].([]any) {
		if _, has := c.(map[string]any)["reason_excerpt"]; has {
			t.Fatal("a citation carries reason_excerpt; the entry must cite by sequence + hash only")
		}
	}
}

// TestDivergence_SecondDecisionOnSameGateDoesNotReAsk: two waives on one gate
// stage record EXACTLY ONE entry, read from the chain after both calls.
func TestDivergence_SecondDecisionOnSameGateDoesNotReAsk(t *testing.T) {
	idx := dvIndex(dvRows(6, decisionindex.ClassConcernDefer, concern.StageKindImplement, "deferred"))
	s, au, cr, runs := dvWaiveServer(t, idx, dvEnabled())
	runID := dvSeedRun(runs)
	stageID := uuid.New()
	for i := int64(1); i <= 2; i++ {
		row := seedConcernRow(t, cr, runID, stageID, concern.StageKindImplement, i, "n")
		if w := postWaive(t, s, row.ID.String(), waiveConcernRequest{Reason: "accepted risk"}); w.Code != http.StatusOK {
			t.Fatalf("waive %d status = %d: %s", i, w.Code, w.Body.String())
		}
	}
	if n := len(dvEntries(t, au)); n != 1 {
		t.Fatalf("precedent_divergence entries = %d, want exactly 1 per gate", n)
	}
}

// TestDivergence_BulkWaiveAsksOnce: the bulk verb inherits the hook through
// applyConcernWaive, and N waived concerns on one gate ask once.
func TestDivergence_BulkWaiveAsksOnce(t *testing.T) {
	idx := dvIndex(dvRows(6, decisionindex.ClassConcernDefer, concern.StageKindImplement, "deferred"))
	s, au, cr, runs := dvWaiveServer(t, idx, dvEnabled())
	runID := dvSeedRun(runs)
	stageID := uuid.New()
	var ids []string
	for i := int64(1); i <= 3; i++ {
		ids = append(ids, seedConcernRow(t, cr, runID, stageID, concern.StageKindImplement, i, "n").ID.String())
	}
	if w := postBulkWaive(t, s, runID.String(), bulkWaiveRequest{ConcernIDs: ids, Reason: "r"}); w.Code != http.StatusOK {
		t.Fatalf("bulk waive status = %d: %s", w.Code, w.Body.String())
	}
	if n := len(dvEntries(t, au)); n != 1 {
		t.Fatalf("precedent_divergence entries = %d, want 1 for a 3-concern bulk waive", n)
	}
}

// TestDivergence_DeferThroughHandlerRecords pins the defer call site: a real
// defer against a unanimous waive precedent records one entry.
func TestDivergence_DeferThroughHandlerRecords(t *testing.T) {
	s, repo, au, cr, _ := deferServer(t)
	s.cfg.PrecedentIndex = dvIndex(dvWaivedPrecedent())
	s.cfg.DivergenceConfig = dvEnabled()
	runID, stageID := uuid.New(), uuid.New()
	seedDeferRun(repo, runID)
	row := seedConcernRow(t, cr, runID, stageID, concern.StageKindImplement, 1, "a follow-up")
	if w := postDefer(t, s, row.ID.String(), deferConcernRequest{ParentEpic: "#389", N: "3"}, withAuth); w.Code != http.StatusOK {
		t.Fatalf("defer status = %d: %s", w.Code, w.Body.String())
	}
	got := dvEntries(t, au)
	if len(got) != 1 || got[0].DecisionClass != "concern_defer" || got[0].ModalOutcome != "waived" {
		t.Fatalf("entries = %+v, want one concern_defer divergence against waived", got)
	}
}

// TestDivergence_AppendFailureLeavesWaiveIntact: the AppendChained failure
// mode through the real handler — the waive still succeeds and no entry lands.
func TestDivergence_AppendFailureLeavesWaiveIntact(t *testing.T) {
	idx := dvIndex(dvRows(6, decisionindex.ClassConcernDefer, concern.StageKindImplement, "deferred"))
	s, au, cr, runs := dvWaiveServer(t, idx, dvEnabled())
	au.appendErrCategory = CategoryPrecedentDivergence
	runID := dvSeedRun(runs)
	row := seedConcernRow(t, cr, runID, uuid.New(), concern.StageKindImplement, 1, "n")
	w := postWaive(t, s, row.ID.String(), waiveConcernRequest{Reason: "accepted risk"})
	if w.Code != http.StatusOK {
		t.Fatalf("waive status = %d, want 200 despite the divergence append failure: %s", w.Code, w.Body.String())
	}
	if n := len(dvEntries(t, au)); n != 0 {
		t.Fatalf("entries = %d, want 0", n)
	}
	if got, _ := cr.GetByIDs(context.Background(), []uuid.UUID{row.ID}); got[0].State != concern.StateWaived {
		t.Fatalf("concern state = %q, want waived", got[0].State)
	}
}

// TestDivergence_FailOpenModes: one case per named fail-open mode of
// noteGateDivergence. Each starts from a decision that WOULD diverge and
// breaks exactly one thing; each must record nothing (and not panic).
func TestDivergence_FailOpenModes(t *testing.T) {
	type tcase struct {
		setup func(s *Server, au *gpAudit, runs *approvalRunRepo, idx *fakePrecedentIndex, d *gateDivergenceDecision)
	}
	cases := map[string]tcase{
		"nil PrecedentIndex": {func(s *Server, _ *gpAudit, _ *approvalRunRepo, _ *fakePrecedentIndex, _ *gateDivergenceDecision) {
			s.cfg.PrecedentIndex = nil
		}},
		"nil RunRepo": {func(s *Server, _ *gpAudit, _ *approvalRunRepo, _ *fakePrecedentIndex, _ *gateDivergenceDecision) {
			s.cfg.RunRepo = nil
		}},
		"run read fails": {func(_ *Server, _ *gpAudit, _ *approvalRunRepo, _ *fakePrecedentIndex, d *gateDivergenceDecision) {
			d.RunID = uuid.New() // not seeded: GetRun -> ErrNotFound
		}},
		"non-UUID run account id": {func(_ *Server, _ *gpAudit, runs *approvalRunRepo, _ *fakePrecedentIndex, d *gateDivergenceDecision) {
			runs.runs[d.RunID].AccountID = "not-a-uuid"
		}},
		"GateContext error": {func(_ *Server, _ *gpAudit, _ *approvalRunRepo, idx *fakePrecedentIndex, _ *gateDivergenceDecision) {
			idx.gateErr = errors.New("boom")
		}},
		"index List error": {func(_ *Server, _ *gpAudit, _ *approvalRunRepo, idx *fakePrecedentIndex, _ *gateDivergenceDecision) {
			idx.listErr = errors.New("boom")
		}},
		"zero candidates": {func(_ *Server, _ *gpAudit, _ *approvalRunRepo, idx *fakePrecedentIndex, _ *gateDivergenceDecision) {
			idx.rows = nil
		}},
		"allow-list miss (merge_verdict)": {func(_ *Server, _ *gpAudit, _ *approvalRunRepo, _ *fakePrecedentIndex, d *gateDivergenceDecision) {
			d.Class = decisionindex.ClassMergeVerdict
		}},
		"allow-list miss (plan approve)": {func(_ *Server, _ *gpAudit, _ *approvalRunRepo, idx *fakePrecedentIndex, d *gateDivergenceDecision) {
			idx.rows = dvRows(6, decisionindex.ClassPlanApproval, "plan", "reject")
			d.Class, d.Outcome, d.StageKind = decisionindex.ClassPlanApproval, "approve", "plan"
		}},
		"config narrows out the class": {func(s *Server, _ *gpAudit, _ *approvalRunRepo, _ *fakePrecedentIndex, _ *gateDivergenceDecision) {
			c := dvEnabled()
			c.AllowedClasses = []string{"concern_waive"}
			s.cfg.DivergenceConfig = c
		}},
		"below threshold": {func(_ *Server, _ *gpAudit, _ *approvalRunRepo, idx *fakePrecedentIndex, _ *gateDivergenceDecision) {
			idx.rows = idx.rows[:4]
		}},
		"agreed with precedent": {func(_ *Server, _ *gpAudit, _ *approvalRunRepo, idx *fakePrecedentIndex, _ *gateDivergenceDecision) {
			idx.rows = dvRows(6, decisionindex.ClassConcernDefer, concern.StageKindImplement, "deferred")
		}},
		"de-duplication read fails": {func(_ *Server, au *gpAudit, _ *approvalRunRepo, _ *fakePrecedentIndex, _ *gateDivergenceDecision) {
			au.listErr = errors.New("boom")
		}},
		"append fails": {func(_ *Server, au *gpAudit, _ *approvalRunRepo, _ *fakePrecedentIndex, _ *gateDivergenceDecision) {
			au.appendErr = errors.New("boom")
		}},
	}
	// Control: the unbroken fixture DOES record, so every case below is a
	// one-thing-broken variant of a firing decision.
	{
		idx := dvIndex(dvWaivedPrecedent())
		s, au, runs := dvHook(idx, dvEnabled())
		s.noteGateDivergence(context.Background(), dvDeferDecision(dvSeedRun(runs)))
		if au.divergences() != 1 {
			t.Fatalf("control: divergences = %d, want 1", au.divergences())
		}
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			idx := dvIndex(dvWaivedPrecedent())
			s, au, runs := dvHook(idx, dvEnabled())
			d := dvDeferDecision(dvSeedRun(runs))
			tc.setup(s, au, runs, idx, &d)
			s.noteGateDivergence(context.Background(), d)
			if n := au.divergences(); n != 0 {
				t.Fatalf("divergences = %d, want 0", n)
			}
			// An allow-list miss is decided before any read: the hook's
			// pre-check, not Decide's, is what keeps it off the index.
			if strings.HasPrefix(name, "allow-list miss") || name == "config narrows out the class" {
				if idx.filterCalls != 0 {
					t.Fatalf("index queried %d times on an allow-list miss; want 0", idx.filterCalls)
				}
			}
		})
	}
	t.Run("nil AuditRepo", func(t *testing.T) {
		idx := dvIndex(dvWaivedPrecedent())
		s, _, runs := dvHook(idx, dvEnabled())
		d := dvDeferDecision(dvSeedRun(runs))
		s.cfg.AuditRepo = nil
		s.noteGateDivergence(context.Background(), d) // must not panic
		if idx.filterCalls != 0 {
			t.Fatalf("index queried %d times with no audit repository; want the hook to stop first", idx.filterCalls)
		}
	})
}

// TestDivergence_DelegatedPriorDecisionsExcludedEndToEnd is the server-side
// twin of the pure delegated test: N-1 human + 2 delegated contrary rows
// reach the rule un-filtered, and only the rule's human filter keeps them
// from counting.
func TestDivergence_DelegatedPriorDecisionsExcludedEndToEnd(t *testing.T) {
	rows := dvRows(6, decisionindex.ClassConcernWaive, concern.StageKindImplement, "waived")
	rows[0].Delegated = true
	rows[1].Delegated = true
	s, au, runs := dvHook(dvIndex(rows), dvEnabled())
	s.noteGateDivergence(context.Background(), dvDeferDecision(dvSeedRun(runs)))
	if n := au.divergences(); n != 0 {
		t.Fatalf("divergences = %d, want 0 (4 human < N=5)", n)
	}
}

// TestDivergence_DecisionIsNotItsOwnPrecedent: the just-recorded decision's
// own index row (same run, same source sequence) is excluded. The fixture is
// 4 contrary rows plus the decision's own row carrying the same contrary
// outcome, so ONLY the exclusion keeps the count below N=5.
func TestDivergence_DecisionIsNotItsOwnPrecedent(t *testing.T) {
	rows := dvRows(5, decisionindex.ClassConcernWaive, concern.StageKindImplement, "waived")
	idx := dvIndex(rows)
	s, au, runs := dvHook(idx, dvEnabled())
	d := dvDeferDecision(dvSeedRun(runs))
	rows[0].RunID = d.RunID
	d.DecisionSequence = rows[0].SourceSequence
	s.noteGateDivergence(context.Background(), d)
	if n := au.divergences(); n != 0 {
		t.Fatalf("divergences = %d, want 0: the decision counted itself as precedent", n)
	}
}
