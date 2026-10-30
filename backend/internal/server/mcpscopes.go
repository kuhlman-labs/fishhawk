package server

import "strings"

// This file carries the ONE reviewable tool-name -> required-scope table the
// /mcp HTTP-layer authorization gate consults (E66.30 / #2459), plus the
// resolution helpers around it.
//
// WHY IT LIVES IN internal/server AND NOT BESIDE THE REGISTRATIONS IN
// internal/mcpserver. The obvious home for this table is next to the
// mcp.AddTool calls it mirrors. It cannot live there: internal/server must NOT
// import internal/mcpserver, because mcpserver's in-package tests drive a real
// server.New (backend/internal/mcpserver/campaign_test.go), so that edge closes
// an import cycle in mcpserver's TEST binary — `go build` stays green and
// `go test ./internal/mcpserver/...` fails. The same constraint is what makes
// MCPServerFactory (mcproute.go) an injected seam rather than a direct call.
// The REVERSE edge is legal and already in use: mcproute_test.go imports
// mcpserver for TestMCPRoute_ToolRegistryParity, and that is exactly what makes
// the two-direction drift test TestMCPToolScopeTable_CoversRegistry possible —
// a table entry with no registered tool, and a registered tool with no entry,
// are both RED. If that test edge were ever removed the drift test would have
// to fall back to AST-parsing the registrations, the technique
// backend/internal/oauthas/scopes.go already uses for its own mirror.
//
// THE DERIVATION RULE, applied per entry: each tool dials one or more of
// fishhawkd's own REST endpoints with the caller's token, so the required
// scope here MIRRORS the predicate that endpoint's handler already enforces.
// This is an enforcement-point RELOCATION, not a new policy — the inner REST
// call remains authoritative, and this gate exists to refuse before a
// per-request MCP tool registry is ever constructed. Where a tool dials more
// than one endpoint, the entry mirrors the endpoint performing the tool's
// PRIMARY action (an incidental read does not lower the bar), any-of across
// them when it performs several.
//
// THE fhm_ NON-REGRESSION, the sharpest hazard in this table: a run-bound MCP
// token (subject "mcp:run:<uuid>", minted by handleIssueMCPToken in
// mcptoken.go) carries ONLY mcp:read, plus write:retries when the stage's spec
// sets executor.agent_self_retry, plus write:scope-amendments on implement
// stages, plus write:messages on plan and review stages (E77.3 / #3737). An
// entry demanding an OPERATOR-vocabulary scope for a tool the in-run
// agent calls would break the agent loop with no compile error. Two shapes
// guard that: any-of rules (retry.go's `write:stages OR write:retries`), and
// runBoundSubjectOK for the handlers that authorize a run-bound token by its
// SUBJECT rather than by scope (gateview.go, product_report.go).
type mcpToolScopeRule struct {
	// anyOf is satisfied when the identity holds AT LEAST ONE member.
	// EMPTY means authenticated-only — see mcpScopeAuthenticatedOnly.
	anyOf []string

	// runBoundSubjectOK mirrors a handler that admits a run-bound
	// ("mcp:run:<uuid>" subject) token WITHOUT the scope in anyOf, gating it
	// instead on the token's own run. Set it ONLY where the mirrored handler
	// really does that, naming the handler in the entry comment.
	runBoundSubjectOK bool
}

// mcpScopeAuthenticatedOnly is the named sentinel for a tool whose REST
// endpoint enforces NO scope beyond authentication. It is deliberately a named
// value rather than a bare `{}` literal so an entry reading
// `mcpScopeAuthenticatedOnly` is a positive claim ("the mirrored handler checks
// no scope") instead of an omission that could be a forgotten fill-in.
//
// The gate has already required a bearer identity carrying a TokenID before it
// consults this table, so "authenticated-only" here is genuinely the endpoint's
// own posture, not an absence of authentication.
var mcpScopeAuthenticatedOnly = mcpToolScopeRule{}

