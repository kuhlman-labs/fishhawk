package repodoc

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
)

// Audit categories written by this package. All three are registered in
// audit.KnownCategories (backend/internal/audit/categories.go) so
// fishhawk_await_audit and GET /v0/runs/{id}/audit accept them without
// allow_unknown; the audit AST completeness sweep collects them from these
// category-named consts and fails the build if any is unregistered.
const (
	// categoryDocumentInjected records one repo-authored document injected
	// into an agent prompt: which path, at which commit, with which content
	// hash, and where the declaration came from.
	categoryDocumentInjected = "document_injected"
	// categoryDocumentTruncated records that the injected document exceeded
	// the effective cap and was cut. Written IN ADDITION to
	// document_injected, never instead of it, so a truncation is visible in
	// the audit trail as its own event rather than only as a flag. It has a
	// SECOND payload shape, discriminated by the "selection" key: a
	// SelectionTruncation, recording that whole documents of a ranked
	// selection were left out to fit a byte budget (see AttributeSet).
	categoryDocumentTruncated = "document_truncated"
	// categoryDocumentInjectionDegraded records that declared documents were
	// WITHHELD from a served prompt rather than resolved (E55.7 / #3746):
	// which paths, which declaration sites, and why. A withheld document is
	// never fetched, so this entry — not a document_injected entry — is the
	// audit trace of the declaration.
	categoryDocumentInjectionDegraded = "document_injection_degraded"
)

// WithheldReasonRunBaseUnrecorded is the Withheld.Reason for run-admission
// declarations on a run whose admission commit was never recorded (a legacy
// row, a run created with no document seam wired, or a degraded capture).
// Resolving them at any other ref would be the mutable read BaseSourceRunAdmission
// exists to rule out, so they are withheld instead.
const WithheldReasonRunBaseUnrecorded = "run_base_commit_unrecorded"

// WithheldReasonOptionalDocumentMissing is the Withheld.Reason for OPTIONAL
// declarations (a review convention declared `required: false`, E55.3 / #2244)
// whose document was RESOLVED at the pinned commit and found absent
// (ErrMissingDocument). Unlike WithheldReasonRunBaseUnrecorded the document WAS
// looked for; it is withheld because there is nothing to inject, and the
// declaration's author opted into skipping rather than failing. A REQUIRED
// declaration that is missing is an error, never this reason.
const WithheldReasonOptionalDocumentMissing = "optional_document_missing"

// Withheld is a set of declarations a prompt assembly deliberately did NOT
// resolve, and the reason. RecordWithheld attributes it; WithheldNotice
// renders it into the prompt so the agent is told the documents exist and
// were withheld, rather than being left to read their absence as "none
// declared".
type Withheld struct {
	// Reason is a fixed machine-readable reason, e.g.
	// WithheldReasonRunBaseUnrecorded or
	// WithheldReasonOptionalDocumentMissing. Required.
	Reason string
	// Declarations are the withheld declarations, in declaration order.
	Declarations []Declaration
}

// appender is the narrow view of audit.Repository this package needs.
// audit.Repository satisfies it.
type appender interface {
	AppendChained(ctx context.Context, p audit.ChainAppendParams) (*audit.Entry, error)
}

// DroppedDocument is one document a selection left OUT of a prompt because
// it did not fit the selection's byte budget.
type DroppedDocument struct {
	// ID is the consumer's identifier for the document (e.g. a record id).
	ID string
	// Path is the document's repo-relative path.
	Path string
	// Status is the consumer's status label for the document, recorded so a
	// reader can tell what kind of document was dropped without re-reading it.
	Status string
	// Rank is the document's 1-based position in the selection's ranking.
	Rank int
}

// SelectionTruncation records that a ranked SELECTION of documents was cut to
// fit a byte budget: some whole documents were left out of the prompt. It is
// written as a document_truncated entry whose payload carries a "selection"
// key — the discriminator from the per-document shape, which never carries
// one.
type SelectionTruncation struct {
	// Selection is a fixed machine-readable name for the selection, chosen by
	// the consumer. Required: it is the payload's shape discriminator.
	Selection string
	// Path is the path of the document the selection was made FROM (e.g. the
	// index that named the candidates), at Commit.
	Path string
	// Commit is the commit the selection was resolved at.
	Commit string
	// CapBytes is the byte budget the selection was fitted to.
	CapBytes int
	// IncludedBytes is the rendered bytes the selection DID inject, counted in
	// the same domain as CapBytes.
	IncludedBytes int
	// Dropped are the documents left out, in rank order. Required: a
	// truncation that dropped nothing is a contradiction.
	Dropped []DroppedDocument
}

