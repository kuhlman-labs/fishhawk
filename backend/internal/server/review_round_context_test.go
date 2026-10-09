package server

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

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
