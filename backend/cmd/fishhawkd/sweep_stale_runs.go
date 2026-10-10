package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/postgres"
	runpkg "github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/stalesweep"
)

const sweepStaleRunsUsage = "Usage: fishhawkd sweep-stale-runs [--db <url>] [--days 14] [--apply] [--cancel-unobserved-pr]"

// sweepStaleRunsTimeout bounds the whole one-shot scan + apply.
const sweepStaleRunsTimeout = 5 * time.Minute

// runSweepStaleRuns is the one-shot sweep of stale non-terminal TOP-LEVEL runs
// (#4185):
//
//	fishhawkd sweep-stale-runs [--db <url>] [--days 14] [--apply] [--cancel-unobserved-pr]
//
// It classifies every pending/running run with no decomposition parent
// (stalesweep.Find) and prints one line per candidate. The default is a DRY
// RUN that writes nothing. With --apply it transitions the candidates whose
// class calls for it through the run state machine (stalesweep.Apply), but
// only after the host live-runner probe succeeded and attributed every live
// fishhawk-runner process: a probe failure or an unattributed runner refuses
// --apply before any write. Exit 0 on success (including a dry run), 1 on a
// connect/scan failure, an apply refusal, or any run that failed to transition
// or whose row failed to append, 2 on a usage error.
func runSweepStaleRuns(args []string, logSink io.Writer) int {
	return runSweepStaleRunsWith(args, os.Stdout, logSink, stalesweep.PSProbe{})
}

func runSweepStaleRunsWith(args []string, out, logSink io.Writer, probe stalesweep.RunnerProbe) int {
	fs := flag.NewFlagSet("fishhawkd sweep-stale-runs", flag.ContinueOnError)
	fs.SetOutput(logSink)
	dbURL := fs.String("db", envOr("FISHHAWKD_DATABASE_URL", ""), "postgres URL")
	days := fs.Int("days", 14, "inactivity threshold in days; a run with any activity newer than this is skipped")
	apply := fs.Bool("apply", false, "transition the listed candidates (default: dry run, writes nothing)")
	cancelUnobserved := fs.Bool("cancel-unobserved-pr", false, "cancel runs whose PR was opened but never observed merged or closed (default: skip them)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() > 0 {
		_, _ = fmt.Fprintf(logSink, "fishhawkd sweep-stale-runs: unexpected argument %q\n%s\n", fs.Arg(0), sweepStaleRunsUsage)
		return exitUsage
	}
	if *days < 1 {
		_, _ = fmt.Fprintf(logSink, "fishhawkd sweep-stale-runs: --days must be >= 1, got %d\n%s\n", *days, sweepStaleRunsUsage)
		return exitUsage
	}
	if *dbURL == "" {
		_, _ = fmt.Fprintf(logSink, "fishhawkd sweep-stale-runs: --db or FISHHAWKD_DATABASE_URL is required\n%s\n", sweepStaleRunsUsage)
		return exitUsage
	}

	ctx, cancel := context.WithTimeout(context.Background(), sweepStaleRunsTimeout)
	defer cancel()
	pool, err := postgres.Connect(ctx, *dbURL)
	if err != nil {
		_, _ = fmt.Fprintf(logSink, "fishhawkd sweep-stale-runs: connect: %v\n", err)
		return exitFailure
	}
	defer pool.Close()
	runs, err := sweepRunStore(runpkg.NewPostgresRepository(pool))
	if err != nil {
		_, _ = fmt.Fprintf(logSink, "fishhawkd sweep-stale-runs: %v\n", err)
		return exitFailure
	}
	au := audit.NewPostgresRepository(pool)

	opts := stalesweep.Options{
		Threshold:          time.Duration(*days) * 24 * time.Hour,
		CancelUnobservedPR: *cancelUnobserved,
	}
	cands, rep, err := stalesweep.Find(ctx, runs, au, probe, opts)
	if err != nil {
		_, _ = fmt.Fprintf(logSink, "fishhawkd sweep-stale-runs: scan: %v\n", err)
		return exitFailure
	}

	if rep.Err != nil {
		_, _ = fmt.Fprintf(out, "runner-probe: unavailable (%v); no run is classified live_runner\n", rep.Err)
	}
	if len(rep.UnattributedPIDs) > 0 {
		_, _ = fmt.Fprintf(out, "runner-probe: fishhawk-runner pid(s) %s carry no attributable --run-id <uuid>\n", strings.Join(rep.UnattributedPIDs, ","))
	}
	counts := map[stalesweep.Class]int{}
	transitioning := 0
	for _, c := range cands {
		counts[c.Class]++
		if c.Action.Transitions() {
			transitioning++
		}
		printCandidate(out, c)
	}

	if !*apply {
		parts := make([]string, 0, len(stalesweep.Classes))
		for _, cl := range stalesweep.Classes {
			parts = append(parts, fmt.Sprintf("%s=%d", cl, counts[cl]))
		}
		_, _ = fmt.Fprintf(out, "dry-run: %d candidate(s): %s; re-run with --apply to transition %d\n", len(cands), strings.Join(parts, " "), transitioning)
		return exitOK
	}

	if rep.Err != nil {
		_, _ = fmt.Fprintf(logSink, "fishhawkd sweep-stale-runs: --apply refused: the live-runner probe failed (%v); run it from a host with ps and database access\n", rep.Err)
		return exitFailure
	}
	if len(rep.UnattributedPIDs) > 0 {
		_, _ = fmt.Fprintf(logSink, "fishhawkd sweep-stale-runs: --apply refused: live fishhawk-runner pid(s) %s carry no attributable --run-id; a stale run may still be driven by one\n", strings.Join(rep.UnattributedPIDs, ","))
		return exitFailure
	}

	return reportSweep(out, stalesweep.Apply(ctx, runs, au, cands, opts))
}

