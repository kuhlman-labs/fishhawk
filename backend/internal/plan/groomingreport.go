package plan

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// KindGroomingReport is the top-level discriminator value carried by a
// grooming_report artifact — the SECOND additive sibling of the plan
// artifact (#2235, ADR-065 §3), after clarification_request. A plan
// artifact has no "kind" field (it carries plan_version), so the three
// are routed apart before validation without touching the frozen plan
// schema.
const KindGroomingReport = "grooming_report"

// GroomingReportVersion is the artifact's schema_version token, mirroring
// the canonical docs/spec/grooming-report-v1.schema.json filename stem's
// version and the workflow-spec `schema:` a grooming_report-producing
// stage must declare.
const GroomingReportVersion = "grooming_report_v1"

// Entry classes. Each is the leading segment of a derived entry id and
// names exactly one of the report's six typed arrays.
const (
	GroomingClassOrdering      = "ordering"
	GroomingClassDuplicate     = "duplicate"
	GroomingClassHygiene       = "hygiene"
	GroomingClassDependency    = "dependency"
	GroomingClassVisionDrift   = "vision_drift"
	GroomingClassDecomposition = "decomposition"
)

// GroomingReport is a parsed and schema-validated grooming_report artifact:
// the typed set of PROPOSALS a `plan`-typed propose stage emits when it
// grooms a backlog slice. JSON tags mirror grooming-report-v1.schema.json.
//
// All six entry arrays are REQUIRED by the schema even when empty: an empty
// array states "none found", which is a different claim from omitting the
// key, and #2240's run-over-run diff must not conflate the two.
type GroomingReport struct {
	Kind            string          `json:"kind"`
	ReportVersion   string          `json:"report_version"`
	TicketReference TicketReference `json:"ticket_reference"`
	GeneratedBy     GeneratedBy     `json:"generated_by"`
	Summary         string          `json:"summary"`
	// CharterRef pins the exact charter revision the report was scored
	// against. Optional in grooming_report_v1 (x-intended-required) only
	// because the charter resolution/injection machinery is E54.2 / #2234's
	// slice; this artifact must not block on it.
	CharterRef               *GroomingCharterRef       `json:"charter_ref,omitempty"`
	Ordering                 []OrderingEntry           `json:"ordering"`
	Duplicates               []DuplicateCandidate      `json:"duplicates"`
	HygieneDefects           []HygieneDefect           `json:"hygiene_defects"`
	DependencyEdges          []DependencyEdge          `json:"dependency_edges"`
	VisionDrift              []VisionDriftFlag         `json:"vision_drift"`
	DecompositionSuggestions []DecompositionSuggestion `json:"decomposition_suggestions"`
}

// GroomingCharterRef identifies the charter revision a report was scored
// against, so a reviewer can reproduce the scoring and #2240 can tell a
// genuinely-changed ranking from one that moved because the charter did.
type GroomingCharterRef struct {
	Path        string `json:"path"`
	ContentHash string `json:"content_hash"`
	Ref         string `json:"ref,omitempty"`
}

// ItemRef references one backlog item. Type is the PROVIDER DISCRIMINATOR
// and participates in the derived item key: identical textual coordinates
// on two forges are DIFFERENT items and must not collide on one key.
type ItemRef struct {
	Type string `json:"type"`
	ID   string `json:"id"`
	URL  string `json:"url"`
}

// RubricCitation cites one charter prioritization-rubric line justifying a
// score. Charter ids are conventionally uppercase (V1–V5, R1–R5, U1–U4,
// S1–S5).
type RubricCitation struct {
	RubricID string `json:"rubric_id"`
	Quote    string `json:"quote,omitempty"`
	Note     string `json:"note,omitempty"`
}

// OrderingEntry is one item's position in the proposed ranking.
// RubricCitations is required with minItems:1 by the schema — the
// citation control (charter §5.1): a ranking that cannot be justified
// against the charter is REJECTED, never shipped undecorated.
type OrderingEntry struct {
	ID              string           `json:"id"`
	ItemRef         ItemRef          `json:"item_ref"`
	Rank            int              `json:"rank"`
	Score           float64          `json:"score"`
	RubricCitations []RubricCitation `json:"rubric_citations"`
	Rationale       string           `json:"rationale,omitempty"`
}

// DuplicateCandidate is a candidate duplicate/superseded pair (charter S2).
// The relation is UNORDERED, which is why its id sorts the two item keys.
type DuplicateCandidate struct {
	ID         string    `json:"id"`
	Pair       []ItemRef `json:"pair"`
	Basis      string    `json:"basis"`
	Confidence string    `json:"confidence"`
}

