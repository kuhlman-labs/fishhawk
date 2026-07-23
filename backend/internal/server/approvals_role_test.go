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

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
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
func (r *approvalGateRunRepo) RetryRun(context.Context, uuid.UUID, run.State) (*run.Run, error) {
	return nil, errors.New("not used")
}
func (r *approvalGateRunRepo) SetRunPullRequestURL(context.Context, uuid.UUID, string) (*run.Run, error) {
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
func (r *approvalGateRunRepo) ListReviewStagesAwaitingApproval(context.Context) ([]*run.Stage, error) {
	return nil, errors.New("not used")
}

func (r *approvalGateRunRepo) ListStagesAwaitingChildren(context.Context) ([]*run.Stage, error) {
	return nil, errors.New("not used")
}

func (r *approvalGateRunRepo) ListStagesDispatched(context.Context) ([]*run.Stage, error) {
	return nil, nil
}

func (r *approvalGateRunRepo) RetryStage(context.Context, uuid.UUID, run.StageState) (*run.Stage, error) {
	return nil, errors.New("not used")
}

// stubTeamLister satisfies role.TeamLister for the resolver in
// these tests. teamMembers is keyed "org/slug".
type stubTeamLister struct {
	teamMembers map[string][]role.TeamMember
}

func (s *stubTeamLister) ListTeamMembers(_ context.Context, _ forge.CredentialScope, org, slug string) ([]role.TeamMember, error) {
	if got, ok := s.teamMembers[org+"/"+slug]; ok {
		return got, nil
	}
	return nil, nil
}

const approvalGateSpec = `
version: "0.3"
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
		// Cache the spec on the run row (#283) so the approval
		// handler reads from storage instead of refetching from
		// GitHub.
		WorkflowSpec: []byte(approvalGateSpec),
	}
	rr := &approvalGateRunRepo{stage: stage, runRow: runRow}
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
	return s, rr, nil, apRepo, auditFake
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

func TestApproval_RoleCheck_NoCachedSpec_AllowsThrough(t *testing.T) {
	// Legacy run row (pre-#283 migration) has no cached spec bytes.
	// The role check falls open — best-effort posture — but the
	// approval still goes through. Pre-#283 this was hit by every
	// real-world approval because fetchGateForStage refetched the
	// spec from GitHub using a blob SHA as the ref → 404 → fall-
	// open → silent bypass of the role gate.
	s, rr, _, apRepo, _ := newRoleApprovalServer(t, nil)
	rr.runRow.WorkflowSpec = nil // simulate legacy row
	w := approveRequest(t, s, rr.stage.ID, "alice", "approve")
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

// deployAllOfSpec declares a delegating deploy stage whose approval gate uses
// all_of [eng_team, leads]: the approving subject must belong to EVERY named
// role (#1390 — the deploy gate routes enforcement through the existing
// checkApproverAuthorization -> RoleResolver.CanApprove path). No constraints
// so checkDeployPreflight passes on the admit case and the stage advances to
// dispatched.
const deployAllOfSpec = `
version: "1.0"
roles:
  eng_team:
    members: ["@acme/eng"]
  leads:
    members: ["@acme/leads"]
workflows:
  release:
    stages:
      - id: deploy
        type: deploy
        executor:
          delegate:
            target: github_actions
            workflow_ref: deploy.yml
        gates:
          - type: approval
            approvers:
              all_of: [eng_team, leads]
`

// newDeployRoleServer stands up a deploy stage parked at
// awaiting_deploy_approval whose gate carries the deployAllOfSpec all_of
// approvers, wired to a real role.Resolver over the supplied team membership.
func newDeployRoleServer(t *testing.T, members map[string][]role.TeamMember) (*Server, *approvalGateRunRepo, *fakeApprovalRepo) {
	t.Helper()
	stage := &run.Stage{
		ID:               uuid.New(),
		RunID:            uuid.New(),
		Type:             run.StageTypeDeploy,
		ExecutorKind:     run.ExecutorAgent,
		ExecutorRef:      "deploy",
		State:            run.StageStateAwaitingDeployApproval,
		RequiresApproval: true,
		CreatedAt:        time.Now().UTC(),
		UpdatedAt:        time.Now().UTC(),
	}
	installation := int64(99)
	runRow := &run.Run{
		ID:             stage.RunID,
		Repo:           "kuhlman-labs/example",
		WorkflowID:     "release",
		WorkflowSHA:    "abc123",
		InstallationID: &installation,
		WorkflowSpec:   []byte(deployAllOfSpec),
	}
	rr := &approvalGateRunRepo{stage: stage, runRow: runRow}
	resolver := role.NewResolver(&stubTeamLister{teamMembers: members})
	apRepo := newFakeApprovalRepo()
	s := New(Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      rr,
		ApprovalRepo: apRepo,
		AuditRepo:    newAuditFake(),
		RoleResolver: resolver,
	})
	return s, rr, apRepo
}

// (step 3) deploy all_of DENIES a subject missing one of the named roles: a
// member of eng_team but not leads cannot satisfy all_of, so the gate returns
// 403 approver_not_authorized and records no approval row — the deploy stage
// stays parked.
func TestApproval_DeployAllOf_DeniesSubjectMissingRole(t *testing.T) {
	s, rr, apRepo := newDeployRoleServer(t, map[string][]role.TeamMember{
		"acme/eng":   {{Login: "alice"}},
		"acme/leads": {{Login: "carol"}},
	})
	// alice is in eng_team but NOT leads → all_of unsatisfied.
	w := approveRequest(t, s, rr.stage.ID, "alice", "approve")
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403:\n%s", w.Code, w.Body.String())
	}
	if body := decodeErrorEnvelope(t, w); body.Code != "approver_not_authorized" {
		t.Errorf("error code = %q, want approver_not_authorized", body.Code)
	}
	if len(apRepo.all) != 0 {
		t.Errorf("expected no approval recorded on all_of denial, got %d", len(apRepo.all))
	}
	if rr.stage.State != run.StageStateAwaitingDeployApproval {
		t.Errorf("stage advanced to %q on a denied approve; want it parked", rr.stage.State)
	}
}

// (step 3) deploy all_of ADMITS a subject in EVERY named role: a member of both
// eng_team and leads satisfies all_of; the gate passes, checkDeployPreflight
// (no constraints) passes, and the deploy stage advances to dispatched. This is
// the cross-boundary seam: role resolver -> approval Submit -> run-repo
// transition in one request.
func TestApproval_DeployAllOf_AdmitsSubjectInAllRoles(t *testing.T) {
	s, rr, apRepo := newDeployRoleServer(t, map[string][]role.TeamMember{
		"acme/eng":   {{Login: "alice"}, {Login: "dana"}},
		"acme/leads": {{Login: "dana"}},
	})
	// dana is in BOTH eng_team and leads → all_of satisfied.
	w := approveRequest(t, s, rr.stage.ID, "dana", "approve")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	if len(apRepo.all) != 1 {
		t.Errorf("expected 1 approval recorded, got %d", len(apRepo.all))
	}
	if rr.stage.State != run.StageStateDispatched {
		t.Errorf("deploy stage state = %q, want dispatched", rr.stage.State)
	}
}

// GetRunAccountID satisfies the REQUIRED run.AccountGetter portion of
// run.Repository (E44.11 / #2074). Untenanted: this fake's runs carry no
// tenant account, matching its pre-promotion effective behavior.
func (*approvalGateRunRepo) GetRunAccountID(_ context.Context, _ uuid.UUID) (string, error) {
	return "", nil
}
