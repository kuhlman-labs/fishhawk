package policy

import (
	"sort"
	"strings"
)

// MIRROR (#2660): runner/internal/constraint/commentonly.go is a
// logic-identical port of this file — the same duplication IsGeneratedPath
// and isTestPath already live under, because that module deliberately
// carries no dependency on this package. CHANGE THEM TOGETHER; a
// divergence makes the runner fail a stage category-B that this
// (authoritative) evaluator would admit, or vice versa.

// CommentOnlySignal is the verdict of DetectCommentOnlyGo: whether the
// stage's emitted unified diff proves the NARROW SYNTACTIC PROPERTY that
// every changed testable-source file is a Go file whose emitted changed
// lines are blank or ordinary `//` line comments.
//
// Reason is drawn from a CLOSED set of fixed codes (see the constants
// below) and NEVER echoes diff content: the signal lands verbatim in the
// `policy_evaluated` audit payload, which must stay bounded and
// redaction-safe. Files lists the .go paths the detector cleared, sorted.
type CommentOnlySignal struct {
	CommentOnly bool     `json:"comment_only"`
	Reason      string   `json:"reason,omitempty"`
	Files       []string `json:"files,omitempty"`
}

// The closed set of Reason codes. Every non-comment-only verdict carries
// exactly one of these; no other string is ever emitted.
const (
	// ReasonEmptyDiff — the diff changed no files at all. An empty diff
	// still fails tests_added_or_updated (it signals "stage produced
	// nothing"), so the detector never rescues it.
	ReasonEmptyDiff = "empty_diff"
	// ReasonNoGoSourceChanged — no testable-source file changed. The
	// existing docs-only branch (#610) owns that case.
	ReasonNoGoSourceChanged = "no_go_source_changed"
	// ReasonNonGoSourceChanged — a changed testable-source file is not a
	// .go file. The exemption is Go-only: nothing here can reason about
	// another language's comment syntax.
	ReasonNonGoSourceChanged = "non_go_source_changed"
	// ReasonUnsupportedStatus — a changed .go file carries a status other
	// than A or M. A deleted, renamed, copied or type-changed Go file is
	// behavioral regardless of what the emitted hunks look like.
	ReasonUnsupportedStatus = "unsupported_status"
	// ReasonPatchAbsent — no patch text reached evaluation (an older
	// bundle, or a runner that could not compute the patch). Without the
	// hunks there is nothing to prove.
	ReasonPatchAbsent = "patch_absent"
	// ReasonPatchTruncated — the runner capped the patch (256 KiB). A
	// truncated patch can hide a behavioral hunk behind a comment-only
	// prefix.
	ReasonPatchTruncated = "patch_truncated"
	// ReasonBinaryPatch — a section carries a binary marker, so its
	// content is not line-inspectable.
	ReasonBinaryPatch = "binary_patch"
	// ReasonQuotedPatchPath — a diff header path is C-quoted (git quotes
	// a path carrying a quote, backslash, control character or non-ASCII
	// byte unless core.quotePath is disabled). The detector refuses
	// rather than decoding it.
	ReasonQuotedPatchPath = "quoted_patch_path"
	// ReasonPatchPathUnmatched — a patch section names a path absent from
	// the name-status change set, so the two views disagree.
	ReasonPatchPathUnmatched = "patch_path_unmatched"
	// ReasonFileMissingFromPatch — a changed .go file has no section in
	// the patch, so its content changes are unproven.
	ReasonFileMissingFromPatch = "file_missing_from_patch"
	// ReasonDuplicatePatchSection — the patch carries more than one
	// section for the same path. A later section would overwrite an
	// earlier one in the per-path map, so a comment-only section could
	// mask a behavioral one for the SAME file; the detector refuses the
	// whole malformed patch rather than picking a section.
	ReasonDuplicatePatchSection = "duplicate_patch_section"
	// ReasonSectionWithoutHunks — a cleared .go file's section carries no
	// `@@` hunk at all (a mode-only change emits only `old mode`/`new
	// mode` header lines). Zero changed lines would pass the line
	// inspection vacuously, so the detector refuses instead.
	ReasonSectionWithoutHunks = "section_without_hunks"
	// ReasonRawStringDelimiterInSection — a backtick appears anywhere in
	// a .go section (context lines included), so a changed line could sit
	// inside a raw-string literal whose delimiters are visible.
	ReasonRawStringDelimiterInSection = "raw_string_delimiter_in_section"
	// ReasonCgoImportInSection — `import "C"` appears anywhere in a .go
	// section (context lines included). In a cgo file the ordinary `//`
	// lines immediately preceding that import are COMPILED C code, so a
	// changed comment-shaped line in such a window is behavioral.
	ReasonCgoImportInSection = "cgo_import_in_section"
	// ReasonDirectiveChanged — a changed line is a Go compiler/tool
	// directive comment (//go:build, //line, // +build, // #cgo …), which
	// is behavioral despite being comment-shaped.
	ReasonDirectiveChanged = "directive_changed"
	// ReasonNonCommentLineChanged — a changed line is neither blank nor a
	// `//` line comment.
	ReasonNonCommentLineChanged = "non_comment_line_changed"
)

