package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// recordEscapeInput is the fishhawk_record_escape tool's input schema (E94.3 /
// #4694, ADR-117 D3 + Amendment A1). It mirrors `POST
// /v0/runs/{run_id}/escapes`, which has TWO shapes: a RECORD (any input
// without supersedes_sequence) and a CLOSE (supersedes_sequence +
// attestation). security_finding_id is deliberately absent (ADR-126 D7
// deferred to E96 / #4774): the backend refuses it.
type recordEscapeInput struct {
	RunID string `json:"run_id" jsonschema:"the run the escaped defect shipped from; the escape is recorded on THIS run's audit chain"`

	// RECORD fields.
	Source             string   `json:"source,omitempty" jsonschema:"RECORD: later_reviewer | follow_up_bug | operator_battery | human_finding | ci_red_on_main"`
	Severity           string   `json:"severity,omitempty" jsonschema:"RECORD: high | medium | low"`
	Category           string   `json:"category,omitempty" jsonschema:"RECORD (closed enum): correctness | security | reliability | concurrency | performance | data_integrity | test_coverage | documentation | other. Use security for ANY vulnerability"`
	Description        string   `json:"description,omitempty" jsonschema:"RECORD: what escaped, at most 4000 characters; required unless category=security, and REFUSED on a security escape"`
	Files              []string `json:"files,omitempty" jsonschema:"RECORD: 1..200 clean repo-relative paths the defect lives in; required unless category=security, and REFUSED on a security escape"`
	HeadSHA            string   `json:"head_sha,omitempty" jsonschema:"RECORD: the 40/64-char lowercase hex commit the defect shipped in; required when the run's PR is not observably merged"`
	IntroducingCommits []string `json:"introducing_commits,omitempty" jsonschema:"RECORD: at most 20 commit shas that introduced the defect (recorded as operator_supplied); REFUSED on a security escape"`
	ConcernID          string   `json:"concern_id,omitempty" jsonschema:"RECORD, later_reviewer only (required there): the stable concern UUID the later review raised. The escape dedupes on concern:<uuid> and carries that concern's review-round reviewed_head"`
	IdempotencyKey     string   `json:"idempotency_key,omitempty" jsonschema:"RECORD, every source EXCEPT later_reviewer (required there, refused on later_reviewer): ^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$; on category=security it must be a canonical lowercase UUID. A replay with the same key is refused escape_already_recorded"`
	SecurityClass      string   `json:"security_class,omitempty" jsonschema:"RECORD, category=security only (required there): an interim slug ^[a-z0-9][a-z0-9_]{0,63}$"`
	AdvisoryID         string   `json:"advisory_id,omitempty" jsonschema:"RECORD, category=security only (required there): the forge's draft-advisory id (a GHSA-xxxx-xxxx-xxxx id on a GitHub run)"`

	// CLOSE fields.
	SupersedesSequence int64                    `json:"supersedes_sequence,omitempty" jsonschema:"CLOSE: the audit_sequence of the pending_judgement birth to close. Its presence makes the call a CLOSE"`
	Attestation        *recordEscapeAttestation `json:"attestation,omitempty" jsonschema:"CLOSE (required there): the human judgement"`

	// Delegated follows drive_run.go's DriveStep.Delegated convention.
	Delegated bool `json:"delegated,omitempty" jsonschema:"the ADR-040 delegated-action marker: set it when you act under a delegation rather than on the human's own instruction. Caller-asserted. On a RECORD it makes the birth pending_judgement (agent_recorded); a CLOSE carrying it is refused escape_attestation_requires_human"`
}

// recordEscapeAttestation is a CLOSE's attestation.
type recordEscapeAttestation struct {
	Judgement string `json:"judgement" jsonschema:"defect (closes counted) | no_defect (closes dismissed)"`
	Note      string `json:"note,omitempty" jsonschema:"optional, at most 2000 characters; REFUSED on a security escape"`
}

