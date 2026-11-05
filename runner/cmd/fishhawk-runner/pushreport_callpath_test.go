package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/runner/internal/agent"
	"github.com/kuhlman-labs/fishhawk/runner/internal/scenario"
	"github.com/kuhlman-labs/fishhawk/runner/internal/upload"
)

// These tests compose the real upload.Client with real call sites against an
// httptest backend (ADR-141 D4). They pin today's order, in which the trace
// ships BEFORE the acceptance-scenario and fix-up push reports and the conflict
// pass ships no trace. None of them claims that a ShipTrace follows a push
// report in production.

const (
	routeTraceRaw      = "trace:raw"
	routeTraceRedacted = "trace:redacted"
	routeAcceptance    = "acceptance"
	routeAcceptPushed  = "report:" + upload.OutcomeAcceptanceScenariosPushed
	routeDropped       = "report:" + upload.OutcomeAcceptanceScenarioRetirementDropped
	routeConflict      = "report:conflict_resolution_pushed"
	routeFailed        = "report:failed"
)

// pushReportBackend records every outcome-bearing POST in arrival order. The
// push report named by refuse answers refuseStatus; every other request succeeds.
type pushReportBackend struct {
	mu           sync.Mutex
	log          []string
	dropReasons  []string
	refuse       string
	refuseStatus int
	prompt       *upload.FetchedPrompt
}

func (b *pushReportBackend) record(route string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.log = append(b.log, route)
}

func (b *pushReportBackend) requests() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.log...)
}

func (b *pushReportBackend) drops() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.dropReasons...)
}

func (b *pushReportBackend) count(route string) int {
	n := 0
	for _, r := range b.requests() {
		if r == route {
			n++
		}
	}
	return n
}

func (b *pushReportBackend) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch {
	case strings.HasSuffix(r.URL.Path, "/signing-key"):
		_, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			http.Error(w, "keygen", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"run_id":      r.PathValue("run_id"),
			"public_key":  base64.StdEncoding.EncodeToString(priv.Public().(ed25519.PublicKey)),
			"private_key": base64.StdEncoding.EncodeToString(priv),
			"issued_at":   time.Now().UTC(),
			"expires_at":  time.Now().UTC().Add(30 * time.Minute),
		})
	case strings.HasSuffix(r.URL.Path, "/prompt"):
		_ = json.NewEncoder(w).Encode(b.prompt)
	case strings.HasSuffix(r.URL.Path, "/installation-token"):
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(upload.FetchInstallationTokenResult{Token: "synthetic-token"})
	case strings.HasSuffix(r.URL.Path, "/trace"):
		variant := r.URL.Query().Get("variant")
		b.record("trace:" + variant)
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(upload.ShipResult{StageID: r.URL.Query().Get("stage_id"), Variant: variant, ContentHash: "h"})
	case strings.HasSuffix(r.URL.Path, "/acceptance"):
		b.record(routeAcceptance)
		_ = json.NewEncoder(w).Encode(upload.ShipAcceptanceResult{ID: "a1", StageID: r.URL.Query().Get("stage_id"), Verdict: "passed", ContentHash: "h"})
	case strings.HasSuffix(r.URL.Path, "/pull-request"):
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			Outcome string `json:"outcome"`
			Reason  string `json:"reason"`
		}
		_ = json.Unmarshal(raw, &body)
		route := "report:" + body.Outcome
		b.record(route)
		if route == routeDropped {
			b.mu.Lock()
			b.dropReasons = append(b.dropReasons, body.Reason)
			b.mu.Unlock()
		}
		if route == b.refuse {
			w.WriteHeader(b.refuseStatus)
			return
		}
		_ = json.NewEncoder(w).Encode(upload.ShipPullRequestResult{StageID: r.URL.Query().Get("stage_id")})
	case strings.HasSuffix(r.URL.Path, "/reap-failure"):
		b.record("reap-failure")
		_, _ = w.Write([]byte(`{}`))
	default:
		http.NotFound(w, r)
	}
}

// startPushReportBackend serves b on the routes the runner calls and returns a
// real client on it. The blip budget stays four attempts, so a report that fell
// off the push-report budget is visible as four attempts instead of three.
func startPushReportBackend(t *testing.T, b *pushReportBackend) *upload.Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle("/v0/runs/{run_id}/", b)
	mux.Handle("/v0/stages/", b)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := upload.New(srv.URL)
	c.MaxRetries = 3
	c.Backoff = time.Millisecond
	c.PushReportMaxRetries = 2
	return c
}

// withRealUploadClient makes run() use c.
func withRealUploadClient(t *testing.T, c *upload.Client) {
	t.Helper()
	orig := newUploadClient
	newUploadClient = func(string) uploadClient { return c }
	t.Cleanup(func() { newUploadClient = orig })
}

// pushReportFailureCases are the two ways a push report can fail: exhausted
// spends the whole retry budget, rejected stops on the first answer.
var pushReportFailureCases = []struct {
	name     string
	status   int
	attempts int
}{
	{name: "exhausted", status: http.StatusServiceUnavailable, attempts: 3},
	{name: "rejected", status: http.StatusConflict, attempts: 1},
}

