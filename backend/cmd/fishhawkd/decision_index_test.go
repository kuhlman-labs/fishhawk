package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/postgres"
)

// TestDecisionIndexCommand_RequiresDB: with neither --db nor the env var the
// command refuses with a usage exit naming BOTH.
func TestDecisionIndexCommand_RequiresDB(t *testing.T) {
	t.Setenv("FISHHAWKD_DATABASE_URL", "")
	for _, mode := range []string{"backfill", "check"} {
		var out strings.Builder
		if got := run([]string{"decision-index", mode}, &out); got != exitUsage {
			t.Errorf("%s exit = %d, want %d", mode, got, exitUsage)
		}
		for _, want := range []string{"--db", "FISHHAWKD_DATABASE_URL"} {
			if !strings.Contains(out.String(), want) {
				t.Errorf("%s output missing %q: %s", mode, want, out.String())
			}
		}
	}
}

// TestDecisionIndexCommand_UnknownMode: an unknown or missing mode refuses
// with a usage exit naming the allowed modes.
func TestDecisionIndexCommand_UnknownMode(t *testing.T) {
	for _, args := range [][]string{{"decision-index", "rebuild"}, {"decision-index"}} {
		var out strings.Builder
		if got := run(args, &out); got != exitUsage {
			t.Errorf("%v exit = %d, want %d", args, got, exitUsage)
		}
		for _, want := range []string{"backfill", "check", "Usage: fishhawkd decision-index"} {
			if !strings.Contains(out.String(), want) {
				t.Errorf("%v output missing %q: %s", args, want, out.String())
			}
		}
	}
}

func TestDecisionIndexCommand_BadFlag(t *testing.T) {
	var out strings.Builder
	if got := run([]string{"decision-index", "check", "--no-such-flag"}, &out); got != exitUsage {
		t.Errorf("exit = %d, want %d", got, exitUsage)
	}
}

// TestDecisionIndexCommand_ConnectFailure: an unreachable database is a
// failure exit, not a usage one.
func TestDecisionIndexCommand_ConnectFailure(t *testing.T) {
	var out strings.Builder
	got := run([]string{"decision-index", "backfill", "--db", "postgres://x:y@127.0.0.1:1/db?connect_timeout=2"}, &out)
	if got != exitFailure {
		t.Errorf("exit = %d, want %d: %s", got, exitFailure, out.String())
	}
	if !strings.Contains(out.String(), "connect failed") {
		t.Errorf("output missing connect failure: %s", out.String())
	}
}

