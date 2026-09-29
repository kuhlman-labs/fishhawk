package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/account"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/delegation"
	"github.com/kuhlman-labs/fishhawk/backend/internal/delegationview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// Per-workflow delegation read (E76.1 / #3747):
// GET /v0/repos/{owner}/{name}/delegation.

// committedWorkflowSpec reads the repository's OWN .fishhawk/workflows.yaml.
// Reading the committed file — rather than a hand-written fixture — is what
// makes the parity and shipped-behaviour tests unsatisfiable by a comment-only
// touch of the projection.
func committedWorkflowSpec(t *testing.T) []byte {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	path := filepath.Join(filepath.Dir(thisFile), "..", "..", "..", ".fishhawk", "workflows.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the committed workflow spec: %v", err)
	}
	return raw
}

// delegationFixture seeds one run of acme/app whose cached spec is specYAML, so
// source=run_cache has something to project.
type delegationFixture struct{ runs *dashRunRepo }

func newDelegationFixture(specYAML string) *delegationFixture {
	f := &delegationFixture{runs: &dashRunRepo{fakeRepo: newFakeRepo(), stages: map[uuid.UUID][]*run.Stage{}}}
	id := dashID(901)
	created := time.Date(2026, 9, 28, 9, 0, 0, 0, time.UTC)
	f.runs.mu.Lock()
	f.runs.runs[id] = &run.Run{
		ID: id, Repo: "acme/app", WorkflowID: "feature_change", WorkflowSHA: "cachedsha",
		TriggerSource: run.TriggerCLI, State: run.StateSucceeded,
		CreatedAt: created, UpdatedAt: created, WorkflowSpec: []byte(specYAML),
	}
	f.runs.mu.Unlock()
	return f
}

func (f *delegationFixture) server() *Server {
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: f.runs})
	s.nowFunc = func() time.Time { return dashNow }
	return s
}

// delegationGET drives the REAL mux.
func delegationGET(t *testing.T, s *Server, query string) *httptest.ResponseRecorder {
	t.Helper()
	return dashGET(t, s, "/v0/repos/acme/app/delegation"+query)
}

// delegationView decodes a 200 body into the wire view.
func delegationView(t *testing.T, rec *httptest.ResponseRecorder) delegationview.View {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	var v delegationview.View
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode: %v; body %s", err, rec.Body.String())
	}
	return v
}

func delegationWorkflow(t *testing.T, v delegationview.View, id string) delegationview.WorkflowDelegation {
	t.Helper()
	for _, wf := range v.Workflows {
		if wf.ID == id {
			return wf
		}
	}
	t.Fatalf("workflow %q absent from the view (%d workflows)", id, len(v.Workflows))
	return delegationview.WorkflowDelegation{}
}

func delegationMatrixByAction(m []delegationview.Action) map[string]delegationview.Action {
	out := make(map[string]delegationview.Action, len(m))
	for _, a := range m {
		out[a.Action] = a
	}
	return out
}

// ---------------------------------------------------------------------------
// PARITY (AC1)
// ---------------------------------------------------------------------------

// noFiredEscalations is a delegation.EscalationResolver that raises nothing.
//
// That is FAITHFUL rather than convenient: the escalation half of this endpoint
// is DECLARATIVE. An escalation matching on `paths` is evaluated at the approval
// gate against an APPROVED PLAN's scope.files, and the run rows this parity test
// builds carry no plan, so nothing fires and nothing is clamped. The parity
// claim is therefore against the UNCLAMPED run-side matrix — which is exactly
// what delegationview.Project produces, with the ceiling reported separately in
// ceiling_matrix.
type noFiredEscalations struct{}

func (noFiredEscalations) ResolveEscalations(context.Context, *run.Run, *spec.Workflow, uuid.UUID) (spec.ComposedRequirements, error) {
	return spec.ComposedRequirements{}, nil
}

