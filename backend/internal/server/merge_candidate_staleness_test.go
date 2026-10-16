package server

// Tests for the read-time merge_candidate_staleness block (E85.4 / #4147, the
// Captain's 2026-10-15 ruling, option b): the mode table over an httptest
// GitHub fake, the read-error degrade, the chain-head-keyed TTL cache, the
// read-only guard, and the cross-boundary end-to-end through the real fan-in,
// both HTTP handlers and the real rebase verb on commitGraph.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/timescale"
)

const (
	stalenessRepo     = "kuhlman-labs/example"
	stalenessPRNumber = 7
	stalenessBaseTip  = "BASETIP0000000000000000000000000000000001"
	stalenessHead     = "HEAD00000000000000000000000000000000000001"
)

// stalenessForge is the httptest GitHub fake: per-route status knobs, a
// blocking compare for the deadline case, and a request log.
type stalenessForge struct {
	mu sync.Mutex

	prStatus int // 0 → 200
	prMerged bool
	prState  string // "" → open
	prHead   string
	prBase   string

	refStatus int // 0 → 200
	baseSHA   string

	compareStatus  int // 0 → 200
	compareCommits int

	// blockRoute ("pulls", "ref" or "compare") blocks that route until the
	// request context is cancelled or release elapses; after the release it
	// answers normally, with the compare reporting 0 commits (fresh).
	blockRoute string
	release    time.Duration

	log []string
}

func newStalenessForge() *stalenessForge {
	return &stalenessForge{prHead: stalenessHead, prBase: "main", baseSHA: stalenessBaseTip}
}

func (f *stalenessForge) requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.log...)
}

func (f *stalenessForge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.log = append(f.log, r.Method+" "+r.URL.Path)
	prStatus, prMerged, prState, prHead, prBase := f.prStatus, f.prMerged, f.prState, f.prHead, f.prBase
	refStatus, baseSHA := f.refStatus, f.baseSHA
	compareStatus, compareCommits := f.compareStatus, f.compareCommits
	blockRoute, release := f.blockRoute, f.release
	f.mu.Unlock()
	block := func(route string) bool {
		if blockRoute != route {
			return true
		}
		select {
		case <-r.Context().Done():
			return false
		case <-time.After(release):
			compareCommits = 0
			return true
		}
	}

	path, ok := strings.CutPrefix(r.URL.Path, "/repos/"+stalenessRepo+"/")
	if !ok || r.Method != http.MethodGet {
		graphNotFound(w)
		return
	}
	switch {
	case path == "pulls/"+strconv.Itoa(stalenessPRNumber):
		if !block("pulls") {
			return
		}
		if prStatus != 0 {
			graphJSON(w, prStatus, map[string]string{"message": "boom"})
			return
		}
		if prState == "" {
			prState = "open"
		}
		graphJSON(w, http.StatusOK, map[string]any{
			"number": stalenessPRNumber, "node_id": "PR_s", "state": prState, "merged": prMerged,
			"head": map[string]string{"sha": prHead, "ref": "fishhawk/consolidated"},
			"base": map[string]string{"ref": prBase},
		})
	case path == "git/ref/heads/main":
		if !block("ref") {
			return
		}
		if refStatus != 0 {
			graphJSON(w, refStatus, map[string]string{"message": "boom"})
			return
		}
		graphJSON(w, http.StatusOK, map[string]any{"object": map[string]string{"sha": baseSHA}})
	case strings.HasPrefix(path, "compare/"):
		if !block("compare") {
			return
		}
		if compareStatus != 0 {
			graphJSON(w, compareStatus, map[string]string{"message": "boom"})
			return
		}
		rows := make([]map[string]string, 0, compareCommits)
		for i := 0; i < compareCommits; i++ {
			rows = append(rows, map[string]string{"sha": fmt.Sprintf("b%039d", i)})
		}
		graphJSON(w, http.StatusOK, map[string]any{"total_commits": compareCommits, "commits": rows})
	default:
		graphNotFound(w)
	}
}

// stalenessFixture is a Server over fakeRepo + auditFake + the forge fake,
// with an open decomposed parent (installation, PR URL, one child).
type stalenessFixture struct {
	s      *Server
	repo   *fakeRepo
	au     *auditFake
	forge  *stalenessForge
	parent *run.Run
	child  *run.Run
	clock  *stalenessClock
}

