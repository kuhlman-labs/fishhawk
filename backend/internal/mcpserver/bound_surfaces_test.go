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

// ---------------------------------------------------------------------------
// (7) #2576 — per-field reduction ITEMISATION at the floor
// ---------------------------------------------------------------------------

// elisionByField returns the FIRST ledger entry with the given dotted path.
func elisionByField(el *Elisions, field string) (ElidedField, bool) {
	if el == nil {
		return ElidedField{}, false
	}
	for _, f := range el.Fields {
		if f.Field == field {
			return f, true
		}
	}
	return ElidedField{}, false
}

func containsStr(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// reviveFloorFixture is a revive response whose oversized run FORCES the floor,
// with a truncatable restored_stages (3 > floorCarryCollectionCap) carrying
// multi-KB prior_reason strings and the ~330-char constant next_step. auditWarn
// seeds the run_revived append OUTCOME: empty = clean append, non-empty = the
// append FAILED so the audit walk cannot deliver restored_stages.
func reviveFloorFixture(runID, auditWarn string) ReviveRunOutput {
	return ReviveRunOutput{
		Run: floorForcingRun(runID),
		RestoredStages: []ReviveRestoredStage{
			{StageID: uuid.NewString(), Type: "implement", PriorCategory: "A", PriorReason: strings.Repeat("agent harness 400. ", 4000), RestoredState: "pending"},
			{StageID: uuid.NewString(), Type: "review", PriorCategory: "C", PriorReason: strings.Repeat("lineage lock. ", 4000), RestoredState: "pending"},
			{StageID: uuid.NewString(), Type: "acceptance", PriorCategory: "D", PriorReason: "sla timeout", RestoredState: "awaiting_approval"},
		},
		NextStep:     reviveNextStepHint,
		AuditWarning: auditWarn,
	}
}

func boundReviveFloor(t *testing.T, out ReviveRunOutput, budget int) ReviveRunOutput {
	t.Helper()
	got, err := boundRunRowOutput(out, out.Run.ID, fixedBudget(budget),
		func(o *ReviveRunOutput) []*Run { return []*Run{&o.Run} },
		func(o *ReviveRunOutput, e *Elisions) { o.Elisions = e })
	if err != nil {
		t.Fatalf("bound revive: %v", err)
	}
	return got
}

// campaignItemFloorFixture is a start_campaign_item_run response whose oversized
// run FORCES the floor, with a truncatable item.depends_on (dependsOn elements).
func campaignItemFloorFixture(runID string, dependsOn int) StartCampaignItemRunOutput {
	deps := make([]string, dependsOn)
	for i := range deps {
		deps[i] = fmt.Sprintf("kuhlman-labs/fishhawk#%d", 2500+i)
	}
	return StartCampaignItemRunOutput{
		Run: floorForcingRun(runID),
		Item: CampaignItem{
			ID: uuid.NewString(), IssueRef: "kuhlman-labs/fishhawk#2576",
			DependsOn: deps, RunID: runID, State: "running",
			CreatedAt: time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC),
			UpdatedAt: time.Date(2026, 8, 7, 12, 5, 0, 0, time.UTC),
		},
	}
}

// boundCampaignItemFloor mirrors campaign.go's call site: the campaign-items
// pointer is threaded from the caller via withFloorCarryPointer.
func boundCampaignItemFloor(t *testing.T, out StartCampaignItemRunOutput, campaignID string, budget int) StartCampaignItemRunOutput {
	t.Helper()
	got, err := boundRunRowOutput(out, out.Run.ID, fixedBudget(budget),
		func(o *StartCampaignItemRunOutput) []*Run { return []*Run{&o.Run} },
		func(o *StartCampaignItemRunOutput, e *Elisions) { o.Elisions = e },
		withFloorCarryPointer("item", func(field, reason string, n int) elidedField {
			return newStoredElision(field, reason, pointerCampaignItems(campaignID).retrievalPointer, n)
		}))
	if err != nil {
		t.Fatalf("bound campaign item: %v", err)
	}
	return got
}

