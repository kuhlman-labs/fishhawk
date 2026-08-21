package server

import (
	"context"
	"errors"
	"fmt"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/prompt"
	"github.com/kuhlman-labs/fishhawk/backend/internal/repodoc"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// resolveInjectedDocuments resolves, attributes and renders the repo-authored
// documents declared for this run/stage (E55.1 / #2242), returning them as the
// plain-data prompt.InjectedDocument values buildPlan / buildPlanReview /
// buildImplement / buildImplementReview render at the head of the cache-stable
// prefix.
//
// INERT BY DEFAULT. Config.DocumentDeclarations and Config.DocumentResolver are
// both nil in production today — no consumer declares a document yet (E55's
// review_conventions[] and #2234's charter.path are the two that will) — so
// this returns (nil, nil) and every served prompt is byte-identical to the
// pre-#2242 render. The seam exists so both consumers attach at ONE point
// rather than each growing its own resolution path.
//
// FAILS CLOSED. A declaration that cannot be resolved, or an injection that
// cannot be attributed, fails the prompt request rather than serving a prompt
// with a governance document silently missing. Attribution failure in
// particular returns NO documents at all: an un-attributed injection is exactly
// what the attribution property forbids, so the caller must not fall back to
// injecting the resolved-but-unattributed document.
//
// RESOLUTION IS COMPLETED BEFORE ATTRIBUTION BEGINS. The two phases are
// ordered, not interleaved, because the audit log is append-only: an entry
// written for document 1 cannot be withdrawn when document 2 fails to resolve,
// so it would stand as a claim that document 1 was injected into a prompt that
// was never served.
//
// PARTIAL CONFIGURATION IS A FAILURE, NOT AN INERT STATE. Inert means NO
// declaration seam: nothing declares a document, so nothing is missing. A
// CONFIGURED declaration seam with a nil DocumentResolver is a different thing
// entirely — a consumer that intends to constrain the agent, and a deployment
// that cannot read the document. Treating that as inert would serve an
// unconstrained prompt with no error and no audit trace, and it would surface
// as an inexplicably unconstrained agent rather than as a fault. So it is an
// error, raised BEFORE the seam is consulted: the mismatch is a wiring defect
// whatever the seam would have returned.
func (s *Server) resolveInjectedDocuments(ctx context.Context, runRow *run.Run, stage *run.Stage) ([]prompt.InjectedDocument, error) {
	if runRow == nil || stage == nil {
		return nil, nil
	}
	if s.cfg.DocumentDeclarations == nil {
		return nil, nil // fully inert: no consumer declares a document
	}
	if s.cfg.DocumentResolver == nil {
		return nil, errors.New("document injection is misconfigured: DocumentDeclarations is configured but DocumentResolver is nil; " +
			"wire a resolver or remove the declaration seam")
	}
	decls, baseRef, err := s.cfg.DocumentDeclarations(ctx, runRow, stage)
	if err != nil {
		return nil, fmt.Errorf("resolve document declarations: %w", err)
	}
	if len(decls) == 0 {
		return nil, nil
	}

	repo, err := parseRepoRef(runRow.Repo)
	if err != nil {
		return nil, err
	}
	var scope forge.CredentialScope
	if s.cfg.DocumentScope != nil {
		scope, err = s.cfg.DocumentScope(ctx, repo)
		if err != nil {
			return nil, fmt.Errorf("resolve credential scope for %s: %w", repo, err)
		}
	}

	// RESOLVE EVERY DECLARATION BEFORE ANY AUDIT ENTRY IS WRITTEN. Attribution
	// used to run per document, interleaved with resolution, so a LATER
	// declaration that failed to resolve left the EARLIER documents'
	// document_injected entries persisted — audit claims that a document was
	// injected into a prompt this request then refused to serve. The audit log
	// is append-only, so the fix is ordering: nothing is claimed until the
	// whole set is known to be resolvable.
	docs := make([]repodoc.Document, 0, len(decls))
	for _, decl := range decls {
		doc, err := s.cfg.DocumentResolver.Resolve(ctx, repodoc.Request{
			Repo:        repo,
			Scope:       scope,
			BaseRef:     baseRef,
			Declaration: decl,
		})
		if err != nil {
			return nil, err
		}
		docs = append(docs, *doc)
	}

	// Attribute the whole set, and return NOTHING on failure — the injection
	// and its audit entries ship together or not at all. repodoc.Attribute
	// orders its own appends so a failure cannot leave a successful-injection
	// claim behind (truncations first, injection claims last, all tagged with
	// one injection_set_id).
	if err := repodoc.Attribute(ctx, s.cfg.AuditRepo, runRow.ID, stage.ID, docs...); err != nil {
		return nil, err
	}

	out := make([]prompt.InjectedDocument, 0, len(docs))
	for i, doc := range docs {
		out = append(out, repodoc.ToPromptDocument(doc, decls[i].Framing))
	}
	return out, nil
}

// documentInjectionErrorDetails extracts the operator-actionable identifiers
// from a repodoc failure. A *repodoc.ResolveError names the path and the
// declaration site; any other error yields the message alone.
func documentInjectionErrorDetails(err error) map[string]any {
	details := map[string]any{"error": err.Error()}
	var re *repodoc.ResolveError
	if errors.As(err, &re) {
		details["path"] = re.Path
		details["declaration_site"] = re.DeclarationSite
	}
	return details
}
