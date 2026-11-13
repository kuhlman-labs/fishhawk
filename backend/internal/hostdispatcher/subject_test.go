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

// TestRouteAllowed pins the allowlist matcher: every allowlisted pattern
// matches its own concrete path, neighbours and malformed paths do not.
// Counterfactual run by hand: making RouteAllowed return true for every
// method turned the wrong-method rows RED.
func TestRouteAllowed(t *testing.T) {
	const id = "0b6a3b8e-0d2f-4a38-9a66-1f5bfb3c2d10"
	for _, c := range []struct {
		method, path string
		want         bool
	}{
		{"GET", "/v0/runs", true},
		{"GET", "/v0/runs/" + id, true},
		{"GET", "/v0/runs/" + id + "/stages", true},
		{"POST", "/v0/runs/" + id + "/auto-drive/acts", true},
		{"POST", "/v0/runs/" + id + "/stages/" + id + "/host-dispatch", true},
		{"POST", "/v0/runs/" + id + "/stages/" + id + "/reap-failure", true},
		{"POST", "/v0/stages/" + id + "/acceptance-admission", true},
		{"POST", "/v0/runs", false},
		{"HEAD", "/v0/runs", false},
		{"POST", "/v0/runs/" + id, false},
		{"POST", "/v0/runs/" + id + "/recover", false},
		{"POST", "/v0/runs/" + id + "/cancel", false},
		{"POST", "/v0/runs/" + id + "/scope-amendments/" + id + "/decision", false},
		{"POST", "/v0/crew-messages/1/escalation-decision", false},
		{"GET", "/v0/runs/", false},
		{"GET", "/v0/runs/" + id + "/", false},
		{"GET", "//v0/runs", false},
		{"GET", "/v0/runs/../tokens", false},
		{"GET", "/v0/runs/" + id + "%2Fstages", false},
		{"GET", "/v0/runs/%zz", false},
		{"GET", "v0/runs", false},
		{"GET", "/v0/runs/" + id + "/audit", false},
	} {
		if got := RouteAllowed(c.method, c.path); got != c.want {
			t.Errorf("RouteAllowed(%s, %s) = %v, want %v", c.method, c.path, got, c.want)
		}
	}
	r := Routes()
	r[0] = "POST /v0/runs"
	if RouteAllowed("POST", "/v0/runs") {
		t.Fatal("Routes() returned the shared slice; a caller widened the allowlist")
	}
}
