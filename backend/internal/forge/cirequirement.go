package forge

import "context"

// CIRequirement is a forge's merge-time CI requirement for a repository:
// whether a merge is refused until the head pipeline succeeds, and whether a
// SKIPPED pipeline counts as success for that purpose (E45.55 / #3490).
//
// On GitLab the two fields are the project settings
// `only_allow_merge_if_pipeline_succeeds` and
// `allow_merge_on_skipped_pipeline` read from GET /projects/:id
// (https://docs.gitlab.com/api/projects/#get-a-single-project). GitHub has
// no single-flag analogue — its requirement is the per-context union the
// branch-protection + rulesets APIs express, which ResolveRequiredChecks in
// the webhook package already captures — so the github adapter does NOT
// implement this capability.
type CIRequirement struct {
	// PipelineMustSucceed is true when the forge refuses to merge until the
	// head pipeline has succeeded. False means the forge requires nothing of
	// CI at merge time — the consumer records a PRESENT-BUT-EMPTY
	// RequiredChecksSnapshot ("nothing required"), never nil.
	PipelineMustSucceed bool
	// AllowSkippedPipeline is true when a pipeline that ran no jobs
	// (status `skipped`) satisfies PipelineMustSucceed. Meaningful only when
	// PipelineMustSucceed is true; the ingester reads it to decide whether a
	// skipped pipeline maps onto a passing or a failing stage check.
	AllowSkippedPipeline bool
}

// CIRequirementReader is the forge-neutral merge-requirement read capability
// (E45.55 / #3490). The GitLab run-creation path consumes it ONCE at
// run-create to capture the run's RequiredChecksSnapshot — the same
// FRESHNESS CONTRACT the GitHub snapshot has: resolved once, never
// re-resolved for the life of the run.
//
// It is deliberately a STANDALONE capability interface rather than a new
// method on Forge, for the same reason FileFetcher (#2022) and
// IssueOperations (#2900) are: widening Forge would churn every existing
// implementation and test fake for a capability ONE consumer uses, while a
// standalone interface lets an adapter carry it off-interface behind a
// compile-time assertion (`var _ forge.CIRequirementReader = (*Forge)(nil)`
// in forge/gitlab). A consumer names it directly and resolves a registered
// forge.Forge to it with a type assertion.
//
// A forge with NO merge-requirement concept MUST NOT implement it. The
// consumer treats a non-implementing forge as a fail-closed nil reader and
// records a NIL snapshot ("greenness unknown"), which is the honest answer;
// an implementation returning a fabricated {false,false} would instead be
// read as an authoritative "nothing required" and pass the ci_green gate
// vacuously.
//
// Errors: implementations return the sentinels in types.go (ErrNotFound,
// ErrForbidden, …) so callers switch on forge-neutral values via errors.Is.
type CIRequirementReader interface {
	// ReadCIRequirement reads repo's merge-time CI requirement under scope.
	// A nil *CIRequirement is returned only alongside a non-nil error.
	ReadCIRequirement(ctx context.Context, scope CredentialScope, repo RepoRef) (*CIRequirement, error)
}
