package gitlabclient

// Self-contained tests for the four issue-thread methods (E50.17 / #2900).
// gitlabclient has no shared client-construction helper, so this file
// builds its own httptest GitLab per case and drives the REAL *http.Client
// through it — no stub Doer — so the Link-header pagination walk, which
// follows ABSOLUTE URLs GitLab hands back, is exercised over a real
// listener rather than a canned response sequence.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// issueRequest is one request the fake recorded: method, path (with query)
// and the decoded JSON body (nil for a bodiless GET).
type issueRequest struct {
	Method string
	Path   string
	Query  string
	Token  string
	Body   map[string]any
}

// issueServer is a minimal httptest GitLab whose mux the caller populates.
// Every request is recorded in arrival order so a test can assert the
// exact shape the client emitted.
type issueServer struct {
	t   *testing.T
	srv *httptest.Server
	mux *http.ServeMux
	mu  sync.Mutex
	log []issueRequest
}

func newIssueServer(t *testing.T) *issueServer {
	t.Helper()
	s := &issueServer{t: t, mux: http.NewServeMux()}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := issueRequest{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Token: r.Header.Get("PRIVATE-TOKEN")}
		if r.Body != nil {
			raw, _ := io.ReadAll(r.Body)
			if len(raw) > 0 {
				_ = json.Unmarshal(raw, &rec.Body)
			}
			r.Body = io.NopCloser(strings.NewReader(string(raw)))
		}
		s.mu.Lock()
		s.log = append(s.log, rec)
		s.mu.Unlock()
		s.mux.ServeHTTP(w, r)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *issueServer) client() *Client {
	return New(s.srv.URL, "glpat-test", WithHTTPClient(s.srv.Client()))
}

func (s *issueServer) requests() []issueRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]issueRequest, len(s.log))
	copy(out, s.log)
	return out
}

func writeIssueJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

// --- GetIssue -------------------------------------------------------------

func TestGitLabClient_GetIssue_RequestShapeAndResult(t *testing.T) {
	s := newIssueServer(t)
	s.mux.HandleFunc("GET /api/v4/projects/42/issues/7", func(w http.ResponseWriter, r *http.Request) {
		writeIssueJSON(w, http.StatusOK, `{"iid":7,"title":"Parent","description":"body text","state":"opened","labels":["type:epic","area:server"],"web_url":"https://gl.example/g/p/-/issues/7"}`)
	})

	is, err := s.client().GetIssue(context.Background(), 42, 7)
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	if is.IID != 7 || is.Title != "Parent" || is.Description != "body text" || is.State != "opened" {
		t.Errorf("Issue = %+v, want iid 7 / Parent / body text / opened", is)
	}
	if len(is.Labels) != 2 || is.Labels[0] != "type:epic" || is.Labels[1] != "area:server" {
		t.Errorf("Labels = %v, want [type:epic area:server]", is.Labels)
	}
	if is.WebURL != "https://gl.example/g/p/-/issues/7" {
		t.Errorf("WebURL = %q", is.WebURL)
	}
	reqs := s.requests()
	if len(reqs) != 1 || reqs[0].Method != http.MethodGet || reqs[0].Path != "/api/v4/projects/42/issues/7" {
		t.Fatalf("requests = %+v, want one GET /api/v4/projects/42/issues/7", reqs)
	}
	if reqs[0].Token != "glpat-test" {
		t.Errorf("PRIVATE-TOKEN = %q, want glpat-test", reqs[0].Token)
	}
}

func TestGitLabClient_GetIssue_APIError(t *testing.T) {
	s := newIssueServer(t)
	s.mux.HandleFunc("GET /api/v4/projects/42/issues/7", func(w http.ResponseWriter, r *http.Request) {
		writeIssueJSON(w, http.StatusNotFound, `{"message":"404 Not found"}`)
	})

	is, err := s.client().GetIssue(context.Background(), 42, 7)
	if is != nil {
		t.Errorf("Issue = %+v on error, want nil", is)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want *APIError", err)
	}
	if apiErr.StatusCode != http.StatusNotFound || apiErr.Op != "get issue" {
		t.Errorf("APIError = %+v, want status 404 op get issue", apiErr)
	}
}

