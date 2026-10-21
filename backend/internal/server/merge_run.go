package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/delegation"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/mergequeue"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/stagecheck"
)

// CategoryMergeVerdictRecorded is the audit-log category for the chained entry
// handleMergeRun writes when an operator records their merge verdict and queues
// the squash merge (E48.7 / #1954). It is a durable, operator-authored
// declaration modeled on operator_commit_vouched (vouch.go): the payload names
// the run, the verdict prose, the PR url, and delegated:false (this is the
// human merge path, distinct from the delegated may_merge arm). Internal audit
// kind projected through the living-anchor timeline — NOT a new issue-comment
// surface, so docs/issue-comment-surfaces.md is untouched.
const CategoryMergeVerdictRecorded = "merge_verdict_recorded"

// mergeRunRequest is the JSON body of POST /v0/runs/{run_id}/merge. verdict is
// required non-empty: the merge is an audited operator declaration, so it must
// carry the operator's verdict prose.
type mergeRunRequest struct {
	Verdict string `json:"verdict"`
}

// mergeRunResponse reports the recorded verdict + queued merge. merge_queued is
// true once the merge helper was dispatched; already_recorded is true when a
// prior merge_verdict_recorded row existed (an idempotent re-POST) so no fresh
// row was appended.
//
// E45.87 / #3622 widens it with four ADDITIVE fields, all zero-valued on the
// ordinary dispatch path so existing consumers are byte-compatible:
//
//   - already_merged — the PR/MR was ALREADY merged, so NO merge was queued
//     (merge_queued is false on that path).
//   - merge_observation_recorded — THIS call appended the
//     merge_observation_recorded row. TRUE ONLY when the append SUCCEEDED
//     (binding approval condition 1): false when the chain already carried
//     evidence, and false when the append itself failed.
//   - run_state — the run's lifecycle state after the best-effort completion
//     re-evaluation; empty when the re-read failed.
//   - message — the operator-facing explanation on the already-merged path:
//     what was (or was not) recorded, and which recovery verb to reach for.
type mergeRunResponse struct {
	RunID           string `json:"run_id"`
	MergeQueued     bool   `json:"merge_queued"`
	VerdictSequence int64  `json:"verdict_sequence"`
	AlreadyRecorded bool   `json:"already_recorded"`
	PRURL           string `json:"pr_url"`

	AlreadyMerged            bool   `json:"already_merged,omitempty"`
	MergeObservationRecorded bool   `json:"merge_observation_recorded,omitempty"`
	RunState                 string `json:"run_state,omitempty"`
	Message                  string `json:"message,omitempty"`

	// ADR-093 slice 1 (E87.4 / #4295) widens it ADDITIVELY for the 202
	// queued answer of the merge-queue enqueue arm (mergeQueueEnqueueArm);
	// every field is zero-valued (omitted) on the 200 paths, and merge_queued
	// stays false when queued is true. queue_position is the 1-based place
	// among the base's held entries (omitted for a live entry);
	// intent_sequence is the merge_queue_entered row's chain sequence;
	// already_queued is true when the entry already carried merge intent, so
	// nothing was written.
	Queued          bool   `json:"queued,omitempty"`
	QueueState      string `json:"queue_state,omitempty"`
	QueuePosition   int    `json:"queue_position,omitempty"`
	LiveRunID       string `json:"live_run_id,omitempty"`
	EntryID         string `json:"entry_id,omitempty"`
	BaseRef         string `json:"base_ref,omitempty"`
	AnchoredHeadSHA string `json:"anchored_head_sha,omitempty"`
	IntentSequence  int64  `json:"intent_sequence,omitempty"`
	AlreadyQueued   bool   `json:"already_queued,omitempty"`
}

