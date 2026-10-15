package server

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/timescale"
)

// Durable review terminal writes against REAL Postgres (E72.64 / #4106,
// test 29). The fake-path suite (review_terminal_write_test.go) proves the
// control flow; these prove the one fact it cannot: that the lost-ack probe's
// (category, stage_id, Timestamp.Equal) match holds across the real
// timestamptz round trip, so a commit whose acknowledgement was lost is found
// rather than appended a second time.

// pgFlakyAudit wraps the real Postgres audit repository. action decides, per
// append call (1-based), whether to pass through, fail WITHOUT writing, or
// COMMIT the row through the real repository and then return an error (the
// lost-acknowledgement shape).
type pgFlakyAudit struct {
	audit.Repository
	action func(n int, p audit.ChainAppendParams) flakyAction

	mu    sync.Mutex
	calls int
}

func (w *pgFlakyAudit) AppendChained(ctx context.Context, p audit.ChainAppendParams) (*audit.Entry, error) {
	w.mu.Lock()
	w.calls++
	n := w.calls
	w.mu.Unlock()
	act := flakyOK
	if w.action != nil {
		act = w.action(n, p)
	}
	switch act {
	case flakyFail:
		return nil, errors.New("pg flaky: injected append failure")
	case flakyCommitThenFail:
		if _, err := w.Repository.AppendChained(ctx, p); err != nil {
			return nil, err
		}
		return nil, errors.New("pg flaky: committed but the ack was lost")
	}
	return w.Repository.AppendChained(ctx, p)
}

// newPGTerminalServer wires a Server over real Postgres with the flaky
// wrapper and seeds a real run + stage (the audit row's foreign keys).
func newPGTerminalServer(t *testing.T) (*Server, *pgFlakyAudit, audit.Repository, uuid.UUID, uuid.UUID) {
	t.Helper()
	pool := pgtest.NewPool(t)
	runRepo := run.NewPostgresRepository(pool)
	real := audit.NewPostgresRepository(pool)
	w := &pgFlakyAudit{Repository: real}
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: runRepo, AuditRepo: w})
	runID, stageID := seedPGReviewRun(t, runRepo)
	return s, w, real, runID, stageID
}

// pgTerminalCfg is a short phase-1 schedule with a settle that cannot run
// before the assertions, and an op timeout generous enough for a real
// Postgres round trip under -race.
func pgTerminalCfg(backoffSteps int) reviewTerminalWriteConfig {
	return rtCfg(rtSteps(backoffSteps, time.Millisecond), rtSteps(1, timescale.D(30*time.Second)), timescale.D(10*time.Second))
}

// pgVerdictWriteFailedRows counts the run's stored rows of category whose
// reason carries the verdict_write_failed prefix.
func pgVerdictWriteFailedRows(t *testing.T, repo audit.Repository, runID uuid.UUID, category string) int {
	t.Helper()
	rows, err := repo.ListForRunByCategory(context.Background(), runID, category)
	if err != nil {
		t.Fatalf("ListForRunByCategory %s: %v", category, err)
	}
	n := 0
	for _, e := range rows {
		var p planreview.ReviewFailedPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("decode %s: %v", category, err)
		}
		if strings.HasPrefix(p.Reason, reviewVerdictWriteFailedPrefix) {
			n++
		}
	}
	return n
}

