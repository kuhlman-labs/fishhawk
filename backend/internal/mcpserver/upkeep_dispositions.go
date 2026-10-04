package mcpserver

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// UpkeepDispositionEntry is one requested per-finding verdict. It is the wire
// shape the backend's capture request body takes, so the tool forwards it
// unchanged. ParentEpic is a pointer so a present-but-empty value reaches the
// backend (which refuses it) instead of being read as absent.
type UpkeepDispositionEntry struct {
	FindingID               string  `json:"finding_id" jsonschema:"the upkeep-report finding's stable DERIVED id (e.g. 'flake:TestWidgetSync'); it must be an id the run's recorded upkeep_report declares"`
	Verdict                 string  `json:"verdict" jsonschema:"one of approved, rejected — the closed upkeep verdict set"`
	AuthorizeDelegationTier bool    `json:"authorize_delegation_tier,omitempty" jsonschema:"optional, approved only: the captain's authorization to apply the finding's OWN proposed autonomy:* label when the apply files its issue; inert when the finding proposes no tier label"`
	ParentEpic              *string `json:"parent_epic,omitempty" jsonschema:"optional, approved only: override the parent epic of the issue the apply files, a positive issue number bare (389) or #-prefixed (#389)"`
}

// RecordUpkeepDispositionsInput is the fishhawk_record_upkeep_dispositions
// tool's input schema (#3923).
type RecordUpkeepDispositionsInput struct {
	RunID        string                   `json:"run_id" jsonschema:"the Fishhawk run UUID whose recorded upkeep_report the dispositions attach to"`
	Dispositions []UpkeepDispositionEntry `json:"dispositions" jsonschema:"one entry per verdict; the batch is validated ATOMICALLY server-side — one unknown finding_id records NOTHING"`
}

// RecordedUpkeepDisposition is one projected disposition — the read-back row
// the capture returns.
type RecordedUpkeepDisposition struct {
	FindingID               string `json:"finding_id"`
	Source                  string `json:"source"`
	Verdict                 string `json:"verdict"`
	AuthorizeDelegationTier bool   `json:"authorize_delegation_tier"`
	ParentEpic              string `json:"parent_epic,omitempty"`
	RecordedAt              string `json:"recorded_at"`
	RecordedBy              string `json:"recorded_by"`
	AuditSequence           int64  `json:"audit_sequence"`
}

// RecordUpkeepDispositionsOutput is the capture's 200 body: the report artifact
// the dispositions attached to plus the FULL current disposition set, so the
// verb gets its read-back in the same call.
type RecordUpkeepDispositionsOutput struct {
	RunID       string `json:"run_id"`
	ArtifactID  string `json:"artifact_id"`
	StageID     string `json:"stage_id"`
	ContentHash string `json:"content_hash"`
	// WindowClosed is true once the #3924 apply has settled this artifact's
	// capture window; after it a capture for this artifact is refused 409
	// upkeep_window_closed.
	WindowClosed bool `json:"window_closed"`
	// Settlement carries the watermark's facts when WindowClosed is true. The
	// backend projects the upkeep watermark through the SAME settlement shape
	// as grooming's, so the unexported groomingWindowSettlement is reused
	// rather than duplicated (it stays off the pinned export surface).
	Settlement   *groomingWindowSettlement   `json:"settlement,omitempty"`
	Dispositions []RecordedUpkeepDisposition `json:"dispositions"`
}

// ListUpkeepDispositionsOutput is the read-back body. Same shape as the
// capture's — both verbs share one server-side projection, so the POST echo and
// the GET read-back are the same bytes by construction.
type ListUpkeepDispositionsOutput = RecordUpkeepDispositionsOutput

