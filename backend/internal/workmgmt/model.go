// Package workmgmt parses and validates the work-management conventions
// config (#1005) and carries the provider-agnostic canonical work-item
// model. The shipped default (defaults/work-management-default.yaml) and
// the schema (schemas/work-management-v0.schema.json) are embedded copies
// of the canonical files under docs/spec/, mirrored by
// scripts/sync-schemas and kept in lockstep by the schema-sync gate.
//
// The conventions config is the value: it turns one filing call into a
// conventions-complete work item (title format, body skeleton, default
// labels/fields, board placement, ADR numbering, epic linking). This
// package owns the model and the parse/validate surface; the provider
// interface, the conventions-application logic, and the concrete GitHub
// Projects provider are layered on top in sibling files of the same
// package.
package workmgmt

// WorkItem is the provider-agnostic canonical work item. A caller
// supplies the type, title, and body; the conventions-application layer
// fills classification, board placement, and relations from the repo's
// conventions before a provider materializes it. The shape is kept
// expressible as both a GitHub issue/Project item and a Jira issue (the
// Jira forcing function) so no provider needs a divergent model.
type WorkItem struct {
	// Type names the work-item type (a key in the conventions' types
	// map): bug, feature, chore, adr, …
	Type string `json:"type"`
	// Title is the rendered item title.
	Title string `json:"title"`
	// Body is the assembled item body.
	Body string `json:"body"`
	// Classification carries labels and the complexity prior.
	Classification Classification `json:"classification"`
	// BoardPlacement carries the project board status/column.
	BoardPlacement BoardPlacement `json:"board_placement"`
	// Relations carries the item's links to other work.
	Relations Relations `json:"relations"`
}

// Classification holds the labels and complexity prior applied to an
// item. Labels are the merged set (type defaults + caller-supplied);
// Complexity is one of low/medium/high.
type Classification struct {
	Labels     []string `json:"labels,omitempty"`
	Complexity string   `json:"complexity,omitempty"`
}

// BoardPlacement holds an item's position on the project board. Status
// is the single-select Status field value (e.g. Backlog); BoardColumn is
// the board column when it is distinct from status.
type BoardPlacement struct {
	Status      string `json:"status,omitempty"`
	BoardColumn string `json:"board_column,omitempty"`
}

// Relations links a work item to other work. ParentEpic is the epic this
// item rolls up to (an issue reference); Supersedes and CompanionTo are
// peer links; EvidenceRuns names the run IDs that motivated the item (the
// operator-agent follow-up-filing path attaches the originating run).
type Relations struct {
	ParentEpic   string   `json:"parent_epic,omitempty"`
	Supersedes   []string `json:"supersedes,omitempty"`
	CompanionTo  []string `json:"companion_to,omitempty"`
	EvidenceRuns []string `json:"evidence_runs,omitempty"`
}
