// Package stagecheck records and reads the state of each blocking
// check declared on a workflow-spec gate (#228). Two writers feed
// it today: GitHub `check_run` webhook events for ci_pass-style
// external checks, and GitLab Pipeline Hook events recorded under
// the `gitlab/pipeline` check name (E45.55 / #3490); the backend's
// own audit-completeness derivation (#229) is the unfilled third.
// Two readers consume it: the review-stage detail page (read-only
// render) and the deploy gate's ci_green verdict.
//
// Rows are append-only — every status update writes a new row, and
// the latest per (stage_id, check_name) is what consumers see. The
// retention story matches audit_entries: nothing is ever mutated
// or deleted.
//
// "Latest" is STRUCTURAL, not a guard in the writer: both latest-row
// readers order by `gitlab_pipeline_id DESC NULLS LAST, ts DESC`, so
// within one (stage_id, check_name) the highest GitLab pipeline id is
// the latest row regardless of ts or delivery order, while GitHub rows
// (NULL id) keep pure ts ordering. See README.md § Precedence.
package stagecheck

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
)

// ErrNotFound signals the requested check has never been observed
// for the (stage_id, check_name) pair. Callers map it to the SPA's
// `not_tracked` placeholder.
var ErrNotFound = errors.New("stagecheck: not found")

// State is the SPA-facing rollup of a check's latest status +
// conclusion. Mirrors the BlockingCheckState union the frontend
// already takes in `frontend/src/components/blocking-checks-panel.tsx`.
type State string

// State values per the SPA contract.
const (
	StatePass       State = "pass"
	StateFail       State = "fail"
	StatePending    State = "pending"
	StateNotTracked State = "not_tracked" // never set in storage; returned by readers when no row exists
)

// Check is the canonical in-memory shape of one stage_checks row,
// with conclusion + status compressed into a single State for the
// SPA. The raw GitHub fields stay around for forensic / audit-
// export use.
type Check struct {
	ID               uuid.UUID
	StageID          uuid.UUID
	Name             string
	State            State
	Status           string  // verbatim GitHub status (queued / in_progress / completed)
	Conclusion       *string // verbatim GitHub conclusion (success / failure / …)
	HeadSHA          string
	GitHubCheckRunID *int64
	// GitLabPipelineID is the GitLab pipeline's instance-global
	// object_attributes.id for a `gitlab/pipeline` row; nil for every
	// GitHub-sourced row. Beyond forensics it is an ORDERING KEY: the
	// latest-row readers sort it DESC NULLS LAST ahead of ts, so the
	// highest id wins within a check_name (#3490).
	GitLabPipelineID *int64
	Timestamp        time.Time
	Payload          json.RawMessage
}

// AppendParams collects the inputs Append needs. Mirrors what an
// ingest path (webhook handler, backend self-derivation) hands the
// repository.
type AppendParams struct {
	StageID          uuid.UUID
	Name             string
	Status           string
	Conclusion       *string
	HeadSHA          string
	GitHubCheckRunID *int64
	GitLabPipelineID *int64 // see Check.GitLabPipelineID; nil for GitHub rows
	Timestamp        time.Time
	Payload          json.RawMessage
}

// StageRef names one stage together with the run that owns it. The
// GitLab pipeline ingester needs both: the stage to append the row to,
// and the run to read the RequiredChecksSnapshot flag from and to
// re-run the post-CI policy evaluation for.
type StageRef struct {
	StageID uuid.UUID
	RunID   uuid.UUID
}

// GitLabPipelineMatch is the project-scoped key the GitLab pipeline
// ingester resolves to review stages. Repo AND InstallationRef are
// BOTH required: a fork of the same project can carry the same
// head_sha and the same merge-request iid, and only the pair
// (runs.repo, runs.installation_ref) pins the row to the project the
// webhook actually came from. MergeRequestIID 0 means "no
// pr_number filter" — a branch pipeline that carries no merge_request
// still matches the run by head_sha alone.
type GitLabPipelineMatch struct {
	Repo            string
	InstallationRef string
	HeadSHA         string
	MergeRequestIID int
}

