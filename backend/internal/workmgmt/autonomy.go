package workmgmt

import (
	"sort"
	"strings"
)

// AutonomyLabelPrefix is the label namespace the campaign autonomy tier is read
// from. It mirrors the conventions' `autonomy:` namespace (LabelDefaults keys on
// "autonomy"; the default value is "autonomy:medium") so the tier a filing
// stamps at creation time is the tier the campaign source reads back.
const AutonomyLabelPrefix = "autonomy:"

// BuiltinDefaultAutonomyTier is the tier ADR-069 decision 4 names as the
// default when an issue declares none: `medium`. It is the floor every
// autonomy resolution degrades to when neither the issue nor the repository
// conventions supply an in-set tier, so a resolved tier is never "" (#5316).
const BuiltinDefaultAutonomyTier = "medium"

// isAutonomyTier reports whether t is one of the recognised campaign autonomy
// tiers — the set the campaign_items.autonomy CHECK (migration 0049) admits,
// minus the legacy empty tier it still tolerates for pre-#5316 rows.
func isAutonomyTier(t string) bool {
	switch t {
	case "low", "medium", "high":
		return true
	}
	return false
}

// autonomyTierRank orders the tiers by permissiveness: low (human-led) is the
// least permissive, high the most. Only meaningful for an in-set tier.
func autonomyTierRank(t string) int {
	switch t {
	case "low":
		return 0
	case "medium":
		return 1
	default:
		return 2
	}
}

// NormalizeAutonomyTier maps a tier to the recognised set: an in-set tier
// (low, medium, high) is returned unchanged, and "" or any out-of-set value
// (a typo like "critical") degrades to BuiltinDefaultAutonomyTier. It never
// returns "" — ADR-069 decision 4 (#5316): an unknown tier is never treated
// more permissively than the named default, and never as "non-human-led by
// omission".
func NormalizeAutonomyTier(tier string) string {
	if isAutonomyTier(tier) {
		return tier
	}
	return BuiltinDefaultAutonomyTier // never ""
}

// ParseAutonomyLabel extracts the autonomy tier from an issue's labels: the
// suffix of the first `autonomy:<tier>` label (e.g. "autonomy:low" -> "low").
// The whole namespace lives in ONE helper, so a future tier is one edit.
//
// A declared in-set tier (low, medium, high) wins. An issue with NO autonomy
// label, or whose first autonomy label carries an unrecognised or empty tier
// (a typo like "autonomy:critical"), resolves to defaultTier — normally the
// repository conventions' default, Conventions.DefaultAutonomyTier() — per
// ADR-069 decision 4 (#5316). defaultTier itself passes through
// NormalizeAutonomyTier, so an empty or out-of-set default degrades to
// BuiltinDefaultAutonomyTier: the result is never "" and never
// "non-human-led by omission".
//
// The result is always in the fail-closed campaign_items.autonomy CHECK set
// (migration 0049), so a mislabelled child degrades to the conventions default
// rather than aborting the whole epic campaign with a CHECK violation when
// Persist writes the row. It is the single source of truth shared by the
// assembly path (the github and gitlab providers) and the reconcile-on-read
// autonomy refresh (#2355).
func ParseAutonomyLabel(labels []string, defaultTier string) string {
	if tier, ok := DeclaredAutonomyTier(labels); ok {
		return tier
	}
	return NormalizeAutonomyTier(defaultTier)
}

// DeclaredAutonomyTier reports the in-set tier an issue's labels DECLARE: the
// suffix of the first `autonomy:<tier>` label when it is low, medium or high.
// ok is false when no autonomy label is present, or when the first one carries
// an unrecognised or empty tier — exactly the cases ParseAutonomyLabel resolves
// to its default. A caller that cannot load the conventions default uses it to
// apply an explicit tier without one (#5316).
func DeclaredAutonomyTier(labels []string) (tier string, ok bool) {
	for _, l := range labels {
		t, found := strings.CutPrefix(l, AutonomyLabelPrefix)
		if !found {
			continue
		}
		if isAutonomyTier(t) {
			return t, true
		}
		break
	}
	return "", false
}

// IgnoredAutonomyDefault names one item type's `label_defaults.autonomy` value
// that DefaultAutonomyTier skipped because it is not `autonomy:low`,
// `autonomy:medium` or `autonomy:high` — a typo such as "autonomy:lows", an
// unrecognised tier such as "autonomy:manual", or a value missing the
// `autonomy:` namespace. Conventions validation admits such a value (it checks
// only the namespace prefix), so this report is the signal the campaign
// callers log a WARN on (#5316).
type IgnoredAutonomyDefault struct {
	ItemType string // the conventions item type that declared the value
	Value    string // the declared label_defaults.autonomy value, verbatim
}

// DefaultAutonomyTier derives the repository's autonomy default from its
// conventions: the LEAST permissive in-set tier any item type declares in
// `label_defaults.autonomy` (low < medium < high), falling back to
// BuiltinDefaultAutonomyTier when no type declares one. A malformed value (an
// out-of-set tier such as "autonomy:critical", or one missing the `autonomy:`
// namespace) is ignored, and reported in ignored (sorted by item type) so the
// caller can log it: a mistyped human-led default ("autonomy:lows") resolves
// to the remaining types' tier or medium, and the report is that residual's
// only signal. A type declaring no autonomy default is not reported.
//
// Taking the least permissive default across types is the accepted,
// fail-closed reading of ADR-069 decision 4's "conventions default" (#5316):
// the campaign path does not infer an issue's item type from its `type:*`
// label, so when types disagree an unlabelled issue resolves to the tier that
// keeps the most human oversight. tier is never "".
func (c Conventions) DefaultAutonomyTier() (tier string, ignored []IgnoredAutonomyDefault) {
	best := ""
	for name, it := range c.Types {
		v := it.LabelDefaults["autonomy"]
		if v == "" {
			continue // the type declares no autonomy default.
		}
		t, ok := strings.CutPrefix(v, AutonomyLabelPrefix)
		if !ok || !isAutonomyTier(t) {
			ignored = append(ignored, IgnoredAutonomyDefault{ItemType: name, Value: v})
			continue
		}
		if best == "" || autonomyTierRank(t) < autonomyTierRank(best) {
			best = t
		}
	}
	sort.Slice(ignored, func(i, j int) bool { return ignored[i].ItemType < ignored[j].ItemType })
	return NormalizeAutonomyTier(best), ignored
}
