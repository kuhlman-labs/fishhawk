package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/operatorrole"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/timescale"
)

// review_rerun_test.go pins POST /v0/runs/{run_id}/reviews/rerun (#4219): the
// end-to-end clearing of the review-coverage hold, one test per refusal (each
// reading COMMITTED state: no review_round_rerun_requested row and no new
// round), auth, the 502 rebuild failure, two concurrent POSTs, the rerun_of
// wire shape and the marker-reachability guard.

const rerunHead = "headsha1"

func rerunOperator() Identity {
	return Identity{Subject: "github:op", TokenID: uuid.NewString(), Scopes: []string{"write:runs"}}
}

func postRerun(t *testing.T, s *Server, runID string, id Identity) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v0/runs/"+runID+"/reviews/rerun", nil)
	req.SetPathValue("run_id", runID)
	req = req.WithContext(context.WithValue(req.Context(), ctxKeyIdentity, id))
	rec := httptest.NewRecorder()
	s.handleRerunReview(rec, req)
	return rec
}

// rerunErr decodes the error envelope's code and details.
func rerunErr(t *testing.T, rec *httptest.ResponseRecorder) (string, map[string]any) {
	t.Helper()
	var body struct {
		Error struct {
			Code    string         `json:"code"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body %s: %v", rec.Body.String(), err)
	}
	return body.Error.Code, body.Error.Details
}

// seedFailedConsolidatedRound seeds a settled consolidated round whose only
// reviewer failed — the d3b1d915 parent round — and returns its sequence.
func seedFailedConsolidatedRound(t *testing.T, f *redispatchFixture) int64 {
	t.Helper()
	f.seedPlanArtifact(t, validPlanBytes(t), "approved-plan", true)
	seq := f.appendAt(t, f.implStage.ID, beforeBoot, "implement_review_started", advisoryImplementStarted(reviewRoundOriginConsolidated, "consolidated-base", rerunHead))
	f.appendAt(t, f.implStage.ID, beforeBoot, "implement_review_failed", planreview.ReviewFailedPayload{Reason: "provider quota exhausted", Authority: planreview.AuthorityAdvisory})
	return seq
}

// assertNothingRerun reads committed state: no review_round_rerun_requested
// row and no implement_review_started after seq.
func assertNothingRerun(t *testing.T, f *redispatchFixture, seq int64) {
	t.Helper()
	if n := len(redispatchEntries(t, f.au, f.runRow.ID, categoryReviewRoundRerunRequested)); n != 0 {
		t.Errorf("%s rows = %d, want 0 (a refused re-run writes nothing)", categoryReviewRoundRerunRequested, n)
	}
	if n := countAfter(t, f.au, f.runRow.ID, "implement_review_started", seq); n != 0 {
		t.Errorf("new implement rounds = %d, want 0", n)
	}
}

// TestRerunReview_ClearsCoverageHold is the end-to-end: a decomposed parent
// whose consolidated round failed (and whose slices were never reviewed) is
// HELD by reviewCoverageRefusal; the re-run rebuilds ComparePatch(base, head),
// re-dispatches under the re-run marker (bypassing the same-head dedup), the
// reviewer returns a verdict, and the hold clears.
//
// COUNTERFACTUALS: drop the dedup-guard bypass term → the rebuilt round dedups
// → 502 → RED; drop the rerun_of stamp → rerunRoundStarted finds no round →
// 502 → RED.
func TestRerunReview_ClearsCoverageHold(t *testing.T) {
	gh, paths := recordingCompareClient(t, cannedCompareOneFile)
	f := newRedispatchFixture(t, redispatchFixtureOpts{github: gh})
	for i := range 2 {
		c := f.rr.seedRun()
		parent, idx := f.runRow.ID, i
		c.DecomposedFrom, c.SliceIndex, c.State = &parent, &idx, run.StateSucceeded
	}
	seq := seedFailedConsolidatedRound(t, f)

	ref, err := f.s.reviewCoverageRefusal(context.Background(), f.runRow)
	if err != nil || ref == nil || ref.Code != mergeCodeReviewCoverageIncomplete {
		t.Fatalf("precondition: coverage refusal = %+v err %v, want %s", ref, err, mergeCodeReviewCoverageIncomplete)
	}

	rec := postRerun(t, f.s, f.runRow.ID.String(), rerunOperator())
	f.s.waitBackgroundReviews()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", rec.Code, rec.Body.String())
	}
	var resp rerunReviewResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	started, seqs := startedAfter(t, f.au, f.runRow.ID, "implement_review_started", seq)
	if len(started) != 1 || resp.NewRoundSequence != seqs[0] || resp.RerunOfSequence != seq {
		t.Fatalf("new rounds = %d (%v), response %+v, want exactly one at the reported sequence re-running %d", len(started), seqs, resp, seq)
	}
	if got := started[0]; got.RerunOf != seq || got.HeadSHA != rerunHead || got.RoundOrigin != reviewRoundOriginConsolidated || got.RoundBaseSHA != "consolidated-base" || got.RedispatchOf != 0 {
		t.Errorf("re-run round = %+v, want rerun_of %d, same source, no D6 lineage", got, seq)
	}
	rows := redispatchEntries(t, f.au, f.runRow.ID, categoryReviewRoundRerunRequested)
	if len(rows) != 1 || rows[0].Sequence >= seqs[0] {
		t.Fatalf("%s rows = %d, want 1 appended BEFORE the new round", categoryReviewRoundRerunRequested, len(rows))
	}
	var row reviewRoundRerunRequestedPayload
	if err := json.Unmarshal(rows[0].Payload, &row); err != nil || row.RerunOfSequence != seq || row.Stage != "implement" || row.HeadSHA != rerunHead || row.LandedBefore != 1 {
		t.Errorf("row = %+v (err %v), want rerun_of_sequence %d, stage implement, head %s, landed 1", row, err, seq, rerunHead)
	}
	f.au.mu.Lock()
	for _, ap := range f.au.appended {
		if ap.Category == categoryReviewRoundRerunRequested && (ap.ActorSubject == nil || *ap.ActorSubject != "github:op") {
			t.Errorf("row actor subject = %v, want github:op", ap.ActorSubject)
		}
	}
	f.au.mu.Unlock()
	if p := paths(); len(p) != 1 || !strings.HasSuffix(p[0], "/compare/consolidated-base..."+rerunHead) {
		t.Errorf("compare paths = %v, want one ComparePatch(consolidated-base, %s)", p, rerunHead)
	}
	if n := countAfter(t, f.au, f.runRow.ID, "implement_reviewed", seqs[0]); n != 1 {
		t.Fatalf("verdicts on the re-run round = %d, want 1", n)
	}
	if ref, err := f.s.reviewCoverageRefusal(context.Background(), f.runRow); err != nil || ref != nil {
		t.Errorf("coverage refusal after the re-run = %+v err %v, want nil (the hold clears)", ref, err)
	}
}

// TestRerunReview_Refusals: one row per refusal. Every fixture is otherwise
// ELIGIBLE (a failed consolidated round, wired reviewer and compare), so the
// row's own check is the only thing in the path; each asserts the code, the
// reason and committed state.
func TestRerunReview_Refusals(t *testing.T) {
	gh, _ := recordingCompareClient(t, cannedCompareOneFile)
	cases := []struct {
		name       string
		opts       redispatchFixtureOpts
		seed       func(t *testing.T, f *redispatchFixture) int64
		wantCode   string
		wantReason string
	}{
		{
			name: "no round", opts: redispatchFixtureOpts{github: gh},
			seed:     func(*testing.T, *redispatchFixture) int64 { return 0 },
			wantCode: rerunCodeNoReviewRound,
		},
		{
			name: "round unsettled", opts: redispatchFixtureOpts{github: gh},
			seed: func(t *testing.T, f *redispatchFixture) int64 {
				return f.appendAt(t, f.implStage.ID, beforeBoot, "implement_review_started", advisoryImplementStarted(reviewRoundOriginConsolidated, "b", rerunHead))
			},
			wantCode: rerunCodeRoundInFlight, wantReason: rerunInFlightUnsettled,
		},
		{
			name: "boot re-dispatch pending", opts: redispatchFixtureOpts{github: gh},
			seed: func(t *testing.T, f *redispatchFixture) int64 {
				seq := seedFailedConsolidatedRound(t, f)
				k := pendingRedispatchKey{runID: f.runRow.ID, stage: "implement", seq: seq}
				markRedispatchPending(k)
				t.Cleanup(func() { clearRedispatchPending(k) })
				return seq
			},
			wantCode: rerunCodeRoundInFlight, wantReason: rerunInFlightRedispatching,
		},
		{
			name: "re-run already pending", opts: redispatchFixtureOpts{github: gh},
			seed: func(t *testing.T, f *redispatchFixture) int64 {
				seq := seedFailedConsolidatedRound(t, f)
				if !claimRerun(f.runRow.ID) {
					t.Fatal("claimRerun = false on a fresh run")
				}
				t.Cleanup(func() { releaseRerun(f.runRow.ID) })
				return seq
			},
			wantCode: rerunCodeRoundInFlight, wantReason: rerunInFlightRerunPending,
		},
		{
			name: "round has a verdict", opts: redispatchFixtureOpts{github: gh},
			seed: func(t *testing.T, f *redispatchFixture) int64 {
				seq := f.appendAt(t, f.implStage.ID, beforeBoot, "implement_review_started", advisoryImplementStarted(reviewRoundOriginConsolidated, "b", rerunHead))
				f.appendAt(t, f.implStage.ID, beforeBoot, "implement_reviewed", planreview.ImplementReviewedPayload{Verdict: planreview.VerdictReject})
				return seq
			},
			wantCode: rerunCodeRoundHasVerdict,
		},
		{
			name: "gating round", opts: redispatchFixtureOpts{github: gh},
			seed: func(t *testing.T, f *redispatchFixture) int64 {
				p := advisoryImplementStarted(reviewRoundOriginConsolidated, "b", rerunHead)
				p.Authority = planreview.AuthorityGating
				seq := f.appendAt(t, f.implStage.ID, beforeBoot, "implement_review_started", p)
				f.appendAt(t, f.implStage.ID, beforeBoot, "implement_review_failed", planreview.ReviewFailedPayload{Reason: "x"})
				return seq
			},
			wantCode: rerunCodeIneligible, wantReason: redispatchSlugGatingAuthority,
		},
		{
			name: "reviewer unwired", opts: redispatchFixtureOpts{github: gh, reviewerUnwired: true},
			seed:     seedFailedConsolidatedRound,
			wantCode: rerunCodeIneligible, wantReason: redispatchSlugReviewerUnwired,
		},
		{
			name: "unknown round source", opts: redispatchFixtureOpts{github: gh},
			seed: func(t *testing.T, f *redispatchFixture) int64 {
				seq := f.appendAt(t, f.implStage.ID, beforeBoot, "implement_review_started", advisoryImplementStarted("", "", rerunHead))
				f.appendAt(t, f.implStage.ID, beforeBoot, "implement_review_failed", planreview.ReviewFailedPayload{Reason: "x"})
				return seq
			},
			wantCode: rerunCodeIneligible, wantReason: redispatchSlugUnknownRoundSource,
		},
		{
			name: "superseded by a fix-up", opts: redispatchFixtureOpts{github: gh},
			seed: func(t *testing.T, f *redispatchFixture) int64 {
				seq := seedFailedConsolidatedRound(t, f)
				f.appendAt(t, f.implStage.ID, beforeBoot, CategoryStageFixupTriggered, map[string]any{})
				return seq
			},
			wantCode: rerunCodeIneligible, wantReason: redispatchSlugSupersededByFixup,
		},
		{
			name: "implement stage terminal", opts: redispatchFixtureOpts{github: gh, implStageState: run.StageStateFailed},
			seed:     seedFailedConsolidatedRound,
			wantCode: rerunCodeIneligible, wantReason: redispatchSlugImplementStageTerminal,
		},
		{
			name: "advanced past review", opts: redispatchFixtureOpts{github: gh, reviewStageState: run.StageStateSucceeded},
			seed:     seedFailedConsolidatedRound,
			wantCode: rerunCodeIneligible, wantReason: redispatchSlugAdvancedPastReview,
		},
		{
			name: "forge compare unavailable", opts: redispatchFixtureOpts{},
			seed:     seedFailedConsolidatedRound,
			wantCode: rerunCodeIneligible, wantReason: redispatchSlugForgeCompareUnavail,
		},
		{
			name: "trace bundle unavailable", opts: redispatchFixtureOpts{github: gh},
			seed: func(t *testing.T, f *redispatchFixture) int64 {
				seq := seedTraceImplementRound(t, f)
				f.appendAt(t, f.implStage.ID, beforeBoot, "implement_review_failed", planreview.ReviewFailedPayload{Reason: "x"})
				return seq
			},
			wantCode: rerunCodeIneligible, wantReason: redispatchSlugTraceBundleUnavail,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newRedispatchFixture(t, tc.opts)
			seq := tc.seed(t, f)
			rec := postRerun(t, f.s, f.runRow.ID.String(), rerunOperator())
			f.s.waitBackgroundReviews()
			if rec.Code != http.StatusConflict {
				t.Fatalf("status = %d, want 409:\n%s", rec.Code, rec.Body.String())
			}
			code, details := rerunErr(t, rec)
			if code != tc.wantCode {
				t.Errorf("code = %q, want %q", code, tc.wantCode)
			}
			if tc.wantReason != "" && details["reason"] != tc.wantReason {
				t.Errorf("reason = %v, want %q", details["reason"], tc.wantReason)
			}
			assertNothingRerun(t, f, seq)
		})
	}
}

// TestRerunReview_Auth: anonymous, a run-bound agent token, a delegated
// operator-agent token and a token without write:runs are refused before any
// read or write.
func TestRerunReview_Auth(t *testing.T) {
	gh, _ := recordingCompareClient(t, cannedCompareOneFile)
	cases := []struct {
		name     string
		id       Identity
		status   int
		wantCode string
	}{
		{"anonymous", Identity{Subject: "anonymous"}, http.StatusUnauthorized, "authentication_required"},
		{"run-bound agent token", Identity{Subject: "mcp:run:" + uuid.NewString(), TokenID: uuid.NewString(), Scopes: []string{"write:runs"}}, http.StatusForbidden, "run_token_forbidden"},
		{"operator-agent token", Identity{Subject: operatorrole.TokenSubjectPrefix + uuid.NewString(), TokenID: uuid.NewString(), Scopes: []string{"write:runs"}}, http.StatusForbidden, "operator_agent_forbidden"},
		{"missing scope", Identity{Subject: "github:op", TokenID: uuid.NewString(), Scopes: []string{"read:runs"}}, http.StatusForbidden, "insufficient_scope"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newRedispatchFixture(t, redispatchFixtureOpts{github: gh})
			seq := seedFailedConsolidatedRound(t, f)
			rec := postRerun(t, f.s, f.runRow.ID.String(), tc.id)
			f.s.waitBackgroundReviews()
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d:\n%s", rec.Code, tc.status, rec.Body.String())
			}
			if code, _ := rerunErr(t, rec); code != tc.wantCode {
				t.Errorf("code = %q, want %q", code, tc.wantCode)
			}
			assertNothingRerun(t, f, seq)
		})
	}
}

// TestRerunReview_RequestErrors: a malformed run id, an unknown run, unwired
// repositories and a failed state read are refused without writing.
func TestRerunReview_RequestErrors(t *testing.T) {
	gh, _ := recordingCompareClient(t, cannedCompareOneFile)
	f := newRedispatchFixture(t, redispatchFixtureOpts{github: gh})
	seq := seedFailedConsolidatedRound(t, f)

	if rec := postRerun(t, f.s, "not-a-uuid", rerunOperator()); rec.Code != http.StatusBadRequest {
		t.Errorf("bad run id status = %d, want 400", rec.Code)
	}
	if rec := postRerun(t, f.s, uuid.NewString(), rerunOperator()); rec.Code != http.StatusNotFound {
		t.Errorf("unknown run status = %d, want 404", rec.Code)
	}
	bare := New(Config{Addr: "127.0.0.1:0"})
	if rec := postRerun(t, bare, f.runRow.ID.String(), rerunOperator()); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("unconfigured status = %d, want 503", rec.Code)
	}
	for _, cat := range []string{"implement_review_started", "implement_review_failed", "implement_reviewed", CategoryStageFixupTriggered} {
		t.Run("read error "+cat, func(t *testing.T) {
			f.au.mu.Lock()
			f.au.listByCategoryErrCategory = cat
			f.au.mu.Unlock()
			rec := postRerun(t, f.s, f.runRow.ID.String(), rerunOperator())
			f.au.mu.Lock()
			f.au.listByCategoryErrCategory = ""
			f.au.mu.Unlock()
			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500:\n%s", rec.Code, rec.Body.String())
			}
			if code, _ := rerunErr(t, rec); code != rerunCodeCheckFailed {
				t.Errorf("code = %q, want %q", code, rerunCodeCheckFailed)
			}
		})
	}
	assertNothingRerun(t, f, seq)
}

// TestRerunReview_AuditAppendFailsDispatchesNothing: the request row is the
// record; without it nothing is dispatched.
//
// COUNTERFACTUAL: ignore the append error → the compare runs and a new round
// starts → RED.
func TestRerunReview_AuditAppendFailsDispatchesNothing(t *testing.T) {
	gh, paths := recordingCompareClient(t, cannedCompareOneFile)
	f := newRedispatchFixture(t, redispatchFixtureOpts{github: gh})
	seq := seedFailedConsolidatedRound(t, f)
	f.au.mu.Lock()
	f.au.appendErr = io.ErrClosedPipe
	f.au.mu.Unlock()
	rec := postRerun(t, f.s, f.runRow.ID.String(), rerunOperator())
	f.s.waitBackgroundReviews()
	f.au.mu.Lock()
	f.au.appendErr = nil
	f.au.mu.Unlock()
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500:\n%s", rec.Code, rec.Body.String())
	}
	if code, _ := rerunErr(t, rec); code != rerunCodeAuditFailed {
		t.Errorf("code = %q, want %q", code, rerunCodeAuditFailed)
	}
	if p := paths(); len(p) != 0 {
		t.Errorf("compare calls = %v, want none", p)
	}
	assertNothingRerun(t, f, seq)
}

// TestRerunReview_RebuildFailureIs502: the round is eligible and recorded, but
// the compare fails, so no round starts: 502 naming compare_failed, the
// request row stands, no new round.
//
// COUNTERFACTUAL: answer 200 without reading back a started round → RED.
func TestRerunReview_RebuildFailureIs502(t *testing.T) {
	gh, _ := recordingCompareClient(t, "not json")
	f := newRedispatchFixture(t, redispatchFixtureOpts{github: gh})
	seq := seedFailedConsolidatedRound(t, f)
	rec := postRerun(t, f.s, f.runRow.ID.String(), rerunOperator())
	f.s.waitBackgroundReviews()
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502:\n%s", rec.Code, rec.Body.String())
	}
	code, details := rerunErr(t, rec)
	if code != rerunCodeNotStarted || details["reason"] != redispatchSlugCompareFailed {
		t.Errorf("error = %s %v, want %s reason %s", code, details, rerunCodeNotStarted, redispatchSlugCompareFailed)
	}
	if n := len(redispatchEntries(t, f.au, f.runRow.ID, categoryReviewRoundRerunRequested)); n != 1 {
		t.Errorf("%s rows = %d, want 1 (recorded before the rebuild)", categoryReviewRoundRerunRequested, n)
	}
	if n := countAfter(t, f.au, f.runRow.ID, "implement_review_started", seq); n != 0 {
		t.Errorf("new rounds = %d, want 0", n)
	}
	if rerunPendingForTest(f.runRow.ID) {
		t.Error("the run is still claimed after the handler returned")
	}
}

func rerunPendingForTest(runID uuid.UUID) bool {
	pendingRerunMu.Lock()
	defer pendingRerunMu.Unlock()
	_, ok := pendingReruns[runID]
	return ok
}

// blockingCompareClient serves the canned compare body only after release is
// closed, signalling entered on each compare request.
func blockingCompareClient(t *testing.T) (*githubclient.Client, chan struct{}, chan struct{}) {
	t.Helper()
	release, entered := make(chan struct{}), make(chan struct{}, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !strings.Contains(r.URL.Path, "/compare/") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		entered <- struct{}{}
		<-release
		_, _ = io.WriteString(w, cannedCompareOneFile)
	}))
	t.Cleanup(srv.Close)
	return &githubclient.Client{
		BaseURL: srv.URL, Tokens: &fakeTokenProvider{tok: "ghs_t"},
		HTTP: &http.Client{Timeout: timescale.D(10 * time.Second)}, AppJWT: func() (string, error) { return "ghs_jwt", nil },
	}, release, entered
}

// TestRerunReview_ConcurrentPOSTsStartOneRound: the first POST blocks in its
// compare; a second POST for the same run is refused review_round_in_flight
// (rerun_pending) without a request row; exactly one new round starts.
//
// COUNTERFACTUAL: drop the claimRerun refusal → the second POST also records
// a request row and reaches the compare → RED.
func TestRerunReview_ConcurrentPOSTsStartOneRound(t *testing.T) {
	gh, release, entered := blockingCompareClient(t)
	f := newRedispatchFixture(t, redispatchFixtureOpts{github: gh})
	seq := seedFailedConsolidatedRound(t, f)
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })

	first := make(chan *httptest.ResponseRecorder, 1)
	go func() { first <- postRerun(t, f.s, f.runRow.ID.String(), rerunOperator()) }()
	select {
	case <-entered:
	case <-time.After(timescale.D(5 * time.Second)):
		t.Fatal("the first re-run never reached its compare")
	}
	second := postRerun(t, f.s, f.runRow.ID.String(), rerunOperator())
	if second.Code != http.StatusConflict {
		t.Fatalf("second status = %d, want 409:\n%s", second.Code, second.Body.String())
	}
	if code, details := rerunErr(t, second); code != rerunCodeRoundInFlight || details["reason"] != rerunInFlightRerunPending {
		t.Errorf("second error = %s %v, want %s reason %s", code, details, rerunCodeRoundInFlight, rerunInFlightRerunPending)
	}
	once.Do(func() { close(release) })
	var rec *httptest.ResponseRecorder
	select {
	case rec = <-first:
	case <-time.After(timescale.D(10 * time.Second)):
		t.Fatal("the first re-run did not finish")
	}
	f.s.waitBackgroundReviews()
	if rec.Code != http.StatusOK {
		t.Fatalf("first status = %d, want 200:\n%s", rec.Code, rec.Body.String())
	}
	if n := countAfter(t, f.au, f.runRow.ID, "implement_review_started", seq); n != 1 {
		t.Errorf("new rounds = %d, want exactly 1", n)
	}
	if n := len(redispatchEntries(t, f.au, f.runRow.ID, categoryReviewRoundRerunRequested)); n != 1 {
		t.Errorf("%s rows = %d, want 1", categoryReviewRoundRerunRequested, n)
	}
	if len(entered) != 0 {
		t.Errorf("further compare calls = %d, want 0", len(entered))
	}
}

// TestReviewStartedPayload_RerunOfWireShape pins rerun_of's omit-when-unset
// and legacy-decode behaviour (approval condition 1: pinned here rather than
// in planreview/review_test.go).
func TestReviewStartedPayload_RerunOfWireShape(t *testing.T) {
	b, err := json.Marshal(planreview.ReviewStartedPayload{ConfiguredAgents: 1, Authority: planreview.AuthorityAdvisory})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "rerun_of") {
		t.Errorf("unset rerun_of serialized: %s", b)
	}
	var legacy planreview.ReviewStartedPayload
	if err := json.Unmarshal([]byte(`{"configured_agents":1,"authority":"advisory","head_sha":"h","redispatch_of":3,"redispatch_depth":1}`), &legacy); err != nil {
		t.Fatal(err)
	}
	if legacy.RerunOf != 0 || legacy.RedispatchOf != 3 {
		t.Errorf("legacy decode = %+v, want rerun_of 0, redispatch_of 3", legacy)
	}
	var set planreview.ReviewStartedPayload
	if err := json.Unmarshal([]byte(`{"configured_agents":1,"rerun_of":17}`), &set); err != nil || set.RerunOf != 17 {
		t.Errorf("rerun_of decode = %+v err %v, want 17", set, err)
	}
}

// TestReviewRerunMarker_Reachability is the re-run marker's reachability guard:
// its key is referenced only by its setter and reader, and the setter only by
// the operator verb's dispatch. It also asserts the ADR-091 D6 table is still
// satisfied, so the new path did not route through the D6 marker.
//
// COUNTERFACTUAL: plant a withReviewRerun call in any other declaration → RED.
func TestReviewRerunMarker_Reachability(t *testing.T) {
	files := parseServerProductionFiles(t)
	for name, want := range map[string][]string{
		"reviewRerunCtxKey": {"review_round_context.go:reviewRerunFrom", "review_round_context.go:withReviewRerun"},
		"withReviewRerun":   {"review_rerun.go:handleRerunReview"},
	} {
		if got := refSites(files, name); !slices.Equal(got, want) {
			t.Errorf("%s referenced at %v, want exactly %v (the re-run marker is set only by the operator verb, #4219)", name, got, want)
		}
	}
	planted := plantFile(t, files, "planted_handler.go", plantedHandler(`_ = withReviewRerun(r.Context(), reviewRerun{Of: 1})`))
	if got := refSites(planted, "withReviewRerun"); len(got) != 2 {
		t.Errorf("planted setter call not observed: %v", got)
	}
	if v := d6Violations(files); len(v) != 0 {
		t.Errorf("ADR-091 D6 guard violations after #4219:\n%s", strings.Join(v, "\n"))
	}
}
