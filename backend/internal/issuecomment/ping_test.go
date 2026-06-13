package issuecomment_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/issuecomment"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

func TestNotifyPagePing_PostsOneLineCommentAndAuditRow(t *testing.T) {
	runID, gh, au, n := happyDeps(t)
	stageID := uuid.New()
	err := n.NotifyPagePing(context.Background(), runID, issuecomment.PagePing{
		Event:   issuecomment.PageEventGateAwaitingApproval,
		StageID: &stageID,
		Summary: "Plan awaiting your approval",
	})
	if err != nil {
		t.Fatalf("NotifyPagePing: %v", err)
	}
	if len(gh.calls) != 1 {
		t.Fatalf("expected 1 GitHub create; got %d", len(gh.calls))
	}
	body := gh.calls[0].body
	if !strings.Contains(body, "Plan awaiting your approval") {
		t.Errorf("body missing summary: %q", body)
	}
	if !strings.Contains(body, "run status") {
		t.Errorf("body missing anchor link text: %q", body)
	}
	// One line only.
	if n := strings.Count(strings.TrimSpace(body), "\n"); n != 0 {
		t.Errorf("ping body should be one line; got %d newlines: %q", n, body)
	}
	if len(au.appended) != 1 {
		t.Fatalf("expected 1 audit row; got %d", len(au.appended))
	}
	var p map[string]any
	if err := json.Unmarshal(au.appended[0].Payload, &p); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if p["kind"] != "page_ping" {
		t.Errorf("payload.kind = %v, want page_ping", p["kind"])
	}
	if p["event"] != issuecomment.PageEventGateAwaitingApproval {
		t.Errorf("payload.event = %v, want %s", p["event"], issuecomment.PageEventGateAwaitingApproval)
	}
	if p["stage_id"] != stageID.String() {
		t.Errorf("payload.stage_id = %v, want %s", p["stage_id"], stageID)
	}
	// The audit row carries the stage id in the chained-entry field too.
	if au.appended[0].StageID == nil || *au.appended[0].StageID != stageID {
		t.Errorf("audit StageID = %v, want %s", au.appended[0].StageID, stageID)
	}
}

func TestNotifyPagePing_LinksAnchorCommentPermalink(t *testing.T) {
	runID, gh, au, n := happyDeps(t)
	// Seed an existing anchor comment id via a status_comment_posted row.
	au.preSeed(runID, issuecomment.CategoryStatusCommentPosted, map[string]any{
		"kind":              "status_update",
		"github_comment_id": 7777,
	})
	if err := n.NotifyPagePing(context.Background(), runID, issuecomment.PagePing{
		Event:   issuecomment.PageEventReviewerReject,
		Summary: "A reviewer rejected the plan",
	}); err != nil {
		t.Fatalf("NotifyPagePing: %v", err)
	}
	if len(gh.calls) != 1 {
		t.Fatalf("expected 1 create; got %d", len(gh.calls))
	}
	body := gh.calls[0].body
	if !strings.Contains(body, "#issuecomment-7777") {
		t.Errorf("body should link the anchor comment permalink: %q", body)
	}
	if !strings.Contains(body, "/x/y/issues/42#") {
		t.Errorf("permalink should target the run's repo+issue: %q", body)
	}
}

func TestNotifyPagePing_FallsBackToRunURLWithoutAnchor(t *testing.T) {
	runID, gh, _, n := happyDeps(t)
	if err := n.NotifyPagePing(context.Background(), runID, issuecomment.PagePing{
		Event:   issuecomment.PageEventMustPageHuman,
		Summary: "Human approval required",
	}); err != nil {
		t.Fatalf("NotifyPagePing: %v", err)
	}
	body := gh.calls[0].body
	if !strings.Contains(body, "/runs/"+runID.String()) {
		t.Errorf("without an anchor the ping should link the run page: %q", body)
	}
}

