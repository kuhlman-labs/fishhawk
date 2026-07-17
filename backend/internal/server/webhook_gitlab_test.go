package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	// produces a transient error, so it can't be reached through the real
	// *webhook.Dispatcher here. Its unmark-on-failure behavior is pinned
	// directly at TestDispatchGitLabDelivery_DispatchError_Unmarks.
	disp := &webhook.Dispatcher{}
	s, _ := newGitLabWebhookServer(t, gitlabServerOpts{dispatcher: disp})
	body := []byte(`{"object_kind":"pipeline","user":{"username":"alice"},
		"project":{"id":1,"path_with_namespace":"g/p"},
		"object_attributes":{"status":"success"}}`)
	if w := postGitLab(t, s, gitlabHeaders("Pipeline Hook", "p1"), body); w.Code != http.StatusAccepted {
		t.Errorf("status = %d, want 202 (recognized skip)", w.Code)
	}
}

// stubDeliveryStore is a DeliveryStore whose Mark result is fixed by
// markErr and whose Unmark calls are recorded, so the receiver's two 5xx
// branches (store-record failure, dispatch failure) can be exercised
// without a real store or dispatcher.
type stubDeliveryStore struct {
	markErr  error
	unmarked []string
}

func (s *stubDeliveryStore) Mark(string) error { return s.markErr }
func (s *stubDeliveryStore) Unmark(id string) error {
	s.unmarked = append(s.unmarked, id)
	return nil
}

// TestHandleWebhookGitLab_StoreMarkFailure_500 pins the reachable
// store-record 5xx branch (E45.6 concern 3): a non-duplicate error from
// the delivery store's Mark surfaces as 500 'failed to record delivery'.
// Because the delivery was never recorded, GitLab's retry re-processes —
// so this branch (unlike the dispatch branch) needs no unmark.
func TestHandleWebhookGitLab_StoreMarkFailure_500(t *testing.T) {
	store := &stubDeliveryStore{markErr: errors.New("delivery store unavailable")}
	s := New(Config{
		Addr:                "127.0.0.1:0",
		GitLabWebhookSecret: []byte(gitlabTestToken),
		WebhookDeliveries:   store,
	})
	body := []byte(`{"object_kind":"issue","user":{"username":"alice"},
		"project":{"id":1,"path_with_namespace":"g/p"},
		"object_attributes":{"iid":1,"action":"open"}}`)
	w := postGitLab(t, s, gitlabHeaders("Issue Hook", "u1"), body)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 on store Mark failure:\n%s", w.Code, w.Body.String())
	}
	if len(store.unmarked) != 0 {
		t.Errorf("unmarked = %v, want none (nothing was recorded to undo)", store.unmarked)
	}
}

// TestDispatchGitLabDelivery_DispatchError_Unmarks pins the high-severity
// retry contract (E45.6 concerns 1 & 2): when dispatch of an
// already-recorded delivery fails, the delivery is UNMARKED so GitLab's
// retry re-records and re-processes it rather than encountering the
// recorded delivery and being deduped to a 202 — a permanent drop. The
// real *webhook.Dispatcher has no GitLab path that returns an error, so
// this drives the receiver's dispatch seam directly with an erroring
// dispatch func and asserts the delivery is releasable afterward.
func TestDispatchGitLabDelivery_DispatchError_Unmarks(t *testing.T) {
	store := webhook.NewMemoryStore(0)
	const deliveryID = "gitlab:dispatch-fail"
	if err := store.Mark(deliveryID); err != nil {
		t.Fatalf("seed Mark: %v", err)
	}
	s := New(Config{Addr: "127.0.0.1:0", WebhookDeliveries: store})
	ev := webhook.Event{DeliveryID: deliveryID, Type: "issue", Forge: webhook.ForgeGitLab}

	wantErr := errors.New("transient backend failure")
	gotErr := s.dispatchGitLabDelivery(context.Background(), ev, func(context.Context, webhook.Event) error {
		return wantErr
	})
	if !errors.Is(gotErr, wantErr) {
		t.Fatalf("dispatchGitLabDelivery err = %v, want the dispatch error surfaced (→ 500)", gotErr)
	}
	// The delivery must be released: a fresh Mark succeeds, proving the
	// retry re-processes instead of being deduped to a 202.
	if err := store.Mark(deliveryID); err != nil {
		t.Errorf("post-dispatch-failure Mark = %v, want nil (delivery must be unmarked so the retry re-processes)", err)
	}
}

