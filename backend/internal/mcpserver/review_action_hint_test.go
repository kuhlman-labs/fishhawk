package mcpserver

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// seedFixupTriggeredAudit appends a stage_fixup_triggered audit entry keyed
// to stageID — the durable fix-up-pass record reviewActionHintFor counts the
// prior passes (and reads the latest-round boundary sequence) against.
func seedFixupTriggeredAudit(fb *fakeBackend, runID, stageID uuid.UUID) {
	sid := stageID.String()
	fb.mu.Lock()
	fb.perRunAuditByRun[runID] = append(fb.perRunAuditByRun[runID], AuditEntry{
		ID:       uuid.New().String(),
		Sequence: int64(len(fb.perRunAuditByRun[runID]) + 1),
		RunID:    runID.String(),
		StageID:  &sid,
		Category: categoryStageFixupTriggered,
	})
	fb.mu.Unlock()
}

// seedFixupNoChangesAudit appends a fixup_no_changes audit entry keyed to
// stageID — the durable refund signal reviewActionHintFor counts to widen the
// normal fix-up budget, mirroring the backend's no-change refund (#967).
func seedFixupNoChangesAudit(fb *fakeBackend, runID, stageID uuid.UUID) {
	sid := stageID.String()
	fb.mu.Lock()
	fb.perRunAuditByRun[runID] = append(fb.perRunAuditByRun[runID], AuditEntry{
		ID:       uuid.New().String(),
		Sequence: int64(len(fb.perRunAuditByRun[runID]) + 1),
		RunID:    runID.String(),
		StageID:  &sid,
		Category: categoryFixupNoChanges,
	})
	fb.mu.Unlock()
}

// seedDispatchReaperFailedAudit appends a dispatch_reaper_failed audit entry
// keyed to stageID carrying failure_category — the #1747 spawn-phase reaper
// death signal fixupInfraRefunds pairs against a trigger window (#1957). A
// failure_category of "C" refunds a normal pass; any other category does not.
func seedDispatchReaperFailedAudit(fb *fakeBackend, runID, stageID uuid.UUID, failureCategory string) {
	sid := stageID.String()
	fb.mu.Lock()
	fb.perRunAuditByRun[runID] = append(fb.perRunAuditByRun[runID], AuditEntry{
		ID:       uuid.New().String(),
		Sequence: int64(len(fb.perRunAuditByRun[runID]) + 1),
		RunID:    runID.String(),
		StageID:  &sid,
		Category: categoryDispatchReaperFailed,
		Payload:  map[string]any{"failure_category": failureCategory},
	})
	fb.mu.Unlock()
}

// seedStageFixupRecoveredAudit appends a stage_fixup_recovered audit entry keyed
// to stageID carrying source_failure_category — the #788 post-agent-work recovery
// death signal fixupInfraRefunds pairs against a trigger window (#1957). A
// source_failure_category of "C" refunds a normal pass; any other does not.
func seedStageFixupRecoveredAudit(fb *fakeBackend, runID, stageID uuid.UUID, sourceFailureCategory string) {
	sid := stageID.String()
	fb.mu.Lock()
	fb.perRunAuditByRun[runID] = append(fb.perRunAuditByRun[runID], AuditEntry{
		ID:       uuid.New().String(),
		Sequence: int64(len(fb.perRunAuditByRun[runID]) + 1),
		RunID:    runID.String(),
		StageID:  &sid,
		Category: categoryStageFixupRecovered,
		Payload:  map[string]any{"source_failure_category": sourceFailureCategory},
	})
	fb.mu.Unlock()
}

// seedInfraSignalUnparseable appends a dispatch_reaper_failed audit entry whose
// payload is a JSON array — it fails to decode into the failure_category struct,
// so fixupInfraRefunds must skip it without error and never refund (#1957).
func seedInfraSignalUnparseable(fb *fakeBackend, runID, stageID uuid.UUID) {
	sid := stageID.String()
	fb.mu.Lock()
	fb.perRunAuditByRun[runID] = append(fb.perRunAuditByRun[runID], AuditEntry{
		ID:       uuid.New().String(),
		Sequence: int64(len(fb.perRunAuditByRun[runID]) + 1),
		RunID:    runID.String(),
		StageID:  &sid,
		Category: categoryDispatchReaperFailed,
		Payload:  []int{1, 2, 3},
	})
	fb.mu.Unlock()
}

// seedRecoveredSignalUnparseable appends a stage_fixup_recovered audit entry
// whose payload is a JSON array — it fails to decode into the
// source_failure_category struct, so fixupInfraRefunds must skip it without
// error and never refund (#1957). The recovered-shape analog of
// seedInfraSignalUnparseable, so the skip guard is exercised on BOTH signal
// branches, not just the reaper one.
func seedRecoveredSignalUnparseable(fb *fakeBackend, runID, stageID uuid.UUID) {
	sid := stageID.String()
	fb.mu.Lock()
	fb.perRunAuditByRun[runID] = append(fb.perRunAuditByRun[runID], AuditEntry{
		ID:       uuid.New().String(),
		Sequence: int64(len(fb.perRunAuditByRun[runID]) + 1),
		RunID:    runID.String(),
		StageID:  &sid,
		Category: categoryStageFixupRecovered,
		Payload:  []int{1, 2, 3},
	})
	fb.mu.Unlock()
}

