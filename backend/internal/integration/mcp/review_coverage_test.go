package mcpe2e_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/defaultbranch"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/orchestrator"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan/planfixture"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	runpkg "github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/server"
	"github.com/kuhlman-labs/fishhawk/backend/internal/signing"
)

// rcWorkflowSpec declares ONE advisory agent reviewer on the implement stage
// (agent: 1 beside human: 1 resolves advisory), so the review-coverage hold
// applies and a re-run round dispatches exactly one reviewer.
var rcWorkflowSpec = []byte(`version: "0.3"
workflows:
  feature_change:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
      - id: implement
        type: implement
        executor:
          agent: claude-code
        reviewers:
          agent: 1
          human: 1
`)

// rcApproveReviewer is the wired reviewer backend: every review approves, and
// the calls are counted so the test can assert the re-run actually reviewed.
type rcApproveReviewer struct {
	mu    sync.Mutex
	calls int
}

func (r *rcApproveReviewer) Review(context.Context, string) (*planreview.ReviewVerdict, string, error) {
	r.mu.Lock()
	r.calls++
	r.mu.Unlock()
	return &planreview.ReviewVerdict{Verdict: planreview.VerdictApprove}, "rc-fake-reviewer", nil
}

func (r *rcApproveReviewer) Calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// rcReviewerSet wires rcApproveReviewer as the deployment's default backend.
type rcReviewerSet struct{ r *rcApproveReviewer }

func (s rcReviewerSet) Default() server.PlanReviewer { return s.r }
func (s rcReviewerSet) For(string, string, ...string) (server.PlanReviewer, error) {
	return s.r, nil
}

type rcTokenProvider struct{}

func (rcTokenProvider) Token(context.Context, int64) (string, error) { return "ghs_rc", nil }

// rcNextAction decodes a tool's next_action.
type rcNextAction struct {
	Action string            `json:"action"`
	Params map[string]string `json:"params"`
}

// rcHarness is the shared cross-boundary fixture of the review-coverage e2e
// tests: a decomposed parent (stored approved plan, implement stage parked at
// its gate, open PR, installation) with two SUCCEEDED slices that have no
// review round of their own, a fishhawkd wired with the counting reviewer and
// a fake forge whose compare answers headSHA, and the real fishhawk-mcp binary
// connected to it.
type rcHarness struct {
	t         *testing.T
	ctx       context.Context
	auditRepo audit.Repository
	reviewer  *rcApproveReviewer
	parent    *runpkg.Run
	implStage *runpkg.Stage
	session   *mcp.ClientSession

	comparesMu sync.Mutex
	compares   []string
}

