package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	neturl "net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/childcancel"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/postgres"
	runpkg "github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/server"
	"github.com/kuhlman-labs/fishhawk/backend/internal/stalesweep"
)

// stubProbe is the live-runner probe seam.
type stubProbe struct {
	rep stalesweep.ProbeReport
	err error
}

func (s stubProbe) LiveRunners(context.Context) (stalesweep.ProbeReport, error) { return s.rep, s.err }

var noLiveRunners = stubProbe{rep: stalesweep.ProbeReport{RunIDs: map[uuid.UUID]bool{}}}

func TestSweepStaleRuns_RequiresDB(t *testing.T) {
	t.Setenv("FISHHAWKD_DATABASE_URL", "")
	var errOut strings.Builder
	if got := runSweepStaleRunsWith(nil, io.Discard, &errOut, noLiveRunners); got != exitUsage {
		t.Errorf("exit = %d, want %d", got, exitUsage)
	}
	if !strings.Contains(errOut.String(), "--db or FISHHAWKD_DATABASE_URL is required") {
		t.Errorf("stderr = %q, want the --db hint", errOut.String())
	}
}

func TestSweepStaleRuns_BadFlag(t *testing.T) {
	if got := run([]string{"sweep-stale-runs", "--no-such-flag"}, io.Discard); got != exitUsage {
		t.Errorf("exit = %d, want %d", got, exitUsage)
	}
	var errOut strings.Builder
	if got := runSweepStaleRunsWith([]string{"--db", "postgres://x", "extra"}, io.Discard, &errOut, noLiveRunners); got != exitUsage ||
		!strings.Contains(errOut.String(), `unexpected argument "extra"`) {
		t.Errorf("positional arg: exit = %d, stderr %q; want %d and the unexpected-argument line", got, errOut.String(), exitUsage)
	}
}

// TestSweepStaleRuns_InvalidDays: --days < 1 is a usage error, checked BEFORE
// --db so it is observable with no database configured.
func TestSweepStaleRuns_InvalidDays(t *testing.T) {
	t.Setenv("FISHHAWKD_DATABASE_URL", "")
	for _, d := range []string{"0", "-3"} {
		var errOut strings.Builder
		if got := runSweepStaleRunsWith([]string{"--days", d}, io.Discard, &errOut, noLiveRunners); got != exitUsage ||
			!strings.Contains(errOut.String(), "--days must be >= 1") {
			t.Errorf("--days %s: exit = %d, stderr %q; want %d naming --days", d, got, errOut.String(), exitUsage)
		}
	}
}

func TestSweepStaleRuns_ConnectFailure(t *testing.T) {
	var errOut strings.Builder
	got := runSweepStaleRunsWith([]string{"--db", "postgres://nobody@127.0.0.1:1/none?sslmode=disable&connect_timeout=1"}, io.Discard, &errOut, noLiveRunners)
	if got != exitFailure || !strings.Contains(errOut.String(), "connect:") {
		t.Errorf("exit = %d, stderr %q; want %d and a connect error", got, errOut.String(), exitFailure)
	}
}

// TestSweepStaleRuns_ScanFailure: a database without the schema fails the scan
// closed with exit 1 and applies nothing.
func TestSweepStaleRuns_ScanFailure(t *testing.T) {
	u, err := neturl.Parse(pgtest.NewURL(t))
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	u.Path = "/postgres" // the maintenance database carries no runs table
	var out, errOut strings.Builder
	if got := runSweepStaleRunsWith([]string{"--db", u.String(), "--apply"}, &out, &errOut, noLiveRunners); got != exitFailure ||
		!strings.Contains(errOut.String(), "scan:") || strings.Contains(out.String(), "applied:") {
		t.Errorf("exit = %d, stdout %q, stderr %q; want %d, a scan error and no apply", got, out.String(), errOut.String(), exitFailure)
	}
}

func TestSweepRunStore_RefusesWithoutCAS(t *testing.T) {
	if _, err := sweepRunStore(runpkg.BaseFake{}); err == nil || !strings.Contains(err.Error(), "StageCASTransitioner") {
		t.Errorf("err = %v, want a refusal naming the missing capability", err)
	}
}

