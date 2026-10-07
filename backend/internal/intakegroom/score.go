package intakegroom

import (
	"fmt"
	"regexp"
	"strings"
)

// rubricRow matches a charter §4 rubric table row: a leading cell holding a
// bolded id, then the line text. That is the shipped shape
// ("| **V1** | Directly unblocks ... |"), and matching it exactly is what
// keeps a prose paragraph mentioning "S4" from being read as a rubric line.
var rubricRow = regexp.MustCompile(`^\s*\|\s*\*\*([A-Za-z]+[0-9]+)\*\*\s*\|\s*(.+?)\s*\|\s*$`)

// Rubric is the set of rubric line ids parsed from the charter, each mapped
// to its line text for citation quoting. The zero Rubric is empty and safe:
// Has reports false for every id, which is what makes an unparsed charter
// drop every citation rather than emit unquoted ones.
type Rubric struct {
	ids   []string
	lines map[string]string
}

// Len returns how many rubric lines were parsed.
func (r Rubric) Len() int { return len(r.ids) }

// IDs returns the parsed ids in charter order.
func (r Rubric) IDs() []string {
	out := make([]string, len(r.ids))
	copy(out, r.ids)
	return out
}

// Has reports whether id is declared in the charter.
func (r Rubric) Has(id string) bool {
	_, ok := r.lines[id]
	return ok
}

// Quote returns the charter's text for id, or "" when it is not declared.
func (r Rubric) Quote(id string) string { return r.lines[id] }

// ParseRubricIDs extracts the rubric lines from a charter document.
//
// A document with no parsable rows returns an empty Rubric — which the
// caller maps to DegradeReasonCharterRubricUnparsed. It never guesses: the
// charter is authoritative about which ids exist (charter §6, "retire ids,
// never recycle them"), so an id this code knows about but the charter does
// not declare is an id that must not be cited.
func ParseRubricIDs(charterMarkdown string) Rubric {
	ids, lines := parseCharterLines(charterMarkdown, rubricRow)
	if len(ids) == 0 {
		return Rubric{}
	}
	return Rubric{ids: ids, lines: lines}
}

// nonGoalBullet matches a charter §3 non-goal bullet: a list dash, then a bold
// span that OPENS with an N-id, a dash separator (em dash, en dash or hyphen,
// spaced) and the statement. That is the shipped shape
// ("- **N3 — Fishhawk is not a CI/CD platform.** It runs on ..."). The id
// capture is anchored to N then digits, and the bullet + bold anchor is what
// keeps running prose that mentions "N3", and a rubric table row, from being
// read as a non-goal.
var nonGoalBullet = regexp.MustCompile(`^\s*-\s+\*\*(N[0-9]+)\s+[—–-]\s+(.+?)\*\*`)

// NonGoals is the set of non-goal ids parsed from the charter's §3 bullets,
// each mapped to its bolded statement for quoting. The zero NonGoals is empty
// and safe: Has reports false for every id.
//
// Intake scoring does not read it. It exists for the comms scan (#4014),
// which renders the non-goals so an n_drift entry can cite the id it drifts
// toward — and, like the rubric, may cite only an id the charter declares.
type NonGoals struct {
	ids   []string
	lines map[string]string
}

// Len returns how many non-goals were parsed.
func (n NonGoals) Len() int { return len(n.ids) }

// IDs returns the parsed ids in charter order.
func (n NonGoals) IDs() []string {
	out := make([]string, len(n.ids))
	copy(out, n.ids)
	return out
}

// Has reports whether id is declared as a non-goal in the charter.
func (n NonGoals) Has(id string) bool {
	_, ok := n.lines[id]
	return ok
}

// Quote returns the charter's bolded statement for id, or "" when it is not
// declared.
func (n NonGoals) Quote(id string) string { return n.lines[id] }

// ParseNonGoals extracts the non-goal bullets from a charter document.
//
// It matches the bullet shape anywhere in the document (the section heading
// is not part of the contract, so renumbering §3 does not silently empty
// it), keeps charter order, and lets the FIRST definition of an id win. A
// document with no matching bullet returns the zero NonGoals; it never
// guesses an id the charter does not declare.
func ParseNonGoals(charterMarkdown string) NonGoals {
	ids, lines := parseCharterLines(charterMarkdown, nonGoalBullet)
	if len(ids) == 0 {
		return NonGoals{}
	}
	return NonGoals{ids: ids, lines: lines}
}

// parseCharterLines is the one line-matching loop both charter parsers share:
// re's first group is the id and its second the quoted text. It keeps charter
// order and the first definition of a duplicated id.
func parseCharterLines(charterMarkdown string, re *regexp.Regexp) ([]string, map[string]string) {
	var ids []string
	lines := make(map[string]string)
	for _, line := range strings.Split(charterMarkdown, "\n") {
		m := re.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		id, text := m[1], strings.TrimSpace(m[2])
		if _, dup := lines[id]; dup {
			continue
		}
		ids = append(ids, id)
		lines[id] = text
	}
	return ids, lines
}

