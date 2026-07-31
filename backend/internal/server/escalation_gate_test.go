package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// --- fixtures ---------------------------------------------------------------

// escGateSpecYAML renders a workflow-v2 document whose `escalations:` block is
// the caller's, so each test declares exactly the grammar it is about and the
// declarations under test come from the real parse pipeline rather than a
// hand-built struct.
func escGateSpecYAML(escalations string) string {
	return `version: "2"
workflows:
  feature_change:
    autonomy: high
` + escalations + `
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        reviewers:
          agents:
            - provider: anthropic
          human: 1
        produces:
          - artifact: plan
            schema: standard_v1
        gates:
          - type: approval
            approvals:
              count: 1
      - id: implement
        type: implement
        executor:
          agent: claude-code
        produces:
          - artifact: pull_request
`
}

// escGatePathsSpec declares one paths-matched escalation raising the count to
// 2 and adding a group — the fixture most of these tests exercise.
const escGatePathsBlock = `    escalations:
      - match:
          paths: ["backend/internal/server/**"]
        require:
          approvals:
            count: 2
            member_of: acme/security
`

func escGateRun(t *testing.T, escalations string, labels []string) *run.Run {
	t.Helper()
	yaml := escGateSpecYAML(escalations)
	if _, err := spec.ParseBytes([]byte(yaml)); err != nil {
		t.Fatalf("fixture spec does not parse: %v", err)
	}
	r := &run.Run{
		ID:            uuid.New(),
		State:         run.StateRunning,
		WorkflowID:    "feature_change",
		WorkflowSpec:  []byte(yaml),
		TriggerSource: run.TriggerCLI,
	}
	if labels != nil {
		r.IssueContext = &run.IssueContext{Labels: labels}
	}
	return r
}

// --- fakes ------------------------------------------------------------------

// escGateRunRepo is the minimal run.Repository slice the escalation resolver
// touches: GetRun, ListStagesForRun. Every other method is unreachable on
// these paths and panics rather than silently returning a zero value.
type escGateRunRepo struct {
	run.Repository
	mu     sync.Mutex
	runs   map[uuid.UUID]*run.Run
	stages map[uuid.UUID][]*run.Stage
	calls  int
	// getErr, when non-nil, is returned by GetRun instead of a seeded row —
	// the transient (non-ErrNotFound) repository error the fail-closed branch
	// must distinguish from a missing row.
	getErr error
}

func newEscGateRunRepo() *escGateRunRepo {
	return &escGateRunRepo{runs: map[uuid.UUID]*run.Run{}, stages: map[uuid.UUID][]*run.Stage{}}
}

func (r *escGateRunRepo) seed(runRow *run.Run, stages ...*run.Stage) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.runs[runRow.ID] = runRow
	r.stages[runRow.ID] = stages
}

func (r *escGateRunRepo) GetRun(_ context.Context, id uuid.UUID) (*run.Run, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	if r.getErr != nil {
		return nil, r.getErr
	}
	got, ok := r.runs[id]
	if !ok {
		return nil, run.ErrNotFound
	}
	return got, nil
}

func (r *escGateRunRepo) ListStagesForRun(_ context.Context, id uuid.UUID) ([]*run.Stage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	return r.stages[id], nil
}

// escGateAuditRepo captures escalation_fired appends and can be made to fail
// on either the READ (the de-duplication list) or the WRITE (the append).
type escGateAuditRepo struct {
	audit.Repository
	mu        sync.Mutex
	entries   []*audit.Entry
	appended  []audit.ChainAppendParams
	appendErr error
	listErr   error
}

func (a *escGateAuditRepo) AppendChained(_ context.Context, p audit.ChainAppendParams) (*audit.Entry, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.appendErr != nil {
		return nil, a.appendErr
	}
	a.appended = append(a.appended, p)
	rid := p.RunID
	e := &audit.Entry{ID: uuid.New(), RunID: &rid, StageID: p.StageID, Category: p.Category, Payload: p.Payload}
	a.entries = append(a.entries, e)
	return e, nil
}

func (a *escGateAuditRepo) ListForRunByCategory(_ context.Context, _ uuid.UUID, category string) ([]*audit.Entry, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.listErr != nil {
		return nil, a.listErr
	}
	var out []*audit.Entry
	for _, e := range a.entries {
		if e.Category == category {
			out = append(out, e)
		}
	}
	return out, nil
}

func (a *escGateAuditRepo) firedCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	for _, p := range a.appended {
		if p.Category == CategoryEscalationFired {
			n++
		}
	}
	return n
}

// escGateArtifactRepo serves ONE plan artifact per plan-stage id, or a list
// error when seeded with one.
type escGateArtifactRepo struct {
	artifact.Repository
	mu      sync.Mutex
	byStage map[uuid.UUID][]*artifact.Artifact
	listErr error
	calls   int
}

