package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/runner/internal/gitops"
)

// fixuplandGit runs a git command in dir, failing the test on error.
func fixuplandGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	full := append([]string{"-C", dir}, args...)
	out, err := exec.Command("git", full...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// fixuplandRepo initializes a temp git repo with one commit and returns its
// directory and the initial commit SHA.
func fixuplandRepo(t *testing.T) (dir, initialSHA string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir = t.TempDir()
	fixuplandGit(t, dir, "init", "--initial-branch=main")
	fixuplandGit(t, dir, "config", "user.name", "init")
	fixuplandGit(t, dir, "config", "user.email", "init@example.com")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fixuplandGit(t, dir, "add", "-A")
	fixuplandGit(t, dir, "commit", "-m", "initial")
	initialSHA = fixuplandGit(t, dir, "rev-parse", "HEAD")
	return dir, initialSHA
}

// --- gitStashList (real repo) --------------------------------------------

func TestGitStashList_EmptyStashIsEmptySliceNilError(t *testing.T) {
	dir, _ := fixuplandRepo(t)
	got, err := gitStashList(context.Background(), dir)
	if err != nil {
		t.Fatalf("an absent stash ref must NOT error, got: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("empty stash must be an empty slice, got %v", got)
	}
}

func TestGitStashList_TwoStashesNewestFirstWithSubjects(t *testing.T) {
	dir, _ := fixuplandRepo(t)
	// First stash.
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("edit one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fixuplandGit(t, dir, "stash", "push", "-m", "first")
	// Second stash.
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("edit two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fixuplandGit(t, dir, "stash", "push", "-m", "second")

	got, err := gitStashList(context.Background(), dir)
	if err != nil {
		t.Fatalf("gitStashList: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 stash entries, got %d: %v", len(got), got)
	}
	// Newest first.
	if !strings.Contains(got[0].Subject, "second") {
		t.Errorf("newest stash first: got[0].Subject = %q, want it to mention 'second'", got[0].Subject)
	}
	if !strings.Contains(got[1].Subject, "first") {
		t.Errorf("got[1].Subject = %q, want it to mention 'first'", got[1].Subject)
	}
	for i, e := range got {
		if len(e.SHA) != 40 {
			t.Errorf("entry %d SHA %q is not a 40-hex commit sha", i, e.SHA)
		}
	}
}

// --- reflogStrandedCommits (real repo) — the condition-1 incident shape ---

// TestReflogStrandedCommits_DanglingVerifyWipCommit reproduces the #2884
// incident BY CONSTRUCTION: commit onto the branch, then `git reset --soft`
// back to the base tip (leaving the commit dangling), create NO stash. The HEAD
// probe reads EQUAL (HEAD is back at the base tip) and no stash exists, so ONLY
// this provenance walk surfaces the dangling commit.
func TestReflogStrandedCommits_DanglingVerifyWipCommit(t *testing.T) {
	dir, baseTip := fixuplandRepo(t)
	preReflog, err := gitReflogCommits(context.Background(), dir)
	if err != nil {
		t.Fatalf("pre reflog: %v", err)
	}
	// The pass: commit a `fishhawk verify wip`, then reset --soft it away.
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("wip work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fixuplandGit(t, dir, "add", "-A")
	fixuplandGit(t, dir, "commit", "-m", "fishhawk verify wip")
	danglingSHA := fixuplandGit(t, dir, "rev-parse", "HEAD")
	fixuplandGit(t, dir, "reset", "--soft", baseTip)
	// HEAD is back at the base tip; prove the two cheap probes would miss it.
	if head := fixuplandGit(t, dir, "rev-parse", "HEAD"); head != baseTip {
		t.Fatalf("setup invariant: HEAD should be back at the base tip, got %q want %q", head, baseTip)
	}

	stranded, err := reflogStrandedCommits(context.Background(), dir, baseTip, preReflog)
	if err != nil {
		t.Fatalf("reflogStrandedCommits: %v", err)
	}
	found := false
	for _, e := range stranded {
		if e.SHA == danglingSHA {
			found = true
			if !strings.Contains(e.Subject, "fishhawk verify wip") {
				t.Errorf("dangling subject = %q, want it to mention the verify wip commit", e.Subject)
			}
		}
	}
	if !found {
		t.Fatalf("dangling commit %s not reported; got %v", danglingSHA, stranded)
	}
}

// TestReflogStrandedCommits_CleanPassReportsNothing is the no-regression
// control: a pass that creates no dangling commit reports none.
func TestReflogStrandedCommits_CleanPassReportsNothing(t *testing.T) {
	dir, baseTip := fixuplandRepo(t)
	preReflog, err := gitReflogCommits(context.Background(), dir)
	if err != nil {
		t.Fatalf("pre reflog: %v", err)
	}
	stranded, err := reflogStrandedCommits(context.Background(), dir, baseTip, preReflog)
	if err != nil {
		t.Fatalf("reflogStrandedCommits: %v", err)
	}
	if len(stranded) != 0 {
		t.Errorf("a clean pass must report no stranded commits, got %v", stranded)
	}
}

// --- strandedFixupWork (seamed) — one case per enumerated mode -----------

// withFixupSeams installs canned fixup helper seams, restoring on cleanup.
func withFixupSeams(t *testing.T,
	stash func(context.Context, string) ([]stashEntry, error),
	head func(context.Context, string) (string, error),
	reflog func(context.Context, string, string, []stashEntry) ([]stashEntry, error),
) {
	t.Helper()
	origStash, origHead, origReflog := fixupStashList, fixupLocalHead, fixupStrandedReflog
	if stash != nil {
		fixupStashList = stash
	}
	if head != nil {
		fixupLocalHead = head
	}
	if reflog != nil {
		fixupStrandedReflog = reflog
	}
	t.Cleanup(func() {
		fixupStashList, fixupLocalHead, fixupStrandedReflog = origStash, origHead, origReflog
	})
}

func cleanReflog(context.Context, string, string, []stashEntry) ([]stashEntry, error) {
	return nil, nil
}

func TestStrandedFixupWork_NetNewStashOnly(t *testing.T) {
	withFixupSeams(t,
		func(context.Context, string) ([]stashEntry, error) {
			return []stashEntry{{SHA: "stashsha00000000000000000000000000000000", Subject: "WIP on fishhawk/run"}}, nil
		},
		func(context.Context, string) (string, error) { return "base", nil },
		cleanReflog,
	)
	reasons, err := strandedFixupWork(context.Background(), ".", "base", nil, nil, "")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(reasons) != 1 || !strings.Contains(reasons[0], "stashsha") {
		t.Fatalf("want one reason naming the stash sha, got %v", reasons)
	}
}

func TestStrandedFixupWork_AdvancedHeadOnly(t *testing.T) {
	withFixupSeams(t,
		func(context.Context, string) ([]stashEntry, error) { return nil, nil },
		func(context.Context, string) (string, error) { return "advancedhead", nil },
		cleanReflog,
	)
	reasons, err := strandedFixupWork(context.Background(), ".", "basetip", nil, nil, "")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(reasons) != 1 || !strings.Contains(reasons[0], "basetip") || !strings.Contains(reasons[0], "advancedhead") {
		t.Fatalf("want one reason naming expected and actual heads, got %v", reasons)
	}
}

func TestStrandedFixupWork_DanglingCommitOnly(t *testing.T) {
	withFixupSeams(t,
		func(context.Context, string) ([]stashEntry, error) { return nil, nil },
		func(context.Context, string) (string, error) { return "base", nil },
		func(context.Context, string, string, []stashEntry) ([]stashEntry, error) {
			return []stashEntry{{SHA: "danglingsha0000000000000000000000000000", Subject: "fishhawk verify wip"}}, nil
		},
	)
	reasons, err := strandedFixupWork(context.Background(), ".", "base", nil, nil, "")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(reasons) != 1 || !strings.Contains(reasons[0], "danglingsha") {
		t.Fatalf("want one reason naming the dangling sha, got %v", reasons)
	}
}

func TestStrandedFixupWork_AllThreeModes(t *testing.T) {
	withFixupSeams(t,
		func(context.Context, string) ([]stashEntry, error) {
			return []stashEntry{{SHA: "stashsha", Subject: "WIP"}}, nil
		},
		func(context.Context, string) (string, error) { return "advanced", nil },
		func(context.Context, string, string, []stashEntry) ([]stashEntry, error) {
			return []stashEntry{{SHA: "danglingsha", Subject: "verify wip"}}, nil
		},
	)
	reasons, err := strandedFixupWork(context.Background(), ".", "base", nil, nil, "")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(reasons) != 3 {
		t.Fatalf("want three reasons, got %d: %v", len(reasons), reasons)
	}
}

func TestStrandedFixupWork_CleanReportsNothing(t *testing.T) {
	withFixupSeams(t,
		func(context.Context, string) ([]stashEntry, error) { return nil, nil },
		func(context.Context, string) (string, error) { return "base", nil },
		cleanReflog,
	)
	reasons, err := strandedFixupWork(context.Background(), ".", "base", nil, nil, "")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(reasons) != 0 {
		t.Fatalf("a clean pass must report no reasons, got %v", reasons)
	}
}

func TestStrandedFixupWork_ProbeErrorPropagates(t *testing.T) {
	sentinel := errors.New("boom")
	withFixupSeams(t,
		func(context.Context, string) ([]stashEntry, error) { return nil, sentinel },
		func(context.Context, string) (string, error) { return "base", nil },
		cleanReflog,
	)
	_, err := strandedFixupWork(context.Background(), ".", "base", nil, nil, "")
	if err == nil || !errors.Is(err, sentinel) {
		t.Fatalf("want the probe error propagated, got %v", err)
	}
}

// --- verifiedFixupWorkStranded (probe 4, #3022) --------------------------

// withFixupBaseTree swaps the probe-4 base-tree seam, restoring on cleanup.
func withFixupBaseTree(t *testing.T, fn func(context.Context, string, string) (string, error)) {
	t.Helper()
	orig := fixupBaseTreeOf
	fixupBaseTreeOf = fn
	t.Cleanup(func() { fixupBaseTreeOf = orig })
}

// TestVerifiedFixupWorkStranded_CommitResetNoStashNoChanges reproduces the
// #3022 incident shape BY CONSTRUCTION in a REAL temp repo: on the base tip,
// write an edit, `git add`, `git commit` (the throwaway shape), capture that
// commit's tree, then `git reset --hard` back to the base tip so NO stash
// exists and HEAD equals the base tip. The verify gate's certified tree
// (the captured commit tree) then differs from the base tip's tree, so the
// probe must fire naming both tree hashes.
func TestVerifiedFixupWorkStranded_CommitResetNoStashNoChanges(t *testing.T) {
	dir, baseTip := fixuplandRepo(t)
	baseTree := fixuplandGit(t, dir, "rev-parse", baseTip+"^{tree}")

	// The pass: commit in-scope work, capture its tree, then hard-reset it away.
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("verified fix work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fixuplandGit(t, dir, "add", "-A")
	fixuplandGit(t, dir, "commit", "-m", "fishhawk verify wip")
	verifiedTree := fixuplandGit(t, dir, "rev-parse", "HEAD^{tree}")
	fixuplandGit(t, dir, "reset", "--hard", baseTip)

	// Positively assert the incident shape: HEAD at the base tip, clean working
	// tree, NO stash — the state probes 1 and 3 read as clean.
	if head := fixuplandGit(t, dir, "rev-parse", "HEAD"); head != baseTip {
		t.Fatalf("setup: HEAD should be the base tip, got %q want %q", head, baseTip)
	}
	if status := fixuplandGit(t, dir, "status", "--porcelain"); status != "" {
		t.Fatalf("setup: working tree must be clean, got %q", status)
	}
	if stash := fixuplandGit(t, dir, "stash", "list"); stash != "" {
		t.Fatalf("setup: no stash must exist, got %q", stash)
	}
	if verifiedTree == baseTree {
		t.Fatalf("setup: the verified tree must differ from the base tree (got both %q)", baseTree)
	}

	reason, err := verifiedFixupWorkStranded(context.Background(), dir, baseTip, verifiedTree)
	if err != nil {
		t.Fatalf("verifiedFixupWorkStranded: %v", err)
	}
	if reason == "" {
		t.Fatal("probe must fire: verified work is on neither the working tree nor the branch")
	}
	if !strings.Contains(reason, verifiedTree) || !strings.Contains(reason, baseTree) {
		t.Errorf("reason must name both the certified tree and the base tree, got: %q", reason)
	}
}

// TestVerifiedFixupWorkStranded_EmptyWitnessAbstains: an empty verifiedTreeSHA
// (no verify gate ran / nothing staged) returns no reason and no error, and
// never touches the repo.
func TestVerifiedFixupWorkStranded_EmptyWitnessAbstains(t *testing.T) {
	withFixupBaseTree(t, func(context.Context, string, string) (string, error) {
		t.Fatal("base-tree resolve must not be called when the witness is empty")
		return "", nil
	})
	reason, err := verifiedFixupWorkStranded(context.Background(), ".", "basetip", "")
	if err != nil || reason != "" {
		t.Fatalf("empty witness must abstain, got reason=%q err=%v", reason, err)
	}
}

// TestVerifiedFixupWorkStranded_EmptyBaseTipAbstains: no base tip to compare
// against abstains (the fixup_base_tip_unresolved log already records it).
func TestVerifiedFixupWorkStranded_EmptyBaseTipAbstains(t *testing.T) {
	withFixupBaseTree(t, func(context.Context, string, string) (string, error) {
		t.Fatal("base-tree resolve must not be called when the base tip is empty")
		return "", nil
	})
	reason, err := verifiedFixupWorkStranded(context.Background(), ".", "", "verifiedtree")
	if err != nil || reason != "" {
		t.Fatalf("empty base tip must abstain, got reason=%q err=%v", reason, err)
	}
}

// TestVerifiedFixupWorkStranded_TreeEqualsBaseIsClean: the certified tree is
// byte-identical to the base tip's tree, so the pass produced no in-scope work
// and the probe abstains.
func TestVerifiedFixupWorkStranded_TreeEqualsBaseIsClean(t *testing.T) {
	dir, baseTip := fixuplandRepo(t)
	baseTree := fixuplandGit(t, dir, "rev-parse", baseTip+"^{tree}")
	reason, err := verifiedFixupWorkStranded(context.Background(), dir, baseTip, baseTree)
	if err != nil {
		t.Fatalf("verifiedFixupWorkStranded: %v", err)
	}
	if reason != "" {
		t.Errorf("a tree equal to the base tree must abstain, got: %q", reason)
	}
}

// TestVerifiedFixupWorkStranded_BaseTreeResolveErrorFailsClosed: an unresolvable
// base tree FAILS CLOSED with the error propagated, never a silent clean verdict.
func TestVerifiedFixupWorkStranded_BaseTreeResolveErrorFailsClosed(t *testing.T) {
	sentinel := errors.New("rev-parse: bad object")
	withFixupBaseTree(t, func(context.Context, string, string) (string, error) {
		return "", sentinel
	})
	reason, err := verifiedFixupWorkStranded(context.Background(), ".", "basetip", "verifiedtree")
	if !errors.Is(err, sentinel) {
		t.Fatalf("a base-tree resolve failure must propagate (never a clean verdict), got reason=%q err=%v", reason, err)
	}
	if reason != "" {
		t.Errorf("no reason must be returned on the fail-closed path, got: %q", reason)
	}
}

// TestStrandedFixupWork_VerifiedTreeStrandedOnly proves probe 4 fires
// INDEPENDENTLY of probes 1-3: a clean stash/HEAD/reflog with a certified tree
// that differs from the base tree yields exactly ONE reason.
func TestStrandedFixupWork_VerifiedTreeStrandedOnly(t *testing.T) {
	withFixupSeams(t,
		func(context.Context, string) ([]stashEntry, error) { return nil, nil },
		func(context.Context, string) (string, error) { return "base", nil },
		cleanReflog,
	)
	withFixupBaseTree(t, func(context.Context, string, string) (string, error) {
		return "basetree", nil
	})
	reasons, err := strandedFixupWork(context.Background(), ".", "base", nil, nil, "verifiedtree")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(reasons) != 1 || !strings.Contains(reasons[0], "verifiedtree") || !strings.Contains(reasons[0], "basetree") {
		t.Fatalf("want exactly one probe-4 reason naming both trees, got %v", reasons)
	}
}

// TestStrandedFixupWork_VerifiedTreeEqualsBaseReportsNothing: the clean path
// with a base-EQUAL certified tree still reports zero reasons.
func TestStrandedFixupWork_VerifiedTreeEqualsBaseReportsNothing(t *testing.T) {
	withFixupSeams(t,
		func(context.Context, string) ([]stashEntry, error) { return nil, nil },
		func(context.Context, string) (string, error) { return "base", nil },
		cleanReflog,
	)
	withFixupBaseTree(t, func(context.Context, string, string) (string, error) {
		return "basetree", nil
	})
	reasons, err := strandedFixupWork(context.Background(), ".", "base", nil, nil, "basetree")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(reasons) != 0 {
		t.Fatalf("a base-equal certified tree must report no reasons, got %v", reasons)
	}
}

// TestStrandedFixupWork_VerifiedTreeProbeErrorPropagates is the probe-4 analogue
// of TestStrandedFixupWork_ProbeErrorPropagates (#3022 review, untested-path): a
// fixupBaseTreeOf seam failure must propagate THROUGH strandedFixupWork wrapped
// as "verified-tree probe:", so the category-C fail-closed path is pinned at the
// call-site boundary (not only at the helper level in
// TestVerifiedFixupWorkStranded_BaseTreeResolveErrorFailsClosed). Probes 1-3 read
// clean and the witness/base-tip are non-empty, so probe 4 is the sole error
// source.
func TestStrandedFixupWork_VerifiedTreeProbeErrorPropagates(t *testing.T) {
	sentinel := errors.New("rev-parse: bad object")
	withFixupSeams(t,
		func(context.Context, string) ([]stashEntry, error) { return nil, nil },
		func(context.Context, string) (string, error) { return "base", nil },
		cleanReflog,
	)
	withFixupBaseTree(t, func(context.Context, string, string) (string, error) {
		return "", sentinel
	})
	_, err := strandedFixupWork(context.Background(), ".", "base", nil, nil, "verifiedtree")
	if err == nil || !errors.Is(err, sentinel) {
		t.Fatalf("want the probe-4 error propagated from the call site, got %v", err)
	}
	if !strings.Contains(err.Error(), "verified-tree probe:") {
		t.Errorf("want the caller's verified-tree probe wrap, got %v", err)
	}
}

// --- verifyFixupPushLanded (seamed) — error IDENTITY per mode -------------

func withFixupRemoteTip(t *testing.T, fn func(context.Context, string, string, string, string) (string, error)) {
	t.Helper()
	orig := fixupRemoteBranchTip
	fixupRemoteBranchTip = fn
	t.Cleanup(func() { fixupRemoteBranchTip = orig })
}

func TestVerifyFixupPushLanded_Match(t *testing.T) {
	withFixupRemoteTip(t, func(context.Context, string, string, string, string) (string, error) {
		return "pushed-head", nil
	})
	if err := verifyFixupPushLanded(context.Background(), ".", "https://x/y", "br", "tok", "pushed-head"); err != nil {
		t.Fatalf("a matching remote tip must land, got: %v", err)
	}
}

func TestVerifyFixupPushLanded_Mismatch(t *testing.T) {
	withFixupRemoteTip(t, func(context.Context, string, string, string, string) (string, error) {
		return "some-other-head", nil
	})
	err := verifyFixupPushLanded(context.Background(), ".", "https://x/y", "br", "tok", "pushed-head")
	if !errors.Is(err, gitops.ErrFixupPushNotLanded) {
		t.Fatalf("a mismatching tip must be ErrFixupPushNotLanded, got: %v", err)
	}
	if !strings.Contains(err.Error(), "pushed-head") || !strings.Contains(err.Error(), "some-other-head") {
		t.Errorf("error must name both heads, got: %v", err)
	}
}

func TestVerifyFixupPushLanded_AbsentBranch(t *testing.T) {
	withFixupRemoteTip(t, func(context.Context, string, string, string, string) (string, error) {
		return "", nil // absent branch: ls-remote exits 0, empty
	})
	err := verifyFixupPushLanded(context.Background(), ".", "https://x/y", "br", "tok", "pushed-head")
	if !errors.Is(err, gitops.ErrFixupPushNotLanded) {
		t.Fatalf("an absent branch must be ErrFixupPushNotLanded, got: %v", err)
	}
	if !strings.Contains(err.Error(), "absent") {
		t.Errorf("error must name the absent tip, got: %v", err)
	}
}

func TestVerifyFixupPushLanded_UnreadableRemoteIsInfra(t *testing.T) {
	withFixupRemoteTip(t, func(context.Context, string, string, string, string) (string, error) {
		return "", errors.New("ls-remote: connection refused")
	})
	err := verifyFixupPushLanded(context.Background(), ".", "https://x/y", "br", "tok", "pushed-head")
	if !errors.Is(err, gitops.ErrVerifyInfraFailure) {
		t.Fatalf("an unreadable remote must be ErrVerifyInfraFailure (category C), got: %v", err)
	}
	if errors.Is(err, gitops.ErrFixupPushNotLanded) {
		t.Fatalf("an infra read failure must NOT be mistaken for a moved branch: %v", err)
	}
}
