// Package diffsecrets is the deterministic half of the diff secrets check
// (ADR-084 D5 / rule 5, E80.3 / #3760): it scans the ADDED lines of a unified
// diff for credential-shaped strings using a caller-supplied redaction pattern
// set (redaction.DefaultPatterns in production) and reports WHERE each hit is
// — path, new-side line number and pattern class — never WHAT matched.
//
// The package is pure: no I/O, no logging, no clock. The server
// (backend/internal/server/diff_secrets.go) turns the grouped hits into
// server-synthesized review concerns; this package owns only the parse and the
// location-only rendering, so the "never echo the secret" property is a
// property of the types here (Hit and Group have no field that can carry
// matched bytes) rather than a convention every caller must remember.
package diffsecrets

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"

	"github.com/kuhlman-labs/fishhawk/redaction"
)

// UnresolvedPath is the path recorded for a hit whose diff section names no
// path the parser can resolve (a malformed or truncated header). A hit is
// never dropped for want of a path: a missed secret is silent, while a hit
// attributed to a placeholder still reaches a human.
const UnresolvedPath = "(unresolved path)"

// UnknownLine is the Line recorded for a hit under a malformed `@@` hunk
// header, whose new-side start cannot be parsed. The added line is still
// scanned — only its position is lost — and Note renders it as
// "<path>:(line unknown)". No real new-side line is 0: a hunk with added
// lines starts at 1 or later.
const UnknownLine = 0

// CheckName is the check identifier carried in every check key and in the
// server's diff_secrets_detected audit payload.
const CheckName = "diff_secrets"

// MarkerToken is the in-line fixture marker (ADR-084 A2, #5230). An ADDED
// line carrying it sets Hit.Marked; Allowlist.Partition honours the marker
// only inside a declared marker scope, and every marker-suppressed hit still
// raises a low, human-only concern (MarkerCheckKey / MarkerNote).
const MarkerToken = "fishhawk:allow-secret"

// MarkerCheckName prefixes every marker-suppressed concern's check key. It
// differs from CheckName+"|", so a MarkerCheckKey never equals a CheckKey.
const MarkerCheckName = "diff_secrets_marker"

// fingerprintHexLen is the truncated hex length of every fingerprint this
// package mints (RuleFingerprint and the per-hit fingerprint): 64 bits.
const fingerprintHexLen = 16

// noteLineCap bounds how many path:line locations one concern note lists; the
// remainder is summarized as a count so a file with hundreds of hits cannot
// produce an unbounded note.
const noteLineCap = 20

// Hit is one added line matching one pattern. It deliberately carries no
// field that can hold the matched bytes: Path is the file (itself passed
// through the pattern set, so a credential-shaped FILE NAME is recorded
// redacted), Line is the new-side line number (UnknownLine under a malformed
// hunk header), Pattern is the pattern's Name.
//
// Marked is true when the ADDED line itself carries MarkerToken: it is per
// line, and context or removed lines are never consulted. PathRedacted is true
// when the pattern set changed the path (a credential-shaped FILE NAME):
// Partition never suppresses such a hit. Fingerprint is set ONLY on a Marked
// hit (K1 of the #5230 plan gate): the first 16 hex of SHA-256 over (path,
// pattern, every match of the pattern on the line). The matched bytes are
// hashed in memory and dropped; the digest is the per-hit identity
// MarkerCheckKey de-duplicates on, so a NEW self-marked line raises its own
// concern while a re-review of the same line raises nothing new.
//
// PathAmbiguous is true when the section's path cannot be trusted as an
// allowlist input (#5230 implement review): Partition never suppresses such a
// hit. Scan sets it from the first header whose path is ambiguous (see
// gitHeaderPath / plusPath / AmbiguousPath) to the END of the patch, because a
// file name carrying a newline can plant a clean-looking header after the
// anomalous one. The server also sets it on every hit when a changed path the
// patch was rebuilt from is itself ambiguous.
type Hit struct {
	Path          string
	Line          int
	Pattern       string
	Marked        bool
	PathRedacted  bool
	PathAmbiguous bool
	Fingerprint   string
}

