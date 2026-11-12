package mcpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"reflect"
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

// --- #5065: complete chain=true read, run selection, genesis, fail-closed ---

// TestVerifyRun_RequestsCompleteChainRead pins that the verifier asks for the
// COMPLETE read (chain=true) rather than the ordinary, withholding feed.
func TestVerifyRun_RequestsCompleteChainRead(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	fb.perRunAuditByRun[runID] = buildChainedAuditEntries(runID, []string{"plan_generated", "stage_completed"})

	r := newResolver(srv, nil)
	if _, _, err := r.verifyRun(context.Background(), nil, VerifyRunInput{RunID: runID.String()}); err != nil {
		t.Fatalf("verifyRun: %v", err)
	}
	q, err := url.ParseQuery(fb.perRunAuditLastQueryByID[runID])
	if err != nil {
		t.Fatalf("parse recorded query: %v", err)
	}
	if got := q.Get("chain"); got != "true" {
		t.Errorf("verify_run request chain = %q, want \"true\" (raw query %q)", got, fb.perRunAuditLastQueryByID[runID])
	}
}

// TestListRunAudit_OmitsChainWhenFalse is the byte-identical-callers half
// (#5065 approval condition 2): an ordinary ListRunAudit (Chain false) sends
// NO chain key at all, so every other caller's request is unchanged.
func TestListRunAudit_OmitsChainWhenFalse(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	runID := uuid.New()

	if _, _, err := r.api.ListRunAudit(context.Background(), runID, ListRunAuditFilter{Limit: 10}); err != nil {
		t.Fatalf("ListRunAudit: %v", err)
	}
	raw := fb.perRunAuditLastQueryByID[runID]
	q, err := url.ParseQuery(raw)
	if err != nil {
		t.Fatalf("parse recorded query: %v", err)
	}
	if _, ok := q["chain"]; ok {
		t.Errorf("ordinary ListRunAudit must carry no chain key; raw query %q", raw)
	}
}

// TestVerifyRun_TruncatedHead_ReturnsVerifiedFalse isolates the genesis
// check: entry 0 of a valid chain is dropped, so the remaining entries' hashes
// and links are all correct and ONLY the first-entry-has-no-prev_hash rule can
// fail the read.
func TestVerifyRun_TruncatedHead_ReturnsVerifiedFalse(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	entries := buildChainedAuditEntries(runID, []string{"plan_generated", "approval_submitted", "stage_completed"})
	fb.perRunAuditByRun[runID] = entries[1:]

	r := newResolver(srv, nil)
	_, out, err := r.verifyRun(context.Background(), nil, VerifyRunInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("verifyRun: %v", err)
	}
	if out.Verified {
		t.Fatalf("Verified = true, want false (chain truncated at the head)")
	}
	if !strings.HasPrefix(out.FailureReason, "first_entry_has_prev_hash") {
		t.Errorf("FailureReason = %q, want first_entry_has_prev_hash", out.FailureReason)
	}
}

// TestVerifyRun_TamperedPayload_ReturnsVerifiedFalse edits a stored payload
// and leaves its entry_hash alone: the recomputed hash diverges.
func TestVerifyRun_TamperedPayload_ReturnsVerifiedFalse(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	entries := buildChainedAuditEntries(runID, []string{"plan_generated", "approval_submitted", "stage_completed"})
	entries[1].Payload = map[string]any{"step": "approval_submitted", "tampered": true}
	fb.perRunAuditByRun[runID] = entries

	r := newResolver(srv, nil)
	_, out, err := r.verifyRun(context.Background(), nil, VerifyRunInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("verifyRun: %v", err)
	}
	if out.Verified {
		t.Fatalf("Verified = true, want false (payload tampered)")
	}
	if !strings.HasPrefix(out.FailureReason, "hash_mismatch") || !strings.Contains(out.FailureReason, entries[1].ID) {
		t.Errorf("FailureReason = %q, want hash_mismatch naming entry %s", out.FailureReason, entries[1].ID)
	}
}

// TestVerifyRun_SequenceNotMonotonic_ReturnsVerifiedFalse repeats a sequence.
// Sequence is not a hash input, so the hashes and links stay valid and only
// the monotonicity check can fail the read.
func TestVerifyRun_SequenceNotMonotonic_ReturnsVerifiedFalse(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	entries := buildChainedAuditEntries(runID, []string{"plan_generated", "approval_submitted", "stage_completed"})
	entries[2].Sequence = entries[1].Sequence
	fb.perRunAuditByRun[runID] = entries

	r := newResolver(srv, nil)
	_, out, err := r.verifyRun(context.Background(), nil, VerifyRunInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("verifyRun: %v", err)
	}
	if out.Verified {
		t.Fatalf("Verified = true, want false (sequence repeated)")
	}
	if !strings.HasPrefix(out.FailureReason, "sequence_not_monotonic") {
		t.Errorf("FailureReason = %q, want sequence_not_monotonic", out.FailureReason)
	}
}