func newRCHarness(t *testing.T, ctx context.Context, prURL, headSHA string) *rcHarness {
	t.Helper()
	fx := newFixture(t)
	h := &rcHarness{t: t, ctx: ctx, reviewer: &rcApproveReviewer{}}

	forge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !strings.Contains(r.URL.Path, "/compare/") {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"message":"Not Found"}`)
			return
		}
		h.comparesMu.Lock()
		h.compares = append(h.compares, r.URL.Path)
		h.comparesMu.Unlock()
		_, _ = io.WriteString(w, `{"total_commits":1,"commits":[{"sha":"`+headSHA+`"}],`+
			`"files":[{"filename":"x.go","status":"modified","changes":2,"patch":"@@ -1 +1 @@\n-a\n+b"}]}`)
	}))
	t.Cleanup(forge.Close)

	h.auditRepo = audit.NewPostgresRepository(fx.pool)
	artifactRepo := artifact.NewPostgresRepository(fx.pool)
	srv := server.New(server.Config{
		Addr:          "127.0.0.1:0",
		RunRepo:       fx.runRepo,
		AuditRepo:     h.auditRepo,
		ArtifactRepo:  artifactRepo,
		SigningRepo:   signing.NewPostgresRepository(fx.pool),
		APITokenRepo:  fx.apitokenRepo,
		PlanReviewers: rcReviewerSet{h.reviewer},
		GitHub: &githubclient.Client{
			BaseURL: forge.URL,
			Tokens:  rcTokenProvider{},
			HTTP:    &http.Client{Timeout: 10 * time.Second},
			AppJWT:  func() (string, error) { return "ghs_jwt", nil },
		},
		// The first-round re-run resolves its compare base from the
		// repository's default branch (#4660); this forge fake serves only
		// the compare, so a fixed source stands in for the forge read.
		DefaultBranch: defaultbranch.Static("main"),
	})
	httpSrv := httptest.NewServer(srv.Handler())
	t.Cleanup(httpSrv.Close)

	// The decomposed parent: a stored approved plan, an implement stage parked
	// at its gate, an open PR and an installation (the forge compare needs it).
	requiresCharter := false
	inst := int64(4219)
	parent, err := fx.runRepo.CreateRun(ctx, runpkg.CreateRunParams{
		Repo:            "kuhlman-labs/fishhawk",
		WorkflowID:      "feature_change",
		WorkflowSHA:     "deadbeef",
		TriggerSource:   runpkg.TriggerCLI,
		WorkflowSpec:    rcWorkflowSpec,
		RequiresCharter: &requiresCharter,
		InstallationID:  &inst,
	})
	if err != nil {
		t.Fatalf("CreateRun(parent): %v", err)
	}
	h.parent = parent
	if _, err := fx.runRepo.TransitionRun(ctx, parent.ID, runpkg.StateRunning); err != nil {
		t.Fatalf("TransitionRun(parent → running): %v", err)
	}
	planStage, err := fx.runRepo.CreateStage(ctx, runpkg.CreateStageParams{
		RunID: parent.ID, Sequence: 1, Type: runpkg.StageTypePlan,
		ExecutorKind: runpkg.ExecutorAgent, ExecutorRef: "fishhawk/runner@v1", RequiresApproval: true,
	})
	if err != nil {
		t.Fatalf("CreateStage(plan): %v", err)
	}
	planContent, err := json.Marshal(planfixture.Valid())
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	sv := "standard_v1"
	sum := sha256.Sum256(planContent)
	if _, err := artifactRepo.Create(ctx, artifact.CreateParams{
		StageID: planStage.ID, Kind: artifact.KindPlan, SchemaVersion: &sv,
		Content: planContent, ContentHash: hex.EncodeToString(sum[:]),
	}); err != nil {
		t.Fatalf("Create plan artifact: %v", err)
	}
	driveStageToSucceeded(t, ctx, fx.runRepo, planStage.ID)
	h.implStage, err = fx.runRepo.CreateStage(ctx, runpkg.CreateStageParams{
		RunID: parent.ID, Sequence: 2, Type: runpkg.StageTypeImplement,
		ExecutorKind: runpkg.ExecutorAgent, ExecutorRef: "fishhawk/runner@v1", RequiresApproval: true,
	})
	if err != nil {
		t.Fatalf("CreateStage(implement): %v", err)
	}
	parkAtGate(t, ctx, fx.runRepo, h.implStage.ID)
	if _, err := fx.runRepo.SetRunPullRequestURL(ctx, parent.ID, prURL); err != nil {
		t.Fatalf("SetRunPullRequestURL: %v", err)
	}

	// Two SUCCEEDED slices, neither with a review round of its own.
	for i := range 2 {
		idx := i
		child, err := fx.runRepo.CreateRun(ctx, runpkg.CreateRunParams{
			Repo: "kuhlman-labs/fishhawk", WorkflowID: "feature_change", WorkflowSHA: "deadbeef",
			TriggerSource: runpkg.TriggerCLI, WorkflowSpec: rcWorkflowSpec, RequiresCharter: &requiresCharter,
			DecomposedFrom: &parent.ID, SliceIndex: &idx,
		})
		if err != nil {
			t.Fatalf("CreateRun(child %d): %v", i, err)
		}
		for _, st := range []runpkg.State{runpkg.StateRunning, runpkg.StateSucceeded} {
			if _, err := fx.runRepo.TransitionRun(ctx, child.ID, st); err != nil {
				t.Fatalf("TransitionRun(child %d → %s): %v", i, st, err)
			}
		}
	}

	h.session = connectMCPClient(t, ctx, fx.mcpBinary, fx.operatorTok, httpSrv.URL)
	return h
}

// appendEntry appends a system-actor entry on the parent implement stage.
func (h *rcHarness) appendEntry(category string, payload any) int64 {
	h.t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		h.t.Fatalf("marshal %s payload: %v", category, err)
	}
	kind := audit.ActorKind("system")
	e, err := h.auditRepo.AppendChained(h.ctx, audit.ChainAppendParams{
		RunID: h.parent.ID, StageID: &h.implStage.ID, Timestamp: time.Now(),
		Category: category, ActorKind: &kind, Payload: raw,
	})
	if err != nil {
		h.t.Fatalf("AppendChained %s: %v", category, err)
	}
	return e.Sequence
}

// entries lists the parent's committed entries of one category.
func (h *rcHarness) entries(category string) []*audit.Entry {
	h.t.Helper()
	rows, err := h.auditRepo.ListForRunByCategory(h.ctx, h.parent.ID, category)
	if err != nil {
		h.t.Fatalf("ListForRunByCategory %s: %v", category, err)
	}
	return rows
}

// countCategory counts the parent's entries of one category sequenced after
// the given sequence.
func (h *rcHarness) countCategory(category string, after int64) int {
	h.t.Helper()
	n := 0
	for _, e := range h.entries(category) {
		if e.Sequence > after {
			n++
		}
	}
	return n
}

// comparePaths snapshots the forge compare request paths.
func (h *rcHarness) comparePaths() []string {
	h.comparesMu.Lock()
	defer h.comparesMu.Unlock()
	return append([]string(nil), h.compares...)
}

// mergeRun calls fishhawk_merge_run on the parent and decodes a non-error
// result's status, next_action and message.
func (h *rcHarness) mergeRun() (*mcp.CallToolResult, string, *rcNextAction, string) {
	h.t.Helper()
	res, err := h.session.CallTool(h.ctx, &mcp.CallToolParams{
		Name:      "fishhawk_merge_run",
		Arguments: map[string]any{"run_id": h.parent.ID.String(), "verdict": "ship it", "timeout_seconds": 30},
	})
	if err != nil {
		h.t.Fatalf("CallTool fishhawk_merge_run: %v", err)
	}
	if res.IsError {
		return res, "", nil, ""
	}
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		h.t.Fatalf("marshal merge_run output: %v", err)
	}
	var out struct {
		Status     string        `json:"status"`
		Message    string        `json:"message"`
		NextAction *rcNextAction `json:"next_action"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		h.t.Fatalf("decode merge_run output: %v\nraw: %s", err, raw)
	}
	return res, out.Status, out.NextAction, out.Message
}

