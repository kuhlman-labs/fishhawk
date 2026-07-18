package runnerbackend

import (
	"context"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	forgegitlab "github.com/kuhlman-labs/fishhawk/backend/internal/forge/gitlab"
	"github.com/kuhlman-labs/fishhawk/backend/internal/gitlabclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/onboarding"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// --- fakes ---

type pipelineCall struct {
	scope forge.CredentialScope
	ref   string
	vars  []gitlabclient.PipelineVariable
}

// fakePipelineTrigger records each TriggerPipeline call so tests assert the ref
// + variables the GitLabCI backend forwards.
type fakePipelineTrigger struct {
	calls []pipelineCall
	err   error
}

func (f *fakePipelineTrigger) TriggerPipeline(_ context.Context, scope forge.CredentialScope,
	ref string, vars []gitlabclient.PipelineVariable) error {
	if f.err != nil {
		return f.err
	}
	f.calls = append(f.calls, pipelineCall{scope: scope, ref: ref, vars: vars})
	return nil
}

var _ PipelineTrigger = (*fakePipelineTrigger)(nil)

func gitlabScope(id string) forge.CredentialScope { return forge.FromRef("gitlab:" + id) }

// GitLabPipelineTrigger returns nil when no GitLab forge is registered (the
// unconfigured / dormant default posture): forge.Get("gitlab") errors, so the
// resolution fails closed to nil rather than a nil-forge dispatch. The
// runnerbackend test binary never registers a gitlab forge, so this is the
// live-through-forge.Get assertion of the nil-safe path.
func TestGitLabPipelineTrigger_UnconfiguredReturnsNil(t *testing.T) {
	if got := GitLabPipelineTrigger(); got != nil {
		t.Errorf("GitLabPipelineTrigger() = %v, want nil when GitLab is unregistered", got)
	}
}

// --- Kind / HostDispatched ---

func TestGitLabCI_KindAndHostDispatched(t *testing.T) {
	g := &GitLabCI{}
	if g.Kind() != run.RunnerKindGitLabCI {
		t.Errorf("Kind() = %q, want %q", g.Kind(), run.RunnerKindGitLabCI)
	}
	if g.HostDispatched() {
		t.Error("HostDispatched() = true, want false (fishhawkd fires the pipeline trigger)")
	}
}

// --- TriggerStage: ref threading (the seam the dispatch design turns on) ---

// TestGitLabCI_TriggerStage_ForwardsRef asserts the backend creates the pipeline
// against the EXACT p.Ref it is handed, for both a top-level run's branch and a
// decomposed child's slice branch, and that the run-provenance CI/CD variables
// carry through in order.
func TestGitLabCI_TriggerStage_ForwardsRef(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	parent := uuid.New()

	cases := []struct {
		name          string
		p             TriggerParams
		wantRef       string
		wantParentVar bool
		wantParentRef string
	}{
		{
			name: "top-level run branch",
			p: TriggerParams{
				RunID: runID, StageID: stageID, WorkflowID: "feature_change",
				StageExecutorRef: "claude-code", Scope: gitlabScope("77"),
				Ref: "fishhawk/run-abc12345",
			},
			wantRef: "fishhawk/run-abc12345",
		},
		{
			name: "decomposed child slice branch",
			p: TriggerParams{
				RunID: runID, StageID: stageID, WorkflowID: "feature_change",
				StageExecutorRef: "claude-code", Scope: gitlabScope("77"),
				Ref:            "fishhawk/run-abc12345/slice-2",
				DecomposedFrom: &parent, SliceIndex: intPtr(2),
			},
			wantRef:       "fishhawk/run-abc12345/slice-2",
			wantParentVar: true,
			wantParentRef: parent.String(),
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ft := &fakePipelineTrigger{}
			g := &GitLabCI{Trigger: ft}
			if err := g.TriggerStage(context.Background(), c.p); err != nil {
				t.Fatalf("TriggerStage: %v", err)
			}
			if len(ft.calls) != 1 {
				t.Fatalf("pipeline calls = %d, want 1", len(ft.calls))
			}
			call := ft.calls[0]
			if call.ref != c.wantRef {
				t.Errorf("ref = %q, want %q", call.ref, c.wantRef)
			}
			if call.scope != c.p.Scope {
				t.Errorf("scope = %v, want %v", call.scope, c.p.Scope)
			}
			got := varsMap(call.vars)
			if got["FISHHAWK_RUN_ID"] != runID.String() || got["FISHHAWK_STAGE_ID"] != stageID.String() ||
				got["FISHHAWK_WORKFLOW_ID"] != "feature_change" || got["FISHHAWK_STAGE"] != "claude-code" {
				t.Errorf("variables = %+v", got)
			}
			pv, ok := got["FISHHAWK_PARENT_RUN_ID"]
			if c.wantParentVar {
				if !ok || pv != c.wantParentRef {
					t.Errorf("parent_run_id = %q (present=%v), want %q", pv, ok, c.wantParentRef)
				}
			} else if ok {
				t.Errorf("non-decomposed run set parent_run_id=%q, want it absent", pv)
			}
		})
	}
}

