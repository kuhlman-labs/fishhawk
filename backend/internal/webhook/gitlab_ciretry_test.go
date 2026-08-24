package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/orchestrator"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// --- fixtures ------------------------------------------------------------

// gitlabPipelineEvent builds a GitLab Pipeline Hook delivery. ref / sha are
// the correlation inputs; iid is the merge-request narrowing hint (0 omits
// the merge_request block, as a branch pipeline's payload does).
func gitlabPipelineEvent(status, ref, sha string, pipelineID, iid int) Event {
	body := map[string]any{
		"object_kind": "pipeline",
		"object_attributes": map[string]any{
			"id":     pipelineID,
			"ref":    ref,
			"sha":    sha,
			"status": status,
		},
	}
	if iid > 0 {
		body["merge_request"] = map[string]any{"iid": iid}
	}
	raw, _ := json.Marshal(body)
	ev := gitlabEvent("pipeline", "alice", string(raw))
	ev.Repo = "acme/widgets"
	ev.DeliveryID = "gitlab:pipe-1"
	ev.CredentialRef = "gitlab:4242"
	ev.Action = status
	return ev
}

// gitlabBuildEvent builds the Job Hook GitLab emits for the SAME failing job
// as the pipeline event above. It carries ref / sha / pipeline_id at the TOP
// level — every correlation input the pipeline payload carries — which is
// what makes the build-event skip a real control rather than a decode
// accident.
func gitlabBuildEvent(status, ref, sha string, pipelineID int) Event {
	raw, _ := json.Marshal(map[string]any{
		"object_kind":  "build",
		"ref":          ref,
		"sha":          sha,
		"build_status": status,
		"pipeline_id":  pipelineID,
	})
	ev := gitlabEvent("build", "alice", string(raw))
	ev.Repo = "acme/widgets"
	ev.DeliveryID = "gitlab:build-1"
	ev.CredentialRef = "gitlab:4242"
	ev.Action = status
	return ev
}

// seedGitLabRun inserts a gitlab_ci run and, when headSHA is non-empty, an
// implement stage carrying the pull_request artifact that records it — the
// run-side half of the (ref, sha) correlation predicate.
func seedGitLabRun(t *testing.T, runs *stubRuns, arts *stubArtifacts, repo, headSHA string,
	retryAttempt int, mrIID int) *run.Run {
	t.Helper()
	triggerRef := "issue:7"
	ref := "gitlab:4242"
	r := &run.Run{
		ID:                 uuid.New(),
		Repo:               repo,
		WorkflowID:         "feature_change",
		WorkflowSHA:        "g1t1absha",
		TriggerSource:      run.TriggerGitHubIssue,
		TriggerRef:         &triggerRef,
		InstallationRef:    &ref,
		WorkflowSpec:       []byte(ciRetrySpec),
		RetryAttempt:       retryAttempt,
		MaxRetriesSnapshot: 1,
		RunnerKind:         run.RunnerKindGitLabCI,
		State:              run.StateRunning,
		CreatedAt:          time.Now().Add(-time.Minute).UTC(),
		UpdatedAt:          time.Now().Add(-time.Minute).UTC(),
	}
	if mrIID > 0 {
		url := fmt.Sprintf("https://gitlab.com/%s/-/merge_requests/%d", repo, mrIID)
		r.PullRequestURL = &url
	}
	runs.mu.Lock()
	runs.created = append(runs.created, r)
	if headSHA != "" {
		st := &run.Stage{ID: uuid.New(), RunID: r.ID, Type: run.StageTypeImplement, State: run.StageStateSucceeded}
		runs.createdStages = append(runs.createdStages, st)
		runs.mu.Unlock()
		arts.add(st.ID, &artifact.Artifact{
			ID:      uuid.New(),
			StageID: st.ID,
			Kind:    artifact.KindPullRequest,
			Content: []byte(fmt.Sprintf(`{"head_sha":%q}`, headSHA)),
		})
		return r
	}
	runs.mu.Unlock()
	return r
}

func newGitLabRetryDispatcher(t *testing.T) (*Dispatcher, *stubRuns, *stubAudit, *stubArtifacts) {
	t.Helper()
	d, _, runs, au := newDispatcherWithStubs(t)
	arts := &stubArtifacts{}
	d.Artifacts = arts
	d.GitLabFiles = &stubFileFetcher{content: []byte(ciRetrySpec), sha: "g1t1absha"}
	// The retry path refuses any project not vouched for by the registry — the
	// SAME gate the create path runs (concerns bc903dd2 / c651b974 / 325a350b).
	// Every test that expects a retry to happen registers the project
	// gitlabPipelineEvent names; the refusal tests replace or clear this.
	d.GitLabProjects = registeredGitLabProject()
	return d, runs, au, arts
}

// retryChildren returns the runs created BEYOND the seeded ones — i.e. the
// retry children this delivery minted.
func retryChildren(runs *stubRuns, seeded int) []*run.Run {
	runs.mu.Lock()
	defer runs.mu.Unlock()
	if len(runs.created) <= seeded {
		return nil
	}
	out := make([]*run.Run, len(runs.created)-seeded)
	copy(out, runs.created[seeded:])
	return out
}

func auditCategories(au *stubAudit) []string {
	au.mu.Lock()
	defer au.mu.Unlock()
	out := make([]string, 0, len(au.appended))
	for _, a := range au.appended {
		out = append(out, a.Category)
	}
	return out
}

// skipPayload returns the single ci_retry_skipped audit payload, failing when
// there is not exactly one. It searches BOTH chains: a skip is chained to a run
// only when that run owns the pipeline's ref, and global-chained otherwise, so
// a helper that read only one chain would silently miss half the outcomes.
// chainedToRun reports which chain carried it.
func skipPayload(t *testing.T, au *stubAudit) (payload map[string]any, chainedToRun bool) {
	t.Helper()
	au.mu.Lock()
	defer au.mu.Unlock()
	var found []map[string]any
	var chained []bool
	decode := func(raw []byte, onRun bool) {
		var p map[string]any
		if err := json.Unmarshal(raw, &p); err != nil {
			t.Fatalf("unmarshal ci_retry_skipped payload: %v", err)
		}
		found = append(found, p)
		chained = append(chained, onRun)
	}
	for _, a := range au.appended {
		if a.Category == "ci_retry_skipped" {
			decode(a.Payload, true)
		}
	}
	for _, a := range au.globalAppended {
		if a.Category == "ci_retry_skipped" {
			decode(a.Payload, false)
		}
	}
	if len(found) != 1 {
		t.Fatalf("ci_retry_skipped rows = %d, want exactly 1", len(found))
	}
	return found[0], chained[0]
}

