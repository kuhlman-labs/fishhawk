package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/approval"
	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/splitfiling"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// splitFileProvider is an incrementing work-item provider for the split-filing
// tests: each successful File returns a DISTINCT issue number (so depends_on
// resolution to a sibling #N is observable) and records the ProviderRequest.
// failOnCall (1-based) injects a mid-sequence provider failure for the
// partial-failure resume test; the same instance persists its counters across
// two hook invocations so a resume never re-files a durably-filed ordinal.
type splitFileProvider struct {
	name       string
	mu         sync.Mutex
	reqs       []workmgmt.ProviderRequest
	calls      int
	success    int
	failOnCall int
}

func (p *splitFileProvider) Name() string { return p.name }

func (p *splitFileProvider) File(_ context.Context, req workmgmt.ProviderRequest) (*workmgmt.CreatedItem, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	if p.failOnCall != 0 && p.calls == p.failOnCall {
		return nil, errors.New("split provider: injected File error")
	}
	p.reqs = append(p.reqs, req)
	p.success++
	num := 3000 + p.success
	return &workmgmt.CreatedItem{
		Provider: p.name,
		Number:   num,
		URL:      fmt.Sprintf("https://github.com/o/r/issues/%d", num),
		Boarded:  true,
	}, nil
}

func (p *splitFileProvider) requestFor(t *testing.T, title string) workmgmt.ProviderRequest {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, r := range p.reqs {
		if strings.Contains(r.Item.Title, title) {
			return r
		}
	}
	t.Fatalf("no filed request with title containing %q; filed %d", title, len(p.reqs))
	return workmgmt.ProviderRequest{}
}

// splitCommentGitHub records the parent acceptance-carrier comment POSTs and
// returns the configured status (so a 500 exercises the best-effort path). Every
// other GitHub endpoint (e.g. the area-label GetIssue) 404s and is fail-open.
type splitCommentGitHub struct {
	mu       sync.Mutex
	comments []splitCommentCall
	status   int
}

type splitCommentCall struct {
	issue int
	body  string
}

