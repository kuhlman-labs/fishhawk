package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/runner/internal/agent"
	"github.com/kuhlman-labs/fishhawk/runner/internal/gitops"
	"github.com/kuhlman-labs/fishhawk/runner/internal/upload"
)

// #3973: the stage-lifetime base pin. run() records the base an implement stage
// starts from (the standalone #3454 advance tip, a decomposition child's wave
// base, or its own pre-existing slice branch tip) as cfg.pinnedBaseSHA before
// the agent runs; both git_diff emitters merge-base against it with no fetch
// (resolveStageDiffBase) and CommitAndPush cuts the branch from it
// (CommitAndPushArgs.PinnedBaseSHA). The no-conflict / fast-forward proofs
// against a real bare origin live in runner/internal/gitops commit_test; these
// tests prove the RUNNER side: capture, threading, the own-slice-branch rule,
// and the #4079 failure-site interaction under the pin.

const (
	basePinRunID   = "11111111-2222-3333-4444-555555555555"
	basePinStageID = "22222222-3333-4444-5555-666666666666"
)

// pushToOrigin commits files onto branch's current tip in a throwaway clone of
// bare and pushes it back, returning the new tip. from names the branch to
// start from when branch does not exist yet on the remote ("" = branch).
func pushToOrigin(t *testing.T, bare, from, branch string, files map[string]string, msg string) string {
	t.Helper()
	if from == "" {
		from = branch
	}
	side := filepath.Join(t.TempDir(), "side")
	if err := runGitErr(filepath.Dir(side), "clone", "-q", "-b", from, bare, side); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"config", "user.name", "other"},
		{"config", "user.email", "other@example.com"},
		{"config", "commit.gpgsign", "false"},
	} {
		if err := runGitErr(side, args...); err != nil {
			t.Fatal(err)
		}
	}
	for name, body := range files {
		mustWrite(t, filepath.Join(side, name), body)
	}
	if err := runGitErr(side, "add", "-A"); err != nil {
		t.Fatal(err)
	}
	if err := runGitErr(side, "commit", "-q", "-m", msg); err != nil {
		t.Fatal(err)
	}
	if err := runGitErr(side, "push", "-q", "origin", "HEAD:refs/heads/"+branch); err != nil {
		t.Fatal(err)
	}
	tip, err := runGitOut(side, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	return tip
}

// gitDiffBaseRefs returns the base_ref label of every git_diff event in the
// bundle, in order.
func gitDiffBaseRefs(t *testing.T, bundlePath string) []string {
	t.Helper()
	var refs []string
	for _, ev := range readBundleEvents(t, bundlePath) {
		if ev.Kind != "git_diff" {
			continue
		}
		p := string(ev.Data)
		const key = `"base_ref":"`
		i := strings.Index(p, key)
		if i < 0 {
			refs = append(refs, "")
			continue
		}
		rest := p[i+len(key):]
		refs = append(refs, rest[:strings.Index(rest, `"`)])
	}
	return refs
}

// logLines returns every stderr line carrying the given event name.
func logLines(log, event string) []string {
	var out []string
	for _, l := range strings.Split(log, "\n") {
		if strings.Contains(l, `"event":"`+event+`"`) {
			out = append(out, l)
		}
	}
	return out
}

