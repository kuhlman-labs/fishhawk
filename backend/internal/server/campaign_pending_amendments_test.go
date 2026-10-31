package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"gopkg.in/yaml.v3"

	"github.com/kuhlman-labs/fishhawk/backend/internal/campaign"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/scopeamendment"
)

// Tests for the campaign-wide pending scope-amendment read (E86.85 / #4486).
// Every fixture uses acme/* repos (ADR-110).

// pendingAmendRunRepo wraps the shared orchestratorRepo fake: its ListRuns
// ALSO honours ParentRunID (the shared fake ignores it) and REJECTS Limit <= 0
// exactly as the Postgres adapter does, so the fake pins approval condition C2.
// listErr / stageErr / getRunErr drive the 500 legs; amendListRuns counts the
// ListRuns calls.
type pendingAmendRunRepo struct {
	*orchestratorRepo
	listMu    sync.Mutex
	listCalls int
	listErr   error
	stageErr  error
	getRunErr error
}

func (r *pendingAmendRunRepo) ListRuns(_ context.Context, f run.ListRunsFilter) ([]*run.Run, error) {
	r.listMu.Lock()
	r.listCalls++
	r.listMu.Unlock()
	if f.Limit <= 0 {
		return nil, fmt.Errorf("list runs: limit must be > 0")
	}
	if r.listErr != nil {
		return nil, r.listErr
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*run.Run
	for _, rr := range r.runs {
		if f.DecomposedFrom != nil && (rr.DecomposedFrom == nil || *rr.DecomposedFrom != *f.DecomposedFrom) {
			continue
		}
		if f.ParentRunID != nil && (rr.ParentRunID == nil || *rr.ParentRunID != *f.ParentRunID) {
			continue
		}
		out = append(out, cloneRunLocked(rr))
	}
	if len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return out, nil
}

func (r *pendingAmendRunRepo) GetStage(ctx context.Context, id uuid.UUID) (*run.Stage, error) {
	if r.stageErr != nil {
		return nil, r.stageErr
	}
	return r.orchestratorRepo.GetStage(ctx, id)
}

func (r *pendingAmendRunRepo) GetRun(ctx context.Context, id uuid.UUID) (*run.Run, error) {
	if r.getRunErr != nil {
		return nil, r.getRunErr
	}
	return r.orchestratorRepo.GetRun(ctx, id)
}

// pendingAmendAmendRepo wraps the amendment fake with a ListByRun error knob
// and a per-run read counter.
type pendingAmendAmendRepo struct {
	*fakeScopeAmendmentRepo
	countMu sync.Mutex
	reads   int
	listErr error
}

func (a *pendingAmendAmendRepo) ListByRun(ctx context.Context, runID uuid.UUID) ([]*scopeamendment.Amendment, error) {
	a.countMu.Lock()
	a.reads++
	a.countMu.Unlock()
	if a.listErr != nil {
		return nil, a.listErr
	}
	return a.fakeScopeAmendmentRepo.ListByRun(ctx, runID)
}

type pendingAmendFixture struct {
	s   *Server
	rr  *pendingAmendRunRepo
	cr  *holdCampaignRepo
	ar  *pendingAmendAmendRepo
	c1  *campaign.Campaign
	c2  *campaign.Campaign
	a   *run.Run
	a0  *run.Run
	b   *run.Run
	b0  *run.Run
	x   *run.Run
	amB *scopeamendment.Amendment
	amX *scopeamendment.Amendment
}

func newPendingAmendServer() (*Server, *pendingAmendRunRepo, *holdCampaignRepo, *pendingAmendAmendRepo) {
	rr := &pendingAmendRunRepo{orchestratorRepo: newOrchestratorRepo()}
	cr := &holdCampaignRepo{fakeCampaignRepo: newFakeCampaignRepo()}
	ar := &pendingAmendAmendRepo{fakeScopeAmendmentRepo: newFakeScopeAmendmentRepo()}
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, CampaignRepo: cr, ScopeAmendmentRepo: ar})
	return s, rr, cr, ar
}

