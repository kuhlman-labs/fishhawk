package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

func TestErrorEnvelope_Shape(t *testing.T) {
	// Decoding a known 400 confirms the envelope matches OpenAPI's
	// error schema verbatim. If the field names drift, clients
	// switching on `error.code` break.
	s := newServer(t, newFakeRepo())
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v0/runs", strings.NewReader("{not json"))
	s.Handler().ServeHTTP(w, req)
	body, _ := io.ReadAll(w.Body)
	var env errorEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if env.Error.Code == "" || env.Error.Message == "" {
		t.Errorf("error envelope missing code/message: %+v", env)
	}
}

// TestGetRun_SurfacesCostFields closes the persist→response seam for the
// cost rollup (#649 / #678 Bug 2): runs.cost_usd_total and
// runs.resolved_model are populated in the DB but were absent from the
// GET /v0/runs/{id} response because toRunResponse never surfaced them.
// Seed a run with a non-zero cost + a resolved model, fetch it through
// the handler, and assert both fields decode with the seeded values.
func TestGetRun_SurfacesCostFields(t *testing.T) {
	repo := newFakeRepo()
	s := newServer(t, repo)

	seeded, _ := repo.CreateRun(context.Background(), run.CreateRunParams{
		Repo: "kuhlman-labs/fishhawk", WorkflowID: "w", WorkflowSHA: "s",
		TriggerSource: run.TriggerCLI,
	})
	// fakeRepo stores the run by pointer; stamp the cost rollup fields
	// the trace handler would have accumulated.
	seeded.CostUSDTotal = 2.99
	seeded.ResolvedModel = "claude-opus-4-8"

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/v0/runs/%s", seeded.ID), nil)
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}

	// Decode into a map so a missing key is distinguishable from a
	// zero value — the bug was the fields being absent entirely.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if _, ok := raw["cost_usd_total"]; !ok {
		t.Error("response missing cost_usd_total field")
	}
	if _, ok := raw["resolved_model"]; !ok {
		t.Error("response missing resolved_model field")
	}

	var resp runResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode runResponse: %v", err)
	}
	if resp.CostUSDTotal != 2.99 {
		t.Errorf("cost_usd_total = %v, want 2.99", resp.CostUSDTotal)
	}
	if resp.ResolvedModel != "claude-opus-4-8" {
		t.Errorf("resolved_model = %q, want claude-opus-4-8", resp.ResolvedModel)
	}
}
