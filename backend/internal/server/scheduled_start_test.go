package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/account"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// scheduledSpec is a minimal v2 spec whose `upkeep` workflow carries the
// caller's applies_to trigger list. A plan + implement stage, because
// handleCreateRun refuses a stageless workflow.
func scheduledSpec(triggers ...string) string {
	appliesTo := ""
	if len(triggers) > 0 {
		appliesTo = "    applies_to:\n      trigger:\n"
		for _, tr := range triggers {
			appliesTo += "        - " + tr + "\n"
		}
	}
	return "version: \"2\"\nworkflows:\n  upkeep:\n" + appliesTo + `    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
      - id: implement
        type: implement
        executor:
          agent: claude-code
        produces:
          - artifact: pull_request
`
}

const scheduledTestKey = "scheduled:upkeep:2026-10-02T09:00:00Z"

func scheduledParams(specYAML string) ScheduledRunParams {
	return ScheduledRunParams{
		Repo:           "kuhlman-labs/fishhawk",
		WorkflowID:     "upkeep",
		WorkflowSHA:    "blobsha",
		WorkflowSpec:   []byte(specYAML),
		IdempotencyKey: scheduledTestKey,
	}
}

func onlyRun(t *testing.T, repo *fakeRepo) *run.Run {
	t.Helper()
	repo.mu.Lock()
	defer repo.mu.Unlock()
	if len(repo.runs) != 1 {
		t.Fatalf("repo holds %d runs, want exactly 1", len(repo.runs))
	}
	for _, r := range repo.runs {
		return r
	}
	return nil
}

// TestStartScheduledRun_StartsThenReplaysAlreadyStarted is the producer's
// headline: the in-process call is ADMITTED PAST the ValidTriggerSources
// membership check (scheduled is not a member) and mints a run stamped
// trigger_source=scheduled carrying the window's Idempotency-Key; the SAME key
// again is the handler's (repo, key) replay → already_started with the SAME
// run id and no second row.
//
// Counterfactual (membership exemption): drop `&& !scheduledAdmitted` from
// handleCreateRun's membership check and the first call is refused 400
// validation_failed — RED on the Kind assertion.
func TestStartScheduledRun_StartsThenReplaysAlreadyStarted(t *testing.T) {
	repo := newFakeRepo()
	s := newServer(t, repo)

	first, err := s.StartScheduledRun(context.Background(), scheduledParams(scheduledSpec("scheduled")))
	if err != nil {
		t.Fatalf("StartScheduledRun: %v", err)
	}
	if first.Kind != ScheduledStartStarted || first.Status != http.StatusCreated || first.RunID == uuid.Nil {
		t.Fatalf("first outcome = %+v, want started/201 with a run id", first)
	}
	r := onlyRun(t, repo)
	if r.ID != first.RunID {
		t.Errorf("outcome run id %s != persisted run %s", first.RunID, r.ID)
	}
	if r.TriggerSource != run.TriggerScheduled {
		t.Errorf("TriggerSource = %q, want scheduled", r.TriggerSource)
	}
	if r.IdempotencyKey == nil || *r.IdempotencyKey != scheduledTestKey {
		t.Errorf("IdempotencyKey = %v, want %q", r.IdempotencyKey, scheduledTestKey)
	}
	if r.TriggerRef != nil {
		t.Errorf("TriggerRef = %q, want nil for an un-anchored schedule", *r.TriggerRef)
	}

	second, err := s.StartScheduledRun(context.Background(), scheduledParams(scheduledSpec("scheduled")))
	if err != nil {
		t.Fatalf("StartScheduledRun (replay): %v", err)
	}
	if second.Kind != ScheduledStartAlreadyStarted || second.Status != http.StatusOK {
		t.Fatalf("replay outcome = %+v, want already_started/200", second)
	}
	if second.RunID != first.RunID {
		t.Errorf("replay run id %s, want the first run %s", second.RunID, first.RunID)
	}
	_ = onlyRun(t, repo) // still exactly one row
}

