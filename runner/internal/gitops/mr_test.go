package gitops

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// stubGitLabAPI mounts a merge_requests handler with a configurable response,
// capturing the request shape for assertions.
type stubGitLabAPI struct {
	respCode  int
	respBody  string
	gotMethod string
	gotPath   string
	gotToken  string
	gotBody   map[string]any
}

func newStubGitLabAPI(t *testing.T) (*stubGitLabAPI, *httptest.Server) {
	t.Helper()
	stub := &stubGitLabAPI{respCode: http.StatusCreated}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		stub.gotMethod = r.Method
		// r.URL.Path is already percent-decoded by net/http; use RawPath
		// (populated only when it differs from the decoded form) to assert
		// the on-the-wire %2F encoding of a nested namespace.
		if r.URL.RawPath != "" {
			stub.gotPath = r.URL.RawPath
		} else {
			stub.gotPath = r.URL.Path
		}
		stub.gotToken = r.Header.Get("PRIVATE-TOKEN")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &stub.gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(stub.respCode)
		_, _ = io.WriteString(w, stub.respBody)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return stub, srv
}

func TestOpenMR_HappyPath(t *testing.T) {
	stub, srv := newStubGitLabAPI(t)
	stub.respBody = `{"iid":7,"web_url":"https://gitlab.com/group/project/-/merge_requests/7"}`
	c := &OpenMRClient{
		HTTP:    &http.Client{Timeout: time.Second},
		BaseURL: srv.URL,
		Token:   "glpat-xyz",
	}

	got, err := c.OpenMR(context.Background(), OpenMRArgs{
		ProjectPath:  "group/project",
		SourceBranch: "fishhawk/run-aaa/stage-bbb",
		TargetBranch: "main",
		Title:        "Add a thing",
		Description:  "context",
	})
	if err != nil {
		t.Fatalf("OpenMR: %v", err)
	}
	if got.PRNumber != 7 || got.PRURL == "" {
		t.Errorf("got = %+v", got)
	}
	if stub.gotMethod != http.MethodPost {
		t.Errorf("method = %q", stub.gotMethod)
	}
	if stub.gotPath != "/api/v4/projects/group%2Fproject/merge_requests" {
		t.Errorf("path = %q, want the %%2F-encoded project segment", stub.gotPath)
	}
	if stub.gotToken != "glpat-xyz" {
		t.Errorf("PRIVATE-TOKEN = %q", stub.gotToken)
	}
	wantBody := map[string]any{
		"source_branch": "fishhawk/run-aaa/stage-bbb",
		"target_branch": "main",
		"title":         "Add a thing",
	}
	for k, want := range wantBody {
		if got := stub.gotBody[k]; got != want {
			t.Errorf("body[%q] = %v, want %v", k, got, want)
		}
	}
}

// TestOpenMR_EscapesNestedNamespace pins that a group/subgroup/project slug is
// collapsed into a single %2F-encoded path segment per GitLab's namespaced-path
// routing.
func TestOpenMR_EscapesNestedNamespace(t *testing.T) {
	stub, srv := newStubGitLabAPI(t)
	stub.respBody = `{"iid":3,"web_url":"https://gitlab.example.com/g/s/p/-/merge_requests/3"}`
	c := &OpenMRClient{BaseURL: srv.URL, Token: "glpat-xyz"}

	_, err := c.OpenMR(context.Background(), OpenMRArgs{
		ProjectPath:  "group/subgroup/project",
		SourceBranch: "h", TargetBranch: "main", Title: "t",
	})
	if err != nil {
		t.Fatalf("OpenMR: %v", err)
	}
	if stub.gotPath != "/api/v4/projects/group%2Fsubgroup%2Fproject/merge_requests" {
		t.Errorf("path = %q, want each namespace separator %%2F-encoded", stub.gotPath)
	}
}

func TestOpenMR_GitLabError(t *testing.T) {
	stub, srv := newStubGitLabAPI(t)
	stub.respCode = http.StatusConflict
	stub.respBody = `{"message":["Another open merge request already exists for this source branch"]}`
	c := &OpenMRClient{BaseURL: srv.URL, Token: "glpat-xyz"}

	_, err := c.OpenMR(context.Background(), OpenMRArgs{
		ProjectPath:  "group/project",
		SourceBranch: "fishhawk/branch", TargetBranch: "main", Title: "Add a thing",
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "409") {
		t.Errorf("err = %v, want 409 in message", err)
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("err = %v, want GitLab message in error", err)
	}
}

func TestOpenMR_RejectsBadInputs(t *testing.T) {
	c := &OpenMRClient{BaseURL: "https://gitlab.com", Token: "x"}
	cases := map[string]OpenMRArgs{
		"missing project": {SourceBranch: "h", TargetBranch: "b", Title: "t"},
		"missing source":  {ProjectPath: "g/p", TargetBranch: "b", Title: "t"},
		"missing target":  {ProjectPath: "g/p", SourceBranch: "h", Title: "t"},
		"missing title":   {ProjectPath: "g/p", SourceBranch: "h", TargetBranch: "b"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := c.OpenMR(context.Background(), args); err == nil {
				t.Error("expected error")
			}
		})
	}

	// Missing token.
	cNoTok := &OpenMRClient{BaseURL: "https://gitlab.com"}
	if _, err := cNoTok.OpenMR(context.Background(), OpenMRArgs{
		ProjectPath: "g/p", SourceBranch: "h", TargetBranch: "b", Title: "t",
	}); err == nil {
		t.Error("expected token-required error")
	}

	// Missing base URL — must NOT silently default to gitlab.com.
	cNoBase := &OpenMRClient{Token: "x"}
	if _, err := cNoBase.OpenMR(context.Background(), OpenMRArgs{
		ProjectPath: "g/p", SourceBranch: "h", TargetBranch: "b", Title: "t",
	}); err == nil {
		t.Error("expected base-URL-required error")
	}
}

