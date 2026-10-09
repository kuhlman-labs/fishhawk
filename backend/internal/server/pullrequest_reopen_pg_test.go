package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/orchestrator"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/webhook"
)

// pullrequest_reopen_pg_test.go is the cross-layer done-means proof of the
// PR-reopen revive (#4082), in the fixture style of
// acceptance_retirement_drop_pg_test.go: pgtest Postgres, the production run
// and chained audit repositories, a real orchestrator, and server.New over
// them, driven through the SIGNED POST /webhooks/github route. Every assertion
// re-reads committed state from Postgres.

type reopenPGFixture struct {
	s        *Server
	runs     run.Repository
	audit    audit.Repository
	runID    uuid.UUID
	reviewID uuid.UUID
	prURL    string
	delivery int
}

// newReopenPGFixture seeds a run at its review gate: running, plan and
// implement succeeded, review parked at awaiting_approval, PR URL set.
func newReopenPGFixture(t *testing.T) *reopenPGFixture {
	t.Helper()
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	runRepo := run.NewPostgresRepository(pool)
	auditRepo := audit.NewPostgresRepository(pool)
	s := New(Config{
		Addr: "127.0.0.1:0", RunRepo: runRepo, AuditRepo: auditRepo,
		Orchestrator:        &orchestrator.Orchestrator{Runs: runRepo, Audit: auditRepo},
		GitHubWebhookSecret: []byte(testSecret), WebhookDeliveries: webhook.NewMemoryStore(0),
	})

	r, err := runRepo.CreateRun(ctx, run.CreateRunParams{
		Repo: "x/y", WorkflowID: "feature_change", WorkflowSHA: "abc",
		TriggerSource: run.TriggerCLI,
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	if _, err := runRepo.TransitionRun(ctx, r.ID, run.StateRunning); err != nil {
		t.Fatalf("run -> running: %v", err)
	}
	var review *run.Stage
	for i, typ := range []run.StageType{run.StageTypePlan, run.StageTypeImplement, run.StageTypeReview} {
		st, err := runRepo.CreateStage(ctx, run.CreateStageParams{
			RunID: r.ID, Sequence: i, Type: typ,
			ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code",
		})
		if err != nil {
			t.Fatalf("create %s stage: %v", typ, err)
		}
		want := run.StageStateSucceeded
		if typ == run.StageTypeReview {
			want = run.StageStateAwaitingApproval
		}
		st = driveStageTo(t, runRepo, st, want)
		if typ == run.StageTypeReview {
			review = st
		}
	}
	prURL := "https://github.com/x/y/pull/77"
	if _, err := runRepo.SetRunPullRequestURL(ctx, r.ID, prURL); err != nil {
		t.Fatalf("set PR URL: %v", err)
	}
	return &reopenPGFixture{s: s, runs: runRepo, audit: auditRepo, runID: r.ID, reviewID: review.ID, prURL: prURL}
}

// deliver POSTs a signed pull_request webhook with the given action.
func (f *reopenPGFixture) deliver(t *testing.T, action string, merged bool, head string) {
	t.Helper()
	f.delivery++
	pr := map[string]any{"html_url": f.prURL, "number": 77, "merged": merged,
		"head": map[string]any{"sha": head}, "base": map[string]any{"sha": "base"}}
	if merged {
		pr["merged_by"] = map[string]any{"login": "carol"}
	}
	body, _ := json.Marshal(map[string]any{
		"action": action, "repository": map[string]any{"full_name": "x/y"},
		"sender": map[string]any{"login": "bob"}, "pull_request": pr,
	})
	w := postWebhook(t, f.s, map[string]string{
		"X-GitHub-Event":      "pull_request",
		"X-GitHub-Delivery":   fmt.Sprintf("00000000-0000-0000-0000-%012d", f.delivery),
		"X-Hub-Signature-256": sign(body),
		"Content-Type":        "application/json",
	}, body)
	if w.Code != http.StatusAccepted {
		t.Fatalf("%s delivery: status = %d, want 202:\n%s", action, w.Code, w.Body.String())
	}
}

// states re-reads the committed run and review-stage states.
func (f *reopenPGFixture) states(t *testing.T) (run.State, run.StageState) {
	t.Helper()
	ctx := context.Background()
	r, err := f.runs.GetRun(ctx, f.runID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	st, err := f.runs.GetStage(ctx, f.reviewID)
	if err != nil {
		t.Fatalf("get review stage: %v", err)
	}
	return r.State, st.State
}

func (f *reopenPGFixture) rows(t *testing.T, category string) []*audit.Entry {
	t.Helper()
	es, err := f.audit.ListForRunByCategory(context.Background(), f.runID, category)
	if err != nil {
		t.Fatalf("list %s: %v", category, err)
	}
	return es
}

// TestPullRequestReopen_PG_CloseReopenMerge is the done-means: closing the PR
// cancels the run, a reopen at the same head inside the window revives it to
// its review gate (run running, review awaiting_approval, run_revived_on_reopen
// committed), and a following merge resolves review and run to succeeded — the
// revived run is reviewable and mergeable. Counterfactual (C13): drop
// run_state_at_close from the close payload and the reopen refuses
// close_did_not_cancel_run, leaving the run cancelled (RED).
func TestPullRequestReopen_PG_CloseReopenMerge(t *testing.T) {
	f := newReopenPGFixture(t)

	f.deliver(t, "closed", false, "aaa")
	if runState, reviewState := f.states(t); runState != run.StateCancelled || reviewState != run.StageStateCancelled {
		t.Fatalf("after close: run/review = %s/%s, want cancelled/cancelled", runState, reviewState)
	}

	f.deliver(t, "reopened", false, "aaa")
	if refused := f.rows(t, CategoryRunReviveOnReopenRefused); len(refused) != 0 {
		t.Fatalf("reopen refused: %s", refused[0].Payload)
	}
	runState, reviewState := f.states(t)
	if runState != run.StateRunning || reviewState != run.StageStateAwaitingApproval {
		t.Fatalf("after reopen: run/review = %s/%s, want running/awaiting_approval", runState, reviewState)
	}
	revived := f.rows(t, CategoryRunRevivedOnReopen)
	if len(revived) != 1 || revived[0].StageID == nil || *revived[0].StageID != f.reviewID {
		t.Fatalf("run_revived_on_reopen rows = %+v, want exactly one on the review stage", revived)
	}

	f.deliver(t, "closed", true, "aaa")
	if runState, reviewState := f.states(t); runState != run.StateSucceeded || reviewState != run.StageStateSucceeded {
		t.Fatalf("after merge: run/review = %s/%s, want succeeded/succeeded", runState, reviewState)
	}
}

// TestPullRequestReopen_PG_WindowElapsedStaysCancelled seeds the PR-close
// cancel shape with its close row stamped 11 minutes ago (one clock domain:
// fishhawkd stamps the row, fishhawkd measures the window), then delivers the
// reopen: the run stays cancelled and one run_revive_on_reopen_refused row
// with reason window_elapsed is committed.
func TestPullRequestReopen_PG_WindowElapsedStaysCancelled(t *testing.T) {
	f := newReopenPGFixture(t)
	ctx := context.Background()

	reviewID := f.reviewID
	closer := "alice"
	user := audit.ActorKind("user")
	payload, _ := json.Marshal(map[string]any{
		"pr_url": f.prURL, "closer": closer, "head_sha": "aaa", "base_sha": "base",
		"run_state_at_close": "running", "review_state_at_close": "awaiting_approval",
	})
	if _, err := f.audit.AppendChained(ctx, audit.ChainAppendParams{
		RunID: f.runID, StageID: &reviewID, Timestamp: time.Now().UTC().Add(-11 * time.Minute),
		Category: CategoryPRClosedWithoutMerge, ActorKind: &user, ActorSubject: &closer, Payload: payload,
	}); err != nil {
		t.Fatalf("seed close row: %v", err)
	}
	if _, err := f.runs.TransitionStage(ctx, f.reviewID, run.StageStateCancelled, nil); err != nil {
		t.Fatalf("review -> cancelled: %v", err)
	}
	if _, err := f.runs.TransitionRun(ctx, f.runID, run.StateCancelled); err != nil {
		t.Fatalf("run -> cancelled: %v", err)
	}

	f.deliver(t, "reopened", false, "aaa")
	if runState, reviewState := f.states(t); runState != run.StateCancelled || reviewState != run.StageStateCancelled {
		t.Fatalf("run/review = %s/%s, want the run left cancelled", runState, reviewState)
	}
	if n := len(f.rows(t, CategoryRunRevivedOnReopen)); n != 0 {
		t.Fatalf("run_revived_on_reopen rows = %d, want 0", n)
	}
	refused := f.rows(t, CategoryRunReviveOnReopenRefused)
	if len(refused) != 1 {
		t.Fatalf("run_revive_on_reopen_refused rows = %d, want 1", len(refused))
	}
	var p struct {
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(refused[0].Payload, &p); err != nil || p.Reason != reopenRefusedWindowElapsed {
		t.Fatalf("refused payload = %s, want reason %s", refused[0].Payload, reopenRefusedWindowElapsed)
	}
}
