package server

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
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
		mergeQueueYieldMergeChecksRed:                                      mergeQueueYieldNextRetriggerCI,
		mergeQueueYieldMergeChecksPending:                                  forgeApprovalNextRetryAfter,
		mergeQueueYieldMergeReviewPending:                                  forgeApprovalNextRetryAfter,
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

// TestMergeOnPassEnforceExpiredWait_Guards (#4748; CF3 vehicle) is a
// direct-call table over the real store, one assertion per branch of the
// expired-wait enforcement. The outcome fed in is a base_moved readvance
// carrying the generic ExpiryYield on an entry whose anchor is backdated past
// the window, unless the row says otherwise. Pass-through rows return o
// UNCHANGED; the pre-filter rows also take no lock. Refusing rows hold with no
// ExpiryYield. Every non-yielding row asserts no marker and no
// merge_queue_yielded row. MECHANISM for the claim rows: the anchor is
// expired, so with the claim guard deleted ApprovalWait reports expired and
// the helper yields a run whose dispatch is claimed, RED. Masking note: on the
// pump path the registry guard is masked by InFlightMarked (the pump clears
// its own mark after apply), so only the direct row pins it.
func TestMergeOnPassEnforceExpiredWait_Guards(t *testing.T) {
	ctx := context.Background()
	type setup struct {
		w   *mqmWorld
		r   *mcqRun
		req *mergeOnPassRequest
		o   *mergeOnPassOutcome
	}
	cases := []struct {
		name     string
		noAnchor bool
		fresh    bool // the anchor is stamped but NOT backdated (not expired)
		mutate   func(s setup)
		kind     mergeOnPassKind // "" = o unchanged
		reason   string
		noLock   bool
	}{
		{name: "nil ExpiryYield passes through", mutate: func(s setup) { s.o.ExpiryYield = nil }, noLock: true},
		{name: "a decision that marked in flight passes through", mutate: func(s setup) { s.o.InFlightMarked = true }, noLock: true},
		{name: "a settle passes through", mutate: func(s setup) { s.o.Kind = mergeOnPassSettle }, noLock: true},
		{name: "no established episode passes through", noAnchor: true, noLock: true},
		{name: "a re-admitted entry holds", mutate: func(s setup) {
			s.w.exec(`UPDATE merge_candidate_queue_entries SET admission_epoch = admission_epoch + 1 WHERE id = $1`, s.req.Entry.ID)
		}, kind: mergeOnPassHeld, reason: mergeOnPassReasonEntryChanged},
		{name: "a merging entry holds", mutate: func(s setup) {
			s.w.exec(`UPDATE merge_candidate_queue_entries SET phase = 'merging' WHERE id = $1`, s.req.Entry.ID)
		}, kind: mergeOnPassHeld, reason: mergeOnPassReasonEntryChanged},
		{name: "the in-flight registry holds", mutate: func(s setup) { s.w.srv.mergeInFlight.mark(s.r.id) },
			kind: mergeOnPassHeld, reason: mergeOnPassReasonAlreadyDispatched},
		{name: "a dispatched claim holds", mutate: func(s setup) {
			if _, ok, err := s.w.ledger.ClaimMergeDispatch(ctx, s.r.id, mergeDispatchPathQueue, "h1"); !ok || err != nil {
				s.w.t.Fatalf("seed claim: %v %v", ok, err)
			}
		}, kind: mergeOnPassHeld, reason: mergeOnPassReasonAlreadyDispatched},
		{name: "a dispatch_failed claim holds", mutate: func(s setup) {
			if _, ok, err := s.w.ledger.ClaimMergeDispatch(ctx, s.r.id, mergeDispatchPathQueue, "h1"); !ok || err != nil {
				s.w.t.Fatalf("seed claim: %v %v", ok, err)
			}
			_ = s.w.ledger.MarkMergeDispatchFailed(ctx, s.r.id, "uncertain")
		}, kind: mergeOnPassHeld, reason: mergeOnPassReasonAlreadyDispatched},
		{name: "a claim read error holds", mutate: func(s setup) { s.w.ledger.readErr = errors.New("injected claim read failure") },
			kind: mergeOnPassHeld, reason: mergeOnPassReasonLedgerUnavailable},
		{name: "no ledger holds", mutate: func(s setup) { s.req.Ledger = nil },
			kind: mergeOnPassHeld, reason: mergeOnPassReasonLedgerUnavailable},
		{name: "an untaken lock holds", mutate: func(s setup) {
			s.w.srv.cfg.MergeQueue = mqmLockQueue{Store: s.w.store, lockErr: errors.New("no lock")}
		}, kind: mergeOnPassHeld, reason: mergeOnPassReasonLockUnavailable},
		{name: "an unexpired wait passes through", fresh: true},
		{name: "an expired wait yields", kind: mergeOnPassExpiredYielded, reason: mergeCandidateQueueReasonForgeApprovalUndetermined},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newMQMWorld(t)
			r, req := w.ready("eg", "h1", mqeSpecYAML)
			switch {
			case tc.fresh:
				if _, err := w.store.ApprovalWait(ctx, req.Entry.ID, req.Entry.AdmissionEpoch, time.Hour); err != nil {
					t.Fatalf("stamp the approval wait: %v", err)
				}
				req.Entry = w.active(r)
			case !tc.noAnchor:
				req = w.mqmExpiredWait(r, req)
			}
			var locks atomic.Int32
			w.srv.cfg.MergeQueue = mqmOnLockQueue{Store: w.store, onLock: func() { locks.Add(1) }}
			o := mergeOnPassOutcome{Kind: mergeOnPassReadvance, Reason: mergeOnPassReasonBaseMoved, Detail: "main moved", HeadSHA: "h1",
				ExpiryYield: mergeOnPassUndeterminedExpiryYield(mergeOnPassReasonBaseMoved, "main moved")}
			if tc.mutate != nil {
				tc.mutate(setup{w: w, r: r, req: &req, o: &o})
			}

			got := w.srv.mergeOnPassEnforceExpiredWait(ctx, req, o)

			if tc.kind == "" {
				if got != o {
					t.Fatalf("outcome = %+v, want o unchanged %+v", got, o)
				}
			} else {
				requireOutcome(t, got, tc.kind, tc.reason)
				if got.ExpiryYield != nil {
					t.Fatalf("enforcement outcome carries ExpiryYield %+v, want nil", *got.ExpiryYield)
				}
			}
			if tc.noLock && locks.Load() != 0 {
				t.Fatalf("merge-intent lock taken %d times, want 0 (pre-filter)", locks.Load())
			}
			e := w.active(r)
			rows := w.rows(r, CategoryMergeQueueYielded)
			if tc.kind == mergeOnPassExpiredYielded {
				if e.State != mergequeue.StateHeld || !e.Yielded() || e.YieldReason != tc.reason || len(rows) != 1 ||
					w.payload(rows[0])["site"] != mergeQueueYieldSiteMergeOnPass || got.Yield.NextStep != forgeApprovalNextRetryAfter {
					t.Fatalf("expired yield: entry = %s %q yielded=%v rows=%d yield=%+v, want held, marked, one merge_on_pass row",
						e.State, e.YieldReason, e.Yielded(), len(rows), got.Yield)
				}
				return
			}
			if e.Yielded() || len(rows) != 0 {
				t.Fatalf("entry yielded=%v merge_queue_yielded rows=%d, want neither", e.Yielded(), len(rows))
			}
			if tc.noAnchor && e.ApprovalWaitStartedAt != nil {
				t.Fatalf("approval_wait_started_at = %v, want NULL (an entry that never waited is never stamped)", e.ApprovalWaitStartedAt)
			}
			// The claim rows seed their own claim, so only the merge and the
			// verdict/authorization rows are asserted absent here.
			w.requireMerges(0)
			if n := len(w.rows(r, CategoryMergeVerdictRecorded)) + len(w.rows(r, CategoryMergeQueueAuthorized)); n != 0 {
				t.Fatalf("verdict/authorization rows = %d, want 0", n)
			}
		})
	}
}