// seedPARun seeds a running acme run with the given lineage links.
func seedPARun(rr *pendingAmendRunRepo, repo string, decomposedFrom, parent *uuid.UUID) *run.Run {
	r := rr.seedRun()
	r.Repo = repo
	r.DecomposedFrom = decomposedFrom
	r.ParentRunID = parent
	return r
}

// seedPAImplement seeds an implement stage in the given state.
func seedPAImplement(rr *pendingAmendRunRepo, runID uuid.UUID, state run.StageState) *run.Stage {
	st := rr.seedStage(runID, 1, state)
	st.Type = run.StageTypeImplement
	return st
}

func seedPAAmendment(t *testing.T, ar *pendingAmendAmendRepo, runID, stageID uuid.UUID, path string) *scopeamendment.Amendment {
	t.Helper()
	a, err := ar.Create(context.Background(), scopeamendment.CreateParams{
		RunID: runID, StageID: stageID,
		Paths:  []scopeamendment.PathEntry{{Path: path, Operation: scopeamendment.OperationModify}},
		Reason: "needs " + path,
	})
	if err != nil {
		t.Fatalf("seed amendment: %v", err)
	}
	return a
}

func idp(id uuid.UUID) *uuid.UUID { return &id }

// newPendingAmendFixture builds fixture F: campaign C1 (acme/widgets) with
// item #1 → run A (slice A0, running implement, no amendment) and item #2 →
// run B (slice B0, decomposed_from only, running implement, one PENDING
// amendment); campaign C2
// (acme/gadgets) whose item run X carries its own pending amendment.
func newPendingAmendFixture(t *testing.T) *pendingAmendFixture {
	t.Helper()
	s, rr, cr, ar := newPendingAmendServer()
	f := &pendingAmendFixture{s: s, rr: rr, cr: cr, ar: ar}
	f.a = seedPARun(rr, "acme/widgets", nil, nil)
	f.a0 = seedPARun(rr, "acme/widgets", idp(f.a.ID), idp(f.a.ID))
	seedPAImplement(rr, f.a0.ID, run.StageStateRunning)
	f.b = seedPARun(rr, "acme/widgets", nil, nil)
	// B0 carries ONLY decomposed_from, so the decomposed half of the lineage
	// union is isolated (the recovery-child test isolates the parent half).
	f.b0 = seedPARun(rr, "acme/widgets", idp(f.b.ID), nil)
	stB0 := seedPAImplement(rr, f.b0.ID, run.StageStateRunning)
	f.amB = seedPAAmendment(t, ar, f.b0.ID, stB0.ID, "internal/widgets/b.go")
	f.c1 = cr.seedCampaignWithItems("acme/widgets", "issue:10", []*campaign.Item{
		{IssueRef: "issue:1", State: campaign.ItemStateRunning, RunID: idp(f.a.ID), Position: 0},
		{IssueRef: "issue:2", State: campaign.ItemStateRunning, RunID: idp(f.b.ID), Position: 1},
	})
	f.x = seedPARun(rr, "acme/gadgets", nil, nil)
	stX := seedPAImplement(rr, f.x.ID, run.StageStateRunning)
	f.amX = seedPAAmendment(t, ar, f.x.ID, stX.ID, "internal/gadgets/x.go")
	f.c2 = cr.seedCampaignWithItems("acme/gadgets", "issue:70", []*campaign.Item{
		{IssueRef: "issue:7", State: campaign.ItemStateRunning, RunID: idp(f.x.ID)},
	})
	return f
}

func callCampaignPendingAmendments(s *Server, runIDText string, id Identity) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v0/runs/"+runIDText+"/campaign-pending-amendments", nil)
	req.SetPathValue("run_id", runIDText)
	s.handleGetRunCampaignPendingAmendments(w, injectIdentity(req, id))
	return w
}

func callCampaignStatusAs(s *Server, campaignID uuid.UUID, id Identity) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v0/campaigns/"+campaignID.String()+"/status", nil)
	req.SetPathValue("campaign_id", campaignID.String())
	s.handleGetCampaignStatus(w, injectIdentity(req, id))
	return w
}

func decodePendingAmendments(t *testing.T, w *httptest.ResponseRecorder) runCampaignPendingAmendmentsResponse {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var got runCampaignPendingAmendmentsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v\n%s", err, w.Body.String())
	}
	return got
}