// handleMergeRun implements POST /v0/runs/{run_id}/merge (E48.7 / #1954): the
// one-verb operator merge path. It records the operator's merge verdict as a
// chained merge_verdict_recorded audit entry (modeled on vouch.go) and queues
// the squash merge through the SAME GitHubMerger seam the delegated may_merge
// arm of AutoDriveRunGate dispatches through (dispatchAcceptanceGatedMerge), so
// the human merge and the delegated merge converge on one path by construction.
// The PR-approval review itself stays a gh step under the operator's own GitHub
// identity (the 2026-07-15 option-a decision; App-identity approval is deferred
// to E39) — this endpoint only records the verdict + queues the merge.
//
// Auth ladder (mirrors vouch.go's operator-only posture):
//   - anonymous → 401 authentication_required;
//   - a run-bound MCP token (subject "mcp:run:<uuid>") → 403
//     run_token_forbidden, even for its own run — an agent self-merging its own
//     PR would bypass the operator gate;
//   - any identity missing write:approvals → 403 insufficient_scope, enforced
//     UNCONDITIONALLY (no cookie-session bypass): queueing a real squash merge
//     is a scoped operator action.
//
// Guards, ALL fail-closed and evaluated BEFORE any write:
//   - 404 when the run is unknown;
//   - 409 when the run carries no PR url (nothing to merge);
//   - 409 when the run is failed or cancelled (terminal-not-succeeded);
//   - 409 when the acceptance gate does not admit a merge (pending / failed /
//     settled-outcome-unknown / read error — ADR-049 decision #6);
//   - 409 acceptance_stale (#4086) when the acceptance stage carries a
//     stage-scoped acceptance_reopened entry newer than the newest recorded
//     outcome — the gate above still reads that stale outcome as merge-eligible.
//     It names the verified and current heads and the re-run verb, and an
//     audit read error is a 500 with nothing recorded (fail-closed). It runs
//     BEFORE the GateMerger guard because it needs no merge seam;
//   - 409 review_coverage_incomplete (#4219) when the run is a decomposed
//     parent with a succeeded slice no SUCCESSFUL agent review covers (see
//     reviewCoverageRefusal). It names the uncovered slices and the verb that
//     clears it (fishhawk_rerun_review, or fishhawk_await_review while a round
//     is in flight). A read error is a 500 with nothing recorded
//     (fail-closed). It runs right after acceptance_stale, before the
//     GateMerger guard;
//   - 409 first_officer_finding_open (ADR-089 D4, E85.6 / #4148) while a
//     first_officer concern is open on the run (firstOfficerHoldRefusal). It
//     names the concern ids and how to disposition them; a read error is a 500
//     with nothing recorded (fail-closed). It runs right after
//     review_coverage_incomplete, before the GateMerger guard;
//   - 503 when the merge seam (GateMerger) is unconfigured;
//   - 409 merge_conflicting when the PR has a merge conflict against its base
//     (E64.14 / #3109). This guard is BEST-EFFORT and FAIL-OPEN — it refuses
//     ONLY on an explicit forge signal (mergeable_state=="dirty" or a
//     documented mergeable==false) and proceeds on every uncertainty (GitHub
//     unwired, no installation, unparseable repo/PR, a GetPullRequest error, or
//     a still-computing null mergeable). It runs AFTER the GateMerger guard and
//     BEFORE the merge_verdict_recorded append, so a merge that structurally
//     cannot queue records no verdict; the post-resolution route is
//     operator-resolve-then-vouch-then-re-merge.
//   - the merge-candidate gate (ADR-090 / E83.33 / #4018), AFTER the conflict
//     guard and BEFORE the verdict append: 409 merge_base_behind when the PR
//     head does not contain the base tip, 409 merge_candidate_unverified /
//     merge_candidate_verify_failed when a Fishhawk-written head (or an
//     operator-vouched head that brings in base commits, ADR-090 D8
//     Amendment 1 / #4368) has no passing merge-candidate verify, and 502
//     merge_candidate_check_failed on a forge or audit read error once every
//     anchor resolved. See
//     mergeCandidateGate for the determinability ladder. Inside it, BEFORE the
//     behind check, 409 merge_candidate_held (ADR-092 / #4200) when the run's
//     pass is HELD in its base's queue (fail-open on a queue read error).
//     With the queue wired, a merge_base_behind / merge_candidate_unverified
//     / merge_candidate_held refusal of a MERGE-QUEUE ELIGIBLE run becomes a
//     202 queued answer instead (ADR-093 slice 1, mergeQueueEnqueueArm); an
//     ineligible run keeps the 409 with an additive merge_queue_ineligible
//     detail and nothing written.
//   - 409 approval_dismissed (#4086), AFTER the merge-candidate gate and
//     BEFORE the verdict append: GitHub reports mergeable_state=="blocked", no
//     reviewer's latest review is APPROVED and one is DISMISSED, so the queued
//     merge would never fire. BEST-EFFORT and FAIL-OPEN like the conflict
//     guard: every uncertainty (see approvalDismissedCheck) admits and is
//     logged with its reason. A never-reviewed PR still queues.
//
// It deliberately does NOT block on a review stage parked at awaiting_approval:
// in feature_change that stage settles ON merge via resolveReviewStageOnMerge,
// so requiring it settled first would deadlock the human merge path.
//
// Idempotence lives on the ENDPOINT, and it has TWO halves that must be stated
// separately (E45.87 / #3622 narrows what used to be a bare "the endpoint is
// idempotent"):
//
//   - the VERDICT half (binding approval condition 1 of #1954): a repeated POST
//     that finds an existing merge_verdict_recorded row appends NO duplicate row
//     and responds already_recorded:true;
//   - the DISPATCH half: a re-POST no longer re-dispatches BLINDLY. Re-queuing a
//     merge for a PR/MR that has already merged errors on both forges, so the
//     documented "re-invoke to resume" recovery returned 502
//     merge_dispatch_failed in the exact case where the operation had WORKED
//     (the merge settled through the webhook during the bounded wait). The
//     observe-before-dispatch rung below reads the chain on every POST and, on a
//     RESUME, the forge; when the PR is already merged it SKIPS the dispatch and
//     answers 200 already_merged:true / merge_queued:false. Every uncertainty
//     falls through to the dispatch exactly as before.
//
// On the merge helper erroring the handler branches on the cause. A
// checks-not-all-passed refusal (forge.ErrPullRequestUnstableStatus — GitHub
// reports the PR in UNSTABLE status, E67.56 / #2717) returns 409
// merge_checks_pending: an expected precondition, not a fault, so the operator
// (and fishhawk_merge_run's bounded wait) is told the checks have not all
// passed and that a failed check means inspecting the PR rather than waiting.
// EVERY other error returns 502 merge_dispatch_failed stating the verdict row is
// durable and the queue step is retryable — a genuine dispatch failure is never
// masked as "just waiting".
func (s *Server) handleMergeRun(w http.ResponseWriter, r *http.Request) {
	id := IdentityFrom(r.Context())
	if id.IsAnonymous() {
		s.writeError(w, r, http.StatusUnauthorized, "authentication_required",
			"an authenticated token is required", nil)
		return
	}
	// Operator-token-only: a run-bound agent token may NEVER queue a merge —
	// not even for its own run. An agent merging its own PR would bypass the
	// operator gate. Rejected outright, mirroring vouch.go / the #961
	// decide_scope_amendment guard.
	if _, runBound := runBoundTokenRunID(id); runBound {
		s.writeError(w, r, http.StatusForbidden, "run_token_forbidden",
			"a run-bound agent token may not queue a merge; recording a merge verdict and merging is an operator action",
			nil)
		return
	}
	// write:approvals enforced UNCONDITIONALLY (no cookie-session bypass): the
	// merge verb records an approval-class verdict AND dispatches a real squash
	// merge, so an authenticated-but-unscoped identity must not reach it.
	if !hasScope(id, "write:approvals") {
		s.writeError(w, r, http.StatusForbidden, "insufficient_scope",
			"token is missing required scope: write:approvals",
			map[string]any{"required_scope": "write:approvals"})
		return
	}

	if s.cfg.RunRepo == nil || s.cfg.AuditRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "merge_unconfigured",
			"merge endpoint requires run + audit repositories", nil)
		return
	}

	runID, err := uuid.Parse(r.PathValue("run_id"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"run_id must be a valid UUID",
			map[string]any{"field": "run_id", "got": r.PathValue("run_id")})
		return
	}

	var reqBody mergeRunRequest
	if r.Body != nil {
		if decErr := json.NewDecoder(r.Body).Decode(&reqBody); decErr != nil && !errors.Is(decErr, io.EOF) {
			s.writeError(w, r, http.StatusBadRequest, "validation_failed",
				"request body must be valid JSON {verdict}",
				map[string]any{"error": decErr.Error()})
			return
		}
	}
	verdict := strings.TrimSpace(reqBody.Verdict)
	if verdict == "" {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"verdict is required: the merge is an audited operator declaration; state your merge verdict",
			map[string]any{"field": "verdict"})
		return
	}

	runRow, err := s.cfg.RunRepo.GetRun(r.Context(), runID)
	if err != nil {
		if errors.Is(err, run.ErrNotFound) {
			s.writeError(w, r, http.StatusNotFound, "run_not_found",
				"no run with that id", map[string]any{"run_id": runID.String()})
			return
		}
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"get run failed", map[string]any{"error": err.Error()})
		return
	}

	// Fail-closed guard: no PR to merge.
	if runRow.PullRequestURL == nil || *runRow.PullRequestURL == "" {
		s.writeError(w, r, http.StatusConflict, "run_not_mergeable",
			"run has no pull request url; there is nothing to merge",
			map[string]any{"run_id": runID.String()})
		return
	}
	prURL := *runRow.PullRequestURL

	// Fail-closed guard: a failed / cancelled run is terminal-not-succeeded and
	// must not be merged.
	if runRow.State == run.StateFailed || runRow.State == run.StateCancelled {
		s.writeError(w, r, http.StatusConflict, "run_not_mergeable",
			"run is "+string(runRow.State)+"; a failed or cancelled run cannot be merged",
			map[string]any{"run_id": runID.String(), "state": string(runRow.State)})
		return
	}

	// Fail-closed guard: the acceptance gate must admit the merge (ADR-049
	// decision #6). Read the stages once and classify, then defer the admitted
	// set to the SHARED acceptanceGateAdmitsMerge predicate so this endpoint, the
	// delegated merge, and the drive presentation cannot drift (E66.37 / #2474).
	// Any pending / un-arbitrated-failed / outcome-unknown / read-error state →
	// 409; passed / not-declared / skipped-out-of-scope / not-validated /
	// arbitrated / undecidable proceed. arbitrated (#2474) is a FAILED verdict
	// whose paged triage an operator discharged on the audit chain —
	// merge-eligible because the override is itself an audited operator act, not
	// because the evidence changed. not-validated (#2347) is the
	// short-circuited zero-criteria-verified outcome: merge-ELIGIBLE by design,
	// because a change with no live target must not be stranded — the operator
	// reads the distinction from the run's next_actions state and status comment,
	// not from a block here. undecidable (#2512) is the post-run twin of that
	// honesty: the stage RAN and reported rows of which at least one could not be
	// DECIDED, with nothing failing — merge-ELIGIBLE with no arbitration at all,
	// which is exactly the #2474 wedge-surface reduction (before it, a validator
	// that could not decide a criterion had to ship `failed` and page a human).
	// Deliberately does NOT block on a review stage
	// awaiting approval (resolveReviewStageOnMerge settles it ON merge).
	stages, err := s.cfg.RunRepo.ListStagesForRun(r.Context(), runID)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"list stages failed", map[string]any{"error": err.Error()})
		return
	}
	gateState, gerr := s.acceptanceGateState(r.Context(), runRow, stages)
	acceptanceMergeOK := gerr == nil && acceptanceGateAdmitsMerge(gateState)
	if !acceptanceMergeOK {
		s.cfg.Logger.LogAttrs(r.Context(), slog.LevelInfo, "merge: acceptance gate does not admit the merge",
			slog.String("run_id", runID.String()),
			slog.String("acceptance_gate_state", gateState),
			slog.Bool("acceptance_read_error", gerr != nil))
		s.writeError(w, r, http.StatusConflict, "acceptance_gate_not_passed",
			"the acceptance gate does not admit a merge (must be passed, not-declared, skipped-out-of-scope, not-validated, undecidable, or arbitrated — POST /v0/runs/{run_id}/acceptance-arbitration to discharge a PAGED triage)",
			map[string]any{"run_id": runID.String(), "acceptance_gate_state": gateState})
		return
	}

	// Stale-acceptance guard (#4086): the acceptance gate above reads the
	// NEWEST recorded outcome, so after a fix-up push re-opened the stage it
	// still admits on the stale `passed` verdict and the merge then times out
	// behind a pending fishhawk_audit_complete. Refuse fast instead, naming
	// both heads and the re-run verb. Placed BEFORE the GateMerger guard: it
	// needs no merge seam, and both refusals record nothing. FAIL-CLOSED on an
	// audit read error (500, nothing recorded), like the acceptance gate.
	stale, serr := s.acceptanceStaleRefusal(r.Context(), runRow, stages)
	if serr != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"read acceptance re-open history failed", map[string]any{"error": serr.Error()})
		return
	}
	if stale != nil {
		s.writeMergeReadinessRefusal(w, r, runID, prURL, stale)
		return
	}

	// Review-coverage hold (#4219): a decomposed parent merges only when every
	// slice is covered by a SUCCESSFUL agent review — the parent's latest
	// implement-review round carries a verdict, or each succeeded slice's own
	// latest round does. An infra-failed consolidated round covers nothing.
	// Same placement and posture as the stale-acceptance guard: before the
	// GateMerger guard (no merge seam needed), 409 with nothing recorded, and
	// FAIL-CLOSED on a read error (500, nothing recorded).
	coverage, cerr := s.reviewCoverageRefusal(r.Context(), runRow)
	if cerr != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"read review coverage failed", map[string]any{"error": cerr.Error()})
		return
	}
	if coverage != nil {
		s.writeMergeReadinessRefusal(w, r, runID, prURL, coverage)
		return
	}

	// First-officer hold (ADR-089 D4, E85.6 / #4148): an open first_officer
	// concern — a seat finding backed by a runner-attested failing specialist
	// job — refuses the merge 409 first_officer_finding_open until the captain
	// dispositions it. Same placement and posture as the review-coverage hold:
	// nothing recorded, FAIL-CLOSED on a read error (500).
	foHold, ferr := s.firstOfficerHoldRefusal(r.Context(), runID, "")
	if ferr != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"read open first-officer concerns failed", map[string]any{"error": ferr.Error()})
		return
	}
	if foHold != nil {
		s.writeMergeReadinessRefusal(w, r, runID, prURL, foHold)
		return
	}

	// Fail-closed guard: the merge seam must be configured BEFORE any write, so
	// a merge that can never be dispatched never records a verdict.
	if s.cfg.GateMerger == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "merge_seam_unconfigured",
			"merge endpoint requires a configured merge client", nil)
		return
	}

	// Conflict precondition (E64.14 / #3109): a PR whose base has advanced into
	// a merge conflict can never fire GitHub's auto-merge, so queuing it would
	// only time out at 360s with a message that says nothing about conflicts.
	// Refuse BEFORE the merge_verdict_recorded append (binding-ratified
	// ordering): a durable verdict for a merge that structurally cannot queue is
	// a false record. A re-POST after the operator resolves the conflict records
	// the verdict normally. This guard is BEST-EFFORT / FAIL-OPEN — it reports a
	// conflict ONLY on an explicit forge signal (see prMergeConflicting); every
	// uncertainty (GitHub unwired, no installation, unparseable repo/PR, a
	// GetPullRequest error, a still-computing null mergeable) proceeds exactly as
	// today. GitLab is unclassified here: the forge fields stay zero on that
	// adapter, so a conflicting GitLab MR falls through to today's timeout
	// behavior (merge_status / detailed_merge_status would be the GitLab signal).
	if conflicting, mergeableState := s.prMergeConflicting(r.Context(), runRow); conflicting {
		s.cfg.Logger.LogAttrs(r.Context(), slog.LevelInfo, "merge: pull request is conflicting; refusing before verdict",
			slog.String("run_id", runID.String()),
			slog.String("mergeable_state", mergeableState))
		s.writeError(w, r, http.StatusConflict, "merge_conflicting",
			"the pull request has a merge conflict against its base and GitHub can never queue the merge; resolve the conflict on the run branch, then vouch the resulting commit with fishhawk_vouch_commit so the fishhawk_audit_complete check re-posts on the new head, re-approve the pull request, and re-invoke the merge",
			map[string]any{"run_id": runID.String(), "pr_url": prURL, "mergeable_state": mergeableState})
		return
	}

	// Merge-candidate gate (ADR-090 D1/D2/D7, E83.33 / #4018): Fishhawk
	// verifies the MERGE CANDIDATE, not the branch. AFTER the #3109 conflict
	// guard (a conflicting PR keeps its own, more specific refusal) and BEFORE
	// the merge_verdict_recorded append, so a merge that cannot land records no
	// verdict. Unlike the conflict guard it FAILS CLOSED on a read error once
	// every anchor resolved (D7); see mergeCandidateGate.
	gate := s.mergeCandidateGate(r.Context(), runRow)
	if !gate.admits() {
		// ADR-093 slice 1 (E87.4 / #4295): an ELIGIBLE run the gate refuses
		// as behind / unverified / held is ENQUEUED (202 queued) instead; an
		// ineligible one keeps today's refusal, plus the reason codes, with
		// nothing written.
		handled, ineligible := s.mergeQueueEnqueueArm(w, r, runRow, prURL, verdict, id.Subject, gate)
		if handled {
			return
		}
		if ineligible != nil {
			if gate.Details == nil {
				gate.Details = map[string]any{}
			}
			gate.Details["merge_queue_ineligible"] = ineligible
		}
		s.writeMergeCandidateRefusal(w, r, runID, prURL, gate)
		return
	}

	// Dismissed-approval precondition (#4086): a vouch commit, fix-up push or
	// rebase can dismiss the PR's approval, after which GitHub reports the PR
	// blocked and the queued merge never fires. AFTER the merge-candidate gate
	// and BEFORE the verdict append, so that merge records no verdict.
	// BEST-EFFORT / FAIL-OPEN like the conflict guard: every uncertainty admits
	// and is logged with its reason (see approvalDismissedCheck).
	approval := s.approvalDismissedCheck(r.Context(), runRow)
	if approval.Refusal != nil {
		s.writeMergeReadinessRefusal(w, r, runID, prURL, approval.Refusal)
		return
	}
	if !approval.Determined {
		s.cfg.Logger.LogAttrs(r.Context(), slog.LevelInfo,
			"merge: approval-dismissed check undetermined; proceeding (fail open)",
			slog.String("run_id", runID.String()),
			slog.String("undetermined", approval.Undetermined))
	}

	// Idempotence on the ENDPOINT (binding condition 1): an existing
	// merge_verdict_recorded row means a prior POST already recorded the
	// verdict; do NOT append a duplicate. Either way the merge helper is
	// dispatched below, so a 502-then-reinvoke re-queues the merge.
	//
	// The read-then-append fast-path covers the SEQUENTIAL retry flow (a
	// 502-then-reinvoke, or a timed-out re-invoke). A genuinely CONCURRENT
	// double-POST for the same run — where both observe zero rows and both
	// append — is closed at the DB level by the 0062 partial unique index
	// audit_entries_merge_verdict_recorded_once_idx (#1983): AppendChainedTx's
	// SELECT ... FOR UPDATE on the run row serializes the two appends, so the
	// race-loser's insert deterministically violates that index and returns a
	// unique_violation. audit.IsMergeVerdictDuplicate catches ONLY that
	// index's collision (not an unrelated 23505 on the hash-chain / entry-hash
	// / (run_id, sequence) uniqueness); the loser re-reads the winner's
	// sequence, responds already_recorded, and still dispatches the merge.
	subject := id.Subject
	if subject == "" {
		subject = "anonymous"
	}
	existing, err := s.cfg.AuditRepo.ListForRunByCategory(r.Context(), runID, CategoryMergeVerdictRecorded)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"read prior merge verdict failed", map[string]any{"error": err.Error()})
		return
	}
	var verdictSequence int64
	alreadyRecorded := len(existing) > 0
	if alreadyRecorded {
		// Reuse the earliest recorded verdict's sequence (chain-stable).
		verdictSequence = earliestMergeVerdictSequence(existing)
	} else {
		// Delegation shadow stamp (E82.1 / #3778): captured on the
		// PRE-decision state, recorded ONLY on the fresh-append path below —
		// never on alreadyRecorded or the lost-race duplicate path. This
		// handler is never delegated (the delegated merge is the auto-driver).
		shadow := s.captureDelegationShadow(r.Context(), runID, delegation.ActionMerge, subject, false)
		actorKind := audit.ActorUser
		payload, _ := json.Marshal(map[string]any{
			"run_id":    runID.String(),
			"verdict":   verdict,
			"pr_url":    prURL,
			"delegated": false,
		})
		entry, aerr := s.cfg.AuditRepo.AppendChained(r.Context(), audit.ChainAppendParams{
			RunID:        runID,
			Timestamp:    time.Now().UTC(),
			Category:     CategoryMergeVerdictRecorded,
			ActorKind:    &actorKind,
			ActorSubject: &subject,
			Payload:      payload,
		})
		switch {
		case aerr == nil:
			verdictSequence = entry.Sequence
			s.recordDelegationShadow(r.Context(), shadow, nil, "merge", CategoryMergeVerdictRecorded, nil)
		case audit.IsMergeVerdictDuplicate(aerr):
			// Lost the concurrent merge-verdict race: another POST's insert
			// won the partial-unique-index slot. Re-read to recover the
			// winner's sequence, then fall through to the merge dispatch (a
			// benign idempotent outcome, NOT a 500).
			winner, rerr := s.cfg.AuditRepo.ListForRunByCategory(r.Context(), runID, CategoryMergeVerdictRecorded)
			if rerr != nil {
				s.writeError(w, r, http.StatusInternalServerError, "internal_error",
					"re-read merge verdict after concurrent-race conflict failed",
					map[string]any{"error": rerr.Error()})
				return
			}
			if len(winner) == 0 {
				// Defensive fail-closed: the index guarantees the winner's row
				// is committed before the loser's insert can conflict, so this
				// is unreachable — but never fabricate a verdict_sequence on a
				// phantom winner. Surface a 500 rather than dispatch.
				s.cfg.Logger.LogAttrs(r.Context(), slog.LevelError,
					"merge: merge-verdict duplicate caught but re-read found no winner row",
					slog.String("run_id", runID.String()))
				s.writeError(w, r, http.StatusInternalServerError, "internal_error",
					"record merge verdict failed: duplicate conflict but no recorded verdict found on re-read",
					map[string]any{"run_id": runID.String()})
				return
			}
			alreadyRecorded = true
			verdictSequence = earliestMergeVerdictSequence(winner)
		default:
			s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn,
				"merge: append merge_verdict_recorded audit entry failed",
				slog.String("run_id", runID.String()),
				slog.String("error", aerr.Error()))
			s.writeError(w, r, http.StatusInternalServerError, "internal_error",
				"record merge verdict failed", map[string]any{"error": aerr.Error()})
			return
		}
	}

	// OBSERVE BEFORE DISPATCH (E45.87 / #3622). Re-queuing a merge for a PR/MR
	// that has ALREADY merged errors on both forges, so the documented
	// "re-invoke to resume" recovery returned 502 merge_dispatch_failed in the
	// exact case where the operation had WORKED — the merge settled through the
	// webhook during fishhawk_merge_run's bounded wait.
	//
	// PLACEMENT: AFTER the durable merge_verdict_recorded append, unlike the
	// conflict guard above which refuses BEFORE it. The rationale differs by
	// case: the conflict guard refuses a merge that structurally CANNOT queue,
	// so its verdict would be a false record, whereas here the merge genuinely
	// HAPPENED — the operator's verdict is a TRUE record and must be durable.
	//
	// Two tiers, and EVERY uncertainty falls through to today's dispatch (the
	// prMergeConflicting fail-open posture):
	//   (a) a CHEAP chain read on every POST (runPRObservablyMerged over
	//       pr_merged / post_merge_observed / merge_observation_recorded). A
	//       read error is logged and treated as NO evidence — never a 500.
	//   (b) a LIVE forge read on a RESUME ONLY (alreadyRecorded — an existing
	//       verdict row, i.e. the shape this issue reports). A first POST pays
	//       ZERO forge requests.
	if obs := s.observeMergeAlreadyLanded(r.Context(), runID, runRow, subject, alreadyRecorded); obs.Landed {
		s.writeAlreadyMergedResponse(w, r, runID, prURL, verdictSequence, alreadyRecorded, obs)
		return
	}

	// Dispatch the shared merge helper. The verdict row is already durable, so a
	// dispatch failure is a retryable 502 (re-POST re-queues without duplicating
	// the row). The merge only ENABLES/queues GitHub's merge — the pr_merged /
	// run-completion settle is left to the pull_request-closed webhook, which is
	// why the MCP tool awaits the terminal state client-side.
	// E64.42 / #3159: recompute and republish fishhawk_audit_complete
	// immediately BEFORE the dispatch. A fix-up push publishes `in_progress`
	// against the new head and nothing is guaranteed to recompute between that
	// synchronize and this merge; now that the check is REQUIRED, GitHub
	// refuses to queue the merge while it is in_progress and this handler
	// returns 409 merge_checks_pending forever. Healing AFTER the dispatch
	// would be useless — the stranded check is what makes the dispatch fail.
	//
	// Placement: AFTER the acceptance-gate guard (a genuinely mid-flight run
	// recomputes to pending; refusing there is the acceptance gate's job, not
	// this publish's), AFTER the conflict guard and the durable verdict
	// append, and BEFORE the dispatch. The delegated may_merge arm does NOT
	// route through here — it calls GitHubMerger.MergePullRequest via
	// dispatchAcceptanceGatedMerge (autodrive.go), which carries the SAME call
	// — which is why both sites need it.
	//
	// Best-effort and never unwinds: a publish failure logs at WARN inside the
	// helper and the merge is dispatched regardless.
	// The returned Result is what lets the 409 below NAME the blocking
	// audit-complete item instead of describing "required checks" generically
	// (E64.59 / #3190). `auditRecomputed` distinguishes "the recompute ran and
	// the state is not pending" from "we never got to look" (dev posture or a
	// compute error), so the generic wording is used for the second case rather
	// than a misleading claim about a state nothing derived.
	//
	// E86.57 / #4397, #4440: the forced republish targets the head the merge
	// will land — the live PR head the merge-candidate gate admitted — because
	// findHeadSHA cannot resolve a conflict-resolution, base-advance or
	// operator-vouched head and would re-post at a stale recorded head while
	// the live head kept its missing or IN_PROGRESS required check. An empty
	// gate.HeadSHA (ungated D7 admit, merged PR) keeps the findHeadSHA head.
	auditRes, auditRecomputed := s.republishAuditCheckBeforeMergeAtHead(r.Context(), runID, gate.HeadSHA)

	if merr := s.cfg.GateMerger.MergePullRequest(r.Context(), runRow); merr != nil {
		// A checks-not-all-passed refusal (E67.56 / #2717) is an expected
		// precondition, NOT a dispatch fault: GitHub reports the PR in UNSTABLE
		// status and refuses to queue auto-merge until its required checks pass.
		// Classify it distinctly as 409 merge_checks_pending so the operator gets
		// a remedy that can work (wait for the checks, or inspect the PR if a
		// check has already failed) instead of the generic 502's "retry the
		// merge", which reproduces the identical failure. The verdict row is
		// already durable, so the tool re-POSTs across a bounded wait with no
		// duplicate row. EVERY other error keeps the generic 502 so a real
		// dispatch failure is never swallowed as "just waiting".
		if errors.Is(merr, forge.ErrPullRequestUnstableStatus) {
			s.cfg.Logger.LogAttrs(r.Context(), slog.LevelInfo, "merge: checks not all passed (unstable status)",
				slog.String("run_id", runID.String()), slog.String("error", merr.Error()))
			details := map[string]any{"run_id": runID.String(), "verdict_sequence": verdictSequence, "pr_url": prURL, "reason": "checks_pending"}
			msg := "the merge verdict is recorded and durable, but the squash merge cannot be queued because the pull request's required checks have not all passed (GitHub reports the pull request in unstable status). An immediate retry cannot succeed. If the checks are still pending, re-invoke once they complete; if a required check has already FAILED, the merge will never queue — inspect the pull request instead of waiting."
			// E64.59 / #3190: when the blocking check is OUR OWN
			// fishhawk_audit_complete sitting at pending, say WHY and WHAT TO
			// DO. Purely additive — the existing keys are untouched, and a
			// recompute that produced nothing, or a non-pending state, keeps
			// the generic wording so a genuinely unrelated failing required
			// check is never mislabelled.
			if auditRecomputed && auditRes.State == stagecheck.StatePending && len(auditRes.Missing) > 0 {
				items := make([]map[string]any, 0, len(auditRes.Missing))
				for _, m := range auditRes.Missing {
					items = append(items, map[string]any{"kind": string(m.Kind), "detail": m.Detail})
				}
				details["audit_complete_state"] = string(auditRes.State)
				details["audit_complete_missing"] = items
				msg += " fishhawk_audit_complete is pending because: " + auditRes.Missing[0].Detail
			}
			s.writeError(w, r, http.StatusConflict, "merge_checks_pending", msg, details)
			return
		}
		// RE-OBSERVE ONCE (E45.87 / #3622). A merge that landed in the
		// observe-to-dispatch window — or one the dispatch itself rejected
		// BECAUSE the PR was already merged — resolves as already_merged
		// instead of a 502 that tells the operator to retry an operation that
		// already succeeded. The forge read is unconditional here (not gated on
		// alreadyRecorded): the dispatch has already failed, so the extra read
		// is paid only on an error path.
		obs := s.observeMergeAlreadyLanded(r.Context(), runID, runRow, subject, true)
		if obs.Landed {
			s.writeAlreadyMergedResponse(w, r, runID, prURL, verdictSequence, alreadyRecorded, obs)
			return
		}
		s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn, "merge: dispatch merge failed",
			slog.String("run_id", runID.String()), slog.String("error", merr.Error()),
			slog.String("forge_merge_state", obs.ForgeState))
		// The observed state is named in the MESSAGE, not only in the details:
		// a 5xx body's details are default-deny redacted (errors.go's
		// redactableDetailKeys), so an un-allow-listed forge_merge_state key
		// would never reach the caller. It is still passed as a detail because
		// writeError logs the FULL pre-redaction map against the error_ref, so
		// the operator's log record carries it alongside the raw cause.
		s.writeError(w, r, http.StatusBadGateway, "merge_dispatch_failed",
			"the merge verdict is recorded and durable, but queuing the squash merge failed; retry the merge. The pull request was re-read from the forge and did NOT confirm a merge (forge_merge_state="+obs.ForgeState+"). If the pull request has in fact already merged, POST /v0/runs/{run_id}/record-merge-observation to record the merge observation, then POST /v0/runs/{run_id}/reconcile-merge to settle the run.",
			map[string]any{
				"run_id":            runID.String(),
				"error":             merr.Error(),
				"verdict_sequence":  verdictSequence,
				"forge_merge_state": obs.ForgeState,
			})
		return
	}

	s.writeJSON(w, r, http.StatusOK, mergeRunResponse{
		RunID:           runID.String(),
		MergeQueued:     true,
		VerdictSequence: verdictSequence,
		AlreadyRecorded: alreadyRecorded,
		PRURL:           prURL,
	})
}

