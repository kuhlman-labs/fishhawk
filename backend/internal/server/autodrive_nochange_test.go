package server

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/delegation"
	"github.com/kuhlman-labs/fishhawk/backend/internal/operatorrole"
	"github.com/kuhlman-labs/fishhawk/backend/internal/orchestrator"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// The #5390 no-change guard tests. Every gate-level fixture is built so that,
// WITHOUT the guard, the delegated route_fixup arm would ACT:
// seedRouteFixupReady makes convergent_concerns met with the implement gate
// parked, and a seeded no-change pass is refunded against the normal budget.
// Each test reads the appended audit after the call.

// soleOpenImplementConcern returns the single open implement concern
// seedRouteFixupReady seeded.
func soleOpenImplementConcern(t *testing.T, cr *fakeConcernRepo, runID uuid.UUID) *concern.Concern {
	t.Helper()
	open, err := cr.ListOpenByRun(context.Background(), runID)
	if err != nil || len(open) != 1 {
		t.Fatalf("ListOpenByRun = %v, %v; want exactly one open concern", open, err)
	}
	return open[0]
}

// autoDriveGate runs the in-process actor once under the campaign identity.
func autoDriveGate(t *testing.T, s *Server, repo *autoDriveRepo, runID uuid.UUID) AutoDriveOutcome {
	t.Helper()
	out, err := s.AutoDriveRunGate(context.Background(), getRun(t, repo, runID), campaignOperatorIdentity(), nil, nil)
	if err != nil {
		t.Fatalf("AutoDriveRunGate: %v", err)
	}
	return out
}

// assertNoChangeDecision asserts the guard's decision_required outcome with
// the expected routed ids, and that the pass appended no trigger.
func assertNoChangeDecision(t *testing.T, au *auditFake, out AutoDriveOutcome, wantIDs []string, wantNewTriggers int) {
	t.Helper()
	if out.Acted || out.Paged || !out.DecisionRequired {
		t.Fatalf("outcome = %+v, want a non-acted, non-paged decision_required", out)
	}
	if out.DecisionState != operatorrole.DecisionStateFixupNoChangesUnaddressed {
		t.Fatalf("DecisionState = %q, want %q", out.DecisionState, operatorrole.DecisionStateFixupNoChangesUnaddressed)
	}
	if out.Action != delegation.ActionRouteFixup {
		t.Errorf("Action = %q, want %q", out.Action, delegation.ActionRouteFixup)
	}
	if !reflect.DeepEqual(out.ConcernIDs, wantIDs) {
		t.Errorf("ConcernIDs = %v, want %v", out.ConcernIDs, wantIDs)
	}
	if n := countAudit(au, CategoryStageFixupTriggered); n != wantNewTriggers {
		t.Errorf("%d %s entries appended, want %d (the guard must route nothing)", n, CategoryStageFixupTriggered, wantNewTriggers)
	}
}

// assertActedRoute asserts the arm routed exactly one new fix-up pass.
func assertActedRoute(t *testing.T, au *auditFake, out AutoDriveOutcome) {
	t.Helper()
	if !out.Acted || out.Action != delegation.ActionRouteFixup {
		t.Fatalf("outcome = %+v, want acted route_fixup (the guard must not fire)", out)
	}
	if out.ConcernIDs != nil {
		t.Errorf("ConcernIDs = %v on an acted outcome, want nil", out.ConcernIDs)
	}
	if n := countAudit(au, CategoryStageFixupTriggered); n != 1 {
		t.Errorf("%d %s entries appended, want 1", n, CategoryStageFixupTriggered)
	}
}

// seedRawFixupTrigger seeds a stage_fixup_triggered entry with an explicit
// raw payload at an explicit sequence.
func seedRawFixupTrigger(au *auditFake, runID, stageID uuid.UUID, seq int64, payload []byte) {
	rid, sid := runID, stageID
	au.mu.Lock()
	au.seeded = append(au.seeded, &audit.Entry{
		RunID: &rid, StageID: &sid, Category: CategoryStageFixupTriggered, Sequence: seq, Payload: payload})
	au.mu.Unlock()
}

