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

	"github.com/kuhlman-labs/fishhawk/backend/internal/defaultbranch"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// Cross-boundary tests for the resolved default branch on the server's
// read and deploy surfaces (#4660, slice B). Every trunk case drives the
// PRODUCTION chain — handler → resolveRunDefaultBranch → defaultbranch.Resolver
// (the githubclient's shared instance) → the REAL githubclient.Client → an
// httptest forge — over the non-Fishhawk fixture repository acme/widgets whose
// forge default branch is `trunk` (ADR-110 3(a)), never defaultbranch.Static.

// surfaceForge is an httptest api.github.com that answers GET
// /repos/{owner}/{repo} with a configurable default_branch (or status), the
// workflow_dispatch POST (recording the dispatched ref), and the actions/runs
// list the dispatched-run resolver reads. Counters are mutex-guarded because
// the handler runs on the server goroutine and the suite runs under -race.
type surfaceForge struct {
	mu            sync.Mutex
	defaultBranch string
	repoStatus    int
	repoGets      int
	dispatches    int
	dispatchRefs  []string
}

func newSurfaceForge(t *testing.T, defaultBranch string, repoStatus int) (*surfaceForge, *githubclient.Client) {
	t.Helper()
	f := &surfaceForge{defaultBranch: defaultBranch, repoStatus: repoStatus}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{repo}", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		f.repoGets++
		status, branch := f.repoStatus, f.defaultBranch
		f.mu.Unlock()
		if status != http.StatusOK {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"message":"refused"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"full_name":"acme/widgets","default_branch":%q}`, branch)
	})
	mux.HandleFunc("POST /repos/{owner}/{repo}/actions/workflows/{file}/dispatches",
		func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				Ref string `json:"ref"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.mu.Lock()
			f.dispatches++
			f.dispatchRefs = append(f.dispatchRefs, body.Ref)
			f.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		})
	mux.HandleFunc("GET /repos/{owner}/{repo}/actions/runs", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"workflow_runs":[]}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return f, &githubclient.Client{
		BaseURL: srv.URL,
		Tokens:  &fakeTokenProvider{tok: "ghs_t"},
		HTTP:    &http.Client{Timeout: 5 * time.Second},
	}
}

func (f *surfaceForge) counts() (repoGets, dispatches int, refs []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.repoGets, f.dispatches, append([]string(nil), f.dispatchRefs...)
}

// getRunDefaultBranchFields drives GET /v0/runs/{id} through the full HTTP
// handler and returns the two wire fields plus whether each KEY was present.
func getRunDefaultBranchFields(t *testing.T, s *Server, runID uuid.UUID) (branch, reason string, hasBranch, hasReason bool) {
	t.Helper()
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v0/runs/"+runID.String(), nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET run status = %d, want 200 (a default-branch read failure must never fail the GET):\n%s", w.Code, w.Body.String())
	}
	var raw map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("unmarshal GET run: %v", err)
	}
	b, hasBranch := raw["default_branch"]
	r, hasReason := raw["default_branch_unresolved_reason"]
	branch, _ = b.(string)
	reason, _ = r.(string)
	return branch, reason, hasBranch, hasReason
}

