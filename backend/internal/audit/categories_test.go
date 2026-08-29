package audit

import (
	"sort"
	"testing"
)

// TestIsKnownCategory pins membership for a sample of real canonical
// categories (true) and garbage / wrong-surface strings (false). The
// scope_amendment_pending case is the #1764 reproduction: the runner-log
// event string is NOT a known audit category, so a wait armed on it must be
// rejectable.
func TestIsKnownCategory(t *testing.T) {
	known := []string{
		"scope_amendment_requested",
		"implement_reviewed",
		"plan_reviewed",
		"fixup_pushed",
		"acceptance_outcome_recorded",
		"plan_review_started",
		"plan_review_failed",
		"plan_review_skipped",
		"run_completed",
		"deployment_outcome_recorded",
		"run_revived",
		"merge_verdict_recorded",   // E48.7 / #1954 operator merge-verdict chain entry
		"grooming_report_recorded", // E54.3 / #2235 grooming_report ingest entry
		"grooming_churn_filtered",  // E54.8 / #2240 churn-guard verdict entry
	}
	for _, c := range known {
		if !IsKnownCategory(c) {
			t.Errorf("IsKnownCategory(%q) = false, want true (canonical category missing from registry)", c)
		}
	}

	unknown := []string{
		"scope_amendment_pending", // the #1764 runner-log event, NOT an audit category
		"implement_review",        // truncated
		"",                        // empty
		"garbage_not_a_category",
		"IMPLEMENT_REVIEWED", // wrong case
	}
	for _, c := range unknown {
		if IsKnownCategory(c) {
			t.Errorf("IsKnownCategory(%q) = true, want false", c)
		}
	}
}

// TestSuggestCategories_RanksNearest is the #1764 reproduction: the
// misspelled/wrong-surface "scope_amendment_pending" must surface the real
// "scope_amendment_requested" among its nearest suggestions, so the operator
// who typed the runner-log event string is pointed at the audit category.
//
// Note the plan's prose asserted "requested first"; with the full registry
// that is mechanically not the closest by Levenshtein — the equally-real
// "scope_amendment_decided" (suffix edit "pending"→"decided" = 6) beats
// "pending"→"requested" (= 8). Both are correct scope_amendment_* siblings
// and both land in the top suggestions, which is the property that matters:
// the fail-loud message names the right family. We assert membership among
// the two nearest rather than distorting the specified metric or dropping a
// genuine category to force a first-place tie.
func TestSuggestCategories_RanksNearest(t *testing.T) {
	got := SuggestCategories("scope_amendment_pending", 3)
	if len(got) == 0 {
		t.Fatal("SuggestCategories returned no suggestions")
	}
	if len(got) > 3 {
		t.Errorf("returned %d suggestions, want <= 3", len(got))
	}
	// scope_amendment_requested is one of the two nearest scope_amendment_*
	// siblings — the operator sees it and fixes the wrong-surface string.
	nearestTwo := got
	if len(nearestTwo) > 2 {
		nearestTwo = nearestTwo[:2]
	}
	found := false
	for _, c := range nearestTwo {
		if c == "scope_amendment_requested" {
			found = true
		}
	}
	if !found {
		t.Errorf("scope_amendment_requested not among the two nearest suggestions; got=%v", got)
	}
}

