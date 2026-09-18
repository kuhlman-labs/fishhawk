package gateiso

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestNewVisibleCaches_FreshEmpty0700(t *testing.T) {
	vc, err := NewVisibleCaches()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = vc.Remove() })
	if !strings.HasPrefix(filepath.Base(vc.Root), "fishhawk-gatecache-") {
		t.Fatalf("root %s lacks the fishhawk-gatecache- prefix", vc.Root)
	}
	for _, d := range []string{vc.Root, vc.GoCache, vc.GoModCache} {
		st, err := os.Lstat(d)
		if err != nil {
			t.Fatal(err)
		}
		if !st.IsDir() || st.Mode().Perm() != 0o700 {
			t.Fatalf("%s: mode %s, want a 0700 directory", d, st.Mode())
		}
		entries, _ := os.ReadDir(d)
		if d != vc.Root && len(entries) != 0 {
			t.Fatalf("%s not empty: %d entries", d, len(entries))
		}
	}
	other, err := NewVisibleCaches()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Remove() })
	if other.Root == vc.Root {
		t.Fatal("two NewVisibleCaches share a root")
	}
}

func TestVisibleCaches_RemoveReadOnlyTree(t *testing.T) {
	vc, err := NewVisibleCaches()
	if err != nil {
		t.Fatal(err)
	}
	// `go mod download` leaves a read-only tree: 0555 dirs, 0444 files.
	deep := filepath.Join(vc.GoModCache, "example.com", "dep@v0.1.0")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(deep, "dep.go"), []byte("package dep\n"), 0o444); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{deep, filepath.Dir(deep), vc.GoModCache} {
		if err := os.Chmod(d, 0o555); err != nil {
			t.Fatal(err)
		}
	}
	if err := vc.Remove(); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Lstat(vc.Root); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("root still present after Remove: %v", err)
	}
	if err := vc.Remove(); err != nil {
		t.Fatalf("second Remove must be a no-op, got %v", err)
	}
	var nilVC *VisibleCaches
	if err := nilVC.Remove(); err != nil {
		t.Fatalf("nil Remove: %v", err)
	}
}

// TestVisibleCaches_RemoveNeverOperatesThroughSymlink is approval condition
// (2): a container may plant a symlink in its visible dir pointing at a host
// file; cleanup must unlink the link and never chmod/open its target.
func TestVisibleCaches_RemoveNeverOperatesThroughSymlink(t *testing.T) {
	outside := t.TempDir()
	target := filepath.Join(outside, "host-secret")
	const content = "host bytes\n"
	if err := os.WriteFile(target, []byte(content), 0o444); err != nil {
		t.Fatal(err)
	}
	targetDir := filepath.Join(outside, "host-dir")
	if err := os.Mkdir(targetDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(targetDir, "inner"), []byte("inner\n"), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(targetDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(targetDir, 0o755) })
	past := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	for _, p := range []string{target, targetDir} {
		if err := os.Chtimes(p, past, past); err != nil {
			t.Fatal(err)
		}
	}

	vc, err := NewVisibleCaches()
	if err != nil {
		t.Fatal(err)
	}
	// Planted deep inside a read-only subtree, as a container would leave it.
	sub := filepath.Join(vc.GoModCache, "cache", "download")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	fileLink := filepath.Join(sub, "planted-file")
	dirLink := filepath.Join(sub, "planted-dir")
	if err := os.Symlink(target, fileLink); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(targetDir, dirLink); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sub, 0o555); err != nil {
		t.Fatal(err)
	}

	if err := vc.Remove(); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Lstat(vc.Root); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("visible root still present: %v", err)
	}
	for _, l := range []string{fileLink, dirLink} {
		if _, err := os.Lstat(l); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("planted link %s still present: %v", l, err)
		}
	}
	st, err := os.Stat(target)
	if err != nil {
		t.Fatalf("host target vanished: %v", err)
	}
	if st.Mode().Perm() != 0o444 {
		t.Fatalf("host target mode = %s, want 0444: cleanup chmod'd THROUGH the symlink", st.Mode())
	}
	if !st.ModTime().Equal(past) {
		t.Fatalf("host target mtime changed: %s", st.ModTime())
	}
	if b, _ := os.ReadFile(target); string(b) != content {
		t.Fatalf("host target contents changed: %q", b)
	}
	dst, err := os.Stat(targetDir)
	if err != nil {
		t.Fatalf("host dir vanished: %v", err)
	}
	if dst.Mode().Perm() != 0o555 {
		t.Fatalf("host dir mode = %s, want 0555: cleanup chmod'd THROUGH the dir symlink", dst.Mode())
	}
	if _, err := os.Stat(filepath.Join(targetDir, "inner")); err != nil {
		t.Fatalf("host dir contents removed THROUGH the symlink: %v", err)
	}
}

// --- SeedModCache -----------------------------------------------------------