// emptyStageLister / emptyConcernLister / emptyAuditLister are the three other
// evaluator dependencies at their pre-run values: no stage has run, so no gate
// is current and ResolveAutonomy falls to the WORKFLOW-level block — the same
// block Project reads.
type emptyStageLister struct{}

func (emptyStageLister) ListStagesForRun(context.Context, uuid.UUID) ([]*run.Stage, error) {
	return nil, nil
}

type emptyConcernLister struct{}

func (emptyConcernLister) ListOpenByRun(context.Context, uuid.UUID) ([]*concern.Concern, error) {
	return nil, nil
}

type emptyAuditLister struct{}

func (emptyAuditLister) ListForRunByCategory(context.Context, uuid.UUID, string) ([]*audit.Entry, error) {
	return nil, nil
}

// TestDelegationView_MatchesRunDelegationBlockForEveryWorkflow is AC1: for
// EVERY workflow the committed spec declares, the projection's autonomy, matrix
// (action/mode/condition/min_severity/source, IN ORDER) and must_page_human
// equal the corresponding fields of the run-side delegation block — built by the
// exact pair handleGetRun uses, delegation.Evaluate + delegationPayloadFrom.
//
// It iterates the PARSED workflow map rather than a hand-written list, so a
// workflow added to the spec is covered automatically.
func TestDelegationView_MatchesRunDelegationBlockForEveryWorkflow(t *testing.T) {
	specBytes := committedWorkflowSpec(t)
	parsed, err := spec.ParseBytes(specBytes)
	if err != nil {
		t.Fatalf("the committed workflow spec does not parse: %v", err)
	}
	if len(parsed.Workflows) == 0 {
		t.Fatal("the committed spec declares no workflows — the parity sweep would be vacuous")
	}
	major := spec.VersionMajor(parsed.Version)

	projected := delegationview.Project(parsed)
	byID := make(map[string]delegationview.WorkflowDelegation, len(projected))
	for _, wf := range projected {
		byID[wf.ID] = wf
	}
	if len(byID) != len(parsed.Workflows) {
		t.Fatalf("projected %d workflows, want the spec's %d", len(byID), len(parsed.Workflows))
	}

	ev, err := delegation.NewEvaluator(emptyStageLister{}, emptyConcernLister{}, emptyAuditLister{}, noFiredEscalations{})
	if err != nil {
		t.Fatalf("new evaluator: %v", err)
	}

	for id := range parsed.Workflows {
		t.Run(id, func(t *testing.T) {
			got, ok := byID[id]
			if !ok {
				t.Fatalf("workflow %q is declared but not projected", id)
			}
			wf := parsed.Workflows[id]
			runRow := &run.Run{
				ID: uuid.New(), Repo: "acme/app", WorkflowID: id,
				State: run.StateRunning, WorkflowSpec: specBytes,
			}
			res, err := ev.Evaluate(context.Background(), runRow, &wf, nil)
			if err != nil {
				t.Fatalf("evaluate: %v", err)
			}
			want := delegationPayloadFrom(res, major)
			if want == nil {
				// The workflow delegates nothing (declares no autonomy block),
				// so the run-side block is omitted. The projection's fail-closed
				// reading is an EMPTY matrix with an empty tier.
				if got.Autonomy != "" || len(got.Matrix) != 0 {
					t.Errorf("run side omits the delegation block but the projection reports autonomy=%q matrix=%+v",
						got.Autonomy, got.Matrix)
				}
				return
			}
			if got.Autonomy != want.Autonomy {
				t.Errorf("autonomy = %q, run side = %q", got.Autonomy, want.Autonomy)
			}
			if len(got.Matrix) != len(want.Matrix) {
				t.Fatalf("matrix has %d classes, run side has %d:\n projected %+v\n run side  %+v",
					len(got.Matrix), len(want.Matrix), got.Matrix, want.Matrix)
			}
			for i := range want.Matrix {
				w, g := want.Matrix[i], got.Matrix[i]
				if g.Action != w.Action || g.Mode != w.Mode || g.Condition != w.Condition ||
					g.MinSeverity != w.MinSeverity || g.Source != w.Source {
					t.Errorf("matrix[%d] = %+v, run side = %+v", i, g, w)
				}
			}
			if !reflect.DeepEqual(got.MustPageHuman, want.MustPageHuman) {
				t.Errorf("must_page_human = %v, run side = %v", got.MustPageHuman, want.MustPageHuman)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// DONE-MEANS / SHIPPED BEHAVIOUR (#1169) and the CROSS-BOUNDARY end-to-end pin
// ---------------------------------------------------------------------------

// featureChangeEscalationPaths is the escalation glob set the committed spec
// declares on feature_change. Stated here so the assertion is on the SHIPPED
// seven globs and not on "whatever the projection produced".
var featureChangeEscalationPaths = []string{
	"backend/internal/spec/**",
	"cli/internal/spec/**",
	"backend/internal/audit/**",
	"backend/internal/policy/**",
	"backend/internal/auth/**",
	"backend/internal/githubapp/**",
	"verifier/**",
}

// TestDelegationView_FeatureChangeEscalationIsSurfaced is the CROSS-BOUNDARY
// end-to-end test (spec -> pure projection -> HTTP payload): it drives the REAL
// http.Handler over a seeded run whose cached spec is the COMMITTED
// .fishhawk/workflows.yaml and asserts on the DECODED JSON BODY.
//
// IT DOES NOT PIN THE JSON TAG NAMES, and an earlier draft of this comment
// wrongly claimed it did. The body is decoded back into delegationview.View —
// the very type the handler serialized — so a renamed tag renames both halves of
// the round trip and every assertion below still passes. The tag names are
// pinned by mcpserver's
// TestDelegationTool_SerializedOutputCarriesTheDeclaredWireFieldNames, which
// reads a serialized delegation view against field names written out as
// literals; that is the SAME shared type this handler serializes, so it covers
// this surface too.
//
// The escalation half is config-shaped and not compiler-enforced, so the
// assertions are on the shipped output: the seven globs, max_autonomy medium,
// and ceiling_matrix reporting waive and merge at gated with source=escalation.
func TestDelegationView_FeatureChangeEscalationIsSurfaced(t *testing.T) {
	f := newDelegationFixture(string(committedWorkflowSpec(t)))
	v := delegationView(t, delegationGET(t, f.server(), "?source=run_cache"))

	if v.Repo != "acme/app" {
		t.Errorf("repo = %q", v.Repo)
	}
	if v.Source != "run_cache" {
		t.Errorf("source = %q, want run_cache — the source is always echoed", v.Source)
	}
	if v.SpecVersion != "2" || v.SchemaMajor != 2 {
		t.Errorf("spec_version/schema_major = %q/%d, want 2/2", v.SpecVersion, v.SchemaMajor)
	}
	if v.WorkflowSHA != "cachedsha" {
		t.Errorf("workflow_sha = %q, want the cached run's", v.WorkflowSHA)
	}
	if v.ContentHash == "" {
		t.Error("the view carries no content_hash")
	}

	fc := delegationWorkflow(t, v, "feature_change")
	if fc.Autonomy != "high" {
		t.Errorf("feature_change autonomy = %q, want high", fc.Autonomy)
	}
	if fc.ContentHash == "" {
		t.Error("the feature_change entry carries no content_hash")
	}
	matrix := delegationMatrixByAction(fc.Matrix)
	for class, want := range map[string]struct{ mode, source, condition string }{
		"approve": {"auto", "tier", "clean_dual_approval"},
		"fixup":   {"auto", "tier", "convergent_concerns"},
		"waive":   {"auto", "tier", "solo_low"},
		"retry":   {"auto", "tier", "infra_flake"},
		"merge":   {"auto", "tier", "gates_resolved_ci_green"},
	} {
		a, ok := matrix[class]
		if !ok {
			t.Fatalf("class %q absent from the decoded matrix", class)
		}
		if a.Mode != want.mode || a.Source != want.source || a.Condition != want.condition {
			t.Errorf("%s = {mode:%q source:%q condition:%q}, want {%q %q %q}",
				class, a.Mode, a.Source, a.Condition, want.mode, want.source, want.condition)
		}
	}
	if len(fc.MustPageHuman) == 0 {
		t.Error("feature_change must surface the tier's page list as must_page_human")
	}

	if len(fc.Escalations) != 1 {
		t.Fatalf("feature_change has %d escalations in the decoded body, want 1", len(fc.Escalations))
	}
	e := fc.Escalations[0]
	if !reflect.DeepEqual(e.Match.Paths, featureChangeEscalationPaths) {
		t.Errorf("escalations[0].match.paths =\n %v\nwant the committed spec's seven globs:\n %v",
			e.Match.Paths, featureChangeEscalationPaths)
	}
	if e.MaxAutonomy != "medium" {
		t.Errorf("escalations[0].max_autonomy = %q, want medium", e.MaxAutonomy)
	}
	if e.Approvals != nil {
		t.Errorf("escalations[0].approvals = %+v, want absent — the committed spec deliberately raises no approval count", e.Approvals)
	}
	ceiling := delegationMatrixByAction(e.CeilingMatrix)
	for _, class := range []string{"waive", "merge"} {
		a, ok := ceiling[class]
		if !ok {
			t.Fatalf("class %q absent from ceiling_matrix", class)
		}
		if a.Mode != "gated" || a.Source != "escalation" {
			t.Errorf("ceiling_matrix %s = {mode:%q source:%q}, want {gated escalation}", class, a.Mode, a.Source)
		}
	}
	for _, class := range []string{"approve", "fixup", "retry"} {
		if a := ceiling[class]; a.Mode != "auto" {
			t.Errorf("ceiling_matrix %s mode = %q, want auto — a medium ceiling keeps these delegated", class, a.Mode)
		}
	}
	// The workflow's OWN matrix is never clamped: the projection is declarative.
	for _, class := range []string{"waive", "merge"} {
		if a := matrix[class]; a.Mode != "auto" {
			t.Errorf("matrix %s mode = %q, want auto — ceiling_matrix must not leak into the workflow matrix", class, a.Mode)
		}
	}
}

// TestRepoDelegation_ContentHashIsStableAcrossSourceAndRef drives the SAME spec
// through source=run_cache and source=ref and requires an identical
// content_hash: the hash covers the delegation content, not where it was read.
func TestRepoDelegation_ContentHashIsStableAcrossSourceAndRef(t *testing.T) {
	specYAML := string(committedWorkflowSpec(t))
	f := newDelegationFixture(specYAML)
	cached := delegationView(t, delegationGET(t, f.server(), "?source=run_cache"))

	fake := newFakeGitHubForRuns(specYAML)
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: f.runs, GitHub: screenGitHub(t, fake)})
	s.nowFunc = func() time.Time { return dashNow }
	fromRef := delegationView(t, delegationGET(t, s, "?source=ref&ref=release/1.2"))

	if fromRef.Source != "ref" || fromRef.Ref != "release/1.2" {
		t.Errorf("source/ref echo = %q/%q, want ref/release/1.2", fromRef.Source, fromRef.Ref)
	}
	if fromRef.WorkflowSHA != "spec_sha" {
		t.Errorf("workflow_sha = %q, want the forge blob sha", fromRef.WorkflowSHA)
	}
	if cached.ContentHash != fromRef.ContentHash {
		t.Errorf("content_hash differs across sources:\n run_cache = %s (sha %q)\n ref       = %s (sha %q)",
			cached.ContentHash, cached.WorkflowSHA, fromRef.ContentHash, fromRef.WorkflowSHA)
	}
	if a, b := delegationWorkflow(t, cached, "feature_change"), delegationWorkflow(t, fromRef, "feature_change"); a.ContentHash != b.ContentHash {
		t.Errorf("per-workflow content_hash differs across sources: %s vs %s", a.ContentHash, b.ContentHash)
	}
}

// TestRepoDelegation_RefSourceFetchesAtDefaultBranch pins the ref="" assumption
// the endpoint's default rests on: no ref parameter reaches the Contents API
// when the caller supplies none, so GitHub serves the repository's DEFAULT
// BRANCH head. If that behaviour ever changes, this fails loudly.
func TestRepoDelegation_RefSourceFetchesAtDefaultBranch(t *testing.T) {
	specYAML := string(committedWorkflowSpec(t))
	fake := newFakeGitHubForRuns(specYAML)
	var gotQuery string
	ghSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/installation") {
			_, _ = w.Write([]byte(fake.installationBody))
			return
		}
		gotQuery = r.URL.RawQuery
		_, _ = w.Write([]byte(fake.specBody))
	}))
	t.Cleanup(ghSrv.Close)

	s := New(Config{Addr: "127.0.0.1:0", RunRepo: newDelegationFixture(specYAML).runs,
		GitHub: newServerWithGitHub(t, newFakeRepo(), ghSrv).cfg.GitHub})
	s.nowFunc = func() time.Time { return dashNow }
	v := delegationView(t, delegationGET(t, s, ""))

	if v.Source != "ref" {
		t.Errorf("source = %q, want the ref default", v.Source)
	}
	if v.Ref != "" {
		t.Errorf("ref = %q, want the empty echo", v.Ref)
	}
	if strings.Contains(gotQuery, "ref=") {
		t.Errorf("the Contents API request carried %q; an absent ref must serve the default branch", gotQuery)
	}
}

