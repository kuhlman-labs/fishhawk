// Command slicereachhelper is the in-image slice-gate reach loader (#5524):
// a STDLIB-ONLY port of runner/cmd/fishhawk-runner/slicereach.go that the
// runner embeds and runs inside the gate container, compiled by the image's
// own go, over the mounted clone. It imports nothing outside the standard
// library (TestHelper_StdlibOnly), so the lone file builds offline with no
// go.mod and no module cache.
//
// Usage: FISHHAWK_SLICE_REACH_NONCE=<nonce> slicereachhelper <root>
//
// It always ends with EXACTLY ONE stdout line
//
//	FISHHAWK_SLICE_REACH_RESULT <nonce> <json>
//
// and exits 0, for a graph and for a refusal alike. The JSON is
// {"reason":...} for a refusal (a fixed token, never go-command, parser or
// file bytes) or {"imports":{dir:[dir...]},"emittable":[dir...]} for a graph.
// A missing nonce or root is a usage error: exit 2 and no result line.
//
// Differences from the host loader: go.work and go.mod are read through
// `go work edit -json` and `go mod edit -json` (golang.org/x/mod/modfile is
// not stdlib), each after a size check; the host-only runner-log sink and env
// sanitizer are replaced by stderr and the container exec's own env.
package main

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
	"syscall"
	"time"
)

// Tunables. Package vars so a test can lower a bound. A test substitutes go
// through PATH: runGo execs the literal "go" so the runner's exec-site guard
// (TestNoAgentSpawnOutsideAdapters) classifies it as a non-agent program.
var (
	// loadTimeout is the helper's ONLY time bound; it equals the host
	// loader's sliceReachLoadTimeout and starts before the root is touched.
	loadTimeout = 2 * time.Minute
	// metadataCap bounds one go.work / go.mod file.
	metadataCap int64 = 1 << 20
	// stdoutCap bounds one go subprocess's stdout.
	stdoutCap int64 = 64 << 20
)

const (
	resultMarker = "FISHHAWK_SLICE_REACH_RESULT"
	nonceEnv     = "FISHHAWK_SLICE_REACH_NONCE"

	waitDelay  = 2 * time.Second
	stderrTail = 2048
	// detailMax bounds the clone-derived detail (a path or an import path)
	// a reason token carries.
	detailMax = 256

	reasonUnsupported = "reach_unsupported_workspace"
	reasonFailed      = "reach_load_failed:"
	reasonTimeout     = reasonFailed + "load_timeout"
)

// metadataFiles are the module metadata files the Lstat predicate covers
// (gateiso.RequireRegularModuleMetadata's seedMetadataFiles).
var metadataFiles = []string{"go.mod", "go.sum", "go.work", "go.work.sum"}

// result is the one JSON document on the result line.
type result struct {
	Reason    string              `json:"reason,omitempty"`
	Imports   map[string][]string `json:"imports,omitempty"`
	Emittable []string            `json:"emittable,omitempty"`
}

// record is one `go list -json` package record.
type record struct {
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

// goWork and goMod are the `go work edit -json` / `go mod edit -json` shapes
// the helper reads (unknown fields are ignored).
type goWork struct {
	Use     []struct{ DiskPath string }
	Replace []replace
}

type goMod struct {
	Module  struct{ Path string }
	Replace []replace
}

type replace struct {
	New struct{ Path string }
}

// use is one workspace module: its root-relative dir, real dir and path.
type use struct {
	rel, abs, modPath string
}

func main() { os.Exit(run(os.Args[1:], os.Getenv, os.Stdout, os.Stderr)) }

// run is the testable entry point.
func run(args []string, env func(string) string, stdout, stderr io.Writer) int {
	nonce := env(nonceEnv)
	if len(args) != 1 || args[0] == "" || !nonceOK(nonce) {
		_, _ = fmt.Fprintf(stderr, "usage: %s=<nonce> slicereachhelper <root>\n", nonceEnv)
		return 2
	}
	imports, emittable, reason := load(context.Background(), args[0], stderr)
	res := result{Reason: reason}
	if reason == "" {
		res.Imports = imports
		res.Emittable = sortedKeys(emittable)
	}
	body, err := json.Marshal(res)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "slicereachhelper: encode result: %v\n", err)
		return 1
	}
	if _, err := fmt.Fprintf(stdout, "%s %s %s\n", resultMarker, nonce, body); err != nil {
		return 1
	}
	return 0
}