// TestAutoDriveRunGate_NoChangePass_DeclinesIdenticalReroute is the #5390
// headline across the route, persistence and guard layers. Pass 1 is a REAL
// delegated route: fixupStageAs writes the real stage_fixup_triggered with
// concern_ids. The test re-parks the implement gate and records the pass's
// fixup_no_changes after that trigger. The second pass must decline with
// decision_required fixup_no_changes_unaddressed naming the routed id, and
// exactly ONE trigger exists in total.
func TestAutoDriveRunGate_NoChangePass_DeclinesIdenticalReroute(t *testing.T) {
	s, repo, au, cr := newAutoDriveServer(t)
	au.stampSequence = true
	runID, impl := seedRouteFixupReady(t, s, repo, au, cr)
	a := soleOpenImplementConcern(t, cr, runID)

	first := autoDriveGate(t, s, repo, runID)
	assertActedRoute(t, au, first)
	trig := auditEntry(t, au, CategoryStageFixupTriggered)
	if !strings.Contains(string(trig.Payload), a.ID.String()) {
		t.Fatalf("pass-1 trigger payload %s does not name the routed concern %s", trig.Payload, a.ID)
	}

	// The pass ran and produced no commit: the stage re-parks at its gate and
	// the runner's report lands fixup_no_changes after the trigger.
	impl.State = run.StageStateAwaitingApproval
	seedFixupNoChangesSeq(au, runID, impl.ID, 5000)

	second := autoDriveGate(t, s, repo, runID)
	assertNoChangeDecision(t, au, second, []string{a.ID.String()}, 1)
	for _, want := range []string{"fishhawk_vouch_commit", "fishhawk_resolve_concerns", "#5496", a.ID.String()} {
		if !strings.Contains(second.Note, want) {
			t.Errorf("Note %q does not name %q", second.Note, want)
		}
	}
}

// TestAutoFixup_NoChangeGuard_ChangedSetRoutes: a no-change window whose routed
// set differs from the routable open set is not a repeat, so the arm routes.
func TestAutoFixup_NoChangeGuard_ChangedSetRoutes(t *testing.T) {
	t.Run("subset open after a waive", func(t *testing.T) {
		s, repo, au, cr := newAutoDriveServer(t)
		runID, impl := seedRouteFixupReady(t, s, repo, au, cr)
		a := soleOpenImplementConcern(t, cr, runID)
		waived := uuid.New().String()
		seedFixupTriggeredWithConcernsSeq(au, runID, impl.ID, 1000, run.StageStateAwaitingApproval, nil, []string{a.ID.String(), waived})
		seedFixupNoChangesSeq(au, runID, impl.ID, 1005)
		assertActedRoute(t, au, autoDriveGate(t, s, repo, runID))
	})
	t.Run("superset open", func(t *testing.T) {
		s, repo, au, cr := newAutoDriveServer(t)
		runID, impl := seedRouteFixupReady(t, s, repo, au, cr)
		a := soleOpenImplementConcern(t, cr, runID)
		seedOpenConcern(t, cr, runID, impl.ID, concern.StageKindImplement, "medium", "correctness", "a second finding")
		seedFixupTriggeredWithConcernsSeq(au, runID, impl.ID, 1000, run.StageStateAwaitingApproval, nil, []string{a.ID.String()})
		seedFixupNoChangesSeq(au, runID, impl.ID, 1005)
		assertActedRoute(t, au, autoDriveGate(t, s, repo, runID))
	})
}

// TestAutoFixup_NoChangeGuard_PushedPassRoutes: a window whose EARLIEST
// following outcome is fixup_pushed is not a no-change pass, so the guard does
// not fire. A pushed pass is not refunded, so with the default one-pass budget
// the arm falls through to fixup_budget_exhausted rather than acting; the
// discriminating assertion is that the decision is NOT the no-change state.
func TestAutoFixup_NoChangeGuard_PushedPassRoutes(t *testing.T) {
	for _, tc := range []struct {
		name     string
		noChange bool
	}{
		{"pushed", false},
		{"pushed then no_changes: earliest outcome decides", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, repo, au, cr := newAutoDriveServer(t)
			runID, impl := seedRouteFixupReady(t, s, repo, au, cr)
			a := soleOpenImplementConcern(t, cr, runID)
			seedFixupTriggeredWithConcernsSeq(au, runID, impl.ID, 1000, run.StageStateAwaitingApproval, nil, []string{a.ID.String()})
			seedFixupPushedSeq(au, runID, impl.ID, 1005)
			if tc.noChange {
				seedFixupNoChangesSeq(au, runID, impl.ID, 1010)
			}
			out := autoDriveGate(t, s, repo, runID)
			if out.DecisionState != "fixup_budget_exhausted" || out.ConcernIDs != nil {
				t.Fatalf("outcome = %+v, want decision_state fixup_budget_exhausted with no concern_ids (the guard must not fire on a pushed pass)", out)
			}
			if n := countAudit(au, CategoryStageFixupTriggered); n != 0 {
				t.Errorf("%d triggers appended, want 0", n)
			}
		})
	}
}

