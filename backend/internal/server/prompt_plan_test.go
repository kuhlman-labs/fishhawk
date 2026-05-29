package server

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan/planfixture"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

/*
 * Tests for the plan-as-contract path on the implement-stage prompt
 * (#223). These need a richer run.Repository fake than the prompt
 * tests use today (must return real stages from ListStagesForRun)
 * and an artifact.Repository fake — both kept local to this file so
 * the existing prompt_test.go scaffolding stays untouched.
 */

// planPromptRunRepo extends promptRunRepo's intent with a working
// ListStagesForRun. Two stages per run is the v0 shape (plan +
// implement). The implement-stage prompt resolution walks
// ListStagesForRun → finds the plan stage → reads its artifacts.
type planPromptRunRepo struct {
	stages    map[uuid.UUID][]*run.Stage
	stageByID map[uuid.UUID]*run.Stage
	runs      map[uuid.UUID]*run.Run
	listErr   error
}

func newPlanPromptRunRepo() *planPromptRunRepo {
	return &planPromptRunRepo{
		stages:    map[uuid.UUID][]*run.Stage{},
		stageByID: map[uuid.UUID]*run.Stage{},
		runs:      map[uuid.UUID]*run.Run{},
	}
}

func (r *planPromptRunRepo) seedRun(rn *run.Run) { r.runs[rn.ID] = rn }
func (r *planPromptRunRepo) seedStages(runID uuid.UUID, stages ...*run.Stage) {
	r.stages[runID] = append(r.stages[runID], stages...)
	for _, s := range stages {
		r.stageByID[s.ID] = s
	}
}

func (r *planPromptRunRepo) GetStage(_ context.Context, id uuid.UUID) (*run.Stage, error) {
	if s, ok := r.stageByID[id]; ok {
		return s, nil
	}
	return nil, run.ErrNotFound
}
func (r *planPromptRunRepo) GetRun(_ context.Context, id uuid.UUID) (*run.Run, error) {
	if rn, ok := r.runs[id]; ok {
		return rn, nil
	}
	return nil, run.ErrNotFound
}
func (r *planPromptRunRepo) ListStagesForRun(_ context.Context, runID uuid.UUID) ([]*run.Stage, error) {
	if r.listErr != nil {
		return nil, r.listErr
	}
	return r.stages[runID], nil
}

func (r *planPromptRunRepo) CreateRun(context.Context, run.CreateRunParams) (*run.Run, error) {
	return nil, errors.New("not used")
}
func (r *planPromptRunRepo) GetRunByIdempotencyKey(context.Context, string, string) (*run.Run, error) {
	return nil, run.ErrNotFound
}
func (r *planPromptRunRepo) ListRuns(context.Context, run.ListRunsFilter) ([]*run.Run, error) {
	return nil, errors.New("not used")
}
func (r *planPromptRunRepo) TransitionRun(context.Context, uuid.UUID, run.State) (*run.Run, error) {
	return nil, errors.New("not used")
}
func (r *planPromptRunRepo) SetRunPullRequestURL(context.Context, uuid.UUID, string) (*run.Run, error) {
	return nil, errors.New("not used")
}
func (r *planPromptRunRepo) CreateStage(context.Context, run.CreateStageParams) (*run.Stage, error) {
	return nil, errors.New("not used")
}
func (r *planPromptRunRepo) ListStagesAwaitingApproval(context.Context) ([]*run.Stage, error) {
	return nil, errors.New("not used")
}

func (r *planPromptRunRepo) ListStagesAwaitingChildren(context.Context) ([]*run.Stage, error) {
	return nil, errors.New("not used")
}
func (r *planPromptRunRepo) ListStagesDispatched(context.Context) ([]*run.Stage, error) {
	return nil, nil
}
func (r *planPromptRunRepo) RetryStage(context.Context, uuid.UUID, run.StageState) (*run.Stage, error) {
	return nil, errors.New("not used")
}
func (r *planPromptRunRepo) TransitionStage(context.Context, uuid.UUID, run.StageState, *run.StageCompletion) (*run.Stage, error) {
	return nil, errors.New("not used")
}

