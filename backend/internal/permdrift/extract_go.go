package permdrift

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// The Go-source extractors read four specific product files with go/parser +
// go/ast and NO type checking. They are therefore tied to those files'
// SHAPES, and each one reports, alongside its grants, the evidence the shape
// guard (Detect) reads to fail CLOSED when a refactor moves a grant out of a
// shape the walker recognizes:
//
//   - Anchors: the anchoring declarations found (the mcpToolScopes var, each
//     manifest composite-literal key, each string-slice var, each function
//     declaring the run-token `scopes` slice). An anchor present at base and
//     missing at head is shape_unrecognized.
//   - Unresolved: every recognized construct holding an expression the walker
//     cannot resolve to a value (a call, a function-local identifier, an
//     unknown struct field, an assignment to the tracked slice other than an
//     append). Any unresolved construct on either side is shape_unrecognized.
//
// Identifier resolution, one rule for every Go extractor:
//
//   - a string literal is its value; a `+` of two literal-valued operands is
//     their concatenation;
//   - an identifier naming a package-level const or var declared IN THIS FILE
//     with a resolvable value resolves to that value (recursively);
//   - an identifier declared in this file as anything else (a func, a type, a
//     multi-value or non-string var), or declared LOCALLY in the enclosing
//     function (a parameter, a := or var, a range variable), or a predeclared
//     name (nil, true, iota...) is UNRESOLVED — its value is a runtime fact;
//   - an identifier declared nowhere in this file is a package-level name
//     from a sibling file (a const like scopeWriteMessages) and is KEPT AS
//     ITS NAME: a stable identity, so replacing a literal with such a name
//     still reads as one key vanishing and another appearing;
//   - a qualified `pkg.Name` whose qualifier is not declared in this file is
//     kept as "pkg.Name" for the same reason;
//   - every other expression (a call, an index, a conversion) is unresolved.
//
// Every resolved name that becomes a dotted key segment (a manifest name, an
// /mcp tool or scope, a run-token scope, an env-allow var or NAME) is escaped
// with keySegment, so a resolved value carrying '.', '*' or '%' — including a
// qualified cross-file name kept as "pkg.Name" — is ONE segment and never a
// wildcard of the file's own making.

// GoExtraction is a Go-source extractor's result.
type GoExtraction struct {
	// Grants is the normalized grant set.
	Grants Grants
	// Anchors is the set of anchoring declarations found, each named by a
	// string starting with the grant-key prefix of the surface it anchors.
	Anchors map[string]bool
	// Unresolved lists the constructs the walker recognized but could not
	// resolve. Non-empty on either side fails the comparison closed.
	Unresolved []Unresolved
}

// Unresolved is one construct the walker could not resolve.
type Unresolved struct {
	// Prefix is the grant-key prefix of the surface the construct belongs to,
	// so a split surface (App permissions vs App events) only fails on its own
	// part.
	Prefix string
	// Detail names the construct STRUCTURALLY (a line number and an
	// expression kind, plus the product anchor name); it never carries file
	// bytes.
	Detail string
}

// The Go extractors' grant-key prefixes.
const (
	MCPToolPrefix  = "mcp_tool."
	RunTokenPrefix = "run_token."
	EnvAllowPrefix = "env_allow."
)

// The anchor and tracked-identifier names the Go extractors key on.
const (
	// mcpToolScopesVar is the /mcp tool -> scope table in
	// backend/internal/server/mcpscopes.go.
	mcpToolScopesVar = "mcpToolScopes"
	// runTokenScopesVar is the local slice handleIssueMCPToken builds the
	// run-bound token's scopes in (backend/internal/server/mcptoken.go).
	runTokenScopesVar = "scopes"
	// extendFunc is the same-file concatenation helper
	// backend/internal/reviewsandbox/env.go builds its allow-lists with.
	extendFunc = "extend"
	// anyStage is the stage a run-token grant with no (or a negated) stage
	// guard is keyed under.
	anyStage = "any_stage"
)

// goFile is one parsed Go file plus its package-level declaration index.
type goFile struct {
	fset *token.FileSet
	file *ast.File
	// values maps a single-valued package-level const/var name to its value
	// expression.
	values map[string]ast.Expr
	// varTypes maps a package-level var name to its declared type expression.
	varTypes map[string]ast.Expr
	// funcs maps a top-level (receiver-less) function name to its decl.
	funcs map[string]*ast.FuncDecl
	// declared is every name this file declares at package level (consts,
	// vars, types, funcs) — a name here that is not in values is unresolved.
	declared map[string]bool
	// varOrder is the package-level var names in source order.
	varOrder []string

	ext GoExtraction
}

func parseGoFile(content []byte) (*goFile, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "surface.go", content, parser.SkipObjectResolution)
	if err != nil {
		return nil, fmt.Errorf("go source: %w", err)
	}
	g := &goFile{
		fset:     fset,
		file:     f,
		values:   map[string]ast.Expr{},
		varTypes: map[string]ast.Expr{},
		funcs:    map[string]*ast.FuncDecl{},
		declared: map[string]bool{},
		ext:      GoExtraction{Grants: Grants{}, Anchors: map[string]bool{}},
	}
	for _, d := range f.Decls {
		switch d := d.(type) {
		case *ast.FuncDecl:
			if d.Recv == nil {
				g.funcs[d.Name.Name] = d
				g.declared[d.Name.Name] = true
			}
		case *ast.GenDecl:
			for _, sp := range d.Specs {
				switch sp := sp.(type) {
				case *ast.TypeSpec:
					g.declared[sp.Name.Name] = true
				case *ast.ValueSpec:
					for i, n := range sp.Names {
						g.declared[n.Name] = true
						if d.Tok == token.VAR {
							g.varOrder = append(g.varOrder, n.Name)
							if sp.Type != nil {
								g.varTypes[n.Name] = sp.Type
							}
						}
						if len(sp.Values) == len(sp.Names) {
							g.values[n.Name] = sp.Values[i]
						}
					}
				}
			}
		}
	}
	return g, nil
}

