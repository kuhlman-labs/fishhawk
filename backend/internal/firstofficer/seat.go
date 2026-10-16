// Package firstofficer defines the ADR-089 first-officer seat identity (E85.6 /
// #4148): the seat token subject grammar, the fixed scope profile a seat holds,
// and the clamp the bearer middleware applies to every seat-subject token. It is
// a leaf package (strings + uuid only) so the server, and later the seat
// runtime, can share ONE definition of what a seat is. Long-form contract:
// README.md.
package firstofficer

import (
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// SubjectPrefix is the token-subject prefix every seat carries (ADR-089 D2's
// first_officer/<campaign-or-run-id>, with the anchor kind spelled out).
const SubjectPrefix = "first_officer/"

// AnchorKind is what a seat is attached to.
type AnchorKind string

// The two anchor kinds. Spelled out in the subject so the campaign and run
// UUID spaces can never collide.
const (
	AnchorCampaign AnchorKind = "campaign"
	AnchorRun      AnchorKind = "run"
)

// Anchor is the campaign or run a seat sits on.
type Anchor struct {
	Kind AnchorKind
	ID   uuid.UUID
}

// ErrInvalidSubject is returned by ParseSubject for a subject that carries the
// seat prefix but not the full first_officer/<kind>/<uuid> shape.
var ErrInvalidSubject = errors.New("firstofficer: invalid seat subject")

// The seat's scope profile (ADR-089 D2): read the run and its audit chain,
// write crew messages, request specialist jobs. Nothing that approves,
// dispatches, merges, reaps or mints a token.
const (
	ScopeReadRuns      = "read:runs"
	ScopeReadAudit     = "read:audit"
	ScopeWriteMessages = "write:messages"
	// ScopeRequestSpecialistJobs equals specialistjob.RequestScope; the leaf
	// package cannot import specialistjob, so seat_test.go pins the equality.
	ScopeRequestSpecialistJobs = "request:specialist_jobs"
)

// SeatScopes returns the fixed seat scope profile. A fresh slice per call, so
// no caller can widen the profile for every other caller by appending to it.
func SeatScopes() []string {
	return []string{ScopeReadRuns, ScopeReadAudit, ScopeWriteMessages, ScopeRequestSpecialistJobs}
}

// IsSeatSubject reports whether subject is a seat subject. It is a PREFIX
// match on purpose: a malformed first_officer/ subject is still a seat, so the
// middleware clamps it and refuses it rather than treating it as a human.
func IsSeatSubject(subject string) bool {
	return strings.HasPrefix(subject, SubjectPrefix)
}

// ParseSubject parses first_officer/campaign/<uuid> or first_officer/run/<uuid>.
// The UUID must be in canonical lowercase hyphenated form, so SubjectFor and
// ParseSubject round-trip exactly and one anchor has exactly one subject.
func ParseSubject(subject string) (Anchor, error) {
	rest, ok := strings.CutPrefix(subject, SubjectPrefix)
	if !ok {
		return Anchor{}, fmt.Errorf("%w: %q lacks the %q prefix", ErrInvalidSubject, subject, SubjectPrefix)
	}
	kind, raw, ok := strings.Cut(rest, "/")
	if !ok {
		return Anchor{}, fmt.Errorf("%w: %q has no anchor id", ErrInvalidSubject, subject)
	}
	k := AnchorKind(kind)
	if k != AnchorCampaign && k != AnchorRun {
		return Anchor{}, fmt.Errorf("%w: %q names unknown anchor kind %q", ErrInvalidSubject, subject, kind)
	}
	id, err := uuid.Parse(raw)
	if err != nil || id == uuid.Nil || id.String() != raw {
		return Anchor{}, fmt.Errorf("%w: %q anchor id is not a canonical UUID", ErrInvalidSubject, subject)
	}
	return Anchor{Kind: k, ID: id}, nil
}

// SubjectFor renders the seat subject for a.
func SubjectFor(a Anchor) string {
	return SubjectPrefix + string(a.Kind) + "/" + a.ID.String()
}

// ClampScopes returns in ∩ SeatScopes, in in's order with duplicates dropped.
// The middleware applies it to every seat-subject token, so a hand-minted seat
// token carrying write:approvals still cannot approve.
func ClampScopes(in []string) []string {
	allowed := map[string]bool{}
	for _, sc := range SeatScopes() {
		allowed[sc] = true
	}
	out := make([]string, 0, len(allowed))
	for _, sc := range in {
		if allowed[sc] {
			out = append(out, sc)
			allowed[sc] = false
		}
	}
	return out
}
