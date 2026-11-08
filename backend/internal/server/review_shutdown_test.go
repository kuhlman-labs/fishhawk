//go:build unix

package server

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/claudecode"
	"github.com/kuhlman-labs/fishhawk/backend/internal/codex"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/procgroup"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/signing"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
	"github.com/kuhlman-labs/fishhawk/backend/internal/timescale"
)

// boundedWait is the per-test bound on anything a test waits for. A blocking
// mutant fails this wait rather than the global test timeout.
var boundedWait = timescale.D(20 * time.Second)

// waitFor fails the test, naming what, when ch never closes within the bound. A
// setup wait passes a what beginning `setup:`, whose failure is never a detection.
func waitFor(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(boundedWait):
		t.Fatalf("%s: not observed within %s", what, boundedWait)
	}
}

// setVar lowers a package var for one test and restores it.
func setVar[T any](t *testing.T, v *T, to T) {
	t.Helper()
	old := *v
	*v = to
	t.Cleanup(func() { *v = old })
}

// --- real process groups ---------------------------------------------------

// groupStubScript writes a stub reviewer that records its own pid, forks a
// long sleeper into its process group and waits. It is invoked by real adapters,
// whose environment is scrubbed, so every path is written into the script.
func groupStubScript(t *testing.T) (script, pidFile string) {
	t.Helper()
	dir := t.TempDir()
	pidFile = filepath.Join(dir, "pid")
	script = filepath.Join(dir, "reviewer-stub")
	body := "#!/bin/sh\nPATH=/usr/bin:/bin\nsleep 300 &\necho $$ > " + pidFile + ".tmp\nmv " + pidFile + ".tmp " + pidFile + "\nwait\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil { //nolint:gosec // an executable test stub
		t.Fatalf("setup: write stub: %v", err)
	}
	return script, pidFile
}

// awaitGroup reads the stub's pid (its pgid: Harden makes it the group leader).
func awaitGroup(t *testing.T, pidFile string) int {
	t.Helper()
	deadline := time.Now().Add(boundedWait)
	for time.Now().Before(deadline) {
		if raw, err := os.ReadFile(pidFile); err == nil { //nolint:gosec // a test temp path
			pgid, perr := strconv.Atoi(strings.TrimSpace(string(raw)))
			if perr != nil {
				t.Fatalf("setup: parse pid: %v", perr)
			}
			t.Cleanup(func() { _ = syscall.Kill(-pgid, syscall.SIGKILL) })
			if syscall.Kill(-pgid, 0) != nil {
				t.Fatalf("setup: reviewer group %d is not running", pgid)
			}
			return pgid
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("setup: reviewer stub never recorded its pid")
	return 0
}

// groupGone polls kill(-pgid, 0) until it reports ESRCH or until runs out. A
// reaped-but-not-yet-gone member can still answer briefly, hence the poll.
func groupGone(pgid int, until time.Duration) bool {
	deadline := time.Now().Add(until)
	for {
		if errors.Is(syscall.Kill(-pgid, 0), syscall.ESRCH) {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// hardenFakeReviewer drives the process group the way the adapters do, with no
// adapter code in the path.
type hardenFakeReviewer struct{ script string }

func (r hardenFakeReviewer) Review(ctx context.Context, _ string) (*planreview.ReviewVerdict, string, error) {
	cmd := exec.CommandContext(ctx, r.script) //nolint:gosec // a test stub path
	procgroup.Harden(cmd, 5*time.Second)
	return nil, "fake", cmd.Run()
}

// startGroupReviewer runs one reviewer under a detached context, as every
// production caller does, and returns its pgid and completion.
func startGroupReviewer(t *testing.T, s *Server, reviewer PlanReviewer, pidFile string) (int, <-chan error) {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		_, _, err := s.invokeReview(context.WithoutCancel(t.Context()), reviewerInvocation{reviewer: reviewer}, "prompt", "")
		done <- err
	}()
	return awaitGroup(t, pidFile), done
}

// Structured entry 1: rvwr's context is WithoutCancel, so only the lifetime's
// cancel can end the group; with it neutered the group stays alive.
func TestShutdown_EndsRunningReviewerProcessGroup(t *testing.T) {
	script, pidFile := groupStubScript(t)
	for _, tc := range []struct {
		name     string
		reviewer func(script string) PlanReviewer
	}{
		{"harden_fake", func(p string) PlanReviewer { return hardenFakeReviewer{script: p} }},
		{"claudecode_adapter", func(p string) PlanReviewer {
			return claudecode.NewReviewer(claudecode.Config{Binary: p, Timeout: time.Hour})
		}},
		{"codex_adapter", func(p string) PlanReviewer {
			return codex.NewReviewer(codex.Config{Binary: p, Timeout: time.Hour})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_ = os.Remove(pidFile)
			s := New(Config{Addr: "127.0.0.1:0", ShutdownTimeout: 100 * time.Millisecond})
			pgid, done := startGroupReviewer(t, s, tc.reviewer(script), pidFile)
			if err := s.Shutdown(context.Background()); err != nil {
				t.Fatalf("Shutdown: %v", err)
			}
			if !groupGone(pgid, timescale.D(2*time.Second)) {
				t.Fatalf("reviewer process group %d is still alive after Shutdown", pgid)
			}
			select {
			case err := <-done:
				if !errors.Is(err, errReviewerStoppedForShutdown) {
					t.Errorf("invokeReview err = %v, want the interrupted sentinel", err)
				}
			case <-time.After(boundedWait):
				t.Fatal("invokeReview did not return after the group died")
			}
		})
	}
}

// Z53: the clock fires during HTTP drain, so the group dies while Shutdown is
// still blocked on a background review.
func TestShutdown_ReviewerStopFiresBeforeHTTPDrainEnds(t *testing.T) {
	setVar(t, &reviewerShutdownDrain, 50*time.Millisecond)
	script, pidFile := groupStubScript(t)
	s := New(Config{Addr: "127.0.0.1:0", ShutdownTimeout: timescale.D(3 * time.Second)})
	s.bgReviews.Add(1)
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(s.bgReviews.Done) }
	t.Cleanup(release)
	pgid, _ := startGroupReviewer(t, s, hardenFakeReviewer{script: script}, pidFile)

	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		_ = s.Shutdown(context.Background())
	}()
	if !groupGone(pgid, timescale.D(2500*time.Millisecond)) {
		t.Fatalf("reviewer group %d still alive most of the way through the drain: the clock did not fire during it", pgid)
	}
	select {
	case <-shutdownDone:
		t.Fatal("Shutdown returned before its drain ended: the group's death was not observed during drain")
	default:
	}
	release()
	waitFor(t, shutdownDone, "Shutdown")
}

// Z46: a reviewer ignoring its context cannot hold Shutdown past the grace.
func TestShutdown_ReviewerIgnoringCancel_BoundedByGrace(t *testing.T) {
	setVar(t, &reviewerShutdownGrace, 200*time.Millisecond)
	s := New(Config{Addr: "127.0.0.1:0", ShutdownTimeout: 50 * time.Millisecond})
	release := make(chan struct{})
	entered := make(chan struct{})
	t.Cleanup(func() { close(release) })
	reviewer := &scriptedReviewer{steps: []reviewStep{func(context.Context) (*planreview.ReviewVerdict, string, error) {
		close(entered)
		<-release
		return nil, "", errors.New("released")
	}}}
	go func() {
		_, _, _ = s.invokeReview(context.Background(), reviewerInvocation{reviewer: reviewer}, "p", "")
	}()
	waitFor(t, entered, "setup: reviewer entered")

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = s.Shutdown(context.Background())
	}()
	select {
	case <-done:
	case <-time.After(timescale.D(5 * time.Second)):
		t.Fatal("Shutdown is held by a reviewer ignoring its context")
	}
	if !s.reviewers.isClosed() {
		t.Error("lifetime is not closed after Shutdown")
	}
}

// Z56: after stop no adapter is touched.
func TestInvokeReview_AfterStop_RefusesWithoutSpawning(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0"})
	s.reviewers.stop(0)
	reviewer := &scriptedReviewer{steps: []reviewStep{approveStep}}
	verdict, _, err := s.invokeReview(context.Background(), reviewerInvocation{reviewer: reviewer}, "p", "")
	if !errors.Is(err, errReviewerStoppedForShutdown) || verdict != nil {
		t.Fatalf("invokeReview = (%v, %v), want the interrupted sentinel", verdict, err)
	}
	if n := reviewer.calls.Load(); n != 0 {
		t.Fatalf("adapter was called %d times after stop", n)
	}
}

// Z26: an ordinary reviewer error stays itself on an open lifetime.
func TestInvokeReview_ReviewerErrorWithoutShutdown_IsNotSentinel(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0"})
	boom := errors.New("reviewer transport failed")
	reviewer := &scriptedReviewer{steps: []reviewStep{func(context.Context) (*planreview.ReviewVerdict, string, error) {
		return nil, "m", boom
	}}}
	_, model, err := s.invokeReview(context.Background(), reviewerInvocation{reviewer: reviewer}, "p", "")
	if errors.Is(err, errReviewerStoppedForShutdown) || !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the reviewer's own error", err)
	}
	if model != "m" {
		t.Errorf("model = %q", model)
	}
}

func TestInvokeReview_VerdictDespiteClose_ReturnedNormally(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0"})
	reviewer := &scriptedReviewer{steps: []reviewStep{func(context.Context) (*planreview.ReviewVerdict, string, error) {
		s.reviewers.closeAndCancel()
		return &planreview.ReviewVerdict{Verdict: planreview.VerdictApprove}, "m", nil
	}}}
	verdict, _, err := s.invokeReview(context.Background(), reviewerInvocation{reviewer: reviewer}, "p", "")
	if err != nil || verdict == nil || verdict.Verdict != planreview.VerdictApprove {
		t.Fatalf("invokeReview = (%v, %v), want the verdict that arrived", verdict, err)
	}
}

func TestServer_Shutdown_NoLiveReviewer_ReturnsPromptly(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0", ShutdownTimeout: time.Minute})
	start := time.Now()
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if elapsed := time.Since(start); elapsed >= reviewerShutdownDrain {
		t.Fatalf("Shutdown took %s with nothing live: an idle daemon must not wait on the reviewer clock", elapsed)
	}
}

// Z38: a directly constructed Server has ShutdownTimeout 0: cancellation is
// immediate and Shutdown adds no grace.
func TestServer_Shutdown_ZeroTimeout_CancelsReviewersAtOnce(t *testing.T) {
	setVar(t, &reviewerShutdownDrain, time.Hour)
	s := &Server{http: &http.Server{ReadHeaderTimeout: time.Second}}
	cancelled := make(chan struct{})
	entered := make(chan struct{})
	reviewer := &scriptedReviewer{steps: []reviewStep{func(ctx context.Context) (*planreview.ReviewVerdict, string, error) {
		close(entered)
		<-ctx.Done()
		close(cancelled)
		return nil, "", ctx.Err()
	}}}
	go func() {
		_, _, _ = s.invokeReview(context.Background(), reviewerInvocation{reviewer: reviewer}, "p", "")
	}()
	waitFor(t, entered, "setup: reviewer entered")
	start := time.Now()
	_ = s.Shutdown(context.Background())
	waitFor(t, cancelled, "reviewer context cancelled")
	if elapsed := time.Since(start); elapsed >= reviewerShutdownGrace {
		t.Fatalf("Shutdown took %s: a reviewer honouring its context must not spend the grace", elapsed)
	}
}