// TestDispatchGitLabDelivery_Success_KeepsRecord confirms the happy path
// does NOT unmark: a successful dispatch leaves the delivery recorded so a
// redelivery (e.g. a lost 202) is correctly deduped rather than reprocessed.
func TestDispatchGitLabDelivery_Success_KeepsRecord(t *testing.T) {
	store := webhook.NewMemoryStore(0)
	const deliveryID = "gitlab:dispatch-ok"
	if err := store.Mark(deliveryID); err != nil {
		t.Fatalf("seed Mark: %v", err)
	}
	s := New(Config{Addr: "127.0.0.1:0", WebhookDeliveries: store})
	ev := webhook.Event{DeliveryID: deliveryID, Type: "issue", Forge: webhook.ForgeGitLab}

	if err := s.dispatchGitLabDelivery(context.Background(), ev, func(context.Context, webhook.Event) error {
		return nil
	}); err != nil {
		t.Fatalf("dispatchGitLabDelivery err = %v, want nil on success", err)
	}
	if err := store.Mark(deliveryID); !errors.Is(err, webhook.ErrDeliveryDuplicate) {
		t.Errorf("post-success Mark = %v, want ErrDeliveryDuplicate (record must stand)", err)
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
	if err := s.handleGitLabMergeRequest(context.Background(), body); err != nil {
		t.Fatalf("handleGitLabMergeRequest err = %v, want nil on a resolved merge", err)
	}

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
	if err := s.handleGitLabMergeRequest(context.Background(), body); err != nil {
		t.Fatalf("handleGitLabMergeRequest err = %v, want nil on a resolved close", err)
	}

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
	if err := s.handleGitLabMergeRequest(context.Background(), gitLabMRBody("update", "https://gitlab.com/group/project/-/merge_requests/7", 7)); err != nil {
		t.Fatalf("handleGitLabMergeRequest err = %v, want nil on a non-terminal action", err)
	}
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
	got, err := s.findRunByGitLabMR(context.Background(), "group/project", 7, "https://gitlab.com/group/project/-/merge_requests/7")
	if err != nil {
		t.Fatalf("findRunByGitLabMR err = %v, want nil", err)
	}
	if got == nil || got.PullRequestURL == nil || *got.PullRequestURL != url7 {
		t.Fatalf("resolved run = %+v, want the iid-7 run", got)
	}
}

func TestFindRunByGitLabMR_NoMatch_WarnsAndReturnsNil(t *testing.T) {
	// A run exists on the project but neither its iid nor its URL match
	// the incoming MR — the merge/close event must NOT silently return
	// nil (which strands the review stage); it must warn so the miss is
	// diagnosable. The GitLab webhook is the only review-gate signal, so
	// there is no reconciler backstop to catch this.
	url9 := "https://gitlab.com/group/project/merge_requests/9"
	rr := &prEventsRunRepo{
		listResult: []*run.Run{{ID: uuid.New(), Repo: "group/project", PullRequestURL: &url9}},
	}
	lh := &recordingLogHandler{}
	s := New(Config{
		Addr:      "127.0.0.1:0",
		RunRepo:   rr,
		AuditRepo: &prEventsAuditRepo{},
		Logger:    slog.New(lh),
	})
	got, err := s.findRunByGitLabMR(context.Background(), "group/project", 7,
		"https://gitlab.com/group/project/-/merge_requests/7")
	if err != nil {
		t.Fatalf("findRunByGitLabMR err = %v, want nil for a genuine no-match", err)
	}
	if got != nil {
		t.Fatalf("resolved run = %+v, want nil for an unmatched MR", got)
	}
	if lh.find("gitlab merge_request: no run matched; review stage will not transition", nil) == nil {
		t.Fatalf("expected a warn log on the unmatched-MR miss; got none")
	}
}

// TestFindRunByGitLabMR_ResolvesByExactURLBeyondWindow pins the E45.20/
// E45.21 done-means: a run whose backing MR has aged past the 50-recent-
// runs project window (so it's absent from the windowed iid/normalized
// scan) is still resolved when its stored PullRequestURL byte-matches the
// webhook URL exactly, via the non-windowed exact-URL DB filter supplement.
func TestFindRunByGitLabMR_ResolvesByExactURLBeyondWindow(t *testing.T) {
	mrURL := "https://gitlab.com/group/project/-/merge_requests/7"
	// The windowed project-scoped scan sees only newer runs whose stored
	// URLs neither iid-match nor normalize-match the incoming MR — modeling
	// the target having aged out of the 50-run window.
	urlOther := "https://gitlab.com/group/project/-/merge_requests/999"
	windowed := []*run.Run{{ID: uuid.New(), Repo: "group/project", PullRequestURL: &urlOther}}
	// The exact-URL DB filter is not recency-windowed and resolves the
	// aged-out target because its stored URL byte-matches the webhook URL.
	target := &run.Run{ID: uuid.New(), Repo: "group/project", PullRequestURL: &mrURL}
	rr := &prEventsRunRepo{
		listResult:     windowed,
		exactURLResult: []*run.Run{target},
	}
	s := prEventsTestServer(t, rr, &prEventsAuditRepo{})
	got, err := s.findRunByGitLabMR(context.Background(), "group/project", 7, mrURL)
	if err != nil {
		t.Fatalf("findRunByGitLabMR err = %v, want nil", err)
	}
	if got != target {
		t.Fatalf("resolved run = %+v, want the exact-URL-matched aged-out target %+v", got, target)
	}
	found := false
	for _, u := range rr.listURLs {
		if u == mrURL {
			found = true
		}
	}
	if !found {
		t.Fatalf("listURLs = %v, want the exact-URL filter to have run with mrURL %q", rr.listURLs, mrURL)
	}
}

// TestFindRunByGitLabMR_AgedOutURLVariant_DeferredToProjectPersistedLookup
// pins the residual gap deliberately deferred to #1861: an aged-out MR
// whose stored URL differs from the webhook URL only by the /-/ infix is
// NOT byte-equal, so the exact-URL DB filter's Go-side equality re-check
// (`*r.PullRequestURL == mrURL`, webhook_gitlab.go) rejects it — only the
// windowed iid/normalized scan tolerates the infix, and that run has aged
// out of the window. Until #1861 wires GitLab run creation with a durable
// project+IID persisted lookup column (or a non-windowed normalized-URL DB
// filter), this case returns (nil, nil) and emits the existing no-match
// warn.
//
// The exactURLResult fake return is populated with the variant-URL target
// specifically so this test exercises (and would catch a regression in)
// the exact-match equality check itself: if that check were loosened to
// compare via normalizeGitLabMRURL instead of raw ==, this same fixture
// would resolve `target` and the assertion below would fail. Without
// populating exactURLResult, the test would pass unchanged whether or not
// URL normalization/variant-handling existed at all, since the run would
// simply be absent from every candidate list — that vacuity is what this
// fixture is built to avoid.
func TestFindRunByGitLabMR_AgedOutURLVariant_DeferredToProjectPersistedLookup(t *testing.T) {
	// The webhook uses the current /-/ form.
	mrURL := "https://gitlab.com/group/project/-/merge_requests/7"
	// The aged-out target's stored URL differs from the webhook's only by
	// that infix (legacy form, no /-/).
	legacyURL := "https://gitlab.com/group/project/merge_requests/7"
	target := &run.Run{ID: uuid.New(), Repo: "group/project", PullRequestURL: &legacyURL}
	// The windowed project-scoped scan models the target having aged past
	// the 50-recent-runs window: it is absent from listResult, so neither
	// the iid nor the normalized-URL pass (both windowed) can see it.
	urlOther := "https://gitlab.com/group/project/-/merge_requests/999"
	rr := &prEventsRunRepo{
		listResult: []*run.Run{{ID: uuid.New(), Repo: "group/project", PullRequestURL: &urlOther}},
		// The exact-URL DB filter is not recency-windowed and DOES surface
		// this row — modeling that the target still exists in the runs
		// table — but its stored URL is the legacy variant, not byte-
		// identical to mrURL, so findRunByGitLabMR's exact-equality
		// re-check discards it.
		exactURLResult: []*run.Run{target},
	}
	lh := &recordingLogHandler{}
	s := New(Config{
		Addr:      "127.0.0.1:0",
		RunRepo:   rr,
		AuditRepo: &prEventsAuditRepo{},
		Logger:    slog.New(lh),
	})
	got, err := s.findRunByGitLabMR(context.Background(), "group/project", 7, mrURL)
	if err != nil {
		t.Fatalf("findRunByGitLabMR err = %v, want nil", err)
	}
	if got != nil {
		t.Fatalf("resolved run = %+v, want nil — the /-/-infix variant must defeat the exact-URL byte match (#1861)", got)
	}
	if lh.find("gitlab merge_request: no run matched; review stage will not transition", nil) == nil {
		t.Fatalf("expected the no-match warn on the deferred aged-out variant; got none")
	}
}

// TestFindRunByGitLabMR_LookupError_ReturnsError pins the transient-
// failure branch (E45.21): a RunRepo ListRuns error must propagate as
// (nil, err) — NOT be swallowed to (nil, nil) — so the caller can 5xx and
// let GitLab redeliver rather than permanently dropping the transition.
func TestFindRunByGitLabMR_LookupError_ReturnsError(t *testing.T) {
	listErr := errors.New("transient db failure")
	rr := &prEventsRunRepo{listErr: listErr}
	s := prEventsTestServer(t, rr, &prEventsAuditRepo{})
	got, err := s.findRunByGitLabMR(context.Background(), "group/project", 7,
		"https://gitlab.com/group/project/-/merge_requests/7")
	if got != nil {
		t.Fatalf("resolved run = %+v, want nil on a lookup error", got)
	}
	if !errors.Is(err, listErr) {
		t.Fatalf("err = %v, want the ListRuns error propagated", err)
	}
}

// TestHandleGitLabMergeRequest_LookupError_PropagatesError confirms the
// consumer surfaces the transient lookup error to its caller and performs
// NO transition and NO audit side effect on that path (E45.21).
func TestHandleGitLabMergeRequest_LookupError_PropagatesError(t *testing.T) {
	listErr := errors.New("transient db failure")
	rr := &prEventsRunRepo{listErr: listErr}
	ar := &prEventsAuditRepo{}
	s := prEventsTestServer(t, rr, ar)
	err := s.handleGitLabMergeRequest(context.Background(),
		gitLabMRBody("merge", "https://gitlab.com/group/project/-/merge_requests/7", 7))
	if !errors.Is(err, listErr) {
		t.Fatalf("err = %v, want the lookup error propagated (→ 5xx)", err)
	}
	if len(rr.transitions) != 0 {
		t.Errorf("transitions = %d, want 0 (no transition on a lookup failure)", len(rr.transitions))
	}
	if len(ar.appended) != 0 {
		t.Errorf("audits = %d, want 0 (no audit side effects on a lookup failure)", len(ar.appended))
	}
}

// TestHandleWebhookGitLab_MergeRequestLookupError_500_Unmarks is the
// cross-layer end-to-end assertion (E45.21): a transient MR run-lookup
// failure at the HTTP receiver yields a 500 AND unmarks the already-recorded
// delivery so GitLab's retry re-records and re-drives the transition rather
// than being deduped to a 202. The fresh-Mark-succeeds technique mirrors
// TestDispatchGitLabDelivery_DispatchError_Unmarks.
func TestHandleWebhookGitLab_MergeRequestLookupError_500_Unmarks(t *testing.T) {
	rr := &prEventsRunRepo{listErr: errors.New("transient db failure")}
	s, store := newGitLabWebhookServer(t, gitlabServerOpts{
		runRepo:   rr,
		auditRepo: &prEventsAuditRepo{},
	})
	body := gitLabMRBody("merge", "https://gitlab.com/group/project/-/merge_requests/7", 7)
	w := postGitLab(t, s, gitlabHeaders("Merge Request Hook", "mr-lookup-err"), body)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 on MR lookup failure:\n%s", w.Code, w.Body.String())
	}
	// The delivery must be released: a fresh Mark on the same id succeeds,
	// proving the retry re-processes instead of being deduped to a 202.
	if err := store.Mark("gitlab:mr-lookup-err"); err != nil {
		t.Errorf("post-failure Mark = %v, want nil (delivery must be unmarked so the retry re-drives the transition)", err)
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

// --- #1861: GitLab pipeline/job CI-failure retry ---

// ciRetryWorkflowSpec is a 2-stage feature_change (plan + implement) with an
// on_ci_failure block so the dispatcher's resolveRetryPolicy picks a cap and
// FilterOutPlanStages leaves a non-plan stage to dispatch.
const ciRetryWorkflowSpec = `version: "0.3"
roles:
  tech_lead:
    members: ["@kuhlman-labs"]
workflows:
  feature_change:
    description: Test workflow with retries
    on_ci_failure:
      max_retries: 1
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
        gates:
          - type: approval
            approvers:
              any_of: [tech_lead]
            sla: 4_business_hours
      - id: implement
        type: implement
        executor:
          agent: claude-code
`

// ciRetryDispatchRuns is the dispatcher's Runs for the GitLab CI-retry tests:
// it records the follow-up run + stages HandleGitLabCIFailure creates.
type ciRetryDispatchRuns struct {
	run.Repository
	mu          sync.Mutex
	createdRuns []*run.Run
	stages      []*run.Stage
}

func (r *ciRetryDispatchRuns) CreateRun(_ context.Context, p run.CreateRunParams) (*run.Run, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rn := &run.Run{
		ID: uuid.New(), Repo: p.Repo, WorkflowID: p.WorkflowID, WorkflowSHA: p.WorkflowSHA,
		TriggerSource: p.TriggerSource, TriggerRef: p.TriggerRef, ParentRunID: p.ParentRunID,
		WorkflowSpec: p.WorkflowSpec, RetryAttempt: p.RetryAttempt, RunnerKind: p.RunnerKind,
		State: run.StatePending,
	}
	r.createdRuns = append(r.createdRuns, rn)
	return rn, nil
}

func (r *ciRetryDispatchRuns) CreateStage(_ context.Context, p run.CreateStageParams) (*run.Stage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := &run.Stage{ID: uuid.New(), RunID: p.RunID, Sequence: p.Sequence, Type: p.Type,
		ExecutorKind: p.ExecutorKind, ExecutorRef: p.ExecutorRef, State: run.StageStatePending}
	r.stages = append(r.stages, st)
	return st, nil
}

func (r *ciRetryDispatchRuns) TransitionStage(_ context.Context, id uuid.UUID, to run.StageState, _ *run.StageCompletion) (*run.Stage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, st := range r.stages {
		if st.ID == id {
			st.State = to
			return st, nil
		}
	}
	return nil, run.ErrNotFound
}

// ciRetryServer wires a server whose RunRepo returns parentRun and whose
// WebhookDispatcher records the retry run it creates.
func ciRetryServer(t *testing.T, parentRun *run.Run, listErr error) (*Server, *ciRetryDispatchRuns) {
	t.Helper()
	dispRuns := &ciRetryDispatchRuns{}
	disp := &webhook.Dispatcher{Runs: dispRuns, Logger: slog.Default()}
	rr := &prEventsRunRepo{listErr: listErr}
	if parentRun != nil {
		rr.listResult = []*run.Run{parentRun}
	}
	s, _ := newGitLabWebhookServer(t, gitlabServerOpts{runRepo: rr, dispatcher: disp})
	return s, dispRuns
}

func gitLabCIParentRun() *run.Run {
	ref := "issue:7"
	return &run.Run{
		ID: uuid.New(), Repo: "group/proj", WorkflowID: "feature_change",
		WorkflowSHA: "abc", TriggerRef: &ref, WorkflowSpec: []byte(ciRetryWorkflowSpec),
		RetryAttempt: 0, RunnerKind: run.RunnerKindGitLabCI, State: run.StateRunning,
	}
}

func gitLabPipelineBody(status string) []byte {
	body, _ := json.Marshal(map[string]any{
		"object_kind":       "pipeline",
		"object_attributes": map[string]any{"id": 1, "status": status, "ref": "main", "sha": "deadbeef"},
		"project":           map[string]any{"id": 555, "path_with_namespace": "group/proj"},
	})
	return body
}

func gitLabJobBody(status string) []byte {
	body, _ := json.Marshal(map[string]any{
		"object_kind":  "build",
		"ref":          "main",
		"sha":          "deadbeef",
		"build_status": status,
		"project_id":   555,
		"project":      map[string]any{"id": 555, "path_with_namespace": "group/proj"},
	})
	return body
}

func TestHandleGitLabPipeline_FailedDrivesRetry(t *testing.T) {
	s, dispRuns := ciRetryServer(t, gitLabCIParentRun(), nil)
	if err := s.handleGitLabPipeline(context.Background(), gitLabPipelineBody("failed")); err != nil {
		t.Fatalf("handleGitLabPipeline err = %v, want nil", err)
	}
	if len(dispRuns.createdRuns) != 1 {
		t.Fatalf("retry runs created = %d, want 1", len(dispRuns.createdRuns))
	}
	child := dispRuns.createdRuns[0]
	if child.RunnerKind != run.RunnerKindGitLabCI {
		t.Errorf("retry runner_kind = %q, want gitlab_ci", child.RunnerKind)
	}
	if child.RetryAttempt != 1 {
		t.Errorf("retry attempt = %d, want 1", child.RetryAttempt)
	}
}

func TestHandleGitLabJob_FailedDrivesRetry(t *testing.T) {
	s, dispRuns := ciRetryServer(t, gitLabCIParentRun(), nil)
	if err := s.handleGitLabJob(context.Background(), gitLabJobBody("failed")); err != nil {
		t.Fatalf("handleGitLabJob err = %v, want nil", err)
	}
	if len(dispRuns.createdRuns) != 1 {
		t.Fatalf("job retry runs created = %d, want 1", len(dispRuns.createdRuns))
	}
}

func TestHandleGitLabPipeline_NonFailedStatusNoRetry(t *testing.T) {
	s, dispRuns := ciRetryServer(t, gitLabCIParentRun(), nil)
	if err := s.handleGitLabPipeline(context.Background(), gitLabPipelineBody("success")); err != nil {
		t.Fatalf("handleGitLabPipeline err = %v, want nil", err)
	}
	if len(dispRuns.createdRuns) != 0 {
		t.Errorf("non-failed pipeline must drive no retry, got %d", len(dispRuns.createdRuns))
	}
}

func TestHandleGitLabJob_NonFailedStatusNoRetry(t *testing.T) {
	s, dispRuns := ciRetryServer(t, gitLabCIParentRun(), nil)
	if err := s.handleGitLabJob(context.Background(), gitLabJobBody("running")); err != nil {
		t.Fatalf("handleGitLabJob err = %v, want nil", err)
	}
	if len(dispRuns.createdRuns) != 0 {
		t.Errorf("non-failed job must drive no retry, got %d", len(dispRuns.createdRuns))
	}
}

func TestHandleGitLabPipeline_TransientRunLookupPropagates(t *testing.T) {
	s, _ := ciRetryServer(t, nil, errors.New("db blip"))
	err := s.handleGitLabPipeline(context.Background(), gitLabPipelineBody("failed"))
	if err == nil {
		t.Fatal("transient RunRepo lookup failure must propagate (5xx), got nil")
	}
}

func TestHandleGitLabPipeline_NoMatchIsBestEffort(t *testing.T) {
	s, dispRuns := ciRetryServer(t, nil, nil) // empty listResult
	if err := s.handleGitLabPipeline(context.Background(), gitLabPipelineBody("failed")); err != nil {
		t.Fatalf("no-match must be best-effort nil, got %v", err)
	}
	if len(dispRuns.createdRuns) != 0 {
		t.Errorf("no matching run must drive no retry, got %d", len(dispRuns.createdRuns))
	}
}
