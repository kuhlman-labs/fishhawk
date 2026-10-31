package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/auditcheckpublisher"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/mergequeue"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/timescale"
)

// #4617 (approval condition (f)): every forge call on the inline and delegated
// merge tails is abandoned at its own package-var bound, well under the 90s
// mergequeue.IntentFnTimeout. Each slow fake blocks until its OWN context is
// done (blockOnCtx) and records the deadline it was handed; with the bound
// shortened it sees context.DeadlineExceeded promptly. Without the bound its
// context carries only the lock fn's 90s deadline (or none on a direct call),
// so the timescale.D(5s) escape fires first and returns errSlowForgeEscape:
// replacing a bound's context.WithTimeout with context.WithCancel fails the
// DeadlineExceeded and entry-deadline assertions. No test here calls
// t.Parallel: they assign package vars (restored in t.Cleanup).

// errSlowForgeEscape is what a slow fake returns when its context never ended.
var errSlowForgeEscape = errors.New("slow forge fake: the escape fired before any context deadline")

// blockOutcome is what one blocked call saw: the error it returned and how
// long its context had left at entry (ok=false when it carried no deadline).
type blockOutcome struct {
	err       error
	remaining time.Duration
	ok        bool
}

// blockOnCtx blocks until ctx is done or the timescale.D(5s) escape fires.
func blockOnCtx(ctx context.Context) blockOutcome {
	out := blockOutcome{}
	if dl, ok := ctx.Deadline(); ok {
		out.remaining, out.ok = time.Until(dl), true
	}
	select {
	case <-ctx.Done():
		out.err = ctx.Err()
	case <-time.After(timescale.D(5 * time.Second)):
		out.err = errSlowForgeEscape
	}
	return out
}

// shortenBound sets *v to timescale.D(200ms) for the test and restores it.
func shortenBound(t *testing.T, v *time.Duration) time.Duration {
	t.Helper()
	orig := *v
	*v = timescale.D(200 * time.Millisecond)
	t.Cleanup(func() { *v = orig })
	return *v
}

// requireBounded asserts a blocked call was abandoned at bound: it saw
// context.DeadlineExceeded and entered with no more than bound left.
func requireBounded(t *testing.T, what string, got []blockOutcome, bound time.Duration) {
	t.Helper()
	if len(got) == 0 {
		t.Fatalf("%s: never called", what)
	}
	o := got[len(got)-1]
	if !errors.Is(o.err, context.DeadlineExceeded) {
		t.Fatalf("%s: saw %v, want context.DeadlineExceeded at the shortened bound", what, o.err)
	}
	if !o.ok || o.remaining > bound {
		t.Fatalf("%s: entry deadline remaining %v (deadline=%v), want <= the shortened bound %v", what, o.remaining, o.ok, bound)
	}
}

// slowMerger is a GitHubMerger blocking on its context (blockOnCtx).
type slowMerger struct {
	mu   sync.Mutex
	n    int
	seen []blockOutcome
}

func (m *slowMerger) MergePullRequest(ctx context.Context, _ *run.Run, _ string) error {
	got := blockOnCtx(ctx)
	m.mu.Lock()
	m.n++
	m.seen = append(m.seen, got)
	m.mu.Unlock()
	return got.err
}

func (m *slowMerger) outcomes() []blockOutcome {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]blockOutcome(nil), m.seen...)
}

