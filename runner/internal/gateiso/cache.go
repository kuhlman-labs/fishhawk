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

// VisibleCaches is one exec's set of container-visible cache directories:
// fresh, empty, 0700, under a single throwaway Root.
type VisibleCaches struct {
	Root       string
	GoCache    string
	GoModCache string
}

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
// without ever writing to the host cache:
//
//  1. REFUSES with ErrDestinationNotEmpty when dest.GoModCache already has
//     entries (or is not a plain directory) — the invariant's control.
//  2. Skips (Skipped=true, no error) when checkout has neither go.mod nor
//     go.work, or no `go` binary is on PATH.
//  3. Runs `go mod download all` in checkout with GOMODCACHE=dest,
//     GOFLAGS=-mod=mod -modcacherw and
//     GOPROXY=file://<hostModCache>/cache/download,<host GOPROXY or
//     https://proxy.golang.org,direct> — the host cache is a read-only
//     proxy source, so every module already on the host is copied into
//     dest and only genuinely new modules reach the network.
//
// hostModCache "" resolves via `go env GOMODCACHE`. timeout bounds the
// download (0 means no bound beyond ctx).
func SeedModCache(ctx context.Context, run SeedExecFunc, checkout, hostModCache string, dest *VisibleCaches, timeout time.Duration) (SeedReport, error) {
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
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	if hostModCache == "" {
		out, err := run(ctx, checkout, os.Environ(), goBin, "env", "GOMODCACHE")
		if err != nil {
			return rep, fmt.Errorf("gateiso: resolve host GOMODCACHE: %w: %s", err, out)
		}
		hostModCache = strings.TrimSpace(string(out))
	}
	fallback := os.Getenv("GOPROXY")
	if fallback == "" {
		fallback = "https://proxy.golang.org,direct"
	}
	rep.Proxy = "file://" + filepath.ToSlash(filepath.Join(hostModCache, "cache", "download")) + "," + fallback
	env := seedEnvWithout(os.Environ(), "GOMODCACHE", "GOFLAGS", "GOPROXY")
	env = append(env,
		"GOMODCACHE="+dest.GoModCache,
		"GOFLAGS=-mod=mod -modcacherw",
		"GOPROXY="+rep.Proxy,
	)
	out, err := run(ctx, checkout, env, goBin, "mod", "download", "all")
	rep.Output = string(out)
	if err != nil {
		return rep, fmt.Errorf("gateiso: go mod download into %s: %w: %s", dest.GoModCache, err, strings.TrimSpace(rep.Output))
	}
	rep.Output = ""
	return rep, nil
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