// scopeRunBoundRead is the scope every run-bound fhm_ token carries
// (mcptoken.go). It is NOT part of the operator vocabulary
// (oauthas.SupportedScopes) and never appears in an anyOf on its own — it is
// named here so the vocabulary pin in mcpscopes_test.go can allow it.
const scopeRunBoundRead = "mcp:read"

// scopeRunBoundRetry, scopeRunBoundScopeAmendments and scopeRunBoundMessages
// are the three conditional run-bound scopes (mcptoken.go: write:retries when
// the stage's spec sets executor.agent_self_retry, write:scope-amendments on
// implement stages, write:messages on plan and review stages ONLY — never
// implement, deploy or acceptance, E77.3 / #3737). scopeRunBoundMessages is
// the SAME constant crewmessage.go's handlers check, so the table and the
// endpoint share one spelling.
const (
	scopeRunBoundRetry           = "write:retries"
	scopeRunBoundScopeAmendments = "write:scope-amendments"
	scopeRunBoundMessages        = scopeWriteMessages
)

// scopeFixupAlternate is the legacy alternate the fixup/waive/defer handlers
// accept alongside write:stages (fixup.go, waive.go, defer_concern.go all read
// `!hasScope(id, "write:stages") && !hasScope(id, "write:fixups")`). It is not
// in the operator default vocabulary; it is named so the entries below and the
// vocabulary pin agree on one spelling.
const scopeFixupAlternate = "write:fixups"

