package reviewsandbox

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/procgroup"
)

// ErrRefUnavailable is wrapped by every ExportTree error that means "the ref
// does not name a commit this repository has, and fetch-on-miss could not
// supply it" (#4066): a non-SHA ref that does not resolve locally, a full
// object id that is absent locally and could not be fetched from origin (the
// fetch failed or timed out), or one that is still not a commit after the
// fetch. The server classifies it as the ref_unavailable degrade reason, so
// the review prompt says the tree for this round could not be provided rather
// than telling the operator to enable grounding. A git-absent host, a
// cancelled context, an archive failure or a bounds violation is NOT wrapped.
var ErrRefUnavailable = errors.New("reviewsandbox: ref unavailable")

// defaultFetchTimeout bounds the fetch-on-miss child (#4066).
const defaultFetchTimeout = 30 * time.Second

// fetchKillGrace is the procgroup.Harden WaitDelay for the fetch child: how
// long Wait may block on a stderr pipe still held by a group member that
// escaped the whole-group kill.
const fetchKillGrace = 2 * time.Second

// fetchStderrTail caps the stderr bytes a failed fetch names in its error.
const fetchStderrTail = 256

// Limits bounds an extraction so a pathological or hostile archive cannot fill
// the disk. Zero fields fall back to the defaults in DefaultLimits.
type Limits struct {
	// MaxFiles caps the number of regular files written.
	MaxFiles int
	// MaxBytes caps the total bytes written across all regular files.
	MaxBytes int64
	// FetchTimeout bounds the fetch-on-miss child (#4066): the whole process
	// group is killed when it fires and the export degrades with a named
	// timeout.
	FetchTimeout time.Duration
}

// DefaultLimits are the export bounds a review loop uses: 50000 files and
// 512 MiB, an order of magnitude over this repository's own tree while still
// refusing a runaway monorepo (the caller degrades to an ungrounded, diff-only
// review rather than exhausting the daemon's disk), and a 30s fetch-on-miss
// bound.
func DefaultLimits() Limits {
	return Limits{MaxFiles: 50000, MaxBytes: 512 << 20, FetchTimeout: defaultFetchTimeout}
}

