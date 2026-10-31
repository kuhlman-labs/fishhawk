package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// LabelConcernInput is the fishhawk_label_concern tool's input schema (E94.8 /
// #4841, ADR-117 D1 phase 3). Mirrors `POST /v0/concerns/{concern_id}/label`:
// EITHER a judgement (with tradeoff_cost only for a tradeoff) OR duplicate_of,
// the id of an EARLIER concern of the same run. Judgement, TradeoffCost and
// DuplicateOf are omitempty and the tool pre-checks none of the judgement or
// exclusivity rules: the backend is the single refusal authority.
type LabelConcernInput struct {
	ConcernID    string `json:"concern_id" jsonschema:"the stable concern UUID to label (from fishhawk_get_gate_view or fishhawk_get_run_status's run.concerns.items[].id); any state"`
	Judgement    string `json:"judgement,omitempty" jsonschema:"the human judgement to record: one of defect, defect_pre_existing, no_defect, tradeoff, preference, stale, insufficient_evidence; unlabelled is refused. Exclusive with duplicate_of. Omit it with backfill:true on a WAIVED concern to let the stored waive reason's false_positive:, accepted_tradeoff: or preference: prefix supply it"`
	TradeoffCost string `json:"tradeoff_cost,omitempty" jsonschema:"the stated cost of a tradeoff judgement (at most 4000 bytes; refused, not truncated): REQUIRED when the judgement is tradeoff and refused with any other judgement and with duplicate_of (400 validation_failed)"`
	DuplicateOf  string `json:"duplicate_of,omitempty" jsonschema:"the UUID of an EARLIER concern of the SAME run this one duplicates; it then inherits that chain root's judgement. Exclusive with judgement and tradeoff_cost"`
	Backfill     bool   `json:"backfill,omitempty" jsonschema:"true to import a historical label: records judgement_backfill, and on a waived concern lets the stored waive reason's prefix supply an omitted judgement"`
}

// LabelConcernOutput surfaces the labelled concern as every read surface
// renders it.
type LabelConcernOutput struct {
	Concern LabelledConcern `json:"concern"`
}

// LabelledConcern is the decoded 200 body of the label verb. It mirrors the
// backend's labelConcernResponse (`backend/internal/server/label_concern.go`),
// which embeds concernJudgementView; every json tag MUST byte-match or the
// field decodes to its zero value silently.
type LabelledConcern struct {
	ID                     string `json:"id"`
	RunID                  string `json:"run_id"`
	StageID                string `json:"stage_id"`
	StageKind              string `json:"stage_kind"`
	State                  string `json:"state" jsonschema:"the concern's lifecycle state; a label never changes it"`
	Outcome                string `json:"outcome,omitempty" jsonschema:"the recorded outcome, or duplicate_of:<concern id> for a duplicate; absent when none is recorded"`
	Judgement              string `json:"judgement" jsonschema:"the effective judgement: the label just recorded, or for a duplicate its chain root's judgement (unlabelled when the root is unjudged)"`
	JudgementInheritedFrom string `json:"judgement_inherited_from,omitempty" jsonschema:"for a duplicate, the chain-root concern whose judgement it reports; absent otherwise"`
	DuplicateOf            string `json:"duplicate_of,omitempty" jsonschema:"the earlier same-run concern this one duplicates; absent for a judgement label (which clears a prior link)"`
	JudgementActorKind     string `json:"judgement_actor_kind,omitempty"`
	JudgementDelegated     bool   `json:"judgement_delegated,omitempty"`
	JudgementBulk          bool   `json:"judgement_bulk,omitempty"`
	JudgementBackfill      bool   `json:"judgement_backfill,omitempty" jsonschema:"true when the label was imported with backfill:true"`
	TradeoffCost           string `json:"tradeoff_cost,omitempty"`
}

// labelConcernRequestBody mirrors the backend's labelConcernRequest. Judgement,
// TradeoffCost and DuplicateOf are omitempty so a blank field never reaches
// the backend as a sent-but-empty value; Backfill is always sent.
type labelConcernRequestBody struct {
	Judgement    string `json:"judgement,omitempty"`
	TradeoffCost string `json:"tradeoff_cost,omitempty"`
	DuplicateOf  string `json:"duplicate_of,omitempty"`
	Backfill     bool   `json:"backfill"`
}