// TestGitLabCI_DispatchVarsMatchTemplateGate reconciles the two halves of the
// GitLab dispatch contract that the separate backend and onboarding tests each
// pass on in isolation: the CI/CD variable KEYS this backend supplies vs. the
// $FISHHAWK_* variable names the customer-side templates/.gitlab-ci.yml gates
// its `rules` on and forwards to the runner. A GitLab pipeline-trigger variable
// becomes a job env var under its EXACT key, so a mismatch (e.g. the backend
// emitting `run_id` while the template gates on `$FISHHAWK_RUN_ID`) would leave
// the gate false → `when: never` → the pipeline runs no stage, invisibly to
// either package's own tests. This test fails on any such drift.
func TestGitLabCI_DispatchVarsMatchTemplateGate(t *testing.T) {
	// Drive a decomposed child so ALL provenance keys — including the
	// parent-run key — are emitted and reconciled.
	ft := &fakePipelineTrigger{}
	g := &GitLabCI{Trigger: ft}
	parent := uuid.New()
	if err := g.TriggerStage(context.Background(), TriggerParams{
		RunID: uuid.New(), StageID: uuid.New(), WorkflowID: "feature_change",
		StageExecutorRef: "claude-code", Scope: gitlabScope("77"),
		Ref:            "fishhawk/run-abc12345/slice-2",
		DecomposedFrom: &parent, SliceIndex: intPtr(2),
	}); err != nil {
		t.Fatalf("TriggerStage: %v", err)
	}
	if len(ft.calls) != 1 {
		t.Fatalf("pipeline calls = %d, want 1", len(ft.calls))
	}
	backendKeys := map[string]bool{}
	for _, v := range ft.calls[0].vars {
		backendKeys[v.Key] = true
	}

	tmpl := string(onboarding.GitLabCITemplate())

	// (1) Every provenance key the backend supplies must be referenced as
	// $KEY somewhere in the template — otherwise the runner would receive an
	// empty flag (--run-id "$FISHHAWK_RUN_ID" with no such var) at go-live.
	for k := range backendKeys {
		if !strings.Contains(tmpl, "$"+k) {
			t.Errorf("backend supplies CI/CD var %q but the template never references $%s", k, k)
		}
	}

	// (2) The template's `rules` gate fires the fishhawk job only when its
	// required $FISHHAWK_* vars are truthy; every var the gate names MUST be
	// one the backend supplies, or the gate evaluates false, falls through to
	// `when: never`, and the dispatched pipeline runs no stage.
	gateVars := fishhawkVarsInRulesGate(tmpl)
	if len(gateVars) == 0 {
		t.Fatal("no $FISHHAWK_* vars found in the template rules gate")
	}
	for _, gv := range gateVars {
		if !backendKeys[gv] {
			t.Errorf("template rules gate requires %s but the backend never supplies that key — "+
				"the dispatched pipeline would match `when: never` and run no stage", gv)
		}
	}
}

// fishhawkVarsInRulesGate returns the FISHHAWK_* variable names (no leading $)
// referenced in the template's `rules:` gate conditions.
func fishhawkVarsInRulesGate(tmpl string) []string {
	varRe := regexp.MustCompile(`\$FISHHAWK_[A-Z_]+`)
	var out []string
	for _, line := range strings.Split(tmpl, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "- if:") && !strings.HasPrefix(trimmed, "if:") {
			continue
		}
		for _, m := range varRe.FindAllString(line, -1) {
			out = append(out, strings.TrimPrefix(m, "$"))
		}
	}
	return out
}

func varsMap(vars []gitlabclient.PipelineVariable) map[string]string {
	m := make(map[string]string, len(vars))
	for _, v := range vars {
		m[v.Key] = v.Value
	}
	return m
}

