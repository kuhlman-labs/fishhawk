package server

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/policy"
	"github.com/kuhlman-labs/fishhawk/backend/internal/signing"
	"github.com/kuhlman-labs/fishhawk/backend/internal/timescale"
)

// Review-round context (#4077, slice 1): the round source and the re-dispatch
// marker ride the dispatch context to emitReviewStarted, and the marker alone
// unlocks the #797 same-head dedup bypass and the diff-secrets skip in
// runImplementReviewsForTree.

// startedPayloadsRaw returns the raw payload of every entry of category for
// the run, in append order.
func startedPayloadsRaw(t *testing.T, au *auditFake, runID uuid.UUID, category string) []string {
	t.Helper()
	entries, err := au.ListForRunByCategory(context.Background(), runID, category)
	if err != nil {
		t.Fatalf("list %s: %v", category, err)
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, string(e.Payload))
	}
	return out
}

// startedPayloads decodes every implement_review_started payload for the run.
func startedPayloads(t *testing.T, au *auditFake, runID uuid.UUID) []planreview.ReviewStartedPayload {
	t.Helper()
	var out []planreview.ReviewStartedPayload
	for _, raw := range startedPayloadsRaw(t, au, runID, "implement_review_started") {
		var p planreview.ReviewStartedPayload
		if err := json.Unmarshal([]byte(raw), &p); err != nil {
			t.Fatalf("decode implement_review_started %s: %v", raw, err)
		}
		out = append(out, p)
	}
	return out
}

// TestReviewRoundContext_Helpers pins the two ctx accessors, including the
// fail-closed reads: an empty origin is no source, and a marker naming no
// orphaned round or no depth is no marker.
//
// COUNTERFACTUALS: drop `|| src.Origin == ""` and the empty-origin arm reads
// ok=true; drop `|| rd.Of <= 0 || rd.Depth <= 0` and the malformed-marker arms
// read ok=true → RED.
func TestReviewRoundContext_Helpers(t *testing.T) {
	ctx := context.Background()

	if _, ok := reviewRoundSourceFrom(ctx); ok {
		t.Error("bare ctx: reviewRoundSourceFrom ok = true, want false")
	}
	if _, ok := reviewRedispatchFrom(ctx); ok {
		t.Error("bare ctx: reviewRedispatchFrom ok = true, want false")
	}

	want := reviewRoundSource{Origin: reviewRoundOriginFixupPush, BaseSHA: "base1"}
	if got, ok := reviewRoundSourceFrom(withReviewRoundSource(ctx, want)); !ok || got != want {
		t.Errorf("round source round-trip = %+v ok=%v, want %+v ok=true", got, ok, want)
	}
	if got, ok := reviewRoundSourceFrom(withReviewRoundSource(ctx, reviewRoundSource{BaseSHA: "base1"})); ok {
		t.Errorf("empty-origin source read ok=true (%+v), want false", got)
	}
	// context.WithoutCancel keeps values: the detached review goroutines
	// (consolidated, fix-up backstop, advisory dispatch) still see the source.
	if got, ok := reviewRoundSourceFrom(context.WithoutCancel(withReviewRoundSource(ctx, want))); !ok || got != want {
		t.Errorf("source through WithoutCancel = %+v ok=%v, want %+v", got, ok, want)
	}

	rd := reviewRedispatch{Of: 42, Depth: 1}
	if got, ok := reviewRedispatchFrom(withReviewRedispatch(ctx, rd)); !ok || got != rd {
		t.Errorf("redispatch round-trip = %+v ok=%v, want %+v ok=true", got, ok, rd)
	}
	for _, bad := range []reviewRedispatch{{Of: 0, Depth: 1}, {Of: -3, Depth: 1}, {Of: 42, Depth: 0}, {Of: 42, Depth: -1}} {
		if got, ok := reviewRedispatchFrom(withReviewRedispatch(ctx, bad)); ok {
			t.Errorf("malformed marker %+v read ok=true (%+v), want false", bad, got)
		}
	}
}

