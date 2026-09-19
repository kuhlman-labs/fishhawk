package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/account"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
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

// TestCreateRun_BadTriggerSourceMessageNamesEverySource is AC5 (E54.22 /
// #2826): the 400 body must name EVERY accepted source, asserted by iterating
// run.ValidTriggerSources() rather than against a hard-coded literal — so the
// message and the enforced set cannot drift apart. A hand-written literal here
// would go stale in exactly the way the message it guards does.
func TestCreateRun_BadTriggerSourceMessageNamesEverySource(t *testing.T) {
	s := newServer(t, newFakeRepo())
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v0/runs",
		strings.NewReader(`{"repo":"r","workflow_id":"w","workflow_sha":"s","trigger_source":"bogus"}`))
	s.handleCreateRun(w, withAuth(req))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400:\n%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, ts := range run.ValidTriggerSources() {
		if !strings.Contains(body, string(ts)) {
			t.Errorf("400 body does not name accepted trigger source %q:\n%s", ts, body)
		}
	}
}

// TestCreateRun_OnDemandWithIssueContext is the on-demand producer at the HTTP
// seam (E54.22 / #2826): an on_demand run carrying an issue_context is
// ACCEPTED and persisted with trigger_source=on_demand. The issue context is
// the point — ADR-065's groom stage declares a REQUIRED github_issue input, so
// a refusal here would make the grooming run unstartable.
func TestCreateRun_OnDemandWithIssueContext(t *testing.T) {
	repo := newFakeRepo()
	s := newServer(t, repo)

	body := `{
		"repo": "kuhlman-labs/fishhawk",
		"workflow_id": "backlog_grooming",
		"workflow_sha": "abc123",
		"trigger_source": "on_demand",
		"trigger_ref": "issue:2826",
		"issue_context": {"number": 2826, "title": "groom the backlog", "url": "https://github.com/kuhlman-labs/fishhawk/issues/2826"}
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
	if got.TriggerSource != string(run.TriggerOnDemand) {
		t.Errorf("TriggerSource = %q, want on_demand", got.TriggerSource)
	}
	// Read the PERSISTED row, not just the echoed response: the accepted set is
	// enforced at the storage layer too (runs_trigger_source_check).
	persisted, ok := repo.runs[got.ID]
	if !ok {
		t.Fatalf("run %s was not persisted", got.ID)
	}
	if persisted.TriggerSource != run.TriggerOnDemand {
		t.Errorf("persisted TriggerSource = %q, want on_demand", persisted.TriggerSource)
	}
	if !persisted.IsIssueAnchored() {
		t.Error("a persisted on_demand run must report IsIssueAnchored() — the issuecomment gate comments depend on it")
	}
}

// TestCreateRun_OnDemandWithoutIssueContext pins a DELIBERATE acceptance
// (E54.24 / #2830, widening E54.22 / #2826): an on_demand run that carries
// NEITHER issue_context NOR trigger_ref is still accepted 201, not refused.
// IsIssueAnchored is a SOURCE-level predicate only — it says on_demand is a
// source that MAY carry an issue reference, never that a given run must. Every
// consumer (the notifier, the prompt renderer) independently re-checks
// TriggerRef and InstallationID afterwards, so an issue-less on_demand run
// simply posts no issue comments (pinned downstream by the on_demand-with-nil-
// TriggerRef row in backend/internal/issuecomment/notifier_test.go's
// TestNotify_IssueAnchoredSuppression) rather than erroring here.
//
// If a future change decides an on_demand run MUST be anchored at creation,
// that is a real design reversal, not a bug fix — and THIS test is where it
// should be argued.
func TestCreateRun_OnDemandWithoutIssueContext(t *testing.T) {
	repo := newFakeRepo()
	s := newServer(t, repo)

	body := `{
		"repo": "kuhlman-labs/fishhawk",
		"workflow_id": "backlog_grooming",
		"workflow_sha": "abc123",
		"trigger_source": "on_demand"
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
	if got.TriggerSource != string(run.TriggerOnDemand) {
		t.Errorf("TriggerSource = %q, want on_demand", got.TriggerSource)
	}
	persisted, ok := repo.runs[got.ID]
	if !ok {
		t.Fatalf("run %s was not persisted", got.ID)
	}
	if persisted.TriggerRef != nil {
		t.Errorf("persisted TriggerRef = %v, want nil — an issue-less on_demand run is un-anchored, not silently defaulted", *persisted.TriggerRef)
	}
	if !persisted.IsIssueAnchored() {
		t.Error("IsIssueAnchored() must stay true regardless of TriggerRef — it is a SOURCE-level predicate, not a ref check")
	}
}

// TestCreateRun_IssueContextRejectedOnNonAnchoredSource is the RETAINED
// control (counterfactual (a), E54.22 / #2826): relaxing the issue_context
// coupling to the issue-anchored set must NOT make it accept cli or ui. Delete
// the refusal block in runs.go and this test goes RED.
func TestCreateRun_IssueContextRejectedOnNonAnchoredSource(t *testing.T) {
	for _, src := range []run.TriggerSource{run.TriggerCLI, run.TriggerUI} {
		t.Run(string(src), func(t *testing.T) {
			repo := newFakeRepo()
			s := newServer(t, repo)
			body := `{
				"repo": "kuhlman-labs/fishhawk",
				"workflow_id": "feature_change",
				"workflow_sha": "abc123",
				"trigger_source": "` + string(src) + `",
				"issue_context": {"number": 42, "title": "t", "url": "https://example.invalid/42"}
			}`
			req := httptest.NewRequest(http.MethodPost, "/v0/runs", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			s.handleCreateRun(w, withAuth(req))

			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 for issue_context on trigger_source=%s:\n%s", w.Code, src, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), "issue_context") {
				t.Errorf("400 body does not name the issue_context field:\n%s", w.Body.String())
			}
			// No run may be created by a refused request.
			if len(repo.runs) != 0 {
				t.Errorf("refused request created %d runs, want 0", len(repo.runs))
			}
		})
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

// --- Forge ladder + gitlab run creation (E45.46 / #3463) ---

// fakeGitLabInstallations is a fixed-answer GitLabInstallationResolver that
// records every lookup so a test can assert the registry was (or was NOT)
// consulted.
type fakeGitLabInstallations struct {
	inst         account.GitLabInstallation
	found        bool
	err          error
	projectCalls int
	lastPath     string
	refCalls     int
	lastRef      string
}

func (f *fakeGitLabInstallations) ResolveGitLabProject(_ context.Context, projectPath string) (account.GitLabInstallation, bool, error) {
	f.projectCalls++
	f.lastPath = projectPath
	return f.inst, f.found, f.err
}

func (f *fakeGitLabInstallations) ResolveGitLabInstallationByRef(_ context.Context, ref string) (account.GitLabInstallation, bool, error) {
	f.refCalls++
	f.lastRef = ref
	return f.inst, f.found, f.err
}

// neverDialedGitHubClient is a *githubclient.Client whose installation endpoint
// fails the test if hit — the never-dialed seam for the gitlab branch.
func neverDialedGitHubClient(t *testing.T) (*githubclient.Client, *installRecorder) {
	t.Helper()
	rec := &installRecorder{}
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/", func(w http.ResponseWriter, r *http.Request) {
		rec.mu.Lock()
		rec.hits++
		rec.mu.Unlock()
		t.Errorf("GitHub client dialed %s %s for a gitlab run; want the App installation lookup skipped", r.Method, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":1}`))
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

func postForgeCreateRun(t *testing.T, s *Server, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v0/runs", strings.NewReader(string(raw)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.handleCreateRun(w, withAuth(req))
	return w
}

func gitlabCreateBody() map[string]any {
	return map[string]any{
		"repo":           "acme/platform/api",
		"workflow_id":    "trivial",
		"workflow_sha":   "abc",
		"trigger_source": "cli",
		"runner_kind":    "local",
		"workflow_spec":  minimalSpecYAML,
		"forge":          "gitlab",
	}
}

func forgeGetRun(t *testing.T, s *Server, id uuid.UUID) runResponse {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v0/runs/"+id.String(), nil)
	req.SetPathValue("run_id", id.String())
	w := httptest.NewRecorder()
	s.handleGetRun(w, withAuth(req))
	if w.Code != http.StatusOK {
		t.Fatalf("GET run status = %d:\n%s", w.Code, w.Body.String())
	}
	var got runResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	return got
}

// TestCreateRun_Forge_ExplicitGitLab_StampsInstallationRef is the
// cross-boundary test: request payload → resolveCreateForge →
// resolveCreateGitLabInstallation → CreateRunForTrigger → run.CreateRunParams
// (committed-state read of the captured params) with the GitHub client
// NEVER dialed. It then pins the read surfaces: GET /v0/runs/{id} carries
// forge:gitlab + forge_base_url (installation column, else cfg fallback) and
// the LIST body carries forge but NO forge_base_url — the single-run-only
// asymmetry the spawn-side helper relies on.
func TestCreateRun_Forge_ExplicitGitLab_StampsInstallationRef(t *testing.T) {
	cases := []struct {
		name        string
		instBaseURL string
		cfgBaseURL  string
		wantBaseURL string
	}{
		{"installation_column_wins", "https://gitlab.example.com", "https://fallback.example.com", "https://gitlab.example.com"},
		{"empty_column_falls_to_cfg", "", "https://fallback.example.com", "https://fallback.example.com"},
		{"neither_omitted", "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := newFakeRepo()
			gh, rec := neverDialedGitHubClient(t)
			gl := &fakeGitLabInstallations{
				inst:  account.GitLabInstallation{InstallationRef: "gitlab:4242", ProjectPath: "acme/platform/api", ForgeBaseURL: tc.instBaseURL},
				found: true,
			}
			s := New(Config{Addr: "127.0.0.1:0", RunRepo: repo, GitHub: gh, GitLabInstallations: gl, GitLabBaseURL: tc.cfgBaseURL,
				// Registry says github for this owner: explicit gitlab must win.
				RepoProviders: &fakeProviderResolver{provider: "github", found: true}})

			w := postForgeCreateRun(t, s, gitlabCreateBody())
			if w.Code != http.StatusCreated {
				t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
			}
			// Committed state: what the handler threaded into the repo.
			p := repo.lastCreateRunParams
			if p.InstallationRef == nil || *p.InstallationRef != "gitlab:4242" {
				t.Errorf("CreateRunParams.InstallationRef = %v, want gitlab:4242", p.InstallationRef)
			}
			if p.InstallationID != nil {
				t.Errorf("CreateRunParams.InstallationID = %d, want nil", *p.InstallationID)
			}
			if p.RunnerKind != run.RunnerKindLocal {
				t.Errorf("CreateRunParams.RunnerKind = %q, want local", p.RunnerKind)
			}
			if gl.projectCalls != 1 || gl.lastPath != "acme/platform/api" {
				t.Errorf("ResolveGitLabProject calls=%d path=%q, want 1 call with the repo", gl.projectCalls, gl.lastPath)
			}
			rec.mu.Lock()
			hits := rec.hits
			rec.mu.Unlock()
			if hits != 0 {
				t.Errorf("GitHub installation endpoint hits = %d, want 0 (never dialed)", hits)
			}
			var created runResponse
			if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
				t.Fatal(err)
			}
			if created.Forge != "gitlab" {
				t.Errorf("201 body forge = %q, want gitlab", created.Forge)
			}

			got := forgeGetRun(t, s, created.ID)
			if got.Forge != "gitlab" {
				t.Errorf("GET forge = %q, want gitlab", got.Forge)
			}
			if got.ForgeBaseURL != tc.wantBaseURL {
				t.Errorf("GET forge_base_url = %q, want %q", got.ForgeBaseURL, tc.wantBaseURL)
			}
			if gl.refCalls != 1 || gl.lastRef != "gitlab:4242" {
				t.Errorf("ResolveGitLabInstallationByRef calls=%d ref=%q, want 1 call with the stamped ref", gl.refCalls, gl.lastRef)
			}

			// LIST: forge present, forge_base_url absent — raw-body check so
			// an omitempty-elided field is distinguishable from an empty one.
			lreq := httptest.NewRequest(http.MethodGet, "/v0/runs?repo=acme/platform/api", nil)
			lw := httptest.NewRecorder()
			s.handleListRuns(lw, withAuth(lreq))
			if lw.Code != http.StatusOK {
				t.Fatalf("list status = %d:\n%s", lw.Code, lw.Body.String())
			}
			var list struct {
				Items []map[string]any `json:"items"`
			}
			if err := json.Unmarshal(lw.Body.Bytes(), &list); err != nil {
				t.Fatal(err)
			}
			if len(list.Items) != 1 {
				t.Fatalf("list items = %d, want 1", len(list.Items))
			}
			if list.Items[0]["forge"] != "gitlab" {
				t.Errorf("list item forge = %v, want gitlab", list.Items[0]["forge"])
			}
			if _, present := list.Items[0]["forge_base_url"]; present {
				t.Errorf("list item carries forge_base_url = %v; want ABSENT (single-run read only)", list.Items[0]["forge_base_url"])
			}
			if gl.refCalls != 1 {
				t.Errorf("list read consulted the registry (refCalls=%d); want no per-row lookup", gl.refCalls)
			}
		})
	}
}

// TestCreateRun_GetRun_ResolverErrorFallsToDeploymentDefault pins the
// warn-and-fall-back branch of the single-run read: a registry error never
// fails the GET; forge_base_url falls to cfg.GitLabBaseURL.
func TestCreateRun_GetRun_ResolverErrorFallsToDeploymentDefault(t *testing.T) {
	repo := newFakeRepo()
	gl := &fakeGitLabInstallations{err: errors.New("registry down")}
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: repo, GitLabInstallations: gl, GitLabBaseURL: "https://fallback.example.com"})
	ref := "gitlab:9"
	seeded, err := repo.CreateRun(context.Background(), run.CreateRunParams{Repo: "acme/api", WorkflowID: "w", WorkflowSHA: "s", TriggerSource: run.TriggerCLI, RunnerKind: run.RunnerKindLocal, InstallationRef: &ref})
	if err != nil {
		t.Fatal(err)
	}
	got := forgeGetRun(t, s, seeded.ID)
	if got.Forge != "gitlab" || got.ForgeBaseURL != "https://fallback.example.com" {
		t.Errorf("GET = forge %q base %q, want gitlab / the deployment default on a resolver error", got.Forge, got.ForgeBaseURL)
	}
}

// TestCreateRun_Forge_DerivedFromRepoProviders: forge omitted + the registry
// says gitlab for the owner → a gitlab run (installation stamped, never
// dialed GitHub).
func TestCreateRun_Forge_DerivedFromRepoProviders(t *testing.T) {
	repo := newFakeRepo()
	gh, _ := neverDialedGitHubClient(t)
	gl := &fakeGitLabInstallations{inst: account.GitLabInstallation{InstallationRef: "gitlab:4242"}, found: true}
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: repo, GitHub: gh, GitLabInstallations: gl,
		RepoProviders: &fakeProviderResolver{provider: "gitlab", found: true}})
	body := gitlabCreateBody()
	delete(body, "forge")
	w := postForgeCreateRun(t, s, body)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
	}
	if p := repo.lastCreateRunParams; p.InstallationRef == nil || *p.InstallationRef != "gitlab:4242" {
		t.Errorf("InstallationRef = %v, want gitlab:4242 derived from the registry", p.InstallationRef)
	}
	var created runResponse
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Forge != "gitlab" {
		t.Errorf("forge = %q, want gitlab", created.Forge)
	}
}

