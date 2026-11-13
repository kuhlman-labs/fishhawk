//go:build unix

package server

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/policy"
	"github.com/kuhlman-labs/fishhawk/backend/internal/prompt"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/signing"
)

const tgtHead = "c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00"

var (
	specImplementGatingTwo   = bytes.Replace(specImplementGatingReviewers, []byte("agent: 1"), []byte("agent: 2"), 1)
	specImplementAdvisoryTwo = bytes.Replace(specImplementAdvisoryReviewers, []byte("agent: 1"), []byte("agent: 2"), 1)
)

// tgtBundle is an implement trace bundle. A non-empty head adds the verify_run
// that makes it the #797 dedup key. pushGated keeps the stage running after the
// trace, for the /pull-request report to drive.
func tgtBundle(t *testing.T, head string, pushGated bool) []byte {
	t.Helper()
	var raw bytes.Buffer
	line := func(seq int, kind string, data any) {
		b, err := json.Marshal(data)
		if err != nil {
			t.Fatalf("marshal %s: %v", kind, err)
		}
		out, _ := json.Marshal(map[string]any{"seq": seq, "kind": kind, "data": json.RawMessage(b)})
		raw.Write(out)
		raw.WriteByte('\n')
	}
	manifest := map[string]any{"bundle_schema": "v1"}
	if pushGated {
		manifest["push_to_shared_branch"] = true
	}
	line(1, "manifest", manifest)
	line(2, "git_diff", map[string]any{
		"kind": "name_status", "base_ref": "origin/main", "num_files": 1,
		"files": []map[string]string{{"path": "backend/internal/foo/foo.go", "status": "M"}},
	})
	if head != "" {
		line(3, "verify_run", map[string]any{"command": "go build ./...", "head_sha": head, "exit_code": 0, "output": "", "outcome": "passed"})
	}
	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	_, _ = w.Write(raw.Bytes())
	_ = w.Close()
	return gz.Bytes()
}

// traceSeam is one process's trace route over stores a next process shares.
type traceSeam struct {
	s      *Server
	sf     *signingFake
	au     *auditFake
	rr     *orchestratorRepo
	runRow *run.Run
	stage  *run.Stage
	rec    *pageClassRecorder
	body   []byte
	sig    string
	sent   [][]byte
}

func newTraceSeam(t *testing.T, set ReviewerSet, workflowSpec, body []byte) *traceSeam {
	t.Helper()
	s, sf, au, rr, runRow, stage := newImplementReviewServerWithSet(t, set, workflowSpec)
	au.stampSequence = true
	rec := &pageClassRecorder{}
	s.issueNotifier = rec
	priv, _ := sf.issue(t, runRow.ID)
	return &traceSeam{
		s: s, sf: sf, au: au, rr: rr, runRow: runRow, stage: stage, rec: rec, body: body,
		sig: hex.EncodeToString(ed25519.Sign(priv, signing.ComputeMessage(body))),
	}
}

// post sends the byte-identical signed upload to this seam's current server.
func (p *traceSeam) post(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	p.sent = append(p.sent, bytes.Clone(p.body))
	return shipRequest(t, p.s, p.runRow.ID, p.stage.ID, "raw", nil, p.sent[len(p.sent)-1], p.sig)
}

func (p *traceSeam) postAsync(t *testing.T) <-chan *httptest.ResponseRecorder {
	t.Helper()
	body := bytes.Clone(p.body)
	p.sent = append(p.sent, body)
	s, done := p.s, make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- shipRequest(t, s, p.runRow.ID, p.stage.ID, "raw", nil, body, p.sig) }()
	return done
}

// nextProcess reboots the seam: a new Server with a later boot marker.
func (p *traceSeam) nextProcess(set ReviewerSet) {
	old := p.s.cfg
	p.s = New(Config{
		Addr: "127.0.0.1:0", SigningRepo: p.sf, TraceStore: old.TraceStore, AuditRepo: p.au, RunRepo: p.rr,
		ArtifactRepo: old.ArtifactRepo, PlanReviewers: set, ProcessStart: time.Now().Add(time.Second),
	})
	p.s.issueNotifier = p.rec
}

func (p *traceSeam) state() run.StageState {
	p.rr.mu.Lock()
	defer p.rr.mu.Unlock()
	return p.stage.State
}

func (p *traceSeam) setState(to run.StageState) {
	p.rr.mu.Lock()
	defer p.rr.mu.Unlock()
	p.stage.State = to
}

func (p *traceSeam) count(category string) int { return countAuditCategory(p.au, category) }

// assertNoDecision: the stage stayed running with no failure, no calibration row
// and no terminal row.
func (p *traceSeam) assertNoDecision(t *testing.T) {
	t.Helper()
	if got := p.state(); got != run.StageStateRunning {
		t.Errorf("stage = %q, want running", got)
	}
	p.rr.mu.Lock()
	failure := p.stage.FailureCategory
	p.rr.mu.Unlock()
	if failure != nil {
		t.Errorf("failure category = %v, want none", *failure)
	}
	for _, category := range []string{"runtime_observed", "implement_review_failed", "implement_reviewed", "implement_review_skipped"} {
		if n := p.count(category); n != 0 {
			t.Errorf("%s rows = %d, want 0", category, n)
		}
	}
}

// assertReplayIdentical pins that every upload this seam sent was the same bytes.
func (p *traceSeam) assertReplayIdentical(t *testing.T) {
	t.Helper()
	if len(p.sent) < 2 {
		t.Fatalf("uploads sent = %d, want a replay", len(p.sent))
	}
	for i := 1; i < len(p.sent); i++ {
		if !bytes.Equal(p.sent[0], p.sent[i]) {
			t.Errorf("upload %d is not byte-identical to the first", i)
		}
	}
}

func awaitResponse(t *testing.T, done <-chan *httptest.ResponseRecorder, what string) *httptest.ResponseRecorder {
	t.Helper()
	select {
	case w := <-done:
		return w
	case <-time.After(boundedWait):
		t.Fatalf("%s: no response within %s", what, boundedWait)
		return nil
	}
}

// heldStep blocks until released or its context ends, then plays final.
func heldStep(started chan<- struct{}, release <-chan struct{}, final reviewStep) reviewStep {
	return func(ctx context.Context) (*planreview.ReviewVerdict, string, error) {
		close(started)
		select {
		case <-release:
			return final(ctx)
		case <-ctx.Done():
			return nil, "claude-sonnet-4-6", ctx.Err()
		}
	}
}

// killGatingTrace posts with a blocked gating reviewer, closes the lifetime and
// asserts the 503 and that the stage took no decision.
func killGatingTrace(t *testing.T, head string, pushGated bool) *traceSeam {
	t.Helper()
	started := make(chan struct{})
	rev := &scriptedReviewer{steps: []reviewStep{blockStep(started)}}
	p := newTraceSeam(t, singleReviewerSet{rev}, specImplementGatingReviewers, tgtBundle(t, head, pushGated))
	done := p.postAsync(t)
	waitFor(t, started, "setup: gating reviewer invoked")
	p.s.reviewers.closeAndCancel()
	assertInterrupted(t, awaitResponse(t, done, "killed gating upload"), http.StatusServiceUnavailable)
	p.assertNoDecision(t)
	return p
}

// --- trace route: interruption ----------------------------------------------

func TestShipTrace_GatingReviewerKilled_StageNotAdvanced(t *testing.T) {
	killGatingTrace(t, "", false)
}

func TestShipTrace_GatingRoundLostByRestart_RetryRefused(t *testing.T) {
	t.Run("replay_refused", func(t *testing.T) {
		p := killGatingTrace(t, tgtHead, false)
		next := &scriptedReviewer{steps: []reviewStep{approveStep}}
		p.nextProcess(singleReviewerSet{next})
		bootSweep(t, p.s)
		before := p.count("implement_review_started")

		assertInterrupted(t, p.post(t), http.StatusConflict)

		p.assertReplayIdentical(t)
		if got := p.state(); got != run.StageStateRunning {
			t.Errorf("stage = %q, want running", got)
		}
		if n := p.count("implement_review_started"); n != before {
			t.Errorf("implement_review_started = %d, want %d: a replay starts no round", n, before)
		}
		if n := next.calls.Load(); n != 0 {
			t.Errorf("next-process reviewer calls = %d, want 0", n)
		}
		if n := p.count("runtime_observed"); n != 0 {
			t.Errorf("runtime_observed rows = %d, want 0", n)
		}
	})
	t.Run("settled_round_control_dedups_and_advances", func(t *testing.T) {
		rev := &scriptedReviewer{steps: []reviewStep{approveStep}}
		p := newTraceSeam(t, singleReviewerSet{rev}, specImplementGatingReviewers, tgtBundle(t, tgtHead, false))
		if w := p.post(t); w.Code != http.StatusAccepted {
			t.Fatalf("first upload = %d:\n%s", w.Code, w.Body.String())
		}
		p.setState(run.StageStateRunning)
		w := p.post(t)
		if w.Code != http.StatusAccepted {
			t.Fatalf("same-process retry = %d, want 202:\n%s", w.Code, w.Body.String())
		}
		if got := p.state(); got != run.StageStateAwaitingApproval {
			t.Errorf("stage = %q, want awaiting_approval", got)
		}
		if n := rev.calls.Load(); n != 1 {
			t.Errorf("reviewer calls = %d, want 1", n)
		}
	})
	t.Run("settled_round_next_process_control_advances", func(t *testing.T) {
		rev := &scriptedReviewer{steps: []reviewStep{approveStep}}
		p := newTraceSeam(t, singleReviewerSet{rev}, specImplementGatingReviewers, tgtBundle(t, tgtHead, false))
		if w := p.post(t); w.Code != http.StatusAccepted {
			t.Fatalf("first upload = %d:\n%s", w.Code, w.Body.String())
		}
		p.setState(run.StageStateRunning)
		p.nextProcess(singleReviewerSet{rev})
		if w := p.post(t); w.Code != http.StatusAccepted {
			t.Fatalf("next-process retry = %d, want 202:\n%s", w.Code, w.Body.String())
		}
		if got := p.state(); got != run.StageStateAwaitingApproval {
			t.Errorf("stage = %q, want awaiting_approval", got)
		}
	})
	t.Run("advisory_same_head_control_stays_202", func(t *testing.T) {
		started, release := make(chan struct{}), make(chan struct{})
		rev := &scriptedReviewer{steps: []reviewStep{heldStep(started, release, approveStep)}}
		p := newTraceSeam(t, singleReviewerSet{rev}, specImplementAdvisoryReviewers, tgtBundle(t, tgtHead, true))
		t.Cleanup(func() { p.s.reviewers.closeAndCancel(); p.s.waitBackgroundReviews() })
		if w := p.post(t); w.Code != http.StatusAccepted {
			t.Fatalf("first upload = %d:\n%s", w.Code, w.Body.String())
		}
		waitFor(t, started, "setup: advisory reviewer invoked")
		next := &scriptedReviewer{steps: []reviewStep{approveStep}}
		a := p.s
		t.Cleanup(func() { a.reviewers.closeAndCancel(); a.waitBackgroundReviews() })
		p.nextProcess(singleReviewerSet{next})
		if w := p.post(t); w.Code != http.StatusAccepted {
			t.Fatalf("advisory retry = %d, want 202:\n%s", w.Code, w.Body.String())
		}
		if n := next.calls.Load(); n != 0 {
			t.Errorf("reviewer calls on the retry = %d, want 0", n)
		}
	})
}

