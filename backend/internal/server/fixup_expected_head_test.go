package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
)

// --- E89.6 / #4574: a recorded base merge counts as fix-up lineage ---

const (
	fixupWalkSeed   = "03797c9700000000000000000000000000000000" // the incident's recorded fix-up head
	fixupWalkMerge  = "e15dab2200000000000000000000000000000000" // the rebase's merge commit
	fixupWalkMerge2 = "e25dab2200000000000000000000000000000000" // a second, chained merge commit
	fixupWalkRacer  = "f0f0f0f000000000000000000000000000000000" // a foreign concurrent push
	fixupWalkBase   = "baseba5e00000000000000000000000000000000" // the base tip merged in
)

// fixupWalkRebasedRow builds a branch_rebased entry BY CONSTRUCTION with the
// field names writeBranchRebasedAudit writes. clean nil omits the
// clean_verified key entirely (a legacy pre-ADR-112 row).
func fixupWalkRebasedRow(seq int64, prior, merge, newHead string, upToDate bool, clean *bool) *audit.Entry {
	fields := map[string]any{
		"prior_head_sha":     prior,
		"new_head_sha":       newHead,
		"merge_commit_sha":   merge,
		"already_up_to_date": upToDate,
		"base_tip_sha":       fixupWalkBase,
	}
	if clean != nil {
		fields["clean_verified"] = *clean
	}
	payload, _ := json.Marshal(fields)
	return &audit.Entry{Category: CategoryBranchRebased, Sequence: seq, Payload: payload}
}

// fixupWalkCleanRow is a clean advance prior -> merge (new head == merge,
// verified).
func fixupWalkCleanRow(seq int64, prior, merge string) *audit.Entry {
	return fixupWalkRebasedRow(seq, prior, merge, merge, false, boolPtr(true))
}

// fixupWalkResetRow builds a branch_reset entry with writeBranchResetAudit's
// field names.
func fixupWalkResetRow(seq int64, prior, resetTo string) *audit.Entry {
	payload, _ := json.Marshal(map[string]any{
		"prior_head_sha":        prior,
		"reset_to_sha":          resetTo,
		"dropped_offending_sha": prior,
	})
	return &audit.Entry{Category: CategoryBranchReset, Sequence: seq, Payload: payload}
}

