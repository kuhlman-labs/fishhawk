package mcpserver

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// GroomingDispositionEntry is one requested per-entry verdict. It is the wire
// shape the backend's capture request body takes, so the tool forwards it
// unchanged.
type GroomingDispositionEntry struct {
	EntryID     string `json:"entry_id" jsonschema:"the grooming-report entry's stable DERIVED id (e.g. 'duplicate:github/acme/app#2+github/acme/app#3'); it must be an id the run's NEWEST grooming_report declares"`
	Verdict     string `json:"verdict" jsonschema:"one of approved, rejected, amended — the closed grooming verdict set"`
	CloseTarget string `json:"close_target,omitempty" jsonschema:"optional: for a duplicate the operator is approving, which item the pair collapses onto; recorded verbatim and interpreted by nothing in this slice"`
}

// RecordGroomingDispositionsInput is the fishhawk_record_grooming_dispositions
// tool's input schema (E54.30 / #2843).
type RecordGroomingDispositionsInput struct {
	RunID        string                     `json:"run_id" jsonschema:"the Fishhawk run UUID whose newest grooming_report the dispositions attach to"`
	Dispositions []GroomingDispositionEntry `json:"dispositions" jsonschema:"one entry per verdict; the batch is validated ATOMICALLY server-side — one unknown entry_id records NOTHING"`
}

// RecordedGroomingDisposition is one projected disposition — the read-back row
// the capture returns.
type RecordedGroomingDisposition struct {
	EntryID       string `json:"entry_id"`
	EntryClass    string `json:"entry_class"`
	Verdict       string `json:"verdict"`
	CloseTarget   string `json:"close_target,omitempty"`
	RecordedAt    string `json:"recorded_at"`
	RecordedBy    string `json:"recorded_by"`
	AuditSequence int64  `json:"audit_sequence"`
}

// RecordGroomingDispositionsOutput is the capture's 200 body: the report
// artifact the dispositions attached to plus the FULL current disposition set,
// so the verb gets its read-back in the same call.
type RecordGroomingDispositionsOutput struct {
	RunID        string                        `json:"run_id"`
	ArtifactID   string                        `json:"artifact_id"`
	StageID      string                        `json:"stage_id"`
	ContentHash  string                        `json:"content_hash"`
	Dispositions []RecordedGroomingDisposition `json:"dispositions"`
}

// ListGroomingDispositionsOutput is the read-back body. Same shape as the
// capture's — both verbs share one server-side projection, so the POST echo and
// the GET read-back are the same bytes by construction.
type ListGroomingDispositionsOutput = RecordGroomingDispositionsOutput

