package server

// Delegation confirmation on handover (E76.5 / #3768, ADR-083 #3751 rule 7).
// Three routes under the E76.1 delegation read:
//
//   - GET  /v0/repos/{owner}/{name}/delegation/confirmation   every workflow
//     of the delegation view at the source/ref, with its confirmation verdict
//     (chain-only handover half + hash-staleness half). Never writes.
//   - POST /v0/repos/{owner}/{name}/delegation/confirm        the sitting
//     captain confirms ONE workflow, binding the exact content_hash it was
//     shown; refused when that hash is no longer current.
//   - POST /v0/repos/{owner}/{name}/delegation/lower          the sitting
//     captain proposes a STRICTLY lower delegation for one workflow. Files one
//     autonomy:low work item carrying the proposed .fishhawk/workflows.yaml
//     edit through the run-absent operator filing path and touches NO
//     repository file. There is no raise verb.
//
// The workflow INVENTORY comes from the delegation view at the source/ref;
// the verdict is folded from the chain (delegationconfirm.Derive). The spec
// source contract (source=ref|run_cache, echoed, never silently switched) is
// the delegation read's own (delegation_view.go).
//
// Long-form contract: backend/internal/server/README.md § "Delegation
// confirmation".

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/delegationconfirm"
	"github.com/kuhlman-labs/fishhawk/backend/internal/delegationview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
	workmgmtgithub "github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt/github"
)

// DelegationConfirmStore is the delegation-confirmation persistence seam;
// production is *delegationconfirm.Store, whose Append serializes the captain
// check with the append under the captain record's lock.
type DelegationConfirmStore interface {
	Read(ctx context.Context, accountID *uuid.UUID, repo string) (*delegationconfirm.Snapshot, error)
	Append(ctx context.Context, p delegationconfirm.AppendParams, decide func(delegationconfirm.State) (delegationconfirm.Event, error)) (*delegationconfirm.Applied, error)
}

const (
	// scopeDelegationConfirmWrite gates confirm and lower: the EXISTING
	// write:approvals scope, so the Auth checklist's impact inventory is empty.
	scopeDelegationConfirmWrite = "write:approvals"
	delegationConfirmMaxBody    = 8 << 10
	// delegationLowerLabel is the autonomy label every lower filing carries:
	// the proposed edit is human-authored work.
	delegationLowerLabel = "autonomy:low"
)

// delegationConfirmRefusal is one typed error's wire mapping.
type delegationConfirmRefusal struct {
	status int
	code   string
}

// delegationConfirmRefusals maps each sentinel to its OWN status and code —
// a table so TestDelegationConfirmRefusals_EachDistinct can assert no two
// refusal modes share a code. (workflow_not_found and delegation_hash_stale
// are written directly by the handler; they carry no sentinel.)
var delegationConfirmRefusals = []struct {
	err error
	delegationConfirmRefusal
}{
	{delegationconfirm.ErrActorRequired, delegationConfirmRefusal{http.StatusUnauthorized, "authentication_required"}},
	{delegationconfirm.ErrAgentIdentity, delegationConfirmRefusal{http.StatusForbidden, "delegation_agent_identity_refused"}},
	{delegationconfirm.ErrNotCaptain, delegationConfirmRefusal{http.StatusForbidden, "delegation_not_captain"}},
	{delegationconfirm.ErrNoCaptain, delegationConfirmRefusal{http.StatusConflict, "delegation_no_captain"}},
	{delegationconfirm.ErrRaiseRefused, delegationConfirmRefusal{http.StatusBadRequest, "delegation_raise_refused"}},
	{delegationconfirm.ErrNothingProposed, delegationConfirmRefusal{http.StatusBadRequest, "delegation_nothing_proposed"}},
	{delegationconfirm.ErrReasonRequired, delegationConfirmRefusal{http.StatusBadRequest, "validation_failed"}},
}

func delegationConfirmErrorStatus(err error) (delegationConfirmRefusal, bool) {
	for _, m := range delegationConfirmRefusals {
		if errors.Is(err, m.err) {
			return m.delegationConfirmRefusal, true
		}
	}
	return delegationConfirmRefusal{http.StatusInternalServerError, "internal_error"}, false
}

