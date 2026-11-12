package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/defaultbranch"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// #4785: a workflow that orders acceptance BEFORE review (feature_change)
// reaches a decomposed parent's acceptance stage first, so the ADR-032
// review-gate tail (consolidated PR, fan-in verify, consolidated review) must
// run ahead of acceptance. Every fake and fixture for these tests lives in this
// file (approval condition C6).

// fanInVerifyCall is one recorded FanInVerifySettled call, with the state the
// hook OBSERVED at call time so a test can assert ordering (PR opened, and
// acceptance still pending, BEFORE the hook is consulted).
type fanInVerifyCall struct {
	runID           uuid.UUID
	base, branch    string
	prURLAtCall     string
	acceptanceState run.StageState
}

// recordingFanInVerify is a FanInVerifyGate fake answering a scripted
// sequence of verdicts (the last one repeats) and recording each call.
type recordingFanInVerify struct {
	mu      sync.Mutex
	answers []bool
	err     error
	calls   []fanInVerifyCall

	// reason and detail, when set, ride every Settled=true answer as the
	// hook's UnavailableReason / UnavailableDetail (#4792): a scripted
	// fail-open release.
	reason, detail string

	// head, when set, rides every answer as the judged consolidated head
	// (#4846), settled or not.
	head string

	// observe, when set, snapshots the repo state at call time.
	runs       *stubRuns
	acceptance *run.Stage
}

func (f *recordingFanInVerify) FanInVerifySettled(_ context.Context, runID uuid.UUID, base, branch string) (FanInVerifyAnswer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := fanInVerifyCall{runID: runID, base: base, branch: branch}
	if f.runs != nil {
		f.runs.mu.Lock()
		if r := f.runs.runs[runID]; r != nil && r.PullRequestURL != nil {
			c.prURLAtCall = *r.PullRequestURL
		}
		f.runs.mu.Unlock()
	}
	if f.acceptance != nil {
		c.acceptanceState = f.acceptance.State
	}
	f.calls = append(f.calls, c)
	if f.err != nil {
		return FanInVerifyAnswer{}, f.err
	}
	settled := true
	if len(f.answers) > 0 {
		i := len(f.calls) - 1
		if i >= len(f.answers) {
			i = len(f.answers) - 1
		}
		settled = f.answers[i]
	}
	if !settled {
		return FanInVerifyAnswer{HeadSHA: f.head}, nil
	}
	return FanInVerifyAnswer{Settled: true, HeadSHA: f.head, UnavailableReason: f.reason, UnavailableDetail: f.detail}, nil
}

func (f *recordingFanInVerify) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// acceptanceAheadParent holds the stages of a feature_change-shaped decomposed
// parent: implement (seq 0), acceptance (seq 1), review (seq 2).
type acceptanceAheadParent struct {
	run        *run.Run
	implement  *run.Stage
	acceptance *run.Stage
	review     *run.Stage
}

