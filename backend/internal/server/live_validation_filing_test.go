package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

const liveValParentIssue = 2045

// liveValFileProvider is a counting work-item provider for the live-validation
// filing tests: each successful File returns a distinct issue number and records
// the ProviderRequest; fail makes every File return an error (the filing-failure
// path). calls counts EVERY File invocation across a harness's lifetime so a
// re-approval that must NOT re-file is observable.
type liveValFileProvider struct {
	name    string
	mu      sync.Mutex
	reqs    []workmgmt.ProviderRequest
	calls   int
	success int
	fail    bool
}

func (p *liveValFileProvider) Name() string { return p.name }

func (p *liveValFileProvider) File(_ context.Context, req workmgmt.ProviderRequest) (*workmgmt.CreatedItem, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	if p.fail {
		return nil, errors.New("live-val provider: injected File error")
	}
	p.reqs = append(p.reqs, req)
	p.success++
	num := 4000 + p.success
	return &workmgmt.CreatedItem{
		Provider: p.name,
		Number:   num,
		URL:      fmt.Sprintf("https://github.com/o/r/issues/%d", num),
		Boarded:  true,
	}, nil
}

// newLiveValIssueGitHub returns a GitHub client whose GetIssue answers with the
// given title (so deriveEpicTitleVar can resolve {epic} from a leading [E<n>]
// token — the epic-parented branch). Every other endpoint 404s.
func newLiveValIssueGitHub(t *testing.T, title string) *githubclient.Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{repo}/issues/{number}",
		func(w http.ResponseWriter, r *http.Request) {
			num, _ := strconv.Atoi(r.PathValue("number"))
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number": num, "title": title, "body": "", "state": "open",
			})
		})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &githubclient.Client{
		BaseURL: srv.URL,
		Tokens:  &fakeTokenProvider{tok: "ghs_t"},
		HTTP:    &http.Client{Timeout: 5 * time.Second},
	}
}

type liveValConfig struct {
	marked       bool // include a requires_live_validation criterion
	installID    *int64
	github       *githubclient.Client
	providerFail bool
	// linkedAppendFail makes the auditFake fail the linked-marker append ONLY,
	// leaving the intent-marker append (a distinct category) to succeed — the
	// partial-failure interleaving.
	linkedAppendFail bool
}

type liveValHarness struct {
	s         *Server
	au        *auditFake
	rr        *promptRunRepo
	provider  *liveValFileProvider
	runID     uuid.UUID
	planStage *run.Stage
}

