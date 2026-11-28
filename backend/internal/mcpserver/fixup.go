package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// PRBodyObligation mirrors one entry of the fix-up response's optional
// pr_body_obligations field (#2782): a routed instruction that named the
// pull-request body — a surface a fix-up pass cannot write. The verb renders
// its operator warning FROM this server-minted set (naming the arrival channel
// and the id), so the ids the operator sees at routing time are the SAME ids
// the reviewer sees later. TextExcerpt is inert DATA — the verb deliberately
// never renders it into the warning, so an Untrusted (acceptance-derived)
// excerpt cannot become an injection path. Each json tag MUST byte-match the
// backend or the field silently decodes to its zero value (the #371 trap).
type PRBodyObligation struct {
	ID          string `json:"id"`
	Source      string `json:"source"`
	TextExcerpt string `json:"text_excerpt"`
	Untrusted   bool   `json:"untrusted,omitempty"`
}

// fixupStageWithObligations posts the fix-up (`POST /v0/stages/{id}/fixup`) and
// returns the re-opened Stage PLUS the server-minted PR-body obligation set
// (#2782) from the SAME response, so the verb can warn the operator FROM the
// response rather than re-deriving anything locally. It lives beside the verb
// (not in the generic client surface) because `pr_body_obligations` is specific
// to this endpoint; it reuses the client's marshal/do path. The Stage fields
// promote to the top level of the response, so the embedded Stage decodes
// exactly as before; pr_body_obligations is the optional sibling (omitempty),
// nil for the common pass and on an older backend that omits it.
func (c *apiClient) fixupStageWithObligations(ctx context.Context, id uuid.UUID, in FixupStageInput) (*Stage, []PRBodyObligation, error) {
	// instruction_targets (E83.37 / #4022) rides beside the shared fixupRequest
	// fields — embedded so they promote to the top level of the body — and is
	// passed through verbatim: the backend owns its validation.
	body, err := json.Marshal(struct {
		fixupRequest
		InstructionTargets map[string]string `json:"instruction_targets,omitempty"`
	}{
		fixupRequest: fixupRequest{
			ConcernIDs:          in.ConcernIDs,
			Concerns:            in.Concerns,
			Reason:              in.Reason,
			AllowCreate:         in.AllowCreate,
			ForceAdditionalPass: in.ForceAdditionalPass,
			ImplementModel:      in.ImplementModel,
			OperatorConcern:     in.OperatorConcern,
			OperatorEvidence:    in.OperatorEvidence,
		},
		InstructionTargets: in.InstructionTargets,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("marshal fixup: %w", err)
	}
	var resp struct {
		Stage
		PRBodyObligations []PRBodyObligation `json:"pr_body_obligations,omitempty"`
	}
	if err := c.do(ctx, http.MethodPost, "/v0/stages/"+id.String()+"/fixup", body, &resp); err != nil {
		return nil, nil, err
	}
	return &resp.Stage, resp.PRBodyObligations, nil
}

// FixupStageInput is the fishhawk_fixup_stage tool's input schema
// (E22.X / #762). Mirrors `POST /v0/stages/{stage_id}/fixup`.
// ConcernIDs is the PRIMARY addressing scheme (#964): stable concern
// UUIDs surfaced by fishhawk_get_run_status's run.concerns block — plus,
// since E45.83 / #3618, the SUPERSEDED implement-stage ids a retry discarded,
// which are read from fishhawk_get_gate_view's settled[] ledger because the
// run-status items[] lists open concerns only.
// Concerns (positional indices into the stage's flattened resolved
// concern set) is DEPRECATED — ambiguous once multiple heterogeneous
// review entries exist per stage — and only valid when ConcernIDs is
// absent; supplying both is rejected. Reason is an optional operator
// AUDIT NOTE recorded on the fix-up audit entry and as the routed
// concerns' state_reason; the fix-up agent never sees it (#5561).
type FixupStageInput struct {
	StageID    string   `json:"stage_id" jsonschema:"the Fishhawk implement stage UUID to fix up (parked at the implement-review gate, or succeeded with the run's review gate still open)"`
	ConcernIDs []string `json:"concern_ids,omitempty" jsonschema:"PRIMARY addressing: stable concern UUIDs to route back to the agent (from fishhawk_get_run_status's run.concerns.items[].id). A SUPERSEDED implement-stage concern id IS routable too (E45.83 / #3618) — a retry DISCARDS the prior attempt's open concerns, and routing one re-opens it against the NEW tree with its reviewer, round and severity preserved, so free-text operator_concern is no longer the only recovery after a retry. Superseded ids do NOT appear in run.concerns.items[], which lists OPEN concerns only — read them from fishhawk_get_gate_view's settled[] ledger (run.concerns.superseded_implement counts them). addressed/waived/deferred/addressed_by_condition ids are rejected. At least one of concern_ids/concerns required; supplying both is rejected"`
	Concerns   []int    `json:"concerns,omitempty" jsonschema:"DEPRECATED positional fallback: indices into the stage's flattened implement-review concern set. Ambiguous when multiple review entries exist per stage — prefer concern_ids. Only valid when concern_ids is absent"`
	Reason     string   `json:"reason,omitempty" jsonschema:"optional operator AUDIT NOTE, recorded on the stage_fixup_triggered audit entry and as the routed concerns' state_reason. The fix-up agent NEVER sees it: it is not rendered into the fix-up prompt and is not an instruction channel. Put anything the agent must act on in operator_concern (delivered as a binding instruction and tracked to closure, #2623); setting reason without operator_concern returns a warnings entry."`
	// AllowCreate declares net-new files this fix-up will create (#823).
	AllowCreate []string `json:"allow_create,omitempty" jsonschema:"optional repo-relative paths the fix-up will CREATE; folded into the effective scope.files for THIS pass only (bounded, explicit, operator-authorized) so the runner stages them instead of failing category-B created-out-of-scope. Any created file NOT declared here still fails category-B."`
	// ForceAdditionalPass is the bounded operator override (#860).
	ForceAdditionalPass bool `json:"force_additional_pass,omitempty" jsonschema:"bounded operator override: set true to grant ONE fix-up pass BEYOND the normal budget when it is already spent (you got fixup_budget_exhausted) but a concern still needs the agent. Hard-capped at the ceiling; the forced pass is audited (forced flag + your reason). At the ceiling the tool returns fixup_ceiling_reached and the override no longer helps. NOT the recourse after a crash: a pass that delivered NOTHING to the PR branch (no commit, or a category-A/category-C death that pushed nothing) is refunded against the normal budget AND credited against the ceiling automatically (#3335, up to 3 credits — absolute cap 6 triggered passes), so check the error details' crashed_without_push and remaining_ceiling_slots first. Default false."`
	// ImplementModel is the optional operator/driver model override for this
	// fix-up pass (#1164).
	ImplementModel string `json:"implement_model,omitempty" jsonschema:"optional operator/driver model override for THIS fix-up pass; default (empty) inherits the run's resolved implement model. Validated against the deployment per-adapter allow-list — a disallowed value returns fixup_invalid_model (422)."`
	// OperatorConcern is the free-text operator instruction routed back with NO
	// pre-existing review concern (#1311).
	OperatorConcern string `json:"operator_concern,omitempty" jsonschema:"optional free-text operator instruction delivered to the agent as a binding instruction with NO pre-existing review concern. Use for a required CI/CodeQL/SAST finding, an ad-hoc steer, or a missed edge case on a clean approve-with-zero-concerns diff. Folded into the routed concern set as a [high/operator] concern AND minted as a durable concern tracked to closure (#2623): if the agent does not address it, it stays open and surfaces as an OPEN concern at the gate — no longer silently dropped. At least one of concern_ids/concerns/operator_concern is required; operator_concern alone is admitted on a zero-concern gate-open stage. A whitespace-only or over-length (>4000 bytes) value returns validation_failed (400)."`
	// OperatorEvidence declares the operator executed a reproduction of the
	// routed concerns (E48.103 / #2551): an authority marker, not prose.
	OperatorEvidence string `json:"operator_evidence,omitempty" jsonschema:"optional declaration that YOU, the operator, EXECUTED a reproduction of the concern(s) this pass routes back. Changes AUTHORITY, not delivery — put the reproduction prose in operator_concern, in your own words (the agent never sees reason, and raw diff hunks do not belong in a binding instruction); this field records that the finding was verified by execution. Every concern routed by this pass is then EXEMPT from reviewer-confirmation auto-resolve: a later reviewer 'confirmed' verdict is vetoed (operator_evidence_routed) and the concern stays open until you close it yourself: fishhawk_resolve_concerns when it is fixed, fishhawk_waive_concern or fishhawk_defer_concern when it is not fixed but acceptable. PERMANENT for those concerns — use it when a reviewer has already retired a defect you can still reproduce. Also use it when re-routing an item a prior bundled pass dropped: describe the still-unaddressed item in operator_concern, and instruct the agent there to state any disagreement in its fix-up self-report rather than skip the item. Not a selection input (it does not by itself satisfy the concern_ids requirement). Whitespace-only or over-length (>4000 bytes) returns validation_failed (400)."`
	// InstructionTargets declares the surface each routed instruction asks the
	// pass to change (E83.37 / #4022), passed through to the backend verbatim.
	InstructionTargets map[string]string `json:"instruction_targets,omitempty" jsonschema:"optional per-instruction target: keys are a routed concern id from concern_ids or 'operator_concern'; values are 'code', 'pr_body', or 'docs'. A 'reason' key returns validation_failed (400) because reason never reaches the agent. An instruction with no key is 'code'. Declare 'pr_body' for an instruction that asks the agent to change the PULL-REQUEST BODY — a surface a fix-up pass cannot write — and the result carries a warning for it; the instruction's wording is never classified for that, so a code instruction that merely mentions the PR body raises nothing. Index-routed (deprecated positional) concerns cannot be targeted. A bad value, or a key naming an instruction this call does not route, returns validation_failed (400)."`
}

// FixupStageOutput surfaces the re-opened Stage row. A successful fix-up
// flips the implement stage awaiting_approval → pending (the orchestrator
// advances it to dispatched before the response returns, re-firing
// workflow_dispatch). Refusals — empty concerns, an out-of-range index,
// no recorded approve_with_concerns verdict, the bounded pass count
// already spent, the hard ceiling reached, or a cross-run token — surface
// as a tool error carrying the backend's error code (e.g.
// fixup_budget_exhausted or fixup_ceiling_reached).
type FixupStageOutput struct {
	Stage Stage `json:"stage"`
	// Warnings carries one advisory line per routed instruction declared with
	// the pr_body target (instruction_targets, E83.37 / #4022) — the
	// PULL-REQUEST BODY, a surface a fix-up pass has NO mechanism to write
	// (#2782). It is rendered FROM the server-minted pr_body_obligations set on
	// the fix-up response (never re-derived locally), so each line names the
	// arrival channel and the same obligation id the reviewer will see, and it
	// is ADVISORY: the fix-up still proceeded and returned its Stage. After the
	// pr_body lines it may carry ONE audit-only-reason advisory
	// (reasonAuditOnlyWarning, #5561: reason set without operator_concern), and
	// then ONE mixed-severity advisory (mixedSeverityWarning, E106.27 / #5212).
	// Present (omitempty) only when one of those three fired; an ordinary pass
	// leaves it nil.
	Warnings []string `json:"warnings,omitempty" jsonschema:"advisory warnings: one per routed instruction declared with the pr_body target (instruction_targets) — the PR body is a surface a fix-up pass cannot write; each line names the arrival channel and obligation id and points at the surfaces that DO work — followed by at most one reason advisory when reason is set without operator_concern (the agent never sees reason), then at most one mixed-severity advisory when the routed set mixes a high concern with medium or low ones (a bundled pass can fix the lower-severity items and skip the high one). Present only when one of those fired; the fix-up still proceeds regardless."`
	// NextSteps is the post-fix-up follow-through (E106.27 / #5212): an ORDERED
	// sequence, plural where fishhawk_dispatch_stage / fishhawk_await_stage /
	// fishhawk_await_children return a singular next_step, because the local
	// runner needs TWO calls in order (dispatch the re-opened pass, then await
	// its re-review) while every other runner needs only the wait.
	NextSteps []SuggestedAction `json:"next_steps,omitempty" jsonschema:"the post-fix-up follow-through, an ORDERED sequence: run the entries in order. Plural where fishhawk_dispatch_stage, fishhawk_await_stage and fishhawk_await_children return a singular next_step, because on the local runner (stage state awaiting_host_dispatch) the re-opened pass must first be dispatched (fishhawk_dispatch_stage, stage implement) and then its re-review awaited — two calls; otherwise it is the one wait. The wait is fishhawk_await_audit over the categories implement_reviewed, implement_review_failed and implement_review_skipped, anchored at since_sequence = fixup_trigger_sequence: count every terminal kind (reviewed, failed or skipped) toward expected_reviewers and re-arm with since_sequence = the returned latest_sequence until all of them have landed. When the anchor could not be read, since_sequence names where to read it (the stage_fixup_triggered entry's sequence, fishhawk_list_audit)."`
	// FixupTriggerSequence is the audit sequence of the stage_fixup_triggered
	// entry this pass recorded — the anchor the post-fix-up review wait arms
	// from. Omitted when the best-effort audit read failed.
	FixupTriggerSequence int64 `json:"fixup_trigger_sequence,omitempty" jsonschema:"the audit sequence of the stage_fixup_triggered entry this pass recorded: the since_sequence anchor for the post-fix-up review wait. Best-effort: omitted when the audit read failed (the fix-up still succeeded)"`
	// ExpectedReviewers is the configured reviewer count of the newest
	// implement_review_started entry (the prior round). Advisory.
	ExpectedReviewers int `json:"expected_reviewers,omitempty" jsonschema:"how many terminal review entries (implement_reviewed, implement_review_failed or implement_review_skipped) to wait for after the fix-up: the configured reviewer count of the newest implement_review_started entry (the prior round). Advisory — a workflow whose reviewer count changes mid-run differs; omitted when it could not be read"`
}

// registerFixupStage wires the fishhawk_fixup_stage tool (E22.X / #762).
//
// The implement-review fix-up routes one or more advisory implement-
// review concerns (ADR-027 approve_with_concerns) back to the implement
// agent for a bounded, operator-gated single pass, instead of the
// operator hand-editing the PR branch. The selected concerns are
// delivered to the agent as binding instructions (the #558 condition-
// delivery framing), the fix-up is committed onto the SAME PR branch
// (no new diff, no new PR — distinct from fishhawk_retry_stage), and the
// implement review re-runs on the result. The operator still owns the
// merge.
//
// Operator-gated + bounded: the bound defaults to one pass per stage,
// enforced server-side by counting prior stage_fixup_triggered audit
// entries; a second attempt returns a fixup_budget_exhausted tool error.
// A run-bound MCP token may fix up only stages within its own run.
//
// operator_evidence (E48.103 / #2551) declares the operator EXECUTED a
// reproduction of the routed concerns. It is an authority marker: those
// concerns can no longer be retired by a reviewer's `confirmed`
// delta-verification verdict — the operator closes them: fishhawk_resolve_concerns
// when fixed, fishhawk_waive_concern / fishhawk_defer_concern when not fixed but
// acceptable. Use it when a reviewer has already retired a defect you can still
// reproduce, or when re-routing an item a prior bundled pass dropped; the
// reproduction prose itself travels in operator_concern (reason never reaches
// the agent, #5561).
//
// Follow-through (E106.27 / #5212): on success the verb makes a best-effort
// audit read for this pass's stage_fixup_triggered anchor and the prior round's
// configured reviewer count, and returns the ordered next_steps (dispatch on the
// local runner, then the anchored review wait). A failed read never fails the
// verb. A fixup_ceiling_reached refusal is wrapped with the fresh-run recipe.
//
// Auth: write tool. Operator-side fhk_* tokens with `write:stages` (or
// the dedicated `write:fixups`) scope succeed; a token without either
// surfaces 403.
func registerFixupStage(srv *mcp.Server, resolver *runResolver) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "fishhawk_fixup_stage",
		Description: strings.TrimSpace(`
Route advisory implement-review concerns back to the implement agent for
a bounded, operator-gated fix-up pass.

Use this when the advisory implement review returned approve_with_concerns
and you want the agent to address one or more concerns, rather than hand-
editing the PR branch yourself. The fix-up:

  - re-invokes the implement agent with the selected concerns delivered as
    binding instructions (MANDATORY, win on conflict — the #558 framing);
  - commits onto the SAME PR branch and UPDATES the existing PR (it does
    NOT regenerate a fresh diff or open a new PR — distinct from
    fishhawk_retry_stage);
  - re-runs the implement review on the result.

Applies to an implement stage in either flow:
  - the implement stage is itself parked at the review gate
    (awaiting_approval), OR
  - the implement stage has succeeded (it opened the PR) and the run's
    SEPARATE review stage is still parked at awaiting_approval — the
    push_and_open_pr flow. The review stage is re-parked alongside the
    re-open so the fix-up flows back into a fresh review. Refused once
    the review gate has merged/resolved.
  - (#5249) the implement stage has succeeded, the review stage is still
    pending, and the acceptance stage is PARKED failed with
    failure_category C — the change itself broke the acceptance preview,
    so a retry re-fails on the same head. The fix-up re-parks acceptance
    failed -> pending, re-opens implement, and leaves review pending.
    Category A/B/D failures and an unsettled acceptance keep the refusal.

Inputs:
  - stage_id : the implement stage (parked at the review gate, or
    succeeded with the run's review gate still open).
  - concern_ids : PRIMARY addressing (#964) — stable concern UUIDs to
    route back (at least one). Read them from fishhawk_get_run_status's
    run.concerns.items[].id (open implement-stage concerns; a plan-stage or
    already-resolved ID is rejected). Routed concerns are marked
    addressed_pending in the durable concern store.

    A SUPERSEDED implement-stage id of this stage is ALSO accepted (E45.83 /
    #3618). A retry DISCARDS the prior attempt's open implement-review
    concerns by superseding them, so the gate reports no open concern even
    though nothing answered them; routing a superseded id RE-OPENS it against
    the new tree, preserving the reviewer, the origin review round and the
    severity, and it is then tracked to closure by the same
    delta-verification machinery as any routed concern. That means free-text
    operator_concern is no longer the only recovery after a retry — and it is
    the better one, because it keeps the reviewer provenance the free-text
    path destroys. Superseded ids are NOT in run.concerns.items[] (open
    concerns only): read them from fishhawk_get_gate_view's settled[] ledger,
    which carries each one's id, reviewer_model, severity, category and note;
    run.concerns.superseded_implement counts them, and the
    implement_gate_settled_after_supersede next_actions state points here.
    addressed / waived / deferred / addressed_by_condition ids stay rejected.
  - concerns : DEPRECATED positional fallback — indices into the stage's
    flattened implement_reviewed concern set. Ambiguous once multiple
    heterogeneous review entries exist per stage; prefer concern_ids.
    Only valid when concern_ids is absent (supplying both is rejected).
  - operator_concern : optional free-text operator instruction routed back
    to the agent with NO pre-existing review concern (#1311). Use it when a
    REQUIRED external check fails with no Fishhawk review concern — a
    CodeQL/SAST alert on a clean approve-with-zero-concerns diff — or for any
    ad-hoc steer that is not an implement-review concern. It is folded into
    the routed set as a [high/operator] binding concern and delivered to the
    agent on the run branch (sole-writer; no foreign hand-commit). It needs
    NO recorded approve_with_concerns verdict: operator_concern ALONE is
    admitted on a zero-concern gate-open stage. Since #2623 it is ALSO minted
    as a DURABLE concern tracked to closure exactly like an id-routed one: it is
    marked addressed_pending, threaded into the re-review's delta-verification,
    and will surface as an OPEN concern at the gate (in the run's concerns block
    and the gate view) if the agent does not address it — so a skipped operator
    instruction is no longer silently dropped. The mint is best-effort. A
    whitespace-only or over-length (>4000 bytes) value returns validation_failed.
  - reason   : optional operator AUDIT NOTE, recorded on the audit entry
    and as the routed concerns' state_reason. The fix-up agent NEVER sees it;
    put anything it must act on in operator_concern. Setting reason without
    operator_concern returns a "warnings" entry saying so.
  - allow_create : optional repo-relative paths the fix-up will CREATE.
    Each declared path is folded into the effective scope.files for THIS
    pass only (bounded, explicit, operator-authorized), so the runner
    stages the new file instead of failing the #818 created-out-of-scope
    gate. Use this when a concern requires a NET-NEW file. Any created
    file NOT declared here still fails category-B (the #818 fail-loud
    contract is preserved). Entries must be repo-relative (no absolute
    paths, no '..'); a bad entry returns validation_failed.
  - force_additional_pass : bounded operator override (#860). When the
    NORMAL budget is spent (you got fixup_budget_exhausted) but a concern
    still needs the agent, set this true to grant ONE pass beyond the
    budget. Hard-capped at the ceiling; the forced pass is audited (a
    'forced' flag plus your reason on the stage_fixup_triggered entry).
    Default false. NOT the recourse after a crash: a pass that delivered
    nothing to the PR branch is refunded against the normal budget AND
    credited against the ceiling automatically (#3085 / #3335, up to 3
    credits — absolute cap 6 triggered passes), so check the
    fixup_budget_exhausted details' crashed_without_push and
    remaining_ceiling_slots before spending the override.
  - instruction_targets : optional map declaring the surface each routed
    instruction asks the pass to change (E83.37 / #4022). Keys: a routed
    concern id from concern_ids, or "operator_concern". Values:
    "code" (the default for any instruction with no key), "pr_body", or
    "docs". A "reason" key returns validation_failed because reason never
    reaches the agent. Declare "pr_body" for an instruction that asks the agent to
    change the pull-request body (see below). Index-routed (deprecated
    positional) concerns cannot be targeted. A bad value, or a key naming an
    instruction this call does not route, returns validation_failed.
  - implement_model : optional operator/driver model override for THIS
    fix-up pass (#1164). Default (empty) inherits the run's already-resolved
    implement model — byte-identical to today. A non-empty value is
    validated against the deployment's per-adapter allow-list; a disallowed
    model returns fixup_invalid_model (422). The effective model is recorded
    on the stage_fixup_triggered audit entry and surfaced on the run's
    fixup_model status field.

A fix-up pass CANNOT update the pull-request body or title. The PR body is
composed once, at PR-open, by the first implement attempt; a fix-up pass only
pushes commits onto the existing branch. So an instruction routed here that asks
the agent to RECORD something in the PR body (e.g. "put the counterfactual table
in the ## Notes") cannot take effect on any pass — route such an attestation to
a surface the pass CAN write (the fix-up self-report sidecar, a run log, or a
file inside the declared scope), or make the change in a fresh run. When you
declare a routed instruction (operator_concern OR a routed concern) with
the "pr_body" target in instruction_targets, the tool result carries a
"warnings" entry naming the arrival channel and the obligation id, and the
implement review reports it as unsatisfiable. "warnings" carries, in order:
those pr_body lines; an advisory when reason is set without operator_concern
(the agent never sees reason); and the mixed-severity advisory (see Sizing
below) when the routed set mixes a high concern with medium or low ones. It is
absent when none fired.
The instruction's wording is never classified for this: an undeclared
instruction that merely mentions the PR body is an ordinary code instruction.

Bounded + operator-gated: the NORMAL bound defaults to ONE pass per stage.
The budget is the number of remaining passes (max − fix-ups already
triggered, observable on the stage_fixup_triggered audit entry's
remaining_budget field). A second attempt once the budget is spent returns
fixup_budget_exhausted — but the operator may grant ONE more pass with
force_additional_pass=true, hard-capped at the ceiling. Once that hard
ceiling is reached the tool returns the DISTINCT fixup_ceiling_reached
error (the override no longer helps — file a follow-up and merge, or start
a fresh run with the recipe under fixup_ceiling_reached below). A run-bound token may fix up only its own run's stages. The
operator still owns the merge.

Delivered-nothing refund (#967 / #1957 / #3085 / #3335): a pass that landed
NOTHING on the PR branch is REFUNDED against the normal budget AND CREDITED
against the hard ceiling — the next trigger is admitted WITHOUT
force_additional_pass, so a crash is not a reason to reach for the override,
and two harness kills that pushed nothing no longer exhaust the whole
remediation budget. Four shapes qualify: the re-dispatch produced no commit
(fixup_no_changes), the runner died on infrastructure before the agent ran,
the pass died category-C on the push/report, or the agent's HARNESS died
category-A (a 400, a zero-token hang). A category-B (policy) failure still
CONSUMES a pass, and so does any pass that PUSHED a commit before it died —
that pass delivered something. The ceiling credit is CAPPED at 3 (#3335), so
the absolute cap is 6 triggered passes (the base 3-pass ceiling plus 3
credits); both 422s report remaining_ceiling_slots so you can size the next
pass before spending it.

What a refund means depends on its shape. A pass refunded because it produced
no commit (fixup_no_changes) means the route did not work: do not repeat the
same route unchanged. Re-route one sharper concern (an operator_concern naming
the exact change), start a fresh run, or finish the change on the run branch
and fishhawk_vouch_commit it. A pass refunded because it DIED having pushed
nothing (category A or C), or a queued pass that was superseded, is different:
re-routing the same concerns is fine once the cause is fixed.

Every routed concern reaches the agent (#4284). The fix-up prompt lists the
routed concerns in one 12000-byte concern block (acceptance failures in their
own block of the same size): operator concerns FIRST and in full, then every
other concern under its concern id with its summary line; a long note body is
trimmed with an explicit per-concern marker, never dropped. A set that cannot
be listed even at that minimum is refused with fixup_concerns_overflow
(below) before anything is spent; an admitted pass whose note bodies were
trimmed records concern_bodies_truncated + truncated_concern_ids on its
stage_fixup_triggered entry.

Sizing, cost and follow-through (#5212):
  - Route one substantive concern per pass, and route a must-land fix or any
    high-severity concern alone. Delivery is guaranteed (#4284) but
    completion is not: a bundled pass can fix the easier items and drop one.
    When the routed set mixes a high concern with medium or low ones, the
    result carries a mixed-severity advisory in "warnings"; the pass still
    proceeds.
  - When each finding needs its own counterfactual cycle (delete the control,
    observe red, restore), route one per pass: several in one implement-stage
    budget can time out and deliver nothing.
  - Cost: a pass that pushes re-opens a settled acceptance stage (its verdict
    is invalidated and acceptance must re-run), and its new commit can dismiss
    an existing pull-request approval where the forge dismisses stale
    approvals. After acceptance has passed, prefer fishhawk_defer_concern for a
    low finding.
  - Before routing an acceptance failure yourself, read the
    acceptance_triage_decided entry's disposition: fixup_dispatched means
    triage already spent a pass from the shared budget, and the normal budget
    is one pass.
  - To re-route an item a prior bundled pass dropped, attach
    operator_evidence, and describe the unaddressed item in operator_concern
    in your own words. Do not paste raw diff hunks there: operator_concern is
    delivered to the agent as a binding instruction, and branch text is
    untrusted. Put the reproduction or diff evidence behind
    operator_evidence's declaration. Instruct the agent in operator_concern to
    state any disagreement in its fix-up self-report rather than skip the item.
    Do not use reason for this: the agent never sees it.
  - Follow-through: the result carries next_steps, an ORDERED list (run the
    entries in order; plural where sibling verbs return one next_step). On
    the local runner (state awaiting_host_dispatch) the first entry is
    fishhawk_dispatch_stage for the re-opened implement pass; the last is
    always fishhawk_await_audit over implement_reviewed,
    implement_review_failed and implement_review_skipped, anchored at
    fixup_trigger_sequence (this pass's stage_fixup_triggered entry). Expect
    expected_reviewers terminal entries (any of the three kinds count) and
    re-arm with since_sequence = the returned latest_sequence until all have
    landed. Both fields are best-effort: a failed audit read omits them and
    the fix-up still succeeds.
  - A re-reviewer's "confirmed" resolution is its opinion over the diff, not
    an executed reproduction: re-run your own reproduction at the fix-up head,
    and attach operator_evidence when a defect still reproduces.

Returns the re-opened Stage row (pending → dispatched) on success, plus
next_steps / fixup_trigger_sequence / expected_reviewers (above).
Returns a tool error on:
  - invalid UUID (caught before the HTTP hop)
  - validation_failed (no selection at all — none of concern_ids /
    concerns / operator_concern / both concern_ids and indices supplied /
    out-of-range index / unknown, foreign, plan-stage, or non-open
    concern_id / whitespace-only or over-length operator_concern, 400)
  - cross_run_fixup (a run-bound token reaching another run's stage, 403)
  - stage_not_found (404)
  - fixup_not_applicable (no recorded approve_with_concerns verdict, or
    the stage is not at the gate / its review gate already resolved, 422)
  - fixup_budget_exhausted (the NORMAL bounded pass count is spent, 422;
    one more pass is available via force_additional_pass=true. The details
    carry max_passes, used, refunded_passes, crashed_without_push —
    the count of refunded passes that DIED rather than merely producing no
    commit — and ceiling_refunded_passes + remaining_ceiling_slots (#3335)
    so you learn the ceiling headroom BEFORE spending the override)
  - fixup_ceiling_reached (the hard ceiling is reached, 422; a hard stop —
    the override cannot push past it. Delivered-nothing refunds credit the
    ceiling up to 3 (absolute cap 6 triggered passes); the details carry
    ceiling_refunded_passes and remaining_ceiling_slots (0 here). File a
    follow-up and merge, or start a fresh run: fold every verified finding
    into the issue body as a dated constraints block, fishhawk_cancel_run
    this run (cancel leaves an open pull request open), close its pull
    request, then start the new run. The tool error carries this recipe)
  - fixup_invalid_model (the resolved implement_model override is not in the
    deployment's per-adapter allow-list, 422; choose an allowed model or
    widen the allow-list)
  - fixup_concerns_overflow (422, #4284: the routed concerns cannot all be
    listed within the fix-up prompt's 12000-byte concern block even with
    every note cut to its summary line and operator concerns in full. Refused
    BEFORE the transition: no pass spent, no audit entry, no operator concern
    minted, no concern marked addressed_pending. The details carry bytes,
    max_bytes, overflow_bytes, concern_count, concern_ids and remedy. Remedy:
    shorten operator_concern, split the concerns across more than one fix-up
    pass, or route fewer concern_ids)
`),
	}, resolver.fixupStage)
}

