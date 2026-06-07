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
	// review:awaiting_approval with a null PR.
	//
	// When Outcome=="pushed" the body is a decomposed-child push-success
	// report (#771): the child committed + pushed onto the shared parent
	// branch but opened no PR (the parent run opens one consolidated PR after
	// all children settle, per ADR-032). The PR fields (pr_number/pr_url) are
	// absent; branch/head_sha/base_sha carry the pushed commit. The handler
	// drives the child stage's terminal transition its push_to_shared_branch
	// trace gate left in `running` without creating a PR artifact.
	//
	// When Outcome=="fixup_pushed" the body is a fix-up re-dispatch
	// push-success report (#794): the fix-up committed + pushed onto the
	// EXISTING PR branch (no new PR — the open PR already tracks the branch).
	// The PR fields are absent; branch/head_sha/base_sha carry the pushed
	// commit. The handler drives the fix-up stage's terminal transition its
	// push_fixup trace gate left in `running` without creating a PR artifact.
	//
	// On the success body Outcome is empty and the PR fields are required.
	// These are declared directly on the struct (with omitempty) so the
	// handler's DisallowUnknownFields decoder accepts ALL shapes without a
	// separate discriminator struct.
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
	if p.Outcome == "failed" {
		if p.Category != "B" && p.Category != "C" {
			return fmt.Errorf("category must be \"B\" or \"C\" for a failed outcome, got %q", p.Category)
		}
		if p.Reason == "" {
			return errors.New("reason is required for a failed outcome")
		}
		return nil
	}
	// Child-push success variant (#771): a decomposed child pushed onto the
	// shared parent branch without opening a PR. No pr_number/pr_url; require
	// the shared-branch commit coordinates so the audit entry pins what
	// landed.
	if p.Outcome == "pushed" {
		switch {
		case p.Branch == "":
			return errors.New("branch is required for a pushed outcome")
		case p.HeadSHA == "":
			return errors.New("head_sha is required for a pushed outcome")
		case p.BaseSHA == "":
			return errors.New("base_sha is required for a pushed outcome")
		}
		return nil
	}
	// Fix-up push success variant (#794): a fix-up re-dispatch committed onto
	// the EXISTING PR branch without opening a new PR. Same shape as the
	// child-push "pushed" variant — no pr_number/pr_url; require the
	// existing-branch commit coordinates so the audit entry pins what landed.
	if p.Outcome == "fixup_pushed" {
		switch {
		case p.Branch == "":
			return errors.New("branch is required for a fixup_pushed outcome")
		case p.HeadSHA == "":
			return errors.New("head_sha is required for a fixup_pushed outcome")
		case p.BaseSHA == "":
			return errors.New("base_sha is required for a fixup_pushed outcome")
		}
		return nil
	}
	if p.Outcome != "" {
		return fmt.Errorf("outcome must be \"failed\", \"pushed\", or \"fixup_pushed\" when set, got %q", p.Outcome)
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

	// Child-push success variant (#771): a decomposed child pushed its commit
	// onto the shared parent branch (no PR). Drive the child stage's terminal
	// transition its push_to_shared_branch trace gate left in `running`, write
	// a child_pushed audit entry, and respond 200. No PR artifact and no
	// pull_request_url backfill — the parent run opens the consolidated PR.
	if pr.Outcome == "pushed" {
		s.succeedChildPushStage(w, r, runID, stage, &pr, authMethod, actorKind, actorSubject)
		return
	}

	// Fix-up push success variant (#794): a fix-up re-dispatch committed +
	// pushed onto the EXISTING PR branch (no new PR). Drive the fix-up stage's
	// terminal transition its push_fixup trace gate left in `running`, write a
	// fixup_pushed audit entry, and respond 200. No PR artifact and no
	// pull_request_url backfill — the PR already exists and tracks this branch.
	if pr.Outcome == "fixup_pushed" {
		s.succeedFixupPushStage(w, r, runID, stage, &pr, authMethod, actorKind, actorSubject)
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

	// Branch lineage guard (ADR-035, #858). Before opening the clean
	// review gate, verify every commit on the run branch is attributable
	// to this run's own reported head SHAs. A foreign commit (#797) fails
	// the stage category-B here instead of riding into the PR diff. The
	// PR number is in the report body, so the anchor resolves directly to
	// the PR's base ref. On a violation the helper already failed the
	// stage + emitted the audit + notified; respond and RETURN without
	// advancing to the review gate.
	if !s.verifyBranchLineage(r.Context(), runID, stage, pr.HeadSHA, pr.PRNumber) {
		s.writeJSON(w, r, http.StatusCreated, pullRequestResponse{
			ID:          created.ID,
			StageID:     created.StageID,
			ContentHash: created.ContentHash,
			PRNumber:    pr.PRNumber,
			PRURL:       pr.PRURL,
			HeadSHA:     pr.HeadSHA,
			Idempotent:  false,
		})
		return
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

// pullRequestChildPushResponse is the 200 body for the child-push success
// variant (#771): a decomposed child pushed onto the shared parent branch and
// the child stage transitioned terminally. HeadSHA echoes the pushed commit.
type pullRequestChildPushResponse struct {
	StageID uuid.UUID `json:"stage_id"`
	Outcome string    `json:"outcome"`
	Branch  string    `json:"branch"`
	HeadSHA string    `json:"head_sha"`
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

// succeedChildPushStage handles the child-push success variant (#771): a
// decomposed child committed + pushed onto the shared parent branch (no PR)
// after the trace gate left the child implement stage in `running`. It drives
// the running → terminal transition via advanceImplementStageAfterPR (a no-op
// if the stage already advanced — e.g. an older runner that never gated),
// writes a child_pushed audit entry pinning the shared-branch commit into the
// chain, fires the sticky status comment, and responds 200. No PR artifact,
// no pull_request_url backfill — the parent run opens the consolidated PR
// after all children settle.
func (s *Server) succeedChildPushStage(w http.ResponseWriter, r *http.Request, runID uuid.UUID,
	stage *run.Stage, pr *pullRequestBody, authMethod string, actorKind audit.ActorKind, actorSubject *string) {
	stageID := stage.ID

	// Idempotency (#776): a runner retry after a 5xx — or a duplicate delivery —
	// must not append a second child_pushed audit entry or fire a redundant
	// status-comment update. Dedup on (stage_id, head_sha): an identical re-push
	// of the SAME commit is suppressed, but a genuine push of NEW work to the
	// shared parent branch carries a different head_sha and is still recorded.
	// Guard first, before advance/audit/notify, so the duplicate is a clean
	// no-op. Fail-open on a read error: a transient failure must never silently
	// drop a legitimate child-push report, so we WARN and fall through.
	if entries, err := s.cfg.AuditRepo.ListForRunByCategory(r.Context(), runID, "child_pushed"); err != nil {
		s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn,
			"child-push report: list child_pushed audit entries failed; proceeding without idempotency guard",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("error", err.Error()))
	} else if childPushAlreadyRecorded(entries, stageID, pr.HeadSHA) {
		s.writeJSON(w, r, http.StatusOK, pullRequestChildPushResponse{
			StageID: stageID,
			Outcome: "pushed",
			Branch:  pr.Branch,
			HeadSHA: pr.HeadSHA,
		})
		return
	}

	// Branch lineage guard (ADR-035, #858). No PR number in the body — the
	// parent run opens the consolidated PR later — so the anchor resolves
	// from the run's tracked pull_request_url (0 = unknown → fail open). A
	// foreign commit on the shared branch fails the stage category-B here.
	if !s.verifyBranchLineage(r.Context(), runID, stage, pr.HeadSHA, 0) {
		s.writeJSON(w, r, http.StatusOK, pullRequestChildPushResponse{
			StageID: stageID,
			Outcome: "pushed",
			Branch:  pr.Branch,
			HeadSHA: pr.HeadSHA,
		})
		return
	}

	if stage.Type == run.StageTypeImplement && stage.State == run.StageStateRunning {
		s.advanceImplementStageAfterPR(r, runID, stage)
	}

	auditPayload, _ := json.Marshal(map[string]any{
		"run_id":              runID.String(),
		"stage_id":            stageID.String(),
		"branch":              pr.Branch,
		"head_sha":            pr.HeadSHA,
		"base_sha":            pr.BaseSHA,
		"files_changed_count": pr.FilesChangedCount,
		"auth_method":         authMethod,
	})
	if _, err := s.cfg.AuditRepo.AppendChained(r.Context(), audit.ChainAppendParams{
		RunID:        runID,
		StageID:      &stageID,
		Timestamp:    time.Now().UTC(),
		Category:     "child_pushed",
		ActorKind:    &actorKind,
		ActorSubject: actorSubject,
		Payload:      auditPayload,
	}); err != nil {
		s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn,
			"child-push report: append audit entry failed",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("error", err.Error()))
	}

	s.notifyStatusUpdate(r.Context(), runID, "child_pushed")

	s.writeJSON(w, r, http.StatusOK, pullRequestChildPushResponse{
		StageID: stageID,
		Outcome: "pushed",
		Branch:  pr.Branch,
		HeadSHA: pr.HeadSHA,
	})
}

// pullRequestFixupPushResponse is the 200 body for the fix-up push success
// variant (#794): a fix-up re-dispatch pushed onto the EXISTING PR branch and
// the fix-up stage transitioned terminally. HeadSHA echoes the pushed commit.
type pullRequestFixupPushResponse struct {
	StageID uuid.UUID `json:"stage_id"`
	Outcome string    `json:"outcome"`
	Branch  string    `json:"branch"`
	HeadSHA string    `json:"head_sha"`
}

// succeedFixupPushStage handles the fix-up push success variant (#794): a
// fix-up re-dispatch committed + pushed onto the EXISTING PR branch (no new PR)
// after the trace gate left the fix-up implement stage in `running`. It drives
// the running → terminal transition via advanceImplementStageAfterPR (a no-op
// if the stage already advanced — e.g. an older runner that never gated), writes
// a fixup_pushed audit entry pinning the pushed commit into the chain, fires the
// sticky status comment, and responds 200. No PR artifact, no pull_request_url
// backfill — the PR already exists and tracks this branch. The terminal
// transition is what the advisory implement re-review (fired at trace time)
// keys off; deferring it here is the #794 fix.
//
// Idempotency-guarded on (stage_id, head_sha) mirroring succeedChildPushStage
// (#776): a runner retry after a 5xx — or a duplicate delivery — must not append
// a second fixup_pushed audit entry or fire a redundant status-comment update.
func (s *Server) succeedFixupPushStage(w http.ResponseWriter, r *http.Request, runID uuid.UUID,
	stage *run.Stage, pr *pullRequestBody, authMethod string, actorKind audit.ActorKind, actorSubject *string) {
	stageID := stage.ID

	// Dedup on (stage_id, head_sha): an identical re-push of the SAME commit is
	// suppressed, but a genuine push of NEW work carries a different head_sha
	// and is still recorded. Guard first, before advance/audit/notify. Fail-open
	// on a read error: WARN and fall through so a transient failure never drops a
	// legitimate fix-up-push report.
	if entries, err := s.cfg.AuditRepo.ListForRunByCategory(r.Context(), runID, "fixup_pushed"); err != nil {
		s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn,
			"fix-up push report: list fixup_pushed audit entries failed; proceeding without idempotency guard",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("error", err.Error()))
	} else if childPushAlreadyRecorded(entries, stageID, pr.HeadSHA) {
		s.writeJSON(w, r, http.StatusOK, pullRequestFixupPushResponse{
			StageID: stageID,
			Outcome: "fixup_pushed",
			Branch:  pr.Branch,
			HeadSHA: pr.HeadSHA,
		})
		return
	}

	// Branch lineage guard (ADR-035, #858). The PR already exists and the
	// run tracks its pull_request_url, so the anchor resolves to the PR's
	// base ref (0 = no body PR number → resolved from the run). A foreign
	// commit on the PR branch fails the stage category-B here instead of
	// advancing the fix-up review gate.
	if !s.verifyBranchLineage(r.Context(), runID, stage, pr.HeadSHA, 0) {
		s.writeJSON(w, r, http.StatusOK, pullRequestFixupPushResponse{
			StageID: stageID,
			Outcome: "fixup_pushed",
			Branch:  pr.Branch,
			HeadSHA: pr.HeadSHA,
		})
		return
	}

	if stage.Type == run.StageTypeImplement && stage.State == run.StageStateRunning {
		s.advanceImplementStageAfterPR(r, runID, stage)
	}

	auditPayload, _ := json.Marshal(map[string]any{
		"run_id":              runID.String(),
		"stage_id":            stageID.String(),
		"branch":              pr.Branch,
		"head_sha":            pr.HeadSHA,
		"base_sha":            pr.BaseSHA,
		"files_changed_count": pr.FilesChangedCount,
		"auth_method":         authMethod,
	})
	if _, err := s.cfg.AuditRepo.AppendChained(r.Context(), audit.ChainAppendParams{
		RunID:        runID,
		StageID:      &stageID,
		Timestamp:    time.Now().UTC(),
		Category:     "fixup_pushed",
		ActorKind:    &actorKind,
		ActorSubject: actorSubject,
		Payload:      auditPayload,
	}); err != nil {
		s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn,
			"fix-up push report: append audit entry failed",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("error", err.Error()))
	}

	s.notifyStatusUpdate(r.Context(), runID, "fixup_pushed")

	s.writeJSON(w, r, http.StatusOK, pullRequestFixupPushResponse{
		StageID: stageID,
		Outcome: "fixup_pushed",
		Branch:  pr.Branch,
		HeadSHA: pr.HeadSHA,
	})
}

