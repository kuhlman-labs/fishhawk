package mcpserver

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// fixtures
// ---------------------------------------------------------------------------

// auditPage builds n sequence-ASCENDING entries whose payloads carry the given
// oversized string, each with a full hash chain — the shape fishhawk_list_audit
// returns at its advertised maximum.
func auditPage(runID string, n int, payload string) []AuditEntry {
	items := make([]AuditEntry, 0, n)
	for i := 1; i <= n; i++ {
		prev := strings.Repeat("a", 64)
		items = append(items, AuditEntry{
			ID:        uuid.NewString(),
			Sequence:  int64(i),
			RunID:     runID,
			Timestamp: time.Date(2026, 8, 7, 12, 0, i%60, 0, time.UTC),
			Category:  "implement_reviewed",
			Payload: map[string]any{
				"verdict":   "request_changes",
				"free_form": payload,
				"concerns":  []any{map[string]any{"note": payload, "severity": "high"}},
			},
			PrevHash:  &prev,
			EntryHash: strings.Repeat("b", 64),
		})
	}
	return items
}

// worstCaseIssueRun is the real 143aea12 shape: a large issue body plus ten
// multi-KB comments on one run row.
func worstCaseIssueRun(runID string) Run {
	comments := make([]IssueComment, 0, 10)
	for i := 0; i < 10; i++ {
		comments = append(comments, IssueComment{
			Author:    "kuhlman-labs",
			Body:      strings.Repeat("comment prose that an operator never gates on. ", 120),
			CreatedAt: "2026-08-07T12:00:00Z",
		})
	}
	pr := "https://github.com/kuhlman-labs/fishhawk/pull/2511"
	return Run{
		ID:             runID,
		Repo:           "kuhlman-labs/fishhawk",
		WorkflowID:     "feature_change",
		State:          "running",
		RunnerKind:     "local",
		PullRequestURL: &pr,
		IssueContext: &IssueContext{
			Title:    "[E48.77] two unbounded surfaces",
			Body:     strings.Repeat("issue body prose. ", 4000),
			URL:      "https://github.com/kuhlman-labs/fishhawk/issues/2510",
			Number:   2510,
			Labels:   []string{"area:mcp", "type:bug"},
			Comments: comments,
		},
		Concerns: &RunConcerns{Open: 3},
	}
}

// floorForcingRun is worstCaseIssueRun with every RETAINED string inflated too,
// so the within-row tiers cannot get the row under the convergence floor and the
// ladder is driven all the way INTO the floor tier. Shared by the convergence
// pin and the floor-honesty table, which both need the floor actually reached.
func floorForcingRun(runID string) Run {
	run := worstCaseIssueRun(runID)
	run.Repo = strings.Repeat("kuhlman-labs/very-long-repo-name-", 500)
	run.WorkflowID = strings.Repeat("feature_change_", 500)
	pr := "https://github.com/kuhlman-labs/fishhawk/pull/" + strings.Repeat("9", 5000)
	run.PullRequestURL = &pr
	parent := strings.Repeat("parent-", 500)
	run.ParentRunID = &parent
	run.RunnerKind = strings.Repeat("local-", 500)
	run.WorkflowSHA = strings.Repeat("f", 5000)
	return run
}

func singleRunRows(o *GetActiveRunOutput) []*Run      { return []*Run{&o.Run} }
func singleRunSet(o *GetActiveRunOutput, e *Elisions) { o.Elisions = e }
func boundOneRow(out GetActiveRunOutput, budget int) (GetActiveRunOutput, error) {
	return boundRunRowOutput(out, out.Run.ID, fixedBudget(budget), singleRunRows, singleRunSet)
}

// fixedBudget wraps a bare byte count as a responseBudget for the many bound-*
// test call sites that predate the discovery ladder (#2509). It reports source
// "default" — the rung a test that never advertises a capability resolves to —
// so the wire DTO's budget_source is a valid, expected value.
func fixedBudget(n int) responseBudget {
	return responseBudget{bytes: n, source: sourceDefault}
}

func marshalLen(t *testing.T, v any) int {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return len(raw)
}

// ---------------------------------------------------------------------------
// (1) ADVERTISED-MAXIMUM regression — one per bounded surface
// ---------------------------------------------------------------------------

// TestBoundListAudit_AtAdvertisedMaxLimit drives the surface at limit=200, the
// value the tool advertises and the one that returns ~172 KB unbounded. The
// escaping rows are separate cases on purpose: jsonEncodedLen's 6x inflation is
// exactly what a raw-byte cap would miss, so a bound proven only on plain ASCII
// would not be a bound at all.
func TestBoundListAudit_AtAdvertisedMaxLimit(t *testing.T) {
	runID := uuid.NewString()
	rows := []struct {
		name    string
		payload string
	}{
		{"plain prose", strings.Repeat("plain reviewer prose. ", 200)},
		{"all-'<' bytes (6x HTML-escape inflation)", strings.Repeat("<", 4000)},
		{"invalid UTF-8 (6x replacement-char inflation)", strings.Repeat("\xff", 4000)},
		{"control bytes (6x \\u00XX inflation)", strings.Repeat("\x01", 4000)},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			budget := mcpResponseByteBudgetDefault
			in := ListAuditOutput{Items: auditPage(runID, listAuditLimitMax, row.payload), NextCursor: "cur-1"}
			if n := marshalLen(t, in); n <= budget {
				t.Fatalf("fixture is only %d bytes — it does not exercise the bound", n)
			}
			out, err := boundListAuditOutput(in, runID, "implement_reviewed", fixedBudget(budget))
			if err != nil {
				t.Fatalf("bound: %v", err)
			}
			if n := marshalLen(t, out); n > budget {
				t.Errorf("bounded list_audit = %d bytes, want <= %d", n, budget)
			}
			if out.Elisions == nil {
				t.Fatal("a reduced response carries no elisions block")
			}
			if err := validateWireElisions(out.Elisions); err != nil {
				t.Errorf("elisions violate their own classification: %v", err)
			}
		})
	}
}

