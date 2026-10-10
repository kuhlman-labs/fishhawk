package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// rbFixture is the restart-blockers server under test: the attention run fake
// (it carries a stage table) plus the audit fake, with the reconcile tests'
// fixed boot marker so "dispatched by this process" is deterministic.
type rbFixture struct {
	runs  *attnRunRepo
	audit *auditFake
	// redispatchWired wires what redispatchEligibility needs for an ADVISORY
	// round to be genuinely eligible (approval condition C3): a reviewer
	// backend, an ArtifactRepo and a trace store. wireRedispatch also seeds
	// the awaiting_approval plan stage row the plan-round check reads.
	redispatchWired bool
}

func newRBFixture() *rbFixture {
	return &rbFixture{runs: newAttnRunRepo(), audit: newAuditFake()}
}

func (f *rbFixture) config() Config {
	cfg := Config{Addr: "127.0.0.1:0", RunRepo: f.runs, AuditRepo: f.audit, ProcessStart: bootMarker}
	if f.redispatchWired {
		cfg.PlanReviewers = singleReviewerSet{&fakePlanReviewer{verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictApprove}, model: "claude-opus-4-8"}}
		cfg.ArtifactRepo = newFakeArtifactRepo()
		cfg.TraceStore = newRedispatchTraceStore()
	}
	return cfg
}

// wireRedispatch makes an advisory round on runID ELIGIBLE for boot
// re-dispatch: it wires the reviewer/artifact/trace backends and seeds the
// round's stage (rbRoundStage) as an awaiting_approval plan stage that
// GetStage resolves.
func (f *rbFixture) wireRedispatch(runID uuid.UUID) {
	f.redispatchWired = true
	f.runs.mu.Lock()
	defer f.runs.mu.Unlock()
	f.runs.stagesByRun[runID] = append(f.runs.stagesByRun[runID], &run.Stage{
		ID: rbRoundStage, RunID: runID, Type: run.StageTypePlan, State: run.StageStateAwaitingApproval,
	})
}

// rbRoundStage is the stage every seeded review round is recorded against.
var rbRoundStage = attnID(900)

func (f *rbFixture) server() *Server { return New(f.config()) }

// seedChild seeds a running decomposition child of parent with one implement
// stage in the given state.
func (f *rbFixture) seedChild(childID, parentID, stageID uuid.UUID, state run.StageState, created time.Time) {
	ru := f.runs.seed(childID, "acme/app", run.StateRunning, created, "")
	p := parentID
	ru.DecomposedFrom = &p
	f.runs.addStage(childID, stageID, run.StageTypeImplement, state, created)
}

// seedParent seeds the decomposition parent run of a seedChild in an explicit
// state, so the undispatched_child parent-state filter (#4184) is exercised
// rather than masked. The parent has no stages and no review rounds.
func (f *rbFixture) seedParent(parentID uuid.UUID, state run.State, created time.Time) {
	f.runs.seed(parentID, "acme/app", state, created, "")
}

// seedReviewRound seeds one GATING *_review_started entry for a running run.
// A gating round is never re-dispatched on boot (redispatchEligibility), so a
// current-process unsettled one is a blocker whatever the fixture wires —
// the in-flight tests below do not depend on the reviewer wiring (#4077).
func (f *rbFixture) seedReviewRound(t *testing.T, runID uuid.UUID, kind string, seq int64, ts time.Time, configured int) {
	t.Helper()
	f.seedStarted(t, runID, kind, seq, ts, planreview.ReviewStartedPayload{ConfiguredAgents: configured, Authority: planreview.AuthorityGating})
}

// seedStarted seeds one *_review_started entry with an explicit payload.
func (f *rbFixture) seedStarted(t *testing.T, runID uuid.UUID, kind string, seq int64, ts time.Time, payload planreview.ReviewStartedPayload) {
	t.Helper()
	seedReviewAuditEntry(t, f.audit, runID, rbRoundStage, seq, ts, kind+"_review_started", payload)
}

// rbAdvisoryStarted is an advisory round redispatchEligibility accepts once
// wireRedispatch has run: an implement round carries a trace round source.
func rbAdvisoryStarted(kind string, depth int) planreview.ReviewStartedPayload {
	p := planreview.ReviewStartedPayload{ConfiguredAgents: 2, Authority: planreview.AuthorityAdvisory, RedispatchDepth: depth}
	if kind == "implement" {
		p.RoundOrigin = reviewRoundOriginTrace
		p.HeadSHA = "abc123"
	}
	return p
}

// rbStageKind resolves a "plan" / "implement" label to its reconcile kind.
func rbStageKind(t *testing.T, kind string) orphanedReviewStageKind {
	t.Helper()
	for _, st := range orphanedReviewStages {
		if st.label == kind {
			return st
		}
	}
	t.Fatalf("no orphaned review stage kind %q", kind)
	return orphanedReviewStageKind{}
}

// assertRoundEligible is the C3 fixture precondition: the seeded round must be
// genuinely ELIGIBLE under redispatchEligibility, or a "not a blocker" result
// could come from some other branch.
func assertRoundEligible(t *testing.T, s *Server, runID uuid.UUID, kind string, want bool) {
	t.Helper()
	stage := rbStageKind(t, kind)
	latest, payload, found, err := s.latestReviewStarted(context.Background(), runID, stage)
	if err != nil || !found {
		t.Fatalf("fixture: latest %s round: found=%v err=%v", kind, found, err)
	}
	ok, slug, err := s.redispatchEligibility(context.Background(), runID, stage, latest, payload)
	if err != nil || ok != want {
		t.Fatalf("fixture precondition: redispatchEligibility(%s) = (%v, %q, %v), want eligible=%v", kind, ok, slug, err, want)
	}
}