// recordEscapeResult mirrors the backend's escapeResponse (server/escape.go);
// every json tag MUST byte-match or the field decodes to its zero value.
type recordEscapeResult struct {
	RunID              string `json:"run_id"`
	AuditSequence      int64  `json:"audit_sequence" jsonschema:"the escape_recorded row's audit sequence; a CLOSE names a birth by it"`
	RecordKind         string `json:"record_kind" jsonschema:"birth | closure"`
	State              string `json:"state" jsonschema:"counted | pending_judgement (birth); counted | dismissed (closure)"`
	JudgementBasis     string `json:"judgement_basis" jsonschema:"not_required | awaiting_attestation | operator_attestation"`
	PendingReason      string `json:"pending_reason,omitempty" jsonschema:"why a birth is pending: review_finding_source | agent_recorded | credential_not_forge_verified"`
	SourceEventKey     string `json:"source_event_key,omitempty" jsonschema:"a birth's dedupe key: concern:<uuid> or idempotency_key:<key>"`
	SupersedesSequence string `json:"supersedes_sequence,omitempty" jsonschema:"a closure's target birth sequence"`
}

// recordEscapeOutput wraps the decoded 201 body.
type recordEscapeOutput struct {
	Result recordEscapeResult `json:"result"`
}

// recordEscapeRecordBody mirrors server/escape.go's escapeRecordRequest. Every
// optional key is omitempty, so an absent input never becomes a PRESENT empty
// field (a security escape refuses a present description or files).
type recordEscapeRecordBody struct {
	Source             string   `json:"source"`
	Severity           string   `json:"severity"`
	Category           string   `json:"category"`
	Description        string   `json:"description,omitempty"`
	Files              []string `json:"files,omitempty"`
	HeadSHA            string   `json:"head_sha,omitempty"`
	IntroducingCommits []string `json:"introducing_commits,omitempty"`
	ConcernID          string   `json:"concern_id,omitempty"`
	IdempotencyKey     string   `json:"idempotency_key,omitempty"`
	SecurityClass      string   `json:"security_class,omitempty"`
	AdvisoryID         string   `json:"advisory_id,omitempty"`
	Delegated          bool     `json:"delegated"`
}

// recordEscapeCloseBody mirrors server/escape.go's escapeCloseRequest.
type recordEscapeCloseBody struct {
	SupersedesSequence int64                    `json:"supersedes_sequence"`
	Attestation        *recordEscapeAttestation `json:"attestation"`
	Delegated          bool                     `json:"delegated"`
}

// RecordEscape POSTs one RECORD or CLOSE body to `POST
// /v0/runs/{run_id}/escapes` (E94.3 / #4694). It lives here rather than in
// client.go, like resolve_concerns.go's method. 4xx/5xx surfaces as *apiError
// carrying the backend's refusal code.
func (c *apiClient) RecordEscape(ctx context.Context, runID uuid.UUID, body any) (*recordEscapeResult, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal escape: %w", err)
	}
	var out recordEscapeResult
	if err := c.do(ctx, http.MethodPost, "/v0/runs/"+runID.String()+"/escapes", raw, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// registerRecordEscape wires the fishhawk_record_escape tool (E94.3 / #4694).
// A later_reviewer escape is keyed concern:<concern_uuid> per #4690 ADR-117
// A1.2 as amended by erratum E1 (re-transcription PR #4784).
//
// Auth: write:runs (mcpscopes.go); the backend refuses every run-bound token.
func registerRecordEscape(srv *mcp.Server, resolver *runResolver) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "fishhawk_record_escape",
		Description: strings.TrimSpace(`
Record an ESCAPE — a defect that got past the live reviews — on the run it
shipped from, or close a pending one with your human judgement (ADR-117 D3
+ Amendment A1).

WHEN: a later reviewer, a follow-up bug, your own battery, a human, or a CI
red on main finds a defect in a change the reviews passed. Recording it is
what lets review effectiveness be measured.

ELIGIBILITY: write:runs. The agent INSIDE a run never records or closes an
escape: every run-bound (mcp:run:) token is refused run_token_forbidden.

TWO SHAPES, one tool:
  - RECORD (no supersedes_sequence): source, severity, category, plus
    description + files (non-security), concern_id (later_reviewer) or
    idempotency_key (every other source).
  - CLOSE (supersedes_sequence + attestation {judgement, note?}): closes the
    pending_judgement birth at that audit sequence. defect -> counted,
    no_defect -> dismissed. The closure copies the birth's escape verbatim.

BIRTH STATE: counted only when a human with a forge-verified credential
(GitHub/GitLab session or OAuth) records a non-later_reviewer escape.
Otherwise pending_judgement, with pending_reason review_finding_source
(later_reviewer), agent_recorded (an agent token or delegated:true) or
credential_not_forge_verified (e.g. a static api token).

ATTESTATION (CLOSE) is HUMAN-ONLY: an agent token or delegated:true is
refused escape_attestation_requires_human, and a credential that is not
forge-verified is refused escape_attestation_requires_forge_credential.
Recording stays open to agents; closing does not.

SECURITY: use category=security for ANY vulnerability. A security escape
carries ONLY its class (security_class slug) and the forge's advisory id
(advisory_id; a GHSA id on GitHub) — description, files and
introducing_commits are refused, and so is a closure note. Its
idempotency_key must be a canonical UUID. There is no security_finding_id
field (ADR-126 D7 deferred).

KEYS: a later_reviewer escape dedupes on concern:<concern_uuid> (one escape
per concern) and carries the reviewed_head of the review round that raised
the concern; other sources dedupe on idempotency_key:<key>. Exactly ONE
closure per birth.

Returns {run_id, audit_sequence, record_kind, state, judgement_basis,
pending_reason?, source_event_key?, supersedes_sequence?}.

Returns a tool error on:
  - an invalid run_id, a CLOSE missing attestation, attestation without
    supersedes_sequence, or a CLOSE mixing RECORD fields (caught before
    the HTTP hop)
  - authentication_required (401)
  - run_token_forbidden, insufficient_scope, account_forbidden (403)
  - escape_attestation_requires_human,
    escape_attestation_requires_forge_credential (403, CLOSE)
  - validation_failed (400, details.field names the field)
  - run_not_found (404)
  - escape_already_recorded (409, existing_sequence),
    escape_already_closed (409, successor_sequence)
  - escape_security_free_text_refused, escape_subject_unresolved,
    escape_concern_not_found, escape_target_not_found,
    escape_target_not_pending (422)
  - escape_unconfigured (503), internal_error (500)
`),
	}, resolver.recordEscape)
}

