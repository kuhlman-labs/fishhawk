package reachability

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// analyzeModule writes a golden Go module tree into a temp dir and runs Analyze
// against it. GOWORK is forced off so the standalone module loads on its own
// rather than being rejected by the repo's parent go.work.
func analyzeModule(t *testing.T, files map[string]string, phases []Phase) Result {
	t.Helper()
	t.Setenv("GOWORK", "off")
	dir := t.TempDir()
	for rel, content := range files {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", full, err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", full, err)
		}
	}
	return Analyze(phases, dir)
}

// modifyPhase builds a phase whose files are all modify-target.
func modifyPhase(title string, files ...string) Phase {
	ph := Phase{Title: title}
	for _, f := range files {
		ph.Files = append(ph.Files, File{Path: f, Operation: "modify"})
	}
	return ph
}

const goMod = "module example.com/m\n\ngo 1.25\n"

func findViolation(res Result, kind, symbol string) (Violation, bool) {
	for _, v := range res.Violations {
		if v.Kind == kind && v.Symbol == symbol {
			return v, true
		}
	}
	return Violation{}, false
}

func phaseByIndex(res Result, idx int) (PhaseResult, bool) {
	for _, p := range res.Phases {
		if p.Index == idx {
			return p, true
		}
	}
	return PhaseResult{}, false
}

// TestAnalyze_NotAGrep proves the engine resolves a cross-boundary construction
// site through the type graph, not textual matching: the use site dot-imports
// the defining package (so the source carries NO package-qualified reference),
// and a same-named decoy type in a third package would defeat any name grep.
// The violation must attribute the construction to the real defining file, not
// the decoy.
func TestAnalyze_NotAGrep(t *testing.T) {
	useSrc := `package b

import . "example.com/m/a"

func New() any { return Widget{Size: 3} }
`
	res := analyzeModule(t, map[string]string{
		"go.mod":          goMod,
		"a/widget.go":     "package a\n\ntype Widget struct {\n\tSize int\n}\n",
		"decoy/widget.go": "package decoy\n\ntype Widget struct {\n\tSize int\n}\n",
		"b/use.go":        useSrc,
	}, []Phase{
		modifyPhase("expand", "a/widget.go"),
		modifyPhase("use", "b/use.go"),
	})

	if !res.Available {
		t.Fatalf("expected Available, got skip: %s", res.SkipReason)
	}
	v, ok := findViolation(res, KindConstructionSite, "Widget")
	if !ok {
		t.Fatalf("expected construction_site violation for Widget; got %+v", res.Violations)
	}
	// The load-bearing not-a-grep assertion: resolved to the real definition,
	// NOT the identically-named decoy, even though the use site names neither.
	if v.DefFile != "a/widget.go" {
		t.Errorf("DefFile = %q, want a/widget.go (type resolution, not the decoy)", v.DefFile)
	}
	if v.DefPhase != 0 || v.UsePhase != 1 || v.UseFile != "b/use.go" {
		t.Errorf("violation phases/use wrong: %+v", v)
	}
	// Guard the premise: the use-site source shares no textual token a grep for
	// the defining package or a qualified reference could key on.
	if strings.Contains(useSrc, "a.Widget") || strings.Contains(useSrc, "decoy") {
		t.Fatalf("test premise broken: use site should carry no qualified reference")
	}
}

// TestAnalyze_ConstructionSite is the struct-construction case: a composite
// literal for a struct defined in a sibling phase is flagged with the symbol
// and both files, and the defining phase's derived count grows past its
// declared count.
func TestAnalyze_ConstructionSite(t *testing.T) {
	res := analyzeModule(t, map[string]string{
		"go.mod":           goMod,
		"core/params.go":   "package core\n\ntype Params struct {\n\tA int\n\tB int\n}\n",
		"caller/caller.go": "package caller\n\nimport \"example.com/m/core\"\n\nfunc Make() core.Params { return core.Params{A: 1, B: 2} }\n",
	}, []Phase{
		modifyPhase("define", "core/params.go"),
		modifyPhase("call", "caller/caller.go"),
	})

	if !res.Available {
		t.Fatalf("expected Available, got skip: %s", res.SkipReason)
	}
	v, ok := findViolation(res, KindConstructionSite, "Params")
	if !ok {
		t.Fatalf("expected construction_site for Params; got %+v", res.Violations)
	}
	if v.DefFile != "core/params.go" || v.DefPhase != 0 {
		t.Errorf("def wrong: %+v", v)
	}
	if v.UseFile != "caller/caller.go" || v.UsePhase != 1 {
		t.Errorf("use wrong: %+v", v)
	}
	p0, _ := phaseByIndex(res, 0)
	if p0.DeclaredCount != 1 || p0.DerivedCount != 2 {
		t.Errorf("phase 0 counts = declared %d derived %d, want 1/2", p0.DeclaredCount, p0.DerivedCount)
	}
}

