package githubclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// gitDataClient wires a Client to a one-off httptest server covering the
// four Git Data API methods (GetRepository, GetCommit, CreateTree,
// CreateCommit). It records the last request path/method + the decoded
// create-tree / create-commit bodies so assertions can verify wiring.
func gitDataClient(t *testing.T) (*Client, *gitDataCapture) {
	t.Helper()
	cap := &gitDataCapture{
		getRepoStatus:     http.StatusOK,
		getCommitStatus:   http.StatusOK,
		createTreeStatus:  http.StatusCreated,
		createCommitState: http.StatusCreated,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{repo}",
		func(w http.ResponseWriter, r *http.Request) {
			cap.getRepoPath = r.URL.Path
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(cap.getRepoStatus)
			_, _ = io.WriteString(w, cap.getRepoBody)
		})
	mux.HandleFunc("GET /repos/{owner}/{repo}/git/commits/{sha}",
		func(w http.ResponseWriter, r *http.Request) {
			cap.getCommitPath = r.URL.Path
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(cap.getCommitStatus)
			_, _ = io.WriteString(w, cap.getCommitBody)
		})
	mux.HandleFunc("POST /repos/{owner}/{repo}/git/trees",
		func(w http.ResponseWriter, r *http.Request) {
			cap.createTreePath = r.URL.Path
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &cap.createTreeBody)
			cap.createTreeCalls++
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(cap.createTreeStatus)
			_, _ = io.WriteString(w, cap.createTreeResp)
		})
	mux.HandleFunc("POST /repos/{owner}/{repo}/git/commits",
		func(w http.ResponseWriter, r *http.Request) {
			cap.createCommitPath = r.URL.Path
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &cap.createCommitBody)
			cap.createCommitCalls++
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(cap.createCommitState)
			_, _ = io.WriteString(w, cap.createCommitResp)
		})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := &Client{
		BaseURL: srv.URL,
		Tokens:  &stubTokens{token: "ghs_canned_token"},
		HTTP:    &http.Client{Timeout: 5 * time.Second},
	}
	return c, cap
}

type gitDataCapture struct {
	getRepoStatus int
	getRepoBody   string
	getRepoPath   string

	getCommitStatus int
	getCommitBody   string
	getCommitPath   string

	createTreeStatus int
	createTreeResp   string
	createTreePath   string
	createTreeCalls  int
	createTreeBody   struct {
		BaseTree string `json:"base_tree"`
		Tree     []struct {
			Path    string `json:"path"`
			Mode    string `json:"mode"`
			Type    string `json:"type"`
			Content string `json:"content"`
		} `json:"tree"`
	}

	createCommitState int
	createCommitResp  string
	createCommitPath  string
	createCommitCalls int
	createCommitBody  struct {
		Message string   `json:"message"`
		Tree    string   `json:"tree"`
		Parents []string `json:"parents"`
	}
}

func TestGetRepository_HappyPath(t *testing.T) {
	c, cap := gitDataClient(t)
	cap.getRepoBody = `{"default_branch":"trunk","name":"y"}`

	got, err := c.GetRepository(context.Background(), 42, RepoRef{Owner: "x", Name: "y"})
	if err != nil {
		t.Fatalf("GetRepository: %v", err)
	}
	if got.DefaultBranch != "trunk" {
		t.Errorf("DefaultBranch = %q, want trunk", got.DefaultBranch)
	}
	if want := "/repos/x/y"; cap.getRepoPath != want {
		t.Errorf("path = %q, want %q", cap.getRepoPath, want)
	}
}

func TestGetRepository_MissingDefaultBranch(t *testing.T) {
	c, cap := gitDataClient(t)
	cap.getRepoBody = `{"name":"y"}`
	_, err := c.GetRepository(context.Background(), 42, RepoRef{Owner: "x", Name: "y"})
	if err == nil || !strings.Contains(err.Error(), "default_branch") {
		t.Errorf("err = %v, want missing default_branch", err)
	}
}