func getRestartBlockers(t *testing.T, s *Server, id Identity) *httptest.ResponseRecorder {
	t.Helper()
	req := withIdentity(httptest.NewRequest(http.MethodGet, "/v0/restart-blockers", nil), id)
	rec := httptest.NewRecorder()
	s.handleListRestartBlockers(rec, req)
	return rec
}

func decodeRestartBlockers(t *testing.T, rec *httptest.ResponseRecorder) restartBlockersResponse {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	var got restartBlockersResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v; body %s", err, rec.Body.String())
	}
	return got
}

func anonymous() Identity { return Identity{Subject: "anonymous"} }

// TestRestartBlockers_UndispatchedChild — counterfactual: mutating the child
// predicate to never match yields zero items.
func TestRestartBlockers_UndispatchedChild(t *testing.T) {
	for _, state := range []run.StageState{run.StageStatePending, run.StageStateAwaitingHostDispatch} {
		t.Run(string(state), func(t *testing.T) {
			f := newRBFixture()
			f.seedParent(attnID(2), run.StateRunning, attnT0)
			f.seedChild(attnID(1), attnID(2), attnID(3), state, attnT0)
			got := decodeRestartBlockers(t, getRestartBlockers(t, f.server(), anonymous()))
			if len(got.Items) != 1 {
				t.Fatalf("items = %+v, want one undispatched_child", got.Items)
			}
			it := got.Items[0]
			if it.RunID != attnID(1).String() || it.Reason != restartBlockerUndispatchedChild ||
				it.Stage != "implement" || it.ParentRunID != attnID(2).String() || it.StageState != string(state) {
				t.Errorf("item = %+v, want child %s of parent %s at %s", it, attnID(1), attnID(2), state)
			}
			// The running parent is scanned too: it is non-terminal.
			if got.ScannedRuns != 2 || got.Truncated {
				t.Errorf("scanned_runs = %d truncated = %v, want 2/false", got.ScannedRuns, got.Truncated)
			}
		})
	}
}

// TestRestartBlockers_ChildDispatched_NotBlocker: a child whose implement
// stage already left pending/awaiting_host_dispatch blocks nothing.
func TestRestartBlockers_ChildDispatched_NotBlocker(t *testing.T) {
	for _, state := range []run.StageState{run.StageStateDispatched, run.StageStateRunning, run.StageStateSucceeded} {
		t.Run(string(state), func(t *testing.T) {
			f := newRBFixture()
			// A LIVE parent, so the zero-item result cannot come from the
			// terminal-parent filter.
			f.seedParent(attnID(2), run.StateRunning, attnT0)
			f.seedChild(attnID(1), attnID(2), attnID(3), state, attnT0)
			got := decodeRestartBlockers(t, getRestartBlockers(t, f.server(), anonymous()))
			if len(got.Items) != 0 {
				t.Fatalf("items = %+v, want none for a dispatched child", got.Items)
			}
		})
	}
}

// TestRestartBlockers_TerminalParentChild_NotBlocker: a pending child of a
// SUCCEEDED / FAILED / CANCELLED decomposition parent can never be dispatched,
// so it is not a blocker (#4184). The #4186 cascade cancels children only on a
// cancel sink, so a failed / succeeded parent's child, and one orphaned before
// the reconcile-orphan-children backfill runs, still reach this skip. The live
// parent arms are the control: the same child under a pending / running parent
// IS reported. Counterfactuals: forcing the terminal test to false reports the
// terminal arms; forcing it to true drops the live arms.
func TestRestartBlockers_TerminalParentChild_NotBlocker(t *testing.T) {
	cases := []struct {
		parent      run.State
		wantItems   int
		wantScanned int
	}{
		// A terminal parent is not in the non-terminal scan set.
		{run.StateCancelled, 0, 1},
		{run.StateFailed, 0, 1},
		{run.StateSucceeded, 0, 1},
		{run.StatePending, 1, 2},
		{run.StateRunning, 1, 2},
	}
	for _, tc := range cases {
		t.Run(string(tc.parent), func(t *testing.T) {
			f := newRBFixture()
			f.seedParent(attnID(2), tc.parent, attnT0)
			f.seedChild(attnID(1), attnID(2), attnID(3), run.StageStatePending, attnT0)
			got := decodeRestartBlockers(t, getRestartBlockers(t, f.server(), anonymous()))
			if len(got.Items) != tc.wantItems || got.ScannedRuns != tc.wantScanned {
				t.Fatalf("parent %s: items = %+v scanned_runs = %d, want %d items over %d runs",
					tc.parent, got.Items, got.ScannedRuns, tc.wantItems, tc.wantScanned)
			}
			if tc.wantItems == 0 {
				return
			}
			it := got.Items[0]
			if it.RunID != attnID(1).String() || it.Reason != restartBlockerUndispatchedChild ||
				it.ParentRunID != attnID(2).String() || it.StageState != string(run.StageStatePending) {
				t.Errorf("item = %+v, want undispatched_child naming child %s of parent %s", it, attnID(1), attnID(2))
			}
		})
	}
}

