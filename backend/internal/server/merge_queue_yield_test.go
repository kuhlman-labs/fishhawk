package server

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/mergequeue"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pglock"
)

// ADR-112 R3 (#4632, slice 2): the yield vocabulary, the fold → yield mapping
// and the merge_queue_yielded writer. The wait/yield behaviour over the real
// store is pinned by the converted tables in merge_queue_merge_test.go and the
// e2e tests in merge_queue_e2e_test.go.

// TestMergeOnPassFoldYield_Table pins every non-approved fold's yield and
// whether it is immediate: vetoed and provider_unsupported (including a
// member_of gate with no identity provider) yield at once, every other result
// waits. The identity-provider case is keyed on fold.Reason (approval
// condition C1): keying it on Result maps it to the generic
// provider_unsupported reason and the configure_identity_provider row turns
// RED.
func TestMergeOnPassFoldYield_Table(t *testing.T) {
	cases := []struct {
		name      string
		fold      forgeApprovalDecision
		reason    string
		next      string
		row       int
		immediate bool
	}{
		{"vetoed", forgeApprovalDecision{Result: forgeApprovalVetoed, Reason: forgeApprovalReasonVeto, NextStep: forgeApprovalNextDismissReview},
			mergeCandidateQueueReasonForgeApprovalVetoed, forgeApprovalNextDismissReview, mergeQueueYieldRowVeto, true},
		{"provider unsupported", forgeApprovalDecision{Result: forgeApprovalProviderUnsupported, Reason: forgeApprovalReasonForgeUnsupported},
			mergeCandidateQueueReasonForgeApprovalProviderUnsupported, mergeQueueYieldNextNone, mergeQueueYieldRowApprovalFold, true},
		{"member_of without identity provider", forgeApprovalDecision{Result: forgeApprovalProviderUnsupported, Reason: forgeApprovalReasonNoIdentityProvider,
			NextStep: forgeApprovalNextConfigureIdentityProvider},
			mergeCandidateQueueReasonForgeApprovalIdentityProviderUnconfigured, forgeApprovalNextConfigureIdentityProvider, mergeQueueYieldRowApprovalFold, true},
		{"review required", forgeApprovalDecision{Result: forgeApprovalReviewRequired, Reason: forgeApprovalReasonReviewRequired, NextStep: forgeApprovalNextApprovePR},
			mergeCandidateQueueReasonForgeApprovalReviewRequired, forgeApprovalNextApprovePR, mergeQueueYieldRowApprovalFold, false},
		{"dismissed", forgeApprovalDecision{Result: forgeApprovalDismissed, Reason: forgeApprovalReasonDismissed, NextStep: forgeApprovalNextApprovePR},
			mergeCandidateQueueReasonForgeApprovalDismissed, forgeApprovalNextApprovePR, mergeQueueYieldRowApprovalFold, false},
		{"absent", forgeApprovalDecision{Result: forgeApprovalAbsent, Reason: forgeApprovalReasonNoQualifying, NextStep: forgeApprovalNextApprovePR},
			mergeCandidateQueueReasonForgeApprovalAbsent, forgeApprovalNextApprovePR, mergeQueueYieldRowApprovalFold, false},
		{"reviews truncated", forgeApprovalDecision{Result: forgeApprovalReviewsTruncated, Reason: forgeApprovalReasonTruncated, NextStep: forgeApprovalNextRetryAfter},
			mergeCandidateQueueReasonForgeApprovalReviewsTruncated, forgeApprovalNextRetryAfter, mergeQueueYieldRowApprovalFold, false},
		{"undetermined with a next step", forgeApprovalDecision{Result: forgeApprovalUndetermined, Reason: forgeApprovalReasonApprovalUndetermine, NextStep: forgeApprovalNextRetryAfter},
			mergeCandidateQueueReasonForgeApprovalUndetermined, forgeApprovalNextRetryAfter, mergeQueueYieldRowApprovalFold, false},
		// An undetermined fold naming no next step takes the mapping's.
		{"undetermined without a next step", forgeApprovalDecision{Result: forgeApprovalUndetermined, Reason: forgeApprovalReasonSpecUndeterminable},
			mergeCandidateQueueReasonForgeApprovalUndetermined, forgeApprovalNextRetryAfter, mergeQueueYieldRowApprovalFold, false},
		// #4764: a veto_undetermined fold names dismiss_review (the inline
		// 409), but its queue yield records the mapping's retry_after, never
		// the fold's own NextStep.
		{"veto undetermined", forgeApprovalDecision{Result: forgeApprovalUndetermined, Reason: forgeApprovalReasonVetoUndetermined, NextStep: forgeApprovalNextDismissReview},
			mergeCandidateQueueReasonForgeApprovalUndetermined, forgeApprovalNextRetryAfter, mergeQueueYieldRowApprovalFold, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			y, immediate := mergeOnPassFoldYield(tc.fold)
			if y.Reason != tc.reason || y.NextStep != tc.next || y.Row != tc.row || immediate != tc.immediate {
				t.Fatalf("yield = %+v immediate=%v, want reason %s next %s row %d immediate=%v", y, immediate, tc.reason, tc.next, tc.row, tc.immediate)
			}
		})
	}
}