// TestWalkRecordedBaseMerges pins every mode of the pure walk: one row per
// mode, entries built by construction with explicit sequences. The seed is at
// sequence 10, so every participating row sits above it.
func TestWalkRecordedBaseMerges(t *testing.T) {
	const seedSeq = 10
	tests := []struct {
		name string
		rows []*audit.Entry
		want string
	}{
		{"no rows resolves the seed", nil, fixupWalkSeed},
		{"the incident shape: one clean advance of the seed resolves the merge commit",
			[]*audit.Entry{fixupWalkCleanRow(11, fixupWalkSeed, fixupWalkMerge)}, fixupWalkMerge},
		{"two chained clean advances resolve the second merge commit",
			[]*audit.Entry{fixupWalkCleanRow(11, fixupWalkSeed, fixupWalkMerge), fixupWalkCleanRow(12, fixupWalkMerge, fixupWalkMerge2)}, fixupWalkMerge2},
		{"a concurrent push (new head != merge commit) does not advance",
			[]*audit.Entry{fixupWalkRebasedRow(11, fixupWalkSeed, fixupWalkMerge, fixupWalkRacer, false, boolPtr(true))}, fixupWalkSeed},
		{"clean_verified false does not advance",
			[]*audit.Entry{fixupWalkRebasedRow(11, fixupWalkSeed, fixupWalkMerge, fixupWalkMerge, false, boolPtr(false))}, fixupWalkSeed},
		{"a legacy row without clean_verified does not advance",
			[]*audit.Entry{fixupWalkRebasedRow(11, fixupWalkSeed, fixupWalkMerge, fixupWalkMerge, false, nil)}, fixupWalkSeed},
		{"an already-up-to-date row does not advance",
			[]*audit.Entry{fixupWalkRebasedRow(11, fixupWalkSeed, fixupWalkMerge, fixupWalkMerge, true, boolPtr(true))}, fixupWalkSeed},
		{"a clean advance of a head the walk never reached does not advance",
			[]*audit.Entry{fixupWalkCleanRow(11, fixupWalkRacer, fixupWalkMerge)}, fixupWalkSeed},
		{"a clean advance recorded before the seed does not advance",
			[]*audit.Entry{fixupWalkCleanRow(seedSeq-1, fixupWalkSeed, fixupWalkMerge)}, fixupWalkSeed},
		{"a clean advance at the seed's own sequence does not advance",
			[]*audit.Entry{fixupWalkCleanRow(seedSeq, fixupWalkSeed, fixupWalkMerge)}, fixupWalkSeed},
		{"out-of-order delivery still chains in sequence order",
			[]*audit.Entry{fixupWalkCleanRow(12, fixupWalkMerge, fixupWalkMerge2), fixupWalkCleanRow(11, fixupWalkSeed, fixupWalkMerge)}, fixupWalkMerge2},
		{"a reset after a clean advance back to the seed resolves the seed",
			[]*audit.Entry{fixupWalkCleanRow(11, fixupWalkSeed, fixupWalkMerge), fixupWalkResetRow(12, fixupWalkMerge, fixupWalkSeed)}, fixupWalkSeed},
		{"a reset to the walked merge commit resolves the merge commit",
			[]*audit.Entry{fixupWalkCleanRow(11, fixupWalkSeed, fixupWalkMerge), fixupWalkCleanRow(12, fixupWalkMerge, fixupWalkMerge2), fixupWalkResetRow(13, fixupWalkMerge2, fixupWalkMerge)}, fixupWalkMerge},
		{"a reset to a sha outside the walked chain resolves the seed",
			[]*audit.Entry{fixupWalkCleanRow(11, fixupWalkSeed, fixupWalkMerge), fixupWalkResetRow(12, fixupWalkMerge, fixupWalkRacer)}, fixupWalkSeed},
		{"an undecodable branch_rebased row is skipped",
			[]*audit.Entry{{Category: CategoryBranchRebased, Sequence: 11, Payload: []byte("{not json")}, fixupWalkCleanRow(12, fixupWalkSeed, fixupWalkMerge)}, fixupWalkMerge},
		{"an undecodable branch_reset row resolves the seed",
			[]*audit.Entry{fixupWalkCleanRow(11, fixupWalkSeed, fixupWalkMerge), {Category: CategoryBranchReset, Sequence: 12, Payload: []byte("{not json")}}, fixupWalkSeed},
		{"nil rows and unrelated categories are ignored",
			[]*audit.Entry{nil, {Category: "fixup_pushed", Sequence: 11, Payload: []byte(`{"head_sha":"` + fixupWalkRacer + `"}`)}, fixupWalkCleanRow(12, fixupWalkSeed, fixupWalkMerge)}, fixupWalkMerge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := walkRecordedBaseMerges(tt.rows, seedSeq, fixupWalkSeed); got != tt.want {
				t.Errorf("walkRecordedBaseMerges = %q, want %q", got, tt.want)
			}
		})
	}
}

// rebasedEntries returns the branch_rebased rows the REAL rebase handler
// appended, decoded through the shared advance struct.
func rebasedEntries(t *testing.T, au *auditFake) []branchRebasedAdvance {
	t.Helper()
	au.mu.Lock()
	defer au.mu.Unlock()
	var out []branchRebasedAdvance
	for _, ap := range au.appended {
		if ap.Category != CategoryBranchRebased {
			continue
		}
		var p branchRebasedAdvance
		if err := json.Unmarshal(ap.Payload, &p); err != nil {
			t.Fatalf("decode branch_rebased: %v", err)
		}
		out = append(out, p)
	}
	return out
}

