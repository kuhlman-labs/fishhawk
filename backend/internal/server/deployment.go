package server

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/signing"
)

// maxDeploymentBundleBytes caps the request body. A deployment artifact
// is small structured JSON (ADR-038's {environment, ref/sha,
// external_run_url, outcome, rollback_handle}), so 32 KB is well above
// any realistic payload — mirroring the pull-request cap.
const maxDeploymentBundleBytes = 32 * 1024

// Deploy audit categories (E23.5 / #1385, ADR-038). Open-set strings —
// audit_entries.category has no CHECK, so these need no migration (only
// the artifacts kind CHECK was widened, by 0037).
const (
	// CategoryDeploymentDispatched records that the delegating deploy stage
	// triggered the external pipeline. It is EMITTED by the E23.4 deploy
	// stage machine (the pre-execution-gated dispatch), not by this ship
	// handler; it is introduced as a constant + surfaced on the issue-comment
	// timeline here so the dispatch and the outcome render consistently.
	CategoryDeploymentDispatched = "deployment_dispatched"
	// CategoryDeploymentOutcomeRecorded records the persisted deployment
	// artifact + its settled outcome. Written by handleShipDeployment on
	// every successful artifact persist.
	CategoryDeploymentOutcomeRecorded = "deployment_outcome_recorded"
	// CategoryDeploymentRollbackInitiated / Completed record an explicit
	// operator rollback sub-action against a prior deploy (ADR-038's
	// rolled_back disposition). Written by handleShipDeployment when the body
	// carries the matching rollback_action.
	CategoryDeploymentRollbackInitiated = "deployment_rollback_initiated"
	CategoryDeploymentRollbackCompleted = "deployment_rollback_completed"
	// CategoryDeployRun is the deploy-side "trace event": the governance
	// record of the external pipeline run the deploy reconciler polled to
	// terminal (#1386 / E23.6). Written by ResolveDeploymentFromPollState
	// alongside the deployment_outcome_recorded entry. An audit category,
	// not an issue-comment surface, so it needs no
	// docs/issue-comment-surfaces.md entry.
	CategoryDeployRun = "deploy_run"
)

// deploymentBody is the wire shape the deploy executor POSTs. It carries
// ADR-038's signed deployment record fields. Stored verbatim as the
// artifact's content; v0 carries no schema_version because the field
// shape isn't yet schema-stable (mirroring pullRequestBody).
type deploymentBody struct {
	// Environment is the deploy target (e.g. "production", "staging"),
	// gated pre-execution by the deploy stage's allowed_environments
	// constraint. Required.
	Environment string `json:"environment"`
	// Ref is the git ref or sha that was deployed (ADR-038's "ref/sha").
	// Required.
	Ref string `json:"ref"`
	// ExternalRunURL points at the external pipeline run Fishhawk delegated
	// to (delegating mode — Fishhawk holds no deploy logic). Required.
	ExternalRunURL string `json:"external_run_url"`
	// Outcome is the terminal disposition: one of run.DeployOutcome
	// (succeeded|failed|partial|rolled_back). Required and validated.
	Outcome string `json:"outcome"`
	// RollbackHandle is the opaque handle the external pipeline returns for
	// reverting this deploy, when one exists. Optional.
	RollbackHandle string `json:"rollback_handle,omitempty"`

	// RollbackAction, when present, reports an explicit rollback sub-action
	// against a prior deploy: "initiated" writes a
	// deployment_rollback_initiated audit entry, "completed" writes a
	// deployment_rollback_completed entry — in ADDITION to the always-written
	// deployment_outcome_recorded entry. Optional; validated to the two
	// values when present.
	RollbackAction string `json:"rollback_action,omitempty"`
}

// validate returns a human-readable error if any required field is
// missing or malformed. A deployment record is the governance trail of a
// real external release, so a 400 here means the executor shipped the
// wrong shape.
func (d *deploymentBody) validate() error {
	switch {
	case d.Environment == "":
		return errors.New("environment is required")
	case d.Ref == "":
		return errors.New("ref is required")
	case d.ExternalRunURL == "" || !strings.HasPrefix(d.ExternalRunURL, "http"):
		return errors.New("external_run_url must be a non-empty http(s) URL")
	case d.Outcome == "":
		return errors.New("outcome is required")
	case !run.DeployOutcome(d.Outcome).Valid():
		return fmt.Errorf("outcome must be one of succeeded/failed/partial/rolled_back, got %q", d.Outcome)
	}
	if d.RollbackAction != "" && d.RollbackAction != "initiated" && d.RollbackAction != "completed" {
		return fmt.Errorf("rollback_action must be \"initiated\" or \"completed\" when set, got %q", d.RollbackAction)
	}
	return nil
}

