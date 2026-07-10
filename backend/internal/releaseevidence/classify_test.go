package releaseevidence_test

import (
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/releaseevidence"
)

// change builds a loop-merged ChangeEvidence with the given prose. The
// classifier reads only PlanSummary, Title, DeferredConcerns, and the
// reduced-evidence flag, so the synthetic values stay minimal.
func change(pr int, title, planSummary string) releaseevidence.ChangeEvidence {
	return releaseevidence.ChangeEvidence{
		PullRequestNumber: pr,
		PullRequestURL:    "https://example.test/pr/" + title,
		Title:             title,
		PlanSummary:       planSummary,
		LoopMerged:        true,
	}
}

// TestClassifyBumpPerSignalClass asserts the SHIPPED classification for
// one change per signal class — the per-signal-class done-means
// discipline (the classifier's correctness is not structurally enforced
// by compilation).
func TestClassifyBumpPerSignalClass(t *testing.T) {
	tests := []struct {
		name      string
		change    releaseevidence.ChangeEvidence
		wantLevel releaseevidence.BumpLevel
		wantPR    int // 0 => expect no signals
	}{
		{
			name:      "additive workflow-spec field is minor",
			change:    change(101, "Add optional retries field to workflow-v0", "Introduce a new optional field on the workflow spec; additive within the current major."),
			wantLevel: releaseevidence.BumpMinor,
			wantPR:    101,
		},
		{
			name:      "additive new endpoint is minor",
			change:    change(102, "Preview endpoint for release notes", "Add a new endpoint that renders the release preview."),
			wantLevel: releaseevidence.BumpMinor,
			wantPR:    102,
		},
		{
			name:      "additive new stage enum member is minor",
			change:    change(103, "Acceptance stage", "Register a new stage type in the feature_change workflow."),
			wantLevel: releaseevidence.BumpMinor,
			wantPR:    103,
		},
		{
			name:      "doc and test only is patch",
			change:    change(104, "Document the release-evidence assembler", "Update docs/ARCHITECTURE.md and add table-driven tests; no product surface changes."),
			wantLevel: releaseevidence.BumpPatch,
			wantPR:    0,
		},
		{
			name:      "breaking schema-major bump is major",
			change:    change(105, "Cut workflow-v1 keystone", "Bump the workflow spec to schema-major workflow-v1."),
			wantLevel: releaseevidence.BumpMajor,
			wantPR:    105,
		},
		{
			name:      "breaking removed OpenAPI path is major",
			change:    change(106, "Retire the legacy runs listing", "Removed endpoint /v0/legacy-runs from the OpenAPI surface."),
			wantLevel: releaseevidence.BumpMajor,
			wantPR:    106,
		},
		{
			name:      "breaking migration down-incompat is major",
			change:    change(107, "Collapse the audit ledger", "Irreversible migration; the down path cannot restore the dropped rows."),
			wantLevel: releaseevidence.BumpMajor,
			wantPR:    107,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ev := &releaseevidence.ReleaseEvidence{Changes: []releaseevidence.ChangeEvidence{tt.change}}
			hint := releaseevidence.ClassifyBump(ev)
			if hint.Level != tt.wantLevel {
				t.Fatalf("level = %q, want %q (signals: %+v)", hint.Level, tt.wantLevel, hint.Signals)
			}
			if tt.wantPR == 0 {
				if len(hint.Signals) != 0 {
					t.Fatalf("expected no signals for a patch range, got %+v", hint.Signals)
				}
				return
			}
			if len(hint.Signals) == 0 {
				t.Fatalf("expected at least one signal naming the introducing PR")
			}
			for _, s := range hint.Signals {
				if s.Level == hint.Level && s.PRNumber != tt.wantPR {
					t.Fatalf("signal names PR #%d, want #%d", s.PRNumber, tt.wantPR)
				}
			}
		})
	}
}

// TestClassifyBumpRollupPrecedence asserts a mixed additive+breaking range
// rolls up to major (the release takes the max signal level).
func TestClassifyBumpRollupPrecedence(t *testing.T) {
	ev := &releaseevidence.ReleaseEvidence{
		Changes: []releaseevidence.ChangeEvidence{
			change(201, "Add optional field", "Introduce a new optional field on the spec."),
			change(202, "Drop legacy path", "Removed endpoint /v0/old from the OpenAPI surface."),
		},
	}
	hint := releaseevidence.ClassifyBump(ev)
	if hint.Level != releaseevidence.BumpMajor {
		t.Fatalf("mixed range level = %q, want major", hint.Level)
	}
	// The rollup keeps both signals; the major one names the breaking PR.
	var sawMajorPR bool
	for _, s := range hint.Signals {
		if s.Level == releaseevidence.BumpMajor && s.PRNumber == 202 {
			sawMajorPR = true
		}
	}
	if !sawMajorPR {
		t.Fatalf("expected a major signal naming PR #202, got %+v", hint.Signals)
	}
}