func newEscGateArtifactRepo() *escGateArtifactRepo {
	return &escGateArtifactRepo{byStage: map[uuid.UUID][]*artifact.Artifact{}}
}

func (f *escGateArtifactRepo) seedPlan(t *testing.T, stageID uuid.UUID, p plan.Plan) {
	t.Helper()
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	sv := "standard_v1"
	f.mu.Lock()
	defer f.mu.Unlock()
	f.byStage[stageID] = []*artifact.Artifact{{
		ID: uuid.New(), StageID: stageID, Kind: artifact.KindPlan,
		SchemaVersion: &sv, Content: b, CreatedAt: time.Now().UTC(),
	}}
}

func (f *escGateArtifactRepo) ListForStage(_ context.Context, stageID uuid.UUID) ([]*artifact.Artifact, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.byStage[stageID], nil
}

func escGateServer(rr *escGateRunRepo, au *escGateAuditRepo, ar *escGateArtifactRepo) *Server {
	return New(Config{Addr: "127.0.0.1:0", RunRepo: rr, AuditRepo: au, ArtifactRepo: ar})
}

// escGatePlan builds a plan whose top-level scope names files. (The
// applies_to plan-gate suite already owns planWithScope; this one returns a
// value so the decomposition/split variants below can extend it inline.)
func escGatePlan(files ...string) plan.Plan {
	p := plan.Plan{}
	for _, f := range files {
		p.Scope.Files = append(p.Scope.Files, plan.ScopeFile{Path: f})
	}
	return p
}

// --- the no-escalations control ---------------------------------------------

// TestResolveEscalations_NoDeclarations_ZeroExtraReads is the CONTROL the
// whole change rests on: a workflow declaring no escalations takes ZERO extra
// repository reads and resolves to the zero requirement, so every existing run
// is on a byte-identical code path.
func TestResolveEscalations_NoDeclarations_ZeroExtraReads(t *testing.T) {
	rr, au, ar := newEscGateRunRepo(), &escGateAuditRepo{}, newEscGateArtifactRepo()
	s := escGateServer(rr, au, ar)
	runRow := escGateRun(t, "", nil)
	stage := &run.Stage{ID: uuid.New(), RunID: runRow.ID, Type: run.StageTypePlan}
	rr.seed(runRow, stage)

	wf, err := s.escalationsForRun(runRow)
	if err != nil {
		t.Fatalf("escalationsForRun: %v", err)
	}
	if wf != nil {
		t.Fatalf("escalationsForRun returned a workflow for a spec declaring no escalations")
	}

	res, err := s.resolveEscalations(context.Background(), runRow, nil, stage.ID)
	if err != nil {
		t.Fatalf("resolveEscalations: %v", err)
	}
	if res.Any() || !res.Requirements.IsZero() {
		t.Fatalf("Result = %+v, want the zero result", res)
	}
	if ar.calls != 0 {
		t.Errorf("artifact repo calls = %d, want 0 (no plan read on the no-escalations path)", ar.calls)
	}
	if au.firedCount() != 0 {
		t.Errorf("escalation_fired entries = %d, want 0", au.firedCount())
	}
}

// --- the plan scope UNION ----------------------------------------------------

// TestResolveEscalations_PlanScopeUnion proves the union across sub_plans and
// split_proposal phases is what the criterion is evaluated against. A
// decomposed run's fan-out child runs bounded to its SLICE scope, so a clean
// top-level scope with an escalated path in a sub-plan MUST still fire.
func TestResolveEscalations_PlanScopeUnion(t *testing.T) {
	subPlanScope := &plan.Scope{Files: []plan.ScopeFile{{Path: "backend/internal/server/runs.go"}}}
	phaseScope := &plan.Scope{Files: []plan.ScopeFile{{Path: "backend/internal/server/quorum.go"}}}

	cases := []struct {
		name string
		p    plan.Plan
		want bool
	}{
		{"top-level scope inside the escalated path fires",
			escGatePlan("backend/internal/server/runs.go"), true},
		{"clean top level, sub_plan reaches the escalated path — fires",
			func() plan.Plan {
				p := escGatePlan("docs/README.md")
				p.Decomposition = &plan.Decomposition{SubPlans: []plan.SubPlanSummary{{Scope: subPlanScope}}}
				return p
			}(), true},
		{"clean top level, split_proposal phase reaches the escalated path — fires",
			func() plan.Plan {
				p := escGatePlan("docs/README.md")
				p.SplitProposal = &plan.SplitProposal{Phases: []plan.SplitPhase{{Scope: phaseScope}}}
				return p
			}(), true},
		{"nothing anywhere in the escalated path — does not fire",
			escGatePlan("docs/README.md", "cli/main.go"), false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr, au, ar := newEscGateRunRepo(), &escGateAuditRepo{}, newEscGateArtifactRepo()
			s := escGateServer(rr, au, ar)
			runRow := escGateRun(t, escGatePathsBlock, nil)
			planStage := &run.Stage{ID: uuid.New(), RunID: runRow.ID, Type: run.StageTypePlan}
			rr.seed(runRow, planStage)
			ar.seedPlan(t, planStage.ID, tc.p)

			wf, err := s.escalationsForRun(runRow)
			if err != nil || wf == nil {
				t.Fatalf("escalationsForRun = %v, %v", wf, err)
			}
			res, err := s.resolveEscalations(context.Background(), runRow, wf, planStage.ID)
			if err != nil {
				t.Fatalf("resolveEscalations: %v", err)
			}
			if res.Any() != tc.want {
				t.Fatalf("fired = %v, want %v (fired set %+v)", res.Any(), tc.want, res.Fired)
			}
			if tc.want {
				if res.Requirements.Count == nil || *res.Requirements.Count != 2 {
					t.Errorf("Count = %v, want 2", res.Requirements.Count)
				}
				if len(res.Requirements.MemberOf) != 1 || res.Requirements.MemberOf[0] != "acme/security" {
					t.Errorf("MemberOf = %v, want [acme/security]", res.Requirements.MemberOf)
				}
			}
		})
	}
}

