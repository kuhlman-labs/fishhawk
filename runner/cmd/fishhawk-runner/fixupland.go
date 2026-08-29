package main

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/kuhlman-labs/fishhawk/runner/internal/gitops"
)

// This file holds the #2884 fix-up landing controls: the checks that prove a
// fix-up pass's work actually reached the PR branch before the stage reports
// success. It is kept out of the ~9.6k-line main.go so the reviewable diff for
// the behavior change is proportional to it; main.go carries only the wiring.
//
// The incident (run 8ae65577, PR #2883): a fix-up pass did its work, the runner
// verify gate committed it as a throwaway `fishhawk verify wip` commit, that
// commit was unwound by gitResetSoftHEAD1 (leaving it DANGLING — reachable from
// neither the base tip nor the branch), the agent's edits ended up in a stash,
// and the pass reported fixup_no_changes. The PR kept its original single
// commit, yet the re-review certified fixes against the orphan sha. Two
// controls close it: strandedFixupWork refuses fixup_no_changes when the pass
// left work behind, and verifyFixupPushLanded refuses fixup_pushed unless the
// remote branch tip reflects the pushed head.

// stashEntry is one `git stash list` or `git reflog` record: the entry's commit
// SHA and its subject line.
type stashEntry struct {
	SHA     string
	Subject string
}

// Package-level seams so the fake-pusher run() tests — which default repoDir to
// "." (the runner's own source repo) — never probe the runner's own repo or a
// live remote. Production wires each to the real helper below; tests swap in
// canned fakes (withFakeFixup* in main_test.go), the same pattern and rationale
// as checkoutFixupBase / fetchDiffBaseTip (main.go ~5820).
var (
	fixupStashList       = gitStashList
	fixupLocalHead       = gitRevParseHEAD
	fixupStrandedReflog  = reflogStrandedCommits
	fixupReflogCommits   = gitReflogCommits
	fixupRemoteBranchTip = gitops.RemoteBranchTipURL
)

// gitStashList returns the stash stack as {SHA, Subject} entries, newest first,
// via `git stash list --format=%H %gs`. An absent stash ref prints nothing and
// exits 0 (the stash ref is created lazily on first push), so an empty stash is
// an empty slice and a nil error — never an error.
func gitStashList(ctx context.Context, repoDir string) ([]stashEntry, error) {
	out, err := exec.CommandContext(ctx, "git", "-C", repoDir, "stash", "list", "--format=%H %gs").Output()
	if err != nil {
		return nil, fmt.Errorf("gitops: stash list: %w", err)
	}
	return parseShaSubjectLines(string(out)), nil
}

// gitReflogCommits returns the HEAD reflog as {SHA, Subject} entries via
// `git reflog --format=%H %gs`, newest first. It is the pre-pass snapshot and
// the walk source for the dangling-commit provenance probe. The run worktree is
// provisioned fresh per run, so the reflog is short and its noise bounded. A
// repo with no HEAD reflog prints nothing and exits 0 → an empty slice, nil.
func gitReflogCommits(ctx context.Context, repoDir string) ([]stashEntry, error) {
	out, err := exec.CommandContext(ctx, "git", "-C", repoDir, "reflog", "--format=%H %gs").Output()
	if err != nil {
		return nil, fmt.Errorf("gitops: reflog: %w", err)
	}
	return parseShaSubjectLines(string(out)), nil
}

// parseShaSubjectLines parses `<sha> <subject>` lines (git's `%H %gs` format)
// into entries, skipping blanks. The subject may be empty (an entry with no
// reflog message), which is fine.
func parseShaSubjectLines(out string) []stashEntry {
	var entries []stashEntry
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		sha, subject, _ := strings.Cut(line, " ")
		entries = append(entries, stashEntry{SHA: sha, Subject: subject})
	}
	return entries
}

// gitIsAncestor reports whether maybeAncestor is an ancestor of descendant via
// `git merge-base --is-ancestor` (exit 0 = ancestor, exit 1 = definitively not,
// any other exit = a real error the caller must surface).
func gitIsAncestor(ctx context.Context, repoDir, maybeAncestor, descendant string) (bool, error) {
	err := exec.CommandContext(ctx, "git", "-C", repoDir,
		"merge-base", "--is-ancestor", maybeAncestor, descendant).Run()
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, nil
	}
	return false, fmt.Errorf("gitops: merge-base --is-ancestor %s %s: %w", maybeAncestor, descendant, err)
}