// prMergeConflicting reports whether the run's pull request has a merge
// conflict against its base (E64.14 / #3109) — a state in which GitHub can
// never fire the queued auto-merge. It is BEST-EFFORT and FAIL-OPEN: it reuses
// the established lineage.go resolution idiom (nil GitHub client, nil/zero
// installation id, unparseable repo, non-positive PR number, or a
// GetPullRequest error all return (false, "") plus a warn log — i.e. behave
// exactly as before this guard existed), and reports conflicting ONLY on an
// explicit forge signal.
//
// The signal is MergeableState == "dirty" (GitHub's merge-conflict value) OR a
// documented Mergeable == false. mergeable_state "blocked" / "behind" /
// "unstable" / "draft" / "unknown" / "" and a nil Mergeable (GitHub's
// background mergeability job is still running, returning JSON null) all
// proceed unchanged — a behind-but-clean or checks-pending branch is not a
// conflict, so THIS guard does not refuse it (a behind branch is refused by
// the ADR-090 merge-candidate gate that runs next, as merge_base_behind; see
// mergeCandidateGate). The documented boolean is kept load-bearing alongside the
// advisory mergeable_state (binding condition 2): mergeable_state can never
// quietly become the only path.
//
// GitLab: the forge fields are zero on that adapter, so a conflicting GitLab MR
// is not classified here and falls through to today's queue-then-timeout
// behavior. Fail-open on an unknown signal is deliberate.
func (s *Server) prMergeConflicting(ctx context.Context, runRow *run.Run) (conflicting bool, mergeableState string) {
	if s.cfg.GitHub == nil {
		return false, ""
	}
	if runRow.InstallationID == nil || *runRow.InstallationID == 0 {
		return false, ""
	}
	repo, err := parseRepoOwnerName(runRow.Repo)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"merge: conflict precondition: unparseable repo; proceeding (fail open)",
			slog.String("run_id", runRow.ID.String()),
			slog.String("repo", runRow.Repo),
			slog.String("error", err.Error()))
		return false, ""
	}
	prNumber := parsePRNumberFromURL(runRow.PullRequestURL)
	if prNumber <= 0 {
		return false, ""
	}
	pr, err := s.cfg.GitHub.GetPullRequest(ctx, forge.FromGitHubInstallationID(*runRow.InstallationID), repo, prNumber)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"merge: conflict precondition: get pr failed; proceeding (fail open)",
			slog.String("run_id", runRow.ID.String()),
			slog.Int("pr_number", prNumber),
			slog.String("error", err.Error()))
		return false, ""
	}
	if pr.MergeableState == "dirty" || (pr.Mergeable != nil && !*pr.Mergeable) {
		return true, pr.MergeableState
	}
	return false, ""
}