// nonceOK admits a non-empty nonce with no space or control character, so it
// cannot break the single-line frame.
func nonceOK(nonce string) bool {
	if nonce == "" {
		return false
	}
	for _, r := range nonce {
		if r <= ' ' || r == 0x7f {
			return false
		}
	}
	return true
}

// detail bounds a clone-derived token detail.
func detail(s string) string {
	if len(s) > detailMax {
		return s[:detailMax]
	}
	return s
}

// refused is the metadata-predicate refusal token for one site.
func refused(site string) string {
	return reasonUnsupported + ":metadata_not_regular:" + site
}

// requireRegular refuses any metadataFiles entry in dir that exists but is
// not a regular file (Lstat: a symlink is refused, never followed).
func requireRegular(dir string) error {
	for _, name := range metadataFiles {
		st, err := os.Lstat(filepath.Join(dir, name))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if !st.Mode().IsRegular() {
			return fmt.Errorf("%s is not a regular file", name)
		}
	}
	return nil
}

// load loads the reach graph of the clone at wt. A non-empty reason means the
// graph is unusable and the gate must run full.
func load(ctx context.Context, wt string, stderr io.Writer) (map[string][]string, map[string]bool, string) {
	// FIRST, before anything touches the root: one bound over all of it.
	ctx, cancel := context.WithTimeout(ctx, loadTimeout)
	defer cancel()

	root, uses, gowork, reason := workspace(ctx, wt, stderr)
	if reason != "" {
		return nil, nil, reason
	}
	walked, reason := walk(ctx, root, uses)
	if reason != "" {
		return nil, nil, reason
	}
	records, reason := goList(ctx, root, gowork, uses, stderr)
	if reason != "" {
		return nil, nil, reason
	}
	return assemble(root, records, walked)
}

// workspace resolves the root, refuses non-regular metadata, reads go.work /
// go.mod and enforces use/replace containment. It returns the uses and the
// GOWORK value (the root go.work or "off").
func workspace(ctx context.Context, wt string, stderr io.Writer) (root string, uses []use, gowork, reason string) {
	abs, err := filepath.Abs(wt)
	if err != nil {
		return "", nil, "", reasonUnsupported
	}
	root, err = filepath.EvalSymlinks(abs)
	if err != nil {
		return "", nil, "", reasonUnsupported
	}
	if st, err := os.Lstat(root); err != nil || !st.IsDir() {
		return "", nil, "", reasonUnsupported
	}
	if requireRegular(root) != nil {
		return "", nil, "", refused("root")
	}
	if err := ctx.Err(); err != nil {
		return "", nil, "", reasonTimeout
	}

	workFile := filepath.Join(root, "go.work")
	var useRels []string
	gowork = "off"
	switch {
	case exists(workFile):
		gowork = workFile
		var wf goWork
		if r := readMetadata(ctx, root, workFile, "work", gowork, &wf, stderr); r != "" {
			return "", nil, "", r
		}
		for _, u := range wf.Use {
			rel := path.Clean(filepath.ToSlash(u.DiskPath))
			if !contained(root, rel) {
				return "", nil, "", reasonUnsupported
			}
			useRels = append(useRels, rel)
		}
		if len(useRels) == 0 {
			return "", nil, "", reasonUnsupported
		}
		for _, r := range wf.Replace {
			if r := checkReplace(root, root, r.New.Path); r != "" {
				return "", nil, "", r
			}
		}
	case exists(filepath.Join(root, "go.mod")):
		useRels = []string{"."}
	default:
		return "", nil, "", reasonUnsupported
	}

	for _, u := range useRels {
		// Contained above; resolve to the real in-root dir the walk descends.
		real, err := filepath.EvalSymlinks(filepath.Join(root, filepath.FromSlash(u)))
		if err != nil {
			return "", nil, "", reasonUnsupported
		}
		rel, _ := relTo(root, real)
		// The root's own metadata was refused or admitted above; a use
		// elsewhere gets its own check before its go.mod is read.
		if real != root && requireRegular(real) != nil {
			return "", nil, "", refused("use")
		}
		modFile := filepath.Join(real, "go.mod")
		var mf goMod
		if r := readMetadata(ctx, root, modFile, "mod", "off", &mf, stderr); r != "" {
			return "", nil, "", r
		}
		if mf.Module.Path == "" {
			return "", nil, "", reasonFailed + "gomod_unreadable:" + detail(path.Join(rel, "go.mod"))
		}
		for _, rp := range mf.Replace {
			if r := checkReplace(root, real, rp.New.Path); r != "" {
				return "", nil, "", r
			}
		}
		uses = append(uses, use{rel: rel, abs: real, modPath: mf.Module.Path})
	}
	return root, uses, gowork, ""
}

