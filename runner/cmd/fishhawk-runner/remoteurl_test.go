package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestRemoteURLFor_PerForge pins one case per mode of the derivation seam,
// including both fail-closed modes. The github arm is the byte-identical
// pin: it must produce exactly the literal the four call sites used to carry,
// so routing them through the seam is a no-op on the github path.
func TestRemoteURLFor_PerForge(t *testing.T) {
	for _, tc := range []struct {
		name     string
		cfg      config
		owner    string
		repo     string
		want     string
		wantErr  string
		wantFail bool
	}{
		{
			name:  "github forge is byte-identical to the replaced literal",
			cfg:   config{forge: forgeGitHub},
			owner: "kuhlman-labs", repo: "fishhawk",
			want: "https://github.com/kuhlman-labs/fishhawk",
		},
		{
			name:  "empty zero-value forge takes the same github branch",
			cfg:   config{},
			owner: "acme", repo: "widgets",
			want: "https://github.com/acme/widgets",
		},
		{
			name:  "gitlab.com",
			cfg:   config{forge: forgeGitLab, gitlabBaseURL: "https://gitlab.com"},
			owner: "group", repo: "project",
			want: "https://gitlab.com/group/project",
		},
		{
			// Both the trailing-slash trim and the nested-group rejoin: the
			// callers split the slug at the FIRST separator, so a nested
			// group's remainder arrives whole in repoName.
			name:  "self-managed with a path prefix, trailing slash and a nested group",
			cfg:   config{forge: forgeGitLab, gitlabBaseURL: "https://gl.example/gitlab/"},
			owner: "group", repo: "sub/project",
			want: "https://gl.example/gitlab/group/sub/project",
		},
		{
			name:  "gitlab with an EMPTY base URL fails closed, never a gitlab.com fallback",
			cfg:   config{forge: forgeGitLab},
			owner: "group", repo: "project",
			wantFail: true, wantErr: "--gitlab-base-url",
		},
		{
			name:  "unknown forge fails closed naming the value",
			cfg:   config{forge: "bitbucket"},
			owner: "group", repo: "project",
			wantFail: true, wantErr: `"bitbucket"`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := remoteURLFor(tc.cfg, tc.owner, tc.repo)
			if tc.wantFail {
				if err == nil {
					t.Fatalf("remoteURLFor = %q, nil; want a non-nil error", got)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error = %q, want it to name %q", err, tc.wantErr)
				}
				if got != "" {
					t.Errorf("URL = %q on the fail-closed path, want empty", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("remoteURLFor: %v", err)
			}
			if got != tc.want {
				t.Errorf("remoteURLFor = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestNoHardcodedForgeHostInRemoteURLConstruction is the tripwire against
// re-introducing a hardcoded forge host: no non-test file in this package may
// carry a string LITERAL building a github.com repo URL from format verbs.
// remoteURLFor is the one sanctioned construction site and it concatenates
// rather than formatting, so it does not trip this.
//
// It inspects the parsed AST's string literals, not the raw source text, so a
// doc comment DESCRIBING the old literal (remoteurl.go carries one) is not a
// false positive. It is a cheap tripwire against literal re-introduction, NOT
// a proof of forge-agnosticism — a semantically equivalent construction
// (concatenation, a const) defeats it, which is why every call site also
// carries its own behavioural per-forge assertion.
func TestNoHardcodedForgeHostInRemoteURLConstruction(t *testing.T) {
	// runTestMain chdirs the whole test binary into a throwaway git repo, so
	// the package directory must be resolved from this file's own path, not
	// from the working directory.
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot resolve this test file's path")
	}
	pkgDir := filepath.Dir(thisFile)
	entries, err := os.ReadDir(pkgDir)
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	scanned := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		// Parse WITHOUT comments so only real string literals are inspected.
		file, err := parser.ParseFile(fset, filepath.Join(pkgDir, name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		scanned++
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			if strings.Contains(lit.Value, "https://github.com/%s") {
				t.Errorf("%s:%d: hardcoded github.com repo-URL format literal %s — derive the remote via remoteURLFor(cfg, owner, repoName) instead (E45.80 / #3613)",
					name, fset.Position(lit.Pos()).Line, lit.Value)
			}
			return true
		})
	}
	if scanned == 0 {
		t.Fatal("scanned no package files — the guard is vacuous")
	}
}
