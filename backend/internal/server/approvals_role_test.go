package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/role"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// approvalGateRunRepo is the fake run.Repository used by the
// role-check tests. It supports GetStage, GetRun, and TransitionStage.
type approvalGateRunRepo struct {
	mu          sync.Mutex
	stage       *run.Stage
	runRow      *run.Run
	transitions []approvalTransition
}

func (r *approvalGateRunRepo) GetStage(_ context.Context, id uuid.UUID) (*run.Stage, error) {
	if r.stage != nil && r.stage.ID == id {
		return r.stage, nil
	}
	return nil, run.ErrNotFound
}

func (r *approvalGateRunRepo) GetRun(_ context.Context, id uuid.UUID) (*run.Run, error) {
	if r.runRow != nil && r.runRow.ID == id {
		return r.runRow, nil
	}
	return nil, run.ErrNotFound
}

func (r *approvalGateRunRepo) TransitionStage(_ context.Context, id uuid.UUID, to run.StageState, c *run.StageCompletion) (*run.Stage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.transitions = append(r.transitions, approvalTransition{StageID: id, To: to, Completion: c})
	if r.stage != nil && r.stage.ID == id {
		r.stage.State = to
	}
	return r.stage, nil
}

// Stub the rest.
func (r *approvalGateRunRepo) CreateRun(context.Context, run.CreateRunParams) (*run.Run, error) {
	return nil, errors.New("not used")
}
func (r *approvalGateRunRepo) GetRunByIdempotencyKey(context.Context, string, string) (*run.Run, error) {
	return nil, run.ErrNotFound
}
func (r *approvalGateRunRepo) ListRuns(context.Context, run.ListRunsFilter) ([]*run.Run, error) {
	return nil, errors.New("not used")
}
func (r *approvalGateRunRepo) TransitionRun(context.Context, uuid.UUID, run.State) (*run.Run, error) {
	return nil, errors.New("not used")
}
func (r *approvalGateRunRepo) CreateStage(context.Context, run.CreateStageParams) (*run.Stage, error) {
	return nil, errors.New("not used")
}
func (r *approvalGateRunRepo) ListStagesForRun(context.Context, uuid.UUID) ([]*run.Stage, error) {
	return nil, errors.New("not used")
}
func (r *approvalGateRunRepo) ListStagesAwaitingApproval(context.Context) ([]*run.Stage, error) {
	return nil, errors.New("not used")
}

func (r *approvalGateRunRepo) ListStagesDispatched(context.Context) ([]*run.Stage, error) {
	return nil, nil
}

// stubTeamLister satisfies role.TeamLister for the resolver in
// these tests. teamMembers is keyed "org/slug".
type stubTeamLister struct {
	teamMembers map[string][]role.TeamMember
}

func (s *stubTeamLister) ListTeamMembers(_ context.Context, _ int64, org, slug string) ([]role.TeamMember, error) {
	if got, ok := s.teamMembers[org+"/"+slug]; ok {
		return got, nil
	}
	return nil, nil
}

const approvalGateSpec = `
version: "0.1"
roles:
  eng_team:
    members: ["@acme/eng"]
  leads:
    members: ["@acme/leads"]
workflows:
  feature_change:
    description: Test workflow
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        gates:
          - type: approval
            approvers:
              any_of: [eng_team]
            sla: 4_hours
`

func newRoleApprovalServer(t *testing.T, members map[string][]role.TeamMember) (*Server, *approvalGateRunRepo, *stubWorkflowSpecFetcher, *fakeApprovalRepo, *auditFake) {
	t.Helper()
	stage := &run.Stage{
		ID:           uuid.New(),
		RunID:        uuid.New(),
		Sequence:     0,
		Type:         run.StageTypeImplement,
		ExecutorKind: run.ExecutorAgent,
		ExecutorRef:  "claude-code",
		State:        run.StageStateAwaitingApproval,
		CreatedAt:    time.Now().UTC(),
		UpdatedAt:    time.Now().UTC(),
	}
	installation := int64(99)
	runRow := &run.Run{
		ID:             stage.RunID,
		Repo:           "kuhlman-labs/example",
		WorkflowID:     "feature_change",
		WorkflowSHA:    "abc123",
		InstallationID: &installation,
	}
	rr := &approvalGateRunRepo{stage: stage, runRow: runRow}
	gh := &stubWorkflowSpecFetcher{content: []byte(approvalGateSpec), sha: "abc123"}
	resolver := role.NewResolver(&stubTeamLister{teamMembers: members})
	apRepo := newFakeApprovalRepo()
	auditFake := newAuditFake()
	s := New(Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      rr,
		ApprovalRepo: apRepo,
		AuditRepo:    auditFake,
		RoleResolver: resolver,
	})
	s.traceWorkflowSpecOverride = gh
	return s, rr, gh, apRepo, auditFake
}

