package main

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

// sliceReachWriteFixture writes a NON-Fishhawk workspace into a fresh temp
// dir. A value "symlink:<target>" makes a symlink (target as written), and
// "fifo:" a writer-less FIFO; anything else is file content. Each FIFO is
// released at cleanup so a reader blocked on its open does not leak.
func sliceReachWriteFixture(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	sliceReachWriteFiles(t, root, files)
	return root
}

func sliceReachWriteFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		switch {
		case strings.HasPrefix(content, "symlink:"):
			if err := os.Symlink(strings.TrimPrefix(content, "symlink:"), p); err != nil {
				t.Fatal(err)
			}
		case content == "fifo:":
			if err := syscall.Mkfifo(p, 0o644); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				// Release a reader blocked on open (no reader: ENXIO, ignored).
				if fd, err := syscall.Open(p, syscall.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
					_ = syscall.Close(fd)
				}
			})
		default:
			if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
}

const sliceReachGoWorkM1M2 = "go 1.22\n\nuse (\n\t./m1\n\t./m2\n)\n"

func sliceReachMod(path string) string { return "module " + path + "\n\ngo 1.22\n" }

// sliceReachBaseFiles is the base fixture: in m1, b imports a, d imports b, c
// is independent, and e imports a ONLY from e_test.go (package e); in m2, f
// imports example.com/m1/a.
func sliceReachBaseFiles() map[string]string {
	return map[string]string{
		"go.work":        sliceReachGoWorkM1M2,
		"m1/go.mod":      sliceReachMod("example.com/m1"),
		"m1/a/a.go":      "package a\n",
		"m1/b/b.go":      "package b\n\nimport _ \"example.com/m1/a\"\n",
		"m1/d/d.go":      "package d\n\nimport _ \"example.com/m1/b\"\n",
		"m1/c/c.go":      "package c\n",
		"m1/e/e.go":      "package e\n",
		"m1/e/e_test.go": "package e\n\nimport _ \"example.com/m1/a\"\n",
		"m2/go.mod":      sliceReachMod("example.com/m2"),
		"m2/f/f.go":      "package f\n\nimport _ \"example.com/m1/a\"\n",
	}
}

type sliceReachResult struct {
	imports   map[string][]string
	emittable map[string]bool
	reason    string
	elapsed   time.Duration
}

// sliceReachRun runs the loader under a 10s watchdog, so a hang fails the
// test instead of reaching the package timeout.
func sliceReachRun(t *testing.T, root string) sliceReachResult {
	t.Helper()
	sliceReachQuietLog(t)
	done := make(chan sliceReachResult, 1)
	start := time.Now()
	go func() {
		imp, em, reason := sliceReachLoad(context.Background(), root)
		done <- sliceReachResult{imp, em, reason, time.Since(start)}
	}()
	select {
	case r := <-done:
		return r
	case <-time.After(10 * time.Second):
		t.Fatal("sliceReachLoad did not return within the 10s watchdog")
		return sliceReachResult{}
	}
}

func sliceReachQuietLog(t *testing.T) {
	t.Helper()
	prev := sliceReachLogSink
	sliceReachLogSink = io.Discard
	t.Cleanup(func() { sliceReachLogSink = prev })
}

func sliceReachSetTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	prev := sliceReachLoadTimeout
	sliceReachLoadTimeout = d
	t.Cleanup(func() { sliceReachLoadTimeout = prev })
}

// sliceReachRecordingGo installs a sliceReachGoBin wrapper that logs each
// invocation and execs the real go binary; the returned func counts calls.
func sliceReachRecordingGo(t *testing.T) func() int {
	t.Helper()
	real, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go toolchain on PATH")
	}
	dir := t.TempDir()
	log := filepath.Join(dir, "calls.log")
	script := "#!/bin/sh\necho \"$@\" >> '" + log + "'\nexec '" + real + "' \"$@\"\n"
	sliceReachSetGoBin(t, sliceReachWriteScript(t, dir, "go", script))
	return func() int {
		data, err := os.ReadFile(log)
		if errors.Is(err, os.ErrNotExist) {
			return 0
		}
		return strings.Count(string(data), "\n")
	}
}

