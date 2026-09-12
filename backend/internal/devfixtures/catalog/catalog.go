// Package catalog is the NAME-ONLY leaf of backend/internal/devfixtures
// (E72.2 / #3326): the closed set of seeded acceptance-preview scenario
// names plus a one-line description of each.
//
// It exists as its own package, importing nothing but the standard
// library, so a pure classifier such as backend/internal/plan can ask
// "is this token a known fixture scenario?" without dragging the
// DB-bearing devfixtures package (run/db, artifact/db, audit/db, pgx,
// yaml) into its import graph. devfixtures itself re-exports Names and
// the two-way test in that package pins this list to the embedded
// scenario files, so a name here without a YAML — or a YAML without a
// name here — fails in-loop.
package catalog

import "sort"

// descriptions is the single source of truth for the catalog: the key set
// IS the name set, and each value is the one-line description GET
// /v0/dev/fixtures renders. Kept as a map so a name cannot be added
// without a description (a nil-description row would be a confusing list
// entry for the acceptance agent that reads it).
var descriptions = map[string]string{
	"grooming-confirm-gate": "backlog_grooming run: groom (plan, agent) succeeded with a grooming_report and one approval; confirm (review, human) parked at awaiting_approval; no implement stage.",
	"plan-gate-parked":      "feature_change run: plan stage parked at awaiting_approval carrying a valid standard_v1 plan artifact.",
	"split-parent-linked":   "two feature_change runs on repo stub/parent-close (one per forge family), each carrying one split_children_filed linkage row (parent #100, contract child #103, parent_forge github / gitlab) so a stub-forge issues.closed delivery for #103 closes the parent through the E50.6 watcher.",
	"trace-upload-target":   "feature_change run: plan stage dispatched, with a four-row backdated cost_recorded spend baseline (aged 1h5m, 2h, 3h, 4h) so a trace upload can trip spend_alert.",
}

// Names is the sorted closed set of scenario names. It is a fresh slice on
// every call so a caller cannot mutate the catalog.
func Names() []string {
	names := make([]string, 0, len(descriptions))
	for n := range descriptions {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Known reports whether name is a catalog scenario. Exact, case-sensitive
// match: the classifier that consumes this lowercases its input first, and
// every catalog name is lowercase.
func Known(name string) bool {
	_, ok := descriptions[name]
	return ok
}

// Description returns the one-line description for name, or "" for a name
// the catalog does not carry.
func Description(name string) string {
	return descriptions[name]
}
