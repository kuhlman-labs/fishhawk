package server

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/signing"
)

// maxAcceptanceBundleBytes caps the acceptance evidence request body. Per
// ADR-049 decision refinement #5 the evidence blobs (logs, screenshots,
// traces) stay customer-side — only the structured verdict + per-criterion
// results + content_hash references to those blobs cross to Fishhawk — so
// 32 KB is well above any realistic payload, mirroring the deployment cap.
const maxAcceptanceBundleBytes = 32 * 1024

// Acceptance audit categories (E31.6 / #1534, ADR-049). Open-set strings —
// audit_entries.category has no CHECK, so these need no migration (only the
// artifacts kind CHECK was widened, by 0045). Kept in lockstep with the kinds
// issuecomment/status_template.go already renders (acceptance_dispatched /
// acceptance_outcome_recorded / acceptance_triage_decided, E31.3).
const (
	// CategoryAcceptanceDispatched records that the orchestrator dispatched an
	// acceptance stage. EMITTED by the orchestrator (emitAcceptanceDispatched),
	// not by this handler; the constant lives here so the outcome and the
	// dispatch categories are defined together.
	CategoryAcceptanceDispatched = "acceptance_dispatched"
	// CategoryAcceptanceOutcomeRecorded records the persisted acceptance
	// artifact + its settled verdict. Written by handleShipAcceptance on every
	// successful artifact persist.
	CategoryAcceptanceOutcomeRecorded = "acceptance_outcome_recorded"
)

// Acceptance verdict + failure-mode values. Server-local open-set strings
// (like the deploy audit categories) — the audit category has no DB CHECK and
// E31.8 triage consumes failure_mode from this package. verdict is the
// pass/fail axis; failure_mode splits a failure into error (crash/500/
// exception) vs assertion_fail (behaved-but-unexpected) for E31.8 triage.
const (
	acceptanceVerdictPassed = "passed"
	acceptanceVerdictFailed = "failed"

	acceptanceFailureError         = "error"
	acceptanceFailureAssertionFail = "assertion_fail"

	acceptanceResultPassed  = "passed"
	acceptanceResultFailed  = "failed"
	acceptanceResultSkipped = "skipped"
)

// acceptanceCriterionResult is one per-criterion evidence entry. ID is the
// plan-criterion join key (E31.1); Result is the pass/fail/skip disposition.
// The optional prose fields carry the validator's observed behavior.
type acceptanceCriterionResult struct {
	ID         string `json:"id"`
	Result     string `json:"result"`
	Observed   string `json:"observed,omitempty"`
	Expected   string `json:"expected,omitempty"`
	StepsTaken string `json:"steps_taken,omitempty"`
	// ExpectationBasis cites where the expectation came from (the criterion's
	// statement, the issue text, a spec section) so a failed assertion is
	// auditable against its source. Optional (E31.7 verdict shape, #1535).
	ExpectationBasis string `json:"expectation_basis,omitempty"`
	// ReproHandle is a re-run pointer for the observation — the command,
	// request, or script the validator used — so a human can reproduce the
	// evidence. Optional (E31.7 verdict shape, #1535).
	ReproHandle string `json:"repro_handle,omitempty"`
}

// acceptanceBody is the wire shape the acceptance validator (E31.7 runner) or
// an operator POSTs. It carries ADR-049's structured acceptance evidence.
// Stored verbatim as the artifact's content; v0 carries no schema_version
// because the field shape isn't yet schema-stable (mirroring deploymentBody).
type acceptanceBody struct {
	// Verdict is the settled disposition: passed | failed. Required.
	Verdict string `json:"verdict"`
	// FailureMode splits a failure for E31.8 triage: error (crash/500/
	// exception) | assertion_fail (behaved-but-unexpected). Required iff
	// verdict==failed; rejected when present on a pass.
	FailureMode string `json:"failure_mode,omitempty"`
	// Criteria carries one result per plan acceptance criterion, keyed by the
	// criterion id (the E31.1 join key). Optional — a verdict can settle
	// before per-criterion evidence is itemized.
	Criteria []acceptanceCriterionResult `json:"criteria,omitempty"`
	// TargetURL is the running instance the validator drove, when declared.
	// Optional; http(s)-prefixed when present.
	TargetURL string `json:"target_url,omitempty"`
	// EvidenceHashes references the customer-side evidence blobs by content
	// hash (ADR-049 #5 default residency customer-side). Optional.
	EvidenceHashes []string `json:"evidence_hashes,omitempty"`
}

