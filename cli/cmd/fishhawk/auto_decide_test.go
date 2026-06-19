package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/cli/internal/httpclient"
)

// --- isAutoApprovable matcher (tightened allowlist, #1233 condition) ---

func TestIsAutoApprovable(t *testing.T) {
	inScope := map[string]bool{
		"cli/cmd/fishhawk/auto_decide.go": true,
		"backend/internal/run/run.go":     true,
	}
	mk := func(paths ...httpclient.ScopeAmendmentPath) httpclient.ScopeAmendment {
		return httpclient.ScopeAmendment{Paths: paths}
	}
	tests := []struct {
		name string
		am   httpclient.ScopeAmendment
		want bool
	}{
		{
			name: "coupled test sibling of in-scope production file",
			am:   mk(httpclient.ScopeAmendmentPath{Path: "cli/cmd/fishhawk/auto_decide_test.go", Operation: "create"}),
			want: true,
		},
		{
			name: "multiple coupled in-scope test siblings",
			am: mk(
				httpclient.ScopeAmendmentPath{Path: "cli/cmd/fishhawk/auto_decide_test.go", Operation: "modify"},
				httpclient.ScopeAmendmentPath{Path: "backend/internal/run/run_test.go", Operation: "create"},
			),
			want: true,
		},
		{
			name: "no paths",
			am:   mk(),
			want: false,
		},
		{
			name: "production file path (not a test)",
			am:   mk(httpclient.ScopeAmendmentPath{Path: "cli/cmd/fishhawk/auto_decide.go", Operation: "modify"}),
			want: false,
		},
		{
			name: "mixed test + production file",
			am: mk(
				httpclient.ScopeAmendmentPath{Path: "cli/cmd/fishhawk/auto_decide_test.go", Operation: "create"},
				httpclient.ScopeAmendmentPath{Path: "cli/cmd/fishhawk/auto_decide.go", Operation: "modify"},
			),
			want: false,
		},
		{
			name: "delete operation on a coupled test",
			am:   mk(httpclient.ScopeAmendmentPath{Path: "cli/cmd/fishhawk/auto_decide_test.go", Operation: "delete"}),
			want: false,
		},
		{
			name: "test file whose production sibling is NOT in scope (tightened case)",
			am:   mk(httpclient.ScopeAmendmentPath{Path: "cli/cmd/fishhawk/stray_test.go", Operation: "create"}),
			want: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isAutoApprovable(tc.am, inScope); got != tc.want {
				t.Errorf("isAutoApprovable() = %v, want %v", got, tc.want)
			}
		})
	}
}

// autoDecideFake is a configurable backend for the auto-decide loop +
// end-to-end tests. It serves GET run, GET stages, GET artifacts, GET
// scope-amendments, and POST decision.
type autoDecideFake struct {
	mu sync.Mutex

	runID    uuid.UUID
	runState string

	// scope.files served via the plan stage's standard_v1 artifact.
	planStageID uuid.UUID
	scopeFiles  []string

	amendments map[uuid.UUID]*httpclient.ScopeAmendment

	decisionPosts  []uuid.UUID
	decisionStatus int // 0 → 200; >=400 → error envelope

	stagesStatus int // 0 → 200; >=400 → error envelope (scope-resolution failure)

	terminalAfterDecision bool

	getRunCount int
	listCount   int
}

