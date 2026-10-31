package server

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/mergequeue"
)

// #4641 (ADR-112 residual H3): the cross-boundary head-pin suite. HTTP
// endpoint (inline merge_run) or the delegated may_merge arm → the
// merge-candidate gate → the ADR-112 fold → the D1 ledger in Postgres → the
// PRODUCTION server.ForgeMerger → the PRODUCTION githubclient.QueueMerge (via
// mqeAutoMerger, which mirrors serve.go's githubAutoMerger.mergeTarget: the
// serve.go leaf lives in package main) → the GraphQL / REST wire → the mcq
// fake → the audit chain. A per-test proxy answers ONLY the
// enablePullRequestAutoMerge mutation locally and forwards everything else,
// so the shared mcq fake is untouched. Every row approves ONLY at h1 and
// moves the PR head h1 -> h2 at one point in the dispatch, then reads
// committed state: the outcome, the PR, the enable calls, the PUTs, the claim
// and the queue/ledger/audit rows.

// Enable-mutation modes of the head-pin proxy.
const (
	// mhpRefuseOnMismatch answers a pinned enable whose expectedHeadOid is not
	// the PR's current head with a GraphQL error envelope (GitHub does not
	// document the wording, so neither does this fake), and APPLIES any other
	// enable — an unpinned one included — at the current head.
	mhpRefuseOnMismatch = "refuse_on_mismatch"
	// mhpCleanStatus answers every enable with GitHub's clean-status refusal,
	// so the ladder takes the REST fallback.
	mhpCleanStatus = "clean_status"
	// mhpApplyThenError (#4749) refuses a mismatched pin like
	// mhpRefuseOnMismatch, but the FIRST enable whose pin equals the current
	// head is APPLIED (recorded Applied), the head then moves to h2, and the
	// answer is a GraphQL error envelope: the client's re-read sees h2 and
	// reports graphql_reread although the enable took effect. Later enables
	// apply cleanly.
	mhpApplyThenError = "apply_then_error"
)

// mhpBound is the GraphQL argument binding for expectedHeadOid: the pin is
// resolved THROUGH the declared variable, never by assuming its name (C1).
var mhpBound = regexp.MustCompile(`expectedHeadOid:\s*\$(\w+)`)

// mhpEnable is one enablePullRequestAutoMerge call the proxy received.
type mhpEnable struct {
	// Pin is the value bound to the expectedHeadOid argument ("" unpinned).
	Pin string
	// Applied is true when the proxy answered success: auto-merge ENABLED at
	// AppliedHead.
	Applied     bool
	AppliedHead string
}

// mhpProxy sits in front of the mqm/mqe chain.
type mhpProxy struct {
	w    *mqmWorld
	r    *mcqRun
	rp   *httputil.ReverseProxy
	mode string

	mu sync.Mutex
	// afterSnapshot is the hook an arm plants to fire right after the fold's
	// review snapshot. It is not run directly: the snapshot request moves it
	// into beforeNext BEFORE the snapshot is forwarded (#4683).
	afterSnapshot func()
	// beforeNext runs once, at the very top of the next request the proxy
	// serves. It is published before the snapshot response is forwarded, so
	// the caller (which sends its requests serially) cannot have sent its next
	// request yet and always finds it set: the head move lands before that
	// request is served, deterministic by construction.
	beforeNext func()
	// onEnable runs once, on receipt of the enable mutation, before it is
	// judged.
	onEnable func()
	enables  []mhpEnable
	// appliedErr: the mhpApplyThenError one-shot has fired.
	appliedErr bool
}

func (p *mhpProxy) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	// A hook published by the previous request fires before anything else, so
	// the snapshot request, which publishes it below, cannot consume its own.
	p.mu.Lock()
	next := p.beforeNext
	p.beforeNext = nil
	p.mu.Unlock()
	if next != nil {
		next()
	}
	if req.Method != http.MethodPost || req.URL.Path != "/graphql" {
		p.rp.ServeHTTP(rw, req)
		return
	}
	body, _ := io.ReadAll(req.Body)
	_ = req.Body.Close()
	if bytes.Contains(body, []byte("enablePullRequestAutoMerge")) {
		p.serveEnable(rw, body)
		return
	}
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	if bytes.Contains(body, []byte("latestOpinionatedReviews")) {
		p.mu.Lock()
		p.beforeNext, p.afterSnapshot = p.afterSnapshot, nil
		p.mu.Unlock()
	}
	p.rp.ServeHTTP(rw, req)
}

