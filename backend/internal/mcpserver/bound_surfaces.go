package mcpserver

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"
)

// This file extends the ADR-077 / #2508 response byte bound from
// fishhawk_get_run_status to the two remaining unbounded MCP surfaces (#2510),
// reusing bound.go's surface-agnostic primitives (jsonEncodedLen, capJSONString,
// the constructor-only elision ledger, the exported wire DTO) rather than
// reimplementing them:
//
//	(1) fishhawk_list_audit — advertises limit up to 200, and limit=55 already
//	    returned 54,262 chars and HARD-FAILED the client.
//	(2) EVERY verb that returns a run row — a single row embedding a large
//	    issue_context can exceed the limit on its own (run 143aea12 = 79,131
//	    bytes), and one of those verbs is a MUTATING verb (cancel_run) whose
//	    server-side effect had ALREADY SUCCEEDED when the response failed to
//	    render. The bound is what decouples a mutating verb's success signal
//	    from whether its body fits.
//
// Both ladders share the get_run_status contract: an under-budget response is
// returned UNCHANGED with no elisions block (byte-identical to today's wire),
// each tier is followed by a MEASURED re-check with the in-progress elisions
// block ATTACHED (never arithmetic, so the block's own bytes are inside every
// measurement), and convergence comes from a constant-size floor rather than
// from monotonicity.

const (
	// issueBodyTierCap bounds an issue body's ENCODED size at the run-row
	// ladder's R2 tier.
	issueBodyTierCap = 512
	// auditPayloadTierCap bounds a single audit-payload string value's ENCODED
	// size at the list-audit ladder's A1 tier. It is applied via capJSONString,
	// NOT compact.go's raw-byte truncateOversizedString, which is not a size
	// bound under escaping (a body of '<' bytes inflates 6x).
	auditPayloadTierCap = 256
)

// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------

// unboundedRuns names the UNBOUNDED REST run ENUMERATION — the surface a
// truncated fishhawk_list_runs page points at. It must never be spelled with an
// include_* flag: validateWireElisions rejects those as circular.
func unboundedRuns() unboundedPointer { return pointerREST("/v0/runs") }

// unboundedRunAudit names the UNBOUNDED REST audit walk for one run — the
// surface a capped audit payload points at. Re-calling the now-bounded
// fishhawk_list_audit would cap the same value again, which is exactly the
// circularity the oversized_capable class exists to refuse.
func unboundedRunAudit(runID string) unboundedPointer {
	return pointerREST("/v0/runs/" + runID + "/audit")
}

// attachAndMeasureOut is the generic form of bound.go's attachAndMeasure: it
// attaches the in-progress projected block via the caller's setter and
// re-marshals the WHOLE output — the measured re-check.
func attachAndMeasureOut[T any](out *T, led *elisionLedger, budget int, set func(*T, *Elisions)) (bool, error) {
	w, err := led.wire()
	if err != nil {
		return false, err
	}
	set(out, w)
	n, err := marshalledLen(*out)
	if err != nil {
		return false, err
	}
	return n <= budget, nil
}

// ---------------------------------------------------------------------------
// The shared RUN-ROW ladder
// ---------------------------------------------------------------------------

// The wired verbs are get_active_run, start_run, cancel_run, list_runs,
// start_campaign_item_run, revive_run and resume_run — EVERY verb whose
// response embeds a Run. fishhawk_drive_run deliberately needs NO wiring, and
// the reason is structural rather than an oversight: DriveRunOutput carries
// run_id and run_state only, never an embedded Run row, so it cannot inherit
// the issue_context defect. That is the machine-checkable form of "every verb
// that returns a run row" — the criterion is "does the response type embed
// Run", not an enumerated list a future verb can silently fall off.
//
// runRowCtx carries what a run-row tier needs to name its own omission: the
// dotted field prefix ("run" for a single-row verb, "items[]" for the
// enumeration), the response-level run id used when a row carries none, and
// whether this is a PAGE — a page's pointers name the enumeration rather than
// any one row's issue or gate view.
type runRowCtx struct {
	prefix string
	runID  string
	page   bool
}

func (c runRowCtx) path(sub string) string { return c.prefix + "." + sub }

func (c runRowCtx) rowID(id string) string {
	if id != "" {
		return id
	}
	return c.runID
}

