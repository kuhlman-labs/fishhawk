package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/gitlabclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// stubFileFetcher is a forge.FileFetcher whose FetchFile returns a fixed
// content/SHA pair, or a fixed error. It records what it was asked for so the
// tests can assert the spec was read at the GitLab project scope rather than
// through the GitHub client.
type stubFileFetcher struct {
	content []byte
	sha     string
	err     error

	calls     int
	gotScope  forge.CredentialScope
	gotRepo   forge.RepoRef
	gotPath   string
	gotRefArg string
}

func (s *stubFileFetcher) FetchFile(_ context.Context, scope forge.CredentialScope,
	repo forge.RepoRef, path, ref string) (*forge.FileContent, error) {
	s.calls++
	s.gotScope, s.gotRepo, s.gotPath, s.gotRefArg = scope, repo, path, ref
	if s.err != nil {
		return nil, s.err
	}
	return &forge.FileContent{Path: path, Content: s.content, SHA: s.sha}, nil
}

// stubGitLabProjects is a GitLabProjectAuthorizer that vouches for an explicit
// set of (credential_ref -> project_path) registrations and records every call.
//
// It deliberately has NO allow-everything mode: the guard binds a PAIR, so a
// fake that answered true for any input would let a test pass while the pair
// binding rotted.
type stubGitLabProjects struct {
	mu    sync.Mutex
	allow map[string]string
	err   error
	calls []stubGitLabAuthzCall
}

// unboundGitLabProject is the E45.26 / #2877 upgrade shape: the installation is
// registered but records no project path, so the authorizer returns the
// ErrGitLabProjectPathUnbound sentinel. It is a REFUSAL carried on an error
// value, which is exactly why the dispatcher must classify it ahead of the
// generic error arm.
func unboundGitLabProject() *stubGitLabProjects {
	return &stubGitLabProjects{err: ErrGitLabProjectPathUnbound}
}

type stubGitLabAuthzCall struct {
	Ref  string
	Path string
}

func (s *stubGitLabProjects) AuthorizedGitLabProject(_ context.Context, credentialRef, projectPath string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, stubGitLabAuthzCall{Ref: credentialRef, Path: projectPath})
	if s.err != nil {
		return false, s.err
	}
	registered, ok := s.allow[credentialRef]
	return ok && registered == projectPath, nil
}

// registeredGitLabProject vouches for exactly the project
// gitlabIssueTriggerEvent names, and for nothing else.
func registeredGitLabProject() *stubGitLabProjects {
	return &stubGitLabProjects{allow: map[string]string{"gitlab:4242": "acme/widgets"}}
}

// gitlabIssueTriggerEvent builds an admitted GitLab issue trigger: the
// fishhawk label present on an `open` action, with the repo path and the
// "gitlab:<project_id>" credential ref a real ParseGitLabEvent would stamp.
func gitlabIssueTriggerEvent() Event {
	ev := gitlabEvent("issue", "alice",
		`{"object_attributes":{"iid":7,"action":"open"},"labels":[{"title":"fishhawk"}]}`)
	ev.Repo = "acme/widgets"
	ev.DeliveryID = "gitlab:deliv-1"
	ev.CredentialRef = "gitlab:4242"
	return ev
}

// newGitLabDispatcher wires a dispatcher whose GitLab file reader serves the
// given spec. GitHub stays wired so a test can prove the GitLab path did NOT
// route through it.
func newGitLabDispatcher(t *testing.T, specYAML string) (*Dispatcher, *stubFileFetcher, *stubGitHub, *stubRuns, *stubAudit) {
	t.Helper()
	d, gh, runs, au := newDispatcherWithStubs(t)
	ff := &stubFileFetcher{content: []byte(specYAML), sha: "g1t1absha"}
	d.GitLabFiles = ff
	// The create path refuses any project not vouched for by a registry
	// (E45.22 / #2043 fix-up), so every test that expects a run to be minted
	// registers the event's project. Tests exercising the refusal replace or
	// clear this.
	d.GitLabProjects = registeredGitLabProject()
	return d, ff, gh, runs, au
}

