package main

import (
	"crypto/sha1" //nolint:gosec // git's blob-object hash is SHA-1; matching it requires SHA-1 (not a security boundary).
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// specFileName is the canonical relative path of the workflow spec
// inside a repo. Mirrors the CLI's contract (cli/cmd/fishhawk/spec_discover.go).
const specFileName = ".fishhawk/workflows.yaml"

// discoveredSpec holds the resolved local workflow spec — its
// absolute path, raw YAML bytes, and the git blob SHA computed
// over the bytes. Returned by discoverSpec; nil when no spec is
// found and no explicit path was supplied.
//
// This is the MCP server's local copy of the CLI's type. Kept here
// rather than shared because the dependency direction is
// `cli → backend`, not the other way around — the CLI imports the
// CLI-side; the MCP binary (which lives under backend) re-implements
// against the backend's spec parser. The logic is small enough that
// duplication is cheaper than a shared package.
type discoveredSpec struct {
	Path     string
	Contents []byte
	BlobSHA  string
}

// discoverSpec resolves the workflow spec the MCP server should
// send to the backend on a fishhawk_start_run call.
//
// Precedence:
//  1. explicit (a non-empty `explicit` path) — if set, the file
//     MUST exist and parse. Failure here is the operator's typo,
//     not a fallback.
//  2. otherwise, walk up from startDir looking for
//     `.fishhawk/workflows.yaml`, stopping at (and including) the
//     dir that contains `.git`. The boundary keeps an MCP call
//     from accidentally adopting a parent repo's spec when a
//     sub-checkout has none.
//
// Returns (nil, nil) when no explicit path was given and the walk
// fails to find a spec — callers treat that as "no local spec"
// rather than an error so the legacy workflow_sha-only path keeps
// working without a checkout.
func discoverSpec(startDir, explicit string) (*discoveredSpec, error) {
	if explicit != "" {
		data, err := os.ReadFile(explicit)
		if err != nil {
			return nil, fmt.Errorf("spec_file: %w", err)
		}
		abs, _ := filepath.Abs(explicit)
		return &discoveredSpec{
			Path:     abs,
			Contents: data,
			BlobSHA:  gitBlobSHA(data),
		}, nil
	}

	dir, err := filepath.Abs(startDir)
	if err != nil {
		return nil, fmt.Errorf("resolve working dir: %w", err)
	}
	for {
		candidate := filepath.Join(dir, specFileName)
		data, err := os.ReadFile(candidate)
		switch {
		case err == nil:
			return &discoveredSpec{
				Path:     candidate,
				Contents: data,
				BlobSHA:  gitBlobSHA(data),
			}, nil
		case errors.Is(err, fs.ErrNotExist):
			// fall through and check the .git boundary / walk up.
		default:
			return nil, fmt.Errorf("read %s: %w", candidate, err)
		}

		// Stop after checking the dir that contains .git (repo
		// root). The spec file at the root would have been picked
		// up above, so reaching this point means the repo doesn't
		// configure Fishhawk locally.
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return nil, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			// Filesystem root; nothing more to walk.
			return nil, nil
		}
		dir = parent
	}
}

// specValidate runs the workflow-v0 JSON Schema check + semantic
// validation on the bytes the MCP server is about to ship. Exposed
// as a var so tests can stub it.
var specValidate = func(data []byte) error {
	_, err := spec.ParseBytes(data)
	return err
}

// annotateStaleSpecError (proposal 3 of #1422) self-diagnoses the
// silent-broken-loop cutover: a `.fishhawk/workflows.yaml` schema-major
// bump breaks every fishhawk_start_run because the live stdio
// fishhawk-mcp validates the spec locally on a STALE pre-bump binary
// that doesn't recognize the new `version`. When the local pre-parse
// fails with a *spec.SchemaError whose Path is "/version" — the shape
// both the pre-ADR-046 enum violation and the ADR-046 unsupported-major
// routing produce (see backend/internal/spec/parse.go) — wrap it with a
// staleness hint pointing at a /mcp reconnect, so even a missed
// reconnect banner is self-diagnosing. %w preserves the original error
// for errors.As/errors.Is. Any other error (a non-version SchemaError,
// a YAMLError, or a plain error) passes through unchanged: an
// unsupported version is the only failure a stale binary uniquely
// causes, so a fuzzy match would risk false hints.
func annotateStaleSpecError(err error) error {
	if err == nil {
		return nil
	}
	var se *spec.SchemaError
	if errors.As(err, &se) && se.Path == "/version" {
		return fmt.Errorf(
			"%w (this fishhawk-mcp binary may be stale — its embedded spec schema predates this version; run /mcp to reconnect and pick up the rebuilt binary)",
			err)
	}
	return err
}

// gitBlobSHA returns the SHA-1 of the git blob object that wraps
// the given file contents: `"blob <len>\0<contents>"`. Matches
// what GitHub returns as the file's blob SHA, which is the
// dispatcher's source of truth for workflow_sha on the
// github_actions path. Mirrors the CLI's implementation byte-for-
// byte.
func gitBlobSHA(content []byte) string {
	h := sha1.New() //nolint:gosec
	_, _ = fmt.Fprintf(h, "blob %d\x00", len(content))
	_, _ = h.Write(content)
	return fmt.Sprintf("%x", h.Sum(nil))
}
