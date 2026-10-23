package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// mergeRunCategories are the audit categories the tool's post-POST poll
// resolves on: the backend lifecycle signals that the run's PR merge has
// settled. pr_merged lands when the merge webhook resolves; the
// post_merge_observed backstop covers the lifecycle post-merge tail
// (#1370). There is NO persisted 'merged' run state — terminal-on-merge is
// StateSucceeded — so the await keys on these categories plus the
// run-terminal backstop, never on a state string.
var mergeRunCategories = []string{"pr_merged", "post_merge_observed"}

// MergeRunInput is the fishhawk_merge_run tool's input schema (E48.7 /
// #1954). run_id + verdict are required; timeout_seconds bounds the
// terminal await (clampAwaitTimeout: default 360, cap 600).
type MergeRunInput struct {
	RunID   string `json:"run_id" jsonschema:"the Fishhawk run UUID whose gate-approved PR to merge; resolved like the other run-keyed verbs"`
	Verdict string `json:"verdict" jsonschema:"required operator merge verdict — recorded verbatim on the chained merge_verdict_recorded audit entry as the audited decision to ship"`
	// TimeoutSeconds bounds the post-POST terminal await. The wait holds no
	// server state, so a timeout is a resumable checkpoint (re-invoke to
	// resume). WHICH half is idempotent, stated precisely (E45.87 / #3622): the
	// merge_verdict_recorded ROW is what a re-POST never duplicates, and the
	// DISPATCH half is safe to repeat because the endpoint observes the pull
	// request before dispatching — a merge that settled during the wait returns
	// already_merged rather than the 502 a blind re-dispatch produced.
	TimeoutSeconds int `json:"timeout_seconds,omitempty" jsonschema:"how long to await the terminal merge (default 360, capped at 600). On timeout the tool returns status=timeout (resumable) — re-invoke to resume. The re-POST never duplicates the merge_verdict_recorded ROW, and re-dispatching is safe because the endpoint OBSERVES the pull request first: a merge that settled during the wait comes back as already_merged instead of a 502"`
	// AllowOverlap / OverlapReason are the merge overlap guard override
	// (E83.61 / #4119), forwarded on every (re-)POST. OverlapReason is sent
	// VERBATIM: the trim only rejects a blank reason locally.
	AllowOverlap  bool   `json:"allow_overlap,omitempty" jsonschema:"override the merge overlap guard (#4119): merge even though another live run's implement stage scopes a path this pull request changes (overlapping_implement_in_flight), or the guard could not rule that out (overlap_undetermined / overlap_check_failed). An AUDITED operator judgement: it requires overlap_reason and is recorded as a merge_overlap_overridden audit row before any merge verdict or merge intent"`
	OverlapReason string `json:"overlap_reason,omitempty" jsonschema:"required non-empty (at most 2000 bytes) when allow_overlap is true: why merging over the overlap is safe. Recorded verbatim on the merge_overlap_overridden audit row"`
}

// mergeOverlapReasonMaxBytes mirrors the backend's overlap_reason cap
// (backend/internal/server/merge_overlap_guard.go, approval condition C4):
// an over-cap reason is a local tool error, never a POST.
const mergeOverlapReasonMaxBytes = 2000

// MergeRunOutput is the fishhawk_merge_run response. Status is one of:
//
//   - "merged"        — a pr_merged / post_merge_observed entry landed past
//     the verdict anchor: the PR is merged and the run resolved. ALSO the
//     status when the endpoint reports the PR was ALREADY merged
//     (already_merged:true, E45.87 / #3622): no merge was queued, the await is
//     SKIPPED entirely, and the reused status keeps every downstream consumer
//     that switches on the string working — the distinction rides
//     AlreadyMerged and the Message.
//   - "timeout"       — nothing landed within the window. Resumable: re-invoke
//     the tool to resume — the re-POST never duplicates the
//     merge_verdict_recorded ROW, and re-dispatching is safe because the
//     endpoint observes the pull request first.
//   - "run_terminal"  — the run reached failed/cancelled while the wait was
//     pending (ADR-036 backstop) — the merge will most likely never settle.
//   - "checks_pending" — the pull request's required checks have not all passed
//     (GitHub reports it in unstable status), so GitHub would not queue the
//     merge within the wall-clock budget (E67.56 / #2717). The verdict row is
//     recorded and durable; the merge itself is NOT queued (merge_queued:false).
//     Resumable: re-invoke once the checks complete — but if a required check
//     has already FAILED the merge will never queue, so inspect the PR instead
//     of waiting. See the per-field notes below for what the flags mean here.
//   - "conflicting"   — the pull request has a merge conflict against its base
//     (E64.14 / #3109), so GitHub can NEVER queue the merge. UNLIKE
//     checks_pending this is NOT resumable by waiting: an immediate return, no
//     verdict row recorded (merge_queued / verdict_recorded / already_recorded
//     all false). The Message names the resolution path — resolve the conflict,
//     vouch the resulting commit so the audit-complete check re-posts, re-approve,
//     and re-merge.
//   - "behind_base", "merge_candidate_unverified", "merge_candidate_verify_failed"
//     — the ADR-090 merge-candidate gate (E83.33 / #4018) refused the merge:
//     the PR head does not contain the base tip (409 merge_base_behind), a
//     Fishhawk-written head has no passing merge-candidate verify, or its
//     verify FAILED. Like conflicting these are IMMEDIATE returns (no poll, no
//     re-POST, no verdict row); NextAction names the verb that clears each one
//     (fishhawk_rebase_run_branch, fishhawk_await_stage, fishhawk_fixup_stage).
//     The gate's 502 merge_candidate_check_failed stays a TOOL ERROR: it is a
//     retryable read failure, not a precondition.
//   - "merge_candidate_held" — ADR-092 (E83.85 / #4200): the run's
//     merge-candidate pass is HELD in its base's queue behind the live pass
//     (409 merge_candidate_held). IMMEDIATE like the ADR-090 refusals, but it
//     clears ON ITS OWN — admission is automatic — so NextAction names
//     fishhawk_get_run_status (watch merge_candidate_queue) rather than a
//     clearing verb; QueuePosition / AnchoredHeadSHA / LiveRunID carry the
//     entry.
//   - "acceptance_stale", "forge_approval_required" — merge-readiness
//     refusals: the recorded acceptance verdict was invalidated by a later
//     acceptance_reopened (a fix-up push) (#4086), or the ADR-112
//     forge-approval fold did not answer approved at the verified head
//     (#4619). IMMEDIATE returns (no poll, no verdict row) instead of a
//     timeout; the Message is the server's verbatim and NextAction names the
//     clearing verb — details.next_step for acceptance_stale
//     (fishhawk_dispatch_stage / fishhawk_await_stage / fishhawk_retry_stage
//     with the stage_id) and for forge_approval_required (approve_pr,
//     dismiss_review, fishhawk_merge_run, or none). approval_dismissed stays
//     mapped (approve_pr ritual) for older backends; the merge endpoint no
//     longer emits it.
//   - "review_coverage_incomplete" — the #4219 decomposed-parent hold: a
//     succeeded slice is covered by no SUCCESSFUL agent review (the parent's
//     latest implement-review round and the slice's own latest round both
//     recorded zero verdicts). IMMEDIATE, no verdict row; NextAction is
//     details.next_step — fishhawk_rerun_review when the parent round settled
//     with no verdict or the parent has no round and its consolidated PR is
//     open (the verb starts the first consolidated round, #4282);
//     fishhawk_await_review while a round is in flight, before the PR opens,
//     or for a gating parent with no round (whose message names
//     fishhawk_fixup_stage).
//   - "queued" — ADR-093 slice 1 (E87.4 / #4295): the run is merge-queue
//     ELIGIBLE and the endpoint's 202 answer ENQUEUED it in its base's merge
//     queue (or marked merge intent on its existing entry) instead of refusing
//     it as behind / unverified / held. IMMEDIATE: the tool NEVER enters the
//     terminal await, because nothing merged and no merge_verdict_recorded row
//     was written (merge_queued / verdict_recorded / already_recorded are all
//     false). QueueState / QueuePosition / LiveRunID / EntryID / BaseRef /
//     AnchoredHeadSHA / AlreadyQueued / IntentSequence carry the entry, and
//     NextAction names fishhawk_get_merge_queue. An INELIGIBLE run instead
//     keeps the behind_base / merge_candidate_unverified / merge_candidate_held
//     status, with MergeQueueIneligible naming the reason codes.
//   - "overlapping_implement_in_flight", "overlap_undetermined" — the merge
//     overlap guard (E83.61 / #4119) refused the merge: another live run's
//     dispatched/running implement stage scopes a path this pull request
//     changes, or the guard could not rule that out and failed closed.
//     IMMEDIATE (no poll, no re-POST) and every merge flag is false: the
//     guard refuses before any verdict or merge-intent append. Overlap carries
//     the blocking / undetermined counts and, only when the caller's token
//     holds read:audit (#4582), the blocking / undetermined runs; otherwise
//     details_withheld is set and the lists are empty. NextAction names
//     fishhawk_await_stage on the blocking stage (overlapping; no run/stage
//     params when withheld) or fishhawk_merge_run with allow_overlap
//     (undetermined — an audited override). The guard's 502
//     overlap_check_failed and 503 overlap_override_unrecorded stay TOOL
//     ERRORS: both are retryable failures, not preconditions.
type MergeRunOutput struct {
	Status string `json:"status" jsonschema:"one of merged, timeout, run_terminal, checks_pending, conflicting, behind_base, merge_candidate_unverified, merge_candidate_verify_failed, merge_candidate_held, acceptance_stale, approval_dismissed, forge_approval_required, review_coverage_incomplete, queued, overlapping_implement_in_flight, overlap_undetermined"`
	// RunState is the run's lifecycle state at resolution (succeeded on a
	// settled merge; failed/cancelled on the run_terminal backstop).
	RunState string `json:"run_state,omitempty" jsonschema:"the run's lifecycle state at resolution"`
	// MergeQueued mirrors the endpoint's merge_queued: the merge was
	// dispatched through the shared GitHubMerger seam. FALSE on checks_pending —
	// GitHub refused to queue the merge because the required checks have not all
	// passed, so nothing is queued yet.
	MergeQueued bool `json:"merge_queued" jsonschema:"true when the endpoint dispatched the squash merge through the shared merger seam; false on checks_pending (GitHub would not queue it — the required checks have not all passed)"`
	// VerdictRecorded is true when THIS call appended the merge_verdict_recorded
	// row; AlreadyRecorded is true when the endpoint found an existing row and
	// skipped the duplicate append (still re-dispatching the merge). Exactly one
	// is true per SUCCESSFUL POST. On checks_pending BOTH are false: the merge
	// was never queued, and the response does not positively distinguish a
	// pre-existing verdict row from one this invocation created (E67.56 / #2717
	// binding condition 4), so provenance is not inferred — the Message carries
	// the durability claim instead.
	VerdictRecorded bool  `json:"verdict_recorded" jsonschema:"true when this call appended the chained merge_verdict_recorded audit entry; false on checks_pending (provenance is not inferred there)"`
	AlreadyRecorded bool  `json:"already_recorded" jsonschema:"true when the endpoint found an existing merge_verdict_recorded row (idempotent resume) and re-dispatched the merge without a duplicate append; false on checks_pending"`
	VerdictSequence int64 `json:"verdict_sequence,omitempty" jsonschema:"the merge_verdict_recorded row's audit sequence — the anchor the terminal await polls past; on checks_pending it is the durable verdict row's sequence when the server surfaced it"`
	// AlreadyMerged is the E45.87 / #3622 arm: the endpoint OBSERVED the pull
	// request already merged and queued NO merge (MergeQueued is false). The
	// tool returns status=merged WITHOUT awaiting — there is nothing left to
	// wait for.
	AlreadyMerged bool `json:"already_merged" jsonschema:"true when the endpoint observed the pull request was ALREADY merged and queued no merge; the tool returns status=merged immediately without awaiting"`
	// MergeObservationRecorded is true ONLY when that call successfully
	// appended the merge_observation_recorded audit row. It is false when the
	// chain already carried merge evidence, and false when the append FAILED —
	// in which case Message names record-merge-observation as the recovery.
	MergeObservationRecorded bool    `json:"merge_observation_recorded" jsonschema:"true only when this call appended the merge_observation_recorded audit row; false when the chain already carried merge evidence and false when the append failed (the message then names the recovery)"`
	PRURL                    string  `json:"pr_url,omitempty" jsonschema:"the merged pull request URL"`
	WaitedSeconds            float64 `json:"waited_seconds" jsonschema:"elapsed wall time spent awaiting the terminal merge"`
	// NextAction surfaces the operator post-merge dev-host step (the reused
	// postMergeStep) on status=merged. Per ADR-038 the MCP surface never
	// mutates the host, so this is SURFACED, not invoked.
	NextAction *SuggestedAction `json:"next_action,omitempty" jsonschema:"on status=merged, the operator post-merge dev-host step (scripts/dev post-merge) — surfaced for you to run, never invoked by the tool (ADR-038); on behind_base / merge_candidate_unverified / merge_candidate_verify_failed, the verb that clears the merge-candidate refusal (fishhawk_rebase_run_branch, fishhawk_await_stage or fishhawk_fixup_stage); on merge_candidate_held, fishhawk_get_run_status (admission is automatic; watch merge_candidate_queue); on acceptance_stale, the acceptance re-run verb with the stage_id; on approval_dismissed, the approve_pr ritual; on forge_approval_required, details.next_step (approve_pr ritual, dismiss_review naming the CHANGES_REQUESTED reviewer, fishhawk_merge_run for retry_after, none for configure_identity_provider or a forge without approval binding); on review_coverage_incomplete, fishhawk_rerun_review or fishhawk_await_review; on queued and merge_queue_owns_merge, fishhawk_get_merge_queue (watch the base's line); on overlapping_implement_in_flight, fishhawk_await_stage on the blocking run's stage; on overlap_undetermined, fishhawk_merge_run with allow_overlap (an audited override)"`
	Message    string           `json:"message,omitempty" jsonschema:"actionable explanation on the timeout / run_terminal / checks_pending / conflicting / merge-candidate / merge-readiness / merge-overlap statuses"`
	// QueuePosition / AnchoredHeadSHA / LiveRunID ride status
	// merge_candidate_held (ADR-092 / #4200), from the 409's details, and
	// status queued (ADR-093 slice 1, E87.4 / #4295), from the 202 body.
	QueuePosition   int    `json:"queue_position,omitempty" jsonschema:"on merge_candidate_held and queued: the run's 1-based place among its base's held entries (omitted when the run holds the line)"`
	AnchoredHeadSHA string `json:"anchored_head_sha,omitempty" jsonschema:"on merge_candidate_held and queued: the PR head the entry is anchored to (admission re-reads and may auto-advance it)"`
	LiveRunID       string `json:"live_run_id,omitempty" jsonschema:"on merge_candidate_held and queued: the run holding the base's merge-candidate line, when one does"`
	// QueueState / EntryID / BaseRef / AlreadyQueued / IntentSequence ride
	// status queued only (ADR-093 slice 1), from the 202 body.
	QueueState     string `json:"queue_state,omitempty" jsonschema:"on queued: live (the run holds its base's line; its merge-candidate pass runs there) or held (waiting at queue_position)"`
	EntryID        string `json:"entry_id,omitempty" jsonschema:"on queued: the merge-queue entry id"`
	BaseRef        string `json:"base_ref,omitempty" jsonschema:"on queued: the base ref whose queue the run entered"`
	AlreadyQueued  bool   `json:"already_queued,omitempty" jsonschema:"on queued: true when the entry already carried merge intent, so this call wrote nothing"`
	IntentSequence int64  `json:"intent_sequence,omitempty" jsonschema:"on queued: the audit sequence of the merge_queue_entered row that recorded your merge intent and verdict"`
	// MergeQueueIneligible rides the behind_base / merge_candidate_unverified
	// / merge_candidate_held statuses (ADR-093 slice 1): the 409's
	// details.merge_queue_ineligible, the eligibility reason codes that kept
	// the run OUT of the merge queue. Absent when the endpoint did not
	// evaluate eligibility (no queue wired, or an older backend).
	MergeQueueIneligible []string `json:"merge_queue_ineligible,omitempty" jsonschema:"on behind_base / merge_candidate_unverified / merge_candidate_held: why the run was NOT enqueued (not_approved, changes_requested, review_pending, review_coverage_incomplete, acceptance_not_eligible, acceptance_stale, checks_not_green, checks_red, blocking_concern_open, undeterminable); nothing was written. Clear each, then re-invoke"`
	// Note restates the split-identity contract: the PR-approval review stays a
	// gh step under the operator's OWN GitHub identity (option a, App-identity
	// approval deferred to E39). Queueing the merge before that approval is
	// safe — GitHub fires the merge once branch protection is satisfied.
	//
	// It also states the audit check honestly (E64.44 / #3161): Fishhawk
	// PUBLISHES fishhawk_audit_complete; whether that check gates the merge is
	// a property of the repository's branch protection, which fishhawk_doctor's
	// merge_gate rung reports. Nothing here may assert it is required.
	Note string `json:"note" jsonschema:"the PR-approval review stays a gh step under your own GitHub identity; queueing the merge before approval is safe — GitHub fires it once branch protection is satisfied. Fishhawk PUBLISHES the fishhawk_audit_complete check; whether it gates the merge depends on this repository's branch protection — fishhawk_doctor's merge_gate rung reports what the forge enforces"`
	// Overlap rides status overlapping_implement_in_flight / overlap_undetermined
	// (E83.61 / #4119), decoded from the 409's details.
	Overlap *mergeOverlapRefusal `json:"overlap,omitempty" jsonschema:"on overlapping_implement_in_flight / overlap_undetermined: the merge overlap guard's refusal — reason (overlap_undetermined: why the guard could not decide), head_sha, blocking_count / undetermined_count, and ONLY when the caller's token holds read:audit (#4582) blocking[] (runs whose live implement stage scopes a path this pull request changes, with the shared paths) and undetermined[] (live runs the guard could not judge). Without read:audit details_withheld is true, required_scope names read:audit and both lists are empty: the refusal carries counts plus next_step only"`
	// ScopeOverlap is informational (E83.60 / #4118): every non-terminal run
	// sharing files with the run being merged, from a best-effort live read
	// made after the merge flow returns. Never refuses; nil when the read
	// failed (a token without read:audit) or against an older backend.
	ScopeOverlap *scopeOverlap `json:"scope_overlap,omitempty" jsonschema:"informational (#4118): every OTHER non-terminal run in the repo whose effective scope shares exact paths with the run being merged (overlaps[] with run_id, issue_ref, decomposed_from, stage, stage_state, paths). Merging while one of them is mid-implement can leave it in a category-B dead end — check it before its next stage. This informational field never refuses the merge (the merge overlap guard, status overlapping_implement_in_flight, does — #4119); absent when the read failed (a token without read:audit) or against an older backend"`
}

