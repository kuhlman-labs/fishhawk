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
