package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/kuhlman-labs/fishhawk/backend/internal/prompt"
	"github.com/kuhlman-labs/fishhawk/backend/internal/repodoc"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// The CHARTER INJECTION CONSUMER (E54.2 / #2234).
//
// This file is the SECOND consumer of the repo-document injection mechanism
// #2242 shipped inert (backend/internal/repodoc + Config.DocumentDeclarations
// / DocumentResolver / DocumentScope). It adds NO resolution, fetch, hashing,
// capping, delimiter-neutralization or audit code of its own — every one of
// those is repodoc's, reached through the untouched
// Server.resolveInjectedDocuments. What it supplies is exactly the three
// charter-specific things the mechanism deliberately does not know:
//
//   - the DECLARATION SITE — `charter.path` in the repo's work-management
//     conventions, reached through the process-wide conventionsLoader seam;
//   - the FRAMING — what a charter is, and the instruction to cite rubric
//     lines by id;
//   - the FAIL-CLOSED POLICY — a grooming propose stage never serves a prompt
//     without its charter.
//
// # Where the base ref comes from, and what is actually guaranteed
//
// The guarantee this consumer makes, stated as it is (approval condition H1)
// rather than as the issue's original wording:
//
//   - Resolution PINS TO A SPECIFIC COMMIT before any fetch, never a mutable
//     ref. repodoc.Resolver.pinCommit refuses any resolution that is not a
//     full 40-hex commit id, and refuses an empty ref outright.
//   - The resolved COMMIT and CONTENT HASH are recorded (the document_injected
//     audit entry), so a ranking is attributable to an exact charter revision.
//   - That commit is the DEFAULT-BRANCH HEAD AT PROMPT-SERVE TIME, which for a
//     non-diff workflow is the defined base: a grooming run produces no diff
//     and owns no branch, so there is no per-run base to resolve and the trunk
//     IS the base.
//
// The honest consequence: a charter amendment landing between run creation and
// prompt serve changes which revision constrains that run. For grooming that is
// the wanted behaviour — the groomer should rank against the CURRENT charter —
// and the recorded commit + content hash make which revision applied decidable
// after the fact.
//
// THIS SHORTCUT IS NOT REUSABLE BY E55's review-conventions consumer. That
// consumer attaches to CODE-CHANGE runs whose base may differ from the default
// branch, so it still needs the per-run base-ref source
// backend/internal/repodoc/README.md names (a persisted base_branch column, or
// a dispatch audit entry). Do not copy this adapter there.
//
// # Two fail-closed layers
//
// L1 (charterDeclarations) refuses to DECLARE the charter when the conventions
// cannot be loaded, no charter block is declared, its path is empty, or the
// base ref cannot be resolved. L2 (assertCharterInjected) runs in
// handleGetStagePrompt AFTER resolveInjectedDocuments and refuses to SERVE a
// grooming propose prompt whose injected set carries no document resolved from
// the declared charter path — whatever the seam configuration. L2 is what
// makes the guarantee unbypassable by a deployment that simply leaves the seam
// unwired, which is precisely the configuration in which L1 cannot run.
//
// L2 CHECKS THE CHARTER, NOT "SOME DOCUMENT" (approval condition H2). It
// re-resolves the declared charter path and requires an injected document AT
// THAT PATH, so a grooming prompt carrying an unrelated injected document and
// no charter is REFUSED — the case an any-document-present check would wave
// through in exactly the situation the control exists to catch.
//
// # Preview divergence (approval condition H5, tracked as #2804)
//
// handleGetStagePromptRender — the unsigned preview surface — does NOT inject
// documents at all (a pre-existing #2242 divergence), and L2 is deliberately
// NOT wired there: asserting on a handler that never injects would refuse
// every grooming preview. So a preview and a served prompt for the same stage
// differ in exactly the security-relevant block. Widening the preview to
// resolve documents is #2804's, not this slice's.

