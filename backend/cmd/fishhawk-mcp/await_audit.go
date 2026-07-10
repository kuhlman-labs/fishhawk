package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
)

// AwaitAuditInput is the fishhawk_await_audit tool's input schema (#962).
// since_sequence is the anchor that makes the wait race-free: only an
// entry whose audit Sequence is strictly greater than it resolves the
// wait, so a stale pre-anchor entry (e.g. the pre-fix-up
// implement_reviewed verdict, #894) can never be returned as the answer.
type AwaitAuditInput struct {
	RunID    string `json:"run_id" jsonschema:"the Fishhawk run UUID"`
	Category string `json:"category" jsonschema:"the audit category to wait for (e.g. 'implement_reviewed', 'fixup_pushed'). Provide this OR categories (or both — they union). An unknown/misspelled category is rejected up front with the nearest known categories unless allow_unknown is set"`
	// Categories is the plural, OR-semantics form (#1764): the wait
	// resolves on the FIRST audit entry (lowest sequence) matching ANY of
	// the listed categories past the anchor, in one call. Unioned with the
	// singular Category. Use it when one wait must resolve across several
	// anchors — e.g. awaiting either implement_reviewed OR fixup_pushed.
	Categories     []string `json:"categories,omitempty" jsonschema:"OR-semantics list of audit categories; the wait resolves on the first entry matching ANY of them past the anchor. Unioned with category. Each is validated against the known-category registry unless allow_unknown is set"`
	SinceSequence  int64    `json:"since_sequence,omitempty" jsonschema:"only an entry with sequence strictly greater than this resolves the wait (default 0 = the next entry of the category). Anchor it at the sequence of the event you are waiting past — e.g. the fixup_pushed entry's sequence when waiting for the post-fix-up implement_reviewed verdict"`
	TimeoutSeconds int      `json:"timeout_seconds,omitempty" jsonschema:"how long to wait before returning 'timeout' (default 360, capped at 600). On timeout, re-call with since_sequence = the returned latest_sequence to resume with no gap"`
	// AllowUnknown bypasses the known-category validation (#1764) for a
	// category legitimately absent from the curated registry. Default false:
	// an unknown category is rejected up front (no wait armed) naming the
	// nearest known categories, so a misspelled or wrong-surface string
	// (e.g. the runner-log event 'scope_amendment_pending' vs the audit
	// category 'scope_amendment_requested') can never silently arm an
	// unsatisfiable wait that blocks the full timeout.
	AllowUnknown bool `json:"allow_unknown,omitempty" jsonschema:"bypass the known-category validation for a category not in the curated registry; default false rejects an unknown category up front with the nearest known categories"`
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
  - category        — audit category to wait for. Provide this OR
                      categories (or both — they union). An unknown or
                      misspelled category is REJECTED up front (no wait
                      armed) naming the nearest known categories, so a
                      wrong-surface string like the runner-log event
                      'scope_amendment_pending' (vs the audit category
                      'scope_amendment_requested') can never silently arm
                      an unsatisfiable wait that blocks the full timeout.
  - categories      — OR-semantics list; the wait resolves on the FIRST
                      entry (lowest sequence) matching ANY listed category
                      past the anchor, in one call. Unioned with category.
  - allow_unknown   — bypass the known-category validation for a category
                      legitimately absent from the curated registry
                      (default false).
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
	cats := requestedCategories(in)
	if len(cats) == 0 {
		return nil, AwaitAuditOutput{}, fmt.Errorf("provide at least one non-empty audit category via category or categories (e.g. 'implement_reviewed')")
	}
	if in.SinceSequence < 0 {
		return nil, AwaitAuditOutput{}, fmt.Errorf("since_sequence must be >= 0; got %d", in.SinceSequence)
	}
	// Fail loud on an unknown category BEFORE arming the wait (#1764): a
	// misspelled or wrong-surface string would otherwise block the full
	// timeout on an unsatisfiable wait. allow_unknown bypasses the check.
	if !in.AllowUnknown {
		if err := validateKnownCategories(cats); err != nil {
			return nil, AwaitAuditOutput{}, err
		}
	}
	timeout := clampAwaitTimeout(in.TimeoutSeconds)
	start := time.Now()

	// Fast path: an entry may already exist. The endpoint is
	// sequence-ascending and the anchor filter applies before pagination,
	// so the first entry past the anchor is the answer — and across
	// categories the OR-resolution returns the lowest-sequence hit.
	entry, err := r.nextAuditEntry(ctx, runID, cats, in.SinceSequence, in.AllowUnknown)
	if err != nil {
		return nil, AwaitAuditOutput{}, fmt.Errorf("list audit: %w", err)
	}
	if entry != nil {
		return nil, awaitAuditFoundOutput(entry, in, cats, start), nil
	}

	// Nothing yet: check the run-terminal backstop once before the loop
	// so a run that is already terminal at call time resolves without a
	// poll tick (ADR-036 — the wait never strands past a dead run).
	if out, done := r.awaitAuditRunTerminalBackstop(ctx, runID, in, cats, start); done {
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
			return nil, awaitAuditTimeoutOutput(in, cats, timeout, start), nil
		case <-ticker.C:
			entry, err := r.nextAuditEntry(pollCtx, runID, cats, in.SinceSequence, in.AllowUnknown)
			if err != nil {
				// A deadline hit mid-poll cancels the in-flight request;
				// that is a timeout, not a transport failure — return
				// the gapless re-arm point rather than an error.
				if pollCtx.Err() != nil {
					return nil, awaitAuditTimeoutOutput(in, cats, timeout, start), nil
				}
				return nil, AwaitAuditOutput{}, fmt.Errorf("poll audit: %w", err)
			}
			if entry != nil {
				return nil, awaitAuditFoundOutput(entry, in, cats, start), nil
			}
			if out, done := r.awaitAuditRunTerminalBackstop(pollCtx, runID, in, cats, start); done {
				return nil, out, nil
			}
		}
	}
}

