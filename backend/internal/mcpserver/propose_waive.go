package mcpserver

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ProposeWaiveInput is the fishhawk_propose_waive tool's input schema (E80.28
// / #5229, ADR-143 A1). Mirrors `POST /v0/concerns/{concern_id}/waive-proposal`.
// concern_id, reason and evidence are schema-required. Judgement and
// TradeoffCost are REQUIRED by the backend but omitempty here, like every
// concern judgement verb (TestConcernJudgementInputs_NotSchemaRequired): the
// backend is the single refusal authority, and the tool pre-checks nothing
// beyond the concern UUID.
type ProposeWaiveInput struct {
	ConcernID       string `json:"concern_id" jsonschema:"the stable UUID of an OPEN server_check concern (from fishhawk_get_gate_view's open[].id); a reviewer-raised concern is refused (concern_not_server_check) and is waived directly instead"`
	Reason          string `json:"reason" jsonschema:"REQUIRED: why the flagged change does not warrant a fix (at most 4000 bytes, refused rather than truncated). Shown to the human labelled agent-authored; it never becomes the concern's state_reason. Never paste the matched bytes: credential-shaped text is refused (waive_proposal_contains_credential)"`
	Evidence        string `json:"evidence" jsonschema:"REQUIRED: what you checked that supports the reason, e.g. the fixture or test that owns the flagged line (at most 4000 bytes). Never paste the matched bytes: credential-shaped text is refused"`
	Judgement       string `json:"judgement,omitempty" jsonschema:"REQUIRED (enforced by the backend): one of defect, defect_pre_existing, no_defect, tradeoff, preference, stale, insufficient_evidence; unlabelled is refused. 400 validation_failed when missing or invalid"`
	TradeoffCost    string `json:"tradeoff_cost,omitempty" jsonschema:"the stated cost of a tradeoff judgement (at most 4000 bytes): REQUIRED with tradeoff and refused with any other judgement. Credential-scanned like reason and evidence"`
	ExpectedHeadSHA string `json:"expected_head_sha,omitempty" jsonschema:"optional: the run head you reviewed. When set and the run's newest RECORDED head differs, the proposal is refused (waive_proposal_head_moved) instead of binding to a head you did not see"`
}

// ProposeWaiveOutput is the decoded 201 body: the recorded proposal, the
// acting-identity echo (proposed_by, credential_kind, #5227) and the server's
// next_step, which names the human Web UI confirm.
type ProposeWaiveOutput struct {
	Proposal       WaiveProposal `json:"proposal" jsonschema:"the recorded pending proposal; proposal_sequence is what a human's confirm or reject names"`
	ProposedBy     string        `json:"proposed_by" jsonschema:"the subject the proposal was recorded under"`
	CredentialKind string        `json:"credential_kind" jsonschema:"the credential kind the backend resolved for the caller"`
	NextStep       string        `json:"next_step" jsonschema:"the server's statement of who decides next: a human signed in to the Fishhawk Web UI, from the attention queue's decision panel; there is no CLI or MCP confirm"`
}

