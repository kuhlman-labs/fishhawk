package server

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/prompt"
	"github.com/kuhlman-labs/fishhawk/backend/internal/repodoc"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
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
// render. The check lives in the shared core (resolveDocumentSet), so the
// prompt endpoints and the in-process review build sites (resolveReviewDocuments,
// #2797) cannot diverge on it; a review site is additionally non-inert when the
// workflow selects a review convention (E55.3 / #2244), which an author prompt
// never does. A configured seam that declares
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
	res, err := s.resolveDocumentSet(ctx, documentResolveInput{
		runRow:    runRow,
		stageID:   stage.ID,
		stage:     stage,
		attribute: attribute,
	})
	if err != nil {
		return nil, err
	}
	return res.injected, nil
}

// reviewDocumentResolveTimeout bounds the RESOLVE phase of an in-process
// review build (#2797 item 1): the declaration-seam call, the credential-scope
// lookup and every forge Resolve run on a context with this deadline. The
// implement-review site resolves while holding reviewDispatchMu (so a
// duplicate dispatch stays a silent no-op), and an unbounded forge read there
// would hold that lock for as long as the forge hangs. The audit appends
// (RecordWithheld / Attribute) deliberately run on the CALLER's context, so an
// expiring deadline can never fail a half-written attribution set. The /prompt
// endpoints get no deadline: their behaviour is unchanged.
//
// Residual: a hung forge still holds reviewDispatchMu for up to this bound per
// dispatch; only runs that declare documents or select review conventions pay
// it.
const reviewDocumentResolveTimeout = 30 * time.Second

// documentResolveInput is one call into the shared resolve/attribute/render
// core (resolveDocumentSet).
type documentResolveInput struct {
	runRow *run.Run
	// stageID is the stage attribution entries are stamped with.
	stageID uuid.UUID
	// stage is the stage handed to the declaration seam. When nil and
	// loadStage is set, it is loaded by stageID — LAZILY, only when the seam
	// is configured (the seam is its only consumer).
	stage     *run.Stage
	loadStage bool
	// conventions are the review conventions selected for this review round
	// (E55.3 / #2244). Empty on every author-prompt path, so a convention can
	// never reach an author prompt.
	conventions []spec.SelectedReviewConvention
	// resolveTimeout bounds the seam call, the credential scope and every
	// Resolve; zero means the caller's context alone.
	resolveTimeout time.Duration
	// attribute gates the audit writes and NOTHING else.
	attribute bool
}

// resolvedDocumentSet is what resolveDocumentSet produced.
type resolvedDocumentSet struct {
	// injected are the seam-declared documents plus, when any run-admission
	// declaration was withheld, the WithheldNotice.
	injected []prompt.InjectedDocument
	// conventions are the RESOLVED review conventions, rendered — review
	// prompts only.
	conventions []prompt.ReviewConvention
	// rendered are the selections behind conventions, in the same order.
	rendered []spec.SelectedReviewConvention
}

