package releaseevidence

// classify.go derives a heuristic semver-bump recommendation (E33.4,
// ADR-051) from an assembled ReleaseEvidence. It walks each change's
// prose — the approved plan summary, the PR title, and any deferred
// concern categories — for breaking, additive, or (by absence) patch
// signals, then rolls the per-change signals up to the release's highest
// level.
//
// The hint is DELIBERATELY heuristic and ADVISORY: the matchers are
// case-insensitive keyword heuristics over prose (ReleaseEvidence carries
// no structured per-file change data), so false positives and negatives
// are expected and acceptable. The classifier never blocks or decides —
// it only annotates. The operator ratifies the actual version at cut time
// (E33.5); a wrong hint has no failure mode beyond an operator override.
//
// Each detected signal names the introducing change (its PR number, URL,
// and title) so the hint is auditable. PreviewLine renders the
// render-ready `suggested bump: <level> (because ...)` string that E33.2's
// preview header (#1587) will consume; this slice provides the formatter
// but wires it into no endpoint.

import (
	"fmt"
	"strings"
)

// BumpLevel is a semver-bump recommendation level. The ordering
// major > minor > patch (via rank) lets the release rollup take the max
// level seen across all changes.
type BumpLevel string

const (
	// BumpPatch is the default when no breaking or additive signal fires
	// (a doc/test-only range).
	BumpPatch BumpLevel = "patch"
	// BumpMinor is an additive, backward-compatible change (new endpoint,
	// new optional field, new stage/artifact enum member).
	BumpMinor BumpLevel = "minor"
	// BumpMajor is a breaking change (schema-major bump, removed/renamed
	// OpenAPI path, migration down-incompat marker).
	BumpMajor BumpLevel = "major"
)

// rank orders the levels so the rollup can take the highest. An unknown
// level ranks 0 (below patch) so it never spuriously wins a rollup.
func (l BumpLevel) rank() int {
	switch l {
	case BumpMajor:
		return 3
	case BumpMinor:
		return 2
	case BumpPatch:
		return 1
	default:
		return 0
	}
}

// BumpSignal is one detected bump signal, tagged with the change that
// introduced it so the hint is auditable.
type BumpSignal struct {
	Level    BumpLevel
	Reason   string
	PRNumber int
	PRURL    string
	Title    string
}

// BumpHint is the release-level recommendation: the rolled-up Level plus
// every BumpSignal that fired across the release's changes.
type BumpHint struct {
	Level   BumpLevel
	Signals []BumpSignal
}

// bumpMatcher is one ordered signal-detector entry. Keywords is the data
// table of case-insensitive substrings this matcher fires on — kept as
// ordered data (not inline conditionals) so E33.5's cut-time surface and
// future tuning stay diffable. Any keyword hit fires the signal.
type bumpMatcher struct {
	Level    BumpLevel
	Reason   string
	Keywords []string
}

// matches reports whether any of the matcher's keywords is a substring of
// the already-lowercased change text.
func (m bumpMatcher) matches(lowerText string) bool {
	for _, kw := range m.Keywords {
		if strings.Contains(lowerText, kw) {
			return true
		}
	}
	return false
}

// bumpMatchers is the ordered signal table. Breaking matchers come first,
// then additive; the rollup takes the max level regardless of order, but
// the ordering keeps the highest-severity classes at the top for review.
// Keywords are lowercase — the classifier lowercases the change text once
// before matching.
var bumpMatchers = []bumpMatcher{
	// Breaking → major.
	{
		Level:  BumpMajor,
		Reason: "schema-major bump in docs/spec",
		Keywords: []string{
			"schema-major",
			"schema major",
			"workflow-v1",
			"workflow v1",
			"standard_v2",
			"major version bump",
			"breaking change",
			"breaking schema",
		},
	},
	{
		Level:  BumpMajor,
		Reason: "removed or renamed OpenAPI path",
		Keywords: []string{
			"removed endpoint",
			"remove endpoint",
			"removed path",
			"renamed endpoint",
			"rename endpoint",
			"renamed path",
			"removed openapi",
			"removed route",
			"remove route",
		},
	},
	{
		Level:  BumpMajor,
		Reason: "migration down-incompat marker",
		Keywords: []string{
			"down-incompat",
			"down incompat",
			"irreversible migration",
			"non-reversible migration",
			"migration is not reversible",
			"drop column",
			"drop table",
		},
	},
	// Additive → minor.
	{
		Level:  BumpMinor,
		Reason: "new endpoint",
		Keywords: []string{
			"new endpoint",
			"add endpoint",
			"new route",
			"add route",
			"new openapi path",
		},
	},
	{
		Level:  BumpMinor,
		Reason: "new optional schema field",
		Keywords: []string{
			"new optional field",
			"optional field",
			"additive field",
			"new field",
			"additive schema",
		},
	},
	{
		Level:  BumpMinor,
		Reason: "new stage/artifact enum member",
		Keywords: []string{
			"new stage",
			"new artifact",
			"new enum",
			"enum member",
			"new stage type",
			"new artifact type",
		},
	},
}

// ClassifyBump derives a heuristic BumpHint from an assembled
// ReleaseEvidence. It walks each change's prose for the signals in
// bumpMatchers, tags every fired signal with the introducing change, and
// rolls the hint level up to the highest signal seen. With no evidence,
// no changes, or no signal, it returns a patch hint.
func ClassifyBump(ev *ReleaseEvidence) BumpHint {
	hint := BumpHint{Level: BumpPatch}
	if ev == nil {
		return hint
	}
	for _, ch := range ev.Changes {
		text := classificationText(ch)
		if text == "" {
			continue
		}
		lower := strings.ToLower(text)
		for _, m := range bumpMatchers {
			if !m.matches(lower) {
				continue
			}
			hint.Signals = append(hint.Signals, BumpSignal{
				Level:    m.Level,
				Reason:   m.Reason,
				PRNumber: ch.PullRequestNumber,
				PRURL:    ch.PullRequestURL,
				Title:    ch.Title,
			})
			if m.Level.rank() > hint.Level.rank() {
				hint.Level = m.Level
			}
		}
	}
	return hint
}

// classificationText builds the prose a change contributes to
// classification. A loop-merged change contributes its plan summary,
// title, and deferred-concern categories; a reduced-evidence change
// (human-led / loop-bypassing, no resolvable run) contributes its title
// only, since it carries no plan summary or concerns.
func classificationText(ch ChangeEvidence) string {
	if ch.ReducedEvidence {
		return strings.TrimSpace(ch.Title)
	}
	parts := []string{ch.PlanSummary, ch.Title}
	for _, c := range ch.DeferredConcerns {
		parts = append(parts, c.Category)
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

// PreviewLine renders the render-ready recommendation line E33.2's
// preview header (#1587) will consume:
//
//	suggested bump: <level> (because <evidence>)
//
// For a minor/major hint the parenthetical names the highest-level
// signals and their introducing PR(s); for a patch hint with no signals
// it names the doc/test-only basis.
func (h BumpHint) PreviewLine() string {
	if len(h.Signals) == 0 {
		return fmt.Sprintf(
			"suggested bump: %s (because no breaking or additive signal detected; doc/test-only changes)",
			h.Level,
		)
	}
	var parts []string
	for _, s := range h.Signals {
		if s.Level != h.Level {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s (#%d)", s.Reason, s.PRNumber))
	}
	return fmt.Sprintf("suggested bump: %s (because %s)", h.Level, strings.Join(parts, "; "))
}
