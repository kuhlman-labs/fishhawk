package main

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/runner/internal/acceptenv"
	"github.com/kuhlman-labs/fishhawk/runner/internal/agent"
	"github.com/kuhlman-labs/fishhawk/runner/internal/agentenv"
	"github.com/kuhlman-labs/fishhawk/runner/internal/upload"
)

// The run-agent marker (#3945): every agent the runner spawns carries
// FISHHAWK_RUN_AGENT=<run id>, stamped LAST by the adapter's env composition
// (agent.WithRunAgentMarker). These tests drive REAL adapters against a fake
// agent binary that dumps its own environment, so they observe the env the
// spawned child actually received rather than the Invocation the runner built.

// productionConflictResolutionAgentInvoker is captured at package init, before
// any test swaps the seam, so the conflict-resolution row drives the REAL call
// site rather than whatever stub a sibling test left installed.
var productionConflictResolutionAgentInvoker = conflictResolutionAgentInvoker

// writeEnvDumpAgent writes a fake agent binary that dumps its environment to a
// per-call file (the path is baked into the script) and exits. It returns the
// binary path and the dump path. /usr/bin/env is absolute so the dump does not
// depend on the composed env carrying PATH.
func writeEnvDumpAgent(t *testing.T) (bin, dump string) {
	t.Helper()
	dir := t.TempDir()
	dump = filepath.Join(dir, "env.dump")
	bin = filepath.Join(dir, "fake-agent")
	script := "#!/bin/sh\n/usr/bin/env > '" + dump + "'\nexit 0\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil { //nolint:gosec // test fixture must be executable
		t.Fatalf("write fake agent: %v", err)
	}
	return bin, dump
}

// runAgentMarkerValues reads the dump and returns every FISHHAWK_RUN_AGENT
// value it holds, in order. A missing dump FAILS: it means the fake agent never
// ran, so an absent-marker assertion would be vacuous.
func runAgentMarkerValues(t *testing.T, dump string) []string {
	t.Helper()
	data, err := os.ReadFile(dump) //nolint:gosec // test-owned temp path
	if err != nil {
		t.Fatalf("the fake agent never wrote its env dump (%v); the spawn under test did not happen", err)
	}
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		if v, ok := strings.CutPrefix(line, agent.RunAgentEnvVar+"="); ok {
			out = append(out, v)
		}
	}
	return out
}

// assertSingleMarker requires exactly one FISHHAWK_RUN_AGENT entry equal to want.
func assertSingleMarker(t *testing.T, dump, want string) {
	t.Helper()
	got := runAgentMarkerValues(t, dump)
	if len(got) != 1 || got[0] != want {
		t.Errorf("child %s entries = %q, want exactly [%q] (the adapter must stamp the run-agent marker last on every spawn)",
			agent.RunAgentEnvVar, got, want)
	}
}

// selectInvokerCaseLabels parses agentselect.go and returns the string case
// labels of fnName's switch — every agent id selectInvoker can return an
// adapter for. An identifier label is resolved to its package-level string
// const value; any other label form is an error, as is a missing function or
// zero labels, so the enumeration can never pass vacuously.
func selectInvokerCaseLabels(fnName string) ([]string, error) {
	dir := pkgSrcDir
	fset := token.NewFileSet()
	consts := map[string]string{}
	var target *ast.FuncDecl
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", name, err)
		}
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Recv == nil && d.Name.Name == fnName && name == "agentselect.go" {
					target = d
				}
			case *ast.GenDecl:
				if d.Tok != token.CONST {
					continue
				}
				for _, spec := range d.Specs {
					vs := spec.(*ast.ValueSpec)
					for i, id := range vs.Names {
						if i >= len(vs.Values) {
							continue
						}
						if lit, ok := vs.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
							if v, err := strconv.Unquote(lit.Value); err == nil {
								consts[id.Name] = v
							}
						}
					}
				}
			}
		}
	}
	if target == nil {
		return nil, fmt.Errorf("func %s not found in agentselect.go", fnName)
	}
	var labels []string
	var labelErr error
	ast.Inspect(target.Body, func(n ast.Node) bool {
		cc, ok := n.(*ast.CaseClause)
		if !ok {
			return true
		}
		for _, expr := range cc.List {
			switch e := expr.(type) {
			case *ast.BasicLit:
				v, err := strconv.Unquote(e.Value)
				if err != nil || e.Kind != token.STRING {
					labelErr = fmt.Errorf("case label %s is not a string literal", e.Value)
					return false
				}
				labels = append(labels, v)
			case *ast.Ident:
				v, ok := consts[e.Name]
				if !ok {
					labelErr = fmt.Errorf("case label %s does not resolve to a package-level string const", e.Name)
					return false
				}
				labels = append(labels, v)
			default:
				labelErr = fmt.Errorf("unsupported case label form %T; extend selectInvokerCaseLabels", expr)
				return false
			}
		}
		return true
	})
	if labelErr != nil {
		return nil, labelErr
	}
	if len(labels) == 0 {
		return nil, fmt.Errorf("func %s has zero case labels; the enumeration would be vacuous", fnName)
	}
	return labels, nil
}

