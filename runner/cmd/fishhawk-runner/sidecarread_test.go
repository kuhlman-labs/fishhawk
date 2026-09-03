package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// --- readSidecarBounded (E64.12 / #3106) ---------------------------------

// TestSidecarCeilingValue pins the SHIPPED ceiling. maxSidecarBytes is a const,
// never a test-injectable var, so the per-loader tests exercise the real number;
// but a constant's value is not structurally enforced by compilation, so a
// silent retune to a useless ceiling (0, or a gigabyte) would otherwise pass
// every other test. This is the done-means pin on the value itself.
func TestSidecarCeilingValue(t *testing.T) {
	if maxSidecarBytes != 1<<20 {
		t.Fatalf("maxSidecarBytes = %d, want %d (1 MiB)", maxSidecarBytes, int64(1<<20))
	}
}

// TestReadSidecarBounded_Absent: an absent path returns an error satisfying
// errors.Is(err, os.ErrNotExist) — the identity every caller's absent-sidecar
// no-op branches on. A wrapping regression (fmt.Errorf without %w on the open
// error) reddens here.
func TestReadSidecarBounded_Absent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nope.json")
	got, err := readSidecarBounded(path)
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("err = %v, want an os.ErrNotExist-satisfying error", err)
	}
	if got != nil {
		t.Errorf("bytes = %q, want nil on an absent path", got)
	}
}

// TestReadSidecarBounded_DirectoryIsNotNotExist: a directory at the path yields
// a NON-ErrNotExist error (EISDIR from the read), pinning that the present-but-
// unreadable discrimination survives the os.ReadFile -> os.Open+ReadAll swap.
// A directory fails deterministically for every user INCLUDING root, unlike a
// chmod-based fixture.
func TestReadSidecarBounded_DirectoryIsNotNotExist(t *testing.T) {
	dir := t.TempDir()
	got, err := readSidecarBounded(dir)
	if err == nil {
		t.Fatal("expected a read error for a directory path")
	}
	if errors.Is(err, os.ErrNotExist) {
		t.Errorf("a directory must NOT report ErrNotExist, got %v", err)
	}
	if errors.Is(err, errSidecarTooLarge) {
		t.Errorf("a directory must NOT report errSidecarTooLarge, got %v", err)
	}
	if got != nil {
		t.Errorf("bytes = %q, want nil on an unreadable path", got)
	}
}

// TestReadSidecarBounded_DanglingSymlinkIsNotExist: os.Open FOLLOWS symlinks
// exactly as os.ReadFile did, so a dangling symlink reports ENOENT for its
// missing target — which is what keeps loadCounterfactualReport's os.Lstat
// re-check load-bearing (it distinguishes actual absence from a present-but-
// broken link) rather than dead code after this swap.
func TestReadSidecarBounded_DanglingSymlinkIsNotExist(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(filepath.Join(dir, "missing-target.json"), link); err != nil {
		t.Skipf("symlink unsupported on this platform: %v", err)
	}
	got, err := readSidecarBounded(link)
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dangling symlink err = %v, want os.ErrNotExist (os.Open follows the link)", err)
	}
	if got != nil {
		t.Errorf("bytes = %q, want nil", got)
	}
}

// TestReadSidecarBounded_ExactlyAtCeiling: a file of exactly maxSidecarBytes is
// accepted and its full bytes returned. Reading limit+1 and comparing against
// limit is what makes the boundary inclusive.
func TestReadSidecarBounded_ExactlyAtCeiling(t *testing.T) {
	path := filepath.Join(t.TempDir(), "at.json")
	content := bytes.Repeat([]byte("a"), int(maxSidecarBytes))
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readSidecarBounded(path)
	if err != nil {
		t.Fatalf("exactly-at-ceiling must be accepted, got err %v", err)
	}
	if int64(len(got)) != maxSidecarBytes {
		t.Errorf("len = %d, want %d (full bytes)", len(got), maxSidecarBytes)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("bytes must round-trip exactly at the ceiling")
	}
}

// TestReadSidecarBounded_OneOverCeiling: a file of maxSidecarBytes+1 returns NIL
// bytes and an error satisfying errors.Is(err, errSidecarTooLarge). The nil
// assertion is the one that enforces "never a partial decode": a regression that
// returned the truncated maxSidecarBytes-length prefix would redden here.
func TestReadSidecarBounded_OneOverCeiling(t *testing.T) {
	path := filepath.Join(t.TempDir(), "over.json")
	content := bytes.Repeat([]byte("a"), int(maxSidecarBytes)+1)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readSidecarBounded(path)
	if !errors.Is(err, errSidecarTooLarge) {
		t.Fatalf("one-over-ceiling err = %v, want errSidecarTooLarge", err)
	}
	if got != nil {
		t.Errorf("bytes = %d bytes, want NIL (never a partial prefix)", len(got))
	}
}

// TestReadSidecarBounded_SmallFileRoundTrip: an ordinary small file round-trips
// byte-exactly.
func TestReadSidecarBounded_SmallFileRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "small.json")
	const content = `{"run_id":"r","stage_id":"s"}`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readSidecarBounded(path)
	if err != nil {
		t.Fatalf("small file must be accepted, got err %v", err)
	}
	if string(got) != content {
		t.Errorf("bytes = %q, want %q", got, content)
	}
}