// Merge-candidate gate error codes (ADR-090 / E83.33 / #4018). The three
// ADR-090 409s are preconditions an operator clears with a named verb; the
// ADR-092 merge_candidate_held 409 clears on its own (admission is automatic);
// the 502 is a read failure on a fully wired run and is retryable.
const (
	mergeCodeBaseBehind            = "merge_base_behind"
	mergeCodeCandidateUnverified   = "merge_candidate_unverified"
	mergeCodeCandidateVerifyFailed = "merge_candidate_verify_failed"
	mergeCodeCandidateCheckFailed  = "merge_candidate_check_failed"
	// mergeCodeCandidateHeld (ADR-092 / E83.85 / #4200): the run's
	// merge-candidate pass is HELD behind its base's live pass. It clears on
	// its own (admission is automatic), so the next step is a status read.
	mergeCodeCandidateHeld = "merge_candidate_held"
)

// Reasons a merge_candidate_check_failed 502 names, carried under the
// allow-listed `reason` detail key so they survive 5xx redaction.
const (
	mergeCandidateCheckForgeRead = "forge_read"
	mergeCandidateCheckAuditRead = "audit_read"
)

// mergeCandidateGateResult is mergeCandidateGate's answer. Code is empty when
// the gate admits the merge; otherwise it is one of the mergeCode* constants
// and Message / Details are the refusal both dispatch paths render. Cause is
// the raw read error on merge_candidate_check_failed: it is LOGGED, never
// shipped to the caller.
type mergeCandidateGateResult struct {
	Code    string
	Message string
	Details map[string]any
	Cause   error
	// HeadSHA is the live PR head the gate read and admitted (E86.57 /
	// #4397). It is set ONLY on the final admit (verify state passed or
	// not_required after the behind probe) and is empty on every refusal, on
	// the merged-PR admit and on every ungated D7 admit. Both merge dispatch
	// sites force-publish fishhawk_audit_complete AT it, because findHeadSHA
	// cannot resolve a conflict-resolution, base-advance or vouched head; an
	// empty value keeps the findHeadSHA resolution.
	HeadSHA string
	// prHeadSHA / prBaseRef are the live PR head and base ref the gate read,
	// set on the merge_base_behind, merge_candidate_unverified and
	// merge_candidate_held refusals: the ADR-093 merge-queue enqueue arm
	// anchors its entry to them (E87.4 / #4295).
	prHeadSHA string
	prBaseRef string
}

