package reviewsandbox

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
)

// Limits bounds an extraction so a pathological or hostile archive cannot fill
// the disk. Zero fields fall back to the defaults in DefaultLimits.
type Limits struct {
	// MaxFiles caps the number of regular files written.
	MaxFiles int
	// MaxBytes caps the total bytes written across all regular files.
	MaxBytes int64
}

// DefaultLimits are the export bounds a review loop uses: 50000 files and
// 512 MiB, an order of magnitude over this repository's own tree while still
// refusing a runaway monorepo (the caller degrades to an ungrounded, diff-only
// review rather than exhausting the daemon's disk).
func DefaultLimits() Limits {
	return Limits{MaxFiles: 50000, MaxBytes: 512 << 20}
}

func (l Limits) resolved() Limits {
	if l.MaxFiles <= 0 {
		l.MaxFiles = 50000
	}
	if l.MaxBytes <= 0 {
		l.MaxBytes = 512 << 20
	}
	return l
}

// Stats reports what an extraction wrote and skipped. The skip counters make an
// incomplete tree observable: the review prompt discloses them so a reviewer
// never treats a tree-wide search over a tree missing symlinks or
// agent-instruction files as exhaustive (C3).
type Stats struct {
	// Files is the number of regular files written.
	Files int
	// Bytes is the total bytes written across all regular files.
	Bytes int64
	// Symlinks is the number of tar symlink/hard-link entries SKIPPED (never
	// materialized on disk). Counted like the instruction skip so the caller can
	// disclose that the tree omits them.
	Symlinks int
	// Instructions is the number of agent-instruction entries SKIPPED (the C1
	// guard): AGENTS.md / AGENTS.override.md / CLAUDE.md / CLAUDE.local.md at any
	// depth, and every entry under a .claude/ or .codex/ directory.
	Instructions int
}

// AnySkipped reports whether the extraction omitted any entry, so the tree is
// not an exhaustive view of the commit's tracked files.
func (s Stats) AnySkipped() bool { return s.Symlinks > 0 || s.Instructions > 0 }

// ExportTree resolves ref to a commit in repoDir, streams
// `git -C repoDir archive --format=tar <sha>` through the stdlib tar reader into
// a throwaway directory, and returns that directory, the resolved commit SHA
// (C4: resolved ONCE here and handed back, so the caller names in the prompt the
// exact commit that was archived — never a second HEAD resolution that could
// name a different commit), the extraction Stats, and a cleanup closure the
// caller MUST call to remove the directory.
//
// Only tracked files at that commit are written — no .git, no untracked files,
// no other branches. Both git children run with a scrubbed minimal environment
// (Env(os.Environ(), BaseAllow, nil)) so they inherit no repository credentials.
//
// On ANY error (unresolvable ref, git absent, not a work tree, a bounds
// violation, a traversal entry, a non-zero git exit) ExportTree removes its own
// partial directory before returning, so no caller can leak it, and returns a
// no-op cleanup. The caller degrades to an ungrounded, diff-only review.
func ExportTree(ctx context.Context, repoDir, ref string, limits Limits) (dir, commit string, stats Stats, cleanup func(), err error) {
	noop := func() {}

	sha, err := resolveCommit(ctx, repoDir, ref)
	if err != nil {
		return "", "", Stats{}, noop, err
	}

	dest, err := os.MkdirTemp("", "fishhawk-review-tree-")
	if err != nil {
		return "", "", Stats{}, noop, fmt.Errorf("reviewsandbox: create export dir: %w", err)
	}
	cleanupFn := func() { _ = os.RemoveAll(dest) }

	st, err := archiveInto(ctx, repoDir, sha, dest, limits.resolved())
	if err != nil {
		cleanupFn()
		return "", "", Stats{}, noop, err
	}
	return dest, sha, st, cleanupFn, nil
}

// resolveCommit resolves ref to a full commit SHA via
// `git -C repoDir rev-parse --verify <ref>^{commit}`. The ^{commit} peel makes a
// tag or tree ref resolve to its commit (or fail), and --verify makes an
// ambiguous or absent ref a non-zero exit rather than a printed error line.
func resolveCommit(ctx context.Context, repoDir, ref string) (string, error) {
	if ref == "" {
		return "", fmt.Errorf("reviewsandbox: empty ref")
	}
	cmd := exec.CommandContext(ctx, "git", "-C", repoDir, "rev-parse", "--verify", ref+"^{commit}")
	cmd.Env = Env(os.Environ(), BaseAllow, nil)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("reviewsandbox: resolve ref %q: %w", ref, err)
	}
	sha := strings.TrimSpace(string(out))
	if sha == "" {
		return "", fmt.Errorf("reviewsandbox: resolve ref %q: empty output", ref)
	}
	return sha, nil
}

// archiveInto streams `git archive` for sha into dest via extractTar. The tar
// stream MUST be read to EOF before cmd.Wait (os/exec.Cmd.Wait closes the
// parent-side pipe fds while a reader may still be using them), so on the happy
// path the remaining stream is drained after extractTar returns. A derived
// cancelable context kills the git child if extractTar aborts early (a bounds
// violation) so the child cannot block forever on a full pipe.
func archiveInto(ctx context.Context, repoDir, sha, dest string, limits Limits) (Stats, error) {
	execCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	cmd := exec.CommandContext(execCtx, "git", "-C", repoDir, "archive", "--format=tar", sha)
	cmd.Env = Env(os.Environ(), BaseAllow, nil)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Stats{}, fmt.Errorf("reviewsandbox: git archive stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return Stats{}, fmt.Errorf("reviewsandbox: start git archive: %w", err)
	}

	stats, xerr := extractTar(stdout, dest, limits)
	if xerr != nil {
		// Abort the git child (it may be blocked writing to the now-unread pipe),
		// reap it, and surface the extraction fault.
		cancel()
		_ = cmd.Wait()
		return Stats{}, xerr
	}

	// Drain any trailing bytes so Wait doesn't close the pipe under a live read.
	_, _ = io.Copy(io.Discard, stdout)
	if werr := cmd.Wait(); werr != nil {
		return Stats{}, fmt.Errorf("reviewsandbox: git archive: %w", werr)
	}
	return stats, nil
}