// readMetadata size-checks one metadata file, then decodes the JSON that
// `go <kind> edit -json <file>` prints for it into out. Every failure is a
// fixed token naming the root-relative file.
func readMetadata(ctx context.Context, root, file, kind, gowork string, out any, stderr io.Writer) string {
	rel := detail(lexRel(root, file))
	if err := ctx.Err(); err != nil {
		return reasonTimeout
	}
	st, err := os.Stat(file)
	if err != nil {
		return reasonFailed + "gomod_unreadable:" + rel
	}
	if st.Size() > metadataCap {
		return reasonFailed + "metadata_too_large:" + rel
	}
	data, status := runGo(ctx, root, gowork, []string{kind, "edit", "-json", file}, "slice_reach_metadata_failed", stderr)
	switch status {
	case goTimedOut:
		return reasonTimeout
	case goOverflow, goFailed:
		return reasonFailed + "gomod_unreadable:" + rel
	}
	if err := json.Unmarshal(data, out); err != nil {
		return reasonFailed + "gomod_unreadable:" + rel
	}
	return ""
}

// contained reports whether a cleaned slash use path is relative, carries no
// `..` segment and resolves (EvalSymlinks) inside root.
func contained(root, rel string) bool {
	if rel == "" || path.IsAbs(rel) || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, "../") {
		return false
	}
	real, err := filepath.EvalSymlinks(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		return false
	}
	if st, err := os.Lstat(real); err != nil || !st.IsDir() {
		return false
	}
	_, ok := relTo(root, real)
	return ok
}

