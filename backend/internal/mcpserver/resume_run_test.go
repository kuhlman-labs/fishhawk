package mcpserver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/apitoken"
	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/failuresig"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	planpkg "github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	runmodel "github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/scopeamendment"
	"github.com/kuhlman-labs/fishhawk/backend/internal/server"
	"github.com/kuhlman-labs/fishhawk/backend/internal/signing"
)

// --- fishhawk_resume_run (#978) ---

func TestResumeRun_HappyPath_PostsBodyReturnsRun(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	parentID := uuid.New()
	_, out, err := r.resumeRun(context.Background(), nil, ResumeRunInput{
		ParentRunID: parentID.String(),
		AddScopeFiles: []RecoverScopePath{
			{Path: "docs/extra.md"},
			{Path: "backend/new_file.go", Operation: "create"},
		},
		Reason: "fold the dropped doc companion",
	})
	if err != nil {
		t.Fatalf("resumeRun: %v", err)
	}
	if out.Run.ID == "" {
		t.Errorf("Run.ID empty; expected the fake to allocate one")
	}
	if out.Run.ParentRunID == nil || *out.Run.ParentRunID != parentID.String() {
		t.Errorf("Run.ParentRunID = %v, want %s", out.Run.ParentRunID, parentID)
	}
	if out.Idempotent {
		t.Errorf("Idempotent = true, want false (fresh create returns 201)")
	}
	if fb.recoverParentID != parentID {
		t.Errorf("backend got parent run id %s, want %s", fb.recoverParentID, parentID)
	}
	if len(fb.recoverBody.AddScopeFiles) != 2 ||
		fb.recoverBody.AddScopeFiles[0].Path != "docs/extra.md" ||
		fb.recoverBody.AddScopeFiles[1].Operation != "create" {
		t.Errorf("backend got AddScopeFiles = %+v", fb.recoverBody.AddScopeFiles)
	}
	if fb.recoverBody.Reason != "fold the dropped doc companion" {
		t.Errorf("backend got Reason = %q", fb.recoverBody.Reason)
	}
	if fb.recoverIdempKey != "" {
		t.Errorf("Idempotency-Key set without input: %q", fb.recoverIdempKey)
	}
}

// TestResumeRun_ExemptScopeFiles_RoundTrips pins the #1229 exempt_scope_files
// lever: the MCP input's ExemptScopeFiles reaches the backend recover request
// body byte-for-byte (path + reason), alongside add_scope_files.
func TestResumeRun_ExemptScopeFiles_RoundTrips(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	parentID := uuid.New()
	_, _, err := r.resumeRun(context.Background(), nil, ResumeRunInput{
		ParentRunID: parentID.String(),
		ExemptScopeFiles: []RecoverExemptPath{
			{Path: "backend/internal/server/handlers.go", Reason: "no change needed on this slice"},
		},
		Reason: "recover with the declared file left unchanged",
	})
	if err != nil {
		t.Fatalf("resumeRun: %v", err)
	}
	if len(fb.recoverBody.ExemptScopeFiles) != 1 ||
		fb.recoverBody.ExemptScopeFiles[0].Path != "backend/internal/server/handlers.go" ||
		fb.recoverBody.ExemptScopeFiles[0].Reason != "no change needed on this slice" {
		t.Errorf("backend got ExemptScopeFiles = %+v", fb.recoverBody.ExemptScopeFiles)
	}
}

func TestResumeRun_InvalidUUID_FailsLocally(t *testing.T) {
	_, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.resumeRun(context.Background(), nil, ResumeRunInput{ParentRunID: "not-a-uuid"})
	if err == nil || !strings.Contains(err.Error(), "not a valid UUID") {
		t.Fatalf("err = %v, want local UUID validation error", err)
	}
}

