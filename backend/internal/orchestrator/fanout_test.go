package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/drive"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// fanoutRunsRepo extends the behaviour of stubRuns (run.Repository
// stub) to support CreateRun + CreateStage + ListStagesAwaitingChildren
// which the fanout path exercises. The base stubRuns marks those
// methods as "not used"; fanoutRunsRepo embeds it and overrides them
// to record creations so the test can assert on them.
type fanoutRunsRepo struct {
	*stubRuns
	mu sync.Mutex

	createdRuns   []*run.Run
	createdStages []*run.Stage
	listFilters   []run.ListRunsFilter
	listResult    []*run.Run
}

func newFanoutRunsRepo() *fanoutRunsRepo {
	return &fanoutRunsRepo{stubRuns: newStubRuns()}
}

func (r *fanoutRunsRepo) CreateRun(_ context.Context, p run.CreateRunParams) (*run.Run, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rr := &run.Run{
		ID:             uuid.New(),
		Repo:           p.Repo,
		WorkflowID:     p.WorkflowID,
		WorkflowSHA:    p.WorkflowSHA,
		TriggerSource:  p.TriggerSource,
		TriggerRef:     p.TriggerRef,
		InstallationID: p.InstallationID,
		ParentRunID:    p.ParentRunID,
		DecomposedFrom: p.DecomposedFrom,
		SliceIndex:     p.SliceIndex,
		RunnerKind:     p.RunnerKind,
		IssueContext:   p.IssueContext,
		WorkflowSpec:   p.WorkflowSpec,
		State:          run.StatePending,
		CreatedAt:      time.Now().UTC(),
		UpdatedAt:      time.Now().UTC(),
	}
	r.createdRuns = append(r.createdRuns, rr)
	r.stubRuns.mu.Lock()
	r.runs[rr.ID] = rr
	r.stubRuns.mu.Unlock()
	return rr, nil
}

func (r *fanoutRunsRepo) CreateStage(_ context.Context, p run.CreateStageParams) (*run.Stage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := &run.Stage{
		ID:           uuid.New(),
		RunID:        p.RunID,
		Sequence:     p.Sequence,
		Type:         p.Type,
		ExecutorKind: p.ExecutorKind,
		ExecutorRef:  p.ExecutorRef,
		State:        run.StageStatePending,
		CreatedAt:    time.Now().UTC(),
		UpdatedAt:    time.Now().UTC(),
	}
	r.createdStages = append(r.createdStages, st)
	r.stubRuns.mu.Lock()
	r.stages[p.RunID] = append(r.stages[p.RunID], st)
	r.stubRuns.mu.Unlock()
	return st, nil
}

func (r *fanoutRunsRepo) ListRuns(ctx context.Context, f run.ListRunsFilter) ([]*run.Run, error) {
	r.mu.Lock()
	r.listFilters = append(r.listFilters, f)
	preset := r.listResult
	r.mu.Unlock()
	if preset != nil {
		return preset, nil
	}
	// No preset: fall back to the embedded stub's DecomposedFrom filter so
	// the inline + refill + backstop dispatch paths actually observe the
	// children CreateRun recorded (honoring Offset/Limit like the real repo).
	return r.stubRuns.ListRuns(ctx, f)
}

// fakeArtifacts is a minimal artifact.Repository returning a fixed
// list of artifacts for any stage.
type fakeArtifacts struct {
	mu      sync.Mutex
	byStage map[uuid.UUID][]*artifact.Artifact
}

// Create records the created artifact into byStage[p.StageID] and returns a
// real *artifact.Artifact so ListForStage/hasPullRequest observe
// orchestrator-created artifacts (#1732). Additive — tests that ignore Create
// are unaffected.
func (f *fakeArtifacts) Create(_ context.Context, p artifact.CreateParams) (*artifact.Artifact, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a := &artifact.Artifact{
		ID:            uuid.New(),
		StageID:       p.StageID,
		Kind:          p.Kind,
		SchemaVersion: p.SchemaVersion,
		Content:       p.Content,
		ContentHash:   p.ContentHash,
		CreatedAt:     time.Now().UTC(),
	}
	if f.byStage == nil {
		f.byStage = map[uuid.UUID][]*artifact.Artifact{}
	}
	f.byStage[p.StageID] = append(f.byStage[p.StageID], a)
	return a, nil
}
func (f *fakeArtifacts) Get(_ context.Context, id uuid.UUID) (*artifact.Artifact, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, list := range f.byStage {
		for _, a := range list {
			if a.ID == id {
				return a, nil
			}
		}
	}
	return nil, artifact.ErrNotFound
}
func (f *fakeArtifacts) ListForStage(_ context.Context, stageID uuid.UUID) ([]*artifact.Artifact, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.byStage[stageID], nil
}
func (f *fakeArtifacts) GetByHash(context.Context, uuid.UUID, string) (*artifact.Artifact, error) {
	return nil, artifact.ErrNotFound
}

// recordingAudit captures every AppendChained call so tests can
// assert on category + payload without touching a database.
type recordingAudit struct {
	mu       sync.Mutex
	appended []audit.ChainAppendParams
}

func (r *recordingAudit) Append(context.Context, audit.AppendParams) (*audit.Entry, error) {
	return nil, nil
}

func (r *recordingAudit) ChainsByParent(_ context.Context, _ uuid.UUID, _ bool) ([]*audit.Entry, error) {
	return nil, nil
}
func (r *recordingAudit) AppendChained(_ context.Context, p audit.ChainAppendParams) (*audit.Entry, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.appended = append(r.appended, p)
	return &audit.Entry{ID: uuid.New()}, nil
}
func (r *recordingAudit) AppendGlobalChained(context.Context, audit.GlobalChainAppendParams) (*audit.Entry, error) {
	return nil, nil
}
func (r *recordingAudit) Get(context.Context, uuid.UUID) (*audit.Entry, error) {
	return nil, audit.ErrNotFound
}
func (r *recordingAudit) ListForRun(context.Context, uuid.UUID) ([]*audit.Entry, error) {
	return nil, nil
}
func (r *recordingAudit) ListGlobal(context.Context) ([]*audit.Entry, error) {
	return nil, nil
}
func (r *recordingAudit) LastForRun(context.Context, uuid.UUID) (*audit.Entry, error) {
	return nil, audit.ErrNotFound
}
func (r *recordingAudit) ListForRunByCategory(_ context.Context, runID uuid.UUID, category string) ([]*audit.Entry, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*audit.Entry
	for _, p := range r.appended {
		if p.RunID == runID && p.Category == category {
			rid := p.RunID
			out = append(out, &audit.Entry{
				ID:       uuid.New(),
				RunID:    &rid,
				StageID:  p.StageID,
				Category: p.Category,
				Payload:  p.Payload,
			})
		}
	}
	return out, nil
}
func (r *recordingAudit) ListAll(context.Context, audit.ListAllParams) ([]*audit.Entry, error) {
	return nil, nil
}