// rebaseWithRecordedFixupHead seeds a rebase run whose recorded head is a
// fixup_pushed at rebasePriorHeadSHA (sequence 0, below every append), drives
// the REAL rebase handler, and returns the seed plus the decoded response.
func rebaseWithRecordedFixupHead(t *testing.T, stub *rebaseGitHub) (*rebaseSeed, uuid.UUID, rebaseBranchResponse) {
	t.Helper()
	sd := seedRebaseRun(t, stub, rebaseOpts{})
	sd.au.stampSequence = true
	seedRunHeadEntry(sd.au, sd.runID, "fixup_pushed", rebasePriorHeadSHA, 0)
	w := postRebaseBranch(t, sd.s, sd.runID,
		rebaseBranchRequest{Reason: "advance", Confirm: true}, withRebaseOperator)
	if w.Code != http.StatusOK {
		t.Fatalf("rebase status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp rebaseBranchResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal rebase response: %v", err)
	}
	if resp.MergeCommitSHA != rebaseNewHeadSHA {
		t.Fatalf("precondition: merge_commit_sha = %q, want %q", resp.MergeCommitSHA, rebaseNewHeadSHA)
	}
	return sd, uuid.New(), resp
}

// TestResolveFixupExpectedHeadSHA_AdvancesThroughRecordedRebase drives the
// REAL rebase handler against the GitHub stub, so the writer -> reader field
// seam (writeBranchRebasedAudit's payload keys -> walkRecordedBaseMerges) is
// pinned, not a hand-built row: after a clean rebase of the recorded fix-up
// head the fix-up expectation is the merge commit (the run 3fa73698 incident).
// The acceptance expectation is deliberately unchanged.
func TestResolveFixupExpectedHeadSHA_AdvancesThroughRecordedRebase(t *testing.T) {
	sd, stageID, resp := rebaseWithRecordedFixupHead(t, cleanRebaseStub())
	if resp.NewHeadSHA != rebaseNewHeadSHA {
		t.Fatalf("precondition: new_head_sha = %q, want the merge commit %q", resp.NewHeadSHA, rebaseNewHeadSHA)
	}
	rows := rebasedEntries(t, sd.au)
	if len(rows) != 1 || rows[0].CleanVerified == nil || !*rows[0].CleanVerified {
		t.Fatalf("precondition: want exactly one branch_rebased row with clean_verified true, got %+v", rows)
	}

	ctx := context.Background()
	if got := sd.s.resolveFixupExpectedHeadSHA(ctx, sd.runID, stageID); got != rebaseNewHeadSHA {
		t.Errorf("fixup_expected_head_sha = %q, want the recorded merge commit %q (a fix-up after a clean rebase must not fail fixup_base_mismatch)",
			got, rebaseNewHeadSHA)
	}
	if got := sd.s.resolveNewestReportedHeadSHA(ctx, sd.runID, stageID, "acceptance_expected_head_sha"); got != rebasePriorHeadSHA {
		t.Errorf("acceptance expected head = %q, want the un-advanced recorded head %q (acceptance is out of scope)",
			got, rebasePriorHeadSHA)
	}
}

// TestResolveFixupExpectedHeadSHA_ConcurrentPushRebaseDoesNotAdvance: a foreign
// push lands after the merge, so the recorded new head is NOT the merge commit.
// The expectation stays the recorded head, and the runner's equality check
// refuses the fix-up (ADR-035: the racer is never laundered into the base).
func TestResolveFixupExpectedHeadSHA_ConcurrentPushRebaseDoesNotAdvance(t *testing.T) {
	stub := cleanRebaseStub()
	stub.headSHASeq = []string{rebasePriorHeadSHA, rebasePriorHeadSHA, rebaseRacerSHA}
	sd, stageID, resp := rebaseWithRecordedFixupHead(t, stub)
	if resp.NewHeadSHA != rebaseRacerSHA {
		t.Fatalf("precondition: new_head_sha = %q, want the raced-in head %q", resp.NewHeadSHA, rebaseRacerSHA)
	}
	if got := sd.s.resolveFixupExpectedHeadSHA(context.Background(), sd.runID, stageID); got != rebasePriorHeadSHA {
		t.Errorf("fixup_expected_head_sha = %q, want the un-advanced recorded head %q (a concurrent push must not advance the base)",
			got, rebasePriorHeadSHA)
	}
}