// TestAutoFixup_NoChangeGuard_OnlyNewestTriggerCounts: an OLDER no-change
// window does not trip the guard once a newer trigger opened a window that
// ended in a death refund with no outcome row.
func TestAutoFixup_NoChangeGuard_OnlyNewestTriggerCounts(t *testing.T) {
	s, repo, au, cr := newAutoDriveServer(t)
	runID, impl := seedRouteFixupReady(t, s, repo, au, cr)
	a := soleOpenImplementConcern(t, cr, runID)
	ids := []string{a.ID.String()}
	seedFixupTriggeredWithConcernsSeq(au, runID, impl.ID, 1000, run.StageStateAwaitingApproval, nil, ids)
	seedFixupNoChangesSeq(au, runID, impl.ID, 1005)
	seedFixupTriggeredWithConcernsSeq(au, runID, impl.ID, 1010, run.StageStateAwaitingApproval, nil, ids)
	seedFixupRecoveredC(au, runID, impl.ID, run.FailureA, 1015)
	assertActedRoute(t, au, autoDriveGate(t, s, repo, runID))
}

// TestAutoFixup_NoChangeGuard_OtherStageIgnored: an identical-id no-change
// window recorded on ANOTHER stage never trips this stage's guard.
func TestAutoFixup_NoChangeGuard_OtherStageIgnored(t *testing.T) {
	s, repo, au, cr := newAutoDriveServer(t)
	runID, _ := seedRouteFixupReady(t, s, repo, au, cr)
	a := soleOpenImplementConcern(t, cr, runID)
	other := uuid.New()
	seedFixupTriggeredWithConcernsSeq(au, runID, other, 1000, run.StageStateAwaitingApproval, nil, []string{a.ID.String()})
	seedFixupNoChangesSeq(au, runID, other, 1005)
	assertActedRoute(t, au, autoDriveGate(t, s, repo, runID))
}

// TestAutoFixup_NoChangeGuard_LegacyTriggerRoutes: a trigger with no
// concern_ids is no positive evidence, so a following fixup_no_changes does
// not trip the guard.
func TestAutoFixup_NoChangeGuard_LegacyTriggerRoutes(t *testing.T) {
	s, repo, au, cr := newAutoDriveServer(t)
	runID, impl := seedRouteFixupReady(t, s, repo, au, cr)
	seedFixupTriggeredSeq(au, runID, impl.ID, 1000)
	seedFixupNoChangesSeq(au, runID, impl.ID, 1005)
	assertActedRoute(t, au, autoDriveGate(t, s, repo, runID))
}

// TestAutoFixup_NoChangeGuard_MalformedTriggerRoutes: a newest trigger whose
// payload is not JSON is no positive evidence, so the arm routes.
func TestAutoFixup_NoChangeGuard_MalformedTriggerRoutes(t *testing.T) {
	s, repo, au, cr := newAutoDriveServer(t)
	runID, impl := seedRouteFixupReady(t, s, repo, au, cr)
	seedRawFixupTrigger(au, runID, impl.ID, 1000, []byte("not json"))
	seedFixupNoChangesSeq(au, runID, impl.ID, 1005)
	assertActedRoute(t, au, autoDriveGate(t, s, repo, runID))
}

// TestNoChangeRepeatOfOpenSet_ReadErrorsFailClosed calls the helper directly
// so no downstream read can mask it: each audit read the guard makes fails in
// turn, and each returns a wrapped error with no refusal.
func TestNoChangeRepeatOfOpenSet_ReadErrorsFailClosed(t *testing.T) {
	for _, cat := range []string{CategoryStageFixupTriggered, CategoryFixupNoChanges, CategoryFixupPushed} {
		t.Run(cat, func(t *testing.T) {
			s, repo, au, cr := newAutoDriveServer(t)
			runID, impl := seedRouteFixupReady(t, s, repo, au, cr)
			a := soleOpenImplementConcern(t, cr, runID)
			seedFixupTriggeredWithConcernsSeq(au, runID, impl.ID, 1000, run.StageStateAwaitingApproval, nil, []string{a.ID.String()})
			seedFixupNoChangesSeq(au, runID, impl.ID, 1005)
			au.listByCategoryErrCategory = cat

			repeat, err := s.noChangeRepeatOfOpenSet(context.Background(), runID, impl.ID, []uuid.UUID{a.ID}, nil)
			if err == nil {
				t.Fatalf("err = nil, want the %s read error (fail-closed)", cat)
			}
			if !strings.Contains(err.Error(), cat) {
				t.Errorf("err = %v, want it to name %s", err, cat)
			}
			if repeat != nil {
				t.Errorf("repeat = %+v, want nil on a read error", repeat)
			}
		})
	}
}

