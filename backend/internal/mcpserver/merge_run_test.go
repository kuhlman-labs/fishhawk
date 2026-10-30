package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	"github.com/modelcontextprotocol/go-sdk/mcp"

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

func (unstableChecksMerger) MergePullRequest(context.Context, *runpkg.Run, string) error {
	return fmt.Errorf("%w: %w: enable auto-merge: Pull request Pull request is in unstable status",
		forge.ErrValidation, forge.ErrPullRequestUnstableStatus)
}

// --- fishhawk_merge_run (E48.7 / #1954) ---

// TestMergeRun_ToolDescriptionNamesHeldRemedy pins (#4302, M9) that the
// WIRE-VISIBLE fishhawk_merge_run description tells an operator agent how a
// merge_candidate_held run clears: the periodic pump self-heals a stalled line
// and fishhawk_rebase_run_branch pumps the base manually — and does NOT say
// there is "nothing to invoke". Counterfactual: reverting the
// merge_candidate_held paragraph in merge_run.go to "... and starts the pass —
// nothing to invoke." → both remedy phrases vanish and the forbidden one
// appears → RED. The description hard-wraps mid-phrase, so whitespace is
// normalized before matching.
func TestMergeRun_ToolDescriptionNamesHeldRemedy(t *testing.T) {
	desc := wireMergeRunDescription(t)
	for _, want := range []string{
		"merge_candidate_held",
		"a stalled line self-heals on the periodic pump",
		"fishhawk_rebase_run_branch pumps the base manually",
	} {
		if !strings.Contains(desc, want) {
			t.Errorf("fishhawk_merge_run description missing %q:\n%s", want, desc)
		}
	}
	if strings.Contains(desc, "nothing to invoke") {
		t.Errorf("fishhawk_merge_run description says %q; a stalled held line has a remedy:\n%s", "nothing to invoke", desc)
	}
}

// TestMergeRun_ToolDescriptionNamesHeadMoved pins (#4641) that the
// WIRE-VISIBLE fishhawk_merge_run description lists merge_head_moved as a 409
// TOOL ERROR with its re-invoke guidance: the merge was pinned to the head the
// merge-candidate gate admitted, only precheck / rest_sha are forge-guaranteed
// "nothing applied" (approval condition C3), and a re-invoke re-gates at the
// new head. Counterfactual: deleting the merge_head_moved entry from the
// description's Tool errors list → every phrase below vanishes → RED.
func TestMergeRun_ToolDescriptionNamesHeadMoved(t *testing.T) {
	desc := wireMergeRunDescription(t)
	errorsAt := strings.Index(desc, "Tool errors:")
	if errorsAt < 0 {
		t.Fatalf("fishhawk_merge_run description has no Tool errors list:\n%s", desc)
	}
	toolErrors := desc[errorsAt:]
	for _, want := range []string{
		"merge_head_moved (409",
		"PINNED to the head the merge-candidate gate admitted (details.head_sha)",
		"details.refusal_basis precheck / rest_sha means the forge applied nothing",
		"graphql_reread / unknown is inferred, so auto-merge may be armed at the pinned head",
		"Re-invoke once the head settles",
		"the re-invoke re-gates at the new head",
		// #4749: graphql_reread holds the claim dispatch_failed.
		"left dispatch_failed on graphql_reread, needing the operator re-arm",
	} {
		if !strings.Contains(toolErrors, want) {
			t.Errorf("fishhawk_merge_run Tool errors list missing %q:\n%s", want, toolErrors)
		}
	}
}