func TestReviewerShutdownDrain_InsideScriptsDevKillWindow(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "scripts", "dev"))
	if err != nil {
		t.Fatalf("read scripts/dev: %v", err)
	}
	if !strings.Contains(string(raw), "fishhawkd killed (SIGKILL after 5s)") {
		t.Fatal("scripts/dev no longer has the 5s SIGKILL branch this clock must fit inside; update reviewerShutdownDrain and this pin")
	}
	if reviewerShutdownDrain >= 5*time.Second {
		t.Fatalf("reviewerShutdownDrain = %s, must be inside scripts/dev's 5s SIGKILL window", reviewerShutdownDrain)
	}
}

func TestReviewerLifetime_Modes(t *testing.T) {
	var l reviewerLifetime
	if l.isClosed() {
		t.Fatal("zero value must be open")
	}
	ctx, release, entered := l.enter(context.Background())
	if !entered {
		t.Fatal("open lifetime must admit")
	}
	release()
	release() // idempotent: a second Done would panic
	if ctx.Err() == nil {
		t.Error("release must cancel the derived context")
	}
	liveCtx, liveRelease, _ := l.enter(context.Background())
	l.closeAndCancel()
	l.closeAndCancel()
	if !errors.Is(liveCtx.Err(), context.Canceled) {
		t.Errorf("closeAndCancel left a live context at %v", liveCtx.Err())
	}
	if ended, still := l.stop(10 * time.Millisecond); ended != 0 || still != 1 {
		t.Errorf("stop with one holder = ended %d, still %d", ended, still)
	}
	liveRelease()
	if ended, still := l.stop(time.Second); ended != 1 || still != 0 {
		t.Errorf("stop after release = ended %d, still %d", ended, still)
	}
	parent := context.Background()
	if got, noop, ok := l.enter(parent); ok || got != parent {
		t.Error("closed lifetime must refuse and return its parent")
	} else {
		noop()
	}
}

// --- plan route: interruption, retries ------------------------------------

// reviewStep is one scripted reviewer call.
type reviewStep func(ctx context.Context) (*planreview.ReviewVerdict, string, error)

func approveStep(context.Context) (*planreview.ReviewVerdict, string, error) {
	return &planreview.ReviewVerdict{Verdict: planreview.VerdictApprove}, "claude-sonnet-4-6", nil
}

func rejectStep(context.Context) (*planreview.ReviewVerdict, string, error) {
	return &planreview.ReviewVerdict{Verdict: planreview.VerdictReject, FreeForm: "no"}, "claude-sonnet-4-6", nil
}

// scriptedReviewer plays steps in call order, repeating the last.
type scriptedReviewer struct {
	calls atomic.Int32
	steps []reviewStep
}

func (r *scriptedReviewer) Review(ctx context.Context, _ string) (*planreview.ReviewVerdict, string, error) {
	n := int(r.calls.Add(1)) - 1
	if n >= len(r.steps) {
		n = len(r.steps) - 1
	}
	return r.steps[n](ctx)
}

// blockStep blocks until its context ends, signalling started on entry.
func blockStep(started chan<- struct{}) reviewStep {
	return func(ctx context.Context) (*planreview.ReviewVerdict, string, error) {
		close(started)
		<-ctx.Done()
		return nil, "claude-sonnet-4-6", ctx.Err()
	}
}

// planReadyRecorder counts plan-ready comments on top of the page recorder.
type planReadyRecorder struct {
	pageClassRecorder
	planReady int
}

func (r *planReadyRecorder) NotifyPlanReady(context.Context, uuid.UUID, *run.Stage, *plan.Plan) error {
	r.planReady++
	return nil
}

// planRouteSeam is one process's plan route over stores a next process shares.
type planRouteSeam struct {
	s      *Server
	sf     *signingFake
	ar     *fakeArtifactRepo
	au     *auditFake
	rr     *promptRunRepo
	rec    *planReadyRecorder
	runID  uuid.UUID
	stage  uuid.UUID
	priv   ed25519.PrivateKey
	body   []byte
	sigHex string
}

// newPlanRouteSeam wires a running gated plan stage whose chain sequences are
// the real append order, so round-relative counting is real.
func newPlanRouteSeam(t *testing.T, reviewer PlanReviewer, workflowSpec []byte) *planRouteSeam {
	t.Helper()
	runID, stageID := uuid.New(), uuid.New()
	s, sf, ar, au, rr := newPlanServerWithReviewer(t, runID, stageID, reviewer, workflowSpec)
	au.stampSequence = true
	st := rr.getStages[stageID]
	st.Type = run.StageTypePlan
	st.State = run.StageStateRunning
	st.RequiresApproval = true
	rr.stagesByRunID = map[uuid.UUID][]*run.Stage{runID: {st}}
	rec := &planReadyRecorder{}
	s.issueNotifier = rec
	priv, _ := sf.issue(t, runID)
	body := validPlanBytes(t)
	return &planRouteSeam{
		s: s, sf: sf, ar: ar, au: au, rr: rr, rec: rec, runID: runID, stage: stageID, priv: priv, body: body,
		sigHex: hex.EncodeToString(ed25519.Sign(priv, signing.ComputeMessage(body))),
	}
}

// post sends the byte-identical signed upload to this seam's current server.
func (p *planRouteSeam) post(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	return shipPlanRequest(t, p.s, p.runID, p.stage, nil, bytes.Clone(p.body), p.sigHex)
}

// nextProcess reboots the seam: a new Server with a later boot marker over the
// same stores, the reviewer set given.
func (p *planRouteSeam) nextProcess(reviewer PlanReviewer) {
	p.s = New(Config{
		Addr: "127.0.0.1:0", SigningRepo: p.sf, ArtifactRepo: p.ar, AuditRepo: p.au, RunRepo: p.rr,
		PlanReviewers: singleReviewerSet{reviewer}, ProcessStart: time.Now().Add(time.Second),
	})
	p.s.issueNotifier = p.rec
}

func (p *planRouteSeam) stageState() run.StageState { return p.rr.getStages[p.stage].State }

func (p *planRouteSeam) transitions() int { return len(p.rr.transitionStageCalls) }