// unresolved records one unresolvable construct.
func (g *goFile) unresolved(prefix string, at ast.Node, what string) {
	g.ext.Unresolved = append(g.ext.Unresolved, Unresolved{
		Prefix: prefix,
		Detail: fmt.Sprintf("line %d: %s", g.fset.Position(at.Pos()).Line, what),
	})
}

// strKind is how resolveString resolved an expression.
type strKind int

const (
	strUnresolved strKind = iota
	strLiteral            // the value is the expression's constant value
	strName               // the value is a cross-file identifier's name
)

// maxResolveDepth bounds identifier chasing so a const cycle cannot spin.
const maxResolveDepth = 8

// predeclared names are never resolvable to a string value.
var predeclared = map[string]bool{"nil": true, "true": true, "false": true, "iota": true}

// resolveString resolves e to a string per the package rule. locals is the
// enclosing function's local names (nil at package level).
func (g *goFile) resolveString(e ast.Expr, locals map[string]bool, depth int) (string, strKind) {
	if depth > maxResolveDepth {
		return "", strUnresolved
	}
	switch e := e.(type) {
	case *ast.BasicLit:
		if e.Kind != token.STRING {
			return "", strUnresolved
		}
		v, err := strconv.Unquote(e.Value)
		if err != nil {
			return "", strUnresolved
		}
		return v, strLiteral
	case *ast.ParenExpr:
		return g.resolveString(e.X, locals, depth+1)
	case *ast.BinaryExpr:
		if e.Op != token.ADD {
			return "", strUnresolved
		}
		l, lk := g.resolveString(e.X, locals, depth+1)
		r, rk := g.resolveString(e.Y, locals, depth+1)
		if lk != strLiteral || rk != strLiteral {
			return "", strUnresolved
		}
		return l + r, strLiteral
	case *ast.Ident:
		if locals[e.Name] || predeclared[e.Name] {
			return "", strUnresolved
		}
		if v, ok := g.values[e.Name]; ok {
			return g.resolveString(v, nil, depth+1)
		}
		if g.declared[e.Name] {
			return "", strUnresolved
		}
		return e.Name, strName
	case *ast.SelectorExpr:
		q, ok := e.X.(*ast.Ident)
		if !ok || locals[q.Name] || g.declared[q.Name] || predeclared[q.Name] {
			return "", strUnresolved
		}
		return q.Name + "." + e.Sel.Name, strName
	}
	return "", strUnresolved
}

// stringOrRecord resolves e to a string, recording it unresolved under prefix
// when it is not.
func (g *goFile) stringOrRecord(prefix string, e ast.Expr, locals map[string]bool, what string) (string, bool) {
	v, k := g.resolveString(e, locals, 0)
	if k == strUnresolved {
		g.unresolved(prefix, e, what+" is "+exprKind(e))
		return "", false
	}
	return v, true
}

// exprKind names an expression's AST kind for an Unresolved detail.
func exprKind(e ast.Expr) string {
	switch e := e.(type) {
	case *ast.CallExpr:
		return "a call"
	case *ast.Ident:
		return "an unresolvable identifier"
	case *ast.SelectorExpr:
		return "an unresolvable selector"
	case *ast.CompositeLit:
		return "an unrecognized composite literal"
	case *ast.BasicLit:
		return "a non-string literal"
	case nil:
		return "missing"
	default:
		return strings.TrimPrefix(fmt.Sprintf("%T", e), "*ast.")
	}
}

// isStringSliceType reports whether t is `[]string`.
func isStringSliceType(t ast.Expr) bool {
	at, ok := t.(*ast.ArrayType)
	if !ok || at.Len != nil {
		return false
	}
	id, ok := at.Elt.(*ast.Ident)
	return ok && id.Name == "string"
}