// seedFixture is a checkout requiring example.com/dep v0.1.0 and a HOST
// module cache that already holds it (populated by go itself from a
// hand-built file:// proxy), so seeding can run with the network `off`.
type seedFixture struct {
	checkout string
	hostMod  string
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func newSeedFixture(t *testing.T) seedFixture {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not on PATH")
	}
	root := t.TempDir()
	// Hand-built GOPROXY layout for example.com/dep v0.1.0.
	const depMod = "module example.com/dep\n\ngo 1.21\n"
	proxy := filepath.Join(root, "proxy")
	vdir := filepath.Join(proxy, "example.com", "dep", "@v")
	writeFile(t, filepath.Join(vdir, "list"), "v0.1.0\n")
	writeFile(t, filepath.Join(vdir, "v0.1.0.info"), `{"Version":"v0.1.0","Time":"2026-01-01T00:00:00Z"}`)
	writeFile(t, filepath.Join(vdir, "v0.1.0.mod"), depMod)
	var zb bytes.Buffer
	zw := zip.NewWriter(&zb)
	for name, body := range map[string]string{
		"example.com/dep@v0.1.0/go.mod": depMod,
		"example.com/dep@v0.1.0/dep.go": "package dep\n\n// V is the dependency's value.\nconst V = 1\n",
	} {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(vdir, "v0.1.0.zip"), zb.String())

	checkout := filepath.Join(root, "checkout")
	writeFile(t, filepath.Join(checkout, "go.mod"), "module example.com/app\n\ngo 1.21\n\nrequire example.com/dep v0.1.0\n")
	writeFile(t, filepath.Join(checkout, "main.go"), "package app\n\nimport \"example.com/dep\"\n\n// V re-exports dep.V.\nconst V = dep.V\n")

	// Populate the HOST cache from the proxy: tidy writes go.sum, and a
	// `go mod download all` then writes the .info entries a file:// proxy
	// consumer needs (tidy alone leaves them out).
	hostMod := filepath.Join(root, "hostmod")
	for _, args := range [][]string{{"mod", "tidy"}, {"mod", "download", "all"}} {
		cmd := exec.Command("go", args...)
		cmd.Dir = checkout
		cmd.Env = append(seedEnvWithout(os.Environ(), "GOMODCACHE", "GOPROXY", "GOSUMDB", "GOFLAGS", "GONOSUMDB", "GOWORK", "GOTOOLCHAIN"),
			"GOMODCACHE="+hostMod, "GOPROXY=file://"+filepath.ToSlash(proxy), "GOSUMDB=off", "GOFLAGS=-modcacherw", "GOWORK=off", "GOTOOLCHAIN=local")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("populate host cache (go %v): %v\n%s", args, err, out)
		}
	}
	for _, f := range []string{"v0.1.0.zip", "v0.1.0.info", "v0.1.0.mod"} {
		if _, err := os.Stat(filepath.Join(hostMod, "cache", "download", "example.com", "dep", "@v", f)); err != nil {
			t.Fatalf("host cache not populated: %v", err)
		}
	}
	t.Cleanup(func() { _ = makeOwnerWritableNoFollow(hostMod) })
	// The seeding step under test must not touch the network, and must not
	// let a toolchain switch route through the proxy chain either.
	t.Setenv("GOPROXY", "off")
	t.Setenv("GOSUMDB", "off")
	t.Setenv("GOWORK", "off")
	t.Setenv("GOTOOLCHAIN", "local")
	return seedFixture{checkout: checkout, hostMod: hostMod}
}

type treeEntry struct {
	mode  fs.FileMode
	size  int64
	mtime time.Time
}

func snapshotTree(t *testing.T, root string) map[string]treeEntry {
	t.Helper()
	m := map[string]treeEntry{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		m[rel] = treeEntry{mode: info.Mode(), size: info.Size(), mtime: info.ModTime()}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func sortedKeys(m map[string]treeEntry) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func assertTreeUnchanged(t *testing.T, what string, before, after map[string]treeEntry) {
	t.Helper()
	bk, ak := sortedKeys(before), sortedKeys(after)
	if strings.Join(bk, "\n") != strings.Join(ak, "\n") {
		t.Fatalf("%s entry set changed:\nbefore %v\nafter  %v", what, bk, ak)
	}
	for k, b := range before {
		if a := after[k]; a != b {
			t.Fatalf("%s entry %s changed: %+v -> %+v", what, k, b, a)
		}
	}
}

func TestSeedModCache_PopulatesFromHostCacheWithoutTouchingIt(t *testing.T) {
	fx := newSeedFixture(t)
	hostBefore := snapshotTree(t, fx.hostMod)
	sumBefore, _ := os.ReadFile(filepath.Join(fx.checkout, "go.sum"))

	vc, err := NewVisibleCaches()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = vc.Remove() })
	var seenEnv []string
	run := func(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, error) {
		seenEnv = env
		return seedDefaultExec(ctx, dir, env, name, args...)
	}
	rep, err := SeedModCache(context.Background(), run, fx.checkout, fx.hostMod, vc, os.Environ(), time.Minute)
	if err != nil {
		t.Fatalf("SeedModCache: %v\n%s", err, rep.Output)
	}
	if rep.Skipped {
		t.Fatalf("unexpected skip: %s", rep.Reason)
	}
	wantProxy := "file://" + filepath.ToSlash(filepath.Join(fx.hostMod, "cache", "download")) + ",off"
	if rep.Proxy != wantProxy {
		t.Fatalf("Proxy = %q, want %q", rep.Proxy, wantProxy)
	}
	for _, kv := range []string{"GOMODCACHE=" + vc.GoModCache, "GOFLAGS=-mod=mod -modcacherw", "GOPROXY=" + wantProxy} {
		if !containsEnv(seenEnv, kv) {
			t.Fatalf("download env lacks %q", kv)
		}
	}
	for _, rel := range []string{
		filepath.Join("cache", "download", "example.com", "dep", "@v", "v0.1.0.zip"),
		filepath.Join("example.com", "dep@v0.1.0", "dep.go"),
	} {
		if _, err := os.Stat(filepath.Join(vc.GoModCache, rel)); err != nil {
			t.Fatalf("destination lacks %s: %v", rel, err)
		}
	}
	if entries, _ := os.ReadDir(vc.GoCache); len(entries) != 0 {
		t.Fatalf("GOCACHE has no seed and must stay empty, has %d entries", len(entries))
	}
	assertTreeUnchanged(t, "host module cache", hostBefore, snapshotTree(t, fx.hostMod))
	if sumAfter, _ := os.ReadFile(filepath.Join(fx.checkout, "go.sum")); !bytes.Equal(sumBefore, sumAfter) {
		t.Fatalf("seeding rewrote the checkout's go.sum:\n%s\n---\n%s", sumBefore, sumAfter)
	}
}

