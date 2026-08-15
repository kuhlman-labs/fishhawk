package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

func TestCreateRun_HappyPath(t *testing.T) {
	repo := newFakeRepo()
	s := newServer(t, repo)

	body := `{
		"repo": "kuhlman-labs/fishhawk",
		"workflow_id": "feature_change",
		"workflow_sha": "abc123",
		"trigger_source": "cli"
	}`
	req := httptest.NewRequest(http.MethodPost, "/v0/runs", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.handleCreateRun(w, withAuth(req))

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
	}

	var got runResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if got.Repo != "kuhlman-labs/fishhawk" {
		t.Errorf("Repo = %q", got.Repo)
	}
	if got.State != string(run.StatePending) {
		t.Errorf("State = %q, want pending", got.State)
	}
	if got.TriggerSource != "cli" {
		t.Errorf("TriggerSource = %q", got.TriggerSource)
	}
	if got.ID == uuid.Nil {
		t.Error("ID is zero")
	}
}

// TestCreateRun_WorkingDirRoundTripsOverHTTP pins the working_dir binding
// (E66.42 / #2482) across the HTTP seam: a POST carrying an absolute working_dir
// is persisted and echoed back on the 201 body — the wire-JSON → run domain →
// response path.
func TestCreateRun_WorkingDirRoundTripsOverHTTP(t *testing.T) {
	repo := newFakeRepo()
	s := newServer(t, repo)

	const wd = "/Users/dev/src/fishhawk"
	body := `{
		"repo": "kuhlman-labs/fishhawk",
		"workflow_id": "feature_change",
		"workflow_sha": "abc123",
		"trigger_source": "cli",
		"runner_kind": "local",
		"working_dir": "` + wd + `"
	}`
	req := httptest.NewRequest(http.MethodPost, "/v0/runs", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.handleCreateRun(w, withAuth(req))

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
	}
	var got runResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if got.WorkingDir != wd {
		t.Errorf("response WorkingDir = %q, want %q", got.WorkingDir, wd)
	}
	// The handler actually threaded it to the repo, not just echoed the request.
	if repo.lastCreateRunParams.WorkingDir != wd {
		t.Errorf("CreateRunParams.WorkingDir = %q, want %q", repo.lastCreateRunParams.WorkingDir, wd)
	}
}

