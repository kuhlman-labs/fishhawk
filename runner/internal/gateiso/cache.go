package gateiso

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/mod/modfile"
)

// Build-cache posture for the container path (ADR-063 gap 3), stated as an
// INVARIANT rather than a preference:
//
//   - The host GOMODCACHE is the TRUSTED SEED. It is never bind-mounted into
//     a container and never opened for writing by the seeding step: it is
//     only READ, as a GOPROXY file:// source, by a `go mod download` running
//     host-side against an empty destination.
//   - Every container-visible cache directory is created EMPTY per exec
//     (NewVisibleCaches), populated host-side (SeedModCache), mounted for
//     exactly one container exec, and removed afterwards (Remove). Host
//     seeding never opens a directory a container has already been given:
//     SeedModCache REFUSES a non-empty destination, so a symlink a previous
//     container planted in its visible dir can never redirect a later
//     host-side write into the host cache or anywhere else on the host.
//   - GOCACHE has NO seed. Each container exec starts with a cold build
//     cache. That is a deliberate, documented cost (one cold build per gate
//     exec, three per stage) rather than a shared cache that a container
//     could poison for the next exec.
//   - The seed runs HOST-SIDE against AGENT-AUTHORED module metadata, so the
//     checkout is treated as untrusted input on both axes. Environment: the
//     download never reads os.Environ(); it runs under the caller's
//     SANITIZED gate env (no runner credential is present) with
//     GOTOOLCHAIN=local (an untrusted `go`/`toolchain` directive fails the
//     seed by name instead of downloading and executing a toolchain),
//     GOWORK bound to the checkout's own go.work or off (no parent-directory
//     walk), and git config pinned to /dev/null. Filesystem: the metadata
//     `go mod download` reads or rewrites — go.mod, go.sum, go.work,
//     go.work.sum, every go.work `use` directory and every directory
//     `replace` target — is REFUSED (ErrSeedCheckout, before any go
//     process runs) when it is a symlink or resolves outside the checkout,
//     so hostile metadata cannot redirect a host-side read or write to an
//     unrelated host file.
//
// Residuals, stated not stronger: the FALLBACK paths (clone, clone-sandbox)
// still run gates with the host caches, exactly as before ADR-063; there is
// no persistent shared cache on the container path, so mod-cache
// repopulation plus a cold GOCACHE is paid on EVERY container exec. The
// container path is opt-in via FISHHAWK_GATE_IMAGE, so no default runner
// pays it.

// ErrDestinationNotEmpty is returned (wrapped) by SeedModCache when the
// destination module cache already has entries. It is the invariant's
// control: a destination a container has touched is never re-seeded.
var ErrDestinationNotEmpty = errors.New("gateiso: refusing to seed a non-empty module cache destination")

// ErrSeedCheckout is returned (wrapped) by SeedModCache when the checkout's
// module metadata would let the host-side seed read or write outside the
// checkout (checkSeedCheckout). No go process has run when it is returned.
var ErrSeedCheckout = errors.New("gateiso: refusing to seed from checkout module metadata")

// VisibleCaches is one exec's set of container-visible cache directories:
// fresh, empty, 0700, under a single throwaway Root.
type VisibleCaches struct {
	Root       string
	GoCache    string
	GoModCache string
}

// seedGoCacheDir is the throwaway GOCACHE the host-side seed itself runs
// with, under Root so Remove sweeps it. It is NOT the container-visible
// GoCache: the container's build cache stays cold (no seed) by invariant.
func (v *VisibleCaches) seedGoCacheDir() string { return filepath.Join(v.Root, "seed-gocache") }

// NewVisibleCaches creates a fresh throwaway cache root
// (`fishhawk-gatecache-*` under the OS temp dir, mode 0700) with EMPTY
// gocache and gomodcache subdirectories.
func NewVisibleCaches() (*VisibleCaches, error) {
	root, err := os.MkdirTemp("", "fishhawk-gatecache-*")
	if err != nil {
		return nil, fmt.Errorf("gateiso: create visible cache root: %w", err)
	}
	vc := &VisibleCaches{
		Root:       root,
		GoCache:    filepath.Join(root, "gocache"),
		GoModCache: filepath.Join(root, "gomodcache"),
	}
	for _, d := range []string{vc.GoCache, vc.GoModCache} {
		if err := os.Mkdir(d, 0o700); err != nil {
			_ = os.RemoveAll(root)
			return nil, fmt.Errorf("gateiso: create visible cache dir: %w", err)
		}
	}
	return vc, nil
}

