package upkeep_test

import (
	"fmt"
	"reflect"
	"sort"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/upkeep"
)

func failed(tree, tail string) upkeep.VerifyAttempt {
	return upkeep.VerifyAttempt{TreeSHA: tree, Outcome: upkeep.VerifyOutcomeFailed, OutputTail: tail}
}

func passed(tree string) upkeep.VerifyAttempt {
	return upkeep.VerifyAttempt{TreeSHA: tree, Outcome: upkeep.VerifyOutcomePassed}
}

const failTestX = "=== RUN   TestX\n    x_test.go:12: boom\n--- FAIL: TestX (0.01s)\nFAIL\nFAIL\tgithub.com/example/pkg\t0.02s\n"

// TestAggregateFlakes_SameTreeFailThenPass (issue AC2): a failed verify
// followed by a passed one on the same non-empty tree within one stage yields
// one flake citing that run and stage.
func TestAggregateFlakes_SameTreeFailThenPass(t *testing.T) {
	stages := []upkeep.FlakeStage{{
		RunID:    "run-1",
		StageID:  "stage-1",
		Attempts: []upkeep.VerifyAttempt{failed("tree-a", failTestX), passed("tree-a")},
	}}
	got := upkeep.AggregateFlakes(stages)
	want := []upkeep.Flake{{
		Subject:     "TestX",
		Occurrences: 1,
		Refs:        []upkeep.FlakeRef{{RunID: "run-1", StageID: "stage-1"}},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("AggregateFlakes = %+v, want %+v", got, want)
	}
}

// assertNoFlakes fails unless stages aggregate to a non-nil empty slice.
func assertNoFlakes(t *testing.T, stages []upkeep.FlakeStage) {
	t.Helper()
	if got := upkeep.AggregateFlakes(stages); got == nil || len(got) != 0 {
		t.Errorf("AggregateFlakes = %#v, want a non-nil empty slice", got)
	}
}

// TestAggregateFlakes_DifferentTreeNone: a failure followed by a pass on a
// DIFFERENT tree is a fix, not a flake.
//
// Counterfactual: dropping the TreeSHA equality in flakyFailure reports TestX
// — the trees differ, so only that check suppresses it — RED.
func TestAggregateFlakes_DifferentTreeNone(t *testing.T) {
	assertNoFlakes(t, []upkeep.FlakeStage{{
		RunID: "run-1", StageID: "stage-1",
		Attempts: []upkeep.VerifyAttempt{failed("tree-a", failTestX), passed("tree-b")},
	}})
}

// TestAggregateFlakes_EmptyTreeNone: an attempt with no recorded tree cannot
// prove the pass verified the same tree.
//
// Counterfactual: dropping the non-empty TreeSHA check makes "" == "" pair
// the two attempts and reports TestX — RED.
func TestAggregateFlakes_EmptyTreeNone(t *testing.T) {
	assertNoFlakes(t, []upkeep.FlakeStage{{
		RunID: "run-1", StageID: "stage-1",
		Attempts: []upkeep.VerifyAttempt{failed("", failTestX), passed("")},
	}})
}

// TestAggregateFlakes_PassThenFailNone: a pass followed by a failure on the
// same tree is a regression signal, not a flaky failure that recovered.
//
// Counterfactual: scanning every attempt instead of only j > i pairs the
// failure with the EARLIER pass and reports TestX — RED.
func TestAggregateFlakes_PassThenFailNone(t *testing.T) {
	assertNoFlakes(t, []upkeep.FlakeStage{{
		RunID: "run-1", StageID: "stage-1",
		Attempts: []upkeep.VerifyAttempt{passed("tree-a"), failed("tree-a", failTestX)},
	}})
}

// TestAggregateFlakes_OnlyFailedThenPassedPairs: only a FAILED attempt can be
// a flaky failure and only a PASSED one can redeem it — a skipped attempt is
// neither.
//
// Counterfactuals: dropping the later-attempt Outcome == passed check pairs
// the failure with the skipped attempt (stage-1); dropping the Outcome ==
// failed check counts the skipped attempt redeemed by a pass (stage-2) — each
// RED.
func TestAggregateFlakes_OnlyFailedThenPassedPairs(t *testing.T) {
	skipped := upkeep.VerifyAttempt{TreeSHA: "tree-a", Outcome: upkeep.VerifyOutcomeSkipped, OutputTail: "--- FAIL: TestSkipTail\n"}
	assertNoFlakes(t, []upkeep.FlakeStage{
		{RunID: "run-1", StageID: "stage-1", Attempts: []upkeep.VerifyAttempt{failed("tree-a", failTestX), skipped}},
		{RunID: "run-1", StageID: "stage-2", Attempts: []upkeep.VerifyAttempt{skipped, passed("tree-a")}},
	})
}

// TestAggregateFlakes_Subjects pins test-name extraction from one failed
// attempt's tail (approval condition 5).
//
// Counterfactuals: dropping the whole-name charset check turns TestBad-Name
// into subject "TestBad-Name" (RED on the whole-name rows); dropping the
// subtest cut turns TestA/case_1 into "TestA/case_1", which then fails the
// charset and yields unnamed (RED on the collapse row).
func TestAggregateFlakes_Subjects(t *testing.T) {
	for _, tc := range []struct {
		name string
		tail string
		want []string
	}{
		{
			name: "subtest collapses to its parent",
			tail: "    --- FAIL: TestA/case_1 (0.00s)\n    --- FAIL: TestA/case-2 (0.00s)\n--- FAIL: TestA (0.01s)\n",
			want: []string{"TestA"},
		},
		{
			name: "no FAIL line is unnamed",
			tail: "panic: runtime error: index out of range\nexit status 2\n",
			want: []string{upkeep.FlakeSubjectUnnamed},
		},
		{
			name: "empty tail is unnamed",
			tail: "",
			want: []string{upkeep.FlakeSubjectUnnamed},
		},
		{
			name: "whole name outside the charset is unnamed, never its prefix",
			tail: "--- FAIL: TestBad-Name (0.00s)\n",
			want: []string{upkeep.FlakeSubjectUnnamed},
		},
		{
			name: "non-charset parent of a subtest is unnamed",
			tail: "    --- FAIL: TestBad-Name/sub (0.00s)\n",
			want: []string{upkeep.FlakeSubjectUnnamed},
		},
		{
			name: "backtick name is unnamed",
			tail: "--- FAIL: Test`rm` (0.00s)\n",
			want: []string{upkeep.FlakeSubjectUnnamed},
		},
		{
			name: "two tests, one unnamed, all deduplicated",
			tail: "--- FAIL: TestB (0.00s)\n--- FAIL: TestC (0.00s)\n--- FAIL: TestB (0.00s)\n--- FAIL: Tést (0.00s)\n",
			want: []string{"TestB", "TestC", upkeep.FlakeSubjectUnnamed},
		},
		{
			name: "FAIL token must start a line",
			tail: "note: saw --- FAIL: TestQuoted in a log\n",
			want: []string{upkeep.FlakeSubjectUnnamed},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := upkeep.AggregateFlakes([]upkeep.FlakeStage{{
				RunID: "run-1", StageID: "stage-1",
				Attempts: []upkeep.VerifyAttempt{failed("tree-a", tc.tail), passed("tree-a")},
			}})
			var subjects []string
			for _, f := range got {
				if f.Occurrences != 1 {
					t.Errorf("subject %q Occurrences = %d, want 1 (deduplicated within the attempt)", f.Subject, f.Occurrences)
				}
				subjects = append(subjects, f.Subject)
			}
			// AggregateFlakes sorts equal-occurrence subjects ascending.
			want := append([]string(nil), tc.want...)
			sort.Strings(want)
			if !reflect.DeepEqual(subjects, want) {
				t.Errorf("subjects = %q, want %q", subjects, want)
			}
		})
	}
}

