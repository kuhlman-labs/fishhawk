package mcpserver

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The MCP half of the #3083/#3136 merge-recovery verb PAIR (E45.88 / #3623).
//
// WHY THESE TOOLS EXIST. `completion_blocked.recovery` on GET /v0/runs/{id}
// (backend/internal/server/runs.go) hands an operator a CLOSED three-value verb
// set — `record-merge-observation`, `reconcile-merge`, `none`. Both non-`none`
// values name a shipped REST route, and neither had a registered MCP tool: an
// MCP-driven agent was told the precise remedy and could not reach it, falling
// back to raw curl (run f199dcf1). These two tools close that, and the drift
// guard in backend/internal/server/mcproute_test.go keeps the discriminator's
// vocabulary and this registry from separating again.
//
// THE OBSERVE/SETTLE SPLIT is the load-bearing thing to understand, and it is
// why BOTH verbs are registered together rather than one at a time:
//
//   - fishhawk_record_merge_observation READS the forge and SETTLES NOTHING. It
//     appends the one merge_observation_recorded evidence row and stops.
//   - fishhawk_reconcile_merge settles the run and NEVER RE-READS THE FORGE. Its
//     evidence gate reads the audit CHAIN only.
//
// So a run whose PR genuinely merged but whose merge was never observed is
// unreachable by reconcile-merge alone; the discriminator hands out ONE verb OR
// THE OTHER depending on which half is missing, and registering only one would
// leave the identical gap for the other arm.

// RecordMergeObservationInput is the observe tool's input schema.
type RecordMergeObservationInput struct {
	RunID string `json:"run_id" jsonschema:"the Fishhawk run UUID whose pull-request merge should be read off the forge and recorded"`
}

// RecordMergeObservationObservation is the fact the verb recorded.
type RecordMergeObservationObservation struct {
	PullRequestURL    string `json:"pull_request_url,omitempty" jsonschema:"the pull request the observation was read from"`
	PullRequestNumber int    `json:"pull_request_number,omitempty" jsonschema:"that pull request's number"`
	MergeCommitSHA    string `json:"merge_commit_sha,omitempty" jsonschema:"the forge's merge commit SHA; the endpoint refuses rather than record an observation without one"`
	MergedAt          string `json:"merged_at,omitempty" jsonschema:"the FORGE's merge timestamp — when the merge happened"`
	ObservedAt        string `json:"observed_at,omitempty" jsonschema:"when Fishhawk READ it — when Fishhawk learned the merge. Deliberately distinct from merged_at: nothing is back-dated, so a reader sees the gap"`
}

// RecordMergeObservationOutput is the observe tool's response.
type RecordMergeObservationOutput struct {
	RunID           string                            `json:"run_id"`
	AlreadyRecorded bool                              `json:"already_recorded" jsonschema:"true when the chain already carried qualifying merge evidence and this call appended NOTHING; the observation block is then EMPTY, because the response must not claim a row it did not write"`
	Observation     RecordMergeObservationObservation `json:"observation" jsonschema:"the fact this call recorded; empty on the already_recorded no-op arm"`
	Message         string                            `json:"message" jsonschema:"what was recorded, or that nothing was, and which verb comes next"`
}

// ReconcileMergeInput is the settle tool's input schema.
type ReconcileMergeInput struct {
	RunID string `json:"run_id" jsonschema:"the Fishhawk run UUID whose merge-parked stages should be superseded so the run can complete"`
}

// ReconcileMergeStage is one stage the reconcile moved (or repaired).
type ReconcileMergeStage struct {
	StageID   string `json:"stage_id"`
	StageType string `json:"stage_type"`
	FromState string `json:"from_state" jsonschema:"the park state the stage was moved out of"`
	Reason    string `json:"reason"`
}