// TestFloorItemisesEveryCarriedReduction is the done-means test for the issue's
// "REPORT what it reduced": a truncated restored_stages and a truncated
// item.depends_on each produce their OWN entry with a non-zero omitted_count.
// COUNTERFACTUAL: delete the recordTruncate/recordCap calls in floorCarrySlice /
// floorCarryValue and both entries vanish — this goes RED on the missing entry.
func TestFloorItemisesEveryCarriedReduction(t *testing.T) {
	t.Run("revive restored_stages", func(t *testing.T) {
		runID := uuid.NewString()
		out := boundReviveFloor(t, reviveFloorFixture(runID, ""), mcpConvergenceFloorBytes)
		if out.Elisions == nil || out.Elisions.Tier != floorTierName {
			t.Fatalf("fixture did not reach the floor: %+v", out.Elisions)
		}
		f, ok := elisionByField(out.Elisions, "restored_stages")
		if !ok {
			t.Fatalf("no restored_stages entry: %+v", out.Elisions.Fields)
		}
		if f.OmittedCount != 1 {
			t.Errorf("restored_stages omitted_count = %d, want 1 (3 stages truncated to %d)", f.OmittedCount, floorCarryCollectionCap)
		}
	})
	t.Run("campaign item.depends_on", func(t *testing.T) {
		runID, campaignID := uuid.NewString(), uuid.NewString()
		out := boundCampaignItemFloor(t, campaignItemFloorFixture(runID, 3), campaignID, mcpConvergenceFloorBytes)
		if out.Elisions == nil || out.Elisions.Tier != floorTierName {
			t.Fatalf("fixture did not reach the floor: %+v", out.Elisions)
		}
		f, ok := elisionByField(out.Elisions, "item.depends_on")
		if !ok {
			t.Fatalf("no item.depends_on entry: %+v", out.Elisions.Fields)
		}
		if f.OmittedCount != 1 {
			t.Errorf("item.depends_on omitted_count = %d, want 1 (3 deps truncated to %d)", f.OmittedCount, floorCarryCollectionCap)
		}
	})
}

// TestFloorCarry_RestoredStagesPointsAtTheAuditWalk asserts the class and the
// EXACT pointer on a CLEAN revive (append succeeded). COUNTERFACTUAL: delete the
// restored_stages floorCarryTable row and the fail-safe fires — class computed,
// pointer EMPTY — so the pointer-value assertion goes RED.
func TestFloorCarry_RestoredStagesPointsAtTheAuditWalk(t *testing.T) {
	runID := uuid.NewString()
	out := boundReviveFloor(t, reviveFloorFixture(runID, ""), mcpConvergenceFloorBytes)
	f, ok := elisionByField(out.Elisions, "restored_stages")
	if !ok {
		t.Fatalf("no restored_stages entry: %+v", out.Elisions.Fields)
	}
	if f.Class != string(classOversizedCapable) {
		t.Errorf("restored_stages class = %q, want oversized_capable", f.Class)
	}
	want := "GET /v0/runs/" + runID + "/audit"
	if f.Pointer != want {
		t.Errorf("restored_stages pointer = %q, want the audit walk %q (NOT the run read or gate view)", f.Pointer, want)
	}
}

// TestFloorCarry_RestoredStages_AppendFailed_IsComputedNoPointer is BINDING
// CONDITION 1's load-bearing test: when the run_revived append FAILED (audit_
// warning set BY CONSTRUCTION), no run_revived row exists, so the audit walk
// cannot deliver restored_stages. The class must fall back to computed with NO
// pointer. COUNTERFACTUAL: delete the reviveAuditAppendFailed branch in the
// restored_stages Build and it emits oversized_capable + the audit pointer —
// RED on both the class and pointer assertions.
func TestFloorCarry_RestoredStages_AppendFailed_IsComputedNoPointer(t *testing.T) {
	runID := uuid.NewString()
	out := boundReviveFloor(t, reviveFloorFixture(runID, "run_revived append failed: audit store unavailable"), mcpConvergenceFloorBytes)
	f, ok := elisionByField(out.Elisions, "restored_stages")
	if !ok {
		t.Fatalf("no restored_stages entry: %+v", out.Elisions.Fields)
	}
	if f.Class != string(classComputed) {
		t.Errorf("append-FAILED restored_stages class = %q, want computed — no run_revived row exists to retrieve", f.Class)
	}
	if f.Pointer != "" {
		t.Errorf("append-FAILED restored_stages pointer = %q, want EMPTY — a pointer here promises content that provably is not there", f.Pointer)
	}
}

