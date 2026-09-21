package server

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
)

// The audit-category rule of the plan-gate surface sweep (#3410).
//
// A plan that introduces a NEW audit category (any string literal reaching
// audit.Entry / ChainAppendParams / AppendChained / an emit helper) obliges
// backend/internal/audit/categories.go in the SAME scope: the committed-tree
// verify gate's TestKnownCategoriesCoversEmittedCategories fails on an
// unregistered emitted category, and an implement agent whose scope omits the
// registry cannot reach it — run 49f642b8 discovered the obligation only by
// failing that gate. Unlike every path-triggered surfacePatterns entry, this
// rule is PROSE-triggered: it reads the planner's own text for a snake_case
// token named as an audit category / entry / row / event, drops the tokens the
// registry already knows (the exactly-decidable half), and flags the rest when
// the registry file is in neither the top-level scope nor any sub-plan scope.
//
// The heuristic bound, stated honestly: it reads the planner's PROSE, so a
// category the plan never names is not caught (the runner-side OUT-OF-SCOPE
// PATHS hint in the verify-fix prompt covers that residual once the gate names
// the file), and a same-shaped non-category token named as an audit entry is a
// false positive — which a plan-declared surface_sweep_exemptions entry
// {pattern: surfacePatternAuditCategoryRegistry, sibling:
// auditCategoryRegistryPath} clears, recorded as an applied exemption so a
// reviewer can challenge it. Both directions are advisory: never a refusal.
const (
	// surfacePatternAuditCategoryRegistry is the finding's Pattern and the
	// exemption key a plan declares to suppress it.
	surfacePatternAuditCategoryRegistry = "new audit category requires registry"
	// auditCategoryRegistryPath is the one sibling the rule obliges.
	auditCategoryRegistryPath = "backend/internal/audit/categories.go"
	// auditCategoryVerifyTest is the committed-tree verify test that fails on
	// an unregistered emitted category; named in the reviewer-facing render.
	auditCategoryVerifyTest = "TestKnownCategoriesCoversEmittedCategories"
	// auditCategorySweepMaxFindings caps the findings per plan so a prose
	// blizzard cannot flood the gate evidence.
	auditCategorySweepMaxFindings = 10
	// mcpToolNamePrefix marks fishhawk_* MCP tool names (NOT an audit category —
	// the name deliberately avoids "category" so the audit registry sweep
	// does not collect it as a value-spec binding), which share the
	// snake_case shape and are routinely named next to "audit entry" in prose.
	mcpToolNamePrefix = "fishhawk_"
)

// auditCategoryMention is one candidate token and where in the plan it was
// named. Location is one of: "summary", "approach step N",
// "verification.test_strategy", "acceptance_criteria[<id>].statement",
// "acceptance_criteria[<id>].verify_hint", "risks_and_assumptions[i]",
// "decomposition.sub_plans[<title>].scope_hint".
type auditCategoryMention struct {
	Category string
	Location string
}

