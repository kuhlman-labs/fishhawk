package webhook_test

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/postgres"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/webhook"
)

// decomposedRetrySpec is the cached workflow spec the retry path reads its cap
// from: on_ci_failure.max_retries 1, a plan stage (skipped for a retry) and an
// implement stage. Mirrors the in-package ciRetrySpec, which an external test
// package cannot import.
const decomposedRetrySpec = `version: "0.3"
roles:
  tech_lead:
    members: ["@kuhlman-labs"]
workflows:
  feature_change:
    description: Test workflow with retries
    on_ci_failure:
      max_retries: 1
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
        gates:
          - type: approval
            approvers:
              any_of: [tech_lead]
            sla: 4_business_hours
      - id: implement
        type: implement
        executor:
          agent: claude-code
`

const decomposedRetryRepo = "kuhlman-labs/fishhawk"

// newRetryPool returns a pool on a fresh, migrated per-test database, following
// the same pgtest / MigrateUp / pgxpool lifecycle as newStore in
// postgres_test.go.
func newRetryPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := pgtest.NewURL(t)
	if err := postgres.MigrateUp(url); err != nil {
		t.Fatalf("MigrateUp: %v", err)
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// seedRetryRun mints a run through the REAL repository. Every seeded run
// carries a valid, non-empty TriggerSource (github_issue); the owning parent's
// PR URL is stamped separately by stampRetryPR after CreateRun, because
// CreateRunParams has no pull_request_url field — the column is written only by
// SetRunPullRequestURL.
func seedRetryRun(t *testing.T, repo run.Repository, mutate func(*run.CreateRunParams)) *run.Run {
	t.Helper()
	triggerRef := "issue:4813"
	p := run.CreateRunParams{
		Repo:          decomposedRetryRepo,
		WorkflowID:    "feature_change",
		WorkflowSHA:   "feedf00d",
		TriggerSource: run.TriggerGitHubIssue,
		TriggerRef:    &triggerRef,
		RequiredChecksSnapshot: &run.RequiredChecksSnapshot{
			Contexts: []string{"ci/build"},
			Sources:  []string{"branch_protection"},
		},
		WorkflowSpec:       []byte(decomposedRetrySpec),
		MaxRetriesSnapshot: 1,
		// local, so the retry path needs no forge client: a host-dispatched
		// backend leaves the child's first stage pending.
		RunnerKind: run.RunnerKindLocal,
	}
	if mutate != nil {
		mutate(&p)
	}
	r, err := repo.CreateRun(context.Background(), p)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	return r
}

// stampRetryPR records the PR URL the CI-failure retry path correlates on.
func stampRetryPR(t *testing.T, repo run.Repository, id uuid.UUID, prNumber int) {
	t.Helper()
	url := "https://github.com/" + decomposedRetryRepo + "/pull/" + strconv.Itoa(prNumber)
	if _, err := repo.SetRunPullRequestURL(context.Background(), id, url); err != nil {
		t.Fatalf("SetRunPullRequestURL(%s): %v", id, err)
	}
}

// failedCheckRunEvent builds a signed-shape check_run delivery: ci/build failed
// at sha on PR prNumber.
func failedCheckRunEvent(t *testing.T, deliveryID string, prNumber int, sha string) webhook.Event {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"action": "completed",
		"check_run": map[string]any{
			"name":          "ci/build",
			"head_sha":      sha,
			"conclusion":    "failure",
			"status":        "completed",
			"pull_requests": []map[string]any{{"number": prNumber}},
		},
		"repository":   map[string]any{"full_name": decomposedRetryRepo},
		"installation": map[string]any{"id": 42},
		"sender":       map[string]any{"login": "alice", "type": "User"},
	})
	if err != nil {
		t.Fatal(err)
	}
	ev, err := webhook.ParseEvent("check_run", deliveryID, raw)
	if err != nil {
		t.Fatalf("ParseEvent: %v", err)
	}
	return ev
}

