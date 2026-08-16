package mcpserver

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// fishhawk_decide_scope_completeness (E22.X / #1231) is the operator
// decision verb for the zero-re-run scope-completeness PARK.
//
// When an implement stage's ONLY committed-tree gate failure is a shortfall
// of EITHER class — the #1151 scope-completeness "missing declared scope
// file(s)" check or an #1171 unsatisfied binding assertion (#2501) — and the
// tree otherwise passed verify, the runner does NOT fail category-B. Instead
// it pushes the verified commit to the run branch (no PR) and parks the stage
// in awaiting_scope_decision, carrying the held commit SHA, run branch,
// verified tree SHA, and the shortfall itself (missing_paths /
// unsatisfied_assertions). This verb resolves the park in-band:
//
//   - decision=exempt: the stage returns to dispatch and the re-dispatched
//     runner opens the PR from the EXACT held commit with NO agent
//     re-invocation (zero re-run), accepting the already-committed tree.
//     Supersedes #1229's one-re-run exempt lever for both shortfall classes.
//   - decision=amend (#2591): the operator WIDENS the parked stage's effective
//     scope with the named paths and the stage RESUMES, so the agent re-runs
//     against the wider scope instead of the pass failing category-B. This is
//     the only non-fail resolution for a BUILD-REQUIRED park (#2548), whose
//     held tree is red by construction and whose `exempt` is refused outright.
//   - decision=fail: the stage falls through to today's category-B
//     fail-and-restore path.
//
// Operator-only: the backend rejects run-bound agent tokens, so the agent
// whose stage parked can never decide its own park (mirrors
// fishhawk_decide_scope_amendment's self_decision rejection).

// DecideScopeCompletenessInput is the fishhawk_decide_scope_completeness
// tool's input schema.
type DecideScopeCompletenessInput struct {
	RunID    string `json:"run_id" jsonschema:"the Fishhawk run UUID whose implement stage is parked in awaiting_scope_decision"`
	Decision string `json:"decision" jsonschema:"exempt (accept the held commit and open the PR with no agent re-run), amend (widen the parked stage's scope with paths and resume it so the agent re-runs), or fail (fall through to category-B)"`
	Reason   string `json:"reason" jsonschema:"operator rationale; required and non-empty. Recorded on the scope_completeness_exempted / scope_completeness_amended / scope_completeness_failed audit entry"`
	// Paths is required on decision=amend and rejected on any other decision.
	Paths []ScopeAmendmentPathEntry `json:"paths,omitempty" jsonschema:"amend ONLY: the repo-relative files to fold into the parked stage's effective scope, each {path, operation} with operation modify or create. Must cover every build_required_path the park recorded"`
	// AcknowledgeOwnerUnresolved re-admits an amend the owner-slice guard could
	// not prove safe. It never relaxes the resolved-and-started refusal.
	AcknowledgeOwnerUnresolved bool `json:"acknowledge_owner_unresolved,omitempty" jsonschema:"amend ONLY: set true to proceed when the backend cannot resolve which slice OWNS an amended path (409 amend_owner_attribution_unresolved). The acknowledgement is recorded on the audit entry — it is an audited risk, not a default"`
}

// DecideScopeCompletenessOutput surfaces the decided park record.
type DecideScopeCompletenessOutput struct {
	Result ScopeCompletenessDecisionResult `json:"result"`
}

