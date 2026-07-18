// Package reachability is a symbol-reachability engine that validates the
// phase partition of a plan's split_proposal against the compiler's view of
// the working tree. It answers a single question per phase: do the files a
// phase declares match the files reachability says belong together to keep the
// phase compile-atomic?
//
// It is NOT a name-grep. The engine loads the working tree through
// go/packages + go/types (the real compiler front end) and reasons over the
// resolved type graph, so it detects a cross-boundary use site even when the
// source shares no textual token with the defining symbol — a dot-imported or
// aliased construction, an interface implemented across a package boundary, a
// test fake reading a struct field. A grep keyed on "<pkg>.<Symbol>" misses
// every one of those; type resolution does not.
//
// Three cross-boundary compile-breaking use-site KINDS are detected, each
// pairing a defining symbol (in one phase) with a use site (in a different
// phase):
//
//   - construction_site — a composite literal T{...} whose named type T is
//     defined in another phase. Breaks when T's fields change.
//   - interface_implementer — a concrete type in one phase that structurally
//     implements an interface defined in another phase. Breaks when the
//     interface's method set changes.
//   - test_fake_field_reader — a field selection x.F inside a _test.go file
//     where F's struct is defined in another phase. Breaks when F is renamed
//     or removed.
//
// The engine is advisory and FAIL-OPEN. packages.Load returns a nil error even
// when an individual package fails to parse or type-check — the failures land
// in each package's .Errors slice, not the returned error. Computing over a
// broken or incomplete type graph would produce garbage, so Analyze walks
// EVERY loaded package (including the synthetic *.test variants produced by
// Tests:true) and, if ANY has a non-empty .Errors, returns a soft
// Result{Available:false, SkipReason} the caller logs and drops rather than a
// partial answer. The same soft skip covers a load-level error, a missing or
// empty split proposal, and a source root with no loadable packages. Analyze
// never returns an error and never panics on malformed input; a caller can
// always publish or drop the Result unconditionally.
//
// Result is the wire contract. The runner ships it to the server as JSON and
// the server owns a mirroring decode struct across the module boundary (the
// backend cannot import this package), so the json tags here are load-bearing
// — a drift between the two structs fails the advisory open silently. See the
// package README for the long-form contract and the transport-contract tests
// that lock the tags.
package reachability

import (
	"go/ast"
	"go/token"
	"go/types"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/tools/go/packages"
)

// Violation kind constants. These strings cross the wire in Result.Violations
// and are asserted by the server decode + get_plan render, so they are part of
// the transport contract — do not rename without updating the mirroring side.
const (
	KindConstructionSite     = "construction_site"
	KindInterfaceImplementer = "interface_implementer"
	KindTestFakeFieldReader  = "test_fake_field_reader"
)

// File is one declared file in a split-proposal phase. Path is repo-relative
// (forward-slash separated, matching the plan artifact); Operation is the
// plan's scope operation ("modify" or "create"). A create-only file does not
// yet exist in the working tree, so it contributes no definitions and no use
// sites — it is counted toward the declared total but is naturally absent from
// the loaded type graph (the repartitioning-of-existing-code case, #1855).
type File struct {
	Path      string
	Operation string
}

// Phase is one split-proposal phase: a title and the files it declares. The
// caller builds these from the plan's split_proposal.phases; the engine does
// not depend on the plan package.
type Phase struct {
	Title string
	Files []File
}

// Result is the engine's advisory output and the runner→server wire contract.
// Available:false means the sweep was skipped (fail-open) — Phases and
// Violations are then empty and SkipReason explains why. Available:true means
// the sweep ran to completion; Phases carries the per-phase declared-vs-derived
// counts and Violations the cross-boundary use sites, either of which may be
// empty for a clean partition.
type Result struct {
	Available  bool          `json:"available"`
	SkipReason string        `json:"skip_reason,omitempty"`
	Phases     []PhaseResult `json:"phases,omitempty"`
	Violations []Violation   `json:"violations,omitempty"`
}

