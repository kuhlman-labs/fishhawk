package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
)

// headMovedCause builds the head-mismatch refusal a pinned dispatch returns:
// forge.ErrMergeConflict wrapped alongside a *githubclient.HeadMismatchError.
func headMovedCause(basis string) error {
	return fmt.Errorf("%w: %w", forge.ErrMergeConflict,
		&githubclient.HeadMismatchError{Basis: basis, ExpectedHeadSHA: "h1", ObservedHeadSHA: "h2"})
}

// TestWriteMergeHeadMoved_ClaimArms (#4683) drives writeMergeHeadMoved's claim
// switch directly, one row per arm: released, held on an uncertain basis, held
// because the release did not land (with the operator re-arm), and unclaimed.
// The rows differ only in (claimed, settled, cause, rearm), so collapsing any
// arm gives two rows the same message. Every substring is hard-coded.
// Counterfactual: deleting the `case !claimed:` arm loses row (d)'s clause;
// dropping the "Once the operator re-arms the claim" lead-in loses rows (b)
// and (c)'s.
func TestWriteMergeHeadMoved_ClaimArms(t *testing.T) {
	g := newMCGateFixture(t, mcvUndelegatedSpecYAML)
	runRow, err := g.repo.GetRun(context.Background(), g.runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	const prURL = "https://github.com/x/y/pull/7"
	cases := []struct {
		name    string
		claimed bool
		settled mergeDispatchLedgerOutcome
		basis   string
		rearm   string
		want    []string
		notWant []string
	}{
		{
			name: "a_released", claimed: true, settled: mergeDispatchRelease, basis: githubclient.HeadMismatchPrecheck,
			want:    []string{"claim was released", " Re-invoke the merge"},
			notWant: []string{"Once the operator re-arms", "dispatch_failed"},
		},
		{
			name: "b_held_uncertain_basis", claimed: true, settled: mergeDispatchUncertain, basis: githubclient.HeadMismatchGraphQLReread,
			rearm:   mergeDispatchRearmNote,
			want:    []string{"does not prove the forge applied nothing, so it is dispatch_failed", "Once the operator re-arms the claim"},
			notWant: []string{"claim was released", "release did not land"},
		},
		{
			name: "c_held_release_failed", claimed: true, settled: mergeDispatchUncertain, basis: githubclient.HeadMismatchPrecheck,
			rearm:   mergeDispatchRearmNote,
			want:    []string{"claim release did not land, so it is dispatch_failed", "Once the operator re-arms the claim", "until the operator re-arms the claim"},
			notWant: []string{"does not prove", "claim was released"},
		},
		{
			name: "d_unclaimed", claimed: false, settled: "", basis: githubclient.HeadMismatchPrecheck,
			want:    []string{"No merge-dispatch claim was held.", " Re-invoke the merge"},
			notWant: []string{"dispatch_failed", "re-arm", "claim was released"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v0/runs/"+g.runID.String()+"/merge", nil)
			g.s.writeMergeHeadMoved(rec, req, runRow, prURL, 7, "h1", headMovedCause(tc.basis), tc.claimed, tc.settled, tc.rearm)
			env := decodeMergeErr(t, rec)
			if rec.Code != http.StatusConflict || env.Error.Code != mergeCodeHeadMoved {
				t.Fatalf("status/code = %d %s, want 409 %s", rec.Code, env.Error.Code, mergeCodeHeadMoved)
			}
			if env.Error.Details["refusal_basis"] != tc.basis || env.Error.Details["head_sha"] != "h1" {
				t.Fatalf("details = %v, want refusal_basis %s head_sha h1", env.Error.Details, tc.basis)
			}
			for _, want := range tc.want {
				if !strings.Contains(env.Error.Message, want) {
					t.Errorf("message = %q, want it to contain %q", env.Error.Message, want)
				}
			}
			for _, bad := range tc.notWant {
				if strings.Contains(env.Error.Message, bad) {
					t.Errorf("message = %q, must NOT contain %q", env.Error.Message, bad)
				}
			}
		})
	}
}

// TestAutoDriveHeadMovedNote_SelectsBySettleOutcome (#4683) pins the delegated
// note selector: it follows the settle outcome, and for a dispatch_failed
// settle it splits on whether the refusal itself was uncertain (the basis does
// not prove nothing applied) or releasable (the release did not land).
func TestAutoDriveHeadMovedNote_SelectsBySettleOutcome(t *testing.T) {
	cases := []struct {
		name    string
		settled mergeDispatchLedgerOutcome
		err     error
		want    string
	}{
		{"released", mergeDispatchRelease, headMovedCause(githubclient.HeadMismatchPrecheck), autoDriveNoteHeadMoved},
		{"uncertain basis held", mergeDispatchUncertain, headMovedCause(githubclient.HeadMismatchGraphQLReread), autoDriveNoteHeadMovedBasisHeld},
		{"unknown basis held", mergeDispatchUncertain, fmt.Errorf("%w: %w", forge.ErrMergeConflict, &githubclient.HeadMismatchError{}), autoDriveNoteHeadMovedBasisHeld},
		{"release failed precheck", mergeDispatchUncertain, headMovedCause(githubclient.HeadMismatchPrecheck), autoDriveNoteHeadMovedReleaseFailed},
		{"release failed rest_sha", mergeDispatchUncertain, headMovedCause(githubclient.HeadMismatchRESTSha), autoDriveNoteHeadMovedReleaseFailed},
		{"release failed bare 409", mergeDispatchUncertain, forge.ErrMergeConflict, autoDriveNoteHeadMovedReleaseFailed},
		{"unclaimed", "", headMovedCause(githubclient.HeadMismatchPrecheck), autoDriveNoteHeadMovedUnclaimed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := autoDriveHeadMovedNote(tc.settled, tc.err); got != tc.want {
				t.Fatalf("autoDriveHeadMovedNote(%q, %v) = %q, want %q", tc.settled, tc.err, got, tc.want)
			}
		})
	}
}

// TestAutoDriveHeadMovedNotes_DistinctAndNameReArm (#4683): the four head-moved
// notes are pairwise distinct, each carries its hard-coded distinguishing
// clause and none carries another's, and exactly the two dispatch_failed
// notes name the operator re-arm. It stops the constant-backed e2e assertions
// from passing vacuously on identical strings.
func TestAutoDriveHeadMovedNotes_DistinctAndNameReArm(t *testing.T) {
	notes := []struct {
		name   string
		note   string
		clause string
		rearm  bool
	}{
		{"released", autoDriveNoteHeadMoved, "the merge-dispatch claim was released", false},
		{"basis held", autoDriveNoteHeadMovedBasisHeld, "does not prove the forge applied nothing", true},
		{"release failed", autoDriveNoteHeadMovedReleaseFailed, "release did not land", true},
		{"unclaimed", autoDriveNoteHeadMovedUnclaimed, "no merge-dispatch claim was held", false},
	}
	for i, a := range notes {
		if !strings.Contains(a.note, a.clause) {
			t.Errorf("%s note = %q, want it to contain %q", a.name, a.note, a.clause)
		}
		if got := strings.Contains(a.note, "operator re-arm"); got != a.rearm {
			t.Errorf("%s note names the operator re-arm = %v, want %v: %q", a.name, got, a.rearm, a.note)
		}
		for j, b := range notes {
			if i == j {
				continue
			}
			if a.note == b.note {
				t.Errorf("%s and %s notes are identical: %q", a.name, b.name, a.note)
			}
			if strings.Contains(a.note, b.clause) {
				t.Errorf("%s note contains the %s clause %q: %q", a.name, b.name, b.clause, a.note)
			}
		}
	}
}
