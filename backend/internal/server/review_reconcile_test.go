package server

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// bootMarker is the fixed processStart the reconcile tests inject so the
// "started before boot" orphan predicate is deterministic.
var bootMarker = time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)

// beforeBoot / afterBoot are audit timestamps on either side of bootMarker.
var (
	beforeBoot = bootMarker.Add(-time.Hour)
	afterBoot  = bootMarker.Add(time.Hour)
)

// newReconcileServer wires a Server with the run + audit fakes and the
// injected boot marker. The recompute-audit-complete seam records the runs it
// fired on so the implement test can assert the republish.
func newReconcileServer(t *testing.T) (*Server, *fakeRepo, *auditFake, *[]uuid.UUID) {
	t.Helper()
	repo := newFakeRepo()
	au := newAuditFake()
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: repo, AuditRepo: au, ProcessStart: bootMarker})
	var recomputed []uuid.UUID
	s.reconcileRecomputeAuditComplete = func(_ context.Context, runID uuid.UUID) {
		recomputed = append(recomputed, runID)
	}
	return s, repo, au, &recomputed
}

// seedRunningRun inserts a running run into the fake repo and returns its id.
func seedRunningRun(t *testing.T, repo *fakeRepo) uuid.UUID {
	t.Helper()
	id := uuid.New()
	repo.runs[id] = &run.Run{
		ID:        id,
		State:     run.StateRunning,
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	return id
}

// seedReviewAuditEntry appends a review audit entry with an explicit
// sequence, timestamp, and stage id — the fields the attempt-correlation and
// boot-marker logic read (seedReviewEntry in runs_get_test.go sets neither
// StageID nor a controllable timestamp).
func seedReviewAuditEntry(t *testing.T, au *auditFake, runID, stageID uuid.UUID, seq int64, ts time.Time, category string, payload any) {
	t.Helper()
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal %s payload: %v", category, err)
	}
	rid := runID
	sid := stageID
	au.seeded = append(au.seeded, &audit.Entry{
		RunID:     &rid,
		StageID:   &sid,
		Sequence:  seq,
		Timestamp: ts,
		Category:  category,
		Payload:   b,
	})
}

// emittedReviewFailures returns the *_review_failed entries the reconcile
// appended for the given category, decoded to their payloads.
func emittedReviewFailures(t *testing.T, au *auditFake, category string) []planreview.ReviewFailedPayload {
	t.Helper()
	au.mu.Lock()
	defer au.mu.Unlock()
	var out []planreview.ReviewFailedPayload
	for _, p := range au.appended {
		if p.Category != category {
			continue
		}
		var payload planreview.ReviewFailedPayload
		if err := json.Unmarshal(p.Payload, &payload); err != nil {
			t.Fatalf("decode emitted %s payload: %v", category, err)
		}
		out = append(out, payload)
	}
	return out
}

// TestReconcileOrphanedReviews_New_StampsProcessStart is the binding
// condition (3) construction assertion: New() stamps a non-zero processStart
// boot marker (kept in review_reconcile_test.go, not server_test.go).
func TestReconcileOrphanedReviews_New_StampsProcessStart(t *testing.T) {
	// Default (no override): a real boot marker is stamped.
	s := New(Config{Addr: "127.0.0.1:0"})
	if s.processStart.IsZero() {
		t.Fatal("New did not stamp a processStart boot marker")
	}
	// Explicit override is honored verbatim.
	s2 := New(Config{Addr: "127.0.0.1:0", ProcessStart: bootMarker})
	if !s2.processStart.Equal(bootMarker) {
		t.Fatalf("processStart = %v, want injected %v", s2.processStart, bootMarker)
	}
}

