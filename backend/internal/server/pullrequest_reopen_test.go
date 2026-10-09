package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/webhook"
)

// pullrequest_reopen_test.go pins handlePullRequestReopened (#4082) as a table
// over ONE all-guards-pass baseline: every row perturbs exactly the input its
// guard reads, so deleting that guard leaves no LATER guard to mask the
// deletion and the row observes a revive instead of its refusal. The cross-
// layer proof over real Postgres is pullrequest_reopen_pg_test.go.

// reopenAuditRepo wraps prEventsAuditRepo with a per-category list failure,
// the input the audit_unreadable guard reads.
type reopenAuditRepo struct {
	*prEventsAuditRepo
	listErr map[string]error
}

func (r *reopenAuditRepo) ListForRunByCategory(ctx context.Context, runID uuid.UUID, category string) ([]*audit.Entry, error) {
	if err := r.listErr[category]; err != nil {
		return nil, err
	}
	return r.prEventsAuditRepo.ListForRunByCategory(ctx, runID, category)
}

// noReviverRunRepo exposes ONLY run.Repository, hiding the fake's
// ReviveRunOnReopen — the input the reviver_unavailable guard reads.
type noReviverRunRepo struct{ run.Repository }

const reopenTestPRURL = "https://github.com/x/y/pull/42"

// reopenFixture is one row's world. Rows mutate it from the baseline.
type reopenFixture struct {
	runID, reviewID, acceptanceID uuid.UUID
	runState                      run.State
	reviewState                   run.StageState
	acceptanceState               run.StageState
	stagesErr                     error
	noReviewStage                 bool

	// The anchoring close row's payload + timestamp. closeFields nil omits
	// the row entirely.
	closeFields map[string]any
	closeAge    time.Duration
	// extra are seeded audit rows, keyed by category.
	extra map[string][]*audit.Entry

	reopenHead string
	listErr    map[string]error
	reviveErr  error
	noReviver  bool
}

func baselineReopenFixture() *reopenFixture {
	return &reopenFixture{
		runID: uuid.New(), reviewID: uuid.New(), acceptanceID: uuid.New(),
		runState:        run.StateCancelled,
		reviewState:     run.StageStateCancelled,
		acceptanceState: run.StageStatePending,
		closeFields: map[string]any{
			"pr_url": reopenTestPRURL, "closer": "alice", "head_sha": "aaa",
			"run_state_at_close": "running", "review_state_at_close": "awaiting_approval",
		},
		closeAge:   2 * time.Minute,
		reopenHead: "aaa",
	}
}

// seq10 is the anchoring close row's chain position; rows that must sit
// after it use a higher sequence, rows before it a lower one.
const reopenCloseSeq = 10

func (f *reopenFixture) entry(seq int64, category string, ts time.Time, fields map[string]any) *audit.Entry {
	rid := f.runID
	body, _ := json.Marshal(fields)
	return &audit.Entry{ID: uuid.New(), RunID: &rid, Sequence: seq, Timestamp: ts, Category: category, Payload: body}
}