// resolveStringSlice resolves a []string-valued expression: a `[]string{...}`
// literal, an identifier naming a same-file string-slice var, `append(s,
// ...)`, or a call to the same-file extend helper (see extendIsPure). ok is
// false (and the construct recorded) on anything else. seen guards var
// cycles.
func (g *goFile) resolveStringSlice(prefix string, e ast.Expr, locals map[string]bool, seen map[string]bool) ([]string, bool) {
	switch e := e.(type) {
	case *ast.CompositeLit:
		if e.Type != nil && !isStringSliceType(e.Type) {
			g.unresolved(prefix, e, "slice literal of a non-[]string type")
			return nil, false
		}
		out := make([]string, 0, len(e.Elts))
		ok := true
		for _, el := range e.Elts {
			v, good := g.stringOrRecord(prefix, el, locals, "slice element")
			if !good {
				ok = false
				continue
			}
			out = append(out, v)
		}
		return out, ok
	case *ast.Ident:
		if e.Name == "nil" {
			return nil, true
		}
		v, isVar := g.values[e.Name]
		if locals[e.Name] || !isVar || seen[e.Name] {
			g.unresolved(prefix, e, "slice operand is "+exprKind(e))
			return nil, false
		}
		seen[e.Name] = true
		defer delete(seen, e.Name)
		return g.resolveStringSlice(prefix, v, nil, seen)
	case *ast.ParenExpr:
		return g.resolveStringSlice(prefix, e.X, locals, seen)
	case *ast.CallExpr:
		fn, isIdent := e.Fun.(*ast.Ident)
		switch {
		case isIdent && fn.Name == "append" && !locals["append"] && !g.declared["append"]:
		case isIdent && fn.Name == extendFunc && !locals[extendFunc] && g.extendIsPure():
		default:
			g.unresolved(prefix, e, "slice value is a call")
			return nil, false
		}
		if len(e.Args) == 0 {
			g.unresolved(prefix, e, "append/extend with no arguments")
			return nil, false
		}
		out, ok := g.resolveStringSlice(prefix, e.Args[0], locals, seen)
		rest := e.Args[1:]
		if e.Ellipsis.IsValid() {
			if len(rest) != 1 {
				g.unresolved(prefix, e, "spread append/extend with extra arguments")
				return nil, false
			}
			more, good := g.resolveStringSlice(prefix, rest[0], locals, seen)
			return append(out, more...), ok && good
		}
		for _, a := range rest {
			v, good := g.stringOrRecord(prefix, a, locals, "append/extend argument")
			if !good {
				ok = false
				continue
			}
			out = append(out, v)
		}
		return out, ok
	}
	g.unresolved(prefix, e, "slice value is "+exprKind(e))
	return nil, false
}

// pureBuiltins are the only calls the extend helper's body may make.
var pureBuiltins = map[string]bool{"make": true, "len": true, "cap": true, "append": true, "copy": true}

// extendIsPure reports whether the same-file extend helper exists and is a
// pure concatenation: its body calls only make/len/cap/append/copy and holds
// no literal of its own, so it cannot inject a name or read the environment.
// A changed body that fails this makes every extend call unresolved.
func (g *goFile) extendIsPure() bool {
	fd, ok := g.funcs[extendFunc]
	if !ok || fd.Body == nil {
		return false
	}
	pure := true
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.BasicLit:
			if n.Kind == token.STRING || n.Kind == token.CHAR {
				pure = false
			}
		case *ast.CallExpr:
			id, isIdent := n.Fun.(*ast.Ident)
			if !isIdent || !pureBuiltins[id.Name] {
				pure = false
			}
		}
		return pure
	})
	return pure
}

// ExtractGoManifest extracts a GitHub App manifest built as a Go composite
// literal (backend/internal/server/manifest.go): the values keyed
// "default_permissions" (a map[string]string literal) and "default_events" (a
// []string literal), emitting the SAME keys as ExtractManifest. Each of the
// two keys found is an anchor named by its grant-key prefix.
func ExtractGoManifest(content []byte) (GoExtraction, error) {
	if len(bytes.TrimSpace(content)) == 0 {
		return emptyGoExtraction(), nil
	}
	g, err := parseGoFile(content)
	if err != nil {
		return GoExtraction{}, err
	}
	perms := map[string]string{}
	var events []string
	ast.Inspect(g.file, func(n ast.Node) bool {
		kv, ok := n.(*ast.KeyValueExpr)
		if !ok {
			return true
		}
		key, kind := g.resolveString(kv.Key, nil, 0)
		if kind != strLiteral {
			return true
		}
		switch key {
		case "default_permissions":
			g.ext.Anchors[ManifestPermissionPrefix] = true
			g.manifestPermissions(kv.Value, perms)
		case "default_events":
			g.ext.Anchors[ManifestEventPrefix] = true
			if evs, ok := g.resolveStringSlice(ManifestEventPrefix, kv.Value, nil, map[string]bool{}); ok {
				events = append(events, evs...)
			}
		}
		return true
	})
	if err := AddManifestGrants(g.ext.Grants, perms, events); err != nil {
		return GoExtraction{}, err
	}
	return g.ext, nil
}

// manifestPermissions reads a default_permissions map literal into perms,
// keeping the higher level on a duplicate name.
func (g *goFile) manifestPermissions(v ast.Expr, perms map[string]string) {
	if id, ok := v.(*ast.Ident); ok {
		if val, isVar := g.values[id.Name]; isVar {
			v = val
		}
	}
	lit, ok := v.(*ast.CompositeLit)
	if !ok {
		g.unresolved(ManifestPermissionPrefix, v, "default_permissions value is "+exprKind(v))
		return
	}
	for _, el := range lit.Elts {
		kv, ok := el.(*ast.KeyValueExpr)
		if !ok {
			g.unresolved(ManifestPermissionPrefix, el, "default_permissions element is not key: value")
			continue
		}
		name, okName := g.stringOrRecord(ManifestPermissionPrefix, kv.Key, nil, "permission name")
		level, okLevel := g.stringOrRecord(ManifestPermissionPrefix, kv.Value, nil, "permission level")
		if !okName || !okLevel {
			continue
		}
		if prev, dup := perms[name]; dup {
			pr, _ := AppLevels.Rank(prev)
			nr, _ := AppLevels.Rank(level)
			if pr >= nr {
				continue
			}
		}
		perms[name] = level
	}
}

