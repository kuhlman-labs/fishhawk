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
	CredentialSource  string `json:"credential_source,omitempty" jsonschema:"the forge credential the read ran under: 'run' (the run's own installation) or 'repository_installation' (the GitHub App's current installation on the run's repository, used because the run carries no credential)"`
	// PullRequestURLSource says where PullRequestURL came from (#4222).
	PullRequestURLSource string `json:"pull_request_url_source,omitempty" jsonschema:"where pull_request_url came from: 'run_row', or 'pull_request_opened_audit' when the run row carries no URL and it was derived from the run's newest pull_request_opened entry (never written back to the run row)"`
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
	// SupersedeStranded opts into the stranded arm (#4222). Omitted/false keeps
	// the call wire-identical to the pre-#4222 verb: no request body is sent.
	SupersedeStranded bool `json:"supersede_stranded,omitempty" jsonschema:"OPT-IN. When true, also retire implement/review/acceptance stages stranded in dispatched/running and review/acceptance gates still pending on this already-merged run. The backend refuses the whole call (reconcile_merge_stage_live) when an in-flight candidate shows activity inside its idle threshold, and for a runner_kind=local run this tool first refuses locally when a runner process for a candidate stage is live on this host. Default false"`
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
	Warnings   []string              `json:"warnings,omitempty" jsonschema:"supersede_stranded only: why the host runner-liveness probe could not confirm every candidate stage idle (a non-local runner_kind, an unreadable run or stage list, an inconclusive probe). The call proceeded; the server-side idle-threshold gate is the authority"`
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

