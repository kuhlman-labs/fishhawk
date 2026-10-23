package workmgmt

import (
	"regexp"
	"sort"
	"strings"
)

// epicTitleType is the conventions type whose title_format names an epic. The
// epic-title parse reads ITS format, so a repository that declares a different
// epic title shape parses its own parents — nothing here assumes a bracketed
// "E<n>" token (ADR-110 guard (i), #4626).
const epicTitleType = "epic"

// EpicTitleNumber extracts the epic number from title using the epic type's
// declared title_format (conv.Types["epic"].TitleFormat), the derived
// replacement for a hard-coded epic-title regexp. It follows
// childNumberRegexp's QuoteMeta precedent: the pattern is anchored at `^\s*`,
// the format's literal text before {number} is QuoteMeta-escaped (any other
// placeholder before {number} becomes `.*?`), and {number} becomes the
// `(\d+)` capture.
//
// closed selects the bracket-CLOSED form. When true, the literal text after
// {number} up to the next placeholder (trailing whitespace trimmed) must
// follow the digit run, so a CHILD title whose digit run continues (".3]")
// cannot pass as its epic (#2179). When that literal trims to nothing, a
// whitespace-or-end boundary is required instead. The open form (closed
// false) stops at the digit run, so a parent title carrying a child-shaped
// token still yields its leading number.
//
// It returns ok=false when the conventions declare no epic type, the epic
// format carries no {number} placeholder, or title does not match.
func EpicTitleNumber(conv Conventions, title string, closed bool) (string, bool) {
	re := epicTitleRegexp(conv, closed)
	if re == nil {
		return "", false
	}
	m := re.FindStringSubmatch(title)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// epicTitleRegexp builds EpicTitleNumber's anchored pattern, or nil when the
// conventions declare no epic type or its format has no {number}. Exposed to
// the package's tests so the exact pattern for today's format is pinned.
func epicTitleRegexp(conv Conventions, closed bool) *regexp.Regexp {
	it, ok := conv.Types[epicTitleType]
	if !ok {
		return nil
	}
	format := strings.TrimLeft(it.TitleFormat, " \t")
	if !strings.Contains(format, "{number}") {
		return nil
	}
	var b strings.Builder
	b.WriteString(`^\s*`)
	last := 0
	locs := childNumberPlaceholderRE.FindAllStringSubmatchIndex(format, -1)
	for i, loc := range locs {
		b.WriteString(regexp.QuoteMeta(format[last:loc[0]]))
		last = loc[1]
		if format[loc[2]:loc[3]] != "number" {
			b.WriteString(`.*?`)
			continue
		}
		b.WriteString(`(\d+)`)
		if closed {
			end := len(format)
			if i+1 < len(locs) {
				end = locs[i+1][0]
			}
			if term := strings.TrimRight(format[last:end], " \t"); term != "" {
				b.WriteString(regexp.QuoteMeta(term))
			} else {
				b.WriteString(`(?:\s|$)`)
			}
		}
		break
	}
	re, err := regexp.Compile(b.String())
	if err != nil {
		return nil
	}
	return re
}

// TypeLabelNamespaces returns the label namespaces an item type declares: the
// sorted, de-duplicated union of its required_label_namespaces, the keys of
// its label_defaults, and the namespace (the text before the first ':') of
// each colon-bearing default_labels entry. A bare default label ("adr",
// "epic") names no namespace. Empty namespaces are dropped. The result is
// never nil.
func TypeLabelNamespaces(it ItemType) []string {
	set := map[string]bool{}
	add := func(ns string) {
		if ns = strings.TrimSpace(ns); ns != "" {
			set[ns] = true
		}
	}
	for _, ns := range it.RequiredLabelNamespaces {
		add(ns)
	}
	for ns := range it.LabelDefaults {
		add(ns)
	}
	for _, l := range it.DefaultLabels {
		if ns, _, ok := strings.Cut(l, ":"); ok {
			add(ns)
		}
	}
	out := make([]string, 0, len(set))
	for ns := range set {
		out = append(out, ns)
	}
	sort.Strings(out)
	return out
}
