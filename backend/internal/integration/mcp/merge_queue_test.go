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

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concurrency"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/mergequeue"
	runpkg "github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/server"
	"github.com/kuhlman-labs/fishhawk/backend/internal/signing"
)

// ADR-093 slice 1 (E87.4 / #4295), the cross-boundary half: one merge_run
// enqueue crosses every hop the per-layer units cannot exercise together —
//
//  1. the real fishhawk-mcp binary over stdio — fishhawk_merge_run maps the
//     202 queued answer to an IMMEDIATE status=queued;
//  2. the server's POST /v0/runs/{id}/merge — the ADR-090 gate refuses the
//     behind run as merge_base_behind, the merge-queue eligibility predicate
//     admits it (live forge approval by an approvals.members identity, no
//     acceptance stage declared, a green check run, no concern) and the enqueue
//     arm writes the entry with merge intent;
//  3. the REAL mergequeue.PostgresStore and audit chain on one pgtest pool;
//  4. the two reads back out — GET /v0/repos/{owner}/{name}/merge-queue via
//     fishhawk_get_merge_queue and GET /v0/runs/{id} via
//     fishhawk_get_run_status — so a json-tag drift between runs.go /
//     merge_queue_read.go / merge_run.go and the mcpserver client.go mirrors
//     fails HERE (each per-layer unit decodes its own literal).
//
// The forge is an httptest fake reached through githubclient.Client.BaseURL
// (approval condition C7), mirroring server/merge_candidate_queue_test.go's
// mcqGitHub mux: pulls, compare (the behind probe), merges, reviews,
// check-runs.

const (
	mqeRepo = "x/y"
	mqeBase = "main"
)

// mqeSpec declares an implement verify command (so the merge-candidate gate
// applies) and a review approval gate whose only member is github:alice.
var mqeSpec = []byte(`version: "1.0"
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
          verify:
            command: "scripts/test verify"
            timeout: "5m"
        produces:
          - artifact: pull_request
      - id: review
        type: review
        executor:
          human: true
        inputs:
          - artifact: pull_request
            from_stage: implement
        gates:
          - type: approval
            approvals:
              count: 1
              members: ["github:alice"]
`)

type mqePR struct{ head, branch string }

// mqeForge is the fake GitHub.
type mqeForge struct {
	mu      sync.Mutex
	prs     map[int]*mqePR
	behind  map[string][]string
	reviews map[int]string
}

type mqeTokens struct{}

func (mqeTokens) Token(context.Context, int64) (string, error) { return "ghs_mqe", nil }