// TestAggregateFlakes_InfraRetries: absorbed infra-flake retries add that many
// occurrences to verify-gate:infra, with no failed/passed pair required.
//
// Counterfactual: dropping the InfraRetries add leaves no infra subject — RED.
func TestAggregateFlakes_InfraRetries(t *testing.T) {
	got := upkeep.AggregateFlakes([]upkeep.FlakeStage{
		{RunID: "run-1", StageID: "stage-1", InfraRetries: 2},
		{RunID: "run-2", StageID: "stage-2", InfraRetries: 1, Attempts: []upkeep.VerifyAttempt{passed("tree-a")}},
		{RunID: "run-3", StageID: "stage-3", InfraRetries: 0},
	})
	want := []upkeep.Flake{{
		Subject:     upkeep.FlakeSubjectInfra,
		Occurrences: 3,
		Refs: []upkeep.FlakeRef{
			{RunID: "run-1", StageID: "stage-1"},
			{RunID: "run-2", StageID: "stage-2"},
		},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("AggregateFlakes = %+v, want %+v", got, want)
	}
}

// TestAggregateFlakes_CrossStageAggregation: occurrences sum per subject, each
// counted failure counts once, refs are distinct and sorted, and output is
// Occurrences descending then Subject ascending — independent of stage order.
func TestAggregateFlakes_CrossStageAggregation(t *testing.T) {
	failY := "--- FAIL: TestY (0.00s)\n"
	failZ := "--- FAIL: TestZ (0.00s)\n"
	stages := []upkeep.FlakeStage{
		{RunID: "run-b", StageID: "s1", Attempts: []upkeep.VerifyAttempt{
			// Two failures on tree-a before its pass: two occurrences.
			failed("tree-a", failTestX), failed("tree-a", failTestX), passed("tree-a"),
		}},
		{RunID: "run-a", StageID: "s2", Attempts: []upkeep.VerifyAttempt{
			failed("tree-c", failTestX), passed("tree-c"),
			failed("tree-d", failY), passed("tree-d"),
		}},
		{RunID: "run-a", StageID: "s1", Attempts: []upkeep.VerifyAttempt{
			failed("tree-e", failZ), passed("tree-e"),
		}},
		{RunID: "run-c", StageID: "s9", InfraRetries: 1},
		{RunID: "run-a", StageID: "s3", Attempts: []upkeep.VerifyAttempt{
			failed("tree-f", failTestX), passed("tree-f"),
		}},
	}
	want := []upkeep.Flake{
		{Subject: "TestX", Occurrences: 4, Refs: []upkeep.FlakeRef{
			{RunID: "run-a", StageID: "s2"}, {RunID: "run-a", StageID: "s3"}, {RunID: "run-b", StageID: "s1"},
		}},
		{Subject: "TestY", Occurrences: 1, Refs: []upkeep.FlakeRef{{RunID: "run-a", StageID: "s2"}}},
		{Subject: "TestZ", Occurrences: 1, Refs: []upkeep.FlakeRef{{RunID: "run-a", StageID: "s1"}}},
		{Subject: upkeep.FlakeSubjectInfra, Occurrences: 1, Refs: []upkeep.FlakeRef{{RunID: "run-c", StageID: "s9"}}},
	}
	if got := upkeep.AggregateFlakes(stages); !reflect.DeepEqual(got, want) {
		t.Errorf("AggregateFlakes =\n%+v\nwant\n%+v", got, want)
	}
	reversed := make([]upkeep.FlakeStage, len(stages))
	for i, s := range stages {
		reversed[len(stages)-1-i] = s
	}
	for i := 0; i < 20; i++ {
		if got := upkeep.AggregateFlakes(reversed); !reflect.DeepEqual(got, want) {
			t.Fatalf("reversed stage order, call %d:\n%+v\nwant\n%+v", i, got, want)
		}
	}
}

// TestAggregateFlakes_RefCap: a subject seen in more than FlakeMaxRefs stages
// keeps the first FlakeMaxRefs refs in (RunID, StageID) order and counts the
// rest in OmittedRefs, while Occurrences still counts every stage.
//
// Counterfactual: dropping the cap returns all 25 refs with OmittedRefs 0 —
// RED.
func TestAggregateFlakes_RefCap(t *testing.T) {
	var stages []upkeep.FlakeStage
	for i := 24; i >= 0; i-- {
		stages = append(stages, upkeep.FlakeStage{
			RunID: fmt.Sprintf("run-%02d", i), StageID: "stage",
			Attempts: []upkeep.VerifyAttempt{failed("t", failTestX), passed("t")},
		})
	}
	got := upkeep.AggregateFlakes(stages)
	if len(got) != 1 {
		t.Fatalf("want one subject, got %+v", got)
	}
	f := got[0]
	if f.Occurrences != 25 {
		t.Errorf("Occurrences = %d, want 25", f.Occurrences)
	}
	if len(f.Refs) != upkeep.FlakeMaxRefs || f.OmittedRefs != 25-upkeep.FlakeMaxRefs {
		t.Fatalf("len(Refs) = %d OmittedRefs = %d, want %d and %d", len(f.Refs), f.OmittedRefs, upkeep.FlakeMaxRefs, 25-upkeep.FlakeMaxRefs)
	}
	for i, r := range f.Refs {
		if want := fmt.Sprintf("run-%02d", i); r.RunID != want {
			t.Errorf("Refs[%d].RunID = %q, want %q", i, r.RunID, want)
		}
	}
}

// TestCapFlakeSubjects (approval condition 4): more than FlakeMaxSubjects
// subjects are cut to the cap with the remainder counted; at or under the cap
// nothing is dropped.
//
// Counterfactual: CapFlakeSubjects returning its input unchanged keeps 35
// subjects with 0 omitted — RED.
func TestCapFlakeSubjects(t *testing.T) {
	var stages []upkeep.FlakeStage
	for i := 0; i < 35; i++ {
		stages = append(stages, upkeep.FlakeStage{
			RunID: "run", StageID: fmt.Sprintf("stage-%02d", i),
			Attempts: []upkeep.VerifyAttempt{failed("t", fmt.Sprintf("--- FAIL: Test%02d\n", i)), passed("t")},
		})
	}
	// Test07 is the most frequent, so it must survive the cut at position 0.
	stages = append(stages, upkeep.FlakeStage{RunID: "run2", StageID: "s",
		Attempts: []upkeep.VerifyAttempt{failed("u", "--- FAIL: Test07\n"), passed("u")}})

	all := upkeep.AggregateFlakes(stages)
	kept, omitted := upkeep.CapFlakeSubjects(all)
	if len(kept) != upkeep.FlakeMaxSubjects || omitted != 35-upkeep.FlakeMaxSubjects {
		t.Fatalf("len(kept) = %d omitted = %d, want %d and %d", len(kept), omitted, upkeep.FlakeMaxSubjects, 35-upkeep.FlakeMaxSubjects)
	}
	if kept[0].Subject != "Test07" {
		t.Errorf("kept[0] = %q, want the most frequent subject Test07", kept[0].Subject)
	}
	if !reflect.DeepEqual(kept, all[:upkeep.FlakeMaxSubjects]) {
		t.Errorf("kept is not the AggregateFlakes prefix")
	}

	under := all[:upkeep.FlakeMaxSubjects]
	if kept, omitted := upkeep.CapFlakeSubjects(under); len(kept) != upkeep.FlakeMaxSubjects || omitted != 0 {
		t.Errorf("at the cap: len(kept) = %d omitted = %d, want %d and 0", len(kept), omitted, upkeep.FlakeMaxSubjects)
	}
}

// TestAggregateFlakes_EmptyIsNonNil: no stages, or stages with no flake, yield
// a non-nil empty slice.
func TestAggregateFlakes_EmptyIsNonNil(t *testing.T) {
	assertNoFlakes(t, nil)
	assertNoFlakes(t, []upkeep.FlakeStage{{RunID: "r", StageID: "s", Attempts: []upkeep.VerifyAttempt{passed("t")}}})
}
