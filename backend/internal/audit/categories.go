package audit

import "sort"

// KnownCategories is the curated registry of canonical audit-log category
// strings (#1764, extended #1850). It is the validation authority behind
// fishhawk_await_audit and GET /v0/runs/{run_id}/audit: a wait armed on a
// category NOT in this set is almost always a misspelling or a
// wrong-surface string (e.g. the runner-log event "scope_amendment_pending"
// instead of the audit category "scope_amendment_requested"), which would
// otherwise block the full timeout on an unsatisfiable wait while the real
// entry sits undecided.
//
// The registry lives in package audit — NOT package server — deliberately:
// backend/internal/server already imports backend/internal/audit, so
// importing the server-side category constants back here would be an import
// cycle. The strings are therefore hardcoded rather than referenced from
// their emit sites; SuggestCategories's reproduction test and the
// IsKnownCategory sampled-membership test guard against drift, and the
// allow_unknown escape hatch bounds the cost of any omission.
//
// Seeded from the full backend/internal inventory: the run/plan/implement/
// acceptance/deployment lifecycle, the review-lifecycle literals, the
// scope-amendment + scope-completeness gates, PR/lineage events, the
// budget/cost/CI signals, and the token/policy/status surfaces. #1850
// closed the remaining gaps — the registry now also covers the API-token
// issue/revoke events, the board/work-item filing + transition family, the
// refinement-draft approve/reject decisions, the runner-kind resolution
// events, the deployment-dispatch failure, and the campaign-lifecycle
// markers (advanced / gate-acted / issue-started / issue-settled /
// issue-restarted / paused) written via audit.AppendGlobalChained. #1941
// added the failed-run revive audit kind (run_revived, #1915). E53.4 / #2227
// added escalation_fired, written at the ONE server-side escalation resolver
// both enforcement seams reach; E55.9 / #3754 added escalation_persona_attached,
// written once per review round whose fired escalations attach reviewer
// personas (server/escalation_persona.go) — INTERNAL like escalation_fired. E75.4 / #3732 added precedent_surfaced, the
// record of exactly what precedent a captain-facing surface showed at an open
// human gate (server/gate_precedent.go) — fingerprint-deduped like
// escalation_fired, INTERNAL (never its own issue-thread activity line), and
// never a gate input. E75.5 / #3733 added precedent_divergence (a captain's
// waive / defer / plan reject went against clear precedent —
// server/gate_divergence.go, shipped disabled) and
// precedent_divergence_answered (the captain's one_off / doctrine_change
// answer to it); both INTERNAL like precedent_surfaced. E53.5 / #2228 added stage_permissions_declared,
// written ONCE per run at run creation when the workflow declares any stage
// `permissions` or `egress` block (declaration-only, enforced: false). E66.37 /
// #2474 added acceptance_triage_arbitrated, the operator-only discharge of a
// paged acceptance triage (server/acceptance_arbitration.go). E48.101 /
// #2548 added parent_awaiting_child_scope_decision, the parent-side signal
// for a decomposition child parked in awaiting_scope_decision on a
// build-required scope-drift shortfall — emitted from BOTH the park-time
// handler (server/pullrequest.go) and, de-duplicated, from
// orchestrator.maybeAdvanceDecomposedParent when a sibling settles. E32.44 /
// #2412 added split_filing_refused, the on-approval hook's refusal marker when a
// split_proposal has a phase whose own declared scope.files count exceeds the
// resolved implement cap (the hook files ZERO children rather than emit a lead
// phase that would itself fail the implement cap). E50.6 / #2062 added
// split_parent_closed, the parent-close watcher's OBSERVATION of what an
// `issues.closed` delivery for a filed split's contract child did to the parent
// (server/split_parent_close.go) — recorded on the global chain and read by
// nothing: it gates no behavior, the forge's own state does. E67.24 / #2621 added
// approval_comment_refused, the issue-comment approval channel's marker for an
// over-cap approve comment refused before Submit — the breadcrumb that makes the
// deliberately SILENT reply-comment refusal (no reply, no approval row) visible
// in the run record. E32.45 / #2415 added plan_scope_cap_override_refused, the
// plan-gate marker for a --override-scope-cap approve REFUSED because the scope's
// minimum physical changed-file count already exceeds the run-immutable
// max_files_changed cap — the override cannot authorize a landing the implement
// stage re-check will reject, so the gate fails fast at approval instead of after
// a full implement run. E67.35 / #2660 added the required-tests plan gate's three
// markers — plan_missing_required_tests (an approve refused because the effective
// scope declares no test-shaped path while the implement stage requires
// tests_added_or_updated), plan_comment_only_override_refused (a --comment-only
// approve refused because the scope carries a non-.go testable-source path the
// comment-only exemption provably cannot cover), and
// plan_comment_only_override_acknowledged (the honored override, recording that
// the implement stage re-derives the verdict from the REAL diff). E48.103 /
// #2551 added concern_resolution_vetoed, the implement re-review's marker for a
// `confirmed` delta-verification resolution REFUSED because the evidence
// contradicts it (the raising reviewer rejected in the same round, the operator
// routed the concern with executed reproduction evidence, the fix-up pass landed
// no changes, or the evidence lookup itself failed). E67.18 / #2591 added
// scope_completeness_amended, the operator's amend-and-resume decision on a
// parked scope-completeness shortfall — it records the paths folded into the
// stage's effective scope, and it is ALSO read as an invalidator by
// server/prompt.go::resolveHeldCommitExemption, which demotes a stale
// `exempted` decision when an amend supersedes it.
// E64.2 / #3083 added stage_superseded_by_merge, the merge-supersede sweep's
// per-stage marker: one chained entry per stage a merge terminalized as
// `superseded` because the merge made it unreachable, naming the stage, its
// type, the state it was parked in and the reason (merge_observed |
// operator_reconcile | repair). It is APPENDED ONLY AFTER the compare-and-swap
// that moved the stage actually succeeded, so a refused sweep leaves a MISSING
// row rather than a false record of a supersession that never happened.
// E64.32 / #3136 added merge_observation_recorded, the OBSERVE half of the
// #3083 recovery pair: one chained entry per operator-invoked
// POST /v0/runs/{run_id}/record-merge-observation that read the run's pull
// request from the forge and found it merged. It carries the forge's merge
// commit SHA, the forge's merged_at (WHEN the merge happened), observed_at
// (when Fishhawk learned it) and reconciled_after_the_fact:true.
//
// It is DELIBERATELY DISTINCT from pr_merged rather than a synthetic pr_merged
// row. pr_merged carries a LIVE-observation timestamp that the latency and cost
// surfaces already read as "when Fishhawk knew", so back-dating one to the
// forge's merge time would corrupt those series and lie about how the merge was
// learned. Recording both timestamps under a separate category lets a reader
// see the gap without back-dating anything.
//
// Internal fact-record kind projected through the audit chain — NOT a new
// issue-comment surface (docs/issue-comment-surfaces.md).
// E55.1 / #2242 added the two document-injection markers written by
// backend/internal/repodoc: document_injected (one per repo-authored document
// injected into an agent prompt, naming the resolved path, the PINNED commit,
// the sha256 content hash over the resolved bytes, and the declaration site)
// and document_truncated (written IN ADDITION when the document exceeded the
// effective cap, naming the cap and the dropped-byte count). Both are written
// per prompt SERVE, so a retry or re-dispatch attributes again — the guarantee
// is that every injection is attributed. E55.7 / #3746 added the third,
// document_injection_degraded: written per served prompt that WITHHELD
// run-admission declarations because the run recorded no admission commit,
// naming the reason, the paths and the declaration sites.
// E54.3 / #2235 added grooming_report_recorded, written once per ingested
// grooming_report artifact on POST /v0/runs/{run_id}/plan (the plan-stage
// discriminator's second additive sibling) carrying the artifact's content hash
// and the per-class entry counts #2240's churn guard reads. E54.5 / #2237 added
// the grooming APPLY pair — grooming_mutation_applied (one row per SETTLED
// grooming mutation: applied, failed AND skipped alike, so one category filter
// returns the whole apply) and grooming_apply_completed (the once-per-apply
// summary) — declared as "…Category" constants in
// backend/internal/workmgmt/grooming_apply.go, the shape the completeness AST
// sweep collects. E54.8 / #2240 added grooming_churn_filtered, written once per
// CHURN GUARD pass on the same ingest path: it carries the proposed/suppressed/
// resurfaced counts and entry ids, the charter_changed flag, and the
// no_changes_proposed flag that IS #2240 AC1's visible "no changes proposed"
// outcome — so an operator awaits this one category to learn what a grooming
// run actually proposes, as distinct from what the agent emitted.
// E54.30 / #2843 added grooming_disposition_recorded, written once per
// DISPOSITION an operator records against an individual grooming-report entry
// on POST /v0/runs/{run_id}/grooming-dispositions. It is a DIFFERENT FACT from
// grooming_mutation_applied and the two must not be conflated: this row is what
// the OPERATOR DECIDED (approved / rejected / amended, with an optional
// close_target) about one entry id; a grooming_mutation_applied row is what was
// APPLIED, and the second derives from the first. Capture is operator-only; as
// of E54.48 / #2991 these rows are CONSUMED by the on-approval apply hook, which
// settles the artifact's window (grooming_apply_window_closed) and applies the
// recorded verdicts.
// E54.48 / #2991 added grooming_apply_window_closed, the WATERMARK the
// capture/apply concurrency protocol appends at settlement
// (audit/grooming_window.go): one chained entry per grooming-report artifact
// whose disposition-capture window has been settled, carrying the artifact id
// and the settlement (approved / rejected). After it lands a capture for that
// artifact is refused (409 grooming_window_closed) and the settlement consumes
// exactly the dispositions recorded below it. It is INTERNAL, audit-only — it
// renders no issue comment.
// E54.77 / #3232 added grooming_apply_started, written ONCE per apply by the
// server's on-approval hook (server/grooming_apply.go) immediately BEFORE the
// detached apply goroutine starts, carrying {candidate_count, budget_seconds,
// started_at}. It is the progress DENOMINATOR fishhawk_get_run_status reads
// against the grooming_mutation_applied rows that follow it, so an over-budget
// apply is visible while it runs rather than only once grooming_apply_completed
// lands. A degrade path (nothing dispatched) writes NO started row. It is
// INTERNAL, audit-only: it renders no issue comment.
// #3649 added campaign_admission_screened, written once per POST
// /v0/campaigns create whose ADVISORY admission screen found at least one
// candidate declared `runnable:no` or naming an implement-stage forbidden path
// (the create response's admission_screen block is the operator surface; this
// is the best-effort chain copy). It is INTERNAL, audit-only: it renders no
// issue comment.
// E54.6 / #2238 added campaign_grooming_source_resolved, written once per
// campaign created from an approved grooming run's ratified order (the third
// POST /v0/campaigns source): it carries the campaign id, the source
// run/stage/artifact ids, the report's content hash, the rank-ordered issue
// refs, the named exclusions, the applied limit and any acknowledged
// supersession. It is a SECOND copy of that provenance and deliberately NOT the
// system of record — the durable one is the campaigns.grooming_source column,
// written by the campaign row's own INSERT, because this emit (like every
// campaign audit emit) is best-effort AFTER persistence. It is an INTERNAL,
// audit-only category: it renders no issue comment.
// E67.96 / #2862 added plan_budget_calibration_crossing, the plan-gate marker
// written when the planner's PRE-calibration runtime estimate
// (raw_predicted_runtime_minutes) and its calibrated predicted_runtime_minutes
// straddle the resolved implement-stage budget — i.e. the fleet calibration
// factor moved the estimate ACROSS the threshold. It records both estimates,
// the number the gate actually read (max of the two), the implied factor, the
// fleet ratio, the resolved budget and the gate outcome, so a decision the
// factor influenced is reconstructable from the trail. A crossing is always
// OVER budget by construction (the gate takes the maximum), so gate_outcome is
// one of refused / decomposition_satisfied / override_acknowledged and NEVER
// within_budget. It is written on EVERY one of those branches — including the
// two that let the approval proceed — and is INTERNAL, audit-only: it renders
// no issue comment and gates nothing. E72.11 / #3447 added
// acceptance_verdict_unshipped, the stage-scoped marker reap-failure appends
// when an acceptance stage settled succeeded but its verdict upload never
// landed (server/reap_failure.go); its liveness is decided by sequence against
// the stage's newest acceptance_dispatched/acceptance_reopened anchor and any
// later acceptance_outcome_recorded entry, and fishhawk_await_audit reads it.
// E45.84 / #3619 added
// concern_auto_closed, the implement re-review's marker for a routed concern
// (addressed_pending) CLOSED because a COMPLETE, unanimously non-reject review
// round re-judged the post-fix-up tree and said nothing about it — the case no
// reviewer `confirmed` entry covers, which previously left the concern open
// forever. It is INTERNAL and advisory (system actor, no issue comment, no
// Notifier method): it carries the closing round's reviewer models and review
// sequences plus the basis (clean_re_review_round), so an operator reading the
// settled ledger can tell an auto-close from a reviewer confirm. When a new
// canonical category is introduced, add it here so
// operators can await it without the allow_unknown escape hatch;
// categories_completeness_test.go's AST sweep fails the build if a
// non-test backend audit-write emits a category absent from this map.
// E75.6 / #3734 added digest_marked_read, the GLOBAL-chain record of a
// captain advancing their per-repository digest read watermark
// (server/digest.go, POST /v0/digest/mark-read). It is appended BEFORE the
// watermark moves, belongs to no run, is not decision-bearing, and is
// deliberately NOT an issue-comment activity category (there is no run thread
// to render it on).
// E76.2 / #3765 (ADR-083 #3751) added the five captain-record categories —
// captain_assigned, captain_claimed, captain_handover_offered,
// captain_handover_withdrawn and captain_relinquished — written by
// backend/internal/captain's Store.Apply via AppendGlobalChainedTx. They are
// repo-keyed GLOBAL-chain entries (payload.repo) belonging to no run; the
// current captain is DERIVED from them (captain.Derive), never stored. They
// are not decision-bearing (decisionindex is untouched) and, like
// digest_marked_read, are deliberately NOT issue-comment activity categories:
// a handover is repo-scoped and has no run thread to render on.
// E77.2 / #3736 (ADR-081 #3727 D2/D3) added the three crew-message categories
// — crew_message_sent, crew_message_disposed and crew_message_escalated —
// written by backend/internal/crewmessage's Mailbox via AppendChainedTx (run
// anchor) or AppendGlobalChainedTx (issue / decision-record anchor). The chain
// is authoritative; the crew_messages table (0090) is rebuilt from them. They
// are INTERNAL, not issue-comment activity categories: no delivery surface and
// no server writer exists yet (docs/issue-comment-surfaces.md).
// E76.5 / #3768 (ADR-083 #3751 rule 7) added delegation_confirmed and
// delegation_lower_proposed, written by backend/internal/delegationconfirm's
// Store.Append via AppendGlobalChainedTx under the captain record's lock. Like
// the captain_* categories they are repo-keyed GLOBAL-chain entries
// (payload.repo + payload.workflow) belonging to no run, are not
// decision-bearing, and are deliberately NOT issue-comment activity
// categories.
// E79.1 / #3725 added scheduled_run_started, scheduled_run_skipped and
// scheduled_run_refused, written by backend/internal/scheduler's Ticker once
// per due schedule window it decides (a new run, an Idempotency-Key replay of
// an existing one, or an admission refusal carrying its code). They ride the
// GLOBAL chain because a refusal has no run; the run linkage travels in
// payload.run_id. Like the captain_* categories they are not issue-comment
// activity categories (docs/issue-comment-surfaces.md is untouched).
// #3921 (E79 / #3726) added the four upkeep categories. upkeep_report_recorded
// is written once per ingested upkeep_report artifact on
// POST /v0/runs/{run_id}/plan (the plan-stage discriminator's third additive
// sibling), carrying the content hash, per-source entry counts and the dedupe
// duplicates. upkeep_disposition_recorded, upkeep_finding_filed and
// upkeep_finding_skipped are emitted by the later upkeep children (#3923 /
// #3924: the captain's per-finding disposition and the apply step's
// filed/skipped outcome); registering them now lets fishhawk_await_audit arm
// on them before their writers land. Like grooming_report_recorded they are
// INTERNAL, not issue-comment activity categories
// (docs/issue-comment-surfaces.md is untouched).
// #3923 added upkeep_apply_window_closed, the upkeep family's WATERMARK
// (audit/grooming_window.go): the #3924 apply appends one per upkeep-report
// artifact it settles, on approve AND reject, after which a capture for that
// artifact is refused (409 upkeep_window_closed). INTERNAL, audit-only.
// #3924 added upkeep_apply_completed: the on-approval upkeep apply's ONE
// summary row per apply (filed / skipped / failed / budget_exhausted counts,
// or degraded:true with a named degrade_reason when the apply did not run).
// INTERNAL, NOT an issue-comment activity category — like the other upkeep
// rows it is read through GET /v0/runs/{id}/audit only
// (docs/issue-comment-surfaces.md is untouched).
// #3763 (E80.6) added upkeep_inflight_pass_completed: the in-flight advisory
// pass's ONE summary row on the scan run (sent / already_sent / per-run skip
// counts, or degraded:true with a named degrade_reason). INTERNAL, audit-only,
// NOT an issue-comment activity category, like the other upkeep rows.
// E82.2 / #3779 (ADR-085 rules 3 and 6) added the two post-merge OUTCOME fact
// records written by backend/internal/mergeoutcome under the system actor
// "merge-outcome-observer": run_merge_reverted (a default-branch push whose
// revert signal resolves to a run's forge-confirmed merge, with the
// forge-diff attestation inverse_diff | partial_inverse | signal_only) and
// run_merge_ci_observed (the merge commit's CI conclusion fixed at maturity).
// Both are deduped per (run, reverting commit / merge commit), carry only
// forge-attested facts (no commit message, PR body or other prose), and are
// INTERNAL fact records, NOT issue-comment surfaces
// (docs/issue-comment-surfaces.md is untouched).
// E82.1 / #3778 (ADR-085 rule 4) added delegation_shadow_evaluated, the
// record-only BLIND shadow stamp: server/delegation_shadow.go appends one,
// actor system, after a HUMAN's decision on a delegable class (approve,
// route_fixup, waive, retry, merge), recording what delegation would have done
// on the pre-decision state. It grants nothing, is INTERNAL, and must never be
// an issue-comment activity category (docs/issue-comment-surfaces.md records
// it as deliberately not a surface).
// #3964 (ADR-087) added stage_concurrency_queued and
// stage_concurrency_admitted: server/stage_concurrency.go appends one queued
// row per queue episode when the host-dispatch marker queues a grouped local
// stage behind its group's holders, and one admitted row on every grouped
// admission. Best-effort, actor system, INTERNAL — deliberately NOT
// issue-comment activity categories (read through GET /v0/runs/{id}/audit).
// #4012 (E3775.2) added the seven comms categories as FORWARD registrations,
// the #3921 upkeep precedent: comms_report_recorded, comms_disposition_recorded
// and comms_apply_window_closed are the comms window family's report, capture
// and watermark rows (audit/grooming_window.go, reached through the generic
// FamilyWindowAppender), and comms_scan_gathered, comms_draft_filed,
// comms_draft_skipped and comms_apply_completed are the gather and apply rows.
// Nothing emits them yet — their writers are the later #3775 comms phases —
// and registering them now lets fishhawk_await_audit arm on them before those
// writers land. INTERNAL, NOT issue-comment activity categories
// (docs/issue-comment-surfaces.md is untouched).
// E35.4 / #1601 (ADR-053 option A) added alert_incident_filed and
// alert_incident_occurrence, written by the HMAC-authenticated
// POST /v0/triggers/alert ingress (server/alert_trigger.go) for an ACCEPTED
// alert only — a rejection is never audited, so an unauthenticated caller
// cannot append to the chain. They ride the GLOBAL chain because an incident
// belongs to no run (an auto-started run travels in payload.auto_start.run_id),
// and they are NOT issue-comment activity categories: the incident issue and
// its occurrence comments are their own egress surfaces
// (docs/issue-comment-surfaces.md).
// E72.59 / #4077 added review_round_redispatched, written by the boot sweep
// (server/review_redispatch.go) once per ADVISORY plan/implement review round
// orphaned by a daemon restart that it re-dispatches against the same plan
// artifact or reviewed head instead of synthesizing *_review_failed. It names
// the orphaned round's *_review_started sequence, which is also the cross-boot
// crash-loop guard: a round already named by one is never re-dispatched again.
// INTERNAL, actor system, NOT an issue-comment activity category
// (docs/issue-comment-surfaces.md is untouched).
var KnownCategories = map[string]struct{}{
	"acceptance_dispatched":                   {},
	"acceptance_outcome_recorded":             {},
	"acceptance_recorded":                     {},
	"acceptance_reopened":                     {},
	"acceptance_scenario_regression":          {},
	"acceptance_scenario_retirement_dropped":  {},
	"acceptance_scenarios_pushed":             {},
	"acceptance_skipped_out_of_scope":         {},
	"acceptance_stage_omitted":                {},
	"acceptance_triage_arbitrated":            {},
	"acceptance_triage_decided":               {},
	"acceptance_verdict_unshipped":            {},
	"agent_request_failed_alert":              {},
	"alert_incident_filed":                    {}, // E35.4 / #1601: an accepted alert filed a new incident issue (global chain)
	"alert_incident_occurrence":               {}, // E35.4 / #1601: a repeat alert commented on its existing incident issue (global chain)
	"anchor_ping_posted":                      {},
	"api_token_issued":                        {},
	"api_token_revoked":                       {},
	"approval_comment_refused":                {},
	"approval_conditions_truncated":           {},
	"approval_conditions_unrecorded":          {},
	"approval_predicate_rejected":             {},
	"approval_sla_elapsed":                    {},
	"approval_submitted":                      {},
	"audit_check_publish_degraded":            {},
	"audit_check_publish_recovered":           {},
	"branch_rebased":                          {},
	"branch_reset":                            {},
	"budget_alert":                            {},
	"budget_alert_sent":                       {},
	"campaign_admission_screened":             {},
	"campaign_advanced":                       {},
	"campaign_cancelled":                      {},
	"campaign_gate_acted":                     {},
	"campaign_gate_paged":                     {},
	"campaign_grooming_source_resolved":       {},
	"campaign_issue_restarted":                {},
	"campaign_issue_settled":                  {},
	"campaign_issue_started":                  {},
	"campaign_item_autonomy_refreshed":        {},
	"campaign_paused":                         {},
	"captain_assigned":                        {},
	"captain_claimed":                         {},
	"captain_handover_offered":                {},
	"captain_handover_withdrawn":              {},
	"captain_relinquished":                    {},
	"child_pushed":                            {},
	"conflict_resolution_pushed":              {},
	"child_redriven":                          {},
	"children_settled":                        {},
	"ci_failure_retry_dispatched":             {},
	"ci_green":                                {},
	"ci_retry_exhausted":                      {},
	"ci_retry_skipped":                        {},
	"ci_retriggered":                          {}, // E83.49 / #4082: fishhawk_retrigger_ci re-ran the PR's failed CI at the current head (server/retrigger_ci.go) — INTERNAL
	"clarification_answered":                  {},
	"clarification_answers_truncated":         {},
	"clarification_requested":                 {},
	"comms_apply_completed":                   {}, // #4012: the comms apply's one summary row (writer: comms apply phase)
	"comms_apply_window_closed":               {}, // #4012: comms capture-window watermark (writer: comms apply phase, via audit.FamilyWindowAppender)
	"comms_disposition_recorded":              {}, // #4012: the captain disposed a comms-report entry (writer: comms capture phase)
	"comms_draft_filed":                       {}, // #4012: the comms apply filed a draft (writer: comms apply phase)
	"comms_draft_skipped":                     {}, // #4012: the comms apply skipped a draft (writer: comms apply phase)
	"comms_report_recorded":                   {}, // #4012: one per ingested comms report artifact (writer: comms ingest phase)
	"comms_scan_gathered":                     {}, // #4012: the comms gather's one scan summary row (writer: comms gather phase)
	"concern_addressed_by_condition":          {},
	"concern_auto_closed":                     {},
	"concern_defer_failed":                    {},
	"concern_deferred":                        {},
	"concern_note_backfilled":                 {},
	"concern_relitigation_suppressed":         {},
	"concern_resolution_vetoed":               {},
	"concern_resolve_failed":                  {}, // E83.53 / #4086: corrective after a failed resolve transition (server/resolve_concerns.go)
	"concern_resolved_with_evidence":          {}, // E83.53 / #4086: a human operator resolved a routed concern as addressed on operator evidence (server/resolve_concerns.go)
	"concern_waive_failed":                    {},
	"concern_waived":                          {},
	"consolidated_pr_opened":                  {},
	"consolidated_review_diff_truncated":      {},
	"cost_recorded":                           {},
	"crew_finding_convert_failed":             {},
	"crew_finding_converted":                  {},
	"crew_message_delivered":                  {},
	"crew_message_disposed":                   {},
	"crew_message_escalated":                  {},
	"crew_message_sent":                       {},
	"crew_work_request_filed":                 {},
	"decomposition_child_cancelled":           {}, // #4186: a parent cancel or the orphan backfill cancelled this decomposition child (childcancel) — INTERNAL, not in issuecomment activityCategories
	"delegation_confirmed":                    {},
	"delegation_lower_proposed":               {},
	"delegation_shadow_evaluated":             {},
	"deploy_preflight_refused":                {},
	"deploy_run":                              {},
	"deployment_dispatch_failed":              {},
	"deployment_dispatched":                   {},
	"deployment_outcome_recorded":             {},
	"deployment_rollback_completed":           {},
	"deployment_rollback_initiated":           {},
	"diff_secrets_detected":                   {}, // E80.3 / #3760: the deterministic diff secrets check raised server_check concerns for credential-shaped additions (server/diff_secrets.go) — INTERNAL, locations and pattern classes only
	"digest_marked_read":                      {},
	"dispatch_reaper_failed":                  {},
	"document_injected":                       {},
	"document_injection_degraded":             {}, // E55.7 / #3746: run-admission declarations withheld from a served prompt (repodoc.RecordWithheld)
	"document_truncated":                      {},
	"dispatch_watchdog_elapsed":               {},
	"escalation_fired":                        {},
	"escalation_persona_attached":             {}, // E55.9 / #3754: fired escalations attached reviewer personas to a review round (server/escalation_persona.go)
	"fixup_no_changes":                        {},
	"fixup_pushed":                            {},
	"gate_isolation_recorded":                 {}, // E51.2 / #2135: which ADR-063 gate isolation path a stage's gates ran under, recorded at raw trace upload (server/gate_isolation.go) — INTERNAL, read by the gate view
	"grooming_apply_completed":                {},
	"grooming_apply_started":                  {}, // E54.77 / #3232: once-per-apply progress denominator (server/grooming_apply.go)
	"grooming_apply_window_closed":            {},
	"grooming_churn_filtered":                 {},
	"grooming_disposition_recorded":           {},
	"grooming_mutation_applied":               {},
	"grooming_report_recorded":                {},
	"host_dispatch_refused":                   {}, // E72.13 / #3500: dev-mode host-dispatch refusal (server/devmode.go)
	"implement_review_backstop_elapsed":       {},
	"implement_review_diff_truncated":         {},
	"implement_review_failed":                 {},
	"implement_review_skipped":                {},
	"implement_review_started":                {},
	"implement_reviewed":                      {},
	"implement_security_findings":             {},
	"fixup_concern_unattempted":               {},
	"fixup_pr_body_unsatisfiable":             {},
	"fixup_report_obligations_declared":       {},
	"fixup_reporting_obligation_undelivered":  {},
	"installation_token_issued":               {},
	"integration_commit_recorded":             {},
	"invariant_violation":                     {},
	"issue_commented":                         {},
	"issue_context_unresolved":                {},
	"lineage_violation":                       {},
	"mcp_token_issued":                        {},
	"merge_observation_recorded":              {},
	"merge_verdict_recorded":                  {},
	"merge_candidate_verified":                {}, // E83.33 / #4018 (ADR-090): the result of a runner verify-only merge-candidate pass, bound to EXACTLY one head SHA; CONSUMES its trigger (server/merge_candidate_verify.go) — INTERNAL, not an issue-comment surface
	"model_resolved":                          {},
	"operator_commit_vouched":                 {},
	"operator_scope_path_undelivered":         {},
	"parent_awaiting_child_scope_decision":    {},
	"parent_awaiting_redrive":                 {},
	"permission_drift_detected":               {}, // E80.4 / #3761: the deterministic permission-drift check raised server_check concerns for widened permission surfaces (server/permission_drift.go) — INTERNAL, surfaces/paths/keys/values only
	"permission_drift_raise_failed":           {}, // E80.4 / #3761: InsertRaised failed twice after permission_drift_detected landed, so the detected entry is the only record — INTERNAL
	"permission_narrowing_noticed":            {}, // E80.4 / #3761: the permission-drift check saw narrowings (never a concern) — INTERNAL
	"plan_acceptance_precheck":                {},
	"plan_add_scope_files_fans_into_slices":   {},
	"plan_budget_calibration_crossing":        {},
	"plan_budget_override_acknowledged":       {},
	"plan_coerced":                            {},
	"plan_comment_only_override_acknowledged": {},
	"plan_comment_only_override_refused":      {},
	"plan_decomposed":                         {},
	"plan_generated":                          {},
	"plan_generated_surface_retry":            {},
	"plan_missing_for_implement":              {},
	"plan_missing_required_tests":             {},
	"plan_periodic_budget_tier_acknowledged":  {},
	"plan_reaction_observed":                  {},
	"plan_reused_from":                        {},
	"plan_review_backstop_elapsed":            {},
	"plan_review_failed":                      {},
	"plan_review_skipped":                     {},
	"plan_review_started":                     {},
	"plan_reviewed":                           {},
	"plan_revised":                            {},
	"plan_schema_retry":                       {},
	"plan_scope_cap_override_acknowledged":    {},
	"plan_scope_cap_override_refused":         {},
	"plan_scope_precheck":                     {},
	"plan_scope_regression":                   {},
	"plan_scope_retry":                        {},
	"plan_surface_sweep":                      {},
	"plan_test_sweep":                         {},
	"plan_warnings":                           {},
	"plan_violates_budget":                    {},
	"plan_violates_periodic_budget":           {},
	"plan_violates_scope_cap":                 {},
	"policy_evaluated":                        {},
	"post_merge_observed":                     {},
	"pr_approved_on_github":                   {},
	"pr_closed_without_merge":                 {},
	"pr_merged":                               {},
	"pr_review_posted":                        {},
	"pr_review_submitted":                     {},
	"pr_status_comment_posted":                {},
	"precedent_divergence":                    {}, // E75.5 / #3733: decision went against clear precedent (server/gate_divergence.go)
	"precedent_divergence_answered":           {}, // E75.5 / #3733: the captain's one_off / doctrine_change answer
	"precedent_surfaced":                      {}, // E75.4 / #3732: precedent shown at an open human gate (server/gate_precedent.go)
	"product_report_filed":                    {},
	"pull_request_closed_after_review_reject": {},
	"pull_request_failed":                     {},
	"push_notification_failed":                {},
	"push_notification_sent":                  {},
	"push_resume_checkpoint":                  {},
	"pull_request_opened":                     {},
	"refinement_draft_approved":               {},
	"refinement_draft_edited":                 {},
	"refinement_draft_rejected":               {},
	"refinement_filing_completed":             {},
	"release_cut":                             {},
	"release_published":                       {},
	"review_head_mismatch":                    {}, // #3655: reviewed tree != pushed tree on a success ship (server/pullrequest.go)
	"review_round_redispatched":               {}, // #4077: boot re-dispatch of a restart-orphaned advisory review round (server/review_redispatch.go)
	"reviewer_capability_unavailable":         {},
	"run_admitted_applies_to_override":        {},
	"run_admitted_budget_override":            {},
	"run_auto_advanced":                       {},
	"run_auto_driven":                         {},
	"run_branches_swept":                      {},
	"run_budget_exceeded":                     {},
	"run_completed":                           {},
	"run_dispatched":                          {},
	"run_merge_ci_observed":                   {}, // E82.2 / #3779: merge-commit CI conclusion fixed at maturity
	"run_merge_reverted":                      {}, // E82.2 / #3779: a forge-confirmed revert of the run's merge
	"run_rejected_applies_to":                 {},
	"run_rejected_budget":                     {},
	"run_rejected_misconfigured":              {},
	"run_rejected_missing_charter":            {},
	"run_revived":                             {},
	"run_revived_on_reopen":                   {}, // E83.49 / #4082: a quick PR reopen revived a PR-close-cancelled run to its review gate (server/pullrequest_reopen.go) — operator-visible
	"run_revive_on_reopen_refused":            {}, // E83.49 / #4082: a PR reopen of a PR-close-cancelled run was refused, naming the guard (server/pullrequest_reopen.go) — INTERNAL
	"runner_kind_mismatch":                    {},
	"runner_kind_resolved":                    {},
	"runtime_observed":                        {},
	"scheduled_run_refused":                   {}, // E79.1 / #3725: a due schedule window refused at admission (scheduler.Ticker, global chain)
	"scheduled_run_skipped":                   {}, // E79.1 / #3725: a due schedule window replayed an existing run (already_started)
	"scheduled_run_started":                   {}, // E79.1 / #3725: a due schedule window started a new run
	"scope_amendment_decided":                 {},
	"scope_amendment_requested":               {},
	"scope_completeness_amended":              {},
	"scope_completeness_exempted":             {},
	"scope_completeness_failed":               {},
	"scope_completeness_parked":               {},
	"scope_files_exempted":                    {},
	"slice_head_missing":                      {},
	"slice_integration_conflict":              {},
	"slice_integration_failed":                {},
	"slices_integrated":                       {},
	"split_children_filed":                    {},
	"split_filing_refused":                    {},
	"split_parent_closed":                     {},
	"spend_alert":                             {},
	"stage_budget_exceeded":                   {},
	"stage_concurrency_admitted":              {},
	"stage_concurrency_queued":                {},
	"stage_conflict_resolution_failed":        {},
	"stage_conflict_resolution_triggered":     {},
	"stage_merge_candidate_verify_triggered":  {}, // E83.33 / #4018 (ADR-090): the durable trigger that re-opens the implement stage for a verify-only merge-candidate pass; never reads or spends the fix-up budget (server/merge_candidate_verify.go) — INTERNAL, not an issue-comment surface
	"stage_fixup_recovered":                   {},
	"stage_fixup_triggered":                   {},
	"stage_override_retried":                  {},
	"stage_permissions_declared":              {},
	"stage_retried":                           {},
	"stage_superseded_by_merge":               {},
	"stale_run_swept":                         {}, // #4185: `fishhawkd sweep-stale-runs --apply` transitioned this stale non-terminal top-level run (stalesweep) — INTERNAL, not in issuecomment activityCategories
	"status_comment_posted":                   {},
	"trace_uploaded":                          {},
	"unpriced_model_alert":                    {},
	"upkeep_apply_completed":                  {}, // #3924: the upkeep apply's one summary row (filed/skipped/failed counts, or a named degrade)
	"upkeep_apply_window_closed":              {}, // #3923: upkeep capture-window watermark (writer: #3924 apply, via audit.UpkeepWindowAppender)
	"upkeep_disposition_recorded":             {}, // #3921: the captain disposed an upkeep finding (writer: #3923)
	"upkeep_finding_filed":                    {}, // #3921: the upkeep apply filed a finding's issue (writer: #3924)
	"upkeep_finding_skipped":                  {}, // #3921: the upkeep apply skipped a finding (writer: #3924)
	"upkeep_inflight_pass_completed":          {}, // #3763: the in-flight advisory pass's one summary row on the scan run (sent/already_sent/skips, or a named degrade)
	"upkeep_report_recorded":                  {}, // #3921: one per ingested upkeep_report artifact
	"verified_tree_discarded":                 {},
	"verify_resume_checkpoint":                {}, // E83.80 / #4190: a reverify-kind held-commit checkpoint (server/pullrequest.go); INTERNAL, not an issue-comment activity line
	"work_item_filed":                         {},
	"work_item_transitioned":                  {},

	// Kept as its own alignment section (the keys outrun the block's column):
	// E83.52 / #4085, the PARTIAL delivery pair, both issue-comment ACTIVITY
	// categories. closing_reference_neutralized: the ship-time guard rewrote a
	// closing reference to the issue as Refs #N (server/partial_delivery_pr.go).
	// remaining_scope_posted: the merge-time remaining-scope comment landed on
	// the issue, and is its dedup key (server/partial_delivery_merge.go).
	"partial_delivery_closing_reference_neutralized": {},
	"partial_delivery_remaining_scope_posted":        {},
}