// TestResolveFixupExpectedHeadSHA_LaterFixupPushSupersedesRebase: a fix-up
// push recorded after the rebase is the new seed, and no base merge follows
// it, so its head is the expectation.
func TestResolveFixupExpectedHeadSHA_LaterFixupPushSupersedesRebase(t *testing.T) {
	sd, stageID, _ := rebaseWithRecordedFixupHead(t, cleanRebaseStub())
	const later = "9a9a9a9a00000000000000000000000000000000"
	seedRunHeadEntry(sd.au, sd.runID, "fixup_pushed", later, 1000)
	sd.au.mu.Lock()
	sd.au.seeded[len(sd.au.seeded)-1].Timestamp = time.Now().Add(time.Hour)
	sd.au.mu.Unlock()
	if got := sd.s.resolveFixupExpectedHeadSHA(context.Background(), sd.runID, stageID); got != later {
		t.Errorf("fixup_expected_head_sha = %q, want the later fix-up head %q", got, later)
	}
}

// TestResolveFixupExpectedHeadSHA_BaseMergeReadErrorKeepsRecordedHead: a read
// error on either base-merge category resolves the un-advanced recorded head —
// never the merge commit (an unread reset could have dropped it) and never ""
// (which would switch the runner to its no-comparison path).
func TestResolveFixupExpectedHeadSHA_BaseMergeReadErrorKeepsRecordedHead(t *testing.T) {
	for _, cat := range []string{CategoryBranchRebased, CategoryBranchReset} {
		t.Run(cat, func(t *testing.T) {
			runID := uuid.New()
			au := newAuditFake()
			seedRunHeadEntry(au, runID, "fixup_pushed", fixupWalkSeed, 1)
			row := fixupWalkCleanRow(2, fixupWalkSeed, fixupWalkMerge)
			row.RunID = &runID
			au.seeded = append(au.seeded, row)
			s := New(Config{Addr: "127.0.0.1:0", AuditRepo: au})
			// Precondition: without the injected error the row advances, so a
			// seed result below is the error branch, not a non-advancing row.
			if got := s.resolveFixupExpectedHeadSHA(context.Background(), runID, uuid.New()); got != fixupWalkMerge {
				t.Fatalf("precondition: fixup_expected_head_sha = %q, want %q", got, fixupWalkMerge)
			}
			au.listByCategoryErrCategory = cat
			if got := s.resolveFixupExpectedHeadSHA(context.Background(), runID, uuid.New()); got != fixupWalkSeed {
				t.Errorf("fixup_expected_head_sha = %q on a %s read error, want the recorded head %q (never the merge, never empty)",
					got, cat, fixupWalkSeed)
			}
		})
	}
}

// TestResolveFixupExpectedHeadSHA_NoRecordedHeadStaysEmpty: a rebase alone
// never invents a head — with no reported head the field stays "" (the
// runner's checkout-only path), even with a clean branch_rebased row present.
func TestResolveFixupExpectedHeadSHA_NoRecordedHeadStaysEmpty(t *testing.T) {
	runID := uuid.New()
	au := newAuditFake()
	row := fixupWalkCleanRow(2, fixupWalkSeed, fixupWalkMerge)
	row.RunID = &runID
	au.seeded = append(au.seeded, row)
	s := New(Config{Addr: "127.0.0.1:0", AuditRepo: au})
	if got := s.resolveFixupExpectedHeadSHA(context.Background(), runID, uuid.New()); got != "" {
		t.Errorf("fixup_expected_head_sha = %q, want empty with no recorded head", got)
	}
}
