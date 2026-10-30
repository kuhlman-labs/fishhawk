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
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/delegation"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/mergequeue"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pglock"
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
//
// allow_overlap overrides a refusing merge overlap guard (E83.61 / #4119) and
// requires a non-empty overlap_reason of at most mergeOverlapReasonMaxBytes;
// the reason is recorded verbatim on the merge_overlap_overridden row.
type mergeRunRequest struct {
	Verdict       string `json:"verdict"`
	AllowOverlap  bool   `json:"allow_overlap,omitempty"`
	OverlapReason string `json:"overlap_reason,omitempty"`
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
//   - the merge overlap guard (E83.61 / #4119, merge_overlap_guard.go), AFTER
//     the conflict guard and BEFORE the merge-candidate gate — so the ADR-093
//     enqueue arm (#4295) can never enqueue an overlap-blocked run: 409
//     overlapping_implement_in_flight when another live run's implement-type
//     stage (dispatched / running) has an effective scope intersecting this
//     PR's changed paths, 409 overlap_undetermined and 502
//     overlap_check_failed when that cannot be decided once a live implement
//     exists elsewhere (FAIL-CLOSED). allow_overlap with a non-empty
//     overlap_reason (400 when empty or over 2000 bytes) admits the merge and
//     records a merge_overlap_overridden row RECORD-BEFORE-COMMIT — before
//     the merge-intent append on the enqueue path and before the verdict
//     append otherwise; a failed record is 503 overlap_override_unrecorded
//     with nothing merged, enqueued or newly recorded. The 409 body names
//     other runs only to a caller holding read:audit (#4582); otherwise it
//     carries counts and details_withheld. The delegated may_merge arm runs
//     the same guard (mergeOverlapAutoDriveHold) with no override.
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
//     202 queued answer instead (ADR-093 slice 1, mergeQueueEnqueueArm). With
//     a head-pinned merger bound, a run whose ONLY ineligibility is the
//     approval leg (not_approved / changes_requested) is also a 202, queued
//     YIELDED (ADR-112 R3, #4632: held with its intent and a marker the queue
//     skips until a re-probe clears it). Any other ineligible or an
//     undeterminable run keeps the 409 with an additive
//     merge_queue_ineligible detail and nothing written.
//   - 409 forge_approval_required (ADR-112 / #4619), INSIDE the merge-intent
//     lock after the ownership check and BEFORE the overlap-override record,
//     the verdict append, the observe, the republish and the claim: when the
//     run's review stage declares an approval gate, the ONE forge-approval
//     fold (forge_approval.go) runs at the gate's verified head
//     (gate.HeadSHA) and anything but approved — a moved head (head_moved),
//     a snapshot read error, an unparseable recorded spec, a non-GitHub forge
//     (provider_unsupported) — refuses with NOTHING recorded. An approved
//     fold writes merge_queue_authorized first; a failed append is 503
//     merge_authorization_unrecorded (retryable), again with nothing else
//     recorded. A run that never recorded a spec, or whose review stage
//     declares no approval gate, makes no fold read. The pre-ADR-112 409
//     approval_dismissed refusal is removed from this endpoint (the gate
//     view keeps it as an advisory display).
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
// A dispatch PINNED to the gate's admitted head (gate.HeadSHA, #4641) that the
// forge refuses for a head mismatch (forge.ErrMergeConflict) returns 409
// merge_head_moved naming the pinned head and its refusal_basis (precheck,
// rest_sha, graphql_reread; only the first two are forge guarantees that
// nothing applied): on precheck / rest_sha (and a bare REST 409 naming no
// basis) the claim is released and a re-invoke re-gates at the new head; on
// graphql_reread, or a head mismatch with an empty or unrecognised basis, the
// claim is left dispatch_failed (#4749) and a re-invoke answers
// merge_already_dispatched until reconciliation or the operator re-arm. An
// unpinned ErrMergeConflict keeps the 502.
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
	// Overlap override contract (E83.61 / #4119): validated before any read.
	// The trim only rejects blank input; the recorded value is the original
	// string (approval condition C3).
	if len(reqBody.OverlapReason) > mergeOverlapReasonMaxBytes {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			fmt.Sprintf("overlap_reason must be at most %d bytes", mergeOverlapReasonMaxBytes),
			map[string]any{"field": "overlap_reason", "max_bytes": mergeOverlapReasonMaxBytes})
		return
	}
	if reqBody.AllowOverlap && strings.TrimSpace(reqBody.OverlapReason) == "" {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"allow_overlap requires a non-empty overlap_reason: overriding the merge overlap guard is an audited operator judgement; state why the overlap is safe",
			map[string]any{"field": "overlap_reason"})
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

	// Merge overlap guard (E83.61 / #4119): another live run's implement
	// stage writing files this PR changes refuses the merge. AFTER the #3109
	// conflict guard and BEFORE the merge-candidate gate, so the ADR-093
	// enqueue arm below can never enqueue an overlap-blocked run, and before
	// any verdict append. FAILS CLOSED; see mergeOverlapGuard. An override
	// (allow_overlap + overlap_reason) proceeds and is recorded by
	// recordOverlapOverride at the first commit point reached. The 409 names
	// other runs' ids, stages and paths only to a caller that may read
	// cross-run overlap (#4582: read:audit, never a run-bound subject);
	// write:approvals alone gets counts (mergeOverlapResult.withheld). The
	// override path keeps the unredacted result for its audited row.
	overlap := s.mergeOverlapGuard(r.Context(), runRow)
	if overlap.refused() && !reqBody.AllowOverlap {
		shown := overlap
		if !overlapDetailsVisible(id) {
			shown = overlap.withheld()
		}
		s.writeMergeOverlapRefusal(w, r, runID, prURL, shown)
		return
	}
	subject := id.Subject
	if subject == "" {
		subject = "anonymous"
	}
	// recordOverlapOverride is the override's record-before-commit step: a
	// no-op when the guard admitted, otherwise the merge_overlap_overridden
	// append (deduped), with a failure rendered as 503 and false returned so
	// the caller commits nothing.
	recordOverlapOverride := func() bool {
		if !overlap.refused() {
			return true
		}
		if err := s.recordMergeOverlapOverride(r.Context(), runID, prURL, subject, overlap, reqBody.OverlapReason); err != nil {
			s.writeMergeOverlapOverrideUnrecorded(w, r, runID, prURL, err)
			return false
		}
		return true
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
		// as behind / unverified / held is ENQUEUED (202 queued) instead, and
		// one failing ONLY the approval leg is enqueued YIELDED (ADR-112 R3);
		// any other ineligible one keeps today's refusal, plus the reason
		// codes, with nothing written. An overlap override is recorded before the
		// merge-intent append (recordOverlapOverride as the pre-commit step).
		handled, ineligible := s.mergeQueueEnqueueArm(w, r, runRow, prURL, verdict, id.Subject, gate, recordOverlapOverride)
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

	// ADR-093 slice 2 (E87.5 / #4296, approval conditions D1/D2): the rest
	// of the inline merge (override record, verdict append, observe,
	// republish, dispatch claim, dispatch, response) runs under the run's
	// merge-intent lock, the SAME lock merge-on-pass and the delegated
	// may_merge arm take, so the three dispatch paths take turns. Its FIRST
	// step is the ownership check: while the run has an active queue entry
	// carrying merge intent (live or held, any phase) the queue owns the
	// merge and this path refuses 409 merge_queue_owns_merge with nothing
	// recorded, unless the PR already merged (then the normal path answers
	// already_merged). A queue read error is 503 merge_queue_unreadable. The
	// lock-acquire wait is bounded (approval condition (g)): a lock that was
	// never taken, by the bound or any other cause, answers 503
	// merge_lock_unavailable with nothing recorded. A lapse after fn ran is a
	// WARN only (fn already answered). r carries the bounded acquire context;
	// fn shadows it with the lock fn's context, so any work it spawns detached
	// starts from pglock.Detach.
	acquireCtx, inlineStarted, stopAcquire := boundMergeLockAcquire(r.Context(), mergeRunLockAcquireTimeout)
	defer stopAcquire()
	r = r.WithContext(acquireCtx)
	var ran bool
	inline := func(fctx context.Context) error {
		if !inlineStarted() {
			return errMergeLockAcquireTimedOut
		}
		ran = true
		r := r.WithContext(fctx)
		owned, oerr := s.mergeQueueOwnsMerge(r.Context(), runID)
		if oerr != nil {
			s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn, "merge: merge-queue ownership unreadable; refusing (fail closed)",
				slog.String("run_id", runID.String()), slog.String("error", oerr.Error()))
			s.writeError(w, r, http.StatusServiceUnavailable, "merge_queue_unreadable",
				"the merge queue could not be read, so whether it owns this run's merge is unknown; nothing was recorded and nothing merged. Re-invoke the merge to retry.",
				map[string]any{"run_id": runID.String(), "pr_url": prURL, "retryable": true})
			return nil
		}
		if owned && !s.observeMergeAlreadyLanded(r.Context(), runID, runRow, subject, true).Landed {
			s.writeMergeQueueOwnsMerge(w, r, runID, prURL)
			// ADR-112 R3 (#4632): a merge_run re-invoke on a YIELDED entry
			// re-probes it now (a no-op on an entry that is not yielded),
			// detached from this lock's fn context; the operator's explicit
			// invocation forces one re-attempt of a forge_review_required
			// yield (mergeQueueYieldClears).
			s.reprobeMergeQueueYieldDetached(r.Context(), runID, true)
			return nil
		}

		// ADR-112 (#4619): the forge-approval fold, AFTER the ownership check
		// and BEFORE every write below (override, verdict, observe,
		// republish, claim), so a refusal records nothing.
		if s.inlineForgeApproval(w, r, runID, runRow, prURL, subject, gate) {
			return nil
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
		//
		// An overlap override (E83.61 / #4119) is recorded FIRST, record-before-
		// commit: a failed record answers 503 overlap_override_unrecorded with no
		// verdict appended and no merge dispatched. On a FIRST POST its row
		// therefore precedes the verdict row; on a resume the verdict row already
		// exists and only a changed judgement appends a new override row.
		if !recordOverlapOverride() {
			return nil
		}
		existing, err := s.cfg.AuditRepo.ListForRunByCategory(r.Context(), runID, CategoryMergeVerdictRecorded)
		if err != nil {
			s.writeError(w, r, http.StatusInternalServerError, "internal_error",
				"read prior merge verdict failed", map[string]any{"error": err.Error()})
			return nil
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
					return nil
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
					return nil
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
				return nil
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
			return nil
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

		// Approval condition D1: claim the run's ONE merge dispatch on the
		// ledger, under the merge-intent lock and BEFORE the forge call. An
		// existing claim (the queue's, the delegated arm's or an earlier inline
		// dispatch, dispatched or dispatch_failed) means the forge is NEVER
		// called: already_merged when the PR landed, else 409
		// merge_already_dispatched. Without a wired queue there is no ledger
		// and no queue dispatcher to race, so the dispatch proceeds unclaimed.
		claim, claimed, handled := s.claimInlineMergeDispatch(w, r, runID, runRow, prURL, subject, verdictSequence, alreadyRecorded, gate)
		if handled {
			return nil
		}

		// #4641 (ADR-112 residual H3): the dispatch is PINNED to gate.HeadSHA,
		// the SAME head the merge-candidate gate admitted, inlineForgeApproval
		// folded at and claimInlineMergeDispatch records, so the fold, the
		// claim, merge_queue_authorized and the forge pin are one value. An
		// empty gate head (ungated D7 admit, merged PR) keeps the unpinned
		// ladder.
		if merr := s.cfg.GateMerger.MergePullRequest(r.Context(), runRow, gate.HeadSHA); merr != nil {
			checksPending := errors.Is(merr, forge.ErrPullRequestUnstableStatus)
			// RE-OBSERVE ONCE (E45.87 / #3622), BEFORE the ledger is settled
			// (matching merge-on-pass): a merge that landed in the
			// observe-to-dispatch window, or one the dispatch itself rejected
			// BECAUSE the PR was already merged, resolves as already_merged and
			// leaves the claim 'dispatched' (the merge landed), instead of a 502
			// that tells the operator to retry an operation that already
			// succeeded. The forge read is unconditional here (not gated on
			// alreadyRecorded): the dispatch has already failed, so the extra read
			// is paid only on an error path. Our own checks_pending refusal
			// dispatched nothing, so it is not re-observed.
			var obs mergeObserveResult
			if !checksPending {
				obs = s.observeMergeAlreadyLanded(r.Context(), runID, runRow, subject, true)
				if obs.Landed {
					s.writeAlreadyMergedResponse(w, r, runID, prURL, verdictSequence, alreadyRecorded, obs)
					return nil
				}
			}
			rearm := ""
			settled := mergeDispatchLedgerOutcome("")
			// Ruling R1: a checks_pending refusal or a forge 405/409/422
			// RELEASES the claim (nothing was applied), so the re-invoke can
			// claim again; any other outcome, a graphql_reread head mismatch
			// included (#4749), is uncertain: dispatch_failed.
			if claimed {
				settled = s.settleMergeDispatchClaim(r.Context(), mergeQueueDispatchLedger{s.cfg.MergeQueue}, claim, merr)
				if settled == mergeDispatchUncertain {
					rearm = mergeDispatchRearmNote
				}
			}
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
			if checksPending {
				s.cfg.Logger.LogAttrs(r.Context(), slog.LevelInfo, "merge: checks not all passed (unstable status)",
					slog.String("run_id", runID.String()), slog.String("error", merr.Error()))
				details := map[string]any{"run_id": runID.String(), "verdict_sequence": verdictSequence, "pr_url": prURL, "reason": "checks_pending"}
				msg := "the merge verdict is recorded and durable, but the squash merge cannot be queued because the pull request's required checks have not all passed (GitHub reports the pull request in unstable status). An immediate retry cannot succeed. If the checks are still pending, re-invoke once they complete; if a required check has already FAILED, the merge will never queue — inspect the pull request instead of waiting." + rearm
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
				return nil
			}
			// #4641: a PINNED dispatch the forge refused for a head mismatch is
			// 409 merge_head_moved, not the generic 502: the PR head moved after
			// the merge-candidate gate admitted gate.HeadSHA, and a re-invoke
			// re-runs the gate and the fold at the new head. An UNPINNED
			// ErrMergeConflict (empty gate head) keeps the 502.
			if gate.HeadSHA != "" && errors.Is(merr, forge.ErrMergeConflict) {
				s.writeMergeHeadMoved(w, r, runRow, prURL, verdictSequence, gate.HeadSHA, merr, claimed, settled, rearm)
				return nil
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
				"the merge verdict is recorded and durable, but queuing the squash merge failed; retry the merge. The pull request was re-read from the forge and did NOT confirm a merge (forge_merge_state="+obs.ForgeState+"). If the pull request has in fact already merged, POST /v0/runs/{run_id}/record-merge-observation to record the merge observation, then POST /v0/runs/{run_id}/reconcile-merge to settle the run."+rearm,
				map[string]any{
					"run_id":            runID.String(),
					"error":             merr.Error(),
					"verdict_sequence":  verdictSequence,
					"forge_merge_state": obs.ForgeState,
				})
			return nil
		}

		s.writeJSON(w, r, http.StatusOK, mergeRunResponse{
			RunID:           runID.String(),
			MergeQueued:     true,
			VerdictSequence: verdictSequence,
			AlreadyRecorded: alreadyRecorded,
			PRURL:           prURL,
		})
		return nil
	}
	lockErr := s.withRunMergeLock(r.Context(), runID, inline)
	if lockErr != nil {
		if !ran {
			s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn, "merge: merge-intent lock unavailable; nothing recorded (fail closed)",
				slog.String("run_id", runID.String()), slog.String("error", lockErr.Error()))
			s.writeError(w, r, http.StatusServiceUnavailable, "merge_lock_unavailable",
				"the run's merge-intent lock could not be taken (another merge of this run holds it, or the wait was bounded out); nothing was recorded and nothing merged. Re-invoke the merge to retry.",
				map[string]any{"run_id": runID.String(), "pr_url": prURL, "retryable": true})
			return
		}
		s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn, "merge: merge-intent lock lapsed after the inline merge ran; its answer stands",
			slog.String("run_id", runID.String()), slog.String("error", lockErr.Error()))
	}
}

