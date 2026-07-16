package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// --- fishhawk_merge_run (E48.7 / #1954) ---

// mergeRunFakeBackend is a self-contained backend stub for the merge_run
// tool. It serves the three endpoints the tool touches — GET /v0/runs/{id}
// (pre-flight + run-state reads + the run-terminal backstop), POST
// /v0/runs/{id}/merge (the verdict-record + queue), and GET
// /v0/runs/{id}/audit (the terminal-merge poll). GetRun returns
// stateBeforeMerge until the merge POST lands, then stateAfterMerge (when
// set) — so a test can flip the run terminal AFTER the POST to drive the
// backstop. The audit handler honors category + since_sequence so the poll's
// per-category anchored read resolves exactly as production does.
type mergeRunFakeBackend struct {
	mu sync.Mutex

	prURL            string // "" means the run carries no PR URL
	stateBeforeMerge string
	stateAfterMerge  string // "" -> unchanged after merge

	merged     bool
	mergeCalls int
	// mergeStatuses is a queue of HTTP statuses for successive POSTs (default
	// 200 when exhausted); mergeErrBodies the matching error bodies. Lets a
	// test drive a 502-then-200 sequence.
	mergeStatuses []int
	mergeErrBody  string
	mergeResp     MergeRunResult

	auditEntries []AuditEntry
	getRunCalls  int
}

func (fb *mergeRunFakeBackend) runState() string {
	if fb.merged && fb.stateAfterMerge != "" {
		return fb.stateAfterMerge
	}
	return fb.stateBeforeMerge
}