// InjectionSet is the COMPLETE set of documents ONE prompt assembly will
// inject, plus any selection truncations that shaped it. AttributeSet records
// it under one injection_set_id.
type InjectionSet struct {
	Documents  []Document
	Selections []SelectionTruncation
}

// Attribute records the injection of docs — the COMPLETE set of documents ONE
// prompt assembly will inject — in the audit trail: one document_truncated
// entry for every document that was cut, then one document_injected entry per
// document. It is AttributeSet with no selection truncations.
//
// FAILS CLOSED. An append error is returned and the caller MUST NOT inject any
// of the documents — an UN-ATTRIBUTED injection is precisely what the
// attribution property forbids, so "log the error and inject anyway" is a
// defect, not a degrade. Callers assert the outcome (no document injected),
// not merely the error.
//
// APPEND ORDER IS LOAD-BEARING. A hash-chained append-only log cannot un-write
// an entry, so ordering is what keeps a FAILED assembly from leaving a
// successful-injection CLAIM for a document no prompt ever carried:
//
//	(1) every document_truncated entry is appended FIRST, across the whole set.
//	    A failure anywhere in that phase therefore leaves at most truncation
//	    events — never an injection claim. (Appending the pair in the other
//	    order left document_injected persisted whenever the truncation append
//	    failed, claiming an injection the caller then refused to make.)
//	(2) document_injected is consequently the LAST append for any document,
//	    which makes it the commit point for that document.
//
// The caller closes the other half: Server.resolveInjectedDocuments resolves
// EVERY declaration before calling Attribute at all, so a later declaration
// that fails to resolve cannot leave an earlier document's entries behind.
//
// SET IDENTITY makes the one irreducible residual self-evident. Appends k and
// k+1 cannot be made atomic without a transactional batch append that
// audit.Repository does not expose, so an append failure PART WAY through
// phase (2) can still leave earlier document_injected entries. Every entry of
// one Attribute call therefore carries the same injection_set_id, and every
// document_injected carries document_index plus document_count: a COMPLETE set
// is exactly document_count document_injected entries sharing one
// injection_set_id, and a SHORT set means the assembly failed and NO document
// reached the prompt. A reader can tell the two apart; without the set fields
// a partial set is indistinguishable from a successful one. See the README.
//
// PER-SERVE, BY DESIGN. Attribution is written at prompt-serve time, so
// every fetch of a stage prompt appends a fresh SET — a retry, a
// re-dispatch, or an operator inspecting the prompt each accumulate another
// set for the same document revisions. That is intended: the guarantee is
// "every injection is attributed", and de-duplicating by content hash would
// trade it for "some injections are attributed". The injection_set_id is also
// what keeps set-completeness readable across those repeats. See the README.
func Attribute(ctx context.Context, a appender, runID, stageID uuid.UUID, docs ...Document) error {
	return AttributeSet(ctx, a, runID, stageID, InjectionSet{Documents: docs})
}

