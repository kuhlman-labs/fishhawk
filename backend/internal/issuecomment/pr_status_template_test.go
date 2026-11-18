package issuecomment

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/diffsecrets"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

func prStatusRun() *run.Run {
	ref := "issue:42"
	return &run.Run{
		ID:            uuid.MustParse("11111111-2222-3333-4444-555555555555"),
		Repo:          "octo/cat",
		WorkflowID:    "feature_change",
		State:         run.StateRunning,
		TriggerSource: run.TriggerGitHubIssue,
		TriggerRef:    &ref,
	}
}

func prAuditEntry(seq int64, category string, payload map[string]any) *audit.Entry {
	body, _ := json.Marshal(payload)
	return &audit.Entry{Sequence: seq, Category: category, Payload: body, Timestamp: time.Unix(seq, 0).UTC()}
}

// acceptanceArtifactJSON builds a KindAcceptance artifact body with the given
// flat criteria — the shape decodePRAcceptanceCriteria reads.
func acceptanceArtifactJSON(t *testing.T, verdict string, criteria []map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{"verdict": verdict, "criteria": criteria})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestRenderPRStatusBody_UnsetExternalURL_DegradesLinks pins #1787 at the E42.1
// sticky PR-status-comment locus (binding condition 1): with the base URL unset
// the header renders the plain backticked short-id (no link) and the footer
// omits the "view run" link (no dangling middot before the Issue-thread link),
// with no localhost / relative run link leaking. Configured, the absolute link
// returns.
func TestRenderPRStatusBody_UnsetExternalURL_DegradesLinks(t *testing.T) {
	r := prStatusRun()
	in := PRStatusInput{
		Run:    r,
		Stages: []*run.Stage{{Type: run.StageTypeReview, State: run.StageStateRunning}},
		Now:    time.Unix(1000, 0).UTC(),
	}

	unset := RenderPRStatusBody(in)
	if strings.Contains(unset, "localhost") || strings.Contains(unset, "/runs/") || strings.Contains(unset, "](/") {
		t.Errorf("unset PR status comment leaked a run link:\n%s", unset)
	}
	if !strings.Contains(unset, "**Fishhawk run `11111111`**") {
		t.Errorf("unset PR status header should carry the plain backticked short-id:\n%s", unset)
	}
	if strings.Contains(unset, "View run") {
		t.Errorf("unset PR status footer should omit the View run link:\n%s", unset)
	}
	// The Issue-thread link is a github.com URL, not derived from the base URL,
	// so it survives — and there is no dangling middot before it.
	if !strings.Contains(unset, "[Issue thread →](https://github.com/octo/cat/issues/42)") {
		t.Errorf("unset PR status footer should still carry the Issue-thread link:\n%s", unset)
	}
	if strings.Contains(unset, "· [Issue thread →]") {
		t.Errorf("unset PR status footer left a dangling middot before the Issue link:\n%s", unset)
	}

	in.ExternalURL = "https://app.example"
	cfg := RenderPRStatusBody(in)
	if !strings.Contains(cfg, "https://app.example/runs/"+r.ID.String()) {
		t.Errorf("configured PR status comment should carry the absolute run link:\n%s", cfg)
	}
}

// TestRenderPRStatusBody_HeaderAndWhatNow pins the header state icon and the
// merge-scoped "what now" line for the running / accepted / rejected states.
func TestRenderPRStatusBody_HeaderAndWhatNow(t *testing.T) {
	t.Run("running", func(t *testing.T) {
		body := RenderPRStatusBody(PRStatusInput{
			Run:         prStatusRun(),
			Stages:      []*run.Stage{{Type: run.StageTypeReview, State: run.StageStateRunning}},
			ExternalURL: "https://app.example",
			Now:         time.Unix(1000, 0).UTC(),
		})
		if !strings.Contains(body, "🔄 running") {
			t.Errorf("header missing running state icon:\n%s", body)
		}
		if !strings.Contains(body, "implemented and reviewed") {
			t.Errorf("what-now missing running phrasing:\n%s", body)
		}
	})

	t.Run("acceptance accepted", func(t *testing.T) {
		body := RenderPRStatusBody(PRStatusInput{
			Run:   prStatusRun(),
			Audit: []*audit.Entry{prAuditEntry(5, "acceptance_outcome_recorded", map[string]any{"outcome": "accepted", "criteria_passed": 2, "criteria_total": 2})},
			Now:   time.Unix(1000, 0).UTC(),
		})
		if !strings.Contains(body, "merge when ready") {
			t.Errorf("what-now missing accepted phrasing:\n%s", body)
		}
	})

	t.Run("acceptance rejected", func(t *testing.T) {
		body := RenderPRStatusBody(PRStatusInput{
			Run:   prStatusRun(),
			Audit: []*audit.Entry{prAuditEntry(5, "acceptance_outcome_recorded", map[string]any{"outcome": "rejected", "criteria_passed": 1, "criteria_total": 2})},
			Now:   time.Unix(1000, 0).UTC(),
		})
		if !strings.Contains(body, "triage before merging") {
			t.Errorf("what-now missing rejected phrasing:\n%s", body)
		}
	})
}

