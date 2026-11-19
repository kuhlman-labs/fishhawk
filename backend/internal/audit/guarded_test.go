package audit_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
)

// guarded_test.go drives AppendChainedGuarded / AppendChainedGuardedTx
// (E80.28 / #5229, ADR-143 A1 V1) against a REAL Postgres: the run-row lock
// and the tx-bound reader live in the store, so a fake cannot exercise them.
// makeRun comes from postgres_test.go (same package).
//
// Hand-run counterfactuals (performed; each restored byte-identically):
//
//   - H3 (lock ordering): move the guard call above LockRunForUpdate in
//     AppendChainedGuardedTx. While A's guard is parked no row lock is held,
//     so B's plain append commits first —
//     TestPostgres_AppendChainedGuarded_GuardHoldsRunLock goes RED on "B
//     returned while A's guard held the run lock".
//   - H4 (refusal aborts the append): ignore the guard's returned error in
//     AppendChainedGuardedTx. The guard refuses (a sentinel, or head B != A),
//     so only the error check stands between it and AppendChainedTx —
//     _GuardRefusalWritesNothing and _GuardSeesCommittedPush go RED on the
//     appended row.

const guardedWaiveCat = "concern_waived"

// guardedHeadMovedError is the test's typed refusal, standing in for the
// server's waiveProposalStaleError: the caller must be able to errors.As it.
type guardedHeadMovedError struct{ Want, Got string }

func (e *guardedHeadMovedError) Error() string {
	return fmt.Sprintf("head moved: want %s, got %s", e.Want, e.Got)
}

// appendHead appends a head-recording row (pull_request_opened /
// fixup_pushed) naming sha through the plain chained path.
func appendHead(t *testing.T, repo audit.Repository, runID uuid.UUID, category, sha string) *audit.Entry {
	t.Helper()
	payload, _ := json.Marshal(map[string]any{"head_sha": sha})
	e, err := repo.AppendChained(context.Background(), audit.ChainAppendParams{
		RunID: runID, Timestamp: time.Now().UTC(), Category: category, Payload: payload,
	})
	if err != nil {
		t.Fatalf("append %s: %v", category, err)
	}
	return e
}

// newestHead returns the head_sha of the highest-sequence pull_request_opened
// or fixup_pushed row read through read.
func newestHead(ctx context.Context, read audit.ChainReader, runID uuid.UUID) (string, error) {
	var head string
	var seq int64 = -1
	for _, cat := range []string{"pull_request_opened", "fixup_pushed"} {
		es, err := read.ListForRunByCategory(ctx, runID, cat)
		if err != nil {
			return "", err
		}
		for _, e := range es {
			if e.Sequence <= seq {
				continue
			}
			var p struct {
				HeadSHA string `json:"head_sha"`
			}
			if json.Unmarshal(e.Payload, &p) == nil {
				head, seq = p.HeadSHA, e.Sequence
			}
		}
	}
	return head, nil
}

func guardedWaiveParams(runID uuid.UUID) audit.ChainAppendParams {
	payload, _ := json.Marshal(map[string]any{
		"concern_id": uuid.NewString(), "reason": "confirmed agent waive proposal",
		"proposal_sequence": 1, "proposal_head_sha": "aaaa", "salt": uuid.NewString(),
	})
	return audit.ChainAppendParams{RunID: runID, Timestamp: time.Now().UTC(), Category: guardedWaiveCat, Payload: payload}
}

func guarded(pool *pgxpool.Pool) audit.GuardedChainAppender {
	return audit.NewPostgresRepository(pool).(audit.GuardedChainAppender)
}