// TestRestartBlockers_IncidentShape_TerminalParents_NoItems replays the #4184
// incident through the REAL mux (anonymous, as scripts/dev calls it), the
// handler, the fake run repository and the JSON encoder: running children with
// pending / awaiting_host_dispatch implement stages spread across two CANCELLED
// parents, plus one pending child under a SUCCEEDED parent. None can ever
// dispatch, so the response is 200 with zero items.
func TestRestartBlockers_IncidentShape_TerminalParents_NoItems(t *testing.T) {
	f := newRBFixture()
	cancelledA, cancelledB, succeeded := attnID(10), attnID(11), attnID(12)
	f.seedParent(cancelledA, run.StateCancelled, attnT0)
	f.seedParent(cancelledB, run.StateCancelled, attnT0)
	f.seedParent(succeeded, run.StateSucceeded, attnT0)
	for i, tc := range []struct {
		parent uuid.UUID
		state  run.StageState
	}{
		{cancelledA, run.StageStatePending},
		{cancelledA, run.StageStateAwaitingHostDispatch},
		{cancelledA, run.StageStatePending},
		{cancelledB, run.StageStateAwaitingHostDispatch},
		{cancelledB, run.StageStatePending},
		{succeeded, run.StageStatePending},
	} {
		f.seedChild(attnID(100+i), tc.parent, attnID(200+i), tc.state, attnT0.Add(time.Duration(i)*time.Second))
	}

	rec := httptest.NewRecorder()
	f.server().Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v0/restart-blockers", nil))
	got := decodeRestartBlockers(t, rec)
	if len(got.Items) != 0 {
		t.Fatalf("items = %+v, want none: every pending child belongs to a terminal parent", got.Items)
	}
	if got.ScannedRuns != 6 || got.Truncated {
		t.Errorf("scanned_runs = %d truncated = %v, want the 6 running children / false", got.ScannedRuns, got.Truncated)
	}
}

// TestRestartBlockers_ParentReadMemoized: the parent is read ONCE per distinct
// parent per request, however many pending children it has. Driver (approval
// condition C1): getRestartBlockers calls s.handleListRestartBlockers
// directly, NOT s.Handler(), so fakeRepo.getRunCalls counts only the parent
// reads the restart-blockers check makes and not the run-scoped authz wrappers
// (resolveRunForAuthz) the mux would add. Counterfactual: bypassing the memo
// makes getRunCalls 2.
func TestRestartBlockers_ParentReadMemoized(t *testing.T) {
	for _, tc := range []struct {
		name      string
		parent    run.State
		wantItems int
	}{
		{"terminal_parent", run.StateCancelled, 0},
		{"live_parent", run.StateRunning, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRBFixture()
			f.seedParent(attnID(2), tc.parent, attnT0)
			f.seedChild(attnID(1), attnID(2), attnID(3), run.StageStatePending, attnT0)
			f.seedChild(attnID(4), attnID(2), attnID(5), run.StageStateAwaitingHostDispatch, attnT0.Add(time.Second))
			got := decodeRestartBlockers(t, getRestartBlockers(t, f.server(), anonymous()))
			if len(got.Items) != tc.wantItems {
				t.Fatalf("items = %+v, want %d", got.Items, tc.wantItems)
			}
			if f.runs.getRunCalls != 1 {
				t.Errorf("GetRun calls = %d, want 1 (one memoized read for the shared parent)", f.runs.getRunCalls)
			}
		})
	}
}

// TestRestartBlockers_NonChildPendingImplement_NotBlocker — counterfactual:
// deleting the DecomposedFrom check reports a non-child's pending implement.
func TestRestartBlockers_NonChildPendingImplement_NotBlocker(t *testing.T) {
	f := newRBFixture()
	f.runs.seed(attnID(1), "acme/app", run.StateRunning, attnT0, "")
	f.runs.addStage(attnID(1), attnID(3), run.StageTypeImplement, run.StageStatePending, attnT0)
	got := decodeRestartBlockers(t, getRestartBlockers(t, f.server(), anonymous()))
	if len(got.Items) != 0 {
		t.Fatalf("items = %+v, want none for a non-decomposition run", got.Items)
	}
}

// TestRestartBlockers_ReviewInFlight: a GATING round dispatched by THIS
// process with fewer verdicts than configured reviewers is a blocker, for both
// stages — the next boot would not re-dispatch it.
func TestRestartBlockers_ReviewInFlight(t *testing.T) {
	for _, kind := range []string{"plan", "implement"} {
		t.Run(kind, func(t *testing.T) {
			f := newRBFixture()
			f.runs.seed(attnID(1), "acme/app", run.StatePending, attnT0, "")
			f.seedReviewRound(t, attnID(1), kind, 1, afterBoot, 2)
			seedReviewAuditEntry(t, f.audit, attnID(1), attnID(900), 2, afterBoot, kind+"_reviewed", map[string]any{})
			got := decodeRestartBlockers(t, getRestartBlockers(t, f.server(), anonymous()))
			if len(got.Items) != 1 {
				t.Fatalf("items = %+v, want one review_in_flight", got.Items)
			}
			it := got.Items[0]
			if it.Reason != restartBlockerReviewInFlight || it.Stage != kind || it.RunID != attnID(1).String() {
				t.Errorf("item = %+v, want %s review_in_flight on %s", it, kind, attnID(1))
			}
			if it.ConfiguredAgents == nil || *it.ConfiguredAgents != 2 || it.Landed == nil || *it.Landed != 1 {
				t.Errorf("counts = configured %v landed %v, want 2/1", it.ConfiguredAgents, it.Landed)
			}
		})
	}
}