// seedImplementReviewedAudit appends an implement_reviewed audit entry keyed
// to stageID carrying an approve_with_concerns verdict with n concerns — the
// round-scoped source reviewActionHintFor counts concerns from (#860).
func seedImplementReviewedAudit(fb *fakeBackend, runID, stageID uuid.UUID, n int) {
	sid := stageID.String()
	fb.mu.Lock()
	fb.perRunAuditByRun[runID] = append(fb.perRunAuditByRun[runID], AuditEntry{
		ID:       uuid.New().String(),
		Sequence: int64(len(fb.perRunAuditByRun[runID]) + 1),
		RunID:    runID.String(),
		StageID:  &sid,
		Category: "implement_reviewed",
		Payload:  withConcerns(n),
	})
	fb.mu.Unlock()
}

// completeStatus builds a complete implement ReviewStatus — the shape
// getRunStatus/run_stage feed reviewActionHintFor for the complete/none gate.
// The concern COUNT is now read from the seeded implement_reviewed audit
// entries (round-scoped), not from this struct's Reviews.
func completeStatus() *ReviewStatus {
	return &ReviewStatus{Stage: "implement", Status: "complete"}
}

// withConcerns is an approve_with_concerns verdict carrying n concerns.
func withConcerns(n int) PlanReview {
	concerns := make([]PlanReviewConcern, n)
	for i := range concerns {
		concerns[i] = PlanReviewConcern{Severity: "medium", Category: "scope", Note: "fix it"}
	}
	return PlanReview{ReviewerKind: "agent", Verdict: "approve_with_concerns", Concerns: concerns}
}