// validate returns a human-readable error if any field is missing or
// malformed. An acceptance record is the governance trail of an independent
// validation, so a 400 here means the producer shipped the wrong shape.
func (a *acceptanceBody) validate() error {
	switch a.Verdict {
	case acceptanceVerdictPassed:
		if a.FailureMode != "" {
			return fmt.Errorf("failure_mode must be omitted on a passed verdict, got %q", a.FailureMode)
		}
	case acceptanceVerdictFailed:
		switch a.FailureMode {
		case acceptanceFailureError, acceptanceFailureAssertionFail:
			// ok
		case "":
			return errors.New("failure_mode is required when verdict is failed (error | assertion_fail)")
		default:
			return fmt.Errorf("failure_mode must be error or assertion_fail, got %q", a.FailureMode)
		}
	case "":
		return errors.New("verdict is required")
	default:
		return fmt.Errorf("verdict must be passed or failed, got %q", a.Verdict)
	}
	for i, c := range a.Criteria {
		if c.ID == "" {
			return fmt.Errorf("criteria[%d].id is required (the plan-criterion join key)", i)
		}
		switch c.Result {
		case acceptanceResultPassed, acceptanceResultFailed, acceptanceResultSkipped:
			// ok
		default:
			return fmt.Errorf("criteria[%d].result must be passed/failed/skipped, got %q", i, c.Result)
		}
	}
	if a.TargetURL != "" && !strings.HasPrefix(a.TargetURL, "http") {
		return fmt.Errorf("target_url must be an http(s) URL when set, got %q", a.TargetURL)
	}
	return nil
}

// acceptanceCriteriaTally returns the passed count and the total, used both
// for the audit payload (E31.8 carry-through) and the issue-comment render
// tally (criteria_passed / criteria_total).
func acceptanceCriteriaTally(criteria []acceptanceCriterionResult) (passed, failed, skipped, total int) {
	for _, c := range criteria {
		switch c.Result {
		case acceptanceResultPassed:
			passed++
		case acceptanceResultFailed:
			failed++
		case acceptanceResultSkipped:
			skipped++
		}
	}
	return passed, failed, skipped, len(criteria)
}

// acceptanceOutcomeLabel maps the wire verdict to the issue-comment render
// vocabulary (accepted | rejected) — the `outcome` field
// issuecomment/status_template.go's renderAcceptanceOutcomeLine reads.
func acceptanceOutcomeLabel(verdict string) string {
	if verdict == acceptanceVerdictPassed {
		return "accepted"
	}
	return "rejected"
}

