package spec

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
)

// Shared path predicate (E53.1 / #2224).
//
// A Predicate is ONE declarative match rule over a change: doublestar path
// globs, labels, change kinds, and trigger forms. It is the single matcher
// E53.3 (`applies_to` routing, #2226), E53.4 (`escalations`, #2227) and
// ADR-068's review-conventions (#2211) consume, so those three do NOT each
// grow a subtly different matcher. NOTHING references it yet — this file
// ships the primitive plus its proof; `$defs/predicate` and every consumer
// are wired by the sibling children.
//
// This file is duplicated BYTE-FOR-BYTE in cli/internal/spec/predicate.go,
// exactly as agentversion.go and v2removed.go already are: the backend and
// cli Go modules cannot share a package, so the matcher lives in both. The
// cli module validates a raw `any` tree and has no typed Spec, so the
// predicate is deliberately SELF-CONTAINED — it references no Spec symbol and
// can be copied with nothing adjusted. Parity is held by TEST CONTENT (the
// predicate_test.go byte-identity assertion) plus the sync-schemas-mirrored
// fixture corpus (docs/spec/predicate-fixtures.json -> both testdata/ dirs),
// NEVER by a shared package.
//
// Match semantics are FIXED and ratified (ADR-066 / #2209, #2224): AND across
// criteria TYPES (every DECLARED type must be satisfied), OR WITHIN a list
// (any one entry satisfies its type). An undeclared (empty) type does not
// constrain. An empty predicate — no criterion at all — is a validation
// ERROR, never match-all.
//
// Path handling is binary-safe and fail-closed, mirroring the posture
// scripts/check-coverage.py had to ship for the patch-coverage gate
// (ADR-059 / #2124): SplitNULPaths splits a `git ... -z` stream, and
// DecodeGitPath decodes git's C-quoted form and returns an actionable error
// NAMING an undecodable path rather than leaving it silently unmatched. Go
// strings are byte sequences with no enforced encoding, so a non-UTF-8 path
// round-trips through both without the surrogateescape handling the Python
// decoder needs — a reader hunting for that equivalent will not find one
// because it is not required.

// TriggerForm is the shape of run that a change arrives through. ADR-067
// decoupled stage types from "produces a diff", so a predicate must be able
// to route the non-diff forms — a scheduled groomer (ADR-065) or an
// on-demand incident intake (ADR-053) — by the same mechanism as a diff run.
type TriggerForm string

const (
	// TriggerDiff is a change carrying a code diff (paths). It is the
	// back-compat default: a Change with an empty Trigger normalizes to
	// TriggerDiff at Match time, so an existing diff-shaped caller need not
	// set it.
	TriggerDiff TriggerForm = "diff"
	// TriggerScheduled is a periodic, non-diff run (e.g. ADR-065's groomer).
	TriggerScheduled TriggerForm = "scheduled"
	// TriggerOnDemand is an operator- or event-initiated non-diff run (e.g.
	// ADR-053's incident intake).
	TriggerOnDemand TriggerForm = "on_demand"
)

// ValidTriggerForms returns the closed set of trigger forms, in declaration
// order. It is the accessor a consumer or doc generator reads rather than
// re-listing the constants.
func ValidTriggerForms() []TriggerForm {
	return []TriggerForm{TriggerDiff, TriggerScheduled, TriggerOnDemand}
}

// Predicate is one declarative match rule. Every field is optional, but at
// least one must be declared (Validate rejects the empty predicate). The
// json/yaml tags are the spec surface E53.3/E53.4 will $ref.
type Predicate struct {
	// Paths are doublestar globs matched against a change's paths. `**`
	// crosses `/`. Matched with doublestar.Match — the exact call the
	// test_conventions sweep makes in backend/internal/server/test_sweep.go
	// against repo-relative, slash-separated paths.
	Paths []string `json:"paths,omitempty" yaml:"paths,omitempty"`
	// Labels are issue/PR label names; the criterion is satisfied when the
	// change carries ANY of them.
	Labels []string `json:"labels,omitempty" yaml:"labels,omitempty"`
	// ChangeKinds are change-kind tokens; satisfied when the change's kind is
	// ANY of them.
	ChangeKinds []string `json:"change_kind,omitempty" yaml:"change_kind,omitempty"`
	// Triggers are the trigger forms this predicate accepts; satisfied when
	// the change's (diff-normalized) trigger is ANY of them.
	Triggers []TriggerForm `json:"trigger,omitempty" yaml:"trigger,omitempty"`
}