// forgeApprovalReasonSnapshotUnreadable is the inline/delegated fold reason
// for a review snapshot transport failure (the queue path holds instead).
const forgeApprovalReasonSnapshotUnreadable = "snapshot_unreadable"

// inlineForgeApproval is the inline merge_run's ADR-112 step, run inside the
// merge-intent lock: handled=true means the response was written (409
// forge_approval_required or 503 merge_authorization_unrecorded) and nothing
// was recorded. A run whose review stage declares no approval gate (or that
// never recorded a spec) makes no fold read. The fold judges gate.HeadSHA, the
// head the merge-candidate gate verified (H1); an EMPTY gate head on a merged
// PR (the gate's merged-PR admit) skips the fold so the observe path answers
// already_merged, and otherwise folds undetermined (head_unknown).
func (s *Server) inlineForgeApproval(w http.ResponseWriter, r *http.Request, runID uuid.UUID, runRow *run.Run,
	prURL, subject string, gate mergeCandidateGateResult) bool {
	ctx := r.Context()
	declared, _, gerr := forgeApprovalGateFor(runRow)
	if gerr == nil && !declared {
		return false
	}
	if gate.HeadSHA == "" && gerr == nil && runForge(runRow) == forgeApprovalProviderGitHub &&
		s.observeMergeAlreadyLanded(ctx, runID, runRow, subject, true).Landed {
		return false
	}
	d, ferr := s.forgeApprovalFold(ctx, runRow, gate.HeadSHA)
	if ferr != nil {
		d = d.undetermined(forgeApprovalReasonSnapshotUnreadable, "the forge review snapshot could not be read", ferr)
	}
	if d.Merged {
		return false
	}
	if !d.approved() {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo, "merge: forge approval did not qualify; refusing with nothing recorded",
			slog.String("run_id", runID.String()), slog.String("fold", d.Result), slog.String("reason", d.Reason),
			slog.String("detail", d.Detail))
		s.writeMergeReadinessRefusal(w, r, runID, prURL, forgeApprovalRefusal(d, runRow))
		return true
	}
	if !d.Declared {
		return false
	}
	if _, aerr := s.writeMergeQueueAuthorized(ctx, runRow, d, forgeApprovalAuthorization{Path: mergequeue.DispatchPathInline,
		ActorKind: audit.ActorUser, ActorSubject: subject}); aerr != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "merge: merge_queue_authorized append failed; refusing with nothing recorded",
			slog.String("run_id", runID.String()), slog.String("error", aerr.Error()))
		s.writeError(w, r, http.StatusServiceUnavailable, mergeCodeAuthorizationUnrecorded,
			"the forge approval qualified but its merge_queue_authorized evidence row could not be recorded, so no merge verdict was recorded and nothing merged (ADR-112 R4: evidence before dispatch). Re-invoke the merge to retry.",
			map[string]any{"run_id": runID.String(), "pr_url": prURL, "retryable": true})
		return true
	}
	return false
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
	// mergeCodeHeadMoved (#4641, ADR-112 residual H3): the forge refused the
	// merge pinned to the head the merge-candidate gate admitted because the
	// pull request's head moved after it. A re-invoke re-gates at the new head.
	mergeCodeHeadMoved = "merge_head_moved"
)