// TestReconcileOrphanedReviews_OrphanedPlan covers a plan review dispatched by
// a prior process (started before the boot marker, no terminal entry): exactly
// one plan_review_failed is synthesized and landed reaches ConfiguredAgents.
func TestReconcileOrphanedReviews_OrphanedPlan(t *testing.T) {
	s, repo, au, _ := newReconcileServer(t)
	runID := seedRunningRun(t, repo)
	stageID := uuid.New()
	seedReviewAuditEntry(t, au, runID, stageID, 1, beforeBoot, "plan_review_started",
		planreview.ReviewStartedPayload{ConfiguredAgents: 1, Authority: planreview.AuthorityAdvisory})

	n, err := s.ReconcileOrphanedReviews(context.Background())
	if err != nil {
		t.Fatalf("ReconcileOrphanedReviews: %v", err)
	}
	if n != 1 {
		t.Fatalf("terminated runs = %d, want 1", n)
	}
	failures := emittedReviewFailures(t, au, "plan_review_failed")
	if len(failures) != 1 {
		t.Fatalf("plan_review_failed emitted = %d, want 1", len(failures))
	}
	if failures[0].Timeout {
		t.Error("synthesized failure Timeout = true, want false")
	}
	if failures[0].Reason == "" {
		t.Error("synthesized failure carries an empty reason")
	}
	// landed now equals ConfiguredAgents, so the mcp reviewStatusFor pending
	// predicate (landed < configured) no longer holds.
	if !planreview.Settled(1, len(failures)) {
		t.Error("round did not settle after synthesizing the missing failure")
	}
}

// TestReconcileOrphanedReviews_OrphanedImplement is the implement analogue and
// additionally asserts the audit-complete republish fired for the run.
func TestReconcileOrphanedReviews_OrphanedImplement(t *testing.T) {
	s, repo, au, recomputed := newReconcileServer(t)
	runID := seedRunningRun(t, repo)
	stageID := uuid.New()
	seedReviewAuditEntry(t, au, runID, stageID, 1, beforeBoot, "implement_review_started",
		planreview.ReviewStartedPayload{ConfiguredAgents: 1, Authority: planreview.AuthorityAdvisory})

	if _, err := s.ReconcileOrphanedReviews(context.Background()); err != nil {
		t.Fatalf("ReconcileOrphanedReviews: %v", err)
	}
	failures := emittedReviewFailures(t, au, "implement_review_failed")
	if len(failures) != 1 {
		t.Fatalf("implement_review_failed emitted = %d, want 1", len(failures))
	}
	if len(*recomputed) != 1 || (*recomputed)[0] != runID {
		t.Fatalf("audit-complete republish = %v, want [%s]", *recomputed, runID)
	}
}

// TestReconcileOrphanedReviews_CurrentProcessPreserved covers a review whose
// latest started entry is AFTER the boot marker (dispatched by THIS process):
// it is still legitimately in flight and must NOT be failed.
func TestReconcileOrphanedReviews_CurrentProcessPreserved(t *testing.T) {
	s, repo, au, _ := newReconcileServer(t)
	runID := seedRunningRun(t, repo)
	stageID := uuid.New()
	seedReviewAuditEntry(t, au, runID, stageID, 1, afterBoot, "plan_review_started",
		planreview.ReviewStartedPayload{ConfiguredAgents: 1, Authority: planreview.AuthorityAdvisory})

	n, err := s.ReconcileOrphanedReviews(context.Background())
	if err != nil {
		t.Fatalf("ReconcileOrphanedReviews: %v", err)
	}
	if n != 0 {
		t.Fatalf("terminated runs = %d, want 0", n)
	}
	if got := emittedReviewFailures(t, au, "plan_review_failed"); len(got) != 0 {
		t.Fatalf("plan_review_failed emitted = %d, want 0 (current-process review)", len(got))
	}
}

// TestReconcileOrphanedReviews_AlreadyTerminalIdempotent covers an already
// settled review (landed == configured): a no-op, and a second pass over the
// mutated state still emits nothing.
func TestReconcileOrphanedReviews_AlreadyTerminalIdempotent(t *testing.T) {
	s, repo, au, _ := newReconcileServer(t)
	runID := seedRunningRun(t, repo)
	stageID := uuid.New()
	seedReviewAuditEntry(t, au, runID, stageID, 1, beforeBoot, "plan_review_started",
		planreview.ReviewStartedPayload{ConfiguredAgents: 1, Authority: planreview.AuthorityAdvisory})
	seedReviewAuditEntry(t, au, runID, stageID, 2, beforeBoot, "plan_reviewed",
		planreview.PlanReviewedPayload{Verdict: planreview.VerdictApprove})

	for pass := 1; pass <= 2; pass++ {
		n, err := s.ReconcileOrphanedReviews(context.Background())
		if err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
		if n != 0 {
			t.Fatalf("pass %d: terminated runs = %d, want 0", pass, n)
		}
	}
	if got := emittedReviewFailures(t, au, "plan_review_failed"); len(got) != 0 {
		t.Fatalf("plan_review_failed emitted = %d, want 0 (already settled)", len(got))
	}
}

