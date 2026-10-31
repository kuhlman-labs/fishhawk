package server

import (
	"context"

	"github.com/google/uuid"
)

// Review-round context (#4077). Two facts about a review round ride the
// dispatch context down to emitReviewStarted, which stamps them on the
// *_review_started payload, so a round orphaned by a daemon restart can be
// re-dispatched against the SAME input on the next boot:
//
//   - the round SOURCE (implement rounds only): where the round's diff came
//     from, and for a compare-sourced round the base it was compared from;
//   - the RE-DISPATCH marker: the audit sequence of the orphaned round this
//     round re-dispatches, and how many re-dispatches deep it is.
//
// They travel on the context rather than as parameters because
// runImplementReviews / runPlanReviews have well over a hundred test call
// sites, and the values are written by exactly four production sites (the
// three implement dispatchers set the source; the boot re-dispatch sets both).
//
// A third fact, the operator RE-RUN marker (#4219, reviewRerun below), rides
// the same way: the re-run verb sets it, and emitReviewStarted stamps its Of as
// the new round's rerun_of.

// Implement-review round origins (planreview.ReviewStartedPayload.RoundOrigin).
const (
	// reviewRoundOriginTrace: the trace-time review hook, whose diff is the
	// stage's uploaded trace bundle.
	reviewRoundOriginTrace = "trace"
	// reviewRoundOriginFixupPush: the post-fix-up re-review, whose diff is
	// ComparePatch(base, pushed head).
	reviewRoundOriginFixupPush = "fixup_push"
	// reviewRoundOriginConsolidated: a decomposed parent's consolidated review,
	// whose diff is ComparePatch(base, consolidated head).
	reviewRoundOriginConsolidated = "consolidated"
)

// reviewRoundSource is where an implement-review round's diff came from.
// BaseSHA is set only for the compare-sourced origins (fixup_push,
// consolidated); a trace round's diff lives in the stored bundle.
//
// HeadSHA and HeadUnresolvedReason feed the round's reviewed_head record
// (ADR-117 A1.7, buildReviewedHead) and persist ONLY through it — neither is a
// payload key of its own. HeadSHA is a trace round's AUTHORITATIVE verify head
// (bundle.ExtractVerifyHead), distinct from the #797 dedup head_sha;
// HeadUnresolvedReason, when set, records the round's head as unknown with that
// reason (a consolidated compare that named no head, or a truncated commit
// listing whose last commit is not the tip).
type reviewRoundSource struct {
	Origin               string
	BaseSHA              string
	HeadSHA              string
	HeadUnresolvedReason string
}

// reviewRedispatch is the re-dispatch marker: Of is the audit sequence of the
// orphaned round's *_review_started entry, Depth the new round's re-dispatch
// depth (the orphaned round's depth + 1, so >= 1).
type reviewRedispatch struct {
	Of    int64
	Depth int
}

// The context keys are UNEXPORTED struct types, so no code outside this
// package can construct them, and no code inside it derives them from a
// request.
type (
	reviewRoundSourceCtxKey struct{}
	reviewRedispatchCtxKey  struct{}
)

// withReviewRoundSource returns ctx carrying the implement round's source.
func withReviewRoundSource(ctx context.Context, src reviewRoundSource) context.Context {
	return context.WithValue(ctx, reviewRoundSourceCtxKey{}, src)
}

// reviewRoundSourceFrom returns the round source ctx carries; ok=false when it
// carries none or an empty origin.
func reviewRoundSourceFrom(ctx context.Context) (reviewRoundSource, bool) {
	src, ok := ctx.Value(reviewRoundSourceCtxKey{}).(reviewRoundSource)
	if !ok || src.Origin == "" {
		return reviewRoundSource{}, false
	}
	return src, true
}

// withReviewRedispatch returns ctx carrying the re-dispatch marker.
//
// SERVER-INTERNAL ONLY (#4077, approval condition C4). The marker makes
// runImplementReviewsForTree bypass the #797 same-head dedup and the diff
// secrets check, so it must be settable ONLY by the server-internal boot
// re-dispatch of an orphaned review round. It is a context value under an
// unexported key type: no HTTP handler, MCP tool, request header, query
// parameter or body field sets or derives it, and none may be made to. A
// request path that needs a second review of the same head is a new design,
// not a call to this function.
func withReviewRedispatch(ctx context.Context, rd reviewRedispatch) context.Context {
	return context.WithValue(ctx, reviewRedispatchCtxKey{}, rd)
}

// reviewRerun is the OPERATOR RE-RUN marker (#4219), a second same-head review
// path DISTINCT from the ADR-091 D6 boot re-dispatch marker above (whose key,
// setter and reachability guard it does not touch). Of is the audit sequence
// of the implement_review_started entry of the settled, verdict-less round the
// operator re-runs; StageID and HeadSHA are that round's stage and reviewed
// head.
//
// The marker is a CLAIM, not a credential: carrying it bypasses nothing by
// itself. runImplementReviewsForTree honours it only when rerunBypassApplies
// re-verifies it against the audit chain — the dispatch's (stage, head) equal
// the marker's AND the run's newest implement_review_started is exactly round
// Of — and the #797 dedup check re-verifies it INSIDE reviewDispatchMu, so a
// second dispatch carrying the same marker finds the first's new round newest
// and dedups as an ordinary round would.
//
// That single-round guarantee is PROCESS-LOCAL (#4282): the in-lock
// re-verification is serialised by reviewDispatchMu and concurrent POSTs by
// claimRerun's pendingReruns set, and both live in this fishhawkd's memory.
// Two fishhawkd replicas each holding the same marker can both observe round
// Of newest under their own lock and each start a round for it — the same
// single-replica limitation as the ADR-091 D6 boot re-dispatch path.
// TestRunImplementReviewsForTree_RerunMarkerConcurrentDispatchStartsOneRound
// pins the in-process half.
type reviewRerun struct {
	Of      int64
	StageID uuid.UUID
	HeadSHA string
}

// reviewRerunCtxKey is unexported for the reason reviewRedispatchCtxKey is: no
// code outside this package can construct it. Its only setter is withReviewRerun,
// whose only production caller is handleRerunReview's dispatch
// (pinned by TestReviewRerunMarker_Reachability).
type reviewRerunCtxKey struct{}

// withReviewRerun returns ctx carrying the operator re-run marker. Called only
// by the re-run verb after its refusals (review_rerun.go).
func withReviewRerun(ctx context.Context, rr reviewRerun) context.Context {
	return context.WithValue(ctx, reviewRerunCtxKey{}, rr)
}

// reviewRerunFrom returns the re-run marker ctx carries. ok=false when it
// carries none, or a marker naming no round (Of <= 0) or no stage: a malformed
// marker never unlocks anything.
func reviewRerunFrom(ctx context.Context) (reviewRerun, bool) {
	rr, ok := ctx.Value(reviewRerunCtxKey{}).(reviewRerun)
	if !ok || rr.Of <= 0 || rr.StageID == uuid.Nil {
		return reviewRerun{}, false
	}
	return rr, true
}

// reviewRedispatchFrom returns the re-dispatch marker ctx carries. ok=false
// when it carries none, or a marker that names no orphaned round (Of <= 0) or
// no depth (Depth <= 0): a malformed marker never unlocks the dedup bypass.
func reviewRedispatchFrom(ctx context.Context) (reviewRedispatch, bool) {
	rd, ok := ctx.Value(reviewRedispatchCtxKey{}).(reviewRedispatch)
	if !ok || rd.Of <= 0 || rd.Depth <= 0 {
		return reviewRedispatch{}, false
	}
	return rd, true
}