// TestEvidenceCategoriesMatchServer pins stalesweep's PR-evidence strings to
// the server's own constants, so a rename on either side fails here.
func TestEvidenceCategoriesMatchServer(t *testing.T) {
	for got, want := range map[string]string{
		stalesweep.CategoryPRMerged:                 server.CategoryPRMerged,
		stalesweep.CategoryPostMergeObserved:        server.CategoryPostMergeObserved,
		stalesweep.CategoryMergeObservationRecorded: server.CategoryMergeObservationRecorded,
		stalesweep.CategoryPRClosedWithoutMerge:     server.CategoryPRClosedWithoutMerge,
	} {
		if got != want {
			t.Errorf("stalesweep evidence category %q != server %q", got, want)
		}
	}
	if !audit.IsKnownCategory(stalesweep.Category) || !audit.IsKnownCategory(stalesweep.CategoryPullRequestOpened) {
		t.Errorf("stale_run_swept / pull_request_opened must be registered audit categories")
	}
}

// sweepEnv is one pgtest database with seeding helpers. Every stale timestamp
// is derived from the DATABASE clock (now() - 30 days) and a fresh row is left
// at the DB's own now(), so the 14-day threshold margin holds by construction
// against any host/DB skew (the #3048 cross-clock trap).
type sweepEnv struct {
	t       *testing.T
	ctx     context.Context
	url     string
	pool    *pgxpool.Pool
	runs    runpkg.Repository
	au      audit.Repository
	staleTS time.Time
}

func newSweepEnv(t *testing.T) *sweepEnv {
	t.Helper()
	ctx := context.Background()
	url := pgtest.NewURL(t)
	pool, err := postgres.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	e := &sweepEnv{t: t, ctx: ctx, url: url, pool: pool, runs: runpkg.NewPostgresRepository(pool), au: audit.NewPostgresRepository(pool)}
	if err := pool.QueryRow(ctx, `SELECT now() - interval '30 days'`).Scan(&e.staleTS); err != nil {
		t.Fatalf("db clock: %v", err)
	}
	return e
}

func (e *sweepEnv) mkRun(parent *uuid.UUID, states ...runpkg.State) *runpkg.Run {
	e.t.Helper()
	r, err := e.runs.CreateRun(e.ctx, runpkg.CreateRunParams{
		Repo: "x/y", WorkflowID: "feature_change", WorkflowSHA: "abc",
		TriggerSource: runpkg.TriggerCLI, DecomposedFrom: parent, ParentRunID: parent,
	})
	if err != nil {
		e.t.Fatalf("create run: %v", err)
	}
	for _, st := range states {
		if r, err = e.runs.TransitionRun(e.ctx, r.ID, st); err != nil {
			e.t.Fatalf("transition -> %s: %v", st, err)
		}
	}
	return r
}

func (e *sweepEnv) mkStage(runID uuid.UUID, seq int, typ runpkg.StageType, path ...runpkg.StageState) *runpkg.Stage {
	e.t.Helper()
	s, err := e.runs.CreateStage(e.ctx, runpkg.CreateStageParams{
		RunID: runID, Sequence: seq, Type: typ, ExecutorKind: runpkg.ExecutorAgent, ExecutorRef: "claude-code",
	})
	if err != nil {
		e.t.Fatalf("create stage: %v", err)
	}
	for _, st := range path {
		if s, err = e.runs.TransitionStage(e.ctx, s.ID, st, nil); err != nil {
			e.t.Fatalf("stage -> %s: %v", st, err)
		}
	}
	return s
}

// toSucceeded / toAwaitingApproval are the legal stage walks.
var (
	toSucceeded        = []runpkg.StageState{runpkg.StageStateDispatched, runpkg.StageStateRunning, runpkg.StageStateSucceeded}
	toAwaitingApproval = []runpkg.StageState{runpkg.StageStateDispatched, runpkg.StageStateRunning, runpkg.StageStateAwaitingApproval}
)

