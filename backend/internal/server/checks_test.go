package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/auditcheckpublisher"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/stagecheck"
)

// stageCheckRepoFake implements the slice of stagecheck.Repository
// that the GET-checks handler exercises. Tests seed canned states.
type stageCheckRepoFake struct {
	byKey         map[string]*stagecheck.Check
	byStage       map[uuid.UUID][]*stagecheck.Check
	listErr       error
	matchingErr   error
	matchedStages []uuid.UUID
	appendCalls   []stagecheck.AppendParams
}

func newStageCheckRepoFake() *stageCheckRepoFake {
	return &stageCheckRepoFake{
		byKey:   map[string]*stagecheck.Check{},
		byStage: map[uuid.UUID][]*stagecheck.Check{},
	}
}

func (f *stageCheckRepoFake) seed(stageID uuid.UUID, c *stagecheck.Check) {
	f.byKey[stageID.String()+":"+c.Name] = c
	f.byStage[stageID] = append(f.byStage[stageID], c)
}

func (f *stageCheckRepoFake) Append(_ context.Context, p stagecheck.AppendParams) (*stagecheck.Check, error) {
	f.appendCalls = append(f.appendCalls, p)
	return &stagecheck.Check{StageID: p.StageID, Name: p.Name, State: stagecheck.DeriveState(p.Status, p.Conclusion)}, nil
}
func (f *stageCheckRepoFake) LatestForStage(_ context.Context, stageID uuid.UUID) ([]*stagecheck.Check, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.byStage[stageID], nil
}
func (f *stageCheckRepoFake) LatestForStageAndName(_ context.Context, stageID uuid.UUID, name string) (*stagecheck.Check, error) {
	if c, ok := f.byKey[stageID.String()+":"+name]; ok {
		return c, nil
	}
	return nil, stagecheck.ErrNotFound
}
func (f *stageCheckRepoFake) FindMatchingStages(_ context.Context, _ int, _, _ string) ([]uuid.UUID, error) {
	if f.matchingErr != nil {
		return nil, f.matchingErr
	}
	return f.matchedStages, nil
}

// stageGetterRepo is a minimal run.Repository fake for the
// checks-read handler. GetStage and GetRun are exercised — the
// latter feeds the run's required-checks snapshot into the
// `declared` list (#251 / #254).
type stageGetterRepo struct {
	stages map[uuid.UUID]*run.Stage
	runs   map[uuid.UUID]*run.Run
	getErr error
}

func newStageGetterRepo() *stageGetterRepo {
	return &stageGetterRepo{
		stages: map[uuid.UUID]*run.Stage{},
		runs:   map[uuid.UUID]*run.Run{},
	}
}
func (r *stageGetterRepo) GetStage(_ context.Context, id uuid.UUID) (*run.Stage, error) {
	if r.getErr != nil {
		return nil, r.getErr
	}
	if s, ok := r.stages[id]; ok {
		return s, nil
	}
	return nil, run.ErrNotFound
}
func (r *stageGetterRepo) CreateRun(context.Context, run.CreateRunParams) (*run.Run, error) {
	return nil, errors.New("not used")
}
func (r *stageGetterRepo) GetRun(_ context.Context, id uuid.UUID) (*run.Run, error) {
	if rn, ok := r.runs[id]; ok {
		return rn, nil
	}
	return nil, run.ErrNotFound
}
func (r *stageGetterRepo) GetRunByIdempotencyKey(context.Context, string, string) (*run.Run, error) {
	return nil, run.ErrNotFound
}
func (r *stageGetterRepo) ListRuns(context.Context, run.ListRunsFilter) ([]*run.Run, error) {
	return nil, errors.New("not used")
}
func (r *stageGetterRepo) TransitionRun(context.Context, uuid.UUID, run.State) (*run.Run, error) {
	return nil, errors.New("not used")
}
func (r *stageGetterRepo) RetryRun(context.Context, uuid.UUID, run.State) (*run.Run, error) {
	return nil, errors.New("not used")
}
func (r *stageGetterRepo) SetRunPullRequestURL(context.Context, uuid.UUID, string) (*run.Run, error) {
	return nil, errors.New("not used")
}
func (r *stageGetterRepo) CreateStage(context.Context, run.CreateStageParams) (*run.Stage, error) {
	return nil, errors.New("not used")
}
func (r *stageGetterRepo) ListStagesForRun(context.Context, uuid.UUID) ([]*run.Stage, error) {
	return nil, errors.New("not used")
}
func (r *stageGetterRepo) ListStagesAwaitingApproval(context.Context) ([]*run.Stage, error) {
	return nil, errors.New("not used")
}
func (r *stageGetterRepo) ListReviewStagesAwaitingApproval(context.Context) ([]*run.Stage, error) {
	return nil, errors.New("not used")
}

