package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// The CHARTER ADMISSION GATE (ADR-065 / E54.4 / #2236).
//
// A backlog-grooming run reads the repo's charter as its prioritization
// anchor: every ranking it proposes cites a rubric line by id. A grooming run
// in a repo with no charter has nothing to anchor on, and ADR-065-as-amended
// says plainly that there is no unanchored-grooming mode. So a run whose
// resolved workflow produces a grooming report is REFUSED at admission when no
// charter is declared.
//
// WHERE THIS RULE IS ENFORCED, AND WHERE IT IS NOT (approval condition G3).
// Enforcement is at RUN ADMISSION — on EVERY seam that mints a run, which
// today means POST /v0/runs and the campaign item-run start — and nowhere
// else. It is
// deliberately NOT enforced by static validation (`fishhawk validate`, the
// CLI's spec check): the CLI does not parse work-management conventions
// anywhere today, so teaching static validation about the charter would mean
// giving the validator a conventions loader it has no other reason to have.
// Run admission is the LOAD-BEARING half — it is what makes starting an
// unanchored grooming run impossible — and it fails closed on a loader error
// too. The static half is tracked as its own follow-up. Do not read this gate
// as "a workflow declaring grooming_report will not validate": it will, and
// then it will not START.
//
// workflowRequiresCharter is kept PURE and EXPORTED-shaped (no receiver, no
// I/O) so wiring it into static validation later is a call rather than a
// rewrite.
//
// THE `haveStageDefs` GUARD IS A NARROWING, NOT THE SAFETY PROPERTY. The call
// site in runs.go sits inside the existing `haveStageDefs` admission region,
// beside checkAppliesTo, so it is fair to ask whether a grooming run could
// reach CreateRunForTrigger with that flag false and skip the gate. It cannot:
// BOTH spec-resolution branches in handleCreateRun set haveStageDefs true in
// the same statement that assigns workflowDef, and each rejects a zero-stage
// workflow before reaching it — so haveStageDefs == false means NO workflow
// definition was resolved at all, workflowDef is the zero spec.Workflow, and
// WorkflowRequiresCharter (which iterates wf.Stages) is false by construction.
// A grooming-capable workflow necessarily DECLARES a grooming_report-producing
// stage, so it can never sit behind that branch. Removing the guard therefore
// changes no outcome — verified by deleting it and re-running the suite green
// — which is exactly what makes the structural early return below, not the
// guard, the load-bearing control.
// TestCharterGate_NoStageDefsPathCarriesNoWorkflow pins the observable half.
//
// EVERY CreateRunForTrigger CALLER IS GATED, NOT JUST THE HTTP HANDLER
// (implement-review follow-up). CreateRunForTrigger has two callers:
// handleCreateRun and StartRunForCampaignIssue (the campaign item-run start,
// which the campaign driver also reaches). The campaign seam is NOT
// structurally unable to carry a grooming workflow the way the
// haveStageDefs=false branch is: it fetches the repo's spec from the forge and
// resolves an operator-named workflow_id out of it, so a repo whose spec
// declares a grooming_report-producing workflow can name it there. Documenting
// that seam as unreachable would therefore be false, so it is GATED instead —
// which is why the decision core below is factored into evaluateCharterAdmission
// and consumed by two arms: checkCharterDeclared (the HTTP arm, writing the 422)
// and ensureCharterDeclared (the ctx/error arm, refusing before the mint call).
// Both fire PRE-INSERT, so AC8's "refused before any run row exists" holds on
// both seams. TestCharterGate_CampaignSeam_* pins the campaign arm, and
// TestCharterGate_EveryCreateRunForTriggerCallerIsGated is the source-level pin
// that a THIRD caller cannot be added without a gate.

// charterRefusalReason enumerates WHY admission was refused. Three branches
// share the 422, so the reason is what makes them distinguishable to an
// operator and to a test.
const (
	// reasonCharterAbsent: conventions loaded, no charter block declared.
	reasonCharterAbsent = "charter_absent"
	// reasonCharterPathEmpty: a charter block whose path is empty/whitespace.
	reasonCharterPathEmpty = "charter_path_empty"
	// reasonConventionsUnavailable: the conventions could not be loaded at
	// all. FAIL-CLOSED, deliberately: admitting on a transient forge fault
	// would let a grooming run start unanchored, and the loader's own
	// documented posture is that a fetch/parse failure fails closed.
	reasonConventionsUnavailable = "conventions_unavailable"
)

// conventionsPathForMessage is the repo-relative path the refusal message
// points the operator at.
const conventionsPathForMessage = ".fishhawk/work-management.yaml"

// WorkflowRequiresCharter reports whether a workflow is a BACKLOG-GROOMING
// workflow for the charter rule's purposes.
//
// The discriminator is STRUCTURAL: a stage declaring the grooming_report
// artifact. It is deliberately NOT the workflow's NAME (a name-keyed gate is
// evaded by renaming the workflow) and deliberately NOT a `kind:` field (AC1
// forbids a workflow-type discriminator — the grooming workflow is built on
// the standard plan/implement/review stage types and is recognised by what it
// PRODUCES).
//
// Pure: no I/O, no receiver, no request context. That is what lets a later
// static-validation pass call it directly.
func WorkflowRequiresCharter(wf spec.Workflow) bool {
	for _, st := range wf.Stages {
		for _, p := range st.Produces {
			if p.Artifact == spec.ArtifactGroomingReport {
				return true
			}
		}
	}
	return false
}

