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

// resolveDeclaredDocuments is the shared resolve/render core behind BOTH
// prompt endpoints: it resolves, optionally attributes, and renders the
// repo-authored documents declared for this run/stage (E55.1 / #2242),
// returning them as the plain-data prompt.InjectedDocument values buildPlan /
// buildPlanReview / buildImplement / buildImplementReview render at the head of
// the cache-stable prefix. `attribute` gates the audit write and NOTHING else,
// so the served wrapper (resolveInjectedDocuments) and the preview wrapper
// (previewInjectedDocuments) cannot diverge on rendered bytes or on any refusal
// (E54.12 / #2804).
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
// A COMPLETE ATTRIBUTION SET IS NOT PROOF OF A SERVE. This call runs BEFORE
// prompt.Build at its call site, so a failure after attribution succeeds — the
// ErrUnsupportedStage branch below Build, any other build failure, or a failed
// response write — leaves a complete, well-formed injection set behind for a
// prompt no agent ever read. A SHORT set still means the assembly failed and
// nothing was injected; a COMPLETE set means only "resolved and handed to the
// renderer". Consumers establish an actual serve from the stage's own evidence.
// Rationale and the reordering trade-off: backend/internal/repodoc/README.md.
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
func (s *Server) resolveDeclaredDocuments(ctx context.Context, runRow *run.Run, stage *run.Stage, attribute bool) ([]prompt.InjectedDocument, error) {
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
	//
	// THE ONE PHASE THE PREVIEW SKIPS. Everything above this point is shared
	// verbatim between the served and preview wrappers, so the rendered bytes
	// and every refusal are computed by identical code; `attribute` gates this
	// call alone. See previewInjectedDocuments for why.
	if attribute {
		if err := repodoc.Attribute(ctx, s.cfg.AuditRepo, runRow.ID, stage.ID, docs...); err != nil {
			return nil, err
		}
	}

	out := make([]prompt.InjectedDocument, 0, len(docs))
	for i, doc := range docs {
		out = append(out, repodoc.ToPromptDocument(doc, decls[i].Framing))
	}
	return out, nil
}

// resolveInjectedDocuments is the SERVED path's wrapper (GET
// /v0/stages/{id}/prompt): it resolves, ATTRIBUTES and renders. Behaviour is
// byte-identical to the pre-split single function — this is the only caller
// that writes document_injected / document_truncated entries.
func (s *Server) resolveInjectedDocuments(ctx context.Context, runRow *run.Run, stage *run.Stage) ([]prompt.InjectedDocument, error) {
	return s.resolveDeclaredDocuments(ctx, runRow, stage, true)
}

// previewInjectedDocuments is the PREVIEW path's wrapper (GET
// /v0/stages/{id}/prompt-render): the same resolve/render core with
// ATTRIBUTION SUPPRESSED (E54.12 / #2804).
//
// WHY ATTRIBUTION IS SUPPRESSED. A document_injected entry is a claim that a
// specific revision of a document CONSTRAINED AN AGENT on this run
// (backend/internal/repodoc/README.md). A preview constrains no agent — no
// agent ever reads its bytes — so attributing it would falsify the claim the
// entry is made of. And /prompt-render is an unsigned read-access GET the SPA
// re-fetches on every session view, so attributing it would let a pure READ
// surface append unbounded chained rows to an append-only audit log.
//
// RESIDUAL, STATED PLAINLY. The preview therefore leaves NO audit trace at
// all: the audit log answers "which revision constrained the run", never
// "which revision did the operator preview". A preview served from a document
// that was later edited is not reconstructible after the fact.
//
// EVERYTHING ELSE IS SHARED. Resolution, base-ref pinning, the credential
// scope, the fail-closed ordering and every error this can return come from
// resolveDeclaredDocuments unchanged, so the two prompt endpoints agree on the
// rendered document block and on every refusal.
func (s *Server) previewInjectedDocuments(ctx context.Context, runRow *run.Run, stage *run.Stage) ([]prompt.InjectedDocument, error) {
	return s.resolveDeclaredDocuments(ctx, runRow, stage, false)
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
