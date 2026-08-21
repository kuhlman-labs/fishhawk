// Package repodoc is the consumer-agnostic mechanism for injecting a
// repo-authored document into an agent prompt (E55.1 / #2242).
//
// The problem it solves: a governance document that lives in the repo
// (review conventions, a product charter) must reach the agent as DATA the
// agent cannot edit and cannot be talked out of — which rules out "go read
// this file", because the agent's working tree is writable by the agent
// itself and by the change under review. So the document is resolved
// SERVER-SIDE, from the run's BASE REF pinned to a resolved commit SHA, and
// rendered into the prompt behind a caller-supplied framing.
//
// The package knows nothing about conventions or charters. A consumer
// supplies a Declaration (which path, and free-form provenance text naming
// where the declaration came from) plus a Framing (the wording that wraps
// the document), and gets back a Document it can render and attribute.
// Two shaped consumers — E55's review_conventions[] and #2234's
// charter.path — drive the same implementation; consumers_test.go pins that.
//
// The four properties this mechanism exists to hold:
//
//  1. BASE-REF PINNING. Resolution reads from a COMMIT SHA, never a mutable
//     branch name and never a filesystem path (this package imports no
//     file-read API at all). An empty base ref is REFUSED rather than
//     defaulted, because forge.FileFetcher documents an empty ref as the
//     repo's default branch — a mutable read. See Resolve.
//  2. ATTRIBUTION. Every injection writes a document_injected audit entry
//     naming the resolved path, the pinned commit, and the content hash.
//     Attribution FAILS CLOSED: a failed append means the caller must not
//     inject. See Attribute.
//  3. SIZE CAP. An over-cap document is cut rune-safely and carries a LOUD
//     marker saying the visible text is incomplete, plus its own
//     document_truncated audit entry. Truncation is never silent.
//  4. FRAMING INTEGRITY. Render delimits the document body and neutralizes
//     any line that would forge the closing delimiter, so committed content
//     cannot close the boundary and speak as framing. See Render.
//
// CONTENT-HASH BYTE DOMAIN (one domain, stated here, in the README, and in
// Document.ContentHash's own comment): ContentHash covers the RESOLVED
// bytes exactly as fetched from the forge — PRE-truncation and
// PRE-neutralization. Attribution answers "which revision of this document
// constrained the agent", and that question must have the same answer
// whether or not the document happened to exceed the cap. What was actually
// SHOWN is described by the sibling fields RenderedBytes, OriginalBytes,
// CapBytes and Truncated, not by the hash.
package repodoc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
)

// DefaultMaxBytes bounds how much of a declared document is rendered into a
// prompt when a Resolver does not override it. 32 KiB is a judgment call
// bounded by the #606 added-prompt-cost precedent, not a measured limit: a
// governance document is per-repo stable, so it rides the cache-stable
// prefix and costs its bytes once per cached prefix rather than per turn.
// A consumer that wants a tighter bound sets Resolver.MaxBytes.
const DefaultMaxBytes = 32768

// Errors callers switch on with errors.Is. Every one of them is returned
// wrapped in a *ResolveError, so the path and the declaration site are on
// the message regardless of which sentinel fired.
var (
	// ErrMissingDocument means the declared path does not exist at the
	// pinned commit (or the credential scope cannot see it — forge.ErrNotFound
	// does not distinguish). A DECLARED document that is absent is an error,
	// never an empty document: silently degrading would drop a governance
	// constraint the operator declared.
	ErrMissingDocument = errors.New("repodoc: declared document not found")
	// ErrInvalidPath means the declared path is not a safe repo-relative
	// path (empty, absolute, backslash-bearing, or carrying a "." / ".."
	// segment).
	ErrInvalidPath = errors.New("repodoc: invalid document path")
	// ErrUnpinnedBaseRef means the caller supplied an empty base ref, or the
	// base ref could not be pinned to a commit SHA. Both are refusals, not
	// fallbacks: reading at an unpinned ref is exactly the mutable read the
	// base-ref-pinning property forbids.
	ErrUnpinnedBaseRef = errors.New("repodoc: base ref is not pinned to a commit")
)

// ResolveError wraps every Resolve failure with the two identifiers an
// operator needs to act: the declared path and the DeclarationSite that
// declared it. The mechanism never interprets DeclarationSite — it only
// echoes it into errors and audit payloads.
type ResolveError struct {
	Path            string
	DeclarationSite string
	Err             error
}

func (e *ResolveError) Error() string {
	return fmt.Sprintf("repodoc: document %q (declared at %s): %v", e.Path, e.DeclarationSite, e.Err)
}

// Unwrap exposes the sentinel to errors.Is / errors.As.
func (e *ResolveError) Unwrap() error { return e.Err }