// interleaveBySequence merges per-run chains into one slice and assigns a
// single global sequence in the given order, the shape the chain=true read
// returns. Sequence is not a hash input, so the chains stay valid.
func interleaveBySequence(order ...[]AuditEntry) []AuditEntry {
	var out []AuditEntry
	for _, part := range order {
		out = append(out, part...)
	}
	for i := range out {
		out[i].Sequence = int64(i + 1)
	}
	return out
}

// TestVerifyRun_DescendantRowsExcluded seeds the chain=true shape: the parent's
// chain interleaved with a CI-retry descendant's independently rooted chain.
// The parent verifies on its own rows; the descendant's rows are reported.
func TestVerifyRun_DescendantRowsExcluded(t *testing.T) {
	fb, srv := newFakeBackend(t)
	parent, child := uuid.New(), uuid.New()
	p := buildChainedAuditEntries(parent, []string{"run_created", "plan_generated", "approval_submitted", "stage_completed"})
	c := buildChainedAuditEntries(child, []string{"run_created", "stage_completed"})
	fb.perRunAuditByRun[parent] = interleaveBySequence(p[:2], c[:1], p[2:3], c[1:], p[3:])

	r := newResolver(srv, nil)
	_, out, err := r.verifyRun(context.Background(), nil, VerifyRunInput{RunID: parent.String()})
	if err != nil {
		t.Fatalf("verifyRun: %v", err)
	}
	if !out.Verified {
		t.Fatalf("Verified = false, want true; failure_reason = %q", out.FailureReason)
	}
	if out.ChainLength != len(p) {
		t.Errorf("ChainLength = %d, want %d (parent rows only)", out.ChainLength, len(p))
	}
	if out.CategoryCounts["run_created"] != 1 || out.CategoryCounts["stage_completed"] != 1 {
		t.Errorf("CategoryCounts = %v, want parent rows only (run_created 1, stage_completed 1)", out.CategoryCounts)
	}
	if out.DescendantEntriesExcluded != len(c) {
		t.Errorf("DescendantEntriesExcluded = %d, want %d", out.DescendantEntriesExcluded, len(c))
	}
	if want := []string{child.String()}; !reflect.DeepEqual(out.DescendantRunIDs, want) {
		t.Errorf("DescendantRunIDs = %v, want %v", out.DescendantRunIDs, want)
	}
	if out.LastEntryHash != p[len(p)-1].EntryHash {
		t.Errorf("LastEntryHash = %q, want the parent's last hash %q", out.LastEntryHash, p[len(p)-1].EntryHash)
	}
}

// TestVerifyRun_UnparseableEntryRunID_FailsClosed appends one EXTRA row with a
// malformed run_id after an otherwise complete, intact chain (#5065 approval
// condition 3): without the fail-closed return the row is skipped and the
// intact chain verifies true, so only the control can fail this read.
func TestVerifyRun_UnparseableEntryRunID_FailsClosed(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	entries := buildChainedAuditEntries(runID, []string{"plan_generated", "approval_submitted", "stage_completed"})
	bad := entries[2]
	bad.ID = uuid.New().String()
	bad.Sequence = entries[2].Sequence + 1
	bad.RunID = "not-a-uuid"
	fb.perRunAuditByRun[runID] = append(entries, bad)

	r := newResolver(srv, nil)
	_, out, err := r.verifyRun(context.Background(), nil, VerifyRunInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("verifyRun: %v", err)
	}
	if out.Verified {
		t.Fatalf("Verified = true, want false (an entry with an unparseable run_id must fail closed)")
	}
	if !strings.HasPrefix(out.FailureReason, "hash_input_error") || !strings.Contains(out.FailureReason, "parse run_id") {
		t.Errorf("FailureReason = %q, want hash_input_error ... parse run_id", out.FailureReason)
	}
}

// TestVerifyRun_ListErrorKeepsGenericWrap pins that only the run_token_forbidden
// refusal becomes the explicit not-verified error: a 500, and a 403 that does
// not carry that code, keep the plain "list run audit:" wrap.
func TestVerifyRun_ListErrorKeepsGenericWrap(t *testing.T) {
	for _, status := range []int{http.StatusInternalServerError, http.StatusForbidden} {
		fb, srv := newFakeBackend(t)
		runID := uuid.New()
		fb.perRunAuditStatus = status

		r := newResolver(srv, nil)
		_, _, err := r.verifyRun(context.Background(), nil, VerifyRunInput{RunID: runID.String()})
		if err == nil {
			t.Fatalf("status %d: want an error", status)
		}
		if !strings.HasPrefix(err.Error(), "list run audit: ") || strings.Contains(err.Error(), "NOT verified") {
			t.Errorf("status %d: error = %q, want the generic list run audit wrap", status, err)
		}
	}
}