func TestReviewActionHintFor(t *testing.T) {
	tests := []struct {
		name   string
		status *ReviewStatus
		// runState is the run state fed to the hint; empty defaults to
		// "running". Terminal states suppress the hint entirely (#968).
		runState string
		// seedConcerns, when > 0, seeds one implement_reviewed entry with
		// that many concerns against the implement stage.
		seedConcerns int
		// priorPasses seeds that many stage_fixup_triggered entries against
		// the implement stage BEFORE the implement_reviewed entry, modelling a
		// prior fix-up round. The latest-round count then only includes the
		// implement_reviewed entry seeded after them.
		priorPasses int
		// refunds seeds that many fixup_no_changes entries against the
		// implement stage — no-change passes refunded against the normal
		// budget (#967/#1150). The hint widens the normal budget by the
		// refund count (clamped to priorPasses).
		refunds int
		// fixupOnOtherStage, when true, seeds the prior passes against a
		// DIFFERENT stage so they do not count against this one.
		fixupOnOtherStage bool
		// infraRounds seeds that many (stage_fixup_triggered, in-window signal)
		// PAIRS interleaved — each trigger immediately followed by one signal
		// sequenced strictly inside its window — modelling the real per-window
		// sequencing (#1957). Each pair counts as a RAW pass IN ADDITION to
		// priorPasses; a category-C signal refunds one normal pass.
		infraRounds int
		// infraSignalKind selects the per-round signal shape: "recovered"
		// (stage_fixup_recovered / source_failure_category) else "reaper"
		// (dispatch_reaper_failed / failure_category, the default).
		infraSignalKind string
		// infraSignalCategory overrides the per-round signal's failure category
		// (default "C"). A non-C category must NOT refund.
		infraSignalCategory string
		// infraSignalUnparseable seeds each per-round signal with a payload that
		// fails to decode — skipped without error, never refunds.
		infraSignalUnparseable bool
		// signalBeforeFirstTrigger seeds ONE category-C reaper signal BEFORE the
		// first trigger (an original-dispatch spawn death) — it matches no
		// window and must never refund.
		signalBeforeFirstTrigger bool
		wantNil                  bool
		wantConcerns             int
		wantRemaining            int
		wantOverride             bool
		// wantMessageContains, when non-empty, asserts the hint Message
		// contains this substring — used to pin the #1097 commit-and-vouch
		// wording on the hard-ceiling arm.
		wantMessageContains string
		// storeAbsent passes a nil *RunConcerns block so the count DEGRADES to
		// the audit-derived path (#3043). Default (false) passes an authoritative
		// store block whose OpenImplement drives the count.
		storeAbsent bool
		// storeOpenImplement overrides the store block's authoritative
		// OpenImplement count; nil defaults it to seedConcerns (so existing
		// budget/ceiling cases keep wantConcerns == seedConcerns via the store).
		storeOpenImplement *int
		// storeOpen overrides the store block's Open field; 0 defaults it to
		// seedConcerns. Used to model a block whose open set includes plan-stage
		// concerns (Open > OpenImplement).
		storeOpen int
		// storeItems sets how many (dummy) items the transported block carries,
		// independent of OpenImplement — to prove a truncated/partial Items list
		// never changes the authoritative count.
		storeItems int
		// wantSource, when non-empty, asserts hint.Source.
		wantSource string
	}{
		{
			name:    "nil status -> no hint",
			status:  nil,
			wantNil: true,
		},
		{
			name:    "status none -> no hint",
			status:  &ReviewStatus{Stage: "implement", Status: "none"},
			wantNil: true,
		},
		{
			name:    "status pending -> no hint",
			status:  &ReviewStatus{Stage: "implement", Status: "pending"},
			wantNil: true,
		},
		{
			name:         "complete with no concerns -> no hint",
			status:       completeStatus(),
			seedConcerns: 0,
			wantNil:      true,
		},
		{
			name:          "complete with concerns, no prior fix-up -> hint",
			status:        completeStatus(),
			seedConcerns:  2,
			wantNil:       false,
			wantConcerns:  2,
			wantRemaining: 1,
			wantOverride:  false,
		},
		{
			name:          "budget spent, below ceiling -> exhaustion hint with override",
			status:        completeStatus(),
			seedConcerns:  1,
			priorPasses:   1,
			wantNil:       false,
			wantConcerns:  1,
			wantRemaining: 0,
			wantOverride:  true,
		},
		{
			// #1150 (a): a no-change pass is refunded against the normal
			// budget — one triggered + one refund => effectiveConsumed=0 < 1
			// => a normal route-back is restored (the core assertion the
			// backend's widened MaxPasses admits without force_additional_pass).
			name:          "refund restores normal budget -> route-back, no override",
			status:        completeStatus(),
			seedConcerns:  1,
			priorPasses:   1,
			refunds:       1,
			wantNil:       false,
			wantConcerns:  1,
			wantRemaining: 1,
			wantOverride:  false,
		},
		{
			// #1150 (c): a refund count exceeding the triggered passes is
			// clamped to priorPasses, so remaining never widens past the
			// normal budget. Mirrors the backend's refundedPasses>priorPasses
			// clamp.
			name:          "refund clamped to prior passes -> remaining capped at budget",
			status:        completeStatus(),
			seedConcerns:  1,
			priorPasses:   1,
			refunds:       2,
			wantNil:       false,
			wantConcerns:  1,
			wantRemaining: 1,
			wantOverride:  false,
		},
		{
			// #1150 (d): two triggered + one refund => effectiveConsumed=1,
			// which is NOT < maxFixupPasses(1), so the NORMAL arm does not
			// fire — the override arm does. raw priorPasses=2 is still < the
			// hard ceiling of 3, so an override pass is available. Proves the
			// override arm keys off RAW priorPasses, not effectiveConsumed.
			name:          "two passes, one refund -> override (keys off raw priorPasses)",
			status:        completeStatus(),
			seedConcerns:  1,
			priorPasses:   2,
			refunds:       1,
			wantNil:       false,
			wantConcerns:  1,
			wantRemaining: 0,
			wantOverride:  true,
		},
		{
			// #1150 (d) boundary: three triggered + one refund =>
			// effectiveConsumed=2. If the ceiling arm wrongly keyed off
			// effectiveConsumed (2 < 3) it would still offer an override; it
			// must key off RAW priorPasses=3, which is at the ceiling => no
			// override left. This is the case that truly distinguishes raw
			// from effective.
			name:                "ceiling keys off raw passes despite refund -> no override",
			status:              completeStatus(),
			seedConcerns:        1,
			priorPasses:         3,
			refunds:             1,
			wantNil:             false,
			wantConcerns:        1,
			wantRemaining:       0,
			wantOverride:        false,
			wantMessageContains: "fishhawk_vouch_commit",
		},
		{
			name:                "ceiling reached -> hard-stop hint, no override",
			status:              completeStatus(),
			seedConcerns:        1,
			priorPasses:         3,
			wantNil:             false,
			wantConcerns:        1,
			wantRemaining:       0,
			wantOverride:        false,
			wantMessageContains: "fishhawk_vouch_commit",
		},
		{
			name:              "fix-up on a different stage does not consume budget -> below-budget hint",
			status:            completeStatus(),
			seedConcerns:      1,
			priorPasses:       1,
			fixupOnOtherStage: true,
			wantNil:           false,
			wantConcerns:      1,
			wantRemaining:     1,
			wantOverride:      false,
		},
		{
			// #1957 (a): a dispatch_reaper_failed(C) signal inside a trigger
			// window refunds the pass against the normal budget — the surface
			// must agree with the backend admitting a normal pass without force.
			name:          "reaper-C in window refunds -> route-back, no override",
			status:        completeStatus(),
			seedConcerns:  1,
			infraRounds:   1,
			wantNil:       false,
			wantConcerns:  1,
			wantRemaining: 1,
			wantOverride:  false,
		},
		{
			// #1957 (b): a stage_fixup_recovered(C) signal inside a window
			// refunds identically — the #788 post-agent-work delivered-nothing
			// recovery path.
			name:            "recovered-C in window refunds -> route-back, no override",
			status:          completeStatus(),
			seedConcerns:    1,
			infraRounds:     1,
			infraSignalKind: "recovered",
			wantNil:         false,
			wantConcerns:    1,
			wantRemaining:   1,
			wantOverride:    false,
		},
		{
			// #1957 (c): a category-C signal sequenced BEFORE the first trigger
			// (an original-dispatch spawn death, not a fix-up) matches no window
			// and does NOT refund — budget stays spent, override available.
			name:                     "category-C before first trigger does not refund -> override",
			status:                   completeStatus(),
			seedConcerns:             1,
			priorPasses:              1,
			signalBeforeFirstTrigger: true,
			wantNil:                  false,
			wantConcerns:             1,
			wantRemaining:            0,
			wantOverride:             true,
		},
		{
			// #1957 (d): a non-C (category A) signal inside a window does NOT
			// refund — only category C (infrastructure, delivered-nothing)
			// refunds; agent/policy failures still consume budget.
			name:                "category-A in window does not refund -> override",
			status:              completeStatus(),
			seedConcerns:        1,
			infraRounds:         1,
			infraSignalCategory: "A",
			wantNil:             false,
			wantConcerns:        1,
			wantRemaining:       0,
			wantOverride:        true,
		},
		{
			// #1957 (e): an unparseable signal payload is skipped without error
			// and never refunds — budget stays spent, override available.
			name:                   "unparseable signal payload skipped -> override",
			status:                 completeStatus(),
			seedConcerns:           1,
			infraRounds:            1,
			infraSignalUnparseable: true,
			wantNil:                false,
			wantConcerns:           1,
			wantRemaining:          0,
			wantOverride:           true,
		},
		{
			// #1957 (d'): the category gate guards the RECOVERED branch too,
			// not just the reaper branch — a non-C (category A)
			// stage_fixup_recovered signal inside a window must NOT refund.
			// Without this, a recovered-block regression that refunded
			// regardless of source_failure_category would still pass case (b)
			// (recovered-C refunds) and case (d) (which only exercises the
			// reaper branch), leaving the recovered category-gate untested.
			name:                "recovered category-A in window does not refund -> override",
			status:              completeStatus(),
			seedConcerns:        1,
			infraRounds:         1,
			infraSignalKind:     "recovered",
			infraSignalCategory: "A",
			wantNil:             false,
			wantConcerns:        1,
			wantRemaining:       0,
			wantOverride:        true,
		},
		{
			// #1957 (e'): the unparseable-payload skip guard covers the
			// RECOVERED branch too — an undecodable stage_fixup_recovered
			// payload is skipped without error and never refunds, mirroring
			// case (e) for the reaper branch.
			name:                   "unparseable recovered signal payload skipped -> override",
			status:                 completeStatus(),
			seedConcerns:           1,
			infraRounds:            1,
			infraSignalKind:        "recovered",
			infraSignalUnparseable: true,
			wantNil:                false,
			wantConcerns:           1,
			wantRemaining:          0,
			wantOverride:           true,
		},
		{
			// #1957 (f): the SUMMED no-change + infra refund is clamped to the
			// raw passes actually triggered, so remaining never widens past the
			// normal budget. One infra round (raw=1, infra refund=1) plus one
			// no-change refund => sum 2 clamped to 1 => remaining 1.
			name:          "summed no-change + infra refund clamped to prior passes",
			status:        completeStatus(),
			seedConcerns:  1,
			infraRounds:   1,
			refunds:       1,
			wantNil:       false,
			wantConcerns:  1,
			wantRemaining: 1,
			wantOverride:  false,
		},
		{
			// #1957 (g): ceiling precedence — three raw triggers each with an
			// in-window C signal (raw=3, refunds=3). The hard-ceiling check is
			// hoisted ahead of the normal-budget arm (matching the backend's
			// ErrFixupCeilingReached-before-budget precedence), so even though
			// the summed refunds leave effectiveConsumed=0, priorPasses=3 at the
			// ceiling yields remaining=0/override=false and the ceiling message,
			// NOT a spurious remaining normal pass.
			name:                "raw ceiling with full refunds -> no override (ceiling precedence)",
			status:              completeStatus(),
			seedConcerns:        1,
			infraRounds:         3,
			wantNil:             false,
			wantConcerns:        1,
			wantRemaining:       0,
			wantOverride:        false,
			wantMessageContains: "fishhawk_vouch_commit",
		},
		{
			// #968: a terminal run has no actionable fix-up — the server
			// refuses with fixup_not_applicable — so the hint must suppress
			// even when concerns remain and the ceiling has headroom (the
			// shape that advertised override_available on run 68e13183).
			name:         "run succeeded -> no hint despite concerns",
			status:       completeStatus(),
			runState:     "succeeded",
			seedConcerns: 1,
			priorPasses:  1,
			wantNil:      true,
		},
		{
			name:         "run failed -> no hint despite concerns",
			status:       completeStatus(),
			runState:     "failed",
			seedConcerns: 1,
			wantNil:      true,
		},
		{
			name:         "run cancelled -> no hint despite concerns",
			status:       completeStatus(),
			runState:     "cancelled",
			seedConcerns: 1,
			wantNil:      true,
		},
		{
			name:          "run running -> hint still surfaces",
			status:        completeStatus(),
			runState:      "running",
			seedConcerns:  1,
			priorPasses:   1,
			wantNil:       false,
			wantConcerns:  1,
			wantRemaining: 0,
			wantOverride:  true,
		},
		{
			// #3043 (a): the store block is authoritative. The audit says 2
			// concerns, but the store reports OpenImplement:0 (every concern
			// waived/deferred/addressed) -> the hint suppresses. This is the core
			// defect: the old audit-derived count mis-fired here.
			name:               "store open_implement:0 despite audit=2 -> no hint",
			status:             completeStatus(),
			seedConcerns:       2,
			storeOpenImplement: intPtr(0),
			storeOpen:          0,
			wantNil:            true,
		},
		{
			// #3043 (b): the count comes from the store's OpenImplement, not the
			// audit sum. Audit seeds 5, store reports 1 -> concerns:1, source store.
			name:               "store open_implement:1 wins over larger audit sum -> concerns 1",
			status:             completeStatus(),
			seedConcerns:       5,
			storeOpenImplement: intPtr(1),
			storeOpen:          1,
			wantNil:            false,
			wantConcerns:       1,
			wantRemaining:      1,
			wantOverride:       false,
			wantSource:         hintSourceStore,
		},
		{
			// #3043 (c): the store carries ONLY a plan-stage open concern
			// (Open:1, OpenImplement:0) -> the hint suppresses (the stage-kind
			// filter). Without it the hint would route a plan concern into an
			// implement fix-up.
			name:               "store open holds only a plan concern -> no hint",
			status:             completeStatus(),
			seedConcerns:       0,
			storeOpen:          1,
			storeOpenImplement: intPtr(0),
			wantNil:            true,
		},
		{
			// #3043 (d): the concerns block is UNAVAILABLE (nil) -> the count
			// degrades to the audit-derived path with today's behavior, marked
			// source=audit_fallback_store_unavailable and the message says the
			// store was unavailable, so a degraded count is not mistaken for an
			// authoritative one.
			name:                "store absent -> audit fallback fires with the old count",
			status:              completeStatus(),
			seedConcerns:        2,
			storeAbsent:         true,
			wantNil:             false,
			wantConcerns:        2,
			wantRemaining:       1,
			wantOverride:        false,
			wantSource:          hintSourceStoreUnavailable,
			wantMessageContains: "the concern store was unavailable",
		},
		{
			// #3043 (e): a TRUNCATED transported Items list never changes the
			// authoritative count. OpenImplement:3 with only 1 retained item ->
			// concerns:3, not 1. (A mixed-stage variant is
			// TestReviewActionHintFor_TruncatedMixedStage.)
			name:               "truncated items -> count from authoritative OpenImplement, not item count",
			status:             completeStatus(),
			seedConcerns:       0,
			storeOpen:          5,
			storeOpenImplement: intPtr(3),
			storeItems:         1,
			wantNil:            false,
			wantConcerns:       3,
			wantRemaining:      1,
			wantOverride:       false,
			wantSource:         hintSourceStore,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fb, srv := newFakeBackend(t)
			runID := uuid.New()
			implementStageID := uuid.New()
			// A category-C signal sequenced BEFORE the first trigger (an
			// original-dispatch spawn death, not a fix-up) must match no window.
			if tc.signalBeforeFirstTrigger {
				seedDispatchReaperFailedAudit(fb, runID, implementStageID, "C")
			}
			// Seed prior fix-up passes first so the latest-round concern
			// count only includes the implement_reviewed entry seeded after.
			for i := 0; i < tc.priorPasses; i++ {
				if tc.fixupOnOtherStage {
					seedFixupTriggeredAudit(fb, runID, uuid.New())
				} else {
					seedFixupTriggeredAudit(fb, runID, implementStageID)
				}
			}
			// Interleaved infra rounds: each trigger immediately followed by a
			// signal sequenced strictly inside its window, so the per-window
			// pairing in fixupInfraRefunds can match it (consecutive triggers
			// would leave no integer sequence between them).
			infraCat := tc.infraSignalCategory
			if infraCat == "" {
				infraCat = "C"
			}
			for i := 0; i < tc.infraRounds; i++ {
				seedFixupTriggeredAudit(fb, runID, implementStageID)
				switch {
				case tc.infraSignalUnparseable && tc.infraSignalKind == "recovered":
					seedRecoveredSignalUnparseable(fb, runID, implementStageID)
				case tc.infraSignalUnparseable:
					seedInfraSignalUnparseable(fb, runID, implementStageID)
				case tc.infraSignalKind == "recovered":
					seedStageFixupRecoveredAudit(fb, runID, implementStageID, infraCat)
				default:
					seedDispatchReaperFailedAudit(fb, runID, implementStageID, infraCat)
				}
			}
			for i := 0; i < tc.refunds; i++ {
				seedFixupNoChangesAudit(fb, runID, implementStageID)
			}
			if tc.seedConcerns > 0 {
				seedImplementReviewedAudit(fb, runID, implementStageID, tc.seedConcerns)
			}
			r := newResolver(srv, nil)

			runState := tc.runState
			if runState == "" {
				runState = "running"
			}
			// Build the authoritative store block (#3043) unless the case models
			// an UNAVAILABLE block (nil -> audit fallback). OpenImplement defaults
			// to seedConcerns so the existing budget/ceiling cases keep
			// wantConcerns == seedConcerns via the store path.
			var store *RunConcerns
			if !tc.storeAbsent {
				openImplement := tc.seedConcerns
				if tc.storeOpenImplement != nil {
					openImplement = *tc.storeOpenImplement
				}
				open := tc.seedConcerns
				if tc.storeOpen != 0 {
					open = tc.storeOpen
				}
				store = &RunConcerns{
					Open:          open,
					OpenImplement: &openImplement,
					Items:         make([]RunConcernItem, tc.storeItems),
				}
			}
			hint, err := r.reviewActionHintFor(context.Background(), runID, implementStageID, runState, tc.status, store)
			if err != nil {
				t.Fatalf("reviewActionHintFor: %v", err)
			}
			if tc.wantNil {
				if hint != nil {
					t.Fatalf("hint = %+v, want nil", hint)
				}
				return
			}
			if hint == nil {
				t.Fatalf("hint = nil, want a populated hint")
			}
			if tc.wantSource != "" && hint.Source != tc.wantSource {
				t.Errorf("Source = %q, want %q", hint.Source, tc.wantSource)
			}
			if hint.Concerns != tc.wantConcerns {
				t.Errorf("Concerns = %d, want %d", hint.Concerns, tc.wantConcerns)
			}
			if hint.RemainingFixupBudget != tc.wantRemaining {
				t.Errorf("RemainingFixupBudget = %d, want %d", hint.RemainingFixupBudget, tc.wantRemaining)
			}
			if hint.OverrideAvailable != tc.wantOverride {
				t.Errorf("OverrideAvailable = %v, want %v", hint.OverrideAvailable, tc.wantOverride)
			}
			if !strings.Contains(hint.Message, "fishhawk_fixup_stage") && !strings.Contains(hint.Message, "fresh run") {
				t.Errorf("Message should reference fishhawk_fixup_stage or a fresh run; got %q", hint.Message)
			}
			// #964: when the hint points at a fix-up it must steer the
			// operator at stable concern_ids (the primary addressing
			// scheme), never the deprecated positional indices.
			if strings.Contains(hint.Message, "fishhawk_fixup_stage") {
				if !strings.Contains(hint.Message, "concern_ids") {
					t.Errorf("Message should point at concern_ids addressing; got %q", hint.Message)
				}
				if strings.Contains(hint.Message, "concern indices") {
					t.Errorf("Message still points at deprecated positional indices; got %q", hint.Message)
				}
			}
			// #1097: the hard-ceiling Message must surface the commit-and-vouch
			// remedy for a late CI/SAST finding.
			if tc.wantMessageContains != "" && !strings.Contains(hint.Message, tc.wantMessageContains) {
				t.Errorf("Message should contain %q; got %q", tc.wantMessageContains, hint.Message)
			}
		})
	}
}