// requestedCategories is the deduplicated, trimmed union of the singular
// Category and the plural Categories inputs, in a stable sorted order.
// Empty/blank entries are dropped, so an all-blank input yields an empty
// slice (the caller rejects it).
func requestedCategories(in AwaitAuditInput) []string {
	seen := make(map[string]struct{})
	for _, c := range append([]string{in.Category}, in.Categories...) {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		seen[c] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for c := range seen {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// validateKnownCategories rejects the first category not in the curated
// registry (#1764) with an actionable error naming the nearest known
// categories, so an unknown/misspelled category never arms a wait.
func validateKnownCategories(cats []string) error {
	for _, c := range cats {
		if !audit.IsKnownCategory(c) {
			return fmt.Errorf("category %q is not a known audit category. Did you mean one of: %s? "+
				"Pass allow_unknown=true to await it anyway",
				c, strings.Join(audit.SuggestCategories(c, 3), ", "))
		}
	}
	return nil
}

// nextAuditEntry fetches the lowest-sequence audit entry for the run
// matching ANY of the given categories with sequence > sinceSeq, or nil
// when none exists yet (#1764 multi-category OR). Per category Limit=1
// suffices because the endpoint is sequence-ascending and the anchor filter
// applies before pagination server-side; the OR-winner is the minimum of
// each category's first past-anchor entry. A single category reduces to the
// prior single-query behavior. allowUnknown threads through so the tool's
// own polling calls for an operator-approved unknown category are not
// re-rejected by the endpoint's known-category validation.
func (r *runResolver) nextAuditEntry(ctx context.Context, runID uuid.UUID, cats []string, sinceSeq int64, allowUnknown bool) (*AuditEntry, error) {
	var best *AuditEntry
	for _, category := range cats {
		entries, _, err := r.api.ListRunAudit(ctx, runID, ListRunAuditFilter{
			Category:      category,
			SinceSequence: sinceSeq,
			Limit:         1,
			AllowUnknown:  allowUnknown,
		})
		if err != nil {
			return nil, err
		}
		if len(entries) == 0 {
			continue
		}
		if best == nil || entries[0].Sequence < best.Sequence {
			e := entries[0]
			best = &e
		}
	}
	return best, nil
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
func (r *runResolver) awaitAuditRunTerminalBackstop(ctx context.Context, runID uuid.UUID, in AwaitAuditInput, cats []string, start time.Time) (AwaitAuditOutput, bool) {
	runRow, err := r.api.GetRun(ctx, runID)
	if err != nil || runRow == nil {
		return AwaitAuditOutput{}, false
	}
	if !runStateIsTerminal(runRow.State) {
		return AwaitAuditOutput{}, false
	}
	// Final read: an entry that landed at/after the terminal transition
	// still resolves as found.
	entry, err := r.nextAuditEntry(ctx, runID, cats, in.SinceSequence, in.AllowUnknown)
	if err == nil && entry != nil {
		return awaitAuditFoundOutput(entry, in, cats, start), true
	}
	return AwaitAuditOutput{
		Status:         "run_terminal",
		LatestSequence: in.SinceSequence,
		WaitedSeconds:  time.Since(start).Seconds(),
		Message: fmt.Sprintf("no %s entry with sequence > %d landed, and run %s has reached terminal state %q — "+
			"the entry will most likely never land, so the wait resolved instead of holding the session open. "+
			"Do not re-arm blindly: check fishhawk_get_run_status for the final run state first.",
			categoriesDisplay(cats), in.SinceSequence, runID, runRow.State),
	}, true
}

// awaitAuditFoundOutput builds the resolved response for a matched entry.
// It applies the compact-by-default projection (#1727) to a COPY of the
// entry so the returned payload has reviewer free_form and issue-context
// body/comments stripped unless the caller opted in — the shared
// backend-fetched AuditEntry is never mutated. LatestSequence is read off
// the entry before the copy, so the anchor semantics are unaffected.
func awaitAuditFoundOutput(entry *AuditEntry, in AwaitAuditInput, _ []string, start time.Time) AwaitAuditOutput {
	projected := *entry
	projected.Payload = compactAuditPayload(entry.Payload, !in.IncludeIssueContext, !in.IncludeReviewProse)
	return AwaitAuditOutput{
		Status:         "found",
		Entry:          &projected,
		LatestSequence: entry.Sequence,
		WaitedSeconds:  time.Since(start).Seconds(),
	}
}

// categoriesDisplay renders the requested categories for a message: a bare
// quoted category for the common single-category wait, or a quoted
// comma-joined list for a multi-category OR wait.
func categoriesDisplay(cats []string) string {
	if len(cats) == 1 {
		return fmt.Sprintf("%q", cats[0])
	}
	quoted := make([]string, len(cats))
	for i, c := range cats {
		quoted[i] = fmt.Sprintf("%q", c)
	}
	return "any of [" + strings.Join(quoted, ", ") + "]"
}

// awaitAuditTimeoutOutput builds the resumable timeout response. The wait
// holds no server state, so a timeout is an idempotent checkpoint, not an
// error. LatestSequence == since_sequence (nothing past the anchor was
// observed — anything observed would have resolved as found), so re-arming
// from it cannot skip an entry.
func awaitAuditTimeoutOutput(in AwaitAuditInput, cats []string, timeout int, start time.Time) AwaitAuditOutput {
	return AwaitAuditOutput{
		Status: "timeout",
		// LatestSequence == the shared since_sequence anchor: nothing past
		// it was observed for ANY requested category (anything observed would
		// have resolved as found), so this single value is the gapless re-arm
		// anchor across ALL categories — the max over every category's
		// past-anchor observations, which is exactly the anchor. Re-arming
		// each category independently from a divergent per-category anchor
		// could skip an entry; one shared anchor cannot.
		LatestSequence:      in.SinceSequence,
		WaitedSeconds:       time.Since(start).Seconds(),
		PollIntervalSeconds: suggestedReviewPollIntervalSeconds,
		Message: fmt.Sprintf("no %s entry with sequence > %d landed within %ds. The wait holds nothing: re-call "+
			"fishhawk_await_audit with since_sequence=%d to resume it with no gap, or poll fishhawk_get_run_status "+
			"every %ds (the authoritative path).",
			categoriesDisplay(cats), in.SinceSequence, timeout, in.SinceSequence, suggestedReviewPollIntervalSeconds),
	}
}