// seedSurfaceRun creates a run on the fake repo and stamps the forge identity
// fields directly (the fake returns the stored pointer).
func seedSurfaceRun(t *testing.T, repo *fakeRepo, repoName string, inst *int64, instRef *string) *run.Run {
	t.Helper()
	got, err := repo.CreateRun(context.Background(), run.CreateRunParams{
		Repo: repoName, WorkflowID: "w", WorkflowSHA: "s", TriggerSource: run.TriggerCLI,
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	got.InstallationID = inst
	got.InstallationRef = instRef
	return got
}

// TestDefaultBranch_GetRunReportsTrunkThroughForge is the cross-boundary e2e:
// HTTP → handleGetRun → resolveRunDefaultBranch → defaultbranch.Resolver →
// real githubclient.Client → httptest forge. Two GETs both report the forge's
// trunk and the forge saw ONE repository read (the shared resolver's cache).
func TestDefaultBranch_GetRunReportsTrunkThroughForge(t *testing.T) {
	forgeSrv, gh := newSurfaceForge(t, "trunk", http.StatusOK)
	repo := newFakeRepo()
	s := newServer(t, repo)
	s.cfg.GitHub = gh
	s.cfg.DefaultBranch = gh.DefaultBranches()
	seeded := seedSurfaceRun(t, repo, "acme/widgets", instID(4242), nil)

	for i := 0; i < 2; i++ {
		branch, reason, hasBranch, hasReason := getRunDefaultBranchFields(t, s, seeded.ID)
		if !hasBranch || branch != "trunk" {
			t.Fatalf("GET #%d default_branch = %q (present=%v), want trunk", i+1, branch, hasBranch)
		}
		if hasReason {
			t.Fatalf("GET #%d default_branch_unresolved_reason present (%q) beside a resolved branch; exactly one must be set", i+1, reason)
		}
	}
	if gets, _, _ := forgeSrv.counts(); gets != 1 {
		t.Errorf("forge GET /repos/acme/widgets count = %d, want 1 (the second read must hit the shared cache)", gets)
	}
}

// plainErrSource is a defaultbranch.Source that returns an UNTYPED error, so
// handleGetRun's fallback (an error carrying no Reason reports lookup_failed)
// is exercised in isolation.
type plainErrSource struct{}

func (plainErrSource) Resolve(context.Context, forge.CredentialScope, forge.RepoRef) (string, error) {
	return "", errors.New("untyped resolver failure")
}

// TestDefaultBranch_GetRunReasonTable asserts each unresolved mode reports
// default_branch ABSENT and the exact named reason, with the GET still 200.
func TestDefaultBranch_GetRunReasonTable(t *testing.T) {
	gitlabRef := "gitlab:77"
	cases := []struct {
		name      string
		repoName  string
		inst      *int64
		instRef   *string
		source    func(gh *githubclient.Client) defaultbranch.Source
		status    int
		want      defaultbranch.Reason
		wantReads int
	}{
		{name: "nil resolver", repoName: "acme/widgets", inst: instID(1),
			source: func(*githubclient.Client) defaultbranch.Source { return nil },
			status: http.StatusOK, want: defaultbranch.ReasonResolverUnconfigured},
		{name: "gitlab run", repoName: "acme/widgets", instRef: &gitlabRef,
			source: func(gh *githubclient.Client) defaultbranch.Source { return gh.DefaultBranches() },
			status: http.StatusOK, want: defaultbranch.ReasonForgeUnsupported},
		{name: "no installation", repoName: "acme/widgets",
			source: func(gh *githubclient.Client) defaultbranch.Source { return gh.DefaultBranches() },
			status: http.StatusOK, want: defaultbranch.ReasonNoInstallation},
		{name: "unparseable repo", repoName: "widgets", inst: instID(1),
			source: func(gh *githubclient.Client) defaultbranch.Source { return gh.DefaultBranches() },
			status: http.StatusOK, want: defaultbranch.ReasonRepositoryInvalid},
		{name: "forge 404", repoName: "acme/widgets", inst: instID(1),
			source: func(gh *githubclient.Client) defaultbranch.Source { return gh.DefaultBranches() },
			status: http.StatusNotFound, want: defaultbranch.ReasonRepositoryNotVisible, wantReads: 1},
		{name: "forge 403", repoName: "acme/widgets", inst: instID(1),
			source: func(gh *githubclient.Client) defaultbranch.Source { return gh.DefaultBranches() },
			status: http.StatusForbidden, want: defaultbranch.ReasonForbidden, wantReads: 1},
		{name: "getter reports an empty branch", repoName: "acme/widgets", inst: instID(1),
			source: func(*githubclient.Client) defaultbranch.Source {
				return defaultbranch.New(&repoGetterFake{branch: "   "})
			},
			status: http.StatusOK, want: defaultbranch.ReasonDefaultBranchMissing},
		{name: "untyped source error", repoName: "acme/widgets", inst: instID(1),
			source: func(*githubclient.Client) defaultbranch.Source { return plainErrSource{} },
			status: http.StatusOK, want: defaultbranch.ReasonLookupFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			forgeSrv, gh := newSurfaceForge(t, "trunk", tc.status)
			repo := newFakeRepo()
			s := newServer(t, repo)
			s.cfg.GitHub = gh
			s.cfg.DefaultBranch = tc.source(gh)
			seeded := seedSurfaceRun(t, repo, tc.repoName, tc.inst, tc.instRef)

			branch, reason, hasBranch, hasReason := getRunDefaultBranchFields(t, s, seeded.ID)
			if hasBranch {
				t.Errorf("default_branch present (%q) on an unresolved read; want absent", branch)
			}
			if !hasReason || reason != string(tc.want) {
				t.Errorf("default_branch_unresolved_reason = %q (present=%v), want %q", reason, hasReason, tc.want)
			}
			if gets, _, _ := forgeSrv.counts(); gets != tc.wantReads {
				t.Errorf("forge repo reads = %d, want %d", gets, tc.wantReads)
			}
		})
	}
}

// TestDefaultBranch_ListRunsCarriesNeitherField pins the list endpoint's
// no-per-row-forge-read posture: neither key appears and the forge is never
// read, even with a resolver wired.
func TestDefaultBranch_ListRunsCarriesNeitherField(t *testing.T) {
	forgeSrv, gh := newSurfaceForge(t, "trunk", http.StatusOK)
	repo := newFakeRepo()
	s := newServer(t, repo)
	s.cfg.GitHub = gh
	s.cfg.DefaultBranch = gh.DefaultBranches()
	seedSurfaceRun(t, repo, "acme/widgets", instID(4242), nil)

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v0/runs?repo=acme/widgets", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("list status = %d:\n%s", w.Code, w.Body.String())
	}
	var got struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal list: %v", err)
	}
	if len(got.Items) != 1 {
		t.Fatalf("list items = %d, want 1", len(got.Items))
	}
	for _, key := range []string{"default_branch", "default_branch_unresolved_reason"} {
		if _, has := got.Items[0][key]; has {
			t.Errorf("list item carries %q; the list endpoint must not pay a per-row forge read", key)
		}
	}
	if gets, _, _ := forgeSrv.counts(); gets != 0 {
		t.Errorf("forge repo reads on the list endpoint = %d, want 0", gets)
	}
}