func sliceReachSetGoBin(t *testing.T, bin string) {
	t.Helper()
	prev := sliceReachGoBin
	sliceReachGoBin = bin
	t.Cleanup(func() { sliceReachGoBin = prev })
}

func sliceReachWriteScript(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func sliceReachWantReason(t *testing.T, got sliceReachResult, want string) {
	t.Helper()
	if got.reason != want {
		t.Fatalf("reason = %q, want %q", got.reason, want)
	}
}

// (9) TestSliceReach_GraphEdges.
func TestSliceReach_GraphEdges(t *testing.T) {
	root := sliceReachWriteFixture(t, sliceReachBaseFiles())
	got := sliceReachRun(t, root)
	sliceReachWantReason(t, got, "")
	want := map[string][]string{
		"m1/a": {}, "m1/b": {"m1/a"}, "m1/c": {}, "m1/d": {"m1/b"},
		"m1/e": {"m1/a"}, "m2/f": {"m1/a"},
	}
	for dir, edges := range want {
		have, ok := got.imports[dir]
		if !ok {
			t.Fatalf("imports lacks node %s: %v", dir, got.imports)
		}
		if len(have) == 0 && len(edges) == 0 {
			continue
		}
		if !reflect.DeepEqual(have, edges) {
			t.Errorf("imports[%s] = %v, want %v", dir, have, edges)
		}
	}
	if len(got.imports) != len(want) {
		t.Errorf("imports = %v, want exactly the six nodes", got.imports)
	}
	for dir := range want {
		if !got.emittable[dir] {
			t.Errorf("emittable lacks %s: %v", dir, got.emittable)
		}
	}
}

// (10) TestSliceReach_TagExcludedTestImportJoinsReach.
func TestSliceReach_TagExcludedTestImportJoinsReach(t *testing.T) {
	root := sliceReachWriteFixture(t, map[string]string{
		"go.work":               "go 1.22\n\nuse ./m3\n",
		"m3/go.mod":             sliceReachMod("example.com/m3"),
		"m3/a/a.go":             "package a\n",
		"m3/h/h.go":             "package h\n",
		"m3/h/h_tagged_test.go": "//go:build fishhawk_never\n\npackage h_test\n\nimport _ \"example.com/m3/a\"\n",
	})
	got := sliceReachRun(t, root)
	sliceReachWantReason(t, got, "")
	if !reflect.DeepEqual(got.imports["m3/h"], []string{"m3/a"}) {
		t.Fatalf("imports[m3/h] = %v, want [m3/a] (the tag-excluded test import)", got.imports["m3/h"])
	}
}

// (11) TestSliceReach_HostExcludedDirIsTraversalOnly.
func TestSliceReach_HostExcludedDirIsTraversalOnly(t *testing.T) {
	root := sliceReachWriteFixture(t, map[string]string{
		"go.work":   "go 1.22\n\nuse ./m3\n",
		"m3/go.mod": sliceReachMod("example.com/m3"),
		"m3/a/a.go": "package a\n",
		"m3/w/w.go": "//go:build fishhawk_never\n\npackage w\n\nimport _ \"example.com/m3/a\"\n",
	})
	got := sliceReachRun(t, root)
	sliceReachWantReason(t, got, "")
	if !reflect.DeepEqual(got.imports["m3/w"], []string{"m3/a"}) {
		t.Fatalf("imports[m3/w] = %v, want [m3/a]", got.imports["m3/w"])
	}
	if got.emittable["m3/w"] {
		t.Fatalf("emittable carries the host-excluded m3/w: %v", got.emittable)
	}
	if !got.emittable["m3/a"] {
		t.Fatalf("emittable lacks m3/a: %v", got.emittable)
	}
}

// (12) TestSliceReach_ErroredPackageIsFull.
func TestSliceReach_ErroredPackageIsFull(t *testing.T) {
	files := sliceReachBaseFiles()
	files["m1/g/g.go"] = "package g\n\nimport _ \"example.org/absent/x\"\n"
	got := sliceReachRun(t, sliceReachWriteFixture(t, files))
	sliceReachWantReason(t, got, "reach_load_failed:package_error:example.com/m1/g")
}

// (13) TestSliceReach_UnparsableFileIsFull.
func TestSliceReach_UnparsableFileIsFull(t *testing.T) {
	files := sliceReachBaseFiles()
	files["m1/z/z.go"] = "package z\n\nimport (\n\t\"fmt\n"
	got := sliceReachRun(t, sliceReachWriteFixture(t, files))
	sliceReachWantReason(t, got, "reach_load_failed:unparsable_go_file:m1/z/z.go")
}

// (14) S9 TestSliceReach_SymlinkGoFileIsFull.
func TestSliceReach_SymlinkGoFileIsFull(t *testing.T) {
	files := sliceReachBaseFiles()
	files["m1/a/real.go.txt"] = "package a\n"
	files["m1/a/link.go"] = "symlink:real.go.txt"
	got := sliceReachRun(t, sliceReachWriteFixture(t, files))
	sliceReachWantReason(t, got, "reach_load_failed:unparsable_go_file:m1/a/link.go")
}

// (14) TestSliceReach_NonRegularGoFileIsFull: behavioural (without the guard
// the read would block), under the 10s watchdog.
func TestSliceReach_NonRegularGoFileIsFull(t *testing.T) {
	files := sliceReachBaseFiles()
	files["m1/a/pipe.go"] = "fifo:"
	got := sliceReachRun(t, sliceReachWriteFixture(t, files))
	sliceReachWantReason(t, got, "reach_load_failed:unparsable_go_file:m1/a/pipe.go")
}

// (15) TestSliceReach_ImportOfWorkspaceNonPackageIsFull.
func TestSliceReach_ImportOfWorkspaceNonPackageIsFull(t *testing.T) {
	files := sliceReachBaseFiles()
	files["m1/a/a.go"] = "package a\n\nimport _ \"example.com/m1/nosuch\"\n"
	got := sliceReachRun(t, sliceReachWriteFixture(t, files))
	sliceReachWantReason(t, got, "reach_load_failed:import_not_package:example.com/m1/nosuch")
}

// sliceReachEnvFile parses an `env` dump into its last-wins values.
func sliceReachEnvFile(t *testing.T, p string) map[string]string {
	t.Helper()
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	out := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if k, v, ok := strings.Cut(sc.Text(), "="); ok {
			out[k] = v
		}
	}
	return out
}