// TestHandle_GitLabTrigger_CreatesGitLabCIRun is the done-means behavioural
// anchor for GitLab run creation (E45.22 / #2043). runner_kind and the
// installation_ref FORMAT are config-shaped values no compiler enforces, so
// this asserts the SHIPPED values on the created run rather than the presence
// of an edit.
func TestHandle_GitLabTrigger_CreatesGitLabCIRun(t *testing.T) {
	d, ff, gh, runs, au := newGitLabDispatcher(t, validSpec)

	if err := d.Handle(context.Background(), gitlabIssueTriggerEvent()); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if len(runs.created) != 1 {
		t.Fatalf("runs.created = %d, want 1", len(runs.created))
	}
	created := runs.created[0]
	if created.RunnerKind != run.RunnerKindGitLabCI {
		t.Errorf("runner_kind = %q, want %q", created.RunnerKind, run.RunnerKindGitLabCI)
	}
	// The ADR-045 lock is the runner's to set. A creation-time HINT must
	// leave runner_kind_resolved false so a later signed self-report can
	// still contradict it.
	if created.RunnerKindResolved {
		t.Error("runner_kind_resolved = true, want false (creation-time hint, not a lock)")
	}
	if created.InstallationRef == nil {
		t.Fatal("installation_ref = nil, want the gitlab: project ref")
	}
	if *created.InstallationRef != "gitlab:4242" {
		t.Errorf("installation_ref = %q, want %q", *created.InstallationRef, "gitlab:4242")
	}
	if created.InstallationID != nil {
		t.Errorf("installation_id = %v, want nil (a GitLab project has no GitHub App installation)",
			*created.InstallationID)
	}

	// The spec was read through the forge-neutral FileFetcher at the GitLab
	// project scope, NOT through the GitHub client (whose scope parse would
	// reject a "gitlab:" ref).
	if ff.calls != 1 {
		t.Errorf("FetchFile calls = %d, want 1", ff.calls)
	}
	if ff.gotScope.Ref() != "gitlab:4242" {
		t.Errorf("FetchFile scope ref = %q, want %q", ff.gotScope.Ref(), "gitlab:4242")
	}
	if ff.gotPath != gitLabWorkflowSpecPath {
		t.Errorf("FetchFile path = %q, want %q", ff.gotPath, gitLabWorkflowSpecPath)
	}
	if gh.specCalls != 0 {
		t.Errorf("GitHub GetWorkflowSpec calls = %d, want 0 (GitLab must not route through the GitHub client)", gh.specCalls)
	}
	if gh.dispatchCalls != 0 {
		t.Errorf("GitHub DispatchWorkflow calls = %d, want 0", gh.dispatchCalls)
	}

	// One stage row per spec stage, and the first stage moved to dispatched.
	if len(runs.createdStages) != 1 {
		t.Fatalf("created stages = %d, want 1", len(runs.createdStages))
	}
	if len(runs.transitions) != 1 || runs.transitions[0].To != run.StageStateDispatched {
		t.Errorf("transitions = %+v, want one to dispatched", runs.transitions)
	}

	if len(au.appended) != 1 {
		t.Fatalf("audit rows = %d, want 1", len(au.appended))
	}
	if au.appended[0].Category != "run_dispatched" {
		t.Errorf("audit category = %q, want run_dispatched", au.appended[0].Category)
	}
}