// TestRun_StandalonePin_ThreadsDispatchTipThroughDiffAndPush proves pin
// THREADING, not the absence of a conflict: its pusher is a fake, so it cannot
// observe a commit-time stash-pop. The no-conflict proof against a real bare
// origin is gitops commit_test's TestCommitAndPush_PinnedBase_MidStageBaseAdvance_NoConflict
// with its unpinned control.
//
// Real git throughout up to the push: a plan-time provision, origin/main
// advanced to T1 before dispatch (the #3454 advance moves the worktree to T1),
// and a SECOND origin/main commit T2 pushed from inside the agent invoke,
// editing the very scope file the agent edits. The stage must stay judged
// against T1: the captured CommitAndPushArgs.PinnedBaseSHA is T1, the diff is
// merge-based against T1 with no fetch, and nothing re-anchors onto T2.
func TestRun_StandalonePin_ThreadsDispatchTipThroughDiffAndPush(t *testing.T) {
	operator, _, advanceOrigin := standaloneMovedBaseRepo(t)
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	bare := cprGit(t, operator, "remote", "get-url", "origin")

	ctx := context.Background()
	if _, err := provisionLineageWorktree(ctx, operator, lineageRoot(basePinRunID, "", false), "main", io.Discard); err != nil {
		t.Fatalf("plan-stage provision: %v", err)
	}
	t1 := advanceOrigin()

	var t2, invokeHEAD string
	withFakeInvoker(t, &fakeInvoker{
		canned: agent.Result{OK: true},
		onInvoke: func(_ int, inv agent.Invocation) {
			invokeHEAD, _ = runGitOut(inv.WorkingDir, "rev-parse", "HEAD")
			// A PR merges into main MID-STAGE, editing the same scope file.
			t2 = pushToOrigin(t, bare, "", "main",
				map[string]string{"scope.txt": "edited by a PR merged mid-stage\n"}, "mid-stage merge")
			if werr := os.WriteFile(filepath.Join(inv.WorkingDir, "scope.txt"),
				[]byte("edited by the implement agent\n"), 0o644); werr != nil {
				t.Error(werr)
			}
		},
	})
	fu := newFakeUploader(t)
	fu.promptResp = &upload.FetchedPrompt{
		StageID:    basePinStageID,
		StageType:  "implement",
		Prompt:     "implement",
		PromptHash: "h",
		ScopeFiles: []upload.ScopeFile{{Path: "scope.txt", Operation: "modify"}},
	}
	withFakeUploader(t, fu)
	fp := &fakePusher{result: &gitops.CommitAndPushResult{HeadSHA: "head-sha-abc", BaseSHA: "base"}}
	origPusher, origOpener := newPusher, newPROpener
	newPusher = func() pusher { return fp }
	newPROpener = func(string) prOpener { return &fakePROpener{} }
	t.Cleanup(func() { newPusher = origPusher; newPROpener = origOpener })

	var stderr strings.Builder
	got := run([]string{
		"--run-id", basePinRunID,
		"--backend-url", "https://api.fishhawk.test",
		"--workflow", "feature_change", "--stage", "implement",
		"--stage-id", basePinStageID,
		"--working-dir", operator,
		"--check-base-ref", "main",
		"--fetch-prompt", "--upload-trace",
	}, &stderr)
	if got != exitOK {
		t.Fatalf("run = %d, want exitOK:\n%s", got, stderr.String())
	}
	log := stderr.String()
	if t2 == "" || t2 == t1 {
		t.Fatalf("fixture: the mid-stage merge did not move origin/main (t1=%s t2=%s)", t1, t2)
	}
	if invokeHEAD != t1 {
		t.Fatalf("worktree HEAD at invoke = %q, want the dispatch tip T1 %q", invokeHEAD, t1)
	}
	if fp.gotArgs == nil {
		t.Fatalf("CommitAndPush was never called:\n%s", log)
	}
	if fp.gotArgs.PinnedBaseSHA != t1 {
		t.Errorf("CommitAndPushArgs.PinnedBaseSHA = %q, want the dispatch tip T1 %q (T2 = %q)", fp.gotArgs.PinnedBaseSHA, t1, t2)
	}
	pinned := logLines(log, "diff_base_pinned")
	if len(pinned) == 0 {
		t.Fatalf("no diff_base_pinned line — the diff was not measured against the pin:\n%s", log)
	}
	for _, l := range pinned {
		if !strings.Contains(l, `"base_sha":"`+t1+`"`) || !strings.Contains(l, `"merge_base":"`+t1+`"`) {
			t.Errorf("diff_base_pinned = %s, want base_sha and merge_base == T1 %s", l, t1)
		}
	}
	for _, l := range logLines(log, "diff_base_reanchored") {
		if strings.Contains(l, t2) {
			t.Errorf("the diff re-anchored onto the mid-stage tip T2 %s: %s", t2, l)
		}
	}
	bp := logLines(log, "base_pinned")
	if len(bp) != 1 || !strings.Contains(bp[0], `"source":"standalone_advance"`) || !strings.Contains(bp[0], `"base_sha":"`+t1+`"`) {
		t.Errorf("base_pinned lines = %v, want exactly one standalone_advance pin at T1 %s", bp, t1)
	}
}