func TestShipTrace_GatingRoundLost_EmptyHead_FreshRoundDecides(t *testing.T) {
	prepare := func(t *testing.T, final reviewStep) (*traceSeam, chan struct{}, chan struct{}, <-chan *httptest.ResponseRecorder) {
		t.Helper()
		// The prior process's lost round is seeded, so the dispatcher under test
		// runs only on the replay: no head means no #797 dedup key.
		started, release := make(chan struct{}), make(chan struct{})
		next := &scriptedReviewer{steps: []reviewStep{heldStep(started, release, final)}}
		p := newTraceSeam(t, singleReviewerSet{next}, specImplementGatingReviewers, tgtBundle(t, "", false))
		lost, _ := json.Marshal(planreview.ReviewStartedPayload{ConfiguredAgents: 1, Authority: planreview.AuthorityGating})
		if _, err := p.au.AppendChained(t.Context(), audit.ChainAppendParams{
			RunID: p.runRow.ID, StageID: &p.stage.ID, Timestamp: time.Now().Add(-time.Hour), Category: "implement_review_started", Payload: lost,
		}); err != nil {
			t.Fatalf("setup: seed the lost round: %v", err)
		}
		p.nextProcess(singleReviewerSet{next})
		bootSweep(t, p.s)
		before := p.count("implement_review_started")
		done := p.postAsync(t)
		waitFor(t, started, "fresh gating reviewer invoked on the replay")
		if n := p.count("implement_review_started"); n != before+1 {
			t.Errorf("implement_review_started = %d, want a fresh round after %d", n, before)
		}
		if got := p.state(); got != run.StageStateRunning {
			t.Errorf("stage while the fresh round blocks = %q, want running", got)
		}
		if n := p.count("runtime_observed"); n != 0 {
			t.Errorf("runtime_observed while the fresh round blocks = %d, want 0", n)
		}
		return p, started, release, done
	}
	t.Run("approve_advances_after_fresh_verdict", func(t *testing.T) {
		p, _, release, done := prepare(t, approveStep)
		close(release)
		if w := awaitResponse(t, done, "fresh round"); w.Code != http.StatusAccepted {
			t.Fatalf("status = %d, want 202:\n%s", w.Code, w.Body.String())
		}
		if got := p.state(); got != run.StageStateAwaitingApproval {
			t.Errorf("stage = %q, want awaiting_approval", got)
		}
		rounds := redispatchEntries(t, p.au, p.runRow.ID, "implement_review_started")
		verdicts := redispatchEntries(t, p.au, p.runRow.ID, "implement_reviewed")
		if len(verdicts) != 1 || verdicts[0].Sequence <= rounds[len(rounds)-1].Sequence {
			t.Errorf("verdict must be sequenced after the fresh round: verdicts=%d rounds=%d", len(verdicts), len(rounds))
		}
	})
	t.Run("reject_fails_category_b", func(t *testing.T) {
		p, _, release, done := prepare(t, rejectStep)
		close(release)
		if w := awaitResponse(t, done, "fresh round"); w.Code != http.StatusAccepted {
			t.Fatalf("status = %d, want 202:\n%s", w.Code, w.Body.String())
		}
		p.rr.mu.Lock()
		defer p.rr.mu.Unlock()
		if p.stage.State != run.StageStateFailed || p.stage.FailureCategory == nil || *p.stage.FailureCategory != run.FailureB {
			t.Errorf("stage = %q / %v, want failed category B", p.stage.State, p.stage.FailureCategory)
		}
	})
	t.Run("kill_answers_503", func(t *testing.T) {
		p, _, _, done := prepare(t, approveStep)
		p.s.reviewers.closeAndCancel()
		assertInterrupted(t, awaitResponse(t, done, "killed fresh round"), http.StatusServiceUnavailable)
		if got := p.state(); got != run.StageStateRunning {
			t.Errorf("stage = %q, want running", got)
		}
	})
}

func TestShipTrace_CurrentProcessRoundAfterClose_RetryRefused(t *testing.T) {
	p := killGatingTrace(t, tgtHead, false)
	before := p.count("implement_review_started")
	assertInterrupted(t, p.post(t), http.StatusServiceUnavailable)
	p.assertReplayIdentical(t)
	p.assertNoDecision(t)
	if n := p.count("implement_review_started"); n != before {
		t.Errorf("implement_review_started = %d, want %d: no second round", n, before)
	}
}

func TestShipTrace_ClosingProcess_SettledRound_RetryStillDedups(t *testing.T) {
	rev := &scriptedReviewer{steps: []reviewStep{approveStep}}
	p := newTraceSeam(t, singleReviewerSet{rev}, specImplementGatingReviewers, tgtBundle(t, tgtHead, false))
	if w := p.post(t); w.Code != http.StatusAccepted {
		t.Fatalf("first upload = %d:\n%s", w.Code, w.Body.String())
	}
	p.setState(run.StageStateRunning)
	p.s.reviewers.closeAndCancel()
	w := p.post(t)
	if w.Code != http.StatusAccepted {
		t.Fatalf("closing-process retry = %d, want 202:\n%s", w.Code, w.Body.String())
	}
	if got := p.state(); got != run.StageStateAwaitingApproval {
		t.Errorf("stage = %q, want awaiting_approval", got)
	}
	if n := p.count("implement_review_started"); n != 1 {
		t.Errorf("implement_review_started = %d, want 1", n)
	}
}

func TestShipTrace_ClosingProcess_EmptyHead_RefusedNoReviewer(t *testing.T) {
	rev := &scriptedReviewer{steps: []reviewStep{approveStep}}
	p := newTraceSeam(t, singleReviewerSet{rev}, specImplementGatingReviewers, tgtBundle(t, "", false))
	p.s.reviewers.closeAndCancel()
	assertInterrupted(t, p.post(t), http.StatusServiceUnavailable)
	if n := rev.calls.Load(); n != 0 {
		t.Errorf("reviewer calls = %d, want 0", n)
	}
	p.assertNoDecision(t)
}

// orderProbeAudit records, at each read of the two categories the #797 branch
// and the lost-round check read, whether reviewDispatchMu was free and how many
// exports were still on disk.
type orderProbeAudit struct {
	*auditFake
	dir     string
	t       *testing.T
	started []orderProbe
	lost    []orderProbe
}

type orderProbe struct {
	unlocked bool
	exports  int
}

func (a *orderProbeAudit) ListForRunByCategory(ctx context.Context, runID uuid.UUID, category string) ([]*audit.Entry, error) {
	if category == "implement_review_started" || category == "implement_reviewed" {
		unlocked := reviewDispatchMu.TryLock()
		if unlocked {
			reviewDispatchMu.Unlock()
		}
		probe := orderProbe{unlocked: unlocked, exports: len(reviewTreeExports(a.t, a.dir))}
		if category == "implement_review_started" {
			a.started = append(a.started, probe)
		} else {
			a.lost = append(a.lost, probe)
		}
	}
	return a.auditFake.ListForRunByCategory(ctx, runID, category)
}

func TestSameHeadGatingRoundLost_ReadsAfterUnlockAndCleanup(t *testing.T) {
	scratch := t.TempDir()
	t.Setenv("TMPDIR", scratch)
	repo, headSHA := gitFixtureRepo(t)
	reviewer := &groundingFakeReviewer{verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictApprove}, model: "gpt-5.5"}
	s, _, au, _, runRow, stage := newImplementReviewServer(t, reviewer, specImplementGatingReviewers)
	au.stampSequence = true
	runRow.WorkingDir = repo
	payload, _ := json.Marshal(planreview.ReviewStartedPayload{ConfiguredAgents: 1, Authority: planreview.AuthorityGating, HeadSHA: headSHA})
	if _, err := au.AppendChained(t.Context(), audit.ChainAppendParams{
		RunID: runRow.ID, StageID: &stage.ID, Timestamp: time.Now().Add(-time.Hour), Category: "implement_review_started", Payload: payload,
	}); err != nil {
		t.Fatalf("seed started: %v", err)
	}
	probe := &orderProbeAudit{auditFake: au, dir: scratch, t: t}
	s.cfg.AuditRepo = probe

	outcome := s.runImplementReviewsForTree(t.Context(), runRow.ID, stage.ID, policy.Diff{}, nil, headSHA, "", "", nil)

	if !outcome.interrupted {
		t.Fatalf("outcome = %+v, want interrupted", outcome)
	}
	if len(probe.started) == 0 || !slices.ContainsFunc(probe.started, func(p orderProbe) bool { return !p.unlocked && p.exports > 0 }) {
		t.Fatalf("setup: the dedup read never saw the held lock and a live export: %+v", probe.started)
	}
	if len(probe.lost) == 0 {
		t.Fatal("the lost-round check read no terminal category")
	}
	last := probe.lost[len(probe.lost)-1]
	if !last.unlocked {
		t.Error("the lost-round read ran under reviewDispatchMu")
	}
	if last.exports != 0 {
		t.Errorf("the lost-round read ran with %d export(s) still on disk", last.exports)
	}
	if n := len(reviewTreeExports(t, scratch)); n != 0 {
		t.Errorf("exports after return = %d, want 0", n)
	}
}

// --- nothing stranded: a real listener and a real Shutdown ------------------

// assertNothingStranded: export gone, both dispatch locks free, a re-run claimable,
// the background group drained.
func assertNothingStranded(t *testing.T, s *Server, runID uuid.UUID, scratch string) {
	t.Helper()
	if n := len(reviewTreeExports(t, scratch)); n != 0 {
		t.Errorf("exports left on disk = %d, want 0", n)
	}
	if !reviewDispatchMu.TryLock() {
		t.Error("reviewDispatchMu is still held")
	} else {
		reviewDispatchMu.Unlock()
	}
	stripe := reconcileEmitLockFor(runID)
	if !stripe.TryLock() {
		t.Error("the run's reconcile stripe is still held")
	} else {
		stripe.Unlock()
	}
	if !claimRerun(runID) {
		t.Error("the run's re-run claim is still held")
	} else {
		releaseRerun(runID)
	}
	drained := make(chan struct{})
	go func() { s.bgReviews.Wait(); close(drained) }()
	waitFor(t, drained, "background reviews drained")
}

// assertReviewerHeldExport proves the stranded-export check is not vacuous.
func assertReviewerHeldExport(t *testing.T, reviewer *groundingFakeReviewer) {
	t.Helper()
	reviewer.mu.Lock()
	defer reviewer.mu.Unlock()
	if reviewer.treeDir == "" || !reviewer.dirExistedAtReview {
		t.Fatalf("setup: the reviewer held no export: tree=%q existed=%v", reviewer.treeDir, reviewer.dirExistedAtReview)
	}
}