func (e *sweepEnv) seedAudit(runID uuid.UUID, category string) {
	e.t.Helper()
	actor := audit.ActorSystem
	if _, err := e.au.AppendChained(e.ctx, audit.ChainAppendParams{
		RunID: runID, Timestamp: e.staleTS, Category: category, ActorKind: &actor, Payload: json.RawMessage(`{}`),
	}); err != nil {
		e.t.Fatalf("seed %s: %v", category, err)
	}
}

// backdate stamps the runs' and their stages' updated_at 30 days back. The
// fishhawk_set_updated_at BEFORE UPDATE triggers would overwrite the value, so
// they are disabled for the one transaction.
func (e *sweepEnv) backdate(ids ...uuid.UUID) {
	e.t.Helper()
	tx, err := e.pool.Begin(e.ctx)
	if err != nil {
		e.t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(e.ctx) }()
	for _, q := range []string{
		`ALTER TABLE runs DISABLE TRIGGER runs_set_updated_at`,
		`ALTER TABLE stages DISABLE TRIGGER stages_set_updated_at`,
	} {
		if _, err := tx.Exec(e.ctx, q); err != nil {
			e.t.Fatalf("%s: %v", q, err)
		}
	}
	if _, err := tx.Exec(e.ctx, `UPDATE runs SET updated_at = now() - interval '30 days' WHERE id = ANY($1)`, ids); err != nil {
		e.t.Fatalf("backdate runs: %v", err)
	}
	if _, err := tx.Exec(e.ctx, `UPDATE stages SET updated_at = now() - interval '30 days' WHERE run_id = ANY($1)`, ids); err != nil {
		e.t.Fatalf("backdate stages: %v", err)
	}
	for _, q := range []string{
		`ALTER TABLE runs ENABLE TRIGGER runs_set_updated_at`,
		`ALTER TABLE stages ENABLE TRIGGER stages_set_updated_at`,
	} {
		if _, err := tx.Exec(e.ctx, q); err != nil {
			e.t.Fatalf("%s: %v", q, err)
		}
	}
	if err := tx.Commit(e.ctx); err != nil {
		e.t.Fatalf("commit: %v", err)
	}
}

func (e *sweepEnv) state(id uuid.UUID) runpkg.State {
	e.t.Helper()
	r, err := e.runs.GetRun(e.ctx, id)
	if err != nil {
		e.t.Fatalf("GetRun: %v", err)
	}
	return r.State
}

func (e *sweepEnv) stageState(id uuid.UUID) runpkg.StageState {
	e.t.Helper()
	s, err := e.runs.GetStage(e.ctx, id)
	if err != nil {
		e.t.Fatalf("GetStage: %v", err)
	}
	return s.State
}

func (e *sweepEnv) rows(id uuid.UUID, category string) []*audit.Entry {
	e.t.Helper()
	es, err := e.au.ListForRunByCategory(e.ctx, id, category)
	if err != nil {
		e.t.Fatalf("list audit: %v", err)
	}
	return es
}

// verifyChain recomputes every entry hash on runID's chain and checks the
// prev_hash links.
func (e *sweepEnv) verifyChain(runID uuid.UUID) {
	e.t.Helper()
	entries, err := e.au.ListForRun(e.ctx, runID)
	if err != nil {
		e.t.Fatalf("list chain: %v", err)
	}
	var prev *string
	for i, en := range entries {
		if (prev == nil) != (en.PrevHash == nil) || (prev != nil && *prev != *en.PrevHash) {
			e.t.Fatalf("run %s entry %d prev_hash = %v, want %v", runID, i, en.PrevHash, prev)
		}
		h, err := audit.ComputeEntryHash(audit.HashInputs{
			RunID: en.RunID, StageID: en.StageID, Timestamp: en.Timestamp, Category: en.Category,
			ActorKind: en.ActorKind, ActorSubject: en.ActorSubject, Payload: en.Payload, PrevHash: en.PrevHash,
		})
		if err != nil || h != en.EntryHash {
			e.t.Fatalf("run %s entry %d hash = %q (err %v), stored %q", runID, i, h, err, en.EntryHash)
		}
		eh := en.EntryHash
		prev = &eh
	}
}

