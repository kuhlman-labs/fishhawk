package server

// report_agent_changes.go implements the E9.3 canned compliance report —
// GET /v0/reports/agent-changes (machine JSON) and
// GET /v0/reports/agent-changes.md (human-readable markdown), #1606.
//
// The report is a PROJECTION over the E9.1 shared export assembly
// (resolveExportPage + assembleRunData in audit_export.go), following the
// exact E9.2 CSV-projection precedent (audit_export_csv.go): it selects the
// SAME run page and reuses the SAME per-run audit-chain assembly, never a
// parallel query path. One report model (agentChangesReport) feeds both
// renders — the JSON handler and renderAgentChangesMarkdown — so the two are
// guaranteed to agree (the one-model-two-renders parity invariant, asserted
// by the JSON-markdown parity test).
//
// UNLIKE the Export v1 JSON body of GET /v0/audit/export, this endpoint is
// NOT verifier-strict-decoded: no external verifier calls
// json.Decoder.DisallowUnknownFields on it. That is why the continuation /
// completeness markers (complete, next_cursor) appear BOTH in the response
// headers (X-Fishhawk-Export-Complete / X-Fishhawk-Export-Next-Cursor, the
// shared export contract) AND in the report body + the markdown header: a
// partial report declares itself partial in-band (ADR-054 size posture). The
// DisallowUnknownFields constraint binds ONLY the Export v1 body of
// /v0/audit/export (documented in audit_export.go); it never parses this
// report, so the extra body fields are safe here.
//
// Auth-scope enforcement (read:audit-export) is deliberately NOT added here —
// E9.5/#1608 owns it for all export surfaces. This endpoint inherits the same
// fail-closed 503 posture as both export handlers (all three repos required).

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// agentChangesReportSchema is the schema string stamped on the JSON body so
// consumers can version their parse. Bumped when the report shape changes
// incompatibly.
const agentChangesReportSchema = "agent-changes-report/v1"

// workflowIDHumanLed is the workflow key whose runs record human-authored
// changes to load-bearing surfaces (.fishhawk/workflows.yaml). Runs on this
// workflow render in the reduced-evidence human-led section (approval + PR +
// chain span only), never the agent-changes section.
const workflowIDHumanLed = "human_led_change"

// agentChangesReport is the one report model both renders project. The
// continuation fields (Complete/NextCursor) are mirrored from the shared
// export page into the body — safe here because no verifier strict-decodes
// this endpoint (see file header).
type agentChangesReport struct {
	Schema          string              `json:"schema"`
	GeneratedAt     time.Time           `json:"generated_at"`
	Filters         agentChangesFilters `json:"filters"`
	Complete        bool                `json:"complete"`
	NextCursor      string              `json:"next_cursor,omitempty"`
	AgentChanges    []agentChangeItem   `json:"agent_changes"`
	HumanLedChanges []agentChangeItem   `json:"human_led_changes"`
	Totals          agentChangesTotals  `json:"totals"`
}

// agentChangesFilters echoes the run-selection filters back so a saved report
// is self-describing.
type agentChangesFilters struct {
	Repo string `json:"repo,omitempty"`
	From string `json:"from,omitempty"`
	To   string `json:"to,omitempty"`
}

// agentChangesTotals counts every selected run by disposition. runs_in_range
// is the page size; the three category counts partition it (agent + human-led
// + without-change == runs_in_range for a given page).
type agentChangesTotals struct {
	RunsInRange       int `json:"runs_in_range"`
	AgentChanges      int `json:"agent_changes"`
	HumanLedChanges   int `json:"human_led_changes"`
	RunsWithoutChange int `json:"runs_without_change"`
}

// agentChangeItem is one selected run that produced a change. Human-led items
// reuse the same struct with Reviews/Acceptance omitted (reduced evidence).
type agentChangeItem struct {
	RunID         string                   `json:"run_id"`
	Repo          string                   `json:"repo"`
	WorkflowID    string                   `json:"workflow_id"`
	TriggerRef    string                   `json:"trigger_ref,omitempty"`
	PR            agentChangePR            `json:"pr"`
	Merge         *agentChangeMerge        `json:"merge,omitempty"`
	Approvals     []agentChangeApproval    `json:"approvals,omitempty"`
	Reviews       []agentChangeReview      `json:"reviews,omitempty"`
	Acceptance    *agentChangeAcceptance   `json:"acceptance,omitempty"`
	AuditChain    agentChangeAuditChain    `json:"audit_chain"`
	EvidenceLinks agentChangeEvidenceLinks `json:"evidence_links"`
}

