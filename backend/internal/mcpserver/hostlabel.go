package mcpserver

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// hostLabelEnv is the explicit host-label override (#4212). It wins over every
// other rung, so an operator can pin the label a machine's MCP processes send
// as the host-dispatch marker's {"host"} body.
const hostLabelEnv = "FISHHAWK_HOST_LABEL"

// Host-label sources, in resolution precedence order.
const (
	hostLabelSourceOverride  = "override"
	hostLabelSourcePersisted = "persisted"
	hostLabelSourceHostname  = "hostname"
	hostLabelSourceNone      = "none"
)

// defaultGroupPrefix and unknownHostLabel mirror backend/internal/concurrency's
// DefaultGroupPrefix and UnknownHost. They are copies so the production MCP
// package gains no import edge onto the store package; a test-only parity
// assertion (TestDefaultGroupKeyParity) pins them to the originals.
const (
	defaultGroupPrefix = "local-implement:"
	unknownHostLabel   = "unknown"
)

// hostLabelResolution is the resolved host label plus the facts fishhawk_doctor
// reports about it: which rung answered, the host-id path when the persisted
// rung was consulted, and a human note.
type hostLabelResolution struct {
	Label  string
	Source string
	Path   string
	Note   string
}

// hostLabelDeps are the resolution seams: os.Getenv, os.Hostname, the host-id
// path, and the random-id generator in production.
type hostLabelDeps struct {
	getenv   func(string) string
	hostname func() (string, error)
	idPath   func() (string, error)
	randID   func() (string, error)
}

// resolveHostLabel resolves the host label the host-dispatch marker sends
// (#3964 / ADR-087, made stable by #4212). The server keys the default local
// concurrency group `local-implement:<label>` on it, so a label that changes on
// one machine splits that machine's group in two and doubles its effective
// limit. The ladder:
//
//  1. FISHHAWK_HOST_LABEL, sanitised. It consults no other rung.
//  2. The persisted per-machine host id (readOrCreateHostID), created once and
//     never overwritten, so it survives a hostname change.
//  3. os.Hostname, sanitised: the volatile last resort (macOS without a set
//     HostName follows the network).
//  4. Nothing: an empty label, which sends no body, so the server files the
//     stage under the `unknown` host.
func resolveHostLabel(d hostLabelDeps) hostLabelResolution {
	if raw := strings.TrimSpace(d.getenv(hostLabelEnv)); raw != "" {
		res := hostLabelResolution{
			Label:  sanitizeHostLabel(raw),
			Source: hostLabelSourceOverride,
			Note:   "Pinned by " + hostLabelEnv + ".",
		}
		if res.Label != raw {
			res.Note = fmt.Sprintf("Pinned by %s; the value %q was sanitised to %q (the accepted class is [A-Za-z0-9._-], at most %d bytes).", hostLabelEnv, raw, res.Label, hostLabelMax)
		}
		return res
	}

	hostLabel := ""
	if name, err := d.hostname(); err == nil {
		hostLabel = sanitizeHostLabel(name)
	}
	// The first creation seeds the host id from today's hostname, so rollout
	// keeps the group key the live queue already uses; a random id stands in
	// only when there is no hostname.
	seed := func() string {
		if hostLabel != "" {
			return hostLabel
		}
		id, err := d.randID()
		if err != nil {
			return ""
		}
		return id
	}

	var cause string
	path, err := d.idPath()
	if err != nil {
		cause = fmt.Sprintf("the host-id path could not be resolved (%v)", err)
	} else if id, err := readOrCreateHostID(path, seed); err != nil {
		cause = fmt.Sprintf("the host-id file %s could not be used (%v)", path, err)
	} else {
		return hostLabelResolution{
			Label:  id,
			Source: hostLabelSourcePersisted,
			Path:   path,
			Note:   "Read from this machine's persisted host-id file, so the label survives a hostname change.",
		}
	}

	if hostLabel != "" {
		return hostLabelResolution{
			Label:  hostLabel,
			Source: hostLabelSourceHostname,
			Path:   path,
			Note:   "Fell back to the hostname because " + cause + ". The hostname can change with the network, which splits this machine's local-implement group; pin the label with " + hostLabelEnv + ".",
		}
	}
	return hostLabelResolution{
		Source: hostLabelSourceNone,
		Path:   path,
		Note:   "No label resolved: " + cause + ", and the hostname is unavailable. The host-dispatch marker sends no host, so the server files the stage under the `" + unknownHostLabel + "` host.",
	}
}