func (p *mhpProxy) serveEnable(rw http.ResponseWriter, body []byte) {
	var gq struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	_ = json.Unmarshal(body, &gq)
	pin := ""
	if m := mhpBound.FindStringSubmatch(gq.Query); m != nil {
		pin, _ = gq.Variables[m[1]].(string)
	}
	p.mu.Lock()
	hook := p.onEnable
	p.onEnable = nil
	p.mu.Unlock()
	if hook != nil {
		hook()
	}
	head := p.head()
	rw.Header().Set("Content-Type", "application/json")
	call := mhpEnable{Pin: pin}
	switch {
	case p.mode == mhpCleanStatus:
		_, _ = rw.Write([]byte(`{"data":null,"errors":[{"type":"UNPROCESSABLE","message":"Pull request is in clean status"}]}`))
	case pin != "" && pin != head:
		_, _ = rw.Write([]byte(`{"data":null,"errors":[{"type":"UNPROCESSABLE","message":"Expected head OID does not match the pull request head"}]}`))
	case p.mode == mhpApplyThenError && !p.firedApplyErr():
		call.Applied, call.AppliedHead = true, head
		p.moveHead()
		_, _ = rw.Write([]byte(`{"data":null,"errors":[{"type":"INTERNAL","message":"Something went wrong while executing your query"}]}`))
	default:
		call.Applied, call.AppliedHead = true, head
		_, _ = rw.Write([]byte(`{"data":{"enablePullRequestAutoMerge":{"pullRequest":{"number":1,"url":"u","state":"OPEN"}}}}`))
	}
	p.mu.Lock()
	p.enables = append(p.enables, call)
	p.mu.Unlock()
}

// firedApplyErr reports whether the mhpApplyThenError one-shot already fired,
// marking it fired.
func (p *mhpProxy) firedApplyErr() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	fired := p.appliedErr
	p.appliedErr = true
	return fired
}

func (p *mhpProxy) head() string {
	var head string
	p.w.gh.set(func() { head = p.w.gh.prs[p.r.pr].head })
	return head
}

func (p *mhpProxy) calls() []mhpEnable {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]mhpEnable(nil), p.enables...)
}

// moveHead moves r's PR head to h2 on the mcq fake.
func (p *mhpProxy) moveHead() {
	p.w.gh.set(func() { p.w.gh.prs[p.r.pr].head = "h2" })
}

// newMHPProxy routes the server's GitHub client AND the head-pin merge leaf
// through a head-pin proxy for r, and returns the proxy plus the PRODUCTION
// ForgeMerger whose github leaf calls the production QueueMerge.
func newMHPProxy(t *testing.T, w *mqmWorld, r *mcqRun, mode string) (*mhpProxy, ForgeMerger) {
	t.Helper()
	orig := w.srv.cfg.GitHub
	target, err := url.Parse(orig.BaseURL)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	p := &mhpProxy{w: w, r: r, rp: httputil.NewSingleHostReverseProxy(target), mode: mode}
	ps := httptest.NewServer(p)
	t.Cleanup(ps.Close)
	c := *orig
	c.BaseURL = ps.URL
	w.srv.cfg.GitHub = &c
	return p, ForgeMerger{GitHub: mqeAutoMerger{gh: &c}}
}

// mhpArm is one head-moving dispatch arm: where the head moves, which proxy
// mode answers the enable, the refusal basis the forge reports, and the claim
// outcome the D1 ledger must be left in (#4749). claim and note are
// HARD-CODED per arm, never derived from the production predicate: note is the
// delegated arm's observe-only note for a claim left in arm.claim (#4683).
type mhpArm struct {
	name  string
	mode  string
	basis string
	claim mergeDispatchLedgerOutcome
	note  string
	plant func(p *mhpProxy)
}

