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

// pending is a not-completed attempt in suite 1 that started 10m before
// maturity.
func pending(id int64, name, status string) githubclient.CheckRunSummary {
	return githubclient.CheckRunSummary{ID: id, Name: name, Status: status, StartedAt: at(-10 * time.Minute), CheckSuiteID: 1}
}

// TestClassifyMergeGateCI pins the merge-gate variant (docs/adr/112 A3.1,
// #4780), one row per A3 check-evidence item: per suite the LATEST attempt
// decides whether or not it completed, cancelled is red, any failing suite
// fails the name, and red beats pending across suites and names.
func TestClassifyMergeGateCI(t *testing.T) {
	cases := []struct {
		name    string
		snap    *run.RequiredChecksSnapshot
		checks  []githubclient.CheckRunSummary
		want    string
		reason  string
		failed  []string
		missing map[string]string
	}{
		// Only the newer in_progress attempt (id 11) distinguishes this from
		// the completed success (id 10) in the SAME suite.
		{name: "older success newer in_progress in one suite is pending", snap: snap("CI Pass"),
			checks: []githubclient.CheckRunSummary{done(10, "CI Pass", "success", -time.Hour), pending(11, "CI Pass", "in_progress")},
			want:   CIConclusionMissing, reason: MissingRequiredCheckPending, missing: map[string]string{"CI Pass": "pending"}},
		{name: "older success newer queued in one suite is pending", snap: snap("CI Pass"),
			checks: []githubclient.CheckRunSummary{done(10, "CI Pass", "success", -time.Hour),
				{ID: 11, Name: "CI Pass", Status: "queued", CheckSuiteID: 1}},
			want: CIConclusionMissing, reason: MissingRequiredCheckPending, missing: map[string]string{"CI Pass": "pending"}},
		{name: "older failure newer success in one suite is green", snap: snap("CI Pass"),
			checks: []githubclient.CheckRunSummary{done(10, "CI Pass", "failure", -2*time.Hour), done(11, "CI Pass", "success", -time.Hour)},
			want:   CIConclusionGreen},
		// The red suite (2, check-run id 3) is OLDER than the green one (1,
		// id 5): an independent failing suite still fails the name.
		{name: "independent older failure suite beside a green suite is red", snap: snap("CI Pass"),
			checks: []githubclient.CheckRunSummary{done(5, "CI Pass", "success", -time.Hour), inSuite(done(3, "CI Pass", "failure", -2*time.Hour), 2)},
			want:   CIConclusionRed, failed: []string{"CI Pass"}},
		{name: "completed cancelled latest attempt is red", snap: snap("CI Pass"),
			checks: []githubclient.CheckRunSummary{done(10, "CI Pass", "success", -2*time.Hour), done(11, "CI Pass", "cancelled", -time.Hour)},
			want:   CIConclusionRed, failed: []string{"CI Pass"}},
		{name: "concurrency-cancelled sibling suite beside a green suite is red", snap: snap("CI Pass"),
			checks: []githubclient.CheckRunSummary{done(5, "CI Pass", "success", -time.Hour), inSuite(done(7, "CI Pass", "cancelled", -time.Hour), 2)},
			want:   CIConclusionRed, failed: []string{"CI Pass"}},
		{name: "completed stale latest attempt is missing", snap: snap("CI Pass"),
			checks: []githubclient.CheckRunSummary{done(10, "CI Pass", "success", -2*time.Hour), done(11, "CI Pass", "stale", -time.Hour)},
			want:   CIConclusionMissing, reason: MissingRequiredCheckUnconcluded, missing: map[string]string{"CI Pass": "unconcluded"}},
		{name: "unknown conclusion is missing", snap: snap("CI Pass"),
			checks: []githubclient.CheckRunSummary{done(10, "CI Pass", "something_new", -time.Hour)},
			want:   CIConclusionMissing, reason: MissingRequiredCheckUnconcluded},
		{name: "empty conclusion is missing", snap: snap("CI Pass"),
			checks: []githubclient.CheckRunSummary{done(10, "CI Pass", "", -time.Hour)},
			want:   CIConclusionMissing, reason: MissingRequiredCheckUnconcluded},
		{name: "pending suite beside a failed suite is red", snap: snap("CI Pass"),
			checks: []githubclient.CheckRunSummary{pending(9, "CI Pass", "in_progress"), inSuite(done(3, "CI Pass", "failure", -time.Hour), 2)},
			want:   CIConclusionRed, failed: []string{"CI Pass"}},
		{name: "pending name beside a failed name is red", snap: snap("lint", "test"),
			checks: []githubclient.CheckRunSummary{pending(9, "lint", "in_progress"), done(3, "test", "failure", -time.Hour)},
			want:   CIConclusionRed, failed: []string{"test"}, missing: map[string]string{"lint": "pending"}},
		{name: "zero runs on a populated snapshot is missing", snap: snap("CI Pass"),
			want: CIConclusionMissing, reason: MissingRequiredCheckAbsent, missing: map[string]string{"CI Pass": "absent"}},
		{name: "zero runs on an empty snapshot is missing", snap: snap(),
			want: CIConclusionMissing, reason: MissingNoChecksReported},
		// No maturity cut-off: an attempt whose forge timestamps lie after
		// now (clock skew) still decides.
		{name: "latest attempt started after now still decides", snap: snap("CI Pass"),
			checks: []githubclient.CheckRunSummary{done(10, "CI Pass", "success", -time.Hour),
				{ID: 11, Name: "CI Pass", Status: "in_progress", StartedAt: at(time.Minute), CheckSuiteID: 1}},
			want: CIConclusionMissing, reason: MissingRequiredCheckPending},
		{name: "merge gate check excluded", snap: snap("CI Pass", auditcheckpublisher.CheckName),
			checks: []githubclient.CheckRunSummary{done(1, "CI Pass", "success", -time.Hour)},
			want:   CIConclusionGreen},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := ClassifyMergeGateCI(tc.snap, tc.checks, false, ciMatures)
			assertVerdict(t, v, tc.want, tc.reason)
			if tc.failed != nil && !reflect.DeepEqual(v.FailedChecks, tc.failed) {
				t.Fatalf("failed = %v, want %v", v.FailedChecks, tc.failed)
			}
			for name, reason := range tc.missing {
				if v.MissingCheckReasons[name] != reason {
					t.Fatalf("missing reasons = %v, want %s: %s", v.MissingCheckReasons, name, reason)
				}
			}
		})
	}

	t.Run("nil snapshot and truncation keep ClassifyMergeCI's verdict order", func(t *testing.T) {
		red := []githubclient.CheckRunSummary{done(1, "CI Pass", "cancelled", -time.Hour)}
		assertVerdict(t, ClassifyMergeGateCI(nil, red, false, ciMatures), CIConclusionMissing, MissingRequiredChecksUnknown)
		assertVerdict(t, ClassifyMergeGateCI(snap("CI Pass"), red, true, ciMatures), CIConclusionMissing, MissingChecksTruncated)
	})
}

