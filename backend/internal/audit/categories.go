package audit

import "sort"

// KnownCategories is the curated registry of canonical audit-log category
// strings (#1764, extended #1850). It is the validation authority behind
// fishhawk_await_audit and GET /v0/runs/{run_id}/audit: a wait armed on a
// category NOT in this set is almost always a misspelling or a
// wrong-surface string (e.g. the runner-log event "scope_amendment_pending"
// instead of the audit category "scope_amendment_requested"), which would
// otherwise block the full timeout on an unsatisfiable wait while the real
// entry sits undecided.
//
// The registry lives in package audit — NOT package server — deliberately:
// backend/internal/server already imports backend/internal/audit, so
// importing the server-side category constants back here would be an import
// cycle. The strings are therefore hardcoded rather than referenced from
// their emit sites; SuggestCategories's reproduction test and the
// IsKnownCategory sampled-membership test guard against drift, and the
// allow_unknown escape hatch bounds the cost of any omission.
//
// Seeded from the full backend/internal inventory: the run/plan/implement/
// acceptance/deployment lifecycle, the review-lifecycle literals, the
// scope-amendment + scope-completeness gates, PR/lineage events, the
// budget/cost/CI signals, and the token/policy/status surfaces. #1850
// closed the remaining gaps — the registry now also covers the API-token
// issue/revoke events, the board/work-item filing + transition family, the
// refinement-draft approve/reject decisions, the runner-kind resolution
// events, the deployment-dispatch failure, and the campaign-lifecycle
// markers (advanced / gate-acted / issue-started / issue-settled /
// issue-restarted / paused) written via audit.AppendGlobalChained. #1941
// added the failed-run revive audit kind (run_revived, #1915). When a
// new canonical category is introduced, add it here so operators can await
// it without the allow_unknown escape hatch;
// categories_completeness_test.go's AST sweep fails the build if a
// non-test backend audit-write emits a category absent from this map.
var KnownCategories = map[string]struct{}{
	"acceptance_dispatched":                   {},
	"acceptance_outcome_recorded":             {},
	"acceptance_recorded":                     {},
	"acceptance_reopened":                     {},
	"acceptance_skipped_out_of_scope":         {},
	"acceptance_triage_decided":               {},
	"anchor_ping_posted":                      {},
	"api_token_issued":                        {},
	"api_token_revoked":                       {},
	"approval_predicate_rejected":             {},
	"approval_sla_elapsed":                    {},
	"approval_submitted":                      {},
	"audit_check_publish_degraded":            {},
	"audit_check_publish_recovered":           {},
	"branch_reset":                            {},
	"budget_alert":                            {},
	"budget_alert_sent":                       {},
	"campaign_advanced":                       {},
	"campaign_gate_acted":                     {},
	"campaign_gate_paged":                     {},
	"campaign_issue_restarted":                {},
	"campaign_issue_settled":                  {},
	"campaign_issue_started":                  {},
	"campaign_paused":                         {},
	"child_pushed":                            {},
	"child_redriven":                          {},
	"children_settled":                        {},
	"ci_failure_retry_dispatched":             {},
	"ci_green":                                {},
	"ci_retry_exhausted":                      {},
	"clarification_answered":                  {},
	"clarification_requested":                 {},
	"concern_defer_failed":                    {},
	"concern_deferred":                        {},
	"concern_relitigation_suppressed":         {},
	"concern_waive_failed":                    {},
	"concern_waived":                          {},
	"consolidated_pr_opened":                  {},
	"consolidated_review_diff_truncated":      {},
	"cost_recorded":                           {},
	"deploy_preflight_refused":                {},
	"deploy_run":                              {},
	"deployment_dispatch_failed":              {},
	"deployment_dispatched":                   {},
	"deployment_outcome_recorded":             {},
	"deployment_rollback_completed":           {},
	"deployment_rollback_initiated":           {},
	"dispatch_reaper_failed":                  {},
	"dispatch_watchdog_elapsed":               {},
	"fixup_no_changes":                        {},
	"fixup_pushed":                            {},
	"implement_review_backstop_elapsed":       {},
	"implement_review_failed":                 {},
	"implement_review_skipped":                {},
	"implement_review_started":                {},
	"implement_reviewed":                      {},
	"implement_security_findings":             {},
	"installation_token_issued":               {},
	"integration_commit_recorded":             {},
	"invariant_violation":                     {},
	"issue_commented":                         {},
	"lineage_violation":                       {},
	"mcp_token_issued":                        {},
	"merge_verdict_recorded":                  {},
	"model_resolved":                          {},
	"operator_commit_vouched":                 {},
	"operator_scope_path_undelivered":         {},
	"parent_awaiting_redrive":                 {},
	"plan_acceptance_precheck":                {},
	"plan_budget_override_acknowledged":       {},
	"plan_coerced":                            {},
	"plan_decomposed":                         {},
	"plan_generated":                          {},
	"plan_missing_for_implement":              {},
	"plan_periodic_budget_tier_acknowledged":  {},
	"plan_reaction_observed":                  {},
	"plan_reused_from":                        {},
	"plan_review_backstop_elapsed":            {},
	"plan_review_failed":                      {},
	"plan_review_skipped":                     {},
	"plan_review_started":                     {},
	"plan_reviewed":                           {},
	"plan_revised":                            {},
	"plan_schema_retry":                       {},
	"plan_scope_cap_override_acknowledged":    {},
	"plan_scope_precheck":                     {},
	"plan_scope_regression":                   {},
	"plan_surface_sweep":                      {},
	"plan_test_sweep":                         {},
	"plan_warnings":                           {},
	"plan_violates_budget":                    {},
	"plan_violates_periodic_budget":           {},
	"plan_violates_scope_cap":                 {},
	"policy_evaluated":                        {},
	"post_merge_observed":                     {},
	"pr_approved_on_github":                   {},
	"pr_closed_without_merge":                 {},
	"pr_merged":                               {},
	"pr_review_posted":                        {},
	"pr_review_submitted":                     {},
	"pr_status_comment_posted":                {},
	"product_report_filed":                    {},
	"pull_request_closed_after_review_reject": {},
	"pull_request_failed":                     {},
	"pull_request_opened":                     {},
	"refinement_draft_approved":               {},
	"refinement_draft_edited":                 {},
	"refinement_draft_rejected":               {},
	"refinement_filing_completed":             {},
	"release_cut":                             {},
	"release_published":                       {},
	"reviewer_capability_unavailable":         {},
	"run_admitted_budget_override":            {},
	"run_auto_advanced":                       {},
	"run_auto_driven":                         {},
	"run_budget_exceeded":                     {},
	"run_completed":                           {},
	"run_dispatched":                          {},
	"run_rejected_budget":                     {},
	"run_rejected_misconfigured":              {},
	"run_revived":                             {},
	"runner_kind_mismatch":                    {},
	"runner_kind_resolved":                    {},
	"runtime_observed":                        {},
	"scope_amendment_decided":                 {},
	"scope_amendment_requested":               {},
	"scope_completeness_exempted":             {},
	"scope_completeness_failed":               {},
	"scope_completeness_parked":               {},
	"scope_files_exempted":                    {},
	"slice_integration_conflict":              {},
	"slice_integration_failed":                {},
	"slices_integrated":                       {},
	"spend_alert":                             {},
	"stage_fixup_recovered":                   {},
	"stage_fixup_triggered":                   {},
	"stage_override_retried":                  {},
	"stage_retried":                           {},
	"status_comment_posted":                   {},
	"trace_uploaded":                          {},
	"unpriced_model_alert":                    {},
	"work_item_filed":                         {},
	"work_item_transitioned":                  {},
}