var mhpArms = []mhpArm{
	// The head moves at the top of the first request after the fold's
	// snapshot request: the client pre-check sees h2 and sends no mutation.
	{"precheck", mhpRefuseOnMismatch, githubclient.HeadMismatchPrecheck, mergeDispatchRelease, autoDriveNoteHeadMoved, func(p *mhpProxy) {
		p.afterSnapshot = p.moveHead
	}},
	// The head moves on receipt of the pinned enable: the forge refuses it
	// (expectedHeadOid h1 vs head h2) and the client's re-read sees h2. The
	// basis is inferred, so the claim is held dispatch_failed (#4749).
	{"graphql", mhpRefuseOnMismatch, githubclient.HeadMismatchGraphQLReread, mergeDispatchUncertain, autoDriveNoteHeadMovedBasisHeld, func(p *mhpProxy) {
		p.onEnable = p.moveHead
	}},
	// The enable answers clean status, so the ladder takes the pinned REST
	// fallback; the head moves before the PUT is judged, so its sha h1 is
	// refused 409.
	{"rest", mhpCleanStatus, githubclient.HeadMismatchRESTSha, mergeDispatchRelease, autoDriveNoteHeadMoved, func(p *mhpProxy) {
		p.w.gh.set(func() { p.w.gh.onPut = func(int) { p.moveHead() } })
	}},
}

// requireHeadPinRefused asserts the committed state every head-moved row
// shares: the PR unmerged, no successful enable, the claim left in arm.claim
// (released: no claim row; dispatch_failed: the row held), one
// merge_dispatch_settled row with outcome arm.claim at h1 carrying the arm's
// refusal_basis (and observed_head_sha h2 where the forge reported it), and
// merge_queue_authorized.head_sha == settled head_sha == every pin the forge
// received == h1.
func requireHeadPinRefused(t *testing.T, w *mqmWorld, r *mcqRun, p *mhpProxy, arm mhpArm, path string) {
	t.Helper()
	var merged bool
	w.gh.set(func() { merged = w.gh.prs[r.pr].merged })
	if merged {
		t.Fatal("the PR merged; want it unmerged (the moved head was never judged)")
	}
	enables := p.calls()
	for _, e := range enables {
		if e.Applied {
			t.Fatalf("auto-merge was ENABLED at %s; want no successful enable", e.AppliedHead)
		}
		if e.Pin != "h1" {
			t.Fatalf("expectedHeadOid received = %q, want h1", e.Pin)
		}
	}
	puts := w.gh.putCalls()
	switch arm.basis {
	case githubclient.HeadMismatchPrecheck:
		if len(enables) != 0 || len(puts) != 0 {
			t.Fatalf("forge calls: %d enables, %d PUTs; want none (the pre-check sends nothing)", len(enables), len(puts))
		}
	case githubclient.HeadMismatchGraphQLReread:
		if len(enables) != 1 || len(puts) != 0 {
			t.Fatalf("forge calls: %d enables, %d PUTs; want 1 refused enable and no PUT", len(enables), len(puts))
		}
	case githubclient.HeadMismatchRESTSha:
		if len(enables) != 1 || len(puts) != 1 || puts[0].SHA != "h1" {
			t.Fatalf("forge calls: %d enables, PUTs %+v; want 1 enable and 1 PUT with sha h1", len(enables), puts)
		}
	}
	st, claimPath := w.claim(r)
	switch arm.claim {
	case mergeDispatchRelease:
		if st != "" {
			t.Fatalf("claim = %q, want released (no row)", st)
		}
	default:
		if st != string(mergequeue.DispatchFailed) || claimPath != path {
			t.Fatalf("claim = %q/%q, want dispatch_failed/%s (held)", st, claimPath, path)
		}
	}
	settled := w.rows(r, categoryMergeDispatchSettled)
	if len(settled) != 1 {
		t.Fatalf("merge_dispatch_settled rows = %d, want 1", len(settled))
	}
	sp := w.payload(settled[0])
	if sp["outcome"] != string(arm.claim) || sp["path"] != path || sp["head_sha"] != "h1" {
		t.Fatalf("settled payload = %v, want %s %s at h1", sp, arm.claim, path)
	}
	if sp["refusal_basis"] != arm.basis || sp["expected_head_sha"] != "h1" {
		t.Fatalf("settled payload = %v, want refusal_basis %s expected_head_sha h1", sp, arm.basis)
	}
	if arm.basis != githubclient.HeadMismatchRESTSha && sp["observed_head_sha"] != "h2" {
		t.Fatalf("settled payload = %v, want observed_head_sha h2", sp)
	}
	auth := w.rows(r, CategoryMergeQueueAuthorized)
	if len(auth) != 1 || w.payload(auth[0])["head_sha"] != sp["head_sha"] {
		t.Fatalf("merge_queue_authorized rows = %d (head %v); want one at the settled head %v", len(auth), auth, sp["head_sha"])
	}
}