// handleShipAcceptance implements POST /v0/runs/{run_id}/acceptance?stage_id=...
//
// Records ADR-049's signed acceptance-evidence artifact and its governance
// trail. Modeled on handleShipDeployment: dual-auth (Ed25519
// X-Fishhawk-Signature runner path OR bearer token with write:runs scope),
// idempotent on (stage_id, content_hash), and chained-audit recording. It
// persists the acceptance artifact (artifact.KindAcceptance), writes an
// acceptance_outcome_recorded audit entry carrying the verdict + failure_mode
// (the E31.8 error-vs-assertion_fail carry-through), and refreshes the run's
// living-anchor comment. NO stage-state transition happens here: the stage
// settles through the ordinary agent trace-bundle path (E31.2 landed
// acceptance with no new states); failure routing/triage is E31.8's scope.
func (s *Server) handleShipAcceptance(w http.ResponseWriter, r *http.Request) {
	if s.cfg.SigningRepo == nil || s.cfg.ArtifactRepo == nil ||
		s.cfg.AuditRepo == nil || s.cfg.RunRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "acceptance_upload_unconfigured",
			"acceptance upload requires signing, artifact, audit, and run repositories", nil)
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
	// An acceptance evidence artifact is scoped to an acceptance stage (ADR-049
	// / #1531). Without this guard a valid run signer or write:runs bearer could
	// pin a signed acceptance record + acceptance audit chain onto a plan/
	// implement/review/deploy stage. Reject any non-acceptance stage before any
	// persistence, mirroring the deploy-stage guard.
	if stage.Type != run.StageTypeAcceptance {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"acceptance artifacts may only be attached to an acceptance stage",
			map[string]any{"stage_id": stageID.String(), "stage_type": string(stage.Type)})
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxAcceptanceBundleBytes+1))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"could not read request body", map[string]any{"error": err.Error()})
		return
	}
	if len(body) > maxAcceptanceBundleBytes {
		s.writeError(w, r, http.StatusRequestEntityTooLarge, "body_too_large",
			"acceptance body exceeds size cap",
			map[string]any{"limit_bytes": maxAcceptanceBundleBytes})
		return
	}

	authMethod, actorKind, actorSubject, ok := s.authorizeAcceptance(w, r, runID, body)
	if !ok {
		return
	}

	var acc acceptanceBody
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&acc); err != nil {
		s.writeError(w, r, http.StatusBadRequest, "acceptance_invalid",
			"acceptance body could not be decoded",
			map[string]any{"error": err.Error()})
		return
	}
	// Reject trailing data after the single acceptance object. Without this an
	// EOF-unverified Decode would accept the first object of a concatenated body
	// (e.g. {"verdict":"passed"}{"verdict":"failed",...}) while the stored
	// artifact bytes are not the single AcceptanceArtifactBody object documented.
	if dec.More() {
		s.writeError(w, r, http.StatusBadRequest, "acceptance_invalid",
			"acceptance body must contain a single JSON object", nil)
		return
	}
	if err := acc.validate(); err != nil {
		s.writeError(w, r, http.StatusBadRequest, "acceptance_invalid",
			"acceptance body missing or malformed fields",
			map[string]any{"error": err.Error()})
		return
	}

	contentHash := sha256Hex(body)
	passed, failed, skipped, total := acceptanceCriteriaTally(acc.Criteria)

	// buildOutcomePayload renders the acceptance_outcome_recorded payload. The
	// `outcome`/`criteria_passed`/`criteria_total` tags are the issue-comment
	// render contract (issuecomment/status_template.go); verdict/failure_mode +
	// the per-result counts are the E31.8 triage carry-through.
	buildOutcomePayload := func(artifactID string) []byte {
		p, _ := json.Marshal(map[string]any{
			"run_id":           runID.String(),
			"stage_id":         stageID.String(),
			"artifact_id":      artifactID,
			"content_hash":     contentHash,
			"verdict":          acc.Verdict,
			"failure_mode":     acc.FailureMode,
			"outcome":          acceptanceOutcomeLabel(acc.Verdict),
			"criteria_passed":  passed,
			"criteria_failed":  failed,
			"criteria_skipped": skipped,
			"criteria_total":   total,
			"target_url":       acc.TargetURL,
			"auth_method":      authMethod,
		})
		return p
	}

	// Idempotency: dedup on (stage_id, content_hash). A re-delivery of the same
	// acceptance record returns the existing artifact rather than creating a
	// duplicate (and writing a second audit entry).
	if existing, err := s.cfg.ArtifactRepo.GetByHash(r.Context(), stageID, contentHash); err == nil {
		// Self-heal the chained governance audit entry (#1396). A prior attempt
		// may have persisted the artifact (Create succeeded) but failed its
		// acceptance_outcome_recorded append (AppendChained failed → 500); this
		// identical retry short-circuits here. Verify the outcome entry exists
		// for this artifact and append it idempotently if missing, so a
		// retry-after-partial-failure ends with BOTH the artifact and its
		// governance record. The helper fails closed on a read error.
		if _, herr := s.ensureGovernanceAuditEntry(r.Context(), runID,
			CategoryAcceptanceOutcomeRecorded, existing.ID.String(), func() error {
				_, aerr := s.cfg.AuditRepo.AppendChained(r.Context(), audit.ChainAppendParams{
					RunID:        runID,
					StageID:      &stageID,
					Timestamp:    time.Now().UTC(),
					Category:     CategoryAcceptanceOutcomeRecorded,
					ActorKind:    &actorKind,
					ActorSubject: actorSubject,
					Payload:      buildOutcomePayload(existing.ID.String()),
				})
				return aerr
			}); herr != nil {
			s.writeError(w, r, http.StatusInternalServerError, "internal_error",
				"heal governance audit entry failed", map[string]any{"error": herr.Error()})
			return
		}
		s.writeJSON(w, r, http.StatusOK, acceptanceResponse{
			ID:          existing.ID,
			StageID:     existing.StageID,
			ContentHash: existing.ContentHash,
			Verdict:     acc.Verdict,
			FailureMode: acc.FailureMode,
			Idempotent:  true,
		})
		return
	} else if !errors.Is(err, artifact.ErrNotFound) {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"check existing acceptance failed", map[string]any{"error": err.Error()})
		return
	}

	created, err := s.cfg.ArtifactRepo.Create(r.Context(), artifact.CreateParams{
		StageID:     stageID,
		Kind:        artifact.KindAcceptance,
		Content:     json.RawMessage(body),
		ContentHash: contentHash,
		// SchemaVersion intentionally nil for v0 — graduate to acceptance_v1
		// once the field shape settles (mirroring deployment).
	})
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"create acceptance artifact failed", map[string]any{"error": err.Error()})
		return
	}

	if _, err := s.cfg.AuditRepo.AppendChained(r.Context(), audit.ChainAppendParams{
		RunID:        runID,
		StageID:      &stageID,
		Timestamp:    time.Now().UTC(),
		Category:     CategoryAcceptanceOutcomeRecorded,
		ActorKind:    &actorKind,
		ActorSubject: actorSubject,
		Payload:      buildOutcomePayload(created.ID.String()),
	}); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"append audit entry failed", map[string]any{"error": err.Error()})
		return
	}

	// Refresh the run's sticky living-anchor comment so the acceptance outcome
	// surfaces on the issue timeline (the acceptance audit categories render
	// data-drivenly through issuecomment's activityCategories set).
	s.notifyStatusUpdate(r.Context(), runID, "acceptance_recorded")

	s.writeJSON(w, r, http.StatusCreated, acceptanceResponse{
		ID:          created.ID,
		StageID:     created.StageID,
		ContentHash: created.ContentHash,
		Verdict:     acc.Verdict,
		FailureMode: acc.FailureMode,
		Idempotent:  false,
	})
}

