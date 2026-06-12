package server

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/mcptoken"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/signing"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// CategoryMCPTokenIssued is the audit-log category for the new
// per-run MCP bearer token (E19.8 / #348). Payload carries
// {token_id, expires_at, ttl_seconds} so a post-hoc reviewer can
// see when the token was minted and when it lapses — never the
// plaintext.
const CategoryMCPTokenIssued = "mcp_token_issued"

// maxMCPTokenRequestBytes caps the request body so a giant payload
// can't OOM the verifier. The body is signed, so even an empty
// body is fine — the signature over `sha256(empty)` is still
// distinct per-run.
const maxMCPTokenRequestBytes = 4096

// mcpTokenResponse is the wire shape returned by POST /v0/runs/
// {id}/mcp-token. token is the plaintext bearer string — the
// caller stores it and feeds it to the agent's environment as
// FISHHAWK_API_TOKEN. The plaintext is shown exactly once; reads
// of the token row never return it.
type mcpTokenResponse struct {
	Token     string    `json:"token"`
	TokenID   uuid.UUID `json:"token_id"`
	RunID     uuid.UUID `json:"run_id"`
	ExpiresAt time.Time `json:"expires_at"`
}

// handleIssueMCPToken implements POST /v0/runs/{run_id}/mcp-token.
// Mints a short-lived (TTL = mcptoken.DefaultTTL = 60min) bearer
// token scoped to the run so the runner-side Claude Code agent
// can call the MCP server's read-only tool surface mid-execution.
//
// Auth: same Ed25519 signing-key path the installation-token
// endpoint uses (#197 / E5.X). The runner already issued a per-
// run key at stage start and proves possession by signing the
// request body. No bearer auth: the runner has no operator
// credentials; the signing key is what binds the request to the
// run.
//
// Audit: a `mcp_token_issued` chain entry records the issuance.
// Payload carries the token id + expiry + TTL — never the
// plaintext. A failure to append the audit row is logged but
// does NOT unwind the issuance; the load-bearing protection is
// the TTL, not the audit row.
func (s *Server) handleIssueMCPToken(w http.ResponseWriter, r *http.Request) {
	if s.cfg.MCPTokenRepo == nil || s.cfg.RunRepo == nil || s.cfg.SigningRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "mcp_token_unconfigured",
			"mcp-token endpoint requires mcp-token, run, and signing repositories", nil)
		return
	}

	runID, err := uuid.Parse(r.PathValue("run_id"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"run_id must be a valid UUID",
			map[string]any{"field": "run_id", "got": r.PathValue("run_id")})
		return
	}

	// Confirm the run exists before verifying the signature. A 404
	// here is much friendlier than a generic 401 when the runner's
	// pointing at the wrong backend.
	runRow, err := s.cfg.RunRepo.GetRun(r.Context(), runID)
	if err != nil {
		if errors.Is(err, run.ErrNotFound) {
			s.writeError(w, r, http.StatusNotFound, "run_not_found",
				"no run with that id", nil)
			return
		}
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"get run failed", map[string]any{"error": err.Error()})
		return
	}

	// Verify the request signature against the run's stored
	// public key. Mirrors installation-token's Ed25519 path
	// exactly — body signed by the per-run key the runner issued
	// at stage start.
	if !s.verifyMCPTokenSignature(w, r, runID) {
		return
	}

	// Determine scopes for the new token. Start with the baseline
	// mcp:read and add write:retries when the executing stage's
	// workflow spec sets executor.agent_self_retry: true.
	scopes := []string{"mcp:read"}
	if agentSelfRetry := s.resolveAgentSelfRetry(r, runRow); agentSelfRetry {
		scopes = append(scopes, "write:retries")
	}
	// Implement-stage tokens always carry write:scope-amendments
	// (E22.X / #961) — UNCONDITIONALLY, independent of the
	// agent_self_retry conditional above — so the implement agent
	// can file mid-stage scope amendment requests. The scope only
	// admits requesting: the decision endpoint requires write:stages
	// and rejects run-bound tokens outright.
	if s.resolveExecutingStageType(r, runRow) == run.StageTypeImplement {
		scopes = append(scopes, "write:scope-amendments")
	}

	tok, err := s.cfg.MCPTokenRepo.Issue(r.Context(), mcptoken.IssueParams{
		RunID:  runRow.ID,
		TTL:    mcptoken.DefaultTTL,
		Scopes: scopes,
	})
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"issue mcp token failed", map[string]any{"error": err.Error()})
		return
	}

	// Audit entry. Best-effort — a failure here logs but doesn't
	// unwind the issuance. The plaintext is NEVER in the payload.
	s.writeMCPTokenIssuedAudit(r, runID, tok, scopes)

	s.writeJSON(w, r, http.StatusCreated, mcpTokenResponse{
		Token:     tok.PlainText,
		TokenID:   tok.ID,
		RunID:     tok.RunID,
		ExpiresAt: tok.ExpiresAt,
	})
}