// extractTar reads a tar stream and materializes it under dest, enforcing each
// bound and skip as its own named guard. It uses the stdlib archive/tar reader —
// not a shelled `tar -x` — so path handling is ours: a traversal or absolute
// entry is REFUSED (error, nothing written), symlink/hard-link entries are
// SKIPPED and counted, agent-instruction entries are SKIPPED and counted (C1),
// and the running file-count / byte total is refused once it exceeds the limit.
// Regular files are written 0600 and directories 0700.
func extractTar(r io.Reader, dest string, limits Limits) (Stats, error) {
	var stats Stats
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return Stats{}, fmt.Errorf("reviewsandbox: read tar: %w", err)
		}

		// Path-traversal guard: reject an absolute path or one that escapes dest
		// via "..". Refuse — never write it — so a hostile archive cannot plant a
		// file outside the export root.
		rel, err := safeRel(hdr.Name)
		if err != nil {
			return Stats{}, err
		}
		if rel == "" {
			// The archive root entry ("./") — nothing to create.
			continue
		}

		// Symlink / hard-link skip: never materialize a link. A symlink could
		// point outside the tree (read amplification) and a link makes the tree
		// misrepresent the commit; both are skipped and counted so the prompt can
		// disclose the tree is not exhaustive (C3).
		if hdr.Typeflag == tar.TypeSymlink || hdr.Typeflag == tar.TypeLink {
			stats.Symlinks++
			continue
		}

		// Agent-instruction skip (C1): never load repository-resident agent
		// instructions as files the reviewer's CLI would auto-discover. The
		// reviewer still sees any change to these paths in the DIFF as data to
		// judge; the tree simply never carries them as instructions.
		if isAgentInstructionPath(rel) {
			stats.Instructions++
			continue
		}

		target := filepath.Join(dest, rel)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o700); err != nil {
				return Stats{}, fmt.Errorf("reviewsandbox: mkdir %q: %w", rel, err)
			}
		case tar.TypeReg:
			if stats.Files+1 > limits.MaxFiles {
				return Stats{}, fmt.Errorf("reviewsandbox: file count exceeds limit of %d", limits.MaxFiles)
			}
			if stats.Bytes+hdr.Size > limits.MaxBytes {
				return Stats{}, fmt.Errorf("reviewsandbox: byte total exceeds limit of %d", limits.MaxBytes)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return Stats{}, fmt.Errorf("reviewsandbox: mkdir parent of %q: %w", rel, err)
			}
			n, err := writeRegular(target, tr)
			if err != nil {
				return Stats{}, err
			}
			stats.Files++
			stats.Bytes += n
		default:
			// Other types (fifo, char/block device, xheader) are not part of a
			// git archive of tracked blobs — skip without counting.
			continue
		}
	}
	return stats, nil
}

// writeRegular writes the current tar entry to target 0600, returning the number
// of bytes written. It copies the whole entry (tar.Reader bounds the copy to the
// entry size) so the byte accounting matches what landed on disk.
func writeRegular(target string, tr io.Reader) (int64, error) {
	f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return 0, fmt.Errorf("reviewsandbox: create %q: %w", target, err)
	}
	n, err := io.Copy(f, tr)
	if cerr := f.Close(); cerr != nil && err == nil {
		err = cerr
	}
	if err != nil {
		return n, fmt.Errorf("reviewsandbox: write %q: %w", target, err)
	}
	return n, nil
}

// safeRel validates a tar entry name and returns the cleaned repo-relative path.
// It returns an error for an absolute path or one that escapes the root via
// "..", and "" for the archive root itself.
func safeRel(name string) (string, error) {
	if strings.HasPrefix(name, "/") {
		return "", fmt.Errorf("reviewsandbox: refusing absolute path %q in archive", name)
	}
	clean := path.Clean(name)
	if clean == "." {
		return "", nil
	}
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("reviewsandbox: refusing path %q escaping export root", name)
	}
	return clean, nil
}

// isAgentInstructionPath reports whether a cleaned, forward-slash repo-relative
// path is an agent-instruction file or lives under an agent-config directory,
// enumerated against both CLIs' documented discovery (see README.md):
//
//   - AGENTS.md, AGENTS.override.md — codex auto-discovers BOTH at every
//     directory level; AGENTS.override.md is codex's local-only override,
//     checked BEFORE AGENTS.md at each level per codex's documented
//     instruction-file discovery, so it loads as an instruction exactly like
//     AGENTS.md and must be skipped too (its omission was the C1 bypass in
//     #2486's fix-up: a tracked AGENTS.override.md at any depth would otherwise
//     survive extraction and be auto-discovered from the grounded tree).
//   - CLAUDE.md, CLAUDE.local.md — claude-code auto-discovers these at every
//     directory level.
//   - any component named .claude or .codex — the two CLIs' config/state
//     directories (settings, commands, agents, skills, config.toml), skipped
//     wholesale so nothing under them is loaded as instructions.
func isAgentInstructionPath(rel string) bool {
	parts := strings.Split(rel, "/")
	for i, p := range parts {
		if p == ".claude" || p == ".codex" {
			return true
		}
		if i == len(parts)-1 {
			switch p {
			case "AGENTS.md", "AGENTS.override.md", "CLAUDE.md", "CLAUDE.local.md":
				return true
			}
		}
	}
	return false
}
