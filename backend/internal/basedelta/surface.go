package basedelta

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/bmatcuk/doublestar/v4"

	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// WorkflowSpecPath is the product's own workflow spec path. A change to it on
// either side always forces a pass, whatever the declaration says, so the
// declaration covers itself (ADR-124 D5).
const WorkflowSpecPath = ".fishhawk/workflows.yaml"

// Surface is a repository's declared verdict-transfer surface: the
// workflow-v2 `verdict_transfer` block (ADR-124 D1), read from the run's
// ADMISSION spec by ResolveSurface. The product carries no built-in pattern,
// directory or version of any repository's layout (ADR-110, ADR-124 D6): a
// nil *Surface is undeclared and Classify refuses it. Every method is
// nil-safe.
type Surface struct {
	// Global are the verdict-wide (G) root-anchored doublestar patterns: a
	// change to a match on EITHER side forces a pass (ADR-098 D1).
	Global []string
	// SharedDirs are the ADR-096 D1(d) shared-artifact directory prefixes,
	// each ending in "/".
	SharedDirs []string
	// DocCheck are the ADR-098 D1 doc-check (D) inputs. Until the D-i/D-ii
	// relaxation lands (#4317) a match on either side forces a pass.
	DocCheck []string
	// Inert are declared-inert patterns, consulted only for a path no other
	// class covers.
	Inert []string
	// Opaque are patterns whose change refuses transfer.
	Opaque []string
	// CompletenessCheck names the conformance test that proves the
	// declaration complete. A declarative reference only: it is never
	// executed, evaluated or interpolated. Without it enforce mode is
	// withheld (ADR-124 D4).
	CompletenessCheck string
}

// view is the one nil-safe accessor every method reads through.
func (s *Surface) view() Surface {
	if s == nil {
		return Surface{}
	}
	return *s
}

// ResolveSurface is THE production resolver (ADR-124 D1, D4): it parses the
// run's cached admission spec and returns its declared surface. nil bytes, or
// a parsed spec without a verdict_transfer block (a v0/v1 document included),
// is (nil, nil) — undeclared. A spec that does not parse is (nil, err).
func ResolveSurface(specBytes []byte) (*Surface, error) {
	if specBytes == nil {
		return nil, nil
	}
	sp, err := spec.ParseBytes(specBytes)
	if err != nil {
		return nil, err
	}
	vt := sp.VerdictTransfer
	if vt == nil {
		return nil, nil
	}
	return &Surface{
		Global:            append([]string(nil), vt.GlobalSurface...),
		SharedDirs:        append([]string(nil), vt.SharedArtifactDirs...),
		DocCheck:          append([]string(nil), vt.DocCheckInputs...),
		Inert:             append([]string(nil), vt.InertPaths...),
		Opaque:            append([]string(nil), vt.OpaquePaths...),
		CompletenessCheck: vt.CompletenessCheck,
	}, nil
}

// matchAny reports the first pattern of pats p matches. A pattern doublestar
// rejects never matches (firstInvalid refuses it before any rule runs).
func matchAny(pats []string, p string) (string, bool) {
	for _, pat := range pats {
		if ok, err := doublestar.Match(pat, p); err == nil && ok {
			return pat, true
		}
	}
	return "", false
}

// matchVerdictWide reports whether a change to p forces a pass: the
// workflow spec itself, or a G match.
func (s *Surface) matchVerdictWide(p string) bool {
	if p == WorkflowSpecPath {
		return true
	}
	_, ok := s.MatchGlobal(p)
	return ok
}

// MatchGlobal reports the first declared G pattern p matches. Matching is
// anchored at the repository root: "testdata/**" matches testdata/wire/x.json
// but not backend/internal/x/testdata/y.json.
func (s *Surface) MatchGlobal(p string) (string, bool) {
	return matchAny(s.view().Global, p)
}

func (s *Surface) matchDocCheck(p string) bool {
	_, ok := matchAny(s.view().DocCheck, p)
	return ok
}

func (s *Surface) matchOpaque(p string) bool {
	_, ok := matchAny(s.view().Opaque, p)
	return ok
}

func (s *Surface) inert(p string) bool {
	_, ok := matchAny(s.view().Inert, p)
	return ok
}

// sharedDir reports a declared shared-artifact entry containing BOTH a and b.
// Every entry is evaluated, so the answer does not depend on declaration
// order (a narrower entry listed first never hides a wider one).
func (s *Surface) sharedDir(a, b string) (string, bool) {
	for _, d := range s.view().SharedDirs {
		if strings.HasPrefix(a, d) && strings.HasPrefix(b, d) {
			return d, true
		}
	}
	return "", false
}

