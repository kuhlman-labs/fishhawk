package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/runner/internal/agent"
	"github.com/kuhlman-labs/fishhawk/runner/internal/gitops"
	"github.com/kuhlman-labs/fishhawk/runner/internal/upload"
)

const (
	akRunID   = "11111111-2222-3333-4444-555555555555"
	akStageID = "22222222-3333-4444-5555-666666666666"
)

// agentKillRepo builds a temp repo whose HEAD holds a.txt and a .gitignore
// ignoring ignored.txt, then dirties it BY CONSTRUCTION: a.txt modified,
// untracked new_test.go and ignored ignored.txt written, nothing staged.
func agentKillRepo(t *testing.T) (repo, head string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo = t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		cmd.Env = append(os.Environ(), gitops.IdentityEnv("t", "t@e")...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run("init", "-q", "--initial-branch=main")
	akWrite(t, repo, "a.txt", "original\n")
	akWrite(t, repo, ".gitignore", "ignored.txt\n")
	run("add", "-A")
	run("commit", "-q", "--no-gpg-sign", "-m", "seed")
	head = cprGit(t, repo, "rev-parse", "HEAD")
	akDirty(t, repo, "modified\n")
	return repo, head
}

func akWrite(t *testing.T, repo, rel, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo, rel), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// akDirty writes the agent's uncommitted work: a modified tracked file, an
// untracked new file and an ignored file.
func akDirty(t *testing.T, repo, aBody string) {
	t.Helper()
	akWrite(t, repo, "a.txt", aBody)
	akWrite(t, repo, "new_test.go", "package x\n")
	akWrite(t, repo, "ignored.txt", "SECRET=1\n")
}

func TestPinAgentKillCheckpoint_PinsTrackedAndUntrackedEdits(t *testing.T) {
	repo, head := agentKillRepo(t)
	pin, pinned, err := pinAgentKillCheckpoint(context.Background(), repo, akRunID, akStageID, "timeout")
	if err != nil || !pinned {
		t.Fatalf("pin = %+v, pinned = %v, err = %v; want a written pin", pin, pinned, err)
	}
	wantRef := checkpointRef(akRunID, akStageID) + "-timeout-1"
	if pin.ref != wantRef {
		t.Errorf("ref = %q, want %q", pin.ref, wantRef)
	}
	if sha, ok := refTarget(repo, pin.ref); !ok || sha != pin.commitSHA {
		t.Fatalf("ref %s resolves to %q (exists %v), want %s", pin.ref, sha, ok, pin.commitSHA)
	}
	if got := cprGit(t, repo, "show", pin.ref+":a.txt"); got != "modified" {
		t.Errorf("%s:a.txt = %q, want the modified bytes", pin.ref, got)
	}
	if got := cprGit(t, repo, "show", pin.ref+":new_test.go"); got != "package x" {
		t.Errorf("%s:new_test.go = %q, want the untracked file's bytes", pin.ref, got)
	}
	if err := exec.Command("git", "-C", repo, "cat-file", "-e", pin.ref+":ignored.txt").Run(); err == nil {
		t.Errorf("%s:ignored.txt exists; a .gitignore'd file must never be captured", pin.ref)
	}
	if got := cprGit(t, repo, "rev-parse", pin.ref+"^"); got != head || pin.parentSHA != head {
		t.Errorf("parent = %q (pin.parentSHA %q), want HEAD %s", got, pin.parentSHA, head)
	}
	// The worktree, real index and HEAD are untouched.
	if b, _ := os.ReadFile(filepath.Join(repo, "a.txt")); string(b) != "modified\n" {
		t.Errorf("worktree a.txt = %q, want still modified", b)
	}
	if _, err := os.Stat(filepath.Join(repo, "new_test.go")); err != nil {
		t.Errorf("worktree new_test.go: %v, want still present", err)
	}
	if got := cprGit(t, repo, "diff", "--cached", "--name-only"); got != "" {
		t.Errorf("real index staged %q, want nothing", got)
	}
	if got := cprGit(t, repo, "status", "--porcelain", "--untracked-files=all"); !strings.Contains(got, "?? new_test.go") {
		t.Errorf("status = %q, want new_test.go still untracked", got)
	}
	if got := cprGit(t, repo, "rev-parse", "HEAD"); got != head {
		t.Errorf("HEAD moved to %s, want %s", got, head)
	}
}

func TestPinAgentKillCheckpoint_SecondPinIncrements(t *testing.T) {
	repo, _ := agentKillRepo(t)
	ctx := context.Background()
	first, ok, err := pinAgentKillCheckpoint(ctx, repo, akRunID, akStageID, "timeout")
	if err != nil || !ok {
		t.Fatalf("first pin: ok %v err %v", ok, err)
	}
	akDirty(t, repo, "modified again\n")
	second, ok, err := pinAgentKillCheckpoint(ctx, repo, akRunID, akStageID, "timeout")
	if err != nil || !ok {
		t.Fatalf("second pin: ok %v err %v", ok, err)
	}
	if want := checkpointRef(akRunID, akStageID) + "-timeout-2"; second.ref != want {
		t.Errorf("second ref = %q, want %q", second.ref, want)
	}
	if sha, ok := refTarget(repo, first.ref); !ok || sha != first.commitSHA {
		t.Errorf("first ref %s = %q (exists %v), want its original %s (never clobbered)", first.ref, sha, ok, first.commitSHA)
	}
	if first.commitSHA == second.commitSHA {
		t.Error("second pin reused the first commit; the tree changed")
	}
}

func TestPinAgentKillCheckpoint_CleanTreeSkips(t *testing.T) {
	repo, head := agentKillRepo(t)
	cprGit(t, repo, "checkout", "--force", head)
	if err := os.Remove(filepath.Join(repo, "new_test.go")); err != nil {
		t.Fatal(err)
	}
	_, pinned, err := pinAgentKillCheckpoint(context.Background(), repo, akRunID, akStageID, "timeout")
	var skip errAgentKillSkip
	if pinned || !errors.As(err, &skip) || skip.reason != "clean_tree" {
		t.Fatalf("pinned = %v, err = %v; want a clean_tree skip", pinned, err)
	}
	if got := cprGit(t, repo, "for-each-ref", checkpointRefPrefix); got != "" {
		t.Errorf("refs written under %s: %q, want none", checkpointRefPrefix, got)
	}
}

func TestAgentKillCheckpointKind(t *testing.T) {
	cases := []struct {
		name      string
		invokeErr error
		ctxErr    error
		want      string
	}{
		{"timeout", agent.ErrTimeout, nil, "timeout"},
		{"wrapped timeout", fmt.Errorf("x: %w", agent.ErrTimeout), nil, "timeout"},
		{"loop", agent.ErrLoopDetected, nil, "killed"},
		{"budget", agent.ErrBudgetExceeded, nil, "killed"},
		{"runner cancel", agent.ErrAgentFailed, context.Canceled, "killed"},
		{"plain crash", agent.ErrAgentFailed, nil, ""},
		{"nil", nil, nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := agentKillCheckpointKind(tc.invokeErr, tc.ctxErr); got != tc.want {
				t.Errorf("agentKillCheckpointKind(%v, %v) = %q, want %q", tc.invokeErr, tc.ctxErr, got, tc.want)
			}
		})
	}
}