// TestRestartBlockers_AdvisoryRound: an advisory current-process round the
// next boot sweep WOULD re-dispatch is not a blocker; the same round with no
// reviewer backend wired is. Counterfactual C10: dropping the
// redispatchEligibility call reports the eligible arm. The fixture is
// asserted ELIGIBLE first (approval condition C3), so the eligible arm cannot
// pass through another branch.
func TestRestartBlockers_AdvisoryRound(t *testing.T) {
	for _, kind := range []string{"plan", "implement"} {
		t.Run(kind+"/eligible_not_blocker", func(t *testing.T) {
			f := newRBFixture()
			f.runs.seed(attnID(1), "acme/app", run.StateRunning, attnT0, "")
			f.wireRedispatch(attnID(1))
			f.seedStarted(t, attnID(1), kind, 1, afterBoot, rbAdvisoryStarted(kind, 0))
			s := f.server()
			assertRoundEligible(t, s, attnID(1), kind, true)
			got := decodeRestartBlockers(t, getRestartBlockers(t, s, anonymous()))
			if len(got.Items) != 0 {
				t.Fatalf("items = %+v, want none: the next boot re-dispatches an eligible advisory round", got.Items)
			}
		})
		t.Run(kind+"/reviewer_unwired_blocker", func(t *testing.T) {
			f := newRBFixture()
			f.runs.seed(attnID(1), "acme/app", run.StateRunning, attnT0, "")
			f.seedStarted(t, attnID(1), kind, 1, afterBoot, rbAdvisoryStarted(kind, 0))
			got := decodeRestartBlockers(t, getRestartBlockers(t, f.server(), anonymous()))
			if len(got.Items) != 1 || got.Items[0].Reason != restartBlockerReviewInFlight || got.Items[0].Stage != kind {
				t.Fatalf("items = %+v, want one %s review_in_flight (no reviewer to re-dispatch to)", got.Items, kind)
			}
		})
	}
}

// TestRestartBlockers_ImplementRoundOnTerminalStage: GET /v0/restart-blockers
// shares redispatchEligibility's implement stage-state check (#4174). A
// current-process advisory implement round on a FAILED or CANCELLED implement
// stage is a blocker, because the next boot would close it failed
// (implement_stage_terminal) instead of re-dispatching it. A SUCCEEDED stage
// is the control: a gateless implement stage settles succeeded on PR upload
// while its advisory round is still in flight, so that round stays eligible
// and blocks nothing. Counterfactuals: a no-op stage-state check drops the
// failed / cancelled items; refusing every terminal state (IsTerminal)
// reports the succeeded arm.
func TestRestartBlockers_ImplementRoundOnTerminalStage(t *testing.T) {
	for _, tc := range []struct {
		state     run.StageState
		wantItems int
	}{
		{run.StageStateFailed, 1},
		{run.StageStateCancelled, 1},
		{run.StageStateSucceeded, 0},
	} {
		t.Run(string(tc.state), func(t *testing.T) {
			runID := attnID(1)
			f := newRBFixture()
			f.runs.seed(runID, "acme/app", run.StateRunning, attnT0, "")
			f.wireRedispatch(runID)
			f.runs.mu.Lock()
			for _, st := range f.runs.stagesByRun[runID] {
				if st.ID == rbRoundStage {
					st.Type, st.State = run.StageTypeImplement, tc.state
				}
			}
			f.runs.mu.Unlock()
			f.seedStarted(t, runID, "implement", 1, afterBoot, rbAdvisoryStarted("implement", 0))
			s := f.server()
			if tc.wantItems == 0 {
				// C3: the control must be genuinely eligible, not pass through
				// another branch.
				assertRoundEligible(t, s, runID, "implement", true)
			}
			got := decodeRestartBlockers(t, getRestartBlockers(t, s, anonymous()))
			if len(got.Items) != tc.wantItems {
				t.Fatalf("items = %+v, want %d on a %s implement stage", got.Items, tc.wantItems, tc.state)
			}
			if tc.wantItems == 1 && (got.Items[0].Reason != restartBlockerReviewInFlight || got.Items[0].Stage != "implement") {
				t.Fatalf("item = %+v, want one implement review_in_flight", got.Items[0])
			}
		})
	}
}