// skipReason extracts the `reason` field from the single ci_retry_skipped
// audit payload, failing when there is not exactly one.
func skipReason(t *testing.T, au *stubAudit) string {
	t.Helper()
	p, _ := skipPayload(t, au)
	reason, _ := p["reason"].(string)
	return reason
}

// --- HIGH 3: Pipeline classified, Build skipped --------------------------

// TestGitLabCIRetry_BuildOnlyEventCreatesNoRetry is the build-only ISOLATION
// test and the counterfactual vehicle for the build-event skip (C6).
//
// It delivers a Job Hook with NO preceding pipeline event, against a run the
// build payload's ref+sha correlate to perfectly. Everything a retry needs is
// present EXCEPT the classification decision, so the only thing standing
// between this delivery and a retry child is the explicit build arm in
// matchGitLabCIFailure. Delete that arm and this test observes one child and
// goes red.
//
// It replaces the combined-delivery test as the vehicle deliberately: with
// runs_retry_child_once_idx in place, deleting the skip in a pipeline-THEN-
// build delivery still yields exactly one child (the second insert loses the
// unique_violation benignly), so that pairing CANNOT go red.
func TestGitLabCIRetry_BuildOnlyEventCreatesNoRetry(t *testing.T) {
	d, runs, au, arts := newGitLabRetryDispatcher(t)
	parent := seedGitLabRun(t, runs, arts, "acme/widgets", "deadbeef", 0, 0)

	ev := gitlabBuildEvent("failed", gitLabRunBranch(parent), "deadbeef", 9001)
	if err := d.Handle(context.Background(), ev); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if got := retryChildren(runs, 1); len(got) != 0 {
		t.Errorf("retry children = %d, want 0 (the pipeline hook is the single trigger)", len(got))
	}
	if cats := auditCategories(au); len(cats) != 0 {
		t.Errorf("audit categories = %v, want none", cats)
	}
}

// TestGitLabCIRetry_CombinedDeliveryEitherOrderCreatesOneChild pins the
// COMBINED-delivery invariant: whichever order the two hooks arrive in, one
// failing job produces exactly one retry child. Its counterfactual vehicle is
// the unique index, not the classification skip — see the build-only test.
func TestGitLabCIRetry_CombinedDeliveryEitherOrderCreatesOneChild(t *testing.T) {
	orders := []struct {
		name  string
		first func(ref string) Event
		then  func(ref string) Event
	}{
		{
			"pipeline_then_build",
			func(ref string) Event { return gitlabPipelineEvent("failed", ref, "deadbeef", 9001, 0) },
			func(ref string) Event { return gitlabBuildEvent("failed", ref, "deadbeef", 9001) },
		},
		{
			"build_then_pipeline",
			func(ref string) Event { return gitlabBuildEvent("failed", ref, "deadbeef", 9001) },
			func(ref string) Event { return gitlabPipelineEvent("failed", ref, "deadbeef", 9001, 0) },
		},
	}
	for _, tc := range orders {
		t.Run(tc.name, func(t *testing.T) {
			d, runs, _, arts := newGitLabRetryDispatcher(t)
			parent := seedGitLabRun(t, runs, arts, "acme/widgets", "deadbeef", 0, 0)
			branch := gitLabRunBranch(parent)

			for _, ev := range []Event{tc.first(branch), tc.then(branch)} {
				if err := d.Handle(context.Background(), ev); err != nil {
					t.Fatalf("Handle: %v", err)
				}
			}
			if got := retryChildren(runs, 1); len(got) != 1 {
				t.Fatalf("retry children = %d, want exactly 1", len(got))
			}
		})
	}
}

// TestMatchGitLabCIFailure_NonFailureAndMalformedSkip pins the classifier's
// remaining refusals: a non-failed status, and a failed payload missing the
// ref or sha the correlation contract requires.
func TestMatchGitLabCIFailure_NonFailureAndMalformedSkip(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"success_status", `{"object_attributes":{"id":1,"ref":"fishhawk/run-abc","sha":"s","status":"success"}}`},
		{"running_status", `{"object_attributes":{"id":1,"ref":"fishhawk/run-abc","sha":"s","status":"running"}}`},
		{"missing_ref", `{"object_attributes":{"id":1,"sha":"s","status":"failed"}}`},
		{"missing_sha", `{"object_attributes":{"id":1,"ref":"fishhawk/run-abc","status":"failed"}}`},
		{"unparseable", `{`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := MatchGitLabEvent(gitlabEvent("pipeline", "alice", tc.body))
			if !m.Skip {
				t.Fatalf("Skip = false, want true (got action %q)", m.Action)
			}
			if m.Reason == "" {
				t.Error("Reason is empty; a skip must name why")
			}
		})
	}
}

// --- authorization: the retry path is gated like the create path ---------

