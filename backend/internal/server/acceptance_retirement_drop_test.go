package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// dedupedAuditFake is the ONLY server-side audit fake carrying the
// audit.DedupedChainAppender capability (#3439). It wraps the shared auditFake
// and implements AppendChainedDeduped with a REAL in-memory scan over the
// appended rows keyed on (stage_id, payload `reason`), returning
// *audit.DedupedDuplicateError exactly as the Postgres core does. Because no
// other fake carries the capability, every pre-existing test keeps exercising
// appendRetirementDropOnce's fallback leg; the tests in this file are the
// capability-path pins.
//
// The fake's scan is NOT atomic (no lock) — the atomicity proof lives in the
// real-Postgres tests (acceptance_retirement_drop_pg_test.go and
// audit/deduped_test.go). This fake pins the server's BRANCHING on the
// capability: duplicate → (false, nil) with no row and no refresh; first
// append → (true, nil); an append error → (false, err).
type dedupedAuditFake struct {
	*auditFake
	// dedupedCalls counts AppendChainedDeduped invocations, so a test can
	// prove the capability path was taken (and the fallback's
	// ListForRunByCategory was not).
	dedupedCalls int
}

func newDedupedAuditFake() *dedupedAuditFake {
	return &dedupedAuditFake{auditFake: newAuditFake()}
}

