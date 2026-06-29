package postgres_test

import (
	"context"
	"io/fs"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/kuhlman-labs/fishhawk/backend/internal/postgres"
)

// startContainer spins up a throwaway Postgres 16 container and
// returns its connection URL. Skips the test if Docker isn't
// reachable so devs without Docker still pass `go test`.
func startContainer(t *testing.T) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	c, err := tcpostgres.Run(ctx,
		"postgres:16-alpine",
		tcpostgres.WithDatabase("fishhawk"),
		tcpostgres.WithUsername("fishhawk"),
		tcpostgres.WithPassword("fishhawk"),
		testcontainers.WithWaitStrategy(
			wait.ForAll(
				wait.ForLog("database system is ready to accept connections").
					WithOccurrence(2).
					WithStartupTimeout(60*time.Second),
				wait.ForListeningPort("5432/tcp"),
			),
		),
	)
	if err != nil {
		if isDockerUnavailable(err) {
			t.Skipf("Docker not available; skipping integration test: %v", err)
		}
		t.Fatalf("start postgres: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = c.Terminate(ctx)
	})

	url, err := c.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("conn string: %v", err)
	}
	return url
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
	return false
}

// TestMigrations_EmbeddedFiles confirms the //go:embed directive
// captured at least one .up.sql and one .down.sql migration. Catches
// the failure mode where someone moves the migrations directory and
// the embed silently empties.
func TestMigrations_EmbeddedFiles(t *testing.T) {
	mfs := postgres.Migrations()
	var entries []string
	if err := fs.WalkDir(mfs, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			entries = append(entries, path)
		}
		return nil
	}); err != nil {
		t.Fatalf("walk migrations: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("expected at least one embedded migration file; got none")
	}

	var foundUp, foundDown bool
	for _, e := range entries {
		switch {
		case strings.HasSuffix(e, ".up.sql"):
			foundUp = true
		case strings.HasSuffix(e, ".down.sql"):
			foundDown = true
		}
	}
	if !foundUp {
		t.Errorf("no .up.sql migration found in embed; entries: %v", entries)
	}
	if !foundDown {
		t.Errorf("no .down.sql migration found in embed; entries: %v", entries)
	}
}

func TestConnect_HappyPath(t *testing.T) {
	url := startContainer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pool, err := postgres.Connect(ctx, url)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer pool.Close()

	if err := pool.Ping(ctx); err != nil {
		t.Errorf("post-Connect Ping: %v", err)
	}
}

func TestConnect_MalformedURL(t *testing.T) {
	_, err := postgres.Connect(context.Background(), "not-a-url-at-all")
	if err == nil {
		t.Fatal("expected error on malformed URL")
	}
}

func TestConnect_UnreachableHost(t *testing.T) {
	// 127.0.0.1:1 is a privileged port no daemon listens on by default.
	// Use a tight context deadline so the test completes quickly even
	// if the OS would otherwise wait for the connect syscall to time
	// out.
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_, err := postgres.Connect(ctx, "postgres://x:y@127.0.0.1:1/db?sslmode=disable")
	if err == nil {
		t.Fatal("expected error connecting to unreachable host")
	}
}