// ExtractGoMCPScopes extracts the /mcp tool -> scope table, the package-level
// var mcpToolScopes (backend/internal/server/mcpscopes.go), which is the
// anchor. Per tool:
//
//   - `mcp_tool.<tool>.admitted`: presence Grant (a tool absent from the
//     table is refused);
//   - `mcp_tool.<tool>.any_of.<scope>`: presence Grant per anyOf member (more
//     members is easier to satisfy);
//   - `mcp_tool.<tool>.any_of.*`: presence Grant for an EMPTY anyOf (the
//     mcpScopeAuthenticatedOnly sentinel resolves to one) — so by wildcard
//     subsumption {X} -> authenticated-only is one widening and the reverse
//     one narrowing;
//   - `mcp_tool.<tool>.run_bound_subject_ok`: presence Grant when true.
//
// A rule value must be a rule composite literal (or a same-file var holding
// one) with keyed anyOf / runBoundSubjectOK fields; anything else, an unknown
// field, or an unresolvable anyOf element is unresolved.
func ExtractGoMCPScopes(content []byte) (GoExtraction, error) {
	if len(bytes.TrimSpace(content)) == 0 {
		return emptyGoExtraction(), nil
	}
	g, err := parseGoFile(content)
	if err != nil {
		return GoExtraction{}, err
	}
	table, ok := g.values[mcpToolScopesVar]
	if !ok {
		return g.ext, nil
	}
	anchor := MCPToolPrefix + mcpToolScopesVar
	g.ext.Anchors[anchor] = true
	lit, ok := table.(*ast.CompositeLit)
	if !ok {
		g.unresolved(MCPToolPrefix, table, mcpToolScopesVar+" value is "+exprKind(table))
		return g.ext, nil
	}
	for _, el := range lit.Elts {
		kv, ok := el.(*ast.KeyValueExpr)
		if !ok {
			g.unresolved(MCPToolPrefix, el, mcpToolScopesVar+" element is not key: value")
			continue
		}
		tool, kind := g.resolveString(kv.Key, nil, 0)
		if tool == "" || kind == strUnresolved {
			g.unresolved(MCPToolPrefix, kv.Key, "tool name is "+exprKind(kv.Key))
			continue
		}
		g.mcpRule(MCPToolPrefix+keySegment(tool), kv.Value, map[string]bool{})
	}
	return g.ext, nil
}

// mcpRule records one tool's rule under prefix (`mcp_tool.<tool>`).
func (g *goFile) mcpRule(prefix string, v ast.Expr, seen map[string]bool) {
	if id, ok := v.(*ast.Ident); ok {
		val, isVar := g.values[id.Name]
		if !isVar || seen[id.Name] {
			g.unresolved(MCPToolPrefix, v, "rule value is "+exprKind(v))
			return
		}
		seen[id.Name] = true
		g.mcpRule(prefix, val, seen)
		return
	}
	lit, ok := v.(*ast.CompositeLit)
	if !ok {
		g.unresolved(MCPToolPrefix, v, "rule value is "+exprKind(v))
		return
	}
	var anyOf []string
	anyOfOK := true
	for _, el := range lit.Elts {
		kv, ok := el.(*ast.KeyValueExpr)
		if !ok {
			g.unresolved(MCPToolPrefix, el, "rule field is positional")
			return
		}
		field, _ := kv.Key.(*ast.Ident)
		switch {
		case field != nil && field.Name == "anyOf":
			anyOf, anyOfOK = g.resolveStringSlice(MCPToolPrefix, kv.Value, nil, map[string]bool{})
		case field != nil && field.Name == "runBoundSubjectOK":
			b, isIdent := kv.Value.(*ast.Ident)
			switch {
			case isIdent && b.Name == "true" && !g.declared["true"]:
				g.ext.Grants.Put(Entry{Key: prefix + ".run_bound_subject_ok", Value: Present, Rank: PresenceRank, Polarity: Grant})
			case isIdent && b.Name == "false" && !g.declared["false"]:
			default:
				g.unresolved(MCPToolPrefix, kv.Value, "runBoundSubjectOK is "+exprKind(kv.Value))
				return
			}
		default:
			g.unresolved(MCPToolPrefix, kv.Key, "unknown rule field")
			return
		}
	}
	if !anyOfOK {
		return
	}
	g.ext.Grants.Put(Entry{Key: prefix + ".admitted", Value: Present, Rank: PresenceRank, Polarity: Grant})
	if len(anyOf) == 0 {
		g.ext.Grants.Put(Entry{Key: prefix + ".any_of" + WildcardSuffix, Value: "authenticated only", Rank: PresenceRank, Polarity: Grant})
		return
	}
	for _, sc := range anyOf {
		g.ext.Grants.Put(Entry{Key: prefix + ".any_of." + keySegment(sc), Value: Present, Rank: PresenceRank, Polarity: Grant})
	}
}

