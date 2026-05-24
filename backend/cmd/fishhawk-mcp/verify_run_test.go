package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
)

// buildChainedAuditEntries builds N valid chained AuditEntry objects whose
// hashes are computed by audit.ComputeEntryHash, ready to seed into the
// fakeBackend's perRunAuditByRun map. The PrevHash of each entry links to
// the preceding entry's EntryHash, forming a complete valid chain.
func buildChainedAuditEntries(runID uuid.UUID, categories []string) []AuditEntry {
	ts := time.Now().UTC().Truncate(time.Microsecond)
	runIDPtr := runID

	var entries []AuditEntry
	var prevHash *string

	for i, cat := range categories {
		payload := map[string]any{"step": cat}
		raw, _ := json.Marshal(payload)

		inputs := audit.HashInputs{
			RunID:     &runIDPtr,
			Timestamp: ts.Add(time.Duration(i) * time.Second),
			Category:  cat,
			Payload:   raw,
			PrevHash:  prevHash,
		}
		hash, _ := audit.ComputeEntryHash(inputs)

		e := AuditEntry{
			ID:        uuid.New().String(),
			Sequence:  int64(i + 1),
			RunID:     runID.String(),
			Timestamp: ts.Add(time.Duration(i) * time.Second),
			Category:  cat,
			Payload:   payload,
			PrevHash:  prevHash,
			EntryHash: hash,
		}
		entries = append(entries, e)
		h := hash
		prevHash = &h
	}
	return entries
}

func TestVerifyRun_HappyChain_ReturnsVerifiedTrue(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	categories := []string{"plan_generated", "approval_submitted", "stage_completed"}
	entries := buildChainedAuditEntries(runID, categories)
	fb.perRunAuditByRun[runID] = entries

	r := newResolver(srv, nil)
	_, out, err := r.verifyRun(context.Background(), nil, VerifyRunInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("verifyRun: %v", err)
	}
	if !out.Verified {
		t.Errorf("Verified = false, want true; failure_reason = %q", out.FailureReason)
	}
	if out.ChainLength != 3 {
		t.Errorf("ChainLength = %d, want 3", out.ChainLength)
	}
	for _, cat := range categories {
		if out.CategoryCounts[cat] != 1 {
			t.Errorf("CategoryCounts[%q] = %d, want 1", cat, out.CategoryCounts[cat])
		}
	}
	if out.LastEntryHash != entries[2].EntryHash {
		t.Errorf("LastEntryHash = %q, want %q", out.LastEntryHash, entries[2].EntryHash)
	}
}

func TestVerifyRun_TamperedEntryHash_ReturnsVerifiedFalse(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	entries := buildChainedAuditEntries(runID, []string{"plan_generated", "approval_submitted", "stage_completed"})
	// Tamper the second entry's stored hash — the recomputed hash will diverge.
	entries[1].EntryHash = "0000000000000000000000000000000000000000000000000000000000000000"
	fb.perRunAuditByRun[runID] = entries

	r := newResolver(srv, nil)
	_, out, err := r.verifyRun(context.Background(), nil, VerifyRunInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("verifyRun: %v", err)
	}
	if out.Verified {
		t.Errorf("Verified = true, want false (entry hash tampered)")
	}
	if !strings.Contains(out.FailureReason, "hash_mismatch") {
		t.Errorf("FailureReason = %q, want to contain 'hash_mismatch'", out.FailureReason)
	}
}

func TestVerifyRun_BrokenChainLink_ReturnsVerifiedFalse(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()

	ts := time.Now().UTC().Truncate(time.Microsecond)
	runIDPtr := runID

	// Build entry1 with a nil prev_hash (chain root).
	payload1 := map[string]any{"step": "plan_generated"}
	raw1, _ := json.Marshal(payload1)
	inputs1 := audit.HashInputs{
		RunID:     &runIDPtr,
		Timestamp: ts,
		Category:  "plan_generated",
		Payload:   raw1,
		PrevHash:  nil,
	}
	hash1, _ := audit.ComputeEntryHash(inputs1)

	// Build entry2 with a wrong prev_hash so the chain link is broken.
	// The entry's own hash is computed correctly against the wrong prev_hash,
	// so the hash check passes but the chain link check fails.
	wrongPrevHash := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	payload2 := map[string]any{"step": "approval_submitted"}
	raw2, _ := json.Marshal(payload2)
	inputs2 := audit.HashInputs{
		RunID:     &runIDPtr,
		Timestamp: ts.Add(time.Second),
		Category:  "approval_submitted",
		Payload:   raw2,
		PrevHash:  &wrongPrevHash,
	}
	hash2, _ := audit.ComputeEntryHash(inputs2)

	entries := []AuditEntry{
		{
			ID:        uuid.New().String(),
			Sequence:  1,
			RunID:     runID.String(),
			Timestamp: ts,
			Category:  "plan_generated",
			Payload:   payload1,
			PrevHash:  nil,
			EntryHash: hash1,
		},
		{
			ID:        uuid.New().String(),
			Sequence:  2,
			RunID:     runID.String(),
			Timestamp: ts.Add(time.Second),
			Category:  "approval_submitted",
			Payload:   payload2,
			PrevHash:  &wrongPrevHash,
			EntryHash: hash2,
		},
	}
	fb.perRunAuditByRun[runID] = entries

	r := newResolver(srv, nil)
	_, out, err := r.verifyRun(context.Background(), nil, VerifyRunInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("verifyRun: %v", err)
	}
	if out.Verified {
		t.Errorf("Verified = true, want false (chain link broken)")
	}
	if !strings.Contains(out.FailureReason, "chain_broken") {
		t.Errorf("FailureReason = %q, want to contain 'chain_broken'", out.FailureReason)
	}
}

func TestVerifyRun_InvalidRunID_RejectsLocally(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.verifyRun(context.Background(), nil, VerifyRunInput{RunID: "not-a-uuid"})
	if err == nil {
		t.Fatal("expected error on malformed run_id")
	}
	if !strings.Contains(err.Error(), "not a valid UUID") {
		t.Errorf("error wording: %v", err)
	}
	// No HTTP calls should have been made before the local UUID parse fails.
	if len(fb.perRunAuditLastQueryByID) != 0 {
		t.Errorf("backend was hit despite local validation failure: %v", fb.perRunAuditLastQueryByID)
	}
}
