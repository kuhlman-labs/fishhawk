// Package wirecontract is a repo-level guard that pins the cross-module JSON
// wire contracts #2558 names: structs that must agree on the wire but live in
// two Go modules (backend and runner) which cannot import one another, so no
// compiler enforces their agreement. The guard parses each side's declared
// struct with go/ast and asserts their json tags agree, running as an ordinary
// Go test so `scripts/test verify`'s per-module `go test -race ./...` loop
// executes it with no shell wiring.
//
// WHOLE-REPO-TREE REQUIREMENT (operator binding condition 1). This package and
// the golden tests it anchors REQUIRE the full repository tree at test time:
// RepoRoot walks up to the `go.work` marker, and Check reads both the backend's
// and a SIBLING module's (runner's) source files by path. This is DELIBERATE
// and FAIL-CLOSED, not skippable — a resolution failure returns an error rather
// than a silent skip, because a guard that can silently cover nothing is the
// exact vacuity class #2558 exists to close. A future module-scoped test job, a
// partial checkout, or a vendored build that runs these packages' tests WITHOUT
// the full tree would go red by design and must account for it (run from the
// repo root with the whole workspace present). Every place these tests execute
// today — CI's `scripts/test lint`/`coverage` from the repo root, and the
// per-module verify loop — has the full tree; the constraint bites only a new
// partial-tree runner.
package wirecontract

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
)

// Field is one struct field's contribution to the JSON wire shape: the json
// name encoding/json would use, plus the tag options after it (e.g.
// "omitempty"). Fields are returned in declaration order, with embedded structs
// flattened in place.
type Field struct {
	// JSONName is the wire key: the explicit json-tag name, or the Go field
	// name when the field is exported and carries no json name.
	JSONName string
	// Options holds the comma-separated tag options after the name — e.g.
	// ["omitempty"] for `json:"x,omitempty"`. Empty options are dropped.
	Options []string
}

// RepoRoot walks up from the caller's working directory to the directory
// containing the `go.work` marker and returns it. It FAILS CLOSED with a named
// error when no marker is found above the start dir — never an empty root and
// nil, because a guard resolved against a bogus root would silently cover
// nothing (see the package doc's whole-tree requirement).
func RepoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("wirecontract: getwd: %w", err)
	}
	return repoRootFrom(dir)
}

// repoRootFrom is RepoRoot's testable core: the walk from an explicit start dir.
func repoRootFrom(start string) (string, error) {
	dir := start
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.work")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("wirecontract: go.work marker not found walking up from %q — these tests require the full repo tree", start)
		}
		dir = parent
	}
}

// ExtractStruct parses the Go source file at path and returns the json wire
// fields of the named struct type, in declaration order. Every resolution
// failure returns an error rather than an empty result:
//
//   - the file does not parse -> error
//   - typeName is absent, or names a non-struct type -> error
//   - an embedded (anonymous) field with no json NAME (no tag, or a tag with an
//     empty name) whose type cannot be resolved in the SAME file -> error naming
//     it (a non-flattening extractor would silently omit the embedded fields,
//     the vacuity class this guard exists to close)
//
// Field semantics mirror encoding/json: a `json:"-"` field is skipped; an
// unexported field is skipped (not marshalled); an exported field with no json
// name takes its Go field name; an anonymous struct field is flattened in place
// UNLESS its json tag supplies a non-empty name — a tag with an empty name such
// as `json:",omitempty"` leaves it anonymous and promoted, since tag OPTIONS
// alone never make an embedded field named.
func ExtractStruct(path, typeName string) ([]Field, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("wirecontract: parse %s: %w", path, err)
	}
	return extractFromFile(path, file, typeName)
}

