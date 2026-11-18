package server

import (
	"fmt"
	"net/http"

	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/identity"
)

// errCodeConcernRequiresHuman is the 403 error code the waive, bulk-waive and
// defer handlers return when a non-human actor tries to clear a
// server-synthesized concern (E80.3 / #3760).
const errCodeConcernRequiresHuman = "concern_requires_human"

// errCodeConcernRequiresSession is the 403 error code the four clearing verbs
// (waive, bulk waive, defer, resolve with operator evidence) return when a
// NON-agent, non-delegated request on any credential kind other than
// `session` tries to clear a server-synthesized concern (ADR-143 D1, #5228).
const errCodeConcernRequiresSession = "concern_requires_session"

// Clearing verbs named on the concern_requires_human refusal's details.verb.
const (
	clearVerbWaive     = "waive"
	clearVerbBulkWaive = "bulk_waive"
	clearVerbDefer     = "defer"
	clearVerbResolve   = "resolve_with_evidence"
	// The two waive-proposal decisions (E80.28 / #5229, ADR-143 A1). Confirm is
	// a clear; reject clears nothing but is the paired human decision on the
	// same concern, so it takes the same channel. Neither has an MCP or CLI
	// tool (webOnlyClearVerbs in the MCP parity test).
	clearVerbConfirmWaiveProposal = "confirm_waive_proposal"
	clearVerbRejectWaiveProposal  = "reject_waive_proposal"
)

// agentAlternativeProposeWaive is the details.agent_alternative pointer the
// four DIRECT clear verbs' refusals carry (ADR-143 A1, V3): an agent cannot
// clear a server-check concern, but it can propose the waive for a human to
// confirm. The confirm and reject refusals carry no pointer: an agent cannot
// decide its own proposal.
const agentAlternativeProposeWaive = "fishhawk_propose_waive"

// agentAlternativeSentence is the one sentence the direct verbs' refusals add.
const agentAlternativeSentence = ". An agent may instead propose the waive (fishhawk_propose_waive) " +
	"for a human to confirm in the Fishhawk Web UI"

// isWaiveProposalDecision reports whether verb decides a waive proposal
// (confirm or reject) rather than clearing the concern directly.
func isWaiveProposalDecision(verb string) bool {
	return verb == clearVerbConfirmWaiveProposal || verb == clearVerbRejectWaiveProposal
}

// waiveProposalDecisionAction renders a proposal-decision verb for a refusal
// message.
func waiveProposalDecisionAction(verb string) string {
	if verb == clearVerbRejectWaiveProposal {
		return "reject a waive proposal on"
	}
	return "confirm a waive proposal on"
}

// refused_actor values on the concern_requires_human refusal: which arm of the
// guard fired. agent_token wins when both hold — the identity is the stronger
// fact, and a delegated:true flag from an agent token adds nothing to it.
const (
	refusedActorAgentToken = "agent_token"
	refusedActorDelegated  = "delegated"
)