// TestResolveEscalations_Labels: a label-only escalation reads the run's
// IMMUTABLE issue-label snapshot and never touches the plan — so a run with no
// plan yet is not refused by a declaration that does not need one.
func TestResolveEscalations_Labels(t *testing.T) {
	const block = `    escalations:
      - match:
          labels: ["security"]
        require:
          max_autonomy: low
`
	for _, tc := range []struct {
		name   string
		labels []string
		want   bool
	}{
		{"matching label fires", []string{"security", "area:backend"}, true},
		{"other labels do not fire", []string{"area:backend"}, false},
		{"absent issue context does not fire", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rr, au, ar := newEscGateRunRepo(), &escGateAuditRepo{}, newEscGateArtifactRepo()
			s := escGateServer(rr, au, ar)
			runRow := escGateRun(t, block, tc.labels)
			rr.seed(runRow)

			wf, _ := s.escalationsForRun(runRow)
			res, err := s.resolveEscalations(context.Background(), runRow, wf, uuid.Nil)
			if err != nil {
				t.Fatalf("resolveEscalations: %v", err)
			}
			if res.Any() != tc.want {
				t.Fatalf("fired = %v, want %v", res.Any(), tc.want)
			}
			if ar.calls != 0 {
				t.Errorf("artifact calls = %d, want 0 (a label-only declaration needs no plan)", ar.calls)
			}
			if tc.want && res.Requirements.MaxAutonomy != spec.TierLow {
				t.Errorf("MaxAutonomy = %q, want low", res.Requirements.MaxAutonomy)
			}
		})
	}
}

// --- fail-closed branches ----------------------------------------------------

// TestResolveEscalations_MatchError_FailsClosed: a malformed glob reaching the
// gate returns an error, never an unescalated zero requirement.
//
// The declaration is built directly rather than parsed, because spec
// validation rejects a malformed glob at parse time — which is exactly the
// point: this is the defence-in-depth branch for a spec that reached the gate
// having bypassed validation (an older binary's cached bytes).
func TestResolveEscalations_MatchError_FailsClosed(t *testing.T) {
	rr, au, ar := newEscGateRunRepo(), &escGateAuditRepo{}, newEscGateArtifactRepo()
	s := escGateServer(rr, au, ar)
	runRow := escGateRun(t, escGatePathsBlock, nil)
	planStage := &run.Stage{ID: uuid.New(), RunID: runRow.ID, Type: run.StageTypePlan}
	rr.seed(runRow, planStage)
	ar.seedPlan(t, planStage.ID, escGatePlan("backend/internal/server/runs.go"))

	wf, _ := s.escalationsForRun(runRow)
	wf.Escalations[0].Match.Paths = []string{"backend/[unclosed"}

	res, err := s.resolveEscalations(context.Background(), runRow, wf, planStage.ID)
	if err == nil {
		t.Fatal("resolveEscalations returned nil error on a malformed glob; the gate would proceed unescalated")
	}
	if !res.Requirements.IsZero() {
		t.Errorf("Requirements = %+v, want zero alongside the error", res.Requirements)
	}
	if au.firedCount() != 0 {
		t.Errorf("an escalation_fired entry was written for an unevaluable declaration")
	}
}

