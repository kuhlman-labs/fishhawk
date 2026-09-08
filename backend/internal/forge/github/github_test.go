package github_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	forgegithub "github.com/kuhlman-labs/fishhawk/backend/internal/forge/github"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
)

// stubTokens is a no-op githubapp.TokenProvider. ResolveRepoScope's only
// GitHub call (GetRepoInstallation) authenticates with the App JWT, not
// an installation token, so Token is never invoked — but the field must
// be non-nil to build a Client.
type stubTokens struct{}

func (stubTokens) Token(context.Context, int64) (string, error) { return "unused", nil }

// newAdapter builds a *forgegithub.Forge whose embedded client points at
// an httptest server that answers GET /repos/{owner}/{repo}/installation
// with the given status and body.
func newAdapter(t *testing.T, status int, body string) *forgegithub.Forge {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{repo}/installation", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if body != "" {
			_, _ = io.WriteString(w, body)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := &githubclient.Client{
		BaseURL: srv.URL,
		Tokens:  stubTokens{},
		HTTP:    &http.Client{Timeout: 5 * time.Second},
		AppJWT:  func() (string, error) { return "ghs_app_jwt", nil },
	}
	return forgegithub.New(c)
}

// TestSatisfiesForgeInterface is the compile-time contract restated as a
// runtime assertion: the embedded client plus Name/ResolveRepoScope must
// cover the whole forge.Forge surface. Reaching a non-empty Name through
// the interface value proves the adapter is dispatchable as a forge.Forge.
func TestSatisfiesForgeInterface(t *testing.T) {
	var f forge.Forge = newAdapter(t, http.StatusOK, `{"id":1}`)
	if f.Name() != "github" {
		t.Errorf("forge.Forge.Name() = %q, want %q", f.Name(), "github")
	}
}

// TestName pins the registry id.
func TestName(t *testing.T) {
	if got := newAdapter(t, http.StatusOK, `{"id":1}`).Name(); got != "github" {
		t.Errorf("Name() = %q, want %q", got, "github")
	}
}

// TestResolveRepoScopeSuccess is the happy path: a resolved installation
// id becomes a CredentialScope whose ref is the stringified id.
func TestResolveRepoScopeSuccess(t *testing.T) {
	f := newAdapter(t, http.StatusOK, `{"id":12345}`)

	scope, err := f.ResolveRepoScope(context.Background(), forge.RepoRef{Owner: "o", Name: "n"})
	if err != nil {
		t.Fatalf("ResolveRepoScope: %v", err)
	}
	if scope.Ref() != "12345" {
		t.Errorf("scope.Ref() = %q, want %q", scope.Ref(), "12345")
	}
	// Round-trips back to the installation id via the GitHub accessor.
	id, err := scope.GitHubInstallationID()
	if err != nil {
		t.Fatalf("GitHubInstallationID: %v", err)
	}
	if id != 12345 {
		t.Errorf("installation id = %d, want 12345", id)
	}
}

// TestResolveRepoScopeNotInstalled is the failure path: a not-installed
// repo (404 on the installation endpoint) propagates githubclient's
// ErrNotInstalled UNMODIFIED — the adapter must not launder it into a
// generic error or the zero scope with a nil error.
func TestResolveRepoScopeNotInstalled(t *testing.T) {
	f := newAdapter(t, http.StatusNotFound, `{"message":"Not Found"}`)

	scope, err := f.ResolveRepoScope(context.Background(), forge.RepoRef{Owner: "o", Name: "n"})
	if err == nil {
		t.Fatal("expected an error for a not-installed repo")
	}
	if !errors.Is(err, forge.ErrNotInstalled) {
		t.Errorf("err = %v, want forge.ErrNotInstalled", err)
	}
	// The propagated ErrNotInstalled is distinct from ErrNotFound — the
	// distinction callers switch on survives the adapter.
	if errors.Is(err, forge.ErrNotFound) {
		t.Errorf("ErrNotInstalled must stay distinct from ErrNotFound; err = %v", err)
	}
	if !scope.IsZero() {
		t.Errorf("scope = %v on error, want the zero scope", scope)
	}
}

// newContentsAdapter builds a *forgegithub.Forge whose embedded client
// points at an httptest server serving the given handler for the
// Contents API file read.
func newContentsAdapter(t *testing.T, handler http.HandlerFunc) *forgegithub.Forge {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{repo}/contents/{path...}", handler)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := &githubclient.Client{
		BaseURL: srv.URL,
		Tokens:  stubTokens{},
		HTTP:    &http.Client{Timeout: 5 * time.Second},
	}
	return forgegithub.New(c)
}

