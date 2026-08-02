package oauthas

import (
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"strconv"
	"testing"
)

// TestSupportedScopes_MatchesOperatorDefault is a REAL drift detector (#2427
// condition 5, option a): operatorDefaultScopes lives in the main package
// backend/cmd/fishhawkd and cannot be imported, so this AST-parses token.go and
// compares the parsed literal against SupportedScopes by value AND order. A
// hardcoded duplicate would be a false assertion; parsing the actual source is not.
func TestSupportedScopes_MatchesOperatorDefault(t *testing.T) {
	t.Parallel()
	const path = "../../cmd/fishhawkd/token.go"
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	got := extractStringSliceVar(t, f, "operatorDefaultScopes")
	if len(got) == 0 {
		t.Fatalf("could not extract operatorDefaultScopes from %s", path)
	}
	if !reflect.DeepEqual(got, SupportedScopes) {
		t.Fatalf("scope drift:\n token.go operatorDefaultScopes = %v\n oauthas.SupportedScopes       = %v", got, SupportedScopes)
	}
}

// extractStringSliceVar returns the string literals of a top-level
// `var name = []string{...}` declaration.
func extractStringSliceVar(t *testing.T, f *ast.File, name string) []string {
	t.Helper()
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.VAR {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, id := range vs.Names {
				if id.Name != name || i >= len(vs.Values) {
					continue
				}
				lit, ok := vs.Values[i].(*ast.CompositeLit)
				if !ok {
					continue
				}
				out := make([]string, 0, len(lit.Elts))
				for _, elt := range lit.Elts {
					bl, ok := elt.(*ast.BasicLit)
					if !ok || bl.Kind != token.STRING {
						continue
					}
					s, err := strconv.Unquote(bl.Value)
					if err != nil {
						t.Fatalf("unquote %q: %v", bl.Value, err)
					}
					out = append(out, s)
				}
				return out
			}
		}
	}
	return nil
}

func TestIsSupportedScope(t *testing.T) {
	t.Parallel()
	if !IsSupportedScope("read:runs") {
		t.Fatalf("read:runs should be supported")
	}
	if IsSupportedScope("mcp:read") {
		t.Fatalf("mcp:read should not be in the operator vocabulary")
	}
	if IsSupportedScope("") {
		t.Fatalf("empty scope should not be supported")
	}
}

func TestParseScope(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		in      string
		want    []string
		wantErr bool
	}{
		{name: "single", in: "read:runs", want: []string{"read:runs"}},
		{name: "multi", in: "read:runs write:runs", want: []string{"read:runs", "write:runs"}},
		{name: "dedup preserves first-seen order", in: "write:runs read:runs write:runs", want: []string{"write:runs", "read:runs"}},
		{name: "multi-space tolerated", in: "read:runs   write:runs", want: []string{"read:runs", "write:runs"}},
		{name: "unknown refused", in: "read:runs bogus:scope", wantErr: true},
		{name: "empty refused", in: "", wantErr: true},
		{name: "all-space refused", in: "   ", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseScope(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseScope(%q) = %v, want error", tc.in, got)
				}
				assertCode(t, err, ErrCodeInvalidScope)
				return
			}
			if err != nil {
				t.Fatalf("ParseScope(%q) unexpected error: %v", tc.in, err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("ParseScope(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestScopeString(t *testing.T) {
	t.Parallel()
	in := []string{"read:runs", "write:runs"}
	if got := ScopeString(in); got != "read:runs write:runs" {
		t.Fatalf("ScopeString = %q", got)
	}
	// Round-trip through ParseScope.
	back, err := ParseScope(ScopeString(in))
	if err != nil {
		t.Fatalf("round-trip parse: %v", err)
	}
	if !reflect.DeepEqual(back, in) {
		t.Fatalf("round-trip = %v, want %v", back, in)
	}
}
