package main

// slicereach.go is the slice-gate reach loader (#5498, slice 3): it loads the
// import graph of a decomposition child's materialized committed-tree clone as
// PRIMITIVE values — imports (root-relative dir -> the root-relative dirs it
// imports, every node a key, traversal-only nodes included) and emittable (the
// dirs `go list` reports, the only ones a scoped `./<dir>` pattern can test) —
// for sliceGateResolveReach (slicegate.go) to close over. It shares no symbol
// with slicegate.go; slicegatehook.go composes the two.
//
// Every refusal returns a FIXED reason token (never go-command, parser or
// file bytes) and the caller runs the FULL gate. The loader is a port of
// backend/internal/basedelta/golist.go (the runner module cannot import
// backend/internal): the same go list field set, the same widening on Error,
// DepsErrors or Incomplete and the same five pinned env values, plus:
//
//   - ONE time bound (sliceReachLoadTimeout), started before the clone is
//     touched, over every metadata read, the walk and the go list (S4/T1);
//   - the shared Lstat predicate (gateiso.RequireRegularModuleMetadata) before
//     any metadata read at the root, at every `use` dir and at every
//     filesystem `replace` target (T1), and capped, ctx-bounded metadata reads;
//   - use/replace containment inside the EvalSymlinks'd clone root (R4);
//   - a constraint-agnostic go/parser walk that Lstats every .go file (S9/R3);
//   - the egress pins of approval condition Q1 (no GOPRIVATE/GONOPROXY direct
//     fetch, no go env file, no GOCACHEPROG).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

	"golang.org/x/mod/modfile"

	"github.com/kuhlman-labs/fishhawk/runner/internal/gateiso"
)

// Reach-loader tunables. Package vars so a test can lower a bound.
var (
	// sliceReachLoadTimeout is the loader's ONLY time bound besides the
	// caller's cancellation; it equals basedelta.DefaultLoadTimeout. The load
	// runs after the #4023 gate budget is computed, so it is neither charged
	// to nor clipped by that budget.
	sliceReachLoadTimeout = 2 * time.Minute
	// sliceReachMetadataCap bounds one go.work / go.mod read.
	sliceReachMetadataCap int64 = 1 << 20
	// sliceReachStdoutCap bounds the go list stdout.
	sliceReachStdoutCap int64 = 64 << 20
	// sliceReachGoBin is the go binary; empty resolves "go" on PATH.
	sliceReachGoBin = ""
	// sliceReachRequireRegularFn is the T1 metadata predicate seam.
	sliceReachRequireRegularFn = gateiso.RequireRegularModuleMetadata
	// sliceReachLogSink receives the loader's runner-log lines (the go list
	// stderr tail never reaches a reason).
	sliceReachLogSink io.Writer = os.Stderr
)

const (
	sliceReachWaitDelay  = 2 * time.Second
	sliceReachStderrTail = 2048
	// sliceReachDetailMax bounds the clone-derived detail (a path or an
	// import path) a reason token carries.
	sliceReachDetailMax = 256

	sliceReachUnsupported = "reach_unsupported_workspace"
	sliceReachFailed      = "reach_load_failed:"
	sliceReachTimeout     = sliceReachFailed + "load_timeout"
)

