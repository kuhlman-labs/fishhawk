package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/basedelta"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concurrency"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/mergequeue"
	"github.com/kuhlman-labs/fishhawk/backend/internal/orchestrator"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
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
	// base is served as GET /pulls/{n}'s base.ref ("" serves mcqBase); a
	// non-empty value is a PR retargeted to another base.
	base string
	// mergeableState is served as GET /pulls/{n}'s mergeable_state ("" by
	// default, which merge-on-pass reads as not a positive answer).
	mergeableState string
}

// mcqPut is one recorded PUT /pulls/{n}/merge (the head-pinned merge).
type mcqPut struct {
	PR     int
	SHA    string `json:"sha"`
	Method string `json:"merge_method"`
}

// mcqGitHub serves several PRs: GET /pulls/{n} answers each PR's live state;
// the behind-probe (compare/{head}...{base}) answers behind[head]; POST
// /merges merges into the named branch, moving that PR's head to
// mergeSHA[branch] (or answers mergeStatus[branch], e.g. 409 conflict). An
// empty mergeSHA[branch] answers a merge with no sha and leaves the PR head at
// its pre-merge value, so every post-merge re-read lags.
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
	// reviews / reviewsStatus serve GET /pulls/{n}/reviews (ADR-093 slice 1
	// eligibility, E87.4 / #4295): a raw JSON array per PR (default `[]`),
	// or a status. checks / checksStatus serve GET /commits/{ref}/check-runs:
	// a raw JSON check_runs array per ref (default one green "ci" run), or a
	// status.
	reviews       map[int]string
	reviewsStatus map[int]int
	checks        map[string]string
	checksStatus  map[string]int
	// puts records every PUT /pulls/{n}/merge; putStatus[n] answers it with
	// that status instead; onPut, when set, runs first on every PUT (outside
	// the lock: the park seam). A PUT whose sha is not the PR's head answers
	// 409 like GitHub. A successful PUT marks the PR merged and closed and
	// MOVES THE BASE: every other open PR's head then reads behind.
	puts      []mcqPut
	putStatus map[int]int
	onPut     func(n int)

	// ADR-112 R1(a)(ii) advance integrity, FAITHFUL by default (so a clean
	// advance verifies and no other consumer's assertions change). GET
	// /git/ref/heads/{branch...} serves baseTip[branch] (default
	// mcqBaseTip(branch); a successful PUT moves the base's tip) or
	// baseTipStatus[branch]. GET /git/commits/{sha} answers the parents
	// recorded at POST /merges — [the PR's pre-merge head, the base tip last
	// served for the merged-in ref] — with wrongParent[branch] replacing the
	// second parent, or commitStatus; an unrecorded sha answers 404.
	baseTip       map[string]string
	baseTipStatus map[string]int
	baseTipServed map[string]string
	wrongParent   map[string]string
	commitStatus  int
	commitParents map[string][]string

	// ADR-112 review snapshot: POST /graphql answers ONLY the review-snapshot
	// query (recognised by `latestOpinionatedReviews`) from reviewSnapshot[n]
	// (default: no reviews, null reviewDecision / mergeStateStatus, a User
	// opener). merged / state / headRefOid come from the PR itself. The fake
	// IGNORES writersOnly (real GitHub pre-filters), so the write floor and
	// the permission filter are what tests exercise. graphQLFail[n] fails
	// that many next calls with 500; graphQLCalls counts every snapshot
	// call. Any other GraphQL query answers 404 as before.
	reviewSnapshot map[int]*mcqReviewSnapshot
	graphQLFail    map[int]int
	graphQLCalls   int

	// ADR-112 R1(c) permission read (#4620): GET
	// /collaborators/{login}/permission answers perms[login] or
	// permStatus[login]; an unlisted login answers 404, so nothing is
	// granted by default. permCalls counts reads per login.
	perms      map[string]mcqPermission
	permStatus map[string]int
	permCalls  map[string]int
}

// mcqBaseTip is the deterministic default tip served for a branch ref.
func mcqBaseTip(branch string) string { return "basetip-" + branch }

// mcqPermission is one served collaborator permission.
type mcqPermission struct {
	RoleName   string
	Permission string
}

// mcqReviewSnapshot is one PR's GraphQL review-snapshot fixture.
type mcqReviewSnapshot struct {
	Reviews []mcqReviewNode
	// ReviewDecision / MergeStateStatus: "" serves null.
	ReviewDecision   string
	MergeStateStatus string
	// Author is the PR opener; a zero Typename serves a null author.
	Author    mcqActor
	IsPrivate bool
	// Truncated makes every page advertise a next page, so the client stops
	// at its page cap with Truncated=true.
	Truncated bool
}

// mcqActor is a served GraphQL actor; a zero Typename serves null.
type mcqActor struct {
	Typename string
	Login    string
	ID       int64
}

// mcqReviewNode is one served latestOpinionatedReviews node.
type mcqReviewNode struct {
	ID          int64
	State       string
	SubmittedAt time.Time
	Commit      string
	Author      mcqActor
}

func (a mcqActor) graphQL() any {
	if a.Typename == "" {
		return nil
	}
	m := map[string]any{"__typename": a.Typename, "login": a.Login}
	// A non-User actor carries a databaseId only when the fixture sets one
	// (a Bot or Mannequin with a real id, R1(d)); zero keeps it absent.
	if a.Typename == "User" || a.ID != 0 {
		m["databaseId"] = a.ID
	}
	return m
}

func nullableEnum(v string) any {
	if v == "" {
		return nil
	}
	return v
}

