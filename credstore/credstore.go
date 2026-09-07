// Package credstore persists minted Fishhawk bearer tokens on the
// local filesystem so `fishhawk` subcommands can authenticate
// without re-passing --token on every call.
//
// The store is a single JSON file under the XDG config directory
// (`$XDG_CONFIG_HOME/fishhawk/credentials`, falling back to
// `~/.config/fishhawk/credentials`). It maps a backend URL to the
// credential minted for it, so an operator can hold tokens for
// several backends at once and the CLI picks the one matching
// `--backend-url`. The file is written 0600 (owner read/write only)
// because it holds live bearer secrets; the containing directory is
// 0700 for the same reason.
package credstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	dirName  = "fishhawk"
	fileName = "credentials"

	// filePerm / dirPerm keep the secret file owner-only. A wider
	// mode would let a co-tenant read a live bearer token.
	filePerm = 0o600
	dirPerm  = 0o700
)

// ErrNotFound means no credential is stored for the requested
// backend URL. Callers distinguish it from a read/parse failure so a
// missing credential degrades to "no token" while a corrupt store
// surfaces loudly.
var ErrNotFound = errors.New("credstore: no credential for backend URL")

// Credential is the record stored per backend URL. Token is the live
// bearer secret; Subject/Scopes/Provider are display metadata captured
// at login so `token list` can show who the token belongs to without a
// backend round-trip.
//
// The refresh fields (#2393 / ADR-076) are what let a consumer perform
// an RFC 6749 §6 refresh on its own: RefreshToken is the rotating
// secret, ClientID the public OAuth client it was issued to, and
// TokenEndpoint the AS token endpoint to present it at. IssuedAt lets
// the proactive-refresh margin be derived from the credential's OWN
// lifetime (see RefreshSkew). Every one is json-omitempty so a store
// written before they existed (a device-flow fhk_ credential) still
// decodes unchanged, and a nil ExpiresAt still means non-expiring.
type Credential struct {
	Token     string     `json:"token"`
	Subject   string     `json:"subject,omitempty"`
	Scopes    []string   `json:"scopes,omitempty"`
	Provider  string     `json:"provider,omitempty"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`

	RefreshToken  string     `json:"refresh_token,omitempty"`
	ClientID      string     `json:"client_id,omitempty"`
	TokenEndpoint string     `json:"token_endpoint,omitempty"`
	IssuedAt      *time.Time `json:"issued_at,omitempty"`
}

// DefaultLifetime is the access-token lifetime assumed when a
// credential carries an ExpiresAt but no IssuedAt, so its lifetime
// cannot be measured. It MUST equal the authorization server's shipped
// default access-token TTL (backend/internal/server
// defaultOAuthAccessTokenTTL); the cross-module pin
// backend/internal/server/oauthttlskew_test.go fails if the two drift.
const DefaultLifetime = 15 * time.Minute

// minRefreshSkew floors the proactive-refresh margin so a very short
// TTL can never leave a margin thinner than one refresh round-trip.
const minRefreshSkew = 30 * time.Second

// RefreshSkew returns the proactive-refresh margin for a credential of
// the given lifetime: refresh once the credential is within this much
// of its expiry. It is DERIVED from the lifetime rather than fixed —
// lifetime*2/15, which is exactly 2 minutes at the shipped 15-minute
// default — floored at 30s (a margin thinner than a refresh round-trip
// would expire the token mid-refresh) and capped at lifetime/2 (the
// floor must never exceed the credential's own life, or a very short
// token would be "always refreshing"). A non-positive lifetime has no
// margin and returns 0.
func RefreshSkew(lifetime time.Duration) time.Duration {
	if lifetime <= 0 {
		return 0
	}
	skew := lifetime * 2 / 15
	if skew < minRefreshSkew {
		skew = minRefreshSkew
	}
	if half := lifetime / 2; skew > half {
		skew = half
	}
	return skew
}

// Refreshable reports whether the credential carries everything an
// RFC 6749 §6 refresh needs: a refresh token, the public client it was
// issued to, and the token endpoint to present it at. A device-flow
// fhk_ credential carries none of these and is never refreshable.
func (c Credential) Refreshable() bool {
	return c.RefreshToken != "" && c.ClientID != "" && c.TokenEndpoint != ""
}

// Lifetime returns the credential's access-token lifetime: ExpiresAt
// minus IssuedAt when both are known, DefaultLifetime when only the
// expiry is known, and 0 for a non-expiring credential.
func (c Credential) Lifetime() time.Duration {
	if c.ExpiresAt == nil {
		return 0
	}
	if c.IssuedAt == nil {
		return DefaultLifetime
	}
	return c.ExpiresAt.Sub(*c.IssuedAt)
}

// Expired reports whether the credential's access token is past its
// expiry at now. A nil ExpiresAt is non-expiring and never expired.
func (c Credential) Expired(now time.Time) bool {
	return c.ExpiresAt != nil && !now.Before(*c.ExpiresAt)
}