// TestEmitReviewStarted_StampsRoundSourceAndRedispatchLineage drives
// emitReviewStarted directly and compares the RAW payload, so byte-identity of
// the no-context shapes is pinned alongside the stamped ones.
//
// COUNTERFACTUAL: drop the `category == "implement_review_started"` gate and
// the "plan round ignores a source" arm carries round_origin → RED.
func TestEmitReviewStarted_StampsRoundSourceAndRedispatchLineage(t *testing.T) {
	src := reviewRoundSource{Origin: reviewRoundOriginFixupPush, BaseSHA: "base1"}
	rd := reviewRedispatch{Of: 41, Depth: 2}
	cases := []struct {
		name     string
		ctx      context.Context
		category string
		agents   int
		head     string
		want     string
	}{
		{
			name: "implement round without context values is byte-identical", ctx: context.Background(),
			category: "implement_review_started", agents: 1, head: "h1",
			want: `{"configured_agents":1,"authority":"advisory","head_sha":"h1"}`,
		},
		{
			name: "implement trace round records its origin", ctx: withReviewRoundSource(context.Background(), reviewRoundSource{Origin: reviewRoundOriginTrace}),
			category: "implement_review_started", agents: 1, head: "h1",
			want: `{"configured_agents":1,"authority":"advisory","head_sha":"h1","round_origin":"trace"}`,
		},
		{
			name: "implement compare round re-dispatched records all four", ctx: withReviewRedispatch(withReviewRoundSource(context.Background(), src), rd),
			category: "implement_review_started", agents: 1, head: "h1",
			want: `{"configured_agents":1,"authority":"advisory","head_sha":"h1","round_origin":"fixup_push","round_base_sha":"base1","redispatch_of":41,"redispatch_depth":2}`,
		},
		{
			name: "implement round with a malformed marker omits the lineage", ctx: withReviewRedispatch(context.Background(), reviewRedispatch{Of: 0, Depth: 1}),
			category: "implement_review_started", agents: 1, head: "h1",
			want: `{"configured_agents":1,"authority":"advisory","head_sha":"h1"}`,
		},
		{
			name: "plan round without context values is byte-identical", ctx: context.Background(),
			category: "plan_review_started", agents: 2,
			want: `{"configured_agents":2,"authority":"advisory"}`,
		},
		{
			name: "plan round ignores a source but records the lineage", ctx: withReviewRedispatch(withReviewRoundSource(context.Background(), src), reviewRedispatch{Of: 9, Depth: 1}),
			category: "plan_review_started", agents: 2,
			want: `{"configured_agents":2,"authority":"advisory","redispatch_of":9,"redispatch_depth":1}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			au := newAuditFake()
			s := New(Config{Addr: "127.0.0.1:0", AuditRepo: au})
			runID, stageID := uuid.New(), uuid.New()
			if _, ok := s.emitReviewStarted(tc.ctx, runID, stageID, tc.category, planreview.AuthorityAdvisory, tc.agents, nil, tc.head, "", "", nil); !ok {
				t.Fatal("emitReviewStarted ok = false, want true")
			}
			got := startedPayloadsRaw(t, au, runID, tc.category)
			if len(got) != 1 || got[0] != tc.want {
				t.Errorf("payloads = %v, want [%s]", got, tc.want)
			}
		})
	}
}

// TestRunImplementReviewsForTree_RedispatchMarkerBypassesSameHeadDedup pins
// the #797 bypass: an implement_review_started already exists for (stage,
// head). Without the marker — and with a malformed one — the round is deduped
// (no new started, no reviewer call). With the marker the round dispatches
// against the SAME head and records its lineage.
//
// COUNTERFACTUAL (C6): drop `&& !redispatch` from the dedup guard → the
// marker arm is deduped too (started count stays 1) → RED.
func TestRunImplementReviewsForTree_RedispatchMarkerBypassesSameHeadDedup(t *testing.T) {
	reviewer := &fakePlanReviewer{verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictApprove}, model: "claude-opus-4-8"}
	s, _, au, _, runRow, implStage := newImplementReviewServer(t, reviewer, specImplementAdvisoryReviewers)
	const head = "orphaned-head"
	seedImplementReviewStartedSeq(au, runRow.ID, implStage.ID, head, 7)
	diff := policy.Diff{ChangedFiles: []policy.ChangedFile{{Path: "backend/internal/foo/foo.go", Status: policy.StatusModified}}}
	traceCtx := withReviewRoundSource(context.Background(), reviewRoundSource{Origin: reviewRoundOriginTrace})

	for _, ctx := range []context.Context{traceCtx, withReviewRedispatch(traceCtx, reviewRedispatch{Of: 0, Depth: 1})} {
		s.runImplementReviewsForTree(ctx, runRow.ID, implStage.ID, diff, nil, head, "", "", nil)
		s.waitBackgroundReviews()
	}
	if got := startedPayloads(t, au, runRow.ID); len(got) != 1 {
		t.Fatalf("started rounds without a valid marker = %d, want 1 (the seeded round; the same head must dedup)", len(got))
	}
	reviewer.mu.Lock()
	calls := len(reviewer.calls)
	reviewer.mu.Unlock()
	if calls != 0 {
		t.Fatalf("reviewer invocations without a valid marker = %d, want 0", calls)
	}

	s.runImplementReviewsForTree(withReviewRedispatch(traceCtx, reviewRedispatch{Of: 7, Depth: 1}), runRow.ID, implStage.ID, diff, nil, head, "", "", nil)
	s.waitBackgroundReviews()
	got := startedPayloads(t, au, runRow.ID)
	if len(got) != 2 {
		t.Fatalf("started rounds after the re-dispatch = %d, want 2 (the marker bypasses the same-head dedup)", len(got))
	}
	last := got[1]
	if last.HeadSHA != head || last.RoundOrigin != reviewRoundOriginTrace || last.RedispatchOf != 7 || last.RedispatchDepth != 1 {
		t.Errorf("re-dispatched round = %+v, want head %q, origin trace, redispatch_of 7, depth 1", last, head)
	}
	reviewer.mu.Lock()
	defer reviewer.mu.Unlock()
	if len(reviewer.calls) != 1 {
		t.Errorf("reviewer invocations after the re-dispatch = %d, want 1", len(reviewer.calls))
	}
}

// TestRunImplementReviewsForTree_RedispatchMarkerSkipsDiffSecretsCheck: an
// ordinary round over a credential-bearing diff raises the diff-secrets
// concern; a re-dispatched round over the same diff raises nothing (the
// orphaned round's dispatch already ran the check).
//
// COUNTERFACTUAL (C7): drop the `if !redispatch` around raiseDiffSecretConcerns
// → the re-dispatch arm raises one row and appends diff_secrets_detected → RED.
func TestRunImplementReviewsForTree_RedispatchMarkerSkipsDiffSecretsCheck(t *testing.T) {
	cases := []struct {
		name     string
		marker   bool
		wantRows int
	}{
		{name: "ordinary round runs the check", marker: false, wantRows: 1},
		{name: "re-dispatched round skips the check", marker: true, wantRows: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reviewer := &fakePlanReviewer{verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictApprove}, model: "claude-opus-4-8"}
			s, _, au, _, runRow, implStage := newImplementReviewServer(t, reviewer, specImplementAdvisoryReviewers)
			cr := newFakeConcernRepo()
			s.cfg.ConcernRepo = cr
			ctx := withReviewRoundSource(context.Background(), reviewRoundSource{Origin: reviewRoundOriginTrace})
			if tc.marker {
				ctx = withReviewRedispatch(ctx, reviewRedispatch{Of: 7, Depth: 1})
			}
			s.runImplementReviewsForTree(ctx, runRow.ID, implStage.ID, keyDiff(), nil, "secret-head", "", "", nil)
			s.waitBackgroundReviews()

			if n := len(serverCheckRows(t, cr, runRow.ID)); n != tc.wantRows {
				t.Errorf("server_check rows = %d, want %d", n, tc.wantRows)
			}
			if p, _ := diffSecretsEntries(t, au); len(p) != tc.wantRows {
				t.Errorf("%s entries = %d, want %d", diffSecretsDetectedCategory, len(p), tc.wantRows)
			}
			// The round itself still dispatched in both arms.
			if got := startedPayloads(t, au, runRow.ID); len(got) != 1 {
				t.Errorf("started rounds = %d, want 1", len(got))
			}
		})
	}
}

// TestTraceUpload_CannotCarryRedispatchMarker is approval condition C4: the
// re-dispatch marker is settable only by the server-internal boot sweep. A
// trace upload through the REAL HTTP handler — carrying every plausible
// request-side spelling of the marker (headers, query parameters) — records an
// ordinary trace round with NO re-dispatch lineage, and a retried upload of the
// same head is still deduped by #797.
//
// COUNTERFACTUAL: make the trace hook derive the marker from the request (wrap
// reviewCtx with withReviewRedispatch from the X-Fishhawk-Redispatch-Of header)
// → the first round records redispatch_of and the retry dispatches a SECOND
// round for the same head → RED on both assertions.
func TestTraceUpload_CannotCarryRedispatchMarker(t *testing.T) {
	reviewer := &fakePlanReviewer{verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictApprove}, model: "claude-opus-4-8"}
	s, sf, au, _, runRow, implStage := newImplementReviewServer(t, reviewer, specImplementAdvisoryReviewers)
	priv, _ := sf.issue(t, runRow.ID)
	bundleBytes := implementPushGatedBundleWithHeadSHA(t, 2, "same-head")

	for i := 0; i < 2; i++ {
		url := fmt.Sprintf("/v0/runs/%s/trace?stage_id=%s&variant=raw&redispatch_of=1&redispatch_depth=1&redispatch=true", runRow.ID, implStage.ID)
		req := httptest.NewRequest(http.MethodPost, url, bytes.NewReader(bundleBytes))
		req.Header.Set("X-Fishhawk-Signature", hex.EncodeToString(ed25519.Sign(priv, signing.ComputeMessage(bundleBytes))))
		req.Header.Set("X-Fishhawk-Redispatch", "true")
		req.Header.Set("X-Fishhawk-Redispatch-Of", "1")
		req.Header.Set("X-Fishhawk-Redispatch-Depth", "1")
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, req)
		if w.Code != http.StatusAccepted {
			t.Fatalf("upload %d status = %d, want 202:\n%s", i, w.Code, w.Body.String())
		}
	}
	s.waitBackgroundReviews()

	got := startedPayloads(t, au, runRow.ID)
	if len(got) != 1 {
		t.Fatalf("implement_review_started rounds = %d, want 1 (a request cannot unlock the same-head dedup bypass)", len(got))
	}
	if got[0].RedispatchOf != 0 || got[0].RedispatchDepth != 0 {
		t.Errorf("trace round lineage = of %d depth %d, want none (a request cannot carry the marker)", got[0].RedispatchOf, got[0].RedispatchDepth)
	}
	if got[0].RoundOrigin != reviewRoundOriginTrace || got[0].RoundBaseSHA != "" || got[0].HeadSHA != "same-head" {
		t.Errorf("trace round source = %+v, want origin trace, no base, head same-head", got[0])
	}
}

// TestDispatchConsolidatedReview_RecordsConsolidatedRoundSource: the
// consolidated dispatcher stamps origin consolidated plus its compare base.
//
// COUNTERFACTUAL: pass reviewCtx instead of srcCtx at the consolidated call
// site → round_origin and round_base_sha are empty → RED.
func TestDispatchConsolidatedReview_RecordsConsolidatedRoundSource(t *testing.T) {
	rr := newOrchestratorRepo()
	art := newFakeArtifactRepo()
	au := newAuditFake()
	reviewer := &fakePlanReviewer{verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictApprove}, model: "claude-opus-4-8"}
	parent, _ := seedConsolidatedParent(t, rr, art, specImplementAdvisoryReviewers)
	s := New(Config{
		Addr:          "127.0.0.1:0",
		RunRepo:       rr,
		ArtifactRepo:  art,
		AuditRepo:     au,
		PlanReviewers: singleReviewerSet{reviewer},
		GitHub:        cannedComparePatchClient(t, cannedCompareOneFile),
	})

	s.DispatchConsolidatedReview(context.Background(), parent.ID, "consolidated-base", "fishhawk/run-"+parent.ID.String()[:8])
	s.waitBackgroundReviews()

	got := startedPayloads(t, au, parent.ID)
	if len(got) != 1 {
		t.Fatalf("implement_review_started rounds = %d, want 1", len(got))
	}
	if got[0].RoundOrigin != reviewRoundOriginConsolidated || got[0].RoundBaseSHA != "consolidated-base" {
		t.Errorf("consolidated round source = origin %q base %q, want consolidated / consolidated-base", got[0].RoundOrigin, got[0].RoundBaseSHA)
	}
	if got[0].RedispatchOf != 0 || got[0].RedispatchDepth != 0 {
		t.Errorf("ordinary consolidated round carries lineage %+v, want none", got[0])
	}
}

// TestBackstopFixupReReview_RecordsFixupPushRoundSource: the post-fix-up
// re-review stamps origin fixup_push plus the pass's compare base.
//
// COUNTERFACTUAL: pass reviewCtx instead of srcCtx at the fix-up call site →
// round_origin and round_base_sha are empty → RED.
func TestBackstopFixupReReview_RecordsFixupPushRoundSource(t *testing.T) {
	reviewer := &fakePlanReviewer{verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictApprove}, model: "claude-opus-4-8"}
	s, _, au, _, runRow, implStage := newFixupReReviewBackstopServer(t, reviewer, cannedCompareOneFile, false)

	s.maybeBackstopFixupReReview(context.Background(), runRow.ID, implStage, "head-pushed", "base-pass")
	s.waitBackgroundReviews()

	got := startedPayloads(t, au, runRow.ID)
	if len(got) != 1 {
		t.Fatalf("implement_review_started rounds = %d, want 1", len(got))
	}
	if got[0].RoundOrigin != reviewRoundOriginFixupPush || got[0].RoundBaseSHA != "base-pass" || got[0].HeadSHA != "head-pushed" {
		t.Errorf("fix-up round source = %+v, want origin fixup_push, base base-pass, head head-pushed", got[0])
	}
	if got[0].RedispatchOf != 0 || got[0].RedispatchDepth != 0 {
		t.Errorf("ordinary fix-up round carries lineage %+v, want none", got[0])
	}
}

// --- Operator re-run marker (#4219) -----------------------------------------

// appendRerunTargetRound appends (through the chain, so it sequences after
// anything already appended) a settled-shape implement_review_started for
// (stageID, head) and returns its stamped sequence. au must stampSequence.
func appendRerunTargetRound(t *testing.T, au *auditFake, runID, stageID uuid.UUID, head string) int64 {
	t.Helper()
	payload, _ := json.Marshal(planreview.ReviewStartedPayload{ConfiguredAgents: 1, Authority: planreview.AuthorityAdvisory, HeadSHA: head, RoundOrigin: reviewRoundOriginTrace})
	sid := stageID
	if _, err := au.AppendChained(context.Background(), audit.ChainAppendParams{
		RunID: runID, StageID: &sid, Category: "implement_review_started", Payload: payload,
	}); err != nil {
		t.Fatalf("append implement_review_started: %v", err)
	}
	au.mu.Lock()
	defer au.mu.Unlock()
	return int64(len(au.appended))
}

// TestReviewRerunMarker_Helpers pins reviewRerunFrom's fail-closed reads.
//
// COUNTERFACTUAL: drop `|| rr.Of <= 0 || rr.StageID == uuid.Nil` → the
// malformed arms read ok=true → RED.
func TestReviewRerunMarker_Helpers(t *testing.T) {
	ctx := context.Background()
	if _, ok := reviewRerunFrom(ctx); ok {
		t.Error("bare ctx: reviewRerunFrom ok = true, want false")
	}
	want := reviewRerun{Of: 9, StageID: uuid.New(), HeadSHA: "h"}
	if got, ok := reviewRerunFrom(context.WithoutCancel(withReviewRerun(ctx, want))); !ok || got != want {
		t.Errorf("rerun marker round-trip = %+v ok=%v, want %+v ok=true", got, ok, want)
	}
	for _, bad := range []reviewRerun{{Of: 0, StageID: uuid.New()}, {Of: -1, StageID: uuid.New()}, {Of: 9}} {
		if got, ok := reviewRerunFrom(withReviewRerun(ctx, bad)); ok {
			t.Errorf("malformed rerun marker %+v read ok=true (%+v), want false", bad, got)
		}
	}
	// The two markers are distinct: neither reads the other.
	if _, ok := reviewRedispatchFrom(withReviewRerun(ctx, want)); ok {
		t.Error("a re-run marker reads as the ADR-091 D6 re-dispatch marker")
	}
	if _, ok := reviewRerunFrom(withReviewRedispatch(ctx, reviewRedispatch{Of: 9, Depth: 1})); ok {
		t.Error("the D6 re-dispatch marker reads as a re-run marker")
	}
}

// TestEmitReviewStarted_StampsRerunOf: an implement round carrying a valid
// re-run marker records rerun_of; a plan round and a malformed marker omit it.
//
// COUNTERFACTUAL: drop the `category == "implement_review_started"` gate on
// the rerun_of stamp → the plan arm carries rerun_of → RED.
func TestEmitReviewStarted_StampsRerunOf(t *testing.T) {
	rr := reviewRerun{Of: 12, StageID: uuid.New(), HeadSHA: "h1"}
	cases := []struct {
		name, category, want string
		ctx                  context.Context
	}{
		{name: "implement re-run round", category: "implement_review_started", ctx: withReviewRerun(context.Background(), rr),
			want: `{"configured_agents":1,"authority":"advisory","head_sha":"h1","rerun_of":12}`},
		{name: "implement malformed marker", category: "implement_review_started", ctx: withReviewRerun(context.Background(), reviewRerun{Of: 12}),
			want: `{"configured_agents":1,"authority":"advisory","head_sha":"h1"}`},
		{name: "plan round ignores the marker", category: "plan_review_started", ctx: withReviewRerun(context.Background(), rr),
			want: `{"configured_agents":1,"authority":"advisory","head_sha":"h1"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			au := newAuditFake()
			s := New(Config{Addr: "127.0.0.1:0", AuditRepo: au})
			runID := uuid.New()
			if _, ok := s.emitReviewStarted(tc.ctx, runID, rr.StageID, tc.category, planreview.AuthorityAdvisory, 1, nil, "h1", "", "", nil); !ok {
				t.Fatal("emitReviewStarted ok = false")
			}
			if got := startedPayloadsRaw(t, au, runID, tc.category); len(got) != 1 || got[0] != tc.want {
				t.Errorf("payloads = %v, want [%s]", got, tc.want)
			}
		})
	}
}