// TestBoundRunRow_AtWorstCaseIssueContext drives the real 143aea12 row shape (a
// ~79 KB issue_context) through the shared run-row ladder.
func TestBoundRunRow_AtWorstCaseIssueContext(t *testing.T) {
	runID := uuid.NewString()
	in := GetActiveRunOutput{Run: worstCaseIssueRun(runID)}
	budget := mcpResponseByteBudgetDefault
	if n := marshalLen(t, in); n <= budget {
		t.Fatalf("fixture is only %d bytes — it does not exercise the bound", n)
	}
	out, err := boundOneRow(in, budget)
	if err != nil {
		t.Fatalf("bound: %v", err)
	}
	if n := marshalLen(t, out); n > budget {
		t.Errorf("bounded run row = %d bytes, want <= %d", n, budget)
	}
	if out.Run.ID != runID || out.Run.State != "running" {
		t.Errorf("the diagnosis core did not survive: id=%q state=%q", out.Run.ID, out.Run.State)
	}
	if out.Elisions == nil {
		t.Fatal("a reduced run row carries no elisions block")
	}
	if err := validateWireElisions(out.Elisions); err != nil {
		t.Errorf("elisions violate their own classification: %v", err)
	}
	// The issue URL is the most actionable unbounded surface and must be what
	// the oversized_capable entry names.
	var sawIssuePointer bool
	for _, f := range out.Elisions.Fields {
		if strings.Contains(f.Pointer, "https://github.com/kuhlman-labs/fishhawk/issues/2510") {
			sawIssuePointer = true
		}
	}
	if !sawIssuePointer {
		t.Errorf("no elision points at the issue itself: %+v", out.Elisions.Fields)
	}
}

// TestBoundListRuns_AtAdvertisedMaxLimit is the multi-row shedding path's own
// advertised-maximum regression (binding condition 4): a full limit=200 page,
// every row carrying the worst-case issue_context, with include_issue_context
// effectively on.
func TestBoundListRuns_AtAdvertisedMaxLimit(t *testing.T) {
	items := make([]Run, 0, listRunsLimitMax)
	for i := 0; i < listRunsLimitMax; i++ {
		items = append(items, worstCaseIssueRun(uuid.NewString()))
	}
	budget := mcpResponseByteBudgetDefault
	in := ListRunsOutput{Items: items, NextCursor: "cur-1"}
	if n := marshalLen(t, in); n <= budget {
		t.Fatalf("fixture is only %d bytes — it does not exercise the bound", n)
	}
	out, err := boundListRunsOutput(in, fixedBudget(budget))
	if err != nil {
		t.Fatalf("bound: %v", err)
	}
	if n := marshalLen(t, out); n > budget {
		t.Errorf("bounded list_runs = %d bytes, want <= %d", n, budget)
	}
	if out.Elisions == nil {
		t.Fatal("a reduced page carries no elisions block")
	}
	if err := validateWireElisions(out.Elisions); err != nil {
		t.Errorf("elisions violate their own classification: %v", err)
	}
}

// ---------------------------------------------------------------------------
// (2) the UNDER-BUDGET pass-through — byte-identical to the pre-bound wire
// ---------------------------------------------------------------------------

func TestBoundedSurfaces_UnderBudgetAreUnchanged(t *testing.T) {
	runID := uuid.NewString()
	budget := mcpResponseByteBudgetDefault

	audit := ListAuditOutput{Items: auditPage(runID, 3, "short"), NextCursor: "cur-1"}
	before := marshalLen(t, audit)
	got, err := boundListAuditOutput(audit, runID, "", fixedBudget(budget))
	if err != nil {
		t.Fatalf("bound audit: %v", err)
	}
	if got.Elisions != nil {
		t.Error("an under-budget list_audit response carries an elisions block")
	}
	if got.NextCursor != "cur-1" {
		t.Errorf("an under-budget response blanked the cursor: %q", got.NextCursor)
	}
	if n := marshalLen(t, got); n != before {
		t.Errorf("under-budget list_audit changed size %d -> %d", before, n)
	}

	pr := "https://github.com/kuhlman-labs/fishhawk/pull/1"
	row := GetActiveRunOutput{Run: Run{ID: runID, Repo: "o/n", WorkflowID: "feature_change", State: "running", PullRequestURL: &pr}}
	before = marshalLen(t, row)
	gotRow, err := boundOneRow(row, budget)
	if err != nil {
		t.Fatalf("bound row: %v", err)
	}
	if gotRow.Elisions != nil {
		t.Error("an under-budget run row carries an elisions block")
	}
	if n := marshalLen(t, gotRow); n != before {
		t.Errorf("under-budget run row changed size %d -> %d", before, n)
	}

	runs := ListRunsOutput{Items: []Run{row.Run}, NextCursor: "cur-1"}
	gotRuns, err := boundListRunsOutput(runs, fixedBudget(budget))
	if err != nil {
		t.Fatalf("bound runs: %v", err)
	}
	if gotRuns.Elisions != nil || gotRuns.NextCursor != "cur-1" || len(gotRuns.Items) != 1 {
		t.Errorf("an under-budget list_runs page was modified: %+v", gotRuns)
	}
}

// ---------------------------------------------------------------------------
// (3) CONVERGENCE under the shared floor
// ---------------------------------------------------------------------------

// TestBoundListAudit_ConvergesUnderFloor is the COUNTERFACTUAL vehicle for the
// list-audit floor tier: delete listAuditFloor's projection and this goes red.
func TestBoundListAudit_ConvergesUnderFloor(t *testing.T) {
	runID := uuid.NewString()
	budget := mcpConvergenceFloorBytes
	// Pathological on every axis the floor must survive: an enormous payload,
	// a long category and long ids — not only the field dropped first.
	items := auditPage(runID, 40, strings.Repeat("<", 20000))
	for i := range items {
		items[i].Category = strings.Repeat("category_", 400)
		items[i].ID = strings.Repeat("id-", 400)
		items[i].RunID = strings.Repeat("run-", 400)
	}
	out, err := boundListAuditOutput(ListAuditOutput{Items: items, NextCursor: "cur-1"}, runID, "implement_reviewed", fixedBudget(budget))
	if err != nil {
		t.Fatalf("bound: %v", err)
	}
	if n := marshalLen(t, out); n > budget {
		t.Errorf("floor-budget list_audit = %d bytes, want <= %d", n, budget)
	}
	if out.Elisions == nil || out.Elisions.Tier != floorTierName {
		t.Fatalf("want the floor tier, got %+v", out.Elisions)
	}
}

