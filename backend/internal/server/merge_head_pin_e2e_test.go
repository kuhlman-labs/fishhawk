package server

import (
	"bytes"
	"encoding/json"
	"io"
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
	// afterSnapshot runs once, after the first review-snapshot response is
	// written and before the caller reads it.
	afterSnapshot func()
	// onEnable runs once, on receipt of the enable mutation, before it is
	// judged.
	onEnable func()
	enables  []mhpEnable
}

func (p *mhpProxy) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
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
	p.rp.ServeHTTP(rw, req)
	if bytes.Contains(body, []byte("latestOpinionatedReviews")) {
		p.mu.Lock()
		hook := p.afterSnapshot
		p.afterSnapshot = nil
		p.mu.Unlock()
		if hook != nil {
			hook()
		}
	}
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
	default:
		call.Applied, call.AppliedHead = true, head
		_, _ = rw.Write([]byte(`{"data":{"enablePullRequestAutoMerge":{"pullRequest":{"number":1,"url":"u","state":"OPEN"}}}}`))
	}
	p.mu.Lock()
	p.enables = append(p.enables, call)
	p.mu.Unlock()
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
// mode answers the enable, and the refusal basis the forge reports.
type mhpArm struct {
	name  string
	mode  string
	basis string
	plant func(p *mhpProxy)
}

var mhpArms = []mhpArm{
	// The head moves after the fold's snapshot: the client pre-check sees h2
	// and sends no mutation.
	{"precheck", mhpRefuseOnMismatch, githubclient.HeadMismatchPrecheck, func(p *mhpProxy) {
		p.afterSnapshot = p.moveHead
	}},
	// The head moves on receipt of the pinned enable: the forge refuses it
	// (expectedHeadOid h1 vs head h2) and the client's re-read sees h2.
	{"graphql", mhpRefuseOnMismatch, githubclient.HeadMismatchGraphQLReread, func(p *mhpProxy) {
		p.onEnable = p.moveHead
	}},
	// The enable answers clean status, so the ladder takes the pinned REST
	// fallback; the head moves before the PUT is judged, so its sha h1 is
	// refused 409.
	{"rest", mhpCleanStatus, githubclient.HeadMismatchRESTSha, func(p *mhpProxy) {
		p.w.gh.set(func() { p.w.gh.onPut = func(int) { p.moveHead() } })
	}},
}

// requireHeadPinRefused asserts the committed state every head-moved row
// shares: the PR unmerged, no successful enable, the claim RELEASED (no claim
// row, one merge_dispatch_settled row outcome released at h1), and
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
	if st, _ := w.claim(r); st != "" {
		t.Fatalf("claim = %q, want released (no row)", st)
	}
	settled := w.rows(r, categoryMergeDispatchSettled)
	if len(settled) != 1 {
		t.Fatalf("merge_dispatch_settled rows = %d, want 1", len(settled))
	}
	sp := w.payload(settled[0])
	if sp["outcome"] != string(mergeDispatchRelease) || sp["path"] != path || sp["head_sha"] != "h1" {
		t.Fatalf("settled payload = %v, want released %s at h1", sp, path)
	}
	auth := w.rows(r, CategoryMergeQueueAuthorized)
	if len(auth) != 1 || w.payload(auth[0])["head_sha"] != sp["head_sha"] {
		t.Fatalf("merge_queue_authorized rows = %d (head %v); want one at the settled head %v", len(auth), auth, sp["head_sha"])
	}
}

// TestMergeHeadPinE2E_Inline: each arm answers 409 merge_head_moved naming h1
// and the arm's refusal_basis, with nothing merged and the claim released. The
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
// may_merge arm are observe-only autoDriveNoteHeadMoved with nothing merged
// and the claim released, so the next tick re-gates at the new head.
// Counterfactual: liveHead -> "" in dispatchAcceptanceGatedMerge flips the
// outcome to Acted (auto-merge enabled at h2, or h2 merged), RED.
func TestMergeHeadPinE2E_Delegated(t *testing.T) {
	for _, arm := range mhpArms {
		t.Run(arm.name, func(t *testing.T) {
			w := newMQMWorld(t)
			r := w.faeDelegated("dpin" + arm.name)
			w.forgeReviews(r, []string{"carol"}, mqmReview(1, "carol", "APPROVED", "h1"))
			p, merger := newMHPProxy(t, w, r, arm.mode)
			arm.plant(p)
			requireObserveOnly(t, w.admqDrive(r, merger), autoDriveNoteHeadMoved)
			requireHeadPinRefused(t, w, r, p, arm, mergequeue.DispatchPathDelegated)
		})
	}
}
