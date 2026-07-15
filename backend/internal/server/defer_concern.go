package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// CategoryConcernDeferred is the audit-log category written when an
// operator defers an open review concern into a follow-up work item
// (E22.X / #1202). Per the binding audit-ordering invariant it is
// appended ONLY AFTER the concern's state transition to `deferred`
// succeeds — it is a FACT that the defer landed, never an attempt. The
// payload carries the concern's stable id, prior state, the filed
// issue's number + url, and the reason. Documented in
// docs/issue-comment-surfaces.md.
const CategoryConcernDeferred = "concern_deferred"

// CategoryConcernDeferFailed is the corrective audit-log category
// appended (warn-only) when the issue was filed but the state
// transition then failed (a concurrent transition raced the defer). It
// names the actual state the concern was found in AND the orphaned
// issue url, so an operator can reconcile the durable external side
// effect. It is the ONLY audit entry emitted on the post-filing race —
// the success concern_deferred entry is never written for a transition
// that did not happen.
const CategoryConcernDeferFailed = "concern_defer_failed"

// deferConcernRequest is the JSON body of
// POST /v0/concerns/{concern_id}/defer. The body is fully auto-drafted
// from the persisted concern + its run; the operator supplies only the
// parent_epic placement and optional overrides, mirroring
// fishhawk_file_issue.
type deferConcernRequest struct {
	// ParentEpic is the epic the follow-up rolls up to (an issue
	// reference like "#1196"); its leading [E<n>] title token is fetched
	// to derive the {epic} title placeholder. Not derivable from the
	// concern — the follow-up's epic placement is an operator judgment.
	ParentEpic string `json:"parent_epic,omitempty"`
	// N is the child number for the [E<epic>.<n>] title format. Optional:
	// when omitted the backend discovers it server-side from the parent
	// epic's existing children (open and closed) and allocates the next one
	// (#1958), so the operator no longer has to guess it. A supplied value
	// is an explicit override that short-circuits discovery.
	N string `json:"n,omitempty"`
	// Type overrides the auto-selected work-item type (bug for a defect
	// category, else chore). Optional.
	Type string `json:"type,omitempty"`
	// Labels are merged on top of the type's default labels.
	Labels []string `json:"labels,omitempty"`
	// Note is an optional operator addendum folded into the follow-up
	// body and the concern's state_reason.
	Note string `json:"note,omitempty"`
}

// deferredConcern is the updated concern row returned on a successful
// defer: state deferred, state_reason referencing the filed issue.
type deferredConcern struct {
	ID          uuid.UUID `json:"id"`
	RunID       uuid.UUID `json:"run_id"`
	StageID     uuid.UUID `json:"stage_id"`
	StageKind   string    `json:"stage_kind"`
	Severity    string    `json:"severity"`
	Category    string    `json:"category"`
	Note        string    `json:"note"`
	State       string    `json:"state"`
	StateReason string    `json:"state_reason"`
}

// deferFiledIssue is the filed follow-up work item returned alongside
// the updated concern.
type deferFiledIssue struct {
	Type          string   `json:"type"`
	Title         string   `json:"title"`
	Number        int      `json:"number"`
	URL           string   `json:"url"`
	Provider      string   `json:"provider"`
	AppliedLabels []string `json:"applied_labels,omitempty"`
	// DefaultedLabels / MissingLabelNamespaces mirror the work-item filing's
	// LOUD label-completeness report (#1616): the auto-draft flows through the
	// same applyAndFileWorkItem core, so an autonomy default (or a missing
	// required namespace) on a deferred follow-up is surfaced identically.
	DefaultedLabels        []string `json:"defaulted_labels,omitempty"`
	MissingLabelNamespaces []string `json:"missing_label_namespaces,omitempty"`
}

// deferConcernResponse is the 200 body: the filed follow-up work item
// plus the now-deferred concern row.
type deferConcernResponse struct {
	Concern deferredConcern `json:"concern"`
	Issue   deferFiledIssue `json:"issue"`
}