// fixupStage is the tool handler. Thin wrapper over the client's
// FixupStage; per-pass bounding, concern resolution, and the subject-
// binding guard all live server-side in server/fixup.go.
func (r *runResolver) fixupStage(ctx context.Context, _ *mcp.CallToolRequest, in FixupStageInput) (*mcp.CallToolResult, FixupStageOutput, error) {
	stageID, err := uuid.Parse(in.StageID)
	if err != nil {
		return nil, FixupStageOutput{}, fmt.Errorf("stage_id %q is not a valid UUID: %w", in.StageID, err)
	}
	if len(in.ConcernIDs) > 0 && len(in.Concerns) > 0 {
		return nil, FixupStageOutput{}, fmt.Errorf("supply concern_ids (stable concern UUIDs — the primary scheme) OR the deprecated positional concerns indices, not both")
	}
	if len(in.ConcernIDs) == 0 && len(in.Concerns) == 0 && strings.TrimSpace(in.OperatorConcern) == "" {
		return nil, FixupStageOutput{}, fmt.Errorf("select at least one of: concern_ids (stable concern UUIDs from fishhawk_get_run_status's run.concerns block), the deprecated positional concerns indices, or a free-text operator_concern")
	}
	fixed, prBodyObligations, err := r.api.fixupStageWithObligations(ctx, stageID, in)
	if err != nil {
		return nil, FixupStageOutput{}, wrapFixupStageError(err)
	}
	out := FixupStageOutput{Stage: *fixed, Warnings: prBodyInstructionWarnings(prBodyObligations)}
	if w := reasonAuditOnlyWarning(in); w != "" {
		out.Warnings = append(out.Warnings, w)
	}
	ft := r.fixupFollowThroughFor(ctx, stageID, fixed)
	out.NextSteps = ft.steps
	out.FixupTriggerSequence = ft.triggerSeq
	out.ExpectedReviewers = ft.expectedReviewers
	if w := mixedSeverityWarning(ft.severities); w != "" {
		out.Warnings = append(out.Warnings, w)
	}
	return nil, out, nil
}