func containsEnv(env []string, kv string) bool {
	for _, e := range env {
		if e == kv {
			return true
		}
	}
	return false
}

// TestSeedModCache_RefusesNonEmptyDestination is the gap-3 INVARIANT: a
// destination a container has been given is never re-seeded, so a symlink
// planted there can never redirect a host-side write.
func TestSeedModCache_RefusesNonEmptyDestination(t *testing.T) {
	fx := newSeedFixture(t)
	canaryDir := t.TempDir()
	canary := filepath.Join(canaryDir, "canary")
	const canaryBytes = "canary\n"
	if err := os.WriteFile(canary, []byte(canaryBytes), 0o644); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	if err := os.Chtimes(canary, past, past); err != nil {
		t.Fatal(err)
	}
	assertCanary := func(step string) {
		t.Helper()
		st, err := os.Stat(canary)
		if err != nil {
			t.Fatalf("%s: canary vanished: %v", step, err)
		}
		if b, _ := os.ReadFile(canary); string(b) != canaryBytes {
			t.Fatalf("%s: canary bytes changed: %q", step, b)
		}
		if !st.ModTime().Equal(past) {
			t.Fatalf("%s: canary mtime changed: %s", step, st.ModTime())
		}
		if entries, _ := os.ReadDir(canaryDir); len(entries) != 1 {
			t.Fatalf("%s: canary dir gained entries: %d", step, len(entries))
		}
	}

	// Seed A once, then plant what a container would: symlinks from A into
	// the canary dir, one at the proxy download root and one at the top.
	a, err := NewVisibleCaches()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Remove() })
	if _, err := SeedModCache(context.Background(), nil, fx.checkout, fx.hostMod, a, os.Environ(), time.Minute); err != nil {
		t.Fatalf("initial seed: %v", err)
	}
	download := filepath.Join(a.GoModCache, "cache", "download")
	_ = os.Chmod(download, 0o755)
	if err := os.Symlink(canaryDir, filepath.Join(download, "evil.example")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(canaryDir, filepath.Join(a.GoModCache, "planted")); err != nil {
		t.Fatal(err)
	}
	hostBefore := snapshotTree(t, fx.hostMod)

	// (i) re-seeding A is REFUSED by identity, and nothing was written.
	_, err = SeedModCache(context.Background(), nil, fx.checkout, fx.hostMod, a, os.Environ(), time.Minute)
	if !errors.Is(err, ErrDestinationNotEmpty) {
		t.Fatalf("re-seed of a used destination: err = %v, want ErrDestinationNotEmpty", err)
	}
	if !strings.Contains(err.Error(), a.GoModCache) {
		t.Fatalf("refusal does not name the destination: %v", err)
	}
	assertCanary("after refused re-seed")

	// (ii) a fresh B is seeded from the HOST cache; the planted entries in
	// A reach neither B nor the host cache, and the canary is untouched.
	b, err := NewVisibleCaches()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Remove() })
	if _, err := SeedModCache(context.Background(), nil, fx.checkout, fx.hostMod, b, os.Environ(), time.Minute); err != nil {
		t.Fatalf("seed B: %v", err)
	}
	for _, rel := range []string{"planted", filepath.Join("cache", "download", "evil.example")} {
		if _, err := os.Lstat(filepath.Join(b.GoModCache, rel)); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("planted entry %s reached B: %v", rel, err)
		}
		if _, err := os.Lstat(filepath.Join(fx.hostMod, rel)); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("planted entry %s reached the HOST cache: %v", rel, err)
		}
	}
	assertTreeUnchanged(t, "host module cache", hostBefore, snapshotTree(t, fx.hostMod))
	assertCanary("after seeding B")

	// A symlinked or non-directory destination is refused by the same
	// control, and a missing one too.
	c := &VisibleCaches{GoModCache: filepath.Join(t.TempDir(), "link")}
	if err := os.Symlink(t.TempDir(), c.GoModCache); err != nil {
		t.Fatal(err)
	}
	if _, err := SeedModCache(context.Background(), nil, fx.checkout, fx.hostMod, c, os.Environ(), time.Minute); !errors.Is(err, ErrDestinationNotEmpty) {
		t.Fatalf("symlinked destination: err = %v, want ErrDestinationNotEmpty", err)
	}
	d := &VisibleCaches{GoModCache: filepath.Join(t.TempDir(), "missing")}
	if _, err := SeedModCache(context.Background(), nil, fx.checkout, fx.hostMod, d, os.Environ(), time.Minute); !errors.Is(err, ErrDestinationNotEmpty) {
		t.Fatalf("missing destination: err = %v, want ErrDestinationNotEmpty", err)
	}
	if _, err := SeedModCache(context.Background(), nil, fx.checkout, fx.hostMod, nil, os.Environ(), time.Minute); err == nil {
		t.Fatal("nil destination accepted")
	}
}

