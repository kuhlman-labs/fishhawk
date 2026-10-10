package mcpserver

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// avEntry builds an audit entry for the #4072 tables, stage-scoped when
// stageID is non-empty and UNSCOPED (nil StageID) otherwise.
func avEntry(category string, seq int64, stageID string, payload any) AuditEntry {
	e := AuditEntry{Category: category, Sequence: seq, Payload: payload}
	if stageID != "" {
		sid := stageID
		e.StageID = &sid
	}
	return e
}

func avOutcome(seq int64, stageID, verdict string) AuditEntry {
	return avEntry(auditCategoryAcceptanceOutcomeRecorded, seq, stageID, map[string]any{"verdict": verdict})
}

func avDispatched(seq int64, stageID string) AuditEntry {
	return avEntry(auditCategoryAcceptanceDispatched, seq, stageID, map[string]any{})
}

// TestAcceptanceAttemptVerdictIn pins the per-attempt walk: newest-first by
// Sequence (whatever the input order), stage-scoped, and stopping at the
// attempt's own anchor so a PRIOR attempt's verdict is never reported.
//
// Counterfactuals (each run, each RED on the named row):
//   - anchor case -> continue: "anchor above older outcome" returns failed.
//   - drop the StageID filter: "other stage's outcome is ignored" returns passed.
//   - delete the sort: "ascending input" stops at the anchor and returns no verdict.
func TestAcceptanceAttemptVerdictIn(t *testing.T) {
	const s = "stage-S"
	const other = "stage-OTHER"
	cases := []struct {
		name    string
		entries []AuditEntry
		want    acceptanceAttemptVerdict
	}{
		{"empty slice is undetermined", nil, acceptanceAttemptVerdict{}},
		{"outcome above anchor", []AuditEntry{avOutcome(11, s, "passed"), avDispatched(10, s)},
			acceptanceAttemptVerdict{Determined: true, Verdict: "passed"}},
		{"anchor above older outcome (prior attempt's verdict is NOT reported)",
			[]AuditEntry{avDispatched(12, s), avOutcome(11, s, "failed")},
			acceptanceAttemptVerdict{Determined: true}},
		{"reopen above older outcome",
			[]AuditEntry{avEntry(categoryAcceptanceReopened, 12, s, map[string]any{}), avOutcome(11, s, "failed")},
			acceptanceAttemptVerdict{Determined: true}},
		{"other stage's outcome is ignored",
			[]AuditEntry{avOutcome(12, other, "passed"), avDispatched(11, s)},
			acceptanceAttemptVerdict{Determined: true}},
		{"ascending input is sorted newest-first",
			[]AuditEntry{avDispatched(10, s), avOutcome(11, s, "passed")},
			acceptanceAttemptVerdict{Determined: true, Verdict: "passed"}},
		{"unshipped marker above outcome",
			[]AuditEntry{avEntry(auditCategoryAcceptanceVerdictUnshipped, 12, s, map[string]any{}), avOutcome(11, s, "passed")},
			acceptanceAttemptVerdict{Determined: true, Disposition: acceptanceVerdictUnshipped}},
		{"outcome above unshipped marker",
			[]AuditEntry{avOutcome(13, s, "passed"), avEntry(auditCategoryAcceptanceVerdictUnshipped, 12, s, map[string]any{})},
			acceptanceAttemptVerdict{Determined: true, Verdict: "passed"}},
		{"skip marker",
			[]AuditEntry{avEntry(auditCategoryAcceptanceSkippedOutOfScope, 5, s, map[string]any{})},
			acceptanceAttemptVerdict{Determined: true, Disposition: acceptanceAttemptDispositionSkipped}},
		{"unscoped outcome is ignored (undetermined)",
			[]AuditEntry{avOutcome(9, "", "passed")},
			acceptanceAttemptVerdict{}},
		{"exhausted slice with only unrelated entries is undetermined",
			[]AuditEntry{avEntry("implement_reviewed", 30, s, map[string]any{}), avEntry("stage_progress", 29, s, map[string]any{})},
			acceptanceAttemptVerdict{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := acceptanceAttemptVerdictIn(tc.entries, s); got != tc.want {
				t.Errorf("acceptanceAttemptVerdictIn = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestAcceptanceVerdictHoldUntil pins the cross-clock clamp to
// [now, now+window] (the #3048 class): skew in either direction can neither
// make the hold unbounded nor negative.
func TestAcceptanceVerdictHoldUntil(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	window := 120 * time.Second
	at := func(d time.Duration) *time.Time { v := now.Add(d); return &v }
	cases := []struct {
		name    string
		endedAt *time.Time
		want    time.Time
	}{
		{"nil ended_at holds a full window from now", nil, now.Add(window)},
		{"fresh settle", at(-5 * time.Second), now.Add(window - 5*time.Second)},
		{"window long closed clamps to now", at(-time.Hour), now},
		{"backend clock ahead clamps to now+window", at(10 * time.Minute), now.Add(window)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := acceptanceVerdictHoldUntil(tc.endedAt, now, window); !got.Equal(tc.want) {
				t.Errorf("acceptanceVerdictHoldUntil = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestAcceptanceVerdictSignal pins the sentinel derivation both nextActionsFor
// call sites share.
//
// Counterfactuals (each run, each RED):
//   - return latestAcceptanceVerdict unconditionally: "fresh settle" row.
//   - remove the window check: "window closed" row.
//   - drop the skip-marker check (binding condition C1): "skip marker" row.
func TestAcceptanceVerdictSignal(t *testing.T) {
	now := time.Now().UTC()
	acc := func(state string, endedAgo *time.Duration) []Stage {
		st := naAcceptanceStages(state)
		if endedAgo != nil {
			v := now.Add(-*endedAgo)
			st[2].EndedAt = &v
		}
		return st
	}
	d := func(v time.Duration) *time.Duration { return &v }
	cases := []struct {
		name   string
		recent []AuditEntry
		stages []Stage
		want   string
	}{
		{"fresh settle, no verdict -> sentinel", nil, acc("succeeded", d(5*time.Second)), acceptanceVerdictPending},
		{"anchor only, fresh settle -> sentinel", []AuditEntry{avDispatched(3, "x")}, acc("succeeded", d(5*time.Second)), acceptanceVerdictPending},
		{"window closed -> empty", nil, acc("succeeded", d(time.Hour)), ""},
		{"nil ended_at -> empty", nil, acc("succeeded", nil), ""},
		{"failed stage -> empty", nil, acc("failed", d(5*time.Second)), ""},
		{"no acceptance stage -> empty", nil, []Stage{naStage("implement", "succeeded")}, ""},
		{"recorded verdict passes through", []AuditEntry{naOutcomeEntry(4, acceptanceVerdictPassed)}, acc("succeeded", d(5*time.Second)), acceptanceVerdictPassed},
		{"unshipped marker passes through", []AuditEntry{naUnshippedEntry(4)}, acc("succeeded", d(5*time.Second)), acceptanceVerdictUnshipped},
		{"skip marker -> empty, never the sentinel (C1)", []AuditEntry{rsSkipEntry(4)}, acc("succeeded", d(5*time.Second)), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := acceptanceVerdictSignal(tc.recent, tc.stages, now); got != tc.want {
				t.Errorf("acceptanceVerdictSignal = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestDecorateSettledWithAcceptanceVerdict pins each release branch of the pure
// decorator, including the #3081-style pairing of the top-level flag with the
// stage_wait_status block.
func TestDecorateSettledWithAcceptanceVerdict(t *testing.T) {
	base := func() AwaitStageOutput {
		return AwaitStageOutput{Status: "settled", State: "succeeded", StageWaitStatus: &StageWaitStatus{Stage: "acceptance", Status: "succeeded"}}
	}
	t.Run("verdict", func(t *testing.T) {
		out := decorateSettledWithAcceptanceVerdict(base(), acceptanceVerdictHold{Verdict: "failed"})
		if out.AcceptanceVerdict != "failed" || out.VerdictPending || out.Message != "" {
			t.Errorf("out = %+v, want acceptance_verdict failed, no pending, no message", out)
		}
	})
	t.Run("pending pairs block and top level", func(t *testing.T) {
		out := decorateSettledWithAcceptanceVerdict(base(), acceptanceVerdictHold{Pending: true})
		if !out.VerdictPending || !out.StageWaitStatus.VerdictPending {
			t.Errorf("verdict_pending top/block = %v/%v, want true/true", out.VerdictPending, out.StageWaitStatus.VerdictPending)
		}
		if !strings.Contains(out.Message, "do NOT fishhawk_retry_stage") || !strings.Contains(out.Message, "may still land") {
			t.Errorf("message = %q, want the may-still-land / do-not-retry advisory", out.Message)
		}
	})
	t.Run("pending with nil block still sets the top level", func(t *testing.T) {
		in := base()
		in.StageWaitStatus = nil
		out := decorateSettledWithAcceptanceVerdict(in, acceptanceVerdictHold{Pending: true})
		if !out.VerdictPending {
			t.Error("verdict_pending = false, want true even with no stage_wait_status block")
		}
	})
	t.Run("confirmed absence names list_audit then retry", func(t *testing.T) {
		out := decorateSettledWithAcceptanceVerdict(base(), acceptanceVerdictHold{})
		if out.VerdictPending {
			t.Error("verdict_pending = true, want false on a confirmed absence past the window")
		}
		li, ri := strings.Index(out.Message, "fishhawk_list_audit"), strings.Index(out.Message, "fishhawk_retry_stage")
		if li < 0 || ri < 0 || li > ri {
			t.Errorf("message = %q, want fishhawk_list_audit named BEFORE fishhawk_retry_stage", out.Message)
		}
	})
	t.Run("unshipped disposition", func(t *testing.T) {
		out := decorateSettledWithAcceptanceVerdict(base(), acceptanceVerdictHold{Disposition: acceptanceVerdictUnshipped})
		if out.VerdictPending || !strings.Contains(out.Message, auditCategoryAcceptanceVerdictUnshipped) {
			t.Errorf("out = %+v, want a message naming %s and no pending", out, auditCategoryAcceptanceVerdictUnshipped)
		}
	})
	t.Run("skipped disposition", func(t *testing.T) {
		out := decorateSettledWithAcceptanceVerdict(base(), acceptanceVerdictHold{Disposition: acceptanceAttemptDispositionSkipped})
		if out.VerdictPending || !strings.Contains(out.Message, auditCategoryAcceptanceSkippedOutOfScope) {
			t.Errorf("out = %+v, want a message naming %s and no pending", out, auditCategoryAcceptanceSkippedOutOfScope)
		}
	})
}

// TestGetRunStatus_AcceptanceVerdictPending drives the REAL getRunStatus over
// the fake backend: wire JSON -> Stage.EndedAt decode -> acceptanceVerdictSignal
// -> classifier -> output. A fresh acceptance settle with only its dispatch
// anchor in the window must classify acceptance_verdict_pending (wait), set
// acceptance_stage_wait_status.verdict_pending, and offer neither retry nor
// merge.
//
// Counterfactuals (each run, each RED): revert tools.go to
// latestAcceptanceVerdict -> state acceptance_settled_outcome_unknown; delete
// the VerdictPending assignment -> the block flag stays false.
func TestGetRunStatus_AcceptanceVerdictPending(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	accID := uuid.NewString()
	prURL := "https://github.com/x/y/pull/4072"
	ended := time.Now().UTC().Add(-3 * time.Second)
	fb.getRunByID[runID] = Run{
		ID: runID.String(), Repo: "x/y", WorkflowID: "feature_change",
		State: "running", PullRequestURL: &prURL,
	}
	fb.stagesByRun[runID] = []Stage{
		{ID: uuid.NewString(), RunID: runID.String(), Sequence: 1, Type: "plan", State: "succeeded"},
		{ID: uuid.NewString(), RunID: runID.String(), Sequence: 2, Type: "implement", State: "succeeded"},
		{ID: accID, RunID: runID.String(), Sequence: 3, Type: "acceptance", State: "succeeded", EndedAt: &ended},
	}
	sid := accID
	fb.auditByRun[runID] = []AuditEntry{{
		ID: uuid.NewString(), Sequence: 1, RunID: runID.String(), StageID: &sid,
		Category: auditCategoryAcceptanceDispatched, Payload: map[string]any{}, EntryHash: "h",
	}}

	r := newResolver(srv, nil)
	_, out, err := r.getRunStatus(context.Background(), nil, GetRunStatusInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getRunStatus: %v", err)
	}
	if out.NextActions == nil || out.NextActions.State != "acceptance_verdict_pending" {
		t.Fatalf("next_actions = %+v, want state acceptance_verdict_pending", out.NextActions)
	}
	if out.AcceptanceStageWaitStatus == nil || !out.AcceptanceStageWaitStatus.VerdictPending {
		t.Errorf("acceptance_stage_wait_status = %+v, want verdict_pending true", out.AcceptanceStageWaitStatus)
	}
	for _, banned := range []string{"fishhawk_retry_stage", "approve_pr", "fishhawk_merge_run"} {
		if nextActionOffered(out.NextActions, banned) {
			t.Errorf("verdict_pending offered %s; want a wait, never retry or merge: %+v", banned, out.NextActions.Actions)
		}
	}
	if !nextActionOffered(out.NextActions, "fishhawk_await_stage") {
		t.Errorf("verdict_pending should offer fishhawk_await_stage; got %+v", out.NextActions.Actions)
	}
}
