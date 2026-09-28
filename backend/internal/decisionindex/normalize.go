package decisionindex

import (
	"sort"
	"strings"
	"unicode"
)

// Canonical concern categories. The ConcernCategory suffix keeps these out of
// the audit-category registry sweep (TestKnownCategoriesCoversEmittedCategories):
// they classify reviewer concerns, not audit rows. Reviewers name categories
// in free text, so the same kind of concern arrives spelled several ways;
// precedent matching needs one key per kind.
const (
	CorrectnessConcernCategory     = "correctness"
	TestingConcernCategory         = "testing"
	PerformanceConcernCategory     = "performance"
	DocumentationConcernCategory   = "documentation"
	MaintainabilityConcernCategory = "maintainability"
	ScopeConcernCategory           = "scope"
)

// concernCategoryAliases maps a surface-normalized category (see
// surfaceNormalize) to its canonical key. The correctness family is seeded from
// backend/internal/server/defer_concern.go's deferDefectCategories — the six
// categories that map to a `bug` follow-up there all normalize to correctness,
// matching the bug/chore split that map already encodes. The other families are
// the obvious reviewer synonyms. Every canonical key maps to itself.
//
// A plain literal so TestNormalizeConcernCategory_Table can range over it.
var concernCategoryAliases = map[string]string{
	// correctness — deferDefectCategories verbatim.
	"correctness": CorrectnessConcernCategory,
	"bug":         CorrectnessConcernCategory,
	"security":    CorrectnessConcernCategory,
	"logic":       CorrectnessConcernCategory,
	"regression":  CorrectnessConcernCategory,
	"data-loss":   CorrectnessConcernCategory,

	"testing":       TestingConcernCategory,
	"test-coverage": TestingConcernCategory,
	"tests":         TestingConcernCategory,

	"performance": PerformanceConcernCategory,
	"perf":        PerformanceConcernCategory,
	"efficiency":  PerformanceConcernCategory,

	"documentation": DocumentationConcernCategory,
	"docs":          DocumentationConcernCategory,
	"doc":           DocumentationConcernCategory,

	"maintainability": MaintainabilityConcernCategory,
	"readability":     MaintainabilityConcernCategory,
	"style":           MaintainabilityConcernCategory,
	"simplification":  MaintainabilityConcernCategory,

	"scope":   ScopeConcernCategory,
	"scoping": ScopeConcernCategory,
}

// NormalizeConcernCategory maps a reviewer-supplied concern category to its
// canonical key. The input is surface-normalized first (trimmed, lowercased,
// every run of whitespace, underscores and hyphens collapsed to one hyphen) and
// then looked up in the alias table.
//
// An UNMAPPED value is returned AS ITSELF — its surface-normalized form — with
// mapped=false: never dropped and never coerced into a different category, so
// the caller indexes it verbatim and reports it. An empty (or all-separator)
// input returns ("", false); the extractor treats that as "no category", not as
// an unmapped one. Nothing here consults the database.
func NormalizeConcernCategory(raw string) (canonical string, mapped bool) {
	s := surfaceNormalize(raw)
	if s == "" {
		return "", false
	}
	if c, ok := concernCategoryAliases[s]; ok {
		return c, true
	}
	return s, false
}

// surfaceNormalize trims, lowercases, and collapses each run of whitespace,
// underscores and hyphens into a single hyphen, dropping leading/trailing
// separators.
func surfaceNormalize(raw string) string {
	var b strings.Builder
	pendingSep := false
	for _, r := range strings.ToLower(strings.TrimSpace(raw)) {
		if r == '_' || r == '-' || unicode.IsSpace(r) {
			pendingSep = true
			continue
		}
		if pendingSep && b.Len() > 0 {
			b.WriteByte('-')
		}
		pendingSep = false
		b.WriteRune(r)
	}
	return b.String()
}

// CanonicalConcernCategories returns the sorted, de-duplicated canonical set.
func CanonicalConcernCategories() []string {
	seen := make(map[string]struct{}, len(concernCategoryAliases))
	for _, c := range concernCategoryAliases {
		seen[c] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for c := range seen {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}
