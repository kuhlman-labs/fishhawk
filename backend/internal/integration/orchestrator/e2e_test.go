// Package orchestratore2e_test drives the full decomposition lifecycle
// end-to-end against a real Postgres database: a seeded parent run
// with a decomposed plan artifact fans out to two child runs, each
// child is driven to a terminal state, and the child-completion
// sweeper resolves the parent.
//
// NOTE on step (m) assertion: the sweeper's internal Advance call
// (wired via advancerAdapter{o}) dispatches the review stage during
// Tick. A second explicit Advance therefore finds no pending stages
// and completes the run (OutcomeRunCompleted), not OutcomeDispatched
// as the plan anticipated. The same applies to the failure-path final
// Advance: it returns OutcomeNoOp because the sweeper's Advance
// already drove the parent to failed inside Tick.
package orchestratore2e_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/kuhlman-labs/fishhawk/backend/internal/apitoken"
	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/childcompletion"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/orchestrator"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/postgres"
	runpkg "github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/scopeamendment"
	"github.com/kuhlman-labs/fishhawk/backend/internal/server"
)

// advancerAdapter satisfies childcompletion.Advancer by wrapping
// *orchestrator.Orchestrator. Orchestrator.Advance returns (Outcome,
// error); the Advancer interface requires only error. Mirrors
// the serve.go childCompletionAdvancer adapter.
type advancerAdapter struct{ o *orchestrator.Orchestrator }

func (a advancerAdapter) Advance(ctx context.Context, runID uuid.UUID) error {
	_, err := a.o.Advance(ctx, runID)
	return err
}

// IntegrateSlices widens advancerAdapter to childcompletion.Integrator
// (ADR-041 / #1142), converting the orchestrator's *SliceConflict to
// childcompletion's identical type — mirrors the serve.go adapter.
func (a advancerAdapter) IntegrateSlices(ctx context.Context, parentRunID uuid.UUID) (*childcompletion.SliceConflict, error) {
	conflict, err := a.o.IntegrateSlices(ctx, parentRunID)
	if err != nil || conflict == nil {
		return nil, err
	}
	return &childcompletion.SliceConflict{
		SliceIndex: conflict.SliceIndex,
		ChildRunID: conflict.ChildRunID,
		Detail:     conflict.Detail,
	}, nil
}

