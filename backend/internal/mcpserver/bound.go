package mcpserver

import (
	"encoding/json"
	"fmt"
	"log"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// ---------------------------------------------------------------------------
// Byte budget (ADR-077 / #2508)
// ---------------------------------------------------------------------------

// runStatusByteBudgetDefault bounds the MARSHALLED fishhawk_get_run_status
// response. The number is evidence-backed, not chosen.
//
// The MCP client rejects on TOKENS ("result (N characters) exceeds maximum
// allowed tokens"), and fishhawkd cannot tokenize, so a byte budget is
// necessarily a PROXY. The character threshold was bracketed by driving
// fishhawk_list_audit through the real MCP client:
//
//	42,280 chars  SUCCEEDED
//	54,262 chars  FAILED
//	81,530 chars  FAILED
//	75,963 chars  FAILED  (an independent result)
//
// so the measured threshold is 42,280 < T < 54,262. 32 KiB is the largest
// confirmed success less ~20% of headroom.
//
// This bounds the INNER response only: the server never sees the transport
// envelope the client actually measures, so the headroom absorbs it.
const runStatusByteBudgetDefault = 32768

// runStatusBudgetEnvVar overrides runStatusByteBudgetDefault for a client with
// a different tokenizer. An override BELOW minimalRunStatusMaxBytes is CLAMPED
// UP, never accepted-and-silently-not-honoured — see runStatusByteBudget.
const runStatusBudgetEnvVar = "FISHHAWKD_MCP_RUN_STATUS_BUDGET_BYTES"

// minimalRunStatusMaxBytes is the constant-size absolute floor minimalRunStatus
// is pinned under. Convergence can only ever guarantee max(budget, this), so it
// is also the clamp target for a below-floor override.
const minimalRunStatusMaxBytes = 4096

// runStatusByteBudget resolves the effective budget through the injectable
// getenv seam (runResolver.getenv) rather than os.Getenv, so every branch is
// testable. Four named branches:
//
//	absent / unparseable / non-positive -> the default
//	below minimalRunStatusMaxBytes      -> CLAMPED UP to the floor
//
// Clamping (not rejecting) is deliberate: the override exists to serve a client
// with a LOWER limit, and rejecting it back up to the 32 KiB default would
// leave that client worse off than the clamp does. The clamp is announced in
// the same breath on TWO surfaces — this log line, and the elisions block's own
// Budget field, which reports the EFFECTIVE (post-clamp) value so the wire
// never claims a bound the ladder did not honour.
func runStatusByteBudget(getenv func(string) string) int {
	if getenv == nil {
		return runStatusByteBudgetDefault
	}
	raw := strings.TrimSpace(getenv(runStatusBudgetEnvVar))
	if raw == "" {
		return runStatusByteBudgetDefault
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		log.Printf("mcp: %s=%q is not an integer; using the %d-byte default", runStatusBudgetEnvVar, raw, runStatusByteBudgetDefault)
		return runStatusByteBudgetDefault
	}
	if n <= 0 {
		log.Printf("mcp: %s=%d is not positive; using the %d-byte default", runStatusBudgetEnvVar, n, runStatusByteBudgetDefault)
		return runStatusByteBudgetDefault
	}
	if n < minimalRunStatusMaxBytes {
		log.Printf("mcp: %s=%d is below the %d-byte convergence floor; CLAMPED to %d (the elisions block reports the effective budget)",
			runStatusBudgetEnvVar, n, minimalRunStatusMaxBytes, minimalRunStatusMaxBytes)
		return minimalRunStatusMaxBytes
	}
	return n
}

// ---------------------------------------------------------------------------
// The escape-aware cap primitive (surface-agnostic — no get_run_status here)
// ---------------------------------------------------------------------------

// jsonEncodedLen returns the EXACT byte length encoding/json produces for s,
// INCLUDING both surrounding quote bytes. A raw byte count is not a bound on
// encoded size: the encoder emits two-byte short escapes, the six-byte \u00XX
// form for every other control byte, the six-byte form for < > & (Go's default
// HTML escaping) and for U+2028 / U+2029, and � (six bytes) for each
// INVALID UTF-8 byte — an inflation factor that reaches 6x.
//
// Invalid UTF-8 is its own cost class: a `for range` over a string yields
// U+FFFD with a one-byte advance for an invalid byte, which is NOT the width
// the encoder emits, so the walk uses utf8.DecodeRuneInString and counts
// RuneError-with-size-1 explicitly.
func jsonEncodedLen(s string) int {
	n := 2 // the surrounding quotes
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		n += jsonRuneCost(r, size)
		i += size
	}
	return n
}

// jsonRuneCost is the encoded byte cost of one decoded rune.
func jsonRuneCost(r rune, size int) int {
	if r == utf8.RuneError && size == 1 {
		return 6 // �
	}
	if r < utf8.RuneSelf {
		switch r {
		case '"', '\\', '\n', '\r', '\t':
			return 2
		case '<', '>', '&':
			return 6 // < etc — Go HTML-escapes by default
		}
		if r < 0x20 {
			return 6 // \u00XX
		}
		return 1
	}
	if r == ' ' || r == ' ' {
		return 6
	}
	return size
}

// capJSONString truncates s to the longest rune-boundary prefix whose
// json-encoded form fits in max(budget, 2) bytes. The max(budget, 2) floor is
// what makes a sub-minimum or negative budget DEFINED — no JSON string encodes
// smaller than "" — instead of an impossible assertion. The result is always
// valid UTF-8 and always a rune-boundary prefix of s.
//
// This is NOT compact.go's truncateOversizedString, which is a raw-byte cap and
// therefore not a size bound under escaping; that function and
// auditPayloadStringCap are deliberately left untouched (#1749).
func capJSONString(s string, budget int) string {
	if budget < 2 {
		budget = 2
	}
	n := 2 // reserve the quotes up front
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			// Stop AT an invalid byte rather than carrying it into the result:
			// including it would keep the prefix property but break the
			// always-valid-UTF-8 one, and the encoder would substitute U+FFFD
			// for it anyway.
			return s[:i]
		}
		cost := jsonRuneCost(r, size)
		if n+cost > budget {
			return s[:i]
		}
		n += cost
		i += size
	}
	return s
}