// readOrCreateHostID returns the host id stored at path, creating the file
// from seed() when it does not exist yet. An existing file is NEVER overwritten:
// an empty or malformed one is an error naming the path, and the caller falls
// back to the hostname.
func readOrCreateHostID(path string, seed func() string) (string, error) {
	id, err := readHostID(path)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	s := seed()
	if s == "" {
		return "", fmt.Errorf("no seed for a new host-id file at %s: the hostname and the random id are both unavailable", path)
	}
	return createHostIDExclusive(path, s)
}

// readHostID reads and validates the host-id file. The content must already be
// a label in the server's accepted class: the file is written that way, so any
// other content is damage, reported rather than repaired.
func readHostID(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	raw := strings.TrimSpace(string(b))
	if raw == "" || sanitizeHostLabel(raw) != raw {
		return "", fmt.Errorf("host-id file %s holds no valid label (want 1-%d bytes of [A-Za-z0-9._-]); delete it to regenerate", path, hostLabelMax)
	}
	return raw, nil
}

// createHostIDExclusive creates the host-id file holding seed, exactly once
// across concurrent first-creators. The seed is written to a temp file in the
// same directory and hard-linked into place: link(2) refuses an existing
// target (EEXIST), so the file is never overwritten and no reader can observe
// a partial write. Whoever wins, the result is re-read from path, so every
// caller returns the value that is actually on disk. The temp file is always
// removed.
func createHostIDExclusive(path, seed string) (string, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create the host-id directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".host-id-*")
	if err != nil {
		return "", fmt.Errorf("create a host-id temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.WriteString(seed + "\n"); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("write the host-id temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("close the host-id temp file: %w", err)
	}
	if err := os.Link(tmpName, path); err != nil && !errors.Is(err, fs.ErrExist) {
		return "", fmt.Errorf("link the host-id file into place: %w", err)
	}
	return readHostID(path)
}

// hostIDPath is the per-machine host-id path: an ABSOLUTE $XDG_STATE_HOME
// gives <it>/fishhawk/host-id (a relative one is invalid per the XDG base-dir
// spec and ignored); otherwise darwin uses os.UserConfigDir
// (~/Library/Application Support) and every other OS the XDG state default
// ~/.local/state.
func hostIDPath(getenv func(string) string, goos string, userConfigDir, userHomeDir func() (string, error)) (string, error) {
	if xdg := getenv("XDG_STATE_HOME"); xdg != "" && filepath.IsAbs(xdg) {
		return filepath.Join(xdg, "fishhawk", "host-id"), nil
	}
	if goos == "darwin" {
		dir, err := userConfigDir()
		if err != nil {
			return "", fmt.Errorf("resolve the user config directory: %w", err)
		}
		return filepath.Join(dir, "fishhawk", "host-id"), nil
	}
	home, err := userHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve the home directory: %w", err)
	}
	return filepath.Join(home, ".local", "state", "fishhawk", "host-id"), nil
}

// randomHostID is the seed for a machine with no hostname: host- + 16 hex chars.
func randomHostID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "host-" + hex.EncodeToString(b[:]), nil
}

// productionHostLabelDeps wires the real process environment.
func productionHostLabelDeps() hostLabelDeps {
	return hostLabelDeps{
		getenv:   os.Getenv,
		hostname: os.Hostname,
		idPath: func() (string, error) {
			return hostIDPath(os.Getenv, runtime.GOOS, os.UserConfigDir, os.UserHomeDir)
		},
		randID: randomHostID,
	}
}

// processHostLabel is this process's host label, resolved lazily and once, on
// first use (the first host-dispatch marker or fishhawk_doctor call). Lazy on
// purpose: fishhawkd's /mcp route builds a tool registry per request and many
// tests call NewServer, so an eager resolution would touch the real HOME at
// daemon boot and during the test run. Config.internal wires it.
var processHostLabel = sync.OnceValue(func() hostLabelResolution {
	return resolveHostLabel(productionHostLabelDeps())
})

// defaultGroupKeyFor is the default local concurrency group the server files a
// host-dispatched stage under for label: concurrency.DefaultGroupKey, mirrored.
func defaultGroupKeyFor(label string) string {
	if label == "" {
		label = unknownHostLabel
	}
	return defaultGroupPrefix + label
}