// fixupCeilingRecovery is the TRUSTED fresh-run recipe appended to a
// fixup_ceiling_reached refusal (E106.27 / #5212, R3). It is product text, not
// server text: the server's remediation string is unchanged.
const fixupCeilingRecovery = "recovery: the hard ceiling is a stop, so either merge with a filed follow-up, or start a fresh run — " +
	"fold every verified finding into the issue body as a dated constraints block (the cancelled run's concerns do not carry over), " +
	"fishhawk_cancel_run this run (it leaves an open pull request open), close its pull request yourself, then start the new run"

// wrapFixupStageError wraps the backend refusal. ONLY fixup_ceiling_reached
// gains the fresh-run recipe; every other code keeps today's text.
func wrapFixupStageError(err error) error {
	var ae *apiError
	if errors.As(err, &ae) && ae.Code == "fixup_ceiling_reached" {
		return fmt.Errorf("fixup stage: %w; %s", err, fixupCeilingRecovery)
	}
	return fmt.Errorf("fixup stage: %w", err)
}

// implementReviewTerminalCategories are the three terminal implement-review
// kinds the post-fix-up wait counts toward expected_reviewers, matching
// reviewRound.Landed (any terminal kind lands a reviewer).
var implementReviewTerminalCategories = []string{"implement_reviewed", "implement_review_failed", "implement_review_skipped"}