// assertInterrupted reads the response's envelope code and the status.
func assertInterrupted(t *testing.T, w *httptest.ResponseRecorder, status int) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status = %d, want %d:\n%s", w.Code, status, w.Body.String())
	}
	var env struct {
		Error struct {
			Code    string         `json:"code"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if env.Error.Code != "review_round_interrupted" {
		t.Fatalf("code = %q, want review_round_interrupted", env.Error.Code)
	}
	for _, key := range []string{"run_id", "stage_id", "stage"} {
		if env.Error.Details[key] == nil {
			t.Errorf("details missing %s: %v", key, env.Error.Details)
		}
	}
}

// killGatingPlanRound posts the upload with a blocked reviewer and closes the
// lifetime; it returns the response.
func killGatingPlanRound(t *testing.T, p *planRouteSeam, started <-chan struct{}) *httptest.ResponseRecorder {
	t.Helper()
	respCh := make(chan *httptest.ResponseRecorder, 1)
	go func() { respCh <- p.post(t) }()
	waitFor(t, started, "setup: gating reviewer started")
	p.s.reviewers.closeAndCancel()
	select {
	case w := <-respCh:
		return w
	case <-time.After(boundedWait):
		t.Fatal("upload did not return after the lifetime closed")
		return nil
	}
}

// Z48 and structured entry 3.
func TestShipPlan_GatingReviewerKilled_StageNotAdvanced(t *testing.T) {
	started := make(chan struct{})
	p := newPlanRouteSeam(t, &scriptedReviewer{steps: []reviewStep{blockStep(started)}}, specGatingReviewers)
	w := killGatingPlanRound(t, p, started)
	assertInterrupted(t, w, http.StatusServiceUnavailable)
	if got := p.stageState(); got != run.StageStateRunning {
		t.Errorf("stage = %s, want running", got)
	}
	if n := p.transitions(); n != 0 {
		t.Errorf("stage took %d transitions, want none", n)
	}
	if n := p.rec.planReady; n != 0 {
		t.Errorf("plan-ready comments = %d, want none", n)
	}
	for _, category := range []string{"plan_review_failed", "plan_reviewed"} {
		if n := countAuditCategory(p.au, category); n != 0 {
			t.Errorf("%s rows = %d, want none: a stopped round writes no terminal", category, n)
		}
	}
	if n := countAuditCategory(p.au, "plan_review_started"); n != 1 {
		t.Errorf("plan_review_started rows = %d, want 1", n)
	}
}

// Z21: an earlier reject never decides a round shutdown stopped.
func TestDispatchPlanReviews_Interrupted_RejectAndPersonaBlockDoNotTransition(t *testing.T) {
	started := make(chan struct{})
	reviewer := &scriptedReviewer{steps: []reviewStep{rejectStep, blockStep(started)}}
	twoAgents := bytes.Replace(specGatingReviewers, []byte("agent: 1"), []byte("agent: 2"), 1)
	p := newPlanRouteSeam(t, reviewer, twoAgents)
	outcomeCh := make(chan reviewDispatchOutcome, 1)
	go func() {
		outcomeCh <- p.s.dispatchPlanReviews(context.Background(), p.runID, p.stage, p.body, nil, nil, nil, nil, nil)
	}()
	waitFor(t, started, "setup: second reviewer started")
	p.s.reviewers.closeAndCancel()
	var outcome reviewDispatchOutcome
	select {
	case outcome = <-outcomeCh:
	case <-time.After(boundedWait):
		t.Fatal("dispatch did not return after the lifetime closed")
	}
	if !outcome.interrupted || outcome.blocked() {
		t.Fatalf("outcome = %+v, want interrupted with no block", outcome)
	}
	if n := p.transitions(); n != 0 {
		t.Fatalf("stage took %d transitions, want none: the landed reject must not fail a stopped round", n)
	}
	if n := countAuditCategory(p.au, "plan_reviewed"); n != 1 {
		t.Errorf("plan_reviewed rows = %d, want the one that landed", n)
	}
	if n := countAuditCategory(p.au, "plan_review_failed"); n != 0 {
		t.Errorf("plan_review_failed rows = %d, want none", n)
	}
	if n := len(p.rec.pageClass); n != 0 {
		t.Errorf("post-loop page fired %d times over a stopped round", n)
	}
}

// Z47: process A loses the round, process B's real boot sweep names the closure,
// and the byte-identical replay is refused.
func TestShipPlan_GatingRoundLostByRestart_RetryRefused(t *testing.T) {
	started := make(chan struct{})
	p := newPlanRouteSeam(t, &scriptedReviewer{steps: []reviewStep{blockStep(started)}}, specGatingReviewers)
	first := p.body
	assertInterrupted(t, killGatingPlanRound(t, p, started), http.StatusServiceUnavailable)

	p.nextProcess(&scriptedReviewer{steps: []reviewStep{approveStep}})
	if _, err := p.s.reconcileRunOrphanedReviewsForBoot(context.Background(), p.runID); err != nil {
		t.Fatalf("setup: boot sweep: %v", err)
	}
	if n := countAuditCategory(p.au, "plan_review_failed"); n != 1 {
		t.Fatalf("setup: boot closure rows = %d, want 1", n)
	}
	if !bytes.Equal(first, p.body) {
		t.Fatal("setup: replay bytes differ")
	}
	assertInterrupted(t, p.post(t), http.StatusConflict)
	if got := p.stageState(); got != run.StageStateRunning {
		t.Errorf("stage = %s, want running", got)
	}
	if n := p.transitions(); n != 0 {
		t.Errorf("replay moved the stage %d times", n)
	}
	if n := countAuditCategory(p.au, "plan_review_started"); n != 1 {
		t.Errorf("plan_review_started rows = %d: the replay must not start a round", n)
	}
}

// resetToRunning models a stage whose advance was lost, so the guard is reached.
func (p *planRouteSeam) resetToRunning() {
	p.rr.getStages[p.stage].State = run.StageStateRunning
	p.rr.transitionStageCalls = nil
}

func assertIdempotentNoEdge(t *testing.T, p *planRouteSeam) {
	t.Helper()
	w := p.post(t)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 idempotent:\n%s", w.Code, w.Body.String())
	}
	if n := p.transitions(); n != 0 {
		t.Errorf("replay moved the stage %d times", n)
	}
}

func TestShipPlan_LostRoundControls_NotRefused(t *testing.T) {
	t.Run("advisory_round", func(t *testing.T) {
		started := make(chan struct{})
		p := newPlanRouteSeam(t, &scriptedReviewer{steps: []reviewStep{blockStep(started)}}, specAdvisoryReviewers)
		if w := p.post(t); w.Code != http.StatusCreated {
			t.Fatalf("setup: advisory upload = %d", w.Code)
		}
		waitFor(t, started, "setup: advisory reviewer started")
		p.s.reviewers.closeAndCancel()
		p.s.waitBackgroundReviews()
		p.nextProcess(&scriptedReviewer{steps: []reviewStep{approveStep}})
		p.resetToRunning()
		assertIdempotentNoEdge(t, p)
	})
	t.Run("gating_round_settled", func(t *testing.T) {
		p := newPlanRouteSeam(t, &scriptedReviewer{steps: []reviewStep{approveStep}}, specGatingReviewers)
		if w := p.post(t); w.Code != http.StatusCreated {
			t.Fatalf("setup: upload = %d:\n%s", w.Code, w.Body.String())
		}
		// Positive control for every no-plan-ready assertion in this file.
		if p.rec.planReady != 1 {
			t.Fatalf("setup: plan-ready comments = %d, want 1 on a settled round", p.rec.planReady)
		}
		p.nextProcess(&scriptedReviewer{steps: []reviewStep{approveStep}})
		p.resetToRunning()
		assertIdempotentNoEdge(t, p)
	})
}

// Z35: a retry reaching the same closing process.
func TestShipPlan_CurrentProcessRoundAfterClose_RetryRefused(t *testing.T) {
	started := make(chan struct{})
	p := newPlanRouteSeam(t, &scriptedReviewer{steps: []reviewStep{blockStep(started)}}, specGatingReviewers)
	assertInterrupted(t, killGatingPlanRound(t, p, started), http.StatusServiceUnavailable)
	assertInterrupted(t, p.post(t), http.StatusServiceUnavailable)
	if got := p.stageState(); got != run.StageStateRunning {
		t.Errorf("stage = %s, want running", got)
	}
	if n := p.transitions(); n != 0 {
		t.Errorf("stage took %d transitions", n)
	}
	if n := p.rec.planReady; n != 0 {
		t.Errorf("plan-ready comments = %d, want none", n)
	}
	if n := countAuditCategory(p.au, "plan_review_started"); n != 1 {
		t.Errorf("plan_review_started rows = %d, want 1", n)
	}
}

func TestShipPlan_ClosingProcess_SettledRound_RetryIdempotent(t *testing.T) {
	p := newPlanRouteSeam(t, &scriptedReviewer{steps: []reviewStep{approveStep}}, specGatingReviewers)
	if w := p.post(t); w.Code != http.StatusCreated {
		t.Fatalf("setup: upload = %d:\n%s", w.Code, w.Body.String())
	}
	p.s.reviewers.closeAndCancel()
	p.resetToRunning()
	assertIdempotentNoEdge(t, p)
}

// Z23: the lost-round decision over seeded rows.
func TestGatingRoundLostByRestart_Modes(t *testing.T) {
	const head = "aaaa"
	otherStage := uuid.New()
	type seed struct {
		category string
		stage    *uuid.UUID
		seq      int64
		payload  any
	}
	restartClosure := planreview.ReviewFailedPayload{Reason: orphanedReviewRestartReason}
	namedClosure := planreview.ReviewFailedPayload{Reason: orphanedReviewNotRedispatchedReason(redispatchSlugGatingAuthority)}
	realFailure := planreview.ReviewFailedPayload{Reason: "reviewer crashed"}
	cases := []struct {
		name       string
		authority  planreview.AuthorityMode
		startedAt  time.Time
		startStage func(stage uuid.UUID) *uuid.UUID
		startHead  string
		terminals  []seed
		closed     bool
		noRound    bool
		wantLost   bool
	}{
		{name: "advisory", authority: planreview.AuthorityAdvisory, startedAt: beforeBoot},
		{name: "no_round", noRound: true, startedAt: beforeBoot},
		{name: "current_process_round", startedAt: afterBoot},
		{name: "current_process_round_after_close", startedAt: afterBoot, closed: true, wantLost: true},
		{name: "current_process_settled_after_close", startedAt: afterBoot, closed: true, terminals: []seed{
			{category: "implement_reviewed", seq: 11, payload: map[string]any{}},
			{category: "implement_reviewed", seq: 12, payload: map[string]any{}},
		}},
		{name: "other_stage", startedAt: beforeBoot, startStage: func(uuid.UUID) *uuid.UUID { return &otherStage }},
		{name: "other_head", startedAt: beforeBoot, startHead: "bbbb"},
		{name: "zero_terminals", startedAt: beforeBoot, wantLost: true},
		{name: "closed_by_boot", startedAt: beforeBoot, wantLost: true, terminals: []seed{
			{category: "implement_review_failed", seq: 11, payload: restartClosure},
			{category: "implement_review_failed", seq: 12, payload: restartClosure},
		}},
		{name: "closed_run_terminal", startedAt: beforeBoot, wantLost: true, terminals: []seed{
			{category: "implement_review_failed", seq: 11, payload: namedClosure},
			{category: "implement_review_failed", seq: 12, payload: namedClosure},
		}},
		{name: "partial_real_verdicts", startedAt: beforeBoot, wantLost: true, terminals: []seed{
			{category: "implement_reviewed", seq: 11, payload: map[string]any{}},
		}},
		{name: "prior_round_terminals_not_counted", startedAt: beforeBoot, wantLost: true, terminals: []seed{
			{category: "implement_reviewed", seq: 5, payload: map[string]any{}},
			{category: "implement_reviewed", seq: 6, payload: map[string]any{}},
		}},
		{name: "other_stage_terminals_not_counted", startedAt: beforeBoot, wantLost: true, terminals: []seed{
			{category: "implement_reviewed", stage: &otherStage, seq: 11, payload: map[string]any{}},
			{category: "implement_reviewed", stage: &otherStage, seq: 12, payload: map[string]any{}},
		}},
		{name: "all_real_terminals", startedAt: beforeBoot, terminals: []seed{
			{category: "implement_reviewed", seq: 11, payload: map[string]any{}},
			{category: "implement_review_failed", seq: 12, payload: realFailure},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			au := newAuditFake()
			s := New(Config{Addr: "127.0.0.1:0", AuditRepo: au, ProcessStart: bootMarker})
			runID, stageID := uuid.New(), uuid.New()
			if !tc.noRound {
				authority := tc.authority
				if authority == "" {
					authority = planreview.AuthorityGating
				}
				startStage := &stageID
				if tc.startStage != nil {
					startStage = tc.startStage(stageID)
				}
				startHead := tc.startHead
				if startHead == "" {
					startHead = head
				}
				seedRoundRow(t, au, runID, startStage, "implement_review_started", 10, tc.startedAt,
					planreview.ReviewStartedPayload{ConfiguredAgents: 2, Authority: authority, HeadSHA: startHead})
			}
			for _, row := range tc.terminals {
				st := row.stage
				if st == nil {
					st = &stageID
				}
				seedRoundRow(t, au, runID, st, row.category, row.seq, afterBoot, row.payload)
			}
			if tc.closed {
				s.reviewers.closeAndCancel()
			}
			lost, err := s.gatingRoundLostByRestart(context.Background(), runID, implementReviewKind(), stageID, head)
			if err != nil {
				t.Fatalf("gatingRoundLostByRestart: %v", err)
			}
			if lost != tc.wantLost {
				t.Fatalf("lost = %v, want %v", lost, tc.wantLost)
			}
			if got := s.sameHeadGatingRoundLost(context.Background(), runID, stageID, head); got != tc.wantLost {
				t.Fatalf("sameHeadGatingRoundLost = %v, want %v", got, tc.wantLost)
			}
		})
	}

	t.Run("plan_kind_ignores_head", func(t *testing.T) {
		au := newAuditFake()
		s := New(Config{Addr: "127.0.0.1:0", AuditRepo: au, ProcessStart: bootMarker})
		runID, stageID := uuid.New(), uuid.New()
		seedRoundRow(t, au, runID, &stageID, "plan_review_started", 10, beforeBoot,
			planreview.ReviewStartedPayload{ConfiguredAgents: 1, Authority: planreview.AuthorityGating})
		if !s.planRoundLost(context.Background(), runID, stageID) {
			t.Fatal("a prior-process gating plan round with no terminal is lost")
		}
	})

	// A read fault fails closed through both wrappers, on the started read, the
	// terminal read and an undecodable terminal row alike.
	for _, tc := range []struct {
		name  string
		fault func(au *auditFake, runID, stageID uuid.UUID)
	}{
		{"read_error", func(au *auditFake, _, _ uuid.UUID) { au.listByCategoryErr = errors.New("audit outage") }},
		{"terminal_read_error", func(au *auditFake, _, _ uuid.UUID) { au.listByCategoryErrCategory = "implement_reviewed" }},
		{"failed_row_undecodable", func(au *auditFake, runID, stageID uuid.UUID) {
			seedRoundRow(t, au, runID, &stageID, "implement_review_failed", 11, afterBoot, json.RawMessage(`[1]`))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			au := newAuditFake()
			s := New(Config{Addr: "127.0.0.1:0", AuditRepo: au, ProcessStart: bootMarker})
			runID, stageID := uuid.New(), uuid.New()
			seedRoundRow(t, au, runID, &stageID, "implement_review_started", 10, beforeBoot,
				planreview.ReviewStartedPayload{ConfiguredAgents: 1, Authority: planreview.AuthorityGating, HeadSHA: head})
			seedRoundRow(t, au, runID, &stageID, "plan_review_started", 10, beforeBoot,
				planreview.ReviewStartedPayload{ConfiguredAgents: 1, Authority: planreview.AuthorityGating})
			tc.fault(au, runID, stageID)
			if _, err := s.gatingRoundLostByRestart(context.Background(), runID, implementReviewKind(), stageID, head); err == nil {
				t.Fatal("gatingRoundLostByRestart swallowed the fault")
			}
			if !s.sameHeadGatingRoundLost(context.Background(), runID, stageID, head) {
				t.Error("sameHeadGatingRoundLost must fail closed")
			}
			if tc.name == "read_error" && !s.planRoundLost(context.Background(), runID, stageID) {
				t.Error("planRoundLost must fail closed")
			}
		})
	}
}

func seedRoundRow(t *testing.T, au *auditFake, runID uuid.UUID, stageID *uuid.UUID, category string, seq int64, ts time.Time, payload any) {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("setup: marshal: %v", err)
	}
	rid := runID
	au.mu.Lock()
	defer au.mu.Unlock()
	au.seeded = append(au.seeded, &audit.Entry{RunID: &rid, StageID: stageID, Category: category, Sequence: seq, Timestamp: ts, Payload: raw})
}

func TestReviewDispatchOutcome_Modes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		outcome reviewDispatchOutcome
		blocked bool
	}{
		{"empty", reviewDispatchOutcome{}, false},
		{"interrupted", reviewDispatchOutcome{interrupted: true}, false},
		{"undecided", reviewDispatchOutcome{undecided: true}, false},
		{"evidence_write_failed", reviewDispatchOutcome{evidenceWriteFailed: true}, false},
		{"block_reason", reviewDispatchOutcome{blockReason: "rejected"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.outcome.blocked(); got != tc.blocked {
				t.Fatalf("blocked() = %v, want %v", got, tc.blocked)
			}
		})
	}
	loop := reviewLoopOutcome{rejected: true, evidenceWriteFailed: true}
	if !loop.rejected || !loop.evidenceWriteFailed || loop.interrupted {
		t.Fatalf("loop outcome fields = %+v", loop)
	}
}

// --- requirement, lifetime, owners, claim ---------------------------------

// failingArtifactRepo faults the plan read.
type failingArtifactRepo struct{ artifact.Repository }

func (failingArtifactRepo) ListForStage(context.Context, uuid.UUID) ([]*artifact.Artifact, error) {
	return nil, errors.New("artifact outage")
}

// runReadFaultRepo faults GetRun.
type runReadFaultRepo struct{ run.Repository }

func (runReadFaultRepo) GetRun(context.Context, uuid.UUID) (*run.Run, error) {
	return nil, errors.New("run outage")
}

func TestSupplementalPassRequirement_Modes(t *testing.T) {
	zeroAgents := bytes.Replace(specImplementGatingReviewers, []byte("agent: 1"), []byte("agent: 0"), 1)
	cases := []struct {
		name      string
		spec      []byte
		mutate    func(s *Server)
		want      supplementalPassRequirementState
		wantCause bool
		wantAgent int
	}{
		{name: "nil_run_repo", spec: specImplementGatingReviewers, mutate: func(s *Server) { s.cfg.RunRepo = nil }, want: passNotRequired},
		{name: "no_workflow_spec", want: passNotRequired},
		{name: "no_implement_reviewers", spec: specNoReviewers, want: passNotRequired},
		{name: "zero_agents", spec: zeroAgents, want: passNotRequired},
		{name: "no_backend", spec: specImplementGatingReviewers, mutate: func(s *Server) { s.cfg.PlanReviewers = nil }, want: passNotRequired},
		{name: "no_approved_plan", spec: specImplementGatingReviewers, mutate: func(s *Server) { s.cfg.ArtifactRepo = newFakeArtifactRepo() }, want: passNotRequired},
		{name: "advisory", spec: specImplementAdvisoryReviewers, want: passRequiredAdvisory, wantAgent: 1},
		{name: "gating", spec: specImplementGatingReviewers, want: passRequiredGating, wantAgent: 1},
		{name: "run_read_error", spec: specImplementGatingReviewers, mutate: func(s *Server) { s.cfg.RunRepo = runReadFaultRepo{s.cfg.RunRepo} }, want: passRequirementUnknown, wantCause: true},
		{name: "plan_read_error", spec: specImplementGatingReviewers, mutate: func(s *Server) { s.cfg.ArtifactRepo = failingArtifactRepo{s.cfg.ArtifactRepo} }, want: passRequirementUnknown, wantCause: true},
		{name: "spec_unparseable", spec: []byte("version: [unterminated"), want: passRequirementUnknown, wantCause: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, _, _, _, runRow, _ := newImplementReviewServer(t, &scriptedReviewer{steps: []reviewStep{approveStep}}, tc.spec)
			if tc.mutate != nil {
				tc.mutate(s)
			}
			req := s.resolveSupplementalPassRequirement(context.Background(), runRow.ID)
			if req.state != tc.want {
				t.Fatalf("state = %d, want %d (err %v)", req.state, tc.want, req.err)
			}
			if (req.err != nil) != tc.wantCause {
				t.Errorf("err = %v, wantCause %v", req.err, tc.wantCause)
			}
			if req.agentCount != tc.wantAgent {
				t.Errorf("agentCount = %d, want %d", req.agentCount, tc.wantAgent)
			}
			if tc.wantAgent > 0 && (req.reviewers == nil || req.runRow == nil || req.plan == nil) {
				t.Errorf("a required pass must carry its reviewers, run and plan: %+v", req)
			}
		})
	}
}

func TestSupplementalPassLifetime_Formula(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0"})
	over := supplementalPassOverhead
	for _, tc := range []struct {
		name   string
		budget planreview.ReviewBudget
		req    supplementalPassRequirement
		want   time.Duration
	}{
		{"cap_above_floor", planreview.ReviewBudget{Floor: 5 * time.Minute, Cap: 10 * time.Minute}, supplementalPassRequirement{agentCount: 2}, 2*10*time.Minute + over},
		{"floor_above_cap", planreview.ReviewBudget{Floor: 15 * time.Minute, Cap: 10 * time.Minute}, supplementalPassRequirement{agentCount: 2}, 2*15*time.Minute + over},
		{"non_positive_cap_defaults", planreview.ReviewBudget{Floor: time.Minute}, supplementalPassRequirement{agentCount: 1}, planreview.DefaultReviewBudget.Cap + over},
		{"count_clamped_to_one", planreview.ReviewBudget{Floor: time.Minute, Cap: 10 * time.Minute}, supplementalPassRequirement{}, 10*time.Minute + over},
		{"stage_timeout_above_cap", planreview.ReviewBudget{Floor: time.Minute, Cap: 10 * time.Minute}, supplementalPassRequirement{agentCount: 1, reviewers: &spec.ReviewersConfig{ReviewTimeout: "30m"}}, 30*time.Minute + over},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s.cfg.ReviewBudget = tc.budget
			if got := s.supplementalPassLifetime(tc.req); got != tc.want {
				t.Fatalf("lifetime = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestSupplementalPassOwners_Modes(t *testing.T) {
	stage := uuid.New()
	key := supplementalPassKey{stageID: stage, attempt: "a1", headSHA: "h1"}
	deadline := time.Now().Add(time.Hour)
	t.Run("zero_value_and_refusal", func(t *testing.T) {
		var o supplementalPassOwners
		release, holder, ok := o.tryAcquire(key, deadline)
		if !ok || !holder.Equal(deadline) {
			t.Fatalf("first acquire = %v, %v", holder, ok)
		}
		if _, _, again := o.tryAcquire(key, deadline); again {
			t.Fatal("same key must be refused")
		}
		release()
		release() // idempotent
		reacquired, _, ok := o.tryAcquire(key, deadline)
		if !ok {
			t.Fatal("key must be free after release")
		}
		release() // a stale release must not free the new holder
		if _, _, stolen := o.tryAcquire(key, deadline); stolen {
			t.Fatal("a released holder's second release freed the current owner")
		}
		reacquired()
	})
	t.Run("holder_deadline_returned", func(t *testing.T) {
		var o supplementalPassOwners
		release, _, _ := o.tryAcquire(key, deadline)
		defer release()
		_, holder, ok := o.tryAcquire(key, deadline.Add(time.Hour))
		if ok || !holder.Equal(deadline) {
			t.Fatalf("busy acquire = %v, %v; want the holder's own deadline", holder, ok)
		}
	})
	t.Run("each_key_field_independent", func(t *testing.T) {
		var o supplementalPassOwners
		release, _, _ := o.tryAcquire(key, deadline)
		defer release()
		for _, other := range []supplementalPassKey{
			{stageID: uuid.New(), attempt: "a1", headSHA: "h1"},
			{stageID: stage, attempt: "a2", headSHA: "h1"},
			{stageID: stage, attempt: "a1", headSHA: "h2"},
		} {
			if _, _, ok := o.tryAcquire(other, deadline); !ok {
				t.Errorf("key %+v must not be owned by %+v", other, key)
			}
		}
	})
	t.Run("one_winner", func(t *testing.T) {
		var o supplementalPassOwners
		var wins atomic.Int32
		var wg sync.WaitGroup
		for range 64 {
			wg.Go(func() {
				if _, _, ok := o.tryAcquire(key, deadline); ok {
					wins.Add(1)
				}
			})
		}
		wg.Wait()
		if n := wins.Load(); n != 1 {
			t.Fatalf("winners = %d, want exactly 1", n)
		}
	})
}

// claimAudit gives the audit fake the atomic start capability. It is its own type,
// never *auditFake, so a later test-binary adapter cannot change these arms.
type claimAudit struct{ *auditFake }

func (claimAudit) AppendChainedDeduped(context.Context, audit.ChainAppendParams, audit.DedupeSpec) (*audit.Entry, error) {
	return nil, errors.New("not reached by a claim")
}

// plainStartAudit hides the capability.
type plainStartAudit struct{ audit.Repository }

// fullEdgeRepo carries the attempt CAS and the guarded edge.
type fullEdgeRepo struct{ *orchestratorRepo }

func (fullEdgeRepo) TransitionStageFromAttemptGuarded(context.Context, uuid.UUID, run.StageState, run.StageState, string, *run.StageCompletion, run.StageEdgeGuard) (*run.Stage, error) {
	return nil, errors.New("not reached by a claim")
}

// guardOnlyRepo carries the guarded edge but not the attempt CAS, so the CAS
// check alone separates attempt_cas_absent's outcome.
type guardOnlyRepo struct{ run.Repository }

func (guardOnlyRepo) TransitionStageFromAttemptGuarded(context.Context, uuid.UUID, run.StageState, run.StageState, string, *run.StageCompletion, run.StageEdgeGuard) (*run.Stage, error) {
	return nil, errors.New("not reached by a claim")
}

// noGuardRepo carries the attempt CAS only. noGuardedEdgeCapability marks it as
// genuinely lacking the guarded edge, so a test-binary adapter leaves it alone.
type noGuardRepo struct{ *orchestratorRepo }

func (noGuardRepo) noGuardedEdgeCapability() {}

var _ interface{ noGuardedEdgeCapability() } = noGuardRepo{}

// claimSeam is a running implement stage under spec with every capability wired.
func claimSeam(t *testing.T, workflowSpec []byte) (*Server, *run.Run, *run.Stage, *orchestratorRepo, *auditFake) {
	t.Helper()
	s, _, au, rr, runRow, stage := newImplementReviewServer(t, &scriptedReviewer{steps: []reviewStep{approveStep}}, workflowSpec)
	stage.State = run.StageStateRunning
	s.cfg.RunRepo = fullEdgeRepo{rr}
	s.cfg.AuditRepo = claimAudit{au}
	return s, runRow, stage, rr, au
}

func withExemptions(head string) *pullRequestBody {
	return &pullRequestBody{HeadSHA: head, SupplementalScopeExemptions: []scopeExemption{{Path: "a.go", Reason: "r"}}}
}

// TestSupplementalPassClaimHelpers_Modes is Z17.
func TestSupplementalPassClaimHelpers_Modes(t *testing.T) {
	ctx := context.Background()
	assertNoClaim := func(t *testing.T, c supplementalPassClaim, wantUnknown bool) {
		t.Helper()
		if c.claimed || c.busy || c.unknown != wantUnknown {
			t.Fatalf("claim = %+v, want no claim, unknown %v", c, wantUnknown)
		}
		if c.release == nil {
			t.Fatal("release must never be nil")
		}
		c.release()
	}

	t.Run("no_exemptions", func(t *testing.T) {
		s, runRow, stage, _, _ := claimSeam(t, specImplementGatingReviewers)
		assertNoClaim(t, s.claimSupplementalGatingPass(ctx, runRow.ID, stage, &pullRequestBody{HeadSHA: "h"}, false), false)
	})
	t.Run("not_running_implement", func(t *testing.T) {
		s, runRow, stage, _, _ := claimSeam(t, specImplementGatingReviewers)
		stage.State = run.StageStateAwaitingApproval
		assertNoClaim(t, s.claimSupplementalGatingPass(ctx, runRow.ID, stage, withExemptions("h"), false), false)
		stage.State = run.StageStateRunning
		stage.Type = run.StageTypePlan
		assertNoClaim(t, s.claimSupplementalGatingPass(ctx, runRow.ID, stage, withExemptions("h"), false), false)
	})
	t.Run("advisory", func(t *testing.T) {
		s, runRow, stage, _, _ := claimSeam(t, specImplementAdvisoryReviewers)
		assertNoClaim(t, s.claimSupplementalGatingPass(ctx, runRow.ID, stage, withExemptions("h"), false), false)
	})
	t.Run("gating_claimed", func(t *testing.T) {
		s, runRow, stage, _, _ := claimSeam(t, specImplementGatingReviewers)
		before := time.Now()
		c := s.claimSupplementalGatingPass(ctx, runRow.ID, stage, withExemptions("h"), false)
		defer c.release()
		if !c.claimed || c.busy || c.unknown {
			t.Fatalf("claim = %+v", c)
		}
		want := supplementalPassKey{stageID: stage.ID, attempt: run.StageAttemptToken(stage.DispatchedAt), headSHA: "h"}
		if c.key != want {
			t.Errorf("key = %+v, want %+v", c.key, want)
		}
		if c.deadline.Before(before.Add(c.lifetime)) || c.lifetime <= supplementalPassOverhead {
			t.Errorf("deadline %s lifetime %s", c.deadline, c.lifetime)
		}
	})
	t.Run("gating_busy", func(t *testing.T) {
		s, runRow, stage, _, _ := claimSeam(t, specImplementGatingReviewers)
		first := s.claimSupplementalGatingPass(ctx, runRow.ID, stage, withExemptions("h"), false)
		second := s.claimSupplementalGatingPass(ctx, runRow.ID, stage, withExemptions("h"), false)
		if !second.busy || second.claimed {
			t.Fatalf("second claim = %+v", second)
		}
		if !second.holderDeadline.Equal(first.deadline) {
			t.Errorf("holderDeadline = %s, want the holder's %s", second.holderDeadline, first.deadline)
		}
		second.release() // a busy claim's release must not free the holder
		if third := s.claimSupplementalGatingPass(ctx, runRow.ID, stage, withExemptions("h"), false); !third.busy {
			t.Error("a busy claim's release freed the holder")
		}
		first.release()
		if fourth := s.claimSupplementalGatingPass(ctx, runRow.ID, stage, withExemptions("h"), false); !fourth.claimed {
			t.Errorf("after release claim = %+v", fourth)
		} else {
			fourth.release()
		}
	})
	t.Run("discharged_omission_claimed", func(t *testing.T) {
		s, runRow, stage, _, _ := claimSeam(t, specImplementGatingReviewers)
		c := s.claimSupplementalGatingPass(ctx, runRow.ID, stage, &pullRequestBody{HeadSHA: "h"}, true)
		defer c.release()
		if !c.claimed {
			t.Fatalf("forced claim = %+v", c)
		}
	})
	t.Run("requirement_unknown", func(t *testing.T) {
		s, runRow, stage, rr, _ := claimSeam(t, specImplementGatingReviewers)
		rr.runs[runRow.ID].WorkflowSpec = []byte("version: [unterminated")
		assertNoClaim(t, s.claimSupplementalGatingPass(ctx, runRow.ID, stage, withExemptions("h"), false), true)
	})
	t.Run("start_capability_absent", func(t *testing.T) {
		s, runRow, stage, _, au := claimSeam(t, specImplementGatingReviewers)
		s.cfg.AuditRepo = plainStartAudit{au}
		assertNoClaim(t, s.claimSupplementalGatingPass(ctx, runRow.ID, stage, withExemptions("h"), false), true)
	})
	t.Run("attempt_cas_absent", func(t *testing.T) {
		s, runRow, stage, rr, _ := claimSeam(t, specImplementGatingReviewers)
		s.cfg.RunRepo = guardOnlyRepo{rr}
		assertNoClaim(t, s.claimSupplementalGatingPass(ctx, runRow.ID, stage, withExemptions("h"), false), true)
	})
	t.Run("guarded_edge_absent", func(t *testing.T) {
		s, runRow, stage, rr, _ := claimSeam(t, specImplementGatingReviewers)
		s.cfg.RunRepo = noGuardRepo{rr}
		assertNoClaim(t, s.claimSupplementalGatingPass(ctx, runRow.ID, stage, withExemptions("h"), false), true)
	})
	t.Run("empty_attempt", func(t *testing.T) {
		s, runRow, stage, _, _ := claimSeam(t, specImplementGatingReviewers)
		stage.DispatchedAt = nil
		assertNoClaim(t, s.claimSupplementalGatingPass(ctx, runRow.ID, stage, withExemptions("h"), false), true)
	})

	t.Run("deadline_set", func(t *testing.T) {
		s := New(Config{Addr: "127.0.0.1:0"})
		parent, cancelParent := context.WithCancel(ctx)
		deadline := time.Now().Add(time.Hour)
		passCtx, release := s.boundSupplementalPassContext(parent, deadline)
		defer release()
		if got, ok := passCtx.Deadline(); !ok || !got.Equal(deadline) {
			t.Fatalf("deadline = %v %v", got, ok)
		}
		cancelParent()
		if passCtx.Err() != nil {
			t.Fatal("a client disconnect must not revoke the pass")
		}
	})
	t.Run("shutdown_cancels", func(t *testing.T) {
		s := New(Config{Addr: "127.0.0.1:0"})
		passCtx, release := s.boundSupplementalPassContext(ctx, time.Now().Add(time.Hour))
		defer release()
		s.reviewers.closeAndCancel()
		if !errors.Is(passCtx.Err(), context.Canceled) {
			t.Fatalf("pass context after shutdown = %v", passCtx.Err())
		}
	})
	t.Run("release_leaves_lifetime", func(t *testing.T) {
		s := New(Config{Addr: "127.0.0.1:0"})
		passCtx, release := s.boundSupplementalPassContext(ctx, time.Now().Add(time.Hour))
		release()
		if passCtx.Err() == nil {
			t.Error("release must cancel the pass context")
		}
		if _, still := s.reviewers.stop(time.Second); still != 0 {
			t.Errorf("still live = %d after release", still)
		}
	})
	t.Run("closed_keeps_deadline", func(t *testing.T) {
		s := New(Config{Addr: "127.0.0.1:0"})
		s.reviewers.closeAndCancel()
		deadline := time.Now().Add(time.Hour)
		passCtx, release := s.boundSupplementalPassContext(ctx, deadline)
		defer release()
		if got, ok := passCtx.Deadline(); !ok || !got.Equal(deadline) {
			t.Fatalf("deadline = %v %v", got, ok)
		}
		if passCtx.Err() != nil {
			t.Errorf("err = %v", passCtx.Err())
		}
	})

	busy := func(t *testing.T, s *Server, holder time.Time) (*httptest.ResponseRecorder, map[string]any) {
		t.Helper()
		w := httptest.NewRecorder()
		s.writeSupplementalPassBusy(w, httptest.NewRequest(http.MethodPost, "/", nil), uuid.New(), uuid.New(), holder)
		var env struct {
			Error struct {
				Code    string         `json:"code"`
				Details map[string]any `json:"details"`
			} `json:"error"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
			t.Fatalf("decode: %v", err)
		}
		details := env.Error.Details
		if details == nil {
			details = map[string]any{}
		}
		details["code"] = env.Error.Code
		return w, details
	}
	t.Run("busy_open_sets_headers", func(t *testing.T) {
		s := New(Config{Addr: "127.0.0.1:0"})
		w, body := busy(t, s, time.Now().Add(90*time.Minute+500*time.Millisecond))
		if w.Code != http.StatusServiceUnavailable || body["code"] != "review_round_in_flight" {
			t.Fatalf("status %d body %v", w.Code, body)
		}
		for _, key := range []string{"run_id", "stage_id", "stage"} {
			if body[key] == nil {
				t.Errorf("details missing %s: %v", key, body)
			}
		}
		if body["stage"] != "implement" {
			t.Errorf("stage = %v", body["stage"])
		}
		remaining, err := strconv.Atoi(w.Header().Get(reviewPassRemainingHeader))
		if err != nil || remaining < 5400 || remaining > 5401 {
			t.Errorf("remaining header = %q (%v)", w.Header().Get(reviewPassRemainingHeader), err)
		}
		if got := w.Header().Get("Retry-After"); got != "15" {
			t.Errorf("Retry-After = %q, want the poll interval", got)
		}
	})
	t.Run("busy_open_retry_after_is_the_smaller", func(t *testing.T) {
		s := New(Config{Addr: "127.0.0.1:0"})
		w, _ := busy(t, s, time.Now().Add(3200*time.Millisecond))
		if got := w.Header().Get("Retry-After"); got != "4" {
			t.Errorf("Retry-After = %q, want the rounded-up remainder", got)
		}
		if got := w.Header().Get(reviewPassRemainingHeader); got != "4" {
			t.Errorf("remaining = %q, want rounded up", got)
		}
	})
	t.Run("busy_closing_no_headers", func(t *testing.T) {
		s := New(Config{Addr: "127.0.0.1:0"})
		s.reviewers.closeAndCancel()
		w, body := busy(t, s, time.Now().Add(time.Hour))
		if w.Code != http.StatusServiceUnavailable || body["code"] != "review_round_interrupted" {
			t.Fatalf("status %d body %v", w.Code, body)
		}
		if w.Header().Get(reviewPassRemainingHeader) != "" || w.Header().Get("Retry-After") != "" {
			t.Errorf("a stopped round must not read as live: %v", w.Header())
		}
	})
	t.Run("busy_past_holder_deadline", func(t *testing.T) {
		s, runRow, stage, _, _ := claimSeam(t, specImplementGatingReviewers)
		first := s.claimSupplementalGatingPass(ctx, runRow.ID, stage, withExemptions("h"), false)
		defer first.release()
		w, body := busy(t, s, time.Now().Add(-time.Second))
		if w.Code != http.StatusConflict || body["code"] != "review_round_interrupted" {
			t.Fatalf("status %d body %v", w.Code, body)
		}
		if w.Header().Get(reviewPassRemainingHeader) != "" || w.Header().Get("Retry-After") != "" {
			t.Errorf("an owner past its deadline is not proven live: %v", w.Header())
		}
		if again := s.claimSupplementalGatingPass(ctx, runRow.ID, stage, withExemptions("h"), false); !again.busy {
			t.Errorf("ownership must stay held past the deadline: %+v", again)
		}
	})
}