// TestEveryAgentSpawnCarriesRunAgentMarker enumerates every adapter
// selectInvoker can return (parsed from its case labels, so a new adapter
// enrolls automatically) and spawns each, through the REAL selectInvoker and
// the REAL env composers, across the runner's spawn shapes: the agentenv base
// (plan / implement / fix-up / base-rebase / conflict), the acceptenv base
// (acceptance), the acceptance shape under an ExecWrapper that execs the agent
// (the netsandbox route), and a nil base. An ambient stale marker is set on the
// parent throughout, so a stamp that is missing OR applied before an overlay
// reads as the wrong value, not just an absent one.
func TestEveryAgentSpawnCarriesRunAgentMarker(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake agent is a /bin/sh script")
	}
	if _, err := selectInvokerCaseLabels("noSuchSelectInvokerFunc"); err == nil {
		t.Fatal("selectInvokerCaseLabels accepted a missing function; the anti-vacuity guard is broken")
	}
	ids, err := selectInvokerCaseLabels("selectInvoker")
	if err != nil {
		t.Fatalf("enumerating selectInvoker's adapters: %v", err)
	}
	t.Setenv(agent.RunAgentEnvVar, "stale-parent")

	agentBase, _ := agentenv.Env(os.Environ())
	acceptBase, _ := acceptenv.Env(os.Environ(), "http://127.0.0.1:9")
	shapes := []struct {
		name string
		inv  agent.Invocation
	}{
		{"agentenv_base", agent.Invocation{BaseEnv: agentBase, Env: map[string]string{"FISHHAWK_BACKEND_URL": "http://127.0.0.1:9"}}},
		{"acceptenv_base", agent.Invocation{BaseEnv: acceptBase}},
		{"acceptenv_base_exec_wrapper", agent.Invocation{BaseEnv: acceptBase, ExecWrapper: []string{"/usr/bin/env"}}},
		{"nil_base_ambient_stale", agent.Invocation{}},
	}
	for _, id := range ids {
		for _, shape := range shapes {
			t.Run(id+"/"+shape.name, func(t *testing.T) {
				bin, dump := writeEnvDumpAgent(t)
				invoker, err := selectInvoker(id, "", bin)
				if err != nil {
					t.Fatalf("selectInvoker(%q): %v", id, err)
				}
				inv := shape.inv
				inv.RunID = "run-" + id + "-" + shape.name
				inv.Stage = "implement"
				inv.Prompt = "dump your env"
				inv.WorkingDir = t.TempDir()
				inv.Budget = agent.Budget{Timeout: 30 * time.Second}
				// The fake is not a real CLI, so the adapter's verdict is
				// meaningless here; only the env the child received is.
				_, _ = invoker.Invoke(context.Background(), inv)
				assertSingleMarker(t, dump, inv.RunID)
			})
		}
	}
}

// TestConflictResolutionSpawnCarriesRunAgentMarker drives the PRODUCTION
// conflict-resolution agent seam and pins that this call site threads
// cfg.runID onto the Invocation: without it the marker degrades to "1".
func TestConflictResolutionSpawnCarriesRunAgentMarker(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake agent is a /bin/sh script")
	}
	bin, dump := writeEnvDumpAgent(t)
	t.Setenv("FISHHAWK_AGENT_BIN", bin)
	t.Setenv(agent.RunAgentEnvVar, "stale-parent")
	promptFile := filepath.Join(t.TempDir(), "prompt.txt")
	if err := os.WriteFile(promptFile, []byte("resolve the conflict"), 0o600); err != nil {
		t.Fatalf("write prompt: %v", err)
	}
	const runID = "11111111-2222-3333-4444-conflict0001"
	cfg := config{
		agent:      "claude-code",
		runID:      runID,
		stage:      "implement",
		promptFile: promptFile,
		workingDir: t.TempDir(),
		timeout:    30 * time.Second,
	}
	_ = productionConflictResolutionAgentInvoker(context.Background(), cfg, io.Discard)
	assertSingleMarker(t, dump, runID)
}