// TestFetchFileMapsContent pins the forge.FileFetcher mapping: the
// embedded client's GetFile result lands field-for-field on
// *forge.FileContent (path, decoded content, blob SHA), the requested ref
// rides the query, and the call is dispatchable through the standalone
// capability interface.
func TestFetchFileMapsContent(t *testing.T) {
	content := base64.StdEncoding.EncodeToString([]byte("version: 1\n"))
	f := newContentsAdapter(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.PathValue("path"); got != ".fishhawk/work-management.yaml" {
			t.Errorf("contents path = %q, want .fishhawk/work-management.yaml", got)
		}
		if got := r.URL.Query().Get("ref"); got != "main" {
			t.Errorf("ref = %q, want main", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w,
			`{"path":".fishhawk/work-management.yaml","sha":"blob123","content":"`+content+`","encoding":"base64","type":"file"}`)
	})

	var fetcher forge.FileFetcher = f
	fc, err := fetcher.FetchFile(context.Background(), forge.FromGitHubInstallationID(42),
		forge.RepoRef{Owner: "o", Name: "n"}, ".fishhawk/work-management.yaml", "main")
	if err != nil {
		t.Fatalf("FetchFile: %v", err)
	}
	if fc.Path != ".fishhawk/work-management.yaml" {
		t.Errorf("Path = %q", fc.Path)
	}
	if got := string(fc.Content); got != "version: 1\n" {
		t.Errorf("Content = %q, want the decoded file body", got)
	}
	if fc.SHA != "blob123" {
		t.Errorf("SHA = %q, want blob123", fc.SHA)
	}
}