// --- fences ----------------------------------------------------------------

// fenceRepo records the attempt-pinned and guarded edges, mimicking the store's
// comparisons. It carries TransitionStage so a capability-absent or token-dropping
// mutant has somewhere to write.
type fenceRepo struct {
	run.Repository
	state   run.StageState
	attempt string
	storeE  error
	writes  int
	calls   int
}

func (f *fenceRepo) TransitionStage(_ context.Context, id uuid.UUID, to run.StageState, _ *run.StageCompletion) (*run.Stage, error) {
	f.calls++
	f.writes++
	f.state = to
	return &run.Stage{ID: id, State: to}, nil
}

func (f *fenceRepo) compareAndMove(id uuid.UUID, from, to run.StageState, expected string) (*run.Stage, error) {
	if f.storeE != nil {
		return nil, f.storeE
	}
	if f.state != from {
		return nil, run.StageStateChangedError{StageID: id, Expected: from, Actual: f.state}
	}
	if expected != "" && expected != f.attempt {
		return nil, run.StageAttemptChangedError{StageID: id, Expected: expected, Actual: f.attempt}
	}
	f.writes++
	f.state = to
	return &run.Stage{ID: id, State: to}, nil
}

// casRepo carries the attempt CAS only.
type casRepo struct{ *fenceRepo }

