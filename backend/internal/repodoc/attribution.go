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

// Attribute records doc's injection in the audit trail: one
// document_injected entry always, plus a document_truncated entry when the
// document was cut.
//
// FAILS CLOSED. An append error is returned and the caller MUST NOT inject
// the document — an UN-ATTRIBUTED injection is precisely what the
// attribution property forbids, so "log the error and inject anyway" is a
// defect, not a degrade. Callers assert the outcome (no document injected),
// not merely the error.
//
// PER-SERVE, BY DESIGN. Attribution is written at prompt-serve time, so
// every fetch of a stage prompt appends fresh entries — a retry, a
// re-dispatch, or an operator inspecting the prompt each accumulate another
// pair for the same document revision. That is intended: the guarantee is
// "every injection is attributed", and de-duplicating by content hash would
// trade it for "some injections are attributed". See the README.
func Attribute(ctx context.Context, a appender, runID, stageID uuid.UUID, doc Document) error {
	if a == nil {
		return fmt.Errorf("repodoc: attribute %q: no audit appender configured", doc.Path)
	}
	actor := audit.ActorSystem
	sid := stageID

	injected, err := json.Marshal(map[string]any{
		"declaration_site": doc.DeclarationSite,
		"path":             doc.Path,
		"commit":           doc.Commit,
		"content_hash":     doc.ContentHash,
		"original_bytes":   doc.OriginalBytes,
		"rendered_bytes":   doc.RenderedBytes,
		"truncated":        doc.Truncated,
	})
	if err != nil {
		return fmt.Errorf("repodoc: marshal %s payload for %q: %w", categoryDocumentInjected, doc.Path, err)
	}
	if _, err := a.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     runID,
		StageID:   &sid,
		Timestamp: time.Now().UTC(),
		Category:  categoryDocumentInjected,
		ActorKind: &actor,
		Payload:   injected,
	}); err != nil {
		return fmt.Errorf("repodoc: attribute %q (declared at %s): %w", doc.Path, doc.DeclarationSite, err)
	}

	if !doc.Truncated {
		return nil
	}
	truncated, err := json.Marshal(map[string]any{
		"path":          doc.Path,
		"commit":        doc.Commit,
		"content_hash":  doc.ContentHash,
		"cap_bytes":     doc.CapBytes,
		"dropped_bytes": doc.DroppedBytes,
	})
	if err != nil {
		return fmt.Errorf("repodoc: marshal %s payload for %q: %w", categoryDocumentTruncated, doc.Path, err)
	}
	if _, err := a.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     runID,
		StageID:   &sid,
		Timestamp: time.Now().UTC(),
		Category:  categoryDocumentTruncated,
		ActorKind: &actor,
		Payload:   truncated,
	}); err != nil {
		return fmt.Errorf("repodoc: attribute truncation of %q (declared at %s): %w", doc.Path, doc.DeclarationSite, err)
	}
	return nil
}
