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
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		return nil, fmt.Errorf("runtime.Caller failed")
	}
	dir := filepath.Dir(self)
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

// agentSpawnSiteAllowlist classifies every non-test exec site in the runner
// module, outside the two adapter packages, whose PROGRAM is not a string
// literal (or which takes exec.Command / exec.CommandContext as a VALUE, an
// indirection that hides the program). Keyed by "<module-relative file>:<enclosing
// func or package-level var>". None of these execs a coding-agent session; the
// only one that execs the agent BINARY is the --version probe, which starts no
// session and so reaches no shell tool. Classification of the spawn-site sweep
// for #3945.
var agentSpawnSiteAllowlist = map[string]string{
	"cmd/fishhawk-runner/agentbin.go:probeVersionCmd":   "agent binary `--version` probe: no session, no shell tool, nothing to mark",
	"cmd/fishhawk-runner/main.go:execBoundedHostArgv":   "verify-gate argv under the sanitized gate env",
	"cmd/fishhawk-runner/main.go:gitPatchIDForCommit":   "git (gitPatchIDBinary test seam)",
	"cmd/fishhawk-runner/main.go:gitDiffTreeNameStatus": "git (gitDiffTreeBinary test seam)",
	"internal/gitdiff/gitdiff.go:Run":                   "git (Cmd test seam)",
	"internal/gitdiff/gitdiff.go:MergeBase":             "git (Cmd test seam)",
	"internal/gitdiff/gitdiff.go:RunPatch":              "git (Cmd test seam)",
	"internal/gitops/commit.go:runOutEnv":               "git (Cmd test seam)",
	"internal/gitops/commit.go:probeOut":                "git (Cmd test seam)",
	"internal/netsandbox/netsandbox.go:probeExec":       "sandbox-exec availability probe",
	"internal/gateiso/sandbox.go:sandboxProbeExec":      "gate-isolation sandbox availability probe",
	"internal/gateiso/runtime.go:DefaultProbes":         "container runtime probe",
	"internal/gateiso/clone.go:runGit":                  "git (resolved binary path)",
	"internal/gateiso/cache.go:seedDefaultExec":         "module-cache seed command",
	"internal/hostload/hostload.go:defaultRunCommand":   "ps / sysctl host-load probe",
}

// TestNoAgentSpawnOutsideAdapters is the source-scan ratchet behind the
// #3945 spawn-site sweep: the run-agent marker is stamped ONLY inside the two
// adapters' Invoke, so a new exec of the agent binary elsewhere would spawn an
// unmarked agent. It walks every non-test .go file in the runner module
// (excluding internal/agent/claudecode and internal/agent/codex) and FAILS on
// any exec site whose program is not a string literal, or is the literal
// "claude"/"codex", unless the site is classified in agentSpawnSiteAllowlist.
// It also fails on a stale allowlist entry so the classification cannot rot.
//
// Residual, stated honestly: it is a SYNTACTIC over-approximation. It sees
// exec.Command / exec.CommandContext calls and value-uses, syscall.Exec and
// os.StartProcess; it does not see an exec through a func value that was
// itself obtained without naming those (e.g. a seam assigned in another
// module), nor a literal-program `sh -c` whose command string names the agent.
func TestNoAgentSpawnOutsideAdapters(t *testing.T) {
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(self), "..", ".."))
	skipDirs := map[string]bool{
		filepath.Join(root, "internal", "agent", "claudecode"): true,
		filepath.Join(root, "internal", "agent", "codex"):      true,
	}
	found := map[string]bool{}
	var unclassified []string
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
			key := rel + ":" + site.owner
			if _, ok := agentSpawnSiteAllowlist[key]; ok && !site.agentLiteral {
				found[key] = true
				continue
			}
			unclassified = append(unclassified, fmt.Sprintf("%s (%s, line %d)", key, site.why, fset.Position(site.pos).Line))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the runner module: %v", err)
	}
	sort.Strings(unclassified)
	for _, u := range unclassified {
		t.Errorf("unclassified exec site %s: if it can exec the coding agent, route it through an agent adapter (or stamp agent.WithRunAgentMarker on its env) and pin it; otherwise classify it in agentSpawnSiteAllowlist (#3945)", u)
	}
	for key := range agentSpawnSiteAllowlist {
		if !found[key] {
			t.Errorf("stale agentSpawnSiteAllowlist entry %q: no such exec site any more; remove or re-key it", key)
		}
	}
}

// execSite is one exec-shaped node the ratchet must classify.
type execSite struct {
	owner        string // enclosing func, or package-level var name
	why          string
	pos          token.Pos
	agentLiteral bool // the program is the literal "claude" / "codex"
}

// execSites returns every exec site in f whose program is not a plain string
// literal (or is the agent's own name), plus every value-use of
// exec.Command / exec.CommandContext.
func execSites(f *ast.File) []execSite {
	var sites []execSite
	visit := func(owner string, root ast.Node) {
		calls := map[*ast.SelectorExpr]bool{}
		ast.Inspect(root, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			switch {
			case pkg.Name == "exec" && (sel.Sel.Name == "Command" || sel.Sel.Name == "CommandContext"):
				calls[sel] = true
				argIdx := 0
				if sel.Sel.Name == "CommandContext" {
					argIdx = 1
				}
				if len(call.Args) <= argIdx {
					return true
				}
				if lit, ok := call.Args[argIdx].(*ast.BasicLit); ok && lit.Kind == token.STRING {
					prog, _ := strconv.Unquote(lit.Value)
					if base := filepath.Base(prog); base == "claude" || base == "codex" {
						sites = append(sites, execSite{owner, "literal agent program " + prog, call.Pos(), true})
					}
					return true
				}
				sites = append(sites, execSite{owner, "non-literal program", call.Pos(), false})
			case (pkg.Name == "syscall" && sel.Sel.Name == "Exec") || (pkg.Name == "os" && sel.Sel.Name == "StartProcess"):
				sites = append(sites, execSite{owner, pkg.Name + "." + sel.Sel.Name, call.Pos(), false})
			}
			return true
		})
		ast.Inspect(root, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok || calls[sel] {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "exec" &&
				(sel.Sel.Name == "Command" || sel.Sel.Name == "CommandContext") {
				sites = append(sites, execSite{owner, "exec." + sel.Sel.Name + " used as a value", sel.Pos(), false})
			}
			return true
		})
	}
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Body != nil {
				visit(d.Name.Name, d.Body)
			}
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