// ExtractGoRunTokenScopes extracts the run-bound MCP token's scope grants
// (backend/internal/server/mcptoken.go). Every function declaring a local
// `scopes` slice with a `[]string{...}` literal is an anchor
// (`run_token.func.<name>`); within it, each literal member and each
// `scopes = append(scopes, X...)` argument is keyed
// `run_token.<scope>@<stage>`, a presence Grant, for each run.StageType*
// selector in its guarding conditions:
//
//   - the conditions of every enclosing if (and the case list of an enclosing
//     switch case) contribute their stage selectors, resolving ONE level into
//     a same-file predicate function's return expressions
//     (stageTypeMayMessage);
//   - a switch case whose body ends in `fallthrough` carries its guard (its
//     stages, and a negated/default marker) into the NEXT case's guard, since
//     the next body also runs for the falling case's stages;
//   - no stage selector, an else branch, a default case, or a NEGATED stage
//     comparison (`!=`, `!`) keys the grant `@any_stage`;
//   - an `||` with an operand carrying NO stage selector (`|| true`, a
//     qualified or cross-file call `|| other.Pred(st)`, a same-file predicate
//     whose own return calls a second predicate the one-level resolution
//     cannot see) keys the grant `@any_stage`: that operand can be true for
//     any stage, so keying only the other operand's stages would hide a grant.
//     This over-approximates (a stage-independent operand that is in fact
//     false reads as @any_stage) — noise, never a miss.
//
// Any other assignment to `scopes` (a helper call, a reassignment), an
// `append(scopes, ...)` not assigned back to `scopes`, `&scopes` (bare or as a
// call argument, `mutate(&scopes)`), an indexed write, `copy(scopes, ...)`, an
// ALIAS of the slice (a name other than `scopes` bound from `scopes` or a
// slice expression of it — `alias := scopes`, `var alias = scopes[:]` — whose
// element writes would share the backing array), and an append argument
// that is a call or a function-local identifier are unresolved. Passing
// `scopes` BY VALUE as a call argument or a composite-literal field value
// (the real handler's `s.issue(scopes)` / `Scopes: scopes` shape) stays
// allowed; a callee or struct field that writes the shared elements is a
// residual this walker does not follow.
func ExtractGoRunTokenScopes(content []byte) (GoExtraction, error) {
	if len(bytes.TrimSpace(content)) == 0 {
		return emptyGoExtraction(), nil
	}
	g, err := parseGoFile(content)
	if err != nil {
		return GoExtraction{}, err
	}
	for _, d := range g.file.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Body == nil || !declaresScopesLiteral(fd.Body) {
			continue
		}
		g.ext.Anchors[RunTokenPrefix+"func."+fd.Name.Name] = true
		w := &tokenWalker{g: g, locals: localNames(fd), assigned: map[*ast.CallExpr]bool{}, visited: map[ast.Node]bool{}}
		w.stmts(fd.Body.List, nil)
		w.checkStrayUses(fd.Body)
	}
	return g.ext, nil
}

// declaresScopesLiteral reports whether body declares `scopes` from a
// composite literal (`scopes := []string{...}` or `var scopes =
// []string{...}`), at any depth.
func declaresScopesLiteral(body *ast.BlockStmt) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.AssignStmt:
			if n.Tok == token.DEFINE && len(n.Lhs) == len(n.Rhs) {
				for i, l := range n.Lhs {
					if isIdentNamed(l, runTokenScopesVar) {
						if _, ok := n.Rhs[i].(*ast.CompositeLit); ok {
							found = true
						}
					}
				}
			}
		case *ast.ValueSpec:
			for i, nm := range n.Names {
				if nm.Name == runTokenScopesVar && i < len(n.Values) {
					if _, ok := n.Values[i].(*ast.CompositeLit); ok {
						found = true
					}
				}
			}
		}
		return !found
	})
	return found
}

func isIdentNamed(e ast.Expr, name string) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == name
}

// localNames is every name fd declares locally: receiver, parameters,
// results, and any := / var / range / type-switch binding in its body.
func localNames(fd *ast.FuncDecl) map[string]bool {
	out := map[string]bool{}
	addFields := func(fl *ast.FieldList) {
		if fl == nil {
			return
		}
		for _, f := range fl.List {
			for _, n := range f.Names {
				out[n.Name] = true
			}
		}
	}
	addFields(fd.Recv)
	addFields(fd.Type.Params)
	addFields(fd.Type.Results)
	addIdent := func(e ast.Expr) {
		if id, ok := e.(*ast.Ident); ok {
			out[id.Name] = true
		}
	}
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.AssignStmt:
			if n.Tok == token.DEFINE {
				for _, l := range n.Lhs {
					addIdent(l)
				}
			}
		case *ast.ValueSpec:
			for _, nm := range n.Names {
				out[nm.Name] = true
			}
		case *ast.RangeStmt:
			if n.Tok == token.DEFINE {
				addIdent(n.Key)
				addIdent(n.Value)
			}
		case *ast.FuncLit:
			addFields(n.Type.Params)
			addFields(n.Type.Results)
		}
		return true
	})
	return out
}

// guard is one enclosing condition's contribution to a run-token grant key.
type guard struct {
	stages  []string
	negated bool
}

// tokenWalker walks one anchor function's statements with the guard stack.
type tokenWalker struct {
	g      *goFile
	locals map[string]bool
	// assigned is every append(scopes, ...) call consumed by a recognized
	// `scopes = append(scopes, ...)`; any other is a stray use.
	assigned map[*ast.CallExpr]bool
	// visited is every assignment the statement walk reached; an assignment
	// to the tracked slice it did not reach (inside a closure, a select) is
	// a stray use.
	visited map[ast.Node]bool
}

func (w *tokenWalker) stmts(list []ast.Stmt, guards []guard) {
	for _, s := range list {
		w.stmt(s, guards)
	}
}