// TestMergeHeadPinE2E_Inline: each arm answers 409 merge_head_moved naming h1
// and the arm's refusal_basis, with nothing merged and the claim in the arm's
// outcome (released on precheck / rest_sha, dispatch_failed on graphql). The
// REST row then re-invokes: the gate and the fold re-run at h2, where no
// approval qualifies, so 409 forge_approval_required — the moved head is
// never merged unapproved. Counterfactual: gate.HeadSHA -> "" at the
// merge_run.go dispatch sends no pin, so the precheck and graphql arms ENABLE
// auto-merge at h2 and the REST arm merges h2: 200, RED.
func TestMergeHeadPinE2E_Inline(t *testing.T) {
	for _, arm := range mhpArms {
		t.Run(arm.name, func(t *testing.T) {
			w, _ := newMQEWorld(t)
			r := w.faeInline("pin"+arm.name, faeGateSpecYAML)
			w.forgeReviews(r, []string{"carol"}, mqmReview(1, "carol", "APPROVED", "h1"))
			p, merger := newMHPProxy(t, w, r, arm.mode)
			w.srv.cfg.GateMerger = merger
			arm.plant(p)
			resp := w.mergeRun(r)
			code, msg, d := mqeErrorDetails(t, resp.Body.Bytes())
			if resp.Code != http.StatusConflict || code != mergeCodeHeadMoved {
				t.Fatalf("merge = %d %s, want 409 %s", resp.Code, resp.Body.String(), mergeCodeHeadMoved)
			}
			if d["head_sha"] != "h1" || d["refusal_basis"] != arm.basis {
				t.Fatalf("details = %v, want head_sha h1 refusal_basis %s", d, arm.basis)
			}
			// A declared gate folded, so the message names the evaluated
			// approval (C2); only the forge-guaranteed bases say nothing merged
			// (C3).
			if !strings.Contains(msg, "the approval was evaluated at") {
				t.Fatalf("message = %q, want the evaluated-approval clause (declared gate)", msg)
			}
			if got, want := strings.Contains(msg, "Nothing merged"), mergeHeadMovedNothingApplied(arm.basis); got != want {
				t.Fatalf("message nothing-merged claim = %v, want %v: %q", got, want, msg)
			}
			if got, want := strings.Contains(msg, "does not prove the forge applied nothing"), arm.claim == mergeDispatchUncertain; got != want {
				t.Fatalf("message held-claim clause = %v, want %v: %q", got, want, msg)
			}
			requireHeadPinRefused(t, w, r, p, arm, mergequeue.DispatchPathInline)
			if arm.basis != githubclient.HeadMismatchRESTSha {
				return
			}
			w.gh.set(func() { w.gh.onPut = nil })
			again := w.mergeRun(r)
			if again.Code != http.StatusConflict {
				t.Fatalf("re-invoke at h2 = %d %s, want 409 %s", again.Code, again.Body.String(), mergeCodeForgeApprovalRequired)
			}
			faeRefusal(t, again.Body.Bytes())
			var merged bool
			w.gh.set(func() { merged = w.gh.prs[r.pr].merged })
			if merged || len(w.gh.putCalls()) != 1 {
				t.Fatalf("after the re-invoke: merged=%v PUTs=%d, want unmerged with no further PUT", merged, len(w.gh.putCalls()))
			}
		})
	}
}