func (s *Server) writeDelegationConfirmError(w http.ResponseWriter, r *http.Request, err error, details map[string]any) {
	ref, ok := delegationConfirmErrorStatus(err)
	if !ok {
		// Unmapped validation sentinels (workflow/hash/paths/filed_ref
		// required) are request-shape faults, not internal ones.
		for _, v := range []error{delegationconfirm.ErrWorkflowRequired, delegationconfirm.ErrContentHashRequired,
			delegationconfirm.ErrEscalationPathsRequired, delegationconfirm.ErrFiledRefRequired, delegationconfirm.ErrRepoRequired} {
			if errors.Is(err, v) {
				s.writeError(w, r, http.StatusBadRequest, "validation_failed", err.Error(), details)
				return
			}
		}
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"delegation confirmation failed", map[string]any{"error": err.Error()})
		return
	}
	s.writeError(w, r, ref.status, ref.code, err.Error(), details)
}

// delegationConfirmConfigured writes the 501 envelope when the store is not
// wired.
func (s *Server) delegationConfirmConfigured(w http.ResponseWriter, r *http.Request) bool {
	if s.cfg.DelegationConfirmStore != nil {
		return true
	}
	s.writeError(w, r, http.StatusNotImplemented, "delegation_confirm_unconfigured",
		"delegation confirmation is not wired on this deployment; missing: delegation_confirm_store", nil)
	return false
}

// normalizeDelegationSource resolves the source value ("" = ref).
func (s *Server) normalizeDelegationSource(w http.ResponseWriter, r *http.Request, source string) (string, bool) {
	switch source {
	case "":
		return delegationSourceRef, true
	case delegationSourceRef, delegationSourceRunCache:
		return source, true
	}
	s.writeError(w, r, http.StatusBadRequest, "validation_failed",
		fmt.Sprintf("source must be %q or %q", delegationSourceRef, delegationSourceRunCache),
		map[string]any{"field": "source", "got": source,
			"accepted": []string{delegationSourceRef, delegationSourceRunCache}})
	return "", false
}

// loadDelegationWorkflows reads the spec at source/ref through the delegation
// read's own helpers and projects it. It writes the refusal and returns false
// on any failure (same codes as GET .../delegation).
func (s *Server) loadDelegationWorkflows(w http.ResponseWriter, r *http.Request, repo, source, ref string) ([]delegationview.WorkflowDelegation, string, bool) {
	var specBytes []byte
	var workflowSHA string
	var ok bool
	if source == delegationSourceRef {
		specBytes, workflowSHA, ok = s.delegationSpecFromRef(w, r, repo, ref)
	} else {
		specBytes, workflowSHA, ok = s.delegationSpecFromRunCache(w, r, repo)
	}
	if !ok {
		return nil, "", false
	}
	parsed, err := spec.ParseBytes(specBytes)
	if err != nil {
		s.writeError(w, r, http.StatusUnprocessableEntity, "workflow_spec_invalid",
			"the workflow spec at this source does not validate; delegation cannot be confirmed against an invalid spec",
			map[string]any{"repo": repo, "source": source, "error": err.Error()})
		return nil, "", false
	}
	return delegationview.Project(parsed), workflowSHA, true
}

func findDelegationWorkflow(ws []delegationview.WorkflowDelegation, id string) (delegationview.WorkflowDelegation, bool) {
	for _, wf := range ws {
		if wf.ID == id {
			return wf, true
		}
	}
	return delegationview.WorkflowDelegation{}, false
}

// delegationStatuses derives the full verdict (chain-only half + hash half)
// for the given workflows from a snapshot.
func delegationStatuses(st delegationconfirm.State, ws []delegationview.WorkflowDelegation) []delegationconfirm.WorkflowStatus {
	ids := make([]string, 0, len(ws))
	current := make(map[string]string, len(ws))
	for _, wf := range ws {
		ids = append(ids, wf.ID)
		current[wf.ID] = wf.ContentHash
	}
	return delegationconfirm.ApplyCurrentHashes(delegationconfirm.Statuses(st, ids), current)
}

// delegationConfirmationResponse is the GET .../delegation/confirmation body.
type delegationConfirmationResponse struct {
	Repo                 string                             `json:"repo"`
	Source               string                             `json:"source"`
	Ref                  string                             `json:"ref,omitempty"`
	WorkflowSHA          string                             `json:"workflow_sha,omitempty"`
	Captain              *string                            `json:"captain"`
	SeatSequence         int64                              `json:"seat_sequence"`
	Workflows            []delegationconfirm.WorkflowStatus `json:"workflows"`
	UnconfirmedWorkflows []string                           `json:"unconfirmed_workflows"`
	SkippedEntries       int                                `json:"skipped_entries"`
	IgnoredEntries       int                                `json:"ignored_entries"`
}