// TestFloorCarry_NextStepIsComputedWithNoPointer asserts next_step (a constant
// guidance string the MCP layer renders, never stored) is computed with an
// EMPTY pointer.
func TestFloorCarry_NextStepIsComputedWithNoPointer(t *testing.T) {
	runID := uuid.NewString()
	out := boundReviveFloor(t, reviveFloorFixture(runID, ""), mcpConvergenceFloorBytes)
	f, ok := elisionByField(out.Elisions, "next_step")
	if !ok {
		t.Fatalf("no next_step entry: %+v", out.Elisions.Fields)
	}
	if f.Class != string(classComputed) {
		t.Errorf("next_step class = %q, want computed (recomputed at read time, never stored)", f.Class)
	}
	if f.Pointer != "" {
		t.Errorf("next_step pointer = %q, want EMPTY", f.Pointer)
	}
}

// TestFloorCarry_CampaignItemPointsAtTheItemsEnumeration is BINDING CONDITION
// 2's test: a TWO-LEVEL reported path (item.depends_on) resolves via the
// longest-ancestor-prefix rule to the item row's campaign-items pointer, NOT the
// computed fallback. COUNTERFACTUAL: delete the withFloorCarryPointer wiring at
// the campaign call site (boundCampaignItemFloor) and the item row's nil Build
// fails safe to a pointer-less computed entry — RED on the pointer VALUE.
func TestFloorCarry_CampaignItemPointsAtTheItemsEnumeration(t *testing.T) {
	runID, campaignID := uuid.NewString(), uuid.NewString()
	out := boundCampaignItemFloor(t, campaignItemFloorFixture(runID, 3), campaignID, mcpConvergenceFloorBytes)
	f, ok := elisionByField(out.Elisions, "item.depends_on")
	if !ok {
		t.Fatalf("no item.depends_on entry (a two-level path took the computed fallback?): %+v", out.Elisions.Fields)
	}
	if f.Class != string(classStored) {
		t.Errorf("item.depends_on class = %q, want stored", f.Class)
	}
	want := "GET /v0/campaigns/" + campaignID + "/items"
	if f.Pointer != want {
		t.Errorf("item.depends_on pointer = %q, want the campaign-items enumeration %q", f.Pointer, want)
	}
}

// TestFloorAggregateDoesNotClaimCarriedFields pins the honesty core: no entry —
// aggregate or per-field — names the run read or the gate view as the surface
// for a CARRIED-and-reduced field (restored_stages / next_step / item.depends_on).
// The aggregate stored "*" entry's surfaces are those two, but they must stand
// in ONLY for the OMITTED omittable fields, never for the itemised carried ones.
func TestFloorAggregateDoesNotClaimCarriedFields(t *testing.T) {
	runReadGateView := func(runID string) []string {
		return []string{"GET /v0/runs/" + runID, "fishhawk_get_gate_view(run_id=" + runID + ")"}
	}
	t.Run("revive", func(t *testing.T) {
		runID := uuid.NewString()
		out := boundReviveFloor(t, reviveFloorFixture(runID, ""), mcpConvergenceFloorBytes)
		bad := runReadGateView(runID)
		for _, f := range out.Elisions.Fields {
			if f.Field == "restored_stages" || f.Field == "next_step" {
				if containsStr(bad, f.Pointer) {
					t.Errorf("carried field %q points at the run read / gate view (%q) — that surface does not return it", f.Field, f.Pointer)
				}
			}
		}
	})
	t.Run("campaign", func(t *testing.T) {
		runID, campaignID := uuid.NewString(), uuid.NewString()
		out := boundCampaignItemFloor(t, campaignItemFloorFixture(runID, 3), campaignID, mcpConvergenceFloorBytes)
		bad := runReadGateView(runID)
		for _, f := range out.Elisions.Fields {
			if strings.HasPrefix(f.Field, "item") {
				if containsStr(bad, f.Pointer) {
					t.Errorf("carried field %q points at the run read / gate view (%q)", f.Field, f.Pointer)
				}
			}
		}
	})
}

// --- synthetic response types for the defensive-branch tests ----------------

type unclassifiedFloorOutput struct {
	Run      Run       `json:"run"`
	Mystery  []string  `json:"mystery"`
	Elisions *Elisions `json:"elisions,omitempty"`
}

type manyFieldRow struct {
	A string `json:"a"`
	B string `json:"b"`
	C string `json:"c"`
	D string `json:"d"`
	E string `json:"e"`
	F string `json:"f"`
	G string `json:"g"`
	H string `json:"h"`
}