func (g mergeCandidateGateResult) admits() bool { return g.Code == "" }

// mergeCandidateCheckFailed builds the D7 fail-closed refusal. The message is
// a static literal per reason: the raw cause rides Cause (logged) and never
// the message, which reaches the caller.
func mergeCandidateCheckFailed(reason string, cause error) mergeCandidateGateResult {
	msg := "the merge-candidate check could not read the pull request from the forge"
	if reason == mergeCandidateCheckAuditRead {
		msg = "the merge-candidate check could not read the run's audit chain"
	}
	msg += ", so the merge is refused (ADR-090 D7: a fully wired GitHub run fails closed on a read error). Nothing was recorded and the call is retryable; re-invoke the merge."
	return mergeCandidateGateResult{
		Code:    mergeCodeCandidateCheckFailed,
		Message: msg,
		Details: map[string]any{"reason": reason, "retryable": true},
		Cause:   cause,
	}
}

// mergeCandidateGate is the ADR-090 merge gate both Fishhawk-owned dispatch
// paths consult — POST /v0/runs/{id}/merge and the delegated may_merge arm —
// before a merge is dispatched.
//
// Determinability ladder (D7), the prMergeConflicting anchors: no GitHub
// client, no installation (which includes every GitLab-family run), an
// unparseable repo, or no PR number means the deployment cannot probe, so the
// gate ADMITS with a WARN log — GitLab merge requests keep today's behaviour as
// a stated residual. Once every anchor resolved, every read FAILS CLOSED with
// merge_candidate_check_failed: a GetPullRequest error, an empty head or base,
// a CompareCommits error, or a mergeCandidateVerifyState error.
//
// Then, in order:
//
//   - a PR the forge reports Merged ADMITS, so the #3622 already-merged resume
//     still answers 200 already_merged;
//   - ADR-092 (#4200): a run whose queue entry is HELD → merge_candidate_held
//     naming its position, the live run and fishhawk_get_run_status, BEFORE
//     the behind check (mergeCandidateHeldRefusal; a queue read error skips
//     this arm);
//   - D1 (universal, Q5): CompareCommits(base=<live PR head>, head=<base ref>)
//     returning ANY commit means the head does not contain the base tip →
//     merge_base_behind naming fishhawk_rebase_run_branch;
//   - D2/D4: mergeCandidateVerifyState for exactly the live head —
//     unverified / in_flight → merge_candidate_unverified, failed →
//     merge_candidate_verify_failed naming fishhawk_fixup_stage (D6: a
//     delegated route was already attempted when the result was recorded)
//     and, as rerun_step, the verify-only re-run (#4391),
//     passed / not_required → admit.
func (s *Server) mergeCandidateGate(ctx context.Context, runRow *run.Run) mergeCandidateGateResult {
	warnUngated := func(why string) mergeCandidateGateResult {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"merge: merge-candidate gate cannot probe this run; proceeding ungated (ADR-090 D7)",
			slog.String("run_id", runRow.ID.String()),
			slog.String("why", why))
		return mergeCandidateGateResult{}
	}
	if s.cfg.GitHub == nil {
		return warnUngated("no GitHub client is wired")
	}
	if runRow.InstallationID == nil || *runRow.InstallationID == 0 {
		return warnUngated("the run carries no GitHub installation")
	}
	repo, err := parseRepoOwnerName(runRow.Repo)
	if err != nil {
		return warnUngated("the run repo is unparseable")
	}
	prNumber := parsePRNumberFromURL(runRow.PullRequestURL)
	if prNumber <= 0 {
		return warnUngated("the run carries no parseable pull request number")
	}
	scope := forge.FromGitHubInstallationID(*runRow.InstallationID)

	pr, err := s.cfg.GitHub.GetPullRequest(ctx, scope, repo, prNumber)
	if err != nil {
		return mergeCandidateCheckFailed(mergeCandidateCheckForgeRead, err)
	}
	if pr.Merged {
		return mergeCandidateGateResult{}
	}
	headSHA, baseRef := pr.HeadSHA, pr.BaseRef
	if headSHA == "" || baseRef == "" {
		return mergeCandidateCheckFailed(mergeCandidateCheckForgeRead,
			errors.New("the forge returned an empty head sha or base ref"))
	}

	// ADR-092 (E83.85 / #4200): a HELD pass refuses BEFORE the behind check —
	// a held PR is expected to be behind (the base moves while it waits), and
	// its admission auto-advances it, so merge_base_behind would name the
	// wrong verb. A live entry falls through to today's checks.
	if held := s.mergeCandidateHeldRefusal(ctx, runRow, headSHA, baseRef); held != nil {
		held.prHeadSHA, held.prBaseRef = headSHA, baseRef
		return *held
	}

	behind, err := s.cfg.GitHub.CompareCommits(ctx, scope, repo, headSHA, baseRef)
	if err != nil {
		return mergeCandidateCheckFailed(mergeCandidateCheckForgeRead, err)
	}
	if len(behind) > 0 {
		return mergeCandidateGateResult{
			Code: mergeCodeBaseBehind,
			Message: fmt.Sprintf("the pull request head %s does not contain the current tip of %s (behind by %d commit(s)), so Fishhawk refuses to merge an unverified merge candidate (ADR-090 D1). Advance the run branch with fishhawk_rebase_run_branch — when the workflow declares a verify command it also authorizes a verify-only merge-candidate pass for the new head — then re-invoke the merge. No merge verdict was recorded.",
				headSHA, baseRef, len(behind)),
			Details: map[string]any{
				"head_sha":  headSHA,
				"base_ref":  baseRef,
				"behind_by": len(behind),
				"next_step": "fishhawk_rebase_run_branch",
			},
			prHeadSHA: headSHA, prBaseRef: baseRef,
		}
	}

	st, err := s.mergeCandidateVerifyState(ctx, runRow, headSHA)
	if err != nil {
		return mergeCandidateCheckFailed(mergeCandidateCheckAuditRead, err)
	}
	switch st.State {
	case mergeCandidateStateInFlight:
		return mergeCandidateGateResult{
			Code: mergeCodeCandidateUnverified,
			Message: fmt.Sprintf("a verify-only merge-candidate pass is in flight for head %s (cause %s) on implement stage %s, and the merge waits for its result (ADR-090 D2). On a local runner dispatch the stage with fishhawk_dispatch_stage, await it with fishhawk_await_stage, then re-invoke the merge. No merge verdict was recorded.",
				headSHA, st.Cause, st.StageID),
			Details: map[string]any{
				"head_sha":     headSHA,
				"cause":        st.Cause,
				"verify_state": st.State,
				"stage_id":     st.StageID.String(),
				"next_step":    "fishhawk_await_stage",
			},
			prHeadSHA: headSHA, prBaseRef: baseRef,
		}
	case mergeCandidateStateUnverified:
		return mergeCandidateGateResult{
			Code: mergeCodeCandidateUnverified,
			Message: fmt.Sprintf("head %s (cause %s) was written by Fishhawk or brings in base commits through an operator-vouched merge, and has no passing merge-candidate verify, so the merge is refused (ADR-090 D2, D8 Amendment 1). Re-invoke fishhawk_rebase_run_branch: its already-up-to-date arm triggers a verify-only pass for the live head. Dispatch and await that stage, then re-invoke the merge. No merge verdict was recorded.",
				headSHA, st.Cause),
			Details: map[string]any{
				"head_sha":     headSHA,
				"cause":        st.Cause,
				"verify_state": st.State,
				"next_step":    "fishhawk_rebase_run_branch",
			},
			prHeadSHA: headSHA, prBaseRef: baseRef,
		}
	case mergeCandidateStateFailed:
		return mergeCandidateGateResult{
			Code: mergeCodeCandidateVerifyFailed,
			Message: fmt.Sprintf("the merge-candidate verify pass FAILED for head %s (cause %s): the declared verify command is red on the combined tree, so the merge is refused (ADR-090 D2/D6). %s %s No merge verdict was recorded.",
				headSHA, st.Cause, mergeCandidateFixupRoute, mergeCandidateRerunHint),
			Details: map[string]any{
				"head_sha":     headSHA,
				"cause":        st.Cause,
				"verify_state": st.State,
				"next_step":    "fishhawk_fixup_stage",
				// #4391: the agent-free alternative when the failure was
				// infrastructure rather than the change.
				"rerun_step": "fishhawk_rebase_run_branch",
			},
		}
	}
	return mergeCandidateGateResult{HeadSHA: headSHA}
}

