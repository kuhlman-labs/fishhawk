package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/cli/internal/httpclient"
)

// reviewGateFake is the backend for the approve-review-gate end-to-end tests.
// Every handler increments a mutex-guarded request counter, and the approvals
// handler its own — that pair is what makes the never-dialed counterfactuals
// (an empty --attest, and the already-succeeded no-op) behavioral rather than
// asserted: the server is a reachable in-test loopback, so a request the CLI
// issues genuinely lands and is counted.
type reviewGateFake struct {
	mu sync.Mutex

	requests         int
	approvalRequests int

	stages       []httpclient.Stage
	stagesStatus int

	approvalStatus  int
	approvalErrCode string
	approvalDetails map[string]any
	approvalResp    httpclient.ApprovalResult
	approvedID      string
	approvalBody    httpclient.SubmitApprovalInput
}

func (f *reviewGateFake) counts() (total, approvals int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests, f.approvalRequests
}

func newReviewGateFake(t *testing.T) (*reviewGateFake, *httptest.Server) {
	t.Helper()
	f := &reviewGateFake{stagesStatus: http.StatusOK, approvalStatus: http.StatusOK}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v0/runs/{run_id}/stages", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.requests++
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(f.stagesStatus)
		if f.stagesStatus >= 400 {
			_ = json.NewEncoder(w).Encode(errEnvelope("internal_error", "boom"))
			return
		}
		_ = json.NewEncoder(w).Encode(httpclient.ListStagesResult{Items: f.stages})
	})
	mux.HandleFunc("POST /v0/stages/{stage_id}/approvals", func(w http.ResponseWriter, r *http.Request) {
		var in httpclient.SubmitApprovalInput
		_ = json.NewDecoder(r.Body).Decode(&in)
		f.mu.Lock()
		f.requests++
		f.approvalRequests++
		f.approvedID = r.PathValue("stage_id")
		f.approvalBody = in
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(f.approvalStatus)
		if f.approvalStatus >= 400 {
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
				"code": f.approvalErrCode, "message": "refused by the backend", "details": f.approvalDetails,
			}})
			return
		}
		_ = json.NewEncoder(w).Encode(f.approvalResp)
	})
	// Catch-all: any OTHER path the CLI might dial still counts, so a
	// never-dialed assertion cannot be satisfied by a request simply
	// missing the two registered routes.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.requests++
		f.mu.Unlock()
		w.WriteHeader(http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return f, srv
}

// reviewGateStages builds a stage list with a review stage in the given state,
// preceded by a plan stage (also at awaiting_approval) so the happy path
// proves the verb picks the REVIEW stage and not merely the first parked one.
func reviewGateStages(runID uuid.UUID, reviewState string) (stages []httpclient.Stage, reviewID uuid.UUID, planID uuid.UUID) {
	planID = uuid.New()
	reviewID = uuid.New()
	return []httpclient.Stage{
		{ID: planID, RunID: runID, Sequence: 1, Type: "plan", State: "awaiting_approval",
			Executor: httpclient.StageExecutor{Kind: "agent", Ref: "claude"}},
		{ID: reviewID, RunID: runID, Sequence: 2, Type: "review", State: reviewState,
			Executor: httpclient.StageExecutor{Kind: "human", Ref: "operator"}},
	}, reviewID, planID
}