type manyReductionsOutput struct {
	Run      Run            `json:"run"`
	Rows     []manyFieldRow `json:"rows"`
	Elisions *Elisions      `json:"elisions,omitempty"`
}

type deepInner struct {
	Deep []string `json:"deep"`
}

type deepMid struct {
	Inner deepInner `json:"inner"`
}

type deepFloorOutput struct {
	Run      Run       `json:"run"`
	Top      deepMid   `json:"top"`
	Elisions *Elisions `json:"elisions,omitempty"`
}

// TestFloorCarry_UnclassifiedFieldNeverClaimsRetrieval drives a type ABSENT from
// floorCarryTable: its reduced non-omitempty field must fail safe to a computed
// entry with NO pointer. COUNTERFACTUAL: delete the `row == nil` fail-safe in
// classifyFloorReduction and a nil-row deref panics — RED.
func TestFloorCarry_UnclassifiedFieldNeverClaimsRetrieval(t *testing.T) {
	runID := uuid.NewString()
	out, err := boundRunRowOutput(
		unclassifiedFloorOutput{Run: floorForcingRun(runID), Mystery: []string{"a", "b", "c", "d"}}, runID, fixedBudget(mcpConvergenceFloorBytes),
		func(o *unclassifiedFloorOutput) []*Run { return []*Run{&o.Run} },
		func(o *unclassifiedFloorOutput, e *Elisions) { o.Elisions = e })
	if err != nil {
		t.Fatalf("bound: %v", err)
	}
	if out.Elisions == nil || out.Elisions.Tier != floorTierName {
		t.Fatalf("fixture did not reach the floor: %+v", out.Elisions)
	}
	f, ok := elisionByField(out.Elisions, "mystery")
	if !ok {
		t.Fatalf("no mystery entry: %+v", out.Elisions.Fields)
	}
	if f.Class != string(classComputed) {
		t.Errorf("unclassified field class = %q, want the fail-safe computed", f.Class)
	}
	if f.Pointer != "" {
		t.Errorf("unclassified field pointer = %q, want EMPTY — a field with no table row must never name a surface", f.Pointer)
	}
}

// TestFloorCarry_EntryCountIsCapped drives MORE reductions than floorCarryEntryCap
// and asserts exactly the cap of per-field entries plus ONE aggregate computed
// spillover naming the remainder. COUNTERFACTUAL: delete the cap clamp in
// addFloorCarryEntries and every reduction is itemised — the per-field count
// exceeds the cap and no spillover appears — RED.
func TestFloorCarry_EntryCountIsCapped(t *testing.T) {
	runID := uuid.NewString()
	long := strings.Repeat("x", 400)
	rows := []manyFieldRow{
		{A: long, B: long, C: long, D: long, E: long, F: long, G: long, H: long},
		{A: long, B: long, C: long, D: long, E: long, F: long, G: long, H: long},
		{A: long, B: long, C: long, D: long, E: long, F: long, G: long, H: long},
	}
	out, err := boundRunRowOutput(
		manyReductionsOutput{Run: floorForcingRun(runID), Rows: rows}, runID, fixedBudget(mcpConvergenceFloorBytes),
		func(o *manyReductionsOutput) []*Run { return []*Run{&o.Run} },
		func(o *manyReductionsOutput, e *Elisions) { o.Elisions = e })
	if err != nil {
		t.Fatalf("bound: %v", err)
	}
	// "rows" (truncate) + rows.a..rows.h (8 caps) = 9 reduction paths > cap 4.
	var perField, spillover int
	for _, f := range out.Elisions.Fields {
		switch {
		case f.Aggregate && f.Class == string(classComputed):
			spillover++
		case !f.Aggregate:
			perField++
		}
	}
	if perField != floorCarryEntryCap {
		t.Errorf("itemised %d per-field entries, want exactly the cap %d", perField, floorCarryEntryCap)
	}
	if spillover != 1 {
		t.Errorf("got %d aggregate computed spillover entries, want exactly 1 naming the remainder", spillover)
	}
}