// TestMergeHeadPinE2E_InlineGateLessHappyPathPinned: a gate-less run (no
// declared approval gate) whose merge-candidate gate admitted h1 is now pinned
// too (the behaviour change): the enable carries expectedHeadOid h1, the
// clean-status fallback PUT carries sha h1, and the PR merges.
func TestMergeHeadPinE2E_InlineGateLessHappyPathPinned(t *testing.T) {
	w, _ := newMQEWorld(t)
	r := w.verified("gl", "h1")
	p, merger := newMHPProxy(t, w, r, mhpCleanStatus)
	w.srv.cfg.GateMerger = merger
	if resp := w.mergeRun(r); resp.Code != http.StatusOK {
		t.Fatalf("merge = %d %s, want 200", resp.Code, resp.Body.String())
	}
	if e := p.calls(); len(e) != 1 || e[0].Pin != "h1" {
		t.Fatalf("enables = %+v, want one pinned to h1", e)
	}
	if puts := w.gh.putCalls(); len(puts) != 1 || puts[0].SHA != "h1" {
		t.Fatalf("PUTs = %+v, want one with sha h1", puts)
	}
	var merged bool
	w.gh.set(func() { merged = w.gh.prs[r.pr].merged })
	if !merged {
		t.Fatal("the PR did not merge on the gate-less happy path")
	}
}

// TestMergeHeadPinE2E_Delegated: the same three arms on the delegated
// may_merge arm are observe-only with nothing merged and the claim in the
// arm's outcome (released: autoDriveNoteHeadMoved, the next tick re-gates at
// the new head; dispatch_failed on graphql: autoDriveNoteHeadMovedBasisHeld,
// held until reconciliation or the operator re-arm), and the head-moved log's
// claim_outcome names that SETTLE outcome (approval condition C1). The mcq
// fake reads the PR unmerged, so #4617's observe-before-settle falls through
// to the head-moved arm. The run declares an approval gate, so the log's
// pinned_to also names the evaluated approval (#4683). Counterfactual: liveHead -> "" in
// dispatchAcceptanceGatedMerge flips the outcome to Acted (auto-merge enabled
// at h2, or h2 merged), RED.
func TestMergeHeadPinE2E_Delegated(t *testing.T) {
	for _, arm := range mhpArms {
		t.Run(arm.name, func(t *testing.T) {
			w := newMQMWorld(t)
			logs := mhpCaptureLogs(w)
			r := w.faeDelegated("dpin" + arm.name)
			w.forgeReviews(r, []string{"carol"}, mqmReview(1, "carol", "APPROVED", "h1"))
			p, merger := newMHPProxy(t, w, r, arm.mode)
			arm.plant(p)
			requireObserveOnly(t, w.admqDrive(r, merger), arm.note)
			requireHeadPinRefused(t, w, r, p, arm, mergequeue.DispatchPathDelegated)
			rec := mhpHeadMovedRecord(t, logs)
			if got := rec["claim_outcome"]; got != string(arm.claim) {
				t.Fatalf("head-moved log claim_outcome = %v, want %s", got, arm.claim)
			}
			if got, _ := rec["pinned_to"].(string); !strings.Contains(got, "the approval was evaluated at") {
				t.Fatalf("head-moved log pinned_to = %q, want the evaluated-approval clause (declared gate)", got)
			}
		})
	}
}

