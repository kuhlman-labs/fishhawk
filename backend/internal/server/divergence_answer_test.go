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
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pglock"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/timescale"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// divergence_answer_test.go pins POST /v0/runs/{run_id}/divergence/{sequence}/answer
// (E75.5 / #3733): one_off records and files nothing; doctrine_change files
// exactly one autonomy:low item and writes no repository file; and every
// refusal is asserted on ERROR IDENTITY *and* on the committed chain afterwards.

// daForgeRecorder is a GitHub API fake that serves the epic-title read the
// filing pipeline makes and RECORDS every request, so a test can assert no
// repository content was ever written.
type daForgeRecorder struct {
	mu   sync.Mutex
	reqs []string
}

func (f *daForgeRecorder) writes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, r := range f.reqs {
		if !strings.HasPrefix(r, "GET ") || strings.Contains(r, "/contents") || strings.Contains(r, "/git/") {
			out = append(out, r)
		}
	}
	return out
}

func newDAForge(t *testing.T) (*githubclient.Client, *daForgeRecorder) {
	t.Helper()
	rec := &daForgeRecorder{}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		rec.mu.Lock()
		rec.reqs = append(rec.reqs, r.Method+" "+r.URL.Path)
		rec.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/installation"):
			_, _ = w.Write([]byte(`{"id":7788}`))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/issues/"):
			_, _ = w.Write([]byte(`{"number":389,"title":"[E75] Precedent","state":"open"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"Not Found"}`))
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &githubclient.Client{
		BaseURL: srv.URL,
		Tokens:  &fakeTokenProvider{tok: "ghs_t"},
		HTTP:    &http.Client{Timeout: 5 * time.Second},
		AppJWT:  func() (string, error) { return "ghs_jwt", nil },
	}, rec
}

type daHarness struct {
	s     *Server
	repo  *approvalRunRepo
	audit *auditFake
	fp    *fakeWorkProvider
	forge *daForgeRecorder
	runID uuid.UUID
	stage uuid.UUID
}

func newDAHarness(t *testing.T) *daHarness {
	t.Helper()
	fp := &fakeWorkProvider{}
	registerFakeProvider(t, fp)
	gh, forge := newDAForge(t)
	repo := newApprovalRunRepo()
	au := newAuditFake()
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: repo, AuditRepo: au, GitHub: gh, DivergenceConfig: dqEnabledConfig()})
	h := &daHarness{s: s, repo: repo, audit: au, fp: fp, forge: forge, runID: uuid.New(), stage: uuid.New()}
	seedDeferRun(repo, h.runID)
	dqSeedDivergence(au, h.runID, h.stage, 10, "plan_approval", "reject")
	return h
}

func (h *daHarness) post(t *testing.T, runID uuid.UUID, seq string, body any, auth func(*http.Request) *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	var raw []byte
	switch b := body.(type) {
	case string:
		raw = []byte(b)
	default:
		raw, _ = json.Marshal(b)
	}
	req := httptest.NewRequest(http.MethodPost, "/v0/runs/"+runID.String()+"/divergence/"+seq+"/answer", bytes.NewReader(raw))
	req.SetPathValue("run_id", runID.String())
	req.SetPathValue("sequence", seq)
	w := httptest.NewRecorder()
	h.s.handleAnswerDivergence(w, auth(req))
	return w
}

// answers returns the precedent_divergence_answered entries appended for runID.
func (h *daHarness) answers(runID uuid.UUID) []precedentDivergenceAnsweredPayload {
	var out []precedentDivergenceAnsweredPayload
	for _, ap := range h.audit.appended {
		if ap.Category != CategoryPrecedentDivergenceAnswered || ap.RunID != runID {
			continue
		}
		var p precedentDivergenceAnsweredPayload
		_ = json.Unmarshal(ap.Payload, &p)
		out = append(out, p)
	}
	return out
}