// verifyMCPTokenSignature reads the X-Fishhawk-Signature header,
// hashes the request body, and verifies via the run's stored
// public key. On any failure it writes the appropriate error
// response and returns false; the caller short-circuits. Mirrors
// installation-token's verification path.
func (s *Server) verifyMCPTokenSignature(w http.ResponseWriter, r *http.Request, runID uuid.UUID) bool {
	sigHeader := r.Header.Get("X-Fishhawk-Signature")
	if sigHeader == "" {
		s.writeError(w, r, http.StatusUnauthorized, "auth_required",
			"X-Fishhawk-Signature (Ed25519) required", nil)
		return false
	}
	signature, err := hex.DecodeString(sigHeader)
	if err != nil {
		s.writeError(w, r, http.StatusUnauthorized, "signature_invalid",
			"X-Fishhawk-Signature is not valid hex",
			map[string]any{"error": err.Error()})
		return false
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxMCPTokenRequestBytes+1))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"could not read request body", map[string]any{"error": err.Error()})
		return false
	}
	if len(body) > maxMCPTokenRequestBytes {
		s.writeError(w, r, http.StatusRequestEntityTooLarge, "body_too_large",
			"request body exceeds size cap",
			map[string]any{"limit_bytes": maxMCPTokenRequestBytes})
		return false
	}
	message := signing.ComputeMessage(body)
	if err := s.cfg.SigningRepo.Verify(r.Context(), runID, message, signature); err != nil {
		switch {
		case errors.Is(err, signing.ErrNotFound):
			s.writeError(w, r, http.StatusNotFound, "signing_key_not_found",
				"no signing key issued for this run", map[string]any{"run_id": runID.String()})
		case errors.Is(err, signing.ErrExpired):
			s.writeError(w, r, http.StatusUnauthorized, "signing_key_expired",
				"signing key TTL has passed", map[string]any{"run_id": runID.String()})
		case errors.Is(err, signing.ErrSignatureInvalid):
			s.writeError(w, r, http.StatusUnauthorized, "signature_invalid",
				"signature does not match the run's stored public key", nil)
		default:
			s.writeError(w, r, http.StatusInternalServerError, "internal_error",
				"signature verification failed", map[string]any{"error": err.Error()})
		}
		return false
	}
	return true
}

func (s *Server) writeMCPTokenIssuedAudit(r *http.Request, runID uuid.UUID, tok *mcptoken.Token, scopes []string) {
	if s.cfg.AuditRepo == nil {
		return
	}
	systemKind := audit.ActorSystem
	actor := "system"
	payload, _ := json.Marshal(map[string]any{
		"token_id":     tok.ID.String(),
		"expires_at":   tok.ExpiresAt.UTC().Format(time.RFC3339),
		"ttl_seconds":  int(time.Until(tok.ExpiresAt).Seconds()),
		"run_id":       runID.String(),
		"token_prefix": mcptoken.TokenPrefix, // not the plaintext — just identifies the kind
		"scopes":       scopes,
	})
	if _, err := s.cfg.AuditRepo.AppendChained(r.Context(), audit.ChainAppendParams{
		RunID:        runID,
		Timestamp:    time.Now().UTC(),
		Category:     CategoryMCPTokenIssued,
		ActorKind:    &systemKind,
		ActorSubject: &actor,
		Payload:      payload,
	}); err != nil {
		s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn,
			"mcp_token_issued audit append failed",
			slog.String("run_id", runID.String()),
			slog.String("token_id", tok.ID.String()),
			slog.String("error", err.Error()))
	}
}

