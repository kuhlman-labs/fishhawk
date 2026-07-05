package server

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/agenteval"
	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/auditcomplete"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
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
	// CategoryAcceptanceSkippedOutOfScope records that the orchestrator
	// AUTO-TERMINATED an acceptance stage (E38.3 / #1657) because the approved
	// plan declared verification.out_of_scope with zero acceptance_criteria —
	// there is no observable criterion for a validator to check. EMITTED by the
	// orchestrator (emitAcceptanceSkippedOutOfScope), which uses the raw string
	// literal at its emit site (matching the acceptance_dispatched convention);
	// this exported const is the single owner of the value. READ by auditcomplete
	// (which exempts the marked stage from the trace-required rule) and by the MCP
	// next_actions surface (which labels the succeeded_acceptance_skipped_out_of_scope
	// state). Keep the literal and this const byte-identical. Open-set string —
	// audit_entries.category has no CHECK, so no migration.
	CategoryAcceptanceSkippedOutOfScope = "acceptance_skipped_out_of_scope"
	// CategoryAcceptanceOutcomeRecorded records the persisted acceptance
	// artifact + its settled verdict. Written by handleShipAcceptance on every
	// successful artifact persist.
	CategoryAcceptanceOutcomeRecorded = "acceptance_outcome_recorded"
	// CategoryAcceptanceTriageDecided records the deterministic triage of a
	// failed acceptance verdict (E31.8 / #1536, ADR-049 decision #2). One
	// chained entry per triage, written AFTER acting so the disposition records
	// what actually happened. The class/disposition/criterion_ids payload tags
	// match the render contract issuecomment/status_template.go already ships
	// (renderAcceptanceTriageLine, E31.3) and the class-3 entry keyed by
	// criterion_ids is the durable per-criterion disposition record E31.11
	// consumes. Open-set string — audit_entries.category has no CHECK, so no
	// migration (same posture as acceptance_outcome_recorded).
	CategoryAcceptanceTriageDecided = "acceptance_triage_decided"
	// CategoryAcceptanceReopened records an operator-gated re-open of an
	// acceptance stage that settled `succeeded` with NO
	// acceptance_outcome_recorded verdict for that stage (E31.16 / #1567).
	// Written by the retry handler's acceptance-reopen branch
	// (server/retry.go) before the orchestrator handoff; no notifier ping of
	// its own — the status refresh rides notifyStatusUpdate. Open-set string
	// (audit_entries.category has no CHECK), so no migration.
	CategoryAcceptanceReopened = "acceptance_reopened"
)

// Acceptance triage class values (E31.8). Strings, matching
// renderAcceptanceTriageLine's "class-%s" contract:
//   - class 1: the code attempts the behavior and objectively fails
//     (failure_mode=error, or assertion_fail where every failed criterion is
//     explicit-source) → bounded fix-up pass.
//   - class 2: assertion_fail where no criterion failed but ≥1 was skipped —
//     validation could not complete (environment/flake) → re-open + re-run.
//   - class 3: a failed criterion is inferred-source or unresolvable against
//     the plan (bad/ambiguous criterion) → page the human, no transition.
//   - class 4: unitemized or provenance-ungroundable failure (works-as-planned,
//     disputed) → page the human, no transition.
//   - class 5: all-skip / externally-unvalidatable — every skipped criterion is
//     a posture-A can't-exhibit skip carrying expectation_basis; the trigger
//     requires an external event the default-deny egress sandbox cannot produce,
//     so retry is deterministically futile → terminal page, no state transition
//     (split off from class 2 so the acceptance stage stays succeeded/terminal
//     and fishhawk_audit_complete can clear; #1671).
const (
	acceptanceClass1 = "1"
	acceptanceClass2 = "2"
	acceptanceClass3 = "3"
	acceptanceClass4 = "4"
	acceptanceClass5 = "5"
)

// Acceptance triage disposition vocabulary (E31.8). The tags
// decodeAcceptanceActivity reads for the issue-comment render, and the tokens
// issuecomment/ping.go's page-class gate keys the must_page_human ping on.
const (
	acceptanceDispositionFixupDispatched  = "fixup_dispatched"
	acceptanceDispositionRetryDispatched  = "retry_dispatched"
	acceptanceDispositionPaged            = "paged"
	acceptanceDispositionRerunBudget      = "rerun_budget_exhausted"
	acceptanceDispositionFixupUnavailable = "fixup_unavailable_paged"
	acceptanceDispositionRetryUnavailable = "retry_unavailable_paged"
	acceptanceDispositionUnsettled        = "unsettled_paged"
	// acceptanceDispositionUnvalidatable is the terminal, non-re-opening paged
	// disposition for a class-5 all-skip externally-unvalidatable verdict
	// (#1671): the acceptance stage stays succeeded so fishhawk_audit_complete
	// clears and the operator arbitrates via the normal gate. Re-declared
	// verbatim in backend/internal/issuecomment/ping.go (string literal) and
	// backend/cmd/fishhawk-mcp/next_actions.go (const) — the three copies are
	// pinned byte-for-byte by a per-package assertion.
	acceptanceDispositionUnvalidatable = "externally_unvalidatable_paged"
)

// defaultMaxAcceptanceReruns bounds the number of auto-routed acceptance
// triage decisions (fixup_dispatched | retry_dispatched) per run before the
// disposition degrades to rerun_budget_exhausted (paged, no action) so
// non-convergence always lands on the human. Package const, no new env var:
// #1536 bounds re-runs at 1–2.
const defaultMaxAcceptanceReruns = 2