// sliceReachRecord is one `go list -json` package record.
type sliceReachRecord struct {
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

// sliceReachUse is one workspace module: its root-relative dir and path.
type sliceReachUse struct {
	rel, abs, modPath string
}

// sliceReachDetail bounds a clone-derived token detail.
func sliceReachDetail(s string) string {
	if len(s) > sliceReachDetailMax {
		return s[:sliceReachDetailMax]
	}
	return s
}

// sliceReachRefused is the T1 refusal token for one site; it names the site
// only, never the predicate's error text or a path taken from it.
func sliceReachRefused(site string) string {
	reason := sliceReachUnsupported + ":metadata_not_regular:" + site
	logEvent(sliceReachLogSink, "slice_reach_metadata_refused", map[string]string{"reason": reason})
	return reason
}

// sliceReachLoad loads the reach graph of the clone at wt. A non-empty reason
// means the graph is unusable and the gate must run full.
func sliceReachLoad(ctx context.Context, wt string) (imports map[string][]string, emittable map[string]bool, reason string) {
	// FIRST, before anything touches the clone (T1): one bound over all of it.
	ctx, cancel := context.WithTimeout(ctx, sliceReachLoadTimeout)
	defer cancel()

	root, uses, gowork, reason := sliceReachWorkspace(ctx, wt)
	if reason != "" {
		return nil, nil, reason
	}
	walked, reason := sliceReachWalk(ctx, root, uses)
	if reason != "" {
		return nil, nil, reason
	}
	records, reason := sliceReachGoList(ctx, root, gowork, uses)
	if reason != "" {
		return nil, nil, reason
	}
	return sliceReachAssemble(root, records, walked)
}

// sliceReachWorkspace resolves the clone root, refuses non-regular metadata
// (T1), parses go.work / go.mod and enforces use/replace containment (R4). It
// returns the uses and the GOWORK value (the root go.work or "off").
func sliceReachWorkspace(ctx context.Context, wt string) (root string, uses []sliceReachUse, gowork, reason string) {
	abs, err := filepath.Abs(wt)
	if err != nil {
		return "", nil, "", sliceReachUnsupported
	}
	root, err = filepath.EvalSymlinks(abs)
	if err != nil {
		return "", nil, "", sliceReachUnsupported
	}
	if st, err := os.Lstat(root); err != nil || !st.IsDir() {
		return "", nil, "", sliceReachUnsupported
	}
	if sliceReachRequireRegularFn(root) != nil {
		return "", nil, "", sliceReachRefused("root")
	}
	if err := ctx.Err(); err != nil {
		return "", nil, "", sliceReachTimeout
	}

	workFile := filepath.Join(root, "go.work")
	var useRels []string
	gowork = "off"
	switch {
	case sliceReachExists(workFile):
		gowork = workFile
		data, r := sliceReachReadMetadata(ctx, root, workFile)
		if r != "" {
			return "", nil, "", r
		}
		wf, err := modfile.ParseWork(workFile, data, sliceReachKeepVersion)
		if err != nil {
			return "", nil, "", sliceReachFailed + "gomod_unreadable:go.work"
		}
		for _, u := range wf.Use {
			use := path.Clean(filepath.ToSlash(u.Path))
			if !sliceReachContained(root, use) {
				return "", nil, "", sliceReachUnsupported
			}
			useRels = append(useRels, use)
		}
		if len(useRels) == 0 {
			return "", nil, "", sliceReachUnsupported
		}
		for _, r := range wf.Replace {
			if r := sliceReachCheckReplace(root, root, r.New.Path); r != "" {
				return "", nil, "", r
			}
		}
	case sliceReachExists(filepath.Join(root, "go.mod")):
		useRels = []string{"."}
	default:
		return "", nil, "", sliceReachUnsupported
	}

	for _, use := range useRels {
		// Contained above; resolve to the real in-root dir the walk descends.
		real, err := filepath.EvalSymlinks(filepath.Join(root, filepath.FromSlash(use)))
		if err != nil {
			return "", nil, "", sliceReachUnsupported
		}
		rel, _ := sliceReachRel(root, real)
		// The root's own metadata was refused or admitted above; a use
		// elsewhere gets its own check before its go.mod is read.
		if real != root && sliceReachRequireRegularFn(real) != nil {
			return "", nil, "", sliceReachRefused("use")
		}
		modFile := filepath.Join(real, "go.mod")
		modRel := path.Join(rel, "go.mod")
		data, r := sliceReachReadMetadata(ctx, root, modFile)
		if r != "" {
			return "", nil, "", r
		}
		mf, err := modfile.Parse(modFile, data, sliceReachKeepVersion)
		if err != nil || mf.Module == nil || mf.Module.Mod.Path == "" {
			return "", nil, "", sliceReachFailed + "gomod_unreadable:" + sliceReachDetail(modRel)
		}
		for _, rp := range mf.Replace {
			if r := sliceReachCheckReplace(root, real, rp.New.Path); r != "" {
				return "", nil, "", r
			}
		}
		uses = append(uses, sliceReachUse{rel: rel, abs: real, modPath: mf.Module.Mod.Path})
	}
	return root, uses, gowork, ""
}

// sliceReachContained reports whether a cleaned slash use path is relative,
// carries no `..` segment and resolves (EvalSymlinks) inside root.
func sliceReachContained(root, use string) bool {
	if use == "" || path.IsAbs(use) || filepath.IsAbs(use) || use == ".." || strings.HasPrefix(use, "../") {
		return false
	}
	real, err := filepath.EvalSymlinks(filepath.Join(root, filepath.FromSlash(use)))
	if err != nil {
		return false
	}
	if st, err := os.Lstat(real); err != nil || !st.IsDir() {
		return false
	}
	_, ok := sliceReachRel(root, real)
	return ok
}

// sliceReachCheckReplace refuses a filesystem replace target (relative to
// base) that resolves outside root, then applies the T1 predicate to it. A
// module-path replacement needs no check.
func sliceReachCheckReplace(root, base, target string) string {
	if !modfile.IsDirectoryPath(target) {
		return ""
	}
	p := target
	if !filepath.IsAbs(p) {
		p = filepath.Join(base, filepath.FromSlash(p))
	}
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		return sliceReachUnsupported
	}
	if _, ok := sliceReachRel(root, real); !ok {
		return sliceReachUnsupported
	}
	if sliceReachRequireRegularFn(real) != nil {
		return sliceReachRefused("replace")
	}
	return ""
}