// servedUpload posts to a real listener and runs a real Shutdown once the reviewer
// has started, returning the client's status and code.
func servedUpload(t *testing.T, s *Server, url, sig, planSeed string, body []byte, started <-chan struct{}) int {
	t.Helper()
	setVar(t, &reviewerShutdownDrain, 50*time.Millisecond)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("setup: listen: %v", err)
	}
	served := make(chan struct{})
	go func() { defer close(served); _ = s.http.Serve(ln) }()
	codes := make(chan int, 1)
	go func() {
		req, _ := http.NewRequest(http.MethodPost, "http://"+ln.Addr().String()+url, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Fishhawk-Signature", sig)
		if planSeed != "" {
			req.Header.Set(planSeedHeader, planSeed)
		}
		resp, rerr := http.DefaultClient.Do(req)
		if rerr != nil {
			codes <- -1
			return
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		codes <- resp.StatusCode
	}()
	waitFor(t, started, "setup: reviewer invoked")
	shutdownDone := make(chan error, 1)
	begin := time.Now()
	go func() { shutdownDone <- s.Shutdown(context.Background()) }()
	var code int
	select {
	case code = <-codes:
	case <-time.After(boundedWait):
		t.Fatalf("client got no answer within %s", boundedWait)
	}
	select {
	case serr := <-shutdownDone:
		if serr != nil {
			t.Errorf("Shutdown = %v, want nil", serr)
		}
		if took := time.Since(begin); took >= s.cfg.ShutdownTimeout {
			t.Errorf("Shutdown took %s, want before ShutdownTimeout %s", took, s.cfg.ShutdownTimeout)
		}
	case <-time.After(boundedWait):
		t.Fatalf("Shutdown did not return within %s", boundedWait)
	}
	waitFor(t, served, "listener closed")
	return code
}

func TestShutdown_GatingRoundInterrupted_NothingStranded(t *testing.T) {
	t.Run("trace", func(t *testing.T) {
		scratch := t.TempDir()
		t.Setenv("TMPDIR", scratch)
		repo, headSHA := gitFixtureRepo(t)
		started := make(chan struct{})
		reviewer := &groundingFakeReviewer{blockOnCtx: true, started: started}
		s, sf, _, rr, runRow, stage := newImplementReviewServer(t, reviewer, specImplementGatingReviewers)
		runRow.WorkingDir = repo
		priv, _ := sf.issue(t, runRow.ID)
		body := tgtBundle(t, headSHA, false)
		code := servedUpload(t, s, fmt.Sprintf("/v0/runs/%s/trace?stage_id=%s&variant=raw", runRow.ID, stage.ID),
			hex.EncodeToString(ed25519.Sign(priv, signing.ComputeMessage(body))), "", body, started)
		if code != http.StatusServiceUnavailable {
			t.Errorf("client status = %d, want 503", code)
		}
		assertReviewerHeldExport(t, reviewer)
		rr.mu.Lock()
		state := stage.State
		rr.mu.Unlock()
		if state != run.StageStateRunning {
			t.Errorf("stage = %q, want running", state)
		}
		assertNothingStranded(t, s, runRow.ID, scratch)
	})
	t.Run("plan", func(t *testing.T) {
		scratch := t.TempDir()
		t.Setenv("TMPDIR", scratch)
		repo, headSHA := gitFixtureRepo(t)
		started := make(chan struct{})
		reviewer := &groundingFakeReviewer{blockOnCtx: true, started: started}
		runID, stageID := uuid.New(), uuid.New()
		s, sf, _, _, rr := newPlanServerWithReviewer(t, runID, stageID, reviewer, specGatingReviewers)
		rr.getRuns[runID].WorkingDir = repo
		st := rr.getStages[stageID]
		st.Type, st.State, st.RequiresApproval = run.StageTypePlan, run.StageStateRunning, true
		rr.stagesByRunID = map[uuid.UUID][]*run.Stage{runID: {st}}
		priv, _ := sf.issue(t, runID)
		body := validPlanBytes(t)
		code := servedUpload(t, s, fmt.Sprintf("/v0/runs/%s/plan?stage_id=%s", runID, stageID),
			hex.EncodeToString(ed25519.Sign(priv, signing.ComputeMessage(body))), headSHA, body, started)
		if code != http.StatusServiceUnavailable {
			t.Errorf("client status = %d, want 503", code)
		}
		assertReviewerHeldExport(t, reviewer)
		assertNothingStranded(t, s, runID, scratch)
	})
}

// --- advisory rounds stopped by shutdown ------------------------------------

// settlementProbeAudit counts reads only the post-loop passes make: the release's
// triage read and the audit-complete recompute's acceptance read.
type settlementProbeAudit struct {
	*auditFake
	mu    sync.Mutex
	reads map[string]int
}

func newSettlementProbe(au *auditFake) *settlementProbeAudit {
	return &settlementProbeAudit{auditFake: au, reads: map[string]int{}}
}

func (a *settlementProbeAudit) ListForRunByCategory(ctx context.Context, runID uuid.UUID, category string) ([]*audit.Entry, error) {
	a.mu.Lock()
	a.reads[category]++
	a.mu.Unlock()
	return a.auditFake.ListForRunByCategory(ctx, runID, category)
}

func (a *settlementProbeAudit) readsOf(category string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.reads[category]
}

// assertNoSettlement: no post-loop pass ran.
func assertNoSettlement(t *testing.T, probe *settlementProbeAudit, rec *pageClassRecorder, cr *fakeConcernRepo, runID uuid.UUID) {
	t.Helper()
	if n := probe.readsOf(CategoryAcceptanceTriageDecided); n != 0 {
		t.Errorf("deferred acceptance release read the triage category %d times, want 0", n)
	}
	if n := probe.readsOf("acceptance_skipped_out_of_scope"); n != 0 {
		t.Errorf("audit-complete recompute ran: its acceptance read fired %d times", n)
	}
	if rec != nil && len(rec.pageClass) != 0 {
		t.Errorf("page-class pings = %d, want 0", len(rec.pageClass))
	}
	if cr != nil {
		rows, _ := cr.ListByRun(context.Background(), runID)
		for _, row := range rows {
			if row.State != concern.StateAddressedPending {
				t.Errorf("seeded concern state = %q, want addressed_pending", row.State)
			}
		}
	}
}

func seedAddressedConcern(cr *fakeConcernRepo, runID, stageID uuid.UUID) {
	cr.mu.Lock()
	defer cr.mu.Unlock()
	cr.rows = append(cr.rows, &concern.Concern{
		ID: uuid.New(), RunID: runID, StageID: stageID, StageKind: concern.StageKindImplement,
		State: concern.StateAddressedPending, Category: "scope", Note: "seeded",
	})
}

// groundedScripted is a scripted reviewer that can ground, recording the export it
// was handed so a test can prove the export existed before it was removed.
type groundedScripted struct {
	*scriptedReviewer
	mu   sync.Mutex
	tree string
}

func (r *groundedScripted) ReviewGrounded(ctx context.Context, promptText, treeDir string) (*planreview.ReviewVerdict, string, error) {
	r.mu.Lock()
	if treeDir != "" {
		r.tree = treeDir
	}
	r.mu.Unlock()
	return r.Review(ctx, promptText)
}

// assertExportRemoved: the reviewer really held an export, and it is gone.
func assertExportRemoved(t *testing.T, r *groundedScripted, scratch string) {
	t.Helper()
	r.mu.Lock()
	tree := r.tree
	r.mu.Unlock()
	if tree == "" {
		t.Fatal("setup: the reviewer was handed no export")
	}
	if n := len(reviewTreeExports(t, scratch)); n != 0 {
		t.Errorf("exports left on disk = %d, want 0", n)
	}
}

// The held arm drives its loop directly with an empty tree: a conventions round's
// export is owned by the dispatcher the streaming arm already covers.
func TestShutdown_KilledReviewer_WritesNoTerminal_NextBootRedispatches(t *testing.T) {
	t.Run("plan", func(t *testing.T) {
		scratch := t.TempDir()
		t.Setenv("TMPDIR", scratch)
		repo, planHead := gitFixtureRepo(t)
		started := make(chan struct{})
		rev := &groundedScripted{scriptedReviewer: &scriptedReviewer{steps: []reviewStep{blockStep(started), approveStep}}}
		f := newRedispatchFixture(t, redispatchFixtureOpts{planStageState: run.StageStateRunning})
		f.runRow.WorkingDir = repo
		f.s.cfg.PlanReviewers = singleReviewerSet{rev}
		f.s.cfg.ProcessStart = time.Now()
		probe := newSettlementProbe(f.au)
		f.s.cfg.AuditRepo = probe
		rec := &pageClassRecorder{}
		f.s.issueNotifier = rec
		sf := newSigningFake()
		f.s.cfg.SigningRepo = sf
		priv, _ := sf.issue(t, f.runRow.ID)
		// The handler reads SigningRepo from the wired config at request time.
		if w := shipPlanRequestWithSeed(t, f.s, f.runRow.ID, f.planStage.ID, priv, validPlanBytes(t), planHead); w.Code != http.StatusCreated {
			t.Fatalf("setup: plan upload = %d:\n%s", w.Code, w.Body.String())
		}
		waitFor(t, started, "setup: first reviewer invoked")
		f.s.reviewers.closeAndCancel()
		f.s.waitBackgroundReviews()
		orphaned := redispatchEntries(t, f.au, f.runRow.ID, "plan_review_started")
		if len(orphaned) != 1 {
			t.Fatalf("setup: plan_review_started = %d, want 1", len(orphaned))
		}
		for _, category := range []string{"plan_review_failed", "plan_reviewed"} {
			if n := countAuditCategory(f.au, category); n != 0 {
				t.Errorf("%s rows = %d, want 0", category, n)
			}
		}
		if n := rev.calls.Load(); n != 1 {
			t.Errorf("reviewer calls = %d, want 1: the second reviewer never runs", n)
		}
		if len(rec.pageClass) != 0 {
			t.Errorf("page-class pings = %d, want 0", len(rec.pageClass))
		}
		assertExportRemoved(t, rev, scratch)
		next := &fakePlanReviewer{verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictApprove}, model: "claude-opus-4-8"}
		b := New(Config{Addr: "127.0.0.1:0", AuditRepo: f.au, RunRepo: f.rr, ArtifactRepo: f.art, PlanReviewers: singleReviewerSet{next}, ProcessStart: time.Now().Add(time.Second)})
		bootSweep(t, b)
		assertRedispatchedRound(t, f.au, f.runRow.ID, orphanedReviewStages[0], orphaned[0].Sequence, 2)
	})
	t.Run("implement_streaming", func(t *testing.T) {
		scratch := t.TempDir()
		t.Setenv("TMPDIR", scratch)
		repo, implHead := gitFixtureRepo(t)
		started := make(chan struct{})
		rev := &groundedScripted{scriptedReviewer: &scriptedReviewer{steps: []reviewStep{blockStep(started), approveStep}}}
		ts := newRedispatchTraceStore()
		p := newTraceSeam(t, singleReviewerSet{rev}, specImplementAdvisoryTwo, tgtBundle(t, implHead, true))
		p.runRow.WorkingDir = repo
		p.s.cfg.TraceStore = ts
		probe := newSettlementProbe(p.au)
		p.s.cfg.AuditRepo = probe
		cr := newFakeConcernRepo()
		seedAddressedConcern(cr, p.runRow.ID, p.stage.ID)
		p.s.cfg.ConcernRepo = cr
		for _, variant := range []string{"raw", "redacted"} {
			if w := shipRequest(t, p.s, p.runRow.ID, p.stage.ID, variant, nil, p.body, p.sig); w.Code != http.StatusAccepted {
				t.Fatalf("setup: %s upload = %d:\n%s", variant, w.Code, w.Body.String())
			}
		}
		waitFor(t, started, "setup: first reviewer invoked")
		p.s.reviewers.closeAndCancel()
		p.s.waitBackgroundReviews()
		orphaned := redispatchEntries(t, p.au, p.runRow.ID, "implement_review_started")
		if len(orphaned) != 1 {
			t.Fatalf("setup: implement_review_started = %d, want 1", len(orphaned))
		}
		for _, category := range []string{"implement_review_failed", "implement_reviewed"} {
			if n := p.count(category); n != 0 {
				t.Errorf("%s rows = %d, want 0", category, n)
			}
		}
		if n := rev.calls.Load(); n != 1 {
			t.Errorf("reviewer calls = %d, want 1: the second reviewer never runs", n)
		}
		assertNoSettlement(t, probe, p.rec, cr, p.runRow.ID)
		assertExportRemoved(t, rev, scratch)
		next := &fakePlanReviewer{verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictApprove}, model: "claude-opus-4-8"}
		p.nextProcess(singleReviewerSet{next})
		p.s.cfg.TraceStore = ts
		bootSweep(t, p.s)
		assertRedispatchedRound(t, p.au, p.runRow.ID, orphanedReviewStages[1], orphaned[0].Sequence, 2)
	})
	t.Run("implement_held", func(t *testing.T) {
		f := newRedispatchFixture(t, redispatchFixtureOpts{traceStore: newRedispatchTraceStore()})
		f.seedPlanArtifact(t, validPlanBytes(t), "approved-plan", true)
		f.seedRedactedBundle(t, tgtBundle(t, tgtHead, true))
		seq := f.appendAt(t, f.implStage.ID, beforeBoot, "implement_review_started", advisoryImplementStarted(reviewRoundOriginTrace, "", tgtHead))
		started := make(chan struct{})
		first := &scriptedReviewer{steps: []reviewStep{blockStep(started)}}
		second := &scriptedReviewer{steps: []reviewStep{approveStep}}
		probe := newSettlementProbe(f.au)
		a := New(Config{Addr: "127.0.0.1:0", AuditRepo: probe, RunRepo: f.rr, ArtifactRepo: f.art, TraceStore: f.s.cfg.TraceStore})
		rec := &pageClassRecorder{}
		a.issueNotifier = rec
		outcomes := make(chan reviewLoopOutcome, 1)
		go func() {
			outcomes <- a.runImplementReviewLoopOutcome(t.Context(), f.runRow.ID, f.implStage.ID, []reviewerInvocation{{reviewer: first}, {reviewer: second}},
				planreview.AuthorityAdvisory, "prompt", "author", "", "", "", planreview.DefaultReviewBudget, "", seq, reviewConventionRound{ModifiedFiles: []string{"docs/conventions/go.md"}})
		}()
		waitFor(t, started, "setup: first reviewer invoked")
		a.reviewers.closeAndCancel()
		select {
		case outcome := <-outcomes:
			if !outcome.interrupted {
				t.Errorf("outcome = %+v, want interrupted", outcome)
			}
		case <-time.After(boundedWait):
			t.Fatal("the held loop did not return")
		}
		if n := second.calls.Load(); n != 0 {
			t.Errorf("second reviewer calls = %d, want 0", n)
		}
		for _, category := range []string{"implement_review_failed", "implement_reviewed"} {
			if n := countAuditCategory(f.au, category); n != 0 {
				t.Errorf("%s rows = %d, want 0", category, n)
			}
		}
		assertNoSettlement(t, probe, rec, nil, f.runRow.ID)
		bootSweep(t, f.s)
		assertRedispatchedRound(t, f.au, f.runRow.ID, orphanedReviewStages[1], seq, 1)
	})
}

func TestShutdown_StreamingRound_LandedVerdictThenKill(t *testing.T) {
	started := make(chan struct{})
	rev := &scriptedReviewer{steps: []reviewStep{rejectStep, blockStep(started)}}
	ts := newRedispatchTraceStore()
	p := newTraceSeam(t, singleReviewerSet{rev}, specImplementAdvisoryTwo, tgtBundle(t, tgtHead, true))
	p.s.cfg.TraceStore = ts
	probe := newSettlementProbe(p.au)
	p.s.cfg.AuditRepo = probe
	for _, variant := range []string{"raw", "redacted"} {
		if w := shipRequest(t, p.s, p.runRow.ID, p.stage.ID, variant, nil, p.body, p.sig); w.Code != http.StatusAccepted {
			t.Fatalf("setup: %s upload = %d:\n%s", variant, w.Code, w.Body.String())
		}
	}
	waitFor(t, started, "setup: second reviewer invoked")
	p.s.reviewers.closeAndCancel()
	p.s.waitBackgroundReviews()
	if n := p.count("implement_reviewed"); n != 1 {
		t.Errorf("landed verdicts = %d, want 1", n)
	}
	if n := p.count("implement_review_failed"); n != 0 {
		t.Errorf("implement_review_failed = %d, want 0 for the killed reviewer", n)
	}
	assertNoSettlement(t, probe, p.rec, nil, p.runRow.ID)
	orphaned := redispatchEntries(t, p.au, p.runRow.ID, "implement_review_started")
	next := &fakePlanReviewer{verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictApprove}, model: "claude-opus-4-8"}
	p.nextProcess(singleReviewerSet{next})
	p.s.cfg.TraceStore = ts
	bootSweep(t, p.s)
	assertRedispatchedRound(t, p.au, p.runRow.ID, orphanedReviewStages[1], orphaned[0].Sequence, 2)
}

