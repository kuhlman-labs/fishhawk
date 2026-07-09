package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// AwaitAuditInput is the fishhawk_await_audit tool's input schema (#962).
// since_sequence is the anchor that makes the wait race-free: only an
// entry whose audit Sequence is strictly greater than it resolves the
// wait, so a stale pre-anchor entry (e.g. the pre-fix-up
// implement_reviewed verdict, #894) can never be returned as the answer.
type AwaitAuditInput struct {
	RunID          string `json:"run_id" jsonschema:"the Fishhawk run UUID"`
	Category       string `json:"category" jsonschema:"the audit category to wait for (e.g. 'implement_reviewed', 'fixup_pushed')"`
	SinceSequence  int64  `json:"since_sequence,omitempty" jsonschema:"only an entry with sequence strictly greater than this resolves the wait (default 0 = the next entry of the category). Anchor it at the sequence of the event you are waiting past — e.g. the fixup_pushed entry's sequence when waiting for the post-fix-up implement_reviewed verdict"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty" jsonschema:"how long to wait before returning 'timeout' (default 360, capped at 600). On timeout, re-call with since_sequence = the returned latest_sequence to resume with no gap"`
	// IncludeIssueContext / IncludeReviewProse opt the heavy free-text
	// back into the returned entry's payload. Both default false: the
	// compact default strips reviewer free_form and issue-context
	// body/comments from the found entry's payload while retaining the
	// verdict/severity/category keys (#1727).
	IncludeIssueContext bool `json:"include_issue_context,omitempty" jsonschema:"include the found entry's issue-context body + comments in its payload; omitted by default to stay within the tool-result token budget"`
	IncludeReviewProse  bool `json:"include_review_prose,omitempty" jsonschema:"include the found entry's reviewer free_form prose in its payload; omitted by default to stay within the tool-result token budget. Verdicts/severities/concern keys are always present regardless of this flag"`
}

// AwaitAuditOutput is the fishhawk_await_audit response. Status is one of:
//
//   - "found"        — an entry with the requested category and sequence >
//     since_sequence landed; Entry carries it.
//   - "timeout"      — nothing landed within the window. LatestSequence is
//     the gapless re-arm anchor: re-calling with since_sequence =
//     latest_sequence cannot skip an entry.
//   - "run_terminal" — the run reached a terminal state (succeeded /
//     failed / cancelled) while the wait was pending (ADR-036 backstop);
//     a final since-anchored read found nothing, so the entry will most
//     likely never land. Do not re-arm blindly.
type AwaitAuditOutput struct {
	Status string `json:"status" jsonschema:"one of found, timeout, run_terminal"`
	// Entry is the matched audit entry, present only on status=found.
	Entry *AuditEntry `json:"entry,omitempty" jsonschema:"the matched audit entry; present only on status=found"`
	// LatestSequence is always set: the matched entry's sequence on
	// found; otherwise the highest matching-category sequence observed
	// (== since_sequence when nothing landed) — the gapless re-arm anchor.
	LatestSequence      int64   `json:"latest_sequence" jsonschema:"the matched entry's sequence on found; otherwise the gapless re-arm anchor (== since_sequence when nothing landed). Re-call with since_sequence set to this value to resume the wait with no gap"`
	WaitedSeconds       float64 `json:"waited_seconds" jsonschema:"elapsed wall time spent waiting"`
	Message             string  `json:"message,omitempty" jsonschema:"actionable explanation on the timeout / run_terminal statuses"`
	PollIntervalSeconds int     `json:"poll_interval_seconds,omitempty" jsonschema:"server-suggested cadence (seconds) for switching to fishhawk_get_run_status polling; present only on the timeout status"`
}