// startPostgres spins up a postgres:16-alpine container via
// testcontainers-go, applies all migrations, opens a pool, and
// registers t.Cleanup for both. Skips the test when Docker is
// unavailable.
func startPostgres(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	c, err := tcpostgres.Run(ctx,
		"postgres:16-alpine",
		tcpostgres.WithDatabase("fishhawk"),
		tcpostgres.WithUsername("fishhawk"),
		tcpostgres.WithPassword("fishhawk"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		if isDockerUnavailable(err) {
			t.Skipf("Docker not available; skipping orchestrator E2E: %v", err)
		}
		t.Fatalf("start postgres: %v", err)
	}
	t.Cleanup(func() {
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer shutCancel()
		_ = c.Terminate(shutCtx)
	})

	pgURL, err := c.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("postgres connection string: %v", err)
	}
	if err := postgres.MigrateUp(pgURL); err != nil {
		t.Fatalf("MigrateUp: %v", err)
	}
	pool, err := pgxpool.New(ctx, pgURL)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func isDockerUnavailable(err error) bool {
	if err == nil {
		return false
	}
	if os.Getenv("FISHHAWK_SKIP_INTEGRATION") != "" {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, marker := range []string{
		"cannot connect to the docker daemon",
		"docker: not found",
		"executable file not found",
		"dial unix /var/run/docker.sock",
	} {
		if strings.Contains(msg, strings.ToLower(marker)) {
			return true
		}
	}
	return errors.Is(err, exec.ErrNotFound)
}

// decomposedPlanContent builds a valid standard_v1 plan JSON with two
// sub_plans ('Part A' and 'Part B'). Mirrors decomposedPlanBytes in
// orchestrator/fanout_test.go.
func decomposedPlanContent(t *testing.T) []byte {
	t.Helper()
	subs := []map[string]any{
		{
			"title":                        "Part A",
			"scope_hint":                   "scope hint for Part A",
			"predicted_runtime_minutes":    10,
			"predicted_runtime_confidence": "high",
		},
		{
			"title":                        "Part B",
			"scope_hint":                   "scope hint for Part B",
			"predicted_runtime_minutes":    10,
			"predicted_runtime_confidence": "high",
		},
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

// parentRunFixture holds the IDs created by seedParentRun.
type parentRunFixture struct {
	runID uuid.UUID
}

// seedParentRun creates a run with three stages (plan, implement,
// review), drives the plan stage to succeeded, and inserts a
// decomposed plan artifact so the orchestrator's fanout path fires.
// installationID and reviewKind let callers exercise the GitHub-wired
// consolidated-PR path (#714): a nil installationID makes fireDispatch /
// the consolidated-PR path skip GitHub silently, and a human reviewKind
// parks the review at awaiting_approval without a workflow_dispatch.
func seedParentRun(t *testing.T, ctx context.Context, runRepo runpkg.Repository, artifactRepo artifact.Repository, planBytes []byte, installationID *int64, reviewKind runpkg.ExecutorKind, workflowSpec []byte) parentRunFixture {
	t.Helper()

	r, err := runRepo.CreateRun(ctx, runpkg.CreateRunParams{
		Repo:           "kuhlman-labs/fishhawk",
		WorkflowID:     "feature_change",
		WorkflowSHA:    "deadbeef",
		TriggerSource:  runpkg.TriggerCLI,
		InstallationID: installationID,
		WorkflowSpec:   workflowSpec,
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	planStage, err := runRepo.CreateStage(ctx, runpkg.CreateStageParams{
		RunID:        r.ID,
		Sequence:     0,
		Type:         runpkg.StageTypePlan,
		ExecutorKind: runpkg.ExecutorAgent,
		ExecutorRef:  "claude-code",
	})
	if err != nil {
		t.Fatalf("CreateStage plan: %v", err)
	}

	if _, err = runRepo.CreateStage(ctx, runpkg.CreateStageParams{
		RunID:        r.ID,
		Sequence:     1,
		Type:         runpkg.StageTypeImplement,
		ExecutorKind: runpkg.ExecutorAgent,
		ExecutorRef:  "claude-code",
	}); err != nil {
		t.Fatalf("CreateStage implement: %v", err)
	}

	reviewRef := "claude-code"
	if reviewKind == runpkg.ExecutorHuman {
		reviewRef = "human"
	}
	if _, err = runRepo.CreateStage(ctx, runpkg.CreateStageParams{
		RunID:        r.ID,
		Sequence:     2,
		Type:         runpkg.StageTypeReview,
		ExecutorKind: reviewKind,
		ExecutorRef:  reviewRef,
	}); err != nil {
		t.Fatalf("CreateStage review: %v", err)
	}

	// Transition plan stage: pending → dispatched → running → succeeded
	for _, to := range []runpkg.StageState{
		runpkg.StageStateDispatched,
		runpkg.StageStateRunning,
		runpkg.StageStateSucceeded,
	} {
		if _, err := runRepo.TransitionStage(ctx, planStage.ID, to, nil); err != nil {
			t.Fatalf("TransitionStage plan to %s: %v", to, err)
		}
	}

	sum := sha256.Sum256(planBytes)
	contentHash := hex.EncodeToString(sum[:])
	schemaV := "standard_v1"
	if _, err := artifactRepo.Create(ctx, artifact.CreateParams{
		StageID:       planStage.ID,
		Kind:          artifact.KindPlan,
		SchemaVersion: &schemaV,
		Content:       planBytes,
		ContentHash:   contentHash,
	}); err != nil {
		t.Fatalf("Create artifact: %v", err)
	}

	return parentRunFixture{runID: r.ID}
}

// TestDecomposition_E2E_HappyPath verifies the full decomposition
// lifecycle: parent fans out to two children, both children succeed,
// the sweeper resolves the parent implement stage, and the
// orchestrator completes the run.
func TestDecomposition_E2E_HappyPath(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()

	runRepo := runpkg.NewPostgresRepository(pool)
	artifactRepo := artifact.NewPostgresRepository(pool)
	auditRepo := audit.NewPostgresRepository(pool)

	o := &orchestrator.Orchestrator{
		Runs:      runRepo,
		Artifacts: artifactRepo,
		Audit:     auditRepo,
		Logger:    slog.Default(),
	}
	sw := &childcompletion.Sweeper{
		Runs:    runRepo,
		Audit:   auditRepo,
		Advance: advancerAdapter{o: o},
		Logger:  slog.Default(),
	}

	planBytes := decomposedPlanContent(t)
	fx := seedParentRun(t, ctx, runRepo, artifactRepo, planBytes, nil, runpkg.ExecutorAgent, nil)
	parentID := fx.runID

	// (e) First Advance: orchestrator detects decomposition and fans out.
	outcome, err := o.Advance(ctx, parentID)
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if outcome != orchestrator.OutcomeDecomposed {
		t.Errorf("Advance outcome = %q, want %q", outcome, orchestrator.OutcomeDecomposed)
	}

	// (f) Two child runs minted, each linked to parentID.
	children, err := runRepo.ListRuns(ctx, runpkg.ListRunsFilter{
		DecomposedFrom: &parentID,
		Limit:          100,
	})
	if err != nil {
		t.Fatalf("ListRuns children: %v", err)
	}
	if got := len(children); got != 2 {
		t.Fatalf("children = %d, want 2", got)
	}

	// (g) Parent implement stage parked in awaiting_children.
	stages, err := runRepo.ListStagesForRun(ctx, parentID)
	if err != nil {
		t.Fatalf("ListStagesForRun: %v", err)
	}
	var implStage *runpkg.Stage
	for _, s := range stages {
		if s.Type == runpkg.StageTypeImplement {
			implStage = s
			break
		}
	}
	if implStage == nil {
		t.Fatal("implement stage not found")
	}
	if implStage.State != runpkg.StageStateAwaitingChildren {
		t.Errorf("implement stage = %q, want awaiting_children", implStage.State)
	}

	// (h) plan_decomposed audit entry recorded.
	decomposed, err := auditRepo.ListForRunByCategory(ctx, parentID, "plan_decomposed")
	if err != nil {
		t.Fatalf("ListForRunByCategory plan_decomposed: %v", err)
	}
	if got := len(decomposed); got != 1 {
		t.Errorf("plan_decomposed entries = %d, want 1", got)
	}

	// (i) Drive each child run to succeeded.
	for _, child := range children {
		if _, err := runRepo.TransitionRun(ctx, child.ID, runpkg.StateRunning); err != nil {
			t.Fatalf("TransitionRun child %s running: %v", child.ID, err)
		}
		if _, err := runRepo.TransitionRun(ctx, child.ID, runpkg.StateSucceeded); err != nil {
			t.Fatalf("TransitionRun child %s succeeded: %v", child.ID, err)
		}
	}

	// (j) Sweeper tick: all children terminal → implement → succeeded,
	// then sweeper's internal Advance dispatches the review stage.
	if err := sw.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	// (k) Parent implement stage now succeeded.
	stages, err = runRepo.ListStagesForRun(ctx, parentID)
	if err != nil {
		t.Fatalf("ListStagesForRun after tick: %v", err)
	}
	for _, s := range stages {
		if s.Type == runpkg.StageTypeImplement {
			implStage = s
			break
		}
	}
	if implStage.State != runpkg.StageStateSucceeded {
		t.Errorf("implement stage after tick = %q, want succeeded", implStage.State)
	}

	// (l) children_settled audit entry recorded.
	settled, err := auditRepo.ListForRunByCategory(ctx, parentID, "children_settled")
	if err != nil {
		t.Fatalf("ListForRunByCategory children_settled: %v", err)
	}
	if got := len(settled); got != 1 {
		t.Errorf("children_settled entries = %d, want 1", got)
	}

	// (m) Sweeper's internal Advance already dispatched review, which is
	// still non-terminal (dispatched). A second explicit Advance finds no
	// pending stage but MUST NOT complete the run while that stage is in
	// flight (#968): it no-ops and the parent stays running at its review.
	outcome, err = o.Advance(ctx, parentID)
	if err != nil {
		t.Fatalf("Advance second: %v", err)
	}
	if outcome != orchestrator.OutcomeNoOp {
		t.Errorf("Advance second outcome = %q, want %q (review still dispatched)", outcome, orchestrator.OutcomeNoOp)
	}
	parent, err := runRepo.GetRun(ctx, parentID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if parent.State != runpkg.StateRunning {
		t.Errorf("parent run state = %q, want running (review stage non-terminal)", parent.State)
	}

	// (n) Drive the review stage to terminal, then Advance completes the
	// run: every stage terminal → OutcomeRunCompleted, state = succeeded.
	var reviewStage *runpkg.Stage
	for _, s := range stages {
		if s.Type == runpkg.StageTypeReview {
			reviewStage = s
			break
		}
	}
	if reviewStage == nil {
		t.Fatal("review stage not found")
	}
	for _, to := range []runpkg.StageState{runpkg.StageStateRunning, runpkg.StageStateSucceeded} {
		if _, err := runRepo.TransitionStage(ctx, reviewStage.ID, to, nil); err != nil {
			t.Fatalf("TransitionStage review %s: %v", to, err)
		}
	}
	outcome, err = o.Advance(ctx, parentID)
	if err != nil {
		t.Fatalf("Advance third: %v", err)
	}
	if outcome != orchestrator.OutcomeRunCompleted {
		t.Errorf("Advance third outcome = %q, want %q", outcome, orchestrator.OutcomeRunCompleted)
	}
	parent, err = runRepo.GetRun(ctx, parentID)
	if err != nil {
		t.Fatalf("GetRun after review terminal: %v", err)
	}
	if parent.State != runpkg.StateSucceeded {
		t.Errorf("parent run state = %q, want succeeded", parent.State)
	}

	// (i) apply_path scoping (#1165/#1213): the deterministic fix-up apply
	// provenance is a fixup_pushed-only audit field. A non-fix-up run — here a
	// decomposition fan-out — must never emit it on ANY audit entry (parent or
	// child). Asserting its absence across the whole run guards against a future
	// refactor threading the field onto a non-fix-up audit surface, the inverse
	// of the mcp fixup_pushed persist test. Crosses the orchestrator → audit
	// persist boundary this harness already exercises.
	allRuns := []uuid.UUID{parentID}
	for _, c := range children {
		allRuns = append(allRuns, c.ID)
	}
	for _, rid := range allRuns {
		entries, err := auditRepo.ListForRun(ctx, rid)
		if err != nil {
			t.Fatalf("ListForRun(%s): %v", rid, err)
		}
		for _, e := range entries {
			if len(e.Payload) == 0 {
				continue
			}
			var payload map[string]any
			if err := json.Unmarshal(e.Payload, &payload); err != nil {
				continue // non-object payloads carry no keys to leak
			}
			if _, leaked := payload["apply_path"]; leaked {
				t.Errorf("audit entry category %q on non-fix-up run %s carries apply_path; the field must be fixup_pushed-only", e.Category, rid)
			}
		}
	}
}

// TestDecomposition_E2E_OneChildFails verifies the failure mode:
// when one child fails, the sweeper transitions the parent implement
// stage to failed and the orchestrator completes the parent run as
// failed.
func TestDecomposition_E2E_OneChildFails(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()

	runRepo := runpkg.NewPostgresRepository(pool)
	artifactRepo := artifact.NewPostgresRepository(pool)
	auditRepo := audit.NewPostgresRepository(pool)

	o := &orchestrator.Orchestrator{
		Runs:      runRepo,
		Artifacts: artifactRepo,
		Audit:     auditRepo,
		Logger:    slog.Default(),
	}
	sw := &childcompletion.Sweeper{
		Runs:    runRepo,
		Audit:   auditRepo,
		Advance: advancerAdapter{o: o},
		Logger:  slog.Default(),
	}

	planBytes := decomposedPlanContent(t)
	fx := seedParentRun(t, ctx, runRepo, artifactRepo, planBytes, nil, runpkg.ExecutorAgent, nil)
	parentID := fx.runID

	// Steps a–e: same setup as happy path through fanout assertion.
	outcome, err := o.Advance(ctx, parentID)
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if outcome != orchestrator.OutcomeDecomposed {
		t.Fatalf("Advance outcome = %q, want %q", outcome, orchestrator.OutcomeDecomposed)
	}

	children, err := runRepo.ListRuns(ctx, runpkg.ListRunsFilter{
		DecomposedFrom: &parentID,
		Limit:          100,
	})
	if err != nil {
		t.Fatalf("ListRuns children: %v", err)
	}
	if got := len(children); got != 2 {
		t.Fatalf("children = %d, want 2", got)
	}

	stages, err := runRepo.ListStagesForRun(ctx, parentID)
	if err != nil {
		t.Fatalf("ListStagesForRun: %v", err)
	}
	var implStage *runpkg.Stage
	for _, s := range stages {
		if s.Type == runpkg.StageTypeImplement {
			implStage = s
			break
		}
	}
	if implStage == nil {
		t.Fatal("implement stage not found")
	}
	if implStage.State != runpkg.StageStateAwaitingChildren {
		t.Errorf("implement stage = %q, want awaiting_children", implStage.State)
	}

	decomposedEntries, err := auditRepo.ListForRunByCategory(ctx, parentID, "plan_decomposed")
	if err != nil {
		t.Fatalf("ListForRunByCategory plan_decomposed: %v", err)
	}
	if got := len(decomposedEntries); got != 1 {
		t.Errorf("plan_decomposed entries = %d, want 1", got)
	}

	// child[0] fails, child[1] succeeds.
	if _, err := runRepo.TransitionRun(ctx, children[0].ID, runpkg.StateRunning); err != nil {
		t.Fatalf("TransitionRun child[0] running: %v", err)
	}
	if _, err := runRepo.TransitionRun(ctx, children[0].ID, runpkg.StateFailed); err != nil {
		t.Fatalf("TransitionRun child[0] failed: %v", err)
	}
	if _, err := runRepo.TransitionRun(ctx, children[1].ID, runpkg.StateRunning); err != nil {
		t.Fatalf("TransitionRun child[1] running: %v", err)
	}
	if _, err := runRepo.TransitionRun(ctx, children[1].ID, runpkg.StateSucceeded); err != nil {
		t.Fatalf("TransitionRun child[1] succeeded: %v", err)
	}

	// Sweeper resolves: implement → failed, then sweeper's internal
	// Advance drives the parent run to failed.
	if err := sw.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	// Parent implement stage = failed.
	stages, err = runRepo.ListStagesForRun(ctx, parentID)
	if err != nil {
		t.Fatalf("ListStagesForRun after tick: %v", err)
	}
	for _, s := range stages {
		if s.Type == runpkg.StageTypeImplement {
			implStage = s
			break
		}
	}
	if implStage.State != runpkg.StageStateFailed {
		t.Errorf("implement stage after tick = %q, want failed", implStage.State)
	}

	// children_settled payload contains the failed child's run ID.
	settled, err := auditRepo.ListForRunByCategory(ctx, parentID, "children_settled")
	if err != nil {
		t.Fatalf("ListForRunByCategory children_settled: %v", err)
	}
	if got := len(settled); got != 1 {
		t.Fatalf("children_settled entries = %d, want 1", got)
	}
	var settledPayload struct {
		ChildRunIDs []string `json:"child_run_ids"`
	}
	if err := json.Unmarshal(settled[0].Payload, &settledPayload); err != nil {
		t.Fatalf("unmarshal children_settled payload: %v", err)
	}
	failedChildID := children[0].ID.String()
	var foundFailed bool
	for _, id := range settledPayload.ChildRunIDs {
		if id == failedChildID {
			foundFailed = true
			break
		}
	}
	if !foundFailed {
		t.Errorf("children_settled payload missing failed child %s; got %v", failedChildID, settledPayload.ChildRunIDs)
	}

	// Sweeper's internal Advance already drove the parent run to failed.
	// Verify the run state, then confirm a redundant Advance is a no-op.
	parent, err := runRepo.GetRun(ctx, parentID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if parent.State != runpkg.StateFailed {
		t.Errorf("parent run state = %q, want failed", parent.State)
	}
	outcome, err = o.Advance(ctx, parentID)
	if err != nil {
		t.Fatalf("Advance on terminal run: %v", err)
	}
	if outcome != orchestrator.OutcomeNoOp {
		t.Errorf("Advance on terminal run = %q, want %q", outcome, orchestrator.OutcomeNoOp)
	}
}

// recordingGitHub stands in for the orchestrator's GitHubAPI in the
// consolidated-PR e2e test. It records every CreatePullRequest call and
// hands back a canned PR so the test can assert the head/base/URL seam
// without a live GitHub.
type recordingGitHub struct {
	createCalls []struct {
		Head string
		Base string
	}
	prURL string

	// Fan-in (#1142) recording + programming.
	branchSHAs     map[string]string
	createRefCalls []string
	mergeCalls     []string
	mergeErrByHead map[string]error
}

func (g *recordingGitHub) DispatchWorkflow(context.Context, int64,
	githubclient.RepoRef, string, string, githubclient.DispatchInputs) error {
	return nil
}

func (g *recordingGitHub) EnableAutoMerge(context.Context, int64,
	githubclient.RepoRef, int, githubclient.MergeMethod) error {
	return nil
}

func (g *recordingGitHub) CreatePullRequest(_ context.Context, _ int64,
	repo githubclient.RepoRef, head, base, _, _ string) (*githubclient.PullRequest, error) {
	g.createCalls = append(g.createCalls, struct {
		Head string
		Base string
	}{Head: head, Base: base})
	url := g.prURL
	if url == "" {
		url = "https://github.com/" + repo.Owner + "/" + repo.Name + "/pull/123"
	}
	return &githubclient.PullRequest{Number: 123, HTMLURL: url, State: "open"}, nil
}

func (g *recordingGitHub) ListOpenPullRequestsByHead(context.Context, int64,
	githubclient.RepoRef, string, string) ([]githubclient.PullRequest, error) {
	return nil, nil
}

func (g *recordingGitHub) GetBranchSHA(_ context.Context, _ int64,
	_ githubclient.RepoRef, branch string) (string, bool, error) {
	sha, ok := g.branchSHAs[branch]
	if !ok {
		return "", false, nil
	}
	return sha, true, nil
}

func (g *recordingGitHub) CreateRef(_ context.Context, _ int64,
	_ githubclient.RepoRef, branch, sha string) error {
	g.createRefCalls = append(g.createRefCalls, branch)
	if g.branchSHAs == nil {
		g.branchSHAs = map[string]string{}
	}
	g.branchSHAs[branch] = sha
	return nil
}

func (g *recordingGitHub) MergeBranch(_ context.Context, _ int64,
	_ githubclient.RepoRef, base, head, _ string) error {
	g.mergeCalls = append(g.mergeCalls, head)
	if err, ok := g.mergeErrByHead[head]; ok {
		return err
	}
	return nil
}

// TestDecomposition_E2E_ConsolidatedPR exercises the #714 / ADR-032 seam
// end-to-end: a decomposed parent fans out, both children settle
// succeeded, and the orchestrator (driven by the sweeper's Advance) opens
// exactly ONE consolidated PR against main, stamps pull_request_url on the
// parent run, and dispatches the review stage to awaiting_approval — NOT
// auto-succeeded. This is the cross-boundary coverage the per-layer units
// can't give (settle → orchestrator → githubclient → run-repo → review
// dispatch in a single test; cf. #618).
func TestDecomposition_E2E_ConsolidatedPR(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()

	runRepo := runpkg.NewPostgresRepository(pool)
	artifactRepo := artifact.NewPostgresRepository(pool)
	auditRepo := audit.NewPostgresRepository(pool)
	gh := &recordingGitHub{}

	o := &orchestrator.Orchestrator{
		Runs:       runRepo,
		Artifacts:  artifactRepo,
		Audit:      auditRepo,
		GitHub:     gh,
		DefaultRef: "main",
		Logger:     slog.Default(),
	}
	sw := &childcompletion.Sweeper{
		Runs:    runRepo,
		Audit:   auditRepo,
		Advance: advancerAdapter{o: o},
		Logger:  slog.Default(),
	}

	planBytes := decomposedPlanContent(t)
	// Wire an installation so the consolidated-PR path runs, and a human
	// review so it parks at awaiting_approval (not auto-merged).
	installID := int64(4242)
	fx := seedParentRun(t, ctx, runRepo, artifactRepo, planBytes, &installID, runpkg.ExecutorHuman, nil)
	parentID := fx.runID

	// Fan out.
	if _, err := o.Advance(ctx, parentID); err != nil {
		t.Fatalf("Advance (fanout): %v", err)
	}
	children, err := runRepo.ListRuns(ctx, runpkg.ListRunsFilter{DecomposedFrom: &parentID, Limit: 100})
	if err != nil {
		t.Fatalf("ListRuns children: %v", err)
	}
	if len(children) != 2 {
		t.Fatalf("children = %d, want 2", len(children))
	}

	// Drive both children to succeeded.
	for _, child := range children {
		if _, err := runRepo.TransitionRun(ctx, child.ID, runpkg.StateRunning); err != nil {
			t.Fatalf("TransitionRun child running: %v", err)
		}
		if _, err := runRepo.TransitionRun(ctx, child.ID, runpkg.StateSucceeded); err != nil {
			t.Fatalf("TransitionRun child succeeded: %v", err)
		}
	}

	// Sweeper tick: resolves the parent implement stage and inline-Advances
	// into the review gate, which opens the consolidated PR.
	if err := sw.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	// Exactly one consolidated PR, head = shared branch, base = main.
	if len(gh.createCalls) != 1 {
		t.Fatalf("CreatePullRequest calls = %d, want 1", len(gh.createCalls))
	}
	wantHead := "fishhawk/run-" + parentID.String()[:8] + "-consolidated"
	if gh.createCalls[0].Head != wantHead {
		t.Errorf("PR head = %q, want %q", gh.createCalls[0].Head, wantHead)
	}
	if gh.createCalls[0].Base != "main" {
		t.Errorf("PR base = %q, want main", gh.createCalls[0].Base)
	}

	// Parent carries the consolidated PR URL.
	parent, err := runRepo.GetRun(ctx, parentID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if parent.PullRequestURL == nil || *parent.PullRequestURL == "" {
		t.Fatal("parent run pull_request_url not stamped")
	}

	// Review dispatched to awaiting_approval — NOT auto-succeeded, and the
	// run is still running (reconciles on the PR's merge).
	stages, err := runRepo.ListStagesForRun(ctx, parentID)
	if err != nil {
		t.Fatalf("ListStagesForRun: %v", err)
	}
	var reviewStage *runpkg.Stage
	for _, s := range stages {
		if s.Type == runpkg.StageTypeReview {
			reviewStage = s
			break
		}
	}
	if reviewStage == nil {
		t.Fatal("review stage not found")
	}
	if reviewStage.State != runpkg.StageStateAwaitingApproval {
		t.Errorf("review stage = %q, want awaiting_approval (not auto-succeeded)", reviewStage.State)
	}
	if parent.State != runpkg.StateRunning {
		t.Errorf("parent run state = %q, want running", parent.State)
	}

	// consolidated_pr_opened audit entry recorded once.
	opened, err := auditRepo.ListForRunByCategory(ctx, parentID, "consolidated_pr_opened")
	if err != nil {
		t.Fatalf("ListForRunByCategory consolidated_pr_opened: %v", err)
	}
	if len(opened) != 1 {
		t.Errorf("consolidated_pr_opened entries = %d, want 1", len(opened))
	}
}

// TestDecomposition_E2E_FanInHappyPath exercises the ADR-041 / #1142 fan-in
// seam end-to-end (real Postgres): a decomposed parent with TWO disjoint
// succeeded slices fans in to ONE consolidated branch containing both
// slices' merges, then opens a single consolidated PR off that branch —
// the cross-boundary path (settle → orchestrator.IntegrateSlices →
// githubclient merges → consolidated PR → review dispatch) the per-layer
// units can't give.
func TestDecomposition_E2E_FanInHappyPath(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()

	runRepo := runpkg.NewPostgresRepository(pool)
	artifactRepo := artifact.NewPostgresRepository(pool)
	auditRepo := audit.NewPostgresRepository(pool)
	gh := &recordingGitHub{branchSHAs: map[string]string{"main": "basesha"}}

	o := &orchestrator.Orchestrator{
		Runs:       runRepo,
		Artifacts:  artifactRepo,
		Audit:      auditRepo,
		GitHub:     gh,
		DefaultRef: "main",
		Logger:     slog.Default(),
	}
	sw := &childcompletion.Sweeper{
		Runs:      runRepo,
		Audit:     auditRepo,
		Advance:   advancerAdapter{o: o},
		Integrate: advancerAdapter{o: o},
		Logger:    slog.Default(),
	}

	installID := int64(4242)
	fx := seedParentRun(t, ctx, runRepo, artifactRepo, decomposedPlanContent(t), &installID, runpkg.ExecutorHuman, nil)
	parentID := fx.runID

	if _, err := o.Advance(ctx, parentID); err != nil {
		t.Fatalf("Advance (fanout): %v", err)
	}
	children, err := runRepo.ListRuns(ctx, runpkg.ListRunsFilter{DecomposedFrom: &parentID, Limit: 100})
	if err != nil {
		t.Fatalf("ListRuns children: %v", err)
	}
	if len(children) != 2 {
		t.Fatalf("children = %d, want 2", len(children))
	}
	for _, child := range children {
		if _, err := runRepo.TransitionRun(ctx, child.ID, runpkg.StateRunning); err != nil {
			t.Fatalf("TransitionRun child running: %v", err)
		}
		if _, err := runRepo.TransitionRun(ctx, child.ID, runpkg.StateSucceeded); err != nil {
			t.Fatalf("TransitionRun child succeeded: %v", err)
		}
	}

	if err := sw.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	prefix := "fishhawk/run-" + parentID.String()[:8]
	consolidated := prefix + "-consolidated"
	// The consolidated branch was created from the base, and BOTH slice
	// branches merged onto it (done-means: one consolidated branch holding
	// both slices). The consolidated name is the non-nesting -consolidated
	// sibling, NOT a parent directory of the prefix/slice-<n> refs (#1243).
	if len(gh.createRefCalls) != 1 || gh.createRefCalls[0] != consolidated {
		t.Fatalf("CreateRef calls = %v, want [%s]", gh.createRefCalls, consolidated)
	}
	wantMerges := map[string]bool{prefix + "/slice-0": false, prefix + "/slice-1": false}
	if len(gh.mergeCalls) != 2 {
		t.Fatalf("MergeBranch calls = %v, want 2 slice merges", gh.mergeCalls)
	}
	for _, h := range gh.mergeCalls {
		if _, ok := wantMerges[h]; !ok {
			t.Errorf("unexpected merge head %q", h)
		}
		wantMerges[h] = true
	}
	for h, seen := range wantMerges {
		if !seen {
			t.Errorf("slice branch %q was not merged", h)
		}
	}

	// One consolidated PR off the now-integrated branch + the slices_integrated audit.
	if len(gh.createCalls) != 1 || gh.createCalls[0].Head != consolidated {
		t.Fatalf("CreatePullRequest calls = %v, want one with head %s", gh.createCalls, consolidated)
	}
	integrated, err := auditRepo.ListForRunByCategory(ctx, parentID, "slices_integrated")
	if err != nil {
		t.Fatalf("ListForRunByCategory slices_integrated: %v", err)
	}
	if len(integrated) != 1 {
		t.Errorf("slices_integrated entries = %d, want 1", len(integrated))
	}
}

// TestDecomposition_E2E_FanInConflict exercises the conflict branch: a
// slice whose merge 409s fails the parent implement stage recoverable
// (category-B) with the slice_integration_conflict audit present, and
// opens NO consolidated PR.
func TestDecomposition_E2E_FanInConflict(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()

	runRepo := runpkg.NewPostgresRepository(pool)
	artifactRepo := artifact.NewPostgresRepository(pool)
	auditRepo := audit.NewPostgresRepository(pool)
	gh := &recordingGitHub{branchSHAs: map[string]string{"main": "basesha"}}

	o := &orchestrator.Orchestrator{
		Runs:       runRepo,
		Artifacts:  artifactRepo,
		Audit:      auditRepo,
		GitHub:     gh,
		DefaultRef: "main",
		Logger:     slog.Default(),
	}
	sw := &childcompletion.Sweeper{
		Runs:      runRepo,
		Audit:     auditRepo,
		Advance:   advancerAdapter{o: o},
		Integrate: advancerAdapter{o: o},
		Logger:    slog.Default(),
	}

	installID := int64(4242)
	fx := seedParentRun(t, ctx, runRepo, artifactRepo, decomposedPlanContent(t), &installID, runpkg.ExecutorHuman, nil)
	parentID := fx.runID

	if _, err := o.Advance(ctx, parentID); err != nil {
		t.Fatalf("Advance (fanout): %v", err)
	}
	children, err := runRepo.ListRuns(ctx, runpkg.ListRunsFilter{DecomposedFrom: &parentID, Limit: 100})
	if err != nil {
		t.Fatalf("ListRuns children: %v", err)
	}
	for _, child := range children {
		if _, err := runRepo.TransitionRun(ctx, child.ID, runpkg.StateRunning); err != nil {
			t.Fatalf("TransitionRun child running: %v", err)
		}
		if _, err := runRepo.TransitionRun(ctx, child.ID, runpkg.StateSucceeded); err != nil {
			t.Fatalf("TransitionRun child succeeded: %v", err)
		}
	}

	// Slice-1's branch fails to merge (an overlapping change). The slice
	// branch nests under the run prefix; the consolidated branch is the
	// non-nesting -consolidated sibling (#1243).
	prefix := "fishhawk/run-" + parentID.String()[:8]
	gh.mergeErrByHead = map[string]error{prefix + "/slice-1": githubclient.ErrMergeConflict}

	if err := sw.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	// The parent implement stage failed recoverable (category-B).
	stages, err := runRepo.ListStagesForRun(ctx, parentID)
	if err != nil {
		t.Fatalf("ListStagesForRun: %v", err)
	}
	var implStage *runpkg.Stage
	for _, s := range stages {
		if s.Type == runpkg.StageTypeImplement {
			implStage = s
			break
		}
	}
	if implStage == nil {
		t.Fatal("implement stage not found")
	}
	if implStage.State != runpkg.StageStateFailed {
		t.Fatalf("implement stage = %q, want failed", implStage.State)
	}
	if implStage.FailureCategory == nil || *implStage.FailureCategory != runpkg.FailureB {
		t.Errorf("failure category = %v, want B (recoverable)", implStage.FailureCategory)
	}

	// No consolidated PR opened on a conflict.
	if len(gh.createCalls) != 0 {
		t.Errorf("CreatePullRequest calls = %d, want 0 on conflict", len(gh.createCalls))
	}

	// slice_integration_conflict audit present.
	conflicts, err := auditRepo.ListForRunByCategory(ctx, parentID, "slice_integration_conflict")
	if err != nil {
		t.Fatalf("ListForRunByCategory slice_integration_conflict: %v", err)
	}
	if len(conflicts) != 1 {
		t.Errorf("slice_integration_conflict entries = %d, want 1", len(conflicts))
	}
}

// rejectingReviewer is a server.PlanReviewer that returns one reject
// verdict carrying a single high-severity concern — the consolidated
// diff's defect the parent review must surface (#1060).
type rejectingReviewer struct{}

func (rejectingReviewer) Review(_ context.Context, _ string) (*planreview.ReviewVerdict, string, error) {
	return &planreview.ReviewVerdict{
		Verdict: planreview.VerdictReject,
		Concerns: []planreview.Concern{
			{Severity: planreview.SeverityHigh, Category: "correctness", Note: "consolidated diff carries a nil deref from child Part A"},
		},
	}, "claude-opus-4-8", nil
}

// staticTokens is a githubapp.TokenProvider returning a fixed token, for
// the compare-endpoint stub.
type staticTokens struct{}

func (staticTokens) Token(_ context.Context, _ int64) (string, error) { return "ghs_e2e", nil }

// specImplementGatingReviewers configures the implement stage with one
// gating agent reviewer so the parent's consolidated review resolves a
// reviewer and runs.
var specImplementGatingReviewersE2E = []byte(`version: "0.3"
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
        reviewers:
          agent: 1
          human: 0
`)

// TestDecomposition_E2E_ConsolidatedReviewGatesParentMerge is the
// cross-boundary gating verification for #1060: a decomposed fan-out whose
// consolidated diff carries a defect drives the orchestrator → server
// consolidated-review hook → githubclient.ComparePatch → review → concern
// seam against a real Postgres database, asserting the implement_reviewed
// concern attaches with StageID == the PARENT implement stage that
// fishhawk_fixup_stage targets (the load-bearing seam) and that a fix-up on
// that stage resolves the concern over the shared branch.
func TestDecomposition_E2E_ConsolidatedReviewGatesParentMerge(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()

	runRepo := runpkg.NewPostgresRepository(pool)
	artifactRepo := artifact.NewPostgresRepository(pool)
	auditRepo := audit.NewPostgresRepository(pool)
	concernRepo := concern.NewPostgresRepository(pool)
	apiRepo := apitoken.NewPostgresRepository(pool)

	o := &orchestrator.Orchestrator{
		Runs:       runRepo,
		Artifacts:  artifactRepo,
		Audit:      auditRepo,
		DefaultRef: "main",
		Logger:     slog.Default(),
	}

	// Compare-endpoint stub: the consolidated base...head diff GitHub
	// returns for the parent. ComparePatch parses it into the review's
	// policy.Diff.
	compareBody := `{
		"total_commits": 2,
		"commits": [{"sha":"c1"},{"sha":"headsha"}],
		"files": [{"filename":"x.go","status":"modified","changes":4,"patch":"@@ -1 +1 @@\n-ok\n+nil deref"}]
	}`
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, compareBody)
	})
	ghSrv := httptest.NewServer(mux)
	t.Cleanup(ghSrv.Close)
	ghClient := &githubclient.Client{
		BaseURL: ghSrv.URL,
		Tokens:  staticTokens{},
		HTTP:    &http.Client{Timeout: 5 * time.Second},
		AppJWT:  func() (string, error) { return "ghs_jwt", nil },
	}

	srv := server.New(server.Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      runRepo,
		ArtifactRepo: artifactRepo,
		AuditRepo:    auditRepo,
		ConcernRepo:  concernRepo,
		APITokenRepo: apiRepo,
		PlanReviewer: rejectingReviewer{},
		GitHub:       ghClient,
		Orchestrator: o,
	})
	// Close the orchestrator→server back-edge for the consolidated
	// decomposition review (#1060): Advance computes the decomposed-parent +
	// consolidated-PR condition and fires this hook, but the review
	// machinery lives server-side (*server.Server).
	o.ConsolidatedReview = srv
	httpSrv := httptest.NewServer(srv.Handler())
	t.Cleanup(httpSrv.Close)

	sw := &childcompletion.Sweeper{
		Runs:    runRepo,
		Audit:   auditRepo,
		Advance: advancerAdapter{o: o},
		Logger:  slog.Default(),
	}

	planBytes := decomposedPlanContent(t)
	installID := int64(55)
	fx := seedParentRun(t, ctx, runRepo, artifactRepo, planBytes, &installID, runpkg.ExecutorHuman, specImplementGatingReviewersE2E)
	parentID := fx.runID

	// (a) Fan out into children.
	if _, err := o.Advance(ctx, parentID); err != nil {
		t.Fatalf("Advance fanout: %v", err)
	}
	children, err := runRepo.ListRuns(ctx, runpkg.ListRunsFilter{DecomposedFrom: &parentID, Limit: 100})
	if err != nil {
		t.Fatalf("ListRuns children: %v", err)
	}
	if len(children) != 2 {
		t.Fatalf("children = %d, want 2", len(children))
	}

	// Pre-stamp the consolidated PR URL so the review-gate hook fires (the
	// orchestrator has no GitHub wired here; the consolidated PR open path
	// is exercised separately in #714's tests).
	if _, err := runRepo.SetRunPullRequestURL(ctx, parentID, "https://github.com/kuhlman-labs/fishhawk/pull/777"); err != nil {
		t.Fatalf("SetRunPullRequestURL: %v", err)
	}

	// Resolve the parent implement stage id — the concern-attach target.
	stages, err := runRepo.ListStagesForRun(ctx, parentID)
	if err != nil {
		t.Fatalf("ListStagesForRun: %v", err)
	}
	var implStageID uuid.UUID
	for _, s := range stages {
		if s.Type == runpkg.StageTypeImplement {
			implStageID = s.ID
		}
	}
	if implStageID == uuid.Nil {
		t.Fatal("parent implement stage not found")
	}

	// (b) Before the consolidated round dispatches, the implement-review
	// round is configured-but-undispatched, so the drive gate must NOT
	// advance to awaiting_merge (#1060 slice 1). Assert no
	// implement_review_started yet.
	if started, _ := auditRepo.ListForRunByCategory(ctx, parentID, "implement_review_started"); len(started) != 0 {
		t.Fatalf("implement_review_started before children settle = %d, want 0", len(started))
	}

	// (c) Drive children to succeeded, then the sweeper resolves the parent
	// implement stage and Advance dispatches the review gate — firing the
	// consolidated review hook.
	for _, child := range children {
		if _, err := runRepo.TransitionRun(ctx, child.ID, runpkg.StateRunning); err != nil {
			t.Fatalf("child running: %v", err)
		}
		if _, err := runRepo.TransitionRun(ctx, child.ID, runpkg.StateSucceeded); err != nil {
			t.Fatalf("child succeeded: %v", err)
		}
	}
	if err := sw.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	// (d) Poll for the consolidated review's concern to land (the dispatch
	// runs on a detached goroutine).
	var rows []*concern.Concern
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		rows, err = concernRepo.ListByRun(ctx, parentID)
		if err != nil {
			t.Fatalf("ListByRun: %v", err)
		}
		if len(rows) >= 1 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if len(rows) != 1 {
		t.Fatalf("consolidated review concerns = %d, want 1", len(rows))
	}

	// (e) LOAD-BEARING ASSERTION (#1060 amendment 2): the concern attaches
	// with StageID == the parent implement stage that fixup_stage targets.
	if rows[0].StageID != implStageID {
		t.Fatalf("concern StageID = %s, want parent implement stage %s", rows[0].StageID, implStageID)
	}
	if rows[0].StageKind != concern.StageKindImplement {
		t.Errorf("concern StageKind = %q, want implement", rows[0].StageKind)
	}
	concernID := rows[0].ID

	// The round dispatched against the parent (drive can now distinguish
	// non-vacuous evidence).
	if started, _ := auditRepo.ListForRunByCategory(ctx, parentID, "implement_review_started"); len(started) != 1 {
		t.Errorf("implement_review_started after dispatch = %d, want 1", len(started))
	}

	// (f) fishhawk_fixup_stage on the PARENT implement stage resolves that
	// concern over the shared branch. Drive the real HTTP handler with an
	// operator token carrying write:stages.
	tok, err := apiRepo.Issue(ctx, "operator@e2e", []string{"read:runs", "read:audit", "write:stages"})
	if err != nil {
		t.Fatalf("Issue token: %v", err)
	}
	// Capture the child count before the fix-up so step (g) can assert the
	// fix-up's downstream Advance mints NO new children (#1063).
	childrenBefore, err := runRepo.ListRuns(ctx, runpkg.ListRunsFilter{DecomposedFrom: &parentID, Limit: 100})
	if err != nil {
		t.Fatalf("ListRuns children before fixup: %v", err)
	}
	fixupBody, _ := json.Marshal(map[string]any{
		"concern_ids": []string{concernID.String()},
		"reason":      "fix the nil deref on the shared branch",
	})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("%s/v0/stages/%s/fixup", httpSrv.URL, implStageID), bytes.NewReader(fixupBody))
	req.Header.Set("Authorization", "Bearer "+tok.PlainText)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("fixup request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("fixup status = %d, want 200: %s", resp.StatusCode, b)
	}

	// (g) fixup_stage genuinely routed the consolidated concern back: the
	// concern attached to the parent implement stage transitioned to
	// addressed_pending. This is the #1060 seam — the consolidated review's
	// concern is addressable by a fix-up on the parent implement stage.
	after, err := concernRepo.ListByRun(ctx, parentID)
	if err != nil {
		t.Fatalf("ListByRun after fixup: %v", err)
	}
	if len(after) != 1 || after[0].State != concern.StateAddressedPending {
		t.Errorf("concern state after fixup = %v, want addressed_pending", after[0].State)
	}

	// (h) #1063 REALIZED CONTRACT: the fix-up handler's downstream Advance
	// re-opens the parent implement stage to pending, but the orchestrator's
	// existing-children idempotency guard now skips fanoutIfDecomposed (the
	// parent already has children) and re-invokes the parent's implement stage
	// against the existing shared branch instead of re-minting a fresh
	// fan-out. Assert NO new children were minted.
	childrenAfter, err := runRepo.ListRuns(ctx, runpkg.ListRunsFilter{DecomposedFrom: &parentID, Limit: 100})
	if err != nil {
		t.Fatalf("ListRuns children after fixup: %v", err)
	}
	if len(childrenAfter) != len(childrenBefore) {
		t.Errorf("children after fixup = %d, want %d (existing-children guard must mint zero new children)",
			len(childrenAfter), len(childrenBefore))
	}

	// (i) Where observable: the re-dispatched parent implement stage routes to
	// the shared consolidated branch. The branch derivation itself is a
	// server-side prompt-handler concern (covered byte-exactly by
	// TestGetStagePrompt_Implement_FixupDecomposedParent_SharedBranch); here
	// the observable proxy is that the guard fell through to dispatch — the
	// parent implement stage left awaiting_children / pending and re-dispatched
	// rather than re-parking awaiting a fresh fan-out.
	reStages, err := runRepo.ListStagesForRun(ctx, parentID)
	if err != nil {
		t.Fatalf("ListStagesForRun after fixup: %v", err)
	}
	for _, s := range reStages {
		if s.ID == implStageID && s.State == runpkg.StageStateAwaitingChildren {
			t.Errorf("parent implement stage re-parked awaiting_children after fixup; want re-dispatched against the shared branch")
		}
	}
}

// findImplementStage returns the run's single implement stage, failing
// the test when it can't be resolved.
func findImplementStage(t *testing.T, ctx context.Context, runRepo runpkg.Repository, runID uuid.UUID) *runpkg.Stage {
	t.Helper()
	stages, err := runRepo.ListStagesForRun(ctx, runID)
	if err != nil {
		t.Fatalf("ListStagesForRun %s: %v", runID, err)
	}
	for _, s := range stages {
		if s.Type == runpkg.StageTypeImplement {
			return s
		}
	}
	t.Fatalf("implement stage not found on run %s", runID)
	return nil
}

// driveStageThrough transitions a stage through the given non-terminal
// states in order (completion is nil — none of these is `failed`).
func driveStageThrough(t *testing.T, ctx context.Context, runRepo runpkg.Repository, stageID uuid.UUID, states ...runpkg.StageState) {
	t.Helper()
	for _, to := range states {
		if _, err := runRepo.TransitionStage(ctx, stageID, to, nil); err != nil {
			t.Fatalf("TransitionStage %s to %s: %v", stageID, to, err)
		}
	}
}

// TestDecomposition_E2E_CategoryBChildRecoverInPlace is the cross-boundary
// verification for #1081: a decomposed fan-out where one child fails its
// implement stage category-B must PARK the parent in awaiting_children (the
// recoverable-in-decomposition gate, NOT failed-C), and the operator's
// in-place recover path (POST /v0/runs/{child}/recover with add_scope_files)
// must re-open that SAME child on the shared branch so its next implement run
// succeeds and the sweeper then resolves the parent's awaiting_children stage
// to succeeded and advances it toward consolidation/review. This drives the
// full loop across the run-layer parking predicate (slice 1), the server
// recover handler + scope-amendment persistence (slice 2), and the
// sweeper/orchestrator consolidation seam — coverage no per-layer unit gives.
func TestDecomposition_E2E_CategoryBChildRecoverInPlace(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()

	runRepo := runpkg.NewPostgresRepository(pool)
	artifactRepo := artifact.NewPostgresRepository(pool)
	auditRepo := audit.NewPostgresRepository(pool)
	scopeRepo := scopeamendment.NewPostgresRepository(pool)
	apiRepo := apitoken.NewPostgresRepository(pool)

	o := &orchestrator.Orchestrator{
		Runs:       runRepo,
		Artifacts:  artifactRepo,
		Audit:      auditRepo,
		DefaultRef: "main",
		Logger:     slog.Default(),
	}
	srv := server.New(server.Config{
		Addr:               "127.0.0.1:0",
		RunRepo:            runRepo,
		ArtifactRepo:       artifactRepo,
		AuditRepo:          auditRepo,
		ScopeAmendmentRepo: scopeRepo,
		APITokenRepo:       apiRepo,
		Orchestrator:       o,
		// A real (never-called) client satisfies the prompt-render handler's
		// issueGetter guard; the recovery run carries no InstallationID, so no
		// GitHub request is made when its implement prompt is rendered (#1229).
		GitHub: githubclient.New(nil),
	})
	httpSrv := httptest.NewServer(srv.Handler())
	t.Cleanup(httpSrv.Close)

	sw := &childcompletion.Sweeper{
		Runs:    runRepo,
		Audit:   auditRepo,
		Advance: advancerAdapter{o: o},
		Logger:  slog.Default(),
	}

	planBytes := decomposedPlanContent(t)
	fx := seedParentRun(t, ctx, runRepo, artifactRepo, planBytes, nil, runpkg.ExecutorAgent, nil)
	parentID := fx.runID

	// (a) Fan out into two children.
	if _, err := o.Advance(ctx, parentID); err != nil {
		t.Fatalf("Advance fanout: %v", err)
	}
	children, err := runRepo.ListRuns(ctx, runpkg.ListRunsFilter{DecomposedFrom: &parentID, Limit: 100})
	if err != nil {
		t.Fatalf("ListRuns children: %v", err)
	}
	if len(children) != 2 {
		t.Fatalf("children = %d, want 2", len(children))
	}
	failing, succeeding := children[0], children[1]

	// (b) Drive the failing child's implement stage to a category-B failure
	// and its run to failed; drive the other child to succeeded.
	failImpl := findImplementStage(t, ctx, runRepo, failing.ID)
	driveStageThrough(t, ctx, runRepo, failImpl.ID, runpkg.StageStateDispatched, runpkg.StageStateRunning)
	cat := runpkg.FailureB
	failReason := "scope violation: edited an out-of-scope file"
	if _, err := runRepo.TransitionStage(ctx, failImpl.ID, runpkg.StageStateFailed,
		&runpkg.StageCompletion{FailureCategory: &cat, FailureReason: &failReason}); err != nil {
		t.Fatalf("fail child implement stage category-B: %v", err)
	}
	if _, err := runRepo.TransitionRun(ctx, failing.ID, runpkg.StateRunning); err != nil {
		t.Fatalf("TransitionRun failing child running: %v", err)
	}
	if _, err := runRepo.TransitionRun(ctx, failing.ID, runpkg.StateFailed); err != nil {
		t.Fatalf("TransitionRun failing child failed: %v", err)
	}
	if _, err := runRepo.TransitionRun(ctx, succeeding.ID, runpkg.StateRunning); err != nil {
		t.Fatalf("TransitionRun succeeding child running: %v", err)
	}
	if _, err := runRepo.TransitionRun(ctx, succeeding.ID, runpkg.StateSucceeded); err != nil {
		t.Fatalf("TransitionRun succeeding child succeeded: %v", err)
	}

	// (c) Sweeper tick: the only failed child is recoverable in decomposition
	// (category B), so the parent PARKS — implement stays awaiting_children,
	// run stays running, NOT resolved to failed-C.
	if err := sw.Tick(ctx); err != nil {
		t.Fatalf("Tick (park): %v", err)
	}
	parentImpl := findImplementStage(t, ctx, runRepo, parentID)
	if parentImpl.State != runpkg.StageStateAwaitingChildren {
		t.Fatalf("parent implement stage after category-B child park = %q, want awaiting_children (NOT failed-C)", parentImpl.State)
	}
	parent, err := runRepo.GetRun(ctx, parentID)
	if err != nil {
		t.Fatalf("GetRun parent: %v", err)
	}
	if parent.State != runpkg.StateRunning {
		t.Errorf("parent run state after park = %q, want running", parent.State)
	}

	// (d) Operator recovers the failed child IN PLACE via the real recover
	// endpoint, folding an add_scope_files amendment. Pointed at the CHILD's
	// own id (not the parent), recovery re-drives the same run — write:runs.
	tok, err := apiRepo.Issue(ctx, "operator@e2e", []string{"read:runs", "write:runs"})
	if err != nil {
		t.Fatalf("Issue token: %v", err)
	}
	const recoveredFile = "backend/internal/server/recover.go"
	// x.go is the plan's declared scope.files path (decomposedPlanContent);
	// exempt it so the runner gate would subtract it (#1229).
	const exemptedFile = "x.go"
	const exemptReason = "declared but unchanged on this recovery slice"
	const resumeReason = "fold the dropped file and re-drive the child in place"
	recoverBody, _ := json.Marshal(map[string]any{
		"add_scope_files":    []map[string]string{{"path": recoveredFile, "operation": "modify"}},
		"exempt_scope_files": []map[string]string{{"path": exemptedFile, "reason": exemptReason}},
		"reason":             resumeReason,
	})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("%s/v0/runs/%s/recover", httpSrv.URL, failing.ID), bytes.NewReader(recoverBody))
	req.Header.Set("Authorization", "Bearer "+tok.PlainText)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("recover request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("recover status = %d, want 201: %s", resp.StatusCode, b)
	}
	var recovered struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&recovered); err != nil {
		t.Fatalf("decode recover response: %v", err)
	}
	// Same run id — in-place re-drive, NOT a freshly minted run.
	if recovered.ID != failing.ID.String() {
		t.Fatalf("recover returned id %s, want the SAME child %s (in-place re-drive, no new run)", recovered.ID, failing.ID)
	}

	// No new DecomposedFrom child was minted (a second row would double-count
	// in resolveParent's consolidation counters — the reason for in-place).
	childrenAfter, err := runRepo.ListRuns(ctx, runpkg.ListRunsFilter{DecomposedFrom: &parentID, Limit: 100})
	if err != nil {
		t.Fatalf("ListRuns children after recover: %v", err)
	}
	if len(childrenAfter) != 2 {
		t.Errorf("children after recover = %d, want 2 (in-place re-drive must mint zero new children)", len(childrenAfter))
	}

	// The child run re-opened failed → running.
	reopened, err := runRepo.GetRun(ctx, failing.ID)
	if err != nil {
		t.Fatalf("GetRun re-opened child: %v", err)
	}
	if reopened.State != runpkg.StateRunning {
		t.Errorf("re-opened child run state = %q, want running", reopened.State)
	}

	// The implement stage re-opened in place (failed → non-terminal) with its
	// id PRESERVED — so the scope amendment keyed to it folds into the prompt.
	reImpl := findImplementStage(t, ctx, runRepo, failing.ID)
	if reImpl.ID != failImpl.ID {
		t.Errorf("re-opened implement stage id = %s, want preserved %s (in-place re-drive)", reImpl.ID, failImpl.ID)
	}
	if reImpl.State == runpkg.StageStateFailed {
		t.Errorf("re-opened implement stage still failed; want re-opened (pending/dispatched)")
	}

	// The operator's add_scope_files landed as an APPROVED amendment on the
	// EXISTING (preserved-id) implement stage.
	amends, err := scopeRepo.ListByRun(ctx, failing.ID)
	if err != nil {
		t.Fatalf("ListByRun amendments: %v", err)
	}
	if len(amends) != 1 {
		t.Fatalf("amendments on re-driven child = %d, want 1", len(amends))
	}
	if amends[0].Status != scopeamendment.StatusApproved {
		t.Errorf("amendment status = %q, want approved", amends[0].Status)
	}
	if amends[0].StageID != failImpl.ID {
		t.Errorf("amendment StageID = %s, want the existing implement stage %s", amends[0].StageID, failImpl.ID)
	}

	// A plan_reused_from provenance entry with source=decomposition_child_recovery.
	reused, err := auditRepo.ListForRunByCategory(ctx, failing.ID, "plan_reused_from")
	if err != nil {
		t.Fatalf("ListForRunByCategory plan_reused_from: %v", err)
	}
	if len(reused) != 1 {
		t.Fatalf("plan_reused_from entries = %d, want 1", len(reused))
	}
	var reusedPayload struct {
		Source        string `json:"source"`
		ExemptedPaths []struct {
			Path   string `json:"path"`
			Reason string `json:"reason"`
		} `json:"exempted_paths"`
	}
	if err := json.Unmarshal(reused[0].Payload, &reusedPayload); err != nil {
		t.Fatalf("decode plan_reused_from payload: %v", err)
	}
	if reusedPayload.Source != "decomposition_child_recovery" {
		t.Errorf("plan_reused_from source = %q, want decomposition_child_recovery", reusedPayload.Source)
	}
	// (#1229) The operator's exempt_scope_files persisted on the provenance.
	if len(reusedPayload.ExemptedPaths) != 1 ||
		reusedPayload.ExemptedPaths[0].Path != exemptedFile ||
		reusedPayload.ExemptedPaths[0].Reason != exemptReason {
		t.Errorf("plan_reused_from exempted_paths = %+v, want the one operator exemption", reusedPayload.ExemptedPaths)
	}

	// (#1229) Cross-boundary delivery: the re-driven child's implement
	// prompt-response carries scope_exemptions AND the resume reason renders
	// into the binding conditions (Part D) — the seam no per-layer unit covers.
	promptReq, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/v0/stages/%s/prompt-render", httpSrv.URL, reImpl.ID), nil)
	promptResp, err := http.DefaultClient.Do(promptReq)
	if err != nil {
		t.Fatalf("prompt-render request: %v", err)
	}
	defer func() { _ = promptResp.Body.Close() }()
	if promptResp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(promptResp.Body)
		t.Fatalf("prompt-render status = %d, want 200: %s", promptResp.StatusCode, b)
	}
	var prompt struct {
		Prompt          string `json:"prompt"`
		ScopeExemptions []struct {
			Path   string `json:"path"`
			Reason string `json:"reason"`
		} `json:"scope_exemptions"`
	}
	if err := json.NewDecoder(promptResp.Body).Decode(&prompt); err != nil {
		t.Fatalf("decode prompt-render: %v", err)
	}
	if len(prompt.ScopeExemptions) != 1 || prompt.ScopeExemptions[0].Path != exemptedFile ||
		prompt.ScopeExemptions[0].Reason != exemptReason {
		t.Errorf("prompt scope_exemptions = %+v, want the one operator exemption (cross-boundary delivery broke)", prompt.ScopeExemptions)
	}
	if !strings.Contains(prompt.Prompt, resumeReason) {
		t.Errorf("implement prompt missing the Part D resume reason %q (reason→binding-conditions broke)", resumeReason)
	}

	// (e) The re-driven child succeeds; the sweeper then resolves the parent's
	// awaiting_children stage to succeeded and advances it toward the review
	// gate (consolidation) — the parked parent fan-out consolidated.
	if _, err := runRepo.TransitionRun(ctx, failing.ID, runpkg.StateSucceeded); err != nil {
		t.Fatalf("TransitionRun re-driven child succeeded: %v", err)
	}
	if err := sw.Tick(ctx); err != nil {
		t.Fatalf("Tick (resolve): %v", err)
	}
	parentImpl = findImplementStage(t, ctx, runRepo, parentID)
	if parentImpl.State != runpkg.StageStateSucceeded {
		t.Fatalf("parent implement stage after re-drive success = %q, want succeeded", parentImpl.State)
	}

	// The sweeper's internal Advance dispatched the parent review stage —
	// the fan-out advanced past implement toward consolidation/review.
	stages, err := runRepo.ListStagesForRun(ctx, parentID)
	if err != nil {
		t.Fatalf("ListStagesForRun parent after resolve: %v", err)
	}
	var reviewStage *runpkg.Stage
	for _, s := range stages {
		if s.Type == runpkg.StageTypeReview {
			reviewStage = s
			break
		}
	}
	if reviewStage == nil {
		t.Fatal("parent review stage not found")
	}
	if reviewStage.State == runpkg.StageStatePending {
		t.Errorf("parent review stage = pending, want advanced (dispatched) — parent did not progress toward consolidation after re-drive")
	}
}
