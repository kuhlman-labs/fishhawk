package server

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/auditcomplete"
)

// --- #4874: one acceptance head resolution (acceptanceHeadAsOf) ---

const (
	ahS  = "5000000000000000000000000000000000000005" // a reported head
	ahF  = "f000000000000000000000000000000000000001" // a fix-up head
	ahC  = "c000000000000000000000000000000000000003" // a reported scenario / conflict head
	ahM  = "a000000000000000000000000000000000000001" // a rebase merge commit
	ahM2 = "a000000000000000000000000000000000000002" // a second, chained merge commit
	ahF1 = "1f00000000000000000000000000000000000001" // an integration merge
	ahF2 = "2f00000000000000000000000000000000000002" // a later integration merge
	ahX  = "eeee000000000000000000000000000000000000" // a foreign concurrent push
	ahH  = "4000000000000000000000000000000000000004" // the implement (PR-open) head of the #4673 shape
)

var ahT0 = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

// ahRow builds one audit entry BY CONSTRUCTION with an explicit sequence and
// timestamp offset (seconds after ahT0), so ordering rows can make sequence
// and timestamp disagree on purpose.
func ahRow(runID uuid.UUID, category string, seq int64, tsOffset int, payload map[string]any) *audit.Entry {
	rid := runID
	p, _ := json.Marshal(payload)
	return &audit.Entry{ID: uuid.New(), RunID: &rid, Category: category, Sequence: seq,
		Timestamp: ahT0.Add(time.Duration(tsOffset) * time.Second), Payload: p}
}

func ahHead(runID uuid.UUID, category string, seq int64, tsOffset int, sha string) *audit.Entry {
	return ahRow(runID, category, seq, tsOffset, map[string]any{"head_sha": sha})
}

func ahIntegration(runID uuid.UUID, seq int64, tsOffset int, sha string) *audit.Entry {
	return ahRow(runID, lineageIntegrationCommitCategory, seq, tsOffset, map[string]any{"merge_sha": sha})
}

// ahRebase is a branch_rebased row with writeBranchRebasedAudit's field names;
// clean nil omits clean_verified (a legacy row).
func ahRebase(runID uuid.UUID, seq int64, tsOffset int, prior, merge, newHead string, upToDate bool, clean *bool) *audit.Entry {
	e := fixupWalkRebasedRow(seq, prior, merge, newHead, upToDate, clean)
	rid := runID
	e.ID, e.RunID, e.Timestamp = uuid.New(), &rid, ahT0.Add(time.Duration(tsOffset)*time.Second)
	return e
}

func ahClean(runID uuid.UUID, seq int64, tsOffset int, prior, merge string) *audit.Entry {
	return ahRebase(runID, seq, tsOffset, prior, merge, merge, false, boolPtr(true))
}

func ahReset(runID uuid.UUID, seq int64, prior, resetTo string) *audit.Entry {
	e := fixupWalkResetRow(seq, prior, resetTo)
	rid := runID
	e.ID, e.RunID, e.Timestamp = uuid.New(), &rid, ahT0.Add(time.Duration(seq)*time.Second)
	return e
}

func ahServer(rows ...*audit.Entry) (*Server, *auditFake) {
	au := newAuditFake()
	au.seeded = append(au.seeded, rows...)
	return New(Config{Addr: "127.0.0.1:0", AuditRepo: au}), au
}

func TestRowsAtOrBefore(t *testing.T) {
	if got := rowsAtOrBefore(nil, 5); got == nil || len(got) != 0 {
		t.Errorf("nil slice = %#v, want an empty non-nil result", got)
	}
	in := []*audit.Entry{nil, {Sequence: 4}, {Sequence: 5}, {Sequence: 6}}
	got := rowsAtOrBefore(in, 5)
	if len(got) != 2 || got[0].Sequence != 4 || got[1].Sequence != 5 {
		t.Errorf("rowsAtOrBefore = %+v, want the sequence 4 and 5 rows (boundary kept, nil and above-bound dropped)", got)
	}
}

