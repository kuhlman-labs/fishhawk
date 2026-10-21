package mcpe2e_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kuhlman-labs/fishhawk/backend/internal/approval"
	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	runpkg "github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/scopeamendment"
	"github.com/kuhlman-labs/fishhawk/backend/internal/server"
	"github.com/kuhlman-labs/fishhawk/backend/internal/signing"
)

// E83.61 / #4119, the cross-boundary half of the merge overlap guard: one
// fishhawk_merge_run crosses every hop the per-layer units fake —
//
//  1. the real fishhawk-mcp binary over stdio — MCP input (allow_overlap /
//     overlap_reason) → the client.go mergeRunRequest wire body, and the 409
//     overlapping_implement_in_flight mapped to an IMMEDIATE status whose
//     next_action names fishhawk_await_stage on the blocking run's stage;
//  2. the server's POST /v0/runs/{id}/merge → mergeOverlapGuard: the #4118
//     candidate enumeration and the blocking run's effective scope read from
//     Postgres (a plan shipped through the real signed handleShipPlan path),
//     its implement stage state, and this PR's changed paths from the GitHub
//     stub's three-dot compare;
//  3. the audit chain in Postgres: no verdict on the refusal; on the override
//     the merge_overlap_overridden row (reason verbatim) lands BEFORE the
//     merge_verdict_recorded row; a re-invoke of the same judgement dedupes.
//
// A json-tag drift on either side of the override fields (the MCP client
// mirror vs the server's mergeRunRequest) drops the override → the second
// call refuses again → RED.

const (
	moRepo = "acme/mergeoverlap"
	moHead = "moheadsha0000000000000000000000000000001"
)

// moForge is the GitHub stub: PR 1 (run A) is open at moHead against main;
// the three-dot compare main...moHead lists A's changed files, and the
// merge-candidate gate's behind probe moHead...main lists no commits.
type moForge struct {
	mu           sync.Mutex
	changed      []string
	diffCompares int
}

