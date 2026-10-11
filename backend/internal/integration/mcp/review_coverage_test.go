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
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
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
	fx := newFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	const (
		prURL    = "https://github.com/kuhlman-labs/fishhawk/pull/4219"
		baseSHA  = "c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0"
		headSHA  = "d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1"
		rerunCat = "review_round_rerun_requested"
	)

	var comparesMu sync.Mutex
	var compares []string
	forge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !strings.Contains(r.URL.Path, "/compare/") {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"message":"Not Found"}`)
			return
		}
		comparesMu.Lock()
		compares = append(compares, r.URL.Path)
		comparesMu.Unlock()
		_, _ = io.WriteString(w, `{"total_commits":1,"commits":[{"sha":"`+headSHA+`"}],`+
			`"files":[{"filename":"x.go","status":"modified","changes":2,"patch":"@@ -1 +1 @@\n-a\n+b"}]}`)
	}))
	t.Cleanup(forge.Close)

	auditRepo := audit.NewPostgresRepository(fx.pool)
	artifactRepo := artifact.NewPostgresRepository(fx.pool)
	reviewer := &rcApproveReviewer{}
	srv := server.New(server.Config{
		Addr:          "127.0.0.1:0",
		RunRepo:       fx.runRepo,
		AuditRepo:     auditRepo,
		ArtifactRepo:  artifactRepo,
		SigningRepo:   signing.NewPostgresRepository(fx.pool),
		APITokenRepo:  fx.apitokenRepo,
		PlanReviewers: rcReviewerSet{reviewer},
		GitHub: &githubclient.Client{
			BaseURL: forge.URL,
			Tokens:  rcTokenProvider{},
			HTTP:    &http.Client{Timeout: 10 * time.Second},
			AppJWT:  func() (string, error) { return "ghs_jwt", nil },
		},
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
	implStage, err := fx.runRepo.CreateStage(ctx, runpkg.CreateStageParams{
		RunID: parent.ID, Sequence: 2, Type: runpkg.StageTypeImplement,
		ExecutorKind: runpkg.ExecutorAgent, ExecutorRef: "fishhawk/runner@v1", RequiresApproval: true,
	})
	if err != nil {
		t.Fatalf("CreateStage(implement): %v", err)
	}
	parkAtGate(t, ctx, fx.runRepo, implStage.ID)
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

	kind := audit.ActorKind("system")
	appendEntry := func(category string, payload any) int64 {
		t.Helper()
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("marshal %s payload: %v", category, err)
		}
		e, err := auditRepo.AppendChained(ctx, audit.ChainAppendParams{
			RunID: parent.ID, StageID: &implStage.ID, Timestamp: time.Now(),
			Category: category, ActorKind: &kind, Payload: raw,
		})
		if err != nil {
			t.Fatalf("AppendChained %s: %v", category, err)
		}
		return e.Sequence
	}
	countCategory := func(category string, after int64) int {
		t.Helper()
		rows, err := auditRepo.ListForRunByCategory(ctx, parent.ID, category)
		if err != nil {
			t.Fatalf("ListForRunByCategory %s: %v", category, err)
		}
		n := 0
		for _, e := range rows {
			if e.Sequence > after {
				n++
			}
		}
		return n
	}

	// The consolidated round, dispatched with one advisory reviewer.
	roundSeq := appendEntry("implement_review_started", planreview.ReviewStartedPayload{
		ConfiguredAgents: 1, Authority: planreview.AuthorityAdvisory,
		RoundOrigin: "consolidated", RoundBaseSHA: baseSHA, HeadSHA: headSHA,
	})

	session := connectMCPClient(t, ctx, fx.mcpBinary, fx.operatorTok, httpSrv.URL)
	type nextAction struct {
		Action string            `json:"action"`
		Params map[string]string `json:"params"`
	}
	mergeRun := func() (*mcp.CallToolResult, string, *nextAction, string) {
		t.Helper()
		res, err := session.CallTool(ctx, &mcp.CallToolParams{
			Name:      "fishhawk_merge_run",
			Arguments: map[string]any{"run_id": parent.ID.String(), "verdict": "ship it", "timeout_seconds": 30},
		})
		if err != nil {
			t.Fatalf("CallTool fishhawk_merge_run: %v", err)
		}
		if res.IsError {
			return res, "", nil, ""
		}
		raw, err := json.Marshal(res.StructuredContent)
		if err != nil {
			t.Fatalf("marshal merge_run output: %v", err)
		}
		var out struct {
			Status     string      `json:"status"`
			Message    string      `json:"message"`
			NextAction *nextAction `json:"next_action"`
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("decode merge_run output: %v\nraw: %s", err, raw)
		}
		return res, out.Status, out.NextAction, out.Message
	}
	rerun := func() *mcp.CallToolResult {
		t.Helper()
		res, err := session.CallTool(ctx, &mcp.CallToolParams{
			Name:      "fishhawk_rerun_review",
			Arguments: map[string]any{"run_id": parent.ID.String()},
		})
		if err != nil {
			t.Fatalf("CallTool fishhawk_rerun_review: %v", err)
		}
		return res
	}

	// 1. IN FLIGHT: the hold names the await verb; the re-run is refused.
	res, status, next, msg := mergeRun()
	if res.IsError || status != "review_coverage_incomplete" {
		t.Fatalf("in-flight merge_run = status %q (tool error %v: %s), want review_coverage_incomplete", status, res.IsError, toolContentString(t, res))
	}
	if next == nil || next.Action != "fishhawk_await_review" {
		t.Errorf("in-flight next_action = %+v, want fishhawk_await_review", next)
	}
	if !strings.Contains(msg, "slice 0") || !strings.Contains(msg, "slice 1") {
		t.Errorf("in-flight message %q does not name both uncovered slices", msg)
	}
	inFlight := rerun()
	if !inFlight.IsError || !strings.Contains(toolContentString(t, inFlight), "review_round_in_flight") {
		t.Fatalf("in-flight rerun = %s (tool error %v), want a review_round_in_flight refusal", toolContentString(t, inFlight), inFlight.IsError)
	}
	if n := countCategory(rerunCat, 0); n != 0 {
		t.Errorf("%s rows after the in-flight refusal = %d, want 0", rerunCat, n)
	}
	if n := countCategory("implement_review_started", roundSeq); n != 0 {
		t.Errorf("new rounds after the in-flight refusal = %d, want 0", n)
	}

	// 2. SETTLED with no verdict: the reviewer failed (quota). The hold now
	// names the re-run verb.
	appendEntry("implement_review_failed", planreview.ReviewFailedPayload{Reason: "provider quota exhausted", Authority: planreview.AuthorityAdvisory})
	res, status, next, _ = mergeRun()
	if res.IsError || status != "review_coverage_incomplete" {
		t.Fatalf("settled merge_run = status %q (tool error %v: %s), want review_coverage_incomplete", status, res.IsError, toolContentString(t, res))
	}
	if next == nil || next.Action != "fishhawk_rerun_review" || next.Params["run_id"] != parent.ID.String() {
		t.Errorf("settled next_action = %+v, want fishhawk_rerun_review on run %s", next, parent.ID)
	}
	if n := countCategory(server.CategoryMergeVerdictRecorded, 0); n != 0 {
		t.Errorf("merge_verdict_recorded rows = %d, want 0 (the hold refuses before the append)", n)
	}

	// 3. The re-run through the real binary.
	ok := rerun()
	if ok.IsError {
		t.Fatalf("rerun returned a tool error: %s", toolContentString(t, ok))
	}
	raw, err := json.Marshal(ok.StructuredContent)
	if err != nil {
		t.Fatalf("marshal rerun output: %v", err)
	}
	var out struct {
		Result struct {
			RunID            string `json:"run_id"`
			RerunOfSequence  int64  `json:"rerun_of_sequence"`
			NewRoundSequence int64  `json:"new_round_sequence"`
			ConfiguredAgents int    `json:"configured_agents"`
		} `json:"result"`
		NextAction *nextAction `json:"next_action"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode rerun output: %v\nraw: %s", err, raw)
	}
	if out.Result.RunID != parent.ID.String() || out.Result.RerunOfSequence != roundSeq || out.Result.NewRoundSequence <= roundSeq || out.Result.ConfiguredAgents != 1 {
		t.Fatalf("rerun result = %+v, want run %s re-running %d as a newer round with 1 reviewer", out.Result, parent.ID, roundSeq)
	}
	if out.NextAction == nil || out.NextAction.Action != "fishhawk_await_review" {
		t.Errorf("rerun next_action = %+v, want fishhawk_await_review", out.NextAction)
	}

	// Committed state: one request row BEFORE the new round, and the new
	// round's rerun_of names the old one.
	rows, err := auditRepo.ListForRunByCategory(ctx, parent.ID, rerunCat)
	if err != nil {
		t.Fatalf("list %s: %v", rerunCat, err)
	}
	if len(rows) != 1 || rows[0].Sequence >= out.Result.NewRoundSequence {
		t.Fatalf("%s rows = %d, want exactly 1 sequenced before the new round", rerunCat, len(rows))
	}
	started, err := auditRepo.ListForRunByCategory(ctx, parent.ID, "implement_review_started")
	if err != nil {
		t.Fatalf("list implement_review_started: %v", err)
	}
	var newRound *planreview.ReviewStartedPayload
	for _, e := range started {
		if e.Sequence == out.Result.NewRoundSequence {
			var p planreview.ReviewStartedPayload
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				t.Fatalf("decode new round: %v", err)
			}
			newRound = &p
		}
	}
	if newRound == nil || newRound.RerunOf != roundSeq || newRound.HeadSHA != headSHA {
		t.Fatalf("new round = %+v, want rerun_of %d at head %s", newRound, roundSeq, headSHA)
	}
	comparesMu.Lock()
	gotCompares := append([]string(nil), compares...)
	comparesMu.Unlock()
	if len(gotCompares) != 1 || !strings.HasSuffix(gotCompares[0], "/compare/"+baseSHA+"..."+headSHA) {
		t.Errorf("forge compares = %v, want one compare of %s...%s", gotCompares, baseSHA, headSHA)
	}

	// The advisory reviewer runs detached: wait for its verdict on the new
	// round.
	deadline := time.Now().Add(30 * time.Second)
	for countCategory("implement_reviewed", out.Result.NewRoundSequence) == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("no implement_reviewed landed on the re-run round %d (reviewer calls = %d)", out.Result.NewRoundSequence, reviewer.Calls())
		}
		time.Sleep(50 * time.Millisecond)
	}

	// 4. The hold CLEARS: merge_run passes the coverage refusal and stops at
	// the next guard, the unwired merge seam.
	res, status, _, _ = mergeRun()
	if !res.IsError {
		t.Fatalf("cleared merge_run = status %q, want the merge_seam_unconfigured tool error past the cleared hold", status)
	}
	if text := toolContentString(t, res); !strings.Contains(text, "merge_seam_unconfigured") || strings.Contains(text, "review_coverage_incomplete") {
		t.Errorf("cleared merge_run error = %s, want merge_seam_unconfigured and no review_coverage_incomplete", text)
	}
}
