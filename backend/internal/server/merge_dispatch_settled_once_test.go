package server

import (
	"context"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/mergequeue"
)

// #4617 item 3: appendReconciledMergedOnce is the ONE writer of
// merge_dispatch_settled outcome reconciled_merged, deduped on (run, outcome)
// over the REAL Postgres audit chain. Each subtest counts committed rows by
// outcome, so the dedupe is the only thing that stops a second row.

// mdsOutcomes counts r's merge_dispatch_settled rows per outcome.
func (w *mqmWorld) mdsOutcomes(r *mcqRun) map[string]int {
	w.t.Helper()
	out := map[string]int{}
	for _, row := range w.rows(r, categoryMergeDispatchSettled) {
		o, _ := w.payload(row)["outcome"].(string)
		out[o]++
	}
	return out
}

func TestAppendReconciledMergedOnce(t *testing.T) {
	ctx := context.Background()
	setup := func(t *testing.T) (*mqmWorld, *mcqRun, mergequeue.DispatchClaim) {
		w := newMQMWorld(t)
		r := w.seed("once", "h1", []byte(mcvUndelegatedSpecYAML))
		return w, r, mergequeue.DispatchClaim{RunID: r.id, State: mergequeue.DispatchFailed, Path: mergeDispatchPathQueue, HeadSHA: "h1"}
	}

	t.Run("postgres_capability", func(t *testing.T) {
		w, r, claim := setup(t)
		w.srv.cfg.AuditRepo = w.mcqFixture.audit
		if _, ok := w.srv.cfg.AuditRepo.(audit.DedupedChainAppender); !ok {
			t.Fatal("fixture: the raw Postgres audit repository must implement DedupedChainAppender")
		}
		// A dispatch_failed row on the same category is not a duplicate.
		if err := w.srv.appendMergeDispatchSettledStrict(ctx, claim, string(mergeDispatchUncertain), "502"); err != nil {
			t.Fatalf("seed dispatch_failed row: %v", err)
		}
		for i := range 2 {
			if err := w.srv.appendReconciledMergedOnce(ctx, claim, "merged"); err != nil {
				t.Fatalf("call %d: %v, want nil (an already-recorded row is success)", i+1, err)
			}
		}
		got := w.mdsOutcomes(r)
		if got[mergeDispatchReconciledMerged] != 1 || got[string(mergeDispatchUncertain)] != 1 {
			t.Fatalf("merge_dispatch_settled outcomes = %v, want exactly one reconciled_merged and the one dispatch_failed", got)
		}
		var row *audit.Entry
		for _, e := range w.rows(r, categoryMergeDispatchSettled) {
			if w.payload(e)["outcome"] == mergeDispatchReconciledMerged {
				row = e
			}
		}
		p := w.payload(row)
		if p["path"] != mergeDispatchPathQueue || p["head_sha"] != "h1" || p["reason"] != "merged" ||
			row.ActorSubject == nil || *row.ActorSubject != mergeCandidateSystemSubject {
			t.Fatalf("reconciled_merged row = %v (actor %v), want path queue, head h1, reason merged, the system actor", p, row.ActorSubject)
		}
	})

	t.Run("fallback_non_capable", func(t *testing.T) {
		w, r, claim := setup(t)
		if _, ok := w.srv.cfg.AuditRepo.(audit.DedupedChainAppender); ok {
			t.Fatal("fixture: mqmAudit must not expose DedupedChainAppender (it embeds the Repository interface)")
		}
		for i := range 2 {
			if err := w.srv.appendReconciledMergedOnce(ctx, claim, "merged"); err != nil {
				t.Fatalf("call %d: %v, want nil", i+1, err)
			}
		}
		if got := w.mdsOutcomes(r); got[mergeDispatchReconciledMerged] != 1 {
			t.Fatalf("merge_dispatch_settled outcomes = %v, want exactly one reconciled_merged", got)
		}
	})

	t.Run("fallback_list_error_proceeds", func(t *testing.T) {
		w, r, claim := setup(t)
		w.srv.cfg.AuditRepo = mqrListErrAudit{Repository: w.mcqFixture.audit, fail: categoryMergeDispatchSettled}
		if err := w.srv.appendReconciledMergedOnce(ctx, claim, "merged"); err != nil {
			t.Fatalf("append with a failing list = %v, want nil (WARN and proceed)", err)
		}
		if got := w.mdsOutcomes(r); got[mergeDispatchReconciledMerged] != 1 {
			t.Fatalf("merge_dispatch_settled outcomes = %v, want the one reconciled_merged row appended", got)
		}
	})

	t.Run("no_audit_repository", func(t *testing.T) {
		w, _, claim := setup(t)
		w.srv.cfg.AuditRepo = nil
		if err := w.srv.appendReconciledMergedOnce(ctx, claim, "merged"); err == nil {
			t.Fatal("append with no audit repository = nil, want an error")
		}
	})

	t.Run("append_error_surfaces", func(t *testing.T) {
		w, r, claim := setup(t)
		w.audit.failCategory = categoryMergeDispatchSettled
		if err := w.srv.appendReconciledMergedOnce(ctx, claim, "merged"); err == nil {
			t.Fatal("append failure = nil, want the append error")
		}
		if got := w.mdsOutcomes(r); len(got) != 0 {
			t.Fatalf("merge_dispatch_settled outcomes = %v, want none", got)
		}
	})
}