// Declaration is one consumer-declared document.
type Declaration struct {
	// Path is the repo-relative path of the document (no leading slash).
	Path string
	// DeclarationSite is free-form provenance text naming where the
	// declaration came from — e.g. "review_conventions[0] in
	// .fishhawk/workflows.yaml" or "charter.path in
	// .fishhawk/work-management.yaml". The mechanism never parses it; it
	// echoes it into every error message and audit payload so an operator
	// reading a failure knows which knob produced it.
	DeclarationSite string
	// Framing is the consumer's wording for the injected block. It rides on
	// the Declaration purely so a single declaration seam can carry both
	// halves of a consumer's contribution (which document, and how it is
	// introduced) through one value. Resolve ignores it entirely; only
	// Render reads it.
	Framing Framing
}

// Document is one resolved document, ready to render and attribute.
type Document struct {
	// Path is the resolved repo-relative path.
	Path string
	// Commit is the COMMIT SHA the document was read at — never a branch
	// name, and never forge.FileContent.SHA (which is the forge's BLOB id).
	Commit string
	// ContentHash is "sha256:<hex>" over the RESOLVED bytes AS FETCHED —
	// pre-truncation, pre-neutralization. See the package doc comment for
	// why this domain and not the injected bytes.
	ContentHash string
	// Content is the document text as it will be shown: post-truncation and
	// post-neutralization (any body line forging a delimiter has already been
	// replaced), including the truncation marker when Truncated.
	Content string
	// Truncated reports whether Content was cut at CapBytes.
	Truncated bool
	// OriginalBytes is len(resolved bytes as fetched).
	OriginalBytes int
	// RenderedBytes is len(Content) — the ACTUALLY-SHOWN byte domain:
	// post-truncation, post-neutralization, marker included. It is measured
	// after neutralization on purpose: a substituted delimiter line changes
	// the body's length, and attribution that reported the pre-substitution
	// count would name bytes the agent never saw.
	RenderedBytes int
	// DroppedBytes is how many of the resolved bytes were CUT (0 when not
	// truncated). Recorded at cut time rather than derived: the loud marker
	// makes Content longer than the kept prefix, so OriginalBytes minus
	// RenderedBytes is not the dropped count (and can even be negative on a
	// document barely over the cap).
	DroppedBytes int
	// CapBytes is the EFFECTIVE cap this document was resolved under (the
	// Resolver's MaxBytes, or DefaultMaxBytes). Carried on the Document so
	// attribution can report the configured cap: it cannot be reconstructed
	// from OriginalBytes and RenderedBytes once a rune-safe cut and a
	// marker have been applied.
	CapBytes int
	// DeclarationSite is echoed from the Declaration.
	DeclarationSite string
}

// fileFetcher is the narrow consumer-side view of forge.FileFetcher.
type fileFetcher interface {
	FetchFile(ctx context.Context, scope forge.CredentialScope, repo forge.RepoRef, path, ref string) (*forge.FileContent, error)
}

// commitResolver is the narrow consumer-side view of forge.Forge's branch
// read. *github.Forge and *gitlab.Forge satisfy both interfaces with no
// widening of either.
type commitResolver interface {
	GetBranchSHA(ctx context.Context, scope forge.CredentialScope, repo forge.RepoRef, branch string) (string, bool, error)
}

// Resolver reads declared documents from a forge at a pinned commit.
type Resolver struct {
	// Fetcher reads one file at a ref. Required.
	Fetcher fileFetcher
	// Commits resolves a branch name to its tip commit SHA. Required unless
	// every base ref handed to Resolve is already a 40-hex commit SHA.
	Commits commitResolver
	// MaxBytes overrides DefaultMaxBytes. Zero means the default.
	MaxBytes int
}

// Request is one resolution: which repo, under which credential scope, at
// which base ref, for which declaration.
type Request struct {
	Repo        forge.RepoRef
	Scope       forge.CredentialScope
	BaseRef     string
	Declaration Declaration
}

