package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"

	"github.com/kuhlman-labs/fishhawk/runner/internal/gitops"
)

// Standalone implement base advance (E68.67 / #3454).
//
// A STANDALONE implement stage ran its agent AND its committed-tree verify
// gates against the lineage worktree's PLAN-TIME base. provisionLineage-
// Worktree seeds the worktree from the operator checkout's HEAD at the PLAN
// stage and then takes the `lineage_worktree_reused` path for implement
// WITHOUT moving HEAD, and the only pre-agent base checkout in run() was
// gated on cfg.decomposedFromRunID != "" (the #1302/#1363 wave-base block).
// Only the COMMIT-time gitops FreshFetchBase re-staged the finished commit
// onto the moved base — far too late: the agent reasoned against the stale
// tree, and runVerifyGateCommitted's throwaway scope-only commit was cut on
// top of that stale HEAD, so the gate ran the PLAN-TIME `scripts/test`. That
// is the run 1bc985d1 / #3390 and run a90d98ee / #3451 loss: the verify-lock
// self-deadlock #3451 had already fixed was re-discovered by a gate running
// pre-fix infra, burning ~55 minutes and a fix-up pass.
//
// This file is the pre-agent, standalone-only fix: fetch the declared base
// (the SAME ref resolveImplementBranchRouting hands CommitAndPush as
// freshFetchBase), fast-forward the lineage worktree's detached HEAD to that
// tip, and log `lineage_worktree_advanced {from,to}` — so the agent view, the
// gate tree and the commit base become ONE base.
//
// It REFUSES loud, pre-agent, mutating nothing, when the worktree is dirty or
// its HEAD is not an ancestor-equal of the base tip (it carries run commits,
// or diverged) rather than force-discarding work, and it reuses the existing
// #1302/#1363 not-wired-vs-transient degrade ladder verbatim so a bare local
// checkout with no origin still runs exactly as it does today.
//
// The #1866 seed-ancestry guard semantics are UNTOUCHED: the advance only ever
// moves HEAD FORWARD along the declared base (guard (g) below is what makes
// that structural), so a worktree that was legitimately seeded from an
// ancestor of the base stays legitimately seeded from one.

// standaloneBaseAdvanceEligible reports whether this dispatch is the
// STANDALONE implement shape the base advance applies to.
//
// Both exclusions are STRUCTURAL, not incidental:
//
//   - cfg.fixup: a fix-up pass MUST stay on the PR branch its
//     checkoutFixupBase established. Advancing it would discard the recorded
//     head the ADR-035 lineage comparison asserts against (checkoutFixupBase
//     verifies the fetched tip against cfg.fixupExpectedHeadSHA and fails fast
//     on a mismatch), so an advance here would either destroy the fix-up's own
//     base or make that comparison meaningless. A fix-up pass is never advanced.
//   - cfg.decomposedFromRunID: a decomposed child already establishes its OWN
//     wave base in the #1302/#1363 block immediately below this one in run().
//     The two predicates are mutually exclusive BY CONSTRUCTION — that block
//     requires decomposedFromRunID != "", this one requires it empty — so the
//     new block and the existing one can never both fire on one dispatch.
//
// Every non-implement stage type is excluded because there is no agent-visible
// base to advance: a plan/review/acceptance stage does not cut a commit onto
// the declared base.
func standaloneBaseAdvanceEligible(stageType string, cfg config) bool {
	return stageType == "implement" && !cfg.fixup && cfg.decomposedFromRunID == ""
}

// lineageBaseAdvanceRefusal is the typed PRE-AGENT refusal the base advance
// returns when it cannot PROVE the worktree is safe to fast-forward: a dirty
// tree, an unreadable dirty set, a HEAD that provably is not an ancestor-equal
// of the base tip, or an ancestry probe that itself failed. Every one of those
// is fail-CLOSED on purpose — refusing here costs zero agent tokens and mutates
// nothing, while a silent force-checkout would destroy uncommitted or
// uncommitted-and-unpushed work.
type lineageBaseAdvanceRefusal struct {
	// kind is the machine-readable refusal mode: "dirty_worktree",
	// "dirty_probe_failed", "head_not_ancestor" or "ancestry_probe_failed".
	kind string
	// repoDir is the lineage worktree the refusal is about.
	repoDir string
	// baseRef is the declared base the advance targeted.
	baseRef string
	// fromSHA / toSHA are the resolved worktree HEAD and the fetched base tip.
	fromSHA string
	toSHA   string
	// detail carries the mode-specific evidence (the dirty path list, the
	// underlying probe error).
	detail string
}