func TestNotifyPagePing_DedupsPerStageAndEvent(t *testing.T) {
	runID, gh, au, n := happyDeps(t)
	stageID := uuid.New()
	ping := issuecomment.PagePing{
		Event:   issuecomment.PageEventGateAwaitingApproval,
		StageID: &stageID,
		Summary: "Plan awaiting your approval",
	}
	// First fire posts.
	if err := n.NotifyPagePing(context.Background(), runID, ping); err != nil {
		t.Fatalf("first: %v", err)
	}
	// Redelivery / retry of the SAME moment is suppressed.
	if err := n.NotifyPagePing(context.Background(), runID, ping); err != nil {
		t.Fatalf("second: %v", err)
	}
	if len(gh.calls) != 1 {
		t.Fatalf("same moment should page once; got %d comments", len(gh.calls))
	}
	if len(au.appended) != 1 {
		t.Fatalf("same moment should write one audit row; got %d", len(au.appended))
	}
	// A DIFFERENT event on the same stage pages again.
	if err := n.NotifyPagePing(context.Background(), runID, issuecomment.PagePing{
		Event:   issuecomment.PageEventReviewerReject,
		StageID: &stageID,
		Summary: "Reviewer rejected",
	}); err != nil {
		t.Fatalf("third: %v", err)
	}
	if len(gh.calls) != 2 {
		t.Fatalf("distinct event should page; got %d comments", len(gh.calls))
	}
}

func TestNotifyPagePing_SkipsNonIssueTrigger(t *testing.T) {
	// happyDepsCLI builds a CLI-triggered run; the ping must no-op.
	runID, gh, _, n := happyDepsCLITrigger(t)
	if err := n.NotifyPagePing(context.Background(), runID, issuecomment.PagePing{
		Event:   issuecomment.PageEventCIFailure,
		Summary: "CI failed",
	}); err != nil {
		t.Fatalf("NotifyPagePing: %v", err)
	}
	if len(gh.calls) != 0 {
		t.Errorf("non-issue trigger should not post; got %d", len(gh.calls))
	}
}

func TestNotifyPagePing_SkipsEmptyEventOrSummary(t *testing.T) {
	runID, gh, _, n := happyDeps(t)
	for _, p := range []issuecomment.PagePing{
		{Event: "", Summary: "x"},
		{Event: issuecomment.PageEventReviewerReject, Summary: "   "},
	} {
		if err := n.NotifyPagePing(context.Background(), runID, p); err != nil {
			t.Fatalf("NotifyPagePing(%+v): %v", p, err)
		}
	}
	if len(gh.calls) != 0 {
		t.Errorf("empty event/summary should not post; got %d", len(gh.calls))
	}
}

func TestNotifyPagePing_NilReceiver(t *testing.T) {
	var n *issuecomment.Notifier
	if err := n.NotifyPagePing(context.Background(), uuid.New(), issuecomment.PagePing{
		Event:   issuecomment.PageEventReviewerReject,
		Summary: "x",
	}); err != nil {
		t.Fatalf("nil receiver should be a no-op; got %v", err)
	}
}

func TestNotifyCIRetry_IncludesAnchorLink(t *testing.T) {
	runID, gh, au, n := happyDeps(t)
	// Seed the retry run's anchor comment id.
	au.preSeed(runID, issuecomment.CategoryStatusCommentPosted, map[string]any{
		"kind":              "status_update",
		"github_comment_id": 4242,
	})
	if err := n.NotifyCIRetry(context.Background(), runID, uuid.New(), "ci/build", 1, 2); err != nil {
		t.Fatalf("NotifyCIRetry: %v", err)
	}
	if len(gh.calls) != 1 {
		t.Fatalf("expected 1 comment; got %d", len(gh.calls))
	}
	body := gh.calls[0].body
	if !strings.Contains(body, "#issuecomment-4242") {
		t.Errorf("CI-retry body should link the run anchor: %q", body)
	}
	if !strings.Contains(body, "run status") {
		t.Errorf("CI-retry body should carry the anchor link text: %q", body)
	}
	// Per-attempt dedup is unchanged: a redelivery of the same attempt is
	// suppressed.
	if err := n.NotifyCIRetry(context.Background(), runID, uuid.New(), "ci/build", 1, 2); err != nil {
		t.Fatalf("NotifyCIRetry redelivery: %v", err)
	}
	if len(gh.calls) != 1 {
		t.Errorf("same attempt should post once; got %d", len(gh.calls))
	}
}

// happyDepsCLITrigger wires a Notifier against a CLI-triggered run so the
// non-issue short-circuit can be exercised.
func happyDepsCLITrigger(t *testing.T) (uuid.UUID, *fakeGitHub, *fakeAudit, *issuecomment.Notifier) {
	t.Helper()
	runID := uuid.New()
	repoRuns := &fakeRuns{
		runs: map[uuid.UUID]*run.Run{runID: {
			ID:            runID,
			Repo:          "x/y",
			WorkflowID:    "feature_change",
			TriggerSource: run.TriggerCLI,
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