// knownCategoryList is the sorted slice form of KnownCategories, computed
// once at package init so KnownCategoryList / SuggestCategories return a
// stable, deterministic order without re-sorting per call.
var knownCategoryList = func() []string {
	out := make([]string, 0, len(KnownCategories))
	for c := range KnownCategories {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}()

// IsKnownCategory reports whether category is a canonical audit-log
// category in the curated registry.
func IsKnownCategory(category string) bool {
	_, ok := KnownCategories[category]
	return ok
}

// KnownCategoryList returns the registry as a sorted slice (a fresh copy so
// callers cannot mutate the shared backing array).
func KnownCategoryList() []string {
	out := make([]string, len(knownCategoryList))
	copy(out, knownCategoryList)
	return out
}

// SuggestCategories returns up to max known categories nearest to input by
// Levenshtein edit distance, closest first, ties broken lexicographically
// (deterministic). It is the "did you mean" suggester the fail-loud
// validation surfaces: SuggestCategories("scope_amendment_pending", 3)
// ranks "scope_amendment_requested" first. max <= 0 or an empty registry
// returns nil.
func SuggestCategories(input string, max int) []string {
	if max <= 0 {
		return nil
	}
	type scored struct {
		category string
		dist     int
	}
	ranked := make([]scored, 0, len(knownCategoryList))
	for _, c := range knownCategoryList {
		ranked = append(ranked, scored{category: c, dist: levenshtein(input, c)})
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].dist != ranked[j].dist {
			return ranked[i].dist < ranked[j].dist
		}
		return ranked[i].category < ranked[j].category
	})
	if max > len(ranked) {
		max = len(ranked)
	}
	out := make([]string, 0, max)
	for i := 0; i < max; i++ {
		out = append(out, ranked[i].category)
	}
	return out
}

// levenshtein computes the edit distance between a and b with the standard
// two-row dynamic-programming table. Self-contained (no external
// dependency) — the registry is small, so the O(len(a)*len(b)) cost per
// candidate is negligible.
func levenshtein(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	la, lb := len(ra), len(rb)
	if la == 0 {
		return lb
	}
	if lb == 0 {
		return la
	}
	prev := make([]int, lb+1)
	curr := make([]int, lb+1)
	for j := 0; j <= lb; j++ {
		prev[j] = j
	}
	for i := 1; i <= la; i++ {
		curr[0] = i
		for j := 1; j <= lb; j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			curr[j] = min3(prev[j]+1, curr[j-1]+1, prev[j-1]+cost)
		}
		prev, curr = curr, prev
	}
	return prev[lb]
}

func min3(a, b, c int) int {
	m := a
	if b < m {
		m = b
	}
	if c < m {
		m = c
	}
	return m
}