// mcpToolScopes maps every tool mcpserver.NewServer registers onto the scope
// its underlying REST endpoint enforces. A tool ABSENT from this map is
// REFUSED (mcpToolScopeFor returns ok=false), so adding a tool without adding
// an entry fails closed — and TestMCPToolScopeTable_CoversRegistry makes it
// fail LOUDLY, in both directions, rather than silently at runtime.
var mcpToolScopes = map[string]mcpToolScopeRule{
	// --- Reads. Every one of these dials a handler that enforces no scope
	// beyond authentication (reads.go, runs.go, diagnostics.go,
	// calibration.go, campaigns.go handleGetCampaignStatus), or one that
	// gates a run-bound token on mcp:read (which such a token always holds)
	// while letting an operator bearer through unscoped
	// (run_stage_wait.go, scope_amendment.go handleListScopeAmendments).
	"fishhawk_get_active_run":      mcpScopeAuthenticatedOnly,
	"fishhawk_get_plan":            mcpScopeAuthenticatedOnly,
	"fishhawk_get_run_status":      mcpScopeAuthenticatedOnly,
	"fishhawk_list_audit":          mcpScopeAuthenticatedOnly,
	"fishhawk_list_runs":           mcpScopeAuthenticatedOnly,
	"fishhawk_runtime_calibration": mcpScopeAuthenticatedOnly,
	// fishhawk_precedent dials GET /v0/precedent (precedent.go
	// handleGetPrecedent), an authenticated read that checks NO scope: its
	// narrowing is the caller's own workspace account plus the point-read
	// repo-visibility DENY, both applied inside the handler.
	"fishhawk_precedent": mcpScopeAuthenticatedOnly,
	// fishhawk_list_schedules (E79.1 / #3725) dials GET /v0/schedules
	// (schedules.go handleGetSchedules), an authenticated read that checks NO
	// scope: its only narrowing is the point-read repo-visibility DENY. It
	// writes nothing and mints no audit entry.
	"fishhawk_list_schedules": mcpScopeAuthenticatedOnly,
	// fishhawk_delegation dials GET /v0/repos/{owner}/{name}/delegation
	// (delegation_view.go handleGetRepoDelegation), an authenticated read that
	// checks NO scope: its narrowing is the shared repoDashPrelude — the
	// caller's own workspace account plus the point-read repo-visibility DENY.
	// It writes nothing and mints no audit entry.
	"fishhawk_delegation":            mcpScopeAuthenticatedOnly,
	"fishhawk_await_stage":           mcpScopeAuthenticatedOnly,
	"fishhawk_await_audit":           mcpScopeAuthenticatedOnly,
	"fishhawk_await_children":        mcpScopeAuthenticatedOnly,
	"fishhawk_await_review":          mcpScopeAuthenticatedOnly,
	"fishhawk_verify_run":            mcpScopeAuthenticatedOnly,
	"fishhawk_get_campaign_status":   mcpScopeAuthenticatedOnly,
	"fishhawk_list_scope_amendments": mcpScopeAuthenticatedOnly,

	// fishhawk_doctor dials GET /v0/onboarding/readiness, an authenticated
	// read (onboarding.go handleGetOnboardingReadiness 401s anonymous and
	// checks no scope). fishhawk_init makes NO HTTP call at all — it renders
	// an embedded preset in-process — so authentication is the whole gate.
	// fishhawk_validate (E45.65 / #3579) likewise makes NO HTTP call — it
	// runs backend/internal/spec.ParseBytes in-process over the bytes it was
	// given — so authentication is the whole gate.
	"fishhawk_doctor":   mcpScopeAuthenticatedOnly,
	"fishhawk_init":     mcpScopeAuthenticatedOnly,
	"fishhawk_validate": mcpScopeAuthenticatedOnly,

	// fishhawk_release_notes defaults to mode=preview, which dials the
	// authenticated read GET /v0/releases/notes/preview (release_notes.go
	// handleReleaseNotesPreview: 401 anonymous, no scope). mode=prepare
	// additionally POSTs /v0/releases/notes, which DOES require write:runs —
	// but the mode is a BODY field this header-driven gate cannot see, and
	// refusing preview for want of a write scope would be STRICTER than the
	// endpoint. The persist arm stays enforced by its own handler.
	"fishhawk_release_notes": mcpScopeAuthenticatedOnly,

	// The #3083/#3136 merge-recovery pair (E45.88 / #3623, tightened by
	// E45.95 / #3635). fishhawk_record_merge_observation POSTs
	// /v0/runs/{run_id}/record-merge-observation (merge_observation.go
	// handleRecordMergeObservation) and fishhawk_reconcile_merge POSTs
	// /v0/runs/{run_id}/reconcile-merge (merge_supersede.go
	// handleReconcileMerge). BOTH handlers now call
	// requireWriteScope(w, r, "write:runs") as their rung 0, ahead of every
	// other refusal, so write:runs IS the predicate those endpoints enforce and
	// is therefore the faithful mirror — the same derivation as every other
	// write:runs row below. (Until #3635 both entries carried
	// mcpScopeAuthenticatedOnly, which faithfully mirrored handlers that
	// enforced ownership only; #3635 raised the endpoints and this table with
	// them, in that order.)
	//
	// runBoundSubjectOK is deliberately NOT set. Neither handler authorizes a
	// run-bound token by its SUBJECT: a run-bound fhm_ token carries only
	// mcp:read (plus the two conditional write scopes, neither of them
	// write:runs), so it is refused at the REST layer too — admitting it here
	// would make the gate LOOSER than the endpoint. Nothing in the in-run agent
	// loop calls either verb; they are operator recovery verbs handed out by
	// completion_blocked on GET /v0/runs/{id}.
	"fishhawk_record_merge_observation": {anyOf: []string{"write:runs"}},
	"fishhawk_reconcile_merge":          {anyOf: []string{"write:runs"}},

	// fishhawk_file_issue POSTs /v0/work-items (workitems.go
	// resolveWorkItemRequest), which since #4481 calls
	// requireWriteScope(w, r, "write:runs") for every NON-run-bound caller
	// before the body is read, and admits a run-bound subject WITHOUT
	// write:runs, gating it instead on its own-run entitlement
	// (runBoundTokenRunID: 403 run_not_entitled on a foreign run_id, 403
	// run_scoped_filing_required on the run-absent arm). That is the
	// mutually-exclusive switch product_report.go handleFileProductReport uses
	// for the same filing-with-deployment-credentials action, so the row is the
	// same shape as fishhawk_report_product_issue's below.
	//
	// runBoundSubjectOK is a DELIBERATE widening relative to the prior
	// mcpScopeAuthenticatedOnly row's derivation, and a permission-drift check
	// will flag it: the run-bound in-run agent already reached this handler
	// (the empty anyOf admitted it), and the handler already admits it by
	// own-run entitlement, so the flag only keeps that path open now that
	// anyOf names write:runs, which a run-bound token never carries. Waive the
	// drift concern with this reason.
	//
	// A first-officer seat's clamped scopes (firstofficer.SeatScopes) lack
	// write:runs, so over /mcp a seat is now refused HERE with
	// insufficient_scope and records no first_officer_action_refused row (the
	// documented gate residual). Over REST the subject wrapper
	// (refuseFirstOfficerTrackerWrite, E85.6 / #4148) still refuses the seat
	// first, by subject, and records the row.
	"fishhawk_file_issue": {anyOf: []string{"write:runs"}, runBoundSubjectOK: true},

	// fishhawk_preview_issue POSTs /v0/work-items/preview
	// (workitem_preview.go handlePreviewWorkItem), which enforces no scope
	// predicate: the preview WRITES NOTHING (no provider.File, no audit), so
	// the #4481 write:runs check on the filing deliberately skips it. The
	// handler itself refuses a run-bound token 403 preview_operator_only
	// (ADR-064: no agent-reachable tracker read) and applies the point-read
	// repo-visibility DENY — a "gate admits, the handler refuses" row (#3774).
	"fishhawk_preview_issue": mcpScopeAuthenticatedOnly,

	// gateview.go handleGetRunGateView: a run-bound subject is authorized by
	// its own run; every other identity needs scopeGateViewRead (read:audit).
	"fishhawk_get_gate_view": {anyOf: []string{scopeGateViewRead}, runBoundSubjectOK: true},

	// fishhawk_get_merge_queue (ADR-093 slice 1, E87.4 / #4295) MIRRORS GET
	// /v0/repos/{owner}/{name}/merge-queue: handleGetMergeQueue refuses a
	// run-bound subject 403 run_token_forbidden and otherwise calls
	// requireWriteScope(scopeMergeQueueRead) = read:audit, so NO
	// runBoundSubjectOK. Pinned by
	// TestMCPToolScopeTable_GetMergeQueueMirrorsHandler.
	"fishhawk_get_merge_queue": {anyOf: []string{scopeGateViewRead}},

	// product_report.go handleFileProductReport: mutually-exclusive switch —
	// a run-bound token may file on its OWN run without write:runs; any other
	// bearer must hold write:runs.
	"fishhawk_report_product_issue": {anyOf: []string{"write:runs"}, runBoundSubjectOK: true},

	// --- write:runs (requireWriteScope / inline hasScope "write:runs").
	"fishhawk_start_run":          {anyOf: []string{"write:runs"}}, // runs.go handleCreateRun
	"fishhawk_cancel_run":         {anyOf: []string{"write:runs"}}, // runs.go handleCancelRun
	"fishhawk_consolidate_slices": {anyOf: []string{"write:runs"}}, // consolidate.go handleConsolidateRun
	"fishhawk_reset_run_branch":   {anyOf: []string{"write:runs"}}, // reset_branch.go handleResetRunBranch
	"fishhawk_resume_run":         {anyOf: []string{"write:runs"}}, // recover.go handleRecoverRun
	"fishhawk_reconcile_reviews":  {anyOf: []string{"write:runs"}}, // review_reconcile_http.go handleReconcileRunReviews
	"fishhawk_rerun_review":       {anyOf: []string{"write:runs"}}, // review_rerun.go handleRerunReview (#4219; also refuses run-bound and operator-agent tokens)
	"fishhawk_reap_stage":         {anyOf: []string{"write:runs"}}, // reap_failure.go handleReapStageFailure
	"fishhawk_dispatch_stage":     {anyOf: []string{"write:runs"}}, // host_dispatch.go handleHostDispatchStage
	"fishhawk_run_stage":          {anyOf: []string{"write:runs"}}, // host_dispatch.go handleHostDispatchStage
	"fishhawk_run_children":       {anyOf: []string{"write:runs"}}, // host_dispatch.go handleHostDispatchStage

	// stage_concurrency_promote.go handlePromoteQueuedStage (#4191). It also
	// refuses a run-bound token, which carries only mcp:read/write:retries.
	"fishhawk_promote_queued_stage": {anyOf: []string{"write:runs"}},

	// escape.go handleRecordEscape (E94.3 / #4694, ADR-117 D3 + A1): the
	// handler refuses ANY run-bound subject 403 run_token_forbidden before its
	// requireWriteScope("write:runs"), so no runBoundSubjectOK.
	"fishhawk_record_escape": {anyOf: []string{"write:runs"}},

	// merge_queue_promote.go handleMergeQueuePromote (ADR-093 D2, E87.6 /
	// #4297). It also refuses a run-bound token.
	"fishhawk_promote_merge_queue_entry": {anyOf: []string{"write:runs"}},

	// --- write:stages.
	"fishhawk_rebase_run_branch":         {anyOf: []string{"write:stages"}}, // rebase_branch.go handleRebaseRunBranch
	"fishhawk_vouch_commit":              {anyOf: []string{"write:stages"}}, // vouch.go handleVouchCommit
	"fishhawk_retrigger_ci":              {anyOf: []string{"write:stages"}}, // retrigger_ci.go handleRetriggerCI (E83.49 / #4082)
	"fishhawk_decide_scope_amendment":    {anyOf: []string{"write:stages"}}, // scope_amendment.go handleDecideScopeAmendment
	"fishhawk_decide_scope_completeness": {anyOf: []string{"write:stages"}}, // scope_completeness.go handleDecideScopeCompleteness
	// specialist_job.go handleRequestSpecialistJob (E85.9 / #4300). The
	// handler ALSO refuses a run-bound token that carries write:stages
	// (specialist_job_requester_not_a_seat), so runBoundSubjectOK stays false.
	// The first-officer seat's specialistjob.RequestScope (E85.6 / #4148) is
	// deliberately NOT an alternative here: this tool spawns the daemon-resolved
	// runner on the daemon host with the daemon's environment in a caller-chosen
	// working_dir (a caller-chosen runner_binary is refused over HTTP since
	// #4476), so a seat reaching it could run repository code on the host. A
	// seat files its request over REST (POST /v0/specialist-jobs), which only
	// queues the job. Pinned by
	// TestFirstOfficerSeat_MCPRefusesRunnerSpawningTools.
	"fishhawk_request_specialist_job": {anyOf: []string{"write:stages"}},

	// --- read:runs. specialist_job.go handleGetSpecialistJob (E85.9 /
	// #4300) is the one read that enforces read:runs rather than
	// authentication alone, so this entry mirrors it instead of joining the
	// authenticated-only reads above.
	"fishhawk_get_specialist_job": {anyOf: []string{"read:runs"}},

	// --- any-of pairs. These are the entries the fhm_ non-regression turns
	// on: retry.go and revive.go accept `write:stages OR write:retries`, and a
	// self-retrying implement agent holds ONLY write:retries.
	"fishhawk_retry_stage": {anyOf: []string{"write:stages", scopeRunBoundRetry}}, // retry.go handleRetryStage
	"fishhawk_revive_run":  {anyOf: []string{"write:stages", scopeRunBoundRetry}}, // revive.go handleReviveRun

	// fixup.go / waive.go / defer_concern.go: `write:stages OR write:fixups`.
	"fishhawk_fixup_stage":    {anyOf: []string{"write:stages", scopeFixupAlternate}},
	"fishhawk_waive_concern":  {anyOf: []string{"write:stages", scopeFixupAlternate}},
	"fishhawk_waive_concerns": {anyOf: []string{"write:stages", scopeFixupAlternate}}, // bulk_waive.go handleBulkWaiveConcerns (E64.77 / #3318) — same predicate as the singular verb
	"fishhawk_defer_concern":  {anyOf: []string{"write:stages", scopeFixupAlternate}},
	// resolve_concerns.go handleResolveConcerns (E83.53 / #4086) — the waive
	// predicate; the handler additionally refuses every agent subject.
	"fishhawk_resolve_concerns": {anyOf: []string{"write:stages", scopeFixupAlternate}},
	// label_concern.go handleLabelConcern (E94.8 / #4841) — the waive
	// predicate; the handler additionally refuses every agent subject.
	"fishhawk_label_concern": {anyOf: []string{"write:stages", scopeFixupAlternate}},
	// divergence_answer.go handleAnswerDivergence (E75.5 / #3733) — the waive/defer posture.
	"fishhawk_answer_divergence": {anyOf: []string{"write:stages", scopeFixupAlternate}},

	// --- write:approvals.
	"fishhawk_approve_plan":                 {anyOf: []string{"write:approvals"}}, // approvals.go handleSubmitApproval
	"fishhawk_reject_plan":                  {anyOf: []string{"write:approvals"}}, // approvals.go handleSubmitApproval
	"fishhawk_answer_clarification":         {anyOf: []string{"write:approvals"}}, // clarification_answer.go handleAnswerClarification
	"fishhawk_revise_plan":                  {anyOf: []string{"write:approvals"}}, // revise.go handleRevisePlan
	"fishhawk_merge_run":                    {anyOf: []string{"write:approvals"}}, // merge_run.go handleMergeRun
	"fishhawk_arbitrate_acceptance":         {anyOf: []string{"write:approvals"}}, // acceptance_arbitration.go handleAcceptanceArbitration
	"fishhawk_record_grooming_dispositions": {anyOf: []string{"write:approvals"}}, // grooming_dispositions.go handleRecordGroomingDispositions
	"fishhawk_record_upkeep_dispositions":   {anyOf: []string{"write:approvals"}}, // upkeep_dispositions.go handleRecordUpkeepDispositions
	"fishhawk_record_comms_dispositions":    {anyOf: []string{"write:approvals"}}, // comms_dispositions.go handleRecordCommsDispositions (#4016)

	// --- write:deploy. approvals.go handleSubmitApproval takes the
	// write:deploy branch for a deploy-gate approval (approvals.go:791).
	"fishhawk_approve_deploy": {anyOf: []string{"write:deploy"}},
	"fishhawk_reject_deploy":  {anyOf: []string{"write:deploy"}},

	// fishhawk_drive_run is the composite operator driver: it POSTs
	// /v0/runs/{id}/auto-drive and /auto-drive/acts (autodrive_http.go:
	// write:approvals) AND /host-dispatch (host_dispatch.go: write:runs).
	// ANY-OF across the two, because the gate must not be STRICTER than the
	// endpoints — each inner call still enforces its own predicate.
	"fishhawk_drive_run": {anyOf: []string{"write:approvals", "write:runs"}},

	// --- write:campaigns (campaigns.go, all four via requireWriteScope).
	"fishhawk_start_campaign": {anyOf: []string{"write:campaigns"}},
	// fishhawk_preview_campaign (#3647) MIRRORS POST /v0/campaigns/preview's own
	// requirement exactly. handlePreviewCampaign calls
	// requireWriteScope("write:campaigns") — the same guard handleCreateCampaign
	// uses, because a preview costs the identical per-item forge sweep and is the
	// pre-flight of a write — so this row must be neither stricter nor looser
	// than the endpoint it fronts.
	"fishhawk_preview_campaign":        {anyOf: []string{"write:campaigns"}},
	"fishhawk_start_campaign_item_run": {anyOf: []string{"write:campaigns"}},
	"fishhawk_resume_campaign":         {anyOf: []string{"write:campaigns"}},
	"fishhawk_cancel_campaign":         {anyOf: []string{"write:campaigns"}},
	// fishhawk_append_campaign_items (E86.47 / #4358) MIRRORS POST
	// /v0/campaigns/{id}/items: handleAppendCampaignItems calls
	// requireWriteScope("write:campaigns"). Pinned by
	// TestMCPToolScopeTable_AppendCampaignItemsMirrorsHandler.
	"fishhawk_append_campaign_items": {anyOf: []string{"write:campaigns"}},
	// fishhawk_campaign_hotfix (E83.100 / #4234) MIRRORS POST
	// /v0/campaigns/{id}/hotfix: handleCampaignHotfix calls
	// requireWriteScope("write:campaigns") and then refuses a run-bound
	// (mcp:run:*) subject and a delegated operator-agent subject outright, so
	// runBoundSubjectOK stays false. The operator-agent refusal is the inner
	// REST call's alone (this table has no subject-class axis beyond run-bound).
	// Pinned by TestMCPToolScopeTable_CampaignHotfixMirrorsHandler.
	"fishhawk_campaign_hotfix": {anyOf: []string{"write:campaigns"}},

	// fishhawk_digest (E75.6 / #3734) dials TWO endpoints: its default read
	// mode GETs /v0/digest (digest.go handleGetDigest:
	// requireWriteScope(scopeDigestRead) = read:audit) and mark_read=true
	// POSTs /v0/digest/mark-read (handleDigestMarkRead:
	// requireWriteScope("write:approvals")). A tool performing several
	// actions takes any-of across them; the inner REST call stays
	// authoritative for the mode actually invoked. Neither handler admits a
	// run-bound subject, so runBoundSubjectOK stays false.
	"fishhawk_digest": {anyOf: []string{scopeDigestRead, scopeDigestMarkRead}},

	// fishhawk_captain (E76.2 / #3765) dials SIX endpoints: action=read GETs
	// /v0/captain (captain.go handleGetCaptain: requireWriteScope(
	// scopeCaptainRead) = read:audit) and the five verbs POST
	// /v0/captain/{offer,withdraw,accept,relinquish,claim}
	// (handleCaptainVerb: requireWriteScope(scopeCaptainWrite) =
	// write:approvals). Any-of across them; the inner REST call stays
	// authoritative for the action invoked. A run-bound subject is an agent
	// identity the captain record refuses, so runBoundSubjectOK stays false.
	"fishhawk_captain": {anyOf: []string{scopeCaptainRead, scopeCaptainWrite}},

	// fishhawk_handover_brief (E76.4 / #3767) dials ONE endpoint: GET
	// /v0/handover-brief (handover_brief.go handleGetHandoverBrief:
	// requireWriteScope(scopeHandoverBriefRead) = scopeDigestRead =
	// read:audit). It never writes. The handler admits no run-bound subject,
	// so runBoundSubjectOK stays false.
	"fishhawk_handover_brief": {anyOf: []string{scopeDigestRead}},

	// The three crew-message tools (E77.3 / #3737, ADR-081 D4) front the
	// /v0/crew-messages surface in crewmessage.go. Each row mirrors its
	// handler's predicate in BOTH directions; runBoundSubjectOK stays false on
	// all three, because no crew-message handler admits a run-bound token by
	// SUBJECT alone — a run-bound token is admitted only while it HOLDS
	// write:messages (minted for plan and review stages only), and then only
	// on its own run's anchor, which the handler still enforces.
	//
	// fishhawk_send_crew_message POSTs /v0/crew-messages
	// (handleSendCrewMessage): a run-bound token needs write:messages
	// (requireCrewMessagesScope), every other identity write:stages.
	"fishhawk_send_crew_message": {anyOf: []string{"write:stages", scopeRunBoundMessages}},
	// fishhawk_read_crew_messages GETs /v0/crew-messages/{sequence} and
	// /v0/crew-messages (handleGetCrewMessage / handleListCrewMessages via
	// authorizeCrewRead): a run-bound token needs write:messages, every other
	// identity read:audit. NOT mcpScopeAuthenticatedOnly: the read routes
	// enforce read:audit, so an authenticated-only row would be LOOSER than
	// the endpoints and would let an implement-stage token build a registry.
	"fishhawk_read_crew_messages": {anyOf: []string{scopeGateViewRead, scopeRunBoundMessages}},
	// fishhawk_decide_crew_escalation POSTs
	// /v0/crew-messages/{sequence}/escalation-decision
	// (handleDecideCrewEscalation): a run-bound token is refused
	// self_decision outright, every other identity needs write:stages — so
	// write:messages is deliberately ABSENT here.
	"fishhawk_decide_crew_escalation": {anyOf: []string{"write:stages"}},
	// fishhawk_convert_crew_finding (E77.7 / #3741) POSTs
	// /v0/crew-messages/{sequence}/convert-to-concern
	// (handleConvertCrewFindingToConcern): the decide row's shape — a
	// run-bound token is refused self_decision, every other identity needs
	// write:stages, so write:messages is deliberately ABSENT.
	"fishhawk_convert_crew_finding": {anyOf: []string{"write:stages"}},
	// fishhawk_delegation_confirm (E76.5 / #3768) dials THREE endpoints:
	// action=read GETs /v0/repos/{owner}/{name}/delegation/confirmation
	// (delegation_confirm.go handleGetDelegationConfirmation), which checks NO
	// scope — its narrowing is the shared repoDashPrelude, the
	// fishhawk_delegation precedent — and confirm/lower POST routes that
	// enforce requireWriteScope(scopeDelegationConfirmWrite) = write:approvals
	// themselves; the inner REST call stays authoritative for the action
	// invoked. Both writes refuse a run-bound subject
	// (delegation_agent_identity_refused), so runBoundSubjectOK stays false.
	"fishhawk_delegation_confirm": mcpScopeAuthenticatedOnly,

	// --- refinement gate. draft_epic dials /v0/refinement/sessions and its
	// siblings, every one behind requireWriteScope(scopeRefinementGate)
	// (refinement.go, refinement_file.go).
	"fishhawk_draft_epic": {anyOf: []string{scopeRefinementGate}},
}