// DetectCommentOnlyGo proves a NARROW SYNTACTIC PROPERTY over the stage's
// emitted unified diff — it does NOT decide behavioral emptiness in
// general, and no caller may read it that way. The property is:
//
//   - every changed testable-source file is a .go file with status A or M;
//   - every one of those files has EXACTLY ONE section in the emitted
//     patch, and that section carries at least one `@@` hunk;
//   - within each such section, every changed (+/-) line is blank or an
//     ordinary `//` line comment — never a Go directive comment
//     (isGoDirectiveComment); and
//   - neither a backtick nor `import "C"` appears anywhere in the
//     section, context lines included.
//
// It is fail-closed on every other input: an empty diff, a non-.go
// testable-source change, any status other than A/M, an absent or
// truncated patch, a binary or C-quoted section header, a patch section
// naming an unknown path, a changed .go file with no section, two
// sections for one path, a section carrying no hunk, a directive line, or
// any other changed line. Each refusal carries one Reason code from the
// closed set above.
//
// HONEST LIMIT (binding, #2660): behavioral emptiness is NOT decidable
// from a unified diff, and the in-section scans above close only what the
// emitted window shows. TWO residuals are admitted:
//
//   - RAW STRING: a changed blank or `//`-shaped line that actually sits
//     inside a Go raw-string literal whose backtick delimiters lie
//     entirely OUTSIDE the emitted context.
//   - CGO PREAMBLE: a changed ordinary `//` line that is cgo preamble C
//     code (e.g. `// int foo() { return 1; }`) in a file whose
//     `import "C"` lies entirely OUTSIDE the emitted context. The ported
//     `#` arm of isGoDirectiveComment catches only the `#cgo`/`#include`
//     preprocessor forms, not plain C declarations.
//
// Both are accepted deliberately; each costs one vacuous satisfaction of
// `tests_added_or_updated` PER OCCURRENCE — neither is bounded at one
// ever — and closing them needs file contents (a runner-side AST or lexer
// signal, where the working tree exists), which a patch plus the trace
// bundle do not carry. Exploiting the cgo residual additionally requires a
// pre-existing `import "C"` file: introducing one is itself a non-comment
// change this detector refuses. See backend/internal/policy/README.md.
func DetectCommentOnlyGo(diff Diff) *CommentOnlySignal {
	if len(diff.ChangedFiles) == 0 {
		return refuse(ReasonEmptyDiff)
	}

	// (2) Classify the change set. Deletes are NOT skipped — a deleted
	// .go file is behavioral, and the status arm below refuses it.
	changedPaths := make(map[string]struct{}, len(diff.ChangedFiles))
	var testable []ChangedFile
	for _, f := range diff.ChangedFiles {
		changedPaths[f.Path] = struct{}{}
		if IsTestableSourcePath(f.Path) {
			testable = append(testable, f)
		}
	}
	if len(testable) == 0 {
		return refuse(ReasonNoGoSourceChanged)
	}
	for _, f := range testable {
		if !strings.HasSuffix(strings.ToLower(f.Path), ".go") {
			return refuse(ReasonNonGoSourceChanged)
		}
	}
	goFiles := make([]string, 0, len(testable))
	for _, f := range testable {
		if f.Status != StatusAdded && f.Status != StatusModified {
			return refuse(ReasonUnsupportedStatus)
		}
		goFiles = append(goFiles, f.Path)
	}

	// (3) The patch is the evidence. No patch, or a patch the runner cut
	// at its size cap, proves nothing.
	if diff.PatchTruncated {
		return refuse(ReasonPatchTruncated)
	}
	if diff.Patch == "" {
		return refuse(ReasonPatchAbsent)
	}

	// (4) Split into per-file sections and match each against the
	// name-status change set.
	sections, reason := splitPatchSections(diff.Patch)
	if reason != "" {
		return refuse(reason)
	}
	for path := range sections {
		if _, ok := changedPaths[path]; !ok {
			return refuse(ReasonPatchPathUnmatched)
		}
	}
	for _, p := range goFiles {
		if _, ok := sections[p]; !ok {
			return refuse(ReasonFileMissingFromPatch)
		}
	}

	// (5)/(6)/(7) Inspect each Go section's changed lines.
	for _, p := range goFiles {
		if reason := inspectGoSection(sections[p]); reason != "" {
			return refuse(reason)
		}
	}

	sort.Strings(goFiles)
	return &CommentOnlySignal{CommentOnly: true, Files: goFiles}
}