func TestSupplementalAdvisoryKilled_NoFabricatedVerdict(t *testing.T) {
	started := make(chan struct{})
	rev := &scriptedReviewer{steps: []reviewStep{blockStep(started)}}
	s, _, au, _, runRow, stage := newImplementReviewServer(t, rev, specImplementAdvisoryReviewers)
	au.stampSequence = true
	seedFirstReviewRound(t, au, runRow.ID, stage.ID)
	outcome := s.dispatchSupplementalReinvokeReview(t.Context(), runRow.ID, stage.ID, tgtHead, "ignored-advisory-attempt", supplementalExemptions())
	if outcome != (reviewDispatchOutcome{}) {
		t.Errorf("advisory outcome = %+v, want empty", outcome)
	}
	waitFor(t, started, "setup: advisory reviewer invoked")
	s.reviewers.closeAndCancel()
	s.waitBackgroundReviews()
	if n := countAuditCategory(au, "implement_review_failed"); n != 0 {
		t.Errorf("implement_review_failed = %d, want 0", n)
	}
	if n := countAuditCategory(au, "implement_reviewed"); n != 1 {
		t.Errorf("implement_reviewed = %d, want only the first round's 1", n)
	}
	if n := countAuditCategory(au, "implement_review_started"); n != 1 {
		t.Errorf("implement_review_started = %d, want the first round's 1", n)
	}
	if findSupplementalImplementReviewed(t, au, stage.ID) != nil {
		t.Error("a supplemental verdict was fabricated for the killed reviewer")
	}
}

func TestPostSuccessRounds_Interrupted_NoStageEdge(t *testing.T) {
	const base, head = "base-pass", "head-pushed"
	for _, round := range []struct {
		name string
		run  func(f *redispatchFixture)
	}{
		{"consolidated", func(f *redispatchFixture) {
			comparer, scope, repo, reason := f.s.forgeCompareFor(f.runRow)
			if reason != "" {
				panic("setup: forge compare not wired: " + reason)
			}
			f.s.bgReviews.Add(1)
			go func() {
				defer f.s.bgReviews.Done()
				_, _ = f.s.runConsolidatedReviewRound(context.Background(), f.runRow, f.implStage.ID, base, head, comparer, scope, repo)
			}()
		}},
		{"fixup_backstop", func(f *redispatchFixture) {
			f.s.maybeBackstopFixupReReview(context.Background(), f.runRow.ID, f.implStage, head, base)
		}},
	} {
		for _, authority := range []struct {
			name string
			spec []byte
		}{{"gating", specImplementGatingReviewers}, {"advisory", specImplementAdvisoryReviewers}} {
			t.Run(round.name+"/"+authority.name, func(t *testing.T) {
				gh, _ := recordingCompareClient(t, cannedCompareOneFile)
				f := newRedispatchFixture(t, redispatchFixtureOpts{github: gh, spec: authority.spec})
				f.seedPlanArtifact(t, validPlanBytes(t), "approved-plan", true)
				f.s.cfg.ProcessStart = time.Now()
				started := make(chan struct{})
				f.s.cfg.PlanReviewers = singleReviewerSet{&scriptedReviewer{steps: []reviewStep{blockStep(started)}}}
				round.run(f)
				waitFor(t, started, "setup: reviewer invoked")
				f.s.reviewers.closeAndCancel()
				f.s.waitBackgroundReviews()
				if f.implStage.State != run.StageStateRunning {
					t.Errorf("stage = %q, want running", f.implStage.State)
				}
				for _, category := range []string{"implement_review_failed", "implement_reviewed"} {
					if n := countAuditCategory(f.au, category); n != 0 {
						t.Errorf("%s rows = %d, want 0", category, n)
					}
				}
				orphaned := redispatchEntries(t, f.au, f.runRow.ID, "implement_review_started")
				if len(orphaned) != 1 {
					t.Fatalf("setup: implement_review_started = %d, want 1", len(orphaned))
				}
				completing := &fakePlanReviewer{verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictApprove}, model: "claude-opus-4-8"}
				b := New(Config{Addr: "127.0.0.1:0", AuditRepo: f.au, RunRepo: f.rr, ArtifactRepo: f.art, GitHub: gh, PlanReviewers: singleReviewerSet{completing}, ProcessStart: time.Now().Add(time.Second)})
				bootSweep(t, b)
				if authority.name == "gating" {
					reasons := failedReasonsAfter(t, f.au, f.runRow.ID, "implement_review_failed", orphaned[0].Sequence)
					if len(reasons) != 1 || !strings.Contains(reasons[0], "not re-dispatched: gating_authority") {
						t.Errorf("closure reasons = %v, want one naming gating_authority", reasons)
					}
					if f.implStage.State != run.StageStateRunning {
						t.Errorf("stage after boot = %q, want running", f.implStage.State)
					}
					return
				}
				assertRedispatchedRound(t, f.au, f.runRow.ID, orphanedReviewStages[1], orphaned[0].Sequence, 1)
			})
		}
	}
}

// --- supplemental dispatcher ------------------------------------------------

// tgtAudit is an audit fake with injectable write and read faults that also carries
// the atomic start capability, so it takes the production assertion.
type tgtAudit struct {
	*auditFake
	mu      sync.Mutex
	onWrite func(ctx context.Context, p audit.ChainAppendParams, n int) flakyAction
	onRead  func(category string) error
	onDedup func(p audit.ChainAppendParams) (*audit.Entry, error)
	writes  map[string]int
	marks   map[string]bool
	reads   map[string]int
	dedups  int
}

func newTGTAudit(au *auditFake) *tgtAudit {
	return &tgtAudit{auditFake: au, writes: map[string]int{}, marks: map[string]bool{}, reads: map[string]int{}}
}

func (a *tgtAudit) AppendChained(ctx context.Context, p audit.ChainAppendParams) (*audit.Entry, error) {
	a.mu.Lock()
	a.writes[p.Category]++
	n := a.writes[p.Category]
	if reviewTerminalWritesBound(ctx) != nil {
		a.marks[p.Category] = true
	}
	hook := a.onWrite
	a.mu.Unlock()
	act := flakyOK
	if hook != nil {
		act = hook(ctx, p, n)
	}
	switch act {
	case flakyFail:
		return nil, errors.New("injected write failure")
	case flakyBlock:
		<-ctx.Done()
		return nil, ctx.Err()
	}
	entry, err := a.auditFake.AppendChained(ctx, p)
	if err != nil {
		return nil, err
	}
	if act == flakyCommitThenFail {
		return nil, errors.New("committed but the ack was lost")
	}
	return entry, nil
}

func (a *tgtAudit) ListForRunByCategory(ctx context.Context, runID uuid.UUID, category string) ([]*audit.Entry, error) {
	a.mu.Lock()
	a.reads[category]++
	hook := a.onRead
	a.mu.Unlock()
	if hook != nil {
		if err := hook(category); err != nil {
			return nil, err
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return a.auditFake.ListForRunByCategory(ctx, runID, category)
}

func (a *tgtAudit) AppendChainedDeduped(ctx context.Context, p audit.ChainAppendParams, spec audit.DedupeSpec) (*audit.Entry, error) {
	a.mu.Lock()
	a.dedups++
	a.mu.Unlock()
	if a.onDedup != nil {
		return a.onDedup(p)
	}
	return testDedupedAppender{a}.AppendChainedDeduped(ctx, p, spec)
}

func (a *tgtAudit) setHooks(onWrite func(context.Context, audit.ChainAppendParams, int) flakyAction, onRead func(string) error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.onWrite, a.onRead = onWrite, onRead
}

func (a *tgtAudit) readsOf(category string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.reads[category]
}

// passSeam is a dispatcher fixture on a running implement stage whose attempt
// token is real.
type passSeam struct {
	s       *Server
	au      *auditFake
	audit   *tgtAudit
	rr      *orchestratorRepo
	runRow  *run.Run
	stage   *run.Stage
	attempt string
	rev     *scriptedReviewer
}

func newPassSeam(t *testing.T, workflowSpec []byte, steps ...reviewStep) *passSeam {
	t.Helper()
	rev := &scriptedReviewer{steps: steps}
	s, _, au, rr, runRow, stage := newImplementReviewServer(t, rev, workflowSpec)
	au.stampSequence = true
	stage.State = run.StageStateRunning
	wrapped := newTGTAudit(au)
	s.cfg.AuditRepo = wrapped
	return &passSeam{s: s, au: au, audit: wrapped, rr: rr, runRow: runRow, stage: stage, attempt: run.StageAttemptToken(stage.DispatchedAt), rev: rev}
}

func (p *passSeam) dispatch(t *testing.T) reviewDispatchOutcome {
	t.Helper()
	return p.s.dispatchSupplementalReinvokeReview(t.Context(), p.runRow.ID, p.stage.ID, tgtHead, p.attempt, supplementalExemptions())
}

func (p *passSeam) seedStart(t *testing.T, attempt string, agents int) {
	t.Helper()
	start := startRow(t, p.stage.ID, 0, tgtHead, attempt, agents, "backend/internal/foo/foo.go")
	p.seed(t, start.Category, start.Payload)
}

func (p *passSeam) seedVerdict(t *testing.T, attempt string, verdict planreview.Verdict) {
	t.Helper()
	row := verdictRow(t, p.stage.ID, 0, tgtHead, attempt, verdict)
	p.seed(t, row.Category, row.Payload)
}

func (p *passSeam) seed(t *testing.T, category string, payload []byte) {
	t.Helper()
	if _, err := p.au.AppendChained(t.Context(), audit.ChainAppendParams{RunID: p.runRow.ID, StageID: &p.stage.ID, Timestamp: time.Now().UTC(), Category: category, Payload: payload}); err != nil {
		t.Fatalf("setup: seed %s: %v", category, err)
	}
}

func (p *passSeam) startRows() int {
	return countAuditCategory(p.au, CategorySupplementalReviewStarted)
}

// wantOutcome names the dispatcher's expected result.
type wantOutcome struct {
	blocked, interrupted, undecided, evidenceFailed bool
}

func (w wantOutcome) check(t *testing.T, got reviewDispatchOutcome) {
	t.Helper()
	if w.blocked != (got.blockReason == implementReviewGatingRejectReason) || w.blocked != got.blocked() {
		t.Errorf("blockReason = %q, want blocked=%v", got.blockReason, w.blocked)
	}
	if got.interrupted != w.interrupted {
		t.Errorf("interrupted = %v, want %v", got.interrupted, w.interrupted)
	}
	if got.undecided != w.undecided {
		t.Errorf("undecided = %v, want %v", got.undecided, w.undecided)
	}
	if got.evidenceWriteFailed != w.evidenceFailed {
		t.Errorf("evidenceWriteFailed = %v, want %v", got.evidenceWriteFailed, w.evidenceFailed)
	}
}

func TestDispatchSupplementalReinvokeReview_Gating_Modes(t *testing.T) {
	const otherAttempt = "2020-01-01T00:00:00Z"
	rows := []struct {
		name    string
		spec    []byte
		steps   []reviewStep
		attempt func(p *passSeam) string
		setup   func(t *testing.T, p *passSeam)
		want    wantOutcome
		calls   int32
		check   func(t *testing.T, p *passSeam)
	}{
		{name: "fresh_approve", calls: 1, check: func(t *testing.T, p *passSeam) {
			if p.startRows() != 1 {
				t.Errorf("start rows = %d, want 1", p.startRows())
			}
		}},
		{name: "fresh_reject", steps: []reviewStep{rejectStep}, want: wantOutcome{blocked: true}, calls: 1},
		{name: "partial_prior_attempt_reruns_every_reviewer", spec: specImplementGatingTwo, calls: 2, setup: func(t *testing.T, p *passSeam) {
			p.seedStart(t, otherAttempt, 2)
			p.seedVerdict(t, otherAttempt, planreview.VerdictApprove)
		}},
		{name: "complete_prior_attempt_reruns", calls: 1, setup: func(t *testing.T, p *passSeam) {
			p.seedStart(t, otherAttempt, 1)
			p.seedVerdict(t, otherAttempt, planreview.VerdictApprove)
		}},
		{name: "complete_current_skips_no_reviewer", setup: func(t *testing.T, p *passSeam) {
			p.seedStart(t, p.attempt, 1)
			p.seedVerdict(t, p.attempt, planreview.VerdictApprove)
		}},
		{name: "reject_prior_attempt_blocks_no_reviewer", want: wantOutcome{blocked: true}, setup: func(t *testing.T, p *passSeam) {
			p.seedVerdict(t, otherAttempt, planreview.VerdictReject)
		}},
		{name: "reject_current_blocks_no_reviewer", want: wantOutcome{blocked: true}, setup: func(t *testing.T, p *passSeam) {
			p.seedStart(t, p.attempt, 1)
			p.seedVerdict(t, p.attempt, planreview.VerdictReject)
		}},
		{name: "start_recorded_before_first_reviewer", calls: 1, setup: func(t *testing.T, p *passSeam) {
			p.rev.steps = []reviewStep{func(ctx context.Context) (*planreview.ReviewVerdict, string, error) {
				if p.startRows() != 1 {
					t.Errorf("start rows at invocation = %d, want 1", p.startRows())
				}
				return approveStep(ctx)
			}}
		}},
		{name: "started_this_attempt_interrupted_no_reviewer", want: wantOutcome{interrupted: true}, setup: func(t *testing.T, p *passSeam) {
			p.seedStart(t, p.attempt, 1)
		}, check: func(t *testing.T, p *passSeam) {
			// The lost allocation's re-read would also answer interrupted, so the
			// early exit is what this row isolates: it never attempts a start.
			p.audit.mu.Lock()
			defer p.audit.mu.Unlock()
			if p.audit.dedups != 0 {
				t.Errorf("start allocations attempted = %d, want 0", p.audit.dedups)
			}
		}},
		{name: "round_state_read_error_undecided_no_reviewer", want: wantOutcome{undecided: true}, setup: func(t *testing.T, p *passSeam) {
			p.audit.setHooks(nil, func(category string) error {
				if category == implementReviewedCategory {
					return errors.New("audit outage")
				}
				return nil
			})
		}, check: func(t *testing.T, p *passSeam) {
			if p.startRows() != 0 {
				t.Errorf("start rows = %d, want 0", p.startRows())
			}
		}},
		{name: "started_append_error_undecided_no_reviewer", want: wantOutcome{undecided: true}, setup: func(t *testing.T, p *passSeam) {
			p.audit.onDedup = func(audit.ChainAppendParams) (*audit.Entry, error) { return nil, errors.New("append outage") }
		}},
		{name: "run_read_error_undecided_no_reviewer", want: wantOutcome{undecided: true}, setup: func(t *testing.T, p *passSeam) {
			p.s.cfg.RunRepo = failingGetRunRepo{p.s.cfg.RunRepo}
		}},
		{name: "plan_read_error_undecided_no_reviewer", want: wantOutcome{undecided: true}, setup: func(t *testing.T, p *passSeam) {
			p.s.cfg.ArtifactRepo = failingListArtifactRepo{p.s.cfg.ArtifactRepo}
		}},
		{name: "capability_absent_undecided_no_reviewer", want: wantOutcome{undecided: true}, setup: func(t *testing.T, p *passSeam) {
			p.s.cfg.AuditRepo = plainAuditOnly{p.au}
		}, check: func(t *testing.T, p *passSeam) {
			if p.startRows() != 0 {
				t.Errorf("start rows = %d, want 0", p.startRows())
			}
		}},
		{name: "nil_audit_undecided_no_reviewer", want: wantOutcome{undecided: true}, setup: func(t *testing.T, p *passSeam) {
			p.s.cfg.AuditRepo = nil
		}},
		{name: "empty_attempt_undecided_no_reviewer", want: wantOutcome{undecided: true}, attempt: func(*passSeam) string { return "" }},
		{name: "duplicate_incomplete_interrupted_no_reviewer", want: wantOutcome{interrupted: true}, setup: func(t *testing.T, p *passSeam) {
			p.loseAllocation(t, 1, nil)
		}},
		{name: "duplicate_discharged_no_reviewer", setup: func(t *testing.T, p *passSeam) {
			p.loseAllocation(t, 1, []planreview.Verdict{planreview.VerdictApprove})
		}},
		{name: "duplicate_reject_blocks_no_reviewer", want: wantOutcome{blocked: true}, setup: func(t *testing.T, p *passSeam) {
			p.loseAllocation(t, 1, []planreview.Verdict{planreview.VerdictReject})
		}},
		{name: "duplicate_reread_error_undecided_no_reviewer", want: wantOutcome{undecided: true}, setup: func(t *testing.T, p *passSeam) {
			p.loseAllocation(t, 1, nil)
			lost := p.audit.onDedup
			p.audit.onDedup = func(q audit.ChainAppendParams) (*audit.Entry, error) {
				entry, err := lost(q)
				p.audit.setHooks(nil, func(string) error { return errors.New("re-read outage") })
				return entry, err
			}
		}},
		{name: "verdicts_without_start_rerun", calls: 1, setup: func(t *testing.T, p *passSeam) {
			p.seedVerdict(t, p.attempt, planreview.VerdictApprove)
		}},
		{name: "spec_unparseable_undecided_no_reviewer", want: wantOutcome{undecided: true}, setup: func(t *testing.T, p *passSeam) {
			p.runRow.WorkflowSpec = []byte("{not yaml: [")
		}},
		{name: "prompt_build_error_undecided", want: wantOutcome{undecided: true}, setup: func(t *testing.T, p *passSeam) {
			setVar(t, &buildImplementReviewPrompt, func(string, prompt.Trigger) (string, error) { return "", errors.New("render failed") })
		}, check: func(t *testing.T, p *passSeam) {
			if p.startRows() != 0 {
				t.Errorf("start rows = %d, want 0", p.startRows())
			}
		}},
		{name: "no_approved_plan_skips_no_reviewer", setup: func(t *testing.T, p *passSeam) {
			p.s.cfg.ArtifactRepo = newFakeArtifactRepo()
		}},
		{name: "document_injection_blocks", want: wantOutcome{blocked: true}, setup: func(t *testing.T, p *passSeam) {
			wireReviewInjection(p.s, missingDecl())
		}, check: func(t *testing.T, p *passSeam) {
			if n := countAuditCategory(p.au, "implement_review_failed"); n != 1 {
				t.Errorf("implement_review_failed = %d, want 1", n)
			}
			if p.startRows() != 0 {
				t.Errorf("start rows = %d, want 0", p.startRows())
			}
		}},
		{name: "document_injection_row_lost_refuses", want: wantOutcome{evidenceFailed: true}, setup: func(t *testing.T, p *passSeam) {
			wireReviewInjection(p.s, missingDecl())
			p.audit.setHooks(func(_ context.Context, q audit.ChainAppendParams, _ int) flakyAction {
				if q.Category == "implement_review_failed" {
					return flakyFail
				}
				return flakyOK
			}, nil)
		}},
		{name: "interrupted", want: wantOutcome{interrupted: true}, calls: 1, setup: func(t *testing.T, p *passSeam) {
			p.rev.steps = []reviewStep{func(ctx context.Context) (*planreview.ReviewVerdict, string, error) {
				p.s.reviewers.closeAndCancel()
				<-ctx.Done()
				return nil, "m", ctx.Err()
			}}
		}, check: func(t *testing.T, p *passSeam) {
			for _, category := range []string{"implement_reviewed", "implement_review_failed"} {
				if n := countAuditCategory(p.au, category); n != 0 {
					t.Errorf("%s rows = %d, want 0", category, n)
				}
			}
		}},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			spec := row.spec
			if spec == nil {
				spec = specImplementGatingReviewers
			}
			steps := row.steps
			if steps == nil {
				steps = []reviewStep{approveStep}
			}
			p := newPassSeam(t, spec, steps...)
			if row.setup != nil {
				row.setup(t, p)
			}
			attempt := p.attempt
			if row.attempt != nil {
				attempt = row.attempt(p)
			}
			got := p.s.dispatchSupplementalReinvokeReview(t.Context(), p.runRow.ID, p.stage.ID, tgtHead, attempt, supplementalExemptions())
			row.want.check(t, got)
			if n := p.rev.calls.Load(); n != row.calls {
				t.Errorf("reviewer calls = %d, want %d", n, row.calls)
			}
			if row.check != nil {
				row.check(t, p)
			}
		})
	}
}

