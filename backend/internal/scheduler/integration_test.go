package scheduler_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/scheduler"
	"github.com/kuhlman-labs/fishhawk/backend/internal/server"
)

// The cross-boundary end-to-end test of E79.1 / #3725: a REAL scheduler.Ticker
// wired to a REAL server.Server's StartScheduledRun, over pgtest Postgres run
// and audit repositories, with a fake clock and a fake SpecSource. Per-layer
// unit tests (the ticker against a recording starter, StartScheduledRun against
// in-memory run fakes) would stay green while the seam between them broke —
// the key derivation not reaching the replay lookup, the outcome mapping
// drifting, the (idempotency_key, repo) index not being what dedupes a restart
// — so this file drives the whole path and reads committed rows back.

const integrationRepo = "kuhlman-labs/fishhawk"

// integrationSpec is a v2 spec whose `upkeep` workflow fires daily at 09:00
// UTC. extraWorkflow / extraUpkeep splice additional YAML into the document so
// one fixture shape serves every test.
func integrationSpec(extraUpkeep, extraWorkflows string) string {
	return `version: "2"
workflows:
  upkeep:
    applies_to:
      trigger:
        - scheduled
    schedule:
      cron: "0 9 * * *"
` + extraUpkeep + `    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        produces:
          - artifact: pull_request
` + extraWorkflows
}

// serverStarter binds scheduler.RunStarter to server.Server.StartScheduledRun —
// the same translation fishhawkd's serve.go adapter performs (that one lives in
// package main and cannot be imported here).
type serverStarter struct{ srv *server.Server }

func (a serverStarter) StartScheduledRun(ctx context.Context, req scheduler.StartRequest) (scheduler.StartOutcome, error) {
	out, err := a.srv.StartScheduledRun(ctx, server.ScheduledRunParams{
		Repo:           req.Repo,
		WorkflowID:     req.WorkflowID,
		WorkflowSHA:    req.WorkflowSHA,
		WorkflowSpec:   req.WorkflowSpec,
		IdempotencyKey: req.IdempotencyKey,
		IssueNumber:    req.IssueNumber,
		RunnerKind:     req.RunnerKind,
	})
	if err != nil {
		return scheduler.StartOutcome{}, err
	}
	so := scheduler.StartOutcome{Kind: scheduler.OutcomeKind(out.Kind), Code: out.Code, Message: out.Message, Status: out.Status}
	if out.RunID != uuid.Nil {
		so.RunID = out.RunID.String()
	}
	return so, nil
}

type fixedSpecs struct{ content string }

func (f fixedSpecs) FetchSpec(context.Context, string) ([]byte, string, error) {
	return []byte(f.content), "blobsha", nil
}

// clock is the fake clock every Ticker in one test shares, so a "restart" (a
// fresh Ticker) sees the same time line.
type clock struct{ now time.Time }

func (c *clock) Now() time.Time { return c.now }

type harness struct {
	pool  *pgxpool.Pool
	runs  run.Repository
	audit audit.Repository
	srv   *server.Server
	clk   *clock
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	pool := pgtest.NewPool(t)
	runs := run.NewPostgresRepository(pool)
	au := audit.NewPostgresRepository(pool)
	return &harness{
		pool:  pool,
		runs:  runs,
		audit: au,
		srv:   server.New(server.Config{Addr: "127.0.0.1:0", RunRepo: runs, AuditRepo: au}),
		clk:   &clock{},
	}
}

// newTicker builds a fresh Ticker over the shared database. A second call is
// the simulated restart: it carries no in-memory attempted mark.
func (h *harness) newTicker(specYAML string) *scheduler.Ticker {
	return &scheduler.Ticker{
		Repos:      []string{integrationRepo},
		Specs:      fixedSpecs{content: specYAML},
		Starter:    serverStarter{srv: h.srv},
		Audit:      h.audit,
		RunnerKind: run.RunnerKindGitHubActions,
		Now:        h.clk.Now,
	}
}

type runRow struct {
	workflowID, triggerSource, idempotencyKey string
}

