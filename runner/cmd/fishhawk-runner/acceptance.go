package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/kuhlman-labs/fishhawk/redaction"
	"github.com/kuhlman-labs/fishhawk/runner/internal/agent"
)

/*
 * Acceptance-agent verdict handling (E31.7 / #1535, ADR-049 decisions
 * #3/#5). The acceptance stage drives an agent against the running
 * target instance and captures its structured verdict:
 *
 *   capture   — Result.StructuredOutput (claudecode --json-schema) wins;
 *               the /tmp/fishhawk-acceptance.json file is the fallback
 *               transport for backends that ignore Invocation.JSONSchema
 *               (codex). Neither present is a category-B
 *               acceptance_verdict_missing.
 *   validate  — a runner-side mirror of the backend acceptanceBody
 *               validator (backend/internal/server/acceptance.go), plus
 *               the served-criteria-ids membership check, so a bad shape
 *               fails in-loop instead of at the signed ship.
 *   redact    — redaction.RedactDefault over the verdict bytes BEFORE
 *               they are embedded in the trace bundle or shipped: the
 *               observed/steps_taken prose comes from a potentially
 *               prompt-injected instance, so it is treated as hostile.
 *   evidence  — the redacted verdict rides into the trace bundle as an
 *               acceptance_evidence event, appended before PackBytes so
 *               both bundle variants carry it.
 *
 * A VALID verdict of "failed" is NOT a runner failure: the validation
 * completed and produced evidence; routing the failure is E31.8's scope.
 */

// acceptanceVerdictDir is the directory the run/stage-keyed acceptance verdict
// fallback lives in (#1777). var (not const) so tests can redirect it to a
// t.TempDir path and avoid /tmp pollution / parallel-test races.
var acceptanceVerdictDir = "/tmp"

// acceptanceVerdictPath is the run/stage-keyed file-fallback transport for the
// verdict (the codex path — Invocation.JSONSchema is a claudecode-only feature,
// so StructuredOutput stays nil there). Keyed by the FULL run id + stage id
// (#1777) so parallel acceptance runners on one host no longer share a single
// fixed path. The acceptance prompt now NAMES this run/stage-keyed path
// (backend/internal/prompt/prompt.go AcceptanceVerdictPath is keyed as of #1780,
// because the acceptance Trigger threads AcceptanceRunID/AcceptanceStageID), so
// the runner's keyed-first read matches the prompt's keyed write on the happy
// path. The legacy fixed path is retained as the fallback (binding condition 1)
// — a trigger missing ids (rendered via the legacy path) is therefore never
// stranded in verdict-missing. MUST stay byte-identical to the prompt's keyed
// AcceptanceVerdictPath format string.
func acceptanceVerdictPath(runID, stageID string) string {
	return filepath.Join(acceptanceVerdictDir, fmt.Sprintf("fishhawk-acceptance-%s-%s.json", runID, stageID))
}

// legacyAcceptanceVerdictPath is the fixed shared path the acceptance prompt
// named before #1780 keyed it (prompt.LegacyAcceptanceVerdictPath). The runner
// reads the keyed path first and falls back to THIS legacy path (binding
// condition 1), so a trigger that threads no ids — rendered via the legacy path
// — still lands its verdict. MUST stay byte-identical to
// prompt.LegacyAcceptanceVerdictPath. var (not const) so tests can redirect it.
var legacyAcceptanceVerdictPath = "/tmp/fishhawk-acceptance.json"