// ReconcileMergeOutput is the settle tool's response.
type ReconcileMergeOutput struct {
	RunID      string                `json:"run_id"`
	Superseded []ReconcileMergeStage `json:"superseded" jsonschema:"the stages THIS call moved to superseded; empty on an idempotent repeat"`
	Repaired   []ReconcileMergeStage `json:"repaired" jsonschema:"stages that were already superseded but carried no audit row and got one back; empty on an idempotent repeat"`
	RunState   string                `json:"run_state" jsonschema:"the run's lifecycle state AFTER the completion re-evaluation — this is how you see whether the reconcile actually settled the run"`
	Message    string                `json:"message" jsonschema:"what was superseded/repaired, or why nothing was, and whether the run settled"`
}

// registerRecordMergeObservation wires the fishhawk_record_merge_observation
// tool.
//
// Auth: TWO gates. The endpoint is registered requireRunAccount(memberWrite)
// for account ownership, and handleRecordMergeObservation itself enforces
// requireWriteScope("write:runs") as its rung 0 (E45.95 / #3635) — the same
// scope the sibling operator recovery verbs enforce. The /mcp gate mirrors that
// with {anyOf: ["write:runs"]} and NO runBoundSubjectOK, because the handler
// does not authorize a run-bound token by subject (see
// backend/internal/server/mcpscopes.go).
func registerRecordMergeObservation(srv *mcp.Server, resolver *runResolver) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "fishhawk_record_merge_observation",
		Description: strings.TrimSpace(`
Use this when fishhawk_get_run_status reports
run.completion_blocked.recovery = "record-merge-observation" — a 'running' run
held open by a merge-supersedable parked stage whose pull-request merge Fishhawk
has never observed. It is the FIRST of the two merge-recovery verbs, and
fishhawk_reconcile_merge is the second: observe, THEN reconcile.

WHAT IT DOES, and what it deliberately does NOT. It reads the run's pull request
off the forge and, only on a live merged=true answer carrying BOTH a merge commit
SHA and a merge timestamp, appends ONE merge_observation_recorded audit row. It
SETTLES NOTHING — no stage moves, no run completes. Its whole job is to supply
the chain evidence fishhawk_reconcile_merge's gate reads, because that verb NEVER
re-reads the forge. After this succeeds, completion_blocked.recovery flips to
"reconcile-merge"; call that next.

Nothing is back-dated: the row carries the forge's own merged_at AND this
observation's observed_at, so a reader sees the gap.

Idempotent. A repeat answers already_recorded:true having appended NOTHING, and
the observation block is EMPTY on that arm on purpose — the response must not
claim a row it did not write.

Input:
  - run_id (required) — the Fishhawk run UUID.

Response: {run_id, already_recorded, observation{pull_request_url,
pull_request_number, merge_commit_sha, merged_at, observed_at}, message}.

Named refusals, each surfacing the backend's code verbatim so you can act on it:
  - record_merge_observation_no_pull_request (409) — the run carries no PR URL.
  - record_merge_observation_malformed_pr_url (409) — the recorded URL does not parse.
  - record_merge_observation_pr_url_repo_mismatch (409) — the URL does not name
    this run's repository on this run's forge family.
  - record_merge_observation_pr_not_merged (409) — the forge says NOT merged;
    recording would manufacture evidence for a change that never shipped.
  - record_merge_observation_no_merge_commit (409) — merged but no commit SHA.
  - record_merge_observation_no_merge_timestamp (409) — merged but no timestamp.
  - record_merge_observation_forge_unavailable (502) — the forge read failed, so
    the merge state is UNKNOWN and nothing was recorded.
  - record_merge_observation_unconfigured (503) — run/audit repositories or the
    forge pull-request reader are unwired.
  - invalid UUID (caught before the HTTP hop), run_not_found (404).
`),
	}, resolver.recordMergeObservation)
}