// mergeHeadMovedBasisUnknown is the refusal_basis of a head-mismatch refusal
// that carried no *githubclient.HeadMismatchError (a leaf that does not name
// how the mismatch was established).
const mergeHeadMovedBasisUnknown = "unknown"

// mergeHeadMovedBasis names how a head-pinned merge's head mismatch was
// established (githubclient.HeadMismatch*), or mergeHeadMovedBasisUnknown.
func mergeHeadMovedBasis(err error) string {
	var hm *githubclient.HeadMismatchError
	if errors.As(err, &hm) && hm.Basis != "" {
		return hm.Basis
	}
	return mergeHeadMovedBasisUnknown
}

// mergeHeadMovedNothingApplied reports whether basis is one the forge
// GUARANTEES applied nothing (ruling C3): the client pre-check sent no
// mutation, and a REST 409 is GitHub's documented sha-mismatch refusal. The
// graphql_reread basis is INFERRED from a fresh read after a GraphQL error, so
// an auto-merge may in fact be armed at the pinned head; an unknown basis is
// treated the same way. Only these two bases RELEASE the D1 claim on a head
// mismatch: graphql_reread and an empty or unrecognised basis leave it
// dispatch_failed (mergeDispatchHeadMismatchUncertain, #4749).
func mergeHeadMovedNothingApplied(basis string) bool {
	return basis == githubclient.HeadMismatchPrecheck || basis == githubclient.HeadMismatchRESTSha
}