// handleDeferConcern implements POST /v0/concerns/{concern_id}/defer
// (E22.X / #1202): the operator verb that converts one OPEN review
// concern into a conventions-complete, boarded, epic-linked follow-up
// work item in a single call, transitioning the concern to the terminal
// `deferred` state that references the filed issue.
//
// Auth is byte-identical to handleWaiveConcern: authenticated, the same
// write:stages / write:fixups scope pair, and the same mcp:run:<uuid>
// subject-binding guard (a run-bound token may defer only its own run's
// concerns). The concern is resolved AFTER the scope check so an
// unscoped token cannot probe concern IDs.
//
// Orphan-issue safety (a GitHub issue is a durable external side
// effect): the handler files FIRST then transitions, with an open-state
// PRE-CHECK before any filing so the provider's File is NEVER called for
// an already-resolved concern. On a successful filing it transitions the
// concern and ONLY THEN appends the concern_deferred audit entry (audit
// categories are facts, not attempts). If the transition fails after a
// successful filing (a concurrent writer raced it), a corrective
// concern_defer_failed entry is appended naming the actual state + the
// orphaned issue url, and the request returns 422 — never a success
// concern_deferred entry for a transition that did not happen.
func (s *Server) handleDeferConcern(w http.ResponseWriter, r *http.Request) {
	id := IdentityFrom(r.Context())
	if id.IsAnonymous() {
		s.writeError(w, r, http.StatusUnauthorized, "authentication_required",
			"an authenticated token is required", nil)
		return
	}
	if id.TokenID != "" && !hasScope(id, "write:stages") && !hasScope(id, "write:fixups") {
		s.writeError(w, r, http.StatusForbidden, "insufficient_scope",
			"token is missing required scope: write:stages or write:fixups",
			map[string]any{"required_scope": "write:stages or write:fixups"})
		return
	}

	if s.cfg.ConcernRepo == nil || s.cfg.AuditRepo == nil || s.cfg.RunRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "concern_store_unconfigured",
			"defer endpoint requires concern + audit + run repositories", nil)
		return
	}

	concernID, err := uuid.Parse(r.PathValue("concern_id"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"concern_id must be a valid UUID",
			map[string]any{"field": "concern_id", "got": r.PathValue("concern_id")})
		return
	}

	var reqBody deferConcernRequest
	if r.Body != nil {
		if decErr := json.NewDecoder(r.Body).Decode(&reqBody); decErr != nil {
			s.writeError(w, r, http.StatusBadRequest, "validation_failed",
				"request body must be valid JSON",
				map[string]any{"error": decErr.Error()})
			return
		}
	}

	rows, err := s.cfg.ConcernRepo.GetByIDs(r.Context(), []uuid.UUID{concernID})
	if err != nil {
		if errors.Is(err, concern.ErrNotFound) {
			s.writeError(w, r, http.StatusNotFound, "concern_not_found",
				"no concern with that id", nil)
			return
		}
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"get concern failed", map[string]any{"error": err.Error()})
		return
	}
	row := rows[0]

	// Subject-binding guard: an MCP run-bound token may only defer
	// concerns within its own run. Byte-identical to handleWaiveConcern.
	if strings.HasPrefix(id.Subject, "mcp:run:") {
		runIDStr := strings.TrimPrefix(id.Subject, "mcp:run:")
		subjectRunID, parseErr := uuid.Parse(runIDStr)
		if parseErr != nil {
			s.writeError(w, r, http.StatusUnauthorized, "authentication_required",
				"mcp token subject is malformed", nil)
			return
		}
		if subjectRunID != row.RunID {
			s.writeError(w, r, http.StatusForbidden, "cross_run_defer",
				"mcp token may only defer concerns within its own run",
				map[string]any{
					"token_run_id":   subjectRunID.String(),
					"concern_run_id": row.RunID.String(),
				})
			return
		}
	}

	// Orphan-issue-safe PRE-CHECK: a closed concern (already waived,
	// superseded, deferred, or addressed) must NOT file an issue. Reject
	// before any provider call so no durable external side effect is
	// created for a concern that cannot transition.
	if !row.State.IsOpen() {
		s.writeError(w, r, http.StatusUnprocessableEntity, "concern_defer_conflict",
			"concern is not in an open state; only raised/addressed_pending/reopened concerns can be deferred",
			map[string]any{"state": string(row.State)})
		return
	}

	// Resolve the concern's OWN run (not a caller-supplied run_id) for the
	// repo, installation, and PR link. Because the run is derived from the
	// concern the caller is already authorized to act on, this does NOT go
	// through handleFileWorkItem's run-id entitlement door — there is no
	// confused-deputy surface to close here.
	rn, err := s.cfg.RunRepo.GetRun(r.Context(), row.RunID)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"could not resolve the concern's run",
			map[string]any{"error": err.Error(), "run_id": row.RunID.String()})
		return
	}
	owner, name, ok := splitRepoFullName(rn.Repo)
	if !ok {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"the concern's run has a malformed repo coordinate",
			map[string]any{"repo": rn.Repo})
		return
	}

	conv, err := conventionsLoader(rn.Repo)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"could not load work-management conventions", map[string]any{"error": err.Error()})
		return
	}

	itemType := strings.TrimSpace(reqBody.Type)
	if itemType == "" {
		itemType = deferTypeForCategory(row.Category)
	}

	filing := workmgmt.FilingRequest{
		Type:    itemType,
		Summary: deferSummary(row.Note),
		Body:    deferBody(row, rn, reqBody.Note),
		Labels:  reqBody.Labels,
		Relations: workmgmt.Relations{
			ParentEpic:   reqBody.ParentEpic,
			EvidenceRuns: []string{row.RunID.String()},
		},
	}
	if n := strings.TrimSpace(reqBody.N); n != "" {
		filing.TitleVars = map[string]string{"n": n}
	}

	target := workmgmt.Target{
		Repo:    workmgmt.Repo{Owner: owner, Name: name},
		Project: conv.Project,
		Jira:    conv.Jira,
	}
	// InstallationID comes from the concern's resolved run; fall back to
	// resolving the App installation for the target repo when the run
	// carries none (mirroring workitems.go). No run-bound rejection here:
	// the installation belongs to the concern's own run, not an arbitrary
	// caller-named repo.
	if rn.InstallationID != nil {
		target.InstallationID = *rn.InstallationID
	}
	if target.InstallationID == 0 && s.cfg.GitHub != nil {
		instID, rerr := s.cfg.GitHub.GetRepoInstallation(r.Context(), githubclient.RepoRef{Owner: owner, Name: name})
		switch {
		case rerr == nil:
			target.InstallationID = instID
		case errors.Is(rerr, githubclient.ErrNotInstalled):
			// App genuinely not installed: leave 0 so the provider fails
			// closed with its own actionable typed error.
		default:
			s.writeError(w, r, http.StatusBadGateway, "work_item_filing_failed",
				"could not resolve the GitHub App installation for the concern's repo",
				map[string]any{"error": rerr.Error()})
			return
		}
	}

	// Snapshot the prior state before any transition: ApplyResolution
	// mutates the concern row, so prior_state must be captured here for
	// the success audit FACT (#1202 done-means asserts it).
	priorState := string(row.State)

	// File FIRST. A filing failure leaves the concern untouched (still
	// OPEN) — no mutation, no audit entry — so the operator can retry.
	item, created, werr := s.applyAndFileWorkItem(r.Context(), filing, conv, target, owner, name)
	if werr != nil {
		s.writeError(w, r, werr.status, werr.code, werr.msg, werr.details)
		return
	}

	// Transition the concern to deferred, its reason naming the filed
	// issue. The audit FACT (concern_deferred) is written only AFTER this
	// succeeds (binding ordering invariant).
	stateReason := fmt.Sprintf("deferred to #%d: %s", created.Number, deferReasonNote(row.Note, reqBody.Note))
	updated, err := s.cfg.ConcernRepo.ApplyResolution(r.Context(), concernID, concern.StateDeferred, stateReason)
	if err != nil {
		// The issue is already filed (orphaned relative to the un-transitioned
		// concern). Keep the chain truthful with the corrective entry naming
		// the actual state + the orphaned issue url, and never a success
		// concern_deferred entry for a transition that did not happen.
		s.writeConcernDeferFailedAudit(r, row, created, err)
		var bad concern.InvalidTransitionError
		if errors.As(err, &bad) {
			s.writeError(w, r, http.StatusUnprocessableEntity, "concern_defer_conflict",
				err.Error(),
				map[string]any{
					"from":         string(bad.From),
					"to":           string(bad.To),
					"filed_issue":  created.URL,
					"filed_number": created.Number,
				})
			return
		}
		if errors.Is(err, concern.ErrNotFound) {
			s.writeError(w, r, http.StatusNotFound, "concern_not_found",
				"no concern with that id", map[string]any{"filed_issue": created.URL})
			return
		}
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"defer concern failed after the issue was filed",
			map[string]any{"error": err.Error(), "filed_issue": created.URL})
		return
	}

	// SUCCESS audit (fact): the transition landed, so record the defer.
	subject := id.Subject
	if subject == "" {
		subject = "anonymous"
	}
	actorKind := actorKindForSubject(subject)
	payload, _ := json.Marshal(map[string]any{
		"concern_id":     row.ID.String(),
		"prior_state":    priorState,
		"reason":         stateReason,
		"stage_kind":     row.StageKind,
		"severity":       row.Severity,
		"category":       row.Category,
		"issue_number":   created.Number,
		"issue_url":      created.URL,
		"issue_type":     item.Type,
		"issue_title":    item.Title,
		"issue_provider": created.Provider,
	})
	if _, aerr := s.cfg.AuditRepo.AppendChained(r.Context(), audit.ChainAppendParams{
		RunID:        row.RunID,
		StageID:      &row.StageID,
		Timestamp:    time.Now().UTC(),
		Category:     CategoryConcernDeferred,
		ActorKind:    &actorKind,
		ActorSubject: &subject,
		Payload:      payload,
	}); aerr != nil {
		// The mutation + the durable issue already landed; the audit FACT
		// is best-effort here (warn-only) so a transient append failure does
		// not fail a defer the operator already committed. Unlike waive
		// (audit-before-mutation), the binding ordering for defer is
		// audit-AFTER-transition, so a hard-fail here would leave a deferred
		// concern with no way to un-defer.
		s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn,
			"defer: append concern_deferred audit entry failed",
			slog.String("run_id", row.RunID.String()),
			slog.String("concern_id", row.ID.String()),
			slog.String("error", aerr.Error()))
	}

	s.writeJSON(w, r, http.StatusOK, deferConcernResponse{
		Concern: deferredConcern{
			ID:          updated.ID,
			RunID:       updated.RunID,
			StageID:     updated.StageID,
			StageKind:   updated.StageKind,
			Severity:    updated.Severity,
			Category:    updated.Category,
			Note:        updated.Note,
			State:       string(updated.State),
			StateReason: updated.StateReason,
		},
		Issue: deferFiledIssue{
			Type:                   item.Type,
			Title:                  item.Title,
			Number:                 created.Number,
			URL:                    created.URL,
			Provider:               created.Provider,
			AppliedLabels:          created.AppliedLabels,
			DefaultedLabels:        item.Classification.DefaultedLabels,
			MissingLabelNamespaces: item.Classification.MissingLabelNamespaces,
		},
	})
}

