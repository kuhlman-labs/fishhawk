package issuecomment

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

const runlinkBase = "https://app.example"

// runlinkRun is the shared fixture: a fixed run id so the short-id ("11111111")
// and the absolute /runs/<id> link are predictable across every surface.
func runlinkRun() *run.Run {
	return &run.Run{
		ID:         uuid.MustParse("11111111-2222-3333-4444-555555555555"),
		WorkflowID: "feature_change",
		State:      run.StateRunning,
	}
}

// TestRunShortLink covers both branches of the shared header/inline helper: a
// configured base URL yields the markdown link, an unset base URL yields the
// bare backticked short-id with no link and no localhost literal (#1787).
func TestRunShortLink(t *testing.T) {
	id := runlinkRun().ID
	if got := runShortLink(runlinkBase, id); got != "[`11111111`](https://app.example/runs/"+id.String()+")" {
		t.Errorf("configured runShortLink = %q", got)
	}
	// A trailing slash on the base is trimmed, not doubled.
	if got := runShortLink(runlinkBase+"/", id); got != "[`11111111`](https://app.example/runs/"+id.String()+")" {
		t.Errorf("trailing-slash runShortLink = %q", got)
	}
	if got := runShortLink("", id); got != "`11111111`" {
		t.Errorf("unset runShortLink = %q, want the bare backticked short-id", got)
	}
}

// TestViewRunLink covers both branches of the footer/attribution helper: a
// configured base URL yields the labeled link, an unset base URL yields "" so
// the caller omits the link AND its separator (#1787).
func TestViewRunLink(t *testing.T) {
	id := runlinkRun().ID
	if got := viewRunLink("View run →", runlinkBase, id); got != "[View run →](https://app.example/runs/"+id.String()+")" {
		t.Errorf("configured viewRunLink = %q", got)
	}
	if got := viewRunLink("View run →", "", id); got != "" {
		t.Errorf("unset viewRunLink = %q, want empty (omit branch)", got)
	}
}

// TestRunURLFor covers both branches of the bare-URL helper that flows into
// commentContext.runURL: configured yields the URL, unset yields "" so the
// downstream renderers branch uniformly on emptiness (#1787).
func TestRunURLFor(t *testing.T) {
	id := runlinkRun().ID
	if got := runURLFor(runlinkBase, id); got != "https://app.example/runs/"+id.String() {
		t.Errorf("configured runURLFor = %q", got)
	}
	if got := runURLFor("", id); got != "" {
		t.Errorf("unset runURLFor = %q, want empty", got)
	}
}

