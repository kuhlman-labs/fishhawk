package postgres_test

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
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
	// 0053 (#1912) widened stages_state_check to admit the parked-for-host-
	// dispatch state 'awaiting_host_dispatch' (the #1912 split of the conflated
	// local 'dispatched' state). Confirm the CHECK names it after a full
	// MigrateUp — without this widening a real awaiting_host_dispatch row is
	// uninsertable (SQLSTATE 23514).
	if !strings.Contains(stageStateCheckDef, "awaiting_host_dispatch") {
		t.Errorf("stages_state_check after MigrateUp does not admit 'awaiting_host_dispatch': %s", stageStateCheckDef)
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
	// 'high'). Confirm the column exists, a known tier inserts and reads back,
	// and an out-of-set value is rejected by the CHECK (SQLSTATE 23514).
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
	autonomyItemID := uuid.New()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO campaign_items (id, campaign_id, issue_ref, state, autonomy)
		 VALUES ($1, $2, 'issue:autonomy', 'pending', 'low')`,
		autonomyItemID, campaignID,
	); err != nil {
		t.Errorf("insert campaign_item with autonomy='low' after MigrateUp failed: %v", err)
	}
	var readAutonomy string
	if err := pool.QueryRow(context.Background(),
		`SELECT autonomy FROM campaign_items WHERE id = $1`, autonomyItemID,
	).Scan(&readAutonomy); err != nil {
		t.Fatalf("read autonomy column after MigrateUp: %v", err)
	}
	if readAutonomy != "low" {
		t.Errorf("campaign_items.autonomy read-back = %q, want low", readAutonomy)
	}
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO campaign_items (id, campaign_id, issue_ref, state, autonomy)
		 VALUES ($1, $2, 'issue:bogus', 'pending', 'bogus')`,
		uuid.New(), campaignID,
	); err == nil {
		t.Error("insert campaign_item with autonomy='bogus' succeeded, want CHECK-constraint rejection")
	}

	// 0050 (#1708) added the nullable api_tokens.auth_method (DEFAULT 'static',
	// CHECK IN ('static','oauth')) and provider TEXT columns. Confirm both
	// columns exist, a row inserted without auth_method reads back the 'static'
	// default with a NULL provider, an explicit ('oauth','github') row round-
	// trips, and an out-of-set auth_method is rejected by the fail-closed CHECK
	// (SQLSTATE 23514).
	var authMethodCol, providerCol int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.columns
		 WHERE table_name = 'api_tokens' AND column_name = 'auth_method'`,
	).Scan(&authMethodCol); err != nil {
		t.Fatalf("query api_tokens.auth_method column: %v", err)
	}
	if authMethodCol != 1 {
		t.Errorf("api_tokens.auth_method count after MigrateUp = %d, want 1", authMethodCol)
	}
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.columns
		 WHERE table_name = 'api_tokens' AND column_name = 'provider'`,
	).Scan(&providerCol); err != nil {
		t.Fatalf("query api_tokens.provider column: %v", err)
	}
	if providerCol != 1 {
		t.Errorf("api_tokens.provider count after MigrateUp = %d, want 1", providerCol)
	}
	staticTokenID := uuid.New()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO api_tokens (id, subject, token_hash, scopes)
		 VALUES ($1, 'github:1', 'hash-static', '{}')`,
		staticTokenID,
	); err != nil {
		t.Errorf("insert api_token without auth_method after MigrateUp failed: %v", err)
	}
	var staticMethod string
	var staticProvider *string
	if err := pool.QueryRow(context.Background(),
		`SELECT auth_method, provider FROM api_tokens WHERE id = $1`, staticTokenID,
	).Scan(&staticMethod, &staticProvider); err != nil {
		t.Fatalf("read back api_token auth_method/provider: %v", err)
	}
	if staticMethod != "static" {
		t.Errorf("api_tokens.auth_method default = %q, want static", staticMethod)
	}
	if staticProvider != nil {
		t.Errorf("api_tokens.provider default = %q, want NULL", *staticProvider)
	}
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO api_tokens (id, subject, token_hash, scopes, auth_method, provider)
		 VALUES ($1, 'github:2', 'hash-oauth', '{}', 'oauth', 'github')`,
		uuid.New(),
	); err != nil {
		t.Errorf("insert oauth api_token after MigrateUp failed (widened CHECK?): %v", err)
	}
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO api_tokens (id, subject, token_hash, scopes, auth_method)
		 VALUES ($1, 'github:3', 'hash-bogus', '{}', 'bogus')`,
		uuid.New(),
	); err == nil {
		t.Error("insert api_token with auth_method='bogus' succeeded, want CHECK-constraint rejection")
	}

	// 0052 (#1854, ADR-057 / ADR-058) created the accounts + installations
	// tenancy tables carrying a forge `provider` discriminator at birth. These
	// are behavioral done-means assertions (a comment-only touch cannot pass):
	// both tables exist; provider defaults to 'github'; the CHECK admits
	// 'gitlab' but rejects 'bitbucket' (the additive-provider guarantee);
	// (provider, account_key) is unique; the endpoint columns are forge-neutral
	// (no accounts column matches 'github_%'); and an installation's provider is
	// pinned to its account's by the composite FK.
	var accountsTable, installationsTable int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.tables WHERE table_name = 'accounts'`,
	).Scan(&accountsTable); err != nil {
		t.Fatalf("query accounts table: %v", err)
	}
	if accountsTable != 1 {
		t.Errorf("'accounts' table count after MigrateUp = %d, want 1", accountsTable)
	}
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.tables WHERE table_name = 'installations'`,
	).Scan(&installationsTable); err != nil {
		t.Fatalf("query installations table: %v", err)
	}
	if installationsTable != 1 {
		t.Errorf("'installations' table count after MigrateUp = %d, want 1", installationsTable)
	}
	// Default mode: INSERT omitting provider reads back the shipped 'github'
	// default (asserts the DEFAULT, not just column presence).
	githubAccountID := uuid.New()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO accounts (id, account_key) VALUES ($1, 'acme-corp')`,
		githubAccountID,
	); err != nil {
		t.Errorf("insert account without provider after MigrateUp failed: %v", err)
	}
	var accountProvider, accountGranularity string
	if err := pool.QueryRow(context.Background(),
		`SELECT provider, granularity FROM accounts WHERE id = $1`, githubAccountID,
	).Scan(&accountProvider, &accountGranularity); err != nil {
		t.Fatalf("read back account provider/granularity: %v", err)
	}
	if accountProvider != "github" {
		t.Errorf("accounts.provider default = %q, want github", accountProvider)
	}
	if accountGranularity != "enterprise" {
		t.Errorf("accounts.granularity default = %q, want enterprise", accountGranularity)
	}
	// CHECK fail-closed mode: an out-of-set provider is rejected on accounts.
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO accounts (id, provider, account_key) VALUES ($1, 'bitbucket', 'bb-team')`,
		uuid.New(),
	); err == nil {
		t.Error("insert account with provider='bitbucket' succeeded, want accounts_provider_check rejection")
	}
	// Additive-provider mode: 'gitlab' succeeds (the guarantee #1854 exists to
	// preserve — a narrower CHECK would fail this).
	gitlabAccountID := uuid.New()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO accounts (id, provider, account_key, granularity)
		 VALUES ($1, 'gitlab', 'acme-group', 'group')`,
		gitlabAccountID,
	); err != nil {
		t.Errorf("insert account with provider='gitlab' after MigrateUp failed (narrower CHECK?): %v", err)
	}
	// Uniqueness mode: a duplicate (provider, account_key) is rejected. The
	// same account_key under a DIFFERENT provider does NOT collide (the key is
	// provider-scoped).
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO accounts (id, provider, account_key) VALUES ($1, 'github', 'acme-corp')`,
		uuid.New(),
	); err == nil {
		t.Error("duplicate (github, acme-corp) account insert succeeded, want unique-constraint conflict")
	}
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO accounts (id, provider, account_key, granularity)
		 VALUES ($1, 'gitlab', 'acme-corp', 'group')`,
		uuid.New(),
	); err != nil {
		t.Errorf("insert (gitlab, acme-corp) account failed, want success (account_key is provider-scoped): %v", err)
	}
	// Forge-neutral-naming mode (Amendment A1 relocation, 0055): the endpoint-
	// config columns forge_base_url/oauth_base_url now live on INSTALLATIONS,
	// not accounts — a forge-agnostic workspace spanning a github.com install and
	// a gitlab.com group cannot share one per-account base URL. Assert they are
	// PRESENT on installations and ABSENT from accounts, and that NEITHER table
	// carries a provider-named endpoint column (pins acceptance criterion 2 as a
	// test on both tables).
	var forgeBaseURLOnInstallations, oauthBaseURLOnInstallations int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.columns
		 WHERE table_name = 'installations' AND column_name = 'forge_base_url'`,
	).Scan(&forgeBaseURLOnInstallations); err != nil {
		t.Fatalf("query installations.forge_base_url column: %v", err)
	}
	if forgeBaseURLOnInstallations != 1 {
		t.Errorf("installations.forge_base_url count after MigrateUp = %d, want 1 (Amendment A1 relocation)", forgeBaseURLOnInstallations)
	}
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.columns
		 WHERE table_name = 'installations' AND column_name = 'oauth_base_url'`,
	).Scan(&oauthBaseURLOnInstallations); err != nil {
		t.Fatalf("query installations.oauth_base_url column: %v", err)
	}
	if oauthBaseURLOnInstallations != 1 {
		t.Errorf("installations.oauth_base_url count after MigrateUp = %d, want 1 (Amendment A1 relocation)", oauthBaseURLOnInstallations)
	}
	var forgeBaseURLOnAccounts, oauthBaseURLOnAccounts int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.columns
		 WHERE table_name = 'accounts' AND column_name = 'forge_base_url'`,
	).Scan(&forgeBaseURLOnAccounts); err != nil {
		t.Fatalf("query accounts.forge_base_url column: %v", err)
	}
	if forgeBaseURLOnAccounts != 0 {
		t.Errorf("accounts.forge_base_url count after MigrateUp = %d, want 0 (0055 dropped it from accounts, Amendment A1)", forgeBaseURLOnAccounts)
	}
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.columns
		 WHERE table_name = 'accounts' AND column_name = 'oauth_base_url'`,
	).Scan(&oauthBaseURLOnAccounts); err != nil {
		t.Fatalf("query accounts.oauth_base_url column: %v", err)
	}
	if oauthBaseURLOnAccounts != 0 {
		t.Errorf("accounts.oauth_base_url count after MigrateUp = %d, want 0 (0055 dropped it from accounts, Amendment A1)", oauthBaseURLOnAccounts)
	}
	var githubNamedAccountCols, githubNamedInstallationCols int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.columns
		 WHERE table_name = 'accounts' AND column_name LIKE 'github_%'`,
	).Scan(&githubNamedAccountCols); err != nil {
		t.Fatalf("query accounts github_%% columns: %v", err)
	}
	if githubNamedAccountCols != 0 {
		t.Errorf("accounts has %d column(s) named 'github_%%', want 0 (endpoint columns must be forge-neutral)", githubNamedAccountCols)
	}
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.columns
		 WHERE table_name = 'installations' AND column_name LIKE 'github_%'`,
	).Scan(&githubNamedInstallationCols); err != nil {
		t.Fatalf("query installations github_%% columns: %v", err)
	}
	if githubNamedInstallationCols != 0 {
		t.Errorf("installations has %d column(s) named 'github_%%', want 0 (endpoint columns must be forge-neutral)", githubNamedInstallationCols)
	}
	// An installations row FK'd to the github account inserts and reads back its
	// TEXT credential-scope key.
	installationID := uuid.New()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO installations (id, account_id, installation_ref) VALUES ($1, $2, '4242')`,
		installationID, githubAccountID,
	); err != nil {
		t.Errorf("insert installation FK'd to github account after MigrateUp failed: %v", err)
	}
	var installationProvider, installationRef string
	if err := pool.QueryRow(context.Background(),
		`SELECT provider, installation_ref FROM installations WHERE id = $1`, installationID,
	).Scan(&installationProvider, &installationRef); err != nil {
		t.Fatalf("read back installation provider/ref: %v", err)
	}
	if installationProvider != "github" {
		t.Errorf("installations.provider default = %q, want github", installationProvider)
	}
	if installationRef != "4242" {
		t.Errorf("installations.installation_ref round-trip = %q, want 4242", installationRef)
	}
	// CHECK fail-closed mode (installations): an out-of-set provider is rejected.
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO installations (id, account_id, provider, installation_ref)
		 VALUES ($1, $2, 'bitbucket', 'bb-1')`,
		uuid.New(), githubAccountID,
	); err == nil {
		t.Error("insert installation with provider='bitbucket' succeeded, want installations_provider_check rejection")
	}
	// Provider-coherence mode: an installation whose provider differs from its
	// account's is rejected by the composite FK (the github account has no
	// (id, 'gitlab') row to reference).
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO installations (id, account_id, provider, installation_ref)
		 VALUES ($1, $2, 'gitlab', 'mismatch-1')`,
		uuid.New(), githubAccountID,
	); err == nil {
		t.Error("insert installation whose provider ('gitlab') differs from its account's ('github') succeeded, want composite-FK rejection")
	}
	// An installation matching its account's provider succeeds via the composite FK.
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO installations (id, account_id, provider, installation_ref)
		 VALUES ($1, $2, 'gitlab', 'gl-auth-1')`,
		uuid.New(), gitlabAccountID,
	); err != nil {
		t.Errorf("insert gitlab installation FK'd to gitlab account failed, want success (composite FK matches): %v", err)
	}
	// Uniqueness mode (installations): a duplicate (provider, installation_ref) is
	// rejected by the UNIQUE (provider, installation_ref). The FIRST '4242' row inserted
	// above (github, FK'd to githubAccountID) makes a second (github, '4242') a
	// conflict — this is the exercised failure path for the UNIQUE constraint. The
	// same installation_ref under a DIFFERENT provider does NOT collide (the key is
	// provider-scoped), mirroring the accounts (provider, account_key) pair above.
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO installations (id, account_id, provider, installation_ref)
		 VALUES ($1, $2, 'github', '4242')`,
		uuid.New(), githubAccountID,
	); err == nil {
		t.Error("duplicate (github, 4242) installation insert succeeded, want unique-constraint conflict")
	}
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO installations (id, account_id, provider, installation_ref)
		 VALUES ($1, $2, 'gitlab', '4242')`,
		uuid.New(), gitlabAccountID,
	); err != nil {
		t.Errorf("insert (gitlab, 4242) installation failed, want success (installation_ref is provider-scoped): %v", err)
	}

	// 0054 (#1861, ADR-058 / E45.8) widened runs_runner_kind_check to admit
	// 'gitlab_ci' — the additive, dormant GitLab pipeline dispatch backend. This
	// is a behavioral done-means assertion (a comment-only touch cannot pass): a
	// run row with runner_kind='gitlab_ci' now INSERTs where it previously
	// violated the CHECK (SQLSTATE 23514), while an out-of-set runner_kind is
	// still rejected by the fail-closed CHECK.
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO runs (id, repo, workflow_id, workflow_sha, trigger_source, state, runner_kind)
		 VALUES ($1, 'r', 'feature_change', 'sha', 'cli', 'pending', 'gitlab_ci')`,
		uuid.New(),
	); err != nil {
		t.Errorf("insert runner_kind='gitlab_ci' run after MigrateUp failed (widened CHECK?): %v", err)
	}
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO runs (id, repo, workflow_id, workflow_sha, trigger_source, state, runner_kind)
		 VALUES ($1, 'r', 'feature_change', 'sha', 'cli', 'pending', 'gitlab_pipeline')`,
		uuid.New(),
	); err == nil {
		t.Error("insert run with runner_kind='gitlab_pipeline' succeeded, want runs_runner_kind_check rejection")
	}

	// 0055 (#1825, E44.1, ADR-057 / ADR-058) threads a nullable account_id FK
	// through every root entity and adds the account_members membership table.
	// These are behavioral done-means assertions (a comment-only touch cannot
	// pass): account_id must exist, be NULLABLE, and carry a <t>_account_id_fkey
	// FK to accounts on ALL EIGHT threaded root tables.
	for _, tbl := range []string{
		"runs", "campaigns", "refinement_drafts", "refinement_decisions",
		"refinement_filing_sessions", "refinement_filed_items", "api_tokens", "audit_entries",
	} {
		var isNullable string
		if err := pool.QueryRow(context.Background(),
			`SELECT is_nullable FROM information_schema.columns
			 WHERE table_name = $1 AND column_name = 'account_id'`, tbl,
		).Scan(&isNullable); err != nil {
			t.Fatalf("query %s.account_id column (missing?): %v", tbl, err)
		}
		if isNullable != "YES" {
			t.Errorf("%s.account_id is_nullable after MigrateUp = %q, want YES (nullable throughout E44.1)", tbl, isNullable)
		}
		var fkCount int
		if err := pool.QueryRow(context.Background(),
			`SELECT count(*) FROM information_schema.table_constraints
			 WHERE table_name = $1 AND constraint_name = $2 AND constraint_type = 'FOREIGN KEY'`,
			tbl, tbl+"_account_id_fkey",
		).Scan(&fkCount); err != nil {
			t.Fatalf("query %s_account_id_fkey: %v", tbl, err)
		}
		if fkCount != 1 {
			t.Errorf("%s_account_id_fkey count after MigrateUp = %d, want 1", tbl, fkCount)
		}
	}

	// account_members: the forge-neutral membership table. Assert it exists, its
	// provider CHECK admits 'gitlab' (FK'd to the gitlab account) but rejects
	// 'bitbucket', and its composite FK rejects a member whose (account_id,
	// provider) has no matching account.
	var accountMembersTable int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.tables WHERE table_name = 'account_members'`,
	).Scan(&accountMembersTable); err != nil {
		t.Fatalf("query account_members table: %v", err)
	}
	if accountMembersTable != 1 {
		t.Errorf("'account_members' table count after MigrateUp = %d, want 1", accountMembersTable)
	}
	// Happy path: a member FK'd to the github account inserts (provider defaults
	// to 'github').
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO account_members (id, account_id, member_ref) VALUES ($1, $2, 'octocat')`,
		uuid.New(), githubAccountID,
	); err != nil {
		t.Errorf("insert account_member FK'd to github account after MigrateUp failed: %v", err)
	}
	// CHECK admits 'gitlab' (FK'd to the gitlab account — the additive-provider
	// guarantee; a narrower CHECK would fail this).
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO account_members (id, account_id, provider, member_ref)
		 VALUES ($1, $2, 'gitlab', 'gl-user')`,
		uuid.New(), gitlabAccountID,
	); err != nil {
		t.Errorf("insert account_member with provider='gitlab' FK'd to gitlab account failed (narrower CHECK?): %v", err)
	}
	// CHECK fail-closed: an out-of-set provider is rejected by
	// account_members_provider_check. Assert the specific constraint name, not
	// merely that some error fired — a bitbucket member FK'd to the github
	// account ALSO violates the composite FK (no (id, 'bitbucket') accounts row),
	// so a non-nil error alone would pass even if the provider CHECK were dropped.
	// Pinning the constraint name proves the CHECK, not the FK, is the rejector.
	_, err = pool.Exec(context.Background(),
		`INSERT INTO account_members (id, account_id, provider, member_ref)
		 VALUES ($1, $2, 'bitbucket', 'bb-user')`,
		uuid.New(), githubAccountID,
	)
	var bitbucketErr *pgconn.PgError
	if !errors.As(err, &bitbucketErr) {
		t.Errorf("insert account_member with provider='bitbucket' returned %v, want *pgconn.PgError from account_members_provider_check", err)
	} else if bitbucketErr.ConstraintName != "account_members_provider_check" {
		t.Errorf("insert account_member with provider='bitbucket' rejected by constraint %q, want account_members_provider_check (the CHECK, not the composite FK)", bitbucketErr.ConstraintName)
	}
	// Composite FK fail-closed: a member whose provider ('gitlab') differs from
	// its account's ('github') is rejected — the github account has no
	// (id, 'gitlab') row to reference.
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO account_members (id, account_id, provider, member_ref)
		 VALUES ($1, $2, 'gitlab', 'mismatch-user')`,
		uuid.New(), githubAccountID,
	); err == nil {
		t.Error("insert account_member whose provider ('gitlab') differs from its account's ('github') succeeded, want composite-FK rejection")
	}

	// 0056 (#1827, E44.3, ADR-057 Amendment A2) binds sessions to their
	// admitting account and stands up the admission-model columns. Behavioral
	// done-means assertions: sessions.account_id exists, is NULLABLE, and
	// carries sessions_account_id_fkey; account_members.origin defaults to
	// 'invited', admits 'auto_join', and rejects an out-of-set origin via
	// account_members_origin_check; accounts.auto_join_role exists nullable.
	var sessionsAccountIDNullable string
	if err := pool.QueryRow(context.Background(),
		`SELECT is_nullable FROM information_schema.columns
		 WHERE table_name = 'sessions' AND column_name = 'account_id'`,
	).Scan(&sessionsAccountIDNullable); err != nil {
		t.Fatalf("query sessions.account_id column (missing?): %v", err)
	}
	if sessionsAccountIDNullable != "YES" {
		t.Errorf("sessions.account_id is_nullable after MigrateUp = %q, want YES", sessionsAccountIDNullable)
	}
	var sessionsAccountFK int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.table_constraints
		 WHERE table_name = 'sessions' AND constraint_name = 'sessions_account_id_fkey'
		   AND constraint_type = 'FOREIGN KEY'`,
	).Scan(&sessionsAccountFK); err != nil {
		t.Fatalf("query sessions_account_id_fkey: %v", err)
	}
	if sessionsAccountFK != 1 {
		t.Errorf("sessions_account_id_fkey count after MigrateUp = %d, want 1", sessionsAccountFK)
	}
	// origin defaults to 'invited' (the pre-0056 rows' semantics)...
	var octoOrigin string
	if err := pool.QueryRow(context.Background(),
		`SELECT origin FROM account_members WHERE member_ref = 'octocat'`,
	).Scan(&octoOrigin); err != nil {
		t.Fatalf("read account_members.origin (missing column?): %v", err)
	}
	if octoOrigin != "invited" {
		t.Errorf("account_members.origin default = %q, want invited", octoOrigin)
	}
	// ...admits 'auto_join'...
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO account_members (id, account_id, member_ref, origin, role)
		 VALUES ($1, $2, 'auto-joiner', 'auto_join', 'member')`,
		uuid.New(), githubAccountID,
	); err != nil {
		t.Errorf("insert account_member with origin='auto_join' failed (narrower CHECK?): %v", err)
	}
	// ...and rejects an out-of-set origin via the named CHECK.
	_, err = pool.Exec(context.Background(),
		`INSERT INTO account_members (id, account_id, member_ref, origin)
		 VALUES ($1, $2, 'synced-user', 'synced')`,
		uuid.New(), githubAccountID,
	)
	var originErr *pgconn.PgError
	if !errors.As(err, &originErr) {
		t.Errorf("insert account_member with origin='synced' returned %v, want *pgconn.PgError from account_members_origin_check", err)
	} else if originErr.ConstraintName != "account_members_origin_check" {
		t.Errorf("insert account_member with origin='synced' rejected by constraint %q, want account_members_origin_check", originErr.ConstraintName)
	}
	var autoJoinRoleNullable string
	if err := pool.QueryRow(context.Background(),
		`SELECT is_nullable FROM information_schema.columns
		 WHERE table_name = 'accounts' AND column_name = 'auto_join_role'`,
	).Scan(&autoJoinRoleNullable); err != nil {
		t.Fatalf("query accounts.auto_join_role column (missing?): %v", err)
	}
	if autoJoinRoleNullable != "YES" {
		t.Errorf("accounts.auto_join_role is_nullable after MigrateUp = %q, want YES (NULL = no auto-join policy)", autoJoinRoleNullable)
	}

	// 0057 (#1830, E44.6, ADR-057) enabled + FORCED row-level security with a
	// <table>_tenant_isolation policy on every account-scoped table: the eight
	// 0055 root tables, 0056's sessions, and stages (scoped via its parent
	// run). Done-means shape pin — an empty/no-op migration fails these. The
	// assertions are shape-only by necessity: this test connects as the
	// superuser owner, which bypasses RLS even under FORCE; the behavioral
	// isolation proof under a purpose-created non-superuser NOBYPASSRLS role
	// is rls_test.go.
	for _, tbl := range []string{
		"runs", "campaigns", "refinement_drafts", "refinement_decisions",
		"refinement_filing_sessions", "refinement_filed_items", "api_tokens",
		"audit_entries", "sessions", "stages",
	} {
		var rowSec, forceSec bool
		if err := pool.QueryRow(context.Background(),
			`SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE relname = $1`, tbl,
		).Scan(&rowSec, &forceSec); err != nil {
			t.Fatalf("query %s pg_class RLS flags: %v", tbl, err)
		}
		if !rowSec || !forceSec {
			t.Errorf("%s relrowsecurity=%v relforcerowsecurity=%v after MigrateUp, want true/true (0057 ENABLE + FORCE)", tbl, rowSec, forceSec)
		}
		var polCount int
		if err := pool.QueryRow(context.Background(),
			`SELECT count(*) FROM pg_policies WHERE tablename = $1 AND policyname = $2`,
			tbl, tbl+"_tenant_isolation",
		).Scan(&polCount); err != nil {
			t.Fatalf("query %s_tenant_isolation policy: %v", tbl, err)
		}
		if polCount != 1 {
			t.Errorf("%s_tenant_isolation policy count after MigrateUp = %d, want 1", tbl, polCount)
		}
	}

	// 0058 (#1828, E44.4, ADR-057) added the partial index serving the
	// per-account run-less chain's prev_hash lookup (run_id IS NULL AND
	// account_id = $1 ORDER BY sequence DESC LIMIT 1). Shape pin: the index
	// exists, is partial on run_id IS NULL, and leads on account_id.
	var globalAccountIdxDef string
	if err := pool.QueryRow(context.Background(),
		`SELECT indexdef FROM pg_indexes
		 WHERE tablename = 'audit_entries' AND indexname = 'audit_entries_global_account_seq_idx'`,
	).Scan(&globalAccountIdxDef); err != nil {
		t.Fatalf("query audit_entries_global_account_seq_idx (missing?): %v", err)
	}
	if !strings.Contains(globalAccountIdxDef, "run_id IS NULL") {
		t.Errorf("audit_entries_global_account_seq_idx indexdef = %q, want partial WHERE run_id IS NULL (0058)", globalAccountIdxDef)
	}
	if !strings.Contains(globalAccountIdxDef, "account_id") {
		t.Errorf("audit_entries_global_account_seq_idx indexdef = %q, want account_id key column (0058)", globalAccountIdxDef)
	}

	// 0059 (#2071, E44.10, ADR-057 Amendment A2) created repo_acl_entries, the
	// per-identity forge repo-permission mirror. Shape pin: the table exists,
	// carries the (provider, subject, repo) natural key as a UNIQUE constraint,
	// and is deliberately NOT account-scoped — it mirrors a per-identity forge
	// fact, not tenant data, so it stands outside the 0057 RLS regime. That
	// last assertion is the one a reviewer should be able to challenge: if the
	// table ever gains an account_id it must gain a policy too, and this test
	// is where the choice becomes visible.
	var repoACLTable int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.tables WHERE table_name = 'repo_acl_entries'`,
	).Scan(&repoACLTable); err != nil {
		t.Fatalf("query repo_acl_entries table: %v", err)
	}
	if repoACLTable != 1 {
		t.Errorf("'repo_acl_entries' table count after MigrateUp = %d, want 1 (0059)", repoACLTable)
	}
	var repoACLUnique int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM pg_indexes
		 WHERE tablename = 'repo_acl_entries' AND indexdef LIKE '%UNIQUE%'
		   AND indexdef LIKE '%provider%' AND indexdef LIKE '%subject%' AND indexdef LIKE '%repo%'`,
	).Scan(&repoACLUnique); err != nil {
		t.Fatalf("query repo_acl_entries unique index: %v", err)
	}
	if repoACLUnique != 1 {
		t.Errorf("repo_acl_entries UNIQUE(provider, subject, repo) index count = %d, want 1 (0059)", repoACLUnique)
	}
	var repoACLAccountCol, repoACLRowSec int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.columns
		 WHERE table_name = 'repo_acl_entries' AND column_name = 'account_id'`,
	).Scan(&repoACLAccountCol); err != nil {
		t.Fatalf("query repo_acl_entries.account_id: %v", err)
	}
	if repoACLAccountCol != 0 {
		t.Errorf("repo_acl_entries.account_id count = %d, want 0 — the mirror is deliberately not account-scoped (0059); adding the column requires an RLS policy too", repoACLAccountCol)
	}
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM pg_class WHERE relname = 'repo_acl_entries' AND relrowsecurity`,
	).Scan(&repoACLRowSec); err != nil {
		t.Fatalf("query repo_acl_entries RLS flag: %v", err)
	}
	if repoACLRowSec != 0 {
		t.Errorf("repo_acl_entries relrowsecurity count = %d, want 0 (outside the 0057 RLS regime by design)", repoACLRowSec)
	}

	// 0060 (#2116, E44.25) created repo_acl_purge_watermarks, the per-(provider,
	// subject) purge generation counter that orders a login purge against an
	// in-flight resolution. Shape pin: the table exists, carries the
	// (provider, subject) PRIMARY KEY, has the generation column and the provider
	// CHECK, and — like 0059's repo_acl_entries — is deliberately NOT
	// account-scoped and stands outside the 0057 RLS regime.
	var watermarkTable int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.tables WHERE table_name = 'repo_acl_purge_watermarks'`,
	).Scan(&watermarkTable); err != nil {
		t.Fatalf("query repo_acl_purge_watermarks table: %v", err)
	}
	if watermarkTable != 1 {
		t.Errorf("'repo_acl_purge_watermarks' table count after MigrateUp = %d, want 1 (0060)", watermarkTable)
	}
	var watermarkPK int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM pg_indexes
		 WHERE tablename = 'repo_acl_purge_watermarks' AND indexdef LIKE '%UNIQUE%'
		   AND indexdef LIKE '%provider%' AND indexdef LIKE '%subject%'`,
	).Scan(&watermarkPK); err != nil {
		t.Fatalf("query repo_acl_purge_watermarks PK: %v", err)
	}
	if watermarkPK != 1 {
		t.Errorf("repo_acl_purge_watermarks PRIMARY KEY(provider, subject) index count = %d, want 1 (0060)", watermarkPK)
	}
	var watermarkGenCol int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.columns
		 WHERE table_name = 'repo_acl_purge_watermarks' AND column_name = 'generation'`,
	).Scan(&watermarkGenCol); err != nil {
		t.Fatalf("query repo_acl_purge_watermarks.generation: %v", err)
	}
	if watermarkGenCol != 1 {
		t.Errorf("repo_acl_purge_watermarks.generation column count = %d, want 1 (0060)", watermarkGenCol)
	}
	var watermarkProviderCheck int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.check_constraints
		 WHERE constraint_name = 'repo_acl_purge_watermarks_provider_check'`,
	).Scan(&watermarkProviderCheck); err != nil {
		t.Fatalf("query repo_acl_purge_watermarks provider CHECK: %v", err)
	}
	if watermarkProviderCheck != 1 {
		t.Errorf("repo_acl_purge_watermarks_provider_check count = %d, want 1 (0060)", watermarkProviderCheck)
	}
	var watermarkAccountCol, watermarkRowSec int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.columns
		 WHERE table_name = 'repo_acl_purge_watermarks' AND column_name = 'account_id'`,
	).Scan(&watermarkAccountCol); err != nil {
		t.Fatalf("query repo_acl_purge_watermarks.account_id: %v", err)
	}
	if watermarkAccountCol != 0 {
		t.Errorf("repo_acl_purge_watermarks.account_id count = %d, want 0 — the watermark is deliberately not account-scoped (0060); adding the column requires an RLS policy too", watermarkAccountCol)
	}
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM pg_class WHERE relname = 'repo_acl_purge_watermarks' AND relrowsecurity`,
	).Scan(&watermarkRowSec); err != nil {
		t.Fatalf("query repo_acl_purge_watermarks RLS flag: %v", err)
	}
	if watermarkRowSec != 0 {
		t.Errorf("repo_acl_purge_watermarks relrowsecurity count = %d, want 0 (outside the 0057 RLS regime by design)", watermarkRowSec)
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

	// MigrateDown rolls back one step. 0060 (#2116, E44.25) is now the latest
	// migration: it created repo_acl_purge_watermarks, the per-(provider,
	// subject) purge generation counter. So its one-step rollback DROPs exactly
	// that table — touching nothing else. 0059's (#2071, E44.10, ADR-057
	// Amendment A2) repo_acl_entries mirror table now SURVIVES. 0058's (#1828, E44.4)
	// audit_entries_global_account_seq_idx partial index now SURVIVES, as do 0057's
	// (#1830, E44.6) RLS ENABLE+FORCE and its <table>_tenant_isolation
	// policies now SURVIVE, as do 0056's (#1827, E44.3) sessions.account_id,
	// account_members.origin, and accounts.auto_join_role columns, 0055's
	// (#1825, E44.1) account_members table, its eight root-table
	// account_id columns, and its Amendment A1 endpoint relocation, 0054's
	// (#1861, ADR-058 / E45.8) runs_runner_kind_check 'gitlab_ci'
	// member, 0053's (#1912) stages_state_check 'awaiting_host_dispatch' member,
	// 0052's (#1854, ADR-057 / ADR-058) accounts + installations tenancy tables,
	// and every prior migration's effect: 0051's (#1587) artifacts_kind_check
	// 'release_notes' member, 0050's (#1708) api_tokens.auth_method/provider
	// columns, 0044's (#1519) stages_type_check 'acceptance' member, 0043's
	// (#1417) runs.upstream_run_id column + partial index, 0042's (#1455)
	// campaigns.idempotency_key column + unique index, 0041's (#1451)
	// operator_agent column, and 0040's (#1446) pause_policy + pause_reason
	// columns + widened 'paused' state CHECK. 0039's (#1437) campaigns +
	// campaign_items tables likewise still EXIST, as does every earlier
	// migration's effect — 0038's (#1400) widened stages_type_check ('deploy'),
	// 0037's (#1385) artifacts_kind_check 'deployment', 0036's (#1346)
	// runs.runner_kind_resolved column, etc.
	//
	// This is the binding TestMigrateDown flip for 0060: the
	// repo_acl_purge_watermarks table must be ABSENT, while 0059's
	// repo_acl_entries mirror (now a prior migration) SURVIVES alongside 0058's
	// partial index, 0057's RLS + policies, 0056's three columns, and 0055's
	// account_members table and eight account_id columns.
	var watermarkTableDown int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.tables WHERE table_name = 'repo_acl_purge_watermarks'`,
	).Scan(&watermarkTableDown); err != nil {
		t.Fatalf("query repo_acl_purge_watermarks table: %v", err)
	}
	if watermarkTableDown != 0 {
		t.Errorf("'repo_acl_purge_watermarks' table count after MigrateDown = %d, want 0 (0060 rolled back)", watermarkTableDown)
	}
	var repoACLTableDown int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.tables WHERE table_name = 'repo_acl_entries'`,
	).Scan(&repoACLTableDown); err != nil {
		t.Fatalf("query repo_acl_entries table: %v", err)
	}
	if repoACLTableDown != 1 {
		t.Errorf("'repo_acl_entries' table count after MigrateDown = %d, want 1 (0059 still applied; only 0060 rolled back)", repoACLTableDown)
	}
	var globalAccountIdxDown int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM pg_indexes
		 WHERE tablename = 'audit_entries' AND indexname = 'audit_entries_global_account_seq_idx'`,
	).Scan(&globalAccountIdxDown); err != nil {
		t.Fatalf("query audit_entries_global_account_seq_idx: %v", err)
	}
	if globalAccountIdxDown != 1 {
		t.Errorf("audit_entries_global_account_seq_idx count after MigrateDown = %d, want 1 (0058 still applied; only 0060 rolled back)", globalAccountIdxDown)
	}
	var accountsTableDown, installationsTableDown int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.tables WHERE table_name = 'accounts'`,
	).Scan(&accountsTableDown); err != nil {
		t.Fatalf("query accounts table: %v", err)
	}
	if accountsTableDown != 1 {
		t.Errorf("'accounts' table count after MigrateDown = %d, want 1 (0052 still applied; only 0060 rolled back)", accountsTableDown)
	}
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.tables WHERE table_name = 'installations'`,
	).Scan(&installationsTableDown); err != nil {
		t.Fatalf("query installations table: %v", err)
	}
	if installationsTableDown != 1 {
		t.Errorf("'installations' table count after MigrateDown = %d, want 1 (0052 still applied; only 0060 rolled back)", installationsTableDown)
	}
	// 0057 (now a prior migration) SURVIVES: RLS stays ENABLEd + FORCEd and
	// every <table>_tenant_isolation policy remains on all ten tables.
	for _, tbl := range []string{
		"runs", "campaigns", "refinement_drafts", "refinement_decisions",
		"refinement_filing_sessions", "refinement_filed_items", "api_tokens",
		"audit_entries", "sessions", "stages",
	} {
		var rowSec, forceSec bool
		if err := pool.QueryRow(context.Background(),
			`SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE relname = $1`, tbl,
		).Scan(&rowSec, &forceSec); err != nil {
			t.Fatalf("query %s pg_class RLS flags: %v", tbl, err)
		}
		if !rowSec || !forceSec {
			t.Errorf("%s relrowsecurity=%v relforcerowsecurity=%v after MigrateDown, want true/true (0057 still applied; only 0060 rolled back)", tbl, rowSec, forceSec)
		}
		var polCount int
		if err := pool.QueryRow(context.Background(),
			`SELECT count(*) FROM pg_policies WHERE tablename = $1 AND policyname = $2`,
			tbl, tbl+"_tenant_isolation",
		).Scan(&polCount); err != nil {
			t.Fatalf("query %s policies: %v", tbl, err)
		}
		if polCount != 1 {
			t.Errorf("%s_tenant_isolation policy count after MigrateDown = %d, want 1 (0057 still applied; only 0060 rolled back)", tbl, polCount)
		}
	}
	// 0056 (now a prior migration) SURVIVES: sessions.account_id,
	// account_members.origin, and accounts.auto_join_role remain.
	for _, col := range []struct{ table, column string }{
		{"sessions", "account_id"},
		{"account_members", "origin"},
		{"accounts", "auto_join_role"},
	} {
		var n int
		if err := pool.QueryRow(context.Background(),
			`SELECT count(*) FROM information_schema.columns
			 WHERE table_name = $1 AND column_name = $2`, col.table, col.column,
		).Scan(&n); err != nil {
			t.Fatalf("query %s.%s column: %v", col.table, col.column, err)
		}
		if n != 1 {
			t.Errorf("%s.%s count after MigrateDown = %d, want 1 (0056 still applied; only 0060 rolled back)", col.table, col.column, n)
		}
	}
	// 0055 (now a prior migration) SURVIVES: account_members exists and every
	// one of the eight root tables still carries account_id.
	var accountMembersTableDown int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.tables WHERE table_name = 'account_members'`,
	).Scan(&accountMembersTableDown); err != nil {
		t.Fatalf("query account_members table: %v", err)
	}
	if accountMembersTableDown != 1 {
		t.Errorf("'account_members' table count after MigrateDown = %d, want 1 (0055 still applied; only 0060 rolled back)", accountMembersTableDown)
	}
	for _, tbl := range []string{
		"runs", "campaigns", "refinement_drafts", "refinement_decisions",
		"refinement_filing_sessions", "refinement_filed_items", "api_tokens", "audit_entries",
	} {
		var accountIDCol int
		if err := pool.QueryRow(context.Background(),
			`SELECT count(*) FROM information_schema.columns
			 WHERE table_name = $1 AND column_name = 'account_id'`, tbl,
		).Scan(&accountIDCol); err != nil {
			t.Fatalf("query %s.account_id column: %v", tbl, err)
		}
		if accountIDCol != 1 {
			t.Errorf("%s.account_id count after MigrateDown = %d, want 1 (0055 still applied)", tbl, accountIDCol)
		}
	}
	// 0055's Amendment A1 relocation likewise survives: the endpoint columns
	// stay on installations and stay off accounts.
	var forgeOnAccountsDown, oauthOnAccountsDown, forgeOnInstallationsDown, oauthOnInstallationsDown int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.columns
		 WHERE table_name = 'accounts' AND column_name = 'forge_base_url'`,
	).Scan(&forgeOnAccountsDown); err != nil {
		t.Fatalf("query accounts.forge_base_url column: %v", err)
	}
	if forgeOnAccountsDown != 0 {
		t.Errorf("accounts.forge_base_url count after MigrateDown = %d, want 0 (0055 still applied)", forgeOnAccountsDown)
	}
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.columns
		 WHERE table_name = 'accounts' AND column_name = 'oauth_base_url'`,
	).Scan(&oauthOnAccountsDown); err != nil {
		t.Fatalf("query accounts.oauth_base_url column: %v", err)
	}
	if oauthOnAccountsDown != 0 {
		t.Errorf("accounts.oauth_base_url count after MigrateDown = %d, want 0 (0055 still applied)", oauthOnAccountsDown)
	}
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.columns
		 WHERE table_name = 'installations' AND column_name = 'forge_base_url'`,
	).Scan(&forgeOnInstallationsDown); err != nil {
		t.Fatalf("query installations.forge_base_url column: %v", err)
	}
	if forgeOnInstallationsDown != 1 {
		t.Errorf("installations.forge_base_url count after MigrateDown = %d, want 1 (0055 still applied)", forgeOnInstallationsDown)
	}
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.columns
		 WHERE table_name = 'installations' AND column_name = 'oauth_base_url'`,
	).Scan(&oauthOnInstallationsDown); err != nil {
		t.Fatalf("query installations.oauth_base_url column: %v", err)
	}
	if oauthOnInstallationsDown != 1 {
		t.Errorf("installations.oauth_base_url count after MigrateDown = %d, want 1 (0055 still applied)", oauthOnInstallationsDown)
	}
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
		t.Errorf("artifacts_kind_check after MigrateDown dropped 'deployment' (0037 still applied; only 0052 rolled back): %s", artifactsKindCheckDef)
	}
	// 0051 (#1587) is now a PRIOR migration (only 0052 rolled back), so its
	// additive 'release_notes' artifact-kind widening SURVIVES the one-step down,
	// alongside 0045's (#1531) 'acceptance' member. Before 0052 shipped, 0051
	// was the migration a one-step down rolled back and 'release_notes' had to be
	// GONE here — 0052 flips that assertion.
	if !strings.Contains(artifactsKindCheckDef, "release_notes") {
		t.Errorf("artifacts_kind_check after MigrateDown dropped 'release_notes' (0051 still applied; only 0052 rolled back): %s", artifactsKindCheckDef)
	}
	if !strings.Contains(artifactsKindCheckDef, "acceptance") {
		t.Errorf("artifacts_kind_check after MigrateDown dropped 'acceptance' (0045 still applied; only 0052 rolled back): %s", artifactsKindCheckDef)
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
		t.Errorf("'refinement_drafts' table count after MigrateDown = %d, want 1 (0046 still applied; only 0048 rolled back)", refinementDraftsTable)
	}
	// 0047 (#1593) is now a PRIOR migration (only 0048 rolled back), so its
	// refinement_decisions table and the refinement_drafts.origin column it added
	// both SURVIVE the one-step down.
	var refinementDecisionsTable int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.tables WHERE table_name = 'refinement_decisions'`,
	).Scan(&refinementDecisionsTable); err != nil {
		t.Fatalf("query refinement_decisions table: %v", err)
	}
	if refinementDecisionsTable != 1 {
		t.Errorf("'refinement_decisions' table count after MigrateDown = %d, want 1 (0047 still applied; only 0048 rolled back)", refinementDecisionsTable)
	}
	var refinementDraftsOriginCol int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.columns
		 WHERE table_name = 'refinement_drafts' AND column_name = 'origin'`,
	).Scan(&refinementDraftsOriginCol); err != nil {
		t.Fatalf("query refinement_drafts.origin column: %v", err)
	}
	if refinementDraftsOriginCol != 1 {
		t.Errorf("refinement_drafts.origin count after MigrateDown = %d, want 1 (0047 still applied; only 0048 rolled back)", refinementDraftsOriginCol)
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
	// 0049 (#1551) is now a PRIOR migration (only 0052 rolled back), so its
	// campaign_items.autonomy column SURVIVES the one-step down.
	var autonomyCol int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.columns
		 WHERE table_name = 'campaign_items' AND column_name = 'autonomy'`,
	).Scan(&autonomyCol); err != nil {
		t.Fatalf("query campaign_items.autonomy column: %v", err)
	}
	if autonomyCol != 1 {
		t.Errorf("campaign_items.autonomy count after MigrateDown = %d, want 1 (0049 still applied; only 0052 rolled back)", autonomyCol)
	}
	// 0050 (#1708) is now a PRIOR migration (only 0052 rolled back), so its two
	// added api_tokens columns — auth_method + provider — both SURVIVE the
	// one-step down.
	var authMethodCol, providerCol int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.columns
		 WHERE table_name = 'api_tokens' AND column_name = 'auth_method'`,
	).Scan(&authMethodCol); err != nil {
		t.Fatalf("query api_tokens.auth_method column: %v", err)
	}
	if authMethodCol != 1 {
		t.Errorf("api_tokens.auth_method count after MigrateDown = %d, want 1 (0050 still applied; only 0052 rolled back)", authMethodCol)
	}
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.columns
		 WHERE table_name = 'api_tokens' AND column_name = 'provider'`,
	).Scan(&providerCol); err != nil {
		t.Fatalf("query api_tokens.provider column: %v", err)
	}
	if providerCol != 1 {
		t.Errorf("api_tokens.provider count after MigrateDown = %d, want 1 (0050 still applied; only 0052 rolled back)", providerCol)
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
	// 0053 (#1912) is now a PRIOR migration (only 0054 rolled back), so its
	// widening — the parked-for-host-dispatch state 'awaiting_host_dispatch' —
	// SURVIVES the one-step down. Before 0054 shipped, 0053 was the migration a
	// one-step down rolled back and 'awaiting_host_dispatch' had to be GONE here
	// — 0054 flips that assertion.
	if !strings.Contains(stageStateCheckDef, "awaiting_host_dispatch") {
		t.Errorf("stages_state_check after MigrateDown dropped 'awaiting_host_dispatch' (0053 still applied; only 0054 rolled back): %s", stageStateCheckDef)
	}
	// 0054 (#1861, ADR-058 / E45.8) is now a PRIOR migration (only 0057 rolled
	// back), so its widening — the 'gitlab_ci' runner_kind — SURVIVES the
	// one-step down. Before 0055 shipped, 0054 was the migration a one-step down
	// rolled back and 'gitlab_ci' had to be GONE here — 0055 flips that assertion
	// (the binding TestMigrateDown flip for 0054). This is a behavioral done-means
	// assertion: a run row with runner_kind='gitlab_ci' now INSERTs, while a
	// 'github_actions' run still inserts too.
	var runnerKindCheckDef string
	if err := pool.QueryRow(context.Background(),
		`SELECT pg_get_constraintdef(oid) FROM pg_constraint
		 WHERE conname = 'runs_runner_kind_check'`,
	).Scan(&runnerKindCheckDef); err != nil {
		t.Fatalf("query runs_runner_kind_check constraint def: %v", err)
	}
	if !strings.Contains(runnerKindCheckDef, "gitlab_ci") {
		t.Errorf("runs_runner_kind_check after MigrateDown dropped 'gitlab_ci' (0054 still applied; only 0060 rolled back): %s", runnerKindCheckDef)
	}
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO runs (id, repo, workflow_id, workflow_sha, trigger_source, state, runner_kind)
		 VALUES ($1, 'r', 'feature_change', 'sha', 'cli', 'pending', 'gitlab_ci')`,
		uuid.New(),
	); err != nil {
		t.Errorf("insert runner_kind='gitlab_ci' run after MigrateDown failed, want success (0054 survives; only 0060 rolled back): %v", err)
	}
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO runs (id, repo, workflow_id, workflow_sha, trigger_source, state, runner_kind)
		 VALUES ($1, 'r', 'feature_change', 'sha', 'cli', 'pending', 'github_actions')`,
		uuid.New(),
	); err != nil {
		t.Errorf("insert runner_kind='github_actions' run after MigrateDown failed, want success (0024's kinds survive): %v", err)
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
// (#1531), 0044 (#1519), 0043 (#1417), 0042 (#1455), 0041 (#1451) and now
// 0052 (#1854) sit above 0040, so fewer MigrateDowns would
// only roll back the inert CHECK/column/table changes and never reach 0040's normalization
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

	// Step down past 0060 (drop repo_acl_purge_watermarks — a per-identity purge
	// generation counter, inert re: campaigns) then 0059 (drop repo_acl_entries — a per-identity forge
	// permission cache, inert re: campaigns) then 0058 (drop the audit_entries_global_account_seq_idx
	// partial index — inert re: campaigns) then 0057 (drop the RLS policies + disable RLS — purely
	// declarative, inert re: campaigns) then 0056 (drop sessions.account_id + account_members.origin +
	// accounts.auto_join_role — inert re: campaigns) then 0055 (drop account_members + the eight account_id columns +
	// reverse the endpoint relocation — inert re: campaigns; no paused row holds
	// an account_id) then 0054 (narrow runs_runner_kind_check — inert re:
	// campaigns; no paused row is a run) then 0053 (narrow stages_state_check + reverse the
	// awaiting_host_dispatch backfill — inert re: campaigns; no paused row holds
	// that state) then 0052 (drop the accounts + installations tenancy tables —
	// inert re: campaigns) then 0051 (narrow artifacts_kind_check — inert re:
	// campaigns) then 0050 (drop api_tokens.auth_method + provider — additive
	// columns, inert re: campaigns) then 0049 (drop campaign_items.autonomy —
	// additive column, inert re: the paused rows) then 0048 (drop the refinement
	// filing ledger — inert re: campaigns) then 0047 (drop refinement_decisions +
	// refinement_drafts.origin — inert re: campaigns) then 0046 (drop
	// refinement_drafts — inert re: campaigns) then 0045 (narrow
	// artifacts_kind_check — inert re: campaigns) then 0044 (narrow
	// stages_type_check — inert re: campaigns) then 0043 (drop upstream_run_id —
	// inert) then 0042 (drop idempotency_key — inert) then 0041 (drop
	// operator_agent — inert), all leaving the paused rows untouched, to reach
	// 0040, the normalizing rollback under test.
	if err := postgres.MigrateDown(url); err != nil {
		t.Fatalf("MigrateDown (roll back 0060 (drop repo_acl_purge_watermarks) failed): %v", err)
	}
	if err := postgres.MigrateDown(url); err != nil {
		t.Fatalf("MigrateDown (roll back 0059 (drop repo_acl_entries) failed): %v", err)
	}
	if err := postgres.MigrateDown(url); err != nil {
		t.Fatalf("MigrateDown (roll back 0058) failed: %v", err)
	}
	if err := postgres.MigrateDown(url); err != nil {
		t.Fatalf("MigrateDown (roll back 0057) failed: %v", err)
	}
	if err := postgres.MigrateDown(url); err != nil {
		t.Fatalf("MigrateDown (roll back 0056) failed: %v", err)
	}
	if err := postgres.MigrateDown(url); err != nil {
		t.Fatalf("MigrateDown (roll back 0055) failed: %v", err)
	}
	if err := postgres.MigrateDown(url); err != nil {
		t.Fatalf("MigrateDown (roll back 0054) failed: %v", err)
	}
	if err := postgres.MigrateDown(url); err != nil {
		t.Fatalf("MigrateDown (roll back 0053) failed: %v", err)
	}
	if err := postgres.MigrateDown(url); err != nil {
		t.Fatalf("MigrateDown (roll back 0052) failed: %v", err)
	}
	if err := postgres.MigrateDown(url); err != nil {
		t.Fatalf("MigrateDown (roll back 0051) failed: %v", err)
	}
	if err := postgres.MigrateDown(url); err != nil {
		t.Fatalf("MigrateDown (roll back 0050) failed: %v", err)
	}
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

// TestMigration0053_BackfillsParkedLocalStages is the #1912 backfill
// round-trip guard (plan failure-mode (f)). It seeds three 'dispatched' stage
// rows under the pre-0053 (narrow) CHECK, re-applies 0053's up migration, and
// asserts ONLY the parked-local row (dispatched + started_at NULL + on a
// non-terminal local run) flips to 'awaiting_host_dispatch' — a github_actions
// row and a re-opened row carrying a prior attempt's started_at both stay
// 'dispatched' (the deliberately-conservative skip). The down then reverses the
// flip, restoring the exact pre-split row shape. This is a behavioral done-means
// assertion: a comment-only touch of the migration cannot pass it.
func TestMigration0053_BackfillsParkedLocalStages(t *testing.T) {
	url := startContainer(t)

	// Apply everything, then roll 0060 (repo_acl_purge_watermarks, inert re:
	// stages), 0059 (repo_acl_entries, inert re: stages),
	// 0058 (the run-less-chain partial index,
	// inert re: stages), 0057 (the RLS policies, inert re: stages —
	// this test connects as the superuser owner anyway), 0056
	// (sessions.account_id + origin + auto_join_role, inert re: stages), 0055
	// (account_members + account_id + endpoint relocation, inert re: stages),
	// 0054 (the runner_kind CHECK widening, inert re: stages) and 0053 back so
	// we can seed 'dispatched' rows under the pre-0053 narrow CHECK before
	// re-applying the backfill.
	if err := postgres.MigrateUp(url); err != nil {
		t.Fatalf("MigrateUp: %v", err)
	}
	if err := postgres.MigrateDown(url); err != nil {
		t.Fatalf("MigrateDown (roll back 0060 to reach 0053): %v", err)
	}
	if err := postgres.MigrateDown(url); err != nil {
		t.Fatalf("MigrateDown (roll back 0059 to reach 0053): %v", err)
	}
	if err := postgres.MigrateDown(url); err != nil {
		t.Fatalf("MigrateDown (roll back 0058 to reach 0053): %v", err)
	}
	if err := postgres.MigrateDown(url); err != nil {
		t.Fatalf("MigrateDown (roll back 0057 to reach 0053): %v", err)
	}
	if err := postgres.MigrateDown(url); err != nil {
		t.Fatalf("MigrateDown (roll back 0056 to reach 0053): %v", err)
	}
	if err := postgres.MigrateDown(url); err != nil {
		t.Fatalf("MigrateDown (roll back 0055 to reach 0053): %v", err)
	}
	if err := postgres.MigrateDown(url); err != nil {
		t.Fatalf("MigrateDown (roll back 0054 to reach 0053): %v", err)
	}
	if err := postgres.MigrateDown(url); err != nil {
		t.Fatalf("MigrateDown (roll back 0053 to seed pre-backfill rows): %v", err)
	}

	pool, err := postgres.Connect(context.Background(), url)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer pool.Close()

	// A non-terminal local run and a non-terminal github_actions run.
	localRunID := uuid.New()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO runs (id, repo, workflow_id, workflow_sha, trigger_source, state, runner_kind)
		 VALUES ($1, 'r', 'feature_change', 'sha', 'cli', 'running', 'local')`,
		localRunID,
	); err != nil {
		t.Fatalf("seed local run: %v", err)
	}
	ghRunID := uuid.New()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO runs (id, repo, workflow_id, workflow_sha, trigger_source, state, runner_kind)
		 VALUES ($1, 'r', 'feature_change', 'sha', 'cli', 'running', 'github_actions')`,
		ghRunID,
	); err != nil {
		t.Fatalf("seed github_actions run: %v", err)
	}

	// Parked-local stage: dispatched + started_at NULL on the non-terminal local
	// run — the backfill flips exactly this one.
	parkedStageID := uuid.New()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO stages (id, run_id, sequence, stage_type, executor_kind, executor_ref, state, started_at)
		 VALUES ($1, $2, 0, 'implement', 'agent', 'claude-code', 'dispatched', NULL)`,
		parkedStageID, localRunID,
	); err != nil {
		t.Fatalf("seed parked-local stage: %v", err)
	}
	// github_actions stage: dispatched + started_at NULL — must stay dispatched
	// (the backfill scopes to runner_kind='local').
	ghStageID := uuid.New()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO stages (id, run_id, sequence, stage_type, executor_kind, executor_ref, state, started_at)
		 VALUES ($1, $2, 0, 'implement', 'agent', 'claude-code', 'dispatched', NULL)`,
		ghStageID, ghRunID,
	); err != nil {
		t.Fatalf("seed github_actions stage: %v", err)
	}
	// Re-opened local stage: dispatched but carrying a PRIOR attempt's started_at
	// — conservatively SKIPPED (started_at IS NOT NULL), stays dispatched.
	reopenedStageID := uuid.New()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO stages (id, run_id, sequence, stage_type, executor_kind, executor_ref, state, started_at)
		 VALUES ($1, $2, 1, 'implement', 'agent', 'claude-code', 'dispatched', now())`,
		reopenedStageID, localRunID,
	); err != nil {
		t.Fatalf("seed re-opened local stage: %v", err)
	}
	pool.Close()

	// Re-apply 0053: the CHECK widens and the backfill runs.
	if err := postgres.MigrateUp(url); err != nil {
		t.Fatalf("MigrateUp (re-apply 0053 backfill): %v", err)
	}

	pool2, err := postgres.Connect(context.Background(), url)
	if err != nil {
		t.Fatalf("re-Connect: %v", err)
	}
	defer pool2.Close()

	stageState := func(id uuid.UUID) string {
		t.Helper()
		var s string
		if err := pool2.QueryRow(context.Background(),
			`SELECT state FROM stages WHERE id = $1`, id,
		).Scan(&s); err != nil {
			t.Fatalf("read stage %s state: %v", id, err)
		}
		return s
	}

	if got := stageState(parkedStageID); got != "awaiting_host_dispatch" {
		t.Errorf("parked-local stage state after backfill = %q, want awaiting_host_dispatch", got)
	}
	if got := stageState(ghStageID); got != "dispatched" {
		t.Errorf("github_actions stage state after backfill = %q, want dispatched (untouched)", got)
	}
	if got := stageState(reopenedStageID); got != "dispatched" {
		t.Errorf("re-opened local stage (started_at set) state after backfill = %q, want dispatched (conservatively skipped)", got)
	}

	// Down reverses the backfill: roll back 0060 (repo_acl_purge_watermarks,
	// inert re: stages) then 0059 (repo_acl_entries, inert re:
	// stages) then 0058 (the run-less-chain partial
	// index, inert re: stages) then 0057 (the RLS policies, inert re:
	// stages) then 0056 (sessions.account_id + origin + auto_join_role, inert
	// re: stages) then 0055 (account_members + account_id + endpoint
	// relocation, inert re: stages) then 0054 (the runner_kind CHECK widening,
	// inert re: stages) then 0053, and the flipped row returns to dispatched.
	if err := postgres.MigrateDown(url); err != nil {
		t.Fatalf("MigrateDown (roll back 0060 to reach 0053): %v", err)
	}
	if err := postgres.MigrateDown(url); err != nil {
		t.Fatalf("MigrateDown (roll back 0059 to reach 0053): %v", err)
	}
	if err := postgres.MigrateDown(url); err != nil {
		t.Fatalf("MigrateDown (roll back 0058 to reach 0053): %v", err)
	}
	if err := postgres.MigrateDown(url); err != nil {
		t.Fatalf("MigrateDown (roll back 0057 to reach 0053): %v", err)
	}
	if err := postgres.MigrateDown(url); err != nil {
		t.Fatalf("MigrateDown (roll back 0056 to reach 0053): %v", err)
	}
	if err := postgres.MigrateDown(url); err != nil {
		t.Fatalf("MigrateDown (roll back 0055 to reach 0053): %v", err)
	}
	if err := postgres.MigrateDown(url); err != nil {
		t.Fatalf("MigrateDown (roll back 0054 to reach 0053): %v", err)
	}
	if err := postgres.MigrateDown(url); err != nil {
		t.Fatalf("MigrateDown (reverse 0053 backfill): %v", err)
	}
	if got := stageState(parkedStageID); got != "dispatched" {
		t.Errorf("parked-local stage state after down = %q, want dispatched (backfill reversed)", got)
	}
}

