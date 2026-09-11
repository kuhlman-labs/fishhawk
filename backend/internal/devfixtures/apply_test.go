package devfixtures_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/approval"
	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/devfixtures"
	"github.com/kuhlman-labs/fishhawk/backend/internal/devfixtures/catalog"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// --- fakes ---------------------------------------------------------------
//
// One armory is shared by every fake so a table row can arm "the Nth call
// of method M fails with errArmed" and the test asserts the returned
// error names the handle that call was materializing. The run fake
// embeds run.BaseFake and the audit fake audit.BaseFake, so the two
// broad repository interfaces stay satisfied if they grow; the
// artifact/approval stores are one-method interfaces and need no base.

var errArmed = errors.New("armed store failure")

type armory struct {
	counts     map[string]int
	failMethod string
	failCall   int // 1-based ordinal of the failing call
}

func (a *armory) hit(method string) error {
	if a.counts == nil {
		a.counts = map[string]int{}
	}
	a.counts[method]++
	if method == a.failMethod && a.counts[method] == a.failCall {
		return errArmed
	}
	return nil
}

type runTransition struct {
	ID uuid.UUID
	To run.State
}

type stageTransition struct {
	ID         uuid.UUID
	To         run.StageState
	Completion *run.StageCompletion
}

type fakeRuns struct {
	run.BaseFake
	arm              *armory
	createdRuns      []run.CreateRunParams
	runIDs           []uuid.UUID
	runTransitions   []runTransition
	createdStages    []run.CreateStageParams
	stageIDs         []uuid.UUID
	stageTransitions []stageTransition
}

func (f *fakeRuns) CreateRun(_ context.Context, p run.CreateRunParams) (*run.Run, error) {
	if err := f.arm.hit("CreateRun"); err != nil {
		return nil, err
	}
	id := uuid.New()
	f.createdRuns = append(f.createdRuns, p)
	f.runIDs = append(f.runIDs, id)
	return &run.Run{ID: id, State: run.StatePending}, nil
}

func (f *fakeRuns) TransitionRun(_ context.Context, id uuid.UUID, to run.State) (*run.Run, error) {
	if err := f.arm.hit("TransitionRun"); err != nil {
		return nil, err
	}
	f.runTransitions = append(f.runTransitions, runTransition{ID: id, To: to})
	return &run.Run{ID: id, State: to}, nil
}

func (f *fakeRuns) CreateStage(_ context.Context, p run.CreateStageParams) (*run.Stage, error) {
	if err := f.arm.hit("CreateStage"); err != nil {
		return nil, err
	}
	id := uuid.New()
	f.createdStages = append(f.createdStages, p)
	f.stageIDs = append(f.stageIDs, id)
	return &run.Stage{ID: id, RunID: p.RunID, State: run.StageStatePending}, nil
}

func (f *fakeRuns) TransitionStage(_ context.Context, id uuid.UUID, to run.StageState, completion *run.StageCompletion) (*run.Stage, error) {
	if err := f.arm.hit("TransitionStage"); err != nil {
		return nil, err
	}
	f.stageTransitions = append(f.stageTransitions, stageTransition{ID: id, To: to, Completion: completion})
	return &run.Stage{ID: id, State: to}, nil
}

// transitionsFor returns the ordered TransitionStage targets recorded
// for one stage id.
func (f *fakeRuns) transitionsFor(id uuid.UUID) []stageTransition {
	var out []stageTransition
	for _, tr := range f.stageTransitions {
		if tr.ID == id {
			out = append(out, tr)
		}
	}
	return out
}

type fakeArtifacts struct {
	arm     *armory
	created []artifact.CreateParams
}

func (f *fakeArtifacts) Create(_ context.Context, p artifact.CreateParams) (*artifact.Artifact, error) {
	if err := f.arm.hit("ArtifactCreate"); err != nil {
		return nil, err
	}
	f.created = append(f.created, p)
	return &artifact.Artifact{ID: uuid.New(), StageID: p.StageID, Kind: p.Kind}, nil
}

type fakeAudit struct {
	audit.BaseFake
	arm      *armory
	appended []audit.ChainAppendParams
}

func (f *fakeAudit) AppendChained(_ context.Context, p audit.ChainAppendParams) (*audit.Entry, error) {
	if err := f.arm.hit("AppendChained"); err != nil {
		return nil, err
	}
	f.appended = append(f.appended, p)
	return &audit.Entry{ID: uuid.New(), Category: p.Category, Timestamp: p.Timestamp}, nil
}