// ownSliceBranchRepo is decomposedWaveBaseRepo plus, when withSlice, the child's
// OWN slice branch already on the remote at a prior commit S cut on top of the
// consolidated wave base W (the #3991 resumed-child shape). The slice branch is
// pushed AFTER the operator clone, so the operator has no tracking ref for it:
// only the runner's own-slice-branch checkout can write one.
func ownSliceBranchRepo(t *testing.T, withSlice bool) (operator, waveBase, waveTip, sliceBranch, sliceTip string) {
	t.Helper()
	operator, _, waveBase = decomposedWaveBaseRepo(t)
	bare := cprGit(t, operator, "remote", "get-url", "origin")
	waveTip = cprGit(t, operator, "rev-parse", "origin/"+waveBase)
	sliceBranch = childSliceBranch(decomposedChildPromptResp().DecomposedFromRunID, 0)
	if withSlice {
		sliceTip = pushToOrigin(t, bare, waveBase, sliceBranch,
			map[string]string{"slice.txt": "the earlier attempt's published slice commit\n"}, "prior slice attempt")
	}
	return operator, waveBase, waveTip, sliceBranch, sliceTip
}

// runPinnedChild drives a decomposed-child implement stage against operator with
// --check-base-ref=waveBase, every git seam PRODUCTION and only the push/PR
// egress faked. The agent edits slice.txt (the declared scope file).
func runPinnedChild(t *testing.T, operator, waveBase string) (fp *fakePusher, log string, invokeHEAD string, bundlePath string, code int) {
	t.Helper()
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	withFakeInvoker(t, &fakeInvoker{
		canned: agent.Result{OK: true},
		onInvoke: func(_ int, inv agent.Invocation) {
			invokeHEAD, _ = runGitOut(inv.WorkingDir, "rev-parse", "HEAD")
			if werr := os.WriteFile(filepath.Join(inv.WorkingDir, "slice.txt"),
				[]byte("this attempt's slice edit\n"), 0o644); werr != nil {
				t.Error(werr)
			}
		},
	})
	fu := newFakeUploader(t)
	fu.promptResp = decomposedChildPromptResp()
	fu.promptResp.ScopeFiles = []upload.ScopeFile{{Path: "slice.txt", Operation: "modify"}}
	withFakeUploader(t, fu)
	fp = &fakePusher{result: &gitops.CommitAndPushResult{NoChanges: true, BaseSHA: "base"}}
	origPusher, origOpener := newPusher, newPROpener
	newPusher = func() pusher { return fp }
	newPROpener = func(string) prOpener { return &fakePROpener{} }
	t.Cleanup(func() { newPusher = origPusher; newPROpener = origOpener })

	bundlePath = filepath.Join(t.TempDir(), "trace.jsonl.gz")
	var stderr strings.Builder
	code = run([]string{
		"--run-id", basePinRunID,
		"--backend-url", "https://api.fishhawk.test",
		"--workflow", "feature_change", "--stage", "implement",
		"--stage-id", basePinStageID,
		"--working-dir", operator,
		"--check-base-ref", waveBase,
		"--fetch-prompt", "--upload-trace",
		"--bundle-out", bundlePath,
	}, &stderr)
	return fp, stderr.String(), invokeHEAD, bundlePath, code
}