// TestReconcileOrphanedReviews_SelfSynthesizedIdempotent closes the low/
// test-coverage gap: TestReconcileOrphanedReviews_AlreadyTerminalIdempotent
// only proves idempotency against a PRE-SEEDED verdict, never against the
// reconcile's OWN synthesized *_review_failed. The in-memory auditFake returns
// appended entries WITHOUT a sequence (Sequence 0), so a re-read of a
// self-synthesized failure would not count as landed (0 is not > the started
// anchor). The real audit store assigns a monotonic sequence to every appended
// entry, which is why production self-idempotency holds; this test bridges the
// fake's gap by promoting the pass-1 synthesized failure into seeded history
// with a real sequence strictly greater than the started anchor (what the store
// does) before a second pass, then asserts the second pass is a true no-op
// against the reconcile's own prior output.
func TestReconcileOrphanedReviews_SelfSynthesizedIdempotent(t *testing.T) {
	s, repo, au, _ := newReconcileServer(t)
	runID := seedRunningRun(t, repo)
	stageID := uuid.New()
	seedReviewAuditEntry(t, au, runID, stageID, 1, beforeBoot, "plan_review_started",
		planreview.ReviewStartedPayload{ConfiguredAgents: 1, Authority: planreview.AuthorityAdvisory})

	// Pass 1: the orphaned round synthesizes exactly one plan_review_failed.
	if _, err := s.ReconcileOrphanedReviews(context.Background()); err != nil {
		t.Fatalf("pass 1: %v", err)
	}
	first := emittedReviewFailures(t, au, "plan_review_failed")
	if len(first) != 1 {
		t.Fatalf("pass 1: plan_review_failed emitted = %d, want 1", len(first))
	}

	// Durably land the synthesized failure the way the real store does: promote
	// pass-1's OWN output into seeded history with a monotonic sequence strictly
	// greater than the started anchor (seq 1), then drop the append buffer so the
	// second pass reads only the persisted state.
	seedReviewAuditEntry(t, au, runID, stageID, 2, beforeBoot, "plan_review_failed", first[0])
	au.mu.Lock()
	au.appended = nil
	au.mu.Unlock()

	// Pass 2: the round is now settled (landed seq 2 > started seq 1 == 1
	// configured) — a no-op against the reconcile's OWN synthesized entry.
	n, err := s.ReconcileOrphanedReviews(context.Background())
	if err != nil {
		t.Fatalf("pass 2: %v", err)
	}
	if n != 0 {
		t.Fatalf("pass 2: terminated runs = %d, want 0 (self-synthesized already landed)", n)
	}
	if got := emittedReviewFailures(t, au, "plan_review_failed"); len(got) != 0 {
		t.Fatalf("pass 2: plan_review_failed emitted = %d, want 0 (idempotent on self-synthesized)", len(got))
	}
}

// TestReconcileOrphanedReviews_Partial covers a round with more than one
// configured agent where one terminal verdict already landed: exactly the
// missing count (1) is synthesized so landed reaches ConfiguredAgents.
func TestReconcileOrphanedReviews_Partial(t *testing.T) {
	s, repo, au, _ := newReconcileServer(t)
	runID := seedRunningRun(t, repo)
	stageID := uuid.New()
	seedReviewAuditEntry(t, au, runID, stageID, 1, beforeBoot, "plan_review_started",
		planreview.ReviewStartedPayload{ConfiguredAgents: 2, Authority: planreview.AuthorityAdvisory})
	seedReviewAuditEntry(t, au, runID, stageID, 2, beforeBoot, "plan_reviewed",
		planreview.PlanReviewedPayload{Verdict: planreview.VerdictApprove})

	if _, err := s.ReconcileOrphanedReviews(context.Background()); err != nil {
		t.Fatalf("ReconcileOrphanedReviews: %v", err)
	}
	failures := emittedReviewFailures(t, au, "plan_review_failed")
	if len(failures) != 1 {
		t.Fatalf("plan_review_failed emitted = %d, want 1", len(failures))
	}
	// 1 pre-landed reviewed + 1 synthesized failed == 2 configured.
	if !planreview.Settled(2, 1+len(failures)) {
		t.Error("round did not settle at 2 terminal entries")
	}
}

