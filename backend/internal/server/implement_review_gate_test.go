package server

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/orchestrator"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// gateAuditRepo is the audit.Repository fake the implement-review gate
// tests drive. It captures AppendChained calls (appended) and replays
// category-scoped reads via ListForRunByCategory: byCategory seeds the
// implement_review_started + terminal-category counts the gate reads,
// and listErr forces those reads to fail so the fail-open posture can
// be pinned.
type gateAuditRepo struct {
	audit.Repository
	mu         sync.Mutex
	appended   []audit.ChainAppendParams
	err        error
	byCategory map[string][]*audit.Entry
	listErr    error
}

func (r *gateAuditRepo) AppendChained(_ context.Context, p audit.ChainAppendParams) (*audit.Entry, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return nil, r.err
	}
	r.appended = append(r.appended, p)
	return &audit.Entry{ID: uuid.New()}, nil
}

// ListForRunByCategory replays the seeded entries for a category. The
// implement-review gate reads implement_review_started + the three
// terminal categories through this surface.
func (r *gateAuditRepo) ListForRunByCategory(_ context.Context, _ uuid.UUID, category string) ([]*audit.Entry, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.listErr != nil {
		return nil, r.listErr
	}
	return r.byCategory[category], nil
}

// seedCategory appends an audit entry of the given category + timestamp
// to the fake's ListForRunByCategory replay set.
func (r *gateAuditRepo) seedCategory(category string, ts time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.byCategory == nil {
		r.byCategory = map[string][]*audit.Entry{}
	}
	r.byCategory[category] = append(r.byCategory[category], &audit.Entry{
		Category:  category,
		Timestamp: ts,
	})
}

// --- ADR-036 (#876): implement-review / merge completion gate --------------
//
// These tests drive the FULL gate-plus-resolver seam
// (resolveReviewStageOnMerge -> checkImplementReviewSettled -> audit-read ->
// stage-transition -> orchestrator-advance), not isolated helper units, so a
// per-layer unit cannot pass while the seam is broken (cf. #618).

// specImplementReviewers builds a feature_change workflow whose IMPLEMENT
// stage declares the given reviewers.agent count. agent>0 is what arms the
// merge completion gate.
func specImplementReviewers(agent int) []byte {
	return []byte(fmt.Sprintf(`version: "0.3"
workflows:
  feature_change:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
      - id: implement
        type: implement
        executor:
          agent: claude-code
        reviewers:
          agent: %d
          human: 0
      - id: review
        type: review
        executor:
          human: true
`, agent))
}

// newImplementReviewGateRun seeds a feature_change run parked at the review
// stage (awaiting_approval), wiring ONE prEventsRunRepo into both Config.RunRepo
// and the Orchestrator so the gate-plus-resolver seam drives the run to its
// terminal state on resolve. The implement stage carries the configured
// reviewers so resolveStageReviewers reads them back.
func newImplementReviewGateRun(t *testing.T, agent int) (*Server, *prEventsRunRepo, *gateAuditRepo, uuid.UUID, uuid.UUID) {
	t.Helper()
	runID := uuid.New()
	reviewStageID := uuid.New()
	prURL := "https://github.com/x/y/pull/42"
	workflowSpec := specImplementReviewers(agent)
	rr := &prEventsRunRepo{
		listResult: []*run.Run{{
			ID:             runID,
			State:          run.StateRunning,
			PullRequestURL: &prURL,
			WorkflowID:     "feature_change",
			WorkflowSpec:   workflowSpec,
		}},
		stages: map[uuid.UUID][]*run.Stage{
			runID: {
				{ID: uuid.New(), RunID: runID, Type: run.StageTypePlan, State: run.StageStateSucceeded},
				{ID: uuid.New(), RunID: runID, Type: run.StageTypeImplement, State: run.StageStateSucceeded},
				{ID: reviewStageID, RunID: runID, Type: run.StageTypeReview, State: run.StageStateAwaitingApproval},
			},
		},
	}
	ar := &gateAuditRepo{}
	s := New(Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      rr,
		AuditRepo:    ar,
		Orchestrator: &orchestrator.Orchestrator{Runs: rr},
	})
	return s, rr, ar, runID, reviewStageID
}

const implementReviewGatePRURL = "https://github.com/x/y/pull/42"