func TestMigrateUp_AppliesAndIsIdempotent(t *testing.T) {
	url := startContainer(t)

	// First application creates the schema.
	if err := postgres.MigrateUp(url); err != nil {
		t.Fatalf("first MigrateUp: %v", err)
	}

	// Verify a known table exists.
	pool, err := postgres.Connect(context.Background(), url)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer pool.Close()

	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.tables WHERE table_name = 'runs'`,
	).Scan(&n); err != nil {
		t.Fatalf("query runs table: %v", err)
	}
	if n != 1 {
		t.Errorf("'runs' table count after MigrateUp = %d, want 1", n)
	}

	// 0035 (#1231) widened stages_state_check to admit
	// 'awaiting_scope_decision' and added the scope_completeness_park
	// column. Confirm both are present after a full MigrateUp.
	var stageStateCheckDef string
	if err := pool.QueryRow(context.Background(),
		`SELECT pg_get_constraintdef(oid) FROM pg_constraint
		 WHERE conname = 'stages_state_check'`,
	).Scan(&stageStateCheckDef); err != nil {
		t.Fatalf("query stages_state_check constraint def: %v", err)
	}
	if !strings.Contains(stageStateCheckDef, "awaiting_scope_decision") {
		t.Errorf("stages_state_check after MigrateUp does not admit 'awaiting_scope_decision': %s", stageStateCheckDef)
	}
	var scopeParkCol int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.columns
		 WHERE table_name = 'stages' AND column_name = 'scope_completeness_park'`,
	).Scan(&scopeParkCol); err != nil {
		t.Fatalf("query stages.scope_completeness_park column: %v", err)
	}
	if scopeParkCol != 1 {
		t.Errorf("stages.scope_completeness_park count after MigrateUp = %d, want 1", scopeParkCol)
	}

	// 0036 (#1346) added the runs.runner_kind_resolved lock flag. Confirm it
	// is present after a full MigrateUp.
	var runnerKindResolvedCol int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.columns
		 WHERE table_name = 'runs' AND column_name = 'runner_kind_resolved'`,
	).Scan(&runnerKindResolvedCol); err != nil {
		t.Fatalf("query runs.runner_kind_resolved column: %v", err)
	}
	if runnerKindResolvedCol != 1 {
		t.Errorf("runs.runner_kind_resolved count after MigrateUp = %d, want 1", runnerKindResolvedCol)
	}

	// 0037 (#1385) widened artifacts_kind_check to admit 'deployment'.
	// Confirm the CHECK names it after a full MigrateUp.
	var artifactsKindCheckDef string
	if err := pool.QueryRow(context.Background(),
		`SELECT pg_get_constraintdef(oid) FROM pg_constraint
		 WHERE conname = 'artifacts_kind_check'`,
	).Scan(&artifactsKindCheckDef); err != nil {
		t.Fatalf("query artifacts_kind_check constraint def: %v", err)
	}
	if !strings.Contains(artifactsKindCheckDef, "deployment") {
		t.Errorf("artifacts_kind_check after MigrateUp does not admit 'deployment': %s", artifactsKindCheckDef)
	}

	// 0038 (#1400) widened stages_type_check to admit 'deploy' and
	// stages_state_check to admit the two deploy states
	// 'awaiting_deploy_approval' and 'awaiting_deployment'. Confirm both
	// CHECKs name them after a full MigrateUp — without this widening a
	// real deploy stage row is uninsertable (SQLSTATE 23514).
	var stageTypeCheckDef string
	if err := pool.QueryRow(context.Background(),
		`SELECT pg_get_constraintdef(oid) FROM pg_constraint
		 WHERE conname = 'stages_type_check'`,
	).Scan(&stageTypeCheckDef); err != nil {
		t.Fatalf("query stages_type_check constraint def: %v", err)
	}
	if !strings.Contains(stageTypeCheckDef, "deploy") {
		t.Errorf("stages_type_check after MigrateUp does not admit 'deploy': %s", stageTypeCheckDef)
	}
	if !strings.Contains(stageStateCheckDef, "awaiting_deploy_approval") {
		t.Errorf("stages_state_check after MigrateUp does not admit 'awaiting_deploy_approval': %s", stageStateCheckDef)
	}
	if !strings.Contains(stageStateCheckDef, "awaiting_deployment") {
		t.Errorf("stages_state_check after MigrateUp does not admit 'awaiting_deployment': %s", stageStateCheckDef)
	}

	// 0039 (#1437) added the campaigns + campaign_items tables (the
	// campaign keystone). Confirm both exist after a full MigrateUp.
	var campaignsTable, campaignItemsTable int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.tables WHERE table_name = 'campaigns'`,
	).Scan(&campaignsTable); err != nil {
		t.Fatalf("query campaigns table: %v", err)
	}
	if campaignsTable != 1 {
		t.Errorf("'campaigns' table count after MigrateUp = %d, want 1", campaignsTable)
	}
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.tables WHERE table_name = 'campaign_items'`,
	).Scan(&campaignItemsTable); err != nil {
		t.Fatalf("query campaign_items table: %v", err)
	}
	if campaignItemsTable != 1 {
		t.Errorf("'campaign_items' table count after MigrateUp = %d, want 1", campaignItemsTable)
	}

	// 0040 (#1446) widened campaigns_state_check + campaign_items_state_check
	// to admit 'paused', added campaigns.pause_policy, and added the nullable
	// campaign_items.pause_reason JSONB. Confirm the columns exist and a
	// 'paused' campaign + item row insert succeeds (the widened CHECK) —
	// without the widening a paused row is uninsertable (SQLSTATE 23514).
	var pausePolicyCol, pauseReasonCol int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.columns
		 WHERE table_name = 'campaigns' AND column_name = 'pause_policy'`,
	).Scan(&pausePolicyCol); err != nil {
		t.Fatalf("query campaigns.pause_policy column: %v", err)
	}
	if pausePolicyCol != 1 {
		t.Errorf("campaigns.pause_policy count after MigrateUp = %d, want 1", pausePolicyCol)
	}
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.columns
		 WHERE table_name = 'campaign_items' AND column_name = 'pause_reason'`,
	).Scan(&pauseReasonCol); err != nil {
		t.Fatalf("query campaign_items.pause_reason column: %v", err)
	}
	if pauseReasonCol != 1 {
		t.Errorf("campaign_items.pause_reason count after MigrateUp = %d, want 1", pauseReasonCol)
	}
	campaignID := uuid.New()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO campaigns (id, repo, epic_ref, state) VALUES ($1, 'r', 'issue:1', 'paused')`,
		campaignID,
	); err != nil {
		t.Errorf("insert 'paused' campaign after MigrateUp failed (widened CHECK?): %v", err)
	}
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO campaign_items (id, campaign_id, issue_ref, state, pause_reason)
		 VALUES ($1, $2, 'issue:2', 'paused', '{"page_event":"campaign_gate_paged"}'::jsonb)`,
		uuid.New(), campaignID,
	); err != nil {
		t.Errorf("insert 'paused' campaign_item after MigrateUp failed (widened CHECK?): %v", err)
	}

	// Second application is a no-op.
	if err := postgres.MigrateUp(url); err != nil {
		t.Errorf("second MigrateUp returned %v, want nil (idempotent)", err)
	}
}

func TestMigrateUp_MalformedURL(t *testing.T) {
	if err := postgres.MigrateUp("not-a-url"); err == nil {
		t.Fatal("expected error on malformed URL")
	}
}

func TestMigrateDown_RemovesTables(t *testing.T) {
	url := startContainer(t)

	if err := postgres.MigrateUp(url); err != nil {
		t.Fatalf("MigrateUp: %v", err)
	}
	if err := postgres.MigrateDown(url); err != nil {
		t.Fatalf("MigrateDown: %v", err)
	}

	pool, err := postgres.Connect(context.Background(), url)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer pool.Close()

	// MigrateDown rolls back one step. 0040 (#1446) is now the latest
	// migration: it is an ALTER (widened the campaign state CHECKs to admit
	// 'paused' and added campaigns.pause_policy + campaign_items.pause_reason),
	// NOT a table create. So its rollback removes the two added columns and
	// re-narrows the CHECK (a 'paused' insert is rejected again), while 0039's
	// (#1437) campaigns + campaign_items tables themselves still EXIST. Every
	// prior migration's effect is likewise still present — notably 0038's
	// (#1400) widened stages_type_check ('deploy') and stages_state_check (the
	// two deploy states), 0037's (#1385) artifacts_kind_check 'deployment',
	// 0036's (#1346) runs.runner_kind_resolved column, etc.
	var campaignsTable, campaignItemsTable int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.tables WHERE table_name = 'campaigns'`,
	).Scan(&campaignsTable); err != nil {
		t.Fatalf("query campaigns table: %v", err)
	}
	if campaignsTable != 1 {
		t.Errorf("'campaigns' table count after MigrateDown = %d, want 1 (0040 is an ALTER; 0039's table survives)", campaignsTable)
	}
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.tables WHERE table_name = 'campaign_items'`,
	).Scan(&campaignItemsTable); err != nil {
		t.Fatalf("query campaign_items table: %v", err)
	}
	if campaignItemsTable != 1 {
		t.Errorf("'campaign_items' table count after MigrateDown = %d, want 1 (0040 is an ALTER; 0039's table survives)", campaignItemsTable)
	}
	// 0040's two added columns are gone after its rollback.
	var pausePolicyCol, pauseReasonCol int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.columns
		 WHERE table_name = 'campaigns' AND column_name = 'pause_policy'`,
	).Scan(&pausePolicyCol); err != nil {
		t.Fatalf("query campaigns.pause_policy column: %v", err)
	}
	if pausePolicyCol != 0 {
		t.Errorf("campaigns.pause_policy count after MigrateDown = %d, want 0 (0040 rolled back)", pausePolicyCol)
	}
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.columns
		 WHERE table_name = 'campaign_items' AND column_name = 'pause_reason'`,
	).Scan(&pauseReasonCol); err != nil {
		t.Fatalf("query campaign_items.pause_reason column: %v", err)
	}
	if pauseReasonCol != 0 {
		t.Errorf("campaign_items.pause_reason count after MigrateDown = %d, want 0 (0040 rolled back)", pauseReasonCol)
	}
	// The re-narrowed CHECK rejects a 'paused' campaign insert again.
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO campaigns (id, repo, epic_ref, state) VALUES ($1, 'r', 'issue:1', 'paused')`,
		uuid.New(),
	); err == nil {
		t.Error("insert 'paused' campaign after MigrateDown succeeded, want rejection (0040's widened CHECK rolled back)")
	}
	var artifactsKindCheckDef string
	if err := pool.QueryRow(context.Background(),
		`SELECT pg_get_constraintdef(oid) FROM pg_constraint
		 WHERE conname = 'artifacts_kind_check'`,
	).Scan(&artifactsKindCheckDef); err != nil {
		t.Fatalf("query artifacts_kind_check constraint def: %v", err)
	}
	if !strings.Contains(artifactsKindCheckDef, "deployment") {
		t.Errorf("artifacts_kind_check after MigrateDown dropped 'deployment' (0037 still applied; only 0039 rolled back): %s", artifactsKindCheckDef)
	}
	// 0038 (#1400) is now a PRIOR migration (only 0039 rolled back), so its
	// widened stages_type_check ('deploy') and stages_state_check (the two
	// deploy states) must still be present.
	var stageTypeCheckDef string
	if err := pool.QueryRow(context.Background(),
		`SELECT pg_get_constraintdef(oid) FROM pg_constraint
		 WHERE conname = 'stages_type_check'`,
	).Scan(&stageTypeCheckDef); err != nil {
		t.Fatalf("query stages_type_check constraint def: %v", err)
	}
	if !strings.Contains(stageTypeCheckDef, "deploy") {
		t.Errorf("stages_type_check after MigrateDown dropped 'deploy' (0038 still applied; only 0039 rolled back): %s", stageTypeCheckDef)
	}
	var runnerKindResolvedCol int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.columns
		 WHERE table_name = 'runs' AND column_name = 'runner_kind_resolved'`,
	).Scan(&runnerKindResolvedCol); err != nil {
		t.Fatalf("query runs.runner_kind_resolved column: %v", err)
	}
	if runnerKindResolvedCol != 1 {
		t.Errorf("runs.runner_kind_resolved count after MigrateDown = %d, want 1 (0036 still applied; only 0039 rolled back)", runnerKindResolvedCol)
	}
	var scopeParkCol int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.columns
		 WHERE table_name = 'stages' AND column_name = 'scope_completeness_park'`,
	).Scan(&scopeParkCol); err != nil {
		t.Fatalf("query stages.scope_completeness_park column: %v", err)
	}
	if scopeParkCol != 1 {
		t.Errorf("stages.scope_completeness_park count after MigrateDown = %d, want 1 (0035 still applied; only 0039 rolled back)", scopeParkCol)
	}
	var sliceIndexCol int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.columns
		 WHERE table_name = 'runs' AND column_name = 'slice_index'`,
	).Scan(&sliceIndexCol); err != nil {
		t.Fatalf("query runs.slice_index column: %v", err)
	}
	if sliceIndexCol != 1 {
		t.Errorf("runs.slice_index count after MigrateDown = %d, want 1 (0034 still applied; only 0039 rolled back)", sliceIndexCol)
	}
	var suggestedPatchCol int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.columns
		 WHERE table_name = 'review_concerns' AND column_name = 'suggested_patch'`,
	).Scan(&suggestedPatchCol); err != nil {
		t.Fatalf("query review_concerns.suggested_patch column: %v", err)
	}
	if suggestedPatchCol != 1 {
		t.Errorf("review_concerns.suggested_patch count after MigrateDown = %d, want 1 (0033 still applied; only 0039 rolled back)", suggestedPatchCol)
	}
	var stageStateCheckDef string
	if err := pool.QueryRow(context.Background(),
		`SELECT pg_get_constraintdef(oid) FROM pg_constraint
		 WHERE conname = 'stages_state_check'`,
	).Scan(&stageStateCheckDef); err != nil {
		t.Fatalf("query stages_state_check constraint def: %v", err)
	}
	// 0038 (#1400) is a PRIOR migration now (only 0039 rolled back), so its
	// widened stages_state_check still admits the two deploy states, and
	// 0035's 'awaiting_scope_decision', 0032's 'awaiting_input' and
	// 'awaiting_children' survive too.
	if !strings.Contains(stageStateCheckDef, "awaiting_deploy_approval") {
		t.Errorf("stages_state_check after MigrateDown dropped 'awaiting_deploy_approval' (0038 still applied; only 0039 rolled back): %s", stageStateCheckDef)
	}
	if !strings.Contains(stageStateCheckDef, "awaiting_deployment") {
		t.Errorf("stages_state_check after MigrateDown dropped 'awaiting_deployment' (0038 still applied; only 0039 rolled back): %s", stageStateCheckDef)
	}
	if !strings.Contains(stageStateCheckDef, "awaiting_scope_decision") {
		t.Errorf("stages_state_check after MigrateDown dropped 'awaiting_scope_decision' (0035 still applied; only 0039 should roll back): %s", stageStateCheckDef)
	}
	if !strings.Contains(stageStateCheckDef, "awaiting_input") {
		t.Errorf("stages_state_check after MigrateDown dropped 'awaiting_input' (0032 still applied; only 0038 should roll back): %s", stageStateCheckDef)
	}
	if !strings.Contains(stageStateCheckDef, "awaiting_children") {
		t.Errorf("stages_state_check after MigrateDown dropped 'awaiting_children': %s", stageStateCheckDef)
	}
	var driveCol int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.columns
		 WHERE table_name = 'runs' AND column_name = 'drive'`,
	).Scan(&driveCol); err != nil {
		t.Fatalf("query runs.drive column: %v", err)
	}
	if driveCol != 1 {
		t.Errorf("runs.drive column count after MigrateDown = %d, want 1 (0031 still applied; only 0033 rolled back)", driveCol)
	}
	var reviewConcernsTable int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.tables
		 WHERE table_name = 'review_concerns'`,
	).Scan(&reviewConcernsTable); err != nil {
		t.Fatalf("query review_concerns table: %v", err)
	}
	if reviewConcernsTable != 1 {
		t.Errorf("review_concerns table count after MigrateDown = %d, want 1 (0030 still applied; only 0033 rolled back)", reviewConcernsTable)
	}
	var scopeAmendmentsTable int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.tables
		 WHERE table_name = 'scope_amendments'`,
	).Scan(&scopeAmendmentsTable); err != nil {
		t.Fatalf("query scope_amendments table: %v", err)
	}
	if scopeAmendmentsTable != 1 {
		t.Errorf("scope_amendments table count after MigrateDown = %d, want 1 (0029 still applied; only 0031 rolled back)", scopeAmendmentsTable)
	}
	var costUSDTotalCol int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.columns
		 WHERE table_name = 'runs' AND column_name = 'cost_usd_total'`,
	).Scan(&costUSDTotalCol); err != nil {
		t.Fatalf("query runs.cost_usd_total column: %v", err)
	}
	if costUSDTotalCol != 1 {
		t.Errorf("runs.cost_usd_total count after MigrateDown = %d, want 1 (0028 still applied after one-step down)", costUSDTotalCol)
	}
	var resolvedModelCol int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.columns
		 WHERE table_name = 'runs' AND column_name = 'resolved_model'`,
	).Scan(&resolvedModelCol); err != nil {
		t.Fatalf("query runs.resolved_model column: %v", err)
	}
	if resolvedModelCol != 1 {
		t.Errorf("runs.resolved_model count after MigrateDown = %d, want 1 (0028 still applied after one-step down)", resolvedModelCol)
	}
	var selfRetryCountCol int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.columns
		 WHERE table_name = 'stages' AND column_name = 'self_retry_count'`,
	).Scan(&selfRetryCountCol); err != nil {
		t.Fatalf("query stages.self_retry_count column: %v", err)
	}
	if selfRetryCountCol != 1 {
		t.Errorf("stages.self_retry_count count after MigrateDown = %d, want 1 (0027 still applied; only 0028 rolled back)", selfRetryCountCol)
	}
	var mcpScopesCol int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.columns
		 WHERE table_name = 'mcp_tokens' AND column_name = 'scopes'`,
	).Scan(&mcpScopesCol); err != nil {
		t.Fatalf("query mcp_tokens.scopes column: %v", err)
	}
	if mcpScopesCol != 1 {
		t.Errorf("mcp_tokens.scopes count after MigrateDown = %d, want 1 (0027 still applied; only 0028 rolled back)", mcpScopesCol)
	}
	var decomposedFromCol int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.columns
		 WHERE table_name = 'runs' AND column_name = 'decomposed_from'`,
	).Scan(&decomposedFromCol); err != nil {
		t.Fatalf("query runs.decomposed_from column: %v", err)
	}
	if decomposedFromCol != 1 {
		t.Errorf("runs.decomposed_from count after MigrateDown = %d, want 1 (0026 still applied; only 0027 rolled back)", decomposedFromCol)
	}
	var issueContextCol int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.columns
		 WHERE table_name = 'runs' AND column_name = 'issue_context'`,
	).Scan(&issueContextCol); err != nil {
		t.Fatalf("query runs.issue_context column: %v", err)
	}
	if issueContextCol != 1 {
		t.Errorf("runs.issue_context count after MigrateDown = %d, want 1 (0025 still applied; only 0027 rolled back)", issueContextCol)
	}
	var runnerKindCol int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.columns
		 WHERE table_name = 'runs' AND column_name = 'runner_kind'`,
	).Scan(&runnerKindCol); err != nil {
		t.Fatalf("query runs.runner_kind column: %v", err)
	}
	if runnerKindCol != 1 {
		t.Errorf("runs.runner_kind count after MigrateDown = %d, want 1 (0024 still applied; only 0027 rolled back)", runnerKindCol)
	}
	var mcpTokensTable int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.tables
		 WHERE table_name = 'mcp_tokens'`,
	).Scan(&mcpTokensTable); err != nil {
		t.Fatalf("query mcp_tokens table: %v", err)
	}
	if mcpTokensTable != 1 {
		t.Errorf("mcp_tokens table count after MigrateDown = %d, want 1 (0023 still applied; only 0027 rolled back)", mcpTokensTable)
	}
	var maxRetriesCol, retryAttemptCol, workflowSpecCol, gateBlockingChecksCol, requiredChecksCol, parentRunIDCol, pullRequestURLCol, stageChecksTable, gateTypeCol, requiresApprovalCol, signingIDCol, idempotencyCol, usersCount, sessionsCount, apiTokensCount, deliveriesCount, approvalsCount, runsCount int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.columns
		 WHERE table_name = 'runs' AND column_name = 'max_retries_snapshot'`,
	).Scan(&maxRetriesCol); err != nil {
		t.Fatalf("query runs.max_retries_snapshot column: %v", err)
	}
	if maxRetriesCol != 1 {
		t.Errorf("runs.max_retries_snapshot count after MigrateDown = %d, want 1 (0021 still applied; only 0022 rolled back)", maxRetriesCol)
	}
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.columns
		 WHERE table_name = 'runs' AND column_name = 'retry_attempt'`,
	).Scan(&retryAttemptCol); err != nil {
		t.Fatalf("query runs.retry_attempt column: %v", err)
	}
	if retryAttemptCol != 1 {
		t.Errorf("runs.retry_attempt count after MigrateDown = %d, want 1 (0020 still applied)", retryAttemptCol)
	}
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.columns
		 WHERE table_name = 'runs' AND column_name = 'workflow_spec'`,
	).Scan(&workflowSpecCol); err != nil {
		t.Fatalf("query runs.workflow_spec column: %v", err)
	}
	if workflowSpecCol != 1 {
		t.Errorf("runs.workflow_spec count after MigrateDown = %d, want 1 (0019 still applied)", workflowSpecCol)
	}
	// 0018 (drop gate_blocking_checks) is still applied — its down
	// would restore the column, but we only rolled back 0019.
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.columns
		 WHERE table_name = 'stages' AND column_name = 'gate_blocking_checks'`,
	).Scan(&gateBlockingChecksCol); err != nil {
		t.Fatalf("query stages.gate_blocking_checks column: %v", err)
	}
	if gateBlockingChecksCol != 0 {
		t.Errorf("stages.gate_blocking_checks count after MigrateDown = %d, want 0 (0018 still applied — only 0019 rolled back)", gateBlockingChecksCol)
	}
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.columns
		 WHERE table_name = 'runs' AND column_name = 'required_checks_snapshot'`,
	).Scan(&requiredChecksCol); err != nil {
		t.Fatalf("query runs.required_checks_snapshot column: %v", err)
	}
	if requiredChecksCol != 1 {
		t.Errorf("runs.required_checks_snapshot count after MigrateDown = %d, want 1 (0017 still applied)", requiredChecksCol)
	}
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.columns
		 WHERE table_name = 'runs' AND column_name = 'parent_run_id'`,
	).Scan(&parentRunIDCol); err != nil {
		t.Fatalf("query runs.parent_run_id column: %v", err)
	}
	if parentRunIDCol != 1 {
		t.Errorf("runs.parent_run_id count after MigrateDown = %d, want 1 (0016 still applied)", parentRunIDCol)
	}
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.columns
		 WHERE table_name = 'runs' AND column_name = 'pull_request_url'`,
	).Scan(&pullRequestURLCol); err != nil {
		t.Fatalf("query runs.pull_request_url column: %v", err)
	}
	if pullRequestURLCol != 1 {
		t.Errorf("runs.pull_request_url count after MigrateDown = %d, want 1 (0016 still applied)", pullRequestURLCol)
	}
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.tables
		 WHERE table_name = 'stage_checks'`,
	).Scan(&stageChecksTable); err != nil {
		t.Fatalf("query stage_checks table: %v", err)
	}
	if stageChecksTable != 1 {
		t.Errorf("stage_checks table count after MigrateDown = %d, want 1 (0015 still applied)", stageChecksTable)
	}
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.columns
		 WHERE table_name = 'stages' AND column_name = 'gate_type'`,
	).Scan(&gateTypeCol); err != nil {
		t.Fatalf("query stages.gate_type column: %v", err)
	}
	if gateTypeCol != 1 {
		t.Errorf("stages.gate_type count after MigrateDown = %d, want 1 (0014 still applied)", gateTypeCol)
	}
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.columns
		 WHERE table_name = 'stages' AND column_name = 'requires_approval'`,
	).Scan(&requiresApprovalCol); err != nil {
		t.Fatalf("query stages.requires_approval column: %v", err)
	}
	if requiresApprovalCol != 1 {
		t.Errorf("stages.requires_approval count after MigrateDown = %d, want 1 (0013 still applied)", requiresApprovalCol)
	}
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.columns
		 WHERE table_name = 'signing_keys' AND column_name = 'id'`,
	).Scan(&signingIDCol); err != nil {
		t.Fatalf("query signing_keys.id column: %v", err)
	}
	if signingIDCol != 1 {
		t.Errorf("signing_keys.id count after MigrateDown = %d, want 1 (0012 still applied)", signingIDCol)
	}
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.columns
		 WHERE table_name = 'runs' AND column_name = 'idempotency_key'`,
	).Scan(&idempotencyCol); err != nil {
		t.Fatalf("query idempotency_key column: %v", err)
	}
	if idempotencyCol != 1 {
		t.Errorf("runs.idempotency_key count after MigrateDown = %d, want 1 (0011 still applied)", idempotencyCol)
	}
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.tables WHERE table_name = 'sessions'`,
	).Scan(&sessionsCount); err != nil {
		t.Fatalf("query sessions table: %v", err)
	}
	if sessionsCount != 1 {
		t.Errorf("sessions count after MigrateDown = %d, want 1 (0010 still applied)", sessionsCount)
	}
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.tables WHERE table_name = 'users'`,
	).Scan(&usersCount); err != nil {
		t.Fatalf("query users table: %v", err)
	}
	if usersCount != 1 {
		t.Errorf("users count after MigrateDown = %d, want 1 (0010 still applied)", usersCount)
	}
	var auditRunIDNullable string
	if err := pool.QueryRow(context.Background(),
		`SELECT is_nullable FROM information_schema.columns
		 WHERE table_name = 'audit_entries' AND column_name = 'run_id'`,
	).Scan(&auditRunIDNullable); err != nil {
		t.Fatalf("query audit_entries.run_id is_nullable: %v", err)
	}
	if auditRunIDNullable != "YES" {
		t.Errorf("audit_entries.run_id is_nullable after MigrateDown = %q, want YES (0009 still applied)", auditRunIDNullable)
	}
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.tables WHERE table_name = 'api_tokens'`,
	).Scan(&apiTokensCount); err != nil {
		t.Fatalf("query api_tokens table: %v", err)
	}
	if apiTokensCount != 1 {
		t.Errorf("api_tokens count after MigrateDown = %d, want 1 (0008 still applied)", apiTokensCount)
	}
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.tables WHERE table_name = 'webhook_deliveries'`,
	).Scan(&deliveriesCount); err != nil {
		t.Fatalf("query webhook_deliveries table: %v", err)
	}
	if deliveriesCount != 1 {
		t.Errorf("webhook_deliveries count after MigrateDown = %d, want 1 (0007 still applied)", deliveriesCount)
	}
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.tables WHERE table_name = 'approvals'`,
	).Scan(&approvalsCount); err != nil {
		t.Fatalf("query approvals table: %v", err)
	}
	if approvalsCount != 1 {
		t.Errorf("approvals count after one MigrateDown = %d, want 1 (still present)", approvalsCount)
	}
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.tables WHERE table_name = 'runs'`,
	).Scan(&runsCount); err != nil {
		t.Fatalf("query runs table: %v", err)
	}
	if runsCount != 1 {
		t.Errorf("'runs' count after one MigrateDown = %d, want 1 (still present)", runsCount)
	}
}

// TestMigrateDown_NormalizesPausedRows is the binding-condition-#1
// rollback-realism guard: 0040's down migration must NOT fail when live
// 'paused' rows exist. Before re-adding the narrower state CHECK constraints
// the down migration normalizes any paused campaign/item to 'running', so the
// re-add validates instead of raising SQLSTATE 23514. Insert a paused campaign
// + item, MigrateDown, and assert it succeeds AND the rows were normalized to
// running (the campaign tables survive — 0040 is an ALTER).
func TestMigrateDown_NormalizesPausedRows(t *testing.T) {
	url := startContainer(t)

	if err := postgres.MigrateUp(url); err != nil {
		t.Fatalf("MigrateUp: %v", err)
	}

	pool, err := postgres.Connect(context.Background(), url)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}

	// Seed a live paused campaign and a paused item (admitted by 0040).
	campaignID := uuid.New()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO campaigns (id, repo, epic_ref, state) VALUES ($1, 'r', 'issue:1', 'paused')`,
		campaignID,
	); err != nil {
		t.Fatalf("seed paused campaign: %v", err)
	}
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO campaign_items (id, campaign_id, issue_ref, state) VALUES ($1, $2, 'issue:2', 'paused')`,
		uuid.New(), campaignID,
	); err != nil {
		t.Fatalf("seed paused item: %v", err)
	}
	pool.Close()

	// The down migration must succeed despite the live paused rows.
	if err := postgres.MigrateDown(url); err != nil {
		t.Fatalf("MigrateDown with a paused row present failed (normalization missing?): %v", err)
	}

	pool, err = postgres.Connect(context.Background(), url)
	if err != nil {
		t.Fatalf("re-Connect: %v", err)
	}
	defer pool.Close()

	// The paused campaign was normalized to running (not dropped — 0040 is an
	// ALTER, so 0039's table survives the one-step down).
	var campaignState string
	if err := pool.QueryRow(context.Background(),
		`SELECT state FROM campaigns WHERE id = $1`, campaignID,
	).Scan(&campaignState); err != nil {
		t.Fatalf("read campaign state after MigrateDown: %v", err)
	}
	if campaignState != "running" {
		t.Errorf("campaign state after MigrateDown = %q, want running (paused normalized)", campaignState)
	}
}

func TestMigrateDown_MalformedURL(t *testing.T) {
	if err := postgres.MigrateDown("not-a-url"); err == nil {
		t.Fatal("expected error on malformed URL")
	}
}