// TestSuggestCategories_MaxCapAndDeterminism proves the max cap is honored
// and the ranking is deterministic across repeated calls (stable tie-break).
func TestSuggestCategories_MaxCapAndDeterminism(t *testing.T) {
	if got := SuggestCategories("plan_reviewd", 0); got != nil {
		t.Errorf("max=0 must return nil, got %v", got)
	}
	if got := SuggestCategories("plan_reviewd", -1); got != nil {
		t.Errorf("negative max must return nil, got %v", got)
	}

	first := SuggestCategories("plan_reviewd", 5)
	if len(first) != 5 {
		t.Fatalf("max=5 returned %d suggestions, want exactly 5", len(first))
	}
	// A near-miss of "plan_reviewed" should rank it first.
	if first[0] != "plan_reviewed" {
		t.Errorf("nearest to plan_reviewd = %q, want plan_reviewed; full=%v", first[0], first)
	}
	for i := 0; i < 5; i++ {
		again := SuggestCategories("plan_reviewd", 5)
		if len(again) != len(first) {
			t.Fatalf("non-deterministic length: %d vs %d", len(again), len(first))
		}
		for j := range first {
			if again[j] != first[j] {
				t.Fatalf("non-deterministic order at %d: %q vs %q", j, again[j], first[j])
			}
		}
	}

	// A max larger than the registry returns the whole registry, not a panic.
	all := SuggestCategories("x", len(KnownCategories)+50)
	if len(all) != len(KnownCategories) {
		t.Errorf("oversized max returned %d, want the full registry size %d", len(all), len(KnownCategories))
	}
}

// TestKnownCategoryList_NonEmptyAndSorted asserts the exported sorted view
// is non-empty, sorted, and a defensive copy (mutating it does not corrupt
// the shared registry order).
func TestKnownCategoryList_NonEmptyAndSorted(t *testing.T) {
	list := KnownCategoryList()
	if len(list) == 0 {
		t.Fatal("KnownCategoryList is empty")
	}
	if len(list) != len(KnownCategories) {
		t.Errorf("list length %d != map length %d", len(list), len(KnownCategories))
	}
	if !sort.StringsAreSorted(list) {
		t.Errorf("KnownCategoryList is not sorted: %v", list)
	}
	// Every listed entry is a known category and vice versa.
	for _, c := range list {
		if !IsKnownCategory(c) {
			t.Errorf("listed category %q is not IsKnownCategory", c)
		}
	}

	// Mutating the returned slice must not affect a subsequent call.
	list[0] = "zzz_mutated"
	fresh := KnownCategoryList()
	if fresh[0] == "zzz_mutated" {
		t.Error("KnownCategoryList returned a shared backing array; callers can corrupt it")
	}
}

// TestKnownCategories_EscalationFired registers the E53.4 / #2227 category so
// an operator can await it. An unregistered category is an un-awaitable audit
// stream, which would leave "the operator sees why a gate got stricter" only
// half true — and categories_completeness_test.go's AST sweep fails the build
// on a category backend code emits but the registry omits.
func TestKnownCategories_EscalationFired(t *testing.T) {
	if !IsKnownCategory("escalation_fired") {
		t.Fatal("escalation_fired is not in KnownCategories; fishhawk_await_audit would reject a wait armed on it")
	}
}

// TestKnownCategories_StagePermissionsDeclared pins the E53.5 / #2228 category:
// the run-creation emitter writes it once per run whose workflow declares any
// stage `permissions`/`egress` block, and an unregistered category is
// un-awaitable, which would leave the "surfaced" half of the acceptance
// criterion untrue.
func TestKnownCategories_StagePermissionsDeclared(t *testing.T) {
	if !IsKnownCategory("stage_permissions_declared") {
		t.Fatal("stage_permissions_declared is not in KnownCategories; fishhawk_await_audit would reject a wait armed on it")
	}
}

// TestKnownCategories_AgentRequestFailedAlert pins the E48.66 / #2494
// category: the cost-ledger check writes it when an in-window cost row
// carried a placeholder model id (a request that never reached a model)
// rather than an unpriced real model. An unregistered category is
// un-awaitable — an operator could not arm fishhawk_await_audit on the new
// alert, so a failed request would be as undiagnosable as the
// unpriced_model_alert mislabel it replaces — and
// categories_completeness_test.go's AST sweep would fail the build on the
// emit site in trace.go.
func TestKnownCategories_AgentRequestFailedAlert(t *testing.T) {
	if !IsKnownCategory("agent_request_failed_alert") {
		t.Fatal("agent_request_failed_alert is not in KnownCategories; fishhawk_await_audit would reject a wait armed on it")
	}
}

