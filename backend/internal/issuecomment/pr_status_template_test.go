package issuecomment

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
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
	if !strings.Contains(body, "claude-opus-4-8: approve") {
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
}

// TestRenderPRStatusBody_NilRun returns empty for a nil run.
func TestRenderPRStatusBody_NilRun(t *testing.T) {
	if got := RenderPRStatusBody(PRStatusInput{}); got != "" {
		t.Errorf("nil run should render empty, got %q", got)
	}
}