// TestCheckpointKilledAgentTree_RealPinSurvivesRestore is the F3 integrated
// regression: the run-level wrapper with the REAL pin on a temp repo (working
// dir from cfg), then a real force-checkout restore. The reported ref still
// resolves to a tree holding the agent's tracked and untracked work, and the
// reason carries the pinned clause. One row per kind.
func TestCheckpointKilledAgentTree_RealPinSurvivesRestore(t *testing.T) {
	for _, tc := range []struct {
		name      string
		invokeErr error
		suffix    string
	}{
		{"timeout", agent.ErrTimeout, "-timeout-1"},
		{"killed", agent.ErrLoopDetected, "-killed-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, head := agentKillRepo(t)
			cfg := config{runID: akRunID, stageID: akStageID, workingDir: repo}
			res := agent.Result{OK: false, FailureCategory: "A", FailureReason: "agent timeout after 1h0m0s"}
			var log strings.Builder
			checkpointKilledAgentTree(context.Background(), cfg, "implement", &res, tc.invokeErr, &log)

			wantRef := checkpointRef(akRunID, akStageID) + tc.suffix
			if !strings.Contains(log.String(), `"event":"agent_kill_checkpoint_pinned"`) || !strings.Contains(log.String(), wantRef) {
				t.Fatalf("log = %s, want agent_kill_checkpoint_pinned naming %s", log.String(), wantRef)
			}
			if !strings.HasPrefix(res.FailureReason, "agent timeout after 1h0m0s; uncommitted agent work pinned at "+wantRef+" (commit ") {
				t.Errorf("reason = %q, want the original token then the pinned clause for %s", res.FailureReason, wantRef)
			}
			// The force-checkout restore that working_tree_restored performs.
			if err := gitops.RestoreHead(context.Background(), repo, head); err != nil {
				t.Fatalf("restore: %v", err)
			}
			if b, _ := os.ReadFile(filepath.Join(repo, "a.txt")); string(b) != "original\n" {
				t.Fatalf("restore did not discard the tracked edit (a.txt = %q); the fixture no longer models the loss", b)
			}
			if _, ok := refTarget(repo, wantRef); !ok {
				t.Fatalf("ref %s does not resolve after the restore", wantRef)
			}
			if got := cprGit(t, repo, "show", wantRef+":a.txt"); got != "modified" {
				t.Errorf("%s:a.txt = %q, want the modified bytes", wantRef, got)
			}
			if got := cprGit(t, repo, "show", wantRef+":new_test.go"); got != "package x" {
				t.Errorf("%s:new_test.go = %q, want the untracked file", wantRef, got)
			}
		})
	}
}