// TestReconcileOrphanedReviews_TwoRounds_SecondOrphaned is binding condition
// (1): a stage with two review rounds — the first fully landed, the second
// orphaned by a boot-marker change — synthesizes a failure for the SECOND
// round only. Counting run-wide terminals (the bug) would see the first
// round's landed verdict and wrongly skip.
func TestReconcileOrphanedReviews_TwoRounds_SecondOrphaned(t *testing.T) {
	s, repo, au, _ := newReconcileServer(t)
	runID := seedRunningRun(t, repo)
	stageID := uuid.New()
	// Round 1: started (seq 1) + reviewed (seq 2), fully landed.
	seedReviewAuditEntry(t, au, runID, stageID, 1, beforeBoot, "plan_review_started",
		planreview.ReviewStartedPayload{ConfiguredAgents: 1, Authority: planreview.AuthorityAdvisory})
	seedReviewAuditEntry(t, au, runID, stageID, 2, beforeBoot, "plan_reviewed",
		planreview.PlanReviewedPayload{Verdict: planreview.VerdictApprove})
	// Round 2: started (seq 3), no terminal — orphaned.
	seedReviewAuditEntry(t, au, runID, stageID, 3, beforeBoot, "plan_review_started",
		planreview.ReviewStartedPayload{ConfiguredAgents: 1, Authority: planreview.AuthorityAdvisory})

	if _, err := s.ReconcileOrphanedReviews(context.Background()); err != nil {
		t.Fatalf("ReconcileOrphanedReviews: %v", err)
	}
	failures := emittedReviewFailures(t, au, "plan_review_failed")
	if len(failures) != 1 {
		t.Fatalf("plan_review_failed emitted = %d, want 1 (second round only)", len(failures))
	}
}

// TestReconcileOrphanedReviews_TwoRounds_FirstOrphaned is the inverse of the
// binding condition (1) case: the first round is orphaned but the second
// (latest) round has landed — the CURRENT attempt is settled, so nothing is
// synthesized despite the stale orphaned first round.
func TestReconcileOrphanedReviews_TwoRounds_FirstOrphaned(t *testing.T) {
	s, repo, au, _ := newReconcileServer(t)
	runID := seedRunningRun(t, repo)
	stageID := uuid.New()
	// Round 1: started (seq 1), no terminal — orphaned.
	seedReviewAuditEntry(t, au, runID, stageID, 1, beforeBoot, "plan_review_started",
		planreview.ReviewStartedPayload{ConfiguredAgents: 1, Authority: planreview.AuthorityAdvisory})
	// Round 2: started (seq 3) + reviewed (seq 4), landed.
	seedReviewAuditEntry(t, au, runID, stageID, 3, beforeBoot, "plan_review_started",
		planreview.ReviewStartedPayload{ConfiguredAgents: 1, Authority: planreview.AuthorityAdvisory})
	seedReviewAuditEntry(t, au, runID, stageID, 4, beforeBoot, "plan_reviewed",
		planreview.PlanReviewedPayload{Verdict: planreview.VerdictApprove})

	n, err := s.ReconcileOrphanedReviews(context.Background())
	if err != nil {
		t.Fatalf("ReconcileOrphanedReviews: %v", err)
	}
	if n != 0 {
		t.Fatalf("terminated runs = %d, want 0 (latest round landed)", n)
	}
	if got := emittedReviewFailures(t, au, "plan_review_failed"); len(got) != 0 {
		t.Fatalf("plan_review_failed emitted = %d, want 0 (latest round landed)", len(got))
	}
}