func (e *lineageBaseAdvanceRefusal) Error() string {
	return fmt.Sprintf(
		"lineage worktree base advance refused (%s): worktree %s is at %s and the declared base %s is at %s; %s. "+
			"The advance only ever fast-forwards along the declared base, so it will not discard this state. "+
			"Recover by committing or discarding the worktree's own changes, or remove the lineage worktree "+
			"(`git worktree remove %s`) and re-dispatch so it is re-seeded from the current base.",
		e.kind, e.repoDir, e.fromSHA, e.baseRef, e.toSHA, e.detail, e.repoDir)
}

// lineageBaseAdvanceFailureReason maps an advanceLineageWorktreeToBase error to
// the runner_failed reason token: "lineage_worktree_advance_refused" for the
// typed pre-agent refusal, else the generic "lineage_base_advance". Factored
// out for the same reason worktreeProvisionFailureReason and
// fixupCheckoutFailReason are — the main.go call site stays a two-line change
// and the reason mapping is unit-testable.
func lineageBaseAdvanceFailureReason(err error) string {
	var r *lineageBaseAdvanceRefusal
	if errors.As(err, &r) {
		return "lineage_worktree_advance_refused"
	}
	return "lineage_base_advance"
}

// advanceLineageWorktreeToBase fast-forwards the lineage worktree's detached
// HEAD to the freshly-fetched tip of the declared base, BEFORE the agent is
// invoked and before every verify gate. It returns (true, nil) when it moved
// HEAD, (false, nil) on every graceful degrade / already-current fast path, and
// a non-nil error when it fails loud.
//
// The ORDER is load-bearing — every guard runs before anything mutates:
//
//	(a) empty baseRef            → no-op (nothing declared to advance to)
//	(b) remoteHasBranch          → absence / not-wired degrade, or fail loud
//	(c) resolveHead              → the `from` SHA
//	(d) fetchDiffBaseTip         → the `to` SHA, WITHOUT touching the tree
//	(e) from == to               → lineage_worktree_base_current fast path
//	(f) dirtyPaths               → refuse on a dirty tree or an unreadable one
//	(g) ancestryProbe            → refuse unless from is ancestor-equal of to
//	(h) checkoutChildBase        → the NON-force detached checkout
//
// (d) is gitops.FetchBaseTip — the same explicit-refspec fetch machinery
// checkoutChildBase runs, MINUS the checkout — so the tip is known before (f)
// and (g) decide, and a refusal leaves the worktree byte-identical.
//
// (h) is gitops.CheckoutRemoteBranchDetached, a NON-force `checkout --detach
// <tip>`. Read its protection HONESTLY: git aborts only when the checkout would
// need to OVERWRITE a local modification, i.e. when the dirty path differs
// between the two commits. A dirty path UNCHANGED across from..to is carried
// forward silently, and an untracked non-conflicting file never blocks a
// checkout at all — both were observed empirically (the guard-(f) deletion
// counterfactual advanced HEAD in BOTH dirty shapes). So (h) is a narrow
// backstop, NOT a general second layer: guard (f) is the only thing standing
// between a dirty worktree and a silent advance in the common case.
//
// It reuses the EXISTING package-level seams (remoteHasBranch, remoteConfigured,
// fetchDiffBaseTip, checkoutChildBase, dirtyPaths, ancestryProbe) rather than
// minting parallel ones, so withFakeGitOps already stubs all of them — with
// remoteHasBranch defaulting to (false, nil), every pre-existing run() test
// takes the base_ref_absent skip at (b) and its behaviour is unchanged.
func advanceLineageWorktreeToBase(ctx context.Context, repoDir, baseRef, authToken string, logSink io.Writer) (bool, error) {
	if repoDir == "" {
		repoDir = "."
	}
	// (a) Nothing declared to advance to.
	if baseRef == "" {
		return false, nil
	}

	// (b) Remote-authoritative base existence (#1363's ls-remote seam). A
	// genuine ABSENCE (query succeeded, empty output) is the #1302 degrade
	// contract — a never-pushed base — and skips. A query FAILURE splits on
	// remoteConfigured exactly as the wave-base block does: against a
	// CONFIGURED remote, silently running on a base of unknown staleness IS the
	// defect this closes, so fail loud (the refusal is pre-agent, so it burns
	// zero tokens and re-dispatch is the recovery); against an UNCONFIGURED
	// remote it is the GitHub-not-wired state (a bare local checkout with no
	// origin) and skips like an absence, which is what keeps a bare local
	// checkout running exactly as it does today.
	baseExists, rhErr := remoteHasBranch(ctx, repoDir, gitops.DefaultRemote, baseRef, authToken)
	if rhErr != nil {
		if remoteConfigured(ctx, repoDir, gitops.DefaultRemote) {
			return false, fmt.Errorf("lineage base advance: query base %q on %s: %w",
				baseRef, gitops.DefaultRemote, rhErr)
		}
		_, _ = fmt.Fprintf(logSink,
			`{"event":"lineage_worktree_advance_skipped","reason":"remote_unconfigured","base_ref":%q}`+"\n", baseRef)
		return false, nil
	}
	if !baseExists {
		_, _ = fmt.Fprintf(logSink,
			`{"event":"lineage_worktree_advance_skipped","reason":"base_ref_absent","base_ref":%q}`+"\n", baseRef)
		return false, nil
	}

	// (c) The worktree's current HEAD — the `from` SHA, pinned ONCE so the
	// ancestry probe and the advanced record name the same immutable commit.
	fromSHA, err := resolveHead(ctx, repoDir)
	if err != nil {
		return false, fmt.Errorf("lineage base advance: resolve worktree HEAD: %w", err)
	}

	// (d) The base's live tip, fetched into the tracking ref WITHOUT touching
	// the working tree, so (f) and (g) decide before anything moves.
	toSHA, err := fetchDiffBaseTip(ctx, repoDir, gitops.DefaultRemote, baseRef, authToken)
	if err != nil {
		return false, fmt.Errorf("lineage base advance: fetch base %q tip: %w", baseRef, err)
	}

	// (e) Already current: the no-advance-needed fast path that keeps every
	// already-current dispatch byte-identical to today REGARDLESS of tree state
	// — the dirty and ancestry guards below are reached ONLY when the base
	// actually moved, so a worktree an operator legitimately left dirty draws no
	// refusal on a base that did not move.
	if fromSHA == toSHA {
		_, _ = fmt.Fprintf(logSink,
			`{"event":"lineage_worktree_base_current","base_ref":%q,"head_sha":%q}`+"\n", baseRef, fromSHA)
		return false, nil
	}

	// (f) Dirty-tree guard. A non-empty dirty set OR a probe ERROR refuses: we
	// cannot PROVE the tree is safe to move, and refusing pre-agent costs
	// nothing while a silent force would destroy work.
	dirty, dErr := dirtyPaths(ctx, repoDir)
	if dErr != nil {
		return false, &lineageBaseAdvanceRefusal{
			kind: "dirty_probe_failed", repoDir: repoDir, baseRef: baseRef,
			fromSHA: fromSHA, toSHA: toSHA,
			detail: "the worktree's dirty set could not be read, so the tree cannot be proven safe to fast-forward: " + dErr.Error(),
		}
	}
	if len(dirty) > 0 {
		return false, &lineageBaseAdvanceRefusal{
			kind: "dirty_worktree", repoDir: repoDir, baseRef: baseRef,
			fromSHA: fromSHA, toSHA: toSHA,
			detail: "the worktree carries uncommitted changes (" + strings.Join(dirty, ", ") + ")",
		}
	}

	// (g) Ancestry guard — this is what makes the advance a FAST-FORWARD ONLY
	// operation and so preserves the #1866 seed-ancestry semantics. An
	// *exec.ExitError with code 1 means fromSHA provably is NOT an
	// ancestor-equal of the base tip (the worktree carries run commits, or
	// diverged). Any OTHER probe error refuses too, on the same fail-closed
	// reasoning: an UNPROVABLE ancestry is not a licence to move HEAD. (This
	// deliberately differs from verifySeedAncestry, which degrades to a logged
	// skip on a probe error — there a skip falls through to today's behaviour,
	// whereas here a skip would fall through to MUTATING the tree.)
	probeErr := ancestryProbe(ctx, repoDir, fromSHA, toSHA)
	if probeErr != nil {
		var ee *exec.ExitError
		if errors.As(probeErr, &ee) && ee.ExitCode() == 1 {
			return false, &lineageBaseAdvanceRefusal{
				kind: "head_not_ancestor", repoDir: repoDir, baseRef: baseRef,
				fromSHA: fromSHA, toSHA: toSHA,
				detail: "the worktree HEAD is provably NOT an ancestor of the base tip — it carries its own commits, or diverged, and a fast-forward would discard them",
			}
		}
		return false, &lineageBaseAdvanceRefusal{
			kind: "ancestry_probe_failed", repoDir: repoDir, baseRef: baseRef,
			fromSHA: fromSHA, toSHA: toSHA,
			detail: "the ancestry probe itself failed, so the fast-forward cannot be proven safe: " + probeErr.Error(),
		}
	}

	// (h) The NON-force detached checkout. git aborts only where the checkout
	// would OVERWRITE a local modification (a dirty path that DIFFERS between
	// from and to), so this is a narrow backstop and not a general second layer
	// under (f) — see the doc comment. Guard (f) carries the dirty case.
	tipSHA, coErr := checkoutChildBase(ctx, repoDir, gitops.DefaultRemote, baseRef, authToken)
	if coErr != nil {
		return false, fmt.Errorf("lineage base advance: check out base %q tip: %w", baseRef, coErr)
	}
	_, _ = fmt.Fprintf(logSink,
		`{"event":"lineage_worktree_advanced","base_ref":%q,"from":%q,"to":%q}`+"\n", baseRef, fromSHA, tipSHA)
	return true, nil
}
