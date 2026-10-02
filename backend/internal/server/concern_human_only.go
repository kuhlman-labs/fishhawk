package server

import (
	"fmt"
	"net/http"

	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
)

// errCodeConcernRequiresHuman is the 403 error code the waive, bulk-waive and
// defer handlers return when a non-human actor tries to clear a
// server-synthesized concern (E80.3 / #3760).
const errCodeConcernRequiresHuman = "concern_requires_human"

// Clearing verbs named on the concern_requires_human refusal's details.verb.
const (
	clearVerbWaive     = "waive"
	clearVerbBulkWaive = "bulk_waive"
	clearVerbDefer     = "defer"
)

// refused_actor values on the concern_requires_human refusal: which arm of the
// guard fired. agent_token wins when both hold — the identity is the stronger
// fact, and a delegated:true flag from an agent token adds nothing to it.
const (
	refusedActorAgentToken = "agent_token"
	refusedActorDelegated  = "delegated"
)

// refuseNonHumanServerCheckClear is the human-only clearing guard for a
// server-synthesized concern (E80.3 / #3760, ADR-084 D5 / rule 5). A concern
// whose provenance is server_check was raised by a deterministic server check
// with no model call — the diff secrets check — and the whole point of that
// check is that no agent can talk its way past it. So when row.IsServerCheck()
// and EITHER the request is delegated (an ADR-040 delegated action, whatever
// token carried it) OR the subject is an agent identity (isAgentSubject: an
// operator-agent/ token or a run-bound mcp:run: token), it writes 403
// concern_requires_human and returns true; the caller must return without
// appending any audit entry, evaluating delegation, or producing any external
// side effect. Otherwise it returns false and writes nothing.
//
// It is provenance-scoped on purpose: a reviewer-raised concern (provenance
// empty) is untouched, so every existing agent and delegated waive/defer path
// behaves byte-identically. The clearing path it leaves open is a HUMAN
// waive (or defer) with a reason, e.g. "known test fixture".
//
// Exactly ONE call site per verb — handleWaiveConcern, handleBulkWaiveConcerns
// (per row, so any server_check row refuses the whole batch) and
// handleDeferConcern — each placed before the first audit append, delegation
// evaluation or work-item filing.
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
		return false
	}
	action := verb
	if verb == clearVerbBulkWaive {
		action = clearVerbWaive
	}
	s.writeError(w, r, http.StatusForbidden, errCodeConcernRequiresHuman,
		fmt.Sprintf("concern %s was raised by a server check (provenance %s), not a model reviewer; "+
			"an agent token or a delegated request cannot %s it. A human operator must %s it with a reason "+
			"(e.g. a known test fixture), or remove the flagged content and rotate any live credential",
			row.ID, row.Provenance, action, action),
		map[string]any{
			"concern_id":    row.ID.String(),
			"provenance":    row.Provenance,
			"verb":          verb,
			"refused_actor": refused,
		})
	return true
}
