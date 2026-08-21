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
	// Content is the (possibly truncated) document text as it will be
	// rendered.
	Content string
	// Truncated reports whether Content was cut at CapBytes.
	Truncated bool
	// OriginalBytes is len(resolved bytes as fetched).
	OriginalBytes int
	// RenderedBytes is len(Content) — what was actually shown, including the
	// truncation marker when Truncated.
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
	if !found || strings.TrimSpace(sha) == "" {
		return "", fmt.Errorf("%w: base ref %q does not resolve to a branch", ErrUnpinnedBaseRef, ref)
	}
	return sha, nil
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
// truncation marker, reporting how many bytes were dropped. The cut is rune-safe (strings.ToValidUTF8 drops the
// trailing partial rune left by slicing at a fixed byte offset), the same
// idiom prompt.CapTextWithRetrieval uses. A document at or under the cap is
// returned unchanged with truncated=false — the > not >= boundary, so an
// exactly-at-cap document renders verbatim.
func capContent(s string, max int, docPath, commit string) (kept string, dropped int, truncated bool) {
	if len(s) <= max {
		return s, 0, false
	}
	prefix := strings.ToValidUTF8(s[:max], "")
	dropped = len(s) - len(prefix)
	marker := fmt.Sprintf(
		"\n\n...[TRUNCATED — this document is INCOMPLETE: %d of %d bytes shown, %d bytes dropped at the %d-byte cap. "+
			"Do NOT read the visible text as the whole document. The full document is %s at commit %s.]",
		len(prefix), len(s), dropped, max, docPath, commit)
	return prefix + marker, dropped, true
}