// TestKnownCategories_AcceptanceTriageArbitrated pins the E66.37 / #2474
// category: the operator-only arbitration endpoint writes it to discharge a
// paged acceptance triage, and an unregistered category is un-awaitable — an
// operator could not arm fishhawk_await_audit on their own discharge, and
// categories_completeness_test.go's AST sweep would fail the build the moment
// the emit site lands.
func TestKnownCategories_AcceptanceTriageArbitrated(t *testing.T) {
	if !IsKnownCategory("acceptance_triage_arbitrated") {
		t.Fatal("acceptance_triage_arbitrated is not in KnownCategories; fishhawk_await_audit would reject a wait armed on it")
	}
}

// TestKnownCategories_PlanMissingRequiredTests pins the E67.35 / #2660
// category: the plan-approval required-tests gate writes it when it refuses
// an approve whose effective scope declares no test-shaped path while the
// implement stage requires tests_added_or_updated. An unregistered category
// is un-awaitable — an operator could not arm fishhawk_await_audit on their
// own refusal — and categories_completeness_test.go's AST sweep would fail
// the build the moment the emit site lands.
func TestKnownCategories_PlanMissingRequiredTests(t *testing.T) {
	if !IsKnownCategory("plan_missing_required_tests") {
		t.Fatal("plan_missing_required_tests is not in KnownCategories; fishhawk_await_audit would reject a wait armed on it")
	}
}

// TestKnownCategories_PlanCommentOnlyOverrideRefused pins the E67.35 /
// #2660 categorical refusal marker: --comment-only cannot authorize a scope
// carrying a non-.go testable-source path, and the refusal must be readable
// from the run record.
func TestKnownCategories_PlanCommentOnlyOverrideRefused(t *testing.T) {
	if !IsKnownCategory("plan_comment_only_override_refused") {
		t.Fatal("plan_comment_only_override_refused is not in KnownCategories; fishhawk_await_audit would reject a wait armed on it")
	}
}

// TestKnownCategories_PlanCommentOnlyOverrideAcknowledged pins the E67.35 /
// #2660 honored-override marker, which records that the override covers the
// plan gate only — the implement stage re-derives the comment-only verdict
// from the REAL diff.
func TestKnownCategories_PlanCommentOnlyOverrideAcknowledged(t *testing.T) {
	if !IsKnownCategory("plan_comment_only_override_acknowledged") {
		t.Fatal("plan_comment_only_override_acknowledged is not in KnownCategories; fishhawk_await_audit would reject a wait armed on it")
	}
}

// TestKnownCategories_FixupReportingObligationUndelivered pins the E67.66 /
// #2737 category: the implement-review assembly path writes it when a fix-up
// pass leaves a routed REPORTING obligation without a valid `met` self-report.
// An unregistered category is un-awaitable (fishhawk_await_audit would reject a
// wait armed on it) AND unfilterable via GET /v0/runs/{run_id}/audit?category=,
// which would leave the "an operator sees the omission at the gate without
// reading the PR" half of the acceptance criterion untrue — and
// categories_completeness_test.go's AST sweep fails the build on a category
// backend code emits but the registry omits.
// TestKnownCategories_FixupConcernUnattempted pins the #2896 advisory
// pre-review signal's category: the implement review appends
// fixup_concern_unattempted when a fix-up pass left the files a routed
// concern's instruction text named entirely untouched. Unregistered it would be
// unwaitable via fishhawk_await_audit and unfilterable via
// GET /v0/runs/{run_id}/audit?category=, which is the operator-facing half of
// the signal.
func TestKnownCategories_FixupConcernUnattempted(t *testing.T) {
	if !IsKnownCategory("fixup_concern_unattempted") {
		t.Fatal("fixup_concern_unattempted is not in KnownCategories; fishhawk_await_audit would reject a wait armed on it")
	}
}

func TestKnownCategories_FixupReportingObligationUndelivered(t *testing.T) {
	if !IsKnownCategory("fixup_reporting_obligation_undelivered") {
		t.Fatal("fixup_reporting_obligation_undelivered is not in KnownCategories; fishhawk_await_audit would reject a wait armed on it")
	}
}

