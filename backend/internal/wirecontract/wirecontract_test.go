package wirecontract

import (
	"path/filepath"
	"testing"
)

// TestCrossModuleWireParity is the repo-level gate: it resolves the repo root,
// runs Check over the REAL seed manifest against the REAL committed backend and
// runner sources, and fails on any returned error. This is the only assertion
// in the repo that reads both modules' declarations in one process, so it is
// the seam #2558 names.
//
// It REQUIRES the full repo tree (RepoRoot walks to go.work; Check reads a
// sibling module's source) and FAILS CLOSED if it cannot — see the package doc.
func TestCrossModuleWireParity(t *testing.T) {
	root, err := RepoRoot()
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	if errs := Check(root, SeedManifest()); len(errs) > 0 {
		for _, e := range errs {
			t.Errorf("cross-module wire parity violation: %v", e)
		}
	}
}

// TestManifestCompleteness asserts the completeness sweep over the REAL
// CoveredFiles returns no uncovered marker-bearing declaration — a new
// duplicated contract that copies the marker but skips the manifest fails here.
func TestManifestCompleteness(t *testing.T) {
	root, err := RepoRoot()
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	m := SeedManifest()
	if errs := checkCompleteness(root, m); len(errs) > 0 {
		for _, e := range errs {
			t.Errorf("manifest completeness violation: %v", e)
		}
	}
}

// TestManifestNonVacuity proves the gate cannot silently cover nothing: the
// manifest must be non-empty and EVERY pair endpoint must RESOLVE to a real
// struct in the real tree. A manifest whose endpoints all fail to resolve would
// make TestCrossModuleWireParity report success having compared nothing.
func TestManifestNonVacuity(t *testing.T) {
	root, err := RepoRoot()
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	m := SeedManifest()
	if len(m.Pairs) == 0 {
		t.Fatal("seed manifest has no pairs — the gate would cover nothing")
	}
	for _, p := range m.Pairs {
		for _, ep := range []Endpoint{p.Emitter, p.Consumer} {
			path := filepath.Join(root, filepath.FromSlash(ep.File))
			fields, err := ExtractStruct(path, ep.Type)
			if err != nil {
				t.Errorf("pair %q endpoint %s.%s does not resolve: %v", p.Name, ep.File, ep.Type, err)
				continue
			}
			if len(fields) == 0 {
				t.Errorf("pair %q endpoint %s.%s resolved to zero wire fields", p.Name, ep.File, ep.Type)
			}
		}
	}
}