// Remove deletes the whole visible cache tree. The module cache is written
// read-only by `go mod download` (and a container may have left anything
// behind), so a symlink-safe chmod walk grants the owner write permission
// first: every entry is examined with Lstat semantics (fs.WalkDir does not
// follow links), an entry whose mode is ModeSymlink is SKIPPED — never
// chmod'd, chown'd or opened through — and only regular files and
// directories reached without following a link are chmod'd. os.RemoveAll
// then unlinks the tree; a symlink is unlinked, its target untouched.
func (v *VisibleCaches) Remove() error {
	if v == nil || v.Root == "" {
		return nil
	}
	if err := makeOwnerWritableNoFollow(v.Root); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("gateiso: prepare visible cache removal: %w", err)
	}
	if err := os.RemoveAll(v.Root); err != nil {
		return fmt.Errorf("gateiso: remove visible cache root: %w", err)
	}
	return nil
}

// makeOwnerWritableNoFollow chmods regular files and directories under
// root to be owner-writable without ever following a symlink.
func makeOwnerWritableNoFollow(root string) error {
	if st, err := os.Lstat(root); err != nil {
		return err
	} else if st.Mode()&fs.ModeSymlink != 0 {
		// The root itself is a link: nothing under it is ours to chmod.
		return nil
	}
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		mode := d.Type()
		if mode&fs.ModeSymlink != 0 {
			return nil // unlink-only: never operate through a link
		}
		if mode.IsDir() {
			return os.Chmod(path, 0o700)
		}
		if mode.IsRegular() {
			info, err := d.Info()
			if err != nil {
				return err
			}
			return os.Chmod(path, info.Mode().Perm()|0o600)
		}
		return nil // sockets, devices, pipes: unlink-only
	})
}

// SeedExecFunc runs a command in dir with env and returns its combined output.
// SeedModCache accepts one so tests can observe the invocation; nil selects
// exec.CommandContext.
type SeedExecFunc func(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, error)

func seedDefaultExec(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = env
	return cmd.CombinedOutput()
}

// SeedReport records what SeedModCache did.
type SeedReport struct {
	// Skipped is true when there was nothing to seed (no go.mod/go.work in
	// the checkout, or no go binary); Reason names why.
	Skipped bool
	Reason  string
	// Proxy is the GOPROXY chain the download ran with.
	Proxy string
	// Output is the download's combined output (empty on success).
	Output string
}

