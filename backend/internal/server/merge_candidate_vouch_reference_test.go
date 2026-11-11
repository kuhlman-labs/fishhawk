package server

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// --- ADR-142 D2 seam: the vouch probe with a caller-supplied reference ------
//
// vouchedHeadProbe is ADR-090 D8 Amendment 1's probe and determinability
// ladder with the verified reference read supplied by the caller, and
// newestVerifiedHeadExcept is the reference read with an exclusion predicate.
// Rule 4 (classifyVouchedHead) supplies newestVerifiedHead, which excludes
// only the probed head. These tests decide the seam directly, with a
// test-local predicate naming SHAs, so no scenario-arm symbol is needed.
//
// Every compare list follows the (merge-base, head] contract: the probed head
// H is mcvHead, an older gated head E brings in base commit B since E, and a
// newer candidate X is an operator edit away from H.

const (
	// vrefOlderGated is E: an older runner-gated head; E...H carries B.
	vrefOlderGated = "a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0"
	// vrefNewerCandidate is X: the newest candidate reference; X...H is an edit.
	vrefNewerCandidate = "b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0"
	// vrefBaseCommit is B: a commit the base contains and E lacks.
	vrefBaseCommit = "c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0"
	// vrefScenarioC1 is C1: a scenario commit with a passed verdict.
	vrefScenarioC1 = "d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0"
)

// vrefCompares serves E...H (base-bringing), X...H (an edit) and main...H
// (the branch-only commits), so the reference choice decides the cause:
// reference E → base_advance, reference X → not_required.
func vrefCompares() map[string][]string {
	return map[string][]string{
		compareKey(vrefOlderGated, mcvHead):     {vrefBaseCommit, vrefNewerCandidate, mcvHead},
		compareKey(vrefNewerCandidate, mcvHead): {mcvHead},
		compareKey(mcvBaseRef, mcvHead):         {vrefOlderGated, vrefNewerCandidate, mcvHead},
	}
}

// vrefReference returns a reference closure over newestVerifiedHeadExcept
// that excludes exactly the named SHAs, and a pointer counting its calls.
func vrefReference(g *mcGateFixture, excluded ...string) (func(context.Context) (string, error), *int) {
	calls := 0
	return func(ctx context.Context) (string, error) {
		calls++
		return g.s.newestVerifiedHeadExcept(ctx, g.runID, func(sha string) bool {
			return slices.Contains(excluded, sha)
		})
	}, &calls
}

func vrefProbe(t *testing.T, g *mcGateFixture, excluded ...string) (string, int, error) {
	t.Helper()
	ref, calls := vrefReference(g, excluded...)
	cause, err := g.s.vouchedHeadProbe(context.Background(), g.runRow(t), mcvHead, ref)
	return cause, *calls, err
}

func assertCompareKeys(t *testing.T, g *mcGateFixture, want ...string) {
	t.Helper()
	if !slices.Equal(g.gh.compareKeys, want) {
		t.Errorf("compare keys = %v, want %v", g.gh.compareKeys, want)
	}
}

