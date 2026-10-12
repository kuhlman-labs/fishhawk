package server

// Cross-layer done-means tests for ADR-096 D4 / #4294: the decomposed-parent
// fan-in builds the consolidated head on the CURRENT base tip, so the ADR-090
// D5 fan-in merge-candidate pass verifies the real merge candidate and a parent
// whose base has not moved since fan-in merges after exactly one pass.
//
// They cross orchestrator fan-in → forge (the real *githubclient.Client over
// commitGraph, an httptest GitHub fake that models a commit graph) → audit
// chain → the D5 hold/trigger → the runner's report endpoint → the merge gate.
//
// commitGraph's conflict model is a deliberate SIMPLIFICATION of git's content
// merge: POST /merges answers 409 when the paths touched on the two sides since
// their merge-base intersect at all (no line-level merge), 204 when head is
// already an ancestor of base, and otherwise writes a two-parent merge commit
// that touches no path of its own. Every path it does not model — contents/ in
// particular, which the consolidated review's permission-drift check reads —
// answers 404 JSON, the forge's "absent" answer.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/orchestrator"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// graphCommit is one commit: its parents and the paths it touches (a merge
// commit the fake writes touches none).
type graphCommit struct {
	parents []string
	paths   []string
}

// commitGraph is the httptest GitHub fake: branches → tip, commits → parents +
// touched paths, plus one pull request whose head tracks prHead.
type commitGraph struct {
	mu       sync.Mutex
	commits  map[string]*graphCommit
	branches map[string]string
	prNumber int
	prHead   string
	prBase   string
	merges   int
}

func newCommitGraph() *commitGraph {
	return &commitGraph{commits: map[string]*graphCommit{}, branches: map[string]string{}}
}

// commit records sha with parents and touched paths.
func (g *commitGraph) commit(sha string, parents []string, paths ...string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.commits[sha] = &graphCommit{parents: parents, paths: paths}
}

func (g *commitGraph) setBranch(name, sha string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.branches[name] = sha
}

func (g *commitGraph) tip(name string) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.branches[name]
}

// isAncestor reports whether a is reachable from b (a commit is its own
// ancestor).
func (g *commitGraph) isAncestor(a, b string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.ancestorsLocked(b)[a]
}

// resolveLocked resolves a branch name or a known commit sha.
func (g *commitGraph) resolveLocked(ref string) (string, bool) {
	if sha, ok := g.branches[ref]; ok {
		return sha, true
	}
	if _, ok := g.commits[ref]; ok {
		return ref, true
	}
	return "", false
}

func (g *commitGraph) ancestorsLocked(sha string) map[string]bool {
	seen := map[string]bool{}
	stack := []string{sha}
	for len(stack) > 0 {
		c := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[c] {
			continue
		}
		seen[c] = true
		if gc, ok := g.commits[c]; ok {
			stack = append(stack, gc.parents...)
		}
	}
	return seen
}

// onlyInLocked lists the commits reachable from head and not from base in
// topological order (parents first), head last — GitHub's three-dot compare.
func (g *commitGraph) onlyInLocked(head, base string) []string {
	exclude := g.ancestorsLocked(base)
	var out []string
	visited := map[string]bool{}
	var visit func(string)
	visit = func(c string) {
		if visited[c] || exclude[c] {
			return
		}
		visited[c] = true
		if gc, ok := g.commits[c]; ok {
			for _, p := range gc.parents {
				visit(p)
			}
		}
		out = append(out, c)
	}
	visit(head)
	return out
}

func (g *commitGraph) pathsLocked(shas []string) map[string]bool {
	out := map[string]bool{}
	for _, c := range shas {
		if gc, ok := g.commits[c]; ok {
			for _, p := range gc.paths {
				out[p] = true
			}
		}
	}
	return out
}

func graphJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func graphNotFound(w http.ResponseWriter) {
	graphJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
}