// TestMergeOnPassExpiredWaitYield_ConcurrentDispatchClaimSuppressesYield
// (#4748, first-officer ruling C1; CF10 vehicle) pins that the expiry
// judgement runs under the run's merge-intent lock, the lock that guards
// SetPhase(merging) and ClaimMergeDispatch. A is anchored past the window,
// its base moves and B is queued behind it. The decision answers a readvance
// carrying an ExpiryYield; then, at the enforcement's lock acquisition and
// EXACTLY once (approval condition L-b: the follow-up pump's D8 resume
// re-takes the lock), a competing dispatch takes the same lock and records
// SetPhase(merging) plus a queue claim. The enforcement must hold, A must
// keep its phase and claim with no marker, and the follow-up pump routes A to
// the D8 resume, so B stays held with no b1 PUT. MECHANISM: the competing
// claim lands after the decision and before the enforcement's lock; with the
// re-read, claim and ApprovalWait judgement moved before withRunMergeLock
// (CF10) the judgement sees passed and no claim, the hook then claims, and
// fn's Yield CAS (live + epoch only; SetPhase keeps the epoch) lands: A
// yields while claimed and the pump admits B, RED.
func TestMergeOnPassExpiredWaitYield_ConcurrentDispatchClaimSuppressesYield(t *testing.T) {
	w := newMQELineWorld(t)
	ctx := context.Background()
	a := w.seed("a", "a1", []byte(mqeSpecYAML))
	b := w.seed("b", "b1", []byte(mqeSpecYAML))
	w.mqpHeldIntent(a, "a1", false)
	w.mqpHeldIntent(b, "b1", false)
	w.forgeReviews(a, []string{"alice"}, mqmReview(1, "alice", "DISMISSED", "a1"))
	w.forgeReviews(b, []string{"alice"}, mqmReview(2, "alice", "APPROVED", "b1"))

	w.pumpBase(a)
	w.requireWaiting(a, "a1")
	w.backdateApprovalWait(a)
	w.behind(a, "a1", "a2")

	store := w.srv.cfg.MergeQueue
	ae := w.active(a)
	req := mergeOnPassRequest{Entry: ae, IntentSequence: ae.MergeIntentSequence, Ledger: w.srv.mergeDispatchLedger()}
	out := w.decide(req)
	requireOutcome(t, out, mergeOnPassReadvance, mergeOnPassReasonBaseMoved)
	if out.ExpiryYield == nil {
		t.Fatal("the base-moved readvance carries no ExpiryYield")
	}

	var once sync.Once
	competed := false
	w.srv.cfg.MergeQueue = mqmOnLockQueue{Store: store, onLock: func() {
		once.Do(func() {
			competed = true
			err := store.WithMergeIntentLock(ctx, a.id, func(lctx context.Context) error {
				if _, err := store.SetPhase(lctx, ae.ID, ae.AdmissionEpoch, mergequeue.PhaseMerging); err != nil {
					return err
				}
				_, err := store.ClaimMergeDispatch(lctx, a.id, mergeDispatchPathQueue, "a1")
				return err
			})
			if err != nil {
				t.Errorf("the competing dispatch did not land: %v", err)
			}
		})
	}}

	enforced := w.srv.mergeOnPassEnforceExpiredWait(ctx, req, out)
	if !competed {
		t.Fatal("the enforcement took no merge-intent lock")
	}
	if enforced.Kind != mergeOnPassHeld || (enforced.Reason != mergeOnPassReasonAlreadyDispatched && enforced.Reason != mergeOnPassReasonEntryChanged) {
		t.Fatalf("enforcement = %s/%s (%s), want held merge_already_dispatched or entry_changed", enforced.Kind, enforced.Reason, enforced.Detail)
	}
	readvances := 0
	if w.srv.applyMergeOnPassOutcome(ctx, req.Entry, enforced, &readvances) {
		t.Fatal("the pump continued past a held enforcement")
	}
	e := w.active(a)
	if e.Yielded() || len(w.rows(a, CategoryMergeQueueYielded)) != 0 {
		t.Fatalf("A yielded=%v rows=%d, want no marker and no merge_queue_yielded row (its dispatch is claimed)", e.Yielded(), len(w.rows(a, CategoryMergeQueueYielded)))
	}
	if e.State != mergequeue.StateLive || e.Phase != mergequeue.PhaseMerging || w.claims(a) != 1 {
		t.Fatalf("A = %s/%s claims=%d, want live, merging, its claim kept", e.State, e.Phase, w.claims(a))
	}

	w.pumpBase(a)
	if st := w.storeEntry(b); st.State != mergequeue.StateHeld {
		t.Fatalf("B after the follow-up pump = %s, want held (A routes to the D8 resume)", st.State)
	}
	for _, p := range w.gh.putCalls() {
		if p.SHA == "b1" {
			t.Fatal("b1 was PUT while A's dispatch is claimed")
		}
	}
}

