package main

// Agent-kill checkpoint (#5147). An implement agent killed at its timeout (or
// by the loop / budget detectors, or by runner cancellation) may already have
// finished its work in the working tree without committing it. Every
// working_tree_restored defer force-checks-out the operator's pre-agent ref at
// run() return, which discards that tree. checkpointKilledAgentTree runs inline
// right after the primary agent returns, so it structurally precedes every
// restore, and pins the full working tree (tracked edits plus untracked,
// non-ignored files) as a commit under a create-only local ref:
//
//	refs/fishhawk/checkpoints/<run>/<stage>-timeout-<n>
//	refs/fishhawk/checkpoints/<run>/<stage>-killed-<n>
//
// The suffixes are LEAF siblings of -stash / -verify / -fixup-: a child path
// <stage>/timeout-<n> would be a git directory/file conflict with the existing
// <run>/<stage> held-commit leaf ref (#4079). The snapshot is built in a
// private index, so the worktree and its real index are never written. The
// whole step is fail-open: a skip or a failure logs and never changes the
// stage reason, category or exit.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/kuhlman-labs/fishhawk/runner/internal/agent"
	"github.com/kuhlman-labs/fishhawk/runner/internal/gitops"
)

const (
	// checkpointTimeoutSuffix and checkpointKilledSuffix are the leaf suffixes
	// on checkpointRef(run, stage) for an agent-kill checkpoint; a numeric
	// counter follows so repeated kills never clobber an earlier pin.
	checkpointTimeoutSuffix = "-timeout-"
	checkpointKilledSuffix  = "-killed-"

	// agentKillCheckpointTimeout bounds the pin, run under a context detached
	// from the (possibly cancelled) runner context.
	agentKillCheckpointTimeout = 30 * time.Second

	// agentKillPinAttempts bounds the n+1 retry on a must-not-exist refusal.
	agentKillPinAttempts = 4
)

// agentKillCheckpointKind maps the primary invocation's error (and the runner
// context's error) to the checkpoint kind: "timeout" for agent.ErrTimeout,
// "killed" for a loop / budget kill or a runner cancellation, "" otherwise.
func agentKillCheckpointKind(invokeErr, ctxErr error) string {
	switch {
	case errors.Is(invokeErr, agent.ErrTimeout):
		return "timeout"
	case errors.Is(invokeErr, agent.ErrLoopDetected), errors.Is(invokeErr, agent.ErrBudgetExceeded), ctxErr != nil:
		return "killed"
	}
	return ""
}

// agentKillPin is one written agent-kill checkpoint.
type agentKillPin struct {
	ref, commitSHA, treeSHA, parentSHA string
}

// agentKillCheckpointPin writes the checkpoint; a seam so a run() test never
// writes refs into the runner's own source repo. It returns pinned=false with
// a skip reason in the error-free skip case (see pinAgentKillCheckpoint).
var agentKillCheckpointPin = pinAgentKillCheckpoint

// errAgentKillSkip carries a skip reason (pinned=false, nothing written).
type errAgentKillSkip struct{ reason string }

func (e errAgentKillSkip) Error() string { return e.reason }