// Resolve reads req.Declaration's document from req.Repo at req.BaseRef,
// PINNED to a commit SHA. Every step fails closed:
//
//	(a) the declared path is validated as repo-relative and traversal-free;
//	(b) an EMPTY base ref is refused (forge.FileFetcher reads an empty ref
//	    as the repo's default branch — a mutable read);
//	(c) the base ref is pinned: a 40-hex ref is used verbatim, anything else
//	    is resolved via GetBranchSHA, whose ("", false, nil) missing-branch
//	    shape becomes an error rather than falling through to an empty ref;
//	(d) the file is fetched AT THE PINNED COMMIT SHA;
//	(e) forge.ErrNotFound becomes ErrMissingDocument; any other fetch error
//	    is wrapped, never degraded to an empty document.
//
// The returned Document carries the pinned commit, the content hash over
// the fetched bytes, and the effective cap.
func (r *Resolver) Resolve(ctx context.Context, req Request) (*Document, error) {
	decl := req.Declaration
	fail := func(err error) (*Document, error) {
		return nil, &ResolveError{Path: decl.Path, DeclarationSite: decl.DeclarationSite, Err: err}
	}

	if err := validatePath(decl.Path); err != nil {
		return fail(err)
	}
	if r.Fetcher == nil {
		return fail(errors.New("no file fetcher configured"))
	}
	if strings.TrimSpace(req.BaseRef) == "" {
		return fail(fmt.Errorf("%w: base ref is empty, which a forge reads as the repo's default branch", ErrUnpinnedBaseRef))
	}

	commit, err := r.pinCommit(ctx, req)
	if err != nil {
		return fail(err)
	}

	fc, err := r.Fetcher.FetchFile(ctx, req.Scope, req.Repo, decl.Path, commit)
	if err != nil {
		if errors.Is(err, forge.ErrNotFound) {
			return fail(fmt.Errorf("%w at commit %s: declare a path that exists, or remove the declaration", ErrMissingDocument, commit))
		}
		return fail(fmt.Errorf("fetch at commit %s: %w", commit, err))
	}
	if fc == nil {
		return fail(fmt.Errorf("%w at commit %s: forge returned no content", ErrMissingDocument, commit))
	}

	sum := sha256.Sum256(fc.Content)
	effectiveCap := r.capBytes()
	content, dropped, truncated := capContent(string(fc.Content), effectiveCap, decl.Path, commit)
	// Neutralize forged delimiter lines HERE, not only at render time, so
	// Content IS the body that will be shown and RenderedBytes counts those
	// exact bytes. Render neutralizes again (the operation is idempotent —
	// the replacement note is not itself a delimiter), so a hand-constructed
	// Document is still framed safely; neither layer alone is load-bearing.
	content = neutralizeBody(content)
	return &Document{
		Path:            decl.Path,
		Commit:          commit,
		ContentHash:     "sha256:" + hex.EncodeToString(sum[:]),
		Content:         content,
		Truncated:       truncated,
		OriginalBytes:   len(fc.Content),
		RenderedBytes:   len(content),
		DroppedBytes:    dropped,
		CapBytes:        effectiveCap,
		DeclarationSite: decl.DeclarationSite,
	}, nil
}

// capBytes returns the effective cap: Resolver.MaxBytes, or DefaultMaxBytes
// when unset.
func (r *Resolver) capBytes() int {
	if r.MaxBytes > 0 {
		return r.MaxBytes
	}
	return DefaultMaxBytes
}

// pinCommit turns req.BaseRef into a commit SHA. A 40-hex ref is already
// pinned. Anything else is a branch name resolved through GetBranchSHA —
// whose found=false shape is turned into ErrUnpinnedBaseRef rather than
// being allowed to fall through as an empty ref.
func (r *Resolver) pinCommit(ctx context.Context, req Request) (string, error) {
	ref := strings.TrimSpace(req.BaseRef)
	if isCommitSHA(ref) {
		return strings.ToLower(ref), nil
	}
	if r.Commits == nil {
		return "", fmt.Errorf("%w: base ref %q is not a commit SHA and no commit resolver is configured", ErrUnpinnedBaseRef, ref)
	}
	sha, found, err := r.Commits.GetBranchSHA(ctx, req.Scope, req.Repo, ref)
	if err != nil {
		return "", fmt.Errorf("resolve base ref %q to a commit: %w", ref, err)
	}
	sha = strings.TrimSpace(sha)
	if !found || sha == "" {
		return "", fmt.Errorf("%w: base ref %q does not resolve to a branch", ErrUnpinnedBaseRef, ref)
	}
	// The resolver's OUTPUT is validated, not merely its found flag. A
	// resolver that returns "HEAD", a branch name, a short SHA, or any other
	// non-commit string would otherwise reach FetchFile as a MUTABLE ref —
	// the pinning code would be present and not pin. Only a full 40-hex
	// commit id is accepted; anything else is refused BEFORE any fetch.
	if !isCommitSHA(sha) {
		return "", fmt.Errorf("%w: base ref %q resolved to %q, which is not a 40-hex commit SHA (a mutable ref must never be fetched)", ErrUnpinnedBaseRef, ref, sha)
	}
	return strings.ToLower(sha), nil
}

// isCommitSHA reports whether s is a full 40-hex git object id.
func isCommitSHA(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}