// mergeRunNote is the split-identity reminder surfaced on every response.
const mergeRunNote = "The PR-approval review (gh pr review --approve) stays a step under your own GitHub identity — App-identity approval is deferred to E39. Queueing the merge before approval is safe: GitHub fires the squash merge once this repository's branch protection is satisfied. Fishhawk PUBLISHES the fishhawk_audit_complete check; whether that check gates the merge depends on the repository's branch protection — fishhawk_doctor's merge_gate rung reports what the forge actually enforces (E64.44 / #3161)."

// registerMergeRun wires the fishhawk_merge_run tool (E48.7 / #1954): the
// one operator verb that takes a gate-approved run from verdict to
// merged+terminal, replacing the bare merge_pr + post_merge hand ceremony.
//
// Auth: operator-only write tool — the backend requires write:approvals and
// rejects a run-bound agent token (403 run_token_forbidden).
func registerMergeRun(srv *mcp.Server, resolver *runResolver) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "fishhawk_merge_run",
		Description: strings.TrimSpace(`
Use this AFTER a run's PR gate is settled and you have approved the PR (gh
pr review --approve under your own identity): fishhawk_merge_run records
your operator merge verdict, queues the squash merge through the same
GitHubMerger seam the delegated drive_run may_merge arm uses, awaits the
webhook-settled terminal run state, and surfaces the operator post-merge
dev-host step (E48.7 / #1954). It replaces the four-step hand ceremony
(approve → merge → post-merge) with one verb.

Records a chained merge_verdict_recorded audit entry (your verdict verbatim)
and dispatches the merge. The tool always re-POSTs on resume with NO
client-side skip, and the endpoint's idempotence has TWO halves worth stating
separately:

  - the VERDICT ROW is what a re-POST never duplicates: a repeated POST finds
    the existing merge_verdict_recorded row and appends nothing
    (already_recorded:true).
  - the DISPATCH is safe to repeat because the endpoint OBSERVES the pull
    request before dispatching (E45.87 / #3622). Re-queuing a merge for an
    already-merged PR/MR errors on both forges, so a resumable re-invoke used
    to return 502 merge_dispatch_failed in exactly the case where the merge had
    SUCCEEDED during the wait. Now the endpoint answers already_merged:true
    with merge_queued:false, and this tool returns status=merged immediately
    without awaiting. merge_observation_recorded says whether THAT call
    persisted the merge_observation_recorded audit row; when it could not, the
    message names POST /v0/runs/{run_id}/record-merge-observation, and a run
    left non-terminal names POST /v0/runs/{run_id}/reconcile-merge.

The PR-approval review itself STAYS a gh step under your own GitHub identity
(App-identity approval is deferred to E39); queueing the merge before that
approval is safe — GitHub fires the merge once this repository's branch
protection is satisfied. Fishhawk PUBLISHES the fishhawk_audit_complete check
but does not make it required: whether it gates the merge is a property of the
repository's branch protection, and fishhawk_doctor's merge_gate rung reports
what the forge actually enforces (E64.44 / #3161).

There is no persisted 'merged' run state — terminal-on-merge is succeeded —
so the await keys on the pr_merged / post_merge_observed audit categories
plus the ADR-036 run-terminal backstop, never on a state string.

Statuses:
  - "merged"        — the merge settled; next_action carries the operator
                     post-merge dev-host step (surfaced, not invoked —
                     ADR-038 keeps host mutation out of the MCP surface).
                     ALSO returned when the endpoint reports the PR was
                     ALREADY merged (already_merged:true) — no merge was
                     queued and the await is skipped. The status is reused
                     deliberately so consumers switching on it keep working.
  - "timeout"       — nothing settled within the window; resumable — re-invoke
                     to resume (no duplicate verdict row, and the re-dispatch
                     is guarded by the endpoint's observe rung).
  - "run_terminal"  — the run reached failed/cancelled while waiting; the
                     merge will most likely never settle — check
                     fishhawk_get_run_status.
  - "checks_pending" — the pull request's required checks have not all passed
                     (GitHub reports it in unstable status), so GitHub would not
                     queue the merge within the wall-clock budget. The verdict is
                     recorded and durable but the merge is NOT queued. Resumable:
                     re-invoke once the checks complete — but if a required check
                     has already FAILED the merge will never queue, so inspect the
                     PR instead of waiting. This is a STATUS, not a tool error: an
                     immediate re-POST cannot succeed, so the tool waits within its
                     timeout budget rather than surfacing a remedy that would fail.
  - "conflicting"   — the pull request has a merge conflict against its base, so
                     GitHub can NEVER queue the merge. UNLIKE checks_pending this
                     is NOT resumable by waiting — the tool returns it IMMEDIATELY
                     (no poll, no re-POST) and records no verdict row. Resolve the
                     conflict on the run branch, vouch the resulting commit with
                     fishhawk_vouch_commit (so the fishhawk_audit_complete check
                     re-posts on the new head), re-approve, and re-invoke.
  - "behind_base"   — the PR head does not contain the current base tip, so
                     Fishhawk refuses to merge an unverified merge candidate
                     (ADR-090 D1). Immediate, no verdict row. next_action names
                     fishhawk_rebase_run_branch, which advances the branch and,
                     when the workflow declares a verify command, authorizes a
                     verify-only merge-candidate pass for the new head.
  - "merge_candidate_unverified" — the live head was written by Fishhawk (a base
                     advance, a conflict-resolution push or a fan-in
                     integration) and has no passing merge-candidate verify for
                     EXACTLY that SHA (ADR-090 D2). Immediate, no verdict row.
                     next_action names fishhawk_await_stage (a pass is in flight
                     on the named stage — dispatch it with fishhawk_dispatch_stage
                     on a local runner) or fishhawk_rebase_run_branch (whose
                     already-up-to-date arm triggers the pass).
  - "merge_candidate_held" — the run's merge-candidate verify pass is HELD in its
                     base's ADR-092 queue behind the pass holding the line.
                     Immediate, no verdict row. It clears ON ITS OWN: when the
                     line frees, the queue advances a behind branch, re-anchors
                     and starts the pass; a stalled line self-heals on the
                     periodic pump (about 1 minute), and
                     fishhawk_rebase_run_branch pumps the base manually.
                     queue_position,
                     anchored_head_sha and live_run_id carry the entry;
                     next_action names fishhawk_get_run_status (watch
                     merge_candidate_queue), then dispatch/await the pass once
                     admitted and re-invoke.
  - "merge_candidate_verify_failed" — the merge-candidate verify FAILED for the
                     live head: the declared verify command is red on the
                     combined tree. Immediate, no verdict row. next_action names
                     fishhawk_fixup_stage (ADR-090 D6: a delegated workflow
                     already routed one bounded fix-up when the result was
                     recorded; otherwise route it yourself).
  - "acceptance_stale" — the recorded acceptance verdict is stale: acceptance
                     was re-opened (a fix-up push landed a new head) after the
                     verdict was recorded, so fishhawk_audit_complete cannot
                     clear and the merge would only time out. Immediate, no
                     verdict row; the message names the verified and current
                     heads. next_action names fishhawk_dispatch_stage (stage
                     pending), fishhawk_await_stage (re-run in flight) or
                     fishhawk_retry_stage, with the acceptance stage_id.
  - "forge_approval_required" — the ADR-112 forge-approval fold did not
                     answer approved at the verified head: no write-or-higher
                     human reviewer satisfying the review gate approved it
                     (absent, dismissed, review_required), a write-or-higher
                     reviewer requested changes (vetoed), the approval could
                     not be determined (undetermined, reviews_truncated; a
                     moved head is undetermined with detail head_moved), or
                     the forge cannot bind an approval to a revision
                     (provider_unsupported; a GitLab run with a declared review
                     gate fails closed). Immediate, nothing recorded.
                     next_action follows details.next_step: approve_pr → the
                     approve_pr ritual; dismiss_review → a gh step naming the
                     CHANGES_REQUESTED reviewer; retry_after →
                     fishhawk_merge_run; configure_identity_provider or no
                     next_step → none (the message names the remedy).
  - "review_coverage_incomplete" — a decomposed parent has a succeeded slice
                     no SUCCESSFUL agent review covers: neither the parent's
                     latest implement-review round nor the slice's own latest
                     round recorded a verdict (a round whose reviewers all
                     failed or were skipped covers nothing). Immediate, no
                     verdict row; the message names the uncovered slice(s).
                     next_action names fishhawk_rerun_review (the parent round
                     settled with no verdict, or the parent has no round and
                     its consolidated PR is open — the verb then starts the
                     parent's first consolidated round) or
                     fishhawk_await_review (a round is in flight, the PR is not
                     open yet, or a gating parent has no round — the message
                     then names fishhawk_fixup_stage).
  - "queued"        — ADR-093 slice 1 (E87.4 / #4295): the run is merge-queue
                     ELIGIBLE (review gate approved with a LIVE forge
                     approval, acceptance merge-eligible and not stale,
                     required checks green at the head, no open blocking
                     concern), so instead of refusing it as behind_base /
                     merge_candidate_unverified / merge_candidate_held the
                     endpoint ENQUEUED it in its base's merge queue with your
                     merge intent (202). Immediate: the tool does NOT await a
                     terminal merge — nothing merged yet and no
                     merge_verdict_recorded row was written; your verdict rides
                     the merge_queue_entered audit row (intent_sequence).
                     queue_state (live / held), queue_position, live_run_id,
                     entry_id, base_ref and anchored_head_sha carry the entry;
                     already_queued:true means the entry already carried merge
                     intent and nothing was written. Admission is automatic:
                     the queue advances the branch, re-anchors and starts its
                     pass. next_action names fishhawk_get_merge_queue. With a
                     head-pinned merge seam bound (ADR-093 D3) your merge
                     intent persists until the entry settles: the queue
                     merges the run pinned to the verified head on its green
                     pass, so you do NOT re-invoke fishhawk_merge_run (a
                     re-invoke answers merge_queue_owns_merge); without one,
                     re-invoke once the pass is green. An INELIGIBLE
                     run keeps the behind_base / merge_candidate_unverified /
                     merge_candidate_held status and nothing is written;
                     merge_queue_ineligible names the reason codes.
  - "merge_queue_owns_merge" — ADR-093 D2/D3 (E87.5 / #4296): the run has
                     an active merge-queue entry (live or held, any phase)
                     carrying your merge intent and a head-pinned merge seam
                     is bound, so the QUEUE owns the merge and merges it
                     pinned to the verified head on its green pass.
                     Immediate: nothing recorded, nothing merged. next_action
                     names fishhawk_get_merge_queue. A PR that already merged
                     answers merged instead.
  - "overlapping_implement_in_flight" — the merge overlap guard (#4119):
                     ANOTHER live run in this repository (outside this run's
                     lineage) has its implement stage dispatched or running,
                     and its effective scope shares paths with what this pull
                     request changes at its head. Merging now moves the base
                     under that agent (the category-B dead-end class).
                     Immediate, no new verdict row, nothing merged or queued.
                     overlap.blocking names each run, its stage and the shared
                     paths ONLY when your token holds read:audit (#4582);
                     without it overlap carries blocking_count /
                     undetermined_count and details_withheld:true, and
                     next_action names fishhawk_await_stage without a run or
                     stage. With read:audit next_action names
                     fishhawk_await_stage on the blocking stage — re-invoke
                     once it settles, or pass allow_overlap with
                     overlap_reason to merge deliberately.
  - "overlap_undetermined" — the guard could not rule an overlap out while a
                     live implement exists elsewhere and FAILS CLOSED:
                     overlap.reason is changed_files_capped (the forge
                     truncated this PR's comparison), rename_source_unknown,
                     run_list_truncated, changed_paths_unavailable (no GitHub
                     anchors — GitLab runs land here) or scope_unresolved (a
                     live run's scope does not resolve). Immediate, no new
                     verdict row. overlap.undetermined names the runs only
                     with read:audit (otherwise undetermined_count and
                     details_withheld). next_action names fishhawk_merge_run
                     with allow_overlap: an override is an audited operator
                     judgement.

Inputs:
  - run_id          (required) — the gate-approved run's UUID; it must carry
                    a PR URL and must not be failed/cancelled (the backend
                    re-validates authoritatively, including the acceptance
                    gate).
  - verdict         (required) — your operator merge verdict, recorded
                    verbatim on the audit entry.
  - timeout_seconds — default 360, capped at 600.
  - allow_overlap   — override a refusing merge overlap guard (any of its
                    409s or its 502). Requires overlap_reason. The override is
                    recorded as a merge_overlap_overridden audit row BEFORE the
                    merge verdict (on a first merge) or the merge intent, and
                    is deduped across re-POSTs of the same judgement.
  - overlap_reason  — required non-empty (at most 2000 bytes) with
                    allow_overlap; recorded verbatim.

Tool errors:
  - invalid UUID (caught before the HTTP hop)
  - allow_overlap with a blank overlap_reason, or an overlap_reason over 2000
    bytes (caught before the HTTP hop)
  - the run has no PR URL, or the run is failed/cancelled (fast local
    refusal before the POST)
  - the backend's authoritative surfaces: validation_failed (400),
    run_token_forbidden / insufficient_scope (403), run_not_found (404),
    run_not_mergeable / acceptance_gate_not_passed (409),
    merge_dispatch_failed (502 — the verdict row is durable and the merge is
    retryable; re-invoke. The endpoint re-observes the forge once before
    returning this, so a merge that landed in the dispatch window comes back
    as already_merged instead. A surviving 502 carries forge_merge_state and
    names POST /v0/runs/{run_id}/record-merge-observation then
    POST /v0/runs/{run_id}/reconcile-merge as the recovery),
    merge_candidate_check_failed (502 — ADR-090 D7: on a fully wired GitHub
    run a forge or audit read error fails the merge-candidate gate CLOSED;
    nothing was recorded and the call is retryable; re-invoke),
    merge_unconfigured (503), merge_queue_unrecorded / merge_queue_write_failed
    (503 — ADR-093 slice 1: the merge intent could not be recorded on the
    audit chain, or the queue could not be written after it was; the run is
    NOT queued and the call is retryable; re-invoke),
    merge_queue_unreadable / merge_lock_unavailable (503 — ADR-093
    slice 2: the queue's ownership of the merge could not be read, or the
    run's merge-intent lock could not be taken within its bound; nothing was
    recorded and the call is retryable; re-invoke),
    merge_already_dispatched (409 — ADR-093 approval condition D1: the run's
    ONE merge dispatch is already claimed by the queue, the delegated arm or
    an earlier inline dispatch, so the forge was not called; a dispatched
    merge settles through the forge, a dispatch_failed claim (an uncertain
    forge outcome) needs the operator re-arm unless the pull request reads
    merged; a merge_checks_pending refusal releases the claim, so its
    re-invoke claims again), merge_head_moved (409 — #4641: the merge was
    PINNED to the head the merge-candidate gate admitted (details.head_sha)
    and the forge refused it because the pull request head moved after it;
    a held claim was released (the message names the re-arm if the release
    did not land). details.refusal_basis precheck / rest_sha means
    the forge applied nothing; graphql_reread / unknown is inferred, so
    auto-merge may be armed at the pinned head; inspect the pull request.
    Re-invoke once the head settles and, on a declared approval gate, is
    re-approved: the re-invoke re-gates at the new head),
    merge_dispatch_unclaimed (503 — the dispatch claim could not be written;
    nothing dispatched; retryable),
    overlap_check_failed (502 — the merge overlap guard could not read the
    forge, the live runs or a run's stages, so it refused; retryable;
    re-invoke, or override with allow_overlap), overlap_override_unrecorded
    (503 — the merge_overlap_overridden row could not be recorded, so nothing
    merged and no new verdict was recorded; retryable; re-invoke)

The backend's 409 merge_checks_pending is NOT surfaced as a tool error: it is
the checks-not-all-passed precondition the tool WAITS on (re-POSTing on the poll
tick within the shared timeout budget), returning status=checks_pending on
budget exhaustion rather than an error whose remedy is guaranteed to fail.

The backend's 409 merge_conflicting is likewise NOT a tool error: it is the
merge-conflict precondition the tool returns IMMEDIATELY as status=conflicting
(no poll, no re-POST — waiting cannot resolve a conflict), naming the
resolve-then-vouch-then-re-merge path.

The backend's 409 merge_base_behind / merge_candidate_unverified /
merge_candidate_verify_failed (ADR-090) are likewise NOT tool errors: each is
returned IMMEDIATELY as status=behind_base / merge_candidate_unverified /
merge_candidate_verify_failed with next_action naming the clearing verb. So is
the ADR-092 409 merge_candidate_held (status=merge_candidate_held,
next_action fishhawk_get_run_status). With the merge queue wired, an ELIGIBLE
run meeting one of those three gates is enqueued instead (status=queued); an
ineligible one keeps the 409 status with merge_queue_ineligible set.

The backend's 409 acceptance_stale (#4086), forge_approval_required (ADR-112 / #4619) and
review_coverage_incomplete (#4219) are likewise NOT tool errors: each is returned IMMEDIATELY under the same status name with
next_action naming the clearing verb, instead of a timeout. So are the merge
overlap guard's 409 overlapping_implement_in_flight / overlap_undetermined
(#4119), checked on every POST including a checks_pending re-POST.

Every non-error response also carries scope_overlap (#4118, informational,
best-effort): the other non-terminal runs whose effective scope shares files
with this run. A run there that is mid-implement may need a rebase or a re-plan
once this merge lands. The scope_overlap field itself never refuses; the
refusing consumer is the merge overlap guard above, which judges only runs
whose implement stage is live against this PR's actual changed paths.
`),
	}, resolver.mergeRun)
}