// TestCreateRun_CapturesRulesetRequiredChecks is the cross-boundary test
// (#2506, cf. #618): a real POST /v0/runs (runner_kind local, inline
// workflow_spec) drives handleCreateRun -> CreateRunForTrigger -> capture ->
// CreateRunParams -> the persisted run row, against a real *githubclient.Client
// pointed at an httptest mux serving default_branch=main, a 404 classic
// protection, and an active branch ruleset requiring CI Pass. It asserts the
// created run row read back from the fake carries Contexts [CI Pass] — the
// SHIPPED observable, not just that a file was touched (#1169).
func TestCreateRun_CapturesRulesetRequiredChecks(t *testing.T) {
	repo := newFakeRepo()
	gh := newRequiredChecksGitHub(t, ghCfg{
		defaultBranch: "main",
		rulesetsList:  `[{"id":7,"target":"branch","enforcement":"active"}]`,
		rulesetBodies: map[int64]string{7: rulesetBodyDefaultBranch("CI Pass")},
	})
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: repo, GitHub: gh})

	raw, err := json.Marshal(map[string]any{
		"repo":           "o/r",
		"workflow_id":    "trivial",
		"workflow_sha":   "abc",
		"trigger_source": "cli",
		"runner_kind":    "local",
		"workflow_spec":  minimalSpecYAML,
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v0/runs", strings.NewReader(string(raw)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.handleCreateRun(w, withAuth(req))
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
	}
	var got runResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	created, err := repo.GetRun(context.Background(), got.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	snap := created.RequiredChecksSnapshot
	if snap == nil {
		t.Fatal("persisted run snapshot nil; want [CI Pass] captured through the HTTP seam")
	}
	if len(snap.Contexts) != 1 || snap.Contexts[0] != "CI Pass" {
		t.Errorf("Contexts = %v, want [CI Pass]", snap.Contexts)
	}
	if len(snap.Sources) != 1 || snap.Sources[0] != "ruleset:7" {
		t.Errorf("Sources = %v, want [ruleset:7]", snap.Sources)
	}
}

// TestCreateRun_ProtectionLookupFailure_StillCreatesRunWithNilSnapshot is the
// fail-safe companion: the same POST against a mux that 500s the protection
// lookup still returns 201 and creates a run whose snapshot is NIL — run-create
// never fails on a forge degrade, and a non-authoritative lookup never writes a
// vacuous-green empty snapshot.
func TestCreateRun_ProtectionLookupFailure_StillCreatesRunWithNilSnapshot(t *testing.T) {
	repo := newFakeRepo()
	gh := newRequiredChecksGitHub(t, ghCfg{protectionStatus: http.StatusInternalServerError})
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: repo, GitHub: gh})

	raw, err := json.Marshal(map[string]any{
		"repo":           "o/r",
		"workflow_id":    "trivial",
		"workflow_sha":   "abc",
		"trigger_source": "cli",
		"runner_kind":    "local",
		"workflow_spec":  minimalSpecYAML,
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v0/runs", strings.NewReader(string(raw)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.handleCreateRun(w, withAuth(req))
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 despite the forge degrade:\n%s", w.Code, w.Body.String())
	}
	var got runResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	created, err := repo.GetRun(context.Background(), got.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if created.RequiredChecksSnapshot != nil {
		t.Fatalf("snapshot = %+v, want nil on a protection-lookup degrade", created.RequiredChecksSnapshot)
	}
}

// TestCreateRun_RejectsRelativeWorkingDir is the C6 control: POST /v0/runs with a
// relative working_dir is a 400 naming the field, AND no run row exists
// afterwards. The second assertion reads COMMITTED STATE (the fake's run store)
// rather than error identity — a control that fired and then rolled back would
// return a byte-identical error, so the store read is what proves the rejection
// committed nothing. Deleting the server-side validation turns it red.
func TestCreateRun_RejectsRelativeWorkingDir(t *testing.T) {
	repo := newFakeRepo()
	s := newServer(t, repo)

	body := `{
		"repo": "kuhlman-labs/fishhawk",
		"workflow_id": "feature_change",
		"workflow_sha": "abc123",
		"trigger_source": "cli",
		"working_dir": "./sub"
	}`
	req := httptest.NewRequest(http.MethodPost, "/v0/runs", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.handleCreateRun(w, withAuth(req))

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "working_dir") {
		t.Errorf("error body does not name working_dir: %s", w.Body.String())
	}
	// Committed-state assertion: no run row was created.
	repo.mu.Lock()
	n := len(repo.runs)
	repo.mu.Unlock()
	if n != 0 {
		t.Errorf("run store has %d rows after a rejected create, want 0", n)
	}
}

func TestCreateRun_OptionalTriggerRef(t *testing.T) {
	repo := newFakeRepo()
	s := newServer(t, repo)

	body := `{
		"repo": "x/y",
		"workflow_id": "w",
		"workflow_sha": "abc",
		"trigger_source": "github_issue",
		"trigger_ref": "issue:1247"
	}`
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v0/runs", strings.NewReader(body))
	s.handleCreateRun(w, withAuth(req))

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
	}
	var got runResponse
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got.TriggerRef == nil || *got.TriggerRef != "issue:1247" {
		t.Errorf("TriggerRef = %v, want issue:1247", got.TriggerRef)
	}
}

