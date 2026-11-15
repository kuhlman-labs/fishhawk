package mcpserver

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/apitoken"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/orchestrator"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	runpkg "github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/server"
)

// gitAuthSeamGoldenPath is the shared runner_failed golden both ends of the
// #5212 seam read: the runner's emission test
// (runner/cmd/fishhawk-runner TestRun_Fixup_BaseCheckoutAuthRejected_EmitsGoldenRunnerFailed)
// asserts its emitted line is JSON-equal to it, and this test reaps the same
// bytes. The runner and backend are separate Go modules, so this file is the
// only link between emission and parse.
func gitAuthSeamGoldenPath(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	// backend/internal/mcpserver → repository root.
	return filepath.Join(wd, "..", "..", "..", "testdata", "wire", "runner_failed_fixup_git_auth.json")
}

// TestFixupGitAuthSeam_RunnerFailedReapedToRenderedRemedy is the BACKEND end of
// the #5212 git-auth seam (J5), over real Postgres, the real server handlers and
// the real api client. A fix-up pass is opened through the REAL POST
// /v0/stages/{id}/fixup (operator_concern only), the shared golden is written as
// the detached runner's log, and the REAL reapDetachedRunner (with
// parseDetachedRunnerFailure) reaps an exited shell through a reporter shaped
// exactly like dispatch_stage.go's production closure, into the REAL
// handleReapStageFailure → maybeRecoverReapedFixupFailure →
// writeFixupRecoveredAudit chain. The golden's reason field carries ONLY the
// token, so the remedy renders only if the token survives every hop.
func TestFixupGitAuthSeam_RunnerFailedReapedToRenderedRemedy(t *testing.T) {
	ctx := context.Background()
	golden, err := os.ReadFile(gitAuthSeamGoldenPath(t)) //nolint:gosec // fixed workspace-relative path
	if err != nil {
		t.Fatalf("reading the shared runner_failed golden: %v", err)
	}

	pool := pgtest.NewPool(t)
	runRepo := runpkg.NewPostgresRepository(pool)
	auditRepo := audit.NewPostgresRepository(pool)
	concernRepo := concern.NewPostgresRepository(pool)

	const bearer = "fhk_fixup_git_auth_seam"
	o := &orchestrator.Orchestrator{Runs: runRepo, Audit: auditRepo}
	srv := server.New(server.Config{
		RunRepo:      runRepo,
		AuditRepo:    auditRepo,
		ConcernRepo:  concernRepo,
		Orchestrator: o,
		APITokenRepo: &stubMCPAPITokens{tok: &apitoken.Token{
			ID: uuid.New(), Subject: "github:op", Scopes: []string{"read:runs", "write:runs", "write:stages"}, PlainText: bearer,
		}},
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	r := &runResolver{api: newAPIClient(config{backendURL: ts.URL, apiToken: bearer}), getenv: func(string) string { return "" }, reviewPollInterval: time.Millisecond}

	inst := int64(42)
	rn, err := runRepo.CreateRun(ctx, runpkg.CreateRunParams{
		Repo: "x/y", WorkflowID: "feature_change", WorkflowSHA: "abc",
		TriggerSource: runpkg.TriggerCLI, InstallationID: &inst,
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	if _, err := runRepo.TransitionRun(ctx, rn.ID, runpkg.StateRunning); err != nil {
		t.Fatalf("run → running: %v", err)
	}
	impl, err := runRepo.CreateStage(ctx, runpkg.CreateStageParams{
		RunID: rn.ID, Sequence: 1, Type: runpkg.StageTypeImplement,
		ExecutorKind: runpkg.ExecutorAgent, ExecutorRef: "claude-code",
	})
	if err != nil {
		t.Fatalf("create implement stage: %v", err)
	}
	review, err := runRepo.CreateStage(ctx, runpkg.CreateStageParams{
		RunID: rn.ID, Sequence: 2, Type: runpkg.StageTypeReview,
		ExecutorKind: runpkg.ExecutorAgent, ExecutorRef: "claude-code",
	})
	if err != nil {
		t.Fatalf("create review stage: %v", err)
	}
	walk := func(id uuid.UUID, states ...runpkg.StageState) {
		t.Helper()
		for _, to := range states {
			if _, err := runRepo.TransitionStage(ctx, id, to, nil); err != nil {
				t.Fatalf("transition stage %s → %s: %v", id, to, err)
			}
		}
	}
	walk(impl.ID, runpkg.StageStateDispatched, runpkg.StageStateRunning, runpkg.StageStateSucceeded)
	walk(review.ID, runpkg.StageStateDispatched, runpkg.StageStateRunning, runpkg.StageStateAwaitingApproval)

	// Open the fix-up pass through the REAL endpoint.
	if _, err := r.api.FixupStage(ctx, impl.ID, nil, nil, "", nil, false, "", "seam: re-check the base checkout", ""); err != nil {
		t.Fatalf("POST /fixup: %v", err)
	}
	// The re-opened pass must be in a reapable state, as a real dispatch leaves
	// it. Walk it to dispatched when the orchestrator did not.
	cur, err := runRepo.GetStage(ctx, impl.ID)
	if err != nil {
		t.Fatalf("GetStage: %v", err)
	}
	switch cur.State {
	case runpkg.StageStatePending, runpkg.StageStateAwaitingHostDispatch:
		walk(impl.ID, runpkg.StageStateDispatched)
	case runpkg.StageStateDispatched, runpkg.StageStateRunning:
	default:
		t.Fatalf("implement stage after fix-up = %s, want a re-opened pass", cur.State)
	}

	// The detached runner's log is the golden's exact bytes.
	logPath := filepath.Join(t.TempDir(), "runner.log")
	if err := os.WriteFile(logPath, golden, 0o600); err != nil {
		t.Fatalf("write runner log: %v", err)
	}
	// Shaped exactly like dispatch_stage.go's production reporter closure.
	report := func(ctx context.Context, category, reason, detail string, exitCode int) error {
		_, rerr := r.api.ReportStageFailure(ctx, rn.ID, impl.ID, category, reason, detail, exitCode)
		return rerr
	}
	reapDetachedRunner(startedShellCmd(t, "exit 1"), logPath, rn.ID.String(), impl.ID.String(), report, nil)

	// Hop: the persisted stage_fixup_recovered row.
	entries, err := auditRepo.ListForRunByCategory(ctx, rn.ID, server.CategoryStageFixupRecovered)
	if err != nil {
		t.Fatalf("list stage_fixup_recovered: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("stage_fixup_recovered entries = %d, want 1 (the reap did not recover the fix-up pass)", len(entries))
	}
	var row struct {
		SourceFailureReason   string `json:"source_failure_reason"`
		SourceFailureCategory string `json:"source_failure_category"`
		DeliveredNothing      *bool  `json:"delivered_nothing"`
	}
	if err := json.Unmarshal(entries[0].Payload, &row); err != nil {
		t.Fatalf("decode stage_fixup_recovered: %v", err)
	}
	if row.SourceFailureReason != "fixup_base_git_auth" {
		t.Errorf("source_failure_reason = %q, want exactly fixup_base_git_auth (the token must survive runner JSONL → reaper → reap handler → recovery audit)", row.SourceFailureReason)
	}
	if row.SourceFailureCategory != "C" {
		t.Errorf("source_failure_category = %q, want C (the local detached reaper reports C)", row.SourceFailureCategory)
	}
	if row.DeliveredNothing == nil || !*row.DeliveredNothing {
		t.Errorf("delivered_nothing = %v, want true (no fixup_pushed landed)", row.DeliveredNothing)
	}

	// Hop: the marker fishhawk_get_run_status and fishhawk_await_stage render.
	rec := r.fixupRecoveryFor(ctx, rn.ID, impl.ID)
	if rec == nil {
		t.Fatal("fixupRecoveryFor = nil, want the recovery marker")
	}
	end := strings.Index(rec.Message, fixupRecoveryUntrustedClose)
	if end < 0 {
		t.Fatalf("marker message carries no untrusted END delimiter: %q", rec.Message)
	}
	remedy := strings.Index(rec.Message, "installation_token_received / base_auth_token_unavailable")
	if remedy < 0 {
		t.Errorf("marker message lacks the git-auth credential-path remedy: %q", rec.Message)
	} else if remedy < end {
		t.Errorf("git-auth remedy renders inside the untrusted envelope (at %d, END at %d): %q", remedy, end, rec.Message)
	}
	if !strings.Contains(rec.Message, "loses no routed work") {
		t.Errorf("marker message lacks the re-route-is-safe sentence for a refunded category-C pass: %q", rec.Message)
	}
}
