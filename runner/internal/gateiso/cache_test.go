package gateiso

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
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
	rep, err := SeedModCache(context.Background(), run, fx.checkout, fx.hostMod, vc, time.Minute)
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
	if _, err := SeedModCache(context.Background(), nil, fx.checkout, fx.hostMod, a, time.Minute); err != nil {
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
	_, err = SeedModCache(context.Background(), nil, fx.checkout, fx.hostMod, a, time.Minute)
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
	if _, err := SeedModCache(context.Background(), nil, fx.checkout, fx.hostMod, b, time.Minute); err != nil {
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
	if _, err := SeedModCache(context.Background(), nil, fx.checkout, fx.hostMod, c, time.Minute); !errors.Is(err, ErrDestinationNotEmpty) {
		t.Fatalf("symlinked destination: err = %v, want ErrDestinationNotEmpty", err)
	}
	d := &VisibleCaches{GoModCache: filepath.Join(t.TempDir(), "missing")}
	if _, err := SeedModCache(context.Background(), nil, fx.checkout, fx.hostMod, d, time.Minute); !errors.Is(err, ErrDestinationNotEmpty) {
		t.Fatalf("missing destination: err = %v, want ErrDestinationNotEmpty", err)
	}
	if _, err := SeedModCache(context.Background(), nil, fx.checkout, fx.hostMod, nil, time.Minute); err == nil {
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
	rep, err := SeedModCache(context.Background(), run, t.TempDir(), t.TempDir(), vc, time.Minute)
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
	rep, err := SeedModCache(context.Background(), run, checkout, t.TempDir(), vc, time.Minute)
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
	rep, err := SeedModCache(context.Background(), run, checkout, t.TempDir(), vc, time.Minute)
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
	rep, err := SeedModCache(context.Background(), run, checkout, "", vc, time.Minute)
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
	_, err = SeedModCache(context.Background(), run, checkout, "", vc, time.Minute)
	if err == nil || !strings.Contains(err.Error(), "resolve host GOMODCACHE") || !strings.Contains(err.Error(), "cannot find GOROOT") {
		t.Fatalf("err = %v", err)
	}
}