// TestCIFailureRetry_DecomposedParent_PostgresRefusesWithAuditRow is the
// cross-boundary test for #4813: webhook delivery -> the real run repository ->
// the real audit chain. It seeds a decomposed parent with its slice child on
// PR 42 and an ORDINARY parent on PR 43, delivers a failed required check for
// each, then reads the committed state back.
//
// The ordinary PR is the positive control: the real DecomposedFrom filter is
// not vacuously refusing everything, and the same dispatcher, repositories and
// event shape DO mint a child where the PR is not a fan-in.
func TestCIFailureRetry_DecomposedParent_PostgresRefusesWithAuditRow(t *testing.T) {
	ctx := context.Background()
	pool := newRetryPool(t)
	runs := run.NewPostgresRepository(pool)
	au := audit.NewPostgresRepository(pool)

	decomposed := seedRetryRun(t, runs, nil)
	stampRetryPR(t, runs, decomposed.ID, 42)
	idx := 0
	slice := seedRetryRun(t, runs, func(p *run.CreateRunParams) {
		p.DecomposedFrom = &decomposed.ID
		p.SliceIndex = &idx
	})
	ordinary := seedRetryRun(t, runs, nil)
	stampRetryPR(t, runs, ordinary.ID, 43)

	d := &webhook.Dispatcher{Runs: runs, Audit: au}

	// 1. A failed required check on the DECOMPOSED parent's consolidated PR.
	if err := d.Handle(ctx, failedCheckRunEvent(t, "deliv-decomposed", 42, "abc123")); err != nil {
		t.Fatalf("Handle (decomposed PR): %v", err)
	}
	// 2. The same failure on an ORDINARY PR.
	if err := d.Handle(ctx, failedCheckRunEvent(t, "deliv-ordinary", 43, "def456")); err != nil {
		t.Fatalf("Handle (ordinary PR): %v", err)
	}

	all, err := runs.ListRuns(ctx, run.ListRunsFilter{Repo: decomposedRetryRepo, Limit: 50})
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	var retryOfDecomposed, retryOfOrdinary []*run.Run
	for _, r := range all {
		if r.ParentRunID == nil {
			continue
		}
		switch *r.ParentRunID {
		case decomposed.ID:
			retryOfDecomposed = append(retryOfDecomposed, r)
		case ordinary.ID:
			retryOfOrdinary = append(retryOfOrdinary, r)
		}
	}

	// The decomposed PR minted NO run, and its refusal is on the parent's chain.
	if len(retryOfDecomposed) != 0 {
		t.Fatalf("retry children of the decomposed parent = %d, want 0", len(retryOfDecomposed))
	}
	skipped, err := au.ListForRunByCategory(ctx, decomposed.ID, "ci_retry_skipped")
	if err != nil {
		t.Fatalf("ListForRunByCategory(decomposed, ci_retry_skipped): %v", err)
	}
	if len(skipped) != 1 {
		t.Fatalf("ci_retry_skipped rows on the decomposed parent = %d, want exactly 1", len(skipped))
	}
	var payload map[string]any
	if err := json.Unmarshal(skipped[0].Payload, &payload); err != nil {
		t.Fatalf("unmarshal ci_retry_skipped payload: %v", err)
	}
	if got, _ := payload["reason"].(string); got != "decomposed_parent_consolidated_lineage" {
		t.Errorf("reason = %q, want %q", got, "decomposed_parent_consolidated_lineage")
	}
	if got, _ := payload["run_id"].(string); got != decomposed.ID.String() {
		t.Errorf("payload run_id = %q, want %s", got, decomposed.ID)
	}
	if got, _ := payload["head_sha"].(string); got != "abc123" {
		t.Errorf("payload head_sha = %q, want abc123", got)
	}
	// The slice child is untouched: the refusal belongs to the parent alone.
	if rows, err := au.ListForRunByCategory(ctx, slice.ID, "ci_retry_skipped"); err != nil {
		t.Fatalf("ListForRunByCategory(slice): %v", err)
	} else if len(rows) != 0 {
		t.Errorf("ci_retry_skipped rows on the slice child = %d, want 0", len(rows))
	}

	// The ordinary PR minted exactly one retry child, with a dispatched row and
	// no refusal.
	if len(retryOfOrdinary) != 1 {
		t.Fatalf("retry children of the ordinary parent = %d, want 1", len(retryOfOrdinary))
	}
	child := retryOfOrdinary[0]
	if child.RetryAttempt != 1 {
		t.Errorf("ordinary retry child RetryAttempt = %d, want 1", child.RetryAttempt)
	}
	dispatched, err := au.ListForRunByCategory(ctx, child.ID, "ci_failure_retry_dispatched")
	if err != nil {
		t.Fatalf("ListForRunByCategory(child, ci_failure_retry_dispatched): %v", err)
	}
	if len(dispatched) != 1 {
		t.Errorf("ci_failure_retry_dispatched rows on the ordinary child = %d, want 1", len(dispatched))
	}
	if rows, err := au.ListForRunByCategory(ctx, ordinary.ID, "ci_retry_skipped"); err != nil {
		t.Fatalf("ListForRunByCategory(ordinary): %v", err)
	} else if len(rows) != 0 {
		t.Errorf("ci_retry_skipped rows on the ordinary parent = %d, want 0", len(rows))
	}
}