// Result is the outcome of one Scan.
type Result struct {
	// Hits in patch order; within one line, in pattern order. One hit per
	// (line, pattern) however many times the pattern matches on that line.
	Hits []Hit
	// AddedLines counts the added (`+`) hunk lines scanned, for logging.
	AddedLines int
}

// Group is every hit of one pattern in one file.
type Group struct {
	Path    string
	Pattern string
	// Lines are the new-side line numbers, sorted ascending, de-duplicated.
	Lines []int
}

// scanState is the parser's position inside one diff section.
type scanState int

const (
	// stateHeader: between `diff --git` (or the start of input) and the first
	// `@@`. Only here are `---` / `+++` lines file headers.
	stateHeader scanState = iota
	// stateHunk: inside a hunk. Every line is content, including one whose
	// content starts with `++ ` or `-- ` (rendered `+++ ` / `--- `).
	stateHunk
)

// Scan walks patch and reports every added line matching any of patterns.
//
// The walk is a header/hunk state machine:
//   - `diff --git` opens a section and resets to header state, seeding the
//     section's path from the header's b-side (GitHub compare-reconstructed
//     patches carry no ---/+++ lines, so this is their only path source); a
//     header (or `+++` path) that resolves ambiguously sets PathAmbiguous on
//     every later hit in the patch;
//   - `+++ <path>` in header state overrides that path (unquoting a git
//     C-quoted name and stripping the `b/` prefix); `+++ /dev/null` (a
//     deletion) keeps the git-header path;
//   - `@@ -a,b +c,d @@` seeds the new-side line counter at c and enters hunk
//     state; a MALFORMED `@@` header enters (or keeps) hunk state too, with
//     the new-side line unknown until the next well-formed header, so the
//     added lines under it are still scanned and record UnknownLine;
//   - in hunk state a `+` line is scanned and advances the counter, a ` `
//     context line (or an empty line, a context line whose trailing space was
//     stripped) advances it, and `-` and `\ No newline at end of file` do not.
//
// Removed and context lines are never scanned: a credential being deleted is
// the fix, not the defect.
func Scan(patch string, patterns []redaction.Pattern) Result {
	var res Result
	state := stateHeader
	path := ""
	newLine := 0
	// lineKnown is false under a malformed `@@` header until the next
	// well-formed one: hits there record UnknownLine instead of a guess. A
	// `diff --git` needs no reset: header state discards every line until
	// the next `@@`, which sets the flag afresh.
	lineKnown := false
	// ambiguous is sticky: once one header resolves ambiguously, no later
	// section's path is trusted (a newline in a forge-reported file name can
	// inject a well-formed header after the malformed one).
	ambiguous := false
	for _, line := range strings.Split(patch, "\n") {
		if strings.HasPrefix(line, "diff --git ") {
			state = stateHeader
			p, trusted := gitHeaderPath(strings.TrimPrefix(line, "diff --git "))
			path = p
			ambiguous = ambiguous || !trusted
			newLine = 0
			continue
		}
		if strings.HasPrefix(line, "@@") {
			// A malformed header still opens (or keeps) a hunk: staying in
			// header state would DISCARD every `+` line under it, and a
			// missed secret is silent. Only the line number is lost.
			start, ok := hunkNewStart(line)
			state = stateHunk
			newLine = start
			lineKnown = ok
			continue
		}
		if state == stateHeader {
			if strings.HasPrefix(line, "+++ ") {
				p, ok, trusted := plusPath(strings.TrimPrefix(line, "+++ "))
				if ok {
					path = p
				}
				ambiguous = ambiguous || !trusted
			}
			continue
		}
		switch {
		case strings.HasPrefix(line, "+"):
			res.AddedLines++
			content := line[1:]
			at := UnknownLine
			if lineKnown {
				at = newLine
			}
			marked := strings.Contains(content, MarkerToken)
			for _, p := range patterns {
				if p.Regex != nil && p.Regex.MatchString(content) {
					shown, redacted := displayPath(path, patterns)
					h := Hit{
						Path:          shown,
						Line:          at,
						Pattern:       p.Name,
						Marked:        marked,
						PathRedacted:  redacted,
						PathAmbiguous: ambiguous,
					}
					if marked {
						h.Fingerprint = hitFingerprint(shown, p.Name, p.Regex.FindAllString(content, -1))
					}
					res.Hits = append(res.Hits, h)
				}
			}
			newLine++
		case strings.HasPrefix(line, " "), line == "":
			newLine++
		}
		// `-` lines and `\ No newline at end of file` do not advance the
		// new-side counter and are never scanned.
	}
	return res
}