// writeConcernDeferFailedAudit appends the corrective concern_defer_failed
// entry after a transition failure that followed a SUCCESSFUL filing. It
// names the actual state the concern was found in AND the orphaned issue
// url so the operator can reconcile the durable side effect.
// Best-effort/warn-only: the 4xx/5xx response already tells the operator
// the defer did not land.
func (s *Server) writeConcernDeferFailedAudit(r *http.Request, row *concern.Concern, created *workmgmt.CreatedItem, cause error) {
	actual := string(row.State)
	var bad concern.InvalidTransitionError
	if errors.As(cause, &bad) {
		actual = string(bad.From)
	}
	payload, _ := json.Marshal(map[string]any{
		"concern_id":     row.ID.String(),
		"intended_state": string(concern.StateDeferred),
		"actual_state":   actual,
		"issue_number":   created.Number,
		"issue_url":      created.URL,
		"error":          cause.Error(),
	})
	systemKind := audit.ActorSystem
	if _, aerr := s.cfg.AuditRepo.AppendChained(r.Context(), audit.ChainAppendParams{
		RunID:     row.RunID,
		StageID:   &row.StageID,
		Timestamp: time.Now().UTC(),
		Category:  CategoryConcernDeferFailed,
		ActorKind: &systemKind,
		Payload:   payload,
	}); aerr != nil {
		s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn,
			"defer: append corrective concern_defer_failed entry failed",
			slog.String("run_id", row.RunID.String()),
			slog.String("concern_id", row.ID.String()),
			slog.String("error", aerr.Error()))
	}
}