// TestReconcileOrphanedReviews_SynthesizedFailedIsTerminal is binding
// condition (2): the synthesized *_review_failed entry carries placeholder
// model/authority and must be treated as a terminal failed entry. reviewStatusFor
// (fishhawk-mcp) counts any *_review_failed toward the landed terminal set via
// the same landed >= configured predicate this asserts through planreview.Settled;
// the placeholder empty model does not disqualify it.
func TestReconcileOrphanedReviews_SynthesizedFailedIsTerminal(t *testing.T) {
	s, repo, au, _ := newReconcileServer(t)
	runID := seedRunningRun(t, repo)
	stageID := uuid.New()
	seedReviewAuditEntry(t, au, runID, stageID, 1, beforeBoot, "implement_review_started",
		planreview.ReviewStartedPayload{ConfiguredAgents: 1, Authority: planreview.AuthorityAdvisory})

	if _, err := s.ReconcileOrphanedReviews(context.Background()); err != nil {
		t.Fatalf("ReconcileOrphanedReviews: %v", err)
	}
	failures := emittedReviewFailures(t, au, "implement_review_failed")
	if len(failures) != 1 {
		t.Fatalf("implement_review_failed emitted = %d, want 1", len(failures))
	}
	f := failures[0]
	// Placeholder fidelity (documented limitation): empty model, run authority,
	// non-timeout. All three are what reviewStatusFor's decodeFailedReviews reads.
	if f.ReviewerModel != "" {
		t.Errorf("ReviewerModel = %q, want empty placeholder", f.ReviewerModel)
	}
	if f.Authority != planreview.AuthorityAdvisory {
		t.Errorf("Authority = %q, want %q", f.Authority, planreview.AuthorityAdvisory)
	}
	if f.Timeout {
		t.Error("Timeout = true, want false")
	}

	// Exercise the mcp consumer-side terminal classification, not just the count.
	// reviewStatusFor's decodeFailedReviews (fishhawk-mcp review.go) unmarshals a
	// *_review_failed payload through a struct keyed on reason/reviewer_model/
	// authority and UNCONDITIONALLY yields a terminal verdict "failed" row — the
	// placeholder empty reviewer_model must NOT drop the entry. Decode the ACTUAL
	// emitted bytes through that exact json contract so this fails if the
	// placeholder fields ever stopped decoding (the concern's failure scenario),
	// rather than only counting len(failures).
	//
	// The unexported reviewStatusFor lives in package main (backend/cmd/
	// fishhawk-mcp) and is unreachable from this package, so this asserts the
	// same decode contract it applies rather than calling it directly. The
	// authoritative end-to-end reviewStatusFor assertion (started+placeholder
	// failed pair resolving to status "failed") belongs in
	// backend/cmd/fishhawk-mcp/review_test.go alongside the existing
	// TestReviewStatusFor_Failed_WinsOverStarted; it is deferred here because
	// that file is outside this change's scope.
	raw := appendedPayload(t, au, "implement_review_failed")
	var consumer struct {
		Reason        string `json:"reason"`
		ReviewerModel string `json:"reviewer_model"`
		Authority     string `json:"authority"`
	}
	if err := json.Unmarshal(raw, &consumer); err != nil {
		t.Fatalf("consumer decode rejected the synthesized placeholder payload: %v", err)
	}
	if consumer.ReviewerModel != "" {
		t.Errorf("consumer reviewer_model = %q, want empty placeholder", consumer.ReviewerModel)
	}
	if consumer.Reason != orphanedReviewRestartReason {
		t.Errorf("consumer reason = %q, want %q", consumer.Reason, orphanedReviewRestartReason)
	}
	// A cleanly-decoded placeholder is a terminal "failed" row for the consumer,
	// so a 1-of-1 round settles — reviewStatusFor flips pending -> failed.
	if !planreview.Settled(1, len(failures)) {
		t.Error("synthesized failed entry did not settle the round")
	}
}

// appendedPayload returns the raw JSON payload of the single audit entry the
// reconcile appended for the given category, so a test can decode it through
// the mcp consumer's exact json contract rather than the producer type.
func appendedPayload(t *testing.T, au *auditFake, category string) []byte {
	t.Helper()
	au.mu.Lock()
	defer au.mu.Unlock()
	var out []byte
	n := 0
	for _, p := range au.appended {
		if p.Category != category {
			continue
		}
		out = p.Payload
		n++
	}
	if n != 1 {
		t.Fatalf("appended %s payloads = %d, want 1", category, n)
	}
	return out
}

// TestReconcileOrphanedReviews_NoStartedEntry covers the guard for a stage
// with no *_review_started (no review was ever dispatched): a no-op.
func TestReconcileOrphanedReviews_NoStartedEntry(t *testing.T) {
	s, repo, au, _ := newReconcileServer(t)
	seedRunningRun(t, repo)

	n, err := s.ReconcileOrphanedReviews(context.Background())
	if err != nil {
		t.Fatalf("ReconcileOrphanedReviews: %v", err)
	}
	if n != 0 {
		t.Fatalf("terminated runs = %d, want 0", n)
	}
	if got := emittedReviewFailures(t, au, "plan_review_failed"); len(got) != 0 {
		t.Fatalf("plan_review_failed emitted = %d, want 0", len(got))
	}
}