func (g *mqeForge) client(t *testing.T) *githubclient.Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{repo}/pulls/{number}", func(w http.ResponseWriter, r *http.Request) {
		n, _ := strconv.Atoi(r.PathValue("number"))
		g.mu.Lock()
		pr, ok := g.prs[n]
		var body string
		if ok {
			body = fmt.Sprintf(`{"node_id":"PR_%d","state":"open","merged":false,"head":{"sha":%q,"ref":%q},"base":{"ref":%q}}`,
				n, pr.head, pr.branch, mqeBase)
		}
		g.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/compare/{basehead...}", func(w http.ResponseWriter, r *http.Request) {
		head, _, _ := strings.Cut(r.PathValue("basehead"), "...")
		g.mu.Lock()
		commits := append([]string(nil), g.behind[head]...)
		g.mu.Unlock()
		parts := make([]string, 0, len(commits))
		for _, c := range commits {
			parts = append(parts, fmt.Sprintf(`{"sha":%q}`, c))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"commits":[`+strings.Join(parts, ",")+`]}`)
	})
	// The line is held by a live blocker in this test, so no admission
	// auto-advances; a merge into a branch answers a conflict so an unexpected
	// advance is loud rather than silently moving a head.
	mux.HandleFunc("POST /repos/{owner}/{repo}/merges", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(w, `{"message":"Merge conflict"}`)
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/pulls/{number}/reviews", func(w http.ResponseWriter, r *http.Request) {
		n, _ := strconv.Atoi(r.PathValue("number"))
		g.mu.Lock()
		body := g.reviews[n]
		g.mu.Unlock()
		if body == "" {
			body = "[]"
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/commits/{ref}/check-runs", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"total_count":1,"check_runs":[{"id":1,"name":"ci","status":"completed","conclusion":"success",`+
			`"started_at":"2026-01-01T00:00:00Z","completed_at":"2026-01-01T00:01:00Z","check_suite":{"id":1},"app":{"slug":"github-actions"}}]}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &githubclient.Client{
		BaseURL: srv.URL,
		Tokens:  mqeTokens{},
		HTTP:    &http.Client{Timeout: 10 * time.Second},
		AppJWT:  func() (string, error) { return "ghs_jwt", nil },
	}
}

// mqeCall invokes one tool over the session and decodes its structured
// output into out, failing on a tool error.
func mqeCall(t *testing.T, ctx context.Context, session *mcp.ClientSession, name string, args map[string]any, out any) string {
	t.Helper()
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool %s: %v", name, err)
	}
	if res.IsError {
		t.Fatalf("%s returned a tool error: %s", name, toolContentString(t, res))
	}
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal %s output: %v", name, err)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		t.Fatalf("decode %s output: %v\nraw: %s", name, err, raw)
	}
	return string(raw)
}

// TestE2E_MergeQueue_MergeRunQueuesAndReadsSurviveEveryHop: a blocker run
// holds the base's line (live, verifying — seeded through the real store), a
// merged history entry gives one 90s line turn, and an approved, behind run
// is merged through fishhawk_merge_run. The run is QUEUED at position 1 with
// merge intent, the terminal await is never armed, and both reads report the
// same entry: fishhawk_get_merge_queue (head_of_line = the blocker with its
// PR's current head, the held entry at position 1 with merge_intent true and
// eta_seconds = 1 x 90) and fishhawk_get_run_status (merge_candidate_queue
// held at position 1 with merge_intent true). Counterfactual: a drifted
// merge_intent / position / eta_seconds tag on either side of any hop decodes
// to the zero value → RED.
func TestE2E_MergeQueue_MergeRunQueuesAndReadsSurviveEveryHop(t *testing.T) {
	t.Parallel()
	fx := newFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	forge := &mqeForge{prs: map[int]*mqePR{}, behind: map[string][]string{}, reviews: map[int]string{}}
	auditRepo := audit.NewPostgresRepository(fx.pool)
	store := mergequeue.NewPostgresStore(fx.pool)
	merger := &fakeGitHubMerger{}
	srv := server.New(server.Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      fx.runRepo,
		AuditRepo:    auditRepo,
		SigningRepo:  signing.NewPostgresRepository(fx.pool),
		APITokenRepo: fx.apitokenRepo,
		GitHub:       forge.client(t),
		GateMerger:   merger,
		MergeQueue:   store,
		Concurrency:  concurrency.NewPostgresStore(fx.pool),
		ConcernRepo:  concern.NewPostgresRepository(fx.pool),
	})
	httpSrv := httptest.NewServer(srv.Handler())
	t.Cleanup(httpSrv.Close)

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := fx.pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}
	inst := int64(4295)
	requiresCharter := false
	seed := func(n int, head string) *runpkg.Run {
		t.Helper()
		r, err := fx.runRepo.CreateRun(ctx, runpkg.CreateRunParams{
			Repo: mqeRepo, WorkflowID: "feature_change", WorkflowSHA: "deadbeef", TriggerSource: runpkg.TriggerCLI,
			InstallationID: &inst, WorkflowSpec: mqeSpec, RequiresCharter: &requiresCharter,
		})
		if err != nil {
			t.Fatalf("CreateRun: %v", err)
		}
		exec(`UPDATE runs SET state = 'running' WHERE id = $1`, r.ID)
		if _, err := fx.runRepo.SetRunPullRequestURL(ctx, r.ID, fmt.Sprintf("https://github.com/%s/pull/%d", mqeRepo, n)); err != nil {
			t.Fatalf("SetRunPullRequestURL: %v", err)
		}
		for i, st := range []struct {
			typ   runpkg.StageType
			state string
		}{{runpkg.StageTypePlan, "succeeded"}, {runpkg.StageTypeImplement, "succeeded"}, {runpkg.StageTypeReview, "awaiting_approval"}} {
			s, err := fx.runRepo.CreateStage(ctx, runpkg.CreateStageParams{
				RunID: r.ID, Sequence: i + 1, Type: st.typ, ExecutorKind: runpkg.ExecutorAgent, ExecutorRef: "claude-code",
			})
			if err != nil {
				t.Fatalf("CreateStage: %v", err)
			}
			exec(`UPDATE stages SET state = $1 WHERE id = $2`, st.state, s.ID)
		}
		forge.mu.Lock()
		forge.prs[n] = &mqePR{head: head, branch: fmt.Sprintf("fishhawk/run-%d", n)}
		forge.mu.Unlock()
		return r
	}

	// The blocker holds the line: enqueued and admitted through the real
	// store, phase verifying, its PR moved past the anchor.
	blocker := seed(1, "blk-moved")
	if _, err := store.Enqueue(ctx, mergequeue.EnqueueRequest{RunID: blocker.ID, BaseRef: mqeBase, HeadSHA: "blk-anchor"}); err != nil {
		t.Fatalf("Enqueue blocker: %v", err)
	}
	adm, err := store.AdmitNext(ctx, mergequeue.Base{Repo: mqeRepo, BaseRef: mqeBase})
	if err != nil || !adm.Admitted || adm.Entry == nil || adm.Entry.RunID != blocker.ID {
		t.Fatalf("AdmitNext = %+v, %v; want the blocker admitted", adm, err)
	}
	if _, err := store.SetPhase(ctx, adm.Entry.ID, adm.Entry.AdmissionEpoch, mergequeue.PhaseVerifying); err != nil {
		t.Fatalf("SetPhase: %v", err)
	}

	// One merged line turn of exactly 90s on the base, BY CONSTRUCTION.
	history := seed(3, "h0")
	exec(`INSERT INTO merge_candidate_queue_entries
  (run_id, repo, base_ref, state, enqueued_at, anchored_head_sha, admitted_at, settled_at)