// fixupFollowThrough is the best-effort post-fix-up read: the ordered
// next_steps, the trigger anchor, the prior round's configured reviewer count
// and the routed set's severities (from the trigger payload).
type fixupFollowThrough struct {
	steps             []SuggestedAction
	triggerSeq        int64
	expectedReviewers int
	severities        []string
}

// fixupFollowThroughFor builds the follow-through for a pass the server just
// admitted (E106.27 / #5212, R8/R9). Both audit reads FAIL OPEN: an error, an
// unparseable run id, or no matching trigger entry omits the field it would
// have filled and never fails the verb — the pass is already admitted. The
// trigger anchor is the HIGHEST-sequence stage_fixup_triggered entry whose
// StageID is THIS stage (the per-entry check guards a backend that ignores the
// stage_id filter); the server appends it before the re-open, so it is on the
// chain by the time the POST returns.
func (r *runResolver) fixupFollowThroughFor(ctx context.Context, stageID uuid.UUID, stage *Stage) fixupFollowThrough {
	var ft fixupFollowThrough
	runID, perr := uuid.Parse(stage.RunID)
	if perr == nil {
		if trig := r.latestFixupTrigger(ctx, runID, stageID); trig != nil {
			ft.triggerSeq = trig.Sequence
			ft.severities = fixupTriggerSeverities(trig)
		}
		if n, _, err := r.decodeLatestStartedConfiguredAgents(ctx, runID, "implement_review_started"); err == nil && n > 0 {
			ft.expectedReviewers = n
		}
	}

	if stage.State == "awaiting_host_dispatch" {
		ft.steps = append(ft.steps, SuggestedAction{
			Action: "fishhawk_dispatch_stage",
			Params: map[string]string{
				"run_id":   stage.RunID,
				"stage_id": stageID.String(),
				"stage":    "implement",
				"workflow": "run.workflow_id (fishhawk_get_run_status)",
			},
			Precondition: "the fix-up re-opened the implement stage and it parked at awaiting_host_dispatch (local runner): nothing runs the pass until you dispatch it",
			Consumes:     consumesNone,
			Reason:       "step 1 of 2: dispatch the re-opened fix-up pass on the host, then run the review wait below",
		})
	}

	since := "the stage_fixup_triggered entry's sequence for this stage (fishhawk_list_audit, category stage_fixup_triggered)"
	if ft.triggerSeq > 0 {
		since = strconv.FormatInt(ft.triggerSeq, 10)
	}
	reviewers := "every configured reviewer"
	if ft.expectedReviewers > 0 {
		reviewers = fmt.Sprintf("%d configured reviewer(s)", ft.expectedReviewers)
	}
	ft.steps = append(ft.steps, SuggestedAction{
		Action: "fishhawk_await_audit",
		Params: map[string]string{
			"run_id":         stage.RunID,
			"categories":     strings.Join(implementReviewTerminalCategories, ","),
			"since_sequence": since,
		},
		Precondition: "the fix-up pass has been dispatched; its re-review lands after the pass pushes",
		Consumes:     consumesNone,
		Reason: fmt.Sprintf(
			"wait for the post-fix-up re-review: expect one terminal entry per configured reviewer (%s) past the fix-up trigger, and count every terminal kind (reviewed, failed or skipped) toward that number — a failed reviewer lands implement_review_failed and a skipped one implement_review_skipped instead of implement_reviewed. Re-arm with since_sequence = the returned latest_sequence until all of them have landed",
			reviewers),
	})
	return ft
}