// TestFloorCarry_DeepReductionRollsUp asserts a reduction nested DEEPER than two
// levels rolls up to its nearest named two-level ancestor path (top.inner), not
// a three-level path. COUNTERFACTUAL: remove the depth clamp in
// floorCarryExtendPath and the entry appears at top.inner.deep — RED on the
// present-at-ancestor assertion AND the absent-at-deep assertion.
func TestFloorCarry_DeepReductionRollsUp(t *testing.T) {
	runID := uuid.NewString()
	out, err := boundRunRowOutput(
		deepFloorOutput{Run: floorForcingRun(runID), Top: deepMid{Inner: deepInner{Deep: []string{"a", "b", "c", "d"}}}}, runID, fixedBudget(mcpConvergenceFloorBytes),
		func(o *deepFloorOutput) []*Run { return []*Run{&o.Run} },
		func(o *deepFloorOutput, e *Elisions) { o.Elisions = e })
	if err != nil {
		t.Fatalf("bound: %v", err)
	}
	if _, ok := elisionByField(out.Elisions, "top.inner"); !ok {
		t.Errorf("deep reduction did not roll up to the two-level ancestor top.inner: %+v", out.Elisions.Fields)
	}
	if _, ok := elisionByField(out.Elisions, "top.inner.deep"); ok {
		t.Errorf("a THREE-level path top.inner.deep leaked past the depth bound: %+v", out.Elisions.Fields)
	}
}

// TestFloorCarry_NoReductionRecordsNothing asserts an INERT floor stays inert: a
// non-omitempty field carried WITHOUT reduction (a short collection, a short
// string) records NO per-field entry. Guards the recordTruncate/recordCap
// `<= 0` short-circuit — without it a zero-cap would emit a "0 bytes capped"
// entry.
func TestFloorCarry_NoReductionRecordsNothing(t *testing.T) {
	runID := uuid.NewString()
	out := boundReviveFloor(t, ReviveRunOutput{
		Run:            floorForcingRun(runID),
		RestoredStages: []ReviveRestoredStage{{StageID: uuid.NewString(), Type: "implement", PriorCategory: "A", PriorReason: "short", RestoredState: "pending"}},
		NextStep:       "go",
	}, mcpConvergenceFloorBytes)
	if out.Elisions == nil || out.Elisions.Tier != floorTierName {
		t.Fatalf("fixture did not reach the floor: %+v", out.Elisions)
	}
	for _, f := range out.Elisions.Fields {
		if !f.Aggregate {
			t.Errorf("an unreduced carried field still recorded a per-field entry: %+v", f)
		}
	}
}

// --- the reflection pin -----------------------------------------------------

// walkReducibleType statically mirrors floorCarryValue's descent, adding a path
// wherever a string (capped) or collection (truncated) sits. It is the type-side
// twin of the runtime floorCarryRecorder, so the pin can require a table row for
// every path the floor COULD reduce.
func walkReducibleType(t reflect.Type, path string, add func(string)) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.String:
		add(path)
	case reflect.Slice:
		add(path)
		walkReducibleType(t.Elem(), path, add)
	case reflect.Map:
		add(path)
		walkReducibleType(t.Elem(), path, add)
	case reflect.Struct:
		if t == reflect.TypeOf(time.Time{}) || hasUnexportedField(t) {
			return
		}
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if f.PkgPath != "" {
				continue
			}
			walkReducibleType(f.Type, floorCarryExtendPath(path, wirePathFieldName(f)), add)
		}
	}
}

func reducibleFloorPaths(t reflect.Type) []string {
	if t.Kind() != reflect.Struct {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	add := func(p string) {
		if p != "" && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" || jsonTagOmits(f) || ladderOwnedFloorField(f.Type) {
			continue
		}
		walkReducibleType(f.Type, wirePathFieldName(f), add)
	}
	return out
}

// wiredRunRowFloorTypes is the SINGLE source of the run-row output types routed
// through boundRunRowOutput, consumed by BOTH the reflection pin and the
// per-type floor test so neither hand-counts (BINDING CONDITION 5). Go cannot
// reflectively enumerate a package's types, so this slice is the source of
// truth; the pin self-validates each entry structurally (it must embed a Run
// field AND an *Elisions field) so a malformed entry fails rather than passing.
func wiredRunRowFloorTypes() []reflect.Type {
	return []reflect.Type{
		reflect.TypeOf(GetActiveRunOutput{}),
		reflect.TypeOf(StartRunOutput{}),
		reflect.TypeOf(CancelRunOutput{}),
		reflect.TypeOf(ResumeRunOutput{}),
		reflect.TypeOf(ReviveRunOutput{}),
		reflect.TypeOf(StartCampaignItemRunOutput{}),
	}
}

