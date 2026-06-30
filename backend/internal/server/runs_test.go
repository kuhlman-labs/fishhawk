package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// modelPolicySpecYAML is delegationSpecYAML's shape plus a workflow-level
// operator_agent.model_policy block (#1421) so the spec→delegation→wire
// seam carries the scenario-A model-selection contract.
const modelPolicySpecYAML = `version: "0.5"
roles:
  tech_lead:
    members: ["@org/tech-leads"]
workflows:
  feature_change:
    operator_agent:
      may_approve: clean_dual_approval
      must_page_human: [reviewer_reject]
      model_policy:
        strategy: explicit_defaults
        defaults:
          plan: claude-opus-4-8
          implement: claude-sonnet-4-6
          review: gpt-5.5
        allowed:
          - claude-opus-4-8
          - claude-sonnet-4-6
          - gpt-5.5
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        reviewers:
          agent: 2
          human: 1
        produces:
          - artifact: plan
            schema: standard_v1
        gates:
          - type: approval
            approvers:
              any_of: [tech_lead]
      - id: implement
        type: implement
        executor:
          agent: claude-code
        produces:
          - artifact: pull_request
`

// TestGetRun_Delegation_ModelPolicy_SpecToWire is the cross-boundary seam
// test for #1421: a workflow spec declaring operator_agent.model_policy is
// parsed, evaluated through delegation.Evaluate, and serialized — GET
// /v0/runs/{id} echoes the strategy, per-stage defaults, and allowed set
// on the delegation block. The fakes and run helpers live in
// runs_get_test.go (same package).
func TestGetRun_Delegation_ModelPolicy_SpecToWire(t *testing.T) {
	s, repo, _, _ := newDelegationServer(t)
	runID, _ := startDriveE2ERun(t, s, repo, map[string]any{
		"repo": "x/y", "workflow_id": "feature_change", "workflow_sha": "abc",
		"trigger_source": "cli", "workflow_spec": modelPolicySpecYAML,
	})

	resp, raw := getRunResponse(t, s, runID)
	if resp.Delegation == nil {
		t.Fatal("delegation block missing")
	}
	mp := resp.Delegation.ModelPolicy
	if mp == nil {
		t.Fatal("delegation.model_policy missing; want the spec-declared policy echoed")
	}
	if mp.Strategy != "explicit_defaults" {
		t.Errorf("model_policy.strategy = %q, want explicit_defaults", mp.Strategy)
	}
	if mp.Defaults == nil {
		t.Fatal("model_policy.defaults missing")
	}
	if mp.Defaults.Plan != "claude-opus-4-8" || mp.Defaults.Implement != "claude-sonnet-4-6" || mp.Defaults.Review != "gpt-5.5" {
		t.Errorf("model_policy.defaults = %+v, want {plan:claude-opus-4-8 implement:claude-sonnet-4-6 review:gpt-5.5}", *mp.Defaults)
	}
	wantAllowed := []string{"claude-opus-4-8", "claude-sonnet-4-6", "gpt-5.5"}
	if len(mp.Allowed) != len(wantAllowed) {
		t.Fatalf("model_policy.allowed = %v, want %v", mp.Allowed, wantAllowed)
	}
	for i, want := range wantAllowed {
		if mp.Allowed[i] != want {
			t.Errorf("model_policy.allowed[%d] = %q, want %q", i, mp.Allowed[i], want)
		}
	}
	// The key is present on the raw wire body (not merely a zero value).
	deleg, ok := raw["delegation"].(map[string]any)
	if !ok {
		t.Fatalf("delegation block not an object in raw body: %v", raw["delegation"])
	}
	if _, present := deleg["model_policy"]; !present {
		t.Errorf("model_policy key absent from the raw delegation block: %v", deleg)
	}
}

// TestGetRun_Delegation_ModelPolicy_AbsentOmitted is the byte-identical
// control for #1421: an operator_agent block with NO model_policy yields a
// delegation block with the model_policy key omitted entirely.
func TestGetRun_Delegation_ModelPolicy_AbsentOmitted(t *testing.T) {
	s, repo, _, _ := newDelegationServer(t)
	runID, _ := startDriveE2ERun(t, s, repo, map[string]any{
		"repo": "x/y", "workflow_id": "feature_change", "workflow_sha": "abc",
		"trigger_source": "cli", "workflow_spec": delegationSpecYAML,
	})

	resp, raw := getRunResponse(t, s, runID)
	if resp.Delegation == nil {
		t.Fatal("delegation block missing")
	}
	if resp.Delegation.ModelPolicy != nil {
		t.Errorf("delegation.model_policy = %+v, want nil when the block declares none", resp.Delegation.ModelPolicy)
	}
	deleg, ok := raw["delegation"].(map[string]any)
	if !ok {
		t.Fatalf("delegation block not an object in raw body: %v", raw["delegation"])
	}
	if _, present := deleg["model_policy"]; present {
		t.Errorf("model_policy key present on a spec with no model_policy: %v", deleg)
	}
}

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

