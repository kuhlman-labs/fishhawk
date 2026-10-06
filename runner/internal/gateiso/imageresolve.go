package gateiso

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// This file resolves a declared gate_container (E51.3 / #2136) to an image on
// the host: the endpoint-bound runtime argv for inspect / pull / build, the
// parse of `image inspect` output (docker and podman), and the in-repo BUILD
// SOURCE. A build's context is the COMMITTED tree at the gate's head SHA,
// read from git objects — never the working tree — so uncommitted or
// untracked files cannot enter a gate image, every gate kind (in-place,
// throwaway clone, container) builds the same bytes, and the content digest
// is git's own: the context path's tree id plus the Dockerfile's blob id (and
// mode) plus the Dockerfile-specific ignore file's blob id. A tree id covers
// every byte, path and file mode beneath it (a mode-only change moves it) and
// the context's own .dockerignore, so the digest moves whenever what the
// builder could see moves.

// Build image tag and labels. The tag is content-addressed by the build
// digest, so an identical committed source is a cache hit on any gate.
const (
	GateBuildRepository   = "fishhawk-gate-build"
	GateBuildLabel        = "fishhawk.gate-build=1"
	GateBuildDigestLabel  = "fishhawk.gate-build.context-digest"
	imageInspectFormat    = "{{.Id}}{{range .RepoDigests}} {{.}}{{end}}"
	buildDigestDomain     = "fishhawk-gate-build/v1"
	dockerignoreSuffix    = ".dockerignore"
	materializedContext   = "context"
	materializedDockerfle = "Dockerfile"
)

// InspectArgv is `<runtime> <endpoint> image inspect --format <id+digests> <ref>`.
func (r Runtime) InspectArgv(ref string) ([]string, error) {
	return r.runtimeArgv(ref, "image", "inspect", "--format", imageInspectFormat, ref)
}

// PullArgv is `<runtime> <endpoint> pull <ref>`.
func (r Runtime) PullArgv(ref string) ([]string, error) {
	return r.runtimeArgv(ref, "pull", ref)
}

// BuildSpec is one in-repo gate image build over a materialized source.
type BuildSpec struct {
	// Dockerfile and Context are ABSOLUTE host paths (BuildLayout).
	Dockerfile string
	Context    string
	// Digest is BuildSource.Digest ("sha256:<64 hex>"): it keys the tag.
	Digest string
}

// BuildArgv is
//
//	<runtime> <endpoint> build --network=none --file <dockerfile>
//	  --tag fishhawk-gate-build:<hex> --label fishhawk.gate-build=1
//	  --label fishhawk.gate-build.context-digest=sha256:<hex> <context>
//
// --network=none confines RUN steps; base pulls go through the daemon (the
// static scan and the allowlist govern them).
func (r Runtime) BuildArgv(b BuildSpec) ([]string, error) {
	_, err := digestHex(b.Digest)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrContainerSpec, err)
	}
	if !filepath.IsAbs(b.Dockerfile) || !filepath.IsAbs(b.Context) {
		return nil, fmt.Errorf("%w: build dockerfile %q and context %q must be absolute", ErrContainerSpec, b.Dockerfile, b.Context)
	}
	return r.runtimeArgv("build", "build", "--network=none",
		"--file", b.Dockerfile,
		"--tag", BuildTag(b.Digest),
		"--label", GateBuildLabel,
		"--label", GateBuildDigestLabel+"="+b.Digest,
		b.Context)
}

// BuildTag is the content-addressed tag for a build digest.
func BuildTag(digest string) string {
	return GateBuildRepository + ":" + strings.TrimPrefix(digest, "sha256:")
}

func (r Runtime) runtimeArgv(subject string, args ...string) ([]string, error) {
	if subject == "" {
		return nil, fmt.Errorf("%w: empty image reference", ErrContainerSpec)
	}
	endpoint, err := r.EndpointArgs()
	if err != nil {
		return nil, err
	}
	argv := append([]string{r.Kind.Binary()}, endpoint...)
	return append(argv, args...), nil
}

var (
	sha256HexRE = regexp.MustCompile(`^[a-f0-9]{64}$`)
	gitOIDRE    = regexp.MustCompile(`^[a-f0-9]{40}(?:[a-f0-9]{24})?$`)
)

