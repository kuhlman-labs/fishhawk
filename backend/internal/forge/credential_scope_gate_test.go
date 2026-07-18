package forge

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// This file is the contract gate for the #1855 forge-credential split
// (phase 5/5, #2013). The staged migration replaced every cross-forge
// `installationID int64` seam with a forge.CredentialScope; this test
// keeps it replaced. It walks the committed Go source of all three
// modules and fails, naming file:line, when an installationID/
// InstallationID int64 DECLARATION appears outside the sanctioned
// survivor set below.
//
// Detection is AST-based, not textual. A line regex only sees the
// `installationID int64` spelling and so misses valid Go that declares
// the same seam differently — grouped names (`installationID, other
// int64`), a comment between the identifier and its type
// (`installationID /* app */ int64`), or a name split across lines. Each
// of those is the same cross-forge seam and each would have passed a
// textual gate. Parsing means the gate sees the declaration the compiler
// sees, and the spelling stops mattering.
//
// The gate matches declarations (`InstallationID int64`, `installationID
// *int64`), not uses: `InstallationID: derefInt64(x)` passes an int64
// into an already-sanctioned struct field and is not itself a new
// cross-forge seam.

// scanModules are the Go modules registered in /go.work. A missing one
// means the workspace layout moved out from under repoRoot's parent
// hops — fail loudly rather than silently scanning nothing.
var scanModules = []string{"backend", "runner", "cli"}

// skipDirs are directory NAMES pruned anywhere in the walk.
//
// "db" mirrors the AGENTS.md coverage-gate convention for sqlc-generated
// packages: generated code models the installation_id column as a bare
// int64 and is not hand-written cross-forge surface. Skipping the
// */db/ DIRECTORY (rather than allowlisting each generated file) means a
// future sqlc package is auto-skipped with no edit here.
var skipDirs = map[string]bool{
	".git":         true,
	"node_modules": true,
	"frontend":     true,
	"testdata":     true,
	"db":           true,
}

// sanctioned maps a repo-relative path prefix to WHY that surface
// legitimately keeps a bare int64 installation id. Everything else is a
// gate failure. Keep the reasons — they are the contract, and a new
// entry should be hard to add without stating one.
var sanctioned = map[string]string{
	// The GitHub App token machinery is GitHub-by-definition: issuing and
	// caching an installation token has no forge-neutral shape.
	"backend/internal/githubapp/": "GitHub App TokenProvider internals",

	// githubclient's UNEXPORTED plumbing (buildRequest/doGraphQL/
	// fetchRulesetContexts) and the credentialTokens adapter resolve a
	// scope to an id exactly once at the exported boundary and pass the
	// int64 down. The exported surface is scope-taking and is pinned by
	// TestNoExportedGitHubClientMethodTakesInt64InstallationID.
	"backend/internal/githubclient/": "githubclient internal plumbing below the scope-taking boundary",

	// Webhook ingest decodes GitHub's wire payload verbatim; the id is a
	// GitHub-shaped input, converted to a scope at the consumer.
	"backend/internal/webhook/webhook.go":    "GitHub webhook ingest payload struct",
	"backend/internal/webhook/dispatcher.go": "GitHub webhook ingest payload struct",

	// Run persistence: installation_id is a nullable column on the run
	// row (*int64). ADR-057's installation_ref migration owns this
	// column's type; the Go mirrors follow it, not this phase.
	"backend/internal/run/run.go":        "run row persistence (installation_id column mirror)",
	"backend/internal/run/repository.go": "run row persistence (installation_id column mirror)",
	"backend/internal/server/runs.go":    "run row persistence (CreateRunParams InstallationID *int64)",

	// Onboarding reports what the GitHub installations API returned. The
	// three payload shapes must stay byte-identical across server, MCP,
	// and CLI, so they mirror the API's int64 verbatim.
	"backend/internal/server/onboarding.go": "onboarding payload mirror of the GitHub installations API",
	"backend/cmd/fishhawk-mcp/client.go":    "onboarding payload mirror of the GitHub installations API",
	"cli/cmd/fishhawk/doctor_onboarding.go": "onboarding payload mirror of the GitHub installations API",

	// (The #1861 runnerbackend.TriggerParams.InstallationID entry was removed
	// once E45.8 flipped that field to a forge.CredentialScope — it was the last
	// known cross-forge int64 seam. The gate now holds it to a scope.)
}