// TestResolveEscalations_PlanUnreadable_FailsClosed covers BOTH named
// unreadable-plan modes — the artifact read erroring, and no plan artifact
// existing at all — while a paths-bearing escalation is declared.
func TestResolveEscalations_PlanUnreadable_FailsClosed(t *testing.T) {
	cases := []struct {
		name  string
		setup func(rr *escGateRunRepo, ar *escGateArtifactRepo, runRow *run.Run)
	}{
		{"artifact read fails", func(rr *escGateRunRepo, ar *escGateArtifactRepo, runRow *run.Run) {
			rr.seed(runRow, &run.Stage{ID: uuid.New(), RunID: runRow.ID, Type: run.StageTypePlan})
			ar.listErr = errors.New("artifact store down")
		}},
		{"no plan artifact exists", func(rr *escGateRunRepo, ar *escGateArtifactRepo, runRow *run.Run) {
			rr.seed(runRow, &run.Stage{ID: uuid.New(), RunID: runRow.ID, Type: run.StageTypePlan})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr, au, ar := newEscGateRunRepo(), &escGateAuditRepo{}, newEscGateArtifactRepo()
			s := escGateServer(rr, au, ar)
			runRow := escGateRun(t, escGatePathsBlock, nil)
			tc.setup(rr, ar, runRow)

			wf, _ := s.escalationsForRun(runRow)
			res, err := s.resolveEscalations(context.Background(), runRow, wf, uuid.Nil)
			if err == nil {
				t.Fatal("resolveEscalations returned nil error with an unreadable plan and a paths-bearing escalation")
			}
			if !errors.Is(err, errEscalationPlanUnreadable) {
				t.Errorf("error = %v, want the plan-unreadable sentinel", err)
			}
			if !res.Requirements.IsZero() {
				t.Errorf("Requirements = %+v, want zero alongside the error", res.Requirements)
			}
		})
	}
}

// --- the single audit emit point --------------------------------------------

