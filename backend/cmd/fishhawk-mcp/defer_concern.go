package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// DeferConcernInput is the fishhawk_defer_concern tool's input schema
// (E22.X / #1202). Mirrors `POST /v0/concerns/{concern_id}/defer`.
// concern_id is required; parent_epic + n are the only NON-derivable
// title coordinates the operator supplies (the follow-up's epic
// placement is an operator judgment) — the entire body is auto-drafted
// from the concern server-side.
type DeferConcernInput struct {
	ConcernID  string   `json:"concern_id" jsonschema:"the stable concern UUID to defer (from fishhawk_get_run_status's run.concerns.items[].id)"`
	ParentEpic string   `json:"parent_epic" jsonschema:"the epic the follow-up rolls up to (an issue reference like '#1196'); its leading [E<n>] title token is fetched to derive the {epic} title placeholder. Operator judgment — not derivable from the concern"`
	N          string   `json:"n" jsonschema:"the child number for the [E<epic>.<n>] title (e.g. '4'); operator judgment, mirroring how fishhawk_file_issue takes {n}"`
	Type       string   `json:"type,omitempty" jsonschema:"optional override of the auto-selected work-item type (bug for a defect category, else chore)"`
	Labels     []string `json:"labels,omitempty" jsonschema:"optional labels merged on top of the type's default labels"`
	Note       string   `json:"note,omitempty" jsonschema:"optional operator addendum folded into the follow-up body and the concern's state_reason"`
}

// DeferConcernOutput surfaces the filed follow-up work item plus the
// now-deferred concern row.
type DeferConcernOutput struct {
	Concern DeferredConcern `json:"concern"`
	Issue   DeferFiledIssue `json:"issue"`
}

// registerDeferConcern wires the fishhawk_defer_concern tool (E22.X /
// #1202): the operator verb that resolves a review concern by filing a
// conventions-complete follow-up work item in one call. It sits between
// fishhawk_fixup_stage (route the concern back to the agent for a bounded
// fix-up) and fishhawk_waive_concern (resolve with no follow-up at all):
// defer says "not now, but track it" — file the issue and resolve.
//
// Auth: write tool. Same scope pair as fix-up/waive (write:stages or
// write:fixups); a run-bound MCP token may defer only its own run's
// concerns.
func registerDeferConcern(srv *mcp.Server, resolver *runResolver) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "fishhawk_defer_concern",
		Description: strings.TrimSpace(`
Defer one open review concern into a pre-drafted follow-up work item and
resolve the concern in a single call.

Use this when a recorded concern is worth a SEPARATE change but should not
block the current merge — instead of hand-authoring a follow-up with
fishhawk_file_issue and then resolving the concern, or leaving it to
clutter every later re-review. It sits between the other two resolution
verbs:

  - fishhawk_fixup_stage  — route the concern BACK to the agent now (a
    bounded fix-up pass; consumes fix-up budget);
  - fishhawk_defer_concern — file a conventions-complete follow-up and
    resolve the concern (this tool; consumes NO fix-up budget);
  - fishhawk_waive_concern — resolve with NO follow-up (a false positive
    or an accepted trade-off).

The defer:

  - files a boarded, epic-linked follow-up work item whose body is
    AUTO-DRAFTED from the concern (its note, severity, category, the
    reviewer model, the evidence run id, and the source PR link) — you do
    NOT write the body;
  - transitions the concern to the terminal deferred state (it stops
    appearing in fishhawk_get_run_status's run.concerns open block and can
    no longer be routed into a fix-up), its state_reason naming the filed
    issue;
  - is orphan-issue-safe: an already-resolved concern is rejected before
    any issue is filed, and a filing failure leaves the concern OPEN so
    you can retry.

You supply only the title coordinates the concern cannot carry: parent_epic
(the epic the follow-up rolls up to) and n (the child number) — exactly
what fishhawk_file_issue requires. type defaults to bug for a defect
category, else chore; labels are merged on top of the type defaults.

Inputs:
  - concern_id  : the stable concern UUID, from fishhawk_get_run_status's
    run.concerns.items[].id.
  - parent_epic : the epic issue reference (e.g. "#1196").
  - n           : the child number for the [E<epic>.<n>] title.
  - type, labels, note : optional overrides / addendum.

Returns the filed follow-up issue (number, url, title, applied labels) and
the updated concern row (state deferred, state_reason naming the issue).
Returns a tool error on:
  - invalid concern_id UUID (caught before the HTTP hop)
  - concern_not_found (404)
  - cross_run_defer (a run-bound token reaching another run's concern, 403)
  - concern_defer_conflict (the concern is not open, or a post-filing
    transition race; 422)
  - work_item_invalid (422) / provider_unimplemented (501) /
    work_item_filing_failed (502) — the concern stays OPEN
  - concern_store_unconfigured (503)
`),
	}, resolver.deferConcern)
}

// deferConcern is the tool handler. It validates concern_id is a UUID
// before the HTTP hop, then delegates to the client; the open-state
// pre-check, the file-first-then-transition ordering, the audit FACT, and
// the subject-binding guard all live server-side in server/defer_concern.go.
func (r *runResolver) deferConcern(ctx context.Context, _ *mcp.CallToolRequest, in DeferConcernInput) (*mcp.CallToolResult, DeferConcernOutput, error) {
	concernID, err := uuid.Parse(in.ConcernID)
	if err != nil {
		return nil, DeferConcernOutput{}, fmt.Errorf("concern_id %q is not a valid UUID: %w", in.ConcernID, err)
	}
	res, err := r.api.DeferConcern(ctx, concernID, DeferConcernParams{
		ParentEpic: in.ParentEpic,
		N:          in.N,
		Type:       in.Type,
		Labels:     in.Labels,
		Note:       in.Note,
	})
	if err != nil {
		return nil, DeferConcernOutput{}, fmt.Errorf("defer concern: %w", err)
	}
	return nil, DeferConcernOutput{Concern: res.Concern, Issue: res.Issue}, nil
}
