package main

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

// seedAuditEntry appends one bare audit entry of the given category at an
// explicit sequence to the fake's per-run audit feed. The await_audit
// tests pin behavior on sequence positions, so unlike the review seeders
// the sequence is caller-chosen rather than derived from len().
func seedAuditEntry(fb *fakeBackend, runID uuid.UUID, category string, seq int64) {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	fb.perRunAuditByRun[runID] = append(fb.perRunAuditByRun[runID], AuditEntry{
		ID:       uuid.New().String(),
		Sequence: seq,
		RunID:    runID.String(),
		Category: category,
	})
}

func TestAwaitAudit_RejectsBadInput(t *testing.T) {
	_, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	if _, _, err := r.awaitAudit(context.Background(), nil, AwaitAuditInput{RunID: "nope", Category: "fixup_pushed"}); err == nil {
		t.Error("expected error on bad run_id")
	}
	if _, _, err := r.awaitAudit(context.Background(), nil, AwaitAuditInput{RunID: uuid.NewString(), Category: "  "}); err == nil {
		t.Error("expected error on empty category")
	}
	if _, _, err := r.awaitAudit(context.Background(), nil, AwaitAuditInput{RunID: uuid.NewString(), Category: "fixup_pushed", SinceSequence: -1}); err == nil {
		t.Error("expected error on negative since_sequence")
	}
}

func TestAwaitAudit_ImmediateHit(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	seedAuditEntry(fb, runID, "fixup_pushed", 4)
	r := newResolver(srv, nil)
	// Leave reviewPollInterval at the production default: the fast path
	// must return before any poll tick could fire.

	_, out, err := r.awaitAudit(context.Background(), nil, AwaitAuditInput{
		RunID:         runID.String(),
		Category:      "fixup_pushed",
		SinceSequence: 2,
	})
	if err != nil {
		t.Fatalf("awaitAudit: %v", err)
	}
	if out.Status != "found" {
		t.Fatalf("Status = %q, want found", out.Status)
	}
	if out.Entry == nil || out.Entry.Sequence != 4 {
		t.Errorf("Entry = %+v, want the seeded sequence-4 entry", out.Entry)
	}
	if out.LatestSequence != 4 {
		t.Errorf("LatestSequence = %d, want 4 (the matched entry's sequence)", out.LatestSequence)
	}
}

func TestAwaitAudit_PollsThenLands(t *testing.T) {
	// Nothing seeded at call time; the entry is appended on the first poll
	// tick's audit query. The injected sub-millisecond interval keeps the
	// loop fast and sleep-free.
	fb, srv := newFakeBackend(t)
	runID := uuid.New()

	var queries atomic.Int64
	fb.reviewFlip = func(category string) {
		// reviewFlip runs under fb.mu (the audit handler holds it), so
		// mutate perRunAuditByRun directly rather than via seedAuditEntry.
		if category == "implement_reviewed" && queries.Add(1) == 2 {
			fb.perRunAuditByRun[runID] = append(fb.perRunAuditByRun[runID], AuditEntry{
				ID:       uuid.New().String(),
				Sequence: 3,
				RunID:    runID.String(),
				Category: "implement_reviewed",
			})
		}
	}

	r := newResolver(srv, nil)
	r.reviewPollInterval = 100 * time.Microsecond

	_, out, err := r.awaitAudit(context.Background(), nil, AwaitAuditInput{
		RunID:          runID.String(),
		Category:       "implement_reviewed",
		TimeoutSeconds: 5,
	})
	if err != nil {
		t.Fatalf("awaitAudit: %v", err)
	}
	if out.Status != "found" {
		t.Fatalf("Status = %q, want found after poll-land", out.Status)
	}
	if out.Entry == nil || out.Entry.Sequence != 3 {
		t.Errorf("Entry = %+v, want the mid-wait sequence-3 entry", out.Entry)
	}
	if out.LatestSequence != 3 {
		t.Errorf("LatestSequence = %d, want 3", out.LatestSequence)
	}
}

func TestAwaitAudit_TimeoutIsGaplessReArm(t *testing.T) {
	// Nothing ever lands. Drive the deadline deterministically (#729
	// convention): cancel the parent context from the audit hook only
	// AFTER the fast path has completed and the poll loop has begun —
	// awaitAudit issues exactly one category query per pass (fast path =
	// query 1, first tick = query 2).
	fb, srv := newFakeBackend(t)
	runID := uuid.New()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var queries atomic.Int64
	fb.reviewFlip = func(category string) {
		if category == "implement_reviewed" && queries.Add(1) == 2 {
			cancel()
		}
	}

	r := newResolver(srv, nil)
	r.reviewPollInterval = 100 * time.Microsecond

	_, out, err := r.awaitAudit(ctx, nil, AwaitAuditInput{
		RunID:          runID.String(),
		Category:       "implement_reviewed",
		SinceSequence:  7,
		TimeoutSeconds: 600,
	})
	if err != nil {
		t.Fatalf("awaitAudit: %v", err)
	}
	if out.Status != "timeout" {
		t.Fatalf("Status = %q, want timeout", out.Status)
	}
	// The gapless re-arm contract: nothing past the anchor was observed,
	// so re-calling with since_sequence = LatestSequence cannot skip an
	// entry — it is exactly the original anchor.
	if out.LatestSequence != 7 {
		t.Errorf("LatestSequence = %d, want 7 (== since_sequence when nothing landed)", out.LatestSequence)
	}
	if out.PollIntervalSeconds != suggestedReviewPollIntervalSeconds {
		t.Errorf("PollIntervalSeconds = %d, want %d", out.PollIntervalSeconds, suggestedReviewPollIntervalSeconds)
	}
	if !strings.Contains(out.Message, "since_sequence=7") {
		t.Errorf("timeout message should name the re-arm anchor: %q", out.Message)
	}
}