// committedRuns reads the run rows back from Postgres in creation order.
func (h *harness) committedRuns(t *testing.T) []runRow {
	t.Helper()
	rows, err := h.pool.Query(context.Background(),
		`SELECT workflow_id, trigger_source, COALESCE(idempotency_key, '') FROM runs WHERE repo = $1 ORDER BY created_at, id`,
		integrationRepo)
	if err != nil {
		t.Fatalf("query runs: %v", err)
	}
	defer rows.Close()
	var out []runRow
	for rows.Next() {
		var r runRow
		if err := rows.Scan(&r.workflowID, &r.triggerSource, &r.idempotencyKey); err != nil {
			t.Fatalf("scan run: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate runs: %v", err)
	}
	return out
}

// globalEntries returns the committed global-chain entries whose category is
// in categories, in append order.
func (h *harness) globalEntries(t *testing.T, categories ...string) []*audit.Entry {
	t.Helper()
	all, err := h.audit.ListGlobal(context.Background())
	if err != nil {
		t.Fatalf("list global audit: %v", err)
	}
	want := map[string]bool{}
	for _, c := range categories {
		want[c] = true
	}
	var out []*audit.Entry
	for _, e := range all {
		if want[e.Category] {
			out = append(out, e)
		}
	}
	return out
}

var schedulerCategories = []string{
	scheduler.CategoryScheduledRunStarted,
	scheduler.CategoryScheduledRunSkipped,
	scheduler.CategoryScheduledRunRefused,
}

func categoriesOf(entries []*audit.Entry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Category)
	}
	return out
}

func payloadOf(t *testing.T, e *audit.Entry) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(e.Payload, &m); err != nil {
		t.Fatalf("decode %s payload: %v", e.Category, err)
	}
	return m
}

// TestScheduler_TwoWindowsAndRestart_ExactlyTwoRuns is the binding end-to-end
// test: a tick inside window W1 creates one run; a later tick in W1 creates no
// run and no audit entry; a SECOND Ticker over the same database (the
// simulated restart) ticking in W1 records scheduled_run_skipped and still
// leaves one run; a tick in W2 creates the second run.
//
// COUNTERFACTUAL (approval condition 6 — the fixture state that makes the
// deletion observable): mutate scheduler.IdempotencyKey's BODY to return "".
// StartScheduledRun then sends no Idempotency-Key header, so handleCreateRun
// treats every request as non-idempotent. The state that makes that visible
// is the RESTARTED Ticker ticking inside W1: it holds no in-memory attempted
// mark for W1 (it is a fresh struct), so the replay lookup the key triggers is
// the ONLY thing between its tick and a second W1 row. With the key empty that
// lookup is skipped, a second row is inserted, and the test observes three
// runs (and started, started, started) instead of two — RED.
func TestScheduler_TwoWindowsAndRestart_ExactlyTwoRuns(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	specYAML := integrationSpec("", "")
	w1 := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	w2 := time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)

	// Tick 1, inside W1: one run, one started entry.
	tk := h.newTicker(specYAML)
	h.clk.now = w1.Add(time.Hour)
	tk.Tick(ctx)
	if got := h.committedRuns(t); len(got) != 1 {
		t.Fatalf("after the first W1 tick: %d runs, want 1: %+v", len(got), got)
	}
	if got := categoriesOf(h.globalEntries(t, schedulerCategories...)); len(got) != 1 || got[0] != scheduler.CategoryScheduledRunStarted {
		t.Fatalf("after the first W1 tick: scheduler audit = %v, want [started]", got)
	}

	// Tick 2, still W1, same process: no run and no audit entry (no flood).
	h.clk.now = w1.Add(5 * time.Hour)
	tk.Tick(ctx)
	if got := h.committedRuns(t); len(got) != 1 {
		t.Fatalf("after a second W1 tick: %d runs, want 1", len(got))
	}
	if got := h.globalEntries(t, schedulerCategories...); len(got) != 1 {
		t.Fatalf("after a second W1 tick: %d scheduler audit entries, want 1 (no per-tick flood): %v", len(got), categoriesOf(got))
	}

	// Restart: a fresh Ticker over the same database, still inside W1. The
	// Idempotency-Key replay answers already_started → skipped, no new row.
	restarted := h.newTicker(specYAML)
	h.clk.now = w1.Add(11 * time.Hour)
	restarted.Tick(ctx)
	if got := h.committedRuns(t); len(got) != 1 {
		t.Fatalf("after the restarted W1 tick: %d runs, want still 1 (the key must replay)", len(got))
	}

	// W2: the second run.
	h.clk.now = w2.Add(30 * time.Minute)
	restarted.Tick(ctx)

	runs := h.committedRuns(t)
	if len(runs) != 2 {
		t.Fatalf("runs = %d, want exactly 2: %+v", len(runs), runs)
	}
	wantKeys := []string{scheduler.IdempotencyKey("upkeep", w1), scheduler.IdempotencyKey("upkeep", w2)}
	if wantKeys[0] != "scheduled:upkeep:2026-10-01T09:00:00Z" || wantKeys[1] != "scheduled:upkeep:2026-10-02T09:00:00Z" {
		t.Fatalf("IdempotencyKey derivation = %v; want scheduled:<wf>:<window UTC RFC3339>", wantKeys)
	}
	for i, r := range runs {
		if r.triggerSource != string(run.TriggerScheduled) || r.workflowID != "upkeep" {
			t.Errorf("run %d = %+v, want trigger_source scheduled, workflow upkeep", i, r)
		}
		if r.idempotencyKey != wantKeys[i] {
			t.Errorf("run %d idempotency key = %q, want %q", i, r.idempotencyKey, wantKeys[i])
		}
	}

	entries := h.globalEntries(t, schedulerCategories...)
	want := []string{scheduler.CategoryScheduledRunStarted, scheduler.CategoryScheduledRunSkipped, scheduler.CategoryScheduledRunStarted}
	got := categoriesOf(entries)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("global audit sequence = %v, want %v", got, want)
	}
	skipped := payloadOf(t, entries[1])
	started := payloadOf(t, entries[0])
	if skipped["reason"] != scheduler.SkipReasonAlreadyStarted || skipped["run_id"] != started["run_id"] {
		t.Errorf("skipped payload = %v; want reason already_started and the W1 run id %v", skipped, started["run_id"])
	}
	if skipped["window_start"] != "2026-10-01T09:00:00Z" || skipped["idempotency_key"] != wantKeys[0] {
		t.Errorf("skipped payload = %v; want the W1 window and key", skipped)
	}
}

