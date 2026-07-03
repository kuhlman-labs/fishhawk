package refinement

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
)

func TestPostgres_CreateGetRoundTrip(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := NewPostgresRepository(pool)

	draft := validDraft()
	draft.Children[1].DependsOn = []int{1}
	draft.Children[0].AcceptanceCriteria = []string{"crit A", "crit B"}
	sessionID := uuid.New()

	stored, err := repo.CreateDraft(context.Background(), CreateParams{
		SessionID: sessionID,
		Brief:     "a brief",
		Draft:     draft,
		Model:     "claude-opus-4-8",
	})
	if err != nil {
		t.Fatalf("CreateDraft: %v", err)
	}
	if stored.ID == uuid.Nil {
		t.Error("CreateDraft returned a nil id")
	}
	if stored.CreatedAt.IsZero() {
		t.Error("CreateDraft returned a zero created_at")
	}

	got, err := repo.GetDraft(context.Background(), stored.ID)
	if err != nil {
		t.Fatalf("GetDraft: %v", err)
	}
	if got.SessionID != sessionID || got.Brief != "a brief" || got.Model != "claude-opus-4-8" {
		t.Errorf("reloaded row = %+v, want session/brief/model preserved", got)
	}
	// JSONB round-trip preserves edges and criteria exactly.
	if got.Draft.Children[1].DependsOn == nil || got.Draft.Children[1].DependsOn[0] != 1 {
		t.Errorf("reloaded depends_on = %v, want [1]", got.Draft.Children[1].DependsOn)
	}
	if len(got.Draft.Children[0].AcceptanceCriteria) != 2 ||
		got.Draft.Children[0].AcceptanceCriteria[0] != "crit A" {
		t.Errorf("reloaded acceptance criteria = %v, want [crit A, crit B]", got.Draft.Children[0].AcceptanceCriteria)
	}
	if got.Draft.Epic.OutOfScope != draft.Epic.OutOfScope {
		t.Errorf("reloaded out_of_scope = %q, want %q", got.Draft.Epic.OutOfScope, draft.Epic.OutOfScope)
	}
}

func TestPostgres_CreateWithoutModel(t *testing.T) {
	// An empty model persists as NULL and reloads as "".
	pool := pgtest.NewPool(t)
	repo := NewPostgresRepository(pool)

	stored, err := repo.CreateDraft(context.Background(), CreateParams{
		SessionID: uuid.New(),
		Brief:     "b",
		Draft:     validDraft(),
	})
	if err != nil {
		t.Fatalf("CreateDraft: %v", err)
	}
	got, err := repo.GetDraft(context.Background(), stored.ID)
	if err != nil {
		t.Fatalf("GetDraft: %v", err)
	}
	if got.Model != "" {
		t.Errorf("reloaded model = %q, want empty (NULL column)", got.Model)
	}
}

func TestPostgres_GetDraftNotFound(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := NewPostgresRepository(pool)

	_, err := repo.GetDraft(context.Background(), uuid.New())
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetDraft on unknown id = %v, want ErrNotFound", err)
	}
}

func TestPostgres_ListForSessionIsolates(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := NewPostgresRepository(pool)

	sessionA := uuid.New()
	sessionB := uuid.New()

	for i := 0; i < 2; i++ {
		if _, err := repo.CreateDraft(context.Background(), CreateParams{
			SessionID: sessionA, Brief: "a", Draft: validDraft(),
		}); err != nil {
			t.Fatalf("CreateDraft A: %v", err)
		}
	}
	if _, err := repo.CreateDraft(context.Background(), CreateParams{
		SessionID: sessionB, Brief: "b", Draft: validDraft(),
	}); err != nil {
		t.Fatalf("CreateDraft B: %v", err)
	}

	listA, err := repo.ListForSession(context.Background(), sessionA)
	if err != nil {
		t.Fatalf("ListForSession A: %v", err)
	}
	if len(listA) != 2 {
		t.Errorf("session A list = %d drafts, want 2", len(listA))
	}
	for _, d := range listA {
		if d.SessionID != sessionA {
			t.Errorf("session A list contains a draft for session %s", d.SessionID)
		}
	}

	listB, err := repo.ListForSession(context.Background(), sessionB)
	if err != nil {
		t.Fatalf("ListForSession B: %v", err)
	}
	if len(listB) != 1 {
		t.Errorf("session B list = %d drafts, want 1", len(listB))
	}
}