// TestRepoDelegation_WorkflowFilter narrows to one workflow and keeps that
// entry's per-workflow hash IDENTICAL to the unfiltered read's, while the
// view-level hash reflects the retained set.
func TestRepoDelegation_WorkflowFilter(t *testing.T) {
	f := newDelegationFixture(string(committedWorkflowSpec(t)))
	s := f.server()
	all := delegationView(t, delegationGET(t, s, "?source=run_cache"))
	one := delegationView(t, delegationGET(t, s, "?source=run_cache&workflow=feature_change"))

	if len(one.Workflows) != 1 || one.Workflows[0].ID != "feature_change" {
		t.Fatalf("filtered view = %d workflows %v", len(one.Workflows), one.Workflows)
	}
	if got, want := one.Workflows[0].ContentHash, delegationWorkflow(t, all, "feature_change").ContentHash; got != want {
		t.Errorf("filtered per-workflow hash = %s, unfiltered = %s — they must agree", got, want)
	}
	if len(all.Workflows) > 1 && one.ContentHash == all.ContentHash {
		t.Error("the view-level hash did not move with the retained set")
	}
}

// ---------------------------------------------------------------------------
// PER-FAILURE-MODE (#1182): seven refusal modes, each with its own status AND
// code. The 502 pair and the empty-spec 404 branch are the last section of this
// file; the six below were the first pass.
// ---------------------------------------------------------------------------