// isChecksPending reports whether err is the backend's 409 merge_checks_pending
// classification (E67.56 / #2717) — the checks-not-all-passed precondition the
// tool WAITS on rather than surfacing as an error. Returns the *apiError so the
// caller can read details.verdict_sequence.
func isChecksPending(err error) (*apiError, bool) {
	var ae *apiError
	if errors.As(err, &ae) && ae.Code == "merge_checks_pending" {
		return ae, true
	}
	return nil, false
}

// isConflicting reports whether err is the backend's 409 merge_conflicting
// classification (E64.14 / #3109) — the PR has a merge conflict against its
// base, so GitHub can NEVER queue the merge. Unlike checks_pending (which is
// resumable by waiting), this is an IMMEDIATE return: re-invoking without
// resolving the conflict cannot succeed, so the tool does not poll or re-POST.
// Returns the *apiError so the caller can read details.pr_url / mergeable_state.
func isConflicting(err error) (*apiError, bool) {
	var ae *apiError
	if errors.As(err, &ae) && ae.Code == "merge_conflicting" {
		return ae, true
	}
	return nil, false
}

// conflictingOutput builds the IMMEDIATE checkpoint returned when the backend
// refuses a conflicting PR (E64.14 / #3109). MergeQueued / VerdictRecorded /
// AlreadyRecorded are ALL false — nothing was queued and no verdict row was
// appended (the backend refuses before the append). The Message names the
// resolution path: resolve the conflict, vouch the resulting commit so the
// audit-complete check re-posts, re-approve, and re-merge. This is NOT
// resumable by waiting, so the tool returns it at once rather than polling.
func conflictingOutput(ae *apiError, start time.Time) MergeRunOutput {
	prURL, _ := ae.Details["pr_url"].(string)
	mergeableState, _ := ae.Details["mergeable_state"].(string)
	msg := "the pull request has a merge conflict against its base, so GitHub can never queue the squash merge. Waiting cannot resolve it. Resolve the conflict on the run branch, then vouch the resulting commit with fishhawk_vouch_commit (so the fishhawk_audit_complete check re-posts on the new head), re-approve the pull request, and re-invoke fishhawk_merge_run."
	if mergeableState != "" {
		msg += " (GitHub reports mergeable_state=" + mergeableState + ".)"
	}
	return MergeRunOutput{
		Status:          "conflicting",
		MergeQueued:     false,
		VerdictRecorded: false,
		AlreadyRecorded: false,
		PRURL:           prURL,
		WaitedSeconds:   time.Since(start).Seconds(),
		Note:            mergeRunNote,
		Message:         msg,
	}
}