func TestResumeRun_IdempotencyKey_SetsHeaderAndFlagsReplay(t *testing.T) {
	fb, srv := newFakeBackend(t)
	fb.recoverStatus = http.StatusOK // backend signals idempotent replay
	r := newResolver(srv, nil)

	_, out, err := r.resumeRun(context.Background(), nil, ResumeRunInput{
		ParentRunID:    uuid.NewString(),
		IdempotencyKey: "recover-once",
	})
	if err != nil {
		t.Fatalf("resumeRun: %v", err)
	}
	if fb.recoverIdempKey != "recover-once" {
		t.Errorf("Idempotency-Key header = %q, want recover-once", fb.recoverIdempKey)
	}
	if !out.Idempotent {
		t.Errorf("Idempotent = false, want true on a 200 replay")
	}
}

func TestResumeRun_NotEligible_MapsActionableError(t *testing.T) {
	fb, srv := newFakeBackend(t)
	fb.recoverStatus = http.StatusConflict
	fb.recoverErrBody = `{"error":{"code":"recovery_not_eligible","message":"recovery requires a succeeded plan stage and an implement stage failed category-B","details":{"plan_state":"succeeded","implement_state":"failed","failure_category":"A"}}}`
	r := newResolver(srv, nil)

	_, _, err := r.resumeRun(context.Background(), nil, ResumeRunInput{ParentRunID: uuid.NewString()})
	if err == nil {
		t.Fatal("err = nil, want recovery_not_eligible mapping")
	}
	for _, want := range []string{"recovery_not_eligible", "failure_category=A", "fishhawk_retry_stage"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err %q missing %q", err.Error(), want)
		}
	}
}

// TestResumeRun_NotEligible_MentionsDecompositionChild pins the
// slice-2 messaging: the recovery_not_eligible mapping explains BOTH
// the top-level and the in-place decomposition-child eligibility legs,
// and surfaces the plan_resolved detail the child branch returns.
func TestResumeRun_NotEligible_MentionsDecompositionChild(t *testing.T) {
	fb, srv := newFakeBackend(t)
	fb.recoverStatus = http.StatusConflict
	fb.recoverErrBody = `{"error":{"code":"recovery_not_eligible","message":"in-place recovery of a decomposition child requires the child's own implement stage failed category-B and an approved plan resolvable via the parent walk","details":{"implement_state":"failed","failure_category":"B","plan_resolved":false}}}`
	r := newResolver(srv, nil)

	_, _, err := r.resumeRun(context.Background(), nil, ResumeRunInput{ParentRunID: uuid.NewString()})
	if err == nil {
		t.Fatal("err = nil, want recovery_not_eligible mapping")
	}
	for _, want := range []string{"decomposition-child", "plan_resolved=false", "in-place"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err %q missing %q", err.Error(), want)
		}
	}
}

// TestResumeRun_NotEligible_SucceededChildSliceConflict_SurfacesRecovery pins
// the #1669 recovery-gap fix: when the decomposition child's OWN implement
// SUCCEEDED (implement_state=succeeded), the ineligibility is a parent
// slice_integration_conflict — not the child-failed-category-B dead-end the
// generic message describes. The mapping must name the WORKING recovery
// (reset_run_branch + re-drive, or abandon + fresh run) instead of the generic
// "requires the CHILD's implement FAILED category-B" text.
func TestResumeRun_NotEligible_SucceededChildSliceConflict_SurfacesRecovery(t *testing.T) {
	fb, srv := newFakeBackend(t)
	fb.recoverStatus = http.StatusConflict
	fb.recoverErrBody = `{"error":{"code":"recovery_not_eligible","message":"in-place recovery of a decomposition child requires the child's own implement stage failed category-B and an approved plan resolvable via the parent walk","details":{"implement_state":"succeeded","failure_category":"","plan_resolved":true}}}`
	r := newResolver(srv, nil)

	_, _, err := r.resumeRun(context.Background(), nil, ResumeRunInput{ParentRunID: uuid.NewString()})
	if err == nil {
		t.Fatal("err = nil, want recovery_not_eligible mapping")
	}
	for _, want := range []string{
		"recovery_not_eligible",
		"implement_state=succeeded",
		"slice_integration_conflict",
		"fishhawk_reset_run_branch",
		"fishhawk_start_run",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err %q missing %q", err.Error(), want)
		}
	}
	// The dead-end child-failed guidance must NOT be surfaced for this disposition.
	if strings.Contains(err.Error(), "requires the CHILD's own implement stage FAILED category-B") {
		t.Errorf("succeeded-child disposition must not surface the child-failed dead-end guidance: %q", err.Error())
	}
}