// stalenessClock is the injected s.nowFunc.
type stalenessClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *stalenessClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *stalenessClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newStalenessFixture(t *testing.T) *stalenessFixture {
	t.Helper()
	fg := newStalenessForge()
	srv := httptest.NewServer(fg)
	t.Cleanup(srv.Close)
	client := &githubclient.Client{
		BaseURL: srv.URL,
		Tokens:  &fakeTokenProvider{tok: "ghs_t"},
		HTTP:    &http.Client{Timeout: 10 * time.Second},
		AppJWT:  func() (string, error) { return "ghs_jwt", nil },
	}
	repo := newFakeRepo()
	au := newAuditFake()
	au.stampSequence = true
	s := New(Config{
		Addr: "127.0.0.1:0", RunRepo: repo, AuditRepo: au,
		ConcernRepo: newFakeConcernRepo(), GitHub: client,
	})
	clock := &stalenessClock{now: time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)}
	s.nowFunc = clock.Now

	inst := int64(55)
	parent, err := repo.CreateRun(context.Background(), run.CreateRunParams{
		Repo: stalenessRepo, WorkflowID: "feature_change", WorkflowSHA: "s",
		TriggerSource: run.TriggerCLI, InstallationID: &inst,
	})
	if err != nil {
		t.Fatal(err)
	}
	prURL := fmt.Sprintf("https://github.com/%s/pull/%d", stalenessRepo, stalenessPRNumber)
	parent.PullRequestURL = &prURL
	parent.State = run.StateRunning
	child, err := repo.CreateRun(context.Background(), run.CreateRunParams{
		Repo: stalenessRepo, WorkflowID: "feature_change", WorkflowSHA: "s",
		TriggerSource: run.TriggerCLI, DecomposedFrom: &parent.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &stalenessFixture{s: s, repo: repo, au: au, forge: fg, parent: parent, child: child, clock: clock}
}

// stalenessReads is one subtest's observation through BOTH real handlers.
type stalenessReads struct {
	gateRaw   map[string]json.RawMessage
	gate      gateViewResponse
	runRaw    map[string]json.RawMessage
	gateBlock map[string]any // nil when the key is absent
	runBlock  map[string]any
}

func readStaleness(t *testing.T, s *Server, runID uuid.UUID) stalenessReads {
	t.Helper()
	var out stalenessReads
	gw := getGateView(t, s, runID, "")
	out.gate = decodeGateView(t, gw)
	if err := json.Unmarshal(gw.Body.Bytes(), &out.gateRaw); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/v0/runs/"+runID.String(), nil)
	req.SetPathValue("run_id", runID.String())
	rw := httptest.NewRecorder()
	s.handleGetRun(rw, req)
	if rw.Code != http.StatusOK {
		t.Fatalf("GET run status = %d, want 200:\n%s", rw.Code, rw.Body.String())
	}
	if err := json.Unmarshal(rw.Body.Bytes(), &out.runRaw); err != nil {
		t.Fatal(err)
	}
	decode := func(raw map[string]json.RawMessage) map[string]any {
		b, ok := raw["merge_candidate_staleness"]
		if !ok {
			return nil
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("decode block: %v", err)
		}
		return m
	}
	out.gateBlock, out.runBlock = decode(out.gateRaw), decode(out.runRaw)
	return out
}

func hasGap(gaps []string, name string) bool {
	for _, g := range gaps {
		if g == name {
			return true
		}
	}
	return false
}

// TestMergeCandidateStaleness_Modes is one subtest per branch of
// mergeCandidateStalenessFor / computeMergeCandidateStaleness, each asserted
// through BOTH the real gate-view handler and GET /v0/runs/{id}.
func TestMergeCandidateStaleness_Modes(t *testing.T) {
	type want struct {
		omitted    bool
		state      string
		behindBy   int
		lowerBound bool
		reason     string
	}
	cases := []struct {
		name  string
		setup func(f *stalenessFixture)
		want  want
	}{
		{"stale", func(f *stalenessFixture) { f.forge.compareCommits = 2 },
			want{state: "stale", behindBy: 2}},
		{"fresh", func(f *stalenessFixture) { f.forge.compareCommits = 0 },
			want{state: "fresh", behindBy: 0}},
		{"stale_at_cap", func(f *stalenessFixture) { f.forge.compareCommits = 250 },
			want{state: "stale", behindBy: 250, lowerBound: true}},
		{"omitted_terminal", func(f *stalenessFixture) {
			f.forge.compareCommits = 2
			f.parent.State = run.StateSucceeded
		}, want{omitted: true}},
		{"omitted_no_pr_url", func(f *stalenessFixture) {
			f.forge.compareCommits = 2
			f.parent.PullRequestURL = nil
		}, want{omitted: true}},
		{"omitted_not_parent", func(f *stalenessFixture) {
			// An ordinary run WITH a PR URL and installation that the fake
			// reports behind: only the children check keeps the block off.
			f.forge.compareCommits = 2
			delete(f.repo.runs, f.child.ID)
		}, want{omitted: true}},
		{"omitted_pr_merged", func(f *stalenessFixture) {
			f.forge.compareCommits = 1
			f.forge.prMerged = true
		}, want{omitted: true}},
		{"omitted_pr_closed", func(f *stalenessFixture) {
			f.forge.compareCommits = 1
			f.forge.prState = "closed"
		}, want{omitted: true}},
		{"undetermined_no_client", func(f *stalenessFixture) {
			f.forge.compareCommits = 2
			f.s.cfg.GitHub = nil
		}, want{state: "undetermined", reason: "forge_not_probeable"}},
		{"undetermined_no_installation", func(f *stalenessFixture) {
			f.forge.compareCommits = 2
			f.parent.InstallationID = nil
		}, want{state: "undetermined", reason: "forge_not_probeable"}},
		{"undetermined_zero_installation", func(f *stalenessFixture) {
			f.forge.compareCommits = 2
			zero := int64(0)
			f.parent.InstallationID = &zero
		}, want{state: "undetermined", reason: "forge_not_probeable"}},
		{"undetermined_repo_unparseable", func(f *stalenessFixture) {
			f.forge.compareCommits = 2
			f.parent.Repo = "not-a-repo"
		}, want{state: "undetermined", reason: "forge_not_probeable"}},
		{"undetermined_pr_number_unparseable", func(f *stalenessFixture) {
			f.forge.compareCommits = 2
			u := "https://github.com/" + stalenessRepo + "/issues/7"
			f.parent.PullRequestURL = &u
		}, want{state: "undetermined", reason: "forge_not_probeable"}},
		{"undetermined_pr_read_failed", func(f *stalenessFixture) {
			f.forge.compareCommits = 2
			f.forge.prStatus = http.StatusInternalServerError
		}, want{state: "undetermined", reason: "pr_read_failed"}},
		{"undetermined_pr_empty_head", func(f *stalenessFixture) {
			f.forge.compareCommits = 2
			f.forge.prHead = ""
		}, want{state: "undetermined", reason: "pr_read_failed"}},
		{"undetermined_pr_empty_base", func(f *stalenessFixture) {
			f.forge.compareCommits = 2
			f.forge.prBase = ""
		}, want{state: "undetermined", reason: "pr_read_failed"}},
		{"undetermined_base_ref_not_found", func(f *stalenessFixture) {
			f.forge.compareCommits = 2
			f.forge.refStatus = http.StatusNotFound
		}, want{state: "undetermined", reason: "base_tip_unresolved"}},
		{"undetermined_base_read_failed", func(f *stalenessFixture) {
			f.forge.compareCommits = 2
			f.forge.refStatus = http.StatusInternalServerError
		}, want{state: "undetermined", reason: "base_tip_unresolved"}},
		{"undetermined_compare_failed", func(f *stalenessFixture) {
			f.forge.compareCommits = 2
			f.forge.compareStatus = http.StatusInternalServerError
		}, want{state: "undetermined", reason: "compare_failed"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newStalenessFixture(t)
			f.repo.mu.Lock()
			f.forge.mu.Lock()
			tc.setup(f)
			f.forge.mu.Unlock()
			f.repo.mu.Unlock()
			got := readStaleness(t, f.s, f.parent.ID)
			if hasGap(got.gate.HistoryGaps, gateViewGapMergeCandidateStaleness) {
				t.Fatalf("history_gaps = %v, want no merge_candidate_staleness gap", got.gate.HistoryGaps)
			}
			for surface, b := range map[string]map[string]any{"gate view": got.gateBlock, "GET run": got.runBlock} {
				assertStalenessBlock(t, surface, b, tc.want.omitted, tc.want.state, tc.want.behindBy, tc.want.lowerBound, tc.want.reason)
			}
		})
	}

	// One read_timeout subtest per forge read: each route blocks until its
	// request context is cancelled or a scaled release elapses (after which
	// the read set would answer fresh); the seam deadline is smaller.
	for _, route := range []string{"pulls", "ref", "compare"} {
		t.Run("undetermined_read_timeout_on_"+route, func(t *testing.T) {
			f := newStalenessFixture(t)
			release := timescale.D(3 * time.Second)
			f.forge.compareCommits = 2
			f.forge.blockRoute, f.forge.release = route, release
			f.s.mergeCandidateStalenessReadTimeout = timescale.D(200 * time.Millisecond)
			// Timed through GET /v0/runs/{id}, which issues no other forge
			// read (the gate view's merge_readiness also reads the PR,
			// unbounded by this deadline); the gate view then serves the
			// cached block.
			start := time.Now()
			first := getRunStaleness(t, f.s, f.parent.ID)
			elapsed := time.Since(start)
			assertStalenessBlock(t, "first GET run", first, false, "undetermined", 0, false, "read_timeout")
			if elapsed >= release {
				t.Errorf("the computing read took %v, want < the release %v: the deadline did not bound the forge reads", elapsed, release)
			}
			got := readStaleness(t, f.s, f.parent.ID)
			for surface, b := range map[string]map[string]any{"gate view": got.gateBlock, "GET run": got.runBlock} {
				assertStalenessBlock(t, surface, b, false, "undetermined", 0, false, "read_timeout")
			}
		})
	}
}

// TestMergeCandidateStaleness_UnwiredRepos: no RunRepo or no AuditRepo answers
// (nil, nil) with no forge read.
func TestMergeCandidateStaleness_UnwiredRepos(t *testing.T) {
	for _, name := range []string{"no_run_repo", "no_audit_repo"} {
		t.Run(name, func(t *testing.T) {
			f := newStalenessFixture(t)
			f.forge.compareCommits = 2
			if name == "no_run_repo" {
				f.s.cfg.RunRepo = nil
			} else {
				f.s.cfg.AuditRepo = nil
			}
			block, err := f.s.mergeCandidateStalenessFor(context.Background(), f.parent)
			if block != nil || err != nil {
				t.Errorf("mergeCandidateStalenessFor = (%v, %v), want (nil, nil)", block, err)
			}
			if n := len(f.forge.requests()); n != 0 {
				t.Errorf("forge requests = %d, want 0", n)
			}
		})
	}
}

// assertStalenessBlock checks one surface's block against the expected mode.
// Every undetermined block also asserts behind_by and next_step are ABSENT.
func assertStalenessBlock(t *testing.T, surface string, b map[string]any, omitted bool, state string, behindBy int, lowerBound bool, reason string) {
	t.Helper()
	if omitted {
		if b != nil {
			t.Errorf("%s: merge_candidate_staleness = %v, want the key absent", surface, b)
		}
		return
	}
	if b == nil {
		t.Fatalf("%s: merge_candidate_staleness absent, want state %s", surface, state)
	}
	if b["state"] != state {
		t.Errorf("%s: state = %v, want %s (block %v)", surface, b["state"], state, b)
	}
	if note, _ := b["note"].(string); note == "" {
		t.Errorf("%s: note is empty", surface)
	}
	if ca, _ := b["checked_at"].(string); ca == "" {
		t.Errorf("%s: checked_at is empty", surface)
	}
	switch state {
	case "undetermined":
		if b["state"] == "fresh" {
			t.Errorf("%s: an undetermined read reported fresh", surface)
		}
		if b["reason"] != reason {
			t.Errorf("%s: reason = %v, want %s", surface, b["reason"], reason)
		}
		for _, k := range []string{"behind_by", "next_step"} {
			if _, has := b[k]; has {
				t.Errorf("%s: undetermined block carries %s = %v, want absent", surface, k, b[k])
			}
		}
	default:
		if got, _ := b["behind_by"].(float64); int(got) != behindBy {
			t.Errorf("%s: behind_by = %v, want %d", surface, b["behind_by"], behindBy)
		}
		if _, has := b["behind_by"]; !has {
			t.Errorf("%s: behind_by absent, want %d", surface, behindBy)
		}
		if b["behind_by_lower_bound"] != lowerBound {
			t.Errorf("%s: behind_by_lower_bound = %v, want %v", surface, b["behind_by_lower_bound"], lowerBound)
		}
		if b["head_sha"] != stalenessHead || b["base_ref"] != "main" || b["base_sha"] != stalenessBaseTip {
			t.Errorf("%s: head/base = %v / %v / %v, want %s / main / %s", surface, b["head_sha"], b["base_ref"], b["base_sha"], stalenessHead, stalenessBaseTip)
		}
		if _, has := b["reason"]; has {
			t.Errorf("%s: %s block carries reason %v", surface, state, b["reason"])
		}
		wantNext := ""
		if state == "stale" {
			wantNext = "fishhawk_rebase_run_branch"
		}
		if got, _ := b["next_step"].(string); got != wantNext {
			t.Errorf("%s: next_step = %q, want %q", surface, got, wantNext)
		}
	}
}

// TestMergeCandidateStaleness_ReadErrors: a children-list error, and
// separately a chain-head category read error, make the gate view omit the
// block with history_incomplete and a merge_candidate_staleness gap, while
// GET /v0/runs/{id} still answers 200 without it.
func TestMergeCandidateStaleness_ReadErrors(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(f *stalenessFixture)
	}{
		{"children_list_error", func(f *stalenessFixture) { f.repo.listErr = errors.New("injected list error") }},
		{"chain_head_read_error", func(f *stalenessFixture) { f.au.listByCategoryErrCategory = "pull_request_opened" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newStalenessFixture(t)
			f.forge.compareCommits = 2
			tc.setup(f)
			got := readStaleness(t, f.s, f.parent.ID)
			if got.gateBlock != nil || got.runBlock != nil {
				t.Errorf("block present on a read error: gate %v, run %v", got.gateBlock, got.runBlock)
			}
			if !got.gate.HistoryIncomplete {
				t.Error("history_incomplete = false, want true")
			}
			if !hasGap(got.gate.HistoryGaps, gateViewGapMergeCandidateStaleness) {
				t.Errorf("history_gaps = %v, want merge_candidate_staleness", got.gate.HistoryGaps)
			}
			for _, r := range f.forge.requests() {
				if strings.Contains(r, "/compare/") || strings.Contains(r, "/git/ref/") {
					t.Errorf("forge request %q on a read error: the reads must stop before the forge", r)
				}
			}
		})
	}
}