func captainPtr(st delegationconfirm.State) *string {
	if st.Captain == "" {
		return nil
	}
	c := st.Captain
	return &c
}

// handleGetDelegationConfirmation serves GET .../delegation/confirmation.
func (s *Server) handleGetDelegationConfirmation(w http.ResponseWriter, r *http.Request) {
	if !s.delegationConfirmConfigured(w, r) {
		return
	}
	repo, _, ok := s.repoDashPrelude(w, r, false, false)
	if !ok {
		return
	}
	q := r.URL.Query()
	source, ok := s.normalizeDelegationSource(w, r, q.Get("source"))
	if !ok {
		return
	}
	ref := q.Get("ref")
	ws, workflowSHA, ok := s.loadDelegationWorkflows(w, r, repo, source, ref)
	if !ok {
		return
	}
	snap, err := s.cfg.DelegationConfirmStore.Read(r.Context(), identityAccountID(r.Context()), repo)
	if err != nil {
		s.writeDelegationConfirmError(w, r, err, nil)
		return
	}
	statuses := delegationStatuses(snap.State, ws)
	s.writeJSON(w, r, http.StatusOK, delegationConfirmationResponse{
		Repo: repo, Source: source, Ref: ref, WorkflowSHA: workflowSHA,
		Captain: captainPtr(snap.State), SeatSequence: snap.State.SeatSequence,
		Workflows: statuses, UnconfirmedWorkflows: delegationconfirm.Unconfirmed(statuses),
		SkippedEntries: snap.State.SkippedEntries, IgnoredEntries: snap.State.IgnoredEntries,
	})
}

// delegationConfirmRequest is the POST .../delegation/confirm body.
type delegationConfirmRequest struct {
	Workflow    string `json:"workflow"`
	ContentHash string `json:"content_hash"`
	Source      string `json:"source,omitempty"`
	Ref         string `json:"ref,omitempty"`
	Delegated   bool   `json:"delegated,omitempty"`
}

// delegationLowerRequest is the POST .../delegation/lower body. It carries no
// field that can name a higher tier than ValidateLower admits, and no
// approvals field at all.
type delegationLowerRequest struct {
	Workflow           string                        `json:"workflow"`
	ProposedTier       string                        `json:"proposed_tier,omitempty"`
	ProposedEscalation *delegationconfirm.Escalation `json:"proposed_escalation,omitempty"`
	Reason             string                        `json:"reason"`
	Source             string                        `json:"source,omitempty"`
	Ref                string                        `json:"ref,omitempty"`
	// ParentEpic / TitleVars / Labels ride to the filing (the repo's
	// conventions may require an {epic}/{n} title). Any autonomy:* label is
	// replaced by autonomy:low.
	ParentEpic string            `json:"parent_epic,omitempty"`
	TitleVars  map[string]string `json:"title_vars,omitempty"`
	Labels     []string          `json:"labels,omitempty"`
	Delegated  bool              `json:"delegated,omitempty"`
}

// delegationEventItem is the recorded chain entry on the wire.
type delegationEventItem struct {
	Sequence  int64           `json:"sequence"`
	EntryHash string          `json:"entry_hash"`
	Category  string          `json:"category"`
	At        time.Time       `json:"at"`
	Payload   json.RawMessage `json:"payload"`
}

// delegationFiledItem is the lower path's filed work item.
type delegationFiledItem struct {
	Number        int      `json:"number"`
	URL           string   `json:"url"`
	Title         string   `json:"title"`
	AppliedLabels []string `json:"applied_labels,omitempty"`
}

// delegationVerbResponse is both write verbs' 200 body.
type delegationVerbResponse struct {
	Repo     string                           `json:"repo"`
	Workflow delegationconfirm.WorkflowStatus `json:"workflow"`
	Event    delegationEventItem              `json:"event"`
	Filed    *delegationFiledItem             `json:"filed,omitempty"`
}

// decodeDelegationBody decodes a bounded, strict JSON body.
func (s *Server) decodeDelegationBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, delegationConfirmMaxBody)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"request body is not valid JSON or contains unknown fields", map[string]any{"error": err.Error()})
		return false
	}
	return true
}

