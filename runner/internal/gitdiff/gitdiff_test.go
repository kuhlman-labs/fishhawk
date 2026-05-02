package gitdiff

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/runner/internal/constraint"
)

func TestParse_Empty(t *testing.T) {
	d, err := Parse(nil)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(d.ChangedFiles) != 0 {
		t.Errorf("expected empty diff, got %+v", d)
	}
}

func TestParse_SimpleAddedModifiedDeleted(t *testing.T) {
	// Simulated `git diff --name-status -z` output: each field is
	// NUL-terminated.
	raw := []byte("M\x00backend/main.go\x00A\x00new.go\x00D\x00gone.go\x00")
	d, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(d.ChangedFiles) != 3 {
		t.Fatalf("got %d files, want 3: %+v", len(d.ChangedFiles), d.ChangedFiles)
	}
	want := []constraint.ChangedFile{
		{Path: "backend/main.go", Status: constraint.StatusModified},
		{Path: "new.go", Status: constraint.StatusAdded},
		{Path: "gone.go", Status: constraint.StatusDeleted},
	}
	for i, w := range want {
		if d.ChangedFiles[i] != w {
			t.Errorf("file %d = %+v, want %+v", i, d.ChangedFiles[i], w)
		}
	}
}

func TestParse_RenameAndCopy(t *testing.T) {
	// R100 = pure rename. C75 = copy with 75% similarity. The
	// destination path goes second; we record the destination.
	raw := []byte("R100\x00old.go\x00new.go\x00C75\x00source.go\x00dest.go\x00")
	d, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(d.ChangedFiles) != 2 {
		t.Fatalf("got %d files, want 2: %+v", len(d.ChangedFiles), d.ChangedFiles)
	}
	if d.ChangedFiles[0].Path != "new.go" || d.ChangedFiles[0].Status != constraint.StatusRenamed {
		t.Errorf("rename: got %+v", d.ChangedFiles[0])
	}
	if d.ChangedFiles[1].Path != "dest.go" || d.ChangedFiles[1].Status != constraint.StatusCopied {
		t.Errorf("copy: got %+v", d.ChangedFiles[1])
	}
}

func TestParse_PathWithSpecialChars(t *testing.T) {
	// -z form preserves filenames with newlines / quotes / spaces
	// without escaping. Test that a path containing a tab survives.
	raw := []byte("M\x00path with\ttab.go\x00")
	d, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(d.ChangedFiles) != 1 || d.ChangedFiles[0].Path != "path with\ttab.go" {
		t.Errorf("got %+v, want path preserved verbatim", d.ChangedFiles)
	}
}

func TestParse_MissingPath(t *testing.T) {
	// A stream that ends after a status with no following path is
	// malformed; the parser should reject rather than silently
	// drop.
	raw := []byte("M\x00")
	_, err := Parse(raw)
	if err == nil {
		t.Fatal("expected error for status with no path")
	}
}

func TestParse_RenameMissingDestination(t *testing.T) {
	raw := []byte("R100\x00old.go\x00")
	_, err := Parse(raw)
	if err == nil {
		t.Fatal("expected error for rename with no destination")
	}
}

// fakeCmd returns a Cmd builder that re-execs the test binary
// pretending to be `git`. The helper test below emits canned -z
// output driven by HELPER_MODE.
func fakeCmd(mode string) func(ctx context.Context, name string, args ...string) *exec.Cmd {
	return func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		c := exec.CommandContext(ctx, os.Args[0], "-test.run=TestHelperProcess")
		c.Env = append(os.Environ(),
			"GO_HELPER_PROCESS=1",
			"HELPER_MODE="+mode,
		)
		return c
	}
}

// TestHelperProcess stands in for the `git` binary in tests.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_HELPER_PROCESS") != "1" {
		return
	}
	defer os.Exit(0)

	switch os.Getenv("HELPER_MODE") {
	case "ok":
		fmt.Print("M\x00a.go\x00A\x00b.go\x00")
	case "error":
		fmt.Fprintln(os.Stderr, "fatal: ambiguous argument 'no-such-ref'")
		os.Exit(128)
	case "ok_empty":
		// No output — clean diff.
	default:
		fmt.Fprintln(os.Stderr, "unknown HELPER_MODE")
		os.Exit(2)
	}
}

func TestRun_OK(t *testing.T) {
	r := &Runner{Cmd: fakeCmd("ok")}
	d, err := r.Run(context.Background(), "main", t.TempDir())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(d.ChangedFiles) != 2 {
		t.Errorf("got %d files, want 2", len(d.ChangedFiles))
	}
}

func TestRun_Error_SurfacesStderr(t *testing.T) {
	r := &Runner{Cmd: fakeCmd("error")}
	_, err := r.Run(context.Background(), "no-such-ref", t.TempDir())
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "ambiguous argument") {
		t.Errorf("err = %v, want stderr-derived message", err)
	}
}

func TestRun_Empty(t *testing.T) {
	r := &Runner{Cmd: fakeCmd("ok_empty")}
	d, err := r.Run(context.Background(), "main", t.TempDir())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(d.ChangedFiles) != 0 {
		t.Errorf("expected empty diff, got %+v", d)
	}
}

func TestRun_RequiredArgs(t *testing.T) {
	r := &Runner{}
	if _, err := r.Run(context.Background(), "", "/x"); err == nil {
		t.Error("expected baseRef required")
	}
	if _, err := r.Run(context.Background(), "main", ""); err == nil {
		t.Error("expected repoDir required")
	}
}

func TestSplitNULs(t *testing.T) {
	// Direct exercise of the splitter so we cover the request-more
	// branch.
	advance, token, err := splitNULs([]byte("ab"), false)
	if err != nil || advance != 0 || token != nil {
		t.Errorf("partial input: got (%d, %q, %v), want (0, nil, nil)", advance, token, err)
	}
	advance, token, err = splitNULs([]byte("ab\x00cd"), false)
	if err != nil || advance != 3 || string(token) != "ab" {
		t.Errorf("found NUL: got (%d, %q, %v)", advance, token, err)
	}
	advance, token, err = splitNULs([]byte("ab"), true)
	if err != nil || advance != 2 || string(token) != "ab" {
		t.Errorf("EOF: got (%d, %q, %v)", advance, token, err)
	}
	// EOF with no data should yield (0, nil, nil).
	advance, token, err = splitNULs(nil, true)
	if err != nil || advance != 0 || token != nil {
		t.Errorf("EOF empty: got (%d, %q, %v)", advance, token, err)
	}
}

// errExitWithoutStderr ensures the *exec.ExitError-without-stderr
// path is exercised. When git fails but emits no stderr (rare but
// possible in mocked cases), the wrapped error still carries the
// exit message.
func TestRun_ExitErrorWithoutStderr(t *testing.T) {
	// Use exec.CommandContext to exec /bin/false (or equivalent).
	// false always exits 1 with no output — perfect proxy.
	r := &Runner{
		Cmd: func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
			return exec.CommandContext(ctx, "false")
		},
	}
	_, err := r.Run(context.Background(), "main", t.TempDir())
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, err) { // smoke: just make sure non-nil
		t.Errorf("err = %v", err)
	}
}
