package workmgmt

import (
	"regexp"
	"strconv"
	"strings"
)

// childNumberPlaceholderRE matches a `{name}` placeholder in a title_format,
// mirroring the github provider's titlePlaceholderRE so the child-number
// regexp is built the same way the numbered-type discovery regexp is.
var childNumberPlaceholderRE = regexp.MustCompile(`\{([a-zA-Z0-9_]+)\}`)

// NextChildNumber computes the next child ordinal {n} for a child type whose
// title_format carries an [E<epic>.<n>] shape, given the epic's already-filed
// children (open AND closed) enumerated via EpicChildrenQuerier (#1958). It is
// the pure, provider-free allocation half of server-side {n} discovery — the
// Apply-purity precedent from allocateNumber (#1269): the server hook fetches
// the children and this function decides the number, so allocation stays
// unit-testable and no provider I/O leaks in.
//
// It builds an anchored regexp from titleFormat with the resolved epic value
// substituted as a literal, scans every child title, collects the {n} values
// that match, and returns (max+1, true). Anchoring at ^ plus the literal '.'
// after the epic value means an epic "7" can never be confused with "[E17.x]",
// "[E70.x]", or the bare "[E7]" epic title itself. Non-conforming child titles
// are skipped, and gaps are tolerated ([E7.1],[E7.5] -> 6), so the number is
// always max+1 over the matched set, never a count.
//
// The second return value reports whether allocation is UNAMBIGUOUS, and the
// function fails CLOSED when it is not — mirroring #1265's empty-list guard for
// ADR numbering. It distinguishes a genuinely-EMPTY child set from a NON-EMPTY
// set with zero numbered matches:
//   - regexp nil (no {n} placeholder or a non-compiling title_format, a
//     caller-guarded / config case): (1, true), unchanged behavior.
//   - zero matches AND len(children) == 0 (a genuinely-first child): (1, true).
//   - zero matches AND len(children) > 0 (children exist but none carry the
//     numbered [E<epic>.<n>] form — the ambiguous zero-match, e.g. epic #389's
//     placeholder [E22.X] corpus): (0, false). Allocating 1 here would collide
//     with an existing child, so the caller must fail closed and ask for an
//     explicit n. A title that matches the regexp but whose digit run overflows
//     strconv.Atoi is NOT counted as a match (so it cannot yield a spurious
//     (1, true)), yet still counts toward "children exist".
//   - one or more matches: (max+1, true).
//
// The collision-free claim is within one deployment, not globally impossible:
// two concurrent omitted-n filings against the same epic are serialized by the
// caller's per-epic in-process lock (server/workitems.go), and hosted
// multi-instance deployments would need a Postgres advisory lock (see the
// workmgmt README).
func NextChildNumber(titleFormat, epic string, children []EpicChild) (int, bool) {
	re := childNumberRegexp(titleFormat, epic)
	if re == nil {
		return 1, true
	}
	max := 0
	matched := 0
	for _, c := range children {
		m := re.FindStringSubmatch(c.Title)
		if m == nil {
			continue
		}
		n, err := strconv.Atoi(m[1])
		if err != nil {
			// A digit run that overflows Atoi (or is otherwise unparseable) is
			// NOT a numbered match — counting it could yield a spurious
			// (1, true) with max==0. It still counts toward len(children) > 0.
			continue
		}
		matched++
		if n > max {
			max = n
		}
	}
	if matched == 0 {
		if len(children) == 0 {
			// Genuinely-first child of an empty epic: allocate 1.
			return 1, true
		}
		// Children exist but none carry the numbered form: allocating 1 would
		// collide. Fail closed — the caller must supply n explicitly.
		return 0, false
	}
	return max + 1, true
}

// childNumberRegexp builds an anchored regexp from a child type's title_format
// that captures the integer substituted for {n}. Literal segments are
// QuoteMeta-escaped, {epic} is replaced by the QuoteMeta'd resolved epic value
// (so an epic "7" matches only [E7.…], never [E17.…]/[E70.…]), {n} becomes a
// (\d+) capture group, and any other {placeholder} (e.g. {summary}) becomes
// .*? so the whole title shape is matched. It anchors at ^ so a stray leading
// token cannot smuggle a false number. Returns nil if the format references no
// {n} placeholder or the assembled pattern fails to compile (either yields the
// first-child allocation of 1 in NextChildNumber).
func childNumberRegexp(format, epic string) *regexp.Regexp {
	if !strings.Contains(format, "{n}") {
		return nil
	}
	var b strings.Builder
	b.WriteString("^")
	last := 0
	for _, loc := range childNumberPlaceholderRE.FindAllStringSubmatchIndex(format, -1) {
		b.WriteString(regexp.QuoteMeta(format[last:loc[0]]))
		name := format[loc[2]:loc[3]]
		switch name {
		case "epic":
			b.WriteString(regexp.QuoteMeta(epic))
		case "n":
			b.WriteString(`(\d+)`)
		default:
			b.WriteString(`.*?`)
		}
		last = loc[1]
	}
	b.WriteString(regexp.QuoteMeta(format[last:]))
	re, err := regexp.Compile(b.String())
	if err != nil {
		return nil
	}
	return re
}
