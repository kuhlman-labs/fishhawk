// Package docgen generates the human-readable Reference pages of the
// documentation site from the canonical sources — the workflow-spec and
// plan JSON Schemas under docs/spec/, the OpenAPI document under
// docs/api/, and the cli/internal/cmdinfo inventory — and splices each
// rendering into its page between HTML marker comments. Orientation
// prose OUTSIDE the markers stays hand-editable; the region BETWEEN them
// is byte-exact and drift-gated (TestSiteReferenceMatchesCommittedTree).
//
// Three of the four generated regions are rendered from the canonical
// schema and OpenAPI documents. The fourth — the CLI region — is
// rendered from the cmdinfo inventory, which is itself bound to the
// executable surface by the package-main TestCLIFlagsMatchExecutableSurface
// asserting both directions against the live flag sets; that binding is
// what makes the CLI region non-transcribed in the sense that matters.
package docgen

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// OperatorGuideLink is the site path of the day-two operator guide every
// generated reference page cross-links (the checkable half of
// "cross-links, not restatement" — the not-restatement half is a review
// judgement, not a machine check).
const OperatorGuideLink = "/fishhawk/operating/driving-a-run/"

// PageIDs are the four generated reference pages.
var PageIDs = []string{"workflow-spec", "plan-schema", "cli", "api"}

// pageFiles maps a page id to its repo-relative site path.
var pageFiles = map[string]string{
	"workflow-spec": "site/src/content/docs/reference/workflow-spec.md",
	"plan-schema":   "site/src/content/docs/reference/plan-schema.md",
	"cli":           "site/src/content/docs/reference/cli.md",
	"api":           "site/src/content/docs/reference/api.md",
}

// PageFile returns the repo-relative path of a page's markdown file.
func PageFile(id string) (string, bool) { p, ok := pageFiles[id]; return p, ok }

// -------------------------------------------------------------------------
// Markdown-table escaping. Repository contents are untrusted input, so the
// per-class behavior below is pinned by a table test (pipe, bare LF, bare
// CR, CRLF, a backtick run forcing fence widening, leading/trailing
// whitespace).
// -------------------------------------------------------------------------

// collapseWS replaces CRLF, CR and LF with a single space and trims the
// result, so a multi-line description never breaks a table row.
func collapseWS(s string) string {
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.TrimSpace(s)
}

// escapePipes escapes the table-cell delimiter.
func escapePipes(s string) string { return strings.ReplaceAll(s, "|", "\\|") }

// proseCell renders arbitrary text as a table cell: newlines collapsed,
// whitespace trimmed, pipes escaped.
func proseCell(s string) string { return escapePipes(collapseWS(s)) }

// codeSpan renders s as an inline code span, widening the backtick fence
// past the longest internal run and padding when s begins or ends with a
// backtick or space (the CommonMark rule). Newlines are collapsed first.
func codeSpan(s string) string {
	s = collapseWS(s)
	longest, cur := 0, 0
	for _, r := range s {
		if r == '`' {
			cur++
			if cur > longest {
				longest = cur
			}
		} else {
			cur = 0
		}
	}
	fence := strings.Repeat("`", longest+1)
	pad := ""
	if strings.HasPrefix(s, "`") || strings.HasSuffix(s, "`") ||
		strings.HasPrefix(s, " ") || strings.HasSuffix(s, " ") {
		pad = " "
	}
	return fence + pad + s + pad + fence
}

// -------------------------------------------------------------------------
// Marker splice. Fail-closed on every malformed marker configuration.
// -------------------------------------------------------------------------

func beginMarker(id string) string { return fmt.Sprintf("<!-- BEGIN GENERATED %s -->", id) }
func endMarker(id string) string   { return fmt.Sprintf("<!-- END GENERATED %s -->", id) }

