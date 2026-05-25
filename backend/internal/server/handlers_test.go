package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHandleHealth(t *testing.T) {
	s := New(Config{})
	rec := httptest.NewRecorder()
	s.handleHealth(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("content-type = %q, want application/json", got)
	}

	var body healthResponse
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Status != "ok" {
		t.Errorf("status field = %q, want ok", body.Status)
	}
	if body.Version == "" {
		t.Error("version field must not be empty")
	}
	if body.GitSHA == "" {
		t.Error("git_sha field must not be empty")
	}
	if body.MinRunnerVersion == "" {
		t.Error("min_runner_version field must not be empty")
	}
	if len(body.Schemas) == 0 {
		t.Error("schemas field must not be empty")
	}
	if body.Schemas["plan-standard-v1"] == "" {
		t.Error("schemas[plan-standard-v1] must not be empty")
	}
	if body.Schemas["workflow-v0"] == "" {
		t.Error("schemas[workflow-v0] must not be empty")
	}
}