SELECT $1, $2, $3, 'merged', c.ts - interval '2 hours', 'h0', c.ts - interval '1 hour' - interval '90 seconds', c.ts - interval '1 hour'
FROM (SELECT clock_timestamp() AS ts) c`, history.ID, mqeRepo, mqeBase)

	// The merged run: behind the base, approved on the forge by alice.
	queued := seed(2, "q0")
	forge.mu.Lock()
	forge.behind["q0"] = []string{"base-1"}
	forge.reviews[2] = `[{"id":1,"user":{"login":"alice"},"state":"APPROVED","submitted_at":"2026-01-01T00:00:00Z"}]`
	forge.mu.Unlock()

	session := connectMCPClient(t, ctx, fx.mcpBinary, fx.operatorTok, httpSrv.URL)

	var merged struct {
		Status          string `json:"status"`
		MergeQueued     bool   `json:"merge_queued"`
		VerdictRecorded bool   `json:"verdict_recorded"`
		QueueState      string `json:"queue_state"`
		QueuePosition   int    `json:"queue_position"`
		LiveRunID       string `json:"live_run_id"`
		EntryID         string `json:"entry_id"`
		BaseRef         string `json:"base_ref"`
		AnchoredHeadSHA string `json:"anchored_head_sha"`
		IntentSequence  int64  `json:"intent_sequence"`
		NextAction      *struct {
			Action string            `json:"action"`
			Params map[string]string `json:"params"`
		} `json:"next_action"`
	}
	start := time.Now()
	raw := mqeCall(t, ctx, session, "fishhawk_merge_run",
		map[string]any{"run_id": queued.ID.String(), "verdict": "ship it", "timeout_seconds": 60}, &merged)
	if merged.Status != "queued" {
		t.Fatalf("fishhawk_merge_run status = %q, want queued\nraw: %s", merged.Status, raw)
	}
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Errorf("fishhawk_merge_run took %s, want an immediate return (the terminal await is never armed)", elapsed)
	}
	if merged.MergeQueued || merged.VerdictRecorded || merged.QueueState != "held" || merged.QueuePosition != 1 ||
		merged.LiveRunID != blocker.ID.String() || merged.BaseRef != mqeBase || merged.AnchoredHeadSHA != "q0" ||
		merged.EntryID == "" || merged.IntentSequence == 0 {
		t.Fatalf("fishhawk_merge_run = %+v, want held position 1 behind %s anchored q0 with an intent sequence\nraw: %s", merged, blocker.ID, raw)
	}
	if merged.NextAction == nil || merged.NextAction.Action != "fishhawk_get_merge_queue" ||
		merged.NextAction.Params["repo"] != mqeRepo || merged.NextAction.Params["base_ref"] != mqeBase {
		t.Errorf("next_action = %+v, want fishhawk_get_merge_queue on %s %s", merged.NextAction, mqeRepo, mqeBase)
	}
	if merger.calls() != 0 {
		t.Errorf("merger called %d times, want 0 (queued, not merged)", merger.calls())
	}

	// Committed state: one merge_queue_entered row at the reported sequence,
	// no merge_verdict_recorded row, one entry with merge intent.
	entered, err := auditRepo.ListForRunByCategory(ctx, queued.ID, server.CategoryMergeQueueEntered)
	if err != nil || len(entered) != 1 || entered[0].Sequence != merged.IntentSequence {
		t.Fatalf("merge_queue_entered rows = %d (%v), want 1 at sequence %d", len(entered), err, merged.IntentSequence)
	}
	if n := mergeVerdictRowCount(t, ctx, fx.pool, queued.ID); n != 0 {
		t.Errorf("merge_verdict_recorded rows = %d, want 0 on the queued path", n)
	}
	st, found, err := store.ActiveForRun(ctx, queued.ID)
	if err != nil || !found || st.Entry.MergeIntentAt == nil || st.Entry.ID.String() != merged.EntryID {
		t.Fatalf("store entry = %+v found=%v err=%v, want entry %s with merge_intent_at set", st.Entry, found, err, merged.EntryID)
	}

	// Read 1: the per-base merge-queue read through fishhawk_get_merge_queue.
	type mqeEntry struct {
		RunID        string `json:"run_id"`
		EntryID      string `json:"entry_id"`
		State        string `json:"state"`
		Phase        string `json:"phase"`
		Position     *int   `json:"position"`
		MergeIntent  *bool  `json:"merge_intent"`
		ETASeconds   *int64 `json:"eta_seconds"`
		AnchoredHead string `json:"anchored_head_sha"`
	}
	var q struct {
		Repo  string `json:"repo"`
		Bases []struct {
			BaseRef    string     `json:"base_ref"`
			Entries    []mqeEntry `json:"entries"`
			HeadOfLine *struct {
				RunID           string `json:"run_id"`
				State           string `json:"state"`
				Phase           string `json:"phase"`
				AnchoredHeadSHA string `json:"anchored_head_sha"`
				CurrentHeadSHA  string `json:"current_head_sha"`
			} `json:"head_of_line"`
			ETABasis struct {
				Samples       int    `json:"samples"`
				MedianSeconds *int64 `json:"median_seconds"`
				Note          string `json:"note"`
			} `json:"eta_basis"`
		} `json:"bases"`
	}
	raw = mqeCall(t, ctx, session, "fishhawk_get_merge_queue", map[string]any{"repo": mqeRepo, "base_ref": mqeBase}, &q)
	if q.Repo != mqeRepo || len(q.Bases) != 1 || q.Bases[0].BaseRef != mqeBase || len(q.Bases[0].Entries) != 2 {
		t.Fatalf("fishhawk_get_merge_queue = %+v, want one %s base with two entries\nraw: %s", q, mqeBase, raw)
	}
	b := q.Bases[0]
	live, held := b.Entries[0], b.Entries[1]
	if live.RunID != blocker.ID.String() || live.State != "live" || live.Phase != "verifying" || live.Position != nil ||
		live.ETASeconds != nil || live.MergeIntent == nil || *live.MergeIntent {
		t.Errorf("live entry = %+v, want the blocker live/verifying, no position, no eta, merge_intent false ON THE WIRE\nraw: %s", live, raw)
	}
	if held.RunID != queued.ID.String() || held.EntryID != merged.EntryID || held.State != "held" || held.Position == nil || *held.Position != 1 ||
		held.MergeIntent == nil || !*held.MergeIntent || held.ETASeconds == nil || *held.ETASeconds != 90 || held.AnchoredHead != "q0" {
		t.Errorf("held entry = %+v, want the queued run at position 1 with merge_intent true and eta 90\nraw: %s", held, raw)
	}
	if hol := b.HeadOfLine; hol == nil || hol.RunID != blocker.ID.String() || hol.State != "live" || hol.Phase != "verifying" ||
		hol.AnchoredHeadSHA != "blk-anchor" || hol.CurrentHeadSHA != "blk-moved" {
		t.Errorf("head_of_line = %+v, want the blocker anchored blk-anchor with current head blk-moved\nraw: %s", b.HeadOfLine, raw)
	}
	if b.ETABasis.Samples != 1 || b.ETABasis.MedianSeconds == nil || *b.ETABasis.MedianSeconds != 90 || b.ETABasis.Note == "" {
		t.Errorf("eta_basis = %+v, want one 90s sample and the basis note\nraw: %s", b.ETABasis, raw)
	}

	// Read 2: the run's own merge_candidate_queue block through
	// fishhawk_get_run_status.
	var status struct {
		Run struct {
			MergeCandidateQueue *struct {
				State         string     `json:"state"`
				Position      int        `json:"position"`
				LiveRunID     string     `json:"live_run_id"`
				MergeIntent   *bool      `json:"merge_intent"`
				MergeIntentAt *time.Time `json:"merge_intent_at"`
			} `json:"merge_candidate_queue"`
		} `json:"run"`
	}
	raw = mqeCall(t, ctx, session, "fishhawk_get_run_status", map[string]any{"run_id": queued.ID.String()}, &status)
	mcq := status.Run.MergeCandidateQueue
	if mcq == nil || mcq.State != "held" || mcq.Position != 1 || mcq.LiveRunID != blocker.ID.String() ||
		mcq.MergeIntent == nil || !*mcq.MergeIntent || mcq.MergeIntentAt == nil {
		t.Fatalf("run.merge_candidate_queue = %+v, want held position 1 behind %s with merge_intent true and merge_intent_at\nraw: %s", mcq, blocker.ID, raw)
	}
}