func TestSeedModCache_SkipsWithoutGoModule(t *testing.T) {
	vc, err := NewVisibleCaches()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = vc.Remove() })
	called := false
	run := func(context.Context, string, []string, string, ...string) ([]byte, error) {
		called = true
		return nil, nil
	}
	rep, err := SeedModCache(context.Background(), run, t.TempDir(), t.TempDir(), vc, os.Environ(), time.Minute)
	if err != nil || !rep.Skipped || !strings.Contains(rep.Reason, "no go.mod or go.work") {
		t.Fatalf("rep = %+v, err = %v; want a named skip", rep, err)
	}
	if called {
		t.Fatal("nothing must run for a checkout without a Go module")
	}
}

func TestSeedModCache_SkipsWithoutGoBinary(t *testing.T) {
	vc, err := NewVisibleCaches()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = vc.Remove() })
	checkout := t.TempDir()
	writeFile(t, filepath.Join(checkout, "go.mod"), "module x\n")
	t.Setenv("PATH", t.TempDir())
	called := false
	run := func(context.Context, string, []string, string, ...string) ([]byte, error) {
		called = true
		return nil, nil
	}
	rep, err := SeedModCache(context.Background(), run, checkout, t.TempDir(), vc, os.Environ(), time.Minute)
	if err != nil || !rep.Skipped || !strings.Contains(rep.Reason, "go not on PATH") {
		t.Fatalf("rep = %+v, err = %v; want a named skip", rep, err)
	}
	if called {
		t.Fatal("nothing must run without a go binary")
	}
}

func TestSeedModCache_DownloadFailureIsNamed(t *testing.T) {
	vc, err := NewVisibleCaches()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = vc.Remove() })
	checkout := t.TempDir()
	writeFile(t, filepath.Join(checkout, "go.mod"), "module x\n")
	run := func(context.Context, string, []string, string, ...string) ([]byte, error) {
		return []byte("boom output"), errors.New("exit status 1")
	}
	rep, err := SeedModCache(context.Background(), run, checkout, t.TempDir(), vc, os.Environ(), time.Minute)
	if err == nil || !strings.Contains(err.Error(), "boom output") || !strings.Contains(err.Error(), vc.GoModCache) {
		t.Fatalf("err = %v; want the output and destination named", err)
	}
	if rep.Output != "boom output" {
		t.Fatalf("Output = %q", rep.Output)
	}
}

func TestSeedModCache_ResolvesHostCacheFromGoEnv(t *testing.T) {
	vc, err := NewVisibleCaches()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = vc.Remove() })
	checkout := t.TempDir()
	writeFile(t, filepath.Join(checkout, "go.mod"), "module x\n")
	t.Setenv("GOPROXY", "https://example.invalid")
	var calls [][]string
	run := func(_ context.Context, _ string, _ []string, name string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string{name}, args...))
		if len(args) == 2 && args[0] == "env" {
			return []byte("/host/gomodcache\n"), nil
		}
		return nil, nil
	}
	rep, err := SeedModCache(context.Background(), run, checkout, "", vc, os.Environ(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if want := "file:///host/gomodcache/cache/download,https://example.invalid"; rep.Proxy != want {
		t.Fatalf("Proxy = %q, want %q", rep.Proxy, want)
	}
	if len(calls) != 2 || calls[0][1] != "env" || strings.Join(calls[1][1:], " ") != "mod download all" {
		t.Fatalf("calls = %q", calls)
	}
}

func TestMakeOwnerWritableNoFollow_EdgeEntries(t *testing.T) {
	t.Run("root is a symlink: nothing chmod'd, no error", func(t *testing.T) {
		target := t.TempDir()
		inner := filepath.Join(target, "f")
		writeFile(t, inner, "x")
		if err := os.Chmod(inner, 0o400); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(t.TempDir(), "link")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		if err := makeOwnerWritableNoFollow(link); err != nil {
			t.Fatal(err)
		}
		st, err := os.Stat(inner)
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != 0o400 {
			t.Fatalf("file behind the link root was chmod'd to %v", st.Mode().Perm())
		}
	})
	t.Run("a fifo is left alone (unlink-only)", func(t *testing.T) {
		root := t.TempDir()
		if err := syscall.Mkfifo(filepath.Join(root, "pipe"), 0o600); err != nil {
			t.Skipf("mkfifo unavailable: %v", err)
		}
		if err := makeOwnerWritableNoFollow(root); err != nil {
			t.Fatal(err)
		}
	})
}

func TestSeedRequireEmptyDir_UnreadableDirIsRefused(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	dir := filepath.Join(t.TempDir(), "d")
	if err := os.Mkdir(dir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	err := seedRequireEmptyDir(dir)
	if !errors.Is(err, ErrDestinationNotEmpty) {
		t.Fatalf("err = %v, want ErrDestinationNotEmpty", err)
	}
}

func TestSeedModCache_GoEnvFailureIsNamed(t *testing.T) {
	vc, err := NewVisibleCaches()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = vc.Remove() })
	checkout := t.TempDir()
	writeFile(t, filepath.Join(checkout, "go.mod"), "module x\n")
	run := func(_ context.Context, _ string, _ []string, _ string, args ...string) ([]byte, error) {
		if len(args) == 2 && args[0] == "env" {
			return []byte("go: cannot find GOROOT"), errors.New("exit status 2")
		}
		t.Fatalf("download must not run after a failed go env, got %q", args)
		return nil, nil
	}
	_, err = SeedModCache(context.Background(), run, checkout, "", vc, os.Environ(), time.Minute)
	if err == nil || !strings.Contains(err.Error(), "resolve host GOMODCACHE") || !strings.Contains(err.Error(), "cannot find GOROOT") {
		t.Fatalf("err = %v", err)
	}
}

