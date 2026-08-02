package oauthas

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestError_CodeDescriptionCauseRoundTrip(t *testing.T) {
	t.Parallel()
	cause := errors.New("internal detail: 10.0.0.5")
	e := newError(ErrCodeInvalidClient, "client rejected").withCause(cause)
	if e.Code != ErrCodeInvalidClient {
		t.Fatalf("Code = %q", e.Code)
	}
	if e.Description != "client rejected" {
		t.Fatalf("Description = %q", e.Description)
	}
	if !errors.Is(e, cause) {
		t.Fatalf("errors.Is(e, cause) = false")
	}
	if got := errors.Unwrap(e); got != cause {
		t.Fatalf("Unwrap = %v, want %v", got, cause)
	}
}

func TestError_IsCodeAxis(t *testing.T) {
	t.Parallel()
	e := newError(ErrCodeInvalidGrant, "bad grant").withCause(ErrPKCEMismatch)
	if !errors.Is(e, &Error{Code: ErrCodeInvalidGrant}) {
		t.Fatalf("errors.Is on code axis failed")
	}
	if errors.Is(e, &Error{Code: ErrCodeInvalidRequest}) {
		t.Fatalf("errors.Is matched the wrong code")
	}
	// A target *Error with an empty Code must NOT match everything.
	if errors.Is(e, &Error{}) {
		t.Fatalf("errors.Is matched an empty-code target")
	}
}

func TestError_IsSentinelAxis(t *testing.T) {
	t.Parallel()
	e := newError(ErrCodeInvalidGrant, "bad grant").withCause(ErrPKCEMismatch)
	if !errors.Is(e, ErrPKCEMismatch) {
		t.Fatalf("errors.Is on sentinel axis failed")
	}
	// A wrapped sentinel (with extra internal detail) is still switchable.
	e2 := newError(ErrCodeInvalidTarget, "blocked").withCause(fmt.Errorf("%w: 127.0.0.1", ErrBlockedAddress))
	if !errors.Is(e2, ErrBlockedAddress) {
		t.Fatalf("errors.Is on wrapped sentinel failed")
	}
	if errors.Is(e2, ErrPKCEMismatch) {
		t.Fatalf("errors.Is matched the wrong sentinel")
	}
}

func TestError_RendersCauseOnlyWhenSet(t *testing.T) {
	t.Parallel()
	withoutCause := newError(ErrCodeInvalidScope, "unsupported scope")
	if got := withoutCause.Error(); strings.Count(got, ":") != 1 || !strings.Contains(got, "invalid_scope: unsupported scope") {
		t.Fatalf("Error() without cause = %q", got)
	}
	withCause := newError(ErrCodeInvalidScope, "unsupported scope").withCause(errors.New("secret detail"))
	if got := withCause.Error(); !strings.Contains(got, "secret detail") {
		t.Fatalf("Error() with cause = %q, want cause rendered", got)
	}
}

func TestError_IsRejectsNonError(t *testing.T) {
	t.Parallel()
	e := newError(ErrCodeServerError, "boom")
	if e.Is(errors.New("plain")) {
		t.Fatalf("Is matched a non-*Error target")
	}
}