// acceptanceTriageSystemSubject is the token-less system identity the class-1
// fix-up routes under: non-anonymous with TokenID=="" passes
// identityHasGateScope (the shape fixupStageAs admits for in-process callers).
const acceptanceTriageSystemSubject = "system:acceptance-triage"

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
	// before per-criterion evidence is itemized. Decoded as a RawMessage so
	// validate() can losslessly coerce the historical object-keyed variant (a
	// JSON object keyed by criterion id) to the schema-required flat array with
	// each key folded into the element id (the #1574 class); see
	// coerceAcceptanceCriteria. The normalized flat slice lands in
	// normalizedCriteria. Moving off the typed slice means the top-level
	// DisallowUnknownFields decoder no longer descends into criteria elements —
	// coerceAcceptanceCriteria's array path re-applies that strictness.
	Criteria json.RawMessage `json:"criteria,omitempty"`
	// TargetURL is the running instance the validator drove, when declared.
	// Optional; a schemeless host[:port] is coerced to an http:// URL by
	// validate(), and any foreign scheme fails closed (the #1574 class).
	TargetURL string `json:"target_url,omitempty"`
	// EvidenceHashes references the customer-side evidence blobs by content
	// hash (ADR-049 #5 default residency customer-side). Optional. Decoded as
	// a RawMessage so validate() can losslessly coerce the historical
	// string-valued object-map variant to its sorted values (the #1574 class);
	// see coerceEvidenceHashes. The normalized flat slice lands in
	// normalizedEvidenceHashes.
	EvidenceHashes json.RawMessage `json:"evidence_hashes,omitempty"`
	// Notes is a declared home for the agent's free-text overflow (#1567):
	// a benign top-level remark that would otherwise fail closed against
	// DisallowUnknownFields. Free text, no validate() rule; stored verbatim
	// in the artifact and covered by the existing whole-verdict redaction on
	// the runner side. The wire twin of acceptanceVerdict.Notes.
	Notes string `json:"notes,omitempty"`

	// normalizedEvidenceHashes is the coerced/validated flat slice, populated
	// by validate() from EvidenceHashes. Unexported (no json tag) so it never
	// marshals into the stored artifact; buildOutcomePayload records it as the
	// canonical shape.
	normalizedEvidenceHashes []string

	// normalizedCriteria is the coerced/validated flat typed slice, populated by
	// validate() from Criteria (an object-keyed criteria field folds into it,
	// each key written into the element id, sorted). Unexported (no json tag) so
	// it never marshals into the stored artifact; every downstream consumer
	// (tally, triage classifier, plan-review-miss + concern synthesis) reads it
	// instead of the raw Criteria field.
	normalizedCriteria []acceptanceCriterionResult
}

// validate returns a human-readable error if any field is missing or
// malformed. An acceptance record is the governance trail of an independent
// validation, so a 400 here means the producer shipped the wrong shape.
//
// It also applies the two lossless coercions of the #1574 class BEFORE the
// fail-closed rejections, mutating the receiver so the recorded outcome uses
// the normalized shape: a string-valued object-map evidence_hashes collapses
// to its sorted values, and a schemeless host[:port] target_url gains an
// http:// prefix. Anything lossy (a non-string/nested map value, a scalar
// evidence_hashes, or a foreign target_url scheme) still fails closed. The
// coercion twin lives in the runner (validateAcceptanceVerdict) — the two
// must stay behavior-identical or a runner-accepted verdict could be
// backend-rejected on ship. logger (nil-tolerant) receives a WARN per
// coercion so the shape drift is observable.
func (a *acceptanceBody) validate(ctx context.Context, logger *slog.Logger) error {
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
	// Coerce criteria before the per-criterion fail-closed checks: an object
	// keyed by criterion id folds into the schema-required flat array with each
	// key written into the element id (the #1574 class); a non-object keyed
	// value, a key/element-id conflict, or a scalar fails closed. A flat array
	// passes through (strict-decoded so an unknown element field still fails
	// closed). The normalized slice is what every downstream consumer reads.
	criteria, coercedCriteria, err := coerceAcceptanceCriteria(a.Criteria)
	if err != nil {
		return err
	}
	a.normalizedCriteria = criteria
	for i, c := range criteria {
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
	if coercedCriteria && logger != nil {
		logger.LogAttrs(ctx, slog.LevelWarn,
			"acceptance verdict: coerced object-keyed criteria to a flat array",
			slog.Int("count", len(criteria)))
	}
	// Coerce evidence_hashes before the fail-closed reject: a string-valued
	// object map collapses to its sorted values (lossless); a non-string/
	// nested value or a scalar fails closed. The normalized slice is what the
	// outcome payload records.
	hashes, coercedHashes, err := coerceEvidenceHashes(a.EvidenceHashes)
	if err != nil {
		return err
	}
	a.normalizedEvidenceHashes = hashes
	if coercedHashes && logger != nil {
		logger.LogAttrs(ctx, slog.LevelWarn,
			"acceptance verdict: coerced string-valued object-map evidence_hashes to sorted values",
			slog.Int("count", len(hashes)))
	}

	// Coerce target_url before the fail-closed reject: a schemeless host[:port]
	// gains an http:// prefix; a foreign scheme (anything with "://" that is
	// not exactly http:// or https://) fails closed.
	coercedURL, err := coerceAcceptanceTargetURL(&a.TargetURL)
	if err != nil {
		return err
	}
	if coercedURL && logger != nil {
		logger.LogAttrs(ctx, slog.LevelWarn,
			"acceptance verdict: coerced schemeless target_url to an http:// URL",
			slog.String("target_url", a.TargetURL))
	}
	return nil
}

// coerceEvidenceHashes normalizes the acceptance verdict's evidence_hashes
// field. It returns the flat slice of hash strings, whether a coercion
// occurred, and an error on any lossy shape. The accepted inputs:
//   - absent / null / empty → nil, no coercion.
//   - a JSON array of strings → the array verbatim, no coercion (a non-string
//     element fails closed, matching the strict prior decode).
//   - a string-valued JSON object map (the #1574 variant) → its values,
//     SORTED, marked coerced; a non-string or nested value fails closed.
//   - anything else (a scalar) → fails closed.
func coerceEvidenceHashes(raw json.RawMessage) ([]string, bool, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil, false, nil
	}
	switch trimmed[0] {
	case '[':
		var arr []string
		if err := json.Unmarshal(trimmed, &arr); err != nil {
			return nil, false, fmt.Errorf("evidence_hashes must be a flat array of strings: %w", err)
		}
		return arr, false, nil
	case '{':
		var m map[string]json.RawMessage
		if err := json.Unmarshal(trimmed, &m); err != nil {
			return nil, false, fmt.Errorf("evidence_hashes object could not be decoded: %w", err)
		}
		vals := make([]string, 0, len(m))
		for _, rv := range m {
			var s string
			if err := json.Unmarshal(rv, &s); err != nil {
				return nil, false, errors.New("evidence_hashes object-map values must all be strings (lossy coercion refused)")
			}
			vals = append(vals, s)
		}
		sort.Strings(vals)
		return vals, true, nil
	default:
		return nil, false, errors.New("evidence_hashes must be a flat array of strings or a string-valued object map")
	}
}

