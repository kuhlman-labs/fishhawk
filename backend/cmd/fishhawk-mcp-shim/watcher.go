package main

import "bytes"

// watcher polls the child binary path for a CONTENT change (sha-256), never
// mtime — a reload rebuild can be a byte-identical no-op that mtime would
// false-trigger, and the ADR ratifies the sha compare. It reports a change only
// when the file's hash both (a) differs from the running child's launch hash
// (the baseline) AND (b) has settled — the same new hash observed on two
// consecutive polls — so a half-written `go build -o` output (whose hash
// differs each poll until the write completes) never triggers a swap.
//
// The watcher is stepped explicitly (step) rather than owning a goroutine, so
// its state stays single-threaded inside the supervisor's event loop and tests
// drive it deterministically with no wall-clock sleeps.
type watcher struct {
	path     string
	baseline []byte
	pending  []byte

	// hashFile is injectable for tests; defaults to sha256File.
	hashFile func(string) ([]byte, error)
}

func newWatcher(path string) *watcher {
	return &watcher{path: path, hashFile: sha256File}
}

// step performs one poll. It returns (true, newHash) exactly when a settled
// content change is confirmed; the caller then swaps and calls setBaseline with
// newHash. Transient stat/read errors and a briefly-missing file are tolerated
// as "no change yet" — never a trigger and never a crash.
func (w *watcher) step() (bool, []byte) {
	h, err := w.hashFile(w.path)
	if err != nil {
		// A transient read error or a briefly-missing file resets any pending
		// candidate so a settle can never straddle an error.
		w.pending = nil
		return false, nil
	}
	if bytes.Equal(h, w.baseline) {
		w.pending = nil
		return false, nil
	}
	// h differs from the running child. Require it to repeat before triggering,
	// so a mid-write binary (unstable hash) is filtered out.
	if w.pending != nil && bytes.Equal(h, w.pending) {
		return true, h
	}
	w.pending = cloneBytes(h)
	return false, nil
}

// setBaseline records the hash of the newly-running child and clears any
// pending candidate. Called after a successful swap/respawn.
func (w *watcher) setBaseline(h []byte) {
	w.baseline = cloneBytes(h)
	w.pending = nil
}