// TestStartScheduledRun_AppliesToRefusesNonScheduledDeclaration proves the
// applies_to gate runs on the in-process path: a workflow declaring only
// [on_demand] refuses the scheduled run with the handler's own 422
// workflow_not_applicable naming the trigger criterion, and mints nothing.
// The same body with [scheduled] is admitted in the test above, so the
// refusal is the predicate's verdict, not a fixture defect.
func TestStartScheduledRun_AppliesToRefusesNonScheduledDeclaration(t *testing.T) {
	repo := newFakeRepo()
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: repo, AuditRepo: newAuditFake()})

	out, err := s.StartScheduledRun(context.Background(), scheduledParams(scheduledSpec("on_demand")))
	if err != nil {
		t.Fatalf("StartScheduledRun: %v", err)
	}
	if out.Kind != ScheduledStartRefused || out.Status != http.StatusUnprocessableEntity || out.Code != "workflow_not_applicable" {
		t.Fatalf("outcome = %+v, want refused/422 workflow_not_applicable", out)
	}
	if !strings.Contains(out.Message, "trigger") {
		t.Errorf("refusal message %q must name the failed trigger criterion", out.Message)
	}
	if n := runRowCount(repo); n != 0 {
		t.Errorf("refused window minted %d runs, want 0", n)
	}
}

// TestStartScheduledRun_BudgetExhaustedRefused proves the blocking periodic
// budget gate runs on the in-process path: an exhausted budget refuses the
// window with 402 budget_exhausted (code + message carried verbatim), writes
// the existing run_rejected_budget audit, and mints no run.
func TestStartScheduledRun_BudgetExhaustedRefused(t *testing.T) {
	au := newAuditFake()
	rr := newBudgetRunRepo()
	rr.spent = 100 // over the 50 limit
	s := New(Config{Addr: "127.0.0.1:0", AuditRepo: au, RunRepo: rr})

	p := scheduledParams(blockingBudgetSpec(50))
	p.WorkflowID = "feature_change"
	out, err := s.StartScheduledRun(context.Background(), p)
	if err != nil {
		t.Fatalf("StartScheduledRun: %v", err)
	}
	if out.Kind != ScheduledStartRefused || out.Status != http.StatusPaymentRequired || out.Code != "budget_exhausted" {
		t.Fatalf("outcome = %+v, want refused/402 budget_exhausted", out)
	}
	if out.Message == "" {
		t.Error("refusal carried an empty message; the envelope message must be carried verbatim")
	}
	if n := countGlobalAudits(au, "run_rejected_budget"); n != 1 {
		t.Errorf("run_rejected_budget audits = %d, want 1", n)
	}
	if n := runRowCount(rr.fakeRepo); n != 0 {
		t.Errorf("refused window minted %d runs, want 0", n)
	}
}

// TestStartScheduledRun_IssueAnchorSetsTriggerRef: a schedule naming an
// anchor issue yields trigger_ref issue:N. With no GitHub client wired the
// issue_context hydration degrades to nil and never blocks the start.
func TestStartScheduledRun_IssueAnchorSetsTriggerRef(t *testing.T) {
	repo := newFakeRepo()
	s := newServer(t, repo)

	p := scheduledParams(scheduledSpec("scheduled"))
	p.IssueNumber = 3112
	out, err := s.StartScheduledRun(context.Background(), p)
	if err != nil || out.Kind != ScheduledStartStarted {
		t.Fatalf("outcome = %+v, err = %v; want started", out, err)
	}
	r := onlyRun(t, repo)
	if r.TriggerRef == nil || *r.TriggerRef != "issue:3112" {
		t.Errorf("TriggerRef = %v, want issue:3112", r.TriggerRef)
	}
	if r.IssueContext != nil {
		t.Errorf("IssueContext = %+v, want nil with no GitHub client wired", r.IssueContext)
	}
}