func TestAwaitAudit_RunTerminalBackstop(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	fb.getRunByID[runID] = Run{ID: runID.String(), State: "failed"}
	r := newResolver(srv, nil)
	r.reviewPollInterval = 100 * time.Microsecond

	// A large timeout: if the backstop did NOT fire the test would hang on
	// the deadline rather than returning, so a prompt return is the proof.
	_, out, err := r.awaitAudit(context.Background(), nil, AwaitAuditInput{
		RunID:          runID.String(),
		Category:       "implement_reviewed",
		SinceSequence:  2,
		TimeoutSeconds: 600,
	})
	if err != nil {
		t.Fatalf("awaitAudit: %v", err)
	}
	if out.Status != "run_terminal" {
		t.Fatalf("Status = %q, want run_terminal", out.Status)
	}
	if out.LatestSequence != 2 {
		t.Errorf("LatestSequence = %d, want 2 (the anchor; nothing landed)", out.LatestSequence)
	}
	if !strings.Contains(out.Message, "Do not re-arm blindly") {
		t.Errorf("run_terminal message should warn against blind re-arm: %q", out.Message)
	}
}

// TestAwaitAudit_RunTerminalBackstop_FinalReadWins pins the backstop's
// final-read ordering: an entry that lands at/after the run's terminal
// transition must resolve as found, not be suppressed by run_terminal.
// The run is terminal from the start, and the entry is appended on the
// SECOND category query — the backstop's final read (the fast path was
// query 1 and found nothing).
func TestAwaitAudit_RunTerminalBackstop_FinalReadWins(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	fb.getRunByID[runID] = Run{ID: runID.String(), State: "succeeded"}

	var queries atomic.Int64
	fb.reviewFlip = func(category string) {
		if category == "run_completed" && queries.Add(1) == 2 {
			fb.perRunAuditByRun[runID] = append(fb.perRunAuditByRun[runID], AuditEntry{
				ID:       uuid.New().String(),
				Sequence: 9,
				RunID:    runID.String(),
				Category: "run_completed",
			})
		}
	}

	r := newResolver(srv, nil)
	r.reviewPollInterval = 100 * time.Microsecond

	_, out, err := r.awaitAudit(context.Background(), nil, AwaitAuditInput{
		RunID:          runID.String(),
		Category:       "run_completed",
		TimeoutSeconds: 600,
	})
	if err != nil {
		t.Fatalf("awaitAudit: %v", err)
	}
	if out.Status != "found" {
		t.Fatalf("Status = %q, want found (the backstop's final read must win)", out.Status)
	}
	if out.Entry == nil || out.Entry.Sequence != 9 {
		t.Errorf("Entry = %+v, want the sequence-9 entry from the final read", out.Entry)
	}
}

// TestAwaitAudit_StaleReviewNeverReturned is the #894 reproduction at the
// construction level: a fix-up re-opened the implement stage, so the audit
// trail holds a stale pre-fix-up implement_reviewed verdict BELOW the
// fixup_pushed sequence and a fresh one above it. Anchoring the await at
// the fix-up entry's sequence makes returning the stale verdict
// impossible by construction — only sequence > anchor satisfies the wait.
func TestAwaitAudit_StaleReviewNeverReturned(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	seedAuditEntry(fb, runID, "implement_reviewed", 2) // stale pre-fix-up verdict
	seedAuditEntry(fb, runID, "fixup_pushed", 5)       // the fix-up boundary
	seedAuditEntry(fb, runID, "implement_reviewed", 7) // post-fix-up verdict
	r := newResolver(srv, nil)

	_, out, err := r.awaitAudit(context.Background(), nil, AwaitAuditInput{
		RunID:         runID.String(),
		Category:      "implement_reviewed",
		SinceSequence: 5,
	})
	if err != nil {
		t.Fatalf("awaitAudit: %v", err)
	}
	if out.Status != "found" {
		t.Fatalf("Status = %q, want found", out.Status)
	}
	if out.Entry == nil || out.Entry.Sequence != 7 {
		t.Fatalf("Entry = %+v, want the post-fix-up sequence-7 verdict, never the stale sequence-2 one", out.Entry)
	}
	if out.LatestSequence != 7 {
		t.Errorf("LatestSequence = %d, want 7", out.LatestSequence)
	}
}