// TestAppendReviewTerminal_PG_AmbiguousCommitProbeMatchesStoredTimestamp: the
// first append COMMITS the row and then errors. The retry probe must find the
// stored row by Timestamp.Equal against the real timestamptz value, so there is
// exactly one row and the returned entry is that row.
//
// COUNTERFACTUAL C1: make the retry probe always report not-found → the retry
// appends a second row → RED.
func TestAppendReviewTerminal_PG_AmbiguousCommitProbeMatchesStoredTimestamp(t *testing.T) {
	s, w, real, runID, stageID := newPGTerminalServer(t)
	w.action = func(n int, _ audit.ChainAppendParams) flakyAction {
		if n == 1 {
			return flakyCommitThenFail
		}
		return flakyOK
	}
	e, err := s.appendReviewTerminalWith(context.Background(), planReviewedParams(runID, stageID), pgTerminalCfg(3))
	if err != nil || e == nil {
		t.Fatalf("append = (%v, %v), want the committed entry", e, err)
	}
	rows, err := real.ListForRunByCategory(context.Background(), runID, "plan_reviewed")
	if err != nil {
		t.Fatalf("ListForRunByCategory: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("stored plan_reviewed rows = %d, want exactly 1 (the lost-ack commit must not be appended twice)", len(rows))
	}
	if e.Sequence != rows[0].Sequence || !e.Timestamp.Equal(rows[0].Timestamp) {
		t.Errorf("returned entry (seq %d, ts %s) is not the stored row (seq %d, ts %s)", e.Sequence, e.Timestamp, rows[0].Sequence, rows[0].Timestamp)
	}
	if n := registered(runID, "plan"); n != 0 {
		t.Errorf("registered = %d, want 0", n)
	}
}

// TestAppendReviewTerminal_PG_RetryLandsOnce: the first two appends fail
// without writing; the retry lands exactly one real row.
func TestAppendReviewTerminal_PG_RetryLandsOnce(t *testing.T) {
	s, w, real, runID, stageID := newPGTerminalServer(t)
	w.action = func(n int, _ audit.ChainAppendParams) flakyAction {
		if n <= 2 {
			return flakyFail
		}
		return flakyOK
	}
	e, err := s.appendReviewTerminalWith(context.Background(), planReviewedParams(runID, stageID), pgTerminalCfg(5))
	if err != nil || e == nil {
		t.Fatalf("append = (%v, %v), want an entry", e, err)
	}
	rows, err := real.ListForRunByCategory(context.Background(), runID, "plan_reviewed")
	if err != nil {
		t.Fatalf("ListForRunByCategory: %v", err)
	}
	if len(rows) != 1 || rows[0].Sequence != e.Sequence {
		t.Fatalf("stored plan_reviewed rows = %d (want 1) matching returned seq %d", len(rows), e.Sequence)
	}
}

// TestAppendReviewTerminal_PG_FinalProbeFindsCommittedOriginal: every phase-1
// attempt fails without writing except the LAST, which commits the row and
// then errors. No retry probe follows the last attempt, so only the terminal
// path's final probe can find it: one row, zero verdict_write_failed rows.
//
// COUNTERFACTUAL C14: make the final probe report not-found → the named
// failure lands beside the committed verdict → RED.
func TestAppendReviewTerminal_PG_FinalProbeFindsCommittedOriginal(t *testing.T) {
	s, w, real, runID, stageID := newPGTerminalServer(t)
	const backoffSteps = 2 // 1 initial + 2 retries
	w.action = func(n int, p audit.ChainAppendParams) flakyAction {
		if p.Category != "plan_reviewed" {
			return flakyOK
		}
		if n == backoffSteps+1 {
			return flakyCommitThenFail
		}
		return flakyFail
	}
	e, err := s.appendReviewTerminalWith(context.Background(), planReviewedParams(runID, stageID), pgTerminalCfg(backoffSteps))
	if err != nil || e == nil {
		t.Fatalf("append = (%v, %v), want the committed original found by the final probe", e, err)
	}
	rows, err := real.ListForRunByCategory(context.Background(), runID, "plan_reviewed")
	if err != nil {
		t.Fatalf("ListForRunByCategory: %v", err)
	}
	if len(rows) != 1 || rows[0].Sequence != e.Sequence {
		t.Fatalf("stored plan_reviewed rows = %d (want 1) matching returned seq %d", len(rows), e.Sequence)
	}
	if n := pgVerdictWriteFailedRows(t, real, runID, "plan_review_failed"); n != 0 {
		t.Fatalf("verdict_write_failed rows = %d, want 0 beside a committed original", n)
	}
	if n := registered(runID, "plan"); n != 0 {
		t.Errorf("registered = %d, want 0", n)
	}
}