// TestRepoDelegation_UnknownSourceIs400. The fixture HOLDS a projectable run,
// so with the source validation removed the handler would answer 200.
func TestRepoDelegation_UnknownSourceIs400(t *testing.T) {
	s := newDelegationFixture(string(committedWorkflowSpec(t))).server()
	for _, bad := range []string{"forge", "cache", "REF", "run-cache"} {
		rec := delegationGET(t, s, "?source="+bad)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("source=%s: status = %d, want 400; body %s", bad, rec.Code, rec.Body.String())
		}
		assertErrorCode(t, rec, "validation_failed")
		if !strings.Contains(rec.Body.String(), `"field":"source"`) {
			t.Errorf("source=%s: body does not name the field: %s", bad, rec.Body.String())
		}
	}
}

// TestRepoDelegation_RefSourceWithoutForgeIs503 is counterfactual (3)'s
// vehicle. The server is built with cfg.GitHub NIL while a run_cache-readable
// run EXISTS for the same repo — so with the guard deleted the branch either
// nil-panics or silently reads the cached spec and answers 200, neither of which
// is the asserted 503.
func TestRepoDelegation_RefSourceWithoutForgeIs503(t *testing.T) {
	s := newDelegationFixture(string(committedWorkflowSpec(t))).server()
	// Proof the fixture is projectable through the OTHER source: without this,
	// a 503 could mean "nothing to read" rather than "the guard fired".
	if rec := delegationGET(t, s, "?source=run_cache"); rec.Code != http.StatusOK {
		t.Fatalf("precondition: source=run_cache = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	for _, q := range []string{"", "?source=ref", "?source=ref&ref=main"} {
		rec := delegationGET(t, s, q)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%q: status = %d, want 503; body %s", q, rec.Code, rec.Body.String())
		}
		assertErrorCode(t, rec, "github_unconfigured")
		if !strings.Contains(rec.Body.String(), "run_cache") {
			t.Errorf("%q: the 503 must name the run_cache alternative: %s", q, rec.Body.String())
		}
	}
}