func (r casRepo) TransitionStageFromAttempt(_ context.Context, id uuid.UUID, from, to run.StageState, expected string, _ *run.StageCompletion) (*run.Stage, error) {
	r.calls++
	return r.compareAndMove(id, from, to, expected)
}

func (casRepo) noGuardedEdgeCapability() {}

var _ interface{ noGuardedEdgeCapability() } = casRepo{}

// guardedRepo also carries the guarded edge, over seeded rows.
type guardedRepo struct {
	casRepo
	rows []run.StageEdgeGuardRow
}

func (r guardedRepo) TransitionStageFromAttemptGuarded(_ context.Context, id uuid.UUID, from, to run.StageState, expected string, _ *run.StageCompletion, guard run.StageEdgeGuard) (*run.Stage, error) {
	r.calls++
	if (expected == "" && !guard.LegacyIdentity) || len(guard.Categories) == 0 || guard.Decide == nil {
		return nil, run.ErrStageEdgeGuardInvalid
	}
	if err := guard.Decide(r.rows); err != nil {
		return nil, err
	}
	return r.compareAndMove(id, from, to, expected)
}

// TestSupplementalTerminalFence_Modes is Z22.
func TestSupplementalTerminalFence_Modes(t *testing.T) {
	ctx := context.Background()
	stageID := uuid.New()
	boom := errors.New("store outage")
	category := run.FailureB
	reason := "rejected"
	failed := &run.StageCompletion{FailureCategory: &category, FailureReason: &reason}

	pinned := []struct {
		name      string
		state     run.StageState
		attempt   string
		storeE    error
		to        run.StageState
		wantState run.StageState
		wantErr   func(error) bool
	}{
		{name: "current_fail", state: run.StageStateRunning, attempt: "a1", to: run.StageStateFailed, wantState: run.StageStateFailed},
		{name: "current_advance", state: run.StageStateRunning, attempt: "a1", to: run.StageStateAwaitingApproval, wantState: run.StageStateAwaitingApproval},
		{name: "other_attempt", state: run.StageStateRunning, attempt: "a2", to: run.StageStateFailed, wantState: run.StageStateRunning, wantErr: func(err error) bool {
			var changed run.StageAttemptChangedError
			return errors.As(err, &changed)
		}},
		{name: "not_running", state: run.StageStateFailed, attempt: "a1", to: run.StageStateAwaitingApproval, wantState: run.StageStateFailed, wantErr: func(err error) bool {
			var changed run.StageStateChangedError
			return errors.As(err, &changed)
		}},
		{name: "store_error", state: run.StageStateRunning, attempt: "a1", storeE: boom, to: run.StageStateFailed, wantState: run.StageStateRunning, wantErr: func(err error) bool { return errors.Is(err, boom) }},
	}
	for _, tc := range pinned {
		t.Run(tc.name, func(t *testing.T) {
			f := &fenceRepo{state: tc.state, attempt: tc.attempt, storeE: tc.storeE}
			s := New(Config{Addr: "127.0.0.1:0", RunRepo: casRepo{f}})
			_, err := s.transitionStageAtAttempt(ctx, stageID, "a1", tc.to, failed)
			if tc.wantErr == nil && err != nil {
				t.Fatalf("err = %v", err)
			}
			if tc.wantErr != nil && !tc.wantErr(err) {
				t.Fatalf("err = %v", err)
			}
			if f.state != tc.wantState {
				t.Fatalf("state = %s, want %s", f.state, tc.wantState)
			}
		})
	}
	t.Run("empty_attempt_refused", func(t *testing.T) {
		f := &fenceRepo{state: run.StageStateRunning, attempt: "a1"}
		s := New(Config{Addr: "127.0.0.1:0", RunRepo: casRepo{f}})
		if _, err := s.transitionStageAtAttempt(ctx, stageID, "", run.StageStateFailed, failed); !errors.Is(err, errSupplementalAttemptUnknown) {
			t.Fatalf("err = %v", err)
		}
		if f.calls != 0 {
			t.Fatalf("repo calls = %d, want zero", f.calls)
		}
	})
	t.Run("capability_absent", func(t *testing.T) {
		f := &fenceRepo{state: run.StageStateRunning, attempt: "a1"}
		s := New(Config{Addr: "127.0.0.1:0", RunRepo: f})
		if _, err := s.transitionStageAtAttempt(ctx, stageID, "a1", run.StageStateFailed, failed); !errors.Is(err, errSupplementalAttemptCASAbsent) {
			t.Fatalf("err = %v", err)
		}
		if f.writes != 0 {
			t.Fatalf("writes = %d, want zero: no unpinned fallback", f.writes)
		}
	})

	refused := errors.New("evidence refuses the edge")
	guard := func(err error) run.StageEdgeGuard {
		return run.StageEdgeGuard{Categories: []string{"implement_reviewed"}, Decide: func([]run.StageEdgeGuardRow) error { return err }}
	}
	t.Run("guarded_advance", func(t *testing.T) {
		f := &fenceRepo{state: run.StageStateRunning, attempt: "a1"}
		s := New(Config{Addr: "127.0.0.1:0", RunRepo: guardedRepo{casRepo: casRepo{f}}})
		if err := s.transitionStageAtAttemptGuarded(ctx, stageID, "a1", run.StageStateAwaitingApproval, guard(nil)); err != nil {
			t.Fatalf("err = %v", err)
		}
		if f.state != run.StageStateAwaitingApproval {
			t.Fatalf("state = %s", f.state)
		}
	})
	t.Run("guarded_refused", func(t *testing.T) {
		f := &fenceRepo{state: run.StageStateRunning, attempt: "a1"}
		s := New(Config{Addr: "127.0.0.1:0", RunRepo: guardedRepo{casRepo: casRepo{f}}})
		if err := s.transitionStageAtAttemptGuarded(ctx, stageID, "a1", run.StageStateAwaitingApproval, guard(refused)); !errors.Is(err, refused) {
			t.Fatalf("err = %v", err)
		}
		if f.writes != 0 {
			t.Fatalf("writes = %d", f.writes)
		}
	})
	t.Run("guarded_empty_attempt", func(t *testing.T) {
		f := &fenceRepo{state: run.StageStateRunning, attempt: "a1"}
		s := New(Config{Addr: "127.0.0.1:0", RunRepo: guardedRepo{casRepo: casRepo{f}}})
		if err := s.transitionStageAtAttemptGuarded(ctx, stageID, "", run.StageStateAwaitingApproval, guard(nil)); !errors.Is(err, run.ErrStageEdgeGuardInvalid) {
			t.Fatalf("err = %v", err)
		}
		if f.writes != 0 {
			t.Fatalf("writes = %d", f.writes)
		}
	})
	t.Run("guarded_capability_absent", func(t *testing.T) {
		f := &fenceRepo{state: run.StageStateRunning, attempt: "a1"}
		s := New(Config{Addr: "127.0.0.1:0", RunRepo: casRepo{f}})
		if err := s.transitionStageAtAttemptGuarded(ctx, stageID, "a1", run.StageStateAwaitingApproval, guard(nil)); !errors.Is(err, errSupplementalAttemptCASAbsent) {
			t.Fatalf("err = %v", err)
		}
		if f.writes != 0 {
			t.Fatalf("writes = %d, want zero: no unguarded fallback", f.writes)
		}
	})
}