// TestAnalyze_InterfaceAndTestFake covers the remaining two kinds together:
// (1) a concrete type in one phase that structurally implements an interface
// defined in another — the implementer's package never even names the
// interface, so only the type graph connects them — and (2) a _test.go field
// read of a struct field defined in a sibling phase.
func TestAnalyze_InterfaceAndTestFake(t *testing.T) {
	res := analyzeModule(t, map[string]string{
		"go.mod": goMod,
		"svc/svc.go": "package svc\n\n" +
			"type Store interface {\n\tGet(id string) string\n}\n\n" +
			"type Record struct {\n\tName string\n}\n",
		"impl/impl.go": "package impl\n\n" +
			"type MemStore struct{}\n\n" +
			"func (MemStore) Get(id string) string { return \"\" }\n",
		"fake/doc.go": "package fake\n",
		"fake/fake_test.go": "package fake\n\n" +
			"import (\n\t\"testing\"\n\n\t\"example.com/m/svc\"\n)\n\n" +
			"func TestRecord(t *testing.T) {\n\tr := new(svc.Record)\n\tif r.Name != \"\" {\n\t\tt.Fatal(\"unexpected\")\n\t}\n}\n",
	}, []Phase{
		modifyPhase("define", "svc/svc.go"),
		modifyPhase("consume", "impl/impl.go", "fake/fake_test.go"),
	})

	if !res.Available {
		t.Fatalf("expected Available, got skip: %s", res.SkipReason)
	}

	iv, ok := findViolation(res, KindInterfaceImplementer, "Store")
	if !ok {
		t.Fatalf("expected interface_implementer for Store; got %+v", res.Violations)
	}
	if iv.DefFile != "svc/svc.go" || iv.DefPhase != 0 || iv.UseFile != "impl/impl.go" || iv.UsePhase != 1 {
		t.Errorf("interface violation wrong: %+v", iv)
	}

	fv, ok := findViolation(res, KindTestFakeFieldReader, "Record.Name")
	if !ok {
		t.Fatalf("expected test_fake_field_reader for Record.Name; got %+v", res.Violations)
	}
	if fv.DefFile != "svc/svc.go" || fv.DefPhase != 0 || fv.UseFile != "fake/fake_test.go" || fv.UsePhase != 1 {
		t.Errorf("field-reader violation wrong: %+v", fv)
	}

	// Phase 0 defines symbols pulled into phase 1 by both kinds, so its derived
	// set (svc + impl + fake_test) exceeds its single declared file.
	p0, _ := phaseByIndex(res, 0)
	if p0.DeclaredCount != 1 || p0.DerivedCount != 3 {
		t.Errorf("phase 0 counts = declared %d derived %d, want 1/3", p0.DeclaredCount, p0.DerivedCount)
	}
}

// TestAnalyze_CleanPartition asserts a partition with no cross-boundary
// references yields zero violations and equal declared/derived counts per
// phase.
func TestAnalyze_CleanPartition(t *testing.T) {
	res := analyzeModule(t, map[string]string{
		"go.mod":         goMod,
		"alpha/alpha.go": "package alpha\n\nfunc A() int { return 1 }\n",
		"beta/beta.go":   "package beta\n\nfunc B() int { return 2 }\n",
	}, []Phase{
		modifyPhase("one", "alpha/alpha.go"),
		modifyPhase("two", "beta/beta.go"),
	})

	if !res.Available {
		t.Fatalf("expected Available, got skip: %s", res.SkipReason)
	}
	if len(res.Violations) != 0 {
		t.Fatalf("expected zero violations, got %+v", res.Violations)
	}
	for _, p := range res.Phases {
		if p.DeclaredCount != p.DerivedCount {
			t.Errorf("phase %d declared %d != derived %d on a clean partition", p.Index, p.DeclaredCount, p.DerivedCount)
		}
	}
}

// TestAnalyze_PerPackageError_FailOpen is the operator's load-bearing case:
// packages.Load returns a nil error even when a package fails to type-check,
// with the failure only in that package's .Errors. The engine must walk every
// loaded package, see the error, and skip the whole advisory rather than
// compute over a broken type graph.
func TestAnalyze_PerPackageError_FailOpen(t *testing.T) {
	res := analyzeModule(t, map[string]string{
		"go.mod":       goMod,
		"bad/bad.go":   "package bad\n\nvar X int = \"not an int\"\n", // type error
		"good/good.go": "package good\n\nfunc G() int { return 1 }\n",
	}, []Phase{
		modifyPhase("a", "bad/bad.go"),
		modifyPhase("b", "good/good.go"),
	})

	if res.Available {
		t.Fatalf("expected fail-open skip on a per-package error, got Available with %+v", res.Violations)
	}
	if !strings.Contains(res.SkipReason, "loaded package has errors") {
		t.Errorf("SkipReason = %q, want the per-package-error branch", res.SkipReason)
	}
	if len(res.Phases) != 0 || len(res.Violations) != 0 {
		t.Errorf("fail-open must publish nothing, got phases=%d violations=%d", len(res.Phases), len(res.Violations))
	}
}