// sliceReachKeepVersion is the modfile VersionFixer that keeps every version
// as written (the check reads directive STRUCTURE, not version hygiene).
func sliceReachKeepVersion(_, version string) (string, error) { return version, nil }

// sliceReachExists reports whether p exists (Lstat: a dangling link counts).
func sliceReachExists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// sliceReachReadMetadata reads one metadata file, at most
// sliceReachMetadataCap+1 bytes, in a goroutine bounded by ctx: a FIFO's open
// or an endless stream cannot hold the load past its bound.
func sliceReachReadMetadata(ctx context.Context, root, p string) ([]byte, string) {
	rel := sliceReachLexRel(root, p)
	if err := ctx.Err(); err != nil {
		return nil, sliceReachTimeout
	}
	limit := sliceReachMetadataCap
	type result struct {
		data []byte
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		f, err := os.Open(p)
		if err != nil {
			ch <- result{err: err}
			return
		}
		defer func() { _ = f.Close() }()
		data, err := io.ReadAll(io.LimitReader(f, limit+1))
		ch <- result{data: data, err: err}
	}()
	select {
	case <-ctx.Done():
		return nil, sliceReachTimeout
	case r := <-ch:
		if r.err != nil {
			return nil, sliceReachFailed + "gomod_unreadable:" + sliceReachDetail(rel)
		}
		if int64(len(r.data)) > limit {
			return nil, sliceReachFailed + "metadata_too_large:" + sliceReachDetail(rel)
		}
		return r.data, ""
	}
}

// sliceReachLexRel is the LEXICAL root-relative slash path of p (no symlink
// resolution: a token names the clone path, never a link's target).
func sliceReachLexRel(root, p string) string {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return filepath.Base(p)
	}
	return filepath.ToSlash(rel)
}