func TestGitLabClient_GetIssue_ValidatesArgs(t *testing.T) {
	s := newIssueServer(t)
	c := s.client()
	if _, err := c.GetIssue(context.Background(), 0, 7); err == nil {
		t.Error("GetIssue(project 0) = nil, want an error")
	}
	if _, err := c.GetIssue(context.Background(), 42, 0); err == nil {
		t.Error("GetIssue(iid 0) = nil, want an error")
	}
	if n := len(s.requests()); n != 0 {
		t.Errorf("argument validation must fire before any HTTP call; got %d requests", n)
	}
}

// --- ListIssueNotes -------------------------------------------------------

// TestGitLabClient_ListIssueNotes_PagesToExhaustion is the named
// counterfactual vehicle for the Link-header paging loop: page 1 carries a
// rel="next" Link to page 2, and the result must carry BOTH pages in order.
// Deleting the loop (returning after the first page) drops note 3 and
// reddens the length assertion.
func TestGitLabClient_ListIssueNotes_PagesToExhaustion(t *testing.T) {
	s := newIssueServer(t)
	s.mux.HandleFunc("GET /api/v4/projects/42/issues/7/notes", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("page") {
		case "", "1":
			w.Header().Set("Link",
				`<`+s.srv.URL+`/api/v4/projects/42/issues/7/notes?page=2&per_page=100>; rel="next", `+
					`<`+s.srv.URL+`/api/v4/projects/42/issues/7/notes?page=1&per_page=100>; rel="first"`)
			writeIssueJSON(w, http.StatusOK, `[
				{"id":1,"body":"first","system":false,"created_at":"2026-09-01T00:00:00Z","author":{"username":"alice"}},
				{"id":2,"body":"changed the description","system":true,"created_at":"2026-09-01T00:01:00Z","author":{"username":"alice"}}
			]`)
		case "2":
			w.Header().Set("Link", `<`+s.srv.URL+`/api/v4/projects/42/issues/7/notes?page=1&per_page=100>; rel="prev"`)
			writeIssueJSON(w, http.StatusOK, `[{"id":3,"body":"<!-- fishhawk:key -->\nlinked","system":false,"created_at":"2026-09-01T00:02:00Z","author":{"username":"bot"}}]`)
		default:
			t.Errorf("unexpected page %q", r.URL.Query().Get("page"))
			writeIssueJSON(w, http.StatusBadRequest, `{}`)
		}
	})

	notes, err := s.client().ListIssueNotes(context.Background(), 42, 7)
	if err != nil {
		t.Fatalf("ListIssueNotes: %v", err)
	}
	if len(notes) != 3 {
		t.Fatalf("len(notes) = %d, want 3 (both pages walked)", len(notes))
	}
	if notes[0].ID != 1 || notes[0].Author != "alice" || notes[0].Body != "first" || notes[0].CreatedAt != "2026-09-01T00:00:00Z" {
		t.Errorf("notes[0] = %+v, want id 1 / alice / first", notes[0])
	}
	if !notes[1].System {
		t.Errorf("notes[1].System = false, want true (system notes are surfaced, not filtered)")
	}
	if notes[2].ID != 3 || notes[2].Body != "<!-- fishhawk:key -->\nlinked" || notes[2].Author != "bot" {
		t.Errorf("notes[2] = %+v, want the page-2 marker note byte-intact", notes[2])
	}

	reqs := s.requests()
	if len(reqs) != 2 {
		t.Fatalf("requests = %d, want 2 (one per page)", len(reqs))
	}
	if !strings.Contains(reqs[0].Query, "per_page=100") {
		t.Errorf("first page query = %q, want per_page=100", reqs[0].Query)
	}
	if !strings.Contains(reqs[1].Query, "page=2") {
		t.Errorf("second request query = %q, want the Link's page=2", reqs[1].Query)
	}
	for i, r := range reqs {
		if r.Token != "glpat-test" {
			t.Errorf("request %d PRIVATE-TOKEN = %q, want glpat-test on every page", i, r.Token)
		}
	}
}

