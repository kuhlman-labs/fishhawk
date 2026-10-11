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
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/policy"
	"github.com/kuhlman-labs/fishhawk/backend/internal/signing"
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
			if _, ok := s.emitReviewStarted(tc.ctx, runID, stageID, tc.category, planreview.AuthorityAdvisory, tc.agents, nil, tc.head, "", ""); !ok {
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
			if _, ok := s.emitReviewStarted(tc.ctx, runID, rr.StageID, tc.category, planreview.AuthorityAdvisory, 1, nil, "h1", "", ""); !ok {
				t.Fatal("emitReviewStarted ok = false")
			}
			if got := startedPayloadsRaw(t, au, runID, tc.category); len(got) != 1 || got[0] != tc.want {
				t.Errorf("payloads = %v, want [%s]", got, tc.want)
			}
		})
	}
}

// TestRunImplementReviewsForTree_RerunMarkerIsNarrow pins the self-verifying
// re-run bypass of the #797 same-head dedup: a round already exists for
// (stage, head) at sequence `of`. Every marker that does not name THIS dispatch
// — none, a stale Of, a wrong head, a wrong stage — dedups (no new round, no
// reviewer call). The valid marker dispatches ONE round stamped rerun_of; a
// second dispatch carrying the SAME marker then finds that new round newest and
// dedups, which is the in-lock re-verification that makes two concurrent
// re-runs start exactly one round.
//
// COUNTERFACTUALS: rerunBypassApplies returning true for any present marker →
// the stale/wrong arms dispatch → RED; dropping the newest==Of check → the
// second valid-marker dispatch starts a second round → RED; dropping the
// `!s.rerunBypassApplies(...)` term from the dedup guard → the valid arm
// dedups → RED.
func TestRunImplementReviewsForTree_RerunMarkerIsNarrow(t *testing.T) {
	reviewer := &fakePlanReviewer{verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictApprove}, model: "claude-opus-4-8"}
	s, _, au, _, runRow, implStage := newImplementReviewServer(t, reviewer, specImplementAdvisoryReviewers)
	au.stampSequence = true
	const head = "rerun-head"
	of := appendRerunTargetRound(t, au, runRow.ID, implStage.ID, head)
	diff := policy.Diff{ChangedFiles: []policy.ChangedFile{{Path: "backend/internal/foo/foo.go", Status: policy.StatusModified}}}
	traceCtx := withReviewRoundSource(context.Background(), reviewRoundSource{Origin: reviewRoundOriginTrace})
	reviewerCalls := func() int {
		reviewer.mu.Lock()
		defer reviewer.mu.Unlock()
		return len(reviewer.calls)
	}

	for _, tc := range []struct {
		name string
		ctx  context.Context
	}{
		{"no marker", traceCtx},
		{"stale Of", withReviewRerun(traceCtx, reviewRerun{Of: of - 1, StageID: implStage.ID, HeadSHA: head})},
		{"wrong head", withReviewRerun(traceCtx, reviewRerun{Of: of, StageID: implStage.ID, HeadSHA: "other-head"})},
		{"wrong stage", withReviewRerun(traceCtx, reviewRerun{Of: of, StageID: uuid.New(), HeadSHA: head})},
	} {
		s.runImplementReviewsForTree(tc.ctx, runRow.ID, implStage.ID, diff, nil, head, "", "", nil)
		s.waitBackgroundReviews()
		if got := startedPayloads(t, au, runRow.ID); len(got) != 1 {
			t.Fatalf("%s: started rounds = %d, want 1 (the same head must dedup)", tc.name, len(got))
		}
		if n := reviewerCalls(); n != 0 {
			t.Fatalf("%s: reviewer invocations = %d, want 0", tc.name, n)
		}
	}

	valid := withReviewRerun(traceCtx, reviewRerun{Of: of, StageID: implStage.ID, HeadSHA: head})
	s.runImplementReviewsForTree(valid, runRow.ID, implStage.ID, diff, nil, head, "", "", nil)
	s.waitBackgroundReviews()
	got := startedPayloads(t, au, runRow.ID)
	if len(got) != 2 {
		t.Fatalf("started rounds after the valid re-run = %d, want 2", len(got))
	}
	if got[1].RerunOf != of || got[1].HeadSHA != head || got[1].RedispatchOf != 0 {
		t.Errorf("re-run round = %+v, want rerun_of %d, head %q, no D6 lineage", got[1], of, head)
	}
	if n := reviewerCalls(); n != 1 {
		t.Errorf("reviewer invocations after the valid re-run = %d, want 1", n)
	}

	s.runImplementReviewsForTree(valid, runRow.ID, implStage.ID, diff, nil, head, "", "", nil)
	s.waitBackgroundReviews()
	if got := startedPayloads(t, au, runRow.ID); len(got) != 2 {
		t.Fatalf("started rounds after a second dispatch with the same marker = %d, want 2 (the marker is spent once a newer round exists)", len(got))
	}
}

// TestRunImplementReviewsForTree_RerunMarkerSkipsDiffSecretsOnlyWhenValid: the
// re-run round was seeded BY CONSTRUCTION without its secrets check, so a
// raised row is attributable to THIS dispatch. A valid marker skips the check
// (the re-run round's own dispatch ran it); a stale marker runs it like an
// ordinary round (and the round dedups).
//
// COUNTERFACTUAL: drop `&& !s.rerunBypassApplies(...)` from the secrets guard
// → the valid arm raises one row → RED; make rerunBypassApplies ignore Of →
// the stale arm raises nothing → RED.
func TestRunImplementReviewsForTree_RerunMarkerSkipsDiffSecretsOnlyWhenValid(t *testing.T) {
	for _, tc := range []struct {
		name     string
		stale    bool
		wantRows int
	}{
		{name: "valid marker skips the check", stale: false, wantRows: 0},
		{name: "stale marker runs the check", stale: true, wantRows: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reviewer := &fakePlanReviewer{verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictApprove}, model: "claude-opus-4-8"}
			s, _, au, _, runRow, implStage := newImplementReviewServer(t, reviewer, specImplementAdvisoryReviewers)
			au.stampSequence = true
			cr := newFakeConcernRepo()
			s.cfg.ConcernRepo = cr
			of := appendRerunTargetRound(t, au, runRow.ID, implStage.ID, "secret-head")
			if tc.stale {
				of--
			}
			ctx := withReviewRerun(withReviewRoundSource(context.Background(), reviewRoundSource{Origin: reviewRoundOriginTrace}),
				reviewRerun{Of: of, StageID: implStage.ID, HeadSHA: "secret-head"})
			s.runImplementReviewsForTree(ctx, runRow.ID, implStage.ID, keyDiff(), nil, "secret-head", "", "", nil)
			s.waitBackgroundReviews()
			if n := len(serverCheckRows(t, cr, runRow.ID)); n != tc.wantRows {
				t.Errorf("server_check rows = %d, want %d", n, tc.wantRows)
			}
		})
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
