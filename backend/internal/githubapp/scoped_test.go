package githubapp

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
)

// stubTokenProvider records the installation id it was called with and
// returns a canned token or error.
type stubTokenProvider struct {
	calledWith int64
	called     bool
	token      string
	err        error
}

func (s *stubTokenProvider) Token(_ context.Context, installationID int64) (string, error) {
	s.called = true
	s.calledWith = installationID
	if s.err != nil {
		return "", s.err
	}
	return s.token, nil
}

func TestScopedProviderDelegatesWithParsedID(t *testing.T) {
	stub := &stubTokenProvider{token: "delegate-token"}
	p := NewScopedProvider(stub)

	got, err := p.Token(context.Background(), forge.FromGitHubInstallationID(4242))
	if err != nil {
		t.Fatalf("Token() error = %v, want nil", err)
	}
	if got != "delegate-token" {
		t.Fatalf("Token() = %q, want %q", got, "delegate-token")
	}
	if !stub.called || stub.calledWith != 4242 {
		t.Fatalf("delegate called with %d (called=%v), want 4242", stub.calledWith, stub.called)
	}
}

func TestScopedProviderZeroScopeFailsClosedWithoutDelegate(t *testing.T) {
	stub := &stubTokenProvider{token: "should-not-be-returned"}
	p := NewScopedProvider(stub)

	_, err := p.Token(context.Background(), forge.CredentialScope{})
	if err == nil {
		t.Fatal("Token() on zero scope: got nil error, want non-nil")
	}
	if stub.called {
		t.Fatal("Token() on zero scope invoked the delegate, want no invocation")
	}
}

func TestScopedProviderNonNumericRefFailsClosedNamingRef(t *testing.T) {
	stub := &stubTokenProvider{token: "should-not-be-returned"}
	p := NewScopedProvider(stub)

	_, err := p.Token(context.Background(), forge.FromRef("gitlab-group/42"))
	if err == nil {
		t.Fatal("Token() on non-numeric ref: got nil error, want non-nil")
	}
	if got := err.Error(); !strings.Contains(got, "gitlab-group/42") {
		t.Fatalf("Token() error = %q, want it to name the offending ref", got)
	}
	if stub.called {
		t.Fatal("Token() on non-numeric ref invoked the delegate, want no invocation")
	}
}

func TestScopedProviderNilTokensFailsClosedWithoutPanic(t *testing.T) {
	p := NewScopedProvider(nil)

	_, err := p.Token(context.Background(), forge.FromGitHubInstallationID(4242))
	if err == nil {
		t.Fatal("Token() with nil wrapped TokenProvider: got nil error, want non-nil")
	}
}

func TestScopedProviderPropagatesDelegateError(t *testing.T) {
	wantErr := errors.New("boom")
	stub := &stubTokenProvider{err: wantErr}
	p := NewScopedProvider(stub)

	_, err := p.Token(context.Background(), forge.FromGitHubInstallationID(4242))
	if !errors.Is(err, wantErr) {
		t.Fatalf("Token() error = %v, want it to wrap %v", err, wantErr)
	}
}
