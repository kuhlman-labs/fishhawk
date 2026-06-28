package server

import (
	"context"
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
	"github.com/kuhlman-labs/fishhawk/backend/internal/prompt"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/signing"
)

// maxPullRequestBundleBytes caps the request body. PR artifacts are
// small structured JSON (a handful of fields, no embedded diff), so
// 32 KB is well above any realistic payload and well below trace's
// 64 MiB cap.
const maxPullRequestBundleBytes = 32 * 1024

// CategoryScopeFilesExempted is the supplemental post-seal audit row the
// success pull-request handler writes on a base-rebase re-invoke ship (#1218):
// the re-invoked agent's freshly-validated scope self-exemptions were reloaded
// AFTER the trace bundle (which folds the FIRST attempt's exemptions into its
// own scope_files_exempted gate_evidence event) already sealed and shipped under
// #742 forward gating, so this row re-surfaces the re-invoke exemption delta the
// sealed bundle could not carry. Payload carries {run_id, stage_id,
// exemptions:[{path, reason}], origin:"base_rebase_reinvoke", auth_method} — the
// origin marker distinguishes it from the bundle-sealed first-attempt event. NOT
// an issue-comment surface: it has no Notifier method and nothing in
// `issuecomment` posts it; see docs/issue-comment-surfaces.md.
const CategoryScopeFilesExempted = "scope_files_exempted"

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
	// When Outcome=="fixup_no_changes" the body is a fix-up re-dispatch that
	// produced NO changes (#856): the fix-up pass committed nothing, so no new
	// commit landed on the EXISTING PR branch (head_sha is absent — the branch
	// tip is unchanged). branch/base_sha pin the unchanged tip. The handler
	// drives the fix-up stage's terminal transition its push_fixup trace gate
	// left in `running` and re-parks the review gate, without creating a PR
	// artifact — otherwise the stage hangs in `running` until the SLA watchdog.
	//
	// On the success body Outcome is empty and the PR fields are required.
	// These are declared directly on the struct (with omitempty) so the
	// handler's DisallowUnknownFields decoder accepts ALL shapes without a
	// separate discriminator struct.
	Outcome  string `json:"outcome,omitempty"`
	Category string `json:"category,omitempty"`
	Reason   string `json:"reason,omitempty"`

	// VerifiedTreeSHA and MissingPaths form the scope-completeness park
	// report variant (#1231), present only when Outcome=="scope_park".
	// The runner's committed-tree gate's ONLY failure was the
	// scope-completeness "missing declared scope file(s)" check; it
	// pushed the gate-verified commit to the run branch (branch/head_sha,
	// no PR) and reports the tree it verified plus the declared scope.files
	// the agent never touched. The handler records a ScopeCompletenessPark
	// and parks the implement stage in awaiting_scope_decision for an
	// operator exempt-or-fail decision — no PR artifact.
	//
	// These mirror run.ScopeCompletenessPark's wire tags byte-for-byte and
	// the runner's park-report upload struct (runner/internal/upload —
	// sibling slice), the established ScopeExemption duplication pattern.
	// The runner omits them on every other variant, so they stay absent
	// there under the DisallowUnknownFields decoder.
	VerifiedTreeSHA string   `json:"verified_tree_sha,omitempty"`
	MissingPaths    []string `json:"missing_paths,omitempty"`

	// ApplyPath is the near-deterministic fix-up apply provenance (#1165/#1213),
	// present only on the Outcome=="fixup_pushed" report: "applied" (a clean
	// git-apply of every routed concern's suggested_patch, no agent), "agent"
	// (no apply-list served / agent re-derived), "apply_failed_fellback" (an
	// apply-list was served, the apply or its verify gate failed, the worktree
	// reset cleanly, and the agent re-derived). succeedFixupPushStage records it
	// onto the fixup_pushed audit entry so an operator can see whether the fix-up
	// collapsed to deterministic apply or fell back to the agent. Declared here
	// (with omitempty) so the DisallowUnknownFields decoder accepts the
	// fixup_pushed body that carries it; the runner omits it on every other
	// variant, so the field stays absent there.
	ApplyPath string `json:"apply_path,omitempty"`

	// SupplementalScopeExemptions is the base-rebase re-invoke exemption delta
	// (#1218), present ONLY on a success ship that followed a base-rebase
	// re-invoke. On that path the runner reloads the re-invoked agent's
	// freshly-validated scope self-exemptions AFTER the trace bundle (and its
	// scope_files_exempted gate_evidence event) already sealed and shipped under
	// #742 forward gating, so any exemption the final scope-completeness gate
	// honored that the sealed event did not carry is invisible to the audit/
	// review. The runner computes that delta and rides it here; the success
	// handler re-emits it as a supplemental scope_files_exempted audit row (the
	// visibility surface for the re-invoke branch). Optional + omitempty: absent
	// on the first ship and every non-re-invoke ship, so the
	// DisallowUnknownFields decoder accepts those byte-identical bodies. The json
	// tags (path/reason) on scopeExemption match the runner's
	// scopeExemptionEvidence marshal — the cross-boundary seam.
	SupplementalScopeExemptions []scopeExemption `json:"supplemental_scope_exemptions,omitempty"`
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
	// Fix-up no-changes variant (#856): a fix-up re-dispatch produced no
	// changes, so no new commit landed on the EXISTING PR branch. No
	// pr_number/pr_url and no head_sha (the branch tip is unchanged); require
	// branch + base_sha so the audit entry pins the unchanged tip.
	if p.Outcome == "fixup_no_changes" {
		switch {
		case p.Branch == "":
			return errors.New("branch is required for a fixup_no_changes outcome")
		case p.BaseSHA == "":
			return errors.New("base_sha is required for a fixup_no_changes outcome")
		}
		return nil
	}
	// Scope-completeness park variant (#1231): the implement stage's ONLY
	// committed-tree gate failure was the missing-declared-scope-file
	// check. The runner pushed the verified commit to the run branch (no
	// PR); require the commit coordinates, the verified tree, and at least
	// one missing path so the park payload pins exactly what is held.
	if p.Outcome == "scope_park" {
		switch {
		case p.Branch == "":
			return errors.New("branch is required for a scope_park outcome")
		case p.HeadSHA == "":
			return errors.New("head_sha is required for a scope_park outcome")
		case p.BaseSHA == "":
			return errors.New("base_sha is required for a scope_park outcome")
		case p.VerifiedTreeSHA == "":
			return errors.New("verified_tree_sha is required for a scope_park outcome")
		case len(p.MissingPaths) == 0:
			return errors.New("missing_paths must be non-empty for a scope_park outcome")
		}
		return nil
	}
	if p.Outcome != "" {
		return fmt.Errorf("outcome must be \"failed\", \"pushed\", \"fixup_pushed\", \"fixup_no_changes\", or \"scope_park\" when set, got %q", p.Outcome)
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
		// ADR-040 D4 (#1027): kind from the token subject — user or agent,
		// both within the DB CHECK and OpenAPI enum. The previous literal
		// "operator" was outside the closed set {agent,user,system}.
		actorKind = actorKindForSubject(id.Subject)
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

	// Fix-up no-changes variant (#856): a fix-up re-dispatch produced no changes,
	// so no new commit landed on the EXISTING PR branch. Drive the fix-up stage's
	// terminal transition its push_fixup trace gate left in `running`, re-park the
	// review gate, write a fixup_no_changes audit entry, and respond 200. No PR
	// artifact and no pull_request_url backfill — the PR already exists and its
	// branch tip is unchanged.
	if pr.Outcome == "fixup_no_changes" {
		s.succeedFixupNoChangesStage(w, r, runID, stage, &pr, authMethod, actorKind, actorSubject)
		return
	}

	// Scope-completeness park variant (#1231): the implement stage's ONLY
	// committed-tree gate failure was the missing-declared-scope-file
	// check, and the runner pushed its verified commit to the run branch
	// (no PR). Record the held-commit park payload, transition the
	// implement stage to awaiting_scope_decision, and write a
	// scope_completeness_parked audit entry — leaving the run parked for an
	// in-band operator exempt-or-fail decision. No PR artifact.
	if pr.Outcome == "scope_park" {
		s.parkScopeCompletenessStage(w, r, runID, stage, &pr, authMethod, actorKind, actorSubject)
		return
	}

	contentHash := sha256Hex(body)

	// Idempotency: dedup on (stage_id, content_hash). The runner
	// computes content_hash over the canonical bytes it shipped, so
	// re-running an identical job returns the same artifact rather
	// than creating a duplicate.
	if existing, err := s.cfg.ArtifactRepo.GetByHash(r.Context(), stageID, contentHash); err == nil {
		// Self-heal the chained governance audit entry (#1396). A prior
		// attempt may have persisted the artifact (Create succeeded) but
		// failed its pull_request_opened append (AppendChained failed →
		// 500); this identical retry short-circuits here. Verify the
		// pull_request_opened entry exists for this artifact and append it
		// idempotently if missing, so a retry-after-partial-failure ends
		// with BOTH the artifact and its governance record. The helper
		// fails closed on a read error (caller 500s; a further retry can
		// re-heal) rather than returning a possibly-gapped 200. Only the
		// primary governance entry is healed; the best-effort supplemental
		// rows and the pull_request_url backfill remain create-path-only.
		if _, herr := s.ensureGovernanceAuditEntry(r.Context(), runID,
			"pull_request_opened", existing.ID.String(), func() error {
				auditPayload, _ := json.Marshal(map[string]any{
					"run_id":              runID.String(),
					"stage_id":            stageID.String(),
					"artifact_id":         existing.ID.String(),
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
				_, aerr := s.cfg.AuditRepo.AppendChained(r.Context(), audit.ChainAppendParams{
					RunID:        runID,
					StageID:      &stageID,
					Timestamp:    time.Now().UTC(),
					Category:     "pull_request_opened",
					ActorKind:    &actorKind,
					ActorSubject: actorSubject,
					Payload:      auditPayload,
				})
				return aerr
			}); herr != nil {
			s.writeError(w, r, http.StatusInternalServerError, "internal_error",
				"heal governance audit entry failed", map[string]any{"error": herr.Error()})
			return
		}
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

	// Supplemental scope-files-exempted audit row (#1218). On a base-rebase
	// re-invoke ship the runner reloads the re-invoked agent's freshly-validated
	// scope self-exemptions AFTER the trace bundle already sealed and shipped
	// (#742 forward gating), so the delta the final scope-completeness gate
	// honored that the bundle's scope_files_exempted gate_evidence event did NOT
	// carry is invisible to the audit/review. The runner rides that delta in
	// SupplementalScopeExemptions; re-emit it here as a standalone
	// scope_files_exempted audit row with an origin marker so it is queryable via
	// the audit endpoint — the visibility surface for the re-invoke branch.
	//
	// This row exists because the sealed bundle (#742) cannot carry the
	// re-invoke's exemptions: the bundle ships at push time, before the runner
	// re-invokes the agent on the fresh base (runner main.go base-rebase block).
	// The first implement review is dispatched at trace-upload time (trace.go
	// runImplementReviews), strictly BEFORE the re-invoke, so its gate_evidence
	// could not carry the delta either. #1250 closes that gate_evidence half
	// WITHOUT re-sealing the bundle or re-running the first review: the
	// terminal-drive branch below (option (b), ADR-042) dispatches a bounded,
	// ADDITIVE supplemental implement-review pass anchored to this PR-upload —
	// the exact point the re-landed tree becomes durable. This audit row and
	// that supplemental review verdict are COMPLEMENTARY surfaces; both ride the
	// same SupplementalScopeExemptions delta.
	//
	// Best-effort: a nil AuditRepo or an append error WARN-logs and does NOT
	// unwind the upload — the pull_request_opened row + PR artifact are the
	// authoritative record of the terminal transition, so an observability row
	// must never wedge the forward-gated stage.
	if len(pr.SupplementalScopeExemptions) > 0 {
		supPayload, _ := json.Marshal(map[string]any{
			"run_id":      runID.String(),
			"stage_id":    stageID.String(),
			"exemptions":  pr.SupplementalScopeExemptions,
			"origin":      "base_rebase_reinvoke",
			"auth_method": authMethod,
		})
		if _, err := s.cfg.AuditRepo.AppendChained(r.Context(), audit.ChainAppendParams{
			RunID:        runID,
			StageID:      &stageID,
			Timestamp:    time.Now().UTC(),
			Category:     CategoryScopeFilesExempted,
			ActorKind:    &actorKind,
			ActorSubject: actorSubject,
			Payload:      supPayload,
		}); err != nil {
			s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn,
				"pull-request upload: append supplemental scope_files_exempted audit entry failed",
				slog.String("run_id", runID.String()),
				slog.String("stage_id", stageID.String()),
				slog.String("error", err.Error()))
		}
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

	// Dangling-PR close on a gating implement-review reject (#877). The
	// gating agent implement-review runs synchronously during the raw-trace
	// upload (advanceStageAfterTrace) and fails the implement stage
	// category-B BEFORE the runner — which has no view of that verdict —
	// opens its PR and POSTs here. By now the stage is terminally failed, so
	// the PR artifact + pull_request_opened audit + URL backfill above stay
	// honestly recorded but advanceImplementStageAfterPR below is a no-op
	// (the stage is not running), leaving the rejected change with an open
	// PR that will never merge. Detect that exact already-failed state
	// (implement + failed + category-B + the gating-reject reason prefix,
	// the same source of truth as the trace.go failure site) and close the
	// just-opened PR. Other category-B sources (lineage, spec-config, policy)
	// don't carry this prefix and are correctly excluded. Return the success
	// response without falling through to the lineage/advance path — the
	// stage is already failed, so there is nothing to advance.
	if stage.Type == run.StageTypeImplement && stage.State == run.StageStateFailed &&
		stage.FailureCategory != nil && *stage.FailureCategory == run.FailureB &&
		stage.FailureReason != nil && strings.HasPrefix(*stage.FailureReason, implementReviewGatingRejectPrefix) {
		s.closePRAfterGatingReject(r, runID, stage, &pr, created.ID)
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
		// Supplemental base-rebase re-invoke review (#1250, option (b) / ADR-042).
		// When this success ship carries the re-invoke exemption delta, dispatch
		// a bounded, additive supplemental implement-review against the PUSHED
		// re-landed tree BEFORE advancing — this is the durable-tree anchor point
		// (#742) and the only place the honored delta is known. A gating reject
		// fails the stage category-B (reusing trace.go's exact path) and closes
		// the dangling PR (reusing the #877 helper), then responds 201 WITHOUT
		// advancing — mirroring the already-failed gating-reject block above.
		// An empty delta (every non-re-invoke ship) skips this entirely and the
		// response is byte-identical.
		if len(pr.SupplementalScopeExemptions) > 0 {
			exemptions := make([]prompt.GateScopeExemption, 0, len(pr.SupplementalScopeExemptions))
			for _, ex := range pr.SupplementalScopeExemptions {
				exemptions = append(exemptions, prompt.GateScopeExemption{Path: ex.Path, Reason: ex.Reason})
			}
			if s.runSupplementalReinvokeReview(r.Context(), runID, stageID, pr.HeadSHA, exemptions) {
				if _, ferr := run.FailStage(r.Context(), s.cfg.RunRepo, stageID, run.FailureB, implementReviewGatingRejectReason); ferr != nil {
					s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn,
						"pull-request upload: transition to failed-B after supplemental reinvoke review gating reject failed",
						slog.String("run_id", runID.String()),
						slog.String("stage_id", stageID.String()),
						slog.String("error", ferr.Error()),
					)
				} else {
					// Reload so the failure-state fields the close helper reads
					// reflect the transition just applied.
					if failed, gerr := s.cfg.RunRepo.GetStage(r.Context(), stageID); gerr == nil {
						stage = failed
					}
					s.closePRAfterGatingReject(r, runID, stage, &pr, created.ID)
				}
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
		}
		s.advanceImplementStageAfterPR(r, runID, stage)
	}

	// Sticky status comment (E20.4 / #330). The PR-opened transition
	// adds the PR URL to the run; the status comment's footer now
	// surfaces the "Pull request →" link, so an update here is the
	// signal that lets operators jump to the PR from the issue thread.
	s.notifyStatusUpdate(r.Context(), runID, "pr_opened")

	// Board-state sync (#1012): the PR-opened edge advances the work item to
	// the in_review canonical state. Best-effort; never unwinds the response.
	s.notifyBoardTransition(r.Context(), runID, lifecyclePROpened)

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

	auditFields := map[string]any{
		"run_id":              runID.String(),
		"stage_id":            stageID.String(),
		"branch":              pr.Branch,
		"head_sha":            pr.HeadSHA,
		"base_sha":            pr.BaseSHA,
		"files_changed_count": pr.FilesChangedCount,
		"auth_method":         authMethod,
	}
	// Near-deterministic fix-up apply provenance (#1165/#1213): record whether the
	// fix-up collapsed to a deterministic git-apply or fell back to the agent.
	// Only the fixup_pushed variant carries it, and only when the runner reports a
	// recognized value — an absent or unknown apply_path leaves the key off the
	// entry rather than persisting a bogus discriminator.
	if ap := normalizeFixupApplyPath(pr.ApplyPath); ap != "" {
		auditFields["apply_path"] = ap
	}
	auditPayload, _ := json.Marshal(auditFields)
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

// pullRequestFixupNoChangesResponse is the 200 body for the fix-up no-changes
// variant (#856): a fix-up re-dispatch produced no changes and the fix-up stage
// transitioned terminally. No head_sha — the branch tip is unchanged.
type pullRequestFixupNoChangesResponse struct {
	StageID uuid.UUID `json:"stage_id"`
	Outcome string    `json:"outcome"`
	Branch  string    `json:"branch"`
}

// succeedFixupNoChangesStage handles the fix-up no-changes variant (#856): a
// fix-up re-dispatch produced no changes (the fix-up pass committed nothing)
// after the trace gate left the fix-up implement stage in `running`. It drives
// the running → terminal transition via advanceImplementStageAfterPR (which
// re-parks the review gate pending → awaiting_approval), writes a
// fixup_no_changes audit entry, fires the sticky status comment, and responds
// 200. No PR artifact, no pull_request_url backfill — the PR already exists and
// its branch tip is unchanged. Without this the stage hangs in `running` until
// the SLA watchdog reaps it, stranding the review stage in `pending`.
//
// This mirrors succeedFixupPushStage minus the head-keyed dedup and the
// verifyBranchLineage guard. Both are skipped DELIBERATELY: no new commit
// landed, so there is no head_sha to key on (the idempotency guard is therefore
// stage-keyed) and the branch tip is unchanged from the last vouched head (the
// pull_request_opened or a prior fixup_pushed audit entry already vouched it).
// A foreign commit on the branch is still caught by the next real push's lineage
// guard and the merge gate (ADR-036). This is a documented trade-off, not a
// verified claim; a reviewer can challenge it here.
func (s *Server) succeedFixupNoChangesStage(w http.ResponseWriter, r *http.Request, runID uuid.UUID,
	stage *run.Stage, pr *pullRequestBody, authMethod string, actorKind audit.ActorKind, actorSubject *string) {
	stageID := stage.ID

	// Stage-keyed idempotency: a runner retry after a 5xx — or a duplicate
	// delivery — must not append a second fixup_no_changes audit entry or fire a
	// redundant status-comment update. Unlike the fixup_pushed path there is no
	// head_sha to key on (no commit landed), so dedup on the existence of ANY
	// prior fixup_no_changes audit entry for this stage. Guard first, before
	// advance/audit/notify. Fail-open on a read error: WARN and fall through so a
	// transient failure never drops a legitimate report.
	if entries, err := s.cfg.AuditRepo.ListForRunByCategory(r.Context(), runID, "fixup_no_changes"); err != nil {
		s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn,
			"fix-up no-changes report: list fixup_no_changes audit entries failed; proceeding without idempotency guard",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("error", err.Error()))
	} else if auditEntryForStage(entries, stageID) {
		s.writeJSON(w, r, http.StatusOK, pullRequestFixupNoChangesResponse{
			StageID: stageID,
			Outcome: "fixup_no_changes",
			Branch:  pr.Branch,
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
		"base_sha":            pr.BaseSHA,
		"files_changed_count": pr.FilesChangedCount,
		"auth_method":         authMethod,
	})
	if _, err := s.cfg.AuditRepo.AppendChained(r.Context(), audit.ChainAppendParams{
		RunID:        runID,
		StageID:      &stageID,
		Timestamp:    time.Now().UTC(),
		Category:     "fixup_no_changes",
		ActorKind:    &actorKind,
		ActorSubject: actorSubject,
		Payload:      auditPayload,
	}); err != nil {
		s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn,
			"fix-up no-changes report: append audit entry failed",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("error", err.Error()))
	}

	s.notifyStatusUpdate(r.Context(), runID, "fixup_no_changes")

	s.writeJSON(w, r, http.StatusOK, pullRequestFixupNoChangesResponse{
		StageID: stageID,
		Outcome: "fixup_no_changes",
		Branch:  pr.Branch,
	})
}

// pullRequestScopeParkResponse is the 200 body for the scope-completeness
// park variant (#1231): the implement stage parked in
// awaiting_scope_decision with its verified commit held on the run branch.
// HeadSHA echoes the held commit; the operator decides exempt-or-fail.
type pullRequestScopeParkResponse struct {
	StageID uuid.UUID `json:"stage_id"`
	Outcome string    `json:"outcome"`
	Branch  string    `json:"branch"`
	HeadSHA string    `json:"head_sha"`
}

// scopeCompletenessParker is the OPTIONAL concrete-repo capability the
// park handler type-asserts (postgresRepo.ParkScopeCompletenessAndAppend):
// it atomically transitions the implement stage running →
// awaiting_scope_decision, writes the held-commit park payload, and
// appends the scope_completeness_parked audit entry in one transaction.
// In-memory fakes that don't implement it fall back to the two-step path.
type scopeCompletenessParker interface {
	ParkScopeCompletenessAndAppend(ctx context.Context, stageID uuid.UUID, park run.ScopeCompletenessPark, p audit.ChainAppendParams) (*run.Stage, bool, error)
}

// parkScopeCompletenessStage handles the scope-completeness park variant
// (#1231): the runner reports {outcome:"scope_park"} after its committed-
// tree gate failed ONLY on the missing-declared-scope-file check and it
// pushed the gate-verified commit to the run branch (no PR). It records
// the held-commit ScopeCompletenessPark payload, transitions the implement
// stage running → awaiting_scope_decision, and appends a
// scope_completeness_parked audit entry — leaving the run parked for an
// in-band operator exempt-or-fail decision (no PR artifact, no
// pull_request_url backfill — there is no PR).
//
// Atomicity + idempotency live in the parker capability's compare-and-set:
// a duplicate runner delivery that arrives after the stage already left
// running observes won=false and is a clean idempotent no-op. A repo that
// doesn't implement the capability (in-memory fakes) degrades to a two-
// step transition + best-effort audit append.
func (s *Server) parkScopeCompletenessStage(w http.ResponseWriter, r *http.Request, runID uuid.UUID,
	stage *run.Stage, pr *pullRequestBody, authMethod string, actorKind audit.ActorKind, actorSubject *string) {
	stageID := stage.ID
	park := run.ScopeCompletenessPark{
		HeldCommitSHA:   pr.HeadSHA,
		RunBranch:       pr.Branch,
		VerifiedTreeSHA: pr.VerifiedTreeSHA,
		MissingPaths:    pr.MissingPaths,
	}

	auditPayload, _ := json.Marshal(map[string]any{
		"run_id":            runID.String(),
		"stage_id":          stageID.String(),
		"branch":            pr.Branch,
		"head_sha":          pr.HeadSHA,
		"base_sha":          pr.BaseSHA,
		"verified_tree_sha": pr.VerifiedTreeSHA,
		"missing_paths":     pr.MissingPaths,
		"auth_method":       authMethod,
	})
	appendParams := audit.ChainAppendParams{
		RunID:        runID,
		StageID:      &stageID,
		Timestamp:    time.Now().UTC(),
		Category:     CategoryScopeCompletenessParked,
		ActorKind:    &actorKind,
		ActorSubject: actorSubject,
		Payload:      auditPayload,
	}

	if parker, ok := s.cfg.RunRepo.(scopeCompletenessParker); ok {
		if _, _, err := parker.ParkScopeCompletenessAndAppend(r.Context(), stageID, park, appendParams); err != nil {
			s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn,
				"scope-completeness park report: park stage failed",
				slog.String("run_id", runID.String()),
				slog.String("stage_id", stageID.String()),
				slog.String("error", err.Error()))
		}
		s.notifyStatusUpdate(r.Context(), runID, "scope_parked")
		s.writeJSON(w, r, http.StatusOK, pullRequestScopeParkResponse{
			StageID: stageID,
			Outcome: "scope_park",
			Branch:  pr.Branch,
			HeadSHA: pr.HeadSHA,
		})
		return
	}

	// Fallback (in-memory fakes): two-step transition + best-effort audit.
	if stage.Type == run.StageTypeImplement && stage.State == run.StageStateRunning {
		if _, err := s.cfg.RunRepo.TransitionStage(r.Context(), stageID, run.StageStateAwaitingScopeDecision, nil); err != nil {
			s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn,
				"scope-completeness park report: transition to awaiting_scope_decision failed",
				slog.String("run_id", runID.String()),
				slog.String("stage_id", stageID.String()),
				slog.String("error", err.Error()))
		}
	}
	if _, err := s.cfg.AuditRepo.AppendChained(r.Context(), appendParams); err != nil {
		s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn,
			"scope-completeness park report: append audit entry failed",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("error", err.Error()))
	}

	s.notifyStatusUpdate(r.Context(), runID, "scope_parked")
	s.writeJSON(w, r, http.StatusOK, pullRequestScopeParkResponse{
		StageID: stageID,
		Outcome: "scope_park",
		Branch:  pr.Branch,
		HeadSHA: pr.HeadSHA,
	})
}