func TestCreateRun_BadJSON(t *testing.T) {
	s := newServer(t, newFakeRepo())
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v0/runs", strings.NewReader("{not json"))
	s.handleCreateRun(w, withAuth(req))
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"validation_failed"`) {
		t.Errorf("body missing code: %s", w.Body.String())
	}
}

func TestCreateRun_UnknownField(t *testing.T) {
	s := newServer(t, newFakeRepo())
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v0/runs",
		strings.NewReader(`{"repo":"r","workflow_id":"w","workflow_sha":"s","trigger_source":"cli","extra":"x"}`))
	s.handleCreateRun(w, withAuth(req))
	// DisallowUnknownFields → 400.
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 on unknown field", w.Code)
	}
}

func TestCreateRun_MissingRequiredFields(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		wantField string
	}{
		{"no repo", `{"workflow_id":"w","workflow_sha":"s","trigger_source":"cli"}`, "repo"},
		{"no workflow_id", `{"repo":"r","workflow_sha":"s","trigger_source":"cli"}`, "workflow_id"},
		{"no workflow_sha", `{"repo":"r","workflow_id":"w","trigger_source":"cli"}`, "workflow_sha"},
	}
	s := newServer(t, newFakeRepo())
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v0/runs", strings.NewReader(tc.body))
			s.handleCreateRun(w, withAuth(req))
			if w.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", w.Code)
			}
			if !strings.Contains(w.Body.String(), tc.wantField) {
				t.Errorf("body missing field name %q: %s", tc.wantField, w.Body.String())
			}
		})
	}
}

func TestCreateRun_BadTriggerSource(t *testing.T) {
	s := newServer(t, newFakeRepo())
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v0/runs",
		strings.NewReader(`{"repo":"r","workflow_id":"w","workflow_sha":"s","trigger_source":"bogus"}`))
	s.handleCreateRun(w, withAuth(req))
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestCreateRun_RepoError(t *testing.T) {
	repo := newFakeRepo()
	repo.createErr = errors.New("disk full")
	s := newServer(t, repo)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v0/runs",
		strings.NewReader(`{"repo":"r","workflow_id":"w","workflow_sha":"s","trigger_source":"cli"}`))
	s.handleCreateRun(w, withAuth(req))
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"internal_error"`) {
		t.Errorf("body missing internal_error code: %s", w.Body.String())
	}
}

func TestCreateRun_NilRepoConfigured(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0"}) // no RunRepo
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v0/runs",
		strings.NewReader(`{"repo":"r","workflow_id":"w","workflow_sha":"s","trigger_source":"cli"}`))
	s.handleCreateRun(w, withAuth(req))
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
}

// -------- Idempotency-Key tests (E8.2) --------

func TestCreateRun_IdempotencyKey_Replay_Returns200(t *testing.T) {
	repo := newFakeRepo()
	s := newServer(t, repo)
	body := `{
		"repo": "x/y",
		"workflow_id": "w",
		"workflow_sha": "s",
		"trigger_source": "cli"
	}`

	req1 := httptest.NewRequest(http.MethodPost, "/v0/runs", strings.NewReader(body))
	req1.Header.Set("Content-Type", "application/json")
	req1.Header.Set("Idempotency-Key", "abc123")
	w1 := httptest.NewRecorder()
	s.handleCreateRun(w1, withAuth(req1))
	if w1.Code != http.StatusCreated {
		t.Fatalf("first status = %d, want 201:\n%s", w1.Code, w1.Body.String())
	}
	var first runResponse
	_ = json.Unmarshal(w1.Body.Bytes(), &first)

	// Replay: same key, same body → 200 with the prior run.
	req2 := httptest.NewRequest(http.MethodPost, "/v0/runs", strings.NewReader(body))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("Idempotency-Key", "abc123")
	w2 := httptest.NewRecorder()
	s.handleCreateRun(w2, withAuth(req2))
	if w2.Code != http.StatusOK {
		t.Fatalf("replay status = %d, want 200:\n%s", w2.Code, w2.Body.String())
	}
	var second runResponse
	_ = json.Unmarshal(w2.Body.Bytes(), &second)
	if second.ID != first.ID {
		t.Errorf("replay returned a different run: first=%s second=%s", first.ID, second.ID)
	}
	if len(repo.runs) != 1 {
		t.Errorf("repo has %d runs, want 1 (replay must not insert)", len(repo.runs))
	}
}