func (g *moForge) client(t *testing.T) *githubclient.Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{repo}/pulls/{number}", func(w http.ResponseWriter, r *http.Request) {
		n, _ := strconv.Atoi(r.PathValue("number"))
		if n != 1 {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"node_id":"PR_1","state":"open","merged":false,"mergeable":true,"mergeable_state":"clean","head":{"sha":%q,"ref":"fishhawk/run-a"},"base":{"ref":"main"}}`, moHead)
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/compare/{basehead...}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.PathValue("basehead") != "main..."+moHead {
			_, _ = io.WriteString(w, `{"total_commits":0,"commits":[],"files":[]}`)
			return
		}
		g.mu.Lock()
		g.diffCompares++
		files := make([]string, 0, len(g.changed))
		for _, f := range g.changed {
			files = append(files, fmt.Sprintf(`{"filename":%q,"status":"modified","changes":1,"patch":"@@ -1 +1 @@"}`, f))
		}
		g.mu.Unlock()
		_, _ = fmt.Fprintf(w, `{"total_commits":1,"commits":[{"sha":%q}],"files":[%s]}`, moHead, strings.Join(files, ","))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &githubclient.Client{
		BaseURL: srv.URL,
		Tokens:  rcTokenProvider{},
		HTTP:    &http.Client{Timeout: 10 * time.Second},
		AppJWT:  func() (string, error) { return "ghs_jwt", nil },
	}
}

// moMergeView decodes the fishhawk_merge_run output the way an MCP consumer
// would, independently of the mcpserver mirror.
type moMergeView struct {
	Status          string `json:"status"`
	MergeQueued     bool   `json:"merge_queued"`
	VerdictRecorded bool   `json:"verdict_recorded"`
	AlreadyRecorded bool   `json:"already_recorded"`
	Message         string `json:"message"`
	NextAction      *struct {
		Action string            `json:"action"`
		Params map[string]string `json:"params"`
	} `json:"next_action"`
	Overlap *struct {
		HeadSHA  string `json:"head_sha"`
		Blocking []struct {
			RunID      string   `json:"run_id"`
			IssueRef   string   `json:"issue_ref"`
			StageID    string   `json:"stage_id"`
			StageType  string   `json:"stage_type"`
			StageState string   `json:"stage_state"`
			Paths      []string `json:"paths"`
		} `json:"blocking"`
		Undetermined []json.RawMessage `json:"undetermined"`
	} `json:"overlap"`
}

// moOverridePayload decodes a merge_overlap_overridden row.
type moOverridePayload struct {
	RunID            string `json:"run_id"`
	HeadSHA          string `json:"head_sha"`
	OverriddenCode   string `json:"overridden_code"`
	OverlapReason    string `json:"overlap_reason"`
	Fingerprint      string `json:"fingerprint"`
	OverriddenReason string `json:"overridden_reason"`
	Blocking         []struct {
		RunID      string   `json:"run_id"`
		StageID    string   `json:"stage_id"`
		StageState string   `json:"stage_state"`
		Paths      []string `json:"paths"`
	} `json:"blocking"`
}

// TestE2E_MergeRun_OverlapGuard: run A (merge-ready, PR 1 changing shared.go
// and a.go) and run B in the same repository, whose shipped plan scopes
// shared.go and whose implement stage is RUNNING.
//
//	(1) fishhawk_merge_run on A → status overlapping_implement_in_flight,
//	    next_action fishhawk_await_stage naming B's run and implement stage,
//	    overlap.blocking naming B on [shared.go] at moHead, zero
//	    merge_verdict_recorded rows, zero merge_overlap_overridden rows, the
//	    merger never called.
//	(2) again with allow_overlap + an overlap_reason carrying leading and
//	    trailing whitespace → not a tool error (merge dispatched; the bounded
//	    terminal await times out), exactly one merge_overlap_overridden row
//	    naming B's run/stage and shared.go with the reason BYTE-FOR-BYTE, its
//	    sequence LOWER than the (first) merge_verdict_recorded row's, and the
//	    row readable through fishhawk_list_audit with the category filter.
//	(3) the same override again → still one override row (fingerprint dedupe
//	    through the real chain), one verdict row (already_recorded).
func TestE2E_MergeRun_OverlapGuard(t *testing.T) {
	t.Parallel()
	fx := newFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	gh := &moForge{changed: []string{"a.go", "shared.go"}}
	auditRepo := audit.NewPostgresRepository(fx.pool)
	signingRepo := signing.NewPostgresRepository(fx.pool)
	merger := &fakeGitHubMerger{}
	srv := server.New(server.Config{
		Addr:               "127.0.0.1:0",
		RunRepo:            fx.runRepo,
		AuditRepo:          auditRepo,
		SigningRepo:        signingRepo,
		ArtifactRepo:       artifact.NewPostgresRepository(fx.pool),
		ApprovalRepo:       approval.NewPostgresRepository(fx.pool),
		ScopeAmendmentRepo: scopeamendment.NewPostgresRepository(fx.pool),
		APITokenRepo:       fx.apitokenRepo,
		GitHub:             gh.client(t),
		GateMerger:         merger,
	})
	httpSrv := httptest.NewServer(srv.Handler())
	t.Cleanup(httpSrv.Close)

	inst := int64(4119)
	newRun := func(n int) *runpkg.Run {
		t.Helper()
		ref := fmt.Sprintf("%s#%d", moRepo, n)
		r, err := fx.runRepo.CreateRun(ctx, runpkg.CreateRunParams{
			Repo: moRepo, WorkflowID: "feature_change", WorkflowSHA: "deadbeef", TriggerSource: runpkg.TriggerCLI,
			TriggerRef: &ref, InstallationID: &inst, WorkflowSpec: scopeOverlapWorkflowSpec,
		})
		if err != nil {
			t.Fatalf("CreateRun %d: %v", n, err)
		}
		if _, err := fx.runRepo.TransitionRun(ctx, r.ID, runpkg.StateRunning); err != nil {
			t.Fatalf("TransitionRun %d → running: %v", n, err)
		}
		return r
	}
	stage := func(r *runpkg.Run, seq int, typ runpkg.StageType) *runpkg.Stage {
		t.Helper()
		st, err := fx.runRepo.CreateStage(ctx, runpkg.CreateStageParams{
			RunID: r.ID, Sequence: seq, Type: typ, ExecutorKind: runpkg.ExecutorAgent, ExecutorRef: "fishhawk/runner@v1",
			RequiresApproval: typ == runpkg.StageTypePlan,
		})
		if err != nil {
			t.Fatalf("CreateStage(%s): %v", typ, err)
		}
		return st
	}

	// Run A: merge-ready — a PR, its implement succeeded.
	a := newRun(1)
	if _, err := fx.runRepo.SetRunPullRequestURL(ctx, a.ID, "https://github.com/"+moRepo+"/pull/1"); err != nil {
		t.Fatalf("SetRunPullRequestURL: %v", err)
	}
	for seq, typ := range []runpkg.StageType{runpkg.StageTypePlan, runpkg.StageTypeImplement} {
		st := stage(a, seq+1, typ)
		walkToRunning(t, ctx, fx, st.ID)
		if _, err := fx.runRepo.TransitionStage(ctx, st.ID, runpkg.StageStateSucceeded, nil); err != nil {
			t.Fatalf("A %s → succeeded: %v", typ, err)
		}
	}

	// Run B: a plan scoping shared.go shipped through the signed path, then
	// its implement stage walked to RUNNING.
	b := newRun(2)
	issued, err := signingRepo.Issue(ctx, b.ID, time.Hour)
	if err != nil {
		t.Fatalf("Issue signing key: %v", err)
	}
	bPlan := stage(b, 1, runpkg.StageTypePlan)
	walkToRunning(t, ctx, fx, bPlan.ID)
	resp := shipPlanSigned(t, ctx, httpSrv.URL, b.ID, bPlan.ID, issued.PrivateKey, regressionPlanJSON("plan for B", []string{"shared.go", "b.go"}))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("ship B's plan status = %d, want 201", resp.StatusCode)
	}
	bImpl := stage(b, 2, runpkg.StageTypeImplement)
	walkToRunning(t, ctx, fx, bImpl.ID)

	session := connectMCPClient(t, ctx, fx.mcpBinary, fx.operatorTok, httpSrv.URL)
	callMerge := func(args map[string]any) moMergeView {
		t.Helper()
		args["run_id"] = a.ID.String()
		args["verdict"] = "ship it"
		args["timeout_seconds"] = 1
		res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "fishhawk_merge_run", Arguments: args})
		if err != nil {
			t.Fatalf("CallTool fishhawk_merge_run: %v", err)
		}
		if res.IsError {
			t.Fatalf("fishhawk_merge_run returned a tool error: %s", toolContentString(t, res))
		}
		var out moMergeView
		decodeStructured(t, res, &out)
		return out
	}
	overrideRows := func() []*audit.Entry {
		t.Helper()
		rows, err := auditRepo.ListForRunByCategory(ctx, a.ID, server.CategoryMergeOverlapOverridden)
		if err != nil {
			t.Fatalf("ListForRunByCategory(merge_overlap_overridden): %v", err)
		}
		return rows
	}

	// (1) The refusal.
	out := callMerge(map[string]any{})
	if out.Status != "overlapping_implement_in_flight" {
		t.Fatalf("status = %q, want overlapping_implement_in_flight (message %q)", out.Status, out.Message)
	}
	if out.MergeQueued || out.VerdictRecorded || out.AlreadyRecorded {
		t.Errorf("flags = %+v, want all false", out)
	}
	if out.NextAction == nil || out.NextAction.Action != "fishhawk_await_stage" ||
		out.NextAction.Params["run_id"] != b.ID.String() || out.NextAction.Params["stage_id"] != bImpl.ID.String() {
		t.Fatalf("next_action = %+v, want fishhawk_await_stage on B %s stage %s", out.NextAction, b.ID, bImpl.ID)
	}
	if out.Overlap == nil || out.Overlap.HeadSHA != moHead || len(out.Overlap.Blocking) != 1 {
		t.Fatalf("overlap = %+v, want one blocker at %s", out.Overlap, moHead)
	}
	if bl := out.Overlap.Blocking[0]; bl.RunID != b.ID.String() || bl.StageID != bImpl.ID.String() || bl.StageState != "running" ||
		bl.StageType != "implement" || bl.IssueRef != moRepo+"#2" || len(bl.Paths) != 1 || bl.Paths[0] != "shared.go" {
		t.Errorf("blocker = %+v, want B's running implement on [shared.go]", bl)
	}
	if n := mergeVerdictRowCount(t, ctx, fx.pool, a.ID); n != 0 {
		t.Errorf("merge_verdict_recorded rows = %d after the refusal, want 0", n)
	}
	if n := len(overrideRows()); n != 0 {
		t.Errorf("merge_overlap_overridden rows = %d after the refusal, want 0", n)
	}
	if merger.calls() != 0 {
		t.Errorf("merger called %d times on the refusal, want 0", merger.calls())
	}
	gh.mu.Lock()
	compares := gh.diffCompares
	gh.mu.Unlock()
	if compares == 0 {
		t.Errorf("the guard never read the PR's changed paths (main...%s compare count 0)", moHead)
	}

	// (2) The override, reason verbatim with surrounding whitespace.
	const reason = "  B only renames a helper in shared.go; judged safe <b>  \n"
	out = callMerge(map[string]any{"allow_overlap": true, "overlap_reason": reason})
	if out.Status == "overlapping_implement_in_flight" || out.Status == "overlap_undetermined" {
		t.Fatalf("status = %q on the override, want the merge admitted (the override fields did not reach the server)", out.Status)
	}
	if !out.MergeQueued || !out.VerdictRecorded {
		t.Errorf("override merge = %+v, want merge_queued + verdict_recorded", out)
	}
	if merger.calls() != 1 {
		t.Errorf("merger called %d times after the override, want 1", merger.calls())
	}
	rows := overrideRows()
	if len(rows) != 1 {
		t.Fatalf("merge_overlap_overridden rows = %d, want 1", len(rows))
	}
	var p moOverridePayload
	if err := json.Unmarshal(rows[0].Payload, &p); err != nil {
		t.Fatalf("decode override payload: %v", err)
	}
	if p.OverlapReason != reason {
		t.Errorf("overlap_reason = %q, want %q byte-for-byte", p.OverlapReason, reason)
	}
	if p.RunID != a.ID.String() || p.HeadSHA != moHead || p.OverriddenCode != "overlapping_implement_in_flight" || p.Fingerprint == "" {
		t.Errorf("override payload = %+v, want run A at %s overriding overlapping_implement_in_flight with a fingerprint", p, moHead)
	}
	if len(p.Blocking) != 1 || p.Blocking[0].RunID != b.ID.String() || p.Blocking[0].StageID != bImpl.ID.String() ||
		len(p.Blocking[0].Paths) != 1 || p.Blocking[0].Paths[0] != "shared.go" {
		t.Errorf("override blocking = %+v, want B's implement stage on [shared.go]", p.Blocking)
	}
	verdicts, err := auditRepo.ListForRunByCategory(ctx, a.ID, server.CategoryMergeVerdictRecorded)
	if err != nil || len(verdicts) != 1 {
		t.Fatalf("merge_verdict_recorded rows = %d (%v), want 1", len(verdicts), err)
	}
	if rows[0].Sequence >= verdicts[0].Sequence {
		t.Errorf("override sequence %d is not before the verdict sequence %d (record-before-commit on the first POST)", rows[0].Sequence, verdicts[0].Sequence)
	}

	// The row is readable through fishhawk_list_audit with the category
	// filter: the category is registered (no allow_unknown needed).
	listRes, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "fishhawk_list_audit",
		Arguments: map[string]any{"run_id": a.ID.String(), "category": server.CategoryMergeOverlapOverridden}})
	if err != nil {
		t.Fatalf("CallTool fishhawk_list_audit: %v", err)
	}
	if listRes.IsError {
		t.Fatalf("fishhawk_list_audit(merge_overlap_overridden) returned a tool error: %s", toolContentString(t, listRes))
	}
	var listed struct {
		Items []struct {
			Category string `json:"category"`
			Sequence int64  `json:"sequence"`
		} `json:"items"`
	}
	decodeStructured(t, listRes, &listed)
	if len(listed.Items) != 1 || listed.Items[0].Sequence != rows[0].Sequence {
		t.Errorf("fishhawk_list_audit items = %+v, want the one override row at sequence %d", listed.Items, rows[0].Sequence)
	}

	// (3) The same judgement again: deduped, verdict already recorded.
	out = callMerge(map[string]any{"allow_overlap": true, "overlap_reason": reason})
	if !out.AlreadyRecorded {
		t.Errorf("re-invoke = %+v, want already_recorded", out)
	}
	if n := len(overrideRows()); n != 1 {
		t.Errorf("merge_overlap_overridden rows = %d after the identical re-invoke, want 1 (deduped)", n)
	}
	if n := mergeVerdictRowCount(t, ctx, fx.pool, a.ID); n != 1 {
		t.Errorf("merge_verdict_recorded rows = %d after the re-invoke, want 1", n)
	}
}