// forgeApprovalFolded reports whether the run's review stage declares an
// ADR-112 approval gate, i.e. whether a dispatch reaching the forge carried an
// APPROVED fold at the pinned head. A gate-less run has no evaluated approval,
// so head-moved wording names only the head the merge-candidate gate admitted
// (ruling C2).
func forgeApprovalFolded(runRow *run.Run) bool {
	declared, _, err := forgeApprovalGateFor(runRow)
	return err == nil && declared
}

// mergeHeadMovedSubject is the head-moved wording shared by the inline 409 and
// the delegated INFO log: the head the merge-candidate gate admitted, plus the
// approval clause ONLY when a declared gate folded (ruling C2).
func mergeHeadMovedSubject(head string, folded bool) string {
	subject := "the head the merge-candidate gate admitted (" + head + ")"
	if folded {
		subject += ", the head the approval was evaluated at"
	}
	return subject
}

// writeMergeHeadMoved writes the inline 409 merge_head_moved (#4641). settled
// is the D1 claim's settle outcome when claimed, the authority for the claim
// state the message reports; rearm is mergeDispatchRearmNote when the claim
// was left dispatch_failed. The uncertain-basis sentence names the basis ONLY
// when the classification itself was uncertain (#4749); a releasable refusal
// whose release did not land is reported as exactly that.
func (s *Server) writeMergeHeadMoved(w http.ResponseWriter, r *http.Request, runRow *run.Run, prURL string,
	verdictSequence int64, head string, merr error, claimed bool, settled mergeDispatchLedgerOutcome, rearm string) {
	basis, folded := mergeHeadMovedBasis(merr), forgeApprovalFolded(runRow)
	s.cfg.Logger.LogAttrs(r.Context(), slog.LevelInfo, "merge: the forge refused the head-pinned merge; the pull request head moved",
		slog.String("run_id", runRow.ID.String()), slog.String("head_sha", head),
		slog.String("refusal_basis", basis), slog.String("error", merr.Error()))
	msg := "the merge verdict is recorded and durable, but the forge refused the merge pinned to " + mergeHeadMovedSubject(head, folded) +
		" because the pull request's head moved after it."
	if mergeHeadMovedNothingApplied(basis) {
		msg += " Nothing merged: the forge applied nothing (refusal_basis " + basis + ")."
	} else {
		msg += " This refusal_basis (" + basis + ") is not a forge guarantee that nothing was applied"
		if basis == githubclient.HeadMismatchGraphQLReread {
			msg += ": the auto-merge enable answered an error and a fresh read showed a different head, so the mismatch is INFERRED"
		}
		msg += "; if the enable had in fact applied, auto-merge may be armed at the pinned head, so inspect the pull request."
	}
	held := claimed && settled == mergeDispatchUncertain
	switch {
	case claimed && settled == mergeDispatchRelease:
		msg += " The run's merge-dispatch claim was released."
	case held && classifyMergeDispatchOutcome(merr) == mergeDispatchUncertain:
		msg += " The run's merge-dispatch claim was NOT released because refusal_basis " + basis +
			" does not prove the forge applied nothing, so it is dispatch_failed."
	case held:
		msg += " The run's merge-dispatch claim release did not land, so it is dispatch_failed."
	case !claimed:
		msg += " No merge-dispatch claim was held."
	}
	if held {
		msg += " Once the operator re-arms the claim (or reconciliation clears it), re-invoke"
	} else {
		msg += " Re-invoke"
	}
	msg += " the merge once the head settles: it re-runs the merge-candidate gate"
	if folded {
		msg += " and the forge-approval fold"
	}
	msg += " at the new head." + rearm
	s.writeError(w, r, http.StatusConflict, mergeCodeHeadMoved, msg,
		map[string]any{
			"run_id":           runRow.ID.String(),
			"pr_url":           prURL,
			"verdict_sequence": verdictSequence,
			"head_sha":         head,
			"refusal_basis":    basis,
			"next_step":        "fishhawk_merge_run",
		})
}

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
// run writes NOTHING: no store row, no audit row, EXCEPT (ADR-112 R3, #4632)
// a determinable result whose every reason is an approval-leg reason with the
// queue owning merges (mergeRunApprovalYield): that run is enqueued YIELDED
// through the same record-before-commit path (recordMergeQueueIntent). An
// already-queued YIELDED entry is re-probed on every pass through the arm, and
// a freshly yielded one once after the write (reprobeMergeQueueYieldDetached).
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
// into a retryable 503 merge_queue_write_failed, after pumping the base so an
// entry the decision just created on an idle base is still admitted. The
// decision runs on the lock's fn context (r.WithContext), so every DB call in
// it is bounded by the intent fn deadline even when the request carries none;
// work it spawns detached starts from pglock.Detach, never on that context.
// An already_queued entry whose merge_queue_entered row cannot be read back
// answers a retryable 503 merge_queue_write_failed, never a 202 naming
// intent_sequence 0; nothing is written on that path.
//
// The base is pumped DETACHED (approval condition C6), on a bounded
// mergeCandidateAdmissionContext tracked by bgMergeQueuePump, so the 202 never
// waits on an admission's auto-advance or pass start; it answers from the
// store state read right after the write.
//
// preCommit (nil = none) runs under the lock immediately BEFORE the
// merge_queue_entered append, only when an intent is about to be recorded:
// the handler passes its merge-overlap override record (E83.61 / #4119), so an
// overridden overlap is durable before any merge intent. A false return means
// preCommit wrote the response; nothing is appended or enqueued.
func (s *Server) mergeQueueEnqueueArm(w http.ResponseWriter, r *http.Request, runRow *run.Run,
	prURL, verdict, subject string, gate mergeCandidateGateResult, preCommit func() bool) (bool, []string) {
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
	routed, found, err := s.cfg.MergeQueue.ActiveForRun(ctx, runID)
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

	if found && routed.Entry.Yielded() {
		// ADR-112 R3 (#4632, approval condition C2(e)): a merge_run re-invoke
		// on an already-queued YIELDED entry re-probes it now, whatever this
		// call answers; the fold re-reads the forge and is the sole decision
		// (forcing one re-attempt of a forge_review_required yield).
		defer s.reprobeMergeQueueYieldDetached(ctx, runID, true)
	}

	elig := s.mergeQueueEligibility(ctx, runRow, gate.prHeadSHA)
	var yield *mergeQueueYield
	if !elig.Eligible {
		y, ok := s.mergeRunApprovalYield(elig)
		if !ok {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo, "merge: run is not merge-queue eligible; refusing without enqueueing",
				slog.String("run_id", runID.String()), slog.String("gate_code", gate.Code),
				slog.Bool("undeterminable", elig.Undeterminable),
				slog.String("reasons", strings.Join(elig.codes(), ",")))
			return false, elig.codes()
		}
		// ADR-112 R3 (#4632): ONLY the approval leg failed and the queue owns
		// merges, so the run is enqueued YIELDED (held with its intent and a
		// marker AdmitNext skips) instead of refused.
		yield = &y
	}

	if subject == "" {
		subject = "anonymous"
	}
	var (
		res          *mergeQueueIntent
		handled, ran bool
	)
	if lerr := s.cfg.MergeQueue.WithMergeIntentLock(ctx, runID, func(fctx context.Context) error {
		ran = true
		res, handled = s.recordMergeQueueIntent(w, r.WithContext(fctx), runRow, prURL, verdict, subject, gate, yield, preCommit)
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
			// The decision's write stands: pump the base before answering,
			// so an entry it just created on an idle base is admitted
			// without waiting for another queue event (the pump is
			// idempotent under the base lock).
			s.pumpMergeCandidateQueueDetached(ctx, res.status.Entry.Base())
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
	if res.marked && (!found || !routed.Entry.Yielded()) {
		// A freshly yielded entry is re-probed at once (an already-yielded
		// one was deferred above): an approval that landed since the
		// eligibility read clears it now rather than on the backoff schedule.
		s.reprobeMergeQueueYieldDetached(ctx, runID, true)
	}

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
	switch {
	case st.Entry.Yielded():
		next := mergeQueueYieldNextStep(st.Entry.YieldReason)
		resp.Message = fmt.Sprintf("the run was queued on %s with your merge intent but YIELDED its place (ADR-112 R3, yield_reason %s, gate %s): the forge approval does not qualify yet, so the queue skips the entry and the rest of the line keeps merging. Next step: %s. The queue re-probes the approval on its own schedule (and at once on an approval review or a merge re-invoke); once it qualifies the yield clears and the entry rejoins the line at its original place, then merges pinned to the verified head (ADR-093 D3). Watch it with fishhawk_get_merge_queue or fishhawk_get_run_status. Nothing merged yet.",
			st.Entry.BaseRef, st.Entry.YieldReason, gate.Code, mergeRunYieldNextStepText(next))
	case s.headPinnedMerger() != nil && st.Entry.State == mergequeue.StateLive:
		resp.Message = fmt.Sprintf("the run is merge-queue eligible and holds the %s line (ADR-093 D2, gate %s); its merge-candidate pass runs there. Your merge intent persists until the entry settles: the queue merges the run pinned to the verified head on its green pass (ADR-093 D3), so you do not re-invoke the merge. Watch it with fishhawk_get_merge_queue or fishhawk_get_run_status. Nothing merged yet.",
			st.Entry.BaseRef, gate.Code)
	case s.headPinnedMerger() != nil:
		resp.Message = fmt.Sprintf("the run is merge-queue eligible and was queued at position %d on %s behind %s (ADR-093 D2, gate %s). Admission is automatic: the queue advances the branch, re-anchors and starts its pass. Your merge intent persists until the entry settles: the queue merges the run pinned to the verified head on its green pass (ADR-093 D3), so you do not re-invoke the merge. Watch it with fishhawk_get_merge_queue or fishhawk_get_run_status. Nothing merged yet.",
			st.Position, st.Entry.BaseRef, holder, gate.Code)
	case st.Entry.State == mergequeue.StateLive:
		resp.Message = fmt.Sprintf("the run is merge-queue eligible and holds the %s line (ADR-093 D2, gate %s); its merge-candidate pass runs there. Watch it with fishhawk_get_merge_queue or fishhawk_get_run_status, then re-invoke the merge once the pass is green. Nothing merged yet; no merge verdict was recorded.",
			st.Entry.BaseRef, gate.Code)
	default:
		resp.Message = fmt.Sprintf("the run is merge-queue eligible and was queued at position %d on %s behind %s (ADR-093 D2, gate %s). Admission is automatic: the queue advances the branch, re-anchors and starts its pass. Watch it with fishhawk_get_merge_queue or fishhawk_get_run_status, then re-invoke the merge once the pass is green. Nothing merged yet; no merge verdict was recorded.",
			st.Position, st.Entry.BaseRef, holder, gate.Code)
	}
	s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo, "merge: run enqueued in the merge queue",
		slog.String("run_id", runID.String()), slog.String("gate_code", gate.Code),
		slog.String("queue_state", resp.QueueState), slog.Int("queue_position", resp.QueuePosition),
		slog.Bool("already_queued", alreadyQueued), slog.String("yield_reason", st.Entry.YieldReason))
	s.writeJSON(w, r, http.StatusAccepted, resp)
	return true, nil
}