func TestCreateRun_IdempotencyKey_DifferentRepo_CreatesSeparateRun(t *testing.T) {
	repo := newFakeRepo()
	s := newServer(t, repo)
	body := func(r string) string {
		return `{"repo":"` + r + `","workflow_id":"w","workflow_sha":"s","trigger_source":"cli"}`
	}

	req1 := httptest.NewRequest(http.MethodPost, "/v0/runs", strings.NewReader(body("a/x")))
	req1.Header.Set("Content-Type", "application/json")
	req1.Header.Set("Idempotency-Key", "shared")
	w1 := httptest.NewRecorder()
	s.handleCreateRun(w1, withAuth(req1))
	if w1.Code != http.StatusCreated {
		t.Fatalf("first status = %d", w1.Code)
	}

	// Same key, different repo → separate run.
	req2 := httptest.NewRequest(http.MethodPost, "/v0/runs", strings.NewReader(body("b/y")))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("Idempotency-Key", "shared")
	w2 := httptest.NewRecorder()
	s.handleCreateRun(w2, withAuth(req2))
	if w2.Code != http.StatusCreated {
		t.Fatalf("second status = %d, want 201 (different repo, no collision)", w2.Code)
	}
	if len(repo.runs) != 2 {
		t.Errorf("repo has %d runs, want 2", len(repo.runs))
	}
}

func TestCreateRun_IdempotencyKey_DifferentKey_CreatesSeparateRun(t *testing.T) {
	repo := newFakeRepo()
	s := newServer(t, repo)
	body := `{"repo":"x/y","workflow_id":"w","workflow_sha":"s","trigger_source":"cli"}`

	for _, key := range []string{"k1", "k2"} {
		req := httptest.NewRequest(http.MethodPost, "/v0/runs", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", key)
		w := httptest.NewRecorder()
		s.handleCreateRun(w, withAuth(req))
		if w.Code != http.StatusCreated {
			t.Fatalf("status = %d for key=%s", w.Code, key)
		}
	}
	if len(repo.runs) != 2 {
		t.Errorf("repo has %d runs, want 2", len(repo.runs))
	}
}

func TestCreateRun_NoIdempotencyKey_AlwaysCreates(t *testing.T) {
	repo := newFakeRepo()
	s := newServer(t, repo)
	body := `{"repo":"x/y","workflow_id":"w","workflow_sha":"s","trigger_source":"cli"}`

	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v0/runs", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		s.handleCreateRun(w, withAuth(req))
		if w.Code != http.StatusCreated {
			t.Fatalf("iter %d status = %d", i, w.Code)
		}
	}
	if len(repo.runs) != 3 {
		t.Errorf("repo has %d runs, want 3 (no key = always create)", len(repo.runs))
	}
}

func TestCreateRun_IdempotencyKey_Whitespace_Trimmed(t *testing.T) {
	repo := newFakeRepo()
	s := newServer(t, repo)
	body := `{"repo":"x/y","workflow_id":"w","workflow_sha":"s","trigger_source":"cli"}`

	req1 := httptest.NewRequest(http.MethodPost, "/v0/runs", strings.NewReader(body))
	req1.Header.Set("Content-Type", "application/json")
	req1.Header.Set("Idempotency-Key", "abc")
	w1 := httptest.NewRecorder()
	s.handleCreateRun(w1, withAuth(req1))

	// Header with surrounding whitespace should match the original.
	req2 := httptest.NewRequest(http.MethodPost, "/v0/runs", strings.NewReader(body))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("Idempotency-Key", "  abc  ")
	w2 := httptest.NewRecorder()
	s.handleCreateRun(w2, withAuth(req2))
	if w2.Code != http.StatusOK {
		t.Errorf("whitespace-padded key didn't match original: status = %d", w2.Code)
	}
	_ = w1
	if len(repo.runs) != 1 {
		t.Errorf("repo has %d runs, want 1", len(repo.runs))
	}
}

