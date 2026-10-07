package decisionindex

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
)

// auditAppenderInterfaces parses package audit's NON-test source and returns
// every exported interface type whose name ends in "Appender" — the optional
// capabilities server/*.go type-asserts off its audit.Repository. A source scan,
// not reflection: reflection cannot enumerate a package's declared interfaces
// (#3730 approval condition 5).
func auditAppenderInterfaces(t *testing.T) []string {
	t.Helper()
	dir := filepath.Join("..", "audit")
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	fset := token.NewFileSet()
	var names []string
	for _, de := range ents {
		n := de.Name()
		if de.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(dir, n), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", n, err)
		}
		for _, decl := range file.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.TYPE {
				continue
			}
			for _, spec := range gd.Specs {
				ts := spec.(*ast.TypeSpec)
				if _, isIface := ts.Type.(*ast.InterfaceType); !isIface {
					continue
				}
				if ts.Name.IsExported() && strings.HasSuffix(ts.Name.Name, "Appender") {
					names = append(names, ts.Name.Name)
				}
			}
		}
	}
	sort.Strings(names)
	return names
}

// TestIndexingRepository_ForwardsEveryOptionalCapability is the capability
// drift guard. Every *Appender interface declared in package audit must be
// satisfied by the decorator; a NEW one lands in the default arm and goes RED
// until it is both forwarded in writer.go (FullRepository + a method that
// indexes) and listed here. Without this, wrapping the concrete repository
// would silently turn the server's type assertion for the new capability into
// ok=false and drop its feature.
func TestIndexingRepository_ForwardsEveryOptionalCapability(t *testing.T) {
	names := auditAppenderInterfaces(t)
	if len(names) < 4 {
		t.Fatalf("source scan found only %v — the scan is broken, not the decorator", names)
	}
	var dec any
	d, err := NewIndexingRepository(audit.NewPostgresRepository(nil), NewStore(nil), failingResolver{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	dec = d
	for _, name := range names {
		var ok bool
		switch name {
		case "AnchoredChainAppender":
			_, ok = dec.(audit.AnchoredChainAppender)
		case "DedupedChainAppender":
			_, ok = dec.(audit.DedupedChainAppender)
		case "FamilyWindowAppender":
			_, ok = dec.(audit.FamilyWindowAppender)
		case "GroomingWindowAppender":
			_, ok = dec.(audit.GroomingWindowAppender)
		case "RetryBudgetAppender":
			_, ok = dec.(audit.RetryBudgetAppender)
		case "UpkeepWindowAppender":
			_, ok = dec.(audit.UpkeepWindowAppender)
		default:
			t.Errorf("audit declares %s, which the decision-index decorator does not forward: add it to decisionindex.FullRepository with an indexing method in writer.go, then list it in this switch", name)
			continue
		}
		if !ok {
			t.Errorf("decorator does not satisfy audit.%s", name)
		}
	}
}

// TestIndexingRepository_CapabilityPathsIndex drives every forwarded
// capability THROUGH the decorator over the real chain and asserts BOTH that
// the entry landed AND that its index row appeared. acceptance_triage_arbitrated
// and grooming_disposition_recorded reach the chain ONLY through
// AnchoredChainAppender and GroomingWindowAppender, so a decorator that
// forwarded those without indexing would drop two decision classes.
//
// Counterfactual: delete the r.index call from the BODY of any forwarded
// method whose subtest appends a DECISION-BEARING class (anchored, grooming
// batch, family grooming batch, deduped, retry budget) — RED on that method's
// row assertion. The upkeep, comms and close forwarders append no
// decision-bearing row, so their subtests assert the forward COMMITS instead.
func TestIndexingRepository_CapabilityPathsIndex(t *testing.T) {
	f := newChainFixture(t)
	ctx := context.Background()
	s := NewStore(f.pool)
	d, logs := decorate(t, f, s, NewPoolResolver(f.pool))

	params := func(cat string, payload map[string]any) audit.ChainAppendParams {
		p := approvalParams(t, f, payload)
		p.StageID = &f.implStageA
		p.Category = cat
		return p
	}
	indexed := func(t *testing.T, e *audit.Entry, want DecisionClass) {
		t.Helper()
		assertEntryCommitted(t, f, e)
		rows, err := s.List(ctx, ListFilter{DecisionClass: want})
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rows {
			if r.SourceSequence == e.Sequence {
				return
			}
		}
		t.Fatalf("%s entry %d committed but NOT indexed (rows %v)", e.Category, e.Sequence, seqsOf(rows))
	}

	t.Run("AnchoredChainAppender", func(t *testing.T) {
		anchor := f.appendEntry(t, f.runA, &f.implStageA, "acceptance_outcome_recorded", map[string]any{"status": "failed"})
		e, err := d.AppendChainedAnchored(ctx,
			params("acceptance_triage_arbitrated", map[string]any{"verdict": "pass", "reason": "wrong tree", "outcome_sequence": anchor.Sequence}),
			audit.AnchorSpec{AnchorCategory: "acceptance_outcome_recorded", AnchorSequence: anchor.Sequence, DedupePayloadKey: "outcome_sequence", DedupeValue: anchor.Sequence})
		if err != nil {
			t.Fatalf("AppendChainedAnchored: %v", err)
		}
		indexed(t, e, ClassAcceptanceArbitration)
	})

	t.Run("GroomingWindowAppender/batch", func(t *testing.T) {
		es, err := d.AppendChainedGroomingDispositionBatch(ctx, "artifact-1", []audit.ChainAppendParams{
			params("grooming_disposition_recorded", map[string]any{"verdict": "keep"}),
			params("grooming_disposition_recorded", map[string]any{"verdict": "drop"}),
		})
		if err != nil {
			t.Fatalf("AppendChainedGroomingDispositionBatch: %v", err)
		}
		if len(es) != 2 {
			t.Fatalf("batch appended %d entries, want 2", len(es))
		}
		for _, e := range es {
			indexed(t, e, ClassGroomingDisposition)
		}
	})

	t.Run("GroomingWindowAppender/close", func(t *testing.T) {
		w, _, err := d.AppendChainedGroomingWindowClose(ctx,
			params(audit.GroomingApplyWindowClosedCategory, map[string]any{"artifact_id": "artifact-1", "settlement": "approved"}), "artifact-1")
		if err != nil {
			t.Fatalf("AppendChainedGroomingWindowClose: %v", err)
		}
		assertEntryCommitted(t, f, w)
		// The watermark is not decision-bearing: forwarded, never indexed.
		var n int
		if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM decision_index WHERE source_sequence = $1`, w.Sequence).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("non-decision watermark %d was indexed", w.Sequence)
		}
	})

	// UpkeepWindowAppender (#3923). The batch's binding re-check requires the
	// newest upkeep_report_recorded row to name the capture's artifact, so it is
	// seeded first. upkeep_disposition_recorded is not decision-bearing today:
	// the assertion is that the forward reaches the inner repo and COMMITS.
	t.Run("UpkeepWindowAppender/batch+close", func(t *testing.T) {
		// Reached through the type assertion the server makes, so a decorator
		// that dropped the forward fails HERE rather than at compile time.
		win, ok := any(d).(audit.UpkeepWindowAppender)
		if !ok {
			t.Fatal("decorator does not forward audit.UpkeepWindowAppender: the server would silently take the non-atomic upkeep fallback")
		}
		f.appendEntry(t, f.runA, &f.implStageA, audit.UpkeepReportRecordedCategory, map[string]any{"artifact_id": "upkeep-1"})
		es, err := win.AppendChainedUpkeepDispositionBatch(ctx, "upkeep-1", []audit.ChainAppendParams{
			params(audit.UpkeepDispositionRecordedCategory, map[string]any{"artifact_id": "upkeep-1", "finding_id": "flake:x", "verdict": "approved"}),
		})
		if err != nil {
			t.Fatalf("AppendChainedUpkeepDispositionBatch: %v", err)
		}
		if len(es) != 1 {
			t.Fatalf("batch appended %d entries, want 1", len(es))
		}
		assertEntryCommitted(t, f, es[0])
		w, consumed, err := win.AppendChainedUpkeepWindowClose(ctx,
			params(audit.UpkeepApplyWindowClosedCategory, map[string]any{"artifact_id": "upkeep-1", "settlement": "approved"}), "upkeep-1")
		if err != nil {
			t.Fatalf("AppendChainedUpkeepWindowClose: %v", err)
		}
		assertEntryCommitted(t, f, w)
		if len(consumed) != 1 || consumed[0].Sequence != es[0].Sequence {
			t.Fatalf("consumed = %v, want exactly the batch entry %d", consumed, es[0].Sequence)
		}
	})

	// FamilyWindowAppender (#4012), comms family. Like the upkeep batch it
	// re-checks its binding, so a comms_report_recorded row naming comms-1 is
	// seeded first. comms_disposition_recorded is not decision-bearing: the
	// assertion is that the generic forward reaches the inner repo, COMMITS,
	// and the close consumes exactly the batch entry.
	t.Run("FamilyWindowAppender/batch+close", func(t *testing.T) {
		win, ok := any(d).(audit.FamilyWindowAppender)
		if !ok {
			t.Fatal("decorator does not forward audit.FamilyWindowAppender: the server would silently take the non-atomic family fallback")
		}
		f.appendEntry(t, f.runA, &f.implStageA, audit.CommsReportRecordedCategory, map[string]any{"artifact_id": "comms-1"})
		es, err := win.AppendChainedFamilyDispositionBatch(ctx, audit.WindowFamilyComms, "comms-1", []audit.ChainAppendParams{
			params(audit.CommsDispositionRecordedCategory, map[string]any{"artifact_id": "comms-1", "entry_id": "report:1", "verdict": "approved"}),
		})
		if err != nil {
			t.Fatalf("AppendChainedFamilyDispositionBatch(comms): %v", err)
		}
		if len(es) != 1 {
			t.Fatalf("batch appended %d entries, want 1", len(es))
		}
		assertEntryCommitted(t, f, es[0])
		w, consumed, err := win.AppendChainedFamilyWindowClose(ctx, audit.WindowFamilyComms,
			params(audit.CommsApplyWindowClosedCategory, map[string]any{"artifact_id": "comms-1", "settlement": "approved"}), "comms-1")
		if err != nil {
			t.Fatalf("AppendChainedFamilyWindowClose(comms): %v", err)
		}
		assertEntryCommitted(t, f, w)
		if len(consumed) != 1 || consumed[0].Sequence != es[0].Sequence {
			t.Fatalf("consumed = %v, want exactly the batch entry %d", consumed, es[0].Sequence)
		}
	})

	// FamilyWindowAppender, GROOMING family (#4012 approval condition 4): the
	// generic batch is a third path for grooming_disposition_recorded, a
	// DECISION-BEARING class, so the forward must index what it appends. The
	// comms subtest above cannot see a missing r.index call — comms rows are
	// not decision-bearing and index() returns before writing for them.
	// "artifact-2" because GroomingWindowAppender/close settled artifact-1.
	//
	// Counterfactual (performed): delete the r.index loop from
	// IndexingRepository.AppendChainedFamilyDispositionBatch's BODY. The fixture
	// state that makes it observable: the appended rows are
	// grooming_disposition_recorded (IsDecisionBearing true), the resolver is
	// the real pool resolver over a run/stage that exists, so the ONLY thing
	// between the commit and a decision_index row is that loop — RED with
	// "grooming_disposition_recorded entry N committed but NOT indexed".
	t.Run("FamilyWindowAppender/grooming-batch-indexes", func(t *testing.T) {
		win, ok := any(d).(audit.FamilyWindowAppender)
		if !ok {
			t.Fatal("decorator does not forward audit.FamilyWindowAppender")
		}
		es, err := win.AppendChainedFamilyDispositionBatch(ctx, audit.WindowFamilyGrooming, "artifact-2", []audit.ChainAppendParams{
			params(audit.GroomingDispositionRecordedCategory, map[string]any{"artifact_id": "artifact-2", "verdict": "keep"}),
		})
		if err != nil {
			t.Fatalf("AppendChainedFamilyDispositionBatch(grooming): %v", err)
		}
		if len(es) != 1 {
			t.Fatalf("batch appended %d entries, want 1", len(es))
		}
		indexed(t, es[0], ClassGroomingDisposition)
	})

	t.Run("DedupedChainAppender", func(t *testing.T) {
		e, err := d.AppendChainedDeduped(ctx, params("merge_verdict_recorded", map[string]any{"verdict": "merge", "head_sha": "abc"}),
			audit.DedupeSpec{StageID: &f.implStageA, PayloadKey: "head_sha", PayloadValue: "abc"})
		if err != nil {
			t.Fatalf("AppendChainedDeduped: %v", err)
		}
		indexed(t, e, ClassMergeVerdict)
	})

	t.Run("RetryBudgetAppender", func(t *testing.T) {
		e, err := d.AppendChainedUnderBudget(ctx, params("scope_amendment_decided", nil), 3, func(int) (json.RawMessage, error) {
			return json.RawMessage(`{"decision":"approved","reason":"coupled"}`), nil
		})
		if err != nil {
			t.Fatalf("AppendChainedUnderBudget: %v", err)
		}
		indexed(t, e, ClassScopeAmendment)
	})

	if strings.Contains(logs.String(), "level=WARN") {
		t.Fatalf("healthy capability writes logged a WARN:\n%s", logs.String())
	}
}
