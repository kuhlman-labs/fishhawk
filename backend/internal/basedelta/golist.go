package basedelta

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/procgroup"
)

// Loader loads the Go import graph of one workspace checkout.
type Loader interface {
	Load(ctx context.Context, workspaceDir string) (*Graph, error)
}

// DefaultLoadTimeout bounds one GoListLoader.Load when Timeout is zero.
const DefaultLoadTimeout = 2 * time.Minute

// killGrace is how long a cancelled go subprocess's pipes may stay open
// before os/exec force-closes them (procgroup.Harden).
const killGrace = 2 * time.Second

// stderrTail bounds how much of a failed go command's stderr an error carries.
const stderrTail = 512

// GoListLoader loads a workspace graph with the go toolchain.
//
// It runs `go list -e -json` once, in the HOST build context, over every
// go.work module (or the root module), and then parses the import clauses of
// EVERY .go file under each module with go/parser (ImportsOnly), ignoring
// build constraints. The parse is what puts an OS-tagged file's imports (an
// x_windows.go on a linux host) into the graph, and what discovers a package
// directory whose files are all excluded on the host, which `./...` skips
// silently. Parsing needs no module source, so the loader never requires a
// non-host-platform third-party module in GOMODCACHE (a linux-only CI cache
// under GOPROXY=off).
//
// It fails closed on a non-zero exit, malformed output, any listed package
// carrying Error, DepsErrors or Incomplete, a package directory outside the
// workspace, an import naming a workspace-module path that is no package, an
// unparsable .go file, zero packages when a go.work or go.mod exists, or a
// go.work module go list reports no package for.
type GoListLoader struct {
	// GoBin is the go binary; empty resolves "go" on PATH.
	GoBin string
	// Timeout bounds the whole Load; zero means DefaultLoadTimeout.
	Timeout time.Duration
	// Env is appended to os.Environ() before the pinned variables, so it
	// cannot override GOWORK, GOPROXY, GOTOOLCHAIN, GOFLAGS or CGO_ENABLED.
	Env []string
}

type goListPackage struct {
	ImportPath   string
	Dir          string
	Module       *struct{ Path, Dir string }
	Imports      []string
	TestImports  []string
	XTestImports []string
	Error        *struct{ Err string }
	DepsErrors   []*struct{ Err string }
	Incomplete   bool
}

type workEdit struct {
	Use []struct{ DiskPath string }
}

// Load implements Loader.
func (l *GoListLoader) Load(ctx context.Context, workspaceDir string) (*Graph, error) {
	abs, err := filepath.Abs(workspaceDir)
	if err != nil {
		return nil, fmt.Errorf("workspace %q: %w", workspaceDir, err)
	}
	root, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("workspace %q: %w", workspaceDir, err)
	}
	goBin := l.GoBin
	if goBin == "" {
		if goBin, err = exec.LookPath("go"); err != nil {
			return nil, fmt.Errorf("go toolchain: %w", err)
		}
	}
	timeout := l.Timeout
	if timeout <= 0 {
		timeout = DefaultLoadTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	workFile := filepath.Join(root, "go.work")
	var uses []string
	gowork := "off"
	switch {
	case fileExists(workFile):
		gowork = workFile
		out, err := l.run(ctx, goBin, root, gowork, "work", "edit", "-json", workFile)
		if err != nil {
			return nil, err
		}
		var we workEdit
		if err := json.Unmarshal(out, &we); err != nil {
			return nil, fmt.Errorf("go work edit -json: malformed output: %w", err)
		}
		for _, u := range we.Use {
			use := path.Clean(filepath.ToSlash(u.DiskPath))
			if u.DiskPath == "" || path.IsAbs(use) || use == ".." || strings.HasPrefix(use, "../") {
				return nil, fmt.Errorf("go.work use %q is not an in-workspace path", u.DiskPath)
			}
			uses = append(uses, use)
		}
		if len(uses) == 0 {
			return nil, errors.New("go.work lists no modules")
		}
	case fileExists(filepath.Join(root, "go.mod")):
		uses = []string{"."}
	default:
		return NewGraph(nil), nil
	}

	args := []string{"list", "-e", "-json=ImportPath,Dir,Module,Imports,TestImports,XTestImports,Error,DepsErrors,Incomplete"}
	for _, u := range uses {
		if u == "." {
			args = append(args, "./...")
		} else {
			args = append(args, "./"+u+"/...")
		}
	}
	out, err := l.run(ctx, goBin, root, gowork, args...)
	if err != nil {
		return nil, err
	}
	listed, err := decodeGoList(out)
	if err != nil {
		return nil, err
	}
	if len(listed) == 0 {
		return nil, errors.New("go list: zero packages")
	}
	return buildGraph(ctx, root, uses, listed)
}