// TestRun_ChildOwnSliceBranch_PinsSliceTip: a resumed child whose slice branch
// already exists on the remote at S bases on S (not the wave base), pins S,
// bounds its policy diff against origin/<slice-branch> (policy_base_
// decomposition_child + the git_diff label), and hands CommitAndPush S so the
// re-run's push is a fast-forward onto its own branch.
func TestRun_ChildOwnSliceBranch_PinsSliceTip(t *testing.T) {
	operator, waveBase, waveTip, sliceBranch, sliceTip := ownSliceBranchRepo(t, true)
	if sliceTip == waveTip {
		t.Fatal("fixture: the slice tip must differ from the wave-base tip")
	}
	fp, log, invokeHEAD, bundlePath, code := runPinnedChild(t, operator, waveBase)
	if code != exitOK {
		t.Fatalf("run = %d, want exitOK:\n%s", code, log)
	}
	if invokeHEAD != sliceTip {
		t.Errorf("worktree HEAD at invoke = %q, want the own slice tip S %q (wave tip %q)", invokeHEAD, sliceTip, waveTip)
	}
	if fp.gotArgs == nil || fp.gotArgs.PinnedBaseSHA != sliceTip {
		t.Errorf("CommitAndPushArgs = %+v, want PinnedBaseSHA == S %q", fp.gotArgs, sliceTip)
	}
	pb := logLines(log, "policy_base_decomposition_child")
	if len(pb) == 0 || !strings.Contains(pb[0], `"shared_branch":"`+sliceBranch+`"`) {
		t.Errorf("policy_base_decomposition_child = %v, want one naming %s:\n%s", pb, sliceBranch, log)
	}
	wantLabel := "origin/" + sliceBranch
	labels := gitDiffBaseRefs(t, bundlePath)
	if len(labels) == 0 {
		t.Fatalf("no git_diff event in the bundle:\n%s", log)
	}
	for _, l := range labels {
		if l != wantLabel {
			t.Errorf("git_diff base_ref = %q, want %q", l, wantLabel)
		}
	}
	dp := logLines(log, "diff_base_pinned")
	if len(dp) == 0 || !strings.Contains(dp[0], `"merge_base":"`+sliceTip+`"`) {
		t.Errorf("diff_base_pinned = %v, want merge_base == S %s", dp, sliceTip)
	}
	ce := logLines(log, "child_base_established")
	if len(ce) != 1 || !strings.Contains(ce[0], `"source":"own_slice_branch"`) || !strings.Contains(ce[0], `"branch":"`+sliceBranch+`"`) {
		t.Errorf("child_base_established = %v, want one own_slice_branch record on %s", ce, sliceBranch)
	}
	bp := logLines(log, "base_pinned")
	if len(bp) != 1 || !strings.Contains(bp[0], `"source":"own_slice_branch"`) || !strings.Contains(bp[0], `"base_sha":"`+sliceTip+`"`) {
		t.Errorf("base_pinned = %v, want one own_slice_branch pin at S %s", bp, sliceTip)
	}
}

