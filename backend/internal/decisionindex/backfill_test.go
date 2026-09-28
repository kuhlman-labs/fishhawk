package decisionindex

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// assertRowsEqual compares complete rows: DecidedAt by instant, every other
// column by deep equality.
func assertRowsEqual(t *testing.T, want, got []Row) {
	t.Helper()
	if len(want) != len(got) {
		t.Fatalf("row count = %d, want %d (got %v, want %v)", len(got), len(want), seqsOf(got), seqsOf(want))
	}
	for i := range want {
		w, g := want[i], got[i]
		if !w.DecidedAt.Equal(g.DecidedAt) {
			t.Errorf("row %d decided_at = %v, want %v", w.SourceSequence, g.DecidedAt, w.DecidedAt)
		}
		w.DecidedAt, g.DecidedAt = time.Time{}, time.Time{}
		if !reflect.DeepEqual(w, g) {
			t.Errorf("row %d differs:\n got  %+v\n want %+v", w.SourceSequence, g, w)
		}
	}
}

// seedAllNine appends one entry of each of the nine decision-bearing
// categories across both runs, interleaved with non-decision entries, and a
// review concern + plan artifacts for the joins. It returns the decision
// entries' sequences by category.
func seedAllNine(t *testing.T, f *chainFixture) map[string]int64 {
	t.Helper()
	base := time.Now().UTC().Add(-time.Hour)
	f.seedPlan(t, f.planStageA, base, "old.go")
	f.seedPlan(t, f.planStageA, base.Add(time.Minute), "b.go", "a.go", "b.go")
	f.seedPlan(t, f.planStgB, base, "gadget.go")
	concern := f.seedConcern(t, f.runA, f.implStageA, "high", "Test Coverage")

	seqs := map[string]int64{}
	add := func(runID uuid.UUID, stage *uuid.UUID, cat string, p map[string]any) {
		e := f.appendEntry(t, runID, stage, cat, p)
		if IsDecisionBearing(cat) {
			seqs[cat] = e.Sequence
		}
	}
	add(f.runA, nil, "run_started", map[string]any{})
	add(f.runA, &f.planStageA, "approval_submitted", map[string]any{"decision": "reject", "reject_class": "scope", "rejection_comment": "too wide", "delegated": "rule-1"})
	add(f.runA, &f.planStageA, "plan_reviewed", map[string]any{"verdict": "approve"})
	add(f.runA, &f.implStageA, "concern_waived", map[string]any{"concern_id": concern.String(), "reason": "accepted", "severity": "low", "category": "perf"})
	add(f.runA, &f.implStageA, "concern_deferred", map[string]any{"concern_id": concern.String(), "reason": "later", "category": "bug", "severity": "medium"})
	// No category/severity in the payload: joined from review_concerns.
	add(f.runA, &f.implStageA, "concern_addressed_by_condition", map[string]any{"concern_id": concern.String(), "verdict": "approve"})
	add(f.runA, &f.implStageA, "scope_amendment_decided", map[string]any{"decision": "approved", "reason": "coupled test"})
	add(f.runA, &f.implStageA, "stage_completed", map[string]any{})
	add(f.runA, &f.implStageA, "acceptance_triage_arbitrated", map[string]any{"verdict": "pass", "reason": "wrong tree", "delegated": true})
	add(f.runA, &f.implStageA, "merge_verdict_recorded", map[string]any{"verdict": "merge"})
	add(f.runB, &f.planStgB, "clarification_answered", map[string]any{"comment": "use v2"})
	add(f.runB, &f.planStgB, "grooming_disposition_recorded", map[string]any{"verdict": "keep", "note": "n"})
	if len(seqs) != 9 {
		t.Fatalf("seeded %d decision categories, want 9", len(seqs))
	}
	return seqs
}