// Splice replaces the region between id's marker pair in existing with
// generated, leaving the marker lines and all surrounding prose intact.
// It FAILS CLOSED on a missing, duplicated or inverted marker pair rather
// than guessing where the generated region belongs.
func Splice(existing, generated, id string) (string, error) {
	begin, end := beginMarker(id), endMarker(id)
	switch strings.Count(existing, begin) {
	case 1:
	case 0:
		return "", fmt.Errorf("docgen: %s: missing opening marker %q", id, begin)
	default:
		return "", fmt.Errorf("docgen: %s: duplicated opening marker %q", id, begin)
	}
	switch strings.Count(existing, end) {
	case 1:
	case 0:
		return "", fmt.Errorf("docgen: %s: missing closing marker %q", id, end)
	default:
		return "", fmt.Errorf("docgen: %s: duplicated closing marker %q", id, end)
	}
	bi := strings.Index(existing, begin)
	ei := strings.Index(existing, end)
	if bi > ei {
		return "", fmt.Errorf("docgen: %s: closing marker precedes opening marker", id)
	}
	before := existing[:bi+len(begin)]
	after := existing[ei:]
	return before + "\n\n" + strings.TrimRight(generated, "\n") + "\n\n" + after, nil
}

// crossLinkBlock is the operator-guide cross-link every generated region
// carries. The link target is asserted by TestEachReferencePageLinksOperatorGuide;
// it resolves to a real content file per TestInternalLinksResolve.
func crossLinkBlock() string {
	return "> **See also:** [Driving a run](" + OperatorGuideLink +
		") — the operator loop these reference surfaces serve. " +
		"This page is the field reference, not a restatement of that guide.\n"
}

// genPreamble is the fixed note atop every generated region.
func genPreamble() string {
	return "_Generated from the canonical sources by `scripts/gen-site-reference`; " +
		"do not edit between the markers. Description-only edits to a source " +
		"are not diffed — the delta tables below compare shape (type, " +
		"requiredness, enum members, default)._\n"
}

// GeneratePageContent renders the generated region (between the markers)
// for a page, reading the canonical sources under root.
func GeneratePageContent(root, id string) (string, error) {
	switch id {
	case "workflow-spec":
		return generateWorkflowSpecPage(root)
	case "plan-schema":
		return generatePlanSchemaPage(root)
	case "cli":
		return generateCLIPage()
	case "api":
		return generateAPIPage(root)
	default:
		return "", fmt.Errorf("docgen: unknown page id %q", id)
	}
}

// RegenerateFile reads a page's committed file, splices the freshly
// generated region into it, and returns the new file content.
func RegenerateFile(root, id string) (string, error) {
	rel, ok := pageFiles[id]
	if !ok {
		return "", fmt.Errorf("docgen: unknown page id %q", id)
	}
	existing, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		return "", err
	}
	gen, err := GeneratePageContent(root, id)
	if err != nil {
		return "", err
	}
	return Splice(string(existing), gen, id)
}

// WriteFile regenerates a page and writes it back to disk. Returns
// whether the file content changed.
func WriteFile(root, id string) (changed bool, err error) {
	rel := pageFiles[id]
	path := filepath.Join(root, rel)
	old, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	next, err := RegenerateFile(root, id)
	if err != nil {
		return false, err
	}
	if string(old) == next {
		return false, nil
	}
	if err := os.WriteFile(path, []byte(next), 0o644); err != nil { //nolint:gosec // docs file, 0644 is intended
		return false, err
	}
	return true, nil
}

// -------------------------------------------------------------------------
// Link / navigability checks (machine verification of publication intent —
// a full Astro build is out of reach in the sandbox, so link resolution
// against the content tree is the criterion).
// -------------------------------------------------------------------------

var internalLinkRE = regexp.MustCompile(`\]\((/fishhawk/[^)]+)\)`)

// InternalLinks returns the /fishhawk/... link targets in markdown.
func InternalLinks(markdown string) []string {
	var out []string
	for _, m := range internalLinkRE.FindAllStringSubmatch(markdown, -1) {
		out = append(out, m[1])
	}
	return out
}

// ResolveSiteLink reports whether a /fishhawk/<path>/ link resolves to a
// real content file under site/src/content/docs. It accounts for the
// trailing slash and for a directory index (index.md / index.mdx).
func ResolveSiteLink(root, href string) bool {
	p := strings.TrimPrefix(href, "/fishhawk/")
	if i := strings.IndexAny(p, "#?"); i >= 0 {
		p = p[:i]
	}
	p = strings.Trim(p, "/")
	base := filepath.Join(root, "site", "src", "content", "docs")
	if p == "" {
		p = "index"
	}
	candidates := []string{
		filepath.Join(base, p+".md"),
		filepath.Join(base, p+".mdx"),
		filepath.Join(base, p, "index.md"),
		filepath.Join(base, p, "index.mdx"),
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return true
		}
	}
	return false
}