// TestGitLabClient_ListIssueNotes_RefusesOffOriginNextLink pins the
// same-origin guard on the Link walk: a rel="next" pointing at a DIFFERENT
// host is refused, and that host is never dialed — the PRIVATE-TOKEN
// header must not follow a forge-supplied URL off-instance. The foreign
// host is a reachable in-test server so that deleting the guard SUCCEEDS
// against it (and reddens the zero-hit assertion) rather than failing on
// a connection error.
func TestGitLabClient_ListIssueNotes_RefusesOffOriginNextLink(t *testing.T) {
	var foreignHits atomic.Int64
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		foreignHits.Add(1)
		writeIssueJSON(w, http.StatusOK, `[]`)
	}))
	t.Cleanup(foreign.Close)

	s := newIssueServer(t)
	s.mux.HandleFunc("GET /api/v4/projects/42/issues/7/notes", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Link", `<`+foreign.URL+`/api/v4/projects/42/issues/7/notes?page=2>; rel="next"`)
		writeIssueJSON(w, http.StatusOK, `[{"id":1,"body":"first","author":{"username":"alice"}}]`)
	})

	notes, err := s.client().ListIssueNotes(context.Background(), 42, 7)
	if err == nil {
		t.Fatalf("ListIssueNotes = %v, nil; want a refusal of the off-origin next link", notes)
	}
	if !strings.Contains(err.Error(), "refusing next-page link") {
		t.Errorf("err = %v, want the same-origin refusal", err)
	}
	if got := foreignHits.Load(); got != 0 {
		t.Errorf("foreign host received %d requests, want 0 (token must not leave the configured origin)", got)
	}
}

// TestGitLabClient_ListIssueNotes_RefusesOffOriginRedirect pins the guard the
// off-origin-Link case above does NOT reach: a SAME-origin notes endpoint that
// answers with an HTTP 302 to a foreign host. sameOrigin validated the initial
// URL, so without a redirect boundary the default *http.Client would follow the
// 302 and carry the custom PRIVATE-TOKEN header there (the stdlib strips only
// Authorization/Cookie on a cross-host hop, not a custom header). The foreign
// host is a reachable in-test server that RECORDS any token it receives, so
// deleting the CheckRedirect guard in doNoOffOriginRedirect SUCCEEDS against it
// and reddens BOTH the zero-hit and zero-token-disclosure assertions rather
// than failing on a connection error.
func TestGitLabClient_ListIssueNotes_RefusesOffOriginRedirect(t *testing.T) {
	var foreignHits atomic.Int64
	var leakedToken atomic.Value // string
	leakedToken.Store("")
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		foreignHits.Add(1)
		leakedToken.Store(r.Header.Get("PRIVATE-TOKEN"))
		writeIssueJSON(w, http.StatusOK, `[]`)
	}))
	t.Cleanup(foreign.Close)

	s := newIssueServer(t)
	s.mux.HandleFunc("GET /api/v4/projects/42/issues/7/notes", func(w http.ResponseWriter, r *http.Request) {
		// Same-origin endpoint that 302s off-instance. A default client would
		// chase this and re-attach PRIVATE-TOKEN to the foreign request.
		http.Redirect(w, r, foreign.URL+"/api/v4/projects/42/issues/7/notes?page=2", http.StatusFound)
	})

	notes, err := s.client().ListIssueNotes(context.Background(), 42, 7)
	if err == nil {
		t.Fatalf("ListIssueNotes = %v, nil; want a refusal of the off-origin redirect", notes)
	}
	if !strings.Contains(err.Error(), "refusing next-page link") {
		t.Errorf("err = %v, want the same-origin refusal from CheckRedirect", err)
	}
	if got := foreignHits.Load(); got != 0 {
		t.Errorf("foreign host received %d requests, want 0 (token must not follow a redirect off-instance)", got)
	}
	if tok := leakedToken.Load().(string); tok != "" {
		t.Errorf("foreign host saw PRIVATE-TOKEN %q, want it never disclosed", tok)
	}
}

func TestGitLabClient_ListIssueNotes_APIError(t *testing.T) {
	s := newIssueServer(t)
	s.mux.HandleFunc("GET /api/v4/projects/42/issues/7/notes", func(w http.ResponseWriter, r *http.Request) {
		writeIssueJSON(w, http.StatusForbidden, `{"message":"403 Forbidden"}`)
	})
	_, err := s.client().ListIssueNotes(context.Background(), 42, 7)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusForbidden || apiErr.Op != "list issue notes" {
		t.Fatalf("err = %v, want *APIError status 403 op list issue notes", err)
	}
}

func TestGitLabClient_ListIssueNotes_ValidatesArgs(t *testing.T) {
	s := newIssueServer(t)
	c := s.client()
	if _, err := c.ListIssueNotes(context.Background(), 0, 7); err == nil {
		t.Error("ListIssueNotes(project 0) = nil, want an error")
	}
	if _, err := c.ListIssueNotes(context.Background(), 42, -1); err == nil {
		t.Error("ListIssueNotes(iid -1) = nil, want an error")
	}
	if n := len(s.requests()); n != 0 {
		t.Errorf("argument validation must fire before any HTTP call; got %d requests", n)
	}
}