// TestHandle_GitLabTrigger_UnregisteredProject_PerformsNoForgeCall is the
// authorization control (E45.22 / #2043 fix-up, concern 403edbf3). The GitLab
// receiver authenticates on a shared X-Gitlab-Token with no HMAC over the body,
// so the project named in a payload is UNTRUSTED input that would otherwise
// steer a deployment-credentialed file read and a pipeline creation.
//
// Every cell asserts on what the FAKES RECORDED, not on a returned value: the
// handler returns nil for a refusal exactly as it does for a success, so a
// return-value assertion could not tell them apart. Zero FetchFile calls, zero
// CreateRun calls and zero CreateStage calls together mean no forge call
// happened — the pipeline trigger is reachable only after a run and its stages
// exist, so zero stages is proof no pipeline was created.
//
// The registered cell is the paired control: the SAME event admitted by the
// registry mints the run, so the refusals cannot be satisfied by a path that
// never creates anything.
func TestHandle_GitLabTrigger_UnregisteredProject_PerformsNoForgeCall(t *testing.T) {
	cases := []struct {
		name        string
		authorizer  GitLabProjectAuthorizer
		wantCreated int
		wantFetches int
		wantReason  string
	}{
		{
			// A project id nobody registered.
			name:       "unregistered_project_id",
			authorizer: &stubGitLabProjects{allow: map[string]string{"gitlab:9999": "acme/widgets"}},
			wantReason: gitLabAuthzNotRegistered,
		},
		{
			// The REGISTERED id paired with a DIFFERENT project path — the
			// spec-read selector aimed at another project. Both halves of the
			// pair must bind, so this is refused too.
			name:       "registered_id_foreign_project_path",
			authorizer: &stubGitLabProjects{allow: map[string]string{"gitlab:4242": "victim/other"}},
			wantReason: gitLabAuthzNotRegistered,
		},
		{
			// A registry fault must not open the gate.
			name:       "registry_lookup_fails",
			authorizer: &stubGitLabProjects{err: errors.New("connection reset")},
			wantReason: gitLabAuthzLookupFailed,
		},
		{
			// The installation IS registered but records no project path
			// (every row predating migration 0078). Refused with its OWN
			// reason, asserted against registry_lookup_fails and
			// unregistered_project_id above so the new reason is provably
			// DISTINGUISHABLE, not merely present: an operator who upgraded
			// without re-registering must get a remedy, not a generic fault.
			name:       "registered_project_path_unbound",
			authorizer: unboundGitLabProject(),
			wantReason: gitLabAuthzProjectPathUnbound,
		},
		{
			// A wrapped sentinel is classified the same way: the production
			// authorizer is free to annotate, and a bare == comparison would
			// silently regress this to the generic fault reason.
			name:       "registered_project_path_unbound_wrapped",
			authorizer: &stubGitLabProjects{err: fmt.Errorf("registry: %w", ErrGitLabProjectPathUnbound)},
			wantReason: gitLabAuthzProjectPathUnbound,
		},
		{
			// No registry wired at all: nothing can be shown authorized.
			name:       "registry_unwired",
			authorizer: nil,
			wantReason: gitLabAuthzRegistryUnwired,
		},
		{
			name:        "registered_project_admits",
			authorizer:  registeredGitLabProject(),
			wantCreated: 1,
			wantFetches: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, ff, gh, runs, au := newGitLabDispatcher(t, validSpec)
			d.GitLabProjects = tc.authorizer

			if err := d.Handle(context.Background(), gitlabIssueTriggerEvent()); err != nil {
				t.Fatalf("Handle: %v (a refusal is a 202, not a 5xx)", err)
			}

			if ff.calls != tc.wantFetches {
				t.Errorf("FetchFile calls = %d, want %d", ff.calls, tc.wantFetches)
			}
			if len(runs.created) != tc.wantCreated {
				t.Errorf("runs created = %d, want %d", len(runs.created), tc.wantCreated)
			}
			if len(runs.createdStages) != tc.wantCreated {
				t.Errorf("stages created = %d, want %d (no stages means no pipeline trigger)",
					len(runs.createdStages), tc.wantCreated)
			}
			if gh.specCalls != 0 || gh.dispatchCalls != 0 {
				t.Errorf("GitHub calls = %d spec / %d dispatch, want 0 / 0",
					gh.specCalls, gh.dispatchCalls)
			}
			if tc.wantReason == "" {
				return
			}
			// The refusal is RECORDED with its own reason, so an operator can
			// tell an unregistered project from a registry fault — and with the
			// gate that refused, so a create refusal is not confusable with the
			// CI-retry refusal that shares this category.
			reasons, paths := gitlabRefusalReasonsAndPaths(t, au)
			if len(reasons) != 1 || reasons[0] != tc.wantReason {
				t.Errorf("refusal reasons = %v, want [%s]", reasons, tc.wantReason)
			}
			if len(paths) != 1 || paths[0] != gitLabAuthzPathCreate {
				t.Errorf("refusal paths = %v, want [%s]", paths, gitLabAuthzPathCreate)
			}
		})
	}
}

