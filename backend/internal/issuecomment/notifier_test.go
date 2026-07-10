package issuecomment_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/drive"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/issuecomment"
	"github.com/kuhlman-labs/fishhawk/backend/internal/latency"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// happyDeps wires a Notifier against an issue-triggered run with no
// prior comment audit entries. Returns the notifier and the fakes
// so tests can assert on emitted state.
func happyDeps(t *testing.T) (uuid.UUID, *fakeGitHub, *fakeAudit, *issuecomment.Notifier) {
	t.Helper()
	runID := uuid.New()
	triggerRef := "issue:42"
	repoRuns := &fakeRuns{
		runs: map[uuid.UUID]*run.Run{runID: {
			ID:             runID,
			Repo:           "x/y",
			WorkflowID:     "feature_change",
			TriggerSource:  run.TriggerGitHubIssue,
			TriggerRef:     &triggerRef,
			InstallationID: int64Ptr(99),
		}},
	}
	gh := &fakeGitHub{}
	au := &fakeAudit{}
	n := issuecomment.New(issuecomment.Deps{
		GitHub:      gh,
		Runs:        repoRuns,
		Audit:       au,
		ExternalURL: "https://app.fishhawk.example.com",
		Now:         func() time.Time { return time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC) },
	})
	if n == nil {
		t.Fatal("notifier nil")
	}
	return runID, gh, au, n
}

// #751 fix at the render seam: when the approval audit payload carries
// a resolved approver_github_login, the footer `@`-mentions THAT login
// even though the provenance `approver` is the raw MCP token subject
// (brett@local-mcp). Without the resolved login, the bare token
// subject renders verbatim inside a code span (#1053) — never as an
// `@`-mention of an unrelated GitHub user.
func TestPlanStatusFooterForAuditPayload_PrefersGithubLogin(t *testing.T) {
	withLogin := mustJSON(t, map[string]any{
		"decision":              "approve",
		"approver":              "brett@local-mcp",
		"approver_github_login": "kuhlman-labs",
	})
	got := issuecomment.PlanStatusFooterForAuditPayload(withLogin)
	if got != "_Status: approved by @kuhlman-labs · implementing now_" {
		t.Errorf("with resolved login, footer = %q, want @kuhlman-labs mention", got)
	}

	bareSubject := mustJSON(t, map[string]any{
		"decision": "approve",
		"approver": "brett@local-mcp",
	})
	got = issuecomment.PlanStatusFooterForAuditPayload(bareSubject)
	if got != "_Status: approved by `brett@local-mcp` · implementing now_" {
		t.Errorf("with bare token subject, footer = %q, want verbatim code-span form (no ping)", got)
	}
}

// TestPlanStatusFooterForAuditPayload_IdentityForms pins the #1053
// three-form identity convention on BOTH approve and reject footer
// branches: resolved login → `@`-mention; operator-agent token subject
// → role instance named, plus the ADR-040 delegation rule when the
// payload recorded one; any other non-login subject → verbatim inside
// a code span with no bare leading `@` (the #751 stop-the-ping
// guarantee); "an approver" strictly for empty / "anonymous". The
// backtick cases pin the security amendment: a subject containing
// backticks (single, double run, or only backticks) must stay inside
// one literal code span — backticks are replaced before wrapping, so
// the subject can never close the span and re-enable markdown or an
// @-mention.
func TestPlanStatusFooterForAuditPayload_IdentityForms(t *testing.T) {
	cases := []struct {
		name      string
		payload   map[string]any
		wantActor string
	}{
		{
			name: "resolved login",
			payload: map[string]any{
				"approver":              "brett@local-mcp",
				"approver_github_login": "kuhlman-labs",
			},
			wantActor: "@kuhlman-labs",
		},
		{
			name: "operator-agent subject with delegated rule",
			payload: map[string]any{
				"approver":  "operator-agent/operator-role-v0",
				"delegated": "clean_dual_approval",
			},
			wantActor: "the operator agent (`operator-agent/operator-role-v0`, delegated: `clean_dual_approval`)",
		},
		{
			name: "operator-agent subject without delegated field",
			payload: map[string]any{
				"approver": "operator-agent/operator-role-v0",
			},
			wantActor: "the operator agent (`operator-agent/operator-role-v0`)",
		},
		{
			// The delegated rule comes from the audit payload; even
			// though writeApprovalAudit only ever stamps a workflow-spec
			// rule identifier, the render must contain a hostile value
			// the same way it contains the subject — backticks replaced,
			// newlines dropped, leading `@` stripped, all inside one
			// code span — so the rule clause can never re-enable
			// markdown or an @-mention.
			name: "delegated rule with backticks, newline, and mention sanitized into its own code span",
			payload: map[string]any{
				"approver":  "operator-agent/operator-role-v0",
				"delegated": "rule`@kuhlman-labs\n**bold**",
			},
			wantActor: "the operator agent (`operator-agent/operator-role-v0`, delegated: `rule'@kuhlman-labs**bold**`)",
		},
		{
			// A rule that sanitizes to empty (only control characters)
			// falls back to the no-rule parenthetical rather than
			// rendering an empty code span.
			name: "delegated rule that sanitizes to empty drops the rule clause",
			payload: map[string]any{
				"approver":  "operator-agent/operator-role-v0",
				"delegated": "\n\t\x07",
			},
			wantActor: "the operator agent (`operator-agent/operator-role-v0`)",
		},
		{
			name:      "other non-login subject renders verbatim in a code span",
			payload:   map[string]any{"approver": "brett@local-mcp"},
			wantActor: "`brett@local-mcp`",
		},
		{
			name:      "single backtick replaced inside the span",
			payload:   map[string]any{"approver": "evil`name"},
			wantActor: "`evil'name`",
		},
		{
			name:      "double backtick run replaced inside the span",
			payload:   map[string]any{"approver": "evil``@kuhlman-labs"},
			wantActor: "`evil''@kuhlman-labs`",
		},
		{
			name:      "subject that is only backticks stays one literal span",
			payload:   map[string]any{"approver": "```"},
			wantActor: "`'''`",
		},
		{
			// Sanitizing can yield "" (every rune dropped); the caller
			// must fall back to "an approver" rather than render an
			// empty code span.
			name:      "subject that is only control characters falls back to an approver",
			payload:   map[string]any{"approver": "\n\t\x07\x1b"},
			wantActor: "an approver",
		},
		{
			// A pathological subject is capped at maxRenderedSubjectRunes
			// (64) so it can't balloon the comment.
			name:      "over-long subject is capped at 64 runes",
			payload:   map[string]any{"approver": strings.Repeat("a", 70)},
			wantActor: "`" + strings.Repeat("a", 64) + "`",
		},
		{
			name:      "empty subject",
			payload:   map[string]any{"approver": ""},
			wantActor: "an approver",
		},
		{
			name:      "anonymous placeholder",
			payload:   map[string]any{"approver": "anonymous"},
			wantActor: "an approver",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for decision, want := range map[string]string{
				"approve": fmt.Sprintf("_Status: approved by %s · implementing now_", tc.wantActor),
				"reject":  fmt.Sprintf("_Status: rejected by %s_", tc.wantActor),
			} {
				payload := map[string]any{"decision": decision}
				for k, v := range tc.payload {
					payload[k] = v
				}
				got := issuecomment.PlanStatusFooterForAuditPayload(mustJSON(t, payload))
				if got != want {
					t.Errorf("%s footer = %q, want %q", decision, got, want)
				}
				// Stop-the-ping (#751): no bare leading `@` outside a
				// code span — every rendered `@` must be the resolved
				// login mention or sit inside backticks.
				if tc.wantActor != "@kuhlman-labs" && strings.Contains(got, " @") {
					t.Errorf("%s footer %q carries a bare @-prefix (mention risk)", decision, got)
				}
			}
		})
	}
}

// TestPlanStatusFooterForAuditPayload_LoginValidation exercises the
// tightened login validator (#751) through the exported render seam:
// a syntactically-valid GitHub login renders as an `@`-mention; any
// non-login string (containing '@'/'.', the "anonymous" placeholder,
// empty, leading/trailing hyphen, or over 39 chars) falls back to
// "an approver". The login is supplied as approver_github_login while
// `approver` is left empty so the fallback is unambiguously "an
// approver" on rejection.
func TestPlanStatusFooterForAuditPayload_LoginValidation(t *testing.T) {
	maxLogin := strings.Repeat("a", 39)
	overLogin := strings.Repeat("a", 40)
	cases := []struct {
		name  string
		login string
		valid bool
	}{
		{"plain login", "kuhlman-labs", true},
		{"single char", "a", true},
		{"39 chars max", maxLogin, true},
		{"internal hyphen", "a-b-c", true},
		{"mcp token subject", "brett@local-mcp", false},
		{"anonymous placeholder", "anonymous", false},
		{"empty", "", false},
		{"leading hyphen", "-foo", false},
		{"trailing hyphen", "foo-", false},
		{"40 chars over cap", overLogin, false},
		{"dotted", "foo.bar", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload := mustJSON(t, map[string]any{
				"decision":              "approve",
				"approver":              "",
				"approver_github_login": tc.login,
			})
			got := issuecomment.PlanStatusFooterForAuditPayload(payload)
			if tc.valid {
				want := fmt.Sprintf("_Status: approved by @%s · implementing now_", tc.login)
				if got != want {
					t.Errorf("footer = %q, want %q", got, want)
				}
				return
			}
			if got != "_Status: approved by an approver · implementing now_" {
				t.Errorf("footer = %q, want fallback to \"an approver\"", got)
			}
		})
	}
}