type fakeApprovals struct {
	arm       *armory
	submitted []approval.SubmitParams
}

func (f *fakeApprovals) Submit(_ context.Context, p approval.SubmitParams) (*approval.SubmitResult, error) {
	if err := f.arm.hit("Submit"); err != nil {
		return nil, err
	}
	f.submitted = append(f.submitted, p)
	return &approval.SubmitResult{Approval: &approval.Approval{ID: uuid.New(), StageID: p.StageID}, Inserted: true}, nil
}

type fakes struct {
	arm       *armory
	runs      *fakeRuns
	artifacts *fakeArtifacts
	audit     *fakeAudit
	approvals *fakeApprovals
}

func newFakes(now time.Time) (fakes, devfixtures.Deps) {
	arm := &armory{}
	f := fakes{
		arm:       arm,
		runs:      &fakeRuns{arm: arm},
		artifacts: &fakeArtifacts{arm: arm},
		audit:     &fakeAudit{arm: arm},
		approvals: &fakeApprovals{arm: arm},
	}
	deps := devfixtures.Deps{
		Runs:      f.runs,
		Artifacts: f.artifacts,
		Audit:     f.audit,
		Approvals: f.approvals,
		Now:       func() time.Time { return now },
	}
	return f, deps
}

// The real repositories must satisfy the narrow stores unchanged; the
// fakes must too. A widened store method surfaces here as a compile
// error, not as a pg test failing to build.
var (
	_ devfixtures.RunStore      = run.BaseFake{}
	_ devfixtures.AuditStore    = audit.BaseFake{}
	_ devfixtures.RunStore      = (*fakeRuns)(nil)
	_ devfixtures.ArtifactStore = (*fakeArtifacts)(nil)
	_ devfixtures.AuditStore    = (*fakeAudit)(nil)
	_ devfixtures.ApprovalStore = (*fakeApprovals)(nil)
)

// walkScenario is a hand-built scenario carrying one stage per reachable
// declared state, declared OUT of sequence order so the sequence-order
// create is observable. It borrows grooming-confirm-gate's validated
// workflow spec so Validate admits it.
func walkScenario(t *testing.T) *devfixtures.Scenario {
	t.Helper()
	base := mustLoad(t, "grooming-confirm-gate")
	s := &devfixtures.Scenario{
		Name: "walk",
		Runs: []devfixtures.Run{{
			Key: "r", Repo: "kuhlman-labs/fishhawk", WorkflowID: "backlog_grooming",
			WorkflowSpec: base.Runs[0].WorkflowSpec, TriggerRef: "issue:9", State: "running",
		}},
		Stages: []devfixtures.Stage{
			{Key: "s-failed", Run: "r", Sequence: 6, Type: "review", ExecutorKind: "human", ExecutorRef: "human", State: "failed", FailureCategory: "A", FailureReason: "seeded"},
			{Key: "s-pending", Run: "r", Sequence: 1, Type: "plan", ExecutorKind: "agent", ExecutorRef: "claude-code", State: "pending"},
			{Key: "s-dispatched", Run: "r", Sequence: 2, Type: "plan", ExecutorKind: "agent", ExecutorRef: "claude-code", State: "dispatched"},
			{Key: "s-running", Run: "r", Sequence: 3, Type: "implement", ExecutorKind: "agent", ExecutorRef: "claude-code", State: "running"},
			{Key: "s-succeeded", Run: "r", Sequence: 4, Type: "plan", ExecutorKind: "agent", ExecutorRef: "claude-code", RequiresApproval: true, State: "succeeded"},
			{Key: "s-awaiting", Run: "r", Sequence: 5, Type: "review", ExecutorKind: "human", ExecutorRef: "human", RequiresApproval: true, State: "awaiting_approval"},
		},
	}
	if err := s.Validate(); err != nil {
		t.Fatalf("control: walkScenario does not validate: %v", err)
	}
	return s
}