// staleAbandoned seeds one stale running run (review parked at
// awaiting_approval, no PR evidence): it classifies abandoned.
func (e *sweepEnv) staleAbandoned() (*runpkg.Run, *runpkg.Stage) {
	r := e.mkRun(nil, runpkg.StateRunning)
	e.mkStage(r.ID, 0, runpkg.StageTypeImplement, toSucceeded...)
	review := e.mkStage(r.ID, 1, runpkg.StageTypeReview, toAwaitingApproval...)
	e.backdate(r.ID)
	return r, review
}

// TestSweepStaleRuns_DryRunThenApply is the end-to-end sweep over real
// Postgres: CLI → stalesweep → run state machine → chained audit. The dry run
// lists every top-level candidate and writes nothing (counterfactual: deleting
// the `if !*apply` return makes the post-dry-run re-read go RED). --apply
// cancels the abandoned and pr_closed runs (cancelling the abandoned run's
// parked review), completes the pending all-succeeded run through the REAL
// state machine's two legal steps (a single pending→succeeded step is refused
// by the repo), cascades the decomposed parent's cancel to its child, leaves
// merged / pr_unobserved / fresh / the stale child untouched, and appends
// exactly one stale_run_swept row per transitioned run on a verifying chain. A
// second --apply transitions nothing.
func TestSweepStaleRuns_DryRunThenApply(t *testing.T) {
	e := newSweepEnv(t)

	abandoned, abandonedReview := e.staleAbandoned()

	pendingAll := e.mkRun(nil)
	e.mkStage(pendingAll.ID, 0, runpkg.StageTypePlan, toSucceeded...)
	e.mkStage(pendingAll.ID, 1, runpkg.StageTypeImplement, toSucceeded...)

	prClosed := e.mkRun(nil, runpkg.StateRunning)
	e.mkStage(prClosed.ID, 0, runpkg.StageTypeReview, toAwaitingApproval...)
	e.seedAudit(prClosed.ID, stalesweep.CategoryPullRequestOpened)
	e.seedAudit(prClosed.ID, stalesweep.CategoryPRClosedWithoutMerge)

	merged := e.mkRun(nil, runpkg.StateRunning)
	e.mkStage(merged.ID, 0, runpkg.StageTypeReview, toAwaitingApproval...)
	e.seedAudit(merged.ID, stalesweep.CategoryPRMerged)

	unobserved := e.mkRun(nil, runpkg.StateRunning)
	e.mkStage(unobserved.ID, 0, runpkg.StageTypeReview, toAwaitingApproval...)
	if _, err := e.runs.SetRunPullRequestURL(e.ctx, unobserved.ID, "https://github.com/x/y/pull/9"); err != nil {
		t.Fatalf("set PR url: %v", err)
	}

	fresh := e.mkRun(nil, runpkg.StateRunning)
	e.mkStage(fresh.ID, 0, runpkg.StageTypeImplement)

	terminalParent := e.mkRun(nil, runpkg.StateRunning, runpkg.StateSucceeded)
	staleChild := e.mkRun(&terminalParent.ID, runpkg.StateRunning)
	e.mkStage(staleChild.ID, 0, runpkg.StageTypeImplement)

	decomposed := e.mkRun(nil, runpkg.StateRunning)
	e.mkStage(decomposed.ID, 0, runpkg.StageTypeImplement, runpkg.StageStateAwaitingChildren)
	decomposedChild := e.mkRun(&decomposed.ID, runpkg.StateRunning)

	e.backdate(pendingAll.ID, prClosed.ID, merged.ID, unobserved.ID, staleChild.ID, decomposed.ID)

	all := []uuid.UUID{abandoned.ID, pendingAll.ID, prClosed.ID, merged.ID, unobserved.ID, fresh.ID, staleChild.ID, decomposed.ID, decomposedChild.ID}
	initial := map[uuid.UUID]runpkg.State{}
	for _, id := range all {
		initial[id] = e.state(id)
	}

	// Dry run.
	var out, errOut bytes.Buffer
	if got := runSweepStaleRunsWith([]string{"--db", e.url}, &out, &errOut, noLiveRunners); got != exitOK {
		t.Fatalf("dry run exit = %d; stderr %s", got, errOut.String())
	}
	o := out.String()
	for id, want := range map[uuid.UUID]string{
		abandoned.ID:  "state=running class=abandoned action=cancel target=cancelled",
		pendingAll.ID: "state=pending class=stages_settled action=reconcile_succeeded target=succeeded",
		prClosed.ID:   "state=running class=pr_closed action=cancel target=cancelled",
		merged.ID:     "state=running class=merged action=delegate_reconcile_merge target=-",
		unobserved.ID: "state=running class=pr_unobserved action=skip target=-",
		fresh.ID:      "state=running class=fresh action=skip target=-",
		decomposed.ID: "state=running class=abandoned action=cancel target=cancelled",
	} {
		if !strings.Contains(o, "run="+id.String()+" "+want) {
			t.Errorf("dry-run output missing %q for %s:\n%s", want, id, o)
		}
	}
	for _, want := range []string{
		"stages=implement:succeeded,review:awaiting_approval pr=none",
		"pr=opened pr_url=https://github.com/x/y/pull/9",
		"remedy: fishhawk_reconcile_merge / POST /v0/runs/" + merged.ID.String() + "/reconcile-merge",
		"remedy: fishhawk_record_merge_observation then reconcile-merge, or re-run with --cancel-unobserved-pr",
		"dry-run: 7 candidate(s): fresh=1 live_runner=0 stages_settled=1 merged=1 pr_closed=1 pr_unobserved=1 abandoned=2; re-run with --apply to transition 4",
	} {
		if !strings.Contains(o, want) {
			t.Errorf("dry-run output missing %q:\n%s", want, o)
		}
	}
	for _, id := range []uuid.UUID{staleChild.ID, decomposedChild.ID} {
		if strings.Contains(o, "run="+id.String()) {
			t.Errorf("dry run listed decomposition child %s:\n%s", id, o)
		}
	}
	for _, id := range all {
		if got := e.state(id); got != initial[id] {
			t.Errorf("dry run changed %s: %s -> %s", id, initial[id], got)
		}
		if n := len(e.rows(id, stalesweep.Category)); n != 0 {
			t.Errorf("dry run appended %d rows on %s", n, id)
		}
	}

	// Apply.
	out.Reset()
	if got := runSweepStaleRunsWith([]string{"--db", e.url, "--apply"}, &out, &errOut, noLiveRunners); got != exitOK {
		t.Fatalf("apply exit = %d; out %s stderr %s", got, out.String(), errOut.String())
	}
	if !strings.Contains(out.String(), "applied: 4 run(s): transitioned=4 skipped_terminal=0 skipped_changed=0 failed=0 errors=0") ||
		!strings.Contains(out.String(), "applied run="+pendingAll.ID.String()+" class=stages_settled outcome=transitioned from=pending to=succeeded path=running,succeeded") {
		t.Errorf("apply output:\n%s", out.String())
	}
	for id, want := range map[uuid.UUID]runpkg.State{
		abandoned.ID: runpkg.StateCancelled, pendingAll.ID: runpkg.StateSucceeded, prClosed.ID: runpkg.StateCancelled,
		decomposed.ID: runpkg.StateCancelled, decomposedChild.ID: runpkg.StateCancelled,
		merged.ID: runpkg.StateRunning, unobserved.ID: runpkg.StateRunning, fresh.ID: runpkg.StateRunning, staleChild.ID: runpkg.StateRunning,
	} {
		if got := e.state(id); got != want {
			t.Errorf("run %s state = %s, want %s", id, got, want)
		}
	}
	if got := e.stageState(abandonedReview.ID); got != runpkg.StageStateCancelled {
		t.Errorf("abandoned run's review stage = %s, want cancelled", got)
	}
	for _, id := range []uuid.UUID{abandoned.ID, pendingAll.ID, prClosed.ID, decomposed.ID} {
		if n := len(e.rows(id, stalesweep.Category)); n != 1 {
			t.Errorf("run %s stale_run_swept rows = %d, want 1", id, n)
		}
		e.verifyChain(id)
	}
	if rs := e.rows(pendingAll.ID, stalesweep.Category); len(rs) == 1 {
		var p map[string]any
		_ = json.Unmarshal(rs[0].Payload, &p)
		if path, _ := p["transition_path"].([]any); p["class"] != "stages_settled" || p["reason"] != stalesweep.ReasonStagesSettled || len(path) != 2 {
			t.Errorf("pending-all-succeeded row = %v", p)
		}
	}
	for _, id := range []uuid.UUID{merged.ID, unobserved.ID, fresh.ID, staleChild.ID, decomposedChild.ID} {
		if n := len(e.rows(id, stalesweep.Category)); n != 0 {
			t.Errorf("untouched run %s carries %d stale_run_swept rows", id, n)
		}
	}
	childRows := e.rows(decomposedChild.ID, childcancel.Category)
	if len(childRows) != 1 {
		t.Fatalf("decomposition child rows = %d, want 1", len(childRows))
	}
	var cp map[string]any
	_ = json.Unmarshal(childRows[0].Payload, &cp)
	if cp["cancel_source"] != stalesweep.Source {
		t.Errorf("child row cancel_source = %v, want %s", cp["cancel_source"], stalesweep.Source)
	}

	// Second apply converges.
	out.Reset()
	if got := runSweepStaleRunsWith([]string{"--db", e.url, "--apply"}, &out, &errOut, noLiveRunners); got != exitOK {
		t.Fatalf("second apply exit = %d", got)
	}
	if !strings.Contains(out.String(), "applied: 0 run(s)") {
		t.Errorf("second apply output:\n%s", out.String())
	}
	for _, id := range []uuid.UUID{abandoned.ID, pendingAll.ID, prClosed.ID, decomposed.ID} {
		if n := len(e.rows(id, stalesweep.Category)); n != 1 {
			t.Errorf("second apply: %s rows = %d, want 1", id, n)
		}
	}
}