// TestBoundRunRow_ConvergesUnderFloor is the COUNTERFACTUAL vehicle for the
// run-row floor: delete the floor projection and this goes red.
//
// The fixture stresses the RETAINED strings, not only the field dropped first
// (binding condition 3): repo, workflow_id, pull_request_url, parent_run_id and
// runner_kind are each pathologically long. The floor guarantee is the whole
// basis for claiming a mutating verb's success signal is decoupled from whether
// its body fits, and a fixture that only inflates issue_context would leave that
// claim untested.
func TestBoundRunRow_ConvergesUnderFloor(t *testing.T) {
	runID := uuid.NewString()
	run := floorForcingRun(runID)

	budget := mcpConvergenceFloorBytes
	out, err := boundOneRow(GetActiveRunOutput{Run: run}, budget)
	if err != nil {
		t.Fatalf("bound: %v", err)
	}
	if n := marshalLen(t, out); n > budget {
		t.Errorf("floor-budget run row = %d bytes, want <= %d", n, budget)
	}
	if out.Elisions == nil || out.Elisions.Tier != floorTierName {
		t.Fatalf("want the floor tier, got %+v", out.Elisions)
	}
	if out.Run.ID == "" || out.Run.State == "" {
		t.Errorf("the floor dropped the diagnosis core: %+v", out.Run)
	}
}

// TestBoundRunRow_FloorCarriesNonOmitemptyFields is the honesty pin for the
// generic floor: a field the schema forbids omitting is emitted whatever the
// floor does with it, so zeroing it does not OMIT it — it publishes a wrong
// value that reads as real, while the aggregate elision claims it was omitted.
//
// Every case drives an output type OTHER than GetActiveRunOutput all the way to
// the FLOOR tier (floor-clamped budget + a pathological row) — the path no test
// reached before — and asserts the true value SURVIVED. It is a state
// assertion, not an error identity: deleting the carryNonOmitemptyFields call
// leaves boundRunRowOutput returning the same nil error with a falsified body.
func TestBoundRunRow_FloorCarriesNonOmitemptyFields(t *testing.T) {
	budget := mcpConvergenceFloorBytes

	t.Run("resume_run keeps a TRUE idempotent replay signal", func(t *testing.T) {
		runID := uuid.NewString()
		out, err := boundRunRowOutput(
			ResumeRunOutput{Run: floorForcingRun(runID), Idempotent: true}, runID, fixedBudget(budget),
			func(o *ResumeRunOutput) []*Run { return []*Run{&o.Run} },
			func(o *ResumeRunOutput, e *Elisions) { o.Elisions = e })
		if err != nil {
			t.Fatalf("bound: %v", err)
		}
		if out.Elisions == nil || out.Elisions.Tier != floorTierName {
			t.Fatalf("the fixture did not reach the floor tier (%+v) — the honesty assertion would be vacuous", out.Elisions)
		}
		if !out.Idempotent {
			t.Error(`the floor rendered "idempotent": false on a call that genuinely REPLAYED — idempotent has no omitempty, so it is PRESENT and WRONG, not omitted; an operator reading it could mint a duplicate recovery run`)
		}
		if n := marshalLen(t, out); n > budget {
			t.Errorf("floor-budget resume_run = %d bytes, want <= %d", n, budget)
		}
		if out.Run.ID == "" {
			t.Errorf("the floor dropped the diagnosis core: %+v", out.Run)
		}
	})

	t.Run("start_campaign_item_run keeps the run<->item linkage", func(t *testing.T) {
		runID := uuid.NewString()
		item := CampaignItem{
			ID:        uuid.NewString(),
			IssueRef:  "kuhlman-labs/fishhawk#2510",
			DependsOn: []string{"kuhlman-labs/fishhawk#2508"},
			RunID:     runID,
			State:     "running",
			CreatedAt: time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC),
			UpdatedAt: time.Date(2026, 8, 7, 12, 5, 0, 0, time.UTC),
		}
		out, err := boundRunRowOutput(
			StartCampaignItemRunOutput{Run: floorForcingRun(runID), Item: item}, runID, fixedBudget(budget),
			func(o *StartCampaignItemRunOutput) []*Run { return []*Run{&o.Run} },
			func(o *StartCampaignItemRunOutput, e *Elisions) { o.Elisions = e })
		if err != nil {
			t.Fatalf("bound: %v", err)
		}
		if out.Elisions == nil || out.Elisions.Tier != floorTierName {
			t.Fatalf("the fixture did not reach the floor tier (%+v) — the honesty assertion would be vacuous", out.Elisions)
		}
		if out.Item.ID != item.ID || out.Item.IssueRef != item.IssueRef || out.Item.State != item.State || out.Item.RunID != runID {
			t.Errorf(`the floor rendered an all-empty but populated-LOOKING "item" (%+v), losing the run<->item linkage; item has no omitempty, so it is PRESENT and WRONG, not omitted`, out.Item)
		}
		if len(out.Item.DependsOn) != 1 || out.Item.DependsOn[0] != item.DependsOn[0] {
			t.Errorf("the carried item lost its dependency edges: %+v", out.Item.DependsOn)
		}
		if !out.Item.CreatedAt.Equal(item.CreatedAt) {
			t.Errorf("the carried item's created_at = %v, want the real %v", out.Item.CreatedAt, item.CreatedAt)
		}
		if n := marshalLen(t, out); n > budget {
			t.Errorf("floor-budget start_campaign_item_run = %d bytes, want <= %d", n, budget)
		}
	})

	t.Run("revive_run keeps restored_stages and next_step", func(t *testing.T) {
		runID := uuid.NewString()
		stages := []ReviveRestoredStage{
			{StageID: uuid.NewString(), Type: "implement", PriorCategory: "A", PriorReason: strings.Repeat("agent harness 400. ", 4000), RestoredState: "pending"},
			{StageID: uuid.NewString(), Type: "review", PriorCategory: "C", PriorReason: strings.Repeat("lineage lock. ", 4000), RestoredState: "pending"},
			{StageID: uuid.NewString(), Type: "acceptance", PriorCategory: "D", PriorReason: "sla timeout", RestoredState: "awaiting_approval"},
		}
		out, err := boundRunRowOutput(
			ReviveRunOutput{Run: floorForcingRun(runID), RestoredStages: stages, NextStep: reviveNextStepHint}, runID, fixedBudget(budget),
			func(o *ReviveRunOutput) []*Run { return []*Run{&o.Run} },
			func(o *ReviveRunOutput, e *Elisions) { o.Elisions = e })
		if err != nil {
			t.Fatalf("bound: %v", err)
		}
		if out.Elisions == nil || out.Elisions.Tier != floorTierName {
			t.Fatalf("the fixture did not reach the floor tier (%+v) — the honesty assertion would be vacuous", out.Elisions)
		}
		if len(out.RestoredStages) == 0 {
			t.Error(`the floor rendered "restored_stages": null on a revive that DID re-park stages; the field has no omitempty, so the floor must carry what it can rather than publish an empty answer`)
		}
		if len(out.RestoredStages) > floorCarryCollectionCap {
			t.Errorf("the floor carried %d stages, want at most %d — carrying must stay bounded", len(out.RestoredStages), floorCarryCollectionCap)
		}
		if len(out.RestoredStages) > 0 && out.RestoredStages[0].StageID != stages[0].StageID {
			t.Errorf("the carried stage is not the real one: %+v", out.RestoredStages[0])
		}
		if out.NextStep == "" {
			t.Error(`the floor rendered "next_step": "" — the field has no omitempty, so an empty value reads as "there is no next step"`)
		}
		// The carried collection is BOUNDED, not verbatim: two multi-KB
		// prior_reason strings would blow the constant-size floor on their own.
		for i, s := range out.RestoredStages {
			if jsonEncodedLen(s.PriorReason) > floorFieldCap {
				t.Errorf("carried stage %d kept a %d-byte prior_reason, want it capped at %d", i, jsonEncodedLen(s.PriorReason), floorFieldCap)
			}
		}
		if n := marshalLen(t, out); n > budget {
			t.Errorf("floor-budget revive_run = %d bytes, want <= %d", n, budget)
		}
	})
}