// displayPath resolves the placeholder for an unresolved path and passes the
// path through the pattern set, so a credential-shaped file NAME never rides a
// hit (and from there a note, an audit payload or a log line) verbatim. The
// bool reports whether the pattern set changed the path.
func displayPath(path string, patterns []redaction.Pattern) (string, bool) {
	if path == "" {
		return UnresolvedPath, false
	}
	out, _ := redaction.Redact([]byte(path), patterns)
	return string(out), string(out) != path
}

// hitFingerprint is the per-hit identity of a marked hit: the truncated
// SHA-256 over (path, pattern, matches), NUL-separated. The matches never
// leave this function.
func hitFingerprint(path, pattern string, matches []string) string {
	h := sha256.New()
	h.Write([]byte(path))
	h.Write([]byte{0})
	h.Write([]byte(pattern))
	for _, m := range matches {
		h.Write([]byte{0})
		h.Write([]byte(m))
	}
	return hex.EncodeToString(h.Sum(nil))[:fingerprintHexLen]
}

// hunkNewStart parses the new-side start line out of a hunk header
// `@@ -a[,b] +c[,d] @@ …`.
func hunkNewStart(line string) (int, bool) {
	rest := strings.TrimPrefix(line, "@@")
	plus := strings.Index(rest, " +")
	if plus < 0 {
		return 0, false
	}
	rest = rest[plus+2:]
	end := strings.IndexAny(rest, ", @")
	if end < 0 {
		end = len(rest)
	}
	n, err := strconv.Atoi(rest[:end])
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// AmbiguousPath reports whether p cannot be trusted as the path of a diff
// section: it holds " b/" (so a `diff --git a/<x> b/<y>` header naming it has
// more than one split) or a control byte (a newline in a forge-reported name
// injects lines into a rebuilt patch). An empty p is not ambiguous.
func AmbiguousPath(p string) bool {
	if strings.Contains(p, " b/") {
		return true
	}
	for i := 0; i < len(p); i++ {
		if p[i] < 0x20 || p[i] == 0x7f {
			return true
		}
	}
	return false
}

// plusPath resolves a `+++ ` header's path. ok is false for /dev/null (a
// deleted file — the git header's path stays) and for an empty or malformed
// value. trusted is false when the value is malformed, carries text after a
// closing quote, or names an AmbiguousPath.
func plusPath(v string) (path string, ok, trusted bool) {
	if strings.HasPrefix(v, `"`) {
		p, n, ok := unquoteC(v)
		if !ok || p == "" {
			return "", false, false
		}
		p = strings.TrimPrefix(p, "b/")
		return p, true, n == len(v) && !AmbiguousPath(p)
	}
	// A plain `diff -u` appends "\t<timestamp>"; git never does.
	if tab := strings.IndexByte(v, '\t'); tab >= 0 {
		v = v[:tab]
	}
	if v == "/dev/null" {
		return "", false, true
	}
	if v == "" {
		return "", false, false
	}
	p := strings.TrimPrefix(v, "b/")
	return p, true, !AmbiguousPath(p)
}

// gitHeaderPath resolves the b-side path of a `diff --git a/<p> b/<p>` header
// (rest is the text after "diff --git "). It handles the C-quoted forms git
// emits for unusual names, and the space-in-name ambiguity of the unquoted
// form: for a non-rename both sides name the same path, so the header is
// exactly "a/" + p + " b/" + p and p is recovered by length; otherwise the
// LAST " b/" separates the sides. Returns "" when nothing resolves.
//
// trusted is false when the split is not unique or the header is not one git
// emits: nothing resolves, text follows a closing quote, an unquoted side
// holds " b/" (a rename whose path holds " b/" resolves by the last-" b/"
// rule to a TAIL an attacker chooses), or the path is an AmbiguousPath.
func gitHeaderPath(rest string) (path string, trusted bool) {
	if strings.HasPrefix(rest, `"`) {
		// Quoted a-side: skip it, then read the b-side (quoted or not).
		_, n, ok := unquoteC(rest)
		if !ok {
			return "", false
		}
		b := strings.TrimPrefix(rest[n:], " ")
		if strings.HasPrefix(b, `"`) {
			p, m, ok := unquoteC(b)
			if !ok {
				return "", false
			}
			p = strings.TrimPrefix(p, "b/")
			return p, m == len(b) && p != "" && !AmbiguousPath(p)
		}
		p := strings.TrimPrefix(b, "b/")
		return p, strings.HasPrefix(b, "b/") && p != "" && !AmbiguousPath(p)
	}
	if strings.HasSuffix(rest, `"`) {
		// Unquoted a-side, quoted b-side.
		if i := strings.LastIndex(rest, ` "b/`); i >= 0 {
			p, m, ok := unquoteC(rest[i+1:])
			if ok {
				p = strings.TrimPrefix(p, "b/")
				return p, m == len(rest)-i-1 && !strings.Contains(rest[:i], " b/") && p != "" && !AmbiguousPath(p)
			}
		}
		return "", false
	}
	if n := len(rest); n >= 5 && (n-5)%2 == 0 && strings.HasPrefix(rest, "a/") {
		lp := (n - 5) / 2
		p := rest[2 : 2+lp]
		if rest[2+lp:2+lp+3] == " b/" && rest[2+lp+3:] == p {
			return p, p != "" && !AmbiguousPath(p)
		}
	}
	if i := strings.LastIndex(rest, " b/"); i >= 0 {
		p := rest[i+3:]
		// A rename: trusted only when " b/" occurs once, so the split is unique.
		return p, strings.HasPrefix(rest, "a/") && strings.Count(rest, " b/") == 1 && p != "" && !AmbiguousPath(p)
	}
	return "", false
}

// unquoteC decodes a git C-quoted string starting at s[0] == '"'. It returns
// the decoded value, the number of bytes of s consumed (through the closing
// quote) and whether the quoting was well-formed.
func unquoteC(s string) (string, int, bool) {
	if !strings.HasPrefix(s, `"`) {
		return "", 0, false
	}
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		c := s[i]
		switch c {
		case '"':
			return b.String(), i + 1, true
		case '\\':
			i++
			if i >= len(s) {
				return "", 0, false
			}
			switch e := s[i]; e {
			case 'a':
				b.WriteByte('\a')
			case 'b':
				b.WriteByte('\b')
			case 't':
				b.WriteByte('\t')
			case 'n':
				b.WriteByte('\n')
			case 'v':
				b.WriteByte('\v')
			case 'f':
				b.WriteByte('\f')
			case 'r':
				b.WriteByte('\r')
			case '"', '\\':
				b.WriteByte(e)
			default:
				if e < '0' || e > '7' || i+2 >= len(s) {
					return "", 0, false
				}
				v, err := strconv.ParseUint(s[i:i+3], 8, 8)
				if err != nil {
					return "", 0, false
				}
				b.WriteByte(byte(v))
				i += 2
			}
		default:
			b.WriteByte(c)
		}
	}
	return "", 0, false
}