// latestFixupTrigger returns the highest-sequence stage_fixup_triggered entry
// for stageID, or nil on a read error or when none matches (fail open).
func (r *runResolver) latestFixupTrigger(ctx context.Context, runID, stageID uuid.UUID) *AuditEntry {
	entries, _, err := r.api.ListRunAudit(ctx, runID, ListRunAuditFilter{
		Category: categoryStageFixupTriggered,
		StageID:  stageID.String(),
		Limit:    reviewAuditQueryLimit,
	})
	if err != nil {
		return nil
	}
	want := stageID.String()
	var latest *AuditEntry
	for i := range entries {
		e := &entries[i]
		if e.StageID == nil || *e.StageID != want {
			continue
		}
		if latest == nil || e.Sequence > latest.Sequence {
			latest = e
		}
	}
	return latest
}

// fixupTriggerSeverities decodes the routed set's severities from the trigger
// payload's concerns[] — the server's selected slice, including the synthetic
// operator_concern it appends at high severity. An undecodable payload yields
// nil, which suppresses the mixed-severity advisory.
func fixupTriggerSeverities(e *AuditEntry) []string {
	if e == nil || e.Payload == nil {
		return nil
	}
	raw, err := json.Marshal(e.Payload)
	if err != nil {
		return nil
	}
	var p struct {
		Concerns []struct {
			Severity string `json:"severity"`
		} `json:"concerns"`
	}
	if json.Unmarshal(raw, &p) != nil {
		return nil
	}
	out := make([]string, 0, len(p.Concerns))
	for _, c := range p.Concerns {
		out = append(out, strings.ToLower(strings.TrimSpace(c.Severity)))
	}
	return out
}