// mergeCandidateStatuses maps the backend's ADR-090 merge-candidate 409 codes
// (E83.33 / #4018) to the tool's IMMEDIATE statuses. The gate's 502
// merge_candidate_check_failed is deliberately absent: a retryable read
// failure stays a tool error.
var mergeCandidateStatuses = map[string]string{
	"merge_base_behind":             "behind_base",
	"merge_candidate_unverified":    "merge_candidate_unverified",
	"merge_candidate_verify_failed": "merge_candidate_verify_failed",
	// ADR-092 (#4200): held behind the base's live pass. Clears on its own.
	"merge_candidate_held": "merge_candidate_held",
}

// isMergeCandidateRefusal reports whether err is one of the backend's ADR-090
// merge-candidate 409 refusals and returns the tool status it maps to. Like a
// conflict, none of them clears by waiting, so the tool returns at once.
func isMergeCandidateRefusal(err error) (*apiError, string, bool) {
	var ae *apiError
	if !errors.As(err, &ae) || ae.StatusCode != http.StatusConflict {
		return nil, "", false
	}
	status, ok := mergeCandidateStatuses[ae.Code]
	if !ok {
		return nil, "", false
	}
	return ae, status, true
}

// mergeCandidateOutput builds the IMMEDIATE checkpoint for a merge-candidate
// refusal. Nothing was queued and no verdict row was appended (the backend
// refuses before the append), so every flag is false. Message carries the
// backend's explanation verbatim; NextAction names the verb from the
// response's details.next_step, falling back to the status's documented verb
// when an older detail map omits it.
func mergeCandidateOutput(ae *apiError, status string, runID uuid.UUID, start time.Time) MergeRunOutput {
	prURL, _ := ae.Details["pr_url"].(string)
	verb, _ := ae.Details["next_step"].(string)
	if verb == "" {
		verb = map[string]string{
			"behind_base":                   "fishhawk_rebase_run_branch",
			"merge_candidate_unverified":    "fishhawk_rebase_run_branch",
			"merge_candidate_verify_failed": "fishhawk_fixup_stage",
			"merge_candidate_held":          "fishhawk_get_run_status",
		}[status]
	}
	params := map[string]string{"run_id": runID.String()}
	if stageID, _ := ae.Details["stage_id"].(string); stageID != "" {
		params["stage_id"] = stageID
	}
	consumes := consumesNone
	if verb == "fishhawk_fixup_stage" {
		consumes = consumesFixupBudget
	}
	msg := ae.Message
	if strings.TrimSpace(msg) == "" {
		msg = "the merge-candidate gate (ADR-090) refused the merge; call " + verb + ", then re-invoke fishhawk_merge_run."
	}
	reason := "ADR-090: Fishhawk merges only an up-to-date, verified merge candidate; re-invoke fishhawk_merge_run once this clears"
	out := MergeRunOutput{
		Status:               status,
		PRURL:                prURL,
		WaitedSeconds:        time.Since(start).Seconds(),
		Note:                 mergeRunNote,
		Message:              msg,
		MergeQueueIneligible: mergeQueueIneligibleFrom(ae.Details),
	}
	if status == "merge_candidate_held" {
		// ADR-092 (#4200): the held entry, from the 409's details. A JSON
		// number decodes as float64.
		if pos, ok := ae.Details["queue_position"].(float64); ok {
			out.QueuePosition = int(pos)
		}
		out.AnchoredHeadSHA, _ = ae.Details["anchored_head_sha"].(string)
		out.LiveRunID, _ = ae.Details["live_run_id"].(string)
		reason = "ADR-092: the pass is held behind its base's live pass and is admitted automatically when the line frees; watch merge_candidate_queue, dispatch and await the pass once admitted, then re-invoke fishhawk_merge_run"
	}
	out.NextAction = &SuggestedAction{
		Action:       verb,
		Params:       params,
		Precondition: "the merge-candidate gate refused this run's merge (" + ae.Code + ")",
		Consumes:     consumes,
		Reason:       reason,
	}
	return out
}

// mergeQueueIneligibleFrom reads details.merge_queue_ineligible (ADR-093
// slice 1, E87.4 / #4295): the eligibility reason codes the endpoint's
// merge-queue enqueue arm refused with. DEFENSIVE: the map crossed an HTTP
// JSON boundary, so an absent, wrong-typed or empty value is nil, and a
// non-string or blank member is skipped.
func mergeQueueIneligibleFrom(details map[string]any) []string {
	raw, _ := details["merge_queue_ineligible"].([]any)
	var out []string
	for _, v := range raw {
		if code, ok := v.(string); ok && strings.TrimSpace(code) != "" {
			out = append(out, code)
		}
	}
	return out
}

// queuedOutput builds the IMMEDIATE status=queued checkpoint for the
// endpoint's ADR-093 202 answer (E87.4 / #4295): the run entered its base's
// merge queue with the operator's merge intent. Nothing merged and no
// merge_verdict_recorded row was written, so every merge flag is false and
// the tool never arms the terminal await. NextAction names
// fishhawk_get_merge_queue on the run's repository and base.
func queuedOutput(res *MergeRunResult, repo string, runID uuid.UUID, start time.Time) MergeRunOutput {
	params := map[string]string{"run_id": runID.String()}
	if repo != "" {
		params["repo"] = repo
	}
	if res.BaseRef != "" {
		params["base_ref"] = res.BaseRef
	}
	msg := res.Message
	if strings.TrimSpace(msg) == "" {
		msg = "the run is merge-queue eligible and was queued in its base's merge queue (ADR-093 D2); nothing merged yet and no merge verdict was recorded. Watch it with fishhawk_get_merge_queue, then re-invoke fishhawk_merge_run once its merge-candidate pass is green."
	}
	return MergeRunOutput{
		Status:          "queued",
		PRURL:           res.PRURL,
		WaitedSeconds:   time.Since(start).Seconds(),
		Note:            mergeRunNote,
		Message:         msg,
		QueueState:      res.QueueState,
		QueuePosition:   res.QueuePosition,
		LiveRunID:       res.LiveRunID,
		EntryID:         res.EntryID,
		BaseRef:         res.BaseRef,
		AnchoredHeadSHA: res.AnchoredHeadSHA,
		AlreadyQueued:   res.AlreadyQueued,
		IntentSequence:  res.IntentSequence,
		NextAction: &SuggestedAction{
			Action:       "fishhawk_get_merge_queue",
			Params:       params,
			Precondition: "the run is in its base's merge queue with merge intent recorded (queued)",
			Consumes:     consumesNone,
			Reason:       "ADR-093: admission is automatic — the queue advances the branch, re-anchors and starts the merge-candidate pass when the line frees; watch the line, dispatch and await the pass once admitted on a local runner, then re-invoke fishhawk_merge_run",
		},
	}
}