// isInstallationIDName reports whether an identifier IS the installation
// id, in either the exported or unexported spelling. Case-insensitive
// exact match: a compound like AppInstallationID is a different
// identifier and not this gate's concern.
func isInstallationIDName(name string) bool {
	return strings.EqualFold(name, "installationID")
}

// isInt64Type reports whether expr is int64 or *int64.
func isInt64Type(expr ast.Expr) bool {
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = star.X
	}
	id, ok := expr.(*ast.Ident)
	return ok && id.Name == "int64"
}

// declaresInstallationIDInt64 reports whether a name list and type
// together declare an int64 installation id. Taking the whole name list
// is what catches a grouped declaration: in `installationID, other
// int64` the type is shared, so a per-identifier view of the source text
// never sees the two adjacent.
func declaresInstallationIDInt64(names []*ast.Ident, typ ast.Expr) bool {
	if typ == nil || !isInt64Type(typ) {
		return false
	}
	for _, n := range names {
		if isInstallationIDName(n.Name) {
			return true
		}
	}
	return false
}

// int64Decl is one offending declaration, located for reporting.
type int64Decl struct {
	rel  string
	line int
	text string
}

func (d int64Decl) String() string {
	return d.rel + ":" + strconv.Itoa(d.line) + ": " + d.text
}

// installationIDDeclsIn parses src and returns every int64 installation-id
// declaration in it: struct fields, func parameters and results,
// interface method signatures (all *ast.Field), and var/const specs
// (*ast.ValueSpec). Uses of an already-declared field are not
// declarations and are not reported.
func installationIDDeclsIn(rel, src string) ([]int64Decl, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, rel, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(src, "\n")
	at := func(pos token.Pos) int64Decl {
		p := fset.Position(pos)
		text := ""
		if p.Line-1 >= 0 && p.Line-1 < len(lines) {
			text = strings.TrimSpace(lines[p.Line-1])
		}
		return int64Decl{rel: rel, line: p.Line, text: text}
	}

	var decls []int64Decl
	ast.Inspect(file, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.Field:
			if declaresInstallationIDInt64(node.Names, node.Type) {
				decls = append(decls, at(node.Pos()))
			}
		case *ast.ValueSpec:
			if declaresInstallationIDInt64(node.Names, node.Type) {
				decls = append(decls, at(node.Pos()))
			}
		}
		return true
	})
	return decls, nil
}

// exportedClientInt64Methods returns every EXPORTED *Client method in src
// that takes an int64 installation id in any parameter position.
func exportedClientInt64Methods(rel, src string) ([]int64Decl, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, rel, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(src, "\n")

	var found []int64Decl
	for _, d := range file.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Recv == nil || len(fn.Recv.List) != 1 || !fn.Name.IsExported() {
			continue
		}
		if !isClientReceiver(fn.Recv.List[0].Type) {
			continue
		}
		for _, p := range fn.Type.Params.List {
			if !declaresInstallationIDInt64(p.Names, p.Type) {
				continue
			}
			pos := fset.Position(fn.Pos())
			text := ""
			if pos.Line-1 >= 0 && pos.Line-1 < len(lines) {
				text = strings.TrimSpace(lines[pos.Line-1])
			}
			found = append(found, int64Decl{rel: rel, line: pos.Line, text: text})
			break
		}
	}
	return found, nil
}

// isClientReceiver reports whether a receiver type is *Client.
func isClientReceiver(expr ast.Expr) bool {
	star, ok := expr.(*ast.StarExpr)
	if !ok {
		return false
	}
	id, ok := star.X.(*ast.Ident)
	return ok && id.Name == "Client"
}

// repoRoot returns the workspace root by hopping three parents up from
// this file's directory (<root>/backend/internal/forge).
func repoRoot(t *testing.T) string {
	t.Helper()
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed: cannot locate the repo root")
	}
	root := filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(self))))
	for _, m := range scanModules {
		if _, err := os.Stat(filepath.Join(root, m)); err != nil {
			t.Fatalf("module %q not found under derived repo root %q: %v"+
				" (the workspace layout moved; fix repoRoot's parent hops"+
				" rather than letting the gate scan nothing)", m, root, err)
		}
	}
	return root
}

