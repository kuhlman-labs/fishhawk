package oauthas

import (
	"errors"
	"fmt"
)

// ErrorCode is an RFC 6749 §4.1.2.1 / §5.2 (and RFC 8707) OAuth error code.
// It is the SAFE, client-returnable identifier for a failure; #2391's handlers
// render Code plus Description to the client and log Cause only.
type ErrorCode string

// RFC 6749 §4.1.2.1 / §5.2 error codes, plus RFC 8707 invalid_target used by
// the resource-identifier check. These are the only codes this package emits.
const (
	// ErrCodeInvalidRequest is RFC 6749 invalid_request.
	ErrCodeInvalidRequest ErrorCode = "invalid_request"
	// ErrCodeInvalidClient is RFC 6749 invalid_client.
	ErrCodeInvalidClient ErrorCode = "invalid_client"
	// ErrCodeInvalidGrant is RFC 6749 invalid_grant.
	ErrCodeInvalidGrant ErrorCode = "invalid_grant"
	// ErrCodeUnauthorizedClient is RFC 6749 unauthorized_client.
	ErrCodeUnauthorizedClient ErrorCode = "unauthorized_client"
	// ErrCodeUnsupportedGrantType is RFC 6749 unsupported_grant_type.
	ErrCodeUnsupportedGrantType ErrorCode = "unsupported_grant_type"
	// ErrCodeUnsupportedResponseType is RFC 6749 unsupported_response_type.
	ErrCodeUnsupportedResponseType ErrorCode = "unsupported_response_type"
	// ErrCodeInvalidScope is RFC 6749 invalid_scope.
	ErrCodeInvalidScope ErrorCode = "invalid_scope"
	// ErrCodeAccessDenied is RFC 6749 access_denied.
	ErrCodeAccessDenied ErrorCode = "access_denied"
	// ErrCodeServerError is RFC 6749 server_error.
	ErrCodeServerError ErrorCode = "server_error"
	// ErrCodeTemporarilyUnavailable is RFC 6749 temporarily_unavailable.
	ErrCodeTemporarilyUnavailable ErrorCode = "temporarily_unavailable"
	// ErrCodeInvalidTarget is RFC 8707 §2 invalid_target.
	ErrCodeInvalidTarget ErrorCode = "invalid_target"
)

// Sentinel wrappers callers switch on with errors.Is. Each is carried in an
// [Error]'s Cause chain so both the code axis (errors.Is(err, &Error{Code: …}))
// and the sentinel axis (errors.Is(err, ErrPKCEMismatch)) are switchable.
var (
	// ErrBlockedAddress marks a connect-time SSRF-guard refusal.
	ErrBlockedAddress = errors.New("oauthas: destination address is blocked")
	// ErrRedirectMismatch marks an RFC 8252 §7.3 redirect-URI matcher refusal.
	ErrRedirectMismatch = errors.New("oauthas: redirect URI does not match")
	// ErrPKCEMismatch marks a failed S256 PKCE verification.
	ErrPKCEMismatch = errors.New("oauthas: PKCE verification failed")
	// ErrDocumentTooLarge marks a client-metadata document over the byte cap.
	ErrDocumentTooLarge = errors.New("oauthas: client metadata document too large")
	// ErrNotSelfReferential marks a client-metadata document that fails the
	// self-referential / shape validation admitting it.
	ErrNotSelfReferential = errors.New("oauthas: client metadata is not self-referential")
)

// Error is a typed OAuth error. Description is the SAFE, client-returnable text;
// Cause is the internal detail (which may carry an internal address or host) that
// #2391's handlers log but never render, so a CIMD fetch failure cannot leak an
// internal address into an OAuth error response.
type Error struct {
	// Code is the client-returnable OAuth error code.
	Code ErrorCode
	// Description is the client-returnable human-readable text. It never echoes
	// attacker-supplied input verbatim.
	Description string
	// Cause is the internal detail; log-only, never rendered to a client. It may
	// wrap a sentinel so errors.Is on the sentinel axis works.
	Cause error
}

// Error renders the code and description, and the cause only when it is set.
func (e *Error) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Description, e.Cause)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Description)
}

// Unwrap exposes the internal cause (and any sentinel it wraps) so errors.Is and
// errors.As traverse into it.
func (e *Error) Unwrap() error { return e.Cause }

// Is reports true when target is an *Error naming the same non-empty Code, so
// errors.Is(err, &Error{Code: ErrCodeInvalidGrant}) matches on the code axis.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	if !ok {
		return false
	}
	return t.Code != "" && t.Code == e.Code
}

// newError builds an *Error with a formatted, client-safe Description and no
// cause. Attach an internal cause with withCause.
func newError(code ErrorCode, format string, args ...any) *Error {
	return &Error{Code: code, Description: fmt.Sprintf(format, args...)}
}

// withCause attaches the internal cause and returns the receiver for chaining.
func (e *Error) withCause(cause error) *Error {
	e.Cause = cause
	return e
}