// extractFromFile is ExtractStruct's core over an already-parsed file, so the
// recursive embedded-flattening walk reuses one parse.
func extractFromFile(path string, file *ast.File, typeName string) ([]Field, error) {
	st, err := findStruct(path, file, typeName)
	if err != nil {
		return nil, err
	}
	var out []Field
	for _, f := range st.Fields.List {
		if len(f.Names) == 0 {
			// Anonymous (embedded) field. encoding/json treats it as a NAMED
			// wire field only when its json tag supplies a NON-EMPTY name; a
			// tag with an empty name (e.g. `json:",omitempty"`) leaves it
			// anonymous and PROMOTES its fields exactly as no tag would — tag
			// OPTIONS alone never make an embedded field named.
			name, options, hasName := jsonTag(f)
			if hasName && name == "-" {
				continue // json:"-" ignores the field entirely
			}
			if hasName && name != "" {
				out = append(out, Field{JSONName: name, Options: options})
				continue
			}
			// No json name given: flatten. The embedded field's own options
			// (an omitempty here) never reach the wire, so they are dropped.
			embName := embeddedTypeName(f.Type)
			if embName == "" {
				return nil, fmt.Errorf("wirecontract: %s: embedded field of type %s in %q is not a same-file named struct and cannot be resolved", path, exprString(f.Type), typeName)
			}
			embFields, ferr := extractFromFile(path, file, embName)
			if ferr != nil {
				return nil, fmt.Errorf("wirecontract: %s: embedded type %q in %q: %w", path, embName, typeName, ferr)
			}
			out = append(out, embFields...)
			continue
		}
		for _, ident := range f.Names {
			if !ident.IsExported() {
				continue // unexported fields never marshal
			}
			name, options, hasTag := jsonTag(f)
			if hasTag {
				if name == "-" {
					continue
				}
				if name == "" {
					name = ident.Name
				}
			} else {
				name = ident.Name
			}
			out = append(out, Field{JSONName: name, Options: options})
		}
	}
	return out, nil
}

// findStruct returns the *ast.StructType for typeName in file, or an error that
// names the file and type when it is absent or is not a struct.
func findStruct(path string, file *ast.File, typeName string) (*ast.StructType, error) {
	var found *ast.TypeSpec
	for _, decl := range file.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.TYPE {
			continue
		}
		for _, spec := range gd.Specs {
			ts, ok := spec.(*ast.TypeSpec)
			if ok && ts.Name.Name == typeName {
				found = ts
			}
		}
	}
	if found == nil {
		return nil, fmt.Errorf("wirecontract: type %q not found in %s", typeName, path)
	}
	st, ok := found.Type.(*ast.StructType)
	if !ok {
		return nil, fmt.Errorf("wirecontract: type %q in %s is not a struct", typeName, path)
	}
	return st, nil
}

// jsonTag reads a field's json struct tag. hasTag reports whether a `json:`
// tag is present at all (distinct from a present-but-empty name); name is the
// part before the first comma and options are the rest (empties dropped).
func jsonTag(f *ast.Field) (name string, options []string, hasTag bool) {
	if f.Tag == nil {
		return "", nil, false
	}
	raw := strings.Trim(f.Tag.Value, "`")
	tag, ok := reflect.StructTag(raw).Lookup("json")
	if !ok {
		return "", nil, false
	}
	parts := strings.Split(tag, ",")
	name = parts[0]
	for _, opt := range parts[1:] {
		if opt != "" {
			options = append(options, opt)
		}
	}
	return name, options, true
}

// embeddedTypeName returns the name of an embedded field's type when it is a
// bare identifier (a same-package type), else "". A `pkg.Type` selector or a
// pointer to one returns "" — it cannot be resolved within the single file, and
// the caller fails closed on that.
func embeddedTypeName(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		if id, ok := t.X.(*ast.Ident); ok {
			return id.Name
		}
	}
	return ""
}

func exprString(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		return "*" + exprString(t.X)
	case *ast.SelectorExpr:
		return exprString(t.X) + "." + t.Sel.Name
	default:
		return fmt.Sprintf("%T", expr)
	}
}

// Check evaluates every manifest pair and the manifest-completeness sweep
// against the committed sources under root, returning one error per violation.
// A nil/empty slice means the contracts agree. Every resolution failure (a
// missing file, a missing/non-struct type, an unresolvable embedded type) is
// itself a returned error — the guard never silently covers nothing.
func Check(root string, m Manifest) []error {
	var errs []error
	for _, p := range m.Pairs {
		errs = append(errs, checkPair(root, p)...)
	}
	errs = append(errs, checkCompleteness(root, m)...)
	return errs
}

func checkPair(root string, p Pair) []error {
	emitterPath := filepath.Join(root, filepath.FromSlash(p.Emitter.File))
	consumerPath := filepath.Join(root, filepath.FromSlash(p.Consumer.File))
	emitter, err := ExtractStruct(emitterPath, p.Emitter.Type)
	if err != nil {
		return []error{fmt.Errorf("pair %q (%s): emitter %s.%s: %w", p.Name, p.Anchor, p.Emitter.File, p.Emitter.Type, err)}
	}
	consumer, err := ExtractStruct(consumerPath, p.Consumer.Type)
	if err != nil {
		return []error{fmt.Errorf("pair %q (%s): consumer %s.%s: %w", p.Name, p.Anchor, p.Consumer.File, p.Consumer.Type, err)}
	}
	switch p.Mode {
	case ModeExact:
		return checkExact(p, emitter, consumer)
	case ModeSubset:
		return checkSubset(p, emitter, consumer)
	default:
		return []error{fmt.Errorf("pair %q (%s): unknown comparison mode %d", p.Name, p.Anchor, p.Mode)}
	}
}

