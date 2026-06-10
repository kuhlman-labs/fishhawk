package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ScopeAmendmentPath is one requested path + operation inside a scope
// amendment (E22.X / #961). Operation is "modify" or "create".
type ScopeAmendmentPath struct {
	Path      string `json:"path"`
	Operation string `json:"operation"`
}

// ScopeAmendmentItem mirrors the backend's scope-amendment wire shape
// (GET /v0/runs/{run_id}/scope-amendments items and the decision
// response). Status is pending|approved|denied.
type ScopeAmendmentItem struct {
	ID             string               `json:"id"`
	RunID          string               `json:"run_id"`
	StageID        string               `json:"stage_id"`
	Paths          []ScopeAmendmentPath `json:"paths"`
	Reason         string               `json:"reason"`
	Status         string               `json:"status"`
	DecisionReason string               `json:"decision_reason,omitempty"`
	DecidedBy      string               `json:"decided_by,omitempty"`
	RequestedAt    string               `json:"requested_at,omitempty"`
	DecidedAt      string               `json:"decided_at,omitempty"`
}

// ListScopeAmendments wraps GET /v0/runs/{run_id}/scope-amendments
// with the operator bearer.
func (c *apiClient) ListScopeAmendments(ctx context.Context, runID uuid.UUID) ([]ScopeAmendmentItem, error) {
	var out struct {
		Items []ScopeAmendmentItem `json:"items"`
	}
	if err := c.do(ctx, http.MethodGet, "/v0/runs/"+runID.String()+"/scope-amendments", nil, &out); err != nil {
		return nil, err
	}
	return out.Items, nil
}

// scopeAmendmentDecisionRequest mirrors the backend's decision body
// (backend/internal/server/scope_amendment.go).
type scopeAmendmentDecisionRequest struct {
	Decision string `json:"decision"`
	Reason   string `json:"reason,omitempty"`
}

// DecideScopeAmendment wraps POST
// /v0/runs/{run_id}/scope-amendments/{amendment_id}/decision with the
// operator bearer.
func (c *apiClient) DecideScopeAmendment(ctx context.Context, runID, amendmentID uuid.UUID, decision, reason string) (*ScopeAmendmentItem, error) {
	body, err := json.Marshal(scopeAmendmentDecisionRequest{Decision: decision, Reason: reason})
	if err != nil {
		return nil, fmt.Errorf("marshal decision: %w", err)
	}
	var out ScopeAmendmentItem
	if err := c.do(ctx, http.MethodPost,
		"/v0/runs/"+runID.String()+"/scope-amendments/"+amendmentID.String()+"/decision",
		body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListScopeAmendmentsInput is the fishhawk_list_scope_amendments
// tool's input schema.
type ListScopeAmendmentsInput struct {
	RunID string `json:"run_id" jsonschema:"the Fishhawk run UUID whose scope amendments to list"`
}

// ListScopeAmendmentsOutput carries every amendment for the run,
// oldest first.
type ListScopeAmendmentsOutput struct {
	Items []ScopeAmendmentItem `json:"items"`
}

// registerListScopeAmendments wires the fishhawk_list_scope_amendments
// tool (E22.X / #961). Read tool: the operator bearer (or any token
// the backend's GET admits) lists the run's amendment requests so the
// operator can see what the implement agent asked for before deciding.
func registerListScopeAmendments(srv *mcp.Server, resolver *runResolver) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "fishhawk_list_scope_amendments",
		Description: strings.TrimSpace(`
List a run's mid-stage scope amendment requests (#961).

While an implement stage runs, the agent may request that specific file
paths (modify or create, plus a reason) be folded into the effective
scope.files. Each request lands as a scope_amendment_requested audit
entry — await it with fishhawk_await_audit anchored on that category —
and parks as a pending row the agent polls while it keeps working on
in-scope files.

Use this tool to inspect the pending request(s) — the paths, the
operation per path, and the agent's reason — before deciding with
fishhawk_decide_scope_amendment. Items return oldest first; status is
pending | approved | denied.
`),
	}, resolver.listScopeAmendments)
}