// TestClassifyMergeCI_UnchangedByGateRules pins that the gate rules do not
// leak into ClassifyMergeCI (E82.2's post-merge observation): the same
// cancelled and newer-in_progress fixtures keep missing (unconcluded) and
// green there.
func TestClassifyMergeCI_UnchangedByGateRules(t *testing.T) {
	cancelled := []githubclient.CheckRunSummary{done(10, "CI Pass", "success", -2*time.Hour), done(11, "CI Pass", "cancelled", -time.Hour)}
	assertVerdict(t, ClassifyMergeCI(snap("CI Pass"), cancelled, false, ciMatures), CIConclusionMissing, MissingRequiredCheckUnconcluded)
	assertVerdict(t, ClassifyMergeGateCI(snap("CI Pass"), cancelled, false, ciMatures), CIConclusionRed, "")

	newer := []githubclient.CheckRunSummary{done(10, "CI Pass", "success", -time.Hour), pending(11, "CI Pass", "in_progress")}
	assertVerdict(t, ClassifyMergeCI(snap("CI Pass"), newer, false, ciMatures), CIConclusionGreen, "")
	assertVerdict(t, ClassifyMergeGateCI(snap("CI Pass"), newer, false, ciMatures), CIConclusionMissing, MissingRequiredCheckPending)

	// The maturity cut-off still applies to ClassifyMergeCI.
	late := []githubclient.CheckRunSummary{done(1, "CI Pass", "success", 2*time.Hour)}
	assertVerdict(t, ClassifyMergeCI(snap("CI Pass"), late, false, ciMatures), CIConclusionMissing, MissingNotConcludedByMaturity)
}