func digestHex(d string) (string, error) {
	h, ok := strings.CutPrefix(d, "sha256:")
	if !ok || !sha256HexRE.MatchString(h) {
		return "", fmt.Errorf("invalid build digest %q (want sha256:<64 lowercase hex>)", d)
	}
	return h, nil
}

// ImageInspect is a parsed `image inspect` line.
type ImageInspect struct {
	// ID is the local image id, normalized to sha256:<hex> (podman prints
	// bare hex). Opaque: on a containerd image store it is an index digest.
	ID string
	// RepoDigests are the registry name@digest entries as the runtime printed
	// them.
	RepoDigests []string
}

// ParseInspect parses InspectArgv's output: the id, then zero or more
// space-separated RepoDigests.
func ParseInspect(out string) (ImageInspect, error) {
	out = strings.TrimSpace(out)
	if out == "" || strings.Contains(out, "\n") {
		return ImageInspect{}, fmt.Errorf("image inspect returned %q, want one line \"<id> [<name@digest>…]\"", out)
	}
	f := strings.Fields(out)
	id := strings.ToLower(f[0])
	if sha256HexRE.MatchString(id) {
		id = "sha256:" + id
	}
	if _, err := digestHex(id); err != nil {
		return ImageInspect{}, fmt.Errorf("image inspect id %q is not a sha256 image id", f[0])
	}
	for _, d := range f[1:] {
		if !strings.Contains(d, "@sha256:") {
			return ImageInspect{}, fmt.Errorf("image inspect repo digest %q is not name@sha256:<hex>", d)
		}
	}
	return ImageInspect{ID: id, RepoDigests: f[1:]}, nil
}

// RepoDigestFor returns ref's normalized name@digest from the inspected
// RepoDigests, matching by normalized repository name (docker prints the
// familiar "alpine@sha256:…"), or false when none matches.
func RepoDigestFor(ref ImageRef, digests []string) (string, bool) {
	for _, d := range digests {
		r, err := ParseImageRef(d)
		if err != nil || !r.Pinned() {
			continue
		}
		if r.Name() == ref.Name() {
			return ref.Name() + "@" + r.Digest, true
		}
	}
	return "", false
}

// PullFailedReason is the named category-C reason for a declared image the
// runtime could not pull. A gate_container `image:` must be pullable from a
// registry; a local-only image is declared through dockerfile/context or the
// operator's FISHHAWK_GATE_IMAGE instead.
func PullFailedReason(ref ImageRef, err error) string {
	return fmt.Sprintf("gate_container image %s could not be pulled from its registry (%v): a declared `image:` must be pullable; for a local-only image declare `dockerfile` + `context` to build it in-repo, or have the operator set FISHHAWK_GATE_IMAGE", ref, err)
}

// ErrBuildSourceRefused is wrapped by ResolveBuildSource and
// MaterializeBuildContext for a declared build source the runner refuses
// (gateRefused): a path absent at the head commit, the wrong object type, a
// symlink or submodule, an oversized Dockerfile, an unsafe tree entry.
// Any other error is operational (gateUnavailable).
var ErrBuildSourceRefused = errors.New("gate_container build source refused")

// ErrContextTooLarge is wrapped when the committed build context exceeds
// ContextLimits (gateUnavailable: the operator narrows the context).
var ErrContextTooLarge = errors.New("gate_container build context too large")

func refuseSource(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrBuildSourceRefused, fmt.Sprintf(format, args...))
}

// GitFunc runs one git command (args after the binary) with stdin (may be
// nil), streaming stdout to stdout.
type GitFunc func(ctx context.Context, stdin io.Reader, stdout io.Writer, args ...string) error