// registerRecordUpkeepDispositions wires fishhawk_record_upkeep_dispositions
// (#3923).
//
// Auth: captain-only. The backend refuses BOTH a run-bound agent token
// ("mcp:run:<uuid>" subject, 403 run_token_forbidden) and a delegated
// operator-agent token ("operator-agent/" subject prefix, 403
// operator_agent_forbidden) — the upkeep report is agent-authored, so an agent
// that could also disposition it would convert the captain gate into a
// self-approval.
func registerRecordUpkeepDispositions(srv *mcp.Server, resolver *runResolver) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "fishhawk_record_upkeep_dispositions",
		Description: strings.TrimSpace(`
Record the captain's per-finding verdicts on an upkeep report, as an auditable
fact (#3923).

WHEN: after reading an upkeep_scan run's upkeep_report and deciding, finding by
finding, whether the apply should file its proposed issue: approved or
rejected. ELIGIBILITY: captain-only — write:approvals, and the backend refuses a
run-bound agent token (even for its own run) AND a delegated operator-agent
token, because the report is agent-authored and an agent dispositioning it would
be a self-approval.

Each disposition persists as one chained upkeep_disposition_recorded audit row
keyed by the finding's stable DERIVED id. An approved entry may also carry:
  - authorize_delegation_tier: true — the captain's authorization to apply the
    finding's OWN proposed autonomy:* label when the apply files the issue
    (inert when the finding proposes no tier label);
  - parent_epic — an override for the filed issue's parent epic.
Both are refused on a rejected verdict: a rejected finding files nothing.

WHICH REPORT. The dispositions bind to the report named by the HIGHEST-sequence
upkeep_report_recorded row on the run's chain, resolved server-side; the
resolved artifact_id comes back in the response. If a newer report is recorded
between resolution and append, the capture is refused 409
upkeep_report_superseded (naming current_artifact_id) and records nothing —
re-read the report and re-capture.

WINDOW. The apply (#3924) settles the capture window by appending a watermark on
approve AND reject; after it, a capture for that report returns 409
upkeep_window_closed and records nothing.

Semantics worth knowing before you call:
  - the batch is ATOMIC — one unknown finding_id records NOTHING;
  - a finding_id may not repeat WITHIN one request (ambiguous intent), but a
    LATER request on the same finding SUPERSEDES the earlier one (last wins) and
    both rows stay in the chain, so the correction is itself auditable.

Inputs:
  - run_id       : the run whose recorded upkeep_report is being dispositioned.
  - dispositions : one {finding_id, verdict, authorize_delegation_tier?, parent_epic?}
                   per decided finding (at most 200).

Returns the resolved artifact plus the FULL current disposition set for it (the
read-back rides along, so no separate read is needed). Tool errors:
  - invalid UUID / empty dispositions / empty finding_id (caught before the HTTP hop)
  - validation_failed (malformed body, > 200 entries, a finding_id repeated in one batch,
    an invalid parent_epic, parent_epic or authorize_delegation_tier on a rejected verdict, 400)
  - upkeep_verdict_invalid (a verdict outside approved/rejected, 400)
  - run_token_forbidden (a run-bound agent token attempted the capture, 403)
  - operator_agent_forbidden (a delegated operator-agent token attempted it, 403)
  - insufficient_scope (token lacks write:approvals, 403)
  - run_not_found (no such run, 404)
  - upkeep_report_absent (the run has no recorded upkeep_report, 409)
  - upkeep_window_closed (this report's capture window has been settled; nothing recorded, 409)
  - upkeep_report_superseded (a newer report was recorded mid-capture; nothing recorded, 409)
  - upkeep_finding_unknown (an id the recorded report does not declare, 422)
  - upkeep_dispositions_unconfigured (repositories not wired, 503)
`),
	}, resolver.recordUpkeepDispositions)
}

// recordUpkeepDispositions is the tool handler. It validates the run UUID and
// the batch shape locally — a fast fail before the HTTP hop — and delegates
// every AUTHORIZATION and DOMAIN decision (the captain-only refusals, the
// verdict set, the finding-id set, the override rules, batch atomicity) to the
// backend, which is the single authority for all of them. A second copy of a
// closed set here would be a drift source.
func (r *runResolver) recordUpkeepDispositions(ctx context.Context, _ *mcp.CallToolRequest,
	in RecordUpkeepDispositionsInput) (*mcp.CallToolResult, RecordUpkeepDispositionsOutput, error) {
	runID, err := uuid.Parse(in.RunID)
	if err != nil {
		return nil, RecordUpkeepDispositionsOutput{},
			fmt.Errorf("run_id %q is not a valid UUID: %w", in.RunID, err)
	}
	if len(in.Dispositions) == 0 {
		return nil, RecordUpkeepDispositionsOutput{},
			fmt.Errorf("dispositions is required: name at least one upkeep-report finding and the verdict to record for it")
	}
	out := make([]UpkeepDispositionEntry, 0, len(in.Dispositions))
	for i, d := range in.Dispositions {
		findingID := strings.TrimSpace(d.FindingID)
		if findingID == "" {
			return nil, RecordUpkeepDispositionsOutput{},
				fmt.Errorf("dispositions[%d].finding_id is required: name the upkeep-report finding the verdict applies to", i)
		}
		e := UpkeepDispositionEntry{
			FindingID:               findingID,
			Verdict:                 strings.TrimSpace(d.Verdict),
			AuthorizeDelegationTier: d.AuthorizeDelegationTier,
		}
		if d.ParentEpic != nil {
			pe := strings.TrimSpace(*d.ParentEpic)
			e.ParentEpic = &pe
		}
		out = append(out, e)
	}
	res, err := r.api.RecordUpkeepDispositions(ctx, runID, out)
	if err != nil {
		return nil, RecordUpkeepDispositionsOutput{}, fmt.Errorf("record upkeep dispositions: %w", err)
	}
	return nil, *res, nil
}