// TestCarryNonOmitemptyFields_SkipsLadderOwnedFields pins the carry's OTHER
// half directly, because boundRunRowOutput's own output cannot distinguish it:
// the diagnosis-core install runs immediately after the carry and overwrites the
// Run row either way, so a floor-tier assertion stays green with the skip
// deleted. Asserting on the carry in ISOLATION is what makes the skip real.
func TestCarryNonOmitemptyFields_SkipsLadderOwnedFields(t *testing.T) {
	src := GetActiveRunOutput{
		Run:      worstCaseIssueRun(uuid.NewString()),
		Elisions: &Elisions{Tier: "R1", Budget: 4096},
	}
	var dst GetActiveRunOutput
	carryNonOmitemptyFields(&dst, &src)
	if dst.Run.ID != "" || dst.Run.IssueContext != nil {
		t.Errorf("the carry wrote the Run row, which the ladder installs itself as the diagnosis core: %+v", dst.Run)
	}
	if dst.Elisions != nil {
		t.Errorf("the carry wrote the elisions block, which the ladder's setter installs: %+v", dst.Elisions)
	}
}

// TestFloorNeverFabricatesNonOmitemptyField is the forward guard for the two
// PAGE surfaces, whose floors build their output by hand rather than through
// carryNonOmitemptyFields. Today the only non-omitempty field on either type is
// `items`, which those floors install themselves — so there is nothing to
// falsify. This fails the day one of them gains another, rather than letting the
// new field ship as a silent zero.
func TestFloorNeverFabricatesNonOmitemptyField(t *testing.T) {
	rows := []struct {
		name     string
		typ      reflect.Type
		installs map[string]bool
	}{
		{"ListRunsOutput", reflect.TypeOf(ListRunsOutput{}), map[string]bool{"items": true}},
		{"ListAuditOutput", reflect.TypeOf(ListAuditOutput{}), map[string]bool{"items": true}},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			for i := 0; i < row.typ.NumField(); i++ {
				f := row.typ.Field(i)
				if f.PkgPath != "" || jsonTagOmits(f) {
					continue
				}
				name := strings.Split(f.Tag.Get("json"), ",")[0]
				if !row.installs[name] {
					t.Errorf("%s.%s (json %q) has no omitempty, so its ZERO value is emitted at the floor as though it were real — carry it like boundRunRowOutput does, or give it omitempty",
						row.name, f.Name, name)
				}
			}
		})
	}
}

// TestBoundListRuns_ConvergesUnderFloor pins the page floor.
func TestBoundListRuns_ConvergesUnderFloor(t *testing.T) {
	items := make([]Run, 0, 20)
	for i := 0; i < 20; i++ {
		r := worstCaseIssueRun(uuid.NewString())
		r.Repo = strings.Repeat("kuhlman-labs/very-long-repo-name-", 500)
		items = append(items, r)
	}
	budget := mcpConvergenceFloorBytes
	out, err := boundListRunsOutput(ListRunsOutput{Items: items, NextCursor: "cur-1"}, fixedBudget(budget))
	if err != nil {
		t.Fatalf("bound: %v", err)
	}
	if n := marshalLen(t, out); n > budget {
		t.Errorf("floor-budget list_runs = %d bytes, want <= %d", n, budget)
	}
	if out.NextCursor != "" {
		t.Errorf("the floor kept a cursor alongside a truncated page: %q", out.NextCursor)
	}
}

// ---------------------------------------------------------------------------
// (4) THE CURSOR RULE — state-asserting counterfactuals
// ---------------------------------------------------------------------------

// TestBoundListAudit_BlanksCursorOnTruncation asserts the STATE — that the
// response carries NO cursor once entries were dropped — rather than an error
// identity, because an error-identity assertion cannot distinguish a cursor
// that was CLEARED from one that was never set. The fixture seeds a non-empty
// cursor BY CONSTRUCTION so the assertion can only pass if the blanking ran.
func TestBoundListAudit_BlanksCursorOnTruncation(t *testing.T) {
	runID := uuid.NewString()
	const cursor = "eyJzZXEiOjIwMH0="
	in := ListAuditOutput{Items: auditPage(runID, 200, strings.Repeat("prose ", 300)), NextCursor: cursor}
	out, err := boundListAuditOutput(in, runID, "implement_reviewed", fixedBudget(mcpResponseByteBudgetDefault))
	if err != nil {
		t.Fatalf("bound: %v", err)
	}
	if len(out.Items) >= 200 {
		t.Fatalf("the fixture did not truncate (%d items kept) — the cursor assertion would be vacuous", len(out.Items))
	}
	if out.NextCursor != "" {
		t.Errorf("next_cursor = %q alongside a TRUNCATED item list: the backend's cursor is positioned after the FULL page, so paging by it silently SKIPS the %d dropped entries",
			out.NextCursor, 200-len(out.Items))
	}
}