// planArtifactRepo holds canned artifacts keyed by stage_id.
type planArtifactRepo struct {
	byStage map[uuid.UUID][]*artifact.Artifact
	listErr error
}

func newPlanArtifactRepo() *planArtifactRepo {
	return &planArtifactRepo{byStage: map[uuid.UUID][]*artifact.Artifact{}}
}

func (r *planArtifactRepo) seed(stageID uuid.UUID, arts ...*artifact.Artifact) {
	r.byStage[stageID] = append(r.byStage[stageID], arts...)
}

func (r *planArtifactRepo) ListForStage(_ context.Context, stageID uuid.UUID) ([]*artifact.Artifact, error) {
	if r.listErr != nil {
		return nil, r.listErr
	}
	return r.byStage[stageID], nil
}

func (r *planArtifactRepo) Get(_ context.Context, _ uuid.UUID) (*artifact.Artifact, error) {
	return nil, errors.New("not used")
}

func (r *planArtifactRepo) GetByHash(_ context.Context, _ uuid.UUID, _ string) (*artifact.Artifact, error) {
	return nil, artifact.ErrNotFound
}

func (r *planArtifactRepo) Create(_ context.Context, _ artifact.CreateParams) (*artifact.Artifact, error) {
	return nil, errors.New("not used")
}

// planAuditRepo records appended entries; the implement-prompt
// handler emits `plan_missing_for_implement` when the plan isn't
// found, and the test asserts that. byRunCategory supports seeding
// canned entries for ListForRunByCategory.
type planAuditRepo struct {
	appended      []audit.ChainAppendParams
	byRunCategory map[string][]*audit.Entry
}

func (a *planAuditRepo) seedByCategory(runID uuid.UUID, category string, entries ...*audit.Entry) {
	if a.byRunCategory == nil {
		a.byRunCategory = map[string][]*audit.Entry{}
	}
	key := runID.String() + ":" + category
	a.byRunCategory[key] = append(a.byRunCategory[key], entries...)
}

func (a *planAuditRepo) Append(context.Context, audit.AppendParams) (*audit.Entry, error) {
	return nil, errors.New("not used")
}

func (a *planAuditRepo) ChainsByParent(_ context.Context, _ uuid.UUID, _ bool) ([]*audit.Entry, error) {
	return nil, nil
}
func (a *planAuditRepo) AppendChained(_ context.Context, p audit.ChainAppendParams) (*audit.Entry, error) {
	a.appended = append(a.appended, p)
	rid := p.RunID
	return &audit.Entry{ID: uuid.New(), RunID: &rid}, nil
}
func (a *planAuditRepo) AppendGlobalChained(context.Context, audit.GlobalChainAppendParams) (*audit.Entry, error) {
	return nil, errors.New("not used")
}
func (a *planAuditRepo) ListGlobal(context.Context) ([]*audit.Entry, error) {
	return nil, nil
}
func (a *planAuditRepo) ListAll(context.Context, audit.ListAllParams) ([]*audit.Entry, error) {
	return nil, nil
}
func (a *planAuditRepo) Get(context.Context, uuid.UUID) (*audit.Entry, error) {
	return nil, audit.ErrNotFound
}
func (a *planAuditRepo) ListForRun(context.Context, uuid.UUID) ([]*audit.Entry, error) {
	return nil, nil
}
func (a *planAuditRepo) ListForRunByCategory(_ context.Context, runID uuid.UUID, category string) ([]*audit.Entry, error) {
	if a.byRunCategory != nil {
		key := runID.String() + ":" + category
		if entries, ok := a.byRunCategory[key]; ok {
			return entries, nil
		}
	}
	return nil, nil
}
func (a *planAuditRepo) LastForRun(context.Context, uuid.UUID) (*audit.Entry, error) {
	return nil, audit.ErrNotFound
}

