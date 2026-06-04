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

// maxPullRequestBundleBytes caps the request body. PR artifacts are
// small structured JSON (a handful of fields, no embedded diff), so
// 32 KB is well above any realistic payload and well below trace's
// 64 MiB cap.
const maxPullRequestBundleBytes = 32 * 1024

// pullRequestBody is the wire shape the runner POSTs. Required
// fields are validated structurally below — there's no JSON Schema
// for v0; v1+ can graduate this to `pull_request_v1.schema.json`.
type pullRequestBody struct {
	PRNumber          int    `json:"pr_number"`
	PRURL             string `json:"pr_url"`
	Branch            string `json:"branch"`
	HeadSHA           string `json:"head_sha"`
	BaseSHA           string `json:"base_sha"`
	Title             string `json:"title"`
	Body              string `json:"body,omitempty"`
	FilesChangedCount int    `json:"files_changed_count"`

	// Outcome, Category, and Reason form the optional failure-report
	// variant (#742). When Outcome=="failed" the body is a runner-reported
	// commit/push/PR-open failure — no PR was opened, so the PR fields above
	// are absent. The handler then fails the implement stage its trace gate
	// left in `running` (category C is retryable, B parks for re-scope)
	// instead of creating a PR artifact, so the run never strands at
	// review:awaiting_approval with a null PR. On the success body Outcome
	// is empty and the PR fields are required. These are declared directly
	// on the struct (with omitempty) so the handler's DisallowUnknownFields
	// decoder accepts BOTH shapes without a separate discriminator struct.
	Outcome  string `json:"outcome,omitempty"`
	Category string `json:"category,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

// validate returns a human-readable error if any required field is
// missing. PR upload is irreversible (real PR exists on GitHub by
// the time this fires), so a 400 here means the runner shipped the
// wrong shape — the operator's audit log will need to be reconciled
// by hand.
func (p *pullRequestBody) validate() error {
	// Failure-report variant (#742): no PR was opened, so the PR fields are
	// absent. Require the outcome marker, a valid failure category, and a
	// reason; the PR-field checks below don't apply.
	if p.Outcome != "" {
		if p.Outcome != "failed" {
			return fmt.Errorf("outcome must be \"failed\" when set, got %q", p.Outcome)
		}
		if p.Category != "B" && p.Category != "C" {
			return fmt.Errorf("category must be \"B\" or \"C\" for a failed outcome, got %q", p.Category)
		}
		if p.Reason == "" {
			return errors.New("reason is required for a failed outcome")
		}
		return nil
	}
	switch {
	case p.PRNumber <= 0:
		return errors.New("pr_number must be a positive integer")
	case p.PRURL == "" || !strings.HasPrefix(p.PRURL, "http"):
		return errors.New("pr_url must be a non-empty http(s) URL")
	case p.Branch == "":
		return errors.New("branch is required")
	case p.HeadSHA == "":
		return errors.New("head_sha is required")
	case p.BaseSHA == "":
		return errors.New("base_sha is required")
	case p.Title == "":
		return errors.New("title is required")
	}
	return nil
}

// hasScope reports whether id contains the exact scope string.
func hasScope(id Identity, scope string) bool {
	for _, s := range id.Scopes {
		if s == scope {
			return true
		}
	}
	return false
}

// handleShipPullRequest implements POST /v0/runs/{run_id}/pull-request.
//
// Accepts either an Ed25519 X-Fishhawk-Signature (runner path) or a
// bearer token with write:runs scope (operator path). When neither is
// present the handler returns 401 signature_or_bearer_required.
func (s *Server) handleShipPullRequest(w http.ResponseWriter, r *http.Request) {
	if s.cfg.SigningRepo == nil || s.cfg.ArtifactRepo == nil ||
		s.cfg.AuditRepo == nil || s.cfg.RunRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "pull_request_upload_unconfigured",
			"pull-request upload requires signing, artifact, audit, and run repositories", nil)
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

	body, err := io.ReadAll(io.LimitReader(r.Body, maxPullRequestBundleBytes+1))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"could not read request body", map[string]any{"error": err.Error()})
		return
	}
	if len(body) > maxPullRequestBundleBytes {
		s.writeError(w, r, http.StatusRequestEntityTooLarge, "body_too_large",
			"pull-request body exceeds size cap",
			map[string]any{"limit_bytes": maxPullRequestBundleBytes})
		return
	}

	var authMethod string
	var actorKind audit.ActorKind
	var actorSubject *string

	sigHeader := r.Header.Get("X-Fishhawk-Signature")
	id := IdentityFrom(r.Context())
	switch {
	case sigHeader != "":
		signature, err := hex.DecodeString(sigHeader)
		if err != nil {
			s.writeError(w, r, http.StatusUnauthorized, "signature_invalid",
				"X-Fishhawk-Signature is not valid hex",
				map[string]any{"error": err.Error()})
			return
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
			return
		}
		authMethod = "ed25519"
		actorKind = audit.ActorKind("system")
	case !id.IsAnonymous() && hasScope(id, "write:runs"):
		authMethod = "bearer"
		actorKind = audit.ActorKind("operator")
		subj := id.Subject
		actorSubject = &subj
	default:
		s.writeError(w, r, http.StatusUnauthorized, "signature_or_bearer_required",
			"request must include X-Fishhawk-Signature or an authenticated bearer token with write:runs scope", nil)
		return
	}

	var pr pullRequestBody
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&pr); err != nil {
		s.writeError(w, r, http.StatusBadRequest, "pull_request_invalid",
			"pull-request body could not be decoded",
			map[string]any{"error": err.Error()})
		return
	}
	if err := pr.validate(); err != nil {
		s.writeError(w, r, http.StatusBadRequest, "pull_request_invalid",
			"pull-request body missing required fields",
			map[string]any{"error": err.Error()})
		return
	}

	// Failure-report variant (#742): the runner's commit/push/PR-open step
	// failed after the trace gate left the implement stage in `running`.
	// Fail the stage (category C is retryable) and advance the run so it
	// never strands at review:awaiting_approval with a null PR. No artifact
	// and no pull_request_url backfill — there is no PR.
	if pr.Outcome == "failed" {
		s.failPullRequestStage(w, r, runID, stage, &pr, authMethod, actorKind, actorSubject)
		return
	}

	contentHash := sha256Hex(body)

	// Idempotency: dedup on (stage_id, content_hash). The runner
	// computes content_hash over the canonical bytes it shipped, so
	// re-running an identical job returns the same artifact rather
	// than creating a duplicate.
	if existing, err := s.cfg.ArtifactRepo.GetByHash(r.Context(), stageID, contentHash); err == nil {
		s.writeJSON(w, r, http.StatusOK, pullRequestResponse{
			ID:          existing.ID,
			StageID:     existing.StageID,
			ContentHash: existing.ContentHash,
			PRNumber:    pr.PRNumber,
			PRURL:       pr.PRURL,
			HeadSHA:     pr.HeadSHA,
			Idempotent:  true,
		})
		return
	} else if !errors.Is(err, artifact.ErrNotFound) {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"check existing pull-request failed", map[string]any{"error": err.Error()})
		return
	}

	created, err := s.cfg.ArtifactRepo.Create(r.Context(), artifact.CreateParams{
		StageID:     stageID,
		Kind:        artifact.KindPullRequest,
		Content:     json.RawMessage(body),
		ContentHash: contentHash,
		// SchemaVersion intentionally nil for v0 — graduate to
		// pull_request_v1 in v0.x once the field shape settles.
	})
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"create pull-request artifact failed", map[string]any{"error": err.Error()})
		return
	}

	auditPayload, _ := json.Marshal(map[string]any{
		"run_id":              runID.String(),
		"stage_id":            stageID.String(),
		"artifact_id":         created.ID.String(),
		"content_hash":        contentHash,
		"pr_number":           pr.PRNumber,
		"pr_url":              pr.PRURL,
		"branch":              pr.Branch,
		"head_sha":            pr.HeadSHA,
		"base_sha":            pr.BaseSHA,
		"files_changed_count": pr.FilesChangedCount,
		"size_bytes":          len(body),
		"auth_method":         authMethod,
	})
	if _, err := s.cfg.AuditRepo.AppendChained(r.Context(), audit.ChainAppendParams{
		RunID:        runID,
		StageID:      &stageID,
		Timestamp:    time.Now().UTC(),
		Category:     "pull_request_opened",
		ActorKind:    &actorKind,
		ActorSubject: actorSubject,
		Payload:      auditPayload,
	}); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"append audit entry failed", map[string]any{"error": err.Error()})
		return
	}

	// Backfill the run's pull_request_url so the threaded-runs view
	// (#216) can group every run on this PR with a single equality
	// query. Best-effort: a write failure logs but doesn't unwind
	// the upload — the PR artifact + audit row are already in
	// place, and a cron-style backfill could reconcile later.
	if _, err := s.cfg.RunRepo.SetRunPullRequestURL(r.Context(), runID, pr.PRURL); err != nil {
		s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn,
			"backfill pull_request_url failed",
			slog.String("run_id", runID.String()),
			slog.String("pr_url", pr.PRURL),
			slog.String("error", err.Error()),
		)
	}

	// Push-and-open-pr terminal drive (#742). When the implement stage was
	// left in `running` by the trace gate (the runner stamped
	// push_and_open_pr), THIS upload is the authoritative driver of the
	// stage's terminal transition — the PR is now durably recorded, so open
	// the review gate. When the stage already advanced (the non-gated flow
	// where the trace handler transitioned it), it isn't in `running` and
	// the helper is a no-op — byte-identical to the prior behavior.
	if stage.Type == run.StageTypeImplement && stage.State == run.StageStateRunning {
		s.advanceImplementStageAfterPR(r, runID, stage)
	}

	// Sticky status comment (E20.4 / #330). The PR-opened transition
	// adds the PR URL to the run; the status comment's footer now
	// surfaces the "Pull request →" link, so an update here is the
	// signal that lets operators jump to the PR from the issue thread.
	s.notifyStatusUpdate(r.Context(), runID, "pr_opened")

	s.writeJSON(w, r, http.StatusCreated, pullRequestResponse{
		ID:          created.ID,
		StageID:     created.StageID,
		ContentHash: created.ContentHash,
		PRNumber:    pr.PRNumber,
		PRURL:       pr.PRURL,
		HeadSHA:     pr.HeadSHA,
		Idempotent:  false,
	})
}

// pullRequestResponse echoes the persisted artifact's identity back
// to the runner. PRNumber and HeadSHA are surfaced explicitly even
// though they're in the artifact body — they're the most operator-
// useful fields for log correlation, and including them avoids a
// second round-trip to read the artifact back.
type pullRequestResponse struct {
	ID          uuid.UUID `json:"id"`
	StageID     uuid.UUID `json:"stage_id"`
	ContentHash string    `json:"content_hash"`
	PRNumber    int       `json:"pr_number"`
	PRURL       string    `json:"pr_url"`
	HeadSHA     string    `json:"head_sha"`
	Idempotent  bool      `json:"idempotent"`
}

// pullRequestFailureResponse is the 200 body for the failure-report
// variant (#742): the runner-reported commit/push/PR-open failure was
// recorded and the implement stage transitioned to failed.
type pullRequestFailureResponse struct {
	StageID  uuid.UUID `json:"stage_id"`
	Outcome  string    `json:"outcome"`
	Category string    `json:"category"`
}

// advanceImplementStageAfterPR drives the implement stage's terminal
// transition once the PR artifact has landed (#742). The trace handler's
// push-and-open-pr gate leaves the stage in `running` until the
// /pull-request upload arrives, so this handler owns the running →
// awaiting_approval (gated) or running → succeeded (gateless) transition,
// mirroring advancePlanStageTerminal (#603).
//
// Best-effort: transition / advance errors are WARN-logged and never
// unwind the upload response — the PR artifact + URL backfill are already
// in place, and a stuck stage is recoverable via GET /v0/runs/{id}/stages.
func (s *Server) advanceImplementStageAfterPR(r *http.Request, runID uuid.UUID, stage *run.Stage) {
	terminal := run.StageStateAwaitingApproval
	if !stage.RequiresApproval {
		terminal = run.StageStateSucceeded
	}
	if _, err := s.cfg.RunRepo.TransitionStage(r.Context(), stage.ID, terminal, nil); err != nil {
		s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn,
			"pull-request upload: transition implement stage to terminal failed",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stage.ID.String()),
			slog.String("target", string(terminal)),
			slog.String("error", err.Error()))
		return
	}

	// Gateless stages get no approval submission to drive the next dispatch
	// — fire the orchestrator ourselves. Best-effort, like the plan handler.
	if terminal == run.StageStateSucceeded && s.cfg.Orchestrator != nil {
		if _, err := s.cfg.Orchestrator.Advance(r.Context(), runID); err != nil {
			s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn,
				"pull-request upload: orchestrator advance after gateless implement stage failed",
				slog.String("run_id", runID.String()),
				slog.String("stage_id", stage.ID.String()),
				slog.String("error", err.Error()))
		}
	}
}

// failPullRequestStage handles the failure-report variant (#742): the
// runner's commit/push/PR-open step failed after the trace gate left the
// implement stage in `running`. It fails the stage with the reported
// category (C is retryable via the failed → pending path; B parks for
// re-scope), advances the run so the orchestrator walks it forward, records
// a pull_request_failed audit entry pinning the runner's reason into the
// chain, and responds 200. The stage row carries the canonical category +
// reason; this never reaches review:awaiting_approval with a null PR.
func (s *Server) failPullRequestStage(w http.ResponseWriter, r *http.Request, runID uuid.UUID,
	stage *run.Stage, pr *pullRequestBody, authMethod string, actorKind audit.ActorKind, actorSubject *string) {
	cat := run.FailureC
	if pr.Category == "B" {
		cat = run.FailureB
	}
	if _, err := run.FailStage(r.Context(), s.cfg.RunRepo, stage.ID, cat, pr.Reason); err != nil {
		s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn,
			"pull-request failure report: fail stage failed",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stage.ID.String()),
			slog.String("error", err.Error()))
	}

	// Advance the run so the orchestrator walks it forward — without this the
	// run stays pending/running after the stage fails. Best-effort, mirroring
	// the trace handler's advanceAfterFailure.
	if s.cfg.Orchestrator != nil {
		if _, err := s.cfg.Orchestrator.Advance(r.Context(), runID); err != nil {
			s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn,
				"pull-request failure report: orchestrator advance failed",
				slog.String("run_id", runID.String()),
				slog.String("stage_id", stage.ID.String()),
				slog.String("error", err.Error()))
		}
	}

	stageID := stage.ID
	auditPayload, _ := json.Marshal(map[string]any{
		"run_id":      runID.String(),
		"stage_id":    stageID.String(),
		"category":    pr.Category,
		"reason":      pr.Reason,
		"auth_method": authMethod,
	})
	if _, err := s.cfg.AuditRepo.AppendChained(r.Context(), audit.ChainAppendParams{
		RunID:        runID,
		StageID:      &stageID,
		Timestamp:    time.Now().UTC(),
		Category:     "pull_request_failed",
		ActorKind:    &actorKind,
		ActorSubject: actorSubject,
		Payload:      auditPayload,
	}); err != nil {
		s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn,
			"pull-request failure report: append audit entry failed",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("error", err.Error()))
	}

	s.notifyStatusUpdate(r.Context(), runID, "pr_failed")

	s.writeJSON(w, r, http.StatusOK, pullRequestFailureResponse{
		StageID:  stageID,
		Outcome:  "failed",
		Category: pr.Category,
	})
}