// assertOnlyB0Amendment asserts the body lists exactly B0's amendment with its
// campaign item attribution and a live decision.
func assertOnlyB0Amendment(t *testing.T, f *pendingAmendFixture, got runCampaignPendingAmendmentsResponse) {
	t.Helper()
	if got.CampaignID == nil || *got.CampaignID != f.c1.ID.String() {
		t.Fatalf("campaign_id = %v, want %s", got.CampaignID, f.c1.ID)
	}
	if got.Truncated {
		t.Fatal("truncated = true, want false")
	}
	if len(got.Items) != 1 {
		t.Fatalf("items = %+v, want exactly B0's amendment", got.Items)
	}
	e := got.Items[0]
	if e.Amendment.ID != f.amB.ID || e.Amendment.RunID != f.b0.ID {
		t.Fatalf("amendment = %+v, want %s on run %s", e.Amendment, f.amB.ID, f.b0.ID)
	}
	if e.IssueRef != "issue:2" || e.ItemRunID != f.b.ID.String() {
		t.Fatalf("attribution = %q/%q, want issue:2/%s", e.IssueRef, e.ItemRunID, f.b.ID)
	}
	if e.StageState != string(run.StageStateRunning) || !e.DecisionLive {
		t.Fatalf("stage_state=%q decision_live=%v, want running/true", e.StageState, e.DecisionLive)
	}
	if len(e.Amendment.Paths) != 1 || e.Amendment.Paths[0].Path != "internal/widgets/b.go" || e.Amendment.Reason != "needs internal/widgets/b.go" {
		t.Fatalf("paths/reason = %+v / %q", e.Amendment.Paths, e.Amendment.Reason)
	}
	if e.Amendment.Status != string(scopeamendment.StatusPending) {
		t.Fatalf("status = %q, want pending", e.Amendment.Status)
	}
}

func TestCampaignPendingAmendments_ListsOtherItemsSliceAmendment(t *testing.T) {
	f := newPendingAmendFixture(t)
	assertOnlyB0Amendment(t, f, decodePendingAmendments(t, callCampaignPendingAmendments(f.s, f.a.ID.String(), gateViewReadIdentity())))
}

func TestCampaignPendingAmendments_AnchoredOnSliceResolvesCampaign(t *testing.T) {
	f := newPendingAmendFixture(t)
	assertOnlyB0Amendment(t, f, decodePendingAmendments(t, callCampaignPendingAmendments(f.s, f.a0.ID.String(), gateViewReadIdentity())))
}

// A session caller (no token) clears requireWriteScope on the route and
// campaignAmendmentsReadable on the status block: the same outcome on both
// surfaces (approval condition C4).
func TestCampaignPendingAmendments_SessionCallerSameOutcomeOnBothSurfaces(t *testing.T) {
	f := newPendingAmendFixture(t)
	session := testOperatorIdentity()
	assertOnlyB0Amendment(t, f, decodePendingAmendments(t, callCampaignPendingAmendments(f.s, f.a.ID.String(), session)))
	block := decodeStatusPendingBlock(t, callCampaignStatusAs(f.s, f.c1.ID, session))
	if block == nil || len(block.Items) != 1 || block.Items[0].Amendment.ID != f.amB.ID {
		t.Fatalf("status pending_amendments = %+v, want B0's amendment", block)
	}
}

func TestCampaignPendingAmendments_IncludesRecoveryChild(t *testing.T) {
	s, rr, cr, ar := newPendingAmendServer()
	parent := seedPARun(rr, "acme/widgets", nil, nil)
	parent.State = run.StateFailed
	// The recovery child carries ONLY parent_run_id (no decomposed_from).
	child := seedPARun(rr, "acme/widgets", nil, idp(parent.ID))
	st := seedPAImplement(rr, child.ID, run.StageStateRunning)
	am := seedPAAmendment(t, ar, child.ID, st.ID, "internal/widgets/r.go")
	c := cr.seedCampaignWithItems("acme/widgets", "issue:10", []*campaign.Item{
		{IssueRef: "issue:3", State: campaign.ItemStateFailed, RunID: idp(parent.ID)},
	})
	got := decodePendingAmendments(t, callCampaignPendingAmendments(s, parent.ID.String(), gateViewReadIdentity()))
	if got.CampaignID == nil || *got.CampaignID != c.ID.String() {
		t.Fatalf("campaign_id = %v, want %s", got.CampaignID, c.ID)
	}
	if len(got.Items) != 1 || got.Items[0].Amendment.ID != am.ID || got.Items[0].ItemRunID != parent.ID.String() || !got.Items[0].DecisionLive {
		t.Fatalf("items = %+v, want the recovery child's live amendment", got.Items)
	}
}

