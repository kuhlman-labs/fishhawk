package server

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/mergequeue"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/postgres"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/signing"
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

// TestRestartBlockers_TerminalRunPastWindow_NotScanned: a terminal run's started
// entry older than its window is no candidate, so nothing names the run. In-window
// terminal rounds are scanned; see the ADR-140 suite below. Counterfactual: forcing
// the discovery window open reads and scans this run.
func TestRestartBlockers_TerminalRunPastWindow_NotScanned(t *testing.T) {
	f := newRBFixture()
	f.runs.seed(attnID(1), "acme/app", run.StateSucceeded, attnT0, "")
	f.seedReviewRound(t, attnID(1), "implement", 1, afterBoot, 1)
	got := decodeRestartBlockers(t, getRestartBlockers(t, f.server(), anonymous()))
	if len(got.Items) != 0 || got.ScannedRuns != 0 {
		t.Fatalf("items = %+v scanned_runs = %d, want nothing for a terminal run past its window", got.Items, got.ScannedRuns)
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

// TestRestartBlockers_MergeInFlight (ADR-093 D8 / #4298): a run whose
// head-of-line queue merge THIS process has in flight is a merge_in_flight
// blocker naming stage merge; a cleared registry, or a terminal run, blocks
// nothing. Counterfactual C1: deleting check (c)'s append drops the item.
func TestRestartBlockers_MergeInFlight(t *testing.T) {
	t.Run("marked", func(t *testing.T) {
		f := newRBFixture()
		f.runs.seed(attnID(1), "acme/app", run.StateRunning, attnT0, "")
		s := f.server()
		s.mergeInFlight.mark(attnID(1))
		got := decodeRestartBlockers(t, getRestartBlockers(t, s, anonymous()))
		if len(got.Items) != 1 {
			t.Fatalf("items = %+v, want one merge_in_flight item", got.Items)
		}
		it := got.Items[0]
		if it.RunID != attnID(1).String() || it.Reason != restartBlockerMergeInFlight || it.Stage != restartBlockerStageMerge ||
			it.ParentRunID != "" || it.StageState != "" || it.ConfiguredAgents != nil || it.Landed != nil {
			t.Fatalf("item = %+v, want {run 1, merge_in_flight, merge} with no other field", it)
		}
	})
	t.Run("cleared", func(t *testing.T) {
		f := newRBFixture()
		f.runs.seed(attnID(1), "acme/app", run.StateRunning, attnT0, "")
		s := f.server()
		s.mergeInFlight.mark(attnID(1))
		s.mergeInFlight.clear(attnID(1))
		if got := decodeRestartBlockers(t, getRestartBlockers(t, s, anonymous())); len(got.Items) != 0 || got.ScannedRuns != 1 {
			t.Fatalf("items = %+v scanned = %d, want nothing once the registry is cleared", got.Items, got.ScannedRuns)
		}
	})
	t.Run("terminal_run", func(t *testing.T) {
		f := newRBFixture()
		f.runs.seed(attnID(1), "acme/app", run.StateSucceeded, attnT0, "")
		s := f.server()
		s.mergeInFlight.mark(attnID(1))
		if got := decodeRestartBlockers(t, getRestartBlockers(t, s, anonymous())); len(got.Items) != 0 || got.ScannedRuns != 0 {
			t.Fatalf("items = %+v scanned = %d, want nothing for a terminal run", got.Items, got.ScannedRuns)
		}
	})
}

// TestRestartBlockers_LiveQueueMergeListed_EndToEnd drives the REAL merge
// queue (Postgres store, D1 ledger, audit chain): while the pinned merger is
// mid-call for the head-of-line entry, GET /v0/restart-blockers through
// s.Handler() lists the run as merge_in_flight / merge; once the pump has
// applied the outcome (ruling R8c) the list is empty. Counterfactual C1:
// deleting check (c) empties the mid-call list.
func TestRestartBlockers_LiveQueueMergeListed_EndToEnd(t *testing.T) {
	w := newMQMWorld(t)
	a := w.seed("a", "h1", []byte(mcvUndelegatedSpecYAML))
	w.mqeRebased(a, "pre-h1", "h1", "h1", false)
	w.mqePassed(a, "h1")
	w.live(a, "h1") // the live head of line: passed, carrying merge intent
	list := func() restartBlockersResponse {
		rec := httptest.NewRecorder()
		w.srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v0/restart-blockers", nil))
		return decodeRestartBlockers(t, rec)
	}
	var during restartBlockersResponse
	probed := false
	w.srv.cfg.GateMerger = mqmProbeMerger{mqmMerger: w.merger, probe: func() {
		probed = true
		during = list()
	}}
	w.srv.PumpMergeCandidateQueues(context.Background())
	if !probed {
		t.Fatal("the pinned merger was never called, so the in-flight window was never observed")
	}
	want := restartBlocker{RunID: a.id.String(), Reason: restartBlockerMergeInFlight, Stage: restartBlockerStageMerge}
	found := false
	for _, it := range during.Items {
		if it.RunID == want.RunID && it.Reason == want.Reason && it.Stage == want.Stage {
			found = true
		}
	}
	if !found {
		t.Fatalf("mid-merge items = %+v, want %+v listed", during.Items, want)
	}
	w.requireState(a, mergequeue.StateMerged, mergeCandidateQueueReasonMergedByQueue)
	if after := list(); len(after.Items) != 0 {
		t.Fatalf("items after the outcome was applied = %+v, want none", after.Items)
	}
}

func restartBlockersGoldenPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(pkgSrcDir, "..", "..", "..", "testdata", "wire", "restart_blockers.json")
}

// TestRestartBlockers_EndToEnd_Golden drives the REAL mux (s.Handler()) with
// NO credential — the anonymous posture scripts/dev relies on — across all
// four reasons, and compares the RAW response bytes to the shared wire golden
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
	// merge_in_flight (ADR-093 D8 / #4298): a run whose head-of-line queue
	// merge this process has in flight.
	f.runs.seed(attnID(7), "acme/app", run.StateRunning, attnT0.Add(4*time.Minute), "")
	s := f.server()
	s.mergeInFlight.mark(attnID(7))

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v0/restart-blockers", nil))
	got := decodeRestartBlockers(t, rec)
	if len(got.Items) != 4 || got.ScannedRuns != 5 || got.Truncated {
		t.Fatalf("items = %+v scanned = %d truncated = %v, want 4 items over 5 runs", got.Items, got.ScannedRuns, got.Truncated)
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
		"enum: [undispatched_child, review_in_flight, check_failed, merge_in_flight]",
		"enum: [plan, implement, merge]", "audit_repo_unconfigured",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("docs/api/v0.openapi.yaml is missing %q", strings.TrimSpace(want))
		}
	}
}

// --- ADR-140 (#5047): terminal-run discovery on the route --------------------------

// rbRunRepo layers per-run read faults, read counts, a state-scan hide list and a
// post-scan hook over the fixture's fake.
type rbRunRepo struct {
	*attnRunRepo
	faultMu sync.Mutex
	faults  map[uuid.UUID]error
	reads   map[uuid.UUID]int
	hidden  map[uuid.UUID]bool
}

func (r *rbRunRepo) GetRun(ctx context.Context, id uuid.UUID) (*run.Run, error) {
	r.faultMu.Lock()
	r.reads[id]++
	fault := r.faults[id]
	r.faultMu.Unlock()
	if fault != nil {
		return nil, fault
	}
	return r.attnRunRepo.GetRun(ctx, id)
}

func (r *rbRunRepo) ListRuns(ctx context.Context, fil run.ListRunsFilter) ([]*run.Run, error) {
	rows, err := r.attnRunRepo.ListRuns(ctx, fil)
	if err != nil {
		return nil, err
	}
	kept := rows[:0:0]
	for _, row := range rows {
		if !r.hidden[row.ID] {
			kept = append(kept, row)
		}
	}
	return kept, nil
}

// rbTerminal is the fixture for rounds seeded relative to the wall clock with the
// process started earlier, read through the store-faithful ListAll.
type rbTerminal struct {
	*rbFixture
	repo         *rbRunRepo
	faithful     *faithfulListAllAudit
	reviewCap    time.Duration
	processStart time.Time
	visibility   RepoVisibility
}

func newRBTerminal() *rbTerminal {
	f := newRBFixture()
	return &rbTerminal{
		rbFixture:    f,
		repo:         &rbRunRepo{attnRunRepo: f.runs, faults: map[uuid.UUID]error{}, reads: map[uuid.UUID]int{}, hidden: map[uuid.UUID]bool{}},
		faithful:     newFaithfulListAllAudit(f.audit),
		reviewCap:    time.Hour,
		processStart: time.Now().Add(-72 * time.Hour),
	}
}

func (f *rbTerminal) newServer() *Server {
	cfg := f.config()
	cfg.RunRepo, cfg.AuditRepo = f.repo, f.faithful
	cfg.ProcessStart = f.processStart
	cfg.ReviewBudget = planreview.ReviewBudget{Floor: time.Minute, Cap: f.reviewCap}
	if f.visibility != nil {
		cfg.AccountRoles = fakeAccountRoles{role: "member"}
		cfg.RepoVisibility = f.visibility
	}
	return New(cfg)
}

func rbAgo(age time.Duration) time.Time { return time.Now().UTC().Add(-age) }

// wireImplementStage makes the fixture's round stage a settled implement stage, the
// shape an otherwise eligible trace round rests on.
func (f *rbTerminal) wireImplementStage(runID uuid.UUID, state run.StageState) {
	f.wireRedispatch(runID)
	f.runs.mu.Lock()
	defer f.runs.mu.Unlock()
	for _, st := range f.runs.stagesByRun[runID] {
		if st.ID == rbRoundStage {
			st.Type, st.State = run.StageTypeImplement, state
		}
	}
}

func (f *rbTerminal) seedChildRun(childID, parentID uuid.UUID, repo string, state run.State) *run.Run {
	ru := f.runs.seed(childID, repo, state, attnT0, "")
	parent := parentID
	ru.DecomposedFrom = &parent
	return ru
}

// seedUnwiredRound seeds an advisory implement round no reviewer can re-dispatch.
func (f *rbTerminal) seedUnwiredRound(t *testing.T, runID uuid.UUID, seq int64, age time.Duration) {
	t.Helper()
	f.seedStarted(t, runID, "implement", seq, rbAgo(age), rbAdvisoryStarted("implement", 0))
}

func rbAssertOneInFlight(t *testing.T, got restartBlockersResponse, runID uuid.UUID, stage string) {
	t.Helper()
	if len(got.Items) != 1 || got.Items[0].RunID != runID.String() || got.Items[0].Reason != restartBlockerReviewInFlight || got.Items[0].Stage != stage {
		t.Fatalf("items = %+v, want one %s review_in_flight on %s", got.Items, stage, runID)
	}
}

// assertNoForeignIdentifiers reports only a boolean: a failure never prints a
// candidate's identity.
func assertNoForeignIdentifiers(t *testing.T, body string, ids ...uuid.UUID) {
	t.Helper()
	leaked := false
	for _, id := range ids {
		leaked = leaked || strings.Contains(body, id.String())
	}
	if leaked {
		t.Error("the response body names a candidate it must not name")
	}
}