// PhaseResult carries one phase's declared-vs-derived file counts. DeclaredCount
// is the number of files the phase declares. DerivedCount is the size of the
// reachability-derived file set: the declared files plus any file in another
// phase that references a symbol this phase defines through one of the three
// compile-breaking kinds. DerivedCount > DeclaredCount signals the phase's
// symbols leak into sibling phases — the partition would produce a
// non-compiling intermediate. They are equal for a clean partition.
type PhaseResult struct {
	Index         int    `json:"index"`
	Title         string `json:"title"`
	DeclaredCount int    `json:"declared_count"`
	DerivedCount  int    `json:"derived_count"`
}

// Violation is one cross-boundary compile-breaking use site: a Symbol defined
// in DefFile (belonging to phase DefPhase) referenced from UseFile (belonging
// to the different phase UsePhase), classified by Kind. All file paths are
// repo-relative forward-slash paths.
type Violation struct {
	Kind     string `json:"kind"`
	Symbol   string `json:"symbol"`
	DefFile  string `json:"def_file"`
	DefPhase int    `json:"def_phase"`
	UseFile  string `json:"use_file"`
	UsePhase int    `json:"use_phase"`
}

// loadMode is the go/packages mode the engine needs: names + files to map
// phase paths, syntax + full type info to resolve use sites, and the
// dependency graph so imported types (including workspace-sibling packages)
// resolve. Tests:true adds the synthetic *.test variants so _test.go field
// readers and their type info are loaded.
const loadMode = packages.NeedName |
	packages.NeedFiles |
	packages.NeedCompiledGoFiles |
	packages.NeedImports |
	packages.NeedDeps |
	packages.NeedSyntax |
	packages.NeedTypes |
	packages.NeedTypesInfo

// Analyze loads the Go working tree rooted at sourceRoot and validates the
// given split-proposal phases. It is fail-open: any load failure, any loaded
// package carrying a type/parse error, or an absent/degenerate set of phases
// yields Result{Available:false} with a SkipReason instead of an error. On a
// clean load it returns the per-phase declared-vs-derived counts and the
// cross-boundary violations.
func Analyze(phases []Phase, sourceRoot string) Result {
	if len(phases) == 0 {
		return skip("no split_proposal phases to analyze")
	}

	// phaseOfFile maps a repo-relative file path to its declared phase index.
	// A file declared by more than one phase keeps its first (lowest-index)
	// owner; the plan validator already rejects that structurally, so this is
	// only a defensive tiebreak.
	phaseOfFile := make(map[string]int)
	declaredTotal := 0
	for i, ph := range phases {
		for _, f := range ph.Files {
			declaredTotal++
			p := normalizePath(f.Path)
			if _, seen := phaseOfFile[p]; !seen {
				phaseOfFile[p] = i
			}
		}
	}
	if declaredTotal == 0 {
		return skip("split_proposal declares no phase files")
	}

	cfg := &packages.Config{
		Mode: loadMode,
		Dir:  sourceRoot,
		// Tests:true loads the synthetic *.test package variants so _test.go
		// field readers are visible AND so their .Errors are inspected for the
		// fail-open gate.
		Tests: true,
	}
	pkgs, err := packages.Load(cfg, "./...")
	if err != nil {
		return skip("packages.Load failed: " + err.Error())
	}
	if len(pkgs) == 0 {
		return skip("no Go packages loaded from source root")
	}

	// Fail-open on ANY per-package error. packages.Load returns a nil error
	// even when a package fails to parse or type-check — the failures surface
	// only here, in each package's .Errors. Walk every loaded package,
	// including the *.test variants and dependencies, and skip the whole
	// advisory rather than compute over a broken/incomplete type graph.
	var broken string
	packages.Visit(pkgs, nil, func(p *packages.Package) {
		if broken == "" && len(p.Errors) > 0 {
			broken = p.Errors[0].Error()
		}
	})
	if broken != "" {
		return skip("loaded package has errors: " + broken)
	}

	an := &analyzer{
		sourceRoot:  sourceRoot,
		phaseOfFile: phaseOfFile,
		seenViol:    make(map[string]bool),
		seenType:    make(map[string]bool),
	}
	packages.Visit(pkgs, nil, func(p *packages.Package) {
		// All packages from one packages.Load share a single FileSet; capture
		// it so object positions (which carry no Fset) can be resolved to
		// filenames.
		if an.fset == nil {
			an.fset = p.Fset
		}
		an.scanPackage(p)
	})
	an.matchInterfaces()

	return an.result(phases)
}