// pinAgentKillCheckpoint snapshots repoDir's working tree into a commit
// parented on HEAD and pins it create-only. It returns (pin, true, nil) on a
// write, (zero, false, errAgentKillSkip) for head_unresolvable / clean_tree,
// and (zero, false, err) on a git failure.
func pinAgentKillCheckpoint(ctx context.Context, repoDir, runID, stageID, kind string) (agentKillPin, bool, error) {
	head, err := akGit(ctx, repoDir, nil, "rev-parse", "--verify", "--quiet", "HEAD^{commit}")
	if err != nil || head == "" {
		return agentKillPin{}, false, errAgentKillSkip{reason: "head_unresolvable"}
	}
	headTree, err := akGit(ctx, repoDir, nil, "rev-parse", "HEAD^{tree}")
	if err != nil {
		return agentKillPin{}, false, fmt.Errorf("resolve HEAD tree: %w", err)
	}

	tmpDir, err := os.MkdirTemp("", "fishhawk-agent-kill-index-")
	if err != nil {
		return agentKillPin{}, false, fmt.Errorf("private index dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()
	tmpIndex := filepath.Join(tmpDir, "index")
	indexEnv := []string{"GIT_INDEX_FILE=" + tmpIndex}

	realIndex, err := akGit(ctx, repoDir, nil, "rev-parse", "--git-path", "index")
	if err != nil {
		return agentKillPin{}, false, fmt.Errorf("locate index: %w", err)
	}
	if !filepath.IsAbs(realIndex) {
		realIndex = filepath.Join(repoDir, realIndex)
	}
	if data, rerr := os.ReadFile(realIndex); rerr == nil {
		if werr := os.WriteFile(tmpIndex, data, 0o600); werr != nil {
			return agentKillPin{}, false, fmt.Errorf("copy index: %w", werr)
		}
	} else if _, err := akGit(ctx, repoDir, indexEnv, "read-tree", "HEAD"); err != nil {
		return agentKillPin{}, false, fmt.Errorf("seed private index: %w", err)
	}

	// Stage tracked edits, deletions and untracked non-ignored files into the
	// PRIVATE index only; .gitignore'd files (e.g. .env) are excluded.
	if _, err := akGit(ctx, repoDir, indexEnv, "add", "-A", "--"); err != nil {
		return agentKillPin{}, false, fmt.Errorf("stage working tree: %w", err)
	}
	tree, err := akGit(ctx, repoDir, indexEnv, "write-tree")
	if err != nil {
		return agentKillPin{}, false, fmt.Errorf("write-tree: %w", err)
	}
	if tree == headTree {
		return agentKillPin{}, false, errAgentKillSkip{reason: "clean_tree"}
	}
	commit, err := akGit(ctx, repoDir, gitops.IdentityEnv(gitops.DefaultAuthorName, gitops.DefaultAuthorEmail),
		"commit-tree", "--no-gpg-sign", tree, "-p", head,
		"-m", fmt.Sprintf("fishhawk agent %s checkpoint %s/%s", kind, runID, stageID))
	if err != nil {
		return agentKillPin{}, false, fmt.Errorf("commit-tree: %w", err)
	}

	suffix := checkpointTimeoutSuffix
	if kind != "timeout" {
		suffix = checkpointKilledSuffix
	}
	prefix := checkpointRef(runID, stageID) + suffix
	n, err := nextAgentKillIndex(ctx, repoDir, prefix)
	if err != nil {
		return agentKillPin{}, false, err
	}
	var lastErr error
	for attempt := 0; attempt < agentKillPinAttempts; attempt++ {
		ref := prefix + strconv.Itoa(n+attempt)
		// An empty old value means the ref must not exist (git-update-ref(1)):
		// create-only, so an earlier pin is never clobbered.
		out, uerr := checkpointGit(ctx, repoDir, "update-ref", ref, commit, "")
		if uerr == nil {
			return agentKillPin{ref: ref, commitSHA: commit, treeSHA: tree, parentSHA: head}, true, nil
		}
		lastErr = fmt.Errorf("git update-ref %s: %w (%s)", ref, uerr, strings.TrimSpace(string(out)))
	}
	return agentKillPin{}, false, lastErr
}

// nextAgentKillIndex returns 1 + the highest numeric suffix among the existing
// refs under prefix (1 when none exist).
func nextAgentKillIndex(ctx context.Context, repoDir, prefix string) (int, error) {
	out, err := akGit(ctx, repoDir, nil, "for-each-ref", "--format=%(refname)", prefix+"*")
	if err != nil {
		return 0, fmt.Errorf("list checkpoint refs: %w", err)
	}
	maxN := 0
	for _, line := range strings.Split(out, "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), prefix)
		if !ok {
			continue
		}
		if v, cerr := strconv.Atoi(rest); cerr == nil && v > maxN {
			maxN = v
		}
	}
	return maxN + 1, nil
}

// akGit runs `git -C repoDir <args>` with extraEnv layered on the process env
// and returns trimmed stdout.
func akGit(ctx context.Context, repoDir string, extraEnv []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", repoDir}, args...)...)
	cmd.Env = append(os.Environ(), extraEnv...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w (%s)", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(string(out)), nil
}

// checkpointKilledAgentTree pins an implement agent's working tree when the
// primary invocation failed by timeout, loop / budget kill or runner
// cancellation (#5147). It is a no-op for any other stage, for --no-pr, for a
// successful result and for an ordinary agent failure. On a pin it logs
// agent_kill_checkpoint_pinned, appends the same event to res.Events and
// appends the pinned clause as the LAST clause of res.FailureReason, so the
// leading `agent timeout after …` token stays intact for classifiers. A skip
// or a failure only logs; both are fail-open.
func checkpointKilledAgentTree(ctx context.Context, cfg config, stageType string, res *agent.Result, invokeErr error, logSink io.Writer) {
	if stageType != "implement" || cfg.noPR || res == nil || res.OK {
		return
	}
	kind := agentKillCheckpointKind(invokeErr, ctx.Err())
	if kind == "" {
		return
	}
	repoDir := cfg.workingDir
	if repoDir == "" {
		repoDir = "."
	}
	pinCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), agentKillCheckpointTimeout)
	defer cancel()
	pin, pinned, err := agentKillCheckpointPin(pinCtx, repoDir, cfg.runID, cfg.stageID, kind)
	var skip errAgentKillSkip
	switch {
	case errors.As(err, &skip):
		_, _ = fmt.Fprintf(logSink,
			`{"event":"agent_kill_checkpoint_skipped","run_id":%q,"stage_id":%q,"kind":%q,"reason":%q}`+"\n",
			cfg.runID, cfg.stageID, kind, skip.reason)
		return
	case err != nil || !pinned:
		detail := "not pinned"
		if err != nil {
			detail = err.Error()
		}
		_, _ = fmt.Fprintf(logSink,
			`{"event":"agent_kill_checkpoint_failed","run_id":%q,"stage_id":%q,"kind":%q,"detail":%q}`+"\n",
			cfg.runID, cfg.stageID, kind, detail)
		return
	}
	_, _ = fmt.Fprintf(logSink,
		`{"event":"agent_kill_checkpoint_pinned","run_id":%q,"stage_id":%q,"kind":%q,"ref":%q,"commit_sha":%q,"tree_sha":%q,"parent_sha":%q}`+"\n",
		cfg.runID, cfg.stageID, kind, pin.ref, pin.commitSHA, pin.treeSHA, pin.parentSHA)
	res.Events = append(res.Events, agent.Event{
		Kind:      "agent_kill_checkpoint_pinned",
		Timestamp: time.Now(),
		Payload: agent.MakePayload(map[string]string{
			"run_id":     cfg.runID,
			"stage_id":   cfg.stageID,
			"kind":       kind,
			"ref":        pin.ref,
			"commit_sha": pin.commitSHA,
			"tree_sha":   pin.treeSHA,
			"parent_sha": pin.parentSHA,
		}),
	})
	res.FailureReason += fmt.Sprintf("; uncommitted agent work pinned at %s (commit %s)", pin.ref, pin.commitSHA)
}