// build wires a Server over the fixture.
func (f *reopenFixture) build(t *testing.T) (*Server, *prEventsRunRepo, *reopenAuditRepo) {
	t.Helper()
	prURL := reopenTestPRURL
	stages := []*run.Stage{
		{ID: uuid.New(), RunID: f.runID, Type: run.StageTypePlan, State: run.StageStateSucceeded},
		{ID: uuid.New(), RunID: f.runID, Type: run.StageTypeImplement, State: run.StageStateSucceeded},
	}
	if !f.noReviewStage {
		stages = append(stages, &run.Stage{ID: f.reviewID, RunID: f.runID, Type: run.StageTypeReview, State: f.reviewState})
	}
	stages = append(stages, &run.Stage{ID: f.acceptanceID, RunID: f.runID, Type: run.StageTypeAcceptance, State: f.acceptanceState})
	rr := &prEventsRunRepo{
		listResult: []*run.Run{{ID: f.runID, State: f.runState, PullRequestURL: &prURL}},
		stages:     map[uuid.UUID][]*run.Stage{f.runID: stages},
		stagesErr:  f.stagesErr,
		reviveErr:  f.reviveErr,
	}
	byCat := map[string][]*audit.Entry{}
	for c, es := range f.extra {
		byCat[c] = append(byCat[c], es...)
	}
	if f.closeFields != nil {
		closer := "alice"
		e := f.entry(reopenCloseSeq, CategoryPRClosedWithoutMerge, time.Now().UTC().Add(-f.closeAge), f.closeFields)
		e.ActorSubject = &closer
		byCat[CategoryPRClosedWithoutMerge] = append(byCat[CategoryPRClosedWithoutMerge], e)
	}
	ar := &reopenAuditRepo{
		prEventsAuditRepo: &prEventsAuditRepo{byRunCategory: map[uuid.UUID]map[string][]*audit.Entry{f.runID: byCat}},
		listErr:           f.listErr,
	}
	var repo run.Repository = rr
	if f.noReviver {
		repo = noReviverRunRepo{rr}
	}
	// The webhook secret + delivery store let webhook_test.go drive the same
	// fixture through the signed POST /webhooks/github route; direct handler
	// calls ignore them.
	return New(Config{
		Addr: "127.0.0.1:0", RunRepo: repo, AuditRepo: ar,
		GitHubWebhookSecret: []byte(testSecret), WebhookDeliveries: webhook.NewMemoryStore(0),
	}), rr, ar
}

func reopenPayload(head, sender string) []byte {
	b, _ := json.Marshal(map[string]any{
		"action": "reopened",
		"pull_request": map[string]any{
			"html_url": reopenTestPRURL, "number": 42, "head": map[string]any{"sha": head},
		},
		"sender": map[string]any{"login": sender},
	})
	return b
}

// appendedOf returns the appended rows of one category.
func appendedOf(ar *reopenAuditRepo, category string) []audit.ChainAppendParams {
	ar.mu.Lock()
	defer ar.mu.Unlock()
	var out []audit.ChainAppendParams
	for _, p := range ar.appended {
		if p.Category == category {
			out = append(out, p)
		}
	}
	return out
}