// seedSurfaceDeploy seeds a dispatched deploy stage on acme/widgets for the
// deploy-trigger tests.
func seedSurfaceDeploy(rr *approvalRunRepo, specYAML string) (*run.Stage, *run.Run) {
	stage, runRow := seedDispatchedDeploy(rr, specYAML, instID(4242))
	runRow.Repo = "acme/widgets"
	return stage, runRow
}

// deploySpecPinnedGitRef pins executor.delegate.git_ref, so neither the
// trigger nor the rollback may read the forge.
const deploySpecPinnedGitRef = `
version: "1.0"
workflows:
  release:
    stages:
      - id: deploy
        type: deploy
        executor:
          delegate:
            target: github_actions
            workflow_ref: deploy.yml
            git_ref: release-2026
`

func TestDefaultBranch_DeployTriggerDispatchesOnTrunk(t *testing.T) {
	s, _, rr, au := newApprovalServer(t)
	forgeSrv, gh := newSurfaceForge(t, "trunk", http.StatusOK)
	s.cfg.GitHub = gh
	s.cfg.DefaultBranch = gh.DefaultBranches()
	stage, _ := seedSurfaceDeploy(rr, deploySpecNoConstraints)

	got, err := s.triggerDeploy(context.Background(), stage)
	if err != nil {
		t.Fatalf("triggerDeploy: %v", err)
	}
	if got.State != run.StageStateAwaitingDeployment {
		t.Fatalf("stage state = %q, want awaiting_deployment", got.State)
	}
	_, dispatches, refs := forgeSrv.counts()
	if dispatches != 1 || len(refs) != 1 || refs[0] != "trunk" {
		t.Fatalf("workflow_dispatch refs = %v (count %d), want exactly [trunk]", refs, dispatches)
	}
	if p := auditPayload(t, au, CategoryDeploymentDispatched); p["git_ref"] != "trunk" {
		t.Errorf("deployment_dispatched git_ref = %v, want trunk", p["git_ref"])
	}
}