// The fixed per-rule weights. They are advisory-only (charter §5.1 says to
// cite rather than blend), and exist so a list of filings sorts sensibly:
// a probable duplicate outranks a structural gap, which outranks an item
// that blocks nothing.
const (
	weightS2 = 2.0
	weightS4 = 1.5
	weightU4 = 0.5
)

// structuralRule is one decidable scoring rule: a rubric id, the weight it
// contributes, and a predicate over the filing that returns the note
// explaining what fired it (or ok=false when it does not fire).
type structuralRule struct {
	id     string
	weight float64
	fire   func(f Filing, dups []DuplicateCandidate) (note string, ok bool)
}

// structuralRules are the three charter lines decidable from a filing's own
// STRUCTURE, in citation order.
//
// The set is deliberately small. Every other rubric line (the whole V and R
// groups, and S1/S3/S5) requires semantic judgement about what the item
// means and where the phase stands — that is the periodic sweep's job
// (#2236), and inventing a structural proxy for it would produce confident
// citations nobody could defend.
var structuralRules = []structuralRule{
	{
		id:     "S2",
		weight: weightS2,
		fire: func(_ Filing, dups []DuplicateCandidate) (string, bool) {
			for _, d := range dups {
				if d.Confidence.AtLeastMedium() {
					return fmt.Sprintf("a %s-confidence duplicate candidate was found (#%d)", d.Confidence, d.Number), true
				}
			}
			return "", false
		},
	},
	{
		id:     "S4",
		weight: weightS4,
		fire: func(f Filing, _ []DuplicateCandidate) (string, bool) {
			var missing []string
			if len(f.MissingLabelNamespaces) > 0 {
				missing = append(missing, "missing label namespaces: "+strings.Join(f.MissingLabelNamespaces, ", "))
			}
			if strings.TrimSpace(f.ParentEpicRef) == "" {
				missing = append(missing, "no parent epic linked")
			}
			if len(missing) == 0 {
				return "", false
			}
			return strings.Join(missing, "; "), true
		},
	},
	{
		id:     "U4",
		weight: weightU4,
		fire: func(f Filing, _ []DuplicateCandidate) (string, bool) {
			if len(f.DependsOn) > 0 {
				return "", false
			}
			return "no depends_on edge declared", true
		},
	},
}

// ScoreFiling applies the decidable structural rules to a filing and returns
// the provisional charter-anchored score.
//
// It is named ScoreFiling rather than Score because Score is the result type.
//
// Two properties are load-bearing:
//
//   - NO FABRICATED CITATION. A rule whose id is absent from the parsed
//     charter drops its citation AND its weight — the charter is
//     authoritative, and a retired id must never be cited (charter §6).
//   - A GAP IS A FINDING. When no citation survives, the result is
//     Unscored with the reason, never a synthesized citation invented to
//     satisfy the shape (charter §6.6).
//
// It takes the whole Charter rather than only its Rubric so the gap can name
// the CAUSE of an empty rubric: a charter that was never read is a different
// operator problem from one that was read and carries no rubric table, and
// reporting both as "the parser found nothing" blames a healthy parser for a
// document that was never fetched (#2827).
func ScoreFiling(f Filing, dups []DuplicateCandidate, c Charter) Score {
	r := c.RubricIDs
	var s Score
	var firedButUndeclared []string
	for _, rule := range structuralRules {
		note, ok := rule.fire(f, dups)
		if !ok {
			continue
		}
		if !r.Has(rule.id) {
			firedButUndeclared = append(firedButUndeclared, rule.id)
			continue
		}
		s.Citations = append(s.Citations, Citation{
			RubricID: rule.id,
			Quote:    r.Quote(rule.id),
			Note:     note,
		})
		s.Value += rule.weight
	}
	if len(s.Citations) > 0 {
		return s
	}
	s.Unscored = true
	path := strings.TrimSpace(c.Path)
	switch {
	// The three empty-rubric arms name three distinct causes. Only the last
	// one is about the PARSER; the first two are about a read that did not
	// happen, and conflating them is the #2827 defect.
	case r.Len() == 0 && !c.Resolved && path != "":
		s.CharterGap = fmt.Sprintf("the charter at %s was not read, so no citation is available", path)
	case r.Len() == 0 && !c.Resolved:
		s.CharterGap = "no charter is declared for this repository, so no citation is available"
	case r.Len() == 0:
		s.CharterGap = fmt.Sprintf("no rubric lines could be parsed from %s, so no citation is available", charterLabel(path))
	case len(firedButUndeclared) > 0:
		s.CharterGap = fmt.Sprintf("the structural rules that fired (%s) cite rubric ids the charter does not declare", strings.Join(firedButUndeclared, ", "))
	default:
		s.CharterGap = "no decidable structural rubric line fires for this filing; a semantic ranking is the periodic sweep's job"
	}
	return s
}

// charterLabel names a RESOLVED charter in a gap string. A resolved charter
// always carries the path it was read from; the fallback exists so a
// hand-constructed Charter cannot render a sentence with a hole in it.
func charterLabel(path string) string {
	if path == "" {
		return "the charter"
	}
	return path
}