// TestPullRequestReopened_Guards is the per-failure-mode table: one row per
// refusal reason, each perturbing ONE input of the all-guards-pass baseline and
// asserting the run is left cancelled with its review cancelled, no
// run_revived_on_reopen row, and exactly one run_revive_on_reopen_refused row
// carrying that reason; plus the not-a-candidate rows (no audit at all) and the
// success rows.
func TestPullRequestReopened_Guards(t *testing.T) {
	const (
		outcomeRevived      = "revived"
		outcomeRefused      = "refused"
		outcomeNotCandidate = "not_candidate"
	)
	cases := []struct {
		name       string
		mutate     func(f *reopenFixture)
		outcome    string
		wantReason string
		// wantReviveCalls is the number of ReviveRunOnReopen calls; only the
		// success rows and revive_transition_refused reach the repository.
		wantReviveCalls int
	}{
		{name: "baseline_revives", mutate: func(*reopenFixture) {}, outcome: outcomeRevived, wantReviveCalls: 1},
		{name: "close_9_minutes_ago_revives", mutate: func(f *reopenFixture) { f.closeAge = 9 * time.Minute },
			outcome: outcomeRevived, wantReviveCalls: 1},
		{
			// (e) fallback: a poll-path close records no head, so the run's own
			// last reported head is the one compared.
			name: "poll_close_without_head_falls_back_to_reported_head",
			mutate: func(f *reopenFixture) {
				f.closeFields["head_sha"] = ""
				f.extra = map[string][]*audit.Entry{"pull_request_opened": {
					f.entry(3, "pull_request_opened", time.Now().UTC().Add(-time.Hour), map[string]any{"head_sha": "aaa"}),
				}}
			},
			outcome: outcomeRevived, wantReviveCalls: 1,
		},
		{
			// (g) boundary: a retirement-drop row strictly BEFORE the anchoring
			// close is not this cancel's row.
			name: "drop_row_before_close_ignored",
			mutate: func(f *reopenFixture) {
				f.extra = map[string][]*audit.Entry{CategoryAcceptanceScenarioRetirementDropped: {
					f.entry(reopenCloseSeq-1, CategoryAcceptanceScenarioRetirementDropped, time.Now().UTC().Add(-time.Hour),
						map[string]any{"cancel_source": cancelSourceStageCancelled}),
				}}
			},
			outcome: outcomeRevived, wantReviveCalls: 1,
		},
		{
			// (g) boundary: only the PR-close cancel's own drop row refuses.
			name: "drop_row_other_cancel_source_ignored",
			mutate: func(f *reopenFixture) {
				f.extra = map[string][]*audit.Entry{CategoryAcceptanceScenarioRetirementDropped: {
					f.entry(reopenCloseSeq+1, CategoryAcceptanceScenarioRetirementDropped, time.Now().UTC(),
						map[string]any{"cancel_source": cancelSourceOperator}),
				}}
			},
			outcome: outcomeRevived, wantReviveCalls: 1,
		},

		// --- not a candidate: no revive, no audit ---
		{name: "redelivery_run_already_running", mutate: func(f *reopenFixture) {
			f.runState, f.reviewState = run.StateRunning, run.StageStateAwaitingApproval
		}, outcome: outcomeNotCandidate},
		{
			// Isolates the run-state half of the candidate gate: the close
			// cancelled the review but the run is still running (the close's
			// Advance failed). Without the gate the guards all pass and the
			// repository refuses, appending a refused row.
			name: "run_running_review_cancelled",
			mutate: func(f *reopenFixture) {
				f.runState = run.StateRunning
			},
			outcome: outcomeNotCandidate,
		},
		{
			// Isolates the review-state half: a run cancelled apart from any
			// PR close keeps its review parked.
			name: "review_not_cancelled",
			mutate: func(f *reopenFixture) {
				f.reviewState = run.StageStateAwaitingApproval
				f.closeFields = nil
			},
			outcome: outcomeNotCandidate,
		},
		{name: "no_review_stage", mutate: func(f *reopenFixture) { f.noReviewStage = true }, outcome: outcomeNotCandidate},
		{name: "stage_list_unreadable", mutate: func(f *reopenFixture) { f.stagesErr = errors.New("db down") }, outcome: outcomeNotCandidate},

		// --- one refusal per guard ---
		{name: "a_audit_unreadable", mutate: func(f *reopenFixture) {
			f.listErr = map[string]error{CategoryPRClosedWithoutMerge: errors.New("db down")}
		}, outcome: outcomeRefused, wantReason: reopenRefusedAuditUnreadable},
		{name: "b_close_found_run_already_cancelled", mutate: func(f *reopenFixture) {
			f.closeFields["run_state_at_close"] = "cancelled"
		}, outcome: outcomeRefused, wantReason: reopenRefusedCloseDidNotCancelRun},
		{name: "b_legacy_close_row_without_field", mutate: func(f *reopenFixture) {
			delete(f.closeFields, "run_state_at_close")
		}, outcome: outcomeRefused, wantReason: reopenRefusedCloseDidNotCancelRun},
		{
			// The newest cancelling close was already revived: the run's
			// current cancel came from elsewhere (e.g. an operator cancel
			// after the revive, then a close that recorded run cancelled).
			name: "b_close_already_revived",
			mutate: func(f *reopenFixture) {
				f.extra = map[string][]*audit.Entry{CategoryRunRevivedOnReopen: {
					f.entry(reopenCloseSeq+1, CategoryRunRevivedOnReopen, time.Now().UTC().Add(-time.Minute), map[string]any{}),
				}}
			},
			outcome: outcomeRefused, wantReason: reopenRefusedCloseDidNotCancelRun,
		},
		{name: "c_review_not_parked_at_close", mutate: func(f *reopenFixture) {
			f.closeFields["review_state_at_close"] = "running"
		}, outcome: outcomeRefused, wantReason: reopenRefusedReviewNotParkedAtClose},
		{name: "d_window_elapsed_11_minutes", mutate: func(f *reopenFixture) {
			f.closeAge = 11 * time.Minute
		}, outcome: outcomeRefused, wantReason: reopenRefusedWindowElapsed},
		{
			// Both heads empty so (e) is the ONLY control in the path: with
			// it deleted the empty heads compare equal and (f) passes.
			name: "e_close_head_unrecorded",
			mutate: func(f *reopenFixture) {
				f.closeFields["head_sha"] = ""
				f.reopenHead = ""
			},
			outcome: outcomeRefused, wantReason: reopenRefusedCloseHeadUnrecorded,
		},
		{name: "f_head_changed", mutate: func(f *reopenFixture) { f.reopenHead = "bbb" },
			outcome: outcomeRefused, wantReason: reopenRefusedHeadChanged},
		{name: "g_acceptance_retirements_dropped", mutate: func(f *reopenFixture) {
			f.extra = map[string][]*audit.Entry{CategoryAcceptanceScenarioRetirementDropped: {
				f.entry(reopenCloseSeq+1, CategoryAcceptanceScenarioRetirementDropped, time.Now().UTC().Add(-time.Minute),
					map[string]any{"cancel_source": cancelSourceStageCancelled, "reason": acceptanceRetirementDropReasonRunCancelled}),
			}}
		}, outcome: outcomeRefused, wantReason: reopenRefusedAcceptanceRetirementsDropped},
		{name: "h_stage_in_flight", mutate: func(f *reopenFixture) { f.acceptanceState = run.StageStateRunning },
			outcome: outcomeRefused, wantReason: reopenRefusedStageInFlight},
		{name: "i_reviver_unavailable", mutate: func(f *reopenFixture) { f.noReviver = true },
			outcome: outcomeRefused, wantReason: reopenRefusedReviverUnavailable},
		{name: "j_revive_transition_refused", mutate: func(f *reopenFixture) {
			f.reviveErr = fmt.Errorf("%w: lock contention", run.ErrReopenNotApplicable)
		}, outcome: outcomeRefused, wantReason: reopenRefusedReviveTransitionRefused, wantReviveCalls: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := baselineReopenFixture()
			tc.mutate(f)
			s, rr, ar := f.build(t)

			s.handlePullRequestReopened(context.Background(), reopenPayload(f.reopenHead, "bob"))

			rr.mu.Lock()
			calls := len(rr.revives)
			runState := rr.listResult[0].State
			reviewState, touched := rr.curState[f.reviewID]
			rr.mu.Unlock()
			if !touched {
				reviewState = f.reviewState
			}
			if calls != tc.wantReviveCalls {
				t.Errorf("ReviveRunOnReopen calls = %d, want %d", calls, tc.wantReviveCalls)
			}
			revived := appendedOf(ar, CategoryRunRevivedOnReopen)
			refused := appendedOf(ar, CategoryRunReviveOnReopenRefused)

			switch tc.outcome {
			case outcomeRevived:
				if runState != run.StateRunning || reviewState != run.StageStateAwaitingApproval {
					t.Fatalf("run/review = %s/%s, want running/awaiting_approval", runState, reviewState)
				}
				if len(refused) != 0 || len(revived) != 1 {
					t.Fatalf("revived rows = %d, refused rows = %d; want 1 and 0", len(revived), len(refused))
				}
				row := revived[0]
				if row.StageID == nil || *row.StageID != f.reviewID {
					t.Errorf("revived row stage = %v, want the review stage %s", row.StageID, f.reviewID)
				}
				if row.ActorKind == nil || *row.ActorKind != audit.ActorKind("user") || row.ActorSubject == nil || *row.ActorSubject != "bob" {
					t.Errorf("revived row actor = %v/%v, want user/bob", row.ActorKind, row.ActorSubject)
				}
				var p map[string]any
				if err := json.Unmarshal(row.Payload, &p); err != nil {
					t.Fatalf("payload: %v", err)
				}
				if p["pr_url"] != reopenTestPRURL || p["reopened_by"] != "bob" || p["closed_by"] != "alice" ||
					p["head_sha"] != f.reopenHead || p["review_stage_id"] != f.reviewID.String() ||
					p["window_seconds"] != float64(600) || p["closed_at"] == "" {
					t.Errorf("revived payload = %s", row.Payload)
				}
				if el, _ := p["elapsed_seconds"].(float64); el < f.closeAge.Seconds()-1 || el > f.closeAge.Seconds()+60 {
					t.Errorf("elapsed_seconds = %v, want ~%v", p["elapsed_seconds"], f.closeAge.Seconds())
				}
			case outcomeRefused:
				if runState != run.StateCancelled || reviewState != run.StageStateCancelled {
					t.Fatalf("run/review = %s/%s, want the run left cancelled", runState, reviewState)
				}
				if len(revived) != 0 || len(refused) != 1 {
					t.Fatalf("revived rows = %d, refused rows = %d; want 0 and exactly 1", len(revived), len(refused))
				}
				var p map[string]any
				if err := json.Unmarshal(refused[0].Payload, &p); err != nil {
					t.Fatalf("payload: %v", err)
				}
				if p["reason"] != tc.wantReason {
					t.Fatalf("refused reason = %v (detail %v), want %q", p["reason"], p["detail"], tc.wantReason)
				}
				if p["detail"] == "" || p["reopened_by"] != "bob" || p["reopen_head_sha"] != f.reopenHead || p["pr_url"] != reopenTestPRURL {
					t.Errorf("refused payload = %s", refused[0].Payload)
				}
				if refused[0].StageID == nil || *refused[0].StageID != f.reviewID {
					t.Errorf("refused row stage = %v, want the review stage", refused[0].StageID)
				}
			case outcomeNotCandidate:
				if runState != f.runState {
					t.Errorf("run state = %s, want it untouched (%s)", runState, f.runState)
				}
				if len(revived) != 0 || len(refused) != 0 {
					t.Fatalf("revived rows = %d, refused rows = %d; a not-a-candidate reopen must record nothing", len(revived), len(refused))
				}
			}
		})
	}
}