// --- Untrusted checkout metadata + seed environment --------------------------

// seedCanary is a host file OUTSIDE every checkout under test, with the
// mode/mtime/bytes a seed must leave untouched.
type seedCanary struct {
	dir, path string
	bytes     string
	past      time.Time
}

func newSeedCanary(t *testing.T) seedCanary {
	t.Helper()
	c := seedCanary{dir: t.TempDir(), bytes: "canary\n", past: time.Now().Add(-2 * time.Hour).Truncate(time.Second)}
	c.path = filepath.Join(c.dir, "canary")
	// 0644, not 0444: a write THROUGH a planted link must be able to
	// succeed so the assertion is on the bytes, not on a permission error.
	if err := os.WriteFile(c.path, []byte(c.bytes), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(c.path, c.past, c.past); err != nil {
		t.Fatal(err)
	}
	return c
}

func (c seedCanary) assertUntouched(t *testing.T, step string) {
	t.Helper()
	st, err := os.Stat(c.path)
	if err != nil {
		t.Fatalf("%s: canary vanished: %v", step, err)
	}
	if b, _ := os.ReadFile(c.path); string(b) != c.bytes {
		t.Fatalf("%s: canary bytes changed: %q", step, b)
	}
	if !st.ModTime().Equal(c.past) {
		t.Fatalf("%s: canary mtime changed: %s", step, st.ModTime())
	}
	if entries, _ := os.ReadDir(c.dir); len(entries) != 1 {
		t.Fatalf("%s: canary dir gained entries: %d", step, len(entries))
	}
}

// TestSeedModCache_RefusesMetadataReachingOutsideCheckout is the
// external-canary fixture for hostile checkout metadata: a go.sum / go.mod /
// go.work / go.work.sum symlinked to a host file outside the checkout, a
// go.work `use` of a directory outside it (directly and through a symlinked
// directory), and a directory `replace` outside it (in go.work and in a
// workspace module's go.mod), and a directory `replace` target inside the
// checkout whose own go.mod is symlinked to a host file (go reads the
// replacement's go.mod) are each REFUSED with ErrSeedCheckout BEFORE any
// go process runs — the exec seam is never reached — and the canary is
// untouched. Deleting checkSeedCheckout turns every row red (the seam is
// reached with the hostile checkout).
func TestSeedModCache_RefusesMetadataReachingOutsideCheckout(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not on PATH")
	}
	canary := newSeedCanary(t)
	outsideMod := t.TempDir()
	writeFile(t, filepath.Join(outsideMod, "go.mod"), "module example.com/outside\n\ngo 1.21\n")
	rows := []struct {
		name  string
		plant func(t *testing.T, co string)
		want  string
	}{
		{"go.sum symlink to host file", func(t *testing.T, co string) {
			writeFile(t, filepath.Join(co, "go.mod"), "module x\n\ngo 1.21\n")
			mustSymlink(t, canary.path, filepath.Join(co, "go.sum"))
		}, "go.sum is not a regular file"},
		{"go.mod symlink to host file", func(t *testing.T, co string) {
			mustSymlink(t, canary.path, filepath.Join(co, "go.mod"))
		}, "go.mod is not a regular file"},
		{"go.work symlink to host file", func(t *testing.T, co string) {
			writeFile(t, filepath.Join(co, "go.mod"), "module x\n\ngo 1.21\n")
			mustSymlink(t, canary.path, filepath.Join(co, "go.work"))
		}, "go.work is not a regular file"},
		{"go.work.sum symlink to host file", func(t *testing.T, co string) {
			writeFile(t, filepath.Join(co, "go.mod"), "module x\n\ngo 1.21\n")
			writeFile(t, filepath.Join(co, "go.work"), "go 1.21\n\nuse .\n")
			mustSymlink(t, canary.path, filepath.Join(co, "go.work.sum"))
		}, "go.work.sum is not a regular file"},
		{"go.work use outside checkout", func(t *testing.T, co string) {
			writeFile(t, filepath.Join(co, "go.mod"), "module x\n\ngo 1.21\n")
			writeFile(t, filepath.Join(co, "go.work"), "go 1.21\n\nuse (\n\t.\n\t"+outsideMod+"\n)\n")
		}, "outside the checkout"},
		{"go.work use through symlinked dir", func(t *testing.T, co string) {
			writeFile(t, filepath.Join(co, "go.mod"), "module x\n\ngo 1.21\n")
			mustSymlink(t, outsideMod, filepath.Join(co, "sub"))
			writeFile(t, filepath.Join(co, "go.work"), "go 1.21\n\nuse ./sub\n")
		}, "outside the checkout"},
		{"go.work replace outside checkout", func(t *testing.T, co string) {
			writeFile(t, filepath.Join(co, "go.mod"), "module x\n\ngo 1.21\n")
			writeFile(t, filepath.Join(co, "go.work"), "go 1.21\n\nuse .\n\nreplace example.com/outside => "+outsideMod+"\n")
		}, "outside the checkout"},
		{"go.mod replace outside checkout", func(t *testing.T, co string) {
			writeFile(t, filepath.Join(co, "go.mod"), "module x\n\ngo 1.21\n\nrequire example.com/outside v0.0.0\n\nreplace example.com/outside => ../../"+filepath.Base(filepath.Dir(outsideMod))+"/"+filepath.Base(outsideMod)+"\n")
		}, "outside the checkout"},
		{"workspace module go.mod replace outside checkout", func(t *testing.T, co string) {
			writeFile(t, filepath.Join(co, "go.work"), "go 1.21\n\nuse ./m\n")
			writeFile(t, filepath.Join(co, "m", "go.mod"), "module x\n\ngo 1.21\n\nreplace example.com/outside => "+outsideMod+"\n")
		}, "outside the checkout"},
		{"workspace module go.sum symlink", func(t *testing.T, co string) {
			writeFile(t, filepath.Join(co, "go.work"), "go 1.21\n\nuse ./m\n")
			writeFile(t, filepath.Join(co, "m", "go.mod"), "module x\n\ngo 1.21\n")
			mustSymlink(t, canary.path, filepath.Join(co, "m", "go.sum"))
		}, "go.sum is not a regular file"},
		{"go.mod replace target go.mod symlink to host file", func(t *testing.T, co string) {
			writeFile(t, filepath.Join(co, "go.mod"), "module x\n\ngo 1.21\n\nrequire example.com/local v0.0.0\n\nreplace example.com/local => ./local\n")
			mustSymlink(t, canary.path, filepath.Join(co, "local", "go.mod"))
		}, "local/go.mod is not a regular file"},
		{"go.work replace target go.mod symlink to host file", func(t *testing.T, co string) {
			writeFile(t, filepath.Join(co, "go.mod"), "module x\n\ngo 1.21\n")
			writeFile(t, filepath.Join(co, "go.work"), "go 1.21\n\nuse .\n\nreplace example.com/local => ./local\n")
			mustSymlink(t, canary.path, filepath.Join(co, "local", "go.mod"))
		}, "local/go.mod is not a regular file"},
		{"unparsable go.work", func(t *testing.T, co string) {
			writeFile(t, filepath.Join(co, "go.mod"), "module x\n\ngo 1.21\n")
			writeFile(t, filepath.Join(co, "go.work"), "use (\n")
		}, "does not parse"},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			co := t.TempDir()
			row.plant(t, co)
			vc, err := NewVisibleCaches()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = vc.Remove() })
			run := func(_ context.Context, dir string, _ []string, name string, args ...string) ([]byte, error) {
				t.Errorf("go must not run against a refused checkout; ran %s %q in %s", name, args, dir)
				return nil, errors.New("must not run")
			}
			_, err = SeedModCache(context.Background(), run, co, t.TempDir(), vc, os.Environ(), time.Minute)
			if !errors.Is(err, ErrSeedCheckout) || !strings.Contains(err.Error(), row.want) {
				t.Fatalf("err = %v; want ErrSeedCheckout containing %q", err, row.want)
			}
			canary.assertUntouched(t, row.name)
			if entries, _ := os.ReadDir(vc.GoModCache); len(entries) != 0 {
				t.Fatalf("destination gained %d entries", len(entries))
			}
		})
	}
}

func mustSymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

// TestSeedModCache_LiveGoSumSymlinkNeverWrittenThrough runs the REAL go
// against the fixture checkout whose go.sum is a symlink to the host canary:
// with the guard the seed refuses and the canary is untouched. (Without the
// guard `go mod download all` under -mod=mod rewrites go.sum THROUGH the
// link — the write the guard exists to stop.)
func TestSeedModCache_LiveGoSumSymlinkNeverWrittenThrough(t *testing.T) {
	fx := newSeedFixture(t)
	canary := newSeedCanary(t)
	sum := filepath.Join(fx.checkout, "go.sum")
	if err := os.Remove(sum); err != nil {
		t.Fatal(err)
	}
	mustSymlink(t, canary.path, sum)
	vc, err := NewVisibleCaches()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = vc.Remove() })
	_, err = SeedModCache(context.Background(), nil, fx.checkout, fx.hostMod, vc, os.Environ(), time.Minute)
	if !errors.Is(err, ErrSeedCheckout) {
		t.Fatalf("err = %v, want ErrSeedCheckout", err)
	}
	canary.assertUntouched(t, "live seed")
}

// TestSeedModCache_AcceptsInsideMetadata: a workspace whose `use` and
// directory `replace` targets stay inside the checkout — including through
// an INTERNAL symlink — is accepted and GOWORK is bound to the checkout's
// own go.work; a plain module binds GOWORK=off.
func TestSeedModCache_AcceptsInsideMetadata(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not on PATH")
	}
	co, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(co, "go.mod"), "module x\n\ngo 1.21\n\nrequire example.com/local v0.0.0\n\nreplace example.com/local => ./local\n")
	writeFile(t, filepath.Join(co, "local", "go.mod"), "module example.com/local\n\ngo 1.21\n")
	writeFile(t, filepath.Join(co, "m", "go.mod"), "module m\n\ngo 1.21\n")
	mustSymlink(t, filepath.Join(co, "m"), filepath.Join(co, "mlink"))
	writeFile(t, filepath.Join(co, "go.work"), "go 1.21\n\nuse (\n\t.\n\t./mlink\n)\n\nreplace example.com/local => ./local\n")
	var seen []string
	var seenDir string
	run := func(_ context.Context, dir string, env []string, _ string, args ...string) ([]byte, error) {
		if args[0] == "mod" {
			seen, seenDir = env, dir
		}
		return nil, nil
	}
	vc, err := NewVisibleCaches()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = vc.Remove() })
	if _, err := SeedModCache(context.Background(), run, co, "/host/mod", vc, nil, time.Minute); err != nil {
		t.Fatal(err)
	}
	if seenDir != co || !containsEnv(seen, "GOWORK="+filepath.Join(co, "go.work")) {
		t.Fatalf("dir = %q env = %q; want the checkout and GOWORK bound to its go.work", seenDir, seen)
	}
	plain := t.TempDir()
	writeFile(t, filepath.Join(plain, "go.mod"), "module x\n\ngo 1.21\n")
	vc2, err := NewVisibleCaches()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = vc2.Remove() })
	if _, err := SeedModCache(context.Background(), run, plain, "/host/mod", vc2, nil, time.Minute); err != nil {
		t.Fatal(err)
	}
	if !containsEnv(seen, "GOWORK=off") {
		t.Fatalf("plain module env = %q; want GOWORK=off", seen)
	}
}