// seedAcceptanceAheadParent seeds a runner_kind-local decomposed parent whose
// implement stage is in implState, with acceptance and review both pending
// and one succeeded child. The PR URL is left EMPTY, so the acceptance arm
// must open the consolidated PR itself.
func seedAcceptanceAheadParent(t *testing.T, rs *stubRuns, implState run.StageState) acceptanceAheadParent {
	t.Helper()
	parent, stages := rs.seed(t, "kuhlman-labs/fishhawk", int64Ptr(55), []stageSeed{
		{Type: run.StageTypeImplement, ExecutorKind: run.ExecutorAgent, State: implState},
		{Type: run.StageTypeAcceptance, ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code", State: run.StageStatePending},
		{Type: run.StageTypeReview, ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code", State: run.StageStatePending},
	})
	parent.RunnerKind = run.RunnerKindLocal
	parent.RunnerKindResolved = true
	seedSucceededSlice(t, rs, parent.ID, int64Ptr(55), 0)
	return acceptanceAheadParent{run: parent, implement: stages[0], acceptance: stages[1], review: stages[2]}
}

// newFanInAcceptanceOrchestrator wires the orchestrator with the recording
// audit, consolidated-review dispatcher and FanInVerifyGate fakes.
func newFanInAcceptanceOrchestrator(t *testing.T, answers ...bool) (*Orchestrator, *stubRuns, *stubGitHub, *recordingAudit, *recordingConsolidatedReview, *recordingFanInVerify) {
	t.Helper()
	o, rs, gh := newOrchestrator(t)
	o.DefaultBranch = defaultbranch.New(&countingRepoGetter{branch: "main"})
	au := &recordingAudit{}
	o.Audit = au
	rec := &recordingConsolidatedReview{}
	o.ConsolidatedReview = rec
	hook := &recordingFanInVerify{answers: answers, runs: rs}
	o.FanInVerify = hook
	return o, rs, gh, au, rec, hook
}

func prURL(rs *stubRuns, id uuid.UUID) string {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if r := rs.runs[id]; r != nil && r.PullRequestURL != nil {
		return *r.PullRequestURL
	}
	return ""
}

// unavailableRows returns every fan_in_verify_unavailable append (#4792).
func unavailableRows(au *recordingAudit) []audit.ChainAppendParams {
	au.mu.Lock()
	defer au.mu.Unlock()
	var out []audit.ChainAppendParams
	for _, e := range au.appended {
		if e.Category == CategoryFanInVerifyUnavailable {
			out = append(out, e)
		}
	}
	return out
}

// assertNoUnavailableRows pins a healthy path: no fail-open signal is recorded.
func assertNoUnavailableRows(t *testing.T, au *recordingAudit) {
	t.Helper()
	if rows := unavailableRows(au); len(rows) != 0 {
		t.Errorf("fan_in_verify_unavailable rows = %d, want 0 on a healthy path: %+v", len(rows), rows)
	}
}

// assertOneUnavailableRow pins EXACTLY one fan_in_verify_unavailable row with
// the reason, detail and the acceptance stage anchor.
func assertOneUnavailableRow(t *testing.T, au *recordingAudit, p acceptanceAheadParent, reason, detail string) {
	t.Helper()
	rows := unavailableRows(au)
	if len(rows) != 1 {
		t.Fatalf("fan_in_verify_unavailable rows = %d, want exactly 1: %+v", len(rows), rows)
	}
	row := rows[0]
	if row.StageID == nil || *row.StageID != p.acceptance.ID {
		t.Errorf("row stage anchor = %v, want the acceptance stage %s", row.StageID, p.acceptance.ID)
	}
	if row.ActorKind == nil || *row.ActorKind != audit.ActorSystem {
		t.Errorf("row actor = %v, want the system actor", row.ActorKind)
	}
	var got FanInVerifyUnavailablePayload
	if err := json.Unmarshal(row.Payload, &got); err != nil {
		t.Fatalf("decode payload %s: %v", row.Payload, err)
	}
	want := FanInVerifyUnavailablePayload{
		Reason:             reason,
		Detail:             detail,
		AcceptanceStageID:  p.acceptance.ID.String(),
		ConsolidatedBranch: consolidatedBranch(p.run.ID),
	}
	if got != want {
		t.Errorf("payload = %+v, want %+v", got, want)
	}
}

// jsonLogger routes the orchestrator's logger to a JSON handler so a test can
// assert on the WARN message and its attrs.
func jsonLogger(o *Orchestrator) *bytes.Buffer {
	buf := &bytes.Buffer{}
	o.Logger = slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	return buf
}

// assertUnavailableWarn pins the WARN naming fan_in_verify_unavailable with the
// reason, run id and acceptance stage id.
func assertUnavailableWarn(t *testing.T, logs *bytes.Buffer, p acceptanceAheadParent, reason string) {
	t.Helper()
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		if strings.Contains(line, `"level":"WARN"`) &&
			strings.Contains(line, "fan_in_verify_unavailable") &&
			strings.Contains(line, `"reason":"`+reason+`"`) &&
			strings.Contains(line, `"run_id":"`+p.run.ID.String()+`"`) &&
			strings.Contains(line, `"acceptance_stage_id":"`+p.acceptance.ID.String()+`"`) {
			return
		}
	}
	t.Errorf("no WARN naming fan_in_verify_unavailable, reason %q, run and acceptance stage; logs:\n%s", reason, logs.String())
}

// assertReleased pins that acceptance left pending on the local runner.
func assertReleased(t *testing.T, p acceptanceAheadParent) {
	t.Helper()
	if p.acceptance.State != run.StageStateAwaitingHostDispatch {
		t.Errorf("acceptance state = %q, want awaiting_host_dispatch (released: a signal, never a hold)", p.acceptance.State)
	}
}

// RELEASE case (hook reports SETTLED): the consolidated PR is opened BEFORE
// acceptance leaves pending, acceptance is then dispatched (parked at
// awaiting_host_dispatch on the local runner) and the consolidated review
// fires.
func TestAdvance_DecomposedParentAcceptance_OpensPRBeforeDispatch(t *testing.T) {
	o, rs, gh, au, rec, hook := newFanInAcceptanceOrchestrator(t, true)
	p := seedAcceptanceAheadParent(t, rs, run.StageStateSucceeded)
	hook.acceptance = p.acceptance

	out, err := o.Advance(context.Background(), p.run.ID)
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	assertNoUnavailableRows(t, au)
	if out != OutcomeDispatched {
		t.Errorf("Outcome = %q, want dispatched", out)
	}
	if len(gh.createPRCalls) != 1 {
		t.Fatalf("CreatePullRequest calls = %d, want 1", len(gh.createPRCalls))
	}
	if want := consolidatedBranch(p.run.ID); gh.createPRCalls[0].Head != want {
		t.Errorf("PR head = %q, want %q", gh.createPRCalls[0].Head, want)
	}
	if prURL(rs, p.run.ID) == "" {
		t.Error("parent pull_request_url not stamped")
	}
	if !auditHasCategory(au, "consolidated_pr_opened") {
		t.Errorf("audit categories = %v, want consolidated_pr_opened", au.appended)
	}
	for _, e := range au.appended {
		if e.Category == "consolidated_pr_opened" && (e.StageID == nil || *e.StageID != p.review.ID) {
			t.Errorf("consolidated_pr_opened stage anchor = %v, want the review stage %s", e.StageID, p.review.ID)
		}
	}
	if len(hook.calls) != 1 {
		t.Fatalf("FanInVerifySettled calls = %d, want 1", len(hook.calls))
	}
	c := hook.calls[0]
	if c.base != "main" || c.branch != consolidatedBranch(p.run.ID) {
		t.Errorf("hook called with (%q, %q), want (main, %q)", c.base, c.branch, consolidatedBranch(p.run.ID))
	}
	if c.prURLAtCall == "" {
		t.Error("hook consulted before the consolidated PR was opened")
	}
	if c.acceptanceState != run.StageStatePending {
		t.Errorf("acceptance state at hook call = %q, want pending (consulted before dispatch)", c.acceptanceState)
	}
	if p.acceptance.State != run.StageStateAwaitingHostDispatch {
		t.Errorf("acceptance state = %q, want awaiting_host_dispatch (released on the local runner)", p.acceptance.State)
	}
	if len(rec.calls) != 1 {
		t.Fatalf("DispatchConsolidatedReview calls = %d, want 1", len(rec.calls))
	}
	if rec.calls[0].base != "main" || rec.calls[0].head != consolidatedBranch(p.run.ID) {
		t.Errorf("DispatchConsolidatedReview(%q, %q), want (main, consolidated branch)", rec.calls[0].base, rec.calls[0].head)
	}
	if p.review.State != run.StageStatePending {
		t.Errorf("review state = %q, want pending (acceptance runs first)", p.review.State)
	}
}

// HOLD case (hook reports NOT settled): acceptance stays pending, the PR is
// opened exactly once (the fixture leaves the URL empty), and no review round
// runs. The ONE DispatchConsolidatedReview call is the verify-pass start: the
// server-side D5 hold starts the verify-only pass and holds the round
// (TestFanInAcceptanceHold_EndToEnd pins zero implement_review_started).
func TestAdvance_DecomposedParentAcceptance_HeldWhileFanInUnsettled(t *testing.T) {
	for _, implState := range []run.StageState{run.StageStateSucceeded, run.StageStateRunning} {
		t.Run(string(implState), func(t *testing.T) {
			o, rs, gh, au, rec, hook := newFanInAcceptanceOrchestrator(t, false)
			p := seedAcceptanceAheadParent(t, rs, implState)

			out, err := o.Advance(context.Background(), p.run.ID)
			if err != nil {
				t.Fatalf("Advance: %v", err)
			}
			assertNoUnavailableRows(t, au)
			if out != OutcomeNoOp {
				t.Errorf("Outcome = %q, want no-op (held)", out)
			}
			if p.acceptance.State != run.StageStatePending {
				t.Errorf("acceptance state = %q, want pending (held until the fan-in verify settles)", p.acceptance.State)
			}
			if len(gh.createPRCalls) != 1 {
				t.Errorf("CreatePullRequest calls = %d, want exactly 1", len(gh.createPRCalls))
			}
			if hook.callCount() != 1 {
				t.Errorf("FanInVerifySettled calls = %d, want 1", hook.callCount())
			}
			if len(rec.calls) != 1 {
				t.Errorf("DispatchConsolidatedReview calls = %d, want 1 (the verify-pass start the D5 hold holds)", len(rec.calls))
			}
			if p.review.State != run.StageStatePending {
				t.Errorf("review state = %q, want pending", p.review.State)
			}
			if p.implement.State != implState {
				t.Errorf("implement state = %q, want unchanged %q", p.implement.State, implState)
			}
			if n := o.fanInHolds[p.run.ID]; n != 1 {
				t.Errorf("hold attempt count = %d, want 1", n)
			}
		})
	}
}

// A hook error fails closed: Advance returns it and acceptance stays pending.
func TestAdvance_DecomposedParentAcceptance_HookErrorHolds(t *testing.T) {
	o, rs, _, _, rec, hook := newFanInAcceptanceOrchestrator(t)
	hook.err = errors.New("compare boom")
	p := seedAcceptanceAheadParent(t, rs, run.StageStateSucceeded)

	if _, err := o.Advance(context.Background(), p.run.ID); err == nil {
		t.Fatal("Advance returned nil error on a hook error")
	}
	if p.acceptance.State != run.StageStatePending {
		t.Errorf("acceptance state = %q, want pending", p.acceptance.State)
	}
	if len(rec.calls) != 0 {
		t.Errorf("DispatchConsolidatedReview calls = %d, want 0 on a hook error", len(rec.calls))
	}
}

// A PR-open error is returned so the next Advance retries; acceptance is not
// dispatched and the hook is not consulted.
func TestAdvance_DecomposedParentAcceptance_PROpenErrorHolds(t *testing.T) {
	o, rs, gh, _, rec, hook := newFanInAcceptanceOrchestrator(t, true)
	gh.createPRErr = errors.New("forge boom")
	p := seedAcceptanceAheadParent(t, rs, run.StageStateSucceeded)

	if _, err := o.Advance(context.Background(), p.run.ID); err == nil {
		t.Fatal("Advance returned nil error on a PR-open error")
	}
	if p.acceptance.State != run.StageStatePending {
		t.Errorf("acceptance state = %q, want pending", p.acceptance.State)
	}
	if hook.callCount() != 0 || len(rec.calls) != 0 {
		t.Errorf("hook calls = %d, review fires = %d, want 0/0 on a PR-open error", hook.callCount(), len(rec.calls))
	}
}

// A children-list error is returned before anything is opened or dispatched.
func TestAdvance_DecomposedParentAcceptance_ListChildrenErrorHolds(t *testing.T) {
	o, rs, gh, _, _, hook := newFanInAcceptanceOrchestrator(t, true)
	p := seedAcceptanceAheadParent(t, rs, run.StageStateSucceeded)
	rs.listRunsErr = errors.New("list boom")

	if _, err := o.Advance(context.Background(), p.run.ID); err == nil {
		t.Fatal("Advance returned nil error on a children-list error")
	}
	if p.acceptance.State != run.StageStatePending {
		t.Errorf("acceptance state = %q, want pending", p.acceptance.State)
	}
	if len(gh.createPRCalls) != 0 || hook.callCount() != 0 {
		t.Errorf("creates = %d, hook calls = %d, want 0/0", len(gh.createPRCalls), hook.callCount())
	}
}

// The hold releases on the Advance after the hook turns settled (the
// merge_candidate_verified report's Advance in production): acceptance is
// dispatched, the PR is not re-opened, and the hold counter counted the
// repeated hold and is cleared on release.
func TestAdvance_DecomposedParentAcceptance_ReleaseAfterSettled(t *testing.T) {
	o, rs, gh, au, rec, _ := newFanInAcceptanceOrchestrator(t, false, false, true)
	p := seedAcceptanceAheadParent(t, rs, run.StageStateSucceeded)

	for i := 0; i < 2; i++ {
		if _, err := o.Advance(context.Background(), p.run.ID); err != nil {
			t.Fatalf("Advance #%d: %v", i+1, err)
		}
		if p.acceptance.State != run.StageStatePending {
			t.Fatalf("after held Advance #%d: acceptance state = %q, want pending", i+1, p.acceptance.State)
		}
	}
	if n := o.fanInHolds[p.run.ID]; n != 2 {
		t.Errorf("hold attempt count after two holds = %d, want 2 (a repeated hold is distinguishable)", n)
	}

	out, err := o.Advance(context.Background(), p.run.ID)
	if err != nil {
		t.Fatalf("release Advance: %v", err)
	}
	if out != OutcomeDispatched {
		t.Errorf("release Outcome = %q, want dispatched", out)
	}
	if p.acceptance.State != run.StageStateAwaitingHostDispatch {
		t.Errorf("acceptance state = %q, want awaiting_host_dispatch after release", p.acceptance.State)
	}
	if len(gh.createPRCalls) != 1 {
		t.Errorf("CreatePullRequest calls = %d, want still 1", len(gh.createPRCalls))
	}
	if len(rec.calls) != 3 {
		t.Errorf("DispatchConsolidatedReview calls = %d, want 3 (two verify-pass holds + the release fire)", len(rec.calls))
	}
	if _, ok := o.fanInHolds[p.run.ID]; ok {
		t.Error("hold attempt count not cleared on release")
	}
	assertNoUnavailableRows(t, au)
}

// A nil hook (CLI/dev posture) still opens the consolidated PR before
// acceptance, dispatches acceptance and fires the consolidated review, and the
// release is RECORDED as hook_not_wired (#4792): the CLI/dev posture expects
// it, so the row is a signal and not a fault. A nil ConsolidatedReview or both
// nil name themselves in the detail.
func TestAdvance_DecomposedParentAcceptance_NilHook_DispatchesWithPR(t *testing.T) {
	cases := []struct {
		name       string
		nilHook    bool
		nilReview  bool
		wantDetail string
		wantFires  int
	}{
		{name: "nil FanInVerify", nilHook: true, wantDetail: "FanInVerify not wired", wantFires: 1},
		{name: "nil ConsolidatedReview", nilReview: true, wantDetail: "ConsolidatedReview not wired"},
		{name: "both nil", nilHook: true, nilReview: true, wantDetail: "FanInVerify and ConsolidatedReview not wired"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o, rs, gh, au, rec, hook := newFanInAcceptanceOrchestrator(t)
			logs := jsonLogger(o)
			if tc.nilHook {
				o.FanInVerify = nil
			}
			if tc.nilReview {
				o.ConsolidatedReview = nil
			}
			p := seedAcceptanceAheadParent(t, rs, run.StageStateSucceeded)

			if _, err := o.Advance(context.Background(), p.run.ID); err != nil {
				t.Fatalf("Advance: %v", err)
			}
			if len(gh.createPRCalls) != 1 {
				t.Errorf("CreatePullRequest calls = %d, want 1", len(gh.createPRCalls))
			}
			assertReleased(t, p)
			if len(rec.calls) != tc.wantFires {
				t.Errorf("DispatchConsolidatedReview calls = %d, want %d", len(rec.calls), tc.wantFires)
			}
			if !tc.nilHook && hook.callCount() != 0 {
				t.Errorf("FanInVerifySettled calls = %d, want 0 (the review dispatcher is not wired)", hook.callCount())
			}
			assertOneUnavailableRow(t, au, p, FanInVerifyUnavailableHookNotWired, tc.wantDetail)
			assertUnavailableWarn(t, logs, p, FanInVerifyUnavailableHookNotWired)
		})
	}
}

