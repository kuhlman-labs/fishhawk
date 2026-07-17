package server

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/webhook"
)

const gitlabTestToken = "gl-shhh-token"

// recordingLogHandler captures slog records so per-kind tests can
// assert on the "webhook received" dispatch record the receiver emits.
type recordingLogHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *recordingLogHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *recordingLogHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r.Clone())
	return nil
}
func (h *recordingLogHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordingLogHandler) WithGroup(string) slog.Handler      { return h }

// findRecord returns the first captured record whose message equals msg
// and whose attrs satisfy match, or nil.
func (h *recordingLogHandler) find(msg string, match func(map[string]any) bool) *slog.Record {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i := range h.records {
		if h.records[i].Message != msg {
			continue
		}
		attrs := map[string]any{}
		h.records[i].Attrs(func(a slog.Attr) bool {
			attrs[a.Key] = a.Value.Any()
			return true
		})
		if match == nil || match(attrs) {
			r := h.records[i]
			return &r
		}
	}
	return nil
}

type gitlabServerOpts struct {
	noSecret   bool
	noStore    bool
	runRepo    run.Repository
	auditRepo  *prEventsAuditRepo
	dispatcher *webhook.Dispatcher
	logHandler *recordingLogHandler
}

func newGitLabWebhookServer(t *testing.T, opts gitlabServerOpts) (*Server, *webhook.MemoryStore) {
	t.Helper()
	cfg := Config{Addr: "127.0.0.1:0"}
	if !opts.noSecret {
		cfg.GitLabWebhookSecret = []byte(gitlabTestToken)
	}
	var store *webhook.MemoryStore
	if !opts.noStore {
		store = webhook.NewMemoryStore(0)
		cfg.WebhookDeliveries = store
	}
	if opts.runRepo != nil {
		cfg.RunRepo = opts.runRepo
	}
	if opts.auditRepo != nil {
		cfg.AuditRepo = opts.auditRepo
	}
	if opts.dispatcher != nil {
		cfg.WebhookDispatcher = opts.dispatcher
	}
	if opts.logHandler != nil {
		cfg.Logger = slog.New(opts.logHandler)
	}
	return New(cfg), store
}

func postGitLab(t *testing.T, s *Server, headers map[string]string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/webhooks/gitlab", bytes.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	return w
}

// gitlabHeaders returns a valid header set for a given event type/uuid.
func gitlabHeaders(event, uuid string) map[string]string {
	return map[string]string{
		"X-Gitlab-Token":      gitlabTestToken,
		"X-Gitlab-Event":      event,
		"X-Gitlab-Event-UUID": uuid,
		"Content-Type":        "application/json",
	}
}

// --- per-failure-mode status matrix ---

func TestHandleWebhookGitLab_SecretUnconfigured_503(t *testing.T) {
	s, _ := newGitLabWebhookServer(t, gitlabServerOpts{noSecret: true})
	w := postGitLab(t, s, gitlabHeaders("Issue Hook", "u1"), []byte(`{}`))
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
}

func TestHandleWebhookGitLab_StoreUnconfigured_503(t *testing.T) {
	s, _ := newGitLabWebhookServer(t, gitlabServerOpts{noStore: true})
	w := postGitLab(t, s, gitlabHeaders("Issue Hook", "u1"), []byte(`{}`))
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
}