// isQueueOwnsMerge reports whether err is the backend's 409
// merge_queue_owns_merge (ADR-093 D2, E87.5 / #4296): the run has an active
// merge-queue entry carrying merge intent, so the queue owns its merge and
// the endpoint recorded nothing.
func isQueueOwnsMerge(err error) (*apiError, bool) {
	var ae *apiError
	if errors.As(err, &ae) && ae.StatusCode == http.StatusConflict && ae.Code == "merge_queue_owns_merge" {
		return ae, true
	}
	return nil, false
}

// queueOwnsMergeOutput builds the IMMEDIATE status=merge_queue_owns_merge
// checkpoint. Nothing was recorded and nothing merged, so every merge flag is
// false; NextAction names fishhawk_get_merge_queue on the run's repository.
func queueOwnsMergeOutput(ae *apiError, repo string, runID uuid.UUID, start time.Time) MergeRunOutput {
	prURL, _ := ae.Details["pr_url"].(string)
	params := map[string]string{"run_id": runID.String()}
	if repo != "" {
		params["repo"] = repo
	}
	msg := ae.Message
	if strings.TrimSpace(msg) == "" {
		msg = "the merge queue owns this run's merge and merges it pinned to the verified head on its green pass; nothing was recorded. Watch it with fishhawk_get_merge_queue."
	}
	return MergeRunOutput{
		Status:        "merge_queue_owns_merge",
		PRURL:         prURL,
		WaitedSeconds: time.Since(start).Seconds(),
		Note:          mergeRunNote,
		Message:       msg,
		NextAction: &SuggestedAction{
			Action:       "fishhawk_get_merge_queue",
			Params:       params,
			Precondition: "the run's merge-queue entry carries your merge intent (merge_queue_owns_merge)",
			Consumes:     consumesNone,
			Reason:       "ADR-093 D3: the queue merges the run itself, pinned to the verified head, on its green pass; watch the line instead of re-invoking fishhawk_merge_run",
		},
	}
}

// readinessStatuses maps the backend's merge-readiness 409 codes (#4086) to the
// tool's IMMEDIATE statuses. None clears by waiting out the merge budget: a
// stale acceptance verdict needs an acceptance re-run, a dismissed approval
// needs a re-approval, an uncovered slice (#4219) needs a review round that
// records a verdict, and a non-qualifying ADR-112 forge-approval fold
// (forge_approval_required) needs what details.next_step names.
// approval_dismissed stays mapped for the gate view's advisory and older
// backends; the merge endpoint itself now refuses with forge_approval_required.
var readinessStatuses = map[string]string{
	"acceptance_stale":           "acceptance_stale",
	"approval_dismissed":         "approval_dismissed",
	"forge_approval_required":    "forge_approval_required",
	"review_coverage_incomplete": "review_coverage_incomplete",
}

// isMergeReadinessRefusal reports whether err is one of the backend's
// merge-readiness 409 refusals and returns the tool status it maps to.
func isMergeReadinessRefusal(err error) (*apiError, string, bool) {
	var ae *apiError
	if !errors.As(err, &ae) || ae.StatusCode != http.StatusConflict {
		return nil, "", false
	}
	status, ok := readinessStatuses[ae.Code]
	if !ok {
		return nil, "", false
	}
	return ae, status, true
}

// mergeReadinessOutput builds the IMMEDIATE checkpoint for a merge-readiness
// refusal (#4086). The backend refuses before the merge_verdict_recorded append,
// so every flag is false, and the server's message is passed through verbatim.
// approval_dismissed names the approve_pr ritual; acceptance_stale names
// details.next_step (falling back to fishhawk_dispatch_stage) with the
// re-opened acceptance stage's id; review_coverage_incomplete names
// details.next_step (fishhawk_rerun_review or fishhawk_await_review, falling
// back to the read-only fishhawk_await_review) with the run id.
func mergeReadinessOutput(ae *apiError, status string, runID uuid.UUID, start time.Time) MergeRunOutput {
	prURL, _ := ae.Details["pr_url"].(string)
	params := map[string]string{"run_id": runID.String()}
	var next *SuggestedAction
	switch status {
	case "review_coverage_incomplete":
		verb, _ := ae.Details["next_step"].(string)
		if verb != "fishhawk_rerun_review" {
			verb = "fishhawk_await_review"
		}
		reason := "await the in-flight (or not yet dispatched) implement-review round on the parent, then re-invoke fishhawk_merge_run"
		if verb == "fishhawk_rerun_review" {
			reason = "re-run the parent's failed implement-review round (or start its first consolidated round when none was dispatched) so a successful agent review covers every slice, await its verdict, then re-invoke fishhawk_merge_run"
		} else {
			params["stage"] = "implement"
		}
		next = &SuggestedAction{
			Action:       verb,
			Params:       params,
			Precondition: "a decomposed slice is not covered by a successful agent review (review_coverage_incomplete)",
			Consumes:     consumesNone,
			Reason:       reason,
		}
	case "forge_approval_required":
		next = forgeApprovalNextAction(ae, params, prURL)
	case "approval_dismissed":
		if prURL != "" {
			params["pr_url"] = prURL
		}
		next = &SuggestedAction{
			Action:       "approve_pr",
			Params:       params,
			Precondition: "no approval is live on the pull request and a prior review was dismissed (approval_dismissed)",
			Consumes:     consumesNone,
			Reason:       "re-approve the pull request under your own GitHub identity (gh pr review --approve), then re-invoke fishhawk_merge_run",
		}
	default:
		verb, _ := ae.Details["next_step"].(string)
		if verb == "" {
			verb = "fishhawk_dispatch_stage"
		}
		stageID, _ := ae.Details["acceptance_stage_id"].(string)
		if stageID == "" {
			stageID, _ = ae.Details["stage_id"].(string)
		}
		if stageID != "" {
			params["stage_id"] = stageID
		}
		consumes := consumesNone
		if verb == "fishhawk_retry_stage" {
			consumes = consumesRetryBudget
		}
		next = &SuggestedAction{
			Action:       verb,
			Params:       params,
			Precondition: "the recorded acceptance verdict was invalidated by a later re-open (acceptance_stale)",
			Consumes:     consumes,
			Reason:       "re-run acceptance against the current head and await its verdict, then re-invoke fishhawk_merge_run",
		}
	}
	msg := ae.Message
	if strings.TrimSpace(msg) == "" {
		msg = "the merge-readiness check refused the merge (" + ae.Code + ")"
		if next != nil {
			msg += "; call " + next.Action + ", then re-invoke fishhawk_merge_run"
		}
		msg += "."
	}
	return MergeRunOutput{
		Status:        status,
		PRURL:         prURL,
		WaitedSeconds: time.Since(start).Seconds(),
		Note:          mergeRunNote,
		Message:       msg,
		NextAction:    next,
	}
}

// forgeApprovalNextAction maps a 409 forge_approval_required (ADR-112 fold,
// #4619) to its NextAction by details.next_step: approve_pr → the approve_pr
// ritual; dismiss_review → a gh step naming the CHANGES_REQUESTED reviewer;
// retry_after → fishhawk_merge_run; configure_identity_provider (and a forge
// with no approval-to-revision binding, which names no next step) → none, the
// server's message carrying the remedy.
func forgeApprovalNextAction(ae *apiError, params map[string]string, prURL string) *SuggestedAction {
	step, _ := ae.Details["next_step"].(string)
	fold, _ := ae.Details["fold"].(string)
	precondition := "no qualifying forge approval covers the verified head (forge_approval_required, fold " + fold + ")"
	if prURL != "" {
		params["pr_url"] = prURL
	}
	switch step {
	case "approve_pr":
		return &SuggestedAction{
			Action:       "approve_pr",
			Params:       params,
			Precondition: precondition,
			Consumes:     consumesNone,
			Reason:       "approve the pull request at the current head under a write-or-higher GitHub identity that satisfies the review gate (gh pr review --approve), then re-invoke fishhawk_merge_run",
		}
	case "dismiss_review":
		reviewer, _ := ae.Details["veto_reviewer"].(string)
		if reviewer != "" {
			params["reviewer"] = reviewer
		}
		return &SuggestedAction{
			Action:       "dismiss_review",
			Params:       params,
			Precondition: precondition,
			Consumes:     consumesNone,
			Reason:       "reviewer " + nonEmptyOr(reviewer, "(see message)") + " requested changes: resolve them and have the review re-approved, or dismiss it (gh api -X PUT repos/{owner}/{repo}/pulls/{number}/reviews/{review_id}/dismissals), then re-invoke fishhawk_merge_run",
		}
	case "retry_after":
		return &SuggestedAction{
			Action:       "fishhawk_merge_run",
			Params:       params,
			Precondition: precondition,
			Consumes:     consumesNone,
			Reason:       "the approval could not be determined (a forge read failed, the head moved, or the listing was truncated); re-invoke fishhawk_merge_run",
		}
	}
	return nil
}