// installAgentKillSpies wires the shared order log for the run()-seam tests:
// captureHead reports a moved HEAD after the pre-agent capture, restoreHead
// appends restore:<ref>, and the pin seam appends checkpoint:<kind> and
// returns pinErr or a fixed pin. Must be called AFTER withFakeGitOps.
func installAgentKillSpies(t *testing.T, pinErr error) *[]string {
	t.Helper()
	order := new([]string)
	origCap, origRes, origPin := captureHead, restoreHead, agentKillCheckpointPin
	calls := 0
	captureHead = func(_ context.Context, _ string) (string, bool, error) {
		calls++
		if calls == 1 {
			return "operator-main", false, nil
		}
		return "agent-branch", false, nil
	}
	restoreHead = func(_ context.Context, _, ref string) error {
		*order = append(*order, "restore:"+ref)
		return nil
	}
	agentKillCheckpointPin = func(_ context.Context, _, runID, stageID, kind string) (agentKillPin, bool, error) {
		*order = append(*order, "checkpoint:"+kind)
		if pinErr != nil {
			return agentKillPin{}, false, pinErr
		}
		return agentKillPin{ref: checkpointRef(runID, stageID) + "-" + kind + "-1", commitSHA: "c0ffee", treeSHA: "7ree", parentSHA: "9a7e"}, true, nil
	}
	t.Cleanup(func() { captureHead, restoreHead, agentKillCheckpointPin = origCap, origRes, origPin })
	return order
}

// setupAgentKillRun installs the implement-stage fakes with a fake invoker
// returning invokeErr.
func setupAgentKillRun(t *testing.T, reason string, invokeErr error) {
	t.Helper()
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	withFakeInvoker(t, &fakeInvoker{
		canned:    agent.Result{OK: false, FailureCategory: "A", FailureReason: reason},
		returnErr: invokeErr,
	})
	fu := newFakeUploader(t)
	fu.promptResp = &upload.FetchedPrompt{StageID: akStageID, StageType: "implement", Prompt: "implement", PromptHash: "h"}
	withFakeUploader(t, fu)
	withFakeGitOps(t, &fakePusher{}, &fakePROpener{})
}

func TestRun_ImplementStage_AgentTimeout_CheckpointsBeforeRestore(t *testing.T) {
	setupAgentKillRun(t, "agent timeout after 1h0m0s", agent.ErrTimeout)
	order := installAgentKillSpies(t, nil)

	var stderr strings.Builder
	if got := runFailedImplementStage(t, &stderr); got != exitFailure {
		t.Fatalf("run = %d, want exitFailure:\n%s", got, stderr.String())
	}
	if want := []string{"checkpoint:timeout", "restore:operator-main"}; strings.Join(*order, ",") != strings.Join(want, ",") {
		t.Fatalf("order = %v, want %v (the pin must precede the restore)", *order, want)
	}
	out := stderr.String()
	pinAt := strings.Index(out, `"event":"agent_kill_checkpoint_pinned"`)
	restoreAt := strings.Index(out, `"event":"working_tree_restored"`)
	if pinAt < 0 || restoreAt < 0 || pinAt > restoreAt {
		t.Errorf("agent_kill_checkpoint_pinned at %d, working_tree_restored at %d; want the pin line first:\n%s", pinAt, restoreAt, out)
	}
	if !strings.Contains(out, "uncommitted agent work pinned at refs/fishhawk/checkpoints/") {
		t.Errorf("runner_completed reason lacks the pinned clause:\n%s", out)
	}
}

