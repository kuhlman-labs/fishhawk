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

// CategoryOperatorCommitVouched is the audit-log category for the entry
// the vouch handler writes when an operator declares a foreign commit on
// a run branch to be run-authored lineage (ADR-035 remediation, #1044).
// It is a durable, operator-authored declaration: the payload names the
// run, the vouched SHA, and the operator's reason. The reported-head
// ledger (lineage.go) unions vouched SHAs alongside the run's own
// pull_request_opened / child_pushed / fixup_pushed provenance, so an
// operator's mechanical remediation commit no longer wedges the run it
// fixed. Internal audit kind — NOT an issue-comment surface (the living
// anchor comment #1067 projects it via the audit chain).
const CategoryOperatorCommitVouched = "operator_commit_vouched"

// lineageVouchedSHAField is the payload field carrying the vouched commit
// SHA on an operator_commit_vouched entry. It is the write→read seam: the
// vouch handler writes it (handleVouchCommit) and the reported-head ledger
// reads it (addVouchedSHAs). Both sides reference this single constant so
// the seam cannot drift on a literal typo.
const lineageVouchedSHAField = "vouched_sha"

// vouchCommitRequest is the JSON body of POST
// /v0/runs/{run_id}/vouch-commit. Both fields are required: the vouch is
// an audited operator declaration, so it must name the commit and carry a
// rationale.
type vouchCommitRequest struct {
	SHA    string `json:"sha"`
	Reason string `json:"reason"`
}

// vouchCommitResponse echoes the recorded declaration.
type vouchCommitResponse struct {
	RunID      string `json:"run_id"`
	VouchedSHA string `json:"vouched_sha"`
	Reason     string `json:"reason"`
}

// handleVouchCommit implements POST /v0/runs/{run_id}/vouch-commit.
//
// It is the operator-gated, audited ADR-035 provenance path (#1044) for a
// foreign commit on a run branch that no loop-native remediation can route
// — an operator's mechanical remediation commit (e.g. a sync-schemas
// output pushed onto a fan-out branch whose children are all terminal with
// zero open concerns). The operator declares the commit run-authored
// lineage; the declaration is recorded as an operator_commit_vouched audit
// entry and unioned into the reported-head ledger, un-wedging the merge
// reconciler. It is distinct from reset-branch (which DROPS an on-top
// foreign commit): vouch KEEPS the operator commit and attributes it.
//
// Auth is operator-token-only by design (ADR-035 sole-writer invariant):
//
//   - anonymous → 401 authentication_required;
//   - a run-bound MCP token ("mcp:run:<uuid>" subject) is REJECTED OUTRIGHT
//     (403 run_token_forbidden), even for its own run. A run-bound agent
//     token self-declaring lineage for a foreign commit on its own branch
//     would defeat the sole-writer invariant the vouch must preserve (the
//     #797/#856 cross-write protection). Mirrors the #961
//     decide_scope_amendment run-bound rejection. Only an operator fhk_*
//     token carrying write:stages may vouch;
//   - a token without write:stages → 403 insufficient_scope.
//
// The handler records the operator's declaration verbatim — it does NOT
// verify the SHA exists on the branch or in the compare set. This is
// deliberate and safe: vouching a non-existent or wrong SHA simply adds an
// unreachable ledger entry and un-wedges nothing (the real foreign commit
// still flags), so the fail-closed property (an unvouched foreign commit
// still fails category-B) holds.
func (s *Server) handleVouchCommit(w http.ResponseWriter, r *http.Request) {
	id := IdentityFrom(r.Context())
	if id.IsAnonymous() {
		s.writeError(w, r, http.StatusUnauthorized, "authentication_required",
			"an authenticated token is required", nil)
		return
	}
	// Operator-token-only: a run-bound agent token may NEVER vouch — not
	// even for its own run. Vouch declares git lineage, and an agent
	// self-declaring lineage for a commit on its own branch defeats the
	// ADR-035 sole-writer invariant. Rejected outright, mirroring the #961
	// decide_scope_amendment guard. Defense in depth: implement-stage
	// tokens are never issued write:stages either.
	if _, runBound := runBoundTokenRunID(id); runBound {
		s.writeError(w, r, http.StatusForbidden, "run_token_forbidden",
			"a run-bound agent token may not vouch a commit; vouching git lineage is an operator action (ADR-035 sole-writer invariant)",
			nil)
		return
	}
	if id.TokenID != "" && !hasScope(id, "write:stages") {
		s.writeError(w, r, http.StatusForbidden, "insufficient_scope",
			"token is missing required scope: write:stages",
			map[string]any{"required_scope": "write:stages"})
		return
	}

	if s.cfg.RunRepo == nil || s.cfg.AuditRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "vouch_unconfigured",
			"vouch-commit endpoint requires run + audit repositories", nil)
		return
	}

	runID, err := uuid.Parse(r.PathValue("run_id"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"run_id must be a valid UUID",
			map[string]any{"field": "run_id", "got": r.PathValue("run_id")})
		return
	}

	var reqBody vouchCommitRequest
	if r.Body != nil {
		if decErr := json.NewDecoder(r.Body).Decode(&reqBody); decErr != nil && !errors.Is(decErr, io.EOF) {
			s.writeError(w, r, http.StatusBadRequest, "validation_failed",
				"request body must be valid JSON {sha, reason}",
				map[string]any{"error": decErr.Error()})
			return
		}
	}
	sha := strings.TrimSpace(reqBody.SHA)
	if sha == "" {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"sha is required: name the commit to vouch as run-authored lineage",
			map[string]any{"field": "sha"})
		return
	}
	reason := strings.TrimSpace(reqBody.Reason)
	if reason == "" {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"reason is required: the vouch is an audited operator declaration; state why this commit is run-authored lineage",
			map[string]any{"field": "reason"})
		return
	}

	if _, err := s.cfg.RunRepo.GetRun(r.Context(), runID); err != nil {
		if errors.Is(err, run.ErrNotFound) {
			s.writeError(w, r, http.StatusNotFound, "run_not_found",
				"no run with that id", map[string]any{"run_id": runID.String()})
			return
		}
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"get run failed", map[string]any{"error": err.Error()})
		return
	}

	subject := id.Subject
	if subject == "" {
		subject = "anonymous"
	}
	actorKind := audit.ActorUser
	payload, _ := json.Marshal(map[string]any{
		"run_id":               runID.String(),
		lineageVouchedSHAField: sha,
		"reason":               reason,
	})
	if _, err := s.cfg.AuditRepo.AppendChained(r.Context(), audit.ChainAppendParams{
		RunID:        runID,
		Timestamp:    time.Now().UTC(),
		Category:     CategoryOperatorCommitVouched,
		ActorKind:    &actorKind,
		ActorSubject: &subject,
		Payload:      payload,
	}); err != nil {
		s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn,
			"vouch-commit: append operator_commit_vouched audit entry failed",
			slog.String("run_id", runID.String()),
			slog.String("vouched_sha", sha),
			slog.String("error", err.Error()))
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"record vouch failed", map[string]any{"error": err.Error()})
		return
	}

	s.writeJSON(w, r, http.StatusOK, vouchCommitResponse{
		RunID:      runID.String(),
		VouchedSHA: sha,
		Reason:     reason,
	})
}