// handleShipDeployment implements POST /v0/runs/{run_id}/deployment.
//
// Records ADR-038's signed deployment artifact and its governance trail.
// Modeled on handleShipPullRequest: dual-auth (Ed25519 X-Fishhawk-Signature
// runner path OR bearer token with write:runs scope), idempotent on
// (stage_id, content_hash), and chained-audit recording. It persists the
// deployment artifact (artifact.KindDeployment), writes a
// deployment_outcome_recorded audit entry, and — when the body reports a
// rollback sub-action — additionally writes a deployment_rollback_initiated
// or deployment_rollback_completed entry. The deploy stage's own state
// transitions are E23.6's concern; this slice is the persistence + audit
// surface only.
func (s *Server) handleShipDeployment(w http.ResponseWriter, r *http.Request) {
	if s.cfg.SigningRepo == nil || s.cfg.ArtifactRepo == nil ||
		s.cfg.AuditRepo == nil || s.cfg.RunRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "deployment_upload_unconfigured",
			"deployment upload requires signing, artifact, audit, and run repositories", nil)
		return
	}

	runID, err := uuid.Parse(r.PathValue("run_id"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"run_id must be a valid UUID",
			map[string]any{"field": "run_id", "got": r.PathValue("run_id")})
		return
	}

	stageID, err := uuid.Parse(r.URL.Query().Get("stage_id"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"stage_id query parameter must be a valid UUID",
			map[string]any{"field": "stage_id", "got": r.URL.Query().Get("stage_id")})
		return
	}

	stage, err := s.cfg.RunRepo.GetStage(r.Context(), stageID)
	if err != nil {
		s.writeError(w, r, http.StatusNotFound, "stage_not_found",
			"stage does not exist",
			map[string]any{"stage_id": stageID.String()})
		return
	}
	if stage.RunID != runID {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"stage does not belong to the supplied run",
			map[string]any{"stage_id": stageID.String(), "run_id": runID.String()})
		return
	}
	// A deployment governance artifact is scoped to a deploy stage (ADR-038 /
	// #1385). Without this guard a valid run signer or write:runs bearer could
	// pin a signed deployment record + deploy audit chain onto a plan/implement/
	// review stage. Reject any non-deploy stage before any persistence.
	if stage.Type != run.StageTypeDeploy {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"deployment artifacts may only be attached to a deploy stage",
			map[string]any{"stage_id": stageID.String(), "stage_type": string(stage.Type)})
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxDeploymentBundleBytes+1))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"could not read request body", map[string]any{"error": err.Error()})
		return
	}
	if len(body) > maxDeploymentBundleBytes {
		s.writeError(w, r, http.StatusRequestEntityTooLarge, "body_too_large",
			"deployment body exceeds size cap",
			map[string]any{"limit_bytes": maxDeploymentBundleBytes})
		return
	}

	authMethod, actorKind, actorSubject, ok := s.authorizeDeployment(w, r, runID, body)
	if !ok {
		return
	}

	var dep deploymentBody
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&dep); err != nil {
		s.writeError(w, r, http.StatusBadRequest, "deployment_invalid",
			"deployment body could not be decoded",
			map[string]any{"error": err.Error()})
		return
	}
	if err := dep.validate(); err != nil {
		s.writeError(w, r, http.StatusBadRequest, "deployment_invalid",
			"deployment body missing required fields",
			map[string]any{"error": err.Error()})
		return
	}

	contentHash := sha256Hex(body)

	// Idempotency: dedup on (stage_id, content_hash). A re-delivery of the
	// same deployment record returns the existing artifact rather than
	// creating a duplicate (and writing a second audit entry).
	if existing, err := s.cfg.ArtifactRepo.GetByHash(r.Context(), stageID, contentHash); err == nil {
		// Self-heal the chained governance audit entry (#1396). A prior
		// attempt may have persisted the artifact (Create succeeded) but
		// failed its deployment_outcome_recorded append (AppendChained
		// failed → 500); this identical retry short-circuits here. Verify
		// the outcome entry exists for this artifact and append it
		// idempotently if missing, so a retry-after-partial-failure ends
		// with BOTH the artifact and its governance record. The helper
		// fails closed on a read error (caller 500s; a further retry can
		// re-heal) rather than returning a possibly-gapped 200.
		if _, herr := s.ensureGovernanceAuditEntry(r.Context(), runID,
			CategoryDeploymentOutcomeRecorded, existing.ID.String(), func() error {
				outcomePayload, _ := json.Marshal(map[string]any{
					"run_id":           runID.String(),
					"stage_id":         stageID.String(),
					"artifact_id":      existing.ID.String(),
					"content_hash":     contentHash,
					"environment":      dep.Environment,
					"ref":              dep.Ref,
					"external_run_url": dep.ExternalRunURL,
					"outcome":          dep.Outcome,
					"rollback_handle":  dep.RollbackHandle,
					"auth_method":      authMethod,
				})
				_, aerr := s.cfg.AuditRepo.AppendChained(r.Context(), audit.ChainAppendParams{
					RunID:        runID,
					StageID:      &stageID,
					Timestamp:    time.Now().UTC(),
					Category:     CategoryDeploymentOutcomeRecorded,
					ActorKind:    &actorKind,
					ActorSubject: actorSubject,
					Payload:      outcomePayload,
				})
				return aerr
			}); herr != nil {
			s.writeError(w, r, http.StatusInternalServerError, "internal_error",
				"heal governance audit entry failed", map[string]any{"error": herr.Error()})
			return
		}
		s.writeJSON(w, r, http.StatusOK, deploymentResponse{
			ID:          existing.ID,
			StageID:     existing.StageID,
			ContentHash: existing.ContentHash,
			Environment: dep.Environment,
			Outcome:     dep.Outcome,
			Idempotent:  true,
		})
		return
	} else if !errors.Is(err, artifact.ErrNotFound) {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"check existing deployment failed", map[string]any{"error": err.Error()})
		return
	}

	created, err := s.cfg.ArtifactRepo.Create(r.Context(), artifact.CreateParams{
		StageID:     stageID,
		Kind:        artifact.KindDeployment,
		Content:     json.RawMessage(body),
		ContentHash: contentHash,
		// SchemaVersion intentionally nil for v0 — graduate to
		// deployment_v1 once the field shape settles.
	})
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"create deployment artifact failed", map[string]any{"error": err.Error()})
		return
	}

	outcomePayload, _ := json.Marshal(map[string]any{
		"run_id":           runID.String(),
		"stage_id":         stageID.String(),
		"artifact_id":      created.ID.String(),
		"content_hash":     contentHash,
		"environment":      dep.Environment,
		"ref":              dep.Ref,
		"external_run_url": dep.ExternalRunURL,
		"outcome":          dep.Outcome,
		"rollback_handle":  dep.RollbackHandle,
		"auth_method":      authMethod,
	})
	if _, err := s.cfg.AuditRepo.AppendChained(r.Context(), audit.ChainAppendParams{
		RunID:        runID,
		StageID:      &stageID,
		Timestamp:    time.Now().UTC(),
		Category:     CategoryDeploymentOutcomeRecorded,
		ActorKind:    &actorKind,
		ActorSubject: actorSubject,
		Payload:      outcomePayload,
	}); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"append audit entry failed", map[string]any{"error": err.Error()})
		return
	}

	// Rollback sub-action (ADR-038's rolled_back disposition). When the body
	// reports a rollback action, additionally pin it into the chain. The
	// deployment_outcome_recorded entry above is the authoritative artifact
	// record; the rollback entry is the additive governance signal that an
	// operator reverted (or began reverting) the deploy. Best-effort: a
	// rollback-entry append failure WARN-logs and does NOT unwind the
	// already-recorded artifact + outcome entry.
	if cat := rollbackCategory(dep.RollbackAction); cat != "" {
		rollbackPayload, _ := json.Marshal(map[string]any{
			"run_id":          runID.String(),
			"stage_id":        stageID.String(),
			"artifact_id":     created.ID.String(),
			"environment":     dep.Environment,
			"rollback_handle": dep.RollbackHandle,
			"rollback_action": dep.RollbackAction,
			"auth_method":     authMethod,
		})
		if _, err := s.cfg.AuditRepo.AppendChained(r.Context(), audit.ChainAppendParams{
			RunID:        runID,
			StageID:      &stageID,
			Timestamp:    time.Now().UTC(),
			Category:     cat,
			ActorKind:    &actorKind,
			ActorSubject: actorSubject,
			Payload:      rollbackPayload,
		}); err != nil {
			s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn,
				"deployment upload: append rollback audit entry failed",
				slog.String("run_id", runID.String()),
				slog.String("stage_id", stageID.String()),
				slog.String("category", cat),
				slog.String("error", err.Error()))
		}
	}

	// Webhook-target terminal transition (#1386 / E23.6). A generic-webhook
	// deploy pipeline has no GitHub run for the reconciler to poll, so it
	// reports its terminal outcome by calling back into THIS endpoint. When
	// the deploy stage is still parked at awaiting_deployment, advance it to
	// the terminal state mapped from the reported outcome (succeeded →
	// succeeded; failed/partial/rolled_back → failed, the disposition riding
	// the artifact's outcome field) and advance the run. github_actions
	// stages reach terminal via the reconciler instead, so this no-ops for a
	// stage already resolved (state != awaiting_deployment). Best-effort: a
	// transition failure WARN-logs rather than 500-ing the already-persisted
	// artifact + outcome record.
	if stage.State == run.StageStateAwaitingDeployment {
		if err := s.advanceDeployStageTerminal(r.Context(), stageID,
			run.DeployOutcome(dep.Outcome), "webhook"); err != nil {
			s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn,
				"deployment upload: webhook terminal transition failed",
				slog.String("run_id", runID.String()),
				slog.String("stage_id", stageID.String()),
				slog.String("outcome", dep.Outcome),
				slog.String("error", err.Error()))
		} else {
			s.advanceRunAfterReviewResolve(r.Context(), runID)
		}
	}

	// Refresh the run's sticky living-anchor comment so the deploy outcome
	// surfaces on the issue timeline (the deploy audit categories render
	// data-drivenly through issuecomment's activityCategories set).
	s.notifyStatusUpdate(r.Context(), runID, "deployment_recorded")

	s.writeJSON(w, r, http.StatusCreated, deploymentResponse{
		ID:          created.ID,
		StageID:     created.StageID,
		ContentHash: created.ContentHash,
		Environment: dep.Environment,
		Outcome:     dep.Outcome,
		Idempotent:  false,
	})
}

