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

// ResolveConcernsInput is the fishhawk_resolve_concerns tool's input schema
// (E83.53 / #4086). Mirrors `POST /v0/runs/{run_id}/concerns/resolve`: a list
// of the run's routed (addressed_pending) concern ids and ONE evidence note.
type ResolveConcernsInput struct {
	RunID      string   `json:"run_id" jsonschema:"the run whose concerns to resolve; every id must belong to THIS run"`
	ConcernIDs []string `json:"concern_ids" jsonschema:"the stable concern UUIDs to resolve (from fishhawk_get_gate_view or fishhawk_get_run_status's run.concerns.items[].id). Every id must be addressed_pending. At most 50 per batch; duplicates are rejected"`
	Evidence   string   `json:"evidence" jsonschema:"REQUIRED operator evidence note (at most 4000 bytes): what you executed or observed that shows the routed concern is fixed. Recorded on EVERY concern_resolved_with_evidence audit entry and stored as each concern's state_reason (prefixed 'operator evidence: ')"`
}

// ResolveConcernsOutput surfaces the per-item outcome list plus the counts.
type ResolveConcernsOutput struct {
	Result ResolveConcernsResult `json:"result"`
}

// ResolveConcernsItem is one concern's outcome. It mirrors the backend's
// resolveConcernsItemResult; every json tag MUST byte-match or the field
// decodes to its zero value silently.
type ResolveConcernsItem struct {
	ConcernID   string `json:"concern_id" jsonschema:"the concern this outcome belongs to"`
	Applied     bool   `json:"applied" jsonschema:"true when this concern was resolved; false when it failed mid-batch"`
	State       string `json:"state,omitempty" jsonschema:"the concern's state after the resolve (addressed) — present only when applied"`
	StateReason string `json:"state_reason,omitempty" jsonschema:"the stored reason ('operator evidence: <evidence>') — present only when applied"`
	ErrorCode   string `json:"error_code,omitempty" jsonschema:"concern_resolve_conflict, audit_append_failed or internal_error — present only when this item failed"`
	Error       string `json:"error,omitempty" jsonschema:"the failure detail for this item"`
}

// ResolveConcernsResult is the decoded 200 body of the resolve. Resolved +
// Failed always sum to len(Results), in REQUEST order.
type ResolveConcernsResult struct {
	RunID    string                `json:"run_id"`
	Evidence string                `json:"evidence" jsonschema:"the single evidence note recorded on every concern_resolved_with_evidence entry in the batch"`
	Resolved int                   `json:"resolved" jsonschema:"how many concerns were resolved to addressed"`
	Failed   int                   `json:"failed" jsonschema:"how many failed mid-batch; read results[] for which"`
	Results  []ResolveConcernsItem `json:"results" jsonschema:"per-concern outcome in request order"`
}

// resolveConcernsRequestBody mirrors the backend's resolveConcernsRequest
// (`backend/internal/server/resolve_concerns.go::resolveConcernsRequest`).
type resolveConcernsRequestBody struct {
	ConcernIDs []string `json:"concern_ids"`
	Evidence   string   `json:"evidence"`
}

