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

// --- strandedFixupWork (seamed) — one case per enumerated mode -----------

// withFixupSeams installs canned fixup helper seams, restoring on cleanup.
func withFixupSeams(t *testing.T,
	stash func(context.Context, string) ([]stashEntry, error),
	head func(context.Context, string) (string, error),
) {
	t.Helper()
	origStash, origHead := fixupStashList, fixupLocalHead
	if stash != nil {
		fixupStashList = stash
	}
	if head != nil {
		fixupLocalHead = head
	}
	t.Cleanup(func() {
		fixupStashList, fixupLocalHead = origStash, origHead
	})
}

func TestStrandedFixupWork_NetNewStashOnly(t *testing.T) {
	withFixupSeams(t,
		func(context.Context, string) ([]stashEntry, error) {
			return []stashEntry{{SHA: "stashsha00000000000000000000000000000000", Subject: "WIP on fishhawk/run"}}, nil
		},
		func(context.Context, string) (string, error) { return "base", nil },
	)
	reasons, err := strandedFixupWork(context.Background(), ".", "base", nil, "")
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
	)
	reasons, err := strandedFixupWork(context.Background(), ".", "basetip", nil, "")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(reasons) != 1 || !strings.Contains(reasons[0], "basetip") || !strings.Contains(reasons[0], "advancedhead") {
		t.Fatalf("want one reason naming expected and actual heads, got %v", reasons)
	}
}

// TestStrandedFixupWork_AllThreeModes drives all THREE surviving probes at once
// and asserts each reason's OWN identifying substring, not just the count
// (#3023). A count-only assertion stays green if a probe is silently dropped
// from strandedFixupWork during a signature change and another probe emits two
// reasons; the per-reason substrings cannot.
func TestStrandedFixupWork_AllThreeModes(t *testing.T) {
	withFixupSeams(t,
		func(context.Context, string) ([]stashEntry, error) {
			return []stashEntry{{SHA: "stashsha", Subject: "WIP"}}, nil
		},
		func(context.Context, string) (string, error) { return "advancedhead", nil },
	)
	withFixupBaseTree(t, func(context.Context, string, string) (string, error) {
		return "basetree", nil
	})
	reasons, err := strandedFixupWork(context.Background(), ".", "basetip", nil, "verifiedtree")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(reasons) != 3 {
		t.Fatalf("want three reasons, got %d: %v", len(reasons), reasons)
	}
	joined := strings.Join(reasons, "\n")
	// Probe 1: the net-new stash sha.
	if !strings.Contains(reasons[0], "stashsha") {
		t.Errorf("probe 1 reason must name the stash sha, got %q (all: %s)", reasons[0], joined)
	}
	// Probe 2: BOTH heads.
	if !strings.Contains(reasons[1], "basetip") || !strings.Contains(reasons[1], "advancedhead") {
		t.Errorf("probe 2 reason must name expected and actual heads, got %q (all: %s)", reasons[1], joined)
	}
	// Probe 3: BOTH tree hashes.
	if !strings.Contains(reasons[2], "verifiedtree") || !strings.Contains(reasons[2], "basetree") {
		t.Errorf("probe 3 reason must name the certified and base trees, got %q (all: %s)", reasons[2], joined)
	}
}

func TestStrandedFixupWork_CleanReportsNothing(t *testing.T) {
	withFixupSeams(t,
		func(context.Context, string) ([]stashEntry, error) { return nil, nil },
		func(context.Context, string) (string, error) { return "base", nil },
	)
	reasons, err := strandedFixupWork(context.Background(), ".", "base", nil, "")
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
	)
	_, err := strandedFixupWork(context.Background(), ".", "base", nil, "")
	if err == nil || !errors.Is(err, sentinel) {
		t.Fatalf("want the probe error propagated, got %v", err)
	}
}

// --- verifiedFixupWorkStranded (probe 3, #3022) --------------------------

// withFixupBaseTree swaps the probe-3 base-tree seam, restoring on cleanup.
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

