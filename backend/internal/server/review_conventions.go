package server

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/policy"
	"github.com/kuhlman-labs/fishhawk/backend/internal/repodoc"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// Review-convention SELECTION and its pure helpers (E55.3 / #2244). The
// resolution itself — fetching each selected convention at the run's admission
// commit, attributing it, withholding it — lives in document_injection.go's
// shared core (resolveReviewDocuments), so a convention rides the SAME
// partition / resolve-before-attribute ordering as every other declared
// document. Everything in this file is pure: no I/O, no audit write.

// reviewConventionMissingPrefix leads the error a REQUIRED review convention
// whose document is absent at the run's admission commit produces. The review
// build site wraps it in reviewDocumentInjectionFailedReason, so the operator
// reads "document_injection_failed: review_convention_missing: ..." on the
// *_review_failed entry.
const reviewConventionMissingPrefix = "review_convention_missing"

// reviewConventionHeadingPrefix is the repodoc.Framing heading a convention's
// declaration carries. prompt.writeReviewConventions renders its OWN
// per-convention heading and never this one; it is set so the Declaration is
// well-formed and so a convention is recognisable in a repodoc error.
const reviewConventionHeadingPrefix = "Review convention "

// reviewConventionChange is the spec.Change a review site matches each
// convention's applies_to against: the run's ADMISSION change (trigger form +
// issue labels — runAdmissionChange, the SAME change the plan gate uses) with
// Paths set to the site's paths. Plan review passes planGateScopePaths(plan);
// implement review passes implementReviewPaths(diff). Both sites derive their
// label/trigger criteria from the one admission change, so a convention whose
// applies_to names only a label or a trigger form selects at BOTH sites.
func reviewConventionChange(runRow *run.Run, paths []string) spec.Change {
	if runRow == nil {
		return spec.Change{Paths: paths}
	}
	c := runAdmissionChange(runRow)
	if len(paths) > 0 {
		c.Paths = paths
	}
	return c
}

// implementReviewPaths returns the implement-review site paths of diff: every
// ChangedFile's Path plus, for a rename/copy row, its OldPath — so a convention
// (or a declared conventions file) matched by a file's PRE-move name is not
// evaded by moving the file. Slash-normalized, de-duplicated, sorted.
func implementReviewPaths(diff policy.Diff) []string {
	seen := make(map[string]struct{}, len(diff.ChangedFiles))
	var out []string
	add := func(p string) {
		if p == "" {
			return
		}
		norm := filepath.ToSlash(p)
		if _, dup := seen[norm]; dup {
			return
		}
		seen[norm] = struct{}{}
		out = append(out, norm)
	}
	for _, f := range diff.ChangedFiles {
		add(f.Path)
		add(f.OldPath)
	}
	sort.Strings(out)
	return out
}

// reviewConventionSelection is what selectReviewConventions read from the
// run's workflow-spec snapshot.
type reviewConventionSelection struct {
	// Selected are the conventions the reviewed stage selects for this change,
	// in the stage's reviewers.conventions order.
	Selected []spec.SelectedReviewConvention
	// DeclaredPaths is the path of EVERY review_conventions entry, selected
	// or not (declaredConventionPaths).
	DeclaredPaths []string
}

// selectReviewConventions reads the run's workflow-spec SNAPSHOT (the bytes
// admission parsed), locates the first stage of stageType in the run's
// workflow exactly as resolveStageReviewers does, and returns the conventions
// that stage selects for change via spec.SelectReviewConventions.
//
// "Nothing declared" is (zero, nil): no snapshot, a workflow absent from the
// snapshot, or no stage of the type. Everything that means a declaration MAY
// exist but could not be read or matched FAILS CLOSED with an error — an
// unparseable snapshot, a selection naming an undeclared convention, a stage
// type that may not select conventions, or an applies_to Match error. A
// governance input is never read as "no convention".
func selectReviewConventions(runRow *run.Run, stageType spec.StageType, change spec.Change) (reviewConventionSelection, error) {
	if runRow == nil || runRow.WorkflowSpec == nil {
		return reviewConventionSelection{}, nil
	}
	parsed, err := spec.ParseBytes(runRow.WorkflowSpec)
	if err != nil {
		return reviewConventionSelection{}, fmt.Errorf("select review conventions: parse the run's workflow spec: %w", err)
	}
	sel := reviewConventionSelection{DeclaredPaths: declaredConventionPaths(parsed)}
	wf, ok := parsed.Workflows[runRow.WorkflowID]
	if !ok {
		return sel, nil
	}
	for i := range wf.Stages {
		st := &wf.Stages[i]
		if st.Type != stageType {
			continue
		}
		selected, err := parsed.SelectReviewConventions(st, change)
		if err != nil {
			return reviewConventionSelection{}, fmt.Errorf("select review conventions for stage %q: %w", st.ID, err)
		}
		sel.Selected = selected
		return sel, nil
	}
	return sel, nil
}

// conventionDeclaration maps a selected convention onto the repodoc
// Declaration the shared resolution core resolves. Base is ALWAYS
// BaseSourceRunAdmission: a convention is read only at the commit recorded at
// run admission (never the run's own agent-writable branch, never a newer
// default-branch head), and is WITHHELD on a run that recorded none.
func conventionDeclaration(sel spec.SelectedReviewConvention) repodoc.Declaration {
	return repodoc.Declaration{
		Path:            sel.Path,
		DeclarationSite: sel.DeclarationSite,
		Framing:         repodoc.Framing{Heading: reviewConventionHeadingPrefix + sel.Name},
		Base:            repodoc.BaseSourceRunAdmission,
	}
}

