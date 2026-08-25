package docgen

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// resolveRepoRoot walks up from start looking for go.work (the workspace
// root). It returns an error rather than skipping so the caller can FAIL
// LOUDLY — an unresolved root must never silently green a drift gate.
func resolveRepoRoot(start string) (string, error) {
	dir := start
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.work")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", &rootError{start: start}
		}
		dir = parent
	}
}

type rootError struct{ start string }

func (e *rootError) Error() string {
	return "docgen: could not resolve repo root (no go.work above " + e.start + ")"
}

// testRepoRoot resolves the repository root for a test and FAILS LOUDLY
// (t.Fatalf, never t.Skip) when it cannot — the drift gate is unenforced
// if it silently skips, so an unresolved root is a hard failure.
func testRepoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	root, err := resolveRepoRoot(wd)
	if err != nil {
		t.Fatalf("drift gate cannot run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "site")); err != nil {
		t.Fatalf("drift gate cannot run: site/ absent under repo root %q: %v", root, err)
	}
	return root
}

// TestResolveRepoRootFailsClosed pins the fail-closed contract of the
// resolver directly: pointed at a directory with no go.work above it, it
// returns an error — it does NOT return a bogus root that a caller might
// treat as success. This is the logic behind testRepoRoot's t.Fatalf.
func TestResolveRepoRootFailsClosed(t *testing.T) {
	// A temp dir under the OS temp root has no go.work anywhere above it
	// within the temp tree; verify the walk terminates in an error.
	tmp := t.TempDir()
	if _, err := resolveRepoRoot(tmp); err == nil {
		// If the OS temp dir happens to sit under a go.work (unusual),
		// this would misfire; guard by asserting the message shape only
		// when an error is returned.
		t.Fatalf("resolveRepoRoot(%q) = nil error, want fail-closed error", tmp)
	}
}

// TestSiteReferenceMatchesCommittedTree is the end-to-end drift gate: it
// regenerates each page in memory from the canonical sources and byte-
// compares against the committed site page, crossing schema/OpenAPI
// parsing, rendering, marker splicing and the committed tree. A hand-edit
// to a generated region, or a canonical source change without a
// regenerate, reddens it.
func TestSiteReferenceMatchesCommittedTree(t *testing.T) {
	root := testRepoRoot(t)
	for _, id := range PageIDs {
		rel, _ := PageFile(id)
		want, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("%s: read committed page: %v", id, err)
		}
		got, err := RegenerateFile(root, id)
		if err != nil {
			t.Fatalf("%s: regenerate: %v", id, err)
		}
		if got != string(want) {
			t.Errorf("%s is out of date; run scripts/gen-site-reference", rel)
		}
	}
}

// TestReferencePagesReachableFromIndex asserts every reference page is
// linked from the reference index — the navigability half of "published
// and navigable" a byte-compare cannot see.
func TestReferencePagesReachableFromIndex(t *testing.T) {
	root := testRepoRoot(t)
	refDir := filepath.Join(root, "site", "src", "content", "docs", "reference")
	idxBytes, err := os.ReadFile(filepath.Join(refDir, "index.md"))
	if err != nil {
		t.Fatalf("read reference index: %v", err)
	}
	idx := string(idxBytes)
	entries, err := os.ReadDir(refDir)
	if err != nil {
		t.Fatalf("read reference dir: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".md") || name == "index.md" {
			continue
		}
		slug := strings.TrimSuffix(name, ".md")
		link := "/fishhawk/reference/" + slug + "/"
		if !strings.Contains(idx, link) {
			t.Errorf("reference index does not link %q (expected %q)", name, link)
		}
	}
}

// TestInternalLinksResolve asserts every internal (/fishhawk/...) link in
// each generated reference page resolves to a real content file. A full
// Astro build is out of reach in the sandbox, so link resolution against
// the content tree is the criterion.
func TestInternalLinksResolve(t *testing.T) {
	root := testRepoRoot(t)
	for _, id := range PageIDs {
		gen, err := GeneratePageContent(root, id)
		if err != nil {
			t.Fatalf("%s: generate: %v", id, err)
		}
		for _, href := range InternalLinks(gen) {
			if !ResolveSiteLink(root, href) {
				t.Errorf("%s: internal link %q does not resolve to a content file", id, href)
			}
		}
	}
}

// TestEachReferencePageLinksOperatorGuide asserts each generated
// reference page contains at least one link to the operator guide (the
// checkable half of "cross-links, not restatement"). The not-restatement
// half is a review judgement, not a machine check.
func TestEachReferencePageLinksOperatorGuide(t *testing.T) {
	root := testRepoRoot(t)
	for _, id := range PageIDs {
		gen, err := GeneratePageContent(root, id)
		if err != nil {
			t.Fatalf("%s: generate: %v", id, err)
		}
		if !strings.Contains(gen, "("+OperatorGuideLink+")") {
			t.Errorf("%s generated region does not link the operator guide %q", id, OperatorGuideLink)
		}
	}
}