// TestCreateRun_Forge_ExplicitGitHubWinsOverRegistryGitLab is the backend half
// of constraint 1's "explicit wins": the registry says gitlab for the owner,
// the body pins github → a github run whose App installation IS resolved, and
// the gitlab registry is never consulted. Counterfactual vehicle for the
// explicit-wins branch of resolveCreateForge.
func TestCreateRun_Forge_ExplicitGitHubWinsOverRegistryGitLab(t *testing.T) {
	repo := newFakeRepo()
	rec := &installRecorder{}
	gh := recordingInstallGitHubClient(t, 77, rec)
	gl := &fakeGitLabInstallations{inst: account.GitLabInstallation{InstallationRef: "gitlab:4242"}, found: true}
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: repo, GitHub: gh, GitLabInstallations: gl,
		RepoProviders: &fakeProviderResolver{provider: "gitlab", found: true}})
	body := gitlabCreateBody()
	body["repo"] = "acme/api"
	body["forge"] = "github"
	w := postForgeCreateRun(t, s, body)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
	}
	p := repo.lastCreateRunParams
	if p.InstallationRef != nil {
		t.Errorf("InstallationRef = %q, want nil on an explicit github run", *p.InstallationRef)
	}
	if p.InstallationID == nil || *p.InstallationID != 77 {
		t.Errorf("InstallationID = %v, want 77 (App installation resolved)", p.InstallationID)
	}
	if gl.projectCalls != 0 {
		t.Errorf("gitlab registry consulted %d times; want 0", gl.projectCalls)
	}
	var created runResponse
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Forge != "github" {
		t.Errorf("forge = %q, want github", created.Forge)
	}
}