// TestBoundListRuns_BlanksCursorOnTruncation is the same state assertion for
// the enumeration (binding condition 1): no surface may return a page-level
// cursor alongside a truncated item list.
func TestBoundListRuns_BlanksCursorOnTruncation(t *testing.T) {
	const cursor = "eyJydW4iOiJ6In0="
	const rowCount = 200
	items := make([]Run, 0, rowCount)
	for i := 0; i < rowCount; i++ {
		r := worstCaseIssueRun(uuid.NewString())
		// Long RETAINED strings, so the page is still over budget after R1..R4
		// have shed every issue_context — otherwise the within-row tiers alone
		// would fit the page and no row would ever be dropped, making the cursor
		// assertion vacuous.
		r.Repo = "kuhlman-labs/" + strings.Repeat("long-repo-segment-", 12)
		pr := "https://github.com/kuhlman-labs/fishhawk/pull/" + strings.Repeat("1", 120)
		r.PullRequestURL = &pr
		items = append(items, r)
	}
	out, err := boundListRunsOutput(ListRunsOutput{Items: items, NextCursor: cursor}, fixedBudget(mcpResponseByteBudgetDefault))
	if err != nil {
		t.Fatalf("bound: %v", err)
	}
	if len(out.Items) >= rowCount {
		t.Fatalf("the fixture did not truncate (%d rows kept) — the cursor assertion would be vacuous", len(out.Items))
	}
	if out.NextCursor != "" {
		t.Errorf("next_cursor = %q alongside a TRUNCATED row list: paging by it silently SKIPS the %d dropped runs",
			out.NextCursor, rowCount-len(out.Items))
	}
	// list_runs has no since_sequence analogue, so the continuation contract is
	// option (b): the dropped rows are named by a stored elision pointing at the
	// UNBOUNDED REST enumeration — never at the now-bounded fishhawk_list_runs.
	var sawDrop bool
	for _, f := range out.Elisions.Fields {
		if f.Field != "items" {
			continue
		}
		sawDrop = true
		if f.Pointer != "GET /v0/runs" {
			t.Errorf("dropped-rows pointer = %q, want the unbounded REST enumeration", f.Pointer)
		}
		if f.OmittedCount == 0 {
			t.Error("dropped-rows elision reports omitted_count 0")
		}
	}
	if !sawDrop && out.Elisions.Tier != floorTierName {
		t.Errorf("rows were dropped with no elision naming them: %+v", out.Elisions)
	}
}

// ---------------------------------------------------------------------------
// (5) POINTER SEMANTICS
// ---------------------------------------------------------------------------

// TestBoundListAudit_SinceSequenceAnchor proves the stored class's at-least
// promise: the anchor equals the LAST KEPT entry's sequence, and re-filtering
// the original page with that since_sequence (the backend's strictly-greater-
// than filter) returns PRECISELY the dropped set.
func TestBoundListAudit_SinceSequenceAnchor(t *testing.T) {
	runID := uuid.NewString()
	original := auditPage(runID, 200, strings.Repeat("prose ", 300))
	out, err := boundListAuditOutput(ListAuditOutput{Items: append([]AuditEntry(nil), original...)}, runID, "implement_reviewed", fixedBudget(mcpResponseByteBudgetDefault))
	if err != nil {
		t.Fatalf("bound: %v", err)
	}
	if len(out.Items) == 0 || len(out.Items) >= len(original) {
		t.Fatalf("the fixture did not truncate (%d kept of %d)", len(out.Items), len(original))
	}
	lastKept := out.Items[len(out.Items)-1].Sequence

	var anchor int64 = -1
	for _, f := range out.Elisions.Fields {
		if f.Field != "items" {
			continue
		}
		if !strings.HasPrefix(f.Pointer, "fishhawk_list_audit(") {
			t.Fatalf("dropped-entries pointer = %q, want the anchored audit walk", f.Pointer)
		}
		if !strings.Contains(f.Pointer, "category=implement_reviewed") {
			t.Errorf("pointer %q dropped the category filter", f.Pointer)
		}
		idx := strings.Index(f.Pointer, "since_sequence=")
		raw := strings.TrimSuffix(f.Pointer[idx+len("since_sequence="):], ")")
		n, perr := strconv.ParseInt(raw, 10, 64)
		if perr != nil {
			t.Fatalf("pointer %q carries an unparseable anchor: %v", f.Pointer, perr)
		}
		anchor = n
	}
	if anchor != lastKept {
		t.Fatalf("anchor = %d, want the LAST KEPT sequence %d", anchor, lastKept)
	}

	// The follow-up call the pointer names, simulated against the same page.
	var followUp, dropped []int64
	for _, e := range original {
		if e.Sequence > anchor {
			followUp = append(followUp, e.Sequence)
		}
	}
	kept := map[int64]bool{}
	for _, e := range out.Items {
		kept[e.Sequence] = true
	}
	for _, e := range original {
		if !kept[e.Sequence] {
			dropped = append(dropped, e.Sequence)
		}
	}
	if fmt.Sprint(followUp) != fmt.Sprint(dropped) {
		t.Errorf("since_sequence=%d returns %v, want exactly the dropped set %v", anchor, followUp, dropped)
	}
}

// TestBoundListAudit_RetainsHashChain pins the verifier-surface invariant:
// entry_hash / prev_hash survive EVERY tier including the floor. It goes red if
// a tier ever starts dropping them to save bytes.
func TestBoundListAudit_RetainsHashChain(t *testing.T) {
	runID := uuid.NewString()
	// floorForcing inflates the RETAINED per-entry strings, so even a
	// single-entry A2 page stays over budget and the ladder is driven all the
	// way INTO the floor tier. Without it the sweep settles at A2 and the
	// floor's own retention is never exercised — which is exactly what the
	// counterfactual run showed.
	floorForcing := func(items []AuditEntry) []AuditEntry {
		for i := range items {
			items[i].Category = strings.Repeat("category_", 400)
			items[i].ID = strings.Repeat("id-", 400)
		}
		return items
	}
	sweep := []struct {
		name   string
		budget int
		items  []AuditEntry
	}{
		{"default budget", mcpResponseByteBudgetDefault, auditPage(runID, 200, strings.Repeat("<", 4000))},
		{"16 KiB", 16 * 1024, auditPage(runID, 200, strings.Repeat("<", 4000))},
		{"8 KiB", 8 * 1024, auditPage(runID, 200, strings.Repeat("<", 4000))},
		{"floor budget", mcpConvergenceFloorBytes, auditPage(runID, 200, strings.Repeat("<", 4000))},
		{"floor TIER (retained strings inflated so the ladder reaches it)",
			mcpConvergenceFloorBytes, floorForcing(auditPage(runID, 200, strings.Repeat("<", 4000)))},
	}
	var sawFloorTier bool
	for _, row := range sweep {
		t.Run(row.name, func(t *testing.T) {
			out, err := boundListAuditOutput(
				ListAuditOutput{Items: row.items, NextCursor: "cur-1"}, runID, "", fixedBudget(row.budget))
			if err != nil {
				t.Fatalf("bound: %v", err)
			}
			if out.Elisions != nil && out.Elisions.Tier == floorTierName {
				sawFloorTier = true
			}
			if len(out.Items) == 0 {
				t.Fatal("every entry was dropped: the floor must keep one so the chain stays verifiable")
			}
			for i, e := range out.Items {
				if e.EntryHash == "" {
					t.Errorf("item %d lost entry_hash — fishhawk_list_audit is the hash-chain VERIFIER surface", i)
				}
				if e.PrevHash == nil || *e.PrevHash == "" {
					t.Errorf("item %d lost prev_hash — fishhawk_list_audit is the hash-chain VERIFIER surface", i)
				}
			}
		})
	}
	if !sawFloorTier {
		t.Fatal("the sweep never reached the floor TIER, so the floor's own hash retention went unexercised")
	}
}

