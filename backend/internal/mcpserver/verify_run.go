package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
)

// VerifyRunInput is the fishhawk_verify_run tool's input schema.
type VerifyRunInput struct {
	RunID string `json:"run_id" jsonschema:"the Fishhawk run UUID whose audit chain is to be verified"`
}

// VerifyRunOutput is the response shape. Verified is true when every one of
// the requested run's own entries passes the sequence, entry-hash, genesis
// and chain-link checks; false when any check fails. ChainLength and
// CategoryCounts cover ONLY the requested run's rows (#5065): rows the
// complete chain=true read returns for other runs (CI-retry descendants, each
// an independently rooted chain) are excluded, counted in
// DescendantEntriesExcluded and named in DescendantRunIDs, and are NOT
// verified by this call.
type VerifyRunOutput struct {
	Verified                  bool           `json:"verified"`
	ChainLength               int            `json:"chain_length"`
	FirstSequence             int64          `json:"first_sequence"`
	LastSequence              int64          `json:"last_sequence"`
	LastEntryHash             string         `json:"last_entry_hash"`
	FailureReason             string         `json:"failure_reason,omitempty"`
	CategoryCounts            map[string]int `json:"category_counts"`
	DescendantEntriesExcluded int            `json:"descendant_entries_excluded"`
	DescendantRunIDs          []string       `json:"descendant_run_ids,omitempty"`
}

// registerVerifyRun wires the fishhawk_verify_run tool. Read-only per
// ADR-021. It reads the complete chain=true audit feed (#5065), which the
// server serves to operator tokens only: a run-bound (mcp:run:) subject is
// refused 403 run_token_forbidden, and the tool surfaces that as an explicit
// "audit chain was NOT verified" error rather than a verified:false result.
func registerVerifyRun(srv *mcp.Server, resolver *runResolver) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "fishhawk_verify_run",
		Description: strings.TrimSpace(`
Verify a run's audit-chain integrity. Use this before opening or merging
a PR as a halt gate — verified=false means the tamper-evident chain is
broken, so stop and file an incident rather than ship.

Reads the COMPLETE per-run chain (GET /v0/runs/{run_id}/audit?chain=true,
cursor-paginated), not the ordinary audit feed, which withholds rows such
as delegation_shadow_evaluated and would break the recomputed links. That
read also returns the run's CI-retry descendants: each is an independently
rooted chain, so the tool selects ONLY the requested run's own rows and
never splices a descendant into it. Descendant rows are NOT verified by
this call: their run ids are listed in descendant_run_ids, and each one
must be verified separately by calling this tool with that run_id.

Checks, over the requested run's own rows in sequence order:
  1. Sequence monotonicity — entries must be strictly increasing.
  2. Entry hash — each stored entry_hash must match the value
     recomputed from the entry's fields.
  3. Genesis — the first entry carries no prev_hash (a non-nil one
     means the read is truncated at the head).
  4. Chain link — each later entry's prev_hash must match the
     preceding entry's entry_hash.
A row whose run_id does not parse fails closed (verified=false).

Requires an operator token. The server refuses the complete read to a
run-bound (mcp:run:) agent token; the tool then returns an error saying
the audit chain was NOT verified. That is an authorization refusal, not
an integrity failure: re-run with an operator token, or run
"fishhawk export --run <id>" and check the bundle with fishhawk-verify.

Response:
  - verified        — true when every check passes.
  - chain_length    — the requested run's own entries verified.
  - first_sequence  — sequence of the first entry.
  - last_sequence   — sequence of the last entry.
  - last_entry_hash — entry_hash of the final entry.
  - failure_reason  — populated only when verified=false; names the
                      specific check that failed and the entry id.
  - category_counts — map of category → count across the run's own
                      entries.
  - descendant_entries_excluded — rows from other runs (CI-retry
                      descendants) excluded from, and NOT verified by,
                      this call.
  - descendant_run_ids — sorted, unique ids of those runs; verify each
                      separately.
`),
	}, resolver.verifyRun)
}

