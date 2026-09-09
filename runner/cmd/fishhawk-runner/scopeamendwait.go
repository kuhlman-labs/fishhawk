package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/kuhlman-labs/fishhawk/runner/internal/agent"
	"github.com/kuhlman-labs/fishhawk/runner/internal/upload"
)

// Scope-amendment settle-wait tuning (E68.42 / #3320). Vars, not consts, so
// tests can shrink them; production callers leave them alone. Mirrors the
// #2748 decision-park block in migrationrenumber.go.
var (
	// scopeAmendmentSettleBudget bounds the WHOLE of
	// awaitPendingScopeAmendmentSettle — the initial probe INCLUDED. The
	// deadline is computed once at function entry, before any network call,
	// so probe time is DEBITED from the budget rather than added to it and a
	// stalled backend can never make the function exceed the bound it
	// advertises.
	//
	// 120s is DELIBERATE and RATIFIED — do NOT change it to 900s. The issue's
	// proposal reads "waits up to the amendment poll window" (900s, the
	// backend's AmendmentPollWindowSeconds, mirrored by
	// migrationRenumberDecisionBudget). 120s ships instead because:
	//   - the agent has ALREADY burned its own ~900s wait-poll on this
	//     amendment INSIDE the invocation, so this is a SETTLE window for a
	//     decision already in flight, not a fresh wait for one;
	//   - the immediately preceding campaign run (#3335) had its implement
	//     stage KILLED by an agent timeout at exactly 3600s, so adding up to
	//     900s of dead wall-clock before every verify loop on a stage that
	//     already has a pending amendment would materially raise that risk
	//     against a 60-minute budget;
	//   - the incident being fixed (run 6d3ba9fe) was an approval landing
	//     inside ONE minute.
	// The 120–900s gap is a KNOWN, ACCEPTED residual: an approval landing in
	// that window still takes the #2601 undecided path, which handles it
	// correctly and safely. See runner/cmd/fishhawk-runner/README.md.
	scopeAmendmentSettleBudget = 120 * time.Second
	// scopeAmendmentSettleWaitSeconds is the per-request `?wait=N` long-poll
	// hold the backend applies (#2748's primitive).
	scopeAmendmentSettleWaitSeconds = 30
	// scopeAmendmentSettlePollInterval is the pause between long-polls. It
	// matters only when the server returns immediately (no pending row to
	// hold on, or a fake in tests); the real path spends its time inside the
	// long-poll.
	scopeAmendmentSettlePollInterval = 2 * time.Second
)

// boundedWaitSeconds is the `?wait=N` hold for one long-poll: the per-request
// cap, FLOORED to the whole seconds left in the budget so the server is never
// asked to hold past it. A sub-second or non-positive remainder yields 0,
// which omits the query parameter entirely and makes the request return
// immediately — the caller's loop then observes the expired deadline.
//
// Shared by the #2748 decision park (boundedRenumberWaitSeconds delegates
// here) and the #3320 settle wait, so the bound-both-ways logic the #2748
// fix-up established has ONE definition.
func boundedWaitSeconds(remaining time.Duration, cap int) int {
	if remaining <= 0 {
		return 0
	}
	secs := int(remaining / time.Second)
	if secs > cap {
		return cap
	}
	return secs
}