// Change is the subject a Predicate matches against. A non-diff change
// (Trigger scheduled or on_demand) carries no Paths, which is exactly why a
// paths-declaring predicate does NOT match it (the paths type is declared and
// unsatisfiable against zero paths — AND across types).
type Change struct {
	Paths      []string
	Labels     []string
	ChangeKind string
	Trigger    TriggerForm
}

// Validate reports whether p is a well-formed predicate, reporting at the
// JSON-pointer-style ptr the caller passes so a downstream consumer
// (E53.3/E53.4) reports at its own declaration site. It returns an error
// when: p declares NO criterion (the anti-match-all rule); any `paths` glob
// is empty or malformed (checked with doublestar.ValidatePattern, so a bad
// glob is caught at declaration time and never reaches Match); any `labels`
// or `change_kind` entry is the empty string; or any `trigger` entry is not
// one of the three valid forms. Returns nil on a valid predicate.
func (p Predicate) Validate(ptr string) error {
	if len(p.Paths) == 0 && len(p.Labels) == 0 && len(p.ChangeKinds) == 0 && len(p.Triggers) == 0 {
		return fmt.Errorf("%s: predicate declares no criterion; an empty predicate is not match-all — declare at least one of paths, labels, change_kind, or trigger", ptr)
	}
	for i, g := range p.Paths {
		if g == "" || !doublestar.ValidatePattern(g) {
			return fmt.Errorf("%s/paths/%d: malformed path glob %q", ptr, i, g)
		}
	}
	for i, l := range p.Labels {
		if l == "" {
			return fmt.Errorf("%s/labels/%d: label is empty", ptr, i)
		}
	}
	for i, ck := range p.ChangeKinds {
		if ck == "" {
			return fmt.Errorf("%s/change_kind/%d: change_kind is empty", ptr, i)
		}
	}
	for i, tf := range p.Triggers {
		if !validTriggerForm(tf) {
			return fmt.Errorf("%s/trigger/%d: unknown trigger form %q; valid forms are %s", ptr, i, tf, triggerFormsList())
		}
	}
	return nil
}

// Match reports whether c satisfies p under the ratified semantics: AND
// across DECLARED criteria types, OR within each list; an undeclared type
// does not constrain. It FAILS CLOSED: a doublestar.Match error (reachable
// only if a caller skipped Validate) returns (false, error) naming the
// offending pattern rather than being swallowed as a non-match — the
// deliberate divergence from the test-sweep's advisory fail-open, which is
// correct for a sweep and wrong for a governance control.
func (p Predicate) Match(c Change) (bool, error) {
	if len(p.Paths) > 0 {
		ok, err := matchAnyPath(p.Paths, c.Paths)
		if err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
	}
	if len(p.Labels) > 0 && !anyStringIn(p.Labels, c.Labels) {
		return false, nil
	}
	if len(p.ChangeKinds) > 0 && !containsString(p.ChangeKinds, c.ChangeKind) {
		return false, nil
	}
	if len(p.Triggers) > 0 {
		trig := c.Trigger
		if trig == "" {
			// Back-compat seam: an unset Trigger is a diff run, so an existing
			// diff-shaped caller need not set it. Documented in the header and
			// in docs/spec/workflow-v2.md.
			trig = TriggerDiff
		}
		if !containsTriggerForm(p.Triggers, trig) {
			return false, nil
		}
	}
	return true, nil
}

// matchAnyPath reports whether ANY change path matches ANY glob via
// doublestar.Match. It fails closed on a malformed glob (returns the error);
// against zero change paths it returns (false, nil) without evaluating a
// glob, so a paths-declaring predicate correctly does NOT match a no-diff
// change.
func matchAnyPath(globs, paths []string) (bool, error) {
	for _, g := range globs {
		for _, path := range paths {
			ok, err := doublestar.Match(g, path)
			if err != nil {
				return false, fmt.Errorf("path predicate: malformed glob %q: %w", g, err)
			}
			if ok {
				return true, nil
			}
		}
	}
	return false, nil
}

