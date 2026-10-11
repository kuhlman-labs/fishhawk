package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// fishhawk_rerun_review (#4219) is the operator-reachable wrapper over
// POST /v0/runs/{run_id}/reviews/rerun: it re-runs a run's latest
// implement-review round when that round SETTLED WITH ZERO VERDICTS (every
// reviewer failed or was skipped). It is the recovery for the decomposed-parent
// review-coverage hold (review_coverage_incomplete): a round with no verdict
// covers no slice, and before this verb nothing could dispatch a second review
// of the SAME head. Every refusal, the audit row and the dedup bypass live
// server-side (backend/internal/server/review_rerun.go); this tool validates
// the run id, passes the result through and names the follow-up wait.

// RerunReviewInput is the fishhawk_rerun_review input schema.
type RerunReviewInput struct {
	RunID string `json:"run_id" jsonschema:"the Fishhawk run UUID whose latest implement-review round settled with no verdict (for a decomposed parent, the PARENT run named by review_coverage_incomplete)"`
}

// RerunReviewResult mirrors the 200 body of POST /v0/runs/{run_id}/reviews/rerun
// (backend/internal/server/review_rerun.go::rerunReviewResponse). Every json
// tag MUST byte-match or the field silently decodes to its zero value (the
// #371 trap).
type RerunReviewResult struct {
	RunID            string `json:"run_id"`
	Stage            string `json:"stage" jsonschema:"the review-bearing stage the re-run targets: implement"`
	StageID          string `json:"stage_id" jsonschema:"the implement stage the re-run round reviews"`
	RerunOfSequence  int64  `json:"rerun_of_sequence" jsonschema:"the audit sequence of the verdict-less implement_review_started round that was re-run"`
	NewRoundSequence int64  `json:"new_round_sequence" jsonschema:"the audit sequence of the NEW implement_review_started round this call dispatched (its rerun_of names rerun_of_sequence)"`
	ConfiguredAgents int    `json:"configured_agents" jsonschema:"how many agent reviewers the re-run round was dispatched with"`
}

// RerunReviewOutput is the tool response.
type RerunReviewOutput struct {
	Result     RerunReviewResult `json:"result"`
	Message    string            `json:"message" jsonschema:"what was re-run and what to do next"`
	NextAction *SuggestedAction  `json:"next_action,omitempty" jsonschema:"the follow-up wait: fishhawk_await_review on the implement stage, whose verdict clears a review_coverage_incomplete hold"`
}

// RerunReview re-runs the run's latest verdict-less implement-review round via
// POST /v0/runs/{run_id}/reviews/rerun (#4219). It lives here rather than in
// client.go, like resolve_concerns.go's method. 4xx/5xx surface as *apiError:
//   - 400 validation_failed (bad UUID)
//   - 401 authentication_required
//   - 403 run_token_forbidden, operator_agent_forbidden, insufficient_scope or
//     account_forbidden
//   - 404 run_not_found
//   - 409 no_review_round, review_round_in_flight, review_round_has_verdict or
//     review_rerun_ineligible (details.reason)
//   - 500 review_rerun_check_failed / review_rerun_audit_failed (nothing re-run)
//   - 502 review_rerun_not_started (recorded, but no round started)
//   - 503 review_rerun_unconfigured
func (c *apiClient) RerunReview(ctx context.Context, runID uuid.UUID) (*RerunReviewResult, error) {
	var res RerunReviewResult
	if err := c.do(ctx, http.MethodPost, "/v0/runs/"+runID.String()+"/reviews/rerun", nil, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// registerRerunReview wires the fishhawk_rerun_review tool (#4219).
//
// Auth: operator write tool. The endpoint requires write:runs and refuses a
// run-bound agent token and a delegated operator-agent token, so it cannot be
// called from inside a run or delegated.
func registerRerunReview(srv *mcp.Server, resolver *runResolver) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "fishhawk_rerun_review",
		Description: strings.TrimSpace(`
Use this when a run's latest implement-review round SETTLED WITH NO VERDICT —
every configured reviewer failed or was skipped (provider outage, quota) — and
you need a real agent review of the same head. The signature is
review_coverage_incomplete on a decomposed parent (fishhawk_merge_run status,
next_actions state, or a fishhawk_get_gate_view merge_readiness blocker whose
details.next_step is fishhawk_rerun_review): a round with no verdict covers no
slice, so the merge stays refused until a round records one.

It re-runs the run's LATEST implement-review round against the round's
recorded source (its head and base), appends a review_round_rerun_requested
audit row before dispatching, and starts a new round whose
implement_review_started carries rerun_of = the re-run round's sequence.
Advisory reviewers then run detached: await them with fishhawk_await_review
(stage=implement). A successful verdict clears the review-coverage hold.

ELIGIBILITY (each refusal is decided before anything is written):
  - the run has an implement-review round (no_review_round otherwise)
  - every configured reviewer of that round has settled and no re-run or
    boot re-dispatch of it is pending (review_round_in_flight otherwise —
    await it; a round orphaned by a fishhawkd restart is closed with
    fishhawk_reconcile_reviews first)
  - the round recorded ZERO verdicts (review_round_has_verdict otherwise)
  - the round is advisory, a reviewer backend is wired, its source is
    known and still current (not superseded by a fix-up, the implement stage
    not terminal, the run not advanced past review) and its inputs are
    readable (trace bundle, forge compare) — review_rerun_ineligible
    otherwise; details.reason names which
A HUMAN operator only: the backend refuses a run-bound agent token
(run_token_forbidden) and a delegated operator-agent token
(operator_agent_forbidden).

A re-run against a provider that is still unavailable (e.g. quota exhausted)
fails again with another verdict-less round, which can itself be re-run.

Input:
  - run_id (required) — the run UUID (for a decomposed parent, the PARENT).

Response: {result{run_id, stage, stage_id, rerun_of_sequence,
new_round_sequence, configured_agents}, message, next_action}.

Tool errors (the backend refusal verbatim, plus a remedy hint):
  - invalid UUID (caught before the HTTP hop)
  - run_not_found (404)
  - run_token_forbidden / operator_agent_forbidden / insufficient_scope (403)
  - no_review_round / review_round_in_flight / review_round_has_verdict /
    review_rerun_ineligible (409)
  - review_rerun_not_started (502 — the request was recorded but the round's
    diff could not be rebuilt; details.reason names the failure)
`),
	}, resolver.rerunReview)
}