// TestRenderPRStatusBody_AcceptanceNotValidated pins the #2347 PR-comment
// headline on the SHIPPED string. Like the issue-anchor twin, this is a
// rendered-vocabulary change compilation cannot enforce, so both the honest
// wording AND the absence of the certification wording are asserted. The
// outcome value is taken from the plan-package constant the orchestrator emits,
// so this doubles as the byte-identity pin for the package-local mirror.
func TestRenderPRStatusBody_AcceptanceNotValidated(t *testing.T) {
	t.Run("full fidelity", func(t *testing.T) {
		body := RenderPRStatusBody(PRStatusInput{
			Run: prStatusRun(),
			Audit: []*audit.Entry{prAuditEntry(5, "acceptance_outcome_recorded", map[string]any{
				"outcome": plan.AcceptanceOutcomeNotValidated, "criteria_passed": 0, "criteria_total": 2,
			})},
			Now: time.Unix(1000, 0).UTC(),
		})
		want := "**Acceptance** — ❓ not validated — 0/2 criteria verified (all criteria skip-expected)"
		if !strings.Contains(body, want) {
			t.Errorf("missing %q:\n%s", want, body)
		}
		for _, forbidden := range []string{"✅ accepted", "criteria passed"} {
			if strings.Contains(body, forbidden) {
				t.Errorf("body still contains %q — a short-circuited stage verified ZERO criteria (#2347):\n%s", forbidden, body)
			}
		}
	})

	t.Run("zero total names the empty-criteria plan", func(t *testing.T) {
		body := RenderPRStatusBody(PRStatusInput{
			Run: prStatusRun(),
			Audit: []*audit.Entry{prAuditEntry(5, "acceptance_outcome_recorded", map[string]any{
				"outcome": plan.AcceptanceOutcomeNotValidated, "criteria_passed": 0, "criteria_total": 0,
			})},
			Now: time.Unix(1000, 0).UTC(),
		})
		want := "**Acceptance** — ❓ not validated — 0 criteria verified (the plan declared none)"
		if !strings.Contains(body, want) {
			t.Errorf("missing %q:\n%s", want, body)
		}
	})

	t.Run("live-validation clause", func(t *testing.T) {
		body := RenderPRStatusBody(PRStatusInput{
			Run: prStatusRun(),
			Audit: []*audit.Entry{prAuditEntry(5, "acceptance_outcome_recorded", map[string]any{
				"outcome": plan.AcceptanceOutcomeNotValidated, "criteria_passed": 0, "criteria_total": 3,
				"criteria_live_validation": 1,
			})},
			Now: time.Unix(1000, 0).UTC(),
		})
		if !strings.Contains(body, "0/3 criteria verified (all criteria skip-expected); 1 require live validation") {
			t.Errorf("missing the live-validation clause:\n%s", body)
		}
	})

	// The COLLAPSED fidelity (degradation ladder drops the per-criterion table)
	// shares renderPRAcceptanceHeadline, so the surviving tally line must be
	// honest too — this is the line an operator reads on an oversize comment.
	t.Run("collapsed fidelity keeps the honest tally", func(t *testing.T) {
		basis := strings.Repeat("x", 180)
		criteria := make([]map[string]any, 0, 500)
		for i := 0; i < 500; i++ {
			criteria = append(criteria, map[string]any{"id": "AC", "result": "skipped", "expectation_basis": basis})
		}
		body := RenderPRStatusBody(PRStatusInput{
			Run: prStatusRun(),
			Audit: []*audit.Entry{prAuditEntry(5, "acceptance_outcome_recorded", map[string]any{
				"outcome": plan.AcceptanceOutcomeNotValidated, "criteria_passed": 0, "criteria_total": 500,
			})},
			AcceptanceArtifact: acceptanceArtifactJSON(t, "passed", criteria),
			ExternalURL:        "https://app.example",
			Now:                time.Unix(1000, 0).UTC(),
		})
		if len(body) > MaxIssueCommentBodyBytes {
			t.Fatalf("body exceeds GitHub cap after degradation: %d > %d", len(body), MaxIssueCommentBodyBytes)
		}
		if strings.Contains(body, "| Criterion | Result | Basis |") {
			t.Errorf("oversize body should have dropped the criteria table")
		}
		want := "**Acceptance** — ❓ not validated — 0/500 criteria verified (all criteria skip-expected)"
		if !strings.Contains(body, want) {
			t.Errorf("collapsed tally line is not the honest one; missing %q", want)
		}
	})
}