// TestMergeQueueYieldVocabulary_FreshMap (T17): the accessor maps every
// closed yield_reason (the three docs/adr/112 A3 merge-gate reasons
// included) to the mapping's next_step, and returns a FRESH map each call
// (mutating one never reaches the server's mapping or a later read).
func TestMergeQueueYieldVocabulary_FreshMap(t *testing.T) {
	v := MergeQueueYieldVocabulary()
	if len(v) != len(mergeQueueYieldReasons) {
		t.Fatalf("vocabulary size = %d, want %d", len(v), len(mergeQueueYieldReasons))
	}
	for _, r := range mergeQueueYieldReasons {
		if got, ok := v[r]; !ok || got != mergeQueueYieldNextStep(r) {
			t.Errorf("vocabulary[%s] = %q (present=%v), want %q", r, got, ok, mergeQueueYieldNextStep(r))
		}
	}
	for r, want := range map[string]string{
		mergeQueueYieldMergeChecksRed:     mergeQueueYieldNextRetriggerCI,
		mergeQueueYieldMergeChecksPending: forgeApprovalNextRetryAfter,
		mergeQueueYieldMergeReviewPending: forgeApprovalNextRetryAfter,
	} {
		if v[r] != want {
			t.Errorf("vocabulary[%s] = %q, want %q", r, v[r], want)
		}
	}
	v[mergeQueueYieldMergeChecksRed] = "mutated"
	if again := MergeQueueYieldVocabulary(); again[mergeQueueYieldMergeChecksRed] != mergeQueueYieldNextRetriggerCI {
		t.Fatal("mutating one vocabulary map reached a later read")
	}
}

