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
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kuhlman-labs/fishhawk/backend/internal/approval"
	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/mcpserver"
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
	// creations records what File RETURNED for each successful call, so the
	// forge-state adoption tests (#2064) can assert an adopted child's recorded
	// url is byte-identical to the url the CREATION pass produced.
	creations []workmgmt.CreatedItem
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
	created := workmgmt.CreatedItem{
		Provider: p.name,
		Number:   num,
		URL:      fmt.Sprintf("https://github.com/o/r/issues/%d", num),
		Boarded:  true,
	}
	p.creations = append(p.creations, created)
	return &created, nil
}

// countRequestsFor reports how many File calls filed a title containing sub.
// The adoption tests assert this is EXACTLY 1 for an ordinal across two passes.
func (p *splitFileProvider) countRequestsFor(sub string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, r := range p.reqs {
		if strings.Contains(r.Item.Title, sub) {
			n++
		}
	}
	return n
}

// createdFor returns what File RETURNED for the first call whose title contains
// sub (the creation-pass record an adoption must reproduce exactly).
func (p *splitFileProvider) createdFor(t *testing.T, sub string) workmgmt.CreatedItem {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, r := range p.reqs {
		if strings.Contains(r.Item.Title, sub) {
			return p.creations[i]
		}
	}
	t.Fatalf("no creation recorded for a title containing %q", sub)
	return workmgmt.CreatedItem{}
}

// bodyFor returns the BODY actually filed for the first call whose title
// contains sub. The interleaving test seeds the adoptable child from this real
// filed body rather than a hand-built one, so the adoption is proven against
// what the implementation genuinely stamped.
func (p *splitFileProvider) bodyFor(t *testing.T, sub string) string {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, r := range p.reqs {
		if strings.Contains(r.Item.Title, sub) {
			return r.Item.Body
		}
	}
	t.Fatalf("no filed request with a title containing %q", sub)
	return ""
}

// splitQueryProvider wraps splitFileProvider with the OPTIONAL
// workmgmt.EpicChildrenQuerier capability (#2064). It is a separate type — not
// a method on splitFileProvider — precisely so the capability-LESS variant
// (the bare splitFileProvider, which gitlab/jira model) stays representable and
// TestSplitFiling_CapabilityLessProviderKeepsTodaysPath has something to
// register.
type splitQueryProvider struct {
	*splitFileProvider
	mu         sync.Mutex
	children   []workmgmt.EpicChild
	err        error
	queryCalls int
}

func (q *splitQueryProvider) EpicChildren(_ context.Context, _ workmgmt.EpicChildrenRequest) (*workmgmt.EpicChildrenResult, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.queryCalls++
	if q.err != nil {
		return nil, q.err
	}
	return &workmgmt.EpicChildrenResult{Children: q.children}, nil
}

func (q *splitQueryProvider) setChildren(children []workmgmt.EpicChild) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.children = children
}

func (q *splitQueryProvider) queries() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.queryCalls
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
	// baseURL is the httptest server's own base, needed to render an absolute
	// rel="next" Link header the real client will follow.
	baseURL string
	// listPages are the comment-thread PAGES the GET handler serves, one
	// []string of bodies per page, linked by a rel="next" Link header. nil
	// serves a single EMPTY page (today's behaviour for every pre-#2064 test).
	listPages [][]string
	// listStatus, when >= 400, makes the GET fail with that status — the m7
	// fail-OPEN branch (a listing error must still post the comment).
	listStatus int
	// listCalls counts GET requests, so a test can assert the thread was read
	// at all (and how many pages were fetched).
	listCalls int
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
	// GET the parent thread — the read half of the #2064 comment dedup. It
	// PAGINATES: page N carries listPages[N-1] and, when a further page exists,
	// a rel="next" Link header, mirroring the shape githubclient's
	// ListIssueComments follows. nil listPages serves one empty page.
	mux.HandleFunc("GET /repos/{owner}/{repo}/issues/{number}/comments",
		func(w http.ResponseWriter, r *http.Request) {
			num, _ := strconv.Atoi(r.PathValue("number"))
			rec.mu.Lock()
			rec.listCalls++
			st, pages, base := rec.listStatus, rec.listPages, rec.baseURL
			rec.mu.Unlock()
			if st >= 400 {
				w.WriteHeader(st)
				return
			}
			page := 1
			if p := r.URL.Query().Get("page"); p != "" {
				if n, err := strconv.Atoi(p); err == nil && n > 0 {
					page = n
				}
			}
			var bodies []string
			if page <= len(pages) {
				bodies = pages[page-1]
			}
			if page < len(pages) {
				w.Header().Set("Link", fmt.Sprintf(`<%s/repos/o/r/issues/%d/comments?per_page=100&page=%d>; rel="next"`,
					base, num, page+1))
			}
			out := make([]map[string]any, 0, len(bodies))
			for i, b := range bodies {
				out = append(out, map[string]any{
					"id":         page*1000 + i,
					"user":       map[string]any{"login": "fishhawk-dev[bot]"},
					"body":       b,
					"created_at": "2026-08-25T00:00:00Z",
				})
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(out)
		})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	rec.baseURL = srv.URL
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
	// planOverride, when non-nil, seeds this plan artifact instead of the
	// default splitPlanBytes(withSplitProposal). Used by the #2412 refusal tests
	// to construct a split_proposal with an over-cap lead phase by construction.
	planOverride []byte
	// withQuerier registers a provider implementing workmgmt.EpicChildrenQuerier
	// (#2064) instead of the capability-less default. Left false, the harness
	// reproduces today's exact path — which is what every pre-#2064 test asserts
	// and what gitlab/jira still get.
	withQuerier bool
	// epicChildren / epicChildrenErr seed the querier's answer. An error drives
	// the fail-CLOSED abort branch.
	epicChildren    []workmgmt.EpicChild
	epicChildrenErr error
}