// SeedModCache populates dest.GoModCache for checkout from hostModCache
// without ever writing to the host cache and without letting the checkout's
// metadata reach beyond the checkout:
//
//  1. REFUSES with ErrDestinationNotEmpty when dest.GoModCache already has
//     entries (or is not a plain directory) — the invariant's control.
//  2. Skips (Skipped=true, no error) when checkout has neither go.mod nor
//     go.work, or no `go` binary is on PATH.
//  3. REFUSES with ErrSeedCheckout, before any go process runs, when
//     go.mod / go.sum / go.work / go.work.sum is a symlink or not a regular
//     file, or when a go.work `use` directory or a directory `replace`
//     target (in go.work or in any workspace module's go.mod) resolves
//     outside the checkout, or when a metadata file does not parse.
//  4. Runs `go mod download all` in checkout under baseEnv — the caller's
//     SANITIZED gate env, never os.Environ() — with GOMODCACHE=dest, a
//     throwaway GOCACHE and GOPATH under dest.Root, GOFLAGS=-mod=mod
//     -modcacherw, GOTOOLCHAIN=local, GOWORK bound to the checkout's own
//     go.work (or off), GIT_CONFIG_GLOBAL/SYSTEM=/dev/null,
//     GIT_TERMINAL_PROMPT=0 and
//     GOPROXY=file://<hostModCache>/cache/download,<baseEnv GOPROXY or
//     https://proxy.golang.org,direct> — the host cache is a read-only
//     proxy source, so every module already on the host is copied into
//     dest and only genuinely new modules reach the network.
//
// hostModCache "" resolves via `go env GOMODCACHE` run under baseEnv (plus
// GOTOOLCHAIN=local and GOWORK=off) in dest.Root, NOT in the checkout, so
// an untrusted toolchain directive cannot influence even that probe. A nil
// baseEnv is the EMPTY environment plus the pins (fail-safe: nothing
// inherited). timeout bounds the download (0 means no bound beyond ctx).
func SeedModCache(ctx context.Context, run SeedExecFunc, checkout, hostModCache string, dest *VisibleCaches, baseEnv []string, timeout time.Duration) (SeedReport, error) {
	if run == nil {
		run = seedDefaultExec
	}
	var rep SeedReport
	if dest == nil || dest.GoModCache == "" {
		return rep, errors.New("gateiso: SeedModCache needs a destination")
	}
	if err := seedRequireEmptyDir(dest.GoModCache); err != nil {
		return rep, err
	}
	if !seedHasGoModule(checkout) {
		rep.Skipped, rep.Reason = true, "no go.mod or go.work in checkout"
		return rep, nil
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		rep.Skipped, rep.Reason = true, "go not on PATH: "+err.Error()
		return rep, nil
	}
	root, workFile, err := checkSeedCheckout(checkout)
	if err != nil {
		return rep, err
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	probeEnv := seedEnvWithout(baseEnv, "GOTOOLCHAIN", "GOWORK")
	probeEnv = append(probeEnv, "GOTOOLCHAIN=local", "GOWORK=off")
	if hostModCache == "" {
		out, err := run(ctx, dest.Root, probeEnv, goBin, "env", "GOMODCACHE")
		if err != nil {
			return rep, fmt.Errorf("gateiso: resolve host GOMODCACHE: %w: %s", err, out)
		}
		hostModCache = strings.TrimSpace(string(out))
	}
	fallback := seedEnvValue(baseEnv, "GOPROXY")
	if fallback == "" {
		fallback = "https://proxy.golang.org,direct"
	}
	rep.Proxy = "file://" + filepath.ToSlash(filepath.Join(hostModCache, "cache", "download")) + "," + fallback
	seedCache := dest.seedGoCacheDir()
	if err := os.MkdirAll(seedCache, 0o700); err != nil {
		return rep, fmt.Errorf("gateiso: create seed GOCACHE: %w", err)
	}
	gowork := "off"
	if workFile != "" {
		gowork = workFile
	}
	pins := []string{
		"GOMODCACHE=" + dest.GoModCache,
		"GOCACHE=" + seedCache,
		"GOPATH=" + filepath.Join(dest.Root, "seed-gopath"),
		"GOFLAGS=-mod=mod -modcacherw",
		"GOPROXY=" + rep.Proxy,
		"GOTOOLCHAIN=local",
		"GOWORK=" + gowork,
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
	}
	env := seedEnvWithout(baseEnv, seedPinKeys(pins)...)
	env = append(env, pins...)
	out, err := run(ctx, root, env, goBin, "mod", "download", "all")
	rep.Output = string(out)
	if err != nil {
		return rep, fmt.Errorf("gateiso: go mod download into %s: %w: %s", dest.GoModCache, err, strings.TrimSpace(rep.Output))
	}
	rep.Output = ""
	return rep, nil
}

// seedPinKeys returns the variable names of a KEY=VALUE list.
func seedPinKeys(pins []string) []string {
	keys := make([]string, 0, len(pins))
	for _, kv := range pins {
		k, _, _ := strings.Cut(kv, "=")
		keys = append(keys, k)
	}
	return keys
}

// seedEnvValue returns the LAST value of name in env ("" when absent).
func seedEnvValue(env []string, name string) string {
	val := ""
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, name+"="); ok {
			val = v
		}
	}
	return val
}

// seedMetadataFiles are the files `go mod download` reads or rewrites at a
// module or workspace root; each must be a regular file or absent.
var seedMetadataFiles = []string{"go.mod", "go.sum", "go.work", "go.work.sum"}

// checkSeedCheckout resolves checkout and refuses (ErrSeedCheckout) any
// module metadata the seed would read or write through a symlink or outside
// the resolved checkout root: the four metadata files at the root, every
// go.work `use` directory (and that module's own metadata files and
// directory `replace` targets), and every directory `replace` target in
// go.work or the root go.mod. It returns the resolved root and the go.work
// path to bind GOWORK to ("" when the checkout has none).
func checkSeedCheckout(checkout string) (root, workFile string, err error) {
	root, err = filepath.EvalSymlinks(checkout)
	if err != nil {
		return "", "", fmt.Errorf("%w: checkout %q is unresolvable: %v", ErrSeedCheckout, checkout, err)
	}
	if st, err := os.Lstat(root); err != nil || !st.IsDir() {
		return "", "", fmt.Errorf("%w: checkout %q is not a directory", ErrSeedCheckout, root)
	}
	if err := seedRequireRegularMetadata(root); err != nil {
		return "", "", err
	}
	work := filepath.Join(root, "go.work")
	if _, err := os.Lstat(work); err == nil {
		data, err := os.ReadFile(work)
		if err != nil {
			return "", "", fmt.Errorf("%w: %s: %v", ErrSeedCheckout, work, err)
		}
		wf, err := modfile.ParseWork(work, data, seedKeepVersion)
		if err != nil {
			return "", "", fmt.Errorf("%w: %s does not parse: %v", ErrSeedCheckout, work, err)
		}
		for _, r := range wf.Replace {
			if err := seedRequireDirInside(root, root, r.New.Path, work+" replace"); err != nil {
				return "", "", err
			}
		}
		for _, u := range wf.Use {
			modDir, err := seedResolveInside(root, root, u.Path, work+" use")
			if err != nil {
				return "", "", err
			}
			if err := seedRequireRegularMetadata(modDir); err != nil {
				return "", "", err
			}
			if err := seedCheckGoModReplaces(root, modDir); err != nil {
				return "", "", err
			}
		}
		return root, work, nil
	}
	if err := seedCheckGoModReplaces(root, root); err != nil {
		return "", "", err
	}
	return root, "", nil
}