func countGuardedRows(t *testing.T, pool *pgxpool.Pool, runID uuid.UUID, category string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM audit_entries WHERE run_id = $1 AND category = $2`, runID, category).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", category, err)
	}
	return n
}

// verifyRunChain re-reads the run's committed chain and checks every entry's
// prev_hash link and recomputed entry_hash, so a guarded append is proven to
// take the same hashing path as every other chained append.
func verifyRunChain(t *testing.T, repo audit.Repository, runID uuid.UUID) []*audit.Entry {
	t.Helper()
	es, err := repo.ListForRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("list run chain: %v", err)
	}
	var prev *string
	for i, e := range es {
		if (prev == nil) != (e.PrevHash == nil) || (prev != nil && *prev != *e.PrevHash) {
			t.Fatalf("entry %d (seq %d) prev_hash = %v, want %v", i, e.Sequence, e.PrevHash, prev)
		}
		h, err := audit.ComputeEntryHash(audit.HashInputs{
			RunID: e.RunID, StageID: e.StageID, Timestamp: e.Timestamp, Category: e.Category,
			ActorKind: e.ActorKind, ActorSubject: e.ActorSubject, Payload: e.Payload, PrevHash: e.PrevHash,
		})
		if err != nil {
			t.Fatalf("recompute hash for seq %d: %v", e.Sequence, err)
		}
		if h != e.EntryHash {
			t.Fatalf("entry seq %d hash = %s, recomputed %s: the chain does not verify", e.Sequence, e.EntryHash, h)
		}
		hh := e.EntryHash
		prev = &hh
	}
	return es
}

// A head-recording push that COMMITTED before the guarded append is seen by
// the guard through the tx reader, and the guard's typed refusal reaches the
// caller unwrapped with nothing written.
func TestPostgres_AppendChainedGuarded_GuardSeesCommittedPush(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := audit.NewPostgresRepository(pool)
	runID := makeRun(t, pool)
	appendHead(t, repo, runID, "pull_request_opened", "aaaa")
	appendHead(t, repo, runID, "fixup_pushed", "bbbb")

	e, err := guarded(pool).AppendChainedGuarded(context.Background(), guardedWaiveParams(runID),
		func(ctx context.Context, read audit.ChainReader) error {
			head, err := newestHead(ctx, read, runID)
			if err != nil {
				return err
			}
			if head != "aaaa" {
				return &guardedHeadMovedError{Want: "aaaa", Got: head}
			}
			return nil
		})
	// Committed state decides this test; the error identity is checked
	// non-fatally so a swallowed refusal still reaches the row count below.
	var moved *guardedHeadMovedError
	if !errors.As(err, &moved) {
		t.Errorf("err = %v (entry %+v), want the guard's *guardedHeadMovedError", err, e)
	} else if moved.Got != "bbbb" {
		t.Errorf("guard saw head %q, want the committed push bbbb", moved.Got)
	}
	if e != nil {
		t.Errorf("refused append returned an entry %+v", e)
	}
	if n := countGuardedRows(t, pool, runID, guardedWaiveCat); n != 0 {
		t.Errorf("committed %s rows = %d, want 0", guardedWaiveCat, n)
	}
}

// A refusing guard leaves the chain length unchanged and the chain verifying.
func TestPostgres_AppendChainedGuarded_GuardRefusalWritesNothing(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := audit.NewPostgresRepository(pool)
	runID := makeRun(t, pool)
	appendHead(t, repo, runID, "pull_request_opened", "aaaa")
	before := len(verifyRunChain(t, repo, runID))

	sentinel := errors.New("refused under the lock")
	e, err := guarded(pool).AppendChainedGuarded(context.Background(), guardedWaiveParams(runID),
		func(context.Context, audit.ChainReader) error { return sentinel })
	if !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want the sentinel", err)
	}
	if e != nil {
		t.Errorf("refused append returned an entry %+v", e)
	}
	if after := len(verifyRunChain(t, repo, runID)); after != before {
		t.Errorf("chain length %d -> %d: a refused guarded append wrote a row", before, after)
	}
}

// An admitting guard appends ONE chained entry that links to the prior entry
// and verifies under the shared hashing path.
func TestPostgres_AppendChainedGuarded_GuardAdmitAppendsChained(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := audit.NewPostgresRepository(pool)
	runID := makeRun(t, pool)
	prior := appendHead(t, repo, runID, "pull_request_opened", "aaaa")

	e, err := guarded(pool).AppendChainedGuarded(context.Background(), guardedWaiveParams(runID),
		func(ctx context.Context, read audit.ChainReader) error {
			head, err := newestHead(ctx, read, runID)
			if err != nil {
				return err
			}
			if head != "aaaa" {
				return &guardedHeadMovedError{Want: "aaaa", Got: head}
			}
			return nil
		})
	if err != nil {
		t.Fatalf("AppendChainedGuarded: %v", err)
	}
	if e == nil || e.Sequence <= prior.Sequence || e.PrevHash == nil || *e.PrevHash != prior.EntryHash {
		t.Fatalf("entry = %+v, want a chained entry after seq %d linking hash %s", e, prior.Sequence, prior.EntryHash)
	}
	es := verifyRunChain(t, repo, runID)
	if last := es[len(es)-1]; last.ID != e.ID {
		t.Errorf("chain tail = %s, want the guarded entry %s", last.ID, e.ID)
	}
	if n := countGuardedRows(t, pool, runID, guardedWaiveCat); n != 1 {
		t.Errorf("committed %s rows = %d, want 1", guardedWaiveCat, n)
	}
}

// The guard runs UNDER the run-row lock: while A's guard is parked, B's plain
// chained append on the same run blocks, and once A commits B is ordered
// after it. Counterfactual H3: move the guard above LockRunForUpdate and B
// commits during the hold (RED on both assertions).
func TestPostgres_AppendChainedGuarded_GuardHoldsRunLock(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := audit.NewPostgresRepository(pool)
	runID := makeRun(t, pool)
	appendHead(t, repo, runID, "pull_request_opened", "aaaa")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	inGuard := make(chan struct{})
	release := make(chan struct{})
	type result struct {
		e   *audit.Entry
		err error
	}
	aDone := make(chan result, 1)
	go func() {
		e, err := guarded(pool).AppendChainedGuarded(ctx, guardedWaiveParams(runID),
			func(context.Context, audit.ChainReader) error {
				close(inGuard)
				select {
				case <-release:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			})
		aDone <- result{e, err}
	}()

	select {
	case <-inGuard:
	case <-ctx.Done():
		t.Fatal("A's guard was never entered")
	}

	bDone := make(chan result, 1)
	go func() {
		payload, _ := json.Marshal(map[string]any{"head_sha": "bbbb"})
		e, err := repo.AppendChained(ctx, audit.ChainAppendParams{
			RunID: runID, Timestamp: time.Now().UTC(), Category: "fixup_pushed", Payload: payload,
		})
		bDone <- result{e, err}
	}()

	select {
	case b := <-bDone:
		close(release)
		<-aDone
		t.Fatalf("B returned while A's guard held the run lock (entry %+v, err %v): the guard does not run under the run-row lock", b.e, b.err)
	case <-time.After(300 * time.Millisecond):
	}

	close(release)
	a := <-aDone
	if a.err != nil {
		t.Fatalf("A: %v", a.err)
	}
	b := <-bDone
	if b.err != nil {
		t.Fatalf("B: %v", b.err)
	}
	if b.e.Sequence <= a.e.Sequence {
		t.Fatalf("B.Sequence %d <= A.Sequence %d: the push was not ordered after the guarded append", b.e.Sequence, a.e.Sequence)
	}
	verifyRunChain(t, repo, runID)
}

// The tx-bound reader answers only for the locked run: asking it for another
// run refuses with ErrGuardReaderRunScope, and a guard that propagates that
// refusal writes nothing.
func TestPostgres_AppendChainedGuarded_ReaderScopedToRun(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := audit.NewPostgresRepository(pool)
	runID := makeRun(t, pool)
	other := makeRun(t, pool)
	appendHead(t, repo, other, "pull_request_opened", "cccc")

	var ownErr, otherErr error
	var otherRows []*audit.Entry
	_, err := guarded(pool).AppendChainedGuarded(context.Background(), guardedWaiveParams(runID),
		func(ctx context.Context, read audit.ChainReader) error {
			_, ownErr = read.ListForRunByCategory(ctx, runID, "pull_request_opened")
			otherRows, otherErr = read.ListForRunByCategory(ctx, other, "pull_request_opened")
			return otherErr
		})
	if ownErr != nil {
		t.Fatalf("reader refused its own run: %v", ownErr)
	}
	if !errors.Is(otherErr, audit.ErrGuardReaderRunScope) {
		t.Errorf("reader for another run: rows %d, err %v; want ErrGuardReaderRunScope", len(otherRows), otherErr)
	}
	if otherRows != nil {
		t.Errorf("reader returned %d rows of another run", len(otherRows))
	}
	if !errors.Is(err, audit.ErrGuardReaderRunScope) {
		t.Errorf("append err = %v, want the scope refusal propagated", err)
	}
	if n := countGuardedRows(t, pool, runID, guardedWaiveCat); n != 0 {
		t.Errorf("committed %s rows = %d, want 0", guardedWaiveCat, n)
	}
}

// A nil guard is refused rather than silently degrading to a plain append.
func TestPostgres_AppendChainedGuarded_NilGuardRefused(t *testing.T) {
	pool := pgtest.NewPool(t)
	runID := makeRun(t, pool)
	e, err := guarded(pool).AppendChainedGuarded(context.Background(), guardedWaiveParams(runID), nil)
	if !errors.Is(err, audit.ErrNilAppendGuard) {
		t.Errorf("err = %v (entry %+v), want ErrNilAppendGuard", err, e)
	}
	if n := countGuardedRows(t, pool, runID, guardedWaiveCat); n != 0 {
		t.Errorf("committed %s rows = %d, want 0", guardedWaiveCat, n)
	}
}

// An unknown run is refused at the lock, before the guard runs.
func TestPostgres_AppendChainedGuarded_UnknownRun(t *testing.T) {
	pool := pgtest.NewPool(t)
	ran := false
	_, err := guarded(pool).AppendChainedGuarded(context.Background(), guardedWaiveParams(uuid.New()),
		func(context.Context, audit.ChainReader) error { ran = true; return nil })
	if err == nil {
		t.Fatal("append on an unknown run succeeded")
	}
	if ran {
		t.Error("the guard ran before the run-row lock was granted")
	}
}
