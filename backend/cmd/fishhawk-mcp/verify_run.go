package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
)

// VerifyRunInput is the fishhawk_verify_run tool's input schema.
type VerifyRunInput struct {
	RunID string `json:"run_id" jsonschema:"the Fishhawk run UUID whose audit chain is to be verified"`
}

// VerifyRunOutput is the response shape. Verified is true when every entry's
// hash and chain link is intact; false when any check fails.
type VerifyRunOutput struct {
	Verified       bool           `json:"verified"`
	ChainLength    int            `json:"chain_length"`
	FirstSequence  int64          `json:"first_sequence"`
	LastSequence   int64          `json:"last_sequence"`
	LastEntryHash  string         `json:"last_entry_hash"`
	FailureReason  string         `json:"failure_reason,omitempty"`
	CategoryCounts map[string]int `json:"category_counts"`
}

// registerVerifyRun wires the fishhawk_verify_run tool. Read-only per
// ADR-021; works with both fhm_* and fhk_* tokens.
func registerVerifyRun(srv *mcp.Server, resolver *runResolver) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "fishhawk_verify_run",
		Description: strings.TrimSpace(`
Verify audit chain integrity for a Fishhawk run.

Fetches every audit entry via cursor pagination and checks:
  1. Sequence monotonicity — entries must be strictly increasing.
  2. Entry hash — each stored entry_hash must match the value
     recomputed from the entry's fields.
  3. Chain link — each entry's prev_hash must match the preceding
     entry's entry_hash.

verified=false is a halt condition before opening a PR. File an
incident when the chain is broken on a completed run.

Response:
  - verified        — true when every check passes.
  - chain_length    — total entries verified.
  - first_sequence  — sequence of the first entry.
  - last_sequence   — sequence of the last entry.
  - last_entry_hash — entry_hash of the final entry.
  - failure_reason  — populated only when verified=false; names the
                      specific check that failed and the entry id.
  - category_counts — map of category → count across all entries.
`),
	}, resolver.verifyRun)
}

// toActorKind converts *string to *audit.ActorKind for hash inputs.
func toActorKind(s *string) *audit.ActorKind {
	if s == nil {
		return nil
	}
	k := audit.ActorKind(*s)
	return &k
}

// entryToHashInputs converts a wire AuditEntry into the audit.HashInputs
// needed to recompute the entry's hash. Payload is typed `any` after the
// HTTP JSON decode, so it is re-marshaled to json.RawMessage before hashing.
func entryToHashInputs(e *AuditEntry) (audit.HashInputs, error) {
	var runID *uuid.UUID
	if e.RunID != "" {
		id, err := uuid.Parse(e.RunID)
		if err != nil {
			return audit.HashInputs{}, fmt.Errorf("parse run_id %q: %w", e.RunID, err)
		}
		runID = &id
	}

	var stageID *uuid.UUID
	if e.StageID != nil && *e.StageID != "" {
		id, err := uuid.Parse(*e.StageID)
		if err != nil {
			return audit.HashInputs{}, fmt.Errorf("parse stage_id %q: %w", *e.StageID, err)
		}
		stageID = &id
	}

	raw, err := json.Marshal(e.Payload)
	if err != nil {
		return audit.HashInputs{}, fmt.Errorf("re-marshal payload: %w", err)
	}

	return audit.HashInputs{
		RunID:        runID,
		StageID:      stageID,
		Timestamp:    e.Timestamp,
		Category:     e.Category,
		ActorKind:    toActorKind(e.ActorKind),
		ActorSubject: e.ActorSubject,
		Payload:      raw,
		PrevHash:     e.PrevHash,
	}, nil
}

// verifyRun is the tool handler.
func (r *runResolver) verifyRun(ctx context.Context, _ *mcp.CallToolRequest, in VerifyRunInput) (*mcp.CallToolResult, VerifyRunOutput, error) {
	runID, err := uuid.Parse(in.RunID)
	if err != nil {
		return nil, VerifyRunOutput{}, fmt.Errorf("run_id %q is not a valid UUID: %w", in.RunID, err)
	}

	var all []AuditEntry
	cursor := ""
	for {
		items, next, err := r.api.ListRunAudit(ctx, runID, ListRunAuditFilter{
			Limit:  listAuditLimitMax,
			Cursor: cursor,
		})
		if err != nil {
			return nil, VerifyRunOutput{}, fmt.Errorf("list run audit: %w", err)
		}
		all = append(all, items...)
		if next == "" {
			break
		}
		cursor = next
	}

	categoryCounts := map[string]int{}
	if len(all) == 0 {
		return nil, VerifyRunOutput{
			Verified:       true,
			ChainLength:    0,
			CategoryCounts: categoryCounts,
		}, nil
	}

	prevEntryHash := ""
	for i := range all {
		e := &all[i]
		categoryCounts[e.Category]++

		// (1) Sequence monotonicity.
		if i > 0 && e.Sequence <= all[i-1].Sequence {
			return nil, VerifyRunOutput{
				Verified:       false,
				ChainLength:    i + 1,
				CategoryCounts: categoryCounts,
				FailureReason: fmt.Sprintf("sequence_not_monotonic: entry %s sequence %d not greater than preceding %d",
					e.ID, e.Sequence, all[i-1].Sequence),
			}, nil
		}

		// (2) Entry hash verification.
		inputs, err := entryToHashInputs(e)
		if err != nil {
			return nil, VerifyRunOutput{
				Verified:       false,
				ChainLength:    i + 1,
				CategoryCounts: categoryCounts,
				FailureReason:  fmt.Sprintf("hash_input_error: entry %s: %v", e.ID, err),
			}, nil
		}
		computed, err := audit.ComputeEntryHash(inputs)
		if err != nil {
			return nil, VerifyRunOutput{
				Verified:       false,
				ChainLength:    i + 1,
				CategoryCounts: categoryCounts,
				FailureReason:  fmt.Sprintf("hash_compute_error: entry %s: %v", e.ID, err),
			}, nil
		}
		if computed != e.EntryHash {
			return nil, VerifyRunOutput{
				Verified:       false,
				ChainLength:    i + 1,
				CategoryCounts: categoryCounts,
				FailureReason: fmt.Sprintf("hash_mismatch: entry %s: computed %s, stored %s",
					e.ID, computed, e.EntryHash),
			}, nil
		}

		// (3) Chain link check (skip for the first entry).
		if i > 0 {
			if e.PrevHash == nil {
				return nil, VerifyRunOutput{
					Verified:       false,
					ChainLength:    i + 1,
					CategoryCounts: categoryCounts,
					FailureReason:  fmt.Sprintf("chain_broken: entry %s has nil prev_hash but is not the first entry", e.ID),
				}, nil
			}
			if *e.PrevHash != prevEntryHash {
				return nil, VerifyRunOutput{
					Verified:       false,
					ChainLength:    i + 1,
					CategoryCounts: categoryCounts,
					FailureReason: fmt.Sprintf("chain_broken: entry %s prev_hash %s does not match preceding entry_hash %s",
						e.ID, *e.PrevHash, prevEntryHash),
				}, nil
			}
		}
		prevEntryHash = e.EntryHash
	}

	last := &all[len(all)-1]
	return nil, VerifyRunOutput{
		Verified:       true,
		ChainLength:    len(all),
		FirstSequence:  all[0].Sequence,
		LastSequence:   last.Sequence,
		LastEntryHash:  last.EntryHash,
		CategoryCounts: categoryCounts,
	}, nil
}
