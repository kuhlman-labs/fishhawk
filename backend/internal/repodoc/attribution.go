package repodoc

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
)

// Audit categories written by this package. Both are registered in
// audit.KnownCategories (backend/internal/audit/categories.go) so
// fishhawk_await_audit and GET /v0/runs/{id}/audit accept them without
// allow_unknown; the audit AST completeness sweep collects them from these
// category-named consts and fails the build if either is unregistered.
const (
	// categoryDocumentInjected records one repo-authored document injected
	// into an agent prompt: which path, at which commit, with which content
	// hash, and where the declaration came from.
	categoryDocumentInjected = "document_injected"
	// categoryDocumentTruncated records that the injected document exceeded
	// the effective cap and was cut. Written IN ADDITION to
	// document_injected, never instead of it, so a truncation is visible in
	// the audit trail as its own event rather than only as a flag.
	categoryDocumentTruncated = "document_truncated"
)

// appender is the narrow view of audit.Repository this package needs.
// audit.Repository satisfies it.
type appender interface {
	AppendChained(ctx context.Context, p audit.ChainAppendParams) (*audit.Entry, error)
}

// Attribute records the injection of docs — the COMPLETE set of documents ONE
// prompt assembly will inject — in the audit trail: one document_truncated
// entry for every document that was cut, then one document_injected entry per
// document.
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
	if len(docs) == 0 {
		return nil
	}
	if a == nil {
		return fmt.Errorf("repodoc: attribute %q: no audit appender configured", docs[0].Path)
	}
	actor := audit.ActorSystem
	sid := stageID
	setID := uuid.New().String()

	appendEntry := func(category string, doc Document, payload map[string]any) error {
		payload["injection_set_id"] = setID
		raw, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("repodoc: marshal %s payload for %q: %w", category, doc.Path, err)
		}
		if _, err := a.AppendChained(ctx, audit.ChainAppendParams{
			RunID:     runID,
			StageID:   &sid,
			Timestamp: time.Now().UTC(),
			Category:  category,
			ActorKind: &actor,
			Payload:   raw,
		}); err != nil {
			return fmt.Errorf("repodoc: attribute %s of %q (declared at %s): %w", category, doc.Path, doc.DeclarationSite, err)
		}
		return nil
	}

	// Phase 1 — truncations. Written before ANY injection claim so a failure
	// here cannot leave one behind.
	for _, doc := range docs {
		if !doc.Truncated {
			continue
		}
		if err := appendEntry(categoryDocumentTruncated, doc, map[string]any{
			"path":          doc.Path,
			"commit":        doc.Commit,
			"content_hash":  doc.ContentHash,
			"cap_bytes":     doc.CapBytes,
			"dropped_bytes": doc.DroppedBytes,
		}); err != nil {
			return err
		}
	}

	// Phase 2 — the injection claims, each the commit point for its document.
	for i, doc := range docs {
		if err := appendEntry(categoryDocumentInjected, doc, map[string]any{
			"declaration_site": doc.DeclarationSite,
			"path":             doc.Path,
			"commit":           doc.Commit,
			"content_hash":     doc.ContentHash,
			"original_bytes":   doc.OriginalBytes,
			"rendered_bytes":   doc.RenderedBytes,
			"truncated":        doc.Truncated,
			"document_index":   i,
			"document_count":   len(docs),
		}); err != nil {
			return err
		}
	}
	return nil
}
