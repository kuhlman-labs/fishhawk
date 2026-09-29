package mcpserver

import (
	"context"
	"fmt"
	"reflect"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kuhlman-labs/fishhawk/backend/internal/digest"
)

// DigestInput is the fishhawk_digest tool's input schema (E75.6 / #3734,
// ADR-082 #3728 rule 7). Two modes over the two REST routes:
//
//   - read (mark_read false, the default): GET /v0/digest. Never writes —
//     retrieving a digest does not advance the watermark.
//   - mark-read (mark_read true): POST /v0/digest/mark-read with to_sequence.
//     section/from_sequence are refused in this mode rather than silently
//     ignored, because a caller passing them believes they are narrowing
//     something.
type DigestInput struct {
	Repo         string `json:"repo,omitempty" jsonschema:"target repo as owner/name; falls back to GITHUB_REPOSITORY env when omitted"`
	Section      string `json:"section,omitempty" jsonschema:"read mode only: one of merges, waivers_and_deferrals, pages, open_decisions, gaps — copy a continuation cursor's section verbatim; omit for the full digest"`
	FromSequence int64  `json:"from_sequence,omitempty" jsonschema:"read mode only: INCLUSIVE window start; copy a continuation cursor's from_sequence verbatim; omitted = your watermark + 1"`
	ToSequence   int64  `json:"to_sequence,omitempty" jsonschema:"read mode: INCLUSIVE window end (omitted = the repository chain head; above the head is refused). mark-read mode: REQUIRED — the to_sequence of the digest you actually read"`
	MarkRead     bool   `json:"mark_read,omitempty" jsonschema:"true advances your read watermark to to_sequence via POST /v0/digest/mark-read (needs write:approvals; appends a digest_marked_read audit entry first). false (default) only reads"`
}

// DigestMarkReadResult mirrors the backend's POST /v0/digest/mark-read 200
// body (server.digestMarkReadResponse: repo + captain_subject + the embedded
// digest.MarkReadResult fields, flattened on the wire).
type DigestMarkReadResult struct {
	Repo             string `json:"repo"`
	CaptainSubject   string `json:"captain_subject"`
	PreviousSequence int64  `json:"previous_sequence"`
	HadPrevious      bool   `json:"had_previous"`
	Sequence         int64  `json:"sequence" jsonschema:"the watermark after the call"`
	Advanced         bool   `json:"advanced" jsonschema:"false when to_sequence was at or below the watermark: a no-op that appended nothing"`
}

// DigestOutput is the tool's result: exactly one of Digest (read mode) or
// MarkRead (mark-read mode) is set. Digest is the SAME digest.Digest wire
// model the REST route serves, re-bounded here by digest.Bound at this
// session's response budget — the one serialized-size bound both surfaces
// share.
type DigestOutput struct {
	Digest   *digest.Digest        `json:"digest,omitempty" jsonschema:"read mode: the bounded digest; when truncated is true, next (and each section's next / gaps_next) names the continuation — pass its section, from_sequence and to_sequence back to this tool"`
	MarkRead *DigestMarkReadResult `json:"mark_read,omitempty" jsonschema:"mark-read mode: the watermark before and after the call"`
}

// digestOutputSchema is the tool's explicit output schema. It is supplied
// rather than reflected because digest.Item/Gap carry uuid.UUID fields: the
// SDK's reflection sees a [16]byte and would advertise an integer array, while
// the value marshals as a string — every non-empty digest would then fail the
// SDK's own output validation. Mapping uuid.UUID to a string schema keeps the
// wire model shared with REST instead of forking a hand-maintained mirror (the
// #371 drift class).
func digestOutputSchema() *jsonschema.Schema {
	s, err := jsonschema.For[DigestOutput](&jsonschema.ForOptions{
		TypeSchemas: map[reflect.Type]*jsonschema.Schema{
			reflect.TypeFor[uuid.UUID](): {Type: "string"},
		},
	})
	if err != nil {
		// A reflection failure is a programming error in the static type
		// above, surfaced at registration like any other AddTool panic.
		panic(fmt.Sprintf("fishhawk_digest output schema: %v", err))
	}
	return s
}