// Approval condition C1: an item in ANY state — here cancelled — whose linked
// run is still running is scanned; its pending amendment is listed on the
// route and on the status block, decision_live true.
func TestCampaignPendingAmendments_CancelledItemLiveRunListed(t *testing.T) {
	s, rr, cr, ar := newPendingAmendServer()
	anchor := seedPARun(rr, "acme/widgets", nil, nil)
	cancelledRun := seedPARun(rr, "acme/widgets", nil, nil)
	st := seedPAImplement(rr, cancelledRun.ID, run.StageStateRunning)
	am := seedPAAmendment(t, ar, cancelledRun.ID, st.ID, "internal/widgets/c.go")
	c := cr.seedCampaignWithItems("acme/widgets", "issue:10", []*campaign.Item{
		{IssueRef: "issue:1", State: campaign.ItemStateRunning, RunID: idp(anchor.ID), Position: 0},
		{IssueRef: "issue:4", State: campaign.ItemStateCancelled, RunID: idp(cancelledRun.ID), Position: 1},
	})
	got := decodePendingAmendments(t, callCampaignPendingAmendments(s, anchor.ID.String(), gateViewReadIdentity()))
	if len(got.Items) != 1 || got.Items[0].Amendment.ID != am.ID || got.Items[0].IssueRef != "issue:4" || !got.Items[0].DecisionLive {
		t.Fatalf("route items = %+v, want the cancelled item's live amendment", got.Items)
	}
	block := decodeStatusPendingBlock(t, callCampaignStatusAs(s, c.ID, gateViewReadIdentity()))
	if block == nil || len(block.Items) != 1 || block.Items[0].Amendment.ID != am.ID || !block.Items[0].DecisionLive {
		t.Fatalf("status pending_amendments = %+v, want the cancelled item's live amendment", block)
	}
}

func TestCampaignPendingAmendments_FailedStageListedNotLive(t *testing.T) {
	s, rr, cr, ar := newPendingAmendServer()
	r := seedPARun(rr, "acme/widgets", nil, nil)
	st := seedPAImplement(rr, r.ID, run.StageStateFailed)
	am := seedPAAmendment(t, ar, r.ID, st.ID, "internal/widgets/f.go")
	cr.seedCampaignWithItems("acme/widgets", "issue:10", []*campaign.Item{
		{IssueRef: "issue:5", State: campaign.ItemStateRunning, RunID: idp(r.ID)},
	})
	got := decodePendingAmendments(t, callCampaignPendingAmendments(s, r.ID.String(), gateViewReadIdentity()))
	if len(got.Items) != 1 || got.Items[0].Amendment.ID != am.ID {
		t.Fatalf("items = %+v, want the failed stage's still-pending amendment listed", got.Items)
	}
	if got.Items[0].StageState != string(run.StageStateFailed) {
		t.Fatalf("stage_state = %q, want failed", got.Items[0].StageState)
	}
	if got.Items[0].DecisionLive {
		t.Fatal("decision_live = true, want false for a settled (failed) stage")
	}
}

func TestCampaignPendingAmendments_DecidedAndLapsedExcluded(t *testing.T) {
	f := newPendingAmendFixture(t)
	st := seedPAImplement(f.rr, f.a0.ID, run.StageStateRunning)
	approved := seedPAAmendment(t, f.ar, f.a0.ID, st.ID, "internal/widgets/ok.go")
	denied := seedPAAmendment(t, f.ar, f.a0.ID, st.ID, "internal/widgets/no.go")
	lapsed := seedPAAmendment(t, f.ar, f.a0.ID, st.ID, "internal/widgets/old.go")
	f.ar.mu.Lock()
	f.ar.rows[approved.ID].Status = scopeamendment.StatusApproved
	f.ar.rows[denied.ID].Status = scopeamendment.StatusDenied
	f.ar.rows[lapsed.ID].Status = scopeamendment.StatusLapsed
	f.ar.mu.Unlock()
	assertOnlyB0Amendment(t, f, decodePendingAmendments(t, callCampaignPendingAmendments(f.s, f.a.ID.String(), gateViewReadIdentity())))
}