// registerDecideScopeCompleteness wires the fishhawk_decide_scope_completeness
// tool (E22.X / #1231).
//
// Auth: operator-only write tool — the backend requires write:stages and
// rejects any run-bound agent token outright (run_token_forbidden), so the
// agent whose stage parked can never decide its own park.
func registerDecideScopeCompleteness(srv *mcp.Server, resolver *runResolver) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "fishhawk_decide_scope_completeness",
		Description: strings.TrimSpace(`
Resolve an implement stage parked in awaiting_scope_decision (#1231):
exempt the already-committed tree, amend its scope and resume, or fail it
to category-B.

The runner parks the stage HERE — instead of failing category-B — only
when the SOLE committed-tree gate shortfall is the #1151 scope-completeness
"missing declared scope file(s)" check OR an #1171 unsatisfied binding
assertion, and verify otherwise passed. It has already pushed the
gate-verified commit to the run branch (no PR opened). A COMPOUND failure
(a shortfall plus any other gate) never parks — it keeps category-B.

On decision=exempt the stage returns to dispatch (pending; on a local run it
then parks at awaiting_host_dispatch for your fishhawk_dispatch_stage), and
the re-dispatched runner opens the PR from the EXACT held commit with NO
agent re-invocation (zero re-run): the held tree is accepted as-is and the
implement-review gate proceeds. On decision=fail the stage falls through to
today's category-B fail-and-restore.

On decision=amend (#2591) the named paths are folded into THIS stage's
effective scope as a pre-approved #961 amendment row and the stage resumes
to pending, so the agent RE-RUNS against the wider scope. The held commit is
kept on the run branch, untouched. amend is the ONLY non-fail resolution for
a BUILD-REQUIRED park (#2548) — that class's held tree is red by
construction because a sibling slice owns a file the build needs, so its
exempt is refused 409 exempt_refused_build_required. The amend must cover
every build_required_path the park recorded.

amend narrowly relaxes the single-owner-file invariant, so it is guarded:
a path whose owning sibling slice has ALREADY STARTED its implement pass is
refused (two branches would carry divergent edits to one file), and a path
whose ownership cannot be RESOLVED is refused too unless you re-admit it with
acknowledge_owner_unresolved:true — which is recorded on the audit entry as a
deliberate operator risk. Note the cost: an operator amend consumes one of
the re-run agent's two mid-stage #961 amendment slots
(agent_amendment_budget_remaining on the response reports what is left).

Operator-only: the backend rejects run-bound agent tokens
(run_token_forbidden), so the agent whose stage parked can never decide its
own park. Read the parked record first (fishhawk_get_run_status surfaces
the awaiting_scope_decision next action, or fishhawk_list_audit on category
scope_completeness_parked carries the shortfall — missing_paths and/or
unsatisfied_assertions — plus the held SHA).

Returns the decided park record. Tool errors:
  - invalid run_id UUID (caught before the HTTP hop)
  - decision not exempt/amend/fail (caught before the HTTP hop)
  - empty reason (caught before the HTTP hop)
  - amend with no paths, or paths on a non-amend decision (caught before the
    HTTP hop)
  - validation_failed (400)
  - run_token_forbidden (a run-bound agent token attempted the decision, 403)
  - insufficient_scope (token lacks write:stages, 403)
  - run_not_found (404)
  - scope_completeness_not_parked (the stage is not parked in
    awaiting_scope_decision, 409)
  - exempt_refused_build_required (exempt on a build-required park; use
    amend or fail, 409)
  - amend_incomplete_coverage (the amend omits a build_required_path, 409)
  - amend_refused_owner_slice_active (a path's owning sibling slice has
    already started its implement pass, 409)
  - amend_owner_attribution_unresolved (ownership unprovable and not
    acknowledged — re-submit with acknowledge_owner_unresolved:true, 409)
  - amend_budget_exhausted (this stage's amend-and-resume budget is spent, 409)
  - amend_unrecorded / amend_resume_failed (the widening or the resume could
    not be persisted; the stage is left parked — re-POST, the amendment row is
    reused rather than duplicated, 500)
`),
	}, resolver.decideScopeCompleteness)
}

func (r *runResolver) decideScopeCompleteness(ctx context.Context, _ *mcp.CallToolRequest, in DecideScopeCompletenessInput) (*mcp.CallToolResult, DecideScopeCompletenessOutput, error) {
	runID, err := uuid.Parse(in.RunID)
	if err != nil {
		return nil, DecideScopeCompletenessOutput{}, fmt.Errorf("run_id %q is not a valid UUID: %w", in.RunID, err)
	}
	if in.Decision != "exempt" && in.Decision != "amend" && in.Decision != "fail" {
		return nil, DecideScopeCompletenessOutput{}, fmt.Errorf("decision must be \"exempt\", \"amend\", or \"fail\", got %q", in.Decision)
	}
	if strings.TrimSpace(in.Reason) == "" {
		return nil, DecideScopeCompletenessOutput{}, fmt.Errorf("reason is required and must be non-empty")
	}
	// Both path rejections are PRE-HTTP: an amend with nothing to fold is a
	// no-op the backend would 400 anyway, and paths on a non-amend decision
	// would silently go nowhere — catching them here keeps the operator's
	// mistake legible instead of surfacing it as a remote validation_failed.
	if in.Decision == "amend" && len(in.Paths) == 0 {
		return nil, DecideScopeCompletenessOutput{}, fmt.Errorf("decision \"amend\" requires paths: name the repo-relative files to fold into the parked stage's effective scope")
	}
	if in.Decision != "amend" && len(in.Paths) > 0 {
		return nil, DecideScopeCompletenessOutput{}, fmt.Errorf("paths may only be supplied with decision \"amend\", got decision %q", in.Decision)
	}
	result, err := r.api.DecideScopeCompleteness(ctx, runID, in.Decision, in.Reason, in.Paths, in.AcknowledgeOwnerUnresolved)
	if err != nil {
		return nil, DecideScopeCompletenessOutput{}, fmt.Errorf("decide scope completeness: %w", err)
	}
	return nil, DecideScopeCompletenessOutput{Result: *result}, nil
}