// walkGoSources calls fn(relPath, content) for every non-test .go file
// in the scanned modules.
func walkGoSources(t *testing.T, fn func(rel, content string)) {
	t.Helper()
	root := repoRoot(t)
	for _, mod := range scanModules {
		err := filepath.WalkDir(filepath.Join(root, mod), func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if skipDirs[d.Name()] {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			fn(filepath.ToSlash(rel), string(b))
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", mod, err)
		}
	}
}

func isSanctioned(rel string) bool {
	for prefix := range sanctioned {
		if strings.HasPrefix(rel, prefix) {
			return true
		}
	}
	return false
}

// collectInstallationIDDecls parses every scanned source file and returns
// all int64 installation-id declarations, sanctioned or not. A file that
// does not parse fails the gate rather than being skipped: an unparseable
// file is an unscanned file, and an unscanned file is a hole.
func collectInstallationIDDecls(t *testing.T) []int64Decl {
	t.Helper()
	var all []int64Decl
	walkGoSources(t, func(rel, content string) {
		decls, err := installationIDDeclsIn(rel, content)
		if err != nil {
			t.Fatalf("parse %s: %v (the gate cannot scan what it cannot parse)", rel, err)
		}
		all = append(all, decls...)
	})
	return all
}

// TestNoUnsanctionedInt64InstallationIDSurface is the contract: after
// #2013 every cross-forge installation seam is a forge.CredentialScope.
// A new `installationID int64` declaration outside the sanctioned set is
// a new forge-coupled surface and fails here with its file:line.
func TestNoUnsanctionedInt64InstallationIDSurface(t *testing.T) {
	var offenders []string
	for _, d := range collectInstallationIDDecls(t) {
		if isSanctioned(d.rel) {
			continue
		}
		offenders = append(offenders, d.String())
	}
	if len(offenders) > 0 {
		t.Fatalf("%d unsanctioned int64 installation-id declaration(s) — use forge.CredentialScope,"+
			" or add a sanctioned[] entry stating why this surface is forge-specific:\n\t%s",
			len(offenders), strings.Join(offenders, "\n\t"))
	}
}

// TestNoExportedGitHubClientMethodTakesInt64InstallationID narrows the
// whole-package githubclient sanction: its EXPORTED surface must stay
// scope-taking. Anchored to the githubclient package deliberately —
// githubapp.Client.IssueInstallationToken matches the same method shape
// and is a sanctioned survivor, so a package-blind pattern would fail on
// a legitimately GitHub-shaped API.
func TestNoExportedGitHubClientMethodTakesInt64InstallationID(t *testing.T) {
	const pkg = "backend/internal/githubclient/"
	var offenders []string
	scanned := 0
	walkGoSources(t, func(rel, content string) {
		if !strings.HasPrefix(rel, pkg) {
			return
		}
		scanned++
		found, err := exportedClientInt64Methods(rel, content)
		if err != nil {
			t.Fatalf("parse %s: %v (the gate cannot scan what it cannot parse)", rel, err)
		}
		for _, d := range found {
			offenders = append(offenders, d.String())
		}
	})
	if scanned == 0 {
		t.Fatalf("scanned no files under %s — the assertion would pass vacuously", pkg)
	}
	if len(offenders) > 0 {
		t.Fatalf("%d exported *githubclient.Client method(s) take a bare int64 installation id;"+
			" the exported surface must take a forge.CredentialScope:\n\t%s",
			len(offenders), strings.Join(offenders, "\n\t"))
	}
}

// TestSanctionedSurfacesStillExist keeps the allowlist honest: an entry
// whose file no longer matches is dead weight that would silently
// re-sanction the path if code moved back into it.
func TestSanctionedSurfacesStillExist(t *testing.T) {
	matched := map[string]bool{}
	for _, d := range collectInstallationIDDecls(t) {
		for prefix := range sanctioned {
			if strings.HasPrefix(d.rel, prefix) {
				matched[prefix] = true
			}
		}
	}
	for prefix, why := range sanctioned {
		if !matched[prefix] {
			t.Errorf("sanctioned entry %q (%s) no longer has an int64 installation-id declaration;"+
				" drop the entry", prefix, why)
		}
	}
}

// TestInstallationIDDeclDetectionForms pins the detector against the
// declaration spellings a line-oriented gate misses. Every "declares"
// case below is a real cross-forge int64 seam that a regex over source
// text does not match; they are the reason detection parses.
func TestInstallationIDDeclDetectionForms(t *testing.T) {
	tests := []struct {
		name     string
		src      string
		declares bool
	}{
		{
			name:     "plain parameter",
			src:      `func f(installationID int64) {}`,
			declares: true,
		},
		{
			name:     "grouped parameter names share a type",
			src:      `func f(installationID, other int64) {}`,
			declares: true,
		},
		{
			name:     "grouped parameter names, installation id second",
			src:      `func f(other, installationID int64) {}`,
			declares: true,
		},
		{
			name:     "block comment between identifier and type",
			src:      `func f(installationID /* the app install */ int64) {}`,
			declares: true,
		},
		{
			name: "grouped names split across lines",
			src: "type T struct {\n" +
				"\tInstallationID,\n" +
				"\tOwnerID int64\n" +
				"}",
			declares: true,
		},
		{
			name:     "pointer parameter",
			src:      `func f(installationID *int64) {}`,
			declares: true,
		},
		{
			name:     "exported struct field",
			src:      "type T struct {\n\tInstallationID int64\n}",
			declares: true,
		},
		{
			name:     "grouped struct field names",
			src:      "type T struct {\n\tOwnerID, InstallationID int64\n}",
			declares: true,
		},
		{
			name:     "interface method parameter",
			src:      "type I interface {\n\tDo(installationID int64) error\n}",
			declares: true,
		},
		{
			name:     "function result",
			src:      `func f() (installationID int64) { return 0 }`,
			declares: true,
		},
		{
			name:     "package-level var",
			src:      `var installationID int64`,
			declares: true,
		},
		{
			name:     "use as a struct literal field is not a declaration",
			src:      `func f(s forge.CredentialScope) T { return T{InstallationID: derefInt64(s)} }`,
			declares: false,
		},
		{
			name:     "compound identifier is a different name",
			src:      `func f(appInstallationID int64) {}`,
			declares: false,
		},
		{
			name:     "installation id that is not an int64",
			src:      `func f(installationID string) {}`,
			declares: false,
		},
		{
			name:     "scope-taking seam is the migrated shape",
			src:      `func f(scope forge.CredentialScope) {}`,
			declares: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			decls, err := installationIDDeclsIn("x.go", "package p\n"+tt.src+"\n")
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if got := len(decls) > 0; got != tt.declares {
				t.Errorf("installationIDDeclsIn(%q) declares = %v, want %v", tt.src, got, tt.declares)
			}
		})
	}
}