// (16) TestSliceReach_EnvPinned, extended by approval condition Q1: every
// inherited value below is overridden last-wins in the go list env.
func TestSliceReach_EnvPinned(t *testing.T) {
	root := sliceReachWriteFixture(t, sliceReachBaseFiles())
	dir := t.TempDir()
	envOut := filepath.Join(dir, "env.out")
	sliceReachSetGoBin(t, sliceReachWriteScript(t, dir, "go", "#!/bin/sh\nenv > '"+envOut+"'\nexit 1\n"))
	for k, v := range map[string]string{
		"CGO_ENABLED":  "1",
		"GOFLAGS":      "-tags=x",
		"GOPROXY":      "direct",
		"GOTOOLCHAIN":  "auto",
		"GONOPROXY":    "example.com/*",
		"GOPRIVATE":    "example.com/*",
		"GONOSUMDB":    "example.com/*",
		"GONOSUMCHECK": "1",
		"GOINSECURE":   "example.com/*",
		"GOENV":        filepath.Join(dir, "goenv"),
		"GOCACHEPROG":  "/bin/false",
	} {
		t.Setenv(k, v)
	}
	got := sliceReachRun(t, root)
	sliceReachWantReason(t, got, "reach_load_failed:go_list_exit")
	env := sliceReachEnvFile(t, envOut)
	want := map[string]string{
		"CGO_ENABLED":  "0",
		"GOFLAGS":      "",
		"GOPROXY":      "off",
		"GOTOOLCHAIN":  "local",
		"GOWORK":       filepath.Join(sliceReachRealRoot(t, root), "go.work"),
		"GONOPROXY":    "",
		"GOPRIVATE":    "",
		"GONOSUMDB":    "",
		"GONOSUMCHECK": "",
		"GOINSECURE":   "",
		"GOENV":        "off",
		"GOCACHEPROG":  "",
	}
	for k, v := range want {
		have, ok := env[k]
		if !ok || have != v {
			t.Errorf("go list env %s = %q (present %v), want %q", k, have, ok, v)
		}
	}
}

