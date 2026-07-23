package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/account"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// seedRun inserts a run with controlled fields directly into the
// fake's map so list/cancel tests don't depend on POST /v0/runs.
func seedRun(repo *fakeRepo, repoName, workflowID string, state run.State, createdAt time.Time) *run.Run {
	r := &run.Run{
		ID:            uuid.New(),
		Repo:          repoName,
		WorkflowID:    workflowID,
		WorkflowSHA:   "sha-" + string(state),
		TriggerSource: run.TriggerCLI,
		State:         state,
		CreatedAt:     createdAt,
		UpdatedAt:     createdAt,
	}
	repo.mu.Lock()
	repo.runs[r.ID] = r
	repo.mu.Unlock()
	return r
}

func strPtr(s string) *string { return &s }

func TestListRuns_HappyPath(t *testing.T) {
	repo := newFakeRepo()
	t0 := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	seedRun(repo, "x/y", "feature_change", run.StatePending, t0)
	seedRun(repo, "x/y", "feature_change", run.StateRunning, t0.Add(time.Second))
	seedRun(repo, "a/b", "hotfix", run.StateSucceeded, t0.Add(2*time.Second))
	s := newServer(t, repo)

	req := httptest.NewRequest(http.MethodGet, "/v0/runs", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
	}
	var got struct {
		Items      []runResponse `json:"items"`
		NextCursor string        `json:"next_cursor"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Items) != 3 {
		t.Errorf("items = %d, want 3", len(got.Items))
	}
	// created_at DESC: most-recently created comes first.
	if got.Items[0].State != string(run.StateSucceeded) {
		t.Errorf("first state = %q, want succeeded", got.Items[0].State)
	}
	if got.NextCursor != "" {
		t.Errorf("next_cursor = %q, want empty", got.NextCursor)
	}
}

func TestListRuns_RepoFilter(t *testing.T) {
	repo := newFakeRepo()
	t0 := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	seedRun(repo, "x/y", "w", run.StatePending, t0)
	seedRun(repo, "a/b", "w", run.StatePending, t0)
	s := newServer(t, repo)

	req := httptest.NewRequest(http.MethodGet, "/v0/runs?repo=x/y", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var got struct {
		Items []runResponse `json:"items"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if len(got.Items) != 1 || got.Items[0].Repo != "x/y" {
		t.Errorf("repo filter broken: %+v", got.Items)
	}
}

func TestListRuns_PullRequestURLFilter(t *testing.T) {
	// Threaded-runs view (#216) filters by pull_request_url to find
	// every run on a PR.
	repo := newFakeRepo()
	t0 := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	target := "https://github.com/x/y/pull/42"
	r1 := seedRun(repo, "x/y", "w", run.StateRunning, t0)
	r1.PullRequestURL = strPtr(target)
	r2 := seedRun(repo, "x/y", "w", run.StateSucceeded, t0.Add(time.Minute))
	r2.PullRequestURL = strPtr(target)
	other := seedRun(repo, "x/y", "w", run.StateRunning, t0.Add(2*time.Minute))
	other.PullRequestURL = strPtr("https://github.com/x/y/pull/99")

	s := newServer(t, repo)

	req := httptest.NewRequest(http.MethodGet, "/v0/runs?pull_request_url="+target, nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var got struct {
		Items []runResponse `json:"items"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if len(got.Items) != 2 {
		t.Fatalf("filter returned %d items, want 2; items=%+v", len(got.Items), got.Items)
	}
	for _, it := range got.Items {
		if it.PullRequestURL == nil || *it.PullRequestURL != target {
			t.Errorf("filtered row has PullRequestURL = %v, want %s", it.PullRequestURL, target)
		}
	}
}

func TestListRuns_TriggerRefFilter(t *testing.T) {
	// Threaded-runs view (#216) also filters by trigger_ref so the
	// dispatcher's parent-finder + the SPA's "all runs on this
	// issue" view share the same query path.
	repo := newFakeRepo()
	t0 := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	r1 := seedRun(repo, "x/y", "w", run.StateRunning, t0)
	r1.TriggerRef = strPtr("issue:42")
	r2 := seedRun(repo, "x/y", "w", run.StateSucceeded, t0.Add(time.Minute))
	r2.TriggerRef = strPtr("issue:99")

	s := newServer(t, repo)
	req := httptest.NewRequest(http.MethodGet, "/v0/runs?trigger_ref=issue:42", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var got struct {
		Items []runResponse `json:"items"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if len(got.Items) != 1 {
		t.Fatalf("filter returned %d items, want 1; items=%+v", len(got.Items), got.Items)
	}
	if got.Items[0].TriggerRef == nil || *got.Items[0].TriggerRef != "issue:42" {
		t.Errorf("filtered row TriggerRef = %v", got.Items[0].TriggerRef)
	}
}

func TestListRuns_StateFilter_BadValue(t *testing.T) {
	s := newServer(t, newFakeRepo())
	req := httptest.NewRequest(http.MethodGet, "/v0/runs?state=fake", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"validation_failed"`) {
		t.Errorf("body missing validation_failed: %s", w.Body.String())
	}
}

func TestListRuns_Pagination(t *testing.T) {
	repo := newFakeRepo()
	t0 := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		seedRun(repo, "x/y", "w", run.StatePending, t0.Add(time.Duration(i)*time.Second))
	}
	s := newServer(t, repo)

	// Page 1: limit=2.
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v0/runs?limit=2", nil))
	var page1 struct {
		Items      []runResponse `json:"items"`
		NextCursor string        `json:"next_cursor"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &page1)
	if len(page1.Items) != 2 {
		t.Errorf("page1 size = %d, want 2", len(page1.Items))
	}
	if page1.NextCursor == "" {
		t.Fatal("page1 next_cursor empty")
	}

	// Follow cursor — page 2.
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v0/runs?limit=2&cursor="+page1.NextCursor, nil))
	var page2 struct {
		Items      []runResponse `json:"items"`
		NextCursor string        `json:"next_cursor"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &page2)
	if len(page2.Items) != 2 {
		t.Errorf("page2 size = %d, want 2", len(page2.Items))
	}
	if page2.NextCursor == "" {
		t.Fatal("page2 next_cursor empty")
	}

	// Page 3 — last item, empty cursor.
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v0/runs?limit=2&cursor="+page2.NextCursor, nil))
	var page3 struct {
		Items      []runResponse `json:"items"`
		NextCursor string        `json:"next_cursor"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &page3)
	if len(page3.Items) != 1 {
		t.Errorf("page3 size = %d, want 1", len(page3.Items))
	}
	if page3.NextCursor != "" {
		t.Errorf("page3 cursor = %q, want empty", page3.NextCursor)
	}
}

func TestListRuns_RepoError(t *testing.T) {
	repo := newFakeRepo()
	repo.listErr = errors.New("db down")
	s := newServer(t, repo)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v0/runs", nil))
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
}

func TestListRuns_NilRepo(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0"})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v0/runs", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
}

func TestListRuns_RunnerKindFilter_Forwards(t *testing.T) {
	repo := newFakeRepo()
	s := newServer(t, repo)
	req := httptest.NewRequest(http.MethodGet, "/v0/runs?runner_kind=github_actions", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
	}
	if repo.lastListFilter.RunnerKind == nil {
		t.Fatal("RunnerKind filter not forwarded to repo")
	}
	if *repo.lastListFilter.RunnerKind != run.RunnerKindGitHubActions {
		t.Errorf("RunnerKind filter = %q, want github_actions", *repo.lastListFilter.RunnerKind)
	}
}

func TestListRuns_RunnerKindFilter_RejectsUnknown(t *testing.T) {
	repo := newFakeRepo()
	s := newServer(t, repo)
	req := httptest.NewRequest(http.MethodGet, "/v0/runs?runner_kind=k8s", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

// --- Repo-scoped list filtering (#2071) ---

// listRunsVisible drives GET /v0/runs through the handler with a filtered
// cookie identity and returns the decoded page.
func listRunsVisible(t *testing.T, s *Server, query string) (repos []string, nextCursor string, rec *httptest.ResponseRecorder) {
	t.Helper()
	req := withIdentity(httptest.NewRequest(http.MethodGet, "/v0/runs"+query, nil), memberIdentity())
	rec = httptest.NewRecorder()
	s.handleListRuns(rec, req)
	if rec.Code != http.StatusOK {
		return nil, "", rec
	}
	var got struct {
		Items      []runResponse `json:"items"`
		NextCursor string        `json:"next_cursor"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	for _, it := range got.Items {
		repos = append(repos, it.Repo)
	}
	return repos, got.NextCursor, rec
}

func visibilityListServer(repo *fakeRepo, vis RepoVisibility, role string) *Server {
	return New(Config{Addr: "127.0.0.1:0", RunRepo: repo,
		AccountRoles: fakeAccountRoles{role: role}, RepoVisibility: vis})
}

// TestListRuns_RepoVisibilityFiltersPage covers the member (filtered) view,
// the admin bypass (mode f), and the no-mirror posture (mode g).
func TestListRuns_RepoVisibilityFiltersPage(t *testing.T) {
	t0 := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	newRepo := func() *fakeRepo {
		fr := newFakeRepo()
		seedRun(fr, "acme/app", "feature_change", run.StatePending, t0)
		seedRun(fr, "other/secret", "feature_change", run.StatePending, t0.Add(time.Second))
		return fr
	}
	visible := map[string]bool{"acme/app": true}

	t.Run("member sees only the repos they can read", func(t *testing.T) {
		s := visibilityListServer(newRepo(), newFakeRepoVisibility(visible), account.RoleMember)
		repos, _, rec := listRunsVisible(t, s, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}
		if len(repos) != 1 || repos[0] != "acme/app" {
			t.Fatalf("repos = %v, want [acme/app]", repos)
		}
	})

	t.Run("mode f: admin sees every repo", func(t *testing.T) {
		s := visibilityListServer(newRepo(), newFakeRepoVisibility(visible), account.RoleAdmin)
		repos, _, _ := listRunsVisible(t, s, "")
		if len(repos) != 2 {
			t.Fatalf("repos = %v, want both", repos)
		}
	})

	t.Run("mode g: no mirror wired lists everything", func(t *testing.T) {
		s := visibilityListServer(newRepo(), nil, account.RoleMember)
		repos, _, _ := listRunsVisible(t, s, "")
		if len(repos) != 2 {
			t.Fatalf("repos = %v, want both", repos)
		}
	})

	t.Run("mode h: mirror store fault 503s the page", func(t *testing.T) {
		vis := &fakeRepoVisibility{visible: map[string]bool{}, err: errors.New("db down")}
		s := visibilityListServer(newRepo(), vis, account.RoleMember)
		_, _, rec := listRunsVisible(t, s, "")
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503; body %s", rec.Code, rec.Body.String())
		}
		assertErrorCode(t, rec, "service_unavailable")
	})

	t.Run("mode e: cross-forge rows are denied with no forge call", func(t *testing.T) {
		fr := newFakeRepo()
		seedRun(fr, "acme/app", "feature_change", run.StatePending, t0)
		seedRun(fr, "gl-group/app", "feature_change", run.StatePending, t0.Add(time.Second))
		vis := newFakeRepoVisibility(map[string]bool{"acme/app": true, "gl-group/app": true})
		s := New(Config{Addr: "127.0.0.1:0", RunRepo: fr,
			AccountRoles:   fakeAccountRoles{role: account.RoleMember},
			RepoVisibility: vis,
			RepoProviders:  mapProviderResolver{"acme/app": "github", "gl-group/app": "gitlab"}})
		repos, _, _ := listRunsVisible(t, s, "")
		if len(repos) != 1 || repos[0] != "acme/app" {
			t.Fatalf("repos = %v, want [acme/app] — the gitlab row is cross-forge", repos)
		}
		for _, c := range vis.calls {
			if strings.Contains(c, "gl-group/app") {
				t.Fatalf("mirror consulted for a cross-forge repo: %v", vis.calls)
			}
		}
	})
}

// TestListRuns_RepoVisibilityShortPagesFollowCursor pins the accepted
// pagination artifact: because the offset cursor counts PRE-filter rows, a
// filtered page can come back shorter than `limit` with next_cursor still set.
// Following the cursor to exhaustion must still yield every visible run
// exactly once — the property clients actually depend on.
func TestListRuns_RepoVisibilityShortPagesFollowCursor(t *testing.T) {
	fr := newFakeRepo()
	t0 := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	wantVisible := map[string]bool{}
	// Interleave visible and invisible repos so most pages are short.
	for i := 0; i < 10; i++ {
		repoName := "other/secret"
		if i%2 == 0 {
			repoName = "acme/app"
		}
		r := seedRun(fr, repoName, "feature_change", run.StatePending, t0.Add(time.Duration(i)*time.Second))
		if repoName == "acme/app" {
			wantVisible[r.ID.String()] = true
		}
	}
	s := visibilityListServer(fr, newFakeRepoVisibility(map[string]bool{"acme/app": true}), account.RoleMember)

	seen := map[string]int{}
	cursor := ""
	sawShortPage := false
	for pages := 0; pages < 20; pages++ {
		q := "?limit=2"
		if cursor != "" {
			q += "&cursor=" + cursor
		}
		req := withIdentity(httptest.NewRequest(http.MethodGet, "/v0/runs"+q, nil), memberIdentity())
		rec := httptest.NewRecorder()
		s.handleListRuns(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}
		var got struct {
			Items      []runResponse `json:"items"`
			NextCursor string        `json:"next_cursor"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if len(got.Items) < 2 && got.NextCursor != "" {
			sawShortPage = true
		}
		for _, it := range got.Items {
			if it.Repo != "acme/app" {
				t.Fatalf("page leaked a non-visible repo: %s", it.Repo)
			}
			seen[it.ID.String()]++
		}
		cursor = got.NextCursor
		if cursor == "" {
			break
		}
	}
	if cursor != "" {
		t.Fatal("cursor never exhausted")
	}
	if !sawShortPage {
		t.Error("expected at least one short page with a non-empty next_cursor")
	}
	if len(seen) != len(wantVisible) {
		t.Fatalf("saw %d distinct visible runs, want %d", len(seen), len(wantVisible))
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("run %s returned %d times, want exactly 1", id, n)
		}
		if !wantVisible[id] {
			t.Errorf("run %s should not be visible", id)
		}
	}
}