// loseAllocation makes the start allocation find a rival's committed start, plus
// the rival's verdicts, as if it committed after this dispatcher's first read.
func (p *passSeam) loseAllocation(t *testing.T, agents int, verdicts []planreview.Verdict) {
	t.Helper()
	p.audit.onDedup = func(q audit.ChainAppendParams) (*audit.Entry, error) {
		p.seedStart(t, p.attempt, agents)
		for _, verdict := range verdicts {
			p.seedVerdict(t, p.attempt, verdict)
		}
		return nil, &audit.DedupedDuplicateError{Existing: &audit.Entry{Sequence: 7, Payload: q.Payload}}
	}
}

func TestDispatchSupplementalReinvokeReview_Advisory_DedupUnchanged(t *testing.T) {
	t.Run("recorded_verdict_dedups", func(t *testing.T) {
		p := newPassSeam(t, specImplementAdvisoryReviewers, approveStep)
		p.seedVerdict(t, "", planreview.VerdictApprove)
		if got := p.dispatch(t); got != (reviewDispatchOutcome{}) {
			t.Errorf("outcome = %+v", got)
		}
		p.s.waitBackgroundReviews()
		if n := p.rev.calls.Load(); n != 0 {
			t.Errorf("reviewer calls = %d, want 0", n)
		}
	})
	t.Run("list_error_proceeds", func(t *testing.T) {
		p := newPassSeam(t, specImplementAdvisoryReviewers, approveStep)
		var once sync.Once
		p.audit.setHooks(nil, func(category string) error {
			var err error
			if category == implementReviewedCategory {
				once.Do(func() { err = errors.New("list outage") })
			}
			return err
		})
		if got := p.dispatch(t); got != (reviewDispatchOutcome{}) {
			t.Errorf("outcome = %+v", got)
		}
		p.s.waitBackgroundReviews()
		if n := p.rev.calls.Load(); n != 1 {
			t.Errorf("reviewer calls = %d, want 1: a list error never suppresses advisory", n)
		}
	})
	t.Run("no_start_no_attempt", func(t *testing.T) {
		p := newPassSeam(t, specImplementAdvisoryReviewers, approveStep)
		if got := p.dispatch(t); got != (reviewDispatchOutcome{}) {
			t.Errorf("outcome = %+v", got)
		}
		p.s.waitBackgroundReviews()
		if p.startRows() != 0 {
			t.Errorf("start rows = %d, want 0", p.startRows())
		}
		rows := redispatchEntries(t, p.au, p.runRow.ID, implementReviewedCategory)
		if len(rows) != 1 || bytes.Contains(rows[0].Payload, []byte("stage_attempt")) {
			t.Errorf("advisory verdict rows = %d, must carry no stage_attempt", len(rows))
		}
	})
}

func TestDispatchSupplementalReinvokeReview_GatingRunsUnderCallerContext(t *testing.T) {
	setVar(t, &defaultReviewTerminalWriteConfig, reviewTerminalWriteConfig{opTimeout: time.Hour, settle: rtSteps(1, time.Millisecond)})
	p := newPassSeam(t, specImplementGatingReviewers, approveStep)
	entered := make(chan struct{})
	release := make(chan struct{})
	t.Cleanup(func() { close(release); p.s.waitBackgroundReviews() })
	var once sync.Once
	p.audit.setHooks(func(ctx context.Context, q audit.ChainAppendParams, _ int) flakyAction {
		if q.Category == implementReviewedCategory {
			once.Do(func() { close(entered) })
			select {
			case <-ctx.Done():
			case <-release:
			}
			return flakyFail
		}
		return flakyOK
	}, nil)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan reviewDispatchOutcome, 1)
	go func() {
		done <- p.s.dispatchSupplementalReinvokeReview(ctx, p.runRow.ID, p.stage.ID, tgtHead, p.attempt, supplementalExemptions())
	}()
	waitFor(t, entered, "setup: blocked verdict append")
	cancel()
	select {
	case <-done:
	case <-time.After(boundedWait):
		t.Fatalf("the dispatcher did not return within %s of its caller's cancel", boundedWait)
	}
}

func TestRunSupplementalReinvokeReview_WrapperReportsBlocked(t *testing.T) {
	t.Run("recorded_block_true", func(t *testing.T) {
		p := newPassSeam(t, specImplementGatingReviewers, rejectStep)
		if !p.s.runSupplementalReinvokeReview(t.Context(), p.runRow.ID, p.stage.ID, tgtHead, supplementalExemptions()) {
			t.Error("a recorded reject must report true")
		}
	})
	t.Run("approve_false_and_reads_stage_attempt", func(t *testing.T) {
		p := newPassSeam(t, specImplementGatingReviewers, approveStep)
		if p.s.runSupplementalReinvokeReview(t.Context(), p.runRow.ID, p.stage.ID, tgtHead, supplementalExemptions()) {
			t.Error("an approve must report false")
		}
		rows := redispatchEntries(t, p.au, p.runRow.ID, implementReviewedCategory)
		var evidence implementReviewedEvidence
		if len(rows) != 1 || json.Unmarshal(rows[0].Payload, &evidence) != nil || evidence.StageAttempt != p.attempt {
			t.Errorf("verdict rows = %d attempt = %q, want the stage's %q", len(rows), evidence.StageAttempt, p.attempt)
		}
	})
	t.Run("undecided_is_not_blocked", func(t *testing.T) {
		p := newPassSeam(t, specImplementGatingReviewers, approveStep)
		p.s.cfg.AuditRepo = plainAuditOnly{p.au}
		if p.s.runSupplementalReinvokeReview(t.Context(), p.runRow.ID, p.stage.ID, tgtHead, supplementalExemptions()) {
			t.Error("an undecided outcome is not a block")
		}
	})
	t.Run("nil_run_repo_false", func(t *testing.T) {
		p := newPassSeam(t, specImplementGatingReviewers, approveStep)
		p.s.cfg.RunRepo = nil
		if p.s.runSupplementalReinvokeReview(t.Context(), p.runRow.ID, p.stage.ID, tgtHead, supplementalExemptions()) {
			t.Error("no run repository means no pass")
		}
		if n := p.rev.calls.Load(); n != 0 {
			t.Errorf("reviewer calls = %d, want 0", n)
		}
	})
}