// TestPlanStatusFooterForAuditPayload_NoDecisionAndMalformed confirms
// the exported render seam returns "" for a payload with no terminal
// decision and for a malformed payload (the awaiting-approval and
// corrupt-row cases the live notifier treats as no-status-yet).
func TestPlanStatusFooterForAuditPayload_NoDecisionAndMalformed(t *testing.T) {
	noDecision := mustJSON(t, map[string]any{"approver": "kuhlman-labs"})
	if got := issuecomment.PlanStatusFooterForAuditPayload(noDecision); got != "" {
		t.Errorf("no-decision footer = %q, want empty", got)
	}
	if got := issuecomment.PlanStatusFooterForAuditPayload([]byte("not json")); got != "" {
		t.Errorf("malformed footer = %q, want empty", got)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func TestNotifyCIRetry_PostsCommentAndAuditEntry(t *testing.T) {
	runID, gh, au, n := happyDeps(t)
	parentID := uuid.New()
	if err := n.NotifyCIRetry(context.Background(), runID, parentID, "ci/build", 1, 1); err != nil {
		t.Fatalf("NotifyCIRetry: %v", err)
	}
	if len(gh.calls) != 1 {
		t.Fatalf("expected 1 GitHub call; got %d", len(gh.calls))
	}
	body := gh.calls[0].body
	if !strings.Contains(body, "ci/build") || !strings.Contains(body, "Retry attempt 1 of 1") {
		t.Errorf("body missing expected text: %q", body)
	}
	if !strings.Contains(body, parentID.String()[:8]) {
		t.Errorf("body should include parent short id: %q", body)
	}
	if len(au.appended) != 1 {
		t.Fatalf("expected 1 audit entry; got %d", len(au.appended))
	}
	var p map[string]any
	if err := json.Unmarshal(au.appended[0].Payload, &p); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if p["kind"] != "ci_retry" {
		t.Errorf("payload.kind = %v, want ci_retry", p["kind"])
	}
	if attempt, _ := p["retry_attempt"].(float64); int(attempt) != 1 {
		t.Errorf("payload.retry_attempt = %v, want 1", p["retry_attempt"])
	}
}

func TestNotifyCIRetry_PerAttemptDedup(t *testing.T) {
	runID, gh, au, n := happyDeps(t)
	// Pre-seed a ci_retry audit at retry_attempt=1: a second
	// NotifyCIRetry for the same attempt is the redelivery case
	// and should skip; an attempt=2 call (different run, but for
	// stub-test purposes) still posts because the dedup is
	// per-attempt.
	au.preSeed(runID, issuecomment.CategoryIssueCommented, map[string]any{
		"kind":          "ci_retry",
		"retry_attempt": 1,
	})

	if err := n.NotifyCIRetry(context.Background(), runID, uuid.New(), "ci/build", 1, 1); err != nil {
		t.Fatal(err)
	}
	if len(gh.calls) != 0 {
		t.Errorf("attempt=1 should dedup; got %d calls", len(gh.calls))
	}
	if err := n.NotifyCIRetry(context.Background(), runID, uuid.New(), "ci/build", 2, 2); err != nil {
		t.Fatal(err)
	}
	if len(gh.calls) != 1 {
		t.Errorf("attempt=2 should post; got %d calls", len(gh.calls))
	}
	if len(au.appended) != 1 {
		t.Errorf("expected 1 new audit entry; got %d", len(au.appended))
	}
}

func TestNotifyCIRetry_SkipsNonIssueTrigger(t *testing.T) {
	runID := uuid.New()
	cliRef := "cli:adhoc"
	repoRuns := &fakeRuns{
		runs: map[uuid.UUID]*run.Run{runID: {
			ID: runID, Repo: "x/y",
			TriggerSource:  run.TriggerCLI,
			TriggerRef:     &cliRef,
			InstallationID: int64Ptr(99),
		}},
	}
	gh := &fakeGitHub{}
	au := &fakeAudit{}
	n := issuecomment.New(issuecomment.Deps{
		GitHub: gh, Runs: repoRuns, Audit: au, ExternalURL: "https://app.fishhawk.example.com",
	})
	if err := n.NotifyCIRetry(context.Background(), runID, uuid.New(), "ci/build", 1, 1); err != nil {
		t.Fatal(err)
	}
	if len(gh.calls) != 0 || len(au.appended) != 0 {
		t.Errorf("expected no GitHub / audit activity; got %d / %d", len(gh.calls), len(au.appended))
	}
}

// --- NotifyStatusUpdate (E20.2 / #328) ---

func TestNotifyStatusUpdate_NilReceiver_NoOp(t *testing.T) {
	var n *issuecomment.Notifier
	if err := n.NotifyStatusUpdate(context.Background(), uuid.New(), "body"); err != nil {
		t.Errorf("nil receiver should return nil; got %v", err)
	}
}

func TestNotifyStatusUpdate_EmptyBody_NoOp(t *testing.T) {
	// Caller didn't render anything (no transition worth surfacing).
	// Skip without touching GitHub or the audit log.
	runID, gh, au, n := happyDeps(t)
	if err := n.NotifyStatusUpdate(context.Background(), runID, ""); err != nil {
		t.Fatal(err)
	}
	if len(gh.calls)+len(gh.updateCalls) != 0 {
		t.Errorf("expected no GitHub activity on empty body; got %d/%d", len(gh.calls), len(gh.updateCalls))
	}
	if len(au.appended) != 0 {
		t.Errorf("expected no audit activity on empty body; got %d", len(au.appended))
	}
}

func TestNotifyStatusUpdate_NonIssueTrigger_NoOp(t *testing.T) {
	// CLI / PR-triggered runs don't have an originating issue;
	// the status comment has nowhere to land.
	runID := uuid.New()
	cliRef := "cli:adhoc"
	repoRuns := &fakeRuns{
		runs: map[uuid.UUID]*run.Run{runID: {
			ID: runID, Repo: "x/y",
			TriggerSource:  run.TriggerCLI,
			TriggerRef:     &cliRef,
			InstallationID: int64Ptr(99),
		}},
	}
	gh := &fakeGitHub{}
	au := &fakeAudit{}
	n := issuecomment.New(issuecomment.Deps{
		GitHub: gh, Runs: repoRuns, Audit: au,
		ExternalURL: "https://app.fishhawk.example.com",
	})
	if err := n.NotifyStatusUpdate(context.Background(), runID, "body"); err != nil {
		t.Fatal(err)
	}
	if len(gh.calls)+len(gh.updateCalls) != 0 || len(au.appended) != 0 {
		t.Errorf("expected no activity for non-issue-trigger; got gh=%d/%d audit=%d",
			len(gh.calls), len(gh.updateCalls), len(au.appended))
	}
}

func TestNotifyStatusUpdate_FirstCall_CreatesCommentAndAuditRow(t *testing.T) {
	runID, gh, au, n := happyDeps(t)
	if err := n.NotifyStatusUpdate(context.Background(), runID, "status v1"); err != nil {
		t.Fatalf("NotifyStatusUpdate: %v", err)
	}
	if len(gh.calls) != 1 {
		t.Fatalf("expected 1 create call; got %d", len(gh.calls))
	}
	if gh.calls[0].body != "status v1" {
		t.Errorf("body = %q", gh.calls[0].body)
	}
	if len(gh.updateCalls) != 0 {
		t.Errorf("expected no update calls on first invocation; got %d", len(gh.updateCalls))
	}
	if len(au.appended) != 1 {
		t.Fatalf("expected 1 audit row; got %d", len(au.appended))
	}
	row := au.appended[0]
	if row.Category != issuecomment.CategoryStatusCommentPosted {
		t.Errorf("category = %q, want %q", row.Category, issuecomment.CategoryStatusCommentPosted)
	}
	var body map[string]any
	if err := json.Unmarshal(row.Payload, &body); err != nil {
		t.Fatalf("decode audit payload: %v", err)
	}
	if body["kind"] != string(issuecomment.KindStatusUpdate) {
		t.Errorf("payload.kind = %v, want status_update", body["kind"])
	}
	// fakeGitHub assigns id=1 to the first create.
	if id, _ := body["github_comment_id"].(float64); int64(id) != 1 {
		t.Errorf("payload.github_comment_id = %v, want 1", body["github_comment_id"])
	}
}

func TestNotifyStatusUpdate_SubsequentCall_EditsExistingComment(t *testing.T) {
	// Pre-seed an existing status comment audit row; the second
	// call should call UpdateIssueComment with the seeded id instead
	// of creating a new comment.
	runID, gh, au, n := happyDeps(t)
	au.preSeed(runID, issuecomment.CategoryStatusCommentPosted, map[string]any{
		"kind":              string(issuecomment.KindStatusUpdate),
		"issue_number":      42,
		"repo":              "x/y",
		"github_comment_id": 4242,
	})

	if err := n.NotifyStatusUpdate(context.Background(), runID, "status v2"); err != nil {
		t.Fatalf("NotifyStatusUpdate: %v", err)
	}
	if len(gh.calls) != 0 {
		t.Errorf("expected no create on edit path; got %d", len(gh.calls))
	}
	if len(gh.updateCalls) != 1 {
		t.Fatalf("expected 1 update call; got %d", len(gh.updateCalls))
	}
	upd := gh.updateCalls[0]
	if upd.commentID != 4242 {
		t.Errorf("update.commentID = %d, want 4242 (from preseed)", upd.commentID)
	}
	if upd.body != "status v2" {
		t.Errorf("update.body = %q", upd.body)
	}
	// Fresh audit row for the updated state.
	if len(au.appended) != 1 {
		t.Fatalf("expected 1 audit row; got %d", len(au.appended))
	}
}

func TestNotifyStatusUpdate_404OnUpdate_FallsBackToCreate(t *testing.T) {
	// Operator manually deleted the prior status comment. PATCH
	// returns 404 → ErrNotFound; notifier falls back to creating a
	// fresh comment and recording its id in a new audit row.
	runID, gh, au, n := happyDeps(t)
	au.preSeed(runID, issuecomment.CategoryStatusCommentPosted, map[string]any{
		"kind":              string(issuecomment.KindStatusUpdate),
		"github_comment_id": 9999,
	})
	gh.updateErr = githubclient.ErrNotFound

	if err := n.NotifyStatusUpdate(context.Background(), runID, "status after delete"); err != nil {
		t.Fatalf("NotifyStatusUpdate: %v", err)
	}
	if len(gh.updateCalls) != 1 {
		t.Errorf("expected 1 update attempt; got %d", len(gh.updateCalls))
	}
	if len(gh.calls) != 1 {
		t.Errorf("expected 1 create fallback; got %d", len(gh.calls))
	}
	if len(au.appended) != 1 {
		t.Fatalf("expected 1 audit row; got %d", len(au.appended))
	}
	var body map[string]any
	_ = json.Unmarshal(au.appended[0].Payload, &body)
	// New comment id from the fallback create (fakeGitHub's len-based
	// id assignment; first create returns id=1).
	if id, _ := body["github_comment_id"].(float64); int64(id) != 1 {
		t.Errorf("payload.github_comment_id = %v, want 1 (fresh id from fallback create)", body["github_comment_id"])
	}
}

func TestNotifyStatusUpdate_UpdateErrorOtherThan404_SurfacesError(t *testing.T) {
	// 403 / 500 / other errors should propagate, not fall through
	// to create. The caller decides whether to retry.
	runID, gh, au, n := happyDeps(t)
	au.preSeed(runID, issuecomment.CategoryStatusCommentPosted, map[string]any{
		"github_comment_id": 4242,
	})
	gh.updateErr = githubclient.ErrForbidden

	err := n.NotifyStatusUpdate(context.Background(), runID, "x")
	if err == nil {
		t.Fatalf("expected error on non-404 update failure")
	}
	if len(gh.calls) != 0 {
		t.Errorf("non-404 update failure should not fall back to create; got %d creates", len(gh.calls))
	}
	if len(au.appended) != 0 {
		t.Errorf("non-404 update failure should not append audit row; got %d", len(au.appended))
	}
}

// happyDepsWithStages returns the happyDeps fixtures plus a stage
// list so NotifyStatusUpdateForRun has something to render against.
func happyDepsWithStages(t *testing.T) (uuid.UUID, *fakeGitHub, *fakeAudit, *fakeRuns, *issuecomment.Notifier) {
	t.Helper()
	runID := uuid.New()
	triggerRef := "issue:42"
	stages := []*run.Stage{
		{ID: uuid.New(), RunID: runID, Sequence: 1, Type: run.StageTypePlan, State: run.StageStateSucceeded},
		{ID: uuid.New(), RunID: runID, Sequence: 2, Type: run.StageTypeImplement, State: run.StageStateRunning},
	}
	repoRuns := &fakeRuns{
		runs: map[uuid.UUID]*run.Run{runID: {
			ID:             runID,
			Repo:           "x/y",
			WorkflowID:     "feature_change",
			TriggerSource:  run.TriggerGitHubIssue,
			TriggerRef:     &triggerRef,
			InstallationID: int64Ptr(99),
			State:          run.StateRunning,
		}},
		stages: map[uuid.UUID][]*run.Stage{runID: stages},
	}
	gh := &fakeGitHub{}
	au := &fakeAudit{}
	n := issuecomment.New(issuecomment.Deps{
		GitHub:      gh,
		Runs:        repoRuns,
		Audit:       au,
		ExternalURL: "https://app.fishhawk.example.com",
		Now:         func() time.Time { return time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC) },
	})
	if n == nil {
		t.Fatal("notifier nil")
	}
	return runID, gh, au, repoRuns, n
}

func TestNotifyStatusUpdateForRun_FirstCall_CreatesAndRenders(t *testing.T) {
	runID, gh, au, _, n := happyDepsWithStages(t)
	if err := n.NotifyStatusUpdateForRun(context.Background(), runID); err != nil {
		t.Fatalf("NotifyStatusUpdateForRun: %v", err)
	}
	if len(gh.calls) != 1 {
		t.Fatalf("expected 1 create call; got %d", len(gh.calls))
	}
	body := gh.calls[0].body
	// Should carry the rendered header + stage list.
	for _, want := range []string{"Fishhawk run", "feature_change", "plan", "implement"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q\n---\n%s", want, body)
		}
	}
	if len(au.appended) != 1 {
		t.Fatalf("expected 1 audit row; got %d", len(au.appended))
	}
	if au.appended[0].Category != issuecomment.CategoryStatusCommentPosted {
		t.Errorf("audit category = %q", au.appended[0].Category)
	}
}