// coerceAcceptanceCriteria normalizes the acceptance verdict's criteria field.
// It returns the flat typed slice, whether a coercion occurred, and an error on
// any lossy or invalid shape. The twin of the runner coerceAcceptanceCriteria
// (runner/cmd/fishhawk-runner/acceptance.go) — the two must stay identical or a
// runner-accepted verdict could be backend-rejected on ship. Accepted:
//   - absent / null / empty → nil, no coercion.
//   - a JSON array → STRICT-decoded (DisallowUnknownFields) into the typed slice
//     verbatim, no coercion. The strict decode re-applies the unknown-field
//     rejection the top-level decoder no longer performs on this now-RawMessage
//     field (an unknown element field fails closed).
//   - a JSON object keyed by criterion id (the #1574 variant) → each value
//     strict-decoded into an element with the object key folded into its id,
//     the elements SORTED by id, marked coerced. A value that is not an object,
//     or a value carrying a non-empty explicit id that conflicts with its key,
//     fails closed.
//   - anything else (a scalar) → fails closed.
func coerceAcceptanceCriteria(raw json.RawMessage) ([]acceptanceCriterionResult, bool, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil, false, nil
	}
	switch trimmed[0] {
	case '[':
		arr, err := strictDecodeCriteriaArray(trimmed)
		if err != nil {
			return nil, false, err
		}
		return arr, false, nil
	case '{':
		var m map[string]json.RawMessage
		if err := json.Unmarshal(trimmed, &m); err != nil {
			return nil, false, fmt.Errorf("criteria object could not be decoded: %w", err)
		}
		out := make([]acceptanceCriterionResult, 0, len(m))
		for key, rv := range m {
			vt := bytes.TrimSpace(rv)
			if len(vt) == 0 || vt[0] != '{' {
				return nil, false, fmt.Errorf("criteria object value for %q must be an object (lossy coercion refused)", key)
			}
			c, err := strictDecodeCriterion(vt)
			if err != nil {
				return nil, false, err
			}
			if c.ID != "" && c.ID != key {
				return nil, false, fmt.Errorf("criteria object key %q conflicts with element id %q", key, c.ID)
			}
			c.ID = key
			out = append(out, c)
		}
		sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
		return out, true, nil
	default:
		return nil, false, errors.New("criteria must be a flat array or an object keyed by criterion id")
	}
}

// strictDecodeCriteriaArray decodes a criteria JSON array with
// DisallowUnknownFields so an unknown field inside an element fails closed —
// the strictness the top-level RawMessage field no longer enforces.
func strictDecodeCriteriaArray(raw json.RawMessage) ([]acceptanceCriterionResult, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var arr []acceptanceCriterionResult
	if err := dec.Decode(&arr); err != nil {
		return nil, fmt.Errorf("criteria array could not be decoded: %w", err)
	}
	return arr, nil
}

// strictDecodeCriterion decodes a single criteria object value with
// DisallowUnknownFields (the object-keyed variant path).
func strictDecodeCriterion(raw json.RawMessage) (acceptanceCriterionResult, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var c acceptanceCriterionResult
	if err := dec.Decode(&c); err != nil {
		return acceptanceCriterionResult{}, fmt.Errorf("criteria object value could not be decoded: %w", err)
	}
	return c, nil
}

// coerceAcceptanceTargetURL normalizes the verdict's target_url in place. A
// schemeless host[:port] gains an http:// prefix (coerced=true). A value
// already carrying an exact http:// or https:// prefix passes through
// unchanged. ANY other value containing "://" (a foreign or near-miss scheme
// such as ftp://, httpx://, or http+unix://) fails closed — the check matches
// ONLY the two exact prefixes, never HasPrefix("http"), so a scheme a naive
// prefix test would wrongly admit is rejected.
func coerceAcceptanceTargetURL(target *string) (bool, error) {
	v := *target
	if v == "" {
		return false, nil
	}
	if strings.HasPrefix(v, "http://") || strings.HasPrefix(v, "https://") {
		return false, nil
	}
	if strings.Contains(v, "://") {
		return false, fmt.Errorf("target_url must be an http(s) URL when set, got %q", v)
	}
	*target = "http://" + v
	return true, nil
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
	if err := acc.validate(r.Context(), s.cfg.Logger); err != nil {
		s.writeError(w, r, http.StatusBadRequest, "acceptance_invalid",
			"acceptance body missing or malformed fields",
			map[string]any{"error": err.Error()})
		return
	}

	contentHash := sha256Hex(body)
	passed, failed, skipped, total := acceptanceCriteriaTally(acc.normalizedCriteria)

	// Bind the verdict to the head the acceptance stage ACTUALLY validated
	// (#1682, binding condition 2): the run's newest recorded head at the
	// moment the stage was DISPATCHED, not the run's latest head at
	// verdict-record time. A post-dispatch fixup_pushed / child_pushed must NOT
	// re-bind this verdict — otherwise Option C's head comparison (retry.go)
	// would see recorded==current for a verdict the fixup already invalidated.
	// Empty ("", false) for a pre-anchor / dispatch-less ship — Option C then
	// fails closed to today's 422 for that entry.
	validatedHead, _ := s.acceptanceValidatedHeadSHA(r.Context(), runID, stageID)

	// buildOutcomePayload renders the acceptance_outcome_recorded payload. The
	// `outcome`/`criteria_passed`/`criteria_total` tags are the issue-comment
	// render contract (issuecomment/status_template.go); verdict/failure_mode +
	// the per-result counts are the E31.8 triage carry-through. head_sha is the
	// #1682 additive field (internal chained-audit map — no docs/spec schema
	// change); older entries simply lack it and Option C fails closed on absence.
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
			"evidence_hashes":  acc.normalizedEvidenceHashes,
			"auth_method":      authMethod,
			"head_sha":         validatedHead,
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

	// E31.8 (#1536): route a freshly persisted verdict:failed artifact through
	// deterministic triage. ONLY on this fresh-create path — the idempotent
	// replay branch above returns before here, so a re-delivered identical
	// verdict cannot double-route. Best-effort relative to the ship: any
	// internal error WARN-logs inside and never unwinds the 201 / artifact /
	// outcome audit already committed.
	if acc.Verdict == acceptanceVerdictFailed {
		s.triageAcceptanceFailure(r.Context(), runID, stage, acc, created.ID.String())
	}

	s.writeJSON(w, r, http.StatusCreated, acceptanceResponse{
		ID:          created.ID,
		StageID:     created.StageID,
		ContentHash: created.ContentHash,
		Verdict:     acc.Verdict,
		FailureMode: acc.FailureMode,
		Idempotent:  false,
	})
}

