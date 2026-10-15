package server

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// --- ADR-090 D8 Amendment 1 (#4368): the operator-vouched head probe -------
//
// Every arm drives the REAL mergeCandidateVerifyState on an mcGateFixture (a
// fully probeable run: GitHub stub, installation, repo, PR url) whose stub
// serves per-pair compares, so the probe's forge reads, its reference-head
// choice and its decision are all on the path.

const (
	// mcvGatedHead is P: a runner-gated (pull_request_opened) head.
	mcvGatedHead = "3333333333333333333333333333333333333333"
	// mcvBaseCommit1/2 are commits the base contains and P lacks.
	mcvBaseCommit1 = "4444444444444444444444444444444444444444"
	mcvBaseCommit2 = "5555555555555555555555555555555555555555"
	// mcvNewerVerified is V2: a head a passed merge-candidate verdict (or a
	// transfer) covers, newer on the chain than P.
	mcvNewerVerified = "6666666666666666666666666666666666666666"
	// mcvOperatorMerge is M: an operator merge of the base, vouched, never
	// verified.
	mcvOperatorMerge = "7777777777777777777777777777777777777777"
)

func compareKey(base, head string) string { return base + "..." + head }

// seedVouchedHeadOnGated seeds the #4293 chain shape: the implement PR opened
// at P (runner-gated), then an operator vouch for the live head mcvHead.
func seedVouchedHeadOnGated(f *mcvFixture) {
	f.appendRow(nil, "pull_request_opened", map[string]any{"head_sha": mcvGatedHead})
	f.appendRow(nil, CategoryOperatorCommitVouched, map[string]any{"vouched_sha": mcvHead})
}

// vouchedBaseMergeCompares is an operator merge of the base: P...head carries
// base commits b1/b2 that main...head (the branch-only commits) does not.
func vouchedBaseMergeCompares() map[string][]string {
	return map[string][]string{
		compareKey(mcvGatedHead, mcvHead): {mcvBaseCommit1, mcvBaseCommit2, mcvHead},
		compareKey(mcvBaseRef, mcvHead):   {mcvGatedHead, mcvHead},
	}
}

// vouchedEditCompares is D8's canonical case: a non-merge operator edit on top
// of P — everything P...head carries is branch-only.
func vouchedEditCompares() map[string][]string {
	return map[string][]string{
		compareKey(mcvGatedHead, mcvHead): {mcvHead},
		compareKey(mcvBaseRef, mcvHead):   {mcvGatedHead, mcvHead},
	}
}

// capShas returns n distinct synthetic SHAs.
func capShas(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("cap%037d", i)
	}
	return out
}

func (g *mcGateFixture) mutateRun(mut func(r *run.Run)) {
	g.repo.mu.Lock()
	defer g.repo.mu.Unlock()
	mut(g.repo.runs[g.runID])
}

