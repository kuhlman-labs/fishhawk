package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
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

func (r *fanoutRunsRepo) ListRuns(_ context.Context, f run.ListRunsFilter) ([]*run.Run, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.listFilters = append(r.listFilters, f)
	if r.listResult != nil {
		return r.listResult, nil
	}
	return nil, nil
}

// fakeArtifacts is a minimal artifact.Repository returning a fixed
// list of artifacts for any stage.
type fakeArtifacts struct {
	byStage map[uuid.UUID][]*artifact.Artifact
}

func (f *fakeArtifacts) Create(context.Context, artifact.CreateParams) (*artifact.Artifact, error) {
	return nil, nil
}
func (f *fakeArtifacts) Get(_ context.Context, id uuid.UUID) (*artifact.Artifact, error) {
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
func (r *recordingAudit) ListForRunByCategory(context.Context, uuid.UUID, string) ([]*audit.Entry, error) {
	return nil, nil
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

	rs.mu.Lock()
	defer rs.mu.Unlock()
	if got, want := len(rs.createdRuns), 3; got != want {
		t.Fatalf("createdRuns = %d, want %d", got, want)
	}
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
