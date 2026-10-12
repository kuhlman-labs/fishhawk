package server

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

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/basedelta"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concurrency"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/mergequeue"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/timescale"
)

// The ADR-092 (#4200) serializer, end to end: the REAL rebase handler, the
// server glue, the REAL mergequeue.PostgresStore, the REAL Postgres run and
// audit repositories — all over ONE pgtest pool (approval condition 1) — and
// a multi-PR fake GitHub. Every verdict is read back from committed state
// (queue rows, stage rows, audit rows), never from a response alone.

const mcqBase = "main"

// mcqPR is one fake pull request.
type mcqPR struct {
	head, branch, state string
	merged              bool
}

// mcqGitHub serves several PRs: GET /pulls/{n} answers each PR's live state;
// the behind-probe (compare/{head}...{base}) answers behind[head]; POST
// /merges merges into the named branch, moving that PR's head to
// mergeSHA[branch] (or answers mergeStatus[branch], e.g. 409 conflict).
type mcqGitHub struct {
	mu            sync.Mutex
	prs           map[int]*mcqPR
	behind        map[string][]string
	compareStatus map[string]int
	mergeSHA      map[string]string
	mergeStatus   map[string]int
	merges        []rebaseMergeCall
	// onMerge, when set, runs on every POST /merges (outside the lock)
	// before the response is written.
	onMerge func(base string)
	// mergeFn, when set, runs FIRST on every POST /merges (outside the lock)
	// and, when it returns a sha, makes that the merge commit for the base —
	// the e2e transfer tests create a real `git merge --no-ff` commit here.
	mergeFn func(base string) string
}