// TestRunLinkDegradation_PerSurface is the core done-means table (#1787). For
// EVERY issue-comment surface renderer it asserts the shipped behavior in both
// branches: with a configured absolute base URL the exact absolute run link is
// present; with an UNSET base URL no localhost literal appears, no relative
// run link (`](/…`) appears, no /runs/ link fragment appears at all, and (for
// the surfaces that render a run short-id) the plain backticked short-id is
// present. Asserting the rendered body — not just that a file was touched —
// means a no-op edit that left a link in the unset branch fails here. The
// anchor (issue locus) and PR-status (E42.1 sticky PR comment locus) are BOTH
// in the table, in both branches (binding condition 1).
func TestRunLinkDegradation_PerSurface(t *testing.T) {
	r := runlinkRun()
	idStr := r.ID.String()
	now := time.Unix(1000, 0).UTC()

	prReviewEntry := implementReviewedFixture(t)

	cases := []struct {
		name    string
		render  func(externalURL string) string
		shortID string // the backticked short-id expected in the unset branch; "" when the surface renders no run short-id (PR review)
	}{
		{
			name: "living anchor",
			render: func(externalURL string) string {
				return RenderAnchorBody(AnchorInput{
					Run:         r,
					Stages:      []*run.Stage{{Type: run.StageTypeImplement, State: run.StageStateRunning}},
					ExternalURL: externalURL,
					Now:         now,
				})
			},
			shortID: "11111111",
		},
		{
			name: "sticky status comment",
			render: func(externalURL string) string {
				return RenderStatusBody(r, []*run.Stage{{Type: run.StageTypeImplement, State: run.StageStateRunning}}, nil, externalURL, now)
			},
			shortID: "11111111",
		},
		{
			name: "sticky PR status comment",
			render: func(externalURL string) string {
				return RenderPRStatusBody(PRStatusInput{
					Run:         r,
					Stages:      []*run.Stage{{Type: run.StageTypeImplement, State: run.StageStateRunning}},
					ExternalURL: externalURL,
					Now:         now,
				})
			},
			shortID: "11111111",
		},
		{
			name: "agent-review PR review",
			render: func(externalURL string) string {
				return RenderPRReviewBody(prReviewEntry, r, externalURL)
			},
			shortID: "", // the PR review body renders only the attribution link, no short-id
		},
		{
			name: "CI-failure retry body",
			render: func(externalURL string) string {
				return renderCIRetryBody(commentContext{run: r}, uuid.MustParse("99999999-8888-7777-6666-555555555555"), "build", 1, 2, externalURL)
			},
			shortID: "11111111",
		},
		{
			name: "budget-alert body",
			render: func(externalURL string) string {
				return renderBudgetAlertBody(commentContext{run: r}, BudgetAlertPayload{
					WorkflowID: "feature_change", Period: "weekly", Spent: 42, Limit: 50, Fraction: 0.84, Tier: "warn",
				}, externalURL)
			},
			shortID: "11111111",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Configured branch: the exact absolute run link is present.
			cfg := tc.render(runlinkBase)
			wantLink := "https://app.example/runs/" + idStr
			if !strings.Contains(cfg, wantLink) {
				t.Errorf("configured %s: missing absolute run link %q\n---\n%s", tc.name, wantLink, cfg)
			}

			// Unset branch: no localhost, no relative run link, no /runs/ link
			// fragment at all, and the plain backticked short-id when the surface
			// renders one.
			unset := tc.render("")
			if strings.Contains(unset, "localhost") {
				t.Errorf("unset %s: leaks a localhost literal\n---\n%s", tc.name, unset)
			}
			if strings.Contains(unset, "](/") {
				t.Errorf("unset %s: rendered a relative run link\n---\n%s", tc.name, unset)
			}
			if strings.Contains(unset, "/runs/") {
				t.Errorf("unset %s: rendered a run-page link fragment\n---\n%s", tc.name, unset)
			}
			if tc.shortID != "" && !strings.Contains(unset, "`"+tc.shortID+"`") {
				t.Errorf("unset %s: missing the plain backticked short-id `%s`\n---\n%s", tc.name, tc.shortID, unset)
			}
		})
	}
}

// TestTruncateForGitHubComment_UnsetExternalURL covers the oversize-comment
// truncation marker's #1787 branch: with the base URL unset the tail degrades to
// a link-less "…truncated." marker (no relative /runs path, no localhost); with
// it configured the tail carries the absolute view-full-plan link. A short body
// is returned unchanged in both branches.
func TestTruncateForGitHubComment_UnsetExternalURL(t *testing.T) {
	id := runlinkRun().ID
	stageID := uuid.New().String()
	big := strings.Repeat("x", MaxIssueCommentBodyBytes+100)

	// Short body: unchanged regardless of branch.
	if got := truncateForGitHubComment("short", "", stageID, "", id.String()); got != "short" {
		t.Errorf("short body should pass through unchanged; got %q", got)
	}

	unset := truncateForGitHubComment(big, "", stageID, "", id.String())
	if !strings.Contains(unset, "_…truncated._") {
		t.Errorf("unset truncation should carry the link-less marker:\n%s", unset[len(unset)-80:])
	}
	if strings.Contains(unset, "localhost") || strings.Contains(unset, "/runs/") || strings.Contains(unset, "](/") {
		t.Errorf("unset truncation must not render a run link")
	}

	cfg := truncateForGitHubComment(big, runURLFor(runlinkBase, id), stageID, runlinkBase, id.String())
	wantTail := "[view full plan →](https://app.example/runs/" + id.String() + "/stages/" + stageID + ")"
	if !strings.Contains(cfg, wantTail) {
		t.Errorf("configured truncation should carry the absolute view-full-plan link:\n%s", cfg[len(cfg)-160:])
	}
}

// implementReviewedFixture builds a minimal terminal implement_reviewed entry so
// RenderPRReviewBody produces a body (a verdict is required, else it returns "").
func implementReviewedFixture(t *testing.T) *audit.Entry {
	t.Helper()
	body, err := json.Marshal(planreview.ImplementReviewedPayload{
		ReviewerModel: "claude-opus-4-8",
		Verdict:       planreview.VerdictApprove,
		FreeForm:      "looks correct",
	})
	if err != nil {
		t.Fatalf("marshal implement_reviewed payload: %v", err)
	}
	return &audit.Entry{Sequence: 5, Category: "implement_reviewed", Payload: body}
}
