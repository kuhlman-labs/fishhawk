package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// divergenceAnswerRequest is the JSON body of
// POST /v0/runs/{run_id}/divergence/{sequence}/answer.
type divergenceAnswerRequest struct {
	// Answer is one of one_off | doctrine_change.
	Answer string `json:"answer"`
	// Note is an optional captain addendum recorded on the answer entry and,
	// for doctrine_change, folded into the filed work item's body.
	Note string `json:"note,omitempty"`
	// ParentEpic, N and Labels place the doctrine_change work item exactly as
	// the defer verb's fields do; ignored for one_off.
	ParentEpic string   `json:"parent_epic,omitempty"`
	N          string   `json:"n,omitempty"`
	Labels     []string `json:"labels,omitempty"`
}

// divergenceAnswerResponse is the 200 body.
type divergenceAnswerResponse struct {
	RunID    string `json:"run_id"`
	Sequence int64  `json:"sequence"`
	Answer   string `json:"answer"`
	// Issue is the filed doctrine-change work item; nil for one_off.
	Issue *deferFiledIssue `json:"issue,omitempty"`
}

// divergenceDoctrineType is the work-item type a doctrine_change files.
const divergenceDoctrineType = "chore"

// divergenceDoctrineAutonomy is the autonomy label a doctrine_change item
// always carries: a standing-order change is human-authored (.fishhawk/** is
// never agent-written), so the item is structurally not agent-drivable.
const divergenceDoctrineAutonomy = "autonomy:low"

// handleAnswerDivergence implements
// POST /v0/runs/{run_id}/divergence/{sequence}/answer (E75.5 / #3733): the
// captain's answer to one divergence question.
//
//   - one_off records precedent_divergence_answered and nothing else; the
//     question stops surfacing.
//   - doctrine_change files ONE autonomy:low work item through the shared
//     applyAndFileWorkItem chokepoint, proposing the standing-order change and
//     citing the divergence and its precedent by sequence + entry hash, then
//     records the answer naming the filed issue. FILE FIRST, THEN RECORD (the
//     handleDeferConcern orphan-safety ordering): a filing failure records
//     nothing, so the question stays open and the captain can retry.
//
// It writes NO repository file — the handler holds no content-write seam.
//
// Auth mirrors the bulk-waive verb: authenticated, write:stages or
// write:fixups, and a run-bound mcp:run:<uuid> token may answer only on its
// own run. The cited sequence is re-read from THIS run's precedent_divergence
// entries, so a stale or fabricated sequence is a named 404.
func (s *Server) handleAnswerDivergence(w http.ResponseWriter, r *http.Request) {
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
	if s.cfg.AuditRepo == nil || s.cfg.RunRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "divergence_store_unconfigured",
			"divergence answer endpoint requires audit + run repositories", nil)
		return
	}

	runID, err := uuid.Parse(r.PathValue("run_id"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"run_id must be a valid UUID",
			map[string]any{"field": "run_id", "got": r.PathValue("run_id")})
		return
	}
	seq, err := strconv.ParseInt(r.PathValue("sequence"), 10, 64)
	if err != nil || seq <= 0 {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"sequence must be a positive integer (the precedent_divergence entry's chain sequence)",
			map[string]any{"field": "sequence", "got": r.PathValue("sequence")})
		return
	}

	// Subject-binding guard: a run-bound token may answer only on its own run.
	if strings.HasPrefix(id.Subject, "mcp:run:") {
		subjectRunID, parseErr := uuid.Parse(strings.TrimPrefix(id.Subject, "mcp:run:"))
		if parseErr != nil {
			s.writeError(w, r, http.StatusUnauthorized, "authentication_required",
				"mcp token subject is malformed", nil)
			return
		}
		if subjectRunID != runID {
			s.writeError(w, r, http.StatusForbidden, "cross_run_divergence_answer",
				"mcp token may only answer divergence questions within its own run",
				map[string]any{"token_run_id": subjectRunID.String(), "path_run_id": runID.String()})
			return
		}
	}

	var req divergenceAnswerRequest
	if r.Body != nil {
		if decErr := json.NewDecoder(r.Body).Decode(&req); decErr != nil {
			s.writeError(w, r, http.StatusBadRequest, "validation_failed",
				"request body must be valid JSON {answer, note}",
				map[string]any{"error": decErr.Error()})
			return
		}
	}
	answer := strings.TrimSpace(req.Answer)
	if answer != divergenceAnswerOneOff && answer != divergenceAnswerDoctrineChange {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"answer must be one of: one_off, doctrine_change",
			map[string]any{"field": "answer", "got": req.Answer})
		return
	}

	ru, err := s.cfg.RunRepo.GetRun(r.Context(), runID)
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

	entries, err := s.cfg.AuditRepo.ListForRunByCategory(r.Context(), runID, CategoryPrecedentDivergence)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"list precedent_divergence entries failed", map[string]any{"error": err.Error()})
		return
	}
	var (
		target    *audit.Entry
		divergent precedentDivergencePayload
	)
	for _, e := range entries {
		if e != nil && e.Sequence == seq && json.Unmarshal(e.Payload, &divergent) == nil {
			target = e
			break
		}
	}
	if target == nil {
		s.writeError(w, r, http.StatusNotFound, "divergence_not_found",
			"no precedent_divergence entry with that sequence on this run",
			map[string]any{"run_id": runID.String(), "sequence": seq})
		return
	}
	answered, err := s.answeredDivergenceSequences(r.Context(), ru)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"list precedent_divergence_answered entries failed", map[string]any{"error": err.Error()})
		return
	}
	if answered[seq] {
		s.writeError(w, r, http.StatusConflict, "divergence_already_answered",
			"this divergence question already has an answer",
			map[string]any{"run_id": runID.String(), "sequence": seq})
		return
	}

	p := precedentDivergenceAnsweredPayload{
		DivergenceSequence: seq,
		Answer:             answer,
		Note:               strings.TrimSpace(req.Note),
		DecisionClass:      divergent.DecisionClass,
		StageID:            divergent.StageID,
	}
	resp := divergenceAnswerResponse{RunID: runID.String(), Sequence: seq, Answer: answer}

	if answer == divergenceAnswerDoctrineChange {
		issue, ok := s.fileDoctrineChange(w, r, ru, target, divergent, req)
		if !ok {
			return
		}
		p.IssueNumber, p.IssueURL, p.IssueProvider = issue.Number, issue.URL, issue.Provider
		resp.Issue = issue
	}

	payload, err := json.Marshal(p)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"marshal precedent_divergence_answered payload failed", map[string]any{"error": err.Error()})
		return
	}
	subject := id.Subject
	if subject == "" {
		subject = "anonymous"
	}
	actorKind := actorKindForSubject(subject)
	if _, err := s.cfg.AuditRepo.AppendChained(r.Context(), audit.ChainAppendParams{
		RunID:        runID,
		StageID:      target.StageID,
		Timestamp:    time.Now().UTC(),
		Category:     CategoryPrecedentDivergenceAnswered,
		ActorKind:    &actorKind,
		ActorSubject: &subject,
		Payload:      payload,
	}); err != nil {
		details := map[string]any{"error": err.Error()}
		if resp.Issue != nil {
			// The work item is durable; name it so the captain does not
			// re-answer doctrine_change and file a duplicate.
			details["filed_issue"] = resp.Issue.URL
		}
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"recording the divergence answer failed", details)
		return
	}
	s.writeJSON(w, r, http.StatusOK, resp)
}