// TestSweepStaleRuns_ApplyRefusesOnProbeProblem: a failed probe, or a live
// fishhawk-runner with no attributable --run-id, is a dry-run WARNING and an
// --apply REFUSAL before any write (the stale candidate stays running).
func TestSweepStaleRuns_ApplyRefusesOnProbeProblem(t *testing.T) {
	for name, tc := range map[string]struct {
		probe   stubProbe
		warn    string
		refusal string
	}{
		"probe error": {stubProbe{err: errors.New("ps: executable file not found")},
			"runner-probe: unavailable (ps: executable file not found)", "--apply refused: the live-runner probe failed"},
		"unattributed runner": {stubProbe{rep: stalesweep.ProbeReport{RunIDs: map[uuid.UUID]bool{}, UnattributedPIDs: []string{"4242", "4343"}}},
			"runner-probe: fishhawk-runner pid(s) 4242,4343 carry no attributable --run-id", "--apply refused: live fishhawk-runner pid(s) 4242,4343"},
	} {
		t.Run(name, func(t *testing.T) {
			e := newSweepEnv(t)
			r, review := e.staleAbandoned()

			var out, errOut bytes.Buffer
			if got := runSweepStaleRunsWith([]string{"--db", e.url}, &out, &errOut, tc.probe); got != exitOK || !strings.Contains(out.String(), tc.warn) {
				t.Errorf("dry run exit = %d, out:\n%s\nwant 0 and %q", got, out.String(), tc.warn)
			}
			out.Reset()
			errOut.Reset()
			if got := runSweepStaleRunsWith([]string{"--db", e.url, "--apply"}, &out, &errOut, tc.probe); got != exitFailure ||
				!strings.Contains(errOut.String(), tc.refusal) || strings.Contains(out.String(), "applied") {
				t.Errorf("apply exit = %d, out %q, stderr %q; want %d, %q and no apply", got, out.String(), errOut.String(), exitFailure, tc.refusal)
			}
			if got := e.state(r.ID); got != runpkg.StateRunning {
				t.Errorf("stale run state = %s, want running (untouched)", got)
			}
			if got := e.stageState(review.ID); got != runpkg.StageStateAwaitingApproval {
				t.Errorf("review stage = %s, want awaiting_approval (untouched)", got)
			}
			if n := len(e.rows(r.ID, stalesweep.Category)); n != 0 {
				t.Errorf("rows = %d, want 0", n)
			}
		})
	}
}

