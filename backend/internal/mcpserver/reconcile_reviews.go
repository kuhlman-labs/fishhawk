package mcpserver

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// fishhawk_reconcile_reviews (E67.55 / #2712) is the operator-reachable
// wrapper over POST /v0/runs/{run_id}/reviews/reconcile — the on-demand twin
// of the orphaned-review recovery fishhawkd runs once at boot (#1781).
//
// WHY A VERB IS NEEDED. When fishhawkd restarts while a plan or implement
// review is in flight, the reviewing goroutine dies with the process and no
// terminal *_review_(reviewed|skipped|failed) entry ever lands. review_status
// is derived from the audit trail, so the round stays 'pending' forever: the
// approval/merge gate never settles and fishhawk_await_review used to burn its
// full timeout. The boot sweep heals exactly this — but only AT BOOT, so an
// operator whose review was already orphaned had to restart the daemon a
// second time. This verb runs the same per-run helper on demand.

// ReconcileReviewsInput is the tool's input schema.
type ReconcileReviewsInput struct {
	RunID string `json:"run_id" jsonschema:"the Fishhawk run UUID whose orphaned review round should be terminated"`
}

// ReconcileReviewsStage is one per-stage row of the result.
type ReconcileReviewsStage struct {
	Stage            string `json:"stage" jsonschema:"the review-bearing stage: plan or implement"`
	ConfiguredAgents int    `json:"configured_agents" jsonschema:"how many agent reviewers the current round was dispatched with"`
	LandedBefore     int    `json:"landed_before" jsonschema:"how many terminal verdicts had already landed for the round before this call; these are preserved, never re-paid for"`
	Synthesized      int    `json:"synthesized" jsonschema:"how many terminal *_review_failed entries this call appended (configured_agents minus landed_before); 0 when nothing needed healing"`
	Skipped          bool   `json:"skipped,omitempty" jsonschema:"true when this stage healed nothing"`
	SkipReason       string `json:"skip_reason,omitempty" jsonschema:"why nothing was healed: no_review_started_entry, no_configured_agents, started_entry_has_no_stage_id, round_already_settled, or review_dispatched_by_this_process (the live-review refusal)"`
}

// ReconcileReviewsOutput is the tool response.
type ReconcileReviewsOutput struct {
	RunID      string                  `json:"run_id"`
	Terminated bool                    `json:"terminated" jsonschema:"true when any stage's orphaned round was terminated by this call"`
	Stages     []ReconcileReviewsStage `json:"stages"`
	Message    string                  `json:"message" jsonschema:"what was synthesized, or why nothing was"`
}

// registerReconcileReviews wires the fishhawk_reconcile_reviews tool.
//
// Auth: operator write tool. The endpoint requires write:runs; a token without
// it surfaces 403 insufficient_scope verbatim.
func registerReconcileReviews(srv *mcp.Server, resolver *runResolver) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "fishhawk_reconcile_reviews",
		Description: strings.TrimSpace(`
Use this when a plan or implement review is STRANDED because fishhawkd
restarted while it was in flight — the signature is fishhawk_await_review
returning status "stranded", or a review_status stuck on "pending" with fewer
landed verdicts than configured_agents and no reviewer process left alive.

It terminates the orphaned round by appending ONLY the MISSING terminal
*_review_failed entries, so the count-gated completeness rule
(landed_terminal >= configured_agents) is satisfied and the gate settles.

What it preserves. Verdicts that already landed are durable audit entries and
are never re-paid for or overwritten: a round with 1 of 2 reviewers landed gets
exactly 1 synthesized entry, and the real verdict — including its concerns —
still reads back verbatim at the gate. Calling it twice is a no-op
(skip_reason=round_already_settled).

What it refuses. It will NOT terminate a review the SERVING daemon still has in
flight: the server compares the round's dispatch timestamp against its own boot
instant and answers skip_reason=review_dispatched_by_this_process. So invoking
it against a healthy, genuinely-running review changes nothing.

Input:
  - run_id (required) — the Fishhawk run UUID.

Response: {run_id, terminated, stages[{stage, configured_agents, landed_before,
synthesized, skipped, skip_reason}], message}.

Tool errors:
  - invalid UUID (caught before the HTTP hop)
  - run_not_found (404)
  - insufficient_scope (403 — the token lacks write:runs)
`),
	}, resolver.reconcileReviews)
}

// reconcileReviews is the tool handler.
func (r *runResolver) reconcileReviews(ctx context.Context, _ *mcp.CallToolRequest, in ReconcileReviewsInput) (*mcp.CallToolResult, ReconcileReviewsOutput, error) {
	runUUID, err := uuid.Parse(in.RunID)
	if err != nil {
		return nil, ReconcileReviewsOutput{}, fmt.Errorf("run_id %q is not a valid UUID: %w", in.RunID, err)
	}
	res, err := r.api.ReconcileRunReviews(ctx, runUUID)
	if err != nil {
		return nil, ReconcileReviewsOutput{}, fmt.Errorf("reconcile reviews: %w", err)
	}
	out := ReconcileReviewsOutput{RunID: res.RunID, Terminated: res.Terminated}
	if out.RunID == "" {
		out.RunID = runUUID.String()
	}
	for _, st := range res.Stages {
		// The wire row and the tool row are structurally identical today; the
		// conversion keeps them INDEPENDENT types so a future field on either
		// side is a compile error here rather than a silent drop.
		out.Stages = append(out.Stages, ReconcileReviewsStage(st))
	}
	out.Message = reconcileReviewsMessage(out)
	return nil, out, nil
}

// reconcileReviewsMessage renders the operator-facing summary: what was
// synthesized per stage (naming the preserved landed verdicts explicitly), or
// — when nothing was — the reason each stage gave. Pure so a table test pins
// it.
func reconcileReviewsMessage(out ReconcileReviewsOutput) string {
	var healed, skipped []string
	for _, st := range out.Stages {
		if st.Synthesized > 0 {
			healed = append(healed, fmt.Sprintf(
				"%s: synthesized %d terminal entr%s to complete the round (%d of %d reviewers had already landed and are preserved)",
				st.Stage, st.Synthesized, plural(st.Synthesized, "y", "ies"), st.LandedBefore, st.ConfiguredAgents))
			continue
		}
		reason := st.SkipReason
		if reason == "" {
			reason = "nothing to heal"
		}
		skipped = append(skipped, fmt.Sprintf("%s: %s", st.Stage, reason))
	}
	if len(healed) == 0 {
		return "no orphaned review round was terminated — " + strings.Join(skipped, "; ") +
			". A review this daemon still has in flight is refused on purpose (review_dispatched_by_this_process); " +
			"an already-settled round is an idempotent no-op."
	}
	msg := strings.Join(healed, "; ") +
		". The review gate can now settle: re-read it with fishhawk_get_run_status or fishhawk_get_gate_view."
	if len(skipped) > 0 {
		msg += " Untouched — " + strings.Join(skipped, "; ") + "."
	}
	return msg
}

// plural picks the singular or plural suffix for n.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