// assertGenericUnavailable pins the static 503: no candidate, no raw cause.
func assertGenericUnavailable(t *testing.T, rec *httptest.ResponseRecorder, raw string, ids ...uuid.UUID) {
	t.Helper()
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	assertErrorCode(t, rec, "service_unavailable")
	body := rec.Body.String()
	assertNoForeignIdentifiers(t, body, ids...)
	if strings.Contains(body, "items") || strings.Contains(body, restartBlockerReviewInFlight) || strings.Contains(body, restartBlockerCheckFailed) {
		t.Error("the 503 carries a partial response")
	}
	if !strings.Contains(body, "could not complete the restart-blocker scan; retry shortly") {
		t.Error("the 503 lost its static message")
	}
	if raw != "" && strings.Contains(body, raw) {
		t.Error("the 503 carries the raw error text")
	}
}

func TestRestartBlockers_TerminalRunRound_Ineligible_IsBlocker(t *testing.T) {
	child, parent := attnID(50), attnID(51)
	t.Run("running_parent_listed", func(t *testing.T) {
		f := newRBTerminal()
		f.runs.seed(parent, "acme/app", run.StateRunning, attnT0, "")
		f.seedChildRun(child, parent, "acme/app", run.StateSucceeded)
		f.seedUnwiredRound(t, child, 1, 30*time.Minute)
		got := decodeRestartBlockers(t, getRestartBlockers(t, f.newServer(), anonymous()))
		rbAssertOneInFlight(t, got, child, "implement")
		if *got.Items[0].ConfiguredAgents != 2 || *got.Items[0].Landed != 0 {
			t.Errorf("counts = %d/%d, want 2/0", *got.Items[0].ConfiguredAgents, *got.Items[0].Landed)
		}
		if got.ScannedRuns != 2 {
			t.Errorf("scanned_runs = %d, want the running parent and the discovered child", got.ScannedRuns)
		}
	})
	t.Run("parent_outside_the_repo_view", func(t *testing.T) {
		f := newRBTerminal()
		f.visibility = newFakeRepoVisibility(map[string]bool{"acme/app": true})
		f.runs.seed(parent, "other/parent", run.StateRunning, attnT0, "")
		f.seedChildRun(child, parent, "acme/app", run.StateSucceeded)
		f.seedUnwiredRound(t, child, 1, 30*time.Minute)
		got := decodeRestartBlockers(t, getRestartBlockers(t, f.newServer(), memberIdentity()))
		rbAssertOneInFlight(t, got, child, "implement")
		if got.ScannedRuns != 1 {
			t.Errorf("scanned_runs = %d, want the discovered child alone", got.ScannedRuns)
		}
	})
}

func TestRestartBlockers_TerminalRunRound_Eligible_NotBlocker(t *testing.T) {
	f := newRBTerminal()
	runID := attnID(52)
	f.runs.seed(runID, "acme/app", run.StateSucceeded, attnT0, "")
	f.wireImplementStage(runID, run.StageStateSucceeded)
	f.seedStarted(t, runID, "implement", 1, rbAgo(30*time.Minute), rbAdvisoryStarted("implement", 0))
	s := f.newServer()
	assertRoundEligible(t, s, runID, "implement", true)
	got := decodeRestartBlockers(t, getRestartBlockers(t, s, anonymous()))
	if len(got.Items) != 0 || got.ScannedRuns != 1 {
		t.Fatalf("items = %+v scanned_runs = %d, want no item over one scanned run", got.Items, got.ScannedRuns)
	}
}

func TestRestartBlockers_TerminalRunRound_PastBudget_NotBlocker(t *testing.T) {
	runID := attnID(53)
	seed := func(t *testing.T, reviewCap time.Duration, configured int, age time.Duration) restartBlockersResponse {
		t.Helper()
		f := newRBTerminal()
		f.reviewCap = reviewCap
		f.runs.seed(runID, "acme/app", run.StateSucceeded, attnT0, "")
		started := rbAdvisoryStarted("implement", 0)
		started.ConfiguredAgents = configured
		f.seedStarted(t, runID, "implement", 1, rbAgo(age), started)
		return decodeRestartBlockers(t, getRestartBlockers(t, f.newServer(), anonymous()))
	}
	t.Run("past_cap_times_configured", func(t *testing.T) {
		got := seed(t, time.Hour, 2, 3*time.Hour)
		if len(got.Items) != 0 || got.ScannedRuns != 0 {
			t.Fatalf("items = %+v scanned_runs = %d, want nothing past Cap x configured", got.Items, got.ScannedRuns)
		}
	})
	t.Run("config_scaled", func(t *testing.T) {
		for _, tc := range []struct {
			name       string
			reviewCap  time.Duration
			configured int
			blocks     bool
		}{
			{"larger_cap_blocks", 2 * time.Hour, 2, true},
			{"smaller_cap_clear", time.Hour, 2, false},
			{"larger_configured_blocks", time.Hour, 4, true},
			{"smaller_configured_clear", time.Hour, 1, false},
		} {
			t.Run(tc.name, func(t *testing.T) {
				got := seed(t, tc.reviewCap, tc.configured, 3*time.Hour)
				if tc.blocks {
					rbAssertOneInFlight(t, got, runID, "implement")
					return
				}
				if len(got.Items) != 0 {
					t.Fatalf("items = %+v, want none", got.Items)
				}
			})
		}
	})
	t.Run("lookback_caps_the_budget", func(t *testing.T) {
		// A 2 x 20h budget stays inside the 24-hour discovery lookback only.
		got := seed(t, 20*time.Hour, 2, 25*time.Hour)
		if len(got.Items) != 0 {
			t.Fatalf("items = %+v, want none past the lookback", got.Items)
		}
	})
}

func TestRestartBlockers_TerminalRun_MixedRoundBudgets(t *testing.T) {
	runID := attnID(54)
	started := func(kind string, configured int) planreview.ReviewStartedPayload {
		p := rbAdvisoryStarted(kind, 0)
		p.ConfiguredAgents = configured
		return p
	}
	newRun := func() *rbTerminal {
		f := newRBTerminal()
		f.runs.seed(runID, "acme/app", run.StateSucceeded, attnT0, "")
		return f
	}
	t.Run("settled_plan_expired_implement", func(t *testing.T) {
		f := newRun()
		f.seedStarted(t, runID, "implement", 1, rbAgo(3*time.Hour), started("implement", 2))
		f.seedStarted(t, runID, "plan", 2, rbAgo(30*time.Minute), started("plan", 1))
		seedReviewAuditEntry(t, f.audit, runID, rbRoundStage, 3, rbAgo(20*time.Minute), "plan_reviewed", map[string]any{})
		got := decodeRestartBlockers(t, getRestartBlockers(t, f.newServer(), anonymous()))
		if len(got.Items) != 0 || got.ScannedRuns != 1 {
			t.Fatalf("items = %+v scanned_runs = %d, want a settled sibling to admit no expired round", got.Items, got.ScannedRuns)
		}
	})
	t.Run("expired_plan_live_implement", func(t *testing.T) {
		f := newRun()
		f.seedStarted(t, runID, "plan", 1, rbAgo(3*time.Hour), started("plan", 2))
		f.seedStarted(t, runID, "implement", 2, rbAgo(30*time.Minute), started("implement", 2))
		rbAssertOneInFlight(t, decodeRestartBlockers(t, getRestartBlockers(t, f.newServer(), anonymous())), runID, "implement")
	})
	t.Run("larger_plan_budget_smaller_implement_budget", func(t *testing.T) {
		f := newRun()
		f.seedStarted(t, runID, "plan", 1, rbAgo(2*time.Hour), started("plan", 3))
		f.seedStarted(t, runID, "implement", 2, rbAgo(2*time.Hour), started("implement", 1))
		rbAssertOneInFlight(t, decodeRestartBlockers(t, getRestartBlockers(t, f.newServer(), anonymous())), runID, "plan")
	})
}

// TestReviewRoundBlocker_WindowGuard is UNIT-BOUNDARY evidence: a direct call of the
// extracted function, not HTTP. Every earlier check passes on the expired arm and
// eligibility says no, so only the guard separates it from the control.
func TestReviewRoundBlocker_WindowGuard(t *testing.T) {
	child, parent := attnID(55), attnID(56)
	stage := rbStageKind(t, "implement")
	oneHour := func(int) time.Duration { return time.Hour }
	fixture := func(t *testing.T, age time.Duration) (*Server, *run.Run) {
		t.Helper()
		f := newRBTerminal()
		f.processStart = time.Now().Add(-3 * time.Hour)
		f.runs.seed(parent, "acme/app", run.StateRunning, attnT0, "")
		ru := f.seedChildRun(child, parent, "acme/app", run.StateSucceeded)
		f.seedUnwiredRound(t, child, 7, age)
		return f.newServer(), ru
	}
	t.Run("expired_actual_round", func(t *testing.T) {
		s, ru := fixture(t, 2*time.Hour)
		if got := s.reviewRoundBlocker(context.Background(), ru, stage, oneHour); got != nil {
			t.Fatalf("blocker = %+v, want nil: the actual latest round is past its window", got)
		}
	})
	t.Run("in_window_control", func(t *testing.T) {
		s, ru := fixture(t, 30*time.Minute)
		got := s.reviewRoundBlocker(context.Background(), ru, stage, oneHour)
		if got == nil || got.Reason != restartBlockerReviewInFlight {
			t.Fatalf("blocker = %+v, want review_in_flight: this fixture blocks inside its window", got)
		}
	})
	t.Run("nil_window_state_scan", func(t *testing.T) {
		s, ru := fixture(t, 2*time.Hour)
		got := s.reviewRoundBlocker(context.Background(), ru, stage, nil)
		if got == nil || got.Reason != restartBlockerReviewInFlight {
			t.Fatalf("blocker = %+v, want review_in_flight: the state scan bounds nothing", got)
		}
	})
	t.Run("expired_pending_handoff", func(t *testing.T) {
		s, ru := fixture(t, 2*time.Hour)
		key := pendingRedispatchKey{runID: child, stage: "implement", seq: 7}
		markRedispatchPending(key)
		t.Cleanup(func() { clearRedispatchPending(key) })
		got := s.reviewRoundBlocker(context.Background(), ru, stage, oneHour)
		if got == nil || got.Reason != restartBlockerReviewInFlight {
			t.Fatalf("blocker = %+v, want review_in_flight: the pending check runs ahead of the guard", got)
		}
	})
}

// rbRefusalArm leaves one eligibility refusal standing on a wired implement round.
type rbRefusalArm struct {
	name    string
	unwired bool
	mutate  func(*planreview.ReviewStartedPayload)
	after   func(t *testing.T, f *rbTerminal, runID uuid.UUID, seq int64)
}

func rbRefusalArms() []rbRefusalArm {
	return []rbRefusalArm{
		{name: "plain_advisory"},
		{name: "gating", mutate: func(p *planreview.ReviewStartedPayload) { p.Authority = planreview.AuthorityGating }},
		{name: "reviewer_unwired", unwired: true},
		{name: "depth_cap_reached", mutate: func(p *planreview.ReviewStartedPayload) { p.RedispatchDepth = maxReviewRoundRedispatches }},
		{name: "already_redispatched", after: func(t *testing.T, f *rbTerminal, runID uuid.UUID, seq int64) {
			seedReviewAuditEntry(t, f.audit, runID, rbRoundStage, seq+1, rbAgo(20*time.Minute), categoryReviewRoundRedispatched, reviewRoundRedispatchedPayload{Stage: "implement", OrphanedRoundSequence: seq})
		}},
	}
}

