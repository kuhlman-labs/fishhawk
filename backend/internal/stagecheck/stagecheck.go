// Package stagecheck records and reads the state of each blocking
// check declared on a workflow-spec gate (#228). Two writers feed
// it today: GitHub `check_run` webhook events for ci_pass-style
// external checks, and (in #229) the backend's own audit-completeness
// derivation. Two readers consume it: the review-stage detail page
// (read-only render) and the approval handler (gate enforcement).
//
// Rows are append-only — every status update writes a new row, and
// the latest per (stage_id, check_name) is what consumers see. The
// retention story matches audit_entries: nothing is ever mutated
// or deleted.
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
	Timestamp        time.Time
	Payload          json.RawMessage
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