// rollbackCategory maps a body's rollback_action to its audit category, or
// "" when the body reports no rollback sub-action. validate() has already
// rejected any value other than the two recognized actions, so the default
// arm is reached only for the empty (no-rollback) case.
func rollbackCategory(action string) string {
	switch action {
	case "initiated":
		return CategoryDeploymentRollbackInitiated
	case "completed":
		return CategoryDeploymentRollbackCompleted
	default:
		return ""
	}
}

// authorizeDeployment resolves the request's auth method + actor, mirroring
// handleShipPullRequest's dual-auth block: an Ed25519 X-Fishhawk-Signature
// over sha256(body) (runner path) OR a bearer token with write:runs scope
// (operator path). On failure it has already written the error response and
// returns ok=false.
func (s *Server) authorizeDeployment(w http.ResponseWriter, r *http.Request, runID uuid.UUID, body []byte) (authMethod string, actorKind audit.ActorKind, actorSubject *string, ok bool) {
	sigHeader := r.Header.Get("X-Fishhawk-Signature")
	id := IdentityFrom(r.Context())
	switch {
	case sigHeader != "":
		signature, err := hex.DecodeString(sigHeader)
		if err != nil {
			s.writeError(w, r, http.StatusUnauthorized, "signature_invalid",
				"X-Fishhawk-Signature is not valid hex",
				map[string]any{"error": err.Error()})
			return "", "", nil, false
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
			return "", "", nil, false
		}
		return "ed25519", audit.ActorKind("system"), nil, true
	case !id.IsAnonymous() && hasScope(id, "write:runs") && hasScope(id, "write:deploy"):
		// ADR-040 D4 (#1027): kind from the token subject — user or agent.
		// write:deploy (ADR-038 / #1390) gates the operator bearer path on
		// top of write:runs — the deploy ship/rollback record is a
		// governance write specific to the deploy gate. The ed25519
		// signature branch above is the runner path and is NOT scope-gated.
		subj := id.Subject
		return "bearer", actorKindForSubject(id.Subject), &subj, true
	case !id.IsAnonymous() && hasScope(id, "write:runs") && !hasScope(id, "write:deploy"):
		// Authenticated with write:runs but missing the deploy-specific
		// scope: a 403 naming the missing scope, distinct from the 401 the
		// fully-unauthenticated default arm returns.
		s.writeError(w, r, http.StatusForbidden, "insufficient_scope",
			"deployment upload requires the write:deploy scope in addition to write:runs",
			map[string]any{"required_scope": "write:deploy"})
		return "", "", nil, false
	default:
		s.writeError(w, r, http.StatusUnauthorized, "signature_or_bearer_required",
			"request must include X-Fishhawk-Signature or an authenticated bearer token with write:runs scope", nil)
		return "", "", nil, false
	}
}

// deploymentResponse echoes the persisted artifact's identity back to the
// caller. Environment + outcome are surfaced explicitly (even though they
// live in the artifact body) as the most operator-useful correlation fields.
type deploymentResponse struct {
	ID          uuid.UUID `json:"id"`
	StageID     uuid.UUID `json:"stage_id"`
	ContentHash string    `json:"content_hash"`
	Environment string    `json:"environment"`
	Outcome     string    `json:"outcome"`
	Idempotent  bool      `json:"idempotent"`
}