func decomposedPlanBytes(t *testing.T, subPlanTitles []string) []byte {
	t.Helper()
	subs := make([]map[string]any, 0, len(subPlanTitles))
	for _, title := range subPlanTitles {
		subs = append(subs, map[string]any{
			"title":                        title,
			"scope_hint":                   "scope hint for " + title,
			"predicted_runtime_minutes":    10,
			"predicted_runtime_confidence": "high",
		})
	}
	body := map[string]any{
		"plan_version": "standard_v1",
		"ticket_reference": map[string]any{
			"type": "github_issue",
			"url":  "https://github.com/example/repo/issues/1",
			"id":   "example/repo#1",
		},
		"generated_by": map[string]any{
			"agent":     "claude-code",
			"model":     "claude-opus-4-7",
			"timestamp": time.Now().UTC().Format(time.RFC3339),
		},
		"summary": "test plan with decomposition",
		"scope": map[string]any{
			"files": []map[string]any{
				{"path": "x.go", "operation": "create"},
			},
		},
		"approach": []map[string]any{
			{"step": 1, "description": "do it"},
		},
		"verification": map[string]any{
			"test_strategy": "run tests",
			"rollback_plan": "revert",
		},
		"predicted_runtime_minutes":    100,
		"predicted_runtime_confidence": "medium",
		"decomposition": map[string]any{
			"rationale": "test decomposition rationale",
			"sub_plans": subs,
		},
	}
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	return b
}