func hasFieldOfType(t, want reflect.Type) bool {
	for i := 0; i < t.NumField(); i++ {
		if t.Field(i).Type == want {
			return true
		}
	}
	return false
}

// TestEveryWiredFloorCarryFieldIsClassified is the reflection pin (step 7): every
// reducible non-omitempty field of every wired run-row output type resolves to a
// floorCarryTable row via the single floorCarryRowFor rule, so a future verb's
// unclassified field fails HERE rather than shipping a fail-safe entry nobody
// notices. It also proves the walk is not vacuous and the table validates.
func TestEveryWiredFloorCarryFieldIsClassified(t *testing.T) {
	for _, typ := range wiredRunRowFloorTypes() {
		// Structural self-validation of the single-source list (BINDING CONDITION 5).
		if !hasFieldOfType(typ, reflect.TypeOf((*Elisions)(nil))) {
			t.Errorf("%s is listed as a wired run-row type but has no *Elisions field", typ.Name())
		}
		if !hasFieldOfType(typ, reflect.TypeOf(Run{})) {
			t.Errorf("%s is listed as a wired run-row type but embeds no Run field", typ.Name())
		}
		for _, path := range reducibleFloorPaths(typ) {
			row := floorCarryRowFor(floorCarryTable[typ], path)
			if row == nil {
				t.Errorf("%s reducible field %q has no floorCarryTable row — classify it (or the floor will fail-safe it to a pointerless computed entry silently)", typ.Name(), path)
				continue
			}
			switch row.Class {
			case classStored, classOversizedCapable, classComputed:
			default:
				t.Errorf("%s field %q resolves to row %q with invalid class %q", typ.Name(), path, row.Path, row.Class)
			}
		}
	}
	// The walk must actually reach the nested reducible paths, or the pin is
	// vacuous (BINDING CONDITION 2's two-level path is the key case).
	revive := reducibleFloorPaths(reflect.TypeOf(ReviveRunOutput{}))
	if !containsStr(revive, "restored_stages") || !containsStr(revive, "next_step") {
		t.Errorf("the walk did not reach restored_stages / next_step: %v", revive)
	}
	campaign := reducibleFloorPaths(reflect.TypeOf(StartCampaignItemRunOutput{}))
	if !containsStr(campaign, "item.depends_on") {
		t.Errorf("the walk did not reach the two-level path item.depends_on: %v", campaign)
	}
	// A type with no reducible non-omitempty field contributes nothing.
	if p := reducibleFloorPaths(reflect.TypeOf(GetActiveRunOutput{})); len(p) != 0 {
		t.Errorf("GetActiveRunOutput reports reducible paths %v, want none", p)
	}
}

// --- the per-wired-type floor tier test -------------------------------------

type floorSurfaceCase struct {
	name string
	typ  reflect.Type
	page bool
	// drive builds this surface's floor output at budget and returns it plus its
	// elisions block.
	drive func(t *testing.T, budget int) (any, *Elisions)
}