// authorizeAcceptance resolves the request's auth method + actor, mirroring
// authorizeDeployment's dual-auth block: an Ed25519 X-Fishhawk-Signature over
// sha256(body) (runner path — per ADR-050 decision #2 the acceptance agent
// ships via signature auth with NO MCP token) OR a bearer token with the
// existing write:runs scope (operator path). Deliberately NO new scope:
// deploy's write:deploy was a deploy-gate-specific governance tightening;
// acceptance evidence is advisory and adding a scope would trigger the
// Auth-change checklist's impact inventory for zero benefit. On failure it has
// already written the error response and returns ok=false.
func (s *Server) authorizeAcceptance(w http.ResponseWriter, r *http.Request, runID uuid.UUID, body []byte) (authMethod string, actorKind audit.ActorKind, actorSubject *string, ok bool) {
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
	case !id.IsAnonymous() && hasScope(id, "write:runs"):
		// ADR-040 D4 (#1027): kind from the token subject — user or agent. The
		// ed25519 signature branch above is the runner path and is NOT
		// scope-gated. No write:acceptance scope (see doc comment).
		subj := id.Subject
		return "bearer", actorKindForSubject(id.Subject), &subj, true
	default:
		s.writeError(w, r, http.StatusUnauthorized, "signature_or_bearer_required",
			"request must include X-Fishhawk-Signature or an authenticated bearer token with write:runs scope", nil)
		return "", "", nil, false
	}
}

// resolveAcceptanceTargetURL is the single named wiring seam for the
// acceptance stage's target-instance URL (ADR-050 decision #1), activated by
// the E31.4/#1532 egress-allowance grammar: it returns the acceptance
// stage's first spec-declared egress target host. The value is a host or
// host:port, deliberately rendered verbatim — the grammar declares hosts,
// not URLs, and fabricating a scheme here would assert something the spec
// does not say. A spec with no egress block (a pre-1.3 spec, or one relying
// on the documented interim posture) yields the empty string and
// buildAcceptance renders its explicit not-declared line.
func (s *Server) resolveAcceptanceTargetURL(ctx context.Context, runRow *run.Run) string {
	hosts := s.resolveAcceptanceEgressTargetHosts(ctx, runRow)
	if len(hosts) == 0 {
		return ""
	}
	return hosts[0]
}

// resolveAcceptanceEgressTargetHosts returns ALL of the acceptance stage's
// spec-declared egress target hosts (the E31.4/#1532 grammar), in declaration
// order. The full list — not just the first host the prompt-text seam renders
// — is served on the acceptance-stage prompt response as egress_target_hosts:
// the runner's ADR-050 egress-proxy allow-list input (E31.7 / #1535). nil for
// a spec with no egress block, so the response field stays omitted.
func (s *Server) resolveAcceptanceEgressTargetHosts(ctx context.Context, runRow *run.Run) []string {
	st, ok := s.resolveAcceptanceStageSpec(ctx, runRow)
	if !ok || st.Egress == nil || len(st.Egress.TargetHosts) == 0 {
		return nil
	}
	return st.Egress.TargetHosts
}

// acceptanceCriteriaIDsFromPlan extracts the approved plan's
// verification.acceptance_criteria ids, in plan order. Served on the
// acceptance-stage prompt response as acceptance_criteria_ids so the runner
// can validate the shipped verdict's criteria[].id join keys against the
// served set (E31.7 / #1535). nil for a nil plan or an empty criteria set,
// so the response field stays omitted.
func acceptanceCriteriaIDsFromPlan(p *plan.Plan) []string {
	if p == nil || len(p.Verification.AcceptanceCriteria) == 0 {
		return nil
	}
	ids := make([]string, 0, len(p.Verification.AcceptanceCriteria))
	for _, c := range p.Verification.AcceptanceCriteria {
		ids = append(ids, c.ID)
	}
	return ids
}

// acceptanceResponse echoes the persisted artifact's identity back to the
// caller. Verdict + failure_mode are surfaced explicitly (even though they
// live in the artifact body) as the most operator-useful correlation fields.
type acceptanceResponse struct {
	ID          uuid.UUID `json:"id"`
	StageID     uuid.UUID `json:"stage_id"`
	ContentHash string    `json:"content_hash"`
	Verdict     string    `json:"verdict"`
	FailureMode string    `json:"failure_mode,omitempty"`
	Idempotent  bool      `json:"idempotent"`
}