// ExecGit returns a GitFunc running `git -C repoDir --literal-pathspecs …`
// (git from PATH) with the operator's global/system config pinned to
// /dev/null (#912) and no terminal prompt. The program is a literal so the
// runner's agent-spawn sweep classifies this site as non-agent (#3945).
func ExecGit(repoDir string) GitFunc {
	return func(ctx context.Context, stdin io.Reader, stdout io.Writer, args ...string) error {
		cmd := exec.CommandContext(ctx, "git", append([]string{"-C", repoDir, "--literal-pathspecs"}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_TERMINAL_PROMPT=0")
		cmd.Stdin = stdin
		cmd.Stdout = stdout
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
		}
		return nil
	}
}

func gitOutput(ctx context.Context, git GitFunc, args ...string) (string, error) {
	var out bytes.Buffer
	err := git(ctx, nil, &out, args...)
	return out.String(), err
}

// BuildSource is a declared build pinned to git objects at one commit.
type BuildSource struct {
	// Commit is the full head commit id the source was read at.
	Commit string
	// Dockerfile and Context are the declared repo-relative paths (cleaned).
	Dockerfile string
	Context    string
	// DockerfileBlob / DockerfileMode identify the Dockerfile at Commit.
	DockerfileBlob string
	DockerfileMode string
	DockerfileSize int64
	// ContextTree is the tree id of Context at Commit.
	ContextTree string
	// IgnoreBlob is the blob id of the Dockerfile-specific ignore file
	// (<dockerfile>.dockerignore) at Commit, "" when absent.
	IgnoreBlob string
	// Digest is "sha256:<hex>" over the domain tag, ContextTree,
	// DockerfileMode + DockerfileBlob, and IgnoreBlob.
	Digest string
}

// Tag is the source's content-addressed build tag.
func (b BuildSource) Tag() string { return BuildTag(b.Digest) }

// gitTreeEntry is one `git ls-tree -z -l` record.
type gitTreeEntry struct {
	mode, typ, oid string
	size           int64
	path           string
}

// parseTreeEntries parses `ls-tree -z -l` output:
// "<mode> SP <type> SP <oid> SP+ <size|-> TAB <path> NUL".
func parseTreeEntries(out string) ([]gitTreeEntry, error) {
	var es []gitTreeEntry
	for _, rec := range strings.Split(out, "\x00") {
		if rec == "" {
			continue
		}
		meta, p, ok := strings.Cut(rec, "\t")
		f := strings.Fields(meta)
		if !ok || len(f) != 4 || !gitOIDRE.MatchString(f[2]) {
			return nil, fmt.Errorf("unparsable git ls-tree record %q", rec)
		}
		var size int64 = -1
		if f[3] != "-" {
			n, err := strconv.ParseInt(f[3], 10, 64)
			if err != nil || n < 0 {
				return nil, fmt.Errorf("unparsable git ls-tree size in %q", rec)
			}
			size = n
		}
		es = append(es, gitTreeEntry{mode: f[0], typ: f[1], oid: f[2], size: size, path: p})
	}
	return es, nil
}

// lookupEntry returns the single tree entry at p in commit, or nil when
// absent. git's tree lookup never follows a symlink, so a path through a
// symlinked directory is simply absent.
func lookupEntry(ctx context.Context, git GitFunc, commit, p string) (*gitTreeEntry, error) {
	out, err := gitOutput(ctx, git, "ls-tree", "-z", "-l", "--full-tree", commit, "--", p)
	if err != nil {
		return nil, err
	}
	es, err := parseTreeEntries(out)
	if err != nil {
		return nil, err
	}
	for i := range es {
		if es[i].path == p {
			return &es[i], nil
		}
	}
	return nil, nil
}

func cleanRepoPath(field, p string) (string, error) {
	if err := checkRepoRelative(field, p); err != nil {
		return "", refuseSource("%v", err)
	}
	c := path.Clean(strings.TrimSuffix(p, "/"))
	if c == ".." || strings.HasPrefix(c, "../") || path.IsAbs(c) {
		return "", refuseSource("%s %q escapes the repository", field, p)
	}
	return c, nil
}

// ResolveBuildSource pins a declared dockerfile/context pair to git objects
// at headSHA: the context must be a TREE (the repository root for "."), the
// Dockerfile a regular-file BLOB (mode 100644 or 100755) of at most 1 MiB,
// and an optional <dockerfile>.dockerignore a regular-file blob. A symlink, a
// submodule, the wrong type, or an absent path is ErrBuildSourceRefused; a
// git failure is returned as-is.
func ResolveBuildSource(ctx context.Context, git GitFunc, headSHA, dockerfile, contextPath string) (BuildSource, error) {
	df, err := cleanRepoPath("dockerfile", dockerfile)
	if err != nil {
		return BuildSource{}, err
	}
	cx, err := cleanRepoPath("context", contextPath)
	if err != nil {
		return BuildSource{}, err
	}
	if headSHA == "" || strings.HasPrefix(headSHA, "-") {
		return BuildSource{}, fmt.Errorf("invalid head commit %q", headSHA)
	}
	out, err := gitOutput(ctx, git, "rev-parse", "--verify", "--quiet", headSHA+"^{commit}")
	if err != nil {
		return BuildSource{}, fmt.Errorf("head commit %q not found: %w", headSHA, err)
	}
	src := BuildSource{Commit: strings.TrimSpace(out), Dockerfile: df, Context: cx}
	if !gitOIDRE.MatchString(src.Commit) {
		return BuildSource{}, fmt.Errorf("git rev-parse returned %q for head %q", src.Commit, headSHA)
	}
	if cx == "." {
		t, err := gitOutput(ctx, git, "rev-parse", "--verify", "--quiet", src.Commit+"^{tree}")
		if err != nil {
			return BuildSource{}, err
		}
		src.ContextTree = strings.TrimSpace(t)
	} else {
		e, err := lookupEntry(ctx, git, src.Commit, cx)
		if err != nil {
			return BuildSource{}, err
		}
		if err := requireType(e, "context", cx, src.Commit, "tree"); err != nil {
			return BuildSource{}, err
		}
		src.ContextTree = e.oid
	}
	e, err := lookupEntry(ctx, git, src.Commit, df)
	if err != nil {
		return BuildSource{}, err
	}
	if err := requireType(e, "dockerfile", df, src.Commit, "blob"); err != nil {
		return BuildSource{}, err
	}
	if e.size > maxDockerfileBytes {
		return BuildSource{}, refuseSource("dockerfile %q is %d bytes, over the %d-byte limit", df, e.size, maxDockerfileBytes)
	}
	src.DockerfileBlob, src.DockerfileMode, src.DockerfileSize = e.oid, e.mode, e.size
	ig, err := lookupEntry(ctx, git, src.Commit, df+dockerignoreSuffix)
	if err != nil {
		return BuildSource{}, err
	}
	if ig != nil {
		if err := requireType(ig, "dockerfile ignore file", df+dockerignoreSuffix, src.Commit, "blob"); err != nil {
			return BuildSource{}, err
		}
		src.IgnoreBlob = ig.oid
	}
	src.Digest = buildDigest(src)
	return src, nil
}

func requireType(e *gitTreeEntry, field, p, commit, want string) error {
	switch {
	case e == nil:
		return refuseSource("%s %q is not present in the committed tree at %s (uncommitted files never enter a gate image)", field, p, shortOID(commit))
	case e.mode == "120000":
		return refuseSource("%s %q is a symlink at %s; declare the real path", field, p, shortOID(commit))
	case e.mode == "160000":
		return refuseSource("%s %q is a submodule at %s", field, p, shortOID(commit))
	case e.typ != want:
		return refuseSource("%s %q is a %s at %s, want a %s", field, p, e.typ, shortOID(commit), want)
	case want == "blob" && e.mode != "100644" && e.mode != "100755":
		return refuseSource("%s %q has mode %s at %s, want a regular file", field, p, e.mode, shortOID(commit))
	}
	return nil
}

func shortOID(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

// buildDigest is the content digest: a domain-separated sha256 over the
// context tree id, the Dockerfile's mode and blob id, and the
// Dockerfile-specific ignore file's blob id ("-" when absent).
func buildDigest(b BuildSource) string {
	ig := b.IgnoreBlob
	if ig == "" {
		ig = "-"
	}
	h := sha256.Sum256(fmt.Appendf(nil, "%s\ncontext-tree %s\ndockerfile %s %s\ndockerfile-ignore %s\n",
		buildDigestDomain, b.ContextTree, b.DockerfileMode, b.DockerfileBlob, ig))
	return "sha256:" + hex.EncodeToString(h[:])
}

// ReadDockerfile returns the committed Dockerfile bytes (the exact bytes the
// static scan screens and the build uses), bounded at 1 MiB.
func ReadDockerfile(ctx context.Context, git GitFunc, src BuildSource) ([]byte, error) {
	return readBlob(ctx, git, src.DockerfileBlob, maxDockerfileBytes)
}

func readBlob(ctx context.Context, git GitFunc, oid string, limit int64) ([]byte, error) {
	if !gitOIDRE.MatchString(oid) {
		return nil, fmt.Errorf("invalid git object id %q", oid)
	}
	var out bytes.Buffer
	if err := git(ctx, nil, &out, "cat-file", "blob", oid); err != nil {
		return nil, err
	}
	if int64(out.Len()) > limit {
		return nil, refuseSource("blob %s is %d bytes, over the %d-byte limit", shortOID(oid), out.Len(), limit)
	}
	return out.Bytes(), nil
}

// ContextLimits bound a materialized build context.
type ContextLimits struct {
	MaxEntries int
	MaxBytes   int64
}

// DefaultContextLimits bounds a build context at 50,000 entries / 1 GiB of
// file content.
var DefaultContextLimits = ContextLimits{MaxEntries: 50000, MaxBytes: 1 << 30}

// BuildLayout is a materialized build: ABSOLUTE paths for BuildSpec.
type BuildLayout struct {
	Dockerfile string
	Context    string
}

// maxSymlinkTarget bounds a committed symlink's target.
const maxSymlinkTarget = 4096

// MaterializeBuildContext writes src into dest (an empty or absent
// directory) from git objects only: dest/context holds the context tree
// (regular files with their committed 0644/0755 mode, symlinks as links that
// are never followed, a submodule as an empty directory), dest/Dockerfile the
// committed Dockerfile, and dest/Dockerfile.dockerignore the
// Dockerfile-specific ignore file when committed. The builder then applies
// the context's .dockerignore (or that file) to what it sends. The limits are
// checked from `ls-tree -l` sizes BEFORE anything is written
// (ErrContextTooLarge). A symlinked .dockerignore/.containerignore at the
// context root is refused (the builder would follow it on the host).
func MaterializeBuildContext(ctx context.Context, git GitFunc, src BuildSource, dest string, lim ContextLimits) (BuildLayout, error) {
	if lim.MaxEntries <= 0 || lim.MaxBytes <= 0 {
		lim = DefaultContextLimits
	}
	if !filepath.IsAbs(dest) {
		return BuildLayout{}, fmt.Errorf("build destination %q must be absolute", dest)
	}
	out, err := gitOutput(ctx, git, "ls-tree", "-r", "-z", "-l", "--full-tree", src.ContextTree)
	if err != nil {
		return BuildLayout{}, err
	}
	entries, err := parseTreeEntries(out)
	if err != nil {
		return BuildLayout{}, err
	}
	if len(entries) > lim.MaxEntries {
		return BuildLayout{}, fmt.Errorf("%w: %d entries, over the %d-entry limit; declare a narrower context", ErrContextTooLarge, len(entries), lim.MaxEntries)
	}
	var total int64
	var blobs []gitTreeEntry
	for _, e := range entries {
		if err := checkTreePath(e.path); err != nil {
			return BuildLayout{}, err
		}
		switch e.mode {
		case "100644", "100755":
			total += e.size
		case "120000":
			if (e.path == ".dockerignore" || e.path == ".containerignore") && e.typ == "blob" {
				return BuildLayout{}, refuseSource("context ignore file %q is a symlink", e.path)
			}
			if e.size > maxSymlinkTarget {
				return BuildLayout{}, refuseSource("symlink %q target is %d bytes", e.path, e.size)
			}
		case "160000":
			continue
		default:
			return BuildLayout{}, refuseSource("tree entry %q has unsupported mode %s", e.path, e.mode)
		}
		if e.typ != "blob" || e.size < 0 {
			return BuildLayout{}, refuseSource("tree entry %q is a %s with mode %s", e.path, e.typ, e.mode)
		}
		if total > lim.MaxBytes {
			return BuildLayout{}, fmt.Errorf("%w: over %d bytes of file content; declare a narrower context", ErrContextTooLarge, lim.MaxBytes)
		}
		blobs = append(blobs, e)
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return BuildLayout{}, err
	}
	if des, err := os.ReadDir(dest); err != nil || len(des) != 0 {
		return BuildLayout{}, fmt.Errorf("build destination %q must be an empty directory (%v)", dest, err)
	}
	root := filepath.Join(dest, materializedContext)
	if err := os.Mkdir(root, 0o755); err != nil {
		return BuildLayout{}, err
	}
	w := &treeWriter{root: root, dirs: map[string]bool{".": true}}
	for _, e := range entries {
		if e.mode == "160000" {
			if err := w.mkdirAll(e.path); err != nil {
				return BuildLayout{}, err
			}
		}
	}
	if err := streamBlobs(ctx, git, blobs, w.write); err != nil {
		return BuildLayout{}, err
	}
	layout := BuildLayout{Dockerfile: filepath.Join(dest, materializedDockerfle), Context: root}
	dfb, err := ReadDockerfile(ctx, git, src)
	if err != nil {
		return BuildLayout{}, err
	}
	if err := writeNew(layout.Dockerfile, dfb, 0o644); err != nil {
		return BuildLayout{}, err
	}
	if src.IgnoreBlob != "" {
		ig, err := readBlob(ctx, git, src.IgnoreBlob, maxDockerfileBytes)
		if err != nil {
			return BuildLayout{}, err
		}
		if err := writeNew(layout.Dockerfile+dockerignoreSuffix, ig, 0o644); err != nil {
			return BuildLayout{}, err
		}
	}
	return layout, nil
}

// checkTreePath refuses a tree path that is not clean and relative.
func checkTreePath(p string) error {
	if p == "" || path.IsAbs(p) || path.Clean(p) != p || p == "." {
		return refuseSource("tree entry path %q is not a clean relative path", p)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." || seg == "." {
			return refuseSource("tree entry path %q has a %q segment", p, seg)
		}
	}
	return nil
}

// treeWriter creates entries under root without ever writing through a
// symlink: every parent directory must be one it created (or verified to be a
// real directory by Lstat), and every file is created O_EXCL.
type treeWriter struct {
	root string
	dirs map[string]bool
}

func (w *treeWriter) mkdirAll(rel string) error {
	if w.dirs[rel] {
		return nil
	}
	if parent := path.Dir(rel); !w.dirs[parent] {
		if err := w.mkdirAll(parent); err != nil {
			return err
		}
	}
	full := filepath.Join(w.root, filepath.FromSlash(rel))
	if fi, err := os.Lstat(full); err == nil {
		if !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
			return refuseSource("tree entry %q collides with a non-directory entry", rel)
		}
	} else if err := os.Mkdir(full, 0o755); err != nil {
		return err
	}
	w.dirs[rel] = true
	return nil
}