func TestDispatchSupplementalReinvokeReview_TerminalWritesBound(t *testing.T) {
	assertMarked := func(t *testing.T, p *passSeam, category string) {
		t.Helper()
		p.audit.mu.Lock()
		defer p.audit.mu.Unlock()
		if p.audit.writes[category] == 0 {
			t.Fatalf("setup: no %s write", category)
		}
		if !p.audit.marks[category] {
			t.Errorf("%s write does not carry the bounded mark", category)
		}
	}
	t.Run("verdict", func(t *testing.T) {
		p := newPassSeam(t, specImplementGatingReviewers, approveStep)
		p.dispatch(t)
		assertMarked(t, p, implementReviewedCategory)
	})
	t.Run("reviewer_failure", func(t *testing.T) {
		p := newPassSeam(t, specImplementGatingReviewers, func(context.Context) (*planreview.ReviewVerdict, string, error) {
			return nil, "m", errors.New("reviewer transport")
		})
		p.dispatch(t)
		assertMarked(t, p, "implement_review_failed")
	})
	t.Run("provider_skip", func(t *testing.T) {
		anthropic := &fakePlanReviewer{verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictApprove}, model: "claude-opus-4-8"}
		s, _, au, rr, runRow, stage := newImplementReviewServerWithSet(t, fakeReviewerSet{providers: map[string]PlanReviewer{"anthropic": anthropic}, def: anthropic}, specImplementHeterogeneousGating)
		au.stampSequence = true
		wrapped := newTGTAudit(au)
		s.cfg.AuditRepo = wrapped
		p := &passSeam{s: s, au: au, audit: wrapped, rr: rr, runRow: runRow, stage: stage, attempt: run.StageAttemptToken(stage.DispatchedAt)}
		p.dispatch(t)
		assertMarked(t, p, "implement_review_skipped")
	})
	t.Run("persona_skip", func(t *testing.T) {
		// This path builds no persona invocation, so the helper is driven directly
		// under the dispatcher's mark: the emit still routes through the writer.
		p := newPassSeam(t, specImplementGatingReviewers, approveStep)
		ctx, _ := withBoundedReviewTerminalWrites(t.Context())
		p.s.emitPersonaDegraded(ctx, p.runRow.ID, p.stage.ID, "implement_review_skipped", planreview.AuthorityGating, reviewerInvocation{persona: &personaInvocation{}}, 1)
		assertMarked(t, p, "implement_review_skipped")
	})
	t.Run("document_injection", func(t *testing.T) {
		p := newPassSeam(t, specImplementGatingReviewers, approveStep)
		wireReviewInjection(p.s, missingDecl())
		p.dispatch(t)
		assertMarked(t, p, "implement_review_failed")
	})
}

// failVerdicts fails the matching rows' writes and every later retry.
func failVerdicts(match func(p audit.ChainAppendParams) bool) func(context.Context, audit.ChainAppendParams, int) flakyAction {
	return func(_ context.Context, p audit.ChainAppendParams, _ int) flakyAction {
		if match(p) {
			return flakyFail
		}
		return flakyOK
	}
}

func verdictIs(p audit.ChainAppendParams, verdict planreview.Verdict) bool {
	if p.Category != implementReviewedCategory {
		return false
	}
	var payload planreview.ImplementReviewedPayload
	return json.Unmarshal(p.Payload, &payload) == nil && payload.Verdict == verdict
}

func TestDispatchSupplementalReinvokeReview_EvidenceWrite_Modes(t *testing.T) {
	stateOf := func(t *testing.T, p *passSeam) supplementalRoundSnapshot {
		t.Helper()
		state, err := p.s.supplementalRoundState(t.Context(), p.runRow.ID, p.stage.ID, tgtHead, p.attempt)
		if err != nil {
			t.Fatalf("round state: %v", err)
		}
		return state
	}
	allVerdicts := func(p audit.ChainAppendParams) bool { return p.Category == implementReviewedCategory }
	t.Run("all_approve_exhausted", func(t *testing.T) {
		p := newPassSeam(t, specImplementGatingTwo, approveStep)
		p.audit.setHooks(failVerdicts(allVerdicts), nil)
		wantOutcome{evidenceFailed: true}.check(t, p.dispatch(t))
		if n := p.rev.calls.Load(); n != 2 {
			t.Errorf("reviewer calls = %d, want 2: remaining reviewers still run", n)
		}
		if supplementalPassDischarged(stateOf(t, p)) {
			t.Error("a lost verdict set discharged the pass")
		}
	})
	t.Run("first_landed_second_exhausted", func(t *testing.T) {
		p := newPassSeam(t, specImplementGatingTwo, approveStep, approveStep)
		var verdicts int
		p.audit.setHooks(func(_ context.Context, q audit.ChainAppendParams, n int) flakyAction {
			if q.Category == implementReviewedCategory {
				verdicts = n
				if n > 1 {
					return flakyFail
				}
			}
			return flakyOK
		}, nil)
		wantOutcome{evidenceFailed: true}.check(t, p.dispatch(t))
		if verdicts < 2 {
			t.Errorf("verdict writes = %d, want the second attempted", verdicts)
		}
		if state := stateOf(t, p); state.currentVerdicts != 1 || supplementalPassDischarged(state) {
			t.Errorf("state = %+v, want one verdict and no discharge", state)
		}
	})
	t.Run("reject_write_exhausted_refuses", func(t *testing.T) {
		p := newPassSeam(t, specImplementGatingReviewers, rejectStep)
		p.audit.setHooks(failVerdicts(allVerdicts), nil)
		got := p.dispatch(t)
		wantOutcome{evidenceFailed: true}.check(t, got)
		if got.blockReason != "" {
			t.Errorf("blockReason = %q, want empty over a known write failure", got.blockReason)
		}
	})
	t.Run("reject_landed_other_exhausted", func(t *testing.T) {
		p := newPassSeam(t, specImplementGatingTwo, rejectStep, approveStep)
		p.audit.setHooks(failVerdicts(func(q audit.ChainAppendParams) bool { return verdictIs(q, planreview.VerdictApprove) }), nil)
		wantOutcome{evidenceFailed: true}.check(t, p.dispatch(t))
		if state := stateOf(t, p); !state.rejected {
			t.Errorf("state = %+v, want the landed reject to stay binding", state)
		}
	})
	t.Run("reject_lost_ack_recovered_blocks", func(t *testing.T) {
		p := newPassSeam(t, specImplementGatingReviewers, rejectStep)
		var once sync.Once
		p.audit.setHooks(func(_ context.Context, q audit.ChainAppendParams, _ int) flakyAction {
			act := flakyOK
			if q.Category == implementReviewedCategory {
				once.Do(func() { act = flakyCommitThenFail })
			}
			return act
		}, nil)
		wantOutcome{blocked: true}.check(t, p.dispatch(t))
	})
	t.Run("healthy_reject_blocks", func(t *testing.T) {
		p := newPassSeam(t, specImplementGatingReviewers, rejectStep)
		wantOutcome{blocked: true}.check(t, p.dispatch(t))
	})
	t.Run("reviewer_error_row_lost", func(t *testing.T) {
		p := newPassSeam(t, specImplementGatingReviewers, func(context.Context) (*planreview.ReviewVerdict, string, error) {
			return nil, "m", errors.New("reviewer transport")
		})
		p.audit.setHooks(failVerdicts(func(q audit.ChainAppendParams) bool { return q.Category == "implement_review_failed" }), nil)
		wantOutcome{evidenceFailed: true}.check(t, p.dispatch(t))
	})
	t.Run("reviewer_error_row_landed_keeps_no_block", func(t *testing.T) {
		p := newPassSeam(t, specImplementGatingReviewers, func(context.Context) (*planreview.ReviewVerdict, string, error) {
			return nil, "m", errors.New("reviewer transport")
		})
		wantOutcome{}.check(t, p.dispatch(t))
		if supplementalPassDischarged(stateOf(t, p)) {
			t.Error("a failed row discharged the pass")
		}
	})
	t.Run("skip_row_lost", func(t *testing.T) {
		anthropic := &fakePlanReviewer{verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictApprove}, model: "claude-opus-4-8"}
		s, _, au, _, runRow, stage := newImplementReviewServerWithSet(t, fakeReviewerSet{providers: map[string]PlanReviewer{"anthropic": anthropic}, def: anthropic}, specImplementHeterogeneousGating)
		au.stampSequence = true
		wrapped := newTGTAudit(au)
		s.cfg.AuditRepo = wrapped
		wrapped.setHooks(failVerdicts(func(q audit.ChainAppendParams) bool { return q.Category == "implement_review_skipped" }), nil)
		got := s.dispatchSupplementalReinvokeReview(t.Context(), runRow.ID, stage.ID, tgtHead, run.StageAttemptToken(stage.DispatchedAt), supplementalExemptions())
		wantOutcome{evidenceFailed: true}.check(t, got)
	})
	t.Run("lost_ack_recovered_counts", func(t *testing.T) {
		p := newPassSeam(t, specImplementGatingReviewers, approveStep)
		var once sync.Once
		p.audit.setHooks(func(_ context.Context, q audit.ChainAppendParams, _ int) flakyAction {
			act := flakyOK
			if q.Category == implementReviewedCategory {
				once.Do(func() { act = flakyCommitThenFail })
			}
			return act
		}, nil)
		wantOutcome{}.check(t, p.dispatch(t))
		if state := stateOf(t, p); !supplementalPassDischarged(state) {
			t.Errorf("state = %+v, want the recovered original to discharge", state)
		}
	})
	t.Run("named_row_not_discharged", func(t *testing.T) {
		p := newPassSeam(t, specImplementGatingReviewers, approveStep)
		p.audit.setHooks(failVerdicts(allVerdicts), nil)
		wantOutcome{evidenceFailed: true}.check(t, p.dispatch(t))
		if n := countAuditCategory(p.au, "implement_review_failed"); n != 1 {
			t.Fatalf("setup: named failures = %d, want 1", n)
		}
		if state := stateOf(t, p); state.currentVerdicts != 0 || supplementalPassDischarged(state) {
			t.Errorf("state = %+v, want a named row to count for nothing", state)
		}
	})
	t.Run("advisory_write_failure_unchanged", func(t *testing.T) {
		p := newPassSeam(t, specImplementAdvisoryReviewers, approveStep)
		p.audit.setHooks(failVerdicts(allVerdicts), nil)
		if got := p.dispatch(t); got != (reviewDispatchOutcome{}) {
			t.Errorf("advisory outcome = %+v, want empty", got)
		}
		p.s.waitBackgroundReviews()
		if p.audit.readsOf("acceptance_skipped_out_of_scope") == 0 {
			t.Error("the advisory round's post-loop passes must still run")
		}
	})
	t.Run("post_loop_skipped", func(t *testing.T) {
		p := newPassSeam(t, specImplementGatingReviewers, rejectStep)
		rec := &pageClassRecorder{}
		p.s.issueNotifier = rec
		cr := newFakeConcernRepo()
		seedAddressedConcern(cr, p.runRow.ID, p.stage.ID)
		p.s.cfg.ConcernRepo = cr
		p.audit.setHooks(failVerdicts(allVerdicts), nil)
		wantOutcome{evidenceFailed: true}.check(t, p.dispatch(t))
		rows, _ := cr.ListByRun(t.Context(), p.runRow.ID)
		if len(rows) != 1 || rows[0].State != concern.StateAddressedPending {
			t.Errorf("concerns after an incomplete round = %+v, want the seeded row untouched by resolution or auto-close", rows)
		}
		if n := p.audit.readsOf("acceptance_skipped_out_of_scope"); n != 0 {
			t.Errorf("audit-complete recompute ran on an incomplete round: %d reads", n)
		}
		if n := p.audit.readsOf(CategoryAcceptanceTriageDecided); n != 0 {
			t.Errorf("deferred release ran on an incomplete round: %d reads", n)
		}
		if len(rec.pageClass) != 0 {
			t.Errorf("page-class pings = %d, want 0", len(rec.pageClass))
		}
	})
	t.Run("deferred_release_skipped", func(t *testing.T) {
		gating := newPassSeam(t, specImplementGatingReviewers, approveStep)
		wantOutcome{}.check(t, gating.dispatch(t))
		if n := gating.audit.readsOf(CategoryAcceptanceTriageDecided); n != 0 {
			t.Errorf("a complete gating pass reached the deferred release: %d reads", n)
		}
		unmarked := newPassSeam(t, specImplementGatingReviewers, approveStep)
		unmarked.s.runImplementReviewInvocations(t.Context(), unmarked.runRow.ID, unmarked.stage.ID, []reviewerInvocation{{reviewer: unmarked.rev}},
			planreview.AuthorityGating, "prompt", "author", "", "", planreview.DefaultReviewBudget, "", 0)
		if unmarked.audit.readsOf(CategoryAcceptanceTriageDecided) == 0 {
			t.Error("control: an unmarked round must reach the deferred release")
		}
	})
}

// --- bounded terminal writer ------------------------------------------------

// recordedOp is one store operation's context as the writer handed it down.
type recordedOp struct {
	kind     string
	deadline time.Time
	hasLimit bool
	err      error
}

// boundedWriterAudit honours its context and records every operation. Its zero
// hooks fail every append and find nothing.
type boundedWriterAudit struct {
	*auditFake
	mu         sync.Mutex
	ops        []recordedOp
	onAppend   func(ctx context.Context, p audit.ChainAppendParams) error
	onList     func(ctx context.Context, category string) error
	stored     []*audit.Entry
	recording  bool
	namedCalls int
}