func TestRun_ImplementStage_AgentKillCheckpointFailure_ReasonUnchanged(t *testing.T) {
	setupAgentKillRun(t, "agent timeout after 1h0m0s", agent.ErrTimeout)
	order := installAgentKillSpies(t, errors.New("update-ref refused"))

	var stderr strings.Builder
	if got := runFailedImplementStage(t, &stderr); got != exitFailure {
		t.Fatalf("run = %d, want exitFailure (fail-open):\n%s", got, stderr.String())
	}
	out := stderr.String()
	if !strings.Contains(out, `"event":"agent_kill_checkpoint_failed"`) {
		t.Errorf("missing agent_kill_checkpoint_failed:\n%s", out)
	}
	if strings.Contains(out, "uncommitted agent work pinned at") {
		t.Errorf("a failed pin must not add the pinned clause:\n%s", out)
	}
	if !strings.Contains(out, `"reason":"agent timeout after 1h0m0s"`) {
		t.Errorf("runner_completed reason changed; want it verbatim:\n%s", out)
	}
	if want := "checkpoint:timeout,restore:operator-main"; strings.Join(*order, ",") != want {
		t.Errorf("order = %v, want %s (the restore still runs)", *order, want)
	}
}

func TestRun_ImplementStage_AgentCrash_NoCheckpoint(t *testing.T) {
	setupAgentKillRun(t, "agent crash", errors.New("agent crash"))
	order := installAgentKillSpies(t, nil)

	var stderr strings.Builder
	if got := runFailedImplementStage(t, &stderr); got != exitFailure {
		t.Fatalf("run = %d, want exitFailure:\n%s", got, stderr.String())
	}
	if want := "restore:operator-main"; strings.Join(*order, ",") != want {
		t.Errorf("order = %v, want %s (an ordinary crash never pins)", *order, want)
	}
}

func TestRun_ImplementStage_AgentTimeout_NoPR_NoCheckpoint(t *testing.T) {
	setupAgentKillRun(t, "agent timeout after 1h0m0s", agent.ErrTimeout)
	order := installAgentKillSpies(t, nil)

	var stderr strings.Builder
	runFailedImplementStage(t, &stderr, "--no-pr")
	for _, e := range *order {
		if strings.HasPrefix(e, "checkpoint:") {
			t.Fatalf("order = %v; --no-pr must never pin", *order)
		}
	}
}

func TestPinAgentKillCheckpoint_UnbornHeadSkips(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()
	cprGit(t, repo, "init", "-q", "--initial-branch=main")
	akWrite(t, repo, "a.txt", "work\n")
	_, pinned, err := pinAgentKillCheckpoint(context.Background(), repo, akRunID, akStageID, "timeout")
	var skip errAgentKillSkip
	if pinned || !errors.As(err, &skip) || skip.reason != "head_unresolvable" || err.Error() != "head_unresolvable" {
		t.Fatalf("pinned = %v, err = %v; want a head_unresolvable skip", pinned, err)
	}
}

// With no real index the private index is seeded from HEAD (read-tree), and
// the real index is still never created.
func TestPinAgentKillCheckpoint_MissingRealIndexSeedsFromHead(t *testing.T) {
	repo, head := agentKillRepo(t)
	realIndex := filepath.Join(repo, ".git", "index")
	if err := os.Remove(realIndex); err != nil {
		t.Fatal(err)
	}
	pin, pinned, err := pinAgentKillCheckpoint(context.Background(), repo, akRunID, akStageID, "killed")
	if err != nil || !pinned {
		t.Fatalf("pin = %+v, pinned = %v, err = %v; want a written pin", pin, pinned, err)
	}
	if want := checkpointRef(akRunID, akStageID) + "-killed-1"; pin.ref != want {
		t.Errorf("ref = %q, want %q", pin.ref, want)
	}
	if got := cprGit(t, repo, "show", pin.ref+":a.txt"); got != "modified" {
		t.Errorf("%s:a.txt = %q, want the modified bytes", pin.ref, got)
	}
	if got := cprGit(t, repo, "show", pin.ref+":.gitignore"); got != "ignored.txt" {
		t.Errorf("%s:.gitignore = %q, want HEAD's file carried from read-tree", pin.ref, got)
	}
	if pin.parentSHA != head {
		t.Errorf("parent = %q, want HEAD %s", pin.parentSHA, head)
	}
	if _, err := os.Stat(realIndex); !os.IsNotExist(err) {
		t.Errorf("real index stat err = %v; the pin must never write the real index", err)
	}
}