// reviewConventionMissingError is the error a REQUIRED convention whose
// document is absent at the run's admission commit yields. It names the
// convention, its path, its declaration site and the commit, and WRAPS err (a
// *repodoc.ResolveError carrying repodoc.ErrMissingDocument) so
// documentInjectionErrorDetails and errors.Is still see through it.
func reviewConventionMissingError(sel spec.SelectedReviewConvention, commit string, err error) error {
	return fmt.Errorf("%s: required review convention %q (path %s, declared at %s) is missing at admission commit %s: %w",
		reviewConventionMissingPrefix, sel.Name, sel.Path, sel.DeclarationSite, commit, err)
}

// isMissingDocument reports whether a Resolve error means the declared
// document is absent at the pinned commit.
func isMissingDocument(err error) bool {
	return errors.Is(err, repodoc.ErrMissingDocument)
}

// declaredConventionPaths returns the path of EVERY review_conventions entry in
// s — selected or not, matched or not — slash-normalized, de-duplicated and
// sorted. A diff touching any of them changes the criteria a LATER review
// applies, whether or not this review selected that convention.
func declaredConventionPaths(s *spec.Spec) []string {
	if s == nil || len(s.ReviewConventions) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(s.ReviewConventions))
	var out []string
	for _, c := range s.ReviewConventions {
		if c.Path == "" {
			continue
		}
		norm := filepath.ToSlash(c.Path)
		if _, dup := seen[norm]; dup {
			continue
		}
		seen[norm] = struct{}{}
		out = append(out, norm)
	}
	sort.Strings(out)
	return out
}

// conventionPathsTouched returns the members of declared that appear in paths,
// in declared (sorted) order.
func conventionPathsTouched(paths, declared []string) []string {
	if len(paths) == 0 || len(declared) == 0 {
		return nil
	}
	in := make(map[string]struct{}, len(paths))
	for _, p := range paths {
		in[filepath.ToSlash(p)] = struct{}{}
	}
	var out []string
	for _, d := range declared {
		if _, ok := in[d]; ok {
			out = append(out, d)
		}
	}
	return out
}

// modifiedConventionFiles returns the declared conventions files diff changes,
// matched on each ChangedFile's Path AND its rename/copy OldPath
// (implementReviewPaths), so moving a conventions file away is detected too.
func modifiedConventionFiles(diff policy.Diff, declared []string) []string {
	return conventionPathsTouched(implementReviewPaths(diff), declared)
}

// conventionCapsFor builds the planreview.ConventionCaps of the conventions
// RENDERED into a round's prompt: name -> declared severity_cap ("" =
// uncapped). Only rendered conventions belong here (a withheld or
// optional-missing one was never shown to the reviewer). Empty input yields
// nil — "zero conventions rendered", which ClampConventionSeverities treats as
// the most restrictive case (every convention-category concern clamps to low).
func conventionCapsFor(rendered []spec.SelectedReviewConvention) planreview.ConventionCaps {
	if len(rendered) == 0 {
		return nil
	}
	caps := make(planreview.ConventionCaps, len(rendered))
	for _, c := range rendered {
		caps[c.Name] = planreview.ConcernSeverity(c.SeverityCap)
	}
	return caps
}

// reviewConventionRound is the per-round review-convention state a review
// build site hands its verdict-ingest loop.
//
// The ZERO VALUE is the no-conventions round: Caps nil means zero conventions
// were rendered, so the ingest clamp touches ONLY convention-category concerns
// a reviewer emitted anyway (clamped to low, approval condition 2 of #2244) and
// leaves every standard category alone; ModifiedFiles empty means no synthesis.
type reviewConventionRound struct {
	// Caps are the RENDERED conventions' severity caps (conventionCapsFor).
	Caps planreview.ConventionCaps
	// ModifiedFiles are the declared conventions files the reviewed diff
	// changes (implement review only). Non-empty obliges ONE
	// conventions_file_modified concern for the round.
	ModifiedFiles []string
}

// isConventionsFileModifiedConcern reports whether c is a
// conventions_file_modified concern, matched on the same normalized category
// form planreview's clamp uses (trimmed, case-folded).
func isConventionsFileModifiedConcern(c planreview.Concern) bool {
	return strings.ToLower(strings.TrimSpace(c.Category)) == planreview.ConventionsFileModifiedConcernCategory
}

// conventionsFileModifiedCarrier decides, ONCE per round and only after every
// reviewer of the round has returned, which verdict carries the server's
// synthesized conventions_file_modified concern. It returns the index of the
// FIRST non-nil verdict when NO verdict of the round already carries a
// conventions_file_modified concern and the stage has not alreadyRaised one
// (an open, waived or deferred row from an earlier round); otherwise -1 —
// including when every verdict is nil (every reviewer failed: nothing to carry
// it, and the next round re-evaluates).
func conventionsFileModifiedCarrier(verdicts []*planreview.ReviewVerdict, alreadyRaised bool) int {
	if alreadyRaised {
		return -1
	}
	first := -1
	for i, v := range verdicts {
		if v == nil {
			continue
		}
		if first < 0 {
			first = i
		}
		for _, c := range v.Concerns {
			if isConventionsFileModifiedConcern(c) {
				return -1
			}
		}
	}
	return first
}
