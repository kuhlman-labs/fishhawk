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
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
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

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/childcompletion"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/orchestrator"
	"github.com/kuhlman-labs/fishhawk/backend/internal/postgres"
	runpkg "github.com/kuhlman-labs/fishhawk/backend/internal/run"
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
func seedParentRun(t *testing.T, ctx context.Context, runRepo runpkg.Repository, artifactRepo artifact.Repository, planBytes []byte, installationID *int64, reviewKind runpkg.ExecutorKind) parentRunFixture {
	t.Helper()

	r, err := runRepo.CreateRun(ctx, runpkg.CreateRunParams{
		Repo:           "kuhlman-labs/fishhawk",
		WorkflowID:     "feature_change",
		WorkflowSHA:    "deadbeef",
		TriggerSource:  runpkg.TriggerCLI,
		InstallationID: installationID,
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
	fx := seedParentRun(t, ctx, runRepo, artifactRepo, planBytes, nil, runpkg.ExecutorAgent)
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

	// (m) Sweeper's internal Advance already dispatched review.
	// A second explicit Advance finds no pending stages and completes
	// the run (OutcomeRunCompleted, state = succeeded).
	outcome, err = o.Advance(ctx, parentID)
	if err != nil {
		t.Fatalf("Advance second: %v", err)
	}
	if outcome != orchestrator.OutcomeRunCompleted {
		t.Errorf("Advance second outcome = %q, want %q", outcome, orchestrator.OutcomeRunCompleted)
	}
	parent, err := runRepo.GetRun(ctx, parentID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if parent.State != runpkg.StateSucceeded {
		t.Errorf("parent run state = %q, want succeeded", parent.State)
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
	fx := seedParentRun(t, ctx, runRepo, artifactRepo, planBytes, nil, runpkg.ExecutorAgent)
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
	fx := seedParentRun(t, ctx, runRepo, artifactRepo, planBytes, &installID, runpkg.ExecutorHuman)
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
	wantHead := "fishhawk/run-" + parentID.String()[:8]
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
