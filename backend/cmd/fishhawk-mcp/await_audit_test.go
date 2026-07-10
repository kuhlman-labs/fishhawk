package main

import (
	"context"
	"encoding/json"
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

// TestAwaitAudit_UnknownCategoryRejected is the #1764 fail-loud proof: an
// unknown/misspelled category is rejected UP FRONT with the nearest known
// category named, and NO wait is armed (the endpoint is never queried for it),
// so it cannot silently block the full timeout on an unsatisfiable wait.
func TestAwaitAudit_UnknownCategoryRejected(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	r := newResolver(srv, nil)

	_, _, err := r.awaitAudit(context.Background(), nil, AwaitAuditInput{
		RunID:    runID.String(),
		Category: "scope_amendment_pending", // the runner-log event, NOT the audit category
	})
	if err == nil {
		t.Fatal("expected a rejection on an unknown category")
	}
	if !strings.Contains(err.Error(), "scope_amendment_requested") {
		t.Errorf("rejection must name the nearest known category; got %q", err.Error())
	}
	// No wait armed: the endpoint was never queried for the unknown category.
	fb.mu.Lock()
	reads := fb.perRunAuditCategoryReads["scope_amendment_pending"]
	fb.mu.Unlock()
	if reads != 0 {
		t.Errorf("unknown category armed a wait (%d audit reads); it must be rejected before any query", reads)
	}
}

// TestAwaitAudit_AllowUnknownAdmitsUnknown proves the allow_unknown escape
// hatch admits an unlisted category: the wait is armed and resolves on a
// seeded entry of that category.
func TestAwaitAudit_AllowUnknownAdmitsUnknown(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	seedAuditEntry(fb, runID, "some_brand_new_category", 4)
	r := newResolver(srv, nil)

	_, out, err := r.awaitAudit(context.Background(), nil, AwaitAuditInput{
		RunID:         runID.String(),
		Category:      "some_brand_new_category",
		SinceSequence: 2,
		AllowUnknown:  true,
	})
	if err != nil {
		t.Fatalf("awaitAudit with allow_unknown: %v", err)
	}
	if out.Status != "found" || out.Entry == nil || out.Entry.Sequence != 4 {
		t.Fatalf("Status=%q Entry=%+v, want found on the seeded seq-4 entry", out.Status, out.Entry)
	}
}

// TestAwaitAudit_EmptyBothRejected proves the both-blank input (no category,
// no categories) is rejected — the unsatisfiable degenerate wait.
func TestAwaitAudit_EmptyBothRejected(t *testing.T) {
	_, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	if _, _, err := r.awaitAudit(context.Background(), nil, AwaitAuditInput{
		RunID:      uuid.NewString(),
		Category:   "  ",
		Categories: []string{"", "   "},
	}); err == nil {
		t.Error("expected an error when neither category nor categories provides a value")
	}
}

// TestAwaitAudit_MultiCategoryResolvesOnFirstLanding is the OR-semantics proof
// (#1764): with two categories both carrying a past-anchor entry, the wait
// resolves on the LOWER-sequence entry — the first to land.
func TestAwaitAudit_MultiCategoryResolvesOnFirstLanding(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	seedAuditEntry(fb, runID, "implement_reviewed", 8)
	seedAuditEntry(fb, runID, "fixup_pushed", 5) // lower sequence — the OR-winner
	r := newResolver(srv, nil)

	_, out, err := r.awaitAudit(context.Background(), nil, AwaitAuditInput{
		RunID:         runID.String(),
		Categories:    []string{"implement_reviewed", "fixup_pushed"},
		SinceSequence: 2,
	})
	if err != nil {
		t.Fatalf("awaitAudit multi-category: %v", err)
	}
	if out.Status != "found" {
		t.Fatalf("Status = %q, want found", out.Status)
	}
	if out.Entry == nil || out.Entry.Sequence != 5 || out.Entry.Category != "fixup_pushed" {
		t.Errorf("Entry = %+v, want the lower-sequence fixup_pushed (seq 5) OR-winner", out.Entry)
	}
	if out.LatestSequence != 5 {
		t.Errorf("LatestSequence = %d, want 5", out.LatestSequence)
	}
}

// TestAwaitAudit_MultiCategoryTimeoutReArmIsSharedMax is the #1764 binding
// condition (2): on a multi-category timeout, the returned latest_sequence is
// the single shared anchor — the MAX gapless re-arm across ALL requested
// categories. A per-category-anchor divergence bug (re-arming each category
// from its own last-seen and returning a per-category value, e.g. 0 for an
// unpolled category) would return something other than the shared anchor and
// fail this. Entries seeded AT/BELOW the anchor never resolve, forcing the
// timeout.
func TestAwaitAudit_MultiCategoryTimeoutReArmIsSharedMax(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	// Both categories carry only pre-anchor entries (seq <= 7), so nothing
	// past the shared anchor resolves and the wait times out.
	seedAuditEntry(fb, runID, "implement_reviewed", 3)
	seedAuditEntry(fb, runID, "fixup_pushed", 6)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Drive the deadline deterministically: cancel once the poll loop has
	// begun. awaitAudit issues one query PER category per pass; with two
	// categories the fast path is queries 1+2 and the first tick is 3+4.
	var queries atomic.Int64
	fb.reviewFlip = func(category string) {
		if queries.Add(1) == 3 {
			cancel()
		}
	}

	r := newResolver(srv, nil)
	r.reviewPollInterval = 100 * time.Microsecond

	_, out, err := r.awaitAudit(ctx, nil, AwaitAuditInput{
		RunID:          runID.String(),
		Categories:     []string{"implement_reviewed", "fixup_pushed"},
		SinceSequence:  7,
		TimeoutSeconds: 600,
	})
	if err != nil {
		t.Fatalf("awaitAudit: %v", err)
	}
	if out.Status != "timeout" {
		t.Fatalf("Status = %q, want timeout", out.Status)
	}
	// The shared anchor IS the max gapless re-arm across every category.
	if out.LatestSequence != 7 {
		t.Errorf("LatestSequence = %d, want 7 (the shared anchor == max re-arm across ALL categories)", out.LatestSequence)
	}
	if !strings.Contains(out.Message, "since_sequence=7") {
		t.Errorf("timeout message should name the re-arm anchor: %q", out.Message)
	}
	// The multi-category message names both categories.
	for _, c := range []string{"implement_reviewed", "fixup_pushed"} {
		if !strings.Contains(out.Message, c) {
			t.Errorf("timeout message should name category %q: %q", c, out.Message)
		}
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

// seedAuditEntryWithPayload appends one audit entry carrying a decoded-JSON
// payload, for the #1727 compact-projection await_audit test.
func seedAuditEntryWithPayload(fb *fakeBackend, runID uuid.UUID, category string, seq int64, payload map[string]any) {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	raw, _ := json.Marshal(payload)
	var decoded any
	_ = json.Unmarshal(raw, &decoded)
	fb.perRunAuditByRun[runID] = append(fb.perRunAuditByRun[runID], AuditEntry{
		ID:       uuid.New().String(),
		Sequence: seq,
		RunID:    runID.String(),
		Category: category,
		Payload:  decoded,
	})
}

// TestAwaitAudit_CompactProjection is the #1727 await_audit projection proof:
// a found entry whose payload carries free_form + body + a verdict returns,
// by default, a payload with free_form/body stripped and the verdict kept;
// the include flags restore the full payload.
func TestAwaitAudit_CompactProjection(t *testing.T) {
	payload := func() map[string]any {
		return map[string]any{
			"verdict":   "approve_with_concerns",
			"free_form": "reviewer prose",
			"body":      "issue body",
		}
	}

	t.Run("default strips free_form and body, keeps verdict", func(t *testing.T) {
		fb, srv := newFakeBackend(t)
		runID := uuid.New()
		seedAuditEntryWithPayload(fb, runID, "implement_reviewed", 4, payload())
		r := newResolver(srv, nil)

		_, out, err := r.awaitAudit(context.Background(), nil, AwaitAuditInput{
			RunID: runID.String(), Category: "implement_reviewed", SinceSequence: 2,
		})
		if err != nil {
			t.Fatalf("awaitAudit: %v", err)
		}
		if out.Status != "found" || out.Entry == nil {
			t.Fatalf("Status = %q, Entry = %+v, want found with entry", out.Status, out.Entry)
		}
		m, ok := out.Entry.Payload.(map[string]any)
		if !ok {
			t.Fatalf("payload is not a map: %T", out.Entry.Payload)
		}
		if _, present := m["free_form"]; present {
			t.Errorf("free_form should be stripped by default, got %+v", m)
		}
		if _, present := m["body"]; present {
			t.Errorf("body should be stripped by default, got %+v", m)
		}
		if m["verdict"] != "approve_with_concerns" {
			t.Errorf("verdict must survive, got %+v", m)
		}
	})

	t.Run("include flags restore the full payload", func(t *testing.T) {
		fb, srv := newFakeBackend(t)
		runID := uuid.New()
		seedAuditEntryWithPayload(fb, runID, "implement_reviewed", 4, payload())
		r := newResolver(srv, nil)

		_, out, err := r.awaitAudit(context.Background(), nil, AwaitAuditInput{
			RunID: runID.String(), Category: "implement_reviewed", SinceSequence: 2,
			IncludeReviewProse: true, IncludeIssueContext: true,
		})
		if err != nil {
			t.Fatalf("awaitAudit: %v", err)
		}
		m := out.Entry.Payload.(map[string]any)
		if m["free_form"] != "reviewer prose" || m["body"] != "issue body" {
			t.Errorf("full payload should be restored under include flags, got %+v", m)
		}
	})
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