// mergeRunApprovalYield decides whether an INELIGIBLE queue-entry result
// enqueues the run YIELDED rather than refusing (ADR-112 R3, #4632). It does
// only when the queue owns merges (a head-pinned merger is bound), every read
// succeeded, and EVERY reason is an approval-leg reason: changes_requested
// yields forge_approval_vetoed (next step dismiss_review, R3 row 2) and
// not_approved yields forge_approval_absent (next step approve_pr, row 3). Any
// other reason, alone or beside an approval reason, keeps today's refusal
// with nothing written.
func (s *Server) mergeRunApprovalYield(elig mergeQueueEligibilityResult) (mergeQueueYield, bool) {
	if s.headPinnedMerger() == nil || elig.Undeterminable || len(elig.Reasons) == 0 {
		return mergeQueueYield{}, false
	}
	reason, row, detail := mergeCandidateQueueReasonForgeApprovalAbsent, mergeQueueYieldRowApprovalFold, ""
	for _, x := range elig.Reasons {
		switch x.Code {
		case mergeQueueReasonChangesRequested:
			reason, row, detail = mergeCandidateQueueReasonForgeApprovalVetoed, mergeQueueYieldRowVeto, x.Detail
		case mergeQueueReasonNotApproved:
			if row != mergeQueueYieldRowVeto {
				detail = x.Detail
			}
		default:
			return mergeQueueYield{}, false
		}
	}
	return newMergeQueueYield(reason, detail, row), true
}

