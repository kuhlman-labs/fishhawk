package concern_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/postgres"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// harness bundles the repo plus a run + implement stage the concerns
// hang off. Migration 0030's up path is exercised by MigrateUp here.
type harness struct {
	repo    concern.Repository
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
		repo:    concern.NewPostgresRepository(pool),
		runRepo: runRepo,
		runID:   r.ID,
		stageID: st.ID,
	}
}

func (h harness) insert(t *testing.T, seq int64, concerns ...concern.RaisedConcern) []*concern.Concern {
	t.Helper()
	if len(concerns) == 0 {
		concerns = []concern.RaisedConcern{{Severity: "medium", Category: "scope", Note: "drift"}}
	}
	rows, err := h.repo.InsertRaised(context.Background(), concern.InsertRaisedParams{
		RunID:                h.runID,
		StageID:              h.stageID,
		StageKind:            concern.StageKindImplement,
		ReviewerModel:        "claude-opus-4-8",
		OriginReviewSequence: seq,
		Concerns:             concerns,
	})
	if err != nil {
		t.Fatalf("InsertRaised: %v", err)
	}
	return rows
}

func TestPostgres_InsertRaised_RoundTrips(t *testing.T) {
	h := newHarness(t)
	rows := h.insert(t, 7,
		concern.RaisedConcern{Severity: "high", Category: "correctness", Note: "off-by-one"},
		concern.RaisedConcern{Severity: "weird-custom", Category: "", Note: "tolerated verbatim"},
	)
	if len(rows) != 2 {
		t.Fatalf("len = %d, want 2", len(rows))
	}
	for _, c := range rows {
		if c.ID == uuid.Nil {
			t.Error("ID is zero")
		}
		if c.State != concern.StateRaised {
			t.Errorf("State = %q, want raised", c.State)
		}
		if c.OriginReviewSequence != 7 {
			t.Errorf("OriginReviewSequence = %d, want 7", c.OriginReviewSequence)
		}
		if c.ReviewerModel == nil || *c.ReviewerModel != "claude-opus-4-8" {
			t.Errorf("ReviewerModel = %v", c.ReviewerModel)
		}
		if c.CreatedAt.IsZero() {
			t.Error("CreatedAt zero")
		}
	}
	// Unknown reviewer-emitted severity is stored verbatim (tolerant
	// TEXT, no CHECK).
	if rows[1].Severity != "weird-custom" {
		t.Errorf("Severity = %q, want weird-custom stored verbatim", rows[1].Severity)
	}
}