func (r *runResolver) listScopeAmendments(ctx context.Context, _ *mcp.CallToolRequest, in ListScopeAmendmentsInput) (*mcp.CallToolResult, ListScopeAmendmentsOutput, error) {
	runID, err := uuid.Parse(in.RunID)
	if err != nil {
		return nil, ListScopeAmendmentsOutput{}, fmt.Errorf("run_id %q is not a valid UUID: %w", in.RunID, err)
	}
	items, err := r.api.ListScopeAmendments(ctx, runID)
	if err != nil {
		return nil, ListScopeAmendmentsOutput{}, fmt.Errorf("list scope amendments: %w", err)
	}
	return nil, ListScopeAmendmentsOutput{Items: items}, nil
}

// DecideScopeAmendmentInput is the fishhawk_decide_scope_amendment
// tool's input schema.
type DecideScopeAmendmentInput struct {
	RunID       string `json:"run_id" jsonschema:"the Fishhawk run UUID the amendment belongs to"`
	AmendmentID string `json:"amendment_id" jsonschema:"the scope amendment UUID (from fishhawk_list_scope_amendments or the scope_amendment_requested audit entry's amendment_id)"`
	Decision    string `json:"decision" jsonschema:"approve or deny"`
	Reason      string `json:"reason,omitempty" jsonschema:"operator rationale; delivered to the agent verbatim on deny (decision_reason), recorded on the scope_amendment_decided audit entry either way"`
}

// DecideScopeAmendmentOutput surfaces the decided amendment row.
type DecideScopeAmendmentOutput struct {
	Amendment ScopeAmendmentItem `json:"amendment"`
}

// registerDecideScopeAmendment wires the fishhawk_decide_scope_amendment
// tool (E22.X / #961).
//
// Auth: operator-only write tool — the backend requires write:stages
// and rejects any run-bound agent token outright (403 self_decision),
// so the requesting agent can never decide its own request.
func registerDecideScopeAmendment(srv *mcp.Server, resolver *runResolver) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "fishhawk_decide_scope_amendment",
		Description: strings.TrimSpace(`
Approve or deny an implement agent's mid-stage scope amendment request
(#961). Operator-only: the backend rejects run-bound agent tokens
(self_decision), so the agent that filed the request can never decide it.

On APPROVE the requested paths fold into the stage's effective
scope.files: the agent's poll loop sees the approval and edits/creates
them; the runner's pre-commit refresh folds the same paths before the
verified-tree gates AND the push, so approved creates pass the
created-out-of-scope gate while anything NOT requested still fails loud
(#818/#825). On DENY the agent reads your reason and must adapt within
the original scope or fail loud.

The agent polls for ~5 minutes per request — decide promptly or the
agent proceeds as if denied. Each implement stage gets at most 2
requests (denied ones count against the budget).

Returns the decided amendment row. Tool errors:
  - invalid UUIDs (caught before the HTTP hop)
  - validation_failed (decision not approve/deny, 400)
  - amendment_not_found (wrong id or wrong run, 404)
  - amendment_already_decided (idempotency guard, 409)
  - self_decision (a run-bound agent token attempted the decision, 403)
  - insufficient_scope (token lacks write:stages, 403)
`),
	}, resolver.decideScopeAmendment)
}

func (r *runResolver) decideScopeAmendment(ctx context.Context, _ *mcp.CallToolRequest, in DecideScopeAmendmentInput) (*mcp.CallToolResult, DecideScopeAmendmentOutput, error) {
	runID, err := uuid.Parse(in.RunID)
	if err != nil {
		return nil, DecideScopeAmendmentOutput{}, fmt.Errorf("run_id %q is not a valid UUID: %w", in.RunID, err)
	}
	amendmentID, err := uuid.Parse(in.AmendmentID)
	if err != nil {
		return nil, DecideScopeAmendmentOutput{}, fmt.Errorf("amendment_id %q is not a valid UUID: %w", in.AmendmentID, err)
	}
	if in.Decision != "approve" && in.Decision != "deny" {
		return nil, DecideScopeAmendmentOutput{}, fmt.Errorf("decision must be \"approve\" or \"deny\", got %q", in.Decision)
	}
	decided, err := r.api.DecideScopeAmendment(ctx, runID, amendmentID, in.Decision, in.Reason)
	if err != nil {
		return nil, DecideScopeAmendmentOutput{}, fmt.Errorf("decide scope amendment: %w", err)
	}
	return nil, DecideScopeAmendmentOutput{Amendment: *decided}, nil
}