// TestMergeGateYieldFor (T18): docs/adr/112 A3.5 tier 2 orders (a) red >
// (b) pending > (c) review; the detail names the deciding tier's reasons,
// the red case names fishhawk_retrigger_ci then fishhawk_merge_run, and every
// lower retryable leg read is named as "also blocking"; a result with no
// retryable leg is ok=false. Every merge-gate yield is row 1 with the
// mapping's next step.
func TestMergeGateYieldFor(t *testing.T) {
	red := mergeQueueIneligibility{Code: mergeQueueReasonChecksRed, Detail: "ci failed"}
	pending := mergeQueueIneligibility{Code: mergeQueueReasonChecksNotGreen, Detail: "ci in progress"}
	review := mergeQueueIneligibility{Code: mergeQueueReasonReviewPending, Detail: "1 of 2 reviews terminal"}
	concern := mergeQueueIneligibility{Code: mergeQueueReasonBlockingConcernOpen, Detail: "a high concern is open"}
	for _, tc := range []struct {
		name    string
		reasons []mergeQueueIneligibility
		want    string
		has     []string
		hasNot  []string
	}{
		{"red beats pending and review", []mergeQueueIneligibility{review, pending, red}, mergeQueueYieldMergeChecksRed,
			[]string{"ci failed", "fishhawk_retrigger_ci, then fishhawk_merge_run", "also blocking: checks_not_green: ci in progress; review_pending"}, nil},
		{"pending beats review", []mergeQueueIneligibility{review, pending}, mergeQueueYieldMergeChecksPending,
			[]string{"ci in progress", "also blocking: review_pending"}, []string{"fishhawk_retrigger_ci"}},
		{"review alone", []mergeQueueIneligibility{review}, mergeQueueYieldMergeReviewPending,
			[]string{"1 of 2 reviews terminal"}, []string{"also blocking", "fishhawk_retrigger_ci"}},
		{"a non-retryable leg is never named as blocking the tier", []mergeQueueIneligibility{concern, pending}, mergeQueueYieldMergeChecksPending,
			[]string{"ci in progress"}, []string{"a high concern"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := mergeQueueEligibilityResult{Reasons: tc.reasons}
			y, ok := mergeGateYieldFor(mergeGateLegsOf(r), r)
			if !ok || y.Reason != tc.want || y.NextStep != mergeQueueYieldNextStep(tc.want) || y.Row != mergeQueueYieldRowEligibility {
				t.Fatalf("mergeGateYieldFor = %+v ok=%v, want %s row 1", y, ok, tc.want)
			}
			for _, h := range tc.has {
				if !strings.Contains(y.Detail, h) {
					t.Errorf("detail %q does not name %q", y.Detail, h)
				}
			}
			for _, h := range tc.hasNot {
				if strings.Contains(y.Detail, h) {
					t.Errorf("detail %q names %q", y.Detail, h)
				}
			}
		})
	}
	for _, r := range []mergeQueueEligibilityResult{
		{}, {Reasons: []mergeQueueIneligibility{concern}},
		{Reasons: []mergeQueueIneligibility{{Code: mergeQueueReasonForgeApprovalVetoed, Detail: "bob"}}},
	} {
		if y, ok := mergeGateYieldFor(mergeGateLegsOf(r), r); ok {
			t.Errorf("mergeGateYieldFor(%v) = %+v, want ok=false (no retryable leg)", r.codes(), y)
		}
	}
	if m := (mergeQueueYield{Reason: mergeQueueYieldForgeReviewRequired}).marker(); !m.Row4 {
		t.Error("a forge_review_required marker does not set Row4")
	}
	if m := (mergeQueueYield{Reason: mergeQueueYieldMergeChecksRed}).marker(); m.Row4 {
		t.Error("a merge_checks_red marker sets Row4")
	}
	for _, r := range []string{mergeQueueYieldMergeChecksRed, mergeQueueYieldMergeChecksPending, mergeQueueYieldMergeReviewPending} {
		if got := mergeQueueYieldRow(r); got != mergeQueueYieldRowEligibility {
			t.Errorf("mergeQueueYieldRow(%s) = %d, want 1", r, got)
		}
	}
}

