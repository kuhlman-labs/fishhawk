package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/mod/modfile"
)

const testNonce = "n0nce-5524"

// writeFiles writes a fixture tree under root. A value "symlink:<target>"
// makes a symlink (target as written) and "fifo:" a writer-less FIFO;
// anything else is file content. Each FIFO is released at cleanup.
func writeFiles(t *testing.T, root string, files map[string]string) {
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

func writeFixture(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	writeFiles(t, root, files)
	return root
}

const goWorkM1M2 = "go 1.22\n\nuse (\n\t./m1\n\t./m2\n)\n"

func mod(p string) string { return "module " + p + "\n\ngo 1.22\n" }

// baseFiles mirrors the host loader's sliceReachBaseFiles.
func baseFiles() map[string]string {
	return map[string]string{
		"go.work":        goWorkM1M2,
		"m1/go.mod":      mod("example.com/m1"),
		"m1/a/a.go":      "package a\n",
		"m1/b/b.go":      "package b\n\nimport _ \"example.com/m1/a\"\n",
		"m1/d/d.go":      "package d\n\nimport _ \"example.com/m1/b\"\n",
		"m1/c/c.go":      "package c\n",
		"m1/e/e.go":      "package e\n",
		"m1/e/e_test.go": "package e\n\nimport _ \"example.com/m1/a\"\n",
		"m2/go.mod":      mod("example.com/m2"),
		"m2/f/f.go":      "package f\n\nimport _ \"example.com/m1/a\"\n",
	}
}

func realGo(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go toolchain on PATH")
	}
	return p
}

func setVar[T any](t *testing.T, p *T, v T) {
	t.Helper()
	prev := *p
	*p = v
	t.Cleanup(func() { *p = prev })
}

// fakeGo puts a go script first on PATH that runs listBody for `go list` and
// the real go for every other subcommand (the go.work / go.mod reads).
func fakeGo(t *testing.T, listBody string) string {
	t.Helper()
	real := realGo(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "go")
	script := "#!/bin/sh\nif [ \"$1\" = list ]; then\n" + listBody + "\nfi\nexec '" + real + "' \"$@\"\n"
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	prependPath(t, dir)
	return dir
}

// prependPath puts dir first on PATH for the test, so runGo's literal "go"
// resolves to the fake installed there.
func prependPath(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// recordingGo installs a go wrapper that logs each `go list` call and
// execs the real go; the returned func counts the list calls.
func recordingGo(t *testing.T) func() int {
	t.Helper()
	log := filepath.Join(t.TempDir(), "calls.log")
	fakeGo(t, "echo \"$@\" >> '"+log+"'")
	return func() int {
		data, err := os.ReadFile(log)
		if err != nil {
			return 0
		}
		return strings.Count(string(data), "\n")
	}
}

type helperResult struct {
	result
	stderr  string
	elapsed time.Duration
}

// runHelper drives run() in-process under a 10s watchdog and asserts the
// contract every outcome shares: exit 0 and EXACTLY ONE stdout line, the
// nonce-framed result, whose JSON decodes strictly.
func runHelper(t *testing.T, root string) helperResult {
	t.Helper()
	type out struct {
		code           int
		stdout, stderr string
		elapsed        time.Duration
	}
	done := make(chan out, 1)
	start := time.Now()
	go func() {
		var so, se bytes.Buffer
		code := run([]string{root}, envOf(map[string]string{nonceEnv: testNonce}), &so, &se)
		done <- out{code, so.String(), se.String(), time.Since(start)}
	}()
	var o out
	select {
	case o = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("run did not return within the 10s watchdog")
	}
	if o.code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr %q)", o.code, o.stderr)
	}
	lines := strings.Split(strings.TrimSuffix(o.stdout, "\n"), "\n")
	if len(lines) != 1 || !strings.HasSuffix(o.stdout, "\n") {
		t.Fatalf("stdout = %q, want exactly one result line", o.stdout)
	}
	prefix := resultMarker + " " + testNonce + " "
	if !strings.HasPrefix(lines[0], prefix) {
		t.Fatalf("result line %q lacks the %q frame", lines[0], prefix)
	}
	var r result
	dec := json.NewDecoder(strings.NewReader(strings.TrimPrefix(lines[0], prefix)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		t.Fatalf("result JSON: %v (%q)", err, lines[0])
	}
	if r.Reason != "" && (r.Imports != nil || r.Emittable != nil) {
		t.Fatalf("a refusal carries a graph: %+v", r)
	}
	return helperResult{result: r, stderr: o.stderr, elapsed: o.elapsed}
}

func envOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func wantReason(t *testing.T, got helperResult, want string) {
	t.Helper()
	if got.Reason != want {
		t.Fatalf("reason = %q, want %q (stderr %q)", got.Reason, want, got.stderr)
	}
}

func TestRun_GraphEdges(t *testing.T) {
	realGo(t)
	got := runHelper(t, writeFixture(t, baseFiles()))
	wantReason(t, got, "")
	want := map[string][]string{
		"m1/a": {}, "m1/b": {"m1/a"}, "m1/c": {}, "m1/d": {"m1/b"},
		"m1/e": {"m1/a"}, "m2/f": {"m1/a"},
	}
	if !reflect.DeepEqual(got.Imports, want) {
		t.Fatalf("imports = %v, want %v", got.Imports, want)
	}
	if em := []string{"m1/a", "m1/b", "m1/c", "m1/d", "m1/e", "m2/f"}; !reflect.DeepEqual(got.Emittable, em) {
		t.Fatalf("emittable = %v, want the sorted %v", got.Emittable, em)
	}
}

func TestRun_TagExcludedTestImportJoinsReach(t *testing.T) {
	realGo(t)
	got := runHelper(t, writeFixture(t, map[string]string{
		"go.work":               "go 1.22\n\nuse ./m3\n",
		"m3/go.mod":             mod("example.com/m3"),
		"m3/a/a.go":             "package a\n",
		"m3/h/h.go":             "package h\n",
		"m3/h/h_tagged_test.go": "//go:build fishhawk_never\n\npackage h_test\n\nimport _ \"example.com/m3/a\"\n",
	}))
	wantReason(t, got, "")
	if !reflect.DeepEqual(got.Imports["m3/h"], []string{"m3/a"}) {
		t.Fatalf("imports[m3/h] = %v, want [m3/a]", got.Imports["m3/h"])
	}
}

func TestRun_HostExcludedDirIsTraversalOnly(t *testing.T) {
	realGo(t)
	got := runHelper(t, writeFixture(t, map[string]string{
		"go.work":   "go 1.22\n\nuse ./m3\n",
		"m3/go.mod": mod("example.com/m3"),
		"m3/a/a.go": "package a\n",
		"m3/w/w.go": "//go:build fishhawk_never\n\npackage w\n\nimport _ \"example.com/m3/a\"\n",
	}))
	wantReason(t, got, "")
	if !reflect.DeepEqual(got.Imports["m3/w"], []string{"m3/a"}) {
		t.Fatalf("imports[m3/w] = %v, want [m3/a]", got.Imports["m3/w"])
	}
	if !reflect.DeepEqual(got.Emittable, []string{"m3/a"}) {
		t.Fatalf("emittable = %v, want only m3/a (m3/w is traversal-only)", got.Emittable)
	}
}

func TestRun_RootModuleOnly(t *testing.T) {
	realGo(t)
	got := runHelper(t, writeFixture(t, map[string]string{
		"go.mod": mod("example.com/solo"),
		"a/a.go": "package a\n",
		"b/b.go": "package b\n\nimport _ \"example.com/solo/a\"\n",
	}))
	wantReason(t, got, "")
	if !reflect.DeepEqual(got.Imports["b"], []string{"a"}) || !reflect.DeepEqual(got.Emittable, []string{"a", "b"}) {
		t.Fatalf("graph = %v / %v", got.Imports, got.Emittable)
	}
}