// TestRestartBlockers_IneligibleWiredRoundIsBlocker: on the fully wired
// (otherwise eligible) fixture, a round the boot sweep would close failed is a
// blocker — a GATING round, and an advisory round at the re-dispatch depth cap.
// Counterfactual: a permissive authority or depth branch in
// redispatchEligibility drops the item.
func TestRestartBlockers_IneligibleWiredRoundIsBlocker(t *testing.T) {
	cases := map[string]planreview.ReviewStartedPayload{
		"gating":    {ConfiguredAgents: 2, Authority: planreview.AuthorityGating},
		"depth_cap": rbAdvisoryStarted("plan", maxReviewRoundRedispatches),
		"under_cap": rbAdvisoryStarted("plan", maxReviewRoundRedispatches-1),
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			f := newRBFixture()
			f.runs.seed(attnID(1), "acme/app", run.StateRunning, attnT0, "")
			f.wireRedispatch(attnID(1))
			f.seedStarted(t, attnID(1), "plan", 1, afterBoot, payload)
			got := decodeRestartBlockers(t, getRestartBlockers(t, f.server(), anonymous()))
			if name == "under_cap" {
				// The control arm: one level below the cap is still re-dispatched.
				if len(got.Items) != 0 {
					t.Fatalf("items = %+v, want none one level under the depth cap", got.Items)
				}
				return
			}
			if len(got.Items) != 1 || got.Items[0].Reason != restartBlockerReviewInFlight || got.Items[0].Stage != "plan" {
				t.Fatalf("items = %+v, want one plan review_in_flight", got.Items)
			}
			if *got.Items[0].ConfiguredAgents != 2 || *got.Items[0].Landed != 0 {
				t.Errorf("counts = %d/%d, want 2/0", *got.Items[0].ConfiguredAgents, *got.Items[0].Landed)
			}
		})
	}
}

// TestRestartBlockers_PendingRedispatchIsBlocker: an orphaned round this
// process's boot sweep handed to a re-dispatch goroutine is a blocker even
// though it predates the boot marker and is otherwise eligible — a restart
// kills the goroutine, and the next boot closes the round failed
// (already_redispatched). Counterfactual C9: deleting the pending check, or
// moving it after the boot-marker skip, drops the item. The unmarked arm is
// the control: the same round with no pending entry blocks nothing.
func TestRestartBlockers_PendingRedispatchIsBlocker(t *testing.T) {
	runID := attnID(4077)
	for _, pending := range []bool{true, false} {
		t.Run(map[bool]string{true: "pending", false: "not_pending"}[pending], func(t *testing.T) {
			f := newRBFixture()
			f.runs.seed(runID, "acme/app", run.StateRunning, attnT0, "")
			f.wireRedispatch(runID)
			f.seedStarted(t, runID, "plan", 7, beforeBoot, rbAdvisoryStarted("plan", 0))
			if pending {
				key := pendingRedispatchKey{runID: runID, stage: "plan", seq: 7}
				markRedispatchPending(key)
				t.Cleanup(func() { clearRedispatchPending(key) })
			}
			got := decodeRestartBlockers(t, getRestartBlockers(t, f.server(), anonymous()))
			if !pending {
				if len(got.Items) != 0 {
					t.Fatalf("items = %+v, want none for a prior-process round with no pending re-dispatch", got.Items)
				}
				return
			}
			if len(got.Items) != 1 || got.Items[0].Reason != restartBlockerReviewInFlight || got.Items[0].Stage != "plan" {
				t.Fatalf("items = %+v, want one plan review_in_flight for the pending re-dispatch", got.Items)
			}
		})
	}
}

// TestRestartBlockers_ReviewSettled_NotBlocker — counterfactual: mutating the
// landed comparison reports a settled round.
func TestRestartBlockers_ReviewSettled_NotBlocker(t *testing.T) {
	f := newRBFixture()
	f.runs.seed(attnID(1), "acme/app", run.StateRunning, attnT0, "")
	f.seedReviewRound(t, attnID(1), "implement", 1, afterBoot, 2)
	seedReviewAuditEntry(t, f.audit, attnID(1), attnID(900), 2, afterBoot, "implement_reviewed", map[string]any{})
	seedReviewAuditEntry(t, f.audit, attnID(1), attnID(900), 3, afterBoot, "implement_review_failed", map[string]any{})
	got := decodeRestartBlockers(t, getRestartBlockers(t, f.server(), anonymous()))
	if len(got.Items) != 0 {
		t.Fatalf("items = %+v, want none for a settled round", got.Items)
	}
}

// TestRestartBlockers_PriorProcessOrphan_NotBlocker — counterfactual: deleting
// the boot-marker condition reports a round an EARLIER process dispatched
// (this process's boot sweep already re-dispatched or closed it; a restart
// loses nothing more).
func TestRestartBlockers_PriorProcessOrphan_NotBlocker(t *testing.T) {
	f := newRBFixture()
	f.runs.seed(attnID(1), "acme/app", run.StateRunning, attnT0, "")
	f.seedReviewRound(t, attnID(1), "plan", 1, beforeBoot, 1)
	got := decodeRestartBlockers(t, getRestartBlockers(t, f.server(), anonymous()))
	if len(got.Items) != 0 {
		t.Fatalf("items = %+v, want none for a prior-process orphan", got.Items)
	}
}

// TestRestartBlockers_ZeroConfigured_NotBlocker: a round with no configured
// reviewer is never pending.
func TestRestartBlockers_ZeroConfigured_NotBlocker(t *testing.T) {
	f := newRBFixture()
	f.runs.seed(attnID(1), "acme/app", run.StateRunning, attnT0, "")
	f.seedReviewRound(t, attnID(1), "plan", 1, afterBoot, 0)
	got := decodeRestartBlockers(t, getRestartBlockers(t, f.server(), anonymous()))
	if len(got.Items) != 0 {
		t.Fatalf("items = %+v, want none for a zero-reviewer round", got.Items)
	}
}