// TestMergeTail_SlowMergerBoundedByCallTimeout pins dispatchMergeBounded: a
// hung merge dispatch is abandoned at mergeTailMergeCallTimeout and FAILS the
// merge (inline 502 merge_dispatch_failed, delegated "merge dispatch failed"),
// the claim dispatch_failed (a deadline is uncertain).
func TestMergeTail_SlowMergerBoundedByCallTimeout(t *testing.T) {
	t.Run("inline", func(t *testing.T) {
		bound := shortenBound(t, &mergeTailMergeCallTimeout)
		w, _ := newMQEWorld(t)
		r := w.verified("slow", "h1")
		m := &slowMerger{}
		w.srv.cfg.GateMerger = m
		rec := w.mergeRun(r)
		if rec.Code != http.StatusBadGateway || mqeCode(t, rec.Body.Bytes()) != "merge_dispatch_failed" {
			t.Fatalf("merge_run = %d %s, want 502 merge_dispatch_failed", rec.Code, rec.Body.String())
		}
		requireBounded(t, "inline merge dispatch", m.outcomes(), bound)
		w.requireClaim(r, mergequeue.DispatchFailed)
	})
	t.Run("delegated", func(t *testing.T) {
		bound := shortenBound(t, &mergeTailMergeCallTimeout)
		w := newMQMWorld(t)
		r := w.admqReady("slowd", "h1")
		var mu sync.Mutex
		var seen []blockOutcome
		m := &admqMerger{hook: func(ctx context.Context, _ *run.Run, _ string) error {
			got := blockOnCtx(ctx)
			mu.Lock()
			seen = append(seen, got)
			mu.Unlock()
			return got.err
		}}
		out, err := w.admqDriveErr(r, m)
		if err == nil || out.Acted || out.Note != "merge dispatch failed" {
			t.Fatalf("outcome = %+v err %v, want the dispatch failure surfaced", out, err)
		}
		mu.Lock()
		got := append([]blockOutcome(nil), seen...)
		mu.Unlock()
		requireBounded(t, "delegated merge dispatch", got, bound)
		w.requireClaim(r, mergequeue.DispatchFailed)
	})
}

// TestMergeTail_SlowObserveReadFallsThroughAndDispatches pins
// observeForgeMergeBounded: a hung observe-rung forge read is abandoned at
// mergeTailForgeReadTimeout and FALLS THROUGH (Landed:false), so the resumed
// inline merge dispatches normally; the delegated landed read is false.
func TestMergeTail_SlowObserveReadFallsThroughAndDispatches(t *testing.T) {
	w, m := newMQEWorld(t)
	r := w.verified("slowread", "h1")
	// A first post the forge refuses checks_pending records the verdict and
	// releases the claim, so the next post is a resume that reads the forge.
	m.mu.Lock()
	m.inlineErr = forge.ErrPullRequestUnstableStatus
	m.mu.Unlock()
	if rec := w.mergeRun(r); rec.Code != http.StatusConflict || mqeCode(t, rec.Body.Bytes()) != "merge_checks_pending" {
		t.Fatalf("first merge_run = %d %s, want 409 merge_checks_pending", rec.Code, rec.Body.String())
	}
	w.requireClaim(r, "")
	m.mu.Lock()
	m.inlineErr = nil
	m.mu.Unlock()

	bound := shortenBound(t, &mergeTailForgeReadTimeout)
	reader := &admqPRReader{block: true}
	w.srv.cfg.PRStateReader = reader
	rec := w.mergeRun(r)
	if rec.Code != http.StatusOK {
		t.Fatalf("resumed merge_run = %d %s, want 200", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.String(), err)
	}
	if body["merge_queued"] != true {
		t.Fatalf("merge_queued = %v, want true", body["merge_queued"])
	}
	requireBounded(t, "inline observe-rung forge read", reader.outcomes(), bound)
	if m.inlineCalls() != 2 {
		t.Fatalf("inline dispatches = %d, want 2 (one more after the hung read)", m.inlineCalls())
	}
	w.requireClaim(r, mergequeue.DispatchDispatched)

	t.Run("delegated_landed_read", func(t *testing.T) {
		runRow, err := w.repo.GetRun(context.Background(), r.id)
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		before := len(reader.outcomes())
		if w.srv.delegatedMergeLanded(context.Background(), runRow) {
			t.Fatal("delegatedMergeLanded = true, want false on a hung read")
		}
		got := reader.outcomes()
		if len(got) != before+1 {
			t.Fatalf("landed reads = %d, want 1", len(got)-before)
		}
		requireBounded(t, "delegated landed read", got, bound)
	})
}

