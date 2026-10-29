package concern_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	concerndb "github.com/kuhlman-labs/fishhawk/backend/internal/concern/db"
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

// TestPostgres_PersonaAttribution_RoundTripAllQueries pins migration 0092's
// three columns (E55.10 / #3755) through EVERY query that returns
// review_concerns rows — InsertReviewConcern's RETURNING,
// GetReviewConcernsByIDs, ListReviewConcernsByRun,
// ListOpenReviewConcernsByRun and UpdateReviewConcernState's RETURNING — so a
// hand-edited sqlc scan list that misses or mis-orders a column fails here
// against the real migrated schema. It also pins the VERBATIM role contract:
// an InsertRaisedParams that omits ReviewerRole stores ” (unattributed),
// never a defaulted "standard".
func TestPostgres_PersonaAttribution_RoundTripAllQueries(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	insertAs := func(role string, seq int64, c concern.RaisedConcern) *concern.Concern {
		t.Helper()
		rows, err := h.repo.InsertRaised(ctx, concern.InsertRaisedParams{
			RunID:                h.runID,
			StageID:              h.stageID,
			StageKind:            concern.StageKindImplement,
			ReviewerModel:        "claude-opus-4-8",
			ReviewerRole:         role,
			OriginReviewSequence: seq,
			Concerns:             []concern.RaisedConcern{c},
		})
		if err != nil {
			t.Fatalf("InsertRaised(role %q): %v", role, err)
		}
		return rows[0]
	}

	type want struct {
		role        string
		unverified  bool
		clampedFrom string
		severity    string
	}
	persona := insertAs("security-reviewer", 21, concern.RaisedConcern{
		Severity: "low", Category: "security", Note: "quoted a passage the remit never said",
		QuoteUnverified: true, SeverityClampedFrom: "high",
	})
	standard := insertAs(concern.ReviewerRoleStandard, 22, concern.RaisedConcern{
		Severity: "high", Category: "correctness", Note: "standard concern",
	})
	unattributed := insertAs("", 23, concern.RaisedConcern{
		Severity: "medium", Category: "scope", Note: "no role named",
	})
	wants := map[uuid.UUID]want{
		persona.ID:      {role: "security-reviewer", unverified: true, clampedFrom: "high", severity: "low"},
		standard.ID:     {role: concern.ReviewerRoleStandard, severity: "high"},
		unattributed.ID: {role: "", severity: "medium"},
	}
	check := func(query string, c *concern.Concern) {
		t.Helper()
		if c == nil {
			t.Errorf("%s: row missing", query)
			return
		}
		w := wants[c.ID]
		if c.ReviewerRole != w.role || c.QuoteUnverified != w.unverified ||
			c.SeverityClampedFrom != w.clampedFrom || c.Severity != w.severity {
			t.Errorf("%s row %s = (reviewer_role %q, quote_unverified %v, severity_clamped_from %q, severity %q), want (%q, %v, %q, %q)",
				query, c.ID, c.ReviewerRole, c.QuoteUnverified, c.SeverityClampedFrom, c.Severity,
				w.role, w.unverified, w.clampedFrom, w.severity)
		}
	}

	// 1. InsertReviewConcern's RETURNING.
	for _, c := range []*concern.Concern{persona, standard, unattributed} {
		check("InsertRaised", c)
	}
	// 2. GetReviewConcernsByIDs.
	got, err := h.repo.GetByIDs(ctx, []uuid.UUID{persona.ID, standard.ID, unattributed.ID})
	if err != nil {
		t.Fatalf("GetByIDs: %v", err)
	}
	for _, c := range got {
		check("GetByIDs", c)
	}
	// 3. ListReviewConcernsByRun and 4. ListOpenReviewConcernsByRun.
	for name, list := range map[string]func() ([]*concern.Concern, error){
		"ListByRun":     func() ([]*concern.Concern, error) { return h.repo.ListByRun(ctx, h.runID) },
		"ListOpenByRun": func() ([]*concern.Concern, error) { return h.repo.ListOpenByRun(ctx, h.runID) },
	} {
		all, err := list()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(all) != 3 {
			t.Fatalf("%s returned %d rows, want 3", name, len(all))
		}
		for _, c := range all {
			check(name, c)
		}
	}
	// 5. UpdateReviewConcernState's RETURNING: a transition must not blank
	// the attribution or the ingest markers.
	waived, err := h.repo.ApplyResolution(ctx, persona.ID, concern.StateWaived, "operator judged non-blocking")
	if err != nil {
		t.Fatalf("ApplyResolution: %v", err)
	}
	check("ApplyResolution", waived)
}