// reopenAcceptanceOnFixupPush invalidates a stale acceptance verdict when a
// fix-up push lands a NEW head AFTER the acceptance stage already settled
// (#1682, Option A — the automatic in-band defense). It locates the run's
// acceptance stage; if that stage is StageStateSucceeded AND carries a recorded
// acceptance_outcome_recorded verdict, it re-opens the stage (succeeded →
// pending) via run.ReopenAcceptanceStage and appends an acceptance_reopened
// invalidation audit entry — the SAME kind the operator-gated #1567 re-open
// uses (no new issue-comment surface). next_actions then routes to
// acceptance_pending so the operator re-dispatches acceptance against the final
// commit.
//
// No-op (everything untouched) when: the run has no acceptance stage; the
// acceptance stage is not succeeded (a PRE-acceptance fix-up must not reopen
// anything); or the succeeded stage has NO recorded verdict (that outcome-less
// hole is the retry handler's #1567 operator-reopen path, not this one).
// Idempotent against a re-delivered fixup_pushed: the caller's (stage_id,
// head_sha) dedup short-circuits before this runs, and ReopenAcceptanceStage
// refuses a non-succeeded (already-pending) stage, so a second delivery cannot
// double-reopen. Best-effort: every failure WARN-logs and never unwinds the
// fix-up push success.
func (s *Server) reopenAcceptanceOnFixupPush(ctx context.Context, runID uuid.UUID, newHeadSHA string) {
	if s.cfg.RunRepo == nil || s.cfg.AuditRepo == nil {
		return
	}
	stages, err := s.cfg.RunRepo.ListStagesForRun(ctx, runID)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"fixup acceptance invalidation: list stages failed; skipping reopen",
			slog.String("run_id", runID.String()), slog.String("error", err.Error()))
		return
	}
	var acceptance *run.Stage
	for _, st := range stages {
		if st.Type == run.StageTypeAcceptance {
			acceptance = st
			break
		}
	}
	// A pre-acceptance fix-up (no acceptance stage, or one not yet succeeded)
	// leaves the gate alone — only a SETTLED acceptance stage can carry a stale
	// verdict to invalidate.
	if acceptance == nil || acceptance.State != run.StageStateSucceeded {
		return
	}
	hasVerdict, err := s.acceptanceStageHasVerdict(ctx, runID, acceptance.ID)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"fixup acceptance invalidation: verdict lookup failed; skipping reopen",
			slog.String("run_id", runID.String()),
			slog.String("acceptance_stage_id", acceptance.ID.String()),
			slog.String("error", err.Error()))
		return
	}
	if !hasVerdict {
		// Outcome-less succeeded acceptance stage — the #1567 operator-reopen
		// path owns it, not a fix-up invalidation.
		return
	}

	dec, err := run.ReopenAcceptanceStage(ctx, s.cfg.RunRepo, acceptance.ID)
	if err != nil {
		// A concurrent/re-delivered reopen (already pending) or a terminal run
		// refuses here — benign; the stage is already (being) re-opened.
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"fixup acceptance invalidation: reopen refused; leaving stage as-is",
			slog.String("run_id", runID.String()),
			slog.String("acceptance_stage_id", acceptance.ID.String()),
			slog.String("error", err.Error()))
		return
	}

	subject := "system:fixup-acceptance-invalidation"
	actorKind := audit.ActorSystem
	payload, _ := json.Marshal(map[string]any{
		"stage_id":    dec.Stage.ID.String(),
		"prior_state": string(dec.PriorState),
		"head_sha":    newHeadSHA,
		"reason":      "a fix-up push landed a new head after the acceptance stage settled; the prior acceptance verdict is invalidated and the stage re-opened for re-validation against the final commit",
	})
	if _, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:        runID,
		StageID:      &dec.Stage.ID,
		Timestamp:    time.Now().UTC(),
		Category:     CategoryAcceptanceReopened,
		ActorKind:    &actorKind,
		ActorSubject: &subject,
		Payload:      payload,
	}); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"fixup acceptance invalidation: append acceptance_reopened audit failed",
			slog.String("run_id", runID.String()),
			slog.String("acceptance_stage_id", dec.Stage.ID.String()),
			slog.String("error", err.Error()))
	}
	s.notifyStatusUpdate(ctx, runID, "acceptance_reopened")
}

// acceptanceStageHasVerdict reports whether the run's audit chain carries an
// acceptance_outcome_recorded entry scoped to stageID — i.e. the acceptance
// stage settled WITH a recorded verdict. Distinguishes the #1682 fix-up
// invalidation target (verdict-ful) from the #1567 outcome-less operator-reopen
// hole. Propagates the read error so the caller fails closed (skips the reopen)
// on an unreadable chain rather than acting on unknown evidence state.
func (s *Server) acceptanceStageHasVerdict(ctx context.Context, runID, stageID uuid.UUID) (bool, error) {
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, CategoryAcceptanceOutcomeRecorded)
	if err != nil {
		return false, err
	}
	for _, e := range entries {
		if e.StageID != nil && *e.StageID == stageID {
			return true, nil
		}
	}
	return false, nil
}

// acceptanceValidatedHeadSHA resolves the head the acceptance stage actually
// validated (#1682, binding condition 2): the run's newest recorded head at
// the moment the stage was DISPATCHED. Head-report entries with sequence
// at-or-before the latest acceptance_dispatched entry for THIS stage are the
// candidates; the precedence winner among them (via the shared
// auditcomplete.LatestReportedHeadSHA) is the validated head. Anchoring on the
// dispatch sequence excludes a fixup_pushed / child_pushed that lands AFTER
// dispatch, so the verdict binds to the commit the validator checked out —
// never a later commit that has not been validated.
//
// A re-opened acceptance stage carries more than one acceptance_dispatched
// entry; the highest-sequence one is the current validation episode. Returns
// ("", false) when no head is recorded at-or-before dispatch, or when the stage
// has no dispatch entry (a bare operator ship with no orchestrator dispatch, or
// a read error) — the caller records an empty head_sha and Option C fails
// closed to today's 422 for such an unanchored verdict.
func (s *Server) acceptanceValidatedHeadSHA(ctx context.Context, runID, stageID uuid.UUID) (string, bool) {
	if s.cfg.AuditRepo == nil {
		return "", false
	}
	dispatchSeq, haveAnchor := s.latestAcceptanceDispatchSeq(ctx, runID, stageID)
	if !haveAnchor {
		return "", false
	}
	var candidates []*audit.Entry
	for _, cat := range auditcomplete.HeadReportCategoriesByPrecedence {
		es, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, cat)
		if err != nil {
			return "", false
		}
		for _, e := range es {
			if e.Sequence <= dispatchSeq {
				candidates = append(candidates, e)
			}
		}
	}
	return auditcomplete.LatestReportedHeadSHA(candidates)
}

