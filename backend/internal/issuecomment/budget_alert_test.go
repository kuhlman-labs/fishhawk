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

func float64Ptr(v float64) *float64 { return &v }

func warnPayload() issuecomment.BudgetAlertPayload {
	return issuecomment.BudgetAlertPayload{
		WorkflowID:  "feature_change",
		Period:      "weekly",
		PeriodStart: "2026-05-04T00:00:00Z",
		Spent:       42,
		Limit:       50,
		Fraction:    0.84,
		WarnAt:      float64Ptr(0.8),
		Tier:        "warn",
	}
}

func overPayload() issuecomment.BudgetAlertPayload {
	p := warnPayload()
	p.Spent = 55
	p.Fraction = 1.1
	p.Tier = "over"
	return p
}

func TestNotifyBudgetAlert_WarnTier_PostsCommentAndAudit(t *testing.T) {
	runID, gh, au, n := happyDeps(t)
	if err := n.NotifyBudgetAlert(context.Background(), runID, warnPayload()); err != nil {
		t.Fatalf("NotifyBudgetAlert: %v", err)
	}
	if len(gh.calls) != 1 {
		t.Fatalf("expected 1 GitHub call; got %d", len(gh.calls))
	}
	body := gh.calls[0].body
	for _, want := range []string{"feature_change", "weekly", "approaching", "$42.00 of $50.00", "advisory", "undercounted"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q:\n%s", want, body)
		}
	}
	if len(au.appended) != 1 {
		t.Fatalf("expected 1 audit entry; got %d", len(au.appended))
	}
	var p map[string]any
	if err := json.Unmarshal(au.appended[0].Payload, &p); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if p["kind"] != "budget_alert" {
		t.Errorf("payload.kind = %v, want budget_alert", p["kind"])
	}
	if p["budget_tier"] != "warn" {
		t.Errorf("payload.budget_tier = %v, want warn", p["budget_tier"])
	}
	if p["period_start"] != "2026-05-04T00:00:00Z" {
		t.Errorf("payload.period_start = %v", p["period_start"])
	}
}

func TestNotifyBudgetAlert_OverTier_RendersExhausted(t *testing.T) {
	runID, gh, _, n := happyDeps(t)
	if err := n.NotifyBudgetAlert(context.Background(), runID, overPayload()); err != nil {
		t.Fatalf("NotifyBudgetAlert: %v", err)
	}
	if len(gh.calls) != 1 {
		t.Fatalf("expected 1 GitHub call; got %d", len(gh.calls))
	}
	body := gh.calls[0].body
	if !strings.Contains(body, "has exhausted") {
		t.Errorf("over-tier body should say exhausted:\n%s", body)
	}
	if !strings.Contains(body, "(110%)") {
		t.Errorf("over-tier body should show fraction:\n%s", body)
	}
}

func TestNotifyBudgetAlert_PerTierDedup(t *testing.T) {
	runID, gh, au, n := happyDeps(t)

	// First the warn tier posts.
	if err := n.NotifyBudgetAlert(context.Background(), runID, warnPayload()); err != nil {
		t.Fatal(err)
	}
	// A redelivery of the SAME (period_start, tier) is absorbed.
	if err := n.NotifyBudgetAlert(context.Background(), runID, warnPayload()); err != nil {
		t.Fatal(err)
	}
	if len(gh.calls) != 1 {
		t.Fatalf("warn redelivery should dedup; got %d calls", len(gh.calls))
	}
	// The 100% tier is a distinct crossing in the same period and posts.
	if err := n.NotifyBudgetAlert(context.Background(), runID, overPayload()); err != nil {
		t.Fatal(err)
	}
	if len(gh.calls) != 2 {
		t.Fatalf("over tier should post; got %d calls", len(gh.calls))
	}
	if len(au.appended) != 2 {
		t.Errorf("expected 2 audit entries (warn + over); got %d", len(au.appended))
	}

	// A new calendar period's warn posts again (period_start differs).
	next := warnPayload()
	next.PeriodStart = "2026-05-11T00:00:00Z"
	if err := n.NotifyBudgetAlert(context.Background(), runID, next); err != nil {
		t.Fatal(err)
	}
	if len(gh.calls) != 3 {
		t.Fatalf("next-period warn should post; got %d calls", len(gh.calls))
	}
}

func TestNotifyBudgetAlert_PreSeededDedup(t *testing.T) {
	runID, gh, au, n := happyDeps(t)
	// An existing budget_alert comment for this (period_start, tier)
	// suppresses a repeat — the durable, restart-surviving dedup.
	au.preSeed(runID, issuecomment.CategoryIssueCommented, map[string]any{
		"kind":         "budget_alert",
		"period_start": "2026-05-04T00:00:00Z",
		"budget_tier":  "warn",
	})
	if err := n.NotifyBudgetAlert(context.Background(), runID, warnPayload()); err != nil {
		t.Fatal(err)
	}
	if len(gh.calls) != 0 {
		t.Errorf("pre-seeded tier should dedup; got %d calls", len(gh.calls))
	}
}

func TestNotifyBudgetAlert_SkipsNonIssueTrigger(t *testing.T) {
	runID := uuid.New()
	cliRef := "cli:adhoc"
	repoRuns := &fakeRuns{
		runs: map[uuid.UUID]*run.Run{runID: {
			ID: runID, Repo: "x/y",
			WorkflowID:     "feature_change",
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
		Now:         func() time.Time { return time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC) },
	})
	if err := n.NotifyBudgetAlert(context.Background(), runID, warnPayload()); err != nil {
		t.Fatal(err)
	}
	if len(gh.calls) != 0 {
		t.Errorf("non-issue trigger should skip; got %d calls", len(gh.calls))
	}
	if len(au.appended) != 0 {
		t.Errorf("non-issue trigger should write no audit; got %d", len(au.appended))
	}
}

func TestNotifyBudgetAlert_EmptyTier_NoOp(t *testing.T) {
	runID, gh, _, n := happyDeps(t)
	p := warnPayload()
	p.Tier = ""
	if err := n.NotifyBudgetAlert(context.Background(), runID, p); err != nil {
		t.Fatal(err)
	}
	if len(gh.calls) != 0 {
		t.Errorf("empty tier should no-op; got %d calls", len(gh.calls))
	}
}