// fileDoctrineChange files the doctrine_change work item through
// applyAndFileWorkItem. On any failure it writes the error response and
// returns ok=false, having recorded nothing.
func (s *Server) fileDoctrineChange(w http.ResponseWriter, r *http.Request, ru *run.Run, entry *audit.Entry, d precedentDivergencePayload, req divergenceAnswerRequest) (*deferFiledIssue, bool) {
	owner, name, ok := splitRepoFullName(ru.Repo)
	if !ok {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"the run has a malformed repo coordinate", map[string]any{"repo": ru.Repo})
		return nil, false
	}
	conv, err := conventionsLoader(r.Context(), ru.Repo)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"could not load work-management conventions", map[string]any{"error": err.Error()})
		return nil, false
	}
	filing := workmgmt.FilingRequest{
		Type:    divergenceDoctrineType,
		Summary: doctrineChangeSummary(d),
		Body:    doctrineChangeBody(ru, entry, d, req.Note),
		Labels:  doctrineChangeLabels(req.Labels),
		Relations: workmgmt.Relations{
			ParentEpic:   req.ParentEpic,
			EvidenceRuns: []string{ru.ID.String()},
		},
	}
	if n := strings.TrimSpace(req.N); n != "" {
		filing.TitleVars = map[string]string{"n": n}
	}
	target := workmgmt.Target{
		Repo:    workmgmt.Repo{Owner: owner, Name: name},
		Project: conv.Project,
		Jira:    conv.Jira,
	}
	if ru.InstallationID != nil {
		target.Scope = forge.FromGitHubInstallationID(*ru.InstallationID)
	}
	if target.Scope.IsZero() && s.cfg.GitHub != nil {
		scope, rerr := s.resolveRepoScope(r.Context(), owner, name)
		if rerr != nil {
			s.writeError(w, r, http.StatusBadGateway, "work_item_filing_failed",
				"could not resolve the GitHub App installation for the run's repo",
				map[string]any{"error": rerr.Error()})
			return nil, false
		}
		target.Scope = scope
	}
	item, created, werr := s.applyAndFileWorkItem(r.Context(), filing, conv, target, owner, name)
	if werr != nil {
		s.writeError(w, r, werr.status, werr.code, werr.msg, werr.details)
		return nil, false
	}
	return &deferFiledIssue{
		Type:                   item.Type,
		Title:                  item.Title,
		Number:                 created.Number,
		URL:                    created.URL,
		Provider:               created.Provider,
		AppliedLabels:          created.AppliedLabels,
		DefaultedLabels:        item.Classification.DefaultedLabels,
		MissingLabelNamespaces: item.Classification.MissingLabelNamespaces,
	}, true
}