// sweepRunStore narrows a run repository to stalesweep.RunStore. The stage
// compare-and-swap (run.StageCASTransitioner) is an optional capability kept
// off run.Repository; the Postgres repository carries it (compile-time
// asserted in backend/internal/run), and a repository without it is refused
// rather than swept without the stage cancels.
func sweepRunStore(r runpkg.Repository) (stalesweep.RunStore, error) {
	rs, ok := r.(stalesweep.RunStore)
	if !ok {
		return nil, fmt.Errorf("run repository %T lacks the stage compare-and-swap (run.StageCASTransitioner)", r)
	}
	return rs, nil
}

// printCandidate prints one classification line, plus a remedy line for a
// delegated merged run or a skipped pr_unobserved run.
func printCandidate(out io.Writer, c stalesweep.Candidate) {
	target := "-"
	if c.Target != "" {
		target = string(c.Target)
	}
	stages := make([]string, 0, len(c.Stages))
	for _, s := range c.Stages {
		stages = append(stages, string(s.Type)+":"+string(s.State))
	}
	stageList := "-"
	if len(stages) > 0 {
		stageList = strings.Join(stages, ",")
	}
	line := fmt.Sprintf("run=%s state=%s class=%s action=%s target=%s last_activity=%s stages=%s pr=%s",
		c.Run.ID, c.Run.State, c.Class, c.Action, target, c.LastActivity.UTC().Format(time.RFC3339), stageList, c.PREvidence)
	if c.PullRequestURL != "" {
		line += " pr_url=" + c.PullRequestURL
	}
	_, _ = fmt.Fprintln(out, line)
	switch {
	case c.Class == stalesweep.ClassMerged:
		_, _ = fmt.Fprintf(out, "  remedy: fishhawk_reconcile_merge / POST /v0/runs/%s/reconcile-merge\n", c.Run.ID)
	case c.Class == stalesweep.ClassPRUnobserved && c.Action == stalesweep.ActionSkip:
		_, _ = fmt.Fprintln(out, "  remedy: fishhawk_record_merge_observation then reconcile-merge, or re-run with --cancel-unobserved-pr")
	}
}

// reportSweep prints one line per applied run plus a summary, and returns
// exitFailure when any run failed OR transitioned with an error (a cascade or
// append failure: the state change landed, something after it did not).
func reportSweep(out io.Writer, results []stalesweep.Result) int {
	counts := map[stalesweep.Outcome]int{}
	withErr := 0
	for _, r := range results {
		counts[r.Outcome]++
		path := make([]string, 0, len(r.TransitionPath))
		for _, s := range r.TransitionPath {
			path = append(path, string(s))
		}
		pathStr := "-"
		if len(path) > 0 {
			pathStr = strings.Join(path, ",")
		}
		line := fmt.Sprintf("applied run=%s class=%s outcome=%s from=%s to=%s path=%s stages_cancelled=%d children_cancelled=%d",
			r.RunID, r.Class, r.Outcome, r.FromState, r.ToState, pathStr, len(r.StagesCancelled), r.ChildrenCancelled)
		if r.Err != nil {
			line += " error=" + r.Err.Error()
			withErr++
		}
		_, _ = fmt.Fprintln(out, line)
	}
	_, _ = fmt.Fprintf(out, "applied: %d run(s): transitioned=%d skipped_terminal=%d skipped_changed=%d failed=%d errors=%d\n",
		len(results), counts[stalesweep.OutcomeTransitioned], counts[stalesweep.OutcomeSkippedTerminal],
		counts[stalesweep.OutcomeSkippedChanged], counts[stalesweep.OutcomeFailed], withErr)
	if counts[stalesweep.OutcomeFailed] > 0 || withErr > 0 {
		return exitFailure
	}
	return exitOK
}