// charterDeclarationSite is the fixed provenance string repodoc echoes into
// every error message and audit payload for this consumer. The mechanism never
// parses it; it names the knob an operator has to turn.
const charterDeclarationSite = "charter.path in " + conventionsPathForMessage

// Prompt-serve refusal reasons. reasonCharterAbsent / reasonCharterPathEmpty /
// reasonConventionsUnavailable are REUSED from charter_gate.go so the
// admission gate and the prompt-serve consumer speak one vocabulary; the two
// below are prompt-serve-only states the admission gate has no equivalent of.
const (
	// reasonCharterNotInjected: a grooming propose prompt reached L2 with no
	// document resolved from the declared charter path. Either the seam is
	// unwired, or the injected set carries something else entirely.
	reasonCharterNotInjected = "charter_not_injected"
	// reasonCharterBaseRefUnresolved: no DocumentBaseRef seam is wired, the
	// forge call failed, or it returned an empty default branch. All three are
	// refusals — an empty ref is a mutable read.
	reasonCharterBaseRefUnresolved = "charter_base_ref_unresolved"
	// reasonGroomingSpecUnreadable: the run's cached workflow spec cannot be
	// re-parsed (or names no known workflow) AND it carries the grooming_report
	// artifact token, so whether this is a grooming propose stage is
	// undecidable. See stageRequiresCharter for why this is narrowed to
	// grooming-shaped specs only.
	reasonGroomingSpecUnreadable = "grooming_workflow_spec_unreadable"
)

// charterInjectionError is this consumer's refusal, carrying the machine-
// readable reason alongside the operator-actionable message. The reason is on
// the error (and echoed into the error string) so a caller, a test and an
// operator reading the HTTP details all identify the SAME branch rather than
// pattern-matching prose.
type charterInjectionError struct {
	Reason  string
	Message string
}

func (e *charterInjectionError) Error() string {
	return e.Message + " (reason: " + e.Reason + ")"
}

// charterRefusal builds a refusal with the shared actionable tail.
func charterRefusal(reason, detail string) error {
	return &charterInjectionError{Reason: reason, Message: detail}
}

// charterAwareInjectionErrorDetails is documentInjectionErrorDetails plus the
// charter consumer's `reason` key. Wrapping rather than editing
// documentInjectionErrorDetails keeps #2242's mechanism-side helper (and every
// non-charter injection failure's details payload) byte-identical: the extra
// key appears only when the error IS a charter refusal.
func charterAwareInjectionErrorDetails(err error) map[string]any {
	details := documentInjectionErrorDetails(err)
	var ce *charterInjectionError
	if errors.As(err, &ce) {
		details["reason"] = ce.Reason
	}
	return details
}

// charterFraming is the charter-specific wording wrapping the injected block.
// The mechanism supplies the fixed data-not-instructions clause, the
// delimiters and the Source metadata line; everything below is this consumer's
// contribution and nothing else distinguishes it from E55's conventions
// consumer.
func charterFraming() repodoc.Framing {
	return repodoc.Framing{
		Heading: "Product charter — the prioritization anchor for this grooming run",
		Preamble: "This repository declares a product charter: the human-authored document that fixes what this " +
			"product is for, what it explicitly will not do, and how backlog work is ranked. Agents read the " +
			"charter; agents never write it. It was resolved server-side at a pinned commit (named on the Source " +
			"line below), so nothing proposed in this run can rewrite the charter that constrains this run.",
		TrustNote: "The charter is the ranking authority here. Every ranking, drift flag and scoping proposal you " +
			"emit MUST cite the charter rubric line it rests on BY ID — the uppercase ids in the charter's rubric " +
			"tables (V*, R*, U*, S*), e.g. \"V1\" or \"R2\" — and a proposal citing no rubric id is unanchored and " +
			"must not be emitted. If an item you believe matters cites no line in the rubric, report that gap as a " +
			"charter defect rather than inventing a criterion.",
	}
}

