package server

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

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
// INERT WITHOUT A DECLARATION SEAM. With Config.DocumentDeclarations nil this
// returns (nil, nil) and every prompt is byte-identical to the pre-#2242
// render; that nil check is the SOLE inert signal, shared by the prompt
// endpoints and the in-process review build sites
// (resolveReviewInjectedDocuments, #2797). A configured seam that declares
// zero documents for a stage is equally byte-identical. The seam exists so
// every consumer (#2234's charter.path, E55's review_conventions[]) attaches
// at ONE point rather than each growing its own resolution path.
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
//
// TWO BASE SOURCES, PARTITIONED PER DECLARATION (E55.7 / #3746). A
// declaration on the zero repodoc.BaseSourceDeclarationSeam resolves against
// the ref the declaration seam returned, exactly as before (the charter's
// serve-time default-branch posture). A repodoc.BaseSourceRunAdmission
// declaration resolves ONLY at runRow.DocumentBaseCommit — the commit recorded
// once at run admission and inherited by every child run — so a retry, a
// recovery child or a fix-up re-serve reads the SAME revision, never a newer
// head and never the run's own branch. When the run recorded no commit (a
// legacy row, an ad-hoc run, a degraded capture) every run-admission
// declaration is WITHHELD: none is resolved or fetched, the served path
// records ONE document_injection_degraded entry (fail-closed, written before
// any document_injected claim), and BOTH endpoints render a
// repodoc.WithheldNotice after the resolved documents so the agent is told
// the documents exist and were withheld. Withholding happens before the
// resolve phase, so it changes nothing about the ordering invariant above.
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

	// PARTITION BY BASE SOURCE. A run-admission declaration on a run that
	// recorded no admission commit is WITHHELD here, before the resolve phase:
	// there is no ref it may honestly be read at (the seam's ref and the run's
	// branch are both mutable), so it is never handed to Resolve at all.
	// Declaration order is preserved within each partition.
	resolvable := make([]repodoc.Declaration, 0, len(decls))
	var withheld repodoc.Withheld
	for _, decl := range decls {
		if decl.Base == repodoc.BaseSourceRunAdmission && runRow.DocumentBaseCommit == nil {
			withheld.Declarations = append(withheld.Declarations, decl)
			continue
		}
		resolvable = append(resolvable, decl)
	}
	if len(withheld.Declarations) > 0 {
		withheld.Reason = repodoc.WithheldReasonRunBaseUnrecorded
	}

	// RESOLVE EVERY DECLARATION BEFORE ANY AUDIT ENTRY IS WRITTEN. Attribution
	// used to run per document, interleaved with resolution, so a LATER
	// declaration that failed to resolve left the EARLIER documents'
	// document_injected entries persisted — audit claims that a document was
	// injected into a prompt this request then refused to serve. The audit log
	// is append-only, so the fix is ordering: nothing is claimed until the
	// whole set is known to be resolvable.
	docs := make([]repodoc.Document, 0, len(resolvable))
	for _, decl := range resolvable {
		ref := baseRef
		if decl.Base == repodoc.BaseSourceRunAdmission {
			// Never the seam's ref: the admission commit, verbatim. A
			// malformed persisted value cannot pass the runs CHECK, and if
			// one ever arrived Resolve's run-admission guard refuses any
			// non-commit ref before a branch lookup or fetch.
			ref = *runRow.DocumentBaseCommit
		}
		doc, err := s.cfg.DocumentResolver.Resolve(ctx, repodoc.Request{
			Repo:        repo,
			Scope:       scope,
			BaseRef:     ref,
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
	// one injection_set_id). The withheld set is recorded FIRST and fails
	// closed the same way: a served prompt missing a declared document with no
	// audit trace of why is the un-attributed omission that entry prevents,
	// and writing it before any document_injected claim means its failure
	// leaves no injection claim behind.
	//
	// THE ONE PHASE THE PREVIEW SKIPS. Everything above this point is shared
	// verbatim between the served and preview wrappers, so the rendered bytes
	// and every refusal are computed by identical code; `attribute` gates this
	// block alone. See previewInjectedDocuments for why.
	if attribute {
		if err := repodoc.RecordWithheld(ctx, s.cfg.AuditRepo, runRow.ID, stage.ID, withheld); err != nil {
			return nil, err
		}
		if err := repodoc.Attribute(ctx, s.cfg.AuditRepo, runRow.ID, stage.ID, docs...); err != nil {
			return nil, err
		}
	}

	out := make([]prompt.InjectedDocument, 0, len(docs)+1)
	for i, doc := range docs {
		out = append(out, repodoc.ToPromptDocument(doc, resolvable[i].Framing))
	}
	if len(withheld.Declarations) > 0 {
		out = append(out, repodoc.WithheldNotice(withheld))
	}
	return out, nil
}

// resolveInjectedDocuments is the SERVED path's wrapper (GET
// /v0/stages/{id}/prompt, and the in-process review builds through
// resolveReviewInjectedDocuments, #2797): it resolves, ATTRIBUTES and renders.
// Behaviour is byte-identical to the pre-split single function — this is the
// only wrapper that writes document_injected / document_truncated /
// document_injection_degraded entries.
func (s *Server) resolveInjectedDocuments(ctx context.Context, runRow *run.Run, stage *run.Stage) ([]prompt.InjectedDocument, error) {
	return s.resolveDeclaredDocuments(ctx, runRow, stage, true)
}

// reviewDocumentInjectionFailedPrefix leads the reason of the
// plan_review_failed / implement_review_failed audit entry a review build site
// records when the documents declared for the reviewed stage cannot be resolved
// or attributed (#2797). It is the operator's one signal that a review produced
// no verdict because of document injection rather than a reviewer failure.
const reviewDocumentInjectionFailedPrefix = "document_injection_failed"

// reviewDocumentInjectionFailedReason renders the *_review_failed reason for a
// review-side document-injection failure. A *repodoc.ResolveError's Error()
// already names the path and declaration site, so the reason carries them.
func reviewDocumentInjectionFailedReason(err error) string {
	return reviewDocumentInjectionFailedPrefix + ": " + err.Error()
}

// resolveReviewInjectedDocuments is the REVIEW build sites' wrapper (#2797):
// runPlanReviews (plan_review), runImplementReviewsForTree (implement_review —
// the trace-time review, the fix-up re-review backstop and the decomposed
// parent's consolidated review) and runSupplementalReinvokeReview. Those
// prompts are built in-process and never pass through the signed /prompt
// endpoint, so without this they carried no declared document at all.
//
// A REVIEW PROMPT CARRIES THE DOCUMENTS DECLARED FOR THE STAGE IT REVIEWS. The
// reviewer is constrained by what constrained the author, so the declaration
// seam is consulted with the REVIEWED stage (the plan stage for plan_review,
// the implement stage for implement_review), loaded here by stageID.
//
// ONE SEAM, NO SECOND INERT SIGNAL. This wrapper adds no short-circuit of its
// own: it delegates straight to resolveInjectedDocuments, so
// Config.DocumentDeclarations == nil (checked inside resolveDeclaredDocuments,
// BEFORE the nil-resolver refusal) is the SOLE inert signal for both the
// endpoint and the review paths, and the partial-configuration refusal, the
// base-source partition and withholding, the resolve-whole-set-before-attribute
// ordering, RecordWithheld and Attribute all run unchanged. The two paths
// cannot diverge on what counts as inert. The cost is one reviewed-stage read
// per review build even when inert.
//
// ATTRIBUTION RUNS PER REVIEW BUILD. One document_injected set is written per
// review round — shared by every reviewer of that round, which all read the
// same prompt — with stage_id = the reviewed stage.
//
// FAILS CLOSED, AND CALLERS MUST HONOUR IT. Every error — a nil run row, a
// reviewed stage that cannot be loaded, a partial seam configuration, a
// declaration that cannot be resolved, an audit append that fails — means the
// declared set could not be resolved or attributed. The caller must then run
// NO reviewer (a document-less review prompt is exactly the unconstrained
// verdict injection exists to prevent), record a *_review_failed entry whose
// reason is reviewDocumentInjectionFailedReason(err), and, under gating
// authority, fail the stage category-B.
func (s *Server) resolveReviewInjectedDocuments(ctx context.Context, runRow *run.Run, stageID uuid.UUID) ([]prompt.InjectedDocument, error) {
	if runRow == nil {
		return nil, errors.New("reviewed run row is unavailable")
	}
	stage, err := s.cfg.RunRepo.GetStage(ctx, stageID)
	if err != nil {
		return nil, fmt.Errorf("load reviewed stage %s: %w", stageID, err)
	}
	if stage == nil {
		return nil, fmt.Errorf("load reviewed stage %s: stage not found", stageID)
	}
	return s.resolveInjectedDocuments(ctx, runRow, stage)
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