// TestReviewActionHintFor_LatestRoundOnly proves the AUDIT-FALLBACK count is
// scoped to the latest review round: a first round with 2 concerns, then a
// fix-up, then a second round with 1 concern must surface 1 — not 3 (#860).
// Exercised with a nil store block so the degraded audit path (the only path
// that round-scopes) is under test (#3043).
func TestReviewActionHintFor_LatestRoundOnly(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	stageID := uuid.New()

	// Round 1: 2 concerns. Then a fix-up pass. Then round 2: 1 concern.
	seedImplementReviewedAudit(fb, runID, stageID, 2)
	seedFixupTriggeredAudit(fb, runID, stageID)
	seedImplementReviewedAudit(fb, runID, stageID, 1)

	r := newResolver(srv, nil)
	// nil store -> the audit fallback, which is what round-scopes.
	hint, err := r.reviewActionHintFor(context.Background(), runID, stageID, "running", completeStatus(), nil)
	if err != nil {
		t.Fatalf("reviewActionHintFor: %v", err)
	}
	if hint == nil {
		t.Fatal("hint = nil, want a populated hint")
	}
	if hint.Source != hintSourceStoreUnavailable {
		t.Errorf("Source = %q, want audit_fallback_store_unavailable (nil store block)", hint.Source)
	}
	if hint.Concerns != 1 {
		t.Errorf("Concerns = %d, want 1 (latest round only, not summed across rounds)", hint.Concerns)
	}
	// One fix-up pass spent the normal budget; below the ceiling -> override.
	if !hint.OverrideAvailable {
		t.Errorf("OverrideAvailable = false, want true (budget spent, below ceiling)")
	}
}