// firstInvalid reports the first malformed declared entry, verbatim: the
// patterns of G, doc-check, inert and opaque in that order, then the shared
// dirs (which must also end in "/", not be "/" alone, and be canonical).
func (s *Surface) firstInvalid() (string, bool) {
	v := s.view()
	for _, list := range [][]string{v.Global, v.DocCheck, v.Inert, v.Opaque} {
		for _, p := range list {
			if !validEntry(p) || !doublestar.ValidatePattern(p) {
				return p, true
			}
		}
	}
	for _, d := range v.SharedDirs {
		trimmed, ok := strings.CutSuffix(d, "/")
		if !validEntry(d) || !ok || trimmed == "" || path.Clean(trimmed) != trimmed {
			return d, true
		}
	}
	return "", false
}

// validEntry is the shape every declared entry shares: non-empty, valid
// UTF-8, root-relative (no leading / or ./), no .. segment, no backslash and
// no control character.
func validEntry(e string) bool {
	if e == "" || !utf8.ValidString(e) || strings.HasPrefix(e, "/") || strings.HasPrefix(e, "./") ||
		strings.Contains(e, `\`) {
		return false
	}
	for _, seg := range strings.Split(e, "/") {
		if seg == ".." {
			return false
		}
	}
	for _, r := range e {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// incomplete reports a declaration with no verdict-wide class: ADR-124
// Option B's fail-open, refused as surface_incomplete.
func (s *Surface) incomplete() bool {
	return len(s.view().Global) == 0
}

// EnforceWithheld reports a declaration that names no completeness check:
// enforce mode stays in shadow for it (ADR-124 D4). An absent declaration is
// not withheld — the classifier itself refuses it (surface_undeclared).
func (s *Surface) EnforceWithheld() bool {
	if s == nil {
		return false
	}
	return strings.TrimSpace(s.CompletenessCheck) == ""
}

// Digest identifies the declaration a verdict was decided against:
// "sha256:<hex>" over every class, each sorted, plus the completeness check.
// It is order-insensitive within a class, changes when any entry, its class
// or the check changes, and is "" for an undeclared (nil) surface, so nil
// and a declared-empty block differ.
func (s *Surface) Digest() string {
	if s == nil {
		return ""
	}
	norm := func(in []string) []string {
		out := append([]string{}, in...)
		sort.Strings(out)
		return out
	}
	raw, err := json.Marshal(struct {
		Global, SharedDirs, DocCheck, Inert, Opaque []string
		CompletenessCheck                           string
	}{norm(s.Global), norm(s.SharedDirs), norm(s.DocCheck), norm(s.Inert), norm(s.Opaque), s.CompletenessCheck})
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Unmatched lists, sorted and de-duplicated, every declared pattern (G,
// doc-check, inert, opaque) and shared dir that matches NO file of files: a
// prospective-coverage DIAGNOSTIC, never a refusal (ADR-124 D3). An invalid
// entry is skipped; invalid_surface reports it.
func (s *Surface) Unmatched(files []string) []string {
	v := s.view()
	seen := map[string]bool{}
	var out []string
	add := func(e string) {
		if !seen[e] {
			seen[e] = true
			out = append(out, e)
		}
	}
	for _, list := range [][]string{v.Global, v.DocCheck, v.Inert, v.Opaque} {
		for _, pat := range list {
			if !validEntry(pat) || !doublestar.ValidatePattern(pat) {
				continue
			}
			hit := false
			for _, f := range files {
				if ok, err := doublestar.Match(pat, f); err == nil && ok {
					hit = true
					break
				}
			}
			if !hit {
				add(pat)
			}
		}
	}
	for _, d := range v.SharedDirs {
		if !validEntry(d) || !strings.HasSuffix(d, "/") {
			continue
		}
		hit := false
		for _, f := range files {
			if strings.HasPrefix(f, d) {
				hit = true
				break
			}
		}
		if !hit {
			add(d)
		}
	}
	sort.Strings(out)
	return out
}

// ListTree lists the regular files and symlinks under dir, repo-relative,
// slash-separated and sorted. Symlinks are listed, never followed.
func ListTree(dir string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() || d.Type()&fs.ModeSymlink != 0 {
			rel, err := filepath.Rel(dir, p)
			if err != nil {
				return err
			}
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("list tree %s: %w", dir, err)
	}
	sort.Strings(out)
	return out, nil
}