func (g *commitGraph) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	const prefix = "/repos/kuhlman-labs/example/"
	path, ok := strings.CutPrefix(r.URL.Path, prefix)
	if !ok {
		graphNotFound(w)
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	switch {
	case r.Method == http.MethodGet && strings.HasPrefix(path, "git/ref/heads/"):
		sha, ok := g.branches[strings.TrimPrefix(path, "git/ref/heads/")]
		if !ok {
			graphNotFound(w)
			return
		}
		graphJSON(w, http.StatusOK, map[string]any{"object": map[string]string{"sha": sha}})
	case r.Method == http.MethodPost && path == "git/refs":
		var body struct{ Ref, SHA string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		branch := strings.TrimPrefix(body.Ref, "refs/heads/")
		if _, exists := g.branches[branch]; exists {
			graphJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "Reference already exists"})
			return
		}
		g.branches[branch] = body.SHA
		graphJSON(w, http.StatusCreated, map[string]any{"ref": body.Ref, "object": map[string]string{"sha": body.SHA}})
	case r.Method == http.MethodPost && path == "merges":
		g.serveMergeLocked(w, r)
	case r.Method == http.MethodGet && strings.HasPrefix(path, "compare/"):
		g.serveCompareLocked(w, strings.TrimPrefix(path, "compare/"))
	case r.Method == http.MethodGet && path == "pulls/"+strconv.Itoa(g.prNumber) && g.prNumber > 0:
		graphJSON(w, http.StatusOK, map[string]any{
			"number": g.prNumber, "node_id": "PR_graph", "state": "open", "merged": false,
			"head": map[string]string{"sha": g.branches[g.prHead], "ref": g.prHead},
			"base": map[string]string{"ref": g.prBase},
		})
	default:
		graphNotFound(w)
	}
}

