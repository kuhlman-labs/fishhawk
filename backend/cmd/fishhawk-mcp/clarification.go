package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// AnswerClarificationInput is the fishhawk_answer_clarification tool's
// input schema (#1088). Takes a run id (matching the next_actions
// awaiting_input arm's params); the tool resolves the run's parked plan
// stage server-side, so the operator never handles a raw stage id.
type AnswerClarificationInput struct {
	RunID   string                `json:"run_id" jsonschema:"the Fishhawk run UUID whose plan stage parked at awaiting_input with a clarification_request"`
	Answers []ClarificationAnswer `json:"answers" jsonschema:"the operator's answers to the parked questions; one {id, answer} per question, keyed by the question id from the clarification_requested audit entry. At least one required"`
	Comment string                `json:"comment,omitempty" jsonschema:"optional free-text note appended after the answers in the binding conditions delivered to the resumed plan agent"`
}

// AnswerClarificationOutput carries the re-opened plan Stage plus the
// resolved plan-stage UUID, so the response makes the run→stage
// resolution visible (mirrors ApprovePlanOutput).
type AnswerClarificationOutput struct {
	Stage   Stage  `json:"stage"`
	StageID string `json:"stage_id" jsonschema:"the resolved plan-stage UUID the answers were posted to"`
}

// registerAnswerClarification wires the fishhawk_answer_clarification tool
// (#1088, the #1057 slice-4/5 answer-and-resume seam). Resolves the plan
// stage from the run id, then posts the operator's answers via
// POST /v0/stages/{stage_id}/clarification.
//
// Auth: a write tool — operator-side fhk_* tokens with scope
// `write:approvals` (the #558 gate-answer family). A run-bound runner
// token's `mcp:read` scope cannot authorize it.
func registerAnswerClarification(srv *mcp.Server, resolver *runResolver) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "fishhawk_answer_clarification",
		Description: strings.TrimSpace(`
Answer the questions a planner parked at awaiting_input so its plan stage
can resume. Use this when fishhawk_get_run_status reports a plan stage
parked at awaiting_input by a clarification_request (#1080/#1057): the
issue was not yet plannable, so the planner asked questions instead of
producing a plan, and the run is stranded until you answer.

Read the parked questions first (fishhawk_get_run_status's recent audit,
or fishhawk_list_audit on category clarification_requested) — each carries
an id. Pass one {id, answer} per question; every parked question needs
exactly one answer, and an unknown/duplicate id is rejected. The answers
are recorded as a dedicated clarification_answered audit entry (NOT an
approval — the plan is not yet approved) and injected into the resumed
plan agent's binding conditions, then the SAME plan stage re-opens
(awaiting_input → pending) in the SAME run — no new run, no duplicate
reviews (distinct from fishhawk_resume_run, which mints a child run).

Takes a run id; the tool resolves the plan stage internally. On a
github_actions/drive run the backend re-dispatches the plan stage; on a
local run, re-run it with fishhawk_run_stage plan after this returns.

Common error shapes (surfaced as tool errors):
  - "this plan stage is not parked at awaiting_input" — the stage is not a
    plan stage in awaiting_input (409 invalid_state_transition)
  - "clarification_answer_invalid" — an answer id is unknown, missing, or
    duplicated relative to the parked questions (400)
`),
	}, resolver.answerClarification)
}

// answerClarification is the tool handler. Resolves the plan stage from
// the run id (reusing resolvePlanStage), then POSTs the answers; the
// backend enforces the plan/awaiting_input gate and the answer-id
// validation, whose codes are mapped onto operator-actionable tool errors.
func (r *runResolver) answerClarification(ctx context.Context, _ *mcp.CallToolRequest, in AnswerClarificationInput) (*mcp.CallToolResult, AnswerClarificationOutput, error) {
	if len(in.Answers) == 0 {
		return nil, AnswerClarificationOutput{}, fmt.Errorf(
			"answers must contain at least one {id, answer}; read the parked questions via fishhawk_get_run_status or fishhawk_list_audit (category clarification_requested) first")
	}

	planStage, err := r.resolvePlanStage(ctx, in.RunID)
	if err != nil {
		return nil, AnswerClarificationOutput{}, err
	}
	stageID, err := uuid.Parse(planStage.ID)
	if err != nil {
		return nil, AnswerClarificationOutput{}, fmt.Errorf("resolved plan stage has invalid id %q: %w", planStage.ID, err)
	}

	updated, err := r.api.AnswerClarification(ctx, stageID, in.Answers, in.Comment)
	if err != nil {
		// Map the backend's gate codes onto operator-actionable tool
		// errors rather than a generic wrap.
		var ae *apiError
		if errors.As(err, &ae) {
			switch ae.Code {
			case "invalid_state_transition":
				return nil, AnswerClarificationOutput{}, fmt.Errorf(
					"invalid_state_transition: this plan stage is not parked at awaiting_input (stage_type=%v stage_state=%v) — clarification answers are accepted only for a plan stage the planner parked with a clarification_request; re-check fishhawk_get_run_status",
					ae.Details["stage_type"], ae.Details["stage_state"])
			case "clarification_answer_invalid":
				return nil, AnswerClarificationOutput{}, fmt.Errorf(
					"clarification_answer_invalid: %s — every parked question needs exactly one answer keyed by its id; read the clarification_requested audit entry's questions (fishhawk_get_run_status / fishhawk_list_audit)", ae.Message)
			case "validation_failed":
				return nil, AnswerClarificationOutput{}, fmt.Errorf("validation_failed: %s", ae.Message)
			case "stage_not_found":
				return nil, AnswerClarificationOutput{}, fmt.Errorf(
					"stage_not_found: the resolved plan stage %s no longer exists", stageID)
			}
		}
		return nil, AnswerClarificationOutput{}, fmt.Errorf("answer clarification: %w", err)
	}
	return nil, AnswerClarificationOutput{Stage: *updated, StageID: stageID.String()}, nil
}