// The three bounded regex shapes. Go regexp has no lookahead/lookbehind, so
// the lowercase-snake_case + underscore + not-a-tool-name rules are a SECOND
// filter step (isAuditCategoryToken) rather than part of the pattern; the
// shapes only bound WHERE a token may sit relative to the audit-category
// keywords. Every shape anchors the token at a non-word left boundary so a
// mixed-case token such as `Mixed_new_cat` yields NO token at all rather than
// a lowercase suffix (`ixed_new_cat`).
var (
	// Shape A, keyword-first: `audit <category|kind|entry|row|event> "tok"` —
	// the token MUST be quoted or backticked, so `audit entry precedes
	// plan_reviewed` never captures the bare word after the keyword.
	auditCategoryKeywordFirstRE = regexp.MustCompile(
		"(?i)\\baudit(?:[ -]log)?\\s+(?:categor(?:y|ies)|kinds?|entr(?:y|ies)|rows?|events?)\\s*(?:named|called|of|for|[:=—–-])?\\s*[`\"'‘“]\\b([A-Za-z][A-Za-z0-9_]*)\\b[`\"'’”]")
	// Shape B, token-first: `"tok" audit <category|entry|row|event|marker>`
	// (quotes optional). The leading class consumes the character before the
	// token (or its opening quote) and rejects a word character, '/' or '.',
	// so the token is a WHOLE word, never a suffix of a longer identifier or
	// a path segment.
	auditCategoryTokenFirstRE = regexp.MustCompile(
		"(?:^|[^A-Za-z0-9_./])[`\"'‘“]?([a-z][a-z0-9_]*)[`\"'’”]?\\s+audit\\s+(?:categor(?:y|ies)|kinds?|entr(?:y|ies)|rows?|events?|markers?)\\b")
	// Shape C, field form: `category: "tok"` / `category = "tok"` /
	// `category is "tok"` — the token MUST be quoted or backticked.
	auditCategoryFieldRE = regexp.MustCompile(
		"(?i)\\bcategory\\s*(?:[:=]|is|of)?\\s*[`\"'‘“]\\b([A-Za-z][A-Za-z0-9_]*)\\b[`\"'’”]")

	// auditCategoryTokenRE is the post-capture filter: lowercase snake_case.
	auditCategoryTokenRE = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
)

// isAuditCategoryToken is the post-capture filter every captured token must
// pass: lowercase snake_case with at least one underscore (every registered
// category has one; a bare word like `entry` never qualifies) and not a
// fishhawk_* MCP tool name.
func isAuditCategoryToken(tok string) bool {
	if !auditCategoryTokenRE.MatchString(tok) {
		return false
	}
	if !strings.Contains(tok, "_") {
		return false
	}
	return !strings.HasPrefix(tok, mcpToolNamePrefix)
}

// extractAuditCategoryMentions runs the three shapes over one prose field and
// returns every token that passes the filter, tagged with location, in
// shape-then-text order. It does NOT apply the known-category filter — that
// lives in the evaluator, so a same-line known + unknown pair is captured raw
// here and the evaluator decides.
func extractAuditCategoryMentions(location, text string) []auditCategoryMention {
	if text == "" {
		return nil
	}
	var out []auditCategoryMention
	for _, re := range []*regexp.Regexp{auditCategoryKeywordFirstRE, auditCategoryTokenFirstRE, auditCategoryFieldRE} {
		for _, m := range re.FindAllStringSubmatch(text, -1) {
			tok := m[1]
			if !isAuditCategoryToken(tok) {
				continue
			}
			out = append(out, auditCategoryMention{Category: tok, Location: location})
		}
	}
	return out
}

// planAuditCategoryMentions walks the plan's prose fields in a fixed order —
// summary, approach steps, verification.test_strategy, each acceptance
// criterion's statement then verify_hint, risks_and_assumptions, each
// decomposition sub-plan's scope_hint — and returns the mentions deduped by
// token (first location wins).
func planAuditCategoryMentions(p *plan.Plan) []auditCategoryMention {
	var all []auditCategoryMention
	all = append(all, extractAuditCategoryMentions("summary", p.Summary)...)
	for _, step := range p.Approach {
		all = append(all, extractAuditCategoryMentions(fmt.Sprintf("approach step %d", step.Step), step.Description)...)
	}
	all = append(all, extractAuditCategoryMentions("verification.test_strategy", p.Verification.TestStrategy)...)
	for _, c := range p.Verification.AcceptanceCriteria {
		all = append(all, extractAuditCategoryMentions(fmt.Sprintf("acceptance_criteria[%s].statement", c.ID), c.Statement)...)
		all = append(all, extractAuditCategoryMentions(fmt.Sprintf("acceptance_criteria[%s].verify_hint", c.ID), c.VerifyHint)...)
	}
	for i, r := range p.RisksAndAssumptions {
		all = append(all, extractAuditCategoryMentions(fmt.Sprintf("risks_and_assumptions[%d]", i), r)...)
	}
	if p.Decomposition != nil {
		for _, sp := range p.Decomposition.SubPlans {
			all = append(all, extractAuditCategoryMentions(fmt.Sprintf("decomposition.sub_plans[%s].scope_hint", sp.Title), sp.ScopeHint)...)
		}
	}

	seen := make(map[string]bool, len(all))
	out := make([]auditCategoryMention, 0, len(all))
	for _, m := range all {
		if seen[m.Category] {
			continue
		}
		seen[m.Category] = true
		out = append(out, m)
	}
	return out
}