func daErrCode(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
		Code string `json:"code"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body.Error.Code != "" {
		return body.Error.Code
	}
	return body.Code
}

// TestAnswer_OneOffRecordsAndFilesNothing: one_off appends exactly one answer
// entry citing the sequence, files nothing, writes nothing to the forge, and
// the question stops surfacing.
func TestAnswer_OneOffRecordsAndFilesNothing(t *testing.T) {
	h := newDAHarness(t)
	w := h.post(t, h.runID, "10", divergenceAnswerRequest{Answer: "one_off", Note: "the generated file was wrong this once"}, withAuth)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
	}
	var resp divergenceAnswerResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Answer != "one_off" || resp.Sequence != 10 || resp.Issue != nil {
		t.Fatalf("response = %+v", resp)
	}
	got := h.answers(h.runID)
	if len(got) != 1 || got[0].DivergenceSequence != 10 || got[0].Answer != "one_off" ||
		got[0].Note != "the generated file was wrong this once" || got[0].DecisionClass != "plan_approval" ||
		got[0].StageID != h.stage.String() || got[0].IssueNumber != 0 {
		t.Fatalf("answer entries = %+v", got)
	}
	ap := h.audit.appended[len(h.audit.appended)-1]
	if ap.StageID == nil || *ap.StageID != h.stage || ap.ActorSubject == nil || *ap.ActorSubject != "github:test-operator" {
		t.Errorf("answer entry stage/actor = %v / %v", ap.StageID, ap.ActorSubject)
	}
	if h.fp.called {
		t.Error("one_off filed a work item")
	}
	if wr := h.forge.writes(); len(wr) != 0 {
		t.Errorf("forge writes = %v, want none", wr)
	}
	if q := h.s.openDivergenceFor(dqOperatorCtx(), &run.Run{ID: h.runID}); q != nil {
		t.Errorf("question still open after one_off: %+v", q)
	}
}

// TestAnswer_DoctrineChangeFilesOneAutonomyLowItem: doctrine_change files
// EXACTLY one item — autonomy:low forced over a caller-supplied autonomy
// label, the run as evidence, the divergence and its precedent cited by
// sequence + entry hash — records the answer naming the issue, and writes no
// repository file.
func TestAnswer_DoctrineChangeFilesOneAutonomyLowItem(t *testing.T) {
	h := newDAHarness(t)
	w := h.post(t, h.runID, "10", divergenceAnswerRequest{
		Answer: "doctrine_change", Note: "generated files are now reject-on-sight",
		ParentEpic: "#389", N: "9", Labels: []string{"area:server", "autonomy:high"},
	}, withAuth)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
	}
	if !h.fp.called {
		t.Fatal("doctrine_change filed nothing")
	}
	labels := h.fp.captured.Item.Classification.Labels
	if !containsStr2(labels, "autonomy:low") || containsStr2(labels, "autonomy:high") || !containsStr2(labels, "area:server") {
		t.Errorf("labels = %v, want autonomy:low (never autonomy:high) and area:server", labels)
	}
	if ev := h.fp.captured.Item.Relations.EvidenceRuns; len(ev) != 1 || ev[0] != h.runID.String() {
		t.Errorf("evidence runs = %v", ev)
	}
	body := h.fp.captured.Item.Body
	for _, want := range []string{"doctrine_change", "sequence 10", "dv-hash-10", "101 `h101`", "102 `h102`", "2 more",
		"generated files are now reject-on-sight", h.runID.String(), "No repository file was written"} {
		if !strings.Contains(body, want) && !strings.Contains(strings.ToLower(body), strings.ToLower(want)) {
			t.Errorf("filed body missing %q:\n%s", want, body)
		}
	}
	var resp divergenceAnswerResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Issue == nil || resp.Issue.Number != 4242 {
		t.Fatalf("response issue = %+v", resp.Issue)
	}
	got := h.answers(h.runID)
	if len(got) != 1 || got[0].Answer != "doctrine_change" || got[0].IssueNumber != 4242 ||
		got[0].IssueURL != "https://github.com/kuhlman-labs/fishhawk/issues/4242" {
		t.Fatalf("answer entries = %+v", got)
	}
	if wr := h.forge.writes(); len(wr) != 0 {
		t.Errorf("forge writes = %v, want none: a doctrine change writes no repository file", wr)
	}
}

// TestAnswer_RefusesUnknownSequence: sequence 11 is a real chain entry of a
// DIFFERENT category on this run — not a divergence — so the verb refuses
// divergence_not_found and the chain carries no answer.
func TestAnswer_RefusesUnknownSequence(t *testing.T) {
	h := newDAHarness(t)
	rid := h.runID
	h.audit.seeded = append(h.audit.seeded, &audit.Entry{Sequence: 11, RunID: &rid, Category: "approval_submitted", Payload: json.RawMessage(`{}`)})
	w := h.post(t, h.runID, "11", divergenceAnswerRequest{Answer: "one_off"}, withAuth)
	if w.Code != http.StatusNotFound || daErrCode(t, w) != "divergence_not_found" {
		t.Fatalf("status = %d code = %q, want 404 divergence_not_found:\n%s", w.Code, daErrCode(t, w), w.Body.String())
	}
	if got := h.answers(h.runID); len(got) != 0 {
		t.Fatalf("answer entries = %+v, want none", got)
	}
}

// TestAnswer_RefusesSecondAnswer: a sequence that already carries an answer is
// refused divergence_already_answered, and the chain still carries ONE answer.
func TestAnswer_RefusesSecondAnswer(t *testing.T) {
	h := newDAHarness(t)
	if w := h.post(t, h.runID, "10", divergenceAnswerRequest{Answer: "one_off"}, withAuth); w.Code != http.StatusOK {
		t.Fatalf("first answer = %d", w.Code)
	}
	w := h.post(t, h.runID, "10", divergenceAnswerRequest{Answer: "doctrine_change", ParentEpic: "#389", N: "9"}, withAuth)
	if w.Code != http.StatusConflict || daErrCode(t, w) != "divergence_already_answered" {
		t.Fatalf("status = %d code = %q, want 409 divergence_already_answered", w.Code, daErrCode(t, w))
	}
	if got := h.answers(h.runID); len(got) != 1 {
		t.Fatalf("answer entries = %d, want exactly 1", len(got))
	}
	if h.fp.called {
		t.Error("the refused second answer filed a work item")
	}
}

// TestAnswer_RefusesCrossRunToken: TWO real runs, each with an open
// divergence. Run A's run-bound token answering run B is refused, and B's
// chain carries no answer. Run A's token answering its OWN run succeeds — the
// discriminator proving the refusal is the subject guard, not a blanket
// run-bound refusal.
func TestAnswer_RefusesCrossRunToken(t *testing.T) {
	h := newDAHarness(t)
	runB := uuid.New()
	seedDeferRun(h.repo, runB)
	dqSeedDivergence(h.audit, runB, uuid.New(), 20, "concern_waive", "waived")
	asA := func(r *http.Request) *http.Request {
		return withIdentity(r, Identity{Subject: "mcp:run:" + h.runID.String(), AccountID: testOperatorAccountID})
	}
	w := h.post(t, runB, "20", divergenceAnswerRequest{Answer: "one_off"}, asA)
	if w.Code != http.StatusForbidden || daErrCode(t, w) != "cross_run_divergence_answer" {
		t.Fatalf("status = %d code = %q, want 403 cross_run_divergence_answer", w.Code, daErrCode(t, w))
	}
	if got := h.answers(runB); len(got) != 0 {
		t.Fatalf("run B answer entries = %+v, want none", got)
	}
	if w := h.post(t, h.runID, "10", divergenceAnswerRequest{Answer: "one_off"}, asA); w.Code != http.StatusOK {
		t.Fatalf("own-run answer = %d, want 200:\n%s", w.Code, w.Body.String())
	}
}

// TestAnswer_RefusesUnknownAnswerValue: an answer outside the closed set is a
// validation_failed refusal and records nothing.
func TestAnswer_RefusesUnknownAnswerValue(t *testing.T) {
	h := newDAHarness(t)
	w := h.post(t, h.runID, "10", divergenceAnswerRequest{Answer: "maybe"}, withAuth)
	if w.Code != http.StatusBadRequest || daErrCode(t, w) != "validation_failed" {
		t.Fatalf("status = %d code = %q, want 400 validation_failed", w.Code, daErrCode(t, w))
	}
	if got := h.answers(h.runID); len(got) != 0 {
		t.Fatalf("answer entries = %+v, want none", got)
	}
}

// TestAnswer_FilingFailureRecordsNothing: FILE FIRST, THEN RECORD — a provider
// failure appends no answer and leaves the question open for a retry.
func TestAnswer_FilingFailureRecordsNothing(t *testing.T) {
	h := newDAHarness(t)
	h.fp.fileErr = errors.New("provider down")
	w := h.post(t, h.runID, "10", divergenceAnswerRequest{Answer: "doctrine_change", ParentEpic: "#389", N: "9"}, withAuth)
	if w.Code != http.StatusBadGateway || daErrCode(t, w) != "work_item_filing_failed" {
		t.Fatalf("status = %d code = %q, want 502 work_item_filing_failed:\n%s", w.Code, daErrCode(t, w), w.Body.String())
	}
	if got := h.answers(h.runID); len(got) != 0 {
		t.Fatalf("answer entries = %+v, want none", got)
	}
	if q := h.s.openDivergenceFor(dqOperatorCtx(), &run.Run{ID: h.runID}); q == nil || q.Sequence != 10 {
		t.Fatalf("question = %+v, want seq 10 still open", q)
	}
}

// TestAnswer_DefensiveBranches: one row per remaining refusal / degrade, each
// asserting the status, the code, and that nothing was recorded.
func TestAnswer_DefensiveBranches(t *testing.T) {
	scoped := func(scopes ...string) func(*http.Request) *http.Request {
		return func(r *http.Request) *http.Request {
			return withIdentity(r, Identity{Subject: "operator:x", TokenID: "tok", Scopes: scopes})
		}
	}
	anon := func(r *http.Request) *http.Request { return withIdentity(r, Identity{}) }
	cases := []struct {
		name     string
		mutate   func(h *daHarness) (uuid.UUID, string, any)
		auth     func(*http.Request) *http.Request
		status   int
		code     string
		wantFile bool
	}{
		{"anonymous", nil, anon, http.StatusUnauthorized, "authentication_required", false},
		{"missing_scope", nil, scoped("read:audit"), http.StatusForbidden, "insufficient_scope", false},
		{"fixups_scope_suffices", nil, scoped("write:fixups"), http.StatusOK, "", false},
		{"unconfigured", func(h *daHarness) (uuid.UUID, string, any) {
			h.s.cfg.RunRepo = nil
			return h.runID, "10", divergenceAnswerRequest{Answer: "one_off"}
		}, withAuth, http.StatusServiceUnavailable, "divergence_store_unconfigured", false},
		{"bad_run_id", func(h *daHarness) (uuid.UUID, string, any) {
			return uuid.Nil, "10", divergenceAnswerRequest{Answer: "one_off"}
		}, withAuth, http.StatusNotFound, "run_not_found", false},
		{"zero_sequence", func(h *daHarness) (uuid.UUID, string, any) {
			return h.runID, "0", divergenceAnswerRequest{Answer: "one_off"}
		}, withAuth, http.StatusBadRequest, "validation_failed", false},
		{"non_numeric_sequence", func(h *daHarness) (uuid.UUID, string, any) {
			return h.runID, "ten", divergenceAnswerRequest{Answer: "one_off"}
		}, withAuth, http.StatusBadRequest, "validation_failed", false},
		{"malformed_body", func(h *daHarness) (uuid.UUID, string, any) {
			return h.runID, "10", "{not json"
		}, withAuth, http.StatusBadRequest, "validation_failed", false},
		{"malformed_run_bound_subject", nil, func(r *http.Request) *http.Request {
			return withIdentity(r, Identity{Subject: "mcp:run:not-a-uuid"})
		}, http.StatusUnauthorized, "authentication_required", false},
		{"divergence_read_fails", func(h *daHarness) (uuid.UUID, string, any) {
			h.audit.listByCategoryErrCategory = CategoryPrecedentDivergence
			return h.runID, "10", divergenceAnswerRequest{Answer: "one_off"}
		}, withAuth, http.StatusInternalServerError, "internal_error", false},
		{"answered_read_fails", func(h *daHarness) (uuid.UUID, string, any) {
			h.audit.listByCategoryErrCategory = CategoryPrecedentDivergenceAnswered
			return h.runID, "10", divergenceAnswerRequest{Answer: "one_off"}
		}, withAuth, http.StatusInternalServerError, "internal_error", false},
		{"malformed_repo_coordinate", func(h *daHarness) (uuid.UUID, string, any) {
			id := uuid.New()
			h.repo.seedRun(&run.Run{ID: id, Repo: "no-slash", State: run.StateRunning})
			dqSeedDivergence(h.audit, id, uuid.New(), 30, "plan_approval", "reject")
			return id, "30", divergenceAnswerRequest{Answer: "doctrine_change", ParentEpic: "#389", N: "9"}
		}, withAuth, http.StatusInternalServerError, "internal_error", false},
		{"one_off_append_fails", func(h *daHarness) (uuid.UUID, string, any) {
			h.audit.appendErrCategory = CategoryPrecedentDivergenceAnswered
			return h.runID, "10", divergenceAnswerRequest{Answer: "one_off"}
		}, withAuth, http.StatusInternalServerError, "internal_error", false},
		{"doctrine_append_fails_names_filed_issue", func(h *daHarness) (uuid.UUID, string, any) {
			h.audit.appendErrCategory = CategoryPrecedentDivergenceAnswered
			return h.runID, "10", divergenceAnswerRequest{Answer: "doctrine_change", ParentEpic: "#389", N: "9"}
		}, withAuth, http.StatusInternalServerError, "internal_error", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newDAHarness(t)
			runID, seq, body := h.runID, "10", any(divergenceAnswerRequest{Answer: "one_off"})
			if tc.mutate != nil {
				runID, seq, body = tc.mutate(h)
			}
			w := h.post(t, runID, seq, body, tc.auth)
			if w.Code != tc.status || (tc.code != "" && daErrCode(t, w) != tc.code) {
				t.Fatalf("status = %d code = %q, want %d %q:\n%s", w.Code, daErrCode(t, w), tc.status, tc.code, w.Body.String())
			}
			if tc.status != http.StatusOK && len(h.answers(runID)) != 0 {
				t.Fatalf("a refused answer was recorded")
			}
			if h.fp.called != tc.wantFile {
				t.Errorf("filed = %v, want %v", h.fp.called, tc.wantFile)
			}
			if tc.wantFile && !strings.Contains(w.Body.String(), "issues/4242") {
				t.Errorf("append failure after filing must name the filed issue: %s", w.Body.String())
			}
		})
	}
}

// TestAnswer_DoctrineChangePreFilingFailures pins the two fileDoctrineChange
// branches that refuse before the provider is reached (#3891 remapped them
// onto returned errors): a conventions-load failure is 500 internal_error and
// an installation-resolve failure is 502 work_item_filing_failed. Each files
// nothing and records no answer.
func TestAnswer_DoctrineChangePreFilingFailures(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(t *testing.T, h *daHarness) (uuid.UUID, string)
		status int
		code   string
		msg    string
	}{
		{"conventions_load_fails", func(t *testing.T, h *daHarness) (uuid.UUID, string) {
			prev := conventionsLoader
			conventionsLoader = func(context.Context, string) (workmgmt.Conventions, error) {
				return workmgmt.Conventions{}, errors.New("work-management.yaml: parse error")
			}
			t.Cleanup(func() { conventionsLoader = prev })
			return h.runID, "10"
		}, http.StatusInternalServerError, "internal_error", "could not load work-management conventions"},
		{"installation_resolve_fails", func(t *testing.T, h *daHarness) (uuid.UUID, string) {
			// A run with no recorded installation makes the filing resolve one
			// through the GitHub App, and the forge answers 500.
			id := uuid.New()
			h.repo.seedRun(&run.Run{ID: id, Repo: "kuhlman-labs/fishhawk", State: run.StateRunning})
			dqSeedDivergence(h.audit, id, uuid.New(), 40, "plan_approval", "reject")
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"message":"boom"}`))
			}))
			t.Cleanup(srv.Close)
			h.s.cfg.GitHub = &githubclient.Client{
				BaseURL: srv.URL,
				Tokens:  &fakeTokenProvider{tok: "ghs_t"},
				HTTP:    &http.Client{Timeout: 5 * time.Second},
				AppJWT:  func() (string, error) { return "ghs_jwt", nil },
			}
			return id, "40"
		}, http.StatusBadGateway, "work_item_filing_failed", "could not resolve the GitHub App installation"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newDAHarness(t)
			runID, seq := tc.mutate(t, h)
			w := h.post(t, runID, seq, divergenceAnswerRequest{Answer: "doctrine_change", ParentEpic: "#389", N: "9"}, withAuth)
			if w.Code != tc.status || daErrCode(t, w) != tc.code || !strings.Contains(w.Body.String(), tc.msg) {
				t.Fatalf("status = %d code = %q, want %d %q naming %q:\n%s", w.Code, daErrCode(t, w), tc.status, tc.code, tc.msg, w.Body.String())
			}
			if h.fp.called {
				t.Error("a refusal before the provider filed a work item")
			}
			if got := h.answers(runID); len(got) != 0 {
				t.Fatalf("answer entries = %+v, want none", got)
			}
		})
	}
}