// mergeCandidateHeldRefusal is the gate's ADR-092 held arm: nil unless the
// run's queue entry is HELD. A queue read error FAILS OPEN with a WARN — the
// held arm is an ordering refusal, and the ADR-090 checks that follow (behind,
// verify state) still fail closed on their own, so an unreadable queue can
// never admit an unverified or behind candidate.
func (s *Server) mergeCandidateHeldRefusal(ctx context.Context, runRow *run.Run, headSHA, baseRef string) *mergeCandidateGateResult {
	st, err := s.mergeCandidateQueueForRun(ctx, runRow.ID)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"merge: merge-candidate queue entry unreadable; skipping the held check (the ADR-090 checks still apply)",
			slog.String("run_id", runRow.ID.String()), slog.String("error", err.Error()))
		return nil
	}
	if st == nil || st.Entry.State != mergequeue.StateHeld {
		return nil
	}
	holder, liveRunID := "no run yet (the next queue event admits it)", ""
	if st.LiveRunID != nil {
		liveRunID = st.LiveRunID.String()
		holder = "run " + liveRunID
	}
	details := map[string]any{
		"queue_state":       string(st.Entry.State),
		"queue_position":    st.Position,
		"anchored_head_sha": st.Entry.AnchoredHeadSHA,
		"head_sha":          headSHA,
		"base_ref":          baseRef,
		"next_step":         "fishhawk_get_run_status",
	}
	if liveRunID != "" {
		details["live_run_id"] = liveRunID
	}
	return &mergeCandidateGateResult{
		Code: mergeCodeCandidateHeld,
		Message: fmt.Sprintf("this run's merge-candidate verify pass is HELD at position %d in the %s queue behind %s, which holds the base's line (ADR-092 D1), so the merge is refused. Admission is automatic: when the line frees the queue advances this branch onto %s if it is behind, re-anchors and starts the pass for the resulting head. A stalled line self-heals on the periodic pump (about 1 minute); fishhawk_rebase_run_branch pumps the base manually. Watch merge_candidate_queue with fishhawk_get_run_status; once admitted, dispatch and await the pass, then re-invoke the merge. No merge verdict was recorded.",
			st.Position, st.Entry.BaseRef, holder, st.Entry.BaseRef),
		Details: details,
	}
}

// writeMergeReadinessRefusal renders a merge-readiness blocker (#4086) as a 409
// carrying run_id, pr_url and the refusal's details.
func (s *Server) writeMergeReadinessRefusal(w http.ResponseWriter, r *http.Request, runID uuid.UUID, prURL string, ref *mergeReadinessRefusal) {
	details := map[string]any{"run_id": runID.String(), "pr_url": prURL}
	for k, v := range ref.Details {
		details[k] = v
	}
	s.cfg.Logger.LogAttrs(r.Context(), slog.LevelInfo, "merge: merge-readiness check refused before verdict",
		slog.String("run_id", runID.String()), slog.String("code", ref.Code))
	s.writeError(w, r, http.StatusConflict, ref.Code, ref.Message, details)
}

// writeMergeCandidateRefusal renders a refused mergeCandidateGate on the
// operator merge endpoint: 502 for merge_candidate_check_failed (the raw cause
// goes to the log and to a non-allow-listed detail key the 5xx redaction
// drops), 409 for the preconditions (the three ADR-090 codes and the ADR-092
// merge_candidate_held).
func (s *Server) writeMergeCandidateRefusal(w http.ResponseWriter, r *http.Request, runID uuid.UUID, prURL string, gate mergeCandidateGateResult) {
	details := map[string]any{"run_id": runID.String(), "pr_url": prURL}
	for k, v := range gate.Details {
		details[k] = v
	}
	attrs := []slog.Attr{slog.String("run_id", runID.String()), slog.String("code", gate.Code)}
	if gate.Code == mergeCodeCandidateCheckFailed {
		cause := ""
		if gate.Cause != nil {
			cause = gate.Cause.Error()
		}
		details["error"] = cause
		s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn, "merge: merge-candidate check failed; refusing (fail closed)",
			append(attrs, slog.String("error", cause))...)
		s.writeError(w, r, http.StatusBadGateway, mergeCodeCandidateCheckFailed, gate.Message, details)
		return
	}
	s.cfg.Logger.LogAttrs(r.Context(), slog.LevelInfo, "merge: merge-candidate gate refused before verdict", attrs...)
	s.writeError(w, r, http.StatusConflict, gate.Code, gate.Message, details)
}

// earliestMergeVerdictSequence returns the smallest Sequence among the given
// merge_verdict_recorded entries (chain-stable: the winning append's row). The
// 0062 partial unique index guarantees at most one such row per run, so this is
// almost always a single-element scan; the loop is retained for defensiveness.
// Callers must pass a non-empty slice.
func earliestMergeVerdictSequence(entries []*audit.Entry) int64 {
	seq := entries[0].Sequence
	for _, e := range entries {
		if e.Sequence < seq {
			seq = e.Sequence
		}
	}
	return seq
}

// mergeObserveResult is what the merge endpoint's observe-before-dispatch rung
// concluded (E45.87 / #3622).
//
// Landed is the only field the dispatch decision reads: true means the PR/MR is
// ALREADY merged and the merge MUST NOT be dispatched — re-queuing a merge for
// an already-merged pull request errors on both forges, which is the whole
// defect. Recorded is true ONLY when THIS call successfully appended the
// merge_observation_recorded row (binding approval condition 1): it is false
// when the chain already carried evidence AND false when the append failed.
// AppendErr carries that append failure so the response can name
// record-merge-observation as the recovery. ForgeState is the stable
// classification token the widened 502 surfaces.
type mergeObserveResult struct {
	Landed     bool
	Recorded   bool
	AppendErr  error
	ForgeState string
}

// observeMergeAlreadyLanded runs the observe rung for handleMergeRun. It is
// BEST-EFFORT and FAIL-OPEN in the established prMergeConflicting posture:
// EVERY uncertainty returns Landed:false so the caller dispatches exactly as it
// does today. It never writes an HTTP response and never returns an error.
//
// Tier (a), always: the CHEAP chain read (runPRObservablyMerged over pr_merged /
// post_merge_observed / merge_observation_recorded). A read error is logged and
// treated as NO evidence — fail open to the dispatch, never a 500.
//
// Tier (b), only when allowForgeRead: the LIVE forge read through the SHARED
// observeForgeMerge ladder, and on its OK verdict the SHARED
// appendMergeObservation. allowForgeRead is the resume-only gate — the happy
// FIRST post pays zero forge requests — and is forced true on the
// dispatch-error re-observe, where the extra read is paid only on an error path.
//
// BINDING APPROVAL CONDITION 1: when the forge CONFIRMS the merge but the
// append FAILS, the result is still Landed:true with Recorded:false. The merge
// already happened, so dispatching it would reproduce the very failure this
// rung exists to prevent; the un-persisted observation is reported to the
// operator with record-merge-observation named as the recovery instead.
func (s *Server) observeMergeAlreadyLanded(ctx context.Context, runID uuid.UUID, runRow *run.Run,
	subject string, allowForgeRead bool) mergeObserveResult {
	already, cerr := s.runPRObservablyMerged(ctx, runID)
	if cerr != nil {
		// Fail OPEN: an unreadable chain is not evidence of a merge, and a
		// merge verb must not 500 because a read it did not need failed.
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"merge: observe rung: chain read failed; proceeding to dispatch (fail open)",
			slog.String("run_id", runID.String()), slog.String("error", cerr.Error()))
		return mergeObserveResult{ForgeState: "chain_read_error"}
	}
	if already {
		return mergeObserveResult{Landed: true, ForgeState: "chain_evidence"}
	}
	if !allowForgeRead {
		// Resume-only gate: a FIRST post never reads the forge.
		return mergeObserveResult{ForgeState: "not_observed"}
	}
	obs, reason, forgeErr := s.observeForgeMerge(ctx, runRow)
	if reason != obsForgeOK {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo,
			"merge: observe rung: forge read did not confirm a merge; proceeding to dispatch (fail open)",
			slog.String("run_id", runID.String()),
			slog.String("forge_merge_state", reason.forgeMergeState()),
			slog.Bool("forge_error", forgeErr != nil))
		return mergeObserveResult{ForgeState: reason.forgeMergeState()}
	}
	if aerr := s.appendMergeObservation(ctx, runID, subject, *obs); aerr != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelError,
			"merge: observe rung: forge confirms the merge but the observation row could not be persisted",
			slog.String("run_id", runID.String()), slog.String("error", aerr.Error()))
		return mergeObserveResult{Landed: true, AppendErr: aerr, ForgeState: reason.forgeMergeState()}
	}
	return mergeObserveResult{Landed: true, Recorded: true, ForgeState: reason.forgeMergeState()}
}