// TestPointerIssueURL_RejectedByWireValidation is the COUNTERFACTUAL vehicle
// for pointerIssueURL's guard. Both bad inputs are seeded BY CONSTRUCTION as
// literals — never by calling the guard inside the test's own setup — so the RED
// lands on the behavioural assertion rather than on fixture setup.
//
// The state assertion (the emitted pointer IS the REST fallback) is what makes
// the relative-URL case discriminating: a relative URL rendered as "GET /path"
// still satisfies validateWireElisions' prefix check, so an error-identity
// assertion alone would stay green with the guard deleted.
func TestPointerIssueURL_RejectedByWireValidation(t *testing.T) {
	rows := []struct {
		name string
		url  string
	}{
		{"relative path names no retrievable surface", "/kuhlman-labs/fishhawk/issues/2510"},
		{"a fishhawk_-bearing URL reads as a bounded MCP tool", "https://example.test/fishhawk_list_audit/2510"},
		{"whitespace breaks the one-token pointer contract", "https://example.test/a b"},
		{"empty", ""},
		// The three the prefix check ACCEPTED (#2510 fix-up). Each satisfies
		// strings.HasPrefix(u, "https://") — or, for the scheme-relative form,
		// is a syntactically valid URL — yet names no retrievable surface, so
		// the "absolute, retrievable URL" invariant the guard documents was not
		// actually established until it started parsing.
		{"scheme with NO host retrieves nothing", "https://"},
		{"empty authority with a path retrieves nothing", "https:///kuhlman-labs/fishhawk/issues/2510"},
		{"a non-http scheme is not a retrievable web surface", "ftp://example.test/issues/2510"},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			// Errorf, not Fatalf: the guard-return check and the emitted-pointer
			// STATE check must both run, so a counterfactual deletion reports
			// which of the two actually bit rather than short-circuiting on the
			// first.
			if _, ok := pointerIssueURL(row.url); ok {
				t.Errorf("pointerIssueURL(%q) was accepted", row.url)
			}
			runID := uuid.NewString()
			run := worstCaseIssueRun(runID)
			run.IssueContext.URL = row.url
			out, err := boundOneRow(GetActiveRunOutput{Run: run}, mcpResponseByteBudgetDefault)
			if err != nil {
				t.Fatalf("bound: %v", err)
			}
			want := "GET /v0/runs/" + runID
			for _, f := range out.Elisions.Fields {
				if f.Class != string(classOversizedCapable) {
					continue
				}
				if f.Pointer != want {
					t.Errorf("oversized_capable pointer = %q, want the REST fallback %q (an unusable issue URL must never reach the wire)", f.Pointer, want)
				}
			}
			if err := validateWireElisions(out.Elisions); err != nil {
				t.Errorf("elisions violate their own classification: %v", err)
			}
		})
	}

	// The accepted shapes, so the guard is not vacuously refusing everything.
	for _, ok := range []string{"https://github.com/o/n/issues/1", "http://internal.test/issues/1"} {
		if _, accepted := pointerIssueURL(ok); !accepted {
			t.Errorf("pointerIssueURL(%q) was refused", ok)
		}
	}
}

// ---------------------------------------------------------------------------
// (6) tier behaviour
// ---------------------------------------------------------------------------

// canonicalRunRowTierOrder is the ladder's least-actionable-first order spelled
// as a LITERAL, deliberately NOT read out of runRowTiers: a yardstick derived
// from the thing being measured moves with it, which is precisely how the
// previous version of this test stayed green under a tier swap.
var canonicalRunRowTierOrder = []string{"R1", "R2", "R3", "R4", floorTierName}

func canonicalTierPosition(name string) int {
	for i, n := range canonicalRunRowTierOrder {
		if n == name {
			return i
		}
	}
	return -1
}