// issuePointer picks the most actionable UNBOUNDED surface for an elided
// issue_context: the issue itself when this is a single row carrying a usable
// URL, the run's REST read when the URL is unusable (pointerIssueURL's guard),
// and the run ENUMERATION for a page — where no single issue URL speaks for
// every dropped row.
func (c runRowCtx) issuePointer(url, id string) unboundedPointer {
	if c.page {
		return unboundedRuns()
	}
	if p, ok := pointerIssueURL(url); ok {
		return p
	}
	return unboundedRun(c.rowID(id))
}

// concernsPointer is the stored-class surface for dropped concern items: the
// one-call gate view for a single row, the enumeration for a page.
func (c runRowCtx) concernsPointer(id string) retrievalPointer {
	if c.page {
		return unboundedRuns().retrievalPointer
	}
	return pointerGateView(c.rowID(id))
}

// runRowTier is one ordered reduction step over EVERY row of the response. A
// tier records ONE ledger entry covering every row it touched (with
// omitted_count summed), never one per row: a 200-row page would otherwise
// answer bloat with more bloat.
type runRowTier struct {
	name  string
	apply func(rows []*Run, c runRowCtx, led *elisionLedger)
}

// runRowTiers is the FIXED least-actionable-first order. None of R1..R4 touches
// a row's id, repo, workflow_id, state, runner_kind, pull_request_url or
// parent_run_id — the diagnosis core the floor also keeps. issue_context's
// title / url / number / labels survive R1 and R2 because they are small and
// are what makes the pointer actionable.
var runRowTiers = []runRowTier{
	{name: "R1", apply: runRowTierIssueComments},
	{name: "R2", apply: runRowTierIssueBody},
	{name: "R3", apply: runRowTierIssueContext},
	{name: "R4", apply: runRowTierConcernItems},
}

func runRowTierIssueComments(rows []*Run, c runRowCtx, led *elisionLedger) {
	total := 0
	var url, id string
	for _, run := range rows {
		if run.IssueContext == nil || len(run.IssueContext.Comments) == 0 {
			continue
		}
		if total == 0 {
			url, id = run.IssueContext.URL, run.ID
		}
		total += len(run.IssueContext.Comments)
		ic := *run.IssueContext
		ic.Comments = nil
		run.IssueContext = &ic
	}
	if total == 0 {
		return
	}
	led.add(newOversizedCapableElision(c.path("issue_context.comments"),
		"the issue's comment thread is dropped (title, url, number and labels are retained so the pointer stays actionable); the thread is stored on the run row and lives in full at the issue itself",
		c.issuePointer(url, id), total))
}

func runRowTierIssueBody(rows []*Run, c runRowCtx, led *elisionLedger) {
	elided := 0
	var url, id string
	for _, run := range rows {
		if run.IssueContext == nil {
			continue
		}
		before := jsonEncodedLen(run.IssueContext.Body)
		if before <= issueBodyTierCap {
			continue
		}
		if elided == 0 {
			url, id = run.IssueContext.URL, run.ID
		}
		capped := capJSONString(run.IssueContext.Body, issueBodyTierCap)
		ic := *run.IssueContext
		ic.Body = capped
		run.IssueContext = &ic
		elided += before - jsonEncodedLen(capped)
	}
	if elided == 0 {
		return
	}
	led.add(newOversizedCapableElision(c.path("issue_context.body"),
		fmt.Sprintf("the issue body is capped to %d encoded bytes (%d elided); the untruncated body is stored on the run row and lives in full at the issue itself", issueBodyTierCap, elided),
		c.issuePointer(url, id), elided))
}

func runRowTierIssueContext(rows []*Run, c runRowCtx, led *elisionLedger) {
	total := 0
	var url, id string
	for _, run := range rows {
		if run.IssueContext == nil {
			continue
		}
		if total == 0 {
			url, id = run.IssueContext.URL, run.ID
		}
		total++
		run.IssueContext = nil
	}
	if total == 0 {
		return
	}
	led.add(newOversizedCapableElision(c.path("issue_context"),
		"the issue context is dropped entirely; the issue body and comments can be arbitrarily large. The full context is stored on the run row and returned by the unbounded REST read",
		c.issuePointer(url, id), total))
}

