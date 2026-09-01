package main

import (
	"context"
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
// commit was unwound by gitResetSoftHEAD1, the agent's edits ended up in a
// stash, and the pass reported fixup_no_changes. The PR kept its original
// single commit, yet the re-review certified fixes against work that is not on
// it. The controls that close it: strandedFixupWork refuses fixup_no_changes
// when the pass left work behind — a net-new stash (probe 1), an advanced local
// HEAD (probe 2), or a verify-certified tree that reached neither the working
// tree nor the branch (probe 3, verifiedFixupWorkStranded) — and
// verifyFixupPushLanded refuses fixup_pushed unless the remote branch tip
// reflects the pushed head.
//
// NO control here detects a DANGLING COMMIT as such, and none claims to. The
// #2884 reflog provenance probe that did was removed in #3023 as provably
// inert: its pre-pass snapshot is captured inside openPRAndShipArtifact, AFTER
// runVerifyFixLoop has already created and unwound the gate's throwaway commit,
// so every commit it could have flagged was already in its own baseline.
// Repositioning the snapshot cannot fix that — an earlier snapshot fires on
// every legitimate pass where verify ran, and a `fishhawk verify wip` subject
// filter excludes exactly the commits of interest — because the incident's
// residue is formally identical to the residue a healthy pass leaves. Probe 3's
// certified-tree witness is what covers that shape instead.

// stashEntry is one `git stash list` record: the entry's commit SHA and its
// subject line.
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
	fixupRemoteBranchTip = gitops.RemoteBranchTipURL
	// fixupBaseTreeOf resolves the fix-up base tip to its tree object hash, the
	// witness probe 3 (verifiedFixupWorkStranded) compares the verify gate's
	// certified tree against. Seamed for the same reason as the others: the
	// fake-pusher run() tests default repoDir to "." (the runner's own source
	// repo), so no probe may touch a real repo unseamed. gitRevParseTreeOf
	// already exists in main.go (~5516); this is only an alias, not a second
	// implementation.
	fixupBaseTreeOf = gitRevParseTreeOf
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

// verifiedFixupWorkStranded is probe 3 (#3022): the "did this pass's work reach
// the branch" detector. It is the instrument for the incident's OWN shape — a
// fix-up that commits its work, resets back to the base tip, leaves NO stash and
// reports NO changes — which probes 1 and 2 both miss (no net-new stash is made,
// and HEAD is back at the base tip). The #2884 reflog provenance probe that
// claimed to cover it missed it too, and was removed in #3023 (see the file
// header).
//
// The instrument is deliberately NOT commit creation and NOT the reflog. It is
// the runner's OWN verify witness: the committed-tree verify gate
// (runVerifyFixLoop / runVerifyGateCommitted) records verifiedTreeSHA — the tree
// object hash of the scope-only throwaway commit it certified. If that certified
// tree differs from the tree of the fix-up base tip yet CommitAndPush reports
// NoChanges, the pass verifiably HAD in-scope work at verify time that is now on
// neither the working tree nor the branch: its work did not reach the branch.
//
// It cannot false-fire on the verify gate's normal `fishhawk verify wip`
// residue, because a HEALTHY pass never reaches the no-changes branch at all:
// after gitResetSoftHEAD1 the gate leaves its edits staged in the working tree,
// so CommitAndPush commits and pushes them and NoChanges is never returned. And
// a genuinely-no-work pass carries an EMPTY verifiedTreeSHA (commitVerifyWIP
// makes no commit when nothing is staged), which this probe treats as no witness
// and never flags.
//
// Semantics, in order:
//
//	(a) verifiedTreeSHA == "" -> ("", nil): no verify witness for this pass
//	    (verify unconfigured, skipped, budget exhausted, or nothing staged) — the
//	    probe abstains rather than guessing.
//	(b) baseTipSHA == "" -> ("", nil): no base to compare against; the
//	    fixup_base_tip_unresolved log already records that degrade.
//	(c) a base-tree resolve failure -> ("", err): fail CLOSED (category C at the
//	    caller) rather than fall through to a clean verdict.
//	(d) verifiedTreeSHA == baseTree -> ("", nil): the certified tree is
//	    byte-identical to the branch tip, so the pass produced no in-scope work.
//	(e) otherwise -> a reason naming both trees and the base tip.
func verifiedFixupWorkStranded(ctx context.Context, repoDir, baseTipSHA, verifiedTreeSHA string) (reason string, err error) {
	if verifiedTreeSHA == "" {
		return "", nil
	}
	if baseTipSHA == "" {
		return "", nil
	}
	baseTree, terr := fixupBaseTreeOf(ctx, repoDir, baseTipSHA)
	if terr != nil {
		return "", fmt.Errorf("resolving base tip tree of %s: %w", baseTipSHA, terr)
	}
	if verifiedTreeSHA == baseTree {
		return "", nil
	}
	return fmt.Sprintf("verified work did not reach the branch: the committed-tree verify gate certified tree %s but the pass reports no changes and the base tip %s still carries tree %s — the pass's in-scope work is on neither the working tree nor the branch",
		verifiedTreeSHA, baseTipSHA, baseTree), nil
}

// strandedFixupWork returns one human-readable reason per detected stranding of
// a fix-up pass that is about to report fixup_no_changes (#2884, #3022). It runs
// three probes, all fail-closed (a probe error returns a non-nil err so the
// caller fails category-C rather than falling through to a success report):
//
//  1. a net-new stash entry — present now, absent from the pre-pass snapshot.
//     A stash is never a valid terminal fix-up artifact; the pre-pass snapshot
//     comparison keeps a pre-existing operator stash from being flagged.
//  2. a local HEAD that advanced past the base tip in unpushed commits.
//  3. verified work that did not reach the branch (verifiedFixupWorkStranded):
//     the verify gate certified a non-base tree yet the pass reports no changes.
//
// Probe 3 is the instrument for the commit-reset-no-stash-no-changes shape
// (#3022). A fourth probe — the #2884 reflog provenance walk — sat between
// probes 2 and 3 until #3023 removed it as provably inert: strandedFixupWork
// runs ONLY on the CommitAndPush NoChanges branch, and CommitAndPush documents
// and implements NoChanges as a short-circuit "with no other side effects", so
// the window between that probe's snapshot and its check contained no
// commit-creating operation at all; the verify gate's own throwaway commit is
// created and unwound BEFORE the snapshot is taken, so it landed in the
// baseline and was skipped. No reflog instrument can separate stranded fix-up
// work from ordinary verify-gate residue at ANY snapshot placement, because the
// two are formally identical. Probe 3 anchors to the gate's certified-tree
// witness instead, which no snapshot ordering can blind.
//
// An empty reasons slice with a nil err means the pass is genuinely clean and
// fixup_no_changes may proceed.
func strandedFixupWork(ctx context.Context, repoDir, baseTipSHA string, preStash []stashEntry, verifiedTreeSHA string) (reasons []string, err error) {
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

	verifiedReason, verr := verifiedFixupWorkStranded(ctx, repoDir, baseTipSHA, verifiedTreeSHA)
	if verr != nil {
		return nil, fmt.Errorf("verified-tree probe: %w", verr)
	}
	if verifiedReason != "" {
		reasons = append(reasons, verifiedReason)
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