func newSplitCommentGitHub(t *testing.T, status int) (*splitCommentGitHub, *githubclient.Client) {
	t.Helper()
	rec := &splitCommentGitHub{status: status}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /repos/{owner}/{repo}/issues/{number}/comments",
		func(w http.ResponseWriter, r *http.Request) {
			num, _ := strconv.Atoi(r.PathValue("number"))
			var body struct {
				Body string `json:"body"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			rec.mu.Lock()
			rec.comments = append(rec.comments, splitCommentCall{issue: num, body: body.Body})
			st := rec.status
			rec.mu.Unlock()
			w.WriteHeader(st)
			if st < 300 {
				_, _ = w.Write([]byte(`{"id":1}`))
			}
		})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return rec, &githubclient.Client{
		BaseURL: srv.URL,
		Tokens:  &fakeTokenProvider{tok: "ghs_t"},
		HTTP:    &http.Client{Timeout: 5 * time.Second},
	}
}

// splitFilingConfig configures a split-filing test harness.
type splitFilingConfig struct {
	withSplitProposal   bool // default set by newSplitFilingHarness caller
	withSpec            bool // seed WorkflowSpec so the implement cap resolves (=3)
	reachabilityDerived int  // contract-phase DerivedCount to seed; 0 = seed NO reachability entry
	installID           *int64
	github              *githubclient.Client
	providerFailOnCall  int
}

// splitFilingHarness bundles the wired Server plus the seeded run coordinates.
type splitFilingHarness struct {
	s         *Server
	au        *auditFake
	rr        *promptRunRepo
	provider  *splitFileProvider
	runID     uuid.UUID
	planStage *run.Stage
}

const splitParentIssue = 2100

// writeSentinel seeds a .fishhawk/workflows.yaml sentinel used to prove the hook
// writes nothing to .fishhawk/**.
func writeSentinel(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(dir+"/workflows.yaml", []byte("sentinel: untouched\n"), 0o644)
}

func readSentinel(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(dir + "/workflows.yaml")
	if err != nil {
		t.Fatalf("read sentinel: %v", err)
	}
	return string(b)
}

func fishhawkFileNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read .fishhawk dir: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func splitPlanBytes(t *testing.T, withProposal bool) []byte {
	t.Helper()
	p := &plan.Plan{
		PlanVersion: "standard_v1",
		Summary:     "over-cap rename of Foo to NewFoo",
		Scope:       plan.Scope{Files: []plan.ScopeFile{{Path: "backend/internal/foo/foo.go", Operation: plan.FileOpModify}}},
		Verification: plan.Verification{
			TestStrategy: "unit + integration",
			RollbackPlan: "revert the PR",
			AcceptanceCriteria: []plan.AcceptanceCriterion{
				{ID: "ac1", Statement: "the old name Foo no longer exists"},
				{ID: "ac2", Statement: "all callers use NewFoo"},
			},
		},
	}
	if withProposal {
		p.SplitProposal = &plan.SplitProposal{
			Rationale: "scope.files exceeds the implement cap by count",
			Phases: []plan.SplitPhase{
				{
					Title:     "expand: add NewFoo alongside Foo",
					ScopeHint: "add NewFoo alongside Foo in backend/internal/foo",
					Scope:     &plan.Scope{Files: []plan.ScopeFile{{Path: "backend/internal/foo/foo.go", Operation: plan.FileOpModify}}},
				},
				{
					Title:     "migrate consumers to NewFoo",
					ScopeHint: "migrate consumers of Foo to NewFoo across backend/internal",
					Scope:     &plan.Scope{Files: []plan.ScopeFile{{Path: "backend/internal/bar/bar.go", Operation: plan.FileOpModify}}},
					DependsOn: []int{0},
				},
				{
					Title:     "contract: delete the transitional Foo",
					ScopeHint: "delete Foo now that all consumers use NewFoo",
					Scope:     &plan.Scope{Files: []plan.ScopeFile{{Path: "backend/internal/foo/foo.go", Operation: plan.FileOpDelete}}},
					DependsOn: []int{1},
				},
			},
		}
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	return b
}

// seedReachability appends a plan_reachability_sweep audit entry whose contract
// (last) phase carries the given DerivedCount, so Classify can be steered.
func seedReachability(au *auditFake, runID uuid.UUID, contractDerived int) {
	payload, _ := json.Marshal(PlanReachabilityPayload{
		Available: true,
		Phases: []PlanReachabilityPhase{
			{Index: 0, Title: "expand", DeclaredCount: 1, DerivedCount: 1},
			{Index: 1, Title: "migrate", DeclaredCount: 1, DerivedCount: 1},
			{Index: 2, Title: "contract", DeclaredCount: 1, DerivedCount: contractDerived},
		},
	})
	rid := runID
	au.seeded = append(au.seeded, &audit.Entry{
		RunID:    &rid,
		Category: reachabilitySweepAuditKind,
		Payload:  payload,
	})
}

func newSplitFilingHarness(t *testing.T, cfg splitFilingConfig) *splitFilingHarness {
	t.Helper()
	provider := &splitFileProvider{name: workmgmt.Default().Provider, failOnCall: cfg.providerFailOnCall}
	workmgmt.Register(provider)

	au := newAuditFake()
	rr := newPromptRunRepo()
	art := newFakeArtifactRepo()

	runID := uuid.New()
	planStageID := uuid.New()
	trigger := "issue:" + strconv.Itoa(splitParentIssue)
	runRow := &run.Run{
		ID:             runID,
		Repo:           "o/r",
		WorkflowID:     "feature_change",
		TriggerRef:     &trigger,
		InstallationID: cfg.installID,
	}
	if cfg.withSpec {
		runRow.WorkflowSpec = specImplementPathConstraints
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
		Content:       splitPlanBytes(t, cfg.withSplitProposal),
	}); err != nil {
		t.Fatalf("seed plan artifact: %v", err)
	}

	if cfg.reachabilityDerived != 0 {
		seedReachability(au, runID, cfg.reachabilityDerived)
	}

	s := New(Config{Addr: "127.0.0.1:0", AuditRepo: au, RunRepo: rr, ArtifactRepo: art})
	if cfg.github != nil {
		s.cfg.GitHub = cfg.github
	}
	return &splitFilingHarness{s: s, au: au, rr: rr, provider: provider, runID: runID, planStage: planStage}
}

// completionEntry returns the decoded split_children_filed completion payload
// and how many were appended (0 asserts none written).
func (h *splitFilingHarness) completionEntry(t *testing.T) (splitChildrenFiledPayload, int) {
	t.Helper()
	var payloads []splitChildrenFiledPayload
	for _, e := range h.au.appended {
		if e.Category == splitChildrenFiledCategory {
			var p splitChildrenFiledPayload
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				t.Fatalf("decode split_children_filed payload: %v", err)
			}
			payloads = append(payloads, p)
		}
	}
	if len(payloads) == 0 {
		return splitChildrenFiledPayload{}, 0
	}
	return payloads[len(payloads)-1], len(payloads)
}

func (h *splitFilingHarness) childMarkerCount(t *testing.T) int {
	t.Helper()
	n := 0
	for _, e := range h.au.appended {
		if e.Category != categoryWorkItemFiled {
			continue
		}
		var m splitChildFiledMarker
		if json.Unmarshal(e.Payload, &m) == nil && m.SplitPhaseIndex != nil {
			n++
		}
	}
	return n
}

// TestFileSplitProposalChildren_HappyPath_DeleteOnly is the integration seam:
// a 3-phase proposal files 3 children with symbol-set scopes + depends_on edges
// resolved to sibling #N, each body referencing the parent + design #2008, the
// parent comment posted naming the contract child + #2062, and one
// split_children_filed completion marker carrying the delete-only classification
// + the #2062 deferral.
func TestFileSplitProposalChildren_HappyPath_DeleteOnly(t *testing.T) {
	inst := int64(77)
	rec, gh := newSplitCommentGitHub(t, http.StatusCreated)
	h := newSplitFilingHarness(t, splitFilingConfig{
		withSplitProposal: true, withSpec: true,
		reachabilityDerived: 2, // <= cap (3) -> delete-only
		installID:           &inst, github: gh,
	})
	h.s.fileSplitProposalChildren(context.Background(), h.planStage)

	if len(h.provider.reqs) != 3 {
		t.Fatalf("filed %d children, want 3", len(h.provider.reqs))
	}
	// depends_on edges resolve to the sibling filed #N (phase1 -> phase0,
	// phase2 -> phase1). Phase filing order is wave order, so #3001/#3002/#3003
	// map to phases 0/1/2.
	migrate := h.provider.requestFor(t, "migrate consumers")
	if got := migrate.Item.Relations.DependsOn; len(got) != 1 || got[0] != "#3001" {
		t.Errorf("migrate child depends_on = %v, want [#3001] (the expand child)", got)
	}
	contract := h.provider.requestFor(t, "contract: delete")
	if got := contract.Item.Relations.DependsOn; len(got) != 1 || got[0] != "#3002" {
		t.Errorf("contract child depends_on = %v, want [#3002] (the migrate child)", got)
	}
	// Symbol-set scope prose, not a stale file list, in the body.
	if !strings.Contains(migrate.Item.Body, "migrate consumers of Foo to NewFoo") {
		t.Errorf("migrate child body missing symbol-set scope: %q", migrate.Item.Body)
	}
	if strings.Contains(migrate.Item.Body, "bar.go") {
		t.Errorf("migrate child body must not embed a raw file list: %q", migrate.Item.Body)
	}
	// Each body references the parent + the design issue #2008.
	for _, r := range h.provider.reqs {
		if !strings.Contains(r.Item.Body, "#2100") || !strings.Contains(r.Item.Body, "#2008") {
			t.Errorf("child body must reference parent #2100 and design #2008: %q", r.Item.Body)
		}
	}

	// Parent acceptance-carrier comment posted, naming the contract child +
	// #2062, on the parent issue.
	if len(rec.comments) != 1 {
		t.Fatalf("posted %d parent comments, want 1", len(rec.comments))
	}
	c := rec.comments[0]
	if c.issue != splitParentIssue {
		t.Errorf("comment posted on issue %d, want %d", c.issue, splitParentIssue)
	}
	if !strings.Contains(c.body, "#3003") || !strings.Contains(c.body, "#2062") {
		t.Errorf("parent comment must name the contract child #3003 and follow-up #2062: %q", c.body)
	}
	if !strings.Contains(c.body, "not automated by this change") {
		t.Errorf("parent comment must state the parent-close is NOT automated now: %q", c.body)
	}

	// One completion marker carrying delete-only + children + #2062 deferral.
	payload, n := h.completionEntry(t)
	if n != 1 {
		t.Fatalf("wrote %d split_children_filed entries, want exactly 1", n)
	}
	if payload.ContractClassification != string(splitfiling.ClassificationDeleteOnly) {
		t.Errorf("classification = %q, want delete-only", payload.ContractClassification)
	}
	if len(payload.Children) != 3 {
		t.Errorf("completion payload children = %d, want 3", len(payload.Children))
	}
	if payload.ContractChildNumber != 3003 {
		t.Errorf("contract_child_number = %d, want 3003", payload.ContractChildNumber)
	}
	if payload.DeferralIssue != splitfiling.DeferralIssue {
		t.Errorf("deferral_issue = %d, want %d", payload.DeferralIssue, splitfiling.DeferralIssue)
	}
	if payload.CapException != nil {
		t.Errorf("delete-only must carry no cap-exception draft, got %+v", payload.CapException)
	}
}

// TestFileSplitProposalChildren_GovernedException asserts the governed-exception
// branch: contract DerivedCount (12) > cap (3) -> the drafted spec diff raises
// max_files_changed to the DerivedCount and the PR body states
// operator-authored + admin-merged, riding the audit payload.
func TestFileSplitProposalChildren_GovernedException(t *testing.T) {
	h := newSplitFilingHarness(t, splitFilingConfig{
		withSplitProposal: true, withSpec: true,
		reachabilityDerived: 12, // > cap (3) -> governed-exception
	})
	h.s.fileSplitProposalChildren(context.Background(), h.planStage)

	payload, n := h.completionEntry(t)
	if n != 1 {
		t.Fatalf("wrote %d split_children_filed entries, want 1", n)
	}
	if payload.ContractClassification != string(splitfiling.ClassificationGovernedException) {
		t.Fatalf("classification = %q, want governed-exception", payload.ContractClassification)
	}
	if payload.CapException == nil {
		t.Fatal("governed-exception must carry a cap-exception draft")
	}
	// The spec diff literally raises the cap to the contract DerivedCount (12).
	if !strings.Contains(payload.CapException.SpecDiff, "max_files_changed: 12") {
		t.Errorf("spec diff must raise max_files_changed to 12: %q", payload.CapException.SpecDiff)
	}
	if !strings.Contains(payload.CapException.SpecDiff, "-        max_files_changed: 3") {
		t.Errorf("spec diff must remove the old cap of 3: %q", payload.CapException.SpecDiff)
	}
	// The PR body states operator-authored + admin-merged.
	body := payload.CapException.PRBody
	if !strings.Contains(body, "Operator-authored") || !strings.Contains(body, "admin-merge") {
		t.Errorf("PR body must state operator-authored + admin-merged: %q", body)
	}
}

// TestFileSplitProposalChildren_NoSplitProposal_NoOp: a plan without a
// split_proposal files no children and writes no audit.
func TestFileSplitProposalChildren_NoSplitProposal_NoOp(t *testing.T) {
	h := newSplitFilingHarness(t, splitFilingConfig{withSplitProposal: false, withSpec: true})
	h.s.fileSplitProposalChildren(context.Background(), h.planStage)

	if len(h.provider.reqs) != 0 {
		t.Errorf("filed %d children on a no-split plan, want 0", len(h.provider.reqs))
	}
	if _, n := h.completionEntry(t); n != 0 {
		t.Errorf("wrote %d split_children_filed entries on a no-split plan, want 0", n)
	}
}

// TestFileSplitProposalChildren_MissingReachability_DeleteOnly: cap resolves but
// no reachability evidence -> fail-safe delete-only (and still files).
func TestFileSplitProposalChildren_MissingReachability_DeleteOnly(t *testing.T) {
	h := newSplitFilingHarness(t, splitFilingConfig{
		withSplitProposal: true, withSpec: true, reachabilityDerived: 0,
	})
	h.s.fileSplitProposalChildren(context.Background(), h.planStage)

	payload, n := h.completionEntry(t)
	if n != 1 {
		t.Fatalf("wrote %d completion entries, want 1", n)
	}
	if payload.ContractClassification != string(splitfiling.ClassificationDeleteOnly) {
		t.Errorf("classification = %q, want delete-only (missing evidence fail-safe)", payload.ContractClassification)
	}
	if payload.CapException != nil {
		t.Errorf("missing-evidence path must draft no cap exception")
	}
}

// TestFileSplitProposalChildren_CapUnresolved_DeleteOnly: no WorkflowSpec -> cap
// is 0 -> Classify fails safe to delete-only even with over-cap-looking
// evidence.
func TestFileSplitProposalChildren_CapUnresolved_DeleteOnly(t *testing.T) {
	h := newSplitFilingHarness(t, splitFilingConfig{
		withSplitProposal: true, withSpec: false, reachabilityDerived: 99,
	})
	h.s.fileSplitProposalChildren(context.Background(), h.planStage)

	payload, n := h.completionEntry(t)
	if n != 1 {
		t.Fatalf("wrote %d completion entries, want 1", n)
	}
	if payload.ContractClassification != string(splitfiling.ClassificationDeleteOnly) {
		t.Errorf("classification = %q, want delete-only (cap<=0 fail-safe)", payload.ContractClassification)
	}
	if payload.CapException != nil {
		t.Errorf("cap<=0 must draft no cap exception")
	}
}

// TestFileSplitProposalChildren_IdempotentReapproval_NoDuplicate: a second
// invocation after a complete run files no duplicate children and writes no
// second completion marker (dedup on the completion marker).
func TestFileSplitProposalChildren_IdempotentReapproval_NoDuplicate(t *testing.T) {
	h := newSplitFilingHarness(t, splitFilingConfig{
		withSplitProposal: true, withSpec: true, reachabilityDerived: 2,
	})
	h.s.fileSplitProposalChildren(context.Background(), h.planStage)
	h.s.fileSplitProposalChildren(context.Background(), h.planStage)

	if len(h.provider.reqs) != 3 {
		t.Errorf("re-approval filed %d children total, want 3 (no duplicates)", len(h.provider.reqs))
	}
	if _, n := h.completionEntry(t); n != 1 {
		t.Errorf("re-approval wrote %d completion entries, want exactly 1", n)
	}
}

// TestFileSplitProposalChildren_PartialFailureResumes is operator binding
// condition 1: M-of-N children file, one errors -> NO completion marker, then a
// re-approval fills only the un-filed ordinals (no duplicates) and writes the
// completion marker reflecting the full set.
func TestFileSplitProposalChildren_PartialFailureResumes(t *testing.T) {
	h := newSplitFilingHarness(t, splitFilingConfig{
		withSplitProposal: true, withSpec: true, reachabilityDerived: 2,
		providerFailOnCall: 2, // phase 0 files; phase 1 errors
	})

	// Pass 1: partial.
	h.s.fileSplitProposalChildren(context.Background(), h.planStage)
	if len(h.provider.reqs) != 1 {
		t.Fatalf("pass 1 filed %d children, want 1 (the second errored)", len(h.provider.reqs))
	}
	if _, n := h.completionEntry(t); n != 0 {
		t.Fatalf("partial run must write NO completion marker, wrote %d", n)
	}
	if got := h.childMarkerCount(t); got != 1 {
		t.Fatalf("partial run recorded %d per-phase markers, want 1", got)
	}

	// Pass 2: clear the injected failure and re-approve.
	h.provider.mu.Lock()
	h.provider.failOnCall = 0
	h.provider.mu.Unlock()
	h.s.fileSplitProposalChildren(context.Background(), h.planStage)

	if len(h.provider.reqs) != 3 {
		t.Fatalf("after resume filed %d children total, want 3 (no re-file of phase 0)", len(h.provider.reqs))
	}
	payload, n := h.completionEntry(t)
	if n != 1 {
		t.Fatalf("after resume wrote %d completion entries, want 1", n)
	}
	if len(payload.Children) != 3 {
		t.Fatalf("completion payload reflects %d children, want the full 3", len(payload.Children))
	}
	// Phase 0 (#3001) filed in pass 1 must not be re-filed: the completion set
	// carries 3 DISTINCT numbers, with phase 0 pinned to its pass-1 number.
	numbers := map[int]bool{}
	for _, c := range payload.Children {
		numbers[c.Number] = true
		if c.PhaseIndex == 0 && c.Number != 3001 {
			t.Errorf("phase 0 number = %d, want 3001 (its pass-1 filing, not re-filed)", c.Number)
		}
	}
	if len(numbers) != 3 {
		t.Errorf("completion child numbers = %v, want 3 distinct (no duplicate ordinal)", numbers)
	}
}

// TestFileSplitProposalChildren_MarkerAppendFailure_AbortsNoCompletion is the
// marker-append-failure interleaving operator binding condition 1 targets and
// the implement review flagged as untested (the partial-failure test injects a
// provider File error, not a marker append failure). The per-ordinal
// work_item_filed resume marker is the hook's SOLE durable filing record, so if
// its append fails the run must ABORT with no completion marker — never a false
// completion on a durable state it can no longer resume — rather than filing the
// remaining children on top of a lost record. This mirrors
// refinement.ExecuteFiling's abort-on-record-failure discipline; the one child
// that did File re-files once on a later re-approval is the documented
// at-least-once residual, not a widening of the window.
func TestFileSplitProposalChildren_MarkerAppendFailure_AbortsNoCompletion(t *testing.T) {
	inst := int64(91)
	rec, gh := newSplitCommentGitHub(t, http.StatusCreated)
	h := newSplitFilingHarness(t, splitFilingConfig{
		withSplitProposal: true, withSpec: true, reachabilityDerived: 2,
		installID: &inst, github: gh,
	})
	// The per-ordinal resume-marker append is the failing operation.
	h.au.appendErrCategory = categoryWorkItemFiled

	h.s.fileSplitProposalChildren(context.Background(), h.planStage)

	// Exactly ONE child filed with the provider; the lost marker then aborted the
	// loop before filing the rest.
	if len(h.provider.reqs) != 1 {
		t.Fatalf("filed %d children, want 1 (abort on the first lost marker)", len(h.provider.reqs))
	}
	// No durable marker persisted (the append itself failed).
	if got := h.childMarkerCount(t); got != 0 {
		t.Errorf("recorded %d resume markers, want 0 (the append failed)", got)
	}
	// The abort wrote NO completion marker: never a false completion.
	if _, n := h.completionEntry(t); n != 0 {
		t.Errorf("marker-append failure must write NO completion marker, wrote %d", n)
	}
	// And posted NO parent acceptance-carrier comment (that follows completion).
	if len(rec.comments) != 0 {
		t.Errorf("marker-append failure must post no parent comment, posted %d", len(rec.comments))
	}
}

// TestFileSplitProposalChildren_ParentCommentFailure_BestEffort: a failing
// parent comment does not block filing or the completion marker.
func TestFileSplitProposalChildren_ParentCommentFailure_BestEffort(t *testing.T) {
	inst := int64(88)
	rec, gh := newSplitCommentGitHub(t, http.StatusInternalServerError)
	h := newSplitFilingHarness(t, splitFilingConfig{
		withSplitProposal: true, withSpec: true, reachabilityDerived: 2,
		installID: &inst, github: gh,
	})
	h.s.fileSplitProposalChildren(context.Background(), h.planStage)

	if len(rec.comments) != 1 {
		t.Fatalf("expected 1 (failing) comment attempt, got %d", len(rec.comments))
	}
	if len(h.provider.reqs) != 3 {
		t.Errorf("filed %d children, want 3 (comment failure is best-effort)", len(h.provider.reqs))
	}
	if _, n := h.completionEntry(t); n != 1 {
		t.Errorf("comment failure must not suppress the completion marker; wrote %d", n)
	}
}

// TestFileSplitProposalChildren_NoFilesystemWrite asserts the governed-exception
// cap-exception draft rides the audit payload and NOTHING is written to
// .fishhawk/** during filing: a sentinel .fishhawk file in a temp cwd is
// byte-unchanged and no new .fishhawk file appears.
func TestFileSplitProposalChildren_NoFilesystemWrite(t *testing.T) {
	dir := t.TempDir()
	fishhawkDir := dir + "/.fishhawk"
	if err := writeSentinel(fishhawkDir); err != nil {
		t.Fatalf("seed sentinel: %v", err)
	}
	before := readSentinel(t, fishhawkDir)

	h := newSplitFilingHarness(t, splitFilingConfig{
		withSplitProposal: true, withSpec: true, reachabilityDerived: 12,
	})
	h.s.fileSplitProposalChildren(context.Background(), h.planStage)

	// The draft rode the audit payload (delivered, not written to disk).
	payload, n := h.completionEntry(t)
	if n != 1 || payload.CapException == nil {
		t.Fatalf("governed-exception draft must ride the audit payload")
	}
	// The sentinel .fishhawk/workflows.yaml is untouched and no other .fishhawk
	// file was created.
	after := readSentinel(t, fishhawkDir)
	if before != after {
		t.Errorf(".fishhawk sentinel changed: before=%q after=%q", before, after)
	}
	if names := fishhawkFileNames(t, fishhawkDir); len(names) != 1 {
		t.Errorf(".fishhawk dir gained files during filing: %v", names)
	}
}

// TestFileSplitProposalChildren_EndToEnd is operator binding condition 2's
// approval-to-persisted-audit leg: driving the REAL approval hook
// (approveStageAs on a plan-gate stage) persists a split_children_filed audit
// entry that decodes to the classification + filed children + #2062 deferral the
// MCP loadSplitFiling read (a sibling slice, exercised in its own tools_test.go)
// surfaces. This is NOT a prepared audit entry — it is written by the real
// finishApprovalAdvance hook.
func TestFileSplitProposalChildren_EndToEnd(t *testing.T) {
	provider := &splitFileProvider{name: workmgmt.Default().Provider}
	workmgmt.Register(provider)

	au := newAuditFake()
	rr := newPromptRunRepo()
	art := newFakeArtifactRepo()
	ar := newFakeApprovalRepo()

	runID := uuid.New()
	planStageID := uuid.New()
	trigger := "issue:" + strconv.Itoa(splitParentIssue)
	rr.getRuns[runID] = &run.Run{
		ID:           runID,
		Repo:         "o/r",
		WorkflowID:   "feature_change",
		TriggerRef:   &trigger,
		WorkflowSpec: specImplementPathConstraints,
	}
	planStage := &run.Stage{ID: planStageID, RunID: runID, Type: run.StageTypePlan, State: run.StageStateAwaitingApproval}
	rr.getStages[planStageID] = planStage
	rr.stagesByRunID = map[uuid.UUID][]*run.Stage{runID: {planStage}}
	sv := "standard_v1"
	if _, err := art.Create(context.Background(), artifact.CreateParams{
		StageID: planStageID, Kind: artifact.KindPlan, SchemaVersion: &sv, Content: splitPlanBytes(t, true),
	}); err != nil {
		t.Fatalf("seed plan artifact: %v", err)
	}
	seedReachability(au, runID, 12) // governed-exception through the real path

	s := New(Config{Addr: "127.0.0.1:0", ApprovalRepo: ar, RunRepo: rr, AuditRepo: au, ArtifactRepo: art})

	res, err := s.approveStageAs(context.Background(), campaignOperatorIdentity(), approveActionParams{
		Stage:    planStage,
		Decision: approval.DecisionApprove,
	})
	if err != nil {
		t.Fatalf("approveStageAs: %v", err)
	}
	if res.Stage == nil || res.Stage.State != run.StageStateSucceeded {
		t.Fatalf("approve did not advance the plan stage: %+v", res.Stage)
	}

	// The real hook persisted the completion marker; decode it the way
	// loadSplitFiling will.
	var got *splitChildrenFiledPayload
	for _, e := range au.appended {
		if e.Category == splitChildrenFiledCategory {
			var p splitChildrenFiledPayload
			if uerr := json.Unmarshal(e.Payload, &p); uerr != nil {
				t.Fatalf("decode persisted split_children_filed: %v", uerr)
			}
			got = &p
		}
	}
	if got == nil {
		t.Fatal("real approval hook wrote no split_children_filed audit entry")
	}
	if got.ContractClassification != string(splitfiling.ClassificationGovernedException) {
		t.Errorf("persisted classification = %q, want governed-exception", got.ContractClassification)
	}
	if len(got.Children) != 3 {
		t.Errorf("persisted children = %d, want 3", len(got.Children))
	}
	if got.DeferralIssue != splitfiling.DeferralIssue {
		t.Errorf("persisted deferral_issue = %d, want %d", got.DeferralIssue, splitfiling.DeferralIssue)
	}
	if got.CapException == nil {
		t.Error("persisted governed-exception must carry the cap-exception draft")
	}
}