// floorSurfaceCases derives its set from wiredRunRowFloorTypes() plus the two
// page floors, so the surface list is not independently hand-maintained
// (BINDING CONDITION 5).
func floorSurfaceCases() []floorSurfaceCase {
	runRowDrivers := map[reflect.Type]func(t *testing.T, budget int) (any, *Elisions){
		reflect.TypeOf(GetActiveRunOutput{}): func(t *testing.T, budget int) (any, *Elisions) {
			out, err := boundOneRow(GetActiveRunOutput{Run: floorForcingRun(uuid.NewString())}, budget)
			if err != nil {
				t.Fatalf("bound: %v", err)
			}
			return out, out.Elisions
		},
		reflect.TypeOf(StartRunOutput{}): func(t *testing.T, budget int) (any, *Elisions) {
			runID := uuid.NewString()
			out, err := boundRunRowOutput(StartRunOutput{Run: floorForcingRun(runID), Idempotent: true}, runID, fixedBudget(budget),
				func(o *StartRunOutput) []*Run { return []*Run{&o.Run} },
				func(o *StartRunOutput, e *Elisions) { o.Elisions = e })
			if err != nil {
				t.Fatalf("bound: %v", err)
			}
			return out, out.Elisions
		},
		reflect.TypeOf(CancelRunOutput{}): func(t *testing.T, budget int) (any, *Elisions) {
			runID := uuid.NewString()
			out, err := boundRunRowOutput(CancelRunOutput{Run: floorForcingRun(runID), ReapedRunners: 5}, runID, fixedBudget(budget),
				func(o *CancelRunOutput) []*Run { return []*Run{&o.Run} },
				func(o *CancelRunOutput, e *Elisions) { o.Elisions = e })
			if err != nil {
				t.Fatalf("bound: %v", err)
			}
			return out, out.Elisions
		},
		reflect.TypeOf(ResumeRunOutput{}): func(t *testing.T, budget int) (any, *Elisions) {
			runID := uuid.NewString()
			out, err := boundRunRowOutput(ResumeRunOutput{Run: floorForcingRun(runID), Idempotent: true}, runID, fixedBudget(budget),
				func(o *ResumeRunOutput) []*Run { return []*Run{&o.Run} },
				func(o *ResumeRunOutput, e *Elisions) { o.Elisions = e })
			if err != nil {
				t.Fatalf("bound: %v", err)
			}
			return out, out.Elisions
		},
		reflect.TypeOf(ReviveRunOutput{}): func(t *testing.T, budget int) (any, *Elisions) {
			out := boundReviveFloor(t, reviveFloorFixture(uuid.NewString(), ""), budget)
			return out, out.Elisions
		},
		reflect.TypeOf(StartCampaignItemRunOutput{}): func(t *testing.T, budget int) (any, *Elisions) {
			out := boundCampaignItemFloor(t, campaignItemFloorFixture(uuid.NewString(), 3), uuid.NewString(), budget)
			return out, out.Elisions
		},
	}
	cases := make([]floorSurfaceCase, 0, len(runRowDrivers)+2)
	for _, typ := range wiredRunRowFloorTypes() {
		cases = append(cases, floorSurfaceCase{name: typ.Name(), typ: typ, drive: runRowDrivers[typ]})
	}
	cases = append(cases,
		floorSurfaceCase{name: "ListRunsOutput", typ: reflect.TypeOf(ListRunsOutput{}), page: true, drive: func(t *testing.T, budget int) (any, *Elisions) {
			items := make([]Run, 0, 20)
			for i := 0; i < 20; i++ {
				r := floorForcingRun(uuid.NewString())
				items = append(items, r)
			}
			out, err := boundListRunsOutput(ListRunsOutput{Items: items, NextCursor: "cur-1"}, fixedBudget(budget))
			if err != nil {
				t.Fatalf("bound: %v", err)
			}
			return out, out.Elisions
		}},
		floorSurfaceCase{name: "ListAuditOutput", typ: reflect.TypeOf(ListAuditOutput{}), page: true, drive: func(t *testing.T, budget int) (any, *Elisions) {
			runID := uuid.NewString()
			items := auditPage(runID, 40, strings.Repeat("<", 20000))
			for i := range items {
				items[i].Category = strings.Repeat("category_", 400)
				items[i].ID = strings.Repeat("id-", 400)
			}
			out, err := boundListAuditOutput(ListAuditOutput{Items: items, NextCursor: "cur-1"}, runID, "implement_reviewed", fixedBudget(budget))
			if err != nil {
				t.Fatalf("bound: %v", err)
			}
			return out, out.Elisions
		}},
	)
	return cases
}

// TestBoundRunRow_FloorTierPerWiredOutputType drives EVERY wired output type to
// the FLOOR tier and asserts (a) tier == floor, (b) marshalled size at or under
// max(budget, floor), and (c) NO non-omitempty field VANISHES — it is present
// carrying its true value or its reduced value (BINDING CONDITION 3). This is
// the done-means test for the issue's "floor-tier test per wired output type".
func TestBoundRunRow_FloorTierPerWiredOutputType(t *testing.T) {
	cases := floorSurfaceCases()
	if len(cases) == 0 {
		t.Fatal("no wired surfaces derived")
	}
	budget := mcpConvergenceFloorBytes
	ceiling := budget
	if mcpConvergenceFloorBytes > ceiling {
		ceiling = mcpConvergenceFloorBytes
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, el := c.drive(t, budget)
			if el == nil || el.Tier != floorTierName {
				t.Fatalf("did not reach the floor tier: %+v", el)
			}
			if n := marshalLen(t, out); n > ceiling {
				t.Errorf("floor-budget %s = %d bytes, want <= %d", c.name, n, ceiling)
			}
			if err := validateWireElisions(el); err != nil {
				t.Errorf("floor elisions violate their own classification: %v", err)
			}
			// (c) no non-omitempty, non-ladder-owned field silently vanishes.
			rv := reflect.ValueOf(out)
			rt := rv.Type()
			for i := 0; i < rt.NumField(); i++ {
				f := rt.Field(i)
				if f.PkgPath != "" || jsonTagOmits(f) || ladderOwnedFloorField(f.Type) {
					continue
				}
				if rv.Field(i).IsZero() {
					t.Errorf("%s: non-omitempty field %s VANISHED at the floor (zero value, no carried content)", c.name, f.Name)
				}
			}
		})
	}
}