// tuple renders a field as "name" or "name,opt1,opt2" (options sorted) for the
// exact-mode equality comparison.
func tuple(f Field) string {
	if len(f.Options) == 0 {
		return f.JSONName
	}
	opts := append([]string(nil), f.Options...)
	sort.Strings(opts)
	return f.JSONName + "," + strings.Join(opts, ",")
}

// checkExact requires the sorted (name, options) tuple lists to be equal — the
// mode for leaf pairs that genuinely round-trip both ways, so options
// (including omitempty) must match too.
func checkExact(p Pair, emitter, consumer []Field) []error {
	em := make([]string, 0, len(emitter))
	for _, f := range emitter {
		em = append(em, tuple(f))
	}
	cm := make([]string, 0, len(consumer))
	for _, f := range consumer {
		cm = append(cm, tuple(f))
	}
	sort.Strings(em)
	sort.Strings(cm)
	if strings.Join(em, "|") == strings.Join(cm, "|") {
		return nil
	}
	return []error{fmt.Errorf(
		"pair %q (%s): ModeExact tag mismatch between emitter %s.%s and consumer %s.%s:\n  emitter: %v\n  consumer: %v",
		p.Name, p.Anchor, p.Emitter.File, p.Emitter.Type, p.Consumer.File, p.Consumer.Type, em, cm)}
}

// checkSubset requires every CONSUMER json name to exist on the EMITTER (names
// only — the deliberate omitempty-asymmetry decision, see the README). A
// consumer name the emitter never emits is the #2558 failure shape and is
// ALWAYS a violation. Emitter-only names must be allow-listed unless the pair
// is EmitterOnlyUnchecked.
//
// CONDITION 3 (fail closed on unanalysed options): options are NOT ignored
// wholesale. omitempty is a marshal-side-only directive and is tolerated, but
// ANY other option (`,string` being the clear example, which alters the encoded
// FORM rather than just presence) on a field that round-trips — a consumer
// field or the emitter field it matches — is a FAILURE naming the field and the
// option, so an option this guard has not analysed is loud rather than
// silently green.
func checkSubset(p Pair, emitter, consumer []Field) []error {
	var errs []error
	emitterNames := make(map[string]Field, len(emitter))
	for _, f := range emitter {
		emitterNames[f.JSONName] = f
	}
	consumerNames := make(map[string]struct{}, len(consumer))
	for _, f := range consumer {
		consumerNames[f.JSONName] = struct{}{}
	}
	for _, f := range consumer {
		if _, ok := emitterNames[f.JSONName]; !ok {
			errs = append(errs, fmt.Errorf(
				"pair %q (%s): consumer %s.%s decodes json name %q that emitter %s.%s never emits — a rename on the emitter left the consumer decoding a key nobody sends",
				p.Name, p.Anchor, p.Consumer.File, p.Consumer.Type, f.JSONName, p.Emitter.File, p.Emitter.Type))
		}
		errs = append(errs, disallowedOptionErrs(p, "consumer", p.Consumer, f)...)
		// The matching emitter field (a round-tripping field) gets the same
		// unanalysed-option check, so a `,string` added to EITHER side is loud.
		if ef, ok := emitterNames[f.JSONName]; ok {
			errs = append(errs, disallowedOptionErrs(p, "emitter", p.Emitter, ef)...)
		}
	}
	if !p.EmitterOnlyUnchecked {
		allowed := make(map[string]struct{}, len(p.AllowedEmitterOnly))
		for _, a := range p.AllowedEmitterOnly {
			allowed[a.JSONName] = struct{}{}
		}
		for _, f := range emitter {
			if _, ok := consumerNames[f.JSONName]; ok {
				continue
			}
			if _, ok := allowed[f.JSONName]; ok {
				continue
			}
			errs = append(errs, fmt.Errorf(
				"pair %q (%s): emitter %s.%s emits json name %q that consumer %s.%s does not decode and that is not in AllowedEmitterOnly — add a reasoned allowance or set EmitterOnlyUnchecked",
				p.Name, p.Anchor, p.Emitter.File, p.Emitter.Type, f.JSONName, p.Consumer.File, p.Consumer.Type))
		}
	}
	return errs
}