// evaluateAuditCategoryRule is the PURE evaluator (no receiver, no I/O),
// mirroring evaluateSurfaceSweep. isKnown is injected (production passes
// audit.IsKnownCategory) so the known-category filter is testable without the
// registry. It returns nil, nil when:
//   - no scoped path (top-level ∪ every declared sub-plan scope) has the
//     backend/ prefix — the rule is fishhawk-backend-specific, like every
//     other registry pattern's literal paths, so a plan for another repository
//     never draws it;
//   - auditCategoryRegistryPath is already in that union scope (obligation
//     satisfied);
//   - every mentioned token is already known.
//
// A matching exemption {surfacePatternAuditCategoryRegistry,
// auditCategoryRegistryPath} yields NO findings and ONE AppliedExemption
// carrying the plan's reason. Otherwise one finding per distinct unknown token,
// sorted by token and capped at auditCategorySweepMaxFindings, each with
// Pattern, TriggerPath naming the token and where it was found, MissingSiblings
// [auditCategoryRegistryPath] and Category set.
func evaluateAuditCategoryRule(p *plan.Plan, isKnown func(string) bool, exemptions []plan.SurfaceSweepExemption) ([]SurfaceSweepFinding, []AppliedExemption) {
	if p == nil {
		return nil, nil
	}
	scope := make(map[string]bool, len(p.Scope.Files))
	for _, f := range p.Scope.Files {
		scope[toSlashPath(f.Path)] = true
	}
	if p.Decomposition != nil {
		for _, sp := range p.Decomposition.SubPlans {
			if sp.Scope == nil {
				continue
			}
			for _, f := range sp.Scope.Files {
				scope[toSlashPath(f.Path)] = true
			}
		}
	}

	backend := false
	for path := range scope {
		if strings.HasPrefix(path, "backend/") {
			backend = true
			break
		}
	}
	if !backend {
		return nil, nil
	}
	if scope[auditCategoryRegistryPath] {
		return nil, nil
	}

	var unknown []auditCategoryMention
	for _, m := range planAuditCategoryMentions(p) {
		if isKnown(m.Category) {
			continue
		}
		unknown = append(unknown, m)
	}
	if len(unknown) == 0 {
		return nil, nil
	}

	for _, e := range exemptions {
		if e.Pattern == surfacePatternAuditCategoryRegistry && toSlashPath(e.Sibling) == auditCategoryRegistryPath {
			return nil, []AppliedExemption{{
				Pattern: surfacePatternAuditCategoryRegistry,
				Sibling: auditCategoryRegistryPath,
				Reason:  e.Reason,
			}}
		}
	}

	sort.Slice(unknown, func(i, j int) bool { return unknown[i].Category < unknown[j].Category })
	if len(unknown) > auditCategorySweepMaxFindings {
		unknown = unknown[:auditCategorySweepMaxFindings]
	}
	findings := make([]SurfaceSweepFinding, 0, len(unknown))
	for _, m := range unknown {
		findings = append(findings, SurfaceSweepFinding{
			Pattern:         surfacePatternAuditCategoryRegistry,
			TriggerPath:     fmt.Sprintf("audit category %q named at %s", m.Category, m.Location),
			MissingSiblings: []string{auditCategoryRegistryPath},
			Category:        m.Category,
		})
	}
	return findings, nil
}