func TestNotifyStatusUpdateForRun_SubsequentCall_EditsSameComment(t *testing.T) {
	// Pre-seed an existing status comment audit row; the convenience
	// method should resolve to UpdateIssueComment with that id.
	runID, gh, au, _, n := happyDepsWithStages(t)
	au.preSeed(runID, issuecomment.CategoryStatusCommentPosted, map[string]any{
		"kind":              string(issuecomment.KindStatusUpdate),
		"issue_number":      42,
		"repo":              "x/y",
		"github_comment_id": 4242,
	})
	if err := n.NotifyStatusUpdateForRun(context.Background(), runID); err != nil {
		t.Fatalf("NotifyStatusUpdateForRun: %v", err)
	}
	if len(gh.calls) != 0 {
		t.Errorf("expected 0 create calls on edit path; got %d", len(gh.calls))
	}
	if len(gh.updateCalls) != 1 {
		t.Fatalf("expected 1 update call; got %d", len(gh.updateCalls))
	}
	if gh.updateCalls[0].commentID != 4242 {
		t.Errorf("update.commentID = %d, want 4242", gh.updateCalls[0].commentID)
	}
}

func TestNotifyStatusUpdateForRun_NonIssueTrigger_SkipsSilently(t *testing.T) {
	runID, gh, au, runs, n := happyDepsWithStages(t)
	runs.runs[runID].TriggerSource = run.TriggerCLI
	if err := n.NotifyStatusUpdateForRun(context.Background(), runID); err != nil {
		t.Fatalf("NotifyStatusUpdateForRun: %v", err)
	}
	if len(gh.calls)+len(gh.updateCalls) != 0 {
		t.Errorf("non-issue trigger should skip; got %d creates + %d updates",
			len(gh.calls), len(gh.updateCalls))
	}
	if len(au.appended) != 0 {
		t.Errorf("non-issue trigger should not append audit rows; got %d", len(au.appended))
	}
}

func TestNotifyStatusUpdateForRun_NilReceiver_NoOp(t *testing.T) {
	var n *issuecomment.Notifier
	if err := n.NotifyStatusUpdateForRun(context.Background(), uuid.New()); err != nil {
		t.Errorf("nil receiver should be a no-op; got %v", err)
	}
}