// TestBoundRunRow_TierOrderIsLeastActionableFirst pins the ORDER R1→R2→R3→R4,
// which the earlier version of this test only named. It accumulated tiers into
// an unordered map and closed on `len(seen) == 0`, already guaranteed by the
// preceding `out.Elisions == nil` fatal, so a refactor that swapped R2 before
// R1 or collapsed R1..R3 stayed green.
//
// Two phases, because neither alone bites on every reordering:
//
//	Phase A pins POSITION → (name, characteristic effect). Each tier's effect is
//	stated so that it DISCRIMINATES against its neighbours — position 0 must drop
//	the comment thread and leave the body, position 1 must cap the body and leave
//	the comment thread — so swapping two entries flips both checks.
//
//	Phase B is the budget sweep the concern asked for: record the settled tier at
//	each budget in a SLICE, generous → tight, and require the sequence to be
//	monotonically NON-DECREASING in canonical ladder position. A tighter budget
//	that settles at an EARLIER tier is the observable signature of a ladder whose
//	declared order and applied order disagree.
func TestBoundRunRow_TierOrderIsLeastActionableFirst(t *testing.T) {
	runID := uuid.NewString()

	// --- Phase A: position → (name, characteristic effect) -------------------
	positions := []struct {
		name   string
		effect string
		did    func(*Run) bool
	}{
		{"R1", "drop the comment thread and LEAVE the body uncapped", func(r *Run) bool {
			return r.IssueContext != nil && len(r.IssueContext.Comments) == 0 &&
				jsonEncodedLen(r.IssueContext.Body) > issueBodyTierCap
		}},
		{"R2", "cap the body and LEAVE the comment thread", func(r *Run) bool {
			return r.IssueContext != nil && len(r.IssueContext.Comments) > 0 &&
				jsonEncodedLen(r.IssueContext.Body) <= issueBodyTierCap
		}},
		{"R3", "drop the whole issue context", func(r *Run) bool { return r.IssueContext == nil }},
		{"R4", "drop the concern items and RETAIN the counts", func(r *Run) bool {
			return r.IssueContext != nil && r.Concerns != nil && len(r.Concerns.Items) == 0 && r.Concerns.Open == 7
		}},
	}
	if len(runRowTiers) != len(positions) {
		t.Fatalf("the ladder declares %d tiers, want the canonical %d (%v) — a collapsed or added tier changes the order this test pins",
			len(runRowTiers), len(positions), canonicalRunRowTierOrder[:len(positions)])
	}
	for i, want := range positions {
		if runRowTiers[i].name != want.name {
			t.Errorf("ladder position %d is tier %q, want %q (order: %v)", i, runRowTiers[i].name, want.name, canonicalRunRowTierOrder)
		}
		row := worstCaseIssueRun(runID)
		row.Concerns = &RunConcerns{Open: 7, ByState: map[string]int{"raised": 7}, Items: concernItems(7)}
		led := &elisionLedger{budget: 1, tier: runRowTiers[i].name}
		runRowTiers[i].apply([]*Run{&row}, runRowCtx{prefix: "run", runID: runID}, led)
		if !want.did(&row) {
			t.Errorf("ladder position %d (declared %q) did not %s — the applied order disagrees with the declared one",
				i, runRowTiers[i].name, want.effect)
		}
	}

	// --- Phase B: generous → tight sweep, recorded in ORDER ------------------
	// The generous end of the sweep is load-bearing, not padding. R1-alone and
	// R2-alone each reclaim a comparable slab of this fixture (~56 KB of comment
	// thread, ~72 KB of body), so a budget above BOTH residuals settles after the
	// FIRST tier alone — which is the only budget at which the settled tier's
	// name reveals which entry the ladder ran first. Below that both tiers always
	// apply and the sweep cannot distinguish their order at all.
	sweep := []int{96 * 1024, 80 * 1024, 64 * 1024, 30 * 1024, 20 * 1024, 12 * 1024, 6 * 1024, 5 * 1024, mcpConvergenceFloorBytes}
	observed := make([]string, 0, len(sweep))
	for _, budget := range sweep {
		row := worstCaseIssueRun(runID)
		row.Concerns = &RunConcerns{Open: 40, ByState: map[string]int{"raised": 40}, Items: concernItems(40)}
		out, err := boundOneRow(GetActiveRunOutput{Run: row}, budget)
		if err != nil {
			t.Fatalf("budget %d: %v", budget, err)
		}
		if n := marshalLen(t, out); n > budget {
			t.Errorf("budget %d: bounded row = %d bytes", budget, n)
		}
		if out.Run.ID == "" {
			t.Errorf("budget %d: the run id did not survive", budget)
		}
		if out.Elisions == nil {
			t.Fatalf("budget %d: no elisions block", budget)
		}
		observed = append(observed, out.Elisions.Tier)
		if out.Run.IssueContext != nil && len(out.Run.IssueContext.Comments) > 0 {
			t.Errorf("budget %d: tier %s kept the comment thread", budget, out.Elisions.Tier)
		}
		// The body assertion is keyed to the SETTLED TIER rather than applied
		// flat, which is strictly stronger than the flat form the sweep's old
		// budget list happened to make true: a settled R1 must leave the body
		// UNCAPPED (R2 is the tier that caps it, and a settled R1 means R2 never
		// ran), and every later tier must have capped it. A flat "always capped"
		// check cannot tell those two apart, which is exactly the blindness that
		// let a swapped ladder pass.
		bodyCapped := out.Run.IssueContext == nil || jsonEncodedLen(out.Run.IssueContext.Body) <= issueBodyTierCap
		if canonicalTierPosition(out.Elisions.Tier) == 0 && bodyCapped {
			t.Errorf("budget %d: settled at tier %s yet the issue body is ALREADY capped — position 0 is the comment tier, so a later tier's work ran early",
				budget, out.Elisions.Tier)
		}
		if canonicalTierPosition(out.Elisions.Tier) > 0 && !bodyCapped {
			t.Errorf("budget %d: tier %s left the issue body uncapped", budget, out.Elisions.Tier)
		}
	}

	t.Logf("generous->tight tier sweep: %v -> %v", sweep, observed)
	prev := -1
	for i, name := range observed {
		pos := canonicalTierPosition(name)
		if pos < 0 {
			t.Fatalf("budget %d settled at tier %q, which is not on the canonical ladder %v", sweep[i], name, canonicalRunRowTierOrder)
		}
		if pos < prev {
			t.Errorf("the sweep runs generous->tight, so the settled tier must never move BACKWARDS along %v; observed %v (budget %d went back to %q)",
				canonicalRunRowTierOrder, observed, sweep[i], name)
		}
		prev = pos
	}
	distinct := map[string]bool{}
	for _, name := range observed {
		distinct[name] = true
	}
	if len(distinct) < 2 {
		t.Fatalf("the sweep settled at ONE tier for every budget (%v) — the monotonicity assertion would be vacuous", observed)
	}
}

// TestCapPayloadStrings_IsEscapeAware pins A1's cap on the value the raw-byte
// helper would miss: a string of '<' bytes inflates 6x on encode.
func TestCapPayloadStrings_IsEscapeAware(t *testing.T) {
	payload := map[string]any{"nested": []any{map[string]any{"note": strings.Repeat("<", 4000)}}, "n": 42}
	capped, elided := capPayloadStrings(payload, auditPayloadTierCap)
	if elided == 0 {
		t.Fatal("nothing was capped")
	}
	raw, err := json.Marshal(capped)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if len(raw) > 4*auditPayloadTierCap {
		t.Errorf("capped payload = %d bytes, want a genuine encoded bound near %d", len(raw), auditPayloadTierCap)
	}
	if !strings.Contains(string(raw), `"n":42`) {
		t.Errorf("a non-string value was altered: %s", raw)
	}
}