// registerReconcileMerge wires the fishhawk_reconcile_merge tool.
//
// Auth: same posture as its observe sibling, in BOTH halves —
// requireRunAccount(memberWrite) plus a handler-side
// requireWriteScope("write:runs") rung 0, mirrored in mcpscopes.go as
// {anyOf: ["write:runs"]} with no runBoundSubjectOK (E45.95 / #3635).
func registerReconcileMerge(srv *mcp.Server, resolver *runResolver) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "fishhawk_reconcile_merge",
		Description: strings.TrimSpace(`
Use this when fishhawk_get_run_status reports
run.completion_blocked.recovery = "reconcile-merge" — a 'running' run held open
by a merge-supersedable parked stage whose pull-request merge IS already on the
audit chain. It is the SECOND of the two merge-recovery verbs: if recovery still
says "record-merge-observation", run fishhawk_record_merge_observation FIRST,
because this verb NEVER re-reads the forge.

WHAT IT DOES. It supersedes only the default-deny (stage_type, state) pairs a
merge left parked, re-appends a missing audit row for a stage already marked
superseded (the 'repaired' list), and re-runs completion — so read run_state to
see whether the run actually settled.

Its evidence gate is CHAIN-ONLY. That is the whole reason the observe verb
exists: a run whose PR genuinely merged but whose merge was never recorded is
refused here with reconcile_merge_pr_not_merged, and no amount of retrying this
verb will change that.

Idempotent. A repeat returns two EMPTY lists and the run's current state.

Input:
  - run_id (required) — the Fishhawk run UUID.

Response: {run_id, superseded[{stage_id, stage_type, from_state, reason}],
repaired[...], run_state, message}.

Named refusals, each surfacing the backend's code verbatim so you can act on it:
  - reconcile_merge_pr_not_merged (409) — the run's PR is not OBSERVABLY merged
    on the chain. Run fishhawk_record_merge_observation first.
  - reconcile_merge_not_applicable (409) — the run holds no pair-table-
    admissible parked stage, so there is nothing this verb may terminalize;
    completion_blocked.reason says what the stage needs instead.
  - reconcile_merge_unconfigured (503) — the run/audit repositories are unwired.
  - invalid UUID (caught before the HTTP hop), run_not_found (404).
`),
	}, resolver.reconcileMerge)
}

// recordMergeObservation is the observe tool's handler. The UUID is parsed
// BEFORE the HTTP hop so a malformed run id never reaches the backend.
func (r *runResolver) recordMergeObservation(ctx context.Context, _ *mcp.CallToolRequest, in RecordMergeObservationInput) (*mcp.CallToolResult, RecordMergeObservationOutput, error) {
	runUUID, err := uuid.Parse(in.RunID)
	if err != nil {
		return nil, RecordMergeObservationOutput{}, fmt.Errorf("run_id %q is not a valid UUID: %w", in.RunID, err)
	}
	res, err := r.api.RecordMergeObservation(ctx, runUUID)
	if err != nil {
		return nil, RecordMergeObservationOutput{}, fmt.Errorf("record merge observation: %w", err)
	}
	out := RecordMergeObservationOutput{
		RunID:           res.RunID,
		AlreadyRecorded: res.AlreadyRecorded,
		Observation: RecordMergeObservationObservation{
			PullRequestURL:    res.Observation.PullRequestURL,
			PullRequestNumber: res.Observation.PullRequestNumber,
			MergeCommitSHA:    res.Observation.MergeCommitSHA,
			MergedAt:          res.Observation.MergedAt,
			ObservedAt:        res.Observation.ObservedAt,
		},
	}
	if out.RunID == "" {
		out.RunID = runUUID.String()
	}
	out.Message = recordMergeObservationMessage(out)
	return nil, out, nil
}