// TestStatusComment_Lifecycle drives the sticky-status comment through
// the operator-visible transitions of a representative run lifecycle
// (E20.5 / #331): dispatch seed → plan-ready → plan-approved →
// implementing → PR-open → merged. Each transition mutates the
// underlying run/stage state and fires NotifyStatusUpdateForRun; the
// test verifies that the notifier (1) creates exactly one comment,
// (2) edits the same comment id on every subsequent transition, and
// (3) renders the right state content at each step.
//
// This is the cross-cutting integration test promised by #331's
// acceptance criteria. The wiring of each handler is unit-tested per
// transition (dispatcher_test, trace_plannotify_test, issue_approval_test,
// pullrequest_review_events_test); this test sits one level up and
// verifies the audit-log-based edit-in-place loop survives a real
// sequence of transitions.
func TestStatusComment_Lifecycle(t *testing.T) {
	runID := uuid.New()
	triggerRef := "issue:42"
	r := &run.Run{
		ID:             runID,
		Repo:           "x/y",
		WorkflowID:     "feature_change",
		TriggerSource:  run.TriggerGitHubIssue,
		TriggerRef:     &triggerRef,
		InstallationID: int64Ptr(99),
		State:          run.StatePending,
	}
	planStage := &run.Stage{ID: uuid.New(), RunID: runID, Sequence: 1, Type: run.StageTypePlan, State: run.StageStatePending}
	implementStage := &run.Stage{ID: uuid.New(), RunID: runID, Sequence: 2, Type: run.StageTypeImplement, State: run.StageStatePending}
	reviewStage := &run.Stage{ID: uuid.New(), RunID: runID, Sequence: 3, Type: run.StageTypeReview, State: run.StageStatePending}
	stages := []*run.Stage{planStage, implementStage, reviewStage}

	repoRuns := &fakeRuns{
		runs:   map[uuid.UUID]*run.Run{runID: r},
		stages: map[uuid.UUID][]*run.Stage{runID: stages},
	}
	gh := &fakeGitHub{}
	au := &fakeAudit{}
	n := issuecomment.New(issuecomment.Deps{
		GitHub:      gh,
		Runs:        repoRuns,
		Audit:       au,
		ExternalURL: "https://app.fishhawk.example.com",
		Now:         func() time.Time { return time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC) },
	})

	ctx := context.Background()

	// Step 1: dispatcher seed. Run is pending, plan stage is
	// pending — the first transition fires after CreateRun. Expect
	// a create call landing comment id=1. This is now the
	// "Fishhawk picked it up" beat (#376 retired the standalone
	// pickup-broadcast).
	r.State = run.StateRunning
	planStage.State = run.StageStateDispatched
	if err := n.NotifyStatusUpdateForRun(ctx, runID); err != nil {
		t.Fatalf("step 1 dispatcher seed: %v", err)
	}
	if len(gh.calls) != 1 || len(gh.updateCalls) != 0 {
		t.Fatalf("step 1: expected 1 create + 0 updates; got %d + %d", len(gh.calls), len(gh.updateCalls))
	}

	// Step 2: plan terminal (trace handler). Plan stage moves to
	// awaiting_approval (the workflow requires approval). Expect an
	// edit on comment id=1.
	planStage.State = run.StageStateAwaitingApproval
	planStage.RequiresApproval = true
	if err := n.NotifyStatusUpdateForRun(ctx, runID); err != nil {
		t.Fatalf("step 2 plan terminal: %v", err)
	}
	if len(gh.calls) != 1 || len(gh.updateCalls) != 1 {
		t.Fatalf("step 2: expected 1 create + 1 update; got %d + %d", len(gh.calls), len(gh.updateCalls))
	}
	if gh.updateCalls[0].commentID != 1 {
		t.Errorf("step 2: edit commentID = %d, want 1", gh.updateCalls[0].commentID)
	}

	// Step 3: plan approved (approval handler). Plan succeeds, implement
	// dispatches.
	planStage.State = run.StageStateSucceeded
	implementStage.State = run.StageStateDispatched
	if err := n.NotifyStatusUpdateForRun(ctx, runID); err != nil {
		t.Fatalf("step 3 plan approved: %v", err)
	}
	if len(gh.calls) != 1 || len(gh.updateCalls) != 2 {
		t.Fatalf("step 3: expected 1 create + 2 updates; got %d + %d", len(gh.calls), len(gh.updateCalls))
	}

	// Step 4: implement terminal (trace handler). Implement succeeds.
	implementStage.State = run.StageStateSucceeded
	if err := n.NotifyStatusUpdateForRun(ctx, runID); err != nil {
		t.Fatalf("step 4 implement terminal: %v", err)
	}
	if len(gh.calls) != 1 || len(gh.updateCalls) != 3 {
		t.Fatalf("step 4: expected 1 create + 3 updates; got %d + %d", len(gh.calls), len(gh.updateCalls))
	}

	// Step 5: PR opened (pullrequest handler). PR URL is stamped on
	// the run; review stage moves to awaiting_approval. Now that the run
	// OWNS a PR, the sticky PR status comment (#1784) fires too: the anchor
	// edit lands on id=1 (update #4) and a NEW PR comment is CREATED (id=2)
	// on the PR number (77, distinct from the issue number 42).
	prURL := "https://github.com/x/y/pull/77"
	r.PullRequestURL = &prURL
	reviewStage.State = run.StageStateAwaitingApproval
	if err := n.NotifyStatusUpdateForRun(ctx, runID); err != nil {
		t.Fatalf("step 5 PR opened: %v", err)
	}
	if len(gh.calls) != 2 || len(gh.updateCalls) != 4 {
		t.Fatalf("step 5: expected 2 creates (anchor + PR) + 4 updates; got %d + %d", len(gh.calls), len(gh.updateCalls))
	}
	// The step-5 anchor edit (update #4, index 3) carries the PR link.
	if !strings.Contains(gh.updateCalls[3].body, prURL) {
		t.Errorf("step 5: anchor comment body should contain PR URL: %q", gh.updateCalls[3].body)
	}

	// Step 6: PR merged (PR-events handler). Review stage succeeds,
	// run state moves to succeeded. Anchor edits id=1 (update #5) and the PR
	// comment (body now differs) edits id=2 (update #6).
	reviewStage.State = run.StageStateSucceeded
	r.State = run.StateSucceeded
	if err := n.NotifyStatusUpdateForRun(ctx, runID); err != nil {
		t.Fatalf("step 6 PR merged: %v", err)
	}
	if len(gh.calls) != 2 || len(gh.updateCalls) != 6 {
		t.Fatalf("step 6: expected 2 creates + 6 updates; got %d + %d", len(gh.calls), len(gh.updateCalls))
	}

	// Every update targets either the anchor (id=1) or the PR comment (id=2) —
	// the test fails loudly if the dedup ever races and stacks a third comment.
	for i, upd := range gh.updateCalls {
		if upd.commentID != 1 && upd.commentID != 2 {
			t.Errorf("update %d targeted comment id %d; expected the anchor (1) or PR comment (2)",
				i, upd.commentID)
		}
	}
	// No stacking: exactly one anchor create (issue 42) and one PR create (77).
	if got := prCreateCount(gh, 42); got != 1 {
		t.Errorf("anchor create count = %d, want 1 (no stacking)", got)
	}
	if got := prCreateCount(gh, 77); got != 1 {
		t.Errorf("PR-comment create count = %d, want 1 (no stacking)", got)
	}

	// The final ANCHOR body should reflect the succeeded state and carry both
	// the run link and the PR link. Select the last update targeting id=1.
	var finalBody string
	for _, upd := range gh.updateCalls {
		if upd.commentID == 1 {
			finalBody = upd.body
		}
	}
	for _, want := range []string{"succeeded", "Pull request", prURL, "View run"} {
		if !strings.Contains(finalBody, want) {
			t.Errorf("final anchor body missing %q\n---\n%s", want, finalBody)
		}
	}

	// Audit-log accounting: one status_comment_posted row per
	// transition (6 total). The chain is what production reads from
	// to find the comment id on subsequent calls, so the count is a
	// load-bearing invariant.
	statusRows := 0
	for _, p := range au.appended {
		if p.Category == issuecomment.CategoryStatusCommentPosted {
			statusRows++
		}
	}
	if statusRows != 6 {
		t.Errorf("expected 6 status_comment_posted audit rows (one per transition); got %d", statusRows)
	}
}

// TestStatusComment_ConcurrentUpdates_TargetSameComment locks in the
// dedup property under concurrent fire (E20.5 / #331 case 3): once a
// status comment exists, concurrent NotifyStatusUpdateForRun calls
// all resolve to UpdateIssueComment with the same id rather than
// racing into multiple CreateIssueComment calls. The production
// path's `ListForRunByCategory` query returns the existing row to
// every concurrent reader, so all callers find the seeded comment id
// and take the edit path.
//
// First-call concurrency (no prior comment) is NOT exercised by this
// test — that's a TOCTOU race the audit-log dedup cannot prevent on
// its own, and the dispatcher's seed step happens before any other
// transition so in practice the race window is bounded.
func TestStatusComment_ConcurrentUpdates_TargetSameComment(t *testing.T) {
	runID, gh, au, _, n := happyDepsWithStages(t)

	// Pre-seed an existing status comment so every concurrent caller
	// reads the same id from the dedup query.
	au.preSeed(runID, issuecomment.CategoryStatusCommentPosted, map[string]any{
		"kind":              string(issuecomment.KindStatusUpdate),
		"issue_number":      42,
		"repo":              "x/y",
		"github_comment_id": 4242,
	})

	const N = 16
	var wg sync.WaitGroup
	errs := make(chan error, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- n.NotifyStatusUpdateForRun(context.Background(), runID)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent NotifyStatusUpdateForRun: %v", err)
		}
	}

	if len(gh.calls) != 0 {
		t.Errorf("no concurrent caller should have taken the create path; got %d", len(gh.calls))
	}
	if len(gh.updateCalls) != N {
		t.Fatalf("expected %d update calls (one per caller); got %d", N, len(gh.updateCalls))
	}
	for i, upd := range gh.updateCalls {
		if upd.commentID != 4242 {
			t.Errorf("update %d targeted comment id %d; want 4242 (seeded)", i, upd.commentID)
		}
	}
}

func TestNew_NilDepsReturnsNilNotifier(t *testing.T) {
	cases := []struct {
		name string
		d    issuecomment.Deps
	}{
		{"no github", issuecomment.Deps{Runs: &fakeRuns{}, Audit: &fakeAudit{}, ExternalURL: "x"}},
		{"no runs", issuecomment.Deps{GitHub: &fakeGitHub{}, Audit: &fakeAudit{}, ExternalURL: "x"}},
		{"no audit", issuecomment.Deps{GitHub: &fakeGitHub{}, Runs: &fakeRuns{}, ExternalURL: "x"}},
		{"no external url", issuecomment.Deps{GitHub: &fakeGitHub{}, Runs: &fakeRuns{}, Audit: &fakeAudit{}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if n := issuecomment.New(tc.d); n != nil {
				t.Errorf("expected nil; got %+v", n)
			}
		})
	}
}

// TestArtifactListerWired pins the accessor the server-package wiring
// test depends on (#1069): nil receiver and an unset Artifacts dep both
// report false; supplying a lister reports true. This is the contract
// that lets TestNew_WiresAnchorPlanArtifactLister assert the production
// constructor carries cfg.ArtifactRepo through to the Notifier.
func TestArtifactListerWired(t *testing.T) {
	var nilN *issuecomment.Notifier
	if nilN.ArtifactListerWired() {
		t.Error("nil receiver should report unwired")
	}

	unset := issuecomment.New(issuecomment.Deps{
		GitHub:      &fakeGitHub{},
		Runs:        &fakeRuns{},
		Audit:       &fakeAudit{},
		ExternalURL: "https://app.fishhawk.example.com",
	})
	if unset == nil {
		t.Fatal("notifier should construct without Artifacts")
	}
	if unset.ArtifactListerWired() {
		t.Error("notifier built without Artifacts should report unwired")
	}

	wired := issuecomment.New(issuecomment.Deps{
		GitHub:      &fakeGitHub{},
		Runs:        &fakeRuns{},
		Audit:       &fakeAudit{},
		ExternalURL: "https://app.fishhawk.example.com",
		Artifacts:   &fakeArtifacts{},
	})
	if wired == nil {
		t.Fatal("notifier should construct with Artifacts")
	}
	if !wired.ArtifactListerWired() {
		t.Error("notifier built with Artifacts should report wired")
	}
}