// seedDecisionChain seeds one run with a plan stage and appends n
// approval_submitted entries through the real audit repository.
func seedDecisionChain(t *testing.T, dbURL string, n int) (*pgxpool.Pool, uuid.UUID, []int64) {
	t.Helper()
	ctx := context.Background()
	pool, err := postgres.Connect(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	runID, stageID := uuid.New(), uuid.New()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO runs (id, repo, workflow_id, workflow_sha, trigger_source, state, runner_kind)
		  VALUES ($1, 'acme/widgets', 'feature_change', 'sha-1', 'cli', 'pending', 'local')`, []any{runID}},
		{`INSERT INTO stages (id, run_id, sequence, stage_type, executor_kind, executor_ref, state)
		  VALUES ($1, $2, 0, 'plan', 'agent', 'claude-code', 'pending')`, []any{stageID, runID}},
	} {
		if _, err := pool.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	repo := audit.NewPostgresRepository(pool)
	seqs := make([]int64, 0, n)
	for i := 0; i < n; i++ {
		payload, _ := json.Marshal(map[string]any{"decision": "reject", "reject_class": "verification", "rejection_comment": "no test"})
		e, err := repo.AppendChained(ctx, audit.ChainAppendParams{
			RunID: runID, StageID: &stageID, Timestamp: time.Now().UTC(),
			Category: "approval_submitted", Payload: payload,
		})
		if err != nil {
			t.Fatal(err)
		}
		seqs = append(seqs, e.Sequence)
	}
	return pool, runID, seqs
}

// TestDecisionIndexCommand_BackfillEndToEnd drives the real run() dispatch
// against a migrated database and reads decision_index back, so a wiring or
// column typo that passes both unit halves fails here. The check that follows
// exits 0 because the backfill closed every gap.
func TestDecisionIndexCommand_BackfillEndToEnd(t *testing.T) {
	dbURL := pgtest.NewURL(t)
	pool, runID, seqs := seedDecisionChain(t, dbURL, 2)

	var out strings.Builder
	if got := run([]string{"decision-index", "backfill", "--db", dbURL}, &out); got != exitOK {
		t.Fatalf("backfill exit = %d, want 0: %s", got, out.String())
	}
	if !strings.Contains(out.String(), `"rows_written":2`) {
		t.Errorf("backfill summary missing rows_written=2: %s", out.String())
	}
	rows, err := pool.Query(context.Background(),
		`SELECT source_sequence, run_id, repo, doctrine_version, decision_class, stage_kind, outcome, reject_class, reason_key
		 FROM decision_index ORDER BY source_sequence`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var (
			seq                                                    int64
			rid                                                    uuid.UUID
			repo, doctrine, class, kind, outcome, rejClass, reason string
		)
		if err := rows.Scan(&seq, &rid, &repo, &doctrine, &class, &kind, &outcome, &rejClass, &reason); err != nil {
			t.Fatal(err)
		}
		if rid != runID {
			t.Errorf("row %d run_id = %s, want %s", seq, rid, runID)
		}
		got = append(got, fmt.Sprintf("%d|%s|%s|%s|%s|%s|%s|%s", seq, repo, doctrine, class, kind, outcome, rejClass, reason))
	}
	var want []string
	for _, s := range seqs {
		want = append(want, fmt.Sprintf("%d|acme/widgets|sha-1|plan_approval|plan|reject|verification|rejection_comment", s))
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("decision_index rows:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	out.Reset()
	if got := run([]string{"decision-index", "check", "--db", dbURL}, &out); got != exitOK {
		t.Errorf("check after backfill exit = %d, want 0: %s", got, out.String())
	}
}

// TestDecisionIndexCommand_CheckExitsNonZeroOnGaps: an unindexed decision
// entry makes check exit 1 and list the gap, bounded by --limit while the
// count stays whole.
func TestDecisionIndexCommand_CheckExitsNonZeroOnGaps(t *testing.T) {
	dbURL := pgtest.NewURL(t)
	_, _, seqs := seedDecisionChain(t, dbURL, 2)

	var out strings.Builder
	if got := run([]string{"decision-index", "check", "--db", dbURL, "--limit", "1"}, &out); got != exitFailure {
		t.Fatalf("check exit = %d, want %d: %s", got, exitFailure, out.String())
	}
	for _, want := range []string{"gaps=2", fmt.Sprintf("gap sequence=%d category=approval_submitted", seqs[0]), "FAIL"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("check output missing %q: %s", want, out.String())
		}
	}
	if strings.Contains(out.String(), fmt.Sprintf("gap sequence=%d", seqs[1])) {
		t.Errorf("check listed past --limit 1: %s", out.String())
	}
}

// TestDecisionIndexCommand_CheckOrphansDoNotFail pins #3730 approval condition
// 6 at the CLI: an entry whose run row is absent is reported as orphaned and
// check still exits 0. The orphan is seeded by construction, since
// audit_entries.run_id is RESTRICT: one transaction drops the run_id FK,
// inserts the orphan and re-adds the FK NOT VALID, so the orphan survives and
// later inserts are checked again. That needs table ownership, not superuser
// (#4050): the host path's superuser has it, and so does the container gate
// role, which owns the template-cloned tables.
func TestDecisionIndexCommand_CheckOrphansDoNotFail(t *testing.T) {
	dbURL := pgtest.NewURL(t)
	ctx := context.Background()
	pool, err := postgres.Connect(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// A failed seed must release the pooled connection, or pool.Close blocks
	// forever (#4050).
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `ALTER TABLE audit_entries DROP CONSTRAINT audit_entries_run_id_fkey`); err != nil {
		t.Fatalf("drop run_id FK: %v", err)
	}
	var seq int64
	if err := tx.QueryRow(ctx, `INSERT INTO audit_entries (id, run_id, category, payload, entry_hash)
		VALUES ($1, $2, 'merge_verdict_recorded', '{}', 'h') RETURNING sequence`, uuid.New(), uuid.New()).Scan(&seq); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `ALTER TABLE audit_entries ADD CONSTRAINT audit_entries_run_id_fkey
		FOREIGN KEY (run_id) REFERENCES runs (id) ON DELETE RESTRICT NOT VALID`); err != nil {
		t.Fatalf("restore run_id FK: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	var out strings.Builder
	if got := run([]string{"decision-index", "check", "--db", dbURL}, &out); got != exitOK {
		t.Fatalf("check exit = %d, want 0 (orphans are not gaps): %s", got, out.String())
	}
	for _, want := range []string{"gaps=0", "orphaned_entries=1", fmt.Sprintf("sequences=[%d]", seq)} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("check output missing %q: %s", want, out.String())
		}
	}
}