func TestNextPageURL(t *testing.T) {
	for _, tc := range []struct {
		name, link, want string
	}{
		{"empty", "", ""},
		{"next only", `<https://gl/api?page=2>; rel="next"`, "https://gl/api?page=2"},
		{"next among others", `<https://gl/api?page=1>; rel="prev", <https://gl/api?page=3>; rel="next", <https://gl/api?page=9>; rel="last"`, "https://gl/api?page=3"},
		{"no next", `<https://gl/api?page=1>; rel="first", <https://gl/api?page=9>; rel="last"`, ""},
		{"malformed segment skipped", `garbage, <https://gl/api?page=2>; rel="next"`, "https://gl/api?page=2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := nextPageURL(tc.link); got != tc.want {
				t.Errorf("nextPageURL(%q) = %q, want %q", tc.link, got, tc.want)
			}
		})
	}
}

// --- CreateIssueNote ------------------------------------------------------

func TestGitLabClient_CreateIssueNote_RequestShapeAndResult(t *testing.T) {
	s := newIssueServer(t)
	s.mux.HandleFunc("POST /api/v4/projects/42/issues/7/notes", func(w http.ResponseWriter, r *http.Request) {
		writeIssueJSON(w, http.StatusCreated, `{"id":99,"body":"<!-- k -->\nhello","system":false,"created_at":"2026-09-02T00:00:00Z","author":{"username":"bot"}}`)
	})

	n, err := s.client().CreateIssueNote(context.Background(), 42, 7, "<!-- k -->\nhello")
	if err != nil {
		t.Fatalf("CreateIssueNote: %v", err)
	}
	if n.ID != 99 || n.Body != "<!-- k -->\nhello" || n.Author != "bot" || n.CreatedAt != "2026-09-02T00:00:00Z" {
		t.Errorf("Note = %+v, want id 99 / marker body / bot", n)
	}
	reqs := s.requests()
	if len(reqs) != 1 || reqs[0].Method != http.MethodPost || reqs[0].Path != "/api/v4/projects/42/issues/7/notes" {
		t.Fatalf("requests = %+v, want one POST .../issues/7/notes", reqs)
	}
	if got := reqs[0].Body["body"]; got != "<!-- k -->\nhello" {
		t.Errorf("request body.body = %v, want the note text byte-intact", got)
	}
}