// ResolveConcerns resolves a LIST of one run's addressed_pending concerns as
// addressed with ONE evidence note via
// `POST /v0/runs/{run_id}/concerns/resolve` (E83.53 / #4086). It lives here
// rather than in client.go, like crew_message.go's methods. The
// PRE-VALIDATION half is all-or-nothing; the APPLY half is per-item. 4xx/5xx
// surfaces as *apiError:
//   - 400 validation_failed (blank or over-cap evidence, empty concern_ids,
//     over the 50-id cap, a duplicate id, a non-UUID id, or an id belonging to
//     another run — details.rule "concern_run_mismatch")
//   - 401 authentication_required
//   - 403 resolve_requires_human (any agent token), insufficient_scope, or
//     account_forbidden
//   - 404 concern_not_found
//   - 409 concern_resolve_conflict (an id not in addressed_pending — the WHOLE
//     batch is refused and nothing is resolved)
//   - 503 concern_store_unconfigured
func (c *apiClient) ResolveConcerns(ctx context.Context, runID uuid.UUID, concernIDs []string, evidence string) (*ResolveConcernsResult, error) {
	body, err := json.Marshal(resolveConcernsRequestBody{ConcernIDs: concernIDs, Evidence: evidence})
	if err != nil {
		return nil, fmt.Errorf("marshal resolve concerns: %w", err)
	}
	var out ResolveConcernsResult
	if err := c.do(ctx, http.MethodPost, "/v0/runs/"+runID.String()+"/concerns/resolve", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// registerResolveConcerns wires the fishhawk_resolve_concerns tool (E83.53 /
// #4086): the human operator's counterpart to the operator_evidence_routed
// veto.
//
// Auth: write tool, the waive scope pair (write:stages or write:fixups); the
// backend refuses EVERY agent token, so it cannot be delegated.
func registerResolveConcerns(srv *mcp.Server, resolver *runResolver) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "fishhawk_resolve_concerns",
		Description: strings.TrimSpace(`
Resolve one run's ROUTED concerns as addressed, on your own operator
evidence. Human-only.

WHEN: you routed concerns back with fishhawk_fixup_stage carrying
operator_evidence, the fix-up landed, and you re-ran the reproduction.
A pass carrying operator_evidence makes its concerns immune to reviewer
auto-resolve (the operator_evidence_routed veto), so the re-review cannot
close them — this verb is how you do.

ELIGIBILITY: every id must be a concern of THIS run in addressed_pending
(routed by a fix-up). A raised or reopened concern must be routed first;
an already-closed concern has nothing to resolve. A HUMAN operator only:
the backend refuses every agent token (operator-agent/ or a run-bound
mcp:run: token, even on its own run) with resolve_requires_human, and
there is no delegated path — operator evidence is your authority claim.

This is NOT a waiver. The concern lands in addressed (the state a
reviewer confirmation produces), not waived, and a later review can still
reopen it. Use fishhawk_waive_concerns instead when a concern does not
warrant a change at all.

Each concern gets a concern_resolved_with_evidence audit entry FIRST
(durable before the state change), carrying your evidence; its
state_reason becomes "operator evidence: <evidence>".

The two halves have deliberately different atomicity:

  - PRE-VALIDATION is all-or-nothing and mutates NOTHING. The first
    violation refuses the WHOLE batch, naming the offending id.
  - the APPLY loop is per-item. A concurrent transition that raced the
    validation fails ONE concern (a concern_resolve_failed corrective
    entry is recorded); the rest still apply. Read results[].

Returns {resolved, failed, results[]} in request order; each result
carries concern_id, applied, and either state/state_reason or
error_code/error (concern_resolve_conflict, audit_append_failed,
internal_error).

Returns a tool error on:
  - invalid run_id or concern id UUID, empty concern_ids, or empty
    evidence (caught before the HTTP hop)
  - validation_failed (400: over the 50-id cap, evidence over 4000
    bytes, a duplicate id, or an id from another run — details.rule
    concern_run_mismatch)
  - resolve_requires_human (403: an agent token)
  - concern_not_found (404)
  - concern_resolve_conflict (409: an id not in addressed_pending — the
    WHOLE batch is refused and nothing is resolved)
  - concern_store_unconfigured (503)
`),
	}, resolver.resolveConcerns)
}

// resolveConcerns is the tool handler. It validates locally and passes the
// backend result through; the human-only guard, the pre-validation ladder and
// the audit-before-mutation ordering all live server-side in
// server/resolve_concerns.go.
func (r *runResolver) resolveConcerns(ctx context.Context, _ *mcp.CallToolRequest, in ResolveConcernsInput) (*mcp.CallToolResult, ResolveConcernsOutput, error) {
	runID, err := uuid.Parse(in.RunID)
	if err != nil {
		return nil, ResolveConcernsOutput{}, fmt.Errorf("run_id %q is not a valid UUID: %w", in.RunID, err)
	}
	if len(in.ConcernIDs) == 0 {
		return nil, ResolveConcernsOutput{}, fmt.Errorf("concern_ids must name at least one concern")
	}
	for _, raw := range in.ConcernIDs {
		if _, perr := uuid.Parse(raw); perr != nil {
			return nil, ResolveConcernsOutput{}, fmt.Errorf("concern_ids entry %q is not a valid UUID: %w", raw, perr)
		}
	}
	if strings.TrimSpace(in.Evidence) == "" {
		return nil, ResolveConcernsOutput{}, fmt.Errorf("evidence is required: the operator evidence note is audited on every concern in the batch and stored as its state_reason")
	}
	res, err := r.api.ResolveConcerns(ctx, runID, in.ConcernIDs, in.Evidence)
	if err != nil {
		return nil, ResolveConcernsOutput{}, fmt.Errorf("resolve concerns: %w", err)
	}
	return nil, ResolveConcernsOutput{Result: *res}, nil
}
