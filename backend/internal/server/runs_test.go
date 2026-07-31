package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
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

// declaredAdvisoryV2SpecYAML is a v2 spec whose plan stage DECLARES
// reviewers.authority: advisory over agents+human:1 (E53.2 / #2225). Advisory
// does not engage the gating run-creation reviewer-availability gate, so the
// run is created with no PlanReviewer wired. Only the plan stage declares a
// reviewers block, so review_authority carries exactly one entry.
const declaredAdvisoryV2SpecYAML = `version: "2"
workflows:
  feature_change:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        reviewers:
          authority: advisory
          agents:
            - provider: claudecode
          human: 1
        produces:
          - artifact: plan
            schema: standard_v1
      - id: implement
        type: implement
        executor:
          agent: claude-code
        produces:
          - artifact: pull_request
`

// declaredPermissionsV2SpecYAML is a plan+implement v2 workflow whose implement
// stage declares a full permissions block (E53.5 / #2228): network (normalized
// into Egress), write globs, and a shell posture.
const declaredPermissionsV2SpecYAML = `version: "2"
workflows:
  feature_change:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
      - id: implement
        type: implement
        executor:
          agent: claude-code
        permissions:
          network:
            target_hosts: ["api.internal.example.com:8443"]
          write: ["src/**/*.go"]
          shell: restricted
        produces:
          - artifact: pull_request
`