// TestBackfill_RebuildIsByteIdentical is the cross-layer proof: a real chain
// through the migrated schema, backfilled, snapshotted, truncated and rebuilt,
// yields the SAME complete rows (#3730 approval condition 4 — there is no
// indexed_at to exclude), each citing its source entry's entry_hash. The first
// pass pages at 2 so a paging defect would also surface as a difference.
func TestBackfill_RebuildIsByteIdentical(t *testing.T) {
	f := newChainFixture(t)
	ctx := context.Background()
	seqs := seedAllNine(t, f)
	s := NewStore(f.pool)

	sum, err := Backfill(ctx, f.pool, Options{PageSize: 2})
	if err != nil {
		t.Fatalf("first backfill: %v", err)
	}
	if sum.EntriesScanned != 9 || sum.RowsWritten != 9 || sum.GapsRemaining != 0 {
		t.Errorf("summary = %+v, want 9 scanned / 9 written / 0 gaps", sum)
	}
	first, err := s.List(ctx, ListFilter{})
	if err != nil {
		t.Fatal(err)
	}

	sum, err = Backfill(ctx, f.pool, Options{Rebuild: true})
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if !sum.Rebuild || sum.RowsWritten != 9 {
		t.Errorf("rebuild summary = %+v, want rebuild with 9 rows", sum)
	}
	second, err := s.List(ctx, ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	assertRowsEqual(t, first, second)

	// Every row cites its source entry's hash, and only decision entries
	// produced rows (the seeded non-decision entries did not).
	hashes := map[int64]string{}
	rows, err := f.pool.Query(ctx, `SELECT sequence, entry_hash FROM audit_entries`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var seq int64
		var h string
		if err := rows.Scan(&seq, &h); err != nil {
			t.Fatal(err)
		}
		hashes[seq] = h
	}
	rows.Close()
	bySeq := map[int64]Row{}
	for _, r := range second {
		if r.SourceEntryHash != hashes[r.SourceSequence] || r.SourceEntryHash == "" {
			t.Errorf("row %d source_entry_hash = %q, want the entry's %q", r.SourceSequence, r.SourceEntryHash, hashes[r.SourceSequence])
		}
		bySeq[r.SourceSequence] = r
	}
	for cat, seq := range seqs {
		r, ok := bySeq[seq]
		if !ok {
			t.Errorf("%s (sequence %d) has no row", cat, seq)
			continue
		}
		if want, _ := ClassFor(cat); r.DecisionClass != want {
			t.Errorf("%s class = %q, want %q", cat, r.DecisionClass, want)
		}
	}

	// The joins landed.
	appr := bySeq[seqs["approval_submitted"]]
	if appr.Repo != "acme/widgets" || appr.DoctrineVersion != "sha-a" || appr.StageKind != "plan" ||
		appr.RejectClass != "scope" || appr.Outcome != "reject" || !appr.Delegated ||
		appr.AccountID == nil || *appr.AccountID != f.accountA || appr.ReasonKey != "rejection_comment" {
		t.Errorf("approval row = %+v", appr)
	}
	if !reflect.DeepEqual(appr.TouchedPaths, []string{"a.go", "b.go"}) {
		t.Errorf("touched_paths = %v, want the LATEST plan's sorted, de-duplicated [a.go b.go]", appr.TouchedPaths)
	}
	addr := bySeq[seqs["concern_addressed_by_condition"]]
	if addr.ConcernCategoryRaw != "Test Coverage" || addr.ConcernCategory != "testing" || addr.Severity != "high" {
		t.Errorf("addressed row concern = %q/%q/%q, want the review_concerns join Test Coverage/testing/high",
			addr.ConcernCategoryRaw, addr.ConcernCategory, addr.Severity)
	}
	clar := bySeq[seqs["clarification_answered"]]
	if clar.Repo != "acme/gadgets" || clar.AccountID == nil || *clar.AccountID != f.accountB ||
		!reflect.DeepEqual(clar.TouchedPaths, []string{"gadget.go"}) {
		t.Errorf("run B row = %+v", clar)
	}
}

// TestBackfill_SkipsNonDecisionEntries: a chain of ONLY non-decision entries
// yields no rows. COUNTERFACTUAL: IsDecisionBearing → true unconditionally
// (the page query's category filter is DecisionBearingCategories, which that
// does not change, so the extractor is the control) → RED on Extract.
func TestBackfill_SkipsNonDecisionEntries(t *testing.T) {
	f := newChainFixture(t)
	for _, c := range []string{"run_started", "plan_reviewed", "escalation_fired", "stage_completed"} {
		f.appendEntry(t, f.runA, &f.planStageA, c, map[string]any{})
	}
	sum, err := Backfill(context.Background(), f.pool, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if sum.EntriesScanned != 0 || sum.RowsWritten != 0 || countRows(t, f.pool) != 0 {
		t.Errorf("summary = %+v, rows = %d; want nothing indexed", sum, countRows(t, f.pool))
	}
}

// TestBackfill_ReportsUnmappedCategories: two concern entries with categories
// absent from the normalization table are REPORTED (sorted, de-duplicated) and
// still WRITTEN, indexed as themselves.
func TestBackfill_ReportsUnmappedCategories(t *testing.T) {
	f := newChainFixture(t)
	ctx := context.Background()
	f.appendEntry(t, f.runA, &f.implStageA, "concern_waived", map[string]any{"category": "flakiness-under-race", "reason": "r"})
	f.appendEntry(t, f.runA, &f.implStageA, "concern_deferred", map[string]any{"category": "Cosmic Rays", "reason": "r"})
	f.appendEntry(t, f.runA, &f.implStageA, "concern_waived", map[string]any{"category": "flakiness-under-race", "reason": "r"})
	f.appendEntry(t, f.runA, &f.implStageA, "concern_waived", map[string]any{"category": "security", "reason": "r"})

	sum, err := Backfill(ctx, f.pool, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(sum.UnmappedCategories, []string{"cosmic-rays", "flakiness-under-race"}) {
		t.Errorf("unmapped = %v, want [cosmic-rays flakiness-under-race]", sum.UnmappedCategories)
	}
	if sum.RowsWritten != 4 {
		t.Errorf("rows written = %d, want 4 (unmapped rows are never dropped)", sum.RowsWritten)
	}
	rows, err := NewStore(f.pool).List(ctx, ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	unmappedRows := 0
	for _, r := range rows {
		if r.ConcernCategoryUnmapped {
			unmappedRows++
		}
	}
	if unmappedRows != 3 {
		t.Errorf("rows flagged unmapped = %d, want 3", unmappedRows)
	}
}

// TestBackfill_EscalationKeysSameStageOnly pins #3730 approval condition 3:
// a decision takes the fired_keys of the LATEST escalation_fired entry on its
// OWN stage with a LOWER sequence. MECHANISM: an earlier and a later escalation
// on the same stage, plus one on a different stage, bracket the decision — so
// ignoring the stage, the ordering, or "latest" each lands different keys.
func TestBackfill_EscalationKeysSameStageOnly(t *testing.T) {
	f := newChainFixture(t)
	ctx := context.Background()
	f.appendEntry(t, f.runA, &f.planStageA, "escalation_fired", map[string]any{"fired_keys": []string{"old"}})
	f.appendEntry(t, f.runA, &f.planStageA, "escalation_fired", map[string]any{"fired_keys": []string{"zeta", "alpha", "zeta"}})
	f.appendEntry(t, f.runA, &f.implStageA, "escalation_fired", map[string]any{"fired_keys": []string{"other-stage"}})
	onStage := f.appendEntry(t, f.runA, &f.planStageA, "approval_submitted", map[string]any{"decision": "approve"})
	f.appendEntry(t, f.runA, &f.planStageA, "escalation_fired", map[string]any{"fired_keys": []string{"after"}})
	// The implement stage's decision precedes nothing but its own escalation;
	// run B's stage has none at all.
	noEsc := f.appendEntry(t, f.runB, &f.planStgB, "approval_submitted", map[string]any{"decision": "approve"})

	if _, err := Backfill(ctx, f.pool, Options{}); err != nil {
		t.Fatal(err)
	}
	rows, err := NewStore(f.pool).List(ctx, ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	got := map[int64][]string{}
	for _, r := range rows {
		got[r.SourceSequence] = r.EscalationKeys
	}
	if !reflect.DeepEqual(got[onStage.Sequence], []string{"alpha", "zeta"}) {
		t.Errorf("same-stage decision keys = %v, want [alpha zeta] (latest lower, sorted, de-duplicated)", got[onStage.Sequence])
	}
	if !reflect.DeepEqual(got[noEsc.Sequence], []string{}) {
		t.Errorf("different-stage decision keys = %v, want empty", got[noEsc.Sequence])
	}
}

// TestBackfill_DryRunWritesNothing: a dry run extracts and counts, and neither
// writes nor truncates — even combined with --rebuild.
func TestBackfill_DryRunWritesNothing(t *testing.T) {
	f := newChainFixture(t)
	ctx := context.Background()
	f.appendEntry(t, f.runA, &f.planStageA, "approval_submitted", map[string]any{"decision": "approve"})
	if err := NewStore(f.pool).Upsert(ctx, sampleRow(f.runA, 999999)); err != nil {
		t.Fatal(err)
	}
	sum, err := Backfill(ctx, f.pool, Options{DryRun: true, Rebuild: true})
	if err != nil {
		t.Fatal(err)
	}
	if sum.RowsExtracted != 1 || sum.RowsWritten != 0 || sum.GapsRemaining != 1 {
		t.Errorf("summary = %+v, want 1 extracted / 0 written / 1 gap remaining", sum)
	}
	if n := countRows(t, f.pool); n != 1 {
		t.Errorf("rows = %d, want the pre-existing 1 (dry run must neither write nor truncate)", n)
	}
}

// TestBackfill_RebuildTruncatesStaleRows: --rebuild drops a row no chain entry
// supports; a plain top-up keeps it.
func TestBackfill_RebuildTruncatesStaleRows(t *testing.T) {
	f := newChainFixture(t)
	ctx := context.Background()
	f.appendEntry(t, f.runA, &f.planStageA, "approval_submitted", map[string]any{"decision": "approve"})
	if err := NewStore(f.pool).Upsert(ctx, sampleRow(f.runA, 999999)); err != nil {
		t.Fatal(err)
	}
	if _, err := Backfill(ctx, f.pool, Options{}); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, f.pool); n != 2 {
		t.Fatalf("top-up rows = %d, want 2 (stale row kept)", n)
	}
	if _, err := Backfill(ctx, f.pool, Options{Rebuild: true}); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, f.pool); n != 1 {
		t.Errorf("rebuild rows = %d, want 1 (stale row truncated)", n)
	}
}

// TestBackfill_SkipsAndNamesOrphanedEntries: an entry whose run row is gone is
// counted and NAMED, not written and not guessed at, and the rest of the page
// still indexes.
func TestBackfill_SkipsAndNamesOrphanedEntries(t *testing.T) {
	f := newChainFixture(t)
	ctx := context.Background()
	f.appendEntry(t, f.runA, &f.planStageA, "approval_submitted", map[string]any{"decision": "approve"})
	orphan := f.insertOrphanEntry(t, "approval_submitted")
	sum, err := Backfill(ctx, f.pool, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if sum.RowsSkippedNoRun != 1 || !reflect.DeepEqual(sum.SkippedNoRunSequences, []int64{orphan}) {
		t.Errorf("skipped = %d %v, want 1 [%d]", sum.RowsSkippedNoRun, sum.SkippedNoRunSequences, orphan)
	}
	if sum.RowsWritten != 1 || sum.GapsRemaining != 0 || sum.OrphanedEntries != 1 {
		t.Errorf("summary = %+v, want 1 written / 0 gaps / 1 orphaned", sum)
	}
}

// TestBackfill_FailsLoudOnUndecodablePayload: a decision entry whose payload
// is a JSON array aborts the backfill naming its sequence, rather than being
// silently dropped.
func TestBackfill_FailsLoudOnUndecodablePayload(t *testing.T) {
	f := newChainFixture(t)
	var seq int64
	if err := f.pool.QueryRow(context.Background(), `INSERT INTO audit_entries (id, run_id, category, payload, entry_hash)
		VALUES ($1, $2, 'approval_submitted', '[1,2]', 'h') RETURNING sequence`, uuid.New(), f.runA).Scan(&seq); err != nil {
		t.Fatal(err)
	}
	_, err := Backfill(context.Background(), f.pool, Options{})
	if err == nil {
		t.Fatal("backfill over an undecodable payload succeeded, want an error")
	}
	if !strings.Contains(err.Error(), "sequence") {
		t.Errorf("error %q does not name the sequence", err)
	}
}

// TestBackfill_EscalationPayloadStageIDIsCaseInsensitive pins the second
// implement-review concern: an escalation_fired entry whose stage_id COLUMN is
// unset and whose PAYLOAD records the stage id non-canonically (upper case) must
// reach LatestEscalationKeys. escalationSQL's payload arm compares against
// uuid.String() output, which is always lower case, while escalationStage parses
// case-insensitively — so without lower() in the SQL the two filters disagree on
// the same entry and the decision silently indexes empty escalation_keys.
// COUNTERFACTUAL: drop lower() from escalationSQL's payload arm — RED here
// (escalation_keys = []).
func TestBackfill_EscalationPayloadStageIDIsCaseInsensitive(t *testing.T) {
	f := newChainFixture(t)
	ctx := context.Background()
	upper := strings.ToUpper(f.planStageA.String())
	f.exec(t, `INSERT INTO audit_entries (id, run_id, category, payload, entry_hash)
	           VALUES ($1, $2, 'escalation_fired', $3, $4)`,
		uuid.New(), f.runA, []byte(`{"stage_id":"`+upper+`","fired_keys":["rk-upper"]}`), "h-"+uuid.NewString())
	dec := f.appendEntry(t, f.runA, &f.planStageA, "approval_submitted", map[string]any{"decision": "approve"})

	if _, err := Backfill(ctx, f.pool, Options{}); err != nil {
		t.Fatal(err)
	}
	rows, err := NewStore(f.pool).List(ctx, ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].SourceSequence != dec.Sequence {
		t.Fatalf("rows = %v, want the decision at sequence %d", seqsOf(rows), dec.Sequence)
	}
	if !reflect.DeepEqual(rows[0].EscalationKeys, []string{"rk-upper"}) {
		t.Errorf("escalation_keys = %#v, want [rk-upper] (payload stage_id matched case-insensitively)", rows[0].EscalationKeys)
	}
}

// TestBackfill_ToleratesHistoricalPlanShape is the cross-layer regression for
// the implement-review concern: a run whose LATEST plan artifact carries the
// legacy bare-string scope.files shape must index with EMPTY touched_paths, not
// abort the page. resolveContexts propagates TouchedPathsFromPlan's error, so an
// intolerant decode here failed the whole backfill — and live indexing for every
// decision on that run — on a shape the index promises to tolerate.
// COUNTERFACTUAL: decode scope.files back into []struct{Path string} in
// extract.go — RED here with `cannot unmarshal string` and 0 rows written.
func TestBackfill_ToleratesHistoricalPlanShape(t *testing.T) {
	f := newChainFixture(t)
	ctx := context.Background()
	base := time.Now().UTC().Add(-time.Hour)
	f.seedPlan(t, f.planStageA, base, "current.go")
	// Newer than the object-shaped plan, so it is the one the LATERAL join takes.
	f.seedPlanRaw(t, f.planStageA, base.Add(time.Minute), []byte(`{"scope":{"files":["legacy.go"]}}`))
	dec := f.appendEntry(t, f.runA, &f.planStageA, "approval_submitted", map[string]any{"decision": "approve"})

	sum, err := Backfill(ctx, f.pool, Options{})
	if err != nil {
		t.Fatalf("backfill over a legacy plan shape failed: %v", err)
	}
	if sum.RowsWritten != 1 {
		t.Fatalf("rows written = %d, want 1 (summary %+v)", sum.RowsWritten, sum)
	}
	rows, err := NewStore(f.pool).List(ctx, ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].SourceSequence != dec.Sequence {
		t.Fatalf("rows = %v, want the decision at sequence %d", seqsOf(rows), dec.Sequence)
	}
	if !reflect.DeepEqual(rows[0].TouchedPaths, []string{}) {
		t.Errorf("touched_paths = %#v, want empty (legacy shape contributes no paths)", rows[0].TouchedPaths)
	}
}

// TestDecisionBearingCategories_SortedForSQL guards the ANY($1) argument's
// determinism the page query relies on.
func TestDecisionBearingCategories_SortedForSQL(t *testing.T) {
	c := DecisionBearingCategories()
	if !sort.StringsAreSorted(c) || len(c) != 9 {
		t.Errorf("categories = %v, want 9 sorted", c)
	}
}