func nonEmptyOr(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

// mergeOverlapRun mirrors one entry of the backend's merge overlap guard
// refusal (backend/internal/server/merge_overlap_guard.go mergeOverlapBlocker,
// E83.61 / #4119): a blocking run (Paths = the shared paths) or an
// undetermined one (Paths empty). The json tags MUST byte-match the backend;
// the MCP → HTTP → Postgres test (integration/mcp/merge_overlap_test.go)
// catches a drift.
type mergeOverlapRun struct {
	RunID          string   `json:"run_id" jsonschema:"the other live run"`
	IssueRef       string   `json:"issue_ref,omitempty" jsonschema:"its trigger ref; a decomposition child carries its parent's issue"`
	DecomposedFrom string   `json:"decomposed_from,omitempty" jsonschema:"its decomposed parent run id, when it is a slice"`
	StageID        string   `json:"stage_id" jsonschema:"its live implement-type stage"`
	StageType      string   `json:"stage_type" jsonschema:"that stage's type (implement: an implement, fix-up, conflict-resolution or merge-candidate verify pass)"`
	StageState     string   `json:"stage_state" jsonschema:"dispatched or running"`
	Paths          []string `json:"paths" jsonschema:"the shared paths, sorted (empty for an undetermined run)"`
}

// mergeOverlapRefusal is the tool's view of a 409 overlapping_implement_in_flight
// / overlap_undetermined body's details. Blocking / Undetermined are populated
// only for a caller whose token holds read:audit (#4582); the counts always
// are.
type mergeOverlapRefusal struct {
	Reason            string            `json:"reason,omitempty" jsonschema:"overlap_undetermined only: changed_files_capped, rename_source_unknown, run_list_truncated, changed_paths_unavailable or scope_unresolved"`
	HeadSHA           string            `json:"head_sha,omitempty" jsonschema:"the pull request head the guard judged (absent when the changed paths were never read)"`
	BlockingCount     int               `json:"blocking_count" jsonschema:"how many live runs block the merge (always present)"`
	UndeterminedCount int               `json:"undetermined_count" jsonschema:"how many live runs the guard could not judge (always present)"`
	DetailsWithheld   bool              `json:"details_withheld,omitempty" jsonschema:"true when the caller's token lacks read:audit (#4582): blocking[] and undetermined[] are empty and only the counts are known"`
	RequiredScope     string            `json:"required_scope,omitempty" jsonschema:"with details_withheld: the scope that unlocks the runs' details (read:audit)"`
	Blocking          []mergeOverlapRun `json:"blocking" jsonschema:"runs whose live implement stage scopes a path this pull request changes; empty when details_withheld"`
	Undetermined      []mergeOverlapRun `json:"undetermined" jsonschema:"live runs the guard could not judge; empty when details_withheld"`
}

// mergeOverlapStatuses maps the backend's merge overlap guard 409 codes
// (E83.61 / #4119) to the tool's IMMEDIATE statuses. The guard's 502
// overlap_check_failed and 503 overlap_override_unrecorded are deliberately
// absent: retryable failures stay tool errors.
var mergeOverlapStatuses = map[string]string{
	"overlapping_implement_in_flight": "overlapping_implement_in_flight",
	"overlap_undetermined":            "overlap_undetermined",
}

// isMergeOverlapRefusal reports whether err is one of the merge overlap
// guard's 409 refusals and returns the tool status it maps to.
func isMergeOverlapRefusal(err error) (*apiError, string, bool) {
	var ae *apiError
	if !errors.As(err, &ae) || ae.StatusCode != http.StatusConflict {
		return nil, "", false
	}
	status, ok := mergeOverlapStatuses[ae.Code]
	if !ok {
		return nil, "", false
	}
	return ae, status, true
}

// mergeOverlapRefusalFrom decodes the guard's details. DEFENSIVE: the map
// crossed an HTTP JSON boundary, so a member that does not decode is dropped
// rather than failing the status; blocking / undetermined are always arrays.
// The counts come from the server's blocking_count / undetermined_count on a
// withheld body (#4582) and from the decoded lists otherwise.
func mergeOverlapRefusalFrom(details map[string]any) *mergeOverlapRefusal {
	out := &mergeOverlapRefusal{}
	out.Reason, _ = details["reason"].(string)
	out.HeadSHA, _ = details["head_sha"].(string)
	out.DetailsWithheld, _ = details["details_withheld"].(bool)
	out.RequiredScope, _ = details["required_scope"].(string)
	decode := func(key string) []mergeOverlapRun {
		runs := []mergeOverlapRun{}
		raw, _ := details[key].([]any)
		for _, v := range raw {
			b, err := json.Marshal(v)
			if err != nil {
				continue
			}
			var r mergeOverlapRun
			if json.Unmarshal(b, &r) != nil || r.RunID == "" {
				continue
			}
			if r.Paths == nil {
				r.Paths = []string{}
			}
			runs = append(runs, r)
		}
		return runs
	}
	out.Blocking = decode("blocking")
	out.Undetermined = decode("undetermined")
	count := func(key string, decoded int) int {
		if n, ok := details[key].(float64); ok && n >= 0 {
			return int(n)
		}
		return decoded
	}
	out.BlockingCount = count("blocking_count", len(out.Blocking))
	out.UndeterminedCount = count("undetermined_count", len(out.Undetermined))
	return out
}

// mergeOverlapOutput builds the IMMEDIATE checkpoint for a merge overlap guard
// refusal (E83.61 / #4119). The guard refuses before any verdict or
// merge-intent append, so every flag is false and the server's message is
// passed through verbatim. overlapping_implement_in_flight names
// fishhawk_await_stage on details.blocking_run_id / stage_id (the first
// blocker); overlap_undetermined names fishhawk_merge_run with allow_overlap,
// because nothing but the cause clearing or an audited override admits it.
func mergeOverlapOutput(ae *apiError, status string, runID uuid.UUID, start time.Time) MergeRunOutput {
	prURL, _ := ae.Details["pr_url"].(string)
	ov := mergeOverlapRefusalFrom(ae.Details)
	var next *SuggestedAction
	if status == "overlapping_implement_in_flight" {
		params := map[string]string{}
		blockingRun, _ := ae.Details["blocking_run_id"].(string)
		stageID, _ := ae.Details["stage_id"].(string)
		if blockingRun == "" && len(ov.Blocking) > 0 {
			blockingRun, stageID = ov.Blocking[0].RunID, ov.Blocking[0].StageID
		}
		if blockingRun != "" {
			params["run_id"] = blockingRun
		}
		if stageID != "" {
			params["stage_id"] = stageID
		}
		next = &SuggestedAction{
			Action:       "fishhawk_await_stage",
			Params:       params,
			Precondition: "another live run's implement stage scopes a path this pull request changes (overlapping_implement_in_flight)",
			Consumes:     consumesNone,
			Reason:       "wait for the overlapping stage to settle, then re-invoke fishhawk_merge_run (or pass allow_overlap with overlap_reason to merge deliberately)",
		}
	} else {
		reason := ov.Reason
		if reason == "" {
			reason = "undetermined"
		}
		next = &SuggestedAction{
			Action:       "fishhawk_merge_run",
			Params:       map[string]string{"run_id": runID.String(), "allow_overlap": "true"},
			Precondition: "the merge overlap guard could not rule out an overlap with a live implement stage elsewhere (overlap_undetermined: " + reason + ")",
			Consumes:     consumesNone,
			Reason:       "clear the cause (" + reason + ") and re-invoke, or re-invoke with allow_overlap and an overlap_reason: overriding is an audited operator judgement recorded as merge_overlap_overridden",
		}
	}
	msg := ae.Message
	if strings.TrimSpace(msg) == "" {
		msg = "the merge overlap guard refused the merge (" + ae.Code + "); call " + next.Action + ", then re-invoke fishhawk_merge_run."
	}
	return MergeRunOutput{
		Status:        status,
		PRURL:         prURL,
		WaitedSeconds: time.Since(start).Seconds(),
		Note:          mergeRunNote,
		Message:       msg,
		NextAction:    next,
		Overlap:       ov,
	}
}

// alreadyMergedMessage renders the operator-facing explanation for the
// already-merged arm (E45.87 / #3622).
//
// It PREFERS the endpoint's own message verbatim: the backend is the only side
// that knows whether the observation row was persisted, and on the
// append-failure path (binding approval condition 1) that message is what names
// POST /v0/runs/{run_id}/record-merge-observation as the recovery. A backend
// that sends no message (an older one, or the additive-compatibility shape)
// degrades to a locally-composed equivalent rather than an empty string, so the
// operator is never left with a bare status.
func alreadyMergedMessage(res *MergeRunResult) string {
	const lead = "the pull request is already merged, so no merge was queued and the tool did not wait."
	if strings.TrimSpace(res.Message) != "" {
		return lead + " " + res.Message
	}
	msg := lead
	if res.MergeObservationRecorded {
		msg += " This call recorded the merge observation on the run's audit chain."
	} else {
		msg += " No merge observation row was appended by this call; if the run's audit chain carries no merge evidence, POST /v0/runs/{run_id}/record-merge-observation to record it."
	}
	if res.RunState != "" && res.RunState != "succeeded" && res.RunState != "failed" && res.RunState != "cancelled" {
		msg += " The run is still " + res.RunState + "; POST /v0/runs/{run_id}/reconcile-merge to settle it."
	}
	return msg
}

// mergeVerdictSequenceFrom best-effort reads details.verdict_sequence (the audit
// sequence of the durable merge_verdict_recorded row) from a merge_checks_pending
// error's details. Returns 0 when absent or untyped. It is REPORTED in
// VerdictSequence but is NOT treated as provenance evidence (binding condition
// 4): the sequence's presence does not distinguish a pre-existing row from one
// this invocation created.
func mergeVerdictSequenceFrom(details map[string]any) int64 {
	if details == nil {
		return 0
	}
	switch n := details["verdict_sequence"].(type) {
	case float64:
		return int64(n)
	case json.Number:
		i, _ := n.Int64()
		return i
	case int64:
		return n
	}
	return 0
}

// checksPendingOutput builds the resumable checks_pending checkpoint returned
// when the shared deadline expires while GitHub still refuses to queue the merge
// (E67.56 / #2717). Per binding condition 4 it does NOT infer provenance the
// response cannot establish: VerdictRecorded and AlreadyRecorded are BOTH false
// (details.verdict_sequence is not evidence of which invocation created the row),
// and MergeQueued is false (nothing was queued). VerdictSequence carries the
// sequence when the server surfaced it, and the Message carries the durability
// claim plus the honest wording condition 1 requires — the checks have not all
// passed, an immediate retry cannot succeed, and a check that has already FAILED
// means inspecting the PR rather than waiting.
// E64.59 / #3190: `details` is the server's 409 detail map. When it carries an
// audit_complete_missing list, the message NAMES the blocking audit-complete
// item and its action, because fishhawk_merge_run is where an operator first
// observes the strand — the last hop of the same serialization boundary.
func checksPendingOutput(seq int64, start time.Time, details map[string]any) MergeRunOutput {
	return MergeRunOutput{
		Status:          "checks_pending",
		MergeQueued:     false,
		VerdictRecorded: false,
		AlreadyRecorded: false,
		VerdictSequence: seq,
		WaitedSeconds:   time.Since(start).Seconds(),
		Note:            mergeRunNote,
		Message: "the merge verdict is recorded and durable, but GitHub will not queue the squash merge because the pull request's required checks have not all passed (GitHub reports the pull request in unstable status). An immediate retry cannot succeed. If the checks are still pending, re-invoke fishhawk_merge_run once they complete; if a required check has already FAILED, the merge will never queue — inspect the pull request rather than waiting." +
			auditCompleteDetailSuffix(details),
	}
}

// checksPendingCheckpoint wraps checksPendingOutput with the E64.63 / #3222
// acceptance-blocker advisory. checksPendingOutput stays a PURE function over
// its inputs (it is the shape a reviewer reads to see what the checkpoint
// claims); this method is the one place the two extra HTTP reads are paid, and
// only on the checks_pending arm — a path already at least the clamped timeout
// deep by construction, so the happy merge path pays nothing.
//
// An empty advisory (degrades D1/D2) leaves the message BYTE-IDENTICAL to
// checksPendingOutput's.
func (r *runResolver) checksPendingCheckpoint(ctx context.Context, runID uuid.UUID, seq int64, start time.Time, details map[string]any) MergeRunOutput {
	out := checksPendingOutput(seq, start, details)
	out.Message = appendAcceptanceAdvisory(out.Message, r.acceptanceBlockerAdvisoryFor(ctx, runID))
	return out
}

// auditCompleteDetailSuffix renders the server's details.audit_complete_missing
// list into the operator-facing message tail (E64.59 / #3190).
//
// DEFENSIVE by contract: this decodes a map that crossed an HTTP JSON boundary,
// so an absent, wrong-typed, empty or item-shape-mismatched value degrades to
// the empty string — today's fixed message — and is NEVER an error. The tool
// must not fail to report a merge block because the block's explanation was
// malformed.
func auditCompleteDetailSuffix(details map[string]any) string {
	raw, ok := details["audit_complete_missing"]
	if !ok {
		return ""
	}
	items, ok := raw.([]any)
	if !ok || len(items) == 0 {
		return ""
	}
	var b strings.Builder
	for _, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		detail, _ := m["detail"].(string)
		if strings.TrimSpace(detail) == "" {
			continue
		}
		if b.Len() == 0 {
			b.WriteString(" fishhawk_audit_complete is pending because:")
		}
		b.WriteString(" " + detail)
	}
	return b.String()
}

