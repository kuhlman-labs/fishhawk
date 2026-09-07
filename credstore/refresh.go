package credstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// RefreshError is the typed failure of an RFC 6749 §6 refresh. Code is
// the §5.2 error code the authorization server returned (for example
// invalid_grant when the refresh token was consumed, revoked, or
// expired), Description its error_description, and StatusCode the
// HTTP status. Callers assert on Code rather than on message text.
type RefreshError struct {
	StatusCode  int
	Code        string
	Description string
}

func (e *RefreshError) Error() string {
	code := e.Code
	if code == "" {
		code = "unexpected response"
	}
	if e.Description != "" {
		return fmt.Sprintf("credstore: refresh refused (HTTP %d %s): %s", e.StatusCode, code, e.Description)
	}
	return fmt.Sprintf("credstore: refresh refused (HTTP %d %s)", e.StatusCode, code)
}

// ErrNotRefreshable means Refresh was handed a credential carrying no
// refresh token, client id, or token endpoint — nothing to refresh
// with. It is never dialed.
var ErrNotRefreshable = errors.New("credstore: credential carries no refresh token, client_id, or token_endpoint")

// maxRefreshBody bounds the token-endpoint response read so a
// misbehaving endpoint cannot make the client buffer without limit.
const maxRefreshBody = 1 << 20