// run executes one hardened go command in root under the pinned environment
// and returns its stdout.
func (l *GoListLoader) run(ctx context.Context, goBin, root, gowork string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, goBin, args...)
	cmd.Dir = root
	cmd.Env = append(append(os.Environ(), l.Env...), pinnedEnv(gowork)...)
	var stdout bytes.Buffer
	stderr := &tailBuffer{max: stderrTail}
	cmd.Stdout = &stdout
	cmd.Stderr = stderr
	procgroup.Harden(cmd, killGrace)
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("go %s: %w", args[0], ctx.Err())
		}
		return nil, fmt.Errorf("go %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// pinnedEnv is appended LAST, so it wins over every inherited value: the
// workspace is the checkout's own go.work (or none), nothing is fetched, a
// toolchain mismatch fails instead of downloading, and no inherited GOFLAGS
// (-mod=vendor, a build tag) changes what is listed.
func pinnedEnv(gowork string) []string {
	return []string{
		"GOWORK=" + gowork,
		"GOPROXY=off",
		"GOTOOLCHAIN=local",
		"GOFLAGS=",
		"CGO_ENABLED=0",
	}
}

func decodeGoList(out []byte) ([]goListPackage, error) {
	dec := json.NewDecoder(bytes.NewReader(out))
	var pkgs []goListPackage
	for {
		var p goListPackage
		err := dec.Decode(&p)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("go list: malformed output: %w", err)
		}
		switch {
		case p.Error != nil:
			return nil, fmt.Errorf("go list: package %s: %s", p.ImportPath, p.Error.Err)
		case len(p.DepsErrors) > 0:
			msg := ""
			if p.DepsErrors[0] != nil {
				msg = p.DepsErrors[0].Err
			}
			return nil, fmt.Errorf("go list: package %s: dependency error: %s", p.ImportPath, msg)
		case p.Incomplete:
			return nil, fmt.Errorf("go list: package %s: incomplete", p.ImportPath)
		case p.Module == nil || p.Module.Path == "" || p.Module.Dir == "":
			return nil, fmt.Errorf("go list: package %s has no module", p.ImportPath)
		}
		pkgs = append(pkgs, p)
	}
	return pkgs, nil
}

// pkgBuild accumulates one package while the graph is assembled.
type pkgBuild struct {
	Package
	imports map[string]bool
}

