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
