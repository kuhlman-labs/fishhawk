// Package fixupattempt derives, for each concern routed back to an implement
// fix-up pass, the repository files that concern's routed instruction text
// NAMES, and classifies a routed concern whose named files the pass left
// entirely untouched as NOT ATTEMPTED (#2896).
//
// It exists because a fix-up pass reports `succeeded` with no mechanical check
// that each routed concern was even attempted: the verify gate certifies only
// that the tree builds, and the implement re-review is diff-only, so a silently
// dropped concern is indistinguishable from an addressed one. That is not
// hypothetical — run 925addab / PR #2895 routed two concerns, the pass touched
// only the file the LOW named, the MEDIUM's file was never opened, and the
// stage reported clean; the drop was caught only because a human diffed the
// fix-up commit by hand.
//
// The signal this package feeds is EVIDENCE ONLY and deliberately weaker than
// "not addressed": untouched named files are evidence the concern was NOT
// ATTEMPTED. A concern can be legitimately resolved by editing a DIFFERENT file
// than the reviewer named, or legitimately declined. The surface therefore warns
// and asks the reviewer to arbitrate; it never fails, re-opens, or re-budgets a
// pass.
//
// Long-form contract: backend/internal/fixupattempt/README.md.
package fixupattempt

import "strings"

// Concern is one concern routed back to a fix-up pass, already resolved against
// the candidate path set by the caller.
type Concern struct {
	// ID is the durable concern UUID when the trigger recorded one. The
	// deprecated positional routing path leaves it empty, which is why Position
	// exists.
	ID string
	// Position is the 1-based routing position captured AT ROUTING TIME — the
	// concern's index in the trigger payload's concern slice, before any
	// filtering. It is carried through classification verbatim and is NEVER
	// re-derived from a filtered slice: deriving it after filtering would label
	// the surviving finding "concern 1" when the concern actually dropped was
	// the second, sending an operator to look at a concern that WAS attempted
	// (binding condition 1 on #2896).
	Position int
	// Severity and Category are the server-derived literals from the concern
	// row. They are rendered; the concern NOTE deliberately is not (it is
	// already rendered, with its own trust framing, by the routed-concern
	// surfaces).
	Severity string
	Category string
	// Implicated is the set of candidate repo-relative paths this concern's
	// routed instruction text named, in candidate order. Empty means the
	// derivation could not decide anything for this concern — see Unattempted.
	Implicated []string
}

// Finding is one routed concern whose implicated files the fix-up pass left
// ENTIRELY untouched.
type Finding struct {
	ID              string
	Position        int
	Severity        string
	Category        string
	ImplicatedFiles []string
}

// pathByte reports whether b can appear inside a repo-relative path token.
//
// The class deliberately EXCLUDES ':' so a `path:LINE` citation (a reviewer
// habit) splits into the path and the line number rather than being rejected as
// one unmatchable token, and excludes every quoting/bracketing byte (backtick,
// quotes, parens, brackets) so a `backticked` or (parenthesised) path is a token
// boundary rather than part of the token.
func pathByte(b byte) bool {
	switch {
	case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9':
		return true
	}
	switch b {
	case '/', '.', '_', '-', '+', '@', '~':
		return true
	}
	return false
}

// tokenize splits note into maximal runs of path bytes.
func tokenize(note string) []string {
	var out []string
	start := -1
	for i := 0; i < len(note); i++ {
		if pathByte(note[i]) {
			if start < 0 {
				start = i
			}
			continue
		}
		if start >= 0 {
			out = append(out, note[start:i])
			start = -1
		}
	}
	if start >= 0 {
		out = append(out, note[start:])
	}
	return out
}

// normalizeToken strips the decorations a reviewer's prose puts around a path:
// a leading "./" (and any repetition of it) and trailing sentence punctuation.
// A path can never legitimately END in '.', ',' or '-', so stripping those is
// lossless; ':' is already a token boundary.
func normalizeToken(tok string) string {
	for strings.HasPrefix(tok, "./") {
		tok = tok[2:]
	}
	tok = strings.TrimRight(tok, ".,-")
	return tok
}