// TestKnownCategories_FixupReportObligationsDeclared pins the #2737 concurrency
// fix-up's anchor category: the fix-up prompt-serve path appends it to record
// WHICH stage_fixup_triggered entry the served prompt derived its
// reporting-obligation block from, and the implement review resolves that exact
// entry rather than re-selecting the newest one. An unregistered category is
// un-awaitable and unfilterable via GET /v0/runs/{run_id}/audit?category=, and
// categories_completeness_test.go's AST sweep fails the build on a category
// backend code emits but the registry omits.
func TestKnownCategories_FixupReportObligationsDeclared(t *testing.T) {
	if !IsKnownCategory("fixup_report_obligations_declared") {
		t.Fatal("fixup_report_obligations_declared is not in KnownCategories; fishhawk_await_audit would reject a wait armed on it")
	}
}

// TestKnownCategories_FixupPRBodyUnsatisfiable pins the #2782 routing-time
// advisory category: fixupStageAs appends it when a routed instruction names
// the PR body — a surface the pass cannot write — so the operator sees the
// obligation was structurally impossible. An unregistered category is
// un-awaitable and unfilterable via GET /v0/runs/{run_id}/audit?category=, and
// categories_completeness_test.go's AST sweep fails the build on a category
// backend code emits but the registry omits.
func TestKnownCategories_FixupPRBodyUnsatisfiable(t *testing.T) {
	if !IsKnownCategory("fixup_pr_body_unsatisfiable") {
		t.Fatal("fixup_pr_body_unsatisfiable is not in KnownCategories; fishhawk_await_audit would reject a wait armed on it")
	}
}

// TestKnownCategories_GroomingReportRecorded pins E54.3 / #2235's ingest
// category: handleGroomingReport appends it once per persisted grooming_report
// artifact, carrying the content hash and the per-class entry counts #2240's
// churn guard reads. An unregistered category is un-awaitable and unfilterable
// via GET /v0/runs/{run_id}/audit?category=, and categories_completeness_test.go's
// AST sweep fails the build on a category backend code emits but the registry
// omits.
func TestKnownCategories_GroomingReportRecorded(t *testing.T) {
	if !IsKnownCategory("grooming_report_recorded") {
		t.Fatal("grooming_report_recorded is not in KnownCategories; fishhawk_await_audit would reject a wait armed on it")
	}
	// SuggestCategories must reproduce it from a plausible near-miss, so an
	// operator who armed a wait on the wrong string is pointed at the real one.
	var found bool
	for _, c := range SuggestCategories("grooming_report_created", 5) {
		if c == "grooming_report_recorded" {
			found = true
		}
	}
	if !found {
		t.Errorf("SuggestCategories(%q) = %v, want it to surface grooming_report_recorded",
			"grooming_report_created", SuggestCategories("grooming_report_created", 5))
	}
}

// TestKnownCategories_RunRejectedMissingCharter pins the E54.4 / #2236
// category: the charter admission gate writes it when it refuses a
// grooming run in a repo whose work-management conventions declare no
// charter. An unregistered category is un-awaitable — an operator could not
// arm fishhawk_await_audit on their own refusal — and
// categories_completeness_test.go's AST sweep would fail the build the
// moment the emit site lands.
// TestKnownCategories_GroomingMutationApplied pins E54.5 / #2237's per-mutation
// apply category: workmgmt.ApplyGrooming records one entry per SETTLED
// candidate — applied, failed AND skipped alike — so one category filter
// returns the whole apply. An unregistered category is un-awaitable and
// unfilterable via GET /v0/runs/{run_id}/audit?category=, and
// categories_completeness_test.go's AST sweep fails the build on a category
// backend code emits but the registry omits.
func TestKnownCategories_GroomingMutationApplied(t *testing.T) {
	if !IsKnownCategory("grooming_mutation_applied") {
		t.Fatal("grooming_mutation_applied is not in KnownCategories; fishhawk_await_audit would reject a wait armed on it")
	}
	// SuggestCategories must reproduce it from a plausible near-miss.
	var found bool
	for _, c := range SuggestCategories("grooming_mutation_recorded", 5) {
		if c == "grooming_mutation_applied" {
			found = true
		}
	}
	if !found {
		t.Errorf("SuggestCategories(%q) = %v, want it to surface grooming_mutation_applied",
			"grooming_mutation_recorded", SuggestCategories("grooming_mutation_recorded", 5))
	}
}

