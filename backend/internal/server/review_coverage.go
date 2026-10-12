package server

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// mergeCodeReviewCoverageIncomplete is the merge-readiness refusal code for a
// decomposed parent with a slice no SUCCESSFUL agent review covers (#4219).
// It is also the drive rule name (drive.RuleReviewCoverageIncomplete), the
// derived_status and the MCP next_actions state, byte-for-byte.
const mergeCodeReviewCoverageIncomplete = "review_coverage_incomplete"

// The review_coverage_incomplete refusal's next_step values. The rerun verb is
// fishhawk_rerun_review (POST /v0/runs/{run_id}/reviews/rerun).
const (
	reviewCoverageNextRerun = "fishhawk_rerun_review"
	reviewCoverageNextAwait = "fishhawk_await_review"
)

// Reasons an uncovered slice carries in uncovered_slices[].reason.
const (
	reviewCoverageReasonNoRound   = "no_review_round"
	reviewCoverageReasonInFlight  = "review_round_in_flight"
	reviewCoverageReasonNoVerdict = "review_round_no_verdict"
)

// implementRoundCoverage is one run's LATEST implement-review round, reduced
// to what coverage needs. Rounds are delimited by implement_review_started
// entries exactly as implementReviewRound delimits them: every terminal entry
// (implement_reviewed / _failed / _skipped) with a sequence above the newest
// started entry belongs to that round.
type implementRoundCoverage struct {
	Started    bool
	Sequence   int64
	Configured int
	Terminal   int
	// Verdicts counts the round's implement_reviewed entries — an agent that
	// returned a verdict, approve or reject. Failed and skipped entries are
	// terminal but cover nothing.
	Verdicts int
}

// inFlight reports a dispatched round not every configured reviewer has
// settled yet.
func (c implementRoundCoverage) inFlight() bool {
	return c.Started && !planreview.Settled(c.Configured, c.Terminal)
}

// latestImplementRoundCoverage reads runRow's latest implement-review round.
// A run with no implement_review_started entry yields the zero value
// (Started=false). Every audit read error is returned, never resolved to
// "covered".
func (s *Server) latestImplementRoundCoverage(ctx context.Context, runRow *run.Run) (implementRoundCoverage, error) {
	started, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runRow.ID, "implement_review_started")
	if err != nil {
		return implementRoundCoverage{}, fmt.Errorf("list implement_review_started for run %s: %w", runRow.ID, err)
	}
	var latest *audit.Entry
	for _, e := range started {
		if latest == nil || e.Sequence > latest.Sequence {
			latest = e
		}
	}
	if latest == nil {
		return implementRoundCoverage{}, nil
	}
	c := implementRoundCoverage{Started: true, Sequence: latest.Sequence}
	var payload planreview.ReviewStartedPayload
	if json.Unmarshal(latest.Payload, &payload) == nil {
		c.Configured = payload.ConfiguredAgents
	}
	if c.Configured == 0 {
		// Pre-#600 or malformed payload: fall back to the spec, like
		// implementReviewRound.
		if cfg := s.resolveStageReviewers(ctx, runRow, spec.StageTypeImplement); cfg != nil {
			c.Configured = cfg.AgentCount()
		}
	}
	for _, cat := range []string{"implement_reviewed", "implement_review_failed", "implement_review_skipped"} {
		entries, lerr := s.cfg.AuditRepo.ListForRunByCategory(ctx, runRow.ID, cat)
		if lerr != nil {
			return implementRoundCoverage{}, fmt.Errorf("list %s for run %s: %w", cat, runRow.ID, lerr)
		}
		for _, e := range entries {
			if e.Sequence <= latest.Sequence {
				continue
			}
			c.Terminal++
			if cat == "implement_reviewed" {
				c.Verdicts++
			}
		}
	}
	return c, nil
}

