package webhook_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/postgres"
	"github.com/kuhlman-labs/fishhawk/backend/internal/webhook"
)

// newStore returns a PostgresStore wired to a fresh container with
// migrations applied. The pool's lifetime is bound to the test via
// t.Cleanup.
func newStore(t *testing.T) *webhook.PostgresStore {
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
	return webhook.NewPostgresStore(pool)
}

func TestPostgresStore_FirstMarkSucceeds(t *testing.T) {
	s := newStore(t)
	if err := s.Mark("delivery-1"); err != nil {
		t.Errorf("Mark: %v", err)
	}
}

func TestPostgresStore_DuplicateRejected(t *testing.T) {
	s := newStore(t)
	if err := s.Mark("delivery-2"); err != nil {
		t.Fatalf("first Mark: %v", err)
	}
	err := s.Mark("delivery-2")
	if !errors.Is(err, webhook.ErrDeliveryDuplicate) {
		t.Errorf("err = %v, want ErrDeliveryDuplicate", err)
	}
}

func TestPostgresStore_EmptyIDRejected(t *testing.T) {
	s := newStore(t)
	err := s.Mark("")
	if !errors.Is(err, webhook.ErrDeliveryMissing) {
		t.Errorf("err = %v, want ErrDeliveryMissing", err)
	}
}

func TestPostgresStore_DistinctIDsBothSucceed(t *testing.T) {
	s := newStore(t)
	if err := s.Mark("a"); err != nil {
		t.Errorf("Mark a: %v", err)
	}
	if err := s.Mark("b"); err != nil {
		t.Errorf("Mark b: %v", err)
	}
}

func TestPostgresStore_Evict(t *testing.T) {
	s := newStore(t)
	for _, id := range []string{"old-1", "old-2", "old-3"} {
		if err := s.Mark(id); err != nil {
			t.Fatal(err)
		}
	}

	// Evict everything older than now+1s — captures all rows we
	// just inserted. The cutoff is in the future so we exercise
	// the "delete all" path without needing to wait or mess with
	// received_at.
	n, err := s.Evict(context.Background(), time.Now().Add(time.Second))
	if err != nil {
		t.Fatalf("Evict: %v", err)
	}
	if n != 3 {
		t.Errorf("Evict returned %d rows, want 3", n)
	}

	// After eviction the IDs should be re-markable as if first-seen.
	if err := s.Mark("old-1"); err != nil {
		t.Errorf("Mark after evict: %v", err)
	}
}

func TestPostgresStore_EvictRespectsCutoff(t *testing.T) {
	s := newStore(t)
	for _, id := range []string{"keep-1", "keep-2"} {
		if err := s.Mark(id); err != nil {
			t.Fatal(err)
		}
	}

	// Cutoff well in the past: nothing should be evicted.
	n, err := s.Evict(context.Background(), time.Now().Add(-1*time.Hour))
	if err != nil {
		t.Fatalf("Evict: %v", err)
	}
	if n != 0 {
		t.Errorf("Evict returned %d rows, want 0", n)
	}

	// And the originals should still be duplicates.
	if err := s.Mark("keep-1"); !errors.Is(err, webhook.ErrDeliveryDuplicate) {
		t.Errorf("expected duplicate after non-evicting Evict, got %v", err)
	}
}