// auditEntryForStage reports whether any of the entries belongs to the given
// stage. It is the stage-keyed idempotency primitive for outcomes that carry no
// head_sha to dedup on (fixup_no_changes, #856) — a single prior entry for the
// stage means the report already landed. Entries are sequence-ascending per
// ListForRunByCategory's contract; nil/mismatched StageIDs are skipped.
func auditEntryForStage(entries []*audit.Entry, stageID uuid.UUID) bool {
	for _, e := range entries {
		if e.StageID != nil && *e.StageID == stageID {
			return true
		}
	}
	return false
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

// closePRAfterGatingReject closes the dangling PR the runner just opened
// for an implement stage that a gating implement-review reject already
// failed category-B (#877). The PR artifact + pull_request_opened audit
// are intentionally left in place by the caller (the honest record that a
// PR was momentarily opened); this best-effort step closes the PR on
// GitHub so a rejected change isn't left with an open PR that will never
// merge, posts a short explanatory comment, and writes a
// pull_request_closed_after_review_reject audit entry.
//
// Fail-open throughout: GitHub unconfigured, a nil InstallationID, an
// unparseable repo, or a close error all WARN and return — the stage is
// already failed, so a failed close must never 500 the handler.
func (s *Server) closePRAfterGatingReject(r *http.Request, runID uuid.UUID,
	stage *run.Stage, pr *pullRequestBody, artifactID uuid.UUID) {
	ctx := r.Context()
	reason := ""
	if stage.FailureReason != nil {
		reason = *stage.FailureReason
	}

	if s.cfg.GitHub == nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"gating-reject PR close: GitHub client not configured; skipping close",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stage.ID.String()),
			slog.Int("pr_number", pr.PRNumber))
		return
	}

	runRow, err := s.cfg.RunRepo.GetRun(ctx, runID)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"gating-reject PR close: load run failed; skipping close",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stage.ID.String()),
			slog.String("error", err.Error()))
		return
	}
	if runRow.InstallationID == nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"gating-reject PR close: run has no installation id; skipping close",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stage.ID.String()),
			slog.Int("pr_number", pr.PRNumber))
		return
	}
	repo, err := parseRepoOwnerName(runRow.Repo)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"gating-reject PR close: unparseable repo; skipping close",
			slog.String("run_id", runID.String()),
			slog.String("repo", runRow.Repo),
			slog.String("error", err.Error()))
		return
	}

	// Best-effort explanatory comment first, so the close has context in the
	// thread even if a reader only sees the closed PR. PR comments use the
	// issues endpoint. A failure here WARNs and never blocks the close.
	comment := fmt.Sprintf(
		"Fishhawk closed this pull request: the implement stage was rejected by a gating agent review before the PR opened, so the change will not merge.\n\nReason: %s",
		reason)
	if _, cerr := s.cfg.GitHub.CreateIssueComment(ctx, *runRow.InstallationID, repo, pr.PRNumber, comment); cerr != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"gating-reject PR close: post explanatory comment failed; proceeding to close",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stage.ID.String()),
			slog.Int("pr_number", pr.PRNumber),
			slog.String("error", cerr.Error()))
	}

	if cerr := s.cfg.GitHub.ClosePullRequest(ctx, *runRow.InstallationID, repo, pr.PRNumber); cerr != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"gating-reject PR close: close pull request failed",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stage.ID.String()),
			slog.Int("pr_number", pr.PRNumber),
			slog.String("error", cerr.Error()))
		// Fall through: still record the audit attempt below? No — the close
		// did not happen, so do not claim it. Return without the audit entry.
		return
	}

	stageID := stage.ID
	systemActor := audit.ActorKind("system")
	auditPayload, _ := json.Marshal(map[string]any{
		"run_id":         runID.String(),
		"stage_id":       stageID.String(),
		"artifact_id":    artifactID.String(),
		"pr_number":      pr.PRNumber,
		"pr_url":         pr.PRURL,
		"failure_reason": reason,
	})
	if _, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     runID,
		StageID:   &stageID,
		Timestamp: time.Now().UTC(),
		Category:  "pull_request_closed_after_review_reject",
		ActorKind: &systemActor,
		Payload:   auditPayload,
	}); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"gating-reject PR close: append audit entry failed",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("error", err.Error()))
	}
}