// TestAuthorizeGitLabProject_CIRetryPathNamesTheUnboundReason is binding
// condition 3. The issue's gate covers TWO entry points — run creation and
// CI-failure retry — and a new refusal reason wired into only one of them would
// leave an unbound row auditing as the GENERIC lookup failure on the other,
// contradicting the criterion that the refusal carries a NAMED reason.
//
// Both entry points route through the single shared authorizeGitLabProject
// helper (gitlab_dispatch.go:205 for create, gitlab_ciretry.go:146 for retry),
// which owns the ONLY reason-selection branch. This drives that helper directly
// under the CI-retry path label and asserts the same named reason — so the
// claim is tested rather than asserted from the call graph.
func TestAuthorizeGitLabProject_CIRetryPathNamesTheUnboundReason(t *testing.T) {
	cases := []struct {
		name       string
		authorizer GitLabProjectAuthorizer
		wantReason string
	}{
		{"unbound_project_path", unboundGitLabProject(), gitLabAuthzProjectPathUnbound},
		// The paired control: a genuine fault on the SAME path still audits as
		// the generic lookup failure, so the unbound reason is provably
		// distinguishable on the retry path too, not a blanket relabel.
		{"registry_fault", &stubGitLabProjects{err: errors.New("connection reset")}, gitLabAuthzLookupFailed},
		{"unregistered", &stubGitLabProjects{allow: map[string]string{"gitlab:9999": "acme/widgets"}}, gitLabAuthzNotRegistered},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, _, _, _, au := newGitLabDispatcher(t, validSpec)
			d.GitLabProjects = tc.authorizer

			ev := gitlabIssueTriggerEvent()
			if d.authorizeGitLabProject(context.Background(), ev, Match{WorkflowID: "feature_change"},
				gitLabAuthzPathCIRetry, time.Now()) {
				t.Fatal("authorizeGitLabProject admitted, want refusal")
			}

			reasons, paths := gitlabRefusalReasonsAndPaths(t, au)
			if len(reasons) != 1 || reasons[0] != tc.wantReason {
				t.Errorf("refusal reasons = %v, want [%s]", reasons, tc.wantReason)
			}
			if len(paths) != 1 || paths[0] != gitLabAuthzPathCIRetry {
				t.Errorf("refusal paths = %v, want [%s]", paths, gitLabAuthzPathCIRetry)
			}
		})
	}
}

// TestHandle_GitLabTrigger_AuthorizationSeesThePayloadIdentity pins WHAT the
// gate is handed: the payload's credential ref and project path, unmodified.
// A gate consulted with the wrong values could vouch for the wrong project
// while every count-based assertion above stayed green.
func TestHandle_GitLabTrigger_AuthorizationSeesThePayloadIdentity(t *testing.T) {
	d, _, _, _, _ := newGitLabDispatcher(t, validSpec)
	authz := registeredGitLabProject()
	d.GitLabProjects = authz

	ev := gitlabIssueTriggerEvent()
	if err := d.Handle(context.Background(), ev); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(authz.calls) != 1 {
		t.Fatalf("authorization calls = %d, want 1", len(authz.calls))
	}
	if got := authz.calls[0]; got.Ref != ev.CredentialRef || got.Path != ev.Repo {
		t.Errorf("authorized (%q, %q), want (%q, %q)", got.Ref, got.Path, ev.CredentialRef, ev.Repo)
	}
}

// stubPipelineTrigger is a runnerbackend.PipelineTrigger that records every
// pipeline creation and can be made to fail. It is the seam that reaches the
// dispatch-FAILED branches of both GitLab paths.
type stubPipelineTrigger struct {
	mu    sync.Mutex
	err   error
	calls []stubPipelineCall
}

type stubPipelineCall struct {
	Ref   string
	Scope string
}

func (s *stubPipelineTrigger) TriggerPipeline(_ context.Context, scope forge.CredentialScope,
	ref string, _ []gitlabclient.PipelineVariable) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, stubPipelineCall{Ref: ref, Scope: scope.Ref()})
	return s.err
}

func (s *stubPipelineTrigger) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