func TestCampaignPendingAmendments_OtherCampaignExcluded(t *testing.T) {
	f := newPendingAmendFixture(t)
	for _, anchor := range []*run.Run{f.a, f.a0, f.b, f.b0} {
		w := callCampaignPendingAmendments(f.s, anchor.ID.String(), gateViewReadIdentity())
		if strings.Contains(w.Body.String(), f.amX.ID.String()) || strings.Contains(w.Body.String(), "internal/gadgets/x.go") {
			t.Fatalf("anchored on %s: C2's amendment leaked:\n%s", anchor.ID, w.Body.String())
		}
	}
	got := decodePendingAmendments(t, callCampaignPendingAmendments(f.s, f.x.ID.String(), gateViewReadIdentity()))
	if got.CampaignID == nil || *got.CampaignID != f.c2.ID.String() || len(got.Items) != 1 || got.Items[0].Amendment.ID != f.amX.ID {
		t.Fatalf("anchored on X: %+v, want only C2's amendment", got)
	}
	if strings.Contains(callCampaignPendingAmendments(f.s, f.x.ID.String(), gateViewReadIdentity()).Body.String(), f.amB.ID.String()) {
		t.Fatal("anchored on X: C1's amendment leaked")
	}
}

// Asserted on RAW bytes: absent, null and empty are three distinct claims.
func TestCampaignPendingAmendments_NotInCampaign(t *testing.T) {
	s, rr, _, _ := newPendingAmendServer()
	lone := seedPARun(rr, "acme/widgets", nil, nil)
	orphan := seedPARun(rr, "acme/widgets", idp(uuid.New()), nil) // missing parent: unresolved ancestry
	for name, r := range map[string]*run.Run{"no campaign": lone, "unresolved ancestry": orphan} {
		t.Run(name, func(t *testing.T) {
			w := callCampaignPendingAmendments(s, r.ID.String(), gateViewReadIdentity())
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
			}
			body := w.Body.String()
			for _, want := range []string{`"campaign_id":null`, `"items":[]`, `"truncated":false`, `"run_id":"` + r.ID.String() + `"`} {
				if !strings.Contains(body, want) {
					t.Fatalf("body missing %s:\n%s", want, body)
				}
			}
		})
	}
}

// The enumeration visits A0 (item #1) before B0 (item #2), so B0's OLDER
// amendment must be moved ahead of A0's newer one by the sort itself.
func TestCampaignPendingAmendments_OldestFirst(t *testing.T) {
	f := newPendingAmendFixture(t)
	st := seedPAImplement(f.rr, f.a0.ID, run.StageStateRunning)
	newer := seedPAAmendment(t, f.ar, f.a0.ID, st.ID, "internal/widgets/a.go")
	base := time.Now().UTC()
	f.ar.mu.Lock()
	f.ar.rows[f.amB.ID].RequestedAt = base.Add(-time.Hour)
	f.ar.rows[newer.ID].RequestedAt = base
	f.ar.mu.Unlock()
	got := decodePendingAmendments(t, callCampaignPendingAmendments(f.s, f.a.ID.String(), gateViewReadIdentity()))
	if len(got.Items) != 2 || got.Items[0].Amendment.ID != f.amB.ID || got.Items[1].Amendment.ID != newer.ID {
		t.Fatalf("items = %+v, want [B0's older, A0's newer]", got.Items)
	}
	if got.Items[0].IssueRef != "issue:2" || got.Items[1].IssueRef != "issue:1" {
		t.Fatalf("issue refs = %q,%q", got.Items[0].IssueRef, got.Items[1].IssueRef)
	}
}