func TestNotifySlashApprovalReply_PostsAndDoesNotDedup(t *testing.T) {
	_, _, _, n := happyDeps(t)
	gh := &fakeGitHub{}
	// Replace the inner GitHub fake by reflecting through New
	// again with a fresh notifier sharing the audit log so dedup
	// state would survive if we erroneously persisted it.
	n2 := issuecomment.New(issuecomment.Deps{
		GitHub:      gh,
		Runs:        &fakeRuns{},
		Audit:       &fakeAudit{},
		ExternalURL: "https://app.fishhawk.example.com",
	})
	_ = n // unused; happyDeps constructs one we don't need here

	if err := n2.NotifySlashApprovalReply(context.Background(), issuecomment.SlashApprovalReply{
		Repo: "x/y", InstallationID: 99, IssueNumber: 42, Body: "Approved by @alice.",
	}); err != nil {
		t.Fatal(err)
	}
	if len(gh.calls) != 1 {
		t.Fatalf("expected 1 call; got %d", len(gh.calls))
	}
	if gh.calls[0].body != "Approved by @alice." {
		t.Errorf("body = %q", gh.calls[0].body)
	}

	// Second call — replies are not deduped.
	if err := n2.NotifySlashApprovalReply(context.Background(), issuecomment.SlashApprovalReply{
		Repo: "x/y", InstallationID: 99, IssueNumber: 42, Body: "Cannot approve: ci_pass failing.",
	}); err != nil {
		t.Fatal(err)
	}
	if len(gh.calls) != 2 {
		t.Errorf("replies should not be deduped; got %d calls", len(gh.calls))
	}
}

func TestNotifySlashApprovalReply_SkipsBadParams(t *testing.T) {
	gh := &fakeGitHub{}
	n := issuecomment.New(issuecomment.Deps{
		GitHub:      gh,
		Runs:        &fakeRuns{},
		Audit:       &fakeAudit{},
		ExternalURL: "https://app.fishhawk.example.com",
	})
	cases := []struct {
		name string
		p    issuecomment.SlashApprovalReply
	}{
		{"zero issue", issuecomment.SlashApprovalReply{Repo: "x/y", InstallationID: 99, Body: "x"}},
		{"zero installation", issuecomment.SlashApprovalReply{Repo: "x/y", IssueNumber: 1, Body: "x"}},
		{"empty body", issuecomment.SlashApprovalReply{Repo: "x/y", InstallationID: 99, IssueNumber: 1}},
		{"malformed repo", issuecomment.SlashApprovalReply{Repo: "no-slash", InstallationID: 99, IssueNumber: 1, Body: "x"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := n.NotifySlashApprovalReply(context.Background(), tc.p); err != nil {
				t.Errorf("expected nil; got %v", err)
			}
		})
	}
	if len(gh.calls) != 0 {
		t.Errorf("expected 0 calls; got %d", len(gh.calls))
	}
}

func TestNotify_NilReceiver_NoOp(t *testing.T) {
	var n *issuecomment.Notifier
	if err := n.NotifyPlanReady(context.Background(), uuid.New(),
		&run.Stage{ID: uuid.New(), Type: run.StageTypePlan}, &plan.Plan{}); err != nil {
		t.Errorf("nil plan should be a no-op; got %v", err)
	}
	if err := n.NotifySlashApprovalReply(context.Background(), issuecomment.SlashApprovalReply{
		Repo: "x/y", InstallationID: 1, IssueNumber: 1, Body: "x",
	}); err != nil {
		t.Errorf("nil reply should be a no-op; got %v", err)
	}
	if err := n.NotifyRunRejected(context.Background(), "x/y", 1, 1, "feature_change", "plan"); err != nil {
		t.Errorf("nil run-rejected should be a no-op; got %v", err)
	}
}

// TestNotifyRunRejected_PostsExplanation covers the #599 surface: the
// run-rejected comment names the offending workflow_id + stage and
// both fixes, posts no audit row (runless; canonical record is the
// dispatcher's global-chain entry), and is not deduped.
func TestNotifyRunRejected_PostsExplanation(t *testing.T) {
	gh := &fakeGitHub{}
	au := &fakeAudit{}
	n := issuecomment.New(issuecomment.Deps{
		GitHub:      gh,
		Runs:        &fakeRuns{},
		Audit:       au,
		ExternalURL: "https://app.fishhawk.example.com",
	})

	if err := n.NotifyRunRejected(context.Background(), "kuhlman-labs/fishhawk", 42, 1247,
		"feature_change", "plan"); err != nil {
		t.Fatal(err)
	}
	if len(gh.calls) != 1 {
		t.Fatalf("expected 1 comment; got %d", len(gh.calls))
	}
	call := gh.calls[0]
	if call.installationID != 42 {
		t.Errorf("installationID = %d, want 42", call.installationID)
	}
	if call.issueNumber != 1247 {
		t.Errorf("issueNumber = %d, want 1247", call.issueNumber)
	}
	for _, want := range []string{"feature_change", "plan", "FISHHAWKD_ANTHROPIC_API_KEY", "reviewers"} {
		if !strings.Contains(call.body, want) {
			t.Errorf("body missing %q:\n%s", want, call.body)
		}
	}
	// Runless surface: no notifier-level audit row (mirrors
	// NotifySlashApprovalReply).
	if len(au.appended) != 0 {
		t.Errorf("expected no audit rows; got %d", len(au.appended))
	}

	// Not deduped: a second refusal posts again.
	if err := n.NotifyRunRejected(context.Background(), "kuhlman-labs/fishhawk", 42, 1247,
		"feature_change", "plan"); err != nil {
		t.Fatal(err)
	}
	if len(gh.calls) != 2 {
		t.Errorf("run-rejected comments should not be deduped; got %d calls", len(gh.calls))
	}
}

// TestNotifyRunRejected_SkipsBadParams exercises the defensive skips:
// zero issue, zero installation, malformed repo all no-op without
// touching GitHub.
func TestNotifyRunRejected_SkipsBadParams(t *testing.T) {
	gh := &fakeGitHub{}
	n := issuecomment.New(issuecomment.Deps{
		GitHub:      gh,
		Runs:        &fakeRuns{},
		Audit:       &fakeAudit{},
		ExternalURL: "https://app.fishhawk.example.com",
	})
	cases := []struct {
		name           string
		repo           string
		installationID int64
		issueNumber    int
	}{
		{"zero issue", "x/y", 99, 0},
		{"zero installation", "x/y", 0, 1},
		{"malformed repo", "no-slash", 99, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := n.NotifyRunRejected(context.Background(), tc.repo, tc.installationID,
				tc.issueNumber, "feature_change", "plan"); err != nil {
				t.Errorf("expected nil; got %v", err)
			}
		})
	}
	if len(gh.calls) != 0 {
		t.Errorf("expected 0 calls; got %d", len(gh.calls))
	}
}

// --- helpers ---

func int64Ptr(v int64) *int64 { return &v }
func ptrStr(s string) *string { return &s }

// --- fakes ---

type ghCommentCall struct {
	installationID int64
	repo           githubclient.RepoRef
	issueNumber    int
	body           string
}

type ghUpdateCommentCall struct {
	installationID int64
	repo           githubclient.RepoRef
	commentID      int64
	body           string
}

type fakeGitHub struct {
	mu          sync.Mutex
	calls       []ghCommentCall
	updateCalls []ghUpdateCommentCall
	err         error
	updateErr   error
	// failOnIssueNumber, when non-zero, fails CreateIssueComment ONLY for that
	// issue/PR number — so a PR-status-comment test can fail the PR-locus post
	// (posted to the PR number) while leaving the anchor post (posted to the
	// triggering issue number) succeeding. Used to prove PR-comment fail-open
	// independently of the anchor.
	failOnIssueNumber int
}

func (f *fakeGitHub) CreateIssueComment(_ context.Context, installationID int64, repo githubclient.RepoRef, issueNumber int, body string) (*githubclient.IssueComment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, ghCommentCall{installationID: installationID, repo: repo, issueNumber: issueNumber, body: body})
	if f.err != nil {
		return nil, f.err
	}
	if f.failOnIssueNumber != 0 && issueNumber == f.failOnIssueNumber {
		return nil, errors.New("fake github: create failed for issue/PR number")
	}
	// Synthesize an id deterministically from the call index so
	// status-comment tests can predict the comment id without
	// extra plumbing. The +1 keeps ids positive (1, 2, 3, …).
	id := int64(len(f.calls))
	return &githubclient.IssueComment{
		ID:      id,
		Body:    body,
		HTMLURL: fmt.Sprintf("https://github.com/%s/issues/%d#issuecomment-%d", repo.String(), issueNumber, id),
	}, nil
}

// UpdateIssueComment records the edit call alongside the existing
// create-call log. Status-comment tests assert on both surfaces.
func (f *fakeGitHub) UpdateIssueComment(_ context.Context, installationID int64, repo githubclient.RepoRef, commentID int64, body string) (*githubclient.IssueComment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updateCalls = append(f.updateCalls, ghUpdateCommentCall{installationID: installationID, repo: repo, commentID: commentID, body: body})
	if f.updateErr != nil {
		return nil, f.updateErr
	}
	return &githubclient.IssueComment{
		ID:      commentID,
		Body:    body,
		HTMLURL: fmt.Sprintf("https://github.com/%s#issuecomment-%d", repo.String(), commentID),
	}, nil
}

type fakeRuns struct {
	run.Repository
	runs   map[uuid.UUID]*run.Run
	stages map[uuid.UUID][]*run.Stage
}

func (f *fakeRuns) GetRun(_ context.Context, id uuid.UUID) (*run.Run, error) {
	r, ok := f.runs[id]
	if !ok {
		return nil, run.ErrNotFound
	}
	return r, nil
}

func (f *fakeRuns) ListStagesForRun(_ context.Context, id uuid.UUID) ([]*run.Stage, error) {
	if f.stages == nil {
		return nil, nil
	}
	return f.stages[id], nil
}

type fakeAudit struct {
	audit.Repository
	mu       sync.Mutex
	appended []audit.ChainAppendParams
	preSeeds []*audit.Entry
}

func (f *fakeAudit) preSeed(runID uuid.UUID, category string, payload map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	body, _ := json.Marshal(payload)
	r := runID
	f.preSeeds = append(f.preSeeds, &audit.Entry{
		ID: uuid.New(), Sequence: int64(len(f.preSeeds) + 1), RunID: &r, Category: category, Payload: body,
	})
}