// TestHandle_GitLabTrigger_PipelineTriggerFails_PersistsAndAuditsFailure pins
// the create path's dispatch-FAILED branch (concerns e57e8cd3 / b57c1fd3),
// which no prior test reached because every fixture let the pipeline trigger
// succeed.
//
// The contract under failure is deliberately NOT a rollback: the run and its
// stages are already persisted and must stay, because the operator's recovery
// verb acts on an existing run. What must NOT happen is the stage advancing to
// `dispatched` — a stage marked dispatched when nothing was dispatched is a run
// that waits forever on a pipeline that never existed. The audit records the
// failure so the discrepancy is visible.
//
// The trigger is a REACHABLE in-test fake returning a chosen error, not an
// unroutable endpoint: a connection error could map to the same outcome whether
// or not the branch handled it.
func TestHandle_GitLabTrigger_PipelineTriggerFails_PersistsAndAuditsFailure(t *testing.T) {
	d, _, _, runs, au := newGitLabDispatcher(t, validSpec)
	trigger := &stubPipelineTrigger{err: errors.New("gitlab: 500 internal error")}
	d.GitLabTrigger = trigger

	if err := d.Handle(context.Background(), gitlabIssueTriggerEvent()); err != nil {
		t.Fatalf("Handle: %v, want nil (a failed trigger is recorded, not redelivered)", err)
	}

	if trigger.callCount() != 1 {
		t.Fatalf("pipeline trigger calls = %d, want 1", trigger.callCount())
	}
	// The run and its stage persist — recovery acts on them.
	if len(runs.created) != 1 {
		t.Errorf("runs created = %d, want 1 (a failed dispatch does not unwind the run)", len(runs.created))
	}
	if len(runs.createdStages) != 1 {
		t.Errorf("stages created = %d, want 1", len(runs.createdStages))
	}
	// But the stage was NOT advanced. Both halves are asserted: that no
	// transition was ATTEMPTED, and that the persisted stage row still READS
	// pending. The second is the one that matters to a recovery verb — it acts
	// on stage state, not on the transition log — and the stub mutates the row
	// on transition, so a row left at pending is a genuine state assertion
	// rather than a restatement of the line above.
	if len(runs.transitions) != 0 {
		t.Errorf("transitions = %+v, want none (nothing was dispatched)", runs.transitions)
	}
	runs.mu.Lock()
	stages := append([]*run.Stage(nil), runs.createdStages...)
	runs.mu.Unlock()
	for _, st := range stages {
		if st.RunID == runs.created[0].ID && st.State != run.StageStatePending {
			t.Errorf("stage %s state = %q, want %q; a stage marked dispatched when no pipeline "+
				"was created leaves the run waiting forever", st.ID, st.State, run.StageStatePending)
		}
	}
	// And the audit says so.
	if cats := auditCategories(au); len(cats) != 1 || cats[0] != "run_dispatched" {
		t.Fatalf("audit = %v, want one run_dispatched row recording the outcome", cats)
	}
	au.mu.Lock()
	payload := au.appended[0].Payload
	au.mu.Unlock()
	var p map[string]any
	if err := json.Unmarshal(payload, &p); err != nil {
		t.Fatalf("unmarshal run_dispatched payload: %v", err)
	}
	if got, _ := p["outcome"].(string); got != "dispatch_failed" {
		t.Errorf("audit outcome = %q, want dispatch_failed; a failed trigger must not read as a success", got)
	}
	if got, _ := p["error"].(string); !strings.Contains(got, "500 internal error") {
		t.Errorf("audit error = %q, want it to carry the trigger failure", got)
	}
}

// gitlabRefusalReasons returns the `reason` of every global-chained
// run_rejected_misconfigured entry written by the GitLab authorization gate.
func gitlabRefusalReasons(t *testing.T, au *stubAudit) []string {
	t.Helper()
	reasons, _ := gitlabRefusalReasonsAndPaths(t, au)
	return reasons
}

// gitlabRefusalReasonsAndPaths returns the `reason` AND the `path` of every
// global-chained run_rejected_misconfigured entry the GitLab authorization gate
// wrote. Both GitLab entry points share one authorizer and one category, so
// `path` is what distinguishes a refused run creation from a refused CI retry.
func gitlabRefusalReasonsAndPaths(t *testing.T, au *stubAudit) (reasons, paths []string) {
	t.Helper()
	au.mu.Lock()
	defer au.mu.Unlock()
	for _, a := range au.globalAppended {
		if a.Category != "run_rejected_misconfigured" {
			continue
		}
		var p map[string]any
		if err := json.Unmarshal(a.Payload, &p); err != nil {
			t.Fatalf("unmarshal run_rejected_misconfigured payload: %v", err)
		}
		if reason, ok := p["reason"].(string); ok {
			reasons = append(reasons, reason)
		}
		if path, ok := p["path"].(string); ok {
			paths = append(paths, path)
		}
	}
	return reasons, paths
}