// seedRequireRegularMetadata refuses any seedMetadataFiles entry in dir that
// exists but is not a regular file (Lstat: a symlink is refused, never
// followed).
func seedRequireRegularMetadata(dir string) error {
	for _, name := range seedMetadataFiles {
		p := filepath.Join(dir, name)
		st, err := os.Lstat(p)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("%w: %s: %v", ErrSeedCheckout, p, err)
		}
		if !st.Mode().IsRegular() {
			return fmt.Errorf("%w: %s is not a regular file (mode %s)", ErrSeedCheckout, p, st.Mode().Type())
		}
	}
	return nil
}

// seedCheckGoModReplaces parses modDir/go.mod (absent → nothing to check)
// and refuses any directory replace target resolving outside root.
func seedCheckGoModReplaces(root, modDir string) error {
	p := filepath.Join(modDir, "go.mod")
	data, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%w: %s: %v", ErrSeedCheckout, p, err)
	}
	// Strict Parse: ParseLax drops replace directives, the very thing this
	// check reads. seedKeepVersion accepts any version string so the check
	// is on directive STRUCTURE, not on go.mod canonical-version hygiene.
	mf, err := modfile.Parse(p, data, seedKeepVersion)
	if err != nil {
		return fmt.Errorf("%w: %s does not parse: %v", ErrSeedCheckout, p, err)
	}
	for _, r := range mf.Replace {
		if err := seedRequireDirInside(root, modDir, r.New.Path, p+" replace"); err != nil {
			return err
		}
	}
	return nil
}

// seedKeepVersion is the modfile.Parse VersionFixer that leaves every
// version as written.
func seedKeepVersion(_, version string) (string, error) { return version, nil }

// seedRequireDirInside is seedResolveInside for a replace target: a module
// path (not a directory path) needs no check.
func seedRequireDirInside(root, base, path, what string) error {
	if !modfile.IsDirectoryPath(path) {
		return nil
	}
	_, err := seedResolveInside(root, base, path, what)
	return err
}

// seedResolveInside resolves path (relative to base when not absolute)
// through symlinks and refuses it unless it lies at or under root.
func seedResolveInside(root, base, path, what string) (string, error) {
	p := path
	if !filepath.IsAbs(p) {
		p = filepath.Join(base, p)
	}
	resolved, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", fmt.Errorf("%w: %s %q is unresolvable: %v", ErrSeedCheckout, what, path, err)
	}
	if !underRoot(resolved, root) {
		return "", fmt.Errorf("%w: %s %q resolves to %q outside the checkout %q", ErrSeedCheckout, what, path, resolved, root)
	}
	return resolved, nil
}

// seedRequireEmptyDir fails unless path is a plain (non-symlink) directory with
// no entries.
func seedRequireEmptyDir(path string) error {
	st, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("%w: %s: %v", ErrDestinationNotEmpty, path, err)
	}
	if st.Mode()&fs.ModeSymlink != 0 || !st.IsDir() {
		return fmt.Errorf("%w: %s is not a plain directory (mode %s)", ErrDestinationNotEmpty, path, st.Mode())
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return fmt.Errorf("%w: %s: %v", ErrDestinationNotEmpty, path, err)
	}
	if len(entries) > 0 {
		return fmt.Errorf("%w: %s already has %d entries (first: %q)", ErrDestinationNotEmpty, path, len(entries), entries[0].Name())
	}
	return nil
}

func seedHasGoModule(checkout string) bool {
	for _, f := range []string{"go.mod", "go.work"} {
		if _, err := os.Stat(filepath.Join(checkout, f)); err == nil {
			return true
		}
	}
	return false
}

// seedEnvWithout returns env without any of the named variables.
func seedEnvWithout(env []string, names ...string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		drop := false
		for _, n := range names {
			if strings.HasPrefix(kv, n+"=") {
				drop = true
				break
			}
		}
		if !drop {
			out = append(out, kv)
		}
	}
	return out
}