func (f *rbTerminal) seedRefusal(t *testing.T, runID uuid.UUID, arm rbRefusalArm) {
	t.Helper()
	f.wireImplementStage(runID, run.StageStateSucceeded)
	f.redispatchWired = !arm.unwired
	started := rbAdvisoryStarted("implement", 0)
	if arm.mutate != nil {
		arm.mutate(&started)
	}
	f.seedStarted(t, runID, "implement", 1, rbAgo(30*time.Minute), started)
	if arm.after != nil {
		arm.after(t, f, runID, 1)
	}
}

func TestRestartBlockers_TerminalRunRound_NoConsumer_NotBlocker(t *testing.T) {
	runID, parent := attnID(57), attnID(58)
	for _, state := range []struct {
		name    string
		arrange func(f *rbTerminal)
	}{
		{"cancelled", func(f *rbTerminal) { f.runs.seed(runID, "acme/app", run.StateCancelled, attnT0, "") }},
		{"succeeded_child_of_terminal_parent", func(f *rbTerminal) {
			f.runs.seed(parent, "acme/app", run.StateFailed, attnT0, "")
			f.seedChildRun(runID, parent, "acme/app", run.StateSucceeded)
		}},
	} {
		for _, arm := range rbRefusalArms() {
			t.Run(state.name+"/"+arm.name, func(t *testing.T) {
				f := newRBTerminal()
				state.arrange(f)
				f.seedRefusal(t, runID, arm)
				got := decodeRestartBlockers(t, getRestartBlockers(t, f.newServer(), anonymous()))
				if len(got.Items) != 0 {
					t.Fatalf("items = %+v, want none: no consumer can use this round's verdict", got.Items)
				}
				if got.ScannedRuns != 1 {
					t.Errorf("scanned_runs = %d, want the discovered run", got.ScannedRuns)
				}
			})
		}
	}
}

func TestRestartBlockers_TerminalRunRound_Failed_KeepsRefusal(t *testing.T) {
	runID := attnID(59)
	newFailed := func() *rbTerminal {
		f := newRBTerminal()
		f.runs.seed(runID, "acme/app", run.StateFailed, attnT0, "")
		return f
	}
	for _, arm := range rbRefusalArms()[1:3] {
		t.Run(arm.name, func(t *testing.T) {
			f := newFailed()
			f.seedRefusal(t, runID, arm)
			rbAssertOneInFlight(t, decodeRestartBlockers(t, getRestartBlockers(t, f.newServer(), anonymous())), runID, "implement")
		})
	}
	t.Run("implement_stage_terminal", func(t *testing.T) {
		f := newFailed()
		f.wireImplementStage(runID, run.StageStateFailed)
		f.seedStarted(t, runID, "implement", 1, rbAgo(30*time.Minute), rbAdvisoryStarted("implement", 0))
		rbAssertOneInFlight(t, decodeRestartBlockers(t, getRestartBlockers(t, f.newServer(), anonymous())), runID, "implement")
	})
	t.Run("eligible_no_item", func(t *testing.T) {
		f := newFailed()
		f.seedRefusal(t, runID, rbRefusalArms()[0])
		s := f.newServer()
		assertRoundEligible(t, s, runID, "implement", true)
		if got := decodeRestartBlockers(t, getRestartBlockers(t, s, anonymous())); len(got.Items) != 0 {
			t.Fatalf("items = %+v, want none for an eligible failed-run round", got.Items)
		}
	})
	t.Run("past_budget_no_item", func(t *testing.T) {
		f := newFailed()
		f.seedStarted(t, runID, "implement", 1, rbAgo(3*time.Hour), rbAdvisoryStarted("implement", 0))
		if got := decodeRestartBlockers(t, getRestartBlockers(t, f.newServer(), anonymous())); len(got.Items) != 0 {
			t.Fatalf("items = %+v, want none past the round's budget", got.Items)
		}
	})
}

func TestRestartBlockers_TerminalRunRound_ConsumerLive_KeepsRefusal(t *testing.T) {
	runID, parent := attnID(60), attnID(61)
	for _, state := range []struct {
		name    string
		arrange func(f *rbTerminal)
	}{
		{"succeeded_top_level", func(f *rbTerminal) { f.runs.seed(runID, "acme/app", run.StateSucceeded, attnT0, "") }},
		{"succeeded_child_of_live_parent", func(f *rbTerminal) {
			f.runs.seed(parent, "acme/app", run.StateRunning, attnT0, "")
			f.seedChildRun(runID, parent, "acme/app", run.StateSucceeded)
		}},
	} {
		for _, arm := range rbRefusalArms()[1:] {
			t.Run(state.name+"/"+arm.name, func(t *testing.T) {
				f := newRBTerminal()
				state.arrange(f)
				f.seedRefusal(t, runID, arm)
				rbAssertOneInFlight(t, decodeRestartBlockers(t, getRestartBlockers(t, f.newServer(), anonymous())), runID, "implement")
			})
		}
	}
}

func TestRestartBlockers_TerminalRunRound_PendingRedispatchIsBlocker(t *testing.T) {
	runID := attnID(62)
	for _, pending := range []bool{true, false} {
		t.Run(map[bool]string{true: "pending", false: "not_pending"}[pending], func(t *testing.T) {
			f := newRBTerminal()
			f.processStart = time.Now().Add(-time.Hour)
			f.runs.seed(runID, "acme/app", run.StateSucceeded, attnT0, "")
			f.wireImplementStage(runID, run.StageStateSucceeded)
			f.seedStarted(t, runID, "implement", 7, rbAgo(30*time.Hour), rbAdvisoryStarted("implement", 0))
			if pending {
				key := pendingRedispatchKey{runID: runID, stage: "implement", seq: 7}
				markRedispatchPending(key)
				t.Cleanup(func() { clearRedispatchPending(key) })
			}
			got := decodeRestartBlockers(t, getRestartBlockers(t, f.newServer(), anonymous()))
			if pending {
				rbAssertOneInFlight(t, got, runID, "implement")
				return
			}
			if len(got.Items) != 0 || got.ScannedRuns != 0 {
				t.Fatalf("items = %+v scanned_runs = %d, want the same old round with no handoff dropped", got.Items, got.ScannedRuns)
			}
		})
	}
}

func TestRestartBlockers_TerminalRun_CandidateVisibility(t *testing.T) {
	candidate, other, parent := attnID(63), attnID(64), attnID(65)
	acctA, acctB := uuid.NewString(), uuid.NewString()
	t.Run("repo_inaccessible", func(t *testing.T) {
		f := newRBTerminal()
		f.visibility = newFakeRepoVisibility(map[string]bool{"alpha/one": true})
		f.runs.seed(candidate, "beta/two", run.StateSucceeded, attnT0, "")
		f.seedUnwiredRound(t, candidate, 1, 30*time.Minute)
		f.runs.seed(other, "alpha/one", run.StateSucceeded, attnT0, "")
		f.seedUnwiredRound(t, other, 2, 30*time.Minute)
		rec := getRestartBlockers(t, f.newServer(), memberIdentity())
		got := decodeRestartBlockers(t, rec)
		assertNoForeignIdentifiers(t, rec.Body.String(), candidate)
		if len(got.Items) != 1 || got.Items[0].RunID != other.String() || got.Items[0].Reason != restartBlockerReviewInFlight {
			t.Errorf("%d items; want the accessible run's one review_in_flight alone", len(got.Items))
		}
		if got.ScannedRuns != 1 {
			t.Errorf("scanned_runs = %d, want the accessible run alone", got.ScannedRuns)
		}
	})
	t.Run("cross_account_null_row", func(t *testing.T) {
		// The row reaches the handler through the account window because it is NULL;
		// the run behind it belongs to another account.
		f := newRBTerminal()
		f.runs.seed(candidate, "acme/app", run.StateSucceeded, attnT0, "").AccountID = acctB
		f.seedUnwiredRound(t, candidate, 1, 30*time.Minute)
		rec := getRestartBlockers(t, f.newServer(), Identity{Subject: "github:op", AccountID: acctA})
		got := decodeRestartBlockers(t, rec)
		assertNoForeignIdentifiers(t, rec.Body.String(), candidate)
		if len(got.Items) != 0 || got.ScannedRuns != 0 {
			t.Errorf("foreign run contributed: %d items, %d scanned", len(got.Items), got.ScannedRuns)
		}
		if f.repo.reads[candidate] == 0 {
			t.Error("setup: the NULL-account row never reached the handler")
		}
		for _, call := range f.faithful.recorded() {
			if call.AccountID != acctA {
				t.Errorf("ListAll account = %q, want the caller's", call.AccountID)
			}
		}
	})
	// A state-scan item exists when the candidate fails, so a discarded partial
	// response is observable.
	seedPartial := func(t *testing.T, f *rbTerminal) {
		t.Helper()
		f.runs.seed(other, "acme/app", run.StateRunning, attnT0, "")
		f.seedReviewRound(t, other, "plan", 90, rbAgo(10*time.Minute), 2)
	}
	callers := []struct {
		name   string
		member bool
		id     Identity
	}{{"anonymous", false, anonymous()}, {"member", true, memberIdentity()}}
	for _, caller := range callers {
		t.Run("run_read_error/"+caller.name, func(t *testing.T) {
			f := newRBTerminal()
			if caller.member {
				f.visibility = newFakeRepoVisibility(map[string]bool{"acme/app": true})
			}
			seedPartial(t, f)
			f.runs.seed(candidate, "acme/app", run.StateSucceeded, attnT0, "")
			f.seedUnwiredRound(t, candidate, 1, 30*time.Minute)
			const raw = "private candidate read failure"
			f.repo.faults[candidate] = errors.New(raw)
			assertGenericUnavailable(t, getRestartBlockers(t, f.newServer(), caller.id), raw, candidate, other)
		})
		t.Run("run_missing/"+caller.name, func(t *testing.T) {
			f := newRBTerminal()
			if caller.member {
				f.visibility = newFakeRepoVisibility(map[string]bool{"acme/app": true})
			}
			seedPartial(t, f)
			f.seedUnwiredRound(t, candidate, 1, 30*time.Minute) // no such run
			assertGenericUnavailable(t, getRestartBlockers(t, f.newServer(), caller.id), run.ErrNotFound.Error(), candidate, other)
		})
	}
	t.Run("visibility_store_fault", func(t *testing.T) {
		f := newRBTerminal()
		f.visibility = fakeErrOnRepoVisibility{allowed: "alpha/one", failing: "beta/two"}
		f.runs.seed(other, "alpha/one", run.StateRunning, attnT0, "")
		f.seedReviewRound(t, other, "plan", 90, rbAgo(10*time.Minute), 2)
		f.runs.seed(candidate, "beta/two", run.StateSucceeded, attnT0, "")
		f.seedUnwiredRound(t, candidate, 1, 30*time.Minute)
		rec := getRestartBlockers(t, f.newServer(), memberIdentity())
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503", rec.Code)
		}
		assertErrorCode(t, rec, "service_unavailable")
		assertNoForeignIdentifiers(t, rec.Body.String(), candidate, other)
	})
	t.Run("invisible_run_unreadable_parent", func(t *testing.T) {
		f := newRBTerminal()
		f.visibility = newFakeRepoVisibility(map[string]bool{"alpha/one": true})
		f.seedChildRun(candidate, parent, "beta/two", run.StateSucceeded)
		f.seedUnwiredRound(t, candidate, 1, 30*time.Minute)
		f.repo.faults[parent] = errors.New("private parent read failure")
		rec := getRestartBlockers(t, f.newServer(), memberIdentity())
		got := decodeRestartBlockers(t, rec)
		assertNoForeignIdentifiers(t, rec.Body.String(), candidate, parent)
		if len(got.Items) != 0 || got.ScannedRuns != 0 {
			t.Errorf("invisible run contributed: %d items, %d scanned", len(got.Items), got.ScannedRuns)
		}
		if n := f.repo.reads[parent]; n != 0 {
			t.Errorf("parent reads = %d, want none before visibility is established", n)
		}
	})
	t.Run("visible_child_parent_read_error", func(t *testing.T) {
		f := newRBTerminal()
		f.seedChildRun(candidate, parent, "acme/app", run.StateSucceeded)
		f.seedUnwiredRound(t, candidate, 1, 30*time.Minute)
		f.repo.faults[parent] = errors.New("parent read failure")
		rec := getRestartBlockers(t, f.newServer(), anonymous())
		got := decodeRestartBlockers(t, rec)
		assertNoForeignIdentifiers(t, rec.Body.String(), parent)
		if len(got.Items) != 1 || got.Items[0].RunID != candidate.String() || got.Items[0].Reason != restartBlockerCheckFailed || got.Items[0].Stage != "implement" {
			t.Errorf("%d items; want one implement check_failed naming only the child", len(got.Items))
		}
	})
	t.Run("null_run_account_visible", func(t *testing.T) {
		f := newRBTerminal()
		f.runs.seed(candidate, "acme/app", run.StateSucceeded, attnT0, "")
		f.seedUnwiredRound(t, candidate, 1, 30*time.Minute)
		listed, err := f.repo.ListRuns(context.Background(), run.ListRunsFilter{State: string(run.StateSucceeded), AccountID: acctA, Limit: 10})
		if err != nil || len(listed) != 1 {
			t.Fatalf("setup: ListRuns = %d rows, %v; want the NULL-account run", len(listed), err)
		}
		rbAssertOneInFlight(t, decodeRestartBlockers(t, getRestartBlockers(t, f.newServer(), Identity{Subject: "github:op", AccountID: acctA})), candidate, "implement")
	})
	t.Run("anonymous_matches_listruns_posture", func(t *testing.T) {
		f := newRBTerminal()
		f.runs.seed(candidate, "acme/app", run.StateSucceeded, attnT0, "").AccountID = acctB
		f.seedUnwiredRound(t, candidate, 1, 30*time.Minute)
		f.runs.seed(other, "acme/app", run.StateSucceeded, attnT0, "")
		f.seedUnwiredRound(t, other, 2, 30*time.Minute)
		f.runs.seed(parent, "acme/app", run.StateSucceeded, attnT0, "")
		f.seedUnwiredRound(t, parent, 3, 3*time.Hour) // not qualifying: past its window
		listed, err := f.repo.ListRuns(context.Background(), run.ListRunsFilter{State: string(run.StateSucceeded), Limit: 10})
		if err != nil || len(listed) != 3 {
			t.Fatalf("setup: ListRuns = %d rows, %v; want the anonymous posture to admit all three", len(listed), err)
		}
		got := decodeRestartBlockers(t, getRestartBlockers(t, f.newServer(), anonymous()))
		emitted := make([]string, 0, len(got.Items))
		for _, item := range got.Items {
			emitted = append(emitted, item.RunID)
		}
		slices.Sort(emitted)
		want := []string{candidate.String(), other.String()}
		slices.Sort(want)
		if !slices.Equal(emitted, want) {
			t.Fatalf("emitted runs differ from the qualifying set the anonymous posture admits")
		}
	})
}