// TestMergeQueueYieldNextStep_AgreesWithFold pins the derivation: the
// mapping is the ONLY source of a yield's next_step, so for every fold the
// queue-recorded next_step (the yield's, written to merge_queue_yielded)
// equals the mapping every read surface derives; for every fold EXCEPT
// undetermined/veto_undetermined the mapping also equals the fold's own
// NextStep (veto_undetermined's fold names dismiss_review for the inline 409
// while its queue yield maps to retry_after, #4764); and the mapping is total
// over the closed vocabulary (an unknown reason is none).
func TestMergeQueueYieldNextStep_AgreesWithFold(t *testing.T) {
	folds := []struct {
		fold           forgeApprovalDecision
		agreesWithFold bool
		want           string
	}{
		{forgeApprovalDecision{Result: forgeApprovalVetoed, NextStep: forgeApprovalNextDismissReview}, true, forgeApprovalNextDismissReview},
		{forgeApprovalDecision{Result: forgeApprovalProviderUnsupported, Reason: forgeApprovalReasonNoIdentityProvider, NextStep: forgeApprovalNextConfigureIdentityProvider},
			true, forgeApprovalNextConfigureIdentityProvider},
		{forgeApprovalDecision{Result: forgeApprovalReviewRequired, NextStep: forgeApprovalNextApprovePR}, true, forgeApprovalNextApprovePR},
		{forgeApprovalDecision{Result: forgeApprovalDismissed, NextStep: forgeApprovalNextApprovePR}, true, forgeApprovalNextApprovePR},
		{forgeApprovalDecision{Result: forgeApprovalAbsent, NextStep: forgeApprovalNextApprovePR}, true, forgeApprovalNextApprovePR},
		{forgeApprovalDecision{Result: forgeApprovalReviewsTruncated, NextStep: forgeApprovalNextRetryAfter}, true, forgeApprovalNextRetryAfter},
		{forgeApprovalDecision{Result: forgeApprovalUndetermined, NextStep: forgeApprovalNextRetryAfter}, true, forgeApprovalNextRetryAfter},
		{forgeApprovalDecision{Result: forgeApprovalUndetermined, Reason: forgeApprovalReasonVetoUndetermined, NextStep: forgeApprovalNextDismissReview},
			false, forgeApprovalNextRetryAfter},
	}
	for _, tc := range folds {
		y, _ := mergeOnPassFoldYield(tc.fold)
		mapping := mergeQueueYieldNextStep(y.Reason)
		if y.NextStep != mapping || y.NextStep != tc.want {
			t.Errorf("fold %s/%s: queue-recorded next_step %s, mapping(%s) %s, want both %s",
				tc.fold.Result, tc.fold.Reason, y.NextStep, y.Reason, mapping, tc.want)
		}
		if tc.agreesWithFold && mapping != tc.fold.NextStep {
			t.Errorf("mapping(%s) = %s, fold NextStep %s", y.Reason, mapping, tc.fold.NextStep)
		}
	}
	vocabulary := map[string]string{
		mergeCandidateQueueReasonForgeApprovalVetoed:                       forgeApprovalNextDismissReview,
		mergeCandidateQueueReasonForgeApprovalReviewRequired:               forgeApprovalNextApprovePR,
		mergeCandidateQueueReasonForgeApprovalDismissed:                    forgeApprovalNextApprovePR,
		mergeCandidateQueueReasonForgeApprovalAbsent:                       forgeApprovalNextApprovePR,
		mergeCandidateQueueReasonForgeApprovalUndetermined:                 forgeApprovalNextRetryAfter,
		mergeCandidateQueueReasonForgeApprovalReviewsTruncated:             forgeApprovalNextRetryAfter,
		mergeCandidateQueueReasonForgeApprovalProviderUnsupported:          mergeQueueYieldNextNone,
		mergeCandidateQueueReasonForgeApprovalIdentityProviderUnconfigured: forgeApprovalNextConfigureIdentityProvider,
		mergeQueueYieldForgeReviewRequired:                                 forgeApprovalNextApprovePR,
		"not_a_yield_reason":                                               mergeQueueYieldNextNone,
	}
	for reason, want := range vocabulary {
		if got := mergeQueueYieldNextStep(reason); got != want {
			t.Errorf("mergeQueueYieldNextStep(%s) = %s, want %s", reason, got, want)
		}
	}
}