// TestVerifyFixReinvokeCarriesRunAgentMarker runs the verify-fix loop with the
// REAL claude-code adapter and pins that the fix re-invoke's spawn carries the
// run id from the base Invocation.
func TestVerifyFixReinvokeCarriesRunAgentMarker(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake agent is a /bin/sh script")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	t.Setenv(agent.RunAgentEnvVar, "stale-parent")
	repo, runGit := compileGateRepo(t)
	mustWrite(t, filepath.Join(repo, "a.txt"), "hello\n")
	runGit("add", "-A")
	runGit("commit", "-m", "initial")
	mustWrite(t, filepath.Join(repo, "a.txt"), "changed\n")

	const runID = "11111111-2222-3333-4444-verifyfix001"
	cfg := config{
		runID:               runID,
		workingDir:          repo,
		verifyCmd:           "false", // always fails, so the fix re-invoke runs
		verifyMaxIterations: 1,
		scopeFiles:          []upload.ScopeFile{{Path: "a.txt", Operation: "modify"}},
	}
	bin, dump := writeEnvDumpAgent(t)
	invoker, err := selectInvoker("claude-code", "", bin)
	if err != nil {
		t.Fatalf("selectInvoker: %v", err)
	}
	agentBase, _ := agentenv.Env(os.Environ())
	baseInv := agent.Invocation{RunID: runID, Stage: "implement", WorkingDir: repo, BaseEnv: agentBase,
		Budget: agent.Budget{Timeout: 30 * time.Second}}
	res := agent.Result{OK: true}
	_, _, _ = runVerifyFixLoop(context.Background(), &cfg, nil, "", invoker, baseInv, &res, io.Discard)
	assertSingleMarker(t, dump, runID)
}

// allowedSpawnSite classifies one owner (see execSites) in
// agentSpawnSiteAllowlist: how many non-literal exec sites it holds today, and
// why none of them can exec a coding-agent session.
type allowedSpawnSite struct {
	sites int    // number of non-literal sites execSites reports under the owner
	why   string // why those sites do not exec the coding agent
}

// agentSpawnSiteAllowlist classifies every non-test exec site in the runner
// module, outside the two adapter packages, whose PROGRAM is not a string
// literal (or which takes a spawn function as a VALUE, an indirection that
// hides the program). Keyed by "<module-relative file>:<owner>", where the
// owner is the enclosing func ("Type.Method" for a method, the receiver's base
// type name with any pointer or type-parameter list stripped), or the
// package-level var. Each entry also pins the number of sites under that owner,
// so a further non-literal exec added inside an owner that is already
// classified fails the ratchet instead of being absorbed by the existing entry.
// None of these execs a coding-agent session; the only one that execs the agent
// BINARY is the --version probe, which starts no session and so reaches no
// shell tool. Classification of the spawn-site sweep for #3945; the count pin,
// import-resolved names and receiver-qualified owners are #3950.
var agentSpawnSiteAllowlist = map[string]allowedSpawnSite{
	"cmd/fishhawk-runner/agentbin.go:probeVersionCmd":   {1, "agent binary `--version` probe: no session, no shell tool, nothing to mark"},
	"cmd/fishhawk-runner/main.go:execBoundedHostArgv":   {1, "verify-gate argv under the sanitized gate env"},
	"cmd/fishhawk-runner/main.go:gitPatchIDForCommit":   {3, "git (gitPatchIDBinary test seam)"},
	"cmd/fishhawk-runner/main.go:gitDiffTreeNameStatus": {1, "git (gitDiffTreeBinary test seam)"},
	"internal/gitdiff/gitdiff.go:Runner.Run":            {1, "git (Cmd test seam)"},
	"internal/gitdiff/gitdiff.go:Runner.MergeBase":      {1, "git (Cmd test seam)"},
	"internal/gitdiff/gitdiff.go:Runner.RunPatch":       {1, "git (Cmd test seam)"},
	"internal/gitops/commit.go:Pusher.runOutEnv":        {1, "git (Cmd test seam)"},
	"internal/gitops/commit.go:Pusher.probeOut":         {1, "git (Cmd test seam)"},
	"internal/netsandbox/netsandbox.go:probeExec":       {1, "sandbox-exec availability probe"},
	"internal/gateiso/sandbox.go:sandboxProbeExec":      {1, "gate-isolation sandbox availability probe"},
	"internal/gateiso/runtime.go:DefaultProbes":         {1, "container runtime probe"},
	"internal/gateiso/clone.go:runGit":                  {1, "git (resolved binary path)"},
	"internal/gateiso/cache.go:seedDefaultExec":         {1, "module-cache seed command"},
	"internal/hostload/hostload.go:defaultRunCommand":   {1, "ps / sysctl host-load probe"},

	"internal/gateiso/dockerconfig.go:ProbeCredentialHelper": {1, "docker credential helper probe (docker-credential-<name> get; name regex-validated, cannot be the coding agent)"},
}

