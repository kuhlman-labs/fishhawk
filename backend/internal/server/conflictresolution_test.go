package server

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
)

// makeConflictResolutionEntry builds a stage_conflict_resolution_triggered
// entry with the given payload fields, bound to the given run and stage.
func makeConflictResolutionEntry(runID, stageID uuid.UUID, fields map[string]any) *audit.Entry {
	payload, _ := json.Marshal(fields)
	rid := runID
	sid := stageID
	return &audit.Entry{
		ID:       uuid.New(),
		Category: CategoryStageConflictResolutionTriggered,
		RunID:    &rid,
		StageID:  &sid,
		Payload:  payload,
	}
}

func conflictTriggerFields(branch, baseRef, head string) map[string]any {
	return map[string]any{"branch": branch, "base_ref": baseRef, "expected_head_sha": head, "pass": 1}
}

// TestResolveConflictResolutionTrigger covers the audit-payload reader
// (E64.62 / #3202) one case per branch: no repo, no entries, a wrong-stage
// entry, a list error, a malformed payload, a HALF-POPULATED payload, the happy
// path, and newest-wins.
//
// The half-populated case is the one that is easy to get wrong: serving a
// trigger with no base ref would hand the runner nothing to merge, and the pass
// would then refuse with the no-conflict reason — a refusal naming the wrong
// cause. Treating it as ABSENT is what keeps the failure honest.
func TestResolveConflictResolutionTrigger(t *testing.T) {
	runID := uuid.New()
	stageID := uuid.New()
	const branch = "fishhawk/run-1a5c4201/stage-4ecee120"
	const head = "0123456789abcdef0123456789abcdef01234567"

	t.Run("no audit repo returns absent", func(t *testing.T) {
		s := New(Config{Addr: "127.0.0.1:0"})
		if _, ok := s.resolveConflictResolutionTrigger(context.Background(), runID, stageID); ok {
			t.Error("ok = true with no AuditRepo configured")
		}
	})

	t.Run("no entries returns absent", func(t *testing.T) {
		s := New(Config{Addr: "127.0.0.1:0", AuditRepo: &feedbackAuditRepo{}})
		if _, ok := s.resolveConflictResolutionTrigger(context.Background(), runID, stageID); ok {
			t.Error("ok = true with no trigger entries")
		}
	})

	t.Run("list error returns absent", func(t *testing.T) {
		s := New(Config{Addr: "127.0.0.1:0", AuditRepo: &feedbackAuditRepo{listErr: errors.New("boom")}})
		if _, ok := s.resolveConflictResolutionTrigger(context.Background(), runID, stageID); ok {
			t.Error("ok = true despite a list failure; the resolver must degrade, not serve a guess")
		}
	})

	t.Run("entry for a different stage is ignored", func(t *testing.T) {
		s := New(Config{Addr: "127.0.0.1:0", AuditRepo: &feedbackAuditRepo{
			byRunID: map[uuid.UUID][]*audit.Entry{runID: {
				makeConflictResolutionEntry(runID, uuid.New(), conflictTriggerFields(branch, "main", head)),
			}},
		}})
		if _, ok := s.resolveConflictResolutionTrigger(context.Background(), runID, stageID); ok {
			t.Error("ok = true for an entry bound to a different stage")
		}
	})

	t.Run("malformed payload is skipped", func(t *testing.T) {
		bad := makeConflictResolutionEntry(runID, stageID, nil)
		bad.Payload = []byte(`{"branch": 17}`)
		s := New(Config{Addr: "127.0.0.1:0", AuditRepo: &feedbackAuditRepo{
			byRunID: map[uuid.UUID][]*audit.Entry{runID: {bad}},
		}})
		if _, ok := s.resolveConflictResolutionTrigger(context.Background(), runID, stageID); ok {
			t.Error("ok = true for an undecodable payload")
		}
	})

	t.Run("half-populated payload is treated as absent", func(t *testing.T) {
		for name, fields := range map[string]map[string]any{
			"no branch":   conflictTriggerFields("", "main", head),
			"no base ref": conflictTriggerFields(branch, "", head),
		} {
			s := New(Config{Addr: "127.0.0.1:0", AuditRepo: &feedbackAuditRepo{
				byRunID: map[uuid.UUID][]*audit.Entry{runID: {makeConflictResolutionEntry(runID, stageID, fields)}},
			}})
			if _, ok := s.resolveConflictResolutionTrigger(context.Background(), runID, stageID); ok {
				t.Errorf("%s: ok = true for a half-populated trigger", name)
			}
		}
	})

	t.Run("happy path returns the payload verbatim", func(t *testing.T) {
		s := New(Config{Addr: "127.0.0.1:0", AuditRepo: &feedbackAuditRepo{
			byRunID: map[uuid.UUID][]*audit.Entry{runID: {
				makeConflictResolutionEntry(runID, stageID, conflictTriggerFields(branch, "main", head)),
			}},
		}})
		got, ok := s.resolveConflictResolutionTrigger(context.Background(), runID, stageID)
		if !ok {
			t.Fatal("ok = false for a fully-populated trigger")
		}
		if got.Branch != branch || got.BaseRef != "main" || got.ExpectedHeadSHA != head || got.Pass != 1 {
			t.Errorf("trigger = %+v, want the payload verbatim", got)
		}
	})

	t.Run("newest entry wins", func(t *testing.T) {
		// ListForRunByCategory returns entries ASC by ts, so the LAST element
		// is the newest. A trigger re-opens the stage to pending, so the
		// prompt must reflect the most recent authorization.
		s := New(Config{Addr: "127.0.0.1:0", AuditRepo: &feedbackAuditRepo{
			byRunID: map[uuid.UUID][]*audit.Entry{runID: {
				makeConflictResolutionEntry(runID, stageID, conflictTriggerFields(branch, "old-base", "aaaa")),
				makeConflictResolutionEntry(runID, stageID, conflictTriggerFields(branch, "main", head)),
			}},
		}})
		got, ok := s.resolveConflictResolutionTrigger(context.Background(), runID, stageID)
		if !ok {
			t.Fatal("ok = false")
		}
		if got.BaseRef != "main" || got.ExpectedHeadSHA != head {
			t.Errorf("trigger = %+v, want the NEWEST entry's payload", got)
		}
	})
}