// (a) merged + agent review in-flight (started present, no terminal): the
// review stage stays awaiting_approval, no pr_merged audit, run not advanced.
func TestImplementReviewGate_InFlight_HoldsMerge(t *testing.T) {
	s, rr, ar, runID, _ := newImplementReviewGateRun(t, 1)
	ar.seedCategory("implement_review_started", time.Now().UTC())

	if err := s.ResolveReviewFromPollState(context.Background(), runID, true, implementReviewGatePRURL); err != nil {
		t.Fatalf("ResolveReviewFromPollState: %v", err)
	}

	if len(rr.transitions) != 0 {
		t.Errorf("transitions = %d, want 0 (held pending implement review)", len(rr.transitions))
	}
	if findCategory(ar.appended, CategoryPRMerged) != nil {
		t.Errorf("unexpected pr_merged row while held: %v", auditCategories(ar.appended))
	}
	if got := rr.runStates[runID]; got != "" {
		t.Errorf("run state = %q, want unset (run not advanced)", got)
	}
}

// (b) the re-poll unblock: a held run resolves on a subsequent
// ResolveReviewFromPollState once a terminal implement_reviewed is seeded —
// transitions to succeeded + writes pr_merged + drives the run to succeeded.
func TestImplementReviewGate_RePollAfterTerminal_Resolves(t *testing.T) {
	s, rr, ar, runID, reviewStageID := newImplementReviewGateRun(t, 1)
	now := time.Now().UTC()
	ar.seedCategory("implement_review_started", now)

	// First poll: held.
	if err := s.ResolveReviewFromPollState(context.Background(), runID, true, implementReviewGatePRURL); err != nil {
		t.Fatalf("first poll: %v", err)
	}
	if len(rr.transitions) != 0 {
		t.Fatalf("first poll transitions = %d, want 0 (held)", len(rr.transitions))
	}

	// Terminal review lands; the reconciler re-polls.
	ar.seedCategory("implement_reviewed", now)
	if err := s.ResolveReviewFromPollState(context.Background(), runID, true, implementReviewGatePRURL); err != nil {
		t.Fatalf("re-poll: %v", err)
	}

	if len(rr.transitions) != 1 || rr.transitions[0].StageID != reviewStageID || rr.transitions[0].To != run.StageStateSucceeded {
		t.Fatalf("transitions = %+v, want one review->succeeded", rr.transitions)
	}
	if findCategory(ar.appended, CategoryPRMerged) == nil {
		t.Errorf("missing pr_merged row after unblock; got %v", auditCategories(ar.appended))
	}
	if got := rr.runStates[runID]; got != run.StateSucceeded {
		t.Errorf("run state = %q, want succeeded", got)
	}
}

// (c) ANY terminal kind unblocks — parametrized across the three terminal
// implement-review categories.
func TestImplementReviewGate_AnyTerminalUnblocks(t *testing.T) {
	for _, terminal := range []string{"implement_reviewed", "implement_review_failed", "implement_review_skipped"} {
		t.Run(terminal, func(t *testing.T) {
			s, rr, ar, runID, _ := newImplementReviewGateRun(t, 1)
			now := time.Now().UTC()
			ar.seedCategory("implement_review_started", now)
			ar.seedCategory(terminal, now)

			if err := s.ResolveReviewFromPollState(context.Background(), runID, true, implementReviewGatePRURL); err != nil {
				t.Fatalf("ResolveReviewFromPollState: %v", err)
			}
			if len(rr.transitions) != 1 || rr.transitions[0].To != run.StageStateSucceeded {
				t.Fatalf("transitions = %+v, want one to succeeded for terminal %q", rr.transitions, terminal)
			}
			if findCategory(ar.appended, CategoryPRMerged) == nil {
				t.Errorf("missing pr_merged row for terminal %q", terminal)
			}
		})
	}
}

// (d) reviewers.agent==0 → resolves immediately (no-op, no hold).
func TestImplementReviewGate_NoAgentReviewer_ResolvesImmediately(t *testing.T) {
	s, rr, ar, runID, _ := newImplementReviewGateRun(t, 0)
	// Even a stray started entry must not arm the gate when agent==0.
	ar.seedCategory("implement_review_started", time.Now().UTC())

	if err := s.ResolveReviewFromPollState(context.Background(), runID, true, implementReviewGatePRURL); err != nil {
		t.Fatalf("ResolveReviewFromPollState: %v", err)
	}
	if len(rr.transitions) != 1 || rr.transitions[0].To != run.StageStateSucceeded {
		t.Fatalf("transitions = %+v, want one to succeeded (agent==0 no-op)", rr.transitions)
	}
}

// (e) no implement_review_started → resolves immediately (configured but
// never dispatched — nothing to wait for).
func TestImplementReviewGate_NoStartedEntry_ResolvesImmediately(t *testing.T) {
	s, rr, _, runID, _ := newImplementReviewGateRun(t, 1)

	if err := s.ResolveReviewFromPollState(context.Background(), runID, true, implementReviewGatePRURL); err != nil {
		t.Fatalf("ResolveReviewFromPollState: %v", err)
	}
	if len(rr.transitions) != 1 || rr.transitions[0].To != run.StageStateSucceeded {
		t.Fatalf("transitions = %+v, want one to succeeded (no started entry)", rr.transitions)
	}
}