func TestHandleWebhookGitLab_MissingToken_401(t *testing.T) {
	s, _ := newGitLabWebhookServer(t, gitlabServerOpts{})
	h := gitlabHeaders("Issue Hook", "u1")
	delete(h, "X-Gitlab-Token")
	w := postGitLab(t, s, h, []byte(`{}`))
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestHandleWebhookGitLab_WrongToken_401(t *testing.T) {
	s, _ := newGitLabWebhookServer(t, gitlabServerOpts{})
	h := gitlabHeaders("Issue Hook", "u1")
	h["X-Gitlab-Token"] = "nope"
	w := postGitLab(t, s, h, []byte(`{}`))
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestHandleWebhookGitLab_MissingEventHeader_400(t *testing.T) {
	s, _ := newGitLabWebhookServer(t, gitlabServerOpts{})
	h := gitlabHeaders("Issue Hook", "u1")
	delete(h, "X-Gitlab-Event")
	w := postGitLab(t, s, h, []byte(`{}`))
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestHandleWebhookGitLab_MissingEventUUID_400(t *testing.T) {
	s, _ := newGitLabWebhookServer(t, gitlabServerOpts{})
	h := gitlabHeaders("Issue Hook", "u1")
	delete(h, "X-Gitlab-Event-UUID")
	w := postGitLab(t, s, h, []byte(`{}`))
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestHandleWebhookGitLab_MalformedJSON_400(t *testing.T) {
	s, _ := newGitLabWebhookServer(t, gitlabServerOpts{})
	w := postGitLab(t, s, gitlabHeaders("Issue Hook", "u1"), []byte(`{not json`))
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestHandleWebhookGitLab_BodyTooLarge_413(t *testing.T) {
	s, _ := newGitLabWebhookServer(t, gitlabServerOpts{})
	big := bytes.Repeat([]byte("a"), maxWebhookBody+1)
	w := postGitLab(t, s, gitlabHeaders("Issue Hook", "u1"), big)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", w.Code)
	}
}

func TestHandleWebhookGitLab_DuplicateDelivery_202_NoSecondDispatch(t *testing.T) {
	// A real dispatcher with a recording ApprovalHandler lets us prove
	// the duplicate is NOT dispatched a second time.
	ah := &recordingWebhookApprovalHandler{}
	disp := &webhook.Dispatcher{ApprovalHandler: ah}
	s, _ := newGitLabWebhookServer(t, gitlabServerOpts{dispatcher: disp})

	body := []byte(`{"object_kind":"note","user":{"username":"alice"},
		"project":{"id":1,"path_with_namespace":"g/p"},
		"object_attributes":{"note":"/fishhawk approve","noteable_type":"Issue"},
		"issue":{"iid":7}}`)
	h := gitlabHeaders("Note Hook", "dup-uuid")

	if w := postGitLab(t, s, h, body); w.Code != http.StatusAccepted {
		t.Fatalf("first delivery status = %d, want 202", w.Code)
	}
	if w := postGitLab(t, s, h, body); w.Code != http.StatusAccepted {
		t.Fatalf("duplicate delivery status = %d, want 202", w.Code)
	}
	if got := ah.count(); got != 1 {
		t.Errorf("approval handler called %d times; duplicate must not re-dispatch", got)
	}
}

func TestHandleWebhookGitLab_RecognizedSkip_202(t *testing.T) {
	// Every GitLab dispatch path (approve/reject, parked run, or a
	// recognized skip like pipeline) returns nil from Handle, so the
	// receiver responds 202. The generic 5xx dispatch-error branch is
	// mirrored verbatim from the GitHub receiver for parity + future
	// GitLab run creation (E45.8); no current GitLab Handle path
	// produces a transient error, so it is exercised by the shared
	// GitHub mapping tests (TestWebhook_*), not reachable from here.
	disp := &webhook.Dispatcher{}
	s, _ := newGitLabWebhookServer(t, gitlabServerOpts{dispatcher: disp})
	body := []byte(`{"object_kind":"pipeline","user":{"username":"alice"},
		"project":{"id":1,"path_with_namespace":"g/p"},
		"object_attributes":{"status":"success"}}`)
	if w := postGitLab(t, s, gitlabHeaders("Pipeline Hook", "p1"), body); w.Code != http.StatusAccepted {
		t.Errorf("status = %d, want 202 (recognized skip)", w.Code)
	}
}

// The registration test (POST /webhooks/gitlab is actually routed) lives
// in handlers_test.go alongside the other *RouteRegistered guards; the
// full-stack CSRF-exempt path is pinned in server_test.go.

// recordingWebhookApprovalHandler records HandleApprovalCommand calls
// so the note-approve end-to-end test can assert the approve reached the
// dispatcher with the issue iid and a zero installation id.
type recordingWebhookApprovalHandler struct {
	mu    sync.Mutex
	calls []webhook.ApprovalCommandParams
}

func (h *recordingWebhookApprovalHandler) HandleApprovalCommand(_ context.Context, p webhook.ApprovalCommandParams) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = append(h.calls, p)
	return nil
}

func (h *recordingWebhookApprovalHandler) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.calls)
}

// --- integration: note approve end-to-end (binding condition test) ---

func TestHandleWebhookGitLab_NoteApprove_EndToEnd(t *testing.T) {
	ah := &recordingWebhookApprovalHandler{}
	disp := &webhook.Dispatcher{ApprovalHandler: ah}
	s, _ := newGitLabWebhookServer(t, gitlabServerOpts{dispatcher: disp})

	// Documented-shape Note-on-Issue payload carrying a /fishhawk approve.
	body := []byte(`{
		"object_kind": "note",
		"user": {"username": "reviewer"},
		"project": {"id": 42, "path_with_namespace": "group/project"},
		"object_attributes": {"note": "/fishhawk approve ship it", "noteable_type": "Issue"},
		"issue": {"iid": 88}
	}`)
	w := postGitLab(t, s, gitlabHeaders("Note Hook", "note-uuid"), body)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202:\n%s", w.Code, w.Body.String())
	}
	if ah.count() != 1 {
		t.Fatalf("approval handler called %d times, want 1", ah.count())
	}
	got := ah.calls[0]
	if got.Decision != webhook.MatchActionApprove {
		t.Errorf("Decision = %q, want approve", got.Decision)
	}
	if got.IssueNumber != 88 {
		t.Errorf("IssueNumber = %d, want 88 (issue iid)", got.IssueNumber)
	}
	if got.InstallationID != 0 {
		t.Errorf("InstallationID = %d, want 0 (GitLab carries none)", got.InstallationID)
	}
	if got.SenderLogin != "reviewer" {
		t.Errorf("SenderLogin = %q, want reviewer", got.SenderLogin)
	}
	if got.Comment != "ship it" {
		t.Errorf("Comment = %q, want 'ship it'", got.Comment)
	}
}

