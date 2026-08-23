package intakegroom

import (
	"regexp"
	"sort"
	"strings"
)

// epicTitle matches an epic's leading conventions token — "[E54]" with NO
// child suffix. "[E54.7]" is a child of that epic and must never be
// suggested as a parent, which is exactly what the closing bracket
// immediately after the digits enforces.
var epicTitle = regexp.MustCompile(`^\s*\[E\d+\]`)

// EpicThreshold is the minimum score at which a parent epic is suggested.
//
// It is deliberately BELOW the duplicate floor (ThresholdLow): an epic title
// is short and thematic ("[E54] Backlog grooming") while a child title is
// long and specific, so their token overlap is structurally small even when
// the parentage is obvious. Requiring duplicate-grade overlap here would
// mean never suggesting anything.
const EpicThreshold = 0.15

// areaLabelBonus is added when the epic and the filing share an area:* label.
const areaLabelBonus = 0.10

// isEpic reports whether a candidate is an epic: either its title carries a
// childless [E<n>] token, or it is labelled type:epic.
func isEpic(c Candidate) bool {
	if epicTitle.MatchString(c.Title) {
		return true
	}
	return labelValue(c.Labels, "type") == "epic"
}

// SuggestEpic returns the single best parent-epic candidate for f, or nil
// when nothing scores above EpicThreshold.
//
// Nil is a real answer, not an absence: the caller renders "none" explicitly
// so a reader can tell "we looked and found nothing" from "we did not look".
// Ties break on the lower issue number, so the output is deterministic.
func SuggestEpic(f Filing, candidates []Candidate) *EpicSuggestion {
	ftokens := tokenize(f.Title)
	if len(ftokens) == 0 {
		return nil
	}
	type scored struct {
		c      Candidate
		score  float64
		shared []string
	}
	best := make([]scored, 0, 4)
	for _, c := range candidates {
		if !isEpic(c) {
			continue
		}
		score, shared := jaccard(ftokens, tokenize(c.Title))
		if sharesNamespace(f.Labels, c.Labels, "area") {
			score += areaLabelBonus
		}
		if score > 1 {
			score = 1
		}
		if score < EpicThreshold {
			continue
		}
		best = append(best, scored{c: c, score: score, shared: shared})
	}
	if len(best) == 0 {
		return nil
	}
	sort.SliceStable(best, func(i, j int) bool {
		if best[i].score != best[j].score {
			return best[i].score > best[j].score
		}
		return best[i].c.Number < best[j].c.Number
	})
	top := best[0]
	band := confidenceFor(top.score)
	if band == "" {
		// Above EpicThreshold but below the duplicate floor: still a real
		// suggestion, reported at the lowest band rather than dropped.
		band = ConfidenceLow
	}
	return &EpicSuggestion{
		Number:     top.c.Number,
		URL:        top.c.URL,
		Title:      top.c.Title,
		Score:      top.score,
		Confidence: band,
		Basis:      strings.Join(top.shared, ", "),
	}
}