// marshalledLen is the measured size of v on the wire.
func marshalledLen(v any) (int, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return 0, err
	}
	return len(raw), nil
}

// ---------------------------------------------------------------------------
// The INTERNAL accumulator: three elision classes, constructor-only
// ---------------------------------------------------------------------------

// elisionClass is the three-way honesty classification (ADR-077 Q1).
type elisionClass string

const (
	// classStored: the omitted content EXISTS in durable storage and the
	// pointer retrieves it.
	classStored elisionClass = "stored"
	// classOversizedCapable: the field is stored but can itself be arbitrarily
	// large, so the pointer must name an UNBOUNDED surface (a REST path), never
	// a bounded fishhawk_* MCP tool that would cap the same value again.
	classOversizedCapable elisionClass = "oversized_capable"
	// classComputed: the value was DERIVED at read time and never stored, so no
	// pointer can retrieve it — recomputation is not retrieval.
	classComputed elisionClass = "computed"
)

// retrievalPointer is a rendered retrieval surface. It is produced ONLY by
// pointerListAudit, pointerGateView or pointerREST — never by a bare string —
// so a stored elision cannot name an arbitrary or circular surface.
type retrievalPointer struct{ ref string }

func (p retrievalPointer) String() string { return p.ref }

// unboundedPointer is a retrievalPointer that additionally promises to be an
// UNBOUNDED surface. It embeds retrievalPointer, so an unboundedPointer widens
// to a retrievalPointer but never the reverse — which is what makes
// newOversizedCapableElision's signature reject a bounded fishhawk_* pointer at
// COMPILE time rather than at review time.
type unboundedPointer struct{ retrievalPointer }

// pointerListAudit names the anchored audit walk. since_sequence is the ANCHOR
// that makes the at-least promise hold: the client omits the parameter at zero
// (client.go gates on `f.SinceSequence > 0`) and the REST filter is
// strictly-greater-than, so zero yields the WHOLE chain — a superset, which the
// at-least contract permits and an exact-equality contract would not.
func pointerListAudit(runID, category string, sinceSequence int64) retrievalPointer {
	if category == "" {
		return retrievalPointer{fmt.Sprintf("fishhawk_list_audit(run_id=%s, since_sequence=%d)", runID, sinceSequence)}
	}
	return retrievalPointer{fmt.Sprintf("fishhawk_list_audit(run_id=%s, category=%s, since_sequence=%d)", runID, category, sinceSequence)}
}

// pointerGateView names the one-call review-gate read.
func pointerGateView(runID string) retrievalPointer {
	return retrievalPointer{fmt.Sprintf("fishhawk_get_gate_view(run_id=%s)", runID)}
}

// pointerREST names an UNBOUNDED REST surface — the only kind an
// oversized-capable elision may point at.
func pointerREST(path string) unboundedPointer {
	return unboundedPointer{retrievalPointer{"GET " + path}}
}

// elidedField is one accumulated omission. ALL FIELDS ARE UNEXPORTED and the
// three constructors below are the only construction path outside this
// package's own tests, so the invalid states are unrepresentable BY SIGNATURE.
type elidedField struct {
	field        string
	reason       string
	class        elisionClass
	pointer      string
	omittedCount int
	aggregate    bool
}

// newStoredElision takes the retrieval surface as a REQUIRED parameter, so a
// stored elision with no pointer cannot be spelled.
func newStoredElision(field, reason string, pointer retrievalPointer, omittedCount int) elidedField {
	return elidedField{field: field, reason: reason, class: classStored, pointer: pointer.String(), omittedCount: omittedCount}
}

// newOversizedCapableElision takes a DISTINCT unboundedPointer, produced only
// by pointerREST — handing it a bounded fishhawk_* MCP pointer is a COMPILE
// error, which is what makes the oversized-capable class real rather than
// decorative.
func newOversizedCapableElision(field, reason string, pointer unboundedPointer, omittedCount int) elidedField {
	return elidedField{field: field, reason: reason, class: classOversizedCapable, pointer: pointer.String(), omittedCount: omittedCount}
}

// newComputedElision takes NO pointer parameter at all, so a computed elision
// carrying a pointer cannot exist. It still carries omittedCount: how many
// items were dropped is honest metadata, not a retrieval claim.
func newComputedElision(field, reason string, omittedCount int) elidedField {
	return elidedField{field: field, reason: reason, class: classComputed, omittedCount: omittedCount}
}

// aggregateStoredElision / aggregateComputedElision are the floor's two
// entries — the explicit, marked exception to per-field itemisation.
func aggregateStoredElision(field, reason string, surfaces []string) elidedField {
	return elidedField{field: field, reason: reason, class: classStored, pointer: strings.Join(surfaces, "; "), aggregate: true}
}

func aggregateComputedElision(field, reason string) elidedField {
	return elidedField{field: field, reason: reason, class: classComputed, aggregate: true}
}

// elisionLedger is the append-only accumulator the ladder builds.
type elisionLedger struct {
	budget  int
	tier    string
	entries []elidedField
	note    string
}

func (l *elisionLedger) add(entries ...elidedField) {
	l.entries = append(l.entries, entries...)
}

// ---------------------------------------------------------------------------
// The EXPORTED wire DTO — a pure PROJECTION of the accumulator
// ---------------------------------------------------------------------------