// TestRun_Refusals is one row per walk / go list / metadata refusal mode the
// real go toolchain produces.
func TestRun_Refusals(t *testing.T) {
	realGo(t)
	with := func(extra map[string]string) map[string]string {
		files := baseFiles()
		for k, v := range extra {
			files[k] = v
		}
		return files
	}
	cases := []struct {
		name  string
		files map[string]string
		want  string
	}{
		{"packageError", with(map[string]string{"m1/g/g.go": "package g\n\nimport _ \"example.org/absent/x\"\n"}), "reach_load_failed:package_error:example.com/m1/g"},
		{"unparsable", with(map[string]string{"m1/z/z.go": "package z\n\nimport (\n\t\"fmt\n"}), "reach_load_failed:unparsable_go_file:m1/z/z.go"},
		{"symlinkGoFile", with(map[string]string{"m1/a/real.go.txt": "package a\n", "m1/a/link.go": "symlink:real.go.txt"}), "reach_load_failed:unparsable_go_file:m1/a/link.go"},
		{"fifoGoFile", with(map[string]string{"m1/a/pipe.go": "fifo:"}), "reach_load_failed:unparsable_go_file:m1/a/pipe.go"},
		{"importNotPackage", with(map[string]string{"m1/a/a.go": "package a\n\nimport _ \"example.com/m1/nosuch\"\n"}), "reach_load_failed:import_not_package:example.com/m1/nosuch"},
		{"brokenGoMod", with(map[string]string{"m1/go.mod": "module example.com/m1\n\ngo 1.22\n\nrequire (\n"}), "reach_load_failed:gomod_unreadable:m1/go.mod"},
		{"modulelessGoMod", with(map[string]string{"m1/go.mod": "go 1.22\n"}), "reach_load_failed:gomod_unreadable:m1/go.mod"},
		{"missingUseGoMod", with(map[string]string{"go.work": "go 1.22\n\nuse (\n\t./m1\n\t./m2\n\t./m3\n)\n", "m3/x.go": "package x\n"}), "reach_load_failed:gomod_unreadable:m3/go.mod"},
		{"brokenGoWork", with(map[string]string{"go.work": "go 1.22\n\nuse (\n"}), "reach_load_failed:gomod_unreadable:go.work"},
		{"noModule", map[string]string{"a/a.go": "package a\n"}, "reach_unsupported_workspace"},
		{"zeroUses", map[string]string{"go.work": "go 1.22\n"}, "reach_unsupported_workspace"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			wantReason(t, runHelper(t, writeFixture(t, c.files)), c.want)
		})
	}
}

// TestRun_MetadataNotRegular: the Lstat predicate at the root, at a use and
// at a replace target, each refused before go list runs.
func TestRun_MetadataNotRegular(t *testing.T) {
	replaceDep := "module example.com/m1\n\ngo 1.22\n\nrequire example.com/dep v0.0.0\n"
	cases := []struct {
		name  string
		files func(parent string) map[string]string
		want  string
	}{
		{"symlinkedGoWork", func(p string) map[string]string {
			return map[string]string{"go.work": "symlink:" + filepath.Join(p, "elsewhere", "go.work")}
		}, "reach_unsupported_workspace:metadata_not_regular:root"},
		{"symlinkedRootGoSum", func(p string) map[string]string {
			return map[string]string{"go.sum": "symlink:" + filepath.Join(p, "elsewhere", "go.sum")}
		}, "reach_unsupported_workspace:metadata_not_regular:root"},
		{"symlinkedGoWorkSum", func(p string) map[string]string {
			return map[string]string{"go.work.sum": "symlink:" + filepath.Join(p, "elsewhere", "go.sum")}
		}, "reach_unsupported_workspace:metadata_not_regular:root"},
		{"fifoRootGoMod", func(string) map[string]string {
			return map[string]string{"go.work": "", "go.mod": "fifo:"}
		}, "reach_unsupported_workspace:metadata_not_regular:root"},
		{"symlinkedUseGoMod", func(p string) map[string]string {
			return map[string]string{"m1/go.mod": "symlink:" + filepath.Join(p, "elsewhere", "go.mod")}
		}, "reach_unsupported_workspace:metadata_not_regular:use"},
		{"symlinkedUseGoSum", func(p string) map[string]string {
			return map[string]string{"m1/go.sum": "symlink:" + filepath.Join(p, "elsewhere", "go.sum")}
		}, "reach_unsupported_workspace:metadata_not_regular:use"},
		{"symlinkedReplaceGoModViaGoWork", func(p string) map[string]string {
			return map[string]string{
				"go.work":    goWorkM1M2 + "\nreplace example.com/dep => ./dep\n",
				"m1/go.mod":  replaceDep,
				"dep/dep.go": "package dep\n",
				"dep/go.mod": "symlink:" + filepath.Join(p, "elsewhere", "go.mod"),
			}
		}, "reach_unsupported_workspace:metadata_not_regular:replace"},
		{"symlinkedReplaceGoModViaGoMod", func(p string) map[string]string {
			return map[string]string{
				"m1/go.mod":  replaceDep + "\nreplace example.com/dep => ../dep\n",
				"dep/dep.go": "package dep\n",
				"dep/go.mod": "symlink:" + filepath.Join(p, "elsewhere", "go.mod"),
			}
		}, "reach_unsupported_workspace:metadata_not_regular:replace"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			setVar(t, &loadTimeout, 5*time.Second)
			parent := t.TempDir()
			root := filepath.Join(parent, "root")
			files := baseFiles()
			for k, v := range c.files(parent) {
				if v == "" {
					delete(files, k)
					continue
				}
				files[k] = v
			}
			writeFiles(t, root, files)
			writeFiles(t, parent, map[string]string{
				"elsewhere/go.work": goWorkM1M2,
				"elsewhere/go.mod":  mod("example.com/m1"),
				"elsewhere/go.sum":  "",
			})
			calls := recordingGo(t)
			wantReason(t, runHelper(t, root), c.want)
			if n := calls(); n != 0 {
				t.Fatalf("go list ran %d time(s), want 0", n)
			}
		})
	}
}