// TestRenderPRStatusBody_AcceptanceUndecidable pins the #2512 PR-comment
// headline on the SHIPPED string. This is the surface the operator's merge
// decision is actually made on, and it is the one the earlier attempts at this
// change silently dropped — so the assertions are on both the honest wording
// AND the absence of the generic "(N/M criteria passed)" shape, which would
// present an undecided criterion as a partial pass.
func TestRenderPRStatusBody_AcceptanceUndecidable(t *testing.T) {
	t.Run("full fidelity", func(t *testing.T) {
		body := RenderPRStatusBody(PRStatusInput{
			Run: prStatusRun(),
			Audit: []*audit.Entry{prAuditEntry(5, "acceptance_outcome_recorded", map[string]any{
				"outcome": plan.AcceptanceOutcomeUndecidable, "criteria_passed": 2,
				"criteria_total": 4, "criteria_undecidable": 2,
			})},
			Now: time.Unix(1000, 0).UTC(),
		})
		want := "**Acceptance** — ❓ undecidable — 2/4 criteria could not be decided (2 passed)"
		if !strings.Contains(body, want) {
			t.Errorf("missing %q:\n%s", want, body)
		}
		for _, forbidden := range []string{"✅ accepted", "❌ rejected", "criteria passed)"} {
			if strings.Contains(body, forbidden) {
				t.Errorf("body still contains %q — an undecidable outcome is neither an acceptance, a rejection, nor a partial pass (#2512):\n%s", forbidden, body)
			}
		}
	})

	// The ALL-undecidable arm the ladder explicitly admits: the stage ran and
	// decided NOTHING. Asserted separately because it is the shape most likely
	// to be misread as a pass, and the one the plan forbids describing as
	// "verified some criteria and could not evaluate others".
	t.Run("all-undecidable decided nothing", func(t *testing.T) {
		body := RenderPRStatusBody(PRStatusInput{
			Run: prStatusRun(),
			Audit: []*audit.Entry{prAuditEntry(5, "acceptance_outcome_recorded", map[string]any{
				"outcome": plan.AcceptanceOutcomeUndecidable, "criteria_passed": 0,
				"criteria_total": 3, "criteria_undecidable": 3,
			})},
			Now: time.Unix(1000, 0).UTC(),
		})
		want := "**Acceptance** — ❓ undecidable — 3/3 criteria could not be decided (0 passed)"
		if !strings.Contains(body, want) {
			t.Errorf("missing %q:\n%s", want, body)
		}
	})

	// Degradation arm: an undecidable outcome with no tally must still render
	// the undecidable sentence rather than the generic "❓ undecidable" fallback,
	// which reads as a bare label with no explanation.
	t.Run("no tally degrades to the count-free sentence", func(t *testing.T) {
		body := RenderPRStatusBody(PRStatusInput{
			Run: prStatusRun(),
			Audit: []*audit.Entry{prAuditEntry(5, "acceptance_outcome_recorded", map[string]any{
				"outcome": plan.AcceptanceOutcomeUndecidable,
			})},
			Now: time.Unix(1000, 0).UTC(),
		})
		want := "**Acceptance** — ❓ undecidable — criteria could not be decided"
		if !strings.Contains(body, want) {
			t.Errorf("missing %q:\n%s", want, body)
		}
	})

	// The COLLAPSED fidelity shares renderPRAcceptanceHeadline, so the surviving
	// line on an oversize comment must be the honest one too — same posture as
	// the not_validated twin above.
	t.Run("collapsed fidelity keeps the honest sentence", func(t *testing.T) {
		basis := strings.Repeat("x", 180)
		criteria := make([]map[string]any, 0, 500)
		for i := 0; i < 500; i++ {
			criteria = append(criteria, map[string]any{"id": "AC", "result": "skipped", "expectation_basis": basis})
		}
		body := RenderPRStatusBody(PRStatusInput{
			Run: prStatusRun(),
			Audit: []*audit.Entry{prAuditEntry(5, "acceptance_outcome_recorded", map[string]any{
				"outcome": plan.AcceptanceOutcomeUndecidable, "criteria_passed": 0,
				"criteria_total": 500, "criteria_undecidable": 500,
			})},
			AcceptanceArtifact: acceptanceArtifactJSON(t, "passed", criteria),
			ExternalURL:        "https://app.example",
			Now:                time.Unix(1000, 0).UTC(),
		})
		if len(body) > MaxIssueCommentBodyBytes {
			t.Fatalf("body exceeds GitHub cap after degradation: %d > %d", len(body), MaxIssueCommentBodyBytes)
		}
		want := "**Acceptance** — ❓ undecidable — 500/500 criteria could not be decided (0 passed)"
		if !strings.Contains(body, want) {
			t.Errorf("collapsed line is not the honest one; missing %q", want)
		}
	})
}

// TestRenderPRStatusBody_CurrentRoundReviewsOnly pins that a stale earlier-round
// verdict (below the latest implement_review_started floor) is EXCLUDED while
// the current round's verdicts render.
func TestRenderPRStatusBody_CurrentRoundReviewsOnly(t *testing.T) {
	entries := []*audit.Entry{
		// Round 1 (stale): a verdict below a later review-started floor.
		prAuditEntry(1, "implement_review_started", map[string]any{}),
		prAuditEntry(2, "implement_reviewed", map[string]any{"reviewer_model": "stale-model", "verdict": "reject"}),
		// Round 2 (current): floor + a fresh verdict above it.
		prAuditEntry(3, "implement_review_started", map[string]any{}),
		prAuditEntry(4, "implement_reviewed", map[string]any{"reviewer_model": "claude-opus-4-8", "verdict": "approve"}),
	}
	body := RenderPRStatusBody(PRStatusInput{
		Run:   prStatusRun(),
		Audit: entries,
		Now:   time.Unix(1000, 0).UTC(),
	})
	if !strings.Contains(body, "PR Reviewer (claude-opus-4-8): approve") {
		t.Errorf("current-round verdict missing:\n%s", body)
	}
	if strings.Contains(body, "stale-model") {
		t.Errorf("stale earlier-round verdict leaked into PR comment:\n%s", body)
	}
}