// The graceful PR-open skip (o.GitHub nil, or no installation id) leaves the
// parent PR-less, so the hook is skipped altogether. That is the most common
// real fail-open posture (#4792): it records no_consolidated_pr, names which
// skip fired, consults no hook and still releases acceptance.
func TestAdvance_DecomposedParentAcceptance_NoConsolidatedPR_RecordsUnavailable(t *testing.T) {
	cases := []struct {
		name       string
		setup      func(o *Orchestrator, p acceptanceAheadParent)
		wantDetail string
	}{
		{name: "github client nil", setup: func(o *Orchestrator, _ acceptanceAheadParent) { o.GitHub = nil }, wantDetail: "github client not wired"},
		{name: "nil installation id", setup: func(_ *Orchestrator, p acceptanceAheadParent) { p.run.InstallationID = nil }, wantDetail: "no installation id"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o, rs, gh, au, rec, hook := newFanInAcceptanceOrchestrator(t, false)
			logs := jsonLogger(o)
			p := seedAcceptanceAheadParent(t, rs, run.StageStateSucceeded)
			tc.setup(o, p)

			if _, err := o.Advance(context.Background(), p.run.ID); err != nil {
				t.Fatalf("Advance: %v", err)
			}
			if prURL(rs, p.run.ID) != "" {
				t.Errorf("pull_request_url = %q, want empty (the open skipped gracefully)", prURL(rs, p.run.ID))
			}
			if len(gh.createPRCalls) != 0 {
				t.Errorf("CreatePullRequest calls = %d, want 0", len(gh.createPRCalls))
			}
			if hook.callCount() != 0 {
				t.Errorf("FanInVerifySettled calls = %d, want 0 with no consolidated PR", hook.callCount())
			}
			if len(rec.calls) != 0 {
				t.Errorf("DispatchConsolidatedReview calls = %d, want 0 with no consolidated PR", len(rec.calls))
			}
			assertReleased(t, p)
			assertOneUnavailableRow(t, au, p, FanInVerifyUnavailableNoConsolidatedPR, tc.wantDetail)
			assertUnavailableWarn(t, logs, p, FanInVerifyUnavailableNoConsolidatedPR)
		})
	}
}

// The hook's own fail-open answer (Settled=true plus an UnavailableReason) is
// recorded verbatim: the release still happens and the consolidated review
// still fires, but the reason and detail reach the chain and the log (#4792).
func TestAdvance_DecomposedParentAcceptance_HookUnavailable_RecordsReason(t *testing.T) {
	cases := []struct {
		name, reason, detail string
	}{
		{name: "no_run_repo", reason: FanInVerifyUnavailableNoRunRepo},
		{name: "forge_not_wired", reason: FanInVerifyUnavailableForgeNotWired, detail: "github client not wired"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o, rs, _, au, rec, hook := newFanInAcceptanceOrchestrator(t, true)
			hook.reason, hook.detail = tc.reason, tc.detail
			logs := jsonLogger(o)
			p := seedAcceptanceAheadParent(t, rs, run.StageStateSucceeded)

			if _, err := o.Advance(context.Background(), p.run.ID); err != nil {
				t.Fatalf("Advance: %v", err)
			}
			if hook.callCount() != 1 {
				t.Errorf("FanInVerifySettled calls = %d, want 1", hook.callCount())
			}
			assertReleased(t, p)
			if len(rec.calls) != 1 {
				t.Errorf("DispatchConsolidatedReview calls = %d, want 1 (the release fire is unchanged)", len(rec.calls))
			}
			assertOneUnavailableRow(t, au, p, tc.reason, tc.detail)
			assertUnavailableWarn(t, logs, p, tc.reason)
		})
	}
}

// UNRESOLVED default branch (#4660): with the consolidated PR already
// stamped, a base the resolver cannot read applies NO hold and never guesses a
// literal. The hook is not consulted, acceptance dispatches as before, the
// consolidated-review fire skips, and a WARN names the reason with the run id.
func TestAdvance_DecomposedParentAcceptance_UnresolvedDefaultBranchDispatchesWithoutHold(t *testing.T) {
	o, rs, gh, au, rec, hook := newFanInAcceptanceOrchestrator(t, false)
	o.DefaultBranch = defaultbranch.New(&countingRepoGetter{err: forge.ErrNotFound})
	var logBuf bytes.Buffer
	o.Logger = slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	p := seedAcceptanceAheadParent(t, rs, run.StageStateSucceeded)
	pr := "https://github.com/kuhlman-labs/fishhawk/pull/9"
	p.run.PullRequestURL = &pr

	out, err := o.Advance(context.Background(), p.run.ID)
	if err != nil {
		t.Fatalf("Advance: %v (an unresolved base must not fail the advance)", err)
	}
	if out != OutcomeDispatched {
		t.Errorf("Outcome = %q, want dispatched", out)
	}
	if hook.callCount() != 0 {
		t.Errorf("FanInVerifySettled calls = %d, want 0 on an unresolved base", hook.callCount())
	}
	if p.acceptance.State != run.StageStateAwaitingHostDispatch {
		t.Errorf("acceptance state = %q, want awaiting_host_dispatch (no hold on an unresolved base)", p.acceptance.State)
	}
	if len(rec.calls) != 0 {
		t.Errorf("DispatchConsolidatedReview calls = %+v, want none on an unresolved base", rec.calls)
	}
	if len(gh.createPRCalls) != 0 {
		t.Errorf("CreatePullRequest calls = %d, want 0 (PR already stamped)", len(gh.createPRCalls))
	}
	var warned bool
	for _, line := range strings.Split(strings.TrimSpace(logBuf.String()), "\n") {
		if strings.Contains(line, `"level":"WARN"`) &&
			strings.Contains(line, "acceptance hold not applied: default branch unresolved") &&
			strings.Contains(line, `"reason":"`+string(defaultbranch.ReasonRepositoryNotVisible)+`"`) &&
			strings.Contains(line, `"run_id":"`+p.run.ID.String()+`"`) {
			warned = true
		}
	}
	if !warned {
		t.Errorf("no WARN naming the unresolved reason and run_id; logs:\n%s", logBuf.String())
	}
	// #4792: the release is also recorded, with the resolver's closed-vocabulary
	// reason as the detail.
	assertOneUnavailableRow(t, au, p, FanInVerifyUnavailableDefaultBranchUnresolved, string(defaultbranch.ReasonRepositoryNotVisible))
	assertUnavailableWarn(t, &logBuf, p, FanInVerifyUnavailableDefaultBranchUnresolved)
}