// agentChangePR is the what-changed derived from the pull_request_opened
// audit entry.
type agentChangePR struct {
	URL     string `json:"url,omitempty"`
	Number  int64  `json:"number,omitempty"`
	Branch  string `json:"branch,omitempty"`
	HeadSHA string `json:"head_sha,omitempty"`
}

// agentChangeMerge is the merge status derived from the pr_merged audit entry.
// Absent (nil) when the run's PR never merged.
type agentChangeMerge struct {
	Merged   bool      `json:"merged"`
	Merger   string    `json:"merger,omitempty"`
	HeadSHA  string    `json:"head_sha,omitempty"`
	BaseSHA  string    `json:"base_sha,omitempty"`
	MergedAt time.Time `json:"merged_at"`
}

// agentChangeApproval is one approval, from an approval_submitted or a
// pr_approved_on_github entry. Subject/ActorKind are the entry's actor;
// Decision/Surface come from the approval_submitted payload (empty for a
// GitHub PR approval).
type agentChangeApproval struct {
	Subject   string    `json:"subject,omitempty"`
	ActorKind string    `json:"actor_kind,omitempty"`
	Decision  string    `json:"decision,omitempty"`
	Surface   string    `json:"surface,omitempty"`
	Timestamp time.Time `json:"timestamp"`
}

// agentChangeReview is one reviewer verdict, from a plan_reviewed or
// implement_reviewed entry decoded via the exported planreview payload types.
type agentChangeReview struct {
	Stage         string    `json:"stage"` // plan | implement
	ReviewerKind  string    `json:"reviewer_kind,omitempty"`
	ReviewerModel string    `json:"reviewer_model,omitempty"`
	Verdict       string    `json:"verdict,omitempty"`
	Timestamp     time.Time `json:"timestamp"`
}

// agentChangeAcceptance is the validation outcome from the
// acceptance_outcome_recorded entry. Absent (nil) for runs predating the
// acceptance stage (#1555) or that never ran acceptance.
type agentChangeAcceptance struct {
	Verdict        string    `json:"verdict,omitempty"`
	FailureMode    string    `json:"failure_mode,omitempty"`
	EvidenceHashes []string  `json:"evidence_hashes,omitempty"`
	ContentHash    string    `json:"content_hash,omitempty"`
	Timestamp      time.Time `json:"timestamp"`
}

// agentChangeAuditChain is the run's audit-chain span: the first/last
// sequence + entry hashes bounding the run's chain and the total entry count.
type agentChangeAuditChain struct {
	FirstSequence  int64  `json:"first_sequence"`
	LastSequence   int64  `json:"last_sequence"`
	FirstEntryHash string `json:"first_entry_hash"`
	LastEntryHash  string `json:"last_entry_hash"`
	EntryCount     int    `json:"entry_count"`
}

// agentChangeEvidenceLinks are redacted-tier pointers into the run/audit/
// export/artifact API surfaces (ADR-054) — never inlined trace bundles. Each
// is ExternalURL-prefixed when cfg.ExternalURL is configured, else a relative
// /v0/... path.
type agentChangeEvidenceLinks struct {
	Run       string   `json:"run"`
	Audit     string   `json:"audit"`
	Export    string   `json:"export"`
	Artifacts []string `json:"artifacts,omitempty"`
}

// approvalSubmittedPayload is the subset of the approval_submitted audit
// payload the report reads. Extra fields are ignored (a lenient decode — a
// malformed payload is skipped with a warn, never a request failure).
type approvalSubmittedPayload struct {
	Decision string `json:"decision"`
	Surface  string `json:"surface"`
}

// prOpenedPayload is the subset of the pull_request_opened payload the report
// reads.
type prOpenedPayload struct {
	PRURL    string `json:"pr_url"`
	PRNumber int64  `json:"pr_number"`
	Branch   string `json:"branch"`
	HeadSHA  string `json:"head_sha"`
}