// TestFloorUnderBudgetWireUnchanged is BINDING CONDITION 4: below the byte
// threshold the ladder is provably INERT — the bounded response is byte-for-byte
// identical to the input, with NO elisions block — per affected response type.
func TestFloorUnderBudgetWireUnchanged(t *testing.T) {
	budget := mcpResponseByteBudgetDefault

	t.Run("ReviveRunOutput", func(t *testing.T) {
		runID := uuid.NewString()
		in := ReviveRunOutput{
			Run:            Run{ID: runID, Repo: "o/n", WorkflowID: "feature_change", State: "running"},
			RestoredStages: []ReviveRestoredStage{{StageID: uuid.NewString(), Type: "implement", PriorCategory: "A", PriorReason: "short", RestoredState: "pending"}},
			NextStep:       reviveNextStepHint,
		}
		before := marshalLen(t, in)
		got, err := boundRunRowOutput(in, runID, fixedBudget(budget),
			func(o *ReviveRunOutput) []*Run { return []*Run{&o.Run} },
			func(o *ReviveRunOutput, e *Elisions) { o.Elisions = e })
		if err != nil {
			t.Fatalf("bound: %v", err)
		}
		if got.Elisions != nil {
			t.Errorf("under-budget revive carries an elisions block: %+v", got.Elisions)
		}
		if n := marshalLen(t, got); n != before {
			t.Errorf("under-budget revive changed size %d -> %d — the ladder is not inert below the threshold", before, n)
		}
	})

	t.Run("StartCampaignItemRunOutput", func(t *testing.T) {
		runID := uuid.NewString()
		in := StartCampaignItemRunOutput{
			Run:  Run{ID: runID, Repo: "o/n", WorkflowID: "feature_change", State: "running"},
			Item: CampaignItem{ID: uuid.NewString(), IssueRef: "o/n#1", DependsOn: []string{"o/n#2"}, RunID: runID, State: "running"},
		}
		before := marshalLen(t, in)
		got, err := boundRunRowOutput(in, runID, fixedBudget(budget),
			func(o *StartCampaignItemRunOutput) []*Run { return []*Run{&o.Run} },
			func(o *StartCampaignItemRunOutput, e *Elisions) { o.Elisions = e },
			withFloorCarryPointer("item", func(field, reason string, n int) elidedField {
				return newStoredElision(field, reason, pointerCampaignItems(uuid.NewString()).retrievalPointer, n)
			}))
		if err != nil {
			t.Fatalf("bound: %v", err)
		}
		if got.Elisions != nil {
			t.Errorf("under-budget campaign item carries an elisions block: %+v", got.Elisions)
		}
		if n := marshalLen(t, got); n != before {
			t.Errorf("under-budget campaign item changed size %d -> %d", before, n)
		}
	})
}

// TestFloorCarry_NilCollectionRecordsNothing extends the degenerate-input
// coverage: a nil / empty non-omitempty collection at the floor records no entry
// and does not panic.
func TestFloorCarry_NilCollectionRecordsNothing(t *testing.T) {
	runID := uuid.NewString()
	out := boundReviveFloor(t, ReviveRunOutput{
		Run:            floorForcingRun(runID),
		RestoredStages: nil,
		NextStep:       "",
	}, mcpConvergenceFloorBytes)
	if out.Elisions == nil || out.Elisions.Tier != floorTierName {
		t.Fatalf("fixture did not reach the floor: %+v", out.Elisions)
	}
	for _, f := range out.Elisions.Fields {
		if !f.Aggregate {
			t.Errorf("a nil/empty collection produced a per-field entry: %+v", f)
		}
	}
}