// --- real Postgres ---------------------------------------------------------

// pgEdgeSeam is one real running implement stage with real run and audit stores.
type pgEdgeSeam struct {
	pool    *pgxpool.Pool
	repo    run.Repository
	guarded run.GuardedStageTransitioner
	auditR  audit.Repository
	runID   uuid.UUID
	stageID uuid.UUID
	attempt string
}

func newPGEdgeSeam(t *testing.T) *pgEdgeSeam {
	t.Helper()
	pool := pgtest.NewPool(t)
	repo := run.NewPostgresRepository(pool)
	guarded, ok := repo.(run.GuardedStageTransitioner)
	if !ok {
		t.Fatal("setup: the postgres repository lacks the guarded edge")
	}
	s := &pgEdgeSeam{pool: pool, repo: repo, guarded: guarded, auditR: audit.NewPostgresRepository(pool)}
	s.runID, _ = seedPGReviewRun(t, repo)
	s.stageID, s.attempt = s.addRunningStage(t, 1)
	return s
}

func (e *pgEdgeSeam) addRunningStage(t *testing.T, sequence int) (uuid.UUID, string) {
	t.Helper()
	ctx := context.Background()
	st, err := e.repo.CreateStage(ctx, run.CreateStageParams{
		RunID: e.runID, Sequence: sequence, Type: run.StageTypeImplement,
		ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code",
	})
	if err != nil {
		t.Fatalf("setup: CreateStage: %v", err)
	}
	for _, to := range []run.StageState{run.StageStateDispatched, run.StageStateRunning} {
		if st, err = e.repo.TransitionStage(ctx, st.ID, to, nil); err != nil {
			t.Fatalf("setup: transition to %s: %v", to, err)
		}
	}
	return st.ID, run.StageAttemptToken(st.DispatchedAt)
}

func (e *pgEdgeSeam) state(t *testing.T) run.StageState {
	t.Helper()
	st, err := e.repo.GetStage(context.Background(), e.stageID)
	if err != nil {
		t.Fatalf("GetStage: %v", err)
	}
	return st.State
}

func (e *pgEdgeSeam) appendRow(t *testing.T, category string, payload string) {
	t.Helper()
	kind := audit.ActorKind("system")
	if _, err := e.auditR.AppendChained(context.Background(), audit.ChainAppendParams{
		RunID: e.runID, StageID: &e.stageID, Timestamp: time.Now().UTC(), Category: category, ActorKind: &kind, Payload: []byte(payload),
	}); err != nil {
		t.Fatalf("setup: append %s: %v", category, err)
	}
}

func noopGuard() run.StageEdgeGuard {
	return run.StageEdgeGuard{Categories: []string{"implement_reviewed"}, Decide: func([]run.StageEdgeGuardRow) error { return nil }}
}