func TestDefaultBranch_DeployTriggerPinnedGitRefNeverReadsForge(t *testing.T) {
	s, _, rr, au := newApprovalServer(t)
	forgeSrv, gh := newSurfaceForge(t, "trunk", http.StatusOK)
	s.cfg.GitHub = gh
	s.cfg.DefaultBranch = nil // a pinned git_ref needs no resolver at all
	stage, _ := seedSurfaceDeploy(rr, deploySpecPinnedGitRef)

	got, err := s.triggerDeploy(context.Background(), stage)
	if err != nil {
		t.Fatalf("triggerDeploy: %v", err)
	}
	if got.State != run.StageStateAwaitingDeployment {
		t.Fatalf("stage state = %q, want awaiting_deployment", got.State)
	}
	gets, _, refs := forgeSrv.counts()
	if len(refs) != 1 || refs[0] != "release-2026" {
		t.Fatalf("workflow_dispatch refs = %v, want [release-2026]", refs)
	}
	if gets != 0 {
		t.Errorf("forge repo reads = %d, want 0 for a pinned git_ref", gets)
	}
	if p := auditPayload(t, au, CategoryDeploymentDispatched); p["git_ref"] != "release-2026" {
		t.Errorf("deployment_dispatched git_ref = %v, want release-2026", p["git_ref"])
	}
}

func TestDefaultBranch_DeployTriggerUnresolvedFailsStage(t *testing.T) {
	s, _, rr, au := newApprovalServer(t)
	forgeSrv, gh := newSurfaceForge(t, "trunk", http.StatusNotFound)
	s.cfg.GitHub = gh
	s.cfg.DefaultBranch = gh.DefaultBranches()
	stage, _ := seedSurfaceDeploy(rr, deploySpecNoConstraints)

	got, err := s.triggerDeploy(context.Background(), stage)
	if err != nil {
		t.Fatalf("triggerDeploy: %v", err)
	}
	if got.State != run.StageStateFailed {
		t.Fatalf("stage state = %q, want failed", got.State)
	}
	if got.FailureCategory == nil || *got.FailureCategory != run.FailureC {
		t.Errorf("failure category = %v, want C", got.FailureCategory)
	}
	if _, dispatches, _ := forgeSrv.counts(); dispatches != 0 {
		t.Errorf("workflow_dispatch requests = %d, want 0 on an unresolved default branch", dispatches)
	}
	if n := countAppendedCategory(au, CategoryDeploymentDispatched); n != 0 {
		t.Errorf("deployment_dispatched rows = %d, want 0", n)
	}
	if n := countAppendedCategory(au, categoryDeploymentDispatchFailed); n != 1 {
		t.Errorf("deployment_dispatch_failed rows = %d, want exactly 1", n)
	}
	p := auditPayload(t, au, categoryDeploymentDispatchFailed)
	if p["default_branch_unresolved_reason"] != string(defaultbranch.ReasonRepositoryNotVisible) {
		t.Errorf("default_branch_unresolved_reason = %v, want repository_not_visible", p["default_branch_unresolved_reason"])
	}
	msg, _ := p["reason"].(string)
	if !strings.Contains(msg, "repository_not_visible") || !strings.Contains(msg, "executor.delegate.git_ref") {
		t.Errorf("failure reason %q must name the reason and the git_ref remedy", msg)
	}
}

func TestDefaultBranch_RollbackDispatchesOnTrunk(t *testing.T) {
	s, _, rr, au := newApprovalServer(t)
	forgeSrv, gh := newSurfaceForge(t, "trunk", http.StatusOK)
	s.cfg.GitHub = gh
	s.cfg.DefaultBranch = gh.DefaultBranches()
	stage, runRow := seedSettledDeployRun(rr, deploySpecNoConstraints, run.StageStateSucceeded, instID(4242))
	runRow.Repo = "acme/widgets"

	w := rollbackRequest(t, s, stage.RunID, nil)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202:\n%s", w.Code, w.Body.String())
	}
	if _, _, refs := forgeSrv.counts(); len(refs) != 1 || refs[0] != "trunk" {
		t.Fatalf("rollback workflow_dispatch refs = %v, want [trunk]", refs)
	}
	if p := auditPayload(t, au, CategoryDeploymentRollbackInitiated); p["git_ref"] != "trunk" {
		t.Errorf("deployment_rollback_initiated git_ref = %v, want the dispatched ref trunk", p["git_ref"])
	}
}