// acceptanceVerdictJSONSchema is the structured-output schema for the
// claudecode --json-schema flag (Invocation.JSONSchema). Hand-authored
// to match the backend's acceptanceBody wire shape EXACTLY
// (backend/internal/server/acceptance.go) — the lockstep is guarded by
// TestAcceptanceVerdictSchema_LockstepWithValidator: a verdict this
// schema admits must pass validateAcceptanceVerdict, whose rules mirror
// the backend validator.
//
// Deliberately carries NO "$schema" dialect declaration: claude CLI
// 2.1.205's strict --json-schema validator tries to RESOLVE the dialect
// URI and fails ("no schema with key or ref ..."), killing the stage
// pre-spawn — the acceptance-side sibling of the #1741/#1742 plan-stage
// outage. The plan path strips $schema in its derivation
// (structuredOutputDroppedKeywords); this hand-authored constant must
// stay dialect-free for the same reason, pinned by
// TestAcceptanceVerdictSchema_NoDialectOrVendorKeys.
const acceptanceVerdictJSONSchema = `{
  "type": "object",
  "additionalProperties": false,
  "required": ["verdict"],
  "properties": {
    "verdict": {"type": "string", "enum": ["passed", "failed"]},
    "failure_mode": {"type": "string", "enum": ["error", "assertion_fail"]},
    "criteria": {
      "type": "array",
      "items": {
        "type": "object",
        "additionalProperties": false,
        "required": ["id", "result"],
        "properties": {
          "id": {"type": "string"},
          "result": {"type": "string", "enum": ["passed", "failed", "skipped"]},
          "observed": {"type": "string"},
          "expected": {"type": "string"},
          "steps_taken": {"type": "string"},
          "expectation_basis": {"type": "string"},
          "repro_handle": {"type": "string"}
        }
      }
    },
    "target_url": {"type": "string"},
    "evidence_hashes": {"type": "array", "items": {"type": "string"}},
    "notes": {"type": "string"}
  }
}`

// errAcceptanceVerdictMissing is the sentinel for "the agent produced
// no verdict on either transport" — no StructuredOutput and no fallback
// file. Category-B at the call site (acceptance_verdict_missing).
var errAcceptanceVerdictMissing = errors.New(
	"acceptance verdict missing: no structured output and no fallback file")

// acceptanceCriterionResult mirrors the backend's per-criterion entry
// by json tag (the runner↔backend wire-contract convention, same as
// upload.ScopeExemption / #1229).
type acceptanceCriterionResult struct {
	ID               string `json:"id"`
	Result           string `json:"result"`
	Observed         string `json:"observed,omitempty"`
	Expected         string `json:"expected,omitempty"`
	StepsTaken       string `json:"steps_taken,omitempty"`
	ExpectationBasis string `json:"expectation_basis,omitempty"`
	ReproHandle      string `json:"repro_handle,omitempty"`
}

// acceptanceVerdict mirrors the backend's acceptanceBody by json tag.
// EvidenceHashes and Criteria are both json.RawMessage (not typed) so
// validateAcceptanceVerdict can losslessly coerce the historical
// #1574-class shape variants before the fail-closed reject — a
// string-valued object-map evidence_hashes, and a criteria object keyed by
// criterion id — the backend twin decodes the same way. The strict decode
// that rejects unknown fields INSIDE a criteria element is re-applied by
// coerceAcceptanceCriteria's array path (the top-level DisallowUnknownFields
// decoder does not descend into a RawMessage field).
type acceptanceVerdict struct {
	Verdict        string          `json:"verdict"`
	FailureMode    string          `json:"failure_mode,omitempty"`
	Criteria       json.RawMessage `json:"criteria,omitempty"`
	TargetURL      string          `json:"target_url,omitempty"`
	EvidenceHashes json.RawMessage `json:"evidence_hashes,omitempty"`
	// Notes is a declared home for the agent's free-text overflow (#1567):
	// a top-level remark that would otherwise fail closed against
	// DisallowUnknownFields. Declaring it makes a benign aside validate
	// while every UNdeclared field still fails the stage. Load-bearing on
	// the file-fallback transport, which carries no JSON schema at all.
	Notes string `json:"notes,omitempty"`
}

// captureAcceptanceVerdict returns the agent's verdict bytes.
// Result.StructuredOutput (the claudecode --json-schema capture) is preferred;
// the file fallback reads the run/stage-KEYED path first and, when it is absent,
// falls back to the LEGACY fixed path (#1777, binding condition 1). The
// acceptance prompt's output contract still names the legacy fixed path (it
// threads no run/stage ids), so the legacy fallback is the path the verdict
// actually lands on today — reading keyed-first means keying the prompt later
// needs no change here, and a fixed-path render is never stranded in
// verdict-missing. A legacy read emits an acceptance_verdict_legacy_path
// deprecation event via warn (nil-tolerant). Returns errAcceptanceVerdictMissing
// when neither transport produced anything.
func captureAcceptanceVerdict(res agent.Result, keyedPath, legacyPath string, warn func(event, detail string)) ([]byte, error) {
	if len(res.StructuredOutput) > 0 {
		return res.StructuredOutput, nil
	}
	b, err := os.ReadFile(keyedPath)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("acceptance verdict fallback read: %w", err)
		}
		// Keyed path absent: fall back to the legacy fixed path the prompt names.
		lb, lerr := os.ReadFile(legacyPath)
		if lerr != nil {
			if os.IsNotExist(lerr) {
				return nil, errAcceptanceVerdictMissing
			}
			return nil, fmt.Errorf("acceptance verdict legacy fallback read: %w", lerr)
		}
		if len(lb) == 0 {
			return nil, errAcceptanceVerdictMissing
		}
		if warn != nil {
			warn("acceptance_verdict_legacy_path",
				"read verdict from legacy fixed path "+legacyPath)
		}
		return lb, nil
	}
	if len(b) == 0 {
		return nil, errAcceptanceVerdictMissing
	}
	return b, nil
}

