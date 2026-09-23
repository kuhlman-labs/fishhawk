package server

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/policy"
)

// The forbidden-criterion rule of the plan-gate surface sweep (#3620).
//
// Every preset's implement stage carries `forbidden_paths` for good reason —
// an implement agent must not rewrite the governance document that constrains
// it, so `.fishhawk/**`, `.github/workflows/**`, `.gitlab-ci.yml`, `LICENSE`
// and `NOTICE` are off limits. An ACCEPTANCE CRITERION demanding a change to
// one of those paths is therefore unsatisfiable from inside the loop: the
// constraint is enforced against the REAL diff, so the implement agent cannot
// touch the file and every review round re-raises the same gap (run f199dcf1:
// flagged six times across three rounds, unfixable every time). The defect is
// undetectable at authoring time and rediscovered by paid reviewers.
//
// Like the #3410 audit-category rule this is PROSE-triggered, not
// path-triggered: it reads each acceptance criterion's statement and
// verify_hint for a path-shaped token and matches it against the run's
// resolved implement-stage forbidden_paths using backend/internal/policy —
// the SAME matcher the post-implement gate runs, so the plan-time verdict is
// identical to the verdict the implement stage would produce for the same
// path. The finding rides the existing plan_surface_sweep entry, so the rule
// introduces no audit category and no issue-comment surface of its own.
//
// The heuristic bound, stated honestly: it reads acceptance-criteria prose
// ONLY. A criterion phrased "the verify gate enforces the new guard" requires
// a forbidden edit without naming any path and is NOT caught, and
// summary/approach/risks prose is deliberately not scanned because a plan
// legitimately discussing the forbidden-paths constraint would self-flag. A
// criterion that names a forbidden path for a reason OTHER than requiring an
// edit to it is a false positive, which a plan-declared
// surface_sweep_exemptions entry {pattern: surfacePatternForbiddenCriterion,
// sibling: <path>} clears with a reviewer-challengeable reason. Advisory in
// both directions: never a refusal.
const (
	// surfacePatternForbiddenCriterion is the finding's Pattern and the
	// exemption key a plan declares to suppress it.
	surfacePatternForbiddenCriterion = "acceptance criterion requires a forbidden path"
	// forbiddenCriterionSweepMaxFindings caps the findings per plan so a prose
	// blizzard cannot flood the gate evidence.
	forbiddenCriterionSweepMaxFindings = 10
)

// criterionPathMention is one path-shaped token and where in the plan's
// acceptance criteria it was named. Location is
// "acceptance_criteria[<id>].statement" or
// "acceptance_criteria[<id>].verify_hint".
type criterionPathMention struct {
	Path     string
	Location string
	// CriterionID is the sort key that makes the finding order deterministic
	// independently of the location string's shape.
	CriterionID string
}

// The two bounded capture shapes. A path-shaped token is captured ONLY when it
// is backticked/quoted (shape A) or carries a forward slash (shape B, the
// post-capture pathTokenHasSlash filter). A BARE unquoted word is deliberately
// NOT captured: prose mentioning the word LICENSE or NOTICE would otherwise
// match the same-named forbidden glob. Both shapes anchor the capture at a
// non-word left boundary so a longer or mixed-case identifier yields the WHOLE
// token rather than a matching suffix (`foo.fishhawk/bar` never yields
// `.fishhawk/bar`), exactly as the audit-category shapes do.
var (
	// Shape A, quoted: the token sits inside a backtick, or a straight or
	// curly single/double quote.
	criterionQuotedPathRE = regexp.MustCompile(
		"[`\"'‘“]([A-Za-z0-9._][A-Za-z0-9._/*-]*)[`\"'’”]")
	// Shape B, bare: a token at a non-word left boundary. The slash
	// requirement is the post-capture filter (Go regexp has no lookahead, and
	// requiring the slash inside the pattern would let a capture start mid-
	// identifier at the segment before the slash).
	criterionBarePathRE = regexp.MustCompile(
		"(?:^|[^A-Za-z0-9_./])([A-Za-z0-9._][A-Za-z0-9._/*-]*)")
)

// pathTokenHasSlash is shape B's post-capture filter: a bare token qualifies
// only when it carries a path separator.
func pathTokenHasSlash(tok string) bool { return strings.Contains(tok, "/") }