func TestGitLabClient_CreateIssueNote_APIError(t *testing.T) {
	s := newIssueServer(t)
	s.mux.HandleFunc("POST /api/v4/projects/42/issues/7/notes", func(w http.ResponseWriter, r *http.Request) {
		writeIssueJSON(w, http.StatusNotFound, `{"message":"404 Not found"}`)
	})
	n, err := s.client().CreateIssueNote(context.Background(), 42, 7, "x")
	if n != nil {
		t.Errorf("Note = %+v on error, want nil", n)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusNotFound || apiErr.Op != "create issue note" {
		t.Fatalf("err = %v, want *APIError status 404 op create issue note", err)
	}
}

func TestGitLabClient_CreateIssueNote_ValidatesArgs(t *testing.T) {
	s := newIssueServer(t)
	c := s.client()
	if _, err := c.CreateIssueNote(context.Background(), 0, 7, "x"); err == nil {
		t.Error("CreateIssueNote(project 0) = nil, want an error")
	}
	if _, err := c.CreateIssueNote(context.Background(), 42, 0, "x"); err == nil {
		t.Error("CreateIssueNote(iid 0) = nil, want an error")
	}
	if _, err := c.CreateIssueNote(context.Background(), 42, 7, "   "); err == nil {
		t.Error("CreateIssueNote(blank body) = nil, want an error")
	}
	if n := len(s.requests()); n != 0 {
		t.Errorf("argument validation must fire before any HTTP call; got %d requests", n)
	}
}

// --- UpdateIssue ----------------------------------------------------------

// TestGitLabClient_UpdateIssue_SendsStateEvent pins the exact wire shape of
// a close: a PUT carrying `state_event: "close"` and NO `state` key — the
// v4 edit-issue endpoint changes state only through state_event.
func TestGitLabClient_UpdateIssue_SendsStateEvent(t *testing.T) {
	s := newIssueServer(t)
	s.mux.HandleFunc("PUT /api/v4/projects/42/issues/7", func(w http.ResponseWriter, r *http.Request) {
		writeIssueJSON(w, http.StatusOK, `{"iid":7,"title":"Parent","state":"closed","labels":[]}`)
	})

	is, err := s.client().UpdateIssue(context.Background(), 42, 7, UpdateIssueParams{StateEvent: "close"})
	if err != nil {
		t.Fatalf("UpdateIssue: %v", err)
	}
	if is.State != "closed" || is.IID != 7 {
		t.Errorf("Issue = %+v, want iid 7 state closed", is)
	}
	reqs := s.requests()
	if len(reqs) != 1 || reqs[0].Method != http.MethodPut || reqs[0].Path != "/api/v4/projects/42/issues/7" {
		t.Fatalf("requests = %+v, want one PUT /api/v4/projects/42/issues/7", reqs)
	}
	if got := reqs[0].Body["state_event"]; got != "close" {
		t.Errorf("body.state_event = %v, want close", got)
	}
	if _, has := reqs[0].Body["state"]; has {
		t.Errorf("body carries a `state` key (%v); GitLab refuses direct state writes", reqs[0].Body["state"])
	}
	for _, k := range []string{"description", "labels"} {
		if _, has := reqs[0].Body[k]; has {
			t.Errorf("body carries unset field %q = %v; only set fields may be transmitted", k, reqs[0].Body[k])
		}
	}
}

func TestGitLabClient_UpdateIssue_DescriptionAndLabels(t *testing.T) {
	s := newIssueServer(t)
	s.mux.HandleFunc("PUT /api/v4/projects/42/issues/7", func(w http.ResponseWriter, r *http.Request) {
		writeIssueJSON(w, http.StatusOK, `{"iid":7,"state":"opened","labels":["a","b"],"description":""}`)
	})
	desc := ""
	labels := []string{"a", "b"}
	if _, err := s.client().UpdateIssue(context.Background(), 42, 7, UpdateIssueParams{Description: &desc, Labels: &labels}); err != nil {
		t.Fatalf("UpdateIssue: %v", err)
	}
	body := s.requests()[0].Body
	if got, has := body["description"]; !has || got != "" {
		t.Errorf("body.description = %v (present=%v), want an explicit empty string (clears the body)", got, has)
	}
	if got := body["labels"]; got != "a,b" {
		t.Errorf("body.labels = %v, want the comma-joined \"a,b\"", got)
	}
	if _, has := body["state_event"]; has {
		t.Errorf("body carries state_event = %v with no state change requested", body["state_event"])
	}
}

func TestGitLabClient_UpdateIssue_RefusesNoFields(t *testing.T) {
	s := newIssueServer(t)
	if _, err := s.client().UpdateIssue(context.Background(), 42, 7, UpdateIssueParams{}); err == nil {
		t.Error("UpdateIssue with no fields = nil, want a refusal")
	}
	if n := len(s.requests()); n != 0 {
		t.Errorf("a no-field update must be refused before any HTTP call; got %d requests", n)
	}
}

func TestGitLabClient_UpdateIssue_APIError(t *testing.T) {
	s := newIssueServer(t)
	s.mux.HandleFunc("PUT /api/v4/projects/42/issues/7", func(w http.ResponseWriter, r *http.Request) {
		writeIssueJSON(w, http.StatusNotFound, `{"message":"404 Not found"}`)
	})
	is, err := s.client().UpdateIssue(context.Background(), 42, 7, UpdateIssueParams{StateEvent: "close"})
	if is != nil {
		t.Errorf("Issue = %+v on error, want nil", is)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusNotFound || apiErr.Op != "update issue" {
		t.Fatalf("err = %v, want *APIError status 404 op update issue", err)
	}
}

func TestGitLabClient_UpdateIssue_ValidatesArgs(t *testing.T) {
	s := newIssueServer(t)
	c := s.client()
	if _, err := c.UpdateIssue(context.Background(), 0, 7, UpdateIssueParams{StateEvent: "close"}); err == nil {
		t.Error("UpdateIssue(project 0) = nil, want an error")
	}
	if _, err := c.UpdateIssue(context.Background(), 42, 0, UpdateIssueParams{StateEvent: "close"}); err == nil {
		t.Error("UpdateIssue(iid 0) = nil, want an error")
	}
	if n := len(s.requests()); n != 0 {
		t.Errorf("argument validation must fire before any HTTP call; got %d requests", n)
	}
}
