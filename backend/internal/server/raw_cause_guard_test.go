package server

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

// The raw-cause AST guard (E67.15 / #2587).
//
// WHAT IT IS: a TRIPWIRE, not the control. The runtime control is the
// writeError chokepoint (errors.go): it reads the ACTUAL status int and
// redacts every 5xx call site with no per-endpoint code. This guard is a
// syntactic regression tripwire over the KNOWN raw-cause syntaxes at 5xx
// call sites whose status it can resolve STATICALLY. It makes NO completeness
// claim: it cannot see helper-returned strings, cross-function data flow,
// struct-field reads, or multi-hop aliasing, and it reports — never silently
// passes — call sites whose status is resolved at runtime (it cannot classify
// those; the chokepoint handles them because it reads the real int). See
// backend/internal/server/README.md for the full contract and residual gaps.
//
// Detected message-argument forms: a `.Error()` call; an
// fmt.Sprint/Sprintf/Sprintln/Errorf whose args contain a `.Error()` call or an
// identifier named `err` / ending in `Err`; a `+` concatenation with such an
// operand; and a plain identifier assigned from such an expression by a
// single-hop assignment in the same function body. Detected details form: any
// of those same forms assigned to an ALLOW-LISTED key (the smuggle surface the
// key-based allow-list cannot see on its own).

type rawCauseFinding struct {
	Func string // enclosing function name
	Line int    // 1-indexed line of the writeError call
	Kind string // "message" or "details:<key>"
}

type guardResult struct {
	Findings  []rawCauseFinding // 5xx sites carrying a raw cause in a recognized syntax
	Unchecked []rawCauseFinding // sites whose status could not be statically resolved
}

// fivexxStatusConsts are the net/http 5xx status constant names.
var fivexxStatusConsts = map[string]struct{}{
	"StatusInternalServerError":           {},
	"StatusNotImplemented":                {},
	"StatusBadGateway":                    {},
	"StatusServiceUnavailable":            {},
	"StatusGatewayTimeout":                {},
	"StatusHTTPVersionNotSupported":       {},
	"StatusVariantAlsoNegotiates":         {},
	"StatusInsufficientStorage":           {},
	"StatusLoopDetected":                  {},
	"StatusNotExtended":                   {},
	"StatusNetworkAuthenticationRequired": {},
}

// statusClass classifies a writeError status argument.
type statusClass int

const (
	statusFivexx  statusClass = iota // a statically-resolved 5xx
	statusOther                      // a statically-resolved non-5xx
	statusRuntime                    // resolved at runtime (struct field, bare ident, ...)
)

func classifyStatus(arg ast.Expr) statusClass {
	switch e := arg.(type) {
	case *ast.SelectorExpr:
		// http.StatusXxx — decidable; anything else (e.g. derr.status) is runtime.
		if x, ok := e.X.(*ast.Ident); ok && x.Name == "http" {
			if _, is5xx := fivexxStatusConsts[e.Sel.Name]; is5xx {
				return statusFivexx
			}
			return statusOther
		}
		return statusRuntime
	case *ast.BasicLit:
		if e.Kind == token.INT {
			if n, err := strconv.Atoi(e.Value); err == nil {
				if n >= 500 && n <= 599 {
					return statusFivexx
				}
				return statusOther
			}
		}
		return statusRuntime
	default:
		// A bare identifier or any other expression: not statically decidable.
		return statusRuntime
	}
}

// isErrIdent reports whether an identifier name reads as an error value.
func isErrIdent(name string) bool {
	return name == "err" || strings.HasSuffix(name, "Err")
}

