package scopeamendment_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/postgres"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/scopeamendment"
)

// harness bundles the repo plus a run + implement stage the
// amendments can hang off.
type harness struct {
	repo    scopeamendment.Repository
	runRepo run.Repository
	runID   uuid.UUID
	stageID uuid.UUID
}

func newHarness(t *testing.T) harness {
	t.Helper()
	url := pgtest.NewURL(t)
	if err := postgres.MigrateUp(url); err != nil {
		t.Fatalf("MigrateUp: %v", err)
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	runRepo := run.NewPostgresRepository(pool)
	r, err := runRepo.CreateRun(context.Background(), run.CreateRunParams{
		Repo:          "kuhlman-labs/fishhawk",
		WorkflowID:    "feature_change",
		WorkflowSHA:   "deadbeef",
		TriggerSource: run.TriggerCLI,
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	st, err := runRepo.CreateStage(context.Background(), run.CreateStageParams{
		RunID:        r.ID,
		Sequence:     1,
		Type:         run.StageTypeImplement,
		ExecutorKind: run.ExecutorAgent,
		ExecutorRef:  "fishhawk/runner@v1",
	})
	if err != nil {
		t.Fatalf("create stage: %v", err)
	}
	return harness{
		repo:    scopeamendment.NewPostgresRepository(pool),
		runRepo: runRepo,
		runID:   r.ID,
		stageID: st.ID,
	}
}

func (h harness) create(t *testing.T, paths ...scopeamendment.PathEntry) *scopeamendment.Amendment {
	t.Helper()
	if len(paths) == 0 {
		paths = []scopeamendment.PathEntry{{Path: "backend/internal/server/extra.go", Operation: scopeamendment.OperationModify}}
	}
	a, err := h.repo.Create(context.Background(), scopeamendment.CreateParams{
		RunID:   h.runID,
		StageID: h.stageID,
		Paths:   paths,
		Reason:  "the seam needs this file",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return a
}

func TestPostgres_Create_RoundTrips(t *testing.T) {
	h := newHarness(t)
	a := h.create(t,
		scopeamendment.PathEntry{Path: "a/b.go", Operation: scopeamendment.OperationModify},
		scopeamendment.PathEntry{Path: "c/new.go", Operation: scopeamendment.OperationCreate},
	)
	if a.ID == uuid.Nil {
		t.Error("ID is zero")
	}
	if a.Status != scopeamendment.StatusPending {
		t.Errorf("Status = %q, want pending", a.Status)
	}
	if a.RequestedAt.IsZero() {
		t.Error("RequestedAt zero")
	}
	got, err := h.repo.GetByID(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if len(got.Paths) != 2 || got.Paths[0].Path != "a/b.go" || got.Paths[1].Operation != scopeamendment.OperationCreate {
		t.Errorf("Paths round-trip mismatch: %+v", got.Paths)
	}
	if got.Reason != "the seam needs this file" {
		t.Errorf("Reason = %q", got.Reason)
	}
}

func TestPostgres_Create_RejectsInvalidPaths(t *testing.T) {
	h := newHarness(t)
	_, err := h.repo.Create(context.Background(), scopeamendment.CreateParams{
		RunID:   h.runID,
		StageID: h.stageID,
		Paths:   []scopeamendment.PathEntry{{Path: "../escape.go", Operation: scopeamendment.OperationModify}},
		Reason:  "r",
	})
	if err == nil {
		t.Fatal("Create with '..' path should error")
	}
}

func TestPostgres_ListByRun_OldestFirst(t *testing.T) {
	h := newHarness(t)
	first := h.create(t, scopeamendment.PathEntry{Path: "first.go", Operation: scopeamendment.OperationModify})
	second := h.create(t, scopeamendment.PathEntry{Path: "second.go", Operation: scopeamendment.OperationModify})

	items, err := h.repo.ListByRun(context.Background(), h.runID)
	if err != nil {
		t.Fatalf("ListByRun: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("len = %d, want 2", len(items))
	}
	if items[0].ID != first.ID || items[1].ID != second.ID {
		t.Errorf("order wrong: got [%s, %s], want [%s, %s]",
			items[0].ID, items[1].ID, first.ID, second.ID)
	}
	// Foreign run lists empty.
	empty, err := h.repo.ListByRun(context.Background(), uuid.New())
	if err != nil {
		t.Fatalf("ListByRun(foreign): %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("foreign run returned %d rows", len(empty))
	}
}

func TestPostgres_CountByStage_StatusBlind(t *testing.T) {
	h := newHarness(t)
	a := h.create(t)
	h.create(t)
	// Deny the first — the count must still include it (budget bounds
	// operator interruptions, not approvals).
	if _, err := h.repo.Decide(context.Background(), scopeamendment.DecideParams{
		ID: a.ID, Status: scopeamendment.StatusDenied, Reason: "no", DecidedBy: "github:operator",
	}); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	n, err := h.repo.CountByStage(context.Background(), h.stageID)
	if err != nil {
		t.Fatalf("CountByStage: %v", err)
	}
	if n != 2 {
		t.Errorf("count = %d, want 2 (denied rows still consume budget)", n)
	}
}

func TestPostgres_Decide_ApproveThenConflict(t *testing.T) {
	h := newHarness(t)
	a := h.create(t)
	decided, err := h.repo.Decide(context.Background(), scopeamendment.DecideParams{
		ID: a.ID, Status: scopeamendment.StatusApproved, Reason: "ok", DecidedBy: "github:operator",
	})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decided.Status != scopeamendment.StatusApproved {
		t.Errorf("Status = %q, want approved", decided.Status)
	}
	if decided.DecidedBy == nil || *decided.DecidedBy != "github:operator" {
		t.Errorf("DecidedBy = %v", decided.DecidedBy)
	}
	if decided.DecidedAt == nil {
		t.Error("DecidedAt nil after decision")
	}
	// Second decide → ErrAlreadyDecided.
	_, err = h.repo.Decide(context.Background(), scopeamendment.DecideParams{
		ID: a.ID, Status: scopeamendment.StatusDenied, Reason: "flip", DecidedBy: "github:other",
	})
	if !errors.Is(err, scopeamendment.ErrAlreadyDecided) {
		t.Errorf("err = %v, want ErrAlreadyDecided", err)
	}
}

func TestPostgres_Decide_NotFound(t *testing.T) {
	h := newHarness(t)
	_, err := h.repo.Decide(context.Background(), scopeamendment.DecideParams{
		ID: uuid.New(), Status: scopeamendment.StatusApproved, Reason: "r", DecidedBy: "github:operator",
	})
	if !errors.Is(err, scopeamendment.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestPostgres_Decide_RejectsBadStatus(t *testing.T) {
	h := newHarness(t)
	a := h.create(t)
	_, err := h.repo.Decide(context.Background(), scopeamendment.DecideParams{
		ID: a.ID, Status: scopeamendment.StatusPending, Reason: "r", DecidedBy: "github:operator",
	})
	if err == nil {
		t.Fatal("Decide with status=pending should error")
	}
}