// registerProposeWaive wires the fishhawk_propose_waive tool (E80.28 / #5229,
// ADR-143 A1): the agent half of propose-and-confirm for server-check
// concerns. There is deliberately NO confirm or reject tool: both are D2
// verbs of ADR-143's server_check_clear entry and take a Web UI session.
//
// Auth: write tool, the waive scope pair (write:stages or write:fixups). The
// backend refuses a run-bound (fhm_ / mcp:run:) token with
// run_token_forbidden, because the run's own agent is the party the check
// guards.
func registerProposeWaive(srv *mcp.Server, resolver *runResolver) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "fishhawk_propose_waive",
		Description: strings.TrimSpace(`
Propose a waive of one OPEN server-check concern for a human to confirm.
It clears nothing.

WHEN: a server_check concern (diff secrets, permission drift,
control_unpinned, diff_secrets_marker_suppressed) is open, you believe
it is a false positive or an accepted trade-off, and fishhawk_waive_concern
or fishhawk_defer_concern refused you with concern_requires_human or
concern_requires_session (that refusal's details.agent_alternative names
this tool). Clearing a server-check concern is human-only (ADR-143):
an agent may only PROPOSE.

ELIGIBILITY: an operator-agent or other non-run-bound credential with
write:stages or write:fixups. A run-bound (fhm_ / mcp:run:) token is
refused with run_token_forbidden: the run's own agent is the party the
check guards. A reviewer-raised concern is refused
(concern_not_server_check); waive it directly.

What it records: one concern_waive_proposed audit row carrying your
judgement, reason and evidence, bound to the run's newest RECORDED head.
The flagged location and pattern class come from the concern's
server-written check_key, never from your text. The concern stays open,
so the merge gate and delegated settlement are unchanged.

Rules:
  - NEVER paste the matched bytes. The server refuses credential-shaped
    text in reason, evidence or tradeoff_cost
    (waive_proposal_contains_credential; details name the field and the
    pattern class only). The scan is best effort, so describe the match
    instead of quoting it.
  - One live proposal per concern (waive_proposal_exists names the live
    proposal_sequence).
  - A moved head drops it: once the recorded run head moves past the
    proposal's head it is shown nowhere and cannot be confirmed; propose
    again at the new head.
  - Only a human signed in to the Fishhawk Web UI confirms or rejects it,
    from the attention queue's decision panel. There is no MCP or CLI
    confirm or reject. fishhawk_get_run_status's next_actions shows a
    display-only confirm_waive_proposal entry while it is pending, and
    fishhawk_get_gate_view shows the waive_proposal block on the concern.

Inputs:
  - concern_id        : the open server_check concern's UUID.
  - reason            : REQUIRED (at most 4000 bytes).
  - evidence          : REQUIRED (at most 4000 bytes).
  - judgement         : REQUIRED by the backend (defect,
    defect_pre_existing, no_defect, tradeoff, preference, stale,
    insufficient_evidence).
  - tradeoff_cost     : REQUIRED with tradeoff, refused otherwise.
  - expected_head_sha : optional; refuse instead of binding a head you did
    not review.

Returns the proposal (proposal_sequence, proposed_by,
proposed_by_actor_kind, head_sha, check_key, agent_authored), plus
proposed_by, credential_kind and next_step.

Returns a tool error on:
  - invalid concern_id UUID (caught before the HTTP hop)
  - validation_failed (400: blank or over-long reason or evidence, the
    judgement rules), waive_proposal_contains_credential (400)
  - run_token_forbidden, insufficient_scope, account_forbidden (403)
  - concern_not_found (404), concern_not_server_check (422)
  - concern_not_open, waive_proposal_head_unresolved (no head recorded
    yet), waive_proposal_head_moved, waive_proposal_exists (409)
  - internal_error / audit_append_failed (500: nothing was written)
  - concern_store_unconfigured, waive_proposal_atomic_append_unavailable
    (503: the proposal is never written non-atomically)
`),
	}, resolver.proposeWaive)
}

// proposeWaive is the tool handler. It validates the concern UUID locally and
// passes the backend result through; the run-bound refusal, the credential
// scan, the head binding and the one-live rule all live server-side in
// server/waive_proposal.go.
func (r *runResolver) proposeWaive(ctx context.Context, _ *mcp.CallToolRequest, in ProposeWaiveInput) (*mcp.CallToolResult, ProposeWaiveOutput, error) {
	concernID, err := uuid.Parse(in.ConcernID)
	if err != nil {
		return nil, ProposeWaiveOutput{}, fmt.Errorf("concern_id %q is not a valid UUID: %w", in.ConcernID, err)
	}
	out, err := r.api.ProposeWaive(ctx, concernID, in.Reason, in.Evidence, in.Judgement, in.TradeoffCost, in.ExpectedHeadSHA)
	if err != nil {
		return nil, ProposeWaiveOutput{}, fmt.Errorf("propose waive: %w", err)
	}
	return nil, *out, nil
}
