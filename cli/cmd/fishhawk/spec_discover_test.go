package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDiscoverSpec_FromExplicitFile exercises the --spec-file path:
// when set, the CLI reads exactly that file without walking.
func TestDiscoverSpec_FromExplicitFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "custom.yaml")
	body := []byte("version: \"0.3\"\nworkflows: {}\n")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}

	// startDir deliberately points elsewhere; --spec-file must win.
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
// error verbatim. A typo on --spec-file should fail the verb, not
// silently fall through to walk-up.
func TestDiscoverSpec_ExplicitFile_NotFound(t *testing.T) {
	_, err := discoverSpec(t.TempDir(), "/definitely/does/not/exist.yaml")
	if err == nil {
		t.Fatal("err = nil, want failure")
	}
	if !strings.Contains(err.Error(), "--spec-file") {
		t.Errorf("err should reference --spec-file: %v", err)
	}
}

// TestDiscoverSpec_WalksUpToGitRoot mirrors the production layout:
// the spec lives at the repo root, the working directory is a
// nested package. Walk should find it.
func TestDiscoverSpec_WalksUpToGitRoot(t *testing.T) {
	root := t.TempDir()
	// Mark the dir as a git repo root with a sentinel.
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
// directory happens to have a .fishhawk/workflows.yaml. Prevents
// `fishhawk run start` inside a sub-checkout from accidentally
// adopting an outer repo's spec.
func TestDiscoverSpec_StopsAtGitBoundary(t *testing.T) {
	outer := t.TempDir()
	// Outer dir has a spec; inner dir is the .git boundary.
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
// "operator wants to mint a run without a local checkout" path.
// runStart then either uses --workflow-sha or errors out with the
// remediation hint.
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

// TestGitBlobSHA_KnownValue locks in the formula used to compute
// the blob hash. The expected hash below is what `git hash-object`
// would emit for the same content. Verifies the framing string
// ("blob N\0…") matches git's.
func TestGitBlobSHA_KnownValue(t *testing.T) {
	// `printf 'hello\n' | git hash-object --stdin`
	// → ce013625030ba8dba906f756967f9e9ca394464a
	got := gitBlobSHA([]byte("hello\n"))
	want := "ce013625030ba8dba906f756967f9e9ca394464a"
	if got != want {
		t.Errorf("gitBlobSHA(\"hello\\n\") = %q, want %q", got, want)
	}
}

// TestRunStart_AutoDiscoverSendsSpec exercises the end-to-end CLI
// behavior: when a spec is reachable from --working-dir, the bytes
// AND the computed SHA forward to the backend. We don't need a
// fake fishhawk-runner here — the fake backend captures the input.
func TestRunStart_AutoDiscoverSendsSpec(t *testing.T) {
	fb, srv := newFakeBackend(t)
	withBackend(t, srv)

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".fishhawk"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := []byte("version: \"0.3\"\nworkflows:\n  trivial:\n    stages:\n      - id: implement\n        type: implement\n        executor:\n          agent: claude-code\n        produces:\n          - artifact: pull_request\n")
	if err := os.WriteFile(filepath.Join(dir, ".fishhawk", "workflows.yaml"), body, 0o600); err != nil {
		t.Fatal(err)
	}

	rc := run([]string{
		"run", "start",
		"--repo", "x/y", "--workflow", "trivial",
		"--working-dir", dir,
	}, io.Discard, io.Discard)
	if rc != exitOK {
		t.Fatalf("rc = %d, want exitOK", rc)
	}
	if fb.startedRun.WorkflowSpec == "" {
		t.Error("WorkflowSpec not forwarded to backend")
	}
	if fb.startedRun.WorkflowSHA != gitBlobSHA(body) {
		t.Errorf("WorkflowSHA = %q, want auto-computed %q",
			fb.startedRun.WorkflowSHA, gitBlobSHA(body))
	}
}

// TestRunStart_SpecFileOverridesAutoDiscover documents the
// precedence: --spec-file wins over the walk-up auto-detect.
func TestRunStart_SpecFileOverridesAutoDiscover(t *testing.T) {
	fb, srv := newFakeBackend(t)
	withBackend(t, srv)

	// The walking starts in cli/cmd/fishhawk which is inside the
	// real fishhawk repo, so without an override it would pick up
	// the real workflows.yaml. The override should win.
	override := filepath.Join(t.TempDir(), "explicit.yaml")
	body := []byte("version: \"0.3\"\nworkflows:\n  trivial:\n    stages:\n      - id: implement\n        type: implement\n        executor:\n          agent: claude-code\n        produces:\n          - artifact: pull_request\n")
	if err := os.WriteFile(override, body, 0o600); err != nil {
		t.Fatal(err)
	}
	rc := run([]string{
		"run", "start",
		"--repo", "x/y", "--workflow", "trivial",
		"--spec-file", override,
	}, io.Discard, io.Discard)
	if rc != exitOK {
		t.Fatalf("rc = %d", rc)
	}
	if fb.startedRun.WorkflowSpec != string(body) {
		t.Errorf("WorkflowSpec did not match --spec-file body")
	}
}

// TestRunStart_NoSpecNoSHA emits the dual-remediation error.
func TestRunStart_NoSpecNoSHA(t *testing.T) {
	_, srv := newFakeBackend(t)
	withBackend(t, srv)

	dir := t.TempDir() // empty dir, no spec
	var stderr strings.Builder
	rc := run([]string{
		"run", "start",
		"--repo", "x/y", "--workflow", "w",
		"--working-dir", dir,
	}, io.Discard, &stderr)
	if rc != exitFailure {
		t.Fatalf("rc = %d, want exitFailure", rc)
	}
	if !strings.Contains(stderr.String(), "--spec-file") {
		t.Errorf("stderr should suggest --spec-file: %s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "--workflow-sha") {
		t.Errorf("stderr should suggest --workflow-sha: %s", stderr.String())
	}
}

// TestRunStart_MalformedSpecFailsFast surfaces YAML errors locally
// instead of round-tripping to the backend.
func TestRunStart_MalformedSpecFailsFast(t *testing.T) {
	_, srv := newFakeBackend(t)
	withBackend(t, srv)

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".fishhawk"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".fishhawk", "workflows.yaml"),
		[]byte("not: valid: yaml: ::\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var stderr strings.Builder
	rc := run([]string{
		"run", "start",
		"--repo", "x/y", "--workflow", "w",
		"--working-dir", dir,
	}, io.Discard, &stderr)
	if rc != exitFailure {
		t.Fatalf("rc = %d, want exitFailure", rc)
	}
	// The error message references the file path so the user
	// knows which file to fix.
	if !strings.Contains(stderr.String(), "workflows.yaml") {
		t.Errorf("stderr should reference the file path: %s", stderr.String())
	}
}