// typeEntry is a phase-attributed named type collected for global interface
// matching: the type plus its repo-relative def file and phase.
type typeEntry struct {
	named *types.Named
	file  string
	phase int
}

// analyzer accumulates violations across every loaded package.
type analyzer struct {
	sourceRoot  string
	fset        *token.FileSet
	phaseOfFile map[string]int
	seenViol    map[string]bool // dedupe violations across variant reprocessing
	seenType    map[string]bool // dedupe collected type entries across variants
	concretes   []typeEntry     // phase-attributed concrete named types
	ifaces      []typeEntry     // phase-attributed interfaces with >=1 method
	violations  []Violation
}

// scanPackage runs the per-file detection kinds over one loaded package and
// collects its phase-attributed named types for the global interface match. A
// package with no type info (a dependency loaded name-only) or no
// phase-attributed file is skipped — a violation requires an attributed use
// site, so packages outside the split can never contribute one, and pruning
// them here avoids walking the entire dependency graph's syntax. Every package
// variant is processed (the base package lacks the _test.go syntax the *.test
// variant carries); duplicate work across variants is collapsed by the
// violation- and type-level dedupe sets.
func (a *analyzer) scanPackage(p *packages.Package) {
	if p.Types == nil || p.TypesInfo == nil {
		return
	}
	if !a.pkgHasPhaseFile(p) {
		return
	}
	a.scanConstructionSites(p)
	a.scanFieldReaders(p)
	a.collectTypes(p)
}

// pkgHasPhaseFile reports whether any of the package's syntax files is
// phase-attributed.
func (a *analyzer) pkgHasPhaseFile(p *packages.Package) bool {
	for _, file := range p.Syntax {
		if _, ok := a.phaseOf(p, file); ok {
			return true
		}
	}
	return false
}

// scanConstructionSites flags composite literals T{...} whose named type T is
// defined in a different phase than the literal.
func (a *analyzer) scanConstructionSites(p *packages.Package) {
	for _, file := range p.Syntax {
		usePhase, ok := a.phaseOf(p, file)
		if !ok {
			continue
		}
		useFile := a.relFile(p, file)
		ast.Inspect(file, func(n ast.Node) bool {
			lit, isLit := n.(*ast.CompositeLit)
			if !isLit {
				return true
			}
			tv, found := p.TypesInfo.Types[lit]
			if !found || tv.Type == nil {
				return true
			}
			named := namedOf(tv.Type)
			if named == nil {
				return true
			}
			if _, isStruct := named.Underlying().(*types.Struct); !isStruct {
				return true
			}
			a.recordDefUse(named.Obj(), KindConstructionSite, named.Obj().Name(), useFile, usePhase)
			return true
		})
	}
}

// scanFieldReaders flags field selections x.F inside _test.go files where F's
// struct is defined in a different phase than the test file.
func (a *analyzer) scanFieldReaders(p *packages.Package) {
	for _, file := range p.Syntax {
		usePhase, ok := a.phaseOf(p, file)
		if !ok {
			continue
		}
		useFile := a.relFile(p, file)
		if !strings.HasSuffix(useFile, "_test.go") {
			continue
		}
		ast.Inspect(file, func(n ast.Node) bool {
			sel, isSel := n.(*ast.SelectorExpr)
			if !isSel {
				return true
			}
			selection, found := p.TypesInfo.Selections[sel]
			if !found || selection.Kind() != types.FieldVal {
				return true
			}
			fieldVar, isVar := selection.Obj().(*types.Var)
			if !isVar || !fieldVar.IsField() {
				return true
			}
			owner := fieldOwner(selection.Recv())
			symbol := fieldVar.Name()
			if owner != nil {
				symbol = owner.Obj().Name() + "." + fieldVar.Name()
			}
			a.recordDefUse(fieldVar, KindTestFakeFieldReader, symbol, useFile, usePhase)
			return true
		})
	}
}

