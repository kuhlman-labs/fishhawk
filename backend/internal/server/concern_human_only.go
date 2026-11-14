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
)

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
// row refuses the whole batch) and handleDeferConcern — each placed before the
// first audit append, delegation evaluation, work-item filing or state
// transition. Every verb passed here is in serverCheckClearAction.Verbs.
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
	action := verb
	if verb == clearVerbBulkWaive {
		action = clearVerbWaive
	}
	s.writeError(w, r, http.StatusForbidden, errCodeConcernRequiresHuman,
		fmt.Sprintf("concern %s was raised by a server check (provenance %s), not a model reviewer; "+
			"an agent token or a delegated request cannot %s it. A human operator must %s it with a reason, "+
			"or remove the flagged change, and rotate any credential it exposed",
			row.ID, row.Provenance, action, action),
		map[string]any{
			"concern_id":    row.ID.String(),
			"provenance":    row.Provenance,
			"verb":          verb,
			"refused_actor": refused,
		})
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
	s.writeError(w, r, http.StatusForbidden, errCodeConcernRequiresSession,
		fmt.Sprintf("concern %s was raised by a server check (provenance %s), not a model reviewer, and only a human "+
			"signed in to the Fishhawk Web UI can clear it; a request on the %s credential kind cannot, whatever its subject (ADR-143 D1). "+
			"Sign in to the Fishhawk Web UI through the forge and waive or defer it from the attention queue's decision panel, "+
			"or remove the flagged change, and rotate any credential it exposed. No CLI or MCP path exists (ADR-143 D3)",
			row.ID, row.Provenance, kind),
		map[string]any{
			"concern_id":      row.ID.String(),
			"provenance":      row.Provenance,
			"verb":            verb,
			"credential_kind": kind,
			"remedy":          "web_ui_session",
		})
	return true
}