// isDirectoryPath equals golang.org/x/mod/modfile.IsDirectoryPath: a path
// that starts with ./ or ../ (or is . or ..), or is rooted.
func isDirectoryPath(ns string) bool {
	return ns == "." || strings.HasPrefix(ns, "./") || strings.HasPrefix(ns, `.\`) ||
		ns == ".." || strings.HasPrefix(ns, "../") || strings.HasPrefix(ns, `..\`) ||
		strings.HasPrefix(ns, "/") || strings.HasPrefix(ns, `\`) ||
		len(ns) >= 2 && ('A' <= ns[0] && ns[0] <= 'Z' || 'a' <= ns[0] && ns[0] <= 'z') && ns[1] == ':'
}

// checkReplace refuses a filesystem replace target (relative to base) that
// resolves outside root, then applies the metadata predicate to it. A
// module-path replacement needs no check.
func checkReplace(root, base, target string) string {
	if !isDirectoryPath(target) {
		return ""
	}
	p := target
	if !filepath.IsAbs(p) {
		p = filepath.Join(base, filepath.FromSlash(p))
	}
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		return reasonUnsupported
	}
	if _, ok := relTo(root, real); !ok {
		return reasonUnsupported
	}
	if requireRegular(real) != nil {
		return refused("replace")
	}
	return ""
}

// exists reports whether p exists (Lstat: a dangling link counts).
func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// lexRel is the LEXICAL root-relative slash path of p (no symlink
// resolution: a token names the clone path, never a link's target).
func lexRel(root, p string) string {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return filepath.Base(p)
	}
	return filepath.ToSlash(rel)
}

// relTo maps an absolute path to a root-relative slash path; ok is false for
// a path outside root. The path is EvalSymlinks'd when it resolves.
func relTo(root, p string) (string, bool) {
	if real, err := filepath.EvalSymlinks(p); err == nil {
		p = real
	}
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return p, false
	}
	rel = filepath.ToSlash(rel)
	if filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, "../") {
		return rel, false
	}
	return rel, true
}

// walk is the constraint-agnostic walk: every .go file of every use, build
// constraints ignored, _test.go included. It returns each visited package
// dir and the workspace dirs it imports.
func walk(ctx context.Context, root string, uses []use) (map[string][]string, string) {
	rawImports := map[string]map[string]bool{} // rel dir -> raw import paths
	dirUse := map[string]use{}
	for _, u := range uses {
		reason := ""
		err := filepath.WalkDir(u.abs, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				reason = reasonFailed + "walk_failed:" + detail(lexRel(root, p))
				return err
			}
			if cerr := ctx.Err(); cerr != nil {
				reason = reasonTimeout
				return cerr
			}
			name := d.Name()
			if d.IsDir() {
				if p == u.abs {
					return nil
				}
				if name == "testdata" || name == "vendor" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
					return filepath.SkipDir
				}
				if exists(filepath.Join(p, "go.mod")) {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(name, ".go") || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
				return nil
			}
			rel := lexRel(root, filepath.Dir(p))
			imps, ok := parseFile(p)
			if !ok {
				reason = reasonFailed + "unparsable_go_file:" + detail(path.Join(rel, name))
				return errors.New(reason)
			}
			set := rawImports[rel]
			if set == nil {
				set = map[string]bool{}
				rawImports[rel] = set
				dirUse[rel] = u
			}
			for _, imp := range imps {
				set[imp] = true
			}
			return nil
		})
		if err != nil {
			if reason == "" {
				reason = reasonFailed + "walk_failed:" + detail(u.rel)
			}
			return nil, reason
		}
	}

	// Each walked dir's import path, from its module's path.
	ipToDir := map[string]string{}
	for rel, u := range dirUse {
		ip := u.modPath
		switch {
		case rel == u.rel:
		case u.rel == ".":
			ip = u.modPath + "/" + rel
		default:
			ip = u.modPath + "/" + strings.TrimPrefix(rel, u.rel+"/")
		}
		ipToDir[ip] = rel
	}
	walked := map[string][]string{}
	for _, rel := range sortedKeys(rawImports) {
		edges := []string{}
		for _, imp := range sortedKeys(rawImports[rel]) {
			if to, ok := ipToDir[imp]; ok {
				if to != rel {
					edges = append(edges, to)
				}
				continue
			}
			for _, u := range uses {
				if imp == u.modPath || strings.HasPrefix(imp, u.modPath+"/") {
					return nil, reasonFailed + "import_not_package:" + detail(imp)
				}
			}
		}
		walked[rel] = edges
	}
	return walked, ""
}

// parseFile Lstats one candidate .go file (a symlink or any non-regular file
// is refused before it is opened) and parses its import clause, build
// constraints ignored.
func parseFile(p string) ([]string, bool) {
	st, err := os.Lstat(p)
	if err != nil || !st.Mode().IsRegular() {
		return nil, false
	}
	f, err := parser.ParseFile(token.NewFileSet(), p, nil, parser.ImportsOnly)
	if err != nil {
		return nil, false
	}
	out := make([]string, 0, len(f.Imports))
	for _, is := range f.Imports {
		ip, err := strconv.Unquote(is.Path.Value)
		if err != nil {
			return nil, false
		}
		out = append(out, ip)
	}
	return out, true
}

// pinnedEnv is appended LAST to every go subprocess's env, so each value
// wins: the host loader's sliceReachPinnedEnv, value for value.
func pinnedEnv(gowork string) []string {
	return []string{
		"GOWORK=" + gowork,
		"GOPROXY=off",
		"GOTOOLCHAIN=local",
		"GOFLAGS=",
		"CGO_ENABLED=0",
		"GONOPROXY=",
		"GOPRIVATE=",
		"GONOSUMDB=",
		"GONOSUMCHECK=",
		"GOINSECURE=",
		"GOENV=off",
		"GOCACHEPROG=",
		"GOSUMDB=off",
	}
}

// cappedWriter keeps at most max bytes; past it, it records the overflow and
// cancels the subprocess so its process group dies early.
type cappedWriter struct {
	buf      bytes.Buffer
	max      int64
	overflow bool
	cancel   context.CancelFunc
}

func (w *cappedWriter) Write(p []byte) (int, error) {
	if w.overflow {
		return len(p), nil
	}
	if int64(w.buf.Len())+int64(len(p)) > w.max {
		w.overflow = true
		w.cancel()
		return len(p), nil
	}
	return w.buf.Write(p)
}

// tail keeps only the last max bytes written to it.
type tail struct {
	max int
	buf []byte
}

func (t *tail) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
	}
	return len(p), nil
}

// goStatus is how one go subprocess ended.
type goStatus int

const (
	goOK goStatus = iota
	goTimedOut
	goOverflow
	goFailed
)

// runGo runs one bounded go subprocess in root under the pinned env. The
// stderr tail goes to the helper's stderr under event, never into a reason.
func runGo(ctx context.Context, root, gowork string, args []string, event string, stderr io.Writer) ([]byte, goStatus) {
	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()
	cmd := exec.CommandContext(runCtx, "go", args...)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), pinnedEnv(gowork)...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return nil
	}
	cmd.WaitDelay = waitDelay
	so := &cappedWriter{max: stdoutCap, cancel: runCancel}
	se := &tail{max: stderrTail}
	cmd.Stdout = so
	cmd.Stderr = se
	runErr := cmd.Run()
	switch {
	case so.overflow:
		return nil, goOverflow
	case ctx.Err() != nil:
		// The load bound stopped the subprocess.
		return nil, goTimedOut
	case runErr != nil:
		_, _ = fmt.Fprintf(stderr, "%s: %s\n", event, strings.TrimSpace(string(se.buf)))
		return nil, goFailed
	}
	return so.buf.Bytes(), goOK
}

// goList runs the ONE `go list -e -json=...` over every use in root and
// decodes its records.
func goList(ctx context.Context, root, gowork string, uses []use, stderr io.Writer) ([]record, string) {
	args := []string{"list", "-e", "-json=ImportPath,Dir,Module,Imports,TestImports,XTestImports,Error,DepsErrors,Incomplete"}
	for _, u := range uses {
		if u.rel == "." {
			args = append(args, "./...")
		} else {
			args = append(args, "./"+u.rel+"/...")
		}
	}
	out, status := runGo(ctx, root, gowork, args, "slice_reach_go_list_failed", stderr)
	switch status {
	case goOverflow:
		return nil, reasonFailed + "go_list_stdout_cap"
	case goTimedOut:
		return nil, reasonTimeout
	case goFailed:
		return nil, reasonFailed + "go_list_exit"
	}
	dec := json.NewDecoder(bytes.NewReader(out))
	var records []record
	for {
		var r record
		err := dec.Decode(&r)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, reasonFailed + "go_list_malformed_json"
		}
		records = append(records, r)
	}
	return records, ""
}

// recordError reports whether go list marked the record unusable.
func recordError(r record) bool {
	return r.Error != nil || len(r.DepsErrors) > 0 || r.Incomplete
}

// assemble maps every go list record to a root-relative dir, refuses a
// record that is errored, moduleless, outside the clone or not visited by the
// walk, and unions the go list edges with the walk's. emittable is the
// go-list-reported dirs; a walked-only dir is a traversal-only node.
func assemble(root string, records []record, walked map[string][]string) (map[string][]string, map[string]bool, string) {
	if len(records) == 0 {
		return nil, nil, reasonFailed + "no_packages"
	}
	ipToDir := map[string]string{}
	dirs := make([]string, len(records))
	for i, r := range records {
		ip := detail(r.ImportPath)
		if recordError(r) {
			return nil, nil, reasonFailed + "package_error:" + ip
		}
		if r.Module == nil || r.Module.Path == "" || r.Module.Dir == "" {
			return nil, nil, reasonFailed + "no_module:" + ip
		}
		dir, ok := relTo(root, r.Dir)
		if !ok {
			return nil, nil, "reach_dir_outside_clone:" + ip
		}
		if _, ok := relTo(root, r.Module.Dir); !ok {
			return nil, nil, "reach_dir_outside_clone:" + ip
		}
		if _, ok := walked[dir]; !ok {
			return nil, nil, reasonFailed + "unwalked_package_dir:" + ip
		}
		ipToDir[r.ImportPath] = dir
		dirs[i] = dir
	}
	edges := map[string]map[string]bool{}
	for dir, to := range walked {
		set := map[string]bool{}
		for _, t := range to {
			set[t] = true
		}
		edges[dir] = set
	}
	emittable := map[string]bool{}
	for i, r := range records {
		dir := dirs[i]
		emittable[dir] = true
		for _, set := range [][]string{r.Imports, r.TestImports, r.XTestImports} {
			for _, imp := range set {
				if to, ok := ipToDir[imp]; ok && to != dir {
					edges[dir][to] = true
				}
			}
		}
	}
	imports := make(map[string][]string, len(edges))
	for dir, set := range edges {
		imports[dir] = sortedKeys(set)
	}
	return imports, emittable, ""
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