// tokenResponse is the RFC 6749 §5.1 success body plus the §5.2
// error envelope, decoded from one shape so a non-2xx carrying either
// is read the same way.
type tokenResponse struct {
	AccessToken      string `json:"access_token"`
	TokenType        string `json:"token_type"`
	ExpiresIn        int64  `json:"expires_in"`
	RefreshToken     string `json:"refresh_token"`
	Scope            string `json:"scope"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// Refresh performs an RFC 6749 §6 refresh for c against c.TokenEndpoint
// as a PUBLIC client: the form carries grant_type=refresh_token, the
// refresh token, and client_id, with NO client_secret and NO
// Authorization header — the Fishhawk token endpoint refuses both
// outright for a token_endpoint_auth_method=none client. It returns a
// new Credential carrying the new access token, the ROTATED refresh
// token (the old one is consumed by the AS the moment the rotation
// commits, so the caller MUST persist the result — see RefreshStored),
// IssuedAt=now and ExpiresAt=now+expires_in, preserving Subject and
// Provider and taking Scopes from the response's scope when present.
//
// A non-2xx decodes the §5.2 envelope into a typed *RefreshError. A
// 2xx with no access_token is an error, never a silently empty bearer.
// A nil hc uses http.DefaultClient.
func Refresh(ctx context.Context, hc *http.Client, c Credential) (Credential, error) {
	return refreshAt(ctx, hc, c, time.Now())
}

// refreshAt is Refresh with an injectable clock so tests can pin the
// derived IssuedAt/ExpiresAt exactly.
func refreshAt(ctx context.Context, hc *http.Client, c Credential, now time.Time) (Credential, error) {
	if !c.Refreshable() {
		return Credential{}, ErrNotRefreshable
	}
	if hc == nil {
		hc = http.DefaultClient
	}
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", c.RefreshToken)
	form.Set("client_id", c.ClientID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return Credential{}, fmt.Errorf("credstore: build refresh request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := hc.Do(req)
	if err != nil {
		return Credential{}, fmt.Errorf("credstore: refresh against %s: %w", c.TokenEndpoint, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxRefreshBody))
	if err != nil {
		return Credential{}, fmt.Errorf("credstore: read refresh response: %w", err)
	}

	var tr tokenResponse
	decodeErr := json.Unmarshal(body, &tr)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		rerr := &RefreshError{StatusCode: resp.StatusCode}
		if decodeErr == nil {
			rerr.Code = tr.Error
			rerr.Description = tr.ErrorDescription
		} else {
			rerr.Description = truncate(string(body), 200)
		}
		return Credential{}, rerr
	}
	if decodeErr != nil {
		return Credential{}, fmt.Errorf("credstore: refresh response from %s is not JSON: %w", c.TokenEndpoint, decodeErr)
	}
	if tr.AccessToken == "" {
		return Credential{}, fmt.Errorf("credstore: refresh response from %s carries no access_token", c.TokenEndpoint)
	}

	fresh := Credential{
		Token:         tr.AccessToken,
		Subject:       c.Subject,
		Scopes:        c.Scopes,
		Provider:      c.Provider,
		RefreshToken:  tr.RefreshToken,
		ClientID:      c.ClientID,
		TokenEndpoint: c.TokenEndpoint,
	}
	if fresh.RefreshToken == "" {
		// RFC 6749 §6: an AS that issues no new refresh token leaves the
		// presented one valid. Fishhawk's AS always rotates; this keeps a
		// standards-conforming non-rotating AS refreshable.
		fresh.RefreshToken = c.RefreshToken
	}
	if tr.Scope != "" {
		fresh.Scopes = strings.Fields(tr.Scope)
	}
	lifetime := time.Duration(tr.ExpiresIn) * time.Second
	if lifetime <= 0 {
		// expires_in is only RECOMMENDED by §5.1. An absent value is read
		// as the shipped default rather than as non-expiring, so a
		// refreshable credential can never become one that is never
		// refreshed again.
		lifetime = DefaultLifetime
	}
	issued := now
	expires := now.Add(lifetime)
	fresh.IssuedAt = &issued
	fresh.ExpiresAt = &expires
	return fresh, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// RefreshStored refreshes the credential stored for backendURL if it
// NeedsRefresh, persists the rotation, and returns the credential a
// consumer should use now. The whole load → refresh → store sequence
// runs under an EXCLUSIVE file lock, and the credential is RE-READ
// after the lock is acquired: two consumers (the CLI and fishhawk-mcp,
// or two CLI invocations) sharing one store would otherwise both
// present the same refresh token, and the loser of that race would
// trip the AS's reuse detection and revoke the whole lineage. Under
// the lock the loser re-reads its sibling's freshly stored credential,
// finds it no longer needs refresh, and uses it without dialing.
//
// A not-needed refresh returns the stored credential unchanged. A
// refresh failure returns the error (a typed *RefreshError when the AS
// refused) and leaves the store untouched; a persist failure after a
// successful rotation is ALSO an error, because a rotated refresh
// token that is not on disk is burned.
//
// The lock is a sibling `credentials.lock` file rather than the
// credentials file itself: Store replaces the file by rename, so a
// lock on the credentials inode would not survive a sibling's Store.
func RefreshStored(ctx context.Context, hc *http.Client, backendURL string) (Credential, error) {
	return refreshStoredAt(ctx, hc, backendURL, time.Now())
}

func refreshStoredAt(ctx context.Context, hc *http.Client, backendURL string, now time.Time) (Credential, error) {
	d, err := configDir()
	if err != nil {
		return Credential{}, err
	}
	if err := os.MkdirAll(d, dirPerm); err != nil {
		return Credential{}, fmt.Errorf("credstore: create %s: %w", d, err)
	}
	unlock, err := lockExclusive(filepath.Join(d, fileName+".lock"))
	if err != nil {
		return Credential{}, err
	}
	defer unlock()

	// Re-read UNDER the lock: a sibling that held it first may have
	// rotated already, in which case this consumer must use its result
	// rather than re-present the consumed token.
	cred, err := Load(backendURL)
	if err != nil {
		return Credential{}, err
	}
	if !NeedsRefresh(cred, now) {
		return cred, nil
	}
	fresh, err := refreshAt(ctx, hc, cred, now)
	if err != nil {
		return Credential{}, err
	}
	if err := Store(backendURL, fresh); err != nil {
		return Credential{}, fmt.Errorf("credstore: persist rotated credential: %w", err)
	}
	return fresh, nil
}

// lockExclusive takes a blocking exclusive flock on path (created
// 0600 if absent) and returns the release func. flock is advisory and
// process-scoped, which is exactly the multi-process serialization
// RefreshStored needs; within one process the kernel still serializes
// distinct file descriptors, so concurrent goroutines are covered too.
func lockExclusive(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, filePerm) //nolint:gosec // path is under the 0700 config dir
	if err != nil {
		return nil, fmt.Errorf("credstore: open lock %s: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("credstore: lock %s: %w", path, err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}