// fakeScheduledGitHub serves the three endpoints the anchored scheduled start
// touches; installStatus lets a case fail the installation lookup.
func fakeScheduledGitHub(t *testing.T, installStatus int) *githubclient.Client {
	t.Helper()
	var mu sync.Mutex
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/repos/kuhlman-labs/fishhawk/installation":
			if installStatus != http.StatusOK {
				w.WriteHeader(installStatus)
				_, _ = w.Write([]byte(`{"message":"boom"}`))
				return
			}
			_, _ = w.Write([]byte(`{"id":7}`))
		case "/repos/kuhlman-labs/fishhawk/issues/3112":
			_, _ = w.Write([]byte(`{"number":3112,"title":"Groom the backlog","body":"anchor body"}`))
		case "/repos/kuhlman-labs/fishhawk/issues/3112/comments":
			_, _ = w.Write([]byte(`[{"id":1,"user":{"login":"captain"},"body":"note","created_at":"2026-10-01T00:00:00Z"}]`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"Not Found"}`))
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &githubclient.Client{
		BaseURL: srv.URL,
		Tokens:  &fakeTokenProvider{tok: "ghs_t"},
		HTTP:    &http.Client{Timeout: 5 * time.Second},
		AppJWT:  func() (string, error) { return "ghs_jwt", nil },
	}
}

// TestStartScheduledRun_IssueAnchorHydratesIssueContext: with a GitHub client
// wired, the anchor issue (title, body, comments) is hydrated onto the run row
// through the shared hydrateCampaignIssueContext.
func TestStartScheduledRun_IssueAnchorHydratesIssueContext(t *testing.T) {
	repo := newFakeRepo()
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: repo, GitHub: fakeScheduledGitHub(t, http.StatusOK)})

	p := scheduledParams(scheduledSpec("scheduled"))
	p.IssueNumber = 3112
	out, err := s.StartScheduledRun(context.Background(), p)
	if err != nil || out.Kind != ScheduledStartStarted {
		t.Fatalf("outcome = %+v, err = %v; want started", out, err)
	}
	r := onlyRun(t, repo)
	if r.IssueContext == nil {
		t.Fatal("IssueContext = nil, want the hydrated anchor issue")
	}
	if r.IssueContext.Number != 3112 || r.IssueContext.Title != "Groom the backlog" || r.IssueContext.Body != "anchor body" {
		t.Errorf("IssueContext = %+v, want number 3112 / title / body from the fake issue", r.IssueContext)
	}
	if len(r.IssueContext.Comments) != 1 || r.IssueContext.Comments[0].Author != "captain" {
		t.Errorf("IssueContext.Comments = %+v, want the one fake comment by captain", r.IssueContext.Comments)
	}
}

// TestStartScheduledRun_IssueHydrationFailureNeverBlocks: an installation
// lookup failure degrades hydration to nil and the window still starts with
// its issue:N trigger_ref — hydration is best-effort by contract.
func TestStartScheduledRun_IssueHydrationFailureNeverBlocks(t *testing.T) {
	repo := newFakeRepo()
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: repo, GitHub: fakeScheduledGitHub(t, http.StatusInternalServerError)})

	p := scheduledParams(scheduledSpec("scheduled"))
	p.IssueNumber = 3112
	out, err := s.StartScheduledRun(context.Background(), p)
	if err != nil || out.Kind != ScheduledStartStarted {
		t.Fatalf("outcome = %+v, err = %v; want started despite the hydration failure", out, err)
	}
	r := onlyRun(t, repo)
	if r.IssueContext != nil {
		t.Errorf("IssueContext = %+v, want nil after a failed installation lookup", r.IssueContext)
	}
	if r.TriggerRef == nil || *r.TriggerRef != "issue:3112" {
		t.Errorf("TriggerRef = %v, want issue:3112", r.TriggerRef)
	}
}

// TestStartScheduledRun_5xxIsTransientError: a server-side failure (the run
// insert errors → 500) is returned as an ERROR, not a refusal, so the
// scheduler leaves the window un-attempted and retries next tick.
func TestStartScheduledRun_5xxIsTransientError(t *testing.T) {
	repo := newFakeRepo()
	repo.createErr = errors.New("disk full")
	s := newServer(t, repo)

	out, err := s.StartScheduledRun(context.Background(), scheduledParams(scheduledSpec("scheduled")))
	if err == nil {
		t.Fatalf("StartScheduledRun returned nil error on a 500 (outcome %+v), want a transient error", out)
	}
	if out.Kind == ScheduledStartRefused || out.Kind == ScheduledStartStarted {
		t.Errorf("outcome kind = %q on a 500, want none (transient)", out.Kind)
	}
	if out.Status != http.StatusInternalServerError {
		t.Errorf("outcome status = %d, want 500", out.Status)
	}
}

// TestClassifyScheduledStart_MalformedAnswersAreErrors pins the fail-closed
// arms of the outcome mapping: a 2xx with no decodable run, a 4xx with no
// error envelope, and an unrecognised status are all ERRORS (retry next tick),
// never a started/refused outcome the scheduler would record as final.
func TestClassifyScheduledStart_MalformedAnswersAreErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"201 empty body", http.StatusCreated, ``},
		{"200 run with nil id", http.StatusOK, `{"id":"00000000-0000-0000-0000-000000000000"}`},
		{"400 no envelope", http.StatusBadRequest, `not json`},
		{"409 envelope without code", http.StatusConflict, `{"error":{"message":"m"}}`},
		{"302 unrecognised", http.StatusFound, ``},
		{"503", http.StatusServiceUnavailable, `{"error":{"code":"run_repo_unconfigured","message":"m"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := classifyScheduledStart(tc.status, []byte(tc.body), "upkeep")
			if err == nil {
				t.Errorf("classifyScheduledStart(%d, %q) = %+v, nil; want an error", tc.status, tc.body, out)
			}
			if out.Kind != "" {
				t.Errorf("Kind = %q, want empty on an error", out.Kind)
			}
		})
	}
}