// validatePath rejects anything that is not a plain repo-relative path.
// Both the RAW segments and the path.Clean'd form are checked: the raw scan
// catches "a/./b" and "a//b" (which Clean would silently normalize away),
// and the Clean comparison catches every remaining non-canonical spelling.
func validatePath(p string) error {
	if p == "" {
		return fmt.Errorf("%w: path is empty", ErrInvalidPath)
	}
	if strings.HasPrefix(p, "/") {
		return fmt.Errorf("%w: path is absolute", ErrInvalidPath)
	}
	if strings.Contains(p, `\`) {
		return fmt.Errorf("%w: path contains a backslash", ErrInvalidPath)
	}
	// The path is rendered as METADATA outside the BEGIN/END delimiters, so a
	// control character in it (a newline above all) could end the Source line
	// and put repo-chosen text at column 0, OUTSIDE the data boundary — the
	// forged-delimiter attack arriving through the file NAME instead of the
	// file body. Refuse it at the declaration boundary; Render sanitizes as
	// well, so neither layer alone is load-bearing.
	if !utf8.ValidString(p) {
		return fmt.Errorf("%w: path is not valid UTF-8", ErrInvalidPath)
	}
	for _, r := range p {
		if isFramingBreaking(r) {
			return fmt.Errorf("%w: path contains the control character %q", ErrInvalidPath, r)
		}
	}
	for _, seg := range strings.Split(p, "/") {
		switch seg {
		case "":
			return fmt.Errorf("%w: path contains an empty segment", ErrInvalidPath)
		case ".", "..":
			return fmt.Errorf("%w: path contains a %q segment", ErrInvalidPath, seg)
		}
	}
	if cleaned := path.Clean(p); cleaned != p {
		return fmt.Errorf("%w: path is not canonical (cleans to %q)", ErrInvalidPath, cleaned)
	}
	return nil
}

// capContent cuts s to at most max bytes and appends a LOUD self-describing
// truncation marker, reporting how many bytes were dropped AT THE CAP.
//
// The cut is rune-safe: trimTrailingPartialRune removes ONLY the partial rune
// a fixed-offset slice can leave at the boundary, so dropped == len(s) -
// len(prefix) is exactly the bytes the cap removed and the marker's arithmetic
// closes. (An earlier form used strings.ToValidUTF8 for the trim, which also
// strips every invalid sequence MID-content — over-reporting dropped bytes and
// removing bytes the marker never disclosed.)
//
// Invalid UTF-8 anywhere in the shown text is then replaced IN BAND with
// U+FFFD, in BOTH branches, so a declared binary file cannot smuggle raw bytes
// into the prompt and the under-cap and over-cap paths behave alike. The
// replacement is visible in the body (that is the point of U+FFFD) and does
// not change the dropped-at-cap accounting, which is about the cut alone.
func capContent(s string, max int, docPath, commit string) (kept string, dropped int, truncated bool) {
	if len(s) <= max {
		return sanitizeUTF8(s), 0, false
	}
	prefix := trimTrailingPartialRune(s[:max])
	dropped = len(s) - len(prefix)
	marker := fmt.Sprintf(
		"\n\n...[TRUNCATED — this document is INCOMPLETE: %d of %d bytes shown, %d bytes dropped at the %d-byte cap. "+
			"Do NOT read the visible text as the whole document. The full document is %s at commit %s.]",
		len(prefix), len(s), dropped, max, docPath, commit)
	return sanitizeUTF8(prefix) + marker, dropped, true
}

// trimTrailingPartialRune drops the trailing bytes of s when — and only when —
// they are the front of a multi-byte rune whose continuation bytes fall beyond
// the end of s. Bytes anywhere else, valid or not, are left exactly as they
// are: this function's whole job is undoing the fixed-offset cut, not
// sanitizing content (sanitizeUTF8 does that, visibly).
func trimTrailingPartialRune(s string) string {
	for i := len(s) - 1; i >= 0 && i > len(s)-utf8.UTFMax; i-- {
		b := s[i]
		if b < utf8.RuneSelf {
			return s // a complete ASCII byte ends the string
		}
		if utf8.RuneStart(b) {
			if r, size := utf8.DecodeRuneInString(s[i:]); r == utf8.RuneError && size <= 1 {
				return s[:i] // lead byte without its continuation bytes
			}
			return s
		}
		// A continuation byte: keep walking back toward its lead byte.
	}
	return s
}

// sanitizeUTF8 replaces every invalid byte sequence with U+FFFD. Replacement
// rather than deletion: a byte that cannot be shown as itself is still
// accounted for on screen instead of vanishing silently.
func sanitizeUTF8(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	return strings.ToValidUTF8(s, "\uFFFD")
}

// isFramingBreaking reports whether r would let repo-authored metadata escape
// the line it is rendered on. Every C0/C1 control (newline and carriage return
// above all), DEL, and the Unicode line/paragraph separators qualify: metadata
// is rendered OUTSIDE the BEGIN/END delimiters, so a value that can start a
// new line can put repo-chosen text at column 0 where the framing lives.
func isFramingBreaking(r rune) bool {
	return unicode.IsControl(r) || r == '\u2028' || r == '\u2029'
}
