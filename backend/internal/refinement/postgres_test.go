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