// ---- #3891: the answer section runs under the per-question answer lock ----

// daLockingRunRepo is approvalRunRepo plus a test-local
// run.DivergenceAnswerLocker: one sync.Mutex per (run, sequence). On
// contention it signals contended BEFORE blocking, so a test observes a
// waiter on the lock without a timer. failErr returns without running fn (an
// acquisition failure); lapseErr runs fn and then returns it (a lapse
// reported after the section completed).
type daLockingRunRepo struct {
	*approvalRunRepo
	mu        sync.Mutex
	locks     map[string]*sync.Mutex
	contended chan string
	failErr   error
	lapseErr  error
	calls     atomic.Int32
}

var _ run.DivergenceAnswerLocker = (*daLockingRunRepo)(nil)

func newDALockingRunRepo(inner *approvalRunRepo) *daLockingRunRepo {
	return &daLockingRunRepo{approvalRunRepo: inner, locks: map[string]*sync.Mutex{}, contended: make(chan string, 4)}
}

func (r *daLockingRunRepo) WithDivergenceAnswerLock(ctx context.Context, runID uuid.UUID, seq int64, fn func(context.Context) error) error {
	r.calls.Add(1)
	if r.failErr != nil {
		return r.failErr
	}
	key := fmt.Sprintf("%s/%d", runID, seq)
	r.mu.Lock()
	l, ok := r.locks[key]
	if !ok {
		l = &sync.Mutex{}
		r.locks[key] = l
	}
	r.mu.Unlock()
	if !l.TryLock() {
		select {
		case r.contended <- key:
		default:
		}
		l.Lock()
	}
	defer l.Unlock()
	if err := fn(ctx); err != nil {
		return err
	}
	return r.lapseErr
}