// prMergedPayload is the subset of the pr_merged payload the report reads.
type prMergedPayload struct {
	PRURL   string `json:"pr_url"`
	Merger  string `json:"merger"`
	HeadSHA string `json:"head_sha"`
	BaseSHA string `json:"base_sha"`
}

// acceptanceOutcomePayload is the subset of the acceptance_outcome_recorded
// payload the report reads.
type acceptanceOutcomePayload struct {
	Verdict        string   `json:"verdict"`
	FailureMode    string   `json:"failure_mode"`
	EvidenceHashes []string `json:"evidence_hashes"`
	ContentHash    string   `json:"content_hash"`
}

// handleAgentChangesReport implements GET /v0/reports/agent-changes (JSON).
func (s *Server) handleAgentChangesReport(w http.ResponseWriter, r *http.Request) {
	// Fail closed: a compliance report must not silently omit its inputs
	// (identical posture to both export handlers).
	if s.cfg.AuditRepo == nil || s.cfg.RunRepo == nil || s.cfg.SigningRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "audit_export_unconfigured",
			"agent-changes report requires configured audit, run, and signing repositories", nil)
		return
	}

	ep, ok := s.resolveExportPage(w, r)
	if !ok {
		// resolveExportPage already wrote the error response.
		return
	}

	report, aerr := s.buildAgentChangesReport(r.Context(), r, ep)
	if aerr != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"assemble agent-changes report failed", map[string]any{"error": aerr.Error()})
		return
	}

	w.Header().Set("X-Fishhawk-Export-Complete", strconv.FormatBool(ep.complete))
	if !ep.complete {
		w.Header().Set("X-Fishhawk-Export-Next-Cursor", ep.nextCursor)
	}
	s.writeJSON(w, r, http.StatusOK, report)
}