// TestNoAgentSpawnOutsideAdapters is the source-scan ratchet behind the
// #3945 spawn-site sweep: the run-agent marker is stamped ONLY inside the two
// adapters' Invoke, so a new exec of the agent binary elsewhere would spawn an
// unmarked agent. It walks every non-test .go file in the runner module
// (excluding internal/agent/claudecode and internal/agent/codex) and FAILS on
// any exec site whose program is not a string literal, or is the literal
// "claude"/"codex", unless the owner is classified in agentSpawnSiteAllowlist.
// Three properties keep the classification from absorbing new sites (#3950):
//
//   - The allowlist pins the number of sites per owner, so a further
//     non-literal exec added inside a classified owner fails with a count
//     mismatch; the author classifies the new site and bumps the pin, or
//     routes it through an adapter.
//   - Package names are resolved from each file's own import specs, so an
//     aliased import is seen under every local name it is bound to, and an
//     identifier that merely happens to be spelled `exec` in a file that does
//     not import os/exec is not a site. A DOT import of a spawn package binds
//     unqualified names the scan cannot attribute, so it is reported as an
//     unclassifiable site and fails closed. A blank import binds no identifier,
//     so it contributes no name and is not a site.
//   - Owners carry the receiver's base type ("Runner.Run"), so same-named
//     methods on different types cannot share one entry.
//
// It also fails on a stale allowlist entry so the classification cannot rot.
//
// What it sees: calls to, and value-uses of, os/exec Command and
// CommandContext, syscall Exec / ForkExec / StartProcess, os.StartProcess and
// golang.org/x/sys/unix Exec; and a composite literal whose type is spelled as
// the os/exec Cmd type (exec.Cmd{...}, &exec.Cmd{...}), the form that sidesteps
// the constructor.
//
// What it does NOT see, stated honestly: it is a SYNTACTIC scan, not a type
// check, and does not claim to be a complete list of ways to start a process.
//   - An exec through a func value that was itself obtained without naming one
//     of the functions above through an import in the scanned file: a seam
//     assigned in another module, or a func value passed in as a parameter.
//   - An exec.Cmd built without a composite literal of that spelling
//     (`new(exec.Cmd)`, `var c exec.Cmd` with fields assigned afterwards, an
//     elided element literal in `[]exec.Cmd{{...}}`) and then started.
//   - Any other x/sys/unix call and any other non-stdlib process package; a raw
//     syscall.Syscall / RawSyscall execve; cgo; reflection; plugins.
//   - A literal-program `sh -c` (or any other literal program that re-execs)
//     whose command string names the agent.
//   - A count-preserving swap: removing one non-literal site and adding another
//     under the same allowlisted owner leaves the pinned count unchanged. The
//     pin catches growth, not replacement; that is for the reviewer reading
//     the diff.
//   - A package whose declared name differs from the last element of its import
//     path (true of none of the stdlib paths above).
func TestNoAgentSpawnOutsideAdapters(t *testing.T) {
	root := filepath.Clean(filepath.Join(pkgSrcDir, "..", ".."))
	skipDirs := map[string]bool{
		filepath.Join(root, "internal", "agent", "claudecode"): true,
		filepath.Join(root, "internal", "agent", "codex"):      true,
	}
	sitesByKey := map[string][]execSite{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipDirs[path] || d.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return fmt.Errorf("parse %s: %w", rel, perr)
		}
		for _, site := range execSites(f) {
			site.line = fset.Position(site.pos).Line
			key := rel + ":" + site.owner
			sitesByKey[key] = append(sitesByKey[key], site)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the runner module: %v", err)
	}
	for _, p := range ratchetProblems(sitesByKey, agentSpawnSiteAllowlist) {
		t.Error(p)
	}
}