func newAutoDecideFake(t *testing.T) (*autoDecideFake, *httptest.Server) {
	t.Helper()
	fb := &autoDecideFake{
		runID:       uuid.New(),
		runState:    "running",
		planStageID: uuid.New(),
		amendments:  map[uuid.UUID]*httpclient.ScopeAmendment{},
	}
	mux := http.NewServeMux()
	writeJSON := func(w http.ResponseWriter, status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}

	mux.HandleFunc("GET /v0/runs/{run_id}", func(w http.ResponseWriter, r *http.Request) {
		fb.mu.Lock()
		fb.getRunCount++
		state := fb.runState
		fb.mu.Unlock()
		writeJSON(w, http.StatusOK, httpclient.Run{ID: fb.runID, State: state})
	})
	mux.HandleFunc("GET /v0/runs/{run_id}/stages", func(w http.ResponseWriter, r *http.Request) {
		fb.mu.Lock()
		status := fb.stagesStatus
		fb.mu.Unlock()
		if status >= 400 {
			writeJSON(w, status, map[string]any{"error": map[string]any{
				"code": "internal", "message": "stage listing failed"}})
			return
		}
		writeJSON(w, http.StatusOK, httpclient.ListStagesResult{Items: []httpclient.Stage{
			{ID: fb.planStageID, RunID: fb.runID, Sequence: 1, Type: "plan", State: "succeeded"},
			{ID: uuid.New(), RunID: fb.runID, Sequence: 2, Type: "implement", State: "running"},
		}})
	})
	mux.HandleFunc("GET /v0/stages/{stage_id}/artifacts", func(w http.ResponseWriter, r *http.Request) {
		fb.mu.Lock()
		files := append([]string(nil), fb.scopeFiles...)
		fb.mu.Unlock()
		type sf struct {
			Path string `json:"path"`
		}
		content := struct {
			Scope struct {
				Files []sf `json:"files"`
			} `json:"scope"`
		}{}
		for _, f := range files {
			content.Scope.Files = append(content.Scope.Files, sf{Path: f})
		}
		raw, _ := json.Marshal(content)
		schema := "standard_v1"
		writeJSON(w, http.StatusOK, map[string]any{"items": []httpclient.Artifact{
			{ID: uuid.New(), StageID: fb.planStageID, Kind: "plan", SchemaVersion: &schema, Content: raw},
		}})
	})
	mux.HandleFunc("GET /v0/runs/{run_id}/scope-amendments", func(w http.ResponseWriter, r *http.Request) {
		fb.mu.Lock()
		fb.listCount++
		items := make([]httpclient.ScopeAmendment, 0, len(fb.amendments))
		for _, a := range fb.amendments {
			items = append(items, *a)
		}
		fb.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"items": items})
	})
	mux.HandleFunc("POST /v0/runs/{run_id}/scope-amendments/{amendment_id}/decision", func(w http.ResponseWriter, r *http.Request) {
		amendID, _ := uuid.Parse(r.PathValue("amendment_id"))
		fb.mu.Lock()
		fb.decisionPosts = append(fb.decisionPosts, amendID)
		status := fb.decisionStatus
		if status == 0 {
			if a := fb.amendments[amendID]; a != nil {
				a.Status = "approved"
			}
			if fb.terminalAfterDecision {
				fb.runState = "succeeded"
			}
		}
		fb.mu.Unlock()
		if status >= 400 {
			writeJSON(w, status, map[string]any{"error": map[string]any{
				"code": "amendment_already_decided", "message": "already decided"}})
			return
		}
		writeJSON(w, http.StatusOK, httpclient.ScopeAmendment{ID: amendID, RunID: fb.runID, Status: "approved"})
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return fb, srv
}

func (fb *autoDecideFake) addAmendment(paths ...httpclient.ScopeAmendmentPath) uuid.UUID {
	id := uuid.New()
	fb.mu.Lock()
	fb.amendments[id] = &httpclient.ScopeAmendment{
		ID: id, RunID: fb.runID, StageID: uuid.New(), Status: "pending", Paths: paths,
	}
	fb.mu.Unlock()
	return id
}

func (fb *autoDecideFake) posts() []uuid.UUID {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	return append([]uuid.UUID(nil), fb.decisionPosts...)
}

// (a) allowlisted amendment → approve POST issued; loop stops on terminal.
func TestAutoDecideLoop_Allowlisted_Approves(t *testing.T) {
	fb, srv := newAutoDecideFake(t)
	fb.terminalAfterDecision = true
	amendID := fb.addAmendment(httpclient.ScopeAmendmentPath{Path: "pkg/foo_test.go", Operation: "create"})
	inScope := map[string]bool{"pkg/foo.go": true}

	c := httpclient.New(srv.URL, "op-tok")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var stdout strings.Builder
	rc := autoDecideLoop(ctx, c, fb.runID, inScope, 1, false, &stdout, io.Discard)
	if rc != exitOK {
		t.Fatalf("rc = %d, want exitOK", rc)
	}
	posts := fb.posts()
	if len(posts) != 1 || posts[0] != amendID {
		t.Fatalf("decision posts = %v, want [%s]", posts, amendID)
	}
	if !strings.Contains(stdout.String(), "auto-approved amendment "+amendID.String()) {
		t.Errorf("stdout missing approval line: %s", stdout.String())
	}
}

// (b) non-allowlisted amendment → NO POST, left pending; loop exits on deadline.
func TestAutoDecideLoop_NonAllowlisted_NoPost(t *testing.T) {
	fb, srv := newAutoDecideFake(t)
	amendID := fb.addAmendment(httpclient.ScopeAmendmentPath{Path: "pkg/foo.go", Operation: "modify"})
	inScope := map[string]bool{"pkg/foo.go": true}

	c := httpclient.New(srv.URL, "op-tok")
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	var stdout strings.Builder
	rc := autoDecideLoop(ctx, c, fb.runID, inScope, 1, false, &stdout, io.Discard)
	if rc != exitOK {
		t.Fatalf("rc = %d, want exitOK", rc)
	}
	if posts := fb.posts(); len(posts) != 0 {
		t.Fatalf("decision posts = %v, want none", posts)
	}
	if !strings.Contains(stdout.String(), "left for manual decision") {
		t.Errorf("stdout missing manual-decision line for %s: %s", amendID, stdout.String())
	}
}

// tightened-matcher case at the loop level: a _test.go whose production
// sibling is NOT in scope must be SKIPPED (no POST).
func TestAutoDecideLoop_TestSiblingNotInScope_Skipped(t *testing.T) {
	fb, srv := newAutoDecideFake(t)
	fb.addAmendment(httpclient.ScopeAmendmentPath{Path: "pkg/foo_test.go", Operation: "create"})
	inScope := map[string]bool{"pkg/other.go": true} // foo.go NOT in scope

	c := httpclient.New(srv.URL, "op-tok")
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	rc := autoDecideLoop(ctx, c, fb.runID, inScope, 1, false, io.Discard, io.Discard)
	if rc != exitOK {
		t.Fatalf("rc = %d, want exitOK", rc)
	}
	if posts := fb.posts(); len(posts) != 0 {
		t.Fatalf("decision posts = %v, want none (sibling out of scope)", posts)
	}
}

// (c) decision returns 409 → loop logs and continues (does not abort).
func TestAutoDecideLoop_AlreadyDecided_ContinuesNotAbort(t *testing.T) {
	fb, srv := newAutoDecideFake(t)
	fb.decisionStatus = http.StatusConflict
	fb.addAmendment(httpclient.ScopeAmendmentPath{Path: "pkg/foo_test.go", Operation: "create"})
	inScope := map[string]bool{"pkg/foo.go": true}

	c := httpclient.New(srv.URL, "op-tok")
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	var stderr strings.Builder
	rc := autoDecideLoop(ctx, c, fb.runID, inScope, 1, false, io.Discard, &stderr)
	if rc != exitOK {
		t.Fatalf("rc = %d, want exitOK (409 must not abort)", rc)
	}
	if posts := fb.posts(); len(posts) == 0 {
		t.Fatal("expected at least one decision POST attempt")
	}
	if !strings.Contains(stderr.String(), "failed (continuing)") {
		t.Errorf("stderr missing continue-on-error line: %s", stderr.String())
	}
}

// (d) run already terminal → loop stops without ever listing amendments.
func TestAutoDecideLoop_TerminalState_StopsImmediately(t *testing.T) {
	fb, srv := newAutoDecideFake(t)
	fb.runState = "succeeded"
	fb.addAmendment(httpclient.ScopeAmendmentPath{Path: "pkg/foo_test.go", Operation: "create"})
	inScope := map[string]bool{"pkg/foo.go": true}

	c := httpclient.New(srv.URL, "op-tok")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var stdout strings.Builder
	rc := autoDecideLoop(ctx, c, fb.runID, inScope, 1, false, &stdout, io.Discard)
	if rc != exitOK {
		t.Fatalf("rc = %d, want exitOK", rc)
	}
	fb.mu.Lock()
	listCount := fb.listCount
	fb.mu.Unlock()
	if listCount != 0 {
		t.Errorf("listCount = %d, want 0 (terminal must short-circuit before listing)", listCount)
	}
	if posts := fb.posts(); len(posts) != 0 {
		t.Fatalf("decision posts = %v, want none", posts)
	}
	if !strings.Contains(stdout.String(), "terminal state") {
		t.Errorf("stdout missing terminal-stop line: %s", stdout.String())
	}
}

// (e) max-duration deadline elapses with a still-pending non-allowlisted
// amendment → loop returns having issued no POST.
func TestAutoDecideLoop_DeadlineElapses_NoPost(t *testing.T) {
	fb, srv := newAutoDecideFake(t)
	fb.addAmendment(httpclient.ScopeAmendmentPath{Path: "pkg/foo.go", Operation: "modify"})
	inScope := map[string]bool{"pkg/foo.go": true}

	c := httpclient.New(srv.URL, "op-tok")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	var stderr strings.Builder
	rc := autoDecideLoop(ctx, c, fb.runID, inScope, 1, false, io.Discard, &stderr)
	if rc != exitOK {
		t.Fatalf("rc = %d, want exitOK", rc)
	}
	if posts := fb.posts(); len(posts) != 0 {
		t.Fatalf("decision posts = %v, want none", posts)
	}
	if !strings.Contains(stderr.String(), "max-duration reached") {
		t.Errorf("stderr missing deadline line: %s", stderr.String())
	}
}

// (f) --dry-run → allowlisted amendment yields a logged verdict but NO POST.
func TestAutoDecideLoop_DryRun_NoPost(t *testing.T) {
	fb, srv := newAutoDecideFake(t)
	amendID := fb.addAmendment(httpclient.ScopeAmendmentPath{Path: "pkg/foo_test.go", Operation: "create"})
	inScope := map[string]bool{"pkg/foo.go": true}

	c := httpclient.New(srv.URL, "op-tok")
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	var stdout strings.Builder
	rc := autoDecideLoop(ctx, c, fb.runID, inScope, 1, true /* dryRun */, &stdout, io.Discard)
	if rc != exitOK {
		t.Fatalf("rc = %d, want exitOK", rc)
	}
	if posts := fb.posts(); len(posts) != 0 {
		t.Fatalf("decision posts = %v, want none in dry-run", posts)
	}
	if !strings.Contains(stdout.String(), "[dry-run] would approve amendment "+amendID.String()) {
		t.Errorf("stdout missing dry-run verdict: %s", stdout.String())
	}
}

// Cross-boundary end-to-end: drive the full `run auto-decide` command
// through the dispatcher against an httptest.Server, exercising the
// transport layer (scope.files resolution + amendment list/decide) and
// the command loop together — poll → resolve scope → match → decide →
// terminal-stop.
func TestRunAutoDecide_EndToEnd_ApprovesCoupledTest(t *testing.T) {
	fb, srv := newAutoDecideFake(t)
	fb.terminalAfterDecision = true
	fb.scopeFiles = []string{"pkg/foo.go", "pkg/bar.go"}
	amendID := fb.addAmendment(httpclient.ScopeAmendmentPath{Path: "pkg/foo_test.go", Operation: "create"})

	t.Setenv("FISHHAWK_BACKEND_URL", srv.URL)
	t.Setenv("FISHHAWK_TOKEN", "op-tok")

	var stdout strings.Builder
	rc := run([]string{"run", "auto-decide", "--poll", "1", fb.runID.String()}, &stdout, io.Discard)
	if rc != exitOK {
		t.Fatalf("rc = %d, want exitOK", rc)
	}
	posts := fb.posts()
	if len(posts) != 1 || posts[0] != amendID {
		t.Fatalf("decision posts = %v, want [%s]", posts, amendID)
	}
	if !strings.Contains(stdout.String(), "auto-approved amendment "+amendID.String()) {
		t.Errorf("stdout missing approval line: %s", stdout.String())
	}
}

// fetchInScopeFiles is the security-load-bearing input to the matcher:
// its result gates every auto-approve. A resolution failure (plan
// artifact missing, malformed, inaccessible, or a backend transport
// error) MUST yield an empty in-scope set so no amendment can be
// auto-approved (fail-closed). This asserts that invariant directly.
func TestFetchInScopeFiles_ResolutionError_EmptySet(t *testing.T) {
	fb, srv := newAutoDecideFake(t)
	fb.stagesStatus = http.StatusInternalServerError // scope resolution fails

	c := httpclient.New(srv.URL, "op-tok")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var stderr strings.Builder
	inScope := fetchInScopeFiles(ctx, c, fb.runID, &stderr)
	if len(inScope) != 0 {
		t.Fatalf("inScope = %v, want empty set on resolution failure", inScope)
	}
	if !strings.Contains(stderr.String(), "no amendment will auto-approve") {
		t.Errorf("stderr missing fail-closed warning: %s", stderr.String())
	}
}

// End-to-end fail-closed: when scope.files cannot be resolved (backend
// error), an amendment that WOULD be allowlisted given a readable scope
// is left undecided — no decision POST — because the in-scope set is
// empty. This locks the central safety property end-to-end through the
// command dispatcher and transport.
func TestRunAutoDecide_EndToEnd_ScopeResolutionFails_NoApprove(t *testing.T) {
	fb, srv := newAutoDecideFake(t)
	fb.stagesStatus = http.StatusInternalServerError
	// A coupled test sibling that would be auto-approved if scope.files
	// resolved to include pkg/foo.go; with resolution failing it must not.
	fb.addAmendment(httpclient.ScopeAmendmentPath{Path: "pkg/foo_test.go", Operation: "create"})

	t.Setenv("FISHHAWK_BACKEND_URL", srv.URL)
	t.Setenv("FISHHAWK_TOKEN", "op-tok")

	var stdout strings.Builder
	// --max-duration keeps the deadline short so the loop exits after
	// declining the (now out-of-scope) amendment.
	rc := run([]string{"run", "auto-decide", "--poll", "1", "--max-duration", "250ms", fb.runID.String()}, &stdout, io.Discard)
	if rc != exitOK {
		t.Fatalf("rc = %d, want exitOK", rc)
	}
	if posts := fb.posts(); len(posts) != 0 {
		t.Fatalf("decision posts = %v, want none (scope unresolved → fail-closed)", posts)
	}
}

// fetchInScopeFiles walks to the parent run when the current run has no
// usable plan artifact (a decomposed child / implement-only run). This
// drives the r.ParentRunID hop: the child has no plan stage, the parent
// carries the standard_v1 plan, and the resolved set is the parent's
// scope.files.
func TestFetchInScopeFiles_ParentWalk(t *testing.T) {
	childID := uuid.New()
	parentID := uuid.New()
	parentPlanStage := uuid.New()
	schema := "standard_v1"

	writeJSON := func(w http.ResponseWriter, status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v0/runs/{run_id}", func(w http.ResponseWriter, r *http.Request) {
		id, _ := uuid.Parse(r.PathValue("run_id"))
		if id == childID {
			writeJSON(w, http.StatusOK, httpclient.Run{ID: childID, State: "running", ParentRunID: &parentID})
			return
		}
		writeJSON(w, http.StatusOK, httpclient.Run{ID: parentID, State: "running"})
	})
	mux.HandleFunc("GET /v0/runs/{run_id}/stages", func(w http.ResponseWriter, r *http.Request) {
		id, _ := uuid.Parse(r.PathValue("run_id"))
		if id == childID {
			// Implement-only child: no plan stage → walk to parent.
			writeJSON(w, http.StatusOK, httpclient.ListStagesResult{Items: []httpclient.Stage{
				{ID: uuid.New(), RunID: childID, Sequence: 1, Type: "implement", State: "running"},
			}})
			return
		}
		writeJSON(w, http.StatusOK, httpclient.ListStagesResult{Items: []httpclient.Stage{
			{ID: parentPlanStage, RunID: parentID, Sequence: 1, Type: "plan", State: "succeeded"},
		}})
	})
	mux.HandleFunc("GET /v0/stages/{stage_id}/artifacts", func(w http.ResponseWriter, r *http.Request) {
		content := struct {
			Scope struct {
				Files []struct {
					Path string `json:"path"`
				} `json:"files"`
			} `json:"scope"`
		}{}
		content.Scope.Files = append(content.Scope.Files, struct {
			Path string `json:"path"`
		}{Path: "pkg/foo.go"})
		raw, _ := json.Marshal(content)
		writeJSON(w, http.StatusOK, map[string]any{"items": []httpclient.Artifact{
			{ID: uuid.New(), StageID: parentPlanStage, Kind: "plan", SchemaVersion: &schema, Content: raw},
		}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := httpclient.New(srv.URL, "op-tok")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	inScope := fetchInScopeFiles(ctx, c, childID, io.Discard)
	if !inScope["pkg/foo.go"] {
		t.Fatalf("inScope = %v, want parent scope.files pkg/foo.go resolved via ParentRunID hop", inScope)
	}
}

// fetchInScopeFiles bounds the parent walk: a chain of plan-less runs
// that never terminates must exit after the depth cap with an empty
// in-scope set (fail-closed), not loop forever. Every run reports no plan
// stage and a fresh ParentRunID, so the walk is exhausted by the cap.
func TestFetchInScopeFiles_ParentWalk_DepthExceeded_EmptySet(t *testing.T) {
	writeJSON := func(w http.ResponseWriter, status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v0/runs/{run_id}", func(w http.ResponseWriter, r *http.Request) {
		id, _ := uuid.Parse(r.PathValue("run_id"))
		next := uuid.New() // always another parent → never resolves
		writeJSON(w, http.StatusOK, httpclient.Run{ID: id, State: "running", ParentRunID: &next})
	})
	mux.HandleFunc("GET /v0/runs/{run_id}/stages", func(w http.ResponseWriter, r *http.Request) {
		// No plan stage on any run → always walk to the parent.
		writeJSON(w, http.StatusOK, httpclient.ListStagesResult{Items: []httpclient.Stage{
			{ID: uuid.New(), Sequence: 1, Type: "implement", State: "running"},
		}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := httpclient.New(srv.URL, "op-tok")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var stderr strings.Builder
	inScope := fetchInScopeFiles(ctx, c, uuid.New(), &stderr)
	if len(inScope) != 0 {
		t.Fatalf("inScope = %v, want empty set when the parent walk exceeds depth", inScope)
	}
	if !strings.Contains(stderr.String(), "exceeded depth") {
		t.Errorf("stderr missing depth-exceeded warning: %s", stderr.String())
	}
}

func TestRunAutoDecide_BadUUID(t *testing.T) {
	var stderr strings.Builder
	rc := run([]string{"run", "auto-decide", "not-a-uuid"}, io.Discard, &stderr)
	if rc != exitUsage {
		t.Errorf("rc = %d, want exitUsage", rc)
	}
	if !strings.Contains(stderr.String(), "not a UUID") {
		t.Errorf("stderr missing diagnostic: %s", stderr.String())
	}
}

func TestRunAutoDecide_MissingArg(t *testing.T) {
	var stderr strings.Builder
	rc := run([]string{"run", "auto-decide"}, io.Discard, &stderr)
	if rc != exitUsage {
		t.Errorf("rc = %d, want exitUsage", rc)
	}
	if !strings.Contains(stderr.String(), "<run-id> required") {
		t.Errorf("stderr missing diagnostic: %s", stderr.String())
	}
}