// writeAlreadyMergedResponse answers the already-merged path with 200,
// already_merged:true and merge_queued:FALSE (E45.87 / #3622).
//
// Before answering it best-effort SETTLES: advanceRunAfterReviewResolve is the
// same helper reconcile-merge uses — it no-ops on a nil orchestrator and on a
// stage set that is not all-terminal, and never unwinds — then the run is
// re-read so run_state reports what actually happened. reconcile-merge's
// stage-supersede sweep is deliberately NOT folded in: merge_observation.go's
// contract splits OBSERVE from SETTLE, and this rung is the observe half.
//
// A still-non-terminal run names POST /v0/runs/{run_id}/reconcile-merge; a
// failed observation append names POST /v0/runs/{run_id}/record-merge-observation
// (binding approval condition 1).
func (s *Server) writeAlreadyMergedResponse(w http.ResponseWriter, r *http.Request, runID uuid.UUID,
	prURL string, verdictSequence int64, alreadyRecorded bool, obs mergeObserveResult) {
	s.advanceRunAfterReviewResolve(r.Context(), runID)

	runState := ""
	if fresh, ferr := s.cfg.RunRepo.GetRun(r.Context(), runID); ferr == nil && fresh != nil {
		runState = string(fresh.State)
	}

	msg := "the pull request is already merged, so no merge was queued."
	switch {
	case obs.AppendErr != nil:
		// STATIC LITERAL, no cause interpolation: writeJSON (unlike writeError)
		// applies no redaction at all, so a storage-layer error string
		// interpolated here would reach the caller unfiltered. The raw cause is
		// already emitted by observeMergeAlreadyLanded's LevelError log record at
		// the append site, which stays the single operator-facing source.
		msg += " The forge confirms the merge, but the merge_observation_recorded row could NOT be persisted; " +
			"POST /v0/runs/{run_id}/record-merge-observation to record it."
	case obs.Recorded:
		msg += " This call recorded the merge observation on the run's audit chain."
	default:
		msg += " The run's audit chain already carried merge evidence, so no new observation row was appended."
	}
	if runState != "" && !run.State(runState).IsTerminal() {
		msg += " The run is still " + runState +
			"; POST /v0/runs/{run_id}/reconcile-merge to settle it."
	}

	s.cfg.Logger.LogAttrs(r.Context(), slog.LevelInfo, "merge: pull request already merged; skipping dispatch",
		slog.String("run_id", runID.String()),
		slog.String("forge_merge_state", obs.ForgeState),
		slog.Bool("merge_observation_recorded", obs.Recorded),
		slog.Bool("merge_observation_append_failed", obs.AppendErr != nil),
		slog.String("run_state", runState))

	s.writeJSON(w, r, http.StatusOK, mergeRunResponse{
		RunID:                    runID.String(),
		MergeQueued:              false,
		VerdictSequence:          verdictSequence,
		AlreadyRecorded:          alreadyRecorded,
		PRURL:                    prURL,
		AlreadyMerged:            true,
		MergeObservationRecorded: obs.Recorded,
		RunState:                 runState,
		Message:                  msg,
	})
}

// CategoryMergeQueueEntered is the chained audit row the ADR-093 merge-queue
// enqueue arm appends when an operator's merge_run records merge intent on a
// queue entry (E87.4 / #4295). It carries the operator's verdict prose (no
// merge_verdict_recorded row is written on the queued path: nothing merged
// yet), which ADR-093 slice 2's merge-on-pass reads. INTERNAL: registered in
// audit/categories.go, not an issue-comment surface.
const CategoryMergeQueueEntered = "merge_queue_entered"

// mergeQueueEnqueueArm is the ADR-093 slice 1 enqueue arm of POST
// /v0/runs/{id}/merge (D2). It runs only when the queue is wired and the
// merge-candidate gate refused with merge_base_behind,
// merge_candidate_unverified (verify state unverified, or in_flight for a run
// that already has an active queue entry) or merge_candidate_held — never
// merge_candidate_verify_failed or merge_candidate_check_failed: enqueueing is
// never a way around a red or unreadable gate.
//
// It returns handled=true when it wrote the response. handled=false leaves
// the caller to render today's refusal; ineligible is then the eligibility
// reason codes (nil when the arm did not evaluate eligibility), rendered as
// the additive merge_queue_ineligible detail. An ineligible or undeterminable
// run writes NOTHING: no store row, no audit row.
//
// An eligible run is enqueued RECORD-BEFORE-COMMIT (approval condition C3):
// the merge_queue_entered row is appended FIRST, and only then is the entry
// created (Enqueue with MergeIntent) or its intent marked (MarkMergeIntent).
// A failed append answers 503 merge_queue_unrecorded with no entry and no
// merge_intent_at, and a retry appends again. The ordering, not the store's
// in-transaction RecordIntent callback, is the guarantee: the chained append
// locks the runs row FOR UPDATE on its own connection, while Enqueue's insert
// holds a FOR KEY SHARE lock on that row through its foreign key, so the
// callback would wait on its own transaction. The residual of the ordering:
// an append whose store write then fails leaves a merge_queue_entered row
// with no entry (503 merge_queue_write_failed; a retry records again).
//
// The intent decision (re-read the entry, append, write, read back) runs
// under the run's merge-intent lock (mergequeue.Store.WithMergeIntentLock,
// recordMergeQueueIntent), so overlapping merges of one run take turns: the
// second sees the first's committed intent and answers already_queued with
// the SAME intent_sequence, and one intent transition gets exactly one
// merge_queue_entered row. A lock that cannot be taken (a canceled request,
// a pool error) fails closed to today's refusal with nothing written; a lock
// that lapsed while the decision ran (ErrIntentLockLapsed) turns its 202
// into a retryable 503 merge_queue_write_failed.
//
// The base is pumped DETACHED (approval condition C6), on a bounded
// mergeCandidateAdmissionContext tracked by bgMergeQueuePump, so the 202 never
// waits on an admission's auto-advance or pass start; it answers from the
// store state read right after the write.
func (s *Server) mergeQueueEnqueueArm(w http.ResponseWriter, r *http.Request, runRow *run.Run,
	prURL, verdict, subject string, gate mergeCandidateGateResult) (bool, []string) {
	if s.cfg.MergeQueue == nil {
		return false, nil
	}
	switch gate.Code {
	case mergeCodeBaseBehind, mergeCodeCandidateHeld, mergeCodeCandidateUnverified:
	default:
		return false, nil
	}
	if gate.prHeadSHA == "" || gate.prBaseRef == "" {
		return false, nil
	}
	ctx := r.Context()
	runID := runRow.ID
	// A routing read only (the in-flight skip below): the intent decision
	// re-reads the entry under the run's merge-intent lock.
	_, found, err := s.cfg.MergeQueue.ActiveForRun(ctx, runID)
	if err != nil {
		// Fail closed: an unreadable queue never enqueues; today's refusal.
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"merge: merge-queue entry unreadable; not enqueueing (fail closed)",
			slog.String("run_id", runID.String()), slog.String("error", err.Error()))
		return false, nil
	}
	if gate.Code == mergeCodeCandidateUnverified && gate.Details["verify_state"] == mergeCandidateStateInFlight && !found {
		// An in-flight pass the queue does not own (e.g. fan_in): today's
		// refusal, which names the stage to await.
		return false, nil
	}

	elig := s.mergeQueueEligibility(ctx, runRow, gate.prHeadSHA)
	if !elig.Eligible {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo, "merge: run is not merge-queue eligible; refusing without enqueueing",
			slog.String("run_id", runID.String()), slog.String("gate_code", gate.Code),
			slog.Bool("undeterminable", elig.Undeterminable),
			slog.String("reasons", strings.Join(elig.codes(), ",")))
		return false, elig.codes()
	}

	if subject == "" {
		subject = "anonymous"
	}
	var (
		res          *mergeQueueIntent
		handled, ran bool
	)
	if lerr := s.cfg.MergeQueue.WithMergeIntentLock(ctx, runID, func() error {
		ran = true
		res, handled = s.recordMergeQueueIntent(w, r, runRow, prURL, verdict, subject, gate)
		return nil
	}); lerr != nil {
		if !ran {
			// Fail closed: no lock, no decision; nothing was written. Today's refusal.
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
				"merge: merge-intent lock unavailable; not enqueueing (fail closed)",
				slog.String("run_id", runID.String()), slog.String("error", lerr.Error()))
			return false, nil
		}
		// The decision ran but its exclusion may have lapsed mid-way
		// (mergequeue.ErrIntentLockLapsed): never a 202 a concurrent merge
		// may contradict. A decision that already wrote its own response
		// keeps it.
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"merge: merge-intent lock lapsed during the intent decision",
			slog.String("run_id", runID.String()), slog.String("error", lerr.Error()))
		if res != nil {
			s.writeError(w, r, http.StatusServiceUnavailable, "merge_queue_write_failed",
				"the merge-intent lock lapsed before the merge intent decision finished, so this answer cannot vouch for the queue state; read fishhawk_get_run_status, then re-invoke the merge (an entry that already carries the intent answers already_queued).",
				map[string]any{"run_id": runID.String(), "pr_url": prURL, "retryable": true})
			return true, nil
		}
	}
	if res == nil {
		return handled, nil
	}
	st, intentSeq, alreadyQueued := res.status, res.intentSeq, res.alreadyQueued
	s.pumpMergeCandidateQueueDetached(ctx, st.Entry.Base())

	resp := mergeRunResponse{
		RunID:           runID.String(),
		PRURL:           prURL,
		Queued:          true,
		QueueState:      string(st.Entry.State),
		QueuePosition:   st.Position,
		EntryID:         st.Entry.ID.String(),
		BaseRef:         st.Entry.BaseRef,
		AnchoredHeadSHA: st.Entry.AnchoredHeadSHA,
		IntentSequence:  intentSeq,
		AlreadyQueued:   alreadyQueued,
	}
	holder := "no run yet (the next queue event admits it)"
	if st.LiveRunID != nil {
		resp.LiveRunID = st.LiveRunID.String()
		holder = "run " + resp.LiveRunID
	}
	if st.Entry.State == mergequeue.StateLive {
		resp.Message = fmt.Sprintf("the run is merge-queue eligible and holds the %s line (ADR-093 D2, gate %s); its merge-candidate pass runs there. Watch it with fishhawk_get_merge_queue or fishhawk_get_run_status, then re-invoke the merge once the pass is green. Nothing merged yet; no merge verdict was recorded.",
			st.Entry.BaseRef, gate.Code)
	} else {
		resp.Message = fmt.Sprintf("the run is merge-queue eligible and was queued at position %d on %s behind %s (ADR-093 D2, gate %s). Admission is automatic: the queue advances the branch, re-anchors and starts its pass. Watch it with fishhawk_get_merge_queue or fishhawk_get_run_status, then re-invoke the merge once the pass is green. Nothing merged yet; no merge verdict was recorded.",
			st.Position, st.Entry.BaseRef, holder, gate.Code)
	}
	s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo, "merge: run enqueued in the merge queue",
		slog.String("run_id", runID.String()), slog.String("gate_code", gate.Code),
		slog.String("queue_state", resp.QueueState), slog.Int("queue_position", resp.QueuePosition),
		slog.Bool("already_queued", alreadyQueued))
	s.writeJSON(w, r, http.StatusAccepted, resp)
	return true, nil
}