// TestEscalationFiredAudit_EmitAndDedup pins the de-duplication contract as it
// is actually implemented (operator condition 2): de-duplicated across
// SEQUENTIAL evaluations, with a genuinely CHANGED fired set writing a second
// entry.
func TestEscalationFiredAudit_EmitAndDedup(t *testing.T) {
	rr, au, ar := newEscGateRunRepo(), &escGateAuditRepo{}, newEscGateArtifactRepo()
	s := escGateServer(rr, au, ar)
	runRow := escGateRun(t, escGatePathsBlock, nil)
	planStage := &run.Stage{ID: uuid.New(), RunID: runRow.ID, Type: run.StageTypePlan}
	rr.seed(runRow, planStage)
	ar.seedPlan(t, planStage.ID, escGatePlan("backend/internal/server/runs.go"))
	wf, _ := s.escalationsForRun(runRow)

	for i := 0; i < 2; i++ {
		if _, err := s.resolveEscalations(context.Background(), runRow, wf, planStage.ID); err != nil {
			t.Fatalf("resolveEscalations pass %d: %v", i, err)
		}
	}
	if n := au.firedCount(); n != 1 {
		t.Fatalf("escalation_fired entries after two unchanged passes = %d, want 1", n)
	}

	got := au.appended[0]
	if got.StageID == nil || *got.StageID != planStage.ID {
		t.Errorf("entry stage_id = %v, want %s", got.StageID, planStage.ID)
	}
	var payload escalationFiredPayload
	if err := json.Unmarshal(got.Payload, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if !strings.Contains(payload.Summary, "backend/internal/server/**") {
		t.Errorf("payload summary %q does not name the fired predicate", payload.Summary)
	}
	if payload.Count == nil || *payload.Count != 2 {
		t.Errorf("payload required_count = %v, want 2", payload.Count)
	}
	if len(payload.MemberOf) != 1 || payload.MemberOf[0] != "acme/security" {
		t.Errorf("payload required_member_of = %v, want [acme/security]", payload.MemberOf)
	}

	// A CHANGED requirement on the SAME stage writes a SECOND entry — "the
	// gate got stricter again" is exactly what this audit exists to surface.
	three := 3
	wf.Escalations[0].Require.Approvals.Count = &three
	if _, err := s.resolveEscalations(context.Background(), runRow, wf, planStage.ID); err != nil {
		t.Fatalf("resolveEscalations after the change: %v", err)
	}
	if n := au.firedCount(); n != 2 {
		t.Fatalf("escalation_fired entries after a CHANGED fired set = %d, want 2", n)
	}
}

// TestEscalationFiredAudit_MaxAutonomyOnly_NoApprovalGate is operator
// condition B's case: an escalation whose ONLY requirement is max_autonomy, so
// the approval seam never raises anything, still writes an entry when the
// DELEGATION seam evaluates it. This is the firing that previously changed
// behaviour silently.
func TestEscalationFiredAudit_MaxAutonomyOnly_NoApprovalGate(t *testing.T) {
	rr, au, ar := newEscGateRunRepo(), &escGateAuditRepo{}, newEscGateArtifactRepo()
	s := escGateServer(rr, au, ar)
	runRow := escGateGatelessRun(t)
	rr.seed(runRow)
	wf, _ := s.escalationsForRun(runRow)
	if wf == nil {
		t.Fatal("the gateless fixture declares no escalations")
	}

	// Drive it through the DELEGATION seam's adapter, not the approval one.
	req, err := s.escalationResolver().ResolveEscalations(context.Background(), runRow, wf, uuid.Nil)
	if err != nil {
		t.Fatalf("ResolveEscalations: %v", err)
	}
	if req.MaxAutonomy != spec.TierLow {
		t.Fatalf("MaxAutonomy = %q, want low", req.MaxAutonomy)
	}
	if n := au.firedCount(); n != 1 {
		t.Fatalf("escalation_fired entries = %d, want 1 — a max_autonomy-only escalation must be audited at the delegation seam", n)
	}
	var payload escalationFiredPayload
	if uerr := json.Unmarshal(au.appended[0].Payload, &payload); uerr != nil {
		t.Fatalf("unmarshal payload: %v", uerr)
	}
	if payload.MaxAutonomy != string(spec.TierLow) {
		t.Errorf("payload max_autonomy = %q, want low", payload.MaxAutonomy)
	}
	if !strings.Contains(payload.Summary, "labels=security") {
		t.Errorf("payload summary %q does not name the fired predicate", payload.Summary)
	}
	if au.appended[0].StageID != nil {
		t.Errorf("entry stage_id = %v, want nil when no stage is gated", au.appended[0].StageID)
	}
}

// TestEscalationFiredAudit_AppendFails_StillEnforced is operator condition 1's
// missing failure-mode test: the audit append fails, and the escalation is
// STILL enforced. Failing the gate because a log line could not be written
// would convert an audit-store outage into a governance outage — strictly
// worse than an unrecorded raise, because the raise itself already happened.
func TestEscalationFiredAudit_AppendFails_StillEnforced(t *testing.T) {
	rr, au, ar := newEscGateRunRepo(), &escGateAuditRepo{}, newEscGateArtifactRepo()
	au.appendErr = errors.New("audit store down")
	s := escGateServer(rr, au, ar)
	// Capture the warning so the required "logged, naming the escalation"
	// half of the error path is ASSERTED, not merely present (operator
	// condition 1): without this the test would pass even if the warn were
	// dropped or stopped naming the raise.
	var logBuf bytes.Buffer
	s.cfg.Logger = slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	runRow := escGateRun(t, escGatePathsBlock, nil)
	planStage := &run.Stage{ID: uuid.New(), RunID: runRow.ID, Type: run.StageTypePlan}
	rr.seed(runRow, planStage)
	ar.seedPlan(t, planStage.ID, escGatePlan("backend/internal/server/runs.go"))
	wf, _ := s.escalationsForRun(runRow)

	res, err := s.resolveEscalations(context.Background(), runRow, wf, planStage.ID)
	if err != nil {
		t.Fatalf("resolveEscalations returned an error on an audit-append failure; the raise must not depend on the log line: %v", err)
	}
	if !res.Any() {
		t.Fatal("the escalation did not fire when the audit append failed")
	}
	if res.Requirements.Count == nil || *res.Requirements.Count != 2 {
		t.Errorf("Count = %v, want the escalated 2 — enforcement is independent of the audit write", res.Requirements.Count)
	}

	// A single WARN must be emitted, and it must NAME the escalation summary so
	// the raise is recoverable from the application log even though the audit
	// row was lost.
	logged := logBuf.String()
	if !strings.Contains(logged, "escalation_fired audit append failed") {
		t.Errorf("no append-failure warning was logged: %q", logged)
	}
	summary := "backend/internal/server/**"
	if !strings.Contains(logged, summary) {
		t.Errorf("append-failure warning does not name the escalation (%q missing): %q", summary, logged)
	}
	if !strings.Contains(logged, au.appendErr.Error()) {
		t.Errorf("append-failure warning does not carry the underlying error %q: %q", au.appendErr.Error(), logged)
	}
}

// TestEscalationFiredAudit_ReadFails_EmitsAnyway: a de-duplication READ
// failure emits anyway (fail toward visibility). A duplicate entry is benign;
// a missing one defeats the surface the audit exists to provide.
func TestEscalationFiredAudit_ReadFails_EmitsAnyway(t *testing.T) {
	rr, au, ar := newEscGateRunRepo(), &escGateAuditRepo{}, newEscGateArtifactRepo()
	au.listErr = errors.New("audit read down")
	s := escGateServer(rr, au, ar)
	runRow := escGateRun(t, escGatePathsBlock, nil)
	planStage := &run.Stage{ID: uuid.New(), RunID: runRow.ID, Type: run.StageTypePlan}
	rr.seed(runRow, planStage)
	ar.seedPlan(t, planStage.ID, escGatePlan("backend/internal/server/runs.go"))
	wf, _ := s.escalationsForRun(runRow)

	for i := 0; i < 2; i++ {
		if _, err := s.resolveEscalations(context.Background(), runRow, wf, planStage.ID); err != nil {
			t.Fatalf("resolveEscalations pass %d: %v", i, err)
		}
	}
	if n := au.firedCount(); n != 2 {
		t.Errorf("escalation_fired entries with an unreadable de-duplication list = %d, want 2 (emit anyway)", n)
	}
}

// TestEscalationFiredAudit_NilAuditRepo: no audit repository wired means no
// entry and no panic — the escalation is still enforced.
func TestEscalationFiredAudit_NilAuditRepo(t *testing.T) {
	rr, ar := newEscGateRunRepo(), newEscGateArtifactRepo()
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, ArtifactRepo: ar})
	runRow := escGateRun(t, escGatePathsBlock, nil)
	planStage := &run.Stage{ID: uuid.New(), RunID: runRow.ID, Type: run.StageTypePlan}
	rr.seed(runRow, planStage)
	ar.seedPlan(t, planStage.ID, escGatePlan("backend/internal/server/runs.go"))
	wf, _ := s.escalationsForRun(runRow)

	res, err := s.resolveEscalations(context.Background(), runRow, wf, planStage.ID)
	if err != nil {
		t.Fatalf("resolveEscalations: %v", err)
	}
	if !res.Any() {
		t.Error("the escalation did not fire with no audit repository wired")
	}
}