// rerunReview is the tool handler.
func (r *runResolver) rerunReview(ctx context.Context, _ *mcp.CallToolRequest, in RerunReviewInput) (*mcp.CallToolResult, RerunReviewOutput, error) {
	runUUID, err := uuid.Parse(in.RunID)
	if err != nil {
		return nil, RerunReviewOutput{}, fmt.Errorf("run_id %q is not a valid UUID: %w", in.RunID, err)
	}
	res, err := r.api.RerunReview(ctx, runUUID)
	if err != nil {
		var ae *apiError
		if errors.As(err, &ae) {
			if hint := rerunReviewRefusalHint(ae.Code); hint != "" {
				return nil, RerunReviewOutput{}, fmt.Errorf("rerun review: %w — %s", err, hint)
			}
		}
		return nil, RerunReviewOutput{}, fmt.Errorf("rerun review: %w", err)
	}
	if res.RunID == "" {
		res.RunID = runUUID.String()
	}
	return nil, RerunReviewOutput{
		Result: *res,
		Message: "re-ran implement-review round " + strconv.FormatInt(res.RerunOfSequence, 10) +
			" as round " + strconv.FormatInt(res.NewRoundSequence, 10) + " with " + strconv.Itoa(res.ConfiguredAgents) +
			" configured reviewer(s). Await its verdict with fishhawk_await_review (stage=implement); a recorded verdict clears a review_coverage_incomplete hold, a round whose reviewers fail again can be re-run again.",
		NextAction: &SuggestedAction{
			Action:       "fishhawk_await_review",
			Params:       map[string]string{"run_id": res.RunID, "stage": "implement"},
			Precondition: "the re-run implement-review round was dispatched",
			Consumes:     consumesNone,
			Reason:       "await the re-run round's verdict; a successful agent review clears the review-coverage hold",
		},
	}, nil
}

// rerunReviewRefusalHint names the verb that resolves each re-run refusal, so
// the tool error tells the operator what to do instead of only what failed.
// Empty for a code with no specific remedy (the error is passed through
// verbatim). Pure so a table test pins it.
func rerunReviewRefusalHint(code string) string {
	switch code {
	case "no_review_round":
		return "no implement review was ever dispatched for this run; there is nothing to re-run"
	case "review_round_in_flight":
		return "await the round with fishhawk_await_review (stage=implement); a round orphaned by a fishhawkd restart is closed with fishhawk_reconcile_reviews before it can be re-run"
	case "review_round_has_verdict":
		return "the round already recorded a reviewer verdict, so it needs no re-run; read it with fishhawk_get_gate_view"
	case "review_rerun_ineligible":
		return "details.reason names why this round cannot be re-run; a superseded or advanced round is reviewed by its successor, not re-run"
	case "review_rerun_not_started":
		return "the request was recorded (review_round_rerun_requested) but no round started; fix the cause named in details.reason and call fishhawk_rerun_review again"
	}
	return ""
}