// --- TriggerStage: fail-closed guards (each its own behavioral assertion) ---

// Binding acceptance criterion 2(a): TriggerStage warn+skips and fires NO
// pipeline when the credential scope is the zero/unwired scope.
func TestGitLabCI_TriggerStage_ScopeIsZero_WarnSkipsNoPipeline(t *testing.T) {
	ft := &fakePipelineTrigger{}
	g := &GitLabCI{Trigger: ft}
	err := g.TriggerStage(context.Background(), TriggerParams{
		RunID: uuid.New(), Ref: "fishhawk/run-abc12345", // no Scope -> zero scope
	})
	if err != nil {
		t.Fatalf("zero-scope skip must return nil, got %v", err)
	}
	if len(ft.calls) != 0 {
		t.Errorf("zero scope must fire NO pipeline, got %d calls", len(ft.calls))
	}
}

// A nil Trigger (GitLab unconfigured) warn+skips rather than dispatching against
// a nil forge.
func TestGitLabCI_TriggerStage_NilTrigger_WarnSkips(t *testing.T) {
	g := &GitLabCI{Trigger: nil}
	if err := g.TriggerStage(context.Background(), TriggerParams{
		RunID: uuid.New(), Scope: gitlabScope("77"), Ref: "fishhawk/run-abc12345",
	}); err != nil {
		t.Fatalf("nil-trigger skip must return nil, got %v", err)
	}
}

// A trigger error propagates so the caller can audit it.
func TestGitLabCI_TriggerStage_PropagatesError(t *testing.T) {
	sentinel := errors.New("boom")
	g := &GitLabCI{Trigger: &fakePipelineTrigger{err: sentinel}}
	err := g.TriggerStage(context.Background(), TriggerParams{
		RunID: uuid.New(), Scope: gitlabScope("77"), Ref: "fishhawk/run-abc12345",
	})
	if !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want sentinel propagated", err)
	}
}

// --- dispatch-only division (through the REAL GitLab forge) ---

// capturingDoer records every HTTP request the real gitlab forge emits, so the
// test can assert what the backend does (and does NOT do) at the wire boundary.
type capturingDoer struct{ reqs []capturedReq }

type capturedReq struct {
	method string
	path   string
	body   []byte
}

func (d *capturingDoer) Do(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		body, _ = io.ReadAll(req.Body)
	}
	d.reqs = append(d.reqs, capturedReq{method: req.Method, path: req.URL.Path, body: body})
	return &http.Response{
		StatusCode: http.StatusCreated,
		Body:       io.NopCloser(strings.NewReader(`{"id":1,"ref":"fishhawk/run-abc12345","status":"created"}`)),
		Header:     make(http.Header),
	}, nil
}

// Binding acceptance criterion 2(b): TriggerStage NEVER writes a commit status.
// Driven through the REAL *gitlab.Forge (which is fully capable of writing a
// commit status via CreateCheckRun -> POST /statuses/:sha), the backend's ONLY
// wire interaction is the pipeline create; no /statuses/ request is ever made.
// A future edit that made GitLabCI also publish a status would surface a second
// request here and fail this named criterion.
func TestGitLabCI_TriggerStage_NeverWritesCommitStatus(t *testing.T) {
	doer := &capturingDoer{}
	glForge := forgegitlab.New("https://gitlab.example",
		forgegitlab.NewStaticCredentialProvider("glpat-test"),
		forgegitlab.WithHTTPClient(doer))

	g := &GitLabCI{Trigger: glForge}
	err := g.TriggerStage(context.Background(), TriggerParams{
		RunID: uuid.New(), StageID: uuid.New(), WorkflowID: "feature_change",
		StageExecutorRef: "claude-code", Scope: gitlabScope("77"),
		Ref: "fishhawk/run-abc12345",
	})
	if err != nil {
		t.Fatalf("TriggerStage: %v", err)
	}
	if len(doer.reqs) != 1 {
		t.Fatalf("wire requests = %d, want exactly 1 (the pipeline create)", len(doer.reqs))
	}
	req := doer.reqs[0]
	if req.method != http.MethodPost || req.path != "/api/v4/projects/77/pipeline" {
		t.Errorf("request = %s %s, want POST /api/v4/projects/77/pipeline", req.method, req.path)
	}
	for _, r := range doer.reqs {
		if strings.Contains(r.path, "/statuses/") {
			t.Errorf("backend wrote a commit status (%s %s) — dispatch-only division violated", r.method, r.path)
		}
	}
}
