package hostdispatcher

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// TestScopes_ExactProfile pins the ratified ADR-104 Q4 profile. Counterfactual
// run by hand: appending "write:approvals" in the Scopes() body turned this
// test RED on both the exact-profile and the never-survives assertions.
func TestScopes_ExactProfile(t *testing.T) {
	want := []string{"read:runs", "read:audit", "write:runs", "write:stages"}
	if got := Scopes(); !slices.Equal(got, want) {
		t.Fatalf("Scopes() = %v, want exactly %v", got, want)
	}
	withheld := []string{
		"write:approvals", "write:campaigns", "write:fixups", "write:retries",
		"write:deploy", "read:audit-export", "write:messages", "request:specialist_jobs",
	}
	got := ClampScopes(append(append([]string{}, withheld...), want...))
	for _, sc := range withheld {
		if slices.Contains(got, sc) {
			t.Errorf("ClampScopes kept withheld scope %q: %v", sc, got)
		}
	}
	if !slices.Equal(got, want) {
		t.Errorf("ClampScopes(withheld+profile) = %v, want %v", got, want)
	}
}

func TestScopes_FreshSlice(t *testing.T) {
	a := Scopes()
	a[0] = "write:approvals"
	if Scopes()[0] != ScopeReadRuns {
		t.Fatal("Scopes() returned a shared slice; a caller widened the profile")
	}
}

func TestClampScopes_OrderAndDedup(t *testing.T) {
	got := ClampScopes([]string{"write:stages", "write:approvals", "read:runs", "write:stages"})
	if want := []string{"write:stages", "read:runs"}; !slices.Equal(got, want) {
		t.Fatalf("ClampScopes = %v, want %v", got, want)
	}
	if got := ClampScopes(nil); len(got) != 0 {
		t.Fatalf("ClampScopes(nil) = %v, want empty", got)
	}
}

func TestOutsideProfile(t *testing.T) {
	got := OutsideProfile([]string{"read:runs", "write:approvals", "write:stages", "write:deploy"})
	if want := []string{"write:approvals", "write:deploy"}; !slices.Equal(got, want) {
		t.Fatalf("OutsideProfile = %v, want %v", got, want)
	}
	if got := OutsideProfile(Scopes()); len(got) != 0 {
		t.Fatalf("OutsideProfile(Scopes()) = %v, want empty", got)
	}
}

// TestIsSubject_PrefixMatch: a malformed dispatcher subject is still a
// dispatcher (classified agent, refused), never a human.
func TestIsSubject_PrefixMatch(t *testing.T) {
	cases := map[string]bool{
		"host-dispatcher/devbox":       true,
		"host-dispatcher/":             true,
		"host-dispatcher/bad/label":    true,
		"host-dispatcher":              false,
		"github:host-dispatcher/x":     false,
		"operator-agent/operator-role": false,
		"":                             false,
	}
	for subject, want := range cases {
		if got := IsSubject(subject); got != want {
			t.Errorf("IsSubject(%q) = %v, want %v", subject, got, want)
		}
	}
}

func TestParseSubject(t *testing.T) {
	good := []string{"devbox", "mac-mini.local", "host_1", strings.Repeat("a", MaxLabelLen)}
	for _, label := range good {
		got, err := ParseSubject(SubjectPrefix + label)
		if err != nil || got != label {
			t.Errorf("ParseSubject(%q) = %q, %v; want %q, nil", SubjectPrefix+label, got, err, label)
		}
		subj, err := SubjectFor(label)
		if err != nil || subj != SubjectPrefix+label {
			t.Errorf("SubjectFor(%q) = %q, %v", label, subj, err)
		}
		if err := ValidateSubject(SubjectPrefix + label); err != nil {
			t.Errorf("ValidateSubject(%q) = %v", SubjectPrefix+label, err)
		}
	}
	bad := []string{
		"host-dispatcher/",
		"host-dispatcher/a/b",
		"host-dispatcher/has space",
		"host-dispatcher/" + strings.Repeat("a", MaxLabelLen+1),
		"host-dispatcher/café",
		"github:42",
	}
	for _, subject := range bad {
		if _, err := ParseSubject(subject); !errors.Is(err, ErrInvalidSubject) {
			t.Errorf("ParseSubject(%q) err = %v, want ErrInvalidSubject", subject, err)
		}
		if err := ValidateSubject(subject); !errors.Is(err, ErrInvalidSubject) {
			t.Errorf("ValidateSubject(%q) err = %v, want ErrInvalidSubject", subject, err)
		}
	}
	if _, err := SubjectFor(""); !errors.Is(err, ErrInvalidSubject) {
		t.Errorf("SubjectFor(\"\") err = %v, want ErrInvalidSubject", err)
	}
}
