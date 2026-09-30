package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/crewmessage"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
	workmgmtgithub "github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt/github"
)

// Crew work requests (E77.7 / #3741, ADR-081 #3727 rule 2): a `work_request`
// send files EXACTLY ONE work item through the shared applyAndFileWorkItem
// core — so the title format, type:* defaults, derived area:*/phase:* labels,
// Status=Backlog, boarding and the epic link are the conventions' own — and
// NEVER creates a run row or reaches a dispatch path. Filing is a FOLLOW-ON to
// the send: the crew_message_sent chain entry is the record and has already
// committed, so a filing failure degrades the send response
// (work_item_filing_error) and leaves the stored message intact. That is the
// inverse of defer-concern's file-first ordering, which protects a side effect
// that cannot be un-created; here the side effect is the optional one.

// CategoryCrewWorkRequestFiled records that a crew work_request filed its work
// item: the sent sequence, the filed number and url. Appended ONLY after the
// provider returned the created item — a fact, never an attempt.
const CategoryCrewWorkRequestFiled = "crew_work_request_filed"

// crewWorkRequestType is the work-item type a crew work request files as. The
// message contract carries no type member (rule 2: nothing in a crew message
// may express scope or dispatch), so the placement is fixed here.
const crewWorkRequestType = "chore"

// crewWorkItemFilingError is the send response's work_item_filing_error: the
// named code and message of the filing branch that failed. The crew message
// itself was recorded; only its follow-on filing did not land.
type crewWorkItemFilingError struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

func crewFilingError(code, msg string, details map[string]any) *crewWorkItemFilingError {
	return &crewWorkItemFilingError{Code: code, Message: msg, Details: details}
}

// fileCrewWorkRequest files the work item a freshly SENT work_request asks for.
// It is called by handleSendCrewMessage after Mailbox.Send returned a row, and
// only for msg.Type == TypeWorkRequest.
//
// Target resolution:
//
//   - a RUN-anchored message files against the anchor run's repository, the
//     run supplying Target.Scope (its installation) exactly as
//     handleDeferConcern resolves the concern's own run;
//   - a RUN-LESS message has no run to scope the filing. A run-bound token is
//     refused run_scoped_filing_required with NOTHING filed — the same
//     provider-independent posture handleFileWorkItem applies: the run-absent
//     path is operator-only. (The send path already refuses a run-bound token
//     a run-less anchor, so this is the defence-in-depth rung.) An operator's
//     issue_ref anchor ("owner/name#N") names the repository; a
//     decision_record_id anchor names none and cannot be filed.
func (s *Server) fileCrewWorkRequest(ctx context.Context, id Identity, msg *crewmessage.Message, row *crewmessage.Row) (*deferFiledIssue, *crewWorkItemFilingError) {
	var anchorRun *run.Run
	if row.RunID != nil && s.cfg.RunRepo != nil {
		rn, err := s.cfg.RunRepo.GetRun(ctx, *row.RunID)
		if err != nil {
			return nil, crewFilingError("internal_error", "could not resolve the work request's anchor run",
				map[string]any{"error": err.Error(), "run_id": row.RunID.String()})
		}
		anchorRun = rn
	}
	repo := ""
	if anchorRun != nil {
		repo = anchorRun.Repo
	} else {
		if strings.HasPrefix(id.Subject, "mcp:run:") {
			return nil, crewFilingError("run_scoped_filing_required",
				"a run-bound agent token may file a work request only through its own run; the run-absent filing path is operator-only", nil)
		}
		repo = crewIssueRefRepo(row.IssueRef)
	}
	owner, name, ok := splitRepoFullName(repo)
	if !ok {
		return nil, crewFilingError("crew_work_request_no_repository",
			"the work request's anchor names no repository to file in; anchor it on a run or on an owner/name#N issue_ref", nil)
	}

	conv, err := conventionsLoader(ctx, owner+"/"+name)
	if err != nil {
		return nil, crewFilingError("internal_error", "could not load work-management conventions",
			map[string]any{"error": err.Error()})
	}
	filing := workmgmt.FilingRequest{
		Type:    crewWorkRequestType,
		Summary: crewWorkRequestSummary(msg),
		Body:    crewWorkRequestBody(msg, row),
		Relations: workmgmt.Relations{
			ParentEpic: crewWorkRequestParentEpic(msg),
		},
	}
	if anchorRun != nil {
		filing.Relations.EvidenceRuns = []string{anchorRun.ID.String()}
	}
	target := workmgmt.Target{
		Repo:    workmgmt.Repo{Owner: owner, Name: name},
		Project: conv.Project,
		Jira:    conv.Jira,
		GitLab:  conv.GitLab,
	}
	if anchorRun != nil && anchorRun.InstallationID != nil {
		target.Scope = forge.FromGitHubInstallationID(*anchorRun.InstallationID)
	}
	if target.Scope.IsZero() && s.cfg.GitHub != nil && conv.Provider == workmgmtgithub.ProviderName {
		scope, rerr := s.resolveRepoScope(ctx, owner, name)
		if rerr != nil {
			return nil, crewFilingError("work_item_filing_failed",
				"could not resolve the GitHub App installation for the work request's repo",
				map[string]any{"error": rerr.Error()})
		}
		target.Scope = scope
	}

	item, created, werr := s.applyAndFileWorkItem(ctx, filing, conv, target, owner, name)
	if werr != nil {
		return nil, crewFilingError(werr.code, werr.msg, werr.details)
	}
	s.recordCrewWorkRequestFiled(ctx, id, row, item, created)
	return &deferFiledIssue{
		Type:                   item.Type,
		Title:                  item.Title,
		Number:                 created.Number,
		URL:                    created.URL,
		Provider:               created.Provider,
		AppliedLabels:          created.AppliedLabels,
		DefaultedLabels:        item.Classification.DefaultedLabels,
		MissingLabelNamespaces: item.Classification.MissingLabelNamespaces,
	}, nil
}