// ratchetProblems is the ratchet's decision, split from the filesystem walk so
// it is unit-testable. sitesByKey groups the scan's sites by
// "<file>:<owner>". It returns, sorted, one problem per:
//   - site that is unclassified (its key is not in allow) or is a literal agent
//     program (never allowable);
//   - allowlisted key whose observed non-literal site count is above zero and
//     differs from the pinned count;
//   - allowlisted key with no observed non-literal site (stale). A stale key
//     yields only this problem, never a count mismatch as well.
func ratchetProblems(sitesByKey map[string][]execSite, allow map[string]allowedSpawnSite) []string {
	var problems []string
	for key, sites := range sitesByKey {
		_, classified := allow[key]
		for _, s := range sites {
			if classified && !s.agentLiteral {
				continue
			}
			problems = append(problems, fmt.Sprintf("unclassified exec site %s (%s, line %d): if it can exec the coding agent, route it through an agent adapter (or stamp agent.WithRunAgentMarker on its env) and pin it; otherwise classify it in agentSpawnSiteAllowlist (#3945)", key, s.why, s.line))
		}
	}
	for key, entry := range allow {
		var lines []string
		for _, s := range sitesByKey[key] {
			if !s.agentLiteral {
				lines = append(lines, strconv.Itoa(s.line))
			}
		}
		switch {
		case len(lines) == 0:
			problems = append(problems, fmt.Sprintf("stale agentSpawnSiteAllowlist entry %q: no such exec site any more; remove or re-key it", key))
		case len(lines) != entry.sites:
			problems = append(problems, fmt.Sprintf("exec site count drift in %s: %d non-literal site(s) observed (lines %s), %d pinned in agentSpawnSiteAllowlist: if a new site can exec the coding agent, route it through an agent adapter (or stamp agent.WithRunAgentMarker on its env); otherwise classify it and update the pinned count (#3950)", key, len(lines), strings.Join(lines, ", "), entry.sites))
		}
	}
	sort.Strings(problems)
	return problems
}

// execSite is one exec-shaped node the ratchet must classify.
type execSite struct {
	owner        string // enclosing func ("Type.Method" for a method), or package-level var name
	why          string
	pos          token.Pos
	line         int  // filled by the caller from its FileSet; execSites has none
	agentLiteral bool // the program is the literal "claude" / "codex"
}

// spawnFuncs lists, per import path, the functions that start a process.
// os/exec's Command and CommandContext additionally take a program argument
// execSites inspects; the rest are reported unconditionally.
var spawnFuncs = map[string]map[string]bool{
	"os/exec":               {"Command": true, "CommandContext": true},
	"syscall":               {"Exec": true, "ForkExec": true, "StartProcess": true},
	"os":                    {"StartProcess": true},
	"golang.org/x/sys/unix": {"Exec": true},
}

// importBase is the default package name for an import path: its last element.
func importBase(importPath string) string {
	return importPath[strings.LastIndex(importPath, "/")+1:]
}

// spawnImports resolves, from f's own import specs, every local name bound to
// a spawn package (spawnFuncs): name -> import path. One path may be bound
// under several names (a file may import os/exec twice). A blank import binds
// no identifier and is skipped. A dot import binds unqualified names the scan
// cannot attribute, so it is returned separately for the caller to fail closed.
func spawnImports(f *ast.File) (names map[string]string, dot []*ast.ImportSpec) {
	names = map[string]string{}
	for _, imp := range f.Imports {
		importPath, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		if _, spawns := spawnFuncs[importPath]; !spawns {
			continue
		}
		switch {
		case imp.Name == nil:
			names[importBase(importPath)] = importPath
		case imp.Name.Name == "_":
		case imp.Name.Name == ".":
			dot = append(dot, imp)
		default:
			names[imp.Name.Name] = importPath
		}
	}
	return names, dot
}

// receiverOwner is the "Type.Method" owner for a method declaration: the
// receiver's base type name with pointer, parentheses and type-parameter lists
// stripped. A receiver that does not reduce to an identifier yields "?.Method",
// which no allowlist entry names, so the site stays unclassified.
func receiverOwner(d *ast.FuncDecl) string {
	typeName := "?"
	if d.Recv != nil && len(d.Recv.List) == 1 {
		e := d.Recv.List[0].Type
	unwrap:
		for {
			switch t := e.(type) {
			case *ast.StarExpr:
				e = t.X
			case *ast.ParenExpr:
				e = t.X
			case *ast.IndexExpr:
				e = t.X
			case *ast.IndexListExpr:
				e = t.X
			case *ast.Ident:
				typeName = t.Name
				break unwrap
			default:
				break unwrap
			}
		}
	}
	return typeName + "." + d.Name.Name
}