// TestRunImplementReviewsForTree_RerunMarkerIsNarrow pins the self-verifying
// re-run bypass of the #797 same-head dedup: TWO rounds already exist for
// (stage, head) — an older one and the target at sequence `of`. Every marker
// that does not name THIS dispatch — none, a stale Of, a wrong head, a wrong
// stage — dedups (no new round, no reviewer call). The valid marker dispatches
// ONE round stamped rerun_of; a second dispatch carrying the SAME marker then
// finds that new round newest and dedups, which is the in-lock re-verification
// that makes two sequential re-runs start exactly one round (the concurrent
// shape is TestRunImplementReviewsForTree_RerunMarkerConcurrentDispatchStartsOneRound).
//
// The stale arm's Of is the OLDER seeded round: a real, positive sequence, so
// reviewRerunFrom reads the marker ok (asserted as a fixture precondition on
// every marker arm) and its stage and head equal the dispatch's — the
// `newest.Sequence != rr.Of` conjunct is the only thing that rejects it. (An
// `of-1` Of on a fresh fake is 0, which reviewRerunFrom rejects before the
// newest==Of comparison is ever reached, so it could not pin that conjunct.)
//
// COUNTERFACTUALS (fixture mechanism: the arm's marker reads ok and names the
// dispatch's stage and head, so the deleted conjunct is the only rejection):
// drop `newest.Sequence != rr.Of` from rerunBypassApplies → the stale arm's
// in-lock re-verification passes, it bypasses the dedup and starts a third
// round → RED; rerunBypassApplies returning true for any present marker → the
// stale/wrong arms dispatch → RED; dropping the `!s.rerunBypassApplies(...)`
// term from the dedup guard → the valid arm dedups → RED.
func TestRunImplementReviewsForTree_RerunMarkerIsNarrow(t *testing.T) {
	reviewer := &fakePlanReviewer{verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictApprove}, model: "claude-opus-4-8"}
	s, _, au, _, runRow, implStage := newImplementReviewServer(t, reviewer, specImplementAdvisoryReviewers)
	au.stampSequence = true
	const head = "rerun-head"
	older := appendRerunTargetRound(t, au, runRow.ID, implStage.ID, head)
	of := appendRerunTargetRound(t, au, runRow.ID, implStage.ID, head)
	diff := policy.Diff{ChangedFiles: []policy.ChangedFile{{Path: "backend/internal/foo/foo.go", Status: policy.StatusModified}}}
	traceCtx := withReviewRoundSource(context.Background(), reviewRoundSource{Origin: reviewRoundOriginTrace})
	reviewerCalls := func() int {
		reviewer.mu.Lock()
		defer reviewer.mu.Unlock()
		return len(reviewer.calls)
	}

	for _, tc := range []struct {
		name   string
		ctx    context.Context
		marker bool
	}{
		{"no marker", traceCtx, false},
		{"stale Of", withReviewRerun(traceCtx, reviewRerun{Of: older, StageID: implStage.ID, HeadSHA: head}), true},
		{"wrong head", withReviewRerun(traceCtx, reviewRerun{Of: of, StageID: implStage.ID, HeadSHA: "other-head"}), true},
		{"wrong stage", withReviewRerun(traceCtx, reviewRerun{Of: of, StageID: uuid.New(), HeadSHA: head}), true},
	} {
		if _, ok := reviewRerunFrom(tc.ctx); ok != tc.marker {
			t.Fatalf("%s: fixture precondition: reviewRerunFrom ok = %v, want %v (a marker arm must not regress to a malformed marker the helper rejects first)", tc.name, ok, tc.marker)
		}
		s.runImplementReviewsForTree(tc.ctx, runRow.ID, implStage.ID, diff, nil, head, "", "", nil)
		s.waitBackgroundReviews()
		if got := startedPayloads(t, au, runRow.ID); len(got) != 2 {
			t.Fatalf("%s: started rounds = %d, want 2 (the same head must dedup)", tc.name, len(got))
		}
		if n := reviewerCalls(); n != 0 {
			t.Fatalf("%s: reviewer invocations = %d, want 0", tc.name, n)
		}
	}

	valid := withReviewRerun(traceCtx, reviewRerun{Of: of, StageID: implStage.ID, HeadSHA: head})
	s.runImplementReviewsForTree(valid, runRow.ID, implStage.ID, diff, nil, head, "", "", nil)
	s.waitBackgroundReviews()
	got := startedPayloads(t, au, runRow.ID)
	if len(got) != 3 {
		t.Fatalf("started rounds after the valid re-run = %d, want 3", len(got))
	}
	if got[2].RerunOf != of || got[2].HeadSHA != head || got[2].RedispatchOf != 0 {
		t.Errorf("re-run round = %+v, want rerun_of %d, head %q, no D6 lineage", got[2], of, head)
	}
	if n := reviewerCalls(); n != 1 {
		t.Errorf("reviewer invocations after the valid re-run = %d, want 1", n)
	}

	s.runImplementReviewsForTree(valid, runRow.ID, implStage.ID, diff, nil, head, "", "", nil)
	s.waitBackgroundReviews()
	if got := startedPayloads(t, au, runRow.ID); len(got) != 3 {
		t.Fatalf("started rounds after a second dispatch with the same marker = %d, want 3 (the marker is spent once a newer round exists)", len(got))
	}
}