func (d *dedupedAuditFake) AppendChainedDeduped(ctx context.Context, p audit.ChainAppendParams, spec audit.DedupeSpec) (*audit.Entry, error) {
	d.mu.Lock()
	d.dedupedCalls++
	for i := range d.appended {
		ap := d.appended[i]
		if ap.Category != p.Category || ap.RunID != p.RunID {
			continue
		}
		if spec.StageID != nil && (ap.StageID == nil || *ap.StageID != *spec.StageID) {
			continue
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(ap.Payload, &fields); err != nil {
			continue
		}
		var v *string
		if raw, ok := fields[spec.PayloadKey]; !ok || json.Unmarshal(raw, &v) != nil || v == nil || *v != spec.PayloadValue {
			continue
		}
		rid := ap.RunID
		d.mu.Unlock()
		return nil, &audit.DedupedDuplicateError{Existing: &audit.Entry{
			ID: uuid.New(), RunID: &rid, StageID: ap.StageID, Category: ap.Category, Sequence: int64(i + 1), Payload: ap.Payload,
		}}
	}
	d.mu.Unlock()
	return d.AppendChained(ctx, p)
}

// Compile-time: the fake carries the capability the helper type-asserts.
var _ audit.DedupedChainAppender = (*dedupedAuditFake)(nil)

// dropOnceParams builds the ChainAppendParams a retirement-drop writer hands
// the helper, with reason in the payload.
func dropOnceParams(runID, stageID uuid.UUID, reason string) audit.ChainAppendParams {
	actorKind := audit.ActorSystem
	payload, _ := json.Marshal(map[string]any{
		"run_id": runID.String(), "stage_id": stageID.String(), "retired": []retiredScenarioEntry{},
		"scenario_ids": []string{}, "reason": reason,
	})
	return audit.ChainAppendParams{
		RunID: runID, StageID: &stageID, Timestamp: time.Now().UTC(),
		Category: CategoryAcceptanceScenarioRetirementDropped, ActorKind: &actorKind, Payload: payload,
	}
}

// newDropOnceServer builds a Server over the given audit repo with a DEBUG
// JSON logger captured in the returned buffer.
func newDropOnceServer(t *testing.T, au audit.Repository) (*Server, *bytes.Buffer) {
	t.Helper()
	s := New(Config{Addr: "127.0.0.1:0", AuditRepo: au, RunRepo: newOrchestratorRepo(), APITokenRepo: stubToken("write:runs")})
	logBuf := &bytes.Buffer{}
	s.cfg.Logger = slog.New(slog.NewJSONHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return s, logBuf
}

// TestAppendRetirementDropOnce_CapabilityPath pins the helper's three
// capability-path branches over the deduped fake. Counterfactual: replacing
// the errors.As duplicate branch with a plain error return makes the
// "duplicate" case fail on err != nil; deleting the type-assert makes
// dedupedCalls stay 0.
func TestAppendRetirementDropOnce_CapabilityPath(t *testing.T) {
	t.Run("first append lands", func(t *testing.T) {
		au := newDedupedAuditFake()
		s, logBuf := newDropOnceServer(t, au)
		runID, stageID := uuid.New(), uuid.New()
		appended, err := s.appendRetirementDropOnce(context.Background(), runID, stageID, "persist_failed", "test", dropOnceParams(runID, stageID, "persist_failed"))
		if err != nil || !appended {
			t.Fatalf("(appended=%v, err=%v), want (true, nil)", appended, err)
		}
		if n := len(appendedOfCategory(au.auditFake, CategoryAcceptanceScenarioRetirementDropped)); n != 1 {
			t.Errorf("rows = %d, want 1", n)
		}
		if au.dedupedCalls != 1 {
			t.Errorf("AppendChainedDeduped calls = %d, want 1 (the capability path must be taken)", au.dedupedCalls)
		}
		if strings.Contains(logBuf.String(), "does not implement DedupedChainAppender") {
			t.Errorf("capability path must not log the fallback line:\n%s", logBuf.String())
		}
	})
	t.Run("duplicate is (false, nil) with no row", func(t *testing.T) {
		au := newDedupedAuditFake()
		s, logBuf := newDropOnceServer(t, au)
		runID, stageID := uuid.New(), uuid.New()
		if _, err := s.appendRetirementDropOnce(context.Background(), runID, stageID, "persist_failed", "test", dropOnceParams(runID, stageID, "persist_failed")); err != nil {
			t.Fatalf("seed: %v", err)
		}
		appended, err := s.appendRetirementDropOnce(context.Background(), runID, stageID, "persist_failed", "test", dropOnceParams(runID, stageID, "persist_failed"))
		if err != nil || appended {
			t.Fatalf("(appended=%v, err=%v), want (false, nil) on the duplicate", appended, err)
		}
		if n := len(appendedOfCategory(au.auditFake, CategoryAcceptanceScenarioRetirementDropped)); n != 1 {
			t.Errorf("rows = %d, want 1 (nothing new on the duplicate branch)", n)
		}
		if !strings.Contains(logBuf.String(), "already recorded; duplicate append rejected under the run-row lock") {
			t.Errorf("duplicate must be DEBUG-logged naming the surviving sequence:\n%s", logBuf.String())
		}
		// A different reason on the same stage is NOT a duplicate.
		appended, err = s.appendRetirementDropOnce(context.Background(), runID, stageID, "no_run_branch", "test", dropOnceParams(runID, stageID, "no_run_branch"))
		if err != nil || !appended {
			t.Fatalf("other reason: (appended=%v, err=%v), want (true, nil)", appended, err)
		}
	})
	t.Run("append error propagates and no row lands", func(t *testing.T) {
		au := newDedupedAuditFake()
		au.appendErrCategory = CategoryAcceptanceScenarioRetirementDropped
		s, _ := newDropOnceServer(t, au)
		runID, stageID := uuid.New(), uuid.New()
		appended, err := s.appendRetirementDropOnce(context.Background(), runID, stageID, "persist_failed", "test", dropOnceParams(runID, stageID, "persist_failed"))
		if err == nil || appended {
			t.Fatalf("(appended=%v, err=%v), want (false, non-nil)", appended, err)
		}
		var dup *audit.DedupedDuplicateError
		if errors.As(err, &dup) {
			t.Errorf("an append error must NOT be reported as the duplicate branch: %v", err)
		}
		if n := len(appendedOfCategory(au.auditFake, CategoryAcceptanceScenarioRetirementDropped)); n != 0 {
			t.Errorf("rows = %d, want 0", n)
		}
	})
}

// TestAppendRetirementDropOnce_FallbackLeg pins the non-capable leg over the
// plain auditFake: a list error is WARN 'proceeding without idempotency guard'
// + the row lands (the #3389 posture, byte-preserved), a duplicate is (false,
// nil), and the fallback DEBUG line is present. Counterfactual: deleting the
// retirementDropAlreadyRecorded check makes the "duplicate" case append twice.
func TestAppendRetirementDropOnce_FallbackLeg(t *testing.T) {
	t.Run("list error is WARN + proceed", func(t *testing.T) {
		au := newAuditFake()
		au.listByCategoryErrCategory = CategoryAcceptanceScenarioRetirementDropped
		s, logBuf := newDropOnceServer(t, au)
		runID, stageID := uuid.New(), uuid.New()
		appended, err := s.appendRetirementDropOnce(context.Background(), runID, stageID, "persist_failed", "fallback test", dropOnceParams(runID, stageID, "persist_failed"))
		if err != nil || !appended {
			t.Fatalf("(appended=%v, err=%v), want (true, nil)", appended, err)
		}
		if n := len(appendedOfCategory(au, CategoryAcceptanceScenarioRetirementDropped)); n != 1 {
			t.Errorf("rows = %d, want 1 (list error is WARN + proceed)", n)
		}
		if !strings.Contains(logBuf.String(), "fallback test: list audit entries failed; proceeding without idempotency guard") {
			t.Errorf("list failure must be WARN-logged with the writer prefix:\n%s", logBuf.String())
		}
		if !strings.Contains(logBuf.String(), "does not implement DedupedChainAppender; falling back to the non-atomic list-then-append") {
			t.Errorf("the non-atomic leg must be DEBUG-logged:\n%s", logBuf.String())
		}
	})
	t.Run("duplicate is (false, nil)", func(t *testing.T) {
		au := newAuditFake()
		s, _ := newDropOnceServer(t, au)
		runID, stageID := uuid.New(), uuid.New()
		if _, err := s.appendRetirementDropOnce(context.Background(), runID, stageID, "persist_failed", "fallback test", dropOnceParams(runID, stageID, "persist_failed")); err != nil {
			t.Fatalf("seed: %v", err)
		}
		appended, err := s.appendRetirementDropOnce(context.Background(), runID, stageID, "persist_failed", "fallback test", dropOnceParams(runID, stageID, "persist_failed"))
		if err != nil || appended {
			t.Fatalf("(appended=%v, err=%v), want (false, nil)", appended, err)
		}
		if n := len(appendedOfCategory(au, CategoryAcceptanceScenarioRetirementDropped)); n != 1 {
			t.Errorf("rows = %d, want 1", n)
		}
	})
	t.Run("append error propagates", func(t *testing.T) {
		au := newAuditFake()
		au.appendErrCategory = CategoryAcceptanceScenarioRetirementDropped
		s, _ := newDropOnceServer(t, au)
		runID, stageID := uuid.New(), uuid.New()
		appended, err := s.appendRetirementDropOnce(context.Background(), runID, stageID, "persist_failed", "fallback test", dropOnceParams(runID, stageID, "persist_failed"))
		if err == nil || appended {
			t.Fatalf("(appended=%v, err=%v), want (false, non-nil)", appended, err)
		}
	})
}

// newCancelDropSeamDeduped is newCancelDropSeam rebuilt over the deduped fake,
// so the cancel writer runs the capability path end to end.
func newCancelDropSeamDeduped(t *testing.T) (*cancelDropSeam, *dedupedAuditFake) {
	t.Helper()
	c := newCancelDropSeam(t, run.StageStatePending, true, cancelDropRetired)
	dau := &dedupedAuditFake{auditFake: c.au}
	c.s.cfg.AuditRepo = dau
	return c, dau
}

// TestRecordRetirementsDroppedOnCancel_CapabilityPath_Idempotent: the
// cancel-sink writer over the deduped fake — three invocations with three
// cancel sources → 1 row, 1 refresh, the DEBUG duplicate line present, and
// every append went through AppendChainedDeduped (never the fallback).
// Counterfactual: deleting the `if !appended { return }` in the writer makes
// the refresh count 3.
func TestRecordRetirementsDroppedOnCancel_CapabilityPath_Idempotent(t *testing.T) {
	c, dau := newCancelDropSeamDeduped(t)
	c.s.recordAcceptanceRetirementsDroppedOnCancel(context.Background(), c.runID, cancelSourceOperator)
	c.s.recordAcceptanceRetirementsDroppedOnCancel(context.Background(), c.runID, cancelSourceRunBudget)
	c.s.recordAcceptanceRetirementsDroppedOnCancel(context.Background(), c.runID, cancelSourceStageCancelled)
	if n := len(c.dropRows()); n != 1 {
		t.Fatalf("rows after three invocations = %d, want 1 (idempotent per stage+reason, NOT per cancel_source)", n)
	}
	if n := len(c.rec.status); n != 1 {
		t.Errorf("status refreshes = %d, want 1 (a duplicate must not refresh)", n)
	}
	if dau.dedupedCalls != 3 {
		t.Errorf("AppendChainedDeduped calls = %d, want 3 (every invocation takes the capability path)", dau.dedupedCalls)
	}
	if n := strings.Count(c.logBuf.String(), "already recorded; duplicate append rejected under the run-row lock"); n != 2 {
		t.Errorf("duplicate DEBUG lines = %d, want 2:\n%s", n, c.logBuf.String())
	}
	if strings.Contains(c.logBuf.String(), "proceeding without idempotency guard") {
		t.Errorf("capability path must never take the fallback:\n%s", c.logBuf.String())
	}
}

// TestRecordAcceptanceRetirementsUnserved_CapabilityPath_Duplicate is the
// third writer's direct dedupe pin (operator condition 1): over the deduped
// fake, a second recordAcceptanceRetirementsUnserved for the same stage is a
// duplicate → no new row, no status refresh (the writer never refreshes), no
// WARN, and both calls went through the capability path.
func TestRecordAcceptanceRetirementsUnserved_CapabilityPath_Duplicate(t *testing.T) {
	au := newDedupedAuditFake()
	s, logBuf := newDropOnceServer(t, au)
	rec := &pageClassRecorder{}
	s.issueNotifier = rec
	runID, stageID := uuid.New(), uuid.New()
	readErr := errors.New("approval chain unreadable")
	s.recordAcceptanceRetirementsUnserved(context.Background(), runID, stageID, readErr)
	s.recordAcceptanceRetirementsUnserved(context.Background(), runID, stageID, readErr)
	rows := appendedOfCategory(au.auditFake, CategoryAcceptanceScenarioRetirementDropped)
	if len(rows) != 1 {
		t.Fatalf("rows after two invocations = %d, want 1", len(rows))
	}
	if rows[0].StageID == nil || *rows[0].StageID != stageID || !strings.Contains(string(rows[0].Payload), `"reason":"approval_chain_unreadable"`) {
		t.Errorf("row = %+v / %s, want the stage + reason approval_chain_unreadable", rows[0].StageID, rows[0].Payload)
	}
	if n := len(rec.status); n != 0 {
		t.Errorf("status refreshes = %d, want 0 (the unserved writer never refreshes)", n)
	}
	if au.dedupedCalls != 2 {
		t.Errorf("AppendChainedDeduped calls = %d, want 2", au.dedupedCalls)
	}
	if strings.Contains(logBuf.String(), `"level":"WARN"`) {
		t.Errorf("a duplicate is not a WARN:\n%s", logBuf.String())
	}
	if !strings.Contains(logBuf.String(), "prompt: acceptance retirements-unserved drop: acceptance retirement drop already recorded") {
		t.Errorf("duplicate must be DEBUG-logged with the writer prefix:\n%s", logBuf.String())
	}
}
