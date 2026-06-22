package audit_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// makeChildRun creates a run parented to parentRunID. When
// decomposedFrom is non-nil the run is treated as a decomposed child;
// otherwise it is a plain CI-retry child.
func makeChildRun(t *testing.T, pool *pgxpool.Pool, parentRunID uuid.UUID, decomposedFrom *uuid.UUID) uuid.UUID {
	t.Helper()
	repo := run.NewPostgresRepository(pool)
	r, err := repo.CreateRun(context.Background(), run.CreateRunParams{
		Repo:           "kuhlman-labs/fishhawk",
		WorkflowID:     "feature_change",
		WorkflowSHA:    "deadbeef",
		TriggerSource:  run.TriggerCLI,
		ParentRunID:    &parentRunID,
		DecomposedFrom: decomposedFrom,
	})
	if err != nil {
		t.Fatalf("create child run: %v", err)
	}
	return r.ID
}

// TestChainsByParent_ExcludesDecomposedChildren seeds a parent run P,
// a CI-retry child C1 (decomposed_from=nil), and a decomposed child C2
// (decomposed_from=P). ChainsByParent(P, false) must return entries for
// P and C1 only — C2 is excluded from CI-retry chain walks.
func TestChainsByParent_ExcludesDecomposedChildren(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := audit.NewPostgresRepository(pool)

	parentID := makeRun(t, pool)
	c1ID := makeChildRun(t, pool, parentID, nil)
	c2ID := makeChildRun(t, pool, parentID, &parentID)

	appendEntry(t, repo, parentID, "run_dispatched", nil)
	appendEntry(t, repo, c1ID, "run_dispatched", nil)
	appendEntry(t, repo, c2ID, "run_dispatched", nil)

	entries, err := repo.ChainsByParent(context.Background(), parentID, false)
	if err != nil {
		t.Fatalf("ChainsByParent: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("len = %d, want 2 (parent + CI-retry child only)", len(entries))
	}
	for _, e := range entries {
		if e.RunID == nil || *e.RunID == c2ID {
			t.Errorf("decomposed child C2 leaked into result: entry run_id=%v", e.RunID)
		}
	}
}

// TestChainsByParent_IncludesDecomposedChildren reuses the same fixture
// but calls ChainsByParent(P, true) — all three entries must be present.
func TestChainsByParent_IncludesDecomposedChildren(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := audit.NewPostgresRepository(pool)

	parentID := makeRun(t, pool)
	c1ID := makeChildRun(t, pool, parentID, nil)
	c2ID := makeChildRun(t, pool, parentID, &parentID)

	appendEntry(t, repo, parentID, "run_dispatched", nil)
	appendEntry(t, repo, c1ID, "run_dispatched", nil)
	appendEntry(t, repo, c2ID, "run_dispatched", nil)

	entries, err := repo.ChainsByParent(context.Background(), parentID, true)
	if err != nil {
		t.Fatalf("ChainsByParent: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("len = %d, want 3 (parent + both children)", len(entries))
	}
}
