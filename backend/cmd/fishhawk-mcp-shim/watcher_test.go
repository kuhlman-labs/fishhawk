package main

import (
	"os"
	"path/filepath"
	"testing"
)

// writeFile writes content to path with 0o644.
func writeFile(t *testing.T, path string, content []byte) {
	t.Helper()
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// TestWatcherContentChangeSettlesThenTriggers pins the core contract: a real
// content change triggers only after it has settled across two consecutive
// polls.
func TestWatcherContentChangeSettlesThenTriggers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "child")
	writeFile(t, path, []byte("v1"))

	w := newWatcher(path)
	base, err := sha256File(path)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	w.setBaseline(base)

	// Baseline unchanged → no trigger.
	if changed, _ := w.step(); changed {
		t.Fatal("unchanged file must not trigger")
	}

	// New content, first observation → not yet settled.
	writeFile(t, path, []byte("v2-longer-content"))
	if changed, _ := w.step(); changed {
		t.Fatal("first observation of new content must not trigger (unsettled)")
	}
	// Same new content on the next poll → settled → trigger.
	changed, h := w.step()
	if !changed {
		t.Fatal("settled new content must trigger")
	}
	want, _ := sha256File(path)
	if string(h) != string(want) {
		t.Fatal("triggered hash must match the new file content")
	}
}

// TestWatcherIdenticalBytesNeverTriggers pins that an mtime-only touch with
// identical bytes never triggers — the reason the watcher hashes content rather
// than comparing mtime.
func TestWatcherIdenticalBytesNeverTriggers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "child")
	writeFile(t, path, []byte("same"))

	w := newWatcher(path)
	base, _ := sha256File(path)
	w.setBaseline(base)

	// Rewrite the SAME bytes several times (a byte-identical no-op rebuild).
	for i := 0; i < 3; i++ {
		writeFile(t, path, []byte("same"))
		if changed, _ := w.step(); changed {
			t.Fatal("byte-identical rewrite must never trigger")
		}
	}
}

// TestWatcherHalfWrittenFileNeverTriggersUntilStable pins the settle debounce:
// a hash that differs on every poll (a mid-`go build -o` write) never triggers
// until it stabilizes.
func TestWatcherHalfWrittenFileNeverTriggersUntilStable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "child")
	writeFile(t, path, []byte("v1"))

	w := newWatcher(path)
	base, _ := sha256File(path)
	w.setBaseline(base)

	// Three polls, three different contents → never settled → never triggers.
	for _, c := range []string{"half-a", "half-ab", "half-abc"} {
		writeFile(t, path, []byte(c))
		if changed, _ := w.step(); changed {
			t.Fatalf("unstable content %q must not trigger", c)
		}
	}
	// Now it stabilizes: same content twice → triggers on the second.
	writeFile(t, path, []byte("final"))
	if changed, _ := w.step(); changed {
		t.Fatal("first observation of stabilized content must not trigger")
	}
	if changed, _ := w.step(); !changed {
		t.Fatal("stabilized content must trigger once settled")
	}
}

// TestWatcherTransientMissingFileTolerated pins that a briefly-missing or
// unreadable file is treated as "no change yet", never a trigger or a crash,
// and does not corrupt a pending settle.
func TestWatcherTransientMissingFileTolerated(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "child")
	writeFile(t, path, []byte("v1"))

	w := newWatcher(path)
	base, _ := sha256File(path)
	w.setBaseline(base)

	// New content observed once (pending), then the file vanishes.
	writeFile(t, path, []byte("v2"))
	if changed, _ := w.step(); changed {
		t.Fatal("first observation must not trigger")
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if changed, _ := w.step(); changed {
		t.Fatal("missing file must not trigger")
	}
	// The missing-file poll cleared the pending candidate: the same new content
	// must settle again (two fresh consecutive polls) before triggering.
	writeFile(t, path, []byte("v2"))
	if changed, _ := w.step(); changed {
		t.Fatal("missing file must reset the settle; first re-observation must not trigger")
	}
	if changed, _ := w.step(); !changed {
		t.Fatal("re-settled content must trigger")
	}
}