// doctrineChangeLabels returns the caller's labels with every autonomy:*
// label replaced by autonomy:low — a doctrine change is never agent-drivable.
func doctrineChangeLabels(in []string) []string {
	out := make([]string, 0, len(in)+1)
	for _, l := range in {
		if strings.HasPrefix(strings.TrimSpace(l), "autonomy:") {
			continue
		}
		out = append(out, l)
	}
	return append(out, divergenceDoctrineAutonomy)
}

// doctrineChangeSummary is the work item's one-line title summary.
func doctrineChangeSummary(d precedentDivergencePayload) string {
	return fmt.Sprintf("Doctrine change: %s decided %q against a %q precedent", d.DecisionClass, d.Outcome, d.ModalOutcome)
}

// doctrineChangeBody drafts the work item: the proposed standing-order change
// and the divergence with its cited precedent, by sequence and entry hash. It
// carries NO prior decision's reason prose (ADR-082 rule 1).
func doctrineChangeBody(ru *run.Run, entry *audit.Entry, d precedentDivergencePayload, note string) string {
	var b strings.Builder
	b.WriteString("## Summary\n\n")
	fmt.Fprintf(&b, "The captain answered a divergence question with **doctrine_change**: a `%s` decision on run `%s` went against clear precedent, and the captain says the precedent — not the decision — should change. This item proposes the charter or workflow-spec (`.fishhawk/**`) change that records the new standing order. It is human-authored: no repository file was written.\n\n", d.DecisionClass, ru.ID)
	b.WriteString("## Observed\n\n")
	fmt.Fprintf(&b, "- Decision: `%s` -> `%s`", d.DecisionClass, d.Outcome)
	if d.RejectClass != "" {
		fmt.Fprintf(&b, " (reject_class `%s`)", d.RejectClass)
	}
	fmt.Fprintf(&b, " on stage `%s` (%s)\n", d.StageID, d.StageKind)
	fmt.Fprintf(&b, "- Precedent: modal outcome `%s`, agreement %.2f over %d human decisions\n", d.ModalOutcome, d.AgreementRatio, d.HumanCount)
	fmt.Fprintf(&b, "- Rule: min_decisions %d, min_agreement %.2f, window %ds, doctrine_version `%s`\n", d.Threshold.MinDecisions, d.Threshold.MinAgreement, d.Threshold.WindowSeconds, d.Threshold.DoctrineVersion)
	fmt.Fprintf(&b, "- Divergence entry: sequence %d, entry hash `%s`\n\n", entry.Sequence, entry.EntryHash)
	b.WriteString("Cited precedent (source sequence, entry hash):\n\n")
	for _, c := range d.Cited {
		fmt.Fprintf(&b, "- %d `%s` (%s)\n", c.SourceSequence, c.SourceEntryHash, c.Outcome)
	}
	if d.CitedTotal > len(d.Cited) {
		fmt.Fprintf(&b, "- … %d more\n", d.CitedTotal-len(d.Cited))
	}
	b.WriteString("\n## Done-means\n\n")
	b.WriteString("The standing order for this decision class is updated in the charter or workflow spec, or this item is closed with a recorded rationale for keeping the precedent.\n\n")
	if n := strings.TrimSpace(note); n != "" {
		b.WriteString("## Notes\n\n")
		fmt.Fprintf(&b, "%s\n\n", n)
	}
	b.WriteString("## Relations\n\n")
	fmt.Fprintf(&b, "Evidence run: `%s`\n", ru.ID)
	return b.String()
}