// TestSeedModCache_ReplaceTargetDirectoryBoundary pins the replace-target
// boundary of the seed guard (#3448 item 5). The guard's job is to refuse
// reads through symlinks and outside the checkout root — NOT to police
// go.mod presence, which is go's own concern and surfaces as go's own
// error. So a directory `replace` target that EXISTS inside the checkout
// but carries no go.mod is ACCEPTED: `go mod download all` runs (a
// download failure then surfaces as the named download error, never as
// ErrSeedCheckout), in both the go.mod and the go.work replace form. A
// target that does NOT EXIST is unresolvable, so it is REFUSED with
// ErrSeedCheckout naming `unresolvable`, before any go process runs and
// with the destination left empty. Making seedResolveInside tolerate an
// unresolvable path turns the refuse rows red (go runs, no ErrSeedCheckout).
func TestSeedModCache_ReplaceTargetDirectoryBoundary(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not on PATH")
	}
	const goModReplace = "module x\n\ngo 1.21\n\nrequire example.com/local v0.0.0\n\nreplace example.com/local => %s\n"
	const goWorkReplace = "go 1.21\n\nuse .\n\nreplace example.com/local => %s\n"
	forms := []struct {
		name  string
		plant func(t *testing.T, co, target string)
	}{
		{"go.mod replace", func(t *testing.T, co, target string) {
			writeFile(t, filepath.Join(co, "go.mod"), fmt.Sprintf(goModReplace, target))
		}},
		{"go.work replace", func(t *testing.T, co, target string) {
			writeFile(t, filepath.Join(co, "go.mod"), "module x\n\ngo 1.21\n")
			writeFile(t, filepath.Join(co, "go.work"), fmt.Sprintf(goWorkReplace, target))
		}},
	}
	newDest := func(t *testing.T) *VisibleCaches {
		t.Helper()
		vc, err := NewVisibleCaches()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = vc.Remove() })
		return vc
	}
	for _, form := range forms {
		t.Run(form.name, func(t *testing.T) {
			t.Run("existing target dir without go.mod is accepted", func(t *testing.T) {
				co := t.TempDir()
				form.plant(t, co, "./local")
				if err := os.Mkdir(filepath.Join(co, "local"), 0o755); err != nil {
					t.Fatal(err)
				}
				var seen [][]string
				run := func(_ context.Context, _ string, _ []string, _ string, args ...string) ([]byte, error) {
					seen = append(seen, args)
					return nil, nil
				}
				if _, err := SeedModCache(context.Background(), run, co, "/host/mod", newDest(t), nil, time.Minute); err != nil {
					t.Fatalf("err = %v; want the guard to accept an existing go.mod-less replace target", err)
				}
				var sawDownload bool
				for _, args := range seen {
					if strings.Join(args, " ") == "mod download all" {
						sawDownload = true
					}
				}
				if !sawDownload {
					t.Fatalf("go invocations = %q; want `go mod download all` to run", seen)
				}
				// A download failure against that tree is go's own verdict,
				// named as the download error — never re-attributed to the
				// guard.
				failing := func(context.Context, string, []string, string, ...string) ([]byte, error) {
					return []byte("no go.mod in replacement"), errors.New("exit status 1")
				}
				dest := newDest(t)
				_, err := SeedModCache(context.Background(), failing, co, "/host/mod", dest, nil, time.Minute)
				if err == nil || errors.Is(err, ErrSeedCheckout) {
					t.Fatalf("err = %v; want a download failure that is NOT ErrSeedCheckout", err)
				}
				if !strings.Contains(err.Error(), "go mod download into "+dest.GoModCache) || !strings.Contains(err.Error(), "no go.mod in replacement") {
					t.Fatalf("err = %v; want the named download failure with go's output", err)
				}
			})
			t.Run("nonexistent target dir is refused", func(t *testing.T) {
				co := t.TempDir()
				form.plant(t, co, "./missing")
				run := func(_ context.Context, dir string, _ []string, name string, args ...string) ([]byte, error) {
					t.Errorf("go must not run against a refused checkout; ran %s %q in %s", name, args, dir)
					return nil, errors.New("must not run")
				}
				vc := newDest(t)
				_, err := SeedModCache(context.Background(), run, co, "/host/mod", vc, nil, time.Minute)
				if !errors.Is(err, ErrSeedCheckout) || !strings.Contains(err.Error(), "unresolvable") {
					t.Fatalf("err = %v; want ErrSeedCheckout containing %q", err, "unresolvable")
				}
				if entries, _ := os.ReadDir(vc.GoModCache); len(entries) != 0 {
					t.Fatalf("destination gained %d entries", len(entries))
				}
			})
		})
	}
}

