package spec

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Review conventions — `review_conventions` (ADR-068 / E55.2 / #2243).
//
// A workflow-v2 document may declare a top-level map of NAMED conventions, each
// naming a repo-relative document, and a plan or implement stage SELECTS them
// by name through `reviewers.conventions`. This file owns the typed shape, the
// semantic validation family Validate wires in, and the pure selection function
// the downstream consumers call (#2244 renders a selected convention into the
// review prompt and enforces severity_cap; #2797 wires the review-prompt
// sites). Nothing here reaches a reviewer: this change is grammar only.
//
// RULE ORDER IS A CONTRACT, mirrored by the CLI's raw-map port so the
// backend's FIRST error is the CLI list's first entry:
//
//  1. DECLARATION checks (validateReviewConventionDeclarations), over the
//     entry names in sorted order, BEFORE the workflow loop: the canonical
//     repo-relative path rule, then the applies_to change_kind refusal, then
//     the shared Predicate.Validate at the declaration site.
//  2. STAGE checks (validateStageReviewConventions), inside validateWorkflow's
//     reviewers block after the reviewers.authority check: the stage type
//     must be plan or implement, then every selected name must resolve to a
//     declared entry, then the stage must configure an agent reviewer.
//  3. REFERENCE check (validateReviewConventionsReferenced), after the
//     workflow loop: a declared entry no resolved stage selects is refused.
//
// Every rule reads the RESOLVED document: parse.go folds workflow-v2
// `defaults` / `extends` before Validate runs, so a selection inherited
// through defaults.reviewers counts as a selection on every inheriting stage
// and is subject to the stage-type rule there.
//
// The rejection texts below are single-line `const … = "…"` declarations so
// cli/internal/spec can carry byte-identical copies (the two Go modules cannot
// share a package); keep them single-line.

// ReviewConvention is one named entry of the top-level review_conventions map.
// The schema's $defs/review_convention is additionalProperties:false and this
// struct round-trips through ParseBytes' DisallowUnknownFields decode, so the
// two MUST stay in lockstep — and there is deliberately no inline `text`
// field: a convention names a file in the repository under review.
type ReviewConvention struct {
	// Path is the repo-relative, slash-separated document path.
	Path string `json:"path" yaml:"path"`
	// AppliesTo optionally narrows which changes the convention attaches to,
	// through the shared E53.1 predicate. nil attaches to every review of
	// every stage that selects it.
	AppliesTo *Predicate `json:"applies_to,omitempty" yaml:"applies_to,omitempty"`
	// SeverityCap optionally bounds the severity a reviewer may assign to a
	// concern resting on this convention; empty means UNCAPPED. Closed set:
	// ReviewConventionSeverityCapLow / ReviewConventionSeverityCapMedium.
	SeverityCap string `json:"severity_cap,omitempty" yaml:"severity_cap,omitempty"`
	// Required reports whether a missing document fails the review stage.
	// nil means the schema default, true — read it through IsRequired, never
	// directly, so absent and explicit-true cannot diverge.
	Required *bool `json:"required,omitempty" yaml:"required,omitempty"`
}

// IsRequired resolves Required against its schema default: absent (nil) is
// true, so a convention whose document is missing fails loudly unless the
// author explicitly declared `required: false`.
func (c ReviewConvention) IsRequired() bool {
	return c.Required == nil || *c.Required
}

// Review-convention severity caps: the CLOSED set the schema's
// $defs/review_convention.severity_cap enum declares, [low, medium].
//
// `high` is deliberately NOT a member. It is the TOP rung of the reviewer
// concern-severity ladder — planreview.ConcernSeverity, ordered
// SeverityHigh > SeverityMedium > SeverityLow in
// backend/internal/planreview/review.go — so a `high` cap would clamp nothing
// and is refused at the schema as a control that does nothing (the E53
// doctrine applied to every other declared-but-inert control). This is a
// PLAN-DERIVED decision recorded in #2243's approved plan, not one of the
// issue's acceptance criteria (AC4 asks only for an optional cap). It fails
// closed and is cheap to relax: admitting `high` later is additive, whereas
// removing a declarable value would be a breaking change.
// TestReviewConventionSeverityCapsMatchSchemaEnum holds these constants and
// the schema enum together.
const (
	ReviewConventionSeverityCapLow    = "low"
	ReviewConventionSeverityCapMedium = "medium"
)

// SelectedReviewConvention is one convention SelectReviewConventions attached
// to a stage's review of a change — the value #2244 renders and #2797 hands to
// the repo-document resolver.
type SelectedReviewConvention struct {
	// Name is the review_conventions map key.
	Name string
	// Path is the declared repo-relative document path.
	Path string
	// SeverityCap is the declared cap; "" means uncapped.
	SeverityCap string
	// Required is IsRequired() resolved: true unless declared false.
	Required bool
	// DeclarationSite is the free-form provenance repodoc.Declaration expects
	// (formatted by DeclarationSiteFmtReviewConvention), echoed into every
	// resolution error so an operator knows which knob produced it.
	DeclarationSite string
}

// DeclarationSiteFmtReviewConvention formats SelectedReviewConvention's
// DeclarationSite from the convention name.
const DeclarationSiteFmtReviewConvention = "review_conventions.%s in .fishhawk/workflows.yaml"

// PathFmtReviewConvention is the reported path for a declaration or reference rejection (convention name).
const PathFmtReviewConvention = "/review_conventions/%s"

// PathFmtStageReviewConventions is the reported path for a stage-type or no-agents rejection (workflow name, stage index).
const PathFmtStageReviewConventions = "/workflows/%s/stages/%d/reviewers/conventions"

// PathFmtStageReviewConventionItem is the reported path for an undeclared-name rejection (workflow name, stage index, selector index).
const PathFmtStageReviewConventionItem = "/workflows/%s/stages/%d/reviewers/conventions/%d"

// MsgFmtReviewConventionPathInvalid rejects a convention path that is not a canonical repo-relative path (name, path, reason).
const MsgFmtReviewConventionPathInvalid = "review_conventions.%s: path %q is not a canonical repo-relative path: %s; name the document relative to the repository root, slash-separated, with no empty, \".\" or \"..\" segment (e.g. docs/conventions/backend.md)"

// MsgReviewConventionPathEmpty is the MsgFmtReviewConventionPathInvalid reason for an empty path.
const MsgReviewConventionPathEmpty = "it is empty"

// MsgReviewConventionPathAbsolute is the reason for a path with a leading slash.
const MsgReviewConventionPathAbsolute = "it is absolute"

// MsgReviewConventionPathBackslash is the reason for a path containing a backslash.
const MsgReviewConventionPathBackslash = "it contains a backslash"

// MsgReviewConventionPathInvalidUTF8 is the reason for a path that is not valid UTF-8.
const MsgReviewConventionPathInvalidUTF8 = "it is not valid UTF-8"

// MsgFmtReviewConventionPathControlChar is the reason for a path carrying a control or line-separator character (the rune).
const MsgFmtReviewConventionPathControlChar = "it contains the control character %q"

// MsgReviewConventionPathEmptySegment is the reason for a path with an empty segment (a doubled or trailing slash).
const MsgReviewConventionPathEmptySegment = "it contains an empty segment"

// MsgFmtReviewConventionPathDotSegment is the reason for a path with a "." or ".." segment (the segment).
const MsgFmtReviewConventionPathDotSegment = "it contains a %q segment"

// MsgReviewConventionChangeKindUnsupported rejects `change_kind` inside a convention's applies_to (no producer emits a change kind — the applies_to / escalations precedent).
const MsgReviewConventionChangeKindUnsupported = "review_conventions does not accept the change_kind criterion in applies_to: nothing produces a change kind today, so the criterion can never be satisfied and this convention would attach to no review; narrow on paths, labels or trigger instead (the shared predicate keeps change_kind for its other consumers)"

// MsgFmtReviewConventionStageType rejects reviewers.conventions on a stage type other than plan / implement (stage id, stage type).
const MsgFmtReviewConventionStageType = "stage %q: reviewers.conventions is valid only on a plan or implement stage, not a %q stage: only those two stages run the agent-review loop a convention is handed to, so this selection would reach no reviewer; move it onto a plan or implement stage (a file-level defaults.reviewers block carrying conventions lands on every stage that inherits it — give this stage its own reviewers block)"

// MsgFmtReviewConventionUnknown rejects a selected name that resolves to no declared entry (stage id, name, name).
const MsgFmtReviewConventionUnknown = "stage %q: reviewers.conventions names %q, which matches no entry in the top-level review_conventions map; declare it there (review_conventions.%s: {path: ...}) or remove it from the selection — a convention is declared, never auto-discovered"

// MsgFmtReviewConventionNoAgents rejects a selection on a stage that configures no agent reviewers (stage id).
const MsgFmtReviewConventionNoAgents = "stage %q: reviewers.conventions selects review conventions but the stage configures no agent reviewers, so no reviewer would be handed them; declare at least one entry under reviewers.agents, or remove reviewers.conventions"

// MsgFmtReviewConventionUnreferenced rejects a declared entry that no resolved stage selects (name).
const MsgFmtReviewConventionUnreferenced = "review_conventions.%s is declared but no stage selects it through reviewers.conventions, so it would reach no reviewer — it is refused rather than silently accepted as a control that does something; select it on a plan or implement stage, or remove the declaration"

// ErrReviewConventionStageType is returned (wrapped) by SelectReviewConventions
// for a stage selecting conventions that is not a plan or implement stage.
var ErrReviewConventionStageType = errors.New("review conventions are selectable only on a plan or implement stage")

// ErrReviewConventionUndeclared is returned (wrapped) by
// SelectReviewConventions for a selected name with no declared entry.
var ErrReviewConventionUndeclared = errors.New("review convention is not declared in review_conventions")

// reviewConventionPathReason returns why p is not a canonical repo-relative
// path, or "" when it is. The rule SET is repodoc.validatePath's (the
// resolver that fetches the document, which stays the AUTHORITATIVE check at
// resolution time); spec cannot import repodoc without pulling forge / audit /
// prompt into this low-level shared library, so it is duplicated here and a
// drift between the two is fail-closed at stage time, never fail-open.
//
// repodoc's trailing path.Clean comparison is OMITTED because it is
// unreachable after the raw segment scan: path.Clean rewrites a relative path
// only by collapsing an empty, "." or ".." element (a trailing slash is an
// empty last element), and each of those is already refused below by name. The
// rejected set is therefore identical — every spelling path.Clean would
// rewrite is refused — which TestReviewConventionPath_RejectsEveryNonCanonicalSpelling
// pins rather than leaving an untestable branch standing.
func reviewConventionPathReason(p string) string {
	if p == "" {
		return MsgReviewConventionPathEmpty
	}
	if strings.HasPrefix(p, "/") {
		return MsgReviewConventionPathAbsolute
	}
	if strings.Contains(p, `\`) {
		return MsgReviewConventionPathBackslash
	}
	if !utf8.ValidString(p) {
		return MsgReviewConventionPathInvalidUTF8
	}
	for _, r := range p {
		if unicode.IsControl(r) || r == '\u2028' || r == '\u2029' {
			return fmt.Sprintf(MsgFmtReviewConventionPathControlChar, r)
		}
	}
	for _, seg := range strings.Split(p, "/") {
		switch seg {
		case "":
			return MsgReviewConventionPathEmptySegment
		case ".", "..":
			return fmt.Sprintf(MsgFmtReviewConventionPathDotSegment, seg)
		}
	}
	return ""
}

// sortedReviewConventionNames returns the declared names in sorted order, so
// both the backend's first error and the CLI's list are deterministic.
func sortedReviewConventionNames(s *Spec) []string {
	names := make([]string, 0, len(s.ReviewConventions))
	for name := range s.ReviewConventions {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// validateReviewConventionDeclarations runs the per-entry DECLARATION checks
// (rule-order rung 1): path, then the change_kind refusal, then the shared
// Predicate.Validate — change_kind first because that criterion is wrong
// regardless of what else the predicate says and its message names a fix the
// generic predicate error cannot (the validateAppliesTo precedent).
func validateReviewConventionDeclarations(s *Spec) error {
	for _, name := range sortedReviewConventionNames(s) {
		conv := s.ReviewConventions[name]
		ptr := fmt.Sprintf(PathFmtReviewConvention, name)
		if reason := reviewConventionPathReason(conv.Path); reason != "" {
			return &ValidationError{
				Path:    ptr + "/path",
				Message: fmt.Sprintf(MsgFmtReviewConventionPathInvalid, name, conv.Path, reason),
			}
		}
		if conv.AppliesTo == nil {
			continue
		}
		if len(conv.AppliesTo.ChangeKinds) > 0 {
			return &ValidationError{
				Path:    ptr + "/applies_to/change_kind",
				Message: MsgReviewConventionChangeKindUnsupported,
			}
		}
		if err := conv.AppliesTo.Validate(ptr + "/applies_to"); err != nil {
			return &ValidationError{Path: ptr + "/applies_to", Message: err.Error()}
		}
	}
	return nil
}

// reviewConventionStageType reports whether a stage type may select
// conventions: only plan and implement run an agent-review loop.
func reviewConventionStageType(t StageType) bool {
	return t == StageTypePlan || t == StageTypeImplement
}

// validateStageReviewConventions runs the STAGE checks (rule-order rung 2) for
// stage index i of workflow wf: stage type, then each name resolves (path
// .../conventions/<j>, mirroring inputs[].from_stage), then the stage has an
// agent reviewer (mirroring the reviewers.authority no-agents rule).
func validateStageReviewConventions(s *Spec, wf string, i int, st *Stage) error {
	if st.Reviewers == nil || len(st.Reviewers.Conventions) == 0 {
		return nil
	}
	if !reviewConventionStageType(st.Type) {
		return &ValidationError{
			Path:    fmt.Sprintf(PathFmtStageReviewConventions, wf, i),
			Message: fmt.Sprintf(MsgFmtReviewConventionStageType, st.ID, st.Type),
		}
	}
	for j, name := range st.Reviewers.Conventions {
		if _, ok := s.ReviewConventions[name]; !ok {
			return &ValidationError{
				Path:    fmt.Sprintf(PathFmtStageReviewConventionItem, wf, i, j),
				Message: fmt.Sprintf(MsgFmtReviewConventionUnknown, st.ID, name, name),
			}
		}
	}
	if st.Reviewers.AgentCount() == 0 {
		return &ValidationError{
			Path:    fmt.Sprintf(PathFmtStageReviewConventions, wf, i),
			Message: fmt.Sprintf(MsgFmtReviewConventionNoAgents, st.ID),
		}
	}
	return nil
}

// validateReviewConventionsReferenced runs the REFERENCE check (rule-order
// rung 3): every declared entry must be selected by at least one stage of the
// resolved document. Only workflows/*/stages/*/reviewers/conventions count —
// the backend's resolver has already deleted `defaults`, so a selection made
// only inside a defaults.reviewers block no stage inherits selects nothing.
func validateReviewConventionsReferenced(s *Spec) error {
	if len(s.ReviewConventions) == 0 {
		return nil
	}
	selected := make(map[string]bool)
	for _, wf := range s.Workflows {
		for _, st := range wf.Stages {
			if st.Reviewers == nil {
				continue
			}
			for _, name := range st.Reviewers.Conventions {
				selected[name] = true
			}
		}
	}
	for _, name := range sortedReviewConventionNames(s) {
		if !selected[name] {
			return &ValidationError{
				Path:    fmt.Sprintf(PathFmtReviewConvention, name),
				Message: fmt.Sprintf(MsgFmtReviewConventionUnreferenced, name),
			}
		}
	}
	return nil
}

// SelectReviewConventions returns the review conventions stage st selects
// that attach to change c, in the order the stage's reviewers.conventions
// lists them. It is the pure consumer contract #2244 (rendering) and #2797
// (review-prompt wiring) call; it has no production caller in this change.
//
// The Change each consumer passes: the PLAN review passes the plan's
// scope.files as Paths plus the run's issue labels and trigger; the IMPLEMENT
// review passes the diff's changed paths (plus the same labels and trigger).
// A convention with no applies_to attaches unconditionally; otherwise the
// shared Predicate.Match decides, and a Match error is RETURNED, never
// swallowed as a non-match (a governance input fails closed).
//
// It fails closed for a hand-built Spec that skipped Validate: a stage type
// other than plan / implement returns ErrReviewConventionStageType, and a
// name with no declared entry returns ErrReviewConventionUndeclared (both
// wrapped). A nil stage, a nil reviewers block, or an empty selection returns
// (nil, nil).
//
// Required is resolved through IsRequired (absent = true). A required
// convention whose document is missing at the reviewed base is NOT detected
// here: that is repodoc's ErrMissingDocument at resolution, and failing the
// review stage on it is #2244's obligation, wired by #2797.
func (s *Spec) SelectReviewConventions(st *Stage, c Change) ([]SelectedReviewConvention, error) {
	if st == nil || st.Reviewers == nil || len(st.Reviewers.Conventions) == 0 {
		return nil, nil
	}
	if !reviewConventionStageType(st.Type) {
		return nil, fmt.Errorf("stage %q (type %q): %w", st.ID, st.Type, ErrReviewConventionStageType)
	}
	var declared map[string]ReviewConvention
	if s != nil {
		declared = s.ReviewConventions
	}
	var out []SelectedReviewConvention
	for _, name := range st.Reviewers.Conventions {
		conv, ok := declared[name]
		if !ok {
			return nil, fmt.Errorf("stage %q selects %q: %w", st.ID, name, ErrReviewConventionUndeclared)
		}
		if conv.AppliesTo != nil {
			matched, err := conv.AppliesTo.Match(c)
			if err != nil {
				return nil, fmt.Errorf("review_conventions.%s: applies_to: %w", name, err)
			}
			if !matched {
				continue
			}
		}
		out = append(out, SelectedReviewConvention{
			Name:            name,
			Path:            conv.Path,
			SeverityCap:     conv.SeverityCap,
			Required:        conv.IsRequired(),
			DeclarationSite: fmt.Sprintf(DeclarationSiteFmtReviewConvention, name),
		})
	}
	return out, nil
}
