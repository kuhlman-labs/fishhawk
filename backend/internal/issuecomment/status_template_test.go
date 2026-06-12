package issuecomment_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/issuecomment"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// statusRun returns a run+stages fixture in a typical mid-flight
// shape: plan succeeded, implement running, review pending. Tests
// mutate it for the variant scenarios.
func statusRun(t *testing.T, runID uuid.UUID) (*run.Run, []*run.Stage) {
	t.Helper()
	r := &run.Run{
		ID:            runID,
		Repo:          "kuhlman-labs/fishhawk",
		WorkflowID:    "feature_change",
		TriggerSource: run.TriggerGitHubIssue,
		State:         run.StateRunning,
	}
	stages := []*run.Stage{
		{ID: uuid.New(), RunID: runID, Sequence: 1, Type: run.StageTypePlan, State: run.StageStateSucceeded},
		{ID: uuid.New(), RunID: runID, Sequence: 2, Type: run.StageTypeImplement, State: run.StageStateRunning},
		{ID: uuid.New(), RunID: runID, Sequence: 3, Type: run.StageTypeReview, State: run.StageStatePending},
	}
	return r, stages
}

func auditEntry(runID uuid.UUID, sequence int64, category string, actor string, ts time.Time, payload map[string]any) *audit.Entry {
	r := runID
	body, _ := json.Marshal(payload)
	var actorPtr *string
	if actor != "" {
		actorPtr = &actor
	}
	return &audit.Entry{
		ID:           uuid.New(),
		Sequence:     sequence,
		RunID:        &r,
		Timestamp:    ts,
		Category:     category,
		ActorSubject: actorPtr,
		Payload:      body,
	}
}

func TestRenderStatusBody_HeaderCarriesShortIDAndWorkflowAndState(t *testing.T) {
	runID := uuid.MustParse("7be5974b-c389-4577-a5a9-43510cadca88")
	r, stages := statusRun(t, runID)

	body := issuecomment.RenderStatusBody(r, stages, nil,
		"https://app.fishhawk.example.com",
		time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC))

	for _, want := range []string{
		"Fishhawk run",
		"7be5974b",
		"https://app.fishhawk.example.com/runs/7be5974b-c389-4577-a5a9-43510cadca88",
		"feature_change",
		"running",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("header missing %q\n---\n%s", want, body)
		}
	}
}

func TestRenderStatusBody_StagesEachStateRendered(t *testing.T) {
	// One row per stage, each with a state-icon. Cover every state
	// the state machine emits so a future state-machine extension
	// is caught by this test rather than silently rendering "❓".
	runID := uuid.New()
	r, _ := statusRun(t, runID)
	stages := []*run.Stage{
		{ID: uuid.New(), Sequence: 1, Type: run.StageTypePlan, State: run.StageStatePending},
		{ID: uuid.New(), Sequence: 2, Type: run.StageTypePlan, State: run.StageStateDispatched},
		{ID: uuid.New(), Sequence: 3, Type: run.StageTypePlan, State: run.StageStateRunning},
		{ID: uuid.New(), Sequence: 4, Type: run.StageTypePlan, State: run.StageStateAwaitingApproval},
		{ID: uuid.New(), Sequence: 5, Type: run.StageTypePlan, State: run.StageStateSucceeded},
		{ID: uuid.New(), Sequence: 6, Type: run.StageTypePlan, State: run.StageStateFailed},
		{ID: uuid.New(), Sequence: 7, Type: run.StageTypePlan, State: run.StageStateCancelled},
	}
	body := issuecomment.RenderStatusBody(r, stages, nil, "https://x", time.Now())
	// Each state-text should appear; if the renderer falls back to
	// the "❓" glyph for an unknown state the substring check still
	// passes (state text is present), but it'd surface as a missing
	// icon in a follow-up assertion. Closed-set guard:
	for _, icon := range []string{"⏳", "🚀", "🔄", "👋", "✅", "❌", "🚫"} {
		if !strings.Contains(body, icon) {
			t.Errorf("stage section missing icon %q\n---\n%s", icon, body)
		}
	}
	if strings.Contains(body, "❓") {
		t.Errorf("rendered the unknown-state fallback; covered set should be complete\n---\n%s", body)
	}
}

func TestRenderStatusBody_NoPullRequestURL_OmitsLink(t *testing.T) {
	r, stages := statusRun(t, uuid.New())
	r.PullRequestURL = nil
	body := issuecomment.RenderStatusBody(r, stages, nil, "https://x", time.Now())
	if strings.Contains(body, "Pull request") {
		t.Errorf("body should not contain Pull request link when URL nil\n---\n%s", body)
	}
	if !strings.Contains(body, "View run") {
		t.Errorf("body should always carry the View run link")
	}
}