func TestClassifyVouchedHead(t *testing.T) {
	cases := []struct {
		name      string
		setup     func(g *mcGateFixture)
		wantState string
		wantCause string
		wantErr   bool
		check     func(t *testing.T, g *mcGateFixture)
	}{
		{
			name: "vouched base merge on a runner-gated head",
			setup: func(g *mcGateFixture) {
				seedVouchedHeadOnGated(g.mcvFixture)
				g.gh.compares = vouchedBaseMergeCompares()
			},
			wantState: mergeCandidateStateUnverified, wantCause: mergeCandidateCauseBaseAdvance,
			check: func(t *testing.T, g *mcGateFixture) {
				// Direction: the reference compare is <verified>...<head> and the
				// branch-only compare is <base ref>...<head>, never head...base.
				want := []string{compareKey(mcvGatedHead, mcvHead), compareKey(mcvBaseRef, mcvHead)}
				if !slices.Equal(g.gh.compareKeys, want) {
					t.Errorf("compare keys = %v, want %v", g.gh.compareKeys, want)
				}
			},
		},
		{
			name: "D8 non-merge operator edit",
			setup: func(g *mcGateFixture) {
				seedVouchedHeadOnGated(g.mcvFixture)
				g.gh.compares = vouchedEditCompares()
			},
			wantState: mergeCandidateStateNotRequired,
		},
		{
			// The reference is the newest VERIFIED head (P), not head^1 (M): M
			// was never verified, so the base commits it brought in still count.
			// A head^1 reference would read M...E = [E] → not_required.
			name: "operator merge then a vouched edit on top",
			setup: func(g *mcGateFixture) {
				g.appendRow(nil, "pull_request_opened", map[string]any{"head_sha": mcvGatedHead})
				g.appendRow(nil, CategoryOperatorCommitVouched, map[string]any{"vouched_sha": mcvOperatorMerge})
				g.appendRow(nil, CategoryOperatorCommitVouched, map[string]any{"vouched_sha": mcvHead})
				g.gh.compares = map[string][]string{
					compareKey(mcvGatedHead, mcvHead):     {mcvBaseCommit1, mcvOperatorMerge, mcvHead},
					compareKey(mcvBaseRef, mcvHead):       {mcvGatedHead, mcvOperatorMerge, mcvHead},
					compareKey(mcvOperatorMerge, mcvHead): {mcvHead},
				}
			},
			wantState: mergeCandidateStateUnverified, wantCause: mergeCandidateCauseBaseAdvance,
		},
		{
			name: "a newer passed verdict head is the reference",
			setup: func(g *mcGateFixture) {
				g.appendRow(nil, "pull_request_opened", map[string]any{"head_sha": mcvGatedHead})
				g.seedVerdict(mcvNewerVerified, mergeCandidateResultPassed)
				g.appendRow(nil, CategoryOperatorCommitVouched, map[string]any{"vouched_sha": mcvHead})
				g.gh.compares = map[string][]string{
					compareKey(mcvGatedHead, mcvHead):     {mcvBaseCommit1, mcvNewerVerified, mcvHead},
					compareKey(mcvNewerVerified, mcvHead): {mcvHead},
					compareKey(mcvBaseRef, mcvHead):       {mcvGatedHead, mcvNewerVerified, mcvHead},
				}
			},
			wantState: mergeCandidateStateNotRequired,
		},
		{
			// Only a PASSED verdict verifies a tree: a newer failed verdict head
			// is not a reference, so the reference stays P.
			name: "a failed verdict head is not a reference",
			setup: func(g *mcGateFixture) {
				g.appendRow(nil, "pull_request_opened", map[string]any{"head_sha": mcvGatedHead})
				g.seedVerdict(mcvNewerVerified, mergeCandidateResultFailed)
				g.appendRow(nil, CategoryOperatorCommitVouched, map[string]any{"vouched_sha": mcvHead})
				g.gh.compares = map[string][]string{
					compareKey(mcvGatedHead, mcvHead):     {mcvBaseCommit1, mcvNewerVerified, mcvHead},
					compareKey(mcvNewerVerified, mcvHead): {mcvHead},
					compareKey(mcvBaseRef, mcvHead):       {mcvGatedHead, mcvNewerVerified, mcvHead},
				}
			},
			wantState: mergeCandidateStateUnverified, wantCause: mergeCandidateCauseBaseAdvance,
		},
		{
			// Exclude-self: H's own passed verdict must not become its reference
			// (H...H is empty, which would read not_required).
			name: "a passed verdict for the head itself reads passed",
			setup: func(g *mcGateFixture) {
				seedVouchedHeadOnGated(g.mcvFixture)
				g.seedVerdict(mcvHead, mergeCandidateResultPassed)
				g.gh.compares = vouchedBaseMergeCompares()
			},
			wantState: mergeCandidateStatePassed, wantCause: mergeCandidateCauseBaseAdvance,
		},
		{
			name: "no verified reference head fails closed",
			setup: func(g *mcGateFixture) {
				g.appendRow(nil, CategoryOperatorCommitVouched, map[string]any{"vouched_sha": mcvHead})
				g.gh.compares = vouchedEditCompares()
			},
			wantState: mergeCandidateStateUnverified, wantCause: mergeCandidateCauseBaseAdvance,
		},
		{
			// Both compares return the SAME 250 SHAs, so the set difference is
			// empty: only the cap guard yields base_advance.
			name: "a compare at the 250-commit cap fails closed",
			setup: func(g *mcGateFixture) {
				seedVouchedHeadOnGated(g.mcvFixture)
				same := capShas(vouchProbeCompareCap)
				g.gh.compares = map[string][]string{
					compareKey(mcvGatedHead, mcvHead): same,
					compareKey(mcvBaseRef, mcvHead):   same,
				}
			},
			wantState: mergeCandidateStateUnverified, wantCause: mergeCandidateCauseBaseAdvance,
		},
		{
			name: "an empty PR base ref fails closed",
			setup: func(g *mcGateFixture) {
				seedVouchedHeadOnGated(g.mcvFixture)
				g.gh.compares = vouchedEditCompares()
				g.gh.baseRef = ""
			},
			wantState: mergeCandidateStateUnverified, wantCause: mergeCandidateCauseBaseAdvance,
		},
		{
			name: "enforce mode: a transferred head is the reference",
			setup: func(g *mcGateFixture) {
				g.s.cfg.MergeCandidateTransfer.Mode = MergeCandidateTransferEnforce
				seedVouchedOnTransfer(g)
			},
			wantState: mergeCandidateStateNotRequired,
		},
		{
			// The same transfer row in shadow mode is ignored (the rollback
			// contract), so the reference falls back to P.
			name: "shadow mode: a transferred head is ignored",
			setup: func(g *mcGateFixture) {
				g.s.cfg.MergeCandidateTransfer.Mode = MergeCandidateTransferShadow
				seedVouchedOnTransfer(g)
			},
			wantState: mergeCandidateStateUnverified, wantCause: mergeCandidateCauseBaseAdvance,
		},
		{
			// A transfer row with no from_head is malformed (the verdict read
			// ignores it too), so it is not a reference even in enforce mode.
			name: "enforce mode: a transfer with no from_head is ignored",
			setup: func(g *mcGateFixture) {
				g.s.cfg.MergeCandidateTransfer.Mode = MergeCandidateTransferEnforce
				seedVouchedOnTransfer(g)
				g.au.mu.Lock()
				for i := range g.au.appended {
					if g.au.appended[i].Category == CategoryMergeCandidateVerdictTransferred {
						g.au.appended[i].Payload = []byte(`{"from_head":"","to_head":"` + mcvNewerVerified + `"}`)
					}
				}
				g.au.mu.Unlock()
			},
			wantState: mergeCandidateStateUnverified, wantCause: mergeCandidateCauseBaseAdvance,
		},
		{
			name: "PR read error is returned",
			setup: func(g *mcGateFixture) {
				seedVouchedHeadOnGated(g.mcvFixture)
				g.gh.compares = vouchedEditCompares()
				g.gh.prStatus = http.StatusInternalServerError
			},
			wantErr: true,
		},
		{
			name: "reference compare error is returned",
			setup: func(g *mcGateFixture) {
				seedVouchedHeadOnGated(g.mcvFixture)
				g.gh.compares = vouchedEditCompares()
				g.gh.compareStatusByKey = map[string]int{compareKey(mcvGatedHead, mcvHead): http.StatusInternalServerError}
			},
			wantErr: true,
		},
		{
			name: "branch-only compare error is returned",
			setup: func(g *mcGateFixture) {
				seedVouchedHeadOnGated(g.mcvFixture)
				g.gh.compares = vouchedEditCompares()
				g.gh.compareStatusByKey = map[string]int{compareKey(mcvBaseRef, mcvHead): http.StatusInternalServerError}
			},
			wantErr: true,
		},
		{
			// The D8-edit fixture reads not_required with no later verdict read,
			// so the reference-head read is the only merge_candidate_verified
			// read on the path.
			name: "merge_candidate_verified read error is returned",
			setup: func(g *mcGateFixture) {
				seedVouchedHeadOnGated(g.mcvFixture)
				g.gh.compares = vouchedEditCompares()
				g.au.listByCategoryErrCategory = CategoryMergeCandidateVerified
			},
			wantErr: true,
		},
		{
			name: "enforce-mode transfer read error is returned",
			setup: func(g *mcGateFixture) {
				g.s.cfg.MergeCandidateTransfer.Mode = MergeCandidateTransferEnforce
				seedVouchedHeadOnGated(g.mcvFixture)
				g.gh.compares = vouchedEditCompares()
				g.au.listByCategoryErrCategory = CategoryMergeCandidateVerdictTransferred
			},
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := newMCGateFixture(t, mcvUndelegatedSpecYAML)
			tc.setup(g)
			got, err := g.s.mergeCandidateVerifyState(context.Background(), g.runRow(t), mcvHead)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("state = %+v, err = nil; want the read error returned (fail closed)", got)
				}
				if got.State != "" {
					t.Errorf("state = %q alongside an error, want none", got.State)
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if got.State != tc.wantState || got.Cause != tc.wantCause {
				t.Errorf("state = %+v, want state %q cause %q", got, tc.wantState, tc.wantCause)
			}
			if tc.check != nil {
				tc.check(t, g)
			}
		})
	}

	// D7: a deployment that cannot probe keeps the pre-amendment exemption.
	// The stub would answer base_advance if the probe ran, so not_required
	// plus zero compare calls proves the probe never ran.
	cannotProbe := map[string]func(g *mcGateFixture){
		"no GitHub client":    func(g *mcGateFixture) { g.s.cfg.GitHub = nil },
		"no installation":     func(g *mcGateFixture) { g.mutateRun(func(r *run.Run) { r.InstallationID = nil }) },
		"unparseable repo":    func(g *mcGateFixture) { g.mutateRun(func(r *run.Run) { r.Repo = "no-slash" }) },
		"no pull request url": func(g *mcGateFixture) { g.mutateRun(func(r *run.Run) { r.PullRequestURL = nil }) },
	}
	for name, mut := range cannotProbe {
		t.Run("cannot probe: "+name, func(t *testing.T) {
			g := newMCGateFixture(t, mcvUndelegatedSpecYAML)
			seedVouchedHeadOnGated(g.mcvFixture)
			g.gh.compares = vouchedBaseMergeCompares()
			mut(g)
			got, err := g.s.mergeCandidateVerifyState(context.Background(), g.runRow(t), mcvHead)
			if err != nil {
				t.Fatalf("err = %v, want the D7 fallback", err)
			}
			if got.State != mergeCandidateStateNotRequired || got.Cause != "" {
				t.Errorf("state = %+v, want not_required (D7 fallback)", got)
			}
			if g.gh.compareCalls != 0 {
				t.Errorf("compare calls = %d, want 0 (the probe must not run)", g.gh.compareCalls)
			}
		})
	}
}