// firstReadFailAudit fails ONLY the first ListForRunByCategory read of one
// category, so the guard's read can be isolated from fixupRefundedPasses'
// later re-read of the same category (G3). auditFake's
// listByCategoryErrCategory fails every read, which the refund chokepoint
// would surface on its own and mask the guard's propagation.
type firstReadFailAudit struct {
	*auditFake
	category string
	mu       sync.Mutex
	calls    int
}

func (f *firstReadFailAudit) ListForRunByCategory(ctx context.Context, runID uuid.UUID, category string) ([]*audit.Entry, error) {
	if category == f.category {
		f.mu.Lock()
		f.calls++
		n := f.calls
		f.mu.Unlock()
		if n == 1 {
			return nil, errInjectedFirstRead
		}
	}
	return f.auditFake.ListForRunByCategory(ctx, runID, category)
}

var errInjectedFirstRead = errors.New("firstReadFailAudit: injected first-read error")

// TestAutoFixup_NoChangeGuard_GateReadErrorFailsClosed: the guard's first
// fixup_no_changes read fails; AutoDriveRunGate must return the error and route
// nothing. With the guard's error swallowed, fixupRefundedPasses' second read
// succeeds, refunds the pass and the arm routes, so this isolates the guard.
func TestAutoFixup_NoChangeGuard_GateReadErrorFailsClosed(t *testing.T) {
	repo := &autoDriveRepo{driveE2ERepo: &driveE2ERepo{fakeRepo: newFakeRepo()}}
	inner := newAuditFake()
	au := &firstReadFailAudit{auditFake: inner, category: CategoryFixupNoChanges}
	cr := newFakeConcernRepo()
	s := New(Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      repo,
		AuditRepo:    au,
		ConcernRepo:  cr,
		ApprovalRepo: newFakeApprovalRepo(),
		Orchestrator: &orchestrator.Orchestrator{Runs: repo},
	})
	runID, impl := seedRouteFixupReady(t, s, repo, inner, cr)
	a := soleOpenImplementConcern(t, cr, runID)
	seedFixupTriggeredWithConcernsSeq(inner, runID, impl.ID, 1000, run.StageStateAwaitingApproval, nil, []string{a.ID.String()})
	seedFixupNoChangesSeq(inner, runID, impl.ID, 1005)

	_, err := s.AutoDriveRunGate(context.Background(), getRun(t, repo, runID), campaignOperatorIdentity(), nil, nil)
	if err == nil || !strings.Contains(err.Error(), errInjectedFirstRead.Error()) {
		t.Fatalf("AutoDriveRunGate err = %v, want the guard's injected read error", err)
	}
	if au.calls < 1 {
		t.Fatal("the fixup_no_changes read was never made")
	}
	if n := countAudit(inner, CategoryStageFixupTriggered); n != 0 {
		t.Errorf("%d triggers appended, want 0 (fail-closed: route nothing on unknown evidence)", n)
	}
}

// TestAutoFixup_NoChangeGuard_ServerCheckLeftoverStillDeclines composes with
// #5477: an open server-check row X beside the reviewer row A never breaks the
// set test in either direction.
func TestAutoFixup_NoChangeGuard_ServerCheckLeftoverStillDeclines(t *testing.T) {
	for _, tc := range []struct {
		name   string
		routeX bool
	}{
		{"delegated route", false},
		{"human route included the server-check row", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, repo, au, cr := newAutoDriveServer(t)
			runID, impl := seedRouteFixupReady(t, s, repo, au, cr)
			a := soleOpenImplementConcern(t, cr, runID)
			x := seedServerCheckConcern(t, cr, runID, impl.ID, concern.StageKindImplement, "high", "leaked-token")
			routed := []string{a.ID.String()}
			if tc.routeX {
				routed = append(routed, x.ID.String())
			}
			seedFixupTriggeredWithConcernsSeq(au, runID, impl.ID, 1000, run.StageStateAwaitingApproval, nil, routed)
			seedFixupNoChangesSeq(au, runID, impl.ID, 1005)

			out := autoDriveGate(t, s, repo, runID)
			assertNoChangeDecision(t, au, out, []string{a.ID.String()}, 0)
			if strings.Contains(strings.Join(out.ConcernIDs, ","), x.ID.String()) {
				t.Errorf("ConcernIDs %v carry the server-check id %s", out.ConcernIDs, x.ID)
			}
		})
	}
}