func TestRenderStatusBody_WithPullRequestURL_RendersLink(t *testing.T) {
	r, stages := statusRun(t, uuid.New())
	prURL := "https://github.com/kuhlman-labs/fishhawk/pull/42"
	r.PullRequestURL = &prURL
	body := issuecomment.RenderStatusBody(r, stages, nil, "https://x", time.Now())
	if !strings.Contains(body, prURL) {
		t.Errorf("body should carry the PR URL\n---\n%s", body)
	}
}

func TestRenderStatusBody_RecentAudit_CappedAtThreeAndOrderedMostRecent(t *testing.T) {
	// Five interesting entries; renderer takes the 3 with the
	// highest sequence and orders them most-recent first.
	runID := uuid.New()
	r, stages := statusRun(t, runID)
	now := time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)
	entries := []*audit.Entry{
		auditEntry(runID, 1, "run_dispatched", "github-webhook", now.Add(-2*time.Hour), nil),
		auditEntry(runID, 2, "plan_generated", "system", now.Add(-90*time.Minute), nil),
		auditEntry(runID, 3, "approval_submitted", "alice", now.Add(-60*time.Minute), map[string]any{"decision": "approve"}),
		auditEntry(runID, 4, "pr_merged", "alice", now.Add(-5*time.Minute), nil),
		auditEntry(runID, 5, "pr_approved_on_github", "bob", now.Add(-2*time.Minute), nil),
	}
	body := issuecomment.RenderStatusBody(r, stages, entries, "https://x", now)

	// Top 3 by sequence: 5, 4, 3 → bob, alice, alice.
	if !strings.Contains(body, "@bob approved on GitHub") {
		t.Errorf("top entry should be @bob's GitHub approval\n---\n%s", body)
	}
	if !strings.Contains(body, "@alice merged the PR") {
		t.Errorf("second entry should be alice's merge\n---\n%s", body)
	}
	if !strings.Contains(body, "@alice approved the plan") {
		t.Errorf("third entry should be alice's plan approval\n---\n%s", body)
	}
	// Plan-generated (seq 2) and run-dispatched (seq 1) are below
	// the cap — they shouldn't appear.
	if strings.Contains(body, "Plan posted") {
		t.Errorf("plan_generated should be capped out (limit=3)\n---\n%s", body)
	}
	// Most-recent first ordering: bob (2m) before alice merged (5m)
	// before alice approved (60m).
	idxBob := strings.Index(body, "@bob approved")
	idxMerged := strings.Index(body, "@alice merged")
	idxApproved := strings.Index(body, "@alice approved the plan")
	if idxBob >= idxMerged || idxMerged >= idxApproved {
		t.Errorf("activity not ordered most-recent first\n---\n%s", body)
	}
}

func TestRenderStatusBody_FiltersNoisyAuditCategories(t *testing.T) {
	// status_comment_posted (recursive), trace_uploaded (per-stage
	// noise), and installation_token_issued (operator-irrelevant)
	// must NOT appear in the activity section.
	runID := uuid.New()
	r, stages := statusRun(t, runID)
	now := time.Now()
	entries := []*audit.Entry{
		auditEntry(runID, 10, "status_comment_posted", "system", now.Add(-1*time.Minute), nil),
		auditEntry(runID, 9, "trace_uploaded", "system", now.Add(-2*time.Minute), nil),
		auditEntry(runID, 8, "installation_token_issued", "system", now.Add(-3*time.Minute), nil),
		auditEntry(runID, 7, "pr_merged", "alice", now.Add(-5*time.Minute), nil),
	}
	body := issuecomment.RenderStatusBody(r, stages, entries, "https://x", now)
	if !strings.Contains(body, "@alice merged the PR") {
		t.Errorf("interesting pr_merged should render\n---\n%s", body)
	}
	for _, noise := range []string{"status_comment_posted", "trace_uploaded", "installation_token_issued"} {
		if strings.Contains(body, noise) {
			t.Errorf("noisy category %q should be filtered\n---\n%s", noise, body)
		}
	}
}

func TestRenderStatusBody_NoActivity_OmitsLatestSection(t *testing.T) {
	r, stages := statusRun(t, uuid.New())
	body := issuecomment.RenderStatusBody(r, stages, nil, "https://x", time.Now())
	if strings.Contains(body, "Latest activity") {
		t.Errorf("empty audit list should omit the Latest activity section\n---\n%s", body)
	}
}

func TestRenderStatusBody_NoStages_RendersPlaceholder(t *testing.T) {
	r, _ := statusRun(t, uuid.New())
	body := issuecomment.RenderStatusBody(r, nil, nil, "https://x", time.Now())
	if !strings.Contains(body, "No stages yet") {
		t.Errorf("expected no-stages placeholder\n---\n%s", body)
	}
}