Legacy runs (#4222). A run that carries NO forge credential reads through the
GitHub App's CURRENT installation on the run's own repository — only when the
run is untenanted or that installation maps to the run's own account; otherwise
it is refused with record_merge_observation_no_credential. A run row with no
pull request URL derives one from the pr_url of the run's newest
pull_request_opened audit entry (read-time only, never written back), and that
URL is still held to the malformed / repo-mismatch checks before any forge read.
observation.credential_source (run | repository_installation) and
observation.pull_request_url_source (run_row | pull_request_opened_audit) say
which path was taken.

Idempotent. A repeat answers already_recorded:true having appended NOTHING, and
the observation block is EMPTY on that arm on purpose — the response must not
claim a row it did not write.

Input:
  - run_id (required) — the Fishhawk run UUID.

Response: {run_id, already_recorded, observation{pull_request_url,
pull_request_number, merge_commit_sha, merged_at, observed_at,
credential_source, pull_request_url_source}, message}.

Named refusals, each surfacing the backend's code verbatim so you can act on it:
  - record_merge_observation_no_pull_request (409) — neither the run row nor any
    pull_request_opened audit entry carries a PR URL.
  - record_merge_observation_malformed_pr_url (409) — the recorded URL does not parse.
  - record_merge_observation_pr_url_repo_mismatch (409) — the URL does not name
    this run's repository on this run's forge family.
  - record_merge_observation_pr_not_merged (409) — the forge says NOT merged;
    recording would manufacture evidence for a change that never shipped.
  - record_merge_observation_no_merge_commit (409) — merged but no commit SHA.
  - record_merge_observation_no_merge_timestamp (409) — merged but no timestamp.
  - record_merge_observation_no_credential (409) — the run carries no forge
    credential and the GitHub App installation on its repository cannot be used:
    the App is not installed there (install it, then retry), or the installation
    is not proven to belong to the run's own account. details.reason says which;
    nothing was recorded and no pull request was read.
  - record_merge_observation_forge_unavailable (502) — the forge read (or the
    installation lookup for a credential-less run) failed, so the merge state is
    UNKNOWN and nothing was recorded.
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

SETTLE-ONLY ARM. A merged run whose stages have ALL already succeeded but whose
run row never completed is settled with two empty lists: completion re-runs and
run_state reports the result.

STRANDED ARM (opt-in, supersede_stranded: true). A legacy run whose PR merged
while implement/review/acceptance stayed dispatched/running, or a review/
acceptance gate never opened (pending), cannot otherwise settle. With the flag
the backend also retires those stages (reason operator_reconcile_stranded) —
but refuses the WHOLE call with reconcile_merge_stage_live when any in-flight
candidate shows database-stamped activity inside its idle threshold (24h), and
re-checks each stage under its row lock. For a runner_kind=local run this tool
first probes this host (pgrep) for a runner process of every dispatched/running
implement/review/acceptance stage and refuses LOCALLY, sending nothing, when one
is live. For any other runner_kind, or when the probe is inconclusive, it
proceeds and says so in warnings; the server-side gate is the authority.

Idempotent. A repeat returns two EMPTY lists and the run's current state.

Input:
  - run_id (required) — the Fishhawk run UUID.
  - supersede_stranded (optional, default false) — opt into the stranded arm.

Response: {run_id, superseded[{stage_id, stage_type, from_state, reason}],
repaired[...], run_state, message, warnings[]}.

Named refusals, each surfacing the backend's code verbatim so you can act on it:
  - reconcile_merge_pr_not_merged (409) — the run's PR is not OBSERVABLY merged
    on the chain. Run fishhawk_record_merge_observation first.
  - reconcile_merge_not_applicable (409) — the run holds no pair-table-
    admissible parked stage, nothing to repair, is not in the settle-only shape,
    and (with supersede_stranded) no stranded candidate, so there is nothing this
    verb may terminalize; completion_blocked.reason says what the stage needs
    instead. For a stage stranded in flight, retry with supersede_stranded: true.
  - reconcile_merge_stage_live (409) — supersede_stranded was set and a
    candidate stage showed activity inside the idle threshold (or was touched
    after the check); details.live_stages names each one. Let it settle.
  - validation_failed (400) — a malformed request body.
  - reconcile_merge_unconfigured (503) — the run/audit repositories are unwired,
    or supersede_stranded was set and the run repository lacks the stranded
    capability.
  - a local refusal naming a live runner (supersede_stranded on a local run;
    no request was sent).
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
			PullRequestURL:       res.Observation.PullRequestURL,
			PullRequestNumber:    res.Observation.PullRequestNumber,
			MergeCommitSHA:       res.Observation.MergeCommitSHA,
			MergedAt:             res.Observation.MergedAt,
			ObservedAt:           res.Observation.ObservedAt,
			CredentialSource:     res.Observation.CredentialSource,
			PullRequestURLSource: res.Observation.PullRequestURLSource,
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
	var warnings []string
	if in.SupersedeStranded {
		w, gerr := r.guardStrandedRunnersIdle(ctx, runUUID)
		if gerr != nil {
			return nil, ReconcileMergeOutput{}, gerr
		}
		warnings = w
	}
	res, err := r.api.ReconcileMerge(ctx, runUUID, in.SupersedeStranded)
	if err != nil {
		return nil, ReconcileMergeOutput{}, fmt.Errorf("reconcile merge: %w", err)
	}
	out := ReconcileMergeOutput{RunID: res.RunID, RunState: res.RunState, Warnings: warnings}
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

// strandedProbeStageTypes are the stage types the backend's stranded table
// (run.StrandedMergeSupersedable) admits in dispatched/running — the only
// candidates a runner process can hold. Mirrored here because this package
// cannot import the run package; the backend's table is the authority and this
// set only decides which stages the host probe looks at.
var strandedProbeStageTypes = map[string]bool{"implement": true, "review": true, "acceptance": true}

// guardStrandedRunnersIdle is the host half of the stranded arm's liveness
// check (#4222), run BEFORE the reconcile POST and only when
// supersede_stranded is set. The backend's idle-threshold gate reads DB-stamped
// activity, which cannot see a runner that is alive but wedged past the
// threshold; this MCP server runs on the host that spawns every local runner
// (ADR-024), so for a runner_kind=local run it probes the process table too.
//
//   - runner_kind=local: every dispatched/running implement/review/acceptance
//     stage is probed through the shared r.livenessProbe() seam. Any runnerLive
//     verdict REFUSES with an error naming each live stage, and no reconcile
//     request is sent. An inconclusive probe adds a warning and proceeds.
//   - any other runner_kind (or an absent one): the host probe is
//     INAPPLICABLE, so it proceeds with a warning that the server-side gate is
//     the authority. No stage list is read and no probe runs.
//   - an unreadable run or stage list: proceeds with a warning. The probe is
//     defence in depth; the backend's gate and its row-locked re-check still
//     decide, and both fail closed.
func (r *runResolver) guardStrandedRunnersIdle(ctx context.Context, runUUID uuid.UUID) ([]string, error) {
	got, err := r.api.GetRun(ctx, runUUID)
	if err != nil {
		return []string{fmt.Sprintf(
			"could not read run %s to decide whether the host runner-liveness probe applies (%v); no host probe ran, so the server-side idle-threshold gate is the only liveness check",
			runUUID, err)}, nil
	}
	if got.RunnerKind != driveRunnerKindLocal {
		kind := got.RunnerKind
		if kind == "" {
			kind = "(absent)"
		}
		return []string{fmt.Sprintf(
			"the host runner-liveness probe is INAPPLICABLE to a runner_kind=%s run, so no host probe ran; the server-side idle-threshold gate is the authority on whether a stranded stage is idle",
			kind)}, nil
	}
	stages, err := r.api.ListRunStages(ctx, runUUID)
	if err != nil {
		return []string{fmt.Sprintf(
			"could not list the stages of run %s (%v); no host probe ran, so the server-side idle-threshold gate is the only liveness check",
			runUUID, err)}, nil
	}
	probe := r.livenessProbe()
	var live, warnings []string
	for _, st := range stages {
		if !strandedProbeStageTypes[st.Type] || (st.State != "dispatched" && st.State != "running") {
			continue
		}
		switch probe(ctx, st.ID) {
		case runnerLive:
			live = append(live, fmt.Sprintf("%s stage %s (%s)", st.Type, st.ID, st.State))
		case runnerUnknown:
			warnings = append(warnings, fmt.Sprintf(
				"the host runner-liveness probe for %s stage %s was inconclusive (pgrep absent from PATH, a syntax/fatal exit, or a timeout); the server-side idle-threshold gate is the authority for it",
				st.Type, st.ID))
		}
	}
	if len(live) > 0 {
		return nil, fmt.Errorf(
			"refusing supersede_stranded: a fishhawk-runner process is live on this host for %s, so that stage is not stranded. "+
				"No reconcile-merge request was sent. Let the runner settle (or stop it), then retry",
			strings.Join(live, ", "))
	}
	return warnings, nil
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
	msg := fmt.Sprintf(
		"recorded one merge_observation_recorded row for pull request %s (merge commit %s, merged at %s, observed at %s). "+
			"This SETTLES NOTHING: call fishhawk_reconcile_merge next to supersede the parked stage and complete the run.",
		observedOrUnknown(out.Observation.PullRequestURL),
		observedOrUnknown(out.Observation.MergeCommitSHA),
		observedOrUnknown(out.Observation.MergedAt),
		observedOrUnknown(out.Observation.ObservedAt))
	// The two legacy-run paths (#4222) are named so the operator sees the read
	// did not rest on the run's own record alone.
	if out.Observation.CredentialSource == "repository_installation" {
		msg += " The run carries no forge credential, so the pull request was read through the GitHub App's current installation on the run's repository."
	}
	if out.Observation.PullRequestURLSource == "pull_request_opened_audit" {
		msg += " The run row carries no pull request URL, so it was derived from the run's newest pull_request_opened audit entry (not written back to the run row)."
	}
	return msg
}

// reconcileMergeMessage renders the settle verb's operator-facing summary:
// what moved (parked and, under supersede_stranded, stranded stages), what was
// repaired, or — when neither — whether the run settled (the settle-only arm or
// an idempotent repeat on a settled run) or the call was a no-op on a run still
// open, always naming the run state so the operator can see whether the run
// actually settled. Pure so a table test pins it.
//
// With two empty lists the response cannot distinguish "this call settled a run
// whose stages had all already succeeded" from "an earlier call settled it", so
// the terminal-state wording names both rather than claiming either.
func reconcileMergeMessage(out ReconcileMergeOutput) string {
	state := observedOrUnknown(out.RunState)
	if len(out.Superseded) == 0 && len(out.Repaired) == 0 {
		if runStateIsTerminal(out.RunState) {
			return "no stage was superseded and none needed repair; completion re-ran and the run is in state " + state +
				" — either this call settled a run whose stages had all already succeeded (the settle-only arm), " +
				"or an earlier call had already settled it (an idempotent repeat)."
		}
		return "no stage was superseded and none needed repair — this call was an idempotent no-op. " +
			"The run is now in state " + state + "; if that is still non-terminal, re-read " +
			"run.completion_blocked with fishhawk_get_run_status for what the stage needs instead."
	}
	var parked, stranded []ReconcileMergeStage
	for _, row := range out.Superseded {
		if row.Reason == reconcileReasonStranded {
			stranded = append(stranded, row)
		} else {
			parked = append(parked, row)
		}
	}
	var parts []string
	if n := len(parked); n > 0 {
		parts = append(parts, fmt.Sprintf("superseded %d parked stage%s (%s)",
			n, plural(n, "", "s"), stageTypeList(parked)))
	}
	if n := len(stranded); n > 0 {
		parts = append(parts, fmt.Sprintf("retired %d stranded stage%s (%s)",
			n, plural(n, "", "s"), stageTypeListAt(stranded, "stranded at")))
	}
	if n := len(out.Repaired); n > 0 {
		parts = append(parts, fmt.Sprintf("re-appended the missing audit row for %d already-superseded stage%s (%s)",
			n, plural(n, "", "s"), stageTypeList(out.Repaired)))
	}
	return strings.Join(parts, "; ") + ". The run is now in state " + state + "."
}

// reconcileReasonStranded is the reason the backend stamps on a stage the
// stranded arm retired (merge_supersede.go supersedeReasonOperatorReconcileStranded).
const reconcileReasonStranded = "operator_reconcile_stranded"

// stageTypeList renders the stage types of a reconcile row set for the message.
func stageTypeList(rows []ReconcileMergeStage) string {
	return stageTypeListAt(rows, "parked at")
}

// stageTypeListAt is stageTypeList with the from_state preposition supplied
// ("parked at" for a park, "stranded at" for a stranded stage).
func stageTypeListAt(rows []ReconcileMergeStage, at string) string {
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		label := row.StageType
		if label == "" {
			label = "unknown"
		}
		if row.FromState != "" {
			label += " " + at + " " + row.FromState
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