// TestMergeHeadPinE2E_ReleaseFailureReportsSettledClaim is approval condition
// C1: a precheck refusal (releasable) whose ReleaseMergeDispatch fails leaves
// the claim dispatch_failed, and BOTH dispatch paths report that settle
// outcome: the inline 409 names a release that did not land (with the operator
// re-arm) and never the uncertain-basis clause, and the delegated arm answers
// autoDriveNoteHeadMovedReleaseFailed while its head-moved log's claim_outcome
// reads dispatch_failed (a re-derived classification would say released).
func TestMergeHeadPinE2E_ReleaseFailureReportsSettledClaim(t *testing.T) {
	arm := mhpArms[0] // precheck
	if arm.basis != githubclient.HeadMismatchPrecheck {
		t.Fatalf("mhpArms[0] = %s, want the precheck arm", arm.name)
	}
	arm.claim = mergeDispatchUncertain
	t.Run("inline", func(t *testing.T) {
		w, _ := newMQEWorld(t)
		w.srv.cfg.MergeQueue = mqeReleaseFailQueue{Store: w.store}
		r := w.faeInline("relfail", faeGateSpecYAML)
		w.forgeReviews(r, []string{"carol"}, mqmReview(1, "carol", "APPROVED", "h1"))
		p, merger := newMHPProxy(t, w, r, arm.mode)
		w.srv.cfg.GateMerger = merger
		arm.plant(p)
		resp := w.mergeRun(r)
		code, msg, d := mqeErrorDetails(t, resp.Body.Bytes())
		if resp.Code != http.StatusConflict || code != mergeCodeHeadMoved || d["refusal_basis"] != arm.basis {
			t.Fatalf("merge = %d %s, want 409 %s refusal_basis %s", resp.Code, resp.Body.String(), mergeCodeHeadMoved, arm.basis)
		}
		if strings.Contains(msg, "does not prove the forge applied nothing") || strings.Contains(msg, "claim was released") ||
			!strings.Contains(msg, "claim release did not land, so it is dispatch_failed") {
			t.Fatalf("message = %q, want the release that did not land named and no uncertain-basis clause", msg)
		}
		// handleMergeRun appends mergeDispatchRearmNote only when the settle
		// left the claim dispatch_failed, so these come through the handler.
		for _, want := range []string{"Once the operator re-arms the claim", "until the operator re-arms the claim"} {
			if !strings.Contains(msg, want) {
				t.Fatalf("message = %q, want the operator re-arm named (%q)", msg, want)
			}
		}
		requireHeadPinRefused(t, w, r, p, arm, mergequeue.DispatchPathInline)
	})
	t.Run("delegated", func(t *testing.T) {
		w := newMQMWorld(t)
		w.srv.cfg.MergeQueue = mqeReleaseFailQueue{Store: w.store}
		logs := mhpCaptureLogs(w)
		r := w.faeDelegated("drelfail")
		w.forgeReviews(r, []string{"carol"}, mqmReview(1, "carol", "APPROVED", "h1"))
		p, merger := newMHPProxy(t, w, r, arm.mode)
		arm.plant(p)
		requireObserveOnly(t, w.admqDrive(r, merger), autoDriveNoteHeadMovedReleaseFailed)
		requireHeadPinRefused(t, w, r, p, arm, mergequeue.DispatchPathDelegated)
		if got := mhpHeadMovedClaimOutcome(t, logs); got != string(mergeDispatchUncertain) {
			t.Fatalf("head-moved log claim_outcome = %q, want dispatch_failed (the settle outcome)", got)
		}
	})
}