// latestAcceptanceDispatchSeq returns the highest audit sequence among the
// run's acceptance_dispatched entries scoped to stageID, and whether any exist.
// The dispatch anchor for acceptanceValidatedHeadSHA. A read error is reported
// as (0, false) so the caller treats an unreadable anchor as "no anchor" and
// records an empty head_sha (fail-closed for Option C), never a wrong head.
func (s *Server) latestAcceptanceDispatchSeq(ctx context.Context, runID, stageID uuid.UUID) (int64, bool) {
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, CategoryAcceptanceDispatched)
	if err != nil {
		return 0, false
	}
	var seq int64
	found := false
	for _, e := range entries {
		if e.StageID != nil && *e.StageID == stageID {
			if !found || e.Sequence > seq {
				seq = e.Sequence
				found = true
			}
		}
	}
	return seq, found
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
// stage's first spec-declared egress target host as a full http(s) URL. A
// schemeless host or host:port gains an http:// prefix so buildAcceptance
// renders a URL (e.g. http://localhost:8090) rather than a bare authority —
// handing the validator the target already in URL form so its verdict's
// target_url does not need the twin decoders' schemeless coercion (the #1574
// class). An egress host that already carries a scheme passes through
// unchanged. A spec with no egress block (a pre-1.3 spec, or one relying on
// the documented interim posture) yields the empty string and buildAcceptance
// renders its explicit not-declared line.
//
// This SUPERSEDES ADR-050 decision #1's verbatim-host posture FOR THE PROMPT
// SEAM ONLY: the prompt text is the sole consumer, and #1574 showed a bare
// host:port here nudges the agent toward emitting a schemeless target_url.
// The sibling resolveAcceptanceEgressTargetHosts KEEPS the verbatim host:port
// grammar unchanged — the egress-proxy allow-list declares hosts, not URLs,
// so no scheme is fabricated there.
func (s *Server) resolveAcceptanceTargetURL(ctx context.Context, runRow *run.Run) string {
	hosts := s.resolveAcceptanceEgressTargetHosts(ctx, runRow)
	if len(hosts) == 0 {
		return ""
	}
	h := hosts[0]
	if strings.Contains(h, "://") {
		return h
	}
	return "http://" + h
}

// resolveAcceptanceEgressTargetHosts returns ALL of the acceptance stage's
// spec-declared egress target hosts (the E31.4/#1532 grammar), in declaration
// order. The full list — not just the first host the prompt-text seam renders
// — is served on the acceptance-stage prompt response as egress_target_hosts:
// the runner's ADR-050 egress-proxy allow-list input (E31.7 / #1535). nil for
// a spec with no egress block, so the response field stays omitted. Unlike
// resolveAcceptanceTargetURL (the prompt seam, which now prefixes http://),
// this KEEPS the verbatim host:port grammar per ADR-050 decision #1 — the
// allow-list matches authorities, not URLs, so no scheme is fabricated.
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

// classifyAcceptanceFailure is the pure triage classifier (E31.8 / #1536): it
// maps a failed verdict's failure_mode plus per-criterion results, resolved
// against the approved plan's acceptance-criteria provenance (explicit vs
// inferred), onto one of four classes. criteria is the approved plan's
// acceptance_criteria (nil/empty when the plan predates the typed contract or
// could not be loaded — provenance cannot be grounded). Returns the class,
// the criterion ids that key the disposition (the E31.11 per-criterion join
// key), and a one-line human-readable reason embedded in the audit payload.
func classifyAcceptanceFailure(acc acceptanceBody, criteria []plan.AcceptanceCriterion) (class string, criterionIDs []string, reason string) {
	// Provenance lookup by criterion id.
	provenance := make(map[string]plan.CriterionSource, len(criteria))
	for _, c := range criteria {
		provenance[c.ID] = c.Source
	}

	// failure_mode=error: the code errored attempting the behavior — it
	// objectively fails, so route to a bounded fix-up pass (class 1). Carry
	// the failed criteria ids when the verdict itemized them.
	if acc.FailureMode == acceptanceFailureError {
		return acceptanceClass1, failedCriterionIDs(acc.normalizedCriteria),
			"failure_mode=error: the code errored attempting the behavior; routing to a bounded fix-up pass"
	}

	// assertion_fail (validate() guarantees failure_mode is error or
	// assertion_fail on a failed verdict). Partition the criteria results.
	var failed []string
	skipped := 0
	for _, c := range acc.normalizedCriteria {
		switch c.Result {
		case acceptanceResultFailed:
			failed = append(failed, c.ID)
		case acceptanceResultSkipped:
			skipped++
		}
	}

	if len(failed) > 0 {
		// Resolve every failed id against the plan provenance.
		var inferredOrUnresolvable []string
		allExplicit := true
		for _, id := range failed {
			src, ok := provenance[id]
			if !ok || src != plan.CriterionSourceExplicit {
				allExplicit = false
				inferredOrUnresolvable = append(inferredOrUnresolvable, id)
			}
		}
		if allExplicit {
			// Every failed criterion is explicit-source — the code objectively
			// fails a stated criterion (class 1).
			return acceptanceClass1, failed,
				"assertion_fail: every failed criterion is explicit-source; the code objectively fails a stated criterion"
		}
		// At least one failed criterion is inferred-source or unresolvable — a
		// bad/ambiguous criterion (class 3). The criterion_ids record the
		// per-criterion disposition E31.11 consumes.
		return acceptanceClass3, inferredOrUnresolvable,
			"assertion_fail: a failed criterion is inferred-source or unresolvable against the plan (bad/ambiguous criterion)"
	}

	// No failed criteria but ≥1 skip. Normally an environment/flake signal
	// (class 2, bounded re-run). Split off the posture-A can't-exhibit shape:
	// when EVERY skipped criterion carries a non-empty expectation_basis (the
	// #1612 signal that the egress-sandboxed acceptance agent could not produce
	// the external trigger — e.g. closing a GitHub issue), the re-run is
	// deterministically futile (the sandbox still can't reach the external
	// service). Route to the terminal class 5 that pages WITHOUT re-opening the
	// stage. A skip that lacks expectation_basis is genuinely ambiguous and
	// keeps the bounded class-2 flake path unchanged (#1671).
	if skipped > 0 {
		if allSkipsCarryExpectationBasis(acc.normalizedCriteria) {
			return acceptanceClass5, nil,
				"assertion_fail: no criterion failed and every skipped criterion is a posture-A can't-exhibit skip with expectation_basis; the external trigger cannot be produced in the default-deny egress sandbox — routing to a terminal page rather than a futile flake retry"
		}
		return acceptanceClass2, nil,
			"assertion_fail: no criterion failed but at least one was skipped; validation could not complete (environment/flake signal)"
	}

	// F empty and no skips, OR the plan carries no acceptance_criteria to
	// ground provenance: unitemized / provenance-ungroundable failure —
	// works-as-planned, disputed (class 4).
	return acceptanceClass4, nil,
		"unitemized or provenance-ungroundable failure; works-as-planned/disputed — paging the human"
}