// TestPersistAndReport_PushReportFailedThenUploadTrace composes the real call
// sites on one client as an isolation probe. It is NOT run()'s order, and it
// claims nothing about ShipTrace following a report in production; see
// TestRun_AcceptanceStage_PushReportFailed_ActualOrder for the real order.
//
// The budgets are the client probe's: arming the 1s terminal phase from the
// report would leave uploadTrace born expired. The pause runs a rejected report's
// clock past that phase's 500ms settling window, because that report takes no
// time of its own.
func TestPersistAndReport_PushReportFailedThenUploadTrace(t *testing.T) {
	for _, tc := range pushReportFailureCases {
		t.Run(tc.name, func(t *testing.T) {
			repo, _, origin, head := persistRepo(t, nil)
			const runID, stageID = "run-callpath", "stage-callpath"

			origTreeDir, origRemote := acceptanceTreeDir, acceptancePersistRemoteURL
			acceptanceTreeDir = t.TempDir()
			acceptancePersistRemoteURL = func(config) string { return origin }
			t.Cleanup(func() { acceptanceTreeDir, acceptancePersistRemoteURL = origTreeDir, origRemote })
			tgit(t, repo, "worktree", "add", "--detach", acceptanceTreePath(runID, stageID), head)

			b := &pushReportBackend{refuse: routeAcceptPushed, refuseStatus: tc.status}
			c := startPushReportBackend(t, b)
			c.TerminalEgressBudget = time.Second
			c.PushReportBackoff = 600 * time.Millisecond
			c.PushReportBackoffCap = 600 * time.Millisecond

			_, priv, err := ed25519.GenerateKey(rand.Reader)
			if err != nil {
				t.Fatalf("setup: keygen: %v", err)
			}
			issued := &upload.IssuedKey{PrivateKey: priv}
			cfg := config{runID: runID, stageID: stageID}
			retired := []scenario.RetiredEntry{{ID: "scenario:issue-7/old", Reason: "behaviour replaced", RunID: "r0", PR: 700, RetiredAt: "2026-09-01T00:00:00Z"}}
			drop := newRetirementDropReporter(retired)

			persistAndReportAcceptanceScenarios(context.Background(), cfg, c, issued, acceptanceReplayInputs{
				runBranch: "fishhawk/run-callpath", prNumber: 742, issue: 101,
				criteria: []upload.AcceptanceCriterionEntry{{ID: "crit-b", Statement: "s", Drivable: true}},
				retired:  retired,
			}, head, passedVerdictFor("crit-b"), drop, io.Discard)

			if got := b.count(routeAcceptPushed); got != tc.attempts {
				t.Errorf("assert: report attempts = %d, want %d", got, tc.attempts)
			}
			if !strings.HasPrefix(drop.reason, "push_report_failed") {
				t.Errorf("assert: drop reason = %q, want push_report_failed", drop.reason)
			}
			if got := tgit(t, origin, "rev-parse", "refs/heads/fishhawk/run-callpath"); got == head {
				t.Errorf("assert: origin run branch = %s, want the scenario commit left on it", got)
			}

			time.Sleep(600 * time.Millisecond)
			if err := uploadTrace(context.Background(), cfg, "raw", []byte("bundle"), io.Discard, c, issued); err != nil {
				t.Errorf("assert: uploadTrace after a failed push report: %v", err)
			}
			if got := b.count(routeTraceRaw); got != 1 {
				t.Errorf("assert: trace requests = %d, want 1", got)
			}
		})
	}
}

// TestRun_AcceptanceStage_PushReportFailed_ActualOrder drives run() itself and
// pins the real request order: trace raw, trace redacted, the verdict, the report
// attempts, then the retirement-drop report. Nothing ships a trace after the
// report. Falling off the push-report budget makes four attempts where the
// exhausted case wants three; moving the persistence above the trace loop
// reorders the log.
func TestRun_AcceptanceStage_PushReportFailed_ActualOrder(t *testing.T) {
	for _, tc := range pushReportFailureCases {
		t.Run(tc.name, func(t *testing.T) {
			_, origin, fu, args := acceptanceReplayStageSetup(t)
			before := tgit(t, origin, "rev-parse", "refs/heads/fishhawk/run-x")
			b := &pushReportBackend{refuse: routeAcceptPushed, refuseStatus: tc.status, prompt: fu.promptResp}
			c := startPushReportBackend(t, b)
			c.PushReportBackoff = time.Millisecond
			c.PushReportBackoffCap = time.Millisecond
			withRealUploadClient(t, c)
			withFakeInvoker(t, &fakeInvoker{canned: agent.Result{OK: true, StructuredOutput: []byte(okVerdict)}})

			var stderr strings.Builder
			if got := run(args, &stderr); got != exitOK {
				t.Errorf("assert: run = %d, want exitOK:\n%s", got, stderr.String())
			}

			want := []string{routeTraceRaw, routeTraceRedacted, routeAcceptance}
			for i := 0; i < tc.attempts; i++ {
				want = append(want, routeAcceptPushed)
			}
			want = append(want, routeDropped)
			if got := b.requests(); !reflect.DeepEqual(got, want) {
				t.Errorf("assert: request order = %v, want %v\n%s", got, want, stderr.String())
			}
			if got := b.drops(); len(got) != 1 || !strings.HasPrefix(got[0], "push_report_failed") {
				t.Errorf("assert: drop reasons = %q, want one naming push_report_failed", got)
			}
			if got := tgit(t, origin, "rev-parse", "refs/heads/fishhawk/run-x"); got == before {
				t.Errorf("assert: origin run branch = %s, want the scenario commit left on it", got)
			}
		})
	}
}