// resolveDocumentSet is the shared core behind resolveDeclaredDocuments (both
// prompt endpoints) and resolveReviewDocuments (the in-process review builds).
// The contract documented on resolveDeclaredDocuments holds for every caller;
// this adds the review-only inputs.
//
// INERT CHECK FIRST. No declaration seam AND no selected convention returns
// the zero value before ANY read — in particular before the lazy reviewed-stage
// load (#2797 item 3), so an otherwise-inert review no longer fails on an
// unloadable stage row.
//
// A SELECTED CONVENTION WITH A NIL RESOLVER FAILS CLOSED, even with no
// declaration seam: the workflow intends to constrain the reviewer and the
// deployment cannot read the document — the same partial-configuration rule
// as the seam's.
//
// CONVENTIONS SHARE THE WHOLE PIPELINE. Their declarations (always
// BaseSourceRunAdmission) are appended after the seam's, go through the same
// base-source partition (no recorded admission commit -> withheld + notice +
// document_injection_degraded) and the same resolve-everything-before-any-
// attribution loop. A missing REQUIRED convention fails the whole set with
// reviewConventionMissingError — before any audit entry is written. A missing
// OPTIONAL convention is withheld with reason
// repodoc.WithheldReasonOptionalDocumentMissing, recorded by RecordWithheld
// BEFORE Attribute, and renders nothing. Resolved conventions are returned in
// conventions, never in injected.
func (s *Server) resolveDocumentSet(ctx context.Context, in documentResolveInput) (resolvedDocumentSet, error) {
	runRow := in.runRow
	seam := s.cfg.DocumentDeclarations
	if seam == nil && len(in.conventions) == 0 {
		return resolvedDocumentSet{}, nil // fully inert: nothing declares a document
	}
	if s.cfg.DocumentResolver == nil {
		if seam != nil {
			return resolvedDocumentSet{}, errors.New("document injection is misconfigured: DocumentDeclarations is configured but DocumentResolver is nil; " +
				"wire a resolver or remove the declaration seam")
		}
		return resolvedDocumentSet{}, errors.New("document injection is misconfigured: the workflow selects review conventions for this review but DocumentResolver is nil; " +
			"wire a resolver (a file-capable forge) or remove the review_conventions selection")
	}

	resolveCtx := ctx
	if in.resolveTimeout > 0 {
		var cancel context.CancelFunc
		resolveCtx, cancel = context.WithTimeout(ctx, in.resolveTimeout)
		defer cancel()
	}

	var (
		decls   []repodoc.Declaration
		baseRef string
	)
	if seam != nil {
		stage := in.stage
		if stage == nil && in.loadStage {
			loaded, err := s.cfg.RunRepo.GetStage(ctx, in.stageID)
			if err != nil {
				return resolvedDocumentSet{}, fmt.Errorf("load reviewed stage %s: %w", in.stageID, err)
			}
			if loaded == nil {
				return resolvedDocumentSet{}, fmt.Errorf("load reviewed stage %s: stage not found", in.stageID)
			}
			stage = loaded
		}
		var err error
		decls, baseRef, err = seam(resolveCtx, runRow, stage)
		if err != nil {
			return resolvedDocumentSet{}, fmt.Errorf("resolve document declarations: %w", err)
		}
	}
	// convOf maps a declaration's index to its convention's index (-1 for a
	// seam declaration).
	convOf := make([]int, len(decls), len(decls)+len(in.conventions))
	for i := range convOf {
		convOf[i] = -1
	}
	for ci, c := range in.conventions {
		decls = append(decls, conventionDeclaration(c))
		convOf = append(convOf, ci)
	}
	if len(decls) == 0 {
		return resolvedDocumentSet{}, nil
	}

	repo, err := parseRepoRef(runRow.Repo)
	if err != nil {
		return resolvedDocumentSet{}, err
	}
	var scope forge.CredentialScope
	if s.cfg.DocumentScope != nil {
		scope, err = s.cfg.DocumentScope(resolveCtx, repo)
		if err != nil {
			return resolvedDocumentSet{}, fmt.Errorf("resolve credential scope for %s: %w", repo, err)
		}
	}

	// PARTITION BY BASE SOURCE. A run-admission declaration on a run that
	// recorded no admission commit is WITHHELD here, before the resolve phase:
	// there is no ref it may honestly be read at (the seam's ref and the run's
	// branch are both mutable), so it is never handed to Resolve at all.
	// Declaration order is preserved within each partition.
	resolvable := make([]int, 0, len(decls))
	var withheld repodoc.Withheld
	for i, decl := range decls {
		if decl.Base == repodoc.BaseSourceRunAdmission && runRow.DocumentBaseCommit == nil {
			withheld.Declarations = append(withheld.Declarations, decl)
			continue
		}
		resolvable = append(resolvable, i)
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
	docDecl := make([]int, 0, len(resolvable))
	optionalMissing := repodoc.Withheld{Reason: repodoc.WithheldReasonOptionalDocumentMissing}
	for _, i := range resolvable {
		decl := decls[i]
		ref := baseRef
		if decl.Base == repodoc.BaseSourceRunAdmission {
			// Never the seam's ref: the admission commit, verbatim. A
			// malformed persisted value cannot pass the runs CHECK, and if
			// one ever arrived Resolve's run-admission guard refuses any
			// non-commit ref before a branch lookup or fetch.
			ref = *runRow.DocumentBaseCommit
		}
		doc, err := s.cfg.DocumentResolver.Resolve(resolveCtx, repodoc.Request{
			Repo:        repo,
			Scope:       scope,
			BaseRef:     ref,
			Declaration: decl,
		})
		if err != nil {
			if ci := convOf[i]; ci >= 0 && isMissingDocument(err) {
				if conv := in.conventions[ci]; conv.Required {
					return resolvedDocumentSet{}, reviewConventionMissingError(conv, ref, err)
				}
				optionalMissing.Declarations = append(optionalMissing.Declarations, decl)
				continue
			}
			return resolvedDocumentSet{}, err
		}
		docs = append(docs, *doc)
		docDecl = append(docDecl, i)
	}

	// Attribute the whole set, and return NOTHING on failure — the injection
	// and its audit entries ship together or not at all. repodoc.Attribute
	// orders its own appends so a failure cannot leave a successful-injection
	// claim behind (truncations first, injection claims last, all tagged with
	// one injection_set_id). Each withheld set is recorded FIRST and fails
	// closed the same way: a served prompt missing a declared document with no
	// audit trace of why is the un-attributed omission that entry prevents,
	// and writing it before any document_injected claim means its failure
	// leaves no injection claim behind. The appends run on the CALLER's
	// context, never the bounded resolve context.
	//
	// THE ONE PHASE THE PREVIEW SKIPS. Everything above this point is shared
	// verbatim between the served and preview wrappers, so the rendered bytes
	// and every refusal are computed by identical code; `attribute` gates this
	// block alone. See previewInjectedDocuments for why.
	if in.attribute {
		if err := repodoc.RecordWithheld(ctx, s.cfg.AuditRepo, runRow.ID, in.stageID, withheld); err != nil {
			return resolvedDocumentSet{}, err
		}
		if err := repodoc.RecordWithheld(ctx, s.cfg.AuditRepo, runRow.ID, in.stageID, optionalMissing); err != nil {
			return resolvedDocumentSet{}, err
		}
		if err := repodoc.Attribute(ctx, s.cfg.AuditRepo, runRow.ID, in.stageID, docs...); err != nil {
			return resolvedDocumentSet{}, err
		}
	}

	var out resolvedDocumentSet
	for j, doc := range docs {
		i := docDecl[j]
		rendered := repodoc.ToPromptDocument(doc, decls[i].Framing)
		if ci := convOf[i]; ci >= 0 {
			conv := in.conventions[ci]
			out.conventions = append(out.conventions, prompt.ReviewConvention{
				Name:        conv.Name,
				SeverityCap: conv.SeverityCap,
				Document:    rendered,
			})
			out.rendered = append(out.rendered, conv)
			continue
		}
		out.injected = append(out.injected, rendered)
	}
	if len(withheld.Declarations) > 0 {
		out.injected = append(out.injected, repodoc.WithheldNotice(withheld))
	}
	return out, nil
}

// resolveInjectedDocuments is the SERVED path's wrapper (GET
// /v0/stages/{id}/prompt): it resolves, ATTRIBUTES and renders. The
// in-process review builds attribute through resolveReviewDocuments (#2797).
// Behaviour is byte-identical to the pre-split single function; it and the
// review resolvers are the only callers that write document_injected /
// document_truncated / document_injection_degraded entries.
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

// reviewDocuments is what a review build site renders into its prompt and
// hands its verdict ingest (E55.3 / #2244).
type reviewDocuments struct {
	// Injected are the declared documents for Trigger.InjectedDocuments.
	Injected []prompt.InjectedDocument
	// Conventions are the resolved review conventions for
	// Trigger.ReviewConventions.
	Conventions []prompt.ReviewConvention
	// Round is the round's clamp caps and modified conventions files.
	Round reviewConventionRound
}

// resolveReviewDocuments is the REVIEW build sites' resolver (#2797, E55.3 /
// #2244): runPlanReviews (plan_review) and runImplementReviewsForTree
// (implement_review — the trace-time review, the fix-up re-review backstop and
// the decomposed parent's consolidated review). Those prompts are built
// in-process and never pass through the signed /prompt endpoint, so without
// this they carried no declared document at all.
//
// A REVIEW PROMPT CARRIES THE DOCUMENTS DECLARED FOR THE STAGE IT REVIEWS. The
// reviewer is constrained by what constrained the author, so the declaration
// seam is consulted with the REVIEWED stage (the plan stage for plan_review,
// the implement stage for implement_review).
//
// REVIEW CONVENTIONS ARE SELECTED HERE, PURELY. stageType locates the reviewed
// stage in the run's workflow-spec snapshot; paths are the site's paths (plan
// review: planGateScopePaths(plan); implement review: implementReviewPaths(diff)),
// combined with the run's admission change by reviewConventionChange so a
// label- or trigger-only applies_to selects identically at both sites. A
// selection error (unparseable snapshot, undeclared name, applies_to Match
// error) fails closed. Selected conventions are resolved at the run's
// admission commit through the shared core and returned in Conventions —
// never in Injected, and never on an author-prompt path. Round.Caps are the
// RENDERED conventions' caps; Round.ModifiedFiles (implement review only) are
// the declared conventions files among paths, decided from EVERY declared
// entry whether or not this round selected it.
//
// LAZY STAGE LOAD, ONE INERT SIGNAL (#2797 item 3). With no declaration seam
// and no selected convention nothing is read — not even the reviewed stage —
// and Injected/Conventions are empty (Round.ModifiedFiles, being pure, is still
// computed). The reviewed stage is loaded only when the seam is configured,
// because the seam is its only consumer; attribution keys on stageID.
//
// BOUNDED RESOLVE PHASE (#2797 item 1). The seam call, the credential scope and
// every forge read run under reviewDocumentResolveTimeout; the audit appends
// run on ctx.
//
// ATTRIBUTION RUNS PER REVIEW BUILD. One document_injected set is written per
// review round — shared by every reviewer of that round, which all read the
// same prompt — with stage_id = the reviewed stage. A resolved convention's
// entry names review_conventions.<name> as its declaration site.
//
// FAILS CLOSED, AND CALLERS MUST HONOUR IT. Every error — a nil run row, a
// selection error, a reviewed stage that cannot be loaded, a partial
// configuration, a declaration that cannot be resolved, a missing REQUIRED
// convention (review_convention_missing), an audit append that fails — means
// the declared set could not be resolved or attributed. The caller must then
// run NO reviewer (a document-less review prompt is exactly the unconstrained
// verdict injection exists to prevent), record a *_review_failed entry whose
// reason is reviewDocumentInjectionFailedReason(err), and, under gating
// authority, fail the stage category-B.
func (s *Server) resolveReviewDocuments(ctx context.Context, runRow *run.Run, stageID uuid.UUID, stageType spec.StageType, paths []string) (reviewDocuments, error) {
	if runRow == nil {
		return reviewDocuments{}, errors.New("reviewed run row is unavailable")
	}
	sel, err := selectReviewConventions(runRow, stageType, reviewConventionChange(runRow, paths))
	if err != nil {
		return reviewDocuments{}, err
	}
	var round reviewConventionRound
	if stageType == spec.StageTypeImplement {
		round.ModifiedFiles = conventionPathsTouched(paths, sel.DeclaredPaths)
	}
	res, err := s.resolveReviewDocumentSet(ctx, runRow, stageID, sel.Selected)
	if err != nil {
		return reviewDocuments{}, err
	}
	round.Caps = conventionCapsFor(res.rendered)
	return reviewDocuments{Injected: res.injected, Conventions: res.conventions, Round: round}, nil
}

// resolveReviewDocumentSet runs the shared core for a review build: lazy
// reviewed-stage load, bounded resolve phase, attribution on.
func (s *Server) resolveReviewDocumentSet(ctx context.Context, runRow *run.Run, stageID uuid.UUID, conventions []spec.SelectedReviewConvention) (resolvedDocumentSet, error) {
	return s.resolveDocumentSet(ctx, documentResolveInput{
		runRow:         runRow,
		stageID:        stageID,
		loadStage:      true,
		conventions:    conventions,
		resolveTimeout: reviewDocumentResolveTimeout,
		attribute:      true,
	})
}

// resolveReviewInjectedDocuments is the NO-CONVENTION review resolver: the
// declared documents of the reviewed stage only, through the same bounded,
// lazy-loading core as resolveReviewDocuments, with no convention selected.
// runSupplementalReinvokeReview uses it (a supplemental re-invoke renders no
// conventions section). Its fail-closed contract is resolveReviewDocuments'.
func (s *Server) resolveReviewInjectedDocuments(ctx context.Context, runRow *run.Run, stageID uuid.UUID) ([]prompt.InjectedDocument, error) {
	if runRow == nil {
		return nil, errors.New("reviewed run row is unavailable")
	}
	res, err := s.resolveReviewDocumentSet(ctx, runRow, stageID, nil)
	if err != nil {
		return nil, err
	}
	return res.injected, nil
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