func TestCreateRun_IdempotencyKey_LookupErrorBubbles(t *testing.T) {
	// Use a repo whose GetRunByIdempotencyKey returns an
	// unexpected error (not ErrNotFound). The handler should 500
	// rather than silently fall through to create.
	repo := &errIdempotencyRepo{}
	s := newServer(t, repo)
	body := `{"repo":"x/y","workflow_id":"w","workflow_sha":"s","trigger_source":"cli"}`
	req := httptest.NewRequest(http.MethodPost, "/v0/runs", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "abc")
	w := httptest.NewRecorder()
	s.handleCreateRun(w, withAuth(req))
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
}

// TestCreateRun_InvalidBody_WithResolvingKey_StillValidationFailed pins the
// OTHER side of the #2366 seam: validation stays AHEAD of the replay lookup.
//
// The key here ALREADY RESOLVES to a run created moments earlier, so the
// lookup would answer 200-with-that-run if it had moved ahead of the
// validator. The malformed body (no workflow_id) must still lose to the field
// check with 400 validation_failed. A malformed-body test using an arbitrary
// key could not fail for the reason it exists — the lookup would miss and fall
// through to the same 400 either way.
func TestCreateRun_InvalidBody_WithResolvingKey_StillValidationFailed(t *testing.T) {
	repo := newFakeRepo()
	s := newServer(t, repo)

	// Seed the key so it resolves.
	req1 := httptest.NewRequest(http.MethodPost, "/v0/runs",
		strings.NewReader(`{"repo":"x/y","workflow_id":"w","workflow_sha":"s","trigger_source":"cli"}`))
	req1.Header.Set("Content-Type", "application/json")
	req1.Header.Set("Idempotency-Key", "resolving")
	w1 := httptest.NewRecorder()
	s.handleCreateRun(w1, withAuth(req1))
	if w1.Code != http.StatusCreated {
		t.Fatalf("seed status = %d, want 201:\n%s", w1.Code, w1.Body.String())
	}

	// Same key, body missing workflow_id.
	req2 := httptest.NewRequest(http.MethodPost, "/v0/runs",
		strings.NewReader(`{"repo":"x/y","workflow_sha":"s","trigger_source":"cli"}`))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("Idempotency-Key", "resolving")
	w2 := httptest.NewRecorder()
	s.handleCreateRun(w2, withAuth(req2))

	if w2.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 — validation must precede the replay lookup:\n%s", w2.Code, w2.Body.String())
	}
	if !strings.Contains(w2.Body.String(), `"validation_failed"`) {
		t.Errorf("body missing validation_failed code: %s", w2.Body.String())
	}
	if len(repo.runs) != 1 {
		t.Errorf("repo has %d runs, want 1 (the malformed request creates nothing)", len(repo.runs))
	}
}

// TestCreateRun_Replay_SkipsPlanReviewerCapabilityGate covers the third
// audit-emitting admission gate the lookup now precedes (#2366). The create
// runs on a reviewer-capable deployment; the replay hits one whose reviewer
// backend is unwired, which would otherwise 400 plan_reviewer_unconfigured and
// append a SECOND run_rejected_misconfigured entry for a run that was already
// admitted. Both servers share the run repository and the audit fake, so the
// pair is one chain and the counts are comparable.
func TestCreateRun_Replay_SkipsPlanReviewerCapabilityGate(t *testing.T) {
	repo := newFakeRepo()
	au := newAuditFake()
	capable := New(Config{Addr: "127.0.0.1:0", RunRepo: repo, AuditRepo: au,
		PlanReviewer: &fakePlanReviewer{}})
	unwired := New(Config{Addr: "127.0.0.1:0", RunRepo: repo, AuditRepo: au})

	raw, err := json.Marshal(map[string]any{
		"repo":           "x/y",
		"workflow_id":    "feature_change",
		"workflow_sha":   "abc",
		"trigger_source": "cli",
		"workflow_spec":  gatingReviewSpecYAML,
	})
	if err != nil {
		t.Fatal(err)
	}
	post := func(s *Server) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v0/runs", strings.NewReader(string(raw)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", "k-reviewer")
		w := httptest.NewRecorder()
		s.handleCreateRun(w, withAuth(req))
		return w
	}

	w1 := post(capable)
	if w1.Code != http.StatusCreated {
		t.Fatalf("first status = %d, want 201 on a reviewer-capable deployment:\n%s", w1.Code, w1.Body.String())
	}
	var first runResponse
	if err := json.Unmarshal(w1.Body.Bytes(), &first); err != nil {
		t.Fatal(err)
	}

	w2 := post(unwired)
	if w2.Code != http.StatusOK {
		t.Fatalf("replay status = %d, want 200 with the existing run, not the capability rejection:\n%s", w2.Code, w2.Body.String())
	}
	var second runResponse
	if err := json.Unmarshal(w2.Body.Bytes(), &second); err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID {
		t.Errorf("replay returned run %s, want the existing %s", second.ID, first.ID)
	}
	if n := countGlobalAudits(au, "run_rejected_misconfigured"); n != 0 {
		t.Errorf("run_rejected_misconfigured audits = %d, want 0 — the capability gate ran on a replay", n)
	}
	if len(repo.runs) != 1 {
		t.Errorf("repo has %d runs, want 1", len(repo.runs))
	}
}

