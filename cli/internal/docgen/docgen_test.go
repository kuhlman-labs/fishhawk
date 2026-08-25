package docgen

import (
	"strings"
	"testing"
)

// --- Marker splice: happy path + one test per fail-closed mode ---

func TestSplice_ReplacesRegion(t *testing.T) {
	existing := "intro\n<!-- BEGIN GENERATED cli -->\nOLD\n<!-- END GENERATED cli -->\noutro\n"
	got, err := Splice(existing, "NEW", "cli")
	if err != nil {
		t.Fatalf("Splice: %v", err)
	}
	if !strings.Contains(got, "NEW") || strings.Contains(got, "OLD") {
		t.Errorf("region not replaced:\n%s", got)
	}
	if !strings.HasPrefix(got, "intro\n<!-- BEGIN GENERATED cli -->") {
		t.Errorf("prose before marker not preserved:\n%s", got)
	}
	if !strings.HasSuffix(got, "<!-- END GENERATED cli -->\noutro\n") {
		t.Errorf("prose after marker not preserved:\n%s", got)
	}
	// Idempotent.
	again, err := Splice(got, "NEW", "cli")
	if err != nil {
		t.Fatalf("second Splice: %v", err)
	}
	if again != got {
		t.Errorf("Splice is not idempotent")
	}
}

func TestSplice_MissingOpeningMarker(t *testing.T) {
	existing := "intro\n<!-- END GENERATED cli -->\noutro\n"
	if _, err := Splice(existing, "NEW", "cli"); err == nil ||
		!strings.Contains(err.Error(), "missing opening marker") {
		t.Fatalf("want missing-opening-marker error, got %v", err)
	}
}

func TestSplice_MissingClosingMarker(t *testing.T) {
	existing := "intro\n<!-- BEGIN GENERATED cli -->\nbody\n"
	if _, err := Splice(existing, "NEW", "cli"); err == nil ||
		!strings.Contains(err.Error(), "missing closing marker") {
		t.Fatalf("want missing-closing-marker error, got %v", err)
	}
}

func TestSplice_DuplicatedMarker(t *testing.T) {
	existing := "<!-- BEGIN GENERATED cli -->\na\n<!-- BEGIN GENERATED cli -->\nb\n<!-- END GENERATED cli -->\n"
	if _, err := Splice(existing, "NEW", "cli"); err == nil ||
		!strings.Contains(err.Error(), "duplicated opening marker") {
		t.Fatalf("want duplicated-marker error, got %v", err)
	}
}

func TestSplice_InvertedMarkers(t *testing.T) {
	existing := "<!-- END GENERATED cli -->\nmiddle\n<!-- BEGIN GENERATED cli -->\n"
	if _, err := Splice(existing, "NEW", "cli"); err == nil ||
		!strings.Contains(err.Error(), "closing marker precedes opening marker") {
		t.Fatalf("want inverted-marker error, got %v", err)
	}
}

// --- Per-class table escaping ---

func TestProseCellEscaping(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"pipe", "a|b", `a\|b`},
		{"bare-LF", "a\nb", "a b"},
		{"bare-CR", "a\rb", "a b"},
		{"CRLF", "a\r\nb", "a b"},
		{"leading-trailing-ws", "  hi  ", "hi"},
		{"pipe-and-newline", "a|b\nc", `a\|b c`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := proseCell(c.in); got != c.want {
				t.Errorf("proseCell(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestCodeSpanFenceWidening(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"no-backtick", "plain", "`plain`"},
		{"one-backtick-run", "a`b", "``a`b``"},
		{"two-backtick-run", "x``y", "```x``y```"},
		{"leading-backtick-pads", "`x", "`` `x ``"},
		{"leading-space-trimmed", " x", "`x`"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := codeSpan(c.in); got != c.want {
				t.Errorf("codeSpan(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// --- Link resolution ---

func TestResolveSiteLink(t *testing.T) {
	root := testRepoRoot(t)
	// The operator guide must resolve; a fabricated path must not.
	if !ResolveSiteLink(root, OperatorGuideLink) {
		t.Errorf("operator guide link %q should resolve", OperatorGuideLink)
	}
	if !ResolveSiteLink(root, "/fishhawk/reference/") {
		t.Errorf("reference index link should resolve")
	}
	if ResolveSiteLink(root, "/fishhawk/nope/does-not-exist/") {
		t.Errorf("nonexistent link should not resolve")
	}
}

func TestInternalLinksExtraction(t *testing.T) {
	md := "see [a](/fishhawk/x/) and [b](/fishhawk/y/z/) and [ext](https://example.com)"
	got := InternalLinks(md)
	if len(got) != 2 || got[0] != "/fishhawk/x/" || got[1] != "/fishhawk/y/z/" {
		t.Errorf("InternalLinks = %v", got)
	}
}

// TestWriteFileUnchangedOnCleanTree drives WriteFile against the committed
// tree: the pages are current, so every page reports unchanged and nothing
// is written. Exercises the read-compare-early-return path without mutating
// the tree.
func TestWriteFileUnchangedOnCleanTree(t *testing.T) {
	root := testRepoRoot(t)
	for _, id := range PageIDs {
		changed, err := WriteFile(root, id)
		if err != nil {
			t.Fatalf("%s: WriteFile: %v", id, err)
		}
		if changed {
			t.Errorf("%s: committed page reported changed; the tree may be stale (run scripts/gen-site-reference)", id)
		}
	}
}

// TestUnknownPageIDFailsClosed covers the unknown-id error branches.
func TestUnknownPageIDFailsClosed(t *testing.T) {
	if _, err := GeneratePageContent(".", "nope"); err == nil {
		t.Error("GeneratePageContent: want error for unknown id")
	}
	if _, err := RegenerateFile(".", "nope"); err == nil {
		t.Error("RegenerateFile: want error for unknown id")
	}
	if _, ok := PageFile("nope"); ok {
		t.Error("PageFile: want !ok for unknown id")
	}
}

// TestRootMappingRejectsMalformed covers the schema-parse error paths.
func TestRootMappingRejectsMalformed(t *testing.T) {
	if _, err := rootMapping([]byte("::: not yaml :::\n\t- broken")); err == nil {
		t.Error("rootMapping: want error on malformed input")
	}
	if _, err := rootMapping([]byte("- a\n- b\n")); err == nil {
		t.Error("rootMapping: want error when root is not a mapping")
	}
	if _, err := AccountableNodes([]byte("- a\n")); err == nil {
		t.Error("AccountableNodes: want error on non-mapping root")
	}
	if ok, _ := PointerResolves([]byte(`{"properties":{"a":{}}}`), "/properties/missing"); ok {
		t.Error("PointerResolves: want false for an absent pointer")
	}
}