// TestFetchFileNotFound pins the ErrNotFound passthrough: a 404 from the
// Contents API surfaces as forge.ErrNotFound UNMODIFIED — the sentinel
// the conventions loader's fall-through branch switches on.
func TestFetchFileNotFound(t *testing.T) {
	f := newContentsAdapter(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"message":"Not Found"}`)
	})

	fc, err := f.FetchFile(context.Background(), forge.FromGitHubInstallationID(42),
		forge.RepoRef{Owner: "o", Name: "n"}, "missing.yaml", "main")
	if !errors.Is(err, forge.ErrNotFound) {
		t.Errorf("err = %v, want forge.ErrNotFound", err)
	}
	if fc != nil {
		t.Errorf("FileContent = %+v on error, want nil", fc)
	}
}

// TestResolveRepoScopeOtherError confirms a non-404 upstream failure
// also propagates (not misclassified as not-installed) and yields the
// zero scope.
func TestResolveRepoScopeOtherError(t *testing.T) {
	f := newAdapter(t, http.StatusInternalServerError, `{"message":"boom"}`)

	scope, err := f.ResolveRepoScope(context.Background(), forge.RepoRef{Owner: "o", Name: "n"})
	if err == nil {
		t.Fatal("expected an error for a 500 response")
	}
	if errors.Is(err, forge.ErrNotInstalled) {
		t.Errorf("a 500 must not become ErrNotInstalled; err = %v", err)
	}
	if !scope.IsZero() {
		t.Errorf("scope = %v on error, want the zero scope", scope)
	}
}

// --- forge.IssueOperations (E50.17 / #2900) -----------------------------

// issueCall is one request the issues fake recorded: method, path and the
// decoded JSON body (nil for a bodiless GET).
type issueCall struct {
	Method string
	Path   string
	Query  string
	Body   map[string]any
}

// issuesAdapter is a *forgegithub.Forge over an httptest GitHub whose mux
// the caller populates; every request is recorded in arrival order so a
// case asserts the exact wire shape the wrapper produced.
type issuesAdapter struct {
	f   *forgegithub.Forge
	mux *http.ServeMux
	mu  sync.Mutex
	log []issueCall
}

func newIssuesAdapter(t *testing.T) *issuesAdapter {
	t.Helper()
	a := &issuesAdapter{mux: http.NewServeMux()}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := issueCall{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery}
		raw, _ := io.ReadAll(r.Body)
		if len(raw) > 0 {
			_ = json.Unmarshal(raw, &call.Body)
		}
		a.mu.Lock()
		a.log = append(a.log, call)
		a.mu.Unlock()
		a.mux.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	a.f = forgegithub.New(&githubclient.Client{
		BaseURL: srv.URL,
		Tokens:  stubTokens{},
		HTTP:    &http.Client{Timeout: 5 * time.Second},
	})
	return a
}

func (a *issuesAdapter) calls() []issueCall {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]issueCall, len(a.log))
	copy(out, a.log)
	return out
}

func writeJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

var (
	issueScope = forge.FromGitHubInstallationID(42)
	issueRepo  = forge.RepoRef{Owner: "o", Name: "n"}
)

// TestFetchIssueMapsFields pins the FetchIssue wrapper: GET
// /repos/{owner}/{repo}/issues/{number}, with number/title/body/state/
// state_reason/labels carried onto forge.Issue and dispatchable through
// the standalone capability interface.
func TestFetchIssueMapsFields(t *testing.T) {
	a := newIssuesAdapter(t)
	a.mux.HandleFunc("GET /repos/o/n/issues/7", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, `{"number":7,"title":"Parent","body":"text","state":"closed","state_reason":"completed","labels":[{"name":"type:epic"},"area:server"]}`)
	})

	var ops forge.IssueOperations = a.f
	is, err := ops.FetchIssue(context.Background(), issueScope, issueRepo, 7)
	if err != nil {
		t.Fatalf("FetchIssue: %v", err)
	}
	want := forge.Issue{Number: 7, Title: "Parent", Body: "text", State: "closed", StateReason: "completed", Labels: []string{"type:epic", "area:server"}}
	if is.Number != want.Number || is.Title != want.Title || is.Body != want.Body || is.State != want.State || is.StateReason != want.StateReason {
		t.Errorf("Issue = %+v, want %+v", *is, want)
	}
	if len(is.Labels) != 2 || is.Labels[0] != "type:epic" || is.Labels[1] != "area:server" {
		t.Errorf("Labels = %v, want [type:epic area:server]", is.Labels)
	}
	calls := a.calls()
	if len(calls) != 1 || calls[0].Method != http.MethodGet || calls[0].Path != "/repos/o/n/issues/7" {
		t.Errorf("calls = %+v, want one GET /repos/o/n/issues/7", calls)
	}
}

// TestFetchIssueCommentsMapsThread pins the FetchIssueComments wrapper:
// GET .../issues/{number}/comments paged to exhaustion by the embedded
// client, with id/author/body/created_at carried onto forge.IssueComment
// and a hidden-HTML-comment marker byte-intact.
func TestFetchIssueCommentsMapsThread(t *testing.T) {
	a := newIssuesAdapter(t)
	a.mux.HandleFunc("GET /repos/o/n/issues/7/comments", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "2" {
			writeJSON(w, http.StatusOK, `[{"id":3,"user":{"login":"bot"},"body":"<!-- fishhawk:key -->\nlinked","created_at":"2026-09-01T00:02:00Z"}]`)
			return
		}
		w.Header().Set("Link", `<`+"http://"+r.Host+`/repos/o/n/issues/7/comments?page=2>; rel="next"`)
		writeJSON(w, http.StatusOK, `[{"id":1,"user":{"login":"alice"},"body":"first","created_at":"2026-09-01T00:00:00Z"}]`)
	})

	var ops forge.IssueOperations = a.f
	comments, err := ops.FetchIssueComments(context.Background(), issueScope, issueRepo, 7)
	if err != nil {
		t.Fatalf("FetchIssueComments: %v", err)
	}
	if len(comments) != 2 {
		t.Fatalf("len(comments) = %d, want 2 (both pages)", len(comments))
	}
	if comments[0].ID != 1 || comments[0].Author != "alice" || comments[0].Body != "first" || comments[0].CreatedAt != "2026-09-01T00:00:00Z" {
		t.Errorf("comments[0] = %+v", comments[0])
	}
	if comments[1].ID != 3 || comments[1].Author != "bot" || comments[1].Body != "<!-- fishhawk:key -->\nlinked" {
		t.Errorf("comments[1] = %+v, want the page-2 marker comment byte-intact", comments[1])
	}
	calls := a.calls()
	if len(calls) != 2 || calls[0].Path != "/repos/o/n/issues/7/comments" || calls[1].Query != "page=2" {
		t.Errorf("calls = %+v, want two GETs on .../issues/7/comments (page 1 then page=2)", calls)
	}
}

// TestPostIssueCommentSendsBody pins the PostIssueComment wrapper: POST
// .../issues/{number}/comments carrying the body byte-intact.
func TestPostIssueCommentSendsBody(t *testing.T) {
	a := newIssuesAdapter(t)
	a.mux.HandleFunc("POST /repos/o/n/issues/7/comments", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusCreated, `{"id":99,"body":"x","html_url":"https://gh/x"}`)
	})

	var ops forge.IssueOperations = a.f
	if err := ops.PostIssueComment(context.Background(), issueScope, issueRepo, 7, "<!-- k -->\nhello"); err != nil {
		t.Fatalf("PostIssueComment: %v", err)
	}
	calls := a.calls()
	if len(calls) != 1 || calls[0].Method != http.MethodPost || calls[0].Path != "/repos/o/n/issues/7/comments" {
		t.Fatalf("calls = %+v, want one POST .../issues/7/comments", calls)
	}
	if got := calls[0].Body["body"]; got != "<!-- k -->\nhello" {
		t.Errorf("body.body = %v, want the comment text byte-intact", got)
	}
}

// TestSetIssueStateSendsStateAndReason pins that the PATCH body carries
// BOTH `state` and `state_reason` — a state-only PATCH would silently drop
// the completion reason the watcher records.
func TestSetIssueStateSendsStateAndReason(t *testing.T) {
	a := newIssuesAdapter(t)
	a.mux.HandleFunc("PATCH /repos/o/n/issues/7", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, `{"number":7,"state":"closed","state_reason":"completed"}`)
	})

	state, reason := "closed", "completed"
	var ops forge.IssueOperations = a.f
	if err := ops.SetIssueState(context.Background(), issueScope, issueRepo, 7, forge.IssueStateUpdate{State: &state, StateReason: &reason}); err != nil {
		t.Fatalf("SetIssueState: %v", err)
	}
	calls := a.calls()
	if len(calls) != 1 || calls[0].Method != http.MethodPatch || calls[0].Path != "/repos/o/n/issues/7" {
		t.Fatalf("calls = %+v, want one PATCH /repos/o/n/issues/7", calls)
	}
	if got := calls[0].Body["state"]; got != "closed" {
		t.Errorf("body.state = %v, want closed", got)
	}
	if got := calls[0].Body["state_reason"]; got != "completed" {
		t.Errorf("body.state_reason = %v, want completed (a state-only PATCH drops the reason)", got)
	}
}

// TestSetIssueStateOmitsAbsentReason pins the pointer semantics: a nil
// StateReason sends NO state_reason key rather than an empty one.
func TestSetIssueStateOmitsAbsentReason(t *testing.T) {
	a := newIssuesAdapter(t)
	a.mux.HandleFunc("PATCH /repos/o/n/issues/7", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, `{"number":7,"state":"open"}`)
	})
	state := "open"
	if err := a.f.SetIssueState(context.Background(), issueScope, issueRepo, 7, forge.IssueStateUpdate{State: &state}); err != nil {
		t.Fatalf("SetIssueState: %v", err)
	}
	body := a.calls()[0].Body
	if body["state"] != "open" {
		t.Errorf("body.state = %v, want open", body["state"])
	}
	if v, has := body["state_reason"]; has {
		t.Errorf("body carries state_reason = %v for a nil StateReason; only set fields may be transmitted", v)
	}
}

// TestSetIssueStateNilStateRefusedBeforeHTTP pins the local refusal: an
// update with no target state is forge.ErrValidation and makes ZERO HTTP
// calls. Deleting the nil-State guard lets the call reach the embedded
// client, which still refuses an empty PATCH — but with a plain error, not
// ErrValidation, so the errors.Is assertion is what reddens.
func TestSetIssueStateNilStateRefusedBeforeHTTP(t *testing.T) {
	a := newIssuesAdapter(t)
	a.mux.HandleFunc("PATCH /repos/o/n/issues/7", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, `{"number":7}`)
	})
	err := a.f.SetIssueState(context.Background(), issueScope, issueRepo, 7, forge.IssueStateUpdate{})
	if !errors.Is(err, forge.ErrValidation) {
		t.Errorf("err = %v, want forge.ErrValidation", err)
	}
	if n := len(a.calls()); n != 0 {
		t.Errorf("a nil-State update made %d HTTP calls, want 0", n)
	}
}

// TestIssueOperationsErrorPassthrough pins that each wrapper propagates
// the embedded client's sentinel UNMODIFIED: a 404 is forge.ErrNotFound
// and a 403 is forge.ErrForbidden on every method.
func TestIssueOperationsErrorPassthrough(t *testing.T) {
	state := "closed"
	for _, tc := range []struct {
		name string
		call func(ops forge.IssueOperations) error
	}{
		{"FetchIssue", func(ops forge.IssueOperations) error {
			_, err := ops.FetchIssue(context.Background(), issueScope, issueRepo, 7)
			return err
		}},
		{"FetchIssueComments", func(ops forge.IssueOperations) error {
			_, err := ops.FetchIssueComments(context.Background(), issueScope, issueRepo, 7)
			return err
		}},
		{"PostIssueComment", func(ops forge.IssueOperations) error {
			return ops.PostIssueComment(context.Background(), issueScope, issueRepo, 7, "x")
		}},
		{"SetIssueState", func(ops forge.IssueOperations) error {
			return ops.SetIssueState(context.Background(), issueScope, issueRepo, 7, forge.IssueStateUpdate{State: &state})
		}},
	} {
		for _, sc := range []struct {
			status int
			want   error
		}{
			{http.StatusNotFound, forge.ErrNotFound},
			{http.StatusForbidden, forge.ErrForbidden},
		} {
			t.Run(tc.name+"/"+http.StatusText(sc.status), func(t *testing.T) {
				a := newIssuesAdapter(t)
				a.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
					writeJSON(w, sc.status, `{"message":"x"}`)
				})
				if err := tc.call(a.f); !errors.Is(err, sc.want) {
					t.Errorf("err = %v, want errors.Is %v", err, sc.want)
				}
			})
		}
	}
}