// handleAgentChangesReportMarkdown implements GET
// /v0/reports/agent-changes.md — the same resolve+build path rendered to
// human-readable markdown with a download filename stamped from nowFunc (the
// CSV precedent).
func (s *Server) handleAgentChangesReportMarkdown(w http.ResponseWriter, r *http.Request) {
	if s.cfg.AuditRepo == nil || s.cfg.RunRepo == nil || s.cfg.SigningRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "audit_export_unconfigured",
			"agent-changes report requires configured audit, run, and signing repositories", nil)
		return
	}

	ep, ok := s.resolveExportPage(w, r)
	if !ok {
		return
	}

	report, aerr := s.buildAgentChangesReport(r.Context(), r, ep)
	if aerr != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"assemble agent-changes report failed", map[string]any{"error": aerr.Error()})
		return
	}

	body := renderAgentChangesMarkdown(report)

	filename := "fishhawk-agent-changes-" + s.nowFunc().UTC().Format("20060102T150405Z") + ".md"
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	w.Header().Set("X-Fishhawk-Export-Complete", strconv.FormatBool(ep.complete))
	if !ep.complete {
		w.Header().Set("X-Fishhawk-Export-Next-Cursor", ep.nextCursor)
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// buildAgentChangesReport iterates the resolved run page, reuses
// assembleRunData per run for its full audit chain, and folds each chain into
// one report item. It never re-queries the run set — that is resolveExportPage's
// single code path. A run with no pull_request_opened entry is counted in
// totals.runs_without_change and omitted from both lists.
func (s *Server) buildAgentChangesReport(ctx context.Context, r *http.Request, ep *exportPage) (agentChangesReport, error) {
	q := r.URL.Query()
	report := agentChangesReport{
		Schema:      agentChangesReportSchema,
		GeneratedAt: s.nowFunc().UTC(),
		Filters: agentChangesFilters{
			Repo: q.Get("repo"),
			From: q.Get("from"),
			To:   q.Get("to"),
		},
		Complete:        ep.complete,
		NextCursor:      ep.nextCursor,
		AgentChanges:    []agentChangeItem{},
		HumanLedChanges: []agentChangeItem{},
	}
	report.Totals.RunsInRange = len(ep.page)

	for _, rn := range ep.page {
		rd, err := s.assembleRunData(ctx, rn.ID)
		if err != nil {
			return agentChangesReport{}, err
		}
		item, produced := s.foldRunIntoItem(ctx, rn, rd.AuditEntries)
		if !produced {
			report.Totals.RunsWithoutChange++
			continue
		}
		if rn.WorkflowID == workflowIDHumanLed {
			// Reduced evidence: reviews + acceptance are agent-run concepts.
			item.Reviews = nil
			item.Acceptance = nil
			report.HumanLedChanges = append(report.HumanLedChanges, item)
			report.Totals.HumanLedChanges++
			continue
		}
		report.AgentChanges = append(report.AgentChanges, item)
		report.Totals.AgentChanges++
	}
	return report, nil
}

// foldRunIntoItem walks a run's audit chain once, keyed on category, folding
// each entry into the item. The bool result is false when the run produced no
// change (no pull_request_opened entry) — such runs are counted, not listed.
// Malformed payloads are skipped with a slog warn, never a request failure
// (the resolveImplementConcerns lenient posture).
func (s *Server) foldRunIntoItem(ctx context.Context, rn *run.Run, entries []exportEntry) (agentChangeItem, bool) {
	item := agentChangeItem{
		RunID:      rn.ID.String(),
		Repo:       rn.Repo,
		WorkflowID: rn.WorkflowID,
	}
	if rn.TriggerRef != nil {
		item.TriggerRef = *rn.TriggerRef
	}

	var producedPR bool
	var chainStarted bool
	// Distinct artifact-producing stages seen in the chain, in first-seen
	// order (deterministic evidence-link ordering).
	var artifactStages []uuid.UUID
	seenStage := map[uuid.UUID]bool{}
	noteStage := func(e *exportEntry) {
		if e.StageID == nil || seenStage[*e.StageID] {
			return
		}
		seenStage[*e.StageID] = true
		artifactStages = append(artifactStages, *e.StageID)
	}

	for i := range entries {
		e := &entries[i]

		// Audit-chain span: first/last sequence + entry hashes + count.
		item.AuditChain.EntryCount++
		if !chainStarted {
			item.AuditChain.FirstSequence = e.Sequence
			item.AuditChain.FirstEntryHash = e.EntryHash
			chainStarted = true
		}
		item.AuditChain.LastSequence = e.Sequence
		item.AuditChain.LastEntryHash = e.EntryHash

		switch e.Category {
		case "pull_request_opened":
			var p prOpenedPayload
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				s.warnMalformed(ctx, rn.ID, e, err)
				continue
			}
			producedPR = true
			item.PR = agentChangePR{
				URL:     p.PRURL,
				Number:  p.PRNumber,
				Branch:  p.Branch,
				HeadSHA: p.HeadSHA,
			}
			noteStage(e)
		case CategoryPRMerged:
			var p prMergedPayload
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				s.warnMalformed(ctx, rn.ID, e, err)
				continue
			}
			item.Merge = &agentChangeMerge{
				Merged:   true,
				Merger:   p.Merger,
				HeadSHA:  p.HeadSHA,
				BaseSHA:  p.BaseSHA,
				MergedAt: e.Timestamp.UTC(),
			}
		case "approval_submitted":
			var p approvalSubmittedPayload
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				s.warnMalformed(ctx, rn.ID, e, err)
				continue
			}
			item.Approvals = append(item.Approvals, agentChangeApproval{
				Subject:   deref(e.ActorSubject),
				ActorKind: deref(e.ActorKind),
				Decision:  p.Decision,
				Surface:   p.Surface,
				Timestamp: e.Timestamp.UTC(),
			})
		case CategoryPRApprovedOnGitHub:
			item.Approvals = append(item.Approvals, agentChangeApproval{
				Subject:   deref(e.ActorSubject),
				ActorKind: deref(e.ActorKind),
				Timestamp: e.Timestamp.UTC(),
			})
		case "plan_reviewed":
			var p planreview.PlanReviewedPayload
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				s.warnMalformed(ctx, rn.ID, e, err)
				continue
			}
			item.Reviews = append(item.Reviews, agentChangeReview{
				Stage:         "plan",
				ReviewerKind:  p.ReviewerKind,
				ReviewerModel: p.ReviewerModel,
				Verdict:       string(p.Verdict),
				Timestamp:     e.Timestamp.UTC(),
			})
		case "implement_reviewed":
			var p planreview.ImplementReviewedPayload
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				s.warnMalformed(ctx, rn.ID, e, err)
				continue
			}
			item.Reviews = append(item.Reviews, agentChangeReview{
				Stage:         "implement",
				ReviewerKind:  p.ReviewerKind,
				ReviewerModel: p.ReviewerModel,
				Verdict:       string(p.Verdict),
				Timestamp:     e.Timestamp.UTC(),
			})
		case CategoryAcceptanceOutcomeRecorded:
			var p acceptanceOutcomePayload
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				s.warnMalformed(ctx, rn.ID, e, err)
				continue
			}
			item.Acceptance = &agentChangeAcceptance{
				Verdict:        p.Verdict,
				FailureMode:    p.FailureMode,
				EvidenceHashes: p.EvidenceHashes,
				ContentHash:    p.ContentHash,
				Timestamp:      e.Timestamp.UTC(),
			}
			noteStage(e)
		}
	}

	if !producedPR {
		return agentChangeItem{}, false
	}
	item.EvidenceLinks = s.buildEvidenceLinks(rn.ID, artifactStages)
	return item, true
}

