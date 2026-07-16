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

// CategoryMergeVerdictRecorded is the audit-log category for the entry the
// one-verb merge endpoint (POST /v0/runs/{run_id}/merge, #1954) chains when
// an operator records a merge verdict before the squash merge is queued. It
// is a durable, operator-authored declaration modeled on
// operator_commit_vouched: the payload names the run, the verdict text, the
// pull-request URL, and delegated:false (the operator acted directly, not the
// campaign auto-driver). Internal chained audit kind — NOT an issue-comment
// surface (the living anchor comment projects it via the audit chain), so
// docs/issue-comment-surfaces.md needs no new row.
const CategoryMergeVerdictRecorded = "merge_verdict_recorded"

// mergeRunRequest is the JSON body of POST /v0/runs/{run_id}/merge. The
// verdict is required: recording the operator's merge decision is the whole
// point of the endpoint (the squash merge it queues is audited against it).
type mergeRunRequest struct {
	Verdict string `json:"verdict"`
}

// mergeRunResponse reports the recorded verdict + queued merge. The MCP
// fishhawk_merge_run tool re-POSTs on resume with NO client-side skip, so it
// relies on AlreadyRecorded to distinguish a first record from an idempotent
// re-record while VerdictSequence stays stable across both.
type mergeRunResponse struct {
	RunID           string `json:"run_id"`
	Verdict         string `json:"verdict"`
	PullRequestURL  string `json:"pr_url"`
	MergeQueued     bool   `json:"merge_queued"`
	VerdictSequence int64  `json:"verdict_sequence"`
	AlreadyRecorded bool   `json:"already_recorded"`
}