// TestRestartBlockers_AttemptCorrelation: the LATEST round decides, and only
// verdicts strictly after it count. Counterfactuals: reading the earliest
// round reports the settled arm; a run-wide terminal count hides the
// unsettled arm.
func TestRestartBlockers_AttemptCorrelation(t *testing.T) {
	t.Run("latest_settled_earlier_unsettled", func(t *testing.T) {
		f := newRBFixture()
		f.runs.seed(attnID(1), "acme/app", run.StateRunning, attnT0, "")
		f.seedReviewRound(t, attnID(1), "implement", 1, afterBoot, 2)
		f.seedReviewRound(t, attnID(1), "implement", 3, afterBoot, 1)
		seedReviewAuditEntry(t, f.audit, attnID(1), attnID(900), 4, afterBoot, "implement_reviewed", map[string]any{})
		got := decodeRestartBlockers(t, getRestartBlockers(t, f.server(), anonymous()))
		if len(got.Items) != 0 {
			t.Fatalf("items = %+v, want none: the latest round is settled", got.Items)
		}
	})
	t.Run("prior_round_verdict_not_counted", func(t *testing.T) {
		f := newRBFixture()
		f.runs.seed(attnID(1), "acme/app", run.StateRunning, attnT0, "")
		f.seedReviewRound(t, attnID(1), "implement", 1, afterBoot, 1)
		seedReviewAuditEntry(t, f.audit, attnID(1), attnID(900), 2, afterBoot, "implement_reviewed", map[string]any{})
		f.seedReviewRound(t, attnID(1), "implement", 3, afterBoot, 1)
		got := decodeRestartBlockers(t, getRestartBlockers(t, f.server(), anonymous()))
		if len(got.Items) != 1 || got.Items[0].Reason != restartBlockerReviewInFlight {
			t.Fatalf("items = %+v, want one review_in_flight for the fresh round", got.Items)
		}
		if *got.Items[0].Landed != 0 {
			t.Errorf("landed = %d, want 0 (the prior round's verdict must not count)", *got.Items[0].Landed)
		}
	})
}

// TestRestartBlockers_TerminalRunNotScanned — counterfactual: dropping the
// per-state filter scans a terminal run's stale started entry.
func TestRestartBlockers_TerminalRunNotScanned(t *testing.T) {
	f := newRBFixture()
	f.runs.seed(attnID(1), "acme/app", run.StateSucceeded, attnT0, "")
	f.seedReviewRound(t, attnID(1), "implement", 1, afterBoot, 1)
	got := decodeRestartBlockers(t, getRestartBlockers(t, f.server(), anonymous()))
	if len(got.Items) != 0 || got.ScannedRuns != 0 {
		t.Fatalf("items = %+v scanned_runs = %d, want nothing for a terminal run", got.Items, got.ScannedRuns)
	}
}

// TestRestartBlockers_CheckFailed: a per-run read error from a reachable
// daemon is reported as check_failed (scripts/dev refuses on it), never
// silently dropped. Counterfactual: mutating each error branch to `continue`
// drops the item.
func TestRestartBlockers_CheckFailed(t *testing.T) {
	t.Run("stage_read", func(t *testing.T) {
		f := newRBFixture()
		f.seedChild(attnID(1), attnID(2), attnID(3), run.StageStatePending, attnT0)
		f.runs.stageErr[attnID(1)] = errors.New("boom")
		got := decodeRestartBlockers(t, getRestartBlockers(t, f.server(), anonymous()))
		if len(got.Items) != 1 || got.Items[0].Reason != restartBlockerCheckFailed || got.Items[0].Stage != "implement" {
			t.Fatalf("items = %+v, want one implement check_failed", got.Items)
		}
	})
	t.Run("parent_read", func(t *testing.T) {
		// GetRun is the ONLY failing read: the stage read succeeds and no
		// review round is seeded, so nothing else reaches GetRun. Counterfactual:
		// skipping the child on a parent error yields zero items, and falling
		// through to the live-parent path yields undispatched_child.
		f := newRBFixture()
		f.seedParent(attnID(2), run.StateRunning, attnT0)
		f.seedChild(attnID(1), attnID(2), attnID(3), run.StageStatePending, attnT0)
		f.runs.getErr = errors.New("db down")
		got := decodeRestartBlockers(t, getRestartBlockers(t, f.server(), anonymous()))
		if len(got.Items) != 1 || got.Items[0].Reason != restartBlockerCheckFailed ||
			got.Items[0].Stage != "implement" || got.Items[0].RunID != attnID(1).String() {
			t.Fatalf("items = %+v, want one implement check_failed on the child", got.Items)
		}
	})
	t.Run("parent_not_found", func(t *testing.T) {
		// The parent is never seeded, so GetRun returns run.ErrNotFound (the
		// concurrent-delete race decomposed_from ON DELETE SET NULL leaves).
		// Fail closed: a parent that cannot be decided is check_failed.
		f := newRBFixture()
		f.seedChild(attnID(1), attnID(2), attnID(3), run.StageStatePending, attnT0)
		got := decodeRestartBlockers(t, getRestartBlockers(t, f.server(), anonymous()))
		if len(got.Items) != 1 || got.Items[0].Reason != restartBlockerCheckFailed ||
			got.Items[0].Stage != "implement" || got.Items[0].RunID != attnID(1).String() {
			t.Fatalf("items = %+v, want one implement check_failed on the child", got.Items)
		}
	})
	t.Run("audit_started_read", func(t *testing.T) {
		f := newRBFixture()
		f.runs.seed(attnID(1), "acme/app", run.StateRunning, attnT0, "")
		f.audit.listByCategoryErrCategory = "plan_review_started"
		got := decodeRestartBlockers(t, getRestartBlockers(t, f.server(), anonymous()))
		if len(got.Items) != 1 || got.Items[0].Reason != restartBlockerCheckFailed || got.Items[0].Stage != "plan" {
			t.Fatalf("items = %+v, want one plan check_failed", got.Items)
		}
	})
	t.Run("started_payload_undecodable", func(t *testing.T) {
		f := newRBFixture()
		f.runs.seed(attnID(1), "acme/app", run.StateRunning, attnT0, "")
		rid, sid := attnID(1), attnID(900)
		f.audit.seeded = append(f.audit.seeded, &audit.Entry{
			RunID: &rid, StageID: &sid, Sequence: 1, Timestamp: afterBoot,
			Category: "implement_review_started", Payload: []byte("{not json"),
		})
		got := decodeRestartBlockers(t, getRestartBlockers(t, f.server(), anonymous()))
		if len(got.Items) != 1 || got.Items[0].Reason != restartBlockerCheckFailed || got.Items[0].Stage != "implement" {
			t.Fatalf("items = %+v, want one implement check_failed", got.Items)
		}
	})
	t.Run("eligibility_read", func(t *testing.T) {
		// Counterfactual: mapping the redispatchEligibility error to
		// "eligible" (continue) drops the item.
		f := newRBFixture()
		f.runs.seed(attnID(1), "acme/app", run.StateRunning, attnT0, "")
		f.wireRedispatch(attnID(1))
		f.seedStarted(t, attnID(1), "plan", 1, afterBoot, rbAdvisoryStarted("plan", 0))
		f.audit.listByCategoryErrCategory = categoryReviewRoundRedispatched
		got := decodeRestartBlockers(t, getRestartBlockers(t, f.server(), anonymous()))
		if len(got.Items) != 1 || got.Items[0].Reason != restartBlockerCheckFailed || got.Items[0].Stage != "plan" {
			t.Fatalf("items = %+v, want one plan check_failed", got.Items)
		}
	})
	t.Run("audit_landed_read", func(t *testing.T) {
		f := newRBFixture()
		f.runs.seed(attnID(1), "acme/app", run.StateRunning, attnT0, "")
		f.seedReviewRound(t, attnID(1), "implement", 1, afterBoot, 1)
		f.audit.listByCategoryErrCategory = "implement_review_skipped"
		got := decodeRestartBlockers(t, getRestartBlockers(t, f.server(), anonymous()))
		if len(got.Items) != 1 || got.Items[0].Reason != restartBlockerCheckFailed || got.Items[0].Stage != "implement" {
			t.Fatalf("items = %+v, want one implement check_failed", got.Items)
		}
	})
}