// TestCreateRunForTrigger_CreatesRunAndStages covers the run-creation core
// extracted from handleCreateRun (E25.5 / #1444): given already-resolved
// inputs it mints the run with the requested trigger source/ref and seeds one
// stage row per workflow stage definition. This is the seam the campaign
// driver reuses, so its behavior is asserted directly (not only via the HTTP
// handler).
func TestCreateRunForTrigger_CreatesRunAndStages(t *testing.T) {
	repo := newFakeRepo()
	s := newServer(t, repo)

	parsed, err := spec.ParseBytes([]byte(minimalSpecYAML))
	if err != nil {
		t.Fatalf("parse spec: %v", err)
	}
	wf := parsed.Workflows["trivial"]

	ref := "issue:1444"
	created, err := s.CreateRunForTrigger(context.Background(), CreateRunForTriggerParams{
		Repo:          "x/y",
		WorkflowID:    "trivial",
		WorkflowSHA:   "abc",
		TriggerSource: run.TriggerGitHubIssue,
		TriggerRef:    &ref,
		HaveStageDefs: true,
		WorkflowDef:   wf,
		WorkflowSpec:  []byte(minimalSpecYAML),
	})
	if err != nil {
		t.Fatalf("CreateRunForTrigger: %v", err)
	}
	if created.TriggerSource != run.TriggerGitHubIssue {
		t.Errorf("trigger source = %q, want github_issue", created.TriggerSource)
	}
	if created.TriggerRef == nil || *created.TriggerRef != ref {
		t.Errorf("trigger ref = %v, want %q", created.TriggerRef, ref)
	}
	if created.State != run.StatePending {
		t.Errorf("state = %q, want pending", created.State)
	}
	stages := repo.stagesFor(created.ID)
	if len(stages) != 1 || stages[0].Type != run.StageTypeImplement {
		t.Fatalf("stages = %+v, want one implement stage", stages)
	}
}

// TestCreateRunForTrigger_StageCreateError surfaces a "create stages failed"
// error so the HTTP handler's existing diagnostic contract is preserved after
// the extraction.
func TestCreateRunForTrigger_StageCreateError(t *testing.T) {
	repo := newFakeRepo()
	repo.createStageErr = errors.New("disk full")
	s := newServer(t, repo)

	parsed, err := spec.ParseBytes([]byte(minimalSpecYAML))
	if err != nil {
		t.Fatalf("parse spec: %v", err)
	}
	wf := parsed.Workflows["trivial"]

	_, err = s.CreateRunForTrigger(context.Background(), CreateRunForTriggerParams{
		Repo:          "x/y",
		WorkflowID:    "trivial",
		WorkflowSHA:   "abc",
		TriggerSource: run.TriggerCLI,
		HaveStageDefs: true,
		WorkflowDef:   wf,
	})
	if err == nil || !strings.Contains(err.Error(), "create stages failed") {
		t.Fatalf("err = %v, want it to contain 'create stages failed'", err)
	}
}

// TestCreateRun_UpstreamRunID_CrossBoundary pins the deploy-gate cross-run
// reference (E23.11 / #1417) end-to-end across the create path: request
// payload -> handleCreateRun -> CreateRunForTrigger -> run.CreateRunParams ->
// persisted run row -> toRunResponse echo (cf. #618). A POST carrying
// upstream_run_id must land in the CreateRun params, be stored on the run,
// and be echoed on the response.
func TestCreateRun_UpstreamRunID_CrossBoundary(t *testing.T) {
	repo := newFakeRepo()
	s := newServer(t, repo)

	upstreamID := uuid.New()
	body := `{
		"repo": "kuhlman-labs/fishhawk",
		"workflow_id": "release",
		"workflow_sha": "abc123",
		"trigger_source": "cli",
		"upstream_run_id": "` + upstreamID.String() + `"
	}`
	req := httptest.NewRequest(http.MethodPost, "/v0/runs", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.handleCreateRun(w, withAuth(req))

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
	}
	// (1) It reached the repo create params.
	if got := repo.lastCreateRunParams.UpstreamRunID; got == nil || *got != upstreamID {
		t.Errorf("CreateRunParams.UpstreamRunID = %v, want %v", got, upstreamID)
	}
	// (2) It is echoed on the response (which the fake stores on the run row).
	var resp runResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if resp.UpstreamRunID == nil || *resp.UpstreamRunID != upstreamID {
		t.Errorf("response UpstreamRunID = %v, want %v", resp.UpstreamRunID, upstreamID)
	}
}

// TestCreateRun_UpstreamRunID_OmittedStaysNil confirms the appended-deploy /
// non-deploy default: a create request omitting upstream_run_id leaves the
// params nil and the response field absent (#1417).
func TestCreateRun_UpstreamRunID_OmittedStaysNil(t *testing.T) {
	repo := newFakeRepo()
	s := newServer(t, repo)

	body := `{
		"repo": "kuhlman-labs/fishhawk",
		"workflow_id": "feature_change",
		"workflow_sha": "abc123",
		"trigger_source": "cli"
	}`
	req := httptest.NewRequest(http.MethodPost, "/v0/runs", strings.NewReader(body))
	w := httptest.NewRecorder()
	s.handleCreateRun(w, withAuth(req))

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
	}
	if got := repo.lastCreateRunParams.UpstreamRunID; got != nil {
		t.Errorf("CreateRunParams.UpstreamRunID = %v, want nil", got)
	}
	var resp runResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if resp.UpstreamRunID != nil {
		t.Errorf("response UpstreamRunID = %v, want nil", resp.UpstreamRunID)
	}
}