// completeReadRefused reports whether err is the server's refusal of the
// complete chain=true read to a run-bound subject (E83.60 / #4118): 403 with
// code run_token_forbidden. verifyRun turns it into an explicit not-verified
// error and never falls back to the filtered ordinary feed (#5065).
func completeReadRefused(err error) bool {
	var ae *apiError
	return errors.As(err, &ae) && ae.StatusCode == http.StatusForbidden && ae.Code == "run_token_forbidden"
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

// verifyRun is the tool handler. It pages the complete chain=true read,
// selects the requested run's own rows (the read also carries independently
// rooted CI-retry descendant chains, which must never be spliced in), and
// walks them with the sequence, entry-hash, genesis and chain-link checks.
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
			Chain:  true,
		})
		if err != nil {
			if completeReadRefused(err) {
				return nil, VerifyRunOutput{}, fmt.Errorf(
					"audit chain was NOT verified for run %s: the backend refused the complete chain=true audit read "+
						"to this run-bound token. This is an authorization refusal, not a chain break. Remedy: re-run "+
						"fishhawk_verify_run with an operator token, or run `fishhawk export --run %s` and check the "+
						"bundle with the standalone fishhawk-verify: %w", runID, runID, err)
			}
			return nil, VerifyRunOutput{}, fmt.Errorf("list run audit: %w", err)
		}
		all = append(all, items...)
		if next == "" {
			break
		}
		cursor = next
	}

	// Run selection (#5065). The chain=true read returns the requested run's
	// rows AND its CI-retry descendants' rows, interleaved by global
	// sequence. Each descendant is an independently rooted chain, so only the
	// requested run's own rows are walked; the rest are counted and named so
	// each descendant can be verified on its own. The server returns rows
	// sequence-ascending, so the filtered slice keeps that order.
	var own []AuditEntry
	descendantSet := map[string]struct{}{}
	excluded := 0
	descendantIDs := func() []string {
		if len(descendantSet) == 0 {
			return nil
		}
		ids := make([]string, 0, len(descendantSet))
		for id := range descendantSet {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		return ids
	}
	categoryCounts := map[string]int{}
	fail := func(chainLength int, reason string) (*mcp.CallToolResult, VerifyRunOutput, error) {
		return nil, VerifyRunOutput{
			Verified:                  false,
			ChainLength:               chainLength,
			CategoryCounts:            categoryCounts,
			FailureReason:             reason,
			DescendantEntriesExcluded: excluded,
			DescendantRunIDs:          descendantIDs(),
		}, nil
	}
	for i := range all {
		e := &all[i]
		entryRunID, err := uuid.Parse(e.RunID)
		if err != nil {
			// Fail closed: an entry the verifier cannot attribute to a run
			// is neither verified nor silently skipped.
			return fail(len(own), fmt.Sprintf("hash_input_error: entry %s: parse run_id %q: %v", e.ID, e.RunID, err))
		}
		if entryRunID != runID {
			excluded++
			descendantSet[entryRunID.String()] = struct{}{}
			continue
		}
		own = append(own, *e)
	}

	if len(own) == 0 {
		return nil, VerifyRunOutput{
			Verified:                  true,
			ChainLength:               0,
			CategoryCounts:            categoryCounts,
			DescendantEntriesExcluded: excluded,
			DescendantRunIDs:          descendantIDs(),
		}, nil
	}

	prevEntryHash := ""
	for i := range own {
		e := &own[i]
		categoryCounts[e.Category]++

		// (1) Sequence monotonicity.
		if i > 0 && e.Sequence <= own[i-1].Sequence {
			return fail(i+1, fmt.Sprintf("sequence_not_monotonic: entry %s sequence %d not greater than preceding %d",
				e.ID, e.Sequence, own[i-1].Sequence))
		}

		// (2) Entry hash verification.
		inputs, err := entryToHashInputs(e)
		if err != nil {
			return fail(i+1, fmt.Sprintf("hash_input_error: entry %s: %v", e.ID, err))
		}
		computed, err := audit.ComputeEntryHash(inputs)
		if err != nil {
			return fail(i+1, fmt.Sprintf("hash_compute_error: entry %s: %v", e.ID, err))
		}
		if computed != e.EntryHash {
			return fail(i+1, fmt.Sprintf("hash_mismatch: entry %s: computed %s, stored %s",
				e.ID, computed, e.EntryHash))
		}

		// (3) Genesis: a run chain's first entry carries no prev_hash — the
		// standalone verifier's IssueFirstEntryHasPrevHash rule
		// (verifier/internal/audit/verify.go). A non-nil one means the read
		// is truncated at the head, which the link check alone cannot see.
		if i == 0 && e.PrevHash != nil {
			return fail(i+1, fmt.Sprintf("first_entry_has_prev_hash: entry %s prev_hash %s; a run chain's genesis entry carries none (the read may be truncated at the head)",
				e.ID, *e.PrevHash))
		}

		// (4) Chain link check (skip for the first entry).
		if i > 0 {
			if e.PrevHash == nil {
				return fail(i+1, fmt.Sprintf("chain_broken: entry %s has nil prev_hash but is not the first entry", e.ID))
			}
			if *e.PrevHash != prevEntryHash {
				return fail(i+1, fmt.Sprintf("chain_broken: entry %s prev_hash %s does not match preceding entry_hash %s",
					e.ID, *e.PrevHash, prevEntryHash))
			}
		}
		prevEntryHash = e.EntryHash
	}

	last := &own[len(own)-1]
	return nil, VerifyRunOutput{
		Verified:                  true,
		ChainLength:               len(own),
		FirstSequence:             own[0].Sequence,
		LastSequence:              last.Sequence,
		LastEntryHash:             last.EntryHash,
		CategoryCounts:            categoryCounts,
		DescendantEntriesExcluded: excluded,
		DescendantRunIDs:          descendantIDs(),
	}, nil
}