// TestPostgres_ProvenanceAndCheckKey_RoundTripAllQueries pins migration
// 0094's two columns (E80.3 / #3760) through EVERY query that returns
// review_concerns rows — InsertReviewConcern's RETURNING,
// GetReviewConcernsByIDs, ListReviewConcernsByRun,
// ListOpenReviewConcernsByRun and UpdateReviewConcernState's RETURNING — so a
// hand-edited sqlc scan list that misses or mis-orders either column fails
// here against the real migrated schema. A server_check insert reads back its
// provenance and per-concern check_key (two concerns of ONE entry carry
// DIFFERENT keys, so a key stamped entry-wide instead of per concern is
// caught); an insert naming neither reads back empty for both, the legacy
// default every reviewer-raised row carries.
func TestPostgres_ProvenanceAndCheckKey_RoundTripAllQueries(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	checkRows, err := h.repo.InsertRaised(ctx, concern.InsertRaisedParams{
		RunID:                h.runID,
		StageID:              h.stageID,
		StageKind:            concern.StageKindImplement,
		Provenance:           concern.ProvenanceServerCheck,
		OriginReviewSequence: 31,
		Concerns: []concern.RaisedConcern{
			{Severity: "high", Category: "security", Note: "credential-shaped addition at a.go:2", CheckKey: "diff_secrets|github-pat-classic|a.go"},
			{Severity: "high", Category: "security", Note: "credential-shaped addition at b.go:7", CheckKey: "diff_secrets|aws-access-key-id|b.go"},
		},
	})
	if err != nil {
		t.Fatalf("InsertRaised(server_check): %v", err)
	}
	reviewer := h.insert(t, 32)[0]

	type want struct{ provenance, checkKey string }
	wants := map[uuid.UUID]want{
		checkRows[0].ID: {concern.ProvenanceServerCheck, "diff_secrets|github-pat-classic|a.go"},
		checkRows[1].ID: {concern.ProvenanceServerCheck, "diff_secrets|aws-access-key-id|b.go"},
		reviewer.ID:     {"", ""},
	}
	check := func(query string, c *concern.Concern) {
		t.Helper()
		if c == nil {
			t.Errorf("%s: row missing", query)
			return
		}
		w, ok := wants[c.ID]
		if !ok {
			t.Errorf("%s: unexpected row %s", query, c.ID)
			return
		}
		if c.Provenance != w.provenance || c.CheckKey != w.checkKey {
			t.Errorf("%s row %s = (provenance %q, check_key %q), want (%q, %q)",
				query, c.ID, c.Provenance, c.CheckKey, w.provenance, w.checkKey)
		}
		if got, wantSC := c.IsServerCheck(), w.provenance == concern.ProvenanceServerCheck; got != wantSC {
			t.Errorf("%s row %s IsServerCheck() = %v, want %v", query, c.ID, got, wantSC)
		}
	}

	// 1. InsertReviewConcern's RETURNING.
	for _, c := range []*concern.Concern{checkRows[0], checkRows[1], reviewer} {
		check("InsertRaised", c)
	}
	// 2. GetReviewConcernsByIDs.
	got, err := h.repo.GetByIDs(ctx, []uuid.UUID{checkRows[0].ID, checkRows[1].ID, reviewer.ID})
	if err != nil {
		t.Fatalf("GetByIDs: %v", err)
	}
	for _, c := range got {
		check("GetByIDs", c)
	}
	// 3. ListReviewConcernsByRun and 4. ListOpenReviewConcernsByRun.
	for name, list := range map[string]func() ([]*concern.Concern, error){
		"ListByRun":     func() ([]*concern.Concern, error) { return h.repo.ListByRun(ctx, h.runID) },
		"ListOpenByRun": func() ([]*concern.Concern, error) { return h.repo.ListOpenByRun(ctx, h.runID) },
	} {
		all, err := list()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(all) != 3 {
			t.Fatalf("%s returned %d rows, want 3", name, len(all))
		}
		for _, c := range all {
			check(name, c)
		}
	}
	// 5. UpdateReviewConcernState's RETURNING: a human waive must not blank
	// the server_check marker (the historian reads it off the waived row).
	waived, err := h.repo.ApplyResolution(ctx, checkRows[0].ID, concern.StateWaived, "known test fixture")
	if err != nil {
		t.Fatalf("ApplyResolution: %v", err)
	}
	check("ApplyResolution", waived)
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

// TestPostgres_SupersededReRoutesToAddressedPending is the E45.83 / #3618
// store-level round-trip: a retry supersedes an implement-review concern, and
// an operator fix-up re-routes THAT row back into the open set through
// MarkAddressedPending. It drives the REAL pgtest-backed store, so it covers
// both the Go state machine and the from-state-guarded UPDATE, and it proves
// migration 0030's `state` column admits the value (the column carries no
// CHECK, so the write is a plain TEXT update — asserted rather than assumed).
//
// COUNTERFACTUAL: deleting the StateSuperseded -> StateAddressedPending edge
// from validTransitions makes MarkAddressedPending return
// InvalidTransitionError and the row stay superseded. The assertion READS THE
// STATE BACK from Postgres rather than trusting the call's error, because the
// production caller (the fix-up handler) treats a MarkAddressedPending failure
// as warn-only.
func TestPostgres_SupersededReRoutesToAddressedPending(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	rows := h.insert(t, 42, concern.RaisedConcern{
		Severity: "medium", Category: "correctness", Note: "the retry never re-checked the nil guard",
	})
	c := rows[0]

	// Seed the superseded state BY CONSTRUCTION through ApplyResolution —
	// exactly as retryStageAs discards a prior attempt's open concerns — so a
	// deletion of the edge under test lands the RED on the re-route assertion
	// below, never on this setup.
	sup, err := h.repo.ApplyResolution(ctx, c.ID, concern.StateSuperseded, "superseded by retry attempt 2")
	if err != nil {
		t.Fatalf("ApplyResolution -> superseded: %v", err)
	}
	if sup.State != concern.StateSuperseded {
		t.Fatalf("seeded State = %q, want superseded", sup.State)
	}
	if sup.State.IsOpen() {
		t.Fatal("superseded row reports IsOpen() = true — it must stay CLOSED until an operator routes it")
	}

	const reason = "the nil guard is still missing on the retried tree"
	if merr := h.repo.MarkAddressedPending(ctx, []uuid.UUID{c.ID}, reason); merr != nil {
		t.Fatalf("MarkAddressedPending on a superseded row: %v (the #3618 edge is what admits this)", merr)
	}

	// Read the COMMITTED state back rather than trusting the call's nil error.
	got, err := h.repo.GetByIDs(ctx, []uuid.UUID{c.ID})
	if err != nil {
		t.Fatalf("GetByIDs: %v", err)
	}
	reRouted := got[0]
	if reRouted.State != concern.StateAddressedPending {
		t.Fatalf("committed State = %q, want addressed_pending — the superseded row did not re-enter the open set", reRouted.State)
	}
	if !reRouted.State.IsOpen() {
		t.Error("re-routed concern reports IsOpen() = false, want true")
	}
	if reRouted.StateReason != reason {
		t.Errorf("StateReason = %q, want the operator's routing reason %q", reRouted.StateReason, reason)
	}
	// The reviewer, round and severity survive the round trip — the provenance
	// the free-text operator_concern fallback destroys.
	if reRouted.ReviewerModel == nil || *reRouted.ReviewerModel != "claude-opus-4-8" {
		t.Errorf("ReviewerModel = %v, want the original reviewer preserved", reRouted.ReviewerModel)
	}
	if reRouted.OriginReviewSequence != 42 {
		t.Errorf("OriginReviewSequence = %d, want 42 (the original round)", reRouted.OriginReviewSequence)
	}
	if reRouted.Severity != "medium" || reRouted.Category != "correctness" {
		t.Errorf("severity/category = %q/%q, want medium/correctness preserved", reRouted.Severity, reRouted.Category)
	}
	if reRouted.Note != "the retry never re-checked the nil guard" {
		t.Errorf("Note = %q, want the reviewer's note preserved", reRouted.Note)
	}
}

// TestPostgres_MarkAddressedPending_RefusesWaived is the negative half of the
// #3618 edge through the REAL store: widening superseded did NOT make the
// genuinely terminal states re-routable. A waived row fails
// InvalidTransitionError and stays waived in Postgres.
func TestPostgres_MarkAddressedPending_RefusesWaived(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	c := h.insert(t, 43)[0]
	if _, err := h.repo.ApplyResolution(ctx, c.ID, concern.StateWaived, "operator waived"); err != nil {
		t.Fatalf("ApplyResolution -> waived: %v", err)
	}

	err := h.repo.MarkAddressedPending(ctx, []uuid.UUID{c.ID}, "try to re-route a waived concern")
	var inv concern.InvalidTransitionError
	if !errors.As(err, &inv) {
		t.Fatalf("MarkAddressedPending on a waived row = %v, want InvalidTransitionError", err)
	}
	if inv.From != concern.StateWaived || inv.To != concern.StateAddressedPending {
		t.Errorf("InvalidTransitionError = %s -> %s, want waived -> addressed_pending", inv.From, inv.To)
	}
	got, err := h.repo.GetByIDs(ctx, []uuid.UUID{c.ID})
	if err != nil {
		t.Fatalf("GetByIDs: %v", err)
	}
	if got[0].State != concern.StateWaived {
		t.Errorf("committed State = %q, want waived (untouched)", got[0].State)
	}
}

// TestNormalizedReviewerRole pins the identity normalization (E55.10 / #3755):
// the empty (unattributed) role reads as standard so a legacy row keeps
// today's veto behaviour, while standard and every persona name pass through
// verbatim (trimmed) — a persona can never collapse onto standard.
func TestNormalizedReviewerRole(t *testing.T) {
	for _, tc := range []struct {
		in, want string
	}{
		{"", concern.ReviewerRoleStandard},
		{"   ", concern.ReviewerRoleStandard},
		{concern.ReviewerRoleStandard, concern.ReviewerRoleStandard},
		{"security-reviewer", "security-reviewer"},
		{"  security-reviewer\t", "security-reviewer"},
		{"Standard", "Standard"},
	} {
		if got := concern.NormalizedReviewerRole(tc.in); got != tc.want {
			t.Errorf("NormalizedReviewerRole(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	if concern.ReviewerRoleStandard != "standard" {
		t.Errorf("ReviewerRoleStandard = %q, want %q (persisted on every standard-reviewer row)", concern.ReviewerRoleStandard, "standard")
	}
}

// rollupHarness is a parent run with an implement stage and a decomposition
// child with its own implement stage, on a non-Fishhawk repository (E94.1 /
// #4692). The pool is exposed so a test can read updated_at back raw.
type rollupHarness struct {
	repo                  concern.Repository
	pool                  *pgxpool.Pool
	parentRun, childRun   uuid.UUID
	parentStage, childStg uuid.UUID
}

func newRollupHarness(t *testing.T) rollupHarness {
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
	mk := func() (uuid.UUID, uuid.UUID) {
		r, err := runRepo.CreateRun(context.Background(), run.CreateRunParams{
			Repo: "acme-widgets/storefront", WorkflowID: "feature_change", WorkflowSHA: "cafef00d", TriggerSource: run.TriggerCLI,
		})
		if err != nil {
			t.Fatalf("create run: %v", err)
		}
		st, err := runRepo.CreateStage(context.Background(), run.CreateStageParams{
			RunID: r.ID, Sequence: 1, Type: run.StageTypeImplement, ExecutorKind: run.ExecutorAgent, ExecutorRef: "fishhawk/runner@v1",
		})
		if err != nil {
			t.Fatalf("create stage: %v", err)
		}
		return r.ID, st.ID
	}
	h := rollupHarness{repo: concern.NewPostgresRepository(pool), pool: pool}
	h.parentRun, h.parentStage = mk()
	h.childRun, h.childStg = mk()
	return h
}

func (h rollupHarness) insertOn(t *testing.T, runID, stageID uuid.UUID, p concern.InsertRaisedParams, cs ...concern.RaisedConcern) []*concern.Concern {
	t.Helper()
	p.RunID, p.StageID, p.StageKind, p.Concerns = runID, stageID, concern.StageKindImplement, cs
	if p.ReviewerModel == "" {
		p.ReviewerModel = "gpt-6-astra"
	}
	rows, err := h.repo.InsertRaised(context.Background(), p)
	if err != nil {
		t.Fatalf("InsertRaised: %v", err)
	}
	return rows
}

func (h rollupHarness) byRun(t *testing.T, runID uuid.UUID) []*concern.Concern {
	t.Helper()
	rows, err := h.repo.ListByRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("ListByRun: %v", err)
	}
	return rows
}

func (h rollupHarness) get(t *testing.T, id uuid.UUID) *concern.Concern {
	t.Helper()
	rows, err := h.repo.GetByIDs(context.Background(), []uuid.UUID{id})
	if err != nil {
		t.Fatalf("GetByIDs: %v", err)
	}
	return rows[0]
}

func (h rollupHarness) importSources(t *testing.T, srcs ...*concern.Concern) []*concern.Concern {
	t.Helper()
	out, err := h.repo.ImportRolledUp(context.Background(), concern.ImportRolledUpParams{
		ParentRunID: h.parentRun, ParentStageID: h.parentStage, Sources: srcs,
	})
	if err != nil {
		t.Fatalf("ImportRolledUp: %v", err)
	}
	return out
}

// TestImportRolledUp_CopiesFields: the copy lands on the parent run and
// implement stage as raised, links back through rolled_up_from and keeps the
// source's origin round, reviewer, severity, category, note, evidence,
// provenance and check_key verbatim.
func TestImportRolledUp_CopiesFields(t *testing.T) {
	h := newRollupHarness(t)
	src := h.insertOn(t, h.childRun, h.childStg, concern.InsertRaisedParams{
		OriginReviewSequence: 4711, ReviewerRole: "architect", Provenance: concern.ProvenanceServerCheck,
	}, concern.RaisedConcern{Severity: "high", Category: "correctness", Note: "cart total drops tax", SuggestedPatch: "--- a\n+++ b\n",
		NewEvidence: "repro", SettledRef: "c-9", QuoteUnverified: true, SeverityClampedFrom: "critical", CheckKey: "permission_drift:k1"})[0]

	copies := h.importSources(t, src)
	if len(copies) != 1 {
		t.Fatalf("copies = %d, want 1", len(copies))
	}
	cp := copies[0]
	if cp.ID == src.ID || cp.RunID != h.parentRun || cp.StageID != h.parentStage || cp.StageKind != concern.StageKindImplement {
		t.Errorf("copy placement = id %s run %s stage %s kind %s, want a new id on parent %s / %s implement", cp.ID, cp.RunID, cp.StageID, cp.StageKind, h.parentRun, h.parentStage)
	}
	if cp.State != concern.StateRaised || cp.RolledUpFrom == nil || *cp.RolledUpFrom != src.ID {
		t.Errorf("copy state = %s rolled_up_from = %v, want raised / %s", cp.State, cp.RolledUpFrom, src.ID)
	}
	if cp.StateReason != concern.RolledUpStateReason(src) {
		t.Errorf("copy state_reason = %q, want %q", cp.StateReason, concern.RolledUpStateReason(src))
	}
	if cp.OriginReviewSequence != 4711 || cp.ReviewerModel == nil || *cp.ReviewerModel != "gpt-6-astra" || cp.ReviewerRole != "architect" ||
		cp.Severity != "high" || cp.Category != "correctness" || cp.Note != "cart total drops tax" || cp.SuggestedPatch != src.SuggestedPatch ||
		cp.NewEvidence != "repro" || cp.SettledRef != "c-9" || !cp.QuoteUnverified || cp.SeverityClampedFrom != "critical" ||
		cp.Provenance != concern.ProvenanceServerCheck || cp.CheckKey != "permission_drift:k1" {
		t.Errorf("copy fields not verbatim: %+v", cp)
	}
	if cp.Outcome != "" {
		t.Errorf("copy outcome = %q, want empty", cp.Outcome)
	}
}

// TestImportRolledUp_MarksSourceCarried: the source's outcome reads carried;
// its state, state_reason and updated_at are unchanged.
func TestImportRolledUp_MarksSourceCarried(t *testing.T) {
	h := newRollupHarness(t)
	src := h.insertOn(t, h.childRun, h.childStg, concern.InsertRaisedParams{OriginReviewSequence: 3},
		concern.RaisedConcern{Severity: "medium", Category: "tests", Note: "no coupon test"})[0]
	var before time.Time
	if err := h.pool.QueryRow(context.Background(), `SELECT updated_at FROM review_concerns WHERE id = $1`, src.ID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	h.importSources(t, src)
	got := h.get(t, src.ID)
	if got.Outcome != concern.OutcomeCarried {
		t.Errorf("source outcome = %q, want carried", got.Outcome)
	}
	if got.State != src.State || got.StateReason != src.StateReason {
		t.Errorf("source state/reason = %s/%q, want unchanged %s/%q", got.State, got.StateReason, src.State, src.StateReason)
	}
	var after time.Time
	if err := h.pool.QueryRow(context.Background(), `SELECT updated_at FROM review_concerns WHERE id = $1`, src.ID).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if !after.Equal(before) {
		t.Errorf("source updated_at moved %v -> %v; an outcome write must not touch it", before, after)
	}
}

// TestImportRolledUp_Idempotent: a second import of the same sources returns
// no error and no copies; the parent keeps exactly one row per source.
func TestImportRolledUp_Idempotent(t *testing.T) {
	h := newRollupHarness(t)
	srcs := h.insertOn(t, h.childRun, h.childStg, concern.InsertRaisedParams{OriginReviewSequence: 5},
		concern.RaisedConcern{Severity: "high", Note: "a"}, concern.RaisedConcern{Severity: "medium", Note: "b"})
	if n := len(h.importSources(t, srcs...)); n != 2 {
		t.Fatalf("first import copies = %d, want 2", n)
	}
	again, err := h.repo.ImportRolledUp(context.Background(), concern.ImportRolledUpParams{
		ParentRunID: h.parentRun, ParentStageID: h.parentStage, Sources: srcs,
	})
	if err != nil {
		t.Fatalf("second ImportRolledUp: %v (the ON CONFLICT clause must absorb the duplicate)", err)
	}
	if len(again) != 0 {
		t.Errorf("second import copies = %d, want 0", len(again))
	}
	if n := len(h.byRun(t, h.parentRun)); n != 2 {
		t.Errorf("parent rows = %d, want 2 (one per source)", n)
	}
}

// TestImportRolledUp_Concurrent: two goroutines import the same source;
// exactly one copy exists.
func TestImportRolledUp_Concurrent(t *testing.T) {
	h := newRollupHarness(t)
	src := h.insertOn(t, h.childRun, h.childStg, concern.InsertRaisedParams{OriginReviewSequence: 6},
		concern.RaisedConcern{Severity: "high", Note: "race"})[0]
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = h.repo.ImportRolledUp(context.Background(), concern.ImportRolledUpParams{
				ParentRunID: h.parentRun, ParentStageID: h.parentStage, Sources: []*concern.Concern{src},
			})
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("import %d: %v", i, err)
		}
	}
	if n := len(h.byRun(t, h.parentRun)); n != 1 {
		t.Errorf("parent rows after two concurrent imports = %d, want 1", n)
	}
}

// TestImportRolledUp_SkipsSourceNoLongerOpen (concern 92d4e9da, C1): a source
// read open by the caller and superseded before the copy is written — a retry
// sweep landing between the fan-in's read and its import — gets no copy and
// keeps an empty outcome. The caller's snapshot still says raised, so only the
// copy statement's own state re-check can refuse it.
func TestImportRolledUp_SkipsSourceNoLongerOpen(t *testing.T) {
	h := newRollupHarness(t)
	src := h.insertOn(t, h.childRun, h.childStg, concern.InsertRaisedParams{OriginReviewSequence: 7},
		concern.RaisedConcern{Severity: "high", Note: "swept before the copy"})[0]
	if _, err := h.repo.ApplyResolution(context.Background(), src.ID, concern.StateSuperseded, "superseded by stage retry (retry seq 9)"); err != nil {
		t.Fatalf("supersede source: %v", err)
	}
	if src.State != concern.StateRaised {
		t.Fatalf("precondition: the caller's snapshot reads %s, want the stale raised", src.State)
	}
	if copies := h.importSources(t, src); len(copies) != 0 {
		t.Errorf("copies of a superseded source = %d, want 0: the copy statement must re-check the source is still open", len(copies))
	}
	if n := len(h.byRun(t, h.parentRun)); n != 0 {
		t.Errorf("parent rows = %d, want 0", n)
	}
	if got := h.get(t, src.ID); got.Outcome != "" {
		t.Errorf("superseded source outcome = %q, want empty: a source with no copy is not carried", got.Outcome)
	}
}

// TestImportRolledUp_LocksSourceAgainstConcurrentSupersession (concern
// 92d4e9da, C1): while an import transaction holds its uncommitted copy, a
// concurrent supersession of the source WAITS for that transaction, so the
// sweep's later parent-ledger scan sees the committed copy. Deterministic: the
// test waits until Postgres reports the supersession blocked on a lock (or
// fails if it completes first), then commits.
func TestImportRolledUp_LocksSourceAgainstConcurrentSupersession(t *testing.T) {
	h := newRollupHarness(t)
	ctx := context.Background()
	src := h.insertOn(t, h.childRun, h.childStg, concern.InsertRaisedParams{OriginReviewSequence: 8},
		concern.RaisedConcern{Severity: "high", Note: "swept during the copy"})[0]
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := concerndb.New(tx).ImportRolledUpReviewConcern(ctx, concerndb.ImportRolledUpReviewConcernParams{
		ID: uuid.New(), ParentRunID: h.parentRun, ParentStageID: h.parentStage,
		StateReason: concern.RolledUpStateReason(src), SourceID: src.ID,
	}); err != nil {
		t.Fatalf("copy inside the open import transaction: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := h.repo.ApplyResolution(ctx, src.ID, concern.StateSuperseded, "superseded by stage retry (retry seq 9)")
		done <- err
	}()
	deadline := time.Now().Add(30 * time.Second)
	for blocked := false; !blocked; {
		select {
		case err := <-done:
			t.Fatalf("the source's supersession completed (err %v) while the import transaction held an uncommitted copy: the copy statement does not lock its source, so a retry sweep's parent scan can run before the copy exists and strand it raised", err)
		default:
		}
		var waiting int
		if err := h.pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		blocked = waiting > 0
		if !blocked && time.Now().After(deadline) {
			t.Fatal("the source's supersession neither blocked nor completed within 30s")
		}
		if !blocked {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit import: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("supersession after the import committed: %v", err)
	}
	var copies int
	for _, r := range h.byRun(t, h.parentRun) {
		if r.RolledUpFrom != nil && *r.RolledUpFrom == src.ID {
			copies++
		}
	}
	if copies != 1 {
		t.Errorf("parent copies visible once the supersession returned = %d, want 1 (the sweep's parent scan must see it)", copies)
	}
	if got := h.get(t, src.ID); got.State != concern.StateSuperseded {
		t.Errorf("source state = %s, want superseded", got.State)
	}
}

// TestImportRolledUp_RequiresParent: a nil parent run or stage is refused
// before any write; an empty source list is a no-op.
func TestImportRolledUp_RequiresParent(t *testing.T) {
	h := newRollupHarness(t)
	src := h.insertOn(t, h.childRun, h.childStg, concern.InsertRaisedParams{OriginReviewSequence: 1},
		concern.RaisedConcern{Severity: "high", Note: "x"})[0]
	for name, p := range map[string]concern.ImportRolledUpParams{
		"nil run":   {ParentStageID: h.parentStage, Sources: []*concern.Concern{src}},
		"nil stage": {ParentRunID: h.parentRun, Sources: []*concern.Concern{src}},
	} {
		if _, err := h.repo.ImportRolledUp(context.Background(), p); err == nil || !strings.Contains(err.Error(), "required") {
			t.Errorf("%s: err = %v, want the Go-side refusal naming the required parent id (before any statement)", name, err)
		}
	}
	if out, err := h.repo.ImportRolledUp(context.Background(), concern.ImportRolledUpParams{ParentRunID: h.parentRun, ParentStageID: h.parentStage}); err != nil || out != nil {
		t.Errorf("empty sources = %v, %v; want nil, nil", out, err)
	}
	if got := h.get(t, src.ID); got.Outcome != "" {
		t.Errorf("source outcome after refused imports = %q, want empty", got.Outcome)
	}
}

// TestStampOpenAtMerge_StampsOpenAndSuperseded: raised, addressed_pending,
// reopened and superseded rows across two runs get open_at_merge; waived,
// deferred, addressed and addressed_by_condition keep an empty outcome; state, state_reason
// and updated_at are unchanged; a re-stamp affects 0 rows.
func TestStampOpenAtMerge_StampsOpenAndSuperseded(t *testing.T) {
	h := newRollupHarness(t)
	ctx := context.Background()
	mk := func(runID, stageID uuid.UUID) *concern.Concern {
		return h.insertOn(t, runID, stageID, concern.InsertRaisedParams{OriginReviewSequence: 9}, concern.RaisedConcern{Severity: "low", Note: "n"})[0]
	}
	walk := func(c *concern.Concern, states ...concern.State) {
		for _, st := range states {
			var err error
			if st == concern.StateAddressedPending {
				err = h.repo.MarkAddressedPending(ctx, []uuid.UUID{c.ID}, "routed")
			} else {
				_, err = h.repo.ApplyResolution(ctx, c.ID, st, "walk "+string(st))
			}
			if err != nil {
				t.Fatalf("walk %s -> %s: %v", c.ID, st, err)
			}
		}
	}
	raised := mk(h.parentRun, h.parentStage)
	pending := mk(h.childRun, h.childStg)
	walk(pending, concern.StateAddressedPending)
	reopened := mk(h.parentRun, h.parentStage)
	walk(reopened, concern.StateAddressedPending, concern.StateAddressed, concern.StateReopened)
	superseded := mk(h.childRun, h.childStg)
	walk(superseded, concern.StateSuperseded)
	waived := mk(h.parentRun, h.parentStage)
	walk(waived, concern.StateWaived)
	deferred := mk(h.parentRun, h.parentStage)
	walk(deferred, concern.StateDeferred)
	addressed := mk(h.childRun, h.childStg)
	walk(addressed, concern.StateAddressedPending, concern.StateAddressed)
	byCond := mk(h.parentRun, h.parentStage)
	walk(byCond, concern.StateAddressedByCondition)

	type snap struct {
		state, reason string
		updated       time.Time
	}
	read := func(id uuid.UUID) snap {
		var s snap
		if err := h.pool.QueryRow(ctx, `SELECT state, state_reason, updated_at FROM review_concerns WHERE id = $1`, id).Scan(&s.state, &s.reason, &s.updated); err != nil {
			t.Fatal(err)
		}
		return s
	}
	all := []*concern.Concern{raised, pending, reopened, superseded, waived, deferred, addressed, byCond}
	before := map[uuid.UUID]snap{}
	for _, c := range all {
		before[c.ID] = read(c.ID)
	}
	n, err := h.repo.StampOpenAtMerge(ctx, []uuid.UUID{h.parentRun, h.childRun})
	if err != nil {
		t.Fatalf("StampOpenAtMerge: %v", err)
	}
	if n != 4 {
		t.Errorf("stamped rows = %d, want 4", n)
	}
	for _, c := range []*concern.Concern{raised, pending, reopened, superseded} {
		if got := h.get(t, c.ID).Outcome; got != concern.OutcomeOpenAtMerge {
			t.Errorf("%s (%s) outcome = %q, want open_at_merge", c.ID, before[c.ID].state, got)
		}
	}
	for _, c := range []*concern.Concern{waived, deferred, addressed, byCond} {
		if got := h.get(t, c.ID).Outcome; got != "" {
			t.Errorf("%s (%s) outcome = %q, want empty", c.ID, before[c.ID].state, got)
		}
	}
	for _, c := range all {
		if after := read(c.ID); after.state != before[c.ID].state || after.reason != before[c.ID].reason || !after.updated.Equal(before[c.ID].updated) {
			t.Errorf("%s changed beyond outcome: %+v -> %+v", c.ID, before[c.ID], after)
		}
	}
	if n, err := h.repo.StampOpenAtMerge(ctx, []uuid.UUID{h.parentRun, h.childRun}); err != nil || n != 0 {
		t.Errorf("re-stamp = %d, %v; want 0, nil", n, err)
	}
	if n, err := h.repo.StampOpenAtMerge(ctx, nil); err != nil || n != 0 {
		t.Errorf("empty run ids = %d, %v; want 0, nil", n, err)
	}
}

// TestStampOpenAtMerge_KeepsRecordedOutcome: an OPEN row already carried
// keeps carried.
func TestStampOpenAtMerge_KeepsRecordedOutcome(t *testing.T) {
	h := newRollupHarness(t)
	src := h.insertOn(t, h.childRun, h.childStg, concern.InsertRaisedParams{OriginReviewSequence: 2},
		concern.RaisedConcern{Severity: "high", Note: "carried then merged"})[0]
	h.importSources(t, src)
	if _, err := h.repo.StampOpenAtMerge(context.Background(), []uuid.UUID{h.childRun}); err != nil {
		t.Fatalf("StampOpenAtMerge: %v", err)
	}
	got := h.get(t, src.ID)
	if !got.State.IsOpen() {
		t.Fatalf("precondition: source state = %s, want open", got.State)
	}
	if got.Outcome != concern.OutcomeCarried {
		t.Errorf("open carried source outcome after the merge stamp = %q, want carried (a recorded outcome is never overwritten)", got.Outcome)
	}
}
