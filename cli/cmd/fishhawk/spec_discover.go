package main

import (
	"crypto/sha1" //nolint:gosec // git's blob-object hash is SHA-1; matching it requires SHA-1 (not a security boundary).
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/kuhlman-labs/fishhawk/cli/internal/spec"
)

// specFileName is the canonical relative path of the workflow spec
// inside a repo. The dispatcher reads it at the same path on the
// github_actions side; the CLI mirrors that contract.
const specFileName = ".fishhawk/workflows.yaml"

// discoveredSpec holds the resolved local workflow spec — its
// absolute path, raw YAML bytes, and the git blob SHA computed
// over the bytes. Returned by discoverSpec; empty when no spec is
// found and no explicit --spec-file was supplied.
type discoveredSpec struct {
	Path     string
	Contents []byte
	BlobSHA  string
}

// discoverSpec resolves the workflow spec the CLI should send to
// the backend.
//
// Precedence:
//  1. explicit (the --spec-file flag) — if set, the file MUST exist
//     and parse. Failure here is the user's typo, not a fallback.
//  2. otherwise, walk up from startDir looking for `.fishhawk/workflows.yaml`,
//     stopping at (and including) the dir that contains `.git`. The
//     boundary keeps `fishhawk run start` from accidentally adopting
//     a parent repo's spec when a sub-checkout has none.
//
// Returns (nil, nil) when no explicit path was given and the walk
// fails to find a spec — callers treat that as "no local spec"
// rather than an error so the legacy --workflow-sha path keeps
// working without a checkout.
func discoverSpec(startDir, explicit string) (*discoveredSpec, error) {
	if explicit != "" {
		data, err := os.ReadFile(explicit)
		if err != nil {
			return nil, fmt.Errorf("--spec-file: %w", err)
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

// cliSpecValidate runs the workflow-v0 JSON Schema check on the
// bytes the CLI is about to ship. Exposed as a var so test files
// can stub it (rarely useful — the embedded schema is the same
// the backend compiles against — but cheap to keep).
var cliSpecValidate = func(data []byte) error {
	return spec.ValidateBytes(data)
}

// gitBlobSHA returns the SHA-1 of the git blob object that wraps
// the given file contents: `"blob <len>\0<contents>"`. Computed
// in-process so the CLI doesn't need to shell out to git.
//
// Matches what GitHub returns as the file's blob SHA, which is the
// dispatcher's source of truth for workflow_sha on the
// github_actions path. Keeping the local SHA stable with the
// remote one means a run minted by `fishhawk run start --local`
// referring to "this checkout's spec" lines up with a later
// github_actions run that picked up the same commit.
func gitBlobSHA(content []byte) string {
	h := sha1.New() //nolint:gosec
	_, _ = fmt.Fprintf(h, "blob %d\x00", len(content))
	_, _ = h.Write(content)
	return fmt.Sprintf("%x", h.Sum(nil))
}
