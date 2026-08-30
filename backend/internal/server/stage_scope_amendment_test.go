package server

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/prompt"
	"github.com/kuhlman-labs/fishhawk/backend/internal/scopeamendment"
)

// seedStageAmendment inserts one amendment row for (runID, stageID) in the given
// status directly into the fake's maps, returning its id. It writes the row BY
// CONSTRUCTION rather than through Create→Decide because the STATUS is the
// control under test in several cases below — producing it by exercising the
// decision path would put the fixture's correctness on the same code the test
// is meant to discriminate against.
func seedStageAmendment(sar *fakeScopeAmendmentRepo, runID, stageID uuid.UUID, status scopeamendment.Status, decisionReason *string, paths ...string) uuid.UUID {
	entries := make([]scopeamendment.PathEntry, 0, len(paths))
	for _, p := range paths {
		entries = append(entries, scopeamendment.PathEntry{Path: p, Operation: scopeamendment.OperationModify})
	}
	a := &scopeamendment.Amendment{
		ID:             uuid.New(),
		RunID:          runID,
		StageID:        stageID,
		Paths:          entries,
		Reason:         "the coupled seam needs this file",
		Status:         status,
		DecisionReason: decisionReason,
		RequestedAt:    time.Now().UTC(),
	}
	sar.mu.Lock()
	sar.rows[a.ID] = a
	sar.order = append(sar.order, a.ID)
	sar.mu.Unlock()
	return a.ID
}

// stageScopeAmendmentServer wires a Server with only the ScopeAmendmentRepo the
// resolver needs.
func stageScopeAmendmentServer() (*Server, *fakeScopeAmendmentRepo) {
	sar := newFakeScopeAmendmentRepo()
	return New(Config{Addr: "127.0.0.1:0", ScopeAmendmentRepo: sar}), sar
}

func pathsOf(recs []prompt.MidStageAmendedScopePath) []string {
	out := make([]string, 0, len(recs))
	for _, r := range recs {
		out = append(out, r.Path)
	}
	return out
}

// An APPROVED amendment on THE STAGE UNDER REVIEW is returned, carrying the
// authorizing amendment id and the operator's decision reason verbatim.
func TestStageApprovedAmendmentScopePaths_ApprovedSameStage(t *testing.T) {
	s, sar := stageScopeAmendmentServer()
	runID, stageID := uuid.New(), uuid.New()
	id := seedStageAmendment(sar, runID, stageID, scopeamendment.StatusApproved,
		strptr("the audit category table is the coupled registration"), "backend/internal/audit/categories.go")

	got := s.stageApprovedAmendmentScopePaths(context.Background(), runID, stageID, planWithScope("backend/internal/foo/foo.go"))
	if len(got) != 1 {
		t.Fatalf("got %d records, want 1: %+v", len(got), got)
	}
	if got[0].Path != "backend/internal/audit/categories.go" {
		t.Errorf("Path = %q", got[0].Path)
	}
	if got[0].AmendmentID != id.String() {
		t.Errorf("AmendmentID = %q, want %q", got[0].AmendmentID, id)
	}
	if got[0].DecisionReason != "the audit category table is the coupled registration" {
		t.Errorf("DecisionReason = %q", got[0].DecisionReason)
	}
}

// A DENIED amendment confers nothing — the over-correction guard. Treating a
// refused request as a grant would silently bless scope the operator withheld,
// a worse defect than the one #2874 repairs.
func TestStageApprovedAmendmentScopePaths_DeniedExcluded(t *testing.T) {
	s, sar := stageScopeAmendmentServer()
	runID, stageID := uuid.New(), uuid.New()
	seedStageAmendment(sar, runID, stageID, scopeamendment.StatusDenied,
		strptr("out of scope for this slice"), "backend/internal/denied/denied.go")

	if got := s.stageApprovedAmendmentScopePaths(context.Background(), runID, stageID, planWithScope("backend/internal/foo/foo.go")); got != nil {
		t.Errorf("denied amendment must confer nothing; got %+v", got)
	}
}