func TestCampaignPendingAmendments_Truncated(t *testing.T) {
	s, rr, cr, ar := newPendingAmendServer()
	root := seedPARun(rr, "acme/widgets", nil, nil)
	for i := 0; i < campaignPendingAmendmentsMaxRuns+1; i++ {
		seedPARun(rr, "acme/widgets", idp(root.ID), idp(root.ID))
	}
	cr.seedCampaignWithItems("acme/widgets", "issue:10", []*campaign.Item{
		{IssueRef: "issue:6", State: campaign.ItemStateRunning, RunID: idp(root.ID)},
	})
	got := decodePendingAmendments(t, callCampaignPendingAmendments(s, root.ID.String(), gateViewReadIdentity()))
	if !got.Truncated {
		t.Fatal("truncated = false, want true for 66 runs over a 64-run cap")
	}
	if ar.reads > campaignPendingAmendmentsMaxRuns {
		t.Fatalf("amendment reads = %d, want at most %d runs read", ar.reads, campaignPendingAmendmentsMaxRuns)
	}
	if got.Items == nil {
		t.Fatal("items = null, want an array")
	}
}

func TestCampaignPendingAmendments_RunBoundTokenForbidden(t *testing.T) {
	f := newPendingAmendFixture(t)
	// Bound to the ANCHOR run itself and carrying read:audit, so neither a
	// cross-run guard nor the scope check can mask a missing run-bound refusal.
	id := Identity{Subject: "mcp:run:" + f.a.ID.String(), TokenID: "tok-run", Scopes: []string{scopeGateViewRead}}
	w := callCampaignPendingAmendments(f.s, f.a.ID.String(), id)
	if w.Code != http.StatusForbidden || !bodyHasCode(w, "run_token_forbidden") {
		t.Fatalf("status = %d body = %s, want 403 run_token_forbidden", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), f.amB.ID.String()) || strings.Contains(w.Body.String(), "internal/widgets/b.go") {
		t.Fatalf("refusal leaked another run's amendment:\n%s", w.Body.String())
	}
}

func TestCampaignPendingAmendments_MissingReadAuditScope(t *testing.T) {
	f := newPendingAmendFixture(t)
	id := Identity{Subject: "github:op", TokenID: "tok-x", Scopes: []string{"write:runs"}}
	w := callCampaignPendingAmendments(f.s, f.a.ID.String(), id)
	if w.Code != http.StatusForbidden || !bodyHasCode(w, "insufficient_scope") {
		t.Fatalf("status = %d body = %s, want 403 insufficient_scope", w.Code, w.Body.String())
	}
	if w := callCampaignPendingAmendments(f.s, f.a.ID.String(), anonIdentity()); w.Code != http.StatusUnauthorized || !bodyHasCode(w, "authentication_required") {
		t.Fatalf("anonymous: status = %d body = %s, want 401", w.Code, w.Body.String())
	}
}