// rerun calls fishhawk_rerun_review on the parent.
func (h *rcHarness) rerun() *mcp.CallToolResult {
	h.t.Helper()
	res, err := h.session.CallTool(h.ctx, &mcp.CallToolParams{
		Name:      "fishhawk_rerun_review",
		Arguments: map[string]any{"run_id": h.parent.ID.String()},
	})
	if err != nil {
		h.t.Fatalf("CallTool fishhawk_rerun_review: %v", err)
	}
	return res
}

// rcRerunOutput is the decoded fishhawk_rerun_review structured output.
type rcRerunOutput struct {
	Result struct {
		RunID            string `json:"run_id"`
		RerunOfSequence  int64  `json:"rerun_of_sequence"`
		NewRoundSequence int64  `json:"new_round_sequence"`
		ConfiguredAgents int    `json:"configured_agents"`
	} `json:"result"`
	Message    string        `json:"message"`
	NextAction *rcNextAction `json:"next_action"`
}

func (h *rcHarness) decodeRerun(res *mcp.CallToolResult) rcRerunOutput {
	h.t.Helper()
	if res.IsError {
		h.t.Fatalf("rerun returned a tool error: %s", toolContentString(h.t, res))
	}
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		h.t.Fatalf("marshal rerun output: %v", err)
	}
	var out rcRerunOutput
	if err := json.Unmarshal(raw, &out); err != nil {
		h.t.Fatalf("decode rerun output: %v\nraw: %s", err, raw)
	}
	return out
}

// roundPayload decodes the parent's implement_review_started at seq, or nil.
func (h *rcHarness) roundPayload(seq int64) *planreview.ReviewStartedPayload {
	h.t.Helper()
	for _, e := range h.entries("implement_review_started") {
		if e.Sequence == seq {
			var p planreview.ReviewStartedPayload
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				h.t.Fatalf("decode round %d: %v", seq, err)
			}
			return &p
		}
	}
	return nil
}

