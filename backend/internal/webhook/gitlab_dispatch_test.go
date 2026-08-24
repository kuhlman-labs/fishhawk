package webhook

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
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

// TestHandle_GitLabTrigger_NoFileReader_SkipsWithoutCreating pins the
// fail-closed branch for an unconfigured GitLab file reader: a deployment
// misconfiguration must not mint a run against a spec that was never read,
// and must not 5xx into a redelivery loop either.
func TestHandle_GitLabTrigger_NoFileReader_SkipsWithoutCreating(t *testing.T) {
	d, _, runs, au := newDispatcherWithStubs(t)
	d.GitLabFiles = nil

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