// errCharterRequired is the sentinel the ctx/error arm wraps, so a caller (and
// a test) can identify THIS refusal rather than pattern-matching a message.
var errCharterRequired = errors.New("charter required")

// charterAdmissionReason maps a conventions load OUTCOME to a refusal reason,
// or "" to admit. Pure — no receiver, no I/O — for the same reason
// WorkflowRequiresCharter is: it is the half a later static-validation pass
// would reuse, and it is what makes the two arms below provably identical in
// their verdict rather than identical-looking.
func charterAdmissionReason(conv workmgmt.Conventions, err error) string {
	switch {
	case err != nil:
		return reasonConventionsUnavailable
	case conv.Charter == nil:
		return reasonCharterAbsent
	case strings.TrimSpace(conv.Charter.Path) == "":
		return reasonCharterPathEmpty
	}
	return ""
}

// charterRefusalMessage renders the actionable refusal text both arms carry.
func charterRefusalMessage(repo, workflowID, reason string) string {
	message := "workflow " + workflowID + " in " + repo + " produces a grooming report, but no backlog charter is declared: " +
		"a grooming run ranks the backlog against the charter's rubric, and there is no unanchored-grooming mode. " +
		"Declare a `charter:` block with its `path:` key in " + conventionsPathForMessage +
		" pointing at the checked-in charter document (conventionally .fishhawk/charter.md), then start the run again."
	if reason == reasonConventionsUnavailable {
		message += " The work-management conventions could not be read for this repo, and an unreadable conventions file is refused rather than assumed to declare a charter."
	}
	return message
}

// evaluateCharterAdmission is the DECISION CORE both admission arms share. It
// returns "" to ADMIT, or the refusal reason — having already appended the
// best-effort refusal audit entry.
//
// Sharing the core is what makes "every run-minting seam is gated" a property
// of one function rather than of two copies that can drift: the HTTP arm and
// the campaign arm differ only in how they REPORT the verdict, never in the
// verdict.
func (s *Server) evaluateCharterAdmission(ctx context.Context, repo, workflowID string, wf spec.Workflow) string {
	if !WorkflowRequiresCharter(wf) {
		// Narrow by construction: an ordinary code-change workflow never
		// reaches the conventions loader, so this gate cannot refuse a run it
		// has no business refusing.
		return ""
	}

	conv, err := conventionsLoader(ctx, repo)
	reason := charterAdmissionReason(conv, err)
	if reason == "" {
		return ""
	}

	// The refusal audit is BEST-EFFORT — the deliberate asymmetry
	// grantAppliesToOverride documents (#2361). A REFUSAL is the safe outcome,
	// so failing to record it must not convert an audit outage into a
	// governance outage; a GRANT records an exception being made and its
	// append is a precondition. This gate issues no grants.
	if s.cfg.AuditRepo != nil {
		payload, _ := json.Marshal(map[string]any{
			"reason":            reason,
			"repo":              repo,
			"workflow_id":       workflowID,
			"conventions_path":  conventionsPathForMessage,
			"required_artifact": string(spec.ArtifactGroomingReport),
		})
		systemKind := audit.ActorKind("system")
		if _, aerr := s.cfg.AuditRepo.AppendGlobalChained(ctx, audit.GlobalChainAppendParams{
			Timestamp: time.Now().UTC(),
			Category:  "run_rejected_missing_charter",
			ActorKind: &systemKind,
			Payload:   payload,
			AccountID: identityAccountID(ctx),
		}); aerr != nil {
			s.cfg.Logger.Warn("append run_rejected_missing_charter audit entry failed",
				"repo", repo, "workflow_id", workflowID, "reason", reason, "error", aerr.Error())
		}
	}
	return reason
}

// checkCharterDeclared is the HTTP admission arm. It returns true to ADMIT and
// false having already written the refusal response.
//
// It is called pre-insert (a refusal leaves no run row) and POST-REPLAY (an
// Idempotency-Key resolving to an existing run short-circuits before it, so a
// replay re-evaluates no configuration decision and appends no second entry) —
// the same seam and the same two properties checkAppliesTo documents.
func (s *Server) checkCharterDeclared(w http.ResponseWriter, r *http.Request, repo, workflowID string, wf spec.Workflow) bool {
	reason := s.evaluateCharterAdmission(r.Context(), repo, workflowID, wf)
	if reason == "" {
		return true
	}

	s.writeError(w, r, http.StatusUnprocessableEntity, "charter_required",
		charterRefusalMessage(repo, workflowID, reason), map[string]any{
			"workflow_id":      workflowID,
			"conventions_path": conventionsPathForMessage,
			"reason":           reason,
		})
	return false
}

// ensureCharterDeclared is the ctx/error admission arm, for run-minting seams
// that hold no ResponseWriter — today StartRunForCampaignIssue, which the
// campaign item-run endpoint and the campaign driver both reach.
//
// It returns nil to ADMIT, or an errCharterRequired-wrapped error carrying the
// same reason and the same actionable message the 422 carries. The caller
// refuses BEFORE CreateRunForTrigger, so AC8's "refused before any run row
// exists" holds identically on this seam.
func (s *Server) ensureCharterDeclared(ctx context.Context, repo, workflowID string, wf spec.Workflow) error {
	reason := s.evaluateCharterAdmission(ctx, repo, workflowID, wf)
	if reason == "" {
		return nil
	}
	return fmt.Errorf("%w (%s): %s", errCharterRequired, reason, charterRefusalMessage(repo, workflowID, reason))
}