// awaitVerdictAfter waits for the detached advisory reviewer's verdict on the
// round at seq.
func (h *rcHarness) awaitVerdictAfter(seq int64) {
	h.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for h.countCategory("implement_reviewed", seq) == 0 {
		if time.Now().After(deadline) {
			h.t.Fatalf("no implement_reviewed landed on round %d (reviewer calls = %d)", seq, h.reviewer.Calls())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// assertHoldCleared: merge_run passes the coverage refusal and stops at the
// next guard, the unwired merge seam (this fixture wires no GateMerger).
func (h *rcHarness) assertHoldCleared() {
	h.t.Helper()
	res, status, _, _ := h.mergeRun()
	if !res.IsError {
		h.t.Fatalf("cleared merge_run = status %q, want the merge_seam_unconfigured tool error past the cleared hold", status)
	}
	if text := toolContentString(h.t, res); !strings.Contains(text, "merge_seam_unconfigured") || strings.Contains(text, "review_coverage_incomplete") {
		h.t.Errorf("cleared merge_run error = %s, want merge_seam_unconfigured and no review_coverage_incomplete", text)
	}
}

// TestE2E_ReviewCoverage_HoldAndRerun is the #4219 cross-boundary seam. A
// decomposed parent whose consolidated implement-review round FAILED (every
// reviewer failed, zero verdicts) and whose two succeeded slices were never
// reviewed crosses Postgres (runs, stages, the plan artifact), the audit chain,
// the server's merge and rerun handlers, and the real fishhawk-mcp binary:
//
//  1. round IN FLIGHT (1 configured, 0 landed): fishhawk_merge_run returns
//     status=review_coverage_incomplete naming fishhawk_await_review, and
//     fishhawk_rerun_review is refused review_round_in_flight with nothing
//     written.
//  2. round SETTLED with no verdict: merge_run's next_action becomes
//     fishhawk_rerun_review, and no merge verdict is recorded.
//  3. fishhawk_rerun_review re-runs the round (a forge compare of the
//     recorded base..head, the same-head dedup bypassed), appends one
//     review_round_rerun_requested row before the new round, and the
//     reviewer's verdict lands on the new round, whose rerun_of names the old.
//  4. the hold CLEARS: merge_run passes the coverage refusal and reaches the
//     next guard (merge_seam_unconfigured — this fixture wires no GateMerger).
func TestE2E_ReviewCoverage_HoldAndRerun(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	const (
		prURL    = "https://github.com/kuhlman-labs/fishhawk/pull/4219"
		baseSHA  = "c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0"
		headSHA  = "d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1"
		rerunCat = "review_round_rerun_requested"
	)
	h := newRCHarness(t, ctx, prURL, headSHA)
	parent := h.parent

	// The consolidated round, dispatched with one advisory reviewer.
	roundSeq := h.appendEntry("implement_review_started", planreview.ReviewStartedPayload{
		ConfiguredAgents: 1, Authority: planreview.AuthorityAdvisory,
		RoundOrigin: "consolidated", RoundBaseSHA: baseSHA, HeadSHA: headSHA,
	})

	// 1. IN FLIGHT: the hold names the await verb; the re-run is refused.
	res, status, next, msg := h.mergeRun()
	if res.IsError || status != "review_coverage_incomplete" {
		t.Fatalf("in-flight merge_run = status %q (tool error %v: %s), want review_coverage_incomplete", status, res.IsError, toolContentString(t, res))
	}
	if next == nil || next.Action != "fishhawk_await_review" {
		t.Errorf("in-flight next_action = %+v, want fishhawk_await_review", next)
	}
	if !strings.Contains(msg, "slice 0") || !strings.Contains(msg, "slice 1") {
		t.Errorf("in-flight message %q does not name both uncovered slices", msg)
	}
	inFlight := h.rerun()
	if !inFlight.IsError || !strings.Contains(toolContentString(t, inFlight), "review_round_in_flight") {
		t.Fatalf("in-flight rerun = %s (tool error %v), want a review_round_in_flight refusal", toolContentString(t, inFlight), inFlight.IsError)
	}
	if n := h.countCategory(rerunCat, 0); n != 0 {
		t.Errorf("%s rows after the in-flight refusal = %d, want 0", rerunCat, n)
	}
	if n := h.countCategory("implement_review_started", roundSeq); n != 0 {
		t.Errorf("new rounds after the in-flight refusal = %d, want 0", n)
	}

	// 2. SETTLED with no verdict: the reviewer failed (quota). The hold now
	// names the re-run verb.
	h.appendEntry("implement_review_failed", planreview.ReviewFailedPayload{Reason: "provider quota exhausted", Authority: planreview.AuthorityAdvisory})
	res, status, next, _ = h.mergeRun()
	if res.IsError || status != "review_coverage_incomplete" {
		t.Fatalf("settled merge_run = status %q (tool error %v: %s), want review_coverage_incomplete", status, res.IsError, toolContentString(t, res))
	}
	if next == nil || next.Action != "fishhawk_rerun_review" || next.Params["run_id"] != parent.ID.String() {
		t.Errorf("settled next_action = %+v, want fishhawk_rerun_review on run %s", next, parent.ID)
	}
	if n := h.countCategory(server.CategoryMergeVerdictRecorded, 0); n != 0 {
		t.Errorf("merge_verdict_recorded rows = %d, want 0 (the hold refuses before the append)", n)
	}

	// 3. The re-run through the real binary.
	out := h.decodeRerun(h.rerun())
	if out.Result.RunID != parent.ID.String() || out.Result.RerunOfSequence != roundSeq || out.Result.NewRoundSequence <= roundSeq || out.Result.ConfiguredAgents != 1 {
		t.Fatalf("rerun result = %+v, want run %s re-running %d as a newer round with 1 reviewer", out.Result, parent.ID, roundSeq)
	}
	if out.NextAction == nil || out.NextAction.Action != "fishhawk_await_review" {
		t.Errorf("rerun next_action = %+v, want fishhawk_await_review", out.NextAction)
	}

	// Committed state: one request row BEFORE the new round, and the new
	// round's rerun_of names the old one.
	rows := h.entries(rerunCat)
	if len(rows) != 1 || rows[0].Sequence >= out.Result.NewRoundSequence {
		t.Fatalf("%s rows = %d, want exactly 1 sequenced before the new round", rerunCat, len(rows))
	}
	if newRound := h.roundPayload(out.Result.NewRoundSequence); newRound == nil || newRound.RerunOf != roundSeq || newRound.HeadSHA != headSHA {
		t.Fatalf("new round = %+v, want rerun_of %d at head %s", newRound, roundSeq, headSHA)
	}
	if got := h.comparePaths(); len(got) != 1 || !strings.HasSuffix(got[0], "/compare/"+baseSHA+"..."+headSHA) {
		t.Errorf("forge compares = %v, want one compare of %s...%s", got, baseSHA, headSHA)
	}

	// The advisory reviewer runs detached: wait for its verdict on the new
	// round.
	h.awaitVerdictAfter(out.Result.NewRoundSequence)

	// 4. The hold CLEARS.
	h.assertHoldCleared()
}

// TestE2E_ReviewCoverage_FirstRoundRecovery is the #4282 cross-boundary seam:
// the no-round dead-end. A decomposed parent with two succeeded, unreviewed
// slices, an OPEN consolidated PR and NO parent implement round — its
// consolidated dispatch never emitted implement_review_started (the forge
// compare failed, or no forge was wired when the orchestrator fired it) —
// crosses Postgres, the audit chain, the coverage predicate, the server's
// merge and rerun handlers, the forge compare and the real fishhawk-mcp
// binary:
//
//  1. fishhawk_merge_run returns status=review_coverage_incomplete whose
//     next_action is fishhawk_rerun_review (before #4282: fishhawk_await_review,
//     and the verb refused no_review_round), with no merge verdict recorded.
//  2. fishhawk_rerun_review starts the parent's FIRST consolidated round:
//     rerun_of_sequence 0, a review_round_rerun_requested row (first_round
//     true, rerun_of_sequence 0) sequenced BEFORE the new round, a forge
//     compare of the default ref against the consolidated branch, and a round
//     with round_origin consolidated, round_base_sha main, the compare's head
//     and NO rerun_of.
//  3. the reviewer's verdict lands on that round and the hold CLEARS.
//
// The fixture wires no ConcernRepo, so the permission-drift check is skipped
// and the single compare is the round's own; rcWorkflowSpec declares no verify
// command, so the ADR-090 D5 fan-in hold does not apply (not_required).
func TestE2E_ReviewCoverage_FirstRoundRecovery(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	const (
		prURL    = "https://github.com/kuhlman-labs/fishhawk/pull/4282"
		headSHA  = "e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2"
		rerunCat = "review_round_rerun_requested"
	)
	h := newRCHarness(t, ctx, prURL, headSHA)
	parent := h.parent
	if n := h.countCategory("implement_review_started", 0); n != 0 {
		t.Fatalf("precondition: the parent carries %d implement rounds, want 0 (the no-round dead-end)", n)
	}

	// 1. The hold names the verb that starts the first round.
	res, status, next, msg := h.mergeRun()
	if res.IsError || status != "review_coverage_incomplete" {
		t.Fatalf("no-round merge_run = status %q (tool error %v: %s), want review_coverage_incomplete", status, res.IsError, toolContentString(t, res))
	}
	if next == nil || next.Action != "fishhawk_rerun_review" || next.Params["run_id"] != parent.ID.String() {
		t.Errorf("no-round next_action = %+v, want fishhawk_rerun_review on run %s", next, parent.ID)
	}
	if !strings.Contains(msg, "fishhawk_rerun_review") || !strings.Contains(msg, "first consolidated round") {
		t.Errorf("no-round message %q does not name the first-round remedy", msg)
	}
	if n := h.countCategory(server.CategoryMergeVerdictRecorded, 0); n != 0 {
		t.Errorf("merge_verdict_recorded rows = %d, want 0 (the hold refuses before the append)", n)
	}

	// 2. The first round through the real binary.
	out := h.decodeRerun(h.rerun())
	if out.Result.RunID != parent.ID.String() || out.Result.RerunOfSequence != 0 || out.Result.NewRoundSequence <= 0 || out.Result.ConfiguredAgents != 1 {
		t.Fatalf("rerun result = %+v, want run %s starting its first round (rerun_of_sequence 0) with 1 reviewer", out.Result, parent.ID)
	}
	if !strings.Contains(out.Message, "FIRST consolidated implement-review round") {
		t.Errorf("rerun message = %q, want it to name the parent's first consolidated round", out.Message)
	}
	if out.NextAction == nil || out.NextAction.Action != "fishhawk_await_review" || out.NextAction.Params["stage"] != "implement" {
		t.Errorf("rerun next_action = %+v, want fishhawk_await_review on stage implement", out.NextAction)
	}

	// Committed state: exactly one request row, marked first_round, BEFORE
	// exactly one round; the round is the consolidated round, not a re-run.
	rows := h.entries(rerunCat)
	if len(rows) != 1 || rows[0].Sequence >= out.Result.NewRoundSequence {
		t.Fatalf("%s rows = %d, want exactly 1 sequenced before the new round", rerunCat, len(rows))
	}
	var req struct {
		RerunOfSequence int64  `json:"rerun_of_sequence"`
		FirstRound      bool   `json:"first_round"`
		RoundOrigin     string `json:"round_origin"`
		StageID         string `json:"stage_id"`
	}
	if err := json.Unmarshal(rows[0].Payload, &req); err != nil {
		t.Fatalf("decode %s: %v", rerunCat, err)
	}
	if !req.FirstRound || req.RerunOfSequence != 0 || req.RoundOrigin != "consolidated" || req.StageID != h.implStage.ID.String() {
		t.Errorf("%s payload = %+v, want first_round true, rerun_of_sequence 0, round_origin consolidated on stage %s", rerunCat, req, h.implStage.ID)
	}
	if n := h.countCategory("implement_review_started", 0); n != 1 {
		t.Fatalf("implement rounds = %d, want exactly 1 (the first consolidated round)", n)
	}
	round := h.roundPayload(out.Result.NewRoundSequence)
	if round == nil || round.RoundOrigin != "consolidated" || round.RoundBaseSHA != "main" || round.HeadSHA != headSHA || round.RerunOf != 0 {
		t.Fatalf("first round = %+v, want round_origin consolidated, round_base_sha main, head %s, no rerun_of", round, headSHA)
	}
	wantCompare := "/compare/main..." + orchestrator.ConsolidatedBranch(parent.ID)
	if got := h.comparePaths(); len(got) != 1 || !strings.HasSuffix(got[0], wantCompare) {
		t.Errorf("forge compares = %v, want one compare ending %s", got, wantCompare)
	}

	// 3. The verdict lands and the hold CLEARS.
	h.awaitVerdictAfter(out.Result.NewRoundSequence)
	h.assertHoldCleared()
}