func runRowTierConcernItems(rows []*Run, c runRowCtx, led *elisionLedger) {
	total := 0
	var id string
	for _, run := range rows {
		if run.Concerns == nil || len(run.Concerns.Items) == 0 {
			continue
		}
		if total == 0 {
			id = run.ID
		}
		total += len(run.Concerns.Items)
		cs := *run.Concerns
		cs.Items = nil
		run.Concerns = &cs
	}
	if total == 0 {
		return
	}
	led.add(newStoredElision(c.path("concerns.items"),
		"the open-concern items are dropped (open + by_state are retained); the full items with notes are stored and returned by the gate view",
		c.concernsPointer(id), total))
}

// runRowDiagnosisCore projects a row to the fields an operator needs to act on
// a response that reached the floor: which run, where, which workflow, what
// state, which runner, its PR and its lineage. Every retained string is capped
// via capJSONString, so the floor is constant-size for ANY input — including a
// row whose repo / workflow_id / pull_request_url are pathologically long,
// which is the case the floor guarantee actually rests on.
func runRowDiagnosisCore(run Run) Run {
	core := Run{
		ID:         capJSONString(run.ID, floorFieldCap),
		Repo:       capJSONString(run.Repo, floorFieldCap),
		WorkflowID: capJSONString(run.WorkflowID, floorFieldCap),
		State:      capJSONString(run.State, floorFieldCap),
		RunnerKind: capJSONString(run.RunnerKind, floorFieldCap),
	}
	if run.PullRequestURL != nil {
		pr := capJSONString(*run.PullRequestURL, floorFieldCap)
		core.PullRequestURL = &pr
	}
	if run.ParentRunID != nil {
		parent := capJSONString(*run.ParentRunID, floorFieldCap)
		core.ParentRunID = &parent
	}
	return core
}

// runRowFloorLedger builds the floor's aggregate entries — the floor tier's
// explicit exception to per-field itemisation.
func runRowFloorLedger(c runRowCtx, budget int, id string) *elisionLedger {
	surfaces := []string{unboundedRuns().String()}
	if !c.page {
		surfaces = []string{unboundedRun(c.rowID(id)).String(), pointerGateView(c.rowID(id)).String()}
	}
	led := &elisionLedger{
		budget: budget,
		tier:   floorTierName,
		note:   "reduced to the constant-size floor: the run diagnosis core, plus any response field the schema forbids omitting (carried, with strings capped). The entry below is an AGGREGATE — the floor tier's explicit exception to per-field itemisation",
	}
	led.add(aggregateStoredElision("*",
		"every OMITTABLE field of the response outside the run diagnosis core (id, repo, workflow_id, state, runner_kind, pull_request_url, parent_run_id) was omitted; the union of retrieval surfaces below returns at least the omitted content. A field whose schema has no omitempty is NOT omitted — it is carried with its real value, its strings capped and its collections truncated, because emitting the zero value of such a field would fabricate a reading rather than withhold one",
		surfaces))
	return led
}

// ---------------------------------------------------------------------------
// The floor's TYPE-AWARE carry-through (#2510 fix-up)
// ---------------------------------------------------------------------------

// floorCarryCollectionCap bounds how many elements of a CARRIED non-omitempty
// collection the floor keeps, so carrying stays constant-size.
const floorCarryCollectionCap = 2

// carryNonOmitemptyFields is what stops the floor FALSIFYING data while
// claiming it omitted it.
//
// Building the floor from the ZERO value of T is honest only for a field whose
// schema carries omitempty: that field's zero value renders as ABSENT, which is
// exactly what the aggregate elision says happened. A field WITHOUT omitempty
// is emitted regardless, so zeroing it does not omit it — it publishes a wrong
// value that reads as real. ResumeRunOutput.Idempotent (`json:"idempotent"`)
// would render "idempotent": false on a call that genuinely REPLAYED, and an
// operator acting on that could mint a duplicate recovery run;
// StartCampaignItemRunOutput.Item (`json:"item"`) would render an all-empty but
// populated-LOOKING CampaignItem, losing the run↔item linkage.
//
// Of the two remedies the concern admits — preserve the true value, or make the
// floor projection type-aware — this takes the SECOND, which subsumes the
// first: the projection is driven by the json tag rather than by an enumerated
// field list, so a field a FUTURE verb adds without omitempty is carried
// automatically instead of silently joining the falsified set. The constant-size
// guarantee survives because carrying is a BOUNDED projection (floorCarryValue:
// every string capped at floorFieldCap, every collection truncated to
// floorCarryCollectionCap), not a verbatim copy.
//
// Fields the ladder itself owns are skipped: the Run row(s), which the caller
// replaces with the diagnosis core, and the *Elisions block, which set installs.
func carryNonOmitemptyFields[T any](dst, src *T) {
	dv := reflect.ValueOf(dst).Elem()
	sv := reflect.ValueOf(src).Elem()
	t := dv.Type()
	if t.Kind() != reflect.Struct {
		return
	}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" {
			continue // unexported: never on the wire
		}
		if jsonTagOmits(f) {
			continue
		}
		if ladderOwnedFloorField(f.Type) {
			continue
		}
		dv.Field(i).Set(floorCarryValue(sv.Field(i)))
	}
}

