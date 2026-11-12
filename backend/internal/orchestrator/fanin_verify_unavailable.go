package orchestrator

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// CategoryFanInVerifyUnavailable is the audit category of the best-effort row
// the orchestrator appends when a decomposed parent's acceptance stage is
// RELEASED with no consolidated fan-in verify consulted (#4792). The row is the
// durable half of the signal (the WARN log is the other): GET /v0/runs/{run_id}
// distills the newest one into the run's fan_in_verify_unavailable block.
//
// It is an INTERNAL audit kind. It is deliberately absent from
// server.activityCategories, so it never renders an issue-thread activity line
// (docs/issue-comment-surfaces.md). It gates nothing: a release is recorded,
// never held, and the ADR-090 merge gate still applies to the unverified head.
const CategoryFanInVerifyUnavailable = "fan_in_verify_unavailable"

// The closed set of reasons a decomposed parent's acceptance is released with
// no fan-in verify consulted (#4792). Each names ONE fail-open arm:
//
//   - no_run_repo: the FanInVerifyGate hook has no run repository, so it cannot
//     read the run (server.FanInVerifySettled).
//   - forge_not_wired: the hook cannot compare the consolidated branch because
//     no forge client / installation / repo / family resolves (every
//     forgeCompareFor reason; the verbatim text is the detail).
//   - default_branch_unresolved: the fan-in base could not be read, so the hook
//     is never consulted (detail is defaultbranch.ReasonOf's closed vocabulary).
//   - hook_not_wired: FanInVerify or ConsolidatedReview is nil (the CLI/dev
//     posture), so there is no hold to apply.
//   - no_consolidated_pr: the parent carries no consolidated PR URL because the
//     graceful PR-open skip fired (no GitHub client / no installation id), so
//     the hook is skipped altogether.
const (
	FanInVerifyUnavailableNoRunRepo               = "no_run_repo"
	FanInVerifyUnavailableForgeNotWired           = "forge_not_wired"
	FanInVerifyUnavailableDefaultBranchUnresolved = "default_branch_unresolved"
	FanInVerifyUnavailableHookNotWired            = "hook_not_wired"
	FanInVerifyUnavailableNoConsolidatedPR        = "no_consolidated_pr"
)

// FanInVerifyUnavailablePayload is the audit payload of a
// fan_in_verify_unavailable row. The server decodes it to build the run-status
// block, so the JSON keys are the contract.
type FanInVerifyUnavailablePayload struct {
	Reason             string `json:"reason"`
	Detail             string `json:"detail,omitempty"`
	AcceptanceStageID  string `json:"acceptance_stage_id"`
	ConsolidatedBranch string `json:"consolidated_branch"`
}

// recordFanInVerifyUnavailable makes ONE fail-open release observable: it logs
// a WARN, then appends a deduplicated fan_in_verify_unavailable audit row
// anchored on the acceptance stage. Best-effort, mirroring
// emitAcceptanceDispatched: it never unwinds the release, a nil Audit logs only,
// a dedupe read failure still appends (a read failure must never lose the
// signal), and an append failure only WARNs.
func (o *Orchestrator) recordFanInVerifyUnavailable(ctx context.Context, r *run.Run, acceptance *run.Stage, reason, detail string) {
	branch := consolidatedBranch(r.ID)
	o.logger().LogAttrs(ctx, slog.LevelWarn,
		"orchestrator: fan_in_verify_unavailable: decomposed parent acceptance released with no consolidated fan-in verify consulted; the merge gate still applies",
		slog.String("run_id", r.ID.String()),
		slog.String("acceptance_stage_id", acceptance.ID.String()),
		slog.String("reason", reason),
		slog.String("detail", detail))
	if o.Audit == nil {
		return
	}
	if o.fanInVerifyUnavailableRecorded(ctx, r.ID, acceptance.ID, reason) {
		return
	}
	// A struct of four strings cannot fail to marshal.
	payload, _ := json.Marshal(FanInVerifyUnavailablePayload{
		Reason:             reason,
		Detail:             detail,
		AcceptanceStageID:  acceptance.ID.String(),
		ConsolidatedBranch: branch,
	})
	systemKind := audit.ActorSystem
	stageID := acceptance.ID
	if _, err := o.Audit.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     r.ID,
		StageID:   &stageID,
		Timestamp: time.Now().UTC(),
		Category:  CategoryFanInVerifyUnavailable,
		ActorKind: &systemKind,
		Payload:   payload,
	}); err != nil {
		o.logger().LogAttrs(ctx, slog.LevelWarn, "orchestrator: append fan_in_verify_unavailable failed",
			slog.String("run_id", r.ID.String()),
			slog.String("error", err.Error()))
	}
}

// fanInVerifyUnavailableRecorded reports whether the run already carries a
// fan_in_verify_unavailable row for the same acceptance stage AND reason. A
// dispatch error followed by a retry re-enters the release arm, so the dedupe
// bounds the chain to one row per (stage, reason). A list error WARNs and
// answers false, and an undecodable row is no match: a read failure can never
// lose the signal. Two concurrent Advance calls can both pass this read and
// write an identical row; readers take the newest, so that race is accepted.
func (o *Orchestrator) fanInVerifyUnavailableRecorded(ctx context.Context, runID, acceptanceID uuid.UUID, reason string) bool {
	rows, err := o.Audit.ListForRunByCategory(ctx, runID, CategoryFanInVerifyUnavailable)
	if err != nil {
		o.logger().LogAttrs(ctx, slog.LevelWarn, "orchestrator: list fan_in_verify_unavailable rows failed; appending without the dedupe read",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()))
		return false
	}
	for _, row := range rows {
		if row == nil {
			continue
		}
		var p FanInVerifyUnavailablePayload
		if json.Unmarshal(row.Payload, &p) != nil {
			continue
		}
		if p.AcceptanceStageID == acceptanceID.String() && p.Reason == reason {
			return true
		}
	}
	return false
}

// fanInHookMissing names the nil gate field(s) of the hook_not_wired arm.
func (o *Orchestrator) fanInHookMissing() string {
	switch {
	case o.FanInVerify == nil && o.ConsolidatedReview == nil:
		return "FanInVerify and ConsolidatedReview not wired"
	case o.FanInVerify == nil:
		return "FanInVerify not wired"
	default:
		return "ConsolidatedReview not wired"
	}
}

// noConsolidatedPRDetail names why the PR open skipped gracefully: the only two
// graceful skips in openConsolidatedPRLocked are a nil GitHub client and a run
// with no installation id.
func (o *Orchestrator) noConsolidatedPRDetail(_ *run.Run) string {
	if o.GitHub == nil {
		return "github client not wired"
	}
	return "no installation id"
}