// GroupHits folds hits by (path, pattern) with sorted, de-duplicated lines.
// Groups are ordered by path, then pattern, so the output is deterministic
// whatever order the hits arrived in.
func GroupHits(hits []Hit) []Group {
	type key struct{ path, pattern string }
	idx := map[key]int{}
	var groups []Group
	for _, h := range hits {
		k := key{h.Path, h.Pattern}
		i, ok := idx[k]
		if !ok {
			i = len(groups)
			idx[k] = i
			groups = append(groups, Group{Path: h.Path, Pattern: h.Pattern})
		}
		groups[i].Lines = append(groups[i].Lines, h.Line)
	}
	for i := range groups {
		sort.Ints(groups[i].Lines)
		groups[i].Lines = uniqueSorted(groups[i].Lines)
	}
	sort.Slice(groups, func(i, j int) bool {
		if groups[i].Path != groups[j].Path {
			return groups[i].Path < groups[j].Path
		}
		return groups[i].Pattern < groups[j].Pattern
	})
	return groups
}

func uniqueSorted(in []int) []int {
	out := in[:0]
	for i, v := range in {
		if i == 0 || v != in[i-1] {
			out = append(out, v)
		}
	}
	return out
}

// CheckKey is the de-duplication key the server stores on the concern row:
// "diff_secrets|<pattern>|<path>". It is injective because no pattern Name
// contains '|' (pinned by TestCheckKey_Injective over DefaultPatterns), so the
// first '|' after the check name always ends the pattern field. Line numbers
// are deliberately NOT part of the key: they shift between fix-up passes and
// would re-raise every concern a human already waived.
func CheckKey(pattern, path string) string {
	return CheckName + "|" + pattern + "|" + path
}

