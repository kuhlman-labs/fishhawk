package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
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

func TestDoctrineChangeLabels(t *testing.T) {
	got := doctrineChangeLabels([]string{"area:x", " autonomy:high", "autonomy:medium", "phase:y"})
	if fmt.Sprint(got) != "[area:x phase:y autonomy:low]" {
		t.Fatalf("labels = %v", got)
	}
	if got := doctrineChangeLabels(nil); fmt.Sprint(got) != "[autonomy:low]" {
		t.Fatalf("nil labels = %v", got)
	}
}