// TestRunImplementReviewsForTree_RerunMarkerSkipsDiffSecretsOnlyWhenValid: the
// re-run round was seeded BY CONSTRUCTION without its secrets check, so a
// raised row is attributable to THIS dispatch. A valid marker skips the check
// (the re-run round's own dispatch ran it); every marker that fails
// re-verification runs it like an ordinary round. Each failing arm's marker
// reads ok (asserted as a fixture precondition) and names the dispatch's own
// stage and head, so exactly ONE rerunBypassApplies conjunct rejects it:
//
//   - stale Of: two rounds on (stage, head), Of = the OLDER one — only
//     `newest.Sequence != rr.Of` rejects (CF28b);
//   - newest round on another stage: the round at Of was recorded on another
//     stage with the same head — only `*newest.StageID != stageID` rejects
//     (CF29);
//   - newest round on another head: the round at Of is on this stage but
//     another head — only `p.HeadSHA == headSHA` rejects (CF30).
//
// COUNTERFACTUALS: drop `&& !s.rerunBypassApplies(...)` from the secrets guard
// → the valid arm raises one row → RED; drop `newest.Sequence != rr.Of` → the
// stale arm raises nothing → RED; drop `|| *newest.StageID != stageID` → the
// another-stage arm raises nothing → RED; `return p.HeadSHA == headSHA` →
// `return true` → the another-head arm raises nothing → RED.
func TestRunImplementReviewsForTree_RerunMarkerSkipsDiffSecretsOnlyWhenValid(t *testing.T) {
	const head = "secret-head"
	for _, tc := range []struct {
		name string
		// seed appends the arm's implement_review_started rounds and returns
		// the marker's Of.
		seed     func(t *testing.T, au *auditFake, runID, stageID uuid.UUID) int64
		wantRows int
	}{
		{name: "valid marker skips the check", wantRows: 0, seed: func(t *testing.T, au *auditFake, runID, stageID uuid.UUID) int64 {
			return appendRerunTargetRound(t, au, runID, stageID, head)
		}},
		{name: "stale Of runs the check", wantRows: 1, seed: func(t *testing.T, au *auditFake, runID, stageID uuid.UUID) int64 {
			older := appendRerunTargetRound(t, au, runID, stageID, head)
			appendRerunTargetRound(t, au, runID, stageID, head)
			return older
		}},
		{name: "newest round on another stage runs the check", wantRows: 1, seed: func(t *testing.T, au *auditFake, runID, _ uuid.UUID) int64 {
			return appendRerunTargetRound(t, au, runID, uuid.New(), head)
		}},
		{name: "newest round on another head runs the check", wantRows: 1, seed: func(t *testing.T, au *auditFake, runID, stageID uuid.UUID) int64 {
			return appendRerunTargetRound(t, au, runID, stageID, "other-head")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reviewer := &fakePlanReviewer{verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictApprove}, model: "claude-opus-4-8"}
			s, _, au, _, runRow, implStage := newImplementReviewServer(t, reviewer, specImplementAdvisoryReviewers)
			au.stampSequence = true
			cr := newFakeConcernRepo()
			s.cfg.ConcernRepo = cr
			of := tc.seed(t, au, runRow.ID, implStage.ID)
			ctx := withReviewRerun(withReviewRoundSource(context.Background(), reviewRoundSource{Origin: reviewRoundOriginTrace}),
				reviewRerun{Of: of, StageID: implStage.ID, HeadSHA: head})
			if _, ok := reviewRerunFrom(ctx); !ok {
				t.Fatalf("fixture precondition: reviewRerunFrom ok = false for Of %d (the arm must carry a well-formed marker)", of)
			}
			s.runImplementReviewsForTree(ctx, runRow.ID, implStage.ID, keyDiff(), nil, head, "", "", nil)
			s.waitBackgroundReviews()
			if n := len(serverCheckRows(t, cr, runRow.ID)); n != tc.wantRows {
				t.Errorf("server_check rows = %d, want %d", n, tc.wantRows)
			}
		})
	}
}