// knownCategoryList is the sorted slice form of KnownCategories, computed
// once at package init so KnownCategoryList / SuggestCategories return a
// stable, deterministic order without re-sorting per call.
var knownCategoryList = func() []string {
	out := make([]string, 0, len(KnownCategories))
	for c := range KnownCategories {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}()

// IsKnownCategory reports whether category is a canonical audit-log
// category in the curated registry.
func IsKnownCategory(category string) bool {
	_, ok := KnownCategories[category]
	return ok
}

// KnownCategoryList returns the registry as a sorted slice (a fresh copy so
// callers cannot mutate the shared backing array).
func KnownCategoryList() []string {
	out := make([]string, len(knownCategoryList))
	copy(out, knownCategoryList)
	return out
}

// SuggestCategories returns up to max known categories nearest to input by
// Levenshtein edit distance, closest first, ties broken lexicographically
// (deterministic). It is the "did you mean" suggester the fail-loud
// validation surfaces: SuggestCategories("scope_amendment_pending", 3)
// ranks "scope_amendment_requested" first. max <= 0 or an empty registry
// returns nil.
func SuggestCategories(input string, max int) []string {
	if max <= 0 {
		return nil
	}
	type scored struct {
		category string
		dist     int
	}
	ranked := make([]scored, 0, len(knownCategoryList))
	for _, c := range knownCategoryList {
		ranked = append(ranked, scored{category: c, dist: levenshtein(input, c)})
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].dist != ranked[j].dist {
			return ranked[i].dist < ranked[j].dist
		}
		return ranked[i].category < ranked[j].category
	})
	if max > len(ranked) {
		max = len(ranked)
	}
	out := make([]string, 0, max)
	for i := 0; i < max; i++ {
		out = append(out, ranked[i].category)
	}
	return out
}

// levenshtein computes the edit distance between a and b with the standard
// two-row dynamic-programming table. Self-contained (no external
// dependency) — the registry is small, so the O(len(a)*len(b)) cost per
// candidate is negligible.
func levenshtein(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	la, lb := len(ra), len(rb)
	if la == 0 {
		return lb
	}
	if lb == 0 {
		return la
	}
	prev := make([]int, lb+1)
	curr := make([]int, lb+1)
	for j := 0; j <= lb; j++ {
		prev[j] = j
	}
	for i := 1; i <= la; i++ {
		curr[0] = i
		for j := 1; j <= lb; j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			curr[j] = min3(prev[j]+1, curr[j-1]+1, prev[j-1]+cost)
		}
		prev, curr = curr, prev
	}
	return prev[lb]
}

func min3(a, b, c int) int {
	m := a
	if b < m {
		m = b
	}
	if c < m {
		m = c
	}
	return m
}