// disallowedOptionErrs reports any tag option other than omitempty on a
// subset-mode field (condition 3).
func disallowedOptionErrs(p Pair, side string, ep Endpoint, f Field) []error {
	var errs []error
	for _, opt := range f.Options {
		if opt == "omitempty" {
			continue
		}
		errs = append(errs, fmt.Errorf(
			"pair %q (%s): %s field %q on %s.%s carries subset-mode-unanalysed tag option %q — subset mode compares names only and does not model this option's wire effect, so it fails closed (widen the guard deliberately or move the pair to ModeExact)",
			p.Name, p.Anchor, side, f.JSONName, ep.File, ep.Type, opt))
	}
	return errs
}

// checkCompleteness sweeps the manifest's CoveredFiles for every struct whose
// doc comment or a field comment carries the marker and requires each to appear
// as a manifest Pair endpoint or in UnpairedExemptions. A NEW duplicated
// contract that copies the marker but skips the manifest fails here rather than
// joining the unguarded population.
func checkCompleteness(root string, m Manifest) []error {
	covered := manifestCoveredTypes(m)
	exempt := make(map[endpointKey]struct{}, len(m.UnpairedExemptions))
	for _, e := range m.UnpairedExemptions {
		exempt[endpointKey(e)] = struct{}{}
	}
	var errs []error
	for _, file := range m.CoveredFiles {
		path := filepath.Join(root, filepath.FromSlash(file))
		types, err := markerBearingStructs(path, file)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, typeName := range types {
			key := endpointKey{File: file, Type: typeName}
			if _, ok := covered[key]; ok {
				continue
			}
			if _, ok := exempt[key]; ok {
				continue
			}
			errs = append(errs, fmt.Errorf(
				"completeness: %s.%s carries the %q marker but is neither a manifest Pair endpoint nor an UnpairedExemptions entry — add a manifest row pairing it with its cross-module counterpart, or an explicitly-reasoned exemption",
				file, typeName, contractMarker))
		}
	}
	return errs
}

type endpointKey struct {
	File string
	Type string
}

func manifestCoveredTypes(m Manifest) map[endpointKey]struct{} {
	out := make(map[endpointKey]struct{})
	for _, p := range m.Pairs {
		out[endpointKey{File: p.Emitter.File, Type: p.Emitter.Type}] = struct{}{}
		out[endpointKey{File: p.Consumer.File, Type: p.Consumer.Type}] = struct{}{}
	}
	return out
}

// contractMarker is the literal doc-comment token a cross-module wire struct
// carries so the completeness sweep can find it.
const contractMarker = "CROSS-MODULE WIRE CONTRACT"

// markerBearingStructs returns the names of struct types in the file at path
// whose doc comment OR any comment inside the struct body carries the marker.
// relFile is used only for error messages. A parse failure returns an error
// (fail closed).
func markerBearingStructs(path, relFile string) ([]string, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("completeness: parse %s: %w", relFile, err)
	}
	var out []string
	for _, decl := range file.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.TYPE {
			continue
		}
		for _, spec := range gd.Specs {
			ts, ok := spec.(*ast.TypeSpec)
			if !ok {
				continue
			}
			st, ok := ts.Type.(*ast.StructType)
			if !ok {
				continue
			}
			if structBearsMarker(gd, ts, st, file) {
				out = append(out, ts.Name.Name)
			}
		}
	}
	return out, nil
}

// structBearsMarker reports whether the marker appears in the type's doc
// comment (GenDecl.Doc for an ungrouped decl, TypeSpec.Doc for a grouped one)
// or in ANY comment group positioned inside the struct body — which catches a
// per-field comment regardless of how go/ast associates it.
func structBearsMarker(gd *ast.GenDecl, ts *ast.TypeSpec, st *ast.StructType, file *ast.File) bool {
	if commentHas(gd.Doc) || commentHas(ts.Doc) {
		return true
	}
	for _, cg := range file.Comments {
		if cg.Pos() >= st.Pos() && cg.End() <= st.End() && commentHas(cg) {
			return true
		}
	}
	return false
}

func commentHas(cg *ast.CommentGroup) bool {
	return cg != nil && strings.Contains(cg.Text(), contractMarker)
}