// childPushAlreadyRecorded reports whether a child_pushed audit entry for the
// given stage with the same head_sha already exists, so a runner retry or a
// duplicate delivery can be suppressed (#776). It mirrors pickRedactedTraceHash
// (trace.go): the entries are sequence-ascending per ListForRunByCategory's
// contract, and we skip any whose StageID is nil or mismatched. The keying is
// (stage_id, head_sha) — a NEW push to the shared parent branch carries a
// different head_sha and is not suppressed.
func childPushAlreadyRecorded(entries []*audit.Entry, stageID uuid.UUID, headSHA string) bool {
	for _, e := range entries {
		if e.StageID == nil || *e.StageID != stageID {
			continue
		}
		var payload struct {
			HeadSHA string `json:"head_sha"`
		}
		if err := json.Unmarshal(e.Payload, &payload); err != nil {
			continue
		}
		if payload.HeadSHA == headSHA {
			return true
		}
	}
	return false
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

	// Fix-up recovery (#788): a push_fixup fix-up re-dispatch whose
	// commit/push/compile-gate step fails reaches here after FailStage lands the
	// implement stage in `failed`. If this is a failed fix-up re-dispatch,
	// maybeRecoverFixupFailure restores the run to its pre-fix-up review gate
	// (implement → succeeded, review → awaiting_approval) and we SKIP the
	// run-failing Advance below so the run stays `running` and the original
	// mergeable PR is not orphaned. A non-fix-up PR-open failure (the common
	// case) returns false and the orchestrator Advance runs unchanged. The
	// pull_request_failed audit entry is still written either way — it is the
	// honest record that the commit/push/PR-open step failed.
	recovered := s.maybeRecoverFixupFailure(r.Context(), runID, stage.ID)

	// Advance the run so the orchestrator walks it forward — without this the
	// run stays pending/running after the stage fails. Best-effort, mirroring
	// the trace handler's advanceAfterFailure. Skipped on recovery: the stage
	// is no longer failed, so advancing would not fail the run, but skipping
	// keeps the intent explicit and avoids a redundant walk.
	if !recovered && s.cfg.Orchestrator != nil {
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