func TestAccountWindowAdmits_Modes(t *testing.T) {
	a, b := uuid.NewString(), uuid.NewString()
	for _, tc := range []struct {
		name          string
		filter, owner string
		want          bool
	}{
		{"empty_filter", "", b, true},
		{"malformed_filter", "not-a-uuid", b, true},
		{"equal", a, a, true},
		{"equal_other_spelling", strings.ToUpper(a), a, true},
		{"other", a, b, false},
		{"null_run_account", a, "", true},
		{"unparseable_run_account", a, "not-a-uuid", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := accountWindowAdmits(tc.filter, tc.owner); got != tc.want {
				t.Errorf("accountWindowAdmits = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRestartBlockers_TerminalRun_ReadModes(t *testing.T) {
	runID := attnID(66)
	t.Run("list_all_error", func(t *testing.T) {
		f := newRBTerminal()
		f.runs.seed(runID, "acme/app", run.StateRunning, attnT0, "")
		f.seedReviewRound(t, runID, "plan", 1, rbAgo(10*time.Minute), 2)
		const raw = "private list failure"
		f.faithful.listAllErr = errors.New(raw)
		assertGenericUnavailable(t, getRestartBlockers(t, f.newServer(), anonymous()), raw, runID)
	})
	t.Run("flipped_between_scans", func(t *testing.T) {
		f := newRBTerminal()
		ru := f.runs.seed(runID, "acme/app", run.StateRunning, attnT0, "")
		f.seedReviewRound(t, runID, "plan", 1, rbAgo(10*time.Minute), 2)
		s := f.newServer()
		// The state scan listed it running; by discovery it reads terminal.
		repo := &rbFlipRepo{rbRunRepo: f.repo, flip: func() { ru.State = run.StateSucceeded }}
		s.cfg.RunRepo = repo
		got := decodeRestartBlockers(t, getRestartBlockers(t, s, anonymous()))
		rbAssertOneInFlight(t, got, runID, "plan")
		if got.ScannedRuns != 1 {
			t.Errorf("scanned_runs = %d, want the run counted once", got.ScannedRuns)
		}
	})
	t.Run("non_terminal_candidate_skipped", func(t *testing.T) {
		f := newRBTerminal()
		f.runs.seed(runID, "acme/app", run.StateRunning, attnT0, "")
		f.repo.hidden[runID] = true // created after its state page was read
		f.seedReviewRound(t, runID, "plan", 1, rbAgo(10*time.Minute), 2)
		got := decodeRestartBlockers(t, getRestartBlockers(t, f.newServer(), anonymous()))
		if len(got.Items) != 0 || got.ScannedRuns != 0 {
			t.Fatalf("items = %+v scanned_runs = %d, want a non-terminal candidate left to the state scan", got.Items, got.ScannedRuns)
		}
		if n := f.repo.reads[runID]; n != 1 {
			t.Errorf("candidate reads = %d, want 1", n)
		}
	})
}

// rbFlipRepo flips a run's state once both state pages have been listed.
type rbFlipRepo struct {
	*rbRunRepo
	flip  func()
	lists int
}

func (r *rbFlipRepo) ListRuns(ctx context.Context, fil run.ListRunsFilter) ([]*run.Run, error) {
	rows, err := r.rbRunRepo.ListRuns(ctx, fil)
	if r.lists++; r.lists == len(reconcileOrphanedReviewStates) {
		r.flip()
	}
	return rows, err
}

func TestRestartBlockers_TerminalRun_Truncated(t *testing.T) {
	runID := attnID(67)
	seedRows := func(t *testing.T, f *rbTerminal, count int, age time.Duration) {
		t.Helper()
		for i := 0; i < count; i++ {
			f.seedStarted(t, runID, "plan", int64(i+1), rbAgo(age), planreview.ReviewStartedPayload{Authority: planreview.AuthorityGating})
		}
	}
	for _, tc := range []struct {
		name  string
		count int
		age   time.Duration
		want  bool
	}{
		{"cap_reached_inside_bound", terminalRoundScanLimit, 30 * time.Minute, true},
		{"cap_reached_past_bound", terminalRoundScanLimit, 30 * time.Hour, false},
		{"below_cap", terminalRoundScanLimit - 1, 30 * time.Minute, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRBTerminal()
			f.runs.seed(runID, "acme/app", run.StateSucceeded, attnT0, "")
			seedRows(t, f, tc.count, tc.age)
			got := decodeRestartBlockers(t, getRestartBlockers(t, f.newServer(), anonymous()))
			if got.Truncated != tc.want {
				t.Errorf("truncated = %v, want %v", got.Truncated, tc.want)
			}
			for _, call := range f.faithful.recorded() {
				if call.Limit != 500 {
					t.Errorf("ListAll limit = %d, want 500", call.Limit)
				}
			}
		})
	}
}

func TestRestartBlockers_TerminalRun_ReadBound(t *testing.T) {
	f := newRBTerminal()
	both, one, expired := attnID(68), attnID(69), attnID(70)
	settle := func(runID uuid.UUID, kind string, seq int64) {
		seedReviewAuditEntry(t, f.audit, runID, rbRoundStage, seq, rbAgo(time.Minute), kind+"_reviewed", map[string]any{})
	}
	settled := planreview.ReviewStartedPayload{ConfiguredAgents: 1, Authority: planreview.AuthorityGating}
	f.runs.seed(both, "acme/app", run.StateSucceeded, attnT0, "")
	f.seedStarted(t, both, "plan", 1, rbAgo(30*time.Minute), settled)
	settle(both, "plan", 2)
	f.seedStarted(t, both, "implement", 3, rbAgo(30*time.Minute), settled)
	settle(both, "implement", 4)
	f.runs.seed(one, "acme/app", run.StateFailed, attnT0, "")
	f.seedStarted(t, one, "plan", 5, rbAgo(30*time.Minute), settled)
	settle(one, "plan", 6)
	f.runs.seed(expired, "acme/app", run.StateCancelled, attnT0, "")
	f.seedStarted(t, expired, "implement", 7, rbAgo(3*time.Hour), settled)
	before := f.runs.getRunCalls

	rec := httptest.NewRecorder()
	req := withIdentity(httptest.NewRequest(http.MethodGet, "/v0/restart-blockers", nil), anonymous())
	f.newServer().handleListRestartBlockers(rec, req)
	got := decodeRestartBlockers(t, rec)
	if len(got.Items) != 0 || got.ScannedRuns != 2 {
		t.Fatalf("items = %+v scanned_runs = %d, want two scanned runs and no item", got.Items, got.ScannedRuns)
	}
	if n := len(f.faithful.recorded()); n != 2 {
		t.Errorf("ListAll calls = %d, want 2", n)
	}
	if n := f.repo.reads[expired]; n != 0 {
		t.Errorf("expired-row run reads = %d, want 0", n)
	}
	if n := f.runs.getRunCalls - before; n != 2 {
		t.Errorf("GetRun calls = %d, want one per distinct in-window run", n)
	}
	// The anonymous route, through the real mux, keeps its 200.
	viaMux := httptest.NewRecorder()
	f.newServer().Handler().ServeHTTP(viaMux, httptest.NewRequest(http.MethodGet, "/v0/restart-blockers", nil))
	if viaMux.Code != http.StatusOK {
		t.Errorf("anonymous route status = %d, want 200", viaMux.Code)
	}
}

// --- rehomed real-store visibility suites (§33): pgtest skip, real roles and accounts

func TestDiscoverTerminalRunRounds_NullAccountCandidate_PG(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	runRepo := run.NewPostgresRepository(pool)
	auditRepo := audit.NewPostgresRepository(pool)
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: runRepo, AuditRepo: auditRepo})
	accountA, accountB := seedPGAccount(t, pool), seedPGAccount(t, pool)
	runID, stageID := seedPGReviewRun(t, runRepo)
	// The started row lands while the run is untenanted, so its account column is NULL.
	appendPGAudit(t, auditRepo, runID, &stageID, terminalRoundAge(time.Hour), "plan_review_started", advisoryPlanStarted())
	if _, err := pool.Exec(ctx, `UPDATE runs SET account_id = $1 WHERE id = $2`, accountB, runID); err != nil {
		t.Fatalf("setup: set run account: %v", err)
	}
	scan, err := s.discoverTerminalRunRounds(ctx, accountA.String(), constantWindow(terminalRoundDiscoveryBound))
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(scan.candidates) != 1 || scan.candidates[0].runID != runID {
		t.Fatalf("real query for account A gave %d candidates, want the NULL-account row", len(scan.candidates))
	}
	ru, err := runRepo.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("read run: %v", err)
	}
	if ru.AccountID != accountB.String() {
		t.Fatal("setup: the run did not move to account B")
	}
	visible, err := candidateVisible(ctx, nil, accountA.String(), ru)
	if err != nil || visible {
		t.Fatalf("candidateVisible = %v, %v; a NULL-account row is never evidence of visibility", visible, err)
	}
	if visible, err := candidateVisible(ctx, nil, accountB.String(), ru); err != nil || !visible {
		t.Fatalf("candidateVisible for the owner = %v, %v; want the positive control", visible, err)
	}
}