// TestRun_Fixup_PushReportExhausted_SendsFailedC pins existing behaviour: the
// trace ships first, then a fix-up report whose retries ran out is followed by a
// failed category-C report, and the run exits failed. The first report's error
// has the shape ShipPullRequest returns on exhaustion.
func TestRun_Fixup_PushReportExhausted_SendsFailedC(t *testing.T) {
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	withFakeInvoker(t, &fakeInvoker{canned: agent.Result{OK: true}})
	fu := newFakeUploader(t)
	fu.promptResp = fixupLandingPrompt()
	fu.prErrSeq = []error{
		fmt.Errorf("upload: ship pull-request exhausted retries: %w", errors.New("upload: ship pull-request: 503: reported_head_unverifiable")),
		nil,
	}
	var reportsAtTrace []int
	fu.beforeShip = func(kind string) {
		if kind == "trace" {
			reportsAtTrace = append(reportsAtTrace, len(fu.gotPRCalls))
		}
	}
	withFakeUploader(t, fu)
	withFakeGitOps(t, &fakePusher{}, &fakePROpener{})
	withFakeFixupRemoteBranchTip(t, "head-sha-abc", nil)

	var stderr strings.Builder
	if got := runFixupLanding(t, &stderr); got != exitFailure {
		t.Errorf("assert: run = %d, want exitFailure:\n%s", got, stderr.String())
	}
	if want := []int{0, 0}; !reflect.DeepEqual(reportsAtTrace, want) {
		t.Errorf("assert: reports sent before each trace = %v, want %v", reportsAtTrace, want)
	}
	if len(fu.gotPRCalls) != 2 {
		t.Fatalf("assert: reports = %+v, want the push report then a failed one", fu.gotPRCalls)
	}
	if got := fu.gotPRCalls[0]; got.Outcome != "fixup_pushed" {
		t.Errorf("assert: first report outcome = %q, want fixup_pushed", got.Outcome)
	}
	if got := fu.gotPRCalls[1]; got.Outcome != "failed" || got.Category != "C" {
		t.Errorf("assert: second report = {%q, %q}, want {failed, C}", got.Outcome, got.Category)
	}
}

// TestRunConflictResolution_PushReportExhausted_NoTraceNoFailedReport pins the
// conflict pass's actual continuation, a known gap tracked by #5052: on exhausted
// retries it sends no trace and no failed report and exits failed, leaving the
// stage running until the watchdog or an operator recovers it.
func TestRunConflictResolution_PushReportExhausted_NoTraceNoFailedReport(t *testing.T) {
	dispatch, _, featureTip, _ := crDispatchRepo(t)
	crStageHarness(t, &fakePusher{}, crResolveInWorkdir)
	implementEnv(t, "kuhlman-labs/fishhawk", "main")

	const runID, stageID = "11111111-2222-3333-4444-555555555555", "22222222-3333-4444-5555-666666666666"
	b := &pushReportBackend{
		refuse: routeConflict, refuseStatus: http.StatusServiceUnavailable,
		prompt: &upload.FetchedPrompt{
			StageID: stageID, StageType: "implement", Prompt: "resolve", PromptHash: "h",
			ConflictResolution: true, ConflictResolutionBranch: "feature",
			ConflictResolutionBaseRef: "main", ConflictResolutionExpectedHeadSHA: featureTip,
		},
	}
	c := startPushReportBackend(t, b)
	c.PushReportBackoff = time.Millisecond
	c.PushReportBackoffCap = time.Millisecond
	withRealUploadClient(t, c)

	var stderr strings.Builder
	got := run([]string{
		"--run-id", runID, "--backend-url", "https://api.fishhawk.test",
		"--workflow", "feature_change", "--stage", "implement", "--stage-id", stageID,
		"--fetch-prompt", "--upload-trace", "--working-dir", dispatch,
	}, &stderr)
	if got != exitFailure {
		t.Errorf("assert: run = %d, want exitFailure:\n%s", got, stderr.String())
	}
	if want := []string{routeConflict, routeConflict, routeConflict}; !reflect.DeepEqual(b.requests(), want) {
		t.Errorf("assert: requests = %v, want three report attempts and nothing else\n%s", b.requests(), stderr.String())
	}
	if !strings.Contains(stderr.String(), `"event":"conflict_resolution_report_failed"`) {
		t.Errorf("assert: missing conflict_resolution_report_failed:\n%s", stderr.String())
	}
}
