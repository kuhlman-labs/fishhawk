package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/decisionindex"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/postgres"
)

// precedent_tuning_test.go drives the E75.5 / #3733 tuning command. The fake
// loader below returns a genuinely-firing history REGARDLESS of its arguments,
// so each fail-closed guard is the only thing standing between a malformed
// invocation and an exit-0 report: delete one and its case goes green-exit,
// which the case reds on.

var ptBase = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

// ptRows: six concern waives, then one contrary defer, one day apart.
func ptRows() []decisionindex.Row {
	var out []decisionindex.Row
	for i := 0; i < 7; i++ {
		class, outcome := decisionindex.ClassConcernWaive, "waived"
		if i == 6 {
			class, outcome = decisionindex.ClassConcernDefer, "deferred"
		}
		out = append(out, decisionindex.Row{
			SourceSequence: int64(i + 1), SourceEntryHash: "h", Repo: "acme/widgets",
			DoctrineVersion: "sha-1", DecisionClass: class, StageKind: "implement", Outcome: outcome,
			TouchedPaths: []string{}, EscalationKeys: []string{},
			DecidedAt: ptBase.Add(time.Duration(i) * 24 * time.Hour),
		})
	}
	return out
}

type ptLoaderCall struct {
	dbURL, repo string
	classes     []decisionindex.DecisionClass
	limit       int
}

func ptLoader(calls *[]ptLoaderCall, rows []decisionindex.Row, err error) tuningRowLoader {
	return func(_ context.Context, dbURL, repo string, classes []decisionindex.DecisionClass, limit int) ([]decisionindex.Row, error) {
		*calls = append(*calls, ptLoaderCall{dbURL, repo, classes, limit})
		return rows, err
	}
}

func ptRun(t *testing.T, args []string, rows []decisionindex.Row, loadErr error) (code int, stdout, stderr string, calls []ptLoaderCall) {
	t.Helper()
	var out, log strings.Builder
	code = runPrecedentTuningWith(args, &out, &log, ptLoader(&calls, rows, loadErr))
	return code, out.String(), log.String(), calls
}

// TestPrecedentTuning_GoldenReport pins the whole stdout for a seeded grid:
// one line per candidate, quietest first, ties in grid order.
func TestPrecedentTuning_GoldenReport(t *testing.T) {
	code, stdout, stderr, calls := ptRun(t, []string{
		"--db", "postgres://fake", "--repo", "acme/widgets",
		"--min-decisions", "5,7", "--min-agreement", "0.8", "--window", "30d,2h",
		"--limit", "40",
	}, ptRows(), nil)
	if code != exitOK {
		t.Fatalf("exit = %d, want 0; stderr: %s", code, stderr)
	}
	want := strings.Join([]string{
		"precedent-tuning repo=acme/widgets rows_read=7 candidates=4",
		"min_decisions=5 min_agreement=0.80 window=2h0m0s examined=7 fired=0 fire_rate=0.0000 below_min_decisions=5 outside_window=2 doctrine_version_mismatch=0 below_min_agreement=0 agreed_with_precedent=0",
		"min_decisions=7 min_agreement=0.80 window=30d examined=7 fired=0 fire_rate=0.0000 below_min_decisions=7 outside_window=0 doctrine_version_mismatch=0 below_min_agreement=0 agreed_with_precedent=0",
		"min_decisions=7 min_agreement=0.80 window=2h0m0s examined=7 fired=0 fire_rate=0.0000 below_min_decisions=7 outside_window=0 doctrine_version_mismatch=0 below_min_agreement=0 agreed_with_precedent=0",
		"min_decisions=5 min_agreement=0.80 window=30d examined=7 fired=1 fire_rate=0.1429 below_min_decisions=5 outside_window=0 doctrine_version_mismatch=0 below_min_agreement=0 agreed_with_precedent=1",
		"",
	}, "\n")
	if stdout != want {
		t.Fatalf("report:\n%s\nwant:\n%s", stdout, want)
	}
	if len(calls) != 1 {
		t.Fatalf("loader calls = %d, want 1", len(calls))
	}
	c := calls[0]
	wantClasses := []decisionindex.DecisionClass{
		decisionindex.ClassConcernDefer, decisionindex.ClassConcernWaive, decisionindex.ClassPlanApproval,
	}
	if c.dbURL != "postgres://fake" || c.repo != "acme/widgets" || c.limit != 40 || !equalClasses(c.classes, wantClasses) {
		t.Fatalf("loader call = %+v, want db/repo/limit passed through and classes %v", c, wantClasses)
	}
}