func runApproveReviewGateCLI(args ...string) (int, string, string) {
	var stdout, stderr strings.Builder
	code := run(append([]string{"approve-review-gate"}, args...), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// TestApproveReviewGate_HappyPath is the DONE-MEANS behavioral test: the POST
// lands on the REVIEW stage's id (not the plan stage's), carries decision
// approve and the TRIMMED attestation as the comment, and exits 0.
func TestApproveReviewGate_HappyPath(t *testing.T) {
	runID := uuid.New()
	f, srv := newReviewGateFake(t)
	stages, reviewID, planID := reviewGateStages(runID, "awaiting_approval")
	f.stages = stages
	f.approvalResp = httpclient.ApprovalResult{Stage: httpclient.Stage{
		ID: reviewID, RunID: runID, Sequence: 2, Type: "review", State: "succeeded",
	}}

	code, stdout, stderr := runApproveReviewGateCLI(runID.String(),
		"--attest", "  checked the forge: #2801 carries area:runner and the parent link  ",
		"--backend-url", srv.URL)
	if code != exitOK {
		t.Fatalf("exit = %d, want %d\nstderr=%s", code, exitOK, stderr)
	}
	f.mu.Lock()
	gotID, gotBody := f.approvedID, f.approvalBody
	f.mu.Unlock()
	if gotID != reviewID.String() {
		t.Errorf("approved stage id = %s, want the REVIEW stage %s (plan stage was %s)", gotID, reviewID, planID)
	}
	if gotBody.Decision != httpclient.ApprovalApprove {
		t.Errorf("decision = %q, want approve", gotBody.Decision)
	}
	if want := "checked the forge: #2801 carries area:runner and the parent link"; gotBody.Comment != want {
		t.Errorf("comment = %q, want the trimmed attestation %q", gotBody.Comment, want)
	}
	if !strings.Contains(stdout, reviewID.String()) {
		t.Errorf("stdout did not echo the approved stage: %q", stdout)
	}
}

// TestApproveReviewGate_EmptyAttest_NeverDialsBackend is the counterfactual
// vehicle for the LOCAL attestation guard (acceptance criterion
// empty-attest-refused-locally). Delete the strings.TrimSpace(*attest) == ""
// guard in review_gate.go and this goes RED: the CLI resolves stages and POSTs,
// so the counter goes non-zero.
func TestApproveReviewGate_EmptyAttest_NeverDialsBackend(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"empty string", []string{"--attest", ""}},
		{"whitespace only", []string{"--attest", "   \t\n  "}},
		{"flag omitted", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runID := uuid.New()
			f, srv := newReviewGateFake(t)
			stages, _, _ := reviewGateStages(runID, "awaiting_approval")
			f.stages = stages

			args := append([]string{runID.String(), "--backend-url", srv.URL}, tc.args...)
			code, _, stderr := runApproveReviewGateCLI(args...)
			if code != exitUsage {
				t.Errorf("exit = %d, want exitUsage (%d)\nstderr=%s", code, exitUsage, stderr)
			}
			if !strings.Contains(stderr, "--attest is required") {
				t.Errorf("stderr does not name the attestation: %q", stderr)
			}
			if total, _ := f.counts(); total != 0 {
				t.Errorf("backend received %d requests, want 0 — the guard must refuse BEFORE any HTTP hop", total)
			}
		})
	}
}

// TestApproveReviewGate_AlreadyApproved_NoOp covers the duplicate no-op branch
// the operator's binding condition fixes: a review stage already at succeeded
// renders a LABELED no-op naming the prior approval, exits 0, and never dials
// the approvals endpoint.
func TestApproveReviewGate_AlreadyApproved_NoOp(t *testing.T) {
	runID := uuid.New()
	f, srv := newReviewGateFake(t)
	stages, reviewID, _ := reviewGateStages(runID, "succeeded")
	f.stages = stages

	code, stdout, stderr := runApproveReviewGateCLI(runID.String(),
		"--attest", "checked the forge", "--backend-url", srv.URL)
	if code != exitOK {
		t.Fatalf("exit = %d, want %d\nstderr=%s", code, exitOK, stderr)
	}
	if !strings.Contains(stderr, "no-op") || !strings.Contains(stderr, "already approved") {
		t.Errorf("stderr is not a labeled no-op: %q", stderr)
	}
	if !strings.Contains(stderr, reviewID.String()) {
		t.Errorf("no-op notice does not name the prior-approved stage %s: %q", reviewID, stderr)
	}
	if _, approvals := f.counts(); approvals != 0 {
		t.Errorf("approvals endpoint dialed %d times, want 0 — a repeated invocation must submit nothing", approvals)
	}
	if !strings.Contains(stdout, reviewID.String()) {
		t.Errorf("stdout did not echo the settled stage: %q", stdout)
	}

	t.Run("json encodes the duplicate label", func(t *testing.T) {
		code, stdout, _ := runApproveReviewGateCLI(runID.String(),
			"--attest", "checked the forge", "--backend-url", srv.URL, "--output", "json")
		if code != exitOK {
			t.Fatalf("exit = %d, want %d", code, exitOK)
		}
		var got map[string]any
		if err := json.Unmarshal([]byte(stdout), &got); err != nil {
			t.Fatalf("decode: %v\nstdout=%s", err, stdout)
		}
		if got["duplicate_submission"] != true {
			t.Errorf("duplicate_submission = %v, want true: %s", got["duplicate_submission"], stdout)
		}
	})
}