// jsonTagOmits reports whether a field's zero value is ABSENT on the wire —
// either because the field is skipped outright (`json:"-"`) or because it
// carries omitempty. Those are the fields the zero-value floor may honestly
// leave alone.
func jsonTagOmits(f reflect.StructField) bool {
	parts := strings.Split(f.Tag.Get("json"), ",")
	if parts[0] == "-" && len(parts) == 1 {
		return true
	}
	for _, opt := range parts[1:] {
		if opt == "omitempty" {
			return true
		}
	}
	return false
}

// ladderOwnedFloorField names the field types the run-row ladder installs
// itself, so the carry-through must not overwrite them.
func ladderOwnedFloorField(t reflect.Type) bool {
	switch t {
	case reflect.TypeOf(Run{}), reflect.TypeOf([]Run(nil)), reflect.TypeOf((*Elisions)(nil)):
		return true
	}
	return false
}

// floorCarryValue returns a CONSTANT-SIZE-BOUNDED copy of v: strings capped at
// floorFieldCap, collections truncated to floorCarryCollectionCap, composites
// projected recursively. A struct with unexported fields (time.Time being the
// one that actually occurs) is copied verbatim — reflect cannot rebuild it, and
// every such type here encodes to a fixed-width form anyway.
func floorCarryValue(v reflect.Value) reflect.Value {
	switch v.Kind() {
	case reflect.String:
		return reflect.ValueOf(capJSONString(v.String(), floorFieldCap)).Convert(v.Type())
	case reflect.Pointer:
		if v.IsNil() {
			return v
		}
		out := reflect.New(v.Type().Elem())
		out.Elem().Set(floorCarryValue(v.Elem()))
		return out
	case reflect.Interface:
		if v.IsNil() {
			return v
		}
		out := reflect.New(v.Type())
		out.Elem().Set(floorCarryValue(v.Elem()))
		return out.Elem()
	case reflect.Struct:
		return floorCarryStruct(v)
	case reflect.Slice:
		if v.IsNil() {
			return v
		}
		return floorCarrySlice(v)
	case reflect.Array:
		return v
	case reflect.Map:
		if v.IsNil() {
			return v
		}
		return floorCarryMap(v)
	default:
		// Bool and every numeric kind: constant-size already, and their zero
		// value is precisely the falsified reading this function exists to
		// prevent, so they are carried verbatim.
		return v
	}
}

func floorCarryStruct(v reflect.Value) reflect.Value {
	t := v.Type()
	if t == reflect.TypeOf(time.Time{}) || hasUnexportedField(t) {
		return v
	}
	out := reflect.New(t).Elem()
	for i := 0; i < t.NumField(); i++ {
		if t.Field(i).PkgPath != "" {
			continue
		}
		out.Field(i).Set(floorCarryValue(v.Field(i)))
	}
	return out
}

func floorCarrySlice(v reflect.Value) reflect.Value {
	n := min(v.Len(), floorCarryCollectionCap)
	out := reflect.MakeSlice(v.Type(), n, n)
	for i := 0; i < n; i++ {
		out.Index(i).Set(floorCarryValue(v.Index(i)))
	}
	return out
}

// floorCarryMap truncates DETERMINISTICALLY for a string-keyed map (sorted
// keys), because a nondeterministic floor is not a testable one. A map with any
// other key kind takes range order, which is bounded but arbitrary — no wired
// response type has one today.
func floorCarryMap(v reflect.Value) reflect.Value {
	out := reflect.MakeMap(v.Type())
	keys := v.MapKeys()
	if v.Type().Key().Kind() == reflect.String {
		sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
	}
	for i, k := range keys {
		if i >= floorCarryCollectionCap {
			break
		}
		out.SetMapIndex(floorCarryValue(k), floorCarryValue(v.MapIndex(k)))
	}
	return out
}

