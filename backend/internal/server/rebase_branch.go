package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// CategoryBranchRebased is the audit-log category for the entry the
// rebase-branch handler writes when it advances a run/PR branch onto its
// declared base (E64.23 / #3125). It is the durable record of an
// operator-gated, runner-performed lineage write, and it drives a sticky
// status-comment refresh (an issue-comment surface).
//
// MECHANISM, stated plainly because the verb's NAME is misleading: the
// forge REST API exposes NO rebase primitive, so the handler performs a
// forge-side MERGE OF THE BASE INTO THE RUN BRANCH
// (POST /repos/{o}/{r}/merges with base=<run branch>, head=<base ref>).
// That leaves a MERGE COMMIT on the run branch and does NOT produce linear
// history. No force-push and no operator write is involved — the App
// installation stays the sole writer under ADR-035.
const CategoryBranchRebased = "branch_rebased"

// rebaseMechanismNote is the constant sentence shipped on EVERY 200 from
// the rebase handler (and echoed into the audit payload), so a reader of
// the response cannot infer linear history from the verb's name. It is
// also asserted verbatim-by-substring by the MCP tool description pin, so
// the mechanism claim cannot drift between the API and the tool surface.
const rebaseMechanismNote = "fishhawk_rebase_run_branch merges the declared base INTO the run branch server-side, leaving a merge commit; this is not a literal rebase and does not produce linear history."

// rebaseBranchRequest is the JSON body of POST /v0/runs/{run_id}/rebase-branch.
// Confirm MUST be true — the verb moves the run branch's head (leaving a
// merge commit and dismissing head-bound approvals), so it is never
// silent/auto: a missing or false confirm returns 400. Reason is an
// operator note recorded on the audit entry.
type rebaseBranchRequest struct {
	Reason  string `json:"reason"`
	Confirm bool   `json:"confirm"`
}

// rebaseBranchResponse summarizes a successful base advance.
//
// Both PriorHeadSHA and NewHeadSHA are reported, and NewHeadSHA is the
// AUTHORITATIVE head: it comes from a live PR re-read after the merge, not
// from MergeBranch's return. MergeCommitSHA is MergeBranch's own return and
// may legitimately be EMPTY (the deliberately benign undecodable-201 shape
// pinned by githubclient's TestMergeBranch_MergedMissingSHAIsBenign), which
// is why nothing in this handler treats it as a signal.
type rebaseBranchResponse struct {
	RunID          string `json:"run_id"`
	PRNumber       int    `json:"pr_number"`
	Branch         string `json:"branch"`
	BaseRef        string `json:"base_ref"`
	PriorHeadSHA   string `json:"prior_head_sha"`
	NewHeadSHA     string `json:"new_head_sha"`
	MergeCommitSHA string `json:"merge_commit_sha"`
	// AlreadyUpToDate reports that the run branch ALREADY contained the
	// declared base, so no merge was attempted on this invocation. It is
	// still a 200 that re-parks the gate and republishes the check at the
	// current head — that is what makes the retry the republish warning
	// advertises genuinely true.
	AlreadyUpToDate       bool   `json:"already_up_to_date"`
	ReparkedReviewStageID string `json:"reparked_review_stage_id,omitempty"`
	// MechanismNote is a constant sentence shipped on every 200 stating the
	// merge-commit mechanism, so no reader of this response can infer that a
	// literal rebase produced linear history.
	MechanismNote string `json:"mechanism_note"`
	// AuditCheckRepublished reports whether the fishhawk_audit_complete Check
	// Run was successfully re-posted at the new head. FALSE when the re-post
	// did not land — it errored, no publisher is wired, or the new head could
	// not be resolved at all (see AuditCheckRepublishWarning).
	AuditCheckRepublished bool `json:"audit_check_republished"`
	// AuditCheckRepublishWarning, when non-empty, names why the re-post did
	// not land and names re-invoking this verb as the idempotent retry.
	AuditCheckRepublishWarning string `json:"audit_check_republish_warning,omitempty"`
	// LineageAttributionWarning, when non-empty, reports that the merge
	// SUCCEEDED but its ADR-035 reported-head attribution is INCOMPLETE, so
	// this 200 must NOT be read as a clean recovery. It covers three cases,
	// each of which leaves the run wedged on the lineage check until the
	// operator acts:
	//
	//   - a CONCURRENT PUSH: the post-merge head diverged from the merge
	//     commit this invocation created, so the divergent head was
	//     deliberately NOT attributed (attributing it would launder a foreign
	//     commit into the ledger);
	//   - the attribution APPEND FAILED to persist;
	//   - NOTHING was attributable at all (an undecodable merge sha AND a
	//     failed post-merge re-read).
	//
	// In the last two cases re-invoking this verb does NOT repair the
	// attribution — the retry takes the already-contains-base arm, which
	// deliberately attributes nothing — so the warning names
	// fishhawk_vouch_commit as the required step instead.
	LineageAttributionWarning string `json:"lineage_attribution_warning,omitempty"`
}