func (l Limits) resolved() Limits {
	if l.MaxFiles <= 0 {
		l.MaxFiles = 50000
	}
	if l.MaxBytes <= 0 {
		l.MaxBytes = 512 << 20
	}
	if l.FetchTimeout <= 0 {
		l.FetchTimeout = defaultFetchTimeout
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
	// depth, and every entry under a .claude/, .codex/ or .agents/ directory.
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
// no other branches. Every git child runs with a scrubbed minimal environment
// (Env(os.Environ(), BaseAllow, nil)) so it inherits no repository credentials.
//
// Fetch-on-miss (#4066): when ref is a full object id (40 or 64 hex chars) that
// the repository does not have locally — a server-pushed consolidated commit a
// decomposed parent's review runs against before it was ever fetched — the
// resolve step fetches exactly that object from origin, bounded by
// limits.FetchTimeout, and resolves again. The fetch names no destination,
// writes no FETCH_HEAD and disables the refmap, so no local branch,
// remote-tracking ref or FETCH_HEAD moves; only objects land. A non-SHA ref is
// never fetched. See resolveCommit.
//
// On ANY error (unresolvable ref, a failed or timed-out fetch, git absent, not
// a work tree, a bounds violation, a traversal entry, a non-zero git exit)
// ExportTree removes its own partial directory before returning, so no caller
// can leak it, and returns a no-op cleanup. An unavailable ref wraps
// ErrRefUnavailable and names the cause. The caller degrades to an ungrounded,
// diff-only review.
func ExportTree(ctx context.Context, repoDir, ref string, limits Limits) (dir, commit string, stats Stats, cleanup func(), err error) {
	noop := func() {}
	limits = limits.resolved()

	sha, err := resolveCommit(ctx, repoDir, ref, limits.FetchTimeout)
	if err != nil {
		return "", "", Stats{}, noop, err
	}

	dest, err := os.MkdirTemp("", "fishhawk-review-tree-")
	if err != nil {
		return "", "", Stats{}, noop, fmt.Errorf("reviewsandbox: create export dir: %w", err)
	}
	cleanupFn := func() { _ = os.RemoveAll(dest) }

	st, err := archiveInto(ctx, repoDir, sha, dest, limits)
	if err != nil {
		cleanupFn()
		return "", "", Stats{}, noop, err
	}
	return dest, sha, st, cleanupFn, nil
}

// resolveCommit resolves ref to a full commit SHA via
// `git -C repoDir rev-parse --verify <ref>^{commit}` (revParse), fetching the
// object from origin once on a miss (#4066).
//
// The miss path is taken only when ALL of these hold, in this order:
//   - the context is still live. A cancelled or deadline-killed rev-parse
//     surfaces as an *exec.ExitError (signal: killed), so the ctx check runs
//     FIRST: that is a ctx error, never a ref miss — no fetch, and the error
//     does not wrap ErrRefUnavailable;
//   - rev-parse exited non-zero (*exec.ExitError). git being absent, or any
//     other start failure, returns as a plain error with no fetch;
//   - ref is a full object id (isFullObjectID). A branch name, `HEAD`, or any
//     token starting with '-' never reaches the fetch argv: fetching a named
//     ref would download its objects under a name the caller did not pin, and
//     a leading '-' would be parsed as a fetch option.
//
// Every miss — a non-SHA ref, a failed or timed-out fetch, or an object still
// not a commit after the fetch — wraps ErrRefUnavailable and names its cause.
func resolveCommit(ctx context.Context, repoDir, ref string, fetchTimeout time.Duration) (string, error) {
	if ref == "" {
		return "", fmt.Errorf("reviewsandbox: empty ref")
	}
	sha, err := revParse(ctx, repoDir, ref)
	if err == nil {
		return sha, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return "", fmt.Errorf("reviewsandbox: resolve ref %q: %w (%w)", ref, ctxErr, err)
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return "", err
	}
	if !isFullObjectID(ref) {
		return "", fmt.Errorf("%w: %v (not a full object id, so it is not fetched from origin)", ErrRefUnavailable, err)
	}
	if ferr := fetchFromOrigin(ctx, repoDir, ref, fetchTimeout); ferr != nil {
		if ctx.Err() != nil {
			return "", ferr
		}
		return "", fmt.Errorf("%w: commit %s is not present locally and could not be fetched from origin: %v", ErrRefUnavailable, ref, ferr)
	}
	sha, err = revParse(ctx, repoDir, ref)
	if err != nil {
		if ctx.Err() != nil {
			return "", err
		}
		return "", fmt.Errorf("%w: %s is still not a commit after fetching it from origin: %v", ErrRefUnavailable, ref, err)
	}
	return sha, nil
}

// revParse runs `git -C repoDir rev-parse --verify <ref>^{commit}`. The
// ^{commit} peel makes a tag or tree ref resolve to its commit (or fail), and
// --verify makes an ambiguous or absent ref a non-zero exit rather than a
// printed error line. The returned error wraps the exec error unchanged so
// resolveCommit can tell a non-zero exit from a start failure.
func revParse(ctx context.Context, repoDir, ref string) (string, error) {
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

// isFullObjectID reports whether ref is a full hex object id: exactly 40 (SHA-1)
// or 64 (SHA-256) characters, each 0-9 or a-f/A-F. It is the guard that keeps
// every non-SHA token — a branch name, `HEAD`, an abbreviated SHA, anything
// starting with '-' — out of the fetch argv.
func isFullObjectID(ref string) bool {
	if len(ref) != 40 && len(ref) != 64 {
		return false
	}
	for i := 0; i < len(ref); i++ {
		c := ref[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}

// fetchFromOrigin fetches one object id from the repository's origin remote:
//
//	git -C repoDir fetch --no-tags --no-write-fetch-head --no-auto-maintenance --refmap= origin <sha>
//
// The flags keep the fetch object-only: no tags follow, FETCH_HEAD is not
// written, the configured refmap is disabled (so even a configured
// remote.origin.fetch moves no remote-tracking ref), and no detached
// maintenance child is forked (the AGENTS.md #3503 class). The refspec names
// no destination, so no local ref moves.
//
// The child runs under the scrubbed BaseAllow environment plus
// GIT_TERMINAL_PROMPT=0, so a remote demanding credentials fails instead of
// prompting. It is bounded by timeout and procgroup-hardened: when the bound
// (or the parent context) fires, the WHOLE process group — git plus any
// upload-pack, ssh or credential-helper grandchild holding the stderr pipe —
// is killed, and WaitDelay bounds Wait.
//
// The returned error names the cause: the parent context's error when the
// PARENT was cancelled (C1: a ctx error, never a timeout or a miss), "timed out
// after <timeout>" when only the fetch bound fired, and otherwise the exit
// status plus a single-line stderr tail of at most fetchStderrTail bytes.
func fetchFromOrigin(ctx context.Context, repoDir, sha string, timeout time.Duration) error {
	fctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(fctx, "git", "-C", repoDir, "fetch",
		"--no-tags", "--no-write-fetch-head", "--no-auto-maintenance", "--refmap=",
		"origin", sha)
	cmd.Env = append(Env(os.Environ(), BaseAllow, nil), "GIT_TERMINAL_PROMPT=0")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	procgroup.Harden(cmd, fetchKillGrace)

	err := cmd.Run()
	if err == nil {
		return nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("reviewsandbox: fetch %s from origin: %w", sha, ctxErr)
	}
	if errors.Is(fctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("fetch from origin timed out after %s", timeout)
	}
	return fmt.Errorf("fetch from origin: %v: %s", err, stderrTail(stderr.Bytes()))
}

// stderrTail renders the last fetchStderrTail bytes of a child's stderr as one
// line (newlines and runs of whitespace collapsed) for an error message.
func stderrTail(b []byte) string {
	if len(b) > fetchStderrTail {
		b = b[len(b)-fetchStderrTail:]
	}
	tail := strings.Join(strings.Fields(string(b)), " ")
	if tail == "" {
		return "(no stderr)"
	}
	return tail
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
//   - any component named .agents — the cross-tool Agent Skills directory
//     (agentskills.io): codex auto-discovers skills from .agents/skills at
//     every directory level up to the repository root, so a tracked SKILL.md
//     there loads as an instruction exactly like AGENTS.md. Skipped wholesale
//     like .claude/.codex rather than narrowed to .agents/skills, so a future
//     discovery path under the same directory is closed by default.
//
// Every comparison is CASE-INSENSITIVE. The tree is extracted onto the host
// filesystem, and on a case-insensitive one (macOS APFS by default) a CLI
// opening `.agents/skills` or `AGENTS.md` resolves to a tracked `.Agents/` or
// `agents.md`, so an exact-case match would let a re-cased path survive
// extraction and still load as an instruction.
func isAgentInstructionPath(rel string) bool {
	parts := strings.Split(rel, "/")
	for i, p := range parts {
		for _, dir := range agentConfigDirs {
			if strings.EqualFold(p, dir) {
				return true
			}
		}
		if i == len(parts)-1 {
			for _, name := range agentInstructionFiles {
				if strings.EqualFold(p, name) {
					return true
				}
			}
		}
	}
	return false
}

// agentConfigDirs are the directory components skipped wholesale, and
// agentInstructionFiles the file basenames skipped at any depth, by
// isAgentInstructionPath (compared case-insensitively).
var (
	agentConfigDirs       = []string{".claude", ".codex", ".agents"}
	agentInstructionFiles = []string{"AGENTS.md", "AGENTS.override.md", "CLAUDE.md", "CLAUDE.local.md"}
)