// TestGitLabCIRetry_UnregisteredProject_CreatesNothing is the authorization
// control for the CI-retry path (concerns bc903dd2 / c651b974 / 325a350b).
//
// The threat it closes is specific. A Pipeline Hook is authenticated only by
// the deployment-shared X-Gitlab-Token, and GitLab signs no HMAC over the body,
// so ev.Repo and ev.CredentialRef are attacker-chosen. Both correlation inputs
// are PUBLIC: a run branch is fishhawk/run-<short> and its head SHA is readable
// from any branch listing. So the fixture hands the forged delivery a PERFECT
// correlation — the real run's branch and the real head SHA — and the only
// thing standing between it and a credentialed pipeline dispatch is the
// registry gate. Delete the gate and every refusal cell mints a child.
//
// Every assertion reads what the FAKES RECORDED, never a returned value: a
// refusal and a success both return nil, so a return-value assertion could not
// tell them apart. Zero children and zero created stages together mean zero
// pipeline triggers — TriggerStage is reachable only after a child and its
// stages exist.
//
// The registered cell is the paired control: the SAME forged-shape delivery,
// admitted by the registry, DOES mint exactly one child. Without it a gate that
// refused unconditionally — or a correlation broken by the fixture — would
// satisfy every refusal cell.
func TestGitLabCIRetry_UnregisteredProject_CreatesNothing(t *testing.T) {
	cases := []struct {
		name         string
		authorizer   GitLabProjectAuthorizer
		wantChildren int
		wantReason   string
	}{
		{
			name:       "unregistered_project_id",
			authorizer: &stubGitLabProjects{allow: map[string]string{"gitlab:9999": "acme/widgets"}},
			wantReason: gitLabAuthzNotRegistered,
		},
		{
			// The registered id paired with a DIFFERENT project path.
			name:       "registered_id_foreign_project_path",
			authorizer: &stubGitLabProjects{allow: map[string]string{"gitlab:4242": "victim/other"}},
			wantReason: gitLabAuthzNotRegistered,
		},
		{
			name:       "registry_lookup_fails",
			authorizer: &stubGitLabProjects{err: errors.New("connection reset")},
			wantReason: gitLabAuthzLookupFailed,
		},
		{
			name:       "registry_unwired",
			authorizer: nil,
			wantReason: gitLabAuthzRegistryUnwired,
		},
		{
			name:         "registered_project_admits",
			authorizer:   registeredGitLabProject(),
			wantChildren: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, runs, au, arts := newGitLabRetryDispatcher(t)
			parent := seedGitLabRun(t, runs, arts, "acme/widgets", "deadbeef", 0, 0)
			d.GitLabProjects = tc.authorizer

			// A perfectly correlating forgery: the real run branch, the real head.
			ev := gitlabPipelineEvent("failed", gitLabRunBranch(parent), "deadbeef", 9001, 0)
			if err := d.Handle(context.Background(), ev); err != nil {
				t.Fatalf("Handle: %v (a refusal is a 202, not a 5xx)", err)
			}

			if got := retryChildren(runs, 1); len(got) != tc.wantChildren {
				t.Errorf("retry children = %d, want %d", len(got), tc.wantChildren)
			}
			// One stage pre-exists: the parent's seeded implement stage. Any
			// stage beyond it means a child was staged and TriggerStage ran.
			if got := len(runs.createdStages) - 1; got != tc.wantChildren {
				t.Errorf("retry stages = %d, want %d (no stages means no pipeline trigger)",
					got, tc.wantChildren)
			}
			if tc.wantReason == "" {
				return
			}
			// The refusal is RECORDED, and names BOTH which check failed and
			// which gate refused — a create refusal and a retry refusal share
			// one category and would otherwise be identical rows.
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

// TestGitLabCIRetry_AuthorizationSeesThePayloadIdentity pins WHAT the retry
// gate is handed — the payload's credential ref and project path, unmodified.
// A gate consulted with the wrong values could vouch for the wrong project
// while every count-based assertion above stayed green.
func TestGitLabCIRetry_AuthorizationSeesThePayloadIdentity(t *testing.T) {
	d, runs, _, arts := newGitLabRetryDispatcher(t)
	parent := seedGitLabRun(t, runs, arts, "acme/widgets", "deadbeef", 0, 0)
	authz := registeredGitLabProject()
	d.GitLabProjects = authz

	ev := gitlabPipelineEvent("failed", gitLabRunBranch(parent), "deadbeef", 9001, 0)
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

// TestGitLabCIRetry_UnregisteredProject_PerformsNoCandidateLookup pins the gate's
// POSITION, not merely its effect: authorization runs before the candidate
// query, so an unauthorized delivery cannot even probe which runs exist for a
// project. A gate placed after the lookup would satisfy the zero-children
// assertions above while still answering "does this project have Fishhawk runs?"
// to an unauthenticated caller.
func TestGitLabCIRetry_UnregisteredProject_PerformsNoCandidateLookup(t *testing.T) {
	d, runs, _, arts := newGitLabRetryDispatcher(t)
	parent := seedGitLabRun(t, runs, arts, "acme/widgets", "deadbeef", 0, 0)
	d.GitLabProjects = &stubGitLabProjects{allow: map[string]string{"gitlab:9999": "acme/widgets"}}

	if err := d.Handle(context.Background(),
		gitlabPipelineEvent("failed", gitLabRunBranch(parent), "deadbeef", 9001, 0)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	runs.mu.Lock()
	listCalls := runs.listCalls
	runs.mu.Unlock()
	if listCalls != 0 {
		t.Errorf("ListRuns calls = %d, want 0 (authorization must precede candidate lookup)", listCalls)
	}
}

// --- HIGH 2: deterministic (ref, sha) correlation ------------------------

// TestGitLabCIRetry_CorrelatesToRunByRefAndSHA drives SEVERAL candidate runs
// on ONE merge request — the shape a retry lineage produces by construction,
// and the reason the merge-request iid alone cannot select a run — and
// asserts the ref+sha predicate picks the right one.
func TestGitLabCIRetry_CorrelatesToRunByRefAndSHA(t *testing.T) {
	d, runs, au, arts := newGitLabRetryDispatcher(t)
	// Three runs on merge request !31, each with its own branch and head.
	_ = seedGitLabRun(t, runs, arts, "acme/widgets", "aaa111", 0, 31)
	want := seedGitLabRun(t, runs, arts, "acme/widgets", "bbb222", 0, 31)
	_ = seedGitLabRun(t, runs, arts, "acme/widgets", "ccc333", 0, 31)

	ev := gitlabPipelineEvent("failed", gitLabRunBranch(want), "bbb222", 9001, 31)
	if err := d.Handle(context.Background(), ev); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	children := retryChildren(runs, 3)
	if len(children) != 1 {
		t.Fatalf("retry children = %d, want 1", len(children))
	}
	if children[0].ParentRunID == nil || *children[0].ParentRunID != want.ID {
		t.Fatalf("child parent = %v, want the ref+sha-matched run %s",
			children[0].ParentRunID, want.ID)
	}
	// Pipeline id is captured as correlation provenance so an operator can
	// join the Fishhawk retry back to the GitLab pipeline that caused it.
	au.mu.Lock()
	defer au.mu.Unlock()
	var payload map[string]any
	for _, a := range au.appended {
		if a.Category == "ci_failure_retry_dispatched" {
			_ = json.Unmarshal(a.Payload, &payload)
		}
	}
	if payload == nil {
		t.Fatal("no ci_failure_retry_dispatched audit row")
	}
	if got, _ := payload["pipeline_id"].(float64); int(got) != 9001 {
		t.Errorf("audit pipeline_id = %v, want 9001", payload["pipeline_id"])
	}
	if got, _ := payload["pipeline_ref"].(string); got != gitLabRunBranch(want) {
		t.Errorf("audit pipeline_ref = %q, want %q", got, gitLabRunBranch(want))
	}
}

// TestGitLabCIRetry_FirstStagePipelineOnDefaultRefIsDeferred is BC1's
// behavioural test. It drives an ACTUAL first-stage pipeline payload — ref is
// the default branch, the SHA is present, and the run branch does not exist
// yet because the runner has not created it — and asserts it correlates to
// NOTHING and creates NO child.
//
// Critically it also asserts the outcome is DISTINGUISHABLE: a deliberate
// deferral writes ci_retry_skipped naming first_stage_pipeline_on_non_run_branch,
// which is a different record from every mis-correlation reason. A no-op that
// looked identical to a bad correlation would leave the next person debugging
// a missing retry unable to tell which happened (#2860).
func TestGitLabCIRetry_FirstStagePipelineOnDefaultRefIsDeferred(t *testing.T) {
	d, runs, au, arts := newGitLabRetryDispatcher(t)
	// A run whose FIRST stage is in flight: no implement stage, no recorded
	// head SHA, no run branch pushed yet.
	seedGitLabRun(t, runs, arts, "acme/widgets", "", 0, 0)

	ev := gitlabPipelineEvent("failed", "main", "c0ffee01", 9001, 0)
	if err := d.Handle(context.Background(), ev); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if got := retryChildren(runs, 1); len(got) != 0 {
		t.Fatalf("retry children = %d, want 0 (first-stage retry is deferred)", len(got))
	}
	if got := skipReason(t, au); got != gitLabRetrySkipFirstStageRef {
		t.Errorf("ci_retry_skipped reason = %q, want %q", got, gitLabRetrySkipFirstStageRef)
	}
	// ATTRIBUTION (concern d90af52b). A default-ref pipeline belongs to no run,
	// so the record must be GLOBAL-chained rather than hung off an unrelated
	// run. The seeded run above is the project's newest gitlab_ci run — exactly
	// the row the earlier "chain to candidates[0]" shape would have picked —
	// and it had no part in this pipeline.
	p, chainedToRun := skipPayload(t, au)
	if chainedToRun {
		t.Error("skip was chained to a run; a default-ref pipeline owns no run, so it must be global-chained")
	}
	if _, ok := p["run_id"]; ok {
		t.Errorf("payload carries run_id = %v; no run owns this pipeline", p["run_id"])
	}
}

// TestGitLabCIRetry_NonCorrelatingPipelinesSkipWithNamedReasons pins the
// remaining non-correlation outcomes, each with its OWN reason so the audit
// distinguishes them from the deliberate first-stage deferral above.
//
// It also pins each outcome's ATTRIBUTION (concern d90af52b): a skip is chained
// to a run ONLY when that run owns the pipeline's ref. The two cells differ on
// exactly that axis — an unknown run branch owns nothing and must be
// global-chained, while a wrong-SHA pipeline on a REAL run branch is genuinely
// about that run and chains to it. A writer that always chained to the newest
// candidate would satisfy the second cell and fail the first.
func TestGitLabCIRetry_NonCorrelatingPipelinesSkipWithNamedReasons(t *testing.T) {
	cases := []struct {
		name string
		// ref/sha are resolved against the seeded run by the closure.
		pipeline    func(parent *run.Run) Event
		want        string
		wantChained bool
	}{
		{
			"run_branch_of_no_candidate",
			func(*run.Run) Event {
				return gitlabPipelineEvent("failed", "fishhawk/run-99999999", "deadbeef", 9001, 0)
			},
			gitLabRetrySkipNoRunForRef,
			false,
		},
		{
			"right_branch_wrong_sha",
			func(p *run.Run) Event {
				return gitlabPipelineEvent("failed", gitLabRunBranch(p), "not-the-head", 9001, 0)
			},
			gitLabRetrySkipSHAMismatch,
			true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, runs, au, arts := newGitLabRetryDispatcher(t)
			parent := seedGitLabRun(t, runs, arts, "acme/widgets", "deadbeef", 0, 0)

			if err := d.Handle(context.Background(), tc.pipeline(parent)); err != nil {
				t.Fatalf("Handle: %v", err)
			}
			if got := retryChildren(runs, 1); len(got) != 0 {
				t.Fatalf("retry children = %d, want 0", len(got))
			}
			p, chainedToRun := skipPayload(t, au)
			if got, _ := p["reason"].(string); got != tc.want {
				t.Errorf("ci_retry_skipped reason = %q, want %q", got, tc.want)
			}
			if chainedToRun != tc.wantChained {
				t.Errorf("chained to a run = %v, want %v", chainedToRun, tc.wantChained)
			}
			if tc.wantChained {
				if got, _ := p["run_id"].(string); got != parent.ID.String() {
					t.Errorf("payload run_id = %q, want the ref-owning run %s", got, parent.ID)
				}
			} else if _, ok := p["run_id"]; ok {
				t.Errorf("payload carries run_id = %v; no run owns this pipeline's ref", p["run_id"])
			}
		})
	}
}

// TestGitLabCIRetry_CancelledLineageSkips pins the cancelled-lineage refusal:
// a manually stopped run is not restarted, and the refusal is named.
func TestGitLabCIRetry_CancelledLineageSkips(t *testing.T) {
	d, runs, au, arts := newGitLabRetryDispatcher(t)
	parent := seedGitLabRun(t, runs, arts, "acme/widgets", "deadbeef", 0, 0)
	runs.mu.Lock()
	parent.State = run.StateCancelled
	runs.mu.Unlock()

	ev := gitlabPipelineEvent("failed", gitLabRunBranch(parent), "deadbeef", 9001, 0)
	if err := d.Handle(context.Background(), ev); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := retryChildren(runs, 1); len(got) != 0 {
		t.Fatalf("retry children = %d, want 0", len(got))
	}
	if got := skipReason(t, au); got != gitLabRetrySkipCancelledLineage {
		t.Errorf("reason = %q, want %q", got, gitLabRetrySkipCancelledLineage)
	}
}

// TestGitLabCIRetry_UnresolvableRetryPolicySkips pins the refusal when the
// correlated run's cached spec cannot be read: the retry cap is unknown, so
// retrying would risk a runaway loop.
func TestGitLabCIRetry_UnresolvableRetryPolicySkips(t *testing.T) {
	d, runs, au, arts := newGitLabRetryDispatcher(t)
	parent := seedGitLabRun(t, runs, arts, "acme/widgets", "deadbeef", 0, 0)
	runs.mu.Lock()
	parent.WorkflowSpec = nil
	runs.mu.Unlock()

	ev := gitlabPipelineEvent("failed", gitLabRunBranch(parent), "deadbeef", 9001, 0)
	if err := d.Handle(context.Background(), ev); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := retryChildren(runs, 1); len(got) != 0 {
		t.Fatalf("retry children = %d, want 0", len(got))
	}
	if got := skipReason(t, au); got != gitLabRetrySkipNoRetryPolicy {
		t.Errorf("reason = %q, want %q", got, gitLabRetrySkipNoRetryPolicy)
	}
}

// TestGitLabCIRetry_NoCandidateRunsSkipsSilently pins the one non-correlation
// with NO audit row: a GitLab project Fishhawk manages no runs for. There is
// no run to chain an entry against, and the delivery is not a Fishhawk
// failure — the structured log is the whole record.
func TestGitLabCIRetry_NoCandidateRunsSkipsSilently(t *testing.T) {
	d, runs, au, _ := newGitLabRetryDispatcher(t)

	ev := gitlabPipelineEvent("failed", "fishhawk/run-abcdef12", "deadbeef", 9001, 0)
	if err := d.Handle(context.Background(), ev); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(runs.created) != 0 {
		t.Errorf("runs.created = %d, want 0", len(runs.created))
	}
	if cats := auditCategories(au); len(cats) != 0 {
		t.Errorf("audit categories = %v, want none (no run to chain against)", cats)
	}
}

// TestGitLabCIRetry_CapHitEmitsExhausted pins the cap: a parent already at
// max_retries emits ci_retry_exhausted and creates nothing.
func TestGitLabCIRetry_CapHitEmitsExhausted(t *testing.T) {
	d, runs, au, arts := newGitLabRetryDispatcher(t)
	// ciRetrySpec sets max_retries: 1, so a parent at attempt 1 is capped.
	parent := seedGitLabRun(t, runs, arts, "acme/widgets", "deadbeef", 1, 0)

	ev := gitlabPipelineEvent("failed", gitLabRunBranch(parent), "deadbeef", 9001, 0)
	if err := d.Handle(context.Background(), ev); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := retryChildren(runs, 1); len(got) != 0 {
		t.Fatalf("retry children = %d, want 0", len(got))
	}
	if cats := auditCategories(au); len(cats) != 1 || cats[0] != "ci_retry_exhausted" {
		t.Errorf("audit categories = %v, want [ci_retry_exhausted]", cats)
	}
}

// --- C1 / BC3: constraint-based dedup ------------------------------------

// TestGitLabCIRetry_DuplicateSentinelIsBenign pins the HANDLER half of the
// dedup contract and nothing more: when CreateRun reports
// run.ErrRetryChildDuplicate — the sentinel run.IsRetryChildDuplicate
// recognizes alongside a real 23505 on runs_retry_child_once_idx — the
// delivery returns nil rather than a 5xx the forge would redeliver, and mints
// no second child.
//
// IT IS NOT THE CONCURRENCY TEST, and its earlier name
// (…_ConcurrentDeliveriesCreateOneChild) overstated it: two SEQUENTIAL Handle
// calls against a fake that is HANDED the duplicate sentinel prove only that
// the handler tolerates the sentinel. Deleting the index would leave this
// green — the stub never consults the schema.
//
// The atomic dedup — two goroutines racing a REAL Postgres repository where the
// index itself decides — is pinned one layer down, by
// TestCreateRun_ConcurrentRetryChildrenRaceToOne in
// backend/internal/run/postgres_test.go. That is the counterfactual vehicle for
// runs_retry_child_once_idx (verified: dropping the index from migration 0076
// makes both concurrent inserts succeed and it goes red), and it lives in the
// run package deliberately, because the index is a persistence-layer
// constraint and this package has no Postgres-backed fixture.
//
// The surviving child's retry_attempt is still PINNED here, because it is the
// caller-side half of the same contract: both deliveries must derive the
// attempt from the PARENT row, never from the latest existing child.
func TestGitLabCIRetry_DuplicateSentinelIsBenign(t *testing.T) {
	d, runs, _, arts := newGitLabRetryDispatcher(t)
	parent := seedGitLabRun(t, runs, arts, "acme/widgets", "deadbeef", 0, 0)
	// First create wins; the second loses the unique-index race.
	runs.createErrQueue = []error{nil, run.ErrRetryChildDuplicate}

	ev := gitlabPipelineEvent("failed", gitLabRunBranch(parent), "deadbeef", 9001, 0)
	for i := 0; i < 2; i++ {
		if err := d.Handle(context.Background(), ev); err != nil {
			t.Fatalf("Handle (delivery %d): %v (a race loser must NOT surface an error)", i, err)
		}
	}

	children := retryChildren(runs, 1)
	if len(children) != 1 {
		t.Fatalf("retry children = %d, want exactly 1", len(children))
	}
	if children[0].RetryAttempt != parent.RetryAttempt+1 {
		t.Errorf("surviving child retry_attempt = %d, want %d (parent-derived, per BC3)",
			children[0].RetryAttempt, parent.RetryAttempt+1)
	}
}

// TestGitLabCIRetry_RetryAttemptIsParentDerived is the direct pin on BC3's
// caller contract across a two-deep lineage: a child of a parent already at
// attempt 1 must be attempt 2 — derived from the parent it retries, not from
// any other row.
func TestGitLabCIRetry_RetryAttemptIsParentDerived(t *testing.T) {
	d, runs, _, arts := newGitLabRetryDispatcher(t)
	// max_retries 1 would cap attempt 1, so raise the cap for this lineage.
	spec2 := `version: "0.3"
roles:
  tech_lead:
    members: ["@kuhlman-labs"]
workflows:
  feature_change:
    description: Test workflow with retries
    on_ci_failure:
      max_retries: 5
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
`
	parent := seedGitLabRun(t, runs, arts, "acme/widgets", "deadbeef", 1, 0)
	runs.mu.Lock()
	parent.WorkflowSpec = []byte(spec2)
	runs.mu.Unlock()

	ev := gitlabPipelineEvent("failed", gitLabRunBranch(parent), "deadbeef", 9001, 0)
	if err := d.Handle(context.Background(), ev); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	children := retryChildren(runs, 1)
	if len(children) != 1 {
		t.Fatalf("retry children = %d, want 1", len(children))
	}
	if children[0].RetryAttempt != 2 {
		t.Errorf("child retry_attempt = %d, want 2 (parent.RetryAttempt + 1)", children[0].RetryAttempt)
	}
}

// --- credential-ref fallback (BC5, dispatch layer) -----------------------

// TestGitLabRunScope_RefLadder pins the three-state installation_ref
// distinction WHERE THE FALLBACK LIVES — the dispatch layer's scope
// resolution — not only at the approval layer. Absent (nil) and
// recorded-as-EMPTY are different states and both must fall back to the zero
// scope rather than producing a scope from an empty ref; only a non-empty ref
// resolves.
func TestGitLabRunScope_RefLadder(t *testing.T) {
	empty := ""
	valid := "gitlab:4242"
	cases := []struct {
		name     string
		ref      *string
		wantRef  string
		wantZero bool
	}{
		{"nil_ref_zero_scope", nil, "", true},
		{"empty_string_ref_zero_scope", &empty, "", true},
		{"valid_ref_resolves", &valid, "gitlab:4242", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := gitLabRunScope(&run.Run{InstallationRef: tc.ref})
			if got.IsZero() != tc.wantZero {
				t.Errorf("IsZero() = %v, want %v", got.IsZero(), tc.wantZero)
			}
			if got.Ref() != tc.wantRef {
				t.Errorf("Ref() = %q, want %q", got.Ref(), tc.wantRef)
			}
		})
	}
}

// TestGitLabRunBranch_MatchesOrchestratorDerivation pins the CROSS-PACKAGE
// equivalence its name claims: every case compares gitLabRunBranch against
// orchestrator.RunBranchRef — the exported seam onto the SAME unexported
// derivation orchestrator.triggerParams dispatches against (pinned there by
// TestRunBranchRef_IsTheRefTriggerParamsDispatches).
//
// The earlier version of this test compared against hard-coded strings, so a
// change to orchestrator.runBranchRef that diverged from this package's copy
// left it green while correlation silently missed EVERY pipeline — the failure
// mode is total and invisible, which is exactly why the assertion has to run
// the other derivation rather than restate its output. Importing the
// orchestrator here is acyclic: orchestrator depends on run/forge/runnerbackend
// and never on webhook.
//
// The exact-string expectations are KEPT alongside the equivalence assertion.
// Equivalence alone would stay green if BOTH derivations changed together, and
// the branch name is also a wire contract with the runner's childSliceBranch.
func TestGitLabRunBranch_MatchesOrchestratorDerivation(t *testing.T) {
	id := uuid.MustParse("1dd0f40b-ea08-41cc-a79c-cc4a6a931648")
	parentID := uuid.MustParse("bff9a242-9c4a-48a3-ad84-5feb66de9da2")
	idx := 2
	otherIdx := 0

	cases := []struct {
		name     string
		run      *run.Run
		wantName string
	}{
		{"top_level_run", &run.Run{ID: id}, "fishhawk/run-1dd0f40b"},
		{"decomposed_child", &run.Run{ID: id, DecomposedFrom: &parentID, SliceIndex: &idx},
			"fishhawk/run-bff9a242/slice-2"},
		{"slice_index_zero", &run.Run{ID: id, DecomposedFrom: &parentID, SliceIndex: &otherIdx},
			"fishhawk/run-bff9a242/slice-0"},
		// Only ONE of the two decomposition fields set is not a slice: both
		// derivations must fall back to the run's own namespace.
		{"decomposed_from_without_slice_index", &run.Run{ID: id, DecomposedFrom: &parentID},
			"fishhawk/run-1dd0f40b"},
		{"slice_index_without_decomposed_from", &run.Run{ID: id, SliceIndex: &idx},
			"fishhawk/run-1dd0f40b"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := gitLabRunBranch(tc.run)
			if want := orchestrator.RunBranchRef(tc.run); got != want {
				t.Errorf("gitLabRunBranch = %q, but orchestrator derives %q — correlation would miss every pipeline",
					got, want)
			}
			if got != tc.wantName {
				t.Errorf("branch = %q, want %q", got, tc.wantName)
			}
		})
	}
}

// --- degrade + error branches ---------------------------------------------

// TestGitLabCIRetry_MissingPipelineRef_Skips pins the defensive guard at the
// top of the handler. It is unreachable through Handle (the matcher fills
// PipelineRef in lock-step with tagging the action), so it is driven
// directly — a structural tripwire against a future matcher that tags the
// action without the ref.
func TestGitLabCIRetry_MissingPipelineRef_Skips(t *testing.T) {
	d, runs, au, _ := newGitLabRetryDispatcher(t)

	err := d.handleGitLabCIRetry(context.Background(),
		gitlabPipelineEvent("failed", "fishhawk/run-abcdef12", "deadbeef", 9001, 0),
		Match{Action: MatchActionCIFailureRetry}) // PipelineRef deliberately nil
	if err != nil {
		t.Fatalf("handleGitLabCIRetry: %v, want nil", err)
	}
	if len(runs.created) != 0 || len(au.appended) != 0 {
		t.Errorf("created = %d runs / %d audit rows, want 0 / 0", len(runs.created), len(au.appended))
	}
}

// TestGitLabCIRetry_CandidateLookupFails_SkipsWithoutRetrying pins the
// degrade when the candidate query itself faults: no child, no audit row, and
// crucially NO error — a 5xx would make the forge redeliver a pipeline event
// we cannot correlate anyway.
func TestGitLabCIRetry_CandidateLookupFails_SkipsWithoutRetrying(t *testing.T) {
	d, runs, au, arts := newGitLabRetryDispatcher(t)
	parent := seedGitLabRun(t, runs, arts, "acme/widgets", "deadbeef", 0, 0)
	branch := gitLabRunBranch(parent)
	runs.listErr = errors.New("connection reset")

	if err := d.Handle(context.Background(),
		gitlabPipelineEvent("failed", branch, "deadbeef", 9001, 0)); err != nil {
		t.Fatalf("Handle: %v, want nil (a query flap must not trigger redelivery)", err)
	}
	if got := retryChildren(runs, 1); len(got) != 0 {
		t.Errorf("retry children = %d, want 0", len(got))
	}
	if cats := auditCategories(au); len(cats) != 0 {
		t.Errorf("audit categories = %v, want none", cats)
	}
}

// TestGitLabCIRetry_CreateChildFails_SurfacesError is the paired negative of
// the duplicate branch: only the runs_retry_child_once_idx collision is
// benign. Any other create error must surface so the forge redelivers.
func TestGitLabCIRetry_CreateChildFails_SurfacesError(t *testing.T) {
	d, runs, _, arts := newGitLabRetryDispatcher(t)
	parent := seedGitLabRun(t, runs, arts, "acme/widgets", "deadbeef", 0, 0)
	runs.createErrQueue = []error{errors.New("connection reset")}

	err := d.Handle(context.Background(),
		gitlabPipelineEvent("failed", gitLabRunBranch(parent), "deadbeef", 9001, 0))
	if err == nil {
		t.Fatal("Handle returned nil; want the non-duplicate create error surfaced")
	}
	if !strings.Contains(err.Error(), "connection reset") {
		t.Errorf("error = %v, want it to carry the underlying create failure", err)
	}
}

// TestGitLabCIRetry_CreateStagesFails_SurfacesError pins the same posture one
// step later: a stage-insert fault is infrastructure, not a policy refusal.
func TestGitLabCIRetry_CreateStagesFails_SurfacesError(t *testing.T) {
	d, runs, _, arts := newGitLabRetryDispatcher(t)
	parent := seedGitLabRun(t, runs, arts, "acme/widgets", "deadbeef", 0, 0)
	runs.createStageErr = errors.New("stage insert failed")

	err := d.Handle(context.Background(),
		gitlabPipelineEvent("failed", gitLabRunBranch(parent), "deadbeef", 9001, 0))
	if err == nil {
		t.Fatal("Handle returned nil; want the stage-create error surfaced")
	}
	if !strings.Contains(err.Error(), "stage insert failed") {
		t.Errorf("error = %v, want it to carry the underlying stage failure", err)
	}
}

// TestGitLabCIRetry_PlanOnlyWorkflow_MintsChildButDispatchesNothing pins the
// no-non-plan-stages branch. A retry skips plan stages (the child reuses the
// parent's approved plan), so a plan-ONLY workflow leaves nothing to retry:
// the child row exists but no stage is created and nothing is dispatched.
func TestGitLabCIRetry_PlanOnlyWorkflow_MintsChildButDispatchesNothing(t *testing.T) {
	planOnly := `version: "0.3"
roles:
  tech_lead:
    members: ["@kuhlman-labs"]
workflows:
  feature_change:
    description: Plan-only workflow
    on_ci_failure:
      max_retries: 2
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
`
	d, runs, au, arts := newGitLabRetryDispatcher(t)
	parent := seedGitLabRun(t, runs, arts, "acme/widgets", "deadbeef", 0, 0)
	runs.mu.Lock()
	parent.WorkflowSpec = []byte(planOnly)
	runs.mu.Unlock()

	if err := d.Handle(context.Background(),
		gitlabPipelineEvent("failed", gitLabRunBranch(parent), "deadbeef", 9001, 0)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := retryChildren(runs, 1); len(got) != 1 {
		t.Fatalf("retry children = %d, want 1 (the child row is created before the stage filter)", len(got))
	}
	// One stage pre-exists: the parent's seeded implement stage (which
	// carries the head-SHA artifact). The retry must add NONE beyond it.
	if len(runs.createdStages) != 1 {
		t.Errorf("stages = %d, want 1 (only the parent's seeded stage; every retry stage was a plan stage)",
			len(runs.createdStages))
	}
	if cats := auditCategories(au); len(cats) != 0 {
		t.Errorf("audit categories = %v, want none (nothing was dispatched)", cats)
	}
}

// TestGitLabCIRetry_TransitionFails_StillAuditsDispatch pins the best-effort
// stage transition: a transition fault logs but must NOT unwind the retry —
// the child and its stage already exist, and the audit row is the record.
func TestGitLabCIRetry_TransitionFails_StillAuditsDispatch(t *testing.T) {
	d, runs, au, arts := newGitLabRetryDispatcher(t)
	parent := seedGitLabRun(t, runs, arts, "acme/widgets", "deadbeef", 0, 0)
	runs.transitionErr = errors.New("row locked")

	if err := d.Handle(context.Background(),
		gitlabPipelineEvent("failed", gitLabRunBranch(parent), "deadbeef", 9001, 0)); err != nil {
		t.Fatalf("Handle: %v, want nil (transition is best-effort)", err)
	}
	if got := retryChildren(runs, 1); len(got) != 1 {
		t.Fatalf("retry children = %d, want 1", len(got))
	}
	if cats := auditCategories(au); len(cats) != 1 || cats[0] != "ci_failure_retry_dispatched" {
		t.Errorf("audit categories = %v, want [ci_failure_retry_dispatched]", cats)
	}
}

// TestGitLabCIRetry_PipelineTriggerFails_ChildPersistsUndispatched pins the
// retry path's dispatch-FAILED branch (concerns e57e8cd3 / b57c1fd3) — the one
// outcome no prior test reached, because every fixture let the trigger succeed.
//
// Same contract as the create path's failure branch, and same reason: the child
// and its stages stay (recovery acts on them), the stage is NOT advanced to
// `dispatched` (that would leave the run waiting on a pipeline that was never
// created), and the audit records dispatch_failed carrying the error. The
// trigger is a reachable in-test fake returning a chosen error rather than an
// unroutable endpoint, so the assertion is on the branch and not on a
// connection failure that could reach the same state either way.
//
// It also pins WHAT was attempted: the trigger is called once, against the
// CHILD's run branch and under the PARENT's credential scope.
func TestGitLabCIRetry_PipelineTriggerFails_ChildPersistsUndispatched(t *testing.T) {
	d, runs, au, arts := newGitLabRetryDispatcher(t)
	parent := seedGitLabRun(t, runs, arts, "acme/widgets", "deadbeef", 0, 0)
	trigger := &stubPipelineTrigger{err: errors.New("gitlab: 500 internal error")}
	d.GitLabTrigger = trigger

	if err := d.Handle(context.Background(),
		gitlabPipelineEvent("failed", gitLabRunBranch(parent), "deadbeef", 9001, 0)); err != nil {
		t.Fatalf("Handle: %v, want nil (a failed trigger is recorded, not redelivered)", err)
	}

	children := retryChildren(runs, 1)
	if len(children) != 1 {
		t.Fatalf("retry children = %d, want 1 (a failed dispatch does not unwind the child)", len(children))
	}
	child := children[0]

	trigger.mu.Lock()
	calls := append([]stubPipelineCall(nil), trigger.calls...)
	trigger.mu.Unlock()
	if len(calls) != 1 {
		t.Fatalf("pipeline trigger calls = %d, want 1", len(calls))
	}
	if want := gitLabRunBranch(child); calls[0].Ref != want {
		t.Errorf("trigger ref = %q, want the CHILD's run branch %q", calls[0].Ref, want)
	}
	if want := gitLabRunScope(parent).Ref(); calls[0].Scope != want {
		t.Errorf("trigger scope = %q, want the PARENT's credential ref %q", calls[0].Scope, want)
	}

	// The stage exists but was not advanced. Asserted BOTH ways: no transition
	// was attempted, and the child's persisted stage row still READS pending.
	// The row is what a recovery verb acts on, and the stub mutates it on
	// transition, so this is a state assertion and not a restatement of the
	// transition log.
	if len(runs.transitions) != 0 {
		t.Errorf("transitions = %+v, want none (nothing was dispatched)", runs.transitions)
	}
	runs.mu.Lock()
	stages := append([]*run.Stage(nil), runs.createdStages...)
	runs.mu.Unlock()
	childStages := 0
	for _, st := range stages {
		if st.RunID != child.ID {
			continue
		}
		childStages++
		if st.State != run.StageStatePending {
			t.Errorf("child stage %s state = %q, want %q; a stage marked dispatched when no "+
				"pipeline was created leaves the retry waiting forever", st.ID, st.State, run.StageStatePending)
		}
	}
	if childStages == 0 {
		t.Error("child has no stage rows; the retry stages must persist for recovery to act on")
	}
	// The audit records the failure rather than a dispatch.
	if cats := auditCategories(au); len(cats) != 1 || cats[0] != "ci_failure_retry_dispatched" {
		t.Fatalf("audit categories = %v, want [ci_failure_retry_dispatched]", cats)
	}
	au.mu.Lock()
	payload := au.appended[0].Payload
	au.mu.Unlock()
	var p map[string]any
	if err := json.Unmarshal(payload, &p); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if got, _ := p["outcome"].(string); got != "dispatch_failed" {
		t.Errorf("audit outcome = %q, want dispatch_failed", got)
	}
	if got, _ := p["error"].(string); !strings.Contains(got, "500 internal error") {
		t.Errorf("audit error = %q, want it to carry the trigger failure", got)
	}
}

// TestGitLabCIRetry_AuditAppendFails_DoesNotUnwind pins both audit writers'
// best-effort posture: an append fault logs at ERROR and leaves the decision
// (retry taken, or retry skipped) exactly as it was.
func TestGitLabCIRetry_AuditAppendFails_DoesNotUnwind(t *testing.T) {
	t.Run("dispatched_path", func(t *testing.T) {
		d, runs, au, arts := newGitLabRetryDispatcher(t)
		parent := seedGitLabRun(t, runs, arts, "acme/widgets", "deadbeef", 0, 0)
		au.appendErr = errors.New("audit down")

		if err := d.Handle(context.Background(),
			gitlabPipelineEvent("failed", gitLabRunBranch(parent), "deadbeef", 9001, 0)); err != nil {
			t.Fatalf("Handle: %v, want nil", err)
		}
		if got := retryChildren(runs, 1); len(got) != 1 {
			t.Errorf("retry children = %d, want 1 (the retry still happened)", len(got))
		}
	})
	t.Run("skipped_path", func(t *testing.T) {
		d, runs, au, arts := newGitLabRetryDispatcher(t)
		seedGitLabRun(t, runs, arts, "acme/widgets", "", 0, 0)
		au.appendErr = errors.New("audit down")

		if err := d.Handle(context.Background(),
			gitlabPipelineEvent("failed", "main", "c0ffee01", 9001, 0)); err != nil {
			t.Fatalf("Handle: %v, want nil", err)
		}
		if got := retryChildren(runs, 1); len(got) != 0 {
			t.Errorf("retry children = %d, want 0 (the skip still held)", len(got))
		}
	})
}

// TestGitLabCIRetry_CapHit_AuditAppendFails_DoesNotUnwind pins the
// exhausted-audit writer's best-effort posture on the same principle: the
// refusal already happened, so a failed append must not turn it into a retry.
func TestGitLabCIRetry_CapHit_AuditAppendFails_DoesNotUnwind(t *testing.T) {
	d, runs, au, arts := newGitLabRetryDispatcher(t)
	parent := seedGitLabRun(t, runs, arts, "acme/widgets", "deadbeef", 1, 0)
	au.appendErr = errors.New("audit down")

	if err := d.Handle(context.Background(),
		gitlabPipelineEvent("failed", gitLabRunBranch(parent), "deadbeef", 9001, 0)); err != nil {
		t.Fatalf("Handle: %v, want nil", err)
	}
	if got := retryChildren(runs, 1); len(got) != 0 {
		t.Errorf("retry children = %d, want 0 (cap still holds)", len(got))
	}
}

// TestGitLabCIRetry_MergeRequestNarrowingFallsBackWhenNothingMatches pins the
// narrowing's deliberate best-effort shape: when the iid matches no
// candidate's recorded merge-request URL, the FULL candidate set is used
// rather than an empty one, so correlation never depends on artifact timing.
func TestGitLabCIRetry_MergeRequestNarrowingFallsBackWhenNothingMatches(t *testing.T) {
	d, runs, _, arts := newGitLabRetryDispatcher(t)
	// The run records merge request !31, but the pipeline claims !99.
	parent := seedGitLabRun(t, runs, arts, "acme/widgets", "deadbeef", 0, 31)

	if err := d.Handle(context.Background(),
		gitlabPipelineEvent("failed", gitLabRunBranch(parent), "deadbeef", 9001, 99)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := retryChildren(runs, 1); len(got) != 1 {
		t.Fatalf("retry children = %d, want 1 (ref+sha still correlate; narrowing only narrows)", len(got))
	}
}

// TestShortRunID pins the 8-character branch-name prefix. The helper's
// len(s) < 8 arm is structurally UNREACHABLE — uuid.UUID.String() is always
// 36 characters — and is kept only to stay byte-identical to the
// orchestrator's shortRunID, whose formula this correlation depends on.
func TestShortRunID(t *testing.T) {
	if got := shortRunID(uuid.Nil); got != "00000000" {
		t.Errorf("shortRunID(uuid.Nil) = %q, want the first 8 chars", got)
	}
}

// TestGitLabRetryCandidates_UnwiredGuards pins the nil-repository / empty-repo
// guard: with nothing to query, the candidate set is empty and NO error is
// produced, so the caller takes the quiet "not a Fishhawk project" path rather
// than the "lookup failed" one.
func TestGitLabRetryCandidates_UnwiredGuards(t *testing.T) {
	pr := &PipelineRef{Ref: "fishhawk/run-abcdef12", SHA: "deadbeef"}
	cases := []struct {
		name string
		d    *Dispatcher
		ev   Event
	}{
		{"nil_run_repository", &Dispatcher{}, Event{Repo: "acme/widgets"}},
		{"empty_repo", &Dispatcher{Runs: &stubRuns{}}, Event{Repo: ""}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.d.gitLabRetryCandidates(context.Background(), tc.ev, pr)
			if err != nil {
				t.Fatalf("err = %v, want nil (an unwired lookup is not a fault)", err)
			}
			if len(got) != 0 {
				t.Errorf("candidates = %d, want 0", len(got))
			}
		})
	}
}

// TestGitLabCIRetry_HeadSHALookupFails_SkipsThatCandidate pins the
// per-candidate degrade inside the correlation walk: a candidate whose stage
// lookup faults is SKIPPED, not treated as a match and not turned into a 5xx.
//
// It ALSO pins the reason that fault is recorded under (concern dbc5052b). The
// pipeline SHA is deliberately the run's REAL head — `deadbeef`, the same value
// seedGitLabRun records — so the only thing preventing a match is the
// repository fault. Pairing a fault with a genuinely-wrong SHA would leave the
// test green under either reason and prove nothing: it would refuse the
// delivery whether or not the fault were distinguished. Because the SHA WOULD
// have matched, reporting this as pipeline_sha_does_not_match_run_head_sha
// would be an outright false statement about a comparison that never ran, and
// the retry that should have happened would be indistinguishable in the audit
// from one correctly declined (BC1 / #2860).
func TestGitLabCIRetry_HeadSHALookupFails_SkipsThatCandidate(t *testing.T) {
	d, runs, au, arts := newGitLabRetryDispatcher(t)
	parent := seedGitLabRun(t, runs, arts, "acme/widgets", "deadbeef", 0, 0)
	branch := gitLabRunBranch(parent)
	runs.listStagesErr = errors.New("stage query failed")

	if err := d.Handle(context.Background(),
		gitlabPipelineEvent("failed", branch, "deadbeef", 9001, 0)); err != nil {
		t.Fatalf("Handle: %v, want nil", err)
	}
	if got := retryChildren(runs, 1); len(got) != 0 {
		t.Fatalf("retry children = %d, want 0 (a faulting candidate must not be treated as a match)", len(got))
	}
	if got := skipReason(t, au); got != gitLabRetrySkipHeadSHAUnreadable {
		t.Errorf("reason = %q, want %q (the head could not be READ; it was never compared)",
			got, gitLabRetrySkipHeadSHAUnreadable)
	}
}