func (g *commitGraph) serveMergeLocked(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Base          string `json:"base"`
		Head          string `json:"head"`
		CommitMessage string `json:"commit_message"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	baseSHA, okBase := g.branches[body.Base]
	headSHA, okHead := g.resolveLocked(body.Head)
	if !okBase || !okHead {
		graphNotFound(w)
		return
	}
	if g.ancestorsLocked(baseSHA)[headSHA] {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	baseSide := g.pathsLocked(g.onlyInLocked(baseSHA, headSHA))
	for p := range g.pathsLocked(g.onlyInLocked(headSHA, baseSHA)) {
		if baseSide[p] {
			graphJSON(w, http.StatusConflict, map[string]string{"message": "Merge conflict"})
			return
		}
	}
	g.merges++
	sha := fmt.Sprintf("merge%d", g.merges)
	g.commits[sha] = &graphCommit{parents: []string{baseSHA, headSHA}}
	g.branches[body.Base] = sha
	graphJSON(w, http.StatusCreated, map[string]string{"sha": sha})
}

func (g *commitGraph) serveCompareLocked(w http.ResponseWriter, spec string) {
	base, head, ok := strings.Cut(spec, "...")
	if !ok {
		graphNotFound(w)
		return
	}
	baseSHA, okBase := g.resolveLocked(base)
	headSHA, okHead := g.resolveLocked(head)
	if !okBase || !okHead {
		graphNotFound(w)
		return
	}
	commits := g.onlyInLocked(headSHA, baseSHA)
	files := make([]string, 0)
	for p := range g.pathsLocked(commits) {
		files = append(files, p)
	}
	sort.Strings(files)
	commitRows := make([]map[string]string, 0, len(commits))
	for _, c := range commits {
		commitRows = append(commitRows, map[string]string{"sha": c})
	}
	fileRows := make([]map[string]any, 0, len(files))
	for _, f := range files {
		fileRows = append(fileRows, map[string]any{"filename": f, "status": "modified", "changes": 2, "patch": "@@ -1 +1 @@\n-a\n+b"})
	}
	graphJSON(w, http.StatusOK, map[string]any{"total_commits": len(commits), "commits": commitRows, "files": fileRows})
}

// baseTipHarness wires ONE *githubclient.Client over the graph fake into both
// an orchestrator (Runs = the fixture repo, Audit = the fixture audit fake)
// and the Server, plus a decomposed parent with two children: slice 1
// depends_on slice 0, so the first integration is the between-wave one.
type baseTipHarness struct {
	t            *testing.T
	graph        *commitGraph
	rr           *fanInRepo
	au           *auditFake
	s            *Server
	o            *orchestrator.Orchestrator
	parent       *run.Run
	impl         *run.Stage
	child0       *run.Run
	child1       *run.Run
	consolidated string
}

func newBaseTipHarness(t *testing.T, implState run.StageState) *baseTipHarness {
	t.Helper()
	g := newCommitGraph()
	g.commit("B0", nil, "README.md")
	g.setBranch("main", "B0")
	srv := httptest.NewServer(g)
	t.Cleanup(srv.Close)
	client := &githubclient.Client{
		BaseURL: srv.URL,
		Tokens:  &fakeTokenProvider{tok: "ghs_t"},
		HTTP:    &http.Client{Timeout: 5 * time.Second},
		AppJWT:  func() (string, error) { return "ghs_jwt", nil },
	}

	rr := &fanInRepo{orchestratorRepo: newOrchestratorRepo()}
	art := newFakeArtifactRepo()
	au := newAuditFake()
	au.stampSequence = true

	parent := rr.seedRun()
	parent.WorkflowID = "feature_change"
	parent.WorkflowSpec = fanInVerifySpec
	parent.Repo = "kuhlman-labs/example"
	inst := int64(55)
	parent.InstallationID = &inst

	planStage := rr.seedStage(parent.ID, 0, run.StageStateSucceeded)
	seedBudgetPlanArtifact(t, art, planStage.ID, &plan.Plan{
		PlanVersion:                "standard_v1",
		Summary:                    "Decomposed parent (ADR-096 D4)",
		PredictedRuntimeMinutes:    10,
		PredictedRuntimeConfidence: plan.RuntimeConfidenceMedium,
		Scope: plan.Scope{Files: []plan.ScopeFile{
			{Path: "a.go", Operation: plan.FileOpModify},
			{Path: "c.go", Operation: plan.FileOpModify},
			{Path: "d.go", Operation: plan.FileOpModify},
		}},
		Decomposition: &plan.Decomposition{
			Rationale: "two waves",
			SubPlans: []plan.SubPlanSummary{
				{Title: "slice 0", ScopeHint: "a", Scope: &plan.Scope{Files: []plan.ScopeFile{{Path: "a.go", Operation: plan.FileOpModify}}}},
				{Title: "slice 1", ScopeHint: "c", DependsOn: []int{0}, Scope: &plan.Scope{Files: []plan.ScopeFile{
					{Path: "c.go", Operation: plan.FileOpModify},
					{Path: "d.go", Operation: plan.FileOpModify},
				}}},
			},
		},
	})
	impl := rr.seedStage(parent.ID, 1, implState)
	impl.Type = run.StageTypeImplement
	review := rr.seedStage(parent.ID, 2, run.StageStateAwaitingApproval)
	review.Type = run.StageTypeReview
	review.ExecutorKind = run.ExecutorHuman

	seedChild := func(idx int, state run.State) *run.Run {
		c := rr.seedRun()
		c.Repo = parent.Repo
		c.DecomposedFrom = &parent.ID
		c.SliceIndex = &idx
		c.State = state
		return c
	}
	child0 := seedChild(0, run.StateSucceeded)
	child1 := seedChild(1, run.StateRunning)

	o := &orchestrator.Orchestrator{
		Runs: rr, GitHub: client, Audit: au, Artifacts: art, DefaultRef: "main",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	s := New(Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      rr,
		ArtifactRepo: art,
		AuditRepo:    au,
		ConcernRepo:  newFakeConcernRepo(),
		PlanReviewers: singleReviewerSet{&fakePlanReviewer{
			verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictApprove},
			model:   "claude-opus-4-8",
		}},
		GitHub:     client,
		TraceStore: &hashKeyedTraceStore{bodies: map[string][]byte{}},
	})
	h := &baseTipHarness{t: t, graph: g, rr: rr, au: au, s: s, o: o, parent: parent, impl: impl,
		child0: child0, child1: child1, consolidated: orchestrator.ConsolidatedBranch(parent.ID)}

	// Slice 0: one commit on B0.
	g.commit("S0", []string{"B0"}, "a.go")
	g.setBranch(orchestrator.SliceBranch(parent.ID, 0), "S0")
	return h
}

// integrateFirstWave runs the between-wave fan-in through the production path
// that creates the consolidated branch early (IntegrateCompletedWave, the
// sweeper's arm; approval condition C3): slice 0 succeeded, slice 1 running
// and unblocked, so the consolidated branch is created at B0 and slice 0
// merged onto it.
func (h *baseTipHarness) integrateFirstWave() {
	h.t.Helper()
	integrated, conflict, err := h.o.IntegrateCompletedWave(context.Background(), h.parent.ID)
	if err != nil || conflict != nil || !integrated {
		h.t.Fatalf("IntegrateCompletedWave = (%v, %+v, %v), want (true, nil, nil)", integrated, conflict, err)
	}
	if tip := h.graph.tip(h.consolidated); tip == "" || !h.graph.isAncestor("B0", tip) || !h.graph.isAncestor("S0", tip) {
		h.t.Fatalf("consolidated tip %q after the first wave, want B0 + slice 0", tip)
	}
}

// advanceMainAndFinishSlice1 moves main to B1 (touching mainPath) and lands
// slice 1's commit (touching slicePath) on the consolidated tip the child
// based on, then marks child 1 succeeded.
func (h *baseTipHarness) advanceMainAndFinishSlice1(mainPath, slicePath string) {
	h.t.Helper()
	h.graph.commit("B1", []string{"B0"}, mainPath)
	h.graph.setBranch("main", "B1")
	h.graph.commit("S1", []string{h.graph.tip(h.consolidated)}, slicePath)
	h.graph.setBranch(orchestrator.SliceBranch(h.parent.ID, 1), "S1")
	h.rr.mu.Lock()
	h.child1.State = run.StateSucceeded
	h.rr.mu.Unlock()
}

// TestFanInOnBaseTip_SinglePassMerge is done-means 1–4. The consolidated
// branch pre-exists at B0 from the between-wave integration and main moves to
// B1 before the final pass, so only the base fold puts B1 into the final head
// H. Then: the D5 hold triggers exactly one fan_in pass for H, the runner's
// passed report for H lands, and the merge gate ADMITS — no merge_base_behind,
// no rebase, no second base_advance pass. Neutering the fold leaves H without
// B1, and the gate answers merge_base_behind.
func TestFanInOnBaseTip_SinglePassMerge(t *testing.T) {
	h := newBaseTipHarness(t, run.StageStateSucceeded)
	ctx := context.Background()
	h.integrateFirstWave()
	h.advanceMainAndFinishSlice1("unrelated.go", "d.go")

	conflict, err := h.o.IntegrateSlices(ctx, h.parent.ID)
	if err != nil || conflict != nil {
		t.Fatalf("final IntegrateSlices = (%+v, %v), want (nil, nil)", conflict, err)
	}
	head := h.graph.tip(h.consolidated)
	// Done-means 1: the final consolidated head is built on the CURRENT tip.
	if !h.graph.isAncestor("B1", head) {
		// Errorf, not Fatalf: the run continues to the merge gate, which is
		// what turns a stale head into a forced second pass (done-means 4).
		t.Errorf("consolidated head %s does not contain main's tip B1: the fan-in integrated on the original branch point", head)
	}
	if !h.graph.isAncestor("S1", head) || !h.graph.isAncestor("S0", head) {
		t.Fatalf("consolidated head %s is missing a slice", head)
	}

	// Done-means 2: the D5 hold triggers exactly one fan_in pass for H.
	prURL := "https://github.com/kuhlman-labs/example/pull/9"
	h.rr.mu.Lock()
	h.parent.PullRequestURL = &prURL
	h.rr.mu.Unlock()
	h.graph.mu.Lock()
	h.graph.prNumber, h.graph.prHead, h.graph.prBase = 9, h.consolidated, "main"
	h.graph.mu.Unlock()

	h.s.DispatchConsolidatedReview(ctx, h.parent.ID, "main", h.consolidated)
	h.s.waitBackgroundReviews()
	rows := mcvTriggerRows(h.au)
	if len(rows) != 1 {
		t.Fatalf("stage_merge_candidate_verify_triggered = %d, want 1", len(rows))
	}
	var trig mergeCandidateVerifyTrigger
	if err := json.Unmarshal(rows[0].Payload, &trig); err != nil {
		t.Fatal(err)
	}
	if trig.ExpectedHeadSHA != head || trig.Cause != mergeCandidateCauseFanIn || trig.Branch != h.consolidated {
		t.Fatalf("trigger = %+v, want a fan_in pass for %s on %s", trig, head, h.consolidated)
	}

	// The runner reports passed for exactly H through the real report arm.
	h.s.cfg.Orchestrator = &orchestrator.Orchestrator{Runs: h.rr, ConsolidatedReview: h.s}
	sf := newSigningFake()
	h.s.cfg.SigningRepo = sf
	priv, _ := sf.issue(t, h.parent.ID)
	h.rr.mu.Lock()
	h.impl.State = run.StageStateRunning
	h.rr.mu.Unlock()
	body, _ := json.Marshal(map[string]any{
		"outcome": outcomeMergeCandidateVerified, "branch": h.consolidated, "head_sha": head,
		"merge_candidate_result": "passed", "merge_candidate_reason": "",
		"merge_candidate_output_tail": "ok",
	})
	if w := shipPRRequest(t, h.s, h.parent.ID, h.impl.ID, priv, body, ""); w.Code != http.StatusOK {
		t.Fatalf("report status = %d:\n%s", w.Code, w.Body.String())
	}
	h.s.waitBackgroundReviews()

	// Done-means 3: with the PR's head = H and base = main the merge gate
	// admits, after exactly one pass and no rebase.
	runRow, err := h.rr.GetRun(ctx, h.parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if res := h.s.mergeCandidateGate(ctx, runRow); !res.admits() {
		t.Fatalf("merge gate refused %s (%s; cause %v): the fan-in head needed a second pass", res.Code, res.Message, res.Cause)
	}
	if n := len(auditEntries(h.au, CategoryMergeCandidateVerified)); n != 1 {
		t.Errorf("merge_candidate_verified rows = %d, want exactly 1", n)
	}
	if n := len(mcvTriggerRows(h.au)); n != 1 {
		t.Errorf("trigger rows = %d, want exactly 1", n)
	}
	if n := len(auditEntries(h.au, CategoryBranchRebased)); n != 0 {
		t.Errorf("branch_rebased rows = %d, want 0", n)
	}
}

// TestFanInOnBaseTip_MainConflictNamesSlice is done-means 5. B1 and slice 1
// both touch c.go; slice 0 does not. Slice 1 merges cleanly onto B0 + slice 0,
// so absent the fold no conflict surfaces at fan-in. With the fold, the final
// pass, driven through POST /v0/runs/{id}/consolidate, folds B1 cleanly and
// slice 1's own merge conflicts: the parent implement stage fails category B
// with the stable prefix and the slice_integration_conflict row names slice 1,
// child 1 and the base path c.go.
func TestFanInOnBaseTip_MainConflictNamesSlice(t *testing.T) {
	h := newBaseTipHarness(t, run.StageStateAwaitingChildren)
	h.integrateFirstWave()
	h.advanceMainAndFinishSlice1("c.go", "c.go")
	h.s.cfg.Orchestrator = h.o
	h.o.ConsolidatedReview = nil

	w := postConsolidate(t, h.s, h.parent.ID, withAuth)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	resp := decodeConsolidate(t, w)
	if resp.Outcome != "slice_conflict" {
		t.Fatalf("outcome = %q, want slice_conflict: slice 1's conflict with main did not surface at fan-in", resp.Outcome)
	}
	if resp.ConflictingSliceIndex == nil || *resp.ConflictingSliceIndex != 1 || resp.ConflictingChildRunID != h.child1.ID.String() {
		t.Errorf("response names slice %v child %q, want slice 1 child %s", resp.ConflictingSliceIndex, resp.ConflictingChildRunID, h.child1.ID)
	}
	if !h.graph.isAncestor("B1", h.graph.tip(h.consolidated)) {
		t.Error("the base fold did not land B1 on the consolidated branch before the slice merge")
	}

	st, err := h.rr.GetStage(context.Background(), h.impl.ID)
	if err != nil {
		t.Fatal(err)
	}
	if st.State != run.StageStateFailed || st.FailureCategory == nil || *st.FailureCategory != run.FailureB {
		t.Errorf("parent implement = %q / %v, want failed category B", st.State, st.FailureCategory)
	}
	if st.FailureReason == nil || !strings.HasPrefix(*st.FailureReason, "slice integration conflict") {
		t.Errorf("failure reason = %v, want the stable 'slice integration conflict' prefix", st.FailureReason)
	}

	entries := auditEntries(h.au, "slice_integration_conflict")
	if len(entries) != 1 {
		t.Fatalf("slice_integration_conflict rows = %d, want 1", len(entries))
	}
	var p struct {
		SliceIndex      int      `json:"conflicting_slice_index"`
		ChildRunID      string   `json:"conflicting_child_run_id"`
		BaseSHA         string   `json:"base_sha"`
		BasePaths       []string `json:"conflicting_base_paths"`
		BaseAttribution string   `json:"base_attribution"`
	}
	if err := json.Unmarshal(entries[0].Payload, &p); err != nil {
		t.Fatal(err)
	}
	if p.SliceIndex != 1 || p.ChildRunID != h.child1.ID.String() {
		t.Errorf("conflict names slice %d child %s, want slice 1 child %s", p.SliceIndex, p.ChildRunID, h.child1.ID)
	}
	if p.BaseSHA != "B1" || len(p.BasePaths) != 1 || p.BasePaths[0] != "c.go" || p.BaseAttribution != "paths_overlap" {
		t.Errorf("attribution = (%q, %v, %q), want (B1, [c.go], paths_overlap)", p.BaseSHA, p.BasePaths, p.BaseAttribution)
	}
}