// validateAcceptanceVerdict decodes + validates the verdict bytes,
// MIRRORING the backend acceptanceBody validator
// (backend/internal/server/acceptance.go: DisallowUnknownFields,
// single-object body, verdict/failure_mode/criteria-result rules) so a
// runner-accepted verdict is backend-acceptable. On top of the mirror
// it enforces the served-criteria-ids join-key membership (E31.7): when
// servedCriteriaIDs is non-empty, every criteria[].id must be a member —
// an unknown id fails closed rather than pinning evidence to a
// criterion the approved plan never declared. An empty served set skips
// the membership check (no approved plan / no declared criteria).
//
// It also applies the two lossless coercions of the #1574 class BEFORE
// the fail-closed rejections — a string-valued object-map evidence_hashes
// collapses to its sorted values, and a schemeless host[:port] target_url
// gains an http:// prefix — mirroring the backend twin so the shapes that
// wedged historical runs decode instead of failing. Anything lossy still
// fails closed. On success it returns the NORMALIZED verdict bytes: when a
// coercion fired the bytes are re-marshaled to the canonical shape so the
// downstream redact + ship carry the normalized form; otherwise the
// original bytes are returned unchanged. warn (nil-tolerant) receives an
// (event, detail) pair per coercion for the runner log seam.
func validateAcceptanceVerdict(raw []byte, servedCriteriaIDs []string, warn func(event, detail string)) ([]byte, error) {
	var v acceptanceVerdict
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("acceptance verdict could not be decoded: %w", err)
	}
	if dec.More() {
		return nil, errors.New("acceptance verdict must be a single JSON object")
	}

	switch v.Verdict {
	case "passed":
		if v.FailureMode != "" {
			return nil, fmt.Errorf("failure_mode must be omitted on a passed verdict, got %q", v.FailureMode)
		}
	case "failed":
		switch v.FailureMode {
		case "error", "assertion_fail":
			// ok
		case "":
			return nil, errors.New("failure_mode is required when verdict is failed (error | assertion_fail)")
		default:
			return nil, fmt.Errorf("failure_mode must be error or assertion_fail, got %q", v.FailureMode)
		}
	case "":
		return nil, errors.New("verdict is required")
	default:
		return nil, fmt.Errorf("verdict must be passed or failed, got %q", v.Verdict)
	}

	// Coerce criteria before the per-criterion fail-closed checks: an object
	// keyed by criterion id folds into the schema-required flat array with each
	// key written into the element id (the #1574 class); a non-object keyed
	// value, a key/element-id conflict, or a scalar fails closed. A flat array
	// passes through (strict-decoded so an unknown element field still fails
	// closed).
	criteria, coercedCriteria, err := coerceAcceptanceCriteria(v.Criteria)
	if err != nil {
		return nil, err
	}

	served := make(map[string]struct{}, len(servedCriteriaIDs))
	for _, id := range servedCriteriaIDs {
		served[id] = struct{}{}
	}
	for i, c := range criteria {
		if c.ID == "" {
			return nil, fmt.Errorf("criteria[%d].id is required (the plan-criterion join key)", i)
		}
		switch c.Result {
		case "passed", "failed", "skipped":
			// ok
		default:
			return nil, fmt.Errorf("criteria[%d].result must be passed/failed/skipped, got %q", i, c.Result)
		}
		if len(served) > 0 {
			if _, ok := served[c.ID]; !ok {
				return nil, fmt.Errorf("criteria[%d].id %q is not in the served acceptance_criteria_ids set", i, c.ID)
			}
		}
	}
	if coercedCriteria {
		normalized, merr := json.Marshal(criteria)
		if merr != nil {
			return nil, fmt.Errorf("re-marshal coerced criteria: %w", merr)
		}
		v.Criteria = normalized
		if warn != nil {
			warn("acceptance_verdict_criteria_coerced",
				fmt.Sprintf("coerced object-keyed criteria to %d sorted flat-array elements", len(criteria)))
		}
	}

	// Coerce evidence_hashes before the fail-closed reject: a string-valued
	// object map collapses to its sorted values (lossless); a non-string/
	// nested value or a scalar fails closed.
	hashes, coercedHashes, err := coerceAcceptanceEvidenceHashes(v.EvidenceHashes)
	if err != nil {
		return nil, err
	}
	if coercedHashes {
		normalized, merr := json.Marshal(hashes)
		if merr != nil {
			return nil, fmt.Errorf("re-marshal coerced evidence_hashes: %w", merr)
		}
		v.EvidenceHashes = normalized
		if warn != nil {
			warn("acceptance_verdict_evidence_hashes_coerced",
				fmt.Sprintf("coerced string-valued object-map evidence_hashes to %d sorted values", len(hashes)))
		}
	}

	// Coerce target_url before the fail-closed reject: a schemeless host[:port]
	// gains an http:// prefix; a foreign scheme (anything with "://" that is
	// not exactly http:// or https://) fails closed.
	coercedURL, err := coerceAcceptanceTargetURL(&v.TargetURL)
	if err != nil {
		return nil, err
	}
	if coercedURL && warn != nil {
		warn("acceptance_verdict_target_url_coerced",
			fmt.Sprintf("coerced schemeless target_url to %s", v.TargetURL))
	}

	// Re-marshal to the canonical shape only when a coercion fired, so the
	// common path preserves the original bytes byte-for-byte.
	if coercedHashes || coercedURL || coercedCriteria {
		out, merr := json.Marshal(v)
		if merr != nil {
			return nil, fmt.Errorf("re-marshal coerced acceptance verdict: %w", merr)
		}
		return out, nil
	}
	return raw, nil
}