// TestApply_WalksCanonicalStagePath asserts the EXACT TransitionStage
// sequence Apply records for every reachable declared state, that only
// the failed transition carries a StageCompletion (with the declared
// category and reason), that stages are created in sequence order
// regardless of declaration order, and that the Result maps each handle
// to the id the store minted for it.
func TestApply_WalksCanonicalStagePath(t *testing.T) {
	f, deps := newFakes(time.Date(2026, 1, 1, 10, 59, 59, 0, time.UTC))
	s := walkScenario(t)

	res, err := devfixtures.Apply(context.Background(), deps, s)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res.Scenario != "walk" {
		t.Errorf("Result.Scenario = %q, want walk", res.Scenario)
	}
	rr, ok := res.Runs["r"]
	if !ok || rr.ID != f.runs.runIDs[0] {
		t.Fatalf("Result.Runs[r] = %+v, want the minted run id %s", rr, f.runs.runIDs[0])
	}

	// Stages created in sequence order 1..6 even though "s-failed" (6)
	// was declared first.
	wantOrder := []string{"s-pending", "s-dispatched", "s-running", "s-succeeded", "s-awaiting", "s-failed"}
	if len(f.runs.createdStages) != len(wantOrder) {
		t.Fatalf("created %d stages, want %d", len(f.runs.createdStages), len(wantOrder))
	}
	for i, key := range wantOrder {
		if f.runs.createdStages[i].Sequence != i+1 {
			t.Errorf("createdStages[%d].Sequence = %d, want %d", i, f.runs.createdStages[i].Sequence, i+1)
		}
		if rr.Stages[key] != f.runs.stageIDs[i] {
			t.Errorf("Result stage %q = %s, want minted id %s (create ordinal %d)", key, rr.Stages[key], f.runs.stageIDs[i], i)
		}
		if f.runs.createdStages[i].RunID != rr.ID {
			t.Errorf("createdStages[%d].RunID = %s, want %s", i, f.runs.createdStages[i].RunID, rr.ID)
		}
	}
	// The create params carry the declared shape verbatim.
	succeededParams := f.runs.createdStages[3]
	if succeededParams.Type != run.StageTypePlan || succeededParams.ExecutorKind != run.ExecutorAgent ||
		succeededParams.ExecutorRef != "claude-code" || !succeededParams.RequiresApproval {
		t.Errorf("s-succeeded create params = %+v, want plan/agent/claude-code/requires_approval", succeededParams)
	}

	walks := map[string][]run.StageState{
		"s-pending":    nil,
		"s-dispatched": {run.StageStateDispatched},
		"s-running":    {run.StageStateDispatched, run.StageStateRunning},
		"s-succeeded":  {run.StageStateDispatched, run.StageStateRunning, run.StageStateSucceeded},
		"s-awaiting":   {run.StageStateDispatched, run.StageStateRunning, run.StageStateAwaitingApproval},
		"s-failed":     {run.StageStateDispatched, run.StageStateRunning, run.StageStateFailed},
	}
	for key, want := range walks {
		got := f.runs.transitionsFor(rr.Stages[key])
		var gotStates []run.StageState
		for _, tr := range got {
			gotStates = append(gotStates, tr.To)
			if tr.To != run.StageStateFailed && tr.Completion != nil {
				t.Errorf("stage %q: transition to %s carried a completion %+v, want nil", key, tr.To, tr.Completion)
			}
		}
		if !reflect.DeepEqual(gotStates, want) {
			t.Errorf("stage %q: TransitionStage sequence = %v, want %v", key, gotStates, want)
		}
	}
	failedWalk := f.runs.transitionsFor(rr.Stages["s-failed"])
	last := failedWalk[len(failedWalk)-1]
	if last.Completion == nil || last.Completion.FailureCategory == nil || *last.Completion.FailureCategory != run.FailureA ||
		last.Completion.FailureReason == nil || *last.Completion.FailureReason != "seeded" {
		t.Errorf("failed transition completion = %+v, want category A reason \"seeded\"", last.Completion)
	}
	// Total transition count pins that no stage received an extra step.
	if got, want := len(f.runs.stageTransitions), 0+1+2+3+3+3; got != want {
		t.Errorf("total TransitionStage calls = %d, want %d", got, want)
	}
}