// HygieneDefect is one objective, reversible structural defect (charter S4).
// Defect doubles as the entry id's qualifier, so one item may carry one
// entry per distinct defect without an id collision.
type HygieneDefect struct {
	ID           string  `json:"id"`
	ItemRef      ItemRef `json:"item_ref"`
	Defect       string  `json:"defect"`
	Detail       string  `json:"detail"`
	SuggestedFix string  `json:"suggested_fix,omitempty"`
}

// DependencyEdge is a suggested dependency edge: From depends on To.
// DIRECTIONAL — see GroomingEntryID for why its id is NOT sorted.
type DependencyEdge struct {
	ID    string  `json:"id"`
	From  ItemRef `json:"from"`
	To    ItemRef `json:"to"`
	Basis string  `json:"basis"`
	Kind  string  `json:"kind"`
}

// VisionDriftFlag records an item advancing a charter non-goal or
// mis-aligned with a phase theme. CharterRefID doubles as the entry id's
// qualifier, LOWERCASED.
type VisionDriftFlag struct {
	ID           string  `json:"id"`
	ItemRef      ItemRef `json:"item_ref"`
	Basis        string  `json:"basis"`
	CharterRefID string  `json:"charter_ref_id"`
	Detail       string  `json:"detail"`
}

// DecompositionSuggestion proposes splitting one item into children.
// Proposal only — nothing is filed by this artifact.
type DecompositionSuggestion struct {
	ID               string               `json:"id"`
	ItemRef          ItemRef              `json:"item_ref"`
	Rationale        string               `json:"rationale"`
	ProposedChildren []DecompositionChild `json:"proposed_children"`
}

// DecompositionChild is one proposed child of a decomposition suggestion.
type DecompositionChild struct {
	Title     string `json:"title"`
	ScopeHint string `json:"scope_hint"`
}

// groomingProviderPrefixes maps each item_ref.type the schema admits to its
// item-key provider discriminator. EVERY value in the schema's item-ref type
// enum MUST appear here (#2235 condition F3): an id scheme whose job is to be
// a stable join key cannot have an undefined case, and a bare
// <owner>/<repo>#<number> key would ALSO collide across forges — the same
// textual coordinates on GitHub and GitLab are different items. Widening the
// schema enum without adding a case here makes the entry id underivable, which
// itemKey reports rather than silently dropping the discriminator.
var groomingProviderPrefixes = map[string]string{
	"github_issue": "github",
	"gitlab_issue": "gitlab",
	"jira_issue":   "jira",
}

// itemKey derives the stable, provider-discriminated, lowercased key for one
// backlog item: "<provider>/<id>". An unknown type yields a key that cannot
// match any derivable id, so the recomposition check rejects the entry with a
// message naming the offending type rather than accepting an ambiguous id.
func itemKey(ref ItemRef) string {
	prefix, ok := groomingProviderPrefixes[ref.Type]
	if !ok {
		return "<unsupported-item-ref-type:" + ref.Type + ">"
	}
	return strings.ToLower(prefix + "/" + ref.ID)
}

// GroomingEntryID derives a grooming-report entry id. It is the SINGLE owner
// of the derivation contract stated normatively in
// docs/spec/grooming-report-v1.md — the validator machine-checks every entry
// by recomposing its id through this function, so a per-run-minted id (which
// cannot recompose) is rejected and #2240's run-over-run diff is mechanical
// rather than aspirational.
//
// Forms:
//
//	ordering / decomposition   → "<class>:<item-key>"
//	hygiene / vision_drift     → "<class>:<item-key>:<qualifier>"
//	duplicate                  → "<class>:<key-a>+<key-b>"   (keys SORTED)
//	dependency                 → "<class>:<from-key>+<to-key>" (keys NOT sorted)
//
// WHY duplicate SORTS AND dependency DOES NOT (#2235 condition F1): a duplicate
// pair is an UNORDERED relation — "A and B overlap" is the same proposal as "B
// and A overlap" — so sorting makes the id orientation-stable. A depends_on edge
// is DIRECTIONAL: A-depends-on-B and B-depends-on-A are different proposals with
// opposite meaning. Sorting there would collapse them onto one id, which would
// (a) make report-wide id uniqueness forbid a report from ever proposing both
// directions, and (b) hide a run-over-run edge FLIP as a content change under an
// identical id — precisely the silent-wrong-answer shape this artifact exists to
// make impossible.
//
// The qualifier is LOWERCASED (#2235 condition F4): the schema's entry-id
// qualifier segment is lowercase-only while charter/rubric ids are
// conventionally uppercase, so the derivation, the schema pattern, and the
// recomposition comparison all state the same rule. The implied consequence —
// two charter ids differing only by case map to one qualifier — means charter
// line ids must be case-insensitively unique (documented in the reference doc).
func GroomingEntryID(class string, qualifier string, refs ...ItemRef) string {
	keys := make([]string, 0, len(refs))
	for _, ref := range refs {
		keys = append(keys, itemKey(ref))
	}
	if class == GroomingClassDuplicate && len(keys) == 2 {
		// Unordered relation: sort so the id is orientation-stable.
		sort.Strings(keys)
	}
	id := class + ":" + strings.Join(keys, "+")
	if qualifier != "" {
		id += ":" + strings.ToLower(qualifier)
	}
	return id
}