// failedCriterionIDs returns the ids of criteria whose result is failed, in
// order.
func failedCriterionIDs(criteria []acceptanceCriterionResult) []string {
	var ids []string
	for _, c := range criteria {
		if c.Result == acceptanceResultFailed {
			ids = append(ids, c.ID)
		}
	}
	return ids
}

// allSkipsCarryExpectationBasis reports whether every skipped criterion in the
// verdict carries a non-empty expectation_basis — the #1612 posture-A
// can't-exhibit discriminator that separates a deterministically-futile
// externally-unvalidatable skip (class 5) from a genuinely ambiguous flake
// skip (class 2). Callers gate on skipped>0 first, so an all-passed set (which
// makes this vacuously true) never reaches here.
func allSkipsCarryExpectationBasis(criteria []acceptanceCriterionResult) bool {
	for _, c := range criteria {
		if c.Result == acceptanceResultSkipped && strings.TrimSpace(c.ExpectationBasis) == "" {
			return false
		}
	}
	return true
}

// triageAcceptanceFailure routes a freshly persisted verdict:failed artifact
// (E31.8 / #1536). Called from handleShipAcceptance ONLY on the fresh-create
// path (never the idempotent replay) and only when acc.Verdict==failed. It is
// best-effort relative to the ship: every internal error WARN-logs and never
// unwinds the 201 / artifact / outcome audit. It ALWAYS ends by writing ONE
// acceptance_triage_decided chained entry recording what actually happened.
func (s *Server) triageAcceptanceFailure(ctx context.Context, runID uuid.UUID, stage *run.Stage, acc acceptanceBody, artifactID string) {
	// Load the approved plan for provenance grounding (nil-tolerant → the
	// classifier grounds class 4).
	var criteria []plan.AcceptanceCriterion
	if p, err := s.loadApprovedPlanForRun(ctx, runID); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"acceptance triage: load approved plan failed; grounding provenance as absent",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()))
	} else if p != nil {
		criteria = p.Verification.AcceptanceCriteria
	}

	class, criterionIDs, reason := classifyAcceptanceFailure(acc, criteria)

	// E31.11 (#1539, ADR-049 decision #4): a class-3 decision is a
	// plan-review miss — a bad criterion the plan gate approved. Build the
	// durable per-criterion record ONCE here so every disposition branch
	// below (paged, unsettled, budget-exhausted) carries it. nil for
	// classes 1/2/4, so the payload field stays omitted there.
	var misses []agenteval.PlanReviewMiss
	if class == acceptanceClass3 {
		misses = buildPlanReviewMisses(acc, criteria, criterionIDs)
	}

	// Count prior auto-routed decisions from the audit chain (the durable
	// mirror of countFixupPasses). A count failure means we cannot bound
	// safely — degrade to paged without acting.
	prior, err := s.countAcceptanceTriageRoutes(ctx, runID)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"acceptance triage: count prior routed decisions failed; paging without action",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()))
		s.writeAcceptanceTriageAudit(ctx, runID, stage.ID, artifactID, class,
			acceptanceDispositionPaged, criterionIDs, acc.FailureMode, prior,
			"triage route count failed; paging without action", misses)
		return
	}

	// Defensive settle check: an operator-bearer ship may race the trace-bundle
	// settle. If the acceptance stage row is not yet succeeded, record the
	// classification with unsettled_paged instead of acting.
	if stage.State != run.StageStateSucceeded {
		s.writeAcceptanceTriageAudit(ctx, runID, stage.ID, artifactID, class,
			acceptanceDispositionUnsettled, criterionIDs, acc.FailureMode, prior,
			fmt.Sprintf("acceptance stage not yet settled (state %q); recording classification without acting", stage.State), misses)
		return
	}

	// Re-run bound: at the cap keep the classified class but degrade to a paged
	// variant so non-convergence lands on the human.
	if prior >= defaultMaxAcceptanceReruns {
		s.writeAcceptanceTriageAudit(ctx, runID, stage.ID, artifactID, class,
			acceptanceDispositionRerunBudget, criterionIDs, acc.FailureMode, prior,
			fmt.Sprintf("re-run budget exhausted (%d of %d auto-routed passes used); paging", prior, defaultMaxAcceptanceReruns), misses)
		return
	}

	// Route by class. Class 3 / class 4 take NO state transition — page. Class 5
	// (all-skip externally-unvalidatable) is ALSO terminal, no transition: it
	// pages under the distinct externally_unvalidatable_paged token so the
	// acceptance stage stays succeeded and never enters the futile class-2 retry
	// loop (#1671). Because it never re-opens the stage it never contributes to
	// the auto-routed count defaultMaxAcceptanceReruns bounds.
	var disposition string
	switch class {
	case acceptanceClass1:
		disposition = s.routeAcceptanceClass1(ctx, runID, stage, acc, criteria, criterionIDs, reason)
	case acceptanceClass2:
		disposition = s.routeAcceptanceClass2(ctx, runID, stage)
	case acceptanceClass5:
		disposition = acceptanceDispositionUnvalidatable
	default:
		disposition = acceptanceDispositionPaged
	}

	s.writeAcceptanceTriageAudit(ctx, runID, stage.ID, artifactID, class,
		disposition, criterionIDs, acc.FailureMode, prior, reason, misses)
}