// deferDefectCategories are the review concern categories that map to a
// `bug` follow-up; everything else defaults to `chore`. Operators can
// override with the request's `type`.
var deferDefectCategories = map[string]struct{}{
	"correctness": {},
	"bug":         {},
	"security":    {},
	"logic":       {},
	"regression":  {},
	"data-loss":   {},
}

// deferTypeForCategory picks the default follow-up type from the
// concern's category: a defect category -> bug, else chore.
func deferTypeForCategory(category string) string {
	if _, ok := deferDefectCategories[strings.ToLower(strings.TrimSpace(category))]; ok {
		return "bug"
	}
	return "chore"
}

// deferSummaryMaxLen bounds the title {summary} derived from the concern
// note so a long note does not blow out the rendered title.
const deferSummaryMaxLen = 100

// deferSummary derives the one-line title summary from the concern note:
// its first line, trimmed and truncated.
func deferSummary(note string) string {
	line := note
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	line = strings.TrimSpace(line)
	if line == "" {
		line = "Deferred review concern"
	}
	if len(line) > deferSummaryMaxLen {
		line = strings.TrimSpace(line[:deferSummaryMaxLen]) + "…"
	}
	return line
}

// deferReasonNote picks the operator note when present, else the concern
// note, for the concern's state_reason.
func deferReasonNote(concernNote, operatorNote string) string {
	if n := strings.TrimSpace(operatorNote); n != "" {
		return n
	}
	return strings.TrimSpace(concernNote)
}