// TestApproveReviewGate_NoAwaitingReviewStage covers the third resolver branch:
// no review stage at awaiting_approval AND none succeeded — the actionable
// error naming the status verb, with nothing submitted.
func TestApproveReviewGate_NoAwaitingReviewStage(t *testing.T) {
	cases := []struct {
		name   string
		stages func(uuid.UUID) []httpclient.Stage
	}{
		{"review stage still running", func(runID uuid.UUID) []httpclient.Stage {
			s, _, _ := reviewGateStages(runID, "running")
			return s
		}},
		{"run has only a plan stage", func(runID uuid.UUID) []httpclient.Stage {
			return []httpclient.Stage{{ID: uuid.New(), RunID: runID, Sequence: 1, Type: "plan", State: "awaiting_approval"}}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runID := uuid.New()
			f, srv := newReviewGateFake(t)
			f.stages = tc.stages(runID)

			code, _, stderr := runApproveReviewGateCLI(runID.String(),
				"--attest", "checked the forge", "--backend-url", srv.URL)
			if code != exitFailure {
				t.Errorf("exit = %d, want exitFailure (%d)\nstderr=%s", code, exitFailure, stderr)
			}
			if !strings.Contains(stderr, "fishhawk_get_run_status") {
				t.Errorf("stderr does not name the status verb: %q", stderr)
			}
			if _, approvals := f.counts(); approvals != 0 {
				t.Errorf("approvals endpoint dialed %d times, want 0", approvals)
			}
		})
	}
}

// distinguishing substrings, one per named refusal. Each refusal test asserts
// its OWN is present and the OTHER TWO are absent, so a single collapsed
// message fails.
var reviewGateRefusalMarkers = map[string]string{
	"attestation_required":     "empty or whitespace-only comment",
	"self_decision":            "run-bound agent token",
	"operator_agent_forbidden": "delegated operator-agent token",
}

func assertOnlyRefusalExplained(t *testing.T, stderr, code string) {
	t.Helper()
	for c, marker := range reviewGateRefusalMarkers {
		if c == code {
			if !strings.Contains(stderr, marker) {
				t.Errorf("stderr missing the %s explanation (%q): %q", c, marker, stderr)
			}
			continue
		}
		if strings.Contains(stderr, marker) {
			t.Errorf("stderr carries the %s explanation (%q) while refusing %s — the explanations must be distinct: %q", c, marker, code, stderr)
		}
	}
	// The verbatim backend envelope must still print: the explanation
	// SUPPLEMENTS the server's own message, never replaces it.
	if !strings.Contains(stderr, "refused by the backend") {
		t.Errorf("stderr dropped the verbatim backend envelope: %q", stderr)
	}
	if !strings.Contains(stderr, code) {
		t.Errorf("stderr does not carry the envelope code %s: %q", code, stderr)
	}
}

