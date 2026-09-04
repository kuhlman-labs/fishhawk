package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/apitoken"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	runpkg "github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/server"
)

// unstableChecksMerger is a server.GitHubMerger that returns the multi-%w
// unstable-status sentinel githubclient.EnableAutoMerge produces on a
// checks-not-all-passed refusal (E67.56 / #2717), carrying the verbatim
// production GraphQL message from run a152a0a5.
type unstableChecksMerger struct{}

func (unstableChecksMerger) MergePullRequest(context.Context, *runpkg.Run) error {
	return fmt.Errorf("%w: %w: enable auto-merge: Pull request Pull request is in unstable status",
		forge.ErrValidation, forge.ErrPullRequestUnstableStatus)
}

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
	// mergeStickyStatus, when non-zero, is returned for EVERY POST (ignoring the
	// queue) — a test drives an always-checks-pending backend with it.
	mergeStickyStatus int
	mergeErrBody      string
	mergeResp         MergeRunResult
	// mergeVerdicts records the verdict body of each POST so a test can assert
	// every re-POST across the wait carries the same verdict (the tool never
	// skips the POST — endpoint-side idempotence).
	mergeVerdicts []string

	auditEntries []AuditEntry
	getRunCalls  int

	// auditReadCalls counts GET /audit hits. auditEntriesAfterReads gates when
	// the entries become visible: they are hidden until auditReadCalls exceeds
	// it (0 == visible from the first read). This lets a test hide an entry at
	// the fast-path read yet reveal it at the backstop / a later poll tick, so
	// the backstop's final-read-wins branch is actually driven.
	auditReadCalls         int
	auditEntriesAfterReads int
	// auditStatuses is a queue of HTTP statuses for successive audit GETs
	// (default 200 when exhausted) — a test injects a transient 500 to drive
	// the poll loop's "keep polling on a transient transport error" branch.
	auditStatuses []int
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
		if fb.mergeStickyStatus != 0 {
			status = fb.mergeStickyStatus
		}
		fb.mergeCalls++
		fb.mergeVerdicts = append(fb.mergeVerdicts, body.Verdict)
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
		fb.auditReadCalls++
		status := http.StatusOK
		if fb.auditReadCalls <= len(fb.auditStatuses) {
			status = fb.auditStatuses[fb.auditReadCalls-1]
		}
		visible := fb.auditReadCalls > fb.auditEntriesAfterReads
		matches := make([]AuditEntry, 0, len(fb.auditEntries))
		if visible {
			for _, e := range fb.auditEntries {
				if e.Category == category && e.Sequence > since {
					matches = append(matches, e)
				}
			}
		}
		fb.mu.Unlock()
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
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
	// E64.44 / #3161: the note reaching the WIRE must not assert the audit
	// check is required — Fishhawk publishes it, and whether it gates the
	// merge is what fishhawk_doctor's merge_gate rung reports. The const is
	// pinned by audit_check_wording_test.go; this asserts the shipped
	// response carries the corrected text rather than a stale copy.
	if strings.Contains(out.Note, "required fishhawk_audit_complete") ||
		strings.Contains(out.Note, "required review + the fishhawk_audit_complete") {
		t.Errorf("note over-claims the audit check as required (#3161): %q", out.Note)
	}
	if !strings.Contains(out.Note, "merge_gate") {
		t.Errorf("note must point at the merge_gate rung that reports what the forge enforces (#3161): %q", out.Note)
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

// TestMergeRun_SucceededRun_AwaitsQueuedMerge pins the correctness fix: a
// normal succeeded_pr_open run (feature_change is terminal-on-succeeded, PR
// not yet merged) must AWAIT its queued merge, not resolve run_terminal the
// instant the POST returns. succeeded predates the verdict anchor and no
// pr_merged entry has landed yet, so with the backstop no longer arming on
// succeeded the bounded await resolves timeout (resumable), never
// run_terminal.
func TestMergeRun_SucceededRun_AwaitsQueuedMerge(t *testing.T) {
	fb := &mergeRunFakeBackend{
		prURL:            "https://github.com/x/y/pull/7",
		stateBeforeMerge: "succeeded", // already terminal-on-succeeded; merge still queued
		stateAfterMerge:  "succeeded",
		mergeResp:        MergeRunResult{MergeQueued: true, VerdictSequence: 5},
		auditEntries:     nil, // the merge webhook has not landed yet
	}
	srv := newMergeRunFakeBackend(t, fb)
	r := newMergeRunResolver(srv)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()

	_, out, err := r.mergeRun(ctx, nil, MergeRunInput{RunID: uuid.NewString(), Verdict: "ship it"})
	if err != nil {
		t.Fatalf("mergeRun: %v", err)
	}
	if out.Status != "timeout" {
		t.Fatalf("status = %q, want timeout — a succeeded run must await its queued merge, not resolve run_terminal", out.Status)
	}
	if out.NextAction != nil {
		t.Errorf("next_action = %+v, want nil on timeout", out.NextAction)
	}
	if !out.VerdictRecorded || !out.MergeQueued {
		t.Errorf("verdict flags = recorded:%v queued:%v, want both true", out.VerdictRecorded, out.MergeQueued)
	}
}

// TestMergeRun_Backstop_FinalReadWins drives the backstop's final-read-wins
// branch: the run flips FAILED while a pr_merged entry lands in the SAME
// window (hidden at the fast-path read, visible at the backstop's final
// read). The entry must win — status merged, not run_terminal — so a merge
// that actually settled is never reported as stranded.
func TestMergeRun_Backstop_FinalReadWins(t *testing.T) {
	fb := &mergeRunFakeBackend{
		prURL:                  "https://github.com/x/y/pull/7",
		stateBeforeMerge:       "running",
		stateAfterMerge:        "failed", // failed arms the backstop
		mergeResp:              MergeRunResult{MergeQueued: true, VerdictSequence: 5},
		auditEntries:           []AuditEntry{{Category: "pr_merged", Sequence: 6}},
		auditEntriesAfterReads: 1, // hidden at fast-path read #1, visible at backstop read #2
	}
	srv := newMergeRunFakeBackend(t, fb)
	r := newMergeRunResolver(srv)

	_, out, err := r.mergeRun(context.Background(), nil, MergeRunInput{RunID: uuid.NewString(), Verdict: "ship it"})
	if err != nil {
		t.Fatalf("mergeRun: %v", err)
	}
	if out.Status != "merged" {
		t.Fatalf("status = %q, want merged — the entry that landed at the terminal transition must win over the backstop", out.Status)
	}
	if out.RunState != "failed" {
		t.Errorf("run_state = %q, want failed (the run flipped terminal as the merge settled)", out.RunState)
	}
	if out.NextAction == nil || out.NextAction.Action != "post_merge" {
		t.Errorf("next_action = %+v, want the surfaced post_merge step on merged", out.NextAction)
	}
}

// TestMergeRun_PollLoop_TransientAuditError drives the poll loop's
// keep-polling-on-transient-transport-error branch: an audit read fails (500)
// with the poll deadline still live, so the loop continues rather than
// aborting, and the next tick's read settles the merge.
func TestMergeRun_PollLoop_TransientAuditError(t *testing.T) {
	fb := &mergeRunFakeBackend{
		prURL:            "https://github.com/x/y/pull/7",
		stateBeforeMerge: "running",
		stateAfterMerge:  "succeeded", // never arms the backstop; the only exits are the entry or timeout
		mergeResp:        MergeRunResult{MergeQueued: true, VerdictSequence: 5},
		auditEntries:     []AuditEntry{{Category: "pr_merged", Sequence: 6}},
		// read #1 (fast path) 200-empty, read #2 (first poll tick) 500 → continue,
		// read #3 (next tick) 200 with the entry now visible → merged.
		auditStatuses:          []int{http.StatusOK, http.StatusInternalServerError},
		auditEntriesAfterReads: 2,
	}
	srv := newMergeRunFakeBackend(t, fb)
	r := newMergeRunResolver(srv)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, out, err := r.mergeRun(ctx, nil, MergeRunInput{RunID: uuid.NewString(), Verdict: "ship it"})
	if err != nil {
		t.Fatalf("mergeRun: %v", err)
	}
	if out.Status != "merged" {
		t.Fatalf("status = %q, want merged — a transient audit-read error must keep polling, not abort", out.Status)
	}
	fb.mu.Lock()
	reads := fb.auditReadCalls
	fb.mu.Unlock()
	if reads < 3 {
		t.Errorf("audit reads = %d, want >= 3 (fast-path empty, transient 500, then the settling read)", reads)
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

// mergeChecksPendingBody is the backend's 409 merge_checks_pending envelope
// (E67.56 / #2717), carrying the durable verdict row's sequence in details.
const mergeChecksPendingBody = `{"error":{"code":"merge_checks_pending",` +
	`"message":"required checks have not all passed",` +
	`"details":{"verdict_sequence":61595,"reason":"checks_pending"}}}`

// mergeChecksPendingBodyNoSeq is the same 409 with NO details.verdict_sequence —
// a shape the handler does not emit today, but the tool's seen-pending decision
// must not depend on the sequence being present (E67.56 / #2717 concern 2). Used
// to drive the sawChecksPending decoupling: a checks-pending refusal with no
// sequence must still resolve to the resumable checkpoint, not a tool error.
const mergeChecksPendingBodyNoSeq = `{"error":{"code":"merge_checks_pending",` +
	`"message":"required checks have not all passed",` +
	`"details":{"reason":"checks_pending"}}}`

// TestMergeRun_ChecksPendingThenSuccess drives the wait-then-queue path
// (E67.56 / #2717): the backend answers 409 merge_checks_pending for the first
// two POSTs then 200, so the tool re-POSTs (more than once) WITHOUT erroring and
// falls through to the terminal await, resolving merged. Also the
// idempotence-shape assertion (plan 10d): every re-POST carries the same verdict
// body — the tool never skips the POST. Counterfactual: deleting isChecksPending's
// code check makes this RED (the first 409 becomes a tool error).
func TestMergeRun_ChecksPendingThenSuccess(t *testing.T) {
	fb := &mergeRunFakeBackend{
		prURL:            "https://github.com/x/y/pull/7",
		stateBeforeMerge: "running",
		stateAfterMerge:  "succeeded",
		mergeStatuses:    []int{http.StatusConflict, http.StatusConflict}, // 409, 409, then default 200
		mergeErrBody:     mergeChecksPendingBody,
		mergeResp:        MergeRunResult{MergeQueued: true, VerdictSequence: 61595, AlreadyRecorded: true, PRURL: "https://github.com/x/y/pull/7"},
		auditEntries:     []AuditEntry{{Category: "pr_merged", Sequence: 61596}},
	}
	srv := newMergeRunFakeBackend(t, fb)
	r := newMergeRunResolver(srv) // reviewPollInterval = 1ms

	_, out, err := r.mergeRun(context.Background(), nil, MergeRunInput{RunID: uuid.NewString(), Verdict: "ship it"})
	if err != nil {
		t.Fatalf("mergeRun: %v — a checks-pending 409 must be WAITED on, not surfaced as an error", err)
	}
	if out.Status != "merged" {
		t.Fatalf("status = %q, want merged (the merge queues once the checks clear)", out.Status)
	}
	fb.mu.Lock()
	calls, verdicts := fb.mergeCalls, append([]string(nil), fb.mergeVerdicts...)
	fb.mu.Unlock()
	if calls < 3 {
		t.Errorf("merge POSTed %d times, want >= 3 (two checks-pending waits then the queue)", calls)
	}
	for i, v := range verdicts {
		if v != "ship it" {
			t.Errorf("POST %d verdict = %q, want 'ship it' (every re-POST carries the same verdict)", i, v)
		}
	}
}

// TestMergeRun_ChecksPendingUntilDeadline pins the resumable checkpoint
// (E67.56 / #2717): the backend answers 409 merge_checks_pending on every POST,
// so the bounded wait expires and the tool returns status=checks_pending — NOT a
// tool error. Binding conditions 1 + 4: the Message names the checks-not-all-passed
// precondition AND the already-FAILED possibility and does NOT say "retry the
// merge"; VerdictSequence is surfaced while VerdictRecorded / AlreadyRecorded /
// MergeQueued are all false (provenance is not inferred). Counterfactual:
// deleting isChecksPending's code check makes this RED (the 409 becomes a tool
// error).
func TestMergeRun_ChecksPendingUntilDeadline(t *testing.T) {
	fb := &mergeRunFakeBackend{
		prURL:             "https://github.com/x/y/pull/7",
		stateBeforeMerge:  "running",
		stateAfterMerge:   "running",
		mergeStickyStatus: http.StatusConflict, // every POST 409 checks-pending
		mergeErrBody:      mergeChecksPendingBody,
	}
	srv := newMergeRunFakeBackend(t, fb)
	r := newMergeRunResolver(srv) // reviewPollInterval = 1ms

	// A short ctx deadline bounds the shared wait without a real multi-second wait.
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	_, out, err := r.mergeRun(ctx, nil, MergeRunInput{RunID: uuid.NewString(), Verdict: "ship it"})
	if err != nil {
		t.Fatalf("mergeRun: %v — a checks-pending deadline must resolve to a status, not a tool error", err)
	}
	if out.Status != "checks_pending" {
		t.Fatalf("status = %q, want checks_pending", out.Status)
	}
	// Binding condition 1: honest wording naming both outcomes, no failing remedy.
	if !strings.Contains(out.Message, "have not all passed") {
		t.Errorf("message must name the checks-not-all-passed precondition: %q", out.Message)
	}
	if !strings.Contains(out.Message, "FAILED") {
		t.Errorf("message must name the already-failed-check possibility: %q", out.Message)
	}
	if strings.Contains(out.Message, "retry the merge") {
		t.Errorf("message must NOT prescribe 'retry the merge': %q", out.Message)
	}
	// Binding condition 4: report the sequence, but infer no provenance.
	if out.VerdictSequence != 61595 {
		t.Errorf("verdict_sequence = %d, want 61595 (surfaced from details)", out.VerdictSequence)
	}
	if out.VerdictRecorded || out.AlreadyRecorded {
		t.Errorf("provenance flags = recorded:%v already:%v, want both false on checks_pending", out.VerdictRecorded, out.AlreadyRecorded)
	}
	if out.MergeQueued {
		t.Error("merge_queued = true on checks_pending, want false (nothing was queued)")
	}
	fb.mu.Lock()
	calls := fb.mergeCalls
	fb.mu.Unlock()
	if calls < 2 {
		t.Errorf("merge POSTed %d times, want >= 2 (the tool re-POSTs across the wait)", calls)
	}
}

// TestMergeRun_ChecksPendingDeadlineBranch pins the checks-pending loop's OWN
// deadline-exhaustion branch (E67.56 / #2717): with a responsive always-409
// backend and NO ctx deadline, the ONLY thing that can end the wait is the
// tool's own clamped deadline (TimeoutSeconds=1 → clampAwaitTimeout → 1s). So
// the `!time.Now().Before(deadline)` return and the timer-clamped final wait are
// reached directly through the tool's deadline expiring, not approximated via a
// parent-ctx cancellation. Counterfactual: deleting the loop's deadline return
// makes the wait never end under a non-cancelling ctx → the test hangs (RED).
func TestMergeRun_ChecksPendingDeadlineBranch(t *testing.T) {
	fb := &mergeRunFakeBackend{
		prURL:             "https://github.com/x/y/pull/7",
		stateBeforeMerge:  "running",
		stateAfterMerge:   "running",
		mergeStickyStatus: http.StatusConflict, // every POST 409 checks-pending
		mergeErrBody:      mergeChecksPendingBody,
	}
	srv := newMergeRunFakeBackend(t, fb)
	r := newMergeRunResolver(srv) // reviewPollInterval = 1ms

	// context.Background() never cancels — the shared deadline is the sole exit.
	_, out, err := r.mergeRun(context.Background(), nil, MergeRunInput{RunID: uuid.NewString(), Verdict: "ship it", TimeoutSeconds: 1})
	if err != nil {
		t.Fatalf("mergeRun: %v — the clamped-deadline expiry must resolve to checks_pending, not a tool error", err)
	}
	if out.Status != "checks_pending" {
		t.Fatalf("status = %q, want checks_pending (reached via the tool's own deadline, not ctx cancel)", out.Status)
	}
	if out.VerdictSequence != 61595 {
		t.Errorf("verdict_sequence = %d, want 61595 (surfaced from details)", out.VerdictSequence)
	}
	if out.WaitedSeconds < 1 {
		t.Errorf("waited_seconds = %f, want >= ~1 (the shared 1s deadline elapsed)", out.WaitedSeconds)
	}
}

// TestMergeRun_ChecksPendingPOSTBoundedByDeadline pins the high-correctness fix
// (E67.56 / #2717): a re-POST after a checks-pending refusal must not outlive the
// shared deadline. The backend returns 409 merge_checks_pending on the first POST
// (setting the seen-pending flag), then HANGS every subsequent POST. Because the
// tool bounds each POST by a context derived from the shared deadline, the hung
// re-POST is cancelled AT the deadline and the tool returns status=checks_pending
// within its wall-clock budget rather than blocking on the hung backend
// indefinitely.
//
// The first 409 deliberately OMITS details.verdict_sequence, so this test also
// pins concern 2's sawChecksPending decoupling: the bounded-wait cancellation
// must resolve to the checkpoint using a dedicated seen-pending flag, not
// checksSeq != 0 (which would be 0 here and misroute to a tool error).
//
// It is a single counterfactual vehicle for all three changed controls:
//   - POST on the unbounded parent ctx (drop deadlineCtx): the tool blocks on the
//     hung backend until the 6s server fallback → elapsed blows past the ~1s
//     budget, and status is not checks_pending → RED.
//   - guard on ctx.Err() instead of deadlineCtx.Err(): the Background parent ctx
//     never errs, so the deadline-cancelled POST surfaces as a tool error → RED.
//   - seen-pending flag as checksSeq != 0: 0 here, so the cancelled POST surfaces
//     as a tool error → RED.
func TestMergeRun_ChecksPendingPOSTBoundedByDeadline(t *testing.T) {
	var calls int32
	prURL := "https://github.com/x/y/pull/7"
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v0/runs/{run_id}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		id, perr := uuid.Parse(r.PathValue("run_id"))
		if perr != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(Run{ID: id.String(), State: "running", PullRequestURL: &prURL})
	})
	mux.HandleFunc("POST /v0/runs/{run_id}/merge", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if atomic.AddInt32(&calls, 1) == 1 {
			// First POST: the checks-pending 409 that sets the seen-pending flag.
			// No verdict_sequence — the seen-pending decision must not need it.
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(mergeChecksPendingBodyNoSeq))
			return
		}
		// Every subsequent POST HANGS until the client cancels its request (the
		// deadline-derived context) or a generous fallback fires. When the tool
		// bounds the POST by the shared deadline the client cancellation propagates
		// to r.Context() and the handler returns promptly.
		select {
		case <-r.Context().Done():
		case <-time.After(6 * time.Second):
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	r := &runResolver{
		api:                newAPIClient(config{backendURL: srv.URL, apiToken: "tok-test"}),
		reviewPollInterval: time.Millisecond,
	}

	// context.Background(): only the tool's OWN shared deadline (TimeoutSeconds=1)
	// can end the wait, so the deadline must both stop the retry loop AND cancel
	// the in-flight hung POST.
	begin := time.Now()
	_, out, err := r.mergeRun(context.Background(), nil, MergeRunInput{RunID: uuid.NewString(), Verdict: "ship it", TimeoutSeconds: 1})
	elapsed := time.Since(begin)
	if err != nil {
		t.Fatalf("mergeRun: %v — a hung re-POST past the deadline must resolve to checks_pending, not a tool error", err)
	}
	if out.Status != "checks_pending" {
		t.Fatalf("status = %q, want checks_pending", out.Status)
	}
	if out.VerdictSequence != 0 {
		t.Errorf("verdict_sequence = %d, want 0 (the 409 carried no sequence — provenance is not inferred)", out.VerdictSequence)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("mergeRun blocked %s, want it bounded by the ~1s shared deadline — the in-flight POST outlived the deadline", elapsed)
	}
}

// TestMergeRun_GenuineError_PassthroughNoRetry is the counterfactual guard that
// the checks-pending wait did NOT swallow every failure: a 502 merge_dispatch_failed
// and a 409 acceptance_gate_not_passed each still surface as a TOOL ERROR after
// exactly ONE POST (zero retries).
func TestMergeRun_GenuineError_PassthroughNoRetry(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		errBody string
		wantMsg string
	}{
		{
			name:    "502 merge_dispatch_failed",
			status:  http.StatusBadGateway,
			errBody: `{"error":{"code":"merge_dispatch_failed","message":"verdict durable, queue retryable"}}`,
			wantMsg: "merge_dispatch_failed",
		},
		{
			name:    "409 acceptance_gate_not_passed",
			status:  http.StatusConflict,
			errBody: `{"error":{"code":"acceptance_gate_not_passed","message":"acceptance gate not passed"}}`,
			wantMsg: "acceptance_gate_not_passed",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fb := &mergeRunFakeBackend{
				prURL:             "https://github.com/x/y/pull/7",
				stateBeforeMerge:  "running",
				stateAfterMerge:   "running",
				mergeStickyStatus: tc.status,
				mergeErrBody:      tc.errBody,
			}
			srv := newMergeRunFakeBackend(t, fb)
			r := newMergeRunResolver(srv)

			_, _, err := r.mergeRun(context.Background(), nil, MergeRunInput{RunID: uuid.NewString(), Verdict: "ship it"})
			if err == nil {
				t.Fatalf("mergeRun: nil error, want the %s surfaced as a tool error (not swallowed by the wait)", tc.wantMsg)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("err = %v, want it to name %s", err, tc.wantMsg)
			}
			fb.mu.Lock()
			calls := fb.mergeCalls
			fb.mu.Unlock()
			if calls != 1 {
				t.Errorf("merge POSTed %d times, want exactly 1 (a genuine error is not retried)", calls)
			}
		})
	}
}