// mergeQueueIntent is recordMergeQueueIntent's verdict: the entry read back
// after the write, the merge_queue_entered row's chain sequence, and whether
// the entry already carried the intent (nothing appended or written).
type mergeQueueIntent struct {
	status        mergequeue.Status
	intentSeq     int64
	alreadyQueued bool
}

// recordMergeQueueIntent is the intent decision of mergeQueueEnqueueArm: it
// runs ONLY under the run's merge-intent lock (Store.WithMergeIntentLock), so
// two overlapping merges of one run take it in turn and the second sees the
// first's committed intent — exactly one merge_queue_entered row per intent
// transition, and both answers name it. It reads the entry itself: a read
// taken before the lock may predate a concurrent intent.
//
// A non-nil result is the enqueued state. A nil result with handled=true
// means an error response was written; with handled=false the caller renders
// today's refusal (the entry was unreadable; nothing was written).
func (s *Server) recordMergeQueueIntent(w http.ResponseWriter, r *http.Request, runRow *run.Run,
	prURL, verdict, subject string, gate mergeCandidateGateResult) (*mergeQueueIntent, bool) {
	ctx := r.Context()
	runID := runRow.ID
	prior, found, err := s.cfg.MergeQueue.ActiveForRun(ctx, runID)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"merge: merge-queue entry unreadable under the intent lock; not enqueueing (fail closed)",
			slog.String("run_id", runID.String()), slog.String("error", err.Error()))
		return nil, false
	}
	sameBase := found && prior.Entry.BaseRef == gate.prBaseRef
	alreadyQueued := sameBase && prior.Entry.MergeIntentAt != nil
	var intentSeq int64
	if alreadyQueued {
		if rows, lerr := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, CategoryMergeQueueEntered); lerr == nil {
			intentSeq = mergeIntentRowSequence(rows, prior.Entry.BaseRef)
		}
	} else {
		fields := map[string]any{
			"run_id":            runID.String(),
			"repo":              runRow.Repo,
			"base_ref":          gate.prBaseRef,
			"anchored_head_sha": gate.prHeadSHA,
			"gate_code":         gate.Code,
			"verdict":           verdict,
			"pr_url":            prURL,
			"delegated":         false,
		}
		if sameBase {
			fields["entry_id"] = prior.Entry.ID.String()
			fields["queue_state"] = string(prior.Entry.State)
			fields["queue_position"] = prior.Position
		}
		payload, _ := json.Marshal(fields)
		actorKind := audit.ActorUser
		entry, aerr := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
			RunID:        runID,
			Timestamp:    time.Now().UTC(),
			Category:     CategoryMergeQueueEntered,
			ActorKind:    &actorKind,
			ActorSubject: &subject,
			Payload:      payload,
		})
		if aerr != nil {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "merge: append merge_queue_entered failed; not enqueueing",
				slog.String("run_id", runID.String()), slog.String("error", aerr.Error()))
			s.writeError(w, r, http.StatusServiceUnavailable, "merge_queue_unrecorded",
				"the merge intent could not be recorded on the audit chain, so the run was NOT enqueued (record before commit). Nothing was written to the queue; re-invoke the merge to retry.",
				map[string]any{"run_id": runID.String(), "pr_url": prURL, "retryable": true})
			return nil, true
		}
		intentSeq = entry.Sequence

		var werr error
		if sameBase {
			_, werr = s.cfg.MergeQueue.MarkMergeIntent(ctx, prior.Entry.ID, nil)
		} else {
			var res mergequeue.EnqueueResult
			res, werr = s.cfg.MergeQueue.Enqueue(ctx, mergequeue.EnqueueRequest{
				RunID: runID, BaseRef: gate.prBaseRef, HeadSHA: gate.prHeadSHA, MergeIntent: true,
			})
			if werr == nil {
				if res.Dropped != nil {
					s.writeMergeCandidateQueueAudit(ctx, CategoryMergeCandidateQueueSettled, *res.Dropped, nil)
					if res.Dropped.State == mergequeue.StateLive {
						s.pumpMergeCandidateQueueDetached(ctx, res.Dropped.Base())
					}
				}
				switch res.Outcome {
				case mergequeue.EnqueueInserted, mergequeue.EnqueueBaseChanged:
					s.writeMergeCandidateQueueAudit(ctx, CategoryMergeCandidateQueueHeld, res.Entry, nil)
				case mergequeue.EnqueueReanchored:
					if res.AnchorChanged {
						s.writeMergeCandidateQueueAudit(ctx, CategoryMergeCandidateQueueReanchored, res.Entry, nil)
					}
				}
			}
		}
		if werr != nil {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "merge: merge-queue write failed after merge_queue_entered was recorded",
				slog.String("run_id", runID.String()), slog.String("error", werr.Error()))
			s.writeError(w, r, http.StatusServiceUnavailable, "merge_queue_write_failed",
				"the merge intent was recorded but the merge queue could not be written, so the run is NOT queued; re-invoke the merge to retry.",
				map[string]any{"run_id": runID.String(), "pr_url": prURL, "retryable": true})
			return nil, true
		}
	}

	st, found, err := s.cfg.MergeQueue.ActiveForRun(ctx, runID)
	if err != nil || !found {
		why := "the entry left the queue"
		if err != nil {
			why = err.Error()
		}
		s.writeError(w, r, http.StatusServiceUnavailable, "merge_queue_write_failed",
			"the merge queue entry could not be read back ("+why+"); read fishhawk_get_run_status, then re-invoke the merge to retry.",
			map[string]any{"run_id": runID.String(), "pr_url": prURL, "retryable": true})
		return nil, true
	}
	return &mergeQueueIntent{status: st, intentSeq: intentSeq, alreadyQueued: alreadyQueued}, true
}

// mergeIntentRowSequence is the chain sequence of the merge_queue_entered row
// that recorded an active entry's intent on baseRef: the NEWEST row naming
// that base (0 when none). Rows are appended only under the run's
// merge-intent lock and only while the entry carries no intent, so a row
// orphaned by a failed queue write is always older than the row that
// recorded the intent; an orphan of a failed base change names another base
// and is skipped.
func mergeIntentRowSequence(rows []*audit.Entry, baseRef string) int64 {
	for i := len(rows) - 1; i >= 0; i-- {
		var p struct {
			BaseRef string `json:"base_ref"`
		}
		if json.Unmarshal(rows[i].Payload, &p) == nil && p.BaseRef == baseRef {
			return rows[i].Sequence
		}
	}
	return 0
}