func TestAccountWindowMatchesListRuns_PG(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	runRepo := run.NewPostgresRepository(pool)
	accountA, accountB := seedPGAccount(t, pool), seedPGAccount(t, pool)
	owners := map[uuid.UUID]*uuid.UUID{}
	for _, owner := range []*uuid.UUID{&accountA, &accountB, nil} {
		id, _ := seedPGReviewRun(t, runRepo)
		owners[id] = owner
		if owner != nil {
			if _, err := pool.Exec(ctx, `UPDATE runs SET account_id = $1 WHERE id = $2`, *owner, id); err != nil {
				t.Fatalf("setup: set run account: %v", err)
			}
		}
	}
	for _, filter := range []string{"", "not-a-uuid", accountA.String(), accountB.String(), strings.ToUpper(accountA.String()), uuid.NewString()} {
		listed, err := runRepo.ListRuns(ctx, run.ListRunsFilter{AccountID: filter, Limit: 10})
		if err != nil {
			t.Fatalf("ListRuns(%q): %v", filter, err)
		}
		inList := map[uuid.UUID]bool{}
		for _, ru := range listed {
			inList[ru.ID] = true
		}
		for id := range owners {
			ru, err := runRepo.GetRun(ctx, id)
			if err != nil {
				t.Fatalf("GetRun: %v", err)
			}
			if got := accountWindowAdmits(filter, ru.AccountID); got != inList[id] {
				t.Errorf("filter %q, run account %q: accountWindowAdmits = %v, ListRuns = %v", filter, ru.AccountID, got, inList[id])
			}
		}
	}
}

