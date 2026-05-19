package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDiscoverSpec_FromExplicitFile exercises the explicit-path
// branch: a supplied spec_file MUST exist and parse. startDir is
// deliberately elsewhere so a misbehaving walk-up would mask the
// expected behavior.
func TestDiscoverSpec_FromExplicitFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "custom.yaml")
	body := []byte("version: \"0.3\"\nworkflows: {}\n")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := discoverSpec(t.TempDir(), path)
	if err != nil {
		t.Fatalf("discoverSpec: %v", err)
	}
	if got == nil {
		t.Fatal("discoverSpec returned nil")
	}
	if string(got.Contents) != string(body) {
		t.Errorf("Contents mismatch: %q vs %q", got.Contents, body)
	}
	if got.BlobSHA == "" {
		t.Error("BlobSHA empty")
	}
}

// TestDiscoverSpec_ExplicitFile_NotFound surfaces the file-read
// error verbatim. A typo on spec_file should fail the tool call,
// not silently fall through to walk-up.
func TestDiscoverSpec_ExplicitFile_NotFound(t *testing.T) {
	_, err := discoverSpec(t.TempDir(), "/definitely/does/not/exist.yaml")
	if err == nil {
		t.Fatal("err = nil, want failure")
	}
	if !strings.Contains(err.Error(), "spec_file") {
		t.Errorf("err should reference spec_file: %v", err)
	}
}

// TestDiscoverSpec_WalksUpToGitRoot mirrors the production layout:
// the spec lives at the repo root, the working directory is a
// nested package. Walk should find it.
func TestDiscoverSpec_WalksUpToGitRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".fishhawk"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := []byte("version: \"0.3\"\nworkflows: {}\n")
	if err := os.WriteFile(filepath.Join(root, ".fishhawk", "workflows.yaml"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(root, "pkg", "subpkg")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := discoverSpec(nested, "")
	if err != nil {
		t.Fatalf("discoverSpec: %v", err)
	}
	if got == nil {
		t.Fatal("walk did not find spec")
	}
	if string(got.Contents) != string(body) {
		t.Errorf("Contents mismatch")
	}
}

// TestDiscoverSpec_StopsAtGitBoundary documents the safety guard:
// the walk does NOT cross out of the repo even if a parent
// directory happens to have a .fishhawk/workflows.yaml.
func TestDiscoverSpec_StopsAtGitBoundary(t *testing.T) {
	outer := t.TempDir()
	if err := os.MkdirAll(filepath.Join(outer, ".fishhawk"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outer, ".fishhawk", "workflows.yaml"),
		[]byte("outer\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	inner := filepath.Join(outer, "inner")
	if err := os.MkdirAll(filepath.Join(inner, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := discoverSpec(inner, "")
	if err != nil {
		t.Fatalf("discoverSpec: %v", err)
	}
	if got != nil {
		t.Errorf("walk crossed .git boundary; got spec from %q", got.Path)
	}
}

// TestDiscoverSpec_NoSpecAnywhere returns (nil, nil) — the
// "agent wants to mint a stage-less seed" path. startRun then
// either uses workflow_sha or errors with the remediation hint.
func TestDiscoverSpec_NoSpecAnywhere(t *testing.T) {
	dir := t.TempDir()
	got, err := discoverSpec(dir, "")
	if err != nil {
		t.Errorf("err = %v, want nil", err)
	}
	if got != nil {
		t.Errorf("got = %#v, want nil", got)
	}
}

// TestGitBlobSHA_KnownValue locks in the formula so the MCP
// server's computed SHA matches git's. The expected hash is what
// `printf 'hello\n' | git hash-object --stdin` emits — same value
// the CLI's test (cli/cmd/fishhawk/spec_discover_test.go) checks.
func TestGitBlobSHA_KnownValue(t *testing.T) {
	got := gitBlobSHA([]byte("hello\n"))
	want := "ce013625030ba8dba906f756967f9e9ca394464a"
	if got != want {
		t.Errorf("gitBlobSHA(\"hello\\n\") = %q, want %q", got, want)
	}
}