// --- per-kind full-path receiver tests (binding condition 3) ---

// TestHandleWebhookGitLab_PerKind_FullPath drives the HTTP handler once
// per accepted event kind (MR, Note, Issue, Pipeline, Job) with a
// documented-shape payload and asserts the 202 response code PLUS the
// structured-log dispatch record ("webhook received") the receiver
// emits for that kind — the mapped internal event (forge/event/action/
// repo). Parser units alone do not satisfy this criterion.
func TestHandleWebhookGitLab_PerKind_FullPath(t *testing.T) {
	cases := []struct {
		name       string
		event      string
		body       string
		wantEvent  string
		wantAction string
		wantRepo   string
	}{
		{
			name:  "merge_request",
			event: "Merge Request Hook",
			body: `{"object_kind":"merge_request","user":{"username":"root"},
				"project":{"id":1,"path_with_namespace":"mike/diaspora"},
				"object_attributes":{"iid":1,"action":"merge","state":"merged",
					"url":"http://example.com/diaspora/-/merge_requests/1",
					"last_commit":{"id":"da1560886d4f094c3e6c9ef40349f7d38b5d27d7"}}}`,
			wantEvent: "merge_request", wantAction: "merge", wantRepo: "mike/diaspora",
		},
		{
			name:  "note",
			event: "Note Hook",
			body: `{"object_kind":"note","user":{"username":"root"},
				"project":{"id":5,"path_with_namespace":"gitlab-org/gitlab-test"},
				"object_attributes":{"note":"/fishhawk run","noteable_type":"Issue"},
				"issue":{"iid":17}}`,
			wantEvent: "note", wantAction: "", wantRepo: "gitlab-org/gitlab-test",
		},
		{
			name:  "issue",
			event: "Issue Hook",
			body: `{"object_kind":"issue","user":{"username":"root"},
				"project":{"id":1,"path_with_namespace":"mike/diaspora"},
				"object_attributes":{"iid":23,"action":"update"},
				"labels":[{"title":"fishhawk"}],
				"changes":{"labels":{"previous":[],"current":[{"title":"fishhawk"}]}}}`,
			wantEvent: "issue", wantAction: "update", wantRepo: "mike/diaspora",
		},
		{
			name:  "pipeline",
			event: "Pipeline Hook",
			body: `{"object_kind":"pipeline","user":{"username":"root"},
				"project":{"id":1,"path_with_namespace":"mike/diaspora"},
				"object_attributes":{"id":31,"iid":3,"status":"success"}}`,
			wantEvent: "pipeline", wantAction: "success", wantRepo: "mike/diaspora",
		},
		{
			name:  "build",
			event: "Job Hook",
			body: `{"object_kind":"build","build_id":1977,"build_status":"success",
				"user":{"username":"root"},
				"project":{"id":380,"path_with_namespace":"gitlab-org/gitlab-test"}}`,
			wantEvent: "build", wantAction: "success", wantRepo: "gitlab-org/gitlab-test",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lh := &recordingLogHandler{}
			// Wire RunRepo/AuditRepo so the MR consumer path runs end to
			// end (it no-ops without a matching run); a real dispatcher
			// with no sub-handlers routes every kind's Match harmlessly.
			s, _ := newGitLabWebhookServer(t, gitlabServerOpts{
				logHandler: lh,
				runRepo:    &prEventsRunRepo{},
				auditRepo:  &prEventsAuditRepo{},
				dispatcher: &webhook.Dispatcher{},
			})
			w := postGitLab(t, s, gitlabHeaders(tc.event, tc.name+"-uuid"), []byte(tc.body))
			if w.Code != http.StatusAccepted {
				t.Fatalf("status = %d, want 202:\n%s", w.Code, w.Body.String())
			}
			rec := lh.find("webhook received", func(a map[string]any) bool {
				return a["forge"] == "gitlab" && a["event"] == tc.wantEvent
			})
			if rec == nil {
				t.Fatalf("no 'webhook received' record for event %q", tc.wantEvent)
			}
			attrs := map[string]any{}
			rec.Attrs(func(a slog.Attr) bool { attrs[a.Key] = a.Value.Any(); return true })
			if attrs["action"] != tc.wantAction {
				t.Errorf("logged action = %v, want %q", attrs["action"], tc.wantAction)
			}
			if attrs["repo"] != tc.wantRepo {
				t.Errorf("logged repo = %v, want %q", attrs["repo"], tc.wantRepo)
			}
		})
	}
}

