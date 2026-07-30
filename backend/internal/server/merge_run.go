package server

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
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
// row was appended — the merge helper is dispatched regardless.
type mergeRunResponse struct {
	RunID           string `json:"run_id"`
	MergeQueued     bool   `json:"merge_queued"`
	VerdictSequence int64  `json:"verdict_sequence"`
	AlreadyRecorded bool   `json:"already_recorded"`
	PRURL           string `json:"pr_url"`
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
//   - 503 when the merge seam (GateMerger) is unconfigured.
//
// It deliberately does NOT block on a review stage parked at awaiting_approval:
// in feature_change that stage settles ON merge via resolveReviewStageOnMerge,
// so requiring it settled first would deadlock the human merge path.
//
// Idempotence lives on the ENDPOINT (binding approval condition 1): a repeated
// POST that finds an existing merge_verdict_recorded row appends NO duplicate
// row and responds already_recorded:true, but ALWAYS re-dispatches the merge
// helper — so a 502-then-reinvoke re-queues the merge without ever duplicating
// the verdict. On the merge helper erroring the handler returns 502 stating the
// verdict row is durable and the queue step is retryable.
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
	// decision #6). Read the stages once and classify. Any pending / failed /
	// outcome-unknown / read-error state → 409; passed / not-declared /
	// skipped-out-of-scope / not-validated proceed. not-validated (#2347) is the
	// short-circuited zero-criteria-verified outcome: merge-ELIGIBLE by design,
	// because a change with no live target must not be stranded — the operator
	// reads the distinction from the run's next_actions state and status comment,
	// not from a block here. Deliberately does NOT block on a review stage
	// awaiting approval (resolveReviewStageOnMerge settles it ON merge).
	stages, err := s.cfg.RunRepo.ListStagesForRun(r.Context(), runID)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"list stages failed", map[string]any{"error": err.Error()})
		return
	}
	gateState, gerr := s.acceptanceGateState(r.Context(), runRow, stages)
	acceptanceMergeOK := gerr == nil && (gateState == acceptanceGateNotDeclared ||
		gateState == acceptanceGatePassed || gateState == acceptanceGateSkippedOutOfScope ||
		gateState == acceptanceGateNotValidated)
	if !acceptanceMergeOK {
		s.cfg.Logger.LogAttrs(r.Context(), slog.LevelInfo, "merge: acceptance gate does not admit the merge",
			slog.String("run_id", runID.String()),
			slog.String("acceptance_gate_state", gateState),
			slog.Bool("acceptance_read_error", gerr != nil))
		s.writeError(w, r, http.StatusConflict, "acceptance_gate_not_passed",
			"the acceptance gate does not admit a merge (must be passed, not-declared, skipped-out-of-scope, or not-validated)",
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
		subject := id.Subject
		if subject == "" {
			subject = "anonymous"
		}
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

	// Dispatch the shared merge helper. The verdict row is already durable, so a
	// dispatch failure is a retryable 502 (re-POST re-queues without duplicating
	// the row). The merge only ENABLES/queues GitHub's merge — the pr_merged /
	// run-completion settle is left to the pull_request-closed webhook, which is
	// why the MCP tool awaits the terminal state client-side.
	if merr := s.cfg.GateMerger.MergePullRequest(r.Context(), runRow); merr != nil {
		s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn, "merge: dispatch merge failed",
			slog.String("run_id", runID.String()), slog.String("error", merr.Error()))
		s.writeError(w, r, http.StatusBadGateway, "merge_dispatch_failed",
			"the merge verdict is recorded and durable, but queuing the squash merge failed; retry the merge",
			map[string]any{"run_id": runID.String(), "error": merr.Error(), "verdict_sequence": verdictSequence})
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