// recordCrewWorkRequestFiled appends the crew_work_request_filed fact on the
// message's own chain: the run chain for a run anchor, the account's run-less
// chain otherwise. Warn-only: the item is filed and the response says so, so a
// transient append failure must not turn a landed filing into an error.
func (s *Server) recordCrewWorkRequestFiled(ctx context.Context, id Identity, row *crewmessage.Row, item *workmgmt.WorkItem, created *workmgmt.CreatedItem) {
	if s.cfg.AuditRepo == nil {
		return
	}
	payload, err := json.Marshal(map[string]any{
		"sent_sequence":  row.SentSequence,
		"sender_role":    string(row.SenderRole),
		"issue_number":   created.Number,
		"issue_url":      created.URL,
		"issue_type":     item.Type,
		"issue_title":    item.Title,
		"issue_provider": created.Provider,
	})
	if err != nil {
		return
	}
	subject := id.Subject
	kind := actorKindForSubject(subject)
	ts := time.Now().UTC()
	if row.RunID != nil {
		_, err = s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
			RunID: *row.RunID, Timestamp: ts, Category: CategoryCrewWorkRequestFiled,
			ActorKind: &kind, ActorSubject: &subject, Payload: payload,
		})
	} else {
		_, err = s.cfg.AuditRepo.AppendGlobalChained(ctx, audit.GlobalChainAppendParams{
			Timestamp: ts, Category: CategoryCrewWorkRequestFiled,
			ActorKind: &kind, ActorSubject: &subject, Payload: payload, AccountID: row.AccountID,
		})
	}
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "crew: append crew_work_request_filed failed",
			slog.Int64("sent_sequence", row.SentSequence),
			slog.String("issue_url", created.URL),
			slog.String("error", err.Error()))
	}
}

// crewIssueRefRepo extracts "owner/name" from an "owner/name#N" issue_ref, or
// "" when the ref carries no repository.
func crewIssueRefRepo(ref string) string {
	repo, _, found := strings.Cut(strings.TrimSpace(ref), "#")
	if !found || !strings.Contains(repo, "/") {
		return ""
	}
	return repo
}

// crewWorkRequestParentEpic takes the epic placement from the FIRST `issue`
// evidence reference, the only member of the closed evidence vocabulary that
// can name one. Placement is not scope: it chooses where the item is filed and
// which epic's area/phase it inherits, and nothing it names reaches a run. A
// bare number is normalised to "#N"; absent, the conventions decide (a child
// type whose title_format needs {epic} is then refused by Apply, surfaced as
// work_item_filing_error).
func crewWorkRequestParentEpic(msg *crewmessage.Message) string {
	for _, e := range msg.Evidence {
		if e.Kind != crewmessage.EvidenceIssue {
			continue
		}
		ref := strings.TrimSpace(e.Ref)
		if _, err := strconv.Atoi(ref); err == nil {
			return "#" + ref
		}
		return ref
	}
	return ""
}

// crewWorkRequestSummary derives the one-line title summary from the
// request's title (required by the schema), falling back to its summary.
func crewWorkRequestSummary(msg *crewmessage.Message) string {
	line := strings.TrimSpace(msg.Payload.Title)
	if line == "" {
		line = strings.TrimSpace(msg.Payload.Summary)
	}
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = strings.TrimSpace(line[:i])
	}
	if line == "" {
		line = "Crew work request"
	}
	if len(line) > deferSummaryMaxLen {
		line = strings.TrimSpace(line[:deferSummaryMaxLen]) + "…"
	}
	return line
}

// crewWorkRequestBody assembles the filed item's body (FilingRequest.Body
// bypasses the per-type skeleton): the request's summary and rationale, the
// message it came from, its evidence references and the originating run.
func crewWorkRequestBody(msg *crewmessage.Message, row *crewmessage.Row) string {
	var b strings.Builder
	b.WriteString("## Summary\n\n")
	fmt.Fprintf(&b, "Filed from a crew `work_request` sent by the %s to the %s.\n\n", msg.SenderRole, msg.RecipientRole)
	if t := strings.TrimSpace(msg.Payload.Title); t != "" {
		fmt.Fprintf(&b, "**%s**\n\n", t)
	}
	for _, ln := range strings.Split(strings.TrimRight(msg.Payload.Summary, "\n"), "\n") {
		fmt.Fprintf(&b, "> %s\n", ln)
	}
	b.WriteString("\n")
	if r := strings.TrimSpace(msg.Payload.Rationale); r != "" {
		b.WriteString("## Rationale\n\n")
		for _, ln := range strings.Split(r, "\n") {
			fmt.Fprintf(&b, "> %s\n", ln)
		}
		b.WriteString("\n")
	}
	b.WriteString("## Relations\n\n")
	fmt.Fprintf(&b, "Crew message: `crew_message:%d`\n", row.SentSequence)
	if row.RunID != nil {
		fmt.Fprintf(&b, "Evidence run: `%s`\n", row.RunID.String())
	}
	for _, e := range msg.Evidence {
		fmt.Fprintf(&b, "Evidence: `%s:%s`\n", e.Kind, e.Ref)
	}
	return b.String()
}
