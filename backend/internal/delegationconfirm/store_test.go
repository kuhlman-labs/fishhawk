package delegationconfirm

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/captain"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
)

func seedGlobal(t *testing.T, a audit.Repository, category string, payload map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(payload)
	kind := audit.ActorUser
	subj, _ := payload["subject"].(string)
	if _, err := a.AppendGlobalChained(context.Background(), audit.GlobalChainAppendParams{
		Timestamp: time.Now().UTC(), Category: category, ActorKind: &kind, ActorSubject: &subj, Payload: raw,
	}); err != nil {
		t.Fatalf("seed %s: %v", category, err)
	}
}

func confirmDecide(workflow, actor, hash string) func(State) (Event, error) {
	return func(st State) (Event, error) {
		return Confirm(st, Params{Repo: repo, Workflow: workflow, Actor: actor, ContentHash: hash})
	}
}

func countConfirmEntries(t *testing.T, s *Store, r string) int {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM audit_entries WHERE run_id IS NULL AND category = ANY($1) AND payload->>'repo' = $2`,
		Categories(), r).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

// TestStore_AppendAndReadRoundTrip: one Append writes exactly ONE run-less
// entry; Read folds it; two workflows do not interfere; a foreign repo's
// entries are not read back.
func TestStore_AppendAndReadRoundTrip(t *testing.T) {
	pool := pgtest.NewPool(t)
	a := audit.NewPostgresRepository(pool)
	s := NewStore(pool)
	ctx := context.Background()
	seedGlobal(t, a, captain.CategoryAssigned, map[string]any{"repo": repo, "subject": "github:cap"})
	seedGlobal(t, a, captain.CategoryAssigned, map[string]any{"repo": "other/x", "subject": "github:cap"})

	for _, wf := range []string{"wf1", "wf2"} {
		applied, err := s.Append(ctx, AppendParams{Repo: repo, Actor: "github:cap", ActorKind: audit.ActorUser, Timestamp: time.Now().UTC()},
			confirmDecide(wf, "github:cap", "h-"+wf))
		if err != nil {
			t.Fatalf("append %s: %v", wf, err)
		}
		if applied.Entry.RunID != nil || applied.Entry.Category != CategoryDelegationConfirmed {
			t.Fatalf("entry = %+v, want a run-less delegation_confirmed", applied.Entry)
		}
	}
	// A foreign-repo confirmation by construction.
	seedGlobal(t, a, CategoryDelegationConfirmed, map[string]any{"repo": "other/x", "subject": "github:cap", "workflow": "wf1", "content_hash": "z"})

	if n := countConfirmEntries(t, s, repo); n != 2 {
		t.Fatalf("entries = %d, want exactly 2 (one per Append)", n)
	}
	snap, err := s.Read(ctx, nil, repo)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	got := Statuses(snap.State, []string{"wf1", "wf2"})
	for i, wf := range []string{"wf1", "wf2"} {
		if got[i].Status != StatusConfirmed || got[i].Confirmation.ContentHash != "h-"+wf {
			t.Errorf("%s = %+v, want confirmed with h-%s", wf, got[i], wf)
		}
	}
	if len(snap.Entries) != 2 || snap.State.SkippedEntries != 0 {
		t.Errorf("read %d entries (skipped %d), want 2 and 0: a foreign repo leaked", len(snap.Entries), snap.State.SkippedEntries)
	}
}

// TestStore_AppendRefusedAppendsNothing: a decide refusal (not the captain)
// rolls back with nothing appended.
func TestStore_AppendRefusedAppendsNothing(t *testing.T) {
	pool := pgtest.NewPool(t)
	a := audit.NewPostgresRepository(pool)
	s := NewStore(pool)
	seedGlobal(t, a, captain.CategoryAssigned, map[string]any{"repo": repo, "subject": "github:cap"})
	_, err := s.Append(context.Background(), AppendParams{Repo: repo, Actor: "github:other", Timestamp: time.Now().UTC()},
		confirmDecide("wf", "github:other", "h"))
	if !errors.Is(err, ErrNotCaptain) {
		t.Fatalf("err = %v, want ErrNotCaptain", err)
	}
	if n := countConfirmEntries(t, s, repo); n != 0 {
		t.Fatalf("entries = %d, want 0", n)
	}
	if _, err := s.Append(context.Background(), AppendParams{}, nil); !errors.Is(err, ErrRepoRequired) {
		t.Errorf("empty repo: %v", err)
	}
	if _, err := s.Append(context.Background(), AppendParams{Repo: repo}, nil); !errors.Is(err, ErrDecideRequired) {
		t.Errorf("nil decide: %v", err)
	}
	if _, err := s.Read(context.Background(), nil, ""); !errors.Is(err, ErrRepoRequired) {
		t.Errorf("read empty repo: %v", err)
	}
	if _, err := s.Append(context.Background(), AppendParams{Repo: repo}, func(State) (Event, error) {
		return Event{Kind: CategoryDelegationConfirmed, Repo: "other/x", Workflow: "w", Subject: "s"}, nil
	}); !errors.Is(err, ErrEventRepoMismatch) {
		t.Errorf("repo mismatch: %v", err)
	}
}

// TestAppend_SharesCaptainLock (approval condition 3): the lock Append takes
// IS the captain record's lock — a real captain.Store.Apply for the same repo
// blocks while it is held, and one for a different repo does not.
func TestAppend_SharesCaptainLock(t *testing.T) {
	pool := pgtest.NewPool(t)
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", lockKey(nil, repo)); err != nil {
		t.Fatalf("lock: %v", err)
	}
	cs := captain.NewStore(pool)
	claim := func(r string) error {
		cctx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
		defer cancel()
		_, err := cs.Apply(cctx, captain.ApplyParams{Repo: r, Actor: "github:c", Timestamp: time.Now().UTC()},
			func(captain.State) (captain.Event, error) {
				return captain.Event{Kind: captain.CategoryClaimed, Repo: r, Subject: "github:c"}, nil
			})
		return err
	}
	if err := claim("other/x"); err != nil {
		t.Fatalf("an unrelated repo's captain verb failed: %v", err)
	}
	if err := claim(repo); err == nil {
		t.Fatal("a captain verb for the same repo committed while the delegation-confirm lock was held: the keys differ")
	}
}