// --- runtime lowering is impossible -----------------------------------------

// TestEffectiveApprovals_NeverLowers is the runtime half of only-ever-raise:
// even an escalated value BELOW the baseline yields the BASELINE, so a spec
// that bypassed declaration-time validation still cannot weaken a gate.
func TestEffectiveApprovals_NeverLowers(t *testing.T) {
	n := func(i int) *int { return &i }
	base := &spec.Approvals{Count: n(3), MinPermission: "maintain", MemberOf: "acme/leads"}

	cases := []struct {
		name    string
		base    *spec.Approvals
		req     spec.ComposedRequirements
		count   int
		perm    string
		members []string
	}{
		{"no escalation leaves the baseline untouched", base, spec.ComposedRequirements{},
			3, "maintain", []string{"acme/leads"}},
		{"a LOWER escalated count yields the baseline", base,
			spec.ComposedRequirements{Count: n(1)}, 3, "maintain", []string{"acme/leads"}},
		{"a WEAKER escalated permission yields the baseline", base,
			spec.ComposedRequirements{MinPermission: "read"}, 3, "maintain", []string{"acme/leads"}},
		{"a higher escalated count raises", base,
			spec.ComposedRequirements{Count: n(5)}, 5, "maintain", []string{"acme/leads"}},
		{"a stricter escalated permission raises", base,
			spec.ComposedRequirements{MinPermission: "admin"}, 3, "admin", []string{"acme/leads"}},
		{"membership is a conjunction, never a replacement", base,
			spec.ComposedRequirements{MemberOf: []string{"acme/security"}},
			3, "maintain", []string{"acme/leads", "acme/security"}},
		{"a duplicate group de-duplicates", base,
			spec.ComposedRequirements{MemberOf: []string{"acme/leads"}},
			3, "maintain", []string{"acme/leads"}},
		{"a nil baseline defaults to a count of 1 and takes every raise", nil,
			spec.ComposedRequirements{Count: n(2), MinPermission: "write", MemberOf: []string{"acme/security"}},
			2, "write", []string{"acme/security"}},
		{"an escalation raises a gate declaring no predicate at all",
			&spec.Approvals{Count: n(1)},
			spec.ComposedRequirements{MemberOf: []string{"acme/security"}, MinPermission: "admin"},
			1, "admin", []string{"acme/security"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := effectiveApprovals(tc.base, tc.req)
			if got.count != tc.count {
				t.Errorf("count = %d, want %d", got.count, tc.count)
			}
			if got.minPermission != tc.perm {
				t.Errorf("minPermission = %q, want %q", got.minPermission, tc.perm)
			}
			if strings.Join(got.memberOf, ",") != strings.Join(tc.members, ",") {
				t.Errorf("memberOf = %v, want %v", got.memberOf, tc.members)
			}
		})
	}

	t.Run("the parsed spec is never mutated", func(t *testing.T) {
		b := &spec.Approvals{Count: n(1), MinPermission: "write", MemberOf: "acme/leads"}
		effectiveApprovals(b, spec.ComposedRequirements{
			Count: n(9), MinPermission: "admin", MemberOf: []string{"acme/security"}})
		if *b.Count != 1 || b.MinPermission != "write" || b.MemberOf != "acme/leads" {
			t.Errorf("baseline mutated: %+v", b)
		}
	})
}