// cgoImportMarker is the import statement that turns the `//` lines
// immediately above it into compiled C. Its presence anywhere in an
// emitted section makes that section's comment-shaped lines unprovable.
const cgoImportMarker = `import "C"`

func refuse(reason string) *CommentOnlySignal {
	return &CommentOnlySignal{CommentOnly: false, Reason: reason}
}

// splitPatchSections cuts the unified diff into per-file sections keyed by
// the section's path, taking the path from the `+++ b/<path>` header line
// (falling back to `--- a/<path>` when +++ is /dev/null). Returns a closed
// reason code instead of a map when a header path is C-quoted or a section
// is binary.
//
// Header recognition is bounded to the span between the section's
// `diff --git` line and its first `@@` hunk header, so an ADDED source
// line beginning with `++ ` can never be mistaken for a `+++` header.
func splitPatchSections(patch string) (map[string][]string, string) {
	sections := make(map[string][]string)
	var (
		cur      []string
		curPath  string
		inHeader bool
		open     bool
	)
	flush := func() string {
		if !open {
			return ""
		}
		if curPath == "" {
			// A section whose header named no usable path: treat it as a
			// path we cannot match against the change set.
			return ReasonPatchPathUnmatched
		}
		if _, dup := sections[curPath]; dup {
			// A malformed patch carrying two sections for one path.
			// Storing the later one would silently DROP the earlier — a
			// comment-only section could mask a behavioral section for the
			// same file — so refuse the whole patch.
			return ReasonDuplicatePatchSection
		}
		sections[curPath] = cur
		return ""
	}
	for _, line := range strings.Split(patch, "\n") {
		if strings.HasPrefix(line, "diff --git ") {
			if reason := flush(); reason != "" {
				return nil, reason
			}
			cur = nil
			curPath = ""
			inHeader = true
			open = true
			continue
		}
		if !open {
			// Preamble before the first section header — nothing to
			// attribute it to; skip.
			continue
		}
		if strings.HasPrefix(line, "Binary files ") || strings.HasPrefix(line, "GIT binary patch") {
			return nil, ReasonBinaryPatch
		}
		if inHeader {
			if strings.HasPrefix(line, "@@") {
				inHeader = false
				cur = append(cur, line)
				continue
			}
			switch {
			case strings.HasPrefix(line, "+++ "):
				p, reason := headerPath(line[4:])
				if reason != "" {
					return nil, reason
				}
				if p != "" {
					curPath = p
				}
			case strings.HasPrefix(line, "--- "):
				p, reason := headerPath(line[4:])
				if reason != "" {
					return nil, reason
				}
				if p != "" && curPath == "" {
					curPath = p
				}
			}
			continue
		}
		cur = append(cur, line)
	}
	if reason := flush(); reason != "" {
		return nil, reason
	}
	if len(sections) == 0 {
		// A non-empty patch that carried no `diff --git` section at all:
		// we cannot attribute any hunk to a file.
		return nil, ReasonFileMissingFromPatch
	}
	return sections, ""
}

// headerPath extracts the repo-relative path from a `---`/`+++` header
// body (the text after the four-byte marker). Returns "" for /dev/null,
// and ReasonQuotedPatchPath for a C-quoted path — git quotes any path
// carrying a quote, backslash, control character or non-ASCII byte unless
// core.quotePath is disabled, and the detector refuses rather than
// decoding it (a comment-only fix to a .go file with a non-ASCII name is
// refused, which is the safe direction).
func headerPath(body string) (string, string) {
	// Strip the trailing tab-separated timestamp git may append.
	if i := strings.IndexByte(body, '\t'); i >= 0 {
		body = body[:i]
	}
	body = strings.TrimRight(body, "\r")
	if body == "/dev/null" {
		return "", ""
	}
	if strings.HasPrefix(body, `"`) {
		return "", ReasonQuotedPatchPath
	}
	// Both `a/` and `b/` prefixes are the default git prefixes.
	if strings.HasPrefix(body, "a/") || strings.HasPrefix(body, "b/") {
		return body[2:], ""
	}
	return body, ""
}

