package run

// Run-from-run child params (E67.17 / #2589).
//
// Five successive mint sites hand-copied a subset of the parent run's
// fields into CreateRunParams, and each new field added to the struct
// had to be remembered at every site. working_dir was dropped three
// times that way. ChildParamsFrom makes inheritance the DEFAULT: a run
// minted FROM another run starts from the parent's fields and the call
// site sets only what makes it distinct, so every divergence is a
// visible, reviewable line rather than a silent omission.
//
// The decision record is childParamsInheritance below — one row per
// CreateRunParams field, with a mode and a one-sentence reason. It is
// not decorative: childparams_test.go reflects over CreateRunParams and
// fails when a field has no row, when a row names no field, and when a
// row's mode disagrees with what ChildParamsFrom actually returns.
// childparams_gate_test.go closes the third gap — that the helper is
// USED — by AST-walking the backend module's non-test sources and
// failing on any hand-rolled CreateRunParams construction outside an
// allow-list keyed by (file, enclosing function).

// inheritanceMode classifies how ChildParamsFrom treats one
// CreateRunParams field when minting a run from a parent run.
type inheritanceMode string

const (
	// modeInherited copies the parent's value verbatim.
	modeInherited inheritanceMode = "inherited"
	// modeDerived computes the child's value from the parent rather
	// than copying it (today: ParentRunID = &parent.ID).
	modeDerived inheritanceMode = "derived"
	// modeNotInherited deliberately leaves the field zero for the
	// call site to set (or not).
	modeNotInherited inheritanceMode = "notInherited"
)

// childFieldDecision is one row of the inheritance decision record.
type childFieldDecision struct {
	Mode inheritanceMode
	// Reason is the one-sentence justification a reviewer reads. A new
	// CreateRunParams field cannot ship without one: the coverage pin
	// in childparams_test.go fails on a field with no row.
	Reason string
}

// childParamsInheritance is the per-field decision record for a run
// minted from another run. Every CreateRunParams field appears exactly
// once; see the two-sided reflection pin in childparams_test.go.
var childParamsInheritance = map[string]childFieldDecision{
	// --- inherited: the child is the same work on the same repo,
	// workflow, trigger and execution backend as the run it came from.
	"Repo": {modeInherited, "A child run acts on the same repository as the run it was minted from."},
	"WorkflowID": {modeInherited,
		"A child executes the same workflow definition as its parent."},
	"WorkflowSHA": {modeInherited,
		"The child is bound to the same spec revision the parent resolved, so a mid-flight spec edit cannot shift its contract."},
	"TriggerSource": {modeInherited,
		"Provenance of the work is the parent's trigger; a child is not a new independent trigger."},
	"TriggerRef": {modeInherited,
		"The child addresses the same issue / ref as the parent (sites that must normalize a degenerate value do so explicitly after the call)."},
	"InstallationID": {modeInherited,
		"The child talks to the same forge installation as the parent (sites that must normalize a pointer-to-zero do so explicitly after the call)."},
	"InstallationRef": {modeInherited,
		"The child authenticates against the same forge credential as the parent (E45.22 / #2043): a retry of a gitlab_ci run that dropped the ref would fall back to a GitHub installation id it does not have."},
	"RunnerKind": {modeInherited,
		"A child runs in the same execution backend as the run it was minted from (ADR-022)."},
	"WorkflowSpec": {modeInherited,
		"The child's prompt resolves policy (max_stage_runtime, retry caps) from the parent's cached spec instead of refetching a spec that may have moved."},
	"WorkingDir": {modeInherited,
		"A child executes against the same local checkout the parent's branch lineage is anchored to (E48.100 / #2547 / #2483)."},
	"RequiredChecksSnapshot": {modeInherited,
		"A child implements against the same protected branch as its parent, so it gates on the same required contexts (#251 / ADR-017)."},
	"MaxRetriesSnapshot": {modeInherited,
		"A child inherits the parent's snapshotted on_ci_failure cap so every row in one chain reports the same N/M (#280)."},
	"IssueContext": {modeInherited,
		"The child works the same issue as its parent, so it reuses the cached context rather than re-fetching it (#415); a site needing a narrowed context overrides after the call."},

	// --- derived: computed from the parent, never copied.
	"ParentRunID": {modeDerived,
		"Set to &parent.ID: copying parent.ParentRunID verbatim would point the child at its GRANDparent and break the lineage walk (#216)."},

	// --- not inherited: deliberately left zero for the call site.
	"RetryAttempt": {modeNotInherited,
		"Each site owns its position in the retry chain (fan-out 0, CI retry parent+1, operator recovery parent's unchanged), so an inherited default would silently mis-place a child."},
	"DecomposedFrom": {modeNotInherited,
		"Decomposition lineage is meaningful only for a fan-out child; copying it would mark a retry or recovery child as decomposed (#455)."},
	"SliceIndex": {modeNotInherited,
		"The sole-writer slice position is assigned per fan-out child; a non-fan-out child has none (E24.1 / #1141 / ADR-041)."},
	"IdempotencyKey": {modeNotInherited,
		"The dedup key is scoped to the REQUEST that mints the child; reusing the parent's would collide the child with its parent."},
	"UpstreamRunID": {modeNotInherited,
		"A deploy-gate pointer, deliberately kept off the lineage path (E23.11 / #1417); inheriting it would re-point a child's deploy gate at the parent's upstream."},
	"Drive": {modeNotInherited,
		"Preserves today's behavior at all three mint sites: a child of a driven parent stays operator-driven, and changing that is a separate product decision (#1023)."},
}

// ChildParamsFrom returns CreateRunParams pre-populated from parent per
// childParamsInheritance. It is the ONLY sanctioned construction point
// for a run minted from another run — the source gate in
// childparams_gate_test.go fails any other hand-rolled literal in the
// backend module's non-test sources.
//
// Call sites set only what makes them distinct (their retry-chain
// position, decomposition lineage, a narrowed issue context, a
// request-scoped idempotency key) AFTER the call, so each divergence is
// one reviewable line.
//
// A nil parent returns the zero value rather than panicking: these call
// sites sit inside HTTP handlers and webhook dispatch, where a nil-deref
// is a 500 on the whole request.
func ChildParamsFrom(parent *Run) CreateRunParams {
	if parent == nil {
		return CreateRunParams{}
	}
	parentID := parent.ID
	return CreateRunParams{
		// inherited
		Repo:                   parent.Repo,
		WorkflowID:             parent.WorkflowID,
		WorkflowSHA:            parent.WorkflowSHA,
		TriggerSource:          parent.TriggerSource,
		TriggerRef:             parent.TriggerRef,
		InstallationID:         parent.InstallationID,
		InstallationRef:        parent.InstallationRef,
		RunnerKind:             parent.RunnerKind,
		WorkflowSpec:           parent.WorkflowSpec,
		WorkingDir:             parent.WorkingDir,
		RequiredChecksSnapshot: parent.RequiredChecksSnapshot,
		MaxRetriesSnapshot:     parent.MaxRetriesSnapshot,
		IssueContext:           parent.IssueContext,

		// derived
		ParentRunID: &parentID,

		// notInherited fields are left zero on purpose; see the table.
	}
}
