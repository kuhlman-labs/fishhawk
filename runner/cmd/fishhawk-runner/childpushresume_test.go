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
	"time"

	"github.com/kuhlman-labs/fishhawk/runner/internal/gitops"
)

// #4079: the decomposed-child push resume, the base-fetch-failure checkpoint
// arm, and the durable checkpoint refs.

const childResumeParentRunID = "4079aaaabbbbccccddddeeeeffff0000"

// childResumeCfg is checkpointResumeCfg for a decomposition child.
func childResumeCfg(repo string) config {
	cfg := checkpointResumeCfg(repo)
	cfg.decomposedFromRunID = childResumeParentRunID
	return cfg
}

// cprGit runs git in dir and returns trimmed stdout, failing the test on error.
func cprGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		var stderr string
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			stderr = string(ee.Stderr)
		}
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, stderr)
	}
	return strings.TrimSpace(string(out))
}

// refTarget resolves ref in repo, reporting whether it exists.
func refTarget(repo, ref string) (string, bool) {
	out, err := exec.Command("git", "-C", repo, "rev-parse", "--verify", "--quiet", ref).Output()
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(out)), true
}

// treeWithEdit builds, BY CONSTRUCTION, the tree object HEAD would have with
// path set to content — through a throwaway index, so the work tree, the real
// index and every ref are untouched. It stands in for the tree a passing
// committed-tree verify certified, without calling any control under test.
func treeWithEdit(t *testing.T, repo, path, content string) string {
	t.Helper()
	blobSrc := filepath.Join(t.TempDir(), "blob")
	if err := os.WriteFile(blobSrc, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	blob := cprGit(t, repo, "hash-object", "-w", blobSrc)
	idx := filepath.Join(t.TempDir(), "index")
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_INDEX_FILE="+idx)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("read-tree", "HEAD")
	run("update-index", "--add", "--cacheinfo", "100644,"+blob+","+path)
	return run("write-tree")
}

// baseFetchArmRepo is the base-fetch-failure fixture: HEAD is the base commit
// (the runner-controlled child base), the agent's edit was STASHED by
// CommitAndPush (so the work tree is clean), and the verified tree is the base
// tree plus that edit, built by construction.
func baseFetchArmRepo(t *testing.T) (repo, headBefore, stashSHA, verifiedTree string) {
	t.Helper()
	repo, _, _, _, _ = pushResumeRepo(t) // on main, clean, origin carries main
	headBefore = cprGit(t, repo, "rev-parse", "HEAD")
	verifiedTree = treeWithEdit(t, repo, "a.txt", "the verified slice edit\n")
	mustWrite(t, filepath.Join(repo, "a.txt"), "the verified slice edit\n")
	cprGit(t, repo, "stash", "push", "--include-untracked", "-m", "fishhawk-4079-fixture")
	stashSHA = cprGit(t, repo, "rev-parse", "refs/stash")
	return repo, headBefore, stashSHA, verifiedTree
}

// TestBaseFetchFailure_ChildArmsSynthesizedCheckpoint drives the REAL arm over
// one fixture per row. Each row asserts the OBSERVABLE outcome: whether a
// checkpoint exists and of which kind, which refs exist and where they point,
// and the named refusal reason.
func TestBaseFetchFailure_ChildArmsSynthesizedCheckpoint(t *testing.T) {
	const base = "fishhawk/run-deadbeef-consolidated"
	const sliceBranch = "fishhawk/run-4079aaaa/slice-0"
	cases := []struct {
		name       string
		cfg        func(repo string) config
		supports   bool
		noVerified bool
		badTree    bool
		noStash    bool
		plainErr   error
		wantArmed  bool
		wantReason string
		wantStash  bool
	}{
		{name: "armed", cfg: childResumeCfg, supports: true, wantArmed: true, wantStash: true},
		{name: "standalone_not_child", cfg: checkpointResumeCfg, supports: true, wantReason: notArmedNotDecomposedChild, wantStash: true},
		{name: "child_fixup", cfg: func(repo string) config {
			c := childResumeCfg(repo)
			c.fixup = true
			return c
		}, supports: true, wantReason: notArmedNotDecomposedChild, wantStash: true},
		{name: "backend_capability_absent", cfg: childResumeCfg, supports: false, wantReason: notArmedBackendCapabilityAbsent, wantStash: true},
		// The #4079 incident shape: the verify was SKIPPED, so no verified tree.
		// Nothing resumable — but the stash commit is still pinned.
		{name: "incident_no_verified_tree", cfg: childResumeCfg, supports: true, noVerified: true, wantReason: notArmedNoVerifiedTree, wantStash: true},
		{name: "synthesis_failed", cfg: childResumeCfg, supports: true, badTree: true, wantReason: notArmedSynthesisFailed, wantStash: true},
		{name: "armed_without_stash_sha", cfg: childResumeCfg, supports: true, noStash: true, wantArmed: true},
		{name: "silent_on_push_transport_error", cfg: childResumeCfg, supports: true, plainErr: fmt.Errorf("x: %w", gitops.ErrPushFailed)},
		{name: "silent_on_plain_error", cfg: childResumeCfg, supports: true, plainErr: errors.New("gitops: commit: nothing to commit")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo, headBefore, stashSHA, verifiedTree := baseFetchArmRepo(t)
			cfg := tc.cfg(repo)
			cfg.commitAuthorName = "fishhawk-dev[bot]"
			cfg.commitAuthorEmail = "42+fishhawk-dev[bot]@users.noreply.github.com"
			served := verifiedTree
			if tc.noVerified {
				served = ""
			}
			if tc.badTree {
				served = "0123456789abcdef0123456789abcdef01234567"
			}
			bfe := &gitops.BaseFetchError{Ref: base, StashSHA: stashSHA, Err: errors.New("gitops: fetch " + base + ": exit status 128")}
			if tc.noStash {
				bfe.StashSHA = ""
			}
			var commitErr error = bfe
			if tc.plainErr != nil {
				commitErr = tc.plainErr
			}
			var cp pushCheckpoint
			var logSink strings.Builder
			armedRef, stashRef := maybeArmBaseFetchFailureCheckpoint(context.Background(), cfg, &logSink, &cp,
				tc.supports, fmt.Errorf("commit+push: %w", commitErr), repo, sliceBranch, served,
				"feat(x): the slice\n\nBody.", "title", "body")
			log := logSink.String()
			ckRef := checkpointRef(cfg.runID, cfg.stageID)
			stRef := checkpointStashRef(cfg.runID, cfg.stageID)

			if tc.plainErr != nil {
				if log != "" || cp.armed || armedRef != "" || stashRef != "" {
					t.Fatalf("a non-base-fetch error must be silent and pin nothing; armed=%t refs=%q,%q log:\n%s", cp.armed, armedRef, stashRef, log)
				}
				for _, r := range []string{ckRef, stRef} {
					if _, ok := refTarget(repo, r); ok {
						t.Errorf("ref %s exists after a non-base-fetch error", r)
					}
				}
				return
			}

			// STASH PIN, independent of arming.
			gotStash, stashExists := refTarget(repo, stRef)
			if tc.wantStash {
				if !stashExists || gotStash != stashSHA || stashRef != stRef {
					t.Errorf("stash ref %s = %q (exists %t, returned %q), want %s", stRef, gotStash, stashExists, stashRef, stashSHA)
				}
			} else if stashExists || stashRef != "" {
				t.Errorf("no stash SHA was carried, yet %s exists (%q) / returned %q", stRef, gotStash, stashRef)
			}

			if cp.armed != tc.wantArmed {
				t.Fatalf("armed = %t, want %t\n%s", cp.armed, tc.wantArmed, log)
			}
			if !tc.wantArmed {
				if !strings.Contains(log, `"reason":"`+tc.wantReason+`"`) {
					t.Errorf("missing push_checkpoint_not_armed %q:\n%s", tc.wantReason, log)
				}
				if _, ok := refTarget(repo, ckRef); ok || armedRef != "" {
					t.Errorf("checkpoint ref %s exists / returned %q on a refusal", ckRef, armedRef)
				}
				return
			}
			if cp.resumeKind != resumeKindPush || cp.branch != sliceBranch || cp.verifiedTreeSHA != verifiedTree {
				t.Errorf("checkpoint = %+v, want push kind on %s carrying tree %s", cp, sliceBranch, verifiedTree)
			}
			if cp.baseSHA != headBefore {
				t.Errorf("base_sha = %s, want HEAD %s (the child base the verify ran on)", cp.baseSHA, headBefore)
			}
			if got := cprGit(t, repo, "rev-parse", cp.headSHA+"^{tree}"); got != verifiedTree {
				t.Errorf("synthesized commit tree = %s, want the verified tree %s", got, verifiedTree)
			}
			if got := cprGit(t, repo, "rev-parse", cp.headSHA+"^"); got != headBefore {
				t.Errorf("synthesized commit parent = %s, want HEAD %s", got, headBefore)
			}
			msg := cprGit(t, repo, "log", "-1", "--format=%B", cp.headSHA)
			if !strings.HasPrefix(msg, "feat(x): the slice") || !strings.Contains(msg, "Signed-off-by: fishhawk-dev[bot] <42+fishhawk-dev[bot]@users.noreply.github.com>") {
				t.Errorf("synthesized commit message lacks the subject or the DCO trailer:\n%s", msg)
			}
			if author := cprGit(t, repo, "log", "-1", "--format=%an <%ae>|%cn", cp.headSHA); author != "fishhawk-dev[bot] <42+fishhawk-dev[bot]@users.noreply.github.com>|fishhawk-dev[bot]" {
				t.Errorf("synthesized commit identity = %q", author)
			}
			if got, ok := refTarget(repo, ckRef); !ok || got != cp.headSHA || armedRef != ckRef {
				t.Errorf("checkpoint ref %s = %q (exists %t, returned %q), want %s", ckRef, got, ok, armedRef, cp.headSHA)
			}
			// Nothing about the checkout moved.
			if got := cprGit(t, repo, "rev-parse", "HEAD"); got != headBefore {
				t.Errorf("HEAD moved to %s", got)
			}
			if st := cprGit(t, repo, "status", "--porcelain"); st != "" {
				t.Errorf("work tree dirtied by the synthesis: %q", st)
			}
			// The reason the audit row carries names each pinned ref.
			suffix := checkpointFailureSuffix(armedRef, stashRef)
			if !strings.Contains(suffix, ckRef) || !strings.Contains(suffix, "fishhawk_retry_stage") {
				t.Errorf("failure suffix %q does not name the armed ref %s", suffix, ckRef)
			}
			if tc.wantStash && !strings.Contains(suffix, stRef) {
				t.Errorf("failure suffix %q does not name the stash ref %s", suffix, stRef)
			}
		})
	}
}

