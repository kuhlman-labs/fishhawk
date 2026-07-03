package refinement

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// Inferencer is the drafting-agent inference surface. Its signature matches
// (*claudecode.Client).Inference exactly, so the existing claudecode adapter
// satisfies it structurally with no wrapper and tests can fake it. Draft uses
// only the response text and model id; the usage block is accepted for
// signature compatibility and ignored here (cost accounting is the caller's
// concern once E34.2 wires this to a run).
type Inferencer interface {
	Inference(ctx context.Context, prompt string) (responseText, model string, usage planreview.Usage, err error)
}

// Drafter turns a natural-language brief into a validated EpicDraft via an
// Inferencer. It carries the repo's work-management conventions so the prompt
// can enumerate the label namespaces each filed child will require (feature
// items need area:* and autonomy:*), steering the agent to emit
// conventions-complete labels.
type Drafter struct {
	inferencer  Inferencer
	conventions workmgmt.Conventions
}

// NewDrafter constructs a Drafter over the given Inferencer and conventions.
func NewDrafter(inf Inferencer, conv workmgmt.Conventions) *Drafter {
	return &Drafter{inferencer: inf, conventions: conv}
}

// Draft runs the drafting agent for a brief and returns the validated
// EpicDraft plus the model id that produced it. It builds the closed-field-set
// prompt, invokes the Inferencer, decodes the response with the
// prose-tolerant closed-set decoder, and Validates (failing early on a
// dangling or cyclic dependency edge). Any inference error, decode failure, or
// validation failure is returned wrapped.
//
// The sessionID identifies the refinement session the draft belongs to; it is
// threaded through to persistence by the caller, not used to shape the prompt.
func (d *Drafter) Draft(ctx context.Context, sessionID uuid.UUID, brief string) (EpicDraft, string, error) {
	_ = sessionID // the caller keys persistence on this; the prompt does not use it.

	resp, model, _, err := d.inferencer.Inference(ctx, d.buildPrompt(brief))
	if err != nil {
		return EpicDraft{}, "", fmt.Errorf("refinement: inference: %w", err)
	}

	draft, err := DecodeDraft([]byte(resp))
	if err != nil {
		return EpicDraft{}, "", fmt.Errorf("refinement: decode draft: %w", err)
	}
	if err := draft.Validate(); err != nil {
		return EpicDraft{}, "", err
	}
	return draft, model, nil
}

// draftSchema is the literal JSON Schema text embedded in the prompt. The
// claude CLI exposes no response-schema flag (see planreview/decode.go), so the
// prompt is the only enforcement point: the schema below plus the prose
// enumeration in buildPrompt plus the DisallowUnknownFields decode is what
// keeps the agent's field set closed.
const draftSchema = `{
  "type": "object",
  "additionalProperties": false,
  "required": ["epic", "children"],
  "properties": {
    "epic": {
      "type": "object",
      "additionalProperties": false,
      "required": ["summary", "scope", "out_of_scope"],
      "properties": {
        "summary": {"type": "string"},
        "scope": {"type": "string"},
        "out_of_scope": {"type": "string"}
      }
    },
    "children": {
      "type": "array",
      "minItems": 1,
      "items": {
        "type": "object",
        "additionalProperties": false,
        "required": ["summary", "proposal", "done_means", "acceptance_criteria", "labels", "depends_on"],
        "properties": {
          "summary": {"type": "string"},
          "proposal": {"type": "string"},
          "done_means": {"type": "string"},
          "acceptance_criteria": {"type": "array", "items": {"type": "string"}},
          "labels": {"type": "array", "items": {"type": "string"}},
          "depends_on": {"type": "array", "items": {"type": "integer"}}
        }
      }
    }
  }
}`

// buildPrompt assembles the drafting prompt: the brief, the strict draft JSON
// schema as literal schema text, and a prose enumeration of the closed field
// set — including every inner-element shape (children[] fields, depends_on as
// integer sibling ordinals, acceptance_criteria as a string array). This is
// the #1543/#1567 lesson applied: enumerate inner-element shapes in BOTH the
// schema and the prose, demand exactly one JSON object with no extra fields and
// no surrounding prose, so the closed-set decode has the best chance of a
// first-pass parse.
func (d *Drafter) buildPrompt(brief string) string {
	var b strings.Builder
	b.WriteString("You are a work-decomposition drafting agent for the Fishhawk work tracker.\n")
	b.WriteString("Turn the brief below into a structured epic-with-children draft.\n\n")

	b.WriteString("Brief:\n")
	b.WriteString(strings.TrimSpace(brief))
	b.WriteString("\n\n")

	b.WriteString("Output CONTRACT — read carefully:\n")
	b.WriteString("- Output EXACTLY ONE JSON object and NOTHING else: no prose before or after, no markdown code fence.\n")
	b.WriteString("- The object MUST match this JSON Schema exactly. Every field listed is REQUIRED; do NOT add any field not listed (unknown fields are rejected):\n\n")
	b.WriteString(draftSchema)
	b.WriteString("\n\n")

	b.WriteString("Field-by-field, the CLOSED field set is:\n")
	b.WriteString("- epic.summary: string — one-line epic title.\n")
	b.WriteString("- epic.scope: string — what the epic covers.\n")
	b.WriteString("- epic.out_of_scope: string — what the epic explicitly excludes (empty string if none).\n")
	b.WriteString("- children: array with at least one element. Each element is an object with EXACTLY these fields:\n")
	b.WriteString("  - summary: string — one-line child title.\n")
	b.WriteString("  - proposal: string — what the child does and how.\n")
	b.WriteString("  - done_means: string — the completion criterion in prose.\n")
	b.WriteString("  - acceptance_criteria: array of strings — at least one concrete, checkable criterion.\n")
	b.WriteString("  - labels: array of strings — classification labels.")
	if ns := d.childRequiredNamespaces(); len(ns) > 0 {
		b.WriteString(" Include a label in each of these namespaces: ")
		b.WriteString(strings.Join(ns, ", "))
		b.WriteString(" (e.g. area:backend, autonomy:medium).")
	}
	b.WriteString("\n")
	b.WriteString("  - depends_on: array of integers — 1-BASED ordinals of OTHER children in this same array that must finish first (the first child is 1, the second is 2, ...). Use an empty array for a child with no dependencies. Do NOT reference an ordinal outside the array and do NOT reference a child's own ordinal.\n")

	return b.String()
}

// childRequiredNamespaces returns the label namespaces a filed child (rendered
// as the feature type) is expected to carry, drawn from the conventions so the
// prompt steers the agent to conventions-complete labels. Returns nil when the
// conventions declare no feature type or no required namespaces.
func (d *Drafter) childRequiredNamespaces() []string {
	ft, ok := d.conventions.Types["feature"]
	if !ok {
		return nil
	}
	return ft.RequiredLabelNamespaces
}