// registerDigest wires the fishhawk_digest tool (E75.6 / #3734): a thin
// wrapper over GET /v0/digest and POST /v0/digest/mark-read.
func registerDigest(srv *mcp.Server, resolver *runResolver) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "fishhawk_digest",
		Description: strings.TrimSpace(`
Use this when you (a captain) return to a repository and need "what happened
since I last looked": merges, waivers and deferrals (with pointers to their
reasons), pages raised and whether each was answered, and decisions still open
(parked gates, unanswered clarifications, escalations) — each item citing its
audit chain entry by sequence and entry hash. Computed on demand against your
per-repository READ WATERMARK (ADR-082 rule 7).

Modes:
  - read (default): GET /v0/digest. NEVER writes — reading a digest does not
    advance your watermark. The window defaults to (watermark, chain head].
  - mark_read=true: POST /v0/digest/mark-read with to_sequence (REQUIRED — the
    to_sequence of the digest you read). Appends a digest_marked_read audit
    entry FIRST, then advances the watermark. A to_sequence at or below the
    watermark is a no-op (advanced=false, nothing appended). Needs
    write:approvals.

Bounded: the digest is trimmed to this session's response byte budget by the
same digest.Bound the REST route applies. When truncated is true, next names
the continuation — call this tool again with its section, from_sequence and
to_sequence (the cursor's call field shows the equivalent REST request).
section=gaps pages through gaps and degradations. Decision-bearing entries the
index never recorded are reported as gaps, never dropped.

Tool errors: repo missing; section/from_sequence passed with mark_read;
to_sequence missing with mark_read; validation_failed (400);
to_sequence_beyond_chain_head (400, names the head); authentication_required
(401); insufficient_scope (403); digest_unconfigured (501).
`),
		OutputSchema: digestOutputSchema(),
	}, resolver.digest)
}

// digest is the tool handler.
func (r *runResolver) digest(ctx context.Context, _ *mcp.CallToolRequest, in DigestInput) (*mcp.CallToolResult, DigestOutput, error) {
	repo := strings.TrimSpace(in.Repo)
	if repo == "" {
		repo = strings.TrimSpace(r.getenv("GITHUB_REPOSITORY"))
	}
	if repo == "" {
		return nil, DigestOutput{}, fmt.Errorf("repo is required: pass repo or set GITHUB_REPOSITORY")
	}
	if in.FromSequence < 0 || in.ToSequence < 0 {
		return nil, DigestOutput{}, fmt.Errorf("from_sequence and to_sequence must be non-negative")
	}
	if in.MarkRead {
		if strings.TrimSpace(in.Section) != "" || in.FromSequence != 0 {
			return nil, DigestOutput{}, fmt.Errorf("section and from_sequence select what to READ and do not apply with mark_read=true: pass only repo and to_sequence")
		}
		if in.ToSequence == 0 {
			return nil, DigestOutput{}, fmt.Errorf("to_sequence is required with mark_read=true: pass the to_sequence of the digest you read, so the watermark never skips an item you have not seen")
		}
		res, err := r.api.MarkDigestRead(ctx, repo, in.ToSequence)
		if err != nil {
			return nil, DigestOutput{}, fmt.Errorf("mark digest read: %w", err)
		}
		return nil, DigestOutput{MarkRead: res}, nil
	}
	d, err := r.api.GetDigest(ctx, repo, strings.TrimSpace(in.Section), in.FromSequence, in.ToSequence)
	if err != nil {
		return nil, DigestOutput{}, fmt.Errorf("get digest: %w", err)
	}
	// The REST route already applied digest.Bound at digest.DefaultByteBudget;
	// re-applying it at this session's resolved budget is what makes one bound
	// govern both surfaces. Bound composes: a section it cuts further keeps
	// the REST-side OmittedCount and moves its cursor to the new first omitted
	// item; a section it leaves alone keeps the REST-side markers verbatim.
	bounded, err := digest.Bound(*d, mcpResponseByteBudget(r.getenv))
	if err != nil {
		return nil, DigestOutput{}, fmt.Errorf("bound digest: %w", err)
	}
	return nil, DigestOutput{Digest: &bounded}, nil
}