// splitFilingHarness bundles the wired Server plus the seeded run coordinates.
type splitFilingHarness struct {
	s         *Server
	au        *auditFake
	rr        *promptRunRepo
	provider  *splitFileProvider
	querier   *splitQueryProvider // nil unless cfg.withQuerier
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

// snapshotTree walks root recursively and returns each entry's root-relative
// path mapped to its content (dirs map to "", keyed with a trailing slash), so
// a before/after comparison detects ANY file created, modified, or removed
// under root — including a nested .fishhawk/** write. A governed-exception draft
// committed to .fishhawk/** (which must NEVER happen: it rides the audit payload
// only) would appear here as a new or changed entry and fail the comparison.
func snapshotTree(t *testing.T, root string) map[string]string {
	t.Helper()
	snap := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return rerr
		}
		if d.IsDir() {
			snap[rel+"/"] = ""
			return nil
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		snap[rel] = string(b)
		return nil
	})
	if err != nil {
		t.Fatalf("snapshot %s: %v", root, err)
	}
	return snap
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
	var querier *splitQueryProvider
	if cfg.withQuerier {
		querier = &splitQueryProvider{splitFileProvider: provider, children: cfg.epicChildren, err: cfg.epicChildrenErr}
		workmgmt.Register(querier)
	} else {
		workmgmt.Register(provider)
	}

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
	planContent := cfg.planOverride
	if planContent == nil {
		planContent = splitPlanBytes(t, cfg.withSplitProposal)
	}
	if _, err := art.Create(context.Background(), artifact.CreateParams{
		StageID:       planStageID,
		Kind:          artifact.KindPlan,
		SchemaVersion: &sv,
		Content:       planContent,
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
	return &splitFilingHarness{s: s, au: au, rr: rr, provider: provider, querier: querier, runID: runID, planStage: planStage}
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
	// #2412 concern: the SUCCESSFUL-filing parent comment must also plainly state
	// independent landability is unverified — the same caveat every child body
	// carries, mirrored onto the parent comment.
	if !strings.Contains(c.body, "Landability is not verified") {
		t.Errorf("parent comment must mirror the landability caveat: %q", c.body)
	}
	if !strings.Contains(c.body, splitfiling.LandabilityCaveat) {
		t.Errorf("parent comment must carry the exact splitfiling.LandabilityCaveat text: %q", c.body)
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
	// E50.6 LINKAGE (#2062): the completion marker must carry the parent's repo
	// and issue number, because the parent-close watcher resolves "which parent
	// does this closed contract child belong to?" as a PURE READ of THIS
	// payload — no run, no forge call. A done-means assertion for the producer
	// half: the payload's correctness is not structurally enforced by
	// compilation, so without this a comment-only touch of split_filing.go would
	// satisfy the scope gate while the watcher matched nothing forever.
	if payload.ParentRepo != "o/r" {
		t.Errorf("parent_repo = %q, want %q (runRow.Repo verbatim)", payload.ParentRepo, "o/r")
	}
	if payload.ParentIssue != splitParentIssue {
		t.Errorf("parent_issue = %d, want %d (parsed from the run's trigger ref)", payload.ParentIssue, splitParentIssue)
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
// cap-exception draft rides the audit payload and NOTHING is written to the
// working tree — the agent-forbidden .fishhawk/** in particular — during filing.
// The hook runs with the temp dir AS the process working directory (t.Chdir), so
// a relative .fishhawk write (the location a draft committed to disk would land)
// resolves INSIDE the snapshotted root; a recursive before/after tree snapshot
// then fails on any created or modified file. This is NOT vacuous: were a
// .fishhawk write introduced, it would land under the observed root and diverge
// the snapshots.
func TestFileSplitProposalChildren_NoFilesystemWrite(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir) // a relative .fishhawk write now lands under the observed root
	fishhawkDir := dir + "/.fishhawk"
	if err := writeSentinel(fishhawkDir); err != nil {
		t.Fatalf("seed sentinel: %v", err)
	}
	before := snapshotTree(t, dir)

	h := newSplitFilingHarness(t, splitFilingConfig{
		withSplitProposal: true, withSpec: true, reachabilityDerived: 12,
	})
	h.s.fileSplitProposalChildren(context.Background(), h.planStage)

	// Positive: the governed-exception draft is delivered ON the audit payload
	// (both the SpecDiff and PRBody present) — proving it exists and is carried
	// by audit, not disk.
	payload, n := h.completionEntry(t)
	if n != 1 || payload.CapException == nil {
		t.Fatalf("governed-exception draft must ride the audit payload")
	}
	if payload.CapException.SpecDiff == "" || payload.CapException.PRBody == "" {
		t.Fatalf("cap-exception draft must carry SpecDiff+PRBody on the audit payload: %+v", payload.CapException)
	}

	// Negative: no filesystem write occurred under any path (the .fishhawk
	// sentinel included). A future edit that commits the draft to .fishhawk/**
	// would add or modify an entry here and fail this comparison.
	after := snapshotTree(t, dir)
	if !reflect.DeepEqual(before, after) {
		t.Errorf("filing wrote to the working tree; before=%v after=%v", before, after)
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

// --- #2412 over-cap-phase refusal ---

// overCapPhaseSplitPlanBytes builds a plan whose split_proposal LEAD phase
// declares leadFiles files (BY CONSTRUCTION, not by calling PhaseCapViolations)
// so that against the harness cap of 3 a leadFiles>3 phase is over cap. The
// second phase stays at 1 file (in cap).
func overCapPhaseSplitPlanBytes(t *testing.T, leadFiles int) []byte {
	t.Helper()
	leadScope := make([]plan.ScopeFile, leadFiles)
	for i := range leadScope {
		leadScope[i] = plan.ScopeFile{Path: fmt.Sprintf("backend/internal/lead/f%d.go", i), Operation: plan.FileOpModify}
	}
	p := &plan.Plan{
		PlanVersion: "standard_v1",
		Summary:     "over-cap lead phase",
		Scope:       plan.Scope{Files: []plan.ScopeFile{{Path: "backend/internal/foo/foo.go", Operation: plan.FileOpModify}}},
		Verification: plan.Verification{
			TestStrategy: "unit", RollbackPlan: "revert the PR",
			AcceptanceCriteria: []plan.AcceptanceCriterion{{ID: "ac1", Statement: "done"}},
		},
		SplitProposal: &plan.SplitProposal{
			Rationale: "split with an over-cap lead phase",
			Phases: []plan.SplitPhase{
				{Title: "expand: over-cap lead", ScopeHint: "expand", Scope: &plan.Scope{Files: leadScope}},
				{Title: "contract: small", ScopeHint: "contract", Scope: &plan.Scope{Files: []plan.ScopeFile{{Path: "backend/internal/foo/foo.go", Operation: plan.FileOpDelete}}}, DependsOn: []int{0}},
			},
		},
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	return b
}

// refusalEntries decodes every split_filing_refused payload the audit fake
// captured FOR THIS RUN, and how many were appended. It filters on RunID as well
// as Category (#2412 fix-up, condition 1's "wrong RUN association" failure mode):
// a hook that wrote the marker under a DIFFERENT run must not be counted here, so
// the committed-state assertions stay decisive on run association and not only on
// category. A refusal entry appended under any other run is a hard failure.
func (h *splitFilingHarness) refusalEntries(t *testing.T) ([]splitFilingRefusedPayload, int) {
	t.Helper()
	var out []splitFilingRefusedPayload
	for _, e := range h.au.appended {
		if e.Category != splitFilingRefusedCategory {
			continue
		}
		if e.RunID != h.runID {
			t.Errorf("split_filing_refused entry written under run %s, want %s (wrong-run association)", e.RunID, h.runID)
			continue
		}
		var p splitFilingRefusedPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("decode split_filing_refused payload: %v", err)
		}
		out = append(out, p)
	}
	return out, len(out)
}

// TestFileSplitProposalChildren_RefusesOverCapPhase is the counterfactual vehicle
// for the refusal branch in fileSplitProposalChildren (#2412). Its control's
// effect is COMMITTED STATE (the hook is best-effort and returns no error), so it
// asserts state READ AFTER the call: the fake forge recorded ZERO issue-create
// calls, the audit holds exactly one split_filing_refused entry naming every
// offending phase and ZERO work_item_filed / split_children_filed entries, and
// exactly one parent comment naming each offending phase. Deleting the refusal
// branch files the over-cap child instead — flipping every assertion.
func TestFileSplitProposalChildren_RefusesOverCapPhase(t *testing.T) {
	inst := int64(93)
	rec, gh := newSplitCommentGitHub(t, http.StatusCreated)
	h := newSplitFilingHarness(t, splitFilingConfig{
		withSplitProposal: true, withSpec: true, reachabilityDerived: 2,
		installID: &inst, github: gh,
		planOverride: overCapPhaseSplitPlanBytes(t, 4), // lead phase 4 files > cap 3
	})
	h.s.fileSplitProposalChildren(context.Background(), h.planStage)

	// ZERO forge issue-create calls.
	if len(h.provider.reqs) != 0 {
		t.Errorf("refusal must file ZERO children, filed %d", len(h.provider.reqs))
	}
	// Exactly one split_filing_refused entry naming the offending lead phase.
	refusals, n := h.refusalEntries(t)
	if n != 1 {
		t.Fatalf("wrote %d split_filing_refused entries, want exactly 1", n)
	}
	ref := refusals[0]
	if ref.Cap != 3 {
		t.Errorf("refusal cap = %d, want 3", ref.Cap)
	}
	if len(ref.Phases) != 1 || ref.Phases[0].Index != 0 || ref.Phases[0].DeclaredCount != 4 {
		t.Errorf("refusal phases = %+v, want one entry index=0 declared=4", ref.Phases)
	}
	if !strings.Contains(ref.Phases[0].Title, "over-cap lead") {
		t.Errorf("refusal phase title = %q, want the offending phase title", ref.Phases[0].Title)
	}
	// ZERO work_item_filed markers and NO split_children_filed completion marker.
	if got := h.childMarkerCount(t); got != 0 {
		t.Errorf("refusal wrote %d work_item_filed markers, want 0", got)
	}
	if _, cn := h.completionEntry(t); cn != 0 {
		t.Errorf("refusal wrote %d split_children_filed entries, want 0", cn)
	}
	// Exactly one parent comment naming the offending phase and stating no
	// children were filed.
	if len(rec.comments) != 1 {
		t.Fatalf("posted %d parent comments, want 1", len(rec.comments))
	}
	c := rec.comments[0]
	if c.issue != splitParentIssue {
		t.Errorf("refusal comment posted on issue %d, want %d", c.issue, splitParentIssue)
	}
	if !strings.Contains(c.body, "over-cap lead") || !strings.Contains(c.body, "No children were filed") {
		t.Errorf("refusal comment must name the offending phase and state no children filed: %q", c.body)
	}
	if !strings.Contains(c.body, "irreducible") {
		t.Errorf("refusal comment must offer the irreducible remedy: %q", c.body)
	}
}

// TestFileSplitProposalChildren_PriorRefusalIsNoOp pins the refusal dedup
// (#2412): a re-approval of a still-over-cap proposal (a prior split_filing_refused
// marker present) neither duplicates the marker nor re-comments.
func TestFileSplitProposalChildren_PriorRefusalIsNoOp(t *testing.T) {
	inst := int64(94)
	rec, gh := newSplitCommentGitHub(t, http.StatusCreated)
	h := newSplitFilingHarness(t, splitFilingConfig{
		withSplitProposal: true, withSpec: true, reachabilityDerived: 2,
		installID: &inst, github: gh,
		planOverride: overCapPhaseSplitPlanBytes(t, 4),
	})
	h.s.fileSplitProposalChildren(context.Background(), h.planStage) // first refusal
	h.s.fileSplitProposalChildren(context.Background(), h.planStage) // re-approval

	if _, n := h.refusalEntries(t); n != 1 {
		t.Errorf("re-approval must not duplicate the refusal marker, got %d", n)
	}
	if len(rec.comments) != 1 {
		t.Errorf("re-approval must not re-post the refusal comment, got %d", len(rec.comments))
	}
	if len(h.provider.reqs) != 0 {
		t.Errorf("re-approval must still file zero children, got %d", len(h.provider.reqs))
	}
}

// TestFileSplitProposalChildren_RefusalMarkerAppendFailureAborts pins the
// hard-prerequisite marker append (#2412): if the refusal marker append fails,
// the hook posts NO parent comment (never a comment claiming a refusal the audit
// does not record) and files no children.
func TestFileSplitProposalChildren_RefusalMarkerAppendFailureAborts(t *testing.T) {
	inst := int64(95)
	rec, gh := newSplitCommentGitHub(t, http.StatusCreated)
	h := newSplitFilingHarness(t, splitFilingConfig{
		withSplitProposal: true, withSpec: true, reachabilityDerived: 2,
		installID: &inst, github: gh,
		planOverride: overCapPhaseSplitPlanBytes(t, 4),
	})
	h.au.appendErrCategory = splitFilingRefusedCategory // the refusal marker append fails

	h.s.fileSplitProposalChildren(context.Background(), h.planStage)

	if len(rec.comments) != 0 {
		t.Errorf("marker-append failure must post no refusal comment, posted %d", len(rec.comments))
	}
	if len(h.provider.reqs) != 0 {
		t.Errorf("refusal path must file zero children, filed %d", len(h.provider.reqs))
	}
}

// TestFileSplitProposalChildren_UnresolvedCapStillFiles pins the fail-open branch
// (#2412): with no implement stage (unresolved cap → capFiles=0), PhaseCapViolations
// returns nil, so an over-cap-phase proposal files every child exactly as before —
// the refusal must never fire on an unresolved cap.
func TestFileSplitProposalChildren_UnresolvedCapStillFiles(t *testing.T) {
	h := newSplitFilingHarness(t, splitFilingConfig{
		withSplitProposal: true, // withSpec:false → cap unresolved
		planOverride:      overCapPhaseSplitPlanBytes(t, 4),
	})
	h.s.fileSplitProposalChildren(context.Background(), h.planStage)

	if _, n := h.refusalEntries(t); n != 0 {
		t.Errorf("unresolved cap must not refuse, wrote %d refusal entries", n)
	}
	if len(h.provider.reqs) != 2 {
		t.Errorf("unresolved cap must file every child (2 phases), filed %d", len(h.provider.reqs))
	}
	if _, cn := h.completionEntry(t); cn != 1 {
		t.Errorf("unresolved cap must write one completion marker, wrote %d", cn)
	}
}

// TestFileSplitProposalChildren_AllPhasesInCapUnchanged is the control that a
// proposal whose phases are ALL within the cap files every child exactly as today
// and writes no refusal marker (#2412). The default splitPlanBytes carries 3
// single-file phases against cap 3.
func TestFileSplitProposalChildren_AllPhasesInCapUnchanged(t *testing.T) {
	h := newSplitFilingHarness(t, splitFilingConfig{
		withSplitProposal: true, withSpec: true, reachabilityDerived: 2,
	})
	h.s.fileSplitProposalChildren(context.Background(), h.planStage)

	if _, n := h.refusalEntries(t); n != 0 {
		t.Errorf("all-in-cap proposal must write no refusal marker, wrote %d", n)
	}
	if len(h.provider.reqs) != 3 {
		t.Errorf("all-in-cap proposal must file every child (3), filed %d", len(h.provider.reqs))
	}
	if _, cn := h.completionEntry(t); cn != 1 {
		t.Errorf("all-in-cap proposal must write one completion marker, wrote %d", cn)
	}
}

// categoryListErrAuditRepo wraps an auditFake and fails ListForRunByCategory ONLY
// for one category, leaving every other read (and every append) intact. It lets a
// test exercise refuseSplitFilingOverCapPhase's dedup-list error branch in
// isolation: the top-of-hook split_children_filed dedup list still succeeds (so
// the hook reaches the refusal), and only the refusal's own split_filing_refused
// dedup list fails.
type categoryListErrAuditRepo struct {
	*auditFake
	failCategory string
}

func (r *categoryListErrAuditRepo) ListForRunByCategory(ctx context.Context, runID uuid.UUID, category string) ([]*audit.Entry, error) {
	if category == r.failCategory {
		return nil, errors.New("audit: injected ListForRunByCategory error for " + category)
	}
	return r.auditFake.ListForRunByCategory(ctx, runID, category)
}

// TestFileSplitProposalChildren_RefusalDedupListError_SilentReturn pins the
// refusal dedup-list failure branch (#2412 fix-up, review concern): when
// refuseSplitFilingOverCapPhase's ListForRunByCategory(split_filing_refused) read
// errors, the hook logs and returns WITHOUT writing a refusal marker, posting a
// parent comment, or filing any child — the same fail-closed posture as the
// marker-append failure. The counterfactual: delete the `if err != nil { return }`
// guard and the branch would fall through to append a marker (n==1), flipping the
// n==0 assertion below.
func TestFileSplitProposalChildren_RefusalDedupListError_SilentReturn(t *testing.T) {
	inst := int64(96)
	rec, gh := newSplitCommentGitHub(t, http.StatusCreated)
	h := newSplitFilingHarness(t, splitFilingConfig{
		withSplitProposal: true, withSpec: true, reachabilityDerived: 2,
		installID: &inst, github: gh,
		planOverride: overCapPhaseSplitPlanBytes(t, 4),
	})
	// Swap in an audit repo that fails ONLY the refusal dedup read, so the hook
	// reaches the refusal branch (its split_children_filed dedup still succeeds)
	// and errors on the split_filing_refused list.
	h.s.cfg.AuditRepo = &categoryListErrAuditRepo{auditFake: h.au, failCategory: splitFilingRefusedCategory}

	h.s.fileSplitProposalChildren(context.Background(), h.planStage)

	// No refusal marker appended (the dedup list errored before the append).
	if _, n := h.refusalEntries(t); n != 0 {
		t.Errorf("dedup-list error must write NO refusal marker, wrote %d", n)
	}
	// No parent comment (the append never happened, so the comment never posts).
	if len(rec.comments) != 0 {
		t.Errorf("dedup-list error must post no parent comment, posted %d", len(rec.comments))
	}
	// And zero children filed (refusal path files none regardless).
	if len(h.provider.reqs) != 0 {
		t.Errorf("dedup-list error must file zero children, filed %d", len(h.provider.reqs))
	}
}

// TestSplitFilingRefused_EndToEndThroughGetPlan is operator binding condition 1's
// single end-to-end test (#2412 fix-up, review concerns 2+3): it drives the REAL
// on-approval hook (approveStageAs -> finishApprovalAdvance ->
// fileSplitProposalChildren -> refuseSplitFilingOverCapPhase) to WRITE the
// split_filing_refused marker, then reads it back through the REAL
// fishhawk_get_plan path (mcpserver's getPlan/loadSplitFilingRefusal resolver,
// driven as a real MCP tool call over the SDK's in-memory transport against the
// server's live HTTP audit endpoint). One test crosses all three seams the
// condition named on the ACTUAL production code, not two legs meeting on a shared
// const:
//
//   - WRONG CATEGORY: the hook writes under splitFilingRefusedCategory; the
//     resolver queries its own "split_filing_refused" literal. A drift surfaces
//     nothing and fails here.
//   - WRONG RUN ASSOCIATION: getPlan is called with THIS run's id and the audit
//     endpoint filters by run, so a hook that wrote the marker under a different
//     run surfaces nothing. A distractor run seeded with its OWN refusal (a
//     different declared count) proves the read does not leak across runs.
//   - PAYLOAD-SHAPE DRIFT: the hook marshals splitFilingRefusedPayload; the
//     resolver decodes planSplitFilingRefused. A json-tag mismatch drops the
//     phases and fails the assertions.
//
// Unlike the sibling mcpserver TestGetPlan_SplitFilingRefusedRoundTrip (which
// seeds the audit entry directly to compile-lock the payload/category shape), this
// test never hand-seeds the refusal — the real hook produces it.
func TestSplitFilingRefused_EndToEndThroughGetPlan(t *testing.T) {
	ctx := context.Background()

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
		WorkflowSpec: specImplementPathConstraints, // resolves the implement cap = 3
	}
	planStage := &run.Stage{ID: planStageID, RunID: runID, Type: run.StageTypePlan, State: run.StageStateAwaitingApproval}
	rr.getStages[planStageID] = planStage
	rr.stagesByRunID = map[uuid.UUID][]*run.Stage{runID: {planStage}}
	sv := "standard_v1"
	if _, err := art.Create(ctx, artifact.CreateParams{
		StageID: planStageID, Kind: artifact.KindPlan, SchemaVersion: &sv,
		Content: overCapPhaseSplitPlanBytes(t, 4), // lead phase 4 files > cap 3
	}); err != nil {
		t.Fatalf("seed plan artifact: %v", err)
	}

	// A DISTRACTOR run with its OWN refusal (a DIFFERENT declared count): if the
	// getPlan read leaked across runs, we would see this count instead.
	distractRun := uuid.New()
	distractPayload, _ := json.Marshal(splitFilingRefusedPayload{
		Reason: "distractor", Cap: 3,
		Phases: []splitFilingRefusedPhase{{Index: 0, Title: "distractor phase", DeclaredCount: 99}},
	})
	au.seeded = append(au.seeded, &audit.Entry{
		RunID: &distractRun, Category: splitFilingRefusedCategory, Payload: distractPayload,
	})

	s := New(Config{Addr: "127.0.0.1:0", ApprovalRepo: ar, RunRepo: rr, AuditRepo: au, ArtifactRepo: art})

	// Drive the REAL approval hook. Approval succeeds (the top-level scope is in
	// cap); the best-effort hook then REFUSES to file the over-cap split.
	res, err := s.approveStageAs(ctx, campaignOperatorIdentity(), approveActionParams{
		Stage:    planStage,
		Decision: approval.DecisionApprove,
	})
	if err != nil {
		t.Fatalf("approveStageAs: %v", err)
	}
	if res.Stage == nil || res.Stage.State != run.StageStateSucceeded {
		t.Fatalf("approve did not advance the plan stage: %+v", res.Stage)
	}

	// Serve the live backend and read the refusal back through the REAL
	// fishhawk_get_plan tool over the SDK's in-memory transport.
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	mcpSrv := mcpserver.NewServer(mcpserver.Config{BackendURL: ts.URL, APIToken: "fhk_unused"})
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	ss, err := mcpSrv.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("mcp server connect: %v", err)
	}
	defer ss.Close()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "split-e2e", Version: "0"}, nil).
		Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("mcp client connect: %v", err)
	}
	defer cs.Close()

	result, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "fishhawk_get_plan",
		Arguments: map[string]any{"run_id": runID.String()},
	})
	if err != nil {
		t.Fatalf("CallTool fishhawk_get_plan: %v", err)
	}
	if result.IsError {
		t.Fatalf("fishhawk_get_plan returned error: %+v", result.Content)
	}

	raw, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured content: %v", err)
	}
	var out struct {
		Status      string `json:"status"`
		SplitFiling *struct {
			Children []any `json:"children"`
			Refused  *struct {
				Cap    int `json:"cap"`
				Phases []struct {
					Index         int    `json:"index"`
					Title         string `json:"title"`
					DeclaredCount int    `json:"declared_count"`
				} `json:"phases"`
			} `json:"refused"`
		} `json:"split_filing"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode get_plan output: %v\nraw: %s", err, raw)
	}

	if out.Status != "available" {
		t.Fatalf("status = %q, want available\nraw: %s", out.Status, raw)
	}
	if out.SplitFiling == nil || out.SplitFiling.Refused == nil {
		t.Fatalf("split_filing.refused not surfaced through the real getPlan path\nraw: %s", raw)
	}
	ref := out.SplitFiling.Refused
	if ref.Cap != 3 {
		t.Errorf("refused cap = %d, want 3", ref.Cap)
	}
	if len(ref.Phases) != 1 || ref.Phases[0].Index != 0 || ref.Phases[0].DeclaredCount != 4 {
		t.Errorf("refused phases = %+v, want one entry index=0 declared=4 (this run's real refusal, not the distractor's 99)", ref.Phases)
	}
	if len(ref.Phases) == 1 && !strings.Contains(ref.Phases[0].Title, "over-cap lead") {
		t.Errorf("refused phase title = %q, want the offending lead phase", ref.Phases[0].Title)
	}
	if len(out.SplitFiling.Children) != 0 {
		t.Errorf("a refusal must carry no filed children, got %+v", out.SplitFiling.Children)
	}
}

// ---------------------------------------------------------------------------
// #2064 / E50.7 — forge-state (query-before-file) idempotency.
//
// The window this closes: provider.File SUCCEEDS and the durable per-ordinal
// work_item_filed marker append then FAILS, so a re-approval has no record of
// the ordinal and re-files a DUPLICATE child. Every case below drives the REAL
// fileSplitProposalChildren hook.
// ---------------------------------------------------------------------------

// splitAdoptableChild builds an EpicChild bearing the key for phaseIndex of
// runID. Bodies are stamped by the SHIPPED stamper and the URL is a GitHub
// ENTERPRISE SERVER host, so an adoption path that composed a github.com url
// instead of carrying the forge's own through is observable.
func splitAdoptableChild(runID uuid.UUID, phaseIndex, number int) workmgmt.EpicChild {
	key := splitfiling.SplitChildKey(runID.String(), phaseIndex)
	return workmgmt.EpicChild{
		Number: number,
		Title:  fmt.Sprintf("phase %d child", phaseIndex),
		Body:   workmgmt.StampIdempotencyKey("## Summary\n\nalready filed\n", key),
		URL:    fmt.Sprintf("https://ghe.example.com/o/r/issues/%d", number),
	}
}

// splitAllAdoptable builds an adoptable child for every phase of the default
// 3-phase proposal, so a pass can adopt the whole set and file ZERO children.
func splitAllAdoptable(runID uuid.UUID) []workmgmt.EpicChild {
	return []workmgmt.EpicChild{
		splitAdoptableChild(runID, 0, 501),
		splitAdoptableChild(runID, 1, 502),
		splitAdoptableChild(runID, 2, 503),
	}
}

// splitChildMarkers decodes every split-filing work_item_filed resume marker
// the audit fake captured, keyed by phase index.
func splitChildMarkers(t *testing.T, h *splitFilingHarness) map[int]splitChildFiledMarker {
	t.Helper()
	out := map[int]splitChildFiledMarker{}
	for _, e := range h.au.appended {
		if e.Category != categoryWorkItemFiled {
			continue
		}
		var m splitChildFiledMarker
		if json.Unmarshal(e.Payload, &m) == nil && m.SplitPhaseIndex != nil {
			out[*m.SplitPhaseIndex] = m
		}
	}
	return out
}

// TestSplitFiling_MarkerAppendFails_ReapprovalAdoptsInsteadOfRefiling is the
// DONE-MEANS behavioural test: it injects the exact interleaving that creates
// the duplicate — File succeeds, the durable marker append fails — then
// re-approves and asserts the ordinal is ADOPTED, not re-filed.
//
// The adoptable child is seeded from the body the implementation ACTUALLY
// filed on pass 1 (provider.bodyFor), not a hand-built one, so the adoption is
// proven against what the code genuinely stamped end to end: key minted in the
// hook -> stamped by workmgmt.Apply -> carried across the provider seam ->
// read back off the children query -> matched.
//
// Counterfactual c1: deleting the adoption branch in split_filing.go reddens
// the creation-count assertion.
func TestSplitFiling_MarkerAppendFails_ReapprovalAdoptsInsteadOfRefiling(t *testing.T) {
	inst := int64(77)
	rec, gh := newSplitCommentGitHub(t, http.StatusCreated)
	h := newSplitFilingHarness(t, splitFilingConfig{
		withSplitProposal: true, withSpec: true, reachabilityDerived: 2,
		installID: &inst, github: gh, withQuerier: true,
	})

	// PASS 1: every work_item_filed append fails, so the first child is created
	// on the forge and its durable record is lost.
	h.au.appendErrCategory = categoryWorkItemFiled
	h.s.fileSplitProposalChildren(context.Background(), h.planStage)

	if h.provider.calls != 1 {
		t.Fatalf("pass 1 filed %d children, want 1 (abort after the lost marker)", h.provider.calls)
	}
	if _, n := h.completionEntry(t); n != 0 {
		t.Fatalf("pass 1 wrote %d completion markers, want 0", n)
	}
	if len(rec.comments) != 0 {
		t.Fatalf("pass 1 posted %d parent comments, want 0", len(rec.comments))
	}
	pass1Created := h.provider.createdFor(t, "expand")
	pass1Body := h.provider.bodyFor(t, "expand")

	// The filed body carries the ordinal-0 key — the durable handle the next
	// pass will find on the forge.
	wantKey := splitfiling.SplitChildKey(h.runID.String(), 0)
	if !workmgmt.BodyHasIdempotencyKey(pass1Body, wantKey) {
		t.Fatalf("filed child body carries no idempotency marker for ordinal 0:\n%s", pass1Body)
	}

	// PASS 2: the marker append recovers, and the forge now returns the child
	// pass 1 created — with the body pass 1 actually filed.
	h.au.appendErrCategory = ""
	h.querier.setChildren([]workmgmt.EpicChild{{
		Number: pass1Created.Number,
		Title:  "[E2100.1] expand: add NewFoo alongside Foo",
		Body:   pass1Body,
		URL:    pass1Created.URL,
	}})
	h.s.fileSplitProposalChildren(context.Background(), h.planStage)

	// (i) EXACTLY ONE creation for that ordinal across BOTH passes.
	if got := h.provider.countRequestsFor("expand"); got != 1 {
		t.Errorf("ordinal 0 was filed %d times across two passes, want exactly 1", got)
	}
	if h.provider.calls != 3 {
		t.Errorf("total File calls = %d, want 3 (1 on pass 1 + 2 un-adoptable ordinals on pass 2)", h.provider.calls)
	}
	// The query ran exactly once per pass — it is a single lookup, not per-ordinal.
	if got := h.querier.queries(); got != 2 {
		t.Errorf("EpicChildren called %d times across two passes, want 2 (once each)", got)
	}

	// (ii) the durable per-ordinal record exists after the second pass.
	markers := splitChildMarkers(t, h)
	m0, ok := markers[0]
	if !ok {
		t.Fatalf("no durable work_item_filed marker for ordinal 0 after adoption; markers = %+v", markers)
	}
	if len(markers) != 3 {
		t.Errorf("durable markers = %d, want 3", len(markers))
	}

	// (iv) the URL recorded on the ADOPTION pass is byte-identical to the URL
	// recorded on the CREATION pass. A composed or empty adopted url fails here.
	if m0.CreatedURL != pass1Created.URL {
		t.Errorf("adopted url = %q, want the creation-pass url %q", m0.CreatedURL, pass1Created.URL)
	}
	if m0.CreatedNumber != pass1Created.Number {
		t.Errorf("adopted number = %d, want %d", m0.CreatedNumber, pass1Created.Number)
	}

	// The completion marker carries the same url for that child.
	payload, n := h.completionEntry(t)
	if n != 1 {
		t.Fatalf("completion markers = %d, want 1", n)
	}
	var found bool
	for _, c := range payload.Children {
		if c.PhaseIndex == 0 {
			found = true
			if c.URL != pass1Created.URL || c.Number != pass1Created.Number {
				t.Errorf("completion child[0] = %+v, want number %d url %q", c, pass1Created.Number, pass1Created.URL)
			}
		}
	}
	if !found {
		t.Errorf("completion marker has no entry for phase 0: %+v", payload.Children)
	}

	// (iii) exactly ONE parent acceptance-carrier comment across both passes.
	if len(rec.comments) != 1 {
		t.Errorf("posted %d parent comments across two passes, want exactly 1", len(rec.comments))
	}
}

// TestSplitFiling_QueryErrorAbortsBeforeAnyCreation is failure mode m1: the
// children query FAILS, so the pass aborts before ANY creation and leaves the
// run resumable. Filing blind on an unreadable child set is what creates the
// duplicate, so an unknown forge state must never be read as an empty one.
//
// Counterfactual c2: deleting the query-error abort reddens the creation count.
func TestSplitFiling_QueryErrorAbortsBeforeAnyCreation(t *testing.T) {
	inst := int64(77)
	rec, gh := newSplitCommentGitHub(t, http.StatusCreated)
	h := newSplitFilingHarness(t, splitFilingConfig{
		withSplitProposal: true, withSpec: true, reachabilityDerived: 2,
		installID: &inst, github: gh, withQuerier: true,
		epicChildrenErr: errors.New("epic children: injected 502"),
	})
	h.s.fileSplitProposalChildren(context.Background(), h.planStage)

	if h.provider.calls != 0 {
		t.Fatalf("filed %d children after a query error, want 0", h.provider.calls)
	}
	if _, n := h.completionEntry(t); n != 0 {
		t.Errorf("wrote %d completion markers after a query error, want 0", n)
	}
	if got := len(splitChildMarkers(t, h)); got != 0 {
		t.Errorf("wrote %d durable child markers, want 0", got)
	}
	if len(rec.comments) != 0 {
		t.Errorf("posted %d parent comments, want 0", len(rec.comments))
	}
}

// TestSplitFiling_QueryDegradeBranches covers failure modes m2, m3 and m4 —
// the querier-capable cases where the query SUCCEEDS but adopts nothing, so
// filing proceeds exactly as before. Each non-matching key is minted from a
// DIFFERENT run or ordinal (definitionally non-matching, seeded by
// construction) rather than by calling the detector in the fixture.
func TestSplitFiling_QueryDegradeBranches(t *testing.T) {
	otherRun := uuid.New()
	for _, tc := range []struct {
		name     string
		children func(runID uuid.UUID) []workmgmt.EpicChild
	}{
		{
			// m2: query returns EMPTY.
			name:     "empty_result",
			children: func(uuid.UUID) []workmgmt.EpicChild { return nil },
		},
		{
			// m3: a child that is NOT this run's, plus one keyed for an ordinal
			// of a DIFFERENT run.
			name: "non_matching_child",
			children: func(uuid.UUID) []workmgmt.EpicChild {
				return []workmgmt.EpicChild{
					{Number: 900, Title: "unrelated", Body: "## Summary\n\nunrelated\n", URL: "https://ghe.example.com/o/r/issues/900"},
					splitAdoptableChild(otherRun, 0, 901),
				}
			},
		},
		{
			// m4: near-miss keys in BOTH directions. #902 bears a key that is a
			// strict PREFIX of ordinal 0's key; #903 bears one that strictly
			// EXTENDS it. Both directions are needed for this case to
			// DISCRIMINATE: a substring matcher looking for the full key finds
			// nothing in the shorter one, but DOES find it inside the longer
			// one and wrongly adopts #903 (counterfactual c3).
			name: "prefix_near_miss",
			children: func(runID uuid.UUID) []workmgmt.EpicChild {
				full := splitfiling.SplitChildKey(runID.String(), 0)
				return []workmgmt.EpicChild{
					{
						Number: 902,
						Title:  "prefix near-miss",
						Body:   workmgmt.StampIdempotencyKey("## Summary\n", full[:len(full)-4]),
						URL:    "https://ghe.example.com/o/r/issues/902",
					},
					{
						Number: 903,
						Title:  "extension near-miss",
						Body:   workmgmt.StampIdempotencyKey("## Summary\n", full+"deadbeef"),
						URL:    "https://ghe.example.com/o/r/issues/903",
					},
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inst := int64(77)
			rec, gh := newSplitCommentGitHub(t, http.StatusCreated)
			h := newSplitFilingHarness(t, splitFilingConfig{
				withSplitProposal: true, withSpec: true, reachabilityDerived: 2,
				installID: &inst, github: gh, withQuerier: true,
			})
			h.querier.setChildren(tc.children(h.runID))
			h.s.fileSplitProposalChildren(context.Background(), h.planStage)

			if h.provider.calls != 3 {
				t.Fatalf("filed %d children, want 3 (nothing adoptable)", h.provider.calls)
			}
			if got := h.querier.queries(); got != 1 {
				t.Errorf("EpicChildren called %d times, want exactly 1", got)
			}
			if _, n := h.completionEntry(t); n != 1 {
				t.Errorf("completion markers = %d, want 1", n)
			}
			if len(rec.comments) != 1 {
				t.Errorf("parent comments = %d, want 1", len(rec.comments))
			}
		})
	}
}

// TestSplitFiling_ClosedChildBearingKeyIsAdopted is failure mode m5: a child
// CLOSED before the re-approval was still FILED, so re-filing it would
// duplicate it. EpicChildren applies no state filter, so it is still returned
// and still adoptable. This test fails if a state filter is ever introduced.
func TestSplitFiling_ClosedChildBearingKeyIsAdopted(t *testing.T) {
	inst := int64(77)
	_, gh := newSplitCommentGitHub(t, http.StatusCreated)
	h := newSplitFilingHarness(t, splitFilingConfig{
		withSplitProposal: true, withSpec: true, reachabilityDerived: 2,
		installID: &inst, github: gh, withQuerier: true,
	})
	closed := splitAllAdoptable(h.runID)
	for i := range closed {
		closed[i].Complete = true // closed-and-completed
	}
	h.querier.setChildren(closed)
	h.s.fileSplitProposalChildren(context.Background(), h.planStage)

	if h.provider.calls != 0 {
		t.Fatalf("filed %d children, want 0 (all three adoptable, though closed)", h.provider.calls)
	}
	markers := splitChildMarkers(t, h)
	if len(markers) != 3 {
		t.Fatalf("durable markers = %d, want 3", len(markers))
	}
	if markers[0].CreatedNumber != 501 || markers[0].CreatedURL != "https://ghe.example.com/o/r/issues/501" {
		t.Errorf("adopted ordinal 0 = %+v, want #501 with the forge's own GHE url", markers[0])
	}
	if _, n := h.completionEntry(t); n != 1 {
		t.Errorf("completion markers = %d, want 1", n)
	}
}

// TestSplitFiling_CapabilityLessProviderKeepsTodaysPath is failure mode m6 and
// the named RESIDUAL: gitlab and jira do not implement EpicChildrenQuerier
// (backend/internal/workmgmt/gitlab/provider.go), so for them the window stays
// open exactly as it is today — fail-OPEN by design. It fails if the query is
// ever made a hard prerequisite for filing.
func TestSplitFiling_CapabilityLessProviderKeepsTodaysPath(t *testing.T) {
	inst := int64(77)
	rec, gh := newSplitCommentGitHub(t, http.StatusCreated)
	h := newSplitFilingHarness(t, splitFilingConfig{
		withSplitProposal: true, withSpec: true, reachabilityDerived: 2,
		installID: &inst, github: gh, // withQuerier deliberately false
	})
	if h.querier != nil {
		t.Fatal("fixture error: this case must register a capability-LESS provider")
	}
	h.s.fileSplitProposalChildren(context.Background(), h.planStage)

	if h.provider.calls != 3 {
		t.Fatalf("filed %d children, want 3 (today's exact path)", h.provider.calls)
	}
	if got := len(splitChildMarkers(t, h)); got != 3 {
		t.Errorf("durable markers = %d, want 3", got)
	}
	if _, n := h.completionEntry(t); n != 1 {
		t.Errorf("completion markers = %d, want 1", n)
	}
	if len(rec.comments) != 1 {
		t.Errorf("parent comments = %d, want 1", len(rec.comments))
	}
	// The children the filed bodies carry are still KEYED — stamping is not
	// gated on the capability, so a capability-less provider that later gains
	// the query finds the markers already in place.
	body := h.provider.bodyFor(t, "expand")
	if !workmgmt.BodyHasIdempotencyKey(body, splitfiling.SplitChildKey(h.runID.String(), 0)) {
		t.Errorf("capability-less filing did not stamp the child body:\n%s", body)
	}
}

// TestSplitFiling_AdoptionMarkerAppendFailureAborts is failure mode m8: the
// durable marker append is a HARD prerequisite on the ADOPTION path too, not
// just the filed one. A lost marker aborts the pass with no completion marker
// rather than proceeding on a durable state the run cannot resume from.
func TestSplitFiling_AdoptionMarkerAppendFailureAborts(t *testing.T) {
	inst := int64(77)
	rec, gh := newSplitCommentGitHub(t, http.StatusCreated)
	h := newSplitFilingHarness(t, splitFilingConfig{
		withSplitProposal: true, withSpec: true, reachabilityDerived: 2,
		installID: &inst, github: gh, withQuerier: true,
	})
	h.querier.setChildren(splitAllAdoptable(h.runID))
	h.au.appendErrCategory = categoryWorkItemFiled
	h.s.fileSplitProposalChildren(context.Background(), h.planStage)

	if h.provider.calls != 0 {
		t.Errorf("filed %d children, want 0 (everything was adoptable)", h.provider.calls)
	}
	if _, n := h.completionEntry(t); n != 0 {
		t.Errorf("wrote %d completion markers after a lost adoption marker, want 0", n)
	}
	if len(rec.comments) != 0 {
		t.Errorf("posted %d parent comments, want 0", len(rec.comments))
	}
}

// TestSplitFiling_DuplicateKeyTieBreakIsDeterministic is failure mode m9: two
// children bear the SAME key (the only way that happens is a prior concurrent
// re-approval — the named TOCTOU residual). The tie-break is deterministic and
// intentional: LOWEST NUMBER WINS. The second child is not reconciled.
func TestSplitFiling_DuplicateKeyTieBreakIsDeterministic(t *testing.T) {
	inst := int64(77)
	_, gh := newSplitCommentGitHub(t, http.StatusCreated)
	h := newSplitFilingHarness(t, splitFilingConfig{
		withSplitProposal: true, withSpec: true, reachabilityDerived: 2,
		installID: &inst, github: gh, withQuerier: true,
	})
	children := splitAllAdoptable(h.runID)
	// A DUPLICATE of ordinal 0, higher-numbered, listed FIRST so a
	// first-match-wins implementation picks the wrong one.
	dup := splitAdoptableChild(h.runID, 0, 777)
	h.querier.setChildren(append([]workmgmt.EpicChild{dup}, children...))
	h.s.fileSplitProposalChildren(context.Background(), h.planStage)

	if h.provider.calls != 0 {
		t.Fatalf("filed %d children, want 0", h.provider.calls)
	}
	markers := splitChildMarkers(t, h)
	if got := markers[0].CreatedNumber; got != 501 {
		t.Errorf("adopted #%d for ordinal 0, want the LOWEST-numbered #501", got)
	}
	if len(markers) != 3 {
		t.Errorf("durable markers = %d, want 3 (exactly one per ordinal)", len(markers))
	}
}

// TestSplitFiling_ParentCommentCarriesIdempotencyMarker is the OUTBOUND half of
// the comment dedup (operator binding condition). Nothing else in the battery
// proves a comment POSTED BY THIS IMPLEMENTATION is stamped: the interleaving
// case aborts before any comment, and the page-2 / CRLF fixtures SEED an
// already-keyed comment, so they only prove the READER finds a marker someone
// else put there. If the outbound stamp were dropped, the dedup would silently
// never fire in production and every one of those cases would still pass.
//
// The assertion is on the body the FAKE SERVER RECEIVED, not on a body handed
// to a stub.
func TestSplitFiling_ParentCommentCarriesIdempotencyMarker(t *testing.T) {
	inst := int64(77)
	rec, gh := newSplitCommentGitHub(t, http.StatusCreated)
	h := newSplitFilingHarness(t, splitFilingConfig{
		withSplitProposal: true, withSpec: true, reachabilityDerived: 2,
		installID: &inst, github: gh, withQuerier: true,
	})
	h.s.fileSplitProposalChildren(context.Background(), h.planStage)

	if len(rec.comments) != 1 {
		t.Fatalf("posted %d parent comments, want 1", len(rec.comments))
	}
	key := splitfiling.SplitCommentKey(h.runID.String(), splitfiling.CommentKindAcceptance)
	if !workmgmt.BodyHasIdempotencyKey(rec.comments[0].body, key) {
		t.Fatalf("the POSTED acceptance-carrier body carries no idempotency marker for %q:\n%s",
			key, rec.comments[0].body)
	}
	// The prose survived the stamp.
	if !strings.Contains(rec.comments[0].body, "#3003") {
		t.Errorf("stamped comment lost the contract-child reference:\n%s", rec.comments[0].body)
	}
	// A refusal-keyed reader must NOT match it (the two kinds are distinct).
	if workmgmt.BodyHasIdempotencyKey(rec.comments[0].body,
		splitfiling.SplitCommentKey(h.runID.String(), splitfiling.CommentKindRefusal)) {
		t.Errorf("acceptance comment matched the REFUSAL key")
	}
	// The thread WAS read before posting (the dedup is wired, not dead code).
	if rec.listCalls == 0 {
		t.Errorf("parent thread was never listed before posting")
	}
}

// TestSplitFiling_RefusalCommentCarriesIdempotencyMarker is the same outbound
// proof for the #2412 over-cap refusal comment, which arises from the identical
// re-approval window.
func TestSplitFiling_RefusalCommentCarriesIdempotencyMarker(t *testing.T) {
	inst := int64(77)
	rec, gh := newSplitCommentGitHub(t, http.StatusCreated)
	h := newSplitFilingHarness(t, splitFilingConfig{
		withSplitProposal: true, withSpec: true,
		installID: &inst, github: gh, withQuerier: true,
		planOverride: overCapPhaseSplitPlanBytes(t, 4), // lead phase 4 files > cap 3
	})
	h.s.fileSplitProposalChildren(context.Background(), h.planStage)

	if h.provider.calls != 0 {
		t.Fatalf("filed %d children on a refusal, want 0", h.provider.calls)
	}
	if len(rec.comments) != 1 {
		t.Fatalf("posted %d refusal comments, want 1", len(rec.comments))
	}
	key := splitfiling.SplitCommentKey(h.runID.String(), splitfiling.CommentKindRefusal)
	if !workmgmt.BodyHasIdempotencyKey(rec.comments[0].body, key) {
		t.Fatalf("the POSTED refusal body carries no idempotency marker for %q:\n%s", key, rec.comments[0].body)
	}
	if !strings.Contains(rec.comments[0].body, "did **not** file") {
		t.Errorf("stamped refusal comment lost its prose:\n%s", rec.comments[0].body)
	}
}

// TestSplitFiling_CommentDedupReadsWholeThread puts the keyed comment on PAGE 2
// behind a rel="next" Link header, with page 1 carrying ONLY unrelated
// comments. The zero-new-posts assertion can therefore only hold for a reader
// that FOLLOWS the next-page link — the thing being searched for is not in the
// first place searched.
//
// Counterfactual c5: deleting the pagination follow in
// githubclient.ListIssueComments reddens this with a duplicate post.
func TestSplitFiling_CommentDedupReadsWholeThread(t *testing.T) {
	inst := int64(77)
	rec, gh := newSplitCommentGitHub(t, http.StatusCreated)
	h := newSplitFilingHarness(t, splitFilingConfig{
		withSplitProposal: true, withSpec: true, reachabilityDerived: 2,
		installID: &inst, github: gh, withQuerier: true,
	})
	key := splitfiling.SplitCommentKey(h.runID.String(), splitfiling.CommentKindAcceptance)
	rec.listPages = [][]string{
		{"first unrelated comment", "second unrelated comment"},
		{"third unrelated comment", splitfiling.StampComment("Fishhawk filed the phased children.", key)},
	}
	h.s.fileSplitProposalChildren(context.Background(), h.planStage)

	if len(rec.comments) != 0 {
		t.Fatalf("posted %d parent comments, want 0 (the keyed comment is already on page 2):\n%v",
			len(rec.comments), rec.comments)
	}
	if rec.listCalls < 2 {
		t.Errorf("thread listed %d times, want >= 2 (page 1 then page 2)", rec.listCalls)
	}
	// The filing itself still ran — the dedup suppresses the COMMENT only.
	if h.provider.calls != 3 {
		t.Errorf("filed %d children, want 3", h.provider.calls)
	}
	if _, n := h.completionEntry(t); n != 1 {
		t.Errorf("completion markers = %d, want 1", n)
	}
}

// TestSplitFiling_CommentDedupToleratesCRLF serves the keyed comment with \r\n
// line endings — the shape GitHub returns for a comment authored or edited
// through the web interface. An exact-line matcher without normalization
// re-posts a comment that is already there.
//
// Counterfactual c6: deleting the CRLF normalization in
// workmgmt.BodyHasIdempotencyKey reddens this with a duplicate post.
func TestSplitFiling_CommentDedupToleratesCRLF(t *testing.T) {
	inst := int64(77)
	rec, gh := newSplitCommentGitHub(t, http.StatusCreated)
	h := newSplitFilingHarness(t, splitFilingConfig{
		withSplitProposal: true, withSpec: true, reachabilityDerived: 2,
		installID: &inst, github: gh, withQuerier: true,
	})
	key := splitfiling.SplitCommentKey(h.runID.String(), splitfiling.CommentKindAcceptance)
	// The trailing "\n" is load-bearing: the marker is the LAST line, so without
	// it the conversion would leave the marker line with no \r at all and this
	// case would pass whether or not the normalization exists (counterfactual
	// c6 would not discriminate).
	lf := splitfiling.StampComment("Fishhawk filed the phased children.\n\nSecond paragraph.", key) + "\n"
	crlf := strings.ReplaceAll(lf, "\n", "\r\n")
	if !strings.HasSuffix(crlf, "-->\r\n") {
		t.Fatalf("fixture error: the seeded marker line does not end \\r\\n: %q", crlf)
	}
	rec.listPages = [][]string{{crlf}}

	h.s.fileSplitProposalChildren(context.Background(), h.planStage)

	if len(rec.comments) != 0 {
		t.Fatalf("posted %d parent comments, want 0 (a CRLF-bodied keyed comment is already there):\n%v",
			len(rec.comments), rec.comments)
	}
}

// TestSplitFiling_CommentListErrorPostsAnyway is failure mode m7: the comment
// path degrades fail-OPEN and DELIBERATELY OPPOSITE to the child path. A
// missing operator-facing acceptance-carrier comment is worse than a duplicated
// one; a duplicate child ISSUE is the bug being fixed. This pins the asymmetry
// rather than leaving it incidental.
func TestSplitFiling_CommentListErrorPostsAnyway(t *testing.T) {
	inst := int64(77)
	rec, gh := newSplitCommentGitHub(t, http.StatusCreated)
	h := newSplitFilingHarness(t, splitFilingConfig{
		withSplitProposal: true, withSpec: true, reachabilityDerived: 2,
		installID: &inst, github: gh, withQuerier: true,
	})
	rec.listStatus = http.StatusInternalServerError
	h.s.fileSplitProposalChildren(context.Background(), h.planStage)

	if rec.listCalls == 0 {
		t.Fatalf("the thread was never listed, so this does not exercise the listing-error branch")
	}
	if len(rec.comments) != 1 {
		t.Fatalf("posted %d parent comments after a listing error, want 1 (fail-OPEN)", len(rec.comments))
	}
	// The child path, by contrast, still completed normally — the asymmetry is
	// per-path, not a blanket posture.
	if h.provider.calls != 3 {
		t.Errorf("filed %d children, want 3", h.provider.calls)
	}
}

// TestSplitFiling_SequentialReapprovalFilesAtMostOnce is the at-most-once
// criterion, honestly scoped to SEQUENTIAL re-approval. The
// children-query-then-file pair is NOT atomic, so two CONCURRENT re-approvals
// can both observe nothing and both create — a named residual of at most one
// duplicate, deliberately not serialized. This asserts only what the code holds.
func TestSplitFiling_SequentialReapprovalFilesAtMostOnce(t *testing.T) {
	inst := int64(77)
	rec, gh := newSplitCommentGitHub(t, http.StatusCreated)
	h := newSplitFilingHarness(t, splitFilingConfig{
		withSplitProposal: true, withSpec: true, reachabilityDerived: 2,
		installID: &inst, github: gh, withQuerier: true,
	})
	// Pass 1: normal, complete filing.
	h.s.fileSplitProposalChildren(context.Background(), h.planStage)
	if h.provider.calls != 3 {
		t.Fatalf("pass 1 filed %d children, want 3", h.provider.calls)
	}
	// Pass 2 re-approval: the completion marker short-circuits at the top.
	h.s.fileSplitProposalChildren(context.Background(), h.planStage)
	if h.provider.calls != 3 {
		t.Errorf("total File calls after a sequential re-approval = %d, want 3", h.provider.calls)
	}
	if _, n := h.completionEntry(t); n != 1 {
		t.Errorf("completion markers = %d, want 1", n)
	}
	if len(rec.comments) != 1 {
		t.Errorf("parent comments = %d, want 1", len(rec.comments))
	}
}