// coerceAcceptanceEvidenceHashes normalizes the verdict's evidence_hashes
// field. It returns the flat slice, whether a coercion occurred, and an
// error on any lossy shape. The twin of the backend coerceEvidenceHashes
// (backend/internal/server/acceptance.go) — the two must stay identical or a
// runner-accepted verdict could be backend-rejected on ship. Accepted:
//   - absent / null / empty → nil, no coercion.
//   - a JSON array of strings → the array verbatim, no coercion (a non-string
//     element fails closed).
//   - a string-valued JSON object map (the #1574 variant) → its values,
//     SORTED, marked coerced; a non-string/nested value fails closed.
//   - anything else (a scalar) → fails closed.
func coerceAcceptanceEvidenceHashes(raw json.RawMessage) ([]string, bool, error) {
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

// coerceAcceptanceCriteria normalizes the verdict's criteria field. It returns
// the flat typed slice, whether a coercion occurred, and an error on any lossy
// or invalid shape. The twin of the backend coerceAcceptanceCriteria
// (backend/internal/server/acceptance.go) — the two must stay identical or a
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
// already carrying an exact http:// or https:// prefix passes through. ANY
// other value containing "://" (a foreign or near-miss scheme such as ftp://,
// httpx://, or http+unix://) fails closed — the check matches ONLY the two
// exact prefixes, never HasPrefix("http"). The twin of the backend helper of
// the same name.
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

// redactAcceptanceVerdict runs RedactDefault over the verdict bytes.
// Called BEFORE the bytes are embedded in the trace bundle or shipped:
// the verdict's prose fields carry text observed from the target
// instance, which is treated as potentially prompt-injected/hostile.
func redactAcceptanceVerdict(raw []byte) ([]byte, []redaction.Hit) {
	return redaction.RedactDefault(raw)
}

// composeAcceptanceEvidence wraps the (already redacted) verdict bytes
// in the acceptance_evidence trace event. Appended to res.Events before
// PackBytes so BOTH bundle variants carry the evidence record.
func composeAcceptanceEvidence(redactedVerdict []byte) agent.Event {
	return agent.Event{
		Kind:      "acceptance_evidence",
		Timestamp: time.Now().UTC(),
		Payload:   json.RawMessage(redactedVerdict),
	}
}