func runRefusal(t *testing.T, status int, code string, details map[string]any) string {
	t.Helper()
	runID := uuid.New()
	f, srv := newReviewGateFake(t)
	stages, _, _ := reviewGateStages(runID, "awaiting_approval")
	f.stages = stages
	f.approvalStatus = status
	f.approvalErrCode = code
	f.approvalDetails = details

	exit, _, stderr := runApproveReviewGateCLI(runID.String(),
		"--attest", "checked the forge", "--backend-url", srv.URL)
	if exit != exitFailure {
		t.Fatalf("exit = %d, want exitFailure (%d)\nstderr=%s", exit, exitFailure, stderr)
	}
	return stderr
}

func TestApproveReviewGate_AttestationRequired_400(t *testing.T) {
	stderr := runRefusal(t, http.StatusBadRequest, "attestation_required", nil)
	assertOnlyRefusalExplained(t, stderr, "attestation_required")
	if !strings.Contains(stderr, "attestation IS what this gate records") {
		t.Errorf("explanation does not name what the gate records: %q", stderr)
	}
}

func TestApproveReviewGate_SelfDecision_403(t *testing.T) {
	stderr := runRefusal(t, http.StatusForbidden, "self_decision", nil)
	assertOnlyRefusalExplained(t, stderr, "self_decision")
	if !strings.Contains(stderr, "executor: human with not: [agent]") {
		t.Errorf("explanation does not name the declared executor constraint: %q", stderr)
	}
	if !strings.Contains(stderr, "BY DESIGN") || !strings.Contains(stderr, "NOT a misconfiguration") {
		t.Errorf("explanation does not frame the refusal as human-only BY DESIGN: %q", stderr)
	}
}

func TestApproveReviewGate_OperatorAgentForbidden_403(t *testing.T) {
	stderr := runRefusal(t, http.StatusForbidden, "operator_agent_forbidden", nil)
	assertOnlyRefusalExplained(t, stderr, "operator_agent_forbidden")
	if !strings.Contains(stderr, "executor: human with not: [agent]") {
		t.Errorf("explanation does not name the declared executor constraint: %q", stderr)
	}
	if !strings.Contains(stderr, "BY DESIGN") {
		t.Errorf("explanation does not frame the refusal as BY DESIGN: %q", stderr)
	}
	if !strings.Contains(stderr, "no MCP verb exists") {
		t.Errorf("explanation does not say why no MCP verb exists: %q", stderr)
	}
}

// TestApproveReviewGate_UnnamedRefusalCode covers the fall-through: a code the
// explanation switch does not name adds NOTHING beyond the verbatim envelope.
func TestApproveReviewGate_UnnamedRefusalCode(t *testing.T) {
	stderr := runRefusal(t, http.StatusForbidden, "insufficient_scope", map[string]any{"required_scope": "write:approvals"})
	if !strings.Contains(stderr, "insufficient_scope") || !strings.Contains(stderr, "refused by the backend") {
		t.Errorf("verbatim envelope missing: %q", stderr)
	}
	for c, marker := range reviewGateRefusalMarkers {
		if strings.Contains(stderr, marker) {
			t.Errorf("unnamed code drew the %s explanation (%q): %q", c, marker, stderr)
		}
	}
}

// TestApproveReviewGate_AdmissionOnlyOn409Code pins that the admission ladder
// is keyed to the review_stage_managed_by_github code, not merely to the
// PRESENCE of an admission_reason key: a 403 whose details happen to carry one
// must NOT draw the admission rendering.
func TestApproveReviewGate_AdmissionOnlyOn409Code(t *testing.T) {
	stderr := runRefusal(t, http.StatusForbidden, "self_decision",
		map[string]any{"admission_reason": "pull_request_managed"})
	if strings.Contains(stderr, "admission_reason=") || strings.Contains(stderr, "merging the pull request") {
		t.Errorf("a non-409 refusal drew the admission rendering: %q", stderr)
	}
	assertOnlyRefusalExplained(t, stderr, "self_decision")
}