// handleRebaseRunBranch implements POST /v0/runs/{run_id}/rebase-branch.
//
// It closes the half of #3109's Done-means that #3125 left open: an
// operator whose run branch has fallen BEHIND its declared base no longer
// has to resolve in a worktree and PUSH to a runner-owned branch. The
// RUNNER (the App installation, sole writer under ADR-035) advances its own
// lineage branch; the operator authorizes rather than performs the write.
//
// MECHANISM (see CategoryBranchRebased): a forge-side merge of the base
// INTO the run branch. It leaves a merge commit; it is not a literal
// rebase. Every operator-facing surface — the tool description, this
// response's mechanism_note, the OpenAPI text and both READMEs — states
// this, so the verb's name cannot mislead.
//
// Auth is operator-token-only, mirroring handleVouchCommit rather than
// reset-branch's softer subject-binding:
//
//   - anonymous → 401 authentication_required;
//   - a run-bound MCP token ("mcp:run:<uuid>") is REJECTED OUTRIGHT (403
//     run_token_forbidden), EVEN FOR ITS OWN RUN. Advancing a branch onto a
//     new base is a lineage-moving write the ADR-035 sole-writer invariant
//     reserves to an operator authorization; an agent self-authorizing it
//     would defeat exactly the invariant this verb exists to preserve;
//   - any identity without write:stages → 403 insufficient_scope, enforced
//     UNCONDITIONALLY with no cookie-session bypass.
//
// FAIL-CLOSED FIRST SLICE: a CONFLICTING merge produces a named
// rebase_conflict refusal that performs NO write and tells the operator
// what happened, cross-linking fishhawk_reset_run_branch (the sibling verb
// for a foreign commit pushed ON TOP, a different problem) and naming
// #3202 as where agent-driven conflict resolution is tracked.
//
// Two structural properties are load-bearing and deliberately NOT
// shortcuts:
//
//   - THE BEHIND-PROBE. "The branch already contains the base" is decided
//     BEFORE any merge, by CompareCommits(base=<live run branch head>,
//     head=<base ref>). GitHub's three-dot compare returns the commits on
//     head since its merge base with base, so zero commits ⟺ the base ref
//     is already an ancestor of the run branch head. Nothing here reads
//     MergeBranch's ambiguous ("", nil) as a discriminator, and
//     githubclient.MergeBranch / forge.Forge / the GitLab stub are
//     untouched.
//   - THE SHARED TAIL. The already-contains-base arm is a 200 that STILL
//     re-parks the gate and republishes the check at the current head. Both
//     arms fall into ONE re-park + audit + republish + notify path with a
//     resolved (newHead, mergeSHA, alreadyUpToDate) triple, so the retry the
//     republish warning advertises is real: invocation 1 merges and fails to
//     publish; the operator re-invokes; the probe short-circuits here and the
//     required check IS published at the correct post-merge head.
func (s *Server) handleRebaseRunBranch(w http.ResponseWriter, r *http.Request) {
	id := IdentityFrom(r.Context())
	if id.IsAnonymous() {
		s.writeError(w, r, http.StatusUnauthorized, "authentication_required",
			"an authenticated token is required", nil)
		return
	}
	// Operator-token-only: a run-bound agent token may NEVER advance its own
	// branch onto a new base, not even for its own run.
	if _, runBound := runBoundTokenRunID(id); runBound {
		s.writeError(w, r, http.StatusForbidden, "run_token_forbidden",
			"a run-bound agent token may not rebase its own run branch; advancing a run branch onto a new base is an operator action (ADR-035 sole-writer invariant)",
			nil)
		return
	}
	// Enforced UNCONDITIONALLY — no `id.TokenID != ""` cookie-session bypass.
	// A cookie session or any authenticated-but-unscoped identity must not be
	// able to move a run branch's head either.
	if !hasScope(id, "write:stages") {
		s.writeError(w, r, http.StatusForbidden, "insufficient_scope",
			"token is missing required scope: write:stages",
			map[string]any{"required_scope": "write:stages"})
		return
	}

	if s.cfg.RunRepo == nil || s.cfg.AuditRepo == nil || s.cfg.GitHub == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "rebase_unconfigured",
			"rebase-branch endpoint requires run + audit repositories and a GitHub client", nil)
		return
	}

	runID, err := uuid.Parse(r.PathValue("run_id"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"run_id must be a valid UUID",
			map[string]any{"field": "run_id", "got": r.PathValue("run_id")})
		return
	}

	var reqBody rebaseBranchRequest
	if r.Body != nil {
		if decErr := json.NewDecoder(r.Body).Decode(&reqBody); decErr != nil && !errors.Is(decErr, io.EOF) {
			s.writeError(w, r, http.StatusBadRequest, "validation_failed",
				"request body must be valid JSON {reason, confirm}",
				map[string]any{"error": decErr.Error()})
			return
		}
	}
	if !reqBody.Confirm {
		s.writeError(w, r, http.StatusBadRequest, "confirmation_required",
			"rebase-branch merges the declared base INTO the run branch, leaving a merge commit and moving the PR head; resend with confirm=true to proceed",
			map[string]any{"field": "confirm"})
		return
	}

	runRow, err := s.cfg.RunRepo.GetRun(r.Context(), runID)
	if err != nil {
		if errors.Is(err, run.ErrNotFound) {
			s.writeError(w, r, http.StatusNotFound, "run_not_found",
				"no run with that id", map[string]any{"run_id": runID.String()})
			return
		}
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"get run failed", map[string]any{"error": err.Error()})
		return
	}

	// Determinability ladder. Every unresolvable anchor is a fail-CLOSED
	// refusal — never a merge on an uncertain read.
	if runRow.InstallationID == nil || *runRow.InstallationID == 0 {
		s.writeRebaseNotDeterminable(w, r, "run has no installation to authorize a GitHub merge")
		return
	}
	scope := forge.FromGitHubInstallationID(*runRow.InstallationID)
	repo, err := parseRepoOwnerName(runRow.Repo)
	if err != nil {
		s.writeRebaseNotDeterminable(w, r, "run repo is unparseable: "+err.Error())
		return
	}
	prNumber := parsePRNumberFromURL(runRow.PullRequestURL)
	if prNumber <= 0 {
		s.writeRebaseNotDeterminable(w, r, "run has no tracked pull request to rebase")
		return
	}
	pr, err := s.cfg.GitHub.GetPullRequest(r.Context(), scope, repo, prNumber)
	if err != nil {
		s.writeRebaseNotDeterminable(w, r, "resolve live PR head failed: "+err.Error())
		return
	}
	headSHA, branch, baseRef := pr.HeadSHA, pr.HeadRef, pr.BaseRef
	if headSHA == "" || branch == "" || baseRef == "" {
		s.writeRebaseNotDeterminable(w, r, "PR returned an empty head sha, branch or base ref")
		return
	}

	// THE BEHIND-PROBE, taken BEFORE any merge. Three-dot compare
	// base=<live run branch head> ... head=<base ref> returns exactly the
	// commits the base advanced by that the run branch does not yet contain.
	// An error fails CLOSED — never a merge on an uncertain read.
	behind, err := s.cfg.GitHub.CompareCommits(r.Context(), scope, repo, headSHA, baseRef)
	if err != nil {
		s.writeRebaseNotDeterminable(w, r, "behind-probe compare failed: "+err.Error())
		return
	}

	newHead, mergeSHA := headSHA, ""
	alreadyUpToDate := len(behind) == 0
	mergePerformed := false
	republishWarning := ""

	if !alreadyUpToDate {
		// LEASE RE-CHECK — the only TOCTOU guard (the merges API has no
		// compare-and-swap). Re-read the live head and abort if it moved
		// since the probe, so a racing push is never silently merged over.
		livePR, lerr := s.cfg.GitHub.GetPullRequest(r.Context(), scope, repo, prNumber)
		if lerr != nil {
			s.writeRebaseNotDeterminable(w, r, "lease re-check: re-read live PR head failed: "+lerr.Error())
			return
		}
		if livePR.HeadSHA != headSHA {
			s.writeRebaseNotDeterminable(w, r,
				"lease re-check: the live PR head changed since the behind-probe (concurrent push); rebase aborted")
			return
		}

		// DIRECTION: the merges API's `base` is the branch that RECEIVES the
		// merge and `head` is the branch merged in. So base is the RUN BRANCH
		// and head is the BASE REF. An inversion here would merge the run
		// branch into the base branch — the worst failure available to this
		// verb — which is why the captured request body is asserted in test.
		msg := fmt.Sprintf("Advance run branch %s onto %s (fishhawk_rebase_run_branch, run %s)",
			branch, baseRef, runID.String())
		sha, merr := s.cfg.GitHub.MergeBranch(r.Context(), scope, repo, branch, baseRef, msg)
		if merr != nil {
			if errors.Is(merr, forge.ErrMergeConflict) {
				s.writeError(w, r, http.StatusUnprocessableEntity, "rebase_conflict",
					"the run branch conflicts with the advanced base; the merge was REFUSED and NOTHING was written (no merge commit, no audit entry, no check re-post). This first slice does not resolve conflicts — agent-driven conflict resolution is tracked in #3202. If the problem is instead a FOREIGN COMMIT pushed ON TOP of the run's commits rather than a base advance, fishhawk_reset_run_branch is the right verb.",
					map[string]any{
						"branch":   branch,
						"base_ref": baseRef,
						"error":    merr.Error(),
					})
				return
			}
			s.writeError(w, r, http.StatusBadGateway, "rebase_merge_failed",
				"merging the declared base into the run branch failed; nothing was written",
				map[string]any{"branch": branch, "base_ref": baseRef, "error": merr.Error()})
			return
		}
		mergePerformed = true
		mergeSHA = sha

		// The AUTHORITATIVE new head comes from a live PR re-read, never from
		// mergeSHA. This is what makes a decoded-201 and the deliberately
		// benign undecodable-201 ("", nil) behave IDENTICALLY.
		postPR, perr := s.cfg.GitHub.GetPullRequest(r.Context(), scope, repo, prNumber)
		if perr != nil || postPR.HeadSHA == "" {
			// The merge ALREADY happened, so a refusal here would misreport a
			// completed write. Return 200 with a warning instead — and
			// deliberately do NOT fall back to publishing at "no override":
			// that resolves to the pre-merge audit-recorded head, which is
			// precisely the staleness this verb exists to remove. Skipping
			// publication and relying on the idempotent retry is strictly
			// safer than pinning the required check to a stale head.
			newHead = ""
			reason := "the post-merge PR re-read returned an empty head"
			if perr != nil {
				reason = perr.Error()
			}
			republishWarning = "the base merge SUCCEEDED, but the resulting head could not be read back (" + reason +
				"), so the fishhawk_audit_complete check was NOT re-posted — publishing at the pre-merge head would pin the required check to a stale sha. Re-invoke fishhawk_rebase_run_branch to retry the re-post; the branch now contains the base, so the retry short-circuits the merge and publishes at the correct head."
		} else {
			newHead = postPR.HeadSHA
		}
	}

	// --- SHARED TAIL: re-park → audit → attribute → republish → notify ---
	// Reachable WITHOUT a merge on this invocation, which is what makes the
	// advertised retry true.

	reparkedID := ""
	if reparked, rerr := s.reparkReviewGateAfterHeadMove(r.Context(), runID); rerr != nil {
		s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn,
			"branch rebase: re-park review gate failed (best-effort)",
			slog.String("run_id", runID.String()),
			slog.String("error", rerr.Error()))
	} else if reparked != nil {
		reparkedID = reparked.ID.String()
	}

	// The branch_rebased entry is appended BEFORE the recompute so the
	// recompute observes it.
	s.writeBranchRebasedAudit(r, runID, prNumber, branch, baseRef,
		headSHA, newHead, mergeSHA, alreadyUpToDate, reqBody.Reason, reparkedID)

	// LINEAGE ATTRIBUTION (E64.23 / #3125). The merge commit this verb
	// creates is authored by the App installation but appears in NO
	// head-report audit category, so the ADR-035 reported-head ledger
	// (lineage.go buildReportedHeadLedger) would read it as FOREIGN and wedge
	// the very run this verb just un-wedged — verified by executing
	// TestRebaseRunBranch_LedgerAttributesTheMergeCommit against the REAL
	// ReverifyBranchLineage recompute, which goes RED without this call. The
	// fix is the confined one the vouch path already uses: union the SHAs into
	// the ledger via an operator_commit_vouched declaration, which
	// addVouchedSHAs reads.
	//
	// Attribution is written ONLY when THIS invocation performed the merge,
	// and covers EXACTLY ONE sha: the merge commit whose provenance this call
	// can prove. The already-contains-base arm deliberately attributes
	// NOTHING, and a post-merge head that DIVERGES from the merge commit is
	// likewise not attributed — vouching whatever head happens to be live
	// would silently launder a genuinely foreign pushed commit and defeat the
	// fail-closed property that makes reset-branch and vouch-commit
	// meaningful. An incomplete attribution is surfaced on the response as
	// lineage_attribution_warning, so a 200 is never read as a clean recovery
	// while the run is still wedged on the lineage check.
	lineageWarning := ""
	if mergePerformed {
		lineageWarning = s.writeRebaseLineageAttribution(r, runID, branch, baseRef, mergeSHA, newHead)
	}

	republished := false
	if newHead != "" {
		var pubErr error
		republished, pubErr = s.recomputeAndPublishAuditCompleteAtHead(r.Context(), runID, newHead)
		if pubErr != nil {
			republishWarning = "the base advance is recorded and durable, but re-posting the fishhawk_audit_complete check at the new head failed; the required check may be absent from the merge head — re-invoke fishhawk_rebase_run_branch to retry the re-post: " + pubErr.Error()
		}
	}

	s.notifyStatusUpdate(r.Context(), runID, "branch_rebased")

	s.writeJSON(w, r, http.StatusOK, rebaseBranchResponse{
		RunID:                      runID.String(),
		PRNumber:                   prNumber,
		Branch:                     branch,
		BaseRef:                    baseRef,
		PriorHeadSHA:               headSHA,
		NewHeadSHA:                 newHead,
		MergeCommitSHA:             mergeSHA,
		AlreadyUpToDate:            alreadyUpToDate,
		ReparkedReviewStageID:      reparkedID,
		MechanismNote:              rebaseMechanismNote,
		AuditCheckRepublished:      republished,
		AuditCheckRepublishWarning: republishWarning,
		LineageAttributionWarning:  lineageWarning,
	})
}