// Omitted forge with an unregistered or doubly-registered owner (found=false)
// defaults to github: today's behaviour, the App installation resolved.
func TestCreateRun_Forge_AmbiguousOrUnknownOwner_DefaultsGitHub(t *testing.T) {
	for _, name := range []string{"unknown", "ambiguous"} {
		t.Run(name, func(t *testing.T) {
			repo := newFakeRepo()
			rec := &installRecorder{}
			gh := recordingInstallGitHubClient(t, 5, rec)
			gl := &fakeGitLabInstallations{found: true, inst: account.GitLabInstallation{InstallationRef: "gitlab:1"}}
			s := New(Config{Addr: "127.0.0.1:0", RunRepo: repo, GitHub: gh, GitLabInstallations: gl,
				RepoProviders: &fakeProviderResolver{found: false}})
			body := gitlabCreateBody()
			body["repo"] = "acme/api"
			delete(body, "forge")
			w := postForgeCreateRun(t, s, body)
			if w.Code != http.StatusCreated {
				t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
			}
			p := repo.lastCreateRunParams
			if p.InstallationID == nil || *p.InstallationID != 5 || p.InstallationRef != nil {
				t.Errorf("params = (id %v, ref %v), want (5, nil): github default", p.InstallationID, p.InstallationRef)
			}
			if gl.projectCalls != 0 {
				t.Errorf("gitlab registry consulted; want 0 calls")
			}
		})
	}
}