// TestRerunBypassApplies_EachConjunctBinds is the conjunct table for
// rerunBypassApplies (#4282). The dispatch is always (stage S, head "h"). Each
// false arm is built so that exactly ONE conjunct rejects — every other one
// passes on that fixture — so deleting that conjunct flips the arm to true.
// Two of them (the newest round's stage, the newest round's head) are
// unreachable through handleRerunReview, which builds the marker from the
// newest round itself; this table is where they bind.
//
// COUNTERFACTUALS (each mutates the BODY of rerunBypassApplies): drop
// `newest.Sequence != rr.Of` → "stale Of" true → RED; drop
// `|| *newest.StageID != stageID` → "newest round on another stage" true →
// RED; `return p.HeadSHA == headSHA` → `return true` → "newest round on another
// head" true → RED; drop `rr.StageID != stageID` → "marker stage != dispatch
// stage" true → RED; drop `rr.HeadSHA != headSHA` → "marker head != dispatch
// head" true → RED; return false unconditionally → "valid" false → RED.
func TestRerunBypassApplies_EachConjunctBinds(t *testing.T) {
	const head = "h"
	stageS := uuid.New()
	stageT := uuid.New()
	type round struct {
		stage uuid.UUID
		head  string
	}
	for _, tc := range []struct {
		name string
		// rounds are appended in order; Of is the sequence of rounds[ofIdx].
		rounds []round
		ofIdx  int
		// noMarker dispatches with no re-run marker at all.
		noMarker    bool
		markerStage uuid.UUID
		markerHead  string
		want        bool
	}{
		{name: "valid", rounds: []round{{stageS, head}}, ofIdx: 0, markerStage: stageS, markerHead: head, want: true},
		{name: "stale Of (a real older round)", rounds: []round{{stageS, head}, {stageS, head}}, ofIdx: 0, markerStage: stageS, markerHead: head},
		{name: "newest round on another stage", rounds: []round{{stageT, head}}, ofIdx: 0, markerStage: stageS, markerHead: head},
		{name: "newest round on another head", rounds: []round{{stageS, "other-head"}}, ofIdx: 0, markerStage: stageS, markerHead: head},
		{name: "marker stage != dispatch stage", rounds: []round{{stageS, head}}, ofIdx: 0, markerStage: stageT, markerHead: head},
		{name: "marker head != dispatch head", rounds: []round{{stageS, head}}, ofIdx: 0, markerStage: stageS, markerHead: "other-head"},
		{name: "no marker", rounds: []round{{stageS, head}}, ofIdx: 0, noMarker: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			au := newAuditFake()
			au.stampSequence = true
			s := New(Config{Addr: "127.0.0.1:0", AuditRepo: au})
			runID := uuid.New()
			var seqs []int64
			for _, r := range tc.rounds {
				seqs = append(seqs, appendRerunTargetRound(t, au, runID, r.stage, r.head))
			}
			ctx := context.Background()
			if !tc.noMarker {
				ctx = withReviewRerun(ctx, reviewRerun{Of: seqs[tc.ofIdx], StageID: tc.markerStage, HeadSHA: tc.markerHead})
				if _, ok := reviewRerunFrom(ctx); !ok {
					t.Fatalf("fixture precondition: reviewRerunFrom ok = false (the arm must carry a well-formed marker)")
				}
			}
			if got := s.rerunBypassApplies(ctx, runID, stageS, head); got != tc.want {
				t.Errorf("rerunBypassApplies = %v, want %v", got, tc.want)
			}
		})
	}
}