// writeRebaseNotDeterminable is the fail-CLOSED refusal: a 422 carrying the
// reason no merge could be attempted with certainty, so the operator learns
// WHY nothing was written. Mirrors writeResetNotDeterminable.
func (s *Server) writeRebaseNotDeterminable(w http.ResponseWriter, r *http.Request, reason string) {
	s.writeError(w, r, http.StatusUnprocessableEntity, "rebase_not_determinable",
		"cannot determine a safe base advance with certainty; refusing to write: "+reason,
		nil)
}

// writeBranchRebasedAudit appends the branch_rebased audit entry recording
// the full action — the prior head, the authoritative new head, the merge
// commit (which may legitimately be empty), whether the branch already
// contained the base, the operator reason, and the mechanism note — so the
// advance is auditable. Operator actor (never a silent system action).
// Best-effort like branch_reset: the write already happened, so an append
// failure WARNs rather than unwinding the response.
func (s *Server) writeBranchRebasedAudit(r *http.Request, runID uuid.UUID, prNumber int,
	branch, baseRef, priorHeadSHA, newHeadSHA, mergeCommitSHA string,
	alreadyUpToDate bool, reason, reparkedReviewStageID string) {
	id := IdentityFrom(r.Context())
	subject := id.Subject
	if subject == "" {
		subject = "anonymous"
	}
	actorKind := audit.ActorUser

	fields := map[string]any{
		"run_id":             runID.String(),
		"pr_number":          prNumber,
		"branch":             branch,
		"base_ref":           baseRef,
		"prior_head_sha":     priorHeadSHA,
		"new_head_sha":       newHeadSHA,
		"merge_commit_sha":   mergeCommitSHA,
		"already_up_to_date": alreadyUpToDate,
		"reason":             reason,
		"mechanism_note":     rebaseMechanismNote,
	}
	if reparkedReviewStageID != "" {
		fields["reparked_review_stage_id"] = reparkedReviewStageID
	}
	payload, _ := json.Marshal(fields)

	if _, err := s.cfg.AuditRepo.AppendChained(r.Context(), audit.ChainAppendParams{
		RunID:        runID,
		Timestamp:    time.Now().UTC(),
		Category:     CategoryBranchRebased,
		ActorKind:    &actorKind,
		ActorSubject: &subject,
		Payload:      payload,
	}); err != nil {
		s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn,
			"branch rebase: append branch_rebased audit entry failed",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()))
	}
}