func TestVouchedHeadProbe_ReferenceExclusion(t *testing.T) {
	refE := compareKey(vrefOlderGated, mcvHead)
	refX := compareKey(vrefNewerCandidate, mcvHead)
	branchOnly := compareKey(mcvBaseRef, mcvHead)

	// Exclusion rows: E is older (sequence 1), X is newer (sequence 2). With X
	// excluded the reference must fall back to E (base_advance through E...H);
	// without the exclusion check the reference is X (not_required through
	// X...H), so the compare keys and the cause both discriminate.
	exclusion := []struct {
		name string
		seed func(g *mcGateFixture)
	}{
		{"an excluded newest gated head", func(g *mcGateFixture) {
			g.appendRow(nil, "pull_request_opened", map[string]any{"head_sha": vrefOlderGated})
			g.appendRow(nil, "fixup_pushed", map[string]any{"head_sha": vrefNewerCandidate})
		}},
		{"an excluded newest passed verdict head", func(g *mcGateFixture) {
			g.appendRow(nil, "pull_request_opened", map[string]any{"head_sha": vrefOlderGated})
			g.seedVerdict(vrefNewerCandidate, mergeCandidateResultPassed)
		}},
		{"an excluded newest enforce-mode transfer head", func(g *mcGateFixture) {
			g.s.cfg.MergeCandidateTransfer.Mode = MergeCandidateTransferEnforce
			g.appendRow(nil, "pull_request_opened", map[string]any{"head_sha": vrefOlderGated})
			g.appendRow(nil, CategoryMergeCandidateVerdictTransferred, mergeCandidateTransferPayload{
				FromHead: vrefOlderGated, ToHead: vrefNewerCandidate, Cause: mergeCandidateCauseBaseAdvance,
			})
		}},
	}
	for _, tc := range exclusion {
		t.Run(tc.name+" yields the next eligible head", func(t *testing.T) {
			g := newMCGateFixture(t, mcvUndelegatedSpecYAML)
			tc.seed(g)
			g.gh.compares = vrefCompares()
			cause, _, err := vrefProbe(t, g, vrefNewerCandidate)
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if cause != mergeCandidateCauseBaseAdvance {
				t.Errorf("cause = %q, want base_advance through the older eligible head", cause)
			}
			assertCompareKeys(t, g, refE, branchOnly)
		})
		t.Run(tc.name+" is the reference when not excluded", func(t *testing.T) {
			g := newMCGateFixture(t, mcvUndelegatedSpecYAML)
			tc.seed(g)
			g.gh.compares = vrefCompares()
			cause, _, err := vrefProbe(t, g)
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if cause != "" {
				t.Errorf("cause = %q, want empty (X...H is an operator edit)", cause)
			}
			assertCompareKeys(t, g, refX, branchOnly)
		})
	}

	// A transfer outside enforce mode is never a reference, whatever the
	// predicate says: with nothing excluded the reference stays E.
	for _, mode := range []MergeCandidateTransferMode{MergeCandidateTransferShadow, MergeCandidateTransferOff} {
		t.Run(fmt.Sprintf("a %s-mode transfer is ignored", mode), func(t *testing.T) {
			g := newMCGateFixture(t, mcvUndelegatedSpecYAML)
			g.s.cfg.MergeCandidateTransfer.Mode = mode
			g.appendRow(nil, "pull_request_opened", map[string]any{"head_sha": vrefOlderGated})
			g.appendRow(nil, CategoryMergeCandidateVerdictTransferred, mergeCandidateTransferPayload{
				FromHead: vrefOlderGated, ToHead: vrefNewerCandidate, Cause: mergeCandidateCauseBaseAdvance,
			})
			g.gh.compares = vrefCompares()
			cause, _, err := vrefProbe(t, g)
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if cause != mergeCandidateCauseBaseAdvance {
				t.Errorf("cause = %q, want base_advance (the transfer is not a reference)", cause)
			}
			assertCompareKeys(t, g, refE, branchOnly)
		})
	}

	t.Run("every candidate excluded fails closed with zero compares", func(t *testing.T) {
		g := newMCGateFixture(t, mcvUndelegatedSpecYAML)
		g.s.cfg.MergeCandidateTransfer.Mode = MergeCandidateTransferEnforce
		g.appendRow(nil, "pull_request_opened", map[string]any{"head_sha": vrefOlderGated})
		g.seedVerdict(vrefNewerCandidate, mergeCandidateResultPassed)
		g.appendRow(nil, CategoryMergeCandidateVerdictTransferred, mergeCandidateTransferPayload{
			FromHead: vrefOlderGated, ToHead: vrefScenarioC1, Cause: mergeCandidateCauseBaseAdvance,
		})
		g.gh.compares = vrefCompares()
		cause, _, err := vrefProbe(t, g, vrefOlderGated, vrefNewerCandidate, vrefScenarioC1)
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if cause != mergeCandidateCauseBaseAdvance {
			t.Errorf("cause = %q, want base_advance (no reference)", cause)
		}
		if g.gh.compareCalls != 0 {
			t.Errorf("compare calls = %d, want 0", g.gh.compareCalls)
		}
	})

	// The newest eligible head is chosen by chain sequence, never by the
	// category read order (runner-gated rows are read before verdict rows).
	t.Run("a newer gated head beats an older verdict head", func(t *testing.T) {
		g := newMCGateFixture(t, mcvUndelegatedSpecYAML)
		g.seedVerdict(vrefOlderGated, mergeCandidateResultPassed)
		g.appendRow(nil, "fixup_pushed", map[string]any{"head_sha": vrefNewerCandidate})
		g.gh.compares = vrefCompares()
		if cause, _, err := vrefProbe(t, g); err != nil || cause != "" {
			t.Fatalf("cause = %q err = %v, want empty through the newer gated head", cause, err)
		}
		assertCompareKeys(t, g, refX, branchOnly)
	})
	t.Run("a newer verdict head beats an older gated head", func(t *testing.T) {
		g := newMCGateFixture(t, mcvUndelegatedSpecYAML)
		g.appendRow(nil, "fixup_pushed", map[string]any{"head_sha": vrefOlderGated})
		g.seedVerdict(vrefNewerCandidate, mergeCandidateResultPassed)
		g.gh.compares = vrefCompares()
		if cause, _, err := vrefProbe(t, g); err != nil || cause != "" {
			t.Fatalf("cause = %q err = %v, want empty through the newer verdict head", cause, err)
		}
		assertCompareKeys(t, g, refX, branchOnly)
	})

	// Cannot-probe arms (ADR-090 D7): empty cause, the existing WARN, no
	// reference read and zero compares. The stub would answer base_advance if
	// the probe ran (reference E).
	cannotProbe := []struct {
		name, why string
		mut       func(g *mcGateFixture)
	}{
		{"no GitHub client", "no GitHub client is wired", func(g *mcGateFixture) { g.s.cfg.GitHub = nil }},
		{"no installation", "the run carries no GitHub installation", func(g *mcGateFixture) {
			g.mutateRun(func(r *run.Run) { r.InstallationID = nil })
		}},
		{"unparseable repo", "the run repo is unparseable", func(g *mcGateFixture) {
			g.mutateRun(func(r *run.Run) { r.Repo = "no-slash" })
		}},
		{"no pull request url", "the run carries no parseable pull request number", func(g *mcGateFixture) {
			g.mutateRun(func(r *run.Run) { r.PullRequestURL = nil })
		}},
	}
	for _, tc := range cannotProbe {
		t.Run("cannot probe: "+tc.name, func(t *testing.T) {
			g := newMCGateFixture(t, mcvUndelegatedSpecYAML)
			g.appendRow(nil, "pull_request_opened", map[string]any{"head_sha": vrefOlderGated})
			g.gh.compares = vrefCompares()
			tc.mut(g)
			if got := g.s.mergeCandidateCannotProbeReason(g.runRow(t)); got != tc.why {
				t.Errorf("cannot-probe reason = %q, want %q", got, tc.why)
			}
			buf := captureLogger(g.s)
			cause, refCalls, err := vrefProbe(t, g)
			if err != nil || cause != "" {
				t.Fatalf("cause = %q err = %v, want the D7 empty cause", cause, err)
			}
			if refCalls != 0 {
				t.Errorf("reference reads = %d, want 0 (cannot-probe is checked first)", refCalls)
			}
			if g.gh.compareCalls != 0 {
				t.Errorf("compare calls = %d, want 0 (the probe must not run)", g.gh.compareCalls)
			}
			var warns []string
			for _, line := range strings.Split(buf.String(), "\n") {
				if strings.Contains(line, "level=WARN") && strings.Contains(line, "cannot probe an operator-vouched head for base commits") {
					warns = append(warns, line)
				}
			}
			if len(warns) != 1 {
				t.Fatalf("cannot-probe WARN lines = %d, want 1:\n%s", len(warns), buf.String())
			}
			for _, want := range []string{"run_id=" + g.runID.String(), "head_sha=" + mcvHead, fmt.Sprintf("why=%q", tc.why)} {
				if !strings.Contains(warns[0], want) {
					t.Errorf("WARN line missing %s:\n%s", want, warns[0])
				}
			}
		})
	}
	t.Run("a probeable run has no cannot-probe reason", func(t *testing.T) {
		g := newMCGateFixture(t, mcvUndelegatedSpecYAML)
		if got := g.s.mergeCandidateCannotProbeReason(g.runRow(t)); got != "" {
			t.Errorf("cannot-probe reason = %q, want empty", got)
		}
	})

	t.Run("a compare at the 250-commit cap fails closed", func(t *testing.T) {
		g := newMCGateFixture(t, mcvUndelegatedSpecYAML)
		g.appendRow(nil, "pull_request_opened", map[string]any{"head_sha": vrefOlderGated})
		same := capShas(vouchProbeCompareCap)
		g.gh.compares = map[string][]string{refE: same, branchOnly: same}
		if cause, _, err := vrefProbe(t, g); err != nil || cause != mergeCandidateCauseBaseAdvance {
			t.Fatalf("cause = %q err = %v, want base_advance (only the cap guard decides)", cause, err)
		}
	})
	t.Run("an empty PR base ref fails closed", func(t *testing.T) {
		g := newMCGateFixture(t, mcvUndelegatedSpecYAML)
		g.appendRow(nil, "fixup_pushed", map[string]any{"head_sha": vrefNewerCandidate})
		g.gh.compares = vrefCompares()
		g.gh.baseRef = ""
		if cause, _, err := vrefProbe(t, g); err != nil || cause != mergeCandidateCauseBaseAdvance {
			t.Fatalf("cause = %q err = %v, want base_advance (X...H alone would read empty)", cause, err)
		}
		if g.gh.compareCalls != 0 {
			t.Errorf("compare calls = %d, want 0", g.gh.compareCalls)
		}
	})

	// Read errors through the seam: each category the reference reads fails
	// alone (listByCategoryErrCategory, with the global listByCategoryErr
	// nil), then the PR read and each compare.
	categories := append(append([]string(nil), mergeCandidateRunnerGatedCategories...),
		CategoryMergeCandidateVerified, CategoryMergeCandidateVerdictTransferred)
	for _, cat := range categories {
		t.Run("read error on "+cat+" is returned", func(t *testing.T) {
			g := newMCGateFixture(t, mcvUndelegatedSpecYAML)
			g.s.cfg.MergeCandidateTransfer.Mode = MergeCandidateTransferEnforce
			g.appendRow(nil, "pull_request_opened", map[string]any{"head_sha": vrefOlderGated})
			g.seedVerdict(vrefNewerCandidate, mergeCandidateResultPassed)
			g.gh.compares = vrefCompares()
			g.au.listByCategoryErr = nil
			g.au.listByCategoryErrCategory = cat
			cause, _, err := vrefProbe(t, g)
			if err == nil {
				t.Fatalf("cause = %q, err = nil; want the %s read error returned", cause, cat)
			}
			if cause != "" {
				t.Errorf("cause = %q alongside an error, want empty", cause)
			}
			if g.gh.compareCalls != 0 {
				t.Errorf("compare calls = %d, want 0 (the reference read fails first)", g.gh.compareCalls)
			}
		})
	}
	forgeErrors := []struct {
		name string
		mut  func(g *mcGateFixture)
	}{
		{"PR read", func(g *mcGateFixture) { g.gh.prStatus = http.StatusInternalServerError }},
		{"reference compare", func(g *mcGateFixture) {
			g.gh.compareStatusByKey = map[string]int{refX: http.StatusInternalServerError}
		}},
		{"branch-only compare", func(g *mcGateFixture) {
			g.gh.compareStatusByKey = map[string]int{branchOnly: http.StatusInternalServerError}
		}},
	}
	for _, tc := range forgeErrors {
		t.Run(tc.name+" error is returned", func(t *testing.T) {
			g := newMCGateFixture(t, mcvUndelegatedSpecYAML)
			g.appendRow(nil, "fixup_pushed", map[string]any{"head_sha": vrefNewerCandidate})
			// X...H carries a base commit so the branch-only compare is reached.
			g.gh.compares = map[string][]string{
				refX:       {vrefBaseCommit, mcvHead},
				branchOnly: {vrefNewerCandidate, mcvHead},
			}
			tc.mut(g)
			cause, _, err := vrefProbe(t, g)
			if err == nil {
				t.Fatalf("cause = %q, err = nil; want the %s error returned", cause, tc.name)
			}
			if cause != "" {
				t.Errorf("cause = %q alongside an error, want empty", cause)
			}
		})
	}
}

