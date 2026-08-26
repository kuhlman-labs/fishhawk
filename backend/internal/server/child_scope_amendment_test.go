package server

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/scopeamendment"
)

// seedAmendment inserts one amendment row (in the given status) for runID into
// the shared fakeScopeAmendmentRepo, returning its id. It uses the fake's
// in-package fields directly rather than the Create→Decide path so a test can
// seed an APPROVED (or pending/denied) row BY CONSTRUCTION — the status is the
// control under test in several cases, so it must not be produced by exercising
// the control's own decision path.
func seedAmendment(sar *fakeScopeAmendmentRepo, runID uuid.UUID, status scopeamendment.Status, paths ...string) uuid.UUID {
	entries := make([]scopeamendment.PathEntry, 0, len(paths))
	for _, p := range paths {
		entries = append(entries, scopeamendment.PathEntry{Path: p, Operation: scopeamendment.OperationModify})
	}
	a := &scopeamendment.Amendment{
		ID:          uuid.New(),
		RunID:       runID,
		StageID:     uuid.New(),
		Paths:       entries,
		Reason:      "test",
		Status:      status,
		RequestedAt: time.Now().UTC(),
	}
	sar.mu.Lock()
	sar.rows[a.ID] = a
	sar.order = append(sar.order, a.ID)
	sar.mu.Unlock()
	return a.ID
}

// childScopeAmendmentServer wires a Server with the RunRepo + ScopeAmendmentRepo
// the resolver needs, returning both fakes so a test can seed children/amendments.
func childScopeAmendmentServer() (*Server, *orchestratorRepo, *fakeScopeAmendmentRepo) {
	rr := newOrchestratorRepo()
	rr.seedRun() // ensure the map is non-empty even before children are seeded
	sar := newFakeScopeAmendmentRepo()
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, ScopeAmendmentRepo: sar})
	return s, rr, sar
}

func TestChildApprovedAmendmentScopePaths_NilRunRepo(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0", ScopeAmendmentRepo: newFakeScopeAmendmentRepo()})
	if got := s.childApprovedAmendmentScopePaths(context.Background(), uuid.New()); got != nil {
		t.Errorf("nil RunRepo: got %v, want nil", got)
	}
}

func TestChildApprovedAmendmentScopePaths_NilScopeAmendmentRepo(t *testing.T) {
	rr := newOrchestratorRepo()
	rr.seedRun()
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr})
	if got := s.childApprovedAmendmentScopePaths(context.Background(), uuid.New()); got != nil {
		t.Errorf("nil ScopeAmendmentRepo: got %v, want nil", got)
	}
}

// A run with no decomposition children pays nothing and renders nothing — an
// ordinary run is not a decomposed parent.
func TestChildApprovedAmendmentScopePaths_NoChildren(t *testing.T) {
	s, _, _ := childScopeAmendmentServer()
	if got := s.childApprovedAmendmentScopePaths(context.Background(), uuid.New()); got != nil {
		t.Errorf("no children: got %v, want nil", got)
	}
}

// One child holding an approved amendment resolves to one record carrying the
// path, amendment id, child run id, and slice index.
func TestChildApprovedAmendmentScopePaths_ApprovedResolved(t *testing.T) {
	s, rr, sar := childScopeAmendmentServer()
	parent := uuid.New()
	child := rr.seedDecomposedChild(parent, 2, run.StateSucceeded)
	amID := seedAmendment(sar, child.ID, scopeamendment.StatusApproved, "backend/internal/foo/childonly.go")

	got := s.childApprovedAmendmentScopePaths(context.Background(), parent)
	if len(got) != 1 {
		t.Fatalf("got %d records, want 1: %+v", len(got), got)
	}
	r := got[0]
	if r.Path != "backend/internal/foo/childonly.go" {
		t.Errorf("path = %q", r.Path)
	}
	if r.AmendmentID != amID.String() {
		t.Errorf("amendment id = %q, want %q", r.AmendmentID, amID)
	}
	if r.ChildRunID != child.ID.String() {
		t.Errorf("child run id = %q, want %q", r.ChildRunID, child.ID)
	}
	if r.SliceIndex == nil || *r.SliceIndex != 2 {
		t.Errorf("slice index = %v, want 2", r.SliceIndex)
	}
}

