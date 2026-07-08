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
// bearer secret; the rest are display metadata captured at login so
// `token list` can show who the token belongs to without a backend
// round-trip.
type Credential struct {
	Token     string     `json:"token"`
	Subject   string     `json:"subject,omitempty"`
	Scopes    []string   `json:"scopes,omitempty"`
	Provider  string     `json:"provider,omitempty"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
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
func Store(backendURL string, cred Credential) error {
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