// refuseNonHumanServerCheckClear is the human-only clearing guard for a
// server-synthesized concern (E80.3 / #3760, ADR-084 D5 / rule 5, ADR-143 D1).
// A concern whose provenance is server_check was raised by a deterministic
// server check with no model call — the diff secrets check (E80.3) or the
// permission-drift check (E80.4 / #3761) — and the whole point of such a check
// is that no agent can talk its way past it. The refusal message is
// check-neutral for that reason: it names no single check's remedy.
//
// When row.IsServerCheck() the arms run in a fixed order, and the first that
// holds writes the 403 and returns true:
//
//  1. the subject is an agent identity (isAgentSubject: an operator-agent/
//     token, a run-bound mcp:run: token or a first-officer seat) →
//     concern_requires_human, refused_actor agent_token;
//  2. the request is delegated (an ADR-040 delegated action, whatever
//     credential carried it) → concern_requires_human, refused_actor
//     delegated;
//  3. ADR-143 D1, while the D2 entry server_check_clear carries its enforced
//     mark (humanOnlyActionEnforced): the request's credential kind, derived
//     by approvalCredentialKind from the server-set markers the auth
//     middleware put on the identity, is anything but `session` →
//     concern_requires_session. Nothing caller-set is consulted — no header,
//     user agent, Mcp-* header or body field — and the absence of an MCP
//     marker grants nothing. So a static, oauth, oauth_as, unrecorded or
//     unknown bearer (direct REST, the CLI, stdio MCP or HTTP /mcp) is
//     refused whatever its subject. An mcp_run credential always carries an
//     mcp:run: subject, so arm 1 refuses it first.
//
// Arms 1 and 2 are the pre-ADR-143 guard, kept byte-identical and evaluated
// first, so D1 adds a refusal and never admits what they refuse. On true the
// caller must return without appending any audit entry, evaluating
// delegation, or producing any external side effect. Otherwise it returns
// false and writes nothing.
//
// It is provenance-scoped on purpose: a reviewer-raised concern (provenance
// empty) is untouched, so every existing agent, delegated and bearer
// waive/defer/resolve path behaves byte-identically. The clearing path it
// leaves open is a HUMAN waive (or defer) with a reason from the Web UI
// session (the attention queue's decision panel). No CLI or MCP path exists
// (ADR-143 D3).
//
// Exactly ONE call site per verb — handleWaiveConcern, handleBulkWaiveConcerns
// and handleResolveConcerns (per row in pre-validation, so any server_check
// row refuses the whole batch), handleDeferConcern, and the waive-proposal
// decisions handleConfirmWaiveProposal and handleRejectWaiveProposal (ADR-143
// A1) — each placed before the first proposal read, audit append, delegation
// evaluation, work-item filing or state transition. Every verb passed here is
// in serverCheckClearAction.Verbs.
//
// The four DIRECT clear verbs' refusals (both codes) carry the additive
// details.agent_alternative = fishhawk_propose_waive and one sentence naming
// it; the confirm and reject refusals do not.
func (s *Server) refuseNonHumanServerCheckClear(w http.ResponseWriter, r *http.Request, row *concern.Concern, subject string, delegated bool, verb string) bool {
	if row == nil || !row.IsServerCheck() {
		return false
	}
	refused := ""
	switch {
	case isAgentSubject(subject):
		refused = refusedActorAgentToken
	case delegated:
		refused = refusedActorDelegated
	default:
		return s.refuseServerCheckClearOffSession(w, r, row, verb)
	}
	details := map[string]any{
		"concern_id":    row.ID.String(),
		"provenance":    row.Provenance,
		"verb":          verb,
		"refused_actor": refused,
	}
	var msg string
	if isWaiveProposalDecision(verb) {
		msg = fmt.Sprintf("concern %s was raised by a server check (provenance %s), not a model reviewer; "+
			"an agent token or a delegated request cannot %s it. Only a human signed in to the Fishhawk Web UI decides a "+
			"waive proposal, from the attention queue's decision panel; otherwise remove the flagged change, "+
			"and rotate any credential it exposed",
			row.ID, row.Provenance, waiveProposalDecisionAction(verb))
	} else {
		action := verb
		if verb == clearVerbBulkWaive {
			action = clearVerbWaive
		}
		msg = fmt.Sprintf("concern %s was raised by a server check (provenance %s), not a model reviewer; "+
			"an agent token or a delegated request cannot %s it. A human operator must %s it with a reason, "+
			"or remove the flagged change, and rotate any credential it exposed",
			row.ID, row.Provenance, action, action) + agentAlternativeSentence
		details["agent_alternative"] = agentAlternativeProposeWaive
	}
	s.writeError(w, r, http.StatusForbidden, errCodeConcernRequiresHuman, msg, details)
	return true
}

// refuseServerCheckClearOffSession is arm 3 of refuseNonHumanServerCheckClear
// (ADR-143 D1): it refuses a server-check clear on any credential kind but
// `session` with 403 concern_requires_session and returns true, or returns
// false and writes nothing. The credential kind comes only from the identity
// the auth middleware set (approvalCredentialKind); no request-supplied value
// is read.
func (s *Server) refuseServerCheckClearOffSession(w http.ResponseWriter, r *http.Request, row *concern.Concern, verb string) bool {
	if !humanOnlyActionEnforced(humanOnlyActionServerCheckClear) {
		return false
	}
	kind := approvalCredentialKind(IdentityFrom(r.Context()))
	if kind == identity.CredentialSession {
		return false
	}
	details := map[string]any{
		"concern_id":      row.ID.String(),
		"provenance":      row.Provenance,
		"verb":            verb,
		"credential_kind": kind,
		"remedy":          "web_ui_session",
	}
	var msg string
	if isWaiveProposalDecision(verb) {
		msg = fmt.Sprintf("concern %s was raised by a server check (provenance %s), not a model reviewer, and only a human "+
			"signed in to the Fishhawk Web UI can %s it; a request on the %s credential kind cannot, whatever its subject (ADR-143 D1, A1). "+
			"Sign in to the Fishhawk Web UI through the forge and use Confirm or Reject on the proposed waive in the attention queue's decision panel, "+
			"or remove the flagged change, and rotate any credential it exposed. No CLI or MCP path exists (ADR-143 D3)",
			row.ID, row.Provenance, waiveProposalDecisionAction(verb), kind)
	} else {
		msg = fmt.Sprintf("concern %s was raised by a server check (provenance %s), not a model reviewer, and only a human "+
			"signed in to the Fishhawk Web UI can clear it; a request on the %s credential kind cannot, whatever its subject (ADR-143 D1). "+
			"Sign in to the Fishhawk Web UI through the forge and waive or defer it from the attention queue's decision panel, "+
			"or remove the flagged change, and rotate any credential it exposed. No CLI or MCP path exists (ADR-143 D3)",
			row.ID, row.Provenance, kind) + agentAlternativeSentence
		details["agent_alternative"] = agentAlternativeProposeWaive
	}
	s.writeError(w, r, http.StatusForbidden, errCodeConcernRequiresSession, msg, details)
	return true
}