// TestPostgres_SuggestedPatch_RoundTrips covers the #1165 additive column:
// a concern inserted WITH a suggested_patch returns it verbatim through
// GetByIDs and ListByRun, and a concern inserted WITHOUT one defaults to the
// empty string (the column is NOT NULL with an empty-string default) rather
// than erroring or returning NULL.
func TestPostgres_SuggestedPatch_RoundTrips(t *testing.T) {
	h := newHarness(t)
	const patch = "--- a/foo.go\n+++ b/foo.go\n@@ -1 +1 @@\n-x\n+y\n"
	rows := h.insert(t, 9,
		concern.RaisedConcern{Severity: "low", Category: "correctness", Note: "typo", SuggestedPatch: patch},
		concern.RaisedConcern{Severity: "medium", Category: "scope", Note: "no patch"},
	)
	if len(rows) != 2 {
		t.Fatalf("len = %d, want 2", len(rows))
	}
	if rows[0].SuggestedPatch != patch {
		t.Errorf("InsertRaised[0].SuggestedPatch = %q, want %q verbatim", rows[0].SuggestedPatch, patch)
	}
	if rows[1].SuggestedPatch != "" {
		t.Errorf("InsertRaised[1].SuggestedPatch = %q, want empty default", rows[1].SuggestedPatch)
	}

	got, err := h.repo.GetByIDs(context.Background(), []uuid.UUID{rows[0].ID, rows[1].ID})
	if err != nil {
		t.Fatalf("GetByIDs: %v", err)
	}
	if got[0].SuggestedPatch != patch {
		t.Errorf("GetByIDs[0].SuggestedPatch = %q, want %q verbatim", got[0].SuggestedPatch, patch)
	}
	if got[1].SuggestedPatch != "" {
		t.Errorf("GetByIDs[1].SuggestedPatch = %q, want empty default", got[1].SuggestedPatch)
	}

	all, err := h.repo.ListByRun(context.Background(), h.runID)
	if err != nil {
		t.Fatalf("ListByRun: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("ListByRun len = %d, want 2", len(all))
	}
	byID := make(map[uuid.UUID]string, 2)
	for _, c := range all {
		byID[c.ID] = c.SuggestedPatch
	}
	if byID[rows[0].ID] != patch {
		t.Errorf("ListByRun patch row SuggestedPatch = %q, want %q verbatim", byID[rows[0].ID], patch)
	}
	if byID[rows[1].ID] != "" {
		t.Errorf("ListByRun no-patch row SuggestedPatch = %q, want empty default", byID[rows[1].ID])
	}
}

// TestPostgres_NewEvidenceAndSettledRef_RoundTripAllQueries covers migration
// 0069's two additive columns (E60.8 / #2353) through EVERY hand-edited sqlc
// query: InsertReviewConcern, GetReviewConcernsByIDs, ListReviewConcernsByRun,
// ListOpenReviewConcernsByRun and UpdateReviewConcernState's RETURNING.
//
// This breadth is the point. The ./db package was hand-edited rather than
// regenerated (a local `sqlc generate` churns every db package in the repo), so
// a column list and its Scan destination list can disagree — and that is a
// RUNTIME "expected N destination arguments in Scan, not M" failure, not a
// compile error. A round-trip that exercised only one query would leave the
// other four broken and green.
func TestPostgres_NewEvidenceAndSettledRef_RoundTripAllQueries(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	const evidence = "the fixup reverted backend/internal/server/gateview.go:441 to the pre-fix mapping; see the diff at HEAD~1"
	settledRef := uuid.New().String()

	rows := h.insert(t, 11,
		concern.RaisedConcern{
			Severity: "high", Category: "correctness", Note: "evidenced",
			NewEvidence: evidence, SettledRef: settledRef,
		},
		concern.RaisedConcern{Severity: "low", Category: "scope", Note: "bare"},
	)
	if len(rows) != 2 {
		t.Fatalf("len = %d, want 2", len(rows))
	}
	// 1. InsertReviewConcern's RETURNING.
	if rows[0].NewEvidence != evidence {
		t.Errorf("InsertRaised[0].NewEvidence = %q, want %q verbatim", rows[0].NewEvidence, evidence)
	}
	if rows[0].SettledRef != settledRef {
		t.Errorf("InsertRaised[0].SettledRef = %q, want %q verbatim", rows[0].SettledRef, settledRef)
	}
	if rows[1].NewEvidence != "" || rows[1].SettledRef != "" {
		t.Errorf("InsertRaised[1] = (%q, %q), want both empty (the no-evidence concern)",
			rows[1].NewEvidence, rows[1].SettledRef)
	}

	// 2. GetReviewConcernsByIDs.
	got, err := h.repo.GetByIDs(ctx, []uuid.UUID{rows[0].ID, rows[1].ID})
	if err != nil {
		t.Fatalf("GetByIDs: %v", err)
	}
	if got[0].NewEvidence != evidence || got[0].SettledRef != settledRef {
		t.Errorf("GetByIDs[0] = (%q, %q), want (%q, %q)",
			got[0].NewEvidence, got[0].SettledRef, evidence, settledRef)
	}
	if got[1].NewEvidence != "" || got[1].SettledRef != "" {
		t.Errorf("GetByIDs[1] = (%q, %q), want both empty", got[1].NewEvidence, got[1].SettledRef)
	}

	// 3. ListReviewConcernsByRun and 4. ListOpenReviewConcernsByRun. Both rows
	// are still open, so the two lists carry the same pair.
	for name, list := range map[string]func() ([]*concern.Concern, error){
		"ListByRun":     func() ([]*concern.Concern, error) { return h.repo.ListByRun(ctx, h.runID) },
		"ListOpenByRun": func() ([]*concern.Concern, error) { return h.repo.ListOpenByRun(ctx, h.runID) },
	} {
		all, err := list()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		byID := make(map[uuid.UUID]*concern.Concern, len(all))
		for _, c := range all {
			byID[c.ID] = c
		}
		if c := byID[rows[0].ID]; c == nil || c.NewEvidence != evidence || c.SettledRef != settledRef {
			t.Errorf("%s evidenced row = %+v, want NewEvidence=%q SettledRef=%q", name, c, evidence, settledRef)
		}
		if c := byID[rows[1].ID]; c == nil || c.NewEvidence != "" || c.SettledRef != "" {
			t.Errorf("%s bare row = %+v, want both fields empty", name, c)
		}
	}

	// 5. UpdateReviewConcernState's RETURNING (via ApplyResolution): a state
	// transition must not blank the evidence it returns.
	waived, err := h.repo.ApplyResolution(ctx, rows[0].ID, concern.StateWaived, "operator judged non-blocking")
	if err != nil {
		t.Fatalf("ApplyResolution: %v", err)
	}
	if waived.NewEvidence != evidence || waived.SettledRef != settledRef {
		t.Errorf("ApplyResolution returned (%q, %q), want (%q, %q) — the settled ledger reads this row",
			waived.NewEvidence, waived.SettledRef, evidence, settledRef)
	}
}

func TestPostgres_GetByIDs_InputOrderAndNotFound(t *testing.T) {
	h := newHarness(t)
	a := h.insert(t, 1)[0]
	b := h.insert(t, 2)[0]

	got, err := h.repo.GetByIDs(context.Background(), []uuid.UUID{b.ID, a.ID})
	if err != nil {
		t.Fatalf("GetByIDs: %v", err)
	}
	if len(got) != 2 || got[0].ID != b.ID || got[1].ID != a.ID {
		t.Errorf("input order not preserved: %+v", got)
	}

	_, err = h.repo.GetByIDs(context.Background(), []uuid.UUID{a.ID, uuid.New()})
	if !errors.Is(err, concern.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestPostgres_ListOpenByRun_ExcludesResolved(t *testing.T) {
	h := newHarness(t)
	a := h.insert(t, 1)[0]
	b := h.insert(t, 2)[0]

	// Walk a to a closed state: raised -> addressed_pending -> addressed.
	if err := h.repo.MarkAddressedPending(context.Background(), []uuid.UUID{a.ID}, "routed"); err != nil {
		t.Fatalf("MarkAddressedPending: %v", err)
	}
	if _, err := h.repo.ApplyResolution(context.Background(), a.ID, concern.StateAddressed, "confirmed"); err != nil {
		t.Fatalf("ApplyResolution: %v", err)
	}

	open, err := h.repo.ListOpenByRun(context.Background(), h.runID)
	if err != nil {
		t.Fatalf("ListOpenByRun: %v", err)
	}
	if len(open) != 1 || open[0].ID != b.ID {
		t.Errorf("open = %+v, want only %s", open, b.ID)
	}

	all, err := h.repo.ListByRun(context.Background(), h.runID)
	if err != nil {
		t.Fatalf("ListByRun: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("ListByRun len = %d, want 2", len(all))
	}
}

func TestPostgres_MarkAddressedPending_IdempotentAndReasoned(t *testing.T) {
	h := newHarness(t)
	a := h.insert(t, 1)[0]
	if err := h.repo.MarkAddressedPending(context.Background(), []uuid.UUID{a.ID}, "fix the seam"); err != nil {
		t.Fatalf("MarkAddressedPending: %v", err)
	}
	// Second routing (forced pass) is an idempotent no-op.
	if err := h.repo.MarkAddressedPending(context.Background(), []uuid.UUID{a.ID}, "again"); err != nil {
		t.Fatalf("MarkAddressedPending (repeat): %v", err)
	}
	got, err := h.repo.GetByIDs(context.Background(), []uuid.UUID{a.ID})
	if err != nil {
		t.Fatalf("GetByIDs: %v", err)
	}
	if got[0].State != concern.StateAddressedPending {
		t.Errorf("State = %q, want addressed_pending", got[0].State)
	}
	if got[0].StateReason != "fix the seam" {
		t.Errorf("StateReason = %q, want first routing's reason preserved", got[0].StateReason)
	}
}

// TestPostgres_ApplyResolution_ReopenWinsOverConfirm exercises the
// precedence rule end-to-end against the store, both orders (#964).
func TestPostgres_ApplyResolution_ReopenWinsOverConfirm(t *testing.T) {
	h := newHarness(t)

	// Order 1: confirm landed first, then a reopen — reopen applies.
	a := h.insert(t, 1)[0]
	if err := h.repo.MarkAddressedPending(context.Background(), []uuid.UUID{a.ID}, "routed"); err != nil {
		t.Fatalf("MarkAddressedPending: %v", err)
	}
	if _, err := h.repo.ApplyResolution(context.Background(), a.ID, concern.StateAddressed, "confirmed"); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	reopened, err := h.repo.ApplyResolution(context.Background(), a.ID, concern.StateReopened, "not actually fixed")
	if err != nil {
		t.Fatalf("reopen after confirm must apply: %v", err)
	}
	if reopened.State != concern.StateReopened {
		t.Errorf("State = %q, want reopened", reopened.State)
	}

	// Order 2: reopen first, then a late confirm — rejected with a
	// loggable transition error; the row stays reopened.
	_, err = h.repo.ApplyResolution(context.Background(), a.ID, concern.StateAddressed, "late confirm")
	var inv concern.InvalidTransitionError
	if !errors.As(err, &inv) {
		t.Fatalf("late confirm err = %v, want InvalidTransitionError", err)
	}
	got, err := h.repo.GetByIDs(context.Background(), []uuid.UUID{a.ID})
	if err != nil {
		t.Fatalf("GetByIDs: %v", err)
	}
	if got[0].State != concern.StateReopened {
		t.Errorf("State = %q, want reopened (never downgraded)", got[0].State)
	}
}

func TestPostgres_MigrationDown(t *testing.T) {
	// MigrateUp ran 0030 in newHarness; the down path is covered by the
	// shared migration test in internal/postgres, but assert here that a
	// fresh insert works post-migration as the smoke check.
	h := newHarness(t)
	rows := h.insert(t, 1)
	if len(rows) != 1 {
		t.Fatalf("insert after migration failed")
	}
}