func TestResumeRun_Unsupported_MapsActionableError(t *testing.T) {
	fb, srv := newFakeBackend(t)
	fb.recoverStatus = http.StatusUnprocessableEntity
	fb.recoverErrBody = `{"error":{"code":"recovery_unsupported","message":"parent run has no cached workflow spec; start a fresh run instead"}}`
	r := newResolver(srv, nil)

	_, _, err := r.resumeRun(context.Background(), nil, ResumeRunInput{ParentRunID: uuid.NewString()})
	if err == nil || !strings.Contains(err.Error(), "fishhawk_start_run") {
		t.Fatalf("err = %v, want recovery_unsupported mapping pointing at fishhawk_start_run", err)
	}
}

func TestResumeRun_NotFound_MapsActionableError(t *testing.T) {
	fb, srv := newFakeBackend(t)
	fb.recoverStatus = http.StatusNotFound
	fb.recoverErrBody = `{"error":{"code":"run_not_found","message":"no run with that id"}}`
	r := newResolver(srv, nil)

	_, _, err := r.resumeRun(context.Background(), nil, ResumeRunInput{ParentRunID: uuid.NewString()})
	if err == nil || !strings.Contains(err.Error(), "fishhawk_list_runs") {
		t.Fatalf("err = %v, want run_not_found mapping pointing at fishhawk_list_runs", err)
	}
}

// TestResumeRun_OversizedRow_BoundedThroughHandler drives the recovery verb
// with an oversized run row (#2510): the recovery run is already minted when
// this response renders, so the row is reduced rather than the response
// rejected.
func TestResumeRun_OversizedRow_BoundedThroughHandler(t *testing.T) {
	fb, srv := newFakeBackend(t)
	parentID := uuid.New()
	childID := uuid.NewString()
	fb.recoverResp = worstCaseIssueRun(childID)

	raw := callBoundedToolOverSDK(t, srv, nil, registerResumeRun, "fishhawk_resume_run",
		map[string]any{"parent_run_id": parentID.String()})

	var out ResumeRunOutput
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	assertBoundedWithElisions(t, "fishhawk_resume_run", raw, out.Elisions, mcpResponseByteBudgetDefault)
	if out.Run.ID != childID {
		t.Errorf("the recovery run id was lost to the bound: %+v", out.Run)
	}
}

// --- E72.62 / #4081: decomposed-parent refusal -----------------------------

// TestResumeRun_UnsupportedDecomposed_MapsRestartActions: the backend's 422
// resume_unsupported_decomposed maps to an actionable error naming the
// restart verbs. The fake's message deliberately names NO tool and carries no
// details, so only the mapping arm can put the verbs in the error.
func TestResumeRun_UnsupportedDecomposed_MapsRestartActions(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	fb.mu.Lock()
	fb.recoverStatus = http.StatusUnprocessableEntity
	fb.recoverErrBody = `{"error":{"code":"resume_unsupported_decomposed","message":"the run is a decomposed parent"}}`
	fb.mu.Unlock()

	_, _, err := r.resumeRun(context.Background(), nil, ResumeRunInput{ParentRunID: uuid.NewString()})
	if err == nil {
		t.Fatal("resumeRun succeeded, want the resume_unsupported_decomposed refusal")
	}
	msg := err.Error()
	for _, want := range []string{
		"resume_unsupported_decomposed",
		"the run is a decomposed parent",
		"fishhawk_start_campaign_item_run",
		"fishhawk_start_run",
		"re-state the prior plan approval's conditions",
		"decomposition child's own run id",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error does not carry %q:\n%s", want, msg)
		}
	}
}