// TestClassifyBumpReducedEvidenceUsesTitleOnly asserts a reduced-evidence
// change classifies from its Title alone — its (populated) PlanSummary is
// never consulted, matching the honesty constraint that a human-led PR
// contributes only what it actually carries.
func TestClassifyBumpReducedEvidenceUsesTitleOnly(t *testing.T) {
	ch := releaseevidence.ChangeEvidence{
		PullRequestNumber: 301,
		Title:             "Housekeeping: tidy the changelog",
		// A reduced change carries no real plan; this planted summary must
		// be ignored, so a breaking phrase here must NOT escalate the hint.
		PlanSummary:     "removed endpoint /v0/should-be-ignored",
		ReducedEvidence: true,
	}
	hint := releaseevidence.ClassifyBump(&releaseevidence.ReleaseEvidence{
		Changes: []releaseevidence.ChangeEvidence{ch},
	})
	if hint.Level != releaseevidence.BumpPatch {
		t.Fatalf("reduced-evidence title-only classify = %q, want patch (plan summary must be ignored)", hint.Level)
	}
}

// TestClassifyBumpDeferredConcernCategory asserts a deferred-concern
// category folds into the classification text as supplementary signal.
func TestClassifyBumpDeferredConcernCategory(t *testing.T) {
	ch := change(401, "Land the change", "Ordinary refactor with no bump signal in the summary or title.")
	ch.DeferredConcerns = []releaseevidence.ConcernSummary{
		{Severity: "low", Category: "new endpoint left untested", Note: "follow-up filed"},
	}
	hint := releaseevidence.ClassifyBump(&releaseevidence.ReleaseEvidence{
		Changes: []releaseevidence.ChangeEvidence{ch},
	})
	if hint.Level != releaseevidence.BumpMinor {
		t.Fatalf("deferred-concern category classify = %q, want minor", hint.Level)
	}
}

// TestClassifyBumpNilAndEmpty asserts the defensive branches: a nil
// evidence and an empty-change range both yield a patch hint with no
// signals (the early return and the default level).
func TestClassifyBumpNilAndEmpty(t *testing.T) {
	for _, tt := range []struct {
		name string
		ev   *releaseevidence.ReleaseEvidence
	}{
		{"nil evidence", nil},
		{"no changes", &releaseevidence.ReleaseEvidence{}},
		{"blank-prose change", &releaseevidence.ReleaseEvidence{Changes: []releaseevidence.ChangeEvidence{{PullRequestNumber: 1}}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			hint := releaseevidence.ClassifyBump(tt.ev)
			if hint.Level != releaseevidence.BumpPatch {
				t.Fatalf("level = %q, want patch", hint.Level)
			}
			if len(hint.Signals) != 0 {
				t.Fatalf("expected no signals, got %+v", hint.Signals)
			}
		})
	}
}

// TestPreviewLine asserts the render-ready formatter: an additive hint
// names the introducing PR, and a patch hint names the doc/test-only
// basis.
func TestPreviewLine(t *testing.T) {
	minor := releaseevidence.ClassifyBump(&releaseevidence.ReleaseEvidence{
		Changes: []releaseevidence.ChangeEvidence{
			change(501, "Preview endpoint", "Add a new endpoint for the release preview."),
		},
	})
	line := minor.PreviewLine()
	if !strings.HasPrefix(line, "suggested bump: minor (because ") {
		t.Fatalf("minor preview line = %q, want the suggested-bump prefix", line)
	}
	if !strings.Contains(line, "#501") {
		t.Fatalf("minor preview line = %q, want it to name the introducing PR #501", line)
	}

	patch := releaseevidence.ClassifyBump(&releaseevidence.ReleaseEvidence{
		Changes: []releaseevidence.ChangeEvidence{
			change(502, "Docs only", "Update docs and tests; no product surface changes."),
		},
	})
	patchLine := patch.PreviewLine()
	if !strings.HasPrefix(patchLine, "suggested bump: patch (because ") {
		t.Fatalf("patch preview line = %q, want the suggested-bump prefix", patchLine)
	}
	if !strings.Contains(patchLine, "doc/test-only") {
		t.Fatalf("patch preview line = %q, want it to name the doc/test-only basis", patchLine)
	}
}