// deferBody assembles the follow-up work item's body verbatim from the
// concern + its run (FilingRequest.Body bypasses the per-type section
// skeleton, so this is type-agnostic across bug/chore). It carries the
// concern note, severity, category, reviewer model, the evidence run id,
// the source PR link, and any operator note — the durable provenance a
// reviewer of the follow-up needs.
func deferBody(row *concern.Concern, rn *run.Run, operatorNote string) string {
	reviewer := "an agent reviewer"
	if row.ReviewerModel != nil && strings.TrimSpace(*row.ReviewerModel) != "" {
		reviewer = *row.ReviewerModel
	}
	prLink := "(no pull request recorded)"
	if rn.PullRequestURL != nil && strings.TrimSpace(*rn.PullRequestURL) != "" {
		prLink = *rn.PullRequestURL
	}

	var b strings.Builder
	b.WriteString("## Summary\n\n")
	fmt.Fprintf(&b, "Deferred from the %s-stage review of %s. This follow-up tracks a review concern the operator accepted as non-blocking but worth a separate change.\n\n", row.StageKind, prLink)

	b.WriteString("## Observed\n\n")
	fmt.Fprintf(&b, "A **%s** severity concern in category **%s**, raised by %s:\n\n", row.Severity, row.Category, reviewer)
	for _, ln := range strings.Split(strings.TrimRight(row.Note, "\n"), "\n") {
		fmt.Fprintf(&b, "> %s\n", ln)
	}
	b.WriteString("\n")

	b.WriteString("## Done-means\n\n")
	b.WriteString("The concern above is resolved or deliberately closed with a recorded rationale.\n\n")

	if n := strings.TrimSpace(operatorNote); n != "" {
		b.WriteString("## Notes\n\n")
		fmt.Fprintf(&b, "%s\n\n", n)
	}

	b.WriteString("## Relations\n\n")
	fmt.Fprintf(&b, "Evidence run: `%s`\n", row.RunID.String())

	return b.String()
}
