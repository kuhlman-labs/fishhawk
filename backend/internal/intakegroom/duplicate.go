package intakegroom

import (
	"regexp"
	"sort"
	"strings"
)

// conventionsPrefix matches the leading conventions token this repository's
// titles carry ("[E54.7] ", "[ADR-065] "). It is stripped before tokenizing
// so every filing does not appear to share the bracket's contents with every
// other filing of the same epic.
var conventionsPrefix = regexp.MustCompile(`^\s*\[[A-Za-z0-9.\-]+\]\s*`)

// nonToken matches everything that is not a token character. Splitting on it
// removes punctuation without a second pass.
var nonToken = regexp.MustCompile(`[^a-z0-9]+`)

// stopwords are dropped before scoring: generic English filler plus the
// verbs that open a large fraction of this tracker's titles. Without them a
// pair of unrelated "add X to Y" titles scores a spurious overlap.
var stopwords = map[string]bool{
	"add": true, "fix": true, "the": true, "a": true, "for": true,
	"to": true, "on": true, "in": true, "of": true, "and": true,
	"when": true, "with": true, "an": true, "is": true, "it": true,
	"be": true, "by": true, "as": true, "at": true, "or": true,
}

// typeLabelBonus is added when a candidate carries the same type:* label as
// the filing. Two items of the same type sharing vocabulary are likelier to
// be the same work than two items of different types sharing it.
const typeLabelBonus = 0.05

// tokenize reduces a title to its scoring token set: conventions prefix
// stripped, lowercased, punctuation split out, stopwords and single
// characters dropped. Returns tokens in first-seen order with duplicates
// removed, so callers get a set with a deterministic rendering order.
func tokenize(title string) []string {
	t := strings.ToLower(strings.TrimSpace(title))
	t = conventionsPrefix.ReplaceAllString(t, "")
	seen := make(map[string]bool)
	out := make([]string, 0, 8)
	for _, tok := range nonToken.Split(t, -1) {
		if len(tok) < 2 || stopwords[tok] || seen[tok] {
			continue
		}
		seen[tok] = true
		out = append(out, tok)
	}
	return out
}

// jaccard is the token-set overlap of a and b: |A∩B| / |A∪B|.
//
// An empty union — both sides tokenized to nothing, an untitled or
// fully-stopworded pair — returns 0, not the NaN a 0/0 division would
// produce. NaN is the dangerous value here: every comparison against it is
// false, so a NaN score would slip past a threshold check silently instead
// of failing loudly.
func jaccard(a, b []string) (score float64, shared []string) {
	inA := make(map[string]bool, len(a))
	for _, t := range a {
		inA[t] = true
	}
	shared = make([]string, 0, len(b))
	for _, t := range b {
		if inA[t] {
			shared = append(shared, t)
		}
	}
	union := len(a) + len(b) - len(shared)
	if union == 0 {
		return 0, nil
	}
	sort.Strings(shared)
	return float64(len(shared)) / float64(union), shared
}

// confidenceFor bands a similarity score. It returns the empty Confidence
// below ThresholdLow, which is how a sub-threshold pair is rejected.
func confidenceFor(score float64) Confidence {
	switch {
	case score >= ThresholdHigh:
		return ConfidenceHigh
	case score >= ThresholdMedium:
		return ConfidenceMedium
	case score >= ThresholdLow:
		return ConfidenceLow
	default:
		return ""
	}
}

// labelValue returns the value of the first label in the given namespace
// ("type:bug" in namespace "type" yields "bug"), or "" when absent.
func labelValue(labels []string, namespace string) string {
	prefix := namespace + ":"
	for _, l := range labels {
		if strings.HasPrefix(l, prefix) {
			return strings.TrimPrefix(l, prefix)
		}
	}
	return ""
}

// sharesNamespace reports whether both label sets carry a non-empty value in
// the same namespace.
func sharesNamespace(a, b []string, namespace string) bool {
	av := labelValue(a, namespace)
	return av != "" && av == labelValue(b, namespace)
}

// Duplicates returns the possible duplicates of f within candidates, best
// first, at most MaxDuplicates.
//
// The signal is LEXICAL, not semantic: Jaccard overlap of title token sets
// plus a small same-type bonus. That produces false positives and false
// negatives, which is acceptable only because nothing acts on the result —
// the confidence band and the rendered basis are what make a wrong candidate
// cheap for a human to dismiss.
//
// Ordering is score DESC then number ASC, so equal scores render in a stable
// order rather than in map or provider order.
func Duplicates(f Filing, candidates []Candidate) []DuplicateCandidate {
	ftokens := tokenize(f.Title)
	if len(ftokens) == 0 {
		return nil
	}
	out := make([]DuplicateCandidate, 0, len(candidates))
	for _, c := range candidates {
		// Self-match guard: an item byte-identical to the filing in both
		// title and body is the filing echoed back by the reader, not a
		// duplicate of it. It requires a NON-EMPTY body deliberately —
		// identity is only inferable when there is a body to compare, and
		// two bodyless items sharing a title are a genuine duplicate pair,
		// the strongest signal this package can emit. A same-title item
		// with a different body is likewise still reported.
		if c.Title == f.Title && f.Body != "" && c.Body == f.Body {
			continue
		}
		ctokens := tokenize(c.Title)
		score, shared := jaccard(ftokens, ctokens)
		if score == 0 {
			continue
		}
		if sharesNamespace(f.Labels, c.Labels, "type") || (f.Type != "" && labelValue(c.Labels, "type") == f.Type) {
			score += typeLabelBonus
		}
		if score > 1 {
			score = 1
		}
		band := confidenceFor(score)
		if band == "" {
			continue
		}
		out = append(out, DuplicateCandidate{
			Number:     c.Number,
			URL:        c.URL,
			Title:      c.Title,
			Score:      score,
			Confidence: band,
			Basis:      strings.Join(shared, ", "),
			Closed:     c.Closed,
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].Number < out[j].Number
	})
	if len(out) > MaxDuplicates {
		out = out[:MaxDuplicates]
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
