package server

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
)

func rebasedRow(seq int64, prior, newHead, merge string, cleanVerified any) *audit.Entry {
	fields := map[string]any{
		"prior_head_sha":     prior,
		"new_head_sha":       newHead,
		"merge_commit_sha":   merge,
		"already_up_to_date": false,
	}
	if cleanVerified != nil {
		fields["clean_verified"] = cleanVerified
	}
	raw, _ := json.Marshal(fields)
	return &audit.Entry{Sequence: seq, Category: CategoryBranchRebased, Payload: raw}
}

// cleanRow is a parent-verified clean advance prior → newHead (= its merge
// commit).
func cleanRow(seq int64, prior, newHead string) *audit.Entry {
	return rebasedRow(seq, prior, newHead, newHead, true)
}

func conflictRow(seq int64, head string) *audit.Entry {
	raw, _ := json.Marshal(map[string]any{"head_sha": head})
	return &audit.Entry{Sequence: seq, Category: CategoryConflictResolutionPushed, Payload: raw}
}

func TestIsCleanAdvance(t *testing.T) {
	tru, fls := true, false
	cases := []struct {
		name string
		p    branchRebasedAdvance
		want bool
	}{
		{"parent-verified merge", branchRebasedAdvance{PriorHeadSHA: "a", NewHeadSHA: "m", MergeCommitSHA: "m", CleanVerified: &tru}, true},
		{"legacy row: no clean_verified key", branchRebasedAdvance{PriorHeadSHA: "a", NewHeadSHA: "m", MergeCommitSHA: "m"}, false},
		{"clean_verified false", branchRebasedAdvance{PriorHeadSHA: "a", NewHeadSHA: "m", MergeCommitSHA: "m", CleanVerified: &fls}, false},
		{"already up to date", branchRebasedAdvance{PriorHeadSHA: "a", NewHeadSHA: "a", AlreadyUpToDate: true, CleanVerified: &tru}, false},
		{"empty merge sha", branchRebasedAdvance{PriorHeadSHA: "a", NewHeadSHA: "x", CleanVerified: &tru}, false},
		{"concurrent push: new head != merge commit", branchRebasedAdvance{PriorHeadSHA: "a", NewHeadSHA: "x", MergeCommitSHA: "m", CleanVerified: &tru}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isCleanAdvance(tc.p); got != tc.want {
				t.Errorf("isCleanAdvance = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestIsCleanAdvance_LegacyRowFromWirePayload decodes a pre-change
// branch_rebased payload (no clean_verified key) from raw bytes: absent is
// not true.
func TestIsCleanAdvance_LegacyRowFromWirePayload(t *testing.T) {
	var p branchRebasedAdvance
	if err := json.Unmarshal([]byte(`{"prior_head_sha":"a","new_head_sha":"m","merge_commit_sha":"m","already_up_to_date":false}`), &p); err != nil {
		t.Fatal(err)
	}
	if isCleanAdvance(p) {
		t.Error("a legacy branch_rebased row with no clean_verified key is clean; want NOT clean")
	}
	if ok, _ := cleanChainReaches([]*audit.Entry{rebasedRow(1, "a", "m", "m", nil)}, "a", "m"); ok {
		t.Error("the chain walk carried an approval across a legacy (unverified) row")
	}
}

func TestCleanChainReaches(t *testing.T) {
	cases := []struct {
		name      string
		rows      []*audit.Entry
		from, to  string
		want      bool
		wantChain []string
	}{
		{"same head", nil, "h0", "h0", true, []string{"h0"}},
		{"clean H0→H1→H2 reaches", []*audit.Entry{cleanRow(1, "h0", "h1"), cleanRow(2, "h1", "h2")}, "h0", "h2", true, []string{"h0", "h1", "h2"}},
		{"fix-up-shaped H1→H2 (new head != merge commit) breaks", []*audit.Entry{cleanRow(1, "h0", "h1"), rebasedRow(2, "h1", "h2", "mX", true)}, "h0", "h2", false, nil},
		{"unverified parents break", []*audit.Entry{cleanRow(1, "h0", "h1"), rebasedRow(2, "h1", "h2", "h2", false)}, "h0", "h2", false, nil},
		{"conflict_resolution_pushed clears", []*audit.Entry{cleanRow(1, "h0", "h1"), conflictRow(2, "hc"), cleanRow(3, "h1", "h2")}, "h0", "h2", false, nil},
		{"review AT the conflict head re-seeds", []*audit.Entry{cleanRow(1, "h0", "h1"), conflictRow(2, "hc"), cleanRow(3, "hc", "h2")}, "hc", "h2", true, []string{"hc", "h2"}},
		{"no edge from the review commit", []*audit.Entry{cleanRow(1, "h0", "h1")}, "hz", "h1", false, nil},
		{"empty from", nil, "", "h1", false, nil},
		{"undecodable row carries nothing", []*audit.Entry{{Sequence: 1, Category: CategoryBranchRebased, Payload: json.RawMessage(`{`)}}, "h0", "h1", false, nil},
		{"other categories ignored", []*audit.Entry{{Sequence: 1, Category: "operator_commit_vouched", Payload: json.RawMessage(`{"prior_head_sha":"h0","new_head_sha":"h1"}`)}, cleanRow(2, "h0", "h1")}, "h0", "h1", true, []string{"h0", "h1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, chain := cleanChainReaches(tc.rows, tc.from, tc.to)
			if got != tc.want || !reflect.DeepEqual(chain, tc.wantChain) {
				t.Errorf("cleanChainReaches = %v %v, want %v %v", got, chain, tc.want, tc.wantChain)
			}
		})
	}
}

// TestCleanChainReaches_SeededAtReviewCommitNotIntentHead pins the seed: an
// approval at H1 (after the intent's anchored H0) reaches H2 through the H1→H2
// clean advance, while an approval at an unrelated head does not reach it even
// though H0 (an intent-shaped anchor) does.
func TestCleanChainReaches_SeededAtReviewCommitNotIntentHead(t *testing.T) {
	rows := []*audit.Entry{cleanRow(1, "h0", "h1"), cleanRow(2, "h1", "h2")}
	if ok, chain := cleanChainReaches(rows, "h1", "h2"); !ok || !reflect.DeepEqual(chain, []string{"h1", "h2"}) {
		t.Errorf("seeded at h1: %v %v, want reached [h1 h2]", ok, chain)
	}
	if ok, _ := cleanChainReaches(rows, "hreview", "h2"); ok {
		t.Error("an approval on a commit with no recorded clean chain reached h2")
	}
}

// TestCleanChainReaches_SequenceOrderOnly delivers the rows out of order: the
// walk orders by audit sequence, so the conflict row (seq 2) still sits
// between the two advances and clears.
func TestCleanChainReaches_SequenceOrderOnly(t *testing.T) {
	inOrder := []*audit.Entry{cleanRow(3, "h1", "h2"), cleanRow(1, "h0", "h1")}
	if ok, chain := cleanChainReaches(inOrder, "h0", "h2"); !ok || !reflect.DeepEqual(chain, []string{"h0", "h1", "h2"}) {
		t.Errorf("out-of-order clean rows: %v %v, want reached [h0 h1 h2]", ok, chain)
	}
	withConflict := []*audit.Entry{cleanRow(3, "h1", "h2"), conflictRow(2, "hc"), cleanRow(1, "h0", "h1")}
	if ok, _ := cleanChainReaches(withConflict, "h0", "h2"); ok {
		t.Error("out-of-order rows: the conflict row at seq 2 did not clear the chain")
	}
}