// TestHandle_GitLabTrigger_NoFileReader_SkipsWithoutCreating pins the
// fail-closed branch for an unconfigured GitLab file reader: a deployment
// misconfiguration must not mint a run against a spec that was never read,
// and must not 5xx into a redelivery loop either.
func TestHandle_GitLabTrigger_NoFileReader_SkipsWithoutCreating(t *testing.T) {
	d, _, runs, au := newDispatcherWithStubs(t)
	d.GitLabFiles = nil
	d.GitLabProjects = registeredGitLabProject()

	if err := d.Handle(context.Background(), gitlabIssueTriggerEvent()); err != nil {
		t.Fatalf("Handle: %v (want nil, not a 5xx)", err)
	}
	if len(runs.created) != 0 {
		t.Errorf("runs.created = %d, want 0", len(runs.created))
	}
	if len(au.appended) != 0 {
		t.Errorf("audit rows = %d, want 0", len(au.appended))
	}
}

// TestHandle_GitLabTrigger_SpecReadFailures covers the two non-transient
// spec-read refusals (forbidden / not-found -> skip) and the transient one
// (any other error -> surfaced so the forge redelivers).
func TestHandle_GitLabTrigger_SpecReadFailures(t *testing.T) {
	cases := []struct {
		name      string
		err       error
		wantErr   bool
		wantCount int
	}{
		{"not_found_skips", forge.ErrNotFound, false, 0},
		{"forbidden_skips", forge.ErrForbidden, false, 0},
		{"transient_surfaces", errors.New("dial tcp: connection refused"), true, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, _, _, runs, _ := newGitLabDispatcher(t, validSpec)
			d.GitLabFiles = &stubFileFetcher{err: tc.err}

			err := d.Handle(context.Background(), gitlabIssueTriggerEvent())
			if tc.wantErr && err == nil {
				t.Fatal("Handle returned nil; want the transient error surfaced")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("Handle: %v, want nil", err)
			}
			if len(runs.created) != tc.wantCount {
				t.Errorf("runs.created = %d, want %d", len(runs.created), tc.wantCount)
			}
		})
	}
}

// TestHandle_GitLabTrigger_MalformedRepo_Skips pins the repo-parse guard: a
// payload with no namespaced path cannot address a project, so the run is
// refused before any forge call.
func TestHandle_GitLabTrigger_MalformedRepo_Skips(t *testing.T) {
	d, ff, _, runs, _ := newGitLabDispatcher(t, validSpec)
	ev := gitlabIssueTriggerEvent()
	ev.Repo = "widgets" // no namespace
	// Register the malformed path so the authorization gate admits it and the
	// refusal under test is the repo PARSE guard, not the new authz guard.
	d.GitLabProjects = &stubGitLabProjects{allow: map[string]string{"gitlab:4242": "widgets"}}

	if err := d.Handle(context.Background(), ev); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(runs.created) != 0 {
		t.Errorf("runs.created = %d, want 0", len(runs.created))
	}
	if ff.calls != 0 {
		t.Errorf("FetchFile calls = %d, want 0 (refused before the forge call)", ff.calls)
	}
}

// TestHandle_GitLabTrigger_SpecRejections pins the three spec-shape refusals
// on the GitLab path, each of which must write the spec-rejection audit and
// create no run.
func TestHandle_GitLabTrigger_SpecRejections(t *testing.T) {
	cases := []struct {
		name string
		spec string
	}{
		{"malformed_yaml", "version: \"0.3\"\nworkflows: [oops"},
		{"workflow_id_absent", "version: \"0.3\"\nworkflows:\n  other:\n    description: x\n    stages:\n      - id: implement\n        type: implement\n        executor:\n          agent: claude-code\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, _, _, runs, au := newGitLabDispatcher(t, tc.spec)

			if err := d.Handle(context.Background(), gitlabIssueTriggerEvent()); err != nil {
				t.Fatalf("Handle: %v", err)
			}
			if len(runs.created) != 0 {
				t.Errorf("runs.created = %d, want 0", len(runs.created))
			}
			// A spec rejection is refused BEFORE any run row exists, so
			// its surface is the structured WARN log (shared with the
			// GitHub path's writeSpecRejectionAudit); no chained audit
			// row is written, and none should be.
			if len(au.appended) != 0 || len(au.globalAppended) != 0 {
				t.Errorf("audit rows = %d chained / %d global, want 0 / 0",
					len(au.appended), len(au.globalAppended))
			}
		})
	}
}

