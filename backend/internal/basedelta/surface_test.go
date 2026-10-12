package basedelta

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/bmatcuk/doublestar/v4"
)

func TestGlobalSurfacePatternsValid(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for _, p := range GlobalSurface {
		if !doublestar.ValidatePattern(p) {
			t.Errorf("invalid pattern %q", p)
		}
		if strings.HasPrefix(p, "/") || strings.HasPrefix(p, "./") {
			t.Errorf("pattern %q must be root-relative without a leading / or ./", p)
		}
		if seen[p] {
			t.Errorf("duplicate pattern %q", p)
		}
		seen[p] = true
	}
	for _, d := range SharedArtifactDirs {
		if !strings.HasSuffix(d, "/") {
			t.Errorf("SharedArtifactDirs entry %q must end in /", d)
		}
	}
}

func TestMatchGlobal_RootAnchored(t *testing.T) {
	t.Parallel()
	for _, p := range []string{
		"testdata/wire/x.json", "backend/go.mod", "go.mod", "go.work", "docs/ARCHITECTURE.md",
		"scripts/test", "backend/internal/postgres/migrations/0102_x.up.sql",
		"cli/cmd/fishhawk-docgen/main.go", "deploy/helm/fishhawk/values.yaml",
	} {
		if _, ok := MatchGlobal(p); !ok {
			t.Errorf("%s should be global", p)
		}
	}
	for _, p := range []string{
		"backend/internal/x/testdata/y.json", "docs/README.md", "backend/internal/server/server.go",
		"frontend/src/a.ts", "README.md", "x/scripts/y", "backend/internal/spec/testdata/other.json",
	} {
		if pat, ok := MatchGlobal(p); ok {
			t.Errorf("%s should not be global (matched %q)", p, pat)
		}
	}
	if d, ok := sharedDir("frontend/src/a.ts"); !ok || d != "frontend/" {
		t.Errorf("sharedDir(frontend/src/a.ts) = %q, %v", d, ok)
	}
	if _, ok := sharedDir("backend/x.go"); ok {
		t.Error("backend/x.go is under no shared dir")
	}
}

// adr096D3Floor is ADR-096 D3 as #4292 enumerates it, hard-coded here and
// NOT read from GlobalSurface or README.md, so dropping a pattern from both
// the var and the README still fails. Each pattern carries paths it must keep
// global.
var adr096D3Floor = []struct {
	pattern string
	samples []string
}{
	{"go.work", []string{"go.work"}},
	{"**/go.mod", []string{"go.mod", "backend/go.mod", "a/b/go.mod"}},
	{"**/go.sum", []string{"go.sum", "backend/go.sum"}},
	{"scripts/**", []string{"scripts/test", "scripts/lib/x.sh"}},
	{".golangci.yml", []string{".golangci.yml"}},
	{"deploy/gate-image/**", []string{"deploy/gate-image/Dockerfile"}},
	{".fishhawk/**", []string{".fishhawk/workflows.yaml", ".fishhawk/x/y.yaml"}},
	{"docs/spec/**", []string{"docs/spec/workflow-v2.schema.json"}},
	{"docs/api/**", []string{"docs/api/v0.openapi.yaml"}},
	{"site/**", []string{"site/astro.config.mjs", "site/src/content/docs/reference/cli.md"}},
	{"docs/adr/**", []string{"docs/adr/index.json"}},
	{"docs/ARCHITECTURE.md", []string{"docs/ARCHITECTURE.md"}},
	{"backend/internal/postgres/migrations/**", []string{"backend/internal/postgres/migrations/0001_init.up.sql"}},
	{"backend/internal/audit/categories.go", []string{"backend/internal/audit/categories.go"}},
	{"testdata/**", []string{"testdata/wire/restart_blockers.json", "testdata/policy/agent-instruction-paths.json"}},
}

func TestGlobalSurfaceKeepsADR096D3Floor(t *testing.T) {
	t.Parallel()
	in := map[string]bool{}
	for _, p := range GlobalSurface {
		in[p] = true
	}
	for _, f := range adr096D3Floor {
		if !in[f.pattern] {
			t.Errorf("ADR-096 D3 floor pattern %q is missing from GlobalSurface; the floor shrinks only through an ADR amendment", f.pattern)
		}
		for _, s := range f.samples {
			if _, ok := MatchGlobal(s); !ok {
				t.Errorf("%s (ADR-096 D3 floor %q) is not global", s, f.pattern)
			}
		}
	}
}

// The README's set table is the documentation the var points at; the two
// must list the same patterns.
func TestGlobalSurfaceMatchesREADME(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join(pkgSrcDir, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	start := strings.Index(body, "\n## Global surface set")
	if start < 0 {
		t.Fatal("README.md has no '## Global surface set' section")
	}
	section := body[start+1:]
	if end := strings.Index(section[3:], "\n## "); end >= 0 {
		section = section[:end+3]
	}
	row := regexp.MustCompile("(?m)^\\| `([^`]+)` \\|")
	doc := map[string]bool{}
	for _, m := range row.FindAllStringSubmatch(section, -1) {
		doc[m[1]] = true
	}
	code := map[string]bool{}
	for _, p := range GlobalSurface {
		code[p] = true
		if !doc[p] {
			t.Errorf("GlobalSurface pattern %q has no row in README.md's set table", p)
		}
	}
	for p := range doc {
		if !code[p] {
			t.Errorf("README.md set table lists %q, absent from GlobalSurface", p)
		}
	}
}
