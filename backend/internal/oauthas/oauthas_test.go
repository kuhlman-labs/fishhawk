package oauthas

import (
	"errors"
	"go/build"
	"strings"
	"testing"
)

func TestParseIssuer(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		in      string
		wantErr bool
		want    string
	}{
		{name: "https ok", in: "https://as.example", want: "https://as.example"},
		{name: "trailing slash normalized", in: "https://as.example/", want: "https://as.example"},
		{name: "path trailing slash normalized", in: "https://as.example/tenant/", want: "https://as.example/tenant"},
		{name: "scheme uppercased normalized", in: "HTTPS://as.example", want: "https://as.example"},
		{name: "http refused", in: "http://as.example", wantErr: true},
		{name: "userinfo refused", in: "https://u:p@as.example", wantErr: true},
		{name: "query refused", in: "https://as.example/?x=1", wantErr: true},
		{name: "fragment refused", in: "https://as.example/#f", wantErr: true},
		{name: "empty-fragment refused", in: "https://as.example/#", wantErr: true},
		{name: "empty refused", in: "", wantErr: true},
		{name: "hostless refused", in: "https:///path", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseIssuer(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseIssuer(%q) = %q, want error", tc.in, got)
				}
				assertCode(t, err, ErrCodeInvalidRequest)
				return
			}
			if err != nil {
				t.Fatalf("ParseIssuer(%q) unexpected error: %v", tc.in, err)
			}
			if got.String() != tc.want {
				t.Fatalf("ParseIssuer(%q) = %q, want %q", tc.in, got.String(), tc.want)
			}
		})
	}
}

func TestParseResourceIdentifier(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{name: "absolute ok", in: "https://mcp.example/v0"},
		{name: "query preserved ok", in: "https://mcp.example/v0?a=b"},
		{name: "http ok (any absolute scheme)", in: "http://mcp.example/v0"},
		{name: "fragment refused", in: "https://mcp.example/v0#frag", wantErr: true},
		{name: "empty-fragment refused", in: "https://mcp.example/v0#", wantErr: true},
		{name: "relative refused", in: "/v0", wantErr: true},
		{name: "hostless refused", in: "urn:example:resource", wantErr: true},
		{name: "empty refused", in: "", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseResourceIdentifier(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseResourceIdentifier(%q) = %q, want error", tc.in, got)
				}
				assertCode(t, err, ErrCodeInvalidTarget)
				return
			}
			if err != nil {
				t.Fatalf("ParseResourceIdentifier(%q) unexpected error: %v", tc.in, err)
			}
			if got.String() != tc.in {
				t.Fatalf("ParseResourceIdentifier(%q) = %q, want raw preserved", tc.in, got.String())
			}
		})
	}
}

func TestResourceMatches(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		audience  string
		requested string
		wantMatch bool
	}{
		{name: "exact", audience: "https://mcp.example/v0", requested: "https://mcp.example/v0", wantMatch: true},
		{name: "default port elided", audience: "https://mcp.example/v0", requested: "https://mcp.example:443/v0", wantMatch: true},
		{name: "http default port elided", audience: "http://mcp.example/v0", requested: "http://mcp.example:80/v0", wantMatch: true},
		{name: "host case-insensitive", audience: "https://MCP.Example/v0", requested: "https://mcp.example/v0", wantMatch: true},
		{name: "scheme case-insensitive", audience: "HTTPS://mcp.example/v0", requested: "https://mcp.example/v0", wantMatch: true},
		{name: "empty path equals slash", audience: "https://mcp.example", requested: "https://mcp.example/", wantMatch: true},
		{name: "query preserved match", audience: "https://mcp.example/v0?a=b", requested: "https://mcp.example/v0?a=b", wantMatch: true},
		{name: "differing path", audience: "https://mcp.example/v0", requested: "https://mcp.example/v1", wantMatch: false},
		{name: "path case-sensitive", audience: "https://mcp.example/V0", requested: "https://mcp.example/v0", wantMatch: false},
		{name: "differing query", audience: "https://mcp.example/v0?a=b", requested: "https://mcp.example/v0?a=c", wantMatch: false},
		{name: "non-default port significant", audience: "https://mcp.example/v0", requested: "https://mcp.example:8443/v0", wantMatch: false},
		{name: "differing host", audience: "https://mcp.example/v0", requested: "https://evil.example/v0", wantMatch: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := ResourceMatches(tc.audience, tc.requested); got != tc.wantMatch {
				t.Fatalf("ResourceMatches(%q, %q) = %v, want %v", tc.audience, tc.requested, got, tc.wantMatch)
			}
		})
	}
}

// TestPackage_ImportsNeitherServerNorDatabase makes the issue's "imports no
// backend/internal/server and touches no database" done-means machine-enforced.
func TestPackage_ImportsNeitherServerNorDatabase(t *testing.T) {
	t.Parallel()
	pkg, err := build.ImportDir(".", 0)
	if err != nil {
		t.Fatalf("build.ImportDir: %v", err)
	}
	forbidden := []string{
		"internal/server",
		"internal/postgres",
		"internal/db",
		"jackc/pgx",
		"internal/apitoken",
	}
	all := append([]string{}, pkg.Imports...)
	all = append(all, pkg.TestImports...)
	for _, imp := range all {
		for _, bad := range forbidden {
			if strings.Contains(imp, bad) {
				t.Errorf("package oauthas imports forbidden path %q (matches %q)", imp, bad)
			}
		}
	}
}

// assertCode asserts err is (or wraps) an *Error carrying the given code.
func assertCode(t *testing.T, err error, code ErrorCode) {
	t.Helper()
	var oe *Error
	if !errors.As(err, &oe) {
		t.Fatalf("error %v is not an *Error", err)
	}
	if oe.Code != code {
		t.Fatalf("error code = %q, want %q (err=%v)", oe.Code, code, err)
	}
}