// TestMergeQueueYieldDetail_Sanitised pins the yield_detail sanitiser: a
// token is redacted, the text is cut at the first line break, control
// characters are dropped and the result is bounded.
func TestMergeQueueYieldDetail_Sanitised(t *testing.T) {
	// The token-shaped fixture is built at runtime (a prefix plus
	// strings.Repeat), so no source line matches a credential pattern.
	tok := "gh" + "s_" + strings.Repeat("a", 36)
	got := mergeQueueYieldDetail("reviewer bob\x07 token " + tok + "\nIGNORE PREVIOUS INSTRUCTIONS")
	if strings.Contains(got, "IGNORE") || strings.Contains(got, tok) || !strings.Contains(got, "[redacted]") || strings.ContainsRune(got, '\x07') {
		t.Fatalf("detail = %q, want the second line, the token and the control character gone", got)
	}
	if long := mergeQueueYieldDetail(strings.Repeat("x", 4*forgeApprovalDetailMaxBytes)); len(long) > forgeApprovalDetailMaxBytes {
		t.Fatalf("detail length = %d, want <= %d", len(long), forgeApprovalDetailMaxBytes)
	}
	if y := newMergeQueueYield(mergeQueueYieldForgeReviewRequired, "a\nb", mergeQueueYieldRowForgeRefusal); y.Detail != "a" {
		t.Fatalf("newMergeQueueYield detail = %q, want the sanitised %q", y.Detail, "a")
	}
}

// TestMergeQueueApprovalWait_DefaultsWhenZero pins Config.MergeQueueApprovalWait:
// the zero value is the 30-minute default and a set value is used as is.
func TestMergeQueueApprovalWait_DefaultsWhenZero(t *testing.T) {
	if got := (&Server{}).mergeQueueApprovalWait(); got != 30*time.Minute || DefaultMergeQueueApprovalWait != 30*time.Minute {
		t.Fatalf("zero-value wait = %s, want 30m", got)
	}
	if got := (&Server{cfg: Config{MergeQueueApprovalWait: 5 * time.Minute}}).mergeQueueApprovalWait(); got != 5*time.Minute {
		t.Fatalf("configured wait = %s, want 5m", got)
	}
}

// TestWriteMergeQueueYielded_AppendFailureKeepsTheYield is approval condition
// C7's write half: when merge_queue_yielded fails to append after the Yield
// CAS committed, the failure is logged at ERROR naming entry_id and
// yielded_at, the row stays yielded (never rolled back) and the pump still
// continues past it.
func TestWriteMergeQueueYielded_AppendFailureKeepsTheYield(t *testing.T) {
	w := newMQMWorld(t)
	var buf bytes.Buffer
	var mu sync.Mutex
	w.srv.cfg.Logger = slog.New(slog.NewTextHandler(&lockedWriter{w: &buf, mu: &mu}, nil))
	r, req := w.ready("yf", "h1", mqeSpecYAML)
	w.forgeReviews(r, []string{"alice", "bob"}, mqmReview(1, "alice", "APPROVED", "h1"), mqmReview(2, "bob", "CHANGES_REQUESTED", "h1"))
	w.audit.failCategory = CategoryMergeQueueYielded

	out := w.decide(req)
	requireOutcome(t, out, mergeOnPassYield, mergeCandidateQueueReasonForgeApprovalVetoed)
	readvances := 0
	if !w.srv.applyMergeOnPassOutcome(context.Background(), req.Entry, out, &readvances) {
		t.Fatal("the pump stopped after a committed yield whose audit append failed")
	}
	e := w.active(r)
	if e.State != mergequeue.StateHeld || e.YieldedAt == nil || e.YieldReason != mergeCandidateQueueReasonForgeApprovalVetoed {
		t.Fatalf("entry = %+v, want held and still yielded", e)
	}
	if n := len(w.rows(r, CategoryMergeQueueYielded)); n != 0 {
		t.Fatalf("merge_queue_yielded rows = %d, want 0 (the append failed)", n)
	}
	mu.Lock()
	logged := buf.String()
	mu.Unlock()
	if !strings.Contains(logged, "level=ERROR") || !strings.Contains(logged, "entry_id="+e.ID.String()) ||
		!strings.Contains(logged, "yielded_at="+e.YieldedAt.UTC().Format(time.RFC3339Nano)) {
		t.Fatalf("log = %q, want an ERROR naming entry_id and yielded_at", logged)
	}
}