// buildPlanReviewMisses joins each class-3 criterion id with the approved
// plan criterion's provenance fields and the shipped verdict's per-criterion
// evidence for that id (E31.11 / #1539). An id that does not resolve against
// the plan still yields a record keyed by the id with empty provenance
// fields — unresolvable is itself the miss. Uses the shared
// agenteval.PlanReviewMiss wire type so the server marshal, the
// distill-corpus tool unmarshal, and the corpus loader cannot drift.
func buildPlanReviewMisses(acc acceptanceBody, criteria []plan.AcceptanceCriterion, criterionIDs []string) []agenteval.PlanReviewMiss {
	planByID := make(map[string]plan.AcceptanceCriterion, len(criteria))
	for _, c := range criteria {
		planByID[c.ID] = c
	}
	resultByID := make(map[string]acceptanceCriterionResult, len(acc.normalizedCriteria))
	for _, c := range acc.normalizedCriteria {
		resultByID[c.ID] = c
	}

	out := make([]agenteval.PlanReviewMiss, 0, len(criterionIDs))
	for _, id := range criterionIDs {
		m := agenteval.PlanReviewMiss{CriterionID: id}
		if pc, ok := planByID[id]; ok {
			m.Statement = pc.Statement
			m.Source = string(pc.Source)
			m.SourceRef = pc.SourceRef
			m.Rationale = pc.Rationale
		}
		if r, ok := resultByID[id]; ok {
			m.Observed = r.Observed
			m.Expected = r.Expected
			m.StepsTaken = r.StepsTaken
			m.ExpectationBasis = r.ExpectationBasis
			m.ReproHandle = r.ReproHandle
			m.Result = r.Result
		}
		out = append(out, m)
	}
	return out
}

// routeAcceptanceClass1 synthesizes the behavioral evidence into
// implement-stage fix-up concerns and routes them via the existing
// fixupStageAs under a token-less system identity, with the triggering
// acceptance stage re-opened (FixupOptions.AcceptanceStageID). Returns the
// disposition: fixup_dispatched on success, fixup_unavailable_paged on ANY
// routing refusal (implement stage not found, budget/ceiling exhausted, stage
// not applicable) so the disposition always lands on the human at the cap.
func (s *Server) routeAcceptanceClass1(ctx context.Context, runID uuid.UUID, stage *run.Stage, acc acceptanceBody, criteria []plan.AcceptanceCriterion, criterionIDs []string, reason string) string {
	stages, err := s.cfg.RunRepo.ListStagesForRun(ctx, runID)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"acceptance triage class-1: list stages failed; paging",
			slog.String("run_id", runID.String()), slog.String("error", err.Error()))
		return acceptanceDispositionFixupUnavailable
	}
	var implement *run.Stage
	for _, st := range stages {
		if st.Type == run.StageTypeImplement {
			implement = st
			break
		}
	}
	if implement == nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"acceptance triage class-1: no implement stage on run; paging",
			slog.String("run_id", runID.String()))
		return acceptanceDispositionFixupUnavailable
	}

	selected := synthesizeAcceptanceConcerns(acc, criteria, criterionIDs, reason)

	priorPasses, err := s.countFixupPasses(ctx, runID, implement.ID)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"acceptance triage class-1: count fixup passes failed; paging",
			slog.String("run_id", runID.String()), slog.String("error", err.Error()))
		return acceptanceDispositionFixupUnavailable
	}

	acceptanceStageID := stage.ID
	dec, ferr := s.fixupStageAs(ctx, Identity{Subject: acceptanceTriageSystemSubject}, fixupActionParams{
		StageID: implement.ID,
		Options: run.FixupOptions{
			PriorPassCount:    priorPasses,
			MaxPasses:         defaultMaxFixupPasses,
			HardCeiling:       defaultFixupCeiling,
			AcceptanceStageID: &acceptanceStageID,
		},
		Selected:    selected,
		PriorPasses: priorPasses,
		Reason:      reason,
	})
	if ferr != nil {
		// A refusal (ErrFixupBudgetExhausted / ErrFixupCeilingReached /
		// ErrFixupNotApplicable) or any other error degrades to a paged
		// disposition — the implement fixup budget therefore ALSO bounds
		// acceptance-driven passes.
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"acceptance triage class-1: fixup route refused; paging",
			slog.String("run_id", runID.String()),
			slog.String("implement_stage_id", implement.ID.String()),
			slog.String("error", ferr.Error()))
		return acceptanceDispositionFixupUnavailable
	}
	// Read the acceptance-driven decision field (E31.8): the fixup helper
	// passed AcceptanceStageID through unchanged and re-opened the settled
	// acceptance stage. A nil here means the re-open did not fire as expected
	// (the acceptance stage was not settled at fixup time) — the fix-up still
	// dispatched, so log and proceed.
	if dec != nil && dec.ReopenedAcceptance == nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"acceptance triage class-1: fix-up dispatched but acceptance stage was not re-opened",
			slog.String("run_id", runID.String()),
			slog.String("acceptance_stage_id", acceptanceStageID.String()))
	}
	return acceptanceDispositionFixupDispatched
}

// routeAcceptanceClass2 re-opens the settled acceptance stage (class-2:
// environment/flake) via run.ReopenAcceptanceStage, then runs the retry-shaped
// post-transition steps (orchestrator Advance, WARN-on-error; status notify).
// Returns retry_dispatched on success, retry_unavailable_paged on a reopen
// refusal.
func (s *Server) routeAcceptanceClass2(ctx context.Context, runID uuid.UUID, stage *run.Stage) string {
	dec, err := run.ReopenAcceptanceStage(ctx, s.cfg.RunRepo, stage.ID)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"acceptance triage class-2: acceptance re-open refused; paging",
			slog.String("run_id", runID.String()),
			slog.String("acceptance_stage_id", stage.ID.String()),
			slog.String("error", err.Error()))
		return acceptanceDispositionRetryUnavailable
	}
	// Hand off to the orchestrator so it walks pending → dispatched and
	// rebuilds a fresh preview. WARN-on-error: the stage stays pending for a
	// manual re-fire, mirroring the retry handler.
	if dec.Stage.State == run.StageStatePending && s.cfg.Orchestrator != nil {
		if _, aerr := s.cfg.Orchestrator.Advance(ctx, runID); aerr != nil {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelError,
				"acceptance triage class-2: orchestrator advance failed",
				slog.String("run_id", runID.String()),
				slog.String("error", aerr.Error()))
		}
	}
	s.notifyStatusUpdate(ctx, runID, "acceptance_triage_reopen")
	return acceptanceDispositionRetryDispatched
}