// TestReviewActionHintFor_TruncatedMixedStage is condition 1's must-land
// uncovered case (#3043): a TRUNCATED, MIXED-STAGE transported block — more
// implied open concerns than the retained items, spanning plan and implement —
// must report the true implement-stage open count from the authoritative
// OpenImplement, NOT the retained-item count. A count re-derived from the
// (bounded) Items list would be the same defect this issue exists to fix.
func TestReviewActionHintFor_TruncatedMixedStage(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	stageID := uuid.New()

	// The audit carries a large sum; it must be ignored on the store path.
	seedImplementReviewedAudit(fb, runID, stageID, 9)

	// The transported block: 5 open concerns total (Open), of which 3 are
	// implement-stage (OpenImplement, the authority). Items is TRUNCATED to a
	// mixed subset of 2 (one plan, one implement) plus a terminal-looking one —
	// far fewer than either count, and not all implement.
	openImplement := 3
	store := &RunConcerns{
		Open:          5,
		OpenImplement: &openImplement,
		Items: []RunConcernItem{
			{ID: uuid.NewString(), StageKind: "implement", State: "raised"},
			{ID: uuid.NewString(), StageKind: "plan", State: "raised"},
		},
	}

	r := newResolver(srv, nil)
	hint, err := r.reviewActionHintFor(context.Background(), runID, stageID, "running", completeStatus(), store)
	if err != nil {
		t.Fatalf("reviewActionHintFor: %v", err)
	}
	if hint == nil {
		t.Fatal("hint = nil, want a populated hint")
	}
	if hint.Source != hintSourceStore {
		t.Errorf("Source = %q, want store", hint.Source)
	}
	if hint.Concerns != 3 {
		t.Errorf("Concerns = %d, want 3 (the authoritative implement-stage open count, NOT the %d retained items nor the %d audit sum)",
			hint.Concerns, len(store.Items), 9)
	}
}