// TestRenderPRStatusBody_AcceptanceTable pins the per-criterion table (pass /
// fail / skip rows, basis rendered when present + omitted when absent), the
// target URL, the validated head SHA, and the triage disposition line.
func TestRenderPRStatusBody_AcceptanceTable(t *testing.T) {
	art := acceptanceArtifactJSON(t, "failed", []map[string]any{
		{"id": "AC-1", "result": "passed", "expectation_basis": "issue statement"},
		{"id": "AC-2", "result": "failed", "observed": "returned 500"},
		{"id": "AC-3", "result": "skipped"},
	})
	entries := []*audit.Entry{
		prAuditEntry(5, "acceptance_outcome_recorded", map[string]any{
			"outcome": "rejected", "criteria_passed": 1, "criteria_total": 3,
			"target_url": "http://localhost:8080", "head_sha": "abcdef1234567890",
			"stage_id": uuid.New().String(), "content_hash": "sha256:x",
		}),
		prAuditEntry(6, "acceptance_triage_decided", map[string]any{"class": "3", "disposition": "waived"}),
	}
	body := RenderPRStatusBody(PRStatusInput{
		Run:                prStatusRun(),
		Audit:              entries,
		AcceptanceArtifact: art,
		Now:                time.Unix(1000, 0).UTC(),
	})

	for _, want := range []string{
		"| Criterion | Result | Basis |",
		"| `AC-1` | ✅ pass | issue statement |",
		"| `AC-2` | ❌ fail | returned 500 |",
		"| `AC-3` | ⏭ skip |  |", // basis absent → empty cell
		"Target: http://localhost:8080",
		"Validated head: `abcdef123456`",
		"Triage — class-3: waived",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("acceptance section missing %q:\n%s", want, body)
		}
	}
}

// TestRenderPRStatusBody_FixupHistory pins one line per fixup_pushed pass with
// the short head SHA, files-changed count, and apply_path discriminator.
func TestRenderPRStatusBody_FixupHistory(t *testing.T) {
	entries := []*audit.Entry{
		prAuditEntry(3, "fixup_pushed", map[string]any{"head_sha": "1111111111112222", "files_changed_count": 1, "apply_path": "git_apply"}),
		prAuditEntry(7, "fixup_pushed", map[string]any{"head_sha": "3333333333334444", "files_changed_count": 3}),
	}
	body := RenderPRStatusBody(PRStatusInput{
		Run:   prStatusRun(),
		Audit: entries,
		Now:   time.Unix(1000, 0).UTC(),
	})
	if !strings.Contains(body, "1. `111111111111` · 1 file changed · git_apply") {
		t.Errorf("fixup pass 1 missing:\n%s", body)
	}
	if !strings.Contains(body, "2. `333333333333` · 3 files changed") {
		t.Errorf("fixup pass 2 missing:\n%s", body)
	}
}

// TestRenderPRStatusBody_NoPlanDump pins that the PR comment carries NO
// plan / scope / approach material (that is issue-locus, on the anchor).
func TestRenderPRStatusBody_NoPlanDump(t *testing.T) {
	body := RenderPRStatusBody(PRStatusInput{
		Run:         prStatusRun(),
		Audit:       []*audit.Entry{prAuditEntry(1, "plan_generated", map[string]any{"schema_version": "standard_v1"})},
		ExternalURL: "https://app.example",
		Now:         time.Unix(1000, 0).UTC(),
	})
	for _, forbidden := range []string{"**Plan**", "**Scope**", "**Approach**", "Plan details"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("PR comment leaked plan-locus section %q:\n%s", forbidden, body)
		}
	}
}

// TestRenderPRStatusBody_Footer pins the run link + issue-thread back link.
func TestRenderPRStatusBody_Footer(t *testing.T) {
	body := RenderPRStatusBody(PRStatusInput{
		Run:         prStatusRun(),
		ExternalURL: "https://app.example",
		Now:         time.Unix(1000, 0).UTC(),
	})
	if !strings.Contains(body, "[View run →](https://app.example/runs/11111111-2222-3333-4444-555555555555)") {
		t.Errorf("footer missing run link:\n%s", body)
	}
	if !strings.Contains(body, "[Issue thread →](https://github.com/octo/cat/issues/42)") {
		t.Errorf("footer missing issue-thread link:\n%s", body)
	}
}