func TestRun_UseEscapes(t *testing.T) {
	outside := writeFixture(t, map[string]string{"go.mod": mod("example.com/out"), "o/o.go": "package o\n"})
	for name, u := range map[string]string{
		"absolute":       outside,
		"dotdot":         "../outside",
		"symlinkOutside": "./m9",
	} {
		t.Run(name, func(t *testing.T) {
			files := baseFiles()
			files["go.work"] = "go 1.22\n\nuse (\n\t./m1\n\t" + u + "\n)\n"
			files["m9"] = "symlink:" + outside
			parent := t.TempDir()
			root := filepath.Join(parent, "root")
			writeFiles(t, root, files)
			writeFiles(t, parent, map[string]string{"outside/go.mod": mod("example.com/out2"), "outside/p/p.go": "package p\n"})
			calls := recordingGo(t)
			wantReason(t, runHelper(t, root), "reach_unsupported_workspace")
			if n := calls(); n != 0 {
				t.Fatalf("go list ran %d time(s), want 0", n)
			}
		})
	}
}

func TestRun_ReplaceEscapes(t *testing.T) {
	rows := map[string]map[string]string{
		"gomod":  {"m1/go.mod": "module example.com/m1\n\ngo 1.22\n\nrequire example.com/x v0.0.0\n\nreplace example.com/x => ../../outside\n"},
		"gowork": {"m1/go.mod": "module example.com/m1\n\ngo 1.22\n\nrequire example.com/x v0.0.0\n", "go.work": goWorkM1M2 + "\nreplace example.com/x => ../outside\n"},
	}
	for name, extra := range rows {
		t.Run(name, func(t *testing.T) {
			parent := t.TempDir()
			root := filepath.Join(parent, "root")
			writeFiles(t, parent, map[string]string{"outside/go.mod": mod("example.com/x"), "outside/x.go": "package x\n"})
			files := baseFiles()
			files["m1/a/a.go"] = "package a\n\nimport _ \"example.com/x\"\n"
			for k, v := range extra {
				files[k] = v
			}
			writeFiles(t, root, files)
			calls := recordingGo(t)
			wantReason(t, runHelper(t, root), "reach_unsupported_workspace")
			if n := calls(); n != 0 {
				t.Fatalf("go list ran %d time(s), want 0", n)
			}
		})
	}
}

func TestRun_OversizeGoModIsFull(t *testing.T) {
	setVar(t, &metadataCap, 64)
	calls := recordingGo(t)
	body := mod("example.com/big") + "// " + strings.Repeat("x", 200) + "\n"
	wantReason(t, runHelper(t, writeFixture(t, map[string]string{"go.mod": body, "a/a.go": "package a\n"})), "reach_load_failed:metadata_too_large:go.mod")
	if n := calls(); n != 0 {
		t.Fatalf("go list ran %d time(s), want 0", n)
	}
}

// TestRun_GoListFailureModes drives each go list failure through a fake go.
func TestRun_GoListFailureModes(t *testing.T) {
	root := writeFixture(t, baseFiles())
	real, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	rec := `{"ImportPath":"example.com/m1/a","Dir":"` + filepath.Join(real, "m1", "a") +
		`","Module":{"Path":"example.com/m1","Dir":"` + filepath.Join(real, "m1") + `"}}`
	cases := []struct {
		name, body string
		cap        int64
		want       string
	}{
		{"nonZeroExit", "echo '" + rec + "'\necho boom >&2\nexit 2", 0, "reach_load_failed:go_list_exit"},
		{"malformedJSON", "echo '" + rec + "'\necho '{garbage'\nexit 0", 0, "reach_load_failed:go_list_malformed_json"},
		{"stdoutCap", "for i in 1 2 3 4 5 6 7 8; do echo '" + rec + "'; done\nexit 0", int64(len(rec) + 10), "reach_load_failed:go_list_stdout_cap"},
		{"noPackages", "exit 0", 0, "reach_load_failed:no_packages"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fakeGo(t, c.body)
			if c.cap > 0 {
				setVar(t, &stdoutCap, c.cap)
			}
			got := runHelper(t, root)
			wantReason(t, got, c.want)
			if c.name == "nonZeroExit" && !strings.Contains(got.stderr, "slice_reach_go_list_failed: boom") {
				t.Fatalf("stderr = %q, want the go list stderr tail", got.stderr)
			}
		})
	}
}