// extractCriterionPathTokens returns the path-shaped tokens in one prose
// field, deduplicated with first-seen order preserved. Quoted tokens are taken
// verbatim; a bare token has ONE trailing sentence-final period stripped
// (`see docs/spec/workflow-v2.md.` names the file, not `…md.`) and must carry
// a slash to qualify.
func extractCriterionPathTokens(text string) []string {
	if text == "" {
		return nil
	}
	seen := make(map[string]bool)
	var out []string
	add := func(tok string) {
		if tok == "" || seen[tok] {
			return
		}
		seen[tok] = true
		out = append(out, tok)
	}
	for _, m := range criterionQuotedPathRE.FindAllStringSubmatch(text, -1) {
		add(m[1])
	}
	for _, m := range criterionBarePathRE.FindAllStringSubmatch(text, -1) {
		tok := strings.TrimSuffix(m[1], ".")
		if !pathTokenHasSlash(tok) {
			continue
		}
		add(tok)
	}
	return out
}

// planCriterionPathMentions walks verification.acceptance_criteria in declared
// order and, per criterion, extracts tokens from statement then verify_hint.
// Duplicates WITHIN one criterion collapse (the same path named in both fields
// draws one finding, attributed to the statement); the same path in two
// criteria draws one finding each, because each criterion is separately
// unenforceable.
func planCriterionPathMentions(p *plan.Plan) []criterionPathMention {
	var out []criterionPathMention
	for _, c := range p.Verification.AcceptanceCriteria {
		seen := make(map[string]bool)
		for _, field := range []struct{ suffix, text string }{
			{"statement", c.Statement},
			{"verify_hint", c.VerifyHint},
		} {
			for _, tok := range extractCriterionPathTokens(field.text) {
				if seen[tok] {
					continue
				}
				seen[tok] = true
				out = append(out, criterionPathMention{
					Path:        tok,
					Location:    fmt.Sprintf("acceptance_criteria[%s].%s", c.ID, field.suffix),
					CriterionID: c.ID,
				})
			}
		}
	}
	return out
}

// forbiddenGlobForPaths maps each candidate path to the FIRST forbidden glob
// that matches it, using policy.Evaluate — the same matcher the post-implement
// gate runs, so the plan-time verdict is identical for the same path. Grounded
// in the code rather than inferred: policy's checkForbidden delegates to
// matchAny, which calls doublestar.PathMatch(pattern, f.Path) and never reads
// f.Status, so feeding it a synthetic modified-status diff of prose-extracted
// tokens is sound (the same property mapFileOperation already documents).
//
// A MALFORMED glob in the workflow spec yields a policy Violation carrying an
// "invalid pattern" Detail and NO Files, so it contributes no mapping and
// cannot crash the rule. Iteration is over the violation slice policy.Evaluate
// returns in pattern order, so "first matching glob" is the first DECLARED
// forbidden path — deterministic.
func forbiddenGlobForPaths(paths []string, forbidden []string) map[string]string {
	if len(paths) == 0 || len(forbidden) == 0 {
		return nil
	}
	diff := policy.Diff{}
	for _, p := range paths {
		diff.ChangedFiles = append(diff.ChangedFiles, policy.ChangedFile{
			Path:   p,
			Status: policy.StatusModified,
		})
	}
	out := make(map[string]string)
	for _, v := range policy.Evaluate(diff, policy.Constraints{ForbiddenPaths: forbidden}) {
		if v.Constraint != "forbidden_paths" || len(v.Files) == 0 {
			continue
		}
		glob := forbiddenGlobFromDetail(v.Detail, forbidden)
		if glob == "" {
			continue
		}
		for _, f := range v.Files {
			if _, ok := out[f]; !ok {
				out[f] = glob
			}
		}
	}
	return out
}

// forbiddenGlobFromDetail recovers WHICH declared glob a forbidden_paths
// violation belongs to. policy.Violation carries the pattern only inside its
// human-readable Detail (`pattern "x" matched`), and keying on Detail TEXT is
// the brittleness the prompt renderer's comment warns against — so this
// resolves the glob by testing each DECLARED pattern for containment in the
// quoted form policy emits, and returns "" when none matches (the caller then
// records no mapping rather than a guess).
func forbiddenGlobFromDetail(detail string, forbidden []string) string {
	for _, pat := range forbidden {
		if strings.Contains(detail, fmt.Sprintf("%q", pat)) {
			return pat
		}
	}
	return ""
}