// synthesizeAcceptanceConcerns builds the []planreview.Concern the class-1
// fix-up routes back to the implement agent: one per failed criterion with the
// behavioral evidence (observed/expected/steps_taken/expectation_basis/
// repro_handle) the verdict carried, composed with the plan criterion
// statement. When the verdict itemized nothing (an error verdict with no
// per-criterion results), a single concern is synthesized from the
// failure_mode / target_url / criteria tally.
func synthesizeAcceptanceConcerns(acc acceptanceBody, criteria []plan.AcceptanceCriterion, criterionIDs []string, reason string) []planreview.Concern {
	statementByID := make(map[string]string, len(criteria))
	for _, c := range criteria {
		statementByID[c.ID] = c.Statement
	}
	failedByID := make(map[string]acceptanceCriterionResult, len(acc.normalizedCriteria))
	for _, c := range acc.normalizedCriteria {
		if c.Result == acceptanceResultFailed {
			failedByID[c.ID] = c
		}
	}

	var out []planreview.Concern
	for _, id := range criterionIDs {
		c, ok := failedByID[id]
		if !ok {
			continue
		}
		out = append(out, planreview.Concern{
			Severity: planreview.SeverityHigh,
			Category: "acceptance",
			Note:     composeAcceptanceConcernNote(c, statementByID[id]),
		})
	}
	if len(out) == 0 {
		out = append(out, planreview.Concern{
			Severity: planreview.SeverityHigh,
			Category: "acceptance",
			Note:     composeAcceptanceFallbackNote(acc, reason),
		})
	}
	return out
}

// composeAcceptanceConcernNote renders one failed criterion's behavioral
// evidence into a fix-up concern note.
func composeAcceptanceConcernNote(c acceptanceCriterionResult, statement string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Acceptance criterion %q failed validation.", c.ID)
	if statement != "" {
		fmt.Fprintf(&b, " Criterion: %s", statement)
	}
	if c.Observed != "" {
		fmt.Fprintf(&b, " Observed: %s", c.Observed)
	}
	if c.Expected != "" {
		fmt.Fprintf(&b, " Expected: %s", c.Expected)
	}
	if c.StepsTaken != "" {
		fmt.Fprintf(&b, " Steps taken: %s", c.StepsTaken)
	}
	if c.ExpectationBasis != "" {
		fmt.Fprintf(&b, " Expectation basis: %s", c.ExpectationBasis)
	}
	if c.ReproHandle != "" {
		fmt.Fprintf(&b, " Repro: %s", c.ReproHandle)
	}
	return b.String()
}

// composeAcceptanceFallbackNote renders a single fix-up concern from the
// verdict envelope when no per-criterion evidence was itemized.
func composeAcceptanceFallbackNote(acc acceptanceBody, reason string) string {
	var b strings.Builder
	b.WriteString("Acceptance validation failed and requires a fix-up.")
	if acc.FailureMode != "" {
		fmt.Fprintf(&b, " Failure mode: %s.", acc.FailureMode)
	}
	if acc.TargetURL != "" {
		fmt.Fprintf(&b, " Target: %s.", acc.TargetURL)
	}
	passed, failed, skipped, total := acceptanceCriteriaTally(acc.normalizedCriteria)
	fmt.Fprintf(&b, " Criteria tally: %d passed / %d failed / %d skipped of %d.", passed, failed, skipped, total)
	if reason != "" {
		fmt.Fprintf(&b, " Triage: %s", reason)
	}
	return b.String()
}

// countAcceptanceTriageRoutes counts the run's prior acceptance_triage_decided
// entries whose disposition auto-routed (fixup_dispatched | retry_dispatched)
// — the durable mirror of countFixupPasses that bounds re-runs across
// restarts.
func (s *Server) countAcceptanceTriageRoutes(ctx context.Context, runID uuid.UUID) (int, error) {
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, CategoryAcceptanceTriageDecided)
	if err != nil {
		return 0, fmt.Errorf("list %s audit entries: %w", CategoryAcceptanceTriageDecided, err)
	}
	n := 0
	for _, e := range entries {
		switch acceptanceTriageDispositionOf(e.Payload) {
		case acceptanceDispositionFixupDispatched, acceptanceDispositionRetryDispatched:
			n++
		}
	}
	return n, nil
}

// acceptanceTriageDispositionOf reads the `disposition` field from an
// acceptance_triage_decided payload. Empty on any decode failure.
func acceptanceTriageDispositionOf(payload []byte) string {
	if len(payload) == 0 {
		return ""
	}
	var p struct {
		Disposition string `json:"disposition"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return ""
	}
	return p.Disposition
}

// writeAcceptanceTriageAudit appends the single acceptance_triage_decided
// chained entry recording the class + realized disposition + criterion_ids +
// bound accounting. Written AFTER acting so the disposition records what
// actually happened. Best-effort: a failure here WARN-logs (the ship is
// already committed). misses is the E31.11 per-criterion plan-review-miss
// record — additive: emitted as the plan_review_miss payload field only when
// non-empty (class 3), omitted entirely otherwise, so existing consumers
// (issuecomment decodeAcceptanceActivity, acceptanceTriageDispositionOf)
// that decode named fields are untouched.
func (s *Server) writeAcceptanceTriageAudit(ctx context.Context, runID, stageID uuid.UUID, artifactID, class, disposition string, criterionIDs []string, failureMode string, priorRoutedPasses int, reason string, misses []agenteval.PlanReviewMiss) {
	if criterionIDs == nil {
		criterionIDs = []string{}
	}
	systemKind := audit.ActorSystem
	fields := map[string]any{
		"run_id":              runID.String(),
		"stage_id":            stageID.String(),
		"artifact_id":         artifactID,
		"class":               class,
		"disposition":         disposition,
		"criterion_ids":       criterionIDs,
		"failure_mode":        failureMode,
		"prior_routed_passes": priorRoutedPasses,
		"reason":              reason,
	}
	if len(misses) > 0 {
		fields["plan_review_miss"] = misses
	}
	payload, _ := json.Marshal(fields)
	if _, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     runID,
		StageID:   &stageID,
		Timestamp: time.Now().UTC(),
		Category:  CategoryAcceptanceTriageDecided,
		ActorKind: &systemKind,
		Payload:   payload,
	}); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"acceptance triage: append acceptance_triage_decided audit entry failed",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("error", err.Error()))
	}
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