func TestApproveReviewGate_AdmissionReason_409(t *testing.T) {
	cases := []struct {
		name    string
		details map[string]any
		want    []string
		absent  []string
	}{
		{
			name:    "pull_request_managed",
			details: map[string]any{"admission_reason": "pull_request_managed"},
			want:    []string{"approve it by merging the pull request", "admission_reason=pull_request_managed"},
			absent:  []string{"fail-closed resolution failure"},
		},
		{
			name:    "multiple_review_spec_stages",
			details: map[string]any{"admission_reason": "multiple_review_spec_stages"},
			want: []string{"admission_reason=multiple_review_spec_stages", "fail-closed resolution failure",
				"CACHED workflow spec"},
			absent: []string{"merging the pull request"},
		},
		{
			name:    "no admission_reason at all",
			details: map[string]any{},
			want:    []string{"review_stage_managed_by_github", "refused by the backend"},
			absent:  []string{"admission_reason=", "fail-closed resolution failure", "merging the pull request"},
		},
		{
			name:    "non-string admission_reason",
			details: map[string]any{"admission_reason": 42},
			want:    []string{"review_stage_managed_by_github"},
			absent:  []string{"admission_reason=", "fail-closed resolution failure"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stderr := runRefusal(t, http.StatusConflict, "review_stage_managed_by_github", tc.details)
			for _, w := range tc.want {
				if !strings.Contains(stderr, w) {
					t.Errorf("stderr missing %q: %q", w, stderr)
				}
			}
			for _, a := range tc.absent {
				if strings.Contains(stderr, a) {
					t.Errorf("stderr fabricated %q: %q", a, stderr)
				}
			}
		})
	}
}

// TestApproveReviewGate_Duplicate_TextNoticeOnStderr covers the BACKEND
// duplicate-labeling render — the same-stage-same-subject window the
// client-side settled branch cannot see (an approval landing between the stage
// list and the POST). The notice goes to STDERR, never stdout, and exit stays 0.
func TestApproveReviewGate_Duplicate_TextNoticeOnStderr(t *testing.T) {
	runID := uuid.New()
	f, srv := newReviewGateFake(t)
	stages, reviewID, _ := reviewGateStages(runID, "awaiting_approval")
	f.stages = stages
	f.approvalResp = httpclient.ApprovalResult{
		Stage:               httpclient.Stage{ID: reviewID, RunID: runID, Type: "review", State: "succeeded"},
		DuplicateSubmission: true,
		PriorDecision:       "approve",
		PriorSubmittedAt:    "2026-09-01T00:00:00Z",
	}

	code, stdout, stderr := runApproveReviewGateCLI(runID.String(),
		"--attest", "checked the forge", "--backend-url", srv.URL)
	if code != exitOK {
		t.Fatalf("exit = %d, want %d\nstderr=%s", code, exitOK, stderr)
	}
	if !strings.Contains(stderr, "duplicate submission") || !strings.Contains(stderr, "2026-09-01T00:00:00Z") {
		t.Errorf("stderr missing the #986 duplicate notice: %q", stderr)
	}
	if strings.Contains(stdout, "duplicate submission") {
		t.Errorf("duplicate notice leaked to stdout: %q", stdout)
	}
}

