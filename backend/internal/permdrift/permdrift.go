// Package permdrift is the deterministic permission-drift check (ADR-084 D5 /
// rule 5, E80.4 / #3761): per declared permission surface, a SEMANTIC
// extractor turns a file's bytes into a normalized grant set, and Compare
// diffs a base set against a head set into widenings and narrowings.
//
// Comparing extracted grants rather than text is what makes the check
// reorder-insensitive (a re-sorted forbidden_paths list or a re-keyed
// permissions block is not a change) and lets it say WHICH power moved and in
// which direction.
//
// The package is pure: no I/O, no logging, no clock. The server
// (backend/internal/server/permission_drift.go) fetches the base and head
// bytes and turns widenings into server-synthesized review concerns.
package permdrift

import (
	"fmt"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Polarity says what an entry's PRESENCE means.
type Polarity int

const (
	// Grant means presence (or a higher rank) is more power. A key appearing in
	// head is a widening; one vanishing is a narrowing.
	Grant Polarity = iota
	// Restriction means presence LIMITS power; absence is unrestricted. A key
	// vanishing from head is a widening; one appearing is a narrowing.
	Restriction
)

// String renders the polarity for test failure messages and notes.
func (p Polarity) String() string {
	if p == Restriction {
		return "restriction"
	}
	return "grant"
}

// Entry is one normalized grant or restriction.
type Entry struct {
	// Key is the stable, surface-specific identity (for example
	// "jobs.build.contents"). A key ending in WildcardSuffix covers every
	// single-segment sibling below its prefix (see Compare).
	Key string
	// Value is the human-readable level, rendered as Before/After.
	Value string
	// Rank is the POWER ordinal: higher is always more power, whatever the
	// polarity. A ranked Restriction (an autonomy ceiling) therefore widens
	// when its rank goes UP, exactly like a ranked Grant.
	Rank int
	// Polarity says what presence means.
	Polarity Polarity
}

// Grants is a normalized grant set, keyed by Entry.Key.
type Grants map[string]Entry

// Put records e under its own key, replacing any previous entry.
func (g Grants) Put(e Entry) { g[e.Key] = e }

// Direction is which way a Change moved power.
type Direction int

const (
	// Widened means more power at head than at base.
	Widened Direction = iota + 1
	// Narrowed means less power at head than at base.
	Narrowed
)

// String renders the direction.
func (d Direction) String() string {
	switch d {
	case Widened:
		return "widened"
	case Narrowed:
		return "narrowed"
	}
	return "unknown"
}

// Change is one key whose power moved between base and head.
type Change struct {
	Key string
	// Before and After are the entry Values, or Absent for a missing side.
	Before    string
	After     string
	Direction Direction
}

// Absent renders a side on which the key does not exist.
const Absent = "(absent)"

// Present is the Value of a presence-only entry (rank PresenceRank).
const Present = "present"

// PresenceRank is the rank every presence-only entry carries.
const PresenceRank = 1

// WildcardSuffix marks a key that covers its single-segment siblings: the
// key "<prefix>.*" covers "<prefix>.<x>" for every x containing no '.'.
const WildcardSuffix = ".*"

// keySegment escapes one FILE-DERIVED dotted key segment (a job id, a scope,
// a manifest permission or event name, an /mcp tool or scope name, a
// run-token scope, an env-allow var or NAME) so the file cannot shape the key
// structure: '%' becomes "%25" FIRST (so the escaping is injective), then
// '*', '.' and every control or format rune (unicode Cc/Cf/Zl/Zp, and any
// invalid UTF-8 byte) is percent-encoded byte by byte. An escaped segment
// therefore never contains a bare '.', so it cannot forge a segment boundary
// (job "a.b" with scope "c" vs job "a" with scope "b.c"), and never equals or
// ends in WildcardSuffix, so a file cannot mint its OWN wildcard (a manifest
// permission literally named "*" would otherwise subsume every sibling
// widening). The extractors' own wildcard keys (write-all, the default token,
// an empty anyOf) append WildcardSuffix after the escaped segment and keep
// the literal suffix. Bracketed values (globs, hosts, canonical matches) end
// in ']' and can never form a wildcard, so they are not escaped.
func keySegment(s string) string {
	if !needsKeyEscape(s) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if keyEscapeRune(r, size) {
			for j := i; j < i+size; j++ {
				fmt.Fprintf(&b, "%%%02X", s[j])
			}
		} else {
			b.WriteString(s[i : i+size])
		}
		i += size
	}
	return b.String()
}

// needsKeyEscape reports whether keySegment would change s.
func needsKeyEscape(s string) bool {
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if keyEscapeRune(r, size) {
			return true
		}
		i += size
	}
	return false
}

// keyEscapeRune reports whether keySegment percent-encodes rune r (of
// encoded width size).
func keyEscapeRune(r rune, size int) bool {
	switch {
	case r == '%', r == '*', r == '.':
		return true
	case r == utf8.RuneError && size == 1:
		return true
	}
	return unicode.In(r, unicode.Cc, unicode.Cf, unicode.Zl, unicode.Zp)
}