// reflogStrandedCommits is the #2884 dangling-commit provenance probe (approval
// condition 1). It walks the HEAD reflog and reports every commit that was
// created DURING the pass (its SHA is not in the pre-pass snapshot) yet is NOT
// reachable from the base tip — the dangling residue a `fishhawk verify wip`
// commit leaves after gitResetSoftHEAD1 returns HEAD to the base tip. Probes 1
// (net-new stash) and 2 (advanced HEAD) both MISS this shape: no stash is made
// and HEAD is back at the base tip, so only a provenance walk surfaces it.
//
// The base tip and every commit reachable from it are ordinary history and are
// never flagged; a commit that diverges from the base tip (the base tip is its
// ancestor, not vice versa) is stranded. Results are deduplicated by SHA. A
// merge-base error on any candidate is surfaced (fail closed), not swallowed.
func reflogStrandedCommits(ctx context.Context, repoDir, baseTipSHA string, preReflog []stashEntry) ([]stashEntry, error) {
	current, err := fixupReflogCommits(ctx, repoDir)
	if err != nil {
		return nil, err
	}
	pre := make(map[string]bool, len(preReflog))
	for _, e := range preReflog {
		pre[e.SHA] = true
	}
	seen := make(map[string]bool)
	var stranded []stashEntry
	for _, e := range current {
		if e.SHA == "" || e.SHA == baseTipSHA || pre[e.SHA] || seen[e.SHA] {
			continue
		}
		anc, aerr := gitIsAncestor(ctx, repoDir, e.SHA, baseTipSHA)
		if aerr != nil {
			return nil, aerr
		}
		if anc {
			// Reachable from the base tip: ordinary history, not stranded.
			continue
		}
		seen[e.SHA] = true
		stranded = append(stranded, e)
	}
	return stranded, nil
}

// strandedFixupWork returns one human-readable reason per detected stranding of
// a fix-up pass that is about to report fixup_no_changes (#2884). It runs three
// probes, all fail-closed (a probe error returns a non-nil err so the caller
// fails category-C rather than falling through to a success report):
//
//  1. a net-new stash entry — present now, absent from the pre-pass snapshot.
//     A stash is never a valid terminal fix-up artifact; the pre-pass snapshot
//     comparison keeps a pre-existing operator stash from being flagged.
//  2. a local HEAD that advanced past the base tip in unpushed commits.
//  3. a dangling commit created during the pass, reachable from neither the
//     base tip nor the branch (reflogStrandedCommits) — the incident shape that
//     probes 1 and 2 miss.
//
// An empty reasons slice with a nil err means the pass is genuinely clean and
// fixup_no_changes may proceed.
func strandedFixupWork(ctx context.Context, repoDir, baseTipSHA string, preStash, preReflog []stashEntry) (reasons []string, err error) {
	currentStash, serr := fixupStashList(ctx, repoDir)
	if serr != nil {
		return nil, fmt.Errorf("stash probe: %w", serr)
	}
	preStashSet := make(map[string]bool, len(preStash))
	for _, e := range preStash {
		preStashSet[e.SHA] = true
	}
	for _, e := range currentStash {
		if !preStashSet[e.SHA] {
			reasons = append(reasons, fmt.Sprintf("net-new stash entry %s (%s) — a stash is never a valid terminal fix-up artifact", e.SHA, e.Subject))
		}
	}

	head, herr := fixupLocalHead(ctx, repoDir)
	if herr != nil {
		return nil, fmt.Errorf("local head probe: %w", herr)
	}
	if baseTipSHA != "" && head != "" && head != baseTipSHA {
		reasons = append(reasons, fmt.Sprintf("local HEAD advanced past the base tip in unpushed commits: expected=%s actual=%s", baseTipSHA, head))
	}

	dangling, derr := fixupStrandedReflog(ctx, repoDir, baseTipSHA, preReflog)
	if derr != nil {
		return nil, fmt.Errorf("reflog provenance probe: %w", derr)
	}
	for _, e := range dangling {
		reasons = append(reasons, fmt.Sprintf("dangling commit %s (%s) created during the pass, on no branch and not reachable from the base tip", e.SHA, e.Subject))
	}

	return reasons, nil
}

// verifyFixupPushLanded proves a fix-up pass's push actually reached the branch
// before the stage reports fixup_pushed (#2884). It reads the remote branch tip
// and returns:
//
//   - nil when the tip EXACTLY equals the pushed head (the push landed);
//   - ErrFixupPushNotLanded (category B) when the tip differs — naming the
//     expected pushed head and the actual tip (or "absent" for a missing
//     branch), the incident's "push did not land" signature;
//   - an ErrVerifyInfraFailure wrap (category C) when the remote tip cannot be
//     read, so a transient ls-remote fault is retried in place, never mistaken
//     for a moved branch.
//
// Semantics decision (approval condition 3): this is EXACT equality, not
// descendant tolerance. A concurrent push landing on top of ours between our
// push and this re-read — a vouch commit, a bot formatter, another runner —
// therefore produces a category-B false positive. The trade is deliberate: a
// descendant check needs a network fetch of the remote commit plus its own
// failure surface on the success path, while the race window here is
// milliseconds and its failure mode is SAFE (category B → the operator retries,
// no bad merge ships). The residual is documented in runner/README.md.
func verifyFixupPushLanded(ctx context.Context, repoDir, remoteURL, branch, pushToken, pushedHeadSHA string) error {
	tip, err := fixupRemoteBranchTip(ctx, repoDir, remoteURL, branch, pushToken)
	if err != nil {
		return fmt.Errorf("%w: reading remote tip of %s: %v", gitops.ErrVerifyInfraFailure, branch, err)
	}
	if tip == pushedHeadSHA {
		return nil
	}
	actual := tip
	if actual == "" {
		actual = "absent"
	}
	return fmt.Errorf("%w: expected=%s actual=%s branch=%s", gitops.ErrFixupPushNotLanded, pushedHeadSHA, actual, branch)
}