// --- integration: MR merge/close drives the review stage ---

// gitLabMRBody builds a documented-shape Merge Request Hook payload with
// the given action + MR url (the /-/ infix form) and iid.
func gitLabMRBody(action, url string, iid int) []byte {
	return []byte(fmt.Sprintf(`{
		"object_kind": "merge_request",
		"user": {"username": "merger-user"},
		"project": {"id": 42, "path_with_namespace": "group/project"},
		"object_attributes": {
			"iid": %d,
			"action": %q,
			"url": %q,
			"last_commit": {"id": "cafef00d"}
		}
	}`, iid, action, url))
}

func TestHandleWebhookGitLab_MergeRequestMerged_TransitionsReview(t *testing.T) {
	runID := uuid.New()
	reviewStageID := uuid.New()
	// Stored URL deliberately differs from the webhook's URL only by the
	// /-/ infix — the iid-keyed resolver must still match (binding cond 1).
	storedURL := "https://gitlab.com/group/project/merge_requests/7"
	rr := &prEventsRunRepo{
		listResult: []*run.Run{{ID: runID, Repo: "group/project", PullRequestURL: &storedURL}},
		stages: map[uuid.UUID][]*run.Stage{
			runID: {{ID: reviewStageID, RunID: runID, Type: run.StageTypeReview, State: run.StageStateAwaitingApproval}},
		},
	}
	ar := &prEventsAuditRepo{}
	s := prEventsTestServer(t, rr, ar)

	body := gitLabMRBody("merge", "https://gitlab.com/group/project/-/merge_requests/7", 7)
	s.handleGitLabMergeRequest(context.Background(), body)

	if len(rr.transitions) != 1 {
		t.Fatalf("transitions = %d, want 1", len(rr.transitions))
	}
	if rr.transitions[0].StageID != reviewStageID || rr.transitions[0].To != run.StageStateSucceeded {
		t.Errorf("transition = %+v, want review→succeeded", rr.transitions[0])
	}
	row := findCategory(ar.appended, CategoryPRMerged)
	if row == nil {
		t.Fatalf("missing pr_merged audit row; got %v", auditCategories(ar.appended))
	}
	if row.ActorSubject == nil || *row.ActorSubject != "merger-user" {
		t.Errorf("merger = %v, want merger-user", row.ActorSubject)
	}
}