// An ordinary (non-decomposed) run's acceptance is untouched: no PR, no hook,
// no consolidated review, acceptance dispatched as before.
func TestAdvance_DecomposedParentAcceptance_NonDecomposedRunUntouched(t *testing.T) {
	o, rs, gh, au, rec, hook := newFanInAcceptanceOrchestrator(t, false)
	r, stages := rs.seed(t, "kuhlman-labs/fishhawk", int64Ptr(55), []stageSeed{
		{Type: run.StageTypeImplement, ExecutorKind: run.ExecutorAgent, State: run.StageStateSucceeded},
		{Type: run.StageTypeAcceptance, ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code", State: run.StageStatePending},
		{Type: run.StageTypeReview, ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code", State: run.StageStatePending},
	})
	r.RunnerKind = run.RunnerKindLocal
	r.RunnerKindResolved = true

	if _, err := o.Advance(context.Background(), r.ID); err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if len(gh.createPRCalls) != 0 {
		t.Errorf("CreatePullRequest calls = %d, want 0", len(gh.createPRCalls))
	}
	if hook.callCount() != 0 {
		t.Errorf("FanInVerifySettled calls = %d, want 0", hook.callCount())
	}
	if len(rec.calls) != 0 {
		t.Errorf("DispatchConsolidatedReview calls = %d, want 0", len(rec.calls))
	}
	if stages[1].State != run.StageStateAwaitingHostDispatch {
		t.Errorf("acceptance state = %q, want awaiting_host_dispatch (dispatched as before)", stages[1].State)
	}
	assertNoUnavailableRows(t, au)
}

// A run with no implement stage can never be a decomposed parent (only an
// implement stage fans out), so the arm does not even list children: a
// ListRuns failure cannot block its acceptance dispatch.
func TestAdvance_DecomposedParentAcceptance_NoImplementStageSkipsChildLookup(t *testing.T) {
	o, rs, gh, _, rec, hook := newFanInAcceptanceOrchestrator(t, false)
	r, stages := rs.seed(t, "kuhlman-labs/fishhawk", int64Ptr(55), []stageSeed{
		{Type: run.StageTypeAcceptance, ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code", State: run.StageStatePending},
	})
	r.RunnerKind = run.RunnerKindLocal
	r.RunnerKindResolved = true
	rs.listRunsErr = errors.New("ListRuns must not be called")

	if _, err := o.Advance(context.Background(), r.ID); err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if stages[0].State != run.StageStateAwaitingHostDispatch {
		t.Errorf("acceptance state = %q, want awaiting_host_dispatch", stages[0].State)
	}
	if len(gh.createPRCalls) != 0 || hook.callCount() != 0 || len(rec.calls) != 0 {
		t.Errorf("creates = %d, hook calls = %d, review fires = %d, want 0/0/0", len(gh.createPRCalls), hook.callCount(), len(rec.calls))
	}
}

// The issue's exact sequence (#4785): an awaiting_children parent with
// acceptance ahead of review, a recoverable failed child that parks it, then
// the re-driven child succeeds. The settle's Advance must open the PR and
// consult the hook BEFORE acceptance leaves pending.
func TestMaybeAdvanceDecomposedParent_RedriveSettle_OpensPRBeforeAcceptance(t *testing.T) {
	o, rs, gh, au, rec, hook := newFanInAcceptanceOrchestrator(t, true)
	gh.branchSHAs = map[string]string{"main": "basesha"} // fan-in creates the consolidated branch
	parent, stages := rs.seed(t, "kuhlman-labs/fishhawk", int64Ptr(55), []stageSeed{
		{Type: run.StageTypeImplement, ExecutorKind: run.ExecutorAgent, State: run.StageStateAwaitingChildren},
		{Type: run.StageTypeAcceptance, ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code", State: run.StageStatePending},
		{Type: run.StageTypeReview, ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code", State: run.StageStatePending},
	})
	parent.RunnerKind = run.RunnerKindLocal
	parent.RunnerKindResolved = true
	hook.acceptance = stages[1]

	cat := run.FailureC
	reason := "runner OOM"
	child, childStages := rs.seed(t, "kuhlman-labs/fishhawk", int64Ptr(55), []stageSeed{
		{Type: run.StageTypeImplement, ExecutorKind: run.ExecutorAgent, State: run.StageStateFailed},
	})
	child.DecomposedFrom = &parent.ID
	idx := 0
	child.SliceIndex = &idx
	child.State = run.StateFailed
	childStages[0].FailureCategory = &cat
	childStages[0].FailureReason = &reason

	o.maybeAdvanceDecomposedParent(context.Background(), parent.ID)
	if stages[0].State != run.StageStateAwaitingChildren {
		t.Fatalf("parent implement = %q, want awaiting_children (parked awaiting re-drive)", stages[0].State)
	}
	if !auditHasCategory(au, "parent_awaiting_redrive") {
		t.Fatal("want parent_awaiting_redrive after the recoverable child failure")
	}

	// The re-drive succeeds the child.
	childStages[0].State = run.StageStateSucceeded
	childStages[0].FailureCategory = nil
	childStages[0].FailureReason = nil
	child.State = run.StateSucceeded

	o.maybeAdvanceDecomposedParent(context.Background(), parent.ID)

	if stages[0].State != run.StageStateSucceeded {
		t.Fatalf("parent implement = %q, want succeeded", stages[0].State)
	}
	if len(gh.createPRCalls) != 1 {
		t.Fatalf("CreatePullRequest calls = %d, want 1", len(gh.createPRCalls))
	}
	if len(hook.calls) != 1 {
		t.Fatalf("FanInVerifySettled calls = %d, want 1", len(hook.calls))
	}
	if hook.calls[0].prURLAtCall == "" {
		t.Error("hook consulted before the consolidated PR was opened")
	}
	if hook.calls[0].acceptanceState != run.StageStatePending {
		t.Errorf("acceptance state at hook call = %q, want pending", hook.calls[0].acceptanceState)
	}
	if stages[1].State != run.StageStateAwaitingHostDispatch {
		t.Errorf("acceptance state = %q, want awaiting_host_dispatch after the settled hook", stages[1].State)
	}
	if len(rec.calls) != 1 {
		t.Errorf("DispatchConsolidatedReview calls = %d, want 1", len(rec.calls))
	}
}

// --- #4792: the recorder's failure modes ------------------------------------

func newRecorderFixture(t *testing.T) (*Orchestrator, acceptanceAheadParent, *recordingAudit, *bytes.Buffer) {
	t.Helper()
	o, rs, _, au, _, _ := newFanInAcceptanceOrchestrator(t)
	logs := jsonLogger(o)
	p := seedAcceptanceAheadParent(t, rs, run.StageStateSucceeded)
	return o, p, au, logs
}

// A repeat of the same (acceptance stage, reason) adds no row, a different
// reason adds one, and an undecodable existing row never suppresses the append.
func TestRecordFanInVerifyUnavailable_Dedupes(t *testing.T) {
	ctx := context.Background()
	o, p, au, _ := newRecorderFixture(t)

	o.recordFanInVerifyUnavailable(ctx, p.run, p.acceptance, FanInVerifyUnavailableHookNotWired, "FanInVerify not wired")
	o.recordFanInVerifyUnavailable(ctx, p.run, p.acceptance, FanInVerifyUnavailableHookNotWired, "FanInVerify not wired")
	if n := len(unavailableRows(au)); n != 1 {
		t.Fatalf("rows after a repeat of the same stage and reason = %d, want 1", n)
	}

	o.recordFanInVerifyUnavailable(ctx, p.run, p.acceptance, FanInVerifyUnavailableNoConsolidatedPR, "github client not wired")
	if n := len(unavailableRows(au)); n != 2 {
		t.Fatalf("rows after a different reason = %d, want 2", n)
	}

	// The same reason on a DIFFERENT acceptance stage is not a repeat.
	other := *p.acceptance
	other.ID = uuid.New()
	o.recordFanInVerifyUnavailable(ctx, p.run, &other, FanInVerifyUnavailableHookNotWired, "FanInVerify not wired")
	if n := len(unavailableRows(au)); n != 3 {
		t.Fatalf("rows after the same reason on another stage = %d, want 3", n)
	}

	// An undecodable existing row is no match. The malformed payload names the
	// SAME stage and reason but carries a wrong-typed detail: encoding/json
	// still fills reason and acceptance_stage_id while reporting the type error,
	// so only the decode-error guard (not the stage/reason comparison) keeps this
	// row from suppressing the append.
	for name, payload := range map[string]func(stageID uuid.UUID) string{
		"truncated json": func(uuid.UUID) string { return `{"reason":"hook_not_wired"` },
		"wrong-typed field": func(id uuid.UUID) string {
			return `{"reason":"hook_not_wired","acceptance_stage_id":"` + id.String() + `","detail":5}`
		},
		"not json at all": func(uuid.UUID) string { return "not json" },
	} {
		t.Run("undecodable existing row does not suppress: "+name, func(t *testing.T) {
			o, p, au, _ := newRecorderFixture(t)
			stageID := p.acceptance.ID
			au.appended = append(au.appended, audit.ChainAppendParams{
				RunID: p.run.ID, StageID: &stageID, Category: CategoryFanInVerifyUnavailable, Payload: []byte(payload(stageID)),
			})
			o.recordFanInVerifyUnavailable(ctx, p.run, p.acceptance, FanInVerifyUnavailableHookNotWired, "FanInVerify not wired")
			if n := len(unavailableRows(au)); n != 2 {
				t.Errorf("rows = %d, want 2 (the malformed row plus the new one)", n)
			}
		})
	}
}

// A dedupe read failure WARNs and still appends: a read failure never loses the
// signal.
func TestRecordFanInVerifyUnavailable_ListErrorStillAppends(t *testing.T) {
	o, p, au, logs := newRecorderFixture(t)
	o.Audit = &erroringCategoryAudit{recordingAudit: au}

	o.recordFanInVerifyUnavailable(context.Background(), p.run, p.acceptance, FanInVerifyUnavailableHookNotWired, "FanInVerify not wired")

	assertOneUnavailableRow(t, au, p, FanInVerifyUnavailableHookNotWired, "FanInVerify not wired")
	if !strings.Contains(logs.String(), "list fan_in_verify_unavailable rows failed") {
		t.Errorf("no WARN for the failed dedupe read; logs:\n%s", logs.String())
	}
}

// An append failure WARNs and never unwinds the release.
func TestRecordFanInVerifyUnavailable_AppendErrorDoesNotBlockRelease(t *testing.T) {
	o, rs, _, au, _, _ := newFanInAcceptanceOrchestrator(t)
	o.FanInVerify = nil
	o.Audit = &failingAppendAudit{recordingAudit: au}
	logs := jsonLogger(o)
	p := seedAcceptanceAheadParent(t, rs, run.StageStateSucceeded)

	if _, err := o.Advance(context.Background(), p.run.ID); err != nil {
		t.Fatalf("Advance: %v (a failed append must not fail the release)", err)
	}
	assertReleased(t, p)
	if rows := unavailableRows(au); len(rows) != 0 {
		t.Errorf("rows = %d, want 0 (the append failed)", len(rows))
	}
	if !strings.Contains(logs.String(), "append fan_in_verify_unavailable failed") {
		t.Errorf("no WARN for the failed append; logs:\n%s", logs.String())
	}
	assertUnavailableWarn(t, logs, p, FanInVerifyUnavailableHookNotWired)
}

// A nil Audit logs the WARN only: no panic, and the release is unchanged.
func TestRecordFanInVerifyUnavailable_NilAuditLogsOnly(t *testing.T) {
	o, rs, _, _, _, _ := newFanInAcceptanceOrchestrator(t)
	o.FanInVerify = nil
	o.Audit = nil
	logs := jsonLogger(o)
	p := seedAcceptanceAheadParent(t, rs, run.StageStateSucceeded)

	if _, err := o.Advance(context.Background(), p.run.ID); err != nil {
		t.Fatalf("Advance: %v", err)
	}
	assertReleased(t, p)
	assertUnavailableWarn(t, logs, p, FanInVerifyUnavailableHookNotWired)
}

// --- #4846: the hold runs BEFORE the acceptance short-circuit --------------

// fanInAllSkipCriteria is an approved plan's acceptance criteria where every
// criterion is skip_expected with a basis, so the all-skip-with-basis
// short-circuit predicate matches.
func fanInAllSkipCriteria() []map[string]any {
	return []map[string]any{
		{"id": "webhook-fires", "statement": "webhook fires on close", "source": "inferred", "rationale": "external", "skip_expected": true, "expectation_basis": "validated in webhook_integration_test.go with a fake"},
		{"id": "issue-closes", "statement": "issue auto-closes", "source": "inferred", "rationale": "external", "skip_expected": true, "expectation_basis": "validated in closer_e2e_test.go"},
	}
}

// fanInDrivableCriteria is one drivable criterion: no short-circuit predicate
// matches, so the stage needs a live target.
func fanInDrivableCriteria() []map[string]any {
	return []map[string]any{{"id": "ac-1", "statement": "POST returns 201", "source": "explicit", "source_ref": "#1", "blocking": true}}
}

// seedPlannedAcceptanceAheadParent seeds a feature_change-shaped decomposed
// parent with a succeeded plan stage carrying an approved standard_v1 plan
// (planBytes), then implement (succeeded), acceptance (acceptanceState) and
// review (pending), one succeeded child, and no PR URL. It wires o.Artifacts.
func seedPlannedAcceptanceAheadParent(t *testing.T, o *Orchestrator, rs *stubRuns, planBytes []byte, acceptanceState run.StageState) acceptanceAheadParent {
	t.Helper()
	parent, stages := rs.seed(t, "kuhlman-labs/fishhawk", int64Ptr(55), []stageSeed{
		{Type: run.StageTypePlan, ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code", State: run.StageStateSucceeded},
		{Type: run.StageTypeImplement, ExecutorKind: run.ExecutorAgent, State: run.StageStateSucceeded},
		{Type: run.StageTypeAcceptance, ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code", State: acceptanceState},
		{Type: run.StageTypeReview, ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code", State: run.StageStatePending},
	})
	parent.RunnerKind = run.RunnerKindLocal
	parent.RunnerKindResolved = true
	seedSucceededSlice(t, rs, parent.ID, int64Ptr(55), 0)
	schemaV := "standard_v1"
	o.Artifacts = &fakeArtifacts{byStage: map[uuid.UUID][]*artifact.Artifact{
		stages[0].ID: {{
			ID:            uuid.New(),
			StageID:       stages[0].ID,
			Kind:          artifact.KindPlan,
			SchemaVersion: &schemaV,
			Content:       planBytes,
			CreatedAt:     time.Now().UTC(),
		}},
	}}
	return acceptanceAheadParent{run: parent, implement: stages[1], acceptance: stages[2], review: stages[3]}
}

// stampPR gives the parent a consolidated PR URL, so the read-only predicate
// reaches the hook instead of holding consolidated_pr_not_opened.
func stampPR(p acceptanceAheadParent) {
	pr := "https://github.com/kuhlman-labs/fishhawk/pull/9"
	p.run.PullRequestURL = &pr
}

// appendIndex returns the index of the first append of category, or -1.
func appendIndex(au *recordingAudit, category string) int {
	au.mu.Lock()
	defer au.mu.Unlock()
	for i, e := range au.appended {
		if e.Category == category {
			return i
		}
	}
	return -1
}

// acceptanceRows returns every acceptance verdict and out-of-scope marker
// append, with the index each landed at.
func acceptanceRows(au *recordingAudit) (idx []int, rows []audit.ChainAppendParams) {
	au.mu.Lock()
	defer au.mu.Unlock()
	for i, e := range au.appended {
		if e.Category == "acceptance_outcome_recorded" || e.Category == "acceptance_skipped_out_of_scope" {
			idx = append(idx, i)
			rows = append(rows, e)
		}
	}
	return idx, rows
}

func decodePayload(t *testing.T, row audit.ChainAppendParams) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(row.Payload, &m); err != nil {
		t.Fatalf("decode %s payload %s: %v", row.Category, row.Payload, err)
	}
	return m
}

// (a) An approved plan that satisfies a short-circuit predicate no longer
// records its verdict before the fan-in verify settles: Advance #1 holds with
// zero verdict rows, and Advance #2 (settled) records exactly one, after the
// consolidated PR opened, carrying the judged head. MECHANISM: under the old
// ordering Advance #1 wrote the verdict before consulting the hook (run
// 9e17e66a, sequence 150794).
func TestAdvance_DecomposedParentAcceptance_ShortCircuitHeldUntilFanInSettles(t *testing.T) {
	cases := []struct {
		name         string
		outOfScope   []string
		criteria     []map[string]any
		wantCategory string
		wantBasis    string
	}{
		{name: "all_skip_with_basis", criteria: fanInAllSkipCriteria(), wantCategory: "acceptance_outcome_recorded", wantBasis: plan.AcceptanceBasisAllSkipWithBasis},
		{name: "empty_criteria", wantCategory: "acceptance_outcome_recorded", wantBasis: plan.AcceptanceBasisEmptyCriteria},
		{name: "out_of_scope_skip", outOfScope: []string{"deploy deferred"}, wantCategory: "acceptance_skipped_out_of_scope"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o, rs, _, au, rec, hook := newFanInAcceptanceOrchestrator(t, false, true)
			hook.head = "consolhead"
			p := seedPlannedAcceptanceAheadParent(t, o, rs, acceptanceSkipPlanBytes(t, tc.outOfScope, tc.criteria), run.StageStatePending)

			out, err := o.Advance(context.Background(), p.run.ID)
			if err != nil {
				t.Fatalf("Advance #1: %v", err)
			}
			if out != OutcomeNoOp {
				t.Errorf("Advance #1 outcome = %q, want no-op (held)", out)
			}
			if p.acceptance.State != run.StageStatePending {
				t.Errorf("acceptance after Advance #1 = %q, want pending (held until the fan-in verify settles)", p.acceptance.State)
			}
			if idx, _ := acceptanceRows(au); len(idx) != 0 {
				t.Fatalf("acceptance verdict/marker rows after the held Advance #1 = %d, want 0 (the short-circuit ran before the fan-in hold)", len(idx))
			}
			if !auditHasCategory(au, "consolidated_pr_opened") {
				t.Error("consolidated_pr_opened not recorded on Advance #1")
			}
			if hook.callCount() != 1 {
				t.Errorf("hook calls after Advance #1 = %d, want 1", hook.callCount())
			}
			if len(rec.calls) != 1 {
				t.Errorf("DispatchConsolidatedReview calls after Advance #1 = %d, want 1 (the verify-pass start)", len(rec.calls))
			}

			if _, err := o.Advance(context.Background(), p.run.ID); err != nil {
				t.Fatalf("Advance #2: %v", err)
			}
			if p.acceptance.State != run.StageStateSucceeded {
				t.Errorf("acceptance after Advance #2 = %q, want succeeded (short-circuited once released)", p.acceptance.State)
			}
			idx, rows := acceptanceRows(au)
			if len(rows) != 1 {
				t.Fatalf("acceptance verdict/marker rows after Advance #2 = %d, want exactly 1", len(rows))
			}
			if prIdx := appendIndex(au, "consolidated_pr_opened"); idx[0] < prIdx {
				t.Errorf("acceptance row at append %d precedes consolidated_pr_opened at %d", idx[0], prIdx)
			}
			if rows[0].Category != tc.wantCategory {
				t.Fatalf("row category = %q, want %q", rows[0].Category, tc.wantCategory)
			}
			got := decodePayload(t, rows[0])
			if tc.wantCategory == "acceptance_skipped_out_of_scope" {
				if _, ok := got["head_sha"]; ok {
					t.Errorf("out-of-scope marker carries head_sha: %s", rows[0].Payload)
				}
				return
			}
			if got["verdict"] != plan.AcceptanceVerdictNotValidated || got[plan.AcceptanceBasisKey] != tc.wantBasis {
				t.Errorf("verdict payload = %s, want verdict %s basis %s", rows[0].Payload, plan.AcceptanceVerdictNotValidated, tc.wantBasis)
			}
			if got["head_sha"] != "consolhead" {
				t.Errorf("verdict head_sha = %v, want consolhead (the hook-judged consolidated head): %s", got["head_sha"], rows[0].Payload)
			}
		})
	}
}

// (b) A fail-open release (nil hook) short-circuits on the first Advance: the
// verdict has no head_sha key, and the hook_not_wired row precedes it.
func TestAdvance_DecomposedParentAcceptance_ShortCircuitFailOpenRelease(t *testing.T) {
	o, rs, _, au, _, _ := newFanInAcceptanceOrchestrator(t)
	o.FanInVerify = nil
	p := seedPlannedAcceptanceAheadParent(t, o, rs, acceptanceSkipPlanBytes(t, nil, fanInAllSkipCriteria()), run.StageStatePending)

	if _, err := o.Advance(context.Background(), p.run.ID); err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if p.acceptance.State != run.StageStateSucceeded {
		t.Errorf("acceptance = %q, want succeeded (fail-open release short-circuits)", p.acceptance.State)
	}
	assertOneUnavailableRow(t, au, p, FanInVerifyUnavailableHookNotWired, "FanInVerify not wired")
	idx, rows := acceptanceRows(au)
	if len(rows) != 1 || rows[0].Category != "acceptance_outcome_recorded" {
		t.Fatalf("acceptance rows = %+v, want exactly one acceptance_outcome_recorded", rows)
	}
	if u := appendIndex(au, CategoryFanInVerifyUnavailable); u < 0 || u > idx[0] {
		t.Errorf("fan_in_verify_unavailable at append %d, verdict at %d: want the row BEFORE the verdict", u, idx[0])
	}
	if _, ok := decodePayload(t, rows[0])["head_sha"]; ok {
		t.Errorf("fail-open verdict carries head_sha: %s", rows[0].Payload)
	}
}

// (c) The read-only predicate's arms, one row each. Every row also asserts
// ZERO audit appends, ZERO PR creates and ZERO consolidated-review fires: the
// predicate decides and never acts.
func TestDecomposedParentFanInHold_Arms(t *testing.T) {
	branchOf := func(p acceptanceAheadParent) string { return consolidatedBranch(p.run.ID) }
	cases := []struct {
		name      string
		answers   []bool
		setup     func(o *Orchestrator, rs *stubRuns, hook *recordingFanInVerify, p acceptanceAheadParent) acceptanceAheadParent
		want      func(p acceptanceAheadParent) FanInHold
		wantErr   string
		wantCalls int
	}{
		{
			name: "child run",
			setup: func(_ *Orchestrator, rs *stubRuns, _ *recordingFanInVerify, p acceptanceAheadParent) acceptanceAheadParent {
				other := uuid.New()
				p.run.DecomposedFrom = &other
				rs.listRunsErr = errors.New("ListRuns must not be called for a child")
				return p
			},
			want: func(acceptanceAheadParent) FanInHold { return FanInHold{} },
		},
		{
			name: "no implement stage",
			setup: func(_ *Orchestrator, rs *stubRuns, _ *recordingFanInVerify, p acceptanceAheadParent) acceptanceAheadParent {
				r, stages := rs.seed(t, "kuhlman-labs/fishhawk", int64Ptr(55), []stageSeed{
					{Type: run.StageTypeAcceptance, ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code", State: run.StageStatePending},
				})
				rs.listRunsErr = errors.New("ListRuns must not be called without an implement stage")
				return acceptanceAheadParent{run: r, acceptance: stages[0]}
			},
			want: func(acceptanceAheadParent) FanInHold { return FanInHold{} },
		},
		{
			name: "no children",
			setup: func(_ *Orchestrator, rs *stubRuns, _ *recordingFanInVerify, _ acceptanceAheadParent) acceptanceAheadParent {
				r, stages := rs.seed(t, "kuhlman-labs/fishhawk", int64Ptr(55), []stageSeed{
					{Type: run.StageTypeImplement, ExecutorKind: run.ExecutorAgent, State: run.StageStateSucceeded},
					{Type: run.StageTypeAcceptance, ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code", State: run.StageStatePending},
				})
				return acceptanceAheadParent{run: r, implement: stages[0], acceptance: stages[1]}
			},
			want: func(acceptanceAheadParent) FanInHold { return FanInHold{} },
		},
		{
			name: "children-list error",
			setup: func(_ *Orchestrator, rs *stubRuns, _ *recordingFanInVerify, p acceptanceAheadParent) acceptanceAheadParent {
				rs.listRunsErr = errors.New("list boom")
				return p
			},
			wantErr: "list boom",
		},
		{
			name: "nil FanInVerify",
			setup: func(o *Orchestrator, _ *stubRuns, _ *recordingFanInVerify, p acceptanceAheadParent) acceptanceAheadParent {
				o.FanInVerify = nil
				return p
			},
			want: func(p acceptanceAheadParent) FanInHold {
				return FanInHold{Parent: true, ConsolidatedBranch: branchOf(p), UnavailableReason: FanInVerifyUnavailableHookNotWired, UnavailableDetail: "FanInVerify not wired"}
			},
		},
		{
			name: "nil ConsolidatedReview",
			setup: func(o *Orchestrator, _ *stubRuns, _ *recordingFanInVerify, p acceptanceAheadParent) acceptanceAheadParent {
				o.ConsolidatedReview = nil
				return p
			},
			want: func(p acceptanceAheadParent) FanInHold {
				return FanInHold{Parent: true, ConsolidatedBranch: branchOf(p), UnavailableReason: FanInVerifyUnavailableHookNotWired, UnavailableDetail: "ConsolidatedReview not wired"}
			},
		},
		{
			name: "both nil",
			setup: func(o *Orchestrator, _ *stubRuns, _ *recordingFanInVerify, p acceptanceAheadParent) acceptanceAheadParent {
				o.FanInVerify, o.ConsolidatedReview = nil, nil
				return p
			},
			want: func(p acceptanceAheadParent) FanInHold {
				return FanInHold{Parent: true, ConsolidatedBranch: branchOf(p), UnavailableReason: FanInVerifyUnavailableHookNotWired, UnavailableDetail: "FanInVerify and ConsolidatedReview not wired"}
			},
		},
		{
			name: "nil URL, nil GitHub",
			setup: func(o *Orchestrator, _ *stubRuns, _ *recordingFanInVerify, p acceptanceAheadParent) acceptanceAheadParent {
				o.GitHub = nil
				p.run.PullRequestURL = nil
				return p
			},
			want: func(p acceptanceAheadParent) FanInHold {
				return FanInHold{Parent: true, ConsolidatedBranch: branchOf(p), UnavailableReason: FanInVerifyUnavailableNoConsolidatedPR, UnavailableDetail: "github client not wired"}
			},
		},
		{
			name: "empty URL, nil installation id",
			setup: func(_ *Orchestrator, _ *stubRuns, _ *recordingFanInVerify, p acceptanceAheadParent) acceptanceAheadParent {
				empty := ""
				p.run.PullRequestURL = &empty
				p.run.InstallationID = nil
				return p
			},
			want: func(p acceptanceAheadParent) FanInHold {
				return FanInHold{Parent: true, ConsolidatedBranch: branchOf(p), UnavailableReason: FanInVerifyUnavailableNoConsolidatedPR, UnavailableDetail: "no installation id"}
			},
		},
		{
			name: "empty URL, zero installation id",
			setup: func(_ *Orchestrator, _ *stubRuns, _ *recordingFanInVerify, p acceptanceAheadParent) acceptanceAheadParent {
				empty := ""
				p.run.PullRequestURL = &empty
				p.run.InstallationID = int64Ptr(0)
				return p
			},
			want: func(p acceptanceAheadParent) FanInHold {
				return FanInHold{Parent: true, ConsolidatedBranch: branchOf(p), UnavailableReason: FanInVerifyUnavailableNoConsolidatedPR, UnavailableDetail: "no installation id"}
			},
		},
		{
			name: "openable PR-less parent, nil URL",
			setup: func(_ *Orchestrator, _ *stubRuns, _ *recordingFanInVerify, p acceptanceAheadParent) acceptanceAheadParent {
				p.run.PullRequestURL = nil
				return p
			},
			want: func(p acceptanceAheadParent) FanInHold {
				return FanInHold{Parent: true, Held: true, HoldCause: FanInHoldCauseConsolidatedPRNotOpened, ConsolidatedBranch: branchOf(p)}
			},
		},
		{
			name: "openable PR-less parent, empty URL",
			setup: func(_ *Orchestrator, _ *stubRuns, _ *recordingFanInVerify, p acceptanceAheadParent) acceptanceAheadParent {
				empty := ""
				p.run.PullRequestURL = &empty
				return p
			},
			want: func(p acceptanceAheadParent) FanInHold {
				return FanInHold{Parent: true, Held: true, HoldCause: FanInHoldCauseConsolidatedPRNotOpened, ConsolidatedBranch: branchOf(p)}
			},
		},
		{
			name: "unresolved default branch",
			setup: func(o *Orchestrator, _ *stubRuns, _ *recordingFanInVerify, p acceptanceAheadParent) acceptanceAheadParent {
				o.DefaultBranch = defaultbranch.New(&countingRepoGetter{err: forge.ErrNotFound})
				stampPR(p)
				return p
			},
			want: func(p acceptanceAheadParent) FanInHold {
				return FanInHold{Parent: true, ConsolidatedBranch: branchOf(p), UnavailableReason: FanInVerifyUnavailableDefaultBranchUnresolved, UnavailableDetail: string(defaultbranch.ReasonRepositoryNotVisible)}
			},
		},
		{
			name: "hook error",
			setup: func(_ *Orchestrator, _ *stubRuns, hook *recordingFanInVerify, p acceptanceAheadParent) acceptanceAheadParent {
				hook.err = errors.New("compare boom")
				stampPR(p)
				return p
			},
			wantErr:   "compare boom",
			wantCalls: 1,
		},
		{
			name: "no_run_repo",
			setup: func(_ *Orchestrator, _ *stubRuns, hook *recordingFanInVerify, p acceptanceAheadParent) acceptanceAheadParent {
				hook.reason = FanInVerifyUnavailableNoRunRepo
				stampPR(p)
				return p
			},
			want: func(p acceptanceAheadParent) FanInHold {
				return FanInHold{Parent: true, ConsolidatedBranch: branchOf(p), UnavailableReason: FanInVerifyUnavailableNoRunRepo}
			},
			wantCalls: 1,
		},
		{
			name: "forge_not_wired",
			setup: func(_ *Orchestrator, _ *stubRuns, hook *recordingFanInVerify, p acceptanceAheadParent) acceptanceAheadParent {
				hook.reason, hook.detail = FanInVerifyUnavailableForgeNotWired, "github client not wired"
				stampPR(p)
				return p
			},
			want: func(p acceptanceAheadParent) FanInHold {
				return FanInHold{Parent: true, ConsolidatedBranch: branchOf(p), UnavailableReason: FanInVerifyUnavailableForgeNotWired, UnavailableDetail: "github client not wired"}
			},
			wantCalls: 1,
		},
		{
			name:    "unsettled",
			answers: []bool{false},
			setup: func(_ *Orchestrator, _ *stubRuns, _ *recordingFanInVerify, p acceptanceAheadParent) acceptanceAheadParent {
				stampPR(p)
				return p
			},
			want: func(p acceptanceAheadParent) FanInHold {
				return FanInHold{Parent: true, Held: true, HoldCause: FanInHoldCauseUnsettled, HeadSHA: "consolhead", ConsolidatedBranch: branchOf(p)}
			},
			wantCalls: 1,
		},
		{
			name:    "settled",
			answers: []bool{true},
			setup: func(_ *Orchestrator, _ *stubRuns, _ *recordingFanInVerify, p acceptanceAheadParent) acceptanceAheadParent {
				stampPR(p)
				return p
			},
			want: func(p acceptanceAheadParent) FanInHold {
				return FanInHold{Parent: true, HeadSHA: "consolhead", ConsolidatedBranch: branchOf(p)}
			},
			wantCalls: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o, rs, gh, au, rec, hook := newFanInAcceptanceOrchestrator(t, tc.answers...)
			hook.head = "consolhead"
			p := tc.setup(o, rs, hook, seedAcceptanceAheadParent(t, rs, run.StageStateSucceeded))
			rs.mu.Lock()
			stages := append([]*run.Stage(nil), rs.stages[p.run.ID]...)
			rs.mu.Unlock()

			got, err := o.DecomposedParentFanInHold(context.Background(), p.run, stages)
			switch {
			case tc.wantErr != "":
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want one naming %q", err, tc.wantErr)
				}
				if got != (FanInHold{}) {
					t.Errorf("answer on error = %+v, want the zero value", got)
				}
			case err != nil:
				t.Fatalf("DecomposedParentFanInHold: %v", err)
			default:
				if want := tc.want(p); got != want {
					t.Errorf("answer = %+v, want %+v", got, want)
				}
			}
			if hook.callCount() != tc.wantCalls {
				t.Errorf("hook calls = %d, want %d", hook.callCount(), tc.wantCalls)
			}
			au.mu.Lock()
			appends := len(au.appended)
			au.mu.Unlock()
			if appends != 0 {
				t.Errorf("audit appends = %d, want 0 (the predicate is read-only)", appends)
			}
			if len(gh.createPRCalls) != 0 {
				t.Errorf("CreatePullRequest calls = %d, want 0 (the predicate never opens the PR)", len(gh.createPRCalls))
			}
			if len(rec.calls) != 0 {
				t.Errorf("DispatchConsolidatedReview calls = %d, want 0 (the predicate fires nothing)", len(rec.calls))
			}
			if _, ok := o.fanInHolds[p.run.ID]; ok {
				t.Error("hold counter touched by the read-only predicate")
			}
		})
	}
}

// RecordFanInHoldRelease records only a parent's fail-open release: a held
// answer, a verified release and a non-parent append nothing.
func TestRecordFanInHoldRelease_RecordsOnlyFailOpenRelease(t *testing.T) {
	cases := []struct {
		name     string
		hold     FanInHold
		wantRows int
	}{
		{name: "fail-open release", hold: FanInHold{Parent: true, UnavailableReason: FanInVerifyUnavailableHookNotWired, UnavailableDetail: "FanInVerify not wired"}, wantRows: 1},
		{name: "not a parent", hold: FanInHold{UnavailableReason: FanInVerifyUnavailableHookNotWired}},
		{name: "held", hold: FanInHold{Parent: true, Held: true, HoldCause: FanInHoldCauseUnsettled, UnavailableReason: FanInVerifyUnavailableHookNotWired}},
		{name: "verified release", hold: FanInHold{Parent: true, HeadSHA: "consolhead"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o, p, au, _ := newRecorderFixture(t)
			o.RecordFanInHoldRelease(context.Background(), p.run, p.acceptance, tc.hold)
			if tc.wantRows == 1 {
				assertOneUnavailableRow(t, au, p, FanInVerifyUnavailableHookNotWired, "FanInVerify not wired")
				return
			}
			if n := len(unavailableRows(au)); n != 0 {
				t.Errorf("rows = %d, want 0", n)
			}
		})
	}
}

// (d) The admission walk consults the shared predicate on a PENDING stage: an
// unsettled hook leaves it pending with no verdict, a settled one
// short-circuits with the judged head, a hook error mutates nothing, and an
// awaiting_host_dispatch stage skips the predicate (R1). MECHANISM: the plan is
// all-skip, so without the hold check the walk settles the pending stage while
// the hook answers unsettled.
func TestTryShortCircuitAcceptance_DecomposedParentFanInHold(t *testing.T) {
	allSkip := func(t *testing.T) []byte { return acceptanceSkipPlanBytes(t, nil, fanInAllSkipCriteria()) }

	t.Run("pending + unsettled holds", func(t *testing.T) {
		o, rs, _, au, _, hook := newFanInAcceptanceOrchestrator(t, false)
		hook.head = "consolhead"
		p := seedPlannedAcceptanceAheadParent(t, o, rs, allSkip(t), run.StageStatePending)
		stampPR(p)

		sc, liveReq, err := o.TryShortCircuitAcceptance(context.Background(), p.run.ID, p.acceptance.ID)
		if err != nil || sc != nil || liveReq {
			t.Fatalf("TryShortCircuitAcceptance = (%+v, %v, %v), want (nil, false, nil) while held", sc, liveReq, err)
		}
		if p.acceptance.State != run.StageStatePending {
			t.Errorf("acceptance = %q, want pending (held)", p.acceptance.State)
		}
		if idx, _ := acceptanceRows(au); len(idx) != 0 {
			t.Errorf("acceptance rows = %d, want 0 while held", len(idx))
		}
		if hook.callCount() != 1 {
			t.Errorf("hook calls = %d, want 1", hook.callCount())
		}
	})

	t.Run("pending + settled short-circuits with head_sha", func(t *testing.T) {
		o, rs, _, au, _, hook := newFanInAcceptanceOrchestrator(t, true)
		hook.head = "consolhead"
		p := seedPlannedAcceptanceAheadParent(t, o, rs, allSkip(t), run.StageStatePending)
		stampPR(p)

		sc, _, err := o.TryShortCircuitAcceptance(context.Background(), p.run.ID, p.acceptance.ID)
		if err != nil || sc == nil {
			t.Fatalf("TryShortCircuitAcceptance = (%+v, %v), want a short-circuit", sc, err)
		}
		if p.acceptance.State != run.StageStateSucceeded {
			t.Errorf("acceptance = %q, want succeeded", p.acceptance.State)
		}
		_, rows := acceptanceRows(au)
		if len(rows) != 1 {
			t.Fatalf("acceptance rows = %d, want 1", len(rows))
		}
		if got := decodePayload(t, rows[0])["head_sha"]; got != "consolhead" {
			t.Errorf("verdict head_sha = %v, want consolhead: %s", got, rows[0].Payload)
		}
		assertNoUnavailableRows(t, au)
	})

	t.Run("hook error mutates nothing", func(t *testing.T) {
		o, rs, _, au, _, hook := newFanInAcceptanceOrchestrator(t)
		hook.err = errors.New("compare boom")
		p := seedPlannedAcceptanceAheadParent(t, o, rs, allSkip(t), run.StageStatePending)
		stampPR(p)

		sc, _, err := o.TryShortCircuitAcceptance(context.Background(), p.run.ID, p.acceptance.ID)
		if err == nil || !strings.Contains(err.Error(), "compare boom") || sc != nil {
			t.Fatalf("TryShortCircuitAcceptance = (%+v, %v), want the hook error", sc, err)
		}
		if p.acceptance.State != run.StageStatePending {
			t.Errorf("acceptance = %q, want pending (untouched)", p.acceptance.State)
		}
		for _, tr := range rs.stageTransitions {
			if tr.StageID == p.acceptance.ID {
				t.Errorf("acceptance transitioned to %q on a hook error", tr.To)
			}
		}
		au.mu.Lock()
		appends := len(au.appended)
		au.mu.Unlock()
		if appends != 0 {
			t.Errorf("audit appends = %d, want 0 on a hook error", appends)
		}
	})

	t.Run("awaiting_host_dispatch skips the predicate (R1)", func(t *testing.T) {
		o, rs, _, au, _, hook := newFanInAcceptanceOrchestrator(t, false)
		hook.head = "consolhead"
		p := seedPlannedAcceptanceAheadParent(t, o, rs, allSkip(t), run.StageStateAwaitingHostDispatch)
		stampPR(p)

		sc, _, err := o.TryShortCircuitAcceptance(context.Background(), p.run.ID, p.acceptance.ID)
		if err != nil || sc == nil {
			t.Fatalf("TryShortCircuitAcceptance = (%+v, %v), want a short-circuit from awaiting_host_dispatch", sc, err)
		}
		if hook.callCount() != 0 {
			t.Errorf("hook calls = %d, want 0 (awaiting_host_dispatch skips the predicate)", hook.callCount())
		}
		_, rows := acceptanceRows(au)
		if len(rows) != 1 {
			t.Fatalf("acceptance rows = %d, want 1", len(rows))
		}
		if _, ok := decodePayload(t, rows[0])["head_sha"]; ok {
			t.Errorf("verdict carries head_sha without the predicate: %s", rows[0].Payload)
		}
	})
}

// failingCategoryAudit is a recordingAudit whose AppendChained fails for ONE
// category only, so the fan_in_verify_unavailable append can fail while the
// verdict still lands.
type failingCategoryAudit struct {
	*recordingAudit
	failCategory string
}

func (f *failingCategoryAudit) AppendChained(ctx context.Context, p audit.ChainAppendParams) (*audit.Entry, error) {
	if p.Category == f.failCategory {
		return nil, errors.New("append boom")
	}
	return f.recordingAudit.AppendChained(ctx, p)
}

// (d2) The admission walk records a fail-open release only when a predicate
// matched, before the verdict (R3/R9). MECHANISM: the re-entry Advance finds
// acceptance terminal and skips consolidateBeforeAcceptance, so without the
// admission-side record no unavailable row exists at all; the non_skippable
// fixture matches no predicate, so an unconditional record would append a row.
func TestTryShortCircuitAcceptance_DecomposedParentFailOpen_RecordsUnavailableBeforeVerdict(t *testing.T) {
	t.Run("skippable", func(t *testing.T) {
		o, rs, _, au, _, _ := newFanInAcceptanceOrchestrator(t)
		o.FanInVerify = nil
		p := seedPlannedAcceptanceAheadParent(t, o, rs, acceptanceSkipPlanBytes(t, nil, fanInAllSkipCriteria()), run.StageStatePending)

		sc, _, err := o.TryShortCircuitAcceptance(context.Background(), p.run.ID, p.acceptance.ID)
		if err != nil || sc == nil {
			t.Fatalf("TryShortCircuitAcceptance = (%+v, %v), want a short-circuit", sc, err)
		}
		assertOneUnavailableRow(t, au, p, FanInVerifyUnavailableHookNotWired, "FanInVerify not wired")
		idx, rows := acceptanceRows(au)
		if len(rows) != 1 || rows[0].Category != "acceptance_outcome_recorded" {
			t.Fatalf("acceptance rows = %+v, want one acceptance_outcome_recorded", rows)
		}
		if u := appendIndex(au, CategoryFanInVerifyUnavailable); u < 0 || u > idx[0] {
			t.Errorf("fan_in_verify_unavailable at append %d, verdict at %d: want the row BEFORE the verdict", u, idx[0])
		}
		if _, ok := decodePayload(t, rows[0])["head_sha"]; ok {
			t.Errorf("fail-open verdict carries head_sha: %s", rows[0].Payload)
		}

		if _, _, err := o.TryShortCircuitAcceptance(context.Background(), p.run.ID, p.acceptance.ID); err != nil {
			t.Fatalf("second TryShortCircuitAcceptance: %v", err)
		}
		if n := len(unavailableRows(au)); n != 1 {
			t.Errorf("rows after a second call = %d, want still 1", n)
		}
	})

	t.Run("non_skippable (R9)", func(t *testing.T) {
		o, rs, _, au, _, _ := newFanInAcceptanceOrchestrator(t)
		o.FanInVerify = nil
		p := seedPlannedAcceptanceAheadParent(t, o, rs, acceptanceSkipPlanBytes(t, nil, fanInDrivableCriteria()), run.StageStatePending)

		sc, liveReq, err := o.TryShortCircuitAcceptance(context.Background(), p.run.ID, p.acceptance.ID)
		if err != nil || sc != nil || !liveReq {
			t.Fatalf("TryShortCircuitAcceptance = (%+v, %v, %v), want (nil, true, nil)", sc, liveReq, err)
		}
		if p.acceptance.State != run.StageStatePending {
			t.Errorf("acceptance = %q, want pending", p.acceptance.State)
		}
		if n := len(unavailableRows(au)); n != 0 {
			t.Errorf("fan_in_verify_unavailable rows = %d, want 0 (no predicate matched, so nothing was released here)", n)
		}
		if idx, _ := acceptanceRows(au); len(idx) != 0 {
			t.Errorf("acceptance rows = %d, want 0", len(idx))
		}
	})

	// C6: the row-before-verdict order holds only when the append succeeds. A
	// failed append WARNs and is swallowed (#4792); the verdict still lands.
	t.Run("unavailable append fails", func(t *testing.T) {
		o, rs, _, au, _, _ := newFanInAcceptanceOrchestrator(t)
		o.FanInVerify = nil
		o.Audit = &failingCategoryAudit{recordingAudit: au, failCategory: CategoryFanInVerifyUnavailable}
		logs := jsonLogger(o)
		p := seedPlannedAcceptanceAheadParent(t, o, rs, acceptanceSkipPlanBytes(t, nil, fanInAllSkipCriteria()), run.StageStatePending)

		sc, _, err := o.TryShortCircuitAcceptance(context.Background(), p.run.ID, p.acceptance.ID)
		if err != nil || sc == nil {
			t.Fatalf("TryShortCircuitAcceptance = (%+v, %v), want a short-circuit despite the failed append", sc, err)
		}
		if p.acceptance.State != run.StageStateSucceeded {
			t.Errorf("acceptance = %q, want succeeded", p.acceptance.State)
		}
		if n := countAcceptanceCategory(au, "acceptance_outcome_recorded"); n != 1 {
			t.Errorf("acceptance_outcome_recorded = %d, want 1 (the verdict is still recorded)", n)
		}
		if n := len(unavailableRows(au)); n != 0 {
			t.Errorf("fan_in_verify_unavailable rows = %d, want 0 (the append failed)", n)
		}
		if !strings.Contains(logs.String(), "append fan_in_verify_unavailable failed") {
			t.Errorf("no WARN for the failed append; logs:\n%s", logs.String())
		}
	})
}