// with returns guards plus g, never aliasing the caller's backing array.
func with(guards []guard, g guard) []guard {
	out := make([]guard, 0, len(guards)+1)
	out = append(out, guards...)
	return append(out, g)
}

func (w *tokenWalker) stmt(s ast.Stmt, guards []guard) {
	switch s := s.(type) {
	case *ast.BlockStmt:
		w.stmts(s.List, guards)
	case *ast.IfStmt:
		if s.Init != nil {
			w.stmt(s.Init, guards)
		}
		w.stmts(s.Body.List, with(guards, w.guardOf(s.Cond)))
		if s.Else != nil {
			w.stmt(s.Else, with(guards, guard{negated: true}))
		}
	case *ast.SwitchStmt:
		if s.Init != nil {
			w.stmt(s.Init, guards)
		}
		// carry is the guard of a preceding case whose body ends in
		// `fallthrough`: the next body also runs for that case's stages.
		var carry guard
		for _, c := range s.Body.List {
			cc, ok := c.(*ast.CaseClause)
			if !ok {
				continue
			}
			g := guard{stages: append([]string(nil), carry.stages...), negated: carry.negated}
			if cc.List == nil {
				g.negated = true
			}
			for _, e := range cc.List {
				eg := w.guardOf(e)
				g.stages = append(g.stages, eg.stages...)
				g.negated = g.negated || eg.negated
			}
			w.stmts(cc.Body, with(guards, g))
			carry = guard{}
			if endsInFallthrough(cc.Body) {
				carry = g
			}
		}
	case *ast.TypeSwitchStmt:
		for _, c := range s.Body.List {
			if cc, ok := c.(*ast.CaseClause); ok {
				w.stmts(cc.Body, guards)
			}
		}
	case *ast.ForStmt:
		w.stmts(s.Body.List, guards)
	case *ast.RangeStmt:
		w.stmts(s.Body.List, guards)
	case *ast.LabeledStmt:
		w.stmt(s.Stmt, guards)
	case *ast.AssignStmt:
		w.assign(s, guards)
	case *ast.DeclStmt:
		gd, ok := s.Decl.(*ast.GenDecl)
		if !ok {
			return
		}
		for _, sp := range gd.Specs {
			vs, ok := sp.(*ast.ValueSpec)
			if !ok {
				continue
			}
			w.visited[vs] = true
			for i, nm := range vs.Names {
				if nm.Name != runTokenScopesVar {
					continue
				}
				if i < len(vs.Values) {
					w.scopesValue(vs.Values[i], guards)
				}
			}
		}
	}
}

// endsInFallthrough reports whether a case body's last statement is
// `fallthrough`.
func endsInFallthrough(body []ast.Stmt) bool {
	if len(body) == 0 {
		return false
	}
	br, ok := body[len(body)-1].(*ast.BranchStmt)
	return ok && br.Tok == token.FALLTHROUGH
}

// assign handles an assignment statement, recognizing the two shapes that
// write the tracked slice: its literal declaration and an append back into
// it.
func (w *tokenWalker) assign(s *ast.AssignStmt, guards []guard) {
	w.visited[s] = true
	for i, l := range s.Lhs {
		if idx, ok := l.(*ast.IndexExpr); ok && isIdentNamed(idx.X, runTokenScopesVar) {
			w.g.unresolved(RunTokenPrefix, l, "indexed write to "+runTokenScopesVar)
			continue
		}
		if !isIdentNamed(l, runTokenScopesVar) {
			continue
		}
		if len(s.Lhs) != len(s.Rhs) {
			w.g.unresolved(RunTokenPrefix, s, runTokenScopesVar+" assigned from a multi-value expression")
			continue
		}
		w.scopesValue(s.Rhs[i], guards)
	}
}

// scopesValue records the grants of one value assigned to the tracked slice.
func (w *tokenWalker) scopesValue(v ast.Expr, guards []guard) {
	stages := stagesOf(guards)
	switch v := v.(type) {
	case *ast.CompositeLit:
		members, _ := w.g.resolveStringSlice(RunTokenPrefix, v, w.locals, map[string]bool{})
		w.put(members, stages)
		return
	case *ast.CallExpr:
		fn, isIdent := v.Fun.(*ast.Ident)
		if isIdent && fn.Name == "append" && len(v.Args) > 0 && isIdentNamed(v.Args[0], runTokenScopesVar) {
			w.assigned[v] = true
			rest := v.Args[1:]
			if v.Ellipsis.IsValid() {
				if len(rest) != 1 {
					w.g.unresolved(RunTokenPrefix, v, "spread append with extra arguments")
					return
				}
				members, _ := w.g.resolveStringSlice(RunTokenPrefix, rest[0], w.locals, map[string]bool{})
				w.put(members, stages)
				return
			}
			for _, a := range rest {
				if sc, ok := w.g.stringOrRecord(RunTokenPrefix, a, w.locals, "append argument"); ok {
					w.put([]string{sc}, stages)
				}
			}
			return
		}
	}
	w.g.unresolved(RunTokenPrefix, v, runTokenScopesVar+" assigned from "+exprKind(v))
}

func (w *tokenWalker) put(scopes, stages []string) {
	for _, sc := range scopes {
		for _, st := range stages {
			w.g.ext.Grants.Put(Entry{Key: RunTokenPrefix + keySegment(sc) + "@" + st, Value: Present, Rank: PresenceRank, Polarity: Grant})
		}
	}
}