// TestRepoDelegation_SpecNotFoundIs404 covers BOTH sources' not-found branch:
// no cached run under run_cache, and a 404 from the forge under ref.
func TestRepoDelegation_SpecNotFoundIs404(t *testing.T) {
	empty := &delegationFixture{runs: &dashRunRepo{fakeRepo: newFakeRepo(), stages: map[uuid.UUID][]*run.Stage{}}}
	rec := delegationGET(t, empty.server(), "?source=run_cache")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("run_cache with no run: status = %d, want 404; body %s", rec.Code, rec.Body.String())
	}
	assertErrorCode(t, rec, "workflow_spec_not_found")

	fake := newFakeGitHubForRuns("")
	fake.specStatus = http.StatusNotFound
	fake.specBody = `{"message":"Not Found"}`
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: empty.runs, GitHub: screenGitHub(t, fake)})
	s.nowFunc = func() time.Time { return dashNow }
	rec = delegationGET(t, s, "?source=ref")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("ref with no spec at the ref: status = %d, want 404; body %s", rec.Code, rec.Body.String())
	}
	assertErrorCode(t, rec, "workflow_spec_not_found")
}

// delegationInvalidSpec DECODES into a workflow map but FAILS validation:
// `mode: auto` on the non-delegable `ordering` grooming class. That shape is
// load-bearing for counterfactual (1) — Project would happily emit a matrix for
// a decoded spec, so the parse guard is the only thing standing between an
// invalid spec and a partial matrix.
const delegationInvalidSpec = `version: "2"
workflows:
  backlog_grooming:
    applies_to:
      trigger: [scheduled]
    autonomy: low
    actions:
      ordering:
        mode: auto
    stages:
      - id: groom
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: grooming_report
            schema: grooming_report_v1
`