// TestBoundedSurfaces_DegenerateInputs drives the ladders' defensive branches
// with inputs no live backend produces but the code must still define: an EMPTY
// page pushed over an absurdly small budget (so the floor runs with zero rows to
// project), and a row whose optional composites are all absent (so every tier
// finds its target missing and correctly records nothing).
func TestBoundedSurfaces_DegenerateInputs(t *testing.T) {
	t.Run("empty audit page at a 1-byte budget", func(t *testing.T) {
		out, err := boundListAuditOutput(ListAuditOutput{NextCursor: "cur-1"}, uuid.NewString(), "", fixedBudget(1))
		if err != nil {
			t.Fatalf("bound: %v", err)
		}
		if len(out.Items) != 0 {
			t.Errorf("the zero-item floor invented %d items", len(out.Items))
		}
		if out.NextCursor != "" {
			t.Errorf("the floor kept a cursor: %q", out.NextCursor)
		}
		if out.Elisions == nil || out.Elisions.Tier != floorTierName {
			t.Fatalf("want the floor tier, got %+v", out.Elisions)
		}
	})

	t.Run("empty runs page at a 1-byte budget", func(t *testing.T) {
		out, err := boundListRunsOutput(ListRunsOutput{NextCursor: "cur-1"}, fixedBudget(1))
		if err != nil {
			t.Fatalf("bound: %v", err)
		}
		if len(out.Items) != 0 || out.NextCursor != "" {
			t.Errorf("the zero-row floor returned %+v", out)
		}
	})

	t.Run("a row with no issue_context and no concerns", func(t *testing.T) {
		runID := uuid.NewString()
		out, err := boundOneRow(GetActiveRunOutput{Run: Run{
			ID: runID, Repo: "o/n", WorkflowID: "feature_change", State: "running",
		}}, 1)
		if err != nil {
			t.Fatalf("bound: %v", err)
		}
		// Every tier's target is absent, so no tier records anything and the
		// ladder falls straight through to the floor.
		if out.Elisions == nil || out.Elisions.Tier != floorTierName {
			t.Fatalf("want the floor tier, got %+v", out.Elisions)
		}
		for _, f := range out.Elisions.Fields {
			if !f.Aggregate {
				t.Errorf("a tier whose target was absent still recorded %+v", f)
			}
		}
		if out.Run.ID != runID {
			t.Errorf("the diagnosis core did not survive: %+v", out.Run)
		}
	})
}

// concernItems builds n open concerns with recognisable ids — the R4 tier's
// target. They are RETAINED through R1..R3 (only issue_context sheds there), so
// they are what a row's remaining bulk looks like once the issue is gone.
func concernItems(n int) []RunConcernItem {
	out := make([]RunConcernItem, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, RunConcernItem{
			ID:           uuid.NewString(),
			StageKind:    "implement",
			Severity:     "high",
			Category:     "correctness",
			State:        "raised",
			ShortSummary: strings.Repeat("a concern short summary that an operator recognises. ", 4),
		})
	}
	return out
}

// TestBoundRunRow_R4DropsConcernItems drives the tier the issue_context
// fixtures never reach: a row whose bulk is its OPEN CONCERN LIST, not its
// issue. It asserts the tier's observable effect (items gone, open + by_state
// retained) and that the stored pointer is the one-call gate view.
func TestBoundRunRow_R4DropsConcernItems(t *testing.T) {
	runID := uuid.NewString()
	run := Run{
		ID: runID, Repo: "kuhlman-labs/fishhawk", WorkflowID: "feature_change", State: "running",
		Concerns: &RunConcerns{Open: 120, ByState: map[string]int{"raised": 120}, Items: concernItems(120)},
	}
	budget := mcpResponseByteBudgetDefault
	if n := marshalLen(t, GetActiveRunOutput{Run: run}); n <= budget {
		t.Fatalf("fixture is only %d bytes — R4 would never be reached", n)
	}
	out, err := boundOneRow(GetActiveRunOutput{Run: run}, budget)
	if err != nil {
		t.Fatalf("bound: %v", err)
	}
	if n := marshalLen(t, out); n > budget {
		t.Errorf("bounded row = %d bytes, want <= %d", n, budget)
	}
	if out.Run.Concerns == nil {
		t.Fatal("R4 dropped the whole concerns block; open + by_state are RETAINED")
	}
	if len(out.Run.Concerns.Items) != 0 {
		t.Errorf("R4 kept %d concern items", len(out.Run.Concerns.Items))
	}
	if out.Run.Concerns.Open != 120 || out.Run.Concerns.ByState["raised"] != 120 {
		t.Errorf("R4 lost the retained counts: %+v", out.Run.Concerns)
	}
	var saw bool
	for _, f := range out.Elisions.Fields {
		if f.Field != "run.concerns.items" {
			continue
		}
		saw = true
		if f.Pointer != "fishhawk_get_gate_view(run_id="+runID+")" {
			t.Errorf("concerns pointer = %q, want the one-call gate view", f.Pointer)
		}
		if f.OmittedCount != 120 {
			t.Errorf("omitted_count = %d, want 120", f.OmittedCount)
		}
	}
	if !saw {
		t.Errorf("no elision named the dropped concern items: %+v", out.Elisions.Fields)
	}
}

// TestBoundListRuns_R4PointsAtTheEnumeration is the PAGE counterpart: no single
// row's gate view speaks for a page, so the stored pointer is the unbounded REST
// enumeration instead.
func TestBoundListRuns_R4PointsAtTheEnumeration(t *testing.T) {
	items := make([]Run, 0, 40)
	for i := 0; i < 40; i++ {
		items = append(items, Run{
			ID: uuid.NewString(), Repo: "kuhlman-labs/fishhawk", WorkflowID: "feature_change", State: "running",
			Concerns: &RunConcerns{Open: 20, ByState: map[string]int{"raised": 20}, Items: concernItems(20)},
		})
	}
	out, err := boundListRunsOutput(ListRunsOutput{Items: items}, fixedBudget(mcpResponseByteBudgetDefault))
	if err != nil {
		t.Fatalf("bound: %v", err)
	}
	var saw bool
	for _, f := range out.Elisions.Fields {
		if f.Field != "items[].concerns.items" {
			continue
		}
		saw = true
		if f.Pointer != "GET /v0/runs" {
			t.Errorf("page concerns pointer = %q, want the unbounded REST enumeration", f.Pointer)
		}
		if f.OmittedCount != 40*20 {
			t.Errorf("omitted_count = %d, want the sum across rows %d", f.OmittedCount, 40*20)
		}
	}
	if !saw && out.Elisions.Tier != floorTierName {
		t.Errorf("no elision named the dropped concern items: %+v", out.Elisions)
	}
}

// TestRunRowCtx_RowIDFallsBackToTheResponseRunID pins the fallback a row with
// no id of its own takes: the pointer must still name a run, never an empty
// path that retrieves nothing.
func TestRunRowCtx_RowIDFallsBackToTheResponseRunID(t *testing.T) {
	runID := uuid.NewString()
	run := worstCaseIssueRun(runID)
	run.ID = "" // e.g. a mirror that decoded without an id
	run.IssueContext.URL = ""
	out, err := boundRunRowOutput(GetActiveRunOutput{Run: run}, runID, fixedBudget(mcpResponseByteBudgetDefault),
		singleRunRows, singleRunSet)
	if err != nil {
		t.Fatalf("bound: %v", err)
	}
	want := "GET /v0/runs/" + runID
	for _, f := range out.Elisions.Fields {
		if f.Class == string(classOversizedCapable) && f.Pointer != want {
			t.Errorf("pointer = %q, want the response-level fallback %q", f.Pointer, want)
		}
	}
}