// warnMalformed logs a skipped malformed audit payload without failing the
// request. A single corrupt payload must not black-hole a whole compliance
// report.
func (s *Server) warnMalformed(ctx context.Context, runID uuid.UUID, e *exportEntry, err error) {
	if s.cfg.Logger == nil {
		return
	}
	s.cfg.Logger.WarnContext(ctx, "agent-changes report: skipping malformed audit payload",
		"run_id", runID.String(),
		"category", e.Category,
		"sequence", e.Sequence,
		"error", err.Error())
}

// buildEvidenceLinks assembles the redacted-tier pointer set for a run. Each
// pointer is ExternalURL-prefixed when cfg.ExternalURL is set, else relative.
func (s *Server) buildEvidenceLinks(runID uuid.UUID, artifactStages []uuid.UUID) agentChangeEvidenceLinks {
	base := strings.TrimRight(s.cfg.ExternalURL, "/")
	id := runID.String()
	links := agentChangeEvidenceLinks{
		Run:    base + "/v0/runs/" + id,
		Audit:  base + "/v0/runs/" + id + "/audit",
		Export: base + "/v0/audit/export?run_id=" + id,
	}
	for _, st := range artifactStages {
		links.Artifacts = append(links.Artifacts, base+"/v0/stages/"+st.String()+"/artifacts")
	}
	return links
}

// renderAgentChangesMarkdown renders the report model to human-readable
// markdown. It is a PURE function over the model (no server state) so the
// golden test needs no HTTP fixture. Ordering is deterministic: the page's
// created_at DESC order (map-free).
func renderAgentChangesMarkdown(report agentChangesReport) []byte {
	var b strings.Builder
	b.WriteString("# Agent changes report\n\n")
	b.WriteString("Generated at: " + report.GeneratedAt.UTC().Format(time.RFC3339) + "\n\n")

	// Filter echo.
	b.WriteString("Filters: ")
	b.WriteString("repo=" + orDash(report.Filters.Repo))
	b.WriteString(", from=" + orDash(report.Filters.From))
	b.WriteString(", to=" + orDash(report.Filters.To))
	b.WriteString("\n\n")

	// Partiality banner: a partial report declares itself partial in-band.
	if !report.Complete {
		b.WriteString("**PARTIAL REPORT — resume with cursor " + report.NextCursor + "**\n\n")
	}

	// Totals line.
	b.WriteString("Totals: ")
	b.WriteString(strconv.Itoa(report.Totals.RunsInRange) + " runs in range, ")
	b.WriteString(strconv.Itoa(report.Totals.AgentChanges) + " agent changes, ")
	b.WriteString(strconv.Itoa(report.Totals.HumanLedChanges) + " human-led changes, ")
	b.WriteString(strconv.Itoa(report.Totals.RunsWithoutChange) + " runs without change\n\n")

	b.WriteString("## Agent-authored changes\n\n")
	if len(report.AgentChanges) == 0 {
		b.WriteString("_None._\n\n")
	}
	for _, item := range report.AgentChanges {
		renderChangeItem(&b, item, false)
	}

	b.WriteString("## Human-led changes (reduced evidence)\n\n")
	if len(report.HumanLedChanges) == 0 {
		b.WriteString("_None._\n\n")
	}
	for _, item := range report.HumanLedChanges {
		renderChangeItem(&b, item, true)
	}

	return []byte(b.String())
}

