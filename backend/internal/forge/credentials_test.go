package forge

import (
	"strings"
	"testing"
)

func TestFromGitHubInstallationIDRoundTrip(t *testing.T) {
	scope := FromGitHubInstallationID(4242)
	id, err := scope.GitHubInstallationID()
	if err != nil {
		t.Fatalf("GitHubInstallationID() error = %v, want nil", err)
	}
	if id != 4242 {
		t.Fatalf("GitHubInstallationID() = %d, want 4242", id)
	}
}

func TestFromGitHubInstallationIDZeroSentinel(t *testing.T) {
	scope := FromGitHubInstallationID(0)
	if !scope.IsZero() {
		t.Fatalf("IsZero() = false, want true for id 0")
	}
	if scope.Ref() != "" {
		t.Fatalf("Ref() = %q, want empty string", scope.Ref())
	}
}

func TestGitHubInstallationIDOnZeroScopeFailsClosed(t *testing.T) {
	var scope CredentialScope
	if _, err := scope.GitHubInstallationID(); err == nil {
		t.Fatal("GitHubInstallationID() on zero scope: got nil error, want non-nil")
	}
}

func TestGitHubInstallationIDOnNonNumericRefFailsClosed(t *testing.T) {
	scope := FromRef("gitlab-group/42")
	_, err := scope.GitHubInstallationID()
	if err == nil {
		t.Fatal("GitHubInstallationID() on non-numeric ref: got nil error, want non-nil")
	}
	if got := err.Error(); !strings.Contains(got, "gitlab-group/42") {
		t.Fatalf("GitHubInstallationID() error = %q, want it to name the offending ref", got)
	}
}

func TestRefReturnsCanonicalDecimalString(t *testing.T) {
	scope := FromGitHubInstallationID(4242)
	if scope.Ref() != "4242" {
		t.Fatalf("Ref() = %q, want %q", scope.Ref(), "4242")
	}
}

func TestFromRefIsVerbatimNoValidation(t *testing.T) {
	scope := FromRef("gitlab-group/42")
	if scope.IsZero() {
		t.Fatal("IsZero() = true for a non-empty ref, want false")
	}
	if scope.Ref() != "gitlab-group/42" {
		t.Fatalf("Ref() = %q, want %q", scope.Ref(), "gitlab-group/42")
	}
}