// seedVouchedOnTransfer seeds P, a transfer of P's verdict to V2, and a vouch
// for the live head; V2...head is clean while P...head carries base commits.
func seedVouchedOnTransfer(g *mcGateFixture) {
	g.appendRow(nil, "pull_request_opened", map[string]any{"head_sha": mcvGatedHead})
	g.appendRow(nil, CategoryMergeCandidateVerdictTransferred, mergeCandidateTransferPayload{
		FromHead: mcvGatedHead, ToHead: mcvNewerVerified, Cause: mergeCandidateCauseBaseAdvance,
	})
	g.appendRow(nil, CategoryOperatorCommitVouched, map[string]any{"vouched_sha": mcvHead})
	g.gh.compares = map[string][]string{
		compareKey(mcvGatedHead, mcvHead):     {mcvBaseCommit1, mcvNewerVerified, mcvHead},
		compareKey(mcvNewerVerified, mcvHead): {mcvHead},
		compareKey(mcvBaseRef, mcvHead):       {mcvGatedHead, mcvNewerVerified, mcvHead},
	}
}

// TestBringsInBaseCommits pins the set difference's direction: a commit the
// head has and the verified head lacks counts only when it is NOT branch-only.
func TestBringsInBaseCommits(t *testing.T) {
	if !bringsInBaseCommits([]string{"b1", "h"}, []string{"h"}) {
		t.Error("b1 is in sinceVerified and not branch-only: want true")
	}
	if bringsInBaseCommits([]string{"h"}, []string{"p", "h"}) {
		t.Error("every sinceVerified commit is branch-only: want false")
	}
	if bringsInBaseCommits(nil, []string{"p"}) {
		t.Error("an empty sinceVerified brings in nothing: want false")
	}
}

