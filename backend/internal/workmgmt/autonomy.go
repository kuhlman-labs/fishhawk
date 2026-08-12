package workmgmt

import "strings"

// AutonomyLabelPrefix is the label namespace the campaign autonomy tier is read
// from. It mirrors the conventions' `autonomy:` namespace (LabelDefaults keys on
// "autonomy"; the default value is "autonomy:medium") so the tier a filing
// stamps at creation time is the tier the campaign source reads back.
const AutonomyLabelPrefix = "autonomy:"

// ParseAutonomyLabel extracts the autonomy tier from an issue's labels: the
// suffix of the first `autonomy:<tier>` label (e.g. "autonomy:low" -> "low"). It
// returns "" when no autonomy label is present (unknown/default, treated
// downstream as non-human-led) so the whole namespace lives in ONE helper — a
// future tier is one edit. Only the recognized tiers ("", "low", "medium",
// "high") pass through; an unrecognized tier (a typo like "autonomy:critical")
// normalizes to "" = non-human-led.
//
// This keeps the parse boundary in lockstep with the fail-closed
// campaign_items.autonomy CHECK (migration 0049): a mislabeled child degrades to
// the autonomous default rather than aborting the entire epic campaign with a
// CHECK violation when Persist writes the row. It is the single source of truth
// shared by the assembly path (via the github provider's delegate) and the
// reconcile-on-read autonomy refresh (#2355).
func ParseAutonomyLabel(labels []string) string {
	for _, l := range labels {
		if tier := strings.TrimPrefix(l, AutonomyLabelPrefix); tier != l {
			switch tier {
			case "low", "medium", "high":
				return tier
			default:
				return ""
			}
		}
	}
	return ""
}