// TestRun_ChildWithoutSliceBranch_PinsWaveBaseTip: a FRESH child (slice branch
// absent) keeps today's wave-base establishment and pins the wave-base tip; no
// slice tracking ref exists, so the policy base stays the wave base.
func TestRun_ChildWithoutSliceBranch_PinsWaveBaseTip(t *testing.T) {
	operator, waveBase, waveTip, _, _ := ownSliceBranchRepo(t, false)
	fp, log, invokeHEAD, bundlePath, code := runPinnedChild(t, operator, waveBase)
	if code != exitOK {
		t.Fatalf("run = %d, want exitOK:\n%s", code, log)
	}
	if invokeHEAD != waveTip {
		t.Errorf("worktree HEAD at invoke = %q, want the wave-base tip %q", invokeHEAD, waveTip)
	}
	if fp.gotArgs == nil || fp.gotArgs.PinnedBaseSHA != waveTip {
		t.Errorf("CommitAndPushArgs = %+v, want PinnedBaseSHA == the wave-base tip %q", fp.gotArgs, waveTip)
	}
	if pb := logLines(log, "policy_base_decomposition_child"); len(pb) != 0 {
		t.Errorf("a fresh child logged policy_base_decomposition_child: %v", pb)
	}
	for _, l := range gitDiffBaseRefs(t, bundlePath) {
		if l != waveBase {
			t.Errorf("git_diff base_ref = %q, want the wave base %q", l, waveBase)
		}
	}
	ce := logLines(log, "child_base_established")
	if len(ce) != 1 || !strings.Contains(ce[0], `"source":"wave_base"`) {
		t.Errorf("child_base_established = %v, want one wave_base record", ce)
	}
	bp := logLines(log, "base_pinned")
	if len(bp) != 1 || !strings.Contains(bp[0], `"source":"wave_base"`) || !strings.Contains(bp[0], `"base_sha":"`+waveTip+`"`) {
		t.Errorf("base_pinned = %v, want one wave_base pin at %s", bp, waveTip)
	}
}

// pinnedChildCfg is the #4079 incident config (a child whose consolidated base
// never exists on the remote) with the pin set to the repo HEAD — the base
// checkoutChildBase established before the agent ran.
func pinnedChildCfg(t *testing.T, repo string) config {
	t.Helper()
	cfg := verifiedTreeCfg(repo, "true")
	cfg.decomposedFromRunID = childResumeParentRunID
	cfg.baseBranch = "fishhawk/run-deadbeef-consolidated" // never exists on the remote
	cfg.pinnedBaseSHA = cprGit(t, repo, "rev-parse", "HEAD")
	return cfg
}