// TestApply_FailedStageDefaultsReason pins the fallback reason for a
// failed stage declared without failure_reason (Validate only requires
// the category).
func TestApply_FailedStageDefaultsReason(t *testing.T) {
	f, deps := newFakes(time.Now())
	s := walkScenario(t)
	s.Stages = s.Stages[:1] // just s-failed
	s.Stages[0].FailureReason = ""
	if _, err := devfixtures.Apply(context.Background(), deps, s); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	last := f.runs.stageTransitions[len(f.runs.stageTransitions)-1]
	if last.To != run.StageStateFailed || last.Completion == nil || last.Completion.FailureReason == nil || *last.Completion.FailureReason == "" {
		t.Fatalf("failed transition = %+v, want a non-empty default reason", last)
	}
}

// TestApply_WalksCanonicalRunPath asserts the TransitionRun sequence per
// declared run state: pending writes nothing, every terminal state is
// reached THROUGH running.
func TestApply_WalksCanonicalRunPath(t *testing.T) {
	cases := map[string][]run.State{
		"pending":   nil,
		"running":   {run.StateRunning},
		"succeeded": {run.StateRunning, run.StateSucceeded},
		"failed":    {run.StateRunning, run.StateFailed},
		"cancelled": {run.StateRunning, run.StateCancelled},
	}
	for state, want := range cases {
		t.Run(state, func(t *testing.T) {
			f, deps := newFakes(time.Now())
			s := mustLoad(t, "plan-gate-parked")
			s.Runs[0].State = state
			res, err := devfixtures.Apply(context.Background(), deps, s)
			if err != nil {
				t.Fatalf("Apply: %v", err)
			}
			var got []run.State
			for _, tr := range f.runs.runTransitions {
				if tr.ID != res.Runs["parked-run"].ID {
					t.Errorf("TransitionRun on unexpected id %s", tr.ID)
				}
				got = append(got, tr.To)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("TransitionRun sequence = %v, want %v", got, want)
			}
		})
	}
}

// TestApply_RunParams pins what CreateRun receives: an untenanted
// github_issue-triggered run carrying the inline spec bytes, the
// declared trigger ref, and a WorkflowSHA that is the sha256 of the spec.
func TestApply_RunParams(t *testing.T) {
	f, deps := newFakes(time.Now())
	s := mustLoad(t, "trace-upload-target")
	if _, err := devfixtures.Apply(context.Background(), deps, s); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(f.runs.createdRuns) != 1 {
		t.Fatalf("CreateRun calls = %d, want 1", len(f.runs.createdRuns))
	}
	p := f.runs.createdRuns[0]
	sum := sha256.Sum256([]byte(s.Runs[0].WorkflowSpec))
	if p.Repo != "kuhlman-labs/fishhawk" || p.WorkflowID != "feature_change" ||
		p.TriggerSource != run.TriggerGitHubIssue || p.TriggerRef == nil || *p.TriggerRef != "issue:3" ||
		string(p.WorkflowSpec) != s.Runs[0].WorkflowSpec || p.WorkflowSHA != hex.EncodeToString(sum[:]) {
		t.Errorf("CreateRunParams = %+v, want repo/workflow/github_issue/issue:3/spec bytes/sha256(spec)", p)
	}
	if p.InstallationID != nil || p.InstallationRef != nil || p.IdempotencyKey != nil || p.ParentRunID != nil {
		t.Errorf("CreateRunParams carries tenancy/lineage fields it must not: %+v", p)
	}
}

