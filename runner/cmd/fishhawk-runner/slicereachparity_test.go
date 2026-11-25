package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// slicereachparity_test.go (#5524) pins the in-image reach helper
// (slicereachhelper/main.go) to the host loader: it builds the lone helper
// file OUTSIDE any module exactly as the container does (no go.mod,
// GOWORK=off, GOPROXY=off), runs the binary over each shared fixture and
// requires its (imports, emittable, reason) to equal sliceReachLoad's on the
// same tree. Fixtures import only the stdlib and workspace packages, so
// GOPROXY=off needs no module cache.

const sliceReachParityNonce = "parity-5524"

// sliceReachParityBuild copies the helper source into a fresh dir outside
// any module and builds it there under the container's pins.
func sliceReachParityBuild(t *testing.T, goBin string) string {
	t.Helper()
	src, err := os.ReadFile(filepath.Join(pkgSrcDir, "slicereachhelper", "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"), src, 0o644); err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(), "GOWORK=off", "GOFLAGS=", "GOTOOLCHAIN=local", "GOPROXY=off", "CGO_ENABLED=0", "GOENV=off")
	gomod := exec.Command(goBin, "env", "GOMOD")
	gomod.Dir, gomod.Env = dir, env
	out, err := gomod.Output()
	if err != nil {
		t.Fatalf("go env GOMOD: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != os.DevNull && got != "" {
		t.Fatalf("the build dir is inside a module (GOMOD=%q); the parity build must be module-less", got)
	}
	bin := filepath.Join(t.TempDir(), "slicereachhelper")
	build := exec.Command(goBin, "build", "-o", bin, "./main.go")
	build.Dir, build.Env = dir, env
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building the lone helper file outside a module: %v\n%s", err, out)
	}
	return bin
}

type sliceReachParityGraph struct {
	Reason    string              `json:"reason,omitempty"`
	Imports   map[string][]string `json:"imports,omitempty"`
	Emittable []string            `json:"emittable,omitempty"`
}

// sliceReachParityRunHelper runs the built helper over root and decodes its
// one nonce-framed result line.
func sliceReachParityRunHelper(t *testing.T, bin, root string) sliceReachParityGraph {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, root)
	cmd.Env = append(os.Environ(), "FISHHAWK_SLICE_REACH_NONCE="+sliceReachParityNonce)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("helper: %v (stderr %q)", err, stderr.String())
	}
	prefix := "FISHHAWK_SLICE_REACH_RESULT " + sliceReachParityNonce + " "
	line := strings.TrimSuffix(string(out), "\n")
	if strings.Contains(line, "\n") || !strings.HasPrefix(line, prefix) {
		t.Fatalf("helper stdout = %q, want exactly one %q line", out, prefix)
	}
	var g sliceReachParityGraph
	dec := json.NewDecoder(strings.NewReader(strings.TrimPrefix(line, prefix)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&g); err != nil {
		t.Fatalf("helper JSON: %v (%q)", err, line)
	}
	return g
}

// sliceReachParityNormalize maps a graph to comparable values: nil edge
// lists become empty and emittable becomes the sorted dir list.
func sliceReachParityNormalize(imports map[string][]string, emittable []string) (map[string][]string, []string) {
	var imp map[string][]string
	if imports != nil {
		imp = make(map[string][]string, len(imports))
		for k, v := range imports {
			if v == nil {
				v = []string{}
			}
			imp[k] = v
		}
	}
	if len(emittable) == 0 {
		emittable = nil
	}
	return imp, emittable
}