// reasonAuditOnlyWarning is the advisory for a pass that set `reason` and no
// operator_concern (#5561): `reason` is an audit note the fix-up agent never
// sees, so an instruction written there is silently undelivered. Returns "" when
// reason is blank or operator_concern is set. Advisory only; it is computed from
// the input, so the pass has already proceeded.
func reasonAuditOnlyWarning(in FixupStageInput) string {
	if strings.TrimSpace(in.Reason) == "" || strings.TrimSpace(in.OperatorConcern) != "" {
		return ""
	}
	return "`reason` is an audit note only: it is recorded on the stage_fixup_triggered entry and as the routed concerns' state_reason, " +
		"and the fix-up agent never sees it. If it holds an instruction, re-route it as operator_concern."
}

// mixedSeverityWarning is the routing-time advisory (E106.27 / #5212, R13):
// when the routed set holds at least one high AND at least one medium or low
// concern, a bundled pass can address the lower-severity items and skip the
// high one. It never blocks the pass. Returns "" for an all-high, all-lower or
// single-concern set, and for an unread set (nil).
func mixedSeverityWarning(severities []string) string {
	high, lower := 0, 0
	for _, s := range severities {
		switch s {
		case "high":
			high++
		case "medium", "low":
			lower++
		}
	}
	if high == 0 || lower == 0 {
		return ""
	}
	return fmt.Sprintf(
		"mixed-severity routing: this pass routed %d high and %d medium/low concern(s) together. A bundled pass can address the lower-severity items and skip the high one; when the high concern's fix must land, route it alone in its own pass. The pass still proceeds.",
		high, lower)
}