// fixturePlanJSON returns a minimal standard_v1 plan as the bytes an
// artifact would carry. Uses planfixture.Valid() so all required schema
// fields (including predicted_runtime_minutes and predicted_runtime_confidence)
// are present.
func fixturePlanJSON(t *testing.T) []byte {
	t.Helper()
	b, err := json.Marshal(planfixture.Valid())
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func newImplementPromptServer(t *testing.T) (*Server, *planPromptRunRepo, *planArtifactRepo, *planAuditRepo, *signingFake, *stubIssueGetter) {
	t.Helper()
	rr := newPlanPromptRunRepo()
	ar := newPlanArtifactRepo()
	au := &planAuditRepo{}
	sf := newSigningFake()
	gh := &stubIssueGetter{}
	s := New(Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      rr,
		ArtifactRepo: ar,
		AuditRepo:    au,
		SigningRepo:  sf,
	})
	s.promptIssueGetterOverride = gh
	return s, rr, ar, au, sf, gh
}

// seedRunWithStages helper: seeds a run with plan + implement stages
// and returns their ids so tests can target either one.
func seedRunWithStages(rr *planPromptRunRepo) (runID, planStageID, implStageID uuid.UUID, _ *run.Run) {
	runID = uuid.New()
	planStageID = uuid.New()
	implStageID = uuid.New()
	triggerRef := "issue:42"
	installation := int64(99)
	rn := &run.Run{
		ID:             runID,
		Repo:           "kuhlman-labs/example",
		WorkflowID:     "feature_change",
		TriggerSource:  run.TriggerGitHubIssue,
		TriggerRef:     &triggerRef,
		InstallationID: &installation,
	}
	rr.seedRun(rn)
	rr.seedStages(runID,
		&run.Stage{ID: planStageID, RunID: runID, Type: run.StageTypePlan, Sequence: 0},
		&run.Stage{ID: implStageID, RunID: runID, Type: run.StageTypeImplement, Sequence: 1},
	)
	return runID, planStageID, implStageID, rn
}

func TestImplementPrompt_LeadsWithApprovedPlan(t *testing.T) {
	s, rr, ar, au, sf, gh := newImplementPromptServer(t)
	runID, planStageID, implStageID, _ := seedRunWithStages(rr)

	// Plan artifact stored under the plan stage.
	v := "standard_v1"
	ar.seed(planStageID, &artifact.Artifact{
		ID:            uuid.New(),
		StageID:       planStageID,
		Kind:          artifact.KindPlan,
		SchemaVersion: &v,
		Content:       fixturePlanJSON(t),
		ContentHash:   "deadbeef",
		CreatedAt:     time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC),
	})

	gh.issue = &githubclient.Issue{Number: 42, Title: "Add foo", Body: "We need a foo helper."}

	// Sign the request with the run's stored key (signature path
	// is unchanged from #218; we're just adding plan resolution).
	priv, _ := sf.issue(t, runID)
	w := promptRequestForStage(t, s, runID, implStageID, priv)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
	}
	var resp promptResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Approved plan (binding instruction)",
		"Add a thing.",
		"a.go (create)",
		"1. Do the work.",
		"binding instruction",
		"diverging silently",
		// Implement-stage prompt links the issue (#244): the body
		// is dropped and the agent is told to fetch.
		"Originating issue (link only — fetch if you need detail):",
		"Triggering issue: #42 · Add foo",
		"https://github.com/kuhlman-labs/example/issues/42",
	} {
		if !strings.Contains(resp.Prompt, want) {
			t.Errorf("prompt missing %q\n---\n%s", want, resp.Prompt)
		}
	}
	if strings.Contains(resp.Prompt, "We need a foo helper.") {
		t.Errorf("implement prompt should not include the issue body verbatim:\n%s", resp.Prompt)
	}
	// No plan_missing_for_implement entry — the plan was found.
	for _, e := range au.appended {
		if e.Category == "plan_missing_for_implement" {
			t.Errorf("audit log emitted plan_missing_for_implement when plan was present: %+v", e)
		}
	}
}