func TestSliceReachHelper_ParityWithHostLoader(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go toolchain on PATH")
	}
	bin := sliceReachParityBuild(t, goBin)
	with := func(extra map[string]string) func(string) map[string]string {
		return func(string) map[string]string {
			files := sliceReachBaseFiles()
			for k, v := range extra {
				if v == "" {
					delete(files, k)
					continue
				}
				files[k] = v
			}
			return files
		}
	}
	outside := func(parent string) string { return filepath.Join(parent, "outside") }
	elsewhere := func(parent, name string) string { return "symlink:" + filepath.Join(parent, "elsewhere", name) }
	depMod := "module example.com/m1\n\ngo 1.22\n\nrequire example.com/dep v0.0.0\n"
	cases := []struct {
		name  string
		files func(parent string) map[string]string
		want  string
	}{
		{"graphEdges", with(nil), ""},
		{"tagExcludedTestImport", func(string) map[string]string {
			return map[string]string{
				"go.work": "go 1.22\n\nuse ./m3\n", "m3/go.mod": sliceReachMod("example.com/m3"), "m3/a/a.go": "package a\n",
				"m3/h/h.go": "package h\n", "m3/h/h_tagged_test.go": "//go:build fishhawk_never\n\npackage h_test\n\nimport _ \"example.com/m3/a\"\n",
			}
		}, ""},
		{"hostExcludedDir", func(string) map[string]string {
			return map[string]string{
				"go.work": "go 1.22\n\nuse ./m3\n", "m3/go.mod": sliceReachMod("example.com/m3"), "m3/a/a.go": "package a\n",
				"m3/w/w.go": "//go:build fishhawk_never\n\npackage w\n\nimport _ \"example.com/m3/a\"\n",
			}
		}, ""},
		{"rootModuleOnly", func(string) map[string]string {
			return map[string]string{
				"go.mod": sliceReachMod("example.com/solo"), "a/a.go": "package a\n",
				"b/b.go": "package b\n\nimport _ \"example.com/solo/a\"\n", "root.go": "package solo\n\nimport _ \"example.com/solo/b\"\n",
			}
		}, ""},
		{"erroredPackage", with(map[string]string{"m1/g/g.go": "package g\n\nimport _ \"example.org/absent/x\"\n"}), "reach_load_failed:package_error:example.com/m1/g"},
		{"unparsableFile", with(map[string]string{"m1/z/z.go": "package z\n\nimport (\n\t\"fmt\n"}), "reach_load_failed:unparsable_go_file:m1/z/z.go"},
		{"symlinkedGoFile", with(map[string]string{"m1/a/real.go.txt": "package a\n", "m1/a/link.go": "symlink:real.go.txt"}), "reach_load_failed:unparsable_go_file:m1/a/link.go"},
		{"fifoGoFile", with(map[string]string{"m1/a/pipe.go": "fifo:"}), "reach_load_failed:unparsable_go_file:m1/a/pipe.go"},
		{"importNotPackage", with(map[string]string{"m1/a/a.go": "package a\n\nimport _ \"example.com/m1/nosuch\"\n"}), "reach_load_failed:import_not_package:example.com/m1/nosuch"},
		{"useEscapeDotdot", with(map[string]string{"go.work": "go 1.22\n\nuse (\n\t./m1\n\t../outside\n)\n"}), "reach_unsupported_workspace"},
		{"useEscapeSymlink", func(p string) map[string]string {
			return with(map[string]string{"go.work": "go 1.22\n\nuse (\n\t./m1\n\t./m9\n)\n", "m9": "symlink:" + outside(p)})(p)
		}, "reach_unsupported_workspace"},
		{"replaceEscapeGoMod", with(map[string]string{
			"m1/go.mod": "module example.com/m1\n\ngo 1.22\n\nrequire example.com/x v0.0.0\n\nreplace example.com/x => ../../outside\n",
		}), "reach_unsupported_workspace"},
		{"replaceEscapeGoWork", with(map[string]string{
			"go.work": sliceReachGoWorkM1M2 + "\nreplace example.com/x => ../outside\n",
		}), "reach_unsupported_workspace"},
		{"symlinkedGoWork", func(p string) map[string]string {
			return with(map[string]string{"go.work": elsewhere(p, "go.work")})(p)
		}, "reach_unsupported_workspace:metadata_not_regular:root"},
		{"symlinkedGoSum", func(p string) map[string]string {
			return map[string]string{"go.mod": sliceReachMod("example.com/solo"), "a/a.go": "package a\n", "go.sum": elsewhere(p, "go.sum")}
		}, "reach_unsupported_workspace:metadata_not_regular:root"},
		{"symlinkedGoWorkSum", func(p string) map[string]string {
			return with(map[string]string{"go.work.sum": elsewhere(p, "go.sum")})(p)
		}, "reach_unsupported_workspace:metadata_not_regular:root"},
		{"symlinkedUseGoMod", func(p string) map[string]string {
			return with(map[string]string{"m1/go.mod": elsewhere(p, "go.mod")})(p)
		}, "reach_unsupported_workspace:metadata_not_regular:use"},
		{"symlinkedUseGoSum", func(p string) map[string]string {
			return with(map[string]string{"m1/go.sum": elsewhere(p, "go.sum")})(p)
		}, "reach_unsupported_workspace:metadata_not_regular:use"},
		{"symlinkedReplaceGoMod", func(p string) map[string]string {
			return with(map[string]string{
				"go.work": sliceReachGoWorkM1M2 + "\nreplace example.com/dep => ./dep\n", "m1/go.mod": depMod,
				"dep/dep.go": "package dep\n", "dep/go.mod": elsewhere(p, "go.mod"),
			})(p)
		}, "reach_unsupported_workspace:metadata_not_regular:replace"},
		{"brokenGoMod", with(map[string]string{"m1/go.mod": "module example.com/m1\n\ngo 1.22\n\nrequire (\n"}), "reach_load_failed:gomod_unreadable:m1/go.mod"},
		{"noModule", func(string) map[string]string { return map[string]string{"a/a.go": "package a\n"} }, "reach_unsupported_workspace"},
		{"zeroUses", func(string) map[string]string { return map[string]string{"go.work": "go 1.22\n"} }, "reach_unsupported_workspace"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			parent := t.TempDir()
			root := filepath.Join(parent, "root")
			sliceReachWriteFiles(t, root, c.files(parent))
			sliceReachWriteFiles(t, parent, map[string]string{
				"outside/go.mod":    sliceReachMod("example.com/x"),
				"outside/x.go":      "package x\n",
				"elsewhere/go.work": sliceReachGoWorkM1M2,
				"elsewhere/go.mod":  sliceReachMod("example.com/m1"),
				"elsewhere/go.sum":  "",
			})
			host := sliceReachRun(t, root)
			helper := sliceReachParityRunHelper(t, bin, root)
			if host.reason != c.want {
				t.Fatalf("host reason = %q, want %q (the fixture does not hit its row)", host.reason, c.want)
			}
			if helper.Reason != host.reason {
				t.Fatalf("helper reason = %q, host reason = %q", helper.Reason, host.reason)
			}
			hostImp, hostEm := sliceReachParityNormalize(host.imports, sliceReachSortedKeys(host.emittable))
			helpImp, helpEm := sliceReachParityNormalize(helper.Imports, helper.Emittable)
			if !reflect.DeepEqual(helpImp, hostImp) {
				t.Errorf("helper imports = %v, host imports = %v", helpImp, hostImp)
			}
			if !reflect.DeepEqual(helpEm, hostEm) {
				t.Errorf("helper emittable = %v, host emittable = %v", helpEm, hostEm)
			}
			if c.want == "" && len(hostImp) == 0 {
				t.Fatal("a graph row produced an empty graph; the comparison would be vacuous")
			}
		})
	}
}