func TestDefaultBranch_RollbackPinnedGitRefNeverReadsForge(t *testing.T) {
	s, _, rr, au := newApprovalServer(t)
	forgeSrv, gh := newSurfaceForge(t, "trunk", http.StatusOK)
	s.cfg.GitHub = gh
	s.cfg.DefaultBranch = nil
	stage, runRow := seedSettledDeployRun(rr, deploySpecPinnedGitRef, run.StageStateSucceeded, instID(4242))
	runRow.Repo = "acme/widgets"

	w := rollbackRequest(t, s, stage.RunID, nil)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202:\n%s", w.Code, w.Body.String())
	}
	gets, _, refs := forgeSrv.counts()
	if len(refs) != 1 || refs[0] != "release-2026" || gets != 0 {
		t.Fatalf("refs = %v, repo reads = %d; want [release-2026] and 0 reads", refs, gets)
	}
	if p := auditPayload(t, au, CategoryDeploymentRollbackInitiated); p["git_ref"] != "release-2026" {
		t.Errorf("deployment_rollback_initiated git_ref = %v, want release-2026", p["git_ref"])
	}
}

func TestDefaultBranch_RollbackUnresolvedRefuses(t *testing.T) {
	s, _, rr, au := newApprovalServer(t)
	forgeSrv, gh := newSurfaceForge(t, "trunk", http.StatusForbidden)
	s.cfg.GitHub = gh
	s.cfg.DefaultBranch = gh.DefaultBranches()
	stage, runRow := seedSettledDeployRun(rr, deploySpecNoConstraints, run.StageStateSucceeded, instID(4242))
	runRow.Repo = "acme/widgets"

	w := rollbackRequest(t, s, stage.RunID, nil)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502:\n%s", w.Code, w.Body.String())
	}
	var body struct {
		Error struct {
			Code    string         `json:"code"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Error.Code != "rollback_dispatch_failed" {
		t.Errorf("code = %q, want rollback_dispatch_failed", body.Error.Code)
	}
	if body.Error.Details["reason"] != "default_branch_unresolved" ||
		body.Error.Details["default_branch_unresolved_reason"] != string(defaultbranch.ReasonForbidden) {
		t.Errorf("details = %v, want reason default_branch_unresolved + forbidden", body.Error.Details)
	}
	if _, dispatches, _ := forgeSrv.counts(); dispatches != 0 {
		t.Errorf("rollback workflow_dispatch requests = %d, want 0", dispatches)
	}
	if n := countAppendedCategory(au, CategoryDeploymentRollbackInitiated); n != 0 {
		t.Errorf("deployment_rollback_initiated rows = %d, want 0 on a refused rollback", n)
	}
}

// TestDefaultBranch_WebhookRollbackRecordsPinnedRefOnly: a webhook rollback
// dispatches no ref, so it never reads the forge and records the delegate's
// git_ref verbatim — empty here, since a webhook delegate pins none.
func TestDefaultBranch_WebhookRollbackRecordsPinnedRefOnly(t *testing.T) {
	s, _, rr, au := newApprovalServer(t)
	forgeSrv, gh := newSurfaceForge(t, "trunk", http.StatusOK)
	s.cfg.GitHub = gh
	s.cfg.DefaultBranch = gh.DefaultBranches()
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(hook.Close)
	stage, runRow := seedSettledDeployRun(rr, fmt.Sprintf(deploySpecWebhookFmt, hook.URL), run.StageStateSucceeded, instID(4242))
	runRow.Repo = "acme/widgets"

	w := rollbackRequest(t, s, stage.RunID, nil)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202:\n%s", w.Code, w.Body.String())
	}
	if gets, _, _ := forgeSrv.counts(); gets != 0 {
		t.Errorf("forge repo reads = %d, want 0 for a webhook rollback", gets)
	}
	p := auditPayload(t, au, CategoryDeploymentRollbackInitiated)
	if p["git_ref"] != "" {
		t.Errorf("webhook deployment_rollback_initiated git_ref = %v, want empty (no guessed branch)", p["git_ref"])
	}
}