// TestScheduler_BudgetExhausted_RecordsRefusalStartsNothing proves the
// blocking periodic budget gate runs on the scheduled path end to end: the
// spec declares a blocking weekly budget with a tiny limit, a prior run of the
// workflow carries cost above it, and one due tick yields exactly one
// scheduled_run_refused (code budget_exhausted, a message naming the period)
// beside the handler's own run_rejected_budget, and zero new run rows.
func TestScheduler_BudgetExhausted_RecordsRefusalStartsNothing(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	specYAML := integrationSpec(`    budgets:
      - period: weekly
        limit_usd: 0.5
        enforcement: blocking
`, "")

	// Seed spend by construction: a prior (hand-started) run of the same
	// workflow, created now (the budget gate sums the real current period),
	// carrying cost above the limit.
	prior, err := h.runs.CreateRun(ctx, run.CreateRunParams{
		Repo:          integrationRepo,
		WorkflowID:    "upkeep",
		WorkflowSHA:   "blobsha",
		TriggerSource: run.TriggerOnDemand,
	})
	if err != nil {
		t.Fatalf("seed prior run: %v", err)
	}
	coster, ok := h.runs.(interface {
		AddRunCost(ctx context.Context, id uuid.UUID, deltaUSD float64, resolvedModel string) (*run.Run, error)
	})
	if !ok {
		t.Fatal("postgres run repository does not expose AddRunCost")
	}
	if _, err := coster.AddRunCost(ctx, prior.ID, 5, "claude-opus-5-5"); err != nil {
		t.Fatalf("seed prior cost: %v", err)
	}

	h.clk.now = time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	h.newTicker(specYAML).Tick(ctx)

	if got := h.committedRuns(t); len(got) != 1 {
		t.Fatalf("runs = %+v, want only the seeded prior run (a refused window mints nothing)", got)
	}
	refused := h.globalEntries(t, schedulerCategories...)
	if got := categoriesOf(refused); len(got) != 1 || got[0] != scheduler.CategoryScheduledRunRefused {
		t.Fatalf("scheduler audit = %v, want exactly [refused]", got)
	}
	p := payloadOf(t, refused[0])
	if p["code"] != "budget_exhausted" {
		t.Errorf("refused code = %v, want budget_exhausted", p["code"])
	}
	if msg, _ := p["message"].(string); !strings.Contains(msg, "weekly") {
		t.Errorf("refused message = %q, want it to name the budget period", msg)
	}
	if status, _ := p["status"].(float64); status != 402 {
		t.Errorf("refused status = %v, want 402", p["status"])
	}
	if n := len(h.globalEntries(t, "run_rejected_budget")); n != 1 {
		t.Errorf("run_rejected_budget entries = %d, want 1 (the handler's own budget audit)", n)
	}
}