func buildGraph(ctx context.Context, root string, uses []string, listed []goListPackage) (*Graph, error) {
	byPath := map[string]*pkgBuild{}
	byDir := map[string]*pkgBuild{}
	modPath := map[string]string{} // module dir -> module path
	for _, lp := range listed {
		dir, err := relDir(root, lp.Dir)
		if err != nil {
			return nil, fmt.Errorf("go list: package %s: %w", lp.ImportPath, err)
		}
		modDir, err := relDir(root, lp.Module.Dir)
		if err != nil {
			return nil, fmt.Errorf("go list: module of %s: %w", lp.ImportPath, err)
		}
		modPath[modDir] = lp.Module.Path
		b := &pkgBuild{Package: Package{ImportPath: lp.ImportPath, Dir: dir, ModuleDir: modDir}, imports: map[string]bool{}}
		for _, set := range [][]string{lp.Imports, lp.TestImports, lp.XTestImports} {
			for _, imp := range set {
				b.imports[imp] = true
			}
		}
		byPath[lp.ImportPath] = b
		byDir[dir] = b
	}

	// The go/parser pass: every .go file of every module directory, build
	// constraints ignored.
	for _, use := range uses {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("parse imports: %w", err)
		}
		mp, ok := modPath[use]
		if !ok {
			return nil, fmt.Errorf("module %s: go list reported no packages for it", use)
		}
		dirs, err := goFileDirs(filepath.Join(root, filepath.FromSlash(use)))
		if err != nil {
			return nil, fmt.Errorf("module %s: %w", use, err)
		}
		for _, d := range dirs {
			rel, err := relDir(root, d.dir)
			if err != nil {
				return nil, err
			}
			b, ok := byDir[rel]
			if !ok {
				ip := mp
				switch {
				case rel == use:
				case use == ".":
					ip = mp + "/" + rel
				default:
					ip = mp + "/" + strings.TrimPrefix(rel, use+"/")
				}
				b = &pkgBuild{Package: Package{ImportPath: ip, Dir: rel, ModuleDir: use}, imports: map[string]bool{}}
				byPath[ip] = b
				byDir[rel] = b
			}
			for _, f := range d.files {
				imps, err := parseImports(f)
				if err != nil {
					return nil, err
				}
				for _, imp := range imps {
					b.imports[imp] = true
				}
			}
		}
	}

	pkgs := make([]Package, 0, len(byPath))
	for _, ip := range sortedKeys(byPath) {
		b := byPath[ip]
		var imps []string
		for _, imp := range sortedKeys(b.imports) {
			if _, ok := byPath[imp]; ok {
				imps = append(imps, imp)
				continue
			}
			for _, m := range modPath {
				if imp == m || strings.HasPrefix(imp, m+"/") {
					return nil, fmt.Errorf("package %s imports %s, which is in workspace module %s but is no package of it", ip, imp, m)
				}
			}
		}
		b.Imports = imps
		pkgs = append(pkgs, b.Package)
	}
	return NewGraph(pkgs), nil
}

type goDir struct {
	dir   string
	files []string
}

// goFileDirs walks one module directory the way `./...` does, without build
// constraints: it skips testdata, vendor, directories and files beginning
// with "." or "_", and nested module roots.
func goFileDirs(modRoot string) ([]goDir, error) {
	var out []goDir
	err := filepath.WalkDir(modRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		if p != modRoot {
			name := d.Name()
			if name == "testdata" || name == "vendor" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
				return filepath.SkipDir
			}
			if fileExists(filepath.Join(p, "go.mod")) {
				return filepath.SkipDir
			}
		}
		entries, err := os.ReadDir(p)
		if err != nil {
			return err
		}
		var files []string
		for _, e := range entries {
			n := e.Name()
			if e.Type().IsRegular() && strings.HasSuffix(n, ".go") && !strings.HasPrefix(n, ".") && !strings.HasPrefix(n, "_") {
				files = append(files, filepath.Join(p, n))
			}
		}
		if len(files) > 0 {
			sort.Strings(files)
			out = append(out, goDir{dir: p, files: files})
		}
		return nil
	})
	return out, err
}

func parseImports(file string) ([]string, error) {
	f, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.ImportsOnly)
	if err != nil {
		return nil, fmt.Errorf("parse imports: %w", err)
	}
	out := make([]string, 0, len(f.Imports))
	for _, is := range f.Imports {
		ip, err := strconv.Unquote(is.Path.Value)
		if err != nil {
			return nil, fmt.Errorf("parse imports: %s: %w", file, err)
		}
		out = append(out, ip)
	}
	return out, nil
}

// relDir converts an absolute directory reported for the workspace into a
// repo-relative slash path, refusing one outside the workspace.
func relDir(root, dir string) (string, error) {
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		dir = real
	}
	rel, err := filepath.Rel(root, dir)
	if err != nil {
		return "", fmt.Errorf("directory %s: %w", dir, err)
	}
	rel = filepath.ToSlash(rel)
	if rel == ".." || strings.HasPrefix(rel, "../") || filepath.IsAbs(rel) {
		return "", fmt.Errorf("directory %s is outside the workspace %s", dir, root)
	}
	return rel, nil
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// tailBuffer keeps only the last max bytes written to it.
type tailBuffer struct {
	max int
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string { return string(t.buf) }