// TestStrandedFixupWork_VerifiedTreeStrandedOnly proves probe 3 fires
// INDEPENDENTLY of probes 1-2: a clean stash and HEAD with a certified tree
// that differs from the base tree yields exactly ONE reason.
func TestStrandedFixupWork_VerifiedTreeStrandedOnly(t *testing.T) {
	withFixupSeams(t,
		func(context.Context, string) ([]stashEntry, error) { return nil, nil },
		func(context.Context, string) (string, error) { return "base", nil },
	)
	withFixupBaseTree(t, func(context.Context, string, string) (string, error) {
		return "basetree", nil
	})
	reasons, err := strandedFixupWork(context.Background(), ".", "base", nil, "verifiedtree")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(reasons) != 1 || !strings.Contains(reasons[0], "verifiedtree") || !strings.Contains(reasons[0], "basetree") {
		t.Fatalf("want exactly one probe-3 reason naming both trees, got %v", reasons)
	}
}

// TestStrandedFixupWork_VerifiedTreeEqualsBaseReportsNothing: the clean path
// with a base-EQUAL certified tree still reports zero reasons.
func TestStrandedFixupWork_VerifiedTreeEqualsBaseReportsNothing(t *testing.T) {
	withFixupSeams(t,
		func(context.Context, string) ([]stashEntry, error) { return nil, nil },
		func(context.Context, string) (string, error) { return "base", nil },
	)
	withFixupBaseTree(t, func(context.Context, string, string) (string, error) {
		return "basetree", nil
	})
	reasons, err := strandedFixupWork(context.Background(), ".", "base", nil, "basetree")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(reasons) != 0 {
		t.Fatalf("a base-equal certified tree must report no reasons, got %v", reasons)
	}
}

// TestStrandedFixupWork_VerifiedTreeProbeErrorPropagates is the probe-3 analogue
// of TestStrandedFixupWork_ProbeErrorPropagates (#3022 review, untested-path): a
// fixupBaseTreeOf seam failure must propagate THROUGH strandedFixupWork wrapped
// as "verified-tree probe:", so the category-C fail-closed path is pinned at the
// call-site boundary (not only at the helper level in
// TestVerifiedFixupWorkStranded_BaseTreeResolveErrorFailsClosed). Probes 1-2 read
// clean and the witness/base-tip are non-empty, so probe 3 is the sole error
// source.
func TestStrandedFixupWork_VerifiedTreeProbeErrorPropagates(t *testing.T) {
	sentinel := errors.New("rev-parse: bad object")
	withFixupSeams(t,
		func(context.Context, string) ([]stashEntry, error) { return nil, nil },
		func(context.Context, string) (string, error) { return "base", nil },
	)
	withFixupBaseTree(t, func(context.Context, string, string) (string, error) {
		return "", sentinel
	})
	_, err := strandedFixupWork(context.Background(), ".", "base", nil, "verifiedtree")
	if err == nil || !errors.Is(err, sentinel) {
		t.Fatalf("want the probe-3 error propagated from the call site, got %v", err)
	}
	if !strings.Contains(err.Error(), "verified-tree probe:") {
		t.Errorf("want the caller's verified-tree probe wrap, got %v", err)
	}
}

// --- the removed #2884 reflog probe: residual + coverage (#3023) ----------