// nowaitRefused probes a row lock from another connection, reporting whether
// Postgres refused with lock_not_available (55P03).
func nowaitRefused(t *testing.T, pool *pgxpool.Pool, table string, id uuid.UUID) bool {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("setup: begin probe: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var one int
	err = tx.QueryRow(ctx, "SELECT 1 FROM "+table+" WHERE id=$1 FOR UPDATE NOWAIT", id).Scan(&one)
	if err == nil {
		return false
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "55P03" {
		return true
	}
	t.Fatalf("probe %s: %v", table, err)
	return false
}

// awaitBlockedBy waits until some backend is blocked by holderPID.
func awaitBlockedBy(t *testing.T, pool *pgxpool.Pool, holderPID int32) {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(boundedWait)
	for time.Now().Before(deadline) {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE $1 = ANY(pg_blocking_pids(pid))`, holderPID).Scan(&n); err != nil {
			t.Fatalf("poll pg_blocking_pids: %v", err)
		}
		if n > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no backend blocked on holder %d within %s", holderPID, boundedWait)
}

func TestGuardedStageEdge_Modes_PG(t *testing.T) {
	ctx := context.Background()
	refusal := errors.New("evidence refuses")

	t.Run("no_rows_advances", func(t *testing.T) {
		e := newPGEdgeSeam(t)
		var saw []run.StageEdgeGuardRow
		guard := run.StageEdgeGuard{Categories: []string{"implement_reviewed"}, Decide: func(rows []run.StageEdgeGuardRow) error {
			saw = rows
			return nil
		}}
		st, err := e.guarded.TransitionStageFromAttemptGuarded(ctx, e.stageID, run.StageStateRunning, run.StageStateAwaitingApproval, e.attempt, nil, guard)
		if err != nil {
			t.Fatalf("edge: %v", err)
		}
		if len(saw) != 0 || st.State != run.StageStateAwaitingApproval || e.state(t) != run.StageStateAwaitingApproval {
			t.Fatalf("rows %d, stage %s, committed %s", len(saw), st.State, e.state(t))
		}
	})
	t.Run("decide_refusal_rolls_back", func(t *testing.T) {
		e := newPGEdgeSeam(t)
		var fired atomic.Int32
		observable, ok := e.repo.(run.StageTerminalObservable)
		if !ok {
			t.Fatal("setup: repo is not observable")
		}
		observable.SetStageTerminalObserver(func(context.Context, *run.Stage) { fired.Add(1) })
		_, err := e.guarded.TransitionStageFromAttemptGuarded(ctx, e.stageID, run.StageStateRunning, run.StageStateSucceeded, e.attempt, nil,
			run.StageEdgeGuard{Categories: []string{"implement_reviewed"}, Decide: func([]run.StageEdgeGuardRow) error { return refusal }})
		if err != refusal { //nolint:errorlint // the refusal must come back unwrapped
			t.Fatalf("err = %v, want the refusal unwrapped", err)
		}
		if got := e.state(t); got != run.StageStateRunning {
			t.Fatalf("committed state = %s", got)
		}
		if fired.Load() != 0 {
			t.Error("observer fired for a refused edge")
		}
	})
	t.Run("state_drift", func(t *testing.T) {
		e := newPGEdgeSeam(t)
		if _, err := e.repo.TransitionStage(ctx, e.stageID, run.StageStateAwaitingApproval, nil); err != nil {
			t.Fatalf("setup: %v", err)
		}
		_, err := e.guarded.TransitionStageFromAttemptGuarded(ctx, e.stageID, run.StageStateRunning, run.StageStateSucceeded, e.attempt, nil, noopGuard())
		var changed run.StageStateChangedError
		if !errors.As(err, &changed) {
			t.Fatalf("err = %v", err)
		}
		if got := e.state(t); got != run.StageStateAwaitingApproval {
			t.Fatalf("committed state = %s", got)
		}
	})
	t.Run("attempt_drift", func(t *testing.T) {
		e := newPGEdgeSeam(t)
		_, err := e.guarded.TransitionStageFromAttemptGuarded(ctx, e.stageID, run.StageStateRunning, run.StageStateAwaitingApproval, "other-attempt", nil, noopGuard())
		var changed run.StageAttemptChangedError
		if !errors.As(err, &changed) {
			t.Fatalf("err = %v", err)
		}
		if got := e.state(t); got != run.StageStateRunning {
			t.Fatalf("committed state = %s", got)
		}
	})
	t.Run("empty_attempt_no_sql", func(t *testing.T) {
		e := newPGEdgeSeam(t)
		ran := false
		_, err := e.guarded.TransitionStageFromAttemptGuarded(ctx, e.stageID, run.StageStateRunning, run.StageStateAwaitingApproval, "", nil,
			run.StageEdgeGuard{Categories: []string{"implement_reviewed"}, Decide: func([]run.StageEdgeGuardRow) error {
				ran = true
				return nil
			}})
		if !errors.Is(err, run.ErrStageEdgeGuardInvalid) || ran {
			t.Fatalf("err = %v, decide ran %v", err, ran)
		}
		if got := e.state(t); got != run.StageStateRunning {
			t.Fatalf("committed state = %s", got)
		}
	})
	t.Run("nil_decide_no_sql", func(t *testing.T) {
		e := newPGEdgeSeam(t)
		for name, guard := range map[string]run.StageEdgeGuard{
			"nil_decide":    {Categories: []string{"implement_reviewed"}},
			"no_categories": {Decide: func([]run.StageEdgeGuardRow) error { return nil }},
		} {
			if _, err := e.guarded.TransitionStageFromAttemptGuarded(ctx, e.stageID, run.StageStateRunning, run.StageStateAwaitingApproval, e.attempt, nil, guard); !errors.Is(err, run.ErrStageEdgeGuardInvalid) {
				t.Errorf("%s: err = %v", name, err)
			}
		}
		if got := e.state(t); got != run.StageStateRunning {
			t.Fatalf("committed state = %s", got)
		}
	})
	t.Run("missing_stage", func(t *testing.T) {
		e := newPGEdgeSeam(t)
		if _, err := e.guarded.TransitionStageFromAttemptGuarded(ctx, uuid.New(), run.StageStateRunning, run.StageStateAwaitingApproval, e.attempt, nil, noopGuard()); !errors.Is(err, run.ErrNotFound) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("failed_needs_category", func(t *testing.T) {
		e := newPGEdgeSeam(t)
		if _, err := e.guarded.TransitionStageFromAttemptGuarded(ctx, e.stageID, run.StageStateRunning, run.StageStateFailed, e.attempt, nil, noopGuard()); err == nil {
			t.Fatal("a move into failed with no category must be refused")
		}
		if got := e.state(t); got != run.StageStateRunning {
			t.Fatalf("committed state = %s", got)
		}
	})
	t.Run("observer_after_commit", func(t *testing.T) {
		e := newPGEdgeSeam(t)
		observable, ok := e.repo.(run.StageTerminalObservable)
		if !ok {
			t.Fatal("setup: repo is not observable")
		}
		var seen atomic.Pointer[run.StageState]
		observable.SetStageTerminalObserver(func(ctx context.Context, st *run.Stage) {
			committed, err := e.repo.GetStage(ctx, st.ID)
			if err == nil {
				seen.Store(&committed.State)
			}
		})
		if _, err := e.guarded.TransitionStageFromAttemptGuarded(ctx, e.stageID, run.StageStateRunning, run.StageStateSucceeded, e.attempt, nil, noopGuard()); err != nil {
			t.Fatalf("edge: %v", err)
		}
		got := seen.Load()
		if got == nil || *got != run.StageStateSucceeded {
			t.Fatalf("observer read committed state %v, want succeeded", got)
		}
	})
	t.Run("cancelled_ctx_no_edge", func(t *testing.T) {
		e := newPGEdgeSeam(t)
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := e.guarded.TransitionStageFromAttemptGuarded(cancelled, e.stageID, run.StageStateRunning, run.StageStateAwaitingApproval, e.attempt, nil, noopGuard()); err == nil {
			t.Fatal("a cancelled context must take no edge")
		}
		if got := e.state(t); got != run.StageStateRunning {
			t.Fatalf("committed state = %s", got)
		}
	})
	// A blocked store call honours the deadline at each statement, and a fault at
	// any of them takes no edge. An ACCESS EXCLUSIVE holder is what blocks a read.
	for _, tc := range []struct {
		name, hold string
		target     func(e *pgEdgeSeam) []any
	}{
		{"deadline_on_stage_lock", `SELECT 1 FROM stages WHERE id=$1 FOR UPDATE`, func(e *pgEdgeSeam) []any { return []any{e.stageID} }},
		{"deadline_on_run_lock", `SELECT 1 FROM runs WHERE id=$1 FOR UPDATE`, func(e *pgEdgeSeam) []any { return []any{e.runID} }},
		{"deadline_on_evidence_read", `LOCK TABLE audit_entries IN ACCESS EXCLUSIVE MODE`, func(*pgEdgeSeam) []any { return nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newPGEdgeSeam(t)
			holder, err := e.pool.Begin(ctx)
			if err != nil {
				t.Fatalf("setup: begin: %v", err)
			}
			defer func() { _ = holder.Rollback(ctx) }()
			if _, err := holder.Exec(ctx, tc.hold, tc.target(e)...); err != nil {
				t.Fatalf("setup: hold: %v", err)
			}
			short, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
			defer cancel()
			decided := false
			guard := run.StageEdgeGuard{Categories: []string{"implement_reviewed"}, Decide: func([]run.StageEdgeGuardRow) error {
				decided = true
				return nil
			}}
			if _, err := e.guarded.TransitionStageFromAttemptGuarded(short, e.stageID, run.StageStateRunning, run.StageStateAwaitingApproval, e.attempt, nil, guard); err == nil {
				t.Fatal("a store fault must take no edge")
			}
			if decided {
				t.Error("Decide ran over evidence that was never read")
			}
			if err := holder.Rollback(ctx); err != nil {
				t.Fatalf("setup: release: %v", err)
			}
			if got := e.state(t); got != run.StageStateRunning {
				t.Fatalf("committed state = %s", got)
			}
		})
	}
	t.Run("legacy_identity_nil_dispatch_advances", func(t *testing.T) {
		e := newPGEdgeSeam(t)
		if _, err := e.pool.Exec(ctx, `UPDATE stages SET dispatched_at=NULL WHERE id=$1`, e.stageID); err != nil {
			t.Fatalf("setup: %v", err)
		}
		guard := noopGuard()
		guard.LegacyIdentity = true
		if _, err := e.guarded.TransitionStageFromAttemptGuarded(ctx, e.stageID, run.StageStateRunning, run.StageStateAwaitingApproval, "", nil, guard); err != nil {
			t.Fatalf("edge: %v", err)
		}
		if got := e.state(t); got != run.StageStateAwaitingApproval {
			t.Fatalf("committed state = %s", got)
		}
	})
	t.Run("legacy_identity_anchored_row_refused", func(t *testing.T) {
		e := newPGEdgeSeam(t)
		guard := noopGuard()
		guard.LegacyIdentity = true
		_, err := e.guarded.TransitionStageFromAttemptGuarded(ctx, e.stageID, run.StageStateRunning, run.StageStateAwaitingApproval, "", nil, guard)
		var changed run.StageAttemptChangedError
		if !errors.As(err, &changed) {
			t.Fatalf("err = %v: an empty identity must never match an anchored row", err)
		}
		if got := e.state(t); got != run.StageStateRunning {
			t.Fatalf("committed state = %s", got)
		}
	})
	t.Run("multi_category_rows_visible", func(t *testing.T) {
		e := newPGEdgeSeam(t)
		e.appendRow(t, "implement_reviewed", `{"verdict":"approve"}`)
		e.appendRow(t, "supplemental_review_started", `{"head_sha":"h"}`)
		e.appendRow(t, "pull_request_opened", `{"ignored":true}`)
		var saw []run.StageEdgeGuardRow
		guard := run.StageEdgeGuard{
			Categories: []string{"implement_reviewed", "supplemental_review_started"},
			Decide: func(rows []run.StageEdgeGuardRow) error {
				saw = rows
				return nil
			},
		}
		if _, err := e.guarded.TransitionStageFromAttemptGuarded(ctx, e.stageID, run.StageStateRunning, run.StageStateAwaitingApproval, e.attempt, nil, guard); err != nil {
			t.Fatalf("edge: %v", err)
		}
		if len(saw) != 2 {
			t.Fatalf("rows = %+v, want the two named categories", saw)
		}
		for _, row := range saw {
			if row.StageID == nil || *row.StageID != e.stageID || row.Sequence == 0 || len(row.Payload) == 0 {
				t.Errorf("row not carried whole: %+v", row)
			}
		}
		if saw[0].Category == saw[1].Category {
			t.Errorf("categories = %s, %s", saw[0].Category, saw[1].Category)
		}
	})
}

// Mechanism: with the read above the run lock, Decide runs before the holder's
// commit, sees nothing, and the edge commits over the reject.
func TestGuardedStageEdge_ReadFollowsRunLock_PG(t *testing.T) {
	ctx := context.Background()
	e := newPGEdgeSeam(t)
	holder, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("setup: begin: %v", err)
	}
	t.Cleanup(func() { _ = holder.Rollback(context.Background()) })
	var holderPID int32
	if err := holder.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&holderPID); err != nil {
		t.Fatalf("setup: pid: %v", err)
	}
	var one int
	if err := holder.QueryRow(ctx, `SELECT 1 FROM runs WHERE id=$1 FOR UPDATE`, e.runID).Scan(&one); err != nil {
		t.Fatalf("setup: hold run row: %v", err)
	}

	refusal := errors.New("reject at this head")
	var decided atomic.Int32
	var sawRows atomic.Int32
	result := make(chan error, 1)
	go func() {
		_, gerr := e.guarded.TransitionStageFromAttemptGuarded(context.WithoutCancel(ctx), e.stageID, run.StageStateRunning, run.StageStateAwaitingApproval, e.attempt, nil,
			run.StageEdgeGuard{Categories: []string{"implement_reviewed"}, Decide: func(rows []run.StageEdgeGuardRow) error {
				decided.Add(1)
				sawRows.Store(int32(len(rows))) //nolint:gosec // a handful of test rows
				if len(rows) > 0 {
					return refusal
				}
				return nil
			}})
		result <- gerr
	}()
	awaitBlockedBy(t, e.pool, holderPID)
	if decided.Load() != 0 {
		t.Fatal("Decide ran before the run lock was granted")
	}
	if !nowaitRefused(t, e.pool, "stages", e.stageID) {
		t.Fatal("the stage row is not locked first: the stage -> run order is broken")
	}

	kind := audit.ActorKind("system")
	if _, err := audit.AppendChainedTx(ctx, holder, audit.ChainAppendParams{
		RunID: e.runID, StageID: &e.stageID, Timestamp: time.Now().UTC(), Category: "implement_reviewed", ActorKind: &kind, Payload: []byte(`{"verdict":"reject"}`),
	}); err != nil {
		t.Fatalf("setup: append under the held lock: %v", err)
	}
	if err := holder.Commit(ctx); err != nil {
		t.Fatalf("setup: commit: %v", err)
	}
	select {
	case gerr := <-result:
		if gerr != refusal { //nolint:errorlint // the refusal must come back unwrapped
			t.Fatalf("edge err = %v, want the refusal: Decide saw %d rows", gerr, sawRows.Load())
		}
	case <-time.After(boundedWait):
		t.Fatal("the edge did not finish after the holder committed")
	}
	if got := e.state(t); got != run.StageStateRunning {
		t.Fatalf("committed state = %s: the edge advanced over a reject it could have read", got)
	}
}

// appendResult is one chained append's outcome.
type appendResult struct {
	entry *audit.Entry
	err   error
}

// awaitAnyBlocked waits until a backend in this database is blocked on a lock.
func awaitAnyBlocked(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	deadline := time.Now().Add(boundedWait)
	for time.Now().Before(deadline) {
		var n int
		if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND cardinality(pg_blocking_pids(pid)) > 0`).Scan(&n); err != nil {
			t.Fatalf("poll pg_blocking_pids: %v", err)
		}
		if n > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no backend blocked within %s", boundedWait)
}

func TestGuardedStageEdge_HoldsBothLocksThroughDecide_PG(t *testing.T) {
	ctx := context.Background()
	e := newPGEdgeSeam(t)
	inDecide := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseDecide := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseDecide)
	result := make(chan error, 1)
	go func() {
		_, gerr := e.guarded.TransitionStageFromAttemptGuarded(ctx, e.stageID, run.StageStateRunning, run.StageStateAwaitingApproval, e.attempt, nil,
			run.StageEdgeGuard{Categories: []string{"implement_reviewed"}, Decide: func([]run.StageEdgeGuardRow) error {
				close(inDecide)
				<-release
				return nil
			}})
		result <- gerr
	}()
	waitFor(t, inDecide, "setup: edge reached Decide")
	if !nowaitRefused(t, e.pool, "stages", e.stageID) {
		t.Error("the stage row must stay locked through Decide")
	}
	if !nowaitRefused(t, e.pool, "runs", e.runID) {
		t.Error("the run row must stay locked through Decide")
	}

	kind := audit.ActorKind("system")
	appended := make(chan appendResult, 1)
	go func() {
		entry, aerr := e.auditR.AppendChained(ctx, audit.ChainAppendParams{
			RunID: e.runID, StageID: &e.stageID, Timestamp: time.Now().UTC(), Category: "implement_reviewed", ActorKind: &kind, Payload: []byte(`{"verdict":"reject"}`),
		})
		appended <- appendResult{entry: entry, err: aerr}
	}()
	awaitAnyBlocked(t, e.pool)
	select {
	case <-appended:
		t.Fatal("a chained append landed while the edge held the run lock")
	default:
	}
	if got := e.state(t); got != run.StageStateRunning {
		t.Fatalf("state before release = %s", got)
	}
	releaseDecide()
	select {
	case gerr := <-result:
		if gerr != nil {
			t.Fatalf("edge: %v", gerr)
		}
	case <-time.After(boundedWait):
		t.Fatal("the edge did not commit after release")
	}
	select {
	case got := <-appended:
		if got.err != nil || got.entry == nil || got.entry.Sequence == 0 {
			t.Fatalf("append after the edge = %+v", got)
		}
	case <-time.After(boundedWait):
		t.Fatal("the append did not land after the edge committed")
	}
	// A reject committed after the edge stays outside the guarantee: both landed.
	if got := e.state(t); got != run.StageStateAwaitingApproval {
		t.Fatalf("final state = %s", got)
	}
}

func TestGuardedStageEdge_VerdictWriterNoDeadlock_PG(t *testing.T) {
	ctx := context.Background()
	e := newPGEdgeSeam(t)
	const rounds = 12
	stages := make([]uuid.UUID, rounds)
	attempts := make([]string, rounds)
	for i := range rounds {
		stages[i], attempts[i] = e.addRunningStage(t, i+2)
	}
	errCh := make(chan error, rounds*2)
	var wg sync.WaitGroup
	kind := audit.ActorKind("system")
	for i := range rounds {
		wg.Go(func() {
			_, err := e.auditR.AppendChained(ctx, audit.ChainAppendParams{
				RunID: e.runID, StageID: &stages[i], Timestamp: time.Now().UTC(), Category: "implement_reviewed", ActorKind: &kind, Payload: []byte(`{"verdict":"approve"}`),
			})
			errCh <- err
		})
		wg.Go(func() {
			_, err := e.guarded.TransitionStageFromAttemptGuarded(ctx, stages[i], run.StageStateRunning, run.StageStateAwaitingApproval, attempts[i], nil, noopGuard())
			errCh <- err
		})
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	waitFor(t, done, "writers and edges finishing without a deadlock")
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Errorf("concurrent writer: %v", err)
		}
	}
}