// replayBody is a 200 replay body: the run handleCreateRun answers when the
// Idempotency-Key matches an existing (repo, key) run.
func replayBody(id uuid.UUID, triggerSource, workflowID string) []byte {
	raw, err := json.Marshal(map[string]any{
		"id":             id,
		"trigger_source": triggerSource,
		"workflow_id":    workflowID,
	})
	if err != nil {
		panic(err)
	}
	return raw
}

// TestClassifyScheduledStart_ReplayOccupancy pins the 200-replay occupancy
// check, one row per failure mode: only a SCHEDULED run of the SAME workflow is
// already_started; any other occupant of the window's key is refused 409
// scheduled_key_occupied carrying the occupant's id. A 201 is unaffected (this
// request minted the run).
//
// Counterfactuals: (i) make the check permissive (condition always false) and
// every refused row reads already_started — RED; (ii) drop ONLY the
// `rr.WorkflowID != workflowID` disjunct and the "scheduled run of another
// workflow" row goes RED while the on_demand rows stay green — that row's
// trigger_source is scheduled, so only the workflow_id comparison can refuse it.
func TestClassifyScheduledStart_ReplayOccupancy(t *testing.T) {
	id := uuid.New()
	for _, tc := range []struct {
		name         string
		status       int
		triggerSrc   string
		workflowID   string
		wantKind     ScheduledStartKind
		wantStatus   int
		wantCode     string
		wantContains []string
	}{
		{"on_demand occupant of the same workflow", http.StatusOK, "on_demand", "upkeep",
			ScheduledStartRefused, http.StatusConflict, ScheduledKeyOccupiedCode,
			[]string{id.String(), "on_demand", "upkeep"}},
		{"scheduled occupant of another workflow", http.StatusOK, "scheduled", "groom",
			ScheduledStartRefused, http.StatusConflict, ScheduledKeyOccupiedCode,
			[]string{id.String(), "groom", "upkeep"}},
		{"empty trigger_source fails closed", http.StatusOK, "", "upkeep",
			ScheduledStartRefused, http.StatusConflict, ScheduledKeyOccupiedCode,
			[]string{id.String()}},
		{"empty workflow_id fails closed", http.StatusOK, "scheduled", "",
			ScheduledStartRefused, http.StatusConflict, ScheduledKeyOccupiedCode,
			[]string{id.String()}},
		{"genuine scheduled same-workflow replay", http.StatusOK, "scheduled", "upkeep",
			ScheduledStartAlreadyStarted, http.StatusOK, "", nil},
		{"201 minted by this request is not occupancy-checked", http.StatusCreated, "on_demand", "upkeep",
			ScheduledStartStarted, http.StatusCreated, "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := classifyScheduledStart(tc.status, replayBody(id, tc.triggerSrc, tc.workflowID), "upkeep")
			if err != nil {
				t.Fatalf("classifyScheduledStart: %v", err)
			}
			if out.Kind != tc.wantKind || out.Status != tc.wantStatus || out.Code != tc.wantCode {
				t.Fatalf("outcome = %+v, want kind %s status %d code %q", out, tc.wantKind, tc.wantStatus, tc.wantCode)
			}
			if out.RunID != id {
				t.Errorf("RunID = %s, want the replayed/occupying run %s", out.RunID, id)
			}
			for _, want := range tc.wantContains {
				if !strings.Contains(out.Message, want) {
					t.Errorf("message %q must contain %q", out.Message, want)
				}
			}
			if tc.wantKind != ScheduledStartRefused && out.Message != "" {
				t.Errorf("non-refused outcome carries message %q", out.Message)
			}
		})
	}
}

// seedOccupant creates, by construction, a run holding scheduledTestKey — the
// squat the occupancy check exists to refuse.
func seedOccupant(t *testing.T, repo *fakeRepo, source run.TriggerSource, workflowID string) *run.Run {
	t.Helper()
	key := scheduledTestKey
	r, err := repo.CreateRun(context.Background(), run.CreateRunParams{
		Repo:           "kuhlman-labs/fishhawk",
		WorkflowID:     workflowID,
		WorkflowSHA:    "blobsha",
		TriggerSource:  source,
		IdempotencyKey: &key,
	})
	if err != nil {
		t.Fatalf("seed occupant: %v", err)
	}
	return r
}