// TestMigration0055_BackfillsRunsAccountID is the #1825 backfill round-trip
// guard. 0055's up migration ends with an UPDATE that associates pre-existing
// runs with their account via the installations mapping:
//
//	UPDATE runs SET account_id = i.account_id FROM installations i
//	 WHERE runs.installation_id IS NOT NULL
//	   AND i.installation_ref = runs.installation_id::text;
//
// The schema-shape/provider/rollback assertions in TestMigrateUp/TestMigrateDown
// all pass even if this UPDATE (or its ::text cast) is removed — they never seed
// a run+installation pair, so the backfill silently updates nothing and the
// column-level assertions can't tell. This test seeds the pair BEFORE 0055 (at
// 0054) and asserts the UPDATE actually fires: a run whose installation_id::text
// matches an installation_ref gets that installation's account_id, while a run
// with NULL installation_id stays NULL (the `installation_id IS NOT NULL`
// guard). Removing the UPDATE, dropping the ::text cast, or breaking the join
// makes this FAIL. Follows the partial-migrate + seed + migrate + assert shape
// of TestMigration0053_BackfillsParkedLocalStages, seeding via raw SQL against
// the migrated-to-0054 DB (not the sqlc package).
func TestMigration0055_BackfillsRunsAccountID(t *testing.T) {
	url := startContainer(t)

	// Apply everything, then roll 0060, 0059, 0058, 0057, 0056 and 0055 back so we can seed a
	// run+installation pair under the pre-0055 schema (accounts + installations
	// exist at 0052; runs.installation_id exists at 0005; runs.account_id does
	// NOT yet exist).
	if err := postgres.MigrateUp(url); err != nil {
		t.Fatalf("MigrateUp: %v", err)
	}
	if err := postgres.MigrateDown(url); err != nil {
		t.Fatalf("MigrateDown (roll back 0060 to reach 0054): %v", err)
	}
	if err := postgres.MigrateDown(url); err != nil {
		t.Fatalf("MigrateDown (roll back 0059 to reach 0054): %v", err)
	}
	if err := postgres.MigrateDown(url); err != nil {
		t.Fatalf("MigrateDown (roll back 0058 to reach 0054): %v", err)
	}
	if err := postgres.MigrateDown(url); err != nil {
		t.Fatalf("MigrateDown (roll back 0057 to reach 0054): %v", err)
	}
	if err := postgres.MigrateDown(url); err != nil {
		t.Fatalf("MigrateDown (roll back 0056 to reach 0054): %v", err)
	}
	if err := postgres.MigrateDown(url); err != nil {
		t.Fatalf("MigrateDown (roll back 0055 to reach 0054): %v", err)
	}

	pool, err := postgres.Connect(context.Background(), url)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer pool.Close()

	// An account and an installation whose installation_ref is the string form of
	// a BIGINT installation id — so installation_id::text = installation_ref joins.
	const installationBigint = int64(987654321)
	accountID := uuid.New()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO accounts (id, account_key) VALUES ($1, 'backfill-acct')`,
		accountID,
	); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO installations (id, account_id, installation_ref) VALUES ($1, $2, $3)`,
		uuid.New(), accountID, strconv.FormatInt(installationBigint, 10),
	); err != nil {
		t.Fatalf("seed installation: %v", err)
	}

	// A run whose installation_id (BIGINT) equals the installation's ref — the
	// backfill must associate it with the account.
	matchedRunID := uuid.New()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO runs (id, repo, workflow_id, workflow_sha, trigger_source, state, runner_kind, installation_id)
		 VALUES ($1, 'r', 'feature_change', 'sha', 'cli', 'running', 'github_actions', $2)`,
		matchedRunID, installationBigint,
	); err != nil {
		t.Fatalf("seed run with installation_id: %v", err)
	}
	// A run with NULL installation_id — the `installation_id IS NOT NULL` guard
	// must leave its account_id NULL.
	nullRunID := uuid.New()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO runs (id, repo, workflow_id, workflow_sha, trigger_source, state, runner_kind, installation_id)
		 VALUES ($1, 'r', 'feature_change', 'sha', 'cli', 'running', 'local', NULL)`,
		nullRunID,
	); err != nil {
		t.Fatalf("seed run with NULL installation_id: %v", err)
	}
	pool.Close()

	// Re-apply 0055: adds runs.account_id then runs the backfill UPDATE.
	if err := postgres.MigrateUp(url); err != nil {
		t.Fatalf("MigrateUp (re-apply 0055 backfill): %v", err)
	}

	pool2, err := postgres.Connect(context.Background(), url)
	if err != nil {
		t.Fatalf("re-Connect: %v", err)
	}
	defer pool2.Close()

	runAccountID := func(id uuid.UUID) *uuid.UUID {
		t.Helper()
		var acct *uuid.UUID
		if err := pool2.QueryRow(context.Background(),
			`SELECT account_id FROM runs WHERE id = $1`, id,
		).Scan(&acct); err != nil {
			t.Fatalf("read run %s account_id: %v", id, err)
		}
		return acct
	}

	if got := runAccountID(matchedRunID); got == nil {
		t.Error("matched run account_id after backfill = NULL, want the seeded account (backfill UPDATE did not fire)")
	} else if *got != accountID {
		t.Errorf("matched run account_id after backfill = %s, want %s", *got, accountID)
	}
	if got := runAccountID(nullRunID); got != nil {
		t.Errorf("NULL-installation run account_id after backfill = %s, want NULL (installation_id IS NOT NULL guard)", *got)
	}
}

func TestMigrateDown_MalformedURL(t *testing.T) {
	if err := postgres.MigrateDown("not-a-url"); err == nil {
		t.Fatal("expected error on malformed URL")
	}
}