// slowPublishGitHub is a CheckRunCreator blocking on its context.
type slowPublishGitHub struct {
	mu   sync.Mutex
	seen []blockOutcome
}

func (g *slowPublishGitHub) CreateCheckRun(ctx context.Context, _ forge.CredentialScope, _ forge.RepoRef, _ forge.CreateCheckRunParams) (*forge.CreateCheckRunResult, error) {
	got := blockOnCtx(ctx)
	g.mu.Lock()
	g.seen = append(g.seen, got)
	g.mu.Unlock()
	return nil, got.err
}

func (g *slowPublishGitHub) outcomes() []blockOutcome {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]blockOutcome(nil), g.seen...)
}

// withSlowPublisher rewires s's audit-check publisher onto a blocking creator.
func withSlowPublisher(s *Server) *slowPublishGitHub {
	g := &slowPublishGitHub{}
	s.auditCheckPublisher = auditcheckpublisher.New(auditcheckpublisher.Deps{
		GitHub:      g,
		Runs:        s.cfg.RunRepo,
		Artifacts:   s.cfg.ArtifactRepo,
		Audit:       s.cfg.AuditRepo,
		ExternalURL: "https://app.fishhawk.example.com",
	})
	return g
}

// TestMergeTail_SlowRepublishBoundedAndDispatches pins the republish bound
// (republishAuditCheckBeforeMergeAtHead): a hung pre-dispatch publish is
// abandoned at mergeTailRepublishTimeout and stays best-effort, so the merge
// dispatches once after the publish attempt.
func TestMergeTail_SlowRepublishBoundedAndDispatches(t *testing.T) {
	t.Run("inline", func(t *testing.T) {
		bound := shortenBound(t, &mergeTailRepublishTimeout)
		log := &mergeOrderLog{}
		s, r, merger := newMergeRepublishServer(t, log, nil, nil)
		g := withSlowPublisher(s)
		rec := postMergeRun(t, s, r.ID, mergeRunRequest{Verdict: "ship it"}, withMergeOperator)
		if rec.Code != http.StatusOK {
			t.Fatalf("merge_run = %d %s, want 200", rec.Code, rec.Body.String())
		}
		requireBounded(t, "inline republish", g.outcomes(), bound)
		if merger.called != 1 {
			t.Fatalf("merges = %d, want 1 after the publish attempt", merger.called)
		}
	})
	t.Run("delegated", func(t *testing.T) {
		bound := shortenBound(t, &mergeTailRepublishTimeout)
		log := &mergeOrderLog{}
		s, r, stages := newGatedMergeRepublishServer(t, log, nil)
		g := withSlowPublisher(s)
		merger := &orderedLogMerger{log: log}
		outcome, _, err := s.dispatchAcceptanceGatedMerge(context.Background(), r, stages, merger, "")
		if outcome != mergeDispatchMerged || err != nil {
			t.Fatalf("dispatch = %v err %v, want mergeDispatchMerged with no error", outcome, err)
		}
		requireBounded(t, "delegated republish", g.outcomes(), bound)
		if merger.called != 1 {
			t.Fatalf("merges = %d, want 1", merger.called)
		}
	})
}