// Key is CheckKey for the group.
func (g Group) Key() string { return CheckKey(g.Pattern, g.Path) }

// Note renders the group's concern note: the pattern class and the path:line
// locations only. The matched value is never an input, so it cannot be an
// output.
func Note(g Group) string {
	return "Diff secrets check: an added line matches the credential pattern " + g.Pattern +
		" at " + locations(g.Path, g.Lines) + ". This concern was raised by the server's deterministic diff secrets check, not by a model reviewer; " +
		"the matched value is deliberately not recorded. Remove the credential (and rotate it if it was ever live), " +
		"or have a human waive this concern with a reason (for example, a known test fixture). " +
		"Agents and delegated actions cannot waive or defer it."
}

// locations renders path:line for at most noteLineCap lines, the remainder
// counted, an UnknownLine as "(line unknown)".
func locations(path string, lines []int) string {
	locs := make([]string, 0, len(lines))
	for i, l := range lines {
		if i == noteLineCap {
			break
		}
		at := strconv.Itoa(l)
		if l == UnknownLine {
			at = "(line unknown)"
		}
		locs = append(locs, path+":"+at)
	}
	where := strings.Join(locs, ", ")
	if extra := len(lines) - len(locs); extra > 0 {
		where += " and " + strconv.Itoa(extra) + " more line(s) in the same file"
	}
	return where
}

// MarkerCheckKey is the de-duplication key of a marker-suppressed concern:
// "diff_secrets_marker|<pattern>|<rule_id>@<rule_fingerprint>|<hit_fingerprint>|<path>".
// It is injective: pattern names carry no '|' (TestCheckKey_Injective), rule
// ids are [a-z0-9-] slugs, both fingerprints are hex, and the path is last.
// The distinct MarkerCheckName prefix keeps it apart from every CheckKey. The
// per-hit fingerprint (K1) makes the key per HIT, not per (pattern, rule,
// path): a new marked line raises again after an earlier one was waived.
func MarkerCheckKey(pattern, ruleID, ruleFingerprint, hitFingerprint, path string) string {
	return MarkerCheckName + "|" + pattern + "|" + ruleID + "@" + ruleFingerprint + "|" + hitFingerprint + "|" + path
}

// MarkerNote renders a marker-suppressed concern's note. It is location-only
// (path:lines, capped like Note) and names the pattern class, the scope's
// SafeRuleID and its fingerprint; the matched value is never an input.
func MarkerNote(rec SuppressionRecord) string {
	return "Diff secrets check: an added line carrying the fixture marker " + MarkerToken +
		" matches the credential pattern " + rec.Pattern + " at " + locations(rec.Path, rec.Lines) +
		", inside the allowlist marker scope " + SafeRuleID(rec.RuleID) + " (rule " + SafeFingerprint(rec.RuleFingerprint) + "). " +
		"The marker was added in this change, so the hit was suppressed instead of raised as a high concern; " +
		"a human must confirm each marked line is a test fixture and not a live credential. " +
		"This concern is low severity and does not block the human merge verb. It was raised by the server's deterministic " +
		"diff secrets check, not by a model reviewer, and the matched value is deliberately not recorded. " +
		"Only a human can waive it, with a reason; agents and delegated actions cannot waive or defer it."
}