// TestAcceptanceHeadAsOf pins every arm of the resolver, each row on its own
// explicit-sequence chain.
func TestAcceptanceHeadAsOf(t *testing.T) {
	run := uuid.New()
	tests := []struct {
		name    string
		rows    []*audit.Entry
		maxSeq  int64
		bounded bool
		want    string
	}{
		{"no rows answers empty", nil, 0, false, ""},
		{"a reported head with no base merge answers the seed",
			[]*audit.Entry{ahHead(run, "pull_request_opened", 10, 0, ahS)}, 0, false, ahS},
		{"a clean advance answers the merge commit",
			[]*audit.Entry{ahHead(run, "pull_request_opened", 10, 0, ahS), ahClean(run, 11, 1, ahS, ahM)}, 0, false, ahM},
		{"chained clean advances answer the second merge",
			[]*audit.Entry{ahHead(run, "pull_request_opened", 10, 0, ahS), ahClean(run, 11, 1, ahS, ahM), ahClean(run, 12, 2, ahM, ahM2)}, 0, false, ahM2},
		{"a concurrent push keeps the seed",
			[]*audit.Entry{ahHead(run, "pull_request_opened", 10, 0, ahS), ahRebase(run, 11, 1, ahS, ahM, ahX, false, boolPtr(true))}, 0, false, ahS},
		{"clean_verified false keeps the seed",
			[]*audit.Entry{ahHead(run, "pull_request_opened", 10, 0, ahS), ahRebase(run, 11, 1, ahS, ahM, ahM, false, boolPtr(false))}, 0, false, ahS},
		{"a legacy row without clean_verified keeps the seed",
			[]*audit.Entry{ahHead(run, "pull_request_opened", 10, 0, ahS), ahRebase(run, 11, 1, ahS, ahM, ahM, false, nil)}, 0, false, ahS},
		{"already_up_to_date keeps the seed",
			[]*audit.Entry{ahHead(run, "pull_request_opened", 10, 0, ahS), ahRebase(run, 11, 1, ahS, ahM, ahM, true, boolPtr(true))}, 0, false, ahS},
		{"a prior-head mismatch keeps the seed",
			[]*audit.Entry{ahHead(run, "pull_request_opened", 10, 0, ahS), ahClean(run, 11, 1, ahX, ahM)}, 0, false, ahS},
		{"a reset on the walked chain answers the reset target",
			[]*audit.Entry{ahHead(run, "pull_request_opened", 10, 0, ahS), ahClean(run, 11, 1, ahS, ahM), ahClean(run, 12, 2, ahM, ahM2), ahReset(run, 13, ahM2, ahM)}, 0, false, ahM},
		{"a reset off the chain answers the seed",
			[]*audit.Entry{ahHead(run, "pull_request_opened", 10, 0, ahS), ahClean(run, 11, 1, ahS, ahM), ahReset(run, 12, ahM, ahX)}, 0, false, ahS},
		{"an undecodable branch_rebased row keeps the seed",
			[]*audit.Entry{ahHead(run, "pull_request_opened", 10, 0, ahS), {RunID: &run, Category: CategoryBranchRebased, Sequence: 11, Payload: []byte("{not json")}}, 0, false, ahS},
		{"a later fixup_pushed supersedes the rebase",
			[]*audit.Entry{ahHead(run, "pull_request_opened", 10, 0, ahS), ahClean(run, 11, 1, ahS, ahM), ahHead(run, "fixup_pushed", 12, 2, ahF)}, 0, false, ahF},
		{"a conflict_resolution_pushed seed walks through a clean rebase",
			[]*audit.Entry{ahHead(run, "pull_request_opened", 10, 0, ahS), ahHead(run, auditcomplete.CategoryConflictResolutionPushed, 11, 1, ahC), ahClean(run, 12, 2, ahC, ahM)}, 0, false, ahM},
		{"a fan-in seed plus a clean rebase answers the merge (the #4817 fallback)",
			[]*audit.Entry{ahIntegration(run, 10, 0, ahF1), ahClean(run, 11, 1, ahF1, ahM)}, 0, false, ahM},
		{"a reported head NEWER than the integration row beats the fan-in seed",
			[]*audit.Entry{ahIntegration(run, 10, 0, ahF1), ahHead(run, "fixup_pushed", 11, 1, ahF)}, 0, false, ahF},
		{"no seed with a rebase row stays empty",
			[]*audit.Entry{ahClean(run, 11, 1, ahS, ahM)}, 0, false, ""},
		// Bounded: S at 10, clean S -> M at 11, bound 12, clean M -> M2 at 13.
		{"bounded excludes a rebase row above maxSeq",
			[]*audit.Entry{ahHead(run, "pull_request_opened", 10, 0, ahS), ahClean(run, 11, 1, ahS, ahM), ahClean(run, 13, 3, ahM, ahM2)}, 12, true, ahM},
		{"the same rows unbounded walk through the above-bound row",
			[]*audit.Entry{ahHead(run, "pull_request_opened", 10, 0, ahS), ahClean(run, 11, 1, ahS, ahM), ahClean(run, 13, 3, ahM, ahM2)}, 0, false, ahM2},
		{"bounded excludes an above-bound seed candidate",
			[]*audit.Entry{ahHead(run, "pull_request_opened", 10, 0, ahS), ahClean(run, 11, 1, ahS, ahM), ahHead(run, "fixup_pushed", 13, 3, ahF)}, 12, true, ahM},
		{"the same rows unbounded answer the above-bound seed",
			[]*audit.Entry{ahHead(run, "pull_request_opened", 10, 0, ahS), ahClean(run, 11, 1, ahS, ahM), ahHead(run, "fixup_pushed", 13, 3, ahF)}, 0, false, ahF},
		// Tie-breaking rows that cannot tell sequence from timestamp ordering.
		{"a newer timestamp with a higher sequence wins",
			[]*audit.Entry{ahHead(run, "pull_request_opened", 10, 0, ahS), ahHead(run, "fixup_pushed", 11, 5, ahF)}, 0, false, ahF},
		{"equal timestamp: the higher sequence wins (conflict above PR)",
			[]*audit.Entry{ahHead(run, "pull_request_opened", 11, 0, ahS), ahHead(run, auditcomplete.CategoryConflictResolutionPushed, 12, 0, ahC), ahClean(run, 13, 0, ahC, ahM)}, 0, false, ahM},
		{"equal timestamp: the higher sequence wins (PR above conflict)",
			[]*audit.Entry{ahHead(run, "pull_request_opened", 12, 0, ahS), ahHead(run, auditcomplete.CategoryConflictResolutionPushed, 11, 0, ahC), ahClean(run, 13, 0, ahS, ahM)}, 0, false, ahM},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, _ := ahServer(tt.rows...)
			got, err := s.acceptanceHeadAsOf(context.Background(), run, tt.maxSeq, tt.bounded)
			if err != nil {
				t.Fatalf("acceptanceHeadAsOf error = %v, want nil", err)
			}
			if got != tt.want {
				t.Errorf("acceptanceHeadAsOf = %q, want %q", got, tt.want)
			}
		})
	}
	t.Run("an unconfigured AuditRepo answers empty", func(t *testing.T) {
		s := New(Config{Addr: "127.0.0.1:0"})
		if got, err := s.acceptanceHeadAsOf(context.Background(), run, 0, false); got != "" || err != nil {
			t.Errorf("= (%q, %v), want (\"\", nil)", got, err)
		}
	})
}