// TestPullRequestReopened_InertInputs pins the early returns that precede the
// candidate gate: an unconfigured server, an unparseable body, a payload with
// no PR URL, and a PR no Fishhawk run owns all record nothing and never reach
// the repository's revive.
func TestPullRequestReopened_InertInputs(t *testing.T) {
	f := baselineReopenFixture()

	t.Run("unconfigured", func(t *testing.T) {
		New(Config{Addr: "127.0.0.1:0"}).handlePullRequestReopened(context.Background(), reopenPayload("aaa", "bob"))
	})
	for name, body := range map[string][]byte{
		"unparseable": []byte("{"),
		"no_pr_url":   []byte(`{"pull_request":{"head":{"sha":"aaa"}}}`),
	} {
		t.Run(name, func(t *testing.T) {
			s, rr, ar := f.build(t)
			s.handlePullRequestReopened(context.Background(), body)
			if len(rr.revives) != 0 || len(ar.appended) != 0 {
				t.Errorf("revives = %d, appended = %d; want nothing", len(rr.revives), len(ar.appended))
			}
		})
	}
	t.Run("unmanaged_pr", func(t *testing.T) {
		s, rr, ar := f.build(t)
		rr.listResult = nil
		s.handlePullRequestReopened(context.Background(), reopenPayload("aaa", "bob"))
		if len(rr.revives) != 0 || len(ar.appended) != 0 {
			t.Errorf("revives = %d, appended = %d; want nothing", len(rr.revives), len(ar.appended))
		}
	})
}

// TestAuditEntryBefore pins the chain-order helper the anchor selection and
// guards (b)/(g) use: sequence first, timestamp when sequences tie.
func TestAuditEntryBefore(t *testing.T) {
	now := time.Now()
	a := &audit.Entry{Sequence: 1, Timestamp: now}
	b := &audit.Entry{Sequence: 2, Timestamp: now.Add(-time.Hour)}
	if !auditEntryBefore(a, b) || auditEntryBefore(b, a) {
		t.Error("sequence must decide when the sequences differ")
	}
	c := &audit.Entry{Timestamp: now.Add(-time.Minute)}
	d := &audit.Entry{Timestamp: now}
	if !auditEntryBefore(c, d) || auditEntryBefore(d, c) || auditEntryBefore(d, d) {
		t.Error("timestamp must decide when the sequences tie, and an entry is not before itself")
	}
}