// TestRepoDelegation_InvalidSpecReturnsNamedErrorNotPartialMatrix is
// counterfactual (1)'s vehicle: with the 422 guard removed the handler falls
// through to projection and answers 200 with a workflows key.
func TestRepoDelegation_InvalidSpecReturnsNamedErrorNotPartialMatrix(t *testing.T) {
	// The fixture is genuinely a validation failure, not a decode failure.
	if _, err := spec.ParseBytes([]byte(delegationInvalidSpec)); err == nil {
		t.Fatal("the fixture spec validates; it cannot serve as the invalid-spec arm")
	}
	f := newDelegationFixture(delegationInvalidSpec)
	rec := delegationGET(t, f.server(), "?source=run_cache")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body %s", rec.Code, rec.Body.String())
	}
	assertErrorCode(t, rec, "workflow_spec_invalid")

	var body map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v; body %s", err, rec.Body.String())
	}
	if _, present := body["workflows"]; present {
		t.Errorf("the 422 body carries a workflows key — a partial matrix was emitted: %s", rec.Body.String())
	}
	if _, present := body["content_hash"]; present {
		t.Errorf("the 422 body carries a content_hash — nothing was shown, so nothing may be bound to: %s", rec.Body.String())
	}
	// The named validation error is reported so the operator can fix the spec.
	if !strings.Contains(rec.Body.String(), "ordering") {
		t.Errorf("the 422 body does not name the offending class: %s", rec.Body.String())
	}
}

