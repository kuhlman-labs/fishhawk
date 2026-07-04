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
	// 0045 (#1531) widened artifacts_kind_check to admit 'acceptance' (the
	// acceptance-evidence artifact, E31.3 / ADR-049). Confirm the CHECK names
	// it after a full MigrateUp — without this widening a real acceptance
	// artifact row is uninsertable (SQLSTATE 23514).
	if !strings.Contains(artifactsKindCheckDef, "acceptance") {
		t.Errorf("artifacts_kind_check after MigrateUp does not admit 'acceptance': %s", artifactsKindCheckDef)
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
	// 0044 (#1519) widened stages_type_check to admit 'acceptance' (no new
	// state — acceptance rides the existing agent-stage lifecycle). Confirm
	// the CHECK names it after a full MigrateUp.
	if !strings.Contains(stageTypeCheckDef, "acceptance") {
		t.Errorf("stages_type_check after MigrateUp does not admit 'acceptance': %s", stageTypeCheckDef)
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

	// 0041 (#1451) added the nullable campaigns.operator_agent JSONB column —
	// the campaign-level delegation override. Confirm it exists and a non-null
	// block round-trips (an additive nullable column, no CHECK).
	var operatorAgentCol int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.columns
		 WHERE table_name = 'campaigns' AND column_name = 'operator_agent'`,
	).Scan(&operatorAgentCol); err != nil {
		t.Fatalf("query campaigns.operator_agent column: %v", err)
	}
	if operatorAgentCol != 1 {
		t.Errorf("campaigns.operator_agent count after MigrateUp = %d, want 1", operatorAgentCol)
	}
	overrideID := uuid.New()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO campaigns (id, repo, epic_ref, state, operator_agent)
		 VALUES ($1, 'r', 'issue:9', 'pending', '{"may_approve":"solo_low"}'::jsonb)`,
		overrideID,
	); err != nil {
		t.Errorf("insert campaign with operator_agent after MigrateUp failed: %v", err)
	}
	var operatorAgentBack string
	if err := pool.QueryRow(context.Background(),
		`SELECT operator_agent::text FROM campaigns WHERE id = $1`, overrideID,
	).Scan(&operatorAgentBack); err != nil {
		t.Fatalf("read back operator_agent: %v", err)
	}
	if !strings.Contains(operatorAgentBack, "may_approve") {
		t.Errorf("operator_agent round-trip = %q, want it to contain may_approve", operatorAgentBack)
	}

	// 0042 (#1455) added the nullable campaigns.idempotency_key TEXT column +
	// the partial unique index over (repo, idempotency_key). Confirm the column
	// exists and the index dedups: two campaigns sharing (repo, key) conflict,
	// while NULL keys never collide.
	var idempotencyKeyCol int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.columns
		 WHERE table_name = 'campaigns' AND column_name = 'idempotency_key'`,
	).Scan(&idempotencyKeyCol); err != nil {
		t.Fatalf("query campaigns.idempotency_key column: %v", err)
	}
	if idempotencyKeyCol != 1 {
		t.Errorf("campaigns.idempotency_key count after MigrateUp = %d, want 1", idempotencyKeyCol)
	}
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO campaigns (id, repo, epic_ref, state, idempotency_key)
		 VALUES ($1, 'idem/r', 'issue:1', 'pending', 'k1')`,
		uuid.New(),
	); err != nil {
		t.Errorf("insert campaign with idempotency_key after MigrateUp failed: %v", err)
	}
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO campaigns (id, repo, epic_ref, state, idempotency_key)
		 VALUES ($1, 'idem/r', 'issue:2', 'pending', 'k1')`,
		uuid.New(),
	); err == nil {
		t.Error("duplicate (repo, idempotency_key) insert succeeded, want unique-index conflict")
	}
	// Two NULL-key campaigns in the same repo do not collide (partial index).
	for i := 0; i < 2; i++ {
		if _, err := pool.Exec(context.Background(),
			`INSERT INTO campaigns (id, repo, epic_ref, state) VALUES ($1, 'idem/r', 'issue:3', 'pending')`,
			uuid.New(),
		); err != nil {
			t.Errorf("NULL-key campaign insert #%d failed (partial index should exclude NULLs): %v", i, err)
		}
	}

	// 0049 (#1551) added the campaign_items.autonomy TEXT column (NOT NULL
	// DEFAULT '') with a fail-closed CHECK admitting only ('','low','medium',
	// 'high'). Confirm the column exists, a valid tier inserts, and a garbage
	// tier is rejected by the CHECK.
	var autonomyCol int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.columns
		 WHERE table_name = 'campaign_items' AND column_name = 'autonomy'`,
	).Scan(&autonomyCol); err != nil {
		t.Fatalf("query campaign_items.autonomy column: %v", err)
	}
	if autonomyCol != 1 {
		t.Errorf("campaign_items.autonomy count after MigrateUp = %d, want 1", autonomyCol)
	}
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO campaign_items (id, campaign_id, issue_ref, state, autonomy)
		 VALUES ($1, $2, 'issue:auto', 'pending', 'low')`,
		uuid.New(), campaignID,
	); err != nil {
		t.Errorf("insert campaign_item with autonomy 'low' after MigrateUp failed: %v", err)
	}
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO campaign_items (id, campaign_id, issue_ref, state, autonomy)
		 VALUES ($1, $2, 'issue:autobad', 'pending', 'bogus')`,
		uuid.New(), campaignID,
	); err == nil {
		t.Error("insert campaign_item with autonomy 'bogus' succeeded, want CHECK rejection (SQLSTATE 23514)")
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

	// MigrateDown rolls back one step. 0045 (#1531) is now the latest
	// migration: it is an additive CHECK widening that admitted the
	// 'acceptance' artifact kind into artifacts_kind_check (no column, no new
	// state). So its rollback narrows artifacts_kind_check back to the 0037 set
	// (plan/pull_request/deployment) and touches nothing else, while 0044's
	// (#1519) stages_type_check 'acceptance' member, 0043's (#1417)
	// runs.upstream_run_id column + partial index, 0042's (#1455)
	// campaigns.idempotency_key column + unique index, 0041's (#1451)
	// operator_agent column, 0040's (#1446) pause_policy + pause_reason
	// columns and widened 'paused' state CHECK now SURVIVE the one-step down
	// (they are prior migrations). 0039's (#1437) campaigns + campaign_items
	// tables themselves likewise still EXIST, as does every earlier
	// migration's effect — notably 0038's (#1400) widened stages_type_check
	// ('deploy'), 0037's (#1385) artifacts_kind_check 'deployment', 0036's
	// (#1346) runs.runner_kind_resolved column, etc.
	var campaignsTable, campaignItemsTable int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.tables WHERE table_name = 'campaigns'`,
	).Scan(&campaignsTable); err != nil {
		t.Fatalf("query campaigns table: %v", err)
	}
	if campaignsTable != 1 {
		t.Errorf("'campaigns' table count after MigrateDown = %d, want 1 (0041 is an ALTER; 0039's table survives)", campaignsTable)
	}
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.tables WHERE table_name = 'campaign_items'`,
	).Scan(&campaignItemsTable); err != nil {
		t.Fatalf("query campaign_items table: %v", err)
	}
	if campaignItemsTable != 1 {
		t.Errorf("'campaign_items' table count after MigrateDown = %d, want 1 (0041 is an ALTER; 0039's table survives)", campaignItemsTable)
	}
	// 0043's added column now SURVIVES the one-step down (only 0044 rolled
	// back) — the binding TestMigrateDown flip for this migration.
	var upstreamRunIDCol int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.columns
		 WHERE table_name = 'runs' AND column_name = 'upstream_run_id'`,
	).Scan(&upstreamRunIDCol); err != nil {
		t.Fatalf("query runs.upstream_run_id column: %v", err)
	}
	if upstreamRunIDCol != 1 {
		t.Errorf("runs.upstream_run_id count after MigrateDown = %d, want 1 (0043 still applied; only 0045 rolled back)", upstreamRunIDCol)
	}
	// 0042's idempotency_key column SURVIVES the one-step down (only 0044
	// rolled back).
	var idempotencyKeyCol int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.columns
		 WHERE table_name = 'campaigns' AND column_name = 'idempotency_key'`,
	).Scan(&idempotencyKeyCol); err != nil {
		t.Fatalf("query campaigns.idempotency_key column: %v", err)
	}
	if idempotencyKeyCol != 1 {
		t.Errorf("campaigns.idempotency_key count after MigrateDown = %d, want 1 (0042 still applied; only 0045 rolled back)", idempotencyKeyCol)
	}
	// 0041's operator_agent column SURVIVES the one-step down (only 0043
	// rolled back).
	var operatorAgentCol int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.columns
		 WHERE table_name = 'campaigns' AND column_name = 'operator_agent'`,
	).Scan(&operatorAgentCol); err != nil {
		t.Fatalf("query campaigns.operator_agent column: %v", err)
	}
	if operatorAgentCol != 1 {
		t.Errorf("campaigns.operator_agent count after MigrateDown = %d, want 1 (0041 still applied; only 0045 rolled back)", operatorAgentCol)
	}
	// 0040's two added columns SURVIVE the one-step down (only 0042 rolled
	// back).
	var pausePolicyCol, pauseReasonCol int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.columns
		 WHERE table_name = 'campaigns' AND column_name = 'pause_policy'`,
	).Scan(&pausePolicyCol); err != nil {
		t.Fatalf("query campaigns.pause_policy column: %v", err)
	}
	if pausePolicyCol != 1 {
		t.Errorf("campaigns.pause_policy count after MigrateDown = %d, want 1 (0040 still applied; only 0045 rolled back)", pausePolicyCol)
	}
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.columns
		 WHERE table_name = 'campaign_items' AND column_name = 'pause_reason'`,
	).Scan(&pauseReasonCol); err != nil {
		t.Fatalf("query campaign_items.pause_reason column: %v", err)
	}
	if pauseReasonCol != 1 {
		t.Errorf("campaign_items.pause_reason count after MigrateDown = %d, want 1 (0040 still applied; only 0045 rolled back)", pauseReasonCol)
	}
	// 0040's widened CHECK survives, so a 'paused' campaign insert now SUCCEEDS
	// after the one-step down (only 0045 rolled back).
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO campaigns (id, repo, epic_ref, state) VALUES ($1, 'r', 'issue:1', 'paused')`,
		uuid.New(),
	); err != nil {
		t.Errorf("insert 'paused' campaign after MigrateDown failed, want success (0040's widened CHECK survives; only 0045 rolled back): %v", err)
	}
	var artifactsKindCheckDef string
	if err := pool.QueryRow(context.Background(),
		`SELECT pg_get_constraintdef(oid) FROM pg_constraint
		 WHERE conname = 'artifacts_kind_check'`,
	).Scan(&artifactsKindCheckDef); err != nil {
		t.Fatalf("query artifacts_kind_check constraint def: %v", err)
	}
	if !strings.Contains(artifactsKindCheckDef, "deployment") {
		t.Errorf("artifacts_kind_check after MigrateDown dropped 'deployment' (0037 still applied; only 0045 rolled back): %s", artifactsKindCheckDef)
	}
	// 0049 (#1551) is now the latest migration: it adds the
	// campaign_items.autonomy column (with its CHECK), touching nothing else. So
	// its one-step rollback drops that column (asserted below) and leaves every
	// prior migration's effect intact — including 0045's (#1531) 'acceptance'
	// artifact-kind widening, which SURVIVES the one-step down (it is no longer
	// the migration rolled back).
	if !strings.Contains(artifactsKindCheckDef, "acceptance") {
		t.Errorf("artifacts_kind_check after MigrateDown dropped 'acceptance' (0045 still applied; only 0049 rolled back): %s", artifactsKindCheckDef)
	}
	// 0046 (#1592) is now a PRIOR migration (only 0049 rolled back), so its
	// refinement_drafts table SURVIVES the one-step down.
	var refinementDraftsTable int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.tables WHERE table_name = 'refinement_drafts'`,
	).Scan(&refinementDraftsTable); err != nil {
		t.Fatalf("query refinement_drafts table: %v", err)
	}
	if refinementDraftsTable != 1 {
		t.Errorf("'refinement_drafts' table count after MigrateDown = %d, want 1 (0046 still applied; only 0049 rolled back)", refinementDraftsTable)
	}
	// 0047 (#1593) is now a PRIOR migration (only 0049 rolled back), so its
	// refinement_decisions table and the refinement_drafts.origin column it added
	// both SURVIVE the one-step down.
	var refinementDecisionsTable int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.tables WHERE table_name = 'refinement_decisions'`,
	).Scan(&refinementDecisionsTable); err != nil {
		t.Fatalf("query refinement_decisions table: %v", err)
	}
	if refinementDecisionsTable != 1 {
		t.Errorf("'refinement_decisions' table count after MigrateDown = %d, want 1 (0047 still applied; only 0049 rolled back)", refinementDecisionsTable)
	}
	var refinementDraftsOriginCol int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.columns
		 WHERE table_name = 'refinement_drafts' AND column_name = 'origin'`,
	).Scan(&refinementDraftsOriginCol); err != nil {
		t.Fatalf("query refinement_drafts.origin column: %v", err)
	}
	if refinementDraftsOriginCol != 1 {
		t.Errorf("refinement_drafts.origin count after MigrateDown = %d, want 1 (0047 still applied; only 0049 rolled back)", refinementDraftsOriginCol)
	}
	// 0048 (#1594) is now a PRIOR migration (only 0049 rolled back), so its two
	// ledger tables — refinement_filing_sessions and refinement_filed_items —
	// both SURVIVE the one-step down.
	var refinementFilingSessionsTable int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.tables WHERE table_name = 'refinement_filing_sessions'`,
	).Scan(&refinementFilingSessionsTable); err != nil {
		t.Fatalf("query refinement_filing_sessions table: %v", err)
	}
	if refinementFilingSessionsTable != 1 {
		t.Errorf("'refinement_filing_sessions' table count after MigrateDown = %d, want 1 (0048 still applied; only 0049 rolled back)", refinementFilingSessionsTable)
	}
	var refinementFiledItemsTable int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.tables WHERE table_name = 'refinement_filed_items'`,
	).Scan(&refinementFiledItemsTable); err != nil {
		t.Fatalf("query refinement_filed_items table: %v", err)
	}
	if refinementFiledItemsTable != 1 {
		t.Errorf("'refinement_filed_items' table count after MigrateDown = %d, want 1 (0048 still applied; only 0049 rolled back)", refinementFiledItemsTable)
	}
	// 0049 (#1551) IS the migration just rolled back, so campaign_items.autonomy
	// must be GONE after the one-step down.
	var autonomyColDown int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.columns
		 WHERE table_name = 'campaign_items' AND column_name = 'autonomy'`,
	).Scan(&autonomyColDown); err != nil {
		t.Fatalf("query campaign_items.autonomy column after down: %v", err)
	}
	if autonomyColDown != 0 {
		t.Errorf("campaign_items.autonomy count after MigrateDown = %d, want 0 (0049 should have rolled it back)", autonomyColDown)
	}
	// 0044 (#1519) is now a PRIOR migration (only 0045 rolled back), so its
	// widening — the 'acceptance' stage type — must STILL be present in
	// stages_type_check, alongside 0038's 'deploy'. 0038's stages_state_check
	// (the two deploy states) is likewise still present; 0045 touched neither.
	var stageTypeCheckDef string
	if err := pool.QueryRow(context.Background(),
		`SELECT pg_get_constraintdef(oid) FROM pg_constraint
		 WHERE conname = 'stages_type_check'`,
	).Scan(&stageTypeCheckDef); err != nil {
		t.Fatalf("query stages_type_check constraint def: %v", err)
	}
	if !strings.Contains(stageTypeCheckDef, "deploy") {
		t.Errorf("stages_type_check after MigrateDown dropped 'deploy' (0038 still applied; only 0045 rolled back): %s", stageTypeCheckDef)
	}
	if !strings.Contains(stageTypeCheckDef, "acceptance") {
		t.Errorf("stages_type_check after MigrateDown dropped 'acceptance' (0044 still applied; only 0045 rolled back): %s", stageTypeCheckDef)
	}
	var runnerKindResolvedCol int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.columns
		 WHERE table_name = 'runs' AND column_name = 'runner_kind_resolved'`,
	).Scan(&runnerKindResolvedCol); err != nil {
		t.Fatalf("query runs.runner_kind_resolved column: %v", err)
	}
	if runnerKindResolvedCol != 1 {
		t.Errorf("runs.runner_kind_resolved count after MigrateDown = %d, want 1 (0036 still applied; only 0045 rolled back)", runnerKindResolvedCol)
	}
	var scopeParkCol int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.columns
		 WHERE table_name = 'stages' AND column_name = 'scope_completeness_park'`,
	).Scan(&scopeParkCol); err != nil {
		t.Fatalf("query stages.scope_completeness_park column: %v", err)
	}
	if scopeParkCol != 1 {
		t.Errorf("stages.scope_completeness_park count after MigrateDown = %d, want 1 (0035 still applied; only 0045 rolled back)", scopeParkCol)
	}
	var sliceIndexCol int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.columns
		 WHERE table_name = 'runs' AND column_name = 'slice_index'`,
	).Scan(&sliceIndexCol); err != nil {
		t.Fatalf("query runs.slice_index column: %v", err)
	}
	if sliceIndexCol != 1 {
		t.Errorf("runs.slice_index count after MigrateDown = %d, want 1 (0034 still applied; only 0045 rolled back)", sliceIndexCol)
	}
	var suggestedPatchCol int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.columns
		 WHERE table_name = 'review_concerns' AND column_name = 'suggested_patch'`,
	).Scan(&suggestedPatchCol); err != nil {
		t.Fatalf("query review_concerns.suggested_patch column: %v", err)
	}
	if suggestedPatchCol != 1 {
		t.Errorf("review_concerns.suggested_patch count after MigrateDown = %d, want 1 (0033 still applied; only 0045 rolled back)", suggestedPatchCol)
	}
	var stageStateCheckDef string
	if err := pool.QueryRow(context.Background(),
		`SELECT pg_get_constraintdef(oid) FROM pg_constraint
		 WHERE conname = 'stages_state_check'`,
	).Scan(&stageStateCheckDef); err != nil {
		t.Fatalf("query stages_state_check constraint def: %v", err)
	}
	// 0038 (#1400) is a PRIOR migration now (only 0045 rolled back), so its
	// widened stages_state_check still admits the two deploy states, and
	// 0035's 'awaiting_scope_decision', 0032's 'awaiting_input' and
	// 'awaiting_children' survive too.
	if !strings.Contains(stageStateCheckDef, "awaiting_deploy_approval") {
		t.Errorf("stages_state_check after MigrateDown dropped 'awaiting_deploy_approval' (0038 still applied; only 0045 rolled back): %s", stageStateCheckDef)
	}
	if !strings.Contains(stageStateCheckDef, "awaiting_deployment") {
		t.Errorf("stages_state_check after MigrateDown dropped 'awaiting_deployment' (0038 still applied; only 0045 rolled back): %s", stageStateCheckDef)
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
// + item, then step DOWN through 0045 (narrow artifacts_kind_check) then 0044
// (narrow stages_type_check) then 0043 (drop upstream_run_id) then 0042
// (drop idempotency_key) then 0041 (drop operator_agent) then 0040 (the
// normalizing rollback under test) — and assert the final step succeeds AND the
// rows were normalized to running. The extra steps are needed because 0045
// (#1531), 0044 (#1519), 0043 (#1417), 0042 (#1455) and 0041 (#1451) now sit
// above 0040, so fewer MigrateDowns would
// only roll back the inert CHECK/column changes and never reach 0040's normalization
// (the campaign tables survive all — 0039 is the table create).
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

	// Step down past 0049 (drop campaign_items.autonomy — inert re: paused rows)
	// then 0048 (drop the refinement filing ledger — inert re: campaigns) then
	// 0047 (drop refinement_decisions + refinement_drafts.origin — inert re:
	// campaigns) then 0046 (drop refinement_drafts — inert re: campaigns) then
	// 0045 (narrow artifacts_kind_check — inert re: campaigns) then 0044 (narrow
	// stages_type_check — inert re: campaigns) then 0043 (drop upstream_run_id —
	// inert) then 0042 (drop idempotency_key — inert) then 0041 (drop
	// operator_agent — inert), all leaving the paused rows untouched, to reach
	// 0040, the normalizing rollback under test.
	if err := postgres.MigrateDown(url); err != nil {
		t.Fatalf("MigrateDown (roll back 0049) failed: %v", err)
	}
	if err := postgres.MigrateDown(url); err != nil {
		t.Fatalf("MigrateDown (roll back 0048) failed: %v", err)
	}
	if err := postgres.MigrateDown(url); err != nil {
		t.Fatalf("MigrateDown (roll back 0047) failed: %v", err)
	}
	if err := postgres.MigrateDown(url); err != nil {
		t.Fatalf("MigrateDown (roll back 0046) failed: %v", err)
	}
	if err := postgres.MigrateDown(url); err != nil {
		t.Fatalf("MigrateDown (roll back 0045) failed: %v", err)
	}
	if err := postgres.MigrateDown(url); err != nil {
		t.Fatalf("MigrateDown (roll back 0044) failed: %v", err)
	}
	if err := postgres.MigrateDown(url); err != nil {
		t.Fatalf("MigrateDown (roll back 0043) failed: %v", err)
	}
	if err := postgres.MigrateDown(url); err != nil {
		t.Fatalf("MigrateDown (roll back 0042) failed: %v", err)
	}
	if err := postgres.MigrateDown(url); err != nil {
		t.Fatalf("MigrateDown (roll back 0041) failed: %v", err)
	}
	// The 0040 down migration must succeed despite the live paused rows.
	if err := postgres.MigrateDown(url); err != nil {
		t.Fatalf("MigrateDown (roll back 0040) with a paused row present failed (normalization missing?): %v", err)
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