// TestCreateRun_EmitsStagePermissionsDeclared is the audit half of E53.5 /
// #2228's done-means (verification 6): run creation appends EXACTLY ONE
// stage_permissions_declared entry whose payload carries the feature-level
// enforced:false and enforcement_tracked_by:"#2133". A comment-only or partial
// edit fails this where a touched-the-file presence gate would pass (#1169).
func TestCreateRun_EmitsStagePermissionsDeclared(t *testing.T) {
	s, repo, au, _ := newDelegationServer(t)
	startDriveE2ERun(t, s, repo, map[string]any{
		"repo": "x/y", "workflow_id": "feature_change", "workflow_sha": "abc",
		"trigger_source": "cli", "workflow_spec": declaredPermissionsV2SpecYAML,
	})

	var found []audit.ChainAppendParams
	for _, e := range au.appended {
		if e.Category == "stage_permissions_declared" {
			found = append(found, e)
		}
	}
	if len(found) != 1 {
		t.Fatalf("stage_permissions_declared entries = %d, want exactly 1", len(found))
	}
	var payload stagePermissionsAuditPayload
	if err := json.Unmarshal(found[0].Payload, &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if payload.Enforced {
		t.Error("feature-level enforced = true, want false (the permissions block is declaration-only)")
	}
	if payload.EnforcementTrackedBy != "#2133" {
		t.Errorf("enforcement_tracked_by = %q, want #2133", payload.EnforcementTrackedBy)
	}
	if len(payload.Stages) != 1 || payload.Stages[0].StageID != "implement" {
		t.Errorf("payload stages = %+v, want one implement entry", payload.Stages)
	}
}

// TestCreateRun_NoPermissions_NoAuditEntry is the control (verification 6 / 10):
// a workflow declaring no permissions/egress appends NO stage_permissions_declared
// entry, keeping its audit stream byte-identical.
func TestCreateRun_NoPermissions_NoAuditEntry(t *testing.T) {
	s, repo, au, _ := newDelegationServer(t)
	startDriveE2ERun(t, s, repo, map[string]any{
		"repo": "x/y", "workflow_id": "feature_change", "workflow_sha": "abc",
		"trigger_source": "cli", "workflow_spec": declaredAdvisoryV2SpecYAML,
	})
	for _, e := range au.appended {
		if e.Category == "stage_permissions_declared" {
			t.Fatal("unexpected stage_permissions_declared entry for a spec declaring no permissions/egress")
		}
	}
}

// TestBuildStagePermissionsPayloads_EnforcedOnlyForAgentAcceptance is the
// enforcement-honesty pin (fix-up high/security + operator condition 3): the
// per-entry `enforced` flag is true ONLY for an acceptance stage whose executor
// is the AGENT branch — the one declaration the runner's default-deny egress
// proxy actually enforces by constraining that agent's egress. A non-agent (human/delegate)
// acceptance stage runs no agent through the proxy, so enforced:true there would
// falsely claim a network allow-list is enforced when no agent proxy is
// involved. Implement (non-acceptance) egress is never enforced today either.
func TestBuildStagePermissionsPayloads_EnforcedOnlyForAgentAcceptance(t *testing.T) {
	hosts := &spec.StageEgress{TargetHosts: []string{"staging.example.com:8443"}}
	wf := spec.Workflow{Stages: []spec.Stage{
		{ID: "accept-agent", Type: spec.StageTypeAcceptance, Executor: spec.Executor{Agent: "claude-code"}, Egress: hosts},
		{ID: "accept-human", Type: spec.StageTypeAcceptance, Executor: spec.Executor{Human: true}, Egress: hosts},
		{ID: "impl-agent", Type: spec.StageTypeImplement, Executor: spec.Executor{Agent: "claude-code"}, Egress: hosts},
	}}
	got := map[string]runStagePermissionsPayload{}
	for _, e := range buildStagePermissionsPayloads(wf) {
		got[e.StageID] = e
	}
	if !got["accept-agent"].Enforced {
		t.Error("agent-executor acceptance egress: enforced = false, want true (the proxy constrains the agent's egress)")
	}
	if !strings.Contains(got["accept-agent"].Note, "enforced today") {
		t.Errorf("agent-executor acceptance note = %q, want the enforced-today note", got["accept-agent"].Note)
	}
	if got["accept-human"].Enforced {
		t.Error("human-executor acceptance egress: enforced = true, want false — no agent runs through the proxy")
	}
	if got["impl-agent"].Enforced {
		t.Error("implement-stage egress: enforced = true, want false (only an acceptance agent's egress is proxy-constrained)")
	}
	// The non-enforced entries carry the declaration-only note either way.
	for _, id := range []string{"accept-human", "impl-agent"} {
		if !strings.Contains(got[id].Note, "#2133") {
			t.Errorf("%s note = %q, want it to name the enforcement tracker #2133", id, got[id].Note)
		}
	}
}

// TestEmitStagePermissionsDeclared_AppendFailsDoesNotFailRun is the best-effort
// degrade pin (fix-up low/untested-path): when the once-per-run audit append
// fails, run creation still succeeds (the legibility surface must never fail run
// creation) and no stage_permissions_declared entry is recorded.
func TestEmitStagePermissionsDeclared_AppendFailsDoesNotFailRun(t *testing.T) {
	s, repo, au, _ := newDelegationServer(t)
	au.appendErrCategory = "stage_permissions_declared"
	// Run creation must not fail on the injected audit-append error.
	startDriveE2ERun(t, s, repo, map[string]any{
		"repo": "x/y", "workflow_id": "feature_change", "workflow_sha": "abc",
		"trigger_source": "cli", "workflow_spec": declaredPermissionsV2SpecYAML,
	})
	for _, e := range au.appended {
		if e.Category == "stage_permissions_declared" {
			t.Fatal("stage_permissions_declared entry recorded despite the injected append error")
		}
	}
}

// TestBuildStagePermissionsSurface_OmitsOnBadSpec pins the two run-status
// omission branches buildStagePermissionsSurface adds (fix-up low/verification):
// a run whose cached spec fails to parse, and a run whose WorkflowID is absent
// from an otherwise-valid cached spec. Both are externally observable
// run-status paths, and both must yield a nil (omitted) permissions surface
// rather than a partial read or a panic.
func TestBuildStagePermissionsSurface_OmitsOnBadSpec(t *testing.T) {
	s, _, _, _ := newDelegationServer(t)

	t.Run("malformed_cached_spec", func(t *testing.T) {
		got := s.buildStagePermissionsSurface(&run.Run{
			ID:           uuid.New(),
			WorkflowID:   "feature_change",
			WorkflowSpec: []byte("this is: not valid: workflow yaml: ["),
		})
		if got != nil {
			t.Errorf("surface = %+v, want nil (unparseable cached spec → omitted)", got)
		}
	})

	t.Run("workflow_not_in_spec", func(t *testing.T) {
		got := s.buildStagePermissionsSurface(&run.Run{
			ID:           uuid.New(),
			WorkflowID:   "no_such_workflow",
			WorkflowSpec: []byte(declaredPermissionsV2SpecYAML),
		})
		if got != nil {
			t.Errorf("surface = %+v, want nil (workflow absent from cached spec → omitted)", got)
		}
	})
}

// reviewAuthorityFor returns the review_authority entry for the named stage.
func reviewAuthorityFor(t *testing.T, resp runResponse, stage string) runReviewAuthorityPayload {
	t.Helper()
	for _, e := range resp.ReviewAuthority {
		if e.Stage == stage {
			return e
		}
	}
	t.Fatalf("no review_authority entry for stage %q in %+v", stage, resp.ReviewAuthority)
	return runReviewAuthorityPayload{}
}

// TestGetRun_ReviewAuthority_Declared is the wire seam for E53.2 (#2225): a
// run whose cached v2 spec DECLARES reviewers.authority surfaces that mode on
// GET /v0/runs/{id} with source "declared" — the operator reads the mode
// instead of re-deriving it.
func TestGetRun_ReviewAuthority_Declared(t *testing.T) {
	s, repo, _, _ := newDelegationServer(t)
	runID, _ := startDriveE2ERun(t, s, repo, map[string]any{
		"repo": "x/y", "workflow_id": "feature_change", "workflow_sha": "abc",
		"trigger_source": "cli", "workflow_spec": declaredAdvisoryV2SpecYAML,
	})

	resp, raw := getRunResponse(t, s, runID)
	got := reviewAuthorityFor(t, resp, "plan")
	if got.Authority != "advisory" || got.Source != "declared" {
		t.Errorf("plan review_authority = %+v, want {authority:advisory source:declared}", got)
	}
	if got.StageType != "plan" {
		t.Errorf("plan review_authority stage_type = %q, want plan", got.StageType)
	}
	// Only the plan stage declares reviewers, so exactly one entry is present.
	if len(resp.ReviewAuthority) != 1 {
		t.Errorf("review_authority = %+v, want exactly the plan entry", resp.ReviewAuthority)
	}
	if _, present := raw["review_authority"]; !present {
		t.Errorf("review_authority key absent from the raw body: %v", raw)
	}
}

// TestGetRun_ReviewAuthority_Derived is the derived-provenance mirror: a spec
// that declares reviewers but NOT authority surfaces the count-derived mode
// with source "derived" (ruling 1 — the response is emitted for derived stages
// too, so an operator never re-derives).
func TestGetRun_ReviewAuthority_Derived(t *testing.T) {
	s, repo, _, _ := newDelegationServer(t)
	// Same spec minus the explicit authority line — the counts (agents+human:1)
	// derive to advisory.
	derived := strings.Replace(declaredAdvisoryV2SpecYAML, "          authority: advisory\n", "", 1)
	runID, _ := startDriveE2ERun(t, s, repo, map[string]any{
		"repo": "x/y", "workflow_id": "feature_change", "workflow_sha": "abc",
		"trigger_source": "cli", "workflow_spec": derived,
	})

	resp, _ := getRunResponse(t, s, runID)
	got := reviewAuthorityFor(t, resp, "plan")
	if got.Authority != "advisory" || got.Source != "derived" {
		t.Errorf("plan review_authority = %+v, want {authority:advisory source:derived}", got)
	}
}

// TestGetRun_ReviewAuthority_OmittedNoReviewers is condition 1's byte-identical
// scope: a run whose spec declares NO reviewers block anywhere omits the
// review_authority field entirely (nil + key absent), keeping the response
// byte-identical to a pre-#2225 response.
func TestGetRun_ReviewAuthority_OmittedNoReviewers(t *testing.T) {
	s, repo, _, _ := newDelegationServer(t)
	// A minimal v2 spec: plan + implement, no reviewers block on either.
	const noReviewersSpec = `version: "2"
workflows:
  feature_change:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
      - id: implement
        type: implement
        executor:
          agent: claude-code
        produces:
          - artifact: pull_request
`
	runID, _ := startDriveE2ERun(t, s, repo, map[string]any{
		"repo": "x/y", "workflow_id": "feature_change", "workflow_sha": "abc",
		"trigger_source": "cli", "workflow_spec": noReviewersSpec,
	})

	resp, raw := getRunResponse(t, s, runID)
	if resp.ReviewAuthority != nil {
		t.Errorf("review_authority = %+v, want nil when no stage declares reviewers", resp.ReviewAuthority)
	}
	if _, present := raw["review_authority"]; present {
		t.Errorf("review_authority key present on a spec with no reviewers block: %v", raw)
	}
}

// TestShipPlan_DeclaredAuthority_FlipsReviewLoopBlocking is the cross-boundary
// proof for E53.2 (#2225) at the spec -> resolver -> review-loop seam: a
// DECLARED reviewers.authority flips the plan-review loop's blocking decision
// AWAY from the count-derived default. Half A: authority: gating with human: 1
// (which the count rule would read as ADVISORY) BLOCKS on an agent reject —
// the stage is transitioned to failed-B and the plan_reviewed payload carries
// gating authority. Half B: authority: advisory with human: 0 (which the count
// rule would read as GATING) does NOT block — the reject is recorded but the
// stage is never failed. This uses the shared package-server ship-plan harness
// (newPlanServerWithReviewer / shipPlanRequest, defined in plan_test.go).
func TestShipPlan_DeclaredAuthority_FlipsReviewLoopBlocking(t *testing.T) {
	// Declared gating over agents+human:1 — count-derived would be advisory.
	const declaredGatingV2 = `version: "2"
workflows:
  feature_change:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        reviewers:
          authority: gating
          agents:
            - provider: claudecode
          human: 1
        produces:
          - artifact: plan
            schema: standard_v1
`
	// Declared advisory over agents+human:0 — count-derived would be gating.
	const declaredAdvisoryV2 = `version: "2"
workflows:
  feature_change:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        reviewers:
          authority: advisory
          agents:
            - provider: claudecode
          human: 0
        produces:
          - artifact: plan
            schema: standard_v1
`

	planReviewedAuthority := func(t *testing.T, au *auditFake) planreview.AuthorityMode {
		t.Helper()
		for _, e := range au.appended {
			if e.Category != "plan_reviewed" {
				continue
			}
			var p planreview.PlanReviewedPayload
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				t.Fatalf("decode plan_reviewed: %v", err)
			}
			return p.Authority
		}
		t.Fatal("no plan_reviewed audit entry")
		return ""
	}
	stageFailed := func(rr *promptRunRepo, stageID uuid.UUID) bool {
		for _, call := range rr.transitionStageCalls {
			if call.StageID == stageID && call.To == run.StageStateFailed {
				return true
			}
		}
		return false
	}

	t.Run("declared gating blocks a reject despite human:1", func(t *testing.T) {
		runID, stageID := uuid.New(), uuid.New()
		reviewer := &fakePlanReviewer{
			verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictReject, FreeForm: "incomplete"},
			model:   "claude-sonnet-4-6",
		}
		s, sf, _, au, rr := newPlanServerWithReviewer(t, runID, stageID, reviewer, []byte(declaredGatingV2))
		priv, _ := sf.issue(t, runID)
		w := shipPlanRequest(t, s, runID, stageID, priv, validPlanBytes(t), "")
		if w.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
		}
		if got := planReviewedAuthority(t, au); got != planreview.AuthorityGating {
			t.Errorf("authority = %q, want gating (declared wins over the advisory count rule)", got)
		}
		if !stageFailed(rr, stageID) {
			t.Errorf("declared gating: stage was not blocked (failed) on reject; transitions = %+v", rr.transitionStageCalls)
		}
	})

	t.Run("declared advisory does not block a reject despite human:0", func(t *testing.T) {
		runID, stageID := uuid.New(), uuid.New()
		reviewer := &fakePlanReviewer{
			verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictReject, FreeForm: "incomplete"},
			model:   "claude-sonnet-4-6",
		}
		s, sf, _, au, rr := newPlanServerWithReviewer(t, runID, stageID, reviewer, []byte(declaredAdvisoryV2))
		priv, _ := sf.issue(t, runID)
		w := shipPlanRequest(t, s, runID, stageID, priv, validPlanBytes(t), "")
		if w.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
		}
		// Advisory review runs detached; drain it before asserting.
		s.waitBackgroundReviews()
		if got := planReviewedAuthority(t, au); got != planreview.AuthorityAdvisory {
			t.Errorf("authority = %q, want advisory (declared wins over the gating count rule)", got)
		}
		if stageFailed(rr, stageID) {
			t.Errorf("declared advisory: stage must NOT be blocked (failed) on reject; transitions = %+v", rr.transitionStageCalls)
		}
	})
}