// daBlockingProvider is a concurrency-safe workmgmt.Provider registered under
// the default provider id. It counts File calls, signals every entry (its
// 1-based ordinal) on entered, and parks ONLY the first call until
// releaseFirst, so a test can pin the first answer inside its section.
type daBlockingProvider struct {
	mu      sync.Mutex
	files   int
	entered chan int
	release chan struct{}
	once    sync.Once
}

func newDABlockingProvider() *daBlockingProvider {
	p := &daBlockingProvider{entered: make(chan int, 4), release: make(chan struct{})}
	workmgmt.Register(p)
	return p
}

func (p *daBlockingProvider) Name() string { return workmgmt.Default().Provider }

func (p *daBlockingProvider) releaseFirst() { p.once.Do(func() { close(p.release) }) }

func (p *daBlockingProvider) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.files
}

func (p *daBlockingProvider) File(ctx context.Context, req workmgmt.ProviderRequest) (*workmgmt.CreatedItem, error) {
	p.mu.Lock()
	p.files++
	n := p.files
	p.mu.Unlock()
	select {
	case p.entered <- n:
	default:
	}
	if n == 1 {
		select {
		case <-p.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	num := 4241 + n
	return &workmgmt.CreatedItem{
		Provider:      p.Name(),
		Number:        num,
		URL:           fmt.Sprintf("https://github.com/kuhlman-labs/fishhawk/issues/%d", num),
		AppliedLabels: req.Item.Classification.Labels,
		Boarded:       true,
	}, nil
}

// daAwaitResponses collects n responses, failing the test when one does not
// arrive within the bound, and returns their status codes sorted plus the
// recorders.
func daAwaitResponses(t *testing.T, ch <-chan *httptest.ResponseRecorder, n int) ([]int, []*httptest.ResponseRecorder) {
	t.Helper()
	var codes []int
	var recs []*httptest.ResponseRecorder
	for i := 0; i < n; i++ {
		select {
		case w := <-ch:
			codes = append(codes, w.Code)
			recs = append(recs, w)
		case <-time.After(timescale.D(10 * time.Second)):
			t.Fatalf("answer %d of %d never returned", i+1, n)
		}
	}
	sort.Ints(codes)
	return codes, recs
}

// daAssertOneFilingOneAnswer asserts the serialized outcome: statuses {200,
// 409 divergence_already_answered} and exactly one File call.
func daAssertOneFilingOneAnswer(t *testing.T, codes []int, recs []*httptest.ResponseRecorder, files int) {
	t.Helper()
	if len(codes) != 2 || codes[0] != http.StatusOK || codes[1] != http.StatusConflict {
		t.Errorf("statuses = %v, want [200 409]: both concurrent answers passed the already-answered check", codes)
	}
	for _, w := range recs {
		if w.Code == http.StatusConflict && daErrCode(t, w) != "divergence_already_answered" {
			t.Errorf("409 code = %q, want divergence_already_answered", daErrCode(t, w))
		}
	}
	if files != 1 {
		t.Errorf("File calls = %d, want exactly 1 (a concurrent duplicate filed a second doctrine_change item)", files)
	}
}

// TestAnswer_ConcurrentDoctrineChangeFilesOnce (#3891): two concurrent
// doctrine_change answers for ONE question file exactly one work item and
// record exactly one answer; the duplicate gets 409.
//
// The overlap is FORCED: answer A is pinned inside File (holding the section)
// by the provider's first-call gate; answer B is then launched, and the test
// waits for B either to signal contention on the answer lock (the serialized
// branch) or to enter File a second time (the broken branch). No timer
// decides either branch; the bound only fails a hung test. Counterfactual:
// with the server's lock call replaced by a bare fn(ctx), B never touches the
// locker, passes the re-read because A has not appended, and files again, so
// the one-filing and {200, 409} assertions go RED.
func TestAnswer_ConcurrentDoctrineChangeFilesOnce(t *testing.T) {
	h := newDAHarness(t)
	locker := newDALockingRunRepo(h.repo)
	h.s.cfg.RunRepo = locker
	bp := newDABlockingProvider()
	t.Cleanup(bp.releaseFirst)
	body := divergenceAnswerRequest{Answer: "doctrine_change", ParentEpic: "#389", N: "9"}

	results := make(chan *httptest.ResponseRecorder, 2)
	go func() { results <- h.post(t, h.runID, "10", body, withAuth) }()
	select {
	case <-bp.entered:
	case <-time.After(timescale.D(5 * time.Second)):
		t.Fatal("the first answer never reached File")
	}

	go func() { results <- h.post(t, h.runID, "10", body, withAuth) }()
	select {
	case <-locker.contended:
	case n := <-bp.entered:
		t.Errorf("File call %d began while the first answer held the section: the duplicate was not serialized", n)
	case <-time.After(timescale.D(5 * time.Second)):
		t.Fatal("the second answer neither contended on the answer lock nor reached File")
	}
	bp.releaseFirst()

	codes, recs := daAwaitResponses(t, results, 2)
	daAssertOneFilingOneAnswer(t, codes, recs, bp.count())
	if got := h.answers(h.runID); len(got) != 1 {
		t.Errorf("answer entries = %d, want exactly 1", len(got))
	}
}

// TestAnswer_LockFailureFilesAndRecordsNothing (#3891): when the answer lock
// cannot be taken, the section never runs, so the endpoint answers 500
// internal_error and nothing is filed or recorded. The status is the vehicle:
// without the !ran branch the handler answers 200 with an empty body while the
// no-filing and no-append reads stay green.
func TestAnswer_LockFailureFilesAndRecordsNothing(t *testing.T) {
	h := newDAHarness(t)
	locker := newDALockingRunRepo(h.repo)
	locker.failErr = errors.New("divergence answer lock: acquire connection: pool closed")
	h.s.cfg.RunRepo = locker

	w := h.post(t, h.runID, "10", divergenceAnswerRequest{Answer: "doctrine_change", ParentEpic: "#389", N: "9"}, withAuth)
	if w.Code != http.StatusInternalServerError || daErrCode(t, w) != "internal_error" {
		t.Fatalf("status = %d code = %q, want 500 internal_error:\n%s", w.Code, daErrCode(t, w), w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "nothing was filed or recorded, retry the answer") {
		t.Errorf("body = %s, want the remedy (the 5xx details are redacted; the lock error is logged)", w.Body.String())
	}
	if locker.calls.Load() != 1 {
		t.Errorf("lock calls = %d, want 1", locker.calls.Load())
	}
	if h.fp.called {
		t.Error("a failed lock acquisition filed a work item")
	}
	if got := h.answers(h.runID); len(got) != 0 {
		t.Fatalf("answer entries = %+v, want none", got)
	}
	if q := h.s.openDivergenceFor(dqOperatorCtx(), &run.Run{ID: h.runID}); q == nil || q.Sequence != 10 {
		t.Fatalf("question = %+v, want seq 10 still open for a retry", q)
	}
}

// TestAnswer_LockLapseAfterAnswerStands (#3891): a lapse reported after the
// section completed leaves the durable answer standing: 200, one filing, one
// answer entry, and a WARN naming the run and sequence.
func TestAnswer_LockLapseAfterAnswerStands(t *testing.T) {
	h := newDAHarness(t)
	locker := newDALockingRunRepo(h.repo)
	locker.lapseErr = fmt.Errorf("divergence answer lock: %w", pglock.ErrLockLapsed)
	h.s.cfg.RunRepo = locker
	var logs bytes.Buffer
	h.s.cfg.Logger = slog.New(slog.NewTextHandler(&logs, nil))

	w := h.post(t, h.runID, "10", divergenceAnswerRequest{Answer: "doctrine_change", ParentEpic: "#389", N: "9"}, withAuth)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (the answer is durable):\n%s", w.Code, w.Body.String())
	}
	if !h.fp.called {
		t.Error("doctrine_change filed nothing")
	}
	if got := h.answers(h.runID); len(got) != 1 || got[0].IssueNumber != 4242 {
		t.Fatalf("answer entries = %+v, want one naming issue 4242", got)
	}
	out := logs.String()
	for _, want := range []string{"level=WARN", "may have lapsed", h.runID.String(), "sequence=10"} {
		if !strings.Contains(out, want) {
			t.Errorf("log missing %q:\n%s", want, out)
		}
	}
}

func TestDoctrineChangeLabels(t *testing.T) {
	got := doctrineChangeLabels([]string{"area:x", " autonomy:high", "autonomy:medium", "phase:y"})
	if fmt.Sprint(got) != "[area:x phase:y autonomy:low]" {
		t.Fatalf("labels = %v", got)
	}
	if got := doctrineChangeLabels(nil); fmt.Sprint(got) != "[autonomy:low]" {
		t.Fatalf("nil labels = %v", got)
	}
}