// mcpToolScopeFor is the ONLY lookup into mcpToolScopes. It returns ok=false
// for an unmapped name so the caller can fail closed rather than defaulting an
// unknown tool to the sentinel.
func mcpToolScopeFor(name string) (mcpToolScopeRule, bool) {
	rule, ok := mcpToolScopes[name]
	return rule, ok
}

// satisfiedBy reports whether id may invoke a tool carrying this rule.
//
// An EMPTY anyOf is satisfied by any identity: the gate has already required a
// bearer identity with a TokenID, and the mirrored handler enforces nothing
// further. A run-bound subject short-circuits only where the mirrored handler
// really admits one on subject alone (runBoundSubjectOK).
func (rule mcpToolScopeRule) satisfiedBy(id Identity) bool {
	if rule.runBoundSubjectOK && strings.HasPrefix(id.Subject, "mcp:run:") {
		return true
	}
	if len(rule.anyOf) == 0 {
		return true
	}
	for _, sc := range rule.anyOf {
		if hasScope(id, sc) {
			return true
		}
	}
	return false
}

// requiredScopeDescription renders the rule for the insufficient_scope
// envelope's details.required_scope, using the same `A or B` spelling the
// mirrored handlers already emit (retry.go, fixup.go, redrive.go).
func (rule mcpToolScopeRule) requiredScopeDescription() string {
	return strings.Join(rule.anyOf, " or ")
}