// Repository is the persistence surface for stage check states.
// Production wires the postgres-backed implementation; tests use a
// memory fake or a stub.
type Repository interface {
	// Append writes a new row. Returns the persisted Check with its
	// derived State filled in. Idempotent at the storage layer
	// only insofar as the canonical-state read picks the most-
	// recent row — duplicate appends produce duplicate history,
	// which is fine for the audit story.
	Append(ctx context.Context, p AppendParams) (*Check, error)

	// LatestForStage returns one Check per check_name on the
	// stage, holding the latest observed state. Returns an empty
	// slice when no checks have been recorded.
	LatestForStage(ctx context.Context, stageID uuid.UUID) ([]*Check, error)

	// LatestForStageAndName returns the most recent state for the
	// (stage_id, check_name) pair, or ErrNotFound when no row
	// exists. Used by the approval handler to enforce the gate.
	LatestForStageAndName(ctx context.Context, stageID uuid.UUID, name string) (*Check, error)

	// FindMatchingStages walks the artifacts table to locate every
	// stage whose run has a `pull_request` artifact with the given
	// (pr_number, head_sha) AND whose gate's blocking_checks
	// contain the given check name. Returns the stage ids the
	// ingest path should write rows for. Empty slice when no run
	// matches — the check_run event is for a non-Fishhawk PR or a
	// PR that doesn't gate on this check.
	FindMatchingStages(ctx context.Context, prNumber int, headSHA, checkName string) ([]uuid.UUID, error)

	// FindMatchingStagesForGitLabPipeline is the GitLab Pipeline Hook
	// sibling of FindMatchingStages (E45.55 / #3490): it walks the
	// same artifacts → implement stage → run → review stage path but
	// is PROJECT-SCOPED — a run matches only when BOTH runs.repo and
	// runs.installation_ref equal the match's, so a fork sharing the
	// head_sha and the merge-request iid never receives a row.
	// MergeRequestIID 0 disables the pr_number predicate. Returns the
	// (stage, run) pairs in run-creation then stage-sequence order;
	// empty when nothing matches.
	FindMatchingStagesForGitLabPipeline(ctx context.Context, m GitLabPipelineMatch) ([]StageRef, error)
}

// DeriveState rolls a GitHub `check_run.status` + `conclusion`
// pair into the SPA-facing State. The mapping is conservative:
// only `success` and `neutral` count as pass; anything else that's
// completed counts as fail; anything that hasn't completed counts
// as pending.
//
// Backend self-derived checks (fishhawk_audit_complete, #229) feed
// the same enum: status="completed" + conclusion="success" for
// pass, status="completed" + conclusion="failure" for fail,
// status="in_progress" for pending.
func DeriveState(status string, conclusion *string) State {
	if status != "completed" {
		return StatePending
	}
	if conclusion == nil {
		// Defensive: GitHub can deliver completed without a
		// conclusion in narrow cases; treat it as pending so the
		// gate refuses approval rather than passing silently.
		return StatePending
	}
	switch *conclusion {
	case "success", "neutral":
		return StatePass
	case "skipped":
		// "skipped" means the check ran and decided not to apply.
		// Treating as pass matches GitHub's own UI semantics.
		return StatePass
	case "failure", "timed_out", "cancelled", "action_required", "stale", "startup_failure":
		return StateFail
	case "skipped_not_allowed":
		// The ONE non-GitHub conclusion (E45.55 / #3490): the GitLab
		// pipeline ingester records a `skipped` pipeline under this
		// conclusion when the run's RequiredChecksSnapshot does NOT
		// carry allow_merge_on_skipped_pipeline — GitLab itself would
		// refuse the merge, so the gate must read it as fail rather
		// than borrow GitHub's skipped-is-pass semantics above.
		return StateFail
	default:
		// Unknown conclusion → pending so we don't accidentally
		// clear a gate on a value GitHub adds in the future.
		return StatePending
	}
}

// ConclusionSuperseded reports whether a check-run conclusion means the
// check was SUPERSEDED by a newer head rather than judged on the code —
// exactly GitHub's `cancelled` and `stale`. `cancelled` is what ci.yml's
// concurrency: cancel-in-progress assigns to the in-flight run's check
// runs when a new push lands in the same group; `stale` is what GitHub
// itself assigns when a check run is superseded. Neither carries a
// verdict about the diff, so the drive ci_failed park must not trip on
// them (#3414). nil and every other conclusion (including `failure` and
// `timed_out`) are NOT superseded — false.
//
// This is deliberately NARROWER than DeriveState's fail set: DeriveState
// still maps cancelled/stale to StateFail (the SPA fail rendering and the
// conservative approval/merge-gate posture are unchanged), while this
// predicate lets the drive park distinguish a superseded head from a real
// red verdict.
func ConclusionSuperseded(conclusion *string) bool {
	if conclusion == nil {
		return false
	}
	switch *conclusion {
	case "cancelled", "stale":
		return true
	default:
		return false
	}
}

// RedVerdict reports whether the check carries a RED VERDICT about the
// code: a StateFail whose conclusion is not a supersession (#3414). The
// drive ci_failed park keys on this rather than State == StateFail, so a
// superseded cancelled/stale conclusion never trips ci_failed. A StateFail
// with a nil Conclusion stays red — production never produces it
// (DeriveState maps nil to pending), so it can only come from a fake, and
// the conservative direction is to park.
func (c *Check) RedVerdict() bool {
	return c.State == StateFail && !ConclusionSuperseded(c.Conclusion)
}