// evaluateForbiddenCriterionRule is the PURE evaluator (no receiver, no I/O),
// mirroring evaluateAuditCategoryRule. forbidden is the run's resolved
// implement-stage forbidden_paths (the caller reads it fail-open via
// resolveImplementConstraints). It returns nil, nil when p is nil, when
// forbidden is empty, when the plan declares no acceptance criteria, and when
// no criterion names a path any glob forbids.
//
// A matching exemption {surfacePatternForbiddenCriterion, <the token path>}
// suppresses EVERY finding for that token and yields ONE AppliedExemption
// carrying the plan's reason. Otherwise one finding per distinct (criterion,
// token) pair, sorted by criterion id then token and capped at
// forbiddenCriterionSweepMaxFindings, each with Pattern, TriggerPath naming
// the token and its location, MissingSiblings nil (nothing is missing from
// scope — the path is structurally unreachable), and ForbiddenPath /
// ForbiddenPattern carrying the token and the glob that forbids it.
func evaluateForbiddenCriterionRule(p *plan.Plan, forbidden []string, exemptions []plan.SurfaceSweepExemption) ([]SurfaceSweepFinding, []AppliedExemption) {
	if p == nil || len(forbidden) == 0 || len(p.Verification.AcceptanceCriteria) == 0 {
		return nil, nil
	}

	mentions := planCriterionPathMentions(p)
	if len(mentions) == 0 {
		return nil, nil
	}
	candidates := make([]string, 0, len(mentions))
	seen := make(map[string]bool, len(mentions))
	for _, m := range mentions {
		if seen[m.Path] {
			continue
		}
		seen[m.Path] = true
		candidates = append(candidates, m.Path)
	}

	matched := forbiddenGlobForPaths(candidates, forbidden)
	if len(matched) == 0 {
		return nil, nil
	}

	// Index the plan's exemptions for this rule by slash-normalized path.
	exByPath := make(map[string]plan.SurfaceSweepExemption)
	for _, e := range exemptions {
		if e.Pattern != surfacePatternForbiddenCriterion {
			continue
		}
		exByPath[toSlashPath(e.Sibling)] = e
	}

	var hits []criterionPathMention
	exemptedReason := make(map[string]string)
	for _, m := range mentions {
		if _, ok := matched[m.Path]; !ok {
			continue
		}
		if e, isEx := exByPath[toSlashPath(m.Path)]; isEx {
			// Suppressed: the reason is recorded once per token below, so a
			// reviewer sees each challengeable justification exactly once.
			exemptedReason[m.Path] = e.Reason
			continue
		}
		hits = append(hits, m)
	}

	// Applied exemptions in first-mention (candidate) order for determinism.
	var applied []AppliedExemption
	for _, path := range candidates {
		reason, ok := exemptedReason[path]
		if !ok {
			continue
		}
		applied = append(applied, AppliedExemption{
			Pattern: surfacePatternForbiddenCriterion,
			Sibling: toSlashPath(path),
			Reason:  reason,
		})
	}

	if len(hits) == 0 {
		return nil, applied
	}

	sort.SliceStable(hits, func(a, b int) bool {
		if hits[a].CriterionID != hits[b].CriterionID {
			return hits[a].CriterionID < hits[b].CriterionID
		}
		return hits[a].Path < hits[b].Path
	})
	if len(hits) > forbiddenCriterionSweepMaxFindings {
		hits = hits[:forbiddenCriterionSweepMaxFindings]
	}
	findings := make([]SurfaceSweepFinding, 0, len(hits))
	for _, m := range hits {
		findings = append(findings, SurfaceSweepFinding{
			Pattern:          surfacePatternForbiddenCriterion,
			TriggerPath:      fmt.Sprintf("path %q named at %s", m.Path, m.Location),
			ForbiddenPath:    toSlashPath(m.Path),
			ForbiddenPattern: matched[m.Path],
		})
	}
	return findings, applied
}