// prBodyInstructionWarnings renders one advisory warning per routed instruction
// declared with the pr_body target (#2782, E83.37 / #4022), FROM the
// server-minted obligation set — never
// re-deriving anything locally, so the arrival channel and id each line reports
// are exactly what the backend recorded and what the reviewer will later see.
// It deliberately does NOT render the obligation's TextExcerpt: the excerpt can
// be attacker-influenceable (an acceptance-derived note), and the reviewer-
// facing gate evidence is the one site that prints an excerpt, under quarantine
// — keeping this operator-facing warning free of any injection surface. Returns
// nil for the common pass that targeted no instruction at the PR body, so an
// ordinary pass's warnings stay nil (the mixed-severity advisory is appended
// separately, after these lines).
func prBodyInstructionWarnings(obligations []PRBodyObligation) []string {
	if len(obligations) == 0 {
		return nil
	}
	warnings := make([]string, 0, len(obligations))
	for _, ob := range obligations {
		warnings = append(warnings, fmt.Sprintf(
			"routed instruction %s (arrived via %s) is targeted at the PULL-REQUEST BODY (instruction_targets: pr_body), which a fix-up pass CANNOT update: "+
				"the PR title and body are composed once at PR-open by the first implement attempt, and a fix-up pass "+
				"only pushes commits — it has no mechanism to re-compose or PATCH them, so this instruction cannot take "+
				"effect. Record the attestation on a surface this pass CAN write — the fix-up self-report sidecar (its "+
				"ids and statuses reach the implement reviewer and the operator at the gate), a run log, or a file "+
				"inside the declared scope — or make the change in a fresh run whose first implement attempt composes "+
				"the body.",
			ob.ID, ob.Source))
	}
	return warnings
}