// TestRenderMemberOf pins the shape-compatibility rule: one group renders as
// the pre-#2227 bare string (byte-identical audit payloads for a run with no
// fired escalation), a genuine conjunction as the list, none as nil.
func TestRenderMemberOf(t *testing.T) {
	if got := renderMemberOf(nil); got != nil {
		t.Errorf("renderMemberOf(nil) = %v, want nil", got)
	}
	if got := renderMemberOf([]string{"acme/leads"}); got != "acme/leads" {
		t.Errorf("renderMemberOf(one) = %v, want the bare string", got)
	}
	got, ok := renderMemberOf([]string{"a", "b"}).([]string)
	if !ok || len(got) != 2 {
		t.Errorf("renderMemberOf(two) = %v, want the list", got)
	}
}

// escGateGatelessRun is the operator-condition-B fixture: a workflow-v2
// document declaring NO approval gate anywhere, so the approval seam can never
// run, and ONE escalation whose only requirement is `max_autonomy`. Its
// behaviour changes purely through the delegation clamp — the firing that
// previously went unaudited.
func escGateGatelessRun(t *testing.T) *run.Run {
	t.Helper()
	const yaml = `version: "2"
workflows:
  feature_change:
    autonomy: high
    escalations:
      - match:
          labels: ["security"]
        require:
          max_autonomy: low
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
	parsed, err := spec.ParseBytes([]byte(yaml))
	if err != nil {
		t.Fatalf("gateless fixture does not parse: %v", err)
	}
	wf := parsed.Workflows["feature_change"]
	for _, st := range wf.Stages {
		for _, g := range st.Gates {
			if g.Type == spec.GateTypeApproval {
				t.Fatalf("the gateless fixture declares an approval gate on stage %q; the condition-B claim would be vacuous", st.ID)
			}
		}
	}
	return &run.Run{
		ID: uuid.New(), State: run.StateRunning,
		WorkflowID: "feature_change", WorkflowSpec: []byte(yaml),
		TriggerSource: run.TriggerCLI,
		IssueContext:  &run.IssueContext{Labels: []string{"security"}},
	}
}

// TestEscalationsForRun_Degradations covers the three "declares none" answers
// that are NOT this gate's fail-closed modes, each with the reason it degrades
// rather than refuses.
func TestEscalationsForRun_Degradations(t *testing.T) {
	s := escGateServer(newEscGateRunRepo(), &escGateAuditRepo{}, newEscGateArtifactRepo())

	cases := []struct {
		name   string
		runRow *run.Run
	}{
		{"nil run row", nil},
		{"no cached spec (a legacy / manual run)", &run.Run{ID: uuid.New(), WorkflowID: "feature_change"}},
		{"unparseable cached spec", &run.Run{ID: uuid.New(), WorkflowID: "feature_change", WorkflowSpec: []byte("{not yaml")}},
		{"workflow absent from its own cached spec", &run.Run{
			ID: uuid.New(), WorkflowID: "other", WorkflowSpec: []byte(escGateSpecYAML(escGatePathsBlock))}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wf, err := s.escalationsForRun(tc.runRow)
			if err != nil {
				t.Fatalf("escalationsForRun: %v, want a degrade not an error", err)
			}
			if wf != nil {
				t.Errorf("workflow = %+v, want nil", wf)
			}
		})
	}
}

// TestResolveStageEscalations_UnreadableRunRow degrades to the zero
// requirement rather than refusing. The declaration lives in the cached spec
// bytes ON the unreadable row, so refusing would not enforce an escalation we
// could otherwise have found — it would only 503 every legacy / spec-less run's
// approval on a path where fetchApprovalsForStage already degrades to the
// legacy gate on the SAME read.
func TestResolveStageEscalations_UnreadableRunRow(t *testing.T) {
	rr := newEscGateRunRepo() // nothing seeded: GetRun returns ErrNotFound
	s := escGateServer(rr, &escGateAuditRepo{}, newEscGateArtifactRepo())
	stage := &run.Stage{ID: uuid.New(), RunID: uuid.New(), Type: run.StageTypePlan}

	req, err := s.resolveStageEscalations(context.Background(), stage)
	if err != nil {
		t.Fatalf("resolveStageEscalations: %v, want a degrade not an error", err)
	}
	if !req.IsZero() {
		t.Errorf("requirements = %+v, want zero", req)
	}
}

// TestResolveStageEscalations_TransientRunRowError_FailsClosed pins the #2227
// fixup: a TRANSIENT (non-ErrNotFound) GetRun error is NOT the legacy-row
// degrade. The run row — and the escalation declaration on it — may exist, so
// degrading to the baseline would be the "proceed unescalated" outcome the file
// header forbids; the resolver must fail closed, which the approval seam turns
// into a retryable 503 and the count seam into a not-advancing gate.
func TestResolveStageEscalations_TransientRunRowError_FailsClosed(t *testing.T) {
	rr := newEscGateRunRepo()
	rr.getErr = errors.New("db connection reset") // NOT run.ErrNotFound
	s := escGateServer(rr, &escGateAuditRepo{}, newEscGateArtifactRepo())
	stage := &run.Stage{ID: uuid.New(), RunID: uuid.New(), Type: run.StageTypePlan}

	req, err := s.resolveStageEscalations(context.Background(), stage)
	if err == nil {
		t.Fatal("resolveStageEscalations returned nil error on a transient GetRun failure; the gate would proceed unescalated")
	}
	if errors.Is(err, run.ErrNotFound) {
		t.Errorf("error = %v, want the transient error surfaced, not the not-found degrade", err)
	}
	if !req.IsZero() {
		t.Errorf("requirements = %+v, want zero alongside the error", req)
	}
}

// TestResolveStageEscalations_ChangedPlanScope_DivergesBetweenResolutions is
// the #2227 TOCTOU pin: the resolver reads MUTABLE plan scope, so two
// resolutions of the SAME stage around a scope change return DIFFERENT fired
// sets. This is the divergence the pre-Submit predicate check and the
// post-Submit quorum count sit on — a change whose scope did not match when a
// pre-Submit predicate check ran (no membership enforced, approval recorded)
// can match by the time the quorum count reads the amended plan.
//
// Membership is enforced at INSERT time (checkApprovalPredicates), and the
// count path CLOSES the residual cross-request window this divergence sits on
// (#2227): when a fired escalation raises a forge predicate, approveStageAs
// re-resolves it against the forge for every counted approver
// (countEscalatedForgeApprovers), so an approval recorded during the
// non-matching window — never membership-checked at Submit — is re-checked at
// quorum time and excluded if it does not satisfy the raised requirement. This
// test pins the resolver-level divergence (the same stage returns a different
// fired set before and after a scope amendment); the count-time closure of the
// window that divergence exposes is pinned end to end by
// TestApproveStageAs_Escalation_CountTimeMembershipReValidation.
func TestResolveStageEscalations_ChangedPlanScope_DivergesBetweenResolutions(t *testing.T) {
	rr, au, ar := newEscGateRunRepo(), &escGateAuditRepo{}, newEscGateArtifactRepo()
	s := escGateServer(rr, au, ar)
	runRow := escGateRun(t, escGatePathsBlock, nil)
	planStage := &run.Stage{ID: uuid.New(), RunID: runRow.ID, Type: run.StageTypePlan}
	rr.seed(runRow, planStage)

	// T1: the plan scope does NOT reach the escalated path — nothing fires, so
	// a pre-Submit predicate check here would enforce no membership.
	ar.seedPlan(t, planStage.ID, escGatePlan("docs/README.md"))
	before, err := s.resolveStageEscalations(context.Background(), planStage)
	if err != nil {
		t.Fatalf("resolveStageEscalations (pre-change): %v", err)
	}
	if !before.IsZero() {
		t.Fatalf("requirements = %+v, want zero — the plan scope does not match the escalation yet", before)
	}

	// A scope amendment moves the plan into the escalated path (the mutable read
	// the two seams share).
	ar.seedPlan(t, planStage.ID, escGatePlan("backend/internal/server/runs.go"))

	// T2: the SAME stage now fires and raises the count + membership — the
	// changed fired set the count seam would enforce against approvals some of
	// which were recorded before the change.
	after, err := s.resolveStageEscalations(context.Background(), planStage)
	if err != nil {
		t.Fatalf("resolveStageEscalations (post-change): %v", err)
	}
	if after.IsZero() {
		t.Fatal("requirements are zero after the plan scope moved into the escalated path; the fired set did not track the mutable plan")
	}
	if after.Count == nil || *after.Count != 2 {
		t.Errorf("Count = %v, want the escalated 2 after the scope change", after.Count)
	}
	if len(after.MemberOf) != 1 || after.MemberOf[0] != "acme/security" {
		t.Errorf("MemberOf = %v, want [acme/security] after the scope change", after.MemberOf)
	}
}

// TestResolveStageEscalations_NoRunRepo: no run repository wired means no
// escalation can be resolved and none is claimed.
func TestResolveStageEscalations_NoRunRepo(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0"})
	stage := &run.Stage{ID: uuid.New(), RunID: uuid.New(), Type: run.StageTypePlan}
	req, err := s.resolveStageEscalations(context.Background(), stage)
	if err != nil {
		t.Fatalf("resolveStageEscalations: %v", err)
	}
	if !req.IsZero() {
		t.Errorf("requirements = %+v, want zero", req)
	}
}