// TestAcceptanceHeadAsOf_SeedOrderedByChainSequence: each row makes sequence
// and timestamp disagree, so it carries the opposite answer timestamp ordering
// (or the old category precedence) would give.
func TestAcceptanceHeadAsOf_SeedOrderedByChainSequence(t *testing.T) {
	run := uuid.New()
	tests := []struct {
		name    string
		rows    []*audit.Entry
		maxSeq  int64
		bounded bool
		want    string
	}{
		{"(a) across categories the higher sequence wins over a later timestamp",
			[]*audit.Entry{ahHead(run, "pull_request_opened", 10, 5, ahS), ahHead(run, "fixup_pushed", 12, 0, ahF)}, 0, false, ahF},
		{"(b) the reverse category direction",
			[]*audit.Entry{ahHead(run, "fixup_pushed", 10, 5, ahF), ahHead(run, "pull_request_opened", 12, 0, ahS)}, 0, false, ahS},
		{"(c) a same-timestamp tie keeps the higher sequence (fixup above PR)",
			[]*audit.Entry{ahHead(run, "pull_request_opened", 10, 0, ahS), ahHead(run, "fixup_pushed", 11, 0, ahF)}, 0, false, ahF},
		{"(c) a same-timestamp tie keeps the higher sequence (PR above fixup)",
			[]*audit.Entry{ahHead(run, "fixup_pushed", 10, 0, ahF), ahHead(run, "pull_request_opened", 11, 0, ahS)}, 0, false, ahS},
		{"(d) bound first: a later-stamped, higher-sequence head above the bound is excluded",
			[]*audit.Entry{ahHead(run, "pull_request_opened", 10, 0, ahS), ahHead(run, "fixup_pushed", 13, 9, ahF)}, 12, true, ahS},
		{"(d) the same rows unbounded",
			[]*audit.Entry{ahHead(run, "pull_request_opened", 10, 0, ahS), ahHead(run, "fixup_pushed", 13, 9, ahF)}, 0, false, ahF},
		{"(e) a rebase between seeds: the sequence-newest seed wins and the earlier rebase does not walk it",
			[]*audit.Entry{ahHead(run, "pull_request_opened", 10, 5, ahS), ahClean(run, 11, 1, ahS, ahM), ahHead(run, "fixup_pushed", 12, 0, ahF)}, 0, false, ahF},
		{"(f) a rebase with an EARLIER timestamp but a higher sequence walks",
			[]*audit.Entry{ahHead(run, "pull_request_opened", 10, 5, ahS), ahClean(run, 11, 0, ahS, ahM)}, 0, false, ahM},
		{"(f) a rebase with a later timestamp but a lower sequence does not walk",
			[]*audit.Entry{ahHead(run, "pull_request_opened", 10, 0, ahS), ahClean(run, 9, 5, ahS, ahM)}, 0, false, ahS},
		{"(g) fan-in: the higher-sequence integration row wins over a later timestamp",
			[]*audit.Entry{ahIntegration(run, 20, 5, ahF1), ahIntegration(run, 21, 0, ahF2)}, 0, false, ahF2},
		{"(g) fan-in bound 20 excludes F2",
			[]*audit.Entry{ahIntegration(run, 20, 5, ahF1), ahIntegration(run, 21, 0, ahF2)}, 20, true, ahF1},
		{"(h) an integration row at 9 now beats a reported head at 5",
			[]*audit.Entry{ahHead(run, "pull_request_opened", 5, 0, ahS), ahIntegration(run, 9, 0, ahF1)}, 0, false, ahF1},
		{"(i) a late integration row beats an older reported head",
			[]*audit.Entry{ahHead(run, "acceptance_scenarios_pushed", 20, 5, ahC), ahIntegration(run, 30, 0, ahF2)}, 0, false, ahF2},
		{"(i) bound 25 answers the reported head",
			[]*audit.Entry{ahHead(run, "acceptance_scenarios_pushed", 20, 5, ahC), ahIntegration(run, 30, 0, ahF2)}, 25, true, ahC},
		{"(j) a newer reported head beats the integration row",
			[]*audit.Entry{ahIntegration(run, 30, 5, ahF2), ahHead(run, "acceptance_scenarios_pushed", 40, 0, ahC)}, 0, false, ahC},
		{"(k) an equal-sequence tie keeps the reported row",
			[]*audit.Entry{ahIntegration(run, 30, 0, ahF2), ahHead(run, "acceptance_scenarios_pushed", 30, 0, ahC)}, 0, false, ahC},
		{"(l) a rebase predating the chosen integration seed does not walk it",
			[]*audit.Entry{ahHead(run, "acceptance_scenarios_pushed", 20, 0, ahC), ahClean(run, 25, 0, ahC, ahM), ahIntegration(run, 30, 0, ahF2)}, 0, false, ahF2},
		// (m) the #4673 shape: PR-open H at 10, the first acceptance episode's
		// corpus commit S at 20 with the LATER timestamp, then a fix-up F at 30
		// with the EARLIER one. Neither the old category precedence
		// (acceptance_scenarios_pushed above fixup_pushed) nor a timestamp
		// order answers F; only the chain sequence does.
		{"(m) a fix-up after the scenario commit wins over the scenario commit's category precedence and later timestamp",
			[]*audit.Entry{ahHead(run, "pull_request_opened", 10, 0, ahH), ahHead(run, "acceptance_scenarios_pushed", 20, 9, ahC), ahHead(run, "fixup_pushed", 30, 3, ahF)}, 0, false, ahF},
		{"(m) bound 25 excludes the fix-up and answers the scenario head",
			[]*audit.Entry{ahHead(run, "pull_request_opened", 10, 0, ahH), ahHead(run, "acceptance_scenarios_pushed", 20, 9, ahC), ahHead(run, "fixup_pushed", 30, 3, ahF)}, 25, true, ahC},
		{"(l) plus a clean rebase of the integration seed walks to it",
			[]*audit.Entry{ahHead(run, "acceptance_scenarios_pushed", 20, 0, ahC), ahClean(run, 25, 0, ahC, ahM), ahIntegration(run, 30, 0, ahF2), ahClean(run, 31, 0, ahF2, ahM2)}, 0, false, ahM2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, _ := ahServer(tt.rows...)
			got, err := s.acceptanceHeadAsOf(context.Background(), run, tt.maxSeq, tt.bounded)
			if err != nil {
				t.Fatalf("acceptanceHeadAsOf error = %v, want nil", err)
			}
			if got != tt.want {
				t.Errorf("acceptanceHeadAsOf = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestAcceptanceHeadAsOf_BaseMergeReadErrorFailsClosed: the seed read succeeds
// (so a seed answer is distinguishable), the base-merge read errors, and the
// resolver answers "" with an error — never the stale seed.
func TestAcceptanceHeadAsOf_BaseMergeReadErrorFailsClosed(t *testing.T) {
	for _, cat := range []string{CategoryBranchRebased, CategoryBranchReset} {
		t.Run(cat, func(t *testing.T) {
			run := uuid.New()
			s, au := ahServer(ahHead(run, "pull_request_opened", 10, 0, ahS), ahClean(run, 11, 1, ahS, ahM))
			au.listByCategoryErrCategory = cat
			got, err := s.acceptanceHeadAsOf(context.Background(), run, 0, false)
			if err == nil || got != "" {
				t.Fatalf("= (%q, %v), want (\"\", an error) on a %s read failure", got, err, cat)
			}
			if !strings.Contains(err.Error(), "list base-merge rows") {
				t.Errorf("error = %v, want it to name the base-merge read", err)
			}
		})
	}
}

// TestAcceptanceHeadAsOf_LedgerReadErrorReturned: a read error on either
// required seed set is returned, even when the other set holds a candidate.
func TestAcceptanceHeadAsOf_LedgerReadErrorReturned(t *testing.T) {
	cases := []struct {
		name, failing, wantErr string
	}{
		{"a reported-head category", "fixup_pushed", "list reported-head rows"},
		// The integration discriminator: reported S is present, so a swallowed
		// error would answer S.
		{"the integration set", lineageIntegrationCommitCategory, "list integration rows"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			run := uuid.New()
			s, au := ahServer(ahHead(run, "pull_request_opened", 10, 0, ahS), ahIntegration(run, 5, 0, ahF1))
			au.listByCategoryErrCategory = tc.failing
			got, err := s.acceptanceHeadAsOf(context.Background(), run, 0, false)
			if err == nil || got != "" {
				t.Fatalf("= (%q, %v), want (\"\", an error) — never the readable set's candidate", got, err)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

// TestLineageLedgerCategories_MembershipUnchanged pins the exact ordered five
// members of the shared export and the server alias: branch_rebased,
// operator_commit_vouched and integration_commit_recorded stay out.
func TestLineageLedgerCategories_MembershipUnchanged(t *testing.T) {
	want := []string{"pull_request_opened", "child_pushed", "fixup_pushed", "acceptance_scenarios_pushed", "conflict_resolution_pushed"}
	if !reflect.DeepEqual(auditcomplete.ReportedHeadLedgerCategories, want) {
		t.Errorf("auditcomplete.ReportedHeadLedgerCategories = %v, want %v", auditcomplete.ReportedHeadLedgerCategories, want)
	}
	if !reflect.DeepEqual(lineageLedgerCategories, want) {
		t.Errorf("lineageLedgerCategories = %v, want %v", lineageLedgerCategories, want)
	}
}

// TestAcceptanceHeadForRun_SeedArmLogging pins the INFO line naming the
// fan-in arm: emitted exactly when the CHOSEN seed is an integration row, with
// the walked head, and the WARN lines on a read error and on no seed.
func TestAcceptanceHeadForRun_SeedArmLogging(t *testing.T) {
	const infoMsg = "prompt: acceptance head resolved from the consolidated fan-in ledger"
	stageID := uuid.New()
	t.Run("fan-in seed plus a clean rebase logs the merge commit", func(t *testing.T) {
		run := uuid.New()
		s, _ := ahServer(ahIntegration(run, 10, 0, ahF1), ahClean(run, 11, 1, ahF1, ahM))
		buf := captureLogger(s)
		if got := s.acceptanceHeadForRun(context.Background(), run, stageID); got != ahM {
			t.Fatalf("head = %q, want %q", got, ahM)
		}
		lines := linesContaining(buf.String(), infoMsg)
		if len(lines) != 1 {
			t.Fatalf("got %d INFO lines, want exactly one:\n%s", len(lines), buf.String())
		}
		for _, attr := range []string{"level=INFO", "run_id=" + run.String(), "stage_id=" + stageID.String(), "head_sha=" + ahM} {
			if !strings.Contains(lines[0], attr) {
				t.Errorf("INFO line missing %q: %s", attr, lines[0])
			}
		}
	})
	t.Run("a reported seed newer than the integration row logs no fan-in line", func(t *testing.T) {
		run := uuid.New()
		s, _ := ahServer(ahIntegration(run, 10, 0, ahF1), ahHead(run, "fixup_pushed", 11, 1, ahF))
		buf := captureLogger(s)
		if got := s.acceptanceHeadForRun(context.Background(), run, stageID); got != ahF {
			t.Fatalf("head = %q, want the reported head %q", got, ahF)
		}
		if strings.Contains(buf.String(), infoMsg) {
			t.Errorf("fan-in INFO logged for a reported seed:\n%s", buf.String())
		}
	})
	t.Run("a late integration row newer than a reported head logs it", func(t *testing.T) {
		run := uuid.New()
		s, _ := ahServer(ahHead(run, "fixup_pushed", 20, 0, ahF), ahIntegration(run, 30, 0, ahF2))
		buf := captureLogger(s)
		if got := s.acceptanceHeadForRun(context.Background(), run, stageID); got != ahF2 {
			t.Fatalf("head = %q, want %q", got, ahF2)
		}
		lines := linesContaining(buf.String(), infoMsg)
		if len(lines) != 1 || !strings.Contains(lines[0], "head_sha="+ahF2) {
			t.Errorf("want one INFO naming %s, got:\n%s", ahF2, buf.String())
		}
	})
	t.Run("a read error answers empty and WARNs", func(t *testing.T) {
		run := uuid.New()
		s, au := ahServer(ahIntegration(run, 10, 0, ahF1))
		au.listByCategoryErrCategory = CategoryBranchRebased
		buf := captureLogger(s)
		if got := s.acceptanceHeadForRun(context.Background(), run, stageID); got != "" {
			t.Fatalf("head = %q, want empty", got)
		}
		lines := linesContaining(buf.String(), "prompt: resolve acceptance head failed; omitting field")
		if len(lines) != 1 || !strings.Contains(lines[0], "level=WARN") || !strings.Contains(lines[0], "field=acceptance_expected_head_sha") {
			t.Errorf("want one read-error WARN, got:\n%s", buf.String())
		}
	})
	t.Run("no rows answers empty with the existing WARN", func(t *testing.T) {
		run := uuid.New()
		s, _ := ahServer()
		buf := captureLogger(s)
		if got := s.acceptanceHeadForRun(context.Background(), run, stageID); got != "" {
			t.Fatalf("head = %q, want empty", got)
		}
		lines := linesContaining(buf.String(), "prompt: no reported-head audit entry found; omitting field")
		if len(lines) != 1 || !strings.Contains(lines[0], "level=WARN") || !strings.Contains(lines[0], "field=acceptance_expected_head_sha") {
			t.Errorf("want the existing no-entry WARN, got:\n%s", buf.String())
		}
	})
}

func linesContaining(logs, needle string) []string {
	var out []string
	for _, l := range strings.Split(logs, "\n") {
		if strings.Contains(l, needle) {
			out = append(out, l)
		}
	}
	return out
}

// ahValidatedServer wires acceptanceValidatedHeadSHA's real fixture (a
// non-post-deploy acceptance stage) over explicit-sequence rows plus the
// stage's acceptance_dispatched anchor at anchorSeq.
func ahValidatedServer(t *testing.T, anchorSeq int64, rows ...*audit.Entry) (*Server, uuid.UUID, uuid.UUID) {
	t.Helper()
	runID, stageID := uuid.New(), uuid.New()
	s, _, _, au, _ := newAcceptanceServer(t, runID, stageID)
	for _, e := range rows {
		rid := runID
		e.RunID = &rid
	}
	au.seeded = append(au.seeded, rows...)
	seedHeadEntry(au, runID, &stageID, CategoryAcceptanceDispatched, anchorSeq, map[string]any{"stage_id": stageID.String()})
	return s, runID, stageID
}

// TestAcceptanceValidatedHead_SeedOrderedByChainSequence drives row (a)
// through a real anchor: the verdict gets the same sequence answer.
func TestAcceptanceValidatedHead_SeedOrderedByChainSequence(t *testing.T) {
	ph := uuid.Nil
	s, runID, stageID := ahValidatedServer(t, 20,
		ahHead(ph, "pull_request_opened", 10, 5, ahS), ahHead(ph, "fixup_pushed", 12, 0, ahF))
	if got, ok := s.acceptanceValidatedHeadSHA(context.Background(), runID, stageID); !ok || got != ahF {
		t.Errorf("validated head = (%q, %v), want (%q, true) — the higher sequence, not the later timestamp", got, ok, ahF)
	}
}

// TestAcceptanceValidatedHead_LateIntegrationSeed: an integration row newer
// than a reported head at or before the anchor binds the verdict; above the
// anchor it cannot.
func TestAcceptanceValidatedHead_LateIntegrationSeed(t *testing.T) {
	ph := uuid.Nil
	rows := func() []*audit.Entry {
		return []*audit.Entry{ahHead(ph, "acceptance_scenarios_pushed", 20, 5, ahC), ahIntegration(ph, 30, 0, ahF2)}
	}
	s, runID, stageID := ahValidatedServer(t, 35, rows()...)
	if got, ok := s.acceptanceValidatedHeadSHA(context.Background(), runID, stageID); !ok || got != ahF2 {
		t.Errorf("anchor 35: validated head = (%q, %v), want (%q, true)", got, ok, ahF2)
	}
	s, runID, stageID = ahValidatedServer(t, 25, rows()...)
	if got, ok := s.acceptanceValidatedHeadSHA(context.Background(), runID, stageID); !ok || got != ahC {
		t.Errorf("anchor 25: validated head = (%q, %v), want (%q, true) — the integration row above the anchor cannot bind", got, ok, ahC)
	}
}

// TestAcceptanceValidatedHead_AdvancesThroughRebase: the verdict binds to the
// clean rebase's merge commit below the anchor, standalone and parent.
func TestAcceptanceValidatedHead_AdvancesThroughRebase(t *testing.T) {
	ph := uuid.Nil
	for _, tc := range []struct {
		name string
		seed *audit.Entry
		from string
	}{
		{"standalone", ahHead(ph, "pull_request_opened", 10, 0, ahS), ahS},
		{"parent", ahIntegration(ph, 10, 0, ahF1), ahF1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const anchor = 12
			rebase := ahClean(ph, 11, 1, tc.from, ahM)
			if anchor <= 0 || anchor <= rebase.Sequence {
				t.Fatalf("precondition: the anchor %d must sit above the rebase row %d", anchor, rebase.Sequence)
			}
			s, runID, stageID := ahValidatedServer(t, anchor, tc.seed, rebase)
			if got, ok := s.acceptanceValidatedHeadSHA(context.Background(), runID, stageID); !ok || got != ahM {
				t.Errorf("validated head = (%q, %v), want the merge commit (%q, true)", got, ok, ahM)
			}
		})
	}
}

// TestAcceptanceValidatedHead_FixupAfterScenarioCommit is the #4673 shape at
// the verdict binder: PR-open H, the first acceptance episode's corpus commit
// S (category-precedence winner, later timestamp), then a fix-up F recorded
// after S and before the second dispatch anchor. The verdict binds to F, never
// to the prior episode's corpus commit S.
func TestAcceptanceValidatedHead_FixupAfterScenarioCommit(t *testing.T) {
	ph := uuid.Nil
	const anchor = 40
	rows := []*audit.Entry{
		ahHead(ph, "pull_request_opened", 10, 0, ahH),
		ahHead(ph, "acceptance_scenarios_pushed", 20, 9, ahC),
		ahHead(ph, "fixup_pushed", 30, 3, ahF),
	}
	if anchor <= rows[2].Sequence {
		t.Fatalf("precondition: the anchor %d must sit above the fix-up row %d", anchor, rows[2].Sequence)
	}
	s, runID, stageID := ahValidatedServer(t, anchor, rows...)
	got, ok := s.acceptanceValidatedHeadSHA(context.Background(), runID, stageID)
	if !ok || got != ahF {
		t.Errorf("validated head = (%q, %v), want (%q, true) — the fix-up head, not the prior episode's corpus commit %q", got, ok, ahF, ahC)
	}
}