// TestNewestVerifiedHead_ReadErrorsAreReturned calls the reference-head read
// directly: through mergeCandidateVerifyState a swallowed read error would be
// masked (no reference fails closed to base_advance, and the verdict read that
// follows hits the same injected error), so only the direct call isolates
// each category's error return.
func TestNewestVerifiedHead_ReadErrorsAreReturned(t *testing.T) {
	categories := append(append([]string(nil), mergeCandidateRunnerGatedCategories...),
		CategoryMergeCandidateVerified, CategoryMergeCandidateVerdictTransferred)
	for _, cat := range categories {
		t.Run(cat, func(t *testing.T) {
			g := newMCGateFixture(t, mcvUndelegatedSpecYAML)
			g.s.cfg.MergeCandidateTransfer.Mode = MergeCandidateTransferEnforce
			seedVouchedOnTransfer(g)
			g.au.listByCategoryErrCategory = cat
			ref, err := g.s.newestVerifiedHead(context.Background(), g.runID, mcvHead)
			if err == nil {
				t.Fatalf("ref = %q, err = nil; want the %s read error returned", ref, cat)
			}
		})
	}
	t.Run("no error reads the newest verified head", func(t *testing.T) {
		g := newMCGateFixture(t, mcvUndelegatedSpecYAML)
		g.s.cfg.MergeCandidateTransfer.Mode = MergeCandidateTransferEnforce
		seedVouchedOnTransfer(g)
		ref, err := g.s.newestVerifiedHead(context.Background(), g.runID, mcvHead)
		if err != nil || ref != mcvNewerVerified {
			t.Fatalf("ref = %q err = %v, want %s", ref, err, mcvNewerVerified)
		}
	})
}