func TestPostgres_OriginRoundTrip(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := NewPostgresRepository(pool)

	// An explicit non-default origin round-trips; an empty origin normalizes to
	// OriginBrief.
	amended, err := repo.CreateDraft(context.Background(), CreateParams{
		SessionID: uuid.New(), Brief: "b", Draft: validDraft(), Origin: OriginAmendment,
	})
	if err != nil {
		t.Fatalf("CreateDraft amendment: %v", err)
	}
	if amended.Origin != OriginAmendment {
		t.Errorf("origin = %q, want %q", amended.Origin, OriginAmendment)
	}
	got, err := repo.GetDraft(context.Background(), amended.ID)
	if err != nil {
		t.Fatalf("GetDraft: %v", err)
	}
	if got.Origin != OriginAmendment {
		t.Errorf("reloaded origin = %q, want %q", got.Origin, OriginAmendment)
	}

	defaulted, err := repo.CreateDraft(context.Background(), CreateParams{
		SessionID: uuid.New(), Brief: "b", Draft: validDraft(),
	})
	if err != nil {
		t.Fatalf("CreateDraft default origin: %v", err)
	}
	if defaulted.Origin != OriginBrief {
		t.Errorf("empty origin normalized to %q, want %q", defaulted.Origin, OriginBrief)
	}
}

func TestPostgres_DecisionRoundTrip(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := NewPostgresRepository(pool)

	sessionID := uuid.New()
	draft := validDraft()
	stored, err := repo.CreateDraft(context.Background(), CreateParams{
		SessionID: sessionID, Brief: "b", Draft: draft,
	})
	if err != nil {
		t.Fatalf("CreateDraft: %v", err)
	}
	hash, err := ContentHash(draft)
	if err != nil {
		t.Fatalf("ContentHash: %v", err)
	}

	dec, err := repo.RecordDecision(context.Background(), DecisionParams{
		SessionID:        sessionID,
		DraftID:          stored.ID,
		Decision:         DecisionApproved,
		Reason:           "looks good",
		DraftContentHash: hash,
		DecidedBy:        "github:operator",
	})
	if err != nil {
		t.Fatalf("RecordDecision: %v", err)
	}
	if dec.ID == uuid.Nil || dec.CreatedAt.IsZero() {
		t.Errorf("decision missing id/created_at: %+v", dec)
	}

	list, err := repo.ListDecisions(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("ListDecisions: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("ListDecisions = %d, want 1", len(list))
	}
	got := list[0]
	if got.DraftID != stored.ID || got.Decision != DecisionApproved ||
		got.Reason != "looks good" || got.DraftContentHash != hash || got.DecidedBy != "github:operator" {
		t.Errorf("reloaded decision = %+v, want fields preserved", got)
	}
}

func TestPostgres_DecisionFKToDraft(t *testing.T) {
	// A decision referencing a draft id that does not exist violates the FK and
	// fails at insert rather than persisting a dangling decision.
	pool := pgtest.NewPool(t)
	repo := NewPostgresRepository(pool)

	_, err := repo.RecordDecision(context.Background(), DecisionParams{
		SessionID:        uuid.New(),
		DraftID:          uuid.New(), // no such draft
		Decision:         DecisionApproved,
		Reason:           "r",
		DraftContentHash: "h",
	})
	if err == nil {
		t.Fatal("RecordDecision against a nonexistent draft succeeded; want FK violation")
	}
}

func TestPostgres_ContentHashSurvivesJSONBRoundTrip(t *testing.T) {
	// Hash-determinism guard: the content hash is computed over the DECODED
	// EpicDraft struct, so a persist -> read-back-through-JSONB -> re-hash must
	// yield the SAME digest. This fails if the struct-marshal determinism
	// assumption (or the JSONB round-trip) is ever wrong.
	pool := pgtest.NewPool(t)
	repo := NewPostgresRepository(pool)

	draft := validDraft()
	draft.Children[1].DependsOn = []int{1}
	draft.Children[0].AcceptanceCriteria = []string{"crit A", "crit B"}
	written, err := ContentHash(draft)
	if err != nil {
		t.Fatalf("ContentHash written: %v", err)
	}

	stored, err := repo.CreateDraft(context.Background(), CreateParams{
		SessionID: uuid.New(), Brief: "b", Draft: draft,
	})
	if err != nil {
		t.Fatalf("CreateDraft: %v", err)
	}
	reloaded, err := repo.GetDraft(context.Background(), stored.ID)
	if err != nil {
		t.Fatalf("GetDraft: %v", err)
	}
	readBack, err := ContentHash(reloaded.Draft)
	if err != nil {
		t.Fatalf("ContentHash read-back: %v", err)
	}
	if written != readBack {
		t.Errorf("content hash drifted across JSONB round-trip: written %s != read-back %s", written, readBack)
	}
}