// TestPrecedentTuning_ClassesNarrowTheRead: --classes concern_waive still reads
// defers (they are a waive's precedent) but no plan approvals.
func TestPrecedentTuning_ClassesNarrowTheRead(t *testing.T) {
	code, _, stderr, calls := ptRun(t, []string{
		"--db", "postgres://fake", "--repo", "acme/widgets", "--classes", "concern_waive",
	}, ptRows(), nil)
	if code != exitOK {
		t.Fatalf("exit = %d; stderr: %s", code, stderr)
	}
	want := []decisionindex.DecisionClass{decisionindex.ClassConcernDefer, decisionindex.ClassConcernWaive}
	if len(calls) != 1 || !equalClasses(calls[0].classes, want) {
		t.Fatalf("loader calls = %+v, want classes %v", calls, want)
	}
}

func equalClasses(a, b []decisionindex.DecisionClass) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestPrecedentTuning_FailsClosed: one case per refusal. Each asserts a
// non-zero exit, the NAMED message on stderr, an empty stdout (no vacuous
// report) and — for every pre-read refusal — that the loader was never called.
func TestPrecedentTuning_FailsClosed(t *testing.T) {
	base := func(extra ...string) []string {
		return append([]string{"--db", "postgres://fake", "--repo", "acme/widgets"}, extra...)
	}
	cases := []struct {
		name      string
		args      []string
		rows      []decisionindex.Row
		loadErr   error
		wantCode  int
		wantMsg   string
		wantLoads int
	}{
		{"missing repo", []string{"--db", "postgres://fake"}, ptRows(), nil, exitUsage, "--repo is required", 0},
		{"blank repo", []string{"--db", "postgres://fake", "--repo", "  "}, ptRows(), nil, exitUsage, "--repo is required", 0},
		{"unparseable min-agreement", base("--min-agreement", "0.8,high"), ptRows(), nil, exitUsage, `--min-agreement: cannot parse "high"`, 0},
		{"unparseable min-decisions", base("--min-decisions", "five"), ptRows(), nil, exitUsage, `--min-decisions: cannot parse "five"`, 0},
		{"unparseable window", base("--window", "forever"), ptRows(), nil, exitUsage, `--window: cannot parse "forever"`, 0},
		{"empty grid", base("--min-decisions", ""), ptRows(), nil, exitUsage, "empty grid: --min-decisions names no value", 0},
		{"empty element", base("--min-agreement", "0.8,,0.9"), ptRows(), nil, exitUsage, `--min-agreement "0.8,,0.9" has an empty element`, 0},
		{"out-of-range agreement", base("--min-agreement", "1.5"), ptRows(), nil, exitUsage, "min agreement must be in (0, 1]", 0},
		{"out-of-range N", base("--min-decisions", "0"), ptRows(), nil, exitUsage, "min decisions must be >= 1", 0},
		{"unknown class", base("--classes", "merge_verdict"), ptRows(), nil, exitUsage, "not in the closed allow-list", 0},
		{"negative limit", base("--limit", "-1"), ptRows(), nil, exitUsage, "--limit must be >= 0", 0},
		{"stray argument", base("extra"), ptRows(), nil, exitUsage, `unexpected argument "extra"`, 0},
		{"bad flag", base("--no-such-flag"), ptRows(), nil, exitUsage, "flag provided but not defined", 0},
		{"no rows read", base(), nil, nil, exitFailure, `no decision-index rows for repo "acme/widgets"`, 1},
		// Rows AND an error: the zero-rows refusal cannot stand in for this one.
		{"read failure", base(), ptRows(), errors.New("boom"), exitFailure, "read decision index failed", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, stdout, stderr, calls := ptRun(t, tc.args, tc.rows, tc.loadErr)
			if code != tc.wantCode {
				t.Fatalf("exit = %d, want %d; stdout: %s; stderr: %s", code, tc.wantCode, stdout, stderr)
			}
			if !strings.Contains(stderr, tc.wantMsg) {
				t.Fatalf("stderr missing %q: %s", tc.wantMsg, stderr)
			}
			if stdout != "" {
				t.Fatalf("a refused invocation printed a report: %s", stdout)
			}
			if len(calls) != tc.wantLoads {
				t.Fatalf("loader calls = %d, want %d", len(calls), tc.wantLoads)
			}
		})
	}
}

