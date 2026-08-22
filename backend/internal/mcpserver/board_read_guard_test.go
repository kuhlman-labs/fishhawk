package mcpserver

// ADR-064 / #2230 ratified invariant, enforced technically: NO board-read path
// is exposed through the MCP agent tool surface.
//
// Why it is an invariant rather than a preference: a board column is stale,
// unaudited, rate-limited and (through a ProjectV2 board item list) silently
// truncating. An agent that read run state off the board would be acting on a
// second, unreconciled source of truth, so the work-management READ capability
// (#2230) reserves the board-as-input seam WITHOUT wiring it to any agent tool.
//
// What this file establishes, precisely:
//
//   - TRANSITIVE reachability (condition C5). TestNoBoardReadOnMCPToolSurface
//     walks the full in-repo dependency closure of this package's NON-TEST code
//     and fails if backend/internal/workmgmt (or any sub-package) is reachable
//     AT ALL, naming the import chain. A path routed via an intermediary would
//     pass a direct-import check while remaining reachable; this does not.
//   - DIRECT symbol references. TestNoBoardReadSymbolsInMCPPackage fails if any
//     non-test file in this package names a board-read symbol, naming
//     file+symbol.
//   - Board WRITES as well as reads (E54.5 / #2237). The grooming APPLY
//     capability (workmgmt.GroomingMutator / MutatorFor / ApplyGrooming) mutates
//     the tracker on an agent's proposal, so an agent tool that could dispatch a
//     grooming mutation would be acting outside the very gate that layer exists
//     to enforce — a strictly larger hazard than reading a stale column. The
//     symbol list therefore carries the apply symbols by EXACT name; a bare
//     "Grooming" substring is deliberately NOT banned, because handling a
//     grooming REPORT (an artifact this surface legitimately ingests) is not
//     dispatching a grooming MUTATION.
//
// What it does NOT establish, stated plainly rather than left implied:
//
//   - It is a SOURCE-LEVEL check, tamper-evident within the repo, not a runtime
//     capability boundary. A stronger boundary would put the read capability
//     behind a separate process/credential surface; that is out of scope here.
//   - The transitive walk covers packages inside this repository's modules
//     only. A dependency reaching workmgmt through a THIRD-PARTY module is not
//     representable (nothing outside this repo can import an internal package),
//     but the walk reports any in-repo import path it could not resolve to a
//     directory so an unexpected gap is visible rather than silent.
//   - It guards the workmgmt CAPABILITY, not the raw transport underneath it.
//     githubclient is already in this package's dependency closure for
//     unrelated reasons, so githubclient.ListRepoIssues is reachable in the
//     closure sense; the direct-symbol check names it too, but a transitive
//     ban on githubclient is not possible without breaking the package.
//
// Today this package's non-test code imports workmgmt ZERO times (only
// campaign_test.go and tools_test.go do), so the guard pins a real current
// state, not an aspiration. It registers NO tool, so tools_test.go's tool-count
// invariant is deliberately untouched.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// workmgmtPkg is the package whose reachability from the MCP tool surface is
// forbidden. Any package under it (workmgmt/github, workmgmt/gitlab, …) is
// matched too, since importing one of those pulls the board-read vocabulary in
// just the same.
const workmgmtPkg = "github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"

// repoModulePrefix is the import-path prefix of this repository's Go modules.
// An import carrying it maps to a directory in the tree; anything else is a
// third-party or standard-library package and cannot reach an internal package
// of this repo.
const repoModulePrefix = "github.com/kuhlman-labs/fishhawk/"

// repoRoot is the repository root relative to this package's directory
// (backend/internal/mcpserver).
const repoRoot = "../../.."

// boardReadSymbols are the work-management board-capability identifiers no
// non-test file in this package may name — the #2230 READ capability and the
// #2237 grooming-APPLY capability. Matching is by prefix (see bannedSymbol),
// so each entry also covers its request/result satellites.
var boardReadSymbols = []string{
	"WorkItemReader",
	"ReadWorkItem",
	"ListWorkItems",
	"WorkItemRecord",
	"WorkItemPage",
	"ReaderFor",
	"ListRepoIssues",
	// E54.5 / #2237 — the grooming-mutation (board WRITE) capability.
	"GroomingMutator",
	"ApplyGroomingMutation",
	"MutatorFor",
	"ApplyGrooming",
	// "GroomingMutation" (not the bare "Grooming") covers the whole mutation
	// vocabulary — Request, Result, Record, Kind — while leaving
	// GroomingReport, which this surface legitimately ingests, alone.
	"GroomingMutation",
}

// packageImports parses every non-test .go file in dir and returns the sorted
// set of imported package paths. A dir that does not exist returns ok=false so
// the caller can report an unresolved edge instead of silently treating it as
// a leaf.
func packageImports(dir string) (imports []string, ok bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, false
	}
	seen := map[string]struct{}{}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.ImportsOnly)
		if err != nil {
			continue
		}
		for _, spec := range f.Imports {
			p, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				continue
			}
			seen[p] = struct{}{}
		}
	}
	for p := range seen {
		imports = append(imports, p)
	}
	sort.Strings(imports)
	return imports, true
}

// dirForImportPath maps an in-repo import path to its directory relative to
// this package, or "" for a path outside this repository's modules.
func dirForImportPath(p string) string {
	if !strings.HasPrefix(p, repoModulePrefix) {
		return ""
	}
	return filepath.Join(repoRoot, strings.TrimPrefix(p, repoModulePrefix))
}