// TestDispatchAcceptanceGatedMerge_NoLedgerObserveBeforeSettle pins that the
// delegated observe-before-settle carries NO claimed conjunct (#4617): with no
// merge queue wired there is no ledger (s.mergeDispatchLedger() == nil), yet
// a dispatch error whose merge landed (a pr_merged row lands inside the
// dispatch) still reports mergeDispatchLandedOnReobserve with no error.
// Counterfactual: adding `&& claimed` to the guard returns
// (mergeDispatchMerged, the error), RED. An unmerged PR keeps the error.
func TestDispatchAcceptanceGatedMerge_NoLedgerObserveBeforeSettle(t *testing.T) {
	for _, tc := range []struct {
		name    string
		land    bool
		outcome mergeDispatchOutcome
		wantErr bool
	}{
		{"landed", true, mergeDispatchLandedOnReobserve, false},
		{"not_landed", false, mergeDispatchMerged, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, r, stages := newGatedMergeRepublishServer(t, &mergeOrderLog{}, nil)
			if s.mergeDispatchLedger() != nil {
				t.Fatal("fixture: no merge-dispatch ledger may be wired")
			}
			m := &admqMerger{hook: func(ctx context.Context, runRow *run.Run, _ string) error {
				if tc.land {
					appendPRMergedRow(t, s, runRow)
				}
				return errors.New("forge 502")
			}}
			outcome, settled, err := dispatchSettled(s, r, stages, m)
			if outcome != tc.outcome || settled != "" || (err != nil) != tc.wantErr {
				t.Fatalf("dispatch = (%v, settled %q, err %v), want (%v, \"\", err=%v)", outcome, settled, err, tc.outcome, tc.wantErr)
			}
			if m.calls() != 1 {
				t.Fatalf("merger calls = %d, want 1", m.calls())
			}
		})
	}
}

// dispatchSettled runs the delegated tail and returns its outcome, settle and
// error.
func dispatchSettled(s *Server, r *run.Run, stages []*run.Stage, m GitHubMerger) (mergeDispatchOutcome, mergeDispatchLedgerOutcome, error) {
	outcome, _, settled, err := s.dispatchAcceptanceGatedMergeSettled(context.Background(), r, stages, m, "")
	return outcome, settled, err
}

// appendPRMergedRow lands a pr_merged row on runRow's chain BY CONSTRUCTION.
func appendPRMergedRow(t *testing.T, s *Server, runRow *run.Run) {
	t.Helper()
	kind, subj := audit.ActorSystem, "test"
	if _, err := s.cfg.AuditRepo.AppendChained(context.Background(), audit.ChainAppendParams{RunID: runRow.ID, Timestamp: time.Now().UTC(),
		Category: CategoryPRMerged, ActorKind: &kind, ActorSubject: &subj, Payload: []byte(`{}`)}); err != nil {
		t.Errorf("seed pr_merged: %v", err)
	}
}

// TestMergeRun_ReconciledMergedOnceAcrossQueueAndInline pins the ONE deduped
// reconciled_merged writer across the queue and inline paths (#4617): a
// dispatch_failed queue claim whose PR merged is reconciled by merge-on-pass
// (one row), then the inline claimInlineMergeDispatch meets the same claim,
// answers already_merged and appends NO second row. Counterfactual: routing
// the inline site back to appendMergeDispatchSettled lands two rows, RED.
func TestMergeRun_ReconciledMergedOnceAcrossQueueAndInline(t *testing.T) {
	ctx := context.Background()
	w := newMQMWorld(t)
	r := w.seed("qi", "h1", []byte(mcvUndelegatedSpecYAML))
	if _, err := w.store.ClaimMergeDispatch(ctx, r.id, mergeDispatchPathQueue, "h1"); err != nil {
		t.Fatalf("seed claim: %v", err)
	}
	if err := w.store.MarkMergeDispatchFailed(ctx, r.id, "github: 502 bad gateway"); err != nil {
		t.Fatalf("seed dispatch_failed: %v", err)
	}
	w.gh.set(func() { w.gh.prs[r.pr].merged, w.gh.prs[r.pr].state = true, "closed" })
	if ok, reason, detail := w.srv.mergeOnPassReconcileMerged(ctx, w.srv.mergeDispatchLedger(), r.id, "h1"); !ok {
		t.Fatalf("merge-on-pass reconcile = %s (%s), want ok", reason, detail)
	}
	if got := w.mdsOutcomes(r); got[mergeDispatchReconciledMerged] != 1 {
		t.Fatalf("after merge-on-pass: outcomes = %v, want one reconciled_merged", got)
	}

	w.srv.cfg.PRStateReader = &fakePRStateReader{pr: mergedPR()}
	runRow, err := w.repo.GetRun(ctx, r.id)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v0/runs/"+r.id.String()+"/merge", nil)
	_, claimed, handled := w.srv.claimInlineMergeDispatch(rec, req, r.id, runRow, "", "operator", 1, true, mergeCandidateGateResult{HeadSHA: "h1"})
	if claimed || !handled {
		t.Fatalf("claimInlineMergeDispatch = claimed %v handled %v, want the already-merged answer", claimed, handled)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.String(), err)
	}
	if rec.Code != http.StatusOK || body["already_merged"] != true {
		t.Fatalf("inline answer = %d %s, want 200 already_merged", rec.Code, rec.Body.String())
	}
	if got := w.mdsOutcomes(r); got[mergeDispatchReconciledMerged] != 1 {
		t.Fatalf("after the inline reconcile: outcomes = %v, want still exactly one reconciled_merged", got)
	}
	w.requireClaim(r, mergequeue.DispatchFailed)
}