// execSites returns every exec site in f whose program is not a plain string
// literal (or is the agent's own name), plus every value-use of a spawn
// function and every os/exec Cmd composite literal. Package names are resolved
// from f's imports (spawnImports). A dot import of a spawn package is itself a
// site, with owner "<import>", because the unqualified calls it enables cannot
// be attributed.
func execSites(f *ast.File) []execSite {
	var sites []execSite
	names, dot := spawnImports(f)
	for _, imp := range dot {
		importPath, _ := strconv.Unquote(imp.Path.Value)
		sites = append(sites, execSite{owner: "<import>", why: "dot-import of " + importPath + " is unscannable", pos: imp.Pos()})
	}
	// spawnSel reports the import path and function name when sel is
	// <resolved spawn package>.<spawn function>.
	spawnSel := func(sel *ast.SelectorExpr) (importPath, fn string, ok bool) {
		pkg, isIdent := sel.X.(*ast.Ident)
		if !isIdent {
			return "", "", false
		}
		importPath, bound := names[pkg.Name]
		if !bound || !spawnFuncs[importPath][sel.Sel.Name] {
			return "", "", false
		}
		return importPath, sel.Sel.Name, true
	}
	visit := func(owner string, root ast.Node) {
		calls := map[*ast.SelectorExpr]bool{}
		ast.Inspect(root, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.CompositeLit:
				if sel, ok := node.Type.(*ast.SelectorExpr); ok && sel.Sel.Name == "Cmd" {
					if pkg, ok := sel.X.(*ast.Ident); ok && names[pkg.Name] == "os/exec" {
						sites = append(sites, execSite{owner: owner, why: "exec.Cmd composite literal", pos: node.Pos()})
					}
				}
			case *ast.CallExpr:
				sel, ok := node.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				importPath, fn, ok := spawnSel(sel)
				if !ok {
					return true
				}
				calls[sel] = true
				if importPath != "os/exec" {
					sites = append(sites, execSite{owner: owner, why: importBase(importPath) + "." + fn, pos: node.Pos()})
					return true
				}
				argIdx := 0
				if fn == "CommandContext" {
					argIdx = 1
				}
				if len(node.Args) <= argIdx {
					return true
				}
				if lit, ok := node.Args[argIdx].(*ast.BasicLit); ok && lit.Kind == token.STRING {
					prog, _ := strconv.Unquote(lit.Value)
					if base := filepath.Base(prog); base == "claude" || base == "codex" {
						sites = append(sites, execSite{owner: owner, why: "literal agent program " + prog, pos: node.Pos(), agentLiteral: true})
					}
					return true
				}
				sites = append(sites, execSite{owner: owner, why: "non-literal program", pos: node.Pos()})
			}
			return true
		})
		ast.Inspect(root, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok || calls[sel] {
				return true
			}
			if importPath, fn, ok := spawnSel(sel); ok {
				sites = append(sites, execSite{owner: owner, why: importBase(importPath) + "." + fn + " used as a value", pos: sel.Pos()})
			}
			return true
		})
	}
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Body == nil {
				continue
			}
			owner := d.Name.Name
			if d.Recv != nil {
				owner = receiverOwner(d)
			}
			visit(owner, d.Body)
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok || len(vs.Names) == 0 {
					continue
				}
				for _, v := range vs.Values {
					visit(vs.Names[0].Name, v)
				}
			}
		}
	}
	return sites
}

// scanSource runs execSites over a synthetic source file.
func scanSource(t *testing.T, src string) []execSite {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "x.go", src, 0)
	if err != nil {
		t.Fatalf("parse synthetic source: %v", err)
	}
	return execSites(f)
}

func siteOwners(sites []execSite) []string {
	owners := make([]string, 0, len(sites))
	for _, s := range sites {
		owners = append(owners, s.owner)
	}
	sort.Strings(owners)
	return owners
}

func TestExecSites_AliasedOSExecImportIsSeen(t *testing.T) {
	t.Run("aliased", func(t *testing.T) {
		sites := scanSource(t, `package p
import (
	"context"
	osexec "os/exec"
)
func run(ctx context.Context, bin string) { _ = osexec.CommandContext(ctx, bin) }
`)
		if len(sites) != 1 || sites[0].owner != "run" || sites[0].agentLiteral {
			t.Fatalf("want one non-literal site owned by run, got %+v", sites)
		}
	})
	t.Run("same path imported under two names", func(t *testing.T) {
		sites := scanSource(t, `package p
import (
	"context"
	"os/exec"
	osexec "os/exec"
)
func a(bin string) { _ = exec.Command(bin) }
func b(ctx context.Context, bin string) { _ = osexec.CommandContext(ctx, bin) }
`)
		if got := siteOwners(sites); len(got) != 2 || got[0] != "a" || got[1] != "b" {
			t.Fatalf("want a site under each import name (owners a, b), got %v", got)
		}
	})
}

func TestExecSites_UnimportedExecIdentIsNotASite(t *testing.T) {
	sites := scanSource(t, `package p
type fake struct{}
func (fake) Command(string) {}
func run(bin string) {
	var exec fake
	exec.Command(bin)
}
`)
	if len(sites) != 0 {
		t.Fatalf("a local ident named exec without an os/exec import is not a site, got %+v", sites)
	}
}