// TestPrecedentTuning_RequiresDB: with neither --db nor the env var the
// command refuses before reading.
func TestPrecedentTuning_RequiresDB(t *testing.T) {
	t.Setenv("FISHHAWKD_DATABASE_URL", "")
	code, _, stderr, calls := ptRun(t, []string{"--repo", "acme/widgets"}, ptRows(), nil)
	if code != exitUsage || !strings.Contains(stderr, "FISHHAWKD_DATABASE_URL") || len(calls) != 0 {
		t.Fatalf("exit = %d, calls = %d, stderr: %s", code, len(calls), stderr)
	}
}

// TestPrecedentTuning_DispatchedFromRun: the subcommand is reachable through
// main's dispatch (a refusal is enough to prove routing without a database).
func TestPrecedentTuning_DispatchedFromRun(t *testing.T) {
	var out strings.Builder
	if got := run([]string{"precedent-tuning"}, &out); got != exitUsage {
		t.Fatalf("exit = %d, want %d: %s", got, exitUsage, out.String())
	}
	if !strings.Contains(out.String(), "Usage: fishhawkd precedent-tuning") {
		t.Fatalf("output missing usage: %s", out.String())
	}
	out.Reset()
	printUsage(&out)
	if !strings.Contains(out.String(), "precedent-tuning") {
		t.Fatalf("top-level usage does not list precedent-tuning: %s", out.String())
	}
}

// TestPrecedentTuning_EndToEnd seeds a real chain (six plan approves then a
// reject), backfills the real decision index, and runs the command through the
// production loader: the reject is the one examined decision and it fires. A
// column, filter or class-routing break in loadTuningRows fails here.
func TestPrecedentTuning_EndToEnd(t *testing.T) {
	dbURL := pgtest.NewURL(t)
	ctx := context.Background()
	pool, err := postgres.Connect(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	runID, stageID := uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO runs (id, repo, workflow_id, workflow_sha, trigger_source, state, runner_kind)
		VALUES ($1, 'acme/widgets', 'feature_change', 'sha-1', 'cli', 'pending', 'local')`, runID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO stages (id, run_id, sequence, stage_type, executor_kind, executor_ref, state)
		VALUES ($1, $2, 0, 'plan', 'agent', 'claude-code', 'pending')`, stageID, runID); err != nil {
		t.Fatal(err)
	}
	repo := audit.NewPostgresRepository(pool)
	ts := time.Now().UTC().Add(-time.Hour)
	for i, decision := range []string{"approve", "approve", "approve", "approve", "approve", "approve", "reject"} {
		payload, _ := json.Marshal(map[string]any{"decision": decision, "rejection_comment": "x"})
		if _, err := repo.AppendChained(ctx, audit.ChainAppendParams{
			RunID: runID, StageID: &stageID, Timestamp: ts.Add(time.Duration(i) * time.Minute),
			Category: "approval_submitted", Payload: payload,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := decisionindex.Backfill(ctx, pool, decisionindex.Options{}); err != nil {
		t.Fatal(err)
	}

	var out, log strings.Builder
	code := runPrecedentTuningWith([]string{
		"--db", dbURL, "--repo", "acme/widgets",
		"--min-decisions", "5,10", "--min-agreement", "0.8", "--window", "30d",
	}, &out, &log, loadTuningRows)
	if code != exitOK {
		t.Fatalf("exit = %d; stderr: %s", code, log.String())
	}
	for _, want := range []string{
		"rows_read=7 candidates=2",
		"min_decisions=10 min_agreement=0.80 window=30d examined=1 fired=0 fire_rate=0.0000 below_min_decisions=1",
		"min_decisions=5 min_agreement=0.80 window=30d examined=1 fired=1 fire_rate=1.0000",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("report missing %q:\n%s", want, out.String())
		}
	}

	// An unknown repository reads zero rows and refuses rather than printing
	// an all-zero report.
	out.Reset()
	log.Reset()
	if code := runPrecedentTuningWith([]string{"--db", dbURL, "--repo", "acme/nope"}, &out, &log, loadTuningRows); code != exitFailure {
		t.Fatalf("unknown repo exit = %d, want %d: %s", code, exitFailure, log.String())
	}
}

// TestPrecedentTuning_ConnectFailure: an unreachable database is a failure
// exit naming the read, not a usage one.
func TestPrecedentTuning_ConnectFailure(t *testing.T) {
	var out, log strings.Builder
	code := runPrecedentTuningWith([]string{
		"--db", "postgres://x:y@127.0.0.1:1/db?connect_timeout=2", "--repo", "acme/widgets",
	}, &out, &log, loadTuningRows)
	if code != exitFailure || !strings.Contains(log.String(), "read decision index failed") {
		t.Fatalf("exit = %d, stderr: %s", code, log.String())
	}
}
