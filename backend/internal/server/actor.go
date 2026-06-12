package server

import (
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