// TestSweepStaleRuns_LiveRunnerNeverTouched: a stale run the probe attributes
// to a live fishhawk-runner is skipped and unchanged after --apply.
func TestSweepStaleRuns_LiveRunnerNeverTouched(t *testing.T) {
	e := newSweepEnv(t)
	r, review := e.staleAbandoned()
	probe := stubProbe{rep: stalesweep.ProbeReport{RunIDs: map[uuid.UUID]bool{r.ID: true}}}
	var out, errOut bytes.Buffer
	if got := runSweepStaleRunsWith([]string{"--db", e.url, "--apply"}, &out, &errOut, probe); got != exitOK {
		t.Fatalf("apply exit = %d; stderr %s", got, errOut.String())
	}
	if !strings.Contains(out.String(), "run="+r.ID.String()+" state=running class=live_runner action=skip") ||
		!strings.Contains(out.String(), "applied: 0 run(s)") {
		t.Errorf("apply output:\n%s", out.String())
	}
	if got := e.state(r.ID); got != runpkg.StateRunning {
		t.Errorf("state = %s, want running", got)
	}
	if got := e.stageState(review.ID); got != runpkg.StageStateAwaitingApproval {
		t.Errorf("review stage = %s, want awaiting_approval", got)
	}
	if n := len(e.rows(r.ID, stalesweep.Category)); n != 0 {
		t.Errorf("rows = %d, want 0", n)
	}
}