// TestSeedModCache_EnvIsBaseEnvPlusPinsNeverProcessEnv: the download and the
// host-cache probe run under the CALLER's env plus the pins — a runner
// credential present only in the process environment never reaches either
// go invocation, GOTOOLCHAIN=local / git-config pins are present on both,
// the probe runs OUTSIDE the checkout with GOWORK=off, and the download's
// GOCACHE is the throwaway seed dir, not the container-visible one. A nil
// base env yields the pins alone.
func TestSeedModCache_EnvIsBaseEnvPlusPinsNeverProcessEnv(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not on PATH")
	}
	secret := "seed-canary-" + strconv.Itoa(os.Getpid())
	for _, k := range []string{"FISHHAWK_GITHUB_TOKEN", "ANTHROPIC_API_KEY", "FISHHAWK_API_TOKEN", "GITHUB_TOKEN"} {
		t.Setenv(k, secret)
	}
	t.Setenv("GOTOOLCHAIN", "go1.99.0")
	t.Setenv("GOPROXY", "https://process-env.invalid")
	checkout := t.TempDir()
	writeFile(t, filepath.Join(checkout, "go.mod"), "module x\n\ngo 1.21\n")
	base := []string{"PATH=" + os.Getenv("PATH"), "HOME=/home/r", "GOPROXY=https://base.invalid,direct",
		"GOTOOLCHAIN=auto", "GOWORK=/elsewhere/go.work", "GOCACHE=/host/gocache", "GOMODCACHE=/host/gomodcache", "GOFLAGS=-mod=vendor",
		"GIT_CONFIG_GLOBAL=/home/r/.gitconfig", "GOPRIVATE=example.internal"}
	type call struct {
		dir  string
		env  []string
		args []string
	}
	var calls []call
	run := func(_ context.Context, dir string, env []string, _ string, args ...string) ([]byte, error) {
		calls = append(calls, call{dir, env, args})
		if args[0] == "env" {
			return []byte("/host/gomodcache\n"), nil
		}
		return nil, nil
	}
	vc, err := NewVisibleCaches()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = vc.Remove() })
	rep, err := SeedModCache(context.Background(), run, checkout, "", vc, base, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 {
		t.Fatalf("calls = %d, want probe + download", len(calls))
	}
	probe, dl := calls[0], calls[1]
	if probe.args[0] != "env" || probe.dir == checkout || probe.dir != vc.Root {
		t.Errorf("probe ran %q in %q; want `go env` in the cache root %q, never the checkout", probe.args, probe.dir, vc.Root)
	}
	for _, kv := range []string{"GOTOOLCHAIN=local", "GOWORK=off", "HOME=/home/r"} {
		if !containsEnv(probe.env, kv) {
			t.Errorf("probe env lacks %q: %q", kv, probe.env)
		}
	}
	resolvedCheckout, _ := filepath.EvalSymlinks(checkout)
	if dl.dir != resolvedCheckout || strings.Join(dl.args, " ") != "mod download all" {
		t.Errorf("download ran %q in %q, want in %q", dl.args, dl.dir, resolvedCheckout)
	}
	if rep.Proxy != "file:///host/gomodcache/cache/download,https://base.invalid,direct" {
		t.Errorf("Proxy = %q; want the BASE env's GOPROXY as fallback, not the process env's", rep.Proxy)
	}
	for _, kv := range []string{"GOTOOLCHAIN=local", "GOWORK=off", "GOCACHE=" + filepath.Join(vc.Root, "seed-gocache"),
		"GOMODCACHE=" + vc.GoModCache, "GOFLAGS=-mod=mod -modcacherw", "GOPROXY=" + rep.Proxy,
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_TERMINAL_PROMPT=0", "HOME=/home/r", "GOPRIVATE=example.internal"} {
		if !containsEnv(dl.env, kv) {
			t.Errorf("download env lacks %q: %q", kv, dl.env)
		}
	}
	for _, env := range [][]string{probe.env, dl.env} {
		for _, kv := range env {
			k, v, _ := strings.Cut(kv, "=")
			switch {
			case v == secret, strings.HasSuffix(k, "_TOKEN"), k == "ANTHROPIC_API_KEY":
				t.Errorf("process-env credential reached a go invocation: %s", k)
			case k == "GOTOOLCHAIN" && v != "local", k == "GOWORK" && v == "/elsewhere/go.work":
				t.Errorf("base value survived a pin: %s", kv)
			}
		}
	}
	for _, kv := range dl.env {
		k, v, _ := strings.Cut(kv, "=")
		switch {
		case k == "GOCACHE" && v == "/host/gocache", k == "GOMODCACHE" && v == "/host/gomodcache", k == "GOFLAGS" && v == "-mod=vendor",
			k == "GIT_CONFIG_GLOBAL" && v != "/dev/null", k == "GOPROXY" && !strings.HasPrefix(v, "file://"):
			t.Errorf("base value survived a download pin: %s", kv)
		}
	}
	if _, err := os.Stat(filepath.Join(vc.Root, "seed-gocache")); err != nil {
		t.Errorf("seed GOCACHE not created under the cache root: %v", err)
	}
	if entries, _ := os.ReadDir(vc.GoCache); len(entries) != 0 {
		t.Errorf("container-visible GOCACHE gained %d entries", len(entries))
	}

	calls = nil
	vc2, err := NewVisibleCaches()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = vc2.Remove() })
	if _, err := SeedModCache(context.Background(), run, checkout, "/host/mod", vc2, nil, time.Minute); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || len(calls[0].env) != 10 {
		t.Errorf("nil base env: want exactly the 10 pins, got %q", calls[0].env)
	}
}