func newBoundedWriterAudit() *boundedWriterAudit {
	return &boundedWriterAudit{auditFake: newAuditFake(), recording: true}
}

func (a *boundedWriterAudit) record(ctx context.Context, kind string) {
	deadline, ok := ctx.Deadline()
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.recording {
		a.ops = append(a.ops, recordedOp{kind: kind, deadline: deadline, hasLimit: ok, err: ctx.Err()})
	}
}

func (a *boundedWriterAudit) AppendChained(ctx context.Context, p audit.ChainAppendParams) (*audit.Entry, error) {
	a.record(ctx, "append:"+p.Category)
	if p.Category == "implement_review_failed" {
		a.mu.Lock()
		a.namedCalls++
		a.mu.Unlock()
	}
	if a.onAppend != nil {
		if err := a.onAppend(ctx, p); err != nil {
			return nil, err
		}
	} else {
		return nil, errors.New("append outage")
	}
	entry := &audit.Entry{RunID: &p.RunID, StageID: p.StageID, Category: p.Category, Timestamp: p.Timestamp, Payload: p.Payload}
	a.mu.Lock()
	a.stored = append(a.stored, entry)
	a.mu.Unlock()
	return entry, nil
}

func (a *boundedWriterAudit) ListForRunByCategory(ctx context.Context, _ uuid.UUID, category string) ([]*audit.Entry, error) {
	a.record(ctx, "list:"+category)
	if a.onList != nil {
		if err := a.onList(ctx, category); err != nil {
			return nil, err
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []*audit.Entry
	for _, e := range a.stored {
		if e.Category == category {
			out = append(out, e)
		}
	}
	return out, nil
}

func (a *boundedWriterAudit) snapshot() []recordedOp {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.recording = false
	return slices.Clone(a.ops)
}

func (a *boundedWriterAudit) named() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.namedCalls
}

// boundedWriterConfig keeps the production operation timeout and backoff, so a
// detached probe's 30s deadline is unmistakable; only the settle schedule, which
// runs after the call under test, is shortened.
func boundedWriterConfig() reviewTerminalWriteConfig {
	cfg := productionReviewTerminalWriteConfig()
	cfg.settle = rtSteps(1, time.Millisecond)
	return cfg
}

func writerParams(runID, stageID uuid.UUID) audit.ChainAppendParams {
	payload, _ := json.Marshal(planreview.ImplementReviewedPayload{ReviewerKind: "agent", Verdict: planreview.VerdictApprove, Authority: planreview.AuthorityGating})
	return audit.ChainAppendParams{RunID: runID, StageID: &stageID, Category: implementReviewedCategory, Payload: payload}
}

func TestAppendReviewTerminal_BoundedMode_CallerContext(t *testing.T) {
	const window = 200 * time.Millisecond
	newCase := func(t *testing.T) (*Server, *boundedWriterAudit, uuid.UUID, uuid.UUID) {
		t.Helper()
		a := newBoundedWriterAudit()
		s := New(Config{Addr: "127.0.0.1:0", AuditRepo: a})
		t.Cleanup(s.waitBackgroundReviews)
		return s, a, uuid.New(), uuid.New()
	}
	opsOfKind := func(ops []recordedOp, prefix string) []recordedOp {
		return slices.DeleteFunc(slices.Clone(ops), func(op recordedOp) bool { return !strings.HasPrefix(op.kind, prefix) })
	}
	t.Run("deadline_reaches_every_operation", func(t *testing.T) {
		s, a, runID, stageID := newCase(t)
		a.onAppend = func(_ context.Context, p audit.ChainAppendParams) error {
			if p.Category == implementReviewedCategory {
				return errors.New("append outage")
			}
			return nil
		}
		base, cancel := context.WithTimeout(t.Context(), window)
		defer cancel()
		limit, _ := base.Deadline()
		ctx, bound := withBoundedReviewTerminalWrites(base)
		if _, err := s.appendReviewTerminalWith(ctx, writerParams(runID, stageID), boundedWriterConfig()); err == nil {
			t.Fatal("append succeeded, want the outage")
		}
		ops := a.snapshot()
		if len(opsOfKind(ops, "append:"+implementReviewedCategory)) == 0 {
			t.Fatal("setup: the original append was never reached")
		}
		if len(opsOfKind(ops, "list:"+implementReviewedCategory)) == 0 {
			t.Fatal("setup: the final probe was never reached")
		}
		for _, op := range ops {
			if !op.hasLimit || op.deadline.After(limit) {
				t.Errorf("%s ran under deadline %v, past the caller's %v", op.kind, op.deadline, limit)
			}
		}
		if !bound.evidenceWriteFailed() {
			t.Error("a lost write must flag the mark")
		}
	})
	t.Run("dependency_blocks_after_deadline", func(t *testing.T) {
		s, a, runID, stageID := newCase(t)
		release := make(chan struct{})
		t.Cleanup(func() { close(release) })
		a.onList = func(ctx context.Context, _ string) error {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-release:
				return errors.New("released")
			}
		}
		base, cancel := context.WithTimeout(t.Context(), window)
		defer cancel()
		ctx, _ := withBoundedReviewTerminalWrites(base)
		done := make(chan struct{})
		go func() {
			defer close(done)
			_, _ = s.appendReviewTerminalWith(ctx, writerParams(runID, stageID), boundedWriterConfig())
		}()
		select {
		case <-done:
		case <-time.After(window + 2*time.Second):
			t.Fatal("the blocked final probe outlived the caller's deadline")
		}
	})
	t.Run("shutdown_cancels_recovery", func(t *testing.T) {
		s, a, runID, stageID := newCase(t)
		a.onAppend = func(_ context.Context, p audit.ChainAppendParams) error {
			s.reviewers.closeAndCancel()
			return errors.New("append outage")
		}
		entered, leave, ok := s.reviewers.enter(context.Background())
		if !ok {
			t.Fatal("setup: enter refused")
		}
		defer leave()
		ctx, bound := withBoundedReviewTerminalWrites(entered)
		if _, err := s.appendReviewTerminalWith(ctx, writerParams(runID, stageID), boundedWriterConfig()); err == nil {
			t.Fatal("append succeeded, want the outage")
		}
		probes := opsOfKind(a.snapshot(), "list:"+implementReviewedCategory)
		if len(probes) == 0 {
			t.Fatal("setup: the final probe was never reached")
		}
		if !errors.Is(probes[len(probes)-1].err, context.Canceled) {
			t.Errorf("final probe's context err = %v, want Canceled", probes[len(probes)-1].err)
		}
		if !bound.evidenceWriteFailed() {
			t.Error("a lost write must flag the mark")
		}
	})
	t.Run("named_attempt_keeps_caller_deadline", func(t *testing.T) {
		s, a, runID, stageID := newCase(t)
		a.onAppend = func(_ context.Context, p audit.ChainAppendParams) error {
			if p.Category == implementReviewedCategory {
				return errors.New("permanently rejected")
			}
			return nil
		}
		cfg := boundedWriterConfig()
		cfg.backoff = nil
		// Shorter than the operation timeout, so only the caller's own deadline
		// can be this close: a detached attempt's would lie about 30s out.
		base, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		limit, _ := base.Deadline()
		ctx, _ := withBoundedReviewTerminalWrites(base)
		if _, err := s.appendReviewTerminalWith(ctx, writerParams(runID, stageID), cfg); err == nil {
			t.Fatal("append succeeded, want the rejection")
		}
		named := opsOfKind(a.snapshot(), "append:implement_review_failed")
		if len(named) != 1 {
			t.Fatalf("setup: named attempts = %d, want 1", len(named))
		}
		if !named[0].hasLimit || named[0].deadline.After(limit) {
			t.Errorf("named attempt deadline %v is past the caller's %v", named[0].deadline, limit)
		}
	})
	t.Run("lost_ack_probe_counts", func(t *testing.T) {
		s, a, runID, stageID := newCase(t)
		a.onAppend = func(_ context.Context, p audit.ChainAppendParams) error { return nil }
		var once sync.Once
		wrapped := &lostAckAudit{boundedWriterAudit: a, once: &once}
		s.cfg.AuditRepo = wrapped
		cfg := boundedWriterConfig()
		cfg.backoff = nil
		ctx, bound := withBoundedReviewTerminalWrites(t.Context())
		entry, err := s.appendReviewTerminalWith(ctx, writerParams(runID, stageID), cfg)
		if err != nil || entry == nil {
			t.Fatalf("append = %v, %v; want the probed original", entry, err)
		}
		if bound.evidenceWriteFailed() {
			t.Error("a recovered original must not flag the mark")
		}
		if n := a.named(); n != 0 {
			t.Errorf("named attempts = %d, want 0", n)
		}
	})
	t.Run("cancelled_before_probe_registers", func(t *testing.T) {
		s, a, runID, stageID := newCase(t)
		cancelCtx, cancel := context.WithCancel(t.Context())
		a.onAppend = func(_ context.Context, p audit.ChainAppendParams) error {
			if p.Category == implementReviewedCategory {
				cancel()
				return errors.New("append outage")
			}
			return nil
		}
		ctx, bound := withBoundedReviewTerminalWrites(cancelCtx)
		// Hold the settle back until the assertions have read the registry.
		cfg := boundedWriterConfig()
		cfg.settle = rtSteps(1, 200*time.Millisecond)
		if _, err := s.appendReviewTerminalWith(ctx, writerParams(runID, stageID), cfg); err == nil {
			t.Fatal("append succeeded, want the outage")
		}
		if !bound.evidenceWriteFailed() {
			t.Error("a lost write must flag the mark")
		}
		if n := a.named(); n != 0 {
			t.Errorf("named attempts under a dead context = %d, want 0", n)
		}
		if n := registered(runID, "implement"); n != 1 {
			t.Errorf("registered lost terminals = %d, want 1", n)
		}
	})
	t.Run("unmarked_keeps_detached_recovery", func(t *testing.T) {
		s, a, runID, stageID := newCase(t)
		cancelCtx, cancel := context.WithCancel(t.Context())
		a.onAppend = func(_ context.Context, p audit.ChainAppendParams) error {
			if p.Category == implementReviewedCategory {
				cancel()
				return errors.New("append outage")
			}
			return nil
		}
		begin := time.Now()
		if _, err := s.appendReviewTerminalWith(cancelCtx, writerParams(runID, stageID), boundedWriterConfig()); err == nil {
			t.Fatal("append succeeded, want the outage")
		}
		probes := opsOfKind(a.snapshot(), "list:"+implementReviewedCategory)
		if len(probes) == 0 {
			t.Fatal("setup: the final probe was never reached")
		}
		final := probes[len(probes)-1]
		if final.err != nil {
			t.Errorf("unmarked final probe saw %v, want the #4106 detached context", final.err)
		}
		if !final.hasLimit || final.deadline.Before(begin.Add(25*time.Second)) {
			t.Errorf("unmarked final probe deadline = %v, want about 30s out", final.deadline)
		}
	})
}

// lostAckAudit commits the first verdict append and loses its acknowledgement.
type lostAckAudit struct {
	*boundedWriterAudit
	once *sync.Once
}

func (a *lostAckAudit) AppendChained(ctx context.Context, p audit.ChainAppendParams) (*audit.Entry, error) {
	entry, err := a.boundedWriterAudit.AppendChained(ctx, p)
	if err != nil {
		return nil, err
	}
	lost := false
	a.once.Do(func() { lost = true })
	if lost {
		return nil, errors.New("committed but the ack was lost")
	}
	return entry, nil
}

func TestLostTerminalSettle_NeverDischarges(t *testing.T) {
	p := newPassSeam(t, specImplementGatingReviewers, approveStep)
	var recovered bool
	var mu sync.Mutex
	p.audit.setHooks(func(_ context.Context, q audit.ChainAppendParams, _ int) flakyAction {
		mu.Lock()
		defer mu.Unlock()
		if !recovered && (q.Category == implementReviewedCategory || q.Category == "implement_review_failed") {
			return flakyFail
		}
		return flakyOK
	}, nil)
	wantOutcome{evidenceFailed: true}.check(t, p.dispatch(t))
	if n := registered(p.runRow.ID, "implement"); n != 1 {
		t.Fatalf("setup: registered lost terminals = %d, want 1", n)
	}
	mu.Lock()
	recovered = true
	mu.Unlock()
	p.s.waitBackgroundReviews()
	if n := countAuditCategory(p.au, "implement_review_failed"); n != 1 {
		t.Fatalf("setup: the settle landed %d named rows, want 1", n)
	}
	state, err := p.s.supplementalRoundState(t.Context(), p.runRow.ID, p.stage.ID, tgtHead, p.attempt)
	if err != nil {
		t.Fatalf("round state: %v", err)
	}
	if state.currentVerdicts != 0 || supplementalPassDischarged(state) {
		t.Errorf("state = %+v, want a settled named row to discharge nothing", state)
	}
	retried, err := p.s.supplementalRoundState(t.Context(), p.runRow.ID, p.stage.ID, tgtHead, "2031-01-01T00:00:00Z")
	if err != nil {
		t.Fatalf("round state: %v", err)
	}
	if retried.currentVerdicts != 0 || retried.startedThisAttempt {
		t.Errorf("retried attempt's state = %+v, want nothing counted", retried)
	}
}

