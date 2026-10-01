package spec

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Review conventions — `review_conventions` (ADR-068 / E55.2 / #2243).
//
// The CLI half of the review_conventions validation family: a raw-map port of
// backend/internal/spec/review_conventions.go, so `fishhawk validate` and
// `fishhawk doctor` refuse locally exactly what the backend refuses at run
// creation. The two Go modules cannot share a package, so the rules are
// re-implemented here over the yaml.v3 tree (this package carries no typed
// decode) and the rejection texts below are BYTE-IDENTICAL copies of the
// backend's constants. Two tests hold the pair together:
// TestReviewConventionsMessageParity reads both source files over the repo
// root and requires every const declaration verbatim, and the shared corpus
// docs/spec/review-conventions-fixtures.json (mirrored into testdata/ by
// scripts/sync-schemas) is run through ValidateBytes here and ParseBytes there,
// each row asserting the same path and message on both sides.
//
// RULE ORDER mirrors the backend's, so the CLI's error list leads with the
// entry the backend returns first:
//
//  1. checkReviewConventionDeclarations, BEFORE the workflow loop, over the
//     entry names in sorted order: the canonical repo-relative path rule, then
//     the applies_to change_kind refusal, then the shared Predicate.Validate
//     at the declaration site (this package's byte-identical predicate.go).
//  2. checkStageReviewConventions, inside the sorted stage loop after
//     checkReviewerAuthority: the stage type must be plan or implement, then
//     every selected name must resolve to a declared entry, then the stage
//     must configure an agent reviewer.
//  3. checkReviewConventionsReferenced, AFTER the workflow loop: a declared
//     entry no stage selects is refused.
//
// Like every other sweep in this package it COLLECTS rather than returning the
// first error, but each rung reports at most ONE entry per convention (rung 1),
// per stage (rung 2) or per unreferenced name (rung 3) — the backend's
// first-error-per-site shape — so a document violating exactly one rule yields
// exactly one entry, which is the corpus's parity claim.
//
// THE REUSE ASYMMETRY rung 3 must respect: the backend's resolver DELETES the
// root and workflow `defaults` blocks after folding them into the stages, while
// this package's resolveV2Reuse KEEPS them. Rung 3 therefore counts selections
// on workflows/*/stages/*/reviewers/conventions ONLY, never inside a
// `defaults` block — otherwise a convention selected only by a
// defaults.reviewers block that every stage overrides would read as selected
// here and unreferenced there. The corpus row
// selected-only-in-unused-defaults pins that agreement.
//
// Shape mismatches are skipped, as everywhere in this package: the sweep runs
// AFTER schema validation, so a node that is not the shape the schema requires
// (a non-string path, a non-map entry) was already rejected upstream.
//
// What stays backend-only is SelectReviewConventions — the pure selection
// function the review-prompt consumers (#2244 rendering, #2797 wiring) call;
// the CLI validates the grammar and selects nothing.

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

// reviewConventionPathReason returns why p is not a canonical repo-relative
// path, or "" when it is — the backend's reviewConventionPathReason, rule for
// rule and in the same order, so the reported reason is the backend's. (The
// backend's copy documents why repodoc's trailing path.Clean comparison is
// omitted: every spelling path.Clean would rewrite is already refused by name
// below.) The empty and invalid-UTF-8 reasons are unreachable through
// ValidateBytes — the schema's minLength refuses "" and YAML cannot carry an
// invalid UTF-8 scalar — and are kept so the function stays the backend's.
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
		if unicode.IsControl(r) || r == ' ' || r == ' ' {
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

// checkReviewConventionDeclarations runs rung 1 over every declared entry in
// sorted name order, reporting at most one entry per convention: the path
// rule, then the change_kind refusal, then the shared Predicate.Validate.
func checkReviewConventionDeclarations(root map[string]any, errs *[]ValidationErrorEntry) {
	declared, ok := root["review_conventions"].(map[string]any)
	if !ok {
		return
	}
	for _, name := range sortedKeys(declared) {
		entry, ok := declared[name].(map[string]any)
		if !ok {
			continue
		}
		ptr := fmt.Sprintf(PathFmtReviewConvention, name)
		if p, ok := entry["path"].(string); ok {
			if reason := reviewConventionPathReason(p); reason != "" {
				*errs = append(*errs, ValidationErrorEntry{
					Path:    ptr + "/path",
					Message: fmt.Sprintf(MsgFmtReviewConventionPathInvalid, name, p, reason),
				})
				continue
			}
		}
		appliesTo, ok := entry["applies_to"].(map[string]any)
		if !ok {
			continue
		}
		pred := predicateFromRaw(appliesTo)
		if len(pred.ChangeKinds) > 0 {
			*errs = append(*errs, ValidationErrorEntry{
				Path:    ptr + "/applies_to/change_kind",
				Message: MsgReviewConventionChangeKindUnsupported,
			})
			continue
		}
		if err := pred.Validate(ptr + "/applies_to"); err != nil {
			*errs = append(*errs, ValidationErrorEntry{Path: ptr + "/applies_to", Message: err.Error()})
		}
	}
}

// reviewConventionStageType reports whether a stage type may select
// conventions: only plan and implement run an agent-review loop.
func reviewConventionStageType(t string) bool {
	return t == "plan" || t == "implement"
}

// checkStageReviewConventions runs rung 2 for stage index i of workflow
// wfName, reporting at most one entry for the stage: stage type, then the
// FIRST selected name with no declared entry, then the agent-reviewer rule.
// declared is the root review_conventions map (nil when absent, so every
// selected name is undeclared). The agent-reviewer rule reads reviewers.agents
// only: the backend's AgentCount also counts the legacy integer
// reviewers.agent, which workflow-v2 removed (validateV2RemovedForms refuses
// it) and review_conventions exists only at v2.
func checkStageReviewConventions(stage map[string]any, wfName string, i int, declared map[string]any, errs *[]ValidationErrorEntry) {
	reviewers, ok := stage["reviewers"].(map[string]any)
	if !ok {
		return
	}
	names := rawStringList(reviewers["conventions"])
	if len(names) == 0 {
		return
	}
	stageID, _ := stage["id"].(string)
	stageType, _ := stage["type"].(string)
	if !reviewConventionStageType(stageType) {
		*errs = append(*errs, ValidationErrorEntry{
			Path:    fmt.Sprintf(PathFmtStageReviewConventions, wfName, i),
			Message: fmt.Sprintf(MsgFmtReviewConventionStageType, stageID, stageType),
		})
		return
	}
	for j, name := range names {
		if _, ok := declared[name]; !ok {
			*errs = append(*errs, ValidationErrorEntry{
				Path:    fmt.Sprintf(PathFmtStageReviewConventionItem, wfName, i, j),
				Message: fmt.Sprintf(MsgFmtReviewConventionUnknown, stageID, name, name),
			})
			return
		}
	}
	if agents, ok := reviewers["agents"].([]any); !ok || len(agents) == 0 {
		*errs = append(*errs, ValidationErrorEntry{
			Path:    fmt.Sprintf(PathFmtStageReviewConventions, wfName, i),
			Message: fmt.Sprintf(MsgFmtReviewConventionNoAgents, stageID),
		})
	}
}

// checkReviewConventionsReferenced runs rung 3: every declared entry must be
// selected by some stage. Selections are read from
// workflows/*/stages/*/reviewers/conventions ONLY — never a `defaults` block,
// which this package's resolver keeps and the backend's deletes (see the
// REUSE ASYMMETRY note above). A selection inherited from a defaults block has
// already been folded onto the inheriting stage, so it is counted there.
func checkReviewConventionsReferenced(root map[string]any, errs *[]ValidationErrorEntry) {
	declared, ok := root["review_conventions"].(map[string]any)
	if !ok || len(declared) == 0 {
		return
	}
	selected := make(map[string]bool)
	workflows, _ := root["workflows"].(map[string]any)
	for _, wfRaw := range workflows {
		wf, ok := wfRaw.(map[string]any)
		if !ok {
			continue
		}
		stages, _ := wf["stages"].([]any)
		for _, stRaw := range stages {
			st, ok := stRaw.(map[string]any)
			if !ok {
				continue
			}
			reviewers, ok := st["reviewers"].(map[string]any)
			if !ok {
				continue
			}
			for _, name := range rawStringList(reviewers["conventions"]) {
				selected[name] = true
			}
		}
	}
	for _, name := range sortedKeys(declared) {
		if !selected[name] {
			*errs = append(*errs, ValidationErrorEntry{
				Path:    fmt.Sprintf(PathFmtReviewConvention, name),
				Message: fmt.Sprintf(MsgFmtReviewConventionUnreferenced, name),
			})
		}
	}
}