// Implicated returns the candidate repo-relative paths that note NAMES, deduped
// and in candidate order.
//
// Matching is ANCHORED on the caller-supplied candidate set (the approved plan's
// scope.files union the committed diff's paths) rather than being free-form path
// extraction, so the function can never mint a phantom path the pass could not
// have touched. A note token resolves to a candidate in exactly two ways:
//
//   - EXACT — the normalized token equals the candidate.
//   - SUFFIX — the reviewer omitted the repository prefix (a note citing
//     `internal/server/README.md` for `backend/internal/server/README.md`).
//     The token must match on a '/' boundary, and if it suffix-matches TWO OR
//     MORE candidates it is AMBIGUOUS and is discarded rather than guessed: a
//     wrong file on this surface is worse than no file, for the same reason
//     Position is never re-derived.
//
// Both rules are boundary-anchored, so a note naming `docs/onboarding.md.bak` or
// `xdocs/onboarding.md` does NOT implicate `docs/onboarding.md`. That matters:
// a spurious match would mark an untouched file as touched and MASK a genuine
// drop.
func Implicated(note string, candidates []string) []string {
	if note == "" || len(candidates) == 0 {
		return nil
	}
	hit := make(map[string]struct{}, len(candidates))
	for _, tok := range tokenize(note) {
		tok = normalizeToken(tok)
		if tok == "" {
			continue
		}
		if resolved, ok := resolveToken(tok, candidates); ok {
			hit[resolved] = struct{}{}
		}
	}
	if len(hit) == 0 {
		return nil
	}
	out := make([]string, 0, len(hit))
	for _, c := range candidates {
		if _, ok := hit[c]; ok {
			delete(hit, c)
			out = append(out, c)
		}
	}
	return out
}

// resolveToken maps one normalized note token onto a candidate. An exact match
// always wins (it cannot be ambiguous — candidates are deduped by the caller and
// duplicates resolve to the same string); otherwise a UNIQUE '/'-boundary suffix
// match resolves, and a suffix match hitting two or more candidates resolves to
// nothing.
func resolveToken(tok string, candidates []string) (string, bool) {
	for _, c := range candidates {
		if c == tok {
			return c, true
		}
	}
	found := ""
	for _, c := range candidates {
		if strings.HasSuffix(c, "/"+tok) {
			if found != "" && found != c {
				// Ambiguous across candidates — undeterminable, never a guess.
				return "", false
			}
			found = c
		}
	}
	if found == "" {
		return "", false
	}
	return found, true
}

// Unattempted classifies the routed concerns against the paths the fix-up pass
// actually committed, returning the concerns whose implicated files were left
// entirely untouched plus a count of the concerns the check could NOT decide.
//
// A routed concern yields a Finding when its Implicated set is NON-EMPTY and
// DISJOINT from touched. A PARTIALLY touched implicated set counts as
// ATTEMPTED: the pass demonstrably opened one of the files the concern named,
// which is all this signal claims to detect.
//
// A concern with an EMPTY implicated set is UNDETERMINABLE — its routed text
// named no candidate path — and is NOT reported per-concern. Reporting it would
// fire on nearly every routine fix-up (most reviewer prose names no file), and a
// signal that fires always is a signal that is ignored. That asymmetry is the
// deliberate noise/coverage trade, and its cost is honest and visible: the
// undeterminable count is returned so the caller can state how much of the pass
// the check could not decide, rather than letting silence read as "clean". A
// concern dropped without its routed text naming a file is NOT caught here.
//
// Findings preserve the caller's routing order and each Finding's Position is
// copied from its Concern, never recomputed from this function's filtered
// output.
func Unattempted(routed []Concern, touched map[string]struct{}) (findings []Finding, undeterminable int) {
	for _, c := range routed {
		if len(c.Implicated) == 0 {
			undeterminable++
			continue
		}
		attempted := false
		for _, p := range c.Implicated {
			if _, ok := touched[p]; ok {
				attempted = true
				break
			}
		}
		if attempted {
			continue
		}
		findings = append(findings, Finding{
			ID:       c.ID,
			Position: c.Position,
			Severity: c.Severity,
			Category: c.Category,
			// Copy so a caller mutating the Concern cannot mutate the Finding.
			ImplicatedFiles: append([]string(nil), c.Implicated...),
		})
	}
	return findings, undeterminable
}

// Untouched returns the subset of paths absent from touched, order-preserving
// and deduped.
//
// It backs the UNATTRIBUTED half of the signal: paths named in the routed
// instruction text as a whole (the operator's fix-up reason and free-text
// operator_concern) rather than inside one concern's own note. When a pass
// routes SEVERAL concerns, a path in that shared text cannot be attributed to
// one of them without guessing — and guessing is exactly what Position and the
// ambiguous-suffix rule refuse to do — so such a path is reported as a FILE the
// routed instructions named and the pass never touched, with no claim about
// WHICH concern it belongs to.
//
// This is what makes the check non-inert on the incident that motivated it: in
// run 925addab neither routed concern NOTE named a file, but the operator's
// routed reason named both `docs/onboarding.md` and
// `backend/internal/server/README.md`, and the pass touched only the second.
func Untouched(paths []string, touched map[string]struct{}) []string {
	var out []string
	seen := make(map[string]struct{}, len(paths))
	for _, p := range paths {
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		if _, ok := touched[p]; ok {
			continue
		}
		out = append(out, p)
	}
	return out
}