// awaitPendingScopeAmendmentSettle is a bounded pause immediately BEFORE the
// mid-stage amendment fold (#961) at agent exit, closing the race between an
// operator's approval and the runner's committed-tree verify gates (#3320).
//
// The incident (run 6d3ba9fe): the runner does exactly ONE amendment fetch at
// agent exit and then immediately runs the verify-fix loop (#651) or the
// single-shot committed gate (#802). An approval landing SECONDS after that
// single fetch is invisible to the fold, so the gates run against a tree that
// excludes the approved file — and the same still-pending row then trips the
// #2601 undecided detection, which withholds the fix-loop reinvoke, so the
// stage cannot even self-correct.
//
// So: when this stage has an amendment still `pending` at agent exit, long-poll
// GET /v0/runs/{id}/scope-amendments (the #2748 `?wait=N` primitive) until every
// pending row for THIS stage is decided, the settle budget elapses, the fetch
// fails, or the context is cancelled — and only then let the fold read the list.
//
// THE BUDGET BOUNDS THE WHOLE FUNCTION, PROBE INCLUDED. The deadline is computed
// ONCE at entry, after the no-op guards and before any network call, and the
// initial no-wait probe derives its context from that same deadline exactly as
// each loop iteration does. A stalled or hung probe therefore cannot exceed
// scopeAmendmentSettleBudget before the bounded loop is even entered.
//
// Takes cfg BY VALUE — deliberately unlike refreshScopeAmendments(*config).
// The settle wait must not be able to mutate the scope set: the fold that runs
// immediately after remains the SINGLE writer of cfg.scopeFiles, so this
// function can change only WHEN the fold observes the amendment list, never
// WHAT is committed, and the #960 verified-tree invariant is untouched.
//
// EVERY degrade is fail-open: the stage proceeds exactly as it does today. The
// wait never sets res.OK=false and never carries a failure category.
func awaitPendingScopeAmendmentSettle(ctx context.Context, client uploadClient, cfg config, mcpToken, stageType string, logSink io.Writer) []agent.Event {
	// No-op guards: no fetch and no clock read at all. The empty-stageID guard
	// mirrors undecidedScopeAmendmentsForStage's — a row whose StageID is also
	// empty would otherwise match by accident. A non-positive budget is the
	// tuning off-switch.
	if client == nil || mcpToken == "" || stageType != "implement" || cfg.stageID == "" || scopeAmendmentSettleBudget <= 0 {
		return nil
	}

	// The one deadline. Computed BEFORE any network call so the advertised
	// total blocking bound is actually total.
	deadline := time.Now().Add(scopeAmendmentSettleBudget)
	started := time.Now()

	checkFailed := func(outcome, detail string) []agent.Event {
		_, _ = fmt.Fprintf(logSink,
			`{"event":"scope_amendment_settle_check_failed","run_id":%q,"stage_id":%q,"outcome":%q,"detail":%q}`+"\n",
			cfg.runID, cfg.stageID, outcome, detail)
		return nil
	}

	// Initial probe: byte-identical to today's fold request in its ARGS
	// (WaitSeconds stays 0), but carrying the entry-computed deadline.
	probeCtx, cancelProbe := context.WithDeadline(ctx, deadline)
	items, err := client.FetchScopeAmendments(probeCtx, upload.FetchScopeAmendmentsArgs{
		RunID:    cfg.runID,
		MCPToken: mcpToken,
	})
	cancelProbe()
	if err != nil {
		// Classify by CAUSE, not by the clock (the #2748 discrimination): only
		// OUR derived deadline firing, with the parent context still healthy,
		// means the budget ended the request rather than a broken backend.
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			return checkFailed("timeout", err.Error())
		}
		return checkFailed("unavailable", err.Error())
	}
	pending := undecidedScopeAmendmentsForStage(items, cfg.stageID)
	if len(pending) == 0 {
		// Nothing to settle. Return with no log line and no policy_event,
		// mirroring refreshScopeAmendments's nil-on-no-op posture so the
		// ordinary stage log is untouched.
		return nil
	}

	ids := scopeAmendmentIDs(pending)
	idsJSON, _ := json.Marshal(ids)
	_, _ = fmt.Fprintf(logSink,
		`{"event":"scope_amendment_settle_wait_started","run_id":%q,"stage_id":%q,"amendments":%s,"budget_seconds":%d}`+"\n",
		cfg.runID, cfg.stageID, idsJSON, int(scopeAmendmentSettleBudget/time.Second))

	outcome, ids := settleLoop(ctx, client, cfg, mcpToken, deadline, ids)

	// waited_ms is measured from the entry-computed deadline's origin, so it
	// INCLUDES the probe.
	waitedMS := time.Since(started).Milliseconds()
	idsJSON, _ = json.Marshal(ids)
	_, _ = fmt.Fprintf(logSink,
		`{"event":"scope_amendment_settle_wait_ended","run_id":%q,"stage_id":%q,"outcome":%q,"waited_ms":%d,"amendments":%s}`+"\n",
		cfg.runID, cfg.stageID, outcome, waitedMS, idsJSON)
	// Mirror the scope_drift / scope_amendments_folded policy_event shape so
	// the record rides into BOTH bundle variants (PackBytes / redactEvents).
	return []agent.Event{{
		Kind: "policy_event",
		Payload: agent.MakePayload(map[string]any{
			"check":      "scope_amendment_settle_wait",
			"outcome":    outcome,
			"amendments": ids,
			"waited_ms":  waitedMS,
		}),
	}}
}

// settleLoop long-polls until no pending row for this stage remains, the budget
// expires, a fetch fails, or the parent context is cancelled. Returns the
// outcome classified by CAUSE (decided / timeout / unavailable) and the
// amendment ids the wait was about.
//
// Bound BOTH ways, as awaitMigrationRenumberDecision is: the per-request
// `?wait=N` hold is capped at the whole seconds remaining, AND each request
// carries a context deadline at the entry-computed deadline, so a backend that
// ignores `?wait` entirely degrades to a polling loop that still terminates on
// time.
func settleLoop(ctx context.Context, client uploadClient, cfg config, mcpToken string, deadline time.Time, ids []string) (string, []string) {
	for {
		if err := ctx.Err(); err != nil {
			return "unavailable", ids
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return "timeout", ids
		}
		fetchCtx, cancel := context.WithDeadline(ctx, deadline)
		items, err := client.FetchScopeAmendments(fetchCtx, upload.FetchScopeAmendmentsArgs{
			RunID:       cfg.runID,
			MCPToken:    mcpToken,
			WaitSeconds: boundedWaitSeconds(remaining, scopeAmendmentSettleWaitSeconds),
		})
		cancel()
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
				return "timeout", ids
			}
			return "unavailable", ids
		}
		pending := undecidedScopeAmendmentsForStage(items, cfg.stageID)
		if len(pending) == 0 {
			// An APPROVED and a DENIED row both end the wait: a denial IS a
			// decision, and burning the rest of the budget on it is pure cost.
			return "decided", ids
		}
		ids = scopeAmendmentIDs(pending)
		remaining = time.Until(deadline)
		if remaining <= 0 {
			return "timeout", ids
		}
		pause := scopeAmendmentSettlePollInterval
		if pause > remaining {
			pause = remaining
		}
		select {
		case <-ctx.Done():
			return "unavailable", ids
		case <-time.After(pause):
		}
	}
}

// scopeAmendmentIDs projects the amendment ids, in order.
func scopeAmendmentIDs(items []upload.ScopeAmendment) []string {
	out := make([]string, 0, len(items))
	for _, a := range items {
		out = append(out, a.ID)
	}
	return out
}