// NeedsRefresh reports whether a consumer should refresh c before
// using it at now: false for a non-expiring credential (nil
// ExpiresAt), false when there is nothing to refresh WITH (see
// Refreshable — an fhk_ credential must never be presented at a token
// endpoint it does not have), otherwise true once now is within
// RefreshSkew(Lifetime()) of ExpiresAt. An already-expired refreshable
// credential also needs refresh: the refresh token outlives the access
// token, so the right move is to refresh, not to fail.
func NeedsRefresh(c Credential, now time.Time) bool {
	if c.ExpiresAt == nil {
		return false
	}
	if !c.Refreshable() {
		return false
	}
	return !now.Before(c.ExpiresAt.Add(-RefreshSkew(c.Lifetime())))
}

// configDir returns the fishhawk config directory, honoring
// XDG_CONFIG_HOME and falling back to ~/.config.
func configDir() (string, error) {
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, dirName), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("credstore: resolve home dir: %w", err)
	}
	return filepath.Join(home, ".config", dirName), nil
}

// Path returns the absolute path of the credentials file. Exposed so
// `token login` / `token list` can tell the operator where the
// secret lives.
func Path() (string, error) {
	d, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, fileName), nil
}

// normalizeURL canonicalizes a backend URL for use as a store key so
// `http://localhost:8080` and `http://localhost:8080/` address the
// same credential.
func normalizeURL(u string) string {
	return strings.TrimRight(strings.TrimSpace(u), "/")
}

// List returns every stored credential keyed by (normalized) backend
// URL. A missing store file is not an error — it returns an empty
// map. A present-but-corrupt file IS an error.
func List() (map[string]Credential, error) {
	p, err := Path()
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]Credential{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("credstore: read %s: %w", p, err)
	}
	var store map[string]Credential
	if err := json.Unmarshal(b, &store); err != nil {
		return nil, fmt.Errorf("credstore: parse %s: %w", p, err)
	}
	if store == nil {
		store = map[string]Credential{}
	}
	return store, nil
}

// Load returns the credential stored for backendURL, or ErrNotFound
// if none is stored. A corrupt store surfaces the parse error.
func Load(backendURL string) (Credential, error) {
	all, err := List()
	if err != nil {
		return Credential{}, err
	}
	c, ok := all[normalizeURL(backendURL)]
	if !ok {
		return Credential{}, ErrNotFound
	}
	return c, nil
}

// Store persists cred for backendURL, merging into any existing
// store. The write is atomic (temp file + rename) so a crash mid-
// write never truncates an existing credential set, and the file is
// left mode 0600.
//
// The read-merge-write runs under the SAME exclusive `credentials.lock`
// RefreshStored uses. Without it a login write racing a refresh would
// read the store before the rotation landed and rename its stale merge
// over it — restoring a refresh token the authorization server has
// already consumed, so the next use trips reuse detection and revokes
// the lineage. The races are cross-BACKEND too (a login for one backend
// clobbering another's rotation), because the store is ONE JSON file.
//
// The wait is bounded at lockWait and fails loud rather than blocking
// forever behind a suspended peer. Callers already holding the lock
// call storeLocked instead — flock would block on a second descriptor.
func Store(backendURL string, cred Credential) error {
	unlock, err := lockStore(context.Background())
	if err != nil {
		return err
	}
	defer unlock()
	return storeLocked(backendURL, cred)
}

// storeMergeBarrier is a TEST SEAM, nil in production. storeLocked calls
// it (with the backend URL being written) between reading the existing
// store and writing the merged result, which is exactly the window the
// lock closes — it lets the credstore tests hold a writer inside that
// window deterministically instead of racing the clock.
var storeMergeBarrier func(backendURL string)

// storeLocked is Store's body with the caller responsible for holding
// the credential-store lock.
func storeLocked(backendURL string, cred Credential) error {
	d, err := configDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(d, dirPerm); err != nil {
		return fmt.Errorf("credstore: create %s: %w", d, err)
	}
	all, err := List()
	if err != nil {
		return err
	}
	if storeMergeBarrier != nil {
		storeMergeBarrier(normalizeURL(backendURL))
	}
	all[normalizeURL(backendURL)] = cred

	b, err := json.MarshalIndent(all, "", "  ")
	if err != nil {
		return fmt.Errorf("credstore: encode: %w", err)
	}

	tmp, err := os.CreateTemp(d, fileName+".tmp-*")
	if err != nil {
		return fmt.Errorf("credstore: create temp: %w", err)
	}
	tmpName := tmp.Name()
	// Best-effort cleanup if we bail before the rename lands.
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("credstore: write temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("credstore: close temp: %w", err)
	}
	if err := os.Chmod(tmpName, filePerm); err != nil {
		return fmt.Errorf("credstore: chmod temp: %w", err)
	}

	p := filepath.Join(d, fileName)
	if err := os.Rename(tmpName, p); err != nil {
		return fmt.Errorf("credstore: rename into place: %w", err)
	}
	return nil
}