// delegationWritePrelude is the shared head of both write verbs: scope, store,
// repo prelude.
func (s *Server) delegationWritePrelude(w http.ResponseWriter, r *http.Request) (string, bool) {
	if !s.requireWriteScope(w, r, scopeDelegationConfirmWrite) {
		return "", false
	}
	if !s.delegationConfirmConfigured(w, r) {
		return "", false
	}
	repo, _, ok := s.repoDashPrelude(w, r, false, false)
	return repo, ok
}

// handleDelegationConfirm serves POST .../delegation/confirm.
//
// Refusals, each with its own status and code: agent/run-bound/delegated
// identity (403 delegation_agent_identity_refused), not the captain (403
// delegation_not_captain), vacant seat (409 delegation_no_captain), unknown
// workflow (404 workflow_not_found), hash no longer current (409
// delegation_hash_stale), and the store unwired (501).
func (s *Server) handleDelegationConfirm(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.delegationWritePrelude(w, r)
	if !ok {
		return
	}
	var req delegationConfirmRequest
	if !s.decodeDelegationBody(w, r, &req) {
		return
	}
	req.Workflow, req.ContentHash = strings.TrimSpace(req.Workflow), strings.TrimSpace(req.ContentHash)
	if req.Workflow == "" || req.ContentHash == "" {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"workflow and content_hash are required", map[string]any{"fields": []string{"workflow", "content_hash"}})
		return
	}
	ctx := r.Context()
	subject := IdentityFrom(ctx).Subject
	params := delegationconfirm.Params{
		Repo: repo, Workflow: req.Workflow, Actor: subject,
		ActorIsAgent: captainActorIsAgent(subject), ActorIsDelegated: req.Delegated,
		ContentHash: req.ContentHash,
	}
	// The ONE agent/run-bound/delegated refusal, before any spec read.
	if err := delegationconfirm.GuardActor(params); err != nil {
		s.writeDelegationConfirmError(w, r, err, map[string]any{"repo": repo, "workflow": req.Workflow})
		return
	}
	source, ok := s.normalizeDelegationSource(w, r, req.Source)
	if !ok {
		return
	}
	ws, workflowSHA, ok := s.loadDelegationWorkflows(w, r, repo, source, req.Ref)
	if !ok {
		return
	}
	wf, found := findDelegationWorkflow(ws, req.Workflow)
	if !found {
		s.writeError(w, r, http.StatusNotFound, "workflow_not_found",
			fmt.Sprintf("the spec at this source declares no workflow %q", req.Workflow),
			map[string]any{"field": "workflow", "got": req.Workflow, "repo": repo, "source": source})
		return
	}
	// Bind to the EXACT hash the operator was shown: a confirmation of a
	// delegation that has since changed is refused, never recorded.
	if wf.ContentHash != req.ContentHash {
		s.writeError(w, r, http.StatusConflict, "delegation_hash_stale",
			"the delegation for this workflow changed since the content_hash you confirmed was read; re-read it and confirm the current hash",
			map[string]any{"workflow": req.Workflow, "content_hash": req.ContentHash,
				"current_content_hash": wf.ContentHash, "source": source})
		return
	}
	params.WorkflowSHA = workflowSHA
	applied, err := s.cfg.DelegationConfirmStore.Append(ctx, delegationconfirm.AppendParams{
		AccountID: identityAccountID(ctx), Repo: repo, Actor: subject,
		ActorKind: actorKindForSubject(subject), Timestamp: time.Now().UTC(),
	}, func(st delegationconfirm.State) (delegationconfirm.Event, error) {
		return delegationconfirm.Confirm(st, params)
	})
	if err != nil {
		s.writeDelegationConfirmError(w, r, err, map[string]any{"repo": repo, "workflow": req.Workflow})
		return
	}
	s.writeJSON(w, r, http.StatusOK, delegationVerbResponse{
		Repo:     repo,
		Workflow: delegationStatuses(applied.State, []delegationview.WorkflowDelegation{wf})[0],
		Event:    delegationEvent(applied),
	})
}

func delegationEvent(a *delegationconfirm.Applied) delegationEventItem {
	return delegationEventItem{
		Sequence: a.Entry.Sequence, EntryHash: a.Entry.EntryHash, Category: a.Entry.Category,
		At: a.Entry.Timestamp, Payload: a.Entry.Payload,
	}
}