func TestRenderStatusBody_NilRun_ReturnsEmpty(t *testing.T) {
	if got := issuecomment.RenderStatusBody(nil, nil, nil, "https://x", time.Now()); got != "" {
		t.Errorf("nil run should produce empty body; got %q", got)
	}
}

func TestRenderStatusBody_RelativeAge(t *testing.T) {
	// Cover the time-bucket switches in relativeAge: just now, m,
	// h, d, absolute. Each bucket gets exactly one audit row so
	// the assertion can look for the right suffix.
	runID := uuid.New()
	r, stages := statusRun(t, runID)
	now := time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		offset time.Duration
		want   string
	}{
		{0, "just now"},
		{30 * time.Second, "just now"},
		{5 * time.Minute, "5m ago"},
		{2 * time.Hour, "2h ago"},
		{2 * 24 * time.Hour, "2d ago"},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			entries := []*audit.Entry{
				auditEntry(runID, 1, "pr_merged", "alice", now.Add(-tc.offset), nil),
			}
			body := issuecomment.RenderStatusBody(r, stages, entries, "https://x", now)
			if !strings.Contains(body, tc.want) {
				t.Errorf("expected %q in body\n---\n%s", tc.want, body)
			}
		})
	}
}

func TestRenderStatusBody_RelativeAge_DistantPast_RendersAbsoluteDate(t *testing.T) {
	runID := uuid.New()
	r, stages := statusRun(t, runID)
	now := time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)
	entries := []*audit.Entry{
		auditEntry(runID, 1, "pr_merged", "alice", now.AddDate(0, -2, 0), nil),
	}
	body := issuecomment.RenderStatusBody(r, stages, entries, "https://x", now)
	if !strings.Contains(body, "Mar 14") {
		t.Errorf("distant-past timestamp should render as absolute Jan 2 format\n---\n%s", body)
	}
}

func TestRenderStatusBody_ApprovalDecisionVerb(t *testing.T) {
	// approval_submitted's payload carries decision: approve | reject.
	// The rendered line uses the right verb.
	runID := uuid.New()
	r, stages := statusRun(t, runID)
	now := time.Now()
	cases := []struct {
		decision string
		want     string
	}{
		{"approve", "@alice approved the plan"},
		{"reject", "@alice rejected the plan"},
		{"", "@alice acted on the plan"}, // missing decision in payload
	}
	for _, tc := range cases {
		t.Run(tc.decision, func(t *testing.T) {
			payload := map[string]any{}
			if tc.decision != "" {
				payload["decision"] = tc.decision
			}
			entries := []*audit.Entry{
				auditEntry(runID, 1, "approval_submitted", "alice", now.Add(-1*time.Minute), payload),
			}
			body := issuecomment.RenderStatusBody(r, stages, entries, "https://x", now)
			if !strings.Contains(body, tc.want) {
				t.Errorf("expected %q\n---\n%s", tc.want, body)
			}
		})
	}
}