// inspectGoSection walks one .go section's hunk body and returns a closed
// reason code, or "" when every changed line is blank or an ordinary line
// comment.
//
// A section with no body at all carries no `@@` hunk (git emits a
// mode-only change as header lines alone). Zero changed lines would pass
// the loops below vacuously, so it is refused rather than cleared.
//
// The backtick and `import "C"` scans cover the WHOLE section — context
// lines included — because a raw-string delimiter anywhere in the emitted
// window means a changed `//`-shaped line could be string data rather than
// a comment, and an `import "C"` anywhere in it means a changed `//` line
// could be cgo preamble C code rather than a comment.
func inspectGoSection(body []string) string {
	if len(body) == 0 {
		return ReasonSectionWithoutHunks
	}
	for _, line := range body {
		if strings.ContainsRune(line, '`') {
			return ReasonRawStringDelimiterInSection
		}
	}
	for _, line := range body {
		if strings.Contains(line, cgoImportMarker) {
			return ReasonCgoImportInSection
		}
	}
	for _, line := range body {
		if line == "" {
			continue
		}
		switch line[0] {
		case '+', '-':
			// A changed line. (The `---`/`+++` headers cannot reach here:
			// splitPatchSections recognizes them only in the header span.)
			if reason := inspectChangedLine(line[1:]); reason != "" {
				return reason
			}
		default:
			// Context (' '), a hunk header ('@@'), or the
			// "\ No newline at end of file" marker — all ignored.
		}
	}
	return ""
}

// inspectChangedLine classifies one changed line's text (marker already
// stripped): blank and ordinary `//` comments are accepted; a directive
// comment and anything else are refused.
func inspectChangedLine(text string) string {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return ""
	}
	if !strings.HasPrefix(trimmed, "//") {
		return ReasonNonCommentLineChanged
	}
	if isGoDirectiveComment(trimmed) {
		return ReasonDirectiveChanged
	}
	return ""
}

// isGoDirectiveComment reports whether s — a whitespace-trimmed line that
// starts with "//" — is a Go compiler or tool directive rather than an
// ordinary comment.
//
// The logic is PORTED, not delegated: `go/ast.IsDirective` DOES NOT EXIST
// (verified against go1.26.3 — go/ast exports only IsExported and
// IsGenerated), so there is no stdlib symbol to call. It reproduces
// go/ast's unexported isDirective — the `line `/`extern `/`export `
// prefixes plus the `[a-z0-9]+:[a-z0-9]` tool-directive shape — and
// extends it CONSERVATIVELY (admitting fewer diffs, never more) with the
// legacy `+build` tag and the cgo `#`-preamble forms.
//
// The UNTRIMMED rest is load-bearing: it is what makes `// go:build` (a
// space after the slashes) an ORDINARY comment while `//go:build` is a
// directive, matching the compiler. The `+build`/`#` arm trims, but
// neither prefix can resurrect `go:build`.
func isGoDirectiveComment(s string) bool {
	rest := s[2:]

	// "//line file:line", "//extern name", "//export name".
	if rest == "line" ||
		strings.HasPrefix(rest, "line ") ||
		strings.HasPrefix(rest, "extern ") ||
		strings.HasPrefix(rest, "export ") {
		return true
	}

	// Tool directive: "//[a-z0-9]+:[a-z0-9]" with no space anywhere in
	// the prefix (go/ast's rule).
	if colon := strings.IndexByte(rest, ':'); colon > 0 && colon+1 < len(rest) {
		ok := true
		for i := 0; i <= colon+1; i++ {
			if i == colon {
				continue
			}
			c := rest[i]
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}

	// Legacy build tag and the cgo preamble forms, which DO tolerate a
	// leading space after the slashes.
	lead := strings.TrimLeft(rest, " \t")
	if strings.HasPrefix(lead, "+build") || strings.HasPrefix(lead, "#") {
		return true
	}
	return false
}
