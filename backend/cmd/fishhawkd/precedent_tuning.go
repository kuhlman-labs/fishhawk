package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/decisionindex"
	"github.com/kuhlman-labs/fishhawk/backend/internal/postgres"
	"github.com/kuhlman-labs/fishhawk/backend/internal/precedent"
)

const precedentTuningUsage = "Usage: fishhawkd precedent-tuning --repo <owner/name> [--db <url>] [--min-decisions 3,5,8] [--min-agreement 0.7,0.8,0.9] [--window 90d,180d,365d] [--classes concern_waive,concern_defer,plan_approval] [--limit N]"

// tuningRowLoader reads the decision-index rows the replay runs over. It is a
// seam so the command's parsing, grid and report are testable without a
// database; production wires loadTuningRows.
type tuningRowLoader func(ctx context.Context, dbURL, repo string, classes []decisionindex.DecisionClass, limit int) ([]decisionindex.Row, error)

// runPrecedentTuning is the operator entry point for the E75.5 / #3733
// divergence-threshold tuning report. Logic lives in
// backend/internal/precedent (Replay).
//
//	fishhawkd precedent-tuning --repo owner/name [--db <url>] [--min-decisions 3,5,8]
//	    [--min-agreement 0.7,0.8,0.9] [--window 90d,180d,365d] [--classes ...] [--limit N]
//
// It replays the divergence rule over the repository's recorded decision
// history for every (N, X, window) cell of the grid and prints one line per
// candidate, quietest first. The report is printed to stdout; usage errors and
// failures go to logSink.
func runPrecedentTuning(args []string, logSink io.Writer) int {
	return runPrecedentTuningWith(args, os.Stdout, logSink, loadTuningRows)
}

func runPrecedentTuningWith(args []string, out, logSink io.Writer, load tuningRowLoader) int {
	fs := flag.NewFlagSet("fishhawkd precedent-tuning", flag.ContinueOnError)
	fs.SetOutput(logSink)
	dbURL := fs.String("db", envOr("FISHHAWKD_DATABASE_URL", ""), "postgres URL")
	repo := fs.String("repo", "", "repository (owner/name) whose decision history to replay (required)")
	minDecisions := fs.String("min-decisions", "3,5,8", "comma-separated candidate N values (fewest prior human decisions)")
	minAgreement := fs.String("min-agreement", "0.7,0.8,0.9", "comma-separated candidate X values in (0, 1] (modal outcome's least share)")
	window := fs.String("window", "90d,180d,365d", "comma-separated candidate recency windows (180d or a Go duration)")
	classes := fs.String("classes", "", "comma-separated decision classes to examine (default: the whole closed allow-list)")
	limit := fs.Int("limit", 10000, "maximum decision-index rows read per decision class, newest first (0 = unbounded)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	usageErr := func(format string, a ...any) int {
		_, _ = fmt.Fprintf(logSink, "fishhawkd precedent-tuning: "+format+"\n", a...)
		_, _ = fmt.Fprintln(logSink, precedentTuningUsage)
		return exitUsage
	}
	if fs.NArg() > 0 {
		return usageErr("unexpected argument %q", fs.Arg(0))
	}
	if strings.TrimSpace(*repo) == "" {
		return usageErr("--repo is required (the report replays ONE repository's decision history)")
	}
	if *limit < 0 {
		return usageErr("--limit must be >= 0, got %d", *limit)
	}

	grid, selected, err := buildTuningGrid(*minDecisions, *minAgreement, *window, *classes)
	if err != nil {
		return usageErr("%v", err)
	}
	if *dbURL == "" {
		return usageErr("--db or FISHHAWKD_DATABASE_URL is required")
	}

	logger := newLogger(logSink)
	rows, err := load(context.Background(), *dbURL, *repo, readClasses(selected), *limit)
	if err != nil {
		logger.Error("precedent-tuning: read decision index failed", slog.String("error", err.Error()))
		return exitFailure
	}
	if len(rows) == 0 {
		// A typo'd --repo or an un-backfilled index would otherwise print a
		// report of all-zero candidates that reads as "never fires".
		_, _ = fmt.Fprintf(logSink, "fishhawkd precedent-tuning: no decision-index rows for repo %q in classes %v — check --repo, or run `fishhawkd decision-index backfill`\n", *repo, readClasses(selected))
		return exitFailure
	}
	writeTuningReport(out, *repo, len(rows), precedent.Replay(rows, grid))
	return exitOK
}

// errEmptyGrid is the refusal for a threshold list that names no value. Every
// grid cell takes one value from each list, so an empty list is the only way
// to an empty grid — which would replay nothing and print an empty, silently
// meaningless report.
var errEmptyGrid = errors.New("empty grid")