func TestImplementPrompt_PicksMostRecentStandardV1Plan(t *testing.T) {
	// Plan stage retried produces multiple plan artifacts; the
	// handler must pick the most-recent. The plan-stage UI's
	// existing "most-recent wins" rule (frontend/routes/stage-detail
	// .tsx) is mirrored here.
	s, rr, ar, _, sf, gh := newImplementPromptServer(t)
	runID, planStageID, implStageID, _ := seedRunWithStages(rr)
	gh.issue = &githubclient.Issue{Number: 42, Title: "Add foo", Body: "ctx"}

	v := "standard_v1"
	older := []byte(`{"plan_version":"standard_v1","summary":"OLDER PLAN","scope":{"files":[]},"approach":[],"verification":{"test_strategy":"x","rollback_plan":"y"},"ticket_reference":{"type":"github_issue","url":"u","id":"i"},"generated_by":{"agent":"a","model":"m","timestamp":"2026-05-07T12:00:00Z"}}`)
	newer := []byte(`{"plan_version":"standard_v1","summary":"NEWER PLAN","scope":{"files":[]},"approach":[],"verification":{"test_strategy":"x","rollback_plan":"y"},"ticket_reference":{"type":"github_issue","url":"u","id":"i"},"generated_by":{"agent":"a","model":"m","timestamp":"2026-05-07T12:00:00Z"}}`)

	ar.seed(planStageID,
		&artifact.Artifact{
			ID: uuid.New(), StageID: planStageID, Kind: artifact.KindPlan, SchemaVersion: &v,
			Content: older, ContentHash: "old", CreatedAt: time.Date(2026, 5, 7, 11, 0, 0, 0, time.UTC),
		},
		&artifact.Artifact{
			ID: uuid.New(), StageID: planStageID, Kind: artifact.KindPlan, SchemaVersion: &v,
			Content: newer, ContentHash: "new", CreatedAt: time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC),
		},
	)

	priv, _ := sf.issue(t, runID)
	w := promptRequestForStage(t, s, runID, implStageID, priv)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var resp promptResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if !strings.Contains(resp.Prompt, "NEWER PLAN") {
		t.Errorf("expected newer plan summary in prompt:\n%s", resp.Prompt)
	}
	if strings.Contains(resp.Prompt, "OLDER PLAN") {
		t.Errorf("older plan leaked into prompt:\n%s", resp.Prompt)
	}
}

func TestImplementPrompt_NoPlan_FallsBackAndAuditsTheGap(t *testing.T) {
	// Plan stage exists but no standard_v1 artifact has been
	// uploaded yet (race between dispatch and upload, or a
	// non-issue-triggered run). The handler returns the
	// issue-only prompt and emits a plan_missing_for_implement
	// audit entry so reviewers can tell the agent worked off the
	// issue rather than an approved plan.
	s, rr, _, au, sf, gh := newImplementPromptServer(t)
	runID, _, implStageID, _ := seedRunWithStages(rr)
	gh.issue = &githubclient.Issue{Number: 42, Title: "Add foo", Body: "context"}

	priv, _ := sf.issue(t, runID)
	w := promptRequestForStage(t, s, runID, implStageID, priv)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
	}
	var resp promptResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if strings.Contains(resp.Prompt, "Approved plan (binding instruction)") {
		t.Errorf("plan section appeared without an artifact:\n%s", resp.Prompt)
	}
	if !strings.Contains(resp.Prompt, "Triggering issue: #42") {
		t.Errorf("issue context missing from fallback prompt:\n%s", resp.Prompt)
	}

	found := false
	for _, e := range au.appended {
		if e.Category == "plan_missing_for_implement" {
			found = true
			if e.StageID == nil || *e.StageID != implStageID {
				t.Errorf("audit StageID = %v, want implement stage %v", e.StageID, implStageID)
			}
		}
	}
	if !found {
		t.Errorf("expected plan_missing_for_implement audit entry; got %+v", au.appended)
	}
}

