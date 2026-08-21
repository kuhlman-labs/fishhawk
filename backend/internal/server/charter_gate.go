package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
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
// Enforcement is at RUN ADMISSION — POST /v0/runs — and nowhere else. It is
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

// checkCharterDeclared is the admission gate. It returns true to ADMIT and
// false having already written the refusal response.
//
// It is called pre-insert (a refusal leaves no run row) and POST-REPLAY (an
// Idempotency-Key resolving to an existing run short-circuits before it, so a
// replay re-evaluates no configuration decision and appends no second entry) —
// the same seam and the same two properties checkAppliesTo documents.
func (s *Server) checkCharterDeclared(w http.ResponseWriter, r *http.Request, repo, workflowID string, wf spec.Workflow) bool {
	if !WorkflowRequiresCharter(wf) {
		// Narrow by construction: an ordinary code-change workflow never
		// reaches the conventions loader, so this gate cannot refuse a run it
		// has no business refusing.
		return true
	}

	conv, err := conventionsLoader(r.Context(), repo)
	var reason string
	switch {
	case err != nil:
		reason = reasonConventionsUnavailable
	case conv.Charter == nil:
		reason = reasonCharterAbsent
	case strings.TrimSpace(conv.Charter.Path) == "":
		reason = reasonCharterPathEmpty
	default:
		return true
	}

	message := "workflow " + workflowID + " in " + repo + " produces a grooming report, but no backlog charter is declared: " +
		"a grooming run ranks the backlog against the charter's rubric, and there is no unanchored-grooming mode. " +
		"Declare a `charter:` block with its `path:` key in " + conventionsPathForMessage +
		" pointing at the checked-in charter document (conventionally .fishhawk/charter.md), then start the run again."
	if reason == reasonConventionsUnavailable {
		message += " The work-management conventions could not be read for this repo, and an unreadable conventions file is refused rather than assumed to declare a charter."
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
		if _, aerr := s.cfg.AuditRepo.AppendGlobalChained(r.Context(), audit.GlobalChainAppendParams{
			Timestamp: time.Now().UTC(),
			Category:  "run_rejected_missing_charter",
			ActorKind: &systemKind,
			Payload:   payload,
			AccountID: identityAccountID(r.Context()),
		}); aerr != nil {
			s.cfg.Logger.Warn("append run_rejected_missing_charter audit entry failed",
				"repo", repo, "workflow_id", workflowID, "reason", reason, "error", aerr.Error())
		}
	}

	s.writeError(w, r, http.StatusUnprocessableEntity, "charter_required", message, map[string]any{
		"workflow_id":      workflowID,
		"conventions_path": conventionsPathForMessage,
		"reason":           reason,
	})
	return false
}