func TestGetRepository_ErrorMapping(t *testing.T) {
	cases := []struct {
		name   string
		status int
		want   error
	}{
		{"forbidden", http.StatusForbidden, ErrForbidden},
		{"not found", http.StatusNotFound, ErrNotFound},
		{"validation", http.StatusUnprocessableEntity, ErrValidation},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, cap := gitDataClient(t)
			cap.getRepoStatus = tc.status
			cap.getRepoBody = `{"message":"nope"}`
			_, err := c.GetRepository(context.Background(), 42, RepoRef{Owner: "x", Name: "y"})
			if err == nil || !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestGetCommit_HappyPath(t *testing.T) {
	c, cap := gitDataClient(t)
	cap.getCommitBody = `{"sha":"commit123","tree":{"sha":"tree456"}}`

	got, err := c.GetCommit(context.Background(), 42, RepoRef{Owner: "x", Name: "y"}, "commit123")
	if err != nil {
		t.Fatalf("GetCommit: %v", err)
	}
	if got.SHA != "commit123" || got.TreeSHA != "tree456" {
		t.Errorf("got = %+v, want sha=commit123 tree=tree456", got)
	}
	if want := "/repos/x/y/git/commits/commit123"; cap.getCommitPath != want {
		t.Errorf("path = %q, want %q", cap.getCommitPath, want)
	}
}

func TestGetCommit_MissingTreeSHA(t *testing.T) {
	c, cap := gitDataClient(t)
	cap.getCommitBody = `{"sha":"commit123"}`
	_, err := c.GetCommit(context.Background(), 42, RepoRef{Owner: "x", Name: "y"}, "commit123")
	if err == nil || !strings.Contains(err.Error(), "tree.sha") {
		t.Errorf("err = %v, want missing tree.sha", err)
	}
}

func TestGetCommit_ErrorMapping(t *testing.T) {
	cases := []struct {
		name   string
		status int
		want   error
	}{
		{"forbidden", http.StatusForbidden, ErrForbidden},
		{"not found", http.StatusNotFound, ErrNotFound},
		{"validation", http.StatusUnprocessableEntity, ErrValidation},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, cap := gitDataClient(t)
			cap.getCommitStatus = tc.status
			cap.getCommitBody = `{"message":"nope"}`
			_, err := c.GetCommit(context.Background(), 42, RepoRef{Owner: "x", Name: "y"}, "s")
			if err == nil || !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestCreateTree_HappyPath(t *testing.T) {
	c, cap := gitDataClient(t)
	cap.createTreeResp = `{"sha":"newtree789"}`

	sha, err := c.CreateTree(context.Background(), 42, RepoRef{Owner: "x", Name: "y"},
		"basetree000", []TreeEntry{
			{Path: ".fishhawk/workflows.yaml", Content: "version: 1.0\n"},
			{Path: "AGENTS.md", Content: "# agents\n"},
		})
	if err != nil {
		t.Fatalf("CreateTree: %v", err)
	}
	if sha != "newtree789" {
		t.Errorf("sha = %q, want newtree789", sha)
	}
	if want := "/repos/x/y/git/trees"; cap.createTreePath != want {
		t.Errorf("path = %q, want %q", cap.createTreePath, want)
	}
	// base_tree must be serialized so the repo's existing files survive.
	if cap.createTreeBody.BaseTree != "basetree000" {
		t.Errorf("base_tree = %q, want basetree000", cap.createTreeBody.BaseTree)
	}
	if len(cap.createTreeBody.Tree) != 2 {
		t.Fatalf("tree entries = %d, want 2", len(cap.createTreeBody.Tree))
	}
	e := cap.createTreeBody.Tree[0]
	if e.Path != ".fishhawk/workflows.yaml" {
		t.Errorf("entry path = %q", e.Path)
	}
	// Inline content (mode 100644, type blob) — no separate blob create.
	if e.Mode != "100644" || e.Type != "blob" {
		t.Errorf("entry mode/type = %q/%q, want 100644/blob", e.Mode, e.Type)
	}
	if e.Content != "version: 1.0\n" {
		t.Errorf("entry content = %q, want inline content", e.Content)
	}
}

func TestCreateTree_NoEntries(t *testing.T) {
	c, _ := gitDataClient(t)
	_, err := c.CreateTree(context.Background(), 42, RepoRef{Owner: "x", Name: "y"}, "b", nil)
	if err == nil || !strings.Contains(err.Error(), "at least one tree entry") {
		t.Errorf("err = %v, want at-least-one-entry error", err)
	}
}

func TestCreateTree_ErrorMapping(t *testing.T) {
	cases := []struct {
		name   string
		status int
		want   error
	}{
		{"forbidden", http.StatusForbidden, ErrForbidden},
		{"not found", http.StatusNotFound, ErrNotFound},
		{"validation", http.StatusUnprocessableEntity, ErrValidation},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, cap := gitDataClient(t)
			cap.createTreeStatus = tc.status
			cap.createTreeResp = `{"message":"nope"}`
			_, err := c.CreateTree(context.Background(), 42, RepoRef{Owner: "x", Name: "y"},
				"b", []TreeEntry{{Path: "f", Content: "c"}})
			if err == nil || !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestCreateCommit_HappyPath(t *testing.T) {
	c, cap := gitDataClient(t)
	cap.createCommitResp = `{"sha":"commitABC"}`

	sha, err := c.CreateCommit(context.Background(), 42, RepoRef{Owner: "x", Name: "y"},
		"scaffold onboarding", "newtree789", []string{"parentSHA"})
	if err != nil {
		t.Fatalf("CreateCommit: %v", err)
	}
	if sha != "commitABC" {
		t.Errorf("sha = %q, want commitABC", sha)
	}
	if want := "/repos/x/y/git/commits"; cap.createCommitPath != want {
		t.Errorf("path = %q, want %q", cap.createCommitPath, want)
	}
	if cap.createCommitBody.Message != "scaffold onboarding" {
		t.Errorf("message = %q", cap.createCommitBody.Message)
	}
	if cap.createCommitBody.Tree != "newtree789" {
		t.Errorf("tree = %q, want newtree789", cap.createCommitBody.Tree)
	}
	if len(cap.createCommitBody.Parents) != 1 || cap.createCommitBody.Parents[0] != "parentSHA" {
		t.Errorf("parents = %v, want [parentSHA]", cap.createCommitBody.Parents)
	}
}

func TestCreateCommit_ErrorMapping(t *testing.T) {
	cases := []struct {
		name   string
		status int
		want   error
	}{
		{"forbidden", http.StatusForbidden, ErrForbidden},
		{"not found", http.StatusNotFound, ErrNotFound},
		{"validation", http.StatusUnprocessableEntity, ErrValidation},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, cap := gitDataClient(t)
			cap.createCommitState = tc.status
			cap.createCommitResp = `{"message":"nope"}`
			_, err := c.CreateCommit(context.Background(), 42, RepoRef{Owner: "x", Name: "y"},
				"m", "t", []string{"p"})
			if err == nil || !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestGitData_ValidationErrors(t *testing.T) {
	c := &Client{Tokens: &stubTokens{}}
	ctx := context.Background()
	repo := RepoRef{Owner: "x", Name: "y"}

	if _, err := c.GetRepository(ctx, 1, RepoRef{Name: "y"}); err == nil ||
		!strings.Contains(err.Error(), "owner and name") {
		t.Errorf("GetRepository missing owner: %v", err)
	}
	if _, err := c.GetCommit(ctx, 1, repo, ""); err == nil ||
		!strings.Contains(err.Error(), "commit sha is required") {
		t.Errorf("GetCommit missing sha: %v", err)
	}
	if _, err := c.CreateCommit(ctx, 1, repo, "", "t", nil); err == nil ||
		!strings.Contains(err.Error(), "commit message is required") {
		t.Errorf("CreateCommit missing message: %v", err)
	}
	if _, err := c.CreateCommit(ctx, 1, repo, "m", "", nil); err == nil ||
		!strings.Contains(err.Error(), "tree sha is required") {
		t.Errorf("CreateCommit missing tree: %v", err)
	}
}

func TestGitData_MissingTokens(t *testing.T) {
	c := &Client{} // no Tokens
	ctx := context.Background()
	repo := RepoRef{Owner: "x", Name: "y"}
	if _, err := c.GetRepository(ctx, 1, repo); err == nil ||
		!strings.Contains(err.Error(), "TokenProvider") {
		t.Errorf("GetRepository: %v", err)
	}
	if _, err := c.CreateTree(ctx, 1, repo, "b", []TreeEntry{{Path: "f", Content: "c"}}); err == nil ||
		!strings.Contains(err.Error(), "TokenProvider") {
		t.Errorf("CreateTree: %v", err)
	}
}