// mergeRunYieldNextStepText renders a closed next_step for the 202 message.
// It is built ONLY from the closed vocabulary: the untrusted yield_detail is
// never interpolated.
func mergeRunYieldNextStepText(next string) string {
	switch next {
	case forgeApprovalNextApprovePR:
		return "approve_pr (a reviewer who qualifies under the review approval gate approves the pull request at its current head)"
	case forgeApprovalNextDismissReview:
		return "dismiss_review (the requested changes are addressed and the changes-requested review is dismissed or superseded by an approval)"
	case forgeApprovalNextRetryAfter:
		return "retry_after (the forge review read could not decide; the re-probe retries it)"
	case forgeApprovalNextConfigureIdentityProvider:
		return "configure_identity_provider (an operator configures the github identity provider the approval gate's members need)"
	}
	return "none (nothing at the forge clears this yield; read fishhawk_get_merge_queue)"
}

// reprobeMergeQueueYieldDetached runs the targeted, no-backoff yield re-probe
// for runID (ReprobeMergeQueueYield, force as there) on a bounded admission
// context tracked by bgMergeQueuePump, so the caller never waits on its forge
// read. It detaches from any merge-intent lock fn context first
// (pglock.Detach): the re-probe's clear pumps the base, which takes its own
// locks. A run with no yielded entry does nothing.
func (s *Server) reprobeMergeQueueYieldDetached(ctx context.Context, runID uuid.UUID, force bool) {
	if s.cfg.MergeQueue == nil {
		return
	}
	pctx, cancel := mergeCandidateAdmissionContext(pglock.Detach(ctx))
	s.bgMergeQueuePump.Add(1)
	go func() {
		defer s.bgMergeQueuePump.Done()
		defer cancel()
		s.ReprobeMergeQueueYield(pctx, runID, force)
	}()
}

// mergeQueueIntent is recordMergeQueueIntent's verdict: the entry read back
// after the write, the merge_queue_entered row's chain sequence, and whether
// the entry already carried the intent (nothing appended or written).
type mergeQueueIntent struct {
	status        mergequeue.Status
	intentSeq     int64
	alreadyQueued bool
	// marked reports THIS call stamped the yield marker (ADR-112 R3).
	marked bool
}