// errIdempotencyRepo wraps fakeRepo to inject a non-ErrNotFound
// error from GetRunByIdempotencyKey while behaving normally for
// every other method. Used to exercise the handler's "unexpected
// error" path.
type errIdempotencyRepo struct {
	fakeRepo
}

func (e *errIdempotencyRepo) GetRunByIdempotencyKey(context.Context, string, string) (*run.Run, error) {
	return nil, errors.New("simulated lookup error")
}

// --- runner_kind (E22.7 / #404) ---

func TestCreateRun_RunnerKind_DefaultsGitHubActions(t *testing.T) {
	repo := newFakeRepo()
	s := newServer(t, repo)
	body := `{
		"repo": "x/y",
		"workflow_id": "feature_change",
		"workflow_sha": "abc",
		"trigger_source": "cli"
	}`
	req := httptest.NewRequest(http.MethodPost, "/v0/runs", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.handleCreateRun(w, withAuth(req))
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d", w.Code)
	}
	var got runResponse
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got.RunnerKind != run.RunnerKindGitHubActions {
		t.Errorf("RunnerKind = %q, want github_actions", got.RunnerKind)
	}
}

func TestCreateRun_RunnerKind_AcceptsLocal(t *testing.T) {
	repo := newFakeRepo()
	s := newServer(t, repo)
	body := `{
		"repo": "x/y",
		"workflow_id": "feature_change",
		"workflow_sha": "abc",
		"trigger_source": "cli",
		"runner_kind": "local"
	}`
	req := httptest.NewRequest(http.MethodPost, "/v0/runs", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.handleCreateRun(w, withAuth(req))
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
	}
	var got runResponse
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got.RunnerKind != run.RunnerKindLocal {
		t.Errorf("RunnerKind = %q, want local", got.RunnerKind)
	}
}

func TestCreateRun_RunnerKind_RejectsUnknown(t *testing.T) {
	repo := newFakeRepo()
	s := newServer(t, repo)
	body := `{
		"repo": "x/y",
		"workflow_id": "feature_change",
		"workflow_sha": "abc",
		"trigger_source": "cli",
		"runner_kind": "k8s"
	}`
	req := httptest.NewRequest(http.MethodPost, "/v0/runs", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.handleCreateRun(w, withAuth(req))
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "runner_kind") {
		t.Errorf("body should reference runner_kind: %s", w.Body.String())
	}
}

// --- Drive mode (#1023) ---

// driveSpecYAML opts the workflow into drive mode at the spec level so
// the resolution tests can assert spec-default vs per-run override.
const driveSpecYAML = `version: "0.3"
workflows:
  trivial:
    drive: true
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        produces:
          - artifact: pull_request
`

