package server

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
)

// seedAuditEntry pushes a pre-existing entry into the auditFake's history so
// ListForRunByCategory returns it — used to stand up the "entry already
// present" branch of ensureGovernanceAuditEntry.
func seedAuditEntry(au *auditFake, runID uuid.UUID, category, artifactID string) {
	payload, _ := json.Marshal(map[string]any{"artifact_id": artifactID})
	au.seeded = append(au.seeded, &audit.Entry{
		RunID:    &runID,
		Category: category,
		Payload:  payload,
	})
}

// TestEnsureGovernanceAuditEntry_AlreadyPresent: an entry whose payload
// carries the matching artifact_id is detected, so the helper is a no-op
// (healed=false) and the append closure is NOT invoked.
func TestEnsureGovernanceAuditEntry_AlreadyPresent(t *testing.T) {
	runID := uuid.New()
	artifactID := uuid.New().String()
	au := newAuditFake()
	seedAuditEntry(au, runID, "pull_request_opened", artifactID)
	s := New(Config{Addr: "127.0.0.1:0", AuditRepo: au})

	invoked := false
	healed, err := s.ensureGovernanceAuditEntry(context.Background(), runID,
		"pull_request_opened", artifactID, func() error { invoked = true; return nil })
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if healed {
		t.Error("healed = true, want false (entry already present)")
	}
	if invoked {
		t.Error("appendEntry was invoked, want NOT invoked (entry already present)")
	}
}

// TestEnsureGovernanceAuditEntry_Missing: no entry carries the artifact_id, so
// the helper invokes the append closure and reports healed=true.
func TestEnsureGovernanceAuditEntry_Missing(t *testing.T) {
	runID := uuid.New()
	au := newAuditFake()
	// Seed an entry of the SAME category but a DIFFERENT artifact_id, so the
	// presence check must key on artifact_id (not merely category).
	seedAuditEntry(au, runID, "pull_request_opened", uuid.New().String())
	s := New(Config{Addr: "127.0.0.1:0", AuditRepo: au})

	invoked := false
	healed, err := s.ensureGovernanceAuditEntry(context.Background(), runID,
		"pull_request_opened", uuid.New().String(), func() error { invoked = true; return nil })
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if !healed {
		t.Error("healed = false, want true (entry missing → appended)")
	}
	if !invoked {
		t.Error("appendEntry was NOT invoked, want invoked (entry missing)")
	}
}

// TestEnsureGovernanceAuditEntry_ListError_FailsClosed: a read error from
// ListForRunByCategory surfaces as the helper's error (caller 500s) and the
// append closure is NOT invoked — governance integrity beats a gapped 200.
func TestEnsureGovernanceAuditEntry_ListError_FailsClosed(t *testing.T) {
	runID := uuid.New()
	au := newAuditFake()
	au.listByCategoryErr = errors.New("audit read down")
	s := New(Config{Addr: "127.0.0.1:0", AuditRepo: au})

	invoked := false
	healed, err := s.ensureGovernanceAuditEntry(context.Background(), runID,
		"pull_request_opened", uuid.New().String(), func() error { invoked = true; return nil })
	if err == nil {
		t.Fatal("err = nil, want the list read error (fail closed)")
	}
	if healed {
		t.Error("healed = true, want false on a read error")
	}
	if invoked {
		t.Error("appendEntry was invoked, want NOT invoked on a read error")
	}
}

// TestEnsureGovernanceAuditEntry_AppendError: when the entry is missing and the
// append closure fails, the helper surfaces that error (handler would 500,
// letting a further retry re-heal).
func TestEnsureGovernanceAuditEntry_AppendError(t *testing.T) {
	runID := uuid.New()
	au := newAuditFake()
	s := New(Config{Addr: "127.0.0.1:0", AuditRepo: au})

	boom := errors.New("append boom")
	healed, err := s.ensureGovernanceAuditEntry(context.Background(), runID,
		"pull_request_opened", uuid.New().String(), func() error { return boom })
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the append error", err)
	}
	if !healed {
		t.Error("healed = false, want true (append was attempted)")
	}
}

// TestEnsureGovernanceAuditEntry_ConcurrentRetries_NoDuplicate pins the #1396
// concurrency requirement the sequential no-duplicate tests miss: two (here:
// many) identical idempotent retries racing after the same partial
// Create-succeeded/AppendChained-failed 500 must NOT both observe the entry as
// missing and both append it. governanceHealMu serializes the helper's
// read-then-append, so every loser re-reads under the lock, sees the winner's
// entry, and no-ops — exactly one append and exactly one healed=true. Run under
// -race this also pins that the heal path itself is data-race-free.
func TestEnsureGovernanceAuditEntry_ConcurrentRetries_NoDuplicate(t *testing.T) {
	runID := uuid.New()
	artifactID := uuid.New().String()
	au := newAuditFake()
	s := New(Config{Addr: "127.0.0.1:0", AuditRepo: au})

	// The append closure writes a real governance entry carrying the
	// artifact_id, so a serialized re-read by a losing goroutine observes it
	// and skips its own append.
	appendOne := func() error {
		payload, _ := json.Marshal(map[string]any{"artifact_id": artifactID})
		_, err := au.AppendChained(context.Background(), audit.ChainAppendParams{
			RunID:    runID,
			Category: "pull_request_opened",
			Payload:  payload,
		})
		return err
	}

	const goroutines = 8
	var (
		wg      sync.WaitGroup
		healedN int64
		start   = make(chan struct{})
	)
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			<-start // release all goroutines together to maximize the race window
			healed, err := s.ensureGovernanceAuditEntry(context.Background(), runID,
				"pull_request_opened", artifactID, appendOne)
			if err != nil {
				t.Errorf("ensureGovernanceAuditEntry: %v", err)
			}
			if healed {
				atomic.AddInt64(&healedN, 1)
			}
		}()
	}
	close(start)
	wg.Wait()

	if n := countByCategory(au, "pull_request_opened"); n != 1 {
		t.Errorf("pull_request_opened entries = %d, want exactly 1 (no duplicate under concurrent retries)", n)
	}
	if healedN != 1 {
		t.Errorf("healed=true count = %d, want exactly 1 (only one goroutine appends)", healedN)
	}
}