// A PENDING amendment confers nothing: undecided is not authorized.
func TestStageApprovedAmendmentScopePaths_PendingExcluded(t *testing.T) {
	s, sar := stageScopeAmendmentServer()
	runID, stageID := uuid.New(), uuid.New()
	seedStageAmendment(sar, runID, stageID, scopeamendment.StatusPending, nil, "backend/internal/pending/pending.go")

	if got := s.stageApprovedAmendmentScopePaths(context.Background(), runID, stageID, planWithScope("backend/internal/foo/foo.go")); got != nil {
		t.Errorf("pending amendment must confer nothing; got %+v", got)
	}
}

// CROSS-STAGE NON-LEAKAGE: an amendment approved on a SIBLING stage of the same
// run is never surfaced to the stage under review. It was never folded into this
// stage's enforced scope, so presenting it would assert in-scope a path the
// runner's scope gate would have rejected for this stage.
func TestStageApprovedAmendmentScopePaths_OtherStageExcluded(t *testing.T) {
	s, sar := stageScopeAmendmentServer()
	runID := uuid.New()
	stageUnderReview, siblingStage := uuid.New(), uuid.New()
	seedStageAmendment(sar, runID, siblingStage, scopeamendment.StatusApproved,
		strptr("approved for the OTHER stage"), "backend/internal/sibling/sibling.go")

	if got := s.stageApprovedAmendmentScopePaths(context.Background(), runID, stageUnderReview, planWithScope("backend/internal/foo/foo.go")); got != nil {
		t.Errorf("a sibling stage's approved amendment must not leak into this stage's records; got %+v", got)
	}
}

// A path already in the plan's raw scope.files is excluded — writeApprovedPlan
// already renders it, so naming it again would only restate existing scope.
func TestStageApprovedAmendmentScopePaths_AlreadyInRawScopeExcluded(t *testing.T) {
	s, sar := stageScopeAmendmentServer()
	runID, stageID := uuid.New(), uuid.New()
	seedStageAmendment(sar, runID, stageID, scopeamendment.StatusApproved, strptr("ok"),
		"backend/internal/foo/foo.go", "backend/internal/new/new.go")

	got := s.stageApprovedAmendmentScopePaths(context.Background(), runID, stageID, planWithScope("backend/internal/foo/foo.go"))
	if want := []string{"backend/internal/new/new.go"}; len(got) != 1 || got[0].Path != want[0] {
		t.Errorf("raw-scope path must be excluded; got %v, want %v", pathsOf(got), want)
	}
}

// Two approved amendments naming the SAME path render once, first-wins, so the
// oldest authorizing amendment is the one shown.
func TestStageApprovedAmendmentScopePaths_DuplicatePathFirstWins(t *testing.T) {
	s, sar := stageScopeAmendmentServer()
	runID, stageID := uuid.New(), uuid.New()
	first := seedStageAmendment(sar, runID, stageID, scopeamendment.StatusApproved, strptr("first"), "backend/internal/dup/dup.go")
	seedStageAmendment(sar, runID, stageID, scopeamendment.StatusApproved, strptr("second"), "backend/internal/dup/dup.go")

	got := s.stageApprovedAmendmentScopePaths(context.Background(), runID, stageID, planWithScope("backend/internal/foo/foo.go"))
	if len(got) != 1 {
		t.Fatalf("duplicate path must render once; got %+v", got)
	}
	if got[0].AmendmentID != first.String() || got[0].DecisionReason != "first" {
		t.Errorf("first-wins violated: got amendment %s reason %q, want %s/%q",
			got[0].AmendmentID, got[0].DecisionReason, first, "first")
	}
}