func TestCreateRun_Forge_ResolverError_503(t *testing.T) {
	repo := newFakeRepo()
	gl := &fakeGitLabInstallations{}
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: repo, GitLabInstallations: gl,
		RepoProviders: &fakeProviderResolver{err: errors.New("db down")}})
	body := gitlabCreateBody()
	delete(body, "forge")
	w := postForgeCreateRun(t, s, body)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503:\n%s", w.Code, w.Body.String())
	}
	if code := decodeErrorCode(t, w); code != "forge_unresolvable" {
		t.Errorf("code = %q, want forge_unresolvable", code)
	}
	if len(repo.runs) != 0 {
		t.Errorf("run rows = %d, want 0", len(repo.runs))
	}
}

func TestCreateRun_Forge_InvalidValue_400(t *testing.T) {
	repo := newFakeRepo()
	// A resolver that would answer if consulted: the explicit invalid value
	// must be refused BEFORE the ladder reaches it.
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: repo,
		RepoProviders: &fakeProviderResolver{provider: "github", found: true}})
	body := gitlabCreateBody()
	body["forge"] = "bitbucket"
	w := postForgeCreateRun(t, s, body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400:\n%s", w.Code, w.Body.String())
	}
	if code := decodeErrorCode(t, w); code != "validation_failed" {
		t.Errorf("code = %q, want validation_failed", code)
	}
	if !strings.Contains(w.Body.String(), `"field":"forge"`) || !strings.Contains(w.Body.String(), "github, gitlab") {
		t.Errorf("body should name the forge field and both accepted values: %s", w.Body.String())
	}
	if len(repo.runs) != 0 {
		t.Errorf("run rows = %d, want 0", len(repo.runs))
	}
}