// Compare diffs base against head. The result is deterministic: one Change
// per moved key, sorted by key.
//
//   - key in both with a POLARITY CHANGE: one Change whatever the ranks — a
//     Restriction becoming a Grant is Widened (the limit vanished and a grant
//     appeared), a Grant becoming a Restriction is Narrowed. Without this rule
//     a gate override flipping from "not auto" to "auto" at one key (both
//     presence entries at PresenceRank) would read as no change;
//   - key in both, same polarity: rank up is Widened, rank down is Narrowed,
//     equal is no change (rank is the power ordinal for either polarity);
//   - key only in head: a Grant is Widened, a Restriction Narrowed;
//   - key only in base: a Grant is Narrowed, a Restriction Widened.
//
// WILDCARD SUBSUMPTION applies to Grants only. A head-only Grant covered by a
// base Grant wildcard of rank >= its own is NOT a widening (base write-all ->
// head {contents: write} gives nothing new), and a base-only Grant covered by
// a head Grant wildcard of rank >= its own is NOT a narrowing (base {contents:
// write} -> head write-all takes nothing away). A wildcard key is itself an
// ordinary key, so a NEW head wildcard is a widening and a vanished base one a
// narrowing.
func Compare(base, head Grants) []Change {
	keys := make([]string, 0, len(base)+len(head))
	for k := range base {
		keys = append(keys, k)
	}
	for k := range head {
		if _, ok := base[k]; !ok {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)

	var out []Change
	for _, k := range keys {
		b, inBase := base[k]
		h, inHead := head[k]
		switch {
		case inBase && inHead:
			if b.Polarity != h.Polarity {
				dir := Widened
				if h.Polarity == Restriction {
					dir = Narrowed
				}
				out = append(out, Change{Key: k, Before: b.Value, After: h.Value, Direction: dir})
				continue
			}
			switch {
			case h.Rank > b.Rank:
				out = append(out, Change{Key: k, Before: b.Value, After: h.Value, Direction: Widened})
			case h.Rank < b.Rank:
				out = append(out, Change{Key: k, Before: b.Value, After: h.Value, Direction: Narrowed})
			}
		case inHead:
			if h.Polarity == Restriction {
				out = append(out, Change{Key: k, Before: Absent, After: h.Value, Direction: Narrowed})
				continue
			}
			if coveredBy(base, h) {
				continue
			}
			out = append(out, Change{Key: k, Before: Absent, After: h.Value, Direction: Widened})
		default: // inBase only
			if b.Polarity == Restriction {
				out = append(out, Change{Key: k, Before: b.Value, After: Absent, Direction: Widened})
				continue
			}
			if coveredBy(head, b) {
				continue
			}
			out = append(out, Change{Key: k, Before: b.Value, After: Absent, Direction: Narrowed})
		}
	}
	return out
}

// coveredBy reports whether set holds a Grant wildcard covering e at a rank
// >= e's own. A wildcard never covers itself (it is matched as an ordinary
// key above), and only a single trailing segment is covered, so a key whose
// remainder contains a '.' is never subsumed — the conservative direction
// for widenings.
func coveredBy(set Grants, e Entry) bool {
	if e.Polarity != Grant || strings.HasSuffix(e.Key, WildcardSuffix) {
		return false
	}
	dot := strings.LastIndexByte(e.Key, '.')
	if dot < 0 {
		return false
	}
	w, ok := set[e.Key[:dot]+WildcardSuffix]
	return ok && w.Polarity == Grant && w.Rank >= e.Rank
}

// Levels is an ordered level vocabulary: index is the rank, so a later
// element is more power.
type Levels []string

// Rank returns v's ordinal, or false for a value outside the vocabulary.
func (l Levels) Rank(v string) (int, bool) {
	for i, s := range l {
		if s == v {
			return i, true
		}
	}
	return 0, false
}

// mustRank is Rank for an extractor: an out-of-vocabulary value is an error,
// so an unrecognized level fails the extraction (and the check fails closed
// to unevaluable) rather than being guessed at.
func (l Levels) mustRank(what, v string) (int, error) {
	r, ok := l.Rank(v)
	if !ok {
		return 0, fmt.Errorf("%s: unrecognized level %q (want one of %s)", what, v, strings.Join(l, ", "))
	}
	return r, nil
}

// The level vocabularies, each ordered least to most power.
var (
	// ActionsLevels is a GitHub Actions GITHUB_TOKEN scope level.
	ActionsLevels = Levels{"none", "read", "write"}
	// AppLevels is a GitHub App permission level.
	AppLevels = Levels{"none", "read", "write", "admin"}
	// AutonomyLevels is a workflow-spec autonomy tier.
	AutonomyLevels = Levels{"low", "medium", "high"}
	// ShellLevels is a workflow-spec stage shell posture.
	ShellLevels = Levels{"none", "restricted", "unrestricted"}
)