// TestAnalyze_LoadError_FailOpen covers the load-level failure branch: an
// unloadable source root skips the advisory.
func TestAnalyze_LoadError_FailOpen(t *testing.T) {
	t.Setenv("GOWORK", "off")
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	res := Analyze([]Phase{modifyPhase("a", "x.go"), modifyPhase("b", "y.go")}, missing)
	if res.Available {
		t.Fatalf("expected fail-open on an unloadable source root, got Available")
	}
	if res.SkipReason == "" {
		t.Errorf("expected a SkipReason on load failure")
	}
	if len(res.Phases) != 0 || len(res.Violations) != 0 {
		t.Errorf("fail-open must publish nothing, got phases=%d violations=%d", len(res.Phases), len(res.Violations))
	}
}

// TestAnalyze_MissingSplitProposal_Skip covers the degenerate-input skips: no
// phases, and phases that declare no files.
func TestAnalyze_MissingSplitProposal_Skip(t *testing.T) {
	if res := Analyze(nil, "."); res.Available || res.SkipReason == "" {
		t.Errorf("nil phases: want skip with reason, got %+v", res)
	}
	if res := Analyze([]Phase{{Title: "empty"}}, "."); res.Available || res.SkipReason == "" {
		t.Errorf("no phase files: want skip with reason, got %+v", res)
	}
}

// TestAnalyze_CreateOnlyFile_NoDefinitions asserts a create-only phase file —
// which does not yet exist in the working tree — is counted toward the declared
// total but contributes no definitions and no violations (the
// repartitioning-of-existing-code case, #1855).
func TestAnalyze_CreateOnlyFile_NoDefinitions(t *testing.T) {
	res := analyzeModule(t, map[string]string{
		"go.mod":         goMod,
		"core/params.go": "package core\n\ntype Params struct {\n\tA int\n}\n",
	}, []Phase{
		{Title: "expand", Files: []File{
			{Path: "feature/new.go", Operation: "create"}, // not on disk
			{Path: "core/params.go", Operation: "modify"},
		}},
		modifyPhase("later", "core/params.go"),
	})

	if !res.Available {
		t.Fatalf("expected Available, got skip: %s", res.SkipReason)
	}
	if len(res.Violations) != 0 {
		t.Fatalf("create-only file must attribute no violations, got %+v", res.Violations)
	}
	p0, ok := phaseByIndex(res, 0)
	if !ok {
		t.Fatal("missing phase 0")
	}
	if p0.DeclaredCount != 2 {
		t.Errorf("declared count = %d, want 2 (create file counted)", p0.DeclaredCount)
	}
}

// TestResult_JSONWireKeys locks the runner→server JSON transport contract at
// its source: the exact wire keys the server decode struct and get_plan render
// mirror across the module boundary. A drift here fails the advisory open
// silently, so slice 2's decode is pinned to these keys.
func TestResult_JSONWireKeys(t *testing.T) {
	res := Result{
		Available:  true,
		SkipReason: "",
		Phases: []PhaseResult{
			{Index: 0, Title: "expand", DeclaredCount: 1, DerivedCount: 2},
		},
		Violations: []Violation{
			{Kind: KindConstructionSite, Symbol: "Widget", DefFile: "a/widget.go", DefPhase: 0, UseFile: "b/use.go", UsePhase: 1},
		},
	}
	data, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(data)
	for _, key := range []string{
		`"available"`, `"phases"`, `"violations"`,
		`"index"`, `"title"`, `"declared_count"`, `"derived_count"`,
		`"kind"`, `"symbol"`, `"def_file"`, `"def_phase"`, `"use_file"`, `"use_phase"`,
	} {
		if !strings.Contains(got, key) {
			t.Errorf("wire JSON missing key %s: %s", key, got)
		}
	}
	// skip_reason is omitempty; absent when empty.
	if strings.Contains(got, "skip_reason") {
		t.Errorf("skip_reason should be omitted when empty: %s", got)
	}

	// A skipped result carries the reason and omits the empty slices.
	skipped, err := json.Marshal(skip("boom"))
	if err != nil {
		t.Fatalf("marshal skip: %v", err)
	}
	if !strings.Contains(string(skipped), `"skip_reason":"boom"`) {
		t.Errorf("skipped result should carry skip_reason: %s", skipped)
	}
	if strings.Contains(string(skipped), `"phases"`) || strings.Contains(string(skipped), `"violations"`) {
		t.Errorf("skipped result should omit empty phases/violations: %s", skipped)
	}
}