// handleMergeRun implements POST /v0/runs/{run_id}/merge (#1954): the one-verb
// operator path that takes a gate-approved run from verdict to merged. It
// records the operator's merge verdict as a chained merge_verdict_recorded
// audit entry (modeled on handleVouchCommit) and queues the squash merge
// through the SAME GitHubMerger seam AutoDriveRunGate's delegated may_merge arm
// uses — extracted into runMergeDispatch so drive_run's merge act and this
// operator verb converge on one path.
//
// The endpoint is IDEMPOTENT by construction (binding approval condition 1):
// on a POST that finds an existing merge_verdict_recorded row for the run it
// does NOT append a duplicate (responds already_recorded:true) but ALWAYS
// re-dispatches the merge seam — enabling/queuing auto-merge (or a REST merge)
// on an already-queued PR is safe and idempotent. So a verdict-recorded-but-
// merge-queue-failed state is retried by a plain re-POST, and the MCP tool
// re-POSTs on resume with no client-side dedup.
//
// Auth mirrors handleVouchCommit / handleAutoDrive:
//   - anonymous → 401 authentication_required;
//   - a run-bound MCP token ("mcp:run:<uuid>" subject) → 403 run_token_forbidden
//     (an agent token may not self-drive its own merge; merging is an operator
//     action, mirroring the #961 decide_scope_amendment guard);
//   - a token missing write:approvals → 403 insufficient_scope.
//
// Fail-closed guards, ALL before any audit write:
//   - 400 on a bad run_id or an empty verdict;
//   - 404 on an unknown run;
//   - 409 when the run has no pull-request URL;
//   - 409 when the run is terminal-failed or cancelled;
//   - 409 when the acceptance gate is unreadable or not passed (ADR-049
//     decision #6) — the operator never merges on unknown/unmet acceptance;
//   - 503 merge_unconfigured when no merge seam is configured.
//
// It deliberately does NOT block on a review stage parked at awaiting_approval
// (unlike mergeGateReady): in feature_change the implement-review stage settles
// ON merge via resolveReviewStageOnMerge, so blocking on it would deadlock the
// human merge path. On a merger error it returns 502 stating the verdict row is
// durable and the queue step is retryable. It does NOT wait for the terminal
// run state — the await is client-side in the MCP tool (the await_audit idiom).
func (s *Server) handleMergeRun(w http.ResponseWriter, r *http.Request) {
	id := IdentityFrom(r.Context())
	if id.IsAnonymous() {
		s.writeError(w, r, http.StatusUnauthorized, "authentication_required",
			"an authenticated token is required", nil)
		return
	}
	// A run-bound agent token may NEVER drive its own merge — merging is an
	// operator action (mirrors the vouch / decide_scope_amendment guard).
	if _, runBound := runBoundTokenRunID(id); runBound {
		s.writeError(w, r, http.StatusForbidden, "run_token_forbidden",
			"a run-bound agent token may not merge a run; recording a merge verdict and queuing the merge is an operator action",
			nil)
		return
	}
	if id.TokenID != "" && !hasScope(id, "write:approvals") {
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
			"verdict is required: state the operator merge verdict recorded against the queued merge",
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

	// Fail-closed guards, all BEFORE any write.
	if runRow.PullRequestURL == nil || *runRow.PullRequestURL == "" {
		s.writeError(w, r, http.StatusConflict, "no_pull_request",
			"run has no pull request to merge", map[string]any{"run_id": runID.String()})
		return
	}
	if runRow.State == run.StateFailed || runRow.State == run.StateCancelled {
		s.writeError(w, r, http.StatusConflict, "run_not_mergeable",
			"run is "+string(runRow.State)+"; a failed or cancelled run cannot be merged",
			map[string]any{"run_id": runID.String(), "state": string(runRow.State)})
		return
	}

	stages, err := s.cfg.RunRepo.ListStagesForRun(r.Context(), runID)
	if err != nil {
		// Fail-closed: we cannot confirm the acceptance gate, so refuse the merge.
		s.writeError(w, r, http.StatusConflict, "acceptance_gate_unverified",
			"cannot read run stages to verify the acceptance gate; merge refused (fail-closed)",
			map[string]any{"run_id": runID.String(), "error": err.Error()})
		return
	}
	// Acceptance gate (ADR-049 decision #6): a read error OR any non-eligible
	// state (pending / outcome-unknown / failed) refuses the merge fail-closed.
	gateState, gerr := s.acceptanceGateState(r.Context(), runRow, stages)
	if !acceptanceMergeEligible(gateState, gerr) {
		details := map[string]any{"run_id": runID.String(), "acceptance_gate_state": gateState}
		if gerr != nil {
			details["error"] = gerr.Error()
		}
		s.writeError(w, r, http.StatusConflict, "acceptance_gate_not_passed",
			"acceptance gate is not passed; the run is not merge-eligible (fail-closed)", details)
		return
	}

	// 503 BEFORE any write: without a merge seam the endpoint cannot queue the
	// merge, so it records nothing rather than a verdict it can never act on.
	merger := s.cfg.GateMerger
	if merger == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "merge_unconfigured",
			"no merge client configured; cannot queue the merge", nil)
		return
	}

	// Idempotence (binding condition 1): if a merge_verdict_recorded row already
	// exists for the run, do NOT append a duplicate — but ALWAYS re-dispatch the
	// merge below.
	existing, err := s.cfg.AuditRepo.ListForRunByCategory(r.Context(), runID, CategoryMergeVerdictRecorded)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"read prior merge verdicts failed", map[string]any{"error": err.Error()})
		return
	}

	alreadyRecorded := false
	var verdictSequence int64
	if len(existing) > 0 {
		alreadyRecorded = true
		for _, e := range existing {
			if e.Sequence > verdictSequence {
				verdictSequence = e.Sequence
			}
		}
	} else {
		subject := id.Subject
		if subject == "" {
			subject = "anonymous"
		}
		actorKind := audit.ActorUser
		payload, _ := json.Marshal(map[string]any{
			"run_id":    runID.String(),
			"verdict":   verdict,
			"pr_url":    *runRow.PullRequestURL,
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
		if aerr != nil {
			s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn,
				"merge: append merge_verdict_recorded audit entry failed",
				slog.String("run_id", runID.String()), slog.String("error", aerr.Error()))
			s.writeError(w, r, http.StatusInternalServerError, "internal_error",
				"record merge verdict failed", map[string]any{"error": aerr.Error()})
			return
		}
		verdictSequence = entry.Sequence
	}

	// Dispatch the merge through the shared seam (converges with the delegated
	// may_merge arm). The verdict row is already durable, so a merger error is a
	// retryable queue failure: 502 tells the client to re-POST.
	md := s.runMergeDispatch(r.Context(), runRow, stages, merger)
	if md.DispatchErr != nil {
		s.writeError(w, r, http.StatusBadGateway, "merge_queue_failed",
			"the merge verdict is recorded and durable, but queuing the merge failed; retry the merge (the verdict is not re-recorded)",
			map[string]any{"run_id": runID.String(), "error": md.DispatchErr.Error(), "verdict_sequence": verdictSequence})
		return
	}

	s.writeJSON(w, r, http.StatusOK, mergeRunResponse{
		RunID:           runID.String(),
		Verdict:         verdict,
		PullRequestURL:  *runRow.PullRequestURL,
		MergeQueued:     true,
		VerdictSequence: verdictSequence,
		AlreadyRecorded: alreadyRecorded,
	})
}