// registerAwaitAudit wires the fishhawk_await_audit tool (#962): the
// sequence-anchored await primitive underlying the category-specific
// waits. Read-only per ADR-021 — it only polls the per-run audit
// endpoint, server-side, on an injectable interval.
func registerAwaitAudit(srv *mcp.Server, resolver *runResolver) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "fishhawk_await_audit",
		Description: strings.TrimSpace(`
Block until the next audit entry with the given category and sequence >
since_sequence lands for a run, and return that entry. This is the
sequence-anchored await primitive (#962) that replaces hand-rolled audit
poll loops: anchoring at a known sequence makes the wait race-free.

The anchoring contract: an event that happens AFTER another event always
has a strictly greater audit sequence. So "the review after the fix-up"
is the implement_reviewed entry with sequence > the fixup_pushed entry's
sequence — pass that as since_sequence and a stale pre-fix-up verdict
can never satisfy the wait (the #894 class of stale-read race).

Statuses:
  - "found"        — Entry carries the matched audit entry;
                     latest_sequence is its sequence.
  - "timeout"      — nothing landed within the window. Gapless re-arm:
                     re-call with since_sequence = the returned
                     latest_sequence (== your anchor when nothing
                     landed) and no entry can be skipped. The wait holds
                     no server state, so a cut-short call is a safe
                     no-op to re-issue.
  - "run_terminal" — the run reached succeeded/failed/cancelled while
                     waiting (the ADR-036 non-stranding backstop); a
                     final anchored read found nothing. The entry will
                     most likely never land — do not re-arm blindly;
                     check fishhawk_get_run_status first.

Inputs:
  - run_id          (required) — Fishhawk run UUID.
  - category        (required) — audit category to wait for.
  - since_sequence  — anchor; default 0 waits for the next entry of the
                      category regardless of history.
  - timeout_seconds — default 360, capped at 600.

The returned entry's payload is compact by default (#1727): oversized
free-text — reviewer free_form prose and issue-context body/comments —
is stripped to stay within the tool-result token budget, while the
verdict/severity/category keys are always retained. Set
include_review_prose=true or include_issue_context=true to restore the
full payload.

Re-polling fishhawk_get_run_status remains the authoritative fallback
path (ADR-037); this tool blocks that poll for you when you would
rather wait synchronously than loop yourself.
`),
	}, resolver.awaitAudit)
}

// awaitAudit is the tool handler. Mirrors awaitReview's structure: a
// fast-path read, then a poll on the injectable reviewPollInterval under
// a clamped deadline, with the ADR-036 run-terminal backstop checked once
// before the loop and on each still-empty tick.
func (r *runResolver) awaitAudit(ctx context.Context, _ *mcp.CallToolRequest, in AwaitAuditInput) (*mcp.CallToolResult, AwaitAuditOutput, error) {
	runID, err := uuid.Parse(in.RunID)
	if err != nil {
		return nil, AwaitAuditOutput{}, fmt.Errorf("run_id %q is not a valid UUID: %w", in.RunID, err)
	}
	if strings.TrimSpace(in.Category) == "" {
		return nil, AwaitAuditOutput{}, fmt.Errorf("category must be a non-empty audit category (e.g. 'implement_reviewed')")
	}
	if in.SinceSequence < 0 {
		return nil, AwaitAuditOutput{}, fmt.Errorf("since_sequence must be >= 0; got %d", in.SinceSequence)
	}
	timeout := clampAwaitTimeout(in.TimeoutSeconds)
	start := time.Now()

	// Fast path: the entry may already exist. The endpoint is
	// sequence-ascending and the anchor filter applies before
	// pagination, so the first entry past the anchor is the answer.
	entry, err := r.nextAuditEntry(ctx, runID, in.Category, in.SinceSequence)
	if err != nil {
		return nil, AwaitAuditOutput{}, fmt.Errorf("list audit: %w", err)
	}
	if entry != nil {
		return nil, awaitAuditFoundOutput(entry, in, start), nil
	}

	// Nothing yet: check the run-terminal backstop once before the loop
	// so a run that is already terminal at call time resolves without a
	// poll tick (ADR-036 — the wait never strands past a dead run).
	if out, done := r.awaitAuditRunTerminalBackstop(ctx, runID, in, start); done {
		return nil, out, nil
	}

	interval := r.reviewPollInterval
	if interval <= 0 {
		interval = defaultReviewPollInterval
	}
	pollCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-pollCtx.Done():
			return nil, awaitAuditTimeoutOutput(in, timeout, start), nil
		case <-ticker.C:
			entry, err := r.nextAuditEntry(pollCtx, runID, in.Category, in.SinceSequence)
			if err != nil {
				// A deadline hit mid-poll cancels the in-flight request;
				// that is a timeout, not a transport failure — return
				// the gapless re-arm point rather than an error.
				if pollCtx.Err() != nil {
					return nil, awaitAuditTimeoutOutput(in, timeout, start), nil
				}
				return nil, AwaitAuditOutput{}, fmt.Errorf("poll audit: %w", err)
			}
			if entry != nil {
				return nil, awaitAuditFoundOutput(entry, in, start), nil
			}
			if out, done := r.awaitAuditRunTerminalBackstop(pollCtx, runID, in, start); done {
				return nil, out, nil
			}
		}
	}
}