func (g *mcqGitHub) client(t *testing.T) *githubclient.Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{repo}/pulls/{number}", func(w http.ResponseWriter, r *http.Request) {
		n, _ := strconv.Atoi(r.PathValue("number"))
		g.mu.Lock()
		pr, ok := g.prs[n]
		var body string
		if ok {
			body = fmt.Sprintf(`{"node_id":"PR_%d","state":%q,"merged":%t,"head":{"sha":%q,"ref":%q},"base":{"ref":%q}}`,
				n, pr.state, pr.merged, pr.head, pr.branch, mcqBase)
		}
		g.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/compare/{basehead...}", func(w http.ResponseWriter, r *http.Request) {
		head, _, _ := strings.Cut(r.PathValue("basehead"), "...")
		g.mu.Lock()
		status := g.compareStatus[head]
		commits := append([]string(nil), g.behind[head]...)
		g.mu.Unlock()
		if status != 0 {
			w.WriteHeader(status)
			return
		}
		parts := make([]string, 0, len(commits))
		for _, c := range commits {
			parts = append(parts, fmt.Sprintf(`{"sha":%q}`, c))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"commits":[` + strings.Join(parts, ",") + `]}`))
	})
	mux.HandleFunc("POST /repos/{owner}/{repo}/merges", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var call rebaseMergeCall
		_ = json.Unmarshal(raw, &call)
		g.mu.Lock()
		mergeFn := g.mergeFn
		g.mu.Unlock()
		if mergeFn != nil {
			if sha := mergeFn(call.Base); sha != "" {
				g.set(func() { g.mergeSHA[call.Base] = sha })
			}
		}
		g.mu.Lock()
		g.merges = append(g.merges, call)
		status := g.mergeStatus[call.Base]
		sha := g.mergeSHA[call.Base]
		if status == 0 {
			for _, pr := range g.prs {
				if pr.branch == call.Base {
					pr.head = sha
				}
			}
		}
		onMerge := g.onMerge
		g.mu.Unlock()
		if onMerge != nil {
			onMerge(call.Base)
		}
		if status != 0 {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"message":"Merge conflict"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"sha":"` + sha + `"}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &githubclient.Client{
		BaseURL: srv.URL,
		Tokens:  &fakeTokenProvider{tok: "ghs_t"},
		HTTP:    &http.Client{Timeout: 5 * time.Second},
		AppJWT:  func() (string, error) { return "ghs_jwt", nil },
	}
}

func (g *mcqGitHub) set(fn func()) {
	g.mu.Lock()
	defer g.mu.Unlock()
	fn()
}

func (g *mcqGitHub) mergeCalls() []rebaseMergeCall {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]rebaseMergeCall(nil), g.merges...)
}

// mcqFixture is the shared-pool world.
type mcqFixture struct {
	t     *testing.T
	pool  *pgxpool.Pool
	repo  run.Repository
	audit audit.Repository
	store *mergequeue.PostgresStore
	gh    *mcqGitHub
	srv   *Server
	nextN int
}

// mcqRun is one seeded run with its PR.
type mcqRun struct {
	id     uuid.UUID
	pr     int
	prURL  string
	branch string
	impl   *run.Stage
	review *run.Stage
}

func newMCQFixture(t *testing.T) *mcqFixture {
	t.Helper()
	pool := pgtest.NewPool(t)
	f := &mcqFixture{
		t: t, pool: pool,
		repo:  run.NewPostgresRepository(pool),
		audit: audit.NewPostgresRepository(pool),
		store: mergequeue.NewPostgresStore(pool),
		gh: &mcqGitHub{prs: map[int]*mcqPR{}, behind: map[string][]string{},
			compareStatus: map[string]int{}, mergeSHA: map[string]string{}, mergeStatus: map[string]int{}},
	}
	f.srv = New(Config{
		Addr:        "127.0.0.1:0",
		RunRepo:     f.repo,
		AuditRepo:   f.audit,
		GitHub:      f.gh.client(t),
		MergeQueue:  f.store,
		Concurrency: concurrency.NewPostgresStore(pool),
	})
	f.srv.postMergeHeadReadBackoff = testPostMergeHeadReadBackoff()
	return f
}

func (f *mcqFixture) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.pool.Exec(context.Background(), sql, args...); err != nil {
		f.t.Fatalf("exec %q: %v", sql, err)
	}
}

func (f *mcqFixture) setStage(id uuid.UUID, st run.StageState) {
	f.exec(`UPDATE stages SET state = $1 WHERE id = $2`, string(st), id)
}

// seed creates a running feature_change run with plan succeeded, implement
// at succeeded and the review stage parked at awaiting_approval, and an open
// PR at head on its own branch.
func (f *mcqFixture) seed(name, head string, spec []byte) *mcqRun {
	f.t.Helper()
	ctx := context.Background()
	f.nextN++
	n := f.nextN
	inst := int64(99)
	r, err := f.repo.CreateRun(ctx, run.CreateRunParams{
		Repo: "x/y", WorkflowID: "feature_change", WorkflowSHA: "deadbeef", TriggerSource: run.TriggerCLI,
		InstallationID: &inst, WorkflowSpec: spec,
	})
	if err != nil {
		f.t.Fatalf("create run: %v", err)
	}
	f.exec(`UPDATE runs SET state = 'running' WHERE id = $1`, r.ID)
	prURL := fmt.Sprintf("https://github.com/x/y/pull/%d", n)
	if _, err := f.repo.SetRunPullRequestURL(ctx, r.ID, prURL); err != nil {
		f.t.Fatalf("set pr url: %v", err)
	}
	mk := func(seq int, typ run.StageType, st run.StageState) *run.Stage {
		s, err := f.repo.CreateStage(ctx, run.CreateStageParams{
			RunID: r.ID, Sequence: seq, Type: typ, ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code",
		})
		if err != nil {
			f.t.Fatalf("create stage: %v", err)
		}
		f.setStage(s.ID, st)
		return s
	}
	mk(1, run.StageTypePlan, run.StageStateSucceeded)
	impl := mk(2, run.StageTypeImplement, run.StageStateSucceeded)
	review := mk(3, run.StageTypeReview, run.StageStateAwaitingApproval)
	branch := "fishhawk/run-" + name
	f.gh.set(func() { f.gh.prs[n] = &mcqPR{head: head, branch: branch, state: "open"} })
	return &mcqRun{id: r.ID, pr: n, prURL: prURL, branch: branch, impl: impl, review: review}
}

// behind makes head behind the base and the next merge into r's branch
// produce mergeSHA.
func (f *mcqFixture) behind(r *mcqRun, head, mergeSHA string) {
	f.gh.set(func() {
		f.gh.behind[head] = []string{"base-" + mergeSHA}
		f.gh.mergeSHA[r.branch] = mergeSHA
	})
}

func (f *mcqFixture) rebase(r *mcqRun) rebaseBranchResponse {
	f.t.Helper()
	w := postRebaseBranch(f.t, f.srv, r.id, rebaseBranchRequest{Confirm: true, Reason: "base moved"}, withRebaseOperator)
	if w.Code != http.StatusOK {
		f.t.Fatalf("rebase %s = %d:\n%s", r.id, w.Code, w.Body.String())
	}
	return decodeRebase(f.t, w)
}

func (f *mcqFixture) entries(r *mcqRun) []mergequeue.Entry {
	f.t.Helper()
	rows, err := f.pool.Query(context.Background(),
		`SELECT id, state, COALESCE(phase,''), COALESCE(eject_reason,''), enqueued_at, anchored_head_sha
		   FROM merge_candidate_queue_entries WHERE run_id = $1 ORDER BY enqueued_at, id`, r.id)
	if err != nil {
		f.t.Fatalf("query entries: %v", err)
	}
	defer rows.Close()
	var out []mergequeue.Entry
	for rows.Next() {
		var e mergequeue.Entry
		var st, ph string
		if err := rows.Scan(&e.ID, &st, &ph, &e.EjectReason, &e.EnqueuedAt, &e.AnchoredHeadSHA); err != nil {
			f.t.Fatalf("scan: %v", err)
		}
		e.State, e.Phase, e.RunID = mergequeue.State(st), mergequeue.Phase(ph), r.id
		out = append(out, e)
	}
	return out
}

// entry returns the run's single newest entry.
func (f *mcqFixture) entry(r *mcqRun) mergequeue.Entry {
	f.t.Helper()
	es := f.entries(r)
	if len(es) == 0 {
		f.t.Fatalf("run %s has no queue entry", r.id)
	}
	return es[len(es)-1]
}

func (f *mcqFixture) rows(r *mcqRun, category string) []*audit.Entry {
	f.t.Helper()
	es, err := f.audit.ListForRunByCategory(context.Background(), r.id, category)
	if err != nil {
		f.t.Fatalf("list %s: %v", category, err)
	}
	return es
}

func (f *mcqFixture) triggers(r *mcqRun) []mergeCandidateVerifyTrigger {
	f.t.Helper()
	var out []mergeCandidateVerifyTrigger
	for _, e := range f.rows(r, CategoryStageMergeCandidateVerifyTriggered) {
		var p mergeCandidateVerifyTrigger
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			f.t.Fatalf("decode trigger: %v", err)
		}
		out = append(out, p)
	}
	return out
}

func (f *mcqFixture) stageState(id uuid.UUID) run.StageState {
	f.t.Helper()
	st, err := f.repo.GetStage(context.Background(), id)
	if err != nil {
		f.t.Fatalf("get stage: %v", err)
	}
	return st.State
}

func (f *mcqFixture) verdict(r *mcqRun, result string) {
	f.t.Helper()
	if _, err := f.srv.recordMergeCandidateVerified(context.Background(), r.id, r.impl.ID, result, "", "",
		audit.ActorSystem, "runner"); err != nil {
		f.t.Fatalf("record %s verdict: %v", result, err)
	}
}

// closePR delivers the pull_request.closed webhook (merged or not), moving the
// fake PR to the same state first.
func (f *mcqFixture) closePR(r *mcqRun, merged bool) {
	f.t.Helper()
	f.closePRCtx(context.Background(), r, merged)
}

// closePRCtx is closePR on the webhook request context ctx.
func (f *mcqFixture) closePRCtx(ctx context.Context, r *mcqRun, merged bool) {
	f.t.Helper()
	f.gh.set(func() { f.gh.prs[r.pr].state, f.gh.prs[r.pr].merged = "closed", merged })
	raw, _ := json.Marshal(map[string]any{
		"pull_request": map[string]any{
			"html_url": r.prURL, "merged": merged,
			"merged_by": map[string]any{"login": "alice"},
			"head":      map[string]any{"sha": "h"}, "base": map[string]any{"sha": "b"},
		},
		"sender": map[string]any{"login": "alice"},
	})
	f.srv.handlePullRequestClosed(ctx, raw)
}

func (f *mcqFixture) requireState(r *mcqRun, state mergequeue.State, reason string) mergequeue.Entry {
	f.t.Helper()
	e := f.entry(r)
	if e.State != state || e.EjectReason != reason {
		f.t.Fatalf("run %s entry = %s/%q, want %s/%q", r.id, e.State, e.EjectReason, state, reason)
	}
	return e
}

// wave seeds A admitted (live, verifying) and B held behind it.
func (f *mcqFixture) wave() (a, b *mcqRun) {
	f.t.Helper()
	spec := []byte(mcvUndelegatedSpecYAML)
	a = f.seed("a", "a0", spec)
	b = f.seed("b", "b0", spec)
	f.behind(a, "a0", "a1")
	f.behind(b, "b0", "b1")
	if resp := f.rebase(a); resp.MergeCandidateVerifyState != mergeCandidateStateInFlight || !resp.MergeCandidateVerifyTriggered {
		f.t.Fatalf("A rebase verify state = %q triggered=%v, want in_flight triggered", resp.MergeCandidateVerifyState, resp.MergeCandidateVerifyTriggered)
	}
	if resp := f.rebase(b); resp.MergeCandidateVerifyState != mergeCandidateStateHeld || resp.MergeCandidateVerifyTriggered {
		f.t.Fatalf("B rebase verify state = %q triggered=%v, want held, not triggered", resp.MergeCandidateVerifyState, resp.MergeCandidateVerifyTriggered)
	}
	return a, b
}

func TestMergeCandidateQueue_EndToEndWave(t *testing.T) {
	f := newMCQFixture(t)
	a, b := f.wave()

	// A is live and its pass started: trigger appended, implement re-opened.
	if e := f.requireState(a, mergequeue.StateLive, ""); e.Phase != mergequeue.PhaseVerifying || e.AnchoredHeadSHA != "a1" {
		t.Fatalf("A entry = phase %q head %q, want verifying at a1", e.Phase, e.AnchoredHeadSHA)
	}
	if tr := f.triggers(a); len(tr) != 1 || tr[0].ExpectedHeadSHA != "a1" {
		t.Fatalf("A triggers = %+v, want one for a1", tr)
	}
	if st := f.stageState(a.impl.ID); st != run.StageStatePending {
		t.Fatalf("A implement = %s, want pending (re-opened)", st)
	}
	// B is HELD: no trigger, implement untouched, a held row.
	heldB := f.requireState(b, mergequeue.StateHeld, "")
	if n := len(f.triggers(b)); n != 0 {
		t.Fatalf("B has %d triggers while A holds the line, want 0", n)
	}
	if st := f.stageState(b.impl.ID); st != run.StageStateSucceeded {
		t.Fatalf("B implement = %s, want succeeded (untouched)", st)
	}
	if n := len(f.rows(b, CategoryMergeCandidateQueueHeld)); n != 1 {
		t.Fatalf("B held rows = %d, want 1", n)
	}
	if st, _ := f.srv.mergeCandidateQueueForRun(context.Background(), b.id); st == nil || st.Position != 1 ||
		st.LiveRunID == nil || *st.LiveRunID != a.id {
		t.Fatalf("B queue status = %+v, want position 1 behind A", st)
	}

	// A second rebase on B re-anchors in place: same enqueued_at, new head.
	f.behind(b, "b1", "b2")
	if resp := f.rebase(b); resp.MergeCandidateVerifyState != mergeCandidateStateHeld {
		t.Fatalf("B second rebase state = %q, want held", resp.MergeCandidateVerifyState)
	}
	re := f.requireState(b, mergequeue.StateHeld, "")
	if !re.EnqueuedAt.Equal(heldB.EnqueuedAt) || re.AnchoredHeadSHA != "b2" || re.ID != heldB.ID {
		t.Fatalf("B re-anchor = %+v, want same row/enqueued_at at b2 (was %+v)", re, heldB)
	}
	if n := len(f.rows(b, CategoryMergeCandidateQueueReanchored)); n != 1 {
		t.Fatalf("B reanchored rows = %d, want 1", n)
	}

	// A's pass passes: it holds the line until merge (D4).
	f.verdict(a, mergeCandidateResultPassed)
	if e := f.requireState(a, mergequeue.StateLive, ""); e.Phase != mergequeue.PhasePassed {
		t.Fatalf("A phase after pass = %q, want passed", e.Phase)
	}
	f.requireState(b, mergequeue.StateHeld, "")

	// A merges: the base moved again, so B's admission auto-advances it.
	f.behind(b, "b2", "b3")
	f.closePR(a, true)
	f.requireState(a, mergequeue.StateMerged, "pr_merged")
	if e := f.requireState(b, mergequeue.StateLive, ""); e.AnchoredHeadSHA != "b3" || e.Phase != mergequeue.PhaseVerifying {
		t.Fatalf("B after hand-off = head %q phase %q, want b3 verifying", e.AnchoredHeadSHA, e.Phase)
	}
	calls := f.gh.mergeCalls()
	last := calls[len(calls)-1]
	if last.Base != b.branch || last.Head != mcqBase {
		t.Fatalf("auto-advance merge = base %q head %q, want base %q head %q", last.Base, last.Head, b.branch, mcqBase)
	}
	rebased := f.rows(b, CategoryBranchRebased)
	if lastRow := rebased[len(rebased)-1]; lastRow.ActorKind == nil || *lastRow.ActorKind != audit.ActorSystem {
		t.Fatalf("auto-advance branch_rebased actor = %v, want system", lastRow.ActorKind)
	}
	tr := f.triggers(b)
	if len(tr) != 1 || tr[0].ExpectedHeadSHA != "b3" || tr[0].Cause != mergeCandidateCauseBaseAdvance {
		t.Fatalf("B triggers = %+v, want one base_advance trigger for b3", tr)
	}
	for _, c := range []string{CategoryMergeCandidateQueueAdmitted, CategoryMergeCandidateQueueSettled} {
		if len(f.rows(a, c))+len(f.rows(b, c)) == 0 {
			t.Fatalf("no %s row written across the wave", c)
		}
	}

	// B's pass is admitted through local-verify:<host>, never local-implement.
	f.setStage(b.impl.ID, run.StageStateAwaitingHostDispatch)
	sc := &scPG{t: t, pool: f.pool, repo: f.repo, audit: f.audit, srv: f.srv}
	resp := sc.admitted(&run.Stage{ID: b.impl.ID, RunID: b.id}, hostBody("h1"))
	if resp.Concurrency == nil || resp.Concurrency.Group != "local-verify:h1" {
		t.Fatalf("B pass admission group = %+v, want local-verify:h1", resp.Concurrency)
	}
}

// TestMergeCandidateQueue_PendingPassTriggerReplaced: a rebase on an admitted
// PR whose pass is still pending REPLACES the trigger rather than refusing
// "stage is in state pending" (D2).
func TestMergeCandidateQueue_PendingPassTriggerReplaced(t *testing.T) {
	f := newMCQFixture(t)
	a, _ := f.wave()
	f.behind(a, "a1", "a2")
	resp := f.rebase(a)
	if resp.MergeCandidateVerifyRefusal != "" || resp.MergeCandidateVerifyState != mergeCandidateStateInFlight {
		t.Fatalf("A re-rebase = state %q refusal %q, want in_flight with no refusal", resp.MergeCandidateVerifyState, resp.MergeCandidateVerifyRefusal)
	}
	tr := f.triggers(a)
	if len(tr) != 2 || tr[1].ExpectedHeadSHA != "a2" || tr[1].PriorState != tr[0].PriorState || tr[1].PriorState == "" {
		t.Fatalf("A triggers = %+v, want a replacement for a2 carrying PriorState %q", tr, tr[0].PriorState)
	}
	if st := f.stageState(a.impl.ID); st != run.StageStatePending {
		t.Fatalf("A implement = %s, want still pending (no second re-open)", st)
	}
	if e := f.entry(a); e.AnchoredHeadSHA != "a2" || e.State != mergequeue.StateLive {
		t.Fatalf("A entry = %+v, want live at a2", e)
	}
}

// TestMergeCandidateQueue_HandOffWhileReviewHeld: approval condition 3 — the
// merge hands the line on even when the ADR-036 implement-review gate holds
// the review stage (checkImplementReviewSettled false).
func TestMergeCandidateQueue_HandOffWhileReviewHeld(t *testing.T) {
	f := newMCQFixture(t)
	spec := []byte(mcvUndelegatedSpecYAML)
	gated := []byte(strings.Replace(mcvUndelegatedSpecYAML,
		"        produces:\n          - artifact: pull_request\n",
		"        reviewers:\n          agent: 1\n          human: 0\n        produces:\n          - artifact: pull_request\n", 1))
	a := f.seed("a", "a0", gated)
	b := f.seed("b", "b0", spec)
	f.behind(a, "a0", "a1")
	f.behind(b, "b0", "b1")
	f.rebase(a)
	f.rebase(b)
	f.requireState(b, mergequeue.StateHeld, "")
	kind, subj := audit.ActorSystem, "reviewer"
	if _, err := f.audit.AppendChained(context.Background(), audit.ChainAppendParams{
		RunID: a.id, Timestamp: time.Now().UTC(), Category: "implement_review_started",
		ActorKind: &kind, ActorSubject: &subj, Payload: []byte(`{}`),
	}); err != nil {
		t.Fatalf("seed implement_review_started: %v", err)
	}
	review, _ := f.repo.GetRun(context.Background(), a.id)
	if f.srv.checkImplementReviewSettled(context.Background(), review, a.review) {
		t.Fatal("fixture: implement review must be unsettled")
	}
	f.closePR(a, true)
	// The ADR-036 gate HELD the resolution: it defers the pr_merged row.
	if n := len(f.rows(a, CategoryPRMerged)); n != 0 {
		t.Fatalf("A pr_merged rows = %d, want 0 (the implement-review gate must hold the resolution)", n)
	}
	f.requireState(a, mergequeue.StateMerged, "pr_merged")
	f.requireState(b, mergequeue.StateLive, "")
	if n := len(f.triggers(b)); n != 1 {
		t.Fatalf("B triggers = %d, want 1 after the hand-off", n)
	}
}

// TestMergeCandidateQueue_HandOffSurvivesWebhookCancellation: the webhook
// sender disconnects (its request context is cancelled) while the hand-off's
// admission of B is mid auto-advance. The hand-off is detached from that
// cancellation, so the re-anchor and the pass start still land: B ends live
// at the advanced head with exactly one trigger, not stranded in phase
// admitted until the stale-admission bound.
func TestMergeCandidateQueue_HandOffSurvivesWebhookCancellation(t *testing.T) {
	f := newMCQFixture(t)
	a, b := f.wave()
	f.behind(b, "b1", "b2")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.gh.set(func() {
		f.gh.onMerge = func(base string) {
			if base == b.branch {
				cancel()
			}
		}
	})
	f.closePRCtx(ctx, a, true)
	if ctx.Err() == nil {
		t.Fatal("fixture: the webhook context was never cancelled; B's admission did not reach the auto-advance merge")
	}
	f.requireState(a, mergequeue.StateMerged, "pr_merged")
	if e := f.requireState(b, mergequeue.StateLive, ""); e.AnchoredHeadSHA != "b2" || e.Phase != mergequeue.PhaseVerifying {
		t.Fatalf("B after a cancelled-webhook hand-off = head %q phase %q, want b2 verifying", e.AnchoredHeadSHA, e.Phase)
	}
	if tr := f.triggers(b); len(tr) != 1 || tr[0].ExpectedHeadSHA != "b2" {
		t.Fatalf("B triggers = %+v, want one for b2", tr)
	}
}

func TestMergeCandidateQueue_VerifyFailedEjectsAndAdmitsNext(t *testing.T) {
	f := newMCQFixture(t)
	a, b := f.wave()
	f.verdict(a, mergeCandidateResultFailed)
	f.requireState(a, mergequeue.StateEjected, mergeCandidateQueueReasonVerifyFailed)
	f.requireState(b, mergequeue.StateLive, "")
	if n := len(f.triggers(b)); n != 1 {
		t.Fatalf("B triggers = %d, want 1", n)
	}
}

func TestMergeCandidateQueue_NotExecutedHoldsTheLine(t *testing.T) {
	f := newMCQFixture(t)
	a, b := f.wave()
	f.verdict(a, mergeCandidateResultNotExecuted)
	if e := f.requireState(a, mergequeue.StateLive, ""); e.Phase != mergequeue.PhaseUnverified {
		t.Fatalf("A phase = %q, want unverified", e.Phase)
	}
	f.requireState(b, mergequeue.StateHeld, "")
}

func TestMergeCandidateQueue_AdvanceConflictEjects(t *testing.T) {
	f := newMCQFixture(t)
	a, b := f.wave()
	f.gh.set(func() {
		f.gh.behind["b1"] = []string{"base-x"}
		f.gh.mergeStatus[b.branch] = http.StatusConflict
	})
	f.closePR(a, true)
	f.requireState(b, mergequeue.StateEjected, mergeCandidateQueueReasonAdvanceConflict)
	if n := len(f.triggers(b)); n != 0 {
		t.Fatalf("B triggers = %d, want 0 after a conflicted advance", n)
	}
}

func TestMergeCandidateQueue_PassNotStartableEjects(t *testing.T) {
	f := newMCQFixture(t)
	a, b := f.wave()
	f.setStage(b.impl.ID, run.StageStateRunning)
	f.closePR(a, true)
	f.requireState(b, mergequeue.StateEjected, mergeCandidateQueueReasonPassNotStartable)
}

func TestMergeCandidateQueue_TransientAdvanceFailureReturnsToHeld(t *testing.T) {
	f := newMCQFixture(t)
	a, b := f.wave()
	held := f.entry(b)
	f.gh.set(func() { f.gh.compareStatus["b1"] = http.StatusInternalServerError })
	f.closePR(a, true)
	e := f.requireState(b, mergequeue.StateHeld, "")
	if !e.EnqueuedAt.Equal(held.EnqueuedAt) {
		t.Fatalf("B enqueued_at moved %v -> %v, want kept", held.EnqueuedAt, e.EnqueuedAt)
	}
	if st, _ := f.srv.mergeCandidateQueueForRun(context.Background(), b.id); st == nil || st.Position != 1 {
		t.Fatalf("B status = %+v, want held at position 1", st)
	}
}

func TestMergeCandidateQueue_DroppedModes(t *testing.T) {
	t.Run("held PR merged externally", func(t *testing.T) {
		f := newMCQFixture(t)
		_, b := f.wave()
		f.closePR(b, true)
		f.requireState(b, mergequeue.StateDropped, mergeCandidateQueueReasonPRMergedExternally)
	})
	t.Run("PR merged with no webhook is found at admission", func(t *testing.T) {
		f := newMCQFixture(t)
		a, b := f.wave()
		f.gh.set(func() { f.gh.prs[b.pr].state, f.gh.prs[b.pr].merged = "closed", true })
		f.closePR(a, true)
		f.requireState(b, mergequeue.StateDropped, mergeCandidateQueueReasonPRMergedExternally)
	})
	t.Run("PR closed with no webhook is found at admission", func(t *testing.T) {
		f := newMCQFixture(t)
		a, b := f.wave()
		f.gh.set(func() { f.gh.prs[b.pr].state = "closed" })
		f.closePR(a, true)
		f.requireState(b, mergequeue.StateDropped, mergeCandidateQueueReasonPRClosed)
	})
	t.Run("PR closed without merge", func(t *testing.T) {
		f := newMCQFixture(t)
		_, b := f.wave()
		f.closePR(b, false)
		f.requireState(b, mergequeue.StateDropped, mergeCandidateQueueReasonPRClosed)
	})
	t.Run("run terminal before admission", func(t *testing.T) {
		f := newMCQFixture(t)
		a, b := f.wave()
		f.exec(`UPDATE runs SET state = 'cancelled' WHERE id = $1`, b.id)
		f.closePR(a, true)
		f.requireState(b, mergequeue.StateDropped, mergeCandidateQueueReasonRunTerminal)
	})
	t.Run("live run terminal is dropped by the next pump", func(t *testing.T) {
		f := newMCQFixture(t)
		a, b := f.wave()
		f.exec(`UPDATE runs SET state = 'failed' WHERE id = $1`, a.id)
		f.pumpBase(a)
		f.requireState(a, mergequeue.StateDropped, mergeCandidateQueueReasonRunTerminal)
		f.requireState(b, mergequeue.StateLive, "")
	})
	t.Run("release for run", func(t *testing.T) {
		f := newMCQFixture(t)
		a, b := f.wave()
		f.srv.releaseMergeCandidateQueueForRun(context.Background(), a.id, mergeCandidateQueueReasonRunCancelled)
		f.requireState(a, mergequeue.StateDropped, mergeCandidateQueueReasonRunCancelled)
		f.requireState(b, mergequeue.StateLive, "")
	})
}

func (f *mcqFixture) pumpBase(r *mcqRun) {
	f.t.Helper()
	runRow, err := f.repo.GetRun(context.Background(), r.id)
	if err != nil {
		f.t.Fatalf("get run: %v", err)
	}
	f.srv.pumpMergeCandidateQueue(context.Background(), mergequeue.Base{AccountID: runRow.AccountID, Repo: runRow.Repo, BaseRef: mcqBase})
}

// TestMergeCandidateQueue_PeriodicPumpReleasesTerminalLiveRun: the live
// entry's run goes terminal WITHOUT the REST cancel (state set to failed
// directly, as a stage failure or a sweep does), so no event hook releases the
// base. ONE periodic sweep — no enqueue, settle or cancel event, no base named
// by the caller — drops the live entry and admits the held one behind it.
func TestMergeCandidateQueue_PeriodicPumpReleasesTerminalLiveRun(t *testing.T) {
	f := newMCQFixture(t)
	a, b := f.wave()
	f.exec(`UPDATE runs SET state = 'failed' WHERE id = $1`, a.id)
	f.requireState(a, mergequeue.StateLive, "")
	f.requireState(b, mergequeue.StateHeld, "")

	if n := f.srv.PumpMergeCandidateQueues(context.Background()); n != 1 {
		t.Fatalf("periodic pump swept %d bases, want 1", n)
	}
	f.requireState(a, mergequeue.StateDropped, mergeCandidateQueueReasonRunTerminal)
	f.requireState(b, mergequeue.StateLive, "")
	if n := len(f.rows(b, CategoryMergeCandidateQueueAdmitted)); n != 1 {
		t.Fatalf("B admitted audit rows = %d, want 1", n)
	}
	if n := f.srv.PumpMergeCandidateQueues(context.Background()); n != 1 {
		t.Fatalf("second sweep pumped %d bases, want 1 (B still live)", n)
	}
	f.requireState(b, mergequeue.StateLive, "")
}

// TestMergeCandidateQueue_PeriodicPumpReclaimsStaleAdmission: a server that
// crashed mid-admission leaves a live entry in phase admitted with no admission
// work done. The periodic pump leaves a FRESH admission alone (its admitting
// pump may still be working) and RE-CLAIMS one older than
// mergequeue.AdmissionStaleAfter, running the full admission: auto-advance,
// re-anchor, pass trigger, and an admitted audit row with readmitted:true.
//
// The crashed-admission state is built by driving the STORE directly
// (Enqueue + AdmitNext, no server admission), never through the stale predicate
// under test. The backdate runs in the DB clock domain (clock_timestamp()) and
// the before/after admitted_at values are both read back from the DB, so no Go
// time.Now() value is compared against a DB stamp (#3048).
// Counterfactuals: replacing AdmitNext's stale predicate with `false` → the
// stale arm leaves A live/admitted at a0 (RED); replacing it with
// `phase = 'admitted'` (no age bound) → the fresh arm's first pump re-claims and
// triggers at once (RED).
func TestMergeCandidateQueue_PeriodicPumpReclaimsStaleAdmission(t *testing.T) {
	f := newMCQFixture(t)
	ctx := context.Background()
	a := f.seed("a", "a0", []byte(mcvUndelegatedSpecYAML))
	f.behind(a, "a0", "a1")

	res, err := f.store.Enqueue(ctx, mergequeue.EnqueueRequest{RunID: a.id, BaseRef: mcqBase, HeadSHA: "a0"})
	if err != nil {
		t.Fatalf("store enqueue: %v", err)
	}
	adm, err := f.store.AdmitNext(ctx, res.Entry.Base())
	if err != nil || !adm.Admitted || adm.Readmitted || adm.Entry == nil {
		t.Fatalf("store AdmitNext = %+v / %v, want a first (not re-) admission", adm, err)
	}
	if e := f.requireState(a, mergequeue.StateLive, ""); e.Phase != mergequeue.PhaseAdmitted || e.AnchoredHeadSHA != "a0" {
		t.Fatalf("seeded entry = phase %q head %q, want admitted at a0", e.Phase, e.AnchoredHeadSHA)
	}

	// FRESH: the pump finds a live, non-stale admission and does nothing.
	if n := f.srv.PumpMergeCandidateQueues(ctx); n != 1 {
		t.Fatalf("fresh-arm pump swept %d bases, want 1", n)
	}
	if e := f.requireState(a, mergequeue.StateLive, ""); e.Phase != mergequeue.PhaseAdmitted || e.AnchoredHeadSHA != "a0" {
		t.Fatalf("fresh admission after pump = phase %q head %q, want untouched admitted at a0", e.Phase, e.AnchoredHeadSHA)
	}
	if n := len(f.gh.mergeCalls()); n != 0 {
		t.Fatalf("fresh admission made %d merge calls, want 0", n)
	}
	if n := len(f.triggers(a)); n != 0 {
		t.Fatalf("fresh admission wrote %d triggers, want 0", n)
	}
	if n := len(f.rows(a, CategoryMergeCandidateQueueAdmitted)); n != 0 {
		t.Fatalf("fresh admission wrote %d admitted rows, want 0", n)
	}

	// STALE: backdate admitted_at past the bound in the DB clock domain and
	// capture the backdated stamp from the same statement.
	var backdated time.Time
	if err := f.pool.QueryRow(ctx,
		`UPDATE merge_candidate_queue_entries SET admitted_at = clock_timestamp() - make_interval(secs => $2)
		   WHERE id = $1 RETURNING admitted_at`,
		adm.Entry.ID, (mergequeue.AdmissionStaleAfter + time.Minute).Seconds()).Scan(&backdated); err != nil {
		t.Fatalf("backdate admitted_at: %v", err)
	}
	if n := f.srv.PumpMergeCandidateQueues(ctx); n != 1 {
		t.Fatalf("stale-arm pump swept %d bases, want 1", n)
	}

	if e := f.requireState(a, mergequeue.StateLive, ""); e.Phase != mergequeue.PhaseVerifying || e.AnchoredHeadSHA != "a1" {
		t.Fatalf("re-claimed entry = phase %q head %q, want verifying at a1", e.Phase, e.AnchoredHeadSHA)
	}
	calls := f.gh.mergeCalls()
	if len(calls) != 1 || calls[0].Base != a.branch || calls[0].Head != mcqBase {
		t.Fatalf("auto-advance merge calls = %+v, want exactly one into %q from %q", calls, a.branch, mcqBase)
	}
	if tr := f.triggers(a); len(tr) != 1 || tr[0].ExpectedHeadSHA != "a1" || tr[0].Cause != mergeCandidateCauseBaseAdvance {
		t.Fatalf("A triggers = %+v, want exactly one base_advance trigger for a1", tr)
	}
	if st := f.stageState(a.impl.ID); st != run.StageStatePending {
		t.Fatalf("A implement = %s, want pending (re-opened by the re-claimed admission)", st)
	}
	admitted := f.rows(a, CategoryMergeCandidateQueueAdmitted)
	if len(admitted) != 1 {
		t.Fatalf("admitted rows = %d, want exactly 1", len(admitted))
	}
	var payload struct {
		Readmitted bool `json:"readmitted"`
	}
	if err := json.Unmarshal(admitted[0].Payload, &payload); err != nil || !payload.Readmitted {
		t.Fatalf("admitted row payload = %s (decode err %v), want readmitted:true", admitted[0].Payload, err)
	}
	var reclaimed time.Time
	if err := f.pool.QueryRow(ctx, `SELECT admitted_at FROM merge_candidate_queue_entries WHERE id = $1`,
		adm.Entry.ID).Scan(&reclaimed); err != nil {
		t.Fatalf("read admitted_at: %v", err)
	}
	if !reclaimed.After(backdated) {
		t.Fatalf("admitted_at after re-claim = %s, want later than the backdated %s", reclaimed, backdated)
	}
}

// TestMergeCandidateQueue_PeriodicPumpListErrorPumpsNothing: an unreadable
// base list pumps no base (the next tick retries), and an unwired queue
// sweeps nothing.
func TestMergeCandidateQueue_PeriodicPumpListErrorPumpsNothing(t *testing.T) {
	fs := &mcqFaultStore{activeBases: func() ([]mergequeue.Base, error) { return nil, errMCQStore }}
	srv := New(Config{Addr: "127.0.0.1:0", MergeQueue: fs})
	if n := srv.PumpMergeCandidateQueues(context.Background()); n != 0 {
		t.Fatalf("sweep on a list error pumped %d bases, want 0", n)
	}
	if n := fs.called("AdmitNext"); n != 0 {
		t.Fatalf("AdmitNext ran %d times after a list error, want 0", n)
	}
	if n := New(Config{Addr: "127.0.0.1:0"}).PumpMergeCandidateQueues(context.Background()); n != 0 {
		t.Fatalf("unwired sweep pumped %d bases, want 0", n)
	}
}

// TestMergeCandidateQueue_PeriodicPumpSweepsOnceAtBootBeforeAnyTick pins the
// loop's BOOT sweep. The interval is an hour, so no tick can fire inside the
// test: the only thing that can reach ActiveBases is the sweep before the
// ticker. Counterfactual: deleting the `s.PumpMergeCandidateQueues(ctx)` that
// opens RunMergeCandidateQueuePump → `swept` never receives and the test fails
// at its deadline with "no boot sweep". The exact count also catches a doubled
// boot sweep. TestMergeCandidateQueue_PeriodicPumpRunsAtBootAndOnTick cannot
// pin this: its 5ms ticker makes its >= 2 wait satisfiable by two ticks alone.
func TestMergeCandidateQueue_PeriodicPumpSweepsOnceAtBootBeforeAnyTick(t *testing.T) {
	swept := make(chan struct{}, 1)
	fs := &mcqFaultStore{activeBases: func() ([]mergequeue.Base, error) {
		select {
		case swept <- struct{}{}:
		default:
		}
		return []mergequeue.Base{}, nil
	}}
	srv := New(Config{Addr: "127.0.0.1:0", MergeQueue: fs})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.RunMergeCandidateQueuePump(ctx, time.Hour)
	}()
	select {
	case <-swept:
	case <-time.After(timescale.D(10 * time.Second)):
		cancel()
		t.Fatalf("no boot sweep: ActiveBases ran %d times before any tick (interval 1h), want 1", fs.called("ActiveBases"))
	}
	cancel()
	select {
	case <-done:
	case <-time.After(timescale.D(10 * time.Second)):
		t.Fatal("pump loop did not return after cancel")
	}
	if n := fs.called("ActiveBases"); n != 1 {
		t.Fatalf("ActiveBases ran %d times with a 1h interval, want exactly 1 (the boot sweep)", n)
	}
}

// TestMergeCandidateQueue_PeriodicPumpRunsAtBootAndOnTick: the loop sweeps
// once immediately, again on the ticker, and returns on cancel. This is the
// TICK pin: with a 5ms ticker its >= 2 wait is satisfied by ticks alone, so the
// boot arm is pinned by ..._PeriodicPumpSweepsOnceAtBootBeforeAnyTick.
func TestMergeCandidateQueue_PeriodicPumpRunsAtBootAndOnTick(t *testing.T) {
	fs := &mcqFaultStore{activeBases: func() ([]mergequeue.Base, error) { return []mergequeue.Base{}, nil }}
	srv := New(Config{Addr: "127.0.0.1:0", MergeQueue: fs})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.RunMergeCandidateQueuePump(ctx, 5*time.Millisecond)
	}()
	deadline := time.Now().Add(10 * time.Second)
	for fs.called("ActiveBases") < 2 {
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("pump loop swept %d times in 10s, want >= 2 (boot + one tick)", fs.called("ActiveBases"))
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("pump loop did not return after cancel")
	}
}

// TestMergeCandidateQueue_FanInNotSerialized: a fan_in pass starts at once
// even while another entry holds the base.
func TestMergeCandidateQueue_FanInNotSerialized(t *testing.T) {
	f := newMCQFixture(t)
	f.wave()
	c := f.seed("c", "c0", []byte(mcvUndelegatedSpecYAML))
	f.setStage(c.impl.ID, run.StageStateAwaitingApproval)
	start, refusal := f.srv.startMergeCandidateVerifyPass(context.Background(), c.id, mergeCandidateVerifyParams{
		Branch: c.branch, BaseRef: mcqBase, HeadSHA: "c0", Cause: mergeCandidateCauseFanIn,
	}, mergeCandidateSystemActor())
	if refusal != nil || start.State != mergeCandidateStateInFlight || !start.Triggered {
		t.Fatalf("fan_in start = %+v / %+v, want in_flight triggered", start, refusal)
	}
	if n := len(f.entries(c)); n != 0 {
		t.Fatalf("fan_in run has %d queue entries, want 0", n)
	}
}

// mcqFaultStore wraps a mergequeue.Store (nil for a pure fake) and lets a test
// override any method, recording which ones ran. An override left nil falls
// through to the wrapped store.
type mcqFaultStore struct {
	mergequeue.Store
	mu           sync.Mutex
	calls        []string
	enqueue      func(mergequeue.EnqueueRequest) (mergequeue.EnqueueResult, error)
	admitNext    func(mergequeue.Base) (mergequeue.Admission, error)
	setPhase     func(uuid.UUID, mergequeue.Phase) (mergequeue.Entry, error)
	settle       func(uuid.UUID, mergequeue.State, string) (mergequeue.SettleResult, error)
	activeForRun func(uuid.UUID) (mergequeue.Status, bool, error)
	activeBases  func() ([]mergequeue.Base, error)
}

func (s *mcqFaultStore) record(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, name)
}

func (s *mcqFaultStore) called(name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, c := range s.calls {
		if c == name {
			n++
		}
	}
	return n
}

func (s *mcqFaultStore) Enqueue(ctx context.Context, req mergequeue.EnqueueRequest) (mergequeue.EnqueueResult, error) {
	s.record("Enqueue")
	if s.enqueue != nil {
		return s.enqueue(req)
	}
	return s.Store.Enqueue(ctx, req)
}

func (s *mcqFaultStore) AdmitNext(ctx context.Context, base mergequeue.Base) (mergequeue.Admission, error) {
	s.record("AdmitNext")
	if s.admitNext != nil {
		return s.admitNext(base)
	}
	return s.Store.AdmitNext(ctx, base)
}

func (s *mcqFaultStore) SetPhase(ctx context.Context, id uuid.UUID, phase mergequeue.Phase) (mergequeue.Entry, error) {
	s.record("SetPhase")
	if s.setPhase != nil {
		return s.setPhase(id, phase)
	}
	return s.Store.SetPhase(ctx, id, phase)
}

func (s *mcqFaultStore) Settle(ctx context.Context, id uuid.UUID, state mergequeue.State, reason string) (mergequeue.SettleResult, error) {
	s.record("Settle")
	if s.settle != nil {
		return s.settle(id, state, reason)
	}
	return s.Store.Settle(ctx, id, state, reason)
}

func (s *mcqFaultStore) ActiveForRun(ctx context.Context, runID uuid.UUID) (mergequeue.Status, bool, error) {
	s.record("ActiveForRun")
	if s.activeForRun != nil {
		return s.activeForRun(runID)
	}
	return s.Store.ActiveForRun(ctx, runID)
}

func (s *mcqFaultStore) ActiveBases(ctx context.Context) ([]mergequeue.Base, error) {
	s.record("ActiveBases")
	if s.activeBases != nil {
		return s.activeBases()
	}
	return s.Store.ActiveBases(ctx)
}

var errMCQStore = fmt.Errorf("queue store unavailable")

// TestMergeCandidateQueue_UnwiredIsNoOp: with Config.MergeQueue nil every
// hook is a no-op and the read seam answers (nil, nil).
func TestMergeCandidateQueue_UnwiredIsNoOp(t *testing.T) {
	srv := New(Config{Addr: "127.0.0.1:0"})
	ctx := context.Background()
	runID := uuid.New()
	if out := srv.pumpMergeCandidateQueue(ctx, mergequeue.Base{Repo: "x/y", BaseRef: mcqBase}); len(out) != 0 {
		t.Fatalf("pump with no queue = %+v, want empty", out)
	}
	srv.mergeCandidateQueueOnVerdict(ctx, runID, "h", mergeCandidateResultFailed)
	srv.mergeCandidateQueueOnPRResolved(ctx, runID, true)
	srv.releaseMergeCandidateQueueForRun(ctx, runID, mergeCandidateQueueReasonRunCancelled)
	if st, err := srv.mergeCandidateQueueForRun(ctx, runID); st != nil || err != nil {
		t.Fatalf("read seam with no queue = %+v / %v, want nil / nil", st, err)
	}
}

// TestMergeCandidateQueue_StoreReadErrorsChangeNothing: an unreadable entry
// never settles, re-phases or pumps anything — an uncertain read is not a
// verdict.
func TestMergeCandidateQueue_StoreReadErrorsChangeNothing(t *testing.T) {
	fs := &mcqFaultStore{
		activeForRun: func(uuid.UUID) (mergequeue.Status, bool, error) { return mergequeue.Status{}, false, errMCQStore },
	}
	srv := New(Config{Addr: "127.0.0.1:0", MergeQueue: fs})
	ctx := context.Background()
	runID := uuid.New()
	srv.mergeCandidateQueueOnVerdict(ctx, runID, "h", mergeCandidateResultFailed)
	srv.mergeCandidateQueueOnPRResolved(ctx, runID, true)
	srv.releaseMergeCandidateQueueForRun(ctx, runID, mergeCandidateQueueReasonRunCancelled)
	if st, err := srv.mergeCandidateQueueForRun(ctx, runID); st != nil || err == nil {
		t.Fatalf("read seam on a store error = %+v / %v, want nil / error", st, err)
	}
	if n := fs.called("ActiveForRun"); n != 4 {
		t.Fatalf("ActiveForRun calls = %d, want 4", n)
	}
	for _, m := range []string{"Settle", "SetPhase", "AdmitNext"} {
		if n := fs.called(m); n != 0 {
			t.Fatalf("%s ran %d times after an unreadable entry, want 0", m, n)
		}
	}
}

// TestMergeCandidateQueue_NoActiveEntryChangesNothing: a run with no active
// entry (a redelivery, or a run that was never queued) is a no-op.
func TestMergeCandidateQueue_NoActiveEntryChangesNothing(t *testing.T) {
	fs := &mcqFaultStore{
		activeForRun: func(uuid.UUID) (mergequeue.Status, bool, error) { return mergequeue.Status{}, false, nil },
	}
	srv := New(Config{Addr: "127.0.0.1:0", MergeQueue: fs})
	ctx := context.Background()
	runID := uuid.New()
	srv.mergeCandidateQueueOnVerdict(ctx, runID, "h", mergeCandidateResultFailed)
	srv.mergeCandidateQueueOnPRResolved(ctx, runID, false)
	srv.releaseMergeCandidateQueueForRun(ctx, runID, mergeCandidateQueueReasonRunCancelled)
	if st, err := srv.mergeCandidateQueueForRun(ctx, runID); st != nil || err != nil {
		t.Fatalf("read seam with no entry = %+v / %v, want nil / nil", st, err)
	}
	if n := fs.called("Settle") + fs.called("SetPhase"); n != 0 {
		t.Fatalf("Settle/SetPhase ran %d times with no active entry, want 0", n)
	}
}

// TestMergeCandidateQueue_VerdictForOtherHeadIgnored: a verdict for a head
// other than the live entry's anchor changes nothing.
func TestMergeCandidateQueue_VerdictForOtherHeadIgnored(t *testing.T) {
	live := mergequeue.Entry{ID: uuid.New(), RunID: uuid.New(), Repo: "x/y", BaseRef: mcqBase,
		State: mergequeue.StateLive, AnchoredHeadSHA: "a1"}
	fs := &mcqFaultStore{
		activeForRun: func(uuid.UUID) (mergequeue.Status, bool, error) {
			return mergequeue.Status{Entry: live}, true, nil
		},
	}
	srv := New(Config{Addr: "127.0.0.1:0", MergeQueue: fs})
	srv.mergeCandidateQueueOnVerdict(context.Background(), live.RunID, "a0", mergeCandidateResultFailed)
	if n := fs.called("Settle") + fs.called("SetPhase"); n != 0 {
		t.Fatalf("a stale-head verdict moved the entry (%d store writes), want 0", n)
	}
}

// TestMergeCandidateQueue_WriteFailuresAreBestEffort: a failed admit, settle
// or phase write is logged and leaves the queue alone — no pump of a line
// that was never freed, no audit row for a move that never committed.
func TestMergeCandidateQueue_WriteFailuresAreBestEffort(t *testing.T) {
	ctx := context.Background()
	live := mergequeue.Entry{ID: uuid.New(), RunID: uuid.New(), Repo: "x/y", BaseRef: mcqBase,
		State: mergequeue.StateLive, AnchoredHeadSHA: "a1"}

	t.Run("admit error stops the pump", func(t *testing.T) {
		fs := &mcqFaultStore{
			admitNext: func(mergequeue.Base) (mergequeue.Admission, error) { return mergequeue.Admission{}, errMCQStore },
		}
		srv := New(Config{Addr: "127.0.0.1:0", MergeQueue: fs})
		if out := srv.pumpMergeCandidateQueue(ctx, live.Base()); len(out) != 0 {
			t.Fatalf("pump after an admit error = %+v, want empty", out)
		}
		if n := fs.called("AdmitNext"); n != 1 {
			t.Fatalf("AdmitNext calls = %d, want 1 (the pump stops on an error)", n)
		}
	})

	t.Run("settle error does not pump", func(t *testing.T) {
		fs := &mcqFaultStore{
			activeForRun: func(uuid.UUID) (mergequeue.Status, bool, error) {
				return mergequeue.Status{Entry: live}, true, nil
			},
			settle: func(uuid.UUID, mergequeue.State, string) (mergequeue.SettleResult, error) {
				return mergequeue.SettleResult{}, errMCQStore
			},
		}
		srv := New(Config{Addr: "127.0.0.1:0", MergeQueue: fs})
		srv.releaseMergeCandidateQueueForRun(ctx, live.RunID, mergeCandidateQueueReasonRunCancelled)
		if fs.called("Settle") != 1 || fs.called("AdmitNext") != 0 {
			t.Fatalf("calls = %v, want one Settle and no pump", fs.calls)
		}
	})

	t.Run("already-terminal entry does not pump", func(t *testing.T) {
		var gotState mergequeue.State
		var gotReason string
		fs := &mcqFaultStore{
			activeForRun: func(uuid.UUID) (mergequeue.Status, bool, error) {
				return mergequeue.Status{Entry: live}, true, nil
			},
			settle: func(_ uuid.UUID, st mergequeue.State, reason string) (mergequeue.SettleResult, error) {
				gotState, gotReason = st, reason
				return mergequeue.SettleResult{Entry: live, Settled: false}, nil
			},
		}
		srv := New(Config{Addr: "127.0.0.1:0", MergeQueue: fs})
		srv.mergeCandidateQueueOnPRResolved(ctx, live.RunID, true)
		if gotState != mergequeue.StateMerged || gotReason != "pr_merged" {
			t.Fatalf("settle = %s/%q, want merged/pr_merged for a live entry", gotState, gotReason)
		}
		if n := fs.called("AdmitNext"); n != 0 {
			t.Fatalf("AdmitNext ran %d times after a no-op settle, want 0", n)
		}
	})

	t.Run("settled live entry with no audit repo still pumps", func(t *testing.T) {
		fs := &mcqFaultStore{
			settle: func(uuid.UUID, mergequeue.State, string) (mergequeue.SettleResult, error) {
				return mergequeue.SettleResult{Entry: live, Settled: true}, nil
			},
			admitNext: func(mergequeue.Base) (mergequeue.Admission, error) { return mergequeue.Admission{}, nil },
		}
		srv := New(Config{Addr: "127.0.0.1:0", MergeQueue: fs})
		if !srv.applyMergeCandidateStartToEntry(ctx, live, mergeCandidateStateFailed) {
			t.Fatal("a failed pass start must report the entry settled")
		}
		if fs.called("Settle") != 1 || fs.called("AdmitNext") != 1 {
			t.Fatalf("calls = %v, want one Settle then one pump", fs.calls)
		}
	})

	t.Run("phase error is logged, entry stays", func(t *testing.T) {
		var phases []mergequeue.Phase
		fs := &mcqFaultStore{
			activeForRun: func(uuid.UUID) (mergequeue.Status, bool, error) {
				return mergequeue.Status{Entry: live}, true, nil
			},
			setPhase: func(_ uuid.UUID, p mergequeue.Phase) (mergequeue.Entry, error) {
				phases = append(phases, p)
				return mergequeue.Entry{}, errMCQStore
			},
		}
		srv := New(Config{Addr: "127.0.0.1:0", MergeQueue: fs})
		srv.mergeCandidateQueueOnVerdict(ctx, live.RunID, "a1", mergeCandidateResultPassed)
		if srv.applyMergeCandidateStartToEntry(ctx, live, mergeCandidateStateNotRequired) {
			t.Fatal("a not_required start must not settle the entry")
		}
		if len(phases) != 2 || phases[0] != mergequeue.PhasePassed || phases[1] != mergequeue.PhasePassed {
			t.Fatalf("phases = %v, want [passed passed]", phases)
		}
		if n := fs.called("Settle"); n != 0 {
			t.Fatalf("Settle ran %d times on a phase error, want 0", n)
		}
	})
}

// TestMergeCandidateQueue_EnqueueErrorRefuses: a queue write failure refuses
// the pass with a retry instruction, starts nothing and leaves no entry.
func TestMergeCandidateQueue_EnqueueErrorRefuses(t *testing.T) {
	f := newMCQFixture(t)
	fs := &mcqFaultStore{Store: f.store,
		enqueue: func(mergequeue.EnqueueRequest) (mergequeue.EnqueueResult, error) {
			return mergequeue.EnqueueResult{}, errMCQStore
		},
	}
	f.srv.cfg.MergeQueue = fs
	a := f.seed("a", "a0", []byte(mcvUndelegatedSpecYAML))
	f.behind(a, "a0", "a1")
	resp := f.rebase(a)
	if !strings.Contains(resp.MergeCandidateVerifyRefusal, "could not be written") || resp.MergeCandidateVerifyTriggered {
		t.Fatalf("rebase on an enqueue error = refusal %q triggered %v, want a write refusal, not triggered",
			resp.MergeCandidateVerifyRefusal, resp.MergeCandidateVerifyTriggered)
	}
	if n := len(f.entries(a)); n != 0 {
		t.Fatalf("run has %d queue entries after a failed enqueue, want 0", n)
	}
	if n := len(f.triggers(a)); n != 0 {
		t.Fatalf("run has %d triggers after a failed enqueue, want 0", n)
	}
}

// --- E87.2 / #4293: ADR-096 verdict transfer through the queue ---

// transfer switches the fixture's server to mode with classifier cls and a
// faked inputs seam (the e2e test exercises the real one).
func (f *mcqFixture) transfer(mode MergeCandidateTransferMode, cls *fakeTransferClassifier) {
	f.srv.cfg.MergeCandidateTransfer = MergeCandidateTransferConfig{Mode: mode, Classifier: cls}
	f.srv.transferInputs = func(context.Context, string, string, string, string) (*mergeCandidateTransferInputs, error) {
		return &mergeCandidateTransferInputs{
			Delta: basedelta.DeltaInputs{FromBase: mctFromBase, ToBase: mctToBase,
				DeltaPaths: []string{"b/b.go"}, PRPaths: []string{"a/a.go"}},
			Workspaces: []string{"/ws/base", "/ws/candidate"},
		}, nil
	}
}

// passedAt binds r to a working dir and records a passed verdict for head
// (a base-advance head the run's rebase already classified), so head is a
// transfer SOURCE.
func (f *mcqFixture) passedAt(r *mcqRun, head string) {
	f.t.Helper()
	f.exec(`UPDATE runs SET working_dir = $1 WHERE id = $2`, mctWorkDir, r.id)
	raw, _ := json.Marshal(mergeCandidateVerifiedPayload{HeadSHA: head, Cause: mergeCandidateCauseBaseAdvance,
		VerifyCommand: mcvCommand, Result: mergeCandidateResultPassed, OutputUntrusted: true})
	kind, subject := audit.ActorSystem, "runner"
	stageID := r.impl.ID
	if _, err := f.audit.AppendChained(context.Background(), audit.ChainAppendParams{
		RunID: r.id, StageID: &stageID, Timestamp: time.Now().UTC(), Category: CategoryMergeCandidateVerified,
		ActorKind: &kind, ActorSubject: &subject, Payload: raw,
	}); err != nil {
		f.t.Fatalf("seed passed verdict: %v", err)
	}
}

func (f *mcqFixture) transferPayloads(r *mcqRun, category string) []mergeCandidateTransferPayload {
	f.t.Helper()
	var out []mergeCandidateTransferPayload
	for _, e := range f.rows(r, category) {
		var p mergeCandidateTransferPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			f.t.Fatalf("decode %s: %v", category, err)
		}
		out = append(out, p)
	}
	return out
}

// handOff seeds the held-B wave with b1 passed, switches transfer on, then
// A's pass passes and A merges while the base moved again, so B's admission
// auto-advances b1 → b2.
func (f *mcqFixture) handOff(mode MergeCandidateTransferMode, cls *fakeTransferClassifier) (a, b *mcqRun) {
	f.t.Helper()
	a, b = f.wave()
	f.passedAt(b, "b1")
	f.transfer(mode, cls)
	f.verdict(a, mergeCandidateResultPassed)
	f.behind(b, "b1", "b2")
	f.closePR(a, true)
	return a, b
}

// TestMergeCandidateQueue_AdmissionTransferGoesStraightToPassed: the
// head-of-line admission's auto-advance of a PASSED head across an
// independent delta transfers the verdict: phase passed, no trigger, the
// implement stage untouched.
func TestMergeCandidateQueue_AdmissionTransferGoesStraightToPassed(t *testing.T) {
	f := newMCQFixture(t)
	cls := independentClassifier()
	_, b := f.handOff(MergeCandidateTransferEnforce, cls)

	if e := f.requireState(b, mergequeue.StateLive, ""); e.AnchoredHeadSHA != "b2" || e.Phase != mergequeue.PhasePassed {
		t.Fatalf("B entry = head %q phase %q, want b2 passed", e.AnchoredHeadSHA, e.Phase)
	}
	if tr := f.triggers(b); len(tr) != 0 {
		t.Fatalf("B triggers = %+v, want none (the verdict transferred)", tr)
	}
	if st := f.stageState(b.impl.ID); st != run.StageStateSucceeded {
		t.Errorf("B implement = %s, want succeeded (not re-opened)", st)
	}
	rows := f.transferPayloads(b, CategoryMergeCandidateVerdictTransferred)
	if len(rows) != 1 || rows[0].FromHead != "b1" || rows[0].ToHead != "b2" {
		t.Fatalf("B transferred rows = %+v, want one b1 → b2", rows)
	}
	if cls.count() != 1 {
		t.Errorf("classifier calls = %d, want 1", cls.count())
	}
}

// TestMergeCandidateQueue_HeldEntryReanchoredKeepsTransferredVerdict: an
// operator rebase of a HELD passed head transfers; the entry stays held, now
// anchored at the transferred head; once the line frees, admission reads
// passed and starts no pass.
func TestMergeCandidateQueue_HeldEntryReanchoredKeepsTransferredVerdict(t *testing.T) {
	f := newMCQFixture(t)
	a, b := f.wave()
	held := f.requireState(b, mergequeue.StateHeld, "")
	f.passedAt(b, "b1")
	f.transfer(MergeCandidateTransferEnforce, independentClassifier())

	f.behind(b, "b1", "b2")
	if resp := f.rebase(b); resp.MergeCandidateVerifyState != mergeCandidateStatePassed || resp.MergeCandidateVerifyTriggered {
		t.Fatalf("B rebase = state %q triggered %v, want passed/false", resp.MergeCandidateVerifyState, resp.MergeCandidateVerifyTriggered)
	}
	e := f.requireState(b, mergequeue.StateHeld, "")
	if e.ID != held.ID || !e.EnqueuedAt.Equal(held.EnqueuedAt) || e.AnchoredHeadSHA != "b2" {
		t.Fatalf("B entry = %+v, want the same held row (same place) anchored at b2", e)
	}
	var transferredReanchor bool
	for _, row := range f.rows(b, CategoryMergeCandidateQueueReanchored) {
		var p map[string]any
		_ = json.Unmarshal(row.Payload, &p)
		if p["verdict_transferred"] == true && p["anchored_head_sha"] == "b2" {
			transferredReanchor = true
		}
	}
	if !transferredReanchor {
		t.Error("no merge_candidate_queue_reanchored row with verdict_transferred:true at b2")
	}

	f.verdict(a, mergeCandidateResultPassed)
	f.closePR(a, true)
	if e := f.requireState(b, mergequeue.StateLive, ""); e.AnchoredHeadSHA != "b2" || e.Phase != mergequeue.PhasePassed {
		t.Fatalf("B after hand-off = head %q phase %q, want b2 passed", e.AnchoredHeadSHA, e.Phase)
	}
	if tr := f.triggers(b); len(tr) != 0 {
		t.Fatalf("B triggers = %+v, want none", tr)
	}
	if st := f.stageState(b.impl.ID); st != run.StageStateSucceeded {
		t.Errorf("B implement = %s, want succeeded", st)
	}
}

// TestMergeCandidateQueue_LiveEntryTransferKeepsPassed: an operator rebase of
// the LIVE entry's passed head transfers; the entry is re-anchored in place
// and stays phase passed, with no new pass.
func TestMergeCandidateQueue_LiveEntryTransferKeepsPassed(t *testing.T) {
	f := newMCQFixture(t)
	a, _ := f.wave()
	f.verdict(a, mergeCandidateResultPassed)
	f.exec(`UPDATE runs SET working_dir = $1 WHERE id = $2`, mctWorkDir, a.id)
	f.transfer(MergeCandidateTransferEnforce, independentClassifier())

	f.behind(a, "a1", "a2")
	if resp := f.rebase(a); resp.MergeCandidateVerifyState != mergeCandidateStatePassed {
		t.Fatalf("A rebase state = %q, want passed", resp.MergeCandidateVerifyState)
	}
	if e := f.requireState(a, mergequeue.StateLive, ""); e.AnchoredHeadSHA != "a2" || e.Phase != mergequeue.PhasePassed {
		t.Fatalf("A entry = head %q phase %q, want a2 passed", e.AnchoredHeadSHA, e.Phase)
	}
	if n := len(f.triggers(a)); n != 1 {
		t.Errorf("A triggers = %d, want still 1 (no pass for a2)", n)
	}
}

// TestMergeCandidateQueue_ShadowAdmissionStillStartsPass: in shadow the
// admission's pass starts exactly as without transfer, and the would-be
// decision lands asynchronously as a shadow row.
func TestMergeCandidateQueue_ShadowAdmissionStillStartsPass(t *testing.T) {
	f := newMCQFixture(t)
	_, b := f.handOff(MergeCandidateTransferShadow, independentClassifier())

	if e := f.requireState(b, mergequeue.StateLive, ""); e.AnchoredHeadSHA != "b2" || e.Phase != mergequeue.PhaseVerifying {
		t.Fatalf("B entry = head %q phase %q, want b2 verifying", e.AnchoredHeadSHA, e.Phase)
	}
	if tr := f.triggers(b); len(tr) != 1 || tr[0].ExpectedHeadSHA != "b2" {
		t.Fatalf("B triggers = %+v, want one for b2", tr)
	}
	f.srv.waitMergeCandidateTransferShadows()
	sh := f.transferPayloads(b, CategoryMergeCandidateVerdictTransferShadow)
	if len(sh) != 1 || sh[0].WouldTransfer == nil || !*sh[0].WouldTransfer || sh[0].FromHead != "b1" || sh[0].ToHead != "b2" {
		t.Fatalf("B shadow rows = %+v, want one would_transfer b1 → b2", sh)
	}
	if n := len(f.rows(b, CategoryMergeCandidateVerdictTransferred)); n != 0 {
		t.Errorf("B transferred rows = %d in shadow, want 0", n)
	}
}

// TestMergeCandidateQueue_AdmissionClassifierPastSubBudgetStillStartsPass
// (approval condition S2): a classifier that blocks past its sub-budget is
// undecidable:deadline_exceeded — a recorded refusal — and the pass still
// starts; the entry is not stranded in phase admitted.
func TestMergeCandidateQueue_AdmissionClassifierPastSubBudgetStillStartsPass(t *testing.T) {
	f := newMCQFixture(t)
	cls := &fakeTransferClassifier{block: true}
	a, b := f.wave()
	f.passedAt(b, "b1")
	f.transfer(MergeCandidateTransferEnforce, cls)
	f.srv.cfg.MergeCandidateTransfer.Deadline = timescale.D(50 * time.Millisecond)
	f.verdict(a, mergeCandidateResultPassed)
	f.behind(b, "b1", "b2")
	f.closePR(a, true)

	if e := f.requireState(b, mergequeue.StateLive, ""); e.AnchoredHeadSHA != "b2" || e.Phase != mergequeue.PhaseVerifying {
		t.Fatalf("B entry = head %q phase %q, want b2 verifying (not stranded)", e.AnchoredHeadSHA, e.Phase)
	}
	if tr := f.triggers(b); len(tr) != 1 || tr[0].ExpectedHeadSHA != "b2" {
		t.Fatalf("B triggers = %+v, want one for b2", tr)
	}
	rows := f.transferPayloads(b, CategoryMergeCandidateVerdictTransferRefused)
	if len(rows) != 1 || rows[0].ReasonCode != transferReasonDeadlineExceeded {
		t.Fatalf("B refused rows = %+v, want one undecidable:deadline_exceeded", rows)
	}
}

// TestMergeCandidateQueue_AdmissionSubBudgetLeavesHandOffReserve (approval
// condition S2): even with a transfer Deadline far past the hand-off budget,
// the classification gets at most the hand-off budget minus the 60s reserve.
func TestMergeCandidateQueue_AdmissionSubBudgetLeavesHandOffReserve(t *testing.T) {
	f := newMCQFixture(t)
	cls := independentClassifier()
	a, b := f.wave()
	f.passedAt(b, "b1")
	f.transfer(MergeCandidateTransferEnforce, cls)
	f.srv.cfg.MergeCandidateTransfer.Deadline = 10 * time.Minute
	f.verdict(a, mergeCandidateResultPassed)
	f.behind(b, "b1", "b2")
	f.closePR(a, true)

	if cls.count() != 1 {
		t.Fatalf("classifier calls = %d, want 1", cls.count())
	}
	if limit := mergeCandidateQueueHandOffBudget - mergeCandidateTransferHandOffReserve; cls.deadline <= 0 || cls.deadline > limit {
		t.Errorf("classifier sub-budget = %s, want (0, %s]", cls.deadline, limit)
	}
}

// appendSystemRow appends one run-level system row to the chain.
func (f *mcqFixture) appendSystemRow(r *mcqRun, category string, payload any) {
	f.t.Helper()
	raw, _ := json.Marshal(payload)
	kind, subject := audit.ActorSystem, mergeCandidateSystemSubject
	if _, err := f.audit.AppendChained(context.Background(), audit.ChainAppendParams{
		RunID: r.id, Timestamp: time.Now().UTC(), Category: category,
		ActorKind: &kind, ActorSubject: &subject, Payload: raw,
	}); err != nil {
		f.t.Fatalf("append %s: %v", category, err)
	}
}