// TestNoBoardReadOnMCPToolSurface is the condition-C5 transitive guard: it
// walks this package's full in-repo dependency closure (non-test code only)
// breadth-first and fails if the work-management package is reachable through
// ANY chain, printing the chain so the offending edge is obvious.
func TestNoBoardReadOnMCPToolSurface(t *testing.T) {
	const self = "github.com/kuhlman-labs/fishhawk/backend/internal/mcpserver"

	// parent records how each package was first reached, so a hit can be
	// rendered as the full import chain rather than just a package name.
	parent := map[string]string{self: ""}
	visited := map[string]bool{self: true}
	queue := []string{self}
	var unresolved []string
	var closure []string

	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]

		dir := dirForImportPath(cur)
		if dir == "" {
			continue // outside this repo: cannot reach an internal package
		}
		imports, ok := packageImports(dir)
		if !ok {
			// An in-repo import path with no directory. Report it rather than
			// treating it as a leaf, so a gap in the walk is visible.
			unresolved = append(unresolved, cur)
			continue
		}
		for _, imp := range imports {
			if !strings.HasPrefix(imp, repoModulePrefix) || visited[imp] {
				continue
			}
			visited[imp] = true
			parent[imp] = cur
			closure = append(closure, imp)

			if imp == workmgmtPkg || strings.HasPrefix(imp, workmgmtPkg+"/") {
				t.Errorf("board-read reachability: %s is reachable from the MCP tool surface via:\n  %s\n"+
					"ADR-064 forbids exposing a board-read path through the MCP agent tool surface — a board column is "+
					"stale, unaudited and truncation-prone, so an agent acting on it would be acting on unreconciled run state.",
					imp, strings.Join(importChain(parent, imp), "\n    -> "))
				continue
			}
			queue = append(queue, imp)
		}
	}

	if len(closure) == 0 {
		t.Fatal("dependency walk found no in-repo imports at all — the walk is not doing its job (check repoRoot / the working directory)")
	}
	// Honesty: report what the walk could not resolve, so a silent gap is
	// impossible. This is informational, not a failure.
	if t.Failed() {
		return
	}
	if len(unresolved) > 0 {
		sort.Strings(unresolved)
		t.Logf("transitive walk could not resolve %d in-repo import path(s) to a directory (not treated as leaves): %s",
			len(unresolved), strings.Join(unresolved, ", "))
	}
	t.Logf("walked %d in-repo packages in the MCP tool surface's dependency closure; none reaches %s", len(closure), workmgmtPkg)
}

// importChain renders the chain from the root to pkg.
func importChain(parent map[string]string, pkg string) []string {
	var chain []string
	for p := pkg; p != ""; p = parent[p] {
		chain = append(chain, p)
	}
	// Reverse: root first.
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}
	return chain
}

// TestNoBoardReadSymbolsInMCPPackage is the DIRECT check kept alongside the
// transitive walk: no non-test file in this package may import workmgmt or
// name a board-read symbol. It fails naming file + symbol, which the
// transitive walk (which names packages) does not.
func TestNoBoardReadSymbolsInMCPPackage(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	fset := token.NewFileSet()
	files := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		files++
		for _, spec := range f.Imports {
			p, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				continue
			}
			if p == workmgmtPkg || strings.HasPrefix(p, workmgmtPkg+"/") {
				t.Errorf("%s imports %s: the MCP agent tool surface must not reach the work-management package (ADR-064)", name, p)
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch v := n.(type) {
			case *ast.SelectorExpr:
				if sym := bannedSymbol(v.Sel.Name); sym != "" {
					t.Errorf("%s references board-read symbol %q: no board-read path may be exposed through the MCP agent tool surface (ADR-064)", name, sym)
				}
			case *ast.Ident:
				if sym := bannedSymbol(v.Name); sym != "" {
					t.Errorf("%s references board-read symbol %q: no board-read path may be exposed through the MCP agent tool surface (ADR-064)", name, sym)
				}
			}
			return true
		})
	}
	if files == 0 {
		t.Fatal("no non-test .go files parsed — the guard is not scanning anything")
	}
}

// bannedSymbol reports the board-read symbol name matches, or "" when it does
// not. Matching is by PREFIX so the request/result satellites of each symbol
// (ListWorkItemsRequest, ReadWorkItemRequest, WorkItemPage…) are caught too — a
// reference to the request type is as much an exposed board-read path as a
// reference to the method.
func bannedSymbol(name string) string {
	for _, s := range boardReadSymbols {
		if strings.HasPrefix(name, s) {
			return name
		}
	}
	return ""
}

// TestBannedSymbolCoversGroomingApplySymbols pins the #2237 extension at the
// matcher itself: every grooming-apply symbol is banned, and the two
// near-misses that must NOT be — a grooming REPORT artifact type and an
// unrelated Apply — still resolve to "". Without the negative half the list
// could be widened to a bare "Grooming"/"Apply" prefix and this file would
// still pass while forbidding legitimate report ingestion.
func TestBannedSymbolCoversGroomingApplySymbols(t *testing.T) {
	banned := []string{
		"GroomingMutator",
		"ApplyGroomingMutation",
		"MutatorFor",
		"ApplyGrooming",
		"GroomingMutationRequest",
		"GroomingMutationResult",
		"GroomingMutationRecord",
	}
	for _, name := range banned {
		if bannedSymbol(name) == "" {
			t.Errorf("bannedSymbol(%q) = \"\", want it banned: a grooming mutation dispatched from the MCP agent tool surface would bypass the apply gate (ADR-064 / #2237)", name)
		}
	}
	allowed := []string{
		"GroomingReport",
		"ParseGroomingReport",
		"ArtifactKindGroomingReport",
		"Apply",
		"ApplyFiling",
	}
	for _, name := range allowed {
		if got := bannedSymbol(name); got != "" {
			t.Errorf("bannedSymbol(%q) = %q, want \"\": ingesting a grooming REPORT is not dispatching a grooming MUTATION", name, got)
		}
	}
}