// preSeedWithStage is the stage-scoped variant — needed for
// approval rows since the plan-status renderer reads stage_id.
func (f *fakeAudit) preSeedWithStage(runID, stageID uuid.UUID, category string, payload map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	body, _ := json.Marshal(payload)
	r := runID
	s := stageID
	f.preSeeds = append(f.preSeeds, &audit.Entry{
		ID: uuid.New(), Sequence: int64(len(f.preSeeds) + 1), RunID: &r, StageID: &s, Category: category, Payload: body,
	})
}

func (f *fakeAudit) AppendChained(_ context.Context, p audit.ChainAppendParams) (*audit.Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.appended = append(f.appended, p)
	r := p.RunID
	return &audit.Entry{ID: uuid.New(), RunID: &r}, nil
}

// appendedToEntries projects the recorded ChainAppendParams back to
// *audit.Entry rows so the lifecycle / lookup queries can see what the
// production code wrote. Sequence is the 1-indexed position; the
// caller's slice order is the canonical chronological order.
func (f *fakeAudit) appendedToEntries() []*audit.Entry {
	out := make([]*audit.Entry, 0, len(f.appended))
	for i, p := range f.appended {
		r := p.RunID
		out = append(out, &audit.Entry{
			ID:        uuid.New(),
			Sequence:  int64(len(f.preSeeds) + i + 1),
			RunID:     &r,
			StageID:   p.StageID,
			Timestamp: p.Timestamp,
			Category:  p.Category,
			Payload:   p.Payload,
		})
	}
	return out
}

func (f *fakeAudit) ListForRunByCategory(_ context.Context, runID uuid.UUID, category string) ([]*audit.Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []*audit.Entry{}
	for _, e := range f.preSeeds {
		if e.RunID != nil && *e.RunID == runID && e.Category == category {
			out = append(out, e)
		}
	}
	for _, e := range f.appendedToEntries() {
		if e.RunID != nil && *e.RunID == runID && e.Category == category {
			out = append(out, e)
		}
	}
	return out, nil
}