// TestBuildReviewAuthorityPayload_Degrades exercises the fail-closed branches
// of buildReviewAuthorityPayload directly (E53.2 / #2225): a run with no cached
// spec, an unparseable cached spec, and a spec missing the run's workflow each
// degrade to nil (field omitted) rather than failing the read.
func TestBuildReviewAuthorityPayload_Degrades(t *testing.T) {
	s := newServer(t, newFakeRepo())
	cases := []struct {
		name string
		run  *run.Run
	}{
		{"no cached spec", &run.Run{ID: uuid.New(), WorkflowID: "feature_change"}},
		{"unparseable spec", &run.Run{ID: uuid.New(), WorkflowID: "feature_change", WorkflowSpec: []byte("::: not yaml :::")}},
		{"workflow missing from spec", &run.Run{ID: uuid.New(), WorkflowID: "absent", WorkflowSpec: []byte(declaredAdvisoryV2SpecYAML)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := s.buildReviewAuthorityPayload(tc.run); got != nil {
				t.Errorf("buildReviewAuthorityPayload = %+v, want nil (degrade)", got)
			}
		})
	}
}

// declaredGatingV2SpecYAML is a v2 plan stage whose reviewers DECLARE
// authority: gating over agents+human:1 (E53.2 / #2225). The count-derived rule
// would read agents+human:1 as ADVISORY, so the declaration is what engages the
// run-creation reviewer-availability gate.
const declaredGatingV2SpecYAML = `version: "2"
workflows:
  feature_change:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        reviewers:
          authority: gating
          agents:
            - provider: claudecode
          human: 1
        produces:
          - artifact: plan
            schema: standard_v1
      - id: implement
        type: implement
        executor:
          agent: claude-code
        produces:
          - artifact: pull_request
`