func TestHandleWebhookGitLab_MergeRequestClosed_CancelsReview(t *testing.T) {
	runID := uuid.New()
	reviewStageID := uuid.New()
	storedURL := "https://gitlab.com/group/project/-/merge_requests/7"
	rr := &prEventsRunRepo{
		listResult: []*run.Run{{ID: runID, Repo: "group/project", PullRequestURL: &storedURL}},
		stages: map[uuid.UUID][]*run.Stage{
			runID: {{ID: reviewStageID, RunID: runID, Type: run.StageTypeReview, State: run.StageStateAwaitingApproval}},
		},
	}
	ar := &prEventsAuditRepo{}
	s := prEventsTestServer(t, rr, ar)

	body := gitLabMRBody("close", "https://gitlab.com/group/project/-/merge_requests/7", 7)
	s.handleGitLabMergeRequest(context.Background(), body)

	if len(rr.transitions) != 1 || rr.transitions[0].To != run.StageStateCancelled {
		t.Fatalf("transitions = %+v, want review→cancelled", rr.transitions)
	}
	if findCategory(ar.appended, CategoryPRClosedWithoutMerge) == nil {
		t.Fatalf("missing pr_closed_without_merge audit row; got %v", auditCategories(ar.appended))
	}
}

func TestHandleWebhookGitLab_MergeRequest_NonTerminalActionIgnored(t *testing.T) {
	rr := &prEventsRunRepo{listResult: []*run.Run{}}
	ar := &prEventsAuditRepo{}
	s := prEventsTestServer(t, rr, ar)
	// action "update" is not a review-gate signal — no lookup, no audit.
	s.handleGitLabMergeRequest(context.Background(), gitLabMRBody("update", "https://gitlab.com/group/project/-/merge_requests/7", 7))
	if len(rr.transitions) != 0 || len(ar.appended) != 0 {
		t.Errorf("update action must be ignored; transitions=%d audits=%d", len(rr.transitions), len(ar.appended))
	}
}

func TestFindRunByGitLabMR_ResolvesByIIDNotURL(t *testing.T) {
	// Two runs on the same project; only the iid distinguishes them.
	// The webhook URL uses the /-/ infix; the stored URLs use the legacy
	// form. Exact string equality would match neither — the iid key must.
	url7 := "https://gitlab.com/group/project/merge_requests/7"
	url9 := "https://gitlab.com/group/project/merge_requests/9"
	rr := &prEventsRunRepo{
		listResult: []*run.Run{
			{ID: uuid.New(), Repo: "group/project", PullRequestURL: &url9},
			{ID: uuid.New(), Repo: "group/project", PullRequestURL: &url7},
		},
	}
	s := prEventsTestServer(t, rr, &prEventsAuditRepo{})
	got := s.findRunByGitLabMR(context.Background(), "group/project", 7, "https://gitlab.com/group/project/-/merge_requests/7")
	if got == nil || got.PullRequestURL == nil || *got.PullRequestURL != url7 {
		t.Fatalf("resolved run = %+v, want the iid-7 run", got)
	}
}

func TestParseGitLabMRIID(t *testing.T) {
	cases := map[string]int{
		"https://gitlab.com/g/p/-/merge_requests/7":       7,
		"https://gitlab.com/g/p/merge_requests/42":        42,
		"https://gitlab.com/g/p/-/merge_requests/9/diffs": 9,
		"https://gitlab.com/g/p/-/issues/3":               0, // not an MR
		"not a url":                                       0,
	}
	for in, want := range cases {
		got, ok := parseGitLabMRIID(in)
		if want == 0 {
			if ok {
				t.Errorf("parseGitLabMRIID(%q) = %d, want no match", in, got)
			}
			continue
		}
		if !ok || got != want {
			t.Errorf("parseGitLabMRIID(%q) = %d,%v want %d", in, got, ok, want)
		}
	}
}