// rebaseVouchRequiredNote is the shipped sentence appended to every
// LineageAttributionWarning whose case re-invoking this verb CANNOT repair.
// Re-invocation takes the already-contains-base arm, which deliberately
// attributes nothing, so advertising it here would be the same false-retry
// defect the republish warning was rejected for. fishhawk_vouch_commit is the
// verb that actually admits a sha into the ADR-035 reported-head ledger.
const rebaseVouchRequiredNote = " Re-invoking fishhawk_rebase_run_branch will NOT repair this: the retry takes the already-contains-base arm, which deliberately attributes nothing. Run fishhawk_vouch_commit against the run branch head to admit it into the ADR-035 ledger."

// writeRebaseLineageAttribution admits THIS INVOCATION'S merge commit into
// the ADR-035 reported-head ledger, using the SAME mechanism the vouch path
// uses (an operator_commit_vouched entry whose lineageVouchedSHAField
// addVouchedSHAs reads). Without it the installation-authored merge commit is
// in no head-report category and buildReportedHeadLedger flags it foreign,
// wedging the run on the very check this verb republishes.
//
// EXACTLY ONE SHA IS EVER ATTRIBUTED, and which one is decided by PROVENANCE
// rather than by availability. This is the fix for the post-merge attribution
// race: the lease re-check runs only BEFORE the merge, so a foreign push
// landing in the window between MergeBranch and the post-merge GetPullRequest
// becomes newHeadSHA. Vouching it would launder into the ledger precisely the
// foreign commit the ledger exists to catch — the same laundering the
// already-contains-base arm refuses, and that
// TestRebaseRunBranch_LedgerStillFlagsAnUnattributedForeignCommit exists to
// prevent.
//
//   - mergeCommitSHA NON-EMPTY: attribute ONLY mergeCommitSHA. It is the sha
//     the merges endpoint returned for the commit THIS call created, so it is
//     the only sha whose provenance this invocation can prove. A non-empty
//     newHeadSHA that DIFFERS from it is positive in-band evidence that
//     something else landed in the window: it is NOT attributed, and the
//     divergence is logged AND surfaced on the response so the operator
//     learns a concurrent push occurred.
//   - mergeCommitSHA EMPTY (the deliberately benign undecodable-201 shape
//     pinned by githubclient's TestMergeBranch_MergedMissingSHAIsBenign):
//     fall back to attributing newHeadSHA alone, because the merge
//     provably happened and there is nothing else to attribute.
//   - BOTH empty: nothing is attributable at all.
//
// The returned string is EMPTY on a clean attribution and otherwise names why
// the attribution is incomplete. It is surfaced on the response as
// lineage_attribution_warning, because a completed merge whose attribution
// did not land must NOT be reported as a clean recovery: the run stays wedged
// on the lineage check. "Load-bearing" and "best-effort with only a Warn log"
// are contradictory, so the append failure is still non-fatal to the already
// completed merge but is no longer SILENT.
func (s *Server) writeRebaseLineageAttribution(r *http.Request, runID uuid.UUID,
	branch, baseRef, mergeCommitSHA, newHeadSHA string) string {
	// Nothing attributable: an undecodable merge sha AND a failed post-merge
	// re-read. Re-invocation cannot repair this — say so rather than
	// advertising a retry that cannot deliver.
	if mergeCommitSHA == "" && newHeadSHA == "" {
		warning := "the base merge SUCCEEDED, but NEITHER the merge commit sha nor the post-merge head could be resolved, so NO lineage attribution was recorded; the merge commit is authored by the App installation and carries no head-report entry, so the ADR-035 ledger classifies it as FOREIGN and the run stays wedged." + rebaseVouchRequiredNote
		s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn,
			"branch rebase: no sha available to attribute; run is left un-attributed",
			slog.String("run_id", runID.String()))
		return warning
	}

	warning := ""
	// PROVENANCE, not availability: prefer the merge sha, and treat a
	// divergent live head as evidence of a concurrent push rather than as a
	// second sha to vouch.
	sha := mergeCommitSHA
	if sha == "" {
		sha = newHeadSHA
	} else if newHeadSHA != "" && newHeadSHA != mergeCommitSHA {
		warning = "the base merge SUCCEEDED and its merge commit " + mergeCommitSHA +
			" was attributed, but the post-merge head read back as " + newHeadSHA +
			", which DIFFERS from it — a concurrent push landed after the merge. That head was deliberately NOT attributed: vouching a commit this invocation did not create would launder a foreign commit into the ADR-035 ledger. Review the pushed commit and, if it is legitimate, admit it with fishhawk_vouch_commit."
		s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn,
			"branch rebase: post-merge head diverged from the merge commit; the divergent head was NOT attributed (concurrent push)",
			slog.String("run_id", runID.String()),
			slog.String("merge_commit_sha", mergeCommitSHA),
			slog.String("post_merge_head_sha", newHeadSHA))
	}

	id := IdentityFrom(r.Context())
	subject := id.Subject
	if subject == "" {
		subject = "anonymous"
	}
	actorKind := audit.ActorUser

	payload, _ := json.Marshal(map[string]any{
		"run_id":               runID.String(),
		lineageVouchedSHAField: sha,
		"reason": "fishhawk_rebase_run_branch advanced " + branch + " onto " + baseRef +
			"; this commit was created by the App installation on the operator's authorization (ADR-035 sole writer), not by a foreign pusher",
	})
	if _, err := s.cfg.AuditRepo.AppendChained(r.Context(), audit.ChainAppendParams{
		RunID:        runID,
		Timestamp:    time.Now().UTC(),
		Category:     CategoryOperatorCommitVouched,
		ActorKind:    &actorKind,
		ActorSubject: &subject,
		Payload:      payload,
	}); err != nil {
		s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn,
			"branch rebase: append lineage attribution failed; run is left un-attributed",
			slog.String("run_id", runID.String()),
			slog.String("sha", sha),
			slog.String("error", err.Error()))
		// The merge already happened, so this does not unwind the response —
		// but it is NOT reported as a clean success either.
		appendWarning := "the base merge SUCCEEDED, but persisting the operator_commit_vouched lineage attribution for " + sha +
			" FAILED (" + err.Error() + "), so the ADR-035 ledger still classifies the merge commit as FOREIGN and the run stays wedged." + rebaseVouchRequiredNote
		if warning != "" {
			return warning + " " + appendWarning
		}
		return appendWarning
	}
	return warning
}