// ValidateGroomingReport validates bytes against the grooming-report-v1 schema
// and then enforces the four semantic invariants the schema cannot express
// (see docs/spec/grooming-report-v1.md § "Semantic rules"):
//
//   - entry ids are UNIQUE across the whole report — the id is the join key
//     across report → operator decision → applied mutation → audit, so a
//     duplicate is ambiguous exactly as a duplicate clarification-question id is;
//   - an entry's id CLASS PREFIX matches the array it appears in (an "ordering:"
//     id inside duplicates is a routing bug);
//   - an entry's id RECOMPOSES from its own item refs and qualifier via
//     GroomingEntryID — the control that makes run-over-run id stability
//     mechanical, since a per-run-minted id cannot recompose;
//   - the ordering ranks are exactly the permutation 1..N — a duplicated or
//     gapped rank is not an applicable proposal.
//
// The returned error is *ParseError, *SchemaError, or *SemanticError. The
// semantic checks live HERE, not only in ParseGroomingReport, so an invalid
// report is rejected even by callers that never decode the typed struct.
func ValidateGroomingReport(data []byte) error {
	if len(bytes.TrimSpace(data)) == 0 {
		return &ParseError{Msg: "empty document"}
	}
	var raw any
	if err := json.Unmarshal(data, &raw); err != nil {
		return &ParseError{Msg: err.Error(), Cause: err}
	}
	if err := compiledGroomingReportSchema.Validate(raw); err != nil {
		var verr *jsonschema.ValidationError
		if errors.As(err, &verr) {
			return schemaErrorFrom(verr)
		}
		return &SchemaError{Path: "/", Message: err.Error()}
	}
	// Schema accepted the shape. Decode the typed struct to run the semantic
	// rules; a decode failure here would be an internal type-mapping bug.
	var gr GroomingReport
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&gr); err != nil {
		return fmt.Errorf("internal: decode to GroomingReport: %w", err)
	}
	return groomingSemanticCheck(&gr)
}

// ParseGroomingReport validates grooming_report bytes and returns the typed
// *GroomingReport. Equivalent to ValidateGroomingReport followed by a strict
// JSON decode.
func ParseGroomingReport(data []byte) (*GroomingReport, error) {
	if err := ValidateGroomingReport(data); err != nil {
		return nil, err
	}
	var gr GroomingReport
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&gr); err != nil {
		// Schema accepted the bytes; this should only fail on an
		// internal type-mapping bug.
		return nil, fmt.Errorf("internal: decode to GroomingReport: %w", err)
	}
	return &gr, nil
}

// groomingEntry is the class-agnostic view of one report entry the semantic
// checks walk: its declared id, the array it appeared in, and the id the
// derivation contract says it MUST carry.
type groomingEntry struct {
	declaredID string
	class      string
	derivedID  string
	// field names the JSON pointer-ish location used in error messages.
	field string
}

// groomingSemanticCheck runs the four rules ValidateGroomingReport documents.
func groomingSemanticCheck(gr *GroomingReport) error {
	entries := collectGroomingEntries(gr)

	// Rule (b): class prefix matches the array. Checked BEFORE recomposition
	// so a routing bug reports as a routing bug rather than as a mismatched id.
	for _, e := range entries {
		prefix := e.class + ":"
		if !strings.HasPrefix(e.declaredID, prefix) {
			return &SemanticError{Message: fmt.Sprintf(
				"%s: entry id %q must start with the class prefix %q for the array it appears in; an id from another class here is a routing bug",
				e.field, e.declaredID, prefix)}
		}
	}

	// Rule (c): the id recomposes from the entry's own fields.
	for _, e := range entries {
		if e.declaredID != e.derivedID {
			return &SemanticError{Message: fmt.Sprintf(
				"%s: entry id %q is not derived from its own item reference(s); expected %q. Entry ids MUST be derived, never minted per run — a per-run id makes the run-over-run report diff (#2240) impossible. See docs/spec/grooming-report-v1.md § Entry-id derivation",
				e.field, e.declaredID, e.derivedID)}
		}
	}

	// Rule (a): ids are unique across the WHOLE report, not per-array.
	seen := make(map[string]string, len(entries))
	for _, e := range entries {
		if prevField, dup := seen[e.declaredID]; dup {
			return &SemanticError{Message: fmt.Sprintf(
				"%s: duplicate entry id %q (already used by %s); the id is the join key across report → operator decision → applied mutation → audit, so a duplicate is ambiguous",
				e.field, e.declaredID, prevField)}
		}
		seen[e.declaredID] = e.field
	}

	// Rule (d): ordering ranks are exactly the permutation 1..N.
	return checkGroomingRanks(gr.Ordering)
}