func (r *stageGetterRepo) ListStagesAwaitingChildren(context.Context) ([]*run.Stage, error) {
	return nil, errors.New("not used")
}
func (r *stageGetterRepo) ListStagesDispatched(context.Context) ([]*run.Stage, error) {
	return nil, nil
}
func (r *stageGetterRepo) TransitionStage(context.Context, uuid.UUID, run.StageState, *run.StageCompletion) (*run.Stage, error) {
	return nil, errors.New("not used")
}
func (r *stageGetterRepo) RetryStage(context.Context, uuid.UUID, run.StageState) (*run.Stage, error) {
	return nil, errors.New("not used")
}

func newChecksServer(t *testing.T) (*Server, *stageGetterRepo, *stageCheckRepoFake) {
	t.Helper()
	rr := newStageGetterRepo()
	scs := newStageCheckRepoFake()
	s := New(Config{
		Addr: "127.0.0.1:0", RunRepo: rr, StageCheckRepo: scs,
	})
	return s, rr, scs
}

func ptrStr(s string) *string { return &s }

func TestListStageChecks_HappyPath(t *testing.T) {
	s, rr, scs := newChecksServer(t)
	runID := uuid.New()
	stageID := uuid.New()
	rr.runs[runID] = &run.Run{
		ID: runID,
		// #251 / #254: the response's `declared` list now sources
		// from the run's branch-protection snapshot rather than the
		// dropped spec-level gate.blocking_checks field.
		RequiredChecksSnapshot: &run.RequiredChecksSnapshot{
			Contexts: []string{"ci_pass", "fishhawk_audit_complete"},
			Sources:  []string{"branch_protection"},
		},
	}
	rr.stages[stageID] = &run.Stage{
		ID:    stageID,
		RunID: runID,
		Type:  run.StageTypeReview,
		Gate: &run.Gate{
			Kind: run.GateKindApproval,
		},
	}
	scs.seed(stageID, &stagecheck.Check{
		StageID: stageID, Name: "ci_pass", State: stagecheck.StatePass,
		Status: "completed", Conclusion: ptrStr("success"),
		HeadSHA: "abc", Timestamp: time.Now().UTC(),
	})

	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/v0/stages/%s/checks", stageID), nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
	}
	var got stageChecksListResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Declared) != 2 {
		t.Errorf("Declared len = %d, want 2", len(got.Declared))
	}
	if len(got.Items) != 1 {
		t.Errorf("Items len = %d, want 1 (only ci_pass observed)", len(got.Items))
	}
	if got.Items[0].Name != "ci_pass" || got.Items[0].State != string(stagecheck.StatePass) {
		t.Errorf("first item = %+v, want ci_pass/pass", got.Items[0])
	}
}

func TestListStageChecks_DeclaredButNotObserved(t *testing.T) {
	// Empty Items + non-empty Declared → SPA renders all as
	// not_tracked. The handler doesn't fill in placeholder rows;
	// the SPA does that derivation. `Declared` sources from the
	// run's required-checks snapshot post-#254.
	s, rr, _ := newChecksServer(t)
	runID := uuid.New()
	stageID := uuid.New()
	rr.runs[runID] = &run.Run{
		ID: runID,
		RequiredChecksSnapshot: &run.RequiredChecksSnapshot{
			Contexts: []string{"ci_pass"},
			Sources:  []string{"branch_protection"},
		},
	}
	rr.stages[stageID] = &run.Stage{
		ID:    stageID,
		RunID: runID,
		Type:  run.StageTypeReview,
		Gate: &run.Gate{
			Kind: run.GateKindApproval,
		},
	}
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/v0/stages/%s/checks", stageID), nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var got stageChecksListResponse
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if len(got.Items) != 0 {
		t.Errorf("Items should be empty when nothing observed, got %+v", got.Items)
	}
	if len(got.Declared) != 1 || got.Declared[0] != "ci_pass" {
		t.Errorf("Declared = %v, want [ci_pass]", got.Declared)
	}
}

