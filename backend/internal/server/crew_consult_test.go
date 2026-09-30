package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/crewmessage"
)

// fakeResponder is a deterministic CrewResponder. When gate is non-nil it
// blocks until gate closes (or ctx ends); err makes it fail.
type fakeResponder struct {
	answer CrewConsultAnswer
	err    error
	gate   chan struct{}
	seen   chan CrewConsultRequest
}

func (f *fakeResponder) Respond(ctx context.Context, req CrewConsultRequest) (CrewConsultAnswer, error) {
	if f.seen != nil {
		select {
		case f.seen <- req:
		default:
		}
	}
	if f.gate != nil {
		select {
		case <-f.gate:
		case <-ctx.Done():
			// Deliberately keep blocking past ctx: the dispatcher must not
			// rely on the responder honouring its deadline.
			<-f.gate
		}
	}
	return f.answer, f.err
}

// (C3) the registry refuses an implementer registration at construction.
func TestCrewResponderRegistry_RefusesImplementer(t *testing.T) {
	_, err := NewCrewResponderRegistry(map[crewmessage.Role]CrewResponder{
		crewmessage.RoleHistorian:   &fakeResponder{},
		crewmessage.RoleImplementer: &fakeResponder{},
	})
	if err == nil || !strings.Contains(err.Error(), "implement stage") {
		t.Fatalf("err = %v, want an implementer refusal", err)
	}
}

func TestCrewResponderRegistry_RefusesUnknownRoleAndNilResponder(t *testing.T) {
	if _, err := NewCrewResponderRegistry(map[crewmessage.Role]CrewResponder{"claude-opus-5": &fakeResponder{}}); err == nil ||
		!strings.Contains(err.Error(), "not a crew role") {
		t.Fatalf("unknown role err = %v, want not-a-crew-role", err)
	}
	if _, err := NewCrewResponderRegistry(map[crewmessage.Role]CrewResponder{crewmessage.RoleHistorian: nil}); err == nil ||
		!strings.Contains(err.Error(), "nil responder") {
		t.Fatalf("nil responder err = %v, want nil-responder refusal", err)
	}
}

func TestCrewResponderRegistry_LookupIsClosed(t *testing.T) {
	h := &fakeResponder{}
	in := map[crewmessage.Role]CrewResponder{crewmessage.RoleHistorian: h}
	reg, err := NewCrewResponderRegistry(in)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	// A caller mutation after construction cannot widen the registry.
	in[crewmessage.RoleSecurity] = &fakeResponder{}
	if got, ok := reg.Lookup(crewmessage.RoleHistorian); !ok || got != h {
		t.Fatalf("historian lookup = %v, %v", got, ok)
	}
	if _, ok := reg.Lookup(crewmessage.RoleSecurity); ok {
		t.Fatal("security resolved after a post-construction mutation of the input map")
	}
	var zero CrewResponderRegistry
	if _, ok := zero.Lookup(crewmessage.RoleHistorian); ok {
		t.Fatal("the zero-value (production) registry resolved a responder")
	}
	empty, err := NewCrewResponderRegistry(nil)
	if err != nil {
		t.Fatalf("empty registry: %v", err)
	}
	if _, ok := empty.Lookup(crewmessage.RoleHistorian); ok {
		t.Fatal("the empty registry resolved a responder")
	}
}

// A sender may ask for LESS time than the window, never more.
func TestCrewConsultDeadline(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	window := now.Add(crewConsultWindow)
	for name, tc := range map[string]struct {
		deadline string
		want     time.Time
	}{
		"absent":          {"", window},
		"shorter":         {"2026-09-30T12:05:00Z", now.Add(5 * time.Minute)},
		"shorter_lower_z": {"2026-09-30t12:05:00z", now.Add(5 * time.Minute)},
		"longer_clamped":  {"2026-09-30T13:00:00Z", window},
		"unparseable":     {"not-a-date", window},
	} {
		t.Run(name, func(t *testing.T) {
			got := crewConsultDeadline(&crewmessage.Message{Deadline: tc.deadline}, now)
			if !got.Equal(tc.want) {
				t.Fatalf("deadline = %v, want %v", got, tc.want)
			}
		})
	}
}
