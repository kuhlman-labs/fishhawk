package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
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

// acceptanceVerdictPath is the file-fallback transport for the verdict
// (the codex path — Invocation.JSONSchema is a claudecode-only feature,
// so StructuredOutput stays nil there). MUST stay byte-identical to the
// path buildAcceptance's output contract names
// (backend/internal/prompt/prompt.go AcceptanceVerdictPath), mirroring
// the plan prompt's /tmp/fishhawk-plan.json convention. A package var
// (not const) so tests can point it at a temp file instead of the real
// shared /tmp path.
var acceptanceVerdictPath = "/tmp/fishhawk-acceptance.json"

// acceptanceVerdictJSONSchema is the structured-output schema for the
// claudecode --json-schema flag (Invocation.JSONSchema). Hand-authored
// to match the backend's acceptanceBody wire shape EXACTLY
// (backend/internal/server/acceptance.go) — the lockstep is guarded by
// TestAcceptanceVerdictSchema_LockstepWithValidator: a verdict this
// schema admits must pass validateAcceptanceVerdict, whose rules mirror
// the backend validator.
const acceptanceVerdictJSONSchema = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
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
    "evidence_hashes": {"type": "array", "items": {"type": "string"}}
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
type acceptanceVerdict struct {
	Verdict        string                      `json:"verdict"`
	FailureMode    string                      `json:"failure_mode,omitempty"`
	Criteria       []acceptanceCriterionResult `json:"criteria,omitempty"`
	TargetURL      string                      `json:"target_url,omitempty"`
	EvidenceHashes []string                    `json:"evidence_hashes,omitempty"`
}

// captureAcceptanceVerdict returns the agent's verdict bytes.
// Result.StructuredOutput (the claudecode --json-schema capture) is
// preferred; the fallback is reading path (the file the prompt's output
// contract tells a non-claudecode agent to write). Returns
// errAcceptanceVerdictMissing when neither transport produced anything.
func captureAcceptanceVerdict(res agent.Result, path string) ([]byte, error) {
	if len(res.StructuredOutput) > 0 {
		return res.StructuredOutput, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, errAcceptanceVerdictMissing
		}
		return nil, fmt.Errorf("acceptance verdict fallback read: %w", err)
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
func validateAcceptanceVerdict(raw []byte, servedCriteriaIDs []string) error {
	var v acceptanceVerdict
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		return fmt.Errorf("acceptance verdict could not be decoded: %w", err)
	}
	if dec.More() {
		return errors.New("acceptance verdict must be a single JSON object")
	}

	switch v.Verdict {
	case "passed":
		if v.FailureMode != "" {
			return fmt.Errorf("failure_mode must be omitted on a passed verdict, got %q", v.FailureMode)
		}
	case "failed":
		switch v.FailureMode {
		case "error", "assertion_fail":
			// ok
		case "":
			return errors.New("failure_mode is required when verdict is failed (error | assertion_fail)")
		default:
			return fmt.Errorf("failure_mode must be error or assertion_fail, got %q", v.FailureMode)
		}
	case "":
		return errors.New("verdict is required")
	default:
		return fmt.Errorf("verdict must be passed or failed, got %q", v.Verdict)
	}

	served := make(map[string]struct{}, len(servedCriteriaIDs))
	for _, id := range servedCriteriaIDs {
		served[id] = struct{}{}
	}
	for i, c := range v.Criteria {
		if c.ID == "" {
			return fmt.Errorf("criteria[%d].id is required (the plan-criterion join key)", i)
		}
		switch c.Result {
		case "passed", "failed", "skipped":
			// ok
		default:
			return fmt.Errorf("criteria[%d].result must be passed/failed/skipped, got %q", i, c.Result)
		}
		if len(served) > 0 {
			if _, ok := served[c.ID]; !ok {
				return fmt.Errorf("criteria[%d].id %q is not in the served acceptance_criteria_ids set", i, c.ID)
			}
		}
	}
	if v.TargetURL != "" && !strings.HasPrefix(v.TargetURL, "http") {
		return fmt.Errorf("target_url must be an http(s) URL when set, got %q", v.TargetURL)
	}
	return nil
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
