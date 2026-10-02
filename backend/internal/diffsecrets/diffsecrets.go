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

// CheckName is the check identifier carried in every check key and in the
// server's diff_secrets_detected audit payload.
const CheckName = "diff_secrets"

// noteLineCap bounds how many path:line locations one concern note lists; the
// remainder is summarized as a count so a file with hundreds of hits cannot
// produce an unbounded note.
const noteLineCap = 20

// Hit is one added line matching one pattern. It deliberately carries no
// field that can hold the matched bytes: Path is the file (itself passed
// through the pattern set, so a credential-shaped FILE NAME is recorded
// redacted), Line is the new-side line number, Pattern is the pattern's Name.
type Hit struct {
	Path    string
	Line    int
	Pattern string
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
//     patches carry no ---/+++ lines, so this is their only path source);
//   - `+++ <path>` in header state overrides that path (unquoting a git
//     C-quoted name and stripping the `b/` prefix); `+++ /dev/null` (a
//     deletion) keeps the git-header path;
//   - `@@ -a,b +c,d @@` seeds the new-side line counter at c and enters hunk
//     state;
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
	for _, line := range strings.Split(patch, "\n") {
		if strings.HasPrefix(line, "diff --git ") {
			state = stateHeader
			path = gitHeaderPath(strings.TrimPrefix(line, "diff --git "))
			newLine = 0
			continue
		}
		if strings.HasPrefix(line, "@@") {
			if start, ok := hunkNewStart(line); ok {
				state = stateHunk
				newLine = start
			}
			continue
		}
		if state == stateHeader {
			if strings.HasPrefix(line, "+++ ") {
				if p, ok := plusPath(strings.TrimPrefix(line, "+++ ")); ok {
					path = p
				}
			}
			continue
		}
		switch {
		case strings.HasPrefix(line, "+"):
			res.AddedLines++
			content := line[1:]
			for _, p := range patterns {
				if p.Regex != nil && p.Regex.MatchString(content) {
					res.Hits = append(res.Hits, Hit{
						Path:    displayPath(path, patterns),
						Line:    newLine,
						Pattern: p.Name,
					})
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
// hit (and from there a note, an audit payload or a log line) verbatim.
func displayPath(path string, patterns []redaction.Pattern) string {
	if path == "" {
		return UnresolvedPath
	}
	out, _ := redaction.Redact([]byte(path), patterns)
	return string(out)
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

// plusPath resolves a `+++ ` header's path. ok is false for /dev/null (a
// deleted file — the git header's path stays) and for an empty value.
func plusPath(v string) (string, bool) {
	if strings.HasPrefix(v, `"`) {
		p, _, ok := unquoteC(v)
		if !ok {
			return "", false
		}
		return strings.TrimPrefix(p, "b/"), p != ""
	}
	// A plain `diff -u` appends "\t<timestamp>"; git never does.
	if tab := strings.IndexByte(v, '\t'); tab >= 0 {
		v = v[:tab]
	}
	if v == "" || v == "/dev/null" {
		return "", false
	}
	return strings.TrimPrefix(v, "b/"), true
}

// gitHeaderPath resolves the b-side path of a `diff --git a/<p> b/<p>` header
// (rest is the text after "diff --git "). It handles the C-quoted forms git
// emits for unusual names, and the space-in-name ambiguity of the unquoted
// form: for a non-rename both sides name the same path, so the header is
// exactly "a/" + p + " b/" + p and p is recovered by length; otherwise the
// LAST " b/" separates the sides. Returns "" when nothing resolves.
func gitHeaderPath(rest string) string {
	if strings.HasPrefix(rest, `"`) {
		// Quoted a-side: skip it, then read the b-side (quoted or not).
		_, n, ok := unquoteC(rest)
		if !ok {
			return ""
		}
		b := strings.TrimPrefix(rest[n:], " ")
		if strings.HasPrefix(b, `"`) {
			p, _, ok := unquoteC(b)
			if !ok {
				return ""
			}
			return strings.TrimPrefix(p, "b/")
		}
		return strings.TrimPrefix(b, "b/")
	}
	if strings.HasSuffix(rest, `"`) {
		// Unquoted a-side, quoted b-side.
		if i := strings.LastIndex(rest, ` "b/`); i >= 0 {
			p, _, ok := unquoteC(rest[i+1:])
			if ok {
				return strings.TrimPrefix(p, "b/")
			}
		}
		return ""
	}
	if n := len(rest); n >= 5 && (n-5)%2 == 0 && strings.HasPrefix(rest, "a/") {
		lp := (n - 5) / 2
		p := rest[2 : 2+lp]
		if rest[2+lp:2+lp+3] == " b/" && rest[2+lp+3:] == p {
			return p
		}
	}
	if i := strings.LastIndex(rest, " b/"); i >= 0 {
		return rest[i+3:]
	}
	return ""
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
	locs := make([]string, 0, len(g.Lines))
	for i, l := range g.Lines {
		if i == noteLineCap {
			break
		}
		locs = append(locs, g.Path+":"+strconv.Itoa(l))
	}
	where := strings.Join(locs, ", ")
	if extra := len(g.Lines) - len(locs); extra > 0 {
		where += " and " + strconv.Itoa(extra) + " more line(s) in the same file"
	}
	return "Diff secrets check: an added line matches the credential pattern " + g.Pattern +
		" at " + where + ". This concern was raised by the server's deterministic diff secrets check, not by a model reviewer; " +
		"the matched value is deliberately not recorded. Remove the credential (and rotate it if it was ever live), " +
		"or have a human waive this concern with a reason (for example, a known test fixture). " +
		"Agents and delegated actions cannot waive or defer it."
}