// TestRun_LoadTimeout: the one internal bound stops a hanging go list, and a
// hanging metadata read.
func TestRun_LoadTimeout(t *testing.T) {
	root := writeFixture(t, baseFiles())
	for name, install := range map[string]func(t *testing.T){
		"goList": func(t *testing.T) { fakeGo(t, "sleep 60") },
		"metadata": func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "go"), []byte("#!/bin/sh\nsleep 60\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			prependPath(t, dir)
		},
	} {
		t.Run(name, func(t *testing.T) {
			install(t)
			setVar(t, &loadTimeout, 2*time.Second)
			got := runHelper(t, root)
			wantReason(t, got, "reach_load_failed:load_timeout")
			if got.elapsed > 6*time.Second {
				t.Fatalf("load took %s, want < 6s", got.elapsed)
			}
		})
	}
}

// TestRun_EnvPinned: every inherited value is overridden last-wins in the go
// list env, value for value with the host loader's sliceReachPinnedEnv.
func TestRun_EnvPinned(t *testing.T) {
	root := writeFixture(t, baseFiles())
	envOut := filepath.Join(t.TempDir(), "env.out")
	fakeGo(t, "env > '"+envOut+"'\nexit 1")
	for k, v := range map[string]string{
		"CGO_ENABLED": "1", "GOFLAGS": "-tags=x", "GOPROXY": "direct", "GOTOOLCHAIN": "auto",
		"GONOPROXY": "example.com/*", "GOPRIVATE": "example.com/*", "GONOSUMDB": "example.com/*",
		"GONOSUMCHECK": "1", "GOINSECURE": "example.com/*", "GOENV": filepath.Join(t.TempDir(), "goenv"),
		"GOCACHEPROG": "/bin/false", "GOSUMDB": "sum.golang.org http://127.0.0.1:1", "GOWORK": "off",
	} {
		t.Setenv(k, v)
	}
	wantReason(t, runHelper(t, root), "reach_load_failed:go_list_exit")
	real, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(envOut)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	env := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if k, v, ok := strings.Cut(sc.Text(), "="); ok {
			env[k] = v
		}
	}
	want := map[string]string{
		"CGO_ENABLED": "0", "GOFLAGS": "", "GOPROXY": "off", "GOTOOLCHAIN": "local",
		"GOWORK": filepath.Join(real, "go.work"), "GONOPROXY": "", "GOPRIVATE": "", "GONOSUMDB": "",
		"GONOSUMCHECK": "", "GOINSECURE": "", "GOENV": "off", "GOCACHEPROG": "", "GOSUMDB": "off",
	}
	for k, v := range want {
		if have, ok := env[k]; !ok || have != v {
			t.Errorf("go list env %s = %q (present %v), want %q", k, have, ok, v)
		}
	}
	if len(pinnedEnv("x")) != len(want) {
		t.Fatalf("pinnedEnv has %d entries, want the %d pinned keys", len(pinnedEnv("x")), len(want))
	}
}

// TestRun_UsageErrors: no nonce, a nonce that would break the frame, or a
// wrong argument count is exit 2 with NO result line.
func TestRun_UsageErrors(t *testing.T) {
	cases := []struct {
		name  string
		args  []string
		nonce string
	}{
		{"missingNonce", []string{"/tmp"}, ""},
		{"nonceWithSpace", []string{"/tmp"}, "a b"},
		{"nonceWithNewline", []string{"/tmp"}, "a\nb"},
		{"noRoot", nil, testNonce},
		{"emptyRoot", []string{""}, testNonce},
		{"twoRoots", []string{"/tmp", "/tmp"}, testNonce},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var so, se bytes.Buffer
			code := run(c.args, envOf(map[string]string{nonceEnv: c.nonce}), &so, &se)
			if code != 2 {
				t.Fatalf("exit = %d, want 2", code)
			}
			if so.Len() != 0 {
				t.Fatalf("stdout = %q, want no result line", so.String())
			}
			if !strings.Contains(se.String(), "usage:") {
				t.Fatalf("stderr = %q, want a usage line", se.String())
			}
		})
	}
}

