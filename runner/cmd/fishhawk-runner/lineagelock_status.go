package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"
)

// lineagelock_status.go is the read-only `fishhawk-runner lineage-lock-status`
// subcommand (#5485). The host dispatcher execs it before it spawns a stage, so
// a stage whose run lineage lock is still held by a live runner (an acceptance
// runner tearing down its preview after its stage already reads succeeded) is
// deferred instead of being spawned into category C lineage_lock_contention.
//
// It answers with the runner's OWN lock rules: lineageRoot for the root,
// lineageLockPath for the path, and readLockPID / readLockRunID /
// processAlive for the holder, so the backend module carries no copy of them.

// lineageLockStatusSubcommand is the argv[1] run() routes here. The backend's
// hostDispatchLineageLockSubcommand byte-matches it.
const lineageLockStatusSubcommand = "lineage-lock-status"

// lineageLockStatusTimeout bounds the git common-dir read.
const lineageLockStatusTimeout = 10 * time.Second

// lineageLockStatus is the one JSON line the subcommand prints to stdout.
type lineageLockStatus struct {
	Root        string `json:"root"`
	LockPath    string `json:"lock_path"`
	Present     bool   `json:"present"`
	Held        bool   `json:"held"`
	HolderPID   int    `json:"holder_pid"`
	HolderRunID string `json:"holder_run_id"`
}

// runLineageLockStatus reports whether the lineage lock a stage spawn would
// take is held by a live process. Exit codes: exitOK with one JSON verdict on
// stdout; exitFailure (nothing on stdout, a lineage_lock_status_failed line on
// logSink) when the lock path cannot be resolved, e.g. --working-dir is not a
// git checkout, or the lockfile cannot be stat'ed; exitUsage (nothing on
// stdout) for a missing required flag, a stray positional or a parse error.
//
// It is strictly read-only: it never creates the fishhawk-worktrees dir or the
// lock, never removes or reclaims a stale lock, never contacts the backend and
// never signals a process (processAlive's signal 0 only checks existence).
func runLineageLockStatus(args []string, stdout, logSink io.Writer) int {
	fs := flag.NewFlagSet(lineageLockStatusSubcommand, flag.ContinueOnError)
	fs.SetOutput(logSink)
	workingDir := fs.String("working-dir", "", "the operator checkout the stage spawn passes (required)")
	runID := fs.String("run-id", "", "the run whose stage would spawn (required)")
	decomposedFrom := fs.String("decomposed-from", "", "the parent run id of a decomposed child (optional)")
	parallelIsolate := fs.Bool("parallel-isolate", false, "the child keys its lineage on its own run id, as --parallel-isolate does for a stage")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	usage := func(detail string) int {
		logEvent(logSink, "lineage_lock_status_usage", map[string]string{"error": detail})
		return exitUsage
	}
	if fs.NArg() > 0 {
		return usage(fmt.Sprintf("unexpected positional argument %q", fs.Arg(0)))
	}
	if *workingDir == "" {
		return usage("--working-dir is required")
	}
	if *runID == "" {
		return usage("--run-id is required")
	}

	root := lineageRoot(*runID, *decomposedFrom, *parallelIsolate)
	ctx, cancel := context.WithTimeout(context.Background(), lineageLockStatusTimeout)
	defer cancel()
	fail := func(err error) int {
		logEvent(logSink, "lineage_lock_status_failed", map[string]string{"root": root, "detail": err.Error()})
		return exitFailure
	}
	lockPath, err := lineageLockPath(ctx, *workingDir, root)
	if err != nil {
		return fail(err)
	}
	present := true
	if _, err := os.Lstat(lockPath); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return fail(fmt.Errorf("stat lineage lock: %w", err))
		}
		present = false
	}
	pid := 0
	holderRunID := ""
	if present {
		pid = readLockPID(lockPath)
		holderRunID = readLockRunID(lockPath)
	}
	// The live-holder predicate acquireLineageLock applies: an empty or
	// unparseable pid reads not held, as acquire would reclaim it.
	held := present && pid > 0 && processAlive(pid)
	out, err := json.Marshal(lineageLockStatus{
		Root: root, LockPath: lockPath, Present: present, Held: held,
		HolderPID: pid, HolderRunID: holderRunID,
	})
	if err != nil {
		return fail(err)
	}
	_, _ = fmt.Fprintf(stdout, "%s\n", out)
	return exitOK
}
