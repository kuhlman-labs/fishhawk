package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/mergequeue"
)

// #4641 (ADR-112 residual H3) and #4676: the cross-boundary head-pin suite.
// HTTP endpoint (inline merge_run) or the delegated may_merge arm → the #3109
// conflict guard → the merge-candidate gate → the ADR-112 fold → the server
// readiness pre-read → the D1 ledger in Postgres → the PRODUCTION
// server.ForgeMerger → the PRODUCTION githubclient.QueueMerge (via
// mqeAutoMerger, which mirrors serve.go's githubAutoMerger.mergeTarget: the
// serve.go leaf lives in package main) → the REST / GraphQL wire → the mcq
// fake → the audit chain. A per-test proxy answers ONLY the
// enablePullRequestAutoMerge mutation locally and forwards everything else,
// so the shared mcq fake is untouched. A head-pinned dispatch must never send
// that mutation (#4676); the proxy APPLIES any enable it receives and keeps it
// ARMED across a later head move, and fireArmed then merges the PR at its
// then-current head, as GitHub's auto-merge would. Every row reads committed
// state: the outcome, the PR, the enable calls, the PUTs, the claim and the
// queue/ledger/audit rows.

// mhpEnable is one enablePullRequestAutoMerge call the proxy received.
type mhpEnable struct {
	// Applied is true when the proxy answered success: auto-merge ARMED at
	// AppliedHead. mhpArmedSurvivesPush keeps it armed across moveHead.
	Applied     bool
	AppliedHead string
}

// mhpArmedSurvivesPush is the proxy's only enable mode (#4676): every enable
// is applied and stays armed after the head moves, the residual the issue
// names (a write-access push does not disarm it).
const mhpArmedSurvivesPush = "armed_survives_push"