// handleDelegationLower serves POST .../delegation/lower.
//
// Order is load-bearing: the agent guard, the workflow lookup, ValidateLower
// and the captain PRE-check all run BEFORE the filing, so a refused proposal
// files nothing. The captain is re-checked under the captain record's lock
// when the delegation_lower_proposed entry is appended; a filing failure
// appends nothing.
func (s *Server) handleDelegationLower(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.delegationWritePrelude(w, r)
	if !ok {
		return
	}
	var req delegationLowerRequest
	if !s.decodeDelegationBody(w, r, &req) {
		return
	}
	req.Workflow = strings.TrimSpace(req.Workflow)
	req.Reason = strings.TrimSpace(req.Reason)
	if req.Workflow == "" {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"workflow is required", map[string]any{"field": "workflow"})
		return
	}
	ctx := r.Context()
	subject := IdentityFrom(ctx).Subject
	lower := delegationconfirm.LowerRequest{
		ProposedTier: strings.TrimSpace(req.ProposedTier), ProposedEscalation: req.ProposedEscalation, Reason: req.Reason,
	}
	params := delegationconfirm.Params{
		Repo: repo, Workflow: req.Workflow, Actor: subject,
		ActorIsAgent: captainActorIsAgent(subject), ActorIsDelegated: req.Delegated,
		Lower: lower,
	}
	if err := delegationconfirm.GuardActor(params); err != nil {
		s.writeDelegationConfirmError(w, r, err, map[string]any{"repo": repo, "workflow": req.Workflow})
		return
	}
	source, ok := s.normalizeDelegationSource(w, r, req.Source)
	if !ok {
		return
	}
	ws, _, ok := s.loadDelegationWorkflows(w, r, repo, source, req.Ref)
	if !ok {
		return
	}
	wf, found := findDelegationWorkflow(ws, req.Workflow)
	if !found {
		s.writeError(w, r, http.StatusNotFound, "workflow_not_found",
			fmt.Sprintf("the spec at this source declares no workflow %q", req.Workflow),
			map[string]any{"field": "workflow", "got": req.Workflow, "repo": repo, "source": source})
		return
	}
	if err := delegationconfirm.ValidateLower(wf.Autonomy, lower); err != nil {
		s.writeDelegationConfirmError(w, r, err, map[string]any{
			"workflow": req.Workflow, "current_tier": wf.Autonomy, "proposed_tier": lower.ProposedTier})
		return
	}
	snap, err := s.cfg.DelegationConfirmStore.Read(ctx, identityAccountID(ctx), repo)
	if err != nil {
		s.writeDelegationConfirmError(w, r, err, nil)
		return
	}
	if err := delegationconfirm.CheckCaptain(snap.State, subject); err != nil {
		s.writeDelegationConfirmError(w, r, err, map[string]any{"repo": repo, "workflow": req.Workflow})
		return
	}

	item, created, werr := s.fileDelegationLower(ctx, repo, wf, req, snap.State)
	if werr != nil {
		s.writeError(w, r, werr.status, werr.code, werr.msg, werr.details)
		return
	}
	params.FiledRef = created.URL
	applied, err := s.cfg.DelegationConfirmStore.Append(ctx, delegationconfirm.AppendParams{
		AccountID: identityAccountID(ctx), Repo: repo, Actor: subject,
		ActorKind: actorKindForSubject(subject), Timestamp: time.Now().UTC(),
	}, func(st delegationconfirm.State) (delegationconfirm.Event, error) {
		return delegationconfirm.ProposeLower(st, params)
	})
	if err != nil {
		// The work item exists; name it so the operator can close it.
		s.writeDelegationConfirmError(w, r, err, map[string]any{
			"repo": repo, "workflow": req.Workflow, "filed_ref": created.URL})
		return
	}
	s.writeJSON(w, r, http.StatusOK, delegationVerbResponse{
		Repo:     repo,
		Workflow: delegationStatuses(applied.State, []delegationview.WorkflowDelegation{wf})[0],
		Event:    delegationEvent(applied),
		Filed: &delegationFiledItem{
			Number: created.Number, URL: created.URL, Title: item.Title, AppliedLabels: created.AppliedLabels,
		},
	})
}