// buildTuningGrid parses the three threshold lists and the class list into the
// cartesian grid of candidate configurations, each validated through
// precedent.NewDivergenceConfig. Any malformed or out-of-range value, an empty
// list, or a class outside the closed allow-list is an error: a report built
// from input it could not parse would print vacuous zeros.
func buildTuningGrid(minDecisions, minAgreement, window, classes string) ([]precedent.DivergenceConfig, []string, error) {
	ns, err := parseTuningList("--min-decisions", minDecisions, func(s string) (int, error) {
		return strconv.Atoi(s)
	})
	if err != nil {
		return nil, nil, err
	}
	xs, err := parseTuningList("--min-agreement", minAgreement, func(s string) (float64, error) {
		return strconv.ParseFloat(s, 64)
	})
	if err != nil {
		return nil, nil, err
	}
	ws, err := parseTuningList("--window", window, precedent.ParseWindow)
	if err != nil {
		return nil, nil, err
	}
	var cls []string
	if strings.TrimSpace(classes) != "" {
		cls = strings.Split(classes, ",")
	}

	grid := make([]precedent.DivergenceConfig, 0, len(ns)*len(xs)*len(ws))
	var selected []string
	for _, n := range ns {
		for _, x := range xs {
			for _, w := range ws {
				cfg, err := precedent.NewDivergenceConfig(true, n, x, w, cls)
				if err != nil {
					return nil, nil, err
				}
				selected = cfg.AllowedClasses
				grid = append(grid, cfg)
			}
		}
	}
	if len(selected) == 0 {
		selected = precedent.DivergenceClasses()
	}
	return grid, selected, nil
}

// parseTuningList splits a comma-separated threshold list. An empty list or an
// empty element is refused (errEmptyGrid / a named element error), never
// skipped.
func parseTuningList[T any](flagName, raw string, parse func(string) (T, error)) ([]T, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, fmt.Errorf("%w: %s names no value", errEmptyGrid, flagName)
	}
	parts := strings.Split(raw, ",")
	out := make([]T, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			return nil, fmt.Errorf("%s %q has an empty element", flagName, raw)
		}
		v, err := parse(p)
		if err != nil {
			return nil, fmt.Errorf("%s: cannot parse %q: %v", flagName, p, err)
		}
		out = append(out, v)
	}
	return out, nil
}

// readClasses is the set of classes whose rows the replay needs: each
// selected class's comparison classes (a concern waive's precedent includes
// prior defers, and vice versa).
func readClasses(selected []string) []decisionindex.DecisionClass {
	seen := map[decisionindex.DecisionClass]struct{}{}
	var out []decisionindex.DecisionClass
	for _, c := range selected {
		for _, cc := range precedent.ComparisonClasses(decisionindex.DecisionClass(c)) {
			if _, ok := seen[cc]; ok {
				continue
			}
			seen[cc] = struct{}{}
			out = append(out, cc)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// loadTuningRows reads the repository's rows for each class through the
// existing decisionindex.Store.List — un-scoped by account, the deployment
// posture the backfill and `decision-index check` also run under.
func loadTuningRows(ctx context.Context, dbURL, repo string, classes []decisionindex.DecisionClass, limit int) ([]decisionindex.Row, error) {
	pool, err := postgres.Connect(ctx, dbURL)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	defer pool.Close()
	store := decisionindex.NewStore(pool)
	var rows []decisionindex.Row
	for _, c := range classes {
		got, err := store.List(ctx, decisionindex.ListFilter{
			Repo: repo, DecisionClass: c, Limit: limit, Newest: true,
		})
		if err != nil {
			return nil, err
		}
		rows = append(rows, got...)
	}
	return rows, nil
}

// writeTuningReport prints one line per candidate, sorted by fire rate
// ascending (quietest first), ties in grid order.
func writeTuningReport(out io.Writer, repo string, rowsRead int, results []precedent.TuningResult) {
	sort.SliceStable(results, func(i, j int) bool { return results[i].FireRate < results[j].FireRate })
	_, _ = fmt.Fprintf(out, "precedent-tuning repo=%s rows_read=%d candidates=%d\n", repo, rowsRead, len(results))
	for _, r := range results {
		nf := r.NotFired
		_, _ = fmt.Fprintf(out,
			"min_decisions=%d min_agreement=%.2f window=%s examined=%d fired=%d fire_rate=%.4f below_min_decisions=%d outside_window=%d doctrine_version_mismatch=%d below_min_agreement=%d agreed_with_precedent=%d\n",
			r.Config.MinDecisions, r.Config.MinAgreement, formatWindow(r.Config.Window),
			r.DecisionsExamined, r.WouldHaveFired, r.FireRate,
			nf.BelowMinDecisions, nf.OutsideWindow, nf.DoctrineVersionMismatch, nf.BelowMinAgreement, nf.AgreedWithPrecedent)
	}
}

// formatWindow renders a whole-day window as "Nd" (the input form) and any
// other duration as a Go duration.
func formatWindow(d time.Duration) string {
	day := 24 * time.Hour
	if d%day == 0 {
		return strconv.FormatInt(int64(d/day), 10) + "d"
	}
	return d.String()
}
