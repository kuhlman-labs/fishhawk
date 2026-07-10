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
	// DefaultedLabels is every label the SYSTEM added that the caller did not
	// supply (#1616): namespace defaults from the type's label_defaults, plus
	// handler-derived area labels appended after Apply. Reported loudly so the
	// operator sees exactly what filing-time completeness added.
	DefaultedLabels []string `json:"defaulted_labels,omitempty"`
	// MissingLabelNamespaces is the required namespaces (from the type's
	// required_label_namespaces) still absent after merge, derivation, and
	// defaulting (#1616). Reported loudly, NEVER a rejection — a filing is
	// never failed on labels alone.
	MissingLabelNamespaces []string `json:"missing_label_namespaces,omitempty"`
}

// BoardPlacement holds an item's position on the project board. Status
// is the single-select Status field value (e.g. Backlog); BoardColumn is
// the board column when it is distinct from status.
type BoardPlacement struct {
	Status      string `json:"status,omitempty"`
	BoardColumn string `json:"board_column,omitempty"`
}

// Canonical work-item lifecycle states (#1012). These are the
// provider-agnostic states the conventions `states` map binds to
// provider-specific board columns/options, and the values the
// `transitions` map points a run-lifecycle event at. The set is closed —
// the schema constrains both the states keys and the transitions values to
// exactly these — so the board-sync hook and the provider agree on a fixed
// vocabulary independent of how any one board labels its columns.
//
// CanonicalStateUpNext (#1816) is the committed/not-started entry column:
// when a campaign transitions pending -> running, the campaign_started edge
// sweeps its still-queued items onto Up Next, and run_started later advances
// an Up Next card to In Progress just as it does a Backlog card.
const (
	CanonicalStateBacklog    = "backlog"
	CanonicalStateUpNext     = "up_next"
	CanonicalStateInProgress = "in_progress"
	CanonicalStateInReview   = "in_review"
	CanonicalStateBlocked    = "blocked"
	CanonicalStateDone       = "done"
)

// TransitionRequest is a board-state move resolved by the run-lifecycle
// hook (#1012): advance the work item identified by IssueNumber to
// CanonicalState — but only when the card's current board status is in
// ExpectedSourceStates. That guard is load-bearing (never-fight-the-human):
// a card a human parked elsewhere (e.g. Blocked) is left untouched and the
// provider returns a Skipped result. Transition touches ONLY the board
// Status column — never labels, fields, or epic links (those are filing,
// #1005).
//
// States is the canonical-state -> provider-option map (Conventions.States),
// carried on the request so the provider resolves CanonicalState and
// ExpectedSourceStates to board options without re-reading the conventions.
// Trigger names the lifecycle event (run_started, pr_opened, …) for audit.
type TransitionRequest struct {
	Item                 WorkItem
	IssueNumber          int
	Trigger              string
	Target               Target
	CanonicalState       string
	ExpectedSourceStates []string
	States               map[string]string
}

// TransitionResult reports what a Transition did. Moved is true with From/To
// holding the previous and new provider-option strings when the card was
// advanced. Skipped is true (with a SkipReason) when the move was a
// deliberate no-op: the card is off-board, its current status is not in the
// expected source set (the never-fight-the-human case), it is already at the
// target, or the canonical state has no configured/board option. A non-nil
// error from Transition is a genuine provider failure, distinct from a
// Skipped no-op; the lifecycle hook logs it best-effort and never unwinds
// the run.
type TransitionResult struct {
	Moved      bool   `json:"moved"`
	From       string `json:"from,omitempty"`
	To         string `json:"to,omitempty"`
	Skipped    bool   `json:"skipped"`
	SkipReason string `json:"skip_reason,omitempty"`
}

// Relations links a work item to other work. ParentEpic is the epic this
// item rolls up to (an issue reference); Supersedes and CompanionTo are
// peer links; EvidenceRuns names the run IDs that motivated the item (the
// operator-agent follow-up-filing path attaches the originating run);
// DependsOn is the issue-level dependency edge.
type Relations struct {
	ParentEpic   string   `json:"parent_epic,omitempty"`
	Supersedes   []string `json:"supersedes,omitempty"`
	CompanionTo  []string `json:"companion_to,omitempty"`
	EvidenceRuns []string `json:"evidence_runs,omitempty"`
	// DependsOn is the issue-level dependency edge a campaign derives its
	// wave DAG from (ADR-047 / #1437). Entries are issue references (`#N`
	// or `N`) among the epic's children; the campaign's epic-children query
	// (EpicChildren) returns the depends_on edges over that sibling set,
	// which E25.3 assembles into plan.Waves. File-time validation is
	// format-only — cycle and existence checks are deferred to
	// campaign-assembly time (E25.3), not file time, because Apply is pure
	// (no I/O) and cycle detection needs the full assembled DAG.
	DependsOn []string `json:"depends_on,omitempty"`
}