// nextAuditEntry fetches the first audit entry for the run with the given
// category and sequence > sinceSeq, or nil when none exists yet. Limit=1
// suffices because the endpoint is sequence-ascending and the anchor
// filter applies before pagination server-side.
func (r *runResolver) nextAuditEntry(ctx context.Context, runID uuid.UUID, category string, sinceSeq int64) (*AuditEntry, error) {
	entries, _, err := r.api.ListRunAudit(ctx, runID, ListRunAuditFilter{
		Category:      category,
		SinceSequence: sinceSeq,
		Limit:         1,
	})
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, nil
	}
	return &entries[0], nil
}

// awaitAuditRunTerminalBackstop resolves the wait when the run itself has
// reached a terminal state while the entry is still pending (ADR-036): the
// entry will most likely never land, so holding the session open to the
// deadline would strand the caller. It performs ONE final since-anchored
// read before resolving — some categories (e.g. run-completion entries)
// land at or just after the terminal transition, and that read must win
// over the backstop. Returns (output, true) to resolve the wait; (zero,
// false) to keep polling. Best-effort — a GetRun error or a non-terminal
// run leaves the normal poll/timeout path in charge.
func (r *runResolver) awaitAuditRunTerminalBackstop(ctx context.Context, runID uuid.UUID, in AwaitAuditInput, start time.Time) (AwaitAuditOutput, bool) {
	runRow, err := r.api.GetRun(ctx, runID)
	if err != nil || runRow == nil {
		return AwaitAuditOutput{}, false
	}
	if !runStateIsTerminal(runRow.State) {
		return AwaitAuditOutput{}, false
	}
	// Final read: an entry that landed at/after the terminal transition
	// still resolves as found.
	entry, err := r.nextAuditEntry(ctx, runID, in.Category, in.SinceSequence)
	if err == nil && entry != nil {
		return awaitAuditFoundOutput(entry, in, start), true
	}
	return AwaitAuditOutput{
		Status:         "run_terminal",
		LatestSequence: in.SinceSequence,
		WaitedSeconds:  time.Since(start).Seconds(),
		Message: fmt.Sprintf("no %q entry with sequence > %d landed, and run %s has reached terminal state %q — "+
			"the entry will most likely never land, so the wait resolved instead of holding the session open. "+
			"Do not re-arm blindly: check fishhawk_get_run_status for the final run state first.",
			in.Category, in.SinceSequence, runID, runRow.State),
	}, true
}

// awaitAuditFoundOutput builds the resolved response for a matched entry.
// It applies the compact-by-default projection (#1727) to a COPY of the
// entry so the returned payload has reviewer free_form and issue-context
// body/comments stripped unless the caller opted in — the shared
// backend-fetched AuditEntry is never mutated. LatestSequence is read off
// the entry before the copy, so the anchor semantics are unaffected.
func awaitAuditFoundOutput(entry *AuditEntry, in AwaitAuditInput, start time.Time) AwaitAuditOutput {
	projected := *entry
	projected.Payload = compactAuditPayload(entry.Payload, !in.IncludeIssueContext, !in.IncludeReviewProse)
	return AwaitAuditOutput{
		Status:         "found",
		Entry:          &projected,
		LatestSequence: entry.Sequence,
		WaitedSeconds:  time.Since(start).Seconds(),
	}
}

// awaitAuditTimeoutOutput builds the resumable timeout response. The wait
// holds no server state, so a timeout is an idempotent checkpoint, not an
// error. LatestSequence == since_sequence (nothing past the anchor was
// observed — anything observed would have resolved as found), so re-arming
// from it cannot skip an entry.
func awaitAuditTimeoutOutput(in AwaitAuditInput, timeout int, start time.Time) AwaitAuditOutput {
	return AwaitAuditOutput{
		Status:              "timeout",
		LatestSequence:      in.SinceSequence,
		WaitedSeconds:       time.Since(start).Seconds(),
		PollIntervalSeconds: suggestedReviewPollIntervalSeconds,
		Message: fmt.Sprintf("no %q entry with sequence > %d landed within %ds. The wait holds nothing: re-call "+
			"fishhawk_await_audit with since_sequence=%d to resume it with no gap, or poll fishhawk_get_run_status "+
			"every %ds (the authoritative path).",
			in.Category, in.SinceSequence, timeout, in.SinceSequence, suggestedReviewPollIntervalSeconds),
	}
}
