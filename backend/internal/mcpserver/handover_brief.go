package mcpserver

import (
	"context"
	"fmt"
	"reflect"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kuhlman-labs/fishhawk/backend/internal/handoverbrief"
)

// HandoverBriefInput is the fishhawk_handover_brief tool's input schema
// (E76.4 / #3767, ADR-083 #3751, ADR-082 #3728 rule 7). Every field is
// optional except the repo (which falls back to GITHUB_REPOSITORY).
type HandoverBriefInput struct {
	Repo         string `json:"repo,omitempty" jsonschema:"target repo as owner/name; falls back to GITHUB_REPOSITORY env when omitted"`
	Section      string `json:"section,omitempty" jsonschema:"one of what_changed, needs_decision, in_flight, delegation_in_force, standing_orders — narrows the brief to that section; omit for the whole brief"`
	FromSequence int64  `json:"from_sequence,omitempty" jsonschema:"INCLUSIVE window start override; omitted = the sequence after the last captain_assigned entry (or 1 for a first captain). Copy a continuation cursor's from_sequence verbatim"`
	ToSequence   int64  `json:"to_sequence,omitempty" jsonschema:"INCLUSIVE window end; omitted = the repository chain head (above the head is refused). Pass a recorded offer's brief_to_sequence to re-read the brief that offer stamped"`
}

// HandoverBriefOutput is the tool's result: the SAME handoverbrief.Brief wire
// model the REST route serves, re-bounded here by handoverbrief.Bound at this
// session's response budget — the one serialized-size bound both surfaces
// share. brief_hash is the hash of the canonical, UNBOUNDED composition and
// is never recomputed over the bounded body.
type HandoverBriefOutput struct {
	Brief *handoverbrief.Brief `json:"brief" jsonschema:"the bounded brief; brief_hash identifies the canonical unbounded composition (the value an offer stamps). When truncated is true, next and each part's next name the continuation — each cursor's call is the exact underlying REST query (digest section, campaigns or runs list, delegation read)"`
}

// handoverBriefOutputSchema is the tool's explicit output schema. It is
// supplied rather than reflected because the brief carries uuid.UUID fields
// (digest.Item / digest.Gap run ids, in-flight campaign and run ids): the
// SDK's reflection sees a [16]byte and would advertise an integer array while
// the value marshals as a string, so every non-empty brief would fail the
// SDK's own output validation. Mapping uuid.UUID to a string schema keeps the
// wire model shared with REST — the fishhawk_digest precedent.
func handoverBriefOutputSchema() *jsonschema.Schema {
	s, err := jsonschema.For[HandoverBriefOutput](&jsonschema.ForOptions{
		TypeSchemas: map[reflect.Type]*jsonschema.Schema{
			reflect.TypeFor[uuid.UUID](): {Type: "string"},
		},
	})
	if err != nil {
		// A reflection failure is a programming error in the static type
		// above, surfaced at registration like any other AddTool panic.
		panic(fmt.Sprintf("fishhawk_handover_brief output schema: %v", err))
	}
	return s
}

// registerHandoverBrief wires the fishhawk_handover_brief tool (E76.4 /
// #3767): a thin, read-only wrapper over GET /v0/handover-brief.
func registerHandoverBrief(srv *mcp.Server, resolver *runResolver) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "fishhawk_handover_brief",
		Description: strings.TrimSpace(`
Use this when you are an incoming captain for a repository (a handover was
offered to you, or you are about to accept or claim the seat) and need
everything that happened since the last handover: what changed (merges,
waivers and deferrals), what needs a decision now (unanswered pages and parked
gates), what is in flight (non-terminal campaigns and runs), the delegation in
force per workflow and the standing orders (workflow spec sha / version) —
every item citing its audit chain entry by sequence and entry hash. Any caller
holding read:audit may call it; it NEVER writes and mints no audit entry.

The window starts after the last captain_assigned entry (basis
since_last_handover) or at sequence 1 for a first captain (basis
first_captain) and ends at the chain head. Sections with no source yet
(charter_revision, adr_index, doctrine_changes) are listed under absent with a
reason; a collaborator read failure is listed under degradations and drops
only that part.

brief_hash identifies the canonical, UNBOUNDED composition: an offer stamps it
on its captain_handover_offered entry (with brief_from_sequence /
brief_to_sequence). To re-read the brief an offer recorded, pass that
to_sequence; live parts (parked gates, campaigns, runs) may differ on a later
read, which changes the hash.

Bounded: the brief is trimmed to this session's response byte budget by the
same handoverbrief.Bound the REST route applies; brief_hash is carried through
unchanged. When truncated is true, next (and each part's next / gaps_next)
names the continuation — its call field is the exact underlying REST query
(GET /v0/digest?..., GET /v0/campaigns?..., GET /v0/runs?...) to page the
remainder.

Tool errors: repo missing; negative sequences; validation_failed (400);
authentication_required (401); insufficient_scope / repo_forbidden (403);
handover_brief_unconfigured (501); handover_brief_unavailable (503 — the
captain record or chain head could not be read).
`),
		OutputSchema: handoverBriefOutputSchema(),
	}, resolver.handoverBrief)
}

// handoverBrief is the tool handler.
func (r *runResolver) handoverBrief(ctx context.Context, _ *mcp.CallToolRequest, in HandoverBriefInput) (*mcp.CallToolResult, HandoverBriefOutput, error) {
	repo := strings.TrimSpace(in.Repo)
	if repo == "" {
		repo = strings.TrimSpace(r.getenv("GITHUB_REPOSITORY"))
	}
	if repo == "" {
		return nil, HandoverBriefOutput{}, fmt.Errorf("repo is required: pass repo or set GITHUB_REPOSITORY")
	}
	if in.FromSequence < 0 || in.ToSequence < 0 {
		return nil, HandoverBriefOutput{}, fmt.Errorf("from_sequence and to_sequence must be non-negative")
	}
	b, err := r.api.GetHandoverBrief(ctx, repo, strings.TrimSpace(in.Section), in.FromSequence, in.ToSequence)
	if err != nil {
		return nil, HandoverBriefOutput{}, fmt.Errorf("get handover brief: %w", err)
	}
	// The REST route already applied handoverbrief.Bound at
	// handoverbrief.DefaultByteBudget; re-applying it at this session's
	// resolved budget is what makes one bound govern both surfaces. Bound
	// copies BriefHash through untouched, so the bounded render still names
	// the canonical composition.
	bounded, err := handoverbrief.Bound(*b, mcpResponseByteBudget(r.getenv))
	if err != nil {
		return nil, HandoverBriefOutput{}, fmt.Errorf("bound handover brief: %w", err)
	}
	return nil, HandoverBriefOutput{Brief: &bounded}, nil
}