// TestExportedClientInt64MethodDetectionForms pins the exported-surface
// assertion against the same class of spellings, plus the receiver and
// export filters it depends on.
func TestExportedClientInt64MethodDetectionForms(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want bool
	}{
		{
			name: "exported method with a bare int64 installation id",
			src:  `func (c *Client) Do(ctx context.Context, installationID int64) error { return nil }`,
			want: true,
		},
		{
			name: "grouped parameter names hide the type",
			src:  `func (c *Client) Do(ctx context.Context, installationID, repoID int64) error { return nil }`,
			want: true,
		},
		{
			name: "installation id in a later parameter position",
			src:  `func (c *Client) Do(ctx context.Context, owner string, installationID int64) error { return nil }`,
			want: true,
		},
		{
			name: "block comment between identifier and type",
			src:  `func (c *Client) Do(ctx context.Context, installationID /* app */ int64) error { return nil }`,
			want: true,
		},
		{
			name: "unexported method is sanctioned internal plumbing",
			src:  `func (c *Client) do(ctx context.Context, installationID int64) error { return nil }`,
			want: false,
		},
		{
			name: "exported scope-taking method is the migrated shape",
			src:  `func (c *Client) Do(ctx context.Context, scope forge.CredentialScope) error { return nil }`,
			want: false,
		},
		{
			name: "another type's method is not the Client surface",
			src:  `func (c *other) Do(ctx context.Context, installationID int64) error { return nil }`,
			want: false,
		},
		{
			name: "plain function is not a Client method",
			src:  `func Do(ctx context.Context, installationID int64) error { return nil }`,
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			found, err := exportedClientInt64Methods("x.go", "package p\n"+tt.src+"\n")
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if got := len(found) > 0; got != tt.want {
				t.Errorf("exportedClientInt64Methods(%q) = %v, want %v", tt.src, got, tt.want)
			}
		})
	}
}