// TestRestartBlockers_Truncated: the scan cap bites observably.
func TestRestartBlockers_Truncated(t *testing.T) {
	f := newRBFixture()
	for i := 0; i < restartBlockersRunScanLimit+1; i++ {
		f.runs.seed(uuid.New(), "acme/app", run.StateRunning, attnT0.Add(time.Duration(i)*time.Second), "")
	}
	got := decodeRestartBlockers(t, getRestartBlockers(t, f.server(), anonymous()))
	if !got.Truncated || got.ScannedRuns != restartBlockersRunScanLimit {
		t.Fatalf("truncated = %v scanned_runs = %d, want true/%d", got.Truncated, got.ScannedRuns, restartBlockersRunScanLimit)
	}
}

// TestRestartBlockers_Unconfigured: each required repository 503s by name.
func TestRestartBlockers_Unconfigured(t *testing.T) {
	f := newRBFixture()
	cfg := f.config()
	cfg.RunRepo = nil
	rec := getRestartBlockers(t, New(cfg), anonymous())
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no run repo: status = %d, want 503", rec.Code)
	}
	assertErrorCode(t, rec, "run_repo_unconfigured")

	cfg = f.config()
	cfg.AuditRepo = nil
	rec = getRestartBlockers(t, New(cfg), anonymous())
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no audit repo: status = %d, want 503", rec.Code)
	}
	assertErrorCode(t, rec, "audit_repo_unconfigured")
}

// TestRestartBlockers_ListRunsError: a run-scan failure is a 500, not an
// empty (clean-looking) list.
func TestRestartBlockers_ListRunsError(t *testing.T) {
	f := newRBFixture()
	f.runs.listErr = errors.New("db down")
	rec := getRestartBlockers(t, f.server(), anonymous())
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body %s", rec.Code, rec.Body.String())
	}
}