// specMentionsGroomingReport reports whether the raw cached spec bytes contain
// the grooming_report artifact token at all.
//
// THIS IS THE H4 NARROWING, and it is a NARROWING of a refusal, never a
// widening. stageRequiresCharter has to decide "is this a grooming propose
// stage" before it can enforce the fail-closed rule, and an unparseable spec
// makes that undecidable. Refusing every undecidable spec would change
// prompt-serve behaviour for EVERY run — the neighbouring readers
// (resolveImplementConstraints, resolveImplementRequiredOutcomes) fail OPEN on
// the same input, and flipping a repo-wide failure mode is not this feature's
// to make.
//
// So the refusal is narrowed to specs that COULD be grooming specs. The
// artifact kind is a literal YAML scalar: a document that never contains the
// bytes "grooming_report" cannot yield a stage declaring
// spec.ArtifactGroomingReport under any parse, so a corrupt spec without the
// token is provably NOT a grooming spec and falls open exactly as its
// neighbours do. A corrupt spec WITH the token is undecidable-but-possibly-
// grooming and fails closed.
//
// The residual is stated rather than hidden: a corruption that also destroys
// the token (a truncation, say) falls open on what may have been a grooming
// run. The cached bytes were validated at run-create, so a parse failure is
// storage corruption rather than a normal or adversarial state, and H4's
// ruling is that keeping non-grooming prompts byte-identical outranks that
// residual.
func specMentionsGroomingReport(specBytes []byte) bool {
	return bytes.Contains(specBytes, []byte(spec.ArtifactGroomingReport))
}

// stageRequiresCharter reports whether this stage is a BACKLOG-GROOMING
// PROPOSE stage, the one stage kind that must never serve a prompt without its
// charter.
//
// The discriminator is STRUCTURAL and reuses the shipped predicate:
// a `plan`-typed run stage (ADR-067 §2's PROPOSE stage — and the only stage
// type on which spec validation permits the artifact) whose resolved workflow
// satisfies WorkflowRequiresCharter, i.e. declares the grooming_report
// artifact. Deliberately NOT the workflow's NAME (renaming would evade it) and
// deliberately NOT a `kind:` field (AC1 forbids a workflow-type
// discriminator).
//
// A nil WorkflowSpec is the LEGACY-ROW case and returns (false, nil): a
// grooming run always carries the spec it was minted from. An unparseable spec
// or an absent workflow returns an error ONLY when the raw bytes could be a
// grooming spec — see specMentionsGroomingReport for the H4 narrowing.
func stageRequiresCharter(runRow *run.Run, stage *run.Stage) (bool, error) {
	if runRow == nil || stage == nil {
		return false, nil
	}
	// grooming_report is valid only on a plan-typed (PROPOSE) stage, so no
	// other stage type can be a grooming propose stage. This keeps every
	// implement / review / deploy / acceptance prompt out of the conventions
	// loader entirely.
	if stage.Type != run.StageTypePlan {
		return false, nil
	}
	if len(runRow.WorkflowSpec) == 0 {
		return false, nil
	}
	parsed, err := spec.ParseBytes(runRow.WorkflowSpec)
	if err != nil {
		if !specMentionsGroomingReport(runRow.WorkflowSpec) {
			return false, nil // provably not a grooming spec — fail open like the neighbours
		}
		return false, charterRefusal(reasonGroomingSpecUnreadable, fmt.Sprintf(
			"the cached workflow spec for run %s declares the %s artifact but cannot be parsed, so whether this is a "+
				"backlog-grooming propose stage is undecidable and the prompt is refused rather than served without a "+
				"charter: %v", runRow.ID, spec.ArtifactGroomingReport, err))
	}
	wf, ok := parsed.Workflows[runRow.WorkflowID]
	if !ok {
		// The spec parsed but names no such workflow. Same narrowing: refuse
		// only when SOME workflow in the document is grooming-shaped, so the
		// undecidable case cannot be resolved as non-grooming.
		for _, candidate := range parsed.Workflows {
			if WorkflowRequiresCharter(candidate) {
				return false, charterRefusal(reasonGroomingSpecUnreadable, fmt.Sprintf(
					"the cached workflow spec for run %s does not declare workflow %q, and another workflow in it "+
						"produces a %s, so whether this is a backlog-grooming propose stage is undecidable and the "+
						"prompt is refused rather than served without a charter",
					runRow.ID, runRow.WorkflowID, spec.ArtifactGroomingReport))
			}
		}
		return false, nil
	}
	return WorkflowRequiresCharter(wf), nil
}