// reviewCoverageRefusal is the ONE predicate deciding whether every slice of a
// decomposed parent is covered by a SUCCESSFUL agent review (#4219). It holds
// unless EITHER the parent's latest implement-review round carries at least one
// implement_reviewed verdict (a consolidated or fix-up round compares the whole
// parent diff, so its verdict covers every slice) OR every SUCCEEDED slice
// child carries one in its own latest round. A round whose reviewers all failed
// or were skipped covers nothing, however settled it is — that is the d3b1d915
// hole: an infra-failed consolidated round "settled" and the parent went
// merge-eligible with a slice no agent ever reviewed.
//
// Latest-round semantics on both sides: an older verdict superseded by a newer
// round with no verdict does not count. A reject verdict counts as coverage —
// the review happened; what to do with a reject is the gate's business.
//
// The slice set is the parent's SUCCEEDED decomposed children, mirroring what
// orchestrator.IntegrateSlices merges (latestSlicesIntegrated /
// handleIntegrateWave): a cancelled or failed child contributed no code.
//
// nil (not applicable) when: the audit or run repository is unwired; the run is
// itself a child; the run has no decomposed children; the implement stage
// configures zero agent reviewers; or no reviewer backend is wired (the
// *_review_skipped degradation every ordinary run already merges through).
// FAIL-CLOSED: every read error is returned; callers refuse (merge endpoint),
// record a history gap (gate view) or skip the tick (drive observer).
//
// Consulted by handleMergeRun, gateViewMergeReadinessFor,
// ObserveParkedReviewForDrive and the delegated may_merge arm, so the four
// cannot drift.
func (s *Server) reviewCoverageRefusal(ctx context.Context, runRow *run.Run) (*mergeReadinessRefusal, error) {
	if s.cfg.AuditRepo == nil || s.cfg.RunRepo == nil || runRow == nil {
		return nil, nil
	}
	if runRow.DecomposedFrom != nil {
		return nil, nil
	}
	if s.defaultPlanReviewer() == nil {
		return nil, nil
	}
	cfg := s.resolveStageReviewers(ctx, runRow, spec.StageTypeImplement)
	if cfg == nil || cfg.AgentCount() == 0 {
		return nil, nil
	}
	children, err := s.listAllDecomposedChildren(ctx, runRow.ID)
	if err != nil {
		return nil, fmt.Errorf("list decomposed children: %w", err)
	}
	if len(children) == 0 {
		return nil, nil
	}
	parent, err := s.latestImplementRoundCoverage(ctx, runRow)
	if err != nil {
		return nil, err
	}
	if parent.Verdicts > 0 {
		return nil, nil
	}

	sort.SliceStable(children, func(i, j int) bool {
		return sliceOrderKey(children[i]) < sliceOrderKey(children[j])
	})
	var uncovered []map[string]any
	var names []string
	for _, ch := range children {
		if ch.State != run.StateSucceeded {
			continue
		}
		cov, cerr := s.latestImplementRoundCoverage(ctx, ch)
		if cerr != nil {
			return nil, cerr
		}
		if cov.Verdicts > 0 {
			continue
		}
		reason := reviewCoverageReasonNoVerdict
		switch {
		case !cov.Started:
			reason = reviewCoverageReasonNoRound
		case cov.inFlight():
			reason = reviewCoverageReasonInFlight
		}
		entry := map[string]any{
			"child_run_id":   ch.ID.String(),
			"round_sequence": cov.Sequence,
			"reason":         reason,
		}
		name := "child " + ch.ID.String()
		if ch.SliceIndex != nil {
			entry["slice_index"] = *ch.SliceIndex
			name = fmt.Sprintf("slice %d (child %s)", *ch.SliceIndex, ch.ID)
		}
		uncovered = append(uncovered, entry)
		names = append(names, name)
	}
	if len(uncovered) == 0 {
		return nil, nil
	}

	prOpen := runRow.PullRequestURL != nil && *runRow.PullRequestURL != ""
	gating := planreview.ResolveAuthority(*cfg) == planreview.AuthorityGating
	nextStep, how := reviewCoverageNextStep(parent, prOpen, gating)
	var parentRound any
	if parent.Started {
		parentRound = map[string]any{
			"sequence":   parent.Sequence,
			"configured": parent.Configured,
			"terminal":   parent.Terminal,
			"verdicts":   parent.Verdicts,
			"in_flight":  parent.inFlight(),
		}
	}
	return &mergeReadinessRefusal{
		Code: mergeCodeReviewCoverageIncomplete,
		Message: fmt.Sprintf("review coverage is incomplete: %s of this decomposed parent %s covered by a successful agent review — neither the parent's latest implement-review round nor the slice's own latest round recorded an implement_reviewed verdict (a round whose reviewers all failed or were skipped covers nothing). %s, then re-invoke the merge. No merge verdict was recorded.",
			strings.Join(names, ", "), pluralWas(len(names)), how),
		Details: map[string]any{
			"uncovered_slices": uncovered,
			"parent_round":     parentRound,
			"next_step":        nextStep,
		},
	}, nil
}