// Pending and denied child amendments confer nothing.
func TestChildApprovedAmendmentScopePaths_PendingAndDeniedExcluded(t *testing.T) {
	s, rr, sar := childScopeAmendmentServer()
	parent := uuid.New()
	child := rr.seedDecomposedChild(parent, 0, run.StateSucceeded)
	seedAmendment(sar, child.ID, scopeamendment.StatusPending, "backend/internal/foo/pending.go")
	seedAmendment(sar, child.ID, scopeamendment.StatusDenied, "backend/internal/foo/denied.go")

	if got := s.childApprovedAmendmentScopePaths(context.Background(), parent); got != nil {
		t.Errorf("pending/denied only: got %v, want nil", got)
	}
}

// A child whose ListByRun errors contributes nothing while a sibling child's
// approved amendment still resolves (best-effort, never all-or-nothing) — C7.
func TestChildApprovedAmendmentScopePaths_PerChildListError_SiblingStillResolves(t *testing.T) {
	s, rr, sar := childScopeAmendmentServer()
	parent := uuid.New()
	// Slice 0 sorts first; its ListByRun (call #1) fails. Slice 1 (call #2)
	// resolves.
	failing := rr.seedDecomposedChild(parent, 0, run.StateSucceeded)
	sibling := rr.seedDecomposedChild(parent, 1, run.StateSucceeded)
	seedAmendment(sar, failing.ID, scopeamendment.StatusApproved, "backend/internal/foo/failing.go")
	amID := seedAmendment(sar, sibling.ID, scopeamendment.StatusApproved, "backend/internal/foo/sibling.go")
	sar.failListOn = 1 // the first per-child ListByRun (the sorted-first child) errors

	got := s.childApprovedAmendmentScopePaths(context.Background(), parent)
	if len(got) != 1 {
		t.Fatalf("got %d records, want 1 (only the sibling): %+v", len(got), got)
	}
	if got[0].Path != "backend/internal/foo/sibling.go" || got[0].AmendmentID != amID.String() {
		t.Errorf("resolved wrong record: %+v", got[0])
	}
}

// A listAllDecomposedChildren error degrades to nil (never blocks the review).
func TestChildApprovedAmendmentScopePaths_ListChildrenError(t *testing.T) {
	s, rr, _ := childScopeAmendmentServer()
	rr.listRunsErr = errors.New("boom")
	if got := s.childApprovedAmendmentScopePaths(context.Background(), uuid.New()); got != nil {
		t.Errorf("listAllDecomposedChildren error: got %v, want nil", got)
	}
}

// Two children amending the SAME path → one record (first-wins dedup).
func TestChildApprovedAmendmentScopePaths_SamePathDedup(t *testing.T) {
	s, rr, sar := childScopeAmendmentServer()
	parent := uuid.New()
	first := rr.seedDecomposedChild(parent, 0, run.StateSucceeded)
	second := rr.seedDecomposedChild(parent, 1, run.StateSucceeded)
	firstAm := seedAmendment(sar, first.ID, scopeamendment.StatusApproved, "backend/internal/foo/shared.go")
	seedAmendment(sar, second.ID, scopeamendment.StatusApproved, "backend/internal/foo/shared.go")

	got := s.childApprovedAmendmentScopePaths(context.Background(), parent)
	if len(got) != 1 {
		t.Fatalf("got %d records, want 1 (deduped): %+v", len(got), got)
	}
	// First-wins: the slice-0 child's amendment authorizes the rendered record.
	if got[0].AmendmentID != firstAm.String() {
		t.Errorf("dedup did not keep the first child's amendment: %+v", got[0])
	}
}

// Children seeded out of slice order render in slice-index order (determinism).
func TestChildApprovedAmendmentScopePaths_OrderedBySliceIndex(t *testing.T) {
	s, rr, sar := childScopeAmendmentServer()
	parent := uuid.New()
	// Seed slice 1 BEFORE slice 0 so map/seed order differs from slice order.
	late := rr.seedDecomposedChild(parent, 1, run.StateSucceeded)
	early := rr.seedDecomposedChild(parent, 0, run.StateSucceeded)
	seedAmendment(sar, late.ID, scopeamendment.StatusApproved, "z.go")
	seedAmendment(sar, early.ID, scopeamendment.StatusApproved, "a.go")

	got := s.childApprovedAmendmentScopePaths(context.Background(), parent)
	if len(got) != 2 {
		t.Fatalf("got %d records, want 2: %+v", len(got), got)
	}
	if *got[0].SliceIndex != 0 || *got[1].SliceIndex != 1 {
		t.Errorf("records not ordered by slice index: %+v", got)
	}
	if got[0].Path != "a.go" || got[1].Path != "z.go" {
		t.Errorf("path order = [%q, %q], want [a.go, z.go]", got[0].Path, got[1].Path)
	}
}
