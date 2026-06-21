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

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
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

// TestGetRun_SurfacesFixupModel closes the persist→response seam for the #1164
// fix-up model surface: GET /v0/runs/{id} returns fixup_model {model, source,
// pass_ordinal} distilled from the run's newest stage_fixup_triggered entry.
func TestGetRun_SurfacesFixupModel(t *testing.T) {
	repo := newFakeRepo()
	s := newServer(t, repo)
	au := newAuditFake()
	s.cfg.AuditRepo = au

	seeded, _ := repo.CreateRun(context.Background(), run.CreateRunParams{
		Repo: "kuhlman-labs/fishhawk", WorkflowID: "w", WorkflowSHA: "s",
		TriggerSource: run.TriggerCLI,
	})
	rid := seeded.ID
	payload, _ := json.Marshal(map[string]any{
		"fixup_model":        "claude-haiku-4-5-20251001",
		"fixup_model_source": "operator",
		"pass_ordinal":       1,
	})
	au.seeded = append(au.seeded, &audit.Entry{
		RunID: &rid, Category: CategoryStageFixupTriggered, Payload: payload,
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/v0/runs/%s", seeded.ID), nil)
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp runResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode runResponse: %v", err)
	}
	if resp.FixupModel == nil {
		t.Fatalf("fixup_model absent; want the surfaced pin")
	}
	if resp.FixupModel.Model != "claude-haiku-4-5-20251001" {
		t.Errorf("fixup_model.model = %q, want claude-haiku-4-5-20251001", resp.FixupModel.Model)
	}
	if resp.FixupModel.Source != "operator" {
		t.Errorf("fixup_model.source = %q, want operator", resp.FixupModel.Source)
	}
	if resp.FixupModel.PassOrdinal != 1 {
		t.Errorf("fixup_model.pass_ordinal = %d, want 1", resp.FixupModel.PassOrdinal)
	}
}

// TestGetRun_OmitsFixupModelWhenNoFixup asserts the fixup_model field is omitted
// when the run has had no fix-up (no stage_fixup_triggered entry) — byte-
// identical to today's response for non-fix-up runs.
func TestGetRun_OmitsFixupModelWhenNoFixup(t *testing.T) {
	repo := newFakeRepo()
	s := newServer(t, repo)
	s.cfg.AuditRepo = newAuditFake()

	seeded, _ := repo.CreateRun(context.Background(), run.CreateRunParams{
		Repo: "kuhlman-labs/fishhawk", WorkflowID: "w", WorkflowSHA: "s",
		TriggerSource: run.TriggerCLI,
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/v0/runs/%s", seeded.ID), nil)
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if _, ok := raw["fixup_model"]; ok {
		t.Errorf("fixup_model present on a run with no fix-up; want omitted")
	}
}

// TestFixupModelForRun_DefensiveBranches covers fixupModelForRun's nil-return
// guards: nil AuditRepo, a malformed payload, and a pre-#1164 entry that
// carried no fixup_model key (the absent-key fall-through, distinguished from a
// present-but-empty pin by key presence).
func TestFixupModelForRun_DefensiveBranches(t *testing.T) {
	ctx := context.Background()
	runID := uuid.New()

	t.Run("nil AuditRepo returns nil", func(t *testing.T) {
		s := New(Config{})
		if got := s.fixupModelForRun(ctx, runID); got != nil {
			t.Fatalf("got %+v, want nil with a nil AuditRepo", got)
		}
	})
	t.Run("malformed payload returns nil", func(t *testing.T) {
		au := newAuditFake()
		au.seeded = append(au.seeded, &audit.Entry{
			RunID: &runID, Category: CategoryStageFixupTriggered, Payload: []byte("{not json"),
		})
		s := New(Config{AuditRepo: au})
		if got := s.fixupModelForRun(ctx, runID); got != nil {
			t.Fatalf("got %+v, want nil on a malformed payload", got)
		}
	})
	t.Run("pre-#1164 entry (no fixup_model key) returns nil", func(t *testing.T) {
		au := newAuditFake()
		payload, _ := json.Marshal(map[string]any{"pass_ordinal": 1})
		au.seeded = append(au.seeded, &audit.Entry{
			RunID: &runID, Category: CategoryStageFixupTriggered, Payload: payload,
		})
		s := New(Config{AuditRepo: au})
		if got := s.fixupModelForRun(ctx, runID); got != nil {
			t.Fatalf("got %+v, want nil on a pre-#1164 entry with no fixup_model key", got)
		}
	})
	t.Run("present-but-empty pin surfaces verbatim", func(t *testing.T) {
		au := newAuditFake()
		payload, _ := json.Marshal(map[string]any{"fixup_model": "", "fixup_model_source": "", "pass_ordinal": 2})
		au.seeded = append(au.seeded, &audit.Entry{
			RunID: &runID, Category: CategoryStageFixupTriggered, Payload: payload,
		})
		s := New(Config{AuditRepo: au})
		got := s.fixupModelForRun(ctx, runID)
		if got == nil {
			t.Fatal("got nil, want a present-but-empty pin surfaced")
		}
		if got.Model != "" || got.Source != "" || got.PassOrdinal != 2 {
			t.Fatalf("got %+v, want {Model:\"\" Source:\"\" PassOrdinal:2}", *got)
		}
	})
}
