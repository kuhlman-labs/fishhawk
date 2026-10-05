package mergeoutcome

import (
	"reflect"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/auditcheckpublisher"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// ciMatures is the maturity instant every classifier test fixes against.
var ciMatures = time.Date(2026, 9, 1, 18, 0, 0, 0, time.UTC)

func at(d time.Duration) *time.Time {
	t := ciMatures.Add(d)
	return &t
}

// done is a check-run attempt in suite 1 that started 30m before and
// completed `completedRel` relative to maturity.
func done(id int64, name, conclusion string, completedRel time.Duration) githubclient.CheckRunSummary {
	return githubclient.CheckRunSummary{ID: id, Name: name, Status: "completed", Conclusion: conclusion,
		StartedAt: at(completedRel - 30*time.Minute), CompletedAt: at(completedRel), CheckSuiteID: 1}
}

func inSuite(c githubclient.CheckRunSummary, suite int64) githubclient.CheckRunSummary {
	c.CheckSuiteID = suite
	return c
}

func snap(contexts ...string) *run.RequiredChecksSnapshot {
	return &run.RequiredChecksSnapshot{Contexts: contexts, Sources: []string{"ruleset:1"}}
}

func assertVerdict(t *testing.T, v CIVerdict, conclusion, reason string) {
	t.Helper()
	if v.Conclusion != conclusion || v.MissingReason != reason {
		t.Fatalf("verdict = %s/%q, want %s/%q (full: %+v)", v.Conclusion, v.MissingReason, conclusion, reason, v)
	}
}

// This repository's real shape (merge commit 8ea092b5): the ruleset requires
// [CI Pass, fishhawk_audit_complete] and only CI Pass runs on the merge commit.
func TestClassifyMergeCI_ExcludesFishhawkAuditComplete(t *testing.T) {
	v := ClassifyMergeCI(snap("CI Pass", auditcheckpublisher.CheckName),
		[]githubclient.CheckRunSummary{done(1, "CI Pass", "success", -time.Hour)}, false, ciMatures)
	assertVerdict(t, v, CIConclusionGreen, "")
	if !reflect.DeepEqual(v.ExcludedChecks, []string{"fishhawk_audit_complete"}) ||
		!reflect.DeepEqual(v.RequiredChecks, []string{"CI Pass"}) || v.RequiredChecksSource != "run_snapshot" {
		t.Fatalf("excluded = %v required = %v source = %q", v.ExcludedChecks, v.RequiredChecks, v.RequiredChecksSource)
	}
}

func TestClassifyMergeCI_SnapshotOnlyMergeGate_FallsBackToObserved(t *testing.T) {
	v := ClassifyMergeCI(snap(auditcheckpublisher.CheckName),
		[]githubclient.CheckRunSummary{done(1, "build", "failure", -time.Hour)}, false, ciMatures)
	assertVerdict(t, v, CIConclusionRed, "")
	if !reflect.DeepEqual(v.FailedChecks, []string{"build"}) {
		t.Fatalf("failed = %v, want [build]", v.FailedChecks)
	}
}

func TestClassifyMergeCI_NilSnapshot_IsMissing(t *testing.T) {
	v := ClassifyMergeCI(nil, []githubclient.CheckRunSummary{done(1, "CI Pass", "success", -time.Hour)}, false, ciMatures)
	assertVerdict(t, v, CIConclusionMissing, MissingRequiredChecksUnknown)
	if v.RequiredChecksSource != "run_snapshot_absent" || !reflect.DeepEqual(v.PassedChecks, []string{"CI Pass"}) {
		t.Fatalf("source = %q passed = %v", v.RequiredChecksSource, v.PassedChecks)
	}
}

func TestClassifyMergeCI_FailedRequired_IsRed(t *testing.T) {
	for _, c := range []string{"failure", "timed_out", "action_required", "startup_failure"} {
		v := ClassifyMergeCI(snap("CI Pass"), []githubclient.CheckRunSummary{done(1, "CI Pass", c, -time.Hour)}, false, ciMatures)
		if v.Conclusion != CIConclusionRed || !reflect.DeepEqual(v.FailedChecks, []string{"CI Pass"}) {
			t.Errorf("%s: verdict = %+v, want red", c, v)
		}
	}
}

func TestClassifyMergeCI_PassConclusions_AreGreen(t *testing.T) {
	for _, c := range []string{"success", "neutral", "skipped"} {
		v := ClassifyMergeCI(snap("CI Pass"), []githubclient.CheckRunSummary{done(1, "CI Pass", c, -time.Hour)}, false, ciMatures)
		if v.Conclusion != CIConclusionGreen {
			t.Errorf("%s: verdict = %+v, want green", c, v)
		}
	}
}

func TestClassifyMergeCI_RedBeatsMissing(t *testing.T) {
	v := ClassifyMergeCI(snap("lint", "test"),
		[]githubclient.CheckRunSummary{done(1, "test", "failure", -time.Hour)}, false, ciMatures)
	assertVerdict(t, v, CIConclusionRed, "")
	if !reflect.DeepEqual(v.MissingChecks, []string{"lint"}) {
		t.Fatalf("missing = %v, want [lint]", v.MissingChecks)
	}
}

func TestClassifyMergeCI_AbsentRequired_IsMissing(t *testing.T) {
	v := ClassifyMergeCI(snap("CI Pass", "e2e"),
		[]githubclient.CheckRunSummary{done(1, "CI Pass", "success", -time.Hour)}, false, ciMatures)
	assertVerdict(t, v, CIConclusionMissing, MissingRequiredCheckAbsent)
	if v.MissingCheckReasons["e2e"] != "absent" || !reflect.DeepEqual(v.MissingChecks, []string{"e2e"}) {
		t.Fatalf("missing = %v reasons = %v", v.MissingChecks, v.MissingCheckReasons)
	}
}

func TestClassifyMergeCI_UnconcludedRequired_IsMissing(t *testing.T) {
	cases := map[string]githubclient.CheckRunSummary{
		"cancelled":   done(1, "CI Pass", "cancelled", -time.Hour),
		"stale":       done(1, "CI Pass", "stale", -time.Hour),
		"unknown":     done(1, "CI Pass", "something_new", -time.Hour),
		"empty":       done(1, "CI Pass", "", -time.Hour),
		"queued":      {ID: 1, Name: "CI Pass", Status: "queued", CheckSuiteID: 1},
		"in_progress": {ID: 1, Name: "CI Pass", Status: "in_progress", StartedAt: at(-time.Hour), CheckSuiteID: 1},
	}
	wantReason := map[string]string{
		"cancelled": MissingRequiredCheckUnconcluded, "stale": MissingRequiredCheckUnconcluded,
		"unknown": MissingRequiredCheckUnconcluded, "empty": MissingRequiredCheckUnconcluded,
		"queued": MissingNotConcludedByMaturity, "in_progress": MissingNotConcludedByMaturity,
	}
	for name, c := range cases {
		v := ClassifyMergeCI(snap("CI Pass"), []githubclient.CheckRunSummary{c}, false, ciMatures)
		if v.Conclusion != CIConclusionMissing || v.MissingReason != wantReason[name] {
			t.Errorf("%s: verdict = %s/%q, want missing/%q", name, v.Conclusion, v.MissingReason, wantReason[name])
		}
	}
}

func TestClassifyMergeCI_EmptySnapshot_NoChecks_IsMissing(t *testing.T) {
	v := ClassifyMergeCI(snap(), nil, false, ciMatures)
	assertVerdict(t, v, CIConclusionMissing, MissingNoChecksReported)
	if v.RequiredChecksSource != "run_snapshot_empty" {
		t.Fatalf("source = %q", v.RequiredChecksSource)
	}
}

func TestClassifyMergeCI_EmptySnapshot_ObservedAllPass_IsGreen(t *testing.T) {
	v := ClassifyMergeCI(snap(), []githubclient.CheckRunSummary{
		done(1, "build", "success", -time.Hour), done(2, "lint", "neutral", -time.Hour),
	}, false, ciMatures)
	assertVerdict(t, v, CIConclusionGreen, "")
	if !reflect.DeepEqual(v.PassedChecks, []string{"build", "lint"}) || v.ChecksObserved != 2 {
		t.Fatalf("passed = %v observed = %d", v.PassedChecks, v.ChecksObserved)
	}
}

func TestClassifyMergeCI_EmptySnapshot_Truncated_NeverGreen(t *testing.T) {
	v := ClassifyMergeCI(snap(), []githubclient.CheckRunSummary{done(1, "build", "success", -time.Hour)}, true, ciMatures)
	assertVerdict(t, v, CIConclusionMissing, MissingChecksTruncated)
	if !v.ChecksTruncated {
		t.Fatal("ChecksTruncated = false, want true")
	}
}

func TestClassifyMergeCI_PopulatedSnapshot_Truncated_NeverGreen(t *testing.T) {
	v := ClassifyMergeCI(snap("CI Pass"), []githubclient.CheckRunSummary{done(1, "CI Pass", "success", -time.Hour)}, true, ciMatures)
	assertVerdict(t, v, CIConclusionMissing, MissingChecksTruncated)
}

// Operator condition 3: a check that failed before maturity and was re-run
// green AFTER maturity records red — the conclusion is fixed at maturity.
func TestClassifyMergeCI_FailedBeforeMaturityRerunGreenAfter_IsRed(t *testing.T) {
	v := ClassifyMergeCI(snap("CI Pass"), []githubclient.CheckRunSummary{
		done(1, "CI Pass", "failure", -2*time.Hour),
		{ID: 2, Name: "CI Pass", Status: "completed", Conclusion: "success",
			StartedAt: at(time.Hour), CompletedAt: at(90 * time.Minute), CheckSuiteID: 1},
	}, false, ciMatures)
	assertVerdict(t, v, CIConclusionRed, "")
}

// The re-run started BEFORE maturity but completed after: the latest attempt
// completed by maturity (the failure) still decides.
func TestClassifyMergeCI_RerunCompletedAfterMaturity_EarlierFailureDecides(t *testing.T) {
	v := ClassifyMergeCI(snap("CI Pass"), []githubclient.CheckRunSummary{
		done(1, "CI Pass", "failure", -2*time.Hour),
		{ID: 2, Name: "CI Pass", Status: "completed", Conclusion: "success",
			StartedAt: at(-10 * time.Minute), CompletedAt: at(20 * time.Minute), CheckSuiteID: 1},
	}, false, ciMatures)
	assertVerdict(t, v, CIConclusionRed, "")
}

// Operator condition 3: a check that completed only after maturity records
// missing (not_concluded_by_maturity) even though it succeeded.
func TestClassifyMergeCI_CompletedOnlyAfterMaturity_IsMissing(t *testing.T) {
	v := ClassifyMergeCI(snap("CI Pass"), []githubclient.CheckRunSummary{
		{ID: 1, Name: "CI Pass", Status: "completed", Conclusion: "success",
			StartedAt: at(-time.Hour), CompletedAt: at(time.Hour), CheckSuiteID: 1},
	}, false, ciMatures)
	assertVerdict(t, v, CIConclusionMissing, MissingNotConcludedByMaturity)
	if v.MissingCheckReasons["CI Pass"] != "not_concluded_by_maturity" {
		t.Fatalf("reasons = %v", v.MissingCheckReasons)
	}
}

// A required check whose only attempt STARTED after maturity is ignored, so
// it is not concluded by maturity.
func TestClassifyMergeCI_StartedOnlyAfterMaturity_IsMissing(t *testing.T) {
	v := ClassifyMergeCI(snap("CI Pass"), []githubclient.CheckRunSummary{done(1, "CI Pass", "success", 2*time.Hour)}, false, ciMatures)
	assertVerdict(t, v, CIConclusionMissing, MissingNotConcludedByMaturity)
}

// On the observed-set fallback an attempt started after maturity does not
// enter the set at all: the only pre-maturity check decides.
func TestClassifyMergeCI_EmptySnapshot_IgnoresAttemptsStartedAfterMaturity(t *testing.T) {
	v := ClassifyMergeCI(snap(), []githubclient.CheckRunSummary{
		done(1, "build", "success", -time.Hour),
		done(2, "nightly", "failure", 3*time.Hour),
	}, false, ciMatures)
	assertVerdict(t, v, CIConclusionGreen, "")
	if !reflect.DeepEqual(v.PassedChecks, []string{"build"}) || len(v.FailedChecks) != 0 {
		t.Fatalf("passed = %v failed = %v", v.PassedChecks, v.FailedChecks)
	}
}

// A flaky failure re-run green BEFORE maturity: the most recent attempt in the
// suite decides, so it is green.
func TestClassifyMergeCI_RerunGreenBeforeMaturity_IsGreen(t *testing.T) {
	v := ClassifyMergeCI(snap("CI Pass"), []githubclient.CheckRunSummary{
		done(2, "CI Pass", "success", -time.Hour),
		done(1, "CI Pass", "failure", -2*time.Hour),
	}, false, ciMatures)
	assertVerdict(t, v, CIConclusionGreen, "")
}

// Operator condition 3 duplicate-name rule: the same name in two suites
// (another workflow / app) — any failed suite fails the name.
func TestClassifyMergeCI_DuplicateName_AnyFailedFails(t *testing.T) {
	v := ClassifyMergeCI(snap("test"), []githubclient.CheckRunSummary{
		done(5, "test", "success", -time.Hour),
		inSuite(done(3, "test", "failure", -2*time.Hour), 2),
	}, false, ciMatures)
	assertVerdict(t, v, CIConclusionRed, "")
}

// ...else any missing suite makes it missing.
func TestClassifyMergeCI_DuplicateName_AnyMissingMissing(t *testing.T) {
	v := ClassifyMergeCI(snap("test"), []githubclient.CheckRunSummary{
		done(5, "test", "success", -time.Hour),
		inSuite(done(6, "test", "cancelled", -time.Hour), 2),
	}, false, ciMatures)
	assertVerdict(t, v, CIConclusionMissing, MissingRequiredCheckUnconcluded)
}

// A duplicate suite whose attempts all started after maturity is ignored.
func TestClassifyMergeCI_DuplicateName_LateSuiteIgnored(t *testing.T) {
	v := ClassifyMergeCI(snap("test"), []githubclient.CheckRunSummary{
		done(5, "test", "success", -time.Hour),
		inSuite(done(9, "test", "failure", 2*time.Hour), 2),
	}, false, ciMatures)
	assertVerdict(t, v, CIConclusionGreen, "")
}

func TestClassifyMergeCI_DuplicateContextsDeduped(t *testing.T) {
	v := ClassifyMergeCI(snap("CI Pass", "CI Pass"), []githubclient.CheckRunSummary{done(1, "CI Pass", "success", -time.Hour)}, false, ciMatures)
	if !reflect.DeepEqual(v.RequiredChecks, []string{"CI Pass"}) {
		t.Fatalf("required = %v, want one CI Pass", v.RequiredChecks)
	}
}