// TestScheduler_UnscheduledWorkflowNeverStarted: a sibling workflow that lists
// scheduled in applies_to.trigger but declares NO schedule is never started —
// declaring the trigger form is not a cadence. Only the scheduled `upkeep`
// runs, and every scheduler audit entry names it.
func TestScheduler_UnscheduledWorkflowNeverStarted(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	specYAML := integrationSpec("", `  groom:
    applies_to:
      trigger:
        - scheduled
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        produces:
          - artifact: pull_request
`)

	h.clk.now = time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	h.newTicker(specYAML).Tick(ctx)

	runs := h.committedRuns(t)
	if len(runs) != 1 || runs[0].workflowID != "upkeep" {
		t.Fatalf("runs = %+v, want exactly one, of the scheduled workflow upkeep", runs)
	}
	entries := h.globalEntries(t, schedulerCategories...)
	if len(entries) != 1 {
		t.Fatalf("scheduler audit = %v, want exactly one entry", categoriesOf(entries))
	}
	if wf := payloadOf(t, entries[0])["workflow_id"]; wf != "upkeep" {
		t.Errorf("audit entry workflow_id = %v, want upkeep (groom declares no schedule)", wf)
	}
}

// TestScheduler_OccupiedKey_RefusedNotAlreadyStarted closes the window-key
// squat gap: a run an ordinary create minted under the upcoming window's
// Idempotency-Key (here trigger_source on_demand, SAME workflow) must not be
// read as "this window was already started". The tick instead records exactly
// one scheduled_run_refused (code scheduled_key_occupied, status 409, run_id =
// the occupant), starts nothing, and a second tick inside the window adds no
// entry (the refusal is definitive for the window). The restart-inside-W1 →
// scheduled_run_skipped path stays proven by
// TestScheduler_TwoWindowsAndRestart_ExactlyTwoRuns.
//
// COUNTERFACTUAL: make the classifier's occupancy check permissive (accept
// every 200 replay as already_started). The fixture seeds an on_demand run of
// the SAME workflow under the EXACT window key, so handleCreateRun's replay
// lookup answers it with 200 and the trigger_source comparison is the only
// thing separating refused from skipped: the test then reads
// [scheduled_run_skipped] — RED.
func TestScheduler_OccupiedKey_RefusedNotAlreadyStarted(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	specYAML := integrationSpec("", "")
	w1 := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)

	// Seed the squatter BY CONSTRUCTION: a non-scheduled run holding W1's key.
	key := scheduler.IdempotencyKey("upkeep", w1)
	occupant, err := h.runs.CreateRun(ctx, run.CreateRunParams{
		Repo:           integrationRepo,
		WorkflowID:     "upkeep",
		WorkflowSHA:    "blobsha",
		TriggerSource:  run.TriggerOnDemand,
		IdempotencyKey: &key,
	})
	if err != nil {
		t.Fatalf("seed occupant run: %v", err)
	}

	h.clk.now = w1.Add(time.Hour)
	tk := h.newTicker(specYAML)
	tk.Tick(ctx)

	runs := h.committedRuns(t)
	if len(runs) != 1 || runs[0].triggerSource != string(run.TriggerOnDemand) || runs[0].idempotencyKey != key {
		t.Fatalf("runs = %+v, want only the on_demand occupant (the refused window mints nothing)", runs)
	}
	entries := h.globalEntries(t, schedulerCategories...)
	if got := categoriesOf(entries); len(got) != 1 || got[0] != scheduler.CategoryScheduledRunRefused {
		t.Fatalf("scheduler audit = %v, want exactly [scheduled_run_refused] (not skipped, not started)", got)
	}
	p := payloadOf(t, entries[0])
	if p["code"] != server.ScheduledKeyOccupiedCode {
		t.Errorf("refused code = %v, want %s", p["code"], server.ScheduledKeyOccupiedCode)
	}
	if status, _ := p["status"].(float64); status != 409 {
		t.Errorf("refused status = %v, want 409", p["status"])
	}
	if p["run_id"] != occupant.ID.String() {
		t.Errorf("refused run_id = %v, want the occupant %s", p["run_id"], occupant.ID)
	}
	if msg, _ := p["message"].(string); !strings.Contains(msg, occupant.ID.String()) || !strings.Contains(msg, "on_demand") {
		t.Errorf("refused message = %q, want it to name the occupant id and its trigger_source on_demand", msg)
	}

	// Definitive for the window: a second tick in W1 adds no entry and no run.
	h.clk.now = w1.Add(5 * time.Hour)
	tk.Tick(ctx)
	if got := h.globalEntries(t, schedulerCategories...); len(got) != 1 {
		t.Errorf("after a second W1 tick: %d scheduler entries, want still 1: %v", len(got), categoriesOf(got))
	}
	if got := h.committedRuns(t); len(got) != 1 {
		t.Errorf("after a second W1 tick: %d runs, want still 1", len(got))
	}
}