// wireMergeRunDescription lists the tools over an in-memory MCP session and
// returns fishhawk_merge_run's wire description, whitespace-normalized because
// the description hard-wraps mid-phrase.
func wireMergeRunDescription(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	srv := mcp.NewServer(&mcp.Implementation{Name: "fishhawk", Version: "test"}, nil)
	registerMergeRun(srv, nil)

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := srv.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	defer func() { _ = serverSession.Close() }()
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer func() { _ = clientSession.Close() }()

	res, err := clientSession.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	var raw string
	for _, tool := range res.Tools {
		if tool.Name == "fishhawk_merge_run" {
			raw = tool.Description
		}
	}
	if raw == "" {
		t.Fatal("fishhawk_merge_run is not wire-visible")
	}
	return strings.Join(strings.Fields(raw), " ")
}

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
	repo             string // the run's repo on GET /v0/runs/{id}
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
	// mergeErrBodies, when set, overrides mergeErrBody per POST index (the
	// last entry repeats) — a test drives a checks_pending-then-refusal pair.
	mergeErrBodies []string
	mergeResp      MergeRunResult
	// mergeRawBody, when set, is the success body served VERBATIM instead of
	// mergeResp — a test pins the wire bytes the backend writes (ADR-093's
	// 202 queued answer) rather than a re-encoding of the client mirror.
	mergeRawBody string
	// mergeVerdicts records the verdict body of each POST so a test can assert
	// every re-POST across the wait carries the same verdict (the tool never
	// skips the POST — endpoint-side idempotence).
	mergeVerdicts []string
	// mergeRawBodies records each POST's raw body, so a test can assert the
	// merge overlap guard override (allow_overlap / overlap_reason, #4119)
	// rides every (re-)POST byte-for-byte, or is absent from the wire.
	mergeRawBodies []string

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

	// stages / stagesStatus back the GET /v0/runs/{run_id}/stages handler the
	// E64.63 (#3222) acceptance-blocker advisory reads. stagesStatus non-zero
	// forces that status on every stages GET, which is how degrade D1 (the
	// stages read FAILS -> the message is byte-identical) is driven.
	stages       []Stage
	stagesStatus int
	// acceptanceAuditStatus non-zero forces that status on the
	// category=acceptance_reopened audit GET ONLY, leaving the terminal-merge
	// poll's own reads alone — degrade D3 (the audit read FAILS -> the GENERIC
	// wording still ships).
	acceptanceAuditStatus int

	// E83.60 / #4118: GET /v0/runs/{run_id}/scope-overlap. scopeOverlapBody,
	// when non-empty, is served with scopeOverlapStatus (default 200);
	// otherwise the route answers 404 (an older backend). scopeOverlapCalls
	// counts reads.
	scopeOverlapBody   string
	scopeOverlapStatus int
	scopeOverlapCalls  int
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
		_ = json.NewEncoder(w).Encode(Run{ID: id.String(), Repo: fb.repo, State: state, PullRequestURL: prPtr})
	})

	mux.HandleFunc("POST /v0/runs/{run_id}/merge", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if _, perr := uuid.Parse(r.PathValue("run_id")); perr != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		rawBody, _ := io.ReadAll(r.Body)
		var body mergeRunRequest
		_ = json.Unmarshal(rawBody, &body)
		fb.mu.Lock()
		fb.mergeRawBodies = append(fb.mergeRawBodies, string(rawBody))
		status := http.StatusOK
		if fb.mergeCalls < len(fb.mergeStatuses) {
			status = fb.mergeStatuses[fb.mergeCalls]
		}
		if fb.mergeStickyStatus != 0 {
			status = fb.mergeStickyStatus
		}
		errBody := fb.mergeErrBody
		if n := len(fb.mergeErrBodies); n > 0 {
			errBody = fb.mergeErrBodies[min(fb.mergeCalls, n-1)]
		}
		fb.mergeCalls++
		fb.mergeVerdicts = append(fb.mergeVerdicts, body.Verdict)
		resp := fb.mergeResp
		if status == http.StatusOK {
			fb.merged = true
		}
		fb.mu.Unlock()
		w.WriteHeader(status)
		// A 2xx other than 200 (the ADR-093 202 queued answer) carries the
		// success body too, and merges nothing.
		if status >= http.StatusBadRequest {
			if errBody != "" {
				_, _ = w.Write([]byte(errBody))
			}
			return
		}
		if fb.mergeRawBody != "" {
			_, _ = w.Write([]byte(fb.mergeRawBody))
			return
		}
		_ = json.NewEncoder(w).Encode(resp)
	})

	mux.HandleFunc("GET /v0/runs/{run_id}/stages", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if _, perr := uuid.Parse(r.PathValue("run_id")); perr != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		fb.mu.Lock()
		status := fb.stagesStatus
		items := append([]Stage(nil), fb.stages...)
		fb.mu.Unlock()
		if status != 0 {
			w.WriteHeader(status)
			return
		}
		_ = json.NewEncoder(w).Encode(listStagesResult{Items: items})
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
		limit := 1
		if l := r.URL.Query().Get("limit"); l != "" {
			limit, _ = strconv.Atoi(l)
		}
		fb.mu.Lock()
		// The acceptance-blocker advisory's read is counted and gated
		// SEPARATELY from the terminal-merge poll's: it is not part of the
		// poll sequence auditStatuses / auditEntriesAfterReads describe.
		if category == categoryAcceptanceReopened {
			status := fb.acceptanceAuditStatus
			matches := make([]AuditEntry, 0, len(fb.auditEntries))
			for _, e := range fb.auditEntries {
				if e.Category == category {
					matches = append(matches, e)
				}
			}
			fb.mu.Unlock()
			if status != 0 {
				w.WriteHeader(status)
				return
			}
			_ = json.NewEncoder(w).Encode(listAuditResult{Items: matches})
			return
		}
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
		if limit > 0 && len(matches) > limit {
			matches = matches[:limit]
		}
		_ = json.NewEncoder(w).Encode(listAuditResult{Items: matches})
	})

	mux.HandleFunc("GET /v0/runs/{run_id}/scope-overlap", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fb.mu.Lock()
		fb.scopeOverlapCalls++
		body, status := fb.scopeOverlapBody, fb.scopeOverlapStatus
		fb.mu.Unlock()
		if body == "" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
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
	// The run NEVER recorded a spec, so it declares no ADR-112 forge-approval
	// gate; a recorded SHA with no cached spec is the legacy row shape the fold
	// treats as undetermined (C4).
	if _, err := pool.Exec(ctx, `UPDATE runs SET workflow_sha = '' WHERE id = $1`, row.ID); err != nil {
		t.Fatalf("clear workflow_sha: %v", err)
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

// --- E64.59 / #3190: the checks_pending message names the audit-complete cause

// mergeChecksPendingBodyWithAuditDetail is the backend's ENRICHED 409
// (E64.59 / #3190): details carries the audit_complete_missing list naming the
// fix-up-re-opened acceptance stage and the re-dispatch action.
const mergeChecksPendingBodyWithAuditDetail = `{"error":{"code":"merge_checks_pending",` +
	`"message":"required checks have not all passed",` +
	`"details":{"verdict_sequence":61595,"reason":"checks_pending",` +
	`"audit_complete_state":"pending",` +
	`"audit_complete_missing":[{"kind":"stage_not_terminal",` +
	`"detail":"acceptance stage 88538e1a was re-opened by a fix-up push and has not been re-run; ` +
	`re-dispatch the acceptance stage (fishhawk_dispatch_stage, stage acceptance)"}]}}}`

// TestChecksPendingOutput_CarriesAuditCompleteDetail is the LAST hop of the
// serialization boundary: fishhawk_merge_run is where an operator first observes
// the strand, so the resumable checkpoint must name the blocking stage and the
// action rather than describing "required checks" generically. Counterfactual:
// deleting the auditCompleteDetailSuffix call in checksPendingOutput makes this
// RED.
func TestChecksPendingOutput_CarriesAuditCompleteDetail(t *testing.T) {
	fb := &mergeRunFakeBackend{
		prURL:             "https://github.com/x/y/pull/7",
		stateBeforeMerge:  "running",
		stateAfterMerge:   "running",
		mergeStickyStatus: http.StatusConflict,
		mergeErrBody:      mergeChecksPendingBodyWithAuditDetail,
	}
	srv := newMergeRunFakeBackend(t, fb)
	r := newMergeRunResolver(srv)

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	_, out, err := r.mergeRun(ctx, nil, MergeRunInput{RunID: uuid.NewString(), Verdict: "ship it"})
	if err != nil {
		t.Fatalf("mergeRun: %v", err)
	}
	if out.Status != "checks_pending" {
		t.Fatalf("status = %q, want checks_pending", out.Status)
	}
	for _, want := range []string{
		"fishhawk_audit_complete is pending because:",
		"88538e1a",
		"re-opened by a fix-up push",
		"fishhawk_dispatch_stage",
	} {
		if !strings.Contains(out.Message, want) {
			t.Errorf("message = %q, want it to contain %q", out.Message, want)
		}
	}
	// The pre-#3190 wording survives — this is purely additive.
	if !strings.Contains(out.Message, "have not all passed") {
		t.Errorf("the generic wording must survive: %q", out.Message)
	}
}

// TestChecksPendingOutput_MalformedAuditDetail_DegradesToFixedMessage pins the
// DEFENSIVE branch. details crossed an HTTP JSON boundary, so every wrong shape
// must degrade to today's fixed message and never to a tool error — the tool
// must not fail to report a merge block because the block's explanation was
// malformed.
//
// One case per rejected shape, so a guard that only handles some of them fails
// here rather than panicking in front of an operator.
func TestChecksPendingOutput_MalformedAuditDetail_DegradesToFixedMessage(t *testing.T) {
	const prefix = `{"error":{"code":"merge_checks_pending","message":"required checks have not all passed","details":{"reason":"checks_pending",`
	cases := []struct {
		name string
		body string
	}{
		{"absent", `{"error":{"code":"merge_checks_pending","message":"m","details":{"reason":"checks_pending"}}}`},
		{"null", prefix + `"audit_complete_missing":null}}}`},
		{"not_an_array", prefix + `"audit_complete_missing":"stage_not_terminal"}}}`},
		{"empty_array", prefix + `"audit_complete_missing":[]}}}`},
		{"items_not_objects", prefix + `"audit_complete_missing":["stage_not_terminal",7]}}}`},
		{"detail_not_a_string", prefix + `"audit_complete_missing":[{"kind":"stage_not_terminal","detail":42}]}}}`},
		{"detail_blank", prefix + `"audit_complete_missing":[{"kind":"stage_not_terminal","detail":"   "}]}}}`},
		{"detail_absent", prefix + `"audit_complete_missing":[{"kind":"stage_not_terminal"}]}}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fb := &mergeRunFakeBackend{
				prURL:             "https://github.com/x/y/pull/7",
				stateBeforeMerge:  "running",
				stateAfterMerge:   "running",
				mergeStickyStatus: http.StatusConflict,
				mergeErrBody:      tc.body,
			}
			srv := newMergeRunFakeBackend(t, fb)
			r := newMergeRunResolver(srv)
			ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
			defer cancel()

			_, out, err := r.mergeRun(ctx, nil, MergeRunInput{RunID: uuid.NewString(), Verdict: "ship it"})
			if err != nil {
				t.Fatalf("mergeRun: %v — a malformed detail must never become a tool error", err)
			}
			if out.Status != "checks_pending" {
				t.Fatalf("status = %q, want checks_pending", out.Status)
			}
			if strings.Contains(out.Message, "fishhawk_audit_complete is pending because") {
				t.Errorf("a malformed detail must degrade to the fixed message: %q", out.Message)
			}
			if !strings.Contains(out.Message, "have not all passed") {
				t.Errorf("the fixed message must survive: %q", out.Message)
			}
		})
	}
}

// --- E64.63 / #3222: the acceptance-blocker advisory on the merge surface ---
//
// A merge blocked by a NON-TERMINAL acceptance stage cannot fire: the
// fishhawk_audit_complete check is pending on that very stage. Before #3222
// fishhawk_merge_run reported only "nothing landed within Ns" and the operator,
// with no way to see the cause, reached for an admin bypass twice. These tests
// drive one case per named branch and one per named DEGRADE.

const advisoryAccStageID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

// mergeAdvisoryFake builds the stranded shape: a run with an open PR, a
// non-terminal acceptance stage, and (optionally) an acceptance_reopened entry
// scoped to reopenedStageID. Nothing ever settles the merge, so the tool takes
// the timeout arm.
func mergeAdvisoryFake(accState string, reopenedStageID string) *mergeRunFakeBackend {
	fb := &mergeRunFakeBackend{
		prURL:            "https://github.com/x/y/pull/7",
		stateBeforeMerge: "running",
		stateAfterMerge:  "running", // never terminal
		mergeResp:        MergeRunResult{MergeQueued: true, VerdictSequence: 5},
	}
	if accState != "" {
		fb.stages = []Stage{
			{ID: uuid.NewString(), Type: "implement", State: "succeeded"},
			{ID: advisoryAccStageID, Type: "acceptance", State: accState},
		}
	}
	if reopenedStageID != "" {
		sid := reopenedStageID
		fb.auditEntries = append(fb.auditEntries, AuditEntry{
			Category: categoryAcceptanceReopened, Sequence: 3, StageID: &sid,
		})
	}
	return fb
}

func runMergeAdvisory(t *testing.T, fb *mergeRunFakeBackend) MergeRunOutput {
	t.Helper()
	srv := newMergeRunFakeBackend(t, fb)
	r := newMergeRunResolver(srv)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	_, out, err := r.mergeRun(ctx, nil, MergeRunInput{RunID: uuid.NewString(), Verdict: "ship it"})
	if err != nil {
		t.Fatalf("mergeRun: %v", err)
	}
	if out.Status != "timeout" {
		t.Fatalf("status = %q, want timeout", out.Status)
	}
	return out
}

// controlTimeoutMessage is the message the timeout arm ships with NO advisory —
// the byte-identity control the D1/D2 degrades are compared against. It is
// captured from a real run of the tool against a fake with no acceptance stage
// at all, not hand-written, so a future edit to the base sentence moves the
// control with it.
func controlTimeoutMessage(t *testing.T) string {
	t.Helper()
	out := runMergeAdvisory(t, mergeAdvisoryFake("", ""))
	if strings.Contains(out.Message, "acceptance stage") {
		t.Fatalf("the control message already names an acceptance stage: %q", out.Message)
	}
	return out.Message
}

func TestMergeRun_Timeout_AcceptanceReopenedDispatchable(t *testing.T) {
	for _, st := range []string{"pending", "awaiting_host_dispatch"} {
		t.Run(st, func(t *testing.T) {
			out := runMergeAdvisory(t, mergeAdvisoryFake(st, advisoryAccStageID))
			for _, want := range []string{
				advisoryAccStageID[:8],
				"re-opened by a fix-up push",
				"fishhawk_dispatch_stage, stage acceptance",
				"the queued merge cannot fire",
			} {
				if !strings.Contains(out.Message, want) {
					t.Errorf("message = %q, want it to contain %q", out.Message, want)
				}
			}
			// The base sentence is untouched — the advisory is APPENDED.
			if !strings.Contains(out.Message, "no pr_merged / post_merge_observed entry landed within") {
				t.Errorf("the existing timeout sentence was modified: %q", out.Message)
			}
		})
	}
}

// TestMergeRun_Timeout_AcceptanceReopenedInFlight is #3222 binding condition 2
// on this surface: an in-flight re-run is not one the operator can dispatch, so
// the message must never name a dispatch.
func TestMergeRun_Timeout_AcceptanceReopenedInFlight(t *testing.T) {
	for _, st := range []string{"dispatched", "running"} {
		t.Run(st, func(t *testing.T) {
			out := runMergeAdvisory(t, mergeAdvisoryFake(st, advisoryAccStageID))
			if !strings.Contains(out.Message, "already in flight") {
				t.Errorf("message = %q, want the in-flight wording", out.Message)
			}
			for _, banned := range []string{"fishhawk_dispatch_stage", "Dispatch the acceptance stage", "Re-dispatch"} {
				if strings.Contains(out.Message, banned) {
					t.Errorf("in-flight message names the inapplicable remedy %q: %q", banned, out.Message)
				}
			}
		})
	}
}

// TestMergeRun_Timeout_AcceptanceGenericNonTerminal is the widened behaviour the
// operator ratified: an acceptance stage that was NEVER dispatched blocks the
// merge exactly as hard as a re-opened one, so the advisory still ships — just
// without the re-opened claim.
func TestMergeRun_Timeout_AcceptanceGenericNonTerminal(t *testing.T) {
	out := runMergeAdvisory(t, mergeAdvisoryFake("pending", ""))
	if strings.Contains(out.Message, "re-opened by a fix-up push") {
		t.Errorf("no acceptance_reopened entry exists, yet the message claims one: %q", out.Message)
	}
	if !strings.Contains(out.Message, "must settle first") {
		t.Errorf("message = %q, want the generic wording", out.Message)
	}
	if !strings.Contains(out.Message, "fishhawk_dispatch_stage, stage acceptance") {
		t.Errorf("a dispatchable acceptance stage must name the dispatch: %q", out.Message)
	}
}

// TestMergeRun_Timeout_AcceptanceReopenedScopedToStage is the #3222
// binding-condition-1 assertion on this surface. A NON-MATCHING scoped entry is
// present — an acceptance_reopened entry belonging to a DIFFERENT stage — so
// the generic wording must ship. An absence-only test (degrade D4 below) passes
// just as happily against an implementation that matches on the category alone.
func TestMergeRun_Timeout_AcceptanceReopenedScopedToStage(t *testing.T) {
	other := uuid.NewString()
	out := runMergeAdvisory(t, mergeAdvisoryFake("pending", other))
	if strings.Contains(out.Message, "re-opened by a fix-up push") {
		t.Errorf("an entry scoped to a DIFFERENT stage drew the re-opened claim: %q", out.Message)
	}
	if !strings.Contains(out.Message, "must settle first") {
		t.Errorf("message = %q, want the generic wording", out.Message)
	}
}

// TestMergeRun_Timeout_MultipleAcceptanceStages is the load-bearing selector
// assertion (#3222 fix-up). A run can carry more than one acceptance stage ROW
// in its history — an earlier one superseded or succeeded, a later one
// non-terminal — and a first-match-on-type selector lands on the TERMINAL
// earlier row, takes the D2 guard and stays SILENT on a merge the later stage is
// genuinely blocking. The stage list is sequence-ordered with the stale row
// FIRST, which is the ordering that discriminates.
func TestMergeRun_Timeout_MultipleAcceptanceStages(t *testing.T) {
	const staleAccStageID = "11111111-2222-3333-4444-555555555555"

	// twoAcceptanceRows builds the multi-acceptance shape: a stale acceptance
	// row in staleState, then the live non-terminal one, plus an
	// acceptance_reopened entry scoped to reopenedStageID.
	twoAcceptanceRows := func(staleState, reopenedStageID string) *mergeRunFakeBackend {
		fb := mergeAdvisoryFake("pending", reopenedStageID)
		fb.stages = []Stage{
			{ID: uuid.NewString(), Type: "implement", State: "succeeded"},
			{ID: staleAccStageID, Type: "acceptance", State: staleState},
			{ID: advisoryAccStageID, Type: "acceptance", State: "pending"},
		}
		return fb
	}

	for _, staleState := range []string{"succeeded", "superseded", "failed"} {
		t.Run("stale_"+staleState+"_first", func(t *testing.T) {
			out := runMergeAdvisory(t, twoAcceptanceRows(staleState, advisoryAccStageID))
			for _, want := range []string{
				advisoryAccStageID[:8],
				"re-opened by a fix-up push",
				"fishhawk_dispatch_stage, stage acceptance",
			} {
				if !strings.Contains(out.Message, want) {
					t.Errorf("message = %q, want it to contain %q", out.Message, want)
				}
			}
			if strings.Contains(out.Message, staleAccStageID[:8]) {
				t.Errorf("the advisory named the STALE terminal acceptance stage: %q", out.Message)
			}
		})
	}

	// The two guards compose: with two acceptance rows present AND the only
	// acceptance_reopened entry scoped to the stale one, the live stage still
	// draws an advisory, and it is the GENERIC wording.
	t.Run("reopened_entry_scoped_to_stale_row", func(t *testing.T) {
		out := runMergeAdvisory(t, twoAcceptanceRows("succeeded", staleAccStageID))
		if !strings.Contains(out.Message, advisoryAccStageID[:8]) {
			t.Errorf("message = %q, want it to name the live acceptance stage", out.Message)
		}
		if strings.Contains(out.Message, "re-opened by a fix-up push") {
			t.Errorf("an entry scoped to the STALE stage drew the re-opened claim: %q", out.Message)
		}
		if !strings.Contains(out.Message, "must settle first") {
			t.Errorf("message = %q, want the generic wording", out.Message)
		}
	})

	// Every acceptance row terminal keeps the D2 byte-identity control: more
	// than one row must not manufacture noise on a healthy merge.
	t.Run("all_terminal", func(t *testing.T) {
		control := controlTimeoutMessage(t)
		fb := mergeAdvisoryFake("succeeded", advisoryAccStageID)
		fb.stages = []Stage{
			{ID: staleAccStageID, Type: "acceptance", State: "superseded"},
			{ID: advisoryAccStageID, Type: "acceptance", State: "succeeded"},
		}
		out := runMergeAdvisory(t, fb)
		if out.Message != control {
			t.Fatalf("two terminal acceptance rows must ship the byte-identical message:\n got %q\nwant %q", out.Message, control)
		}
	})
}

func TestMergeRun_ChecksPending_CarriesAcceptanceAdvisory(t *testing.T) {
	fb := mergeAdvisoryFake("pending", advisoryAccStageID)
	// Every POST refuses with the checks-not-all-passed 409, so the shared
	// deadline expires into the resumable checks_pending checkpoint.
	fb.mergeStickyStatus = http.StatusConflict
	fb.mergeErrBody = mergeChecksPendingBody
	srv := newMergeRunFakeBackend(t, fb)
	r := newMergeRunResolver(srv)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()

	_, out, err := r.mergeRun(ctx, nil, MergeRunInput{RunID: uuid.NewString(), Verdict: "ship it"})
	if err != nil {
		t.Fatalf("mergeRun: %v", err)
	}
	if out.Status != "checks_pending" {
		t.Fatalf("status = %q, want checks_pending", out.Status)
	}
	for _, want := range []string{
		advisoryAccStageID[:8],
		"re-opened by a fix-up push",
		"fishhawk_dispatch_stage, stage acceptance",
	} {
		if !strings.Contains(out.Message, want) {
			t.Errorf("message = %q, want it to contain %q", out.Message, want)
		}
	}
	if !strings.Contains(out.Message, "GitHub will not queue the squash merge") {
		t.Errorf("the existing checks_pending sentence was modified: %q", out.Message)
	}
}

// TestMergeRun_AcceptanceAdvisory_D1_StagesReadFails: we know NOTHING, so we say
// nothing — the message is BYTE-IDENTICAL to the no-advisory control.
func TestMergeRun_AcceptanceAdvisory_D1_StagesReadFails(t *testing.T) {
	control := controlTimeoutMessage(t)
	fb := mergeAdvisoryFake("pending", advisoryAccStageID)
	fb.stagesStatus = http.StatusInternalServerError
	out := runMergeAdvisory(t, fb)
	if out.Message != control {
		t.Fatalf("D1 must ship the byte-identical message:\n got %q\nwant %q", out.Message, control)
	}
}

// TestMergeRun_AcceptanceAdvisory_D2_TerminalOrAbsent: no new noise on a healthy
// merge. Both shapes — a TERMINAL acceptance stage, and no acceptance stage at
// all — are byte-identical to the control.
func TestMergeRun_AcceptanceAdvisory_D2_TerminalOrAbsent(t *testing.T) {
	control := controlTimeoutMessage(t)
	t.Run("terminal", func(t *testing.T) {
		// The acceptance_reopened entry is HISTORY and never goes away, so it is
		// present here too: a terminal stage must stay silent regardless.
		out := runMergeAdvisory(t, mergeAdvisoryFake("succeeded", advisoryAccStageID))
		if out.Message != control {
			t.Fatalf("a terminal acceptance stage must ship the byte-identical message:\n got %q\nwant %q", out.Message, control)
		}
	})
	t.Run("absent", func(t *testing.T) {
		out := runMergeAdvisory(t, mergeAdvisoryFake("", ""))
		if out.Message != control {
			t.Fatalf("a run with no acceptance stage must ship the byte-identical message:\n got %q\nwant %q", out.Message, control)
		}
	})
}

// TestMergeRun_AcceptanceAdvisory_D3_AuditReadFails: the stages read succeeded,
// so we KNOW a non-terminal acceptance stage blocks the merge — we just cannot
// claim a fix-up re-opened it. The GENERIC wording still ships.
func TestMergeRun_AcceptanceAdvisory_D3_AuditReadFails(t *testing.T) {
	fb := mergeAdvisoryFake("pending", advisoryAccStageID)
	fb.acceptanceAuditStatus = http.StatusInternalServerError
	out := runMergeAdvisory(t, fb)
	if strings.Contains(out.Message, "re-opened by a fix-up push") {
		t.Errorf("an unreadable audit must not yield the re-opened claim: %q", out.Message)
	}
	if !strings.Contains(out.Message, "must settle first") {
		t.Errorf("message = %q, want the generic wording", out.Message)
	}
	if !strings.Contains(out.Message, advisoryAccStageID[:8]) {
		t.Errorf("message = %q, want it to name the acceptance stage", out.Message)
	}
}

// TestMergeRun_AcceptanceAdvisory_D4_NoMatchingEntry: the audit read SUCCEEDS
// and matches nothing — the same generic wording as D3.
func TestMergeRun_AcceptanceAdvisory_D4_NoMatchingEntry(t *testing.T) {
	out := runMergeAdvisory(t, mergeAdvisoryFake("running", ""))
	if strings.Contains(out.Message, "re-opened by a fix-up push") {
		t.Errorf("zero entries must not yield the re-opened claim: %q", out.Message)
	}
	if !strings.Contains(out.Message, "already in flight") {
		t.Errorf("message = %q, want the generic in-flight wording", out.Message)
	}
}

// --- E45.87 / #3622: the already-merged arm ---------------------------------

// TestMergeRun_AlreadyMerged_SkipsAwait is the CROSS-BOUNDARY seam between the
// endpoint's new already_merged field and the tool's skip-the-await decision:
// per-layer units would each pass while the seam broke.
//
// The proof that the await was SKIPPED (not merely fast) is the backend's
// audit-read counter: the terminal-merge poll's ONLY way to resolve is GET
// /v0/runs/{id}/audit, so ZERO recorded audit requests means the poll never ran.
func TestMergeRun_AlreadyMerged_SkipsAwait(t *testing.T) {
	fb := &mergeRunFakeBackend{
		prURL:            "https://github.com/x/y/pull/7",
		stateBeforeMerge: "running",
		mergeResp: MergeRunResult{
			MergeQueued: false, VerdictSequence: 5, PRURL: "https://github.com/x/y/pull/7",
			AlreadyRecorded: true, AlreadyMerged: true, MergeObservationRecorded: true,
			RunState: "succeeded",
			Message:  "the run's audit chain now carries the observation.",
		},
		// Present but unreachable: a pr_merged entry the await WOULD resolve on.
		// Its presence makes the zero-read assertion meaningful — the await is
		// skipped, not merely starved.
		auditEntries: []AuditEntry{{Category: "pr_merged", Sequence: 6}},
	}
	srv := newMergeRunFakeBackend(t, fb)
	r := newMergeRunResolver(srv)

	_, out, err := r.mergeRun(context.Background(), nil, MergeRunInput{RunID: uuid.NewString(), Verdict: "ship it"})
	if err != nil {
		t.Fatalf("mergeRun: %v", err)
	}
	if out.Status != "merged" {
		t.Fatalf("status = %q, want merged (the reused status, not a sixth value)", out.Status)
	}
	if !out.AlreadyMerged {
		t.Error("already_merged = false, want true")
	}
	if !out.MergeObservationRecorded {
		t.Error("merge_observation_recorded = false, want true")
	}
	if out.MergeQueued {
		t.Error("merge_queued = true, want false — no merge was queued")
	}
	if out.RunState != "succeeded" {
		t.Errorf("run_state = %q, want succeeded (from the endpoint's own response)", out.RunState)
	}
	if out.NextAction == nil || out.NextAction.Action != "post_merge" {
		t.Fatalf("next_action = %+v, want the surfaced post_merge step", out.NextAction)
	}
	if !strings.Contains(out.Message, "already merged") {
		t.Errorf("message = %q, want it to say the PR was already merged", out.Message)
	}
	// The endpoint's own message is carried verbatim: it is the only side that
	// knows whether the observation row persisted.
	if !strings.Contains(out.Message, "the run's audit chain now carries the observation.") {
		t.Errorf("message = %q, want the backend's message carried verbatim", out.Message)
	}
	fb.mu.Lock()
	reads, posts := fb.auditReadCalls, fb.mergeCalls
	fb.mu.Unlock()
	if reads != 0 {
		t.Errorf("audit endpoint recorded %d requests, want 0 — the terminal await must be SKIPPED", reads)
	}
	if posts != 1 {
		t.Errorf("merge POSTs = %d, want exactly 1", posts)
	}
}

// TestMergeRun_AlreadyMerged_RunStateNamesReconcile pins both arms of the
// locally-composed fallback message (a backend that sends none): a NON-TERMINAL
// run_state names reconcile-merge, a terminal one does NOT.
func TestMergeRun_AlreadyMerged_RunStateNamesReconcile(t *testing.T) {
	cases := []struct {
		name          string
		runState      string
		wantReconcile bool
	}{
		{name: "non_terminal_names_reconcile_merge", runState: "running", wantReconcile: true},
		{name: "terminal_does_not", runState: "succeeded", wantReconcile: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fb := &mergeRunFakeBackend{
				prURL:            "https://github.com/x/y/pull/7",
				stateBeforeMerge: "running",
				mergeResp: MergeRunResult{
					MergeQueued: false, VerdictSequence: 5, AlreadyRecorded: true,
					AlreadyMerged: true, MergeObservationRecorded: true, RunState: tc.runState,
				},
			}
			srv := newMergeRunFakeBackend(t, fb)
			r := newMergeRunResolver(srv)

			_, out, err := r.mergeRun(context.Background(), nil, MergeRunInput{RunID: uuid.NewString(), Verdict: "ship it"})
			if err != nil {
				t.Fatalf("mergeRun: %v", err)
			}
			if got := strings.Contains(out.Message, "reconcile-merge"); got != tc.wantReconcile {
				t.Errorf("message names reconcile-merge = %v, want %v; message = %q",
					got, tc.wantReconcile, out.Message)
			}
		})
	}
}

// TestMergeRun_AlreadyMerged_ObservationNotRecorded_NamesRecovery is BINDING
// APPROVAL CONDITION 1 reaching the operator through the tool: when the
// endpoint reports already_merged with merge_observation_recorded:FALSE and no
// message of its own, the locally-composed fallback still names
// record-merge-observation as the recovery.
func TestMergeRun_AlreadyMerged_ObservationNotRecorded_NamesRecovery(t *testing.T) {
	fb := &mergeRunFakeBackend{
		prURL:            "https://github.com/x/y/pull/7",
		stateBeforeMerge: "running",
		mergeResp: MergeRunResult{
			MergeQueued: false, VerdictSequence: 5, AlreadyRecorded: true,
			AlreadyMerged: true, MergeObservationRecorded: false, RunState: "running",
		},
	}
	srv := newMergeRunFakeBackend(t, fb)
	r := newMergeRunResolver(srv)

	_, out, err := r.mergeRun(context.Background(), nil, MergeRunInput{RunID: uuid.NewString(), Verdict: "ship it"})
	if err != nil {
		t.Fatalf("mergeRun: %v", err)
	}
	if out.MergeObservationRecorded {
		t.Error("merge_observation_recorded = true, want false")
	}
	if !strings.Contains(out.Message, "record-merge-observation") {
		t.Errorf("message must name record-merge-observation; got %q", out.Message)
	}
}

// --- ADR-090 / E83.33 / #4018: merge-candidate refusals ----------------------

// mergeCandidateErrBody renders a backend merge-candidate refusal envelope.
func mergeCandidateErrBody(code, message string, details map[string]any) string {
	raw, _ := json.Marshal(map[string]any{"error": map[string]any{"code": code, "message": message, "details": details}})
	return string(raw)
}

// TestMergeRun_MergeCandidateRefusalsReturnImmediately pins each ADR-090 409
// as an IMMEDIATE status: exactly ONE POST, ZERO terminal-await audit reads
// (the await is never armed — a fake that would serve the poll is never
// reached), no verdict flags, the backend message verbatim, and next_action
// naming the clearing verb. Counterfactual: deleting the
// isMergeCandidateRefusal arm surfaces each 409 as a tool error → RED.
func TestMergeRun_MergeCandidateRefusalsReturnImmediately(t *testing.T) {
	stageID := uuid.NewString()
	cases := []struct {
		code       string
		details    map[string]any
		wantStatus string
		wantVerb   string
		wantStage  bool
		consumes   string
	}{
		{"merge_base_behind", map[string]any{"pr_url": "https://github.com/x/y/pull/7", "head_sha": "H", "base_ref": "main", "behind_by": 3, "next_step": "fishhawk_rebase_run_branch"},
			"behind_base", "fishhawk_rebase_run_branch", false, consumesNone},
		{"merge_candidate_unverified", map[string]any{"head_sha": "H", "cause": "base_advance", "verify_state": "in_flight", "stage_id": stageID, "next_step": "fishhawk_await_stage"},
			"merge_candidate_unverified", "fishhawk_await_stage", true, consumesNone},
		{"merge_candidate_verify_failed", map[string]any{"head_sha": "H", "cause": "fan_in", "verify_state": "failed", "next_step": "fishhawk_fixup_stage"},
			"merge_candidate_verify_failed", "fishhawk_fixup_stage", false, consumesFixupBudget},
		// An older detail map with no next_step falls back to the documented verb.
		{"merge_candidate_unverified", map[string]any{"head_sha": "H", "verify_state": "unverified"},
			"merge_candidate_unverified", "fishhawk_rebase_run_branch", false, consumesNone},
	}
	for _, tc := range cases {
		t.Run(tc.code+"/"+tc.wantVerb, func(t *testing.T) {
			msg := "backend says: " + tc.code
			fb := &mergeRunFakeBackend{
				prURL:             "https://github.com/x/y/pull/7",
				stateBeforeMerge:  "running",
				mergeStickyStatus: http.StatusConflict,
				mergeErrBody:      mergeCandidateErrBody(tc.code, msg, tc.details),
				// A pr_merged entry the await WOULD resolve on, so an armed
				// await would turn this into status=merged.
				auditEntries: []AuditEntry{{Sequence: 99, Category: "pr_merged"}},
			}
			srv := newMergeRunFakeBackend(t, fb)
			r := newMergeRunResolver(srv)
			runID := uuid.New()

			start := time.Now()
			_, out, err := r.mergeRun(context.Background(), nil, MergeRunInput{RunID: runID.String(), Verdict: "ship it", TimeoutSeconds: 600})
			if err != nil {
				t.Fatalf("mergeRun: %v — a %s 409 must be a status, not a tool error", err, tc.code)
			}
			if out.Status != tc.wantStatus {
				t.Fatalf("status = %q, want %q", out.Status, tc.wantStatus)
			}
			if out.MergeQueued || out.VerdictRecorded || out.AlreadyRecorded || out.AlreadyMerged {
				t.Errorf("flags = %+v, want all false (nothing queued, no verdict)", out)
			}
			if out.Message != msg {
				t.Errorf("message = %q, want the backend message verbatim", out.Message)
			}
			if out.NextAction == nil || out.NextAction.Action != tc.wantVerb {
				t.Fatalf("next_action = %+v, want %s", out.NextAction, tc.wantVerb)
			}
			if out.NextAction.Params["run_id"] != runID.String() {
				t.Errorf("next_action run_id = %q, want %s", out.NextAction.Params["run_id"], runID)
			}
			if got := out.NextAction.Params["stage_id"]; tc.wantStage != (got == stageID) {
				t.Errorf("next_action stage_id = %q, want present=%v", got, tc.wantStage)
			}
			if out.NextAction.Consumes != tc.consumes {
				t.Errorf("next_action consumes = %q, want %q", out.NextAction.Consumes, tc.consumes)
			}
			fb.mu.Lock()
			calls, reads := fb.mergeCalls, fb.auditReadCalls
			fb.mu.Unlock()
			if calls != 1 {
				t.Errorf("merge POSTed %d times, want EXACTLY 1 (no re-POST)", calls)
			}
			if reads != 0 {
				t.Errorf("terminal-await audit reads = %d, want 0 (no await is armed)", reads)
			}
			if elapsed := time.Since(start); elapsed > 5*time.Second {
				t.Errorf("mergeRun took %s, want an immediate return", elapsed)
			}
		})
	}
}

// TestMergeRun_MergeCandidateHeldReturnsImmediately pins the ADR-092 409
// merge_candidate_held (#4200) as an IMMEDIATE status=merge_candidate_held:
// one POST, zero terminal-await reads (the await is never armed even though
// the fake would resolve it to merged), the backend message verbatim,
// queue_position / anchored_head_sha / live_run_id lifted from the details,
// and next_action fishhawk_get_run_status (admission is automatic). A detail
// map with no next_step still falls back to fishhawk_get_run_status.
// Counterfactual: deleting the merge_candidate_held row from
// mergeCandidateStatuses surfaces the 409 as a tool error → RED; deleting the
// held arm in mergeCandidateOutput leaves queue_position 0 → RED.
func TestMergeRun_MergeCandidateHeldReturnsImmediately(t *testing.T) {
	live := uuid.NewString()
	for _, tc := range []struct {
		name    string
		details map[string]any
	}{
		{"with next_step", map[string]any{"queue_state": "held", "queue_position": 2, "live_run_id": live,
			"anchored_head_sha": "A2", "head_sha": "H", "base_ref": "main", "next_step": "fishhawk_get_run_status"}},
		{"older details without next_step", map[string]any{"queue_state": "held", "queue_position": 2, "live_run_id": live,
			"anchored_head_sha": "A2"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msg := "backend says: held at position 2"
			fb := &mergeRunFakeBackend{
				prURL:             "https://github.com/x/y/pull/7",
				stateBeforeMerge:  "running",
				mergeStickyStatus: http.StatusConflict,
				mergeErrBody:      mergeCandidateErrBody("merge_candidate_held", msg, tc.details),
				auditEntries:      []AuditEntry{{Sequence: 99, Category: "pr_merged"}},
			}
			r := newMergeRunResolver(newMergeRunFakeBackend(t, fb))
			runID := uuid.New()
			_, out, err := r.mergeRun(context.Background(), nil, MergeRunInput{RunID: runID.String(), Verdict: "ship it", TimeoutSeconds: 600})
			if err != nil {
				t.Fatalf("mergeRun: %v — merge_candidate_held must be a status, not a tool error", err)
			}
			if out.Status != "merge_candidate_held" || out.Message != msg {
				t.Fatalf("status/message = %q/%q, want merge_candidate_held with the backend message", out.Status, out.Message)
			}
			if out.QueuePosition != 2 || out.AnchoredHeadSHA != "A2" || out.LiveRunID != live {
				t.Errorf("queue fields = pos %d head %q live %q, want 2/A2/%s", out.QueuePosition, out.AnchoredHeadSHA, out.LiveRunID, live)
			}
			if out.MergeQueued || out.VerdictRecorded || out.AlreadyRecorded || out.AlreadyMerged {
				t.Errorf("flags = %+v, want all false", out)
			}
			if out.NextAction == nil || out.NextAction.Action != "fishhawk_get_run_status" ||
				out.NextAction.Params["run_id"] != runID.String() || out.NextAction.Consumes != consumesNone ||
				!strings.Contains(out.NextAction.Reason, "ADR-092") {
				t.Fatalf("next_action = %+v, want fishhawk_get_run_status for %s (ADR-092 reason, consumes none)", out.NextAction, runID)
			}
			fb.mu.Lock()
			calls, reads := fb.mergeCalls, fb.auditReadCalls
			fb.mu.Unlock()
			if calls != 1 || reads != 0 {
				t.Errorf("POSTs = %d, await reads = %d; want 1 and 0 (immediate, no await armed)", calls, reads)
			}
		})
	}
}

// TestMergeRun_MergeCandidateCheckFailedIsToolError pins the 502 as a tool
// error (a retryable read failure, not a precondition), and that an
// unrelated 409 code is NOT swallowed as a merge-candidate status.
func TestMergeRun_MergeCandidateCheckFailedIsToolError(t *testing.T) {
	for _, tc := range []struct {
		status int
		code   string
	}{
		{http.StatusBadGateway, "merge_candidate_check_failed"},
		{http.StatusConflict, "acceptance_gate_not_passed"},
	} {
		t.Run(tc.code, func(t *testing.T) {
			fb := &mergeRunFakeBackend{
				prURL:             "https://github.com/x/y/pull/7",
				stateBeforeMerge:  "running",
				mergeStickyStatus: tc.status,
				mergeErrBody:      mergeCandidateErrBody(tc.code, "refused", map[string]any{"reason": "forge_read", "retryable": true}),
			}
			srv := newMergeRunFakeBackend(t, fb)
			r := newMergeRunResolver(srv)
			_, out, err := r.mergeRun(context.Background(), nil, MergeRunInput{RunID: uuid.NewString(), Verdict: "ship it", TimeoutSeconds: 1})
			if err == nil {
				t.Fatalf("err = nil, out = %+v; want a tool error for %s", out, tc.code)
			}
			if !strings.Contains(err.Error(), tc.code) {
				t.Errorf("error %q does not name %s", err, tc.code)
			}
		})
	}
}

// TestIsMergeCandidateRefusal pins the code→status table and that only a 409
// maps (a merge-candidate code on another status is not swallowed).
func TestIsMergeCandidateRefusal(t *testing.T) {
	for code, want := range map[string]string{
		"merge_base_behind":             "behind_base",
		"merge_candidate_unverified":    "merge_candidate_unverified",
		"merge_candidate_verify_failed": "merge_candidate_verify_failed",
		"merge_candidate_held":          "merge_candidate_held",
	} {
		if _, got, ok := isMergeCandidateRefusal(&apiError{StatusCode: http.StatusConflict, Code: code}); !ok || got != want {
			t.Errorf("%s → (%q, %v), want (%q, true)", code, got, ok, want)
		}
		if _, _, ok := isMergeCandidateRefusal(&apiError{StatusCode: http.StatusBadGateway, Code: code}); ok {
			t.Errorf("%s on a 502 mapped to a status, want a tool error", code)
		}
	}
	for _, err := range []error{
		&apiError{StatusCode: http.StatusBadGateway, Code: "merge_candidate_check_failed"},
		&apiError{StatusCode: http.StatusConflict, Code: "merge_conflicting"},
		errors.New("plain"),
	} {
		if _, _, ok := isMergeCandidateRefusal(err); ok {
			t.Errorf("%v mapped to a merge-candidate status, want not", err)
		}
	}
}

// --- #4086: merge-readiness refusals -----------------------------------------

// runReadinessRefusal drives one mergeRun against a backend that answers every
// POST with the given 409 body, and asserts the shared IMMEDIATE-status
// contract: no tool error, exactly ONE POST, ZERO terminal-await reads, every
// flag false and the backend message verbatim.
func runReadinessRefusal(t *testing.T, code, msg string, details map[string]any) (MergeRunOutput, uuid.UUID) {
	t.Helper()
	fb := &mergeRunFakeBackend{
		prURL:             "https://github.com/x/y/pull/7",
		stateBeforeMerge:  "running",
		mergeStickyStatus: http.StatusConflict,
		mergeErrBody:      mergeCandidateErrBody(code, msg, details),
		// A pr_merged entry an armed await WOULD resolve on.
		auditEntries: []AuditEntry{{Sequence: 99, Category: "pr_merged"}},
	}
	srv := newMergeRunFakeBackend(t, fb)
	r := newMergeRunResolver(srv)
	runID := uuid.New()
	_, out, err := r.mergeRun(context.Background(), nil, MergeRunInput{RunID: runID.String(), Verdict: "ship it", TimeoutSeconds: 600})
	if err != nil {
		t.Fatalf("mergeRun: %v — a %s 409 must be a status, not a tool error", err, code)
	}
	if out.MergeQueued || out.VerdictRecorded || out.AlreadyRecorded || out.AlreadyMerged {
		t.Errorf("flags = %+v, want all false (nothing queued, no verdict)", out)
	}
	if out.Message != msg {
		t.Errorf("message = %q, want the backend message verbatim", out.Message)
	}
	fb.mu.Lock()
	calls, reads := fb.mergeCalls, fb.auditReadCalls
	fb.mu.Unlock()
	if calls != 1 || reads != 0 {
		t.Errorf("POSTs = %d, await reads = %d; want exactly 1 and 0 (immediate, not a timeout)", calls, reads)
	}
	return out, runID
}

// TestMergeRun_ApprovalDismissed_ImmediateStatus pins the MCP twin of D1: the
// backend's 409 approval_dismissed is an immediate status whose next_action is
// the approve_pr ritual. Counterfactual: removing the readinessStatuses entry
// turns the 409 into a tool error → RED.
func TestMergeRun_ApprovalDismissed_ImmediateStatus(t *testing.T) {
	const head = "1111111111111111111111111111111111111111"
	msg := "no approval is live on the pull request and a prior review was dismissed; the current head " + head + " is not covered"
	out, runID := runReadinessRefusal(t, "approval_dismissed", msg, map[string]any{
		"pr_url": "https://github.com/x/y/pull/7", "dismissing_commit": head, "approved_commit": "2222", "dismissing_cause": "vouch_commit", "next_step": "approve_pr",
	})
	if out.Status != "approval_dismissed" {
		t.Fatalf("status = %q, want approval_dismissed", out.Status)
	}
	if !strings.Contains(out.Message, head) {
		t.Errorf("message %q does not name the dismissing head", out.Message)
	}
	if out.NextAction == nil || out.NextAction.Action != "approve_pr" {
		t.Fatalf("next_action = %+v, want approve_pr", out.NextAction)
	}
	if out.NextAction.Params["run_id"] != runID.String() || out.NextAction.Params["pr_url"] != "https://github.com/x/y/pull/7" {
		t.Errorf("next_action params = %v, want run_id + pr_url", out.NextAction.Params)
	}
	if out.PRURL != "https://github.com/x/y/pull/7" {
		t.Errorf("pr_url = %q", out.PRURL)
	}
}

// TestMergeRun_AcceptanceStale_ImmediateStatus pins the MCP twin of D2: the
// 409 acceptance_stale is an immediate status whose next_action is
// details.next_step with the acceptance stage id, falling back to
// fishhawk_dispatch_stage when an older detail map omits next_step.
func TestMergeRun_AcceptanceStale_ImmediateStatus(t *testing.T) {
	stageID := uuid.NewString()
	for _, tc := range []struct {
		name     string
		details  map[string]any
		wantVerb string
		consumes string
	}{
		{"dispatch", map[string]any{"acceptance_stage_id": stageID, "verified_head_sha": "h1", "current_head_sha": "h2", "next_step": "fishhawk_dispatch_stage"}, "fishhawk_dispatch_stage", consumesNone},
		{"await", map[string]any{"acceptance_stage_id": stageID, "next_step": "fishhawk_await_stage"}, "fishhawk_await_stage", consumesNone},
		{"retry", map[string]any{"acceptance_stage_id": stageID, "next_step": "fishhawk_retry_stage"}, "fishhawk_retry_stage", consumesRetryBudget},
		{"fallback without next_step", map[string]any{"stage_id": stageID}, "fishhawk_dispatch_stage", consumesNone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msg := "the recorded acceptance verdict is stale: it validated head h1, re-opened at head h2"
			out, runID := runReadinessRefusal(t, "acceptance_stale", msg, tc.details)
			if out.Status != "acceptance_stale" {
				t.Fatalf("status = %q, want acceptance_stale", out.Status)
			}
			if out.NextAction == nil || out.NextAction.Action != tc.wantVerb {
				t.Fatalf("next_action = %+v, want %s", out.NextAction, tc.wantVerb)
			}
			if out.NextAction.Params["run_id"] != runID.String() || out.NextAction.Params["stage_id"] != stageID {
				t.Errorf("next_action params = %v, want run_id + stage_id %s", out.NextAction.Params, stageID)
			}
			if out.NextAction.Consumes != tc.consumes {
				t.Errorf("consumes = %q, want %q", out.NextAction.Consumes, tc.consumes)
			}
		})
	}
}

// TestMergeRun_ReadinessRefusalDuringChecksPendingWait pins that the readiness
// arm is consulted on EVERY POST: a re-POST during the checks_pending wait
// that meets acceptance_stale returns at once instead of waiting out the
// budget as checks_pending.
func TestMergeRun_ReadinessRefusalDuringChecksPendingWait(t *testing.T) {
	fb := &mergeRunFakeBackend{
		prURL:             "https://github.com/x/y/pull/7",
		stateBeforeMerge:  "running",
		mergeStickyStatus: http.StatusConflict,
		mergeErrBodies: []string{
			mergeCandidateErrBody("merge_checks_pending", "checks pending", map[string]any{"verdict_sequence": 4}),
			mergeCandidateErrBody("acceptance_stale", "stale", map[string]any{"next_step": "fishhawk_dispatch_stage"}),
		},
	}
	srv := newMergeRunFakeBackend(t, fb)
	r := newMergeRunResolver(srv)
	start := time.Now()
	_, out, err := r.mergeRun(context.Background(), nil, MergeRunInput{RunID: uuid.NewString(), Verdict: "ship it", TimeoutSeconds: 600})
	if err != nil {
		t.Fatalf("mergeRun: %v", err)
	}
	if out.Status != "acceptance_stale" {
		t.Fatalf("status = %q, want acceptance_stale on the re-POST", out.Status)
	}
	fb.mu.Lock()
	calls := fb.mergeCalls
	fb.mu.Unlock()
	if calls != 2 {
		t.Errorf("POSTs = %d, want 2 (one checks_pending, one refusal)", calls)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("mergeRun took %s, want an immediate return", elapsed)
	}
}

// TestIsMergeReadinessRefusal pins the code→status table, that only a 409
// maps, and that the merge-candidate and conflict codes are not swallowed.
func TestIsMergeReadinessRefusal(t *testing.T) {
	for _, code := range []string{"acceptance_stale", "approval_dismissed"} {
		if _, got, ok := isMergeReadinessRefusal(&apiError{StatusCode: http.StatusConflict, Code: code}); !ok || got != code {
			t.Errorf("%s → (%q, %v), want (%q, true)", code, got, ok, code)
		}
		if _, _, ok := isMergeReadinessRefusal(&apiError{StatusCode: http.StatusInternalServerError, Code: code}); ok {
			t.Errorf("%s on a 500 mapped to a status, want a tool error", code)
		}
	}
	for _, err := range []error{
		&apiError{StatusCode: http.StatusConflict, Code: "merge_base_behind"},
		&apiError{StatusCode: http.StatusConflict, Code: "acceptance_gate_not_passed"},
		errors.New("plain"),
	} {
		if _, _, ok := isMergeReadinessRefusal(err); ok {
			t.Errorf("%v mapped to a readiness status, want not", err)
		}
	}
}

// TestMergeReadinessOutput_EmptyMessageFallback pins that a refusal with no
// message still carries an actionable one naming the clearing verb.
func TestMergeReadinessOutput_EmptyMessageFallback(t *testing.T) {
	out := mergeReadinessOutput(&apiError{StatusCode: http.StatusConflict, Code: "approval_dismissed", Details: map[string]any{}},
		"approval_dismissed", uuid.New(), time.Now())
	if !strings.Contains(out.Message, "approve_pr") || out.NextAction.Params["pr_url"] != "" {
		t.Errorf("out = %+v, want a fallback message naming approve_pr and no pr_url param", out)
	}
}

// TestMergeRun_ReviewCoverageIncomplete_ImmediateStatus pins the #4219 arm:
// the backend's 409 review_coverage_incomplete is an IMMEDIATE status (no
// poll, no verdict row) whose next_action is details.next_step —
// fishhawk_rerun_review, or fishhawk_await_review (stage implement) — and a
// missing or unknown next_step falls back to the read-only await. It NEVER
// offers the merge.
func TestMergeRun_ReviewCoverageIncomplete_ImmediateStatus(t *testing.T) {
	for _, tc := range []struct {
		name      string
		details   map[string]any
		wantVerb  string
		wantStage string
	}{
		{"rerun", map[string]any{"next_step": "fishhawk_rerun_review"}, "fishhawk_rerun_review", ""},
		{"await", map[string]any{"next_step": "fishhawk_await_review"}, "fishhawk_await_review", "implement"},
		{"fallback without next_step", map[string]any{}, "fishhawk_await_review", "implement"},
		{"unknown next_step", map[string]any{"next_step": "fishhawk_merge_run"}, "fishhawk_await_review", "implement"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msg := "review coverage is incomplete: slice 1 (child c1) of this decomposed parent was never covered by a successful agent review"
			out, runID := runReadinessRefusal(t, "review_coverage_incomplete", msg, tc.details)
			if out.Status != "review_coverage_incomplete" {
				t.Fatalf("status = %q, want review_coverage_incomplete", out.Status)
			}
			if out.Message != msg {
				t.Errorf("message = %q, want the server's verbatim", out.Message)
			}
			if out.NextAction == nil || out.NextAction.Action != tc.wantVerb {
				t.Fatalf("next_action = %+v, want %s", out.NextAction, tc.wantVerb)
			}
			if out.NextAction.Params["run_id"] != runID.String() || out.NextAction.Params["stage"] != tc.wantStage {
				t.Errorf("next_action params = %v, want run_id + stage %q", out.NextAction.Params, tc.wantStage)
			}
			if out.MergeQueued || out.NextAction.Consumes != consumesNone {
				t.Errorf("merge_queued=%v consumes=%q, want false / none", out.MergeQueued, out.NextAction.Consumes)
			}
		})
	}
}

// --- scope_overlap on fishhawk_merge_run (E83.60 / #4118) ---

// mergedFake is a backend whose merge settles immediately (pr_merged past the
// verdict anchor).
func mergedFake() *mergeRunFakeBackend {
	return &mergeRunFakeBackend{
		prURL:            "https://github.com/x/y/pull/7",
		stateBeforeMerge: "running",
		stateAfterMerge:  "succeeded",
		mergeResp:        MergeRunResult{MergeQueued: true, VerdictSequence: 5, PRURL: "https://github.com/x/y/pull/7"},
		auditEntries:     []AuditEntry{{Category: "pr_merged", Sequence: 6}},
	}
}

// TestMergeRun_ScopeOverlap_NamesOverlappingRuns: a merged response carries the
// backend's live overlap for the run being merged, informational only.
func TestMergeRun_ScopeOverlap_NamesOverlappingRuns(t *testing.T) {
	runID := uuid.NewString()
	other := uuid.NewString()
	fb := mergedFake()
	fb.scopeOverlapBody = `{"run_id":"` + runID + `","self_scope_resolved":true,"overlaps":[{"run_id":"` + other +
		`","issue_ref":"x/y#11","stage":"implement","stage_state":"running","paths":["shared.go"]}],` +
		`"scanned_runs":1,"unscoped_runs":0,"truncated":false}`
	r := newMergeRunResolver(newMergeRunFakeBackend(t, fb))

	_, out, err := r.mergeRun(context.Background(), nil, MergeRunInput{RunID: runID, Verdict: "ship it"})
	if err != nil {
		t.Fatalf("mergeRun: %v", err)
	}
	if out.Status != "merged" {
		t.Fatalf("status = %q, want merged", out.Status)
	}
	ov := out.ScopeOverlap
	if ov == nil || len(ov.Overlaps) != 1 {
		t.Fatalf("scope_overlap = %+v, want the seeded overlap", ov)
	}
	if o := ov.Overlaps[0]; o.RunID != other || o.IssueRef != "x/y#11" || o.Stage != "implement" ||
		len(o.Paths) != 1 || o.Paths[0] != "shared.go" {
		t.Errorf("overlap = %+v, want run %s implement sharing shared.go", o, other)
	}
	if fb.scopeOverlapCalls != 1 {
		t.Errorf("scope-overlap reads = %d, want 1", fb.scopeOverlapCalls)
	}
}

// TestMergeRun_ScopeOverlap_ReadFailureOmitted: a 500, a 403 (a token without
// read:audit) and an unrouted 404 each leave scope_overlap nil while the merge
// still answers status=merged — the read never refuses the merge.
func TestMergeRun_ScopeOverlap_ReadFailureOmitted(t *testing.T) {
	for _, tc := range []struct {
		name   string
		body   string
		status int
	}{
		{"500", `{"code":"internal_error","message":"boom"}`, http.StatusInternalServerError},
		{"403", `{"code":"insufficient_scope","message":"read:audit"}`, http.StatusForbidden},
		{"unrouted 404", "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fb := mergedFake()
			fb.scopeOverlapBody, fb.scopeOverlapStatus = tc.body, tc.status
			r := newMergeRunResolver(newMergeRunFakeBackend(t, fb))
			_, out, err := r.mergeRun(context.Background(), nil, MergeRunInput{RunID: uuid.NewString(), Verdict: "ship it"})
			if err != nil {
				t.Fatalf("mergeRun: %v (a failed overlap read must not fail the merge)", err)
			}
			if out.Status != "merged" || out.ScopeOverlap != nil {
				t.Errorf("status = %q scope_overlap = %+v, want merged with no block", out.Status, out.ScopeOverlap)
			}
			if fb.scopeOverlapCalls != 1 {
				t.Errorf("scope-overlap reads = %d, want 1 (attempted)", fb.scopeOverlapCalls)
			}
		})
	}
}

// TestMergeRun_ScopeOverlap_NotReadOnToolError: a tool error from the merge
// flow (no PR URL) makes no scope-overlap read and returns the error unchanged.
func TestMergeRun_ScopeOverlap_NotReadOnToolError(t *testing.T) {
	fb := mergedFake()
	fb.prURL = ""
	fb.scopeOverlapBody = `{"run_id":"x","self_scope_resolved":true,"overlaps":[],"scanned_runs":0,"unscoped_runs":0,"truncated":false}`
	r := newMergeRunResolver(newMergeRunFakeBackend(t, fb))
	_, _, err := r.mergeRun(context.Background(), nil, MergeRunInput{RunID: uuid.NewString(), Verdict: "ship it"})
	if err == nil || !strings.Contains(err.Error(), "no pull request URL") {
		t.Fatalf("err = %v, want the no-PR refusal", err)
	}
	if fb.scopeOverlapCalls != 0 {
		t.Errorf("scope-overlap reads = %d, want 0 on a tool error", fb.scopeOverlapCalls)
	}
}

// queuedWireBody is the backend's ADR-093 202 queued answer as the server
// writes it (backend/internal/server/merge_run.go::mergeRunResponse): every
// zero-valued field omitted, merge_queued absent (false), the queued fields
// in the server's json-tag spelling.
const queuedWireBody = `{"run_id":"%s","pr_url":"https://github.com/x/y/pull/7","queued":true,"queue_state":"held","queue_position":2,"live_run_id":"%s","entry_id":"%s","base_ref":"main","anchored_head_sha":"H2","intent_sequence":41,"message":"backend says: queued at position 2"}`

// TestMergeRun_QueuedReturnsImmediately pins the ADR-093 slice 1 202 queued
// answer (E87.4 / #4295) as an IMMEDIATE status=queued: exactly ONE POST and
// ZERO terminal-await audit reads, even though the fake carries a pr_merged
// entry an armed await would resolve to status=merged; every merge flag
// false (nothing merged, no merge_verdict_recorded row); the entry lifted from
// the raw wire body; next_action fishhawk_get_merge_queue on the run's repo
// and base. Counterfactual: delete the res.Queued arm in mergeRunCore → the
// queued answer falls into awaitMergeTerminal → status=merged with audit
// reads → RED.
func TestMergeRun_QueuedReturnsImmediately(t *testing.T) {
	runID, live, entry := uuid.New(), uuid.NewString(), uuid.NewString()
	fb := &mergeRunFakeBackend{
		prURL:            "https://github.com/x/y/pull/7",
		repo:             "x/y",
		stateBeforeMerge: "succeeded",
		mergeStatuses:    []int{http.StatusAccepted},
		mergeRawBody:     fmt.Sprintf(queuedWireBody, runID, live, entry),
		auditEntries:     []AuditEntry{{Sequence: 99, Category: "pr_merged"}},
	}
	r := newMergeRunResolver(newMergeRunFakeBackend(t, fb))
	start := time.Now()
	_, out, err := r.mergeRun(context.Background(), nil, MergeRunInput{RunID: runID.String(), Verdict: "ship it", TimeoutSeconds: 600})
	if err != nil {
		t.Fatalf("mergeRun: %v — a 202 queued answer must be a status, not a tool error", err)
	}
	if out.Status != "queued" {
		t.Fatalf("status = %q, want queued", out.Status)
	}
	if out.MergeQueued || out.VerdictRecorded || out.AlreadyRecorded || out.AlreadyMerged || out.VerdictSequence != 0 {
		t.Errorf("merge flags = %+v, want all false (nothing merged, no verdict row)", out)
	}
	if out.QueueState != "held" || out.QueuePosition != 2 || out.LiveRunID != live || out.EntryID != entry ||
		out.BaseRef != "main" || out.AnchoredHeadSHA != "H2" || out.IntentSequence != 41 || out.AlreadyQueued {
		t.Errorf("queue fields = state %q pos %d live %q entry %q base %q head %q seq %d already %v, want held/2/%s/%s/main/H2/41/false",
			out.QueueState, out.QueuePosition, out.LiveRunID, out.EntryID, out.BaseRef, out.AnchoredHeadSHA, out.IntentSequence, out.AlreadyQueued, live, entry)
	}
	if out.Message != "backend says: queued at position 2" || out.PRURL != "https://github.com/x/y/pull/7" {
		t.Errorf("message/pr_url = %q/%q, want the backend's verbatim", out.Message, out.PRURL)
	}
	na := out.NextAction
	if na == nil || na.Action != "fishhawk_get_merge_queue" || na.Params["repo"] != "x/y" || na.Params["base_ref"] != "main" ||
		na.Params["run_id"] != runID.String() || na.Consumes != consumesNone {
		t.Fatalf("next_action = %+v, want fishhawk_get_merge_queue {repo x/y, base_ref main, run_id}", na)
	}
	fb.mu.Lock()
	calls, reads := fb.mergeCalls, fb.auditReadCalls
	fb.mu.Unlock()
	if calls != 1 || reads != 0 {
		t.Errorf("POSTs = %d, terminal-await audit reads = %d; want 1 and 0 (the await is never armed)", calls, reads)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("mergeRun took %s, want an immediate return", elapsed)
	}
}

// TestMergeRun_QueuedLiveAndAlreadyQueued: the run that holds the line (live,
// no position, no live_run_id of another run) and a repeat merge_run on an
// entry that already carried intent (already_queued) both stay status=queued
// with the flags carried through; an answer with no message, repo or base
// falls back to a constant message and a next_action keyed on run_id alone.
func TestMergeRun_QueuedLiveAndAlreadyQueued(t *testing.T) {
	runID := uuid.New()
	for _, tc := range []struct {
		name, body, repo string
		wantMsg          bool
	}{
		{"live already queued", `{"queued":true,"queue_state":"live","entry_id":"e1","base_ref":"main","anchored_head_sha":"H","intent_sequence":7,"already_queued":true,"message":"holds the line"}`, "x/y", true},
		{"bare answer", `{"queued":true}`, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fb := &mergeRunFakeBackend{
				prURL: "https://github.com/x/y/pull/7", repo: tc.repo, stateBeforeMerge: "succeeded",
				mergeStatuses: []int{http.StatusAccepted}, mergeRawBody: tc.body,
				auditEntries: []AuditEntry{{Sequence: 99, Category: "pr_merged"}},
			}
			r := newMergeRunResolver(newMergeRunFakeBackend(t, fb))
			_, out, err := r.mergeRun(context.Background(), nil, MergeRunInput{RunID: runID.String(), Verdict: "ship it"})
			if err != nil || out.Status != "queued" {
				t.Fatalf("mergeRun = %q, %v; want status=queued", out.Status, err)
			}
			if tc.wantMsg {
				if out.QueueState != "live" || out.QueuePosition != 0 || !out.AlreadyQueued || out.IntentSequence != 7 || out.Message != "holds the line" {
					t.Errorf("out = %+v, want live, no position, already_queued, intent_sequence 7, the backend message", out)
				}
				if out.NextAction.Params["repo"] != "x/y" || out.NextAction.Params["base_ref"] != "main" {
					t.Errorf("next_action params = %v, want repo + base_ref", out.NextAction.Params)
				}
			} else {
				if !strings.Contains(out.Message, "fishhawk_get_merge_queue") || !strings.Contains(out.Message, "no merge verdict was recorded") {
					t.Errorf("fallback message = %q, want the constant queued explanation", out.Message)
				}
				if _, ok := out.NextAction.Params["repo"]; ok {
					t.Errorf("next_action params = %v, want no repo when the run carries none", out.NextAction.Params)
				}
				if _, ok := out.NextAction.Params["base_ref"]; ok {
					t.Errorf("next_action params = %v, want no base_ref when the answer carries none", out.NextAction.Params)
				}
				if out.NextAction.Params["run_id"] != runID.String() {
					t.Errorf("next_action run_id = %q, want %s", out.NextAction.Params["run_id"], runID)
				}
			}
			fb.mu.Lock()
			reads := fb.auditReadCalls
			fb.mu.Unlock()
			if reads != 0 {
				t.Errorf("terminal-await audit reads = %d, want 0", reads)
			}
		})
	}
}

// TestMergeRun_IneligibleSurfacesReasons: an INELIGIBLE run keeps today's
// behind_base / merge_candidate_unverified / merge_candidate_held status, and
// the 409's details.merge_queue_ineligible reason codes ride
// merge_queue_ineligible (ADR-093 slice 1). Counterfactual: make
// mergeQueueIneligibleFrom return nil → RED.
func TestMergeRun_IneligibleSurfacesReasons(t *testing.T) {
	for _, tc := range []struct{ code, status string }{
		{"merge_base_behind", "behind_base"},
		{"merge_candidate_unverified", "merge_candidate_unverified"},
		{"merge_candidate_held", "merge_candidate_held"},
	} {
		t.Run(tc.code, func(t *testing.T) {
			details := map[string]any{"pr_url": "https://github.com/x/y/pull/7", "merge_queue_ineligible": []string{"not_approved", "checks_not_green"}}
			fb := &mergeRunFakeBackend{
				prURL: "https://github.com/x/y/pull/7", stateBeforeMerge: "succeeded",
				mergeStickyStatus: http.StatusConflict, mergeErrBody: mergeCandidateErrBody(tc.code, "refused", details),
			}
			r := newMergeRunResolver(newMergeRunFakeBackend(t, fb))
			_, out, err := r.mergeRun(context.Background(), nil, MergeRunInput{RunID: uuid.NewString(), Verdict: "ship it"})
			if err != nil || out.Status != tc.status {
				t.Fatalf("mergeRun = %q, %v; want %s", out.Status, err, tc.status)
			}
			if strings.Join(out.MergeQueueIneligible, ",") != "not_approved,checks_not_green" {
				t.Errorf("merge_queue_ineligible = %v, want [not_approved checks_not_green]", out.MergeQueueIneligible)
			}
		})
	}
}

// TestMergeQueueIneligibleFrom pins the defensive decode of a detail that
// crossed an HTTP JSON boundary: absent, wrong-typed or empty is nil, and a
// non-string or blank member is skipped rather than failing the status.
func TestMergeQueueIneligibleFrom(t *testing.T) {
	for _, tc := range []struct {
		name    string
		details map[string]any
		want    string
	}{
		{"nil details", nil, ""},
		{"absent", map[string]any{"pr_url": "p"}, ""},
		{"wrong type", map[string]any{"merge_queue_ineligible": "not_approved"}, ""},
		{"empty", map[string]any{"merge_queue_ineligible": []any{}}, ""},
		{"mixed members", map[string]any{"merge_queue_ineligible": []any{"undeterminable", 3.0, " ", nil, "checks_red"}}, "undeterminable,checks_red"},
	} {
		got := mergeQueueIneligibleFrom(tc.details)
		if strings.Join(got, ",") != tc.want || (tc.want == "" && got != nil) {
			t.Errorf("%s: got %#v, want %q", tc.name, got, tc.want)
		}
	}
}

// --- merge overlap guard (E83.61 / #4119) ---

// runOverlapRefusal drives one fishhawk_merge_run against a backend whose
// every POST answers the given 409, with a pr_merged entry an armed await
// WOULD resolve on, and returns the output plus the POST / await-read counts.
func runOverlapRefusal(t *testing.T, code, msg string, details map[string]any) (MergeRunOutput, uuid.UUID, int, int) {
	t.Helper()
	fb := &mergeRunFakeBackend{
		prURL:             "https://github.com/x/y/pull/7",
		stateBeforeMerge:  "running",
		mergeStickyStatus: http.StatusConflict,
		mergeErrBody:      mergeCandidateErrBody(code, msg, details),
		auditEntries:      []AuditEntry{{Sequence: 99, Category: "pr_merged"}},
	}
	srv := newMergeRunFakeBackend(t, fb)
	r := newMergeRunResolver(srv)
	runID := uuid.New()
	_, out, err := r.mergeRun(context.Background(), nil, MergeRunInput{RunID: runID.String(), Verdict: "ship it", TimeoutSeconds: 600})
	if err != nil {
		t.Fatalf("mergeRun: %v — a %s 409 must be a status, not a tool error", err, code)
	}
	fb.mu.Lock()
	defer fb.mu.Unlock()
	return out, runID, fb.mergeCalls, fb.auditReadCalls
}

// TestMergeRun_OverlappingImplementReturnsImmediately pins the 409
// overlapping_implement_in_flight as an IMMEDIATE status: exactly ONE POST,
// zero terminal-await reads, every merge flag false, the server message
// verbatim, overlap.blocking decoded, and next_action fishhawk_await_stage on
// details.blocking_run_id / stage_id. Counterfactual: deleting the
// isMergeOverlapRefusal arm in mergeRunCore surfaces the 409 as a tool error
// → RED (runOverlapRefusal's t.Fatalf).
func TestMergeRun_OverlappingImplementReturnsImmediately(t *testing.T) {
	blockRun, blockStage := uuid.NewString(), uuid.NewString()
	msg := "run " + blockRun + " has its implement stage " + blockStage + " running on 1 path(s) this pull request changes at head H. No new merge verdict was recorded."
	out, runID, calls, reads := runOverlapRefusal(t, "overlapping_implement_in_flight", msg, map[string]any{
		"run_id": uuid.NewString(), "pr_url": "https://github.com/x/y/pull/7", "head_sha": "H",
		"blocking": []any{map[string]any{"run_id": blockRun, "issue_ref": "x/y#9", "stage_id": blockStage,
			"stage_type": "implement", "stage_state": "running", "paths": []any{"shared.go"}}},
		"undetermined":    []any{},
		"next_step":       "fishhawk_await_stage",
		"blocking_run_id": blockRun,
		"stage_id":        blockStage,
	})
	if out.Status != "overlapping_implement_in_flight" {
		t.Fatalf("status = %q, want overlapping_implement_in_flight", out.Status)
	}
	if calls != 1 || reads != 0 {
		t.Errorf("POSTs = %d, await reads = %d; want exactly 1 and 0 (immediate, no re-POST)", calls, reads)
	}
	if out.MergeQueued || out.VerdictRecorded || out.AlreadyRecorded || out.AlreadyMerged {
		t.Errorf("flags = %+v, want all false (no verdict, nothing merged)", out)
	}
	if out.Message != msg {
		t.Errorf("message = %q, want the server's verbatim", out.Message)
	}
	if out.PRURL != "https://github.com/x/y/pull/7" {
		t.Errorf("pr_url = %q", out.PRURL)
	}
	na := out.NextAction
	if na == nil || na.Action != "fishhawk_await_stage" || na.Params["run_id"] != blockRun || na.Params["stage_id"] != blockStage || na.Consumes != consumesNone {
		t.Fatalf("next_action = %+v, want fishhawk_await_stage on run %s stage %s consuming none", na, blockRun, blockStage)
	}
	if na.Params["run_id"] == runID.String() {
		t.Errorf("next_action names the merging run, want the BLOCKING run")
	}
	if !strings.Contains(na.Reason, "allow_overlap") {
		t.Errorf("next_action reason %q does not name the override", na.Reason)
	}
	ov := out.Overlap
	if ov == nil || ov.HeadSHA != "H" || len(ov.Blocking) != 1 || ov.Undetermined == nil || len(ov.Undetermined) != 0 {
		t.Fatalf("overlap = %+v, want one blocker at head H and an empty undetermined array", ov)
	}
	if b := ov.Blocking[0]; b.RunID != blockRun || b.StageID != blockStage || b.StageState != "running" || b.IssueRef != "x/y#9" ||
		len(b.Paths) != 1 || b.Paths[0] != "shared.go" {
		t.Errorf("blocker = %+v, want run %s stage %s running on [shared.go]", b, blockRun, blockStage)
	}
}

// TestMergeRun_OverlappingImplementFallsBackToFirstBlocker pins that a detail
// map without blocking_run_id / stage_id (an older backend shape) still names
// the first blocker's run and stage.
func TestMergeRun_OverlappingImplementFallsBackToFirstBlocker(t *testing.T) {
	blockRun, blockStage := uuid.NewString(), uuid.NewString()
	out, _, _, _ := runOverlapRefusal(t, "overlapping_implement_in_flight", "", map[string]any{
		"blocking": []any{
			map[string]any{"run_id": blockRun, "stage_id": blockStage, "stage_type": "implement", "stage_state": "dispatched", "paths": []any{"a.go"}},
			"not-an-object",
		},
	})
	if out.NextAction == nil || out.NextAction.Params["run_id"] != blockRun || out.NextAction.Params["stage_id"] != blockStage {
		t.Fatalf("next_action = %+v, want the first blocker %s / %s", out.NextAction, blockRun, blockStage)
	}
	if len(out.Overlap.Blocking) != 1 {
		t.Errorf("blocking = %+v, want the malformed member dropped", out.Overlap.Blocking)
	}
	if !strings.Contains(out.Message, "fishhawk_await_stage") {
		t.Errorf("empty-message fallback %q does not name the verb", out.Message)
	}
}

// TestMergeRun_OverlapUndeterminedReturnsImmediately pins the 409
// overlap_undetermined as an IMMEDIATE status whose next_action is
// fishhawk_merge_run on THIS run with allow_overlap, the reason naming
// details.reason and the audited override. Counterfactual: deleting
// "overlap_undetermined" from mergeOverlapStatuses makes it a tool error → RED.
func TestMergeRun_OverlapUndeterminedReturnsImmediately(t *testing.T) {
	other := uuid.NewString()
	out, runID, calls, reads := runOverlapRefusal(t, "overlap_undetermined", "cannot rule out an overlap", map[string]any{
		"reason":   "changed_paths_unavailable",
		"blocking": []any{},
		"undetermined": []any{map[string]any{"run_id": other, "stage_id": uuid.NewString(), "stage_type": "implement",
			"stage_state": "running", "paths": []any{}}},
	})
	if out.Status != "overlap_undetermined" {
		t.Fatalf("status = %q, want overlap_undetermined", out.Status)
	}
	if calls != 1 || reads != 0 {
		t.Errorf("POSTs = %d, await reads = %d; want 1 and 0", calls, reads)
	}
	na := out.NextAction
	if na == nil || na.Action != "fishhawk_merge_run" || na.Params["run_id"] != runID.String() || na.Params["allow_overlap"] != "true" {
		t.Fatalf("next_action = %+v, want fishhawk_merge_run on %s with allow_overlap", na, runID)
	}
	if !strings.Contains(na.Reason, "changed_paths_unavailable") || !strings.Contains(na.Reason, "audited") {
		t.Errorf("next_action reason %q must name details.reason and the audited override", na.Reason)
	}
	if out.Overlap == nil || out.Overlap.Reason != "changed_paths_unavailable" || len(out.Overlap.Undetermined) != 1 ||
		out.Overlap.Undetermined[0].RunID != other || out.Overlap.Blocking == nil {
		t.Errorf("overlap = %+v, want reason changed_paths_unavailable with one undetermined run %s", out.Overlap, other)
	}
}

// TestMergeRun_AllowOverlapRequiresReasonLocally pins the local override
// validation (approval conditions C3/C4): allow_overlap with a blank reason,
// and a reason over 2000 bytes (with or without allow_overlap), are tool errors
// with ZERO HTTP calls. Counterfactual: deleting either check lets the POST
// happen → RED on the call count.
func TestMergeRun_AllowOverlapRequiresReasonLocally(t *testing.T) {
	for _, tc := range []struct {
		name   string
		allow  bool
		reason string
		want   string
	}{
		{"empty", true, "", "non-empty overlap_reason"},
		{"whitespace", true, " \t\n ", "non-empty overlap_reason"},
		{"over cap with allow", true, strings.Repeat("r", mergeOverlapReasonMaxBytes+1), "at most 2000 bytes"},
		{"over cap without allow", false, strings.Repeat("r", mergeOverlapReasonMaxBytes+1), "at most 2000 bytes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fb := &mergeRunFakeBackend{prURL: "https://github.com/x/y/pull/7", stateBeforeMerge: "running"}
			srv := newMergeRunFakeBackend(t, fb)
			r := newMergeRunResolver(srv)
			_, _, err := r.mergeRun(context.Background(), nil, MergeRunInput{RunID: uuid.NewString(), Verdict: "ship it",
				AllowOverlap: tc.allow, OverlapReason: tc.reason, TimeoutSeconds: 1})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want a tool error naming %q", err, tc.want)
			}
			fb.mu.Lock()
			defer fb.mu.Unlock()
			if fb.mergeCalls != 0 || fb.getRunCalls != 0 {
				t.Errorf("POSTs = %d, GetRun = %d; want 0 and 0 (refused before any HTTP hop)", fb.mergeCalls, fb.getRunCalls)
			}
		})
	}
}

// TestMergeRun_OverlapOverrideForwardedOnEveryPOST pins that the override
// rides the first POST AND every checks_pending re-POST, with overlap_reason
// byte-for-byte (leading/trailing whitespace kept — approval condition C3),
// and that a reason of exactly the cap is accepted. Counterfactual: sending
// mergeRunRequest{Verdict: verdict} (dropping the override) → RED.
func TestMergeRun_OverlapOverrideForwardedOnEveryPOST(t *testing.T) {
	reason := "  judged safe: B only touches docs <b>  \n"
	for _, tc := range []struct{ name, reason string }{
		{"verbatim whitespace", reason},
		{"exactly the cap", strings.Repeat("x", mergeOverlapReasonMaxBytes)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fb := &mergeRunFakeBackend{
				prURL:            "https://github.com/x/y/pull/7",
				stateBeforeMerge: "running",
				mergeStatuses:    []int{http.StatusConflict},
				mergeErrBody:     mergeCandidateErrBody("merge_checks_pending", "checks pending", map[string]any{"verdict_sequence": 4}),
				mergeResp:        MergeRunResult{MergeQueued: true, VerdictSequence: 4, PRURL: "https://github.com/x/y/pull/7"},
				auditEntries:     []AuditEntry{{Sequence: 9, Category: "pr_merged"}},
			}
			srv := newMergeRunFakeBackend(t, fb)
			r := newMergeRunResolver(srv)
			_, out, err := r.mergeRun(context.Background(), nil, MergeRunInput{RunID: uuid.NewString(), Verdict: "ship it",
				AllowOverlap: true, OverlapReason: tc.reason, TimeoutSeconds: 30})
			if err != nil {
				t.Fatalf("mergeRun: %v", err)
			}
			if out.Status != "merged" {
				t.Fatalf("status = %q, want merged", out.Status)
			}
			fb.mu.Lock()
			defer fb.mu.Unlock()
			if len(fb.mergeRawBodies) != 2 {
				t.Fatalf("POSTs = %d, want 2 (checks_pending, then the merge)", len(fb.mergeRawBodies))
			}
			for i, raw := range fb.mergeRawBodies {
				var got map[string]any
				if err := json.Unmarshal([]byte(raw), &got); err != nil {
					t.Fatalf("POST %d body %q: %v", i, raw, err)
				}
				if got["allow_overlap"] != true || got["overlap_reason"] != tc.reason || got["verdict"] != "ship it" {
					t.Errorf("POST %d body = %s, want allow_overlap true and overlap_reason verbatim", i, raw)
				}
			}
		})
	}
}

// TestMergeRun_NoOverrideOmitsFieldsOnTheWire pins that a merge without an
// override sends today's verdict-only body: allow_overlap / overlap_reason
// are ABSENT, not false / "".
func TestMergeRun_NoOverrideOmitsFieldsOnTheWire(t *testing.T) {
	fb := &mergeRunFakeBackend{
		prURL:            "https://github.com/x/y/pull/7",
		stateBeforeMerge: "running",
		mergeResp:        MergeRunResult{MergeQueued: true, VerdictSequence: 4},
		auditEntries:     []AuditEntry{{Sequence: 9, Category: "pr_merged"}},
	}
	srv := newMergeRunFakeBackend(t, fb)
	r := newMergeRunResolver(srv)
	if _, _, err := r.mergeRun(context.Background(), nil, MergeRunInput{RunID: uuid.NewString(), Verdict: "ship it", TimeoutSeconds: 5}); err != nil {
		t.Fatalf("mergeRun: %v", err)
	}
	fb.mu.Lock()
	defer fb.mu.Unlock()
	if len(fb.mergeRawBodies) != 1 || strings.Contains(fb.mergeRawBodies[0], "overlap") {
		t.Errorf("bodies = %q, want one verdict-only body", fb.mergeRawBodies)
	}
}

// TestMergeRun_OverlapRefusalDuringChecksPendingWait pins that the overlap
// arm is consulted on EVERY POST: an overlap 409 met on a checks_pending
// re-POST returns at once instead of waiting out the budget.
func TestMergeRun_OverlapRefusalDuringChecksPendingWait(t *testing.T) {
	fb := &mergeRunFakeBackend{
		prURL:             "https://github.com/x/y/pull/7",
		stateBeforeMerge:  "running",
		mergeStickyStatus: http.StatusConflict,
		mergeErrBodies: []string{
			mergeCandidateErrBody("merge_checks_pending", "checks pending", map[string]any{"verdict_sequence": 4}),
			mergeCandidateErrBody("overlapping_implement_in_flight", "overlap", map[string]any{"blocking_run_id": "b", "stage_id": "s"}),
		},
	}
	srv := newMergeRunFakeBackend(t, fb)
	r := newMergeRunResolver(srv)
	start := time.Now()
	_, out, err := r.mergeRun(context.Background(), nil, MergeRunInput{RunID: uuid.NewString(), Verdict: "ship it", TimeoutSeconds: 600})
	if err != nil {
		t.Fatalf("mergeRun: %v", err)
	}
	if out.Status != "overlapping_implement_in_flight" {
		t.Fatalf("status = %q, want overlapping_implement_in_flight on the re-POST", out.Status)
	}
	fb.mu.Lock()
	calls := fb.mergeCalls
	fb.mu.Unlock()
	if calls != 2 {
		t.Errorf("POSTs = %d, want 2", calls)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("mergeRun took %s, want an immediate return", elapsed)
	}
}

// TestMergeRun_OverlapRetryableFailuresAreToolErrors pins that the guard's 502
// overlap_check_failed and 503 overlap_override_unrecorded stay TOOL ERRORS
// (retryable failures, not preconditions), and the 400 overlap_reason
// validation passes through as one.
func TestMergeRun_OverlapRetryableFailuresAreToolErrors(t *testing.T) {
	for _, tc := range []struct {
		status int
		code   string
	}{
		{http.StatusBadGateway, "overlap_check_failed"},
		{http.StatusServiceUnavailable, "overlap_override_unrecorded"},
		{http.StatusBadRequest, "validation_failed"},
	} {
		t.Run(tc.code, func(t *testing.T) {
			fb := &mergeRunFakeBackend{
				prURL:             "https://github.com/x/y/pull/7",
				stateBeforeMerge:  "running",
				mergeStickyStatus: tc.status,
				mergeErrBody:      mergeCandidateErrBody(tc.code, "refused", map[string]any{"reason": "forge_read", "retryable": true}),
			}
			srv := newMergeRunFakeBackend(t, fb)
			r := newMergeRunResolver(srv)
			_, out, err := r.mergeRun(context.Background(), nil, MergeRunInput{RunID: uuid.NewString(), Verdict: "ship it",
				AllowOverlap: true, OverlapReason: "safe", TimeoutSeconds: 1})
			if err == nil || !strings.Contains(err.Error(), tc.code) {
				t.Fatalf("err = %v, out = %+v; want a tool error naming %s", err, out, tc.code)
			}
		})
	}
}

// TestMergeOverlapRefusalFrom_Counts pins the #4582 decode (approval
// condition C2): a WITHHELD body (a token without read:audit) decodes empty
// lists TOGETHER with details_withheld, required_scope and the server's
// counts, so a consumer can tell "withheld" from "none"; a VISIBLE body's
// counts are the decoded list lengths. Counterfactual: dropping the server
// count read reports blocking_count 0 on the withheld body → RED.
func TestMergeOverlapRefusalFrom_Counts(t *testing.T) {
	blocker := map[string]any{"run_id": uuid.NewString(), "stage_id": uuid.NewString(), "stage_type": "implement", "stage_state": "running", "paths": []any{"a.go"}}
	for name, tc := range map[string]struct {
		details                map[string]any
		blocking, undetermined int
		lists                  int
		withheld               bool
		scope                  string
	}{
		"withheld": {details: map[string]any{"blocking_count": float64(2), "undetermined_count": float64(1), "details_withheld": true,
			"required_scope": "read:audit", "head_sha": "H", "next_step": "fishhawk_await_stage"}, blocking: 2, undetermined: 1, withheld: true, scope: "read:audit"},
		"visible": {details: map[string]any{"blocking": []any{blocker}, "undetermined": []any{}, "head_sha": "H"}, blocking: 1, lists: 1},
		"withheld_none": {details: map[string]any{"blocking_count": float64(0), "undetermined_count": float64(0), "details_withheld": true,
			"required_scope": "read:audit"}, withheld: true, scope: "read:audit"},
		"malformed_count_falls_back": {details: map[string]any{"blocking": []any{blocker}, "blocking_count": "two", "undetermined_count": float64(-1)}, blocking: 1, lists: 1},
	} {
		t.Run(name, func(t *testing.T) {
			ov := mergeOverlapRefusalFrom(tc.details)
			if ov.BlockingCount != tc.blocking || ov.UndeterminedCount != tc.undetermined {
				t.Fatalf("counts = %d/%d, want %d/%d", ov.BlockingCount, ov.UndeterminedCount, tc.blocking, tc.undetermined)
			}
			if ov.DetailsWithheld != tc.withheld || ov.RequiredScope != tc.scope {
				t.Fatalf("details_withheld = %v required_scope = %q, want %v %q", ov.DetailsWithheld, ov.RequiredScope, tc.withheld, tc.scope)
			}
			if ov.Blocking == nil || ov.Undetermined == nil || len(ov.Blocking) != tc.lists || len(ov.Undetermined) != 0 {
				t.Fatalf("lists = %+v / %+v, want non-nil with %d blocker(s)", ov.Blocking, ov.Undetermined, tc.lists)
			}
		})
	}
}

// TestMergeRun_OverlapWithheldSurfacesCounts: through mergeRunCore, a withheld
// 409 overlapping_implement_in_flight still returns IMMEDIATELY, with the
// server's counts and details_withheld on the output and fishhawk_await_stage
// naming no run or stage (none is known to this caller).
func TestMergeRun_OverlapWithheldSurfacesCounts(t *testing.T) {
	out, _, calls, reads := runOverlapRefusal(t, "overlapping_implement_in_flight", "1 live run(s) elsewhere ... withheld", map[string]any{
		"run_id": uuid.NewString(), "pr_url": "https://github.com/x/y/pull/7", "head_sha": "H",
		"blocking_count": 1, "undetermined_count": 0, "details_withheld": true, "required_scope": "read:audit",
		"next_step": "fishhawk_await_stage",
	})
	if out.Status != "overlapping_implement_in_flight" || calls != 1 || reads != 0 {
		t.Fatalf("status = %q POSTs = %d reads = %d, want an immediate overlapping_implement_in_flight", out.Status, calls, reads)
	}
	ov := out.Overlap
	if ov == nil || ov.BlockingCount != 1 || ov.UndeterminedCount != 0 || !ov.DetailsWithheld || ov.RequiredScope != "read:audit" ||
		ov.HeadSHA != "H" || ov.Blocking == nil || len(ov.Blocking) != 0 {
		t.Fatalf("overlap = %+v, want the withheld counts", ov)
	}
	if na := out.NextAction; na == nil || na.Action != "fishhawk_await_stage" || na.Params["run_id"] != "" || na.Params["stage_id"] != "" {
		t.Fatalf("next_action = %+v, want fishhawk_await_stage with no run or stage", out.NextAction)
	}
}

// TestIsMergeOverlapRefusal pins the code→status table and that only a 409
// maps.
func TestIsMergeOverlapRefusal(t *testing.T) {
	for _, code := range []string{"overlapping_implement_in_flight", "overlap_undetermined"} {
		if _, got, ok := isMergeOverlapRefusal(&apiError{StatusCode: http.StatusConflict, Code: code}); !ok || got != code {
			t.Errorf("%s → (%q, %v), want (%q, true)", code, got, ok, code)
		}
		if _, _, ok := isMergeOverlapRefusal(&apiError{StatusCode: http.StatusBadGateway, Code: code}); ok {
			t.Errorf("%s on a 502 mapped; want only a 409", code)
		}
	}
	for _, code := range []string{"overlap_check_failed", "overlap_override_unrecorded", "merge_base_behind"} {
		if _, _, ok := isMergeOverlapRefusal(&apiError{StatusCode: http.StatusConflict, Code: code}); ok {
			t.Errorf("%s mapped; want it left to the other arms", code)
		}
	}
	if _, _, ok := isMergeOverlapRefusal(errors.New("plain")); ok {
		t.Error("a non-apiError mapped")
	}
}

// TestMergeRun_QueueOwnsMergeStatus: the backend's 409 merge_queue_owns_merge
// (ADR-093 D2/D3, E87.5 / #4296) is an IMMEDIATE status, not a tool error:
// nothing recorded, every merge flag false, next_action
// fishhawk_get_merge_queue, one POST and no terminal await.
func TestMergeRun_QueueOwnsMergeStatus(t *testing.T) {
	runID := uuid.New()
	fb := &mergeRunFakeBackend{
		prURL:            "https://github.com/x/y/pull/7",
		repo:             "x/y",
		stateBeforeMerge: "succeeded",
		mergeStatuses:    []int{http.StatusConflict},
		mergeErrBody:     `{"error":{"code":"merge_queue_owns_merge","message":"the merge queue owns this run's merge","details":{"pr_url":"https://github.com/x/y/pull/7"}}}`,
	}
	r := newMergeRunResolver(newMergeRunFakeBackend(t, fb))
	start := time.Now()
	_, out, err := r.mergeRun(context.Background(), nil, MergeRunInput{RunID: runID.String(), Verdict: "ship it", TimeoutSeconds: 600})
	if err != nil {
		t.Fatalf("mergeRun: %v — merge_queue_owns_merge must be a status, not a tool error", err)
	}
	if out.Status != "merge_queue_owns_merge" || out.Message != "the merge queue owns this run's merge" || out.PRURL != "https://github.com/x/y/pull/7" {
		t.Fatalf("out = %+v, want status merge_queue_owns_merge with the backend message", out)
	}
	if out.MergeQueued || out.VerdictRecorded || out.AlreadyRecorded || out.AlreadyMerged {
		t.Errorf("merge flags = %+v, want all false (nothing recorded)", out)
	}
	na := out.NextAction
	if na == nil || na.Action != "fishhawk_get_merge_queue" || na.Params["run_id"] != runID.String() || na.Params["repo"] != "x/y" {
		t.Fatalf("next_action = %+v, want fishhawk_get_merge_queue", na)
	}
	fb.mu.Lock()
	calls, reads := fb.mergeCalls, fb.auditReadCalls
	fb.mu.Unlock()
	if calls != 1 || reads != 0 {
		t.Errorf("POSTs = %d, audit reads = %d; want 1 and 0", calls, reads)
	}
	if time.Since(start) > 5*time.Second {
		t.Error("mergeRun did not return immediately")
	}
	// A blank message falls back to the constant.
	if got := queueOwnsMergeOutput(&apiError{Code: "merge_queue_owns_merge"}, "", runID, start); !strings.Contains(got.Message, "fishhawk_get_merge_queue") {
		t.Errorf("fallback message = %q", got.Message)
	}
	if _, owned := isQueueOwnsMerge(errors.New("plain")); owned {
		t.Error("a non-API error classified as merge_queue_owns_merge")
	}
}

// TestMergeRun_ForgeApprovalRequired_NextStepMapping pins the ADR-112 (#4619)
// 409 forge_approval_required: an immediate status whose next_action follows
// details.next_step. Counterfactual: removing the readinessStatuses entry
// turns the 409 into a tool error → RED.
func TestMergeRun_ForgeApprovalRequired_NextStepMapping(t *testing.T) {
	const prURL = "https://github.com/x/y/pull/7"
	cases := []struct {
		step, reviewer, wantAction string
	}{
		{"approve_pr", "", "approve_pr"},
		{"dismiss_review", "vera", "dismiss_review"},
		{"retry_after", "", "fishhawk_merge_run"},
		{"configure_identity_provider", "", ""},
		{"", "", ""},
	}
	for _, tc := range cases {
		t.Run("next_step="+tc.step, func(t *testing.T) {
			details := map[string]any{"pr_url": prURL, "fold": "absent", "reason": "no_qualifying_approval", "next_step": tc.step}
			if tc.reviewer != "" {
				details["veto_reviewer"] = tc.reviewer
			}
			out, runID := runReadinessRefusal(t, "forge_approval_required", "no qualifying forge approval covers the verified head", details)
			if out.Status != "forge_approval_required" {
				t.Fatalf("status = %q, want forge_approval_required", out.Status)
			}
			if tc.wantAction == "" {
				if out.NextAction != nil {
					t.Fatalf("next_action = %+v, want none", out.NextAction)
				}
				return
			}
			if out.NextAction == nil || out.NextAction.Action != tc.wantAction {
				t.Fatalf("next_action = %+v, want %s", out.NextAction, tc.wantAction)
			}
			if out.NextAction.Params["run_id"] != runID.String() || out.NextAction.Params["pr_url"] != prURL {
				t.Errorf("params = %v", out.NextAction.Params)
			}
			if tc.reviewer != "" && (out.NextAction.Params["reviewer"] != tc.reviewer || !strings.Contains(out.NextAction.Reason, tc.reviewer)) {
				t.Errorf("dismiss_review does not name the reviewer: %+v", out.NextAction)
			}
		})
	}
	// An empty server message still renders without a next action.
	out := mergeReadinessOutput(&apiError{StatusCode: http.StatusConflict, Code: "forge_approval_required",
		Details: map[string]any{"next_step": "configure_identity_provider"}}, "forge_approval_required", uuid.New(), time.Now())
	if out.NextAction != nil || !strings.Contains(out.Message, "forge_approval_required") {
		t.Errorf("empty-message output = %+v", out)
	}
}