// TestMergeOnPassEnforceExpiredWait_ClosedApprovalWaitNeverFires (T19, the
// SERVER half (ii) of approval condition "A3.7 approval anchor, rule (i)";
// CF17 vehicle): a live entry whose approval anchor is RETAINED past the
// window but whose approval wait is CLOSED (a clear closed it; seeded by
// construction) and whose decision is a base_moved readvance carrying an
// ExpiryYield is returned UNCHANGED with no lock taken, no marker and no
// merge_queue_yielded row. MECHANISM: the anchor is expired, so with the
// open-bit condition deleted the enforcement yields
// forge_approval_undetermined, RED.
func TestMergeOnPassEnforceExpiredWait_ClosedApprovalWaitNeverFires(t *testing.T) {
	ctx := context.Background()
	w := newMQMWorld(t)
	r, req := w.ready("eg", "h1", mqeSpecYAML)
	req = w.mqmExpiredWait(r, req)
	w.exec(`UPDATE merge_candidate_queue_entries SET approval_wait_open = false WHERE id = $1`, req.Entry.ID)
	req.Entry = w.active(r)
	if req.Entry.ApprovalWaitStartedAt == nil || req.Entry.ApprovalWaitOpen {
		t.Fatalf("fixture: anchor %v open=%v, want a retained anchor with the wait closed", req.Entry.ApprovalWaitStartedAt, req.Entry.ApprovalWaitOpen)
	}
	var locks atomic.Int32
	w.srv.cfg.MergeQueue = mqmOnLockQueue{Store: w.store, onLock: func() { locks.Add(1) }}
	o := mergeOnPassOutcome{Kind: mergeOnPassReadvance, Reason: mergeOnPassReasonBaseMoved, Detail: "main moved", HeadSHA: "h1",
		ExpiryYield: mergeOnPassUndeterminedExpiryYield(mergeOnPassReasonBaseMoved, "main moved")}

	if got := w.srv.mergeOnPassEnforceExpiredWait(ctx, req, o); got != o {
		t.Fatalf("outcome = %+v, want o unchanged %+v", got, o)
	}
	if locks.Load() != 0 {
		t.Fatalf("merge-intent lock taken %d times, want 0 (pre-filter)", locks.Load())
	}
	if e := w.active(r); e.Yielded() || len(w.rows(r, CategoryMergeQueueYielded)) != 0 {
		t.Fatalf("entry yielded=%v rows=%d, want neither (a retained anchor never expires a readvance)", e.Yielded(), len(w.rows(r, CategoryMergeQueueYielded)))
	}
}

