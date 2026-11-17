package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/approval"
	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/orchestrator"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// The non-Fishhawk declaration changes its input without changing its name:
// routing must follow the declaration, not a repository/workflow allow-list.
const outsideReviewSpec = `version: "1.0"
workflows:
  outside_review:
    stages:
      - id: review
        type: review
        executor:
          human: true
        inputs:
          - source: pull_request
            required: true
        gates:
          - type: approval
            approvals:
              count: 1
              members: ["github:captain"]
              not: [author, agent]
`

// Every transition still reaches the real PostgreSQL state machine. Faults
// model an interrupted URL backfill or one mechanical transition, not approval.
type reviewInitFaultRepo struct {
	run.Repository
	failStage               run.StageState
	urlErr, getErr, listErr error
	readOverride            *run.Run
}

func (r *reviewInitFaultRepo) CreateRun(ctx context.Context, p run.CreateRunParams) (*run.Run, error) {
	// Stand in for installation attribution; the forge itself is an HTTP stub.
	inst := int64(7)
	p.InstallationID = &inst
	return r.Repository.CreateRun(ctx, p)
}
func (r *reviewInitFaultRepo) TransitionStage(ctx context.Context, id uuid.UUID, to run.StageState, c *run.StageCompletion) (*run.Stage, error) {
	if to == r.failStage {
		return nil, errors.New("injected stage transition failure")
	}
	return r.Repository.TransitionStage(ctx, id, to, c)
}
func (r *reviewInitFaultRepo) SetRunPullRequestURL(ctx context.Context, id uuid.UUID, url string) (*run.Run, error) {
	if r.urlErr != nil {
		return nil, r.urlErr
	}
	return r.Repository.SetRunPullRequestURL(ctx, id, url)
}
func (r *reviewInitFaultRepo) GetRun(ctx context.Context, id uuid.UUID) (*run.Run, error) {
	if r.getErr != nil {
		return nil, r.getErr
	}
	if r.readOverride != nil {
		return r.readOverride, nil
	}
	return r.Repository.GetRun(ctx, id)
}
func (r *reviewInitFaultRepo) ListStagesForRun(ctx context.Context, id uuid.UUID) ([]*run.Stage, error) {
	if r.listErr != nil {
		return nil, r.listErr
	}
	return r.Repository.ListStagesForRun(ctx, id)
}

