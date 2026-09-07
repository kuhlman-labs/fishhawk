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
// stages. An entry demanding an OPERATOR-vocabulary scope for a tool the in-run
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

// scopeRunBoundRetry and scopeRunBoundScopeAmendments are the two conditional
// run-bound scopes (mcptoken.go: write:retries when the stage's spec sets
// executor.agent_self_retry, write:scope-amendments on implement stages).
const (
	scopeRunBoundRetry           = "write:retries"
	scopeRunBoundScopeAmendments = "write:scope-amendments"
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
	"fishhawk_get_active_run":        mcpScopeAuthenticatedOnly,
	"fishhawk_get_plan":              mcpScopeAuthenticatedOnly,
	"fishhawk_get_run_status":        mcpScopeAuthenticatedOnly,
	"fishhawk_list_audit":            mcpScopeAuthenticatedOnly,
	"fishhawk_list_runs":             mcpScopeAuthenticatedOnly,
	"fishhawk_runtime_calibration":   mcpScopeAuthenticatedOnly,
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
	"fishhawk_doctor": mcpScopeAuthenticatedOnly,
	"fishhawk_init":   mcpScopeAuthenticatedOnly,

	// fishhawk_release_notes defaults to mode=preview, which dials the
	// authenticated read GET /v0/releases/notes/preview (release_notes.go
	// handleReleaseNotesPreview: 401 anonymous, no scope). mode=prepare
	// additionally POSTs /v0/releases/notes, which DOES require write:runs —
	// but the mode is a BODY field this header-driven gate cannot see, and
	// refusing preview for want of a write scope would be STRICTER than the
	// endpoint. The persist arm stays enforced by its own handler.
	"fishhawk_release_notes": mcpScopeAuthenticatedOnly,

	// fishhawk_file_issue POSTs /v0/work-items, which enforces no scope:
	// workitems.go gates on run ENTITLEMENT (runBoundTokenRunID) and
	// repo consistency, not on a scope predicate.
	"fishhawk_file_issue": mcpScopeAuthenticatedOnly,

	// gateview.go handleGetRunGateView: a run-bound subject is authorized by
	// its own run; every other identity needs scopeGateViewRead (read:audit).
	"fishhawk_get_gate_view": {anyOf: []string{scopeGateViewRead}, runBoundSubjectOK: true},

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
	"fishhawk_reap_stage":         {anyOf: []string{"write:runs"}}, // reap_failure.go handleReapStageFailure
	"fishhawk_dispatch_stage":     {anyOf: []string{"write:runs"}}, // host_dispatch.go handleHostDispatchStage
	"fishhawk_run_stage":          {anyOf: []string{"write:runs"}}, // host_dispatch.go handleHostDispatchStage
	"fishhawk_run_children":       {anyOf: []string{"write:runs"}}, // host_dispatch.go handleHostDispatchStage

	// --- write:stages.
	"fishhawk_rebase_run_branch":         {anyOf: []string{"write:stages"}}, // rebase_branch.go handleRebaseRunBranch
	"fishhawk_vouch_commit":              {anyOf: []string{"write:stages"}}, // vouch.go handleVouchCommit
	"fishhawk_decide_scope_amendment":    {anyOf: []string{"write:stages"}}, // scope_amendment.go handleDecideScopeAmendment
	"fishhawk_decide_scope_completeness": {anyOf: []string{"write:stages"}}, // scope_completeness.go handleDecideScopeCompleteness

	// --- any-of pairs. These are the entries the fhm_ non-regression turns
	// on: retry.go and revive.go accept `write:stages OR write:retries`, and a
	// self-retrying implement agent holds ONLY write:retries.
	"fishhawk_retry_stage": {anyOf: []string{"write:stages", scopeRunBoundRetry}}, // retry.go handleRetryStage
	"fishhawk_revive_run":  {anyOf: []string{"write:stages", scopeRunBoundRetry}}, // revive.go handleReviveRun

	// fixup.go / waive.go / defer_concern.go: `write:stages OR write:fixups`.
	"fishhawk_fixup_stage":   {anyOf: []string{"write:stages", scopeFixupAlternate}},
	"fishhawk_waive_concern": {anyOf: []string{"write:stages", scopeFixupAlternate}},
	"fishhawk_defer_concern": {anyOf: []string{"write:stages", scopeFixupAlternate}},

	// --- write:approvals.
	"fishhawk_approve_plan":                 {anyOf: []string{"write:approvals"}}, // approvals.go handleSubmitApproval
	"fishhawk_reject_plan":                  {anyOf: []string{"write:approvals"}}, // approvals.go handleSubmitApproval
	"fishhawk_answer_clarification":         {anyOf: []string{"write:approvals"}}, // clarification_answer.go handleAnswerClarification
	"fishhawk_revise_plan":                  {anyOf: []string{"write:approvals"}}, // revise.go handleRevisePlan
	"fishhawk_merge_run":                    {anyOf: []string{"write:approvals"}}, // merge_run.go handleMergeRun
	"fishhawk_arbitrate_acceptance":         {anyOf: []string{"write:approvals"}}, // acceptance_arbitration.go handleAcceptanceArbitration
	"fishhawk_record_grooming_dispositions": {anyOf: []string{"write:approvals"}}, // grooming_dispositions.go handleRecordGroomingDispositions

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
	"fishhawk_start_campaign":          {anyOf: []string{"write:campaigns"}},
	"fishhawk_start_campaign_item_run": {anyOf: []string{"write:campaigns"}},
	"fishhawk_resume_campaign":         {anyOf: []string{"write:campaigns"}},
	"fishhawk_cancel_campaign":         {anyOf: []string{"write:campaigns"}},

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