func TestOpenMR_RejectsMalformedSuccess(t *testing.T) {
	stub, srv := newStubGitLabAPI(t)
	stub.respBody = `{"iid":0,"web_url":""}` // 201 but missing fields
	c := &OpenMRClient{BaseURL: srv.URL, Token: "x"}

	_, err := c.OpenMR(context.Background(), OpenMRArgs{
		ProjectPath: "g/p", SourceBranch: "h", TargetBranch: "b", Title: "t",
	})
	if err == nil {
		t.Fatal("expected error on missing iid/web_url")
	}
}

// TestCommitAndPush_ShapedRemote_LineageIsRemoteShapeIndependent is the binding
// approval condition #2 shaped-remote half (terra 73134486 / opus 23dbab2f): it
// exercises the run-branch push + ADR-035 lineage/tree-ownership machinery
// against a bare remote configured with a GitLab-shaped remote URL and
// GitLab-flow branch naming, pinning that the lineage logic (BaseSHA fork-point
// from the freshly-fetched authoritative base, HeadSHA≠BaseSHA, TreeSHA = the
// pushed commit's tree, sole-writer branch ownership) is INDEPENDENT of the
// remote's host shape — the git-level machinery never parses the forge host.
// The live end-to-end GitLab walk with real credentials is re-homed to #2032
// (E45.18) per the same approval condition.
func TestCommitAndPush_ShapedRemote_LineageIsRemoteShapeIndependent(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	// GitLab-flow run branch naming (identical shape across forges — this is
	// exactly the point being pinned: the branch/remote shape does not steer
	// the lineage machinery).
	const branch = "fishhawk/run-glab1859/stage-x"

	dir := t.TempDir()
	repo := filepath.Join(dir, "src")
	bare := filepath.Join(dir, "gitlab.example.com-group-project.git")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "init", "--initial-branch=main")
	mustGit(t, repo, "config", "user.name", "init")
	mustGit(t, repo, "config", "user.email", "init@example.com")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("# initial\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "add", "-A")
	mustGit(t, repo, "commit", "-m", "initial")

	// Establish the bare remote with main at the authoritative tip. The bare
	// path stands in for a self-managed GitLab remote; a file remote is the
	// only offline-safe fixture, and the lineage assertions below are precisely
	// what proves the host shape is irrelevant.
	mustGit(t, repo, "init", "--bare", bare)
	// Configure a GitLab-shaped remote URL on origin so any accidental host
	// coupling in the lineage machinery would surface; the actual fetch/push
	// target below is the offline bare path.
	mustGit(t, repo, "remote", "add", "origin", "https://gitlab.example.com/group/project.git")
	mustGit(t, repo, "push", bare, "main")
	authoritativeTip := mustGitOut(t, repo, "rev-parse", "HEAD")

	// The agent's uncommitted working-tree edit.
	if err := os.WriteFile(filepath.Join(repo, "feature.txt"), []byte("agent edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	p := &Pusher{}
	res, err := p.CommitAndPush(context.Background(), CommitAndPushArgs{
		RepoDir:        repo,
		Branch:         branch,
		CommitMessage:  "agent stage commit",
		RemoteURL:      bare,
		FreshFetchBase: "main",
		ForceWithLease: true, // standalone sole-writer posture (ADR-035 / #1872)
	})
	if err != nil {
		t.Fatalf("CommitAndPush (shaped remote): %v", err)
	}

	// ADR-035 lineage: BaseSHA is the fetched authoritative-base fork point.
	if res.BaseSHA != authoritativeTip {
		t.Errorf("BaseSHA = %q, want fetched authoritative tip %q", res.BaseSHA, authoritativeTip)
	}
	if res.HeadSHA == "" || res.HeadSHA == res.BaseSHA {
		t.Errorf("HeadSHA = %q, want a distinct commit off BaseSHA %q", res.HeadSHA, res.BaseSHA)
	}
	parent := mustGitOut(t, repo, "rev-parse", "HEAD^")
	if parent != authoritativeTip {
		t.Errorf("run branch parent = %q, want authoritative tip %q", parent, authoritativeTip)
	}

	// Tree-ownership: TreeSHA is the pushed commit's tree object hash.
	if res.TreeSHA == "" {
		t.Error("TreeSHA empty")
	}
	if got := mustGitOut(t, repo, "rev-parse", res.HeadSHA+"^{tree}"); got != res.TreeSHA {
		t.Errorf("TreeSHA = %q, want rev-parse %s^{tree} = %q", res.TreeSHA, res.HeadSHA, got)
	}

	// Sole-writer branch ownership: the run branch landed in the bare remote at
	// exactly our pushed HEAD.
	out, err := exec.Command("git", "--git-dir="+bare, "rev-parse", branch).Output()
	if err != nil {
		t.Fatalf("verify run branch in bare remote: %v", err)
	}
	if strings.TrimSpace(string(out)) != res.HeadSHA {
		t.Errorf("bare remote %s = %q, want pushed HEAD %q", branch, strings.TrimSpace(string(out)), res.HeadSHA)
	}
}