// rendezvousAuditRepo wraps the fixture's auditFake and HOLDS the first two
// ListForRunByCategory reads of category until both have arrived, so two
// concurrent dispatches observe the chain at the same point. Every later read
// passes straight through. A held read is released when the second arrives,
// on timeout (so correct code can never deadlock here) and at t.Cleanup. The
// hook never holds the fake's mutex while it waits, and never touches t: the
// test reads bothArrived as a precondition instead. bothArrived closes ONLY
// when the second read arrives while the first is still held — a second read
// that comes after the first timed out (the two were serialised, e.g. both
// behind reviewDispatchMu) leaves it open, so a timeout-release fails the
// precondition rather than passing it vacuously.
type rendezvousAuditRepo struct {
	*auditFake
	category string
	timeout  time.Duration

	mu          sync.Mutex
	arrived     int
	timedOut    bool          // the first held read gave up waiting
	bothArrived chan struct{} // closed when the second read meets the held first
	release     chan struct{} // closed once: both arrived, timeout or cleanup
	releaseOnce sync.Once
}

func newRendezvousAuditRepo(t *testing.T, au *auditFake, category string) *rendezvousAuditRepo {
	r := &rendezvousAuditRepo{
		auditFake:   au,
		category:    category,
		timeout:     timescale.D(5 * time.Second),
		bothArrived: make(chan struct{}),
		release:     make(chan struct{}),
	}
	t.Cleanup(r.open)
	return r
}