// TestMergeRun_CrossBoundary_RealServerChecksPending is binding condition 2's
// TRUE end-to-end test (E67.56 / #2717): it mounts the REAL server merge handler
// (with a merger returning the unstable-wrapped sentinel — the same fmt.Errorf
// multi-%w shape githubclient produces) on an httptest server, and drives the
// REAL mcpserver apiClient and the REAL merge tool handler against it. Unlike the
// hand-rolled table cases, NOTHING hand-authors the 409 envelope — the real
// handler emits it and the real client reads it, so a mismatch between the two
// would be caught here. The tool WAITS on the 409 (bounded by a short ctx) and
// returns status=checks_pending, never a tool error, and the verdict row is
// durable in the real audit repo across the wait. Counterfactual: deleting the
// server's 409 branch turns this RED (the tool sees the generic 502 and returns
// a tool error).
func TestMergeRun_CrossBoundary_RealServerChecksPending(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	runRepo := runpkg.NewPostgresRepository(pool)
	auditRepo := audit.NewPostgresRepository(pool)

	row, err := runRepo.CreateRun(ctx, runpkg.CreateRunParams{
		Repo: "x/y", WorkflowID: "feature_change", WorkflowSHA: "abc", TriggerSource: runpkg.TriggerCLI,
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	if _, err := runRepo.TransitionRun(ctx, row.ID, runpkg.StateRunning); err != nil {
		t.Fatalf("transition running: %v", err)
	}
	if _, err := runRepo.SetRunPullRequestURL(ctx, row.ID, "https://github.com/x/y/pull/7"); err != nil {
		t.Fatalf("set pr url: %v", err)
	}

	const bearer = "fhk_merge_checks_e2e"
	tokRepo := &stubMCPAPITokens{tok: &apitoken.Token{
		ID: uuid.New(), Subject: "github:op", Scopes: []string{"write:approvals"}, PlainText: bearer,
	}}
	s := server.New(server.Config{
		RunRepo:      runRepo,
		AuditRepo:    auditRepo,
		APITokenRepo: tokRepo,
		GateMerger:   unstableChecksMerger{},
	})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	r := &runResolver{
		api:                newAPIClient(config{backendURL: ts.URL, apiToken: bearer}),
		reviewPollInterval: time.Millisecond,
	}

	// A short ctx deadline bounds the shared wait; the real handler answers 409
	// merge_checks_pending on every POST (the merger always refuses unstable).
	waitCtx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
	defer cancel()

	_, out, err := r.mergeRun(waitCtx, nil, MergeRunInput{RunID: row.ID.String(), Verdict: "ship it"})
	if err != nil {
		t.Fatalf("mergeRun (cross-boundary): %v — the REAL server's 409 merge_checks_pending must be waited on, not surfaced as an error", err)
	}
	if out.Status != "checks_pending" {
		t.Fatalf("status = %q, want checks_pending (real handler → real client → real tool)", out.Status)
	}
	if strings.Contains(out.Message, "retry the merge") {
		t.Errorf("message must NOT prescribe 'retry the merge': %q", out.Message)
	}
	if !strings.Contains(out.Message, "FAILED") {
		t.Errorf("message must name the already-failed-check possibility: %q", out.Message)
	}
	// The verdict row IS durable across the wait — read it back from the REAL
	// audit repo (committed state, binding condition 4's durability claim).
	entries, err := auditRepo.ListForRunByCategory(ctx, row.ID, "merge_verdict_recorded")
	if err != nil {
		t.Fatalf("list merge verdicts: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("merge_verdict_recorded rows = %d, want 1 (verdict durable across the checks-pending wait)", len(entries))
	}
}

// TestMergeVerdictSequenceFrom pins the best-effort details.verdict_sequence
// reader across every value shape it must tolerate (E67.56 / #2717): a nil map,
// the float64 a JSON decode yields, a json.Number, a native int64, and the
// no-provenance fallbacks (absent key, wrong type) that both return 0.
func TestMergeVerdictSequenceFrom(t *testing.T) {
	cases := []struct {
		name    string
		details map[string]any
		want    int64
	}{
		{name: "nil map", details: nil, want: 0},
		{name: "float64 (json decode)", details: map[string]any{"verdict_sequence": float64(61595)}, want: 61595},
		{name: "json.Number", details: map[string]any{"verdict_sequence": json.Number("42")}, want: 42},
		{name: "native int64", details: map[string]any{"verdict_sequence": int64(9)}, want: 9},
		{name: "absent key", details: map[string]any{"reason": "checks_pending"}, want: 0},
		{name: "wrong type", details: map[string]any{"verdict_sequence": "not-a-number"}, want: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := mergeVerdictSequenceFrom(tc.details); got != tc.want {
				t.Errorf("mergeVerdictSequenceFrom(%v) = %d, want %d", tc.details, got, tc.want)
			}
		})
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

// mergeConflictingBody is the backend's 409 merge_conflicting envelope (E64.14 /
// #3109), carrying pr_url + mergeable_state in details.
const mergeConflictingBody = `{"error":{"code":"merge_conflicting",` +
	`"message":"the pull request has a merge conflict against its base",` +
	`"details":{"pr_url":"https://github.com/x/y/pull/7","mergeable_state":"dirty"}}}`

// TestMergeRunToolConflictingReturnsImmediately (c5) pins the immediate
// conflict return: a 409 merge_conflicting yields status=conflicting after
// EXACTLY ONE POST — the count is what proves the tool does NOT re-arm a doomed
// wait (unlike checks_pending it does not poll or re-POST) — and returns well
// inside the timeout budget. The fake backend is an in-test httptest server, so
// deleting the isConflicting arm cannot pass for the unrelated reason of an
// unreachable address. Counterfactual: deleting the isConflicting arm in the
// POST loop makes this RED (the 409 surfaces as a bare tool error).
func TestMergeRunToolConflictingReturnsImmediately(t *testing.T) {
	fb := &mergeRunFakeBackend{
		prURL:             "https://github.com/x/y/pull/7",
		stateBeforeMerge:  "running",
		stateAfterMerge:   "running",
		mergeStickyStatus: http.StatusConflict, // every POST 409 conflicting
		mergeErrBody:      mergeConflictingBody,
	}
	srv := newMergeRunFakeBackend(t, fb)
	r := newMergeRunResolver(srv)

	start := time.Now()
	// A generous timeout: the point is the tool returns IMMEDIATELY, not that the
	// budget expired. A conflicting return must not consume the wait budget.
	_, out, err := r.mergeRun(context.Background(), nil, MergeRunInput{RunID: uuid.NewString(), Verdict: "ship it", TimeoutSeconds: 600})
	if err != nil {
		t.Fatalf("mergeRun: %v — a conflict 409 must be returned as status=conflicting, not a tool error", err)
	}
	if out.Status != "conflicting" {
		t.Fatalf("status = %q, want conflicting", out.Status)
	}
	if out.MergeQueued || out.VerdictRecorded || out.AlreadyRecorded {
		t.Errorf("flags = queued:%v recorded:%v already:%v, want all false (nothing queued, no verdict)", out.MergeQueued, out.VerdictRecorded, out.AlreadyRecorded)
	}
	if out.PRURL != "https://github.com/x/y/pull/7" {
		t.Errorf("pr_url = %q, want the details pr_url", out.PRURL)
	}
	if !strings.Contains(out.Message, "fishhawk_vouch_commit") {
		t.Errorf("message must name the resolve-then-vouch path: %q", out.Message)
	}
	if !strings.Contains(out.Message, "dirty") {
		t.Errorf("message should surface mergeable_state: %q", out.Message)
	}
	fb.mu.Lock()
	calls := fb.mergeCalls
	fb.mu.Unlock()
	if calls != 1 {
		t.Errorf("merge POSTed %d times, want EXACTLY 1 (conflict does not re-arm a doomed wait)", calls)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("mergeRun took %s, want an immediate return well inside the 600s budget", elapsed)
	}
}