// --- verdict attempt stamp --------------------------------------------------

func TestSupplementalVerdict_CarriesCapturedAttempt(t *testing.T) {
	plainPayload := planreview.ImplementReviewedPayload{
		ReviewerKind: "agent", ReviewerModel: "m", Authority: planreview.AuthorityGating, Verdict: planreview.VerdictApprove,
		Origin: planreview.OriginBaseRebaseReinvoke, HeadSHA: tgtHead,
	}
	t.Run("write_read_parity", func(t *testing.T) {
		p := newPassSeam(t, specImplementGatingReviewers, approveStep)
		wantOutcome{}.check(t, p.dispatch(t))
		rows := redispatchEntries(t, p.au, p.runRow.ID, implementReviewedCategory)
		if len(rows) != 1 {
			t.Fatalf("setup: verdict rows = %d", len(rows))
		}
		var evidence implementReviewedEvidence
		if err := json.Unmarshal(rows[0].Payload, &evidence); err != nil || evidence.StageAttempt != p.attempt {
			t.Errorf("decoded attempt = %q (%v), want %q", evidence.StageAttempt, err, p.attempt)
		}
		var plain planreview.ImplementReviewedPayload
		if err := json.Unmarshal(rows[0].Payload, &plain); err != nil || plain.Verdict != planreview.VerdictApprove || plain.HeadSHA != tgtHead || plain.Origin != planreview.OriginBaseRebaseReinvoke {
			t.Errorf("plain decode = %+v (%v)", plain, err)
		}
		state, err := p.s.supplementalRoundState(t.Context(), p.runRow.ID, p.stage.ID, tgtHead, p.attempt)
		if err != nil || state.currentVerdicts != 1 || !supplementalPassDischarged(state) {
			t.Errorf("state = %+v (%v), want the row counted and discharged", state, err)
		}
	})
	t.Run("first_review_row_byte_identical", func(t *testing.T) {
		rev := &scriptedReviewer{steps: []reviewStep{approveStep}}
		p := newTraceSeam(t, singleReviewerSet{rev}, specImplementGatingReviewers, tgtBundle(t, tgtHead, false))
		if w := p.post(t); w.Code != http.StatusAccepted {
			t.Fatalf("setup: upload = %d", w.Code)
		}
		rows := redispatchEntries(t, p.au, p.runRow.ID, implementReviewedCategory)
		if len(rows) != 1 || bytes.Contains(rows[0].Payload, []byte("stage_attempt")) {
			t.Errorf("first-review rows = %d, must carry no stage_attempt key", len(rows))
		}
	})
	t.Run("advisory_row_unmarked", func(t *testing.T) {
		p := newPassSeam(t, specImplementAdvisoryReviewers, approveStep)
		p.dispatch(t)
		p.s.waitBackgroundReviews()
		rows := redispatchEntries(t, p.au, p.runRow.ID, implementReviewedCategory)
		if len(rows) != 1 || bytes.Contains(rows[0].Payload, []byte("stage_attempt")) {
			t.Errorf("advisory rows = %d, must carry no stage_attempt key", len(rows))
		}
	})
	t.Run("decoded_by_prior_dedup", func(t *testing.T) {
		p := newPassSeam(t, specImplementGatingReviewers, approveStep)
		wantOutcome{}.check(t, p.dispatch(t))
		rows := redispatchEntries(t, p.au, p.runRow.ID, implementReviewedCategory)
		if !supplementalReinvokeReviewAlreadyRecorded(rows, p.stage.ID, tgtHead) {
			t.Error("a stamped row must still satisfy the prior dedup, the rollback half of skew")
		}
	})
	t.Run("existing_keys_unchanged", func(t *testing.T) {
		stamped, _ := json.Marshal(implementReviewedEvidence{ImplementReviewedPayload: plainPayload, StageAttempt: "attempt-1"})
		plain, _ := json.Marshal(plainPayload)
		var stampedKeys, plainKeys map[string]json.RawMessage
		if err := json.Unmarshal(stamped, &stampedKeys); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(plain, &plainKeys); err != nil {
			t.Fatal(err)
		}
		if string(stampedKeys["stage_attempt"]) != `"attempt-1"` {
			t.Errorf("stage_attempt = %s", stampedKeys["stage_attempt"])
		}
		delete(stampedKeys, "stage_attempt")
		if !maps.EqualFunc(stampedKeys, plainKeys, func(a, b json.RawMessage) bool { return bytes.Equal(a, b) }) {
			t.Errorf("existing keys changed:\n%s\n%s", stamped, plain)
		}
	})
	t.Run("empty_attempt_byte_identical", func(t *testing.T) {
		stamped, _ := json.Marshal(implementReviewedEvidence{ImplementReviewedPayload: plainPayload})
		plain, _ := json.Marshal(plainPayload)
		if !bytes.Equal(stamped, plain) {
			t.Errorf("empty attempt changed the bytes:\n%s\n%s", stamped, plain)
		}
	})
	t.Run("old_reader_ignores_attempt", func(t *testing.T) {
		// A frozen copy of the fields an older reader decodes.
		var old struct {
			Verdict string `json:"verdict"`
			Origin  string `json:"origin"`
			HeadSHA string `json:"head_sha"`
		}
		stamped, _ := json.Marshal(implementReviewedEvidence{ImplementReviewedPayload: plainPayload, StageAttempt: "attempt-1"})
		if err := json.Unmarshal(stamped, &old); err != nil || old.Verdict != string(planreview.VerdictApprove) || old.HeadSHA != tgtHead {
			t.Errorf("frozen reader = %+v (%v)", old, err)
		}
	})
	t.Run("new_attempt_decode_count_parity", func(t *testing.T) {
		p := newPassSeam(t, specImplementGatingReviewers, approveStep)
		wantOutcome{}.check(t, p.dispatch(t))
		state, err := p.s.supplementalRoundState(t.Context(), p.runRow.ID, p.stage.ID, tgtHead, "another-attempt")
		if err != nil || state.currentVerdicts != 0 {
			t.Errorf("state on another attempt = %+v (%v), want zero counted", state, err)
		}
	})
}

func TestSupplementalVerdict_AttemptCapturedBeforeInvocation(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	p := newPassSeam(t, specImplementGatingReviewers, heldStep(started, release, approveStep))
	captured := p.attempt
	done := make(chan reviewDispatchOutcome, 1)
	go func() {
		done <- p.s.dispatchSupplementalReinvokeReview(context.Background(), p.runRow.ID, p.stage.ID, tgtHead, captured, supplementalExemptions())
	}()
	waitFor(t, started, "setup: reviewer invoked")
	p.rr.mu.Lock()
	p.rr.stampDispatchedLocked(p.stage)
	moved := run.StageAttemptToken(p.stage.DispatchedAt)
	p.rr.mu.Unlock()
	if moved == captured {
		t.Fatal("setup: the re-dispatch did not move the attempt token")
	}
	close(release)
	select {
	case <-done:
	case <-time.After(boundedWait):
		t.Fatal("setup: dispatcher did not return")
	}
	rows := redispatchEntries(t, p.au, p.runRow.ID, implementReviewedCategory)
	var evidence implementReviewedEvidence
	if len(rows) != 1 || json.Unmarshal(rows[0].Payload, &evidence) != nil {
		t.Fatalf("setup: verdict rows = %d", len(rows))
	}
	if evidence.StageAttempt != captured {
		t.Errorf("verdict attempt = %q, want the captured %q, not the re-dispatch's %q", evidence.StageAttempt, captured, moved)
	}
}

// --- AST guards -------------------------------------------------------------

// productionReferrers maps each named method to the enclosing production function
// of every reference, excluding its own declaration.
func productionReferrers(t *testing.T, names ...string) map[string][]string {
	t.Helper()
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	out := map[string][]string{}
	for _, name := range names {
		out[name] = nil
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		file, perr := parser.ParseFile(fset, entry.Name(), nil, parser.SkipObjectResolution)
		if perr != nil {
			t.Fatalf("parse %s: %v", entry.Name(), perr)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				if _, wanted := out[sel.Sel.Name]; wanted && !slices.Contains(out[sel.Sel.Name], fn.Name.Name) {
					out[sel.Sel.Name] = append(out[sel.Sel.Name], fn.Name.Name)
				}
				return true
			})
		}
	}
	return out
}

func TestReviewWrappers_CallersOwnNoStageEdge(t *testing.T) {
	allowed := map[string][]string{
		"runPlanReviews":                               {"redispatchPlanRound"},
		"runImplementReviews":                          {"redispatchImplementRound", "runConsolidatedReviewRound", "maybeBackstopFixupReReview", "startConflictResolutionRound"},
		"runPlanReviewLoop":                            {},
		"runPlanReviewLoopWithConventions":             {"runPlanReviewLoop"},
		"runImplementReviewInvocations":                {},
		"runImplementReviewInvocationsWithConventions": {},
		// Slice 2 moves the handler to the typed dispatcher and pins that with
		// TestSupplementalWrapper_NoHandlerReferrer; this slice cannot edit its file.
		"runSupplementalReinvokeReview": {"handleShipPullRequest"},
	}
	got := productionReferrers(t, slices.Collect(maps.Keys(allowed))...)
	for wrapper, referrers := range got {
		for _, referrer := range referrers {
			if !slices.Contains(allowed[wrapper], referrer) {
				t.Errorf("%s has production referrer %s: a caller owning a stage edge must read the typed outcome", wrapper, referrer)
			}
		}
	}
	for _, edgeOwner := range []string{"handleShipPlan", "advanceStageAfterTrace", "handleShipTrace"} {
		for wrapper, referrers := range got {
			if slices.Contains(referrers, edgeOwner) {
				t.Errorf("edge owner %s calls bool wrapper %s", edgeOwner, wrapper)
			}
		}
	}
}

// conflictRoundStageEdgeNames are the stage-edge owners
// startConflictResolutionRound must never reference (#4795 D5).
var conflictRoundStageEdgeNames = []string{
	"TransitionStage", "TransitionRun", "transitionStageAtIdentity", "transitionStageAtAttempt",
	"transitionStageAtAttemptGuarded", "failStageCategoryB", "advanceStage", "advanceStageAfterTrace",
	"advanceVariantStageAfterReport",
}

// inspectConflictRoundBody reports every violation in fn's body: a referenced
// stage-edge name, or a runImplementReviews call that is not a bare
// expression statement (its outcome must be discarded), or not exactly one
// such call.
func inspectConflictRoundBody(fn *ast.FuncDecl) []string {
	var out []string
	exprCalls, allCalls := 0, 0
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.ExprStmt:
			if call, ok := x.X.(*ast.CallExpr); ok {
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "runImplementReviews" {
					exprCalls++
				}
			}
		case *ast.CallExpr:
			if sel, ok := x.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "runImplementReviews" {
				allCalls++
			}
		case *ast.Ident:
			if slices.Contains(conflictRoundStageEdgeNames, x.Name) {
				out = append(out, "references stage edge "+x.Name)
			}
		}
		return true
	})
	if allCalls != 1 || exprCalls != 1 {
		out = append(out, fmt.Sprintf("runImplementReviews calls = %d, as bare statements = %d; want exactly one discarded call", allCalls, exprCalls))
	}
	return out
}

func conflictRoundFuncDecl(t *testing.T, file *ast.File) *ast.FuncDecl {
	t.Helper()
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "startConflictResolutionRound" && fn.Body != nil {
			return fn
		}
	}
	t.Fatal("startConflictResolutionRound not found")
	return nil
}

// TestConflictResolutionRound_OwnsNoStageEdge pins #4795 D5: the
// conflict-resolution round body owns no stage edge and discards the review
// outcome. The self-checks plant two violating copies through the same
// inspector, so a weakened inspector fails here.
func TestConflictResolutionRound_OwnsNoStageEdge(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "conflictresolution_review.go", nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if v := inspectConflictRoundBody(conflictRoundFuncDecl(t, file)); len(v) != 0 {
		t.Fatalf("startConflictResolutionRound: %v", v)
	}
	planted := map[string]string{
		"stage_edge": `package server
func (s *Server) startConflictResolutionRound() string {
	s.cfg.RunRepo.TransitionStage(nil)
	s.runImplementReviews(nil)
	return ""
}`,
		"outcome_read": `package server
func (s *Server) startConflictResolutionRound() string {
	out := s.runImplementReviews(nil)
	_ = out
	return ""
}`,
	}
	for name, src := range planted {
		pf, err := parser.ParseFile(fset, name+".go", src, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse planted %s: %v", name, err)
		}
		if v := inspectConflictRoundBody(conflictRoundFuncDecl(t, pf)); len(v) == 0 {
			t.Errorf("self-check %s: the inspector passed a violating body", name)
		}
	}
}

func TestAppendReviewTerminalSingle_OnlyLockedCallers(t *testing.T) {
	got := productionReferrers(t, "appendReviewTerminalSingleWith")["appendReviewTerminalSingleWith"]
	if !slices.Equal(got, []string{"emitReviewFailedLocked"}) {
		t.Errorf("appendReviewTerminalSingleWith referrers = %v, want only emitReviewFailedLocked: its detached write must never serve a gating pass", got)
	}
}