// TestRepoDelegation_UnknownWorkflowIs404. The spec DECLARES workflows, so with
// the filter's empty-set check removed the handler would answer 200 with an
// empty workflows array.
func TestRepoDelegation_UnknownWorkflowIs404(t *testing.T) {
	f := newDelegationFixture(string(committedWorkflowSpec(t)))
	rec := delegationGET(t, f.server(), "?source=run_cache&workflow=no_such_workflow")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body %s", rec.Code, rec.Body.String())
	}
	assertErrorCode(t, rec, "workflow_not_found")
	if !strings.Contains(rec.Body.String(), "no_such_workflow") {
		t.Errorf("the 404 does not name the requested workflow: %s", rec.Body.String())
	}
}

// TestRepoDelegation_InvisibleRepoIs403 is counterfactual (2)'s vehicle. The
// identity's workspace is seeded with visibility over other/repo ONLY while the
// seeded run and its spec live under acme/app — so with repoVisibleOr403's call
// site removed the handler finds the run and answers 200 with a real matrix.
func TestRepoDelegation_InvisibleRepoIs403(t *testing.T) {
	f := newDelegationFixture(string(committedWorkflowSpec(t)))
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: f.runs,
		AccountRoles:   fakeAccountRoles{role: account.RoleMember},
		RepoVisibility: newFakeRepoVisibility(map[string]bool{"other/repo": true})})
	s.nowFunc = func() time.Time { return dashNow }

	req := httptest.NewRequest(http.MethodGet, "/v0/repos/acme/app/delegation?source=run_cache", nil)
	req.SetPathValue("owner", "acme")
	req.SetPathValue("name", "app")
	rec := httptest.NewRecorder()
	s.handleGetRepoDelegation(rec, withIdentity(req, memberIdentity()))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body %s", rec.Code, rec.Body.String())
	}
	assertErrorCode(t, rec, "repo_forbidden")
}

// TestRepoDelegation_RunRepoUnconfiguredIs503 is the prelude's own 503, which
// this route inherits rather than reimplements.
func TestRepoDelegation_RunRepoUnconfiguredIs503(t *testing.T) {
	rec := delegationGET(t, New(Config{Addr: "127.0.0.1:0"}), "?source=run_cache")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body %s", rec.Code, rec.Body.String())
	}
	assertErrorCode(t, rec, "run_repo_unconfigured")
}

// ---------------------------------------------------------------------------
// The forge-fault branches (fix-up: the 502 pair + the empty-spec 404)
// ---------------------------------------------------------------------------
//
// The six-refusal sweep above covers 400/403/404/422/503. delegationSpecFromRef
// carries three more branches it did not reach: a NON-NotFound error from
// GetRepoInstallation, a NON-NotFound error from GetWorkflowSpec (both 502
// forge_unavailable) and a 200 whose spec content is EMPTY (404
// workflow_spec_not_found). Each is a straight writeError with no logic behind
// it, so these are hardening rather than a claimed defect — but an untested
// branch is an unasserted status code.