// collectTypes records the package's phase-attributed named types, split into
// concrete types and interfaces (with at least one method), for the global
// interface-implementer match. Cross-package implementation (an interface in
// one phase's package satisfied by a concrete type in another phase's package)
// is the common case, so the match cannot be scoped to a single package's
// scope. Entries are deduped by def-file + type name so the base + *.test
// variants of a package contribute each type once.
func (a *analyzer) collectTypes(p *packages.Package) {
	scope := p.Types.Scope()
	for _, name := range scope.Names() {
		obj, isType := scope.Lookup(name).(*types.TypeName)
		if !isType {
			continue
		}
		named, isNamed := obj.Type().(*types.Named)
		if !isNamed {
			continue
		}
		file, phase, ok := a.phaseOfObj(obj)
		if !ok {
			continue
		}
		key := file + "." + name
		if a.seenType[key] {
			continue
		}
		a.seenType[key] = true
		entry := typeEntry{named: named, file: file, phase: phase}
		if it, isIface := named.Underlying().(*types.Interface); isIface {
			if it.NumMethods() > 0 {
				a.ifaces = append(a.ifaces, entry)
			}
			continue
		}
		a.concretes = append(a.concretes, entry)
	}
}

// matchInterfaces flags each concrete named type that structurally implements
// an interface defined in a different phase. The interface is the defining
// symbol (its method set is what breaks the implementer); the concrete type's
// file is the use site. Run once after every package is collected.
func (a *analyzer) matchInterfaces() {
	for _, c := range a.concretes {
		for _, i := range a.ifaces {
			if c.phase == i.phase {
				continue
			}
			it, ok := i.named.Underlying().(*types.Interface)
			if !ok || it.NumMethods() == 0 {
				continue
			}
			if !implements(c.named, it) {
				continue
			}
			a.addViolation(Violation{
				Kind:     KindInterfaceImplementer,
				Symbol:   i.named.Obj().Name(),
				DefFile:  i.file,
				DefPhase: i.phase,
				UseFile:  c.file,
				UsePhase: c.phase,
			})
		}
	}
}

// recordDefUse records a violation when obj is defined in a phase-attributed
// file whose phase differs from usePhase.
func (a *analyzer) recordDefUse(obj types.Object, kind, symbol, useFile string, usePhase int) {
	if obj == nil {
		return
	}
	defFile := a.objFile(obj)
	if defFile == "" {
		return
	}
	defPhase, ok := a.phaseOfFile[defFile]
	if !ok || defPhase == usePhase {
		return
	}
	a.addViolation(Violation{
		Kind:     kind,
		Symbol:   symbol,
		DefFile:  defFile,
		DefPhase: defPhase,
		UseFile:  useFile,
		UsePhase: usePhase,
	})
}

// addViolation appends a violation, deduping on its full identity so a file
// reprocessed under multiple package variants contributes it once.
func (a *analyzer) addViolation(v Violation) {
	k := v.Kind + "|" + v.Symbol + "|" + v.DefFile + "|" + v.UseFile
	if a.seenViol[k] {
		return
	}
	a.seenViol[k] = true
	a.violations = append(a.violations, v)
}