func TestDiscoverTerminalRunRounds_RLSProbeRole_PG(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	dbURL := pgtest.NewURL(t)
	admin, err := postgres.Connect(ctx, dbURL)
	if err != nil {
		t.Fatalf("setup: connect admin pool: %v", err)
	}
	t.Cleanup(admin.Close)
	runRepo := run.NewPostgresRepository(admin)
	adminAudit := audit.NewPostgresRepository(admin)
	accountA, accountB := seedPGAccount(t, admin), seedPGAccount(t, admin)
	setOwner := func(id, owner uuid.UUID) {
		t.Helper()
		if _, err := admin.Exec(ctx, `UPDATE runs SET account_id = $1 WHERE id = $2`, owner, id); err != nil {
			t.Fatalf("setup: set run account: %v", err)
		}
	}
	// foreignRow's row carries account B; nullOnForeign's row lands NULL, then its run
	// moves to account B.
	foreignRow, foreignStage := seedPGReviewRun(t, runRepo)
	setOwner(foreignRow, accountB)
	appendPGAudit(t, adminAudit, foreignRow, &foreignStage, terminalRoundAge(time.Hour), "plan_review_started", advisoryPlanStarted())
	nullOnForeign, nullStage := seedPGReviewRun(t, runRepo)
	appendPGAudit(t, adminAudit, nullOnForeign, &nullStage, terminalRoundAge(2*time.Hour), "plan_review_started", advisoryPlanStarted())
	setOwner(nullOnForeign, accountB)

	// CREATE ROLE is cluster-wide, so the name is unique and dropped in cleanup, the way
	// backend/internal/postgres/rls_test.go builds its own (unexported) fixture.
	role := "fh_rb_probe_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	const password = "fishhawk-rb-probe"
	for _, stmt := range []string{
		fmt.Sprintf("CREATE ROLE %s LOGIN PASSWORD '%s' NOSUPERUSER NOBYPASSRLS", role, password),
		"GRANT USAGE ON SCHEMA public TO " + role,
		"GRANT SELECT ON ALL TABLES IN SCHEMA public TO " + role,
	} {
		if _, err := admin.Exec(ctx, stmt); err != nil {
			t.Fatalf("setup: %s: %v", strings.Fields(stmt)[0], err)
		}
	}
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.Background(), 30*time.Second)
		defer done()
		conn, err := pgx.Connect(cleanup, dbURL)
		if err != nil {
			return // best effort: the per-test database is dropped regardless
		}
		defer func() { _ = conn.Close(cleanup) }()
		_, _ = conn.Exec(cleanup, "DROP OWNED BY "+role)
		_, _ = conn.Exec(cleanup, "DROP ROLE IF EXISTS "+role)
	})
	probeURL, err := url.Parse(dbURL)
	if err != nil {
		t.Fatalf("setup: parse url: %v", err)
	}
	probeURL.User = url.UserPassword(role, password)
	probe, err := pgxpool.New(ctx, probeURL.String())
	if err != nil {
		t.Fatalf("setup: connect probe pool: %v", err)
	}
	t.Cleanup(probe.Close)
	var super, bypass bool
	if err := probe.QueryRow(ctx, `SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&super, &bypass); err != nil {
		t.Fatalf("setup: read probe role: %v", err)
	}
	if super || bypass {
		t.Fatalf("setup: probe rolsuper=%v rolbypassrls=%v, RLS would not engage", super, bypass)
	}

	candidates := func(t *testing.T, repo audit.Repository, filter string) map[uuid.UUID]bool {
		t.Helper()
		s := New(Config{Addr: "127.0.0.1:0", RunRepo: runRepo, AuditRepo: repo})
		scan, err := s.discoverTerminalRunRounds(ctx, filter, constantWindow(terminalRoundDiscoveryBound))
		if err != nil {
			t.Fatalf("discover: %v", err)
		}
		out := map[uuid.UUID]bool{}
		for _, c := range scan.candidates {
			out[c.runID] = true
		}
		return out
	}
	// The admin control: without RLS the unfiltered query reads both rows, so the policy
	// is what removes account B's row below.
	if got := candidates(t, adminAudit, ""); !got[foreignRow] || !got[nullOnForeign] {
		t.Fatalf("setup: the admin read did not see both rows")
	}
	for _, filter := range []string{"", accountA.String()} {
		got := candidates(t, audit.NewPostgresRepository(probe), filter)
		if got[foreignRow] {
			t.Errorf("filter %q: account B's row became a candidate under the policy", filter)
		}
		if !got[nullOnForeign] {
			t.Errorf("filter %q: the NULL-account row did not appear under the policy", filter)
		}
	}
	ru, err := runRepo.GetRun(ctx, nullOnForeign)
	if err != nil {
		t.Fatalf("read run: %v", err)
	}
	if visible, err := candidateVisible(ctx, nil, accountA.String(), ru); err != nil || visible {
		t.Fatalf("candidateVisible = %v, %v; the NULL-account row on account B's run must be rejected", visible, err)
	}
}

// --- the ordinary terminal edge (§41) and alternative outcomes (§43), real stores ---

// rbFailedEdgePark adds one more park over edgeParkRepo: the first failed edge of an
// owner's pass, which lets its reject commit while its edge has not landed.
type rbFailedEdgePark struct {
	*edgeParkRepo
	armed    atomic.Bool
	reached  chan struct{}
	release  chan struct{}
	openOnce sync.Once
}

func (r *rbFailedEdgePark) open() { r.openOnce.Do(func() { close(r.release) }) }

func (r *rbFailedEdgePark) TransitionStageFromAttempt(ctx context.Context, id uuid.UUID, from, to run.StageState, expected string, completion *run.StageCompletion) (*run.Stage, error) {
	if to == run.StageStateFailed && r.armed.CompareAndSwap(true, false) {
		close(r.reached)
		select {
		case <-r.release:
		case <-ctx.Done():
		}
	}
	return r.edgeParkRepo.TransitionStageFromAttempt(ctx, id, from, to, expected, completion)
}

// rbRace drives an ordinary request against another request's pass over real stores.
// A setup wait failing begins "setup:", which is never a ledger RED.
type rbRace struct {
	*pgRouteSeam
	ownerEdge *rbFailedEdgePark
}

func newRBRace(t *testing.T, requiresApproval bool, steps ...reviewStep) *rbRace {
	t.Helper()
	p := newPGRouteSeam(t, specImplementGatingReviewers, requiresApproval, steps...)
	layer := &rbFailedEdgePark{edgeParkRepo: p.edge, reached: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(layer.open)
	p.s.cfg.RunRepo = layer
	t.Cleanup(p.s.waitBackgroundReviews)
	return &rbRace{pgRouteSeam: p, ownerEdge: layer}
}

// park posts body and waits until it has parked at entry to its terminal store call,
// past every outside read and holding no lock.
func (o *rbRace) park(t *testing.T, body []byte) <-chan *httptest.ResponseRecorder {
	t.Helper()
	o.edge.parkNext()
	done := o.postAsync(body)
	waitFor(t, o.edge.reached, "setup: the request parked at its edge")
	return done
}

func (o *rbRace) ordinary(t *testing.T) []byte {
	t.Helper()
	return mustPRBody(t, prSeamHead)
}

// ownerRejectsParked runs another upload's real pass until its reject is committed and
// its failed edge is parked, and proves the commit on another connection.
func (o *rbRace) ownerRejectsParked(t *testing.T) <-chan *httptest.ResponseRecorder {
	t.Helper()
	o.ownerEdge.armed.Store(true)
	owner := o.postAsync(o.body(t))
	waitFor(t, o.ownerEdge.reached, "setup: the owner's failed edge parked")
	if err := recordedRejectInRows(o.verdictRows(t)); err != nil {
		t.Fatal("setup: no committed reject at this head")
	}
	return owner
}

func (o *rbRace) wantStatus(t *testing.T, done <-chan *httptest.ResponseRecorder, what string, status int) *httptest.ResponseRecorder {
	t.Helper()
	w := awaitResponse(t, done, what)
	if w.Code != status {
		t.Fatalf("%s = %d, want %d:\n%s", what, w.Code, status, w.Body.String())
	}
	return w
}

func (o *rbRace) assertNoSuccessEdge(t *testing.T) {
	t.Helper()
	if n := o.edge.advances.Load(); n != 0 {
		t.Errorf("successful edges = %d, want 0", n)
	}
	if got := o.committed(t).State; got == run.StageStateAwaitingApproval || got == run.StageStateSucceeded {
		t.Errorf("committed stage = %q, the upload advanced over recorded evidence", got)
	}
}

// postCancellable posts body with a request context the test can end.
func (o *rbRace) postCancellable(ctx context.Context, body []byte) <-chan *httptest.ResponseRecorder {
	done := make(chan *httptest.ResponseRecorder, 1)
	target := fmt.Sprintf("/v0/runs/%s/pull-request?stage_id=%s", o.runID, o.stageID)
	sig := hex.EncodeToString(ed25519.Sign(o.priv, signing.ComputeMessage(body)))
	go func() {
		req := httptest.NewRequest(http.MethodPost, target, bytes.NewReader(body)).WithContext(ctx)
		req.Header.Set("X-Fishhawk-Signature", sig)
		w := httptest.NewRecorder()
		o.s.Handler().ServeHTTP(w, req)
		done <- w
	}()
	return done
}

func (o *rbRace) setDispatchedAt(t *testing.T, value any) {
	t.Helper()
	if _, err := o.pool.Exec(context.Background(), `UPDATE stages SET dispatched_at = $1 WHERE id = $2`, value, o.stageID); err != nil {
		t.Fatalf("setup: set dispatched_at: %v", err)
	}
}

func (o *rbRace) appendVerdict(t *testing.T, stageID uuid.UUID, evidence implementReviewedEvidence) {
	t.Helper()
	o.appendRow(t, stageID, implementReviewedCategory, recoveryPayload(t, evidence))
}

func TestShipPullRequest_OrdinaryRejectBeforeEdge_PG(t *testing.T) {
	// raceReject is the headline: an ordinary request parks past every outside read, then
	// a same-head pass commits its reject. Row and token still match at release; only
	// the read after the run lock sees the reject.
	raceReject := func(t *testing.T, requiresApproval bool) {
		t.Helper()
		o := newRBRace(t, requiresApproval, rejectStep)
		attempt := o.attempt(t)
		ordinary := o.park(t, o.ordinary(t))
		owner := o.ownerRejectsParked(t)
		o.edge.open()
		o.wantStatus(t, ordinary, "ordinary", http.StatusCreated)
		o.assertNoSuccessEdge(t)
		o.assertFailedBOn(t, attempt)
		o.ownerEdge.open()
		o.wantStatus(t, owner, "owner", http.StatusCreated)
		o.assertNoSuccessEdge(t)
		o.assertFailedBOn(t, attempt) // the owner's released failed edge is refused: no second close.
	}
	t.Run("create_gated", func(t *testing.T) { raceReject(t, true) })
	t.Run("create_gateless", func(t *testing.T) { raceReject(t, false) })
	t.Run("owner_failed_first", func(t *testing.T) {
		o := newRBRace(t, true, rejectStep)
		attempt := o.attempt(t)
		ordinary := o.park(t, o.ordinary(t))
		o.wantStatus(t, o.postAsync(o.body(t)), "owner", http.StatusCreated)
		o.assertFailedBOn(t, attempt)
		o.edge.open()
		o.wantStatus(t, ordinary, "ordinary", http.StatusCreated)
		o.assertNoSuccessEdge(t)
		o.assertFailedBOn(t, attempt)
	})
	heal := func(t *testing.T, requiresApproval bool) {
		t.Helper()
		o := newRBRace(t, requiresApproval, rejectStep)
		attempt := o.attempt(t)
		fault := errors.New("forced store fault")
		o.edge.failNext(fault)
		assertStaticEdgeError(t, o.post(t, o.ordinary(t)), fault.Error())
		replay := o.park(t, o.ordinary(t))
		owner := o.ownerRejectsParked(t)
		o.edge.open()
		o.wantStatus(t, replay, "replay", http.StatusOK)
		o.assertNoSuccessEdge(t)
		o.assertFailedBOn(t, attempt)
		o.ownerEdge.open()
		o.wantStatus(t, owner, "owner", http.StatusCreated)
		o.assertFailedBOn(t, attempt)
	}
	t.Run("heal_gated", func(t *testing.T) { heal(t, true) })
	t.Run("heal_gateless", func(t *testing.T) { heal(t, false) })
	t.Run("earlier_attempt_reject_same_head", func(t *testing.T) {
		// Reconciled: §17's admission refuses an omitting upload once a start row names
		// the path, so attempt 1 is held ahead of its start rather than in its reviewer.
		o := newRBRace(t, true, rejectStep)
		held := newHeldStartAudit(t, o.audits)
		attemptOne := o.newServer(held)
		attemptOne.cfg.RunRepo = o.ownerEdge
		first := o.postAsyncTo(attemptOne, o.body(t))
		waitFor(t, held.reached, "setup: attempt 1 held ahead of its start")
		attempt := o.retry(t)
		ordinary := o.park(t, o.ordinary(t))
		held.open()
		o.wantStatus(t, first, "attempt 1", http.StatusCreated)
		if err := recordedRejectInRows(o.verdictRows(t)); err != nil {
			t.Fatal("setup: no committed reject at this head")
		}
		o.assertRunning(t)
		o.edge.open()
		o.wantStatus(t, ordinary, "attempt 2's ordinary upload", http.StatusCreated)
		o.assertNoSuccessEdge(t)
		o.assertFailedBOn(t, attempt)
	})

	// Category B on the empty identity: its failed edge is the guarded one, so the
	// attempt-pinned recorder stays empty and this arm reads the row itself.
	t.Run("legacy_identity_reject", func(t *testing.T) {
		o := newRBRace(t, true, approveStep)
		o.setDispatchedAt(t, nil)
		ordinary := o.park(t, o.ordinary(t))
		o.appendVerdict(t, o.stageID, supplementalVerdict(prSeamHead, "", planreview.VerdictReject))
		o.edge.open()
		o.wantStatus(t, ordinary, "ordinary", http.StatusCreated)
		o.assertNoSuccessEdge(t)
		got := o.committed(t)
		if got.State != run.StageStateFailed || got.FailureCategory == nil || *got.FailureCategory != run.FailureB {
			t.Fatalf("committed stage = %q %v, want failed category B", got.State, got.FailureCategory)
		}
		if n := o.closeCalls(); n != 1 {
			t.Errorf("PR closes = %d, want 1", n)
		}
		o.assertNoNotification(t)
	})
	t.Run("legacy_identity_no_rows_advances", func(t *testing.T) {
		o := newRBRace(t, true, approveStep)
		o.setDispatchedAt(t, nil)
		o.wantStatus(t, o.postAsync(o.ordinary(t)), "ordinary", http.StatusCreated)
		o.assertAdvancedOnce(t, run.StageStateAwaitingApproval)
	})
	t.Run("legacy_identity_row_gained_dispatch_refused", func(t *testing.T) {
		o := newRBRace(t, true, approveStep)
		o.setDispatchedAt(t, nil)
		ordinary := o.park(t, o.ordinary(t))
		o.setDispatchedAt(t, time.Now().UTC())
		o.edge.open()
		o.wantStatus(t, ordinary, "ordinary", http.StatusCreated)
		o.assertRunning(t)
		o.assertNoEffects(t)
	})
	t.Run("identity_drift_no_edge", func(t *testing.T) {
		o := newRBRace(t, true, approveStep)
		first := o.attempt(t)
		ordinary := o.park(t, o.ordinary(t))
		newer := o.retry(t)
		o.appendVerdict(t, o.stageID, supplementalVerdict(prSeamHead, first, planreview.VerdictReject))
		o.edge.open()
		o.wantStatus(t, ordinary, "ordinary", http.StatusCreated)
		got := o.committed(t)
		if got.State != run.StageStateRunning || run.StageAttemptToken(got.DispatchedAt) != newer || got.FailureCategory != nil {
			t.Errorf("newer attempt = %q token %q category %v, want it running untouched", got.State, run.StageAttemptToken(got.DispatchedAt), got.FailureCategory)
		}
		o.assertNoEffects(t)
	})
	t.Run("state_drift_no_edge", func(t *testing.T) {
		o := newRBRace(t, true, approveStep)
		ordinary := o.park(t, o.ordinary(t))
		if _, err := run.FailStage(context.Background(), o.realRuns, o.stageID, run.FailureC, "failed by the test"); err != nil {
			t.Fatalf("setup: fail stage: %v", err)
		}
		o.edge.open()
		o.wantStatus(t, ordinary, "ordinary", http.StatusCreated)
		if got := o.committed(t); got.State != run.StageStateFailed || got.FailureCategory == nil || *got.FailureCategory != run.FailureC {
			t.Errorf("committed stage = %q %v, want the failure that won", got.State, got.FailureCategory)
		}
		o.assertNoEffects(t)
	})

	// Positives: one edge, one status comment, one board move.
	assertOneNotify := func(t *testing.T, o *rbRace) {
		t.Helper()
		if n := o.notes.n.Load(); n != 1 {
			t.Errorf("status comments = %d, want 1", n)
		}
		if n := o.boards.Load(); n != 1 {
			t.Errorf("board transitions = %d, want 1", n)
		}
	}
	t.Run("ordinary_no_rows_advances_once", func(t *testing.T) {
		for _, tc := range []struct {
			name             string
			requiresApproval bool
			want             run.StageState
		}{
			{"gated", true, run.StageStateAwaitingApproval},
			{"gateless", false, run.StageStateSucceeded},
		} {
			t.Run(tc.name, func(t *testing.T) {
				o := newRBRace(t, tc.requiresApproval, approveStep)
				ordinary := o.park(t, o.ordinary(t))
				o.edge.open()
				o.wantStatus(t, ordinary, "ordinary", http.StatusCreated)
				o.assertAdvancedOnce(t, tc.want)
				assertOneNotify(t, o)
				if advanced := o.walks.Load() != 0; advanced != !tc.requiresApproval {
					t.Errorf("orchestrator fired = %v, gateless = %v", advanced, !tc.requiresApproval)
				}
			})
		}
	})
	t.Run("ordinary_complete_approve_advances_once", func(t *testing.T) {
		// The owner's full set is written and it is parked before its edge. Another
		// process, with no local owner, advances; the owner's edge is then refused.
		o := newRBRace(t, true, approveStep)
		owner := o.park(t, o.body(t))
		if n := len(o.verdictRows(t)); n != 1 {
			t.Fatalf("setup: verdict rows = %d, want the owner's", n)
		}
		o.wantStatus(t, o.postAsyncTo(o.newServer(o.audits), o.ordinary(t)), "ordinary", http.StatusCreated)
		o.edge.open()
		o.wantStatus(t, owner, "owner", http.StatusCreated)
		o.assertAdvancedOnce(t, run.StageStateAwaitingApproval)
	})
	t.Run("approve_no_start_advances", func(t *testing.T) {
		o := newRBRace(t, true, approveStep)
		ordinary := o.park(t, o.ordinary(t))
		o.appendVerdict(t, o.stageID, supplementalVerdict(prSeamHead, o.attempt(t), planreview.VerdictApprove))
		o.edge.open()
		o.wantStatus(t, ordinary, "ordinary", http.StatusCreated)
		o.assertAdvancedOnce(t, run.StageStateAwaitingApproval)
		assertOneNotify(t, o)
	})

	// Exclusions: an in-window row that is no binding reject at this head of this stage.
	authority := func(a planreview.AuthorityMode) func(*implementReviewedEvidence) {
		return func(e *implementReviewedEvidence) { e.Authority = a }
	}
	for _, tc := range []struct {
		name   string
		head   string
		mutate func(*implementReviewedEvidence)
	}{
		{name: "other_head_reject_excluded", head: prSeamOther},
		{name: "other_origin_reject_excluded", head: prSeamHead, mutate: func(e *implementReviewedEvidence) { e.Origin = "" }},
		{name: "advisory_reject_excluded", head: prSeamHead, mutate: authority(planreview.AuthorityAdvisory)},
		{name: "gateless_authority_reject_excluded", head: prSeamHead, mutate: authority(planreview.AuthorityGateless)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := newRBRace(t, true, approveStep)
			ordinary := o.park(t, o.ordinary(t))
			evidence := supplementalVerdict(tc.head, o.attempt(t), planreview.VerdictReject)
			if tc.mutate != nil {
				tc.mutate(&evidence)
			}
			o.appendVerdict(t, o.stageID, evidence)
			o.edge.open()
			o.wantStatus(t, ordinary, "ordinary", http.StatusCreated)
			o.assertAdvancedOnce(t, run.StageStateAwaitingApproval)
		})
	}
	t.Run("other_stage_reject_excluded", func(t *testing.T) {
		o := newRBRace(t, true, approveStep)
		other, err := o.realRuns.CreateStage(context.Background(), run.CreateStageParams{RunID: o.runID, Sequence: 2, Type: run.StageTypeImplement, ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code"})
		if err != nil {
			t.Fatalf("setup: %v", err)
		}
		ordinary := o.park(t, o.ordinary(t))
		o.appendVerdict(t, other.ID, supplementalVerdict(prSeamHead, o.attempt(t), planreview.VerdictReject))
		o.edge.open()
		o.wantStatus(t, ordinary, "ordinary", http.StatusCreated)
		o.assertAdvancedOnce(t, run.StageStateAwaitingApproval)
	})
	t.Run("no_authority_reject_binds", func(t *testing.T) {
		// No start row exists, so the reject predicate alone separates the outcomes.
		o := newRBRace(t, true, approveStep)
		attempt := o.attempt(t)
		ordinary := o.park(t, o.ordinary(t))
		evidence := supplementalVerdict(prSeamHead, attempt, planreview.VerdictReject)
		evidence.Authority = ""
		o.appendVerdict(t, o.stageID, evidence)
		o.edge.open()
		o.wantStatus(t, ordinary, "ordinary", http.StatusCreated)
		o.assertNoSuccessEdge(t)
		o.assertFailedBOn(t, attempt)
	})

	// Refusals: nothing moved, closed or notified.
	t.Run("verdict_row_undecodable", func(t *testing.T) {
		o := newRBRace(t, true, approveStep)
		ordinary := o.park(t, o.ordinary(t))
		o.appendRow(t, o.stageID, implementReviewedCategory, recoveryPayload(t, map[string]any{"origin": string(planreview.OriginBaseRebaseReinvoke), "head_sha": prSeamHead, "verdict": 123}))
		o.edge.open()
		w := awaitResponse(t, ordinary, "ordinary")
		o.assertRunning(t)
		o.assertNoEffects(t)
		assertStaticEdgeError(t, w)
	})
	t.Run("evidence_read_error", func(t *testing.T) {
		// A boundary fault, labelled as such: real rows cannot fail the read on demand.
		o := newRBRace(t, true, approveStep)
		fault := errors.New("forced evidence read fault")
		o.edge.failNext(fault)
		assertStaticEdgeError(t, o.post(t, o.ordinary(t)), fault.Error())
		o.assertRunning(t)
		o.assertNoEffects(t)
		o.wantStatus(t, o.postAsync(o.ordinary(t)), "replay", http.StatusOK)
		o.assertAdvancedOnce(t, run.StageStateAwaitingApproval)
	})
	t.Run("caller_cancelled_no_edge", func(t *testing.T) {
		o := newRBRace(t, true, approveStep)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		o.edge.parkNext()
		ordinary := o.postCancellable(ctx, o.ordinary(t))
		waitFor(t, o.edge.reached, "setup: the request parked at its edge")
		cancel()
		awaitResponse(t, ordinary, "ordinary")
		o.assertRunning(t)
		o.assertNoEffects(t)
		o.edge.open()
		o.wantStatus(t, o.postAsync(o.ordinary(t)), "replay", http.StatusOK)
		o.assertAdvancedOnce(t, run.StageStateAwaitingApproval)
	})
	t.Run("reject_after_terminal_commit", func(t *testing.T) {
		o := newRBRace(t, true, approveStep)
		o.wantStatus(t, o.postAsync(o.ordinary(t)), "ordinary", http.StatusCreated)
		o.appendVerdict(t, o.stageID, supplementalVerdict(prSeamHead, o.attempt(t), planreview.VerdictReject))
		o.assertAdvancedOnce(t, run.StageStateAwaitingApproval)
	})

	// The admission race: a start commits after this upload's admission read.
	t.Run("pass_started_obligation_unmet", func(t *testing.T) {
		for _, tc := range []struct {
			name  string
			final reviewStep
			check func(t *testing.T, o *rbRace, attempt string)
		}{
			{"then_approve_advances_once", approveStep, func(t *testing.T, o *rbRace, _ string) {
				o.assertAdvancedOnce(t, run.StageStateAwaitingApproval)
			}},
			{"then_reject_fails_once", rejectStep, func(t *testing.T, o *rbRace, attempt string) { o.assertFailedBOn(t, attempt) }},
		} {
			t.Run(tc.name, func(t *testing.T) {
				started, release := make(chan struct{}), make(chan struct{})
				var once sync.Once
				open := func() { once.Do(func() { close(release) }) }
				t.Cleanup(open)
				o := newRBRace(t, true, heldStep(started, release, tc.final))
				attempt := o.attempt(t)
				ordinary := o.park(t, o.ordinary(t))
				owner := o.postAsync(o.body(t))
				waitFor(t, started, "setup: the owner's reviewer")
				if n := len(o.startRows(t)); n != 1 {
					t.Fatalf("setup: start rows = %d, want the committed start", n)
				}
				o.edge.open()
				assertCode(t, awaitResponse(t, ordinary, "ordinary"), http.StatusConflict, "review_obligation_missing")
				o.assertRunning(t)
				o.assertNoEffects(t)
				open()
				o.wantStatus(t, owner, "owner", http.StatusCreated)
				tc.check(t, o, attempt)
			})
		}
	})
	t.Run("pass_lost_obligation_unmet", func(t *testing.T) {
		o := newRBRace(t, true, approveStep)
		attempt := o.attempt(t)
		ordinary := o.park(t, o.ordinary(t))
		// A committed start whose owner is gone: allocated by a server that never runs it.
		if _, err := o.newServer(o.audits).allocateSupplementalPass(context.Background(), o.runID, o.stageID, prSeamHead, attempt, 1, []string{prSeamPath}); err != nil {
			t.Fatalf("setup: allocate: %v", err)
		}
		o.edge.open()
		assertCode(t, awaitResponse(t, ordinary, "ordinary"), http.StatusConflict, "review_obligation_missing")
		o.assertRunning(t)
		o.assertNoEffects(t)
	})
	t.Run("failure_report_lands", func(t *testing.T) {
		o := newRBRace(t, true, approveStep)
		ordinary := o.park(t, o.ordinary(t))
		failed, err := json.Marshal(map[string]any{"outcome": "failed", "category": "C", "reason": "gave up"})
		if err != nil {
			t.Fatal(err)
		}
		o.wantStatus(t, o.postAsync(failed), "failure report", http.StatusOK)
		if got := o.committed(t).State; got != run.StageStateFailed {
			t.Fatalf("committed stage = %q, want the runner's failure to land", got)
		}
		o.edge.open()
		o.wantStatus(t, ordinary, "ordinary", http.StatusCreated)
		o.assertNoSuccessEdge(t)
	})
}

// rowCount reads one outcome's committed rows.
func (o *rbRace) rowCount(t *testing.T, category string) int {
	t.Helper()
	rows, err := o.audits.ListForRunByCategory(context.Background(), o.runID, category)
	if err != nil {
		t.Fatalf("list %s: %v", category, err)
	}
	return len(rows)
}

func (o *rbRace) assertRefusedClaim(t *testing.T, w *httptest.ResponseRecorder, c variantCase) {
	t.Helper()
	o.assertRunning(t)
	o.assertNoSuccessEdge(t)
	if n := o.rowCount(t, c.row); n != 0 {
		t.Errorf("%s rows = %d, want 0", c.row, n)
	}
	if n := o.closeCalls(); n != 0 {
		t.Errorf("PR closes = %d, want 0", n)
	}
	o.assertNoNotification(t)
	assertCode(t, w, http.StatusConflict, "review_obligation_missing")
	if !strings.Contains(w.Body.String(), `"outcome":"`+c.outcome+`"`) || !strings.Contains(w.Body.String(), `"missing_count"`) {
		t.Errorf("details: %s", w.Body.String())
	}
}

func TestShipPullRequest_WrongClaimedOutcome_PG(t *testing.T) {
	for _, c := range prVariantCases {
		t.Run(c.outcome, func(t *testing.T) {
			t.Run("reject_in_window", func(t *testing.T) {
				o := newRBRace(t, true, rejectStep)
				claim := o.park(t, c.body(t))
				owner := o.ownerRejectsParked(t)
				o.edge.open()
				o.assertRefusedClaim(t, awaitResponse(t, claim, "claim"), c)
				o.ownerEdge.open()
				o.wantStatus(t, owner, "owner", http.StatusCreated)
				if n := o.edge.advances.Load(); n != 0 {
					t.Errorf("successful edges = %d, want 0", n)
				}
				if n := o.closeCalls(); n != 1 {
					t.Errorf("PR closes = %d, want the owner's one", n)
				}
			})
			t.Run("start_in_window", func(t *testing.T) {
				started, release := make(chan struct{}), make(chan struct{})
				var once sync.Once
				open := func() { once.Do(func() { close(release) }) }
				t.Cleanup(open)
				o := newRBRace(t, true, heldStep(started, release, approveStep))
				claim := o.park(t, c.body(t))
				owner := o.postAsync(o.body(t))
				waitFor(t, started, "setup: the owner's reviewer")
				o.edge.open()
				o.assertRefusedClaim(t, awaitResponse(t, claim, "claim"), c)
				open()
				o.wantStatus(t, owner, "owner", http.StatusCreated)
				o.assertAdvancedOnce(t, run.StageStateAwaitingApproval)
			})
			if c.head != "" {
				t.Run("fabricated_other_head", func(t *testing.T) {
					o := newRBRace(t, true, approveStep)
					fabricated := c
					fabricated.head = prSeamOther
					claim := o.park(t, fabricated.body(t))
					o.appendVerdict(t, o.stageID, supplementalVerdict(prSeamHead, o.attempt(t), planreview.VerdictReject))
					o.edge.open()
					o.assertRefusedClaim(t, awaitResponse(t, claim, "claim"), c)
				})
			}
			t.Run("earlier_attempt_reject", func(t *testing.T) {
				o := newRBRace(t, true, approveStep)
				first := o.attempt(t)
				o.retry(t)
				claim := o.park(t, c.body(t))
				o.appendVerdict(t, o.stageID, supplementalVerdict(prSeamHead, first, planreview.VerdictReject))
				o.edge.open()
				o.assertRefusedClaim(t, awaitResponse(t, claim, "claim"), c)
			})
			t.Run("settled_after_reject", func(t *testing.T) {
				o := newRBRace(t, true, approveStep)
				if _, err := o.realRuns.TransitionStage(context.Background(), o.stageID, run.StageStateAwaitingApproval, nil); err != nil {
					t.Fatalf("setup: settle: %v", err)
				}
				o.appendVerdict(t, o.stageID, supplementalVerdict(prSeamHead, o.attempt(t), planreview.VerdictReject))
				w := o.post(t, c.body(t))
				assertCode(t, w, http.StatusConflict, "review_obligation_missing")
				if got := o.committed(t).State; got != run.StageStateAwaitingApproval {
					t.Errorf("committed stage = %q, want it untouched", got)
				}
				if n := o.rowCount(t, c.row); n != 0 {
					t.Errorf("%s rows = %d, want 0", c.row, n)
				}
				o.assertNoEffects(t)
			})
			t.Run("caller_cancelled_no_edge", func(t *testing.T) {
				o := newRBRace(t, true, approveStep)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				o.edge.parkNext()
				claim := o.postCancellable(ctx, c.body(t))
				waitFor(t, o.edge.reached, "setup: the claim parked at its edge")
				cancel()
				awaitResponse(t, claim, "claim")
				o.assertRunning(t)
				if n := o.rowCount(t, c.row); n != 0 {
					t.Errorf("%s rows = %d, want 0", c.row, n)
				}
				o.assertNoEffects(t)
			})
		})
	}
}

// makeRealChild turns the seam's run into a real decomposition child of a real parent.
func (o *rbRace) makeRealChild(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	parent, err := o.realRuns.CreateRun(ctx, run.CreateRunParams{
		Repo: "kuhlman-labs/example", WorkflowID: "feature_change", WorkflowSHA: "deadbeef",
		TriggerSource: run.TriggerCLI, RunnerKind: run.RunnerKindLocal,
	})
	if err != nil {
		t.Fatalf("setup: parent run: %v", err)
	}
	if _, err := o.pool.Exec(ctx, `UPDATE runs SET decomposed_from = $1 WHERE id = $2`, parent.ID, o.runID); err != nil {
		t.Fatalf("setup: link parent: %v", err)
	}
}

const (
	rbFixupConcerns   = `[{"severity":"HIGH","category":"c","note":"n"}]`
	rbFixupTrigger    = `{"concerns":` + rbFixupConcerns + `}`
	rbConflictTrigger = `{"branch":"b","base_ref":"main","expected_head_sha":"h"}`
)

func (o *rbRace) proveMode(t *testing.T, c variantCase) {
	t.Helper()
	switch c.outcome {
	case "pushed":
		o.makeRealChild(t)
	case "fixup_pushed", "fixup_no_changes":
		o.appendRow(t, o.stageID, CategoryStageFixupTriggered, []byte(rbFixupTrigger))
	case "conflict_resolution_pushed":
		o.appendRow(t, o.stageID, CategoryStageConflictResolutionTriggered, []byte(rbConflictTrigger))
	}
}

func (o *rbRace) assertGenuineEdge(t *testing.T, c variantCase, wantRows int) {
	t.Helper()
	o.assertAdvancedOnce(t, run.StageStateAwaitingApproval)
	if n := o.rowCount(t, c.row); n != wantRows {
		t.Errorf("%s rows = %d, want %d", c.row, n, wantRows)
	}
}

func TestShipPullRequest_GenuineModeEdges_PG(t *testing.T) {
	byOutcome := map[string]variantCase{}
	for _, c := range prVariantCases {
		byOutcome[c.outcome] = c
	}
	pushed, fixup, noChanges, conflict := byOutcome["pushed"], byOutcome["fixup_pushed"], byOutcome["fixup_no_changes"], byOutcome["conflict_resolution_pushed"]
	oldReject := func(t *testing.T, o *rbRace, head string) {
		t.Helper()
		o.appendVerdict(t, o.stageID, supplementalVerdict(head, o.attempt(t), planreview.VerdictReject))
	}
	t.Run("real_child_pushed", func(t *testing.T) {
		o := newRBRace(t, true, approveStep)
		o.proveMode(t, pushed)
		oldReject(t, o, prSeamOther)
		o.wantStatus(t, o.postAsync(pushed.body(t)), "child push", http.StatusOK)
		o.assertGenuineEdge(t, pushed, 1)
	})
	t.Run("live_fixup_new_head_over_old_reject", func(t *testing.T) {
		o := newRBRace(t, true, approveStep)
		oldReject(t, o, prSeamOther)
		o.proveMode(t, fixup)
		claim := o.park(t, fixup.body(t))
		o.edge.open()
		o.wantStatus(t, claim, "fix-up", http.StatusOK)
		o.assertGenuineEdge(t, fixup, 1)
	})
	t.Run("live_fixup_no_changes_over_old_reject", func(t *testing.T) {
		o := newRBRace(t, true, approveStep)
		oldReject(t, o, prSeamHead)
		o.proveMode(t, noChanges)
		o.wantStatus(t, o.postAsync(noChanges.body(t)), "no-change report", http.StatusOK)
		o.assertGenuineEdge(t, noChanges, 1)
	})
	t.Run("live_conflict_new_head", func(t *testing.T) {
		o := newRBRace(t, true, approveStep)
		oldReject(t, o, prSeamOther)
		o.proveMode(t, conflict)
		o.wantStatus(t, o.postAsync(conflict.body(t)), "conflict resolution", http.StatusOK)
		o.assertGenuineEdge(t, conflict, 1)
	})
	t.Run("retried_live_fixup", func(t *testing.T) {
		o := newRBRace(t, true, approveStep)
		oldReject(t, o, prSeamOther)
		o.proveMode(t, fixup)
		o.retry(t)
		o.wantStatus(t, o.postAsync(fixup.body(t)), "fix-up", http.StatusOK)
		o.assertGenuineEdge(t, fixup, 1)
	})
	t.Run("legacy_identity_advances", func(t *testing.T) {
		o := newRBRace(t, true, approveStep)
		o.setDispatchedAt(t, nil)
		o.wantStatus(t, o.postAsync(fixup.body(t)), "fix-up", http.StatusOK)
		o.assertGenuineEdge(t, fixup, 1)
	})
	t.Run("nothing_recorded_advances", func(t *testing.T) {
		for _, c := range prVariantCases {
			t.Run(c.outcome, func(t *testing.T) {
				o := newRBRace(t, true, approveStep)
				o.wantStatus(t, o.postAsync(c.body(t)), "report", http.StatusOK)
				o.assertGenuineEdge(t, c, 1)
			})
		}
	})
	for _, tc := range []struct {
		name string
		c    variantCase
	}{
		{"child_replay", pushed},
		{"fixup_replay", fixup},
		{"no_changes_replay", noChanges},
		{"conflict_replay", conflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := newRBRace(t, true, approveStep)
			o.proveMode(t, tc.c)
			body := tc.c.body(t)
			o.wantStatus(t, o.postAsync(body), "report", http.StatusOK)
			o.wantStatus(t, o.postAsync(body), "replay", http.StatusOK)
			o.assertGenuineEdge(t, tc.c, 1)
		})
	}

	// Stale proofs: the newest stage-bound mode row decides.
	refused := func(t *testing.T, o *rbRace, c variantCase, wantRows int) {
		t.Helper()
		w := o.post(t, c.body(t))
		o.assertRunning(t)
		o.assertNoSuccessEdge(t)
		if n := o.rowCount(t, c.row); n != wantRows {
			t.Errorf("%s rows = %d, want %d", c.row, n, wantRows)
		}
		assertCode(t, w, http.StatusConflict, "review_obligation_missing")
	}
	consumedAt := recoveryPayload(t, map[string]any{"head_sha": prSeamOther})
	t.Run("consumed_fixup_trigger", func(t *testing.T) {
		o := newRBRace(t, true, approveStep)
		o.proveMode(t, fixup)
		// The later row carries concerns, so its category alone separates it from a live
		// trigger: the concerns check cannot mask a mode row read as the trigger.
		o.appendRow(t, o.stageID, "fixup_pushed", recoveryPayload(t, map[string]any{"head_sha": prSeamOther, "concerns": json.RawMessage(rbFixupConcerns)}))
		oldReject(t, o, prSeamOther)
		refused(t, o, fixup, 1)
	})
	t.Run("consumed_conflict_trigger", func(t *testing.T) {
		o := newRBRace(t, true, approveStep)
		o.proveMode(t, conflict)
		// The pass's own pushed row replays by the handler's stage-keyed dedup, so the
		// consumption this report can still reach is the failure marker.
		o.appendRow(t, o.stageID, CategoryStageConflictResolutionFailed, consumedAt)
		oldReject(t, o, prSeamOther)
		refused(t, o, conflict, 0)
	})
	t.Run("conflict_trigger_spelled_fixup", func(t *testing.T) {
		o := newRBRace(t, true, approveStep)
		o.proveMode(t, conflict)
		oldReject(t, o, prSeamOther)
		refused(t, o, fixup, 0)
	})
	t.Run("proved_same_head_reject", func(t *testing.T) {
		o := newRBRace(t, true, approveStep)
		o.proveMode(t, fixup)
		oldReject(t, o, prSeamHead)
		refused(t, o, fixup, 0)
	})

	// Drift: the variant's normal 200, with no row that would consume a newer trigger.
	t.Run("attempt_drift_no_edge_no_row", func(t *testing.T) {
		o := newRBRace(t, true, approveStep)
		o.proveMode(t, fixup)
		claim := o.park(t, fixup.body(t))
		newer := o.retry(t)
		o.edge.open()
		o.wantStatus(t, claim, "fix-up", http.StatusOK)
		got := o.committed(t)
		if got.State != run.StageStateRunning || run.StageAttemptToken(got.DispatchedAt) != newer {
			t.Errorf("newer attempt = %q token %q, want it running on %q", got.State, run.StageAttemptToken(got.DispatchedAt), newer)
		}
		if n := o.rowCount(t, fixup.row); n != 0 {
			t.Errorf("fixup_pushed rows = %d, want 0", n)
		}
		o.assertNoEffects(t)
	})
	t.Run("legacy_row_gained_dispatch_refused", func(t *testing.T) {
		o := newRBRace(t, true, approveStep)
		o.setDispatchedAt(t, nil)
		claim := o.park(t, fixup.body(t))
		o.setDispatchedAt(t, time.Now().UTC())
		o.edge.open()
		o.wantStatus(t, claim, "fix-up", http.StatusOK)
		o.assertRunning(t)
		if n := o.rowCount(t, fixup.row); n != 0 {
			t.Errorf("fixup_pushed rows = %d, want 0", n)
		}
		o.assertNoEffects(t)
	})
	t.Run("trigger_undecodable_500", func(t *testing.T) {
		o := newRBRace(t, true, approveStep)
		o.appendRow(t, o.stageID, CategoryStageFixupTriggered, []byte(`{"concerns":7}`))
		w := o.post(t, fixup.body(t))
		o.assertRunning(t)
		if n := o.rowCount(t, fixup.row); n != 0 {
			t.Errorf("fixup_pushed rows = %d, want 0", n)
		}
		o.assertNoEffects(t)
		assertStaticEdgeError(t, w)
	})
}