// One case per branch: github_actions explicit, and empty (which would
// default to github_actions). Both refused 400 naming both accepted kinds,
// BEFORE the registry is consulted. Counterfactual vehicle for the
// runner_kind pairing check.
func TestCreateRun_GitLab_RejectsRunnerKindGitHubActionsAndEmpty(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind any // nil → omit the field
	}{
		{"github_actions", "github_actions"},
		{"empty", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := newFakeRepo()
			gl := &fakeGitLabInstallations{found: true, inst: account.GitLabInstallation{InstallationRef: "gitlab:1"}}
			s := New(Config{Addr: "127.0.0.1:0", RunRepo: repo, GitLabInstallations: gl})
			body := gitlabCreateBody()
			if tc.kind == nil {
				delete(body, "runner_kind")
			} else {
				body["runner_kind"] = tc.kind
			}
			w := postForgeCreateRun(t, s, body)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400:\n%s", w.Code, w.Body.String())
			}
			if code := decodeErrorCode(t, w); code != "validation_failed" {
				t.Errorf("code = %q, want validation_failed", code)
			}
			b := w.Body.String()
			if !strings.Contains(b, `"field":"runner_kind"`) || !strings.Contains(b, "local") || !strings.Contains(b, "gitlab_ci") {
				t.Errorf("body should name runner_kind and both accepted kinds (local, gitlab_ci): %s", b)
			}
			if gl.projectCalls != 0 {
				t.Errorf("registry consulted %d times; want 0 (pairing refused first)", gl.projectCalls)
			}
			if len(repo.runs) != 0 {
				t.Errorf("run rows = %d, want 0", len(repo.runs))
			}
		})
	}
}