func (f *fakeAudit) ListForRun(_ context.Context, runID uuid.UUID) ([]*audit.Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []*audit.Entry{}
	for _, e := range f.preSeeds {
		if e.RunID != nil && *e.RunID == runID {
			out = append(out, e)
		}
	}
	for _, e := range f.appendedToEntries() {
		if e.RunID != nil && *e.RunID == runID {
			out = append(out, e)
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------
// Living-anchor cross-boundary integration (#1054).
// ---------------------------------------------------------------------

type fakeArtifacts struct {
	byStage map[uuid.UUID][]*artifact.Artifact
}

func (f *fakeArtifacts) ListForStage(_ context.Context, stageID uuid.UUID) ([]*artifact.Artifact, error) {
	return f.byStage[stageID], nil
}

func planArtifactJSON(t *testing.T, summary string, files ...string) json.RawMessage {
	t.Helper()
	p := plan.Plan{Summary: summary}
	for _, f := range files {
		p.Scope.Files = append(p.Scope.Files, plan.ScopeFile{Path: f, Operation: plan.FileOpModify})
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestNotifyStatusUpdateForRun_ModelRecommendationAndResolved drives the
// #1013 render seam end to end: a plan artifact carrying a model_recommendation
// plus a gate model_resolved audit entry → the anchor comment shows both the
// recommendation (implement_model + rationale) and the resolved {model,
// source} block.
func TestNotifyStatusUpdateForRun_ModelRecommendationAndResolved(t *testing.T) {
	runID := uuid.New()
	planStageID := uuid.New()
	triggerRef := "issue:42"
	repoRuns := &fakeRuns{
		runs: map[uuid.UUID]*run.Run{runID: {
			ID: runID, Repo: "x/y", WorkflowID: "feature_change", State: run.StateRunning,
			TriggerSource: run.TriggerGitHubIssue, TriggerRef: &triggerRef, InstallationID: int64Ptr(99),
		}},
		stages: map[uuid.UUID][]*run.Stage{runID: {
			{ID: planStageID, RunID: runID, Type: run.StageTypePlan, State: run.StageStateSucceeded},
		}},
	}
	gh := &fakeGitHub{}
	au := &fakeAudit{}
	au.preSeedWithStage(runID, planStageID, "approval_submitted", map[string]any{"decision": "approve"})
	au.preSeedWithStage(runID, planStageID, "model_resolved", map[string]any{
		"model": "claude-opus-4-8", "model_source": "operator",
	})

	p := plan.Plan{
		Summary: "Resolve the model at the gate.",
		ModelRecommendation: &plan.ModelRecommendation{
			ImplementModel: "claude-sonnet-4-6", Rationale: "medium complexity", ComplexityAssessed: plan.ComplexityMedium,
		},
	}
	content, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	arts := &fakeArtifacts{byStage: map[uuid.UUID][]*artifact.Artifact{
		planStageID: {{ID: uuid.New(), StageID: planStageID, Kind: artifact.KindPlan, Content: content, CreatedAt: time.Unix(100, 0)}},
	}}
	n := issuecomment.New(issuecomment.Deps{
		GitHub: gh, Runs: repoRuns, Audit: au, Artifacts: arts,
		ExternalURL: "https://app.example",
		Now:         func() time.Time { return time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC) },
	})
	if err := n.NotifyStatusUpdateForRun(context.Background(), runID); err != nil {
		t.Fatalf("NotifyStatusUpdateForRun: %v", err)
	}
	if len(gh.calls) != 1 {
		t.Fatalf("expected 1 create call; got %d", len(gh.calls))
	}
	body := gh.calls[0].body
	if !strings.Contains(body, "Model recommendation: `claude-sonnet-4-6` — medium complexity") {
		t.Errorf("anchor missing model recommendation:\n%s", body)
	}
	if !strings.Contains(body, "**Implement model** — `claude-opus-4-8` (source: operator)") {
		t.Errorf("anchor missing resolved model block:\n%s", body)
	}
}

// TestNotifyStatusUpdateForRun_AnchorEndToEnd drives a feature_change
// shape across the full audit-chain → anchor-projection → GitHub-I/O
// seam (#1054): a first plan version rejected (with a reason) and
// replanned, two reviewer verdicts (approve + reject), and the replanned
// plan parked at the approval gate. Through repeated
// NotifyStatusUpdateForRun calls it asserts: exactly ONE anchor comment
// (single status_comment_posted id reused + edited in place), page-class
// pings posted once each as NEW comments, reviewer verdicts visible
// inline, the CURRENT plan summary projected from the artifact store, and
// — the supersede/replan path (concern #2) — the SUPERSEDED plan
// preserved collapsed with its rejection reason.
func TestNotifyStatusUpdateForRun_AnchorEndToEnd(t *testing.T) {
	runID := uuid.New()
	planStageID := uuid.New()
	triggerRef := "issue:42"
	repoRuns := &fakeRuns{
		runs: map[uuid.UUID]*run.Run{runID: {
			ID: runID, Repo: "x/y", WorkflowID: "feature_change", State: run.StateRunning,
			TriggerSource: run.TriggerGitHubIssue, TriggerRef: &triggerRef, InstallationID: int64Ptr(99),
		}},
		// The plan stage is parked at its approval gate (the replanned v2
		// awaiting a human) — the precondition for the plan-awaiting ping.
		stages: map[uuid.UUID][]*run.Stage{runID: {
			{ID: planStageID, RunID: runID, Type: run.StageTypePlan, State: run.StageStateAwaitingApproval},
		}},
	}
	gh := &fakeGitHub{}
	au := &fakeAudit{}
	// Round 1: plan v1 generated, reviewed (approve + reject), then the
	// plan gate REJECTED with an operator reason — this is what retires v1.
	au.preSeedWithStage(runID, planStageID, "plan_generated", map[string]any{"schema_version": "standard_v1"})
	au.preSeedWithStage(runID, planStageID, "plan_review_started", map[string]any{})
	au.preSeedWithStage(runID, planStageID, "plan_reviewed", map[string]any{"reviewer_model": "claude-opus-4-8", "verdict": "approve"})
	au.preSeedWithStage(runID, planStageID, "plan_reviewed", map[string]any{
		"reviewer_model": "gpt-5.5", "verdict": "reject",
		"concerns":  []map[string]any{{"severity": "high", "category": "correctness", "note": "boom"}},
		"free_form": "the codex note",
	})
	au.preSeedWithStage(runID, planStageID, "approval_submitted", map[string]any{
		"decision": "reject", "approver_github_login": "alice",
		"rejection_comment": "scoped the wrong fork",
	})
	// Round 2: replanned plan v2 generated (now awaiting approval).
	au.preSeedWithStage(runID, planStageID, "plan_generated", map[string]any{"schema_version": "standard_v1"})

	// Two plan artifacts on the stage: v1 (older, superseded) and v2
	// (newer, current). loadAnchorPlans orders by CreatedAt.
	arts := &fakeArtifacts{byStage: map[uuid.UUID][]*artifact.Artifact{
		planStageID: {
			{ID: uuid.New(), StageID: planStageID, Kind: artifact.KindPlan,
				Content: planArtifactJSON(t, "First attempt on the wrong fork", "a.go"), CreatedAt: time.Unix(100, 0)},
			{ID: uuid.New(), StageID: planStageID, Kind: artifact.KindPlan,
				Content: planArtifactJSON(t, "Add the living anchor", "b.go"), CreatedAt: time.Unix(200, 0)},
		},
	}}
	n := issuecomment.New(issuecomment.Deps{
		GitHub: gh, Runs: repoRuns, Audit: au, Artifacts: arts,
		ExternalURL: "https://app.example",
		Now:         func() time.Time { return time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC) },
	})

	for i := 0; i < 3; i++ {
		if err := n.NotifyStatusUpdateForRun(context.Background(), runID); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}

	anchorCreates, pingCreates := 0, 0
	for _, c := range gh.calls {
		if strings.Contains(c.body, "Fishhawk run") {
			anchorCreates++
		} else {
			pingCreates++
		}
	}
	if anchorCreates != 1 {
		t.Errorf("expected exactly 1 anchor comment create; got %d", anchorCreates)
	}
	// Page-class events in the chain: the plan gate awaiting approval +
	// a reviewer reject = 2 pings, each fired once across the 3
	// transitions (the gate-decision reject is not itself a ping class).
	if pingCreates != 2 {
		t.Errorf("expected 2 page-class pings (plan awaiting + reviewer reject); got %d", pingCreates)
	}
	if len(gh.updateCalls) < 2 {
		t.Errorf("expected the anchor to edit in place on later transitions; got %d edits", len(gh.updateCalls))
	}

	body := gh.updateCalls[len(gh.updateCalls)-1].body
	if !strings.Contains(body, "claude-opus-4-8: approve") || !strings.Contains(body, "gpt-5.5: reject (1 high)") {
		t.Errorf("anchor missing inline reviewer verdicts:\n%s", body)
	}
	if !strings.Contains(body, "Add the living anchor") {
		t.Errorf("anchor missing CURRENT plan summary projected from artifact store:\n%s", body)
	}
	// Supersede/replan seam (concern #2): the older plan is preserved
	// collapsed, labeled superseded, WITH its rejection reason.
	if !strings.Contains(body, "Superseded plan") || !strings.Contains(body, "First attempt on the wrong fork") {
		t.Errorf("anchor missing superseded plan section:\n%s", body)
	}
	if !strings.Contains(body, "scoped the wrong fork") {
		t.Errorf("superseded plan missing its rejection reason:\n%s", body)
	}
}

// econEntry is a small audit-chain entry builder for the BuildRunEconomics
// fold test: category + ascending sequence + timestamp + optional payload.
func econEntry(seq int64, category string, ts int64, payload map[string]any) *audit.Entry {
	var raw json.RawMessage
	if payload != nil {
		raw, _ = json.Marshal(payload)
	}
	return &audit.Entry{Sequence: seq, Category: category, Timestamp: time.Unix(ts, 0).UTC(), Payload: raw}
}

// TestBuildRunEconomics_FoldsChain exercises the notifier's fold (#1702): a
// seeded chain with cost_recorded ledger rows and all three gate boundaries
// produces the cost / cache / latency rollups, with the authoritative run
// total (CostUSDTotal) as the cost total and the audit-timestamp deltas as the
// gate waits. Also asserts the rendered block reconciles.
func TestBuildRunEconomics_FoldsChain(t *testing.T) {
	runRow := &run.Run{
		ID:           uuid.New(),
		CreatedAt:    time.Unix(100, 0).UTC(),
		CostUSDTotal: 0.40,
	}
	entries := []*audit.Entry{
		econEntry(1, "plan_generated", 100, nil),
		econEntry(2, "approval_submitted", 2800, map[string]any{"decision": "approve"}), // plan approval = 2700
		econEntry(3, "cost_recorded", 2900, map[string]any{
			"usd": 0.30, "source": "", "model": "claude-opus-4-8",
			"input_tokens": 500, "output_tokens": 100,
			"cache_read_input_tokens": 1000, "cache_write_input_tokens": 200,
		}),
		econEntry(4, "implement_reviewed", 5000, nil),
		econEntry(5, "acceptance_dispatched", 6800, nil), // implement_review_to_dispatch = 1800
		econEntry(6, drive.Category, 7000, map[string]any{"rule": string(drive.RuleChecksGreenAwaitingMerge)}),
		econEntry(7, "cost_recorded", 7100, map[string]any{"usd": 0.10, "source": "plan_review", "model": "claude-opus-4-8"}),
		econEntry(8, "pr_merged", 7900, nil), // checks_green_to_merge = 900; wall clock = 7800
	}

	econ := issuecomment.BuildRunEconomics(runRow, entries)
	if econ == nil {
		t.Fatal("BuildRunEconomics returned nil")
	}

	// Cost: authoritative total + per-stage breakdown.
	if econ.Cost.TotalUSD != 0.40 {
		t.Errorf("cost total = %v, want the authoritative 0.40", econ.Cost.TotalUSD)
	}
	stageCost := map[string]float64{}
	for _, st := range econ.Cost.Stages {
		stageCost[st.Source] = st.CostUSD
	}
	if stageCost["agent"] != 0.30 || stageCost["plan_review"] != 0.10 {
		t.Errorf("per-stage cost = %+v, want agent 0.30 + plan_review 0.10", stageCost)
	}

	// Cache: tokens folded, net savings surfaced.
	if econ.Cache.CacheReadTokens != 1000 || econ.Cache.CacheWriteTokens != 200 {
		t.Errorf("cache tokens = read %d / write %d, want 1000 / 200", econ.Cache.CacheReadTokens, econ.Cache.CacheWriteTokens)
	}

	// Latency: three gate waits equal to the audit-timestamp deltas.
	waits := map[string]float64{}
	for _, g := range econ.Latency.Gates {
		waits[g.Gate] = g.WaitSeconds
	}
	if waits[latency.GatePlanApproval] != 2700 {
		t.Errorf("plan approval wait = %v, want 2700", waits[latency.GatePlanApproval])
	}
	if waits[latency.GateImplementReviewToDispatch] != 1800 {
		t.Errorf("implement→dispatch wait = %v, want 1800", waits[latency.GateImplementReviewToDispatch])
	}
	if waits[latency.GateChecksGreenToMerge] != 900 {
		t.Errorf("checks-green→merge wait = %v, want 900", waits[latency.GateChecksGreenToMerge])
	}
	if econ.Latency.WallClockSeconds != 7800 {
		t.Errorf("wall clock = %v, want 7800 (pr_merged - created)", econ.Latency.WallClockSeconds)
	}

	block := issuecomment.RenderEconomicsBlock(*econ)
	for _, want := range []string{"**Total cost**: $0.40", "plan approval: 45m", "**Wall clock**: 2h 10m"} {
		if !strings.Contains(block, want) {
			t.Errorf("rendered block missing %q:\n%s", want, block)
		}
	}
}

// TestBuildRunEconomics_EmptyChainRendersNothing is the defensive branch: a
// run with no cost and no gate boundaries yields an all-zero rollup whose
// rendered block is empty (dropped from the anchor).
func TestBuildRunEconomics_EmptyChainRendersNothing(t *testing.T) {
	runRow := &run.Run{ID: uuid.New(), CreatedAt: time.Unix(100, 0).UTC()}
	econ := issuecomment.BuildRunEconomics(runRow, []*audit.Entry{
		econEntry(1, "stage_dispatched", 200, nil),
	})
	if got := issuecomment.RenderEconomicsBlock(*econ); got != "" {
		t.Errorf("empty-signal chain should render an empty block; got %q", got)
	}
}

// TestBuildRunEconomics_MalformedCostSkipped is the unparsable-payload branch:
// a cost_recorded entry with an invalid payload is skipped, not fatal — the
// valid entries still fold and the rollup is unaffected.
func TestBuildRunEconomics_MalformedCostSkipped(t *testing.T) {
	runRow := &run.Run{ID: uuid.New(), CreatedAt: time.Unix(100, 0).UTC(), CostUSDTotal: 0.25}
	entries := []*audit.Entry{
		{Sequence: 1, Category: "cost_recorded", Timestamp: time.Unix(150, 0).UTC(), Payload: json.RawMessage(`{not valid json`)},
		econEntry(2, "cost_recorded", 200, map[string]any{"usd": 0.25, "source": "agent", "model": "claude-opus-4-8"}),
	}
	econ := issuecomment.BuildRunEconomics(runRow, entries)
	// The single valid entry is the only stage; the malformed one contributed nothing.
	if len(econ.Cost.Stages) != 1 || econ.Cost.Stages[0].Source != "agent" {
		t.Fatalf("malformed cost entry must be skipped, valid one kept: %+v", econ.Cost.Stages)
	}
	if econ.Cost.Stages[0].CostUSD != 0.25 {
		t.Errorf("agent stage cost = %v, want 0.25", econ.Cost.Stages[0].CostUSD)
	}
}

// TestBuildRunEconomics_SynthesizesCIGreenGate exercises the run_auto_advanced
// → ci_green boundary synthesis: a checks-green auto-advance paired with a
// following pr_merged yields the checks_green_to_merge gate, while a
// non-matching run_auto_advanced rule synthesizes no boundary.
func TestBuildRunEconomics_SynthesizesCIGreenGate(t *testing.T) {
	runRow := &run.Run{ID: uuid.New(), CreatedAt: time.Unix(100, 0).UTC(), CostUSDTotal: 0.10}
	entries := []*audit.Entry{
		econEntry(1, "cost_recorded", 150, map[string]any{"usd": 0.10, "source": "agent"}),
		econEntry(2, drive.Category, 7000, map[string]any{"rule": string(drive.RuleChecksGreenAwaitingMerge)}),
		econEntry(3, "pr_merged", 7900, nil), // checks_green_to_merge = 900
		econEntry(4, drive.Category, 7950, map[string]any{"rule": "some_other_rule"}),
	}
	econ := issuecomment.BuildRunEconomics(runRow, entries)
	var checksGreen float64 = -1
	for _, g := range econ.Latency.Gates {
		if g.Gate == latency.GateChecksGreenToMerge {
			checksGreen = g.WaitSeconds
		}
	}
	if checksGreen != 900 {
		t.Errorf("checks_green_to_merge wait = %v, want 900 (synthesized from run_auto_advanced)", checksGreen)
	}
}

// ---------------------------------------------------------------------
// Sticky PR status comment cross-boundary integration (E42.1 / #1784).
// ---------------------------------------------------------------------

// prStatusDeps wires a Notifier against an issue-triggered run that OWNS a PR
// (non-empty pull_request_url) with an acceptance stage and an artifact lister,
// so NotifyStatusUpdateForRun maintains both the issue anchor and the PR status
// comment. The returned run pointer is mutable so a test can change state
// between rebuilds. PR number = 7, issue number = 42.
func prStatusDeps(t *testing.T) (runID, acceptanceStageID uuid.UUID, runRow *run.Run, gh *fakeGitHub, au *fakeAudit, arts *fakeArtifacts, n *issuecomment.Notifier) {
	t.Helper()
	runID = uuid.New()
	acceptanceStageID = uuid.New()
	triggerRef := "issue:42"
	prURL := "https://github.com/x/y/pull/7"
	runRow = &run.Run{
		ID: runID, Repo: "x/y", WorkflowID: "feature_change",
		TriggerSource: run.TriggerGitHubIssue, TriggerRef: &triggerRef,
		InstallationID: int64Ptr(99), State: run.StateRunning,
		PullRequestURL: &prURL,
	}
	stages := []*run.Stage{
		{ID: uuid.New(), RunID: runID, Sequence: 1, Type: run.StageTypeImplement, State: run.StageStateSucceeded},
		{ID: acceptanceStageID, RunID: runID, Sequence: 2, Type: run.StageTypeAcceptance, State: run.StageStateSucceeded},
	}
	repoRuns := &fakeRuns{
		runs:   map[uuid.UUID]*run.Run{runID: runRow},
		stages: map[uuid.UUID][]*run.Stage{runID: stages},
	}
	gh = &fakeGitHub{}
	au = &fakeAudit{}
	arts = &fakeArtifacts{byStage: map[uuid.UUID][]*artifact.Artifact{}}
	n = issuecomment.New(issuecomment.Deps{
		GitHub: gh, Runs: repoRuns, Audit: au, Artifacts: arts,
		ExternalURL: "https://app.example",
		Now:         func() time.Time { return time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC) },
	})
	if n == nil {
		t.Fatal("notifier nil")
	}
	return runID, acceptanceStageID, runRow, gh, au, arts, n
}

func prCreateCount(gh *fakeGitHub, issueNumber int) int {
	gh.mu.Lock()
	defer gh.mu.Unlock()
	c := 0
	for _, call := range gh.calls {
		if call.issueNumber == issueNumber {
			c++
		}
	}
	return c
}

func prUpdateCount(gh *fakeGitHub, commentID int64) int {
	gh.mu.Lock()
	defer gh.mu.Unlock()
	c := 0
	for _, call := range gh.updateCalls {
		if call.commentID == commentID {
			c++
		}
	}
	return c
}

func prStatusAuditCount(au *fakeAudit) int {
	c := 0
	for _, p := range au.appended {
		if p.Category == issuecomment.CategoryPRStatusCommentPosted {
			c++
		}
	}
	return c
}

// TestPRStatusComment_CreatesThenEditsSameComment: the first rebuild after
// pull_request_url is stamped CREATES exactly one PR comment; a second rebuild
// (state changed so the body differs) EDITS the same comment id — no stacking.
func TestPRStatusComment_CreatesThenEditsSameComment(t *testing.T) {
	runID, _, runRow, gh, au, _, n := prStatusDeps(t)

	if err := n.NotifyStatusUpdateForRun(context.Background(), runID); err != nil {
		t.Fatalf("first rebuild: %v", err)
	}
	// Anchor create → id 1 (issue 42); PR create → id 2 (issue/PR 7).
	if got := prCreateCount(gh, 7); got != 1 {
		t.Fatalf("expected 1 PR create; got %d", got)
	}
	if got := prStatusAuditCount(au); got != 1 {
		t.Fatalf("expected 1 pr_status_comment_posted row; got %d", got)
	}

	// Change state so the rendered PR body differs, forcing an edit.
	runRow.State = run.StateSucceeded
	if err := n.NotifyStatusUpdateForRun(context.Background(), runID); err != nil {
		t.Fatalf("second rebuild: %v", err)
	}
	if got := prCreateCount(gh, 7); got != 1 {
		t.Errorf("PR comment stacked: expected 1 create total, got %d", got)
	}
	if got := prUpdateCount(gh, 2); got != 1 {
		t.Errorf("expected 1 PR edit on comment id 2; got %d", got)
	}
}

// TestPRStatusComment_IdenticalBodySkipsEdit: a second rebuild with no state
// change renders an identical body, so the GitHub edit is skipped (no PATCH).
func TestPRStatusComment_IdenticalBodySkipsEdit(t *testing.T) {
	runID, _, _, gh, au, _, n := prStatusDeps(t)

	if err := n.NotifyStatusUpdateForRun(context.Background(), runID); err != nil {
		t.Fatalf("first rebuild: %v", err)
	}
	if err := n.NotifyStatusUpdateForRun(context.Background(), runID); err != nil {
		t.Fatalf("second rebuild: %v", err)
	}
	// PR comment id 2 must NOT have been edited (identical body → skip).
	if got := prUpdateCount(gh, 2); got != 0 {
		t.Errorf("identical body should skip the PR edit; got %d PATCH(es)", got)
	}
	if got := prStatusAuditCount(au); got != 1 {
		t.Errorf("skip should not append a second audit row; got %d", got)
	}
}

// TestPRStatusComment_SkipsWhenNoPR: a run without pull_request_url is skipped
// (owns-PR-only guard) — the anchor still posts, but no PR comment.
func TestPRStatusComment_SkipsWhenNoPR(t *testing.T) {
	runID, _, runRow, gh, au, _, n := prStatusDeps(t)
	runRow.PullRequestURL = nil // does not own a PR

	if err := n.NotifyStatusUpdateForRun(context.Background(), runID); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if got := prCreateCount(gh, 7); got != 0 {
		t.Errorf("no PR comment should be created without pull_request_url; got %d", got)
	}
	if got := prStatusAuditCount(au); got != 0 {
		t.Errorf("no pr_status_comment_posted row without a PR; got %d", got)
	}
	// The anchor still posted to the issue.
	if got := prCreateCount(gh, 42); got != 1 {
		t.Errorf("anchor comment should still be created; got %d", got)
	}
}

// TestPRStatusComment_GitHubErrorSwallowed: a PR-comment post failure is
// swallowed — NotifyStatusUpdateForRun still returns nil and the anchor update
// is unaffected (fail-open).
func TestPRStatusComment_GitHubErrorSwallowed(t *testing.T) {
	runID, _, _, gh, au, _, n := prStatusDeps(t)
	gh.failOnIssueNumber = 7 // fail only the PR-locus create, not the anchor

	if err := n.NotifyStatusUpdateForRun(context.Background(), runID); err != nil {
		t.Fatalf("PR-comment failure must not propagate; got %v", err)
	}
	if got := prStatusAuditCount(au); got != 0 {
		t.Errorf("failed post must not append an audit row; got %d", got)
	}
	// The anchor comment landed despite the PR-comment failure.
	if got := prCreateCount(gh, 42); got != 1 {
		t.Errorf("anchor comment unaffected: expected 1 create; got %d", got)
	}
}

// TestPRStatusComment_DeletedCommentRecreated: an operator-deleted PR comment
// (ErrNotFound on edit) is re-created rather than left stale.
func TestPRStatusComment_DeletedCommentRecreated(t *testing.T) {
	runID, _, _, gh, au, _, n := prStatusDeps(t)
	// Seed a prior PR comment id with a stale hash so the rebuild attempts an
	// edit (hash differs) rather than skipping.
	au.preSeed(runID, issuecomment.CategoryPRStatusCommentPosted, map[string]any{
		"kind": "pr_status_update", "pr_number": 7, "repo": "x/y",
		"github_comment_id": 555, "body_hash": "stale",
	})
	gh.updateErr = githubclient.ErrNotFound // the comment was deleted

	if err := n.NotifyStatusUpdateForRun(context.Background(), runID); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	// The edit 404'd, so a fresh create to the PR number happened.
	if got := prCreateCount(gh, 7); got != 1 {
		t.Errorf("deleted comment should be re-created; got %d PR create(s)", got)
	}
	if got := prStatusAuditCount(au); got != 1 {
		t.Errorf("re-create should append one fresh audit row; got %d", got)
	}
}

// TestPRStatusComment_LoadsAcceptanceArtifact: an acceptance_outcome_recorded
// entry causes the KindAcceptance artifact to be loaded and the per-criterion
// table to render in the PR comment.
func TestPRStatusComment_LoadsAcceptanceArtifact(t *testing.T) {
	runID, acceptanceStageID, _, gh, au, arts, n := prStatusDeps(t)

	// The KindAcceptance artifact carries the per-criterion detail (absent from
	// the audit payload).
	body, _ := json.Marshal(map[string]any{
		"verdict": "failed",
		"criteria": []map[string]any{
			{"id": "AC-1", "result": "passed", "expectation_basis": "issue statement"},
			{"id": "AC-2", "result": "failed", "observed": "returned 500"},
		},
	})
	arts.byStage[acceptanceStageID] = []*artifact.Artifact{
		{ID: uuid.New(), StageID: acceptanceStageID, Kind: artifact.KindAcceptance, ContentHash: "h1", Content: body},
	}
	// The outcome audit row references the acceptance stage + content hash so
	// loadAcceptanceArtifact resolves the artifact above.
	au.preSeed(runID, "acceptance_outcome_recorded", map[string]any{
		"outcome": "rejected", "criteria_passed": 1, "criteria_total": 2,
		"stage_id": acceptanceStageID.String(), "content_hash": "h1",
	})

	if err := n.NotifyStatusUpdateForRun(context.Background(), runID); err != nil {
		t.Fatalf("rebuild: %v", err)
	}

	// Find the PR-locus create (issue/PR number 7).
	var prBody string
	gh.mu.Lock()
	for _, c := range gh.calls {
		if c.issueNumber == 7 {
			prBody = c.body
		}
	}
	gh.mu.Unlock()
	if prBody == "" {
		t.Fatal("no PR comment was created")
	}
	for _, want := range []string{
		"| Criterion | Result | Basis |",
		"| `AC-1` | ✅ pass | issue statement |",
		"| `AC-2` | ❌ fail | returned 500 |",
	} {
		if !strings.Contains(prBody, want) {
			t.Errorf("PR comment missing acceptance table row %q:\n%s", want, prBody)
		}
	}
}