func TestCampaignPendingAmendments_ErrorModes(t *testing.T) {
	op := gateViewReadIdentity()
	cases := []struct {
		name     string
		mutate   func(f *pendingAmendFixture)
		anchor   func(f *pendingAmendFixture) string
		wantCode int
		wantErr  string
	}{
		{name: "malformed run id", anchor: func(*pendingAmendFixture) string { return "nope" }, wantCode: http.StatusBadRequest, wantErr: "validation_failed"},
		{name: "unknown run", anchor: func(*pendingAmendFixture) string { return uuid.NewString() }, wantCode: http.StatusNotFound, wantErr: "run_not_found"},
		{name: "nil run repo", mutate: func(f *pendingAmendFixture) { f.s.cfg.RunRepo = nil }, wantCode: http.StatusServiceUnavailable, wantErr: "run_repo_unconfigured"},
		{name: "nil campaign repo", mutate: func(f *pendingAmendFixture) { f.s.cfg.CampaignRepo = nil }, wantCode: http.StatusServiceUnavailable, wantErr: "campaign_repo_unconfigured"},
		{name: "nil scope-amendment repo", mutate: func(f *pendingAmendFixture) { f.s.cfg.ScopeAmendmentRepo = nil }, wantCode: http.StatusServiceUnavailable, wantErr: "scope_amendment_unconfigured"},
		{name: "get run error", mutate: func(f *pendingAmendFixture) { f.rr.getRunErr = errors.New("db down") }, wantCode: http.StatusInternalServerError, wantErr: "internal_error"},
		{name: "ancestry read error", mutate: func(f *pendingAmendFixture) { f.cr.listForRunErr = errors.New("db down") }, wantCode: http.StatusInternalServerError, wantErr: "internal_error"},
		{name: "campaign items read error", mutate: func(f *pendingAmendFixture) { f.cr.itemsErr = errors.New("db down") }, wantCode: http.StatusInternalServerError, wantErr: "internal_error"},
		{name: "descendant list error", mutate: func(f *pendingAmendFixture) { f.rr.listErr = errors.New("db down") }, wantCode: http.StatusInternalServerError, wantErr: "internal_error"},
		{name: "amendment list error", mutate: func(f *pendingAmendFixture) { f.ar.listErr = errors.New("db down") }, wantCode: http.StatusInternalServerError, wantErr: "internal_error"},
		{name: "stage read error", mutate: func(f *pendingAmendFixture) { f.rr.stageErr = errors.New("db down") }, wantCode: http.StatusInternalServerError, wantErr: "internal_error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newPendingAmendFixture(t)
			if tc.mutate != nil {
				tc.mutate(f)
			}
			anchor := f.a.ID.String()
			if tc.anchor != nil {
				anchor = tc.anchor(f)
			}
			w := callCampaignPendingAmendments(f.s, anchor, op)
			if w.Code != tc.wantCode || !bodyHasCode(w, tc.wantErr) {
				t.Fatalf("status = %d body = %s, want %d %s", w.Code, w.Body.String(), tc.wantCode, tc.wantErr)
			}
			if tc.wantCode == http.StatusInternalServerError && strings.Contains(w.Body.String(), f.amB.ID.String()) {
				t.Fatalf("500 carried a partial result:\n%s", w.Body.String())
			}
		})
	}
}

// decodeStatusPendingBlock returns the status body's pending_amendments block,
// nil when the key is ABSENT (asserted on the raw key, not a zero value).
func decodeStatusPendingBlock(t *testing.T, w *httptest.ResponseRecorder) *campaignPendingAmendmentsPayload {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, key := range []string{"campaign", "items", "rollup", "next_action"} {
		if _, ok := raw[key]; !ok {
			t.Fatalf("status body missing %q:\n%s", key, w.Body.String())
		}
	}
	block, ok := raw["pending_amendments"]
	if !ok {
		return nil
	}
	var out campaignPendingAmendmentsPayload
	if err := json.Unmarshal(block, &out); err != nil {
		t.Fatalf("decode pending_amendments: %v", err)
	}
	return &out
}

func TestCampaignStatus_ListsPendingAmendments(t *testing.T) {
	f := newPendingAmendFixture(t)
	block := decodeStatusPendingBlock(t, callCampaignStatusAs(f.s, f.c1.ID, gateViewReadIdentity()))
	if block == nil {
		t.Fatal("pending_amendments absent, want the computed block")
	}
	if len(block.Items) != 1 || block.Items[0].Amendment.ID != f.amB.ID || block.Items[0].IssueRef != "issue:2" || !block.Items[0].DecisionLive || block.Truncated {
		t.Fatalf("pending_amendments = %+v, want B0's live amendment", block)
	}
	// Computed with none pending → present with items [].
	f.ar.mu.Lock()
	f.ar.rows[f.amB.ID].Status = scopeamendment.StatusApproved
	f.ar.mu.Unlock()
	w := callCampaignStatusAs(f.s, f.c1.ID, gateViewReadIdentity())
	if !strings.Contains(w.Body.String(), `"pending_amendments":{"items":[],"truncated":false}`) {
		t.Fatalf("want a present, empty block:\n%s", w.Body.String())
	}
}

func TestCampaignStatus_PendingAmendmentsOmittedForRunBoundCaller(t *testing.T) {
	f := newPendingAmendFixture(t)
	id := Identity{Subject: "mcp:run:" + f.a.ID.String(), TokenID: "tok-run", Scopes: []string{scopeGateViewRead}}
	w := callCampaignStatusAs(f.s, f.c1.ID, id)
	if block := decodeStatusPendingBlock(t, w); block != nil {
		t.Fatalf("pending_amendments = %+v, want the key absent for a run-bound caller", block)
	}
	if strings.Contains(w.Body.String(), "internal/widgets/b.go") {
		t.Fatalf("status leaked another run's amendment path:\n%s", w.Body.String())
	}
}