func TestListStageChecks_StageMissing_404(t *testing.T) {
	s, _, _ := newChecksServer(t)
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/v0/stages/%s/checks", uuid.New()), nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestListStageChecks_BadStageID_400(t *testing.T) {
	s, _, _ := newChecksServer(t)
	req := httptest.NewRequest(http.MethodGet, "/v0/stages/not-a-uuid/checks", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestListStageChecks_Unconfigured_503(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0"})
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/v0/stages/%s/checks", uuid.New()), nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
}

// TestFixupHeadResolution_ServerAndPublisherAgree is the #1682 binding
// condition 1 cross-boundary assertion: the server-side resolver
// (s.latestRunHeadSHA) and the publisher-side resolver (findHeadSHA, exercised
// through Publish's posted head) resolve the IDENTICAL head for the SAME audit
// history — a newest fixup_pushed head that supersedes the stale PR-open
// artifact head. Both delegate to auditcomplete.LatestReportedHeadSHA, so they
// cannot diverge; this proves it end-to-end across the two packages.
func TestFixupHeadResolution_ServerAndPublisherAgree(t *testing.T) {
	rr := newOrchestratorRepo()
	r := rr.seedRun()
	r.InstallationID = ptrInt64(99)
	r.Repo = "x/y"
	impl := rr.seedStage(r.ID, 1, run.StageStateSucceeded)
	impl.Type = run.StageTypeImplement

	arts := newFakeArtifactRepo()
	// The PR-open artifact carries the STALE head; a fix-up pushed a newer one.
	arts.all = append(arts.all, &artifact.Artifact{
		ID: uuid.New(), StageID: impl.ID,
		Kind:    artifact.KindPullRequest,
		Content: pullRequestArtifactBody("stalehead"),
	})

	au := newAuditFake()
	rid := r.ID
	fixPayload, _ := json.Marshal(map[string]any{"head_sha": "freshfixhead"})
	prPayload, _ := json.Marshal(map[string]any{"head_sha": "stalehead"})
	au.seeded = []*audit.Entry{
		{RunID: &rid, Category: "pull_request_opened", Sequence: 1, Payload: prPayload},
		{RunID: &rid, Category: "fixup_pushed", Sequence: 9, Payload: fixPayload},
	}

	s := New(Config{
		Addr: "127.0.0.1:0", RunRepo: rr, AuditRepo: au, ArtifactRepo: arts,
		ExternalURL: "https://app.fishhawk.example.com",
	})

	// Server-side resolution.
	serverHead, ok, err := s.latestRunHeadSHA(context.Background(), r.ID)
	if err != nil || !ok {
		t.Fatalf("latestRunHeadSHA = (%q, %v, %v)", serverHead, ok, err)
	}

	// Publisher-side resolution (through Publish's posted head).
	gh := newPublisherFakeGitHub()
	pub := auditcheckpublisher.New(auditcheckpublisher.Deps{
		GitHub: gh, Runs: rr, Artifacts: arts, Audit: au,
		ExternalURL: "https://app.fishhawk.example.com",
	})
	if pub == nil {
		t.Fatal("publisher nil")
	}
	if _, err := pub.Publish(context.Background(), r.ID, stagecheck.StatePass, nil); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if len(gh.calls()) != 1 {
		t.Fatalf("expected 1 publish, got %d", len(gh.calls()))
	}
	publisherHead := gh.calls()[0].params.HeadSHA

	if serverHead != "freshfixhead" {
		t.Errorf("server head = %q, want freshfixhead", serverHead)
	}
	if serverHead != publisherHead {
		t.Errorf("head divergence: server = %q, publisher = %q (must be identical)", serverHead, publisherHead)
	}
}