// TestRepoDelegation_InstallationFaultIs502 seeds a NON-404 fault on the
// installation endpoint. The fixture also holds a run_cache-readable run, so a
// 502 cannot be "nothing to read": the precondition below proves it.
func TestRepoDelegation_InstallationFaultIs502(t *testing.T) {
	f := newDelegationFixture(string(committedWorkflowSpec(t)))
	fake := newFakeGitHubForRuns(string(committedWorkflowSpec(t)))
	fake.installationStatus = http.StatusInternalServerError
	fake.installationBody = `{"message":"boom"}`
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: f.runs, GitHub: screenGitHub(t, fake)})
	s.nowFunc = func() time.Time { return dashNow }

	if rec := delegationGET(t, s, "?source=run_cache"); rec.Code != http.StatusOK {
		t.Fatalf("precondition: source=run_cache = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	rec := delegationGET(t, s, "?source=ref&ref=main")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body %s", rec.Code, rec.Body.String())
	}
	assertErrorCode(t, rec, "forge_unavailable")
	// The 502 is distinguished from the sibling 404: a forge FAULT is not
	// "no installation is visible".
	if strings.Contains(rec.Body.String(), "workflow_spec_not_found") {
		t.Errorf("a forge fault was reported as a not-found: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "installation") {
		t.Errorf("the 502 does not say WHICH forge call failed: %s", rec.Body.String())
	}
}

// TestRepoDelegation_SpecFetchFaultIs502 seeds a healthy installation lookup and
// a NON-404 fault on the Contents API, so the 502 attributed to the SPEC fetch
// is reached only past the installation branch.
func TestRepoDelegation_SpecFetchFaultIs502(t *testing.T) {
	f := newDelegationFixture(string(committedWorkflowSpec(t)))
	fake := newFakeGitHubForRuns(string(committedWorkflowSpec(t)))
	fake.specStatus = http.StatusInternalServerError
	fake.specBody = `{"message":"boom"}`
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: f.runs, GitHub: screenGitHub(t, fake)})
	s.nowFunc = func() time.Time { return dashNow }

	rec := delegationGET(t, s, "?source=ref&ref=main")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body %s", rec.Code, rec.Body.String())
	}
	assertErrorCode(t, rec, "forge_unavailable")
	if !strings.Contains(rec.Body.String(), "spec") {
		t.Errorf("the 502 does not attribute the failure to the spec fetch: %s", rec.Body.String())
	}
	// The installation lookup DID succeed, so this 502 is the second branch and
	// not the first one firing under a different name.
	if fake.installationCalls == 0 {
		t.Error("the installation endpoint was never called; the fixture does not isolate the spec-fetch branch")
	}
	if fake.specCalls == 0 {
		t.Error("the Contents endpoint was never called; the spec-fetch branch was not reached")
	}
}

// TestRepoDelegation_EmptySpecAtRefIs404 covers the empty-content branch: the
// forge answers 200 with a spec file that is empty or whitespace-only. Both are
// 404 workflow_spec_not_found naming the path as EMPTY — NOT the 422 an invalid
// spec draws, which is what the guard buys: "the file is there but has nothing
// in it" is a different operator action from "the spec does not validate".
func TestRepoDelegation_EmptySpecAtRefIs404(t *testing.T) {
	for _, content := range []string{"", "\n\n   \n\t\n"} {
		f := newDelegationFixture(string(committedWorkflowSpec(t)))
		s := New(Config{Addr: "127.0.0.1:0", RunRepo: f.runs,
			GitHub: screenGitHub(t, newFakeGitHubForRuns(content))})
		s.nowFunc = func() time.Time { return dashNow }

		rec := delegationGET(t, s, "?source=ref&ref=main")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("content %q: status = %d, want 404; body %s", content, rec.Code, rec.Body.String())
		}
		assertErrorCode(t, rec, "workflow_spec_not_found")
		if !strings.Contains(rec.Body.String(), "is empty at this ref") {
			t.Errorf("content %q: the 404 does not say the file is EMPTY: %s", content, rec.Body.String())
		}
	}
}
