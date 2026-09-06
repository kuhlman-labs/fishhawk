package wirecontract

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeGo writes a Go source fixture into dir and returns its path. Fixtures
// are temp-dir sources written by the test, never the real tree, so a RED lands
// on the behavioral assertion and not on fixture setup.
func writeGo(t *testing.T, dir, name, src string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

// onePairCheck runs checkPair for a single pair against a fixture root (a temp
// dir treated as the repo root), so a unit test can drive one contract in
// isolation.
func onePairCheck(root string, p Pair) []error {
	return checkPair(root, p)
}

func hasErrContaining(errs []error, sub string) bool {
	for _, e := range errs {
		if strings.Contains(e.Error(), sub) {
			return true
		}
	}
	return false
}

// --- C1: consumer-only json name is a violation in both modes ---

func TestCheck_ConsumerOnlyNameIsViolation(t *testing.T) {
	dir := t.TempDir()
	writeGo(t, dir, "emit.go", "package p\ntype E struct{ A string `json:\"a\"` }\n")
	writeGo(t, dir, "cons.go", "package p\ntype C struct{ A string `json:\"a\"`\n B string `json:\"b\"` }\n")
	for _, mode := range []Mode{ModeExact, ModeSubset} {
		p := Pair{Name: "t", Anchor: "x", Emitter: Endpoint{File: "emit.go", Type: "E"}, Consumer: Endpoint{File: "cons.go", Type: "C"}, Mode: mode}
		errs := onePairCheck(dir, p)
		if len(errs) == 0 {
			t.Fatalf("mode %d: expected a violation for consumer-only name %q", mode, "b")
		}
		if mode == ModeSubset && !hasErrContaining(errs, "never emits") {
			t.Errorf("subset: expected a 'never emits' violation, got %v", errs)
		}
	}
}

// --- C2: undeclared emitter-only name is a violation under ModeSubset;
// an Allowance / EmitterOnlyUnchecked suppresses it ---

func TestCheck_UndeclaredEmitterOnlyNameIsViolation(t *testing.T) {
	dir := t.TempDir()
	writeGo(t, dir, "emit.go", "package p\ntype E struct{ A string `json:\"a\"`\n Extra string `json:\"extra\"` }\n")
	writeGo(t, dir, "cons.go", "package p\ntype C struct{ A string `json:\"a\"` }\n")
	p := Pair{Name: "t", Anchor: "x", Emitter: Endpoint{File: "emit.go", Type: "E"}, Consumer: Endpoint{File: "cons.go", Type: "C"}, Mode: ModeSubset}
	errs := onePairCheck(dir, p)
	if !hasErrContaining(errs, "AllowedEmitterOnly") {
		t.Fatalf("expected an emitter-only violation naming AllowedEmitterOnly, got %v", errs)
	}
}

func TestCheck_AllowedEmitterOnlySuppresses(t *testing.T) {
	dir := t.TempDir()
	writeGo(t, dir, "emit.go", "package p\ntype E struct{ A string `json:\"a\"`\n Extra string `json:\"extra\"` }\n")
	writeGo(t, dir, "cons.go", "package p\ntype C struct{ A string `json:\"a\"` }\n")
	p := Pair{Name: "t", Anchor: "x", Emitter: Endpoint{File: "emit.go", Type: "E"}, Consumer: Endpoint{File: "cons.go", Type: "C"}, Mode: ModeSubset,
		AllowedEmitterOnly: []Allowance{{JSONName: "extra", Reason: "SPA only"}}}
	if errs := onePairCheck(dir, p); len(errs) != 0 {
		t.Fatalf("an allowance must suppress the emitter-only violation, got %v", errs)
	}
}

func TestCheck_EmitterOnlyUncheckedSuppresses(t *testing.T) {
	dir := t.TempDir()
	writeGo(t, dir, "emit.go", "package p\ntype E struct{ A string `json:\"a\"`\n Extra string `json:\"extra\"` }\n")
	writeGo(t, dir, "cons.go", "package p\ntype C struct{ A string `json:\"a\"` }\n")
	p := Pair{Name: "t", Anchor: "x", Emitter: Endpoint{File: "emit.go", Type: "E"}, Consumer: Endpoint{File: "cons.go", Type: "C"}, Mode: ModeSubset, EmitterOnlyUnchecked: true}
	if errs := onePairCheck(dir, p); len(errs) != 0 {
		t.Fatalf("EmitterOnlyUnchecked must suppress the emitter-only violation, got %v", errs)
	}
}

// --- C3: option (omitempty) drift is a violation under ModeExact, tolerated
// under ModeSubset (the deliberate asymmetry) ---

func TestCheck_ExactModeRejectsOptionDrift(t *testing.T) {
	dir := t.TempDir()
	writeGo(t, dir, "emit.go", "package p\ntype E struct{ X string `json:\"x,omitempty\"` }\n")
	writeGo(t, dir, "cons.go", "package p\ntype C struct{ X string `json:\"x\"` }\n")
	p := Pair{Name: "t", Anchor: "x", Emitter: Endpoint{File: "emit.go", Type: "E"}, Consumer: Endpoint{File: "cons.go", Type: "C"}, Mode: ModeExact}
	if errs := onePairCheck(dir, p); !hasErrContaining(errs, "ModeExact tag mismatch") {
		t.Fatalf("ModeExact must reject omitempty drift, got %v", errs)
	}
}

func TestCheck_SubsetModeIgnoresOptionDrift(t *testing.T) {
	dir := t.TempDir()
	writeGo(t, dir, "emit.go", "package p\ntype E struct{ X string `json:\"x,omitempty\"` }\n")
	writeGo(t, dir, "cons.go", "package p\ntype C struct{ X string `json:\"x\"` }\n")
	p := Pair{Name: "t", Anchor: "x", Emitter: Endpoint{File: "emit.go", Type: "E"}, Consumer: Endpoint{File: "cons.go", Type: "C"}, Mode: ModeSubset}
	if errs := onePairCheck(dir, p); len(errs) != 0 {
		t.Fatalf("ModeSubset must tolerate omitempty drift (the deliberate asymmetry), got %v", errs)
	}
}

// --- CONDITION 3: any subset-mode option OTHER than omitempty fails closed ---

func TestCheck_SubsetModeRejectsStringOptionOnEmitter(t *testing.T) {
	dir := t.TempDir()
	writeGo(t, dir, "emit.go", "package p\ntype E struct{ X int `json:\"x,string\"` }\n")
	writeGo(t, dir, "cons.go", "package p\ntype C struct{ X int `json:\"x\"` }\n")
	p := Pair{Name: "t", Anchor: "x", Emitter: Endpoint{File: "emit.go", Type: "E"}, Consumer: Endpoint{File: "cons.go", Type: "C"}, Mode: ModeSubset}
	errs := onePairCheck(dir, p)
	if !hasErrContaining(errs, "unanalysed tag option") || !hasErrContaining(errs, "string") {
		t.Fatalf("subset mode must fail closed on a `,string` emitter option naming it, got %v", errs)
	}
}

func TestCheck_SubsetModeRejectsStringOptionOnConsumer(t *testing.T) {
	dir := t.TempDir()
	writeGo(t, dir, "emit.go", "package p\ntype E struct{ X int `json:\"x\"` }\n")
	writeGo(t, dir, "cons.go", "package p\ntype C struct{ X int `json:\"x,string\"` }\n")
	p := Pair{Name: "t", Anchor: "x", Emitter: Endpoint{File: "emit.go", Type: "E"}, Consumer: Endpoint{File: "cons.go", Type: "C"}, Mode: ModeSubset}
	errs := onePairCheck(dir, p)
	if !hasErrContaining(errs, "unanalysed tag option") || !hasErrContaining(errs, "string") {
		t.Fatalf("subset mode must fail closed on a `,string` consumer option naming it, got %v", errs)
	}
}

// --- C4: marker-bearing struct absent from manifest AND exemptions ---

func TestCompleteness_UncoveredMarkerDeclarationIsViolation(t *testing.T) {
	dir := t.TempDir()
	writeGo(t, dir, "cov.go", "package p\n// CROSS-MODULE WIRE CONTRACT: pins something.\ntype Orphan struct{ A string `json:\"a\"` }\n")
	m := Manifest{CoveredFiles: []string{"cov.go"}}
	errs := checkCompleteness(dir, m)
	if !hasErrContaining(errs, "Orphan") || !hasErrContaining(errs, "marker") {
		t.Fatalf("an uncovered marker-bearing struct must be a violation naming it, got %v", errs)
	}
}

func TestCompleteness_ExemptionSuppresses(t *testing.T) {
	dir := t.TempDir()
	writeGo(t, dir, "cov.go", "package p\n// CROSS-MODULE WIRE CONTRACT: pins something.\ntype Orphan struct{ A string `json:\"a\"` }\n")
	m := Manifest{CoveredFiles: []string{"cov.go"}, UnpairedExemptions: []Endpoint{{File: "cov.go", Type: "Orphan"}}}
	if errs := checkCompleteness(dir, m); len(errs) != 0 {
		t.Fatalf("an exemption must suppress the uncovered-declaration violation, got %v", errs)
	}
}

func TestCompleteness_FieldCommentMarkerIsDetected(t *testing.T) {
	dir := t.TempDir()
	writeGo(t, dir, "cov.go", "package p\ntype Host struct{\n // CROSS-MODULE WIRE CONTRACT: this field's tag is load-bearing.\n A string `json:\"a\"` }\n")
	m := Manifest{CoveredFiles: []string{"cov.go"}}
	if errs := checkCompleteness(dir, m); !hasErrContaining(errs, "Host") {
		t.Fatalf("a marker in a FIELD comment must be detected on the enclosing struct, got %v", errs)
	}
}

// --- C5: endpoint resolution fails closed ---

func TestCheck_MissingFileFailsClosed(t *testing.T) {
	dir := t.TempDir()
	writeGo(t, dir, "cons.go", "package p\ntype C struct{ A string `json:\"a\"` }\n")
	p := Pair{Name: "t", Anchor: "x", Emitter: Endpoint{File: "nope.go", Type: "E"}, Consumer: Endpoint{File: "cons.go", Type: "C"}, Mode: ModeExact}
	if errs := onePairCheck(dir, p); !hasErrContaining(errs, "nope.go") {
		t.Fatalf("a nonexistent file must fail closed naming it, got %v", errs)
	}
}

func TestCheck_MissingTypeFailsClosed(t *testing.T) {
	dir := t.TempDir()
	writeGo(t, dir, "emit.go", "package p\ntype E struct{ A string `json:\"a\"` }\n")
	writeGo(t, dir, "cons.go", "package p\ntype C struct{ A string `json:\"a\"` }\n")
	p := Pair{Name: "t", Anchor: "x", Emitter: Endpoint{File: "emit.go", Type: "Missing"}, Consumer: Endpoint{File: "cons.go", Type: "C"}, Mode: ModeExact}
	if errs := onePairCheck(dir, p); !hasErrContaining(errs, `type "Missing" not found`) {
		t.Fatalf("a nonexistent type must fail closed naming it, got %v", errs)
	}
}

func TestCheck_NonStructTypeFailsClosed(t *testing.T) {
	dir := t.TempDir()
	writeGo(t, dir, "emit.go", "package p\ntype E = string\n")
	writeGo(t, dir, "cons.go", "package p\ntype C struct{ A string `json:\"a\"` }\n")
	p := Pair{Name: "t", Anchor: "x", Emitter: Endpoint{File: "emit.go", Type: "E"}, Consumer: Endpoint{File: "cons.go", Type: "C"}, Mode: ModeExact}
	if errs := onePairCheck(dir, p); !hasErrContaining(errs, "is not a struct") {
		t.Fatalf("a non-struct type must fail closed, got %v", errs)
	}
}

// --- C6: RepoRoot fails closed when no go.work exists above the start dir ---

func TestRepoRoot_NoGoWorkFailsClosed(t *testing.T) {
	dir := t.TempDir() // a temp dir outside the repo, with no go.work above it
	root, err := repoRootFrom(dir)
	if err == nil {
		t.Fatalf("repoRootFrom must fail closed with no go.work above %q, got root=%q", dir, root)
	}
	if !strings.Contains(err.Error(), "go.work") {
		t.Errorf("error must name the missing marker, got %v", err)
	}
}

// --- C7: unresolvable embedded type fails closed ---

func TestExtract_UnresolvableEmbeddedFailsClosed(t *testing.T) {
	dir := t.TempDir()
	// Sibling is declared in ANOTHER file, so it cannot be resolved in this one.
	path := writeGo(t, dir, "emit.go", "package p\ntype E struct{ Sibling\n A string `json:\"a\"` }\n")
	_, err := ExtractStruct(path, "E")
	if err == nil {
		t.Fatal("an embedded type absent from the same file must fail closed")
	}
	if !strings.Contains(err.Error(), "Sibling") {
		t.Errorf("error must name the unresolvable embedded type, got %v", err)
	}
}

// --- Positive: embedded-field flattening, dash-skip, untagged naming ---

func TestExtract_FlattensEmbeddedStruct(t *testing.T) {
	dir := t.TempDir()
	path := writeGo(t, dir, "emit.go", "package p\ntype Inner struct{ Title string `json:\"title\"` }\ntype Outer struct{\n Outcome string `json:\"outcome\"`\n Inner }\n")
	fields, err := ExtractStruct(path, "Outer")
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	names := jsonNames(fields)
	if !contains(names, "outcome") || !contains(names, "title") {
		t.Fatalf("embedded fields must be flattened in place, got %v", names)
	}
}

func TestExtract_SkipsDashTag(t *testing.T) {
	dir := t.TempDir()
	path := writeGo(t, dir, "emit.go", "package p\ntype E struct{ A string `json:\"a\"`\n B string `json:\"-\"` }\n")
	fields, err := ExtractStruct(path, "E")
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if names := jsonNames(fields); contains(names, "-") || contains(names, "B") || len(names) != 1 {
		t.Fatalf("a json:\"-\" field must be skipped, got %v", names)
	}
}

func TestExtract_UntaggedFieldUsesGoName(t *testing.T) {
	dir := t.TempDir()
	path := writeGo(t, dir, "emit.go", "package p\ntype E struct{ Untagged string\n lower string }\n")
	fields, err := ExtractStruct(path, "E")
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	names := jsonNames(fields)
	if !contains(names, "Untagged") {
		t.Fatalf("an untagged exported field must take its Go name, got %v", names)
	}
	if contains(names, "lower") {
		t.Fatalf("an unexported field must be skipped, got %v", names)
	}
}

// --- Concern (high): an anonymous embedded field with an EMPTY-name json tag
// (`json:",omitempty"`) is flattened, NOT treated as a named field using the Go
// type name — matching encoding/json, where tag OPTIONS alone never make an
// embedded field named. This is the discriminating regression for the guard
// bug: before the fix "Inner" appeared as a named wire field and "title" (the
// promoted field) vanished. ---

func TestExtract_EmptyNameTaggedEmbeddedIsFlattened(t *testing.T) {
	dir := t.TempDir()
	path := writeGo(t, dir, "emit.go", "package p\ntype Inner struct{ Title string `json:\"title\"` }\ntype Outer struct{\n Outcome string `json:\"outcome\"`\n Inner `json:\",omitempty\"` }\n")
	fields, err := ExtractStruct(path, "Outer")
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	names := jsonNames(fields)
	if !contains(names, "title") {
		t.Fatalf("an empty-name-tagged embedded struct must be flattened, promoting %q; got %v", "title", names)
	}
	if contains(names, "Inner") {
		t.Fatalf("an empty json name must NOT make the embedded field named %q; got %v", "Inner", names)
	}
}

// The kept-behavior counterpart: a NON-EMPTY embedded tag name still makes it a
// named field (not flattened), so the fix narrows only the empty-name case.
func TestExtract_NonEmptyNameTaggedEmbeddedIsNamed(t *testing.T) {
	dir := t.TempDir()
	path := writeGo(t, dir, "emit.go", "package p\ntype Inner struct{ Title string `json:\"title\"` }\ntype Outer struct{ Inner `json:\"inner\"` }\n")
	fields, err := ExtractStruct(path, "Outer")
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	names := jsonNames(fields)
	if !contains(names, "inner") {
		t.Fatalf("a non-empty embedded tag name must make it a named field %q; got %v", "inner", names)
	}
	if contains(names, "title") {
		t.Fatalf("a named embedded field must NOT be flattened; got %v", names)
	}
}

// --- Concern (medium): the fail-closed parse-error branches in ExtractStruct
// and markerBearingStructs are exercised by malformed-source fixtures. The
// real-tree tests parse only valid committed files, so these hold the branches
// honest. ---

func TestExtract_MalformedSourceFailsClosed(t *testing.T) {
	dir := t.TempDir()
	// An illegal token (@) makes the file unparseable.
	path := writeGo(t, dir, "bad.go", "package p\nvar x = @\n")
	if _, err := ExtractStruct(path, "E"); err == nil {
		t.Fatal("a syntactically invalid source must fail closed in ExtractStruct")
	} else if !strings.Contains(err.Error(), "parse") {
		t.Errorf("error must name the parse failure, got %v", err)
	}
}

func TestCompleteness_MalformedSourceFailsClosed(t *testing.T) {
	dir := t.TempDir()
	writeGo(t, dir, "bad.go", "package p\nvar x = @\n")
	m := Manifest{CoveredFiles: []string{"bad.go"}}
	if errs := checkCompleteness(dir, m); !hasErrContaining(errs, "parse") {
		t.Fatalf("a syntactically invalid covered file must fail closed in the completeness sweep, got %v", errs)
	}
}

// --- Positive against the REAL tree: embedded flattening contributes the
// HeldCommitPRText names, so the subset check is not vacuously satisfied ---

func TestExtract_RunnerScopeParkBodyCarriesHeldCommitPRText(t *testing.T) {
	root, err := RepoRoot()
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	path := filepath.Join(root, filepath.FromSlash(uploadFile))
	fields, err := ExtractStruct(path, "pullRequestScopeParkBody")
	if err != nil {
		t.Fatalf("extract pullRequestScopeParkBody: %v", err)
	}
	names := jsonNames(fields)
	if !contains(names, "title") || !contains(names, "body") {
		t.Fatalf("without embedded flattening, HeldCommitPRText's title/body vanish and the subset check passes vacuously; got %v", names)
	}
}

func jsonNames(fields []Field) []string {
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		out = append(out, f.JSONName)
	}
	return out
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