func TestImplementPrompt_WalksParentRunForRetry(t *testing.T) {
	// A CI-failure retry run (#279 / E16) has no plan stage of its
	// own — variant A skips plan in the retry. The implement-stage
	// prompt builder must walk parent_run_id to find the parent's
	// approved plan; without this the retry would fall back to the
	// issue-only prompt and the audit log would (incorrectly) emit
	// plan_missing_for_implement for every retry.
	s, rr, ar, au, sf, gh := newImplementPromptServer(t)
	parentRunID, parentPlanStageID, _, _ := seedRunWithStages(rr)
	gh.issue = &githubclient.Issue{Number: 42, Title: "Add foo", Body: "ctx"}

	v := "standard_v1"
	ar.seed(parentPlanStageID, &artifact.Artifact{
		ID:            uuid.New(),
		StageID:       parentPlanStageID,
		Kind:          artifact.KindPlan,
		SchemaVersion: &v,
		Content:       fixturePlanJSON(t),
		ContentHash:   "p",
		CreatedAt:     time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC),
	})

	// Seed a retry run pointing at parent, with no plan stage of
	// its own — only implement.
	retryRunID := uuid.New()
	retryImplStageID := uuid.New()
	parentID := parentRunID
	triggerRef := "issue:42"
	installation := int64(99)
	rr.seedRun(&run.Run{
		ID:             retryRunID,
		Repo:           "kuhlman-labs/example",
		WorkflowID:     "feature_change",
		TriggerSource:  run.TriggerGitHubIssue,
		TriggerRef:     &triggerRef,
		InstallationID: &installation,
		ParentRunID:    &parentID,
		RetryAttempt:   1,
	})
	rr.seedStages(retryRunID,
		&run.Stage{ID: retryImplStageID, RunID: retryRunID, Type: run.StageTypeImplement, Sequence: 0},
	)

	priv, _ := sf.issue(t, retryRunID)
	w := promptRequestForStage(t, s, retryRunID, retryImplStageID, priv)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
	}
	var resp promptResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if !strings.Contains(resp.Prompt, "Approved plan (binding instruction)") {
		t.Errorf("retry prompt missed parent's plan:\n%s", resp.Prompt)
	}
	if !strings.Contains(resp.Prompt, "Add a thing.") {
		t.Errorf("retry prompt missed parent plan summary:\n%s", resp.Prompt)
	}
	for _, e := range au.appended {
		if e.Category == "plan_missing_for_implement" {
			t.Errorf("retry path should not emit plan_missing_for_implement when parent had a plan: %+v", e)
		}
	}
}

func TestImplementPrompt_PlanRender_WorksWithoutSignatureRequirement(t *testing.T) {
	// The SPA-readable /prompt-render endpoint resolves the plan
	// the same way as the signature-authed runner endpoint. This
	// is what the implement-stage session view (#215) shows the
	// user — it must reflect the same approved plan.
	s, rr, ar, _, _, gh := newImplementPromptServer(t)
	runID, planStageID, implStageID, _ := seedRunWithStages(rr)
	v := "standard_v1"
	ar.seed(planStageID, &artifact.Artifact{
		ID: uuid.New(), StageID: planStageID, Kind: artifact.KindPlan, SchemaVersion: &v,
		Content:   fixturePlanJSON(t),
		CreatedAt: time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC),
	})
	gh.issue = &githubclient.Issue{Number: 42, Title: "Add foo", Body: "ctx"}
	_ = runID

	req := httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/v0/stages/%s/prompt-render", implStageID), nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
	}
	var resp promptResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if !strings.Contains(resp.Prompt, "Approved plan (binding instruction)") {
		t.Errorf("plan-render path missed the plan section:\n%s", resp.Prompt)
	}
}

func TestImplementPrompt_BothEndpointsProduceIdenticalBody(t *testing.T) {
	// Belt-and-suspenders for the audit story: the signature-authed
	// runner path and the SPA-readable render path must produce
	// byte-identical prompts. If they ever diverge, the audit log's
	// "what the agent saw" record stops matching what reviewers
	// inspect.
	s, rr, ar, _, sf, gh := newImplementPromptServer(t)
	runID, planStageID, implStageID, _ := seedRunWithStages(rr)
	v := "standard_v1"
	ar.seed(planStageID, &artifact.Artifact{
		ID: uuid.New(), StageID: planStageID, Kind: artifact.KindPlan, SchemaVersion: &v,
		Content: fixturePlanJSON(t), CreatedAt: time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC),
	})
	gh.issue = &githubclient.Issue{Number: 42, Title: "Add foo", Body: "context"}

	priv, _ := sf.issue(t, runID)
	signed := promptRequestForStage(t, s, runID, implStageID, priv)
	if signed.Code != http.StatusOK {
		t.Fatalf("signed status = %d", signed.Code)
	}
	rendered := httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/v0/stages/%s/prompt-render", implStageID), nil)
	rw := httptest.NewRecorder()
	s.Handler().ServeHTTP(rw, rendered)
	if rw.Code != http.StatusOK {
		t.Fatalf("rendered status = %d", rw.Code)
	}

	var sb, rb promptResponse
	_ = json.Unmarshal(signed.Body.Bytes(), &sb)
	_ = json.Unmarshal(rw.Body.Bytes(), &rb)
	if sb.Prompt != rb.Prompt {
		t.Errorf("signed vs rendered diverged:\nsigned:\n%s\n---\nrendered:\n%s", sb.Prompt, rb.Prompt)
	}
	if sb.PromptHash != rb.PromptHash {
		t.Errorf("hash diverged: %q vs %q", sb.PromptHash, rb.PromptHash)
	}
}