// An approved amendment with NO decision reason (the decision endpoint does not
// require one) is RETAINED with an empty reason — not dropped.
func TestStageApprovedAmendmentScopePaths_EmptyDecisionReasonRetained(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reason *string
	}{
		{"nil", nil},
		{"empty string", strptr("")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, sar := stageScopeAmendmentServer()
			runID, stageID := uuid.New(), uuid.New()
			seedStageAmendment(sar, runID, stageID, scopeamendment.StatusApproved, tc.reason, "backend/internal/noreason/x.go")

			got := s.stageApprovedAmendmentScopePaths(context.Background(), runID, stageID, planWithScope("backend/internal/foo/foo.go"))
			if len(got) != 1 {
				t.Fatalf("record must be retained with an empty reason; got %+v", got)
			}
			if got[0].DecisionReason != "" {
				t.Errorf("DecisionReason = %q, want empty", got[0].DecisionReason)
			}
		})
	}
}

// A nil ScopeAmendmentRepo returns nil with no allocation.
func TestStageApprovedAmendmentScopePaths_NilRepo(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0"})
	if got := s.stageApprovedAmendmentScopePaths(context.Background(), uuid.New(), uuid.New(), planWithScope("a.go")); got != nil {
		t.Errorf("nil ScopeAmendmentRepo: got %+v, want nil", got)
	}
}

// A ListByRun error FAILS OPEN: WARN-logged, contributes nothing, no panic. A
// provenance lookup must never block a review.
//
// The WARN LOG is the assertion that discriminates the branch, and it is here
// deliberately. Asserting only the nil return is VACUOUS: ListByRun returns
// (nil, err) on failure, so the loop below the guard iterates zero rows and the
// function returns nil whether or not the guard exists — verified empirically by
// deleting the guard and observing this test stay GREEN. The observable effect
// of the error branch is the operator-facing WARN naming the run and stage, so
// that is what is pinned.
func TestStageApprovedAmendmentScopePaths_ListErrorFailsOpen(t *testing.T) {
	s, sar := stageScopeAmendmentServer()
	var logBuf bytes.Buffer
	s.cfg.Logger = slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	runID, stageID := uuid.New(), uuid.New()
	seedStageAmendment(sar, runID, stageID, scopeamendment.StatusApproved, strptr("ok"), "backend/internal/new/new.go")
	sar.failListOn = 1 // fail the resolver's only ListByRun call

	if got := s.stageApprovedAmendmentScopePaths(context.Background(), runID, stageID, planWithScope("backend/internal/foo/foo.go")); got != nil {
		t.Errorf("list error must contribute nothing; got %+v", got)
	}
	logged := logBuf.String()
	for _, want := range []string{
		"mid-stage-amendment provenance contributes nothing for this run",
		runID.String(),
		stageID.String(),
		"transient list failure",
	} {
		if !strings.Contains(logged, want) {
			t.Errorf("fail-open WARN missing %q:\n%s", want, logged)
		}
	}
}

// A nil approvedPlan is tolerated (no raw scope to exclude against) and an empty
// scope.files does NOT short-circuit: an operator-approved amendment is
// authorization in its own right regardless of how thin the declared scope is.
func TestStageApprovedAmendmentScopePaths_NilAndEmptyPlanScope(t *testing.T) {
	for _, tc := range []struct {
		name string
		p    *plan.Plan
	}{
		{"nil plan", nil},
		{"empty scope.files", planWithScope()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, sar := stageScopeAmendmentServer()
			runID, stageID := uuid.New(), uuid.New()
			seedStageAmendment(sar, runID, stageID, scopeamendment.StatusApproved, strptr("ok"), "backend/internal/new/new.go")

			got := s.stageApprovedAmendmentScopePaths(context.Background(), runID, stageID, tc.p)
			if len(got) != 1 || got[0].Path != "backend/internal/new/new.go" {
				t.Errorf("approved amendment must resolve regardless of declared scope; got %v", pathsOf(got))
			}
		})
	}
}