func TestAdvance_FanoutDecomposedPlan(t *testing.T) {
	rs := newFanoutRunsRepo()
	parent, stages := rs.seed(t, "example/repo", nil, []stageSeed{
		{Type: run.StageTypePlan, ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code", State: run.StageStateSucceeded},
		{Type: run.StageTypeImplement, ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code", State: run.StageStatePending},
		{Type: run.StageTypeReview, ExecutorKind: run.ExecutorHuman, ExecutorRef: "human", State: run.StageStatePending},
	})
	planStage, implementStage := stages[0], stages[1]

	// The parent carries a cached workflow spec; each minted child must
	// inherit it so its implement-stage prompt resolves the policy
	// max_stage_runtime instead of the runner's 15m default. The bytes
	// need not parse here — only byte-equality is asserted.
	parentSpec := []byte("workflows:\n  feature_change:\n    policy:\n      max_stage_runtime: 30m\n")
	parent.WorkflowSpec = parentSpec

	planBytes := decomposedPlanBytes(t, []string{"Part A", "Part B", "Part C"})
	schemaV := "standard_v1"
	arts := &fakeArtifacts{
		byStage: map[uuid.UUID][]*artifact.Artifact{
			planStage.ID: {{
				ID:            uuid.New(),
				StageID:       planStage.ID,
				Kind:          artifact.KindPlan,
				SchemaVersion: &schemaV,
				Content:       planBytes,
				ContentHash:   "deadbeef",
				CreatedAt:     time.Now().UTC(),
			}},
		},
	}
	au := &recordingAudit{}

	o := &Orchestrator{
		Runs:      rs,
		Logger:    slog.Default(),
		Artifacts: arts,
		Audit:     au,
	}
	out, err := o.Advance(context.Background(), parent.ID)
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if out != OutcomeDecomposed {
		t.Errorf("Advance outcome = %q, want %q", out, OutcomeDecomposed)
	}

	// This parent carries a nil IssueContext (seed leaves it unset) — the
	// campaign-minted shape (#1721). Child scope linkage now rests on the
	// persisted SliceIndex, set independently of IssueContext, so the pin
	// below asserts a distinct 0-based SliceIndex on every child regardless.
	if parent.IssueContext != nil {
		t.Fatalf("test precondition: parent.IssueContext = %+v, want nil (campaign-minted shape)", parent.IssueContext)
	}

	rs.mu.Lock()
	defer rs.mu.Unlock()
	if got, want := len(rs.createdRuns), 3; got != want {
		t.Fatalf("createdRuns = %d, want %d", got, want)
	}
	seenSlice := map[int]bool{}
	for i, child := range rs.createdRuns {
		if child.ParentRunID == nil || *child.ParentRunID != parent.ID {
			t.Errorf("child %d parent_run_id = %v, want %s", i, child.ParentRunID, parent.ID)
		}
		if child.DecomposedFrom == nil || *child.DecomposedFrom != parent.ID {
			t.Errorf("child %d decomposed_from = %v, want %s", i, child.DecomposedFrom, parent.ID)
		}
		if child.WorkflowID != parent.WorkflowID {
			t.Errorf("child %d workflow_id = %q, want %q", i, child.WorkflowID, parent.WorkflowID)
		}
		if !bytes.Equal(child.WorkflowSpec, parentSpec) {
			t.Errorf("child %d workflow_spec = %q, want inherited parent spec %q", i, child.WorkflowSpec, parentSpec)
		}
		// SliceIndex linkage pin (#1721): every child carries a distinct
		// 0-based sub_plan index even though the parent's IssueContext is nil.
		if child.SliceIndex == nil {
			t.Errorf("child %d slice_index = nil, want a 0-based index", i)
			continue
		}
		if *child.SliceIndex != i {
			t.Errorf("child %d slice_index = %d, want %d (ordered fan-out)", i, *child.SliceIndex, i)
		}
		if seenSlice[*child.SliceIndex] {
			t.Errorf("child %d slice_index = %d duplicated; want distinct per child", i, *child.SliceIndex)
		}
		seenSlice[*child.SliceIndex] = true
	}
	if got := len(rs.createdStages); got != 3 {
		t.Errorf("createdStages = %d, want 3 (one implement per child)", got)
	}
	for i, st := range rs.createdStages {
		if st.Type != run.StageTypeImplement {
			t.Errorf("child stage %d type = %q, want implement", i, st.Type)
		}
	}

	// Parent implement transitioned to awaiting_children.
	if implementStage.State != run.StageStateAwaitingChildren {
		t.Errorf("parent implement state = %q, want awaiting_children", implementStage.State)
	}

	// One plan_decomposed audit entry.
	au.mu.Lock()
	defer au.mu.Unlock()
	if got := len(au.appended); got != 1 {
		t.Fatalf("audit appended = %d, want 1", got)
	}
	if au.appended[0].Category != "plan_decomposed" {
		t.Errorf("audit category = %q, want plan_decomposed", au.appended[0].Category)
	}
	var payload struct {
		ChildRunIDs []string `json:"child_run_ids"`
		Rationale   string   `json:"rationale"`
	}
	if err := json.Unmarshal(au.appended[0].Payload, &payload); err != nil {
		t.Fatalf("unmarshal audit payload: %v", err)
	}
	if len(payload.ChildRunIDs) != 3 {
		t.Errorf("payload child_run_ids = %d, want 3", len(payload.ChildRunIDs))
	}
	if payload.Rationale != "test decomposition rationale" {
		t.Errorf("payload rationale = %q", payload.Rationale)
	}
}

// subPlanSpec is a sub_plan title plus its depends_on edges, used by
// decomposedPlanBytesWithDeps to build a dependency-bearing decomposition.
type subPlanSpec struct {
	title     string
	dependsOn []int
}

// decomposedPlanBytesWithDeps mirrors decomposedPlanBytes but threads a
// depends_on array onto each sub_plan so the orchestrator's plan.Waves call
// produces multi-index waves (#1258 slice B).
func decomposedPlanBytesWithDeps(t *testing.T, specs []subPlanSpec) []byte {
	t.Helper()
	subs := make([]map[string]any, 0, len(specs))
	for _, sp := range specs {
		m := map[string]any{
			"title":                        sp.title,
			"scope_hint":                   "scope hint for " + sp.title,
			"predicted_runtime_minutes":    10,
			"predicted_runtime_confidence": "high",
		}
		if len(sp.dependsOn) > 0 {
			m["depends_on"] = sp.dependsOn
		}
		subs = append(subs, m)
	}
	body := map[string]any{
		"plan_version": "standard_v1",
		"ticket_reference": map[string]any{
			"type": "github_issue",
			"url":  "https://github.com/example/repo/issues/1",
			"id":   "example/repo#1",
		},
		"generated_by": map[string]any{
			"agent":     "claude-code",
			"model":     "claude-opus-4-7",
			"timestamp": time.Now().UTC().Format(time.RFC3339),
		},
		"summary": "test plan with dependency-ordered decomposition",
		"scope": map[string]any{
			"files": []map[string]any{{"path": "x.go", "operation": "create"}},
		},
		"approach":                     []map[string]any{{"step": 1, "description": "do it"}},
		"verification":                 map[string]any{"test_strategy": "run tests", "rollback_plan": "revert"},
		"predicted_runtime_minutes":    100,
		"predicted_runtime_confidence": "medium",
		"decomposition": map[string]any{
			"rationale": "test decomposition rationale",
			"sub_plans": subs,
		},
	}
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	return b
}

// decodePlanDecomposedWaves runs a fanout from the given plan bytes and returns
// the waves field of the emitted plan_decomposed audit payload.
func decodePlanDecomposedWaves(t *testing.T, planBytes []byte) [][]int {
	t.Helper()
	rs := newFanoutRunsRepo()
	parent, stages := rs.seed(t, "example/repo", nil, []stageSeed{
		{Type: run.StageTypePlan, ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code", State: run.StageStateSucceeded},
		{Type: run.StageTypeImplement, ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code", State: run.StageStatePending},
		{Type: run.StageTypeReview, ExecutorKind: run.ExecutorHuman, ExecutorRef: "human", State: run.StageStatePending},
	})
	planStage := stages[0]
	schemaV := "standard_v1"
	arts := &fakeArtifacts{
		byStage: map[uuid.UUID][]*artifact.Artifact{
			planStage.ID: {{
				ID:            uuid.New(),
				StageID:       planStage.ID,
				Kind:          artifact.KindPlan,
				SchemaVersion: &schemaV,
				Content:       planBytes,
				CreatedAt:     time.Now().UTC(),
			}},
		},
	}
	au := &recordingAudit{}
	o := &Orchestrator{Runs: rs, Logger: slog.Default(), Artifacts: arts, Audit: au}
	if _, err := o.Advance(context.Background(), parent.ID); err != nil {
		t.Fatalf("Advance: %v", err)
	}
	au.mu.Lock()
	defer au.mu.Unlock()
	if len(au.appended) != 1 || au.appended[0].Category != "plan_decomposed" {
		t.Fatalf("want 1 plan_decomposed entry, got %+v", au.appended)
	}
	var payload struct {
		Waves [][]int `json:"waves"`
	}
	if err := json.Unmarshal(au.appended[0].Payload, &payload); err != nil {
		t.Fatalf("unmarshal plan_decomposed payload: %v", err)
	}
	return payload.Waves
}

// TestAdvance_FanoutPlanDecomposedWaves_DependsOn asserts a depends_on
// decomposition's plan_decomposed payload carries the ordered topological
// waves: A (no deps) in wave 0, B and C (both depend on A) in wave 1.
func TestAdvance_FanoutPlanDecomposedWaves_DependsOn(t *testing.T) {
	planBytes := decomposedPlanBytesWithDeps(t, []subPlanSpec{
		{title: "Part A"},
		{title: "Part B", dependsOn: []int{0}},
		{title: "Part C", dependsOn: []int{0}},
	})
	waves := decodePlanDecomposedWaves(t, planBytes)
	want := [][]int{{0}, {1, 2}}
	if !equalIntWaves(waves, want) {
		t.Errorf("waves = %v, want %v", waves, want)
	}
}

// TestAdvance_FanoutPlanDecomposedWaves_NoDependsOn asserts a no-depends_on
// decomposition's plan_decomposed payload carries a single all-indices wave —
// the back-compat collapse that the run_children loop dispatches as one
// concurrent wave and never integrate-waves.
func TestAdvance_FanoutPlanDecomposedWaves_NoDependsOn(t *testing.T) {
	planBytes := decomposedPlanBytes(t, []string{"Part A", "Part B", "Part C"})
	waves := decodePlanDecomposedWaves(t, planBytes)
	want := [][]int{{0, 1, 2}}
	if !equalIntWaves(waves, want) {
		t.Errorf("waves = %v, want %v", waves, want)
	}
}

func equalIntWaves(a, b [][]int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if len(a[i]) != len(b[i]) {
			return false
		}
		for j := range a[i] {
			if a[i][j] != b[i][j] {
				return false
			}
		}
	}
	return true
}

func TestAdvance_FanoutSkippedWhenChildrenExist(t *testing.T) {
	// #1063: a fix-up on a decomposed parent re-opens its implement stage to
	// pending. Re-entering Advance must NOT re-mint a fresh fan-out — the
	// parent already has children. The existing-children guard skips the
	// fanout and falls through to dispatch so the parent implement stage is
	// re-invoked against the existing shared branch.
	rs := newFanoutRunsRepo()
	parent, stages := rs.seed(t, "example/repo", nil, []stageSeed{
		{Type: run.StageTypePlan, ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code", State: run.StageStateSucceeded},
		{Type: run.StageTypeImplement, ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code", State: run.StageStatePending},
		{Type: run.StageTypeReview, ExecutorKind: run.ExecutorHuman, ExecutorRef: "human", State: run.StageStatePending},
	})
	planStage := stages[0]

	// A pre-existing child decomposed from the parent. The guard's ListRuns
	// returns this, so the fanout is skipped.
	childParentID := parent.ID
	rs.listResult = []*run.Run{{
		ID:             uuid.New(),
		DecomposedFrom: &childParentID,
		State:          run.StateRunning,
	}}

	planBytes := decomposedPlanBytes(t, []string{"Part A", "Part B"})
	schemaV := "standard_v1"
	arts := &fakeArtifacts{
		byStage: map[uuid.UUID][]*artifact.Artifact{
			planStage.ID: {{
				ID:            uuid.New(),
				StageID:       planStage.ID,
				Kind:          artifact.KindPlan,
				SchemaVersion: &schemaV,
				Content:       planBytes,
				CreatedAt:     time.Now().UTC(),
			}},
		},
	}

	o := &Orchestrator{Runs: rs, Logger: slog.Default(), Artifacts: arts, Audit: &recordingAudit{}}
	out, err := o.Advance(context.Background(), parent.ID)
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if out == OutcomeDecomposed {
		t.Errorf("fanout re-minted on a parent with existing children: outcome = %q", out)
	}
	if out != OutcomeDispatched {
		t.Errorf("outcome = %q, want %q (re-invoke parent implement)", out, OutcomeDispatched)
	}

	rs.mu.Lock()
	defer rs.mu.Unlock()
	if got := len(rs.createdRuns); got != 0 {
		t.Errorf("createdRuns = %d, want 0 (no new children minted)", got)
	}
	if got := len(rs.createdStages); got != 0 {
		t.Errorf("createdStages = %d, want 0 (no new child stages)", got)
	}
	// The guard's ListRuns probe filtered on DecomposedFrom == parent.ID.
	var sawProbe bool
	for _, f := range rs.listFilters {
		if f.DecomposedFrom != nil && *f.DecomposedFrom == parent.ID {
			sawProbe = true
			break
		}
	}
	if !sawProbe {
		t.Errorf("expected a ListRuns probe filtered on DecomposedFrom == %s, got filters %+v", parent.ID, rs.listFilters)
	}
}

func TestAdvance_NoDecomposition_DispatchesNormally(t *testing.T) {
	rs := newFanoutRunsRepo()
	parent, stages := rs.seed(t, "example/repo", nil, []stageSeed{
		{Type: run.StageTypePlan, ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code", State: run.StageStateSucceeded},
		{Type: run.StageTypeImplement, ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code", State: run.StageStatePending},
	})
	planStage := stages[0]

	// Plan without decomposition: missing the field entirely.
	planBytes, _ := json.Marshal(map[string]any{
		"plan_version": "standard_v1",
		"summary":      "no decomp",
	})
	schemaV := "standard_v1"
	arts := &fakeArtifacts{
		byStage: map[uuid.UUID][]*artifact.Artifact{
			planStage.ID: {{
				ID:            uuid.New(),
				StageID:       planStage.ID,
				Kind:          artifact.KindPlan,
				SchemaVersion: &schemaV,
				Content:       planBytes,
				CreatedAt:     time.Now().UTC(),
			}},
		},
	}
	o := &Orchestrator{Runs: rs, Logger: slog.Default(), Artifacts: arts, Audit: &recordingAudit{}}
	out, err := o.Advance(context.Background(), parent.ID)
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if out != OutcomeDispatched {
		t.Errorf("outcome = %q, want %q", out, OutcomeDispatched)
	}
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if len(rs.createdRuns) != 0 {
		t.Errorf("createdRuns = %d, want 0 (no decomposition)", len(rs.createdRuns))
	}
}

// TestAdvance_FanoutLocalLockedParent_ChildrenParkAwaitingHostDispatch is the
// #1980 regression pin: a decomposed parent LOCKED to runner_kind=local
// (RunnerKindResolved=true) fans out, and EVERY minted child's implement stage
// must land at awaiting_host_dispatch — never the legacy 'dispatched' — with
// ZERO workflow_dispatch fired. This FAILS on pre-#1980 code where the child row
// is minted runner_kind-unresolved, defeating the resolved-gated park predicate:
// the children flip to 'dispatched' (and, with an installation set, fire a
// github_actions workflow_dispatch each) and run_children then treats them as
// in-flight, deadlocking the fan-out.
func TestAdvance_FanoutLocalLockedParent_ChildrenParkAwaitingHostDispatch(t *testing.T) {
	rs := newFanoutRunsRepo()
	parent, stages := rs.seed(t, "example/repo", int64Ptr(42), []stageSeed{
		{Type: run.StageTypePlan, ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code", State: run.StageStateSucceeded},
		{Type: run.StageTypeImplement, ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code", State: run.StageStatePending},
		{Type: run.StageTypeReview, ExecutorKind: run.ExecutorHuman, ExecutorRef: "human", State: run.StageStatePending},
	})
	planStage := stages[0]
	// Lock the parent to the local channel — the dogfood shape at fan-out time.
	parent.RunnerKind = run.RunnerKindLocal
	parent.RunnerKindResolved = true

	planBytes := decomposedPlanBytes(t, []string{"Part A", "Part B", "Part C"})
	schemaV := "standard_v1"
	arts := &fakeArtifacts{
		byStage: map[uuid.UUID][]*artifact.Artifact{
			planStage.ID: {{
				ID: uuid.New(), StageID: planStage.ID, Kind: artifact.KindPlan,
				SchemaVersion: &schemaV, Content: planBytes, CreatedAt: time.Now().UTC(),
			}},
		},
	}
	gh := &stubGitHub{}
	au := &recordingAudit{}
	o := &Orchestrator{
		Runs: rs, GitHub: gh, Logger: slog.Default(), Artifacts: arts, Audit: au,
		DefaultRef: "main", MaxParallelChildren: 0, Drive: &drive.Engine{Audit: au},
	}

	out, err := o.Advance(context.Background(), parent.ID)
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if out != OutcomeDecomposed {
		t.Fatalf("outcome = %q, want %q", out, OutcomeDecomposed)
	}

	rs.mu.Lock()
	defer rs.mu.Unlock()
	if len(rs.createdRuns) != 3 {
		t.Fatalf("createdRuns = %d, want 3", len(rs.createdRuns))
	}
	if len(rs.createdStages) != 3 {
		t.Fatalf("createdStages = %d, want 3", len(rs.createdStages))
	}
	// Every minted child implement stage parked awaiting a host spawn — the
	// #1980 done-means.
	for i, st := range rs.createdStages {
		if st.Type != run.StageTypeImplement {
			t.Errorf("child stage %d type = %q, want implement", i, st.Type)
		}
		if st.State != run.StageStateAwaitingHostDispatch {
			t.Errorf("child stage %d state = %q, want awaiting_host_dispatch (never dispatched)", i, st.State)
		}
	}
	// The local lock suppressed every github_actions workflow_dispatch.
	if len(gh.calls) != 0 {
		t.Errorf("workflow_dispatch fired for a local-locked decomposition: %d calls", len(gh.calls))
	}
}

func TestAdvance_ChildRunSkipsFanout(t *testing.T) {
	// A run with decomposed_from set must NOT itself fanout, even
	// when a (hypothetical) plan stage with sub_plans is present.
	// Without this guard, the fanout would recurse infinitely.
	rs := newFanoutRunsRepo()
	parent, _ := rs.seed(t, "example/repo", nil, []stageSeed{
		{Type: run.StageTypeImplement, ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code", State: run.StageStatePending},
	})
	parentID := uuid.New()
	parent.DecomposedFrom = &parentID

	o := &Orchestrator{Runs: rs, Logger: slog.Default(), Artifacts: &fakeArtifacts{}, Audit: &recordingAudit{}}
	out, err := o.Advance(context.Background(), parent.ID)
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if out == OutcomeDecomposed {
		t.Errorf("child run incorrectly fanned out: outcome = %q", out)
	}
}

// --- E24.3 / #1143: concurrent decomposed-child dispatch ------------------

// seedAwaitingChildrenParent seeds a parent run already parked in
// awaiting_children (plan succeeded, implement awaiting_children) so the
// per-mode DispatchDecomposedChildren tests can drive the dispatch seam
// directly without re-running the fanout mint.
func seedAwaitingChildrenParent(t *testing.T, rs *fanoutRunsRepo) *run.Run {
	t.Helper()
	parent, _ := rs.seed(t, "example/repo", nil, []stageSeed{
		{Type: run.StageTypePlan, ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code", State: run.StageStateSucceeded},
		{Type: run.StageTypeImplement, ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code", State: run.StageStateAwaitingChildren},
	})
	return parent
}

// mintPendingChildren creates n pending decomposed children (each with a
// single pending implement stage) under parentID, in ascending slice
// order, with the given runner kind.
func mintPendingChildren(t *testing.T, rs *fanoutRunsRepo, parentID uuid.UUID, n int, runnerKind string) []*run.Run {
	t.Helper()
	children := make([]*run.Run, 0, n)
	for i := 0; i < n; i++ {
		idx := i
		pid := parentID
		c, err := rs.CreateRun(context.Background(), run.CreateRunParams{
			Repo:           "example/repo",
			WorkflowID:     "feature_change",
			ParentRunID:    &pid,
			DecomposedFrom: &pid,
			SliceIndex:     &idx,
			RunnerKind:     runnerKind,
		})
		if err != nil {
			t.Fatalf("CreateRun child %d: %v", i, err)
		}
		if _, err := rs.CreateStage(context.Background(), run.CreateStageParams{
			RunID:        c.ID,
			Sequence:     0,
			Type:         run.StageTypeImplement,
			ExecutorKind: run.ExecutorAgent,
			ExecutorRef:  "claude-code",
		}); err != nil {
			t.Fatalf("CreateStage child %d: %v", i, err)
		}
		children = append(children, c)
	}
	return children
}

// implState returns the state of runID's implement stage.
func implState(t *testing.T, rs *fanoutRunsRepo, runID uuid.UUID) run.StageState {
	t.Helper()
	stages, err := rs.ListStagesForRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("ListStagesForRun: %v", err)
	}
	for _, s := range stages {
		if s.Type == run.StageTypeImplement {
			return s.State
		}
	}
	t.Fatalf("no implement stage for run %s", runID)
	return ""
}

// countDriveDispatchEntries counts run_auto_advanced audit entries that
// name RuleChildrenDispatch.
func countDriveDispatchEntries(au *recordingAudit) int {
	au.mu.Lock()
	defer au.mu.Unlock()
	n := 0
	for _, p := range au.appended {
		if p.Category != "run_auto_advanced" {
			continue
		}
		var adv struct {
			Rule string `json:"rule"`
		}
		if json.Unmarshal(p.Payload, &adv) == nil && adv.Rule == "children_dispatch" {
			n++
		}
	}
	return n
}

func countByState(children []*run.Run, state run.State) int {
	n := 0
	for _, c := range children {
		if c.State == state {
			n++
		}
	}
	return n
}

// TestDispatchDecomposedChildren_DispatchesUpToCap is the DONE-MEANS test:
// a decomposed parent with an unlimited cap (0) dispatches EVERY child —
// each child run transitions pending -> running and its implement stage to
// dispatched — driven end-to-end through the fanout -> dispatch ->
// drive-record seam, with one RuleChildrenDispatch run_auto_advanced entry
// per child and no per-child operator call.
func TestDispatchDecomposedChildren_DispatchesUpToCap(t *testing.T) {
	rs := newFanoutRunsRepo()
	parent, stages := rs.seed(t, "example/repo", nil, []stageSeed{
		{Type: run.StageTypePlan, ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code", State: run.StageStateSucceeded},
		{Type: run.StageTypeImplement, ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code", State: run.StageStatePending},
		{Type: run.StageTypeReview, ExecutorKind: run.ExecutorHuman, ExecutorRef: "human", State: run.StageStatePending},
	})
	planStage, implementStage := stages[0], stages[1]

	planBytes := decomposedPlanBytes(t, []string{"Part A", "Part B", "Part C"})
	schemaV := "standard_v1"
	arts := &fakeArtifacts{byStage: map[uuid.UUID][]*artifact.Artifact{
		planStage.ID: {{ID: uuid.New(), StageID: planStage.ID, Kind: artifact.KindPlan, SchemaVersion: &schemaV, Content: planBytes, CreatedAt: time.Now().UTC()}},
	}}
	au := &recordingAudit{}
	o := &Orchestrator{
		Runs:                rs,
		Logger:              slog.Default(),
		Artifacts:           arts,
		Audit:               au,
		MaxParallelChildren: 0, // unlimited
		Drive:               &drive.Engine{Audit: au, Logger: slog.Default()},
	}

	out, err := o.Advance(context.Background(), parent.ID)
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if out != OutcomeDecomposed {
		t.Fatalf("outcome = %q, want %q", out, OutcomeDecomposed)
	}

	// Parent implement stays parked awaiting children.
	if implementStage.State != run.StageStateAwaitingChildren {
		t.Errorf("parent implement state = %q, want awaiting_children", implementStage.State)
	}

	rs.mu.Lock()
	children := append([]*run.Run(nil), rs.createdRuns...)
	rs.mu.Unlock()
	if len(children) != 3 {
		t.Fatalf("createdRuns = %d, want 3", len(children))
	}
	// Every child dispatched: run running + implement stage dispatched.
	for i, c := range children {
		if c.State != run.StateRunning {
			t.Errorf("child %d state = %q, want running (dispatched)", i, c.State)
		}
		if st := implState(t, rs, c.ID); st != run.StageStateDispatched {
			t.Errorf("child %d implement state = %q, want dispatched", i, st)
		}
	}
	// One RuleChildrenDispatch entry per child.
	if got := countDriveDispatchEntries(au); got != 3 {
		t.Errorf("RuleChildrenDispatch entries = %d, want 3", got)
	}
}

// TestDispatchDecomposedChildren_CapThrottles asserts a finite cap binds:
// cap=2 with 5 pending children dispatches exactly 2 and leaves 3 pending
// (budget.ParallelDecision.Allowed honored).
func TestDispatchDecomposedChildren_CapThrottles(t *testing.T) {
	rs := newFanoutRunsRepo()
	parent := seedAwaitingChildrenParent(t, rs)
	children := mintPendingChildren(t, rs, parent.ID, 5, "")
	au := &recordingAudit{}
	o := &Orchestrator{Runs: rs, Logger: slog.Default(), Audit: au, MaxParallelChildren: 2, Drive: &drive.Engine{Audit: au}}

	n, err := o.DispatchDecomposedChildren(context.Background(), parent.ID)
	if err != nil {
		t.Fatalf("DispatchDecomposedChildren: %v", err)
	}
	if n != 2 {
		t.Fatalf("dispatched = %d, want 2 (cap binds)", n)
	}
	if got := countByState(children, run.StateRunning); got != 2 {
		t.Errorf("running children = %d, want 2", got)
	}
	if got := countByState(children, run.StatePending); got != 3 {
		t.Errorf("pending children = %d, want 3 (throttled)", got)
	}
	// The two earliest slices (0,1) are the ones dispatched.
	for _, c := range children {
		want := run.StatePending
		if *c.SliceIndex < 2 {
			want = run.StateRunning
		}
		if c.State != want {
			t.Errorf("slice %d state = %q, want %q", *c.SliceIndex, c.State, want)
		}
	}
}

// TestDispatchDecomposedChildren_UnlimitedCap asserts cap=0 dispatches all.
func TestDispatchDecomposedChildren_UnlimitedCap(t *testing.T) {
	rs := newFanoutRunsRepo()
	parent := seedAwaitingChildrenParent(t, rs)
	children := mintPendingChildren(t, rs, parent.ID, 4, "")
	o := &Orchestrator{Runs: rs, Logger: slog.Default(), Audit: &recordingAudit{}, MaxParallelChildren: 0}

	n, err := o.DispatchDecomposedChildren(context.Background(), parent.ID)
	if err != nil {
		t.Fatalf("DispatchDecomposedChildren: %v", err)
	}
	if n != 4 {
		t.Fatalf("dispatched = %d, want 4 (unlimited)", n)
	}
	if got := countByState(children, run.StateRunning); got != 4 {
		t.Errorf("running children = %d, want 4", got)
	}
}

// TestDispatchDecomposedChildren_AtCapNoHeadroom asserts the headroom<=0
// guard: re-invoking while the in-flight count already equals the cap
// dispatches 0 more even though pending children remain.
func TestDispatchDecomposedChildren_AtCapNoHeadroom(t *testing.T) {
	rs := newFanoutRunsRepo()
	parent := seedAwaitingChildrenParent(t, rs)
	children := mintPendingChildren(t, rs, parent.ID, 5, "")
	o := &Orchestrator{Runs: rs, Logger: slog.Default(), Audit: &recordingAudit{}, MaxParallelChildren: 2}

	if n, err := o.DispatchDecomposedChildren(context.Background(), parent.ID); err != nil || n != 2 {
		t.Fatalf("initial dispatch = (%d, %v), want (2, nil)", n, err)
	}
	// Two children are now in-flight (running) == cap; no slot is free even
	// though 3 children remain pending.
	n, err := o.DispatchDecomposedChildren(context.Background(), parent.ID)
	if err != nil {
		t.Fatalf("re-dispatch at cap: %v", err)
	}
	if n != 0 {
		t.Errorf("re-dispatch at cap = %d, want 0 (no headroom)", n)
	}
	if got := countByState(children, run.StatePending); got != 3 {
		t.Errorf("pending children = %d, want 3 (held at cap)", got)
	}
}

// TestDispatchDecomposedChildren_EventDrivenRefill asserts the
// maybeAdvanceDecomposedParent event path tops up the dispatch as in-flight
// children settle: after a cap=2 throttle, settling one running child
// dispatches exactly one more pending child, holding the active count at 2.
func TestDispatchDecomposedChildren_EventDrivenRefill(t *testing.T) {
	rs := newFanoutRunsRepo()
	parent := seedAwaitingChildrenParent(t, rs)
	children := mintPendingChildren(t, rs, parent.ID, 5, "")
	au := &recordingAudit{}
	o := &Orchestrator{Runs: rs, Logger: slog.Default(), Audit: au, MaxParallelChildren: 2, Drive: &drive.Engine{Audit: au}}

	if n, err := o.DispatchDecomposedChildren(context.Background(), parent.ID); err != nil || n != 2 {
		t.Fatalf("initial dispatch = (%d, %v), want (2, nil)", n, err)
	}

	// Settle slice 0 (one of the two in-flight children) to terminal.
	children[0].State = run.StateSucceeded

	// Drive the event-driven refill via the maybeAdvanceDecomposedParent
	// path (fires on each child terminal transition).
	o.maybeAdvanceDecomposedParent(context.Background(), parent.ID)

	// Exactly one more pending child dispatched (slice 2), holding the
	// active (running) count at the cap of 2.
	if got := countByState(children, run.StateRunning); got != 2 {
		t.Errorf("running children after refill = %d, want 2 (held at cap)", got)
	}
	if got := countByState(children, run.StatePending); got != 2 {
		t.Errorf("pending children after refill = %d, want 2", got)
	}
	if children[2].State != run.StateRunning {
		t.Errorf("slice 2 state = %q, want running (refilled)", children[2].State)
	}
}

// TestDispatchDecomposedChildren_IdempotentReDispatch asserts a second
// invocation against unchanged child state dispatches 0 more children and
// records no duplicate drive entry.
func TestDispatchDecomposedChildren_IdempotentReDispatch(t *testing.T) {
	rs := newFanoutRunsRepo()
	parent := seedAwaitingChildrenParent(t, rs)
	mintPendingChildren(t, rs, parent.ID, 3, "")
	au := &recordingAudit{}
	o := &Orchestrator{Runs: rs, Logger: slog.Default(), Audit: au, MaxParallelChildren: 0, Drive: &drive.Engine{Audit: au}}

	if n, err := o.DispatchDecomposedChildren(context.Background(), parent.ID); err != nil || n != 3 {
		t.Fatalf("first dispatch = (%d, %v), want (3, nil)", n, err)
	}
	if got := countDriveDispatchEntries(au); got != 3 {
		t.Fatalf("drive entries after first dispatch = %d, want 3", got)
	}

	// Second call: children are all running now, so nothing pending.
	n, err := o.DispatchDecomposedChildren(context.Background(), parent.ID)
	if err != nil {
		t.Fatalf("second DispatchDecomposedChildren: %v", err)
	}
	if n != 0 {
		t.Errorf("second dispatch = %d, want 0 (idempotent)", n)
	}
	if got := countDriveDispatchEntries(au); got != 3 {
		t.Errorf("drive entries after second dispatch = %d, want 3 (no duplicates)", got)
	}
}

// TestDispatchDecomposedChildren_LocalRunnerParks asserts a local-runner
// child's recorded drive entry is Parked with a run_implement_stage next
// action (the backend cannot host-spawn the local runner, ADR-024).
func TestDispatchDecomposedChildren_LocalRunnerParks(t *testing.T) {
	rs := newFanoutRunsRepo()
	parent := seedAwaitingChildrenParent(t, rs)
	mintPendingChildren(t, rs, parent.ID, 1, run.RunnerKindLocal)
	au := &recordingAudit{}
	o := &Orchestrator{Runs: rs, Logger: slog.Default(), Audit: au, MaxParallelChildren: 0, Drive: &drive.Engine{Audit: au}}

	if n, err := o.DispatchDecomposedChildren(context.Background(), parent.ID); err != nil || n != 1 {
		t.Fatalf("dispatch = (%d, %v), want (1, nil)", n, err)
	}

	au.mu.Lock()
	defer au.mu.Unlock()
	var found bool
	for _, p := range au.appended {
		if p.Category != "run_auto_advanced" {
			continue
		}
		var adv drive.Advance
		if err := json.Unmarshal(p.Payload, &adv); err != nil {
			t.Fatalf("unmarshal drive payload: %v", err)
		}
		if adv.Rule != drive.RuleChildrenDispatch {
			continue
		}
		found = true
		if !adv.Parked {
			t.Errorf("local child drive entry Parked = false, want true")
		}
		if adv.NextAction == nil || adv.NextAction.Action != "run_implement_stage" {
			t.Errorf("local child next action = %+v, want run_implement_stage", adv.NextAction)
		}
	}
	if !found {
		t.Fatal("no RuleChildrenDispatch entry recorded for the local child")
	}
}

// TestFanout_BestEffortDispatchDoesNotUnwind asserts a dispatch failure at
// the fanout call site does NOT unwind the parked parent: the parent stays
// awaiting_children and the minted children remain.
func TestFanout_BestEffortDispatchDoesNotUnwind(t *testing.T) {
	rs := newFanoutRunsRepo()
	parent, stages := rs.seed(t, "example/repo", nil, []stageSeed{
		{Type: run.StageTypePlan, ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code", State: run.StageStateSucceeded},
		{Type: run.StageTypeImplement, ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code", State: run.StageStatePending},
	})
	planStage, implementStage := stages[0], stages[1]
	planBytes := decomposedPlanBytes(t, []string{"Part A", "Part B"})
	schemaV := "standard_v1"
	arts := &fakeArtifacts{byStage: map[uuid.UUID][]*artifact.Artifact{
		planStage.ID: {{ID: uuid.New(), StageID: planStage.ID, Kind: artifact.KindPlan, SchemaVersion: &schemaV, Content: planBytes, CreatedAt: time.Now().UTC()}},
	}}
	au := &recordingAudit{}
	o := &Orchestrator{Runs: rs, Logger: slog.Default(), Artifacts: arts, Audit: au, MaxParallelChildren: 0, Drive: &drive.Engine{Audit: au}}

	// Force every child Advance to fail at its run pending->running step.
	// fanoutIfDecomposed transitions only stages (not runs) and the parent
	// is already running, so the fanout mint + park still succeeds.
	rs.transitionRunErr = errors.New("boom")

	out, err := o.Advance(context.Background(), parent.ID)
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if out != OutcomeDecomposed {
		t.Fatalf("outcome = %q, want %q (fanout not unwound)", out, OutcomeDecomposed)
	}
	if implementStage.State != run.StageStateAwaitingChildren {
		t.Errorf("parent implement state = %q, want awaiting_children (not unwound)", implementStage.State)
	}
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if len(rs.createdRuns) != 2 {
		t.Errorf("createdRuns = %d, want 2 (children remain minted)", len(rs.createdRuns))
	}
	for i, c := range rs.createdRuns {
		if c.State != run.StatePending {
			t.Errorf("child %d state = %q, want pending (dispatch failed, child untouched)", i, c.State)
		}
	}
}

// --- E24.5 / #1145: GitHub Actions parallel child dispatch ----------------

// implStageID returns the id of runID's implement stage.
func implStageID(t *testing.T, rs *fanoutRunsRepo, runID uuid.UUID) uuid.UUID {
	t.Helper()
	stages, err := rs.ListStagesForRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("ListStagesForRun: %v", err)
	}
	for _, s := range stages {
		if s.Type == run.StageTypeImplement {
			return s.ID
		}
	}
	t.Fatalf("no implement stage for run %s", runID)
	return uuid.Nil
}

// setChildInstallation stamps installationID on each child run so its
// fireDispatch reaches the real DispatchWorkflow arm. mintPendingChildren's
// CreateRunParams does NOT propagate the parent's InstallationID to children
// (production fanoutIfDecomposed does), and fireDispatch reads the CHILD
// run's InstallationID — so setting it on the seeded parent alone would skip
// the dispatch arm and len(stub.calls)==childCount would fail (#1145).
func setChildInstallation(children []*run.Run, installationID int64) {
	for _, c := range children {
		id := installationID
		c.InstallationID = &id
	}
}

// TestDispatchDecomposedChildren_GitHubActions_FiresPerChildDispatch is the
// DONE-MEANS test at the orchestrator->GitHubAPI seam: a github_actions
// decomposed parent with an unlimited cap (0) fires exactly ONE
// workflow_dispatch per child — each carrying that child's own run_id and
// implement stage_id (pairwise distinct across calls, proving each child
// checks out its own slice branch and pushes a distinct branch with no
// collision) against the base ref (o.DefaultRef).
func TestDispatchDecomposedChildren_GitHubActions_FiresPerChildDispatch(t *testing.T) {
	rs := newFanoutRunsRepo()
	parent := seedAwaitingChildrenParent(t, rs)
	children := mintPendingChildren(t, rs, parent.ID, 3, run.RunnerKindGitHubActions)
	setChildInstallation(children, 42)

	gh := &stubGitHub{}
	au := &recordingAudit{}
	o := &Orchestrator{
		Runs:                rs,
		GitHub:              gh,
		Logger:              slog.Default(),
		Audit:               au,
		DefaultRef:          "main",
		MaxParallelChildren: 0, // unlimited
		Drive:               &drive.Engine{Audit: au},
	}

	n, err := o.DispatchDecomposedChildren(context.Background(), parent.ID)
	if err != nil {
		t.Fatalf("DispatchDecomposedChildren: %v", err)
	}
	if n != 3 {
		t.Fatalf("dispatched = %d, want 3", n)
	}

	gh.mu.Lock()
	calls := append([]dispatchCall(nil), gh.calls...)
	gh.mu.Unlock()
	if len(calls) != len(children) {
		t.Fatalf("workflow_dispatch calls = %d, want %d (one per child)", len(calls), len(children))
	}

	// Build the expected run_id -> implement stage_id map for the children.
	wantStageByRun := map[string]string{}
	for _, c := range children {
		wantStageByRun[c.ID.String()] = implStageID(t, rs, c.ID).String()
	}

	seenRun := map[string]bool{}
	seenStage := map[string]bool{}
	for i, call := range calls {
		if call.Ref != o.DefaultRef {
			t.Errorf("call %d Ref = %q, want %q (the base each slice cuts from)", i, call.Ref, o.DefaultRef)
		}
		if call.InstallationID != 42 {
			t.Errorf("call %d InstallationID = %d, want 42", i, call.InstallationID)
		}
		runID := call.Inputs["run_id"]
		stageID := call.Inputs["stage_id"]
		wantStage, ok := wantStageByRun[runID]
		if !ok {
			t.Errorf("call %d run_id = %q is not a minted child", i, runID)
			continue
		}
		if stageID != wantStage {
			t.Errorf("call %d stage_id = %q, want %q (child %s implement stage)", i, stageID, wantStage, runID)
		}
		// #1227: every decomposed child carries its decomposition-parent id as
		// parent_run_id so the customer workflow can key an Actions concurrency:
		// group on the run family and serialize siblings.
		if got := call.Inputs["parent_run_id"]; got != parent.ID.String() {
			t.Errorf("call %d parent_run_id = %q, want %q (the decomposition parent / run family)", i, got, parent.ID.String())
		}
		// Pairwise distinct run_id + stage_id across calls — proves per-slice
		// independence (no shared branch target).
		if seenRun[runID] {
			t.Errorf("call %d run_id = %q duplicated across dispatch calls", i, runID)
		}
		if seenStage[stageID] {
			t.Errorf("call %d stage_id = %q duplicated across dispatch calls", i, stageID)
		}
		seenRun[runID] = true
		seenStage[stageID] = true
	}
}

// TestDispatchDecomposedChildren_GitHubActions_CapThrottlesDispatch is the
// Actions analogue of TestDispatchDecomposedChildren_CapThrottles asserted at
// the dispatch-CALL boundary: cap=2 with 5 children fires exactly 2
// workflow_dispatch calls (the two earliest slice indices) and leaves 3
// children pending.
func TestDispatchDecomposedChildren_GitHubActions_CapThrottlesDispatch(t *testing.T) {
	rs := newFanoutRunsRepo()
	parent := seedAwaitingChildrenParent(t, rs)
	children := mintPendingChildren(t, rs, parent.ID, 5, run.RunnerKindGitHubActions)
	setChildInstallation(children, 42)

	gh := &stubGitHub{}
	au := &recordingAudit{}
	o := &Orchestrator{
		Runs:                rs,
		GitHub:              gh,
		Logger:              slog.Default(),
		Audit:               au,
		DefaultRef:          "main",
		MaxParallelChildren: 2,
		Drive:               &drive.Engine{Audit: au},
	}

	n, err := o.DispatchDecomposedChildren(context.Background(), parent.ID)
	if err != nil {
		t.Fatalf("DispatchDecomposedChildren: %v", err)
	}
	if n != 2 {
		t.Fatalf("dispatched = %d, want 2 (cap binds)", n)
	}

	gh.mu.Lock()
	calls := append([]dispatchCall(nil), gh.calls...)
	gh.mu.Unlock()
	if len(calls) != 2 {
		t.Fatalf("workflow_dispatch calls = %d, want 2 (throttled at the dispatch boundary)", len(calls))
	}

	// The two dispatched calls are the two earliest slices (0, 1).
	wantRuns := map[string]bool{}
	for _, c := range children {
		if *c.SliceIndex < 2 {
			wantRuns[c.ID.String()] = true
		}
	}
	for i, call := range calls {
		runID := call.Inputs["run_id"]
		if !wantRuns[runID] {
			t.Errorf("call %d run_id = %q, want one of the two earliest slices", i, runID)
		}
	}
	if got := countByState(children, run.StatePending); got != 3 {
		t.Errorf("pending children = %d, want 3 (throttled)", got)
	}
}

// TestDispatchDecomposedChildren_GitHubActions_NilInstallationSkipsDispatch
// asserts fireDispatch's graceful-skip arm: a github_actions child with nil
// InstallationID records ZERO workflow_dispatch calls but still advances run
// state (run running, implement stage dispatched).
func TestDispatchDecomposedChildren_GitHubActions_NilInstallationSkipsDispatch(t *testing.T) {
	rs := newFanoutRunsRepo()
	parent := seedAwaitingChildrenParent(t, rs)
	children := mintPendingChildren(t, rs, parent.ID, 1, run.RunnerKindGitHubActions)
	// Deliberately leave the child's InstallationID nil to exercise the skip
	// arm — fireDispatch must not reach DispatchWorkflow.

	gh := &stubGitHub{}
	au := &recordingAudit{}
	o := &Orchestrator{
		Runs:                rs,
		GitHub:              gh,
		Logger:              slog.Default(),
		Audit:               au,
		DefaultRef:          "main",
		MaxParallelChildren: 0,
		Drive:               &drive.Engine{Audit: au},
	}

	n, err := o.DispatchDecomposedChildren(context.Background(), parent.ID)
	if err != nil {
		t.Fatalf("DispatchDecomposedChildren: %v", err)
	}
	if n != 1 {
		t.Fatalf("dispatched = %d, want 1 (run-state advance even without installation)", n)
	}

	gh.mu.Lock()
	got := len(gh.calls)
	gh.mu.Unlock()
	if got != 0 {
		t.Errorf("workflow_dispatch calls = %d, want 0 (nil installation skips dispatch)", got)
	}

	// Run state still advanced: child running, implement stage dispatched.
	child := children[0]
	if child.State != run.StateRunning {
		t.Errorf("child state = %q, want running (advanced despite skipped dispatch)", child.State)
	}
	if st := implState(t, rs, child.ID); st != run.StageStateDispatched {
		t.Errorf("child implement state = %q, want dispatched", st)
	}
}

// ensure plan parses the schema_version field as we expect.
func TestDecomposedPlanShape(t *testing.T) {
	b := decomposedPlanBytes(t, []string{"A"})
	var p plan.Plan
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if p.Decomposition == nil || len(p.Decomposition.SubPlans) != 1 {
		t.Fatalf("decomposition not parsed correctly: %+v", p.Decomposition)
	}
}