// SplitNULPaths splits a `git ... -z` NUL-delimited stream into paths. git
// TERMINATES rather than separates each record, so the trailing empty record
// after the final NUL is dropped; a path containing a literal newline stays
// ONE path instead of splitting into two the way a line-based split would.
func SplitNULPaths(b []byte) []string {
	parts := strings.Split(string(b), "\x00")
	if n := len(parts); n > 0 && parts[n-1] == "" {
		parts = parts[:n-1]
	}
	return parts
}

// DecodeGitPath decodes git's C-quoted path form back to the real bytes. git
// quotes a path containing a double quote, a backslash, a control character
// (including a NEWLINE), or — unless core.quotePath=false — a non-ASCII byte,
// wrapping it in double quotes and escaping the offending bytes; the non-`-z`
// forms a caller may hand in carry this quoting. An unquoted value is
// returned verbatim. It FAILS CLOSED with an actionable error NAMING raw on a
// trailing backslash, a short or non-octal octal escape, or an unknown escape
// — the exact three modes scripts/check-coverage.py's decode_git_path raises
// PathDecodeError for — so a decode failure can never degrade to a silent
// non-match. Go strings are byte slices, so a non-UTF-8 path needs no
// surrogateescape round-trip.
func DecodeGitPath(raw string) (string, error) {
	if len(raw) < 2 || raw[0] != '"' || raw[len(raw)-1] != '"' {
		return raw, nil
	}
	body := raw[1 : len(raw)-1]
	out := make([]byte, 0, len(body))
	for i := 0; i < len(body); {
		ch := body[i]
		if ch != '\\' {
			out = append(out, ch)
			i++
			continue
		}
		i++
		if i >= len(body) {
			return "", fmt.Errorf("trailing backslash in quoted git path %q", raw)
		}
		esc := body[i]
		if esc >= '0' && esc <= '7' {
			if i+3 > len(body) || !allOctalDigits(body[i:i+3]) {
				return "", fmt.Errorf("bad octal escape in quoted git path %q", raw)
			}
			v, _ := strconv.ParseUint(body[i:i+3], 8, 16)
			out = append(out, byte(v))
			i += 3
			continue
		}
		b, ok := gitCEscapes[esc]
		if !ok {
			return "", fmt.Errorf("unknown escape \\%c in quoted git path %q", esc, raw)
		}
		out = append(out, b)
		i++
	}
	return string(out), nil
}

// DecodeGitPaths maps DecodeGitPath over raw, returning on the FIRST
// undecodable path so a decode failure can never degrade to a silent
// non-match.
func DecodeGitPaths(raw []string) ([]string, error) {
	out := make([]string, 0, len(raw))
	for _, r := range raw {
		d, err := DecodeGitPath(r)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, nil
}

// gitCEscapes is git's single-character C-style escape set (quote_c_style);
// anything else after a backslash is an octal byte escape.
var gitCEscapes = map[byte]byte{
	'a':  0x07,
	'b':  0x08,
	'f':  0x0C,
	'n':  0x0A,
	'r':  0x0D,
	't':  0x09,
	'v':  0x0B,
	'\\': 0x5C,
	'"':  0x22,
}

// allOctalDigits reports whether s is exactly three octal digits.
func allOctalDigits(s string) bool {
	if len(s) != 3 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '7' {
			return false
		}
	}
	return true
}

// validTriggerForm reports whether tf is one of the three ratified forms.
func validTriggerForm(tf TriggerForm) bool {
	switch tf {
	case TriggerDiff, TriggerScheduled, TriggerOnDemand:
		return true
	default:
		return false
	}
}

// triggerFormsList renders the valid trigger forms for an error message.
func triggerFormsList() string {
	forms := ValidTriggerForms()
	parts := make([]string, len(forms))
	for i, f := range forms {
		parts[i] = string(f)
	}
	return strings.Join(parts, ", ")
}

// anyStringIn reports whether any element of want is present in have.
func anyStringIn(want, have []string) bool {
	for _, w := range want {
		for _, h := range have {
			if w == h {
				return true
			}
		}
	}
	return false
}

// containsString reports whether list contains s.
func containsString(list []string, s string) bool {
	for _, e := range list {
		if e == s {
			return true
		}
	}
	return false
}

// containsTriggerForm reports whether list contains tf.
func containsTriggerForm(list []TriggerForm, tf TriggerForm) bool {
	for _, e := range list {
		if e == tf {
			return true
		}
	}
	return false
}