// charterDeclaredPath resolves the repo's declared charter path through the
// process-wide conventions loader, or refuses.
//
// The three refusal branches carry the SAME reason vocabulary the admission
// gate uses (charter_gate.go) with a PROMPT-SERVE-specific message: the
// admission gate's message ends "start the run again", which is the wrong
// instruction for a stage whose run already exists.
func charterDeclaredPath(ctx context.Context, repo, workflowID string) (string, error) {
	conv, err := conventionsLoader(ctx, repo)
	if err != nil {
		return "", charterRefusal(reasonConventionsUnavailable, charterServeMessage(repo, workflowID,
			"the work-management conventions could not be read, and an unreadable conventions file is refused "+
				"rather than assumed to declare a charter: "+err.Error()))
	}
	if conv.Charter == nil {
		return "", charterRefusal(reasonCharterAbsent, charterServeMessage(repo, workflowID,
			"no `charter:` block is declared"))
	}
	path := ""
	if conv.Charter != nil {
		path = strings.TrimSpace(conv.Charter.Path)
	}
	if path == "" {
		return "", charterRefusal(reasonCharterPathEmpty, charterServeMessage(repo, workflowID,
			"the declared `charter:` block carries an empty `path:`"))
	}
	return path, nil
}

// charterServeMessage renders the actionable prompt-serve refusal text.
func charterServeMessage(repo, workflowID, detail string) string {
	return "cannot serve the backlog-grooming prompt for workflow " + workflowID + " in " + repo + ": " + detail +
		". A grooming run ranks the backlog against the charter's rubric and there is no unanchored-grooming mode. " +
		"Declare a `charter:` block with its `path:` key in " + conventionsPathForMessage +
		" pointing at the checked-in charter document (conventionally .fishhawk/charter.md), then re-dispatch this stage."
}

// charterDeclarations is layer L1 and the Config.DocumentDeclarations
// implementation: it declares the charter for a backlog-grooming propose
// stage, and NOTHING for every other stage — which is what keeps every
// non-grooming prompt byte-identical (resolveInjectedDocuments short-circuits
// on an empty declaration set, and the conventions loader is never called).
//
// It adds no resolution, fetch, hash, cap or audit code: it returns a
// repodoc.Declaration and the base ref, and repodoc does the rest.
func (s *Server) charterDeclarations(ctx context.Context, runRow *run.Run, stage *run.Stage) ([]repodoc.Declaration, string, error) {
	required, err := stageRequiresCharter(runRow, stage)
	if err != nil {
		return nil, "", err
	}
	if !required {
		return nil, "", nil
	}

	path, err := charterDeclaredPath(ctx, runRow.Repo, runRow.WorkflowID)
	if err != nil {
		return nil, "", err
	}

	baseRef, err := s.charterBaseRef(ctx, runRow)
	if err != nil {
		return nil, "", err
	}

	return []repodoc.Declaration{{
		Path:            path,
		DeclarationSite: charterDeclarationSite,
		Framing:         charterFraming(),
	}}, baseRef, nil
}