func TestCreateRun_GitLab_RequiresInlineSpec_422(t *testing.T) {
	repo := newFakeRepo()
	gl := &fakeGitLabInstallations{found: true, inst: account.GitLabInstallation{InstallationRef: "gitlab:1"}}
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: repo, GitLabInstallations: gl})
	body := gitlabCreateBody()
	delete(body, "workflow_spec")
	w := postForgeCreateRun(t, s, body)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422:\n%s", w.Code, w.Body.String())
	}
	if code := decodeErrorCode(t, w); code != "workflow_spec_required" {
		t.Errorf("code = %q, want workflow_spec_required", code)
	}
	if gl.projectCalls != 0 {
		t.Errorf("registry consulted; want 0 calls")
	}
	if len(repo.runs) != 0 {
		t.Errorf("run rows = %d, want 0", len(repo.runs))
	}
}

func TestCreateRun_GitLab_UnregisteredProject_422_NamesRemedy(t *testing.T) {
	repo := newFakeRepo()
	gl := &fakeGitLabInstallations{found: false}
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: repo, GitLabInstallations: gl})
	w := postForgeCreateRun(t, s, gitlabCreateBody())
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422:\n%s", w.Code, w.Body.String())
	}
	if code := decodeErrorCode(t, w); code != "gitlab_project_not_registered" {
		t.Errorf("code = %q, want gitlab_project_not_registered", code)
	}
	var e errorEnvelope
	if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil {
		t.Fatal(err)
	}
	want := "fishhawkd installation register --provider gitlab --account-key acme --installation-ref gitlab:<project_id> --project-path acme/platform/api"
	if !strings.Contains(e.Error.Message, want) {
		t.Errorf("message should name the remedy %q:\n%s", want, e.Error.Message)
	}
	if len(repo.runs) != 0 {
		t.Errorf("run rows = %d, want 0", len(repo.runs))
	}
}

