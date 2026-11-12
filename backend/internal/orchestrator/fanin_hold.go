package orchestrator

import (
	"context"
	"fmt"

	"github.com/kuhlman-labs/fishhawk/backend/internal/defaultbranch"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// The closed set of reasons a decomposed parent's acceptance is HELD (#4846).
//
//   - fan_in_verify_unsettled: the FanInVerifyGate answered not settled for the
//     consolidated head (unverified or in flight).
//   - consolidated_pr_not_opened: the parent carries no consolidated PR yet and
//     the open would NOT skip gracefully, so the next Advance opens it. Only the
//     read-only predicate (DecomposedParentFanInHold) reports this cause: Advance
//     opens the PR before deciding, so it never sees an openable, unopened PR.
const (
	FanInHoldCauseUnsettled               = "fan_in_verify_unsettled"
	FanInHoldCauseConsolidatedPRNotOpened = "consolidated_pr_not_opened"
)

// FanInHold is the read-only fan-in verify hold answer for a decomposed
// parent's acceptance stage (#4846). Parent false is the zero value: a child
// run, a run with no implement stage, or a run with no decomposed children, so
// no hold applies. Held true means acceptance must not settle or spawn yet;
// HoldCause names why. Held false on a parent is a release: UnavailableReason
// (one of the FanInVerifyUnavailable* constants) is set when the release
// consulted no fan-in verify (a fail-open arm, #4792), and HeadSHA is the
// consolidated head the hook judged (empty on every fail-open arm).
type FanInHold struct {
	Parent             bool
	Held               bool
	HoldCause          string
	HeadSHA            string
	ConsolidatedBranch string
	UnavailableReason  string
	UnavailableDetail  string
}

// fanInDecision is decideFanInAcceptance's answer: the shared arm order Advance
// (consolidateBeforeAcceptance) and the read-only DecomposedParentFanInHold
// both evaluate, so the two callers cannot disagree about an arm (#4846 R2).
// base is the resolved fan-in base when one was read; baseErr carries the
// unresolved default-branch error for the caller's WARN.
type fanInDecision struct {
	held              bool
	holdCause         string
	base              string
	headSHA           string
	unavailableReason string
	unavailableDetail string
	baseErr           error
}

// fanInGate is consolidateBeforeAcceptance's answer to Advance: whether the run
// is a decomposed parent, whether acceptance is held, and the consolidated head
// the hook judged on a verified release (carried onto the short-circuit
// verdict as head_sha).
type fanInGate struct {
	isParent bool
	held     bool
	headSHA  string
}

// decomposedParentForAcceptance reports whether r is a decomposed parent: it
// has an implement stage (only an implement stage fans out, ADR-025 D4) and at
// least one decomposed child. A run with no implement stage skips the children
// read entirely. A children-read error is returned.
func (o *Orchestrator) decomposedParentForAcceptance(ctx context.Context, r *run.Run, stages []*run.Stage) (bool, error) {
	hasImplement := false
	for _, s := range stages {
		if s.Type == run.StageTypeImplement {
			hasImplement = true
			break
		}
	}
	if !hasImplement {
		return false, nil
	}
	children, err := o.Runs.ListRuns(ctx, run.ListRunsFilter{
		DecomposedFrom: &r.ID,
		Limit:          1,
	})
	if err != nil {
		return false, fmt.Errorf("list decomposed children: %w", err)
	}
	return len(children) > 0, nil
}

// consolidatedPROpenSkips reports whether opening the consolidated PR would
// skip gracefully. It MIRRORS openConsolidatedPRLocked's two graceful-skip
// conditions (a nil GitHub client, a run with no installation id); a change to
// either must change both.
func (o *Orchestrator) consolidatedPROpenSkips(r *run.Run) bool {
	return o.GitHub == nil || r.InstallationID == nil || *r.InstallationID == 0
}

// decideFanInAcceptance evaluates the fan-in verify hold arms for a decomposed
// parent's acceptance stage, in order (#4785, #4792, #4846):
//
//  1. FanInVerify or ConsolidatedReview nil: release, hook_not_wired.
//  2. prOpenPending (the consolidated PR is openable but not opened): hold,
//     consolidated_pr_not_opened, without consulting the hook.
//  3. No consolidated PR (the open skipped gracefully): release,
//     no_consolidated_pr, without consulting the hook.
//  4. The default branch is unresolved: release, default_branch_unresolved,
//     carrying baseErr.
//  5. The hook errs: the error is returned.
//  6. The hook answers not settled: hold, fan_in_verify_unsettled, with the head.
//  7. The hook answers settled: release with its reason and detail, and with its
//     head only when no reason is set.
//
// It is READ-ONLY: it writes no audit row, opens no PR, fires nothing and
// touches no hold counter. The callers own those effects.
func (o *Orchestrator) decideFanInAcceptance(ctx context.Context, r *run.Run, prOpenPending bool) (fanInDecision, error) {
	switch {
	case o.FanInVerify == nil || o.ConsolidatedReview == nil:
		return fanInDecision{unavailableReason: FanInVerifyUnavailableHookNotWired, unavailableDetail: o.fanInHookMissing()}, nil
	case prOpenPending:
		return fanInDecision{held: true, holdCause: FanInHoldCauseConsolidatedPRNotOpened}, nil
	case r.PullRequestURL == nil || *r.PullRequestURL == "":
		return fanInDecision{unavailableReason: FanInVerifyUnavailableNoConsolidatedPR, unavailableDetail: o.noConsolidatedPRDetail(r)}, nil
	}
	// The fan-in base is the repository's resolved default branch (#4660),
	// never a literal. An unresolved read applies NO hold: acceptance dispatches
	// as before (the merge gate still blocks on the unverified head).
	base, berr := o.runBaseBranch(ctx, r)
	if berr != nil {
		reason, _ := defaultbranch.ReasonOf(berr)
		return fanInDecision{
			unavailableReason: FanInVerifyUnavailableDefaultBranchUnresolved,
			unavailableDetail: string(reason),
			baseErr:           berr,
		}, nil
	}
	answer, err := o.FanInVerify.FanInVerifySettled(ctx, r.ID, base, consolidatedBranch(r.ID))
	if err != nil {
		return fanInDecision{base: base}, fmt.Errorf("fan-in verify state: %w", err)
	}
	if !answer.Settled {
		return fanInDecision{held: true, holdCause: FanInHoldCauseUnsettled, base: base, headSHA: answer.HeadSHA}, nil
	}
	d := fanInDecision{base: base, unavailableReason: answer.UnavailableReason, unavailableDetail: answer.UnavailableDetail}
	if answer.UnavailableReason == "" {
		d.headSHA = answer.HeadSHA
	}
	return d, nil
}

// DecomposedParentFanInHold is the exported, READ-ONLY fan-in verify hold
// predicate (#4846) for a top-level decomposed parent's acceptance stage. The
// acceptance-admission walk and the host-dispatch marker consult it so neither
// can settle or spawn a held acceptance stage behind Advance's back. It shares
// decideFanInAcceptance with Advance; the one difference is the PR-less arm:
// Advance opens the consolidated PR before deciding, while this predicate never
// opens it, so an openable but unopened PR holds (consolidated_pr_not_opened)
// and a PR open that would skip gracefully releases (no_consolidated_pr).
//
// A child run, a run with no implement stage, or a run with no decomposed
// children answers the zero value. A children-read or hook error is returned.
// It writes no audit row, opens no PR, fires nothing and touches no hold
// counter; RecordFanInHoldRelease records a fail-open release once the caller
// actually releases.
func (o *Orchestrator) DecomposedParentFanInHold(ctx context.Context, r *run.Run, stages []*run.Stage) (FanInHold, error) {
	if r.DecomposedFrom != nil {
		return FanInHold{}, nil
	}
	isParent, err := o.decomposedParentForAcceptance(ctx, r, stages)
	if err != nil {
		return FanInHold{}, err
	}
	if !isParent {
		return FanInHold{}, nil
	}
	prOpenPending := (r.PullRequestURL == nil || *r.PullRequestURL == "") && !o.consolidatedPROpenSkips(r)
	d, err := o.decideFanInAcceptance(ctx, r, prOpenPending)
	if err != nil {
		return FanInHold{}, err
	}
	return FanInHold{
		Parent:             true,
		Held:               d.held,
		HoldCause:          d.holdCause,
		HeadSHA:            d.headSHA,
		ConsolidatedBranch: consolidatedBranch(r.ID),
		UnavailableReason:  d.unavailableReason,
		UnavailableDetail:  d.unavailableDetail,
	}, nil
}

// RecordFanInHoldRelease records a fail-open release the caller has made
// (#4846): when h is a decomposed parent's release that consulted no fan-in
// verify (an UnavailableReason is set), it appends the fan_in_verify_unavailable
// row with the same payload, per-(stage, reason) dedupe and WARN as Advance.
// Every other answer (not a parent, held, or a verified release) is a no-op.
// Best-effort: an append failure only WARNs (recordFanInVerifyUnavailable).
func (o *Orchestrator) RecordFanInHoldRelease(ctx context.Context, r *run.Run, acceptance *run.Stage, h FanInHold) {
	if !h.Parent || h.Held || h.UnavailableReason == "" {
		return
	}
	o.recordFanInVerifyUnavailable(ctx, r, acceptance, h.UnavailableReason, h.UnavailableDetail)
}