// recoverE2ESpecYAML is a plan+implement workflow the real recover handler
// parses to re-create the non-plan stages on a minted child.
const recoverE2ESpecYAML = `version: "0.4"
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

// seedRecoverE2EParent persists, in real Postgres, a top-level run with the
// cached spec, a SUCCEEDED plan stage carrying a standard_v1 plan artifact
// (decomposed into subPlans sub_plans when subPlans > 0) and an implement
// stage FAILED category-B with the given reason — the fully eligible recover
// shape. Returns the run id.
func seedRecoverE2EParent(t *testing.T, ctx context.Context, runRepo runmodel.Repository, artRepo artifact.Repository, subPlans int, reason string) uuid.UUID {
	t.Helper()
	row, err := runRepo.CreateRun(ctx, runmodel.CreateRunParams{
		Repo: "x/y", WorkflowID: "feature_change", WorkflowSHA: "abc", TriggerSource: runmodel.TriggerCLI,
		WorkflowSpec: []byte(recoverE2ESpecYAML),
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	newStage := func(seq int, typ runmodel.StageType, final runmodel.StageState, done *runmodel.StageCompletion) *runmodel.Stage {
		t.Helper()
		st, err := runRepo.CreateStage(ctx, runmodel.CreateStageParams{
			RunID: row.ID, Sequence: seq, Type: typ, ExecutorKind: runmodel.ExecutorAgent, ExecutorRef: "claude-code",
		})
		if err != nil {
			t.Fatalf("create %s stage: %v", typ, err)
		}
		for _, to := range []runmodel.StageState{runmodel.StageStateDispatched, runmodel.StageStateRunning} {
			if _, err := runRepo.TransitionStage(ctx, st.ID, to, nil); err != nil {
				t.Fatalf("transition %s stage to %s: %v", typ, to, err)
			}
		}
		if _, err := runRepo.TransitionStage(ctx, st.ID, final, done); err != nil {
			t.Fatalf("transition %s stage to %s: %v", typ, final, err)
		}
		return st
	}
	planStage := newStage(0, runmodel.StageTypePlan, runmodel.StageStateSucceeded, nil)
	catB := runmodel.FailureB
	newStage(1, runmodel.StageTypeImplement, runmodel.StageStateFailed,
		&runmodel.StageCompletion{FailureCategory: &catB, FailureReason: &reason})

	p := planpkg.Plan{
		PlanVersion:  "standard_v1",
		Summary:      "e2e recover plan",
		Verification: planpkg.Verification{TestStrategy: "ts", RollbackPlan: "rb"},
		Scope: planpkg.Scope{
			Files: []planpkg.ScopeFile{{Path: "backend/internal/server/handlers.go", Operation: planpkg.FileOpModify}},
		},
	}
	if subPlans > 0 {
		p.Decomposition = &planpkg.Decomposition{Rationale: "too big for one implement timeout"}
		for i := 0; i < subPlans; i++ {
			p.Decomposition.SubPlans = append(p.Decomposition.SubPlans, planpkg.SubPlanSummary{
				Title: "slice " + string(rune('A'+i)), ScopeHint: "backend/internal/server",
				PredictedRuntimeMinutes: 30, PredictedRuntimeConfidence: planpkg.RuntimeConfidenceMedium,
			})
		}
	}
	body, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	sum := sha256.Sum256(body)
	sv := "standard_v1"
	if _, err := artRepo.Create(ctx, artifact.CreateParams{
		StageID: planStage.ID, Kind: artifact.KindPlan, SchemaVersion: &sv,
		Content: body, ContentHash: hex.EncodeToString(sum[:]),
	}); err != nil {
		t.Fatalf("create plan artifact: %v", err)
	}
	return row.ID
}

// childrenOf lists the runs whose ParentRunID is parent, read back from Postgres.
func childrenOf(t *testing.T, ctx context.Context, runRepo runmodel.Repository, parent uuid.UUID) []*runmodel.Run {
	t.Helper()
	rows, err := runRepo.ListRuns(ctx, runmodel.ListRunsFilter{Limit: 100})
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	var out []*runmodel.Run
	for _, r := range rows {
		if r.ParentRunID != nil && *r.ParentRunID == parent {
			out = append(out, r)
		}
	}
	return out
}

// TestResumeRun_DecomposedParentRefusal_EndToEnd is the cross-boundary test:
// MCP tool -> real HTTP client -> real route + auth (requireWriteScope,
// requireRunAccount) -> handleRecoverRun -> plan resolution over the real
// artifact store -> run persistence in real Postgres. The decomposed parent's
// call is refused with resume_unsupported_decomposed and NO child run with
// ParentRunID = that parent exists afterwards; the flat control parent's call
// through the IDENTICAL wiring SUCCEEDS and mints a child, so the decomposed
// arm's refusal cannot be a 401/403/404 masking it.
func TestResumeRun_DecomposedParentRefusal_EndToEnd(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	runRepo := runmodel.NewPostgresRepository(pool)
	artRepo := artifact.NewPostgresRepository(pool)
	auditRepo := audit.NewPostgresRepository(pool)

	giveUp := failuresig.AnchorSliceIntegrationGiveUp + " 5 attempts: seeded"
	decomposed := seedRecoverE2EParent(t, ctx, runRepo, artRepo, 2, giveUp)
	control := seedRecoverE2EParent(t, ctx, runRepo, artRepo, 0, "undeclared created file")

	const bearer = "fhk_resume_decomposed_e2e"
	tokRepo := &stubMCPAPITokens{tok: &apitoken.Token{
		ID: uuid.New(), Subject: "github:op", Scopes: []string{"read:runs", "read:audit", "write:runs"}, PlainText: bearer,
	}}
	srv := server.New(server.Config{
		RunRepo: runRepo, ArtifactRepo: artRepo, AuditRepo: auditRepo,
		SigningRepo: signing.NewPostgresRepository(pool), APITokenRepo: tokRepo,
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	r := &runResolver{api: newAPIClient(config{backendURL: ts.URL, apiToken: bearer}), getenv: envFuncFromMap(nil)}

	// Decomposed parent: refused, nothing minted.
	// Errorf, not Fatal: on a regression the no-child assertion below (C4)
	// must still run and report the minted flat run.
	_, _, err := r.resumeRun(ctx, nil, ResumeRunInput{ParentRunID: decomposed.String()})
	if err == nil {
		t.Errorf("resume of the decomposed parent succeeded, want resume_unsupported_decomposed")
	} else {
		for _, want := range []string{"resume_unsupported_decomposed", "fishhawk_start_campaign_item_run", "fishhawk_start_run", "sub_plan_count=2"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("decomposed refusal does not carry %q:\n%v", want, err)
			}
		}
	}
	if kids := childrenOf(t, ctx, runRepo, decomposed); len(kids) != 0 {
		t.Errorf("runs with ParentRunID = decomposed parent = %d, want 0 (a flat run was minted)", len(kids))
	}

	// Flat control parent: the same wiring mints a child.
	_, out, err := r.resumeRun(ctx, nil, ResumeRunInput{ParentRunID: control.String()})
	if err != nil {
		t.Fatalf("resume of the flat control parent: %v", err)
	}
	if out.Run.ParentRunID == nil || *out.Run.ParentRunID != control.String() {
		t.Errorf("control child ParentRunID = %v, want %s", out.Run.ParentRunID, control)
	}
	if kids := childrenOf(t, ctx, runRepo, control); len(kids) != 1 {
		t.Errorf("runs with ParentRunID = control parent = %d, want 1", len(kids))
	}
}

// --- #4798: the slice-ownership refusal through the real client ---------------

// failNthListArtifacts fails only the Nth ListForStage call on a real repository.
// The eligibility block's plan read comes first, so the SECOND call is the guard's.
type failNthListArtifacts struct {
	artifact.Repository
	n     int
	calls int
	err   error
}

func (f *failNthListArtifacts) ListForStage(ctx context.Context, stageID uuid.UUID) ([]*artifact.Artifact, error) {
	f.calls++
	if f.calls == f.n {
		return nil, f.err
	}
	return f.Repository.ListForStage(ctx, stageID)
}

// resumeOwnershipE2E is a real-Postgres decomposition: slice 0 {docs/a.md}, slice 1
// {docs/b.md}, slice 2 {pkg/c.go} depends_on [0], with the requester at slice 2
// failed category-B. A nil decomposition leaves the plan flat.
type resumeOwnershipE2E struct {
	child    *runmodel.Run
	impl     *runmodel.Stage
	sibling1 *runmodel.Run
}

func seedResumeOwnershipE2E(t *testing.T, ctx context.Context, runRepo runmodel.Repository, artRepo artifact.Repository, decomposed bool) resumeOwnershipE2E {
	t.Helper()
	mint := func(parent *uuid.UUID, idx *int) *runmodel.Run {
		t.Helper()
		r, err := runRepo.CreateRun(ctx, runmodel.CreateRunParams{
			Repo: "x/y", WorkflowID: "feature_change", WorkflowSHA: "abc", TriggerSource: runmodel.TriggerCLI,
			WorkflowSpec: []byte(recoverE2ESpecYAML), ParentRunID: parent, DecomposedFrom: parent, SliceIndex: idx,
		})
		if err != nil {
			t.Fatalf("create run: %v", err)
		}
		return r
	}
	walk := func(id uuid.UUID, states []runmodel.StageState, last *runmodel.StageCompletion) {
		t.Helper()
		for i, to := range states {
			var c *runmodel.StageCompletion
			if i == len(states)-1 {
				c = last
			}
			if _, err := runRepo.TransitionStage(ctx, id, to, c); err != nil {
				t.Fatalf("transition stage to %s: %v", to, err)
			}
		}
	}
	newStage := func(runID uuid.UUID, seq int, typ runmodel.StageType) *runmodel.Stage {
		t.Helper()
		st, err := runRepo.CreateStage(ctx, runmodel.CreateStageParams{
			RunID: runID, Sequence: seq, Type: typ, ExecutorKind: runmodel.ExecutorAgent, ExecutorRef: "claude-code",
		})
		if err != nil {
			t.Fatalf("create stage: %v", err)
		}
		return st
	}

	parent := mint(nil, nil)
	planStage := newStage(parent.ID, 0, runmodel.StageTypePlan)
	walk(planStage.ID, []runmodel.StageState{runmodel.StageStateDispatched, runmodel.StageStateRunning, runmodel.StageStateSucceeded}, nil)

	scope := func(path string) *planpkg.Scope {
		return &planpkg.Scope{Files: []planpkg.ScopeFile{{Path: path, Operation: planpkg.FileOpModify}}}
	}
	p := planpkg.Plan{
		PlanVersion: "standard_v1", Summary: "s",
		Verification: planpkg.Verification{TestStrategy: "ts", RollbackPlan: "rb"},
	}
	if decomposed {
		p.Decomposition = &planpkg.Decomposition{Rationale: "split", SubPlans: []planpkg.SubPlanSummary{
			{Title: "a", ScopeHint: "docs", Scope: scope("docs/a.md")},
			{Title: "b", ScopeHint: "docs", Scope: scope("docs/b.md")},
			{Title: "c", ScopeHint: "pkg", Scope: scope("pkg/c.go"), DependsOn: []int{0}},
		}}
	}
	body, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	sv := "standard_v1"
	if _, err := artRepo.Create(ctx, artifact.CreateParams{
		StageID: planStage.ID, Kind: artifact.KindPlan, SchemaVersion: &sv, Content: body, ContentHash: hex.EncodeToString(sum[:]),
	}); err != nil {
		t.Fatalf("create plan artifact: %v", err)
	}

	pid := parent.ID
	var out resumeOwnershipE2E
	for i := 0; i < 3; i++ {
		idx := i
		c := mint(&pid, &idx)
		switch i {
		case 1:
			out.sibling1 = c
		case 2:
			out.child = c
		}
	}
	for _, to := range []runmodel.State{runmodel.StateRunning, runmodel.StateFailed} {
		if _, err := runRepo.TransitionRun(ctx, out.child.ID, to); err != nil {
			t.Fatalf("transition run to %s: %v", to, err)
		}
	}
	out.impl = newStage(out.child.ID, 1, runmodel.StageTypeImplement)
	catB := runmodel.FailureB
	reason := "scope violation"
	walk(out.impl.ID, []runmodel.StageState{runmodel.StageStateDispatched, runmodel.StageStateRunning, runmodel.StageStateFailed},
		&runmodel.StageCompletion{FailureCategory: &catB, FailureReason: &reason})
	return out
}

// TestResumeRun_SliceOwnershipRefusal_EndToEnd crosses the MCP tool, the real API
// client, the real HTTP handler and real Postgres run, artifact, amendment and
// audit repositories. The ancestor control goes through identical wiring, so the
// refusals cannot be a 401/403 masking them.
func TestResumeRun_SliceOwnershipRefusal_EndToEnd(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	runRepo := runmodel.NewPostgresRepository(pool)
	artRepo := artifact.NewPostgresRepository(pool)
	auditRepo := audit.NewPostgresRepository(pool)
	amendRepo := scopeamendment.NewPostgresRepository(pool)

	const bearer = "fhk_resume_ownership_e2e"
	tokRepo := &stubMCPAPITokens{tok: &apitoken.Token{
		ID: uuid.New(), Subject: "github:op", Scopes: []string{"read:runs", "read:audit", "write:runs"}, PlainText: bearer,
	}}
	resolverFor := func(t *testing.T, arts artifact.Repository) *runResolver {
		t.Helper()
		srv := server.New(server.Config{
			RunRepo: runRepo, ArtifactRepo: arts, AuditRepo: auditRepo, ScopeAmendmentRepo: amendRepo,
			SigningRepo: signing.NewPostgresRepository(pool), APITokenRepo: tokRepo,
		})
		ts := httptest.NewServer(srv.Handler())
		t.Cleanup(ts.Close)
		return &runResolver{api: newAPIClient(config{backendURL: ts.URL, apiToken: bearer}), getenv: envFuncFromMap(nil)}
	}
	assertStillFailed := func(t *testing.T, seeded resumeOwnershipE2E) {
		t.Helper()
		gotRun, err := runRepo.GetRun(ctx, seeded.child.ID)
		if err != nil {
			t.Fatal(err)
		}
		if gotRun.State != runmodel.StateFailed {
			t.Errorf("child run state = %q, want still failed", gotRun.State)
		}
		gotStage, err := runRepo.GetStage(ctx, seeded.impl.ID)
		if err != nil {
			t.Fatal(err)
		}
		if gotStage.State != runmodel.StageStateFailed {
			t.Errorf("implement stage state = %q, want still failed", gotStage.State)
		}
		rows, err := amendRepo.ListByRun(ctx, seeded.child.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 0 {
			t.Errorf("%d amendment rows, want 0", len(rows))
		}
	}
	apiErrOf := func(t *testing.T, err error) *apiError {
		t.Helper()
		var ae *apiError
		if !errors.As(err, &ae) {
			t.Fatalf("err = %v, want an *apiError through the chain", err)
		}
		return ae
	}
	resume := func(t *testing.T, r *runResolver, child uuid.UUID, path string) error {
		t.Helper()
		_, _, err := r.resumeRun(ctx, nil, ResumeRunInput{
			ParentRunID: child.String(), Reason: "resume", AddScopeFiles: []RecoverScopePath{{Path: path}},
		})
		return err
	}

	t.Run("non_ancestor", func(t *testing.T) {
		seeded := seedResumeOwnershipE2E(t, ctx, runRepo, artRepo, true)
		ae := apiErrOf(t, resume(t, resolverFor(t, artRepo), seeded.child.ID, "docs/b.md"))
		if ae.Code != "amendment_path_owned_by_sibling_slice" || ae.StatusCode != http.StatusConflict {
			t.Fatalf("got %d %q, want 409 amendment_path_owned_by_sibling_slice: %v", ae.StatusCode, ae.Code, ae)
		}
		conflicts, _ := ae.Details["conflicts"].([]any)
		if len(conflicts) == 0 {
			t.Fatalf("details.conflicts empty: %v", ae.Details)
		}
		first, _ := conflicts[0].(map[string]any)
		if first["owning_slice"] != float64(1) || first["owning_child_run_id"] != seeded.sibling1.ID.String() {
			t.Errorf("conflicts[0] = %v, want slice 1 and child %s", first, seeded.sibling1.ID)
		}
		if remedy, _ := ae.Details["remedy"].(string); remedy == "" {
			t.Errorf("details.remedy empty: %v", ae.Details)
		}
		if ae.Details["retryable"] != false {
			t.Errorf("details.retryable = %v, want false", ae.Details["retryable"])
		}
		assertStillFailed(t, seeded)
		entries, err := auditRepo.ListForRunByCategory(ctx, seeded.child.ID, server.CategoryScopeAmendmentRefused)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 1 {
			t.Errorf("%d scope_amendment_refused chain rows, want 1", len(entries))
		}
	})

	t.Run("no_decomposition", func(t *testing.T) {
		seeded := seedResumeOwnershipE2E(t, ctx, runRepo, artRepo, false)
		ae := apiErrOf(t, resume(t, resolverFor(t, artRepo), seeded.child.ID, "docs/a.md"))
		if ae.Code != "slice_ownership_check_failed" || ae.StatusCode != http.StatusInternalServerError {
			t.Fatalf("got %d %q: %v", ae.StatusCode, ae.Code, ae)
		}
		if ae.Details["retryable"] != false || ae.Details["reason"] != "decomposition_absent" {
			t.Errorf("details = %v, want decomposition_absent not retryable", ae.Details)
		}
		if remedy, _ := ae.Details["remedy"].(string); remedy == "" {
			t.Errorf("details.remedy empty: %v", ae.Details)
		}
		assertStillFailed(t, seeded)
	})

	t.Run("transient", func(t *testing.T) {
		seeded := seedResumeOwnershipE2E(t, ctx, runRepo, artRepo, true)
		const cause = "resume e2e plan read offline sentinel"
		arts := &failNthListArtifacts{Repository: artRepo, n: 2, err: errors.New(cause)}
		ae := apiErrOf(t, resume(t, resolverFor(t, arts), seeded.child.ID, "docs/a.md"))
		if ae.Code != "slice_ownership_check_failed" || ae.StatusCode != http.StatusInternalServerError {
			t.Fatalf("got %d %q: %v", ae.StatusCode, ae.Code, ae)
		}
		if ae.Details["retryable"] != true || ae.Details["reason"] != "parent_plan_unreadable" {
			t.Errorf("details = %v, want parent_plan_unreadable retryable", ae.Details)
		}
		if ae.ErrorRef == "" {
			t.Error("a 500 must carry a non-empty error_ref")
		}
		if strings.Contains(ae.Error(), cause) {
			t.Errorf("raw cause reached the caller: %v", ae)
		}
		assertStillFailed(t, seeded)
	})

	t.Run("ancestor_control", func(t *testing.T) {
		seeded := seedResumeOwnershipE2E(t, ctx, runRepo, artRepo, true)
		if err := resume(t, resolverFor(t, artRepo), seeded.child.ID, "docs/a.md"); err != nil {
			t.Fatalf("resume naming an ancestor's path: %v", err)
		}
		gotRun, err := runRepo.GetRun(ctx, seeded.child.ID)
		if err != nil {
			t.Fatal(err)
		}
		if gotRun.State != runmodel.StateRunning {
			t.Errorf("child run state = %q, want re-opened", gotRun.State)
		}
		rows, err := amendRepo.ListByRun(ctx, seeded.child.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 || rows[0].Status != scopeamendment.StatusApproved {
			t.Errorf("rows = %+v, want one approved", rows)
		}
	})
}