// TestClassifyVouchedHead_DirectKeepsScenarioEvidence pins that rule 4's
// direct live-head vouch keeps its existing reference (ADR-142 D2): a passed
// scenario commit stays eligible, because newestVerifiedHead excludes only the
// probed head. It reads the REAL mergeCandidateVerifyState with
// acceptance_scenarios_pushed rows on the chain.
func TestClassifyVouchedHead_DirectKeepsScenarioEvidence(t *testing.T) {
	// The live head mcvHead plays C2 (or V in the last row). F is the gated
	// head, B a base commit F lacks, C1 a scenario commit.
	const f = vrefOlderGated
	seedC2 := func(g *mcGateFixture, passC1 bool) {
		g.appendRow(nil, "pull_request_opened", map[string]any{"head_sha": f})
		g.appendRow(nil, CategoryAcceptanceScenariosPushed, map[string]any{
			"outcome": "acceptance_scenarios_pushed", "head_sha": vrefScenarioC1, "base_sha": f,
		})
		if passC1 {
			g.seedVerdict(vrefScenarioC1, mergeCandidateResultPassed)
		}
		g.appendRow(nil, CategoryAcceptanceScenariosPushed, map[string]any{
			"outcome": "acceptance_scenarios_pushed", "head_sha": mcvHead, "base_sha": vrefScenarioC1,
		})
		g.appendRow(nil, CategoryOperatorCommitVouched, map[string]any{"vouched_sha": mcvHead})
		g.gh.compares = map[string][]string{
			compareKey(vrefScenarioC1, mcvHead): {mcvHead},
			compareKey(f, mcvHead):              {vrefBaseCommit, vrefScenarioC1, mcvHead},
			compareKey(mcvBaseRef, mcvHead):     {f, vrefScenarioC1, mcvHead},
		}
	}

	cases := []struct {
		name      string
		setup     func(g *mcGateFixture)
		wantState string
		wantCause string
		wantKeys  []string
	}{
		{
			name:      "vouch of exact C2 with a passed C1 keeps C1 as the reference",
			setup:     func(g *mcGateFixture) { seedC2(g, true) },
			wantState: mergeCandidateStateNotRequired,
			wantKeys:  []string{compareKey(vrefScenarioC1, mcvHead), compareKey(mcvBaseRef, mcvHead)},
		},
		{
			name:      "vouch of exact C2 with no pass at C1 reads base_advance through F",
			setup:     func(g *mcGateFixture) { seedC2(g, false) },
			wantState: mergeCandidateStateUnverified, wantCause: mergeCandidateCauseBaseAdvance,
			wantKeys: []string{compareKey(f, mcvHead), compareKey(mcvBaseRef, mcvHead)},
		},
		{
			// Today's answer, reachable after a reset back to V; pinned as
			// unchanged by ADR-142, not as a claim that it is ideal.
			name: "live V vouched with a passed scenario descendant C1",
			setup: func(g *mcGateFixture) {
				g.appendRow(nil, "pull_request_opened", map[string]any{"head_sha": f})
				g.appendRow(nil, CategoryOperatorCommitVouched, map[string]any{"vouched_sha": mcvHead})
				g.appendRow(nil, CategoryAcceptanceScenariosPushed, map[string]any{
					"outcome": "acceptance_scenarios_pushed", "head_sha": vrefScenarioC1, "base_sha": mcvHead,
				})
				g.seedVerdict(vrefScenarioC1, mergeCandidateResultPassed)
				g.gh.compares = map[string][]string{
					// V is an ancestor of C1, so C1...V is empty.
					compareKey(vrefScenarioC1, mcvHead): {},
					compareKey(f, mcvHead):              {vrefBaseCommit, mcvHead},
					compareKey(mcvBaseRef, mcvHead):     {f, mcvHead},
				}
			},
			wantState: mergeCandidateStateNotRequired,
			wantKeys:  []string{compareKey(vrefScenarioC1, mcvHead)},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := newMCGateFixture(t, mcvUndelegatedSpecYAML)
			tc.setup(g)
			got, err := g.s.mergeCandidateVerifyState(context.Background(), g.runRow(t), mcvHead)
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if got.State != tc.wantState || got.Cause != tc.wantCause {
				t.Errorf("state = %+v, want state %q cause %q", got, tc.wantState, tc.wantCause)
			}
			assertCompareKeys(t, g, tc.wantKeys...)
		})
	}
}