// TestHandle_GitLabTrigger_PlanReviewerGuard pins the COARSE plan-reviewer
// capability gate on the GitLab creation path — the symmetric counterpart to
// the GitHub path's guard (#577 / ADR-027). A gating plan stage (agent > 0,
// human == 0) on a deployment with no review backend at all can never satisfy
// its gate, so the run is refused before it is minted. Advisory mode (both
// > 0) and a configured reviewer both admit, so the refusal cannot be
// satisfied by a path that never creates runs.
func TestHandle_GitLabTrigger_PlanReviewerGuard(t *testing.T) {
	cases := []struct {
		name        string
		agent       int
		human       int
		configured  bool
		wantCreated int
	}{
		{"gating_unconfigured_refuses", 1, 0, false, 0},
		{"gating_configured_admits", 1, 0, true, 1},
		{"advisory_unconfigured_admits", 1, 1, false, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, _, _, runs, au := newGitLabDispatcher(t, specWithReviewers(tc.agent, tc.human))
			d.PlanReviewerConfigured = tc.configured

			if err := d.Handle(context.Background(), gitlabIssueTriggerEvent()); err != nil {
				t.Fatalf("Handle: %v", err)
			}
			if len(runs.created) != tc.wantCreated {
				t.Errorf("runs created = %d, want %d", len(runs.created), tc.wantCreated)
			}
			wantRefusals := 0
			if tc.wantCreated == 0 {
				wantRefusals = 1
			}
			if n := countGlobalCategory(au, "run_rejected_misconfigured"); n != wantRefusals {
				t.Errorf("run_rejected_misconfigured entries = %d, want %d", n, wantRefusals)
			}
		})
	}
}

// TestHandle_GitLabTrigger_PersistenceFailuresSurface pins the two
// infrastructure faults on the create path. Neither is a policy refusal, so
// both must surface as an error the receiver turns into a 5xx and the forge
// redelivers — not be swallowed into a silent no-run.
func TestHandle_GitLabTrigger_PersistenceFailuresSurface(t *testing.T) {
	cases := []struct {
		name    string
		inject  func(*stubRuns)
		wantSub string
	}{
		{"create_run_fails", func(r *stubRuns) { r.createErr = errors.New("run insert failed") }, "run insert failed"},
		{"create_stages_fails", func(r *stubRuns) { r.createStageErr = errors.New("stage insert failed") }, "stage insert failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, _, _, runs, _ := newGitLabDispatcher(t, validSpec)
			tc.inject(runs)

			err := d.Handle(context.Background(), gitlabIssueTriggerEvent())
			if err == nil {
				t.Fatal("Handle returned nil; want the persistence fault surfaced")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error = %v, want it to carry %q", err, tc.wantSub)
			}
		})
	}
}

// TestHandle_GitLabTrigger_TransitionFails_StillAudits pins the best-effort
// stage transition: a transition fault logs but must NOT unwind the dispatch
// — the run and its stages already exist, and the run_dispatched audit row is
// the record of what happened.
func TestHandle_GitLabTrigger_TransitionFails_StillAudits(t *testing.T) {
	d, _, _, runs, au := newGitLabDispatcher(t, validSpec)
	runs.transitionErr = errors.New("row locked")

	if err := d.Handle(context.Background(), gitlabIssueTriggerEvent()); err != nil {
		t.Fatalf("Handle: %v, want nil (transition is best-effort)", err)
	}
	if len(runs.created) != 1 {
		t.Fatalf("runs created = %d, want 1", len(runs.created))
	}
	if len(au.appended) != 1 || au.appended[0].Category != "run_dispatched" {
		t.Errorf("audit rows = %+v, want one run_dispatched", au.appended)
	}
}
