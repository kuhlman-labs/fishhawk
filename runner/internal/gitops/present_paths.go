package gitops

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// PresentPaths returns the order-preserving subset of paths that still exist
// at park time — in the commit at commitSHA OR in the working tree under
// repoDir (#5222). The build-required-drift park (#2548) records the
// `git status` drift list taken at staging time, and a path that vanished
// before the park (a transient duplicate a sync client wrote and removed, a
// scratch file a test deleted) is not a coupling any sibling slice can own.
// Recording it sends the operator after a junk path. A path is dropped only
// when it is absent from BOTH places:
//
//   - in the commit: `git cat-file -e <commitSHA>:<path>`. A tracked file the
//     agent deleted on disk as drift is still in the held scope-only commit,
//     so it is kept.
//   - on disk: os.Lstat. Any Lstat error other than not-exist keeps the path
//     (fail open).
//
// Both lookups are literal: the commit lookup takes `<tree-ish>:<path>`, not a
// pathspec, so a glob-named path matches only itself.
//
// A path that still carries a porcelain C-escape is KEPT unexamined
// (approval condition G2). `git status --porcelain` C-quotes an unusual path
// (a backslash escape such as \t, \" or \303\251 for non-ASCII bytes, inside
// double quotes); porcelainPath trims the quotes but does not decode the
// escapes, so neither lookup can resolve such a path and it would otherwise be
// dropped as absent. Without decoding, keeping it is the only safe answer.
//
// The commit is validated first (`git cat-file -e <commitSHA>^{commit}`):
// `git cat-file -e` exits non-zero for a missing path and for a bad object
// alike, so an unresolvable commit would otherwise read as every path absent.
// It returns an error instead, and the caller fails open (records the
// unfiltered drift). A cancelled context during the per-path lookups is also
// an error, for the same reason.
func PresentPaths(ctx context.Context, repoDir, commitSHA string, paths []string) ([]string, error) {
	p := &Pusher{}
	if err := p.run(ctx, repoDir, "cat-file", "-e", commitSHA+"^{commit}"); err != nil {
		return nil, fmt.Errorf("gitops: present paths: resolve commit %q: %w", commitSHA, err)
	}
	present := make([]string, 0, len(paths))
	for _, path := range paths {
		if carriesPorcelainEscape(path) {
			present = append(present, path)
			continue
		}
		onDisk := true
		if _, err := os.Lstat(filepath.Join(repoDir, filepath.FromSlash(path))); err != nil && errors.Is(err, fs.ErrNotExist) {
			onDisk = false
		}
		inCommit := true
		if !onDisk {
			if err := p.run(ctx, repoDir, "cat-file", "-e", commitSHA+":"+path); err != nil {
				if cerr := ctx.Err(); cerr != nil {
					return nil, fmt.Errorf("gitops: present paths: %w", cerr)
				}
				inCommit = false
			}
		}
		if !onDisk && !inCommit {
			continue
		}
		present = append(present, path)
	}
	return present, nil
}

// carriesPorcelainEscape reports whether a porcelain-derived path still holds
// a C-quote artifact: a backslash escape, or a surrounding double quote a
// caller did not trim. Either means the string is not the on-disk name.
func carriesPorcelainEscape(path string) bool {
	return strings.ContainsRune(path, '\\') || strings.HasPrefix(path, `"`) || strings.HasSuffix(path, `"`)
}