// liveValPlanBytes builds a plan whose acceptance criteria include one
// requires_live_validation criterion when marked; otherwise a plain plan.
func liveValPlanBytes(t *testing.T, marked bool) []byte {
	t.Helper()
	crits := []plan.AcceptanceCriterion{
		{ID: "ac1", Statement: "the webhook fires on a real push"},
		{ID: "ac2", Statement: "the response is 200"},
	}
	if marked {
		crits[0].RequiresLiveValidation = true
		// Per the slice-1 planner guidance, a live-validation criterion also
		// carries skip_expected + expectation_basis so acceptance short-circuits.
		crits[0].SkipExpected = true
		crits[0].ExpectationBasis = "validated by the integration test with a fake forge"
	}
	p := &plan.Plan{
		PlanVersion: "standard_v1",
		Summary:     "wire the push webhook",
		Scope:       plan.Scope{Files: []plan.ScopeFile{{Path: "backend/internal/webhook/webhook.go", Operation: plan.FileOpModify}}},
		Verification: plan.Verification{
			TestStrategy:       "unit + integration",
			RollbackPlan:       "revert the PR",
			AcceptanceCriteria: crits,
		},
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	return b
}

func newLiveValHarness(t *testing.T, cfg liveValConfig) *liveValHarness {
	t.Helper()
	provider := &liveValFileProvider{name: workmgmt.Default().Provider, fail: cfg.providerFail}
	workmgmt.Register(provider)

	au := newAuditFake()
	if cfg.linkedAppendFail {
		au.appendErrCategory = liveValidationWalkLinkedKind
	}
	rr := newPromptRunRepo()
	art := newFakeArtifactRepo()

	runID := uuid.New()
	planStageID := uuid.New()
	trigger := "issue:" + strconv.Itoa(liveValParentIssue)
	runRow := &run.Run{
		ID:             runID,
		Repo:           "o/r",
		WorkflowID:     "feature_change",
		TriggerRef:     &trigger,
		InstallationID: cfg.installID,
	}
	rr.getRuns[runID] = runRow
	planStage := &run.Stage{ID: planStageID, RunID: runID, Type: run.StageTypePlan, State: run.StageStateSucceeded}
	rr.getStages[planStageID] = planStage
	rr.stagesByRunID = map[uuid.UUID][]*run.Stage{runID: {planStage}}

	sv := "standard_v1"
	if _, err := art.Create(context.Background(), artifact.CreateParams{
		StageID:       planStageID,
		Kind:          artifact.KindPlan,
		SchemaVersion: &sv,
		Content:       liveValPlanBytes(t, cfg.marked),
	}); err != nil {
		t.Fatalf("seed plan artifact: %v", err)
	}

	s := New(Config{Addr: "127.0.0.1:0", AuditRepo: au, RunRepo: rr, ArtifactRepo: art})
	if cfg.github != nil {
		s.cfg.GitHub = cfg.github
	}
	return &liveValHarness{s: s, au: au, rr: rr, provider: provider, runID: runID, planStage: planStage}
}

// markerCount counts appended markers of a category.
func (h *liveValHarness) markerCount(category string) int {
	n := 0
	for _, e := range h.au.appended {
		if e.Category == category {
			n++
		}
	}
	return n
}

// newestLinked returns the newest appended linked marker (and whether any).
func (h *liveValHarness) newestLinked(t *testing.T) (liveValidationWalkMarker, bool) {
	t.Helper()
	var last liveValidationWalkMarker
	found := false
	for _, e := range h.au.appended {
		if e.Category != liveValidationWalkLinkedKind {
			continue
		}
		if err := json.Unmarshal(e.Payload, &last); err != nil {
			t.Fatalf("decode linked marker: %v", err)
		}
		found = true
	}
	return last, found
}

// seedLiveValidationMarker appends a marker directly to the auditFake's seeded
// history — used by the surface tests (and cross-package by runs_get_test.go /
// gateview_test.go) to drive liveValidationForRun without running the hook.
func seedLiveValidationMarker(au *auditFake, runID uuid.UUID, category string, m liveValidationWalkMarker) {
	payload, _ := json.Marshal(m)
	rid := runID
	au.seeded = append(au.seeded, &audit.Entry{
		RunID: &rid, Category: category, Payload: payload, Timestamp: time.Now().UTC(),
	})
}

// TestFileOrLinkLiveValidationWalk_HappyPath: a marked plan files exactly one
// chore walk, records a linked marker carrying the walk ref, and the surface
// reports the pending count + walk ref with filing_failed=false.
func TestFileOrLinkLiveValidationWalk_HappyPath(t *testing.T) {
	inst := int64(77)
	h := newLiveValHarness(t, liveValConfig{marked: true, installID: &inst})
	h.s.fileOrLinkLiveValidationWalk(context.Background(), h.planStage)

	if h.provider.calls != 1 {
		t.Fatalf("provider File called %d times, want 1", h.provider.calls)
	}
	if got := h.markerCount(liveValidationWalkIntentKind); got != 1 {
		t.Errorf("intent markers = %d, want 1", got)
	}
	linked, ok := h.newestLinked(t)
	if !ok {
		t.Fatalf("no linked marker written")
	}
	if linked.FilingFailed || linked.WalkRef == "" {
		t.Errorf("linked marker = %+v, want a walk_ref and filing_failed=false", linked)
	}
	if linked.PendingCriteriaCount != 1 {
		t.Errorf("pending count = %d, want 1", linked.PendingCriteriaCount)
	}

	surface := h.s.liveValidationForRun(context.Background(), h.runID)
	if surface == nil {
		t.Fatalf("surface nil, want the pending walk")
	}
	if surface.FilingFailed || surface.FilingIncomplete || surface.WalkRef == "" || surface.PendingCriteriaCount != 1 {
		t.Errorf("surface = %+v, want healthy walk (walk_ref set, not failed/incomplete, count 1)", surface)
	}
}

// TestFileOrLinkLiveValidationWalk_FilingFailure (replan directive 1): the
// forge filing fails, but a linked marker is STILL written with
// filing_failed=true and an empty ref, and the surface reflects the
// file-manually variant — approval never advances leaving the pending criteria
// with zero surfaced indication.
func TestFileOrLinkLiveValidationWalk_FilingFailure(t *testing.T) {
	inst := int64(77)
	h := newLiveValHarness(t, liveValConfig{marked: true, installID: &inst, providerFail: true})
	h.s.fileOrLinkLiveValidationWalk(context.Background(), h.planStage)

	linked, ok := h.newestLinked(t)
	if !ok {
		t.Fatalf("no linked marker written on filing failure — nothing surfaced")
	}
	if !linked.FilingFailed || linked.WalkRef != "" {
		t.Errorf("linked marker = %+v, want filing_failed=true and an empty walk_ref", linked)
	}
	if linked.PendingCriteriaCount != 1 {
		t.Errorf("pending count = %d, want 1", linked.PendingCriteriaCount)
	}

	surface := h.s.liveValidationForRun(context.Background(), h.runID)
	if surface == nil || !surface.FilingFailed || surface.FilingIncomplete || surface.WalkRef != "" {
		t.Errorf("surface = %+v, want file-manually (filing_failed, not incomplete, no ref)", surface)
	}
}

// TestFileOrLinkLiveValidationWalk_PartialFailureReapproval (replan directive 2
// / binding condition A): the linked-marker write fails AFTER the walk is filed,
// so the newest marker is a bare intent marker. A second approval reads that
// intent marker and NO-OPS — the provider File is invoked EXACTLY ONCE across
// both approvals (the intent-marker-before-file guard) — and the surface renders
// the stranded-intent file-manually variant.
func TestFileOrLinkLiveValidationWalk_PartialFailureReapproval(t *testing.T) {
	inst := int64(77)
	h := newLiveValHarness(t, liveValConfig{marked: true, installID: &inst, linkedAppendFail: true})

	// First approval: intent appended, walk filed, linked-marker append fails.
	h.s.fileOrLinkLiveValidationWalk(context.Background(), h.planStage)
	if h.provider.calls != 1 {
		t.Fatalf("after first approval provider File called %d times, want 1", h.provider.calls)
	}
	if got := h.markerCount(liveValidationWalkIntentKind); got != 1 {
		t.Fatalf("intent markers after first approval = %d, want 1", got)
	}
	if got := h.markerCount(liveValidationWalkLinkedKind); got != 0 {
		t.Fatalf("linked markers after first approval = %d, want 0 (append injected to fail)", got)
	}

	// Second approval: the intent marker guards re-filing.
	h.s.fileOrLinkLiveValidationWalk(context.Background(), h.planStage)
	if h.provider.calls != 1 {
		t.Errorf("after re-approval provider File called %d times total, want 1 (intent-marker guard)", h.provider.calls)
	}
	if got := h.markerCount(liveValidationWalkIntentKind); got != 1 {
		t.Errorf("intent markers after re-approval = %d, want 1 (no second intent)", got)
	}

	// The stranded intent marker surfaces as file-manually / incomplete.
	surface := h.s.liveValidationForRun(context.Background(), h.runID)
	if surface == nil || !surface.FilingFailed || !surface.FilingIncomplete || surface.WalkRef != "" {
		t.Errorf("surface = %+v, want file-manually incomplete (filing_failed+filing_incomplete, no ref)", surface)
	}
	if surface != nil && surface.PendingCriteriaCount != 1 {
		t.Errorf("pending count = %d, want 1", surface.PendingCriteriaCount)
	}
}

// TestFileOrLinkLiveValidationWalk_StrandedIntentNoOp (binding condition A(3)):
// an intent marker present with NO linked marker (the crash-window residual) —
// a re-approval is a NO-OP (provider File invoked zero additional times) and the
// surface renders the file-manually variant for the stranded intent.
func TestFileOrLinkLiveValidationWalk_StrandedIntentNoOp(t *testing.T) {
	inst := int64(77)
	h := newLiveValHarness(t, liveValConfig{marked: true, installID: &inst})
	// Pre-seed a bare intent marker (no linked marker follows).
	seedLiveValidationMarker(h.au, h.runID, liveValidationWalkIntentKind, liveValidationWalkMarker{
		Phase: "intent", PendingCriteriaCount: 1, CriterionIDs: []string{"ac1"},
	})

	h.s.fileOrLinkLiveValidationWalk(context.Background(), h.planStage)
	if h.provider.calls != 0 {
		t.Errorf("provider File called %d times, want 0 (bare intent marker → no-op)", h.provider.calls)
	}
	if got := h.markerCount(liveValidationWalkIntentKind); got != 0 {
		t.Errorf("appended intent markers = %d, want 0 (the seeded one guards re-filing)", got)
	}

	surface := h.s.liveValidationForRun(context.Background(), h.runID)
	if surface == nil || !surface.FilingFailed || !surface.FilingIncomplete || surface.WalkRef != "" {
		t.Errorf("surface = %+v, want file-manually incomplete for the stranded intent", surface)
	}
}

// TestFileOrLinkLiveValidationWalk_NoMarkedCriterion_NoOp: a plan with no
// requires_live_validation criterion files no walk, writes no marker, and the
// surface is omitted.
func TestFileOrLinkLiveValidationWalk_NoMarkedCriterion_NoOp(t *testing.T) {
	inst := int64(77)
	h := newLiveValHarness(t, liveValConfig{marked: false, installID: &inst})
	h.s.fileOrLinkLiveValidationWalk(context.Background(), h.planStage)

	if h.provider.calls != 0 {
		t.Errorf("provider File called %d times, want 0 (no marked criterion)", h.provider.calls)
	}
	if got := h.markerCount(liveValidationWalkIntentKind); got != 0 {
		t.Errorf("intent markers = %d, want 0", got)
	}
	if got := h.markerCount(liveValidationWalkLinkedKind); got != 0 {
		t.Errorf("linked markers = %d, want 0", got)
	}
	if surface := h.s.liveValidationForRun(context.Background(), h.runID); surface != nil {
		t.Errorf("surface = %+v, want nil (no marked criterion)", surface)
	}
}

// TestFileOrLinkLiveValidationWalk_EpicParenting (replan directives 3 & 4): the
// best-effort epic-parenting has two branches — the epic is derivable (the walk
// parents to it) or it is not (the walk companion-links to the triggering issue
// via a title_vars fallback).
func TestFileOrLinkLiveValidationWalk_EpicParenting(t *testing.T) {
	parentRef := "#" + strconv.Itoa(liveValParentIssue)

	t.Run("derivable_parents_to_epic", func(t *testing.T) {
		inst := int64(77)
		gh := newLiveValIssueGitHub(t, "[E48.35] first-class live-validation criteria")
		h := newLiveValHarness(t, liveValConfig{marked: true, installID: &inst, github: gh})
		h.s.fileOrLinkLiveValidationWalk(context.Background(), h.planStage)

		if len(h.provider.reqs) != 1 {
			t.Fatalf("filed %d walks, want 1", len(h.provider.reqs))
		}
		req := h.provider.reqs[0]
		if req.Item.Relations.ParentEpic != parentRef {
			t.Errorf("walk parent_epic = %q, want %q (parented to the epic)", req.Item.Relations.ParentEpic, parentRef)
		}
		if len(req.Item.Relations.CompanionTo) != 0 {
			t.Errorf("derivable walk must not companion-link: %v", req.Item.Relations.CompanionTo)
		}
		// The [E48.35] title token derived {epic}=48; with the explicit n the
		// chore title renders under the epic.
		if req.Item.Title == "" || req.Item.Title[:3] != "[E4" {
			t.Errorf("walk title = %q, want an [E48.<n>] epic-parented title", req.Item.Title)
		}
	})

	t.Run("underivable_companion_links", func(t *testing.T) {
		inst := int64(77)
		// No GitHub client: deriveEpicTitleVar cannot resolve {epic}, so the
		// epic attempt 422s at Apply and the fallback files.
		h := newLiveValHarness(t, liveValConfig{marked: true, installID: &inst})
		h.s.fileOrLinkLiveValidationWalk(context.Background(), h.planStage)

		if len(h.provider.reqs) != 1 {
			t.Fatalf("filed %d walks, want 1 (fallback)", len(h.provider.reqs))
		}
		req := h.provider.reqs[0]
		if len(req.Item.Relations.CompanionTo) != 1 || req.Item.Relations.CompanionTo[0] != parentRef {
			t.Errorf("walk companion_to = %v, want [%s]", req.Item.Relations.CompanionTo, parentRef)
		}
		if req.Item.Relations.ParentEpic != "" {
			t.Errorf("fallback walk must not set parent_epic: %q", req.Item.Relations.ParentEpic)
		}
		// The title_vars fallback used the triggering issue number as {epic}.
		wantTitleEpic := "[E" + strconv.Itoa(liveValParentIssue) + ".1]"
		if len(req.Item.Title) < len(wantTitleEpic) || req.Item.Title[:len(wantTitleEpic)] != wantTitleEpic {
			t.Errorf("walk title = %q, want prefix %q (title_vars fallback)", req.Item.Title, wantTitleEpic)
		}
	})
}

// --- defensive fail-open branches ----------------------------------------

// TestLiveValidationForRun_NilAuditRepo: with no audit repo wired the surface
// is omitted rather than the read panicking.
func TestLiveValidationForRun_NilAuditRepo(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0"})
	if got := s.liveValidationForRun(context.Background(), uuid.New()); got != nil {
		t.Errorf("surface = %+v, want nil with no audit repo", got)
	}
}

// TestLiveValidationForRun_LinkedReadFailure: a linked-category read error
// degrades to an omitted field (best-effort), never a failed read.
func TestLiveValidationForRun_LinkedReadFailure(t *testing.T) {
	au := newAuditFake()
	au.listByCategoryErr = errors.New("boom")
	s := New(Config{Addr: "127.0.0.1:0", AuditRepo: au})
	if got := s.liveValidationForRun(context.Background(), uuid.New()); got != nil {
		t.Errorf("surface = %+v, want nil on a read failure", got)
	}
}

// TestLiveValidationForRun_UndecodableLinked: a corrupt linked-marker payload
// degrades to an omitted field rather than surfacing a half-decoded block.
func TestLiveValidationForRun_UndecodableLinked(t *testing.T) {
	au := newAuditFake()
	runID := uuid.New()
	rid := runID
	au.seeded = append(au.seeded, &audit.Entry{
		RunID: &rid, Category: liveValidationWalkLinkedKind,
		Payload: []byte(`{not json`), Timestamp: time.Now().UTC(),
	})
	s := New(Config{Addr: "127.0.0.1:0", AuditRepo: au})
	if got := s.liveValidationForRun(context.Background(), runID); got != nil {
		t.Errorf("surface = %+v, want nil on an undecodable linked marker", got)
	}
}

// TestFileOrLinkLiveValidationWalk_NilRepos: the hook no-ops (no panic) when the
// audit or run repo is unwired.
func TestFileOrLinkLiveValidationWalk_NilRepos(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0"}) // no AuditRepo / RunRepo
	stage := &run.Stage{ID: uuid.New(), RunID: uuid.New(), Type: run.StageTypePlan}
	s.fileOrLinkLiveValidationWalk(context.Background(), stage) // must not panic
}

// TestFileOrLinkLiveValidationWalk_NonIssueTrigger: a run whose trigger ref is
// not an issue has no originating issue to companion-link against, so the hook
// files no walk and records no marker (skips rather than filing an orphan).
func TestFileOrLinkLiveValidationWalk_NonIssueTrigger(t *testing.T) {
	inst := int64(77)
	h := newLiveValHarness(t, liveValConfig{marked: true, installID: &inst})
	trigger := "push:refs/heads/main"
	h.rr.getRuns[h.runID].TriggerRef = &trigger

	h.s.fileOrLinkLiveValidationWalk(context.Background(), h.planStage)
	if h.provider.calls != 0 {
		t.Errorf("provider File called %d times, want 0 (no originating issue)", h.provider.calls)
	}
	if h.markerCount(liveValidationWalkIntentKind) != 0 {
		t.Errorf("intent markers written, want 0 for a non-issue trigger")
	}
}

// TestFileOrLinkLiveValidationWalk_IntentAppendFailure: when the intent-marker
// append fails, the hook files NOTHING (no forge call, no linked marker) so a
// re-approval retries cleanly with no orphan walk.
func TestFileOrLinkLiveValidationWalk_IntentAppendFailure(t *testing.T) {
	inst := int64(77)
	h := newLiveValHarness(t, liveValConfig{marked: true, installID: &inst})
	h.au.appendErrCategory = liveValidationWalkIntentKind

	h.s.fileOrLinkLiveValidationWalk(context.Background(), h.planStage)
	if h.provider.calls != 0 {
		t.Errorf("provider File called %d times, want 0 (intent append failed → filed nothing)", h.provider.calls)
	}
	if h.markerCount(liveValidationWalkLinkedKind) != 0 {
		t.Errorf("linked markers written, want 0 when the intent append failed")
	}
}