// TestApply_BackdatesAuditRowsByAge pins Timestamp == Now − age for every
// aged row and Timestamp == Now for a row without age, with the stage
// handle resolved to the minted stage id and the actor fields threaded.
// Counterfactual: delete the `now.Add(-age)` subtraction in apply.go →
// every aged row lands at Now → RED.
func TestApply_BackdatesAuditRowsByAge(t *testing.T) {
	now := time.Date(2026, 1, 1, 10, 59, 59, 0, time.UTC)
	f, deps := newFakes(now)
	s := mustLoad(t, "trace-upload-target")
	// One un-aged, stage-less row with no subject so the nil branches
	// are observed alongside the aged rows.
	s.Audit = append(s.Audit, devfixtures.AuditRow{
		Run: "target-run", Category: "run_auto_driven", ActorKind: "agent", Payload: `{}`,
	})
	if err := s.Validate(); err != nil {
		t.Fatalf("control: scenario does not validate: %v", err)
	}

	res, err := devfixtures.Apply(context.Background(), deps, s)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(f.audit.appended) != len(s.Audit) {
		t.Fatalf("AppendChained calls = %d, want %d", len(f.audit.appended), len(s.Audit))
	}
	runID := res.Runs["target-run"].ID
	planID := res.Runs["target-run"].Stages["plan"]
	wantAges := []time.Duration{65 * time.Minute, 2 * time.Hour, 3 * time.Hour, 4 * time.Hour, 0}
	for i, p := range f.audit.appended {
		want := now.Add(-wantAges[i])
		if !p.Timestamp.Equal(want) {
			t.Errorf("audit[%d] Timestamp = %s, want Now−%s = %s", i, p.Timestamp, wantAges[i], want)
		}
		if p.RunID != runID {
			t.Errorf("audit[%d] RunID = %s, want %s", i, p.RunID, runID)
		}
		if p.Category != s.Audit[i].Category {
			t.Errorf("audit[%d] Category = %q, want %q", i, p.Category, s.Audit[i].Category)
		}
		if p.ActorKind == nil || string(*p.ActorKind) != s.Audit[i].ActorKind {
			t.Errorf("audit[%d] ActorKind = %v, want %q", i, p.ActorKind, s.Audit[i].ActorKind)
		}
		if string(p.Payload) != s.Audit[i].Payload {
			t.Errorf("audit[%d] Payload = %s, want %s", i, p.Payload, s.Audit[i].Payload)
		}
	}
	aged := f.audit.appended[0]
	if aged.StageID == nil || *aged.StageID != planID {
		t.Errorf("aged row StageID = %v, want plan stage %s", aged.StageID, planID)
	}
	if aged.ActorSubject == nil || *aged.ActorSubject != "devfixtures" {
		t.Errorf("aged row ActorSubject = %v, want devfixtures", aged.ActorSubject)
	}
	bare := f.audit.appended[4]
	if bare.StageID != nil {
		t.Errorf("stage-less row StageID = %v, want nil", bare.StageID)
	}
	if bare.ActorSubject != nil {
		t.Errorf("subject-less row ActorSubject = %v, want nil", bare.ActorSubject)
	}
}

// TestApply_ArtifactsAndApprovals pins the artifact create params (kind,
// sha256 content hash over the content bytes, schema pointer) and the
// approval submit params (decision, surface, comment pointer) against
// the grooming-confirm-gate scenario, plus the nil branches for an empty
// schema_version and comment.
func TestApply_ArtifactsAndApprovals(t *testing.T) {
	f, deps := newFakes(time.Now())
	s := mustLoad(t, "grooming-confirm-gate")
	s.Artifacts = append(s.Artifacts, devfixtures.Artifact{Key: "bare", Stage: "confirm", Kind: "plan", Content: `{}`})
	s.Approvals = append(s.Approvals, devfixtures.Approval{Stage: "confirm", ApproverSubject: "other", Decision: "reject", Surface: "cli"})
	if err := s.Validate(); err != nil {
		t.Fatalf("control: scenario does not validate: %v", err)
	}

	res, err := devfixtures.Apply(context.Background(), deps, s)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	stages := res.Runs["groom-run"].Stages
	if len(f.artifacts.created) != 2 {
		t.Fatalf("artifact Create calls = %d, want 2", len(f.artifacts.created))
	}
	report := f.artifacts.created[0]
	sum := sha256.Sum256([]byte(s.Artifacts[0].Content))
	if report.StageID != stages["groom"] || report.Kind != artifact.KindGroomingReport ||
		report.SchemaVersion == nil || *report.SchemaVersion != "grooming_report_v1" ||
		string(report.Content) != s.Artifacts[0].Content || report.ContentHash != hex.EncodeToString(sum[:]) {
		t.Errorf("report artifact params = %+v, want groom stage / grooming_report / grooming_report_v1 / sha256(content)", report)
	}
	if bare := f.artifacts.created[1]; bare.StageID != stages["confirm"] || bare.SchemaVersion != nil {
		t.Errorf("bare artifact params = %+v, want confirm stage with nil SchemaVersion", bare)
	}

	if len(f.approvals.submitted) != 2 {
		t.Fatalf("approval Submit calls = %d, want 2", len(f.approvals.submitted))
	}
	first := f.approvals.submitted[0]
	if first.StageID != stages["groom"] || first.ApproverSubject != "fixture-operator" ||
		first.Decision != approval.DecisionApprove || first.Surface != approval.SurfaceAPI ||
		first.Comment == nil || *first.Comment != s.Approvals[0].Comment {
		t.Errorf("first approval params = %+v, want groom / fixture-operator / approve / api / comment", first)
	}
	second := f.approvals.submitted[1]
	if second.StageID != stages["confirm"] || second.Decision != approval.DecisionReject ||
		second.Surface != approval.SurfaceCLI || second.Comment != nil {
		t.Errorf("second approval params = %+v, want confirm / reject / cli / nil comment", second)
	}
}

