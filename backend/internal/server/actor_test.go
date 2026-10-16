package server

import (
	"context"
	"net/http"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
)

// operatorAgentSubject is the canonical ADR-040 D4 token subject the
// attribution tests drive the handlers with.
const operatorAgentSubject = "operator-agent/operator-role-v0"

// operatorAgentIdentity returns a bearer-token Identity for an
// operator-agent role instance carrying the default operator scope set
// (#526) — the issuance shape `fishhawkd token issue --subject
// operator-agent/operator-role-v0` produces.
func operatorAgentIdentity() Identity {
	return Identity{
		Subject: operatorAgentSubject,
		TokenID: "tok-operator-agent",
		Scopes:  []string{"read:runs", "read:audit", "write:runs", "write:approvals", "write:stages"},
	}
}

// withOperatorAgentAuth injects the operator-agent identity into req's
// context, mirroring withAuth for the role-instance case.
func withOperatorAgentAuth(req *http.Request) *http.Request {
	return req.WithContext(context.WithValue(req.Context(), ctxKeyIdentity, operatorAgentIdentity()))
}

func TestActorKindForSubject(t *testing.T) {
	cases := []struct {
		name    string
		subject string
		want    audit.ActorKind
	}{
		{"operator-agent subject is agent", operatorAgentSubject, audit.ActorAgent},
		{"future role-spec version is agent", "operator-agent/operator-role-v1", audit.ActorAgent},
		{"human token subject is user", "github:42", audit.ActorUser},
		{"bootstrap subject is user", "bootstrap", audit.ActorUser},
		{"empty subject is user", "", audit.ActorUser},
		{"anonymous fallback is user", "anonymous", audit.ActorUser},
		{"mcp run-bound subject is user", "mcp:run:00000000-0000-0000-0000-000000000001", audit.ActorUser},
		{"bare operator-agent without slash is user", "operator-agent", audit.ActorUser},
		{"first-officer run seat is agent", "first_officer/run/6f1c2a52-6a0e-4b8a-9d0c-1f2e3d4c5b6a", audit.ActorAgent},
		{"first-officer campaign seat is agent", "first_officer/campaign/6f1c2a52-6a0e-4b8a-9d0c-1f2e3d4c5b6a", audit.ActorAgent},
		{"malformed first-officer subject is agent", "first_officer/bogus", audit.ActorAgent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := actorKindForSubject(tc.subject); got != tc.want {
				t.Errorf("actorKindForSubject(%q) = %q, want %q", tc.subject, got, tc.want)
			}
		})
	}
}

// TestIsAgentSubject_FirstOfficerSeat pins the seat rows of the shared agent
// classification (E85.6 / #4148): the captain verbs, the human-only
// server-check clearing guard and the re-run refusal all treat a seat as an
// agent. Counterfactual run by hand: removing the firstofficer.IsSeatSubject
// term from isAgentSubject turned this test RED on every seat row.
func TestIsAgentSubject_FirstOfficerSeat(t *testing.T) {
	cases := []struct {
		subject string
		want    bool
	}{
		{"first_officer/run/6f1c2a52-6a0e-4b8a-9d0c-1f2e3d4c5b6a", true},
		{"first_officer/campaign/6f1c2a52-6a0e-4b8a-9d0c-1f2e3d4c5b6a", true},
		{"first_officer/not-a-valid-seat", true},
		{"github:first_officer", false},
		{"first_officer", false},
	}
	for _, tc := range cases {
		if got := isAgentSubject(tc.subject); got != tc.want {
			t.Errorf("isAgentSubject(%q) = %v, want %v", tc.subject, got, tc.want)
		}
	}
}