func hasUnexportedField(t reflect.Type) bool {
	for i := 0; i < t.NumField(); i++ {
		if t.Field(i).PkgPath != "" {
			return true
		}
	}
	return false
}

// boundRunRowOutput bounds a response whose heavy content is an embedded run
// row. rows returns pointers INTO out (so a tier mutates the response in
// place); set installs the projected elisions block.
//
// The floor starts from the ZERO value of T and installs the first row's
// diagnosis core, so it is constant-size for ANY response type — which is what
// makes the guarantee "a mutating verb's success signal does not depend on
// whether its body fits" hold for cancel_run, start_campaign_item_run and the
// rest, rather than only for the fields this ladder happens to enumerate.
//
// A zero value is an honest OMISSION only for a field the schema lets the
// encoder drop. carryNonOmitemptyFields therefore re-installs, from the real
// response, every field whose json tag has no omitempty — bounded, never
// verbatim — so the floor never publishes a fabricated `"idempotent": false` or
// an all-empty `"item"` while its aggregate elision claims those fields were
// omitted. See that function for the full rationale.
func boundRunRowOutput[T any](out T, runID string, budget int, rows func(*T) []*Run, set func(*T, *Elisions)) (T, error) {
	n, err := marshalledLen(out)
	if err != nil {
		return out, err
	}
	if n <= budget {
		return out, nil
	}

	c := runRowCtx{prefix: "run", runID: runID}
	led := &elisionLedger{budget: budget, note: "response reduced to fit the tool-result byte budget"}
	for _, tier := range runRowTiers {
		led.tier = tier.name
		tier.apply(rows(&out), c, led)
		fits, ferr := attachAndMeasureOut(&out, led, budget, set)
		if ferr != nil {
			return out, ferr
		}
		if fits {
			return out, nil
		}
	}

	var floor T
	carryNonOmitemptyFields(&floor, &out)
	fr, sr := rows(&floor), rows(&out)
	id := runID
	if len(fr) > 0 && len(sr) > 0 {
		*fr[0] = runRowDiagnosisCore(*sr[0])
		id = fr[0].ID
	}
	w, err := runRowFloorLedger(c, budget, id).wire()
	if err != nil {
		return out, err
	}
	set(&floor, w)
	return floor, nil
}

// ---------------------------------------------------------------------------
// fishhawk_list_runs — the multi-row ladder
// ---------------------------------------------------------------------------