// TestCreateRun_DeclaredGating_NilReviewer_Rejected is condition 2 (E53.2 /
// #2225): declaring authority: gating newly engages the run-creation
// reviewer-availability hard-fail at runs.go:739, so a declared-gating plan
// stage on a deployment with NO reviewer backend wired is rejected with 400 +
// plan_reviewer_unconfigured, while the SAME stage WITHOUT the declaration
// (agents+human:1, derived advisory) is still created. This proves the
// behaviour change the plan flagged in risk prose is real and correct.
func TestCreateRun_DeclaredGating_NilReviewer_Rejected(t *testing.T) {
	postCreate := func(t *testing.T, spec string) (*httptest.ResponseRecorder, *fakeRepo, *auditFake) {
		t.Helper()
		repo := newFakeRepo()
		au := newAuditFake()
		s := New(Config{Addr: "127.0.0.1:0", RunRepo: repo, AuditRepo: au})
		body, _ := json.Marshal(map[string]any{
			"repo": "x/y", "workflow_id": "feature_change", "workflow_sha": "abc",
			"trigger_source": "cli", "workflow_spec": spec,
		})
		req := httptest.NewRequest(http.MethodPost, "/v0/runs", strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		s.handleCreateRun(w, withAuth(req))
		return w, repo, au
	}

	// (a) Declared gating is rejected — no PlanReviewer wired.
	w, repo, au := postCreate(t, declaredGatingV2SpecYAML)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("declared-gating status = %d, want 400:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"plan_reviewer_unconfigured"`) {
		t.Errorf("body missing plan_reviewer_unconfigured code: %s", w.Body.String())
	}
	if len(repo.runs) != 0 {
		t.Errorf("expected zero runs created for the rejected declared-gating spec, got %d", len(repo.runs))
	}
	// The operational audit trail must record the rejection: a
	// run_rejected_misconfigured global entry whose payload carries the
	// plan_reviewer_unconfigured reason. Asserting only the HTTP code would let a
	// regression that dropped this audit (losing the operator-visible record of
	// why the run was refused) still pass.
	var foundReject bool
	for _, e := range au.globalAppended {
		if e.Category != "run_rejected_misconfigured" {
			continue
		}
		foundReject = true
		var p map[string]any
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("run_rejected_misconfigured payload not JSON: %v", err)
		}
		if p["reason"] != "plan_reviewer_unconfigured" {
			t.Errorf("run_rejected_misconfigured reason = %v, want plan_reviewer_unconfigured", p["reason"])
		}
		if p["stage"] != "plan" {
			t.Errorf("run_rejected_misconfigured stage = %v, want plan", p["stage"])
		}
	}
	if !foundReject {
		t.Errorf("expected a run_rejected_misconfigured audit entry, got %+v", au.globalAppended)
	}

	// (b) The SAME stage WITHOUT the declaration derives to advisory and is
	// created — the reviewer-availability gate only fires for gating.
	derived := strings.Replace(declaredGatingV2SpecYAML, "          authority: gating\n", "", 1)
	w2, _, au2 := postCreate(t, derived)
	if w2.Code != http.StatusCreated {
		t.Fatalf("un-declared (derived advisory) status = %d, want 201:\n%s", w2.Code, w2.Body.String())
	}
	for _, e := range au2.globalAppended {
		if e.Category == "run_rejected_misconfigured" {
			t.Errorf("derived-advisory run must not emit run_rejected_misconfigured")
		}
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

// campaignHydrationGitHub builds an httptest GitHub server serving the four
// endpoints StartRunForCampaignIssue's #1721 hydration path exercises:
// installation + workflow-spec (to reach CreateRunForTrigger) and issue +
// comments (the IssueContext hydration). issueStatus/commentsStatus let a test
// force a fetch failure to prove the best-effort degradation.
func campaignHydrationGitHub(t *testing.T, issueStatus, commentsStatus int) *githubclient.Client {
	t.Helper()
	specJSON := `{"path":".fishhawk/workflows.yaml","sha":"spec_sha","content":"` +
		base64.StdEncoding.EncodeToString([]byte(gatedSpecYAML)) + `","encoding":"base64","type":"file"}`
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{repo}/installation", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"id":12345,"app_id":1}`)
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/contents/", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, specJSON)
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/issues/{number}/comments", func(w http.ResponseWriter, _ *http.Request) {
		if commentsStatus != http.StatusOK {
			w.WriteHeader(commentsStatus)
			return
		}
		_, _ = io.WriteString(w, `[{"user":{"login":"octocat"},"body":"first comment","created_at":"2026-07-01T00:00:00Z"}]`)
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/issues/{number}", func(w http.ResponseWriter, _ *http.Request) {
		if issueStatus != http.StatusOK {
			w.WriteHeader(issueStatus)
			return
		}
		_, _ = io.WriteString(w, `{"number":100,"title":"Campaign parent issue","body":"parent body","state":"open"}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &githubclient.Client{
		BaseURL: srv.URL,
		Tokens:  &ghTokensStub{tok: "ghs_test"},
		HTTP:    &http.Client{Timeout: 5 * time.Second},
		AppJWT:  func() (string, error) { return "gha_app_jwt", nil },
	}
}

// TestStartRunForCampaignIssue_HydratesIssueContext pins leg 3 of #1721: a
// campaign-minted run carries the fetched issue title/body/url/number (+ mapped
// comments) so a decomposed parent's fan-out children inherit real context.
func TestStartRunForCampaignIssue_HydratesIssueContext(t *testing.T) {
	rrepo := newFakeRepo()
	gh := campaignHydrationGitHub(t, http.StatusOK, http.StatusOK)
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rrepo, GitHub: gh})

	if _, err := s.StartRunForCampaignIssue(context.Background(),
		"kuhlman-labs/fishhawk", "issue:100", "feature_change", "", "local"); err != nil {
		t.Fatalf("StartRunForCampaignIssue: %v", err)
	}

	ic := rrepo.lastCreateRunParams.IssueContext
	if ic == nil {
		t.Fatal("created run row carries no IssueContext; want hydrated context")
	}
	if ic.Title != "Campaign parent issue" || ic.Body != "parent body" || ic.Number != 100 {
		t.Errorf("IssueContext = {Title:%q Body:%q Number:%d}, want the fetched issue", ic.Title, ic.Body, ic.Number)
	}
	if ic.URL != "https://github.com/kuhlman-labs/fishhawk/issues/100" {
		t.Errorf("IssueContext.URL = %q, want the canonical github.com issue URL", ic.URL)
	}
	if len(ic.Comments) != 1 || ic.Comments[0].Author != "octocat" || ic.Comments[0].Body != "first comment" {
		t.Errorf("IssueContext.Comments = %+v, want the single fetched comment", ic.Comments)
	}
}

// TestStartRunForCampaignIssue_HydrationDegradesOnFetchError pins the best-effort
// posture: a GetIssue failure logs a warn and starts the run with a nil
// IssueContext rather than blocking the campaign run start (#1721).
func TestStartRunForCampaignIssue_HydrationDegradesOnFetchError(t *testing.T) {
	rrepo := newFakeRepo()
	gh := campaignHydrationGitHub(t, http.StatusInternalServerError, http.StatusOK)
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rrepo, GitHub: gh})

	created, err := s.StartRunForCampaignIssue(context.Background(),
		"kuhlman-labs/fishhawk", "issue:100", "feature_change", "", "local")
	if err != nil {
		t.Fatalf("StartRunForCampaignIssue must still start the run on a GetIssue error: %v", err)
	}
	if created == nil {
		t.Fatal("created run is nil; want a started run despite the hydration failure")
	}
	if rrepo.lastCreateRunParams.IssueContext != nil {
		t.Errorf("IssueContext = %+v, want nil after the GetIssue failure", rrepo.lastCreateRunParams.IssueContext)
	}
}

// TestStartRunForCampaignIssue_HydrationDegradesOnCommentError pins the partial-
// degradation branch: a comment-list fetch failure keeps title+body and leaves
// the comments slice nil rather than discarding the whole context (#1721).
func TestStartRunForCampaignIssue_HydrationDegradesOnCommentError(t *testing.T) {
	rrepo := newFakeRepo()
	gh := campaignHydrationGitHub(t, http.StatusOK, http.StatusInternalServerError)
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rrepo, GitHub: gh})

	if _, err := s.StartRunForCampaignIssue(context.Background(),
		"kuhlman-labs/fishhawk", "issue:100", "feature_change", "", "local"); err != nil {
		t.Fatalf("StartRunForCampaignIssue: %v", err)
	}
	ic := rrepo.lastCreateRunParams.IssueContext
	if ic == nil {
		t.Fatal("IssueContext nil; want title+body retained despite the comment-fetch failure")
	}
	if ic.Title != "Campaign parent issue" || ic.Body != "parent body" {
		t.Errorf("IssueContext title/body = {%q,%q}, want retained", ic.Title, ic.Body)
	}
	if len(ic.Comments) != 0 {
		t.Errorf("IssueContext.Comments = %+v, want empty after the comment-fetch failure", ic.Comments)
	}
}

// TestEmitReviewerCapabilityUnavailable_StampsIdentityAccount pins the
// run-create audit emissions' account source (ADR-057 / #1828): the entry
// carries the creating caller Identity's account so the degradation lands on
// that tenant's run-less chain partition.
func TestEmitReviewerCapabilityUnavailable_StampsIdentityAccount(t *testing.T) {
	acct := uuid.New()
	rec := &campaignAuditRecorder{}
	s := New(Config{AuditRepo: rec})

	ctx := context.WithValue(context.Background(), ctxKeyIdentity,
		Identity{Subject: "github:alice", AccountID: acct.String()})
	s.emitReviewerCapabilityUnavailable(ctx, "acme/widgets", "feature_change", "plan", 1,
		unavailableReviewer{provider: "anthropic", optional: true, err: errors.New("no api key")})

	if len(rec.entries) != 1 {
		t.Fatalf("audit entries = %d, want 1", len(rec.entries))
	}
	got := rec.entries[0]
	if got.Category != "reviewer_capability_unavailable" {
		t.Fatalf("category = %q, want reviewer_capability_unavailable", got.Category)
	}
	if got.AccountID == nil || *got.AccountID != acct {
		t.Fatalf("AccountID = %v, want %s", got.AccountID, acct)
	}
}

// --- W3: issue_context.labels survives the create → persist → read round-trip ---
//
// The seam test the operator named: labels are fetched by a client, shipped
// inline on POST /v0/runs, marshalled into the runs.issue_context JSONB
// snapshot, and read back by BOTH applies_to evaluation points. A field
// dropped anywhere along that path leaves every labels-declaring workflow
// rejecting every legitimate run, while per-layer unit tests all pass.

// appliesToLabelsSpecYAML is a v2 spec whose workflow declares no applies_to,
// so this test isolates the PLUMBING from the gate: labels must round-trip
// whether or not anything evaluates them.
const appliesToLabelsSpecYAML = `version: "2"
workflows:
  feature_change:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
      - id: implement
        type: implement
        executor:
          agent: claude-code
        produces:
          - artifact: pull_request
`

func TestCreateRun_W3_IssueContextLabels_RoundTripThroughPersistedRun(t *testing.T) {
	s, _, _, _ := newDelegationServer(t)
	w := createRunViaHandler(t, s, map[string]any{
		"repo": "x/y", "workflow_id": "feature_change", "workflow_sha": "abc",
		"trigger_source": "github_issue", "workflow_spec": appliesToLabelsSpecYAML,
		"issue_context": map[string]any{
			"title": "t", "body": "b", "url": "https://example.invalid/i/7", "number": 7,
			"labels": []string{"dependencies", "area:backend"},
		},
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
	}
	var created runResponse
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}

	// Read the run back through GET /v0/runs/{id} — the PERSISTED row, not
	// the create response — so the assertion covers the store round-trip the
	// two evaluation points depend on.
	resp, raw := getRunResponse(t, s, created.ID)
	if resp.IssueContext == nil {
		t.Fatal("issue_context missing from the persisted run")
	}
	if got := strings.Join(resp.IssueContext.Labels, ","); got != "dependencies,area:backend" {
		t.Errorf("issue_context.labels = %q, want the two posted labels in order", got)
	}
	// And on the raw wire, under exactly the key both clients marshal.
	ic, _ := raw["issue_context"].(map[string]any)
	if ic == nil || ic["labels"] == nil {
		t.Errorf("raw issue_context = %+v, want a `labels` key — a renamed tag silently drops the field", ic)
	}
}

// TestCreateRun_W3_LegacyIssueContext_LoadsAsNilLabels is the back-compat
// half: a pre-#2226 payload carrying NO `labels` key must still load. This is
// what makes the claim "additive, no migration" checkable rather than
// asserted — the same shape #618 relied on for `comments`.
func TestCreateRun_W3_LegacyIssueContext_LoadsAsNilLabels(t *testing.T) {
	s, _, _, _ := newDelegationServer(t)
	w := createRunViaHandler(t, s, map[string]any{
		"repo": "x/y", "workflow_id": "feature_change", "workflow_sha": "abc",
		"trigger_source": "github_issue", "workflow_spec": appliesToLabelsSpecYAML,
		"issue_context": map[string]any{
			"title": "t", "body": "b", "url": "https://example.invalid/i/7", "number": 7,
		},
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 — a labels-less legacy payload must still create a run:\n%s", w.Code, w.Body.String())
	}
	var created runResponse
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	resp, raw := getRunResponse(t, s, created.ID)
	if resp.IssueContext == nil {
		t.Fatal("issue_context missing")
	}
	if resp.IssueContext.Labels != nil {
		t.Errorf("labels = %+v, want nil for a payload that never carried the key", resp.IssueContext.Labels)
	}
	if ic, _ := raw["issue_context"].(map[string]any); ic != nil {
		if _, present := ic["labels"]; present {
			t.Error("an empty label set serialized a `labels` key; omitempty keeps legacy payloads byte-identical")
		}
	}
}