// result assembles the final Result: per-phase declared/derived counts plus the
// sorted violation list.
func (a *analyzer) result(phases []Phase) Result {
	// derived[i] is the set of files reachability attributes to phase i: its
	// declared files plus any sibling-phase file that references a symbol phase
	// i defines.
	derived := make([]map[string]bool, len(phases))
	declared := make([]int, len(phases))
	for i, ph := range phases {
		derived[i] = make(map[string]bool)
		for _, f := range ph.Files {
			p := normalizePath(f.Path)
			derived[i][p] = true
			declared[i]++
		}
	}
	for _, v := range a.violations {
		if v.DefPhase >= 0 && v.DefPhase < len(derived) {
			derived[v.DefPhase][v.UseFile] = true
		}
	}

	phaseResults := make([]PhaseResult, len(phases))
	for i, ph := range phases {
		phaseResults[i] = PhaseResult{
			Index:         i,
			Title:         ph.Title,
			DeclaredCount: declared[i],
			DerivedCount:  len(derived[i]),
		}
	}

	sort.Slice(a.violations, func(i, j int) bool {
		vi, vj := a.violations[i], a.violations[j]
		if vi.DefPhase != vj.DefPhase {
			return vi.DefPhase < vj.DefPhase
		}
		if vi.UsePhase != vj.UsePhase {
			return vi.UsePhase < vj.UsePhase
		}
		if vi.Kind != vj.Kind {
			return vi.Kind < vj.Kind
		}
		if vi.Symbol != vj.Symbol {
			return vi.Symbol < vj.Symbol
		}
		return vi.UseFile < vj.UseFile
	})

	return Result{
		Available:  true,
		Phases:     phaseResults,
		Violations: a.violations,
	}
}

// phaseOf returns the phase index of a syntax file if it is phase-attributed.
func (a *analyzer) phaseOf(p *packages.Package, file *ast.File) (int, bool) {
	rel := a.relFile(p, file)
	if rel == "" {
		return 0, false
	}
	idx, ok := a.phaseOfFile[rel]
	return idx, ok
}

// phaseOfObj returns the repo-relative def file and phase index of an object if
// it is defined in a phase-attributed file.
func (a *analyzer) phaseOfObj(obj types.Object) (string, int, bool) {
	file := a.objFile(obj)
	if file == "" {
		return "", 0, false
	}
	idx, ok := a.phaseOfFile[file]
	return file, idx, ok
}

// objFile returns the repo-relative file that defines obj, or "" if it is not
// under the source root (stdlib, module cache, synthetic). Objects carry no
// Fset, so positions resolve through the shared fileset captured at load.
func (a *analyzer) objFile(obj types.Object) string {
	if obj == nil || a.fset == nil || !obj.Pos().IsValid() {
		return ""
	}
	return a.relFromAbs(a.fset.Position(obj.Pos()).Filename)
}

// relFile returns the repo-relative path of a syntax file.
func (a *analyzer) relFile(p *packages.Package, file *ast.File) string {
	return a.relFromAbs(p.Fset.Position(file.Pos()).Filename)
}

// relFromAbs converts an absolute filesystem path to a repo-relative
// forward-slash path under the source root, or "" if it is outside the root.
func (a *analyzer) relFromAbs(abs string) string {
	if abs == "" {
		return ""
	}
	rel, err := filepath.Rel(a.sourceRoot, abs)
	if err != nil {
		return ""
	}
	rel = filepath.ToSlash(rel)
	if rel == "" || strings.HasPrefix(rel, "../") {
		return ""
	}
	return rel
}

// --- package-free helpers ---

// namedOf unwraps a type to its *types.Named, following pointers, or returns
// nil for an unnamed type.
func namedOf(t types.Type) *types.Named {
	switch u := t.(type) {
	case *types.Named:
		return u
	case *types.Pointer:
		return namedOf(u.Elem())
	default:
		return nil
	}
}

// fieldOwner returns the named struct type that owns a selected field, given
// the selection's receiver type, or nil.
func fieldOwner(recv types.Type) *types.Named {
	return namedOf(recv)
}

// implements reports whether the concrete type (value or pointer) satisfies the
// interface.
func implements(concrete *types.Named, iface *types.Interface) bool {
	if types.Implements(concrete, iface) {
		return true
	}
	return types.Implements(types.NewPointer(concrete), iface)
}

// skip builds a fail-open Result.
func skip(reason string) Result {
	return Result{Available: false, SkipReason: reason}
}

// normalizePath normalizes a declared phase path to a forward-slash,
// cleaned, relative form so it matches the loader-derived file keys.
func normalizePath(p string) string {
	p = filepath.ToSlash(filepath.Clean(p))
	return strings.TrimPrefix(p, "./")
}