// stalenessReadSetCount counts the staleness reads in a request log: the
// base-ref read and the compare are issued only by this block.
func stalenessReadSetCount(log []string) (pulls, refs, compares int) {
	for _, r := range log {
		switch {
		case strings.HasSuffix(r, "/pulls/"+strconv.Itoa(stalenessPRNumber)):
			pulls++
		case strings.Contains(r, "/git/ref/heads/"):
			refs++
		case strings.Contains(r, "/compare/"):
			compares++
		}
	}
	return pulls, refs, compares
}

func getRunStaleness(t *testing.T, s *Server, runID uuid.UUID) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v0/runs/"+runID.String(), nil)
	req.SetPathValue("run_id", runID.String())
	w := httptest.NewRecorder()
	s.handleGetRun(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET run status = %d:\n%s", w.Code, w.Body.String())
	}
	var env struct {
		Block map[string]any `json:"merge_candidate_staleness"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	return env.Block
}

// TestMergeCandidateStaleness_CacheBounded pins the chain-head-keyed TTL
// cache through GET /v0/runs/{id} (which issues no other forge read).
func TestMergeCandidateStaleness_CacheBounded(t *testing.T) {
	f := newStalenessFixture(t)
	f.forge.compareCommits = 2

	first := getRunStaleness(t, f.s, f.parent.ID)
	if first == nil || first["state"] != "stale" {
		t.Fatalf("first read = %v, want stale", first)
	}
	if p, r, c := stalenessReadSetCount(f.forge.requests()); p != 1 || r != 1 || c != 1 {
		t.Fatalf("first read issued (pulls %d, refs %d, compares %d), want one of each: %v", p, r, c, f.forge.requests())
	}
	n0 := len(f.forge.requests())

	// Within the TTL: zero new forge requests and the same checked_at.
	f.clock.advance(mergeCandidateStalenessTTL - time.Second)
	second := getRunStaleness(t, f.s, f.parent.ID)
	if n := len(f.forge.requests()); n != n0 {
		t.Errorf("in-TTL read issued %d new forge request(s), want 0: %v", n-n0, f.forge.requests()[n0:])
	}
	if second["checked_at"] != first["checked_at"] {
		t.Errorf("in-TTL checked_at = %v, want the cached %v", second["checked_at"], first["checked_at"])
	}

	// Past the TTL: exactly one new read set.
	f.forge.mu.Lock()
	f.forge.compareCommits = 0
	f.forge.mu.Unlock()
	f.clock.advance(2 * time.Second)
	third := getRunStaleness(t, f.s, f.parent.ID)
	if p, r, c := stalenessReadSetCount(f.forge.requests()[n0:]); p != 1 || r != 1 || c != 1 {
		t.Errorf("post-TTL read issued (pulls %d, refs %d, compares %d), want exactly one of each", p, r, c)
	}
	if third["state"] != "fresh" || third["checked_at"] == first["checked_at"] {
		t.Errorf("post-TTL read = %v, want a re-read fresh block with a new checked_at", third)
	}

	t.Run("head_change_busts_the_entry", func(t *testing.T) {
		n1 := len(f.forge.requests())
		f.forge.mu.Lock()
		f.forge.compareCommits = 3
		f.forge.mu.Unlock()
		// A performed rebase moves the chain-recorded head INSIDE the TTL.
		payload, _ := json.Marshal(map[string]any{"new_head_sha": "MERGED0001", "already_up_to_date": false})
		if _, err := f.au.AppendChained(context.Background(), audit.ChainAppendParams{
			RunID: f.parent.ID, Category: CategoryBranchRebased, Payload: payload,
		}); err != nil {
			t.Fatal(err)
		}
		got := getRunStaleness(t, f.s, f.parent.ID)
		if p, r, c := stalenessReadSetCount(f.forge.requests()[n1:]); p != 1 || r != 1 || c != 1 {
			t.Errorf("post-head-change read issued (pulls %d, refs %d, compares %d), want one re-read set", p, r, c)
		}
		if got["state"] != "stale" {
			t.Errorf("post-head-change state = %v, want the re-read stale (the cached block was fresh)", got["state"])
		}
	})

	t.Run("expired_entries_are_pruned_on_store", func(t *testing.T) {
		other := uuid.New()
		f.s.storeMergeCandidateStaleness(other, mergeCandidateStalenessEntry{computedAt: f.clock.Now()})
		f.clock.advance(mergeCandidateStalenessTTL + time.Second)
		f.s.storeMergeCandidateStaleness(uuid.New(), mergeCandidateStalenessEntry{computedAt: f.clock.Now()})
		f.s.mergeCandidateStalenessMu.Lock()
		_, kept := f.s.mergeCandidateStalenessCache[other]
		size := len(f.s.mergeCandidateStalenessCache)
		f.s.mergeCandidateStalenessMu.Unlock()
		if kept || size != 1 {
			t.Errorf("after a store past the TTL: expired entry kept = %v, cache size = %d, want pruned and 1", kept, size)
		}
	})
}

// TestMergeCandidateStaleness_ReadOnly is the "fishhawkd executes no verify
// command" criterion: (a) only GETs reach the forge, (b) the reads write
// nothing, (c) the file imports no os/exec and selects only the three read
// methods on cfg.GitHub.
func TestMergeCandidateStaleness_ReadOnly(t *testing.T) {
	f := newStalenessFixture(t)
	f.forge.compareCommits = 2
	ctx := context.Background()
	impl, err := f.repo.CreateStage(ctx, run.CreateStageParams{RunID: f.parent.ID, Type: run.StageTypeImplement, Sequence: 1})
	if err != nil {
		t.Fatal(err)
	}
	stageStates := func() map[uuid.UUID]run.StageState {
		// fakeRepo does not implement ListStagesForRun; read its store.
		f.repo.mu.Lock()
		defer f.repo.mu.Unlock()
		out := map[uuid.UUID]run.StageState{}
		for _, st := range f.repo.stagesByRun[f.parent.ID] {
			out[st.ID] = st.State
		}
		return out
	}
	chainBefore, err := f.au.ListForRun(ctx, f.parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	statesBefore := stageStates()

	got := readStaleness(t, f.s, f.parent.ID)
	if got.gateBlock == nil || got.gateBlock["state"] != "stale" {
		t.Fatalf("gate view block = %v, want stale", got.gateBlock)
	}

	t.Run("a_only_gets", func(t *testing.T) {
		log := f.forge.requests()
		if len(log) == 0 {
			t.Fatal("the fake recorded no request: the guard is vacuous")
		}
		for _, r := range log {
			if !strings.HasPrefix(r, "GET ") {
				t.Errorf("non-GET forge request %q", r)
			}
		}
	})
	t.Run("b_writes_nothing", func(t *testing.T) {
		chainAfter, err := f.au.ListForRun(ctx, f.parent.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(chainAfter) != len(chainBefore) {
			t.Errorf("audit chain length %d -> %d across the reads, want unchanged", len(chainBefore), len(chainAfter))
		}
		after := stageStates()
		if after[impl.ID] != statesBefore[impl.ID] || len(after) != len(statesBefore) {
			t.Errorf("stage states %v -> %v, want unchanged", statesBefore, after)
		}
		for _, cat := range []string{CategoryStageMergeCandidateVerifyTriggered, CategoryBranchRebased} {
			if n := len(auditEntries(f.au, cat)); n != 0 {
				t.Errorf("%s rows = %d, want 0", cat, n)
			}
		}
	})
	t.Run("c_ast_guard", func(t *testing.T) {
		assertStalenessFileReadOnly(t, filepath.Join(pkgSrcDir, "merge_candidate_staleness.go"))
	})
}

// stalenessFileViolations parses path and names every read-only violation: an
// os/exec import, a method selected on s.cfg.GitHub outside the three reads,
// an Append* / Transition* selector, or no s.cfg.GitHub selection at all (a
// vacuous guard).
func stalenessFileViolations(path string) ([]string, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, imp := range file.Imports {
		if p, _ := strconv.Unquote(imp.Path.Value); p == "os/exec" {
			out = append(out, "imports os/exec")
		}
	}
	allowed := map[string]bool{"GetPullRequest": true, "GetBranchSHA": true, "CompareCommits": true}
	selected := map[string]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if strings.HasPrefix(sel.Sel.Name, "Append") || strings.HasPrefix(sel.Sel.Name, "Transition") {
			out = append(out, "selects "+sel.Sel.Name)
		}
		inner, ok := sel.X.(*ast.SelectorExpr)
		if !ok || inner.Sel.Name != "GitHub" {
			return true
		}
		if cfg, ok := inner.X.(*ast.SelectorExpr); ok && cfg.Sel.Name == "cfg" {
			selected[sel.Sel.Name] = true
		}
		return true
	})
	if len(selected) == 0 {
		out = append(out, "selects no method on s.cfg.GitHub (vacuous)")
	}
	for name := range selected {
		if !allowed[name] {
			out = append(out, "selects s.cfg.GitHub."+name)
		}
	}
	sort.Strings(out)
	return out, nil
}

func assertStalenessFileReadOnly(t *testing.T, path string) {
	t.Helper()
	v, err := stalenessFileViolations(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(v) != 0 {
		t.Errorf("%s is not read-only: %v (want only GetPullRequest / GetBranchSHA / CompareCommits on s.cfg.GitHub, no os/exec, no Append*/Transition*)", path, v)
	}
}

// TestMergeCandidateStaleness_ReadOnlyGuardIsNotVacuous runs the AST guard
// over a file carrying each forbidden shape and requires a violation.
func TestMergeCandidateStaleness_ReadOnlyGuardIsNotVacuous(t *testing.T) {
	reads := "s.cfg.GitHub.GetPullRequest(); s.cfg.GitHub.GetBranchSHA(); s.cfg.GitHub.CompareCommits()"
	for name, src := range map[string]string{
		"os_exec_import": "package server\nimport _ \"os/exec\"\nfunc (s *Server) f() { " + reads + " }\n",
		"forge_write":    "package server\nfunc (s *Server) f() { " + reads + "; s.cfg.GitHub.MergeBranch() }\n",
		"audit_append":   "package server\nfunc (s *Server) f() { " + reads + "; s.cfg.AuditRepo.AppendChained() }\n",
		"no_reads":       "package server\nfunc (s *Server) f() {}\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "probe.go")
			if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
				t.Fatal(err)
			}
			v, err := stalenessFileViolations(path)
			if err != nil {
				t.Fatal(err)
			}
			if len(v) == 0 {
				t.Errorf("the AST guard passed a %s probe", name)
			}
		})
	}
}

// TestMergeCandidateStaleness_EndToEnd crosses the orchestrator fan-in, the
// forge client over commitGraph, the audit chain, both HTTP handlers and the
// real rebase verb.
func TestMergeCandidateStaleness_EndToEnd(t *testing.T) {
	h := newBaseTipHarness(t, run.StageStateSucceeded)
	ctx := context.Background()
	clock := &stalenessClock{now: time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)}
	h.s.nowFunc = clock.Now
	h.integrateFirstWave()
	h.advanceMainAndFinishSlice1("unrelated.go", "d.go")
	if conflict, err := h.o.IntegrateSlices(ctx, h.parent.ID); err != nil || conflict != nil {
		t.Fatalf("final IntegrateSlices = (%+v, %v), want (nil, nil)", conflict, err)
	}
	head := h.graph.tip(h.consolidated)
	if !h.graph.isAncestor("B1", head) {
		t.Fatalf("consolidated head %s does not contain B1", head)
	}
	prURL := "https://github.com/kuhlman-labs/example/pull/9"
	h.rr.mu.Lock()
	h.parent.PullRequestURL = &prURL
	h.rr.mu.Unlock()
	h.graph.mu.Lock()
	h.graph.prNumber, h.graph.prHead, h.graph.prBase = 9, h.consolidated, "main"
	h.graph.mu.Unlock()

	gateBlock := func() map[string]any {
		t.Helper()
		return readStaleness(t, h.s, h.parent.ID).gateBlock
	}
	if b := gateBlock(); b == nil || b["state"] != "fresh" || b["base_sha"] != "B1" {
		t.Fatalf("after fan-in on main@B1: block = %v, want fresh against B1", b)
	}

	// main advances to B2; inside the TTL the cached fresh still answers.
	h.graph.commit("B2", []string{"B1"}, "unrelated2.go")
	h.graph.setBranch("main", "B2")
	clock.advance(10 * time.Second)
	if b := gateBlock(); b == nil || b["state"] != "fresh" {
		t.Fatalf("inside the TTL after main moved: block = %v, want the cached fresh", b)
	}

	// Past the TTL both surfaces read stale against B2.
	clock.advance(mergeCandidateStalenessTTL)
	got := readStaleness(t, h.s, h.parent.ID)
	for surface, b := range map[string]map[string]any{"gate view": got.gateBlock, "GET run": got.runBlock} {
		if b == nil || b["state"] != "stale" || b["base_sha"] != "B2" || b["head_sha"] != head ||
			b["next_step"] != "fishhawk_rebase_run_branch" {
			t.Fatalf("%s past the TTL: block = %v, want stale against B2 naming fishhawk_rebase_run_branch", surface, b)
		}
		if n, _ := b["behind_by"].(float64); n != 1 {
			t.Errorf("%s: behind_by = %v, want 1", surface, b["behind_by"])
		}
	}
	for _, cat := range []string{CategoryStageMergeCandidateVerifyTriggered, CategoryBranchRebased} {
		if n := len(auditEntries(h.au, cat)); n != 0 {
			t.Fatalf("the staleness reads appended %d %s row(s), want 0", n, cat)
		}
	}

	// The operator's move: the real rebase verb performs the forge merge.
	h.s.cfg.Orchestrator = h.o
	rw := postRebaseBranch(t, h.s, h.parent.ID, rebaseBranchRequest{Confirm: true}, withRebaseOperator)
	if rw.Code != http.StatusOK {
		t.Fatalf("rebase-branch status = %d, want 200:\n%s", rw.Code, rw.Body.String())
	}
	var rb rebaseBranchResponse
	if err := json.Unmarshal(rw.Body.Bytes(), &rb); err != nil {
		t.Fatal(err)
	}
	if rb.AlreadyUpToDate || rb.NewHeadSHA == "" || !h.graph.isAncestor("B2", h.graph.tip(h.consolidated)) {
		t.Fatalf("rebase response = %s, want a performed merge of B2", rw.Body.String())
	}

	// NO clock advance: the branch_rebased chain head busts the cache key.
	if b := gateBlock(); b == nil || b["state"] != "fresh" || b["base_sha"] != "B2" {
		t.Fatalf("after the rebase, inside the TTL: block = %v, want a re-read fresh against B2", b)
	}
}
