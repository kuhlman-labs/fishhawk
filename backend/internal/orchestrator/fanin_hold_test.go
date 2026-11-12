package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/defaultbranch"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// #4846 R2: the read-only DecomposedParentFanInHold predicate and Advance's
// consolidateBeforeAcceptance share decideFanInAcceptance. This file pins that
// they AGREE arm by arm. It reuses fanin_acceptance_test.go's fakes and seed
// helpers (same package) and defines its own parity helpers here (C8).

// parityAnswer is one caller's normalized answer for a fixture: an error, a
// hold (acceptance still pending), or a release with the fail-open reason and
// detail the release reports (both empty on a verified release).
type parityAnswer struct {
	err    string
	held   bool
	reason string
	detail string
}

// predicateAnswer maps the predicate's FanInHold onto a parityAnswer. A
// non-parent (the zero value) is a release with no reason, which is what
// Advance does for it.
func predicateAnswer(h FanInHold, err error) parityAnswer {
	if err != nil {
		return parityAnswer{err: err.Error()}
	}
	if h.Held {
		return parityAnswer{held: true}
	}
	return parityAnswer{reason: h.UnavailableReason, detail: h.UnavailableDetail}
}

// advanceAnswer runs one Advance on the twin fixture and reads its COMMITTED
// effect: an error, acceptance still pending (held), or acceptance parked at
// awaiting_host_dispatch with at most one fan_in_verify_unavailable row naming
// the reason and detail.
func advanceAnswer(t *testing.T, o *Orchestrator, au *recordingAudit, p acceptanceAheadParent) parityAnswer {
	t.Helper()
	if _, err := o.Advance(context.Background(), p.run.ID); err != nil {
		return parityAnswer{err: err.Error()}
	}
	rows := unavailableRows(au)
	switch p.acceptance.State {
	case run.StageStatePending:
		if len(rows) != 0 {
			t.Errorf("Advance held but recorded %d fan_in_verify_unavailable rows: %+v", len(rows), rows)
		}
		return parityAnswer{held: true}
	case run.StageStateAwaitingHostDispatch:
	default:
		t.Fatalf("Advance left acceptance %q, want pending (held) or awaiting_host_dispatch (released)", p.acceptance.State)
	}
	if len(rows) > 1 {
		t.Fatalf("Advance released with %d fan_in_verify_unavailable rows, want at most 1: %+v", len(rows), rows)
	}
	if len(rows) == 0 {
		return parityAnswer{}
	}
	var payload FanInVerifyUnavailablePayload
	if err := json.Unmarshal(rows[0].Payload, &payload); err != nil {
		t.Fatalf("decode fan_in_verify_unavailable payload %s: %v", rows[0].Payload, err)
	}
	return parityAnswer{reason: payload.Reason, detail: payload.Detail}
}

// parityFixture is one side of a parity row: its own orchestrator, fakes and
// seeded parent, built identically for both callers.
type parityFixture struct {
	o    *Orchestrator
	rs   *stubRuns
	gh   *stubGitHub
	au   *recordingAudit
	hook *recordingFanInVerify
	p    acceptanceAheadParent
}