func (r *rendezvousAuditRepo) open() { r.releaseOnce.Do(func() { close(r.release) }) }

func (r *rendezvousAuditRepo) ListForRunByCategory(ctx context.Context, runID uuid.UUID, category string) ([]*audit.Entry, error) {
	if category == r.category {
		r.mu.Lock()
		r.arrived++
		n := r.arrived
		r.mu.Unlock()
		switch n {
		case 1:
			select {
			case <-r.release:
			case <-time.After(r.timeout):
				r.mu.Lock()
				r.timedOut = true
				r.mu.Unlock()
				r.open()
			}
		case 2:
			r.mu.Lock()
			met := !r.timedOut
			r.mu.Unlock()
			if met {
				close(r.bothArrived)
			}
			r.open()
		}
	}
	return r.auditFake.ListForRunByCategory(ctx, runID, category)
}

// TestRunImplementReviewsForTree_RerunMarkerConcurrentDispatchStartsOneRound
// pins the IN-LOCK re-verification of the re-run marker (#4282, CF34)
// deterministically: two dispatches carrying the SAME valid marker run
// concurrently and exactly one round starts.
//
// Fixture mechanism: each goroutine's FIRST implement_review_started read is
// its entry-time rerunBypassApplies (the diff-secrets check, OUTSIDE
// reviewDispatchMu — nothing earlier in runImplementReviewsForTree reads that
// category). The rendezvous holds those two reads until both arrive, so both
// entry-time verifications observe the target round as newest BEFORE either
// goroutine emits; both therefore reach reviewDispatchMu believing the bypass
// applies, and only the re-verification INSIDE the lock — where the second
// holder finds the first's new round newest — keeps the second from starting
// a round. That both reads met is asserted as a PRECONDITION: a rendezvous
// that released on timeout proves nothing and fails the test.
//
// COUNTERFACTUALS: replace the in-lock `s.rerunBypassApplies(...)` with the
// value computed at the entry-time secrets check → both goroutines carry
// bypass=true into the lock and both emit → 2 new rounds → RED; drop the
// in-lock `!s.rerunBypassApplies(...)` term (always dedup) → 0 new rounds →
// RED.
func TestRunImplementReviewsForTree_RerunMarkerConcurrentDispatchStartsOneRound(t *testing.T) {
	reviewer := &fakePlanReviewer{verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictApprove}, model: "claude-opus-4-8"}
	s, _, au, _, runRow, implStage := newImplementReviewServer(t, reviewer, specImplementAdvisoryReviewers)
	au.stampSequence = true
	const head = "concurrent-head"
	of := appendRerunTargetRound(t, au, runRow.ID, implStage.ID, head)
	rv := newRendezvousAuditRepo(t, au, "implement_review_started")
	s.cfg.AuditRepo = rv

	diff := policy.Diff{ChangedFiles: []policy.ChangedFile{{Path: "backend/internal/foo/foo.go", Status: policy.StatusModified}}}
	ctx := withReviewRerun(withReviewRoundSource(context.Background(), reviewRoundSource{Origin: reviewRoundOriginTrace}),
		reviewRerun{Of: of, StageID: implStage.ID, HeadSHA: head})
	if _, ok := reviewRerunFrom(ctx); !ok {
		t.Fatal("fixture precondition: reviewRerunFrom ok = false (the marker must be well-formed)")
	}

	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.runImplementReviewsForTree(ctx, runRow.ID, implStage.ID, diff, nil, head, "", "", nil)
		}()
	}
	wg.Wait()
	s.waitBackgroundReviews()

	select {
	case <-rv.bothArrived:
	default:
		t.Fatalf("precondition: the two entry-time implement_review_started reads did not meet within %s — the rendezvous released on timeout, so the two dispatches were not concurrent at the entry-time verification and this run proves nothing", rv.timeout)
	}

	got := startedPayloads(t, au, runRow.ID)
	if len(got) != 2 {
		t.Fatalf("started rounds = %d, want 2 (the target plus exactly ONE re-run round)", len(got))
	}
	if got[1].RerunOf != of || got[1].HeadSHA != head {
		t.Errorf("re-run round = %+v, want rerun_of %d, head %q", got[1], of, head)
	}
	reviewer.mu.Lock()
	calls := len(reviewer.calls)
	reviewer.mu.Unlock()
	if calls != 1 {
		t.Errorf("reviewer invocations = %d, want 1", calls)
	}
}

// TestRunImplementReviewsForTree_RerunVerificationReadErrorDedups: an audit
// read error during the marker's re-verification never unlocks the bypass.
//
// COUNTERFACTUAL: make rerunBypassApplies return true on a read error → a
// second round dispatches → RED.
func TestRunImplementReviewsForTree_RerunVerificationReadErrorDedups(t *testing.T) {
	reviewer := &fakePlanReviewer{verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictApprove}, model: "claude-opus-4-8"}
	s, _, au, _, runRow, implStage := newImplementReviewServer(t, reviewer, specImplementAdvisoryReviewers)
	au.stampSequence = true
	of := appendRerunTargetRound(t, au, runRow.ID, implStage.ID, "h")
	ctx := withReviewRerun(context.Background(), reviewRerun{Of: of, StageID: implStage.ID, HeadSHA: "h"})
	au.mu.Lock()
	au.listByCategoryErr = errors.New("injected")
	au.mu.Unlock()
	if s.rerunBypassApplies(ctx, runRow.ID, implStage.ID, "h") {
		t.Fatal("rerunBypassApplies = true on an audit read error, want false (fail-closed)")
	}
}