// (f) backstop: started older than the bound with no terminal → resolves and
// writes exactly one implement_review_backstop_elapsed audit entry.
func TestImplementReviewGate_BackstopElapsed_ResolvesAndAuditsOnce(t *testing.T) {
	s, rr, ar, runID, _ := newImplementReviewGateRun(t, 1)
	// Default ReviewBudget.Cap is 1200s × 1 agent; seed well past it.
	ar.seedCategory("implement_review_started", time.Now().UTC().Add(-2*time.Hour))

	if err := s.ResolveReviewFromPollState(context.Background(), runID, true, implementReviewGatePRURL); err != nil {
		t.Fatalf("ResolveReviewFromPollState: %v", err)
	}
	if len(rr.transitions) != 1 || rr.transitions[0].To != run.StageStateSucceeded {
		t.Fatalf("transitions = %+v, want one to succeeded (backstop elapsed)", rr.transitions)
	}
	backstops := 0
	for _, e := range ar.appended {
		if e.Category == "implement_review_backstop_elapsed" {
			backstops++
		}
	}
	if backstops != 1 {
		t.Errorf("implement_review_backstop_elapsed rows = %d, want exactly 1", backstops)
	}
}

// (g) read-error on ListForRunByCategory fails open (resolves).
func TestImplementReviewGate_ReadError_FailsOpen(t *testing.T) {
	s, rr, ar, runID, _ := newImplementReviewGateRun(t, 1)
	ar.listErr = errors.New("transient backend hiccup")

	if err := s.ResolveReviewFromPollState(context.Background(), runID, true, implementReviewGatePRURL); err != nil {
		t.Fatalf("ResolveReviewFromPollState: %v", err)
	}
	if len(rr.transitions) != 1 || rr.transitions[0].To != run.StageStateSucceeded {
		t.Fatalf("transitions = %+v, want one to succeeded (fail open)", rr.transitions)
	}
}

// (h) merged==false (closed without merge) is NEVER gated, even with an
// in-flight implement review: an abandoned PR is terminal regardless of
// review state.
func TestImplementReviewGate_ClosedUnmerged_NeverGated(t *testing.T) {
	s, rr, ar, runID, _ := newImplementReviewGateRun(t, 1)
	ar.seedCategory("implement_review_started", time.Now().UTC())

	if err := s.ResolveReviewFromPollState(context.Background(), runID, false, implementReviewGatePRURL); err != nil {
		t.Fatalf("ResolveReviewFromPollState: %v", err)
	}
	if len(rr.transitions) != 1 || rr.transitions[0].To != run.StageStateCancelled {
		t.Fatalf("transitions = %+v, want one to cancelled (close never gated)", rr.transitions)
	}
	if findCategory(ar.appended, CategoryPRClosedWithoutMerge) == nil {
		t.Errorf("missing pr_closed_without_merge row; got %v", auditCategories(ar.appended))
	}
}

// TestImplementReviewGate_WebhookHeld_NoDuplicateAuditPerTick is the
// deliberate behavioral-change guard: deferring writePRMergedAudit behind the
// gate means a held run writes NO pr_merged row on each re-poll tick. Two
// held polls must leave the audit chain free of pr_merged entirely.
func TestImplementReviewGate_HeldPolls_NoPRMergedSpam(t *testing.T) {
	s, _, ar, runID, _ := newImplementReviewGateRun(t, 1)
	ar.seedCategory("implement_review_started", time.Now().UTC())

	for i := 0; i < 3; i++ {
		if err := s.ResolveReviewFromPollState(context.Background(), runID, true, implementReviewGatePRURL); err != nil {
			t.Fatalf("poll %d: %v", i, err)
		}
	}
	if row := findCategory(ar.appended, CategoryPRMerged); row != nil {
		t.Errorf("pr_merged written while held across re-polls; want none: %v", auditCategories(ar.appended))
	}
}

// sanity: the gate-built run's WorkflowSpec parses to the configured reviewers
// so the seam under test actually arms (guards against a spec typo silently
// disabling every gate assertion above via the nil-reviewers no-op path).
func TestImplementReviewGate_SpecArmsReviewers(t *testing.T) {
	s, rr, _, _, _ := newImplementReviewGateRun(t, 2)
	cfg := s.resolveStageReviewers(context.Background(), rr.listResult[0], spec.StageTypeImplement)
	if cfg == nil || cfg.Agent != 2 {
		t.Fatalf("resolveStageReviewers(implement) = %+v, want agent:2", cfg)
	}
}