// LabelConcern relabels one concern via `POST /v0/concerns/{concern_id}/label`
// (E94.8 / #4841, ADR-117 D1 phase 3). It lives here rather than in client.go,
// like resolve_concerns.go's method. 4xx/5xx surfaces as *apiError:
//   - 400 validation_failed (details.rule: duplicate_of_excludes_judgement,
//     duplicate_of_not_found — a missing target and another run's target,
//     indistinguishably — duplicate_of_not_earlier, judgement_required, an
//     unknown or unlabelled judgement, a tradeoff/tradeoff_cost mismatch,
//     tradeoff_cost_too_long, judgement_conflicts_reason_prefix)
//   - 401 authentication_required
//   - 403 label_requires_human (any agent token), insufficient_scope, or
//     account_forbidden
//   - 404 concern_not_found
//   - 409 concern_label_conflict (a concurrent relabel landed first)
//   - 500 audit_append_failed (nothing was written)
//   - 503 concern_store_unconfigured
func (c *apiClient) LabelConcern(ctx context.Context, id uuid.UUID, judgement, tradeoffCost, duplicateOf string, backfill bool) (*LabelledConcern, error) {
	body, err := json.Marshal(labelConcernRequestBody{
		Judgement: judgement, TradeoffCost: tradeoffCost, DuplicateOf: duplicateOf, Backfill: backfill,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal label concern: %w", err)
	}
	var out LabelledConcern
	if err := c.do(ctx, http.MethodPost, "/v0/concerns/"+id.String()+"/label", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// registerLabelConcern wires the fishhawk_label_concern tool (E94.8 / #4841).
//
// Auth: write tool, the waive scope pair (write:stages or write:fixups); the
// backend refuses EVERY agent token, so it cannot be delegated.
func registerLabelConcern(srv *mcp.Server, resolver *runResolver) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "fishhawk_label_concern",
		Description: strings.TrimSpace(`
Set or correct the human judgement on one concern, or mark it a duplicate
of an earlier concern of the same run. Human-only.

WHEN:
  - a recorded judgement is wrong and must be corrected (a relabel is a
    new versioned judgement: the audit row carries the prior one);
  - two concerns of a run report the same thing: mark the LATER one
    duplicate_of the earlier, and it inherits the earlier chain root's
    judgement on every read surface (it keeps inheriting when the root is
    relabelled);
  - importing historical labels: backfill:true records
    judgement_backfill, and on a WAIVED concern an omitted judgement is
    taken from the stored waive reason's false_positive:,
    accepted_tradeoff: or preference: prefix.

ELIGIBILITY: any concern, in ANY state. The label never changes the
concern's state, state_reason, outcome or lifecycle time. A HUMAN
operator only: the backend refuses every agent token (operator-agent/,
a run-bound mcp:run: token, or a first-officer seat) with
label_requires_human, and there is no delegated path.

Send EXACTLY ONE of:
  - judgement     : one of defect, defect_pre_existing, no_defect,
    tradeoff, preference, stale, insufficient_evidence (unlabelled is
    refused); tradeoff_cost (at most 4000 bytes, refused rather than
    truncated) is required with a tradeoff and refused otherwise. A
    judgement label clears any prior duplicate_of link.
  - duplicate_of  : the UUID of a concern of the SAME run that sorts
    strictly EARLIER in the run ledger (origin review sequence, then
    creation time, then id). Self-links and later targets are refused,
    so a chain can never form a cycle.

Each call records a concern_labelled audit entry FIRST (durable before
the write), carrying the prior judgement and prior duplicate_of. A write
that then fails records a corrective concern_label_failed entry.

Returns the labelled concern: id, run_id, stage_id, stage_kind, state,
and its judgement as the read surfaces render it (a duplicate shows
judgement_inherited_from and outcome duplicate_of:<id>).

Returns a tool error on:
  - invalid concern_id or duplicate_of UUID (caught before the HTTP hop)
  - validation_failed (400; details.rule names the rule, checked by the
    backend only): duplicate_of_excludes_judgement,
    duplicate_of_not_found (no concern of THIS run has that id — a
    missing target and another run's target read identically),
    duplicate_of_not_earlier, judgement_required, an unknown or
    unlabelled judgement, a tradeoff/tradeoff_cost mismatch,
    tradeoff_cost_too_long, judgement_conflicts_reason_prefix
  - label_requires_human (403: an agent token)
  - concern_not_found (404)
  - concern_label_conflict (409: a concurrent relabel landed first;
    re-read the concern and label it again)
  - audit_append_failed (500: nothing was written)
  - concern_store_unconfigured (503)
`),
	}, resolver.labelConcern)
}

// labelConcern is the tool handler. It validates the two UUIDs locally and
// passes the backend result through; the human-only guard, the exclusivity
// and duplicate_of guards, the judgement rules and the audit-before-write
// ordering all live server-side in server/label_concern.go.
func (r *runResolver) labelConcern(ctx context.Context, _ *mcp.CallToolRequest, in LabelConcernInput) (*mcp.CallToolResult, LabelConcernOutput, error) {
	concernID, err := uuid.Parse(in.ConcernID)
	if err != nil {
		return nil, LabelConcernOutput{}, fmt.Errorf("concern_id %q is not a valid UUID: %w", in.ConcernID, err)
	}
	if in.DuplicateOf != "" {
		if _, perr := uuid.Parse(in.DuplicateOf); perr != nil {
			return nil, LabelConcernOutput{}, fmt.Errorf("duplicate_of %q is not a valid UUID: %w", in.DuplicateOf, perr)
		}
	}
	labelled, err := r.api.LabelConcern(ctx, concernID, in.Judgement, in.TradeoffCost, in.DuplicateOf, in.Backfill)
	if err != nil {
		return nil, LabelConcernOutput{}, fmt.Errorf("label concern: %w", err)
	}
	return nil, LabelConcernOutput{Concern: *labelled}, nil
}
