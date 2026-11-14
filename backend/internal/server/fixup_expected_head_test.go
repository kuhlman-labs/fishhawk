package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/auditcomplete"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
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

// fixupWalkVouched is a vouched tip V (#4856).
const fixupWalkVouched = "7a0c4ed000000000000000000000000000000000"

// fixupWalkVouchRow builds an operator_commit_vouched entry BY CONSTRUCTION
// with arbitrary payload fields.
func fixupWalkVouchRow(seq int64, fields map[string]any) *audit.Entry {
	payload, _ := json.Marshal(fields)
	return &audit.Entry{Category: CategoryOperatorCommitVouched, Sequence: seq, Payload: payload}
}

// fixupWalkVerifiedVouchRow is the handler's verified shape.
func fixupWalkVerifiedVouchRow(seq int64, prior, vouched string) *audit.Entry {
	return fixupWalkVouchRow(seq, map[string]any{
		"run_id": "r", auditcomplete.VouchedSHAField: vouched, "reason": "x",
		auditcomplete.VouchPriorHeadSHAField: prior, auditcomplete.VouchHeadAdvanceVerifiedField: true,
	})
}

// fixupWalkLegacyVouchRow is today's {run_id, vouched_sha, reason} key set.
func fixupWalkLegacyVouchRow(seq int64, vouched string) *audit.Entry {
	return fixupWalkVouchRow(seq, map[string]any{"run_id": "r", "vouched_sha": vouched, "reason": "x"})
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
		// #4856: the verified-vouch arm.
		{"(1) a verified vouch whose prior is the seed advances to the vouched sha",
			[]*audit.Entry{fixupWalkVerifiedVouchRow(11, fixupWalkSeed, fixupWalkVouched)}, fixupWalkVouched},
		{"(2) a verified vouch whose prior is not the walked head is skipped",
			[]*audit.Entry{fixupWalkVerifiedVouchRow(11, fixupWalkRacer, fixupWalkVouched)}, fixupWalkSeed},
		{"(3) a vouch with the seed as prior and head_advance_verified absent is skipped",
			[]*audit.Entry{fixupWalkVouchRow(11, map[string]any{"vouched_sha": fixupWalkVouched, "prior_head_sha": fixupWalkSeed})}, fixupWalkSeed},
		{"(4) a vouch with the seed as prior and head_advance_verified false is skipped",
			[]*audit.Entry{fixupWalkVouchRow(11, map[string]any{"vouched_sha": fixupWalkVouched, "prior_head_sha": fixupWalkSeed, "head_advance_verified": false})}, fixupWalkSeed},
		{"(5) a legacy vouch with neither advance key is skipped",
			[]*audit.Entry{fixupWalkLegacyVouchRow(11, fixupWalkVouched)}, fixupWalkSeed},
		{"(6) the rebase verb's attribution row is skipped",
			[]*audit.Entry{fixupWalkVouchRow(11, map[string]any{"run_id": "r", "vouched_sha": fixupWalkSeed, "reason": "fishhawk_rebase_run_branch advanced run/branch onto main"})}, fixupWalkSeed},
		{"(7) an undecodable vouch payload is skipped and the walk continues",
			[]*audit.Entry{{Category: CategoryOperatorCommitVouched, Sequence: 11, Payload: []byte("{not json")}, fixupWalkCleanRow(12, fixupWalkSeed, fixupWalkMerge)}, fixupWalkMerge},
		{"(8) a verified vouch with an empty vouched_sha is skipped",
			[]*audit.Entry{fixupWalkVerifiedVouchRow(11, fixupWalkSeed, "")}, fixupWalkSeed},
		{"(9) a verified vouch then a reset to the vouched sha stays at the vouched sha",
			[]*audit.Entry{fixupWalkVerifiedVouchRow(11, fixupWalkSeed, fixupWalkVouched), fixupWalkResetRow(12, fixupWalkRacer, fixupWalkVouched)}, fixupWalkVouched},
		{"(10) a verified vouch then a clean rebase of the vouched sha advances to the merge (run 5fcb6201)",
			[]*audit.Entry{fixupWalkVerifiedVouchRow(11, fixupWalkSeed, fixupWalkVouched), fixupWalkCleanRow(12, fixupWalkVouched, fixupWalkMerge)}, fixupWalkMerge},
		{"(11) a verified vouch at or below the seed's sequence is ignored",
			[]*audit.Entry{fixupWalkVerifiedVouchRow(seedSeq, fixupWalkSeed, fixupWalkVouched)}, fixupWalkSeed},
		{"(C7) legacy vouch, then a rebase of it, then a re-vouch ends at the vouched tip, not the merge",
			[]*audit.Entry{fixupWalkLegacyVouchRow(11, fixupWalkVouched), fixupWalkCleanRow(12, fixupWalkVouched, fixupWalkMerge), fixupWalkVerifiedVouchRow(13, fixupWalkSeed, fixupWalkVouched)}, fixupWalkVouched},
		{"(C7 exit) a fresh clean rebase from the re-vouched tip walks through to its merge",
			[]*audit.Entry{fixupWalkLegacyVouchRow(11, fixupWalkVouched), fixupWalkCleanRow(12, fixupWalkVouched, fixupWalkMerge), fixupWalkVerifiedVouchRow(13, fixupWalkSeed, fixupWalkVouched), fixupWalkCleanRow(14, fixupWalkVouched, fixupWalkMerge2)}, fixupWalkMerge2},
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
// Since #4874 the acceptance expectation walks the same clean rebase, so it
// names the merge commit too.
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
	if got := sd.s.acceptanceHeadForRun(ctx, sd.runID, stageID); got != rebaseNewHeadSHA {
		t.Errorf("acceptance expected head = %q, want the recorded merge commit %q (#4874)",
			got, rebaseNewHeadSHA)
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

// TestResolveFixupExpectedHeadSHA_SeedKeepsTimestampOrdering feeds a clock-
// inverted chain to BOTH resolvers (#4874): F at seq 10 with a later timestamp
// and S at seq 11 with an earlier one. The fix-up seed keeps its timestamp
// ordering and answers F; the acceptance head orders by chain sequence and
// answers S, so nothing was smuggled into fix-up.
func TestResolveFixupExpectedHeadSHA_SeedKeepsTimestampOrdering(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, _ := ahServer(
		ahHead(runID, "fixup_pushed", 10, 5, ahF),
		ahHead(runID, "pull_request_opened", 11, 0, ahS),
	)
	ctx := context.Background()
	if got := s.resolveFixupExpectedHeadSHA(ctx, runID, stageID); got != ahF {
		t.Errorf("fixup_expected_head_sha = %q, want %q (timestamp-then-sequence, unchanged)", got, ahF)
	}
	if got, err := s.acceptanceHeadAsOf(ctx, runID, 0, false); err != nil || got != ahS {
		t.Errorf("acceptanceHeadAsOf = (%q, %v), want (%q, nil) (chain sequence)", got, err, ahS)
	}
}

// TestVouchAdvanceRow_DecodesSeamKeys pins that vouchAdvanceRow's json tags are
// the auditcomplete seam constants the writer uses (approval condition C6).
func TestVouchAdvanceRow_DecodesSeamKeys(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{
		auditcomplete.VouchedSHAField:               fixupWalkVouched,
		auditcomplete.VouchPriorHeadSHAField:        fixupWalkSeed,
		auditcomplete.VouchHeadAdvanceVerifiedField: true,
	})
	var p vouchAdvanceRow
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if p.VouchedSHA != fixupWalkVouched || p.PriorHeadSHA != fixupWalkSeed || p.HeadAdvanceVerified == nil || !*p.HeadAdvanceVerified {
		t.Errorf("decoded %+v: the struct tags drifted from the auditcomplete seam constants", p)
	}
	if !isVerifiedVouchAdvance(p) {
		t.Error("isVerifiedVouchAdvance(writer shape) = false, want true")
	}
}

// TestResolveFixupExpectedHeadSHA_AdvancesThroughVerifiedVouch drives the REAL
// vouch handler (writer to reader field seam) over a recorded
// acceptance_scenarios_pushed head H (the run 3a390a2e shape): after a verified
// vouch of V the fix-up expectation is V, and resolveLastRunAuthoredHead with
// tip V answers V too, so reset and the fix-up base check agree.
func TestResolveFixupExpectedHeadSHA_AdvancesThroughVerifiedVouch(t *testing.T) {
	f := newVouchProbeFixture(t, vouchProbeOpts{})
	f.seedHead("acceptance_scenarios_pushed", vouchProbeHead)
	f.forge.set(vouchProbeTip, vouchProbeHead)
	f.forge.set(vouchProbeHead, vouchProbeTip, vouchProbeTip)
	f.forge.set("main", vouchProbeTip, vouchProbeHead, vouchProbeTip)
	if resp := f.vouch(t, vouchProbeTip); !resp.RecordedHeadAdvanced {
		t.Fatalf("vouch did not advance: %q", resp.RecordedHeadAdvanceNote)
	}
	ctx := context.Background()
	got := f.s.resolveFixupExpectedHeadSHA(ctx, f.runRow.ID, f.stageID)
	if got != vouchProbeTip {
		t.Fatalf("resolveFixupExpectedHeadSHA = %q, want the vouched tip %q", got, vouchProbeTip)
	}
	repo, _ := parseRepoOwnerName(f.runRow.Repo)
	last, _, _, ok := f.s.resolveLastRunAuthoredHead(ctx, f.runRow, forge.FromGitHubInstallationID(99), repo, vouchProbeTip, 7)
	if !ok || last != got {
		t.Errorf("resolveLastRunAuthoredHead = (%q, ok=%v), want %q == the fix-up head", last, ok, got)
	}
}

// TestResolveFixupExpectedHeadSHA_UnverifiedVouchKeepsRecordedHead: a legacy
// vouch row is membership-only.
func TestResolveFixupExpectedHeadSHA_UnverifiedVouchKeepsRecordedHead(t *testing.T) {
	f := newVouchProbeFixture(t, vouchProbeOpts{})
	f.seedHead("acceptance_scenarios_pushed", vouchProbeHead)
	f.appendRow(CategoryOperatorCommitVouched, map[string]any{"run_id": f.runRow.ID.String(), "vouched_sha": vouchProbeTip, "reason": "x"})
	if got := f.s.resolveFixupExpectedHeadSHA(context.Background(), f.runRow.ID, f.stageID); got != vouchProbeHead {
		t.Errorf("resolveFixupExpectedHeadSHA = %q, want the recorded head %q", got, vouchProbeHead)
	}
}

// TestResolveFixupExpectedHeadSHA_VouchReadErrorKeepsRecordedHead: a read error
// on the vouch category answers the seed, never "".
func TestResolveFixupExpectedHeadSHA_VouchReadErrorKeepsRecordedHead(t *testing.T) {
	f := newVouchProbeFixture(t, vouchProbeOpts{})
	f.seedHead("fixup_pushed", vouchProbeHead)
	f.seedVerifiedVouch(vouchProbeHead, vouchProbeTip)
	f.au.listByCategoryErrCategory = CategoryOperatorCommitVouched
	if got := f.s.resolveFixupExpectedHeadSHA(context.Background(), f.runRow.ID, f.stageID); got != vouchProbeHead {
		t.Errorf("resolveFixupExpectedHeadSHA = %q, want the seed %q", got, vouchProbeHead)
	}
}

// TestRecordedFixupHead_AgreesWithFixupResolver: the strict resolver and the
// dispatch-time resolver share one seed rule and one walk.
func TestRecordedFixupHead_AgreesWithFixupResolver(t *testing.T) {
	tests := []struct {
		name string
		seed func(f *vouchProbeFixture)
		want string
	}{
		{"no seed", func(*vouchProbeFixture) {}, ""},
		{"seed only", func(f *vouchProbeFixture) { f.seedHead("fixup_pushed", vouchProbeHead) }, vouchProbeHead},
		{"newest reported head wins", func(f *vouchProbeFixture) {
			f.seedHead("pull_request_opened", vouchProbeOlder)
			f.seedHead("fixup_pushed", vouchProbeHead)
		}, vouchProbeHead},
		{"verified vouch advances", func(f *vouchProbeFixture) {
			f.seedHead("fixup_pushed", vouchProbeHead)
			f.seedVerifiedVouch(vouchProbeHead, vouchProbeTip)
		}, vouchProbeTip},
		{"verified vouch then clean rebase", func(f *vouchProbeFixture) {
			f.seedHead("fixup_pushed", vouchProbeHead)
			f.seedVerifiedVouch(vouchProbeHead, vouchProbeTip)
			f.appendRow(CategoryBranchRebased, map[string]any{"prior_head_sha": vouchProbeTip, "new_head_sha": fixupWalkMerge, "merge_commit_sha": fixupWalkMerge, "already_up_to_date": false, "clean_verified": true})
		}, fixupWalkMerge},
		{"legacy vouch stays", func(f *vouchProbeFixture) {
			f.seedHead("fixup_pushed", vouchProbeHead)
			f.appendRow(CategoryOperatorCommitVouched, map[string]any{"vouched_sha": vouchProbeTip})
		}, vouchProbeHead},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newVouchProbeFixture(t, vouchProbeOpts{})
			tt.seed(f)
			ctx := context.Background()
			strict, err := f.s.recordedFixupHead(ctx, f.runRow.ID)
			if err != nil {
				t.Fatalf("recordedFixupHead: %v", err)
			}
			lenient := f.s.resolveFixupExpectedHeadSHA(ctx, f.runRow.ID, f.stageID)
			if strict != lenient || strict != tt.want {
				t.Errorf("recordedFixupHead = %q, resolveFixupExpectedHeadSHA = %q, want both %q", strict, lenient, tt.want)
			}
		})
	}
}