// Elisions / ElidedField are EXPORTED with EXPORTED FIELDS on purpose, and must
// stay that way. Do NOT "simplify" them back into the unexported accumulator
// above with a custom MarshalJSON: modelcontextprotocol/go-sdk v1.7.0 validates
// the MARSHALLED tool output against the reflected output schema INSIDE the
// handler (mcp/server.go:422 applySchema(outJSON, outputResolved, true);
// mcp/server.go:424 wraps a failure as "validating tool output"), and
// google/jsonschema-go v0.4.3 both sets AdditionalProperties = falseSchema() on
// every reflected struct (jsonschema/infer.go:246) and SKIPS unexported fields
// entirely (jsonschema/util.go:434 fieldJSONInfo returns omit: true). So an
// all-unexported type with a hand-written MarshalJSON emits keys the reflected
// schema declares no properties for and forbids as additional — breaking
// fishhawk_get_run_status AT RUNTIME.
//
// TestGetRunStatus_PopulatedElisions_PassesSDKOutputSchemaValidation is the
// regression catch; counterfactual C17 (unexport the fields) is the empirical
// proof the constraint is real rather than inferred from reading SDK source.
//
// Because a composite literal can still build a malformed DTO that no
// constructor vetted, validateWireElisions runs over the PROJECTED DTO
// immediately before wire() returns and fails LOUDLY — converting
// "unrepresentable" into "unbuildable without detection", the strongest form
// available under the SDK constraint.
//
// There is deliberately NO fields_list_capped signal on this DTO. The ladder
// never caps the fields LIST: list bloat (a per-stage T7 entry explosion, say)
// falls through to the skeleton, which discards the tier ledger and builds a
// fresh fixed-size one, and then to the floor's exactly-two aggregates. A flag
// no code path can ever set is a schema surface that lies to its reader, so the
// convergence design is stated here instead of advertised as a signal.
type Elisions struct {
	Budget int           `json:"budget" jsonschema:"the EFFECTIVE byte budget the ladder honoured (post-clamp — an override below the convergence floor is clamped up, so this never claims a bound the ladder did not honour)"`
	Tier   string        `json:"tier" jsonschema:"the deepest reduction tier applied: T1..T9 (ordered least-actionable-first), skeleton (per-field itemised projection), or floor (the constant-size absolute floor)"`
	Fields []ElidedField `json:"fields,omitempty" jsonschema:"one entry per omitted field path; at the floor tier exactly two aggregate entries stand in for the per-field list"`
	Note   string        `json:"note,omitempty" jsonschema:"one-line operator note about the reduction"`
}

// ElidedField is one omitted field path and how (or whether) to get it back.
type ElidedField struct {
	Field        string `json:"field" jsonschema:"the omitted field's dotted path, e.g. run.issue_context or stages[].failure_reason"`
	Reason       string `json:"reason" jsonschema:"why it was omitted"`
	Class        string `json:"class" jsonschema:"stored (the content exists in durable storage and pointer retrieves it), oversized_capable (stored but arbitrarily large, so pointer names an UNBOUNDED REST surface), or computed (derived at read time and never stored, so no pointer can retrieve it)"`
	Pointer      string `json:"pointer,omitempty" jsonschema:"the retrieval surface, which returns AT LEAST the omitted content. Empty on a computed entry — recomputation is not retrieval"`
	OmittedCount int    `json:"omitted_count,omitempty" jsonschema:"how many items were dropped, when the omission dropped a set"`
	Aggregate    bool   `json:"aggregate,omitempty" jsonschema:"true only at the floor tier, where two aggregate entries stand in for the per-field list"`
}

// wire projects the internal accumulator into the exported DTO. It is the ONLY
// producer of Elisions / ElidedField in the package (pinned by
// TestProjectionIsSoleProducerOfWireDTO), and it validates its own output
// before returning it.
func (l *elisionLedger) wire() (*Elisions, error) {
	out := &Elisions{Budget: l.budget, Tier: l.tier, Note: l.note}
	for _, e := range l.entries {
		out.Fields = append(out.Fields, e.wireField())
	}
	if err := validateWireElisions(out); err != nil {
		return nil, err
	}
	return out, nil
}

// wireField projects one accumulated entry. Called by wire() alone.
func (e elidedField) wireField() ElidedField {
	return ElidedField{
		Field:        e.field,
		Reason:       e.reason,
		Class:        string(e.class),
		Pointer:      e.pointer,
		OmittedCount: e.omittedCount,
		Aggregate:    e.aggregate,
	}
}

const floorTierName = "floor"