// TestRenderPRStatusBody_DegradationLadder pins the ladder: under oversize the
// acceptance per-criterion table collapses to the tally line first, then the
// fix-up history drops, while the header / reviews / acceptance-outcome /
// footer survive.
func TestRenderPRStatusBody_DegradationLadder(t *testing.T) {
	// Many criteria rows — enough that the full per-criterion table blows the
	// cap (each cell is capped at 200 bytes by oneLine, so the oversize must
	// come from row count) while the collapsed tally line stays tiny.
	basis := strings.Repeat("x", 180)
	criteria := make([]map[string]any, 0, 500)
	for i := 0; i < 500; i++ {
		criteria = append(criteria, map[string]any{"id": "AC", "result": "failed", "expectation_basis": basis})
	}
	art := acceptanceArtifactJSON(t, "failed", criteria)
	entries := []*audit.Entry{
		prAuditEntry(5, "acceptance_outcome_recorded", map[string]any{"outcome": "rejected", "criteria_passed": 0, "criteria_total": 500}),
		prAuditEntry(6, "fixup_pushed", map[string]any{"head_sha": "1111111111112222", "files_changed_count": 1}),
	}
	body := RenderPRStatusBody(PRStatusInput{
		Run:                prStatusRun(),
		Audit:              entries,
		AcceptanceArtifact: art,
		ExternalURL:        "https://app.example",
		Now:                time.Unix(1000, 0).UTC(),
	})
	if len(body) > MaxIssueCommentBodyBytes {
		t.Fatalf("body exceeds GitHub cap after degradation: %d > %d", len(body), MaxIssueCommentBodyBytes)
	}
	if strings.Contains(body, "| Criterion | Result | Basis |") {
		t.Errorf("oversize body should have dropped the criteria table:\n%s", body[:200])
	}
	// The acceptance OUTCOME tally line and header survive the collapse.
	if !strings.Contains(body, "**Acceptance** — ❌ rejected (0/500 criteria passed)") {
		t.Errorf("acceptance tally line dropped under degradation:\n%s", body[:200])
	}
	if !strings.Contains(body, "Fishhawk run") {
		t.Errorf("header dropped under degradation:\n%s", body[:200])
	}
	// The hidden sticky marker (#1793) is accounted for in the size budget and
	// preserved through the ladder + tail-truncation.
	if marker := stickyMarker(stickyLocusPRStatus, prStatusRun().ID); !strings.Contains(body, marker) {
		t.Errorf("sticky marker must survive the degradation ladder: want %q in body", marker)
	}
}

// TestRenderPRStatusBody_NilRun returns empty for a nil run.
func TestRenderPRStatusBody_NilRun(t *testing.T) {
	if got := RenderPRStatusBody(PRStatusInput{}); got != "" {
		t.Errorf("nil run should render empty, got %q", got)
	}
}

// TestRenderPRStatusBody_EmbedsStickyMarker pins #1793: the PR status body leads
// with the hidden pr-status sticky marker so the orphan-rediscovery fallback can
// match it, and it is distinct from the anchor marker (different locus).
func TestRenderPRStatusBody_EmbedsStickyMarker(t *testing.T) {
	r := prStatusRun()
	marker := stickyMarker(stickyLocusPRStatus, r.ID)
	if marker != "<!-- fishhawk-sticky locus=pr-status run=11111111-2222-3333-4444-555555555555 -->" {
		t.Fatalf("unexpected marker format: %q", marker)
	}
	body := RenderPRStatusBody(PRStatusInput{Run: r, ExternalURL: "https://app.example", Now: time.Unix(1000, 0).UTC()})
	if !strings.HasPrefix(body, marker) {
		t.Errorf("pr-status marker must be the FIRST body section; body = %q", body)
	}
	// Distinct from the anchor marker so a locus never cross-matches.
	if strings.Contains(body, stickyMarker(stickyLocusAnchor, r.ID)) {
		t.Errorf("PR status body must not carry the anchor marker")
	}
}