func (w *treeWriter) write(e gitTreeEntry, content []byte) error {
	if err := w.mkdirAll(path.Dir(e.path)); err != nil {
		return err
	}
	full := filepath.Join(w.root, filepath.FromSlash(e.path))
	switch e.mode {
	case "120000":
		return os.Symlink(string(content), full)
	case "100755":
		return writeNew(full, content, 0o755)
	}
	return writeNew(full, content, 0o644)
}

func writeNew(p string, content []byte, mode os.FileMode) error {
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := f.Write(content); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Chmod(p, mode) // the umask must not change the committed mode
}

// streamBlobs runs one `git cat-file --batch` for every blob, in order, and
// hands each verified object to fn.
func streamBlobs(ctx context.Context, git GitFunc, blobs []gitTreeEntry, fn func(gitTreeEntry, []byte) error) error {
	if len(blobs) == 0 {
		return nil
	}
	var in strings.Builder
	for _, b := range blobs {
		in.WriteString(b.oid + "\n")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	pr, pw := io.Pipe()
	errc := make(chan error, 1)
	go func() {
		err := git(ctx, strings.NewReader(in.String()), pw, "cat-file", "--batch")
		_ = pw.CloseWithError(err)
		errc <- err
	}()
	err := consumeBatch(bufio.NewReader(pr), blobs, fn)
	if err != nil {
		cancel()
		_ = pr.CloseWithError(err)
		<-errc
		return err
	}
	return <-errc
}

func consumeBatch(r *bufio.Reader, blobs []gitTreeEntry, fn func(gitTreeEntry, []byte) error) error {
	for _, b := range blobs {
		hdr, err := r.ReadString('\n')
		if err != nil {
			return fmt.Errorf("git cat-file --batch: reading header for %s: %w", b.path, err)
		}
		f := strings.Fields(hdr)
		if len(f) != 3 || f[0] != b.oid || f[1] != "blob" {
			return fmt.Errorf("git cat-file --batch: unexpected header %q for %s (%s)", strings.TrimSpace(hdr), b.path, b.oid)
		}
		n, err := strconv.ParseInt(f[2], 10, 64)
		if err != nil || n != b.size {
			return fmt.Errorf("git cat-file --batch: size %q for %s, ls-tree reported %d", f[2], b.path, b.size)
		}
		buf := make([]byte, n+1)
		if _, err := io.ReadFull(r, buf); err != nil {
			return fmt.Errorf("git cat-file --batch: reading %s: %w", b.path, err)
		}
		if buf[n] != '\n' {
			return fmt.Errorf("git cat-file --batch: missing terminator after %s", b.path)
		}
		if err := fn(b, buf[:n]); err != nil {
			return err
		}
	}
	return nil
}
