package firstofficer

import (
	"errors"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/specialistjob"
)

func TestSubjectRoundTrip(t *testing.T) {
	id := uuid.MustParse("6f1c2a52-6a0e-4b8a-9d0c-1f2e3d4c5b6a")
	for _, kind := range []AnchorKind{AnchorCampaign, AnchorRun} {
		a := Anchor{Kind: kind, ID: id}
		subj := SubjectFor(a)
		if want := "first_officer/" + string(kind) + "/" + id.String(); subj != want {
			t.Fatalf("SubjectFor = %q, want %q", subj, want)
		}
		got, err := ParseSubject(subj)
		if err != nil {
			t.Fatalf("ParseSubject(%q): %v", subj, err)
		}
		if got != a {
			t.Errorf("round trip = %+v, want %+v", got, a)
		}
		if !IsSeatSubject(subj) {
			t.Errorf("IsSeatSubject(%q) = false", subj)
		}
	}
}

// TestParseSubject_Malformed pins that every malformed shape is refused, and
// that each is STILL a seat subject (prefix match), so the middleware clamps
// and refuses it instead of treating it as a human.
func TestParseSubject_Malformed(t *testing.T) {
	cases := []string{
		"first_officer/",
		"first_officer/run",
		"first_officer/run/",
		"first_officer/run/not-a-uuid",
		"first_officer/lease/6f1c2a52-6a0e-4b8a-9d0c-1f2e3d4c5b6a",
		"first_officer/6f1c2a52-6a0e-4b8a-9d0c-1f2e3d4c5b6a",
		"first_officer/run/6F1C2A52-6A0E-4B8A-9D0C-1F2E3D4C5B6A",
		"first_officer/run/00000000-0000-0000-0000-000000000000",
		"first_officer/run/6f1c2a52-6a0e-4b8a-9d0c-1f2e3d4c5b6a/extra",
	}
	for _, subj := range cases {
		if _, err := ParseSubject(subj); !errors.Is(err, ErrInvalidSubject) {
			t.Errorf("ParseSubject(%q) err = %v, want ErrInvalidSubject", subj, err)
		}
		if !IsSeatSubject(subj) {
			t.Errorf("IsSeatSubject(%q) = false; a malformed prefixed subject must still be clamped", subj)
		}
	}
	if _, err := ParseSubject("github:42"); !errors.Is(err, ErrInvalidSubject) {
		t.Errorf("ParseSubject(non-seat) err = %v, want ErrInvalidSubject", err)
	}
}

func TestIsSeatSubject_NonSeat(t *testing.T) {
	for _, subj := range []string{"", "anonymous", "github:42", "operator-agent/operator-role-v0",
		"mcp:run:6f1c2a52-6a0e-4b8a-9d0c-1f2e3d4c5b6a", "first_officer", "First_Officer/run/x", "x/first_officer/"} {
		if IsSeatSubject(subj) {
			t.Errorf("IsSeatSubject(%q) = true, want false", subj)
		}
	}
}

func TestClampScopes(t *testing.T) {
	in := []string{"write:approvals", "read:runs", "write:runs", "read:runs", "write:messages",
		"write:stages", "request:specialist_jobs", "read:audit", "attest:specialist-jobs"}
	got := ClampScopes(in)
	want := []string{"read:runs", "write:messages", "request:specialist_jobs", "read:audit"}
	if !slices.Equal(got, want) {
		t.Errorf("ClampScopes = %v, want %v", got, want)
	}
	if got := ClampScopes(nil); len(got) != 0 {
		t.Errorf("ClampScopes(nil) = %v, want empty", got)
	}
}

// TestSeatScopes_FixedProfile pins the ADR-089 D2 grant and that the returned
// slice is a copy (mutating it cannot widen the profile).
func TestSeatScopes_FixedProfile(t *testing.T) {
	want := []string{"read:runs", "read:audit", "write:messages", "request:specialist_jobs"}
	got := SeatScopes()
	if !slices.Equal(got, want) {
		t.Fatalf("SeatScopes = %v, want %v", got, want)
	}
	got[0] = "write:approvals"
	if SeatScopes()[0] != "read:runs" {
		t.Error("mutating the returned slice widened the shared profile")
	}
	for _, forbidden := range []string{"write:approvals", "write:runs", "write:stages", "write:fixups", "attest:specialist-jobs"} {
		if slices.Contains(SeatScopes(), forbidden) {
			t.Errorf("seat profile carries %q", forbidden)
		}
	}
}

// TestRequestScopeMatchesSpecialistJob pins the leaf package's literal to the
// constant the specialist-job handler enforces.
func TestRequestScopeMatchesSpecialistJob(t *testing.T) {
	if ScopeRequestSpecialistJobs != specialistjob.RequestScope {
		t.Errorf("ScopeRequestSpecialistJobs = %q, specialistjob.RequestScope = %q", ScopeRequestSpecialistJobs, specialistjob.RequestScope)
	}
}