// TestRenderPRWhatNow_AcceptancePrecedence is the per-arm behavioral table for
// the #3026 hoist: the acceptance-derived switch now runs AHEAD of the
// StateSucceeded arm, while StateCancelled and StateFailed keep precedence over
// both. One case per arm — the list is exhaustive and matches the plan's
// verification strategy.
//
// Case (c) is the DISCRIMINATING control for the reworded acceptance criterion:
// a completed run whose acceptance was ACCEPTED must render the pre-existing
// acceptance-passed sentence VERBATIM (a MOVE, not a rewrite) and must NEVER
// render the not-validated wording.
func TestRenderPRWhatNow_AcceptancePrecedence(t *testing.T) {
	const (
		sentReady        = "_What now: run complete — the PR is ready to merge._"
		sentCancelled    = "_What now: run cancelled — the PR will not merge from this run._"
		sentFailed       = "_What now: a stage failed — review the failure, then retry or replan before merging._"
		sentAccepted     = "_What now: acceptance passed — review the verdicts below and merge when ready._"
		sentRejected     = "_What now: acceptance failed — see the criteria table below and triage before merging._"
		zeroVerification = "ZERO criteria were verified"
	)
	// Stages that would otherwise select the in-progress "being implemented
	// and reviewed" sentence, so case (g) proves the acceptance arm wins over
	// the stage-derived fallback rather than merely over a nil stage list.
	inFlight := []*run.Stage{{Type: run.StageTypeReview, State: run.StageStateRunning}}

	cases := []struct {
		name     string
		state    run.State
		acc      *prAcceptanceView
		want     string   // exact sentence, when the arm renders a fixed string
		contains []string // substrings, for the two zero-verification arms
		absent   []string
	}{
		{
			name:     "(a) succeeded + not_validated names zero verification and #2347",
			state:    run.StateSucceeded,
			acc:      &prAcceptanceView{outcome: acceptanceOutcomeNotValidated},
			contains: []string{"NOT validated", zeroVerification, "#2347", "merge-eligible"},
			absent:   []string{sentReady},
		},
		{
			name:     "(b) succeeded + undecidable names zero verification and #2347",
			state:    run.StateSucceeded,
			acc:      &prAcceptanceView{outcome: acceptanceOutcomeUndecidable},
			contains: []string{"UNDECIDABLE", zeroVerification, "#2347", "merge-eligible"},
			absent:   []string{sentReady},
		},
		{
			name:   "(c) succeeded + accepted renders the acceptance-passed sentence VERBATIM",
			state:  run.StateSucceeded,
			acc:    &prAcceptanceView{outcome: "accepted"},
			want:   sentAccepted,
			absent: []string{"NOT validated", zeroVerification, sentReady},
		},
		{
			name:  "(d) succeeded + no acceptance view still renders the ready-to-merge sentence",
			state: run.StateSucceeded,
			acc:   nil,
			want:  sentReady,
		},
		{
			name:  "(e) FAILED + not_validated keeps the failed sentence",
			state: run.StateFailed,
			acc:   &prAcceptanceView{outcome: acceptanceOutcomeNotValidated},
			want:  sentFailed,
		},
		{
			name:  "(f) CANCELLED + not_validated keeps the cancelled sentence",
			state: run.StateCancelled,
			acc:   &prAcceptanceView{outcome: acceptanceOutcomeNotValidated},
			want:  sentCancelled,
		},
		{
			name:     "(g) in-progress + not_validated renders the acceptance-derived sentence",
			state:    run.StateRunning,
			acc:      &prAcceptanceView{outcome: acceptanceOutcomeNotValidated},
			contains: []string{"NOT validated", zeroVerification, "#2347"},
			absent:   []string{"being implemented and reviewed"},
		},
		{
			name:  "(h) succeeded + rejected renders the acceptance-failed sentence verbatim",
			state: run.StateSucceeded,
			acc:   &prAcceptanceView{outcome: "rejected"},
			want:  sentRejected,
		},
		{
			name:  "(i) succeeded + an outcome no arm names falls through to the state sentence",
			state: run.StateSucceeded,
			acc:   &prAcceptanceView{outcome: "something_new"},
			want:  sentReady,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := prStatusRun()
			r.State = tc.state
			got := renderPRWhatNow(r, inFlight, tc.acc)
			if tc.want != "" && got != tc.want {
				t.Fatalf("renderPRWhatNow = %q, want %q", got, tc.want)
			}
			for _, w := range tc.contains {
				if !strings.Contains(got, w) {
					t.Errorf("renderPRWhatNow = %q, missing %q", got, w)
				}
			}
			for _, a := range tc.absent {
				if strings.Contains(got, a) {
					t.Errorf("renderPRWhatNow = %q, must not contain %q", got, a)
				}
			}
		})
	}
}

// TestRenderPRStatusBody_AcceptanceHeadLine pins the head-line rule (#4846 R8):
// "Validated head" renders only for an outcome that decided criteria against
// the head (accepted, rejected, undecidable with at least one decided
// criterion). Every other outcome that records a head renders "Covers head:
// `<sha>` (not validated)", including the decomposed-parent short-circuit
// verdict (not_validated with basis all-skip-with-basis), which now carries
// head_sha. An absent head_sha renders neither line.
func TestRenderPRStatusBody_AcceptanceHeadLine(t *testing.T) {
	const (
		head      = "abcdef1234567890"
		validated = "- Validated head: `abcdef123456`"
		covers    = "- Covers head: `abcdef123456` (not validated)"
	)
	cases := []struct {
		name    string
		payload map[string]any
		want    string // "" means neither head line renders
	}{
		{name: "accepted", payload: map[string]any{"outcome": "accepted", "criteria_passed": 2, "criteria_total": 2}, want: validated},
		{name: "rejected", payload: map[string]any{"outcome": "rejected", "criteria_passed": 0, "criteria_failed": 1, "criteria_total": 1}, want: validated},
		{name: "undecidable with one passed", payload: map[string]any{"outcome": "undecidable", "criteria_passed": 1, "criteria_undecidable": 1, "criteria_total": 2}, want: validated},
		{name: "undecidable with one failed", payload: map[string]any{"outcome": "undecidable", "criteria_failed": 1, "criteria_undecidable": 1, "criteria_total": 2}, want: validated},
		{name: "undecidable with zero decided", payload: map[string]any{"outcome": "undecidable", "criteria_undecidable": 2, "criteria_total": 2}, want: covers},
		{name: "not_validated all-skip-with-basis", payload: map[string]any{"outcome": "not_validated", "verdict": "not_validated", "basis": "all-skip-with-basis", "criteria_passed": 0, "criteria_failed": 0, "criteria_total": 2}, want: covers},
		{name: "skipped_out_of_scope", payload: map[string]any{"outcome": "skipped_out_of_scope"}, want: covers},
		{name: "empty outcome", payload: map[string]any{}, want: covers},
		{name: "unknown outcome", payload: map[string]any{"outcome": "something_new", "criteria_passed": 3}, want: covers},
		{name: "absent head_sha", payload: map[string]any{"outcome": "accepted", "criteria_passed": 1, "criteria_total": 1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload := map[string]any{"stage_id": uuid.New().String()}
			for k, v := range tc.payload {
				payload[k] = v
			}
			if tc.name != "absent head_sha" {
				payload["head_sha"] = head
			}
			body := RenderPRStatusBody(PRStatusInput{
				Run:   prStatusRun(),
				Audit: []*audit.Entry{prAuditEntry(5, "acceptance_outcome_recorded", payload)},
				Now:   time.Unix(1000, 0).UTC(),
			})
			if !strings.Contains(body, "**Acceptance**") {
				t.Fatalf("acceptance section missing (fixture defect):\n%s", body)
			}
			for _, line := range []string{validated, covers} {
				has := strings.Contains(body, line)
				if line == tc.want && !has {
					t.Errorf("body missing %q:\n%s", line, body)
				}
				if line != tc.want && has {
					t.Errorf("body must not contain %q:\n%s", line, body)
				}
			}
			if tc.want != validated && strings.Contains(body, "Validated head") {
				t.Errorf("body certifies an unvalidated head with 'Validated head':\n%s", body)
			}
		})
	}
}