func TestExecSites_BlankImportIsNotASite(t *testing.T) {
	sites := scanSource(t, `package p
import _ "os/exec"
func run() {}
`)
	if len(sites) != 0 {
		t.Fatalf("a blank import binds no identifier and is not a site, got %+v", sites)
	}
}

func TestExecSites_DotImportIsUnscannable(t *testing.T) {
	for _, importPath := range []string{"os/exec", "syscall", "os", "golang.org/x/sys/unix"} {
		t.Run(importPath, func(t *testing.T) {
			sites := scanSource(t, "package p\nimport . \""+importPath+"\"\n")
			if len(sites) != 1 || sites[0].owner != "<import>" || sites[0].agentLiteral ||
				!strings.Contains(sites[0].why, "dot-import of "+importPath) {
				t.Fatalf("want one unscannable dot-import site, got %+v", sites)
			}
		})
	}
	// A dot import of a package the scan does not model is not a spawn site.
	if sites := scanSource(t, "package p\nimport . \"strings\"\n"); len(sites) != 0 {
		t.Fatalf("dot import of a non-spawn package is not a site, got %+v", sites)
	}
}

func TestExecSites_ReceiverDisambiguatesOwner(t *testing.T) {
	sites := scanSource(t, `package p
import "os/exec"
type A struct{}
type B struct{}
type G[T any] struct{}
type H[K comparable, V any] struct{}
func (A) Run(bin string)       { _ = exec.Command(bin) }
func (*B) Run(bin string)      { _ = exec.Command(bin) }
func (g *G[T]) Run(bin string) { _ = exec.Command(bin) }
func (h H[K, V]) Run(bin string) { _ = exec.Command(bin) }
func Run(bin string)           { _ = exec.Command(bin) }
`)
	got := siteOwners(sites)
	want := []string{"A.Run", "B.Run", "G.Run", "H.Run", "Run"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("owners = %v, want %v", got, want)
	}
}

func TestExecSites_UnreducibleReceiverStaysUnclassified(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "x.go", `package p
import "os/exec"
func (a *A) Run(bin string) { _ = exec.Command(bin) }
`, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Replace the receiver type with a shape that does not reduce to an ident.
	fd := f.Decls[1].(*ast.FuncDecl)
	fd.Recv.List[0].Type = &ast.ArrayType{Elt: ast.NewIdent("A")}
	if got := siteOwners(execSites(f)); len(got) != 1 || got[0] != "?.Run" {
		t.Fatalf("owners = %v, want [?.Run]", got)
	}
}

func TestExecSites_ExecCmdCompositeLiteral(t *testing.T) {
	sites := scanSource(t, `package p
import (
	"os/exec"
	osexec "os/exec"
)
type holder struct{ c *exec.Cmd }
func value(p string) exec.Cmd   { return exec.Cmd{Path: p} }
func pointer(p string) *exec.Cmd { return &exec.Cmd{Path: p} }
func aliased(p string) *osexec.Cmd { return &osexec.Cmd{Path: p} }
func typeOnly(c *exec.Cmd) *exec.Cmd { var d *exec.Cmd = c; return d }
`)
	got := siteOwners(sites)
	want := []string{"aliased", "pointer", "value"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("owners = %v, want %v (a type mention without a literal is not a site)", got, want)
	}
	// A composite literal of a same-named type from a package the file does not
	// import as os/exec is not a site.
	if sites := scanSource(t, `package p
type other struct{ Cmd struct{} }
func f() { var exec other; _ = exec.Cmd }
func g() { _ = struct{ Path string }{} }
`); len(sites) != 0 {
		t.Fatalf("got %+v, want none", sites)
	}
}

func TestExecSites_SyscallAndOSSpawnForms(t *testing.T) {
	cases := []struct {
		name, imports, body, wantWhy string
	}{
		{"syscall.ForkExec", `"syscall"`, `_, _, _ = syscall.ForkExec(p, nil, nil)`, "syscall.ForkExec"},
		{"syscall.StartProcess", `"syscall"`, `_, _, _ = syscall.StartProcess(p, nil, nil)`, "syscall.StartProcess"},
		{"syscall.Exec", `"syscall"`, `_ = syscall.Exec(p, nil, nil)`, "syscall.Exec"},
		{"os.StartProcess", `"os"`, `_, _ = os.StartProcess(p, nil, nil)`, "os.StartProcess"},
		{"unix.Exec", `"golang.org/x/sys/unix"`, `_ = unix.Exec(p, nil, nil)`, "unix.Exec"},
		{"aliased syscall", `sc "syscall"`, `_, _, _ = sc.ForkExec(p, nil, nil)`, "syscall.ForkExec"},
		{"value-use", `"syscall"`, `f := syscall.ForkExec; _ = f`, "syscall.ForkExec used as a value"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sites := scanSource(t, "package p\nimport "+tc.imports+"\nfunc run(p string) { "+tc.body+" }\n")
			if len(sites) != 1 || sites[0].owner != "run" || sites[0].why != tc.wantWhy {
				t.Fatalf("want one site %q under run, got %+v", tc.wantWhy, sites)
			}
		})
	}
	// Without the import, the same spelling is a plain identifier, not a site.
	if sites := scanSource(t, "package p\nfunc run(syscall interface{ ForkExec() }) { syscall.ForkExec() }\n"); len(sites) != 0 {
		t.Fatalf("an unimported syscall ident is not a site, got %+v", sites)
	}
}