// recordEscapeHasRecordFields reports whether in carries any RECORD-only
// field, so a CLOSE cannot silently drop one.
func recordEscapeHasRecordFields(in recordEscapeInput) bool {
	return in.Source != "" || in.Severity != "" || in.Category != "" || in.Description != "" ||
		in.Files != nil || in.HeadSHA != "" || in.IntroducingCommits != nil || in.ConcernID != "" ||
		in.IdempotencyKey != "" || in.SecurityClass != "" || in.AdvisoryID != ""
}

// recordEscape is the tool handler. It validates the shape locally and
// forwards one body; every field rule, the birth rule and the attestation
// predicate live server-side in server/escape.go.
func (r *runResolver) recordEscape(ctx context.Context, _ *mcp.CallToolRequest, in recordEscapeInput) (*mcp.CallToolResult, recordEscapeOutput, error) {
	runID, err := uuid.Parse(in.RunID)
	if err != nil {
		return nil, recordEscapeOutput{}, fmt.Errorf("run_id %q is not a valid UUID: %w", in.RunID, err)
	}
	var body any
	if in.SupersedesSequence != 0 {
		if in.Attestation == nil {
			return nil, recordEscapeOutput{}, fmt.Errorf("a CLOSE (supersedes_sequence set) requires attestation {judgement, note?}")
		}
		if recordEscapeHasRecordFields(in) {
			return nil, recordEscapeOutput{}, fmt.Errorf("a CLOSE carries only supersedes_sequence, attestation and delegated; drop the RECORD fields")
		}
		body = recordEscapeCloseBody{SupersedesSequence: in.SupersedesSequence, Attestation: in.Attestation, Delegated: in.Delegated}
	} else {
		if in.Attestation != nil {
			return nil, recordEscapeOutput{}, fmt.Errorf("attestation is a CLOSE field: set supersedes_sequence to the birth's audit_sequence")
		}
		body = recordEscapeRecordBody{
			Source: in.Source, Severity: in.Severity, Category: in.Category, Description: in.Description,
			Files: in.Files, HeadSHA: in.HeadSHA, IntroducingCommits: in.IntroducingCommits,
			ConcernID: in.ConcernID, IdempotencyKey: in.IdempotencyKey,
			SecurityClass: in.SecurityClass, AdvisoryID: in.AdvisoryID, Delegated: in.Delegated,
		}
	}
	res, err := r.api.RecordEscape(ctx, runID, body)
	if err != nil {
		return nil, recordEscapeOutput{}, fmt.Errorf("record escape: %w", err)
	}
	return nil, recordEscapeOutput{Result: *res}, nil
}
