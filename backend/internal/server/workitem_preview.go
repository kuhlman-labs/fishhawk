package server

// This file carries the NON-MUTATING intake preview of a work-item filing
// (#3774 / E81.4): run the repo's conventions and the intake hook on a draft
// and return exactly what a filing would send, without filing it.
//
// WHAT "NON-MUTATING" MEANS HERE, precisely. provider.File is the ONLY
// mutating provider call on the filing path — board placement, the epic
// sub-issue link and every label write happen inside it — and the preview
// never makes it. Everything prepareWorkItem does before File is a READ:
// {epic}/{n} derivation reads the parent epic and its children, the capacity
// guard reads the child count, area/phase derivation read issue labels, the
// intake hook reads the duplicate window and the charter, and sequential-number
// discovery reads the in-use numbers. Number discovery RESERVES NOTHING: the
// number a preview reports is the one a filing made at that instant would get,
// and a concurrent filing between preview and file can take it. The preview
// writes NO audit entry and creates nothing.
//
// The preview still takes the same per-epic child-number and per-(repo,
// prefix) sequential-number locks a filing takes, because they are acquired
// inside prepareWorkItem, and releases them as soon as prepare returns. A
// preview therefore briefly serializes with a filing of the same epic or
// numbered type exactly as a filing does, and never holds a lock past its own
// return.

import (
	"context"
	"net/http"

	"github.com/kuhlman-labs/fishhawk/backend/internal/intakegroom"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// workItemPreview is what a filing WOULD produce, rendered by the same
// prepareWorkItem core the filing uses. Body is the RenderBody output
// provider.File would receive (advisory section and hidden marker included).
type workItemPreview struct {
	Type                   string
	Title                  string
	Body                   string
	Labels                 []string
	Number                 int
	Provider               string
	Complexity             string
	Status                 string
	BoardColumn            string
	Relations              workmgmt.Relations
	DefaultedLabels        []string
	MissingLabelNamespaces []string
	Intake                 intakegroom.Signals
}

// previewWorkItem is the SERVER-INTERNAL preview entry point (#3774): the scan
// workflows (#3750 advisory watch, #3775 user_report_scan) call it directly,
// and POST /v0/work-items/preview reaches it through the shared request
// prelude.
//
// It runs prepareWorkItem, releases the locks prepare took immediately, and
// NEVER calls provider.File — the only mutating provider call on the path —
// and writes NO audit. Number discovery is a read with no reservation, so the
// returned Number is a point-in-time view. A *workItemError maps exactly as
// the filing handler maps it.
func (s *Server) previewWorkItem(ctx context.Context, filing workmgmt.FilingRequest, conv workmgmt.Conventions, target workmgmt.Target, owner, name string) (*workItemPreview, *workItemError) {
	prep, release, werr := s.prepareWorkItem(ctx, filing, conv, target, owner, name)
	if werr != nil {
		return nil, werr
	}
	// Released NOW, not deferred past a File that never comes: a preview must
	// not hold the child-number or sequential-number lock any longer than its
	// own reads need.
	release()

	item := prep.item
	return &workItemPreview{
		Type:                   item.Type,
		Title:                  item.Title,
		Body:                   item.Body,
		Labels:                 item.Classification.Labels,
		Number:                 prep.number,
		Provider:               conv.Provider,
		Complexity:             item.Classification.Complexity,
		Status:                 item.BoardPlacement.Status,
		BoardColumn:            item.BoardPlacement.BoardColumn,
		Relations:              item.Relations,
		DefaultedLabels:        item.Classification.DefaultedLabels,
		MissingLabelNamespaces: item.Classification.MissingLabelNamespaces,
		Intake:                 prep.signals,
	}, nil
}

// workItemPreviewResponse is the POST /v0/work-items/preview 200 body.
// Number is the sequential number a filing would allocate (omitted when the
// type is not numbered); Intake is the same advisory signals object the 201
// filing response carries, never omitted here because producing it is the
// point of the call.
type workItemPreviewResponse struct {
	Type                   string              `json:"type"`
	Title                  string              `json:"title"`
	Body                   string              `json:"body"`
	Labels                 []string            `json:"labels"`
	Number                 int                 `json:"number,omitempty"`
	Provider               string              `json:"provider"`
	Complexity             string              `json:"complexity,omitempty"`
	Status                 string              `json:"status,omitempty"`
	BoardColumn            string              `json:"board_column,omitempty"`
	Relations              *workmgmt.Relations `json:"relations,omitempty"`
	DefaultedLabels        []string            `json:"defaulted_labels,omitempty"`
	MissingLabelNamespaces []string            `json:"missing_label_namespaces,omitempty"`
	Intake                 intakegroom.Signals `json:"intake"`
}

// handlePreviewWorkItem implements POST /v0/work-items/preview (#3774): the
// filing request prelude in preview mode (operator-only, no run_id, the
// point-read repo-visibility DENY), then previewWorkItem, then 200. Error
// mapping is the filing handler's verbatim.
func (s *Server) handlePreviewWorkItem(w http.ResponseWriter, r *http.Request) {
	rq, ok := s.resolveWorkItemRequest(w, r, true)
	if !ok {
		return
	}
	pv, werr := s.previewWorkItem(r.Context(), rq.filing, rq.conv, rq.target, rq.owner, rq.name)
	if werr != nil {
		s.writeError(w, r, werr.status, werr.code, werr.msg, werr.details)
		return
	}
	labels := pv.Labels
	if labels == nil {
		labels = []string{}
	}
	var relations *workmgmt.Relations
	if !relationsEmpty(pv.Relations) {
		rel := pv.Relations
		relations = &rel
	}
	s.writeJSON(w, r, http.StatusOK, workItemPreviewResponse{
		Type:                   pv.Type,
		Title:                  pv.Title,
		Body:                   pv.Body,
		Labels:                 labels,
		Number:                 pv.Number,
		Provider:               pv.Provider,
		Complexity:             pv.Complexity,
		Status:                 pv.Status,
		BoardColumn:            pv.BoardColumn,
		Relations:              relations,
		DefaultedLabels:        pv.DefaultedLabels,
		MissingLabelNamespaces: pv.MissingLabelNamespaces,
		Intake:                 pv.Intake,
	})
}

// relationsEmpty reports whether r carries no relation at all, so the preview
// response omits an all-empty relations object rather than rendering `{}`.
func relationsEmpty(r workmgmt.Relations) bool {
	return r.ParentEpic == "" && len(r.Supersedes) == 0 && len(r.CompanionTo) == 0 &&
		len(r.EvidenceRuns) == 0 && len(r.DependsOn) == 0
}
