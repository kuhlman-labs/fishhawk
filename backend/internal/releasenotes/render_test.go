package releasenotes_test

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/releaseevidence"
	"github.com/kuhlman-labs/fishhawk/backend/internal/releasenotes"
)

// update regenerates the golden files from the fixtures. Run with
// `go test ./backend/internal/releasenotes/ -run TestRender -update`.
var update = flag.Bool("update", false, "update golden files")

// mixedFixture builds a release mixing a fully loop-merged change (plan +
// verdicts + acceptance + concerns + cost) with a reduced-evidence change (a
// human-led / loop-bypassing PR with no resolvable run). It exercises the
// honesty constraint at the render layer: the reduced entry must be explicitly
// marked and must omit the fabricated verdict/acceptance/cost fields.
func mixedFixture() *releaseevidence.ReleaseEvidence {
	return &releaseevidence.ReleaseEvidence{
		Repo:         "kuhlman-labs/fishhawk",
		PreviousRef:  "v0.1.0",
		CandidateRef: "main",
		TotalCostUSD: 4.20,
		Changes: []releaseevidence.ChangeEvidence{
			{
				PullRequestURL:    "https://github.com/kuhlman-labs/fishhawk/pull/101",
				PullRequestNumber: 101,
				Title:             "Add release_notes artifact kind",
				PlanSummary:       "Persist and render evidence-derived release notes.",
				PlanLink:          "https://github.com/kuhlman-labs/fishhawk/pull/101",
				ReviewerVerdicts: []releaseevidence.ReviewerVerdict{
					{ReviewerModel: "claude-opus-4-8", Verdict: "approve"},
					{ReviewerModel: "gpt-5.5", Verdict: "approve_with_concerns"},
				},
				AcceptanceOutcome: &releaseevidence.AcceptanceOutcome{Verdict: "passed"},
				DeferredConcerns: []releaseevidence.ConcernSummary{
					{Severity: "low", Category: "style", Note: "prefer a table-driven test here"},
				},
				CostUSD:    4.20,
				RunCount:   1,
				LoopMerged: true,
			},
			{
				PullRequestURL:    "https://github.com/kuhlman-labs/fishhawk/pull/102",
				PullRequestNumber: 102,
				Title:             "Hotfix: bump pinned redocly version",
				ReducedEvidence:   true,
				ReducedReason:     "no Fishhawk run resolved for this merged PR (human-led or loop-bypassing change)",
			},
		},
	}
}

// allLoopFixture builds a release where every change is loop-merged, including
// a change with an acceptance failure mode, to pin the failure-mode rendering
// and the multi-change cost rollup.
func allLoopFixture() *releaseevidence.ReleaseEvidence {
	return &releaseevidence.ReleaseEvidence{
		Repo:         "kuhlman-labs/fishhawk",
		PreviousRef:  "abc1234",
		CandidateRef: "def5678",
		TotalCostUSD: 7.50,
		Changes: []releaseevidence.ChangeEvidence{
			{
				PullRequestURL:    "https://github.com/kuhlman-labs/fishhawk/pull/201",
				PullRequestNumber: 201,
				Title:             "Wire the evidence assembler",
				PlanSummary:       "Assemble merged-run evidence between two refs.",
				PlanLink:          "https://github.com/kuhlman-labs/fishhawk/pull/201",
				ReviewerVerdicts: []releaseevidence.ReviewerVerdict{
					{ReviewerModel: "claude-opus-4-8", Verdict: "approve"},
				},
				AcceptanceOutcome: &releaseevidence.AcceptanceOutcome{Verdict: "passed"},
				CostUSD:           5.00,
				RunCount:          1,
				LoopMerged:        true,
			},
			{
				PullRequestURL:    "https://github.com/kuhlman-labs/fishhawk/pull/202",
				PullRequestNumber: 202,
				Title:             "Add the preview endpoint",
				PlanSummary:       "Expose GET /v0/releases/notes/preview.",
				PlanLink:          "https://github.com/kuhlman-labs/fishhawk/pull/202",
				ReviewerVerdicts: []releaseevidence.ReviewerVerdict{
					{ReviewerModel: "claude-opus-4-8", Verdict: "approve"},
					{ReviewerModel: "gpt-5.5", Verdict: "request_changes"},
				},
				AcceptanceOutcome: &releaseevidence.AcceptanceOutcome{Verdict: "failed", FailureMode: "assertion_fail"},
				DeferredConcerns: []releaseevidence.ConcernSummary{
					{Severity: "medium", Category: "correctness", Note: "handle the empty-range case"},
				},
				CostUSD:    2.50,
				RunCount:   2,
				LoopMerged: true,
			},
		},
	}
}

func TestRender_MixedLoopNonLoop(t *testing.T) {
	got := releasenotes.Render(mixedFixture())
	assertGolden(t, "mixed_loop_nonloop.golden.md", got)

	// The reduced-evidence entry is explicitly marked and names its reason,
	// while omitting the fabricated evidence fields.
	if !strings.Contains(got, "> **Reduced evidence.** no Fishhawk run resolved") {
		t.Errorf("reduced-evidence marker + reason missing:\n%s", got)
	}
	// The loop-merged entry carries working plan + PR links and the header
	// cost rollup.
	if !strings.Contains(got, "- Plan: https://github.com/kuhlman-labs/fishhawk/pull/101") {
		t.Errorf("loop-merged plan link missing:\n%s", got)
	}
	if !strings.Contains(got, "Total cost: $4.20") {
		t.Errorf("header cost rollup missing:\n%s", got)
	}
	// The reduced entry must NOT fabricate a reviewer/acceptance line for #102.
	if strings.Contains(got, "Reviewer verdicts:\n- ") && strings.Count(got, "Reviewer verdicts:") != 1 {
		t.Errorf("reduced entry fabricated a reviewer section:\n%s", got)
	}
}

func TestRender_AllLoop(t *testing.T) {
	got := releasenotes.Render(allLoopFixture())
	assertGolden(t, "all_loop.golden.md", got)

	// Acceptance failure mode is surfaced for the failing change.
	if !strings.Contains(got, "Acceptance: failed (failure mode: assertion_fail)") {
		t.Errorf("acceptance failure mode missing:\n%s", got)
	}
	// The ref range is pinned in the header.
	if !strings.Contains(got, "Range: `abc1234..def5678`") {
		t.Errorf("ref range missing:\n%s", got)
	}
}

// TestRender_NilAndEmpty pins the two defensive render branches: a nil model
// and a release with no changes each produce a stable, non-fabricated document.
func TestRender_NilAndEmpty(t *testing.T) {
	if got := releasenotes.Render(nil); !strings.Contains(got, "_No release evidence._") {
		t.Errorf("nil render = %q, want the empty-evidence document", got)
	}
	empty := &releaseevidence.ReleaseEvidence{Repo: "kuhlman-labs/fishhawk", PreviousRef: "a", CandidateRef: "b"}
	got := releasenotes.Render(empty)
	if !strings.Contains(got, "_No merged changes in range._") {
		t.Errorf("empty render missing the no-changes marker:\n%s", got)
	}
}

// assertGolden compares got against the named golden file, regenerating it when
// -update is set. Byte-exact: a formatting drift (non-deterministic field order
// or float precision) fails here.
func assertGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatalf("write golden %s: %v", name, err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v (run with -update to regenerate)", name, err)
	}
	if got != string(want) {
		t.Errorf("render mismatch for %s (run with -update to regenerate):\n--- got ---\n%s\n--- want ---\n%s", name, got, want)
	}
}