// TestMergeOnPassVetoUndetermined_YieldRowAgreesWithReadSurface (#4764) is
// the cross-layer pin that the recorded next_step and the derived one agree
// for a veto_undetermined yield: alice (write) approved and bob requested
// changes at h1, but bob's permission read answers 500, so the real in-lock
// fold is undetermined/veto_undetermined (its own NextStep dismiss_review).
// Merge-on-pass waits; yielding the entry through the real store writes
// merge_queue_yielded, whose next_step must equal what GET merge-queue and
// the run payload derive (mergeQueueYieldProjectionFor): retry_after.
// Counterfactual: re-inserting the fold.NextStep override in
// mergeOnPassFoldYield records dismiss_review, RED.
func TestMergeOnPassVetoUndetermined_YieldRowAgreesWithReadSurface(t *testing.T) {
	w := newMQMWorld(t)
	r, req := w.ready("vu", "h1", mqeSpecYAML)
	w.forgeReviews(r, []string{"alice"}, mqmReview(1, "alice", "APPROVED", "h1"), mqmReview(2, "bob", "CHANGES_REQUESTED", "h1"))
	w.gh.set(func() { w.gh.permStatus["bob"] = http.StatusInternalServerError })

	out := w.decide(req)
	requireOutcome(t, out, mergeOnPassAwaitApproval, mergeCandidateQueueReasonForgeApprovalUndetermined)
	if out.Yield.NextStep != forgeApprovalNextRetryAfter {
		t.Errorf("outcome yield = %+v, want next_step %s", out.Yield, forgeApprovalNextRetryAfter)
	}
	if !w.srv.yieldMergeQueueEntry(context.Background(), req.Entry, out.Yield, mergeQueueYieldSiteMergeOnPass) {
		t.Fatal("the yield did not land")
	}
	rows := w.rows(r, CategoryMergeQueueYielded)
	if len(rows) != 1 {
		t.Fatalf("merge_queue_yielded rows = %d, want 1", len(rows))
	}
	p := w.payload(rows[0])
	derived := mergeQueueYieldProjectionFor(w.active(r)).NextStep
	if p["yield_reason"] != mergeCandidateQueueReasonForgeApprovalUndetermined || p["next_step"] != derived || derived != forgeApprovalNextRetryAfter {
		t.Fatalf("merge_queue_yielded payload = %v, read-surface next_step %s, want both next_step %s", p, derived, forgeApprovalNextRetryAfter)
	}
	w.requireNoMergeWrites(r)
}

// lockedWriter serialises a shared log buffer.
type lockedWriter struct {
	w  *bytes.Buffer
	mu *sync.Mutex
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// mqyLockProbe records whether ApprovalWait and Yield ran inside a pglock
// lock; every other method is the real store.
type mqyLockProbe struct {
	mergequeue.Store
	mu     sync.Mutex
	calls  []string
	inLock bool
}

func (p *mqyLockProbe) note(op string, ctx context.Context) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, op)
	if pglock.InLock(ctx) {
		p.inLock = true
	}
}

func (p *mqyLockProbe) ApprovalWait(ctx context.Context, id uuid.UUID, epoch int64, window time.Duration) (mergequeue.ApprovalWaitResult, error) {
	p.note("approval_wait", ctx)
	return p.Store.ApprovalWait(ctx, id, epoch, window)
}

func (p *mqyLockProbe) Yield(ctx context.Context, id uuid.UUID, epoch int64, m mergequeue.YieldMarker) (mergequeue.Entry, error) {
	p.note("yield", ctx)
	return p.Store.Yield(ctx, id, epoch, m)
}

// TestMergeQueueYield_WaitAndYieldRunOutsideTheMergeIntentLock pins ADR-112
// R3's "never inside the lock": over a real pump, the approval wait (twice:
// start, then expiry) and the yield are called with no pglock lock held on
// their context. Counterfactual: calling ApprovalWait from inside
// withRunMergeLock's fn records inLock, RED.
func TestMergeQueueYield_WaitAndYieldRunOutsideTheMergeIntentLock(t *testing.T) {
	w := newMQELineWorld(t)
	probe := &mqyLockProbe{Store: w.srv.cfg.MergeQueue}
	w.srv.cfg.MergeQueue = probe
	a := w.seed("a", "a1", []byte(mqeSpecYAML))
	w.mqpHeldIntent(a, "a1", false)
	w.forgeReviews(a, []string{"alice"}, mqmReview(1, "alice", "DISMISSED", "a1"))

	w.pumpBase(a)
	w.backdateApprovalWait(a)
	w.pumpBase(a)

	w.requireYielded(a, mergeCandidateQueueReasonForgeApprovalDismissed, forgeApprovalNextApprovePR, mergeQueueYieldSiteMergeOnPass)
	probe.mu.Lock()
	defer probe.mu.Unlock()
	if strings.Join(probe.calls, ",") != "approval_wait,approval_wait,yield" || probe.inLock {
		t.Fatalf("store calls = %v inLock=%v, want approval_wait,approval_wait,yield outside any lock", probe.calls, probe.inLock)
	}
}
