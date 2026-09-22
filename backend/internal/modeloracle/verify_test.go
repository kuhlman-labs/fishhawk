package modeloracle_test

import (
	"context"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/modeloracle"
)

// TestVerify_VerifiedOnFreshPresent: a model present in a fresh+ok snapshot is
// ModelVerified with no suggestion or available set.
func TestVerify_VerifiedOnFreshPresent(t *testing.T) {
	o := modeloracle.Static{Models: map[string][]string{"anthropic": {"claude-opus-4-8"}}, Fresh: true}
	v := modeloracle.Verify(context.Background(), o, "anthropic", "claude-opus-4-8")
	if v.Status != modeloracle.ModelVerified {
		t.Fatalf("status = %q, want verified", v.Status)
	}
	if v.Suggestion != "" || len(v.Available) != 0 {
		t.Errorf("verified verdict carries suggestion=%q available=%v, want both empty", v.Suggestion, v.Available)
	}
}

// TestVerify_RejectedOnFreshAbsence: a model absent from a fresh+ok snapshot is
// ModelRejected and carries the sorted available set.
func TestVerify_RejectedOnFreshAbsence(t *testing.T) {
	o := modeloracle.Static{Models: map[string][]string{"anthropic": {"claude-sonnet-4-6", "claude-opus-4-8"}}, Fresh: true}
	v := modeloracle.Verify(context.Background(), o, "anthropic", "totally-unrelated-9")
	if v.Status != modeloracle.ModelRejected {
		t.Fatalf("status = %q, want rejected", v.Status)
	}
	// Available is sorted regardless of snapshot order.
	if len(v.Available) != 2 || v.Available[0] != "claude-opus-4-8" || v.Available[1] != "claude-sonnet-4-6" {
		t.Errorf("available = %v, want sorted [claude-opus-4-8 claude-sonnet-4-6]", v.Available)
	}
}

// TestVerify_UnverifiableOnStaleOrNoSnapshot: a stale snapshot and an
// unregistered provider both fail open to ModelUnverifiable (absence from a
// non-authoritative list cannot reject).
func TestVerify_UnverifiableOnStaleOrNoSnapshot(t *testing.T) {
	stale := modeloracle.Static{Models: map[string][]string{"anthropic": {"claude-opus-4-8"}}, Fresh: false}
	if v := modeloracle.Verify(context.Background(), stale, "anthropic", "claude-typo"); v.Status != modeloracle.ModelUnverifiable {
		t.Errorf("stale snapshot: status = %q, want unverifiable", v.Status)
	}
	noProvider := modeloracle.Static{Models: map[string][]string{"codex": {"gpt-5.5"}}, Fresh: true}
	if v := modeloracle.Verify(context.Background(), noProvider, "anthropic", "claude-opus-4-8"); v.Status != modeloracle.ModelUnverifiable {
		t.Errorf("unregistered provider: status = %q, want unverifiable", v.Status)
	}
}

// TestVerify_NilOracleUnverifiable: a nil oracle is ModelUnverifiable — the
// key-less-deployment fail-open path.
func TestVerify_NilOracleUnverifiable(t *testing.T) {
	if v := modeloracle.Verify(context.Background(), nil, "anthropic", "anything"); v.Status != modeloracle.ModelUnverifiable {
		t.Errorf("nil oracle: status = %q, want unverifiable", v.Status)
	}
}

// TestVerify_DidYouMeanPresentForNearMiss: a near-miss typo yields a Suggestion;
// an unrelated string yields none.
func TestVerify_DidYouMeanPresentForNearMiss(t *testing.T) {
	o := modeloracle.Static{Models: map[string][]string{"anthropic": {"claude-opus-4-8"}}, Fresh: true}

	near := modeloracle.Verify(context.Background(), o, "anthropic", "claude-opus-4-7")
	if near.Suggestion != "claude-opus-4-8" {
		t.Errorf("near-miss suggestion = %q, want claude-opus-4-8", near.Suggestion)
	}

	unrelated := modeloracle.Verify(context.Background(), o, "anthropic", "zzzzzzzzzzz")
	if unrelated.Suggestion != "" {
		t.Errorf("unrelated suggestion = %q, want empty", unrelated.Suggestion)
	}
}

// TestVerify_RejectMessageGolden pins the reject sentence byte-for-byte against
// the exact string spec.modelRejectMessage produced before the move, including
// the did-you-mean clause, the sorted available list, and the (none) branch.
func TestVerify_RejectMessageGolden(t *testing.T) {
	o := modeloracle.Static{Models: map[string][]string{"anthropic": {"claude-sonnet-4-6", "claude-opus-4-8"}}, Fresh: true}
	v := modeloracle.Verify(context.Background(), o, "anthropic", "claude-opus-4-7")
	const want = `model "claude-opus-4-7" is not a known "anthropic" model (did you mean "claude-opus-4-8"?); available: claude-opus-4-8, claude-sonnet-4-6`
	if got := v.RejectMessage(); got != want {
		t.Errorf("RejectMessage() = %q\nwant                 %q", got, want)
	}

	// Empty available set → the (none) branch.
	empty := modeloracle.Verdict{Status: modeloracle.ModelRejected, Provider: "anthropic", Model: "x"}
	const wantNone = `model "x" is not a known "anthropic" model; available: (none)`
	if got := empty.RejectMessage(); got != wantNone {
		t.Errorf("RejectMessage() empty = %q, want %q", got, wantNone)
	}
}