func TestHandleGetStagePromptRender_BudgetContext(t *testing.T) {
	// Seed a run with a 30m workflow policy and a plan artifact where
	// predicted_runtime_minutes=9. Assert the implement-stage prompt
	// render includes the Budget context section with the expected
	// values, and that the plan-stage prompt does NOT include it.
	s, rr, ar, _, _, gh := newImplementPromptServer(t)
	_, planStageID, implStageID, rn := seedRunWithStages(rr)

	// WorkflowSpec with a 30m policy gives resolveAgentTimeout → 1800s → 30 minutes.
	rn.WorkflowSpec = []byte(planStageSpecYAML30m)

	planJSON, err := json.Marshal(planfixture.Valid(func(m map[string]any) {
		m["predicted_runtime_minutes"] = 9
		m["predicted_runtime_confidence"] = "medium"
	}))
	if err != nil {
		t.Fatal(err)
	}
	v := "standard_v1"
	ar.seed(planStageID, &artifact.Artifact{
		ID:            uuid.New(),
		StageID:       planStageID,
		Kind:          artifact.KindPlan,
		SchemaVersion: &v,
		Content:       planJSON,
		ContentHash:   "budget-ctx-test",
		CreatedAt:     time.Date(2026, 5, 25, 12, 0, 0, 0, time.UTC),
	})
	gh.issue = &githubclient.Issue{Number: 42, Title: "Budget test", Body: "ctx"}

	// Implement stage → expect Budget context section.
	req := httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/v0/stages/%s/prompt-render", implStageID), nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("implement stage status = %d:\n%s", w.Code, w.Body.String())
	}
	var implResp promptResponse
	if err := json.Unmarshal(w.Body.Bytes(), &implResp); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"### Budget context", "9 minutes", "30 minutes"} {
		if !strings.Contains(implResp.Prompt, want) {
			t.Errorf("implement prompt missing %q\n---\n%s", want, implResp.Prompt)
		}
	}

	// Plan stage for the same run → Budget context section must be absent.
	req = httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/v0/stages/%s/prompt-render", planStageID), nil)
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("plan stage status = %d:\n%s", w.Code, w.Body.String())
	}
	var planResp promptResponse
	if err := json.Unmarshal(w.Body.Bytes(), &planResp); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(planResp.Prompt, "### Budget context") {
		t.Errorf("plan-stage prompt should not contain Budget context section:\n%s", planResp.Prompt)
	}
}