// CharterDocumentDeclarations is the exported wiring entry point for
// Config.DocumentDeclarations.
//
// It exists because cmd/fishhawkd cannot name the unexported method (Go
// visibility) and because server.New COPIES its Config by value, so the seam
// must be installed BEFORE construction while the implementation needs the
// constructed Server. The wiring therefore closes over a *Server variable
// assigned by server.New and calls this method at request time. Approval
// condition H3.
func (s *Server) CharterDocumentDeclarations(ctx context.Context, runRow *run.Run, stage *run.Stage) ([]repodoc.Declaration, string, error) {
	return s.charterDeclarations(ctx, runRow, stage)
}

// charterBaseRef resolves the ref the charter is pinned against, refusing
// rather than defaulting. Both an unwired seam and a forge failure are
// refusals: repodoc reads an empty base ref as the repo's default branch — the
// mutable read pinning exists to prevent — so there is no safe fallback.
func (s *Server) charterBaseRef(ctx context.Context, runRow *run.Run) (string, error) {
	if s.cfg.DocumentBaseRef == nil {
		return "", charterRefusal(reasonCharterBaseRefUnresolved, fmt.Sprintf(
			"cannot resolve the base ref to pin the charter against for %s: no document base-ref resolver is "+
				"configured. The charter must be read at a pinned commit, and an empty ref would be read as the "+
				"repo's default branch — a mutable read. Wire the forge-backed document seam on this deployment.",
			runRow.Repo))
	}
	repo, err := parseRepoRef(runRow.Repo)
	if err != nil {
		return "", charterRefusal(reasonCharterBaseRefUnresolved, fmt.Sprintf(
			"cannot resolve the base ref to pin the charter against: %v", err))
	}
	baseRef, err := s.cfg.DocumentBaseRef(ctx, repo)
	if err != nil {
		return "", charterRefusal(reasonCharterBaseRefUnresolved, fmt.Sprintf(
			"cannot resolve the base ref to pin the charter against for %s: %v", runRow.Repo, err))
	}
	if strings.TrimSpace(baseRef) == "" {
		return "", charterRefusal(reasonCharterBaseRefUnresolved, fmt.Sprintf(
			"the document base-ref resolver returned an empty ref for %s, which a forge reads as the repo's "+
				"default branch — a mutable read. Refused rather than defaulted.", runRow.Repo))
	}
	return baseRef, nil
}

// assertCharterInjected is layer L2: the post-resolution check that a grooming
// propose prompt is actually anchored.
//
// It verifies CHARTER IDENTITY, not mere document presence (approval condition
// H2): it re-resolves the declared charter path and requires an injected
// document AT THAT PATH. An any-document-present check would pass in exactly
// the case this control exists to catch — a prompt carrying some other
// injected document and no charter.
//
// It runs independently of the declaration seam, so it still refuses on a
// deployment that never wired Config.DocumentDeclarations at all — the one
// configuration in which L1 cannot run.
// The receiver is deliberately UNUSED: L2 depends on no Server state at all,
// which is exactly why it still refuses on a deployment that wired nothing.
func (*Server) assertCharterInjected(ctx context.Context, runRow *run.Run, stage *run.Stage, injected []prompt.InjectedDocument) error {
	required, err := stageRequiresCharter(runRow, stage)
	if err != nil {
		return err
	}
	if !required {
		return nil
	}
	path, err := charterDeclaredPath(ctx, runRow.Repo, runRow.WorkflowID)
	if err != nil {
		return err
	}
	for _, doc := range injected {
		if doc.Path == path {
			return nil
		}
	}
	return charterRefusal(reasonCharterNotInjected, fmt.Sprintf(
		"refusing to serve the backlog-grooming prompt for workflow %s in %s: none of the %d injected documents was "+
			"resolved from the declared charter path %q (declared at %s). A grooming run ranks the backlog against "+
			"the charter's rubric and there is no unanchored-grooming mode; wire the repo-document injection seam "+
			"(document resolver, credential scope, base ref and declarations) on this deployment.",
		runRow.WorkflowID, runRow.Repo, len(injected), path, charterDeclarationSite))
}