// TestReviewActionHintFor_LegacyPeerNoScalar is condition 1/2's must-land
// mixed-version case (#3043): a PRESENT concerns block from a backend peer that
// PREDATES the open_implement field — carrying open implement items but NO
// open_implement key — must decode OpenImplement to nil and DEGRADE to the audit
// fallback under the DISTINCT audit_fallback_legacy_peer marker, never suppress.
// The block is decoded from a LEGACY JSON body (not a hand-built struct) so the
// realistic wire shape is what is asserted; a plain-int OpenImplement would
// decode the absent key to an authoritative 0 and suppress the hint — the very
// defect the pointer closes. The message must NOT claim the store was
// unavailable (it was not — the read succeeded).
func TestReviewActionHintFor_LegacyPeerNoScalar(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	stageID := uuid.New()

	// The audit is the fallback source once the scalar is absent.
	seedImplementReviewedAudit(fb, runID, stageID, 2)

	// A legacy wire body: a present concerns block with open implement items and
	// NO open_implement key at all.
	legacy := `{"open":2,"by_state":{"raised":2},"items":[` +
		`{"id":"` + uuid.NewString() + `","stage_kind":"implement","severity":"medium","category":"scope","state":"raised"},` +
		`{"id":"` + uuid.NewString() + `","stage_kind":"implement","severity":"low","category":"style","state":"raised"}]}`
	var store RunConcerns
	if err := json.Unmarshal([]byte(legacy), &store); err != nil {
		t.Fatalf("unmarshal legacy concerns body: %v", err)
	}
	if store.OpenImplement != nil {
		t.Fatalf("legacy body decoded OpenImplement = %v, want nil (absent key)", *store.OpenImplement)
	}

	r := newResolver(srv, nil)
	hint, err := r.reviewActionHintFor(context.Background(), runID, stageID, "running", completeStatus(), &store)
	if err != nil {
		t.Fatalf("reviewActionHintFor: %v", err)
	}
	if hint == nil {
		t.Fatal("hint = nil, want the audit fallback to FIRE (never suppress on a legacy peer)")
	}
	if hint.Source != hintSourceLegacyPeer {
		t.Errorf("Source = %q, want audit_fallback_legacy_peer", hint.Source)
	}
	if hint.Concerns != 2 {
		t.Errorf("Concerns = %d, want 2 (the audit fallback count)", hint.Concerns)
	}
	// The wording names what is actually missing and does NOT falsely claim the
	// store was unavailable.
	if !strings.Contains(hint.Message, "predates the authoritative implement-stage concern count") {
		t.Errorf("legacy-peer message should name the missing authoritative count; got %q", hint.Message)
	}
	if strings.Contains(hint.Message, "the concern store was unavailable") {
		t.Errorf("legacy-peer message must NOT claim the store was unavailable (the read succeeded); got %q", hint.Message)
	}
}