// TestRenderStatusBody_ApproverMention_PrefersGithubLogin guards #755: the
// "Latest activity" approval row must @-mention the resolved GitHub login
// (#751, approver_github_login) — and must NEVER @-mention the raw MCP token
// subject (brett@local-mcp), which would ping an unrelated real user.
func TestRenderStatusBody_ApproverMention_PrefersGithubLogin(t *testing.T) {
	runID := uuid.New()
	r, stages := statusRun(t, runID)
	now := time.Now()

	t.Run("resolved github login preferred over token subject", func(t *testing.T) {
		entries := []*audit.Entry{
			auditEntry(runID, 1, "approval_submitted", "brett@local-mcp", now.Add(-1*time.Minute),
				map[string]any{"decision": "approve", "approver_github_login": "kuhlman-labs"}),
		}
		body := issuecomment.RenderStatusBody(r, stages, entries, "https://x", now)
		if !strings.Contains(body, "@kuhlman-labs approved the plan") {
			t.Errorf("expected @kuhlman-labs mention\n---\n%s", body)
		}
		if strings.Contains(body, "brett@local-mcp") {
			t.Errorf("must NOT render the raw token subject (#755)\n---\n%s", body)
		}
	})

	t.Run("non-login subject with no resolved login renders verbatim in a code span", func(t *testing.T) {
		entries := []*audit.Entry{
			auditEntry(runID, 1, "approval_submitted", "brett@local-mcp", now.Add(-1*time.Minute),
				map[string]any{"decision": "approve"}),
		}
		body := issuecomment.RenderStatusBody(r, stages, entries, "https://x", now)
		if !strings.Contains(body, "`brett@local-mcp` approved the plan") {
			t.Errorf("expected the verbatim code-span form (#1053)\n---\n%s", body)
		}
		if strings.Contains(body, "@brett") {
			t.Errorf("must NOT @-mention a non-login subject (#755)\n---\n%s", body)
		}
	})

	t.Run("operator-agent subject names the role and the delegation rule", func(t *testing.T) {
		entries := []*audit.Entry{
			auditEntry(runID, 1, "approval_submitted", "operator-agent/operator-role-v0", now.Add(-1*time.Minute),
				map[string]any{
					"decision":  "approve",
					"approver":  "operator-agent/operator-role-v0",
					"delegated": "clean_dual_approval",
				}),
		}
		body := issuecomment.RenderStatusBody(r, stages, entries, "https://x", now)
		want := "the operator agent (`operator-agent/operator-role-v0`, delegated: `clean_dual_approval`) approved the plan"
		if !strings.Contains(body, want) {
			t.Errorf("expected %q (#1053 / ADR-040 attribution)\n---\n%s", want, body)
		}
	})

	t.Run("hostile delegated rule is contained in its own code span", func(t *testing.T) {
		// The rule is read from the audit payload; even though the
		// server only writes workflow-spec rule identifiers, the
		// activity line must sanitize it like the subject so the
		// delegated clause can't re-enable markdown or a mention.
		entries := []*audit.Entry{
			auditEntry(runID, 1, "approval_submitted", "operator-agent/operator-role-v0", now.Add(-1*time.Minute),
				map[string]any{
					"decision":  "approve",
					"approver":  "operator-agent/operator-role-v0",
					"delegated": "rule`@kuhlman-labs\n**bold**",
				}),
		}
		body := issuecomment.RenderStatusBody(r, stages, entries, "https://x", now)
		want := "the operator agent (`operator-agent/operator-role-v0`, delegated: `rule'@kuhlman-labs**bold**`) approved the plan"
		if !strings.Contains(body, want) {
			t.Errorf("expected sanitized rule clause\n---\n%s", body)
		}
		if strings.Contains(body, " @kuhlman-labs") {
			t.Errorf("delegated rule leaked a bare @-mention\n---\n%s", body)
		}
	})
}

func TestRenderStatusBody_CIRetryAttemptSuffix(t *testing.T) {
	// ci_failure_retry_dispatched payload carries retry_attempt +
	// max_retries; the rendered line includes "(attempt N/M)".
	runID := uuid.New()
	r, stages := statusRun(t, runID)
	now := time.Now()
	entries := []*audit.Entry{
		auditEntry(runID, 1, "ci_failure_retry_dispatched", "github-webhook",
			now.Add(-1*time.Minute),
			map[string]any{"retry_attempt": 1, "max_retries": 2}),
	}
	body := issuecomment.RenderStatusBody(r, stages, entries, "https://x", now)
	if !strings.Contains(body, "CI failed; retry dispatched (attempt 1/2)") {
		t.Errorf("expected retry-attempt suffix\n---\n%s", body)
	}
}

func TestRenderStatusBody_ActorlessRow_RendersWithoutAtMention(t *testing.T) {
	// run_dispatched fires with actor_subject="github-webhook" or
	// nil — neither should render as "@github-webhook" in the body.
	runID := uuid.New()
	r, stages := statusRun(t, runID)
	now := time.Now()
	entries := []*audit.Entry{
		auditEntry(runID, 1, "run_dispatched", "github-webhook", now.Add(-1*time.Minute), nil),
	}
	body := issuecomment.RenderStatusBody(r, stages, entries, "https://x", now)
	if strings.Contains(body, "@github-webhook") {
		t.Errorf("system actor should not render as @mention\n---\n%s", body)
	}
	if !strings.Contains(body, "Fishhawk run dispatched") {
		t.Errorf("run_dispatched line should still render\n---\n%s", body)
	}
}

func TestRenderStatusBody_StateIconsForRunLevel(t *testing.T) {
	// Header icon tracks run.State.
	runID := uuid.New()
	cases := []struct {
		state run.State
		icon  string
	}{
		{run.StatePending, "⏳"},
		{run.StateRunning, "🔄"},
		{run.StateSucceeded, "✅"},
		{run.StateFailed, "❌"},
		{run.StateCancelled, "🚫"},
	}
	for _, tc := range cases {
		t.Run(string(tc.state), func(t *testing.T) {
			r, stages := statusRun(t, runID)
			r.State = tc.state
			body := issuecomment.RenderStatusBody(r, stages, nil, "https://x", time.Now())
			// The icon should appear next to the state text in the
			// header. Loose match is fine; specific positioning
			// would over-pin the template.
			if !strings.Contains(body, tc.icon) {
				t.Errorf("header missing icon %q for state %q\n---\n%s", tc.icon, tc.state, body)
			}
		})
	}
}