// TestMergeQueueYield_AwaitApprovalAfterAClearYieldsAtOnce (the SERVER half
// (i) of approval condition "A3.7 approval anchor, rule (i)"): A waits at the
// head on a DISMISSED review, its anchor expires and it yields; the marker is
// then cleared (the store's ClearYield, as a re-probe would) and ONE pump
// re-admits A in a later epoch, where the fold still reads DISMISSED: the
// await_approval judges the RETAINED anchor expired and A yields again in
// that same pump, continuing the cleared episode's yielded_at. MECHANISM: the
// anchor is two hours old, so with ClearYield nulling it again the pump
// stamps a fresh anchor and waits, RED.
func TestMergeQueueYield_AwaitApprovalAfterAClearYieldsAtOnce(t *testing.T) {
	ctx := context.Background()
	w := newMQELineWorld(t)
	a := w.seed("a", "a1", []byte(mqeSpecYAML))
	w.mqpHeldIntent(a, "a1", false)
	y := w.yieldDismissed(a)
	anchor := *y.ApprovalWaitStartedAt
	if _, err := w.store.ClearYield(ctx, y.ID, y.AnchoredHeadSHA, y.YieldRevision, false); err != nil {
		t.Fatalf("ClearYield: %v", err)
	}
	w.srv.writeMergeQueueYieldCleared(ctx, y, w.active(a), forgeApprovalDecision{})

	w.pumpBase(a)

	e := w.active(a)
	if e.State != mergequeue.StateHeld || !e.Yielded() || e.YieldReason != mergeCandidateQueueReasonForgeApprovalDismissed {
		t.Fatalf("A after one pump = %s %q yielded=%v, want yielded forge_approval_dismissed at once", e.State, e.YieldReason, e.Yielded())
	}
	if e.ApprovalWaitStartedAt == nil || !e.ApprovalWaitStartedAt.Equal(anchor) || e.AdmissionEpoch <= y.AdmissionEpoch {
		t.Fatalf("A anchor %v epoch %d, want the retained %v in a later epoch than %d", e.ApprovalWaitStartedAt, e.AdmissionEpoch, anchor, y.AdmissionEpoch)
	}
	if !e.YieldedAt.Equal(*y.YieldedAt) {
		t.Fatalf("A yielded_at %v, want the continued episode %v (docs/adr/112 A3.7)", e.YieldedAt, y.YieldedAt)
	}
}