// checkStrayUses records every use of the tracked slice that could change
// its contents outside the recognized shapes: `&scopes` (bare or as a call
// argument), an `append(scopes, ...)` whose result is not assigned back to
// `scopes`, `copy(scopes, ...)`, an alias of the slice bound to another name,
// and an assignment to or declaration of `scopes` the statement walk never
// reached.
func (w *tokenWalker) checkStrayUses(body *ast.BlockStmt) {
	ast.Inspect(body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.AssignStmt:
			if !w.visited[n] && assignsScopes(n.Lhs) {
				w.g.unresolved(RunTokenPrefix, n, "assignment to "+runTokenScopesVar+" outside a recognized statement")
			}
			for i, r := range n.Rhs {
				if !aliasesScopes(r) {
					continue
				}
				if len(n.Lhs) == len(n.Rhs) && isIdentNamed(n.Lhs[i], runTokenScopesVar) {
					continue // a write back into scopes itself: the walk judges it
				}
				w.g.unresolved(RunTokenPrefix, r, "alias of "+runTokenScopesVar)
			}
		case *ast.ValueSpec:
			if !w.visited[n] {
				for _, nm := range n.Names {
					if nm.Name == runTokenScopesVar {
						w.g.unresolved(RunTokenPrefix, n, "declaration of "+runTokenScopesVar+" outside a recognized statement")
					}
				}
			}
			for i, v := range n.Values {
				if aliasesScopes(v) && (i >= len(n.Names) || n.Names[i].Name != runTokenScopesVar) {
					w.g.unresolved(RunTokenPrefix, v, "alias of "+runTokenScopesVar)
				}
			}
		case *ast.UnaryExpr:
			if n.Op == token.AND && isIdentNamed(n.X, runTokenScopesVar) {
				w.g.unresolved(RunTokenPrefix, n, "address of "+runTokenScopesVar+" taken")
			}
		case *ast.CallExpr:
			fn, isIdent := n.Fun.(*ast.Ident)
			if isIdent && fn.Name == "append" && len(n.Args) > 0 && isIdentNamed(n.Args[0], runTokenScopesVar) && !w.assigned[n] {
				w.g.unresolved(RunTokenPrefix, n, "append to "+runTokenScopesVar+" not assigned back")
			}
			if isIdent && fn.Name == "copy" && len(n.Args) > 0 && aliasesScopes(n.Args[0]) {
				w.g.unresolved(RunTokenPrefix, n, "copy into "+runTokenScopesVar)
			}
		}
		return true
	})
}

// aliasesScopes reports whether e is the tracked slice itself or a slice
// expression of it (`scopes`, `scopes[:]`, `scopes[i:j]`, parenthesized):
// a value sharing its backing array.
func aliasesScopes(e ast.Expr) bool {
	for {
		switch x := e.(type) {
		case *ast.ParenExpr:
			e = x.X
		case *ast.SliceExpr:
			e = x.X
		default:
			return isIdentNamed(e, runTokenScopesVar)
		}
	}
}

// assignsScopes reports whether an assignment's left side writes the
// tracked slice (directly or by index).
func assignsScopes(lhs []ast.Expr) bool {
	for _, l := range lhs {
		if idx, ok := l.(*ast.IndexExpr); ok {
			l = idx.X
		}
		if isIdentNamed(l, runTokenScopesVar) {
			return true
		}
	}
	return false
}

// stagesOf folds a guard stack into the stages a grant is keyed under.
func stagesOf(guards []guard) []string {
	set := map[string]bool{}
	for _, g := range guards {
		if g.negated {
			return []string{anyStage}
		}
		for _, s := range g.stages {
			set[s] = true
		}
	}
	if len(set) == 0 {
		return []string{anyStage}
	}
	out := make([]string, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// guardOf collects cond's stage selectors, resolving one level into a
// same-file predicate function.
func (w *tokenWalker) guardOf(cond ast.Expr) guard {
	return w.collect(cond, true)
}

// collect walks e for run.StageType* selectors. A `!=` comparison or a `!`
// over an expression holding a stage selector marks the guard negated, and so
// does an `||` one of whose operands is stageFree. resolve allows ONE level of
// same-file predicate resolution.
func (w *tokenWalker) collect(e ast.Expr, resolve bool) guard {
	var g guard
	ast.Inspect(e, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.SelectorExpr:
			if st, ok := stageSelector(n); ok {
				g.stages = append(g.stages, st)
			}
		case *ast.BinaryExpr:
			switch n.Op {
			case token.NEQ:
				l, r := w.collect(n.X, false), w.collect(n.Y, false)
				if len(l.stages)+len(r.stages) > 0 {
					g.negated = true
				}
			case token.LOR:
				// An operand naming no stage can hold for ANY stage, so the
				// whole disjunction does: key the grant @any_stage.
				l, r := w.collect(n.X, resolve), w.collect(n.Y, resolve)
				if stageFree(l) || stageFree(r) {
					g.negated = true
				}
			}
		case *ast.UnaryExpr:
			if n.Op == token.NOT {
				if sub := w.collect(n.X, resolve); len(sub.stages) > 0 {
					g.negated = true
				}
			}
		case *ast.CallExpr:
			if !resolve {
				return true
			}
			fn, ok := n.Fun.(*ast.Ident)
			if !ok {
				return true
			}
			fd, ok := w.g.funcs[fn.Name]
			if !ok || fd.Body == nil {
				return true
			}
			ast.Inspect(fd.Body, func(m ast.Node) bool {
				ret, ok := m.(*ast.ReturnStmt)
				if !ok {
					return true
				}
				for _, r := range ret.Results {
					sub := w.collect(r, false)
					g.stages = append(g.stages, sub.stages...)
					g.negated = g.negated || sub.negated
				}
				return false
			})
		}
		return true
	})
	return g
}