// TestKnownCategories_GroomingApplyCompleted pins E54.5 / #2237's once-per-apply
// summary category, carrying the applied/failed/skipped counts and entry ids.
func TestKnownCategories_GroomingApplyCompleted(t *testing.T) {
	if !IsKnownCategory("grooming_apply_completed") {
		t.Fatal("grooming_apply_completed is not in KnownCategories; fishhawk_await_audit would reject a wait armed on it")
	}
	var found bool
	for _, c := range SuggestCategories("grooming_apply_complete", 5) {
		if c == "grooming_apply_completed" {
			found = true
		}
	}
	if !found {
		t.Errorf("SuggestCategories(%q) = %v, want it to surface grooming_apply_completed",
			"grooming_apply_complete", SuggestCategories("grooming_apply_complete", 5))
	}
}

func TestKnownCategories_RunRejectedMissingCharter(t *testing.T) {
	if !IsKnownCategory("run_rejected_missing_charter") {
		t.Fatal("run_rejected_missing_charter is not in KnownCategories; fishhawk_await_audit would reject a wait armed on it")
	}
}

// TestKnownCategories_GroomingChurnFiltered pins E54.8 / #2240's churn-guard
// category. It is written once per guard pass on the grooming-report ingest
// path and carries #2240 AC1's visible "no changes proposed" outcome, so it is
// the ONE surface an operator arms fishhawk_await_audit on to learn what a
// grooming run actually proposes — as distinct from what the agent emitted. An
// unregistered category is un-awaitable and would 400 a
// GET /v0/runs/{run_id}/audit?category=grooming_churn_filtered.
func TestKnownCategories_GroomingChurnFiltered(t *testing.T) {
	if !IsKnownCategory("grooming_churn_filtered") {
		t.Fatal("grooming_churn_filtered is not in KnownCategories; fishhawk_await_audit would reject a wait armed on it")
	}
	var found bool
	for _, c := range SuggestCategories("grooming_churn_filter", 5) {
		if c == "grooming_churn_filtered" {
			found = true
		}
	}
	if !found {
		t.Errorf("SuggestCategories(%q) = %v, want it to surface grooming_churn_filtered",
			"grooming_churn_filter", SuggestCategories("grooming_churn_filter", 5))
	}
}

// TestKnownCategories_CampaignGroomingSourceResolved pins the E54.6 / #2238
// registration of campaign_grooming_source_resolved — the audit copy of a
// grooming-sourced campaign's provenance (source run/stage/artifact, report
// content hash, ordered refs, exclusions, limit, acknowledged supersession).
//
// It matters even though the durable record is the campaigns.grooming_source
// column: an unregistered category is UN-AWAITABLE, so
// GET /v0/runs/{run_id}/audit?category=campaign_grooming_source_resolved would
// 400 and an operator could not watch for the provenance row at all.
func TestKnownCategories_CampaignGroomingSourceResolved(t *testing.T) {
	if !IsKnownCategory("campaign_grooming_source_resolved") {
		t.Fatal("campaign_grooming_source_resolved is not in KnownCategories; an audit wait armed on it would be rejected")
	}
	var found bool
	for _, c := range SuggestCategories("campaign_grooming_source", 5) {
		if c == "campaign_grooming_source_resolved" {
			found = true
		}
	}
	if !found {
		t.Errorf("SuggestCategories(%q) = %v, want it to surface campaign_grooming_source_resolved",
			"campaign_grooming_source", SuggestCategories("campaign_grooming_source", 5))
	}
}