// TestMergeTail_EmptyGateHeadClaimsUnresolved pins that both non-queue claim
// sites record mergeDispatchClaimHeadUnresolved for an empty gate head
// (#4617), the exact value mergeDispatchClaimNamesHead reconciles against any
// merged head. Counterfactual: either site claiming at any other value is
// RED.
func TestMergeTail_EmptyGateHeadClaimsUnresolved(t *testing.T) {
	ctx := context.Background()
	t.Run("inline", func(t *testing.T) {
		w := newMQMWorld(t)
		r := w.seed("inl", "h1", []byte(mcvUndelegatedSpecYAML))
		runRow, err := w.repo.GetRun(ctx, r.id)
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v0/runs/"+r.id.String()+"/merge", nil)
		if _, claimed, handled := w.srv.claimInlineMergeDispatch(rec, req, r.id, runRow, "", "operator", 1, false, mergeCandidateGateResult{}); !claimed || handled {
			t.Fatalf("claimInlineMergeDispatch = claimed %v handled %v (%d %s), want a fresh claim", claimed, handled, rec.Code, rec.Body.String())
		}
		var head string
		if st, path := w.claimRow(r, &head); st != string(mergequeue.DispatchDispatched) || path != mergequeue.DispatchPathInline || head != mergeDispatchClaimHeadUnresolved {
			t.Fatalf("claim = %s/%s@%q, want dispatched/inline@%q", st, path, head, mergeDispatchClaimHeadUnresolved)
		}
	})
	t.Run("delegated", func(t *testing.T) {
		w := newMQMWorld(t)
		r := w.admqReady("del", "h1")
		runRow, err := w.repo.GetRun(ctx, r.id)
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		stages, err := w.repo.ListStagesForRun(ctx, r.id)
		if err != nil {
			t.Fatalf("ListStagesForRun: %v", err)
		}
		m := &admqMerger{}
		if outcome, _, err := w.srv.dispatchAcceptanceGatedMerge(ctx, runRow, stages, m, ""); outcome != mergeDispatchMerged || err != nil || m.calls() != 1 {
			t.Fatalf("dispatch = %v err %v calls %d, want one clean unpinned dispatch", outcome, err, m.calls())
		}
		var head string
		if st, path := w.claimRow(r, &head); st != string(mergequeue.DispatchDispatched) || path != mergequeue.DispatchPathDelegated || head != mergeDispatchClaimHeadUnresolved {
			t.Fatalf("claim = %s/%s@%q, want dispatched/delegated@%q", st, path, head, mergeDispatchClaimHeadUnresolved)
		}
	})
}

// TestDelegatedMergeLanded_NoAuditRepoSkipsChainTier pins the nil-AuditRepo
// guard: with no audit repository the chain tier is skipped (never a nil
// dereference) and, with no forge reader either, the answer is not landed.
// Counterfactual: deleting the guard panics, RED.
func TestDelegatedMergeLanded_NoAuditRepoSkipsChainTier(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0"})
	if s.delegatedMergeLanded(context.Background(), &run.Run{ID: uuid.New()}) {
		t.Fatal("delegatedMergeLanded = true with no audit repository and no forge reader, want false")
	}
}