// TestApply_ErrorBranches arms every store call Apply makes to fail and
// asserts the returned error wraps the armed sentinel AND names the
// handle (or index) being materialized. The Deps/scenario refusals that
// precede any write are in the same table so the whole fail-closed
// surface is enumerated in one place.
func TestApply_ErrorBranches(t *testing.T) {
	type tc struct {
		name     string
		deps     func(devfixtures.Deps) devfixtures.Deps
		scenario func(t *testing.T) *devfixtures.Scenario
		arm      string
		call     int
		wantSub  []string
		wantArm  bool
		wantNone bool // no store call at all must have been recorded
	}
	ident := func(d devfixtures.Deps) devfixtures.Deps { return d }
	grooming := func(t *testing.T) *devfixtures.Scenario {
		return goodScenario(t) // grooming-confirm-gate + one aged audit row
	}
	cases := []tc{
		{name: "nil Runs", deps: func(d devfixtures.Deps) devfixtures.Deps { d.Runs = nil; return d }, scenario: grooming, wantSub: []string{"Deps.Runs is nil"}, wantNone: true},
		{name: "nil Artifacts", deps: func(d devfixtures.Deps) devfixtures.Deps { d.Artifacts = nil; return d }, scenario: grooming, wantSub: []string{"Deps.Artifacts is nil"}, wantNone: true},
		{name: "nil Audit", deps: func(d devfixtures.Deps) devfixtures.Deps { d.Audit = nil; return d }, scenario: grooming, wantSub: []string{"Deps.Audit is nil"}, wantNone: true},
		{name: "nil Approvals", deps: func(d devfixtures.Deps) devfixtures.Deps { d.Approvals = nil; return d }, scenario: grooming, wantSub: []string{"Deps.Approvals is nil"}, wantNone: true},
		{name: "nil scenario", deps: ident, scenario: func(*testing.T) *devfixtures.Scenario { return nil }, wantSub: []string{"nil scenario"}, wantNone: true},
		{name: "invalid scenario refused before any write", deps: ident, scenario: func(t *testing.T) *devfixtures.Scenario {
			s := grooming(t)
			s.Stages[1].Run = "ghost-run"
			return s
		}, wantSub: []string{`scenario "grooming-confirm-gate"`, `stage "confirm"`, `unknown run handle "ghost-run"`}, wantNone: true},
		{name: "CreateRun fails", deps: ident, scenario: grooming, arm: "CreateRun", call: 1, wantSub: []string{`run "groom-run"`, "create"}, wantArm: true},
		{name: "TransitionRun fails", deps: ident, scenario: grooming, arm: "TransitionRun", call: 1, wantSub: []string{`run "groom-run"`, "transition to running"}, wantArm: true},
		{name: "CreateStage fails on second stage", deps: ident, scenario: grooming, arm: "CreateStage", call: 2, wantSub: []string{`stage "confirm"`, "create"}, wantArm: true},
		{name: "TransitionStage fails on first stage's terminal step", deps: ident, scenario: grooming, arm: "TransitionStage", call: 3, wantSub: []string{`stage "groom"`, "transition to succeeded"}, wantArm: true},
		{name: "TransitionStage fails on second stage's park", deps: ident, scenario: grooming, arm: "TransitionStage", call: 6, wantSub: []string{`stage "confirm"`, "transition to awaiting_approval"}, wantArm: true},
		{name: "artifact Create fails", deps: ident, scenario: grooming, arm: "ArtifactCreate", call: 1, wantSub: []string{`artifact "report"`}, wantArm: true},
		{name: "approval Submit fails", deps: ident, scenario: grooming, arm: "Submit", call: 1, wantSub: []string{"approvals[0]", `stage "groom"`}, wantArm: true},
		{name: "AppendChained fails", deps: ident, scenario: grooming, arm: "AppendChained", call: 1, wantSub: []string{"audit[0]", "cost_recorded", `run "groom-run"`}, wantArm: true},
		{name: "AppendChained fails on the fourth baseline row", deps: ident, scenario: func(t *testing.T) *devfixtures.Scenario { return mustLoad(t, "trace-upload-target") }, arm: "AppendChained", call: 4, wantSub: []string{"audit[3]", "cost_recorded", `run "target-run"`}, wantArm: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f, deps := newFakes(time.Now())
			f.arm.failMethod, f.arm.failCall = c.arm, c.call
			deps = c.deps(deps)
			res, err := devfixtures.Apply(context.Background(), deps, c.scenario(t))
			if err == nil {
				t.Fatalf("Apply succeeded, want an error; result %+v", res)
			}
			if res.Runs != nil {
				t.Errorf("failed Apply returned a populated Result %+v, want zero", res)
			}
			for _, sub := range c.wantSub {
				if !strings.Contains(err.Error(), sub) {
					t.Errorf("error %q does not contain %q", err, sub)
				}
			}
			if c.wantArm && !errors.Is(err, errArmed) {
				t.Errorf("error %q does not wrap the armed sentinel", err)
			}
			if c.wantNone && len(f.arm.counts) != 0 {
				t.Errorf("a pre-write refusal still reached the stores: %v", f.arm.counts)
			}
		})
	}
}