// TestMergeHeadPinE2E_AppliedEnableErrorKeepsClaim is the issue-mandated
// applied-then-error fake (#4749): the pinned enable at h1 is APPLIED, the
// head then moves to h2, and the forge answers a GraphQL error, so the client
// reports graphql_reread. The claim must be HELD dispatch_failed: an approval
// is then planted at h2 so the fold approves the new head (it cannot mask the
// claim), and the re-invoke (inline: 409 merge_already_dispatched) / next tick
// (delegated: observe-only already claimed) never sends a second enable.
// Counterfactual: mergeDispatchHeadMismatchUncertain returning false releases
// the claim, so the re-invoke re-claims and enables again at h2, RED.
func TestMergeHeadPinE2E_AppliedEnableErrorKeepsClaim(t *testing.T) {
	requireApplied := func(t *testing.T, w *mqmWorld, r *mcqRun, p *mhpProxy, path string) {
		t.Helper()
		enables := p.calls()
		if len(enables) != 1 || !enables[0].Applied || enables[0].Pin != "h1" || enables[0].AppliedHead != "h1" {
			t.Fatalf("enables = %+v, want exactly one, pinned and applied at h1", enables)
		}
		var merged bool
		w.gh.set(func() { merged = w.gh.prs[r.pr].merged })
		if merged {
			t.Fatal("the PR merged; want it unmerged")
		}
		if st, cp := w.claim(r); st != string(mergequeue.DispatchFailed) || cp != path {
			t.Fatalf("claim = %q/%q, want dispatch_failed/%s", st, cp, path)
		}
	}
	t.Run("inline", func(t *testing.T) {
		w, _ := newMQEWorld(t)
		r := w.faeInline("applerr", faeGateSpecYAML)
		w.forgeReviews(r, []string{"carol"}, mqmReview(1, "carol", "APPROVED", "h1"))
		p, merger := newMHPProxy(t, w, r, mhpApplyThenError)
		w.srv.cfg.GateMerger = merger
		resp := w.mergeRun(r)
		code, msg, d := mqeErrorDetails(t, resp.Body.Bytes())
		if resp.Code != http.StatusConflict || code != mergeCodeHeadMoved || d["refusal_basis"] != githubclient.HeadMismatchGraphQLReread {
			t.Fatalf("merge = %d %s, want 409 %s refusal_basis graphql_reread", resp.Code, resp.Body.String(), mergeCodeHeadMoved)
		}
		if !strings.Contains(msg, "does not prove the forge applied nothing") {
			t.Fatalf("message = %q, want the held claim named", msg)
		}
		requireApplied(t, w, r, p, mergequeue.DispatchPathInline)
		w.forgeReviews(r, []string{"carol"}, mqmReview(1, "carol", "APPROVED", "h1"), mqmReview(2, "carol", "APPROVED", "h2"))
		again := w.mergeRun(r)
		if again.Code != http.StatusConflict || mqeCode(t, again.Body.Bytes()) != "merge_already_dispatched" {
			t.Fatalf("re-invoke at h2 = %d %s, want 409 merge_already_dispatched", again.Code, again.Body.String())
		}
		requireApplied(t, w, r, p, mergequeue.DispatchPathInline)
	})
	t.Run("delegated", func(t *testing.T) {
		w := newMQMWorld(t)
		r := w.faeDelegated("dapplerr")
		w.forgeReviews(r, []string{"carol"}, mqmReview(1, "carol", "APPROVED", "h1"))
		p, merger := newMHPProxy(t, w, r, mhpApplyThenError)
		requireObserveOnly(t, w.admqDrive(r, merger), autoDriveNoteHeadMovedBasisHeld)
		requireApplied(t, w, r, p, mergequeue.DispatchPathDelegated)
		w.forgeReviews(r, []string{"carol"}, mqmReview(1, "carol", "APPROVED", "h1"), mqmReview(2, "carol", "APPROVED", "h2"))
		requireObserveOnly(t, w.admqDrive(r, merger), autoDriveNoteAlreadyClaimed)
		requireApplied(t, w, r, p, mergequeue.DispatchPathDelegated)
	})
}

// mhpCaptureLogs swaps w's logger for a JSON handler over a mutex-guarded
// buffer (#3226) so a test can read the delegated head-moved log line.
func mhpCaptureLogs(w *mqmWorld) *syncBuffer {
	buf := &syncBuffer{}
	w.srv.cfg.Logger = slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return buf
}

// mhpHeadMovedRecord returns the ONE delegated head-moved log record, so
// pinned_to and claim_outcome are read from the same line.
func mhpHeadMovedRecord(t *testing.T, logs *syncBuffer) map[string]any {
	t.Helper()
	var recs []map[string]any
	for _, line := range strings.Split(logs.String(), "\n") {
		var rec map[string]any
		if json.Unmarshal([]byte(line), &rec) != nil {
			continue
		}
		if m, _ := rec["msg"].(string); strings.HasPrefix(m, "auto-drive: the forge refused the head-pinned merge") {
			recs = append(recs, rec)
		}
	}
	if len(recs) != 1 {
		t.Fatalf("head-moved log lines = %d (%v), want 1", len(recs), recs)
	}
	return recs[0]
}

// mhpHeadMovedClaimOutcome returns the claim_outcome of the ONE delegated
// head-moved log line.
func mhpHeadMovedClaimOutcome(t *testing.T, logs *syncBuffer) string {
	t.Helper()
	o, _ := mhpHeadMovedRecord(t, logs)["claim_outcome"].(string)
	return o
}