// Approval condition C3: a non-run-bound token WITHOUT read:audit gets the
// status (200) with the key absent.
func TestCampaignStatus_PendingAmendmentsOmittedWithoutReadAudit(t *testing.T) {
	f := newPendingAmendFixture(t)
	id := Identity{Subject: "github:op", TokenID: "tok-x", Scopes: []string{"read:runs"}}
	if block := decodeStatusPendingBlock(t, callCampaignStatusAs(f.s, f.c1.ID, id)); block != nil {
		t.Fatalf("pending_amendments = %+v, want the key absent without read:audit", block)
	}
}

func TestCampaignStatus_PendingAmendmentsOmittedOnReadError(t *testing.T) {
	f := newPendingAmendFixture(t)
	f.ar.listErr = errors.New("db down")
	w := callCampaignStatusAs(f.s, f.c1.ID, gateViewReadIdentity())
	if block := decodeStatusPendingBlock(t, w); block != nil {
		t.Fatalf("pending_amendments = %+v, want the key absent on a read failure", block)
	}
	var body struct {
		Items []campaignItemResponse `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || len(body.Items) != 2 {
		t.Fatalf("status items = %+v (err %v), want the rest of the status intact", body.Items, err)
	}
}

func TestCampaignAmendmentsReadable(t *testing.T) {
	cases := []struct {
		name string
		id   Identity
		want bool
	}{
		{"run-bound with read:audit", Identity{Subject: "mcp:run:" + uuid.NewString(), TokenID: "t", Scopes: []string{scopeGateViewRead}}, false},
		{"anonymous", anonIdentity(), false},
		{"token without read:audit", Identity{Subject: "github:op", TokenID: "t", Scopes: []string{"read:runs"}}, false},
		{"token with read:audit", gateViewReadIdentity(), true},
		{"session", testOperatorIdentity(), true},
	}
	for _, tc := range cases {
		if got := campaignAmendmentsReadable(tc.id); got != tc.want {
			t.Errorf("%s: readable = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The ListRuns contract: every descendant listing carries an explicit Limit
// (the wrapper refuses Limit <= 0 like the Postgres adapter), so the read
// succeeds and actually issued listings (approval condition C2).
func TestCampaignPendingAmendments_DescendantListingsCarryLimit(t *testing.T) {
	f := newPendingAmendFixture(t)
	decodePendingAmendments(t, callCampaignPendingAmendments(f.s, f.a.ID.String(), gateViewReadIdentity()))
	if f.rr.listCalls == 0 {
		t.Fatal("no descendant listings issued")
	}
}

func TestOpenAPI_CampaignPendingAmendmentsDocumented(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(pkgSrcDir, "..", "..", "..", "docs", "api", "v0.openapi.yaml"))
	if err != nil {
		t.Fatalf("read openapi: %v", err)
	}
	var doc struct {
		Paths map[string]map[string]struct {
			OperationID string `yaml:"operationId"`
		} `yaml:"paths"`
		Components struct {
			Schemas map[string]struct {
				Properties map[string]any `yaml:"properties"`
			} `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse openapi: %v", err)
	}
	op, ok := doc.Paths["/v0/runs/{run_id}/campaign-pending-amendments"]["get"]
	if !ok || op.OperationID != "getRunCampaignPendingAmendments" {
		t.Fatalf("route get = %+v (present %v), want operationId getRunCampaignPendingAmendments", op, ok)
	}
	for _, name := range []string{"CampaignPendingAmendment", "CampaignPendingAmendments", "RunCampaignPendingAmendments"} {
		if _, ok := doc.Components.Schemas[name]; !ok {
			t.Errorf("schema %s missing", name)
		}
	}
	if _, ok := doc.Components.Schemas["CampaignStatus"].Properties["pending_amendments"]; !ok {
		t.Error("CampaignStatus.pending_amendments missing")
	}
}