// TestCreateRun_Drive_Resolution covers the create-time resolution
// table: the request's `drive` field (tri-state via pointer) wins over
// the workflow spec's default; absent everywhere resolves false.
func TestCreateRun_Drive_Resolution(t *testing.T) {
	cases := []struct {
		name string
		body map[string]any
		want bool
	}{
		{
			name: "absent_no_spec_defaults_false",
			body: map[string]any{},
			want: false,
		},
		{
			name: "request_true_no_spec",
			body: map[string]any{"drive": true},
			want: true,
		},
		{
			name: "spec_default_true_no_override",
			body: map[string]any{"workflow_spec": driveSpecYAML},
			want: true,
		},
		{
			name: "request_false_overrides_spec_true",
			body: map[string]any{"workflow_spec": driveSpecYAML, "drive": false},
			want: false,
		},
		{
			name: "request_true_overrides_spec_absent",
			body: map[string]any{"workflow_spec": minimalSpecYAML, "drive": true},
			want: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := newFakeRepo()
			s := newServer(t, repo)
			body := map[string]any{
				"repo":           "x/y",
				"workflow_id":    "trivial",
				"workflow_sha":   "abc",
				"trigger_source": "cli",
			}
			for k, v := range tc.body {
				body[k] = v
			}
			raw, _ := json.Marshal(body)
			req := httptest.NewRequest(http.MethodPost, "/v0/runs", strings.NewReader(string(raw)))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			s.handleCreateRun(w, withAuth(req))
			if w.Code != http.StatusCreated {
				t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
			}
			if got := repo.lastCreateRunParams.Drive; got != tc.want {
				t.Errorf("CreateRunParams.Drive = %v, want %v", got, tc.want)
			}
		})
	}
}

// v2AutoAdvanceSpecYAML and v1DriveSpecYAML are a matched pair differing only
// in the version and the flag's SPELLING. workflow-v2 renames v0/v1's `drive`
// to `auto_advance` (E52.6 / #2218); the parser rewrites it back to the
// `drive` key before the typed decode, so every downstream consumer — the Go
// field, the runs.drive column, the read sites that surface next_action — sees
// the identical value.
const v2AutoAdvanceSpecYAML = `version: "2"
workflows:
  trivial:
    auto_advance: true
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        produces:
          - artifact: pull_request
`

const v1DriveSpecYAML = `version: "1.6"
workflows:
  trivial:
    drive: true
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        produces:
          - artifact: pull_request
`

// TestCreateRun_V2AutoAdvanceParityWithV1Drive is defense-in-depth for the
// rename (binding approval condition 2). Routing auto_advance to the existing
// drive key at parse time makes the parse-level assertion Workflow.Drive ==
// true close to a proof by construction, but acceptance criterion 4 names
// next_action surfacing explicitly, so this pins that the v2 SPELLING reaches
// the same created-run flag through the REAL run-create path: a normalization
// that silently stopped firing would leave the v2 run un-driven while the v1
// run advanced. It deliberately does NOT re-test v1's downstream next_action
// behaviour, which is pre-existing and unchanged.
func TestCreateRun_V2AutoAdvanceParityWithV1Drive(t *testing.T) {
	createWithSpec := func(t *testing.T, specYAML string) bool {
		t.Helper()
		repo := newFakeRepo()
		s := newServer(t, repo)
		raw, _ := json.Marshal(map[string]any{
			"repo":           "x/y",
			"workflow_id":    "trivial",
			"workflow_sha":   "abc",
			"trigger_source": "cli",
			"workflow_spec":  specYAML,
		})
		req := httptest.NewRequest(http.MethodPost, "/v0/runs", strings.NewReader(string(raw)))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		s.handleCreateRun(w, withAuth(req))
		if w.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
		}
		return repo.lastCreateRunParams.Drive
	}

	v1Drive := createWithSpec(t, v1DriveSpecYAML)
	v2Drive := createWithSpec(t, v2AutoAdvanceSpecYAML)
	if !v1Drive {
		t.Fatalf("v1 `drive: true` produced Drive=false; the pair is not exercising the flag")
	}
	if v2Drive != v1Drive {
		t.Errorf("v2 `auto_advance: true` produced Drive=%v, want the same %v as v1 `drive: true`", v2Drive, v1Drive)
	}
}