// TestReconcileOrphanedReviews_ZeroConfiguredAgents covers the guard for a
// started entry with ConfiguredAgents <= 0 (no reviewer actually configured):
// never pending, so a no-op.
func TestReconcileOrphanedReviews_ZeroConfiguredAgents(t *testing.T) {
	s, repo, au, _ := newReconcileServer(t)
	runID := seedRunningRun(t, repo)
	stageID := uuid.New()
	seedReviewAuditEntry(t, au, runID, stageID, 1, beforeBoot, "plan_review_started",
		planreview.ReviewStartedPayload{ConfiguredAgents: 0, Authority: planreview.AuthorityAdvisory})

	if _, err := s.ReconcileOrphanedReviews(context.Background()); err != nil {
		t.Fatalf("ReconcileOrphanedReviews: %v", err)
	}
	if got := emittedReviewFailures(t, au, "plan_review_failed"); len(got) != 0 {
		t.Fatalf("plan_review_failed emitted = %d, want 0 (no reviewer configured)", len(got))
	}
}

// TestReconcileOrphanedReviews_NilStageID covers the defensive guard for a
// started entry that (impossibly) carries no stage id: skipped rather than
// emitting a terminal entry with no stage anchor.
func TestReconcileOrphanedReviews_NilStageID(t *testing.T) {
	s, repo, au, _ := newReconcileServer(t)
	runID := seedRunningRun(t, repo)
	b, _ := json.Marshal(planreview.ReviewStartedPayload{ConfiguredAgents: 1, Authority: planreview.AuthorityAdvisory})
	rid := runID
	au.seeded = append(au.seeded, &audit.Entry{
		RunID:     &rid,
		StageID:   nil, // no stage anchor
		Sequence:  1,
		Timestamp: beforeBoot,
		Category:  "plan_review_started",
		Payload:   b,
	})

	if _, err := s.ReconcileOrphanedReviews(context.Background()); err != nil {
		t.Fatalf("ReconcileOrphanedReviews: %v", err)
	}
	if got := emittedReviewFailures(t, au, "plan_review_failed"); len(got) != 0 {
		t.Fatalf("plan_review_failed emitted = %d, want 0 (nil stage id)", len(got))
	}
}

// TestReconcileOrphanedReviews_PerRunAuditErrorIsolated covers the best-effort
// per-run isolation: an audit read error on one run is logged and skipped, not
// returned, so the sweep completes without aborting.
func TestReconcileOrphanedReviews_PerRunAuditErrorIsolated(t *testing.T) {
	s, repo, au, _ := newReconcileServer(t)
	seedRunningRun(t, repo)
	au.listByCategoryErr = context.DeadlineExceeded

	n, err := s.ReconcileOrphanedReviews(context.Background())
	if err != nil {
		t.Fatalf("ReconcileOrphanedReviews returned error, want per-run isolation: %v", err)
	}
	if n != 0 {
		t.Fatalf("terminated runs = %d, want 0", n)
	}
}

// TestReconcileOrphanedReviews_ListRunsErrorAborts covers the systemic-listing
// abort: a ListRuns failure is returned rather than swallowed.
func TestReconcileOrphanedReviews_ListRunsErrorAborts(t *testing.T) {
	s, repo, _, _ := newReconcileServer(t)
	repo.listErr = context.DeadlineExceeded

	if _, err := s.ReconcileOrphanedReviews(context.Background()); err == nil {
		t.Fatal("ReconcileOrphanedReviews: want error on ListRuns failure, got nil")
	}
}

// TestReconcileOrphanedReviews_NilReposError covers the wiring guards: a nil
// RunRepo or AuditRepo returns an error rather than panicking.
func TestReconcileOrphanedReviews_NilReposError(t *testing.T) {
	au := newAuditFake()
	sNoRun := New(Config{Addr: "127.0.0.1:0", AuditRepo: au, ProcessStart: bootMarker})
	if _, err := sNoRun.ReconcileOrphanedReviews(context.Background()); err == nil {
		t.Error("want error when RunRepo is nil")
	}
	repo := newFakeRepo()
	sNoAudit := New(Config{Addr: "127.0.0.1:0", RunRepo: repo, ProcessStart: bootMarker})
	if _, err := sNoAudit.ReconcileOrphanedReviews(context.Background()); err == nil {
		t.Error("want error when AuditRepo is nil")
	}
}