// TestOpenPRAndShipArtifact_PinnedChild_UnresolvableBase_StillPushes is the
// #4079 incident shape under the pin, with the REAL pusher against a real bare
// origin: the consolidated base never exists on the remote, yet the pinned child
// cuts its slice branch from the pin with no commit-time fetch and pushes. The
// unpinned guard for the same shape is #4079's
// TestOpenPRAndShipArtifact_ChildBaseFetchFailure_ArmsAndNamesRefs.
func TestOpenPRAndShipArtifact_PinnedChild_UnresolvableBase_StillPushes(t *testing.T) {
	repo, bare, _ := verifiedTreeRepo(t) // a.txt carries the uncommitted agent edit
	verifiedTree := treeWithEdit(t, repo, "a.txt", "agent change\n")
	withFakePROpenerOnly(t)
	fu := newFakeUploader(t)
	issued, err := fu.IssueKey(context.Background(), verifiedTreeRunID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	cfg := pinnedChildCfg(t, repo)
	cp := pushCheckpoint{backendSupportsPushResume: true}
	var logSink strings.Builder
	if err := openPRAndShipArtifact(context.Background(), cfg, &logSink, fu, issued, "", false, false, nil, false, verifiedTree, "", nil, nil, nil, &cp); err != nil {
		t.Fatalf("pinned child push failed: %v\n%s", err, logSink.String())
	}
	slice := childSliceBranch(childResumeParentRunID, runSliceIndex)
	pushed := cprGit(t, bare, "rev-parse", "refs/heads/"+slice)
	if parent := cprGit(t, bare, "rev-parse", pushed+"^"); parent != cfg.pinnedBaseSHA {
		t.Errorf("pushed slice commit parent = %s, want the pin %s", parent, cfg.pinnedBaseSHA)
	}
	if tree := cprGit(t, bare, "rev-parse", pushed+"^{tree}"); tree != verifiedTree {
		t.Errorf("pushed tree = %s, want the verified tree %s", tree, verifiedTree)
	}
	var reported bool
	for _, c := range fu.gotPRCalls {
		if c.Outcome == "pushed" && c.Branch == slice {
			reported = true
		}
	}
	if !reported {
		t.Errorf("the child was not reported pushed on %s: %+v", slice, fu.gotPRCalls)
	}
}

// TestOpenPRAndShipArtifact_PinnedChild_CheckoutFailure_ArmsAndNamesRefs: the
// pinned arm's `checkout -B <slice> <pin>` fails AFTER the stash (a slice
// branch ref .lock). #4079's failure site must still see a *BaseFetchError
// naming the stash: category C, an armed push-kind checkpoint whose base is the
// pin, both checkpoint refs pinned and named, nothing on the remote.
func TestOpenPRAndShipArtifact_PinnedChild_CheckoutFailure_ArmsAndNamesRefs(t *testing.T) {
	repo, bare, _ := verifiedTreeRepo(t)
	verifiedTree := treeWithEdit(t, repo, "a.txt", "agent change\n")
	withFakePROpenerOnly(t)
	fu := newFakeUploader(t)
	issued, err := fu.IssueKey(context.Background(), verifiedTreeRunID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	cfg := pinnedChildCfg(t, repo)
	slice := childSliceBranch(childResumeParentRunID, runSliceIndex)
	lock := filepath.Join(repo, ".git", "refs", "heads", filepath.FromSlash(slice)+".lock")
	if err := os.MkdirAll(filepath.Dir(lock), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, lock, "")

	cp := pushCheckpoint{backendSupportsPushResume: true}
	var logSink strings.Builder
	err = openPRAndShipArtifact(context.Background(), cfg, &logSink, fu, issued, "", false, false, nil, false, verifiedTree, "", nil, nil, nil, &cp)
	if !errors.Is(err, gitops.ErrBaseFetchFailed) {
		t.Fatalf("err = %v, want ErrBaseFetchFailed\n%s", err, logSink.String())
	}
	if got := pushFailureCategory(err); got != "C" {
		t.Errorf("pushFailureCategory = %q, want C", got)
	}
	if !cp.armed || cp.resumeKind != resumeKindPush {
		t.Fatalf("checkpoint = %+v, want an armed push-kind checkpoint\n%s", cp, logSink.String())
	}
	if cp.baseSHA != cfg.pinnedBaseSHA {
		t.Errorf("checkpoint base = %s, want the pin %s", cp.baseSHA, cfg.pinnedBaseSHA)
	}
	ckRef := checkpointRef(cfg.runID, cfg.stageID)
	stRef := checkpointStashRef(cfg.runID, cfg.stageID)
	if got, ok := refTarget(repo, ckRef); !ok || got != cp.headSHA {
		t.Errorf("checkpoint ref %s = %q (exists %t), want %s", ckRef, got, ok, cp.headSHA)
	}
	var bfe *gitops.BaseFetchError
	if !errors.As(err, &bfe) || bfe.StashSHA == "" {
		t.Fatalf("no stash SHA on the error: %v", err)
	}
	if got, ok := refTarget(repo, stRef); !ok || got != bfe.StashSHA {
		t.Errorf("stash ref %s = %q (exists %t), want %s", stRef, got, ok, bfe.StashSHA)
	}
	for _, ref := range []string{ckRef, stRef} {
		if !strings.Contains(err.Error(), ref) {
			t.Errorf("returned reason does not name %s: %v", ref, err)
		}
	}
	if _, rerr := exec.Command("git", "--git-dir="+bare, "rev-parse", "--verify", "refs/heads/"+slice).Output(); rerr == nil {
		t.Errorf("slice branch %s reached the remote on a pinned checkout failure", slice)
	}
}

// TestOpenPRAndShipArtifact_PinnedChild_PushFailure_StaysCategoryC: a push
// transport failure on a pinned child is still ErrPushFailed (the pinned arm
// leaves the push tail untouched), category C, and #3621/#4079's checkpoint ref
// is pinned at the failure site and named — with the pin threaded to the pusher.
func TestOpenPRAndShipArtifact_PinnedChild_PushFailure_StaysCategoryC(t *testing.T) {
	repo, _, _, headSHA, treeSHA := pushResumeRepo(t)
	pin := cprGit(t, repo, "rev-parse", "main")
	slice := childSliceBranch(childResumeParentRunID, runSliceIndex)
	// CommitAndPush leaves the commit, cut from the pin, on HEAD when only the
	// push fails.
	cprGit(t, repo, "checkout", "-q", "-b", slice, headSHA)
	fp := withFakePusher(t)
	fp.err = fmt.Errorf("gitops: push: %w", gitops.ErrPushFailed)
	withFakePROpenerOnly(t)
	fu := newFakeUploader(t)
	issued, err := fu.IssueKey(context.Background(), verifiedTreeRunID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	cfg := childResumeCfg(repo)
	cfg.pinnedBaseSHA = pin
	cp := pushCheckpoint{backendSupportsPushResume: true}
	var logSink strings.Builder
	err = openPRAndShipArtifact(context.Background(), cfg, &logSink, fu, issued, "", false, false, nil, false, treeSHA, "", nil, nil, nil, &cp)
	if fp.gotArgs == nil || fp.gotArgs.PinnedBaseSHA != pin {
		t.Errorf("CommitAndPushArgs = %+v, want PinnedBaseSHA == the pin %s", fp.gotArgs, pin)
	}
	if !errors.Is(err, gitops.ErrPushFailed) {
		t.Fatalf("err = %v, want ErrPushFailed", err)
	}
	if got := pushFailureCategory(err); got != "C" {
		t.Errorf("pushFailureCategory = %q, want C", got)
	}
	if !cp.armed || cp.headSHA != headSHA || cp.baseSHA != pin {
		t.Fatalf("checkpoint = %+v, want armed at %s on the pin %s\n%s", cp, headSHA, pin, logSink.String())
	}
	ref := checkpointRef(cfg.runID, cfg.stageID)
	if got, ok := refTarget(repo, ref); !ok || got != headSHA {
		t.Errorf("checkpoint ref %s = %q (exists %t), want %s", ref, got, ok, headSHA)
	}
	if !strings.Contains(err.Error(), ref) {
		t.Errorf("reason does not name the pinned ref %s: %v", ref, err)
	}
}

// TestRun_ChildOwnSliceBranchQueryError splits the own-slice-branch query
// failure the same way the wave-base query splits: against a CONFIGURED remote
// it fails loud pre-agent at child_base_checkout; against an UNCONFIGURED one
// (GitHub not wired) it is treated as absent and the wave base is established.
func TestRun_ChildOwnSliceBranchQueryError(t *testing.T) {
	const waveBase = "fishhawk/run-aaaaaaaa/consolidated"
	sliceErrOnly := func(branch string) (bool, error) {
		if strings.Contains(branch, "/slice-") {
			return false, errors.New("ls-remote origin: ssh: connect to host github.com port 22: operation timed out")
		}
		return true, nil
	}
	for _, tc := range []struct {
		name       string
		configured bool
	}{
		{"configured remote fails loud", true},
		{"unconfigured remote falls through to the wave base", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			implementEnv(t, "kuhlman-labs/fishhawk", "main")
			withFakeRemoteBranchExists(t, false)
			inv := &fakeInvoker{canned: agent.Result{OK: true}}
			withFakeInvoker(t, inv)
			fu := newFakeUploader(t)
			fu.promptResp = decomposedChildPromptResp()
			withFakeUploader(t, fu)
			withFakeGitOps(t, &fakePusher{}, &fakePROpener{})
			withFakeRemoteHasBranchFunc(t, sliceErrOnly)
			withFakeRemoteConfigured(t, tc.configured)
			var checkedOut []string
			origCheckout := checkoutChildBase
			checkoutChildBase = func(_ context.Context, _, _, branch, _ string) (string, error) {
				checkedOut = append(checkedOut, branch)
				return "wave-tip-sha", nil
			}
			t.Cleanup(func() { checkoutChildBase = origCheckout })

			var stderr strings.Builder
			got := runDecomposedChildStageWithBase(t, &stderr, waveBase)
			log := stderr.String()
			if tc.configured {
				if got != exitFailure {
					t.Fatalf("run = %d, want exitFailure:\n%s", got, log)
				}
				if inv.gotInv != nil {
					t.Error("agent was invoked despite the own-slice-branch query failure")
				}
				if !strings.Contains(log, `"reason":"child_base_checkout"`) || !strings.Contains(log, "query own slice branch") {
					t.Errorf("missing the child_base_checkout own-slice-branch failure:\n%s", log)
				}
				if len(checkedOut) != 0 {
					t.Errorf("checked out %v despite the failed query", checkedOut)
				}
				return
			}
			if got != exitOK {
				t.Fatalf("run = %d, want exitOK:\n%s", got, log)
			}
			if len(checkedOut) != 1 || checkedOut[0] != waveBase {
				t.Errorf("checked out %v, want exactly the wave base %q", checkedOut, waveBase)
			}
			if !strings.Contains(log, `"source":"wave_base"`) {
				t.Errorf("missing the wave_base child_base_established record:\n%s", log)
			}
		})
	}
}

// TestResolveStageDiffBase_PinUnresolvable_FailsOpen: a pin that is not a
// commit in the repository cannot be merge-based, so the diff falls back to
// today's resolveDiffBaseRef chain (logging diff_base_pin_unresolved) rather
// than blocking the diff or measuring against an unresolvable ref.
func TestResolveStageDiffBase_PinUnresolvable_FailsOpen(t *testing.T) {
	repo := initRepo(t) // no origin: the delegate takes the local merge-base
	head := headOf(t, repo)
	withFakeRemoteConfigured(t, false)
	cfg := config{stageID: "s", pinnedBaseSHA: strings.Repeat("d", 40)}
	branch := cprGit(t, repo, "rev-parse", "--abbrev-ref", "HEAD")
	var log strings.Builder
	got := resolveStageDiffBase(context.Background(), cfg, branch, repo, &log)
	if got != head {
		t.Errorf("resolveStageDiffBase = %q, want the delegate's merge-base %q", got, head)
	}
	if !strings.Contains(log.String(), `"event":"diff_base_pin_unresolved"`) {
		t.Errorf("missing diff_base_pin_unresolved:\n%s", log.String())
	}
	if strings.Contains(log.String(), `"event":"diff_base_pinned"`) {
		t.Errorf("logged diff_base_pinned for an unresolvable pin:\n%s", log.String())
	}
}

// TestPinBase_EmptySHA_LeavesStageUnpinned: every skip / degrade path hands
// pinBase "" and must leave the stage unpinned and silent.
func TestPinBase_EmptySHA_LeavesStageUnpinned(t *testing.T) {
	cfg := config{runID: "r", stageID: "s"}
	var log strings.Builder
	pinBase(&cfg, "main", "", baseSourceStandaloneAdvance, &log)
	if cfg.pinnedBaseSHA != "" || log.String() != "" {
		t.Errorf("empty sha pinned %q / logged %q, want unpinned and silent", cfg.pinnedBaseSHA, log.String())
	}
	pinBase(&cfg, "main", "abc", baseSourceWaveBase, &log)
	if cfg.pinnedBaseSHA != "abc" || !strings.Contains(log.String(), `"source":"wave_base"`) {
		t.Errorf("pin = %q / log %q, want abc with a wave_base base_pinned line", cfg.pinnedBaseSHA, log.String())
	}
}