// mhpProxy sits in front of the mqm/mqe chain.
type mhpProxy struct {
	w  *mqmWorld
	r  *mcqRun
	rp *httputil.ReverseProxy

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
	enables    []mhpEnable
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
		head := p.head()
		p.mu.Lock()
		p.enables = append(p.enables, mhpEnable{Applied: true, AppliedHead: head})
		p.mu.Unlock()
		rw.Header().Set("Content-Type", "application/json")
		_, _ = rw.Write([]byte(`{"data":{"enablePullRequestAutoMerge":{"pullRequest":{"number":1,"url":"u","state":"OPEN"}}}}`))
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

// moveHead moves r's PR head to h2 on the mcq fake (a write-access push).
func (p *mhpProxy) moveHead() {
	p.w.gh.set(func() { p.w.gh.prs[p.r.pr].head = "h2" })
}

// fireArmed is GitHub's auto-merge firing: when any enable was applied (and,
// mhpArmedSurvivesPush, survived a head move) the PR merges at its CURRENT
// head, which it returns ("" when nothing was armed or the PR already merged).
func (p *mhpProxy) fireArmed() string {
	if len(p.calls()) == 0 {
		return ""
	}
	var head string
	p.w.gh.set(func() {
		pr := p.w.gh.prs[p.r.pr]
		if pr.merged {
			return
		}
		pr.merged, pr.state, head = true, "closed", pr.head
	})
	return head
}

// setState serves mergeable_state on r's PR (the mqm world's GET override).
func (p *mhpProxy) setState(state string) {
	p.w.mergeable(p.r, state)
}

// newMHPProxy routes the server's GitHub client AND the head-pin merge leaf
// through a head-pin proxy for r, and returns the proxy plus the PRODUCTION
// ForgeMerger whose github leaf calls the production QueueMerge.
func newMHPProxy(t *testing.T, w *mqmWorld, r *mcqRun) (*mhpProxy, ForgeMerger) {
	t.Helper()
	orig := w.srv.cfg.GitHub
	target, err := url.Parse(orig.BaseURL)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	p := &mhpProxy{w: w, r: r, rp: httputil.NewSingleHostReverseProxy(target)}
	ps := httptest.NewServer(p)
	t.Cleanup(ps.Close)
	c := *orig
	c.BaseURL = ps.URL
	w.srv.cfg.GitHub = &c
	return p, ForgeMerger{GitHub: mqeAutoMerger{gh: &c}}
}

// mhpArm is one head-moving dispatch arm: where the head moves, the refusal
// basis the forge reports, and the claim outcome the D1 ledger must be left
// in (#4749). claim and note are HARD-CODED per arm, never derived from the
// production predicate: note is the delegated arm's observe-only note for a
// claim left in arm.claim (#4683). Both arms serve mergeable_state clean, so
// the server pre-read falls through and the leaf alone answers. The
// graphql_reread arm is gone with the pinned enable (#4676).
type mhpArm struct {
	name  string
	basis string
	claim mergeDispatchLedgerOutcome
	note  string
	plant func(p *mhpProxy)
}

var mhpArms = []mhpArm{
	// The head moves at the top of the first request after the fold's
	// snapshot request: the pre-read sees h2 and falls through, and the leaf's
	// pre-check sees h2 and sends nothing.
	{"precheck", githubclient.HeadMismatchPrecheck, mergeDispatchRelease, autoDriveNoteHeadMoved, func(p *mhpProxy) {
		p.setState("clean")
		p.afterSnapshot = p.moveHead
	}},
	// The readiness GET reads h1 clean, so the leaf sends the pinned PUT; the
	// head moves before the PUT is judged, so its sha h1 is refused 409.
	{"rest", githubclient.HeadMismatchRESTSha, mergeDispatchRelease, autoDriveNoteHeadMoved, func(p *mhpProxy) {
		p.setState("clean")
		p.w.gh.set(func() { p.w.gh.onPut = func(int) { p.moveHead() } })
	}},
}

// requireHeadPinRefused asserts the committed state every head-moved row
// shares: the PR unmerged, NO auto-merge enable at all (#4676), the claim left
// in arm.claim (released: no claim row; dispatch_failed: the row held), one
// merge_dispatch_settled row with outcome arm.claim at h1 carrying the arm's
// refusal_basis (and observed_head_sha h2 where the forge reported it), and
// merge_queue_authorized.head_sha == settled head_sha == every PUT sha == h1.
func requireHeadPinRefused(t *testing.T, w *mqmWorld, r *mcqRun, p *mhpProxy, arm mhpArm, path string) {
	t.Helper()
	var merged bool
	w.gh.set(func() { merged = w.gh.prs[r.pr].merged })
	if merged {
		t.Fatal("the PR merged; want it unmerged (the moved head was never judged)")
	}
	if enables := p.calls(); len(enables) != 0 {
		t.Fatalf("auto-merge enables = %+v; want none (a head-pinned dispatch never arms auto-merge)", enables)
	}
	puts := w.gh.putCalls()
	switch arm.basis {
	case githubclient.HeadMismatchPrecheck:
		if len(puts) != 0 {
			t.Fatalf("PUTs = %+v; want none (the pre-check sends nothing)", puts)
		}
	case githubclient.HeadMismatchRESTSha:
		if len(puts) != 1 || puts[0].SHA != "h1" {
			t.Fatalf("PUTs = %+v; want 1 PUT with sha h1", puts)
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
// and the arm's refusal_basis, with nothing merged, nothing armed and the
// claim released. The REST row then re-invokes: the gate and the fold re-run
// at h2, where no approval qualifies, so 409 forge_approval_required — the
// moved head is never merged unapproved. Counterfactual: gate.HeadSHA -> ""
// at the merge_run.go dispatch sends no pin, so the unpinned ladder arms
// auto-merge (an enable is recorded): RED.
func TestMergeHeadPinE2E_Inline(t *testing.T) {
	for _, arm := range mhpArms {
		t.Run(arm.name, func(t *testing.T) {
			w, _ := newMQEWorld(t)
			r := w.faeInline("pin"+arm.name, faeGateSpecYAML)
			w.forgeReviews(r, []string{"carol"}, mqmReview(1, "carol", "APPROVED", "h1"))
			p, merger := newMHPProxy(t, w, r)
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
			// approval (C2); both forge-guaranteed bases say nothing merged
			// (C3).
			if !strings.Contains(msg, "the approval was evaluated at") {
				t.Fatalf("message = %q, want the evaluated-approval clause (declared gate)", msg)
			}
			if got, want := strings.Contains(msg, "Nothing merged"), mergeHeadMovedNothingApplied(arm.basis); got != want || !want {
				t.Fatalf("message nothing-merged claim = %v, want %v: %q", got, want, msg)
			}
			if strings.Contains(msg, "does not prove the forge applied nothing") {
				t.Fatalf("message = %q, want no held-claim clause (the basis is forge-guaranteed)", msg)
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

// TestMergeHeadPinE2E_InlineGateLessHappyPathPinned (X8): a gate-less run (no
// declared approval gate) whose merge-candidate gate admitted h1 is pinned
// too: no auto-merge enable, ONE synchronous PUT carrying sha h1, and the PR
// merges.
func TestMergeHeadPinE2E_InlineGateLessHappyPathPinned(t *testing.T) {
	w, _ := newMQEWorld(t)
	r := w.verified("gl", "h1")
	p, merger := newMHPProxy(t, w, r)
	p.setState("clean")
	w.srv.cfg.GateMerger = merger
	if resp := w.mergeRun(r); resp.Code != http.StatusOK {
		t.Fatalf("merge = %d %s, want 200", resp.Code, resp.Body.String())
	}
	if e := p.calls(); len(e) != 0 {
		t.Fatalf("enables = %+v, want none (a head-pinned dispatch never arms auto-merge)", e)
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

// TestMergeHeadPinE2E_Delegated: the same arms on the delegated may_merge arm
// are observe-only with nothing merged, nothing armed and the claim released
// (autoDriveNoteHeadMoved, the next tick re-gates at the new head), and the
// head-moved log's claim_outcome names that SETTLE outcome (approval condition
// C1). The mcq fake reads the PR unmerged, so #4617's observe-before-settle
// falls through to the head-moved arm. The run declares an approval gate, so
// the log's pinned_to also names the evaluated approval (#4683).
// Counterfactual: liveHead -> "" in dispatchAcceptanceGatedMerge flips the
// outcome to Acted (auto-merge armed), RED.
func TestMergeHeadPinE2E_Delegated(t *testing.T) {
	for _, arm := range mhpArms {
		t.Run(arm.name, func(t *testing.T) {
			w := newMQMWorld(t)
			logs := mhpCaptureLogs(w)
			r := w.faeDelegated("dpin" + arm.name)
			w.forgeReviews(r, []string{"carol"}, mqmReview(1, "carol", "APPROVED", "h1"))
			p, merger := newMHPProxy(t, w, r)
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

// requireNeverArmed asserts the #4676 property after a write-access push: no
// auto-merge was armed, so firing "armed" merges nothing and the PR stays
// unmerged at its moved head.
func requireNeverArmed(t *testing.T, w *mqmWorld, r *mcqRun, p *mhpProxy) {
	t.Helper()
	p.moveHead()
	if head := p.fireArmed(); head != "" {
		t.Fatalf("an armed auto-merge fired at %s after the push; want nothing armed (enables %+v)", head, p.calls())
	}
	var merged bool
	w.gh.set(func() { merged = w.gh.prs[r.pr].merged })
	if merged {
		t.Fatal("PR merged; want it unmerged (the pushed head was never judged)")
	}
	if e := p.calls(); len(e) != 0 {
		t.Fatalf("enables = %+v, want none", e)
	}
}

// requireNotMergeReady409 asserts a 409 merge_checks_pending reason
// not_merge_ready and returns its details.
func requireNotMergeReady409(t *testing.T, resp *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	code, _, d := mqeErrorDetails(t, resp.Body.Bytes())
	if resp.Code != http.StatusConflict || code != "merge_checks_pending" || d["reason"] != "not_merge_ready" {
		t.Fatalf("merge = %d %s, want 409 merge_checks_pending reason not_merge_ready", resp.Code, resp.Body.String())
	}
	return d
}

// TestMergeHeadPinE2E_PinnedNeverArmsAutoMerge is the #4676 cross-boundary
// pin: a head-pinned dispatch, inline and delegated, never arms a GitHub
// auto-merge, so a write-access push after the dispatch can never fire a merge
// at a head the forge-approval fold never judged.
//
//   - inline / delegated serve an EMPTY mergeable_state at h1, so the server
//     pre-read FALLS THROUGH and the leaf is the only control on the path (the
//     pre-read cannot mask C27): the leaf refuses not-merge-ready with nothing
//     sent and the claim is released (one released settle row, no claim row:
//     C31 reads that committed state). The head then moves to h2 and the proxy
//     fires anything armed: nothing is, so the PR stays unmerged.
//   - control proves the fixture: an unpinned enable through the same proxy IS
//     armed, survives the push and merges h2.
//   - inline_pre_read serves blocked at h1: the server pre-read refuses
//     before the claim, so ZERO claim rows and ZERO settle rows.
//
// Counterfactual C27: routing QueueMerge's pinned arm to EnableAutoMerge arms
// auto-merge at h1, the inline merge answers 200, and the fire after the push
// merges h2: RED.
func TestMergeHeadPinE2E_PinnedNeverArmsAutoMerge(t *testing.T) {
	t.Run("inline", func(t *testing.T) {
		w, _ := newMQEWorld(t)
		r := w.faeInline("never", faeGateSpecYAML)
		w.forgeReviews(r, []string{"carol"}, mqmReview(1, "carol", "APPROVED", "h1"))
		p, merger := newMHPProxy(t, w, r)
		w.srv.cfg.GateMerger = merger
		d := requireNotMergeReady409(t, w.mergeRun(r))
		if d["head_sha"] != "h1" {
			t.Fatalf("details = %v, want head_sha h1", d)
		}
		if puts := w.gh.putCalls(); len(puts) != 0 {
			t.Fatalf("PUTs = %+v, want none", puts)
		}
		if st, _ := w.claim(r); st != "" {
			t.Fatalf("claim = %q, want released (no row): the leaf refused before any PUT", st)
		}
		settled := w.rows(r, categoryMergeDispatchSettled)
		if len(settled) != 1 || w.payload(settled[0])["outcome"] != string(mergeDispatchRelease) {
			t.Fatalf("merge_dispatch_settled rows = %d, want one released", len(settled))
		}
		requireNeverArmed(t, w, r, p)
		// The re-invoke at the pushed head re-runs the gate and the fold at
		// h2, where no approval qualifies.
		again := w.mergeRun(r)
		if again.Code != http.StatusConflict {
			t.Fatalf("re-invoke at h2 = %d %s, want 409 %s", again.Code, again.Body.String(), mergeCodeForgeApprovalRequired)
		}
		faeRefusal(t, again.Body.Bytes())
	})
	t.Run("delegated", func(t *testing.T) {
		w := newMQMWorld(t)
		r := w.faeDelegated("dnever")
		w.forgeReviews(r, []string{"carol"}, mqmReview(1, "carol", "APPROVED", "h1"))
		p, merger := newMHPProxy(t, w, r)
		out := w.admqDrive(r, merger)
		if out.Acted || out.MergeNotReady == nil || out.MergeNotReady.ClaimOutcome != autoDriveClaimOutcomeReleased {
			t.Fatalf("outcome = %+v (merge_not_ready %+v), want observe-only not-merge-ready, claim released", out, out.MergeNotReady)
		}
		if st, _ := w.claim(r); st != "" {
			t.Fatalf("claim = %q, want released (no row)", st)
		}
		if puts := w.gh.putCalls(); len(puts) != 0 {
			t.Fatalf("PUTs = %+v, want none", puts)
		}
		requireNeverArmed(t, w, r, p)
	})
	t.Run("control", func(t *testing.T) {
		w, _ := newMQEWorld(t)
		r := w.faeInline("ctl", faeGateSpecYAML)
		p, _ := newMHPProxy(t, w, r)
		// The unpinned enable is the pre-#4676 pinned path's effect (an
		// auto-merge armed on the PR); the mcq fake routes any owner/repo.
		if err := w.srv.cfg.GitHub.EnableAutoMerge(context.Background(), forge.FromGitHubInstallationID(1),
			githubclient.RepoRef{Owner: "x", Name: "y"}, r.pr, forge.MergeMethodSquash); err != nil {
			t.Fatalf("EnableAutoMerge: %v", err)
		}
		p.moveHead()
		if head := p.fireArmed(); head != "h2" {
			t.Fatalf("fireArmed = %q, want h2 (the fixture must merge the pushed head when anything is armed)", head)
		}
	})
	t.Run("inline_pre_read", func(t *testing.T) {
		w, _ := newMQEWorld(t)
		r := w.faeInline("preread", faeGateSpecYAML)
		w.forgeReviews(r, []string{"carol"}, mqmReview(1, "carol", "APPROVED", "h1"))
		p, merger := newMHPProxy(t, w, r)
		p.setState("blocked")
		w.srv.cfg.GateMerger = merger
		d := requireNotMergeReady409(t, w.mergeRun(r))
		if d["mergeable_state"] != "blocked" || d["resumable"] != true {
			t.Fatalf("details = %v, want mergeable_state blocked resumable true", d)
		}
		if st, _ := w.claim(r); st != "" {
			t.Fatalf("claim = %q, want none (the pre-read refuses before the claim)", st)
		}
		if n := len(w.rows(r, categoryMergeDispatchSettled)); n != 0 {
			t.Fatalf("merge_dispatch_settled rows = %d, want 0", n)
		}
		if puts := w.gh.putCalls(); len(puts) != 0 {
			t.Fatalf("PUTs = %+v, want none", puts)
		}
		requireNeverArmed(t, w, r, p)
	})
}

// TestMergeHeadPinE2E_PinnedMergeStates walks each mergeable_state class end
// to end on a gate-less pinned inline run (#4676): only clean and has_hooks
// send the PUT; unstable keeps checks_pending (CC5); the other states answer
// not_merge_ready from the server pre-read with no claim (resumable per
// state); an empty state falls through the pre-read and the LEAF refuses,
// releasing its claim; a 405 on the PUT keeps the 502 (X1) and releases; and
// dirty is refused by the #3109 conflict guard BEFORE the verdict (Z1).
// No row ever records an auto-merge enable.
func TestMergeHeadPinE2E_PinnedMergeStates(t *testing.T) {
	cases := []struct {
		name      string
		state     string
		putStatus int
		status    int
		code      string
		reason    string
		resumable any // nil = not asserted
		puts      int
		settled   int // released settle rows
		verdicts  int
		merged    bool
	}{
		{name: "clean", state: "clean", status: http.StatusOK, puts: 1, verdicts: 1, merged: true},
		{name: "has_hooks", state: "has_hooks", status: http.StatusOK, puts: 1, verdicts: 1, merged: true},
		{name: "unstable", state: "unstable", status: http.StatusConflict, code: "merge_checks_pending", reason: "checks_pending", verdicts: 1},
		{name: "blocked", state: "blocked", status: http.StatusConflict, code: "merge_checks_pending", reason: "not_merge_ready", resumable: true, verdicts: 1},
		{name: "unknown", state: "unknown", status: http.StatusConflict, code: "merge_checks_pending", reason: "not_merge_ready", resumable: true, verdicts: 1},
		{name: "behind", state: "behind", status: http.StatusConflict, code: "merge_checks_pending", reason: "not_merge_ready", resumable: false, verdicts: 1},
		{name: "draft", state: "draft", status: http.StatusConflict, code: "merge_checks_pending", reason: "not_merge_ready", resumable: false, verdicts: 1},
		{name: "empty_leaf", state: "", status: http.StatusConflict, code: "merge_checks_pending", reason: "not_merge_ready", settled: 1, verdicts: 1},
		{name: "put_405", state: "clean", putStatus: http.StatusMethodNotAllowed, status: http.StatusBadGateway, code: "merge_dispatch_failed", puts: 1, settled: 1, verdicts: 1},
		{name: "dirty_guard_first", state: "dirty", status: http.StatusConflict, code: "merge_conflicting"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w, _ := newMQEWorld(t)
			r := w.verified("st"+tc.name, "h1")
			p, merger := newMHPProxy(t, w, r)
			p.setState(tc.state)
			if tc.putStatus != 0 {
				w.gh.set(func() { w.gh.putStatus[r.pr] = tc.putStatus })
			}
			w.srv.cfg.GateMerger = merger
			resp := w.mergeRun(r)
			if resp.Code != tc.status {
				t.Fatalf("merge = %d %s, want %d", resp.Code, resp.Body.String(), tc.status)
			}
			if tc.code != "" {
				code, msg, d := mqeErrorDetails(t, resp.Body.Bytes())
				if code != tc.code {
					t.Fatalf("code = %q (%s), want %q", code, resp.Body.String(), tc.code)
				}
				if tc.reason != "" && d["reason"] != tc.reason {
					t.Fatalf("details.reason = %v, want %s", d["reason"], tc.reason)
				}
				if tc.resumable != nil && d["resumable"] != tc.resumable {
					t.Fatalf("details.resumable = %v, want %v", d["resumable"], tc.resumable)
				}
				if tc.code == "merge_dispatch_failed" && (!strings.Contains(msg, "merge method squash") || strings.Contains(resp.Body.String(), "not_merge_ready")) {
					t.Fatalf("502 message = %q, want the resolved method named and no not_merge_ready", msg)
				}
			}
			puts := w.gh.putCalls()
			if len(puts) != tc.puts {
				t.Fatalf("PUTs = %+v, want %d", puts, tc.puts)
			}
			for _, put := range puts {
				if put.SHA != "h1" {
					t.Fatalf("PUT sha = %q, want h1", put.SHA)
				}
			}
			if e := p.calls(); len(e) != 0 {
				t.Fatalf("enables = %+v, want none", e)
			}
			st, _ := w.claim(r)
			if tc.merged {
				if st != string(mergequeue.DispatchDispatched) {
					t.Fatalf("claim = %q, want dispatched (merged)", st)
				}
			} else if st != "" {
				t.Fatalf("claim = %q, want none (refused before the claim, or released)", st)
			}
			settled := w.rows(r, categoryMergeDispatchSettled)
			if len(settled) != tc.settled {
				t.Fatalf("merge_dispatch_settled rows = %d, want %d", len(settled), tc.settled)
			}
			for _, e := range settled {
				if w.payload(e)["outcome"] != string(mergeDispatchRelease) {
					t.Fatalf("settled payload = %v, want released", w.payload(e))
				}
			}
			if n := len(w.rows(r, CategoryMergeVerdictRecorded)); n != tc.verdicts {
				t.Fatalf("merge_verdict_recorded rows = %d, want %d", n, tc.verdicts)
			}
			var merged bool
			w.gh.set(func() { merged = w.gh.prs[r.pr].merged })
			if merged != tc.merged {
				t.Fatalf("PR merged = %v, want %v", merged, tc.merged)
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
		p, merger := newMHPProxy(t, w, r)
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
		p, merger := newMHPProxy(t, w, r)
		arm.plant(p)
		requireObserveOnly(t, w.admqDrive(r, merger), autoDriveNoteHeadMovedReleaseFailed)
		requireHeadPinRefused(t, w, r, p, arm, mergequeue.DispatchPathDelegated)
		if got := mhpHeadMovedClaimOutcome(t, logs); got != string(mergeDispatchUncertain) {
			t.Fatalf("head-moved log claim_outcome = %q, want dispatch_failed (the settle outcome)", got)
		}
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