// TestRestartBlockers_Narrowing: the scan is bounded by the caller's account
// and the repo-visibility filter, exactly as GET /v0/runs; a filter fault
// fails the whole request closed.
func TestRestartBlockers_Narrowing(t *testing.T) {
	memberServer := func(f *rbFixture, vis RepoVisibility) *Server {
		cfg := f.config()
		cfg.AccountRoles = fakeAccountRoles{role: "member"}
		cfg.RepoVisibility = vis
		return New(cfg)
	}
	seedTwo := func(f *rbFixture) {
		f.runs.seed(attnID(1), "alpha/one", run.StateRunning, attnT0, "")
		f.seedReviewRound(t, attnID(1), "plan", 1, afterBoot, 1)
		f.runs.seed(attnID(4), "beta/two", run.StateRunning, attnT0.Add(time.Minute), "")
		f.seedReviewRound(t, attnID(4), "plan", 2, afterBoot, 1)
	}
	t.Run("repo_filter", func(t *testing.T) {
		f := newRBFixture()
		seedTwo(f)
		s := memberServer(f, newFakeRepoVisibility(map[string]bool{"alpha/one": true}))
		got := decodeRestartBlockers(t, getRestartBlockers(t, s, memberIdentity()))
		if len(got.Items) != 1 || got.Items[0].RunID != attnID(1).String() || got.ScannedRuns != 1 {
			t.Fatalf("items = %+v scanned = %d, want only alpha/one's run", got.Items, got.ScannedRuns)
		}
	})
	t.Run("repo_filter_error_fails_closed", func(t *testing.T) {
		f := newRBFixture()
		seedTwo(f)
		s := memberServer(f, fakeErrOnRepoVisibility{allowed: "alpha/one", failing: "beta/two"})
		rec := getRestartBlockers(t, s, memberIdentity())
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503; body %s", rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), attnID(1).String()) {
			t.Errorf("fail-closed body leaks a partially-narrowed page: %s", rec.Body.String())
		}
	})
	t.Run("account_filter", func(t *testing.T) {
		f := newRBFixture()
		seedTwo(f)
		acctA, acctB := uuid.NewString(), uuid.NewString()
		f.runs.runs[attnID(1)].AccountID = acctA
		f.runs.runs[attnID(4)].AccountID = acctB
		got := decodeRestartBlockers(t, getRestartBlockers(t, f.server(), Identity{Subject: "github:op", AccountID: acctA}))
		if len(got.Items) != 1 || got.Items[0].RunID != attnID(1).String() {
			t.Fatalf("items = %+v, want only account A's run", got.Items)
		}
	})
}

func restartBlockersGoldenPath(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed; cannot resolve the wire golden fixture path")
	}
	return filepath.Join(filepath.Dir(thisFile), "..", "..", "..", "testdata", "wire", "restart_blockers.json")
}

// TestRestartBlockers_EndToEnd_Golden drives the REAL mux (s.Handler()) with
// NO credential — the anonymous posture scripts/dev relies on — across all
// three reasons, and compares the RAW response bytes to the shared wire golden
// testdata/wire/restart_blockers.json. scripts/test-dev's RB-k case feeds the
// same file to scripts/dev's _parse_restart_blockers, so a field rename on
// either side fails one of the two. Regenerate with
// FISHHAWK_UPDATE_WIRE_GOLDEN=1.
func TestRestartBlockers_EndToEnd_Golden(t *testing.T) {
	f := newRBFixture()
	// undispatched_child (the parent, attnID(2), is seeded LIVE below)
	f.seedChild(attnID(1), attnID(2), attnID(3), run.StageStateAwaitingHostDispatch, attnT0)
	// review_in_flight (plan): a GATING round, which no boot re-dispatches
	// (#4077), so the item survives the narrowed predicate and the golden
	// stays byte-identical.
	f.runs.seed(attnID(4), "acme/app", run.StatePending, attnT0.Add(time.Minute), "")
	f.seedReviewRound(t, attnID(4), "plan", 1, afterBoot, 2)
	// check_failed (child stage read)
	f.seedChild(attnID(5), attnID(2), attnID(6), run.StageStatePending, attnT0.Add(2*time.Minute))
	f.runs.stageErr[attnID(5)] = errors.New("boom")
	// the live decomposition parent of the two children above: a running run
	// with no stages and no review rounds, so it is scanned and blocks nothing.
	// It must be non-terminal or the undispatched_child item is filtered (#4184).
	f.seedParent(attnID(2), run.StateRunning, attnT0.Add(3*time.Minute))

	rec := httptest.NewRecorder()
	f.server().Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v0/restart-blockers", nil))
	got := decodeRestartBlockers(t, rec)
	if len(got.Items) != 3 || got.ScannedRuns != 4 || got.Truncated {
		t.Fatalf("items = %+v scanned = %d truncated = %v, want 3 items over 4 runs", got.Items, got.ScannedRuns, got.Truncated)
	}

	path := restartBlockersGoldenPath(t)
	if os.Getenv("FISHHAWK_UPDATE_WIRE_GOLDEN") == "1" {
		if err := os.WriteFile(path, rec.Body.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	golden, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read wire golden %s: %v", path, err)
	}
	if !bytes.Equal(rec.Body.Bytes(), golden) {
		t.Fatalf("GET /v0/restart-blockers body drifted from the shared wire golden %s (scripts/test-dev RB-k parses that file — update both sides deliberately):\n--- got ---\n%s\n--- golden ---\n%s", path, rec.Body.String(), golden)
	}
}

// TestOpenAPI_RestartBlockersRouteDocumented: the route, its schemas and both
// closed enums are in the OpenAPI source of truth.
func TestOpenAPI_RestartBlockersRouteDocumented(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", "api", "v0.openapi.yaml"))
	if err != nil {
		t.Fatalf("read openapi: %v", err)
	}
	doc := string(raw)
	for _, want := range []string{
		"\n  /v0/restart-blockers:\n", "operationId: listRestartBlockers",
		"\n    RestartBlockerList:\n", "\n    RestartBlocker:\n",
		"enum: [undispatched_child, review_in_flight, check_failed]",
		"enum: [plan, implement]", "audit_repo_unconfigured",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("docs/api/v0.openapi.yaml is missing %q", strings.TrimSpace(want))
		}
	}
}