// collectGroomingEntries flattens the six typed arrays into the class-agnostic
// view, deriving each entry's contractual id through the single owner
// (GroomingEntryID) so the check can never drift from the derivation.
func collectGroomingEntries(gr *GroomingReport) []groomingEntry {
	out := make([]groomingEntry, 0,
		len(gr.Ordering)+len(gr.Duplicates)+len(gr.HygieneDefects)+
			len(gr.DependencyEdges)+len(gr.VisionDrift)+len(gr.DecompositionSuggestions))

	for i, e := range gr.Ordering {
		out = append(out, groomingEntry{
			declaredID: e.ID,
			class:      GroomingClassOrdering,
			derivedID:  GroomingEntryID(GroomingClassOrdering, "", e.ItemRef),
			field:      fmt.Sprintf("/ordering/%d/id", i),
		})
	}
	for i, e := range gr.Duplicates {
		derived := ""
		if len(e.Pair) == 2 {
			derived = GroomingEntryID(GroomingClassDuplicate, "", e.Pair[0], e.Pair[1])
		}
		out = append(out, groomingEntry{
			declaredID: e.ID,
			class:      GroomingClassDuplicate,
			derivedID:  derived,
			field:      fmt.Sprintf("/duplicates/%d/id", i),
		})
	}
	for i, e := range gr.HygieneDefects {
		out = append(out, groomingEntry{
			declaredID: e.ID,
			class:      GroomingClassHygiene,
			derivedID:  GroomingEntryID(GroomingClassHygiene, e.Defect, e.ItemRef),
			field:      fmt.Sprintf("/hygiene_defects/%d/id", i),
		})
	}
	for i, e := range gr.DependencyEdges {
		out = append(out, groomingEntry{
			declaredID: e.ID,
			class:      GroomingClassDependency,
			// NOT sorted: the edge is directional (condition F1).
			derivedID: GroomingEntryID(GroomingClassDependency, "", e.From, e.To),
			field:     fmt.Sprintf("/dependency_edges/%d/id", i),
		})
	}
	for i, e := range gr.VisionDrift {
		out = append(out, groomingEntry{
			declaredID: e.ID,
			class:      GroomingClassVisionDrift,
			derivedID:  GroomingEntryID(GroomingClassVisionDrift, e.CharterRefID, e.ItemRef),
			field:      fmt.Sprintf("/vision_drift/%d/id", i),
		})
	}
	for i, e := range gr.DecompositionSuggestions {
		out = append(out, groomingEntry{
			declaredID: e.ID,
			class:      GroomingClassDecomposition,
			derivedID:  GroomingEntryID(GroomingClassDecomposition, "", e.ItemRef),
			field:      fmt.Sprintf("/decomposition_suggestions/%d/id", i),
		})
	}
	return out
}

// checkGroomingRanks enforces that the ordering ranks are exactly the
// permutation 1..N. A duplicated rank makes two items claim one position and a
// gap means the proposal does not describe an applicable order; the schema's
// per-entry minimum:1 cannot express either.
func checkGroomingRanks(ordering []OrderingEntry) error {
	n := len(ordering)
	seen := make(map[int]int, n)
	for i, e := range ordering {
		if prev, dup := seen[e.Rank]; dup {
			return &SemanticError{Message: fmt.Sprintf(
				"/ordering/%d/rank: duplicate rank %d (already claimed by /ordering/%d); ranks must be exactly the permutation 1..%d",
				i, e.Rank, prev, n)}
		}
		seen[e.Rank] = i
	}
	for want := 1; want <= n; want++ {
		if _, ok := seen[want]; !ok {
			return &SemanticError{Message: fmt.Sprintf(
				"/ordering: rank %d is missing; ranks must be exactly the permutation 1..%d with no gaps (got %s)",
				want, n, formatRanks(ordering))}
		}
	}
	return nil
}

// formatRanks renders the observed rank multiset for a gap error message.
func formatRanks(ordering []OrderingEntry) string {
	got := make([]string, 0, len(ordering))
	for _, e := range ordering {
		got = append(got, fmt.Sprint(e.Rank))
	}
	return "[" + strings.Join(got, ", ") + "]"
}