// serveReviewSnapshot answers one review-snapshot GraphQL call.
func (g *mcqGitHub) serveReviewSnapshot(w http.ResponseWriter, number int, cursor any) {
	g.mu.Lock()
	g.graphQLCalls++
	if g.graphQLFail[number] > 0 {
		g.graphQLFail[number]--
		g.mu.Unlock()
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	pr, ok := g.prs[number]
	var prCopy mcqPR
	if ok {
		prCopy = *pr
	}
	fx := mcqReviewSnapshot{Author: mcqActor{Typename: "User", Login: "pr-opener", ID: 1}}
	if f := g.reviewSnapshot[number]; f != nil {
		fx = *f
		fx.Reviews = append([]mcqReviewNode(nil), f.Reviews...)
	}
	g.mu.Unlock()
	if !ok {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"repository":{"isPrivate":false,"pullRequest":null}}}`))
		return
	}
	nodes := []any{}
	if cursor == nil || fx.Truncated {
		for _, r := range fx.Reviews {
			var commit any
			if r.Commit != "" {
				commit = map[string]any{"oid": r.Commit}
			}
			var submitted any
			if !r.SubmittedAt.IsZero() {
				submitted = r.SubmittedAt.UTC().Format(time.RFC3339)
			}
			nodes = append(nodes, map[string]any{"databaseId": r.ID, "state": r.State,
				"submittedAt": submitted, "commit": commit, "author": r.Author.graphQL()})
		}
	}
	state := "OPEN"
	switch {
	case prCopy.merged:
		state = "MERGED"
	case prCopy.state == "closed":
		state = "CLOSED"
	}
	page := 1
	if c, isStr := cursor.(string); isStr {
		_, _ = fmt.Sscanf(c, "c%d", &page)
		page++
	}
	body := map[string]any{"data": map[string]any{"repository": map[string]any{
		"isPrivate": fx.IsPrivate,
		"pullRequest": map[string]any{
			"merged": prCopy.merged, "state": state, "headRefOid": prCopy.head,
			"reviewDecision": nullableEnum(fx.ReviewDecision), "mergeStateStatus": nullableEnum(fx.MergeStateStatus),
			"author": fx.Author.graphQL(),
			"latestOpinionatedReviews": map[string]any{
				"pageInfo": map[string]any{"hasNextPage": fx.Truncated, "endCursor": fmt.Sprintf("c%d", page)},
				"nodes":    nodes,
			},
		},
	}}}
	raw, _ := json.Marshal(body)
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(raw)
}

// setReviewSnapshot installs PR n's GraphQL review-snapshot fixture.
func (g *mcqGitHub) setReviewSnapshot(n int, fx mcqReviewSnapshot) {
	g.set(func() { g.reviewSnapshot[n] = &fx })
}

// setPermission serves login's collaborator permission.
func (g *mcqGitHub) setPermission(login, roleName, permission string) {
	g.set(func() { g.perms[login] = mcqPermission{RoleName: roleName, Permission: permission} })
}

// snapshotCalls is the number of review-snapshot GraphQL calls served.
func (g *mcqGitHub) snapshotCalls() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.graphQLCalls
}

// permissionCalls is the number of permission reads served for login.
func (g *mcqGitHub) permissionCalls(login string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.permCalls[login]
}

// mcqGreenChecks is the default check-runs listing: one completed, green run.
const mcqGreenChecks = `[{"id":1,"name":"ci","status":"completed","conclusion":"success","started_at":"2026-01-01T00:00:00Z","completed_at":"2026-01-01T00:01:00Z","check_suite":{"id":1},"app":{"slug":"github-actions"}}]`

func (g *mcqGitHub) client(t *testing.T) *githubclient.Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{repo}/pulls/{number}", func(w http.ResponseWriter, r *http.Request) {
		n, _ := strconv.Atoi(r.PathValue("number"))
		g.mu.Lock()
		pr, ok := g.prs[n]
		var body string
		if ok {
			base := mcqBase
			if pr.base != "" {
				base = pr.base
			}
			body = fmt.Sprintf(`{"node_id":"PR_%d","state":%q,"merged":%t,"mergeable_state":%q,"head":{"sha":%q,"ref":%q},"base":{"ref":%q}}`,
				n, pr.state, pr.merged, pr.mergeableState, pr.head, pr.branch, base)
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
		if status == 0 && sha != "" {
			prior := ""
			for _, pr := range g.prs {
				if pr.branch == call.Base {
					prior = pr.head
					pr.head = sha
				}
			}
			second := g.baseTipServed[call.Head]
			if wp := g.wrongParent[call.Base]; wp != "" {
				second = wp
			}
			g.commitParents[sha] = []string{prior, second}
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
	mux.HandleFunc("PUT /repos/{owner}/{repo}/pulls/{number}/merge", func(w http.ResponseWriter, r *http.Request) {
		n, _ := strconv.Atoi(r.PathValue("number"))
		var put mcqPut
		_ = json.NewDecoder(r.Body).Decode(&put)
		put.PR = n
		g.mu.Lock()
		onPut := g.onPut
		g.mu.Unlock()
		if onPut != nil {
			onPut(n)
		}
		g.mu.Lock()
		defer g.mu.Unlock()
		g.puts = append(g.puts, put)
		pr, ok := g.prs[n]
		status := g.putStatus[n]
		switch {
		case !ok:
			status = http.StatusNotFound
		case status == 0 && put.SHA != "" && put.SHA != pr.head:
			status = http.StatusConflict
		}
		if status != 0 {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"message":"Head branch was modified. Review and try the merge again."}`))
			return
		}
		pr.merged, pr.state = true, "closed"
		tip := fmt.Sprintf("base-after-pr-%d", n)
		g.baseTip[mcqBase] = tip
		for other, o := range g.prs {
			if other != n && o.state == "open" {
				g.behind[o.head] = append(g.behind[o.head], tip)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"merged":true,"sha":"merged-` + put.SHA + `"}`))
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/pulls/{number}/reviews", func(w http.ResponseWriter, r *http.Request) {
		n, _ := strconv.Atoi(r.PathValue("number"))
		g.mu.Lock()
		status, body := g.reviewsStatus[n], g.reviews[n]
		g.mu.Unlock()
		if status != 0 {
			w.WriteHeader(status)
			return
		}
		if body == "" {
			body = "[]"
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/commits/{ref}/check-runs", func(w http.ResponseWriter, r *http.Request) {
		ref := r.PathValue("ref")
		g.mu.Lock()
		status, body := g.checksStatus[ref], g.checks[ref]
		g.mu.Unlock()
		if status != 0 {
			w.WriteHeader(status)
			return
		}
		if body == "" {
			body = mcqGreenChecks
		}
		var runs []json.RawMessage
		_ = json.Unmarshal([]byte(body), &runs)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"total_count":%d,"check_runs":%s}`, len(runs), body)
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/git/ref/heads/{branch...}", func(w http.ResponseWriter, r *http.Request) {
		branch := r.PathValue("branch")
		g.mu.Lock()
		status := g.baseTipStatus[branch]
		tip := g.baseTip[branch]
		if tip == "" {
			tip = mcqBaseTip(branch)
		}
		if status == 0 {
			g.baseTipServed[branch] = tip
		}
		g.mu.Unlock()
		if status != 0 {
			w.WriteHeader(status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"object":{"sha":%q}}`, tip)
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/git/commits/{sha}", func(w http.ResponseWriter, r *http.Request) {
		sha := r.PathValue("sha")
		g.mu.Lock()
		status := g.commitStatus
		parents, ok := g.commitParents[sha]
		g.mu.Unlock()
		if status == 0 && !ok {
			status = http.StatusNotFound
		}
		if status != 0 {
			w.WriteHeader(status)
			return
		}
		ps := make([]string, 0, len(parents))
		for _, p := range parents {
			ps = append(ps, fmt.Sprintf(`{"sha":%q}`, p))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"sha":%q,"tree":{"sha":"tree-%s"},"parents":[%s]}`, sha, sha, strings.Join(ps, ","))
	})
	mux.HandleFunc("POST /graphql", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if !strings.Contains(req.Query, "latestOpinionatedReviews") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		n, _ := req.Variables["number"].(float64)
		g.serveReviewSnapshot(w, int(n), req.Variables["cursor"])
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/collaborators/{login}/permission", func(w http.ResponseWriter, r *http.Request) {
		login := r.PathValue("login")
		g.mu.Lock()
		g.permCalls[login]++
		status := g.permStatus[login]
		perm, ok := g.perms[login]
		g.mu.Unlock()
		if status == 0 && !ok {
			status = http.StatusNotFound
		}
		if status != 0 {
			w.WriteHeader(status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"permission":%q,"role_name":%q,"user":{"login":%q}}`, perm.Permission, perm.RoleName, login)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &githubclient.Client{
		BaseURL: srv.URL,
		Tokens:  &fakeTokenProvider{tok: "ghs_t"},
		// Scaled, so a merge request a test deliberately holds open (the
		// Shutdown drain tests) is never cut by the client's own timeout.
		HTTP:   &http.Client{Timeout: timescale.D(30 * time.Second)},
		AppJWT: func() (string, error) { return "ghs_jwt", nil },
	}
}

func (g *mcqGitHub) set(fn func()) {
	g.mu.Lock()
	defer g.mu.Unlock()
	fn()
}

func (g *mcqGitHub) putCalls() []mcqPut {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]mcqPut(nil), g.puts...)
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
			compareStatus: map[string]int{}, mergeSHA: map[string]string{}, mergeStatus: map[string]int{},
			reviews: map[int]string{}, reviewsStatus: map[int]int{}, checks: map[string]string{}, checksStatus: map[string]int{},
			putStatus: map[int]int{},
			baseTip:   map[string]string{}, baseTipStatus: map[string]int{}, baseTipServed: map[string]string{},
			wrongParent: map[string]string{}, commitParents: map[string][]string{},
			reviewSnapshot: map[int]*mcqReviewSnapshot{}, graphQLFail: map[int]int{},
			perms: map[string]mcqPermission{}, permStatus: map[string]int{}, permCalls: map[string]int{}},
	}
	f.srv = New(Config{
		Addr:        "127.0.0.1:0",
		RunRepo:     f.repo,
		AuditRepo:   f.audit,
		GitHub:      f.gh.client(t),
		MergeQueue:  f.store,
		Concurrency: concurrency.NewPostgresStore(pool),
		// ADR-093 slice 1: eligibility leg (d) reads open concerns.
		ConcernRepo: concern.NewPostgresRepository(pool),
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

// TestMergeCandidateQueue_MergeOnSpecWithoutReviewStageAppendsNoMembersRefusal
// pins R3 of #4132 on the real Postgres stores: the MCQ fixture's cached spec
// (mcvUndelegatedSpecYAML) declares no review-type stage while the run carries
// one, so the merge-settled members check ADMITS (no gate can declare members)
// rather than holding members_unevaluable. It asserts FIRST that the review
// stage reached succeeded — the merge passed both the ADR-036 hold and the
// members seam — so the empty-refusal assertion is not vacuous. A pgtest
// integration test; the in-process vehicle for CF6 is
// TestMergeSettledMembers_Verdicts row 04.
func TestMergeCandidateQueue_MergeOnSpecWithoutReviewStageAppendsNoMembersRefusal(t *testing.T) {
	f := newMCQFixture(t)
	r := f.seed("nomembers", "n0", []byte(mcvUndelegatedSpecYAML))
	f.closePR(r, true)
	if got := f.stageState(r.review.ID); got != run.StageStateSucceeded {
		t.Fatalf("review stage = %s, want succeeded (the merge must pass the members seam)", got)
	}
	if rows := f.rows(r, CategoryMergeSettleMembersRefused); len(rows) != 0 {
		t.Errorf("merge_settle_members_refused rows = %d, want 0 on a spec without a review stage", len(rows))
	}
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
	// A takes the line merge-queue ELIGIBLE (ADR-093 slice 1: a configured
	// agent reviewer with no terminal review would answer not_queued), then
	// its cached spec gains the agent reviewer so the ADR-036 gate holds.
	a := f.seed("a", "a0", spec)
	b := f.seed("b", "b0", spec)
	f.behind(a, "a0", "a1")
	f.behind(b, "b0", "b1")
	f.rebase(a)
	f.rebase(b)
	f.requireState(b, mergequeue.StateHeld, "")
	f.exec(`UPDATE runs SET workflow_spec = $1 WHERE id = $2`, gated, a.id)
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
	f.pumpBaseCtx(context.Background(), r)
}

// pumpBaseCtx pumps r's base on ctx and returns the pump's per-run answers.
func (f *mcqFixture) pumpBaseCtx(ctx context.Context, r *mcqRun) map[uuid.UUID]mergeCandidateAdmitResult {
	f.t.Helper()
	runRow, err := f.repo.GetRun(context.Background(), r.id)
	if err != nil {
		f.t.Fatalf("get run: %v", err)
	}
	return f.srv.pumpMergeCandidateQueue(ctx, mergequeue.Base{AccountID: runRow.AccountID, Repo: runRow.Repo, BaseRef: mcqBase})
}

// freeLine settles a's live entry merged through the STORE alone (no hook, no
// pump), so the base's line is free and its held entries wait for a pump.
func (f *mcqFixture) freeLine(a *mcqRun) {
	f.t.Helper()
	if _, err := f.store.Settle(context.Background(), f.entry(a).ID, mergequeue.StateMerged, "pr_merged"); err != nil {
		f.t.Fatalf("settle A through the store: %v", err)
	}
}

// handOff frees a's line and pumps the base directly, returning b's admission
// answer from the pump's map (the refusal wording never reaches a queue or
// audit row, so the map is the only place to read it).
func (f *mcqFixture) handOff(a, b *mcqRun) mergeCandidateAdmitResult {
	f.t.Helper()
	f.freeLine(a)
	out := f.pumpBaseCtx(context.Background(), b)
	res, ok := out[b.id]
	if !ok {
		f.t.Fatalf("the pump did not admit B: %+v", out)
	}
	return res
}

// mergesInto counts the fake's merge calls into r's branch.
func (f *mcqFixture) mergesInto(r *mcqRun) int {
	n := 0
	for _, c := range f.gh.mergeCalls() {
		if c.Base == r.branch {
			n++
		}
	}
	return n
}

// seedTrigger appends a merge-candidate verify trigger for r's implement
// stage at head BY CONSTRUCTION: no queue hook runs and no stage is re-opened.
func (f *mcqFixture) seedTrigger(r *mcqRun, head string) {
	f.t.Helper()
	if err := f.srv.appendMergeCandidateTrigger(context.Background(), r.id, r.impl.ID, mergeCandidateVerifyTrigger{
		Branch: r.branch, BaseRef: mcqBase, ExpectedHeadSHA: head, Cause: mergeCandidateCauseBaseAdvance,
		VerifyCommand: "scripts/test verify", PriorState: string(run.StageStateSucceeded),
	}, mergeCandidateSystemActor()); err != nil {
		f.t.Fatalf("seed trigger at %s: %v", head, err)
	}
}

// waveABC is wave plus a third run C held behind B.
func (f *mcqFixture) waveABC() (a, b, c *mcqRun) {
	f.t.Helper()
	a, b = f.wave()
	c = f.seed("c", "c0", []byte(mcvUndelegatedSpecYAML))
	f.behind(c, "c0", "c1")
	if resp := f.rebase(c); resp.MergeCandidateVerifyState != mergeCandidateStateHeld || resp.MergeCandidateVerifyTriggered {
		f.t.Fatalf("C rebase verify state = %q triggered=%v, want held, not triggered", resp.MergeCandidateVerifyState, resp.MergeCandidateVerifyTriggered)
	}
	return a, b, c
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
// override any method, recording every call with the context it ran on. An
// override left nil falls through to the wrapped store.
type mcqFaultStore struct {
	mergequeue.Store
	mu        sync.Mutex
	calls     []mcqCall
	enqueue   func(mergequeue.EnqueueRequest) (mergequeue.EnqueueResult, error)
	admitNext func(mergequeue.Base) (mergequeue.Admission, error)
	unadmit   func(uuid.UUID) (mergequeue.Entry, error)
	// unadmitAt, when set, replaces Unadmit with the caller's ctx and epoch
	// (the barrier seam of merge_queue_pump_test.go); it wins over unadmit.
	unadmitAt    func(ctx context.Context, id uuid.UUID, epoch int64) (mergequeue.Entry, error)
	setPhase     func(uuid.UUID, mergequeue.Phase) (mergequeue.Entry, error)
	reanchor     func(uuid.UUID, string, string) (mergequeue.Entry, error)
	settle       func(uuid.UUID, mergequeue.State, string) (mergequeue.SettleResult, error)
	activeForRun func(uuid.UUID) (mergequeue.Status, bool, error)
	activeBases  func() ([]mergequeue.Base, error)
	markIntent   func(uuid.UUID) (mergequeue.MergeIntentResult, error)
	intentLock   func(ctx context.Context, fn func(context.Context) error) error
}

// mcqCall is one recorded store call: the method, the id it named (the entry
// id, the run id for ActiveForRun, uuid.Nil otherwise), when it ran and its
// context's deadline (hasDeadline false: the context carried none).
type mcqCall struct {
	method      string
	id          uuid.UUID
	at          time.Time
	deadline    time.Time
	hasDeadline bool
}

func (s *mcqFaultStore) record(ctx context.Context, name string, id uuid.UUID) {
	deadline, ok := ctx.Deadline()
	c := mcqCall{method: name, id: id, at: time.Now(), deadline: deadline, hasDeadline: ok}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, c)
}

func (s *mcqFaultStore) called(name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, c := range s.calls {
		if c.method == name {
			n++
		}
	}
	return n
}

// names is the recorded method sequence, for failure messages.
func (s *mcqFaultStore) names() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.calls))
	for _, c := range s.calls {
		out = append(out, c.method)
	}
	return out
}

// onlyCall returns the single recorded call of method naming id, failing the
// test unless there is exactly one.
func (s *mcqFaultStore) onlyCall(t *testing.T, method string, id uuid.UUID) mcqCall {
	t.Helper()
	return s.nthOfCalls(t, method, id, 1)
}

// nthOfCalls requires exactly n calls of method naming id and returns the
// last. ADR-093 D8: an admission records two phases (advancing before the
// behind-probe, then verifying or passed), so its SetPhase count is 2.
func (s *mcqFaultStore) nthOfCalls(t *testing.T, method string, id uuid.UUID, n int) mcqCall {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	var hits []mcqCall
	for _, c := range s.calls {
		if c.method == method && c.id == id {
			hits = append(hits, c)
		}
	}
	if len(hits) != n {
		t.Fatalf("%s calls naming %s = %d, want exactly %d", method, id, len(hits), n)
	}
	return hits[n-1]
}

func (s *mcqFaultStore) Enqueue(ctx context.Context, req mergequeue.EnqueueRequest) (mergequeue.EnqueueResult, error) {
	s.record(ctx, "Enqueue", uuid.Nil)
	if s.enqueue != nil {
		return s.enqueue(req)
	}
	if s.Store == nil {
		return mergequeue.EnqueueResult{}, errMCQNilStore
	}
	return s.Store.Enqueue(ctx, req)
}

func (s *mcqFaultStore) AdmitNext(ctx context.Context, base mergequeue.Base) (mergequeue.Admission, error) {
	s.record(ctx, "AdmitNext", uuid.Nil)
	if s.admitNext != nil {
		return s.admitNext(base)
	}
	if s.Store == nil {
		return mergequeue.Admission{}, errMCQNilStore
	}
	return s.Store.AdmitNext(ctx, base)
}

// errMCQNilStore is a pure fake's (nil embedded Store) answer to a method it
// has no override for: a named error instead of a nil-pointer panic.
var errMCQNilStore = fmt.Errorf("mcqFaultStore: no embedded store and no override for this method")

func (s *mcqFaultStore) Unadmit(ctx context.Context, id uuid.UUID, epoch int64) (mergequeue.Entry, error) {
	s.record(ctx, "Unadmit", id)
	if s.unadmitAt != nil {
		return s.unadmitAt(ctx, id, epoch)
	}
	if s.unadmit != nil {
		return s.unadmit(id)
	}
	if s.Store == nil {
		return mergequeue.Entry{}, errMCQNilStore
	}
	return s.Store.Unadmit(ctx, id, epoch)
}

func (s *mcqFaultStore) Reanchor(ctx context.Context, id uuid.UUID, headSHA, baseSHA string) (mergequeue.Entry, error) {
	s.record(ctx, "Reanchor", id)
	if s.reanchor != nil {
		return s.reanchor(id, headSHA, baseSHA)
	}
	if s.Store == nil {
		return mergequeue.Entry{}, errMCQNilStore
	}
	return s.Store.Reanchor(ctx, id, headSHA, baseSHA)
}

// ReanchorAt records as "Reanchor" and honours the reanchor override, so a
// test counting or failing re-anchors sees the admission's CAS re-anchor.
func (s *mcqFaultStore) ReanchorAt(ctx context.Context, id uuid.UUID, epoch int64, headSHA, baseSHA string) (mergequeue.Entry, error) {
	s.record(ctx, "Reanchor", id)
	if s.reanchor != nil {
		return s.reanchor(id, headSHA, baseSHA)
	}
	if s.Store == nil {
		return mergequeue.Entry{}, errMCQNilStore
	}
	return s.Store.ReanchorAt(ctx, id, epoch, headSHA, baseSHA)
}

func (s *mcqFaultStore) SetPhase(ctx context.Context, id uuid.UUID, epoch int64, phase mergequeue.Phase) (mergequeue.Entry, error) {
	s.record(ctx, "SetPhase", id)
	if s.setPhase != nil {
		return s.setPhase(id, phase)
	}
	if s.Store == nil {
		return mergequeue.Entry{}, errMCQNilStore
	}
	return s.Store.SetPhase(ctx, id, epoch, phase)
}

func (s *mcqFaultStore) Settle(ctx context.Context, id uuid.UUID, state mergequeue.State, reason string) (mergequeue.SettleResult, error) {
	s.record(ctx, "Settle", id)
	if s.settle != nil {
		return s.settle(id, state, reason)
	}
	if s.Store == nil {
		return mergequeue.SettleResult{}, errMCQNilStore
	}
	return s.Store.Settle(ctx, id, state, reason)
}

// SettleAt records as "Settle" and honours the settle override, so a test
// counting or failing settles sees the admission's CAS settle.
func (s *mcqFaultStore) SettleAt(ctx context.Context, id uuid.UUID, epoch int64, state mergequeue.State, reason string) (mergequeue.SettleResult, error) {
	s.record(ctx, "Settle", id)
	if s.settle != nil {
		return s.settle(id, state, reason)
	}
	if s.Store == nil {
		return mergequeue.SettleResult{}, errMCQNilStore
	}
	return s.Store.SettleAt(ctx, id, epoch, state, reason)
}

func (s *mcqFaultStore) ListForBase(ctx context.Context, base mergequeue.Base) ([]mergequeue.Entry, error) {
	s.record(ctx, "ListForBase", uuid.Nil)
	if s.Store == nil {
		return nil, errMCQNilStore
	}
	return s.Store.ListForBase(ctx, base)
}

func (s *mcqFaultStore) RecentLineTurns(ctx context.Context, base mergequeue.Base, limit int) ([]time.Duration, error) {
	s.record(ctx, "RecentLineTurns", uuid.Nil)
	if s.Store == nil {
		return nil, errMCQNilStore
	}
	return s.Store.RecentLineTurns(ctx, base, limit)
}

func (s *mcqFaultStore) ClaimMergeDispatch(ctx context.Context, runID uuid.UUID, path, headSHA string) (mergequeue.DispatchClaim, error) {
	s.record(ctx, "ClaimMergeDispatch", runID)
	if s.Store == nil {
		return mergequeue.DispatchClaim{}, errMCQNilStore
	}
	return s.Store.ClaimMergeDispatch(ctx, runID, path, headSHA)
}

func (s *mcqFaultStore) MarkMergeDispatchFailed(ctx context.Context, runID uuid.UUID, cause string) error {
	s.record(ctx, "MarkMergeDispatchFailed", runID)
	if s.Store == nil {
		return errMCQNilStore
	}
	return s.Store.MarkMergeDispatchFailed(ctx, runID, cause)
}

func (s *mcqFaultStore) ReleaseMergeDispatch(ctx context.Context, claim mergequeue.DispatchClaim) error {
	s.record(ctx, "ReleaseMergeDispatch", claim.RunID)
	if s.Store == nil {
		return errMCQNilStore
	}
	return s.Store.ReleaseMergeDispatch(ctx, claim)
}

func (s *mcqFaultStore) MergeDispatch(ctx context.Context, runID uuid.UUID) (mergequeue.DispatchClaim, bool, error) {
	s.record(ctx, "MergeDispatch", runID)
	if s.Store == nil {
		return mergequeue.DispatchClaim{}, false, errMCQNilStore
	}
	return s.Store.MergeDispatch(ctx, runID)
}

func (s *mcqFaultStore) ActiveForRun(ctx context.Context, runID uuid.UUID) (mergequeue.Status, bool, error) {
	s.record(ctx, "ActiveForRun", runID)
	if s.activeForRun != nil {
		return s.activeForRun(runID)
	}
	if s.Store == nil {
		return mergequeue.Status{}, false, errMCQNilStore
	}
	return s.Store.ActiveForRun(ctx, runID)
}

func (s *mcqFaultStore) ActiveBases(ctx context.Context) ([]mergequeue.Base, error) {
	s.record(ctx, "ActiveBases", uuid.Nil)
	if s.activeBases != nil {
		return s.activeBases()
	}
	if s.Store == nil {
		return nil, errMCQNilStore
	}
	return s.Store.ActiveBases(ctx)
}

func (s *mcqFaultStore) MarkMergeIntent(ctx context.Context, id uuid.UUID, req *mergequeue.MergeIntentRequest) (mergequeue.MergeIntentResult, error) {
	s.record(ctx, "MarkMergeIntent", id)
	if s.markIntent != nil {
		return s.markIntent(id)
	}
	if s.Store == nil {
		return mergequeue.MergeIntentResult{}, errMCQNilStore
	}
	return s.Store.MarkMergeIntent(ctx, id, req)
}

// WithMergeIntentLock delegates to the wrapped store's lock; a pure fake (nil
// Store) has no second process to exclude, so it just runs fn.
func (s *mcqFaultStore) WithMergeIntentLock(ctx context.Context, runID uuid.UUID, fn func(context.Context) error) error {
	s.record(ctx, "WithMergeIntentLock", runID)
	if s.intentLock != nil {
		return s.intentLock(ctx, fn)
	}
	if s.Store == nil {
		return fn(ctx)
	}
	return s.Store.WithMergeIntentLock(ctx, runID, fn)
}

var errMCQStore = fmt.Errorf("queue store unavailable")

// mcqFailTriggerAudit wraps an audit.Repository and, while armed, fails every
// merge-candidate verify TRIGGER append; every other category falls through.
type mcqFailTriggerAudit struct {
	audit.Repository
	armed atomic.Bool
}

func (a *mcqFailTriggerAudit) AppendChained(ctx context.Context, p audit.ChainAppendParams) (*audit.Entry, error) {
	if a.armed.Load() && p.Category == CategoryStageMergeCandidateVerifyTriggered {
		return nil, errMCQTriggerAppend
	}
	return a.Repository.AppendChained(ctx, p)
}

var errMCQTriggerAppend = fmt.Errorf("trigger append unavailable")

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
	srv.releaseMergeCandidateQueueOnReopen(ctx, runID, mergeCandidateQueueReasonReopenedForFixup, true)
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
	srv.releaseMergeCandidateQueueOnReopen(ctx, runID, mergeCandidateQueueReasonReopenedForFixup, true)
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
			t.Fatalf("calls = %v, want one Settle and no pump", fs.names())
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
		if srv.applyMergeCandidateStartToEntry(ctx, live, mergeCandidateStateFailed, mergeQueueVerifiedSourceAlreadyPassed) != admitSettled {
			t.Fatal("a failed pass start must report the entry settled")
		}
		if fs.called("Settle") != 1 || fs.called("AdmitNext") != 1 {
			t.Fatalf("calls = %v, want one Settle then one pump", fs.names())
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
		if srv.applyMergeCandidateStartToEntry(ctx, live, mergeCandidateStateNotRequired, mergeQueueVerifiedSourceAlreadyPassed) == admitSettled {
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

// TestMergeCandidateQueue_PeriodicPumpSurvivesSignalCancellation (#4303): the
// periodic pump runs on fishhawkd's signal context, and SIGINT/SIGTERM cancels
// it while B's admission is mid auto-advance (the fake's merge into B's branch
// cancels it: AdmitNext has already committed B live in phase admitted at b1,
// and Reanchor has not run). The admission is detached from that cancellation
// with its own budget, so the re-anchor and the pass start still land: B ends
// live and verifying at b2 with one trigger, not stranded in phase admitted.
// Counterfactual: handing admitMergeCandidateEntry the caller ctx instead of
// the admission ctx fails the merge request, the re-anchor and the recovery
// Unadmit, leaving B admitted at b1 with no trigger, and leaves SetPhase with
// no deadline.
func TestMergeCandidateQueue_PeriodicPumpSurvivesSignalCancellation(t *testing.T) {
	f := newMCQFixture(t)
	a, b := f.wave()
	f.exec(`UPDATE runs SET state = 'failed' WHERE id = $1`, a.id)
	f.behind(b, "b1", "b2")
	fs := &mcqFaultStore{Store: f.store}
	f.srv.cfg.MergeQueue = fs
	bEntry := f.entry(b)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.gh.set(func() {
		f.gh.onMerge = func(base string) {
			if base == b.branch {
				cancel()
			}
		}
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.srv.RunMergeCandidateQueuePump(ctx, time.Hour)
	}()
	select {
	case <-done:
	case <-time.After(timescale.D(30 * time.Second)):
		t.Fatal("the pump loop did not return after its signal context was cancelled")
	}
	if ctx.Err() == nil {
		t.Fatal("fixture: the signal context was never cancelled; B's admission did not reach the auto-advance merge")
	}

	f.requireState(a, mergequeue.StateDropped, mergeCandidateQueueReasonRunTerminal)
	if e := f.requireState(b, mergequeue.StateLive, ""); e.AnchoredHeadSHA != "b2" || e.Phase != mergequeue.PhaseVerifying {
		t.Fatalf("B after a signal-cancelled pump = head %q phase %q, want b2 verifying", e.AnchoredHeadSHA, e.Phase)
	}
	if tr := f.triggers(b); len(tr) != 1 || tr[0].ExpectedHeadSHA != "b2" || tr[0].Cause != mergeCandidateCauseBaseAdvance {
		t.Fatalf("B triggers = %+v, want exactly one base_advance trigger for b2", tr)
	}
	if st := f.stageState(b.impl.ID); st != run.StageStatePending {
		t.Fatalf("B implement = %s, want pending (re-opened by the admission)", st)
	}
	c := fs.nthOfCalls(t, "SetPhase", bEntry.ID, 2) // advancing, then verifying (ADR-093 D8)
	if !c.hasDeadline || c.deadline.After(c.at.Add(mergeCandidateQueueHandOffBudget)) {
		t.Fatalf("B SetPhase ran with deadline %v (has=%v) at %v, want a deadline within mergeCandidateQueueHandOffBudget (the signal context carries none)",
			c.deadline, c.hasDeadline, c.at)
	}
}

// TestMergeCandidateQueue_AdmissionBudgetIsPerEntry: one webhook hand-off
// chains admissions (A merges, B's admission ejects it on a pre-recorded
// failed verdict, the nested pump admits C and starts C's pass). Each admitted
// entry gets its OWN fresh budget, so the deadlines A's hook settle, B's
// admission settle and C's admission SetPhase ran under strictly increase.
// Counterfactuals: running the admission on the caller ctx, or wrapping the
// budget per BASE in PumpMergeCandidateQueues instead, leaves all three on the
// hook's one deadline (D_A == D_B).
func TestMergeCandidateQueue_AdmissionBudgetIsPerEntry(t *testing.T) {
	f := newMCQFixture(t)
	a, b, c := f.waveABC()
	f.seedTrigger(b, "b1")
	f.verdict(b, mergeCandidateResultFailed)
	f.requireState(b, mergequeue.StateHeld, "") // the verdict hook leaves a HELD entry alone
	aID, bID, cID := f.entry(a).ID, f.entry(b).ID, f.entry(c).ID
	fs := &mcqFaultStore{Store: f.store}
	f.srv.cfg.MergeQueue = fs

	f.closePR(a, true)

	f.requireState(a, mergequeue.StateMerged, "pr_merged")
	f.requireState(b, mergequeue.StateEjected, mergeCandidateQueueReasonVerifyFailed)
	if e := f.requireState(c, mergequeue.StateLive, ""); e.AnchoredHeadSHA != "c1" || e.Phase != mergequeue.PhaseVerifying {
		t.Fatalf("C after the chain = head %q phase %q, want c1 verifying", e.AnchoredHeadSHA, e.Phase)
	}
	if tr := f.triggers(c); len(tr) != 1 || tr[0].ExpectedHeadSHA != "c1" {
		t.Fatalf("C triggers = %+v, want one for c1", tr)
	}
	dA := fs.onlyCall(t, "Settle", aID)
	dB := fs.onlyCall(t, "Settle", bID)
	dC := fs.nthOfCalls(t, "SetPhase", cID, 2) // advancing, then verifying (ADR-093 D8)
	for name, d := range map[string]mcqCall{"A settle": dA, "B settle": dB, "C set phase": dC} {
		if !d.hasDeadline {
			t.Fatalf("%s ran with no deadline, want a bounded context", name)
		}
	}
	if !dA.deadline.Before(dB.deadline) || !dB.deadline.Before(dC.deadline) {
		t.Fatalf("deadlines A settle %v, B settle %v, C set phase %v: want strictly increasing (a fresh budget per admitted entry)",
			dA.deadline, dB.deadline, dC.deadline)
	}
}

// TestMergeCandidateQueue_CancelledPumpAdmitsNothingNew: the caller ctx still
// gates AdmitNext. A pump on an already-cancelled context with the line free
// and B held claims nothing: B stays held at its place, no admitted row, no
// merge. Counterfactual: detaching the WHOLE pump loop (AdmitNext included)
// admits and auto-advances B.
func TestMergeCandidateQueue_CancelledPumpAdmitsNothingNew(t *testing.T) {
	f := newMCQFixture(t)
	a, b := f.wave()
	f.behind(b, "b1", "b2") // an admission WOULD merge into B's branch
	held := f.entry(b)
	f.freeLine(a)
	merges := f.mergesInto(b)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if out := f.pumpBaseCtx(ctx, b); len(out) != 0 {
		t.Fatalf("a cancelled pump answered %+v, want nothing admitted", out)
	}

	if e := f.requireState(b, mergequeue.StateHeld, ""); !e.EnqueuedAt.Equal(held.EnqueuedAt) || e.AnchoredHeadSHA != "b1" {
		t.Fatalf("B after a cancelled pump = enqueued_at %v head %q, want %v at b1", e.EnqueuedAt, e.AnchoredHeadSHA, held.EnqueuedAt)
	}
	if n := len(f.rows(b, CategoryMergeCandidateQueueAdmitted)); n != 0 {
		t.Fatalf("B admitted rows = %d after a cancelled pump, want 0", n)
	}
	if n := f.mergesInto(b) - merges; n != 0 {
		t.Fatalf("a cancelled pump made %d merge calls into B's branch, want 0", n)
	}
}

// blockedPumpAdmission starts the periodic pump loop over a wave whose live
// run A failed and whose held B is behind b1->b2, with the fake's merge into
// B's branch held open. It returns once B's admission is INSIDE that merge:
// the loop's bgMergeQueuePump.Add precedes the request whose handler closes
// entered, so the Add happens-before any Shutdown the caller makes next.
// release unblocks the merge (idempotent, and run in t.Cleanup together with
// a bounded wait for the loop, so a failing test never wedges httptest Close
// or the pool); cancel is the loop's signal context; done closes when the
// loop returns.
func (f *mcqFixture) blockedPumpAdmission(shutdownTimeout time.Duration) (b *mcqRun, cancel context.CancelFunc, release func(), done <-chan struct{}) {
	f.t.Helper()
	a, b := f.wave()
	f.exec(`UPDATE runs SET state = 'failed' WHERE id = $1`, a.id)
	f.behind(b, "b1", "b2")
	f.srv.cfg.ShutdownTimeout = shutdownTimeout

	entered, released := make(chan struct{}), make(chan struct{})
	var enterOnce, releaseOnce sync.Once
	release = func() { releaseOnce.Do(func() { close(released) }) }
	f.gh.set(func() {
		f.gh.onMerge = func(base string) {
			if base == b.branch {
				enterOnce.Do(func() { close(entered) })
				<-released
			}
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	loopDone := make(chan struct{})
	go func() {
		defer close(loopDone)
		f.srv.RunMergeCandidateQueuePump(ctx, time.Hour)
	}()
	f.t.Cleanup(func() {
		release()
		cancel()
		select {
		case <-loopDone:
		case <-time.After(timescale.D(30 * time.Second)):
		}
	})
	select {
	case <-entered:
	case <-time.After(timescale.D(30 * time.Second)):
		f.t.Fatal("fixture: B's admission never reached the auto-advance merge")
	}
	return b, cancel, release, loopDone
}

// TestShutdown_DrainsMergeCandidateQueuePump: Shutdown WAITS for the periodic
// pump's in-flight admission (it has not returned while the merge is held),
// and the admission, detached from the cancelled signal context, completes
// inside the drain: B ends live and verifying at b2. Counterfactual: deleting
// the bgMergeQueuePump Wait (or its Add/Done) returns Shutdown at once.
func TestShutdown_DrainsMergeCandidateQueuePump(t *testing.T) {
	f := newMCQFixture(t)
	b, cancel, release, done := f.blockedPumpAdmission(timescale.D(30 * time.Second))
	cancel() // the SIGINT/SIGTERM stand-in

	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- f.srv.Shutdown(context.Background()) }()
	select {
	case <-shutdownDone:
		t.Fatal("Shutdown returned while the pump admission was in flight — it did not drain the merge-candidate queue pump")
	case <-time.After(timescale.D(300 * time.Millisecond)):
	}
	release()
	select {
	case <-shutdownDone:
	case <-time.After(timescale.D(10 * time.Second)):
		t.Fatal("Shutdown did not return after the pump admission finished")
	}
	select {
	case <-done:
	default:
		t.Fatal("Shutdown returned before the pump loop exited")
	}
	if e := f.requireState(b, mergequeue.StateLive, ""); e.AnchoredHeadSHA != "b2" || e.Phase != mergequeue.PhaseVerifying {
		t.Fatalf("B after the drained admission = head %q phase %q, want b2 verifying", e.AnchoredHeadSHA, e.Phase)
	}
	if tr := f.triggers(b); len(tr) != 1 || tr[0].ExpectedHeadSHA != "b2" {
		t.Fatalf("B triggers = %+v, want one for b2", tr)
	}
}

// TestShutdown_MergeCandidateQueuePumpDrainBoundedByDeadline: a wedged pump
// admission cannot hold Shutdown past its deadline. Shutdown returns while the
// admission is STILL blocked (the loop has not exited), and once released the
// loop exits. Counterfactual: a synchronous bgMergeQueuePump.Wait outside the
// shutdown-context select holds Shutdown until release.
func TestShutdown_MergeCandidateQueuePumpDrainBoundedByDeadline(t *testing.T) {
	f := newMCQFixture(t)
	_, cancel, release, done := f.blockedPumpAdmission(timescale.D(200 * time.Millisecond))
	cancel()

	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- f.srv.Shutdown(context.Background()) }()
	select {
	case <-shutdownDone:
	case <-time.After(timescale.D(5 * time.Second)):
		t.Fatal("Shutdown was held past its deadline by a wedged pump admission")
	}
	select {
	case <-done:
		t.Fatal("fixture: the pump loop exited before release, so Shutdown's return proves nothing about the bound")
	default:
	}
	release()
	select {
	case <-done:
	case <-time.After(timescale.D(10 * time.Second)):
		t.Fatal("the pump loop did not exit after its admission was released")
	}
}

// TestMergeCandidateQueue_AdmissionBranches: one behavioural test per
// admitMergeCandidateEntry branch the other tests do not reach. Each frees
// A's line through the store and pumps the base directly, so B's answer is
// read from the pump's map; every verdict is read back from committed queue,
// trigger and stage rows.
func TestMergeCandidateQueue_AdmissionBranches(t *testing.T) {
	t.Run("a: already failed at admission ejects verify_failed", func(t *testing.T) {
		f := newMCQFixture(t)
		a, b := f.wave()
		f.seedTrigger(b, "b1")
		f.verdict(b, mergeCandidateResultFailed)
		merges := f.mergesInto(b)
		res := f.handOff(a, b)
		f.requireState(b, mergequeue.StateEjected, mergeCandidateQueueReasonVerifyFailed)
		if res.refusal == nil || !strings.Contains(res.refusal.Reason, "already has a failed merge-candidate verdict") {
			t.Fatalf("B answer = %+v, want the already-failed refusal", res)
		}
		if n := len(f.triggers(b)); n != 1 {
			t.Fatalf("B triggers = %d, want 1 (the seeded one; no new trigger)", n)
		}
		if n := f.mergesInto(b) - merges; n != 0 {
			t.Fatalf("B admission made %d merge calls, want 0", n)
		}
	})

	t.Run("b: in_flight at admission holds the line verifying", func(t *testing.T) {
		f := newMCQFixture(t)
		a, b := f.wave()
		f.seedTrigger(b, "b1")
		res := f.handOff(a, b)
		if e := f.requireState(b, mergequeue.StateLive, ""); e.Phase != mergequeue.PhaseVerifying || e.AnchoredHeadSHA != "b1" {
			t.Fatalf("B entry = phase %q head %q, want verifying at b1", e.Phase, e.AnchoredHeadSHA)
		}
		if res.start == nil || res.start.State != mergeCandidateStateInFlight || res.start.StageID != b.impl.ID {
			t.Fatalf("B answer = %+v, want in_flight on its implement stage", res)
		}
		if n := len(f.triggers(b)); n != 1 {
			t.Fatalf("B triggers = %d, want 1 (no new trigger)", n)
		}
		if st := f.stageState(b.impl.ID); st != run.StageStateSucceeded {
			t.Fatalf("B implement = %s, want succeeded (not re-opened)", st)
		}
	})

	t.Run("c: passed at admission holds the line passed", func(t *testing.T) {
		f := newMCQFixture(t)
		a, b := f.wave()
		f.seedTrigger(b, "b1")
		f.verdict(b, mergeCandidateResultPassed)
		res := f.handOff(a, b)
		if e := f.requireState(b, mergequeue.StateLive, ""); e.Phase != mergequeue.PhasePassed {
			t.Fatalf("B phase = %q, want passed", e.Phase)
		}
		if res.start == nil || res.start.State != mergeCandidateStatePassed {
			t.Fatalf("B answer = %+v, want passed", res)
		}
		if n := len(f.triggers(b)); n != 1 {
			t.Fatalf("B triggers = %d, want 1 (no new trigger)", n)
		}
	})

	t.Run("d: unresolved head after a merge returns to held", func(t *testing.T) {
		f := newMCQFixture(t)
		a, b := f.wave()
		// The merge answers no sha and the PR head stays at the pre-merge b1
		// for every post-merge re-read (the fixture's millisecond re-read
		// schedule, postMergeHeadReadBackoff, keeps that short).
		f.gh.set(func() {
			f.gh.behind["b1"] = []string{"base-x"}
			f.gh.mergeSHA[b.branch] = ""
		})
		fs := &mcqFaultStore{Store: f.store}
		f.srv.cfg.MergeQueue = fs
		held := f.entry(b)
		merges := f.mergesInto(b)
		res := f.handOff(a, b)
		if res.refusal == nil || !strings.Contains(res.refusal.Reason, "resulting head could not be read back") {
			t.Fatalf("B answer = %+v, want the unresolved-head refusal", res)
		}
		if e := f.requireState(b, mergequeue.StateHeld, ""); !e.EnqueuedAt.Equal(held.EnqueuedAt) || e.AnchoredHeadSHA != "b1" {
			t.Fatalf("B = enqueued_at %v head %q, want held at its place (%v) at b1", e.EnqueuedAt, e.AnchoredHeadSHA, held.EnqueuedAt)
		}
		if n := f.mergesInto(b) - merges; n != 1 {
			t.Fatalf("B admission made %d merge calls, want exactly 1", n)
		}
		if n := fs.called("Reanchor"); n != 0 {
			t.Fatalf("Reanchor ran %d times on an unresolved head, want 0", n)
		}
		if n := fs.called("Unadmit"); n != 1 {
			t.Fatalf("Unadmit ran %d times, want 1", n)
		}
		if n := len(f.triggers(b)); n != 0 {
			t.Fatalf("B triggers = %d, want 0", n)
		}
	})

	t.Run("e: re-anchor error returns to held", func(t *testing.T) {
		f := newMCQFixture(t)
		a, b := f.wave()
		f.behind(b, "b1", "b2")
		fs := &mcqFaultStore{Store: f.store,
			reanchor: func(uuid.UUID, string, string) (mergequeue.Entry, error) { return mergequeue.Entry{}, errMCQStore },
		}
		f.srv.cfg.MergeQueue = fs
		held := f.entry(b)
		res := f.handOff(a, b)
		if res.refusal == nil || !strings.Contains(res.refusal.Reason, "re-anchoring the entry failed") {
			t.Fatalf("B answer = %+v, want the re-anchor refusal", res)
		}
		if e := f.requireState(b, mergequeue.StateHeld, ""); !e.EnqueuedAt.Equal(held.EnqueuedAt) {
			t.Fatalf("B enqueued_at moved %v -> %v, want kept", held.EnqueuedAt, e.EnqueuedAt)
		}
		if fs.called("Reanchor") != 1 || fs.called("Unadmit") != 1 {
			t.Fatalf("calls = %v, want one Reanchor and one Unadmit", fs.names())
		}
		if n := len(f.triggers(b)); n != 0 {
			t.Fatalf("B triggers = %d, want 0", n)
		}
	})

	t.Run("f: no installation returns to held", func(t *testing.T) {
		f := newMCQFixture(t)
		a, b := f.wave()
		f.exec(`UPDATE runs SET installation_id = NULL WHERE id = $1`, b.id)
		fs := &mcqFaultStore{Store: f.store}
		f.srv.cfg.MergeQueue = fs
		held := f.entry(b)
		merges := f.mergesInto(b)
		res := f.handOff(a, b)
		if res.refusal == nil || !strings.Contains(res.refusal.Reason, "no installation") {
			t.Fatalf("B answer = %+v, want the no-installation refusal", res)
		}
		if e := f.requireState(b, mergequeue.StateHeld, ""); !e.EnqueuedAt.Equal(held.EnqueuedAt) {
			t.Fatalf("B enqueued_at moved %v -> %v, want kept", held.EnqueuedAt, e.EnqueuedAt)
		}
		if n := fs.called("Unadmit"); n != 1 {
			t.Fatalf("Unadmit ran %d times, want 1", n)
		}
		if n := f.mergesInto(b) - merges; n != 0 {
			t.Fatalf("B admission made %d merge calls with no installation, want 0", n)
		}
		if n := len(f.triggers(b)); n != 0 {
			t.Fatalf("B triggers = %d, want 0", n)
		}
	})

	// g pins TODAY's behaviour: a transient replacement-trigger append failure
	// at admission ejects pass_not_startable (an Unadmit might be friendlier).
	// The settled row persists only the reason code; the detailed refusal is
	// read from the pump's map.
	t.Run("g: pending-replacement trigger append failure ejects pass_not_startable", func(t *testing.T) {
		f := newMCQFixture(t)
		a, b := f.wave()
		f.seedTrigger(b, "b0")
		f.setStage(b.impl.ID, run.StageStatePending)
		fa := &mcqFailTriggerAudit{Repository: f.audit}
		f.srv.cfg.AuditRepo = fa
		fa.armed.Store(true)
		res := f.handOff(a, b)
		f.requireState(b, mergequeue.StateEjected, mergeCandidateQueueReasonPassNotStartable)
		if res.refusal == nil || !strings.Contains(res.refusal.Reason, "recording the replacement merge-candidate verify trigger failed") {
			t.Fatalf("B answer = %+v, want the replacement-trigger refusal", res)
		}
		if tr := f.triggers(b); len(tr) != 1 || tr[0].ExpectedHeadSHA != "b0" {
			t.Fatalf("B triggers = %+v, want still the one seeded at b0", tr)
		}
		if st := f.stageState(b.impl.ID); st != run.StageStatePending {
			t.Fatalf("B implement = %s, want still pending", st)
		}
	})
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

// transferHandOff seeds the held-B wave with b1 passed, switches transfer on, then
// A's pass passes and A merges while the base moved again, so B's admission
// auto-advances b1 → b2.
func (f *mcqFixture) transferHandOff(mode MergeCandidateTransferMode, cls *fakeTransferClassifier) (a, b *mcqRun) {
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
	_, b := f.transferHandOff(MergeCandidateTransferEnforce, cls)

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
	_, b := f.transferHandOff(MergeCandidateTransferShadow, independentClassifier())

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

// S2a (fix-up): in SHADOW a classifier that blocks never delays the admission
// hand-off — the entry goes live and its pass starts while the shadow
// classification is still blocked, and the shadow row lands only after the
// release. Counterfactual: classify on the admission goroutine and the pump
// waits on the classifier — RED.
func TestMergeCandidateQueue_ShadowBlockingClassifierNeverDelaysAdmission(t *testing.T) {
	f := newMCQFixture(t)
	cls := gatedClassifier(t)
	a, b := f.wave()
	f.passedAt(b, "b1")
	f.transfer(MergeCandidateTransferShadow, cls)
	f.srv.cfg.MergeCandidateTransfer.Deadline = 10 * time.Minute
	f.verdict(a, mergeCandidateResultPassed)
	f.behind(b, "b1", "b2")
	runRow, err := f.repo.GetRun(context.Background(), b.id)
	if err != nil {
		t.Fatal(err)
	}
	f.freeLine(a)

	done := make(chan struct{})
	go func() {
		defer close(done)
		f.srv.pumpMergeCandidateQueue(context.Background(), mergequeue.Base{AccountID: runRow.AccountID, Repo: runRow.Repo, BaseRef: mcqBase})
	}()
	select {
	case <-done:
	case <-time.After(timescale.D(20 * time.Second)):
		t.Fatal("the admission hand-off waited on the blocked shadow classification")
	}
	awaitEntered(t, cls)
	if e := f.requireState(b, mergequeue.StateLive, ""); e.AnchoredHeadSHA != "b2" || e.Phase != mergequeue.PhaseVerifying {
		t.Fatalf("B entry = head %q phase %q, want b2 verifying", e.AnchoredHeadSHA, e.Phase)
	}
	if tr := f.triggers(b); len(tr) != 1 || tr[0].ExpectedHeadSHA != "b2" {
		t.Fatalf("B triggers = %+v, want the pass started for b2 before the shadow decision", tr)
	}
	if n := len(f.rows(b, CategoryMergeCandidateVerdictTransferShadow)); n != 0 {
		t.Fatalf("B shadow rows = %d while the classifier is blocked, want 0", n)
	}
	close(cls.gate)
	f.srv.waitMergeCandidateTransferShadows()
	if sh := f.transferPayloads(b, CategoryMergeCandidateVerdictTransferShadow); len(sh) != 1 || !*sh[0].WouldTransfer {
		t.Errorf("B shadow rows after release = %+v, want one would_transfer", sh)
	}
}

// R3 (fix-up), admission arm: the auto-advance's post-merge re-read observes a
// concurrent push, so enforce records not_clean_advance and runs the pass for
// the observed head instead of transferring. Counterfactual: drop the
// new-head == merge-commit check and the verdict transfers — RED.
func TestMergeCandidateQueue_AdmissionConcurrentPushNeverTransfers(t *testing.T) {
	f := newMCQFixture(t)
	cls := independentClassifier()
	a, b := f.wave()
	f.passedAt(b, "b1")
	f.transfer(MergeCandidateTransferEnforce, cls)
	f.verdict(a, mergeCandidateResultPassed)
	f.behind(b, "b1", "b2")
	f.gh.set(func() {
		f.gh.onMerge = func(base string) {
			if base == b.branch {
				f.gh.set(func() { f.gh.prs[b.pr].head = "b2-racer" })
			}
		}
	})
	f.closePR(a, true)

	if e := f.requireState(b, mergequeue.StateLive, ""); e.AnchoredHeadSHA != "b2-racer" || e.Phase != mergequeue.PhaseVerifying {
		t.Fatalf("B entry = head %q phase %q, want b2-racer verifying", e.AnchoredHeadSHA, e.Phase)
	}
	if n := len(f.rows(b, CategoryMergeCandidateVerdictTransferred)); n != 0 {
		t.Fatalf("B transferred rows = %d, want 0 for a concurrent-push head", n)
	}
	if rows := f.transferPayloads(b, CategoryMergeCandidateVerdictTransferRefused); len(rows) != 1 || rows[0].ReasonCode != transferReasonNotCleanAdvance {
		t.Errorf("B refused rows = %+v, want one not_clean_advance", rows)
	}
	if tr := f.triggers(b); len(tr) != 1 || tr[0].ExpectedHeadSHA != "b2-racer" {
		t.Errorf("B triggers = %+v, want one for b2-racer", tr)
	}
	if cls.count() != 0 {
		t.Errorf("classifier calls = %d, want 0", cls.count())
	}
}

// infraParkWave seeds the #4356 shape on the real stores: A holds the line
// (live entry) with a DISPATCHED acceptance stage sequenced before its open
// review stage, B is held behind it, and a REAL orchestrator is wired so the
// reap-failure handler's Advance runs the production park path.
func (f *mcqFixture) infraParkWave() (a, b *mcqRun, acc *run.Stage) {
	f.t.Helper()
	a, b = f.wave()
	// A's admission re-opened implement for the merge-candidate verify pass;
	// settle it and re-park the review gate so acceptance is A's only moving
	// stage, then wire the real orchestrator the reap handler advances.
	f.setStage(a.impl.ID, run.StageStateSucceeded)
	f.setStage(a.review.ID, run.StageStateAwaitingApproval)
	f.srv.cfg.Orchestrator = &orchestrator.Orchestrator{Runs: f.repo, Audit: f.audit}
	acc, err := f.repo.CreateStage(context.Background(), run.CreateStageParams{
		RunID: a.id, Sequence: 4, Type: run.StageTypeAcceptance, ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code",
	})
	if err != nil {
		f.t.Fatalf("create acceptance stage: %v", err)
	}
	f.setStage(acc.ID, run.StageStateDispatched)
	// feature_change order: acceptance precedes the review stage.
	f.exec(`UPDATE stages SET sequence = 5 WHERE id = $1`, a.review.ID)
	return a, b, acc
}

// reapAcceptanceInfra posts the category-C acceptance_preview_provision_failed
// reap for acc through the real handler.
func (f *mcqFixture) reapAcceptanceInfra(a *mcqRun, acc *run.Stage) {
	f.t.Helper()
	rec := postReapFailure(f.t, f.srv, a.id, acc.ID, reapFailureRequest{
		Category: "C", Reason: "acceptance_preview_provision_failed",
	}, withReapOperator)
	if rec.Code != http.StatusOK {
		f.t.Fatalf("reap-failure = %d: %s", rec.Code, rec.Body.String())
	}
}

func (f *mcqFixture) runState(r *mcqRun) run.State {
	f.t.Helper()
	row, err := f.repo.GetRun(context.Background(), r.id)
	if err != nil {
		f.t.Fatalf("get run: %v", err)
	}
	return row.State
}

func (f *mcqFixture) settledRunTerminal(r *mcqRun) int {
	f.t.Helper()
	n := 0
	for _, e := range f.rows(r, CategoryMergeCandidateQueueSettled) {
		if strings.Contains(string(e.Payload), mergeCandidateQueueReasonRunTerminal) {
			n++
		}
	}
	return n
}

// TestMergeCandidateQueue_AcceptanceInfraFailureKeepsLiveEntry is the #4356
// cross-layer test: request handler (reap-failure) -> orchestrator.Advance ->
// Postgres run/stage state -> the merge-candidate pump. A category-C acceptance
// failure on the line holder parks the run (running, stage failed/C), so the
// pump's run_terminal drop never fires: A keeps its live entry with the SAME
// phase and B stays held. Mechanism: without the Advance park branch the reap
// handler's Advance completes A failed and the pump drops A run_terminal and
// admits B.
func TestMergeCandidateQueue_AcceptanceInfraFailureKeepsLiveEntry(t *testing.T) {
	f := newMCQFixture(t)
	a, b, acc := f.infraParkWave()
	phaseBefore := f.requireState(a, mergequeue.StateLive, "").Phase

	f.reapAcceptanceInfra(a, acc)

	if got := f.runState(a); got != run.StateRunning {
		t.Fatalf("run A state = %s, want running (parked)", got)
	}
	st, err := f.repo.GetStage(context.Background(), acc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if st.State != run.StageStateFailed || st.FailureCategory == nil || *st.FailureCategory != run.FailureC {
		t.Fatalf("acceptance stage = %s/%v, want failed/C", st.State, st.FailureCategory)
	}
	if n := len(f.rows(a, run.CategoryStageInfraParked)); n != 1 {
		t.Fatalf("stage_infra_parked rows = %d, want 1", n)
	}

	f.srv.PumpMergeCandidateQueues(context.Background())

	if e := f.requireState(a, mergequeue.StateLive, ""); e.Phase != phaseBefore {
		t.Fatalf("A phase = %s, want unchanged %s", e.Phase, phaseBefore)
	}
	f.requireState(b, mergequeue.StateHeld, "")
	if n := f.settledRunTerminal(a); n != 0 {
		t.Fatalf("A run_terminal settle rows = %d, want 0", n)
	}

	// The parked stage stays retry-eligible: failed -> pending.
	if _, err := run.RetryStage(context.Background(), f.repo, acc.ID, run.RetryOptions{}); err != nil {
		t.Fatalf("RetryStage on the parked acceptance stage: %v", err)
	}
	if got := f.stageState(acc.ID); got != run.StageStatePending {
		t.Fatalf("acceptance stage after retry = %s, want pending", got)
	}
}

// TestMergeCandidateQueue_AcceptanceInfraBudgetExhaustedDropsRunTerminal is the
// budget twin: with three earlier-episode parks already recorded for the stage,
// the 4th consecutive category-C failure fails the run with one
// stage_infra_park_budget_exhausted row, and the pump drops A run_terminal and
// admits B — a genuinely terminal run still settles run_terminal.
func TestMergeCandidateQueue_AcceptanceInfraBudgetExhaustedDropsRunTerminal(t *testing.T) {
	f := newMCQFixture(t)
	a, b, acc := f.infraParkWave()
	sys := audit.ActorSystem
	for i := 0; i < run.InfraFailureParkRetryBudget; i++ {
		payload, _ := json.Marshal(map[string]any{"failed_at": fmt.Sprintf("2026-01-0%dT00:00:00Z", i+1), "retry_ordinal": i})
		stageID := acc.ID
		if _, err := f.audit.AppendChained(context.Background(), audit.ChainAppendParams{
			RunID: a.id, StageID: &stageID, Timestamp: time.Now().UTC(), Category: run.CategoryStageInfraParked, ActorKind: &sys, Payload: payload,
		}); err != nil {
			t.Fatal(err)
		}
	}

	f.reapAcceptanceInfra(a, acc)

	if got := f.runState(a); got != run.StateFailed {
		t.Fatalf("run A state = %s, want failed (budget exhausted)", got)
	}
	if n := len(f.rows(a, run.CategoryStageInfraParkBudgetExhausted)); n != 1 {
		t.Fatalf("stage_infra_park_budget_exhausted rows = %d, want 1", n)
	}

	f.srv.PumpMergeCandidateQueues(context.Background())

	f.requireState(a, mergequeue.StateDropped, mergeCandidateQueueReasonRunTerminal)
	f.requireState(b, mergequeue.StateLive, "")
}

// ---- Re-open release (#4352) ----

// gate restores r's stages to the review gate BY CONSTRUCTION: implement
// succeeded, review awaiting_approval — the state a passed verify pass or a
// no-op fix-up leaves in production (the fixture's runner never settles them).
func (f *mcqFixture) gate(r *mcqRun) {
	f.t.Helper()
	f.setStage(r.impl.ID, run.StageStateSucceeded)
	f.setStage(r.review.ID, run.StageStateAwaitingApproval)
}

// fixup routes one fix-up on r's implement stage through the REAL
// fixupStageAs (and so the real run.FixupStage).
func (f *mcqFixture) fixup(r *mcqRun) error {
	f.t.Helper()
	_, _, err := f.srv.fixupStageAs(context.Background(),
		Identity{Subject: "operator-test", Scopes: []string{"write:fixups"}},
		fixupActionParams{
			StageID: r.impl.ID,
			Options: run.FixupOptions{MaxPasses: 2, HardCeiling: 3},
			Selected: []planreview.Concern{{Severity: planreview.SeverityHigh, Category: "security",
				Note: "tighten the guard"}},
			Reason: "route the review concern",
		})
	return err
}

// settledRows decodes r's merge_candidate_queue_settled payloads.
func (f *mcqFixture) settledRows(r *mcqRun) []map[string]any {
	f.t.Helper()
	var out []map[string]any
	for _, e := range f.rows(r, CategoryMergeCandidateQueueSettled) {
		var p map[string]any
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			f.t.Fatalf("decode settled row: %v", err)
		}
		out = append(out, p)
	}
	return out
}

// passedWave is wave with A's pass PASSED at a1 (A live, phase passed; B held)
// and A's stages back at the review gate.
func (f *mcqFixture) passedWave() (a, b *mcqRun) {
	f.t.Helper()
	a, b = f.wave()
	f.verdict(a, mergeCandidateResultPassed)
	if e := f.requireState(a, mergequeue.StateLive, ""); e.Phase != mergequeue.PhasePassed || e.AnchoredHeadSHA != "a1" {
		f.t.Fatalf("fixture: A entry = phase %q head %q, want passed at a1", e.Phase, e.AnchoredHeadSHA)
	}
	f.gate(a)
	return a, b
}

// TestMergeCandidateQueue_FixupReopenReleasesTheLine (#4352): a fix-up routed
// on the LIVE, passed line holder drops its entry reopened_for_fixup and the
// held entry behind it is admitted at once, not after the agent pass.
func TestMergeCandidateQueue_FixupReopenReleasesTheLine(t *testing.T) {
	f := newMCQFixture(t)
	a, b := f.passedWave()
	if n := len(f.triggers(b)); n != 0 {
		t.Fatalf("fixture: B triggers = %d before the fix-up, want 0", n)
	}

	if err := f.fixup(a); err != nil {
		t.Fatalf("fixupStageAs: %v", err)
	}

	f.requireState(a, mergequeue.StateDropped, mergeCandidateQueueReasonReopenedForFixup)
	rows := f.settledRows(a)
	if len(rows) != 1 {
		t.Fatalf("A settled rows = %d, want exactly 1", len(rows))
	}
	if r := rows[0]; r["reason"] != mergeCandidateQueueReasonReopenedForFixup || r["from_state"] != string(mergequeue.StateLive) ||
		r["phase"] != string(mergequeue.PhasePassed) || r["anchored_head_sha"] != "a1" {
		t.Fatalf("A settled row = %+v, want reason %s from_state live phase passed at a1", r, mergeCandidateQueueReasonReopenedForFixup)
	}
	f.requireState(b, mergequeue.StateLive, "")
	if tr := f.triggers(b); len(tr) != 1 || tr[0].ExpectedHeadSHA != "b1" {
		t.Fatalf("B triggers = %+v, want exactly one at b1 (admitted without waiting for A)", tr)
	}

	t.Run("after verify_failed eject is a no-op", func(t *testing.T) {
		f := newMCQFixture(t)
		a, b := f.wave()
		f.verdict(a, mergeCandidateResultFailed)
		f.requireState(a, mergequeue.StateEjected, mergeCandidateQueueReasonVerifyFailed)
		f.requireState(b, mergequeue.StateLive, "")
		f.gate(a)
		if err := f.fixup(a); err != nil {
			t.Fatalf("fixupStageAs: %v", err)
		}
		f.requireState(a, mergequeue.StateEjected, mergeCandidateQueueReasonVerifyFailed)
		if n := len(f.settledRows(a)); n != 1 {
			t.Fatalf("A settled rows = %d, want exactly 1 (the verify_failed eject)", n)
		}
		f.requireState(b, mergequeue.StateLive, "")
	})
}

// TestMergeCandidateQueue_RefusedFixupKeepsTheLine: a fix-up run.FixupStage
// REFUSES (the implement stage is left pending, as the verify re-open left it)
// releases nothing — the release runs only after the re-open is admitted.
//
// Ordering counterfactual (approval condition C1), run by hand: the release
// was MOVED above run.FixupStage in fixupStageAs, keyed by the run id from a
// stage read on p.StageID (`if st0, gerr := s.cfg.RunRepo.GetStage(ctx,
// p.StageID); gerr == nil { s.releaseMergeCandidateQueueOnReopen(...,
// st0.RunID, ...) }`), and the original call removed. It compiled, and this
// test went RED: "run <A> entry = dropped/"reopened_for_fixup", want
// live/""". Restored byte-identically, GREEN.
func TestMergeCandidateQueue_RefusedFixupKeepsTheLine(t *testing.T) {
	f := newMCQFixture(t)
	a, b := f.wave()
	f.verdict(a, mergeCandidateResultPassed)
	if st := f.stageState(a.impl.ID); st != run.StageStatePending {
		t.Fatalf("fixture: A implement = %s, want pending (the verify re-open's state)", st)
	}

	err := f.fixup(a)
	if !errors.Is(err, run.ErrFixupNotApplicable) {
		t.Fatalf("fixupStageAs = %v, want ErrFixupNotApplicable", err)
	}
	if e := f.requireState(a, mergequeue.StateLive, ""); e.Phase != mergequeue.PhasePassed {
		t.Fatalf("A phase = %q, want passed (untouched)", e.Phase)
	}
	if st, _ := f.srv.mergeCandidateQueueForRun(context.Background(), b.id); st == nil || st.Entry.State != mergequeue.StateHeld ||
		st.Position != 1 {
		t.Fatalf("B status = %+v, want held at position 1", st)
	}
	if n := len(f.settledRows(a)); n != 0 {
		t.Fatalf("A settled rows = %d, want 0 on a refused fix-up", n)
	}
}

// TestMergeCandidateQueue_FixupReopenDropsHeldEntry: a fix-up on a HELD entry
// drops it (an admission mid-fix-up would auto-advance the branch under the
// running agent); a held drop frees nothing and pumps nothing.
func TestMergeCandidateQueue_FixupReopenDropsHeldEntry(t *testing.T) {
	f := newMCQFixture(t)
	a, b, c := f.waveABC()
	mergesBefore := f.mergesInto(c)

	if err := f.fixup(c); err != nil {
		t.Fatalf("fixupStageAs: %v", err)
	}

	f.requireState(c, mergequeue.StateDropped, mergeCandidateQueueReasonReopenedForFixup)
	if rows := f.settledRows(c); len(rows) != 1 || rows[0]["from_state"] != string(mergequeue.StateHeld) {
		t.Fatalf("C settled rows = %+v, want one from_state held", rows)
	}
	f.requireState(a, mergequeue.StateLive, "")
	f.requireState(b, mergequeue.StateHeld, "")

	f.handOff(a, b)
	f.requireState(b, mergequeue.StateLive, "")
	if st, err := f.srv.mergeCandidateQueueForRun(context.Background(), c.id); err != nil || st != nil {
		t.Fatalf("C active entry = %+v / %v, want none", st, err)
	}
	if n := f.mergesInto(c); n != mergesBefore {
		t.Fatalf("merges into C's branch = %d, want unchanged %d (no auto-advance under the agent)", n, mergesBefore)
	}
}

// TestMergeCandidateQueue_FixupNoOpKeepsPassedVerdict: the release never
// touches the verdict. A fix-up that delivered nothing (head still a1, stages
// back at the gate) still reads passed, and an up-to-date rebase starts no pass.
func TestMergeCandidateQueue_FixupNoOpKeepsPassedVerdict(t *testing.T) {
	f := newMCQFixture(t)
	a, _ := f.passedWave()
	if err := f.fixup(a); err != nil {
		t.Fatalf("fixupStageAs: %v", err)
	}
	f.gate(a)

	runRow, err := f.repo.GetRun(context.Background(), a.id)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	st, err := f.srv.mergeCandidateVerifyState(context.Background(), runRow, "a1")
	if err != nil || st.State != mergeCandidateStatePassed {
		t.Fatalf("A verify state at a1 = %+v / %v, want passed", st, err)
	}
	resp := f.rebase(a)
	if resp.MergeCandidateVerifyState != mergeCandidateStatePassed || resp.MergeCandidateVerifyTriggered {
		t.Fatalf("A up-to-date rebase = state %q triggered %v, want passed, not triggered",
			resp.MergeCandidateVerifyState, resp.MergeCandidateVerifyTriggered)
	}
	if n := len(f.triggers(a)); n != 1 {
		t.Fatalf("A triggers = %d, want unchanged 1", n)
	}
	if st, err := f.srv.mergeCandidateQueueForRun(context.Background(), a.id); err != nil || st != nil {
		t.Fatalf("A active entry = %+v / %v, want none", st, err)
	}
	if rows := f.settledRows(a); len(rows) != 1 || rows[0]["reason"] != mergeCandidateQueueReasonReopenedForFixup {
		t.Fatalf("A settled rows = %+v, want one naming %s", rows, mergeCandidateQueueReasonReopenedForFixup)
	}
}

// TestMergeCandidateQueue_ReleasedHolderReentersAtTail (ADR-093 Q2): after the
// release, the fix-up's pushed head needs a pass; the run's rebase enqueues it
// HELD behind the entry that took the line, not back at the head.
func TestMergeCandidateQueue_ReleasedHolderReentersAtTail(t *testing.T) {
	f := newMCQFixture(t)
	a, b := f.passedWave()
	if err := f.fixup(a); err != nil {
		t.Fatalf("fixupStageAs: %v", err)
	}
	f.requireState(b, mergequeue.StateLive, "")
	f.gh.set(func() { f.gh.prs[a.pr].head = "a2" })
	f.gate(a)
	f.behind(a, "a2", "a3")

	resp := f.rebase(a)
	if resp.MergeCandidateVerifyState != mergeCandidateStateHeld || resp.MergeCandidateVerifyTriggered {
		t.Fatalf("A rebase = state %q triggered %v, want held, not triggered",
			resp.MergeCandidateVerifyState, resp.MergeCandidateVerifyTriggered)
	}
	if e := f.requireState(a, mergequeue.StateHeld, ""); e.AnchoredHeadSHA != "a3" {
		t.Fatalf("A newest entry head = %q, want a3", e.AnchoredHeadSHA)
	}
	if st, _ := f.srv.mergeCandidateQueueForRun(context.Background(), a.id); st == nil || st.Position != 1 ||
		st.LiveRunID == nil || *st.LiveRunID != b.id {
		t.Fatalf("A status = %+v, want held at position 1 behind B", st)
	}
	f.requireState(b, mergequeue.StateLive, "")
}

// TestMergeCandidateQueue_ConflictResolutionReopenReleasesTheLine: a rebase
// of the live holder that conflicts re-opens it for a conflict-resolution
// pass, which drops its entry and admits the held entry behind it.
func TestMergeCandidateQueue_ConflictResolutionReopenReleasesTheLine(t *testing.T) {
	f := newMCQFixture(t)
	a, b := f.passedWave()
	f.gh.set(func() {
		f.gh.behind["a1"] = []string{"base-x"}
		f.gh.mergeStatus[a.branch] = http.StatusConflict
	})

	w := postRebaseBranch(t, f.srv, a.id, rebaseBranchRequest{Confirm: true, Reason: "base moved"}, withRebaseOperator)
	if w.Code != http.StatusAccepted {
		t.Fatalf("rebase = %d, want 202:\n%s", w.Code, w.Body.String())
	}
	var resp rebaseBranchResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || !resp.ConflictResolutionTriggered {
		t.Fatalf("rebase body = %+v / %v, want conflict_resolution_triggered", resp, err)
	}
	f.requireState(a, mergequeue.StateDropped, mergeCandidateQueueReasonReopenedForConflictResolution)
	f.requireState(b, mergequeue.StateLive, "")
	if n := len(f.triggers(b)); n != 1 {
		t.Fatalf("B triggers = %d, want 1", n)
	}
}

// ADR-093 slice 1 (E87.4 / #4295): eligibility decides QUEUE ADMISSION.

// TestMergeCandidateQueue_UnapprovedRebaseDoesNotTakeLineApprovedBehindDoes:
// A (unapproved) and B (approved by a listed member) are both behind on one
// base and otherwise identical. A's rebase-path serialize answers not_queued
// and writes no entry; B's is admitted live.
func TestMergeCandidateQueue_UnapprovedRebaseDoesNotTakeLineApprovedBehindDoes(t *testing.T) {
	f := newMCQFixture(t)
	a := f.seed("a", "a0", []byte(mqeSpecYAML))
	b := f.seed("b", "b0", []byte(mqeSpecYAML))
	f.behind(a, "a0", "a1")
	f.behind(b, "b0", "b1")
	f.gh.set(func() { f.gh.reviews[b.pr] = mqeApprovedBy("alice") })

	resp := f.rebase(a)
	if resp.MergeCandidateVerifyState != mergeCandidateStateNotQueued || resp.MergeCandidateVerifyTriggered ||
		!strings.Contains(resp.MergeCandidateVerifyNote, mergeQueueReasonNotApproved) ||
		!strings.Contains(resp.MergeCandidateVerifyNote, "fishhawk_merge_run") {
		t.Fatalf("A rebase = state %q triggered=%v note %q, want not_queued naming not_approved and fishhawk_merge_run",
			resp.MergeCandidateVerifyState, resp.MergeCandidateVerifyTriggered, resp.MergeCandidateVerifyNote)
	}
	if es := f.entries(a); len(es) != 0 {
		t.Fatalf("A queue entries = %d, want 0", len(es))
	}
	if st := f.stageState(a.impl.ID); st != run.StageStateSucceeded {
		t.Fatalf("A implement = %s, want succeeded (no re-open on not_queued)", st)
	}
	if resp := f.rebase(b); resp.MergeCandidateVerifyState != mergeCandidateStateInFlight || !resp.MergeCandidateVerifyTriggered {
		t.Fatalf("B rebase = state %q triggered=%v, want in_flight", resp.MergeCandidateVerifyState, resp.MergeCandidateVerifyTriggered)
	}
	f.requireState(b, mergequeue.StateLive, "")
}

// enqueueDirect seeds a HELD entry through the store, bypassing every enqueue
// control.
func (f *mcqFixture) enqueueDirect(r *mcqRun, head string) {
	f.t.Helper()
	if _, err := f.store.Enqueue(context.Background(), mergequeue.EnqueueRequest{RunID: r.id, BaseRef: mcqBase, HeadSHA: head}); err != nil {
		f.t.Fatalf("enqueue: %v", err)
	}
}

// TestMergeCandidateQueue_AdmissionDropsIneligibleHeldEntry: A (unapproved)
// and B (approved) are seeded HELD directly, A at the head of line. One pump
// drops A not_merge_eligible and admits B.
func TestMergeCandidateQueue_AdmissionDropsIneligibleHeldEntry(t *testing.T) {
	f := newMCQFixture(t)
	a := f.seed("a", "a0", []byte(mqeSpecYAML))
	b := f.seed("b", "b0", []byte(mqeSpecYAML))
	f.gh.set(func() { f.gh.reviews[b.pr] = mqeApprovedBy("alice") })
	f.enqueueDirect(a, "a0")
	f.enqueueDirect(b, "b0")
	f.srv.PumpMergeCandidateQueues(context.Background())
	f.requireState(a, mergequeue.StateDropped, mergeCandidateQueueReasonNotMergeEligible)
	if n := len(f.rows(a, CategoryMergeCandidateQueueSettled)); n != 1 {
		t.Fatalf("A settled rows = %d, want 1", n)
	}
	f.requireState(b, mergequeue.StateLive, "")
}

// TestMergeCandidateQueue_AdmissionUndeterminableUnadmits: an unreadable
// reviews listing returns the head of line to held at its place.
func TestMergeCandidateQueue_AdmissionUndeterminableUnadmits(t *testing.T) {
	f := newMCQFixture(t)
	a := f.seed("a", "a0", []byte(mqeSpecYAML))
	b := f.seed("b", "b0", []byte(mqeSpecYAML))
	f.gh.set(func() {
		f.gh.reviewsStatus[a.pr] = http.StatusInternalServerError
		f.gh.reviews[b.pr] = mqeApprovedBy("alice")
	})
	f.enqueueDirect(a, "a0")
	f.enqueueDirect(b, "b0")
	f.srv.PumpMergeCandidateQueues(context.Background())
	f.requireState(a, mergequeue.StateHeld, "")
	f.requireState(b, mergequeue.StateHeld, "")
	st, found, err := f.store.ActiveForRun(context.Background(), a.id)
	if err != nil || !found || st.Position != 1 {
		t.Fatalf("A status = %+v found=%v err=%v, want held at position 1", st, found, err)
	}
}

// TestMergeCandidateQueue_LiveEntryNotReevaluated: a live entry whose
// approval is dismissed afterwards keeps the line (slice 1 re-evaluates only
// at admission).
func TestMergeCandidateQueue_LiveEntryNotReevaluated(t *testing.T) {
	f := newMCQFixture(t)
	a := f.seed("a", "a0", []byte(mqeSpecYAML))
	f.gh.set(func() { f.gh.reviews[a.pr] = mqeApprovedBy("alice") })
	f.enqueueDirect(a, "a0")
	f.srv.PumpMergeCandidateQueues(context.Background())
	f.requireState(a, mergequeue.StateLive, "")
	f.gh.set(func() { f.gh.reviews[a.pr] = `[]` })
	f.srv.PumpMergeCandidateQueues(context.Background())
	f.requireState(a, mergequeue.StateLive, "")
}

// TestMCQFake_ReviewSnapshotAndPermissionRoutes exercises every ADR-112 route
// and helper the mcqGitHub fake gained, through the PRODUCTION githubclient:
// the branch-ref tip, the recorded merge-commit parents (and the wrong-parent
// and status knobs), the review snapshot (fixture decode, writersOnly
// IGNORED, truncation, failure budget, call counter, non-snapshot query 404)
// and the collaborator permission read (listed, unlisted 404, status knob,
// per-login counter).
func TestMCQFake_ReviewSnapshotAndPermissionRoutes(t *testing.T) {
	f := newMCQFixture(t)
	gh := f.srv.cfg.GitHub
	ctx := context.Background()
	scope := forge.FromGitHubInstallationID(1)
	repo := forge.RepoRef{Owner: "x", Name: "y"}
	f.gh.set(func() { f.gh.prs[5] = &mcqPR{head: "h1", branch: "fishhawk/run/five", state: "open"} })

	if tip, found, err := gh.GetBranchSHA(ctx, scope, repo, mcqBase); err != nil || !found || tip != mcqBaseTip(mcqBase) {
		t.Fatalf("base tip = %q %v %v, want %s", tip, found, err, mcqBaseTip(mcqBase))
	}
	f.gh.set(func() { f.gh.mergeSHA["fishhawk/run/five"] = "m1" })
	if sha, err := gh.MergeBranch(ctx, scope, repo, "fishhawk/run/five", mcqBase, "advance"); err != nil || sha != "m1" {
		t.Fatalf("merge = %q %v", sha, err)
	}
	if c, err := gh.GetCommit(ctx, scope, repo, "m1"); err != nil || !reflect.DeepEqual(c.Parents, []string{"h1", mcqBaseTip(mcqBase)}) {
		t.Fatalf("merge commit parents = %+v %v, want [h1 %s]", c, err, mcqBaseTip(mcqBase))
	}
	f.gh.set(func() {
		f.gh.wrongParent["fishhawk/run/five"] = "wrong"
		f.gh.mergeSHA["fishhawk/run/five"] = "m2"
	})
	if _, err := gh.MergeBranch(ctx, scope, repo, "fishhawk/run/five", mcqBase, "advance"); err != nil {
		t.Fatalf("merge 2: %v", err)
	}
	if c, err := gh.GetCommit(ctx, scope, repo, "m2"); err != nil || !reflect.DeepEqual(c.Parents, []string{"m1", "wrong"}) {
		t.Fatalf("wrong-parent knob = %+v %v, want [m1 wrong]", c, err)
	}
	if _, err := gh.GetCommit(ctx, scope, repo, "unknown"); !errors.Is(err, githubclient.ErrNotFound) {
		t.Errorf("unrecorded commit err = %v, want ErrNotFound", err)
	}
	f.gh.set(func() {
		f.gh.commitStatus = http.StatusInternalServerError
		f.gh.baseTipStatus[mcqBase] = http.StatusInternalServerError
	})
	if _, err := gh.GetCommit(ctx, scope, repo, "m1"); err == nil {
		t.Error("commitStatus knob did not fail the read")
	}
	if _, _, err := gh.GetBranchSHA(ctx, scope, repo, mcqBase); err == nil {
		t.Error("baseTipStatus knob did not fail the read")
	}

	// Review snapshot: default fixture.
	snap, err := gh.PullRequestReviewSnapshot(ctx, scope, repo, 5)
	if err != nil || len(snap.Reviews) != 0 || snap.ReviewDecision != "" || snap.HeadOID != "m2" || snap.Merged ||
		snap.Author.Typename != "User" {
		t.Fatalf("default snapshot = %+v %v", snap, err)
	}
	at := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	f.gh.setReviewSnapshot(5, mcqReviewSnapshot{
		Reviews: []mcqReviewNode{
			{ID: 11, State: "APPROVED", SubmittedAt: at, Commit: "m2", Author: mcqActor{Typename: "User", Login: "reader", ID: 7}},
			{ID: 12, State: "DISMISSED", SubmittedAt: at, Commit: "h1", Author: mcqActor{Typename: "Bot", Login: "ci[bot]"}},
			{ID: 13, State: "CHANGES_REQUESTED", SubmittedAt: at},
		},
		ReviewDecision: "REVIEW_REQUIRED", MergeStateStatus: "BLOCKED",
		Author: mcqActor{Typename: "Bot", Login: "dependabot[bot]"}, IsPrivate: true,
	})
	snap, err = gh.PullRequestReviewSnapshot(ctx, scope, repo, 5)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	// writersOnly is IGNORED: a read-permission reviewer's node is served.
	if len(snap.Reviews) != 3 || snap.Reviews[0].Author.Login != "reader" || snap.Reviews[0].Author.DatabaseID != 7 ||
		snap.Reviews[1].Author.Typename != "Bot" || snap.Reviews[2].Author.Present || snap.Reviews[2].CommitOID != "" ||
		!snap.Reviews[0].SubmittedAt.Equal(at) {
		t.Fatalf("fixture reviews = %+v", snap.Reviews)
	}
	if snap.ReviewDecision != "REVIEW_REQUIRED" || snap.MergeStateStatus != "BLOCKED" || !snap.IsPrivate || snap.Author.Typename != "Bot" {
		t.Fatalf("fixture pr fields = %+v", snap)
	}
	f.gh.set(func() { f.gh.reviewSnapshot[5].Truncated = true })
	before := f.gh.snapshotCalls()
	if snap, err = gh.PullRequestReviewSnapshot(ctx, scope, repo, 5); err != nil || !snap.Truncated {
		t.Fatalf("truncated snapshot = %+v %v", snap, err)
	}
	if n := f.gh.snapshotCalls() - before; n != 3 {
		t.Errorf("truncated walk calls = %d, want 3 (the page cap)", n)
	}
	f.gh.set(func() { f.gh.graphQLFail[5] = 1 })
	if _, err := gh.PullRequestReviewSnapshot(ctx, scope, repo, 5); err == nil {
		t.Error("graphQLFail budget did not fail the call")
	}
	if _, err := gh.PullRequestReviewSnapshot(ctx, scope, repo, 999); !errors.Is(err, githubclient.ErrNotFound) {
		t.Errorf("unknown PR err = %v, want ErrNotFound", err)
	}
	if _, err := gh.ListRepoIssues(ctx, scope, repo, githubclient.ListRepoIssuesOptions{}); err == nil {
		t.Error("a non-snapshot GraphQL query was answered; want 404")
	}

	// Permission read.
	f.gh.setPermission("writer", "maintain", "write")
	if p, err := gh.CollaboratorPermission(ctx, scope, repo, "writer"); err != nil || p.EffectiveRole() != "maintain" {
		t.Fatalf("listed permission = %+v %v", p, err)
	}
	if _, err := gh.CollaboratorPermission(ctx, scope, repo, "stranger"); !errors.Is(err, githubclient.ErrNotFound) {
		t.Errorf("unlisted login err = %v, want ErrNotFound", err)
	}
	f.gh.set(func() { f.gh.permStatus["writer"] = http.StatusInternalServerError })
	if _, err := gh.CollaboratorPermission(ctx, scope, repo, "writer"); err == nil {
		t.Error("permStatus knob did not fail the read")
	}
	if n := f.gh.permissionCalls("writer"); n != 2 {
		t.Errorf("permission calls for writer = %d, want 2", n)
	}
}

// Mode (4), ADR-093 slice 3 (#4297): an auto-advance conflict ejects
// advance_conflict, records ONE merge_queue_ejected row handing off to the
// conflict-resolution path by NAMING fishhawk_rebase_run_branch, and admits the
// next held entry. The hand-off is named, never auto-started: no
// stage_conflict_resolution_triggered row.
func TestAdmission_AdvanceConflictEjectsNamingRebase(t *testing.T) {
	f := newMCQFixture(t)
	a, b, c := f.waveABC()
	f.gh.set(func() {
		f.gh.behind["b1"] = []string{"base-x"}
		f.gh.mergeStatus[b.branch] = http.StatusConflict
	})
	f.closePR(a, true)
	f.requireState(b, mergequeue.StateEjected, mergeCandidateQueueReasonAdvanceConflict)
	f.oneEjection(b, mergeCandidateQueueReasonAdvanceConflict, mergeQueueEjectNextRebaseRunBranch)
	f.requireState(c, mergequeue.StateLive, "")
	if n := len(f.rows(b, CategoryStageConflictResolutionTriggered)); n != 0 {
		t.Fatalf("stage_conflict_resolution_triggered rows = %d, want 0 (the hand-off is named, not started)", n)
	}
}

// Mode (6): a pass that cannot start ejects pass_not_startable and records
// ONE row naming fishhawk_get_run_status.
func TestAdmission_PassNotStartableRecordsEjection(t *testing.T) {
	f := newMCQFixture(t)
	a, b := f.wave()
	f.setStage(b.impl.ID, run.StageStateRunning)
	f.closePR(a, true)
	f.requireState(b, mergequeue.StateEjected, mergeCandidateQueueReasonPassNotStartable)
	p := f.oneEjection(b, mergeCandidateQueueReasonPassNotStartable, mergeQueueEjectNextGetRunStatus)
	if _, ok := p["fixup_routed"]; ok {
		t.Fatalf("a non-verify ejection carries fixup_routed: %v", p)
	}
}