func TestExecSites_ProgramLiteralHandling(t *testing.T) {
	sites := scanSource(t, `package p
import (
	"context"
	"os/exec"
)
func git(ctx context.Context)    { _ = exec.CommandContext(ctx, "git") }
func claude(ctx context.Context) { _ = exec.CommandContext(ctx, "claude") }
func codex()                     { _ = exec.Command("/usr/local/bin/codex") }
func dynamic(bin string)         { _ = exec.Command(bin) }
func asValue()                   { f := exec.Command; _ = f }
`)
	byOwner := map[string]execSite{}
	for _, s := range sites {
		byOwner[s.owner] = s
	}
	if len(sites) != 4 {
		t.Fatalf("want 4 sites (claude, codex, dynamic, asValue; literal git is none), got %+v", sites)
	}
	if !byOwner["claude"].agentLiteral || !byOwner["codex"].agentLiteral {
		t.Errorf("literal claude/codex programs must be flagged agentLiteral: %+v", byOwner)
	}
	if byOwner["dynamic"].agentLiteral || byOwner["asValue"].agentLiteral {
		t.Errorf("non-literal sites must not be flagged agentLiteral: %+v", byOwner)
	}
}

func TestRatchetProblems_ExtraSiteInClassifiedOwnerFails(t *testing.T) {
	const k = "internal/x/x.go:Runner.Run"
	allow := map[string]allowedSpawnSite{k: {1, "git"}}
	site := func(line int) execSite { return execSite{owner: "Runner.Run", why: "non-literal program", line: line} }
	literal := execSite{owner: "Runner.Run", why: "literal agent program claude", line: 9, agentLiteral: true}

	cases := []struct {
		name     string
		observed map[string][]execSite
		want     []string // substrings, one per expected problem, in sorted order
	}{
		{"pinned count matches", map[string][]execSite{k: {site(3)}}, nil},
		{"extra site is a count drift", map[string][]execSite{k: {site(3), site(7)}},
			[]string{"exec site count drift in " + k + ": 2 non-literal site(s) observed (lines 3, 7), 1 pinned"}},
		{"zero observed is only stale", map[string][]execSite{}, []string{"stale agentSpawnSiteAllowlist entry \"" + k + "\""}},
		{"unlisted key is unclassified", map[string][]execSite{k: {site(3)}, "internal/y/y.go:Other": {site(5)}},
			[]string{"unclassified exec site internal/y/y.go:Other (non-literal program, line 5)"}},
		{"agent literal under a classified key is unclassified", map[string][]execSite{k: {site(3), literal}},
			[]string{"unclassified exec site " + k + " (literal agent program claude, line 9)"}},
		{"only an agent literal under a classified key is unclassified and stale", map[string][]execSite{k: {literal}},
			[]string{"stale agentSpawnSiteAllowlist entry", "unclassified exec site " + k}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ratchetProblems(tc.observed, allow)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d problems, want %d: %q", len(got), len(tc.want), got)
			}
			for i, sub := range tc.want {
				if !strings.Contains(got[i], sub) {
					t.Errorf("problem %d = %q, want it to contain %q", i, got[i], sub)
				}
			}
		})
	}

	// Pinned above the observed count is also drift (a removed site must lower
	// the pin), and a pin of 3 with 3 observed is clean.
	allow3 := map[string]allowedSpawnSite{k: {3, "git"}}
	if got := ratchetProblems(map[string][]execSite{k: {site(1), site(2)}}, allow3); len(got) != 1 ||
		!strings.Contains(got[0], "2 non-literal site(s) observed (lines 1, 2), 3 pinned") {
		t.Errorf("under-count: got %q", got)
	}
	if got := ratchetProblems(map[string][]execSite{k: {site(1), site(2), site(3)}}, allow3); len(got) != 0 {
		t.Errorf("matching pin of 3: got %q, want none", got)
	}
}