// activeOrNextStage resolves the stage a runner-side request is
// acting for: the first dispatched/running stage in the (sequence-
// ordered) slice, or — when none exists — the first NON-TERMINAL
// stage. The fallback covers the local-runner first-stage gap
// (#1030): local-runner stages stay `pending` for their whole
// execution (they only walk pending→dispatched→running at trace-
// upload time), so a run whose FIRST stage is implement (a
// decomposition child) has no dispatched/running stage at token-
// issuance time. Non-terminal — not merely pending — keeps earlier
// gates authoritative: an awaiting_approval plan stage blocks the
// fallback from reaching a later pending implement stage. Returns
// nil when every stage is terminal.
func activeOrNextStage(stages []*run.Stage) *run.Stage {
	for _, st := range stages {
		if st.State == run.StageStateDispatched || st.State == run.StageStateRunning {
			return st
		}
	}
	for _, st := range stages {
		if !st.State.IsTerminal() {
			return st
		}
	}
	return nil
}

// resolveExecutingStageType returns the type of the run's currently
// dispatched/running stage, falling back to the run's next non-
// terminal stage (the local-runner first-stage gap, #1030), or ""
// when none is resolvable. Unlike resolveAgentSelfRetry it needs no
// workflow spec — the stage row's own type is the signal — so the
// write:scope-amendments grant is independent of spec parseability.
func (s *Server) resolveExecutingStageType(r *http.Request, runRow *run.Run) run.StageType {
	if s.cfg.RunRepo == nil {
		return ""
	}
	stages, err := s.cfg.RunRepo.ListStagesForRun(r.Context(), runRow.ID)
	if err != nil {
		s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn,
			"list stages for mcp token stage type failed",
			slog.String("run_id", runRow.ID.String()),
			slog.String("error", err.Error()))
		return ""
	}
	st := activeOrNextStage(stages)
	if st == nil {
		return ""
	}
	return st.Type
}

// resolveAgentSelfRetry looks up the active-or-next stage for runRow
// (same #1030 fallback as resolveExecutingStageType, so a
// decomposition child's still-pending first stage resolves) and
// checks whether executor.agent_self_retry is true in the workflow
// spec. Returns false on any error (nil spec, missing stage, parse
// failure) — the token is issued with baseline mcp:read scopes.
func (s *Server) resolveAgentSelfRetry(r *http.Request, runRow *run.Run) bool {
	if len(runRow.WorkflowSpec) == 0 || s.cfg.RunRepo == nil {
		return false
	}
	stages, err := s.cfg.RunRepo.ListStagesForRun(r.Context(), runRow.ID)
	if err != nil {
		s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn,
			"list stages for mcp token scopes failed",
			slog.String("run_id", runRow.ID.String()),
			slog.String("error", err.Error()))
		return false
	}
	activeStage := activeOrNextStage(stages)
	if activeStage == nil {
		return false
	}
	parsed, parseErr := spec.ParseBytes(runRow.WorkflowSpec)
	if parseErr != nil {
		s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn,
			"parse workflow spec for mcp token scopes failed",
			slog.String("run_id", runRow.ID.String()),
			slog.String("error", parseErr.Error()))
		return false
	}
	wf, ok := parsed.Workflows[runRow.WorkflowID]
	if !ok || activeStage.Sequence < 1 || activeStage.Sequence > len(wf.Stages) {
		return false
	}
	return wf.Stages[activeStage.Sequence-1].Executor.AgentSelfRetry
}