// exprCarriesCause reports whether a string-typed expression is derived from an
// error cause via one of the recognized syntaxes.
func exprCarriesCause(e ast.Expr) bool {
	switch v := e.(type) {
	case *ast.ParenExpr:
		return exprCarriesCause(v.X)
	case *ast.CallExpr:
		if sel, ok := v.Fun.(*ast.SelectorExpr); ok {
			// <x>.Error()
			if sel.Sel.Name == "Error" && len(v.Args) == 0 {
				return true
			}
			// fmt.Sprint / Sprintf / Sprintln / Errorf
			if x, ok := sel.X.(*ast.Ident); ok && x.Name == "fmt" {
				switch sel.Sel.Name {
				case "Sprint", "Sprintf", "Sprintln", "Errorf":
					for _, a := range v.Args {
						if argCarriesCause(a) {
							return true
						}
					}
				}
			}
		}
		return false
	case *ast.BinaryExpr:
		if v.Op == token.ADD {
			return exprCarriesCause(v.X) || exprCarriesCause(v.Y)
		}
		return false
	default:
		return false
	}
}

// argCarriesCause is exprCarriesCause plus a bare err-identifier, used for the
// arguments of an fmt call and for details-map values (where `err` embeds).
func argCarriesCause(e ast.Expr) bool {
	if exprCarriesCause(e) {
		return true
	}
	if id, ok := e.(*ast.Ident); ok && isErrIdent(id.Name) {
		return true
	}
	return false
}

// identAliasesCause reports whether name is assigned a cause-carrying expression
// by a single-hop `:=`/`=` assignment somewhere in body.
func identAliasesCause(body *ast.BlockStmt, name string) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for i, lhs := range as.Lhs {
			id, ok := lhs.(*ast.Ident)
			if !ok || id.Name != name {
				continue
			}
			if i < len(as.Rhs) && exprCarriesCause(as.Rhs[i]) {
				found = true
			}
		}
		return true
	})
	return found
}

// scanRawCause analyzes one parsed file for writeError raw-cause syntaxes.
func scanRawCause(fset *token.FileSet, file *ast.File) guardResult {
	var res guardResult
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		fnName := fn.Name.Name
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "writeError" || len(call.Args) < 6 {
				return true
			}
			line := fset.Position(call.Pos()).Line
			switch classifyStatus(call.Args[2]) {
			case statusRuntime:
				res.Unchecked = append(res.Unchecked, rawCauseFinding{Func: fnName, Line: line, Kind: "runtime-status"})
				return true
			case statusOther:
				return true
			case statusFivexx:
				// (i) message argument (index 4).
				msg := call.Args[4]
				if exprCarriesCause(msg) {
					res.Findings = append(res.Findings, rawCauseFinding{Func: fnName, Line: line, Kind: "message"})
				} else if id, ok := msg.(*ast.Ident); ok && identAliasesCause(fn.Body, id.Name) {
					res.Findings = append(res.Findings, rawCauseFinding{Func: fnName, Line: line, Kind: "message"})
				}
				// (ii) details argument (index 5): a cause under an allow-listed key.
				if lit, ok := call.Args[5].(*ast.CompositeLit); ok {
					for _, elt := range lit.Elts {
						kv, ok := elt.(*ast.KeyValueExpr)
						if !ok {
							continue
						}
						key, ok := stringLitValue(kv.Key)
						if !ok {
							continue
						}
						if _, allowListed := redactableDetailKeys[key]; !allowListed {
							continue
						}
						if argCarriesCause(kv.Value) {
							res.Findings = append(res.Findings, rawCauseFinding{Func: fnName, Line: line, Kind: "details:" + key})
						}
					}
				}
			}
			return true
		})
	}
	return res
}

func stringLitValue(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	v, err := strconv.Unquote(lit.Value)
	if err != nil {
		return "", false
	}
	return v, true
}