// TestApply_DefaultsClock pins the nil-Now branch: with Deps.Now unset a
// row without age is dated close to wall-clock now, not the zero time.
func TestApply_DefaultsClock(t *testing.T) {
	f, deps := newFakes(time.Time{})
	deps.Now = nil
	s := mustLoad(t, "trace-upload-target")
	before := time.Now()
	if _, err := devfixtures.Apply(context.Background(), deps, s); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	after := time.Now()
	// The 1h5m row must land at wall-clock-now − 1h5m, bracketed by the
	// call window.
	got := f.audit.appended[0].Timestamp
	if got.Before(before.Add(-65*time.Minute)) || got.After(after.Add(-65*time.Minute)) {
		t.Errorf("Timestamp with nil Now = %s, want within [%s, %s]", got, before.Add(-65*time.Minute), after.Add(-65*time.Minute))
	}
}

// TestApplier_LoadsByCatalogName pins the name-keyed wrapper the dev
// route consumes: Names/Describe mirror the catalog, Apply on a known
// name materializes it (Result.Scenario echoes the name), and an unknown
// name wraps ErrUnknownScenario naming the known set WITHOUT touching a
// store.
func TestApplier_LoadsByCatalogName(t *testing.T) {
	f, deps := newFakes(time.Now())
	a := devfixtures.NewApplier(deps)

	if !reflect.DeepEqual(a.Names(), catalog.Names()) {
		t.Errorf("Names() = %v, want catalog %v", a.Names(), catalog.Names())
	}
	for _, n := range catalog.Names() {
		if a.Describe(n) != catalog.Description(n) || a.Describe(n) == "" {
			t.Errorf("Describe(%q) = %q, want catalog description", n, a.Describe(n))
		}
	}
	if a.Describe("nope") != "" {
		t.Errorf("Describe(nope) = %q, want empty", a.Describe("nope"))
	}

	_, err := a.Apply(context.Background(), "not-a-scenario")
	if !errors.Is(err, devfixtures.ErrUnknownScenario) {
		t.Fatalf("Apply(unknown) error = %v, want ErrUnknownScenario", err)
	}
	for _, n := range catalog.Names() {
		if !strings.Contains(err.Error(), n) {
			t.Errorf("unknown-name error %q does not list %q", err, n)
		}
	}
	if len(f.arm.counts) != 0 {
		t.Errorf("unknown name reached the stores: %v", f.arm.counts)
	}

	res, err := a.Apply(context.Background(), "plan-gate-parked")
	if err != nil {
		t.Fatalf("Apply(plan-gate-parked): %v", err)
	}
	if res.Scenario != "plan-gate-parked" || len(res.Runs) != 1 || len(res.Runs["parked-run"].Stages) != 1 {
		t.Errorf("Result = %+v, want scenario plan-gate-parked with one run carrying one stage", res)
	}
	if len(f.artifacts.created) != 1 || f.artifacts.created[0].Kind != artifact.KindPlan {
		t.Errorf("artifacts created = %+v, want one plan artifact", f.artifacts.created)
	}
}