// reviewCoverageNextStep names the verb that clears the hold from the parent
// round's state, plus the sentence the refusal message uses. A settled parent
// round with no verdict is re-runnable; an in-flight one is awaited.
//
// A parent with NO round (#4282) is awaited until its consolidated PR opens —
// the orchestrator dispatches the consolidated round then. Once the PR is open
// and still no round exists, the dispatch may have failed (a forge compare
// error, or no forge wired), and nothing re-fires it: the re-run verb starts
// the first consolidated round, so next_step is fishhawk_rerun_review. A GATING
// parent is the exception (approval condition C5): the verb refuses it
// gating_authority, so next_step stays fishhawk_await_review and the sentence
// names fishhawk_fixup_stage as the remedy when the dispatch failed, rather
// than bouncing the operator between two surfaces.
func reviewCoverageNextStep(parent implementRoundCoverage, prOpen, gating bool) (string, string) {
	switch {
	case parent.inFlight():
		return reviewCoverageNextAwait, fmt.Sprintf("The parent's implement-review round %d is still in flight: await it with fishhawk_await_review", parent.Sequence)
	case parent.Started:
		return reviewCoverageNextRerun, fmt.Sprintf("The parent's latest implement-review round %d settled with no verdict: re-run it with fishhawk_rerun_review (POST /v0/runs/{run_id}/reviews/rerun)", parent.Sequence)
	case !prOpen:
		return reviewCoverageNextAwait, "No implement-review round has been dispatched on the parent yet and its consolidated PR is not open: await the consolidated review with fishhawk_await_review"
	case gating:
		return reviewCoverageNextAwait, "No implement-review round has been dispatched on the parent although its consolidated PR is open. Its implement reviewers are gating, which fishhawk_rerun_review refuses (gating_authority): await the consolidated review with fishhawk_await_review, or, if the consolidated dispatch failed, route fishhawk_fixup_stage on the parent implement stage, whose pushed head dispatches the review"
	}
	return reviewCoverageNextRerun, "No implement-review round has been dispatched on the parent although its consolidated PR is open: start its first consolidated round with fishhawk_rerun_review (POST /v0/runs/{run_id}/reviews/rerun), which dedups against a dispatch already under way and reports a fan-in verify hold or an unwired forge compare"
}

// sliceOrderKey orders children by slice index, unindexed children last, ties
// broken by id, so uncovered_slices is deterministic.
func sliceOrderKey(r *run.Run) string {
	if r.SliceIndex != nil {
		return fmt.Sprintf("0:%08d:%s", *r.SliceIndex, r.ID)
	}
	return "1:" + r.ID.String()
}

func pluralWas(n int) string {
	if n == 1 {
		return "was never"
	}
	return "were never"
}

// reviewCoverageDriveAction maps a review_coverage_incomplete refusal to the
// drive next_action verb: rerun_review when next_step names the re-run verb (a
// re-runnable parent round, or a parent whose first consolidated round it can
// start, #4282), await_review otherwise. Never merge_pr.
func reviewCoverageDriveAction(ref *mergeReadinessRefusal) string {
	if ref != nil && ref.Details["next_step"] == reviewCoverageNextRerun {
		return "rerun_review"
	}
	return "await_review"
}
