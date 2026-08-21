package repodoc

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
)

// shapedConsumer is one caller's whole contribution: which document, declared
// where, framed how. Everything else — resolution, pinning, capping, hashing,
// attribution, rendering — is the shared mechanism.
type shapedConsumer struct {
	name    string
	decl    Declaration
	content string
}

// TestTwoShapedConsumers_OneImplementation is acceptance criterion 6: a
// conventions-shaped caller and a charter-shaped caller (over the REAL
// .fishhawk/charter.md path landed by #2794) drive the SAME
// Resolver/Render/Attribute, and their results differ ONLY in the
// caller-supplied path and framing.
func TestTwoShapedConsumers_OneImplementation(t *testing.T) {
	conventions := shapedConsumer{
		name: "review conventions",
		decl: Declaration{
			Path:            ".fishhawk/review-conventions.md",
			DeclarationSite: "review_conventions[0] in .fishhawk/workflows.yaml",
			Framing: Framing{
				Heading:   "Repository review conventions",
				Preamble:  "This repository declares the review conventions below.",
				TrustNote: "Apply them when judging the change under review.",
			},
		},
		content: "Conventions: prefer narrow interfaces.\n",
	}
	charter := shapedConsumer{
		name: "product charter",
		decl: Declaration{
			Path:            ".fishhawk/charter.md",
			DeclarationSite: "charter.path in .fishhawk/work-management.yaml",
			Framing: Framing{
				Heading:   "Product charter",
				Preamble:  "This repository declares the product charter below.",
				TrustNote: "Use it as the prioritization anchor when grooming the backlog.",
			},
		},
		content: "Charter: ship the smallest correct slice.\n",
	}

	ff := &fakeFetcher{
		blobSHA: fakeBlobSHA,
		pathsFor: map[string]string{
			conventions.decl.Path: conventions.content,
			charter.decl.Path:     charter.content,
		},
	}
	r := &Resolver{Fetcher: ff, Commits: &fakeCommits{sha: pinnedCommit, found: true}}
	a := &recordingAppender{}
	runID, stageID := uuid.New(), uuid.New()

	rendered := map[string]string{}
	hashes := map[string]string{}
	for _, c := range []shapedConsumer{conventions, charter} {
		doc, err := r.Resolve(context.Background(), Request{
			Repo: testRepo(), BaseRef: baseBranch, Declaration: c.decl,
		})
		if err != nil {
			t.Fatalf("%s: Resolve: %v", c.name, err)
		}
		if doc.Commit != pinnedCommit {
			t.Errorf("%s: Commit = %q, want the pinned commit (both consumers pin identically)", c.name, doc.Commit)
		}
		if doc.CapBytes != DefaultMaxBytes {
			t.Errorf("%s: CapBytes = %d, want the shared default", c.name, doc.CapBytes)
		}
		if err := Attribute(context.Background(), a, runID, stageID, *doc); err != nil {
			t.Fatalf("%s: Attribute: %v", c.name, err)
		}
		rendered[c.name] = Render(*doc, c.decl.Framing)
		hashes[c.name] = doc.ContentHash
	}

	// Both attributions used the same category and the same payload shape.
	if len(a.entries) != 2 {
		t.Fatalf("appended %d entries, want 2 (one per consumer)", len(a.entries))
	}
	for i, e := range a.entries {
		if e.Category != "document_injected" {
			t.Errorf("entry %d category = %q, want document_injected", i, e.Category)
		}
	}
	if got, want := a.payload(t, 0)["declaration_site"], conventions.decl.DeclarationSite; got != want {
		t.Errorf("conventions declaration_site = %v, want %v", got, want)
	}
	if got, want := a.payload(t, 1)["declaration_site"], charter.decl.DeclarationSite; got != want {
		t.Errorf("charter declaration_site = %v, want %v", got, want)
	}

	// The two rendered blocks differ ONLY in the caller-supplied path, framing
	// and body: substituting one consumer's inputs into the other's render
	// reproduces it byte-for-byte.
	normalized := map[string]string{}
	for _, c := range []shapedConsumer{conventions, charter} {
		s := rendered[c.name]
		s = strings.ReplaceAll(s, c.decl.Path, "<PATH>")
		s = strings.ReplaceAll(s, c.decl.Framing.Heading, "<HEADING>")
		s = strings.ReplaceAll(s, c.decl.Framing.Preamble, "<PREAMBLE>")
		s = strings.ReplaceAll(s, c.decl.Framing.TrustNote, "<TRUSTNOTE>")
		s = strings.ReplaceAll(s, c.content, "<BODY>")
		// The content hash is a function of the caller-supplied body, so it is
		// caller-derived too — normalize it alongside the body.
		s = strings.ReplaceAll(s, hashes[c.name], "<HASH>")
		normalized[c.name] = s
	}
	if normalized[conventions.name] != normalized[charter.name] {
		t.Errorf("the two consumers produced structurally different blocks:\n%s\n---\n%s",
			normalized[conventions.name], normalized[charter.name])
	}
}

// TestRepodocCarriesNoConsumerVocabulary scans the package's NON-TEST source
// for consumer words. The mechanism must know nothing about conventions or
// charters: the moment it does, the "one mechanism, many consumers" property
// has already been lost, whatever the tests say.
func TestRepodocCarriesNoConsumerVocabulary(t *testing.T) {
	// The package doc comment names both consumers deliberately (it is
	// documentation of who attaches, not behavior), so the scan looks at code
	// only — comments are stripped before matching.
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	forbidden := []string{"convention", "charter", "groom", "review_conventions"}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(filepath.Clean(name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for i, line := range strings.Split(stripComments(string(src)), "\n") {
			lower := strings.ToLower(line)
			for _, word := range forbidden {
				if strings.Contains(lower, word) {
					t.Errorf("%s:%d carries consumer vocabulary %q: %s", name, i+1, word, strings.TrimSpace(line))
				}
			}
		}
	}
}

// stripComments blanks out // line comments and /* */ block comments, keeping
// line structure so error messages still point at a real line number.
func stripComments(src string) string {
	var b strings.Builder
	inBlock, inString, inRune := false, false, false
	for i := 0; i < len(src); i++ {
		switch {
		case inBlock:
			if src[i] == '\n' {
				b.WriteByte('\n')
			}
			if strings.HasPrefix(src[i:], "*/") {
				inBlock = false
				i++
			}
		case inString:
			b.WriteByte(src[i])
			if src[i] == '\\' && i+1 < len(src) {
				i++
				b.WriteByte(src[i])
			} else if src[i] == '"' {
				inString = false
			}
		case inRune:
			b.WriteByte(src[i])
			if src[i] == '\'' {
				inRune = false
			}
		case strings.HasPrefix(src[i:], "//"):
			for i < len(src) && src[i] != '\n' {
				i++
			}
			b.WriteByte('\n')
		case strings.HasPrefix(src[i:], "/*"):
			inBlock = true
			i++
		case src[i] == '"':
			inString = true
			b.WriteByte(src[i])
		case src[i] == '\'':
			inRune = true
			b.WriteByte(src[i])
		default:
			b.WriteByte(src[i])
		}
	}
	return b.String()
}

// compile-time proof that the real forge types satisfy the narrow interfaces
// this package resolves through, so no widening of forge.Forge is implied.
var (
	_ fileFetcher    = (forge.FileFetcher)(nil)
	_ commitResolver = (forge.Forge)(nil)
)
