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
	reasons, err := strandedFixupWork(context.Background(), ".", "base", nil, nil)
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
	reasons, err := strandedFixupWork(context.Background(), ".", "basetip", nil, nil)
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
	reasons, err := strandedFixupWork(context.Background(), ".", "base", nil, nil)
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
	reasons, err := strandedFixupWork(context.Background(), ".", "base", nil, nil)
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
	reasons, err := strandedFixupWork(context.Background(), ".", "base", nil, nil)
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
	_, err := strandedFixupWork(context.Background(), ".", "base", nil, nil)
	if err == nil || !errors.Is(err, sentinel) {
		t.Fatalf("want the probe error propagated, got %v", err)
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