// boundListRunsOutput bounds a whole PAGE of run rows. It sheds WITHIN rows
// first (R1..R4, exactly the shared run-row tiers) and only then drops whole
// trailing rows.
//
// THE CURSOR RULE (binding, #2510). The backend's next_cursor is positioned
// after the FULL page it returned. Returning it alongside a TRUNCATED item list
// would make an operator paging by cursor SILENTLY SKIP every dropped row — a
// data-loss bug strictly worse than the hard failure this bound fixes. So the
// moment any row is dropped, NextCursor is BLANKED. Do not "restore" it as a
// convenience on a later refactor: the field is cleared because it is WRONG,
// not because it is redundant.
//
// list_runs has no since_sequence analogue to anchor a partial continuation, so
// the list_audit remedy (an anchored pointer that returns exactly the dropped
// set) does not transfer. This surface takes option (b) of the two the bound
// admits: drop trailing rows, blank the cursor, and point the stored elision at
// the UNBOUNDED REST enumeration (GET /v0/runs) — never at the now-bounded
// fishhawk_list_runs, which would cap the same page again.
func boundListRunsOutput(out ListRunsOutput, budget int) (ListRunsOutput, error) {
	set := func(o *ListRunsOutput, e *Elisions) { o.Elisions = e }
	rows := func(o *ListRunsOutput) []*Run {
		ptrs := make([]*Run, len(o.Items))
		for i := range o.Items {
			ptrs[i] = &o.Items[i]
		}
		return ptrs
	}

	n, err := marshalledLen(out)
	if err != nil {
		return out, err
	}
	if n <= budget {
		return out, nil
	}

	c := runRowCtx{prefix: "items[]", page: true}
	led := &elisionLedger{budget: budget, note: "response reduced to fit the tool-result byte budget"}
	for _, tier := range runRowTiers {
		led.tier = tier.name
		tier.apply(rows(&out), c, led)
		fits, ferr := attachAndMeasureOut(&out, led, budget, set)
		if ferr != nil {
			return out, ferr
		}
		if fits {
			return out, nil
		}
	}

	// R5: drop whole trailing rows, halving the kept prefix until it fits.
	// Every iteration re-derives the entry from the post-R4 prefix, so the
	// ledger carries ONE R5 entry reporting the FINAL dropped count.
	base := append([]elidedField(nil), led.entries...)
	total := len(out.Items)
	for len(out.Items) > 1 {
		keep := len(out.Items) / 2
		out.Items = out.Items[:keep]
		out.NextCursor = "" // THE CURSOR RULE — see the doc comment above.
		led.tier = "R5"
		led.entries = append(append([]elidedField(nil), base...),
			newStoredElision("items", fmt.Sprintf(
				"%d trailing rows were dropped to fit the byte budget, and next_cursor was BLANKED because the backend's cursor is positioned after the FULL page — returning it here would silently skip the dropped rows. Re-read the enumeration from the unbounded REST surface below",
				total-keep), unboundedRuns().retrievalPointer, total-keep))
		fits, ferr := attachAndMeasureOut(&out, led, budget, set)
		if ferr != nil {
			return out, ferr
		}
		if fits {
			return out, nil
		}
	}

	// Floor: at most ONE row, projected to the diagnosis core, cursor blanked.
	// No carryNonOmitemptyFields pass is needed here (nor in listAuditFloor):
	// the only non-omitempty field on either page type is `items`, which the
	// floor installs itself, so there is no field whose zero value would be
	// emitted as though it were real. TestFloorNeverFabricatesNonOmitemptyField
	// pins that, so a page type that later gains one fails rather than silently
	// falsifying it.
	floor := ListRunsOutput{}
	if len(out.Items) > 0 {
		floor.Items = []Run{runRowDiagnosisCore(out.Items[0])}
	}
	w, err := runRowFloorLedger(c, budget, "").wire()
	if err != nil {
		return out, err
	}
	floor.Elisions = w
	return floor, nil
}

// ---------------------------------------------------------------------------
// fishhawk_list_audit — the verifier surface
// ---------------------------------------------------------------------------

// boundListAuditOutput bounds a page of audit entries.
//
// THE CURSOR RULE (binding, #2510), identical in kind to list_runs': the
// backend's next_cursor is the token for the FULL page, so the moment A2 (or
// the floor) drops any item, NextCursor is BLANKED and the since_sequence-
// anchored elision pointer becomes the SOLE continuation path. Do not "restore"
// the cursor on a later refactor — it is cleared because it is WRONG.
//
// entry_hash / prev_hash are RETAINED at every tier including the floor:
// fishhawk_list_audit is the VERIFIER surface for the audit hash chain
// (client.go's comment on AuditEntry.EntryHash says so explicitly), and dropping
// the chain to save bytes would break verification rather than shrink it.
func boundListAuditOutput(out ListAuditOutput, runID, category string, budget int) (ListAuditOutput, error) {
	set := func(o *ListAuditOutput, e *Elisions) { o.Elisions = e }

	n, err := marshalledLen(out)
	if err != nil {
		return out, err
	}
	if n <= budget {
		return out, nil
	}

	led := &elisionLedger{budget: budget, note: "response reduced to fit the tool-result byte budget"}

	// A1: cap every oversized payload string value, escape-aware.
	led.tier = "A1"
	elided := 0
	for i := range out.Items {
		capped, dropped := capPayloadStrings(out.Items[i].Payload, auditPayloadTierCap)
		out.Items[i].Payload = capped
		elided += dropped
	}
	if elided > 0 {
		led.add(newOversizedCapableElision("items[].payload", fmt.Sprintf(
			"oversized payload string values capped to %d encoded bytes each (%d elided in total); an audit payload can itself be arbitrarily large, so the untruncated entries are read from the UNBOUNDED REST audit walk — re-calling this now-bounded tool would cap the same values again",
			auditPayloadTierCap, elided), unboundedRunAudit(runID), elided))
	}
	fits, err := attachAndMeasureOut(&out, led, budget, set)
	if err != nil {
		return out, err
	}
	if fits {
		return out, nil
	}

	// A2: drop items from the TAIL. Rows are sequence-ASCENDING, so keeping the
	// PREFIX makes the dropped set exactly "sequence > the last kept sequence"
	// — precisely what the strictly-greater-than since_sequence filter returns.
	base := append([]elidedField(nil), led.entries...)
	total := len(out.Items)
	for len(out.Items) > 1 {
		keep := len(out.Items) / 2
		out.Items = out.Items[:keep]
		out.NextCursor = "" // THE CURSOR RULE — see the doc comment above.
		led.tier = "A2"
		led.entries = append(append([]elidedField(nil), base...),
			newStoredElision("items", fmt.Sprintf(
				"%d trailing entries were dropped to fit the byte budget, and next_cursor was BLANKED because the backend's cursor is positioned after the FULL page — returning it here would silently skip the dropped entries. The pointer below is anchored at the last kept sequence, so it returns exactly the dropped set",
				total-keep),
				pointerListAudit(runID, category, out.Items[keep-1].Sequence), total-keep))
		fits, ferr := attachAndMeasureOut(&out, led, budget, set)
		if ferr != nil {
			return out, ferr
		}
		if fits {
			return out, nil
		}
	}

	return listAuditFloor(out, runID, category, budget, total)
}

