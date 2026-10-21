package mcpserver

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// fishhawk_get_merge_queue (ADR-093 slice 1 D7, E87.4 / #4295) is the MCP half
// of the per-base merge-queue read, GET /v0/repos/{owner}/{name}/merge-queue
// (backend/internal/server/merge_queue_read.go): every active entry of one
// repository's merge queue in the caller's account, in queue order, with each
// entry's position, merge intent and ETA, and the head of line with its PR's
// current head. READ-ONLY: it writes nothing and records no audit entry.
//
// The I/O types are unexported on purpose (the package's exported surface is
// pinned by export_surface_test.go); the SDK's schema reflection reads their
// exported FIELDS, which is all it needs.

// getMergeQueueInput is the tool's input schema.
type getMergeQueueInput struct {
	Repo    string `json:"repo" jsonschema:"owner/name of the repository whose merge queue to read"`
	BaseRef string `json:"base_ref,omitempty" jsonschema:"narrow the read to one base ref (e.g. main); omit for every base of the repository with an active entry in your account"`
}

// getMergeQueueOutput is the tool's output schema: the read's body verbatim.
type getMergeQueueOutput struct {
	Repo  string           `json:"repo" jsonschema:"the repository as owner/name"`
	Bases []mergeQueueBase `json:"bases" jsonschema:"one block per base ref: the requested base_ref, or every base of the repository with an active entry in your account; empty when no base has one"`
}

// registerGetMergeQueue wires the fishhawk_get_merge_queue tool.
func registerGetMergeQueue(srv *mcp.Server, resolver *runResolver) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "fishhawk_get_merge_queue",
		Description: strings.TrimSpace(`
WHEN: after fishhawk_merge_run returns status=queued (its next_action), or
whenever you want to see which runs hold or wait for a repository's merge line:
reads the per-base merge queue (ADR-093 slice 1, E87.4 / #4295) for one
repository — every active entry in YOUR account, in queue order.

ELIGIBILITY: an operator token with read:audit (the gate view's tier); a
run-bound agent token is refused 403 run_token_forbidden, because the read
names other runs and their heads. Read-only: it writes nothing and records no
audit entry. Another account's queue on the same repository is never visible.

Per base ref it reports:
  - entries[]: the live entry first (it holds the line; its merge-candidate
    pass runs there), then held entries FIFO by enqueue time. Each carries
    run_id, entry_id, state (live / held), phase (a live entry's progress),
    position (1-based among held entries), merge_intent (true when
    fishhawk_merge_run recorded merge intent; false for an entry the ADR-092
    rebase path enqueued), anchored_head_sha and eta_seconds.
  - head_of_line: the first entry plus its pull request's CURRENT head
    (current_head_sha, read best-effort from the forge; current_head_unavailable
    names why it is absent). When it differs from anchored_head_sha the
    admission re-anchors before the pass.
  - eta_basis: samples + median_seconds of the base's newest merged line turns
    (admission to merge). eta_seconds = position x that median, omitted with
    no merged history. Until ADR-093 slice 2's merge-on-pass lands, a line turn
    includes YOUR merge step, so the estimate is only as fast as merges are.

base_ref (optional) narrows the read to one base. Admission is automatic; a
held entry needs no verb. Re-invoke fishhawk_merge_run once your run's pass is
green. fishhawk_get_run_status's merge_candidate_queue block shows one run's
entry; this read shows the whole line.

Tool errors: repo not owner/name (caught before the HTTP hop); the backend's
401 / 403 insufficient_scope / 403 run_token_forbidden / 403 repo_forbidden;
500 (a queue read failed); 503 merge_queue_unconfigured (no merge queue wired).
`),
	}, resolver.getMergeQueue)
}

// getMergeQueue is the tool handler. A backend refusal is a TOOL ERROR, never
// an empty queue: an empty bases list reads as "nothing is queued", which is
// the opposite of "we could not tell you".
func (r *runResolver) getMergeQueue(ctx context.Context, _ *mcp.CallToolRequest, in getMergeQueueInput) (*mcp.CallToolResult, getMergeQueueOutput, error) {
	repo := strings.TrimSpace(in.Repo)
	res, err := r.api.GetMergeQueue(ctx, repo, strings.TrimSpace(in.BaseRef))
	if err != nil {
		return nil, getMergeQueueOutput{}, fmt.Errorf("get merge queue: %w", err)
	}
	out := getMergeQueueOutput{Repo: res.Repo, Bases: res.Bases}
	if out.Bases == nil {
		out.Bases = []mergeQueueBase{}
	}
	return nil, out, nil
}