// recordMergeQueueIntent is the intent decision of mergeQueueEnqueueArm: it
// runs ONLY under the run's merge-intent lock (Store.WithMergeIntentLock), on
// the lock's fn context (r's context is bounded and pglock-marked), so
// two overlapping merges of one run take it in turn and the second sees the
// first's committed intent — exactly one merge_queue_entered row per intent
// transition, and both answers name it. It reads the entry itself: a read
// taken before the lock may predate a concurrent intent.
//
// A non-nil yield enqueues the run YIELDED (ADR-112 R3, #4632): with no
// entry, or one on another base, a new held row carries the intent and the
// marker; a same-base HELD entry is re-anchored and gains the intent and the
// marker in one Enqueue; a same-base LIVE entry only gains the intent
// (MarkMergeIntent: a live entry is never marked, and merge-on-pass judges
// its approval). merge_queue_entered records the decision first
// (record-before-commit, yielded + yield_reason), and merge_queue_yielded
// (site merge_run) follows a marker that landed. An entry already carrying
// the intent writes nothing, yielded or not.
//
// A non-nil result is the enqueued state. A nil result with handled=true
// means an error response was written; with handled=false the caller renders
// today's refusal (the entry was unreadable; nothing was written).
func (s *Server) recordMergeQueueIntent(w http.ResponseWriter, r *http.Request, runRow *run.Run,
	prURL, verdict, subject string, gate mergeCandidateGateResult, yield *mergeQueueYield, preCommit func() bool) (*mergeQueueIntent, bool) {
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
	var marked *mergequeue.Entry
	if alreadyQueued && prior.Entry.MergeIntentSequence > 0 {
		// The entry's STORED intent sequence (0108) names the row that
		// recorded the intent; a newer orphan row is never named.
		intentSeq = prior.Entry.MergeIntentSequence
	} else if alreadyQueued {
		// Legacy entry (intent recorded before 0108): the newest row naming
		// the base.
		rows, lerr := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, CategoryMergeQueueEntered)
		if lerr != nil {
			// Never a 202 naming intent_sequence 0: the answer must name the
			// row that recorded the intent. Nothing is written here.
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
				"merge: merge_queue_entered unreadable for an already-queued entry; not answering 202",
				slog.String("run_id", runID.String()), slog.String("error", lerr.Error()))
			s.writeError(w, r, http.StatusServiceUnavailable, "merge_queue_write_failed",
				"the run is already queued, but its merge_queue_entered row could not be read back, so this answer cannot name the intent_sequence; nothing was written. Re-invoke the merge to retry.",
				map[string]any{"run_id": runID.String(), "pr_url": prURL, "retryable": true})
			return nil, true
		}
		intentSeq = mergeIntentRowSequence(rows, prior.Entry.BaseRef)
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
		// A same-base LIVE entry is never marked (it holds the line, and
		// merge-on-pass judges its approval).
		if yield != nil && sameBase && prior.Entry.State == mergequeue.StateLive {
			yield = nil
		}
		if yield != nil {
			fields["yielded"] = true
			fields["yield_reason"] = yield.Reason
		}
		payload, _ := json.Marshal(fields)
		if preCommit != nil && !preCommit() {
			return nil, true
		}
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
		if sameBase && yield == nil {
			_, werr = s.cfg.MergeQueue.MarkMergeIntent(ctx, prior.Entry.ID, &mergequeue.MergeIntentRequest{IntentSequence: intentSeq})
		} else {
			req := mergequeue.EnqueueRequest{
				RunID: runID, BaseRef: gate.prBaseRef, HeadSHA: gate.prHeadSHA, MergeIntent: true,
				IntentSequence: intentSeq,
			}
			if yield != nil {
				m := yield.marker()
				if sameBase {
					// A same-reason re-yield of a cleared entry on the same
					// head and base continues its episode (ADR-112 R3).
					m.Continue = s.mergeQueueYieldContinuation(ctx, prior.Entry, yield.Reason)
				}
				req.Yield = &m
				if sameBase && prior.Entry.AnchoredHeadSHA == gate.prHeadSHA {
					// An unchanged head keeps the entry's recorded base anchor.
					req.BaseSHA = prior.Entry.AnchoredBaseSHA
				}
			}
			var res mergequeue.EnqueueResult
			res, werr = s.cfg.MergeQueue.Enqueue(ctx, req)
			if werr == nil && res.Marked {
				e := res.Entry
				marked = &e
			}
			if werr == nil {
				if res.Dropped != nil {
					s.writeMergeCandidateQueueAudit(ctx, CategoryMergeCandidateQueueSettled, *res.Dropped, nil)
					if res.Dropped.State == mergequeue.StateLive {
						// ctx is the lock's fn context: detach so the pump
						// is not refused as a nested pglock lock.
						s.pumpMergeCandidateQueueDetached(pglock.Detach(ctx), res.Dropped.Base())
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
		if marked != nil {
			// The marker has committed: a failed append is logged at ERROR
			// and healed by the re-probe sweep (approval condition C7).
			_ = s.writeMergeQueueYielded(ctx, *marked, *yield, mergeQueueYieldSiteMergeRun, "")
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
	return &mergeQueueIntent{status: st, intentSeq: intentSeq, alreadyQueued: alreadyQueued, marked: marked != nil}, true
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

// mergeRunLockAcquireTimeout bounds how long the inline merge_run waits to
// take the run's merge-intent lock (approval condition (g)); a wait past it
// answers 503 merge_lock_unavailable with nothing recorded. A var so a test
// can shorten it.
var mergeRunLockAcquireTimeout = 30 * time.Second

// errMergeLockAcquireTimedOut is the inline fn's refusal to start once the
// acquire bound fired first.
var errMergeLockAcquireTimedOut = errors.New("merge: merge-intent lock acquire wait exceeded its bound")

// boundMergeLockAcquire bounds ONLY the lock-acquire wait: ctx is canceled
// when the bound fires before fn starts, and never once fn started (the
// bound and the start race on one compare-and-swap, so exactly one wins).
// started reports, once, from inside fn, whether fn won; stop releases the
// timer and the context.
func boundMergeLockAcquire(parent context.Context, bound time.Duration) (ctx context.Context, started func() bool, stop func()) {
	ctx, cancel := context.WithCancel(parent)
	var state atomic.Bool
	timer := time.AfterFunc(bound, func() {
		if state.CompareAndSwap(false, true) {
			cancel()
		}
	})
	return ctx, func() bool { return state.CompareAndSwap(false, true) }, func() {
		timer.Stop()
		cancel()
	}
}

// writeMergeQueueOwnsMerge renders the D2 refusal: the queue owns the run's
// merge, so the inline path records nothing and dispatches nothing.
func (s *Server) writeMergeQueueOwnsMerge(w http.ResponseWriter, r *http.Request, runID uuid.UUID, prURL string) {
	s.writeError(w, r, http.StatusConflict, "merge_queue_owns_merge",
		"the merge queue owns this run's merge: its entry carries your merge intent, and the queue merges it pinned to the verified head on its green pass (ADR-093 D3). Nothing was recorded and nothing merged; do not re-invoke the merge. Watch it with fishhawk_get_merge_queue.",
		map[string]any{"run_id": runID.String(), "pr_url": prURL, "next_step": "fishhawk_get_merge_queue"})
}

// mergeDispatchRearmNote is appended to an inline dispatch failure whose
// outcome was UNCERTAIN after a D1 claim: the claim is dispatch_failed, so a
// re-invoke answers already_merged if the PR landed, else is refused until the
// operator re-arms it. A released claim (checks_pending, 405/409/422) gets no
// note: the re-invoke claims again.
const mergeDispatchRearmNote = " The forge outcome is uncertain, so the run's merge-dispatch claim is now dispatch_failed (ADR-093 approval condition D1): a re-invoke answers already_merged if the pull request merged, else merge_already_dispatched until the operator re-arms the claim (backend/internal/mergequeue/README.md § \"Merge-dispatch ledger\")."

// claimInlineMergeDispatch claims the run's ONE merge dispatch for the inline
// path (approval condition D1), under the merge-intent lock and before the
// forge, returning the claim for the post-dispatch release / dispatch_failed
// record. claimed=false with handled=false means no ledger is wired (no queue,
// so no queue dispatcher to race): dispatch unclaimed. handled=true means the
// response was written: already_merged when an existing claim's PR landed
// (reconciliation, ruling R1), 409 merge_already_dispatched otherwise, or 503
// merge_dispatch_unclaimed on a claim error. The forge is never called on a
// handled answer.
func (s *Server) claimInlineMergeDispatch(w http.ResponseWriter, r *http.Request, runID uuid.UUID, runRow *run.Run,
	prURL, subject string, verdictSequence int64, alreadyRecorded bool, gate mergeCandidateGateResult) (claim mergequeue.DispatchClaim, claimed, handled bool) {
	if s.cfg.MergeQueue == nil {
		return mergequeue.DispatchClaim{}, false, false
	}
	head := gate.HeadSHA
	if head == "" {
		head = gate.prHeadSHA
	}
	if head == "" {
		// The ungated D7 admit carries no head; the claim is run-scoped, so
		// the unresolved head is recorded as such.
		head = "unresolved"
	}
	existing, err := s.cfg.MergeQueue.ClaimMergeDispatch(r.Context(), runID, mergequeue.DispatchPathInline, head)
	if err == nil {
		return existing, true, false
	}
	if !errors.Is(err, mergequeue.ErrAlreadyDispatched) {
		s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn, "merge: merge dispatch could not be claimed; not dispatching",
			slog.String("run_id", runID.String()), slog.String("error", err.Error()))
		s.writeError(w, r, http.StatusServiceUnavailable, "merge_dispatch_unclaimed",
			"the merge verdict is recorded and durable, but the run's merge dispatch could not be claimed on the merge-dispatch ledger, so nothing was dispatched (ADR-093 approval condition D1). Re-invoke the merge to retry.",
			map[string]any{"run_id": runID.String(), "pr_url": prURL, "verdict_sequence": verdictSequence, "retryable": true})
		return mergequeue.DispatchClaim{}, false, true
	}
	if obs := s.observeMergeAlreadyLanded(r.Context(), runID, runRow, subject, true); obs.Landed {
		if existing.State == mergequeue.DispatchFailed {
			s.appendMergeDispatchSettled(r.Context(), existing, mergeDispatchReconciledMerged, "the forge reports the pull request merged")
		}
		s.writeAlreadyMergedResponse(w, r, runID, prURL, verdictSequence, alreadyRecorded, obs)
		return mergequeue.DispatchClaim{}, false, true
	}
	s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn, "merge: merge already dispatched for this run; the forge was not called",
		slog.String("run_id", runID.String()), slog.String("claim_state", string(existing.State)),
		slog.String("claim_path", existing.Path), slog.String("failure_cause", existing.FailureCause))
	s.writeError(w, r, http.StatusConflict, "merge_already_dispatched",
		fmt.Sprintf("a merge dispatch is already claimed for this run (state %s, path %s, head %s), so the forge was not called again (ADR-093 approval condition D1). A dispatched merge settles through the forge: watch fishhawk_get_run_status. A dispatch_failed claim needs the operator re-arm (backend/internal/mergequeue/README.md § \"Merge-dispatch ledger\") before a re-invoke.",
			existing.State, existing.Path, existing.HeadSHA),
		map[string]any{"run_id": runID.String(), "pr_url": prURL, "verdict_sequence": verdictSequence,
			"claim_state": string(existing.State), "claim_path": existing.Path, "claim_head_sha": existing.HeadSHA,
			"claimed_at": existing.ClaimedAt, "failure_cause": existing.FailureCause})
	return mergequeue.DispatchClaim{}, false, true
}