func sliceReachRealRoot(t *testing.T, root string) string {
	t.Helper()
	real, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	return real
}

// Q1 TestSliceReach_PrivateModuleMakesNoNetworkAttempt: GOPRIVATE matches an
// uncached required module, the parent GOPROXY is unroutable and `git` is a
// stub that records every call. The load widens with a fixed token and the
// stub is never run: no direct (VCS) fetch is attempted.
func TestSliceReach_PrivateModuleMakesNoNetworkAttempt(t *testing.T) {
	root := sliceReachWriteFixture(t, map[string]string{
		"go.mod": "module example.com/m1\n\ngo 1.22\n\nrequire example.com/x.git v1.0.0\n",
		// go.sum entries make go attempt the download (without them it
		// refuses before any fetch, which would hide the direct-fetch path).
		"go.sum": "example.com/x.git v1.0.0 h1:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=\n" +
			"example.com/x.git v1.0.0/go.mod h1:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=\n",
		"a/a.go": "package a\n\nimport _ \"example.com/x.git/y\"\n",
		"b/b.go": "package b\n",
	})
	stub := t.TempDir()
	calls := filepath.Join(stub, "git.calls")
	sliceReachWriteScript(t, stub, "git", "#!/bin/sh\necho \"$@\" >> '"+calls+"'\nexit 128\n")
	real, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go toolchain on PATH")
	}
	sliceReachSetGoBin(t, real)
	t.Setenv("PATH", stub+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GOPRIVATE", "example.com/*")
	t.Setenv("GONOPROXY", "example.com/*")
	t.Setenv("GOPROXY", "http://127.0.0.1:1/")
	t.Setenv("GOMODCACHE", t.TempDir())
	t.Setenv("GOFLAGS", "-modcacherw")
	got := sliceReachRun(t, root)
	if data, err := os.ReadFile(calls); err == nil {
		t.Fatalf("the reach load ran git (a direct fetch attempt): %q", data)
	}
	sliceReachWantReason(t, got, "reach_load_failed:package_error:example.com/m1/a")
}

// (16) TestSliceReach_NonZeroExitIsFull.
func TestSliceReach_NonZeroExitIsFull(t *testing.T) {
	root := sliceReachWriteFixture(t, sliceReachBaseFiles())
	dir := t.TempDir()
	rec := `{"ImportPath":"example.com/m1/a","Dir":"` + filepath.Join(sliceReachRealRoot(t, root), "m1", "a") +
		`","Module":{"Path":"example.com/m1","Dir":"` + filepath.Join(sliceReachRealRoot(t, root), "m1") + `"}}`
	sliceReachSetGoBin(t, sliceReachWriteScript(t, dir, "go", "#!/bin/sh\necho '"+rec+"'\nexit 2\n"))
	sliceReachWantReason(t, sliceReachRun(t, root), "reach_load_failed:go_list_exit")
}

// (16) TestSliceReach_MalformedJSONIsFull.
func TestSliceReach_MalformedJSONIsFull(t *testing.T) {
	root := sliceReachWriteFixture(t, sliceReachBaseFiles())
	dir := t.TempDir()
	rec := `{"ImportPath":"example.com/m1/a","Dir":"` + filepath.Join(sliceReachRealRoot(t, root), "m1", "a") +
		`","Module":{"Path":"example.com/m1","Dir":"` + filepath.Join(sliceReachRealRoot(t, root), "m1") + `"}}`
	sliceReachSetGoBin(t, sliceReachWriteScript(t, dir, "go", "#!/bin/sh\necho '"+rec+"'\necho '{garbage'\nexit 0\n"))
	sliceReachWantReason(t, sliceReachRun(t, root), "reach_load_failed:go_list_malformed_json")
}

// (16) TestSliceReach_StdoutCapIsFull.
func TestSliceReach_StdoutCapIsFull(t *testing.T) {
	root := sliceReachWriteFixture(t, sliceReachBaseFiles())
	dir := t.TempDir()
	rec := `{"ImportPath":"example.com/m1/a","Dir":"` + filepath.Join(sliceReachRealRoot(t, root), "m1", "a") +
		`","Module":{"Path":"example.com/m1","Dir":"` + filepath.Join(sliceReachRealRoot(t, root), "m1") + `"}}`
	sliceReachSetGoBin(t, sliceReachWriteScript(t, dir, "go", "#!/bin/sh\nfor i in 1 2 3 4 5 6 7 8; do echo '"+rec+"'; done\nexit 0\n"))
	prev := sliceReachStdoutCap
	sliceReachStdoutCap = int64(len(rec) + 10)
	t.Cleanup(func() { sliceReachStdoutCap = prev })
	sliceReachWantReason(t, sliceReachRun(t, root), "reach_load_failed:go_list_stdout_cap")
}

// (16) TestSliceReach_BrokenGoModIsFull.
func TestSliceReach_BrokenGoModIsFull(t *testing.T) {
	files := sliceReachBaseFiles()
	files["m1/go.mod"] = "module example.com/m1\n\ngo 1.22\n\nrequire (\n"
	got := sliceReachRun(t, sliceReachWriteFixture(t, files))
	sliceReachWantReason(t, got, "reach_load_failed:gomod_unreadable:m1/go.mod")
}

// (17) S4 TestSliceReach_LoadTimeout.
func TestSliceReach_LoadTimeout(t *testing.T) {
	root := sliceReachWriteFixture(t, sliceReachBaseFiles())
	dir := t.TempDir()
	sliceReachSetGoBin(t, sliceReachWriteScript(t, dir, "go", "#!/bin/sh\nsleep 60\n"))
	sliceReachSetTimeout(t, 200*time.Millisecond)
	got := sliceReachRun(t, root)
	sliceReachWantReason(t, got, "reach_load_failed:load_timeout")
	if got.elapsed > 5*time.Second {
		t.Fatalf("load took %s, want < 5s", got.elapsed)
	}
}

// (18) R4 TestSliceReach_UseEscapes.
func TestSliceReach_UseEscapes(t *testing.T) {
	outside := sliceReachWriteFixture(t, map[string]string{
		"go.mod": sliceReachMod("example.com/out"),
		"o/o.go": "package o\n",
	})
	for name, use := range map[string]string{
		"absolute":       outside,
		"dotdot":         "../outside",
		"symlinkOutside": "./m9",
	} {
		t.Run(name, func(t *testing.T) {
			files := sliceReachBaseFiles()
			files["go.work"] = "go 1.22\n\nuse (\n\t./m1\n\t" + use + "\n)\n"
			files["m9"] = "symlink:" + outside
			parent := t.TempDir()
			root := filepath.Join(parent, "root")
			sliceReachWriteFiles(t, root, files)
			sliceReachWriteFiles(t, parent, map[string]string{
				"outside/go.mod": sliceReachMod("example.com/out2"),
				"outside/p/p.go": "package p\n",
			})
			calls := sliceReachRecordingGo(t)
			sliceReachWantReason(t, sliceReachRun(t, root), "reach_unsupported_workspace")
			if n := calls(); n != 0 {
				t.Fatalf("go list ran %d time(s), want 0", n)
			}
		})
	}
}

// (18) R4 TestSliceReach_ReplaceEscapes.
func TestSliceReach_ReplaceEscapes(t *testing.T) {
	rows := map[string]func(root, outside string) map[string]string{
		"gomod": func(root, outside string) map[string]string {
			return map[string]string{
				"m1/go.mod": "module example.com/m1\n\ngo 1.22\n\nrequire example.com/x v0.0.0\n\nreplace example.com/x => ../../outside\n",
			}
		},
		"gowork": func(root, outside string) map[string]string {
			return map[string]string{
				"m1/go.mod": "module example.com/m1\n\ngo 1.22\n\nrequire example.com/x v0.0.0\n",
				"go.work":   sliceReachGoWorkM1M2 + "\nreplace example.com/x => ../outside\n",
			}
		},
	}
	for name, mk := range rows {
		t.Run(name, func(t *testing.T) {
			parent := t.TempDir()
			root := filepath.Join(parent, "root")
			outside := filepath.Join(parent, "outside")
			sliceReachWriteFiles(t, parent, map[string]string{
				"outside/go.mod": sliceReachMod("example.com/x"),
				"outside/x.go":   "package x\n",
			})
			files := sliceReachBaseFiles()
			files["m1/a/a.go"] = "package a\n\nimport _ \"example.com/x\"\n"
			for k, v := range mk(root, outside) {
				files[k] = v
			}
			sliceReachWriteFiles(t, root, files)
			calls := sliceReachRecordingGo(t)
			sliceReachWantReason(t, sliceReachRun(t, root), "reach_unsupported_workspace")
			if n := calls(); n != 0 {
				t.Fatalf("go list ran %d time(s), want 0", n)
			}
		})
	}
}

// (19) Pure sliceReachAssemble rows.
func TestSliceReachAssemble(t *testing.T) {
	root := "/clone"
	mod := &struct{ Path, Dir string }{Path: "example.com/m1", Dir: "/clone/m1"}
	walked := map[string][]string{"m1/a": nil, "m1/b": {"m1/a"}}
	rec := func(ip, dir string) sliceReachRecord {
		return sliceReachRecord{ImportPath: ip, Dir: dir, Module: mod}
	}
	cases := []struct {
		name    string
		records []sliceReachRecord
		want    string
	}{
		{"outside", []sliceReachRecord{rec("example.com/m1/a", "/clone/m1/a"), rec("example.com/evil", "/elsewhere/evil")}, "reach_dir_outside_clone:example.com/evil"},
		{"unwalked", []sliceReachRecord{rec("example.com/m1/a", "/clone/m1/a"), rec("example.com/m1/q", "/clone/m1/q")}, "reach_load_failed:unwalked_package_dir:example.com/m1/q"},
		{"noModule", []sliceReachRecord{rec("example.com/m1/a", "/clone/m1/a"), {ImportPath: "example.com/m1/b", Dir: "/clone/m1/b"}}, "reach_load_failed:no_module:example.com/m1/b"},
		{"noPackages", nil, "reach_load_failed:no_packages"},
		{"errored", []sliceReachRecord{{ImportPath: "example.com/m1/a", Dir: "/clone/m1/a", Module: mod, Incomplete: true}}, "reach_load_failed:package_error:example.com/m1/a"},
		{"ok", []sliceReachRecord{rec("example.com/m1/a", "/clone/m1/a"), rec("example.com/m1/b", "/clone/m1/b")}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			imp, em, reason := sliceReachAssemble(root, c.records, walked)
			if reason != c.want {
				t.Fatalf("reason = %q, want %q", reason, c.want)
			}
			if c.want == "" && (!em["m1/a"] || !em["m1/b"] || !reflect.DeepEqual(imp["m1/b"], []string{"m1/a"})) {
				t.Fatalf("graph = %v / %v", imp, em)
			}
		})
	}
}

// T1 rows (34)-(40): each lowers the bound to 2s, asserts the exact token and,
// where refused, that go list never ran.

// (34) TestSliceReach_SymlinkedGoWorkIsFull.
func TestSliceReach_SymlinkedGoWorkIsFull(t *testing.T) {
	sliceReachSetTimeout(t, 2*time.Second)
	parent := t.TempDir()
	root := filepath.Join(parent, "root")
	files := sliceReachBaseFiles()
	delete(files, "go.work")
	files["go.work"] = "symlink:" + filepath.Join(parent, "elsewhere", "go.work")
	sliceReachWriteFiles(t, root, files)
	sliceReachWriteFiles(t, parent, map[string]string{"elsewhere/go.work": sliceReachGoWorkM1M2})
	calls := sliceReachRecordingGo(t)
	sliceReachWantReason(t, sliceReachRun(t, root), "reach_unsupported_workspace:metadata_not_regular:root")
	if n := calls(); n != 0 {
		t.Fatalf("go list ran %d time(s), want 0", n)
	}
}

// (35) TestSliceReach_FIFOGoModIsFullWithinBound.
func TestSliceReach_FIFOGoModIsFullWithinBound(t *testing.T) {
	sliceReachSetTimeout(t, 2*time.Second)
	root := sliceReachWriteFixture(t, map[string]string{"go.mod": "fifo:", "a/a.go": "package a\n"})
	calls := sliceReachRecordingGo(t)
	got := sliceReachRun(t, root)
	sliceReachWantReason(t, got, "reach_unsupported_workspace:metadata_not_regular:root")
	if got.elapsed > 3*time.Second {
		t.Fatalf("load took %s, want < 3s", got.elapsed)
	}
	if n := calls(); n != 0 {
		t.Fatalf("go list ran %d time(s), want 0", n)
	}
}

// (36) TestSliceReach_DevZeroGoModIsFullWithinBound.
func TestSliceReach_DevZeroGoModIsFullWithinBound(t *testing.T) {
	sliceReachSetTimeout(t, 2*time.Second)
	root := sliceReachWriteFixture(t, map[string]string{"go.mod": "symlink:/dev/zero", "a/a.go": "package a\n"})
	calls := sliceReachRecordingGo(t)
	got := sliceReachRun(t, root)
	sliceReachWantReason(t, got, "reach_unsupported_workspace:metadata_not_regular:root")
	if got.elapsed > 3*time.Second {
		t.Fatalf("load took %s, want < 3s", got.elapsed)
	}
	if n := calls(); n != 0 {
		t.Fatalf("go list ran %d time(s), want 0", n)
	}
}

// (37) TestSliceReach_SymlinkedUseGoModIsFull.
func TestSliceReach_SymlinkedUseGoModIsFull(t *testing.T) {
	sliceReachSetTimeout(t, 2*time.Second)
	parent := t.TempDir()
	root := filepath.Join(parent, "root")
	files := sliceReachBaseFiles()
	files["m1/go.mod"] = "symlink:" + filepath.Join(parent, "elsewhere", "go.mod")
	sliceReachWriteFiles(t, root, files)
	sliceReachWriteFiles(t, parent, map[string]string{"elsewhere/go.mod": sliceReachMod("example.com/m1")})
	calls := sliceReachRecordingGo(t)
	sliceReachWantReason(t, sliceReachRun(t, root), "reach_unsupported_workspace:metadata_not_regular:use")
	if n := calls(); n != 0 {
		t.Fatalf("go list ran %d time(s), want 0", n)
	}
}

// (38) TestSliceReach_SymlinkedReplaceTargetGoModIsFull.
func TestSliceReach_SymlinkedReplaceTargetGoModIsFull(t *testing.T) {
	rows := map[string]map[string]string{
		"gowork": {
			"go.work":   sliceReachGoWorkM1M2 + "\nreplace example.com/dep => ./dep\n",
			"m1/go.mod": "module example.com/m1\n\ngo 1.22\n\nrequire example.com/dep v0.0.0\n",
		},
		"gomod": {
			"m1/go.mod": "module example.com/m1\n\ngo 1.22\n\nrequire example.com/dep v0.0.0\n\nreplace example.com/dep => ../dep\n",
		},
	}
	for name, extra := range rows {
		t.Run(name, func(t *testing.T) {
			sliceReachSetTimeout(t, 2*time.Second)
			parent := t.TempDir()
			root := filepath.Join(parent, "root")
			files := sliceReachBaseFiles()
			files["m1/a/a.go"] = "package a\n\nimport _ \"example.com/dep\"\n"
			files["dep/dep.go"] = "package dep\n"
			files["dep/go.mod"] = "symlink:" + filepath.Join(parent, "elsewhere", "go.mod")
			for k, v := range extra {
				files[k] = v
			}
			sliceReachWriteFiles(t, root, files)
			sliceReachWriteFiles(t, parent, map[string]string{"elsewhere/go.mod": sliceReachMod("example.com/dep")})
			calls := sliceReachRecordingGo(t)
			sliceReachWantReason(t, sliceReachRun(t, root), "reach_unsupported_workspace:metadata_not_regular:replace")
			if n := calls(); n != 0 {
				t.Fatalf("go list ran %d time(s), want 0", n)
			}
		})
	}
}

// (39) TestSliceReach_MetadataReadBounded: the predicate is stubbed out, so
// only a deadline already running when the read starts releases the open.
func TestSliceReach_MetadataReadBounded(t *testing.T) {
	sliceReachSetTimeout(t, 2*time.Second)
	prev := sliceReachRequireRegularFn
	sliceReachRequireRegularFn = func(string) error { return nil }
	t.Cleanup(func() { sliceReachRequireRegularFn = prev })
	root := sliceReachWriteFixture(t, map[string]string{"go.mod": "fifo:", "a/a.go": "package a\n"})
	got := sliceReachRun(t, root)
	sliceReachWantReason(t, got, "reach_load_failed:load_timeout")
	if got.elapsed > 3*time.Second {
		t.Fatalf("load took %s, want < 3s", got.elapsed)
	}
}

// (40) TestSliceReach_OversizeGoModIsFull.
func TestSliceReach_OversizeGoModIsFull(t *testing.T) {
	sliceReachSetTimeout(t, 2*time.Second)
	prev := sliceReachMetadataCap
	sliceReachMetadataCap = 64
	t.Cleanup(func() { sliceReachMetadataCap = prev })
	mod := sliceReachMod("example.com/big") + "// " + strings.Repeat("x", 200) + "\n"
	root := sliceReachWriteFixture(t, map[string]string{"go.mod": mod, "a/a.go": "package a\n"})
	sliceReachWantReason(t, sliceReachRun(t, root), "reach_load_failed:metadata_too_large:go.mod")
}

// TestSliceReach_RootModuleOnly: a root go.mod with no go.work loads as the
// single use ".".
func TestSliceReach_RootModuleOnly(t *testing.T) {
	root := sliceReachWriteFixture(t, map[string]string{
		"go.mod": sliceReachMod("example.com/solo"),
		"a/a.go": "package a\n",
		"b/b.go": "package b\n\nimport _ \"example.com/solo/a\"\n",
	})
	got := sliceReachRun(t, root)
	sliceReachWantReason(t, got, "")
	if !reflect.DeepEqual(got.imports["b"], []string{"a"}) || !got.emittable["a"] || !got.emittable["b"] {
		t.Fatalf("graph = %v / %v", got.imports, got.emittable)
	}
}

// TestSliceReach_NoModuleIsUnsupported: neither go.work nor go.mod, and a
// go.work with zero uses, are reach_unsupported_workspace.
func TestSliceReach_NoModuleIsUnsupported(t *testing.T) {
	sliceReachWantReason(t, sliceReachRun(t, sliceReachWriteFixture(t, map[string]string{"a/a.go": "package a\n"})), "reach_unsupported_workspace")
	sliceReachWantReason(t, sliceReachRun(t, sliceReachWriteFixture(t, map[string]string{"go.work": "go 1.22\n"})), "reach_unsupported_workspace")
}