// TestFixupInfraRefunds_OneRefundPerTriggerWindow pins the #1987 round-2
// review concern (761fb56d): fixupInfraRefunds must count AT MOST ONE refund
// per trigger window, even when multiple category-C death signals land inside
// the SAME window. Two reaper-death signals are seeded, both sequenced inside
// the first trigger window (the second window is empty); without the inner
// break at review_action_hint.go:520 every signal in a window would count,
// yielding 2 refunds for one fix-up pass instead of 1.
func TestFixupInfraRefunds_OneRefundPerTriggerWindow(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	stageID := uuid.New()

	// Both signals land at sequences 1 and 2 (insertion order); the trigger
	// window [0, 3) contains both, and the second window [3, +inf) is empty.
	seedDispatchReaperFailedAudit(fb, runID, stageID, "C")
	seedDispatchReaperFailedAudit(fb, runID, stageID, "C")

	r := newResolver(srv, nil)
	refunds, err := r.fixupInfraRefunds(context.Background(), runID, stageID, []int64{0, 3})
	if err != nil {
		t.Fatalf("fixupInfraRefunds: %v", err)
	}
	if refunds != 1 {
		t.Errorf("refunds = %d, want 1 (at most one refund per trigger window despite two in-window signals)", refunds)
	}
}

// TestImplementReviewMergeHint covers the #947 local-loop parity hint: a
// display-only merge-readiness warning surfaced ONLY while the implement-stage
// agent review is pending (dispatched, no verdict). It mirrors the backend's
// review-pending presence gate; once the review reaches any terminal status
// the hint is empty (the required fishhawk_audit_complete check flips green).
func TestImplementReviewMergeHint(t *testing.T) {
	tests := []struct {
		name     string
		status   *ReviewStatus
		wantHint bool
	}{
		{"nil status -> no hint", nil, false},
		{"none -> no hint", &ReviewStatus{Stage: "implement", Status: "none"}, false},
		{"pending -> hint", &ReviewStatus{Stage: "implement", Status: "pending"}, true},
		{"complete -> no hint", &ReviewStatus{Stage: "implement", Status: "complete"}, false},
		{"skipped -> no hint", &ReviewStatus{Stage: "implement", Status: "skipped"}, false},
		{"failed -> no hint", &ReviewStatus{Stage: "implement", Status: "failed"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := implementReviewMergeHint(tc.status)
			if tc.wantHint {
				if got == "" {
					t.Fatalf("expected a merge-readiness hint, got empty")
				}
				if !strings.Contains(got, "not") || !strings.Contains(got, "fishhawk_audit_complete") {
					t.Errorf("hint should warn the PR is not safe to merge and name the held check: %q", got)
				}
			} else if got != "" {
				t.Errorf("expected no hint for status %v, got %q", tc.status, got)
			}
		})
	}
}