// stageFree reports whether an operand's guard names no stage and is not
// itself negated: an operand that can be true whatever the stage.
func stageFree(g guard) bool { return len(g.stages) == 0 && !g.negated }

// stageSelector maps `run.StageTypeFooBar` to "foo_bar".
func stageSelector(s *ast.SelectorExpr) (string, bool) {
	q, ok := s.X.(*ast.Ident)
	const p = "StageType"
	if !ok || q.Name != "run" || !strings.HasPrefix(s.Sel.Name, p) || len(s.Sel.Name) == len(p) {
		return "", false
	}
	var b strings.Builder
	for i, r := range s.Sel.Name[len(p):] {
		if unicode.IsUpper(r) {
			if i > 0 {
				b.WriteByte('_')
			}
			r = unicode.ToLower(r)
		}
		b.WriteRune(r)
	}
	return b.String(), true
}

// ExtractGoEnvAllow extracts every package-level string-slice var of an
// environment allow-list file (backend/internal/reviewsandbox/env.go): a var
// whose declared type is []string, or whose value is a `[]string{...}`
// literal, an append, or a call to the same-file pure extend helper. Each
// such var is an anchor (`env_allow.<Var>`) and each of its resolved names a
// presence Grant `env_allow.<Var>.<NAME>` — expansion follows same-file slice
// identifiers and extend(base, extra...), so a name added to BaseAllow is a
// widening on every list built from it.
func ExtractGoEnvAllow(content []byte) (GoExtraction, error) {
	if len(bytes.TrimSpace(content)) == 0 {
		return emptyGoExtraction(), nil
	}
	g, err := parseGoFile(content)
	if err != nil {
		return GoExtraction{}, err
	}
	for _, name := range g.varOrder {
		v, hasValue := g.values[name]
		if !g.isStringSliceVar(name, v, hasValue) {
			continue
		}
		prefix := EnvAllowPrefix + keySegment(name)
		g.ext.Anchors[prefix] = true
		if !hasValue {
			continue // a declared-only []string var holds nothing
		}
		names, ok := g.resolveStringSlice(EnvAllowPrefix, v, nil, map[string]bool{name: true})
		if !ok {
			continue
		}
		for _, n := range names {
			g.ext.Grants.Put(Entry{Key: prefix + "." + keySegment(n), Value: Present, Rank: PresenceRank, Polarity: Grant})
		}
	}
	return g.ext, nil
}

// isStringSliceVar reports whether package var name is a string-slice var by
// its declared type or by its value's shape.
func (g *goFile) isStringSliceVar(name string, v ast.Expr, hasValue bool) bool {
	if t, ok := g.varTypes[name]; ok {
		return isStringSliceType(t)
	}
	if !hasValue {
		return false
	}
	switch v := v.(type) {
	case *ast.CompositeLit:
		return v.Type != nil && isStringSliceType(v.Type)
	case *ast.CallExpr:
		fn, ok := v.Fun.(*ast.Ident)
		return ok && (fn.Name == "append" || fn.Name == extendFunc)
	}
	return false
}

func emptyGoExtraction() GoExtraction {
	return GoExtraction{Grants: Grants{}, Anchors: map[string]bool{}}
}

// withPrefix returns the part of x belonging to the surface whose keys start
// with prefix: its grants, anchors and unresolved constructs.
func (x GoExtraction) withPrefix(prefix string) GoExtraction {
	out := emptyGoExtraction()
	for k, e := range x.Grants {
		if strings.HasPrefix(k, prefix) {
			out.Grants[k] = e
		}
	}
	for a := range x.Anchors {
		if strings.HasPrefix(a, prefix) {
			out.Anchors[a] = true
		}
	}
	for _, u := range x.Unresolved {
		if strings.HasPrefix(u.Prefix, prefix) {
			out.Unresolved = append(out.Unresolved, u)
		}
	}
	return out
}

// shapeProblem is the shape guard: it returns a structural description of
// why base -> head cannot be compared, or "" when it can. It fires when
//
//   - either side holds an unresolved construct (partial recognition loss);
//   - an anchor present at base is missing at head (a renamed or reshaped
//     anchoring declaration);
//   - base yielded >= 1 entry and head yields zero.
//
// Without it, every one of those reads as a silent all-narrowing.
func shapeProblem(base, head GoExtraction) string {
	if len(head.Unresolved) > 0 {
		return "head: " + head.Unresolved[0].Detail
	}
	if len(base.Unresolved) > 0 {
		return "base: " + base.Unresolved[0].Detail
	}
	anchors := make([]string, 0, len(base.Anchors))
	for a := range base.Anchors {
		if !head.Anchors[a] {
			anchors = append(anchors, a)
		}
	}
	if len(anchors) > 0 {
		sort.Strings(anchors)
		return "anchor " + anchors[0] + " present at base is missing at head"
	}
	if len(base.Grants) > 0 && len(head.Grants) == 0 {
		return "base yielded entries, head yields none"
	}
	return ""
}