// listAuditFloor keeps EXACTLY ONE entry with its payload dropped and its
// hash-chain fields intact, guaranteeing convergence under
// max(budget, mcpConvergenceFloorBytes). Every retained non-hash string is
// capped; the hashes are never capped, because a truncated hash is not a
// smaller hash, it is a broken one.
func listAuditFloor(out ListAuditOutput, runID, category string, budget, total int) (ListAuditOutput, error) {
	floor := ListAuditOutput{}
	anchor := int64(0)
	if len(out.Items) > 0 {
		e := out.Items[0]
		anchor = e.Sequence
		kept := AuditEntry{
			ID:        capJSONString(e.ID, floorFieldCap),
			Sequence:  e.Sequence,
			RunID:     capJSONString(e.RunID, floorFieldCap),
			Timestamp: e.Timestamp,
			Category:  capJSONString(e.Category, floorFieldCap),
			PrevHash:  e.PrevHash,
			EntryHash: e.EntryHash,
		}
		floor.Items = []AuditEntry{kept}
	}
	led := &elisionLedger{
		budget: budget,
		tier:   floorTierName,
		note:   "reduced to the constant-size floor: one entry with its hash chain intact and its payload dropped. The entry below is an AGGREGATE — the floor tier's explicit exception to per-field itemisation",
	}
	led.add(aggregateStoredElision("*", fmt.Sprintf(
		"every payload and every entry beyond the first was omitted (%d entries dropped), and next_cursor was BLANKED so cursor paging cannot silently skip them; entry_hash / prev_hash are retained on the kept entry because this is the hash-chain verifier surface. The anchored walk below returns exactly the dropped entries; the REST surface returns the whole chain untruncated",
		max(total-len(floor.Items), 0)),
		[]string{pointerListAudit(runID, category, anchor).String(), unboundedRunAudit(runID).String()}))
	w, err := led.wire()
	if err != nil {
		return out, err
	}
	floor.Elisions = w
	return floor, nil
}

// capPayloadStrings returns a copy of a decoded-JSON audit payload with every
// oversized string value capped ESCAPE-AWARE via capJSONString, plus the total
// encoded bytes elided. It descends into maps and slices exactly as compact.go's
// truncateValue does; unlike that helper it is a genuine size BOUND, because a
// raw byte cap is not one under JSON escaping.
func capPayloadStrings(v any, budget int) (any, int) {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		total := 0
		for k, val := range t {
			capped, n := capPayloadStrings(val, budget)
			out[k] = capped
			total += n
		}
		return out, total
	case []any:
		out := make([]any, len(t))
		total := 0
		for i := range t {
			capped, n := capPayloadStrings(t[i], budget)
			out[i] = capped
			total += n
		}
		return out, total
	case string:
		before := jsonEncodedLen(t)
		if before <= budget {
			return t, 0
		}
		capped := capJSONString(t, budget)
		return capped, before - jsonEncodedLen(capped)
	default:
		return v, 0
	}
}