// diffSecretsEntry builds a diff_secrets_detected entry by marshalling the
// SHARED wire type, so the reader is pinned to the writer's contract.
func diffSecretsEntry(t *testing.T, seq int64, sum diffsecrets.SuppressionSummary) *audit.Entry {
	t.Helper()
	body, err := json.Marshal(struct {
		Check string `json:"check"`
		diffsecrets.SuppressionSummary
	}{"diff_secrets", sum})
	if err != nil {
		t.Fatal(err)
	}
	return &audit.Entry{Sequence: seq, Category: "diff_secrets_detected", Payload: body, Timestamp: time.Unix(seq, 0).UTC()}
}

func renderDiffSecretsPR(t *testing.T, entries ...*audit.Entry) string {
	t.Helper()
	return RenderPRStatusBody(PRStatusInput{Run: prStatusRun(), Audit: entries, Now: time.Unix(1000, 0).UTC()})
}

// TestRenderPRStatus_DiffSecretsSuppressed (#5230 C3): the section shows the
// totals, by-rule attribution (SafeRuleID + short fingerprint), marker
// locations, the admission short SHA and the rejected count; it is absent with
// nothing suppressed or rejected; it caps marker locations; it sanitizes
// paths; it survives degradation level 2; a corrupted rule_id never renders.
//
// COUNTERFACTUAL: render x.RuleID raw instead of through SafeRuleID — the
// "corrupted row" arm's rule_id is a runtime-built slug-shaped credential
// that the fold and the location list would then print verbatim, RED.
func TestRenderPRStatus_DiffSecretsSuppressed(t *testing.T) {
	const admit = "abcdef0123456789abcdef0123456789abcdef01"
	status := &diffsecrets.AllowlistStatusRecord{Path: diffsecrets.AllowlistPath, Ref: admit, Status: diffsecrets.StatusLoaded, RejectedCount: 2}
	fpE, fpM := "1111111111111111", "2222222222222222"
	sum := diffsecrets.SuppressionSummary{
		Allowlist: status, SuppressedHitCount: 3, EntrySuppressedHitCount: 2, MarkerSuppressedHitCount: 1,
		Suppressed: []diffsecrets.SuppressionRecord{
			{Path: "docs/p.md", Lines: []int{3, 7}, Pattern: "github-pat-classic", By: diffsecrets.ByEntry, RuleID: "docs-example", RuleFingerprint: fpE},
			{Path: "fix/a.go", Lines: []int{2}, Pattern: "github-pat-classic", By: diffsecrets.ByMarker, RuleID: "fixtures", RuleFingerprint: fpM, CheckKey: "k1"},
		},
	}
	t.Run("renders totals, rules, locations, ref and rejected", func(t *testing.T) {
		body := renderDiffSecretsPR(t, diffSecretsEntry(t, 4, sum))
		for _, want := range []string{
			"**Diff secrets suppressed**",
			"3 hits suppressed: 2 by allowlist entries, 1 by fixture marker.",
			"admission commit `abcdef012345`", "2 rules rejected",
			"- entry rule `docs-example` (`11111111`): 2 hits",
			"- marker rule `fixtures` (`22222222`): 1 hit",
			"- `fix/a.go:2` (github-pat-classic) - scope `fixtures`",
			"low, human-only concern",
		} {
			if !strings.Contains(body, want) {
				t.Errorf("body missing %q:\n%s", want, body)
			}
		}
		if strings.Contains(body, fpE) || strings.Contains(body, "docs/p.md:3") {
			t.Errorf("full fingerprint or an entry-suppressed location rendered:\n%s", body)
		}
	})
	t.Run("union across rounds takes the newest lines", func(t *testing.T) {
		newer := sum
		newer.Suppressed = []diffsecrets.SuppressionRecord{
			{Path: "fix/a.go", Lines: []int{5}, Pattern: "github-pat-classic", By: diffsecrets.ByMarker, RuleID: "fixtures", RuleFingerprint: fpM, CheckKey: "k1"},
		}
		body := renderDiffSecretsPR(t, diffSecretsEntry(t, 9, newer), diffSecretsEntry(t, 4, sum))
		if !strings.Contains(body, "`fix/a.go:5`") || strings.Contains(body, "`fix/a.go:2`") ||
			!strings.Contains(body, "3 hits suppressed: 2 by allowlist entries, 1 by fixture marker.") {
			t.Errorf("union wrong:\n%s", body)
		}
	})
	t.Run("absent with nothing suppressed or rejected", func(t *testing.T) {
		empty := diffsecrets.SuppressionSummary{Allowlist: &diffsecrets.AllowlistStatusRecord{Status: diffsecrets.StatusAbsent}, Suppressed: []diffsecrets.SuppressionRecord{}}
		for name, entries := range map[string][]*audit.Entry{
			"no entries":      nil,
			"nothing":         {diffSecretsEntry(t, 1, empty)},
			"pre-#5230 shape": {prAuditEntry(1, "diff_secrets_detected", map[string]any{"hit_count": 2, "groups": []any{}})},
			"undecodable":     {{Sequence: 1, Category: "diff_secrets_detected", Payload: []byte("{not json")}},
		} {
			if body := renderDiffSecretsPR(t, entries...); strings.Contains(body, "Diff secrets suppressed") {
				t.Errorf("%s: section rendered:\n%s", name, body)
			}
		}
	})
	t.Run("rejected only renders", func(t *testing.T) {
		rej := diffsecrets.SuppressionSummary{Allowlist: &diffsecrets.AllowlistStatusRecord{Status: diffsecrets.StatusLoaded, RejectedCount: 1}}
		if body := renderDiffSecretsPR(t, diffSecretsEntry(t, 1, rej)); !strings.Contains(body, "1 rule rejected") {
			t.Errorf("rejected-only section missing:\n%s", body)
		}
	})
	t.Run("caps marker locations", func(t *testing.T) {
		many := diffsecrets.SuppressionSummary{Allowlist: status}
		for i := 0; i < prMarkerLocationCap+3; i++ {
			many.Suppressed = append(many.Suppressed, diffsecrets.SuppressionRecord{
				Path: "fix/f" + string(rune('a'+i)) + ".go", Lines: []int{1}, Pattern: "github-pat-classic",
				By: diffsecrets.ByMarker, RuleID: "fixtures", RuleFingerprint: fpM, CheckKey: "k" + string(rune('a'+i))})
		}
		body := renderDiffSecretsPR(t, diffSecretsEntry(t, 1, many))
		if !strings.Contains(body, "- and 3 more") || strings.Count(body, " - scope `fixtures`") != prMarkerLocationCap {
			t.Errorf("cap wrong:\n%s", body)
		}
	})
	t.Run("sanitizes paths", func(t *testing.T) {
		evil := diffsecrets.SuppressionSummary{Allowlist: status, Suppressed: []diffsecrets.SuppressionRecord{
			{Path: "fix/a`b\n## injected.go", Lines: []int{1}, Pattern: "github-pat-classic", By: diffsecrets.ByMarker, RuleID: "fixtures", RuleFingerprint: fpM, CheckKey: "k"},
		}}
		body := renderDiffSecretsPR(t, diffSecretsEntry(t, 1, evil))
		if strings.Contains(body, "a`b") || strings.Contains(body, "\n## injected") || !strings.Contains(body, "`fix/a'b ## injected.go:1`") {
			t.Errorf("path not sanitized:\n%s", body)
		}
	})
	t.Run("corrupted rule id renders invalid", func(t *testing.T) {
		cred := "glpat-" + strings.Repeat("n", 20)
		bad := diffsecrets.SuppressionSummary{Allowlist: status, Suppressed: []diffsecrets.SuppressionRecord{
			{Path: "fix/a.go", Lines: []int{1}, Pattern: "github-pat-classic", By: diffsecrets.ByMarker, RuleID: cred, RuleFingerprint: "ghp_" + strings.Repeat("Q", 36), CheckKey: "k"},
		}}
		body := renderDiffSecretsPR(t, diffSecretsEntry(t, 1, bad))
		if strings.Contains(body, cred) || strings.Contains(body, "ghp_") || !strings.Contains(body, diffsecrets.InvalidRuleID) {
			t.Errorf("corrupted row leaks:\n%s", body)
		}
	})
	t.Run("kept at degradation level 2", func(t *testing.T) {
		// Enough fix-up passes that only level 2 (which drops the fix-up
		// history) fits under the cap.
		entries := []*audit.Entry{diffSecretsEntry(t, 1, sum)}
		for i := 0; i < 3000; i++ {
			entries = append(entries, prAuditEntry(int64(10+i), "fixup_pushed", map[string]any{"head_sha": "1111111111112222", "files_changed_count": 1, "apply_path": "git_apply"}))
		}
		body := renderDiffSecretsPR(t, entries...)
		if len(body) > MaxIssueCommentBodyBytes || strings.Contains(body, "**Fix-up history**") {
			t.Fatalf("not at level 2: %d bytes, fix-ups present=%v", len(body), strings.Contains(body, "**Fix-up history**"))
		}
		if !strings.Contains(body, "**Diff secrets suppressed**") || !strings.Contains(body, "`fix/a.go:2`") {
			t.Errorf("section dropped at level 2:\n%s", body)
		}
	})
}