func newReviewFirstFixture(t *testing.T, specBytes []byte, workflowID string, initialize bool) (*Server, *reviewInitFaultRepo, uuid.UUID, uuid.UUID) {
	t.Helper()
	pool := pgtest.NewPool(t)
	rr := &reviewInitFaultRepo{Repository: run.NewPostgresRepository(pool)}
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, ArtifactRepo: artifact.NewPostgresRepository(pool), AuditRepo: audit.NewPostgresRepository(pool), ApprovalRepo: approval.NewPostgresRepository(pool), SigningRepo: newSigningFake()})
	if initialize {
		s.cfg.Orchestrator = &orchestrator.Orchestrator{Runs: rr}
	}
	raw, _ := json.Marshal(map[string]any{"repo": "other/widgets", "workflow_id": workflowID, "workflow_sha": "fixture", "trigger_source": "cli", "runner_kind": "local", "workflow_spec": string(specBytes)})
	req := httptest.NewRequest(http.MethodPost, "/v0/runs", bytes.NewReader(raw))
	w := httptest.NewRecorder()
	s.handleCreateRun(w, withAuth(req))
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", w.Code, w.Body.String())
	}
	var created runResponse
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if initialize && created.State != string(run.StateRunning) {
		t.Fatalf("create response state=%s, want running", created.State)
	}
	stages, err := rr.ListStagesForRun(context.Background(), created.ID)
	if err != nil || len(stages) != 1 {
		t.Fatalf("stages=%v error=%v", stages, err)
	}
	return s, rr, created.ID, stages[0].ID
}
func reviewFirstPRBytes(t *testing.T) []byte {
	t.Helper()
	var pr pullRequestBody
	if err := json.Unmarshal(validPRBytes(t), &pr); err != nil {
		t.Fatal(err)
	}
	pr.PRURL = "https://github.com/other/widgets/pull/42"
	raw, err := json.Marshal(pr)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func reviewFirstUpload(t *testing.T, s *Server, runID, stageID uuid.UUID, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/v0/runs/%s/pull-request?stage_id=%s", runID, stageID), bytes.NewReader(body))
	req.SetPathValue("run_id", runID.String())
	id := testOperatorIdentity()
	id.Scopes = []string{"write:runs"}
	req = req.WithContext(context.WithValue(req.Context(), ctxKeyIdentity, id))
	w := httptest.NewRecorder()
	s.handleShipPullRequest(w, req)
	return w
}
func assertReviewFirstState(t *testing.T, rr run.Repository, runID, stageID uuid.UUID, rs run.State, ss run.StageState) {
	t.Helper()
	ctx := context.Background()
	row, err := rr.GetRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	st, err := rr.GetStage(ctx, stageID)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != rs || st.State != ss {
		t.Fatalf("run/stage=%s/%s, want %s/%s", row.State, st.State, rs, ss)
	}
}
func assertReviewAdmissionReads(t *testing.T, s *Server, runID, stageID uuid.UUID, want string) {
	t.Helper()
	for _, path := range []string{fmt.Sprintf("/v0/runs/%s/stages", runID), fmt.Sprintf("/v0/stages/%s", stageID), fmt.Sprintf("/v0/runs/%s/stages/%s", runID, stageID)} {
		req := withAuth(httptest.NewRequest(http.MethodGet, path, nil))
		req.SetPathValue("run_id", runID.String())
		req.SetPathValue("stage_id", stageID.String())
		w := httptest.NewRecorder()
		switch path {
		case fmt.Sprintf("/v0/runs/%s/stages", runID):
			s.handleListRunStages(w, req)
		case fmt.Sprintf("/v0/stages/%s", stageID):
			s.handleGetStage(w, req)
		default:
			s.handleGetRunStage(w, req)
		}
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s=%d: %s", path, w.Code, w.Body.String())
		}
		var raw map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
			t.Fatal(err)
		}
		if items, ok := raw["items"].([]any); ok {
			raw = items[0].(map[string]any)
		}
		if got := raw["review_gate_admission"]; got != want {
			t.Errorf("GET %s admission=%v, want %s", path, got, want)
		}
	}
}
func setReviewFirstForge(t *testing.T, s *Server, member string) *membersGitHubStub {
	t.Helper()
	stub := &membersGitHubStub{mmGitHub: mmGitHub{merger: "outside-merger"}}
	s.cfg.GitHub = newMembersGitHubClient(t, stub)
	t.Cleanup(s.waitBackgroundReviews)
	if member != "" {
		stub.setReviews([]mmReview{{login: member, state: "APPROVED"}})
	}
	return stub
}