// approveRequest builds a POST /v0/stages/{id}/approvals request
// with the given subject. We inject identity directly via context
// + set the path value, bypassing the muxer (which would otherwise
// run bearerAuth and overwrite our injected identity).
func approveRequest(t *testing.T, s *Server, stageID uuid.UUID, subject, decision string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"decision": decision})
	req := httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/v0/stages/%s/approvals", stageID),
		strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("stage_id", stageID.String())
	ctx := context.WithValue(req.Context(), ctxKeyIdentity, Identity{Subject: subject})
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()
	s.handleSubmitApproval(w, req)
	return w
}

func TestApproval_RoleCheck_Allowed(t *testing.T) {
	s, rr, _, apRepo, _ := newRoleApprovalServer(t, map[string][]role.TeamMember{
		"acme/eng": {{Login: "alice"}, {Login: "bob"}},
	})
	w := approveRequest(t, s, rr.stage.ID, "alice", "approve")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	if len(apRepo.all) != 1 {
		t.Errorf("expected 1 approval recorded, got %d", len(apRepo.all))
	}
}

func TestApproval_RoleCheck_Denied(t *testing.T) {
	s, rr, _, apRepo, _ := newRoleApprovalServer(t, map[string][]role.TeamMember{
		"acme/eng": {{Login: "alice"}},
	})
	w := approveRequest(t, s, rr.stage.ID, "mallory", "approve")
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403:\n%s", w.Code, w.Body.String())
	}
	if len(apRepo.all) != 0 {
		t.Errorf("expected no approval recorded, got %d", len(apRepo.all))
	}
}

func TestApproval_RoleCheck_NoResolver_AllowsAll(t *testing.T) {
	stage := &run.Stage{
		ID:           uuid.New(),
		RunID:        uuid.New(),
		Type:         run.StageTypeImplement,
		ExecutorKind: run.ExecutorAgent,
		ExecutorRef:  "claude-code",
		State:        run.StageStateAwaitingApproval,
	}
	rr := &approvalGateRunRepo{stage: stage, runRow: &run.Run{ID: stage.RunID}}
	apRepo := newFakeApprovalRepo()
	s := New(Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      rr,
		ApprovalRepo: apRepo,
		AuditRepo:    newAuditFake(),
	})
	w := approveRequest(t, s, stage.ID, "anyone", "approve")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (no resolver = no role check):\n%s", w.Code, w.Body.String())
	}
	if len(apRepo.all) != 1 {
		t.Errorf("expected approval recorded")
	}
}

func TestApproval_RoleCheck_SpecFetchFailure_AllowsThrough(t *testing.T) {
	s, rr, gh, apRepo, _ := newRoleApprovalServer(t, nil)
	gh.getErr = errors.New("github down")
	w := approveRequest(t, s, rr.stage.ID, "alice", "approve")
	// Best-effort: spec fetch failure should NOT black-hole the
	// approval. The submission goes through.
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (best-effort):\n%s", w.Code, w.Body.String())
	}
	if len(apRepo.all) != 1 {
		t.Errorf("expected approval recorded")
	}
}

func TestApproval_RoleCheck_CaseInsensitive(t *testing.T) {
	s, rr, _, _, _ := newRoleApprovalServer(t, map[string][]role.TeamMember{
		"acme/eng": {{Login: "Alice"}},
	})
	w := approveRequest(t, s, rr.stage.ID, "alice", "approve")
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (case-fold should match)", w.Code)
	}
}

func TestApproval_RoleCheck_RejectAlsoChecked(t *testing.T) {
	// reject must satisfy the same role check as approve —
	// otherwise an attacker could reject other people's stages.
	s, rr, _, _, _ := newRoleApprovalServer(t, map[string][]role.TeamMember{
		"acme/eng": {{Login: "alice"}},
	})
	w := approveRequest(t, s, rr.stage.ID, "mallory", "reject")
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 (reject must enforce role)", w.Code)
	}
}