func TestApproveReviewGate_JSONOutput(t *testing.T) {
	t.Run("duplicate encodes the labeled fields", func(t *testing.T) {
		runID := uuid.New()
		f, srv := newReviewGateFake(t)
		stages, reviewID, _ := reviewGateStages(runID, "awaiting_approval")
		f.stages = stages
		f.approvalResp = httpclient.ApprovalResult{
			Stage:               httpclient.Stage{ID: reviewID, RunID: runID, Type: "review", State: "succeeded"},
			DuplicateSubmission: true,
			PriorDecision:       "approve",
			PriorSubmittedAt:    "2026-09-01T00:00:00Z",
		}
		code, stdout, stderr := runApproveReviewGateCLI(runID.String(),
			"--attest", "checked the forge", "--backend-url", srv.URL, "-o", "json")
		if code != exitOK {
			t.Fatalf("exit = %d\nstderr=%s", code, stderr)
		}
		var got map[string]any
		if err := json.Unmarshal([]byte(stdout), &got); err != nil {
			t.Fatalf("decode: %v\n%s", err, stdout)
		}
		if got["duplicate_submission"] != true || got["prior_decision"] != "approve" {
			t.Errorf("duplicate fields not encoded: %s", stdout)
		}
	})
	t.Run("first submission omits them", func(t *testing.T) {
		runID := uuid.New()
		f, srv := newReviewGateFake(t)
		stages, reviewID, _ := reviewGateStages(runID, "awaiting_approval")
		f.stages = stages
		f.approvalResp = httpclient.ApprovalResult{
			Stage: httpclient.Stage{ID: reviewID, RunID: runID, Type: "review", State: "succeeded"},
		}
		code, stdout, stderr := runApproveReviewGateCLI(runID.String(),
			"--attest", "checked the forge", "--backend-url", srv.URL, "--output", "json")
		if code != exitOK {
			t.Fatalf("exit = %d\nstderr=%s", code, stderr)
		}
		var got map[string]any
		if err := json.Unmarshal([]byte(stdout), &got); err != nil {
			t.Fatalf("decode: %v\n%s", err, stdout)
		}
		for _, k := range []string{"duplicate_submission", "prior_decision", "prior_submitted_at"} {
			if _, ok := got[k]; ok {
				t.Errorf("first submission encoded %q: %s", k, stdout)
			}
		}
	})
}

func TestApproveReviewGate_UsageBranches(t *testing.T) {
	_, srv := newReviewGateFake(t)
	id := uuid.New().String()
	cases := []struct {
		name string
		args []string
	}{
		{"bad uuid", []string{"not-a-uuid", "--attest", "x", "--backend-url", srv.URL}},
		{"missing positional", []string{"--attest", "x", "--backend-url", srv.URL}},
		{"extra positional", []string{id, id, "--attest", "x", "--backend-url", srv.URL}},
		{"invalid output", []string{id, "--attest", "x", "--output", "garbage", "--backend-url", srv.URL}},
		{"unknown flag", []string{id, "--nope", "--backend-url", srv.URL}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, _, stderr := runApproveReviewGateCLI(tc.args...)
			if code != exitUsage {
				t.Errorf("exit = %d, want exitUsage (%d)\nstderr=%s", code, exitUsage, stderr)
			}
		})
	}
}

func TestApproveReviewGate_StagesListFailure(t *testing.T) {
	runID := uuid.New()
	f, srv := newReviewGateFake(t)
	f.stagesStatus = http.StatusInternalServerError

	code, _, stderr := runApproveReviewGateCLI(runID.String(),
		"--attest", "checked the forge", "--backend-url", srv.URL)
	if code != exitFailure {
		t.Errorf("exit = %d, want exitFailure (%d)", code, exitFailure)
	}
	if !strings.Contains(stderr, "list stages") {
		t.Errorf("stderr does not name the failed call: %q", stderr)
	}
	if _, approvals := f.counts(); approvals != 0 {
		t.Errorf("approvals endpoint dialed %d times after a stage-list failure, want 0", approvals)
	}
}

// TestRun_Help_ListsApproveReviewGate pins the cmdinfo-rendered top-level usage
// listing. A cmdinfo entry without the dispatch wiring (or vice versa) also
// reddens the existing TestInventoryCoversDispatch.
func TestRun_Help_ListsApproveReviewGate(t *testing.T) {
	var stdout, stderr strings.Builder
	if got := run([]string{"help"}, &stdout, &stderr); got != exitOK {
		t.Fatalf("help exit = %d", got)
	}
	if !strings.Contains(stdout.String(), "approve-review-gate") {
		t.Errorf("top-level usage does not list approve-review-gate:\n%s", stdout.String())
	}
}