// TestStrandedFixupWork_DanglingCommitResidual pins what the tree ACTUALLY
// detects after the inert #2884 reflog provenance probe was removed (#3023),
// against the incident's genuine post-unwind state built BY CONSTRUCTION in a
// real repo — the honest replacement for the deleted
// TestReflogStrandedCommits_DanglingVerifyWipCommit, whose name encoded a
// detection claim the runner never made in production.
//
// The state is established POSITIVELY, not assumed. `git reset --soft` alone
// does NOT reproduce it: it moves HEAD but leaves the index and working tree
// still holding the change, which is a DIFFERENT state from the one the gap
// describes. So the fixture soft-resets (the shape gitResetSoftHEAD1 leaves),
// then brings the repo to the genuine no-changes state and asserts every part
// of it — empty `git status --porcelain`, an empty staged diff, an empty
// unstaged diff, HEAD at the base tip, and no stash entry — before any probe is
// called. It also proves the dangling residue really is present: the commit
// object still exists and is NOT an ancestor of the base tip.
//
// The expected outcomes follow from what verifiedFixupWorkStranded actually
// does, which is read from the code and not assumed: it compares the caller's
// verifiedTreeSHA against `git rev-parse <baseTipSHA>^{tree}`. Both sides come
// from the OBJECT GRAPH — it never consults the working tree, the local HEAD or
// the remote tip. Hence:
//
//	(1) with NO verify witness (verifiedTreeSHA == ""), the probe abstains and
//	    the surviving three probes report NOTHING. This is the residual: no
//	    control detects a dangling commit as such, exactly as the file header
//	    and runner/README.md now state. The removed reflog probe would not have
//	    caught it either — its baseline is snapshotted after the gate already
//	    created and unwound this very commit.
//	(2) with the gate's witness present, probe 3 DOES detect this shape and the
//	    gap is closed. The certified tree differs from the base tip's tree, so
//	    exactly one reason fires naming both trees.
func TestStrandedFixupWork_DanglingCommitResidual(t *testing.T) {
	dir, baseTip := fixuplandRepo(t)
	baseTree := fixuplandGit(t, dir, "rev-parse", baseTip+"^{tree}")

	// The pass: commit the work as the verify gate does, capture the tree it
	// would certify, then unwind exactly as gitResetSoftHEAD1 does.
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("wip work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fixuplandGit(t, dir, "add", "-A")
	fixuplandGit(t, dir, "commit", "-m", "fishhawk verify wip")
	danglingSHA := fixuplandGit(t, dir, "rev-parse", "HEAD")
	verifiedTree := fixuplandGit(t, dir, "rev-parse", "HEAD^{tree}")
	fixuplandGit(t, dir, "reset", "--soft", baseTip)

	// A soft reset alone leaves the change STAGED — assert that is still true
	// here, so the step below is a real state change and not a no-op.
	if staged := fixuplandGit(t, dir, "diff", "--cached", "--name-only"); staged == "" {
		t.Fatal("setup invariant: reset --soft must leave the change staged; the fixture is not exercising the unwind shape")
	}

	// Now establish the GENUINE no-changes state the gap describes, and assert
	// every part of it before calling any probe.
	fixuplandGit(t, dir, "reset", "--hard", baseTip)
	if head := fixuplandGit(t, dir, "rev-parse", "HEAD"); head != baseTip {
		t.Fatalf("setup: HEAD must be the base tip, got %q want %q", head, baseTip)
	}
	if status := fixuplandGit(t, dir, "status", "--porcelain"); status != "" {
		t.Fatalf("setup: working tree must be clean, got %q", status)
	}
	if staged := fixuplandGit(t, dir, "diff", "--cached", "--name-only"); staged != "" {
		t.Fatalf("setup: index must be clean, got %q", staged)
	}
	if unstaged := fixuplandGit(t, dir, "diff", "--name-only"); unstaged != "" {
		t.Fatalf("setup: working tree must have no unstaged change, got %q", unstaged)
	}
	if stash := fixuplandGit(t, dir, "stash", "list"); stash != "" {
		t.Fatalf("setup: no stash entry must exist, got %q", stash)
	}
	// The dangling residue really is present: the object survives and is on no
	// branch (not an ancestor of the base tip).
	if typ := fixuplandGit(t, dir, "cat-file", "-t", danglingSHA); typ != "commit" {
		t.Fatalf("setup: the dangling commit object must still exist, cat-file -t = %q", typ)
	}
	anc := exec.Command("git", "-C", dir, "merge-base", "--is-ancestor", danglingSHA, baseTip)
	if err := anc.Run(); err == nil {
		t.Fatalf("setup: %s must NOT be reachable from the base tip, or it is not dangling", danglingSHA)
	}
	if verifiedTree == baseTree {
		t.Fatalf("setup: the certified tree must differ from the base tree (got both %q)", baseTree)
	}

	// (1) The residual. Real (unseamed) probes against the real repo, with NO
	// verify witness: nothing detects the dangling commit.
	reasons, err := strandedFixupWork(context.Background(), dir, baseTip, nil, "")
	if err != nil {
		t.Fatalf("strandedFixupWork: %v", err)
	}
	if len(reasons) != 0 {
		t.Fatalf("documented residual: with no verify witness NO probe detects a dangling commit, got %v", reasons)
	}

	// (2) The gap is closed when the gate's witness exists: probe 3 fires.
	reasons, err = strandedFixupWork(context.Background(), dir, baseTip, nil, verifiedTree)
	if err != nil {
		t.Fatalf("strandedFixupWork (with witness): %v", err)
	}
	if len(reasons) != 1 {
		t.Fatalf("want exactly one probe-3 reason, got %d: %v", len(reasons), reasons)
	}
	if !strings.Contains(reasons[0], verifiedTree) || !strings.Contains(reasons[0], baseTree) {
		t.Errorf("probe 3 reason must name the certified tree and the base tree, got: %q", reasons[0])
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
