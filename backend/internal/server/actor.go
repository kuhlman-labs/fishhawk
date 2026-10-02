package server

import (
	"strings"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/operatorrole"
)

// actorKindForSubject selects the audit actor kind for a delegated-
// action writer from the authenticated token subject (ADR-040 D4,
// #1027): a subject carrying the operator-agent token prefix is the
// role instance acting — actor_kind=agent — while every other subject
// (human tokens, GitHub logins, cookie sessions, the "anonymous"
// fallback) stays actor_kind=user. One shared definition of the "role
// instance vs human" distinction; mcp:run:<uuid> subjects never reach
// these writers with the prefix, so they classify as user here and are
// guarded by their own subject-binding checks upstream.
func actorKindForSubject(subject string) audit.ActorKind {
	if operatorrole.IsTokenSubject(subject) {
		return audit.ActorAgent
	}
	return audit.ActorUser
}

// isAgentSubject reports whether subject is an AGENT identity (E80.3 /
// #3760): the operator-agent token family (operatorrole.IsTokenSubject, the
// same prefix actorKindForSubject keys on) or a run-bound mcp:run:<uuid>
// subject, minted for the agent executing inside a run. Every other subject —
// a human token, a GitHub login, a cookie session — is a human.
//
// It is the ONE classification shared by the captain verbs
// (captainActorIsAgent) and the human-only server-check clearing guard
// (refuseNonHumanServerCheckClear), so the two cannot drift. It only
// CLASSIFIES; each caller owns its refusal. The residual is the ADR-040 trust
// model's: an agent session presenting a human operator's token is
// indistinguishable from that human and is treated as one.
func isAgentSubject(subject string) bool {
	return operatorrole.IsTokenSubject(subject) || strings.HasPrefix(subject, "mcp:run:")
}