// TestAssemble is the pure graph assembly, one row per refusal.
func TestAssemble(t *testing.T) {
	root := "/clone"
	m := &struct{ Path, Dir string }{Path: "example.com/m1", Dir: "/clone/m1"}
	walked := map[string][]string{"m1/a": nil, "m1/b": {"m1/a"}}
	rec := func(ip, dir string) record { return record{ImportPath: ip, Dir: dir, Module: m} }
	long := "example.com/" + strings.Repeat("p", 400)
	cases := []struct {
		name    string
		records []record
		want    string
	}{
		{"outside", []record{rec("example.com/m1/a", "/clone/m1/a"), rec("example.com/evil", "/elsewhere/evil")}, "reach_dir_outside_clone:example.com/evil"},
		{"moduleOutside", []record{{ImportPath: "example.com/m1/a", Dir: "/clone/m1/a", Module: &struct{ Path, Dir string }{"example.com/m1", "/elsewhere"}}}, "reach_dir_outside_clone:example.com/m1/a"},
		{"unwalked", []record{rec("example.com/m1/a", "/clone/m1/a"), rec("example.com/m1/q", "/clone/m1/q")}, "reach_load_failed:unwalked_package_dir:example.com/m1/q"},
		{"noModule", []record{rec("example.com/m1/a", "/clone/m1/a"), {ImportPath: "example.com/m1/b", Dir: "/clone/m1/b"}}, "reach_load_failed:no_module:example.com/m1/b"},
		{"noPackages", nil, "reach_load_failed:no_packages"},
		{"errored", []record{{ImportPath: "example.com/m1/a", Dir: "/clone/m1/a", Module: m, Incomplete: true}}, "reach_load_failed:package_error:example.com/m1/a"},
		{"depsErrored", []record{{ImportPath: "example.com/m1/a", Dir: "/clone/m1/a", Module: m, DepsErrors: []*struct{ Err string }{{"x"}}}}, "reach_load_failed:package_error:example.com/m1/a"},
		{"boundedDetail", []record{{ImportPath: long, Dir: "/clone/m1/a", Error: &struct{ Err string }{"x"}}}, "reach_load_failed:package_error:" + long[:detailMax]},
		{"ok", []record{rec("example.com/m1/a", "/clone/m1/a"), rec("example.com/m1/b", "/clone/m1/b")}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			imp, em, reason := assemble(root, c.records, walked)
			if reason != c.want {
				t.Fatalf("reason = %q, want %q", reason, c.want)
			}
			if c.want == "" && (!em["m1/a"] || !em["m1/b"] || !reflect.DeepEqual(imp["m1/b"], []string{"m1/a"})) {
				t.Fatalf("graph = %v / %v", imp, em)
			}
		})
	}
}

// TestIsDirectoryPath pins the stdlib port to modfile.IsDirectoryPath.
func TestIsDirectoryPath(t *testing.T) {
	for _, p := range []string{
		".", "..", "./x", "../x", `.\x`, `..\x`, "/abs", `\abs`, "C:", "c:/x", "Z:x",
		"example.com/x", "x", "", ".x", "..x", "1:", ":x", "@:x",
	} {
		if got, want := isDirectoryPath(p), modfile.IsDirectoryPath(p); got != want {
			t.Errorf("isDirectoryPath(%q) = %v, modfile says %v", p, got, want)
		}
	}
}

// TestHelper_StdlibOnly: the embedded source must build offline with no
// go.mod, so main.go may import the standard library only (no import whose
// first path element contains a dot).
func TestHelper_StdlibOnly(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, parser.ImportsOnly)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Imports) == 0 {
		t.Fatal("main.go parsed with no imports; the check would be vacuous")
	}
	for _, is := range f.Imports {
		ip, err := strconv.Unquote(is.Path.Value)
		if err != nil {
			t.Fatal(err)
		}
		first, _, _ := strings.Cut(ip, "/")
		if strings.Contains(first, ".") {
			t.Errorf("main.go imports %q: the in-image helper must be stdlib-only", ip)
		}
	}
}