// parseFixture parses a testdata/rawcause/*.go.txt fixture.
func parseFixture(t *testing.T, name string) (*token.FileSet, *ast.File) {
	t.Helper()
	src, err := os.ReadFile(filepath.Join("testdata", "rawcause", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	fset := token.NewFileSet()
	// Parse under a .go name so positions read naturally; content is Go source.
	f, err := parser.ParseFile(fset, strings.TrimSuffix(name, ".txt"), src, 0)
	if err != nil {
		t.Fatalf("parse fixture %s: %v", name, err)
	}
	return fset, f
}

func kindsByFunc(findings []rawCauseFinding) map[string][]string {
	m := map[string][]string{}
	for _, f := range findings {
		m[f.Func] = append(m[f.Func], f.Kind)
	}
	for k := range m {
		sort.Strings(m[k])
	}
	return m
}

// TestRawCauseGuard_DetectsClaimedMessageForms proves each claimed message-half
// syntax is detected. Counterfactual: delete the message-half check in
// scanRawCause (the exprCarriesCause / identAliasesCause block) and this goes RED.
func TestRawCauseGuard_DetectsClaimedMessageForms(t *testing.T) {
	fset, f := parseFixture(t, "violations.go.txt")
	got := kindsByFunc(scanRawCause(fset, f).Findings)

	for _, fn := range []string{"msgDotError", "msgSprintf", "msgConcat", "msgAlias"} {
		kinds := got[fn]
		if len(kinds) != 1 || kinds[0] != "message" {
			t.Errorf("func %s: got kinds %v, want exactly [message]", fn, kinds)
		}
	}
}

// TestRawCauseGuard_DetectsSmuggledCauseUnderAllowlistedKey proves a cause
// assigned to an allow-listed details key is detected. Counterfactual: delete
// the details-half check in scanRawCause and this goes RED.
func TestRawCauseGuard_DetectsSmuggledCauseUnderAllowlistedKey(t *testing.T) {
	fset, f := parseFixture(t, "violations.go.txt")
	got := kindsByFunc(scanRawCause(fset, f).Findings)

	kinds := got["detailsSmuggle"]
	if len(kinds) != 1 || kinds[0] != "details:reason" {
		t.Errorf("detailsSmuggle: got kinds %v, want exactly [details:reason]", kinds)
	}
}

// TestRawCauseGuard_CleanFixtureHasNoFindings pins the negative control: a
// static message + allow-listed literal details yields zero findings.
func TestRawCauseGuard_CleanFixtureHasNoFindings(t *testing.T) {
	fset, f := parseFixture(t, "clean.go.txt")
	res := scanRawCause(fset, f)
	if len(res.Findings) != 0 {
		t.Errorf("clean fixture produced findings: %+v", res.Findings)
	}
}

// TestRawCauseGuard_ReportsRuntimeStatusSites proves the guard REPORTS rather
// than silently passes call sites whose status is resolved at runtime — so its
// reach is never overstated. The re-scan found FIVE such sites (deploy_rollback,
// release_notes x2, defer_concern, workitems), not the four the prior survey
// claimed; each flows through the runtime chokepoint, which reads the real int.
func TestRawCauseGuard_ReportsRuntimeStatusSites(t *testing.T) {
	res := scanPackage(t)
	// The known runtime-status writeError call sites flow through the runtime
	// chokepoint (it reads the real int); assert the guard surfaces each rather
	// than dropping it silently.
	if len(res.Unchecked) < 5 {
		t.Errorf("guard reported %d runtime-status sites, want >= 5: %+v", len(res.Unchecked), res.Unchecked)
	}
}

// TestRawCauseGuard_PackageIsClean runs the guard over the REAL package source
// and asserts zero raw-cause findings — this is what proves the hand-fix of the
// message-half sites is complete for the recognized syntaxes. If it reports a
// finding, either a new raw-cause site landed or the hand-fix was incomplete.
func TestRawCauseGuard_PackageIsClean(t *testing.T) {
	res := scanPackage(t)
	if len(res.Findings) != 0 {
		t.Errorf("package has raw-cause findings at 5xx sites: %+v", res.Findings)
	}
}

// scanPackage parses every non-test .go file in the package and scans it.
func scanPackage(t *testing.T) guardResult {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	fset := token.NewFileSet()
	var agg guardResult
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		r := scanRawCause(fset, f)
		agg.Findings = append(agg.Findings, r.Findings...)
		agg.Unchecked = append(agg.Unchecked, r.Unchecked...)
	}
	return agg
}