// reconcileMerge is the settle tool's handler. Same pre-hop UUID guard.
func (r *runResolver) reconcileMerge(ctx context.Context, _ *mcp.CallToolRequest, in ReconcileMergeInput) (*mcp.CallToolResult, ReconcileMergeOutput, error) {
	runUUID, err := uuid.Parse(in.RunID)
	if err != nil {
		return nil, ReconcileMergeOutput{}, fmt.Errorf("run_id %q is not a valid UUID: %w", in.RunID, err)
	}
	res, err := r.api.ReconcileMerge(ctx, runUUID)
	if err != nil {
		return nil, ReconcileMergeOutput{}, fmt.Errorf("reconcile merge: %w", err)
	}
	out := ReconcileMergeOutput{RunID: res.RunID, RunState: res.RunState}
	if out.RunID == "" {
		out.RunID = runUUID.String()
	}
	// The wire row and the tool row are structurally identical today; the
	// per-field conversion keeps them INDEPENDENT types so a future field on
	// either side is a compile error here rather than a silent drop.
	for _, st := range res.Superseded {
		out.Superseded = append(out.Superseded, ReconcileMergeStage(st))
	}
	for _, st := range res.Repaired {
		out.Repaired = append(out.Repaired, ReconcileMergeStage(st))
	}
	out.Message = reconcileMergeMessage(out)
	return nil, out, nil
}

// recordMergeObservationMessage renders the observe verb's operator-facing
// summary, naming the NEXT verb on the success arm and stating plainly that the
// no-op arm appended nothing. Pure so a table test pins it without an HTTP
// fixture.
func recordMergeObservationMessage(out RecordMergeObservationOutput) string {
	if out.AlreadyRecorded {
		return "no merge observation was appended — the audit chain already carried qualifying merge evidence, " +
			"so this call recorded NOTHING and the observation block is empty. " +
			"If the run is still held open, fishhawk_reconcile_merge is the verb that settles it."
	}
	return fmt.Sprintf(
		"recorded one merge_observation_recorded row for pull request %s (merge commit %s, merged at %s, observed at %s). "+
			"This SETTLES NOTHING: call fishhawk_reconcile_merge next to supersede the parked stage and complete the run.",
		observedOrUnknown(out.Observation.PullRequestURL),
		observedOrUnknown(out.Observation.MergeCommitSHA),
		observedOrUnknown(out.Observation.MergedAt),
		observedOrUnknown(out.Observation.ObservedAt))
}

// reconcileMergeMessage renders the settle verb's operator-facing summary:
// what moved, what was repaired, or — when neither — that the call was an
// idempotent no-op, always naming the run state so the operator can see whether
// the run actually settled. Pure so a table test pins it.
func reconcileMergeMessage(out ReconcileMergeOutput) string {
	state := observedOrUnknown(out.RunState)
	if len(out.Superseded) == 0 && len(out.Repaired) == 0 {
		return "no stage was superseded and none needed repair — this call was an idempotent no-op. " +
			"The run is now in state " + state + "; if that is still non-terminal, re-read " +
			"run.completion_blocked with fishhawk_get_run_status for what the stage needs instead."
	}
	var parts []string
	if n := len(out.Superseded); n > 0 {
		parts = append(parts, fmt.Sprintf("superseded %d parked stage%s (%s)",
			n, plural(n, "", "s"), stageTypeList(out.Superseded)))
	}
	if n := len(out.Repaired); n > 0 {
		parts = append(parts, fmt.Sprintf("re-appended the missing audit row for %d already-superseded stage%s (%s)",
			n, plural(n, "", "s"), stageTypeList(out.Repaired)))
	}
	return strings.Join(parts, "; ") + ". The run is now in state " + state + "."
}

// stageTypeList renders the stage types of a reconcile row set for the message.
func stageTypeList(rows []ReconcileMergeStage) string {
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		label := row.StageType
		if label == "" {
			label = "unknown"
		}
		if row.FromState != "" {
			label += " parked at " + row.FromState
		}
		out = append(out, label)
	}
	return strings.Join(out, ", ")
}

// observedOrUnknown keeps a message from rendering a bare empty string for a
// field the backend omitted.
func observedOrUnknown(s string) string {
	if s == "" {
		return "(unknown)"
	}
	return s
}