// TestReportSweep_ExitCodes: a failed run, and a transitioned run carrying a
// cascade/append error, each make --apply exit 1; skips and clean transitions
// exit 0.
func TestReportSweep_ExitCodes(t *testing.T) {
	ok := stalesweep.Result{RunID: uuid.New(), Outcome: stalesweep.OutcomeTransitioned,
		TransitionPath: []runpkg.State{runpkg.StateCancelled}}
	skip := stalesweep.Result{RunID: uuid.New(), Outcome: stalesweep.OutcomeSkippedChanged}
	failed := stalesweep.Result{RunID: uuid.New(), Outcome: stalesweep.OutcomeFailed, Err: errors.New("transition boom")}
	appendErr := stalesweep.Result{RunID: uuid.New(), Outcome: stalesweep.OutcomeTransitioned, Err: errors.New("append boom")}
	for name, tc := range map[string]struct {
		in   []stalesweep.Result
		want int
		line string
	}{
		"clean":        {[]stalesweep.Result{ok, skip}, exitOK, "transitioned=1 skipped_terminal=0 skipped_changed=1 failed=0 errors=0"},
		"failed run":   {[]stalesweep.Result{ok, failed}, exitFailure, "failed=1 errors=1"},
		"append error": {[]stalesweep.Result{appendErr}, exitFailure, "transitioned=1 skipped_terminal=0 skipped_changed=0 failed=0 errors=1"},
	} {
		t.Run(name, func(t *testing.T) {
			var out strings.Builder
			if got := reportSweep(&out, tc.in); got != tc.want || !strings.Contains(out.String(), tc.line) {
				t.Errorf("exit = %d, out:\n%s\nwant %d and %q", got, out.String(), tc.want, tc.line)
			}
		})
	}
}