// renderChangeItem renders one change subsection. When humanLed is true the
// reviews/acceptance blocks are omitted (they are always nil for human-led
// items, but the flag documents the reduced-evidence contract at the render).
func renderChangeItem(b *strings.Builder, item agentChangeItem, humanLed bool) {
	b.WriteString("### " + item.RunID + "\n\n")
	b.WriteString("- Repo: " + orDash(item.Repo) + "\n")
	b.WriteString("- Workflow: " + orDash(item.WorkflowID) + "\n")
	if item.TriggerRef != "" {
		b.WriteString("- Trigger: " + item.TriggerRef + "\n")
	}

	// PR line.
	b.WriteString("- PR: " + orDash(item.PR.URL))
	if item.PR.Number != 0 {
		b.WriteString(" (#" + strconv.FormatInt(item.PR.Number, 10) + ")")
	}
	if item.PR.Branch != "" {
		b.WriteString(" branch=" + item.PR.Branch)
	}
	if item.PR.HeadSHA != "" {
		b.WriteString(" head=" + item.PR.HeadSHA)
	}
	b.WriteString("\n")

	// Merge status.
	if item.Merge != nil {
		b.WriteString("- Merged: yes by " + orDash(item.Merge.Merger) +
			" at " + item.Merge.MergedAt.UTC().Format(time.RFC3339))
		if item.Merge.BaseSHA != "" {
			b.WriteString(" base=" + item.Merge.BaseSHA)
		}
		b.WriteString("\n")
	} else {
		b.WriteString("- Merged: no\n")
	}

	// Approvals.
	if len(item.Approvals) > 0 {
		b.WriteString("- Approvals:\n")
		for _, a := range item.Approvals {
			line := "  - " + orDash(a.Subject)
			if a.Decision != "" {
				line += " decision=" + a.Decision
			}
			if a.Surface != "" {
				line += " surface=" + a.Surface
			}
			line += " at " + a.Timestamp.UTC().Format(time.RFC3339)
			b.WriteString(line + "\n")
		}
	}

	// Reviews (agent-only).
	if !humanLed && len(item.Reviews) > 0 {
		b.WriteString("- Reviews:\n")
		for _, rv := range item.Reviews {
			line := "  - " + rv.Stage + " " + orDash(rv.ReviewerModel) +
				" (" + orDash(rv.ReviewerKind) + ") verdict=" + orDash(rv.Verdict)
			line += " at " + rv.Timestamp.UTC().Format(time.RFC3339)
			b.WriteString(line + "\n")
		}
	}

	// Acceptance (agent-only).
	if !humanLed && item.Acceptance != nil {
		line := "- Acceptance: verdict=" + orDash(item.Acceptance.Verdict)
		if item.Acceptance.FailureMode != "" {
			line += " failure_mode=" + item.Acceptance.FailureMode
		}
		if len(item.Acceptance.EvidenceHashes) > 0 {
			line += " evidence=" + strings.Join(item.Acceptance.EvidenceHashes, ",")
		}
		if item.Acceptance.ContentHash != "" {
			line += " content_hash=" + item.Acceptance.ContentHash
		}
		b.WriteString(line + "\n")
	}

	// Audit-chain span.
	b.WriteString("- Audit chain: sequences " +
		strconv.FormatInt(item.AuditChain.FirstSequence, 10) + "–" +
		strconv.FormatInt(item.AuditChain.LastSequence, 10) + " (" +
		strconv.Itoa(item.AuditChain.EntryCount) + " entries), first=" +
		orDash(item.AuditChain.FirstEntryHash) + " last=" +
		orDash(item.AuditChain.LastEntryHash) + "\n")

	// Evidence links.
	b.WriteString("- Evidence:\n")
	b.WriteString("  - run: " + item.EvidenceLinks.Run + "\n")
	b.WriteString("  - audit: " + item.EvidenceLinks.Audit + "\n")
	b.WriteString("  - export: " + item.EvidenceLinks.Export + "\n")
	for _, art := range item.EvidenceLinks.Artifacts {
		b.WriteString("  - artifacts: " + art + "\n")
	}

	b.WriteString("\n")
}

// orDash renders an empty string as an em-dash so a blank field is visible
// (never silently absent) in the markdown.
func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}