// AttributeSet is Attribute for a set that may also carry selection
// truncations. Phase (1) writes every per-document document_truncated entry
// AND every selection-level document_truncated entry before phase (2) writes
// any document_injected, so a failure while recording what was LEFT OUT can
// never leave an injection claim behind. Every entry shares one
// injection_set_id; document_count counts set.Documents only.
//
// A selection truncation with an empty Selection name or no Dropped document
// is refused BEFORE any append, so a malformed set leaves no entry at all.
func AttributeSet(ctx context.Context, a appender, runID, stageID uuid.UUID, set InjectionSet) error {
	docs := set.Documents
	if len(docs) == 0 && len(set.Selections) == 0 {
		return nil
	}
	if a == nil {
		return fmt.Errorf("repodoc: attribute %q: no audit appender configured", set.firstPath())
	}
	for i, sel := range set.Selections {
		if sel.Selection == "" {
			return fmt.Errorf("repodoc: attribute selection truncation %d of %q: no selection name given", i, sel.Path)
		}
		if len(sel.Dropped) == 0 {
			return fmt.Errorf("repodoc: attribute selection truncation %q of %q: no dropped document listed", sel.Selection, sel.Path)
		}
	}
	actor := audit.ActorSystem
	sid := stageID
	setID := uuid.New().String()

	appendEntry := func(category, subject, site string, payload map[string]any) error {
		payload["injection_set_id"] = setID
		raw, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("repodoc: marshal %s payload for %q: %w", category, subject, err)
		}
		if _, err := a.AppendChained(ctx, audit.ChainAppendParams{
			RunID:     runID,
			StageID:   &sid,
			Timestamp: time.Now().UTC(),
			Category:  category,
			ActorKind: &actor,
			Payload:   raw,
		}); err != nil {
			return fmt.Errorf("repodoc: attribute %s of %q (declared at %s): %w", category, subject, site, err)
		}
		return nil
	}

	// Phase 1 — truncations, per document and per selection. Written before
	// ANY injection claim so a failure here cannot leave one behind.
	for _, doc := range docs {
		if !doc.Truncated {
			continue
		}
		if err := appendEntry(categoryDocumentTruncated, doc.Path, doc.DeclarationSite, map[string]any{
			"path":          doc.Path,
			"commit":        doc.Commit,
			"content_hash":  doc.ContentHash,
			"cap_bytes":     doc.CapBytes,
			"dropped_bytes": doc.DroppedBytes,
		}); err != nil {
			return err
		}
	}
	for _, sel := range set.Selections {
		dropped := make([]map[string]any, 0, len(sel.Dropped))
		for _, d := range sel.Dropped {
			dropped = append(dropped, map[string]any{
				"id":     d.ID,
				"path":   d.Path,
				"status": d.Status,
				"rank":   d.Rank,
			})
		}
		if err := appendEntry(categoryDocumentTruncated, sel.Path, "selection "+sel.Selection, map[string]any{
			"selection":      sel.Selection,
			"path":           sel.Path,
			"commit":         sel.Commit,
			"cap_bytes":      sel.CapBytes,
			"included_bytes": sel.IncludedBytes,
			"dropped":        dropped,
			"dropped_count":  len(sel.Dropped),
		}); err != nil {
			return err
		}
	}

	// Phase 2 — the injection claims, each the commit point for its document.
	for i, doc := range docs {
		if err := appendEntry(categoryDocumentInjected, doc.Path, doc.DeclarationSite, map[string]any{
			"declaration_site": doc.DeclarationSite,
			"path":             doc.Path,
			"commit":           doc.Commit,
			"content_hash":     doc.ContentHash,
			"original_bytes":   doc.OriginalBytes,
			"rendered_bytes":   doc.RenderedBytes,
			"truncated":        doc.Truncated,
			"base_source":      doc.BaseSource.AuditValue(),
			"document_index":   i,
			"document_count":   len(docs),
		}); err != nil {
			return err
		}
	}
	return nil
}

// firstPath names the set's first subject for an error message.
func (s InjectionSet) firstPath() string {
	if len(s.Documents) > 0 {
		return s.Documents[0].Path
	}
	return s.Selections[0].Path
}

// RecordWithheld writes ONE document_injection_degraded entry naming every
// withheld declaration's path and declaration site, the reason, and the
// count. An empty set is a no-op.
//
// FAILS CLOSED, exactly like Attribute: an append error (or a nil appender, or
// an empty Reason) is returned and the caller MUST NOT serve the prompt. A
// served prompt silently missing a declared document with no audit trace is
// the un-attributed omission this entry exists to prevent; the preview path,
// which writes nothing, does not call it.
func RecordWithheld(ctx context.Context, a appender, runID, stageID uuid.UUID, w Withheld) error {
	if len(w.Declarations) == 0 {
		return nil
	}
	if a == nil {
		return fmt.Errorf("repodoc: record withheld %q: no audit appender configured", w.Declarations[0].Path)
	}
	if w.Reason == "" {
		return fmt.Errorf("repodoc: record withheld %q: no reason given", w.Declarations[0].Path)
	}
	paths := make([]string, 0, len(w.Declarations))
	sites := make([]string, 0, len(w.Declarations))
	for _, d := range w.Declarations {
		paths = append(paths, d.Path)
		sites = append(sites, d.DeclarationSite)
	}
	raw, err := json.Marshal(map[string]any{
		"reason":            w.Reason,
		"paths":             paths,
		"declaration_sites": sites,
		"document_count":    len(w.Declarations),
	})
	if err != nil {
		return fmt.Errorf("repodoc: marshal %s payload: %w", categoryDocumentInjectionDegraded, err)
	}
	actor := audit.ActorSystem
	sid := stageID
	if _, err := a.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     runID,
		StageID:   &sid,
		Timestamp: time.Now().UTC(),
		Category:  categoryDocumentInjectionDegraded,
		ActorKind: &actor,
		Payload:   raw,
	}); err != nil {
		return fmt.Errorf("repodoc: attribute %s (%s) of %d document(s): %w",
			categoryDocumentInjectionDegraded, w.Reason, len(w.Declarations), err)
	}
	return nil
}