// TestReviewActionHint_SuggestedActions pins the hint → next_actions
// translation (#1024): the concern-arm entries derive FROM the computed
// hint value, so each budget branch maps to a fixed action set and the
// two surfaces cannot disagree on the remaining budget.
func TestReviewActionHint_SuggestedActions(t *testing.T) {
	run := &Run{ID: uuid.NewString(), State: "running"}
	stageID := uuid.NewString()

	t.Run("below budget -> fixup, defer, merge; fixup consumes fixup_budget", func(t *testing.T) {
		h := &ReviewActionHint{Concerns: 2, RemainingFixupBudget: 1}
		actions := h.suggestedActions(run, stageID)
		if len(actions) != 3 ||
			actions[0].Action != "fishhawk_fixup_stage" ||
			actions[1].Action != "fishhawk_defer_concern" ||
			actions[2].Action != "merge_and_file_follow_up" {
			t.Fatalf("actions = %+v, want [fishhawk_fixup_stage fishhawk_defer_concern merge_and_file_follow_up]", actions)
		}
		if actions[0].Consumes != consumesFixupBudget {
			t.Errorf("fixup consumes = %q, want fixup_budget", actions[0].Consumes)
		}
		if actions[0].Params["stage_id"] != stageID {
			t.Errorf("fixup params.stage_id = %q, want %s", actions[0].Params["stage_id"], stageID)
		}
		// defer is always legal while a concern is open and consumes nothing.
		if actions[1].Consumes != consumesNone {
			t.Errorf("defer consumes = %q, want none", actions[1].Consumes)
		}
		if actions[1].Params["concern_ids"] != "run.concerns.items[].id" {
			t.Errorf("defer params.concern_ids = %q, want the items source", actions[1].Params["concern_ids"])
		}
		// The remaining-budget number rides on the reason — the figure the
		// integration test cross-checks against the hint itself.
		if !strings.Contains(actions[0].Reason, "1 normal fix-up pass") {
			t.Errorf("fixup reason should carry the remaining budget; got %q", actions[0].Reason)
		}
		if _, forced := actions[0].Params["force_additional_pass"]; forced {
			t.Error("below-budget fixup action must not carry force_additional_pass")
		}
		// #1549: the fix-up precondition must NOT tell the operator to check out
		// the run branch (that CAUSES the worktree-conflict failure) and MUST
		// name the runner's lineage worktree instead.
		if actions[0].Precondition == "" {
			t.Error("fixup precondition must be non-empty")
		}
		if strings.Contains(actions[0].Precondition, "checkout the run branch") {
			t.Errorf("fixup precondition still says to checkout the run branch: %q", actions[0].Precondition)
		}
		if !strings.Contains(actions[0].Precondition, "lineage worktree") {
			t.Errorf("fixup precondition should name the lineage worktree; got %q", actions[0].Precondition)
		}
	})

	t.Run("budget spent, override available -> merge, defer, forced fixup", func(t *testing.T) {
		h := &ReviewActionHint{Concerns: 1, RemainingFixupBudget: 0, OverrideAvailable: true}
		actions := h.suggestedActions(run, stageID)
		if len(actions) != 3 ||
			actions[0].Action != "merge_and_file_follow_up" ||
			actions[1].Action != "fishhawk_defer_concern" ||
			actions[2].Action != "fishhawk_fixup_stage" {
			t.Fatalf("actions = %+v, want [merge_and_file_follow_up fishhawk_defer_concern fishhawk_fixup_stage]", actions)
		}
		if actions[1].Consumes != consumesNone {
			t.Errorf("defer consumes = %q, want none", actions[1].Consumes)
		}
		if actions[2].Params["force_additional_pass"] != "true" {
			t.Errorf("override fixup params = %v, want force_additional_pass=true", actions[2].Params)
		}
	})

	t.Run("ceiling reached -> merge-with-follow-up, commit-and-vouch, or fresh run", func(t *testing.T) {
		h := &ReviewActionHint{Concerns: 1, RemainingFixupBudget: 0, OverrideAvailable: false}
		actions := h.suggestedActions(run, stageID)
		if len(actions) != 3 ||
			actions[0].Action != "merge_and_file_follow_up" ||
			actions[1].Action != "commit_and_vouch" ||
			actions[2].Action != "fishhawk_start_run" {
			t.Fatalf("actions = %+v, want [merge_and_file_follow_up commit_and_vouch fishhawk_start_run]", actions)
		}
		// #1097: commit-and-vouch is the in-loop remedy for a late CI/SAST
		// finding at the ceiling — it consumes NO fix-up budget and steers the
		// operator at fishhawk_vouch_commit.
		if actions[1].Consumes != consumesNone {
			t.Errorf("commit_and_vouch consumes = %q, want none", actions[1].Consumes)
		}
		if !strings.Contains(actions[1].Reason, "fishhawk_vouch_commit") {
			t.Errorf("commit_and_vouch reason should name fishhawk_vouch_commit; got %q", actions[1].Reason)
		}
		if actions[2].Consumes != consumesNewRun {
			t.Errorf("fresh-run consumes = %q, want new_run", actions[2].Consumes)
		}
	})
}