func TestCreateRun_GitLab_NoRegistry_503(t *testing.T) {
	repo := newFakeRepo()
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: repo}) // GitLabInstallations nil
	w := postForgeCreateRun(t, s, gitlabCreateBody())
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503:\n%s", w.Code, w.Body.String())
	}
	if code := decodeErrorCode(t, w); code != "gitlab_unconfigured" {
		t.Errorf("code = %q, want gitlab_unconfigured", code)
	}
	if len(repo.runs) != 0 {
		t.Errorf("run rows = %d, want 0", len(repo.runs))
	}
}

func TestCreateRun_GitLab_ResolverError_500(t *testing.T) {
	repo := newFakeRepo()
	gl := &fakeGitLabInstallations{err: errors.New("registry down")}
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: repo, GitLabInstallations: gl})
	w := postForgeCreateRun(t, s, gitlabCreateBody())
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500:\n%s", w.Code, w.Body.String())
	}
	if code := decodeErrorCode(t, w); code != "internal_error" {
		t.Errorf("code = %q, want internal_error", code)
	}
	if len(repo.runs) != 0 {
		t.Errorf("run rows = %d, want 0", len(repo.runs))
	}
}

// A plain github create (no forge field, no resolver wired) is byte-for-byte
// today's behaviour, and the response now ALWAYS carries forge:github on both
// the 201 body and the list read.
func TestCreateRun_GitHub_Unchanged_ForgeFieldPresent(t *testing.T) {
	repo := newFakeRepo()
	rec := &installRecorder{}
	gh := recordingInstallGitHubClient(t, 9, rec)
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: repo, GitHub: gh})
	body := gitlabCreateBody()
	body["repo"] = "acme/api"
	delete(body, "forge")
	w := postForgeCreateRun(t, s, body)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
	}
	if p := repo.lastCreateRunParams; p.InstallationID == nil || *p.InstallationID != 9 || p.InstallationRef != nil {
		t.Errorf("params = (id %v, ref %v), want (9, nil)", p.InstallationID, p.InstallationRef)
	}
	var raw map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if raw["forge"] != "github" {
		t.Errorf("201 body forge = %v, want github", raw["forge"])
	}
	if _, present := raw["forge_base_url"]; present {
		t.Errorf("201 body carries forge_base_url on a github run; want absent")
	}
	id := uuid.MustParse(raw["id"].(string))
	got := forgeGetRun(t, s, id)
	if got.Forge != "github" || got.ForgeBaseURL != "" {
		t.Errorf("GET = forge %q base %q, want github / empty", got.Forge, got.ForgeBaseURL)
	}
}

// TestCreateRun_BadRunnerKindMessageNamesEveryKind is the done-means for the
// derived message: the 400 for an unknown runner_kind must name EVERY member
// of run.ValidRunnerKinds — the literal it replaced silently omitted gitlab_ci.
func TestCreateRun_BadRunnerKindMessageNamesEveryKind(t *testing.T) {
	repo := newFakeRepo()
	s := newServer(t, repo)
	body := gitlabCreateBody()
	delete(body, "forge")
	body["runner_kind"] = "k8s"
	w := postForgeCreateRun(t, s, body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400:\n%s", w.Code, w.Body.String())
	}
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil {
		t.Fatal(err)
	}
	for kind := range run.ValidRunnerKinds {
		if !strings.Contains(e.Error.Message, kind) {
			t.Errorf("message %q does not name runner_kind %q", e.Error.Message, kind)
		}
	}
	if len(run.ValidRunnerKinds) < 3 {
		t.Fatalf("ValidRunnerKinds has %d members; the done-means needs gitlab_ci present", len(run.ValidRunnerKinds))
	}
}