// acceptanceAdvisoryReadTimeout bounds the two extra reads the acceptance
// advisory costs. Short by design: it is paid only on the timeout /
// checks_pending arms, and a backend too slow to answer it within the budget
// degrades to D1 (say nothing) rather than extending the tool's wall clock.
const acceptanceAdvisoryReadTimeout = 10 * time.Second

// mergeAdvisoryTail is the merge surface's tail clause, appended verbatim by
// run.AcceptanceBlockerAdvisory: on this surface what the blocked acceptance
// stage is holding back is the QUEUED MERGE, via the audit-complete check.
const mergeAdvisoryTail = " — the queued merge cannot fire until the acceptance stage settles and the fishhawk_audit_complete check clears."

// categoryAcceptanceReopened is the audit category the server writes when a
// fix-up push invalidates a settled acceptance verdict (#1682). Spelled as a
// literal here for the same reason auditcomplete spells it as one: the MCP tool
// layer decodes audit rows off the HTTP surface and does not import package
// server.
const categoryAcceptanceReopened = "acceptance_reopened"

// shortStageID renders a stage id the way the audit-complete detail does — the
// first 8 characters — so the same stage reads identically on both surfaces.
func shortStageID(id string) string {
	if len(id) >= 8 {
		return id[:8]
	}
	return id
}

// acceptanceBlockerAdvisoryFor renders the shared acceptance-blocker advisory
// for a run whose merge did not settle (E64.63 / #3222).
//
// WHY this surface says anything at all: a merge blocked by a non-terminal
// acceptance stage is exactly the shape the fix-up path has described precisely
// since #3116, while fishhawk_merge_run reported only "nothing landed within
// Ns" — and the operator, with no way to see the cause, reached for an admin
// bypass twice. The advisory keys on a NON-TERMINAL acceptance stage rather
// than on the acceptance_reopened entry: a merge blocked by an acceptance stage
// that was never dispatched is blocked exactly as hard. The entry only SHARPENS
// the wording.
//
// THE FAILURE CONTRACT — four degrades, each with its own outcome:
//
//	D1  the stages read FAILS            → "" (byte-identical message; we know
//	                                       nothing, so we say nothing)
//	D2  no NON-TERMINAL acceptance stage  → "" (byte-identical; no new noise on
//	                                       a healthy merge). The stage is
//	                                       selected by non-terminality, so an
//	                                       older terminal acceptance row never
//	                                       masks a later blocking one.
//	D3  the audit read FAILS             → the GENERIC wording (a non-terminal
//	                                       acceptance stage does block the
//	                                       merge; we just cannot claim a fix-up
//	                                       re-opened it)
//	D4  the audit read matches no entry
//	    SCOPED TO THIS STAGE             → the same GENERIC wording
//
// So a failed STAGES read says nothing while a failed or empty AUDIT read still
// says the true generic thing; the two never both apply.
//
// D4 is correlated on the entry's STAGE ID, never on the category alone (#3222
// binding condition 1): a run carrying more than one acceptance stage in its
// history, or a stale entry from an earlier one, must not draw the stronger "a
// fix-up re-opened this" claim with nothing supporting it.
//
// Best-effort by construction: every path returns a string, never an error, so
// this can never turn a resolved merge status into a tool error.
func (r *runResolver) acceptanceBlockerAdvisoryFor(ctx context.Context, runID uuid.UUID) string {
	// DETACHED from the caller's cancellation, with its own bound. Every arm
	// that renders this advisory is reached BECAUSE a bounded wait expired, and
	// on the checks-pending arms that wait is the shared deadlineCtx — so
	// inheriting cancellation would silence the explanation on exactly the paths
	// that exist to explain a wait that ran out. The tool is returning a
	// response either way; what remains is a bounded pair of reads to say WHY.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), acceptanceAdvisoryReadTimeout)
	defer cancel()

	stages, err := r.api.ListRunStages(ctx, runID)
	if err != nil {
		return "" // D1
	}
	// Selected by NON-TERMINALITY, not by first-match-on-type: a run can carry
	// more than one acceptance stage row in its history, and a first-match
	// selector lands on an earlier SUPERSEDED/succeeded one, hits the D2 guard
	// and silences the advisory on a merge the LATER stage is genuinely
	// blocking. blockingAcceptanceStage is the package's existing owner of that
	// idiom (it mirrors run.blockingAcceptanceStage), so both MCP surfaces and
	// the fix-up refusal now agree on which stage "the" acceptance stage is.
	acc := blockingAcceptanceStage(stages)
	if acc == nil {
		return "" // D2
	}

	// D3/D4 both render the generic shape; only a stage-CORRELATED entry
	// sharpens it to the re-opened wording.
	reopened := false
	entries, _, aerr := r.api.ListRunAudit(ctx, runID, ListRunAuditFilter{
		Category: categoryAcceptanceReopened,
		Limit:    200,
	})
	if aerr == nil {
		for _, e := range entries {
			if e.StageID != nil && *e.StageID == acc.ID {
				reopened = true
				break
			}
		}
	}
	return run.AcceptanceBlockerAdvisory(shortStageID(acc.ID), run.StageState(acc.State), reopened, mergeAdvisoryTail)
}

// appendAcceptanceAdvisory appends a non-empty advisory to an existing message,
// leaving the existing sentence unmodified (and the whole message byte-identical
// when the advisory is empty).
func appendAcceptanceAdvisory(msg, advisory string) string {
	if advisory == "" {
		return msg
	}
	return msg + " " + advisory
}

// mergeRun is the tool handler. It validates locally, refuses fast when the run
// cannot merge, ALWAYS re-POSTs the verdict (endpoint-side idempotence per #1954
// binding condition 1 — no client-side skip), WAITS through a checks-not-all-passed
// (409 merge_checks_pending) refusal within a single shared deadline (E67.56 /
// #2717) rather than surfacing it as an error whose remedy would fail, then
// awaits the terminal merge with the remaining budget via the await_audit poll
// idiom.
func (r *runResolver) mergeRun(ctx context.Context, req *mcp.CallToolRequest, in MergeRunInput) (*mcp.CallToolResult, MergeRunOutput, error) {
	res, out, err := r.mergeRunCore(ctx, req, in)
	if err != nil {
		return res, out, err
	}
	// Informational cross-run scope overlap (E83.60 / #4118): best-effort,
	// read after the merge flow so it never delays or refuses the merge. The
	// run_id already parsed inside mergeRunCore, so this parse cannot fail.
	if runID, perr := uuid.Parse(in.RunID); perr == nil {
		if ov, oerr := r.api.GetScopeOverlap(ctx, runID); oerr == nil {
			out.ScopeOverlap = ov
		}
	}
	return res, out, nil
}