// fileDelegationLower files the ONE autonomy:low work item proposing the
// workflows.yaml edit, through the run-absent operator filing path
// (conventionsLoader -> resolveRepoScope -> applyAndFileWorkItem). It dials
// no repository write seam: the edit is a human's to author.
func (s *Server) fileDelegationLower(ctx context.Context, repo string, wf delegationview.WorkflowDelegation, req delegationLowerRequest, st delegationconfirm.State) (*workmgmt.WorkItem, *workmgmt.CreatedItem, *workItemError) {
	owner, name, _ := strings.Cut(repo, "/")
	conv, err := conventionsLoader(ctx, repo)
	if err != nil {
		return nil, nil, &workItemError{status: http.StatusInternalServerError, code: "internal_error",
			msg: "could not load work-management conventions", details: map[string]any{"error": err.Error()}}
	}
	labels := []string{}
	for _, l := range req.Labels {
		if !strings.HasPrefix(strings.TrimSpace(l), "autonomy:") {
			labels = append(labels, l)
		}
	}
	labels = append(labels, delegationLowerLabel)
	summary := fmt.Sprintf("Lower delegation for workflow %s", wf.ID)
	if req.ProposedTier != "" {
		summary = fmt.Sprintf("Lower delegation for workflow %s from %s to %s", wf.ID, wf.Autonomy, req.ProposedTier)
	}
	filing := workmgmt.FilingRequest{
		Type:      "chore",
		Summary:   summary,
		Labels:    labels,
		TitleVars: req.TitleVars,
		Sections: map[string]string{
			"Summary":    renderDelegationLowerSummary(repo, wf, req, st),
			"Done-means": "`.fishhawk/workflows.yaml` carries the edit above (human-authored), and the incoming captain confirms the resulting delegation through POST .../delegation/confirm.",
		},
	}
	if req.ParentEpic != "" {
		filing.Relations = workmgmt.Relations{ParentEpic: req.ParentEpic}
	}
	target := workmgmt.Target{
		Repo: workmgmt.Repo{Owner: owner, Name: name}, Project: conv.Project, Jira: conv.Jira, GitLab: conv.GitLab,
	}
	if s.cfg.GitHub != nil && conv.Provider == workmgmtgithub.ProviderName {
		scope, rerr := s.resolveRepoScope(ctx, owner, name)
		if rerr != nil {
			return nil, nil, &workItemError{status: http.StatusBadGateway, code: "work_item_filing_failed",
				msg: "could not resolve the GitHub App installation for the target repo", details: map[string]any{"error": rerr.Error()}}
		}
		target.Scope = scope
	}
	return s.applyAndFileWorkItem(ctx, filing, conv, target, owner, name)
}

// renderDelegationLowerSummary renders the proposal body: the handover it
// answers, the reason, and the EXACT proposed workflows.yaml edit.
func renderDelegationLowerSummary(repo string, wf delegationview.WorkflowDelegation, req delegationLowerRequest, st delegationconfirm.State) string {
	var b strings.Builder
	fmt.Fprintf(&b, "The incoming captain of `%s` proposes LOWERING the delegation of workflow `%s` on handover (ADR-083 rule 7).", repo, wf.ID)
	if st.SeatSequence > 0 {
		fmt.Fprintf(&b, " Handover: the seat change at global-chain sequence %d.", st.SeatSequence)
	}
	fmt.Fprintf(&b, "\n\nCurrent tier: `%s` (content_hash `%s`).\n\nReason: %s\n\n", wf.Autonomy, wf.ContentHash, req.Reason)
	b.WriteString("Proposed edit to `.fishhawk/workflows.yaml`:\n\n```yaml\n")
	b.WriteString(renderDelegationLowerEdit(wf.ID, req))
	b.WriteString("```\n\nThis proposal changed no repository file. Nothing is raised by it.")
	return b.String()
}

// renderDelegationLowerEdit renders the YAML fragment for the proposed edit.
// Scalars are quoted through strconv so a path cannot inject YAML structure.
func renderDelegationLowerEdit(workflow string, req delegationLowerRequest) string {
	var b strings.Builder
	fmt.Fprintf(&b, "workflows:\n  %s:\n", strconv.Quote(workflow))
	if req.ProposedTier != "" {
		fmt.Fprintf(&b, "    autonomy: %s\n", strconv.Quote(req.ProposedTier))
	}
	if e := req.ProposedEscalation; e != nil {
		paths := append([]string(nil), e.Paths...)
		sort.Strings(paths)
		b.WriteString("    escalations:  # append this entry to the existing list\n")
		b.WriteString("      - match:\n          paths:\n")
		for _, p := range paths {
			fmt.Fprintf(&b, "            - %s\n", strconv.Quote(p))
		}
		fmt.Fprintf(&b, "        require:\n          max_autonomy: %s\n", strconv.Quote(e.MaxAutonomy))
	}
	return b.String()
}