// TestStartScheduledRun_OccupiedKeyRefused drives the REAL handleCreateRun
// replay path: a run an ordinary create minted (on_demand, same workflow)
// under the window's key is refused 409 scheduled_key_occupied naming the
// occupant — never already_started — and nothing is minted.
//
// Counterfactual: make the classifier's occupancy check permissive; the replay
// answers already_started and this goes RED. The fixture isolates the
// trigger_source half: the occupant is of the SAME workflow, so no workflow_id
// disagreement can refuse it.
func TestStartScheduledRun_OccupiedKeyRefused(t *testing.T) {
	repo := newFakeRepo()
	s := newServer(t, repo)
	occupant := seedOccupant(t, repo, run.TriggerOnDemand, "upkeep")

	out, err := s.StartScheduledRun(context.Background(), scheduledParams(scheduledSpec("scheduled")))
	if err != nil {
		t.Fatalf("StartScheduledRun: %v", err)
	}
	if out.Kind != ScheduledStartRefused || out.Status != http.StatusConflict || out.Code != ScheduledKeyOccupiedCode {
		t.Fatalf("outcome = %+v, want refused/409 %s", out, ScheduledKeyOccupiedCode)
	}
	if out.RunID != occupant.ID {
		t.Errorf("RunID = %s, want the occupant %s", out.RunID, occupant.ID)
	}
	if !strings.Contains(out.Message, occupant.ID.String()) || !strings.Contains(out.Message, "on_demand") {
		t.Errorf("message %q must name the occupant id and its trigger_source on_demand", out.Message)
	}
	if n := runRowCount(repo); n != 1 {
		t.Errorf("run rows = %d, want 1 (only the occupant; the refused window mints nothing)", n)
	}
}

// TestStartScheduledRun_OccupiedKeyOtherWorkflowRefused isolates the
// workflow_id half: the occupant IS a scheduled run, so trigger_source passes
// and only the workflow comparison can refuse it.
//
// Counterfactual: drop ONLY the `rr.WorkflowID != workflowID` disjunct and the
// replay reads already_started — RED, while the on_demand test above stays
// green.
func TestStartScheduledRun_OccupiedKeyOtherWorkflowRefused(t *testing.T) {
	repo := newFakeRepo()
	s := newServer(t, repo)
	occupant := seedOccupant(t, repo, run.TriggerScheduled, "groom")

	out, err := s.StartScheduledRun(context.Background(), scheduledParams(scheduledSpec("scheduled")))
	if err != nil {
		t.Fatalf("StartScheduledRun: %v", err)
	}
	if out.Kind != ScheduledStartRefused || out.Status != http.StatusConflict || out.Code != ScheduledKeyOccupiedCode {
		t.Fatalf("outcome = %+v, want refused/409 %s", out, ScheduledKeyOccupiedCode)
	}
	if out.RunID != occupant.ID {
		t.Errorf("RunID = %s, want the occupant %s", out.RunID, occupant.ID)
	}
	if !strings.Contains(out.Message, "groom") {
		t.Errorf("message %q must name the occupant's workflow groom", out.Message)
	}
	if n := runRowCount(repo); n != 1 {
		t.Errorf("run rows = %d, want 1 (only the occupant)", n)
	}
}

// TestCapturingResponseWriter_DefaultsTo200 pins the net/http default the
// writer mirrors: a handler that writes a body without WriteHeader answered
// 200, and only the FIRST WriteHeader counts.
func TestCapturingResponseWriter_DefaultsTo200(t *testing.T) {
	c := &capturingResponseWriter{header: http.Header{}}
	if c.status() != http.StatusOK {
		t.Errorf("unwritten status = %d, want 200", c.status())
	}
	_, _ = c.Write([]byte("x"))
	c.WriteHeader(http.StatusTeapot)
	if c.status() != http.StatusOK {
		t.Errorf("status after Write then WriteHeader = %d, want 200 (first write wins)", c.status())
	}
}