func TestHumanReviewFirst_Lifecycle_PgBacked(t *testing.T) {
	shipped, err := os.ReadFile(filepath.Join(pkgSrcDir, "..", "..", "..", ".fishhawk", "workflows.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, id, member string
		spec             []byte
	}{{"external", "outside_review", "captain", []byte(outsideReviewSpec)}, {"shipped", "human_led_change", "kuhlman-labs", shipped}} {
		t.Run(tc.name, func(t *testing.T) {
			s, rr, rid, sid := newReviewFirstFixture(t, tc.spec, tc.id, true)
			assertReviewFirstState(t, rr, rid, sid, run.StateRunning, run.StageStatePending)
			// A second generic Advance must also honor the missing PR input.
			if _, err := s.cfg.Orchestrator.Advance(context.Background(), rid); err != nil {
				t.Fatal(err)
			}
			assertReviewFirstState(t, rr, rid, sid, run.StateRunning, run.StageStatePending)
			assertReviewAdmissionReads(t, s, rid, sid, "pull_request_managed")
			if w := reviewFirstUpload(t, s, rid, sid, []byte(`{}`)); w.Code != http.StatusBadRequest {
				t.Fatalf("invalid PR=%d: %s", w.Code, w.Body.String())
			}
			assertReviewFirstState(t, rr, rid, sid, run.StateRunning, run.StageStatePending)
			for _, before := range []bool{true, false} {
				if !before {
					if w := reviewFirstUpload(t, s, rid, sid, reviewFirstPRBytes(t)); w.Code != http.StatusCreated {
						t.Fatalf("upload=%d: %s", w.Code, w.Body.String())
					}
					assertReviewFirstState(t, rr, rid, sid, run.StateRunning, run.StageStateAwaitingApproval)
					if w := reviewFirstUpload(t, s, rid, sid, reviewFirstPRBytes(t)); w.Code != http.StatusOK {
						t.Fatalf("replay=%d: %s", w.Code, w.Body.String())
					}
				}
				if w := submitApproval(t, s, sid, `{"decision":"approve","comment":"operator assertion cannot land a PR"}`); w.Code != http.StatusConflict {
					t.Fatalf("direct approval=%d: %s", w.Code, w.Body.String())
				}
			}
			items, err := s.cfg.ArtifactRepo.ListForStage(context.Background(), sid)
			if err != nil || len(items) != 1 {
				t.Fatalf("artifacts=%v err=%v", items, err)
			}
			stub := setReviewFirstForge(t, s, "")
			url := "https://github.com/other/widgets/pull/42"
			if err := s.ResolveReviewFromPollState(context.Background(), rid, true, url); err != nil {
				t.Fatal(err)
			}
			assertReviewFirstState(t, rr, rid, sid, run.StateRunning, run.StageStateAwaitingApproval)
			stub.setReviews([]mmReview{{login: tc.member, state: "APPROVED"}})
			if err := s.ResolveReviewFromPollState(context.Background(), rid, true, url); err != nil {
				t.Fatal(err)
			}
			assertReviewFirstState(t, rr, rid, sid, run.StateSucceeded, run.StageStateSucceeded)
			if stub.calls() == 0 {
				t.Fatal("merge settled without reading forge identities")
			}
			if err := s.ResolveReviewFromPollState(context.Background(), rid, true, url); err != nil {
				t.Fatal(err)
			}
			assertReviewFirstState(t, rr, rid, sid, run.StateSucceeded, run.StageStateSucceeded)
		})
	}
}

func TestHumanReviewFirst_PRReplayHealsInitialization_PgBacked(t *testing.T) {
	for _, tc := range []struct {
		name       string
		fail, held run.StageState
		url        bool
	}{{"url-backfill", "", run.StageStatePending, true}, {"before-dispatched", run.StageStateDispatched, run.StageStatePending, false}, {"before-running", run.StageStateRunning, run.StageStateDispatched, false}, {"before-gate", run.StageStateAwaitingApproval, run.StageStateRunning, false}} {
		t.Run(tc.name, func(t *testing.T) {
			s, rr, rid, sid := newReviewFirstFixture(t, []byte(outsideReviewSpec), "outside_review", true)
			rr.failStage = tc.fail
			if tc.url {
				rr.urlErr = errors.New("injected backfill failure")
			}
			if w := reviewFirstUpload(t, s, rid, sid, reviewFirstPRBytes(t)); w.Code != http.StatusInternalServerError {
				t.Fatalf("interrupted upload=%d: %s", w.Code, w.Body.String())
			}
			assertReviewFirstState(t, rr, rid, sid, run.StateRunning, tc.held)
			rr.failStage = ""
			rr.urlErr = nil
			if w := reviewFirstUpload(t, s, rid, sid, reviewFirstPRBytes(t)); w.Code != http.StatusOK {
				t.Fatalf("replay=%d: %s", w.Code, w.Body.String())
			}
			assertReviewFirstState(t, rr, rid, sid, run.StateRunning, run.StageStateAwaitingApproval)
			items, err := s.cfg.ArtifactRepo.ListForStage(context.Background(), sid)
			if err != nil || len(items) != 1 {
				t.Fatalf("artifacts=%v err=%v", items, err)
			}
			// A replay after parking must not advance a later stage past the human.
			later, err := rr.CreateStage(context.Background(), run.CreateStageParams{RunID: rid, Sequence: 1, Type: run.StageTypeImplement, ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code"})
			if err != nil {
				t.Fatal(err)
			}
			if w := reviewFirstUpload(t, s, rid, sid, reviewFirstPRBytes(t)); w.Code != http.StatusOK {
				t.Fatalf("parked replay=%d: %s", w.Code, w.Body.String())
			}
			got, err := rr.GetStage(context.Background(), later.ID)
			if err != nil || got.State != run.StageStatePending {
				t.Fatalf("later stage=%v err=%v", got, err)
			}
		})
	}
}

func TestHumanReviewFirst_DeclarationSelectsGate_PgBacked(t *testing.T) {
	specBytes := []byte(strings.Replace(outsideReviewSpec, "        inputs:\n          - source: pull_request\n            required: true\n", "", 1))
	s, rr, rid, sid := newReviewFirstFixture(t, specBytes, "outside_review", true)
	assertReviewFirstState(t, rr, rid, sid, run.StateRunning, run.StageStateAwaitingApproval)
	assertReviewAdmissionReads(t, s, rid, sid, "admitted")
}

func TestHumanReviewFirst_LegacyPendingMerge_PgBacked(t *testing.T) {
	s, rr, rid, sid := newReviewFirstFixture(t, []byte(outsideReviewSpec), "outside_review", false)
	assertReviewFirstState(t, rr, rid, sid, run.StatePending, run.StageStatePending)
	// Persist the PR as the old upload path did, with no initializer available.
	if w := reviewFirstUpload(t, s, rid, sid, reviewFirstPRBytes(t)); w.Code != http.StatusInternalServerError {
		t.Fatalf("uninitialized upload=%d: %s", w.Code, w.Body.String())
	}
	setReviewFirstForge(t, s, "captain")
	if err := s.ResolveReviewFromPollState(context.Background(), rid, true, "https://github.com/other/widgets/pull/42"); err != nil {
		t.Fatal(err)
	}
	assertReviewFirstState(t, rr, rid, sid, run.StatePending, run.StageStatePending)
	s.cfg.Orchestrator = &orchestrator.Orchestrator{Runs: rr}
	if err := s.ResolveReviewFromPollState(context.Background(), rid, true, "https://github.com/other/widgets/pull/42"); err != nil {
		t.Fatal(err)
	}
	assertReviewFirstState(t, rr, rid, sid, run.StateSucceeded, run.StageStateSucceeded)
}

// Actual handler bytes are shared with the MCP classifier tests; neither side
// can silently rename or omit the routing field while its own unit tests pass.
func TestHumanReviewFirst_WireFixture(t *testing.T) {
	s, _, rid, sid := newReviewFirstFixture(t, []byte(outsideReviewSpec), "outside_review", true)
	s.cfg.FeedbackProviders = func() []string { return []string{"github_projects"} }
	ids := map[string]string{rid.String(): "00000000-0000-0000-0000-000000000001", sid.String(): "00000000-0000-0000-0000-000000000002"}
	var unknown []string
	snapshot := func() map[string]any {
		return map[string]any{
			"run":    canonicalizeWireBody(t, getRawBody(t, s, fmt.Sprintf("/v0/runs/%s", rid)), ids, &unknown),
			"stages": canonicalizeWireBody(t, getRawBody(t, s, fmt.Sprintf("/v0/runs/%s/stages", rid)), ids, &unknown),
		}
	}
	waiting := snapshot()
	if w := reviewFirstUpload(t, s, rid, sid, reviewFirstPRBytes(t)); w.Code != http.StatusCreated {
		t.Fatalf("upload=%d: %s", w.Code, w.Body.String())
	}
	raw, err := json.MarshalIndent(map[string]any{"waiting": waiting, "reviewing": snapshot()}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, '\n')
	if len(unknown) != 0 {
		t.Fatalf("unseeded IDs: %v", unknown)
	}
	want, err := os.ReadFile(filepath.Join(pkgSrcDir, "testdata", "human_review_first_wire.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, want) {
		t.Fatalf("BEGIN REVIEW FIRST WIRE\n%sEND REVIEW FIRST WIRE", raw)
	}
}

func TestHumanReviewFirst_ReadFailuresDoNotInitialize_PgBacked(t *testing.T) {
	s, rr, rid, sid := newReviewFirstFixture(t, []byte(outsideReviewSpec), "outside_review", true)
	stage, err := rr.GetStage(context.Background(), sid)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.initializeHumanReviewAfterPR(context.Background(), rid, stage, ""); err == nil {
		t.Fatal("missing PR URL initialized review")
	}
	rr.getErr = errors.New("injected run read failure")
	assertReviewAdmissionReads(t, s, rid, sid, "run_unavailable")
	if err := s.initializeHumanReviewAfterPR(context.Background(), rid, stage, "https://github.com/other/widgets/pull/42"); err == nil {
		t.Fatal("run read failure initialized review")
	}
	rr.getErr = nil
	rr.listErr = errors.New("injected stage list failure")
	if err := s.initializeHumanReviewAfterPR(context.Background(), rid, stage, "https://github.com/other/widgets/pull/42"); err == nil {
		t.Fatal("stage list failure initialized review")
	}
	rr.listErr = nil
	assertReviewFirstState(t, rr, rid, sid, run.StateRunning, run.StageStatePending)
}

func TestHumanReviewFirst_UnavailableDeclarationDoesNotInitialize_PgBacked(t *testing.T) {
	for _, tc := range []struct{ name, workflow, spec string }{{"absent", "outside_review", ""}, {"malformed", "outside_review", "not: [yaml"}, {"workflow-missing", "unknown", outsideReviewSpec}} {
		t.Run(tc.name, func(t *testing.T) {
			s, rr, rid, sid := newReviewFirstFixture(t, []byte(outsideReviewSpec), "outside_review", true)
			row, err := rr.GetRun(context.Background(), rid)
			if err != nil {
				t.Fatal(err)
			}
			// Supply an unavailable read, never rewrite the persisted cached workflow.
			override := *row
			override.WorkflowSpec = []byte(tc.spec)
			override.WorkflowID = tc.workflow
			rr.readOverride = &override
			stage, err := rr.GetStage(context.Background(), sid)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.initializeHumanReviewFirst(context.Background(), &override, []*run.Stage{stage}); err == nil {
				t.Fatal("unresolved declaration initialized review")
			}
			if err := s.initializeHumanReviewAfterPR(context.Background(), rid, stage, "https://github.com/other/widgets/pull/42"); err == nil {
				t.Fatal("PR attachment admitted unresolved declaration")
			}
			if _, err := s.cfg.Orchestrator.Advance(context.Background(), rid); err == nil {
				t.Fatal("generic Advance admitted unavailable declaration")
			}
			rr.readOverride = nil
			assertReviewFirstState(t, rr, rid, sid, run.StateRunning, run.StageStatePending)
		})
	}
}

func TestHumanReviewFirst_StaleReplayCannotAdvancePastGate_PgBacked(t *testing.T) {
	s, rr, rid, sid := newReviewFirstFixture(t, []byte(outsideReviewSpec), "outside_review", true)
	stale, err := rr.GetStage(context.Background(), sid)
	if err != nil {
		t.Fatal(err)
	}
	if w := reviewFirstUpload(t, s, rid, sid, reviewFirstPRBytes(t)); w.Code != http.StatusCreated {
		t.Fatalf("upload=%d: %s", w.Code, w.Body.String())
	}
	later, err := rr.CreateStage(context.Background(), run.CreateStageParams{RunID: rid, Sequence: 1, Type: run.StageTypeImplement, ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.initializeHumanReviewAfterPR(context.Background(), rid, stale, "https://github.com/other/widgets/pull/42"); err != nil {
		t.Fatal(err)
	}
	row, err := rr.GetRun(context.Background(), rid)
	if err != nil {
		t.Fatal(err)
	}
	stages, err := rr.ListStagesForRun(context.Background(), rid)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.initializeHumanReviewFirst(context.Background(), row, stages); err != nil {
		t.Fatal(err)
	}
	parked, err := rr.GetStage(context.Background(), sid)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.initializeHumanReviewAfterPR(context.Background(), rid, parked, "https://github.com/other/widgets/pull/42"); err != nil {
		t.Fatal(err)
	}
	next, err := rr.GetStage(context.Background(), later.ID)
	if err != nil || next.State != run.StageStatePending {
		t.Fatalf("later=%v err=%v", next, err)
	}
	assertReviewFirstState(t, rr, rid, sid, run.StateRunning, run.StageStateAwaitingApproval)
}

func TestHumanReviewFirst_PRReplayStillRequiresInitialization_PgBacked(t *testing.T) {
	s, rr, rid, sid := newReviewFirstFixture(t, []byte(outsideReviewSpec), "outside_review", true)
	rr.failStage = run.StageStateAwaitingApproval
	for i := 0; i < 2; i++ {
		if w := reviewFirstUpload(t, s, rid, sid, reviewFirstPRBytes(t)); w.Code != http.StatusInternalServerError {
			t.Fatalf("attempt %d=%d: %s", i, w.Code, w.Body.String())
		}
	}
	assertReviewFirstState(t, rr, rid, sid, run.StateRunning, run.StageStateRunning)
	rr.failStage = ""
	if w := reviewFirstUpload(t, s, rid, sid, reviewFirstPRBytes(t)); w.Code != http.StatusOK {
		t.Fatalf("retry=%d: %s", w.Code, w.Body.String())
	}
	assertReviewFirstState(t, rr, rid, sid, run.StateRunning, run.StageStateAwaitingApproval)
}

func TestHumanReviewFirst_PRReplayChecksLineage_PgBacked(t *testing.T) {
	s, rr, rid, sid := newReviewFirstFixture(t, []byte(outsideReviewSpec), "outside_review", true)
	rr.failStage = run.StageStateDispatched
	if w := reviewFirstUpload(t, s, rid, sid, reviewFirstPRBytes(t)); w.Code != http.StatusInternalServerError {
		t.Fatalf("first=%d: %s", w.Code, w.Body.String())
	}
	rr.failStage = ""
	stub := &lineageGitHub{baseRef: "main", commitsByBase: map[string][]string{"main": {"ffffffffffffffffffffffffffffffffffffffff", "1111111111111111111111111111111111111111"}}}
	s.cfg.GitHub = newLineageGitHubClient(t, stub)
	t.Cleanup(s.waitBackgroundReviews)
	if w := reviewFirstUpload(t, s, rid, sid, reviewFirstPRBytes(t)); w.Code != http.StatusOK {
		t.Fatalf("replay=%d: %s", w.Code, w.Body.String())
	}
	stage, err := rr.GetStage(context.Background(), sid)
	if err != nil {
		t.Fatal(err)
	}
	if stage.State == run.StageStateAwaitingApproval {
		t.Fatal("foreign branch initialized human review")
	}
	if calls := stub.compareCalls(); len(calls) == 0 {
		t.Fatal("replay never checked branch lineage")
	}
}

func TestHumanReviewFirst_LinkedMergeCannotConfirmOperatorGate_PgBacked(t *testing.T) {
	specBytes := []byte(strings.Replace(outsideReviewSpec, "        inputs:\n          - source: pull_request\n            required: true\n", "", 1))
	s, rr, rid, sid := newReviewFirstFixture(t, specBytes, "outside_review", true)
	if w := reviewFirstUpload(t, s, rid, sid, reviewFirstPRBytes(t)); w.Code != http.StatusCreated {
		t.Fatalf("linked PR=%d: %s", w.Code, w.Body.String())
	}
	setReviewFirstForge(t, s, "captain")
	if err := s.ResolveReviewFromPollState(context.Background(), rid, true, "https://github.com/other/widgets/pull/42"); err != nil {
		t.Fatal(err)
	}
	assertReviewFirstState(t, rr, rid, sid, run.StateRunning, run.StageStateAwaitingApproval)
	assertReviewAdmissionReads(t, s, rid, sid, "admitted")
}
