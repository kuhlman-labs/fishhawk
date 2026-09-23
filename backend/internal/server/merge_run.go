package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
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
	auditRes, auditRecomputed := s.republishAuditCheckBeforeMerge(r.Context(), runID)

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
// proceed unchanged — a behind-but-clean or checks-pending branch still merges
// as it does today. The documented boolean is kept load-bearing alongside the
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
		msg += " The forge confirms the merge, but the merge_observation_recorded row could NOT be persisted (" +
			obs.AppendErr.Error() + "); POST /v0/runs/{run_id}/record-merge-observation to record it."
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