// reservedBody is a FULLY VALID create body naming trigger_source=scheduled —
// the same spec StartScheduledRun is admitted with — so the unexported
// marker is the only difference between it and the in-process call.
func reservedBody(t *testing.T) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"repo":           "kuhlman-labs/fishhawk",
		"workflow_id":    "upkeep",
		"workflow_sha":   "blobsha",
		"trigger_source": "scheduled",
		"workflow_spec":  scheduledSpec("scheduled"),
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func assertReserved(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 trigger_source_reserved:\n%s", w.Code, w.Body.String())
	}
	env := decodeErrorEnvelope(t, w)
	if env.Code != "trigger_source_reserved" {
		t.Errorf("error code = %q, want trigger_source_reserved (NOT the generic validation_failed):\n%s", env.Code, w.Body.String())
	}
	if !strings.Contains(env.Message, "scheduler") {
		t.Errorf("message %q must name the scheduler the source is reserved to", env.Message)
	}
}

// TestCreateRun_ScheduledReservedOverHTTP is the reservation guard through
// the REAL mux (approval condition 1): an ordinary POST /v0/runs carrying a
// bearer token with write:runs and a fully valid body naming
// trigger_source=scheduled is refused 400 trigger_source_reserved — the
// dedicated code, not validation_failed — and mints no run. The positive
// control on the SAME server and spec (StartScheduledRun → started) proves
// the fixture is otherwise admissible, isolating the marker as the control.
//
// Counterfactual: make the guard permissive (accept scheduled regardless of
// isScheduledAdmission). With the body valid and the token scoped, nothing
// else refuses it, so the POST answers 201 and one row lands — RED.
func TestCreateRun_ScheduledReservedOverHTTP(t *testing.T) {
	runs := newFakeRepo()
	tokens := newFakeTokenRepo()
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: runs, APITokenRepo: tokens,
		AccountRoles: fakeAccountRoles{role: account.RoleAdmin}})
	tok, err := tokens.Issue(context.Background(), "github:42", []string{"write:runs"})
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v0/runs", bytes.NewReader(reservedBody(t)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok.PlainText)
	req.Header.Set("Idempotency-Key", scheduledTestKey)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	assertReserved(t, w)
	if n := runRowCount(runs); n != 0 {
		t.Fatalf("refused POST minted %d runs, want 0", n)
	}

	// Positive control: the in-process call with the same spec is admitted.
	out, err := s.StartScheduledRun(context.Background(), scheduledParams(scheduledSpec("scheduled")))
	if err != nil || out.Kind != ScheduledStartStarted {
		t.Fatalf("positive control: outcome = %+v, err = %v; want started", out, err)
	}
}

// TestCreateRun_ScheduledReservedForCookieOperator: the cookie-session
// operator — who bypasses scope enforcement entirely — is refused the
// reserved source too. The guard keys on the marker, not on the caller's
// privilege.
func TestCreateRun_ScheduledReservedForCookieOperator(t *testing.T) {
	runs := newFakeRepo()
	s := newServer(t, runs)
	req := httptest.NewRequest(http.MethodPost, "/v0/runs", bytes.NewReader(reservedBody(t)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.handleCreateRun(w, withAuth(req))

	assertReserved(t, w)
	if n := runRowCount(runs); n != 0 {
		t.Errorf("refused request minted %d runs, want 0", n)
	}
}

// TestCreateRun_ScheduledReservedBeforeFieldValidation pins the guard's
// ORDERING (approval condition 1): it fires FIRST in body validation, so a
// reserved-source request missing a required field still answers
// trigger_source_reserved rather than a field-level validation_failed.
func TestCreateRun_ScheduledReservedBeforeFieldValidation(t *testing.T) {
	s := newServer(t, newFakeRepo())
	req := httptest.NewRequest(http.MethodPost, "/v0/runs",
		strings.NewReader(`{"repo":"","workflow_id":"w","workflow_sha":"s","trigger_source":"scheduled"}`))
	w := httptest.NewRecorder()
	s.handleCreateRun(w, withAuth(req))
	assertReserved(t, w)
}

// TestScheduledAdmissionMarker_OnlySetByWithScheduledAdmission: a bare
// context, and one carrying a look-alike value under a DIFFERENT key type, do
// not read as admitted.
func TestScheduledAdmissionMarker_OnlySetByWithScheduledAdmission(t *testing.T) {
	type lookalike struct{}
	if isScheduledAdmission(context.Background()) {
		t.Error("bare context reads as scheduled admission")
	}
	if isScheduledAdmission(context.WithValue(context.Background(), lookalike{}, true)) {
		t.Error("a look-alike key reads as scheduled admission")
	}
	if !isScheduledAdmission(withScheduledAdmission(context.Background())) {
		t.Error("withScheduledAdmission did not mark the context")
	}
}