// registerRecordGroomingDispositions wires fishhawk_record_grooming_dispositions
// (E54.30 / #2843).
//
// Auth: operator-only. The backend refuses BOTH a run-bound agent token
// ("mcp:run:<uuid>" subject, 403 run_token_forbidden) and a delegated
// operator-agent token ("operator-agent/" subject prefix, 403
// operator_agent_forbidden) — the report is agent-authored, so an agent that
// could also disposition it would convert an operator gate into a self-approval.
func registerRecordGroomingDispositions(srv *mcp.Server, resolver *runResolver) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "fishhawk_record_grooming_dispositions",
		Description: strings.TrimSpace(`
Record the operator's per-entry verdicts on a grooming report, as an auditable
fact (E54.30 / #2843).

Use this after reading a backlog_grooming run's grooming_report and deciding,
entry by entry, what should happen: approved, rejected, or amended — plus an
optional close_target naming which item a duplicate pair collapses onto. Each
disposition persists as one chained grooming_disposition_recorded audit row
keyed by the entry's stable DERIVED id.

NOTHING CONSUMES THESE DISPOSITIONS YET. This verb is CAPTURE only: recording an
approval does NOT apply anything, does not close a duplicate, and does not
re-rank a backlog. The consumption half — the apply stage and its concurrency
protocol — is #2991. Until that lands, a recorded disposition is inert,
forward-compatible audit history. Do not use this expecting a tracker mutation.

The audit category is DELIBERATELY distinct from grooming_mutation_applied:
this row is what the OPERATOR DECIDED; that one is what was APPLIED.

Operator-only. The backend refuses a run-bound agent token (even for its own
run) AND a delegated operator-agent token: the grooming report is agent-authored,
so an agent dispositioning it would be a self-approval.

Semantics worth knowing before you call:
  - the dispositions attach to the run's NEWEST grooming_report artifact,
    resolved server-side; the resolved artifact_id comes back in the response;
  - the batch is ATOMIC — one unknown entry_id records NOTHING, so a partial
    capture is unreachable;
  - an entry_id may not repeat WITHIN one request (ambiguous intent), but a
    LATER request on the same entry SUPERSEDES the earlier one (last wins) and
    both rows stay in the chain, so the correction is itself auditable.

Inputs:
  - run_id       : the run whose newest grooming_report is being dispositioned.
  - dispositions : one {entry_id, verdict, close_target?} per decided entry.

Returns the resolved artifact plus the FULL current disposition set for it (the
read-back rides along, so no separate read is needed). Tool errors:
  - invalid UUID / empty dispositions / empty entry_id (caught before the HTTP hop)
  - validation_failed (empty batch, empty entry_id, an entry_id repeated in one batch, 400)
  - grooming_verdict_invalid (a verdict outside approved/rejected/amended, 400)
  - run_token_forbidden (a run-bound agent token attempted the capture, 403)
  - operator_agent_forbidden (a delegated operator-agent token attempted it, 403)
  - insufficient_scope (token lacks write:approvals, 403)
  - grooming_report_absent (the run shipped no grooming_report, 409)
  - grooming_entry_unknown (an id the newest report does not declare, 422)
  - grooming_dispositions_unconfigured (repositories not wired, 503)
`),
	}, resolver.recordGroomingDispositions)
}

// recordGroomingDispositions is the tool handler. It validates the run UUID and
// the batch shape locally — a fast fail before the HTTP hop — and delegates
// every AUTHORIZATION and DOMAIN decision (the operator-only refusals, the
// verdict set, the entry-id set, batch atomicity) to the backend, which is the
// single authority for all of them. Client-side pre-validation deliberately
// does NOT duplicate the verdict or entry-id checks: a second copy of a closed
// set is a drift source, and the backend's refusal is the one that matters.
func (r *runResolver) recordGroomingDispositions(ctx context.Context, _ *mcp.CallToolRequest,
	in RecordGroomingDispositionsInput) (*mcp.CallToolResult, RecordGroomingDispositionsOutput, error) {
	runID, err := uuid.Parse(in.RunID)
	if err != nil {
		return nil, RecordGroomingDispositionsOutput{},
			fmt.Errorf("run_id %q is not a valid UUID: %w", in.RunID, err)
	}
	if len(in.Dispositions) == 0 {
		return nil, RecordGroomingDispositionsOutput{},
			fmt.Errorf("dispositions is required: name at least one grooming-report entry and the verdict to record for it")
	}
	out := make([]GroomingDispositionEntry, 0, len(in.Dispositions))
	for i, d := range in.Dispositions {
		entryID := strings.TrimSpace(d.EntryID)
		if entryID == "" {
			return nil, RecordGroomingDispositionsOutput{},
				fmt.Errorf("dispositions[%d].entry_id is required: name the grooming-report entry the verdict applies to", i)
		}
		out = append(out, GroomingDispositionEntry{
			EntryID:     entryID,
			Verdict:     strings.TrimSpace(d.Verdict),
			CloseTarget: strings.TrimSpace(d.CloseTarget),
		})
	}
	res, err := r.api.RecordGroomingDispositions(ctx, runID, out)
	if err != nil {
		return nil, RecordGroomingDispositionsOutput{}, fmt.Errorf("record grooming dispositions: %w", err)
	}
	return nil, *res, nil
}