// Every candidate ref refused (a directory/file conflict on each of the
// bounded attempts) returns the last update-ref error and leaves the
// conflicting refs untouched.
func TestPinAgentKillCheckpoint_AllAttemptsRefusedFails(t *testing.T) {
	repo, head := agentKillRepo(t)
	prefix := checkpointRef(akRunID, akStageID) + checkpointTimeoutSuffix
	for i := 1; i <= agentKillPinAttempts; i++ {
		cprGit(t, repo, "update-ref", fmt.Sprintf("%s%d/blocker", prefix, i), head)
	}
	_, pinned, err := pinAgentKillCheckpoint(context.Background(), repo, akRunID, akStageID, "timeout")
	var skip errAgentKillSkip
	if pinned || err == nil || errors.As(err, &skip) {
		t.Fatalf("pinned = %v, err = %v; want a non-skip failure", pinned, err)
	}
	if want := fmt.Sprintf("git update-ref %s%d", prefix, agentKillPinAttempts); !strings.Contains(err.Error(), want) {
		t.Errorf("err = %v, want the last attempt (%s) named", err, want)
	}
	for i := 1; i <= agentKillPinAttempts; i++ {
		if sha, ok := refTarget(repo, fmt.Sprintf("%s%d/blocker", prefix, i)); !ok || sha != head {
			t.Errorf("blocker %d = %q (exists %v), want untouched %s", i, sha, ok, head)
		}
	}
}

func TestCheckpointKilledAgentTree_SkipLogsAndKeepsReason(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()
	cprGit(t, repo, "init", "-q", "--initial-branch=main")
	cfg := config{runID: akRunID, stageID: akStageID, workingDir: repo}
	res := agent.Result{OK: false, FailureReason: "agent timeout after 1h0m0s"}
	var log strings.Builder
	checkpointKilledAgentTree(context.Background(), cfg, "implement", &res, agent.ErrTimeout, &log)
	if !strings.Contains(log.String(), `"event":"agent_kill_checkpoint_skipped"`) || !strings.Contains(log.String(), `"reason":"head_unresolvable"`) {
		t.Errorf("log = %s, want agent_kill_checkpoint_skipped with reason head_unresolvable", log.String())
	}
	if res.FailureReason != "agent timeout after 1h0m0s" || len(res.Events) != 0 {
		t.Errorf("reason = %q, events = %d; a skip must change neither", res.FailureReason, len(res.Events))
	}
}

// The wrapper's gates: only a failed implement result killed by timeout /
// loop / budget / cancellation reaches the pin seam, and an empty working dir
// pins ".".
func TestCheckpointKilledAgentTree_Gates(t *testing.T) {
	var calls []string
	orig := agentKillCheckpointPin
	agentKillCheckpointPin = func(_ context.Context, repoDir, _, _, kind string) (agentKillPin, bool, error) {
		calls = append(calls, repoDir+"|"+kind)
		return agentKillPin{}, false, nil
	}
	t.Cleanup(func() { agentKillCheckpointPin = orig })

	cfg := config{runID: akRunID, stageID: akStageID}
	failed := func() *agent.Result { return &agent.Result{OK: false, FailureReason: "r"} }
	for _, tc := range []struct {
		name      string
		cfg       config
		stage     string
		res       *agent.Result
		invokeErr error
	}{
		{"plan stage", cfg, "plan", failed(), agent.ErrTimeout},
		{"no-pr", config{runID: akRunID, stageID: akStageID, noPR: true}, "implement", failed(), agent.ErrTimeout},
		{"nil result", cfg, "implement", nil, agent.ErrTimeout},
		{"ok result", cfg, "implement", &agent.Result{OK: true}, agent.ErrTimeout},
		{"ordinary crash", cfg, "implement", failed(), agent.ErrAgentFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls = nil
			var log strings.Builder
			checkpointKilledAgentTree(context.Background(), tc.cfg, tc.stage, tc.res, tc.invokeErr, &log)
			if len(calls) != 0 || log.Len() != 0 {
				t.Errorf("calls = %v, log = %q; want no pin attempt and no log", calls, log.String())
			}
		})
	}

	calls = nil
	res := failed()
	var log strings.Builder
	checkpointKilledAgentTree(context.Background(), cfg, "implement", res, agent.ErrBudgetExceeded, &log)
	if want := ".|killed"; len(calls) != 1 || calls[0] != want {
		t.Fatalf("calls = %v, want [%s]", calls, want)
	}
	if !strings.Contains(log.String(), `"event":"agent_kill_checkpoint_failed"`) || !strings.Contains(log.String(), `"detail":"not pinned"`) {
		t.Errorf("log = %s, want agent_kill_checkpoint_failed with detail not pinned", log.String())
	}
	if res.FailureReason != "r" {
		t.Errorf("reason = %q, want unchanged", res.FailureReason)
	}
}