// sliceReachRel maps an absolute path to a root-relative slash path; ok is
// false for a path outside root (absolute or `..`-prefixed result). The path
// is EvalSymlinks'd when it resolves.
func sliceReachRel(root, p string) (string, bool) {
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

// sliceReachWalk is the constraint-agnostic walk (R3, S9): every .go file of
// every use, build constraints ignored, _test.go included. It returns each
// visited package dir and the workspace dirs it imports.
func sliceReachWalk(ctx context.Context, root string, uses []sliceReachUse) (map[string][]string, string) {
	rawImports := map[string]map[string]bool{} // rel dir -> raw import paths
	dirUse := map[string]sliceReachUse{}
	for _, u := range uses {
		reason := ""
		err := filepath.WalkDir(u.abs, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				reason = sliceReachFailed + "walk_failed:" + sliceReachDetail(sliceReachLexRel(root, p))
				return err
			}
			if cerr := ctx.Err(); cerr != nil {
				reason = sliceReachTimeout
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
				if sliceReachExists(filepath.Join(p, "go.mod")) {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(name, ".go") || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
				return nil
			}
			rel := sliceReachLexRel(root, filepath.Dir(p))
			fileRel := path.Join(rel, name)
			imps, ok := sliceReachParseFile(p)
			if !ok {
				reason = sliceReachFailed + "unparsable_go_file:" + sliceReachDetail(fileRel)
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
				reason = sliceReachFailed + "walk_failed:" + sliceReachDetail(u.rel)
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
	for _, rel := range sliceReachSortedKeys(rawImports) {
		var edges []string
		for _, imp := range sliceReachSortedKeys(rawImports[rel]) {
			if to, ok := ipToDir[imp]; ok {
				if to != rel {
					edges = append(edges, to)
				}
				continue
			}
			for _, u := range uses {
				if imp == u.modPath || strings.HasPrefix(imp, u.modPath+"/") {
					return nil, sliceReachFailed + "import_not_package:" + sliceReachDetail(imp)
				}
			}
		}
		walked[rel] = edges
	}
	return walked, ""
}

// sliceReachParseFile Lstats one candidate .go file (a symlink or any
// non-regular file is refused before it is opened) and parses its import
// clause, build constraints ignored. ok is false on any refusal or parse
// error; the caller reports a fixed token, never the parser text.
func sliceReachParseFile(p string) ([]string, bool) {
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

// sliceReachPinnedEnv is appended LAST to the sanitized env, so each value
// wins: the clone's own go.work (or none), no fetch, no toolchain switch, no
// inherited flags or cgo (basedelta parity) and, per approval condition Q1, no
// direct-fetch or insecure-fetch pattern (GONOPROXY/GOPRIVATE match a module
// that would otherwise be fetched DIRECT even under GOPROXY=off), no go env
// file and no GOCACHEPROG helper process.
func sliceReachPinnedEnv(gowork string) []string {
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
	}
}

// sliceReachCappedWriter keeps at most max bytes; past it, it records the
// overflow and cancels the go list so the process group dies early.
type sliceReachCappedWriter struct {
	buf      bytes.Buffer
	max      int64
	overflow bool
	cancel   context.CancelFunc
}

func (w *sliceReachCappedWriter) Write(p []byte) (int, error) {
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

// sliceReachTail keeps only the last max bytes written to it.
type sliceReachTail struct {
	max int
	buf []byte
}

func (t *sliceReachTail) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
	}
	return len(p), nil
}

// sliceReachGoList runs the ONE `go list -e -json=...` over every use in root
// and decodes its records.
func sliceReachGoList(ctx context.Context, root, gowork string, uses []sliceReachUse) ([]sliceReachRecord, string) {
	goBin := sliceReachGoBin
	if goBin == "" {
		var err error
		if goBin, err = exec.LookPath("go"); err != nil {
			return nil, sliceReachFailed + "go_list_exit"
		}
	}
	args := []string{"list", "-e", "-json=ImportPath,Dir,Module,Imports,TestImports,XTestImports,Error,DepsErrors,Incomplete"}
	for _, u := range uses {
		if u.rel == "." {
			args = append(args, "./...")
		} else {
			args = append(args, "./"+u.rel+"/...")
		}
	}
	listCtx, listCancel := context.WithCancel(ctx)
	defer listCancel()
	cmd := exec.CommandContext(listCtx, goBin, args...)
	cmd.Dir = root
	cmd.Env = append(sanitizeEnv(os.Environ()), sliceReachPinnedEnv(gowork)...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return nil
	}
	cmd.WaitDelay = sliceReachWaitDelay
	stdout := &sliceReachCappedWriter{max: sliceReachStdoutCap, cancel: listCancel}
	stderr := &sliceReachTail{max: sliceReachStderrTail}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	runErr := cmd.Run()
	switch {
	case stdout.overflow:
		return nil, sliceReachFailed + "go_list_stdout_cap"
	case ctx.Err() != nil:
		// The load bound (or the caller's cancellation) stopped the load.
		return nil, sliceReachTimeout
	case runErr != nil:
		logEvent(sliceReachLogSink, "slice_reach_go_list_failed", map[string]string{
			"reason":      sliceReachFailed + "go_list_exit",
			"stderr_tail": strings.TrimSpace(string(stderr.buf)),
		})
		return nil, sliceReachFailed + "go_list_exit"
	}
	dec := json.NewDecoder(bytes.NewReader(stdout.buf.Bytes()))
	var records []sliceReachRecord
	for {
		var r sliceReachRecord
		err := dec.Decode(&r)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, sliceReachFailed + "go_list_malformed_json"
		}
		records = append(records, r)
	}
	return records, ""
}

// sliceReachRecordError reports whether go list marked the record unusable.
func sliceReachRecordError(r sliceReachRecord) bool {
	return r.Error != nil || len(r.DepsErrors) > 0 || r.Incomplete
}

// sliceReachAssemble is the pure graph assembly: it maps every go list record
// to a root-relative dir, refuses a record that is errored, moduleless,
// outside the clone or not visited by the walk, and unions the go list edges
// with the walk's. emittable is the go-list-reported dirs; a walked-only dir
// (all files host-excluded) is a traversal-only node.
func sliceReachAssemble(root string, records []sliceReachRecord, walked map[string][]string) (map[string][]string, map[string]bool, string) {
	if len(records) == 0 {
		return nil, nil, sliceReachFailed + "no_packages"
	}
	ipToDir := map[string]string{}
	dirs := make([]string, len(records))
	for i, r := range records {
		ip := sliceReachDetail(r.ImportPath)
		if sliceReachRecordError(r) {
			return nil, nil, sliceReachFailed + "package_error:" + ip
		}
		if r.Module == nil || r.Module.Path == "" || r.Module.Dir == "" {
			return nil, nil, sliceReachFailed + "no_module:" + ip
		}
		dir, ok := sliceReachRel(root, r.Dir)
		if !ok {
			return nil, nil, "reach_dir_outside_clone:" + ip
		}
		if _, ok := sliceReachRel(root, r.Module.Dir); !ok {
			return nil, nil, "reach_dir_outside_clone:" + ip
		}
		if _, ok := walked[dir]; !ok {
			return nil, nil, sliceReachFailed + "unwalked_package_dir:" + ip
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
		imports[dir] = sliceReachSortedKeys(set)
	}
	return imports, emittable, ""
}

func sliceReachSortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