// mergeRunCore is the merge flow proper; mergeRun wraps it with the
// informational scope_overlap read.
func (r *runResolver) mergeRunCore(ctx context.Context, _ *mcp.CallToolRequest, in MergeRunInput) (*mcp.CallToolResult, MergeRunOutput, error) {
	runID, err := uuid.Parse(in.RunID)
	if err != nil {
		return nil, MergeRunOutput{}, fmt.Errorf("run_id %q is not a valid UUID: %w", in.RunID, err)
	}
	verdict := strings.TrimSpace(in.Verdict)
	if verdict == "" {
		return nil, MergeRunOutput{}, fmt.Errorf("verdict is required: the merge records an audited operator decision to ship")
	}
	// The merge overlap guard override (E83.61 / #4119), validated before any
	// HTTP hop and mirroring the backend's 400s. The trim only rejects a blank
	// reason: the POST carries in.OverlapReason verbatim (approval condition C3).
	if len(in.OverlapReason) > mergeOverlapReasonMaxBytes {
		return nil, MergeRunOutput{}, fmt.Errorf("overlap_reason is %d bytes; it must be at most %d bytes", len(in.OverlapReason), mergeOverlapReasonMaxBytes)
	}
	if in.AllowOverlap && strings.TrimSpace(in.OverlapReason) == "" {
		return nil, MergeRunOutput{}, fmt.Errorf("allow_overlap requires a non-empty overlap_reason: overriding the merge overlap guard is an audited operator judgement — state why the overlap is safe")
	}
	mergeReq := mergeRunRequest{Verdict: verdict, AllowOverlap: in.AllowOverlap, OverlapReason: in.OverlapReason}

	// Pre-flight fast refusal (the backend re-validates authoritatively). A
	// run with no PR to merge, or one already failed/cancelled, can never
	// merge — refuse before the POST so the operator gets a clear local error
	// rather than a 409 round-trip.
	run, err := r.api.GetRun(ctx, runID)
	if err != nil {
		return nil, MergeRunOutput{}, fmt.Errorf("get run: %w", err)
	}
	if run.PullRequestURL == nil || *run.PullRequestURL == "" {
		return nil, MergeRunOutput{}, fmt.Errorf("run %s has no pull request URL — there is nothing to merge (dispatch and review the implement stage first)", runID)
	}
	if run.State == "failed" || run.State == "cancelled" {
		return nil, MergeRunOutput{}, fmt.Errorf("run %s is %s — a terminal-failed run cannot be merged; recover or start a fresh run", runID, run.State)
	}

	// One shared deadline bounds BOTH the checks-pending retry loop and the
	// terminal await (E67.56 / #2717), so the tool's overall wall-clock contract
	// is unchanged: a merge held back by pending checks cannot make the tool wait
	// longer than it does today.
	timeout := clampAwaitTimeout(in.TimeoutSeconds)
	start := time.Now()
	deadline := start.Add(time.Duration(timeout) * time.Second)

	// The checks-pending re-POSTs are bounded by the SHARED deadline too: a POST
	// issued on an unbounded ctx could let a slow or hung backend push the tool
	// past its promised timeout indefinitely instead of returning the resumable
	// checks_pending checkpoint (E67.56 / #2717). deadlineCtx caps every POST so
	// an in-flight one cannot outlive the deadline; a POST cancelled by it (or by
	// the parent ctx) after a checks-pending refusal is the bounded wait expiring,
	// not a dispatch failure, so it resolves to the checkpoint below.
	deadlineCtx, cancelDeadline := context.WithDeadline(ctx, deadline)
	defer cancelDeadline()

	interval := r.reviewPollInterval
	if interval <= 0 {
		interval = defaultReviewPollInterval
	}

	// Record the verdict + queue the merge. ALWAYS POST on resume (no
	// client-side skip): the endpoint is idempotent (#1954 condition 1), so a
	// re-invoke records no duplicate verdict row and still re-dispatches the
	// merge. A 409 merge_checks_pending is NOT an error — GitHub reports the PR's
	// required checks have not all passed and will not queue the merge, so the
	// tool re-POSTs on the poll tick until the endpoint accepts the merge or the
	// shared deadline expires (then returns the resumable checks_pending status).
	// A 502 (merge_dispatch_failed) and every OTHER error surface here verbatim —
	// the verdict row is durable, so a re-invoke re-queues the merge.
	//
	// sawChecksPending records that a checks-pending refusal was observed. It is a
	// dedicated flag rather than checksSeq != 0 so the "return the resumable
	// checkpoint on a bounded-wait cancellation" decision does not depend on the
	// server having surfaced details.verdict_sequence (mergeVerdictSequenceFrom
	// returns 0 when it is absent) — a checks-pending 409 with no sequence still
	// resolves to the checkpoint, not a spurious tool error.
	var res *MergeRunResult
	var checksSeq int64
	var sawChecksPending bool
	// checksDetails carries the LAST checks-pending 409's detail map so the
	// resumable checkpoint can name the blocking audit-complete item (#3190).
	var checksDetails map[string]any
	for {
		// Check expiry before every (re-)POST: the timer below is clamped to the
		// deadline, so it can fire AT the deadline and loop back here — re-POSTing
		// then would issue a doomed HTTP call past the tool's promised timeout. On
		// exhaustion return the resumable checkpoint instead of POSTing again.
		if sawChecksPending && !time.Now().Before(deadline) {
			return nil, r.checksPendingCheckpoint(ctx, runID, checksSeq, start, checksDetails), nil
		}
		var merr error
		res, merr = r.api.mergeRunWith(deadlineCtx, runID, mergeReq)
		if merr == nil {
			break
		}
		// A conflicting PR (E64.14 / #3109) can NEVER queue, so return the
		// immediate checkpoint — no poll, no re-POST. Checked before the
		// checks-pending arm because it is not a wait-and-resolve precondition:
		// waiting cannot clear a merge conflict.
		if cae, conflicting := isConflicting(merr); conflicting {
			return nil, conflictingOutput(cae, start), nil
		}
		// The ADR-090 merge-candidate refusals (E83.33 / #4018) clear only by a
		// named operator verb, never by waiting: return them immediately too.
		if mae, status, refused := isMergeCandidateRefusal(merr); refused {
			return nil, mergeCandidateOutput(mae, status, runID, start), nil
		}
		// The #4086 merge-readiness refusals (a stale acceptance verdict, a
		// dismissed approval) likewise clear only by a named verb. Checked on
		// every POST, so a re-POST during the checks_pending wait that meets one
		// returns it at once instead of waiting out the budget.
		if rae, status, refused := isMergeReadinessRefusal(merr); refused {
			return nil, mergeReadinessOutput(rae, status, runID, start), nil
		}
		// The merge overlap guard (E83.61 / #4119) clears only when the other
		// run's implement settles or the operator overrides: never by waiting
		// out the budget. Checked on every POST, like the readiness arm.
		if oae, status, refused := isMergeOverlapRefusal(merr); refused {
			return nil, mergeOverlapOutput(oae, status, runID, start), nil
		}
		// ADR-093 D2/D3 (E87.5 / #4296): the merge queue owns the run's merge
		// and merges it pinned to the verified head on its green pass; waiting
		// here cannot change that, so return at once.
		if qae, owned := isQueueOwnsMerge(merr); owned {
			return nil, queueOwnsMergeOutput(qae, run.Repo, runID, start), nil
		}
		ae, pending := isChecksPending(merr)
		if !pending {
			// A POST cancelled mid-retry AFTER a checks-pending hit — by the shared
			// deadline or the parent ctx — is the bounded wait expiring, not a
			// genuine dispatch failure. Return the resumable checkpoint rather than
			// a spurious tool error.
			if sawChecksPending && deadlineCtx.Err() != nil {
				return nil, r.checksPendingCheckpoint(ctx, runID, checksSeq, start, checksDetails), nil
			}
			return nil, MergeRunOutput{}, fmt.Errorf("merge run: %w", merr)
		}
		sawChecksPending = true
		checksDetails = ae.Details
		if seq := mergeVerdictSequenceFrom(ae.Details); seq != 0 {
			checksSeq = seq
		}
		// Wait one poll tick, bounded by the shared deadline and ctx. On
		// exhaustion return the resumable checks_pending checkpoint.
		if !time.Now().Before(deadline) {
			return nil, r.checksPendingCheckpoint(ctx, runID, checksSeq, start, checksDetails), nil
		}
		wait := interval
		if d := time.Until(deadline); d < wait {
			wait = d
		}
		timer := time.NewTimer(wait)
		select {
		case <-deadlineCtx.Done():
			timer.Stop()
			return nil, r.checksPendingCheckpoint(ctx, runID, checksSeq, start, checksDetails), nil
		case <-timer.C:
		}
	}

	// QUEUED ARM (ADR-093 slice 1, E87.4 / #4295). The endpoint ENQUEUED an
	// eligible behind / unverified / held run (202) instead of merging it:
	// nothing merged and no merge_verdict_recorded row exists, so there is no
	// anchor to await past and nothing would settle within the budget. Return
	// at once, BEFORE the terminal await is ever armed.
	if res.Queued {
		return nil, queuedOutput(res, run.Repo, runID, start), nil
	}

	out := MergeRunOutput{
		MergeQueued:              res.MergeQueued,
		VerdictRecorded:          !res.AlreadyRecorded,
		AlreadyRecorded:          res.AlreadyRecorded,
		VerdictSequence:          res.VerdictSequence,
		PRURL:                    res.PRURL,
		AlreadyMerged:            res.AlreadyMerged,
		MergeObservationRecorded: res.MergeObservationRecorded,
		Note:                     mergeRunNote,
	}

	// ALREADY-MERGED ARM (E45.87 / #3622). The endpoint observed the pull
	// request already merged and queued NO merge, so there is nothing for the
	// terminal await to wait FOR — skip it entirely and return at once. Status
	// reuses the existing "merged" rather than introducing a sixth value: the
	// PR is merged, which is what a consumer switching on the status string
	// needs to know, and the distinction rides AlreadyMerged + Message.
	if res.AlreadyMerged {
		out.Status = "merged"
		out.RunState = res.RunState
		out.WaitedSeconds = time.Since(start).Seconds()
		step := postMergeStep(run)
		out.NextAction = &step
		out.Message = alreadyMergedMessage(res)
		return nil, out, nil
	}

	// Await the terminal merge with the REMAINING budget (the shared deadline),
	// so the checks-pending retry loop + the terminal await never exceed the one
	// clamped timeout. Anchored past the verdict row so a stale pr_merged from an
	// earlier attempt cannot resolve the wait.
	remaining := int(time.Until(deadline).Seconds())
	if remaining < 0 {
		remaining = 0
	}
	status, runState, waited := r.awaitMergeTerminal(ctx, runID, res.VerdictSequence, remaining, start)
	out.Status = status
	out.RunState = runState
	out.WaitedSeconds = waited

	switch status {
	case "merged":
		step := postMergeStep(run)
		out.NextAction = &step
	case "timeout":
		out.Message = appendAcceptanceAdvisory(
			fmt.Sprintf("no pr_merged / post_merge_observed entry landed within %ds. The merge is queued; re-invoke fishhawk_merge_run to resume the wait (the endpoint is idempotent — the re-POST records no duplicate verdict row), or poll fishhawk_get_run_status.", clampAwaitTimeout(in.TimeoutSeconds)),
			r.acceptanceBlockerAdvisoryFor(ctx, runID))
	case "run_terminal":
		out.Message = fmt.Sprintf("run %s reached terminal state %q while awaiting the merge and no pr_merged / post_merge_observed entry landed — the merge will most likely never settle. Check fishhawk_get_run_status before re-invoking.", runID, runState)
	}
	return nil, out, nil
}

// awaitMergeTerminal polls the run audit for the first pr_merged /
// post_merge_observed entry past the verdict anchor, mirroring the
// fishhawk_await_audit idiom (a since-anchored fast read, then a poll on the
// injectable reviewPollInterval under a clamped deadline, with the ADR-036
// run-terminal backstop checked once before the loop and on each still-empty
// tick). Returns (status, runState, waitedSeconds) where status is one of
// merged / timeout / run_terminal.
func (r *runResolver) awaitMergeTerminal(ctx context.Context, runID uuid.UUID, sinceSeq int64, timeout int, start time.Time) (string, string, float64) {
	// Fast path: the merge may already have settled (a resume after the
	// webhook landed).
	if entry, err := r.nextAuditEntry(ctx, runID, mergeRunCategories, sinceSeq, false); err == nil && entry != nil {
		return "merged", r.mergeRunState(ctx, runID), time.Since(start).Seconds()
	}

	// Nothing yet: the ADR-036 run-terminal backstop resolves a run that is
	// already terminal at call time before a poll tick.
	if status, runState, done := r.mergeTerminalBackstop(ctx, runID, sinceSeq); done {
		return status, runState, time.Since(start).Seconds()
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
			return "timeout", "", time.Since(start).Seconds()
		case <-ticker.C:
			entry, err := r.nextAuditEntry(pollCtx, runID, mergeRunCategories, sinceSeq, false)
			if err != nil {
				// A deadline hit mid-poll cancels the in-flight request; that is
				// a timeout, not a transport failure.
				if pollCtx.Err() != nil {
					return "timeout", "", time.Since(start).Seconds()
				}
				// A transient transport error is not fatal — keep polling to the
				// bounded deadline rather than aborting the merge await.
				continue
			}
			if entry != nil {
				return "merged", r.mergeRunState(pollCtx, runID), time.Since(start).Seconds()
			}
			if status, runState, done := r.mergeTerminalBackstop(pollCtx, runID, sinceSeq); done {
				return status, runState, time.Since(start).Seconds()
			}
		}
	}
}

// mergeTerminalBackstop resolves the await ONLY when the run has reached a
// FAILED / CANCELLED terminal state while the merge entry is still pending
// (ADR-036) — states past which the queued merge will most likely never
// settle. It deliberately does NOT arm on 'succeeded': feature_change is
// terminal-on-succeeded, so a succeeded_pr_open run whose squash merge is
// still queued (no pr_merged / post_merge_observed entry yet) is the NORMAL
// happy path, and treating it as run_terminal would abandon the await the
// instant it started rather than letting the merge settle (or time out
// resumably). It does ONE final since-anchored read first — a pr_merged /
// post_merge_observed that landed at/after the terminal transition still
// resolves as merged and wins over the backstop. A failed/cancelled run with
// no such entry resolves run_terminal. Best-effort: a GetRun error or a
// non-failed/cancelled run keeps the poll/timeout path in charge.
func (r *runResolver) mergeTerminalBackstop(ctx context.Context, runID uuid.UUID, sinceSeq int64) (string, string, bool) {
	run, err := r.api.GetRun(ctx, runID)
	if err != nil || run == nil {
		return "", "", false
	}
	// Only failed / cancelled arm the backstop. 'succeeded' is the expected
	// terminal-on-merge state, not a stranded-merge signal (see the concern
	// #1954 fix-up: a normal succeeded run must await its queued merge).
	if run.State != "failed" && run.State != "cancelled" {
		return "", "", false
	}
	// Final read: an entry that landed at/after the terminal transition still
	// resolves as merged.
	if entry, rerr := r.nextAuditEntry(ctx, runID, mergeRunCategories, sinceSeq, false); rerr == nil && entry != nil {
		return "merged", run.State, true
	}
	return "run_terminal", run.State, true
}

// mergeRunState reads the run's current lifecycle state for the resolved
// output. Best-effort: a read error yields "" rather than failing the
// already-resolved merge await.
func (r *runResolver) mergeRunState(ctx context.Context, runID uuid.UUID) string {
	run, err := r.api.GetRun(ctx, runID)
	if err != nil || run == nil {
		return ""
	}
	return run.State
}