// validateWireElisions fails LOUDLY on a DTO that misclassifies its own
// elisions. It never silently normalises: the returned error names the
// offending Field and the invariant it broke, and the caller surfaces it as a
// programming defect rather than shipping a DTO that lies.
func validateWireElisions(el *Elisions) error {
	if el == nil {
		return nil
	}
	for _, f := range el.Fields {
		switch elisionClass(f.Class) {
		case classComputed:
			if f.Pointer != "" {
				return fmt.Errorf("elisions: field %q is class computed but carries pointer %q — a computed value was derived at read time and never stored, so no pointer can retrieve it", f.Field, f.Pointer)
			}
		case classStored:
			if f.Pointer == "" {
				return fmt.Errorf("elisions: field %q is class stored but carries no pointer — a stored elision must name the surface that returns at least the omitted content", f.Field)
			}
		case classOversizedCapable:
			if f.Pointer == "" {
				return fmt.Errorf("elisions: field %q is class oversized_capable but carries no pointer", f.Field)
			}
			if !strings.HasPrefix(f.Pointer, "GET ") || strings.Contains(f.Pointer, "fishhawk_") {
				return fmt.Errorf("elisions: field %q is class oversized_capable but pointer %q is not an unbounded REST path — a bounded fishhawk_* tool would cap the same value again", f.Field, f.Pointer)
			}
		default:
			return fmt.Errorf("elisions: field %q carries unrecognised class %q", f.Field, f.Class)
		}
		if strings.Contains(f.Pointer, "include_") {
			return fmt.Errorf("elisions: field %q pointer %q names an include_* flag — repeating a flag is circular, because the same deterministic ladder elides the restored content again on the re-read", f.Field, f.Pointer)
		}
		if f.OmittedCount < 0 {
			return fmt.Errorf("elisions: field %q carries a negative omitted_count %d", f.Field, f.OmittedCount)
		}
		if f.Aggregate && el.Tier != floorTierName {
			return fmt.Errorf("elisions: field %q is an aggregate entry at tier %q — aggregates are the floor tier's explicit exception to per-field itemisation", f.Field, el.Tier)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// The PATH-KEYED classification table — the single source the tiers, the
// skeleton, the floor's union and the reflection pin all read.
// ---------------------------------------------------------------------------

// pathClassification is one response path's tier, class and retrieval
// surface(s). Surfaces is nil for a computed or never-elided row; when it
// returns more than one surface the FIRST is the per-field pointer and ALL of
// them fold into the floor's union.
type pathClassification struct {
	Path     string
	Tier     string
	Class    elisionClass
	Surfaces func(runID string) []retrievalPointer
	// Unbounded is set ONLY on an oversized-capable row and is the typed
	// accessor elisionForPath hands to newOversizedCapableElision. Keeping the
	// unboundedPointer type all the way from pointerREST to the constructor is
	// what preserves the compile-time refusal — a bounded fishhawk_* pointer
	// cannot reach that call site.
	Unbounded func(runID string) unboundedPointer
}

// tierNever marks a path the ladder never elides at any tier.
const tierNever = "never"

func unboundedRun(runID string) unboundedPointer { return pointerREST("/v0/runs/" + runID) }

func unboundedStages(runID string) unboundedPointer {
	return pointerREST("/v0/runs/" + runID + "/stages")
}

func restRun(runID string) []retrievalPointer {
	return []retrievalPointer{unboundedRun(runID).retrievalPointer}
}

func restStages(runID string) []retrievalPointer {
	return []retrievalPointer{unboundedStages(runID).retrievalPointer}
}

func auditAll(runID string) []retrievalPointer {
	return []retrievalPointer{pointerListAudit(runID, "", 0)}
}

func gateView(runID string) []retrievalPointer {
	return []retrievalPointer{pointerGateView(runID)}
}

// runStatusPathTable classifies EVERY response path. It is the ONE source, so a
// tier's pointer, the floor's union and the reflection pin can never disagree.
var runStatusPathTable = []pathClassification{
	// --- top level -------------------------------------------------------
	{Path: "recent_audit", Tier: "T5", Class: classStored, Surfaces: auditAll},
	{Path: "implement_reviews", Tier: "T4", Class: classStored, Surfaces: func(runID string) []retrievalPointer {
		return []retrievalPointer{
			pointerListAudit(runID, "implement_reviewed", 0),
			pointerListAudit(runID, "implement_review_skipped", 0),
		}
	}},
	{Path: "plan_review_status", Tier: "skeleton", Class: classStored, Surfaces: auditAll},
	{Path: "implement_review_status", Tier: "skeleton", Class: classStored, Surfaces: auditAll},
	{Path: "plan_stage_wait_status", Tier: tierNever},
	{Path: "implement_stage_wait_status", Tier: tierNever},
	{Path: "acceptance_stage_wait_status", Tier: tierNever},
	{Path: "budget", Tier: "T1", Class: classComputed},
	{Path: "cache_efficiency", Tier: "T1", Class: classComputed},
	{Path: "cost", Tier: "T1", Class: classComputed},
	{Path: "latency", Tier: "T1", Class: classComputed},
	{Path: "review_action_hint", Tier: "T9", Class: classComputed},
	{Path: "implement_review_merge_hint", Tier: "T9", Class: classComputed},
	{Path: "drive_status", Tier: "skeleton", Class: classStored, Surfaces: restRun},
	{Path: "drive_status.auto_advanced", Tier: "T9", Class: classStored, Surfaces: auditAll},
	{Path: "children_status", Tier: "T2", Class: classComputed},
	{Path: "security_findings", Tier: "T3", Class: classStored, Surfaces: func(runID string) []retrievalPointer {
		return []retrievalPointer{pointerListAudit(runID, "implement_security_findings", 0)}
	}},
	{Path: "elisions", Tier: tierNever},

	// --- run.* -----------------------------------------------------------
	{Path: "run.id", Tier: tierNever},
	{Path: "run.state", Tier: tierNever},
	{Path: "run.workflow_id", Tier: tierNever},
	{Path: "run.repo", Tier: "skeleton", Class: classStored, Surfaces: restRun},
	{Path: "run.workflow_sha", Tier: "skeleton", Class: classStored, Surfaces: restRun},
	{Path: "run.trigger_source", Tier: "skeleton", Class: classStored, Surfaces: restRun},
	{Path: "run.trigger_ref", Tier: "skeleton", Class: classStored, Surfaces: restRun},
	{Path: "run.parent_run_id", Tier: "skeleton", Class: classStored, Surfaces: restRun},
	{Path: "run.upstream_run_id", Tier: "skeleton", Class: classStored, Surfaces: restRun},
	{Path: "run.pull_request_url", Tier: "skeleton", Class: classStored, Surfaces: restRun},
	{Path: "run.retry_attempt", Tier: "skeleton", Class: classStored, Surfaces: restRun},
	{Path: "run.max_retries_snapshot", Tier: "skeleton", Class: classStored, Surfaces: restRun},
	{Path: "run.runner_kind", Tier: "skeleton", Class: classStored, Surfaces: restRun},
	{Path: "run.runner_kind_resolved", Tier: "skeleton", Class: classStored, Surfaces: restRun},
	{Path: "run.working_dir", Tier: "skeleton", Class: classStored, Surfaces: restRun},
	{Path: "run.predicted_runtime_minutes", Tier: "skeleton", Class: classStored, Surfaces: restRun},
	{Path: "run.created_at", Tier: "skeleton", Class: classStored, Surfaces: restRun},
	{Path: "run.updated_at", Tier: "skeleton", Class: classStored, Surfaces: restRun},
	{Path: "run.issue_context", Tier: "T8", Class: classOversizedCapable, Surfaces: restRun, Unbounded: unboundedRun},
	{Path: "run.live_validation", Tier: "T9", Class: classStored, Surfaces: restRun},
	{Path: "run.review_authority", Tier: "T9", Class: classStored, Surfaces: restRun},
	{Path: "run.concerns.open", Tier: "skeleton", Class: classStored, Surfaces: gateView},
	{Path: "run.concerns.by_state", Tier: "skeleton", Class: classStored, Surfaces: gateView},
	{Path: "run.concerns.items", Tier: "T9", Class: classStored, Surfaces: gateView},

	// --- stages[].* ------------------------------------------------------
	{Path: "stages[].type", Tier: tierNever},
	{Path: "stages[].state", Tier: tierNever},
	{Path: "stages[].failure_category", Tier: tierNever},
	{Path: "stages[].failure_reason", Tier: "T7", Class: classOversizedCapable, Surfaces: restStages, Unbounded: unboundedStages},
	{Path: "stages[].id", Tier: "skeleton", Class: classStored, Surfaces: restStages},
	{Path: "stages[].run_id", Tier: "skeleton", Class: classStored, Surfaces: restStages},
	{Path: "stages[].sequence", Tier: "skeleton", Class: classStored, Surfaces: restStages},
	{Path: "stages[].executor", Tier: "skeleton", Class: classStored, Surfaces: restStages},
	{Path: "stages[].started_at", Tier: "skeleton", Class: classStored, Surfaces: restStages},
	{Path: "stages[].ended_at", Tier: "skeleton", Class: classStored, Surfaces: restStages},
	{Path: "stages[].created_at", Tier: "skeleton", Class: classStored, Surfaces: restStages},
	{Path: "stages[].updated_at", Tier: "skeleton", Class: classStored, Surfaces: restStages},

	// --- next_actions.* --------------------------------------------------
	{Path: "next_actions.state", Tier: tierNever},
	{Path: "next_actions.actions", Tier: "T9", Class: classComputed},
}

// pathClassificationFor looks a path up in the table.
func pathClassificationFor(path string) (pathClassification, bool) {
	for _, row := range runStatusPathTable {
		if row.Path == path {
			return row, true
		}
	}
	return pathClassification{}, false
}

// floorStoredSurfaceUnion folds EVERY stored / oversized-capable row of the
// table into a de-duplicated, deterministically sorted surface set. Because the
// fold reads the same table the tiers read, the floor's aggregate necessarily
// names every surface its members would have named individually — including the
// fishhawk_get_gate_view surface T9's run.concerns pointer uses — and
// automatically absorbs any surface a FUTURE tier introduces.
func floorStoredSurfaceUnion(runID string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, row := range runStatusPathTable {
		if row.Surfaces == nil {
			continue
		}
		if row.Class != classStored && row.Class != classOversizedCapable {
			continue
		}
		for _, p := range row.Surfaces(runID) {
			if _, dup := seen[p.String()]; dup {
				continue
			}
			seen[p.String()] = struct{}{}
			out = append(out, p.String())
		}
	}
	sort.Strings(out)
	return out
}

// primarySurface returns a row's per-field pointer (its first surface).
func (p pathClassification) primarySurface(runID string) (retrievalPointer, bool) {
	if p.Surfaces == nil {
		return retrievalPointer{}, false
	}
	s := p.Surfaces(runID)
	if len(s) == 0 {
		return retrievalPointer{}, false
	}
	return s[0], true
}

// elisionForPath builds the per-field entry for a table row, routed by class so
// the entry cannot disagree with its own classification.
func elisionForPath(row pathClassification, runID, reason string, omittedCount int) elidedField {
	switch row.Class {
	case classComputed:
		return newComputedElision(row.Path, reason, omittedCount)
	case classOversizedCapable:
		return newOversizedCapableElision(row.Path, reason, row.Unbounded(runID), omittedCount)
	default:
		ptr, _ := row.primarySurface(runID)
		return newStoredElision(row.Path, reason, ptr, omittedCount)
	}
}

// mustPath panics on an unclassified path. Every literal path in the ladder is
// a table key, and TestEveryResponsePathIsClassified pins the table against the
// response types, so a miss is a programming defect, not a runtime condition.
func mustPath(path string) pathClassification {
	row, ok := pathClassificationFor(path)
	if !ok {
		panic("mcpserver: unclassified response path " + path)
	}
	return row
}

func classified(path, runID, reason string, omittedCount int) elidedField {
	return elisionForPath(mustPath(path), runID, reason, omittedCount)
}

// ---------------------------------------------------------------------------
// The tier ladder
// ---------------------------------------------------------------------------

const (
	// recentAuditTierCap is the newest-N window T5 keeps.
	recentAuditTierCap = 5
	// failureReasonTierCap bounds a stage failure_reason's ENCODED size at T7.
	failureReasonTierCap = 512
	// nextActionsTierCap bounds the next_actions action list at T9.
	nextActionsTierCap = 3
	// skeletonFailureReasonCap bounds the skeleton's retained failure_reason.
	skeletonFailureReasonCap = 256
	// floorFieldCap bounds every string the constant-size floor emits.
	floorFieldCap = 96
	// floorNextActionsState is the floor's next_actions presence marker.
	floorNextActionsState = "elided_see_elisions"
)

// runStatusTier is one ordered reduction step.
type runStatusTier struct {
	name  string
	apply func(out *GetRunStatusOutput, runID string, led *elisionLedger)
}

// runStatusTiers is the FIXED least-actionable-first order. A tier whose target
// is already absent records nothing. None of T1..T9 touches run.id, run.state,
// any stage's state or failure_category, or next_actions presence.
var runStatusTiers = []runStatusTier{
	{name: "T1", apply: tierDerivedEconomics},
	{name: "T2", apply: tierChildrenDetail},
	{name: "T3", apply: tierSecurityFindings},
	{name: "T4", apply: tierImplementReviews},
	{name: "T5", apply: tierRecentAuditCap},
	{name: "T6", apply: tierRecentAuditDrop},
	{name: "T7", apply: tierFailureReasonCap},
	{name: "T8", apply: tierIssueContext},
	{name: "T9", apply: tierResidualBounded},
}

func tierDerivedEconomics(out *GetRunStatusOutput, runID string, led *elisionLedger) {
	const why = "recomputed at read time from the cost ledger or the audit-chain timestamps; never stored, so no pointer can retrieve it"
	if out.Cost != nil {
		out.Cost = nil
		led.add(classified("cost", runID, why, 0))
	}
	if out.CacheEfficiency != nil {
		out.CacheEfficiency = nil
		led.add(classified("cache_efficiency", runID, why, 0))
	}
	if out.Latency != nil {
		out.Latency = nil
		led.add(classified("latency", runID, why, 0))
	}
	if out.Budget != nil {
		out.Budget = nil
		led.add(classified("budget", runID, why, 0))
	}
}

func tierChildrenDetail(out *GetRunStatusOutput, runID string, led *elisionLedger) {
	if out.ChildrenStatus == nil || len(out.ChildrenStatus.Children) == 0 {
		return
	}
	n := len(out.ChildrenStatus.Children)
	cs := *out.ChildrenStatus
	cs.Children = nil
	out.ChildrenStatus = &cs
	led.add(classified("children_status", runID,
		"per-child detail dropped (integration_phase and the counts are retained); each child's state is re-derived from a live per-child read, never stored on this run", n))
}

func tierSecurityFindings(out *GetRunStatusOutput, runID string, led *elisionLedger) {
	if len(out.SecurityFindings) == 0 {
		return
	}
	n := len(out.SecurityFindings)
	out.SecurityFindings = nil
	led.add(classified("security_findings", runID,
		"the full findings list is stored on the newest implement_security_findings audit entry", n))
}

func tierImplementReviews(out *GetRunStatusOutput, runID string, led *elisionLedger) {
	if len(out.ImplementReviews) == 0 {
		return
	}
	n := len(out.ImplementReviews)
	out.ImplementReviews = nil
	row := mustPath("implement_reviews")
	surfaces := row.Surfaces(runID)
	// TWO entries, one per originating category — the two loadImplementReviews
	// unions — each with its own category-exact anchored pointer.
	led.add(
		newStoredElision("implement_reviews", "the implement_reviewed verdicts are stored on the audit chain", surfaces[0], n),
		newStoredElision("implement_reviews", "the implement_review_skipped rows are stored on the audit chain", surfaces[1], n),
	)
}

func tierRecentAuditCap(out *GetRunStatusOutput, runID string, led *elisionLedger) {
	if len(out.RecentAudit) <= recentAuditTierCap {
		return
	}
	dropped := len(out.RecentAudit) - recentAuditTierCap
	out.RecentAudit = out.RecentAudit[:recentAuditTierCap]
	led.add(classified("recent_audit", runID,
		fmt.Sprintf("capped to the newest %d entries; the %d dropped entries are reachable from the start of the audit chain (since_sequence=0) with next_cursor pagination", recentAuditTierCap, dropped), dropped))
}

func tierRecentAuditDrop(out *GetRunStatusOutput, runID string, led *elisionLedger) {
	if len(out.RecentAudit) == 0 {
		return
	}
	dropped := len(out.RecentAudit)
	out.RecentAudit = nil
	led.add(classified("recent_audit", runID,
		"the recent-audit window is dropped entirely; the whole chain is stored and reachable from since_sequence=0", dropped))
}

func tierFailureReasonCap(out *GetRunStatusOutput, runID string, led *elisionLedger) {
	for i := range out.Stages {
		fr := out.Stages[i].FailureReason
		if fr == nil {
			continue
		}
		before := jsonEncodedLen(*fr)
		if before <= failureReasonTierCap {
			continue
		}
		capped := capJSONString(*fr, failureReasonTierCap)
		out.Stages[i].FailureReason = &capped
		led.add(elisionForPath(mustPath("stages[].failure_reason"), runID,
			fmt.Sprintf("stage %d failure_reason capped to %d encoded bytes (%d elided); the untruncated value is stored on the stage row", i, failureReasonTierCap, before-jsonEncodedLen(capped)),
			before-jsonEncodedLen(capped)))
	}
}

func tierIssueContext(out *GetRunStatusOutput, runID string, led *elisionLedger) {
	if out.Run.IssueContext == nil {
		return
	}
	out.Run.IssueContext = nil
	led.add(classified("run.issue_context", runID,
		"the issue body and comments can be arbitrarily large; the full context is stored on the run row and returned by the unbounded REST read", 0))
}

func tierResidualBounded(out *GetRunStatusOutput, runID string, led *elisionLedger) {
	if out.Run.Concerns != nil && len(out.Run.Concerns.Items) > 0 {
		n := len(out.Run.Concerns.Items)
		c := *out.Run.Concerns
		c.Items = nil
		out.Run.Concerns = &c
		led.add(classified("run.concerns.items", runID,
			"the open-concern items are dropped (open + by_state are retained); the full items with notes are stored and returned by the gate view", n))
	}
	if len(out.Run.ReviewAuthority) > 0 {
		n := len(out.Run.ReviewAuthority)
		out.Run.ReviewAuthority = nil
		led.add(classified("run.review_authority", runID,
			"the per-stage resolved review authority is stored on the run row", n))
	}
	if out.Run.LiveValidation != nil {
		out.Run.LiveValidation = nil
		led.add(classified("run.live_validation", runID,
			"the pending live-validation walk is stored on the run row", 0))
	}
	if out.DriveStatus != nil && len(out.DriveStatus.AutoAdvanced) > 0 {
		n := len(out.DriveStatus.AutoAdvanced)
		ds := *out.DriveStatus
		ds.AutoAdvanced = nil
		out.DriveStatus = &ds
		led.add(classified("drive_status.auto_advanced", runID,
			"the auto-advanced transition list is stored on the audit chain", n))
	}
	if out.ReviewActionHint != nil {
		out.ReviewActionHint = nil
		led.add(classified("review_action_hint", runID,
			"a display-only pointer derived at read time from the review status and the fix-up budget; never stored", 0))
	}
	if out.ImplementReviewMergeHint != "" {
		out.ImplementReviewMergeHint = ""
		led.add(classified("implement_review_merge_hint", runID,
			"a display-only warning derived at read time from the implement review status; never stored", 0))
	}
	if out.NextActions != nil && len(out.NextActions.Actions) > nextActionsTierCap {
		dropped := len(out.NextActions.Actions) - nextActionsTierCap
		na := *out.NextActions
		na.Actions = na.Actions[:nextActionsTierCap]
		out.NextActions = &na
		led.add(classified("next_actions.actions", runID,
			fmt.Sprintf("the action list is capped to the first %d (the suggested default first); %d dropped. The block is a pure function of the run snapshot — recomputed at read time, never stored", nextActionsTierCap, dropped), dropped))
	}
}

// boundRunStatusOutput bounds the marshalled response to budget BY
// CONSTRUCTION. It runs at ONE call site in getRunStatus, after every read and
// helper computation and immediately before the handler returns.
//
// The bound is a MEASURED re-check after each tier with the in-progress
// elisions block ATTACHED — never arithmetic — so the block's own bytes are
// inside every measurement and can never push a just-under response back over.
// An under-budget response is returned UNCHANGED with no elisions block, so it
// is byte-identical to the pre-#2508 wire.
//
// The ladder is deliberately NOT size-monotonic: attaching the growing elisions
// block can make a later measurement larger than an earlier one. Convergence
// comes from the constant-size floor, not from monotonicity.
func boundRunStatusOutput(out GetRunStatusOutput, runID string, budget int) (GetRunStatusOutput, error) {
	n, err := marshalledLen(out)
	if err != nil {
		return out, err
	}
	if n <= budget {
		return out, nil
	}

	led := &elisionLedger{budget: budget, note: "response reduced to fit the tool-result byte budget"}
	for _, tier := range runStatusTiers {
		led.tier = tier.name
		tier.apply(&out, runID, led)
		fits, err := attachAndMeasure(&out, led, budget)
		if err != nil {
			return out, err
		}
		if fits {
			return out, nil
		}
	}

	sk, fits, err := skeletonRunStatus(out, runID, budget)
	if err != nil {
		return out, err
	}
	if fits {
		return sk, nil
	}

	return minimalRunStatus(out, runID, budget)
}

// attachAndMeasure attaches the in-progress projected block and re-marshals the
// WHOLE output — the measured re-check.
func attachAndMeasure(out *GetRunStatusOutput, led *elisionLedger, budget int) (bool, error) {
	w, err := led.wire()
	if err != nil {
		return false, err
	}
	out.Elisions = w
	n, err := marshalledLen(*out)
	if err != nil {
		return false, err
	}
	return n <= budget, nil
}

// ---------------------------------------------------------------------------
// The skeleton tier — per-field itemisation, NESTED-aware
// ---------------------------------------------------------------------------

// skeletonRetainedPaths is the FIXED retained field set. Every other path in
// runStatusPathTable is itemised as its own omission — including omissions
// NESTED inside a RETAINED composite (run.issue_context, run.concerns.items,
// stages[].executor, next_actions.actions, ...), which a top-level-only diff
// could not express.
var skeletonRetainedPaths = map[string]struct{}{
	"run.id":                       {},
	"run.state":                    {},
	"run.workflow_id":              {},
	"stages[].type":                {},
	"stages[].state":               {},
	"stages[].failure_category":    {},
	"stages[].failure_reason":      {},
	"plan_stage_wait_status":       {},
	"implement_stage_wait_status":  {},
	"acceptance_stage_wait_status": {},
	"next_actions.state":           {},
	"elisions":                     {},
}

// skeletonRunStatus projects to the retained field set and itemises every
// omitted PATH from the table. Diagnosis OUTRANKS itemisation at and below this
// tier: an operator reaches here precisely when something went badly wrong.
func skeletonRunStatus(out GetRunStatusOutput, runID string, budget int) (GetRunStatusOutput, bool, error) {
	sk := GetRunStatusOutput{
		Run: Run{
			ID:         out.Run.ID,
			State:      out.Run.State,
			WorkflowID: out.Run.WorkflowID,
		},
		PlanStageWaitStatus:       out.PlanStageWaitStatus,
		ImplementStageWaitStatus:  out.ImplementStageWaitStatus,
		AcceptanceStageWaitStatus: out.AcceptanceStageWaitStatus,
	}
	for _, s := range out.Stages {
		row := Stage{Type: s.Type, State: s.State, FailureCategory: s.FailureCategory}
		if s.FailureReason != nil {
			capped := capJSONString(*s.FailureReason, skeletonFailureReasonCap)
			row.FailureReason = &capped
		}
		sk.Stages = append(sk.Stages, row)
	}
	if out.NextActions != nil {
		sk.NextActions = &NextActions{State: out.NextActions.State}
	}

	led := &elisionLedger{
		budget: budget,
		tier:   "skeleton",
		note:   "reduced to the diagnosis skeleton; every omission below is itemised with its own class and retrieval surface",
	}
	for _, row := range runStatusPathTable {
		if row.Tier == tierNever {
			continue
		}
		if _, retained := skeletonRetainedPaths[row.Path]; retained {
			continue
		}
		led.add(elisionForPath(row, runID, "omitted by the diagnosis skeleton (tier "+row.Tier+")", 0))
	}

	fits, err := attachAndMeasure(&sk, led, budget)
	if err != nil {
		return sk, false, err
	}
	return sk, fits, nil
}

// ---------------------------------------------------------------------------
// The absolute floor
// ---------------------------------------------------------------------------

// minimalRunStatus returns a CONSTANT-SIZE output pinned under
// minimalRunStatusMaxBytes for ANY input: run id + state, one stage row for the
// failing stage, a next_actions presence marker, and EXACTLY TWO aggregate
// elisions — one STORED whose pointer names the UNION of every surface its
// members would have named individually (derived from runStatusPathTable, so it
// cannot drift from the tiers), one COMPUTED carrying NO pointer.
func minimalRunStatus(out GetRunStatusOutput, runID string, budget int) (GetRunStatusOutput, error) {
	failingType, failingState, failureCategory := diagnosisCore(out)
	return minimalRunStatusFrom(runID, out.Run.State, failingType, failingState, failureCategory, out.NextActions != nil, budget)
}

// diagnosisCore picks the stage an operator needs: the first failed stage, else
// the last stage.
func diagnosisCore(out GetRunStatusOutput) (stageType, stageState, failureCategory string) {
	if len(out.Stages) == 0 {
		return "", "", ""
	}
	pick := out.Stages[len(out.Stages)-1]
	for _, s := range out.Stages {
		if s.State == "failed" {
			pick = s
			break
		}
	}
	fc := ""
	if pick.FailureCategory != nil {
		fc = *pick.FailureCategory
	}
	return pick.Type, pick.State, fc
}

func minimalRunStatusFrom(runID, runState, failingStageType, failingStageState, failureCategory string, hasNextActions bool, budget int) (GetRunStatusOutput, error) {
	fc := capJSONString(failureCategory, floorFieldCap)
	sk := GetRunStatusOutput{
		Run: Run{
			ID:    capJSONString(runID, floorFieldCap),
			State: capJSONString(runState, floorFieldCap),
		},
	}
	if failingStageType != "" || failingStageState != "" || fc != "" {
		stage := Stage{
			Type:  capJSONString(failingStageType, floorFieldCap),
			State: capJSONString(failingStageState, floorFieldCap),
		}
		if fc != "" {
			stage.FailureCategory = &fc
		}
		sk.Stages = []Stage{stage}
	}
	if hasNextActions {
		sk.NextActions = &NextActions{State: floorNextActionsState}
	}

	led := &elisionLedger{
		budget: budget,
		tier:   floorTierName,
		note:   "reduced to the constant-size floor; the two entries below are AGGREGATES — the floor's explicit exception to per-field itemisation",
	}
	led.add(
		aggregateStoredElision("*",
			"every stored and oversized-capable field of the response was omitted; the union of retrieval surfaces below returns at least the omitted content",
			floorStoredSurfaceUnion(runID)),
		aggregateComputedElision("*",
			"every derived field of the response was omitted; these values were computed at read time from the cost ledger, the audit-chain timestamps and the run snapshot, and were never stored — so no pointer can retrieve them"),
	)
	w, err := led.wire()
	if err != nil {
		return sk, err
	}
	sk.Elisions = w
	return sk, nil
}

// ---------------------------------------------------------------------------
// The nested-aware reflection pin's walk targets
// ---------------------------------------------------------------------------

// skeletonRetainedComposites maps a dotted path to the struct type whose OWN
// fields must be classified. The pin recurses into these and requires every
// other field path to appear in runStatusPathTable — so a new NESTED field (the
// commonest real case: Run gains a mirrored backend field, as LiveValidation
// and ReviewAuthority both recently did) cannot silently bypass the budget any
// more than a new top-level field can.
//
// The walk is only as deep as this map: a new nested composite PROMOTED into
// the retained set needs adding here. A new field on a type that is already
// wholly elided by a tier needs no entry, which is correct.
func retainedCompositeTypes() map[string]reflect.Type {
	return map[string]reflect.Type{
		"run":          reflect.TypeOf(Run{}),
		"stages[]":     reflect.TypeOf(Stage{}),
		"next_actions": reflect.TypeOf(NextActions{}),
		"run.concerns": reflect.TypeOf(RunConcerns{}),
	}
}

// rootPathAlias renames a root field whose nested paths use a slice marker.
var rootPathAlias = map[string]string{"stages": "stages[]"}

// classifiedResponsePaths walks GetRunStatusOutput and, recursively, the
// retained composites, returning every LEAF path the response can carry. The
// reflection pin diffs this against runStatusPathTable.
func classifiedResponsePaths() []string {
	composites := retainedCompositeTypes()
	var out []string
	var walk func(prefix string, t reflect.Type)
	walk = func(prefix string, t reflect.Type) {
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if f.PkgPath != "" {
				continue // unexported: never on the wire
			}
			name := wirePathFieldName(f)
			if name == "" {
				continue
			}
			path := name
			if prefix != "" {
				path = prefix + "." + name
			}
			if prefix == "" {
				if alias, ok := rootPathAlias[path]; ok {
					path = alias
				}
			}
			if child, ok := composites[path]; ok {
				walk(path, child)
				continue
			}
			out = append(out, path)
		}
	}
	walk("", reflect.TypeOf(GetRunStatusOutput{}))
	sort.Strings(out)
	return out
}

// wirePathFieldName reads a struct field's wire name, honouring `json:"-"`.
func wirePathFieldName(f reflect.StructField) string {
	tag := f.Tag.Get("json")
	if tag == "-" {
		return ""
	}
	if idx := strings.Index(tag, ","); idx >= 0 {
		tag = tag[:idx]
	}
	if tag == "" {
		return f.Name
	}
	return tag
}