// Z54: no row or advisory lock crosses a reviewer invocation.
func TestReviewRound_HoldsNoDatabaseLock_AcrossKill_PG(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	repo := run.NewPostgresRepository(pool)
	created, err := repo.CreateRun(ctx, run.CreateRunParams{
		Repo: "kuhlman-labs/fishhawk", WorkflowID: "feature_change", WorkflowSHA: "deadbeef",
		TriggerSource: run.TriggerCLI, RunnerKind: run.RunnerKindLocal, WorkflowSpec: specGatingReviewers,
	})
	if err != nil {
		t.Fatalf("setup: CreateRun: %v", err)
	}
	st, err := repo.CreateStage(ctx, run.CreateStageParams{
		RunID: created.ID, Sequence: 0, Type: run.StageTypePlan, ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code",
	})
	if err != nil {
		t.Fatalf("setup: CreateStage: %v", err)
	}
	started := make(chan struct{})
	s := New(Config{
		Addr: "127.0.0.1:0", RunRepo: repo, AuditRepo: audit.NewPostgresRepository(pool),
		PlanReviewers: singleReviewerSet{&scriptedReviewer{steps: []reviewStep{blockStep(started)}}},
	})
	outcome := make(chan reviewDispatchOutcome, 1)
	go func() {
		outcome <- s.dispatchPlanReviews(ctx, created.ID, st.ID, validPlanBytes(t), nil, nil, nil, nil, nil)
	}()
	waitFor(t, started, "setup: reviewer started")

	advisoryLocks := func() int {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_locks l JOIN pg_database d ON d.oid = l.database WHERE l.locktype = 'advisory' AND d.datname = current_database()`).Scan(&n); err != nil {
			t.Fatalf("count advisory locks: %v", err)
		}
		return n
	}
	assertNoLocks := func(when string) {
		t.Helper()
		if nowaitRefused(t, pool, "runs", created.ID) {
			t.Errorf("%s: the run row is locked", when)
		}
		if n := advisoryLocks(); n != 0 {
			t.Errorf("%s: %d advisory locks held", when, n)
		}
	}
	assertNoLocks("while the reviewer blocks")
	s.reviewers.closeAndCancel()
	select {
	case o := <-outcome:
		if !o.interrupted {
			t.Fatalf("outcome = %+v, want interrupted", o)
		}
	case <-time.After(boundedWait):
		t.Fatal("dispatch did not return after the kill")
	}
	assertNoLocks("after the kill")

	// M0.13 positive control: the probe does report a held row lock.
	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("control: begin: %v", err)
	}
	defer func() { _ = holder.Rollback(ctx) }()
	var one int
	if err := holder.QueryRow(ctx, `SELECT 1 FROM runs WHERE id=$1 FOR UPDATE`, created.ID).Scan(&one); err != nil {
		t.Fatalf("control: hold: %v", err)
	}
	if !nowaitRefused(t, pool, "runs", created.ID) {
		t.Fatal("control: the probe did not report a held row lock as 55P03")
	}
}

// The bound's cooperative-dependency assumption against the real store.
func TestRunStore_BlockedRowLock_HonoursDeadline_PG(t *testing.T) {
	ctx := context.Background()
	e := newPGEdgeSeam(t)
	holder, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("setup: begin: %v", err)
	}
	t.Cleanup(func() { _ = holder.Rollback(context.Background()) })
	var one int
	if err := holder.QueryRow(ctx, `SELECT 1 FROM stages WHERE id=$1 FOR UPDATE`, e.stageID).Scan(&one); err != nil {
		t.Fatalf("setup: hold stage: %v", err)
	}
	short, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, terr := e.repo.TransitionStage(short, e.stageID, run.StageStateAwaitingApproval, nil)
		result <- terr
	}()
	select {
	case terr := <-result:
		if terr == nil {
			t.Fatal("a write blocked on a row lock succeeded under a passed deadline")
		}
	case <-time.After(boundedWait):
		t.Fatal("the store write ignored its deadline")
	}
	if err := holder.Rollback(ctx); err != nil {
		t.Fatalf("setup: release: %v", err)
	}
	if _, err := e.repo.TransitionStage(ctx, e.stageID, run.StageStateAwaitingApproval, nil); err != nil {
		t.Fatalf("control: the same write after release: %v", err)
	}
}