func newMergeRunFakeBackend(t *testing.T, fb *mergeRunFakeBackend) *httptest.Server {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /v0/runs/{run_id}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		id, perr := uuid.Parse(r.PathValue("run_id"))
		if perr != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		fb.mu.Lock()
		fb.getRunCalls++
		state := fb.runState()
		var prPtr *string
		if fb.prURL != "" {
			pr := fb.prURL
			prPtr = &pr
		}
		fb.mu.Unlock()
		_ = json.NewEncoder(w).Encode(Run{ID: id.String(), State: state, PullRequestURL: prPtr})
	})

	mux.HandleFunc("POST /v0/runs/{run_id}/merge", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if _, perr := uuid.Parse(r.PathValue("run_id")); perr != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var body mergeRunRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		fb.mu.Lock()
		status := http.StatusOK
		if fb.mergeCalls < len(fb.mergeStatuses) {
			status = fb.mergeStatuses[fb.mergeCalls]
		}
		fb.mergeCalls++
		errBody := fb.mergeErrBody
		resp := fb.mergeResp
		if status == http.StatusOK {
			fb.merged = true
		}
		fb.mu.Unlock()
		w.WriteHeader(status)
		if status != http.StatusOK {
			if errBody != "" {
				_, _ = w.Write([]byte(errBody))
			}
			return
		}
		_ = json.NewEncoder(w).Encode(resp)
	})

	mux.HandleFunc("GET /v0/runs/{run_id}/audit", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if _, perr := uuid.Parse(r.PathValue("run_id")); perr != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		category := r.URL.Query().Get("category")
		var since int64
		if s := r.URL.Query().Get("since_sequence"); s != "" {
			since, _ = strconv.ParseInt(s, 10, 64)
		}
		fb.mu.Lock()
		matches := make([]AuditEntry, 0, len(fb.auditEntries))
		for _, e := range fb.auditEntries {
			if e.Category == category && e.Sequence > since {
				matches = append(matches, e)
			}
		}
		fb.mu.Unlock()
		sort.Slice(matches, func(i, j int) bool { return matches[i].Sequence < matches[j].Sequence })
		if len(matches) > 1 {
			matches = matches[:1] // the tool passes Limit=1
		}
		_ = json.NewEncoder(w).Encode(listAuditResult{Items: matches})
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func newMergeRunResolver(srv *httptest.Server) *runResolver {
	return &runResolver{
		api:                newAPIClient(config{backendURL: srv.URL, apiToken: "tok-test"}),
		getenv:             envFuncFromMap(nil),
		reviewPollInterval: time.Millisecond,
	}
}

// TestMergeRun_HappyPath_MergedWithPostMergeStep drives the settle-to-merged
// path: POST once, the poll finds a pr_merged entry past the verdict anchor,
// and the output carries status=merged, verdict_recorded, and the reused
// postMergeStep next_action.
func TestMergeRun_HappyPath_MergedWithPostMergeStep(t *testing.T) {
	fb := &mergeRunFakeBackend{
		prURL:            "https://github.com/x/y/pull/7",
		stateBeforeMerge: "running",
		stateAfterMerge:  "succeeded",
		mergeResp:        MergeRunResult{MergeQueued: true, VerdictSequence: 5, PRURL: "https://github.com/x/y/pull/7"},
		auditEntries:     []AuditEntry{{Category: "pr_merged", Sequence: 6}},
	}
	srv := newMergeRunFakeBackend(t, fb)
	r := newMergeRunResolver(srv)

	_, out, err := r.mergeRun(context.Background(), nil, MergeRunInput{RunID: uuid.NewString(), Verdict: "ship it"})
	if err != nil {
		t.Fatalf("mergeRun: %v", err)
	}
	if out.Status != "merged" {
		t.Fatalf("status = %q, want merged", out.Status)
	}
	if out.RunState != "succeeded" {
		t.Errorf("run_state = %q, want succeeded", out.RunState)
	}
	if !out.VerdictRecorded || out.AlreadyRecorded {
		t.Errorf("verdict flags = recorded:%v already:%v, want recorded:true already:false", out.VerdictRecorded, out.AlreadyRecorded)
	}
	if !out.MergeQueued {
		t.Error("merge_queued = false, want true")
	}
	if out.NextAction == nil || out.NextAction.Action != "post_merge" {
		t.Fatalf("next_action = %+v, want the surfaced post_merge step", out.NextAction)
	}
	if !strings.Contains(out.NextAction.Reason, "scripts/dev post-merge") {
		t.Errorf("post_merge reason = %q, want it to name scripts/dev post-merge", out.NextAction.Reason)
	}
	if out.Note == "" {
		t.Error("note is empty, want the split-identity gh-approval reminder")
	}
	if fb.mergeCalls != 1 {
		t.Errorf("merge POSTed %d times, want exactly 1", fb.mergeCalls)
	}
}

// TestMergeRun_Timeout_Resumable pins the resumable timeout: the run stays
// non-terminal and no merge entry lands, so the bounded await returns
// status=timeout with an actionable resume message and no next_action.
func TestMergeRun_Timeout_Resumable(t *testing.T) {
	fb := &mergeRunFakeBackend{
		prURL:            "https://github.com/x/y/pull/7",
		stateBeforeMerge: "running",
		stateAfterMerge:  "running", // never terminal
		mergeResp:        MergeRunResult{MergeQueued: true, VerdictSequence: 5},
		auditEntries:     nil, // nothing settles
	}
	srv := newMergeRunFakeBackend(t, fb)
	r := newMergeRunResolver(srv)

	// A short ctx deadline bounds the poll without a real multi-second wait —
	// pollCtx inherits the earliest deadline.
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()

	_, out, err := r.mergeRun(ctx, nil, MergeRunInput{RunID: uuid.NewString(), Verdict: "ship it"})
	if err != nil {
		t.Fatalf("mergeRun: %v", err)
	}
	if out.Status != "timeout" {
		t.Fatalf("status = %q, want timeout", out.Status)
	}
	if out.NextAction != nil {
		t.Errorf("next_action = %+v, want nil on timeout", out.NextAction)
	}
	if !strings.Contains(out.Message, "re-invoke") {
		t.Errorf("timeout message = %q, want a resume hint", out.Message)
	}
	// The verdict was still recorded and the merge queued — a timeout is only
	// the await giving up, not a merge failure.
	if !out.VerdictRecorded || !out.MergeQueued {
		t.Errorf("verdict flags = recorded:%v queued:%v, want both true", out.VerdictRecorded, out.MergeQueued)
	}
}

// TestMergeRun_RunTerminal_Backstop drives the ADR-036 backstop: the run
// flips terminal (cancelled) AFTER the POST while no merge entry lands, so
// the await resolves run_terminal rather than holding the session open.
func TestMergeRun_RunTerminal_Backstop(t *testing.T) {
	fb := &mergeRunFakeBackend{
		prURL:            "https://github.com/x/y/pull/7",
		stateBeforeMerge: "running", // passes the pre-flight
		stateAfterMerge:  "cancelled",
		mergeResp:        MergeRunResult{MergeQueued: true, VerdictSequence: 5},
		auditEntries:     nil,
	}
	srv := newMergeRunFakeBackend(t, fb)
	r := newMergeRunResolver(srv)

	_, out, err := r.mergeRun(context.Background(), nil, MergeRunInput{RunID: uuid.NewString(), Verdict: "ship it"})
	if err != nil {
		t.Fatalf("mergeRun: %v", err)
	}
	if out.Status != "run_terminal" {
		t.Fatalf("status = %q, want run_terminal", out.Status)
	}
	if out.RunState != "cancelled" {
		t.Errorf("run_state = %q, want cancelled", out.RunState)
	}
	if out.NextAction != nil {
		t.Errorf("next_action = %+v, want nil on run_terminal", out.NextAction)
	}
	if !strings.Contains(out.Message, "cancelled") {
		t.Errorf("message = %q, want it to name the terminal state", out.Message)
	}
}

// TestMergeRun_AlwaysRePOSTsOnResume pins the INVERTED idempotence (#1954
// binding condition 1): the tool ALWAYS re-POSTs on resume with NO
// client-side skip. Two invocations each POST — the endpoint (not the tool)
// dedups the verdict row and reports already_recorded on the second, which
// the tool surfaces without suppressing the re-dispatch.
func TestMergeRun_AlwaysRePOSTsOnResume(t *testing.T) {
	fb := &mergeRunFakeBackend{
		prURL:            "https://github.com/x/y/pull/7",
		stateBeforeMerge: "running",
		stateAfterMerge:  "succeeded",
		mergeResp:        MergeRunResult{MergeQueued: true, VerdictSequence: 5},
		auditEntries:     []AuditEntry{{Category: "post_merge_observed", Sequence: 7}},
	}
	srv := newMergeRunFakeBackend(t, fb)
	r := newMergeRunResolver(srv)
	runID := uuid.NewString()

	if _, _, err := r.mergeRun(context.Background(), nil, MergeRunInput{RunID: runID, Verdict: "ship it"}); err != nil {
		t.Fatalf("first mergeRun: %v", err)
	}
	// Simulate the endpoint's idempotent second response: already_recorded.
	fb.mu.Lock()
	fb.mergeResp.AlreadyRecorded = true
	fb.mu.Unlock()

	_, out, err := r.mergeRun(context.Background(), nil, MergeRunInput{RunID: runID, Verdict: "ship it"})
	if err != nil {
		t.Fatalf("second mergeRun: %v", err)
	}
	if fb.mergeCalls != 2 {
		t.Fatalf("merge POSTed %d times across two invocations, want 2 (no client-side skip)", fb.mergeCalls)
	}
	if !out.AlreadyRecorded || out.VerdictRecorded {
		t.Errorf("second-invoke flags = already:%v recorded:%v, want already:true recorded:false", out.AlreadyRecorded, out.VerdictRecorded)
	}
	if !out.MergeQueued {
		t.Error("merge_queued = false on the idempotent re-invoke, want the merge re-dispatched")
	}
}

// TestMergeRun_502ThenReinvoke_ReQueues pins the 502-retry contract: a first
// POST that 502s (merge_dispatch_failed — the verdict row durable) surfaces as
// a tool error, and a re-invoke re-POSTs and settles. Two POSTs total: the
// tool never swallows the 502 nor skips the resume POST.
func TestMergeRun_502ThenReinvoke_ReQueues(t *testing.T) {
	fb := &mergeRunFakeBackend{
		prURL:            "https://github.com/x/y/pull/7",
		stateBeforeMerge: "running",
		stateAfterMerge:  "succeeded",
		mergeStatuses:    []int{http.StatusBadGateway}, // first POST 502, then default 200
		mergeErrBody:     `{"error":{"code":"merge_dispatch_failed","message":"verdict durable, queue retryable"}}`,
		mergeResp:        MergeRunResult{MergeQueued: true, VerdictSequence: 5, AlreadyRecorded: true},
		auditEntries:     []AuditEntry{{Category: "pr_merged", Sequence: 6}},
	}
	srv := newMergeRunFakeBackend(t, fb)
	r := newMergeRunResolver(srv)
	runID := uuid.NewString()

	// First invoke: the POST 502s and surfaces as a tool error.
	if _, _, err := r.mergeRun(context.Background(), nil, MergeRunInput{RunID: runID, Verdict: "ship it"}); err == nil {
		t.Fatal("first mergeRun: expected the 502 to surface as an error")
	} else if !strings.Contains(err.Error(), "merge_dispatch_failed") {
		t.Errorf("first mergeRun err = %v, want merge_dispatch_failed", err)
	}

	// Re-invoke: the POST now succeeds and the merge settles.
	_, out, err := r.mergeRun(context.Background(), nil, MergeRunInput{RunID: runID, Verdict: "ship it"})
	if err != nil {
		t.Fatalf("second mergeRun: %v", err)
	}
	if out.Status != "merged" {
		t.Errorf("status = %q, want merged after the retry", out.Status)
	}
	if fb.mergeCalls != 2 {
		t.Errorf("merge POSTed %d times, want 2 (502 then re-queue)", fb.mergeCalls)
	}
}

// TestMergeRun_InvalidUUID_FailsLocally pins the fast local UUID reject: no
// GetRun, no POST.
func TestMergeRun_InvalidUUID_FailsLocally(t *testing.T) {
	fb := &mergeRunFakeBackend{prURL: "https://github.com/x/y/pull/7", stateBeforeMerge: "running"}
	srv := newMergeRunFakeBackend(t, fb)
	r := newMergeRunResolver(srv)

	_, _, err := r.mergeRun(context.Background(), nil, MergeRunInput{RunID: "not-a-uuid", Verdict: "ship it"})
	if err == nil || !strings.Contains(err.Error(), "not a valid UUID") {
		t.Fatalf("err = %v, want UUID parse error", err)
	}
	if fb.getRunCalls != 0 || fb.mergeCalls != 0 {
		t.Errorf("backend touched (getRun=%d merge=%d), want 0/0 on a local reject", fb.getRunCalls, fb.mergeCalls)
	}
}

// TestMergeRun_EmptyVerdict_FailsLocally pins the required-verdict reject
// before any HTTP hop.
func TestMergeRun_EmptyVerdict_FailsLocally(t *testing.T) {
	fb := &mergeRunFakeBackend{prURL: "https://github.com/x/y/pull/7", stateBeforeMerge: "running"}
	srv := newMergeRunFakeBackend(t, fb)
	r := newMergeRunResolver(srv)

	_, _, err := r.mergeRun(context.Background(), nil, MergeRunInput{RunID: uuid.NewString(), Verdict: "   "})
	if err == nil || !strings.Contains(err.Error(), "verdict is required") {
		t.Fatalf("err = %v, want verdict-required error", err)
	}
	if fb.mergeCalls != 0 {
		t.Errorf("merge POSTed %d times, want 0 on a local reject", fb.mergeCalls)
	}
}

// TestMergeRun_NoPRURL_RefusesBeforePOST pins the pre-flight refusal: a run
// with no PR URL can never merge, so the tool refuses after GetRun and before
// the POST.
func TestMergeRun_NoPRURL_RefusesBeforePOST(t *testing.T) {
	fb := &mergeRunFakeBackend{prURL: "", stateBeforeMerge: "running"}
	srv := newMergeRunFakeBackend(t, fb)
	r := newMergeRunResolver(srv)

	_, _, err := r.mergeRun(context.Background(), nil, MergeRunInput{RunID: uuid.NewString(), Verdict: "ship it"})
	if err == nil || !strings.Contains(err.Error(), "no pull request URL") {
		t.Fatalf("err = %v, want no-PR refusal", err)
	}
	if fb.mergeCalls != 0 {
		t.Errorf("merge POSTed %d times, want 0 — the refusal precedes the POST", fb.mergeCalls)
	}
}

// TestMergeRun_TerminalFailedRun_RefusesBeforePOST pins the pre-flight
// refusal of a failed/cancelled run.
func TestMergeRun_TerminalFailedRun_RefusesBeforePOST(t *testing.T) {
	for _, state := range []string{"failed", "cancelled"} {
		t.Run(state, func(t *testing.T) {
			fb := &mergeRunFakeBackend{prURL: "https://github.com/x/y/pull/7", stateBeforeMerge: state}
			srv := newMergeRunFakeBackend(t, fb)
			r := newMergeRunResolver(srv)

			_, _, err := r.mergeRun(context.Background(), nil, MergeRunInput{RunID: uuid.NewString(), Verdict: "ship it"})
			if err == nil || !strings.Contains(err.Error(), state) {
				t.Fatalf("err = %v, want a %s-run refusal", err, state)
			}
			if fb.mergeCalls != 0 {
				t.Errorf("merge POSTed %d times, want 0 — the refusal precedes the POST", fb.mergeCalls)
			}
		})
	}
}