func TestHandleGetStagePromptRender_SpecBearingImplementResolvesPolicyTimeout(t *testing.T) {
	// A decomposed child carries the inherited workflow spec (30m policy).
	// Its implement-stage prompt must resolve agent_timeout_seconds to the
	// policy max_stage_runtime (1800s), not the runner's 15m (900s) default.
	// This is the behavioral guard for issue #593: without spec inheritance
	// the prompt layer falls back to 900 and the decomposition budget breaks.
	s, rr, ar, _, _, gh := newImplementPromptServer(t)
	_, planStageID, implStageID, rn := seedRunWithStages(rr)
	rn.WorkflowSpec = []byte(planStageSpecYAML30m)

	v := "standard_v1"
	ar.seed(planStageID, &artifact.Artifact{
		ID:            uuid.New(),
		StageID:       planStageID,
		Kind:          artifact.KindPlan,
		SchemaVersion: &v,
		Content:       fixturePlanJSON(t),
		ContentHash:   "spec-timeout-test",
		CreatedAt:     time.Date(2026, 5, 29, 12, 0, 0, 0, time.UTC),
	})
	gh.issue = &githubclient.Issue{Number: 42, Title: "Timeout test", Body: "ctx"}

	req := httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/v0/stages/%s/prompt-render", implStageID), nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("implement stage status = %d:\n%s", w.Code, w.Body.String())
	}
	var resp promptResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.AgentTimeoutSeconds != 1800 {
		t.Errorf("agent_timeout_seconds = %d, want 1800 (policy max_stage_runtime, not the 900s runner default)", resp.AgentTimeoutSeconds)
	}
}

func TestImplementPrompt_ApprovalConditions_Rendered(t *testing.T) {
	// Seed a run with an approval_submitted entry carrying decision=approve
	// and a non-empty comment. The implement-stage prompt must contain the
	// "Approval conditions" section with the comment verbatim.
	s, rr, ar, au, sf, gh := newImplementPromptServer(t)
	runID, planStageID, implStageID, _ := seedRunWithStages(rr)

	v := "standard_v1"
	ar.seed(planStageID, &artifact.Artifact{
		ID:            uuid.New(),
		StageID:       planStageID,
		Kind:          artifact.KindPlan,
		SchemaVersion: &v,
		Content:       fixturePlanJSON(t),
		ContentHash:   "conditions-test",
		CreatedAt:     time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC),
	})
	gh.issue = &githubclient.Issue{Number: 42, Title: "Add foo", Body: "ctx"}

	approveComment := "add the cross-branch rejection test"
	payload, _ := json.Marshal(map[string]any{
		"decision": "approve",
		"comment":  approveComment,
	})
	au.seedByCategory(runID, "approval_submitted", &audit.Entry{
		ID:       uuid.New(),
		Payload:  payload,
		Category: "approval_submitted",
	})

	priv, _ := sf.issue(t, runID)
	w := promptRequestForStage(t, s, runID, implStageID, priv)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
	}
	var resp promptResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Approval conditions", approveComment} {
		if !strings.Contains(resp.Prompt, want) {
			t.Errorf("prompt missing %q\n---\n%s", want, resp.Prompt)
		}
	}
}

func TestImplementPrompt_ApprovalConditions_AbsentWhenNoComment(t *testing.T) {
	// No approval_submitted entry with a comment → "Approval conditions"
	// section must not appear.
	s, rr, ar, _, sf, gh := newImplementPromptServer(t)
	runID, planStageID, implStageID, _ := seedRunWithStages(rr)

	v := "standard_v1"
	ar.seed(planStageID, &artifact.Artifact{
		ID:            uuid.New(),
		StageID:       planStageID,
		Kind:          artifact.KindPlan,
		SchemaVersion: &v,
		Content:       fixturePlanJSON(t),
		ContentHash:   "no-conditions-test",
		CreatedAt:     time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC),
	})
	gh.issue = &githubclient.Issue{Number: 42, Title: "Add foo", Body: "ctx"}

	priv, _ := sf.issue(t, runID)
	w := promptRequestForStage(t, s, runID, implStageID, priv)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
	}
	var resp promptResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(resp.Prompt, "Approval conditions") {
		t.Errorf("Approval conditions section should not appear when no approve comment exists:\n%s", resp.Prompt)
	}
}

// promptRequestForStage signs a GET /v0/stages/{id}/prompt request
// with the run's private key (mirroring the runner's behavior).
// Reuses the canonical signing message helper so the signing stays
// in lockstep with the production path.
func promptRequestForStage(t *testing.T, s *Server, _ uuid.UUID, stageID uuid.UUID, priv ed25519.PrivateKey) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/v0/stages/%s/prompt", stageID), nil)
	sig := ed25519.Sign(priv, PromptCanonicalMessage(stageID))
	req.Header.Set("X-Fishhawk-Signature", hex.EncodeToString(sig))
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	return w
}