// (c2) TestDecomposedParentFanInHold_ParityWithAdvance walks every arm of
// consolidateBeforeAcceptance in order. For each it builds TWO identical
// fixtures, asks the read-only predicate on one and runs Advance on the other,
// and asserts the same answer: an error the Advance error carries, a hold
// (acceptance still pending after Advance), or a release (acceptance at
// awaiting_host_dispatch) with exactly the fail-open reason and detail the
// predicate reports, or none. The openable PR-less row is the one deliberate
// difference in mechanism, not in answer: the predicate holds it as
// consolidated_pr_not_opened without consulting the hook, while Advance opens
// the PR and then holds on the unsettled hook. MECHANISM: both callers share
// decideFanInAcceptance, so a reordered, dropped or diverging arm in either
// wrapper (the PR-open skip mirror, the parent detection, the effect mapping)
// shows up as a row mismatch.
func TestDecomposedParentFanInHold_ParityWithAdvance(t *testing.T) {
	cases := []struct {
		name    string
		answers []bool
		setup   func(t *testing.T, f *parityFixture)
		// want pins the shared answer, so a row cannot pass by both callers
		// drifting the same way.
		want parityAnswer
		// wantErr is a substring of the shared error.
		wantErr string
		// after asserts arm-specific effects on the Advance side.
		after func(t *testing.T, f *parityFixture)
	}{
		{
			name: "no implement stage",
			setup: func(t *testing.T, f *parityFixture) {
				r, stages := f.rs.seed(t, "kuhlman-labs/fishhawk", int64Ptr(55), []stageSeed{
					{Type: run.StageTypeAcceptance, ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code", State: run.StageStatePending},
				})
				r.RunnerKind = run.RunnerKindLocal
				r.RunnerKindResolved = true
				f.p = acceptanceAheadParent{run: r, acceptance: stages[0]}
			},
		},
		{
			name: "no children",
			setup: func(t *testing.T, f *parityFixture) {
				r, stages := f.rs.seed(t, "kuhlman-labs/fishhawk", int64Ptr(55), []stageSeed{
					{Type: run.StageTypeImplement, ExecutorKind: run.ExecutorAgent, State: run.StageStateSucceeded},
					{Type: run.StageTypeAcceptance, ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code", State: run.StageStatePending},
				})
				r.RunnerKind = run.RunnerKindLocal
				r.RunnerKindResolved = true
				f.p = acceptanceAheadParent{run: r, implement: stages[0], acceptance: stages[1]}
			},
		},
		{
			name:    "children-list error",
			setup:   func(_ *testing.T, f *parityFixture) { f.rs.listRunsErr = errors.New("list boom") },
			wantErr: "list boom",
		},
		{
			name:  "nil FanInVerify",
			setup: func(_ *testing.T, f *parityFixture) { f.o.FanInVerify = nil },
			want:  parityAnswer{reason: FanInVerifyUnavailableHookNotWired, detail: "FanInVerify not wired"},
		},
		{
			name:  "nil ConsolidatedReview",
			setup: func(_ *testing.T, f *parityFixture) { f.o.ConsolidatedReview = nil },
			want:  parityAnswer{reason: FanInVerifyUnavailableHookNotWired, detail: "ConsolidatedReview not wired"},
		},
		{
			name:    "openable PR-less parent, unsettled hook",
			answers: []bool{false},
			want:    parityAnswer{held: true},
			after: func(t *testing.T, f *parityFixture) {
				if len(f.gh.createPRCalls) != 1 || prURL(f.rs, f.p.run.ID) == "" {
					t.Errorf("Advance side: CreatePullRequest calls = %d, pull_request_url = %q; want the PR opened before the hold",
						len(f.gh.createPRCalls), prURL(f.rs, f.p.run.ID))
				}
			},
		},
		{
			name:  "no consolidated PR: github client nil",
			setup: func(_ *testing.T, f *parityFixture) { f.o.GitHub = nil },
			want:  parityAnswer{reason: FanInVerifyUnavailableNoConsolidatedPR, detail: "github client not wired"},
		},
		{
			name:  "no consolidated PR: nil installation id",
			setup: func(_ *testing.T, f *parityFixture) { f.p.run.InstallationID = nil },
			want:  parityAnswer{reason: FanInVerifyUnavailableNoConsolidatedPR, detail: "no installation id"},
		},
		{
			name:  "no consolidated PR: zero installation id",
			setup: func(_ *testing.T, f *parityFixture) { f.p.run.InstallationID = int64Ptr(0) },
			want:  parityAnswer{reason: FanInVerifyUnavailableNoConsolidatedPR, detail: "no installation id"},
		},
		{
			name: "unresolved default branch",
			setup: func(_ *testing.T, f *parityFixture) {
				f.o.DefaultBranch = defaultbranch.New(&countingRepoGetter{err: forge.ErrNotFound})
				stampPR(f.p)
			},
			want: parityAnswer{reason: FanInVerifyUnavailableDefaultBranchUnresolved, detail: string(defaultbranch.ReasonRepositoryNotVisible)},
		},
		{
			name: "hook error",
			setup: func(_ *testing.T, f *parityFixture) {
				f.hook.err = errors.New("compare boom")
				stampPR(f.p)
			},
			wantErr: "compare boom",
		},
		{
			name:    "unsettled",
			answers: []bool{false},
			setup:   func(_ *testing.T, f *parityFixture) { stampPR(f.p) },
			want:    parityAnswer{held: true},
		},
		{
			name: "settled: no_run_repo",
			setup: func(_ *testing.T, f *parityFixture) {
				f.hook.reason = FanInVerifyUnavailableNoRunRepo
				stampPR(f.p)
			},
			want: parityAnswer{reason: FanInVerifyUnavailableNoRunRepo},
		},
		{
			name: "settled: forge_not_wired",
			setup: func(_ *testing.T, f *parityFixture) {
				f.hook.reason, f.hook.detail = FanInVerifyUnavailableForgeNotWired, "github client not wired"
				stampPR(f.p)
			},
			want: parityAnswer{reason: FanInVerifyUnavailableForgeNotWired, detail: "github client not wired"},
		},
		{
			name:    "settled",
			answers: []bool{true},
			setup:   func(_ *testing.T, f *parityFixture) { stampPR(f.p) },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			build := func() *parityFixture {
				o, rs, gh, au, _, hook := newFanInAcceptanceOrchestrator(t, tc.answers...)
				hook.head = "consolhead"
				f := &parityFixture{o: o, rs: rs, gh: gh, au: au, hook: hook, p: seedAcceptanceAheadParent(t, rs, run.StageStateSucceeded)}
				if tc.setup != nil {
					tc.setup(t, f)
				}
				return f
			}
			pf, af := build(), build()

			pf.rs.mu.Lock()
			stages := append([]*run.Stage(nil), pf.rs.stages[pf.p.run.ID]...)
			pf.rs.mu.Unlock()
			pred := predicateAnswer(pf.o.DecomposedParentFanInHold(context.Background(), pf.p.run, stages))
			adv := advanceAnswer(t, af.o, af.au, af.p)

			if tc.wantErr != "" {
				if !strings.Contains(pred.err, tc.wantErr) {
					t.Errorf("predicate err = %q, want one naming %q", pred.err, tc.wantErr)
				}
				if !strings.Contains(adv.err, tc.wantErr) || !strings.Contains(adv.err, pred.err) {
					t.Errorf("Advance err = %q, want one carrying the predicate's error %q", adv.err, pred.err)
				}
				if af.p.acceptance.State != run.StageStatePending {
					t.Errorf("Advance side: acceptance state = %q, want pending on an error", af.p.acceptance.State)
				}
			} else {
				if pred != adv {
					t.Errorf("parity mismatch: predicate = %+v, Advance = %+v", pred, adv)
				}
				if pred != tc.want {
					t.Errorf("shared answer = %+v, want %+v", pred, tc.want)
				}
			}
			if tc.after != nil {
				tc.after(t, af)
			}
		})
	}
}