// TestRecordedFixupHead_ReadErrorReturned: every category read error is
// RETURNED, never degraded to the seed.
func TestRecordedFixupHead_ReadErrorReturned(t *testing.T) {
	cats := append(append([]string{}, lineageLedgerCategories...), fixupHeadWalkCategories...)
	for _, cat := range cats {
		t.Run(cat, func(t *testing.T) {
			f := newVouchProbeFixture(t, vouchProbeOpts{})
			f.seedHead("fixup_pushed", vouchProbeHead)
			f.au.listByCategoryErrCategory = cat
			got, err := f.s.recordedFixupHead(context.Background(), f.runRow.ID)
			if err == nil {
				t.Fatalf("recordedFixupHead = (%q, nil), want the %s read error", got, cat)
			}
			if got != "" {
				t.Errorf("recordedFixupHead head = %q on error, want \"\"", got)
			}
		})
	}
}

// TestNewestReportedHeadEntry_SkipsUnusableRows: a nil entry, an undecodable
// payload and an empty head_sha never become the seed, even when newer.
func TestNewestReportedHeadEntry_SkipsUnusableRows(t *testing.T) {
	base := time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)
	good := &audit.Entry{Timestamp: base, Sequence: 1, Payload: json.RawMessage(`{"head_sha":"` + fixupWalkSeed + `"}`)}
	entries := []*audit.Entry{
		good,
		nil,
		{Timestamp: base.Add(time.Minute), Sequence: 2, Payload: json.RawMessage(`not json`)},
		{Timestamp: base.Add(2 * time.Minute), Sequence: 3, Payload: json.RawMessage(`{"head_sha":""}`)},
	}
	gotEntry, gotSHA := newestReportedHeadEntry(entries)
	if gotEntry != good || gotSHA != fixupWalkSeed {
		t.Errorf("newestReportedHeadEntry = (%v, %q), want the only usable row (%q)", gotEntry, gotSHA, fixupWalkSeed)
	}
	if e, sha := newestReportedHeadEntry(entries[1:]); e != nil || sha != "" {
		t.Errorf("newestReportedHeadEntry(no usable row) = (%v, %q), want (nil, \"\")", e, sha)
	}
}

// TestRecordedFixupHead_NoAuditRepo: an unconfigured audit repository answers
// no recorded head and no error.
func TestRecordedFixupHead_NoAuditRepo(t *testing.T) {
	s := &Server{}
	got, err := s.recordedFixupHead(context.Background(), uuid.New())
	if got != "" || err != nil {
		t.Errorf("recordedFixupHead = (%q, %v), want (\"\", nil)", got, err)
	}
}