// TestCheckpointFailureSuffix pins the empty case: with nothing pinned the
// reason is byte-identical to before #4079.
func TestCheckpointFailureSuffix(t *testing.T) {
	if got := checkpointFailureSuffix("", ""); got != "" {
		t.Errorf("suffix with nothing pinned = %q, want empty", got)
	}
	if got := checkpointFailureSuffix("", "refs/x-stash"); strings.Contains(got, "fishhawk_retry_stage") || !strings.Contains(got, "git stash apply refs/x-stash") {
		t.Errorf("stash-only suffix = %q: must name the stash recovery and claim NO agent-free resume", got)
	}
}

// TestOpenPRAndShipArtifact_ChildBaseFetchFailure_ArmsAndNamesRefs is the
// #4079 incident reproduced through the REAL commit path: a decomposition child
// whose base (the consolidated branch) does not exist on the remote, so the real
// CommitAndPush stashes the edit and its real `git fetch` fails. With a verified
// tree in hand the stage arms a push-kind checkpoint and the returned reason
// names both pinned refs.
func TestOpenPRAndShipArtifact_ChildBaseFetchFailure_ArmsAndNamesRefs(t *testing.T) {
	repo, bare, _ := verifiedTreeRepo(t) // a.txt carries the uncommitted agent edit
	headBefore := cprGit(t, repo, "rev-parse", "HEAD")
	verifiedTree := treeWithEdit(t, repo, "a.txt", "agent change\n")
	withFakePROpenerOnly(t)
	fu := newFakeUploader(t)
	issued, err := fu.IssueKey(context.Background(), verifiedTreeRunID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	cfg := verifiedTreeCfg(repo, "true")
	cfg.decomposedFromRunID = childResumeParentRunID
	cfg.baseBranch = "fishhawk/run-deadbeef-consolidated" // never exists on the remote
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
	if want := childSliceBranch(childResumeParentRunID, runSliceIndex); cp.branch != want {
		t.Errorf("checkpoint branch = %q, want the slice branch %q", cp.branch, want)
	}
	if cp.baseSHA != headBefore || cprGit(t, repo, "rev-parse", cp.headSHA+"^{tree}") != verifiedTree {
		t.Errorf("checkpoint %+v does not hold the verified tree on HEAD %s", cp, headBefore)
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
	if _, rerr := exec.Command("git", "--git-dir="+bare, "rev-parse", "--verify", "refs/heads/"+cp.branch).Output(); rerr == nil {
		t.Errorf("slice branch %s reached the remote on a base-fetch failure", cp.branch)
	}
}

// TestPushFailureCheckpoint_PinsCheckpointRef (CONTROL K): the #3621
// ErrPushFailed arm, driven through openPRAndShipArtifact's error site, pins the
// held commit at checkpointRef and names it in the reason.
func TestPushFailureCheckpoint_PinsCheckpointRef(t *testing.T) {
	repo, _, branch, headSHA, treeSHA := pushResumeRepo(t)
	cprGit(t, repo, "checkout", branch) // CommitAndPush leaves the commit on HEAD when only the push fails
	fp := withFakePusher(t)
	fp.err = fmt.Errorf("gitops: push: %w", gitops.ErrPushFailed)
	withFakePROpenerOnly(t)
	fu := newFakeUploader(t)
	issued, err := fu.IssueKey(context.Background(), verifiedTreeRunID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	cfg := checkpointResumeCfg(repo)
	cp := pushCheckpoint{backendSupportsPushResume: true}
	var logSink strings.Builder
	err = openPRAndShipArtifact(context.Background(), cfg, &logSink, fu, issued, "", false, false, nil, false, treeSHA, "", nil, nil, nil, &cp)
	if !errors.Is(err, gitops.ErrPushFailed) {
		t.Fatalf("err = %v, want ErrPushFailed", err)
	}
	if !cp.armed || cp.headSHA != headSHA {
		t.Fatalf("checkpoint = %+v, want armed at %s\n%s", cp, headSHA, logSink.String())
	}
	ref := checkpointRef(cfg.runID, cfg.stageID)
	if got, ok := refTarget(repo, ref); !ok || got != headSHA {
		t.Errorf("checkpoint ref %s = %q (exists %t), want %s", ref, got, ok, headSHA)
	}
	if !strings.Contains(err.Error(), ref) {
		t.Errorf("reason does not name the pinned ref %s: %v", ref, err)
	}
}

// TestPushFailureCheckpoint_NotArmed_PinsNothing isolates the pin's own guard:
// an ordinary pre-push failure arms nothing, so nothing may be pinned, no pin
// may even be attempted, and the reason stays byte-identical.
func TestPushFailureCheckpoint_NotArmed_PinsNothing(t *testing.T) {
	repo, _, branch, _, treeSHA := pushResumeRepo(t)
	cprGit(t, repo, "checkout", branch)
	fp := withFakePusher(t)
	fp.err = errors.New("gitops: commit: boom")
	withFakePROpenerOnly(t)
	fu := newFakeUploader(t)
	issued, err := fu.IssueKey(context.Background(), verifiedTreeRunID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	cfg := checkpointResumeCfg(repo)
	cp := pushCheckpoint{backendSupportsPushResume: true}
	var logSink strings.Builder
	err = openPRAndShipArtifact(context.Background(), cfg, &logSink, fu, issued, "", false, false, nil, false, treeSHA, "", nil, nil, nil, &cp)
	if err == nil || err.Error() != "commit+push: gitops: commit: boom" {
		t.Fatalf("err = %v, want the unchanged commit+push reason", err)
	}
	if strings.Contains(logSink.String(), "checkpoint_ref") {
		t.Errorf("no checkpoint ref may be pinned or attempted on an unarmed failure:\n%s", logSink.String())
	}
	if _, ok := refTarget(repo, checkpointRef(cfg.runID, cfg.stageID)); ok {
		t.Error("checkpoint ref exists after an unarmed failure")
	}
}

// TestBaseFetchFailure_NilCheckpoint_PinsStashOnly: with no checkpoint
// out-parameter the arm still pins the stash and arms nothing.
func TestBaseFetchFailure_NilCheckpoint_PinsStashOnly(t *testing.T) {
	repo, _, stashSHA, verifiedTree := baseFetchArmRepo(t)
	cfg := childResumeCfg(repo)
	var logSink strings.Builder
	bfe := &gitops.BaseFetchError{Ref: "x", StashSHA: stashSHA, Err: errors.New("gitops: fetch x: boom")}
	armedRef, stashRef := maybeArmBaseFetchFailureCheckpoint(context.Background(), cfg, &logSink, nil, true, bfe, repo, "b", verifiedTree, "m", "", "")
	if armedRef != "" || stashRef != checkpointStashRef(cfg.runID, cfg.stageID) {
		t.Errorf("refs = %q, %q; want only the stash ref", armedRef, stashRef)
	}
	if strings.Contains(logSink.String(), "push_checkpoint") {
		t.Errorf("nothing may be armed or refused without a checkpoint:\n%s", logSink.String())
	}
}

// pinBothCheckpointRefs seeds both refs BY CONSTRUCTION so a resume test can
// assert they are released (or kept).
func pinBothCheckpointRefs(t *testing.T, repo string, cfg config, sha string) {
	t.Helper()
	cprGit(t, repo, "update-ref", checkpointRef(cfg.runID, cfg.stageID), sha)
	cprGit(t, repo, "update-ref", checkpointStashRef(cfg.runID, cfg.stageID), sha)
}

// TestChildPushResume_PublishesAndReportsPushed (CONTROL R1): a child push
// resume publishes the held commit on the slice branch, reports "pushed", NEVER
// opens a PR, and releases the checkpoint refs.
func TestChildPushResume_PublishesAndReportsPushed(t *testing.T) {
	repo, _, branch, headSHA, treeSHA := pushResumeRepo(t)
	base := cprGit(t, repo, "rev-parse", "main")
	cfg := childResumeCfg(repo)
	pinBothCheckpointRefs(t, repo, cfg, headSHA)
	fp := withFakePusher(t)
	fpr := withFakePROpenerOnly(t)
	fu := newFakeUploader(t)
	issued, err := fu.IssueKey(context.Background(), verifiedTreeRunID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	var logSink strings.Builder
	if code := openHeldCommitPR(context.Background(), cfg, headSHA, branch, base, resumeKindPush, treeSHA, "", "", &logSink, fu, issued); code != exitOK {
		t.Fatalf("exit = %d, want exitOK\n%s", code, logSink.String())
	}
	if fp.pushCommittedArgs == nil || fp.pushCommittedArgs.HeadSHA != headSHA || fp.pushCommittedArgs.Branch != branch {
		t.Fatalf("published %+v, want %s on %s", fp.pushCommittedArgs, headSHA, branch)
	}
	if fpr.gotArgs != nil {
		t.Fatalf("a child resume must NEVER open a pull request, got %+v", fpr.gotArgs)
	}
	if len(fu.gotPRCalls) != 1 {
		t.Fatalf("pull-request reports = %d, want exactly the pushed report", len(fu.gotPRCalls))
	}
	got := fu.gotPRCalls[0]
	if got.Outcome != "pushed" || got.Branch != branch || got.HeadSHA != headSHA || got.BaseSHA != base {
		t.Errorf("report = outcome %q branch %q head %q base %q, want pushed %s@%s from %s", got.Outcome, got.Branch, got.HeadSHA, got.BaseSHA, branch, headSHA, base)
	}
	if got.FilesChangedCount != 1 {
		t.Errorf("files_changed_count = %d, want 1 (a.txt, held commit vs its base)", got.FilesChangedCount)
	}
	for _, ref := range []string{checkpointRef(cfg.runID, cfg.stageID), checkpointStashRef(cfg.runID, cfg.stageID)} {
		if _, ok := refTarget(repo, ref); ok {
			t.Errorf("checkpoint ref %s survived a successful child resume", ref)
		}
	}
	if !strings.Contains(logSink.String(), `"event":"push_resume_child_pushed"`) {
		t.Errorf("missing push_resume_child_pushed:\n%s", logSink.String())
	}
}

// TestChildHeldCommit_RefusesNonPushKinds (CONTROL R2). The branch is
// PRE-PUBLISHED so the remote tip EQUALS the held commit: the pr_open tip guard
// would pass on its own, so only the child refusal stands between the served
// kind and an opened PR.
func TestChildHeldCommit_RefusesNonPushKinds(t *testing.T) {
	for _, kind := range []string{resumeKindPROpen, ""} {
		t.Run("kind_"+kind, func(t *testing.T) {
			repo, _, branch, headSHA, treeSHA := pushResumeRepo(t)
			cprGit(t, repo, "push", "origin", branch)
			fp := withFakePusher(t)
			fpr := withFakePROpenerOnly(t)
			fu := newFakeUploader(t)
			issued, err := fu.IssueKey(context.Background(), verifiedTreeRunID, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			var logSink strings.Builder
			code := openHeldCommitPR(context.Background(), childResumeCfg(repo), headSHA, branch, "base-sha-4079", kind, treeSHA, "", "", &logSink, fu, issued)
			if code != exitFailure {
				t.Fatalf("exit = %d, want exitFailure\n%s", code, logSink.String())
			}
			if fpr.gotArgs != nil {
				t.Fatalf("a child must never open a PR, got %+v", fpr.gotArgs)
			}
			if fp.pushCommittedArgs != nil {
				t.Errorf("nothing may be pushed on a refusal, got %+v", fp.pushCommittedArgs)
			}
			got := reportedFailureBody(t, fu)
			if !strings.HasPrefix(got.Reason, refuseChildResumeKindUnsupported+":") {
				t.Errorf("reason = %q, want the %q token", got.Reason, refuseChildResumeKindUnsupported)
			}
			if got.ResumeKind != resumeKindPushDiscarded {
				t.Errorf("resume_kind = %q, want %q (a permanent refusal)", got.ResumeKind, resumeKindPushDiscarded)
			}
		})
	}
}

// TestChildPushResume_ReportFailureRearmsAsPush (CONTROL R3): the publish
// lands, then the pushed report fails. The failure report must re-arm as PUSH
// (not the standalone pr_open downgrade, which would send the next retry to open
// a PR from the slice branch), and the checkpoint refs must survive.
func TestChildPushResume_ReportFailureRearmsAsPush(t *testing.T) {
	repo, _, branch, headSHA, treeSHA := pushResumeRepo(t)
	cfg := childResumeCfg(repo)
	pinBothCheckpointRefs(t, repo, cfg, headSHA)
	fp := withFakePusher(t)
	fpr := withFakePROpenerOnly(t)
	fu := newFakeUploader(t)
	fu.prErrSeq = []error{errors.New("backend 502 on the pushed report"), nil}
	issued, err := fu.IssueKey(context.Background(), verifiedTreeRunID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	var logSink strings.Builder
	if code := openHeldCommitPR(context.Background(), cfg, headSHA, branch, "base-sha-4079", resumeKindPush, treeSHA, "", "", &logSink, fu, issued); code != exitFailure {
		t.Fatalf("exit = %d, want exitFailure\n%s", code, logSink.String())
	}
	if fp.pushCommittedArgs == nil {
		t.Fatal("the publish must have landed before the report failed (the shape under test)")
	}
	if fpr.gotArgs != nil {
		t.Fatalf("a child must never open a PR, got %+v", fpr.gotArgs)
	}
	if len(fu.gotPRCalls) != 2 || fu.gotPRCalls[0].Outcome != "pushed" {
		t.Fatalf("reports = %+v, want the pushed report then the failure report", fu.gotPRCalls)
	}
	got := fu.gotPRCalls[1]
	if got.Outcome != "failed" || got.ResumeKind != resumeKindPush {
		t.Errorf("failure report outcome %q resume_kind %q, want failed + %q", got.Outcome, got.ResumeKind, resumeKindPush)
	}
	if got.HeadSHA != headSHA || got.Branch != branch || got.TreeSHA != treeSHA {
		t.Errorf("re-armed checkpoint = %s@%s tree %s, want %s@%s tree %s", got.Branch, got.HeadSHA, got.TreeSHA, branch, headSHA, treeSHA)
	}
	if _, ok := refTarget(repo, checkpointRef(cfg.runID, cfg.stageID)); !ok {
		t.Error("the checkpoint ref must survive a failed resume")
	}
}

// TestPushResume_StandaloneSuccessReleasesCheckpointRefs: the standalone push
// resume releases the refs once its artifact lands — including the #4190
// verify-head pin, which every held-commit success path must drop.
func TestPushResume_StandaloneSuccessReleasesCheckpointRefs(t *testing.T) {
	repo, _, branch, headSHA, treeSHA := pushResumeRepo(t)
	cfg := checkpointResumeCfg(repo)
	pinBothCheckpointRefs(t, repo, cfg, headSHA)
	cprGit(t, repo, "update-ref", checkpointVerifyRef(cfg.runID, cfg.stageID), headSHA)
	withFakePusher(t)
	withFakePROpenerOnly(t)
	fu := newFakeUploader(t)
	issued, err := fu.IssueKey(context.Background(), verifiedTreeRunID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	var logSink strings.Builder
	if code := openHeldCommitPR(context.Background(), cfg, headSHA, branch, "base-sha-4079", resumeKindPush, treeSHA, "", "", &logSink, fu, issued); code != exitOK {
		t.Fatalf("exit = %d\n%s", code, logSink.String())
	}
	for _, ref := range []string{checkpointRef(cfg.runID, cfg.stageID), checkpointStashRef(cfg.runID, cfg.stageID), checkpointVerifyRef(cfg.runID, cfg.stageID)} {
		if _, ok := refTarget(repo, ref); ok {
			t.Errorf("checkpoint ref %s survived a successful resume", ref)
		}
	}
	if !strings.Contains(logSink.String(), `"event":"checkpoint_refs_released"`) {
		t.Errorf("missing checkpoint_refs_released:\n%s", logSink.String())
	}
}

// TestCheckpointRefHelpers covers the helpers' own refusals: an empty object id
// is refused before git runs, an invalid ref name is git's refusal, and
// releasing absent refs is a silent no-op.
func TestCheckpointRefHelpers(t *testing.T) {
	repo, _, _, headSHA, _ := pushResumeRepo(t)
	ctx := context.Background()
	if err := pinCheckpointRef(ctx, repo, checkpointRef("r", "s"), ""); err == nil {
		t.Error("an empty sha must be refused")
	}
	if err := pinCheckpointRef(ctx, repo, checkpointRef("", "s"), headSHA); err == nil {
		t.Error("an invalid ref name (empty run id) must fail")
	}
	var logSink strings.Builder
	releaseCheckpointRefs(ctx, repo, "absent-run", "absent-stage", &logSink)
	if logSink.String() != "" {
		t.Errorf("releasing absent refs must log nothing, got:\n%s", logSink.String())
	}
	if checkpointStashRef("r", "s") != "refs/fishhawk/checkpoints/r/s-stash" {
		t.Errorf("stash ref = %q", checkpointStashRef("r", "s"))
	}
}

// TestSynthesizeVerifiedCommit_Refusals covers the synthesizer's preconditions,
// each refused before any object is written.
func TestSynthesizeVerifiedCommit_Refusals(t *testing.T) {
	repo, _, _, _, treeSHA := pushResumeRepo(t)
	ctx := context.Background()
	// Each refusal is asserted by its CAUSE: git would also fail most of these
	// further down, so "an error" alone would not tell the guard from git.
	if _, _, err := synthesizeVerifiedCommit(ctx, repo, "", "msg", "", ""); err == nil || !strings.Contains(err.Error(), "no verified tree") {
		t.Errorf("an empty verified tree must be refused by name, got %v", err)
	}
	if _, _, err := synthesizeVerifiedCommit(ctx, repo, treeSHA, " \n", "", ""); err == nil || !strings.Contains(err.Error(), "empty commit message") {
		t.Errorf("an empty commit message must be refused by name, got %v", err)
	}
	unborn := t.TempDir() // a repository with no commit: HEAD does not resolve
	cprGit(t, unborn, "init", "--initial-branch=main")
	if _, _, err := synthesizeVerifiedCommit(ctx, unborn, treeSHA, "msg", "", ""); err == nil || !strings.Contains(err.Error(), "resolve HEAD") {
		t.Errorf("an unresolvable HEAD must be refused by name, got %v", err)
	}
	// Defaults: an empty identity falls back to the gitops bot identity.
	sha, _, err := synthesizeVerifiedCommit(ctx, repo, treeSHA, "msg", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := cprGit(t, repo, "log", "-1", "--format=%an <%ae>", sha); got != gitops.DefaultAuthorName+" <"+gitops.DefaultAuthorEmail+">" {
		t.Errorf("default identity = %q", got)
	}
}

// TestHeldCommitFilesChanged_Unavailable: an unresolvable base degrades to 0
// with the diagnosable event, never a failure.
func TestHeldCommitFilesChanged_Unavailable(t *testing.T) {
	repo, _, _, headSHA, _ := pushResumeRepo(t)
	var logSink strings.Builder
	if n := heldCommitFilesChanged(context.Background(), repo, "not-a-rev", headSHA, "r", "s", &logSink); n != 0 {
		t.Errorf("count = %d, want 0", n)
	}
	if !strings.Contains(logSink.String(), "child_push_files_changed_unavailable") {
		t.Errorf("missing child_push_files_changed_unavailable:\n%s", logSink.String())
	}
}
