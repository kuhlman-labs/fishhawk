package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/policy"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/stagecheck"
	"github.com/kuhlman-labs/fishhawk/backend/internal/webhook"
)

// makeGitLabPipelinePayload builds a Pipeline Hook body with the fields the
// ingester reads. mrIID 0 omits merge_request (a branch pipeline).
func makeGitLabPipelinePayload(projectID int64, path string, pipelineID int64, sha, status, createdAt, finishedAt string, mrIID int) []byte {
	body := map[string]any{
		"object_kind": "pipeline",
		"project":     map[string]any{"id": projectID, "path_with_namespace": path},
		"object_attributes": map[string]any{
			"id":          pipelineID,
			"ref":         "feat",
			"sha":         sha,
			"status":      status,
			"source":      "merge_request_event",
			"created_at":  createdAt,
			"finished_at": finishedAt,
		},
	}
	if mrIID != 0 {
		body["merge_request"] = map[string]any{"iid": mrIID}
	}
	b, _ := json.Marshal(body)
	return b
}

// gitLabPipelineEvent wraps a payload in the Event the receiver would hand
// the ingester (ParseGitLabEvent sets Repo / CredentialRef / Forge).
func gitLabPipelineEvent(repo, credentialRef string, body []byte) webhook.Event {
	return webhook.Event{
		Type:          "pipeline",
		DeliveryID:    "gitlab:" + uuid.NewString(),
		Repo:          repo,
		CredentialRef: credentialRef,
		Forge:         webhook.ForgeGitLab,
		RawBody:       body,
	}
}

// pipelineRunRepo is reevalRunRepo with a working GetRun — the ingester
// reads the run's snapshot flag by id, where the GitHub re-eval resolves
// the run by PR URL.
type pipelineRunRepo struct {
	*reevalRunRepo
	getErr error
}

func (r *pipelineRunRepo) GetRun(_ context.Context, id uuid.UUID) (*run.Run, error) {
	if r.getErr != nil {
		return nil, r.getErr
	}
	for _, rn := range r.runs {
		if rn.ID == id {
			return rn, nil
		}
	}
	return nil, run.ErrNotFound
}

// pipelineCheckRepo is stageCheckRepoFake whose Append ALSO makes the row
// visible to the latest-row readers (the plain fake only records the
// call), so the per-run re-eval the ingester triggers reads what it just
// wrote — the shape the real repository has.
type pipelineCheckRepo struct {
	*stageCheckRepoFake
}

func (f *pipelineCheckRepo) Append(ctx context.Context, p stagecheck.AppendParams) (*stagecheck.Check, error) {
	c, err := f.stageCheckRepoFake.Append(ctx, p)
	if err != nil {
		return nil, err
	}
	c.Status, c.Conclusion, c.HeadSHA = p.Status, p.Conclusion, p.HeadSHA
	c.GitLabPipelineID, c.Timestamp, c.Payload = p.GitLabPipelineID, p.Timestamp, p.Payload
	f.byKey[p.StageID.String()+":"+p.Name] = c
	// Replace the latest-per-name entry rather than accumulating.
	kept := f.byStage[p.StageID][:0]
	for _, prev := range f.byStage[p.StageID] {
		if prev.Name != p.Name {
			kept = append(kept, prev)
		}
	}
	f.byStage[p.StageID] = append(kept, c)
	return c, nil
}

// pipelineFixture is one or more GitLab runs (implement + review stage
// each, a `gitlab/pipeline` snapshot, a prior deferred policy_evaluated
// row on the implement stage) behind the stagecheck fake, whose
// gitlabMatches decide which review stages the ingester writes to.
type pipelineFixture struct {
	srv    *Server
	runs   *pipelineRunRepo
	audit  *reevalAuditRepo
	checks *stageCheckRepoFake
}

func newPipelineFixture(t *testing.T) *pipelineFixture {
	t.Helper()
	runs := &pipelineRunRepo{reevalRunRepo: newReevalRunRepo()}
	aud := newReevalAuditRepo()
	checks := newStageCheckRepoFake()
	srv := New(Config{
		Addr:           "127.0.0.1:0",
		RunRepo:        runs,
		AuditRepo:      aud,
		StageCheckRepo: &pipelineCheckRepo{stageCheckRepoFake: checks},
	})
	return &pipelineFixture{srv: srv, runs: runs, audit: aud, checks: checks}
}

// addRun seeds a run with the given snapshot and returns (runID, implement
// stage id, review stage id). The review stage is registered as a match.
func (f *pipelineFixture) addRun(snap *run.RequiredChecksSnapshot) (runID, implID, reviewID uuid.UUID) {
	runID, implID, reviewID = uuid.New(), uuid.New(), uuid.New()
	ref := "gitlab:4242"
	f.runs.runs = append(f.runs.runs, &run.Run{
		ID: runID, Repo: "acme/widgets", InstallationRef: &ref,
		RequiredChecksSnapshot: snap,
	})
	f.runs.stagesByRunID[runID] = []*run.Stage{
		{ID: implID, RunID: runID, Sequence: 0, Type: run.StageTypeImplement},
		{ID: reviewID, RunID: runID, Sequence: 1, Type: run.StageTypeReview},
	}
	f.audit.seedPolicyEvaluated(runID, implID, policy.EvaluationPayload{
		StageType: "implement",
		Diff:      []policy.DiffEntry{{Path: "x.go", Status: policy.Status("modified")}},
		Applied:   policy.Constraints{RequiredOutcomes: []string{"ci_green"}},
		Passed:    true, DeferredOutcomes: []string{"ci_green"},
	})
	f.checks.gitlabMatches = append(f.checks.gitlabMatches, stagecheck.StageRef{StageID: reviewID, RunID: runID})
	return runID, implID, reviewID
}

func gitLabSnapshot(allowSkipped bool) *run.RequiredChecksSnapshot {
	return &run.RequiredChecksSnapshot{
		Contexts:                   []string{webhook.GitLabPipelineCheckContext},
		Sources:                    []string{webhook.GitLabPipelineRequirementSource},
		GitLabAllowSkippedPipeline: allowSkipped,
	}
}

// policyEvaluatedFor returns the CIGreen of the newest appended
// policy_evaluated row anchored on stageID, and whether one exists.
func (f *pipelineFixture) policyEvaluatedFor(t *testing.T, stageID uuid.UUID) (*bool, bool) {
	t.Helper()
	f.audit.mu.Lock()
	defer f.audit.mu.Unlock()
	for i := len(f.audit.appended) - 1; i >= 0; i-- {
		p := f.audit.appended[i]
		if p.Category != policy.CategoryPolicyEvaluated || p.StageID == nil || *p.StageID != stageID {
			continue
		}
		var pl policy.EvaluationPayload
		if err := json.Unmarshal(p.Payload, &pl); err != nil {
			t.Fatalf("decode policy_evaluated: %v", err)
		}
		return pl.Applied.CIGreen, true
	}
	return nil, false
}

// TestGitLabPipelineCheckState pins every documented GitLab pipeline
// status onto the stage_checks vocabulary, including BOTH skipped arms and
// the unknown-status fallback that must never clear a gate.
func TestGitLabPipelineCheckState(t *testing.T) {
	cases := []struct {
		status       string
		allowSkipped bool
		wantStatus   string
		wantConcl    string // "" = nil
		wantState    stagecheck.State
	}{
		{"success", false, "completed", "success", stagecheck.StatePass},
		{"failed", false, "completed", "failure", stagecheck.StateFail},
		{"canceled", false, "completed", "cancelled", stagecheck.StateFail},
		{"cancelled", false, "completed", "cancelled", stagecheck.StateFail},
		{"skipped", true, "completed", "skipped", stagecheck.StatePass},
		{"skipped", false, "completed", "skipped_not_allowed", stagecheck.StateFail},
		{"created", false, "queued", "", stagecheck.StatePending},
		{"waiting_for_resource", false, "queued", "", stagecheck.StatePending},
		{"preparing", false, "queued", "", stagecheck.StatePending},
		{"pending", false, "queued", "", stagecheck.StatePending},
		{"scheduled", false, "queued", "", stagecheck.StatePending},
		{"running", false, "in_progress", "", stagecheck.StatePending},
		{"manual", false, "in_progress", "", stagecheck.StatePending},
		{"canceling", false, "in_progress", "", stagecheck.StatePending},
		{"some_future_status", false, "in_progress", "", stagecheck.StatePending},
		{"", false, "in_progress", "", stagecheck.StatePending},
	}
	for _, c := range cases {
		t.Run(c.status+"/allow="+map[bool]string{true: "on", false: "off"}[c.allowSkipped], func(t *testing.T) {
			gotStatus, gotConcl := gitLabPipelineCheckState(c.status, c.allowSkipped)
			if gotStatus != c.wantStatus {
				t.Errorf("status = %q, want %q", gotStatus, c.wantStatus)
			}
			switch {
			case c.wantConcl == "" && gotConcl != nil:
				t.Errorf("conclusion = %q, want nil", *gotConcl)
			case c.wantConcl != "" && (gotConcl == nil || *gotConcl != c.wantConcl):
				t.Errorf("conclusion = %v, want %q", gotConcl, c.wantConcl)
			}
			if got := stagecheck.DeriveState(gotStatus, gotConcl); got != c.wantState {
				t.Errorf("DeriveState = %q, want %q", got, c.wantState)
			}
		})
	}
}

// TestGitLabPipelineTimestamp pins the two documented layouts, the
// finished_at-over-created_at preference, and the now() fallback.
func TestGitLabPipelineTimestamp(t *testing.T) {
	ctx := context.Background()
	logger := slog.New(slog.DiscardHandler)
	docExample := time.Date(2016, 8, 12, 15, 23, 28, 0, time.UTC)

	t.Run("documented_space_layout", func(t *testing.T) {
		got, ok := gitLabPipelineTimestamp(ctx, logger, "2016-08-12 15:23:28 UTC", "")
		if !ok || !got.Equal(docExample) {
			t.Fatalf("got (%v, %v), want (%v, true) — GitLab's documented Pipeline Hook example layout must parse", got, ok, docExample)
		}
	})
	t.Run("rfc3339", func(t *testing.T) {
		got, ok := gitLabPipelineTimestamp(ctx, logger, "2016-08-12T15:23:28Z", "")
		if !ok || !got.Equal(docExample) {
			t.Fatalf("got (%v, %v), want (%v, true)", got, ok, docExample)
		}
	})
	t.Run("finished_at_preferred_over_created_at", func(t *testing.T) {
		got, ok := gitLabPipelineTimestamp(ctx, logger, "2016-08-12 15:23:28 UTC", "2016-08-12 15:20:00 UTC")
		if !ok || !got.Equal(docExample) {
			t.Fatalf("got (%v, %v), want finished_at %v", got, ok, docExample)
		}
	})
	t.Run("created_at_when_finished_at_absent", func(t *testing.T) {
		got, ok := gitLabPipelineTimestamp(ctx, logger, "", "2016-08-12 15:23:28 UTC")
		if !ok || !got.Equal(docExample) {
			t.Fatalf("got (%v, %v), want created_at %v", got, ok, docExample)
		}
	})
	t.Run("unparseable_falls_back_to_now", func(t *testing.T) {
		before := time.Now().UTC().Add(-time.Second)
		got, ok := gitLabPipelineTimestamp(ctx, logger, "yesterday-ish", "12/08/2016")
		if ok {
			t.Fatalf("ok = true, want false (fallback)")
		}
		if got.Before(before) || got.After(time.Now().UTC().Add(time.Second)) {
			t.Fatalf("fallback ts %v is not ~now", got)
		}
	})
	t.Run("both_empty_falls_back_to_now", func(t *testing.T) {
		if _, ok := gitLabPipelineTimestamp(ctx, logger, "", ""); ok {
			t.Fatalf("ok = true, want false (fallback)")
		}
	})
}

// TestIngestGitLabPipeline_ProjectScopedMatchInputs asserts the ingester
// hands the repository the FULL project-scoped key — repo, installation_ref,
// sha AND the MR iid — and persists the pipeline id, the payload
// timestamp, and the mapped state on the appended row.
func TestIngestGitLabPipeline_ProjectScopedMatchInputs(t *testing.T) {
	fx := newPipelineFixture(t)
	_, _, reviewID := fx.addRun(gitLabSnapshot(false))
	body := makeGitLabPipelinePayload(4242, "acme/widgets", 100, "abc123", "success",
		"2026-09-19 10:00:00 UTC", "2026-09-19 10:05:00 UTC", 7)

	fx.srv.ingestGitLabPipeline(context.Background(), gitLabPipelineEvent("acme/widgets", "gitlab:4242", body))

	if n := len(fx.checks.gitlabMatchCalls); n != 1 {
		t.Fatalf("FindMatchingStagesForGitLabPipeline calls = %d, want 1", n)
	}
	got := fx.checks.gitlabMatchCalls[0]
	want := stagecheck.GitLabPipelineMatch{Repo: "acme/widgets", InstallationRef: "gitlab:4242", HeadSHA: "abc123", MergeRequestIID: 7}
	if got != want {
		t.Fatalf("match key = %+v, want %+v", got, want)
	}
	if n := len(fx.checks.appendCalls); n != 1 {
		t.Fatalf("Append calls = %d, want 1", n)
	}
	a := fx.checks.appendCalls[0]
	if a.StageID != reviewID || a.Name != webhook.GitLabPipelineCheckContext || a.Status != "completed" ||
		a.Conclusion == nil || *a.Conclusion != "success" || a.HeadSHA != "abc123" {
		t.Errorf("appended row = %+v, want review stage / gitlab/pipeline / completed / success / abc123", a)
	}
	if a.GitLabPipelineID == nil || *a.GitLabPipelineID != 100 {
		t.Errorf("GitLabPipelineID = %v, want 100", a.GitLabPipelineID)
	}
	if a.GitHubCheckRunID != nil {
		t.Errorf("GitHubCheckRunID = %v, want nil on a GitLab row", a.GitHubCheckRunID)
	}
	if wantTS := time.Date(2026, 9, 19, 10, 5, 0, 0, time.UTC); !a.Timestamp.Equal(wantTS) {
		t.Errorf("Timestamp = %v, want finished_at %v", a.Timestamp, wantTS)
	}
	if string(a.Payload) != string(body) {
		t.Errorf("Payload is not the verbatim hook body")
	}
}

// TestIngestGitLabPipeline_BranchPipelineMatchesWithoutIID pins the
// mr_iid=0 arm: a branch pipeline (no merge_request) still resolves by sha.
func TestIngestGitLabPipeline_BranchPipelineMatchesWithoutIID(t *testing.T) {
	fx := newPipelineFixture(t)
	fx.addRun(gitLabSnapshot(false))
	body := makeGitLabPipelinePayload(4242, "acme/widgets", 100, "abc123", "success", "", "", 0)
	fx.srv.ingestGitLabPipeline(context.Background(), gitLabPipelineEvent("acme/widgets", "gitlab:4242", body))
	if n := len(fx.checks.gitlabMatchCalls); n != 1 || fx.checks.gitlabMatchCalls[0].MergeRequestIID != 0 {
		t.Fatalf("match calls = %+v, want one with MergeRequestIID 0", fx.checks.gitlabMatchCalls)
	}
	if n := len(fx.checks.appendCalls); n != 1 {
		t.Fatalf("Append calls = %d, want 1", n)
	}
}

// TestIngestGitLabPipeline_SkipsWhenMatchKeyMissing is the per-failure-mode
// table for the pre-repository guards: a missing repo, credential ref, sha
// or non-positive pipeline id, and an unparseable body, each make NO
// repository call.
func TestIngestGitLabPipeline_SkipsWhenMatchKeyMissing(t *testing.T) {
	good := makeGitLabPipelinePayload(4242, "acme/widgets", 100, "abc123", "success", "", "", 7)
	cases := []struct {
		name string
		ev   webhook.Event
	}{
		{"repo_missing", gitLabPipelineEvent("", "gitlab:4242", good)},
		{"credential_ref_missing", gitLabPipelineEvent("acme/widgets", "", good)},
		{"sha_missing", gitLabPipelineEvent("acme/widgets", "gitlab:4242",
			makeGitLabPipelinePayload(4242, "acme/widgets", 100, "", "success", "", "", 7))},
		{"pipeline_id_zero", gitLabPipelineEvent("acme/widgets", "gitlab:4242",
			makeGitLabPipelinePayload(4242, "acme/widgets", 0, "abc123", "success", "", "", 7))},
		{"pipeline_id_negative", gitLabPipelineEvent("acme/widgets", "gitlab:4242",
			makeGitLabPipelinePayload(4242, "acme/widgets", -1, "abc123", "success", "", "", 7))},
		{"body_unparseable", gitLabPipelineEvent("acme/widgets", "gitlab:4242", []byte(`{not json`))},
		{"body_empty", gitLabPipelineEvent("acme/widgets", "gitlab:4242", nil)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fx := newPipelineFixture(t)
			fx.addRun(gitLabSnapshot(false))
			fx.srv.ingestGitLabPipeline(context.Background(), c.ev)
			if n := len(fx.checks.gitlabMatchCalls); n != 0 {
				t.Fatalf("FindMatchingStagesForGitLabPipeline calls = %d, want 0", n)
			}
			if n := len(fx.checks.appendCalls); n != 0 {
				t.Fatalf("Append calls = %d, want 0", n)
			}
		})
	}
}

// TestIngestGitLabPipeline_RepoUnwired pins the nil-repo early return.
func TestIngestGitLabPipeline_RepoUnwired(t *testing.T) {
	body := makeGitLabPipelinePayload(4242, "acme/widgets", 100, "abc123", "success", "", "", 7)
	for _, c := range []struct {
		name string
		cfg  Config
	}{
		{"no_stagecheck_repo", Config{Addr: "127.0.0.1:0", RunRepo: &pipelineRunRepo{reevalRunRepo: newReevalRunRepo()}}},
		{"no_run_repo", Config{Addr: "127.0.0.1:0", StageCheckRepo: newStageCheckRepoFake()}},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := New(c.cfg)
			// Must not panic and must not touch the (possibly nil) repos.
			s.ingestGitLabPipeline(context.Background(), gitLabPipelineEvent("acme/widgets", "gitlab:4242", body))
			if f, ok := c.cfg.StageCheckRepo.(*stageCheckRepoFake); ok && len(f.gitlabMatchCalls) != 0 {
				t.Fatalf("repo called with RunRepo unwired")
			}
		})
	}
}

// TestIngestGitLabPipeline_MatchErrorAndNoMatch pins the two post-parse
// degrade branches: a repository error and an empty match both append
// nothing and re-evaluate nothing.
func TestIngestGitLabPipeline_MatchErrorAndNoMatch(t *testing.T) {
	body := makeGitLabPipelinePayload(4242, "acme/widgets", 100, "abc123", "success", "", "", 7)
	t.Run("match_error", func(t *testing.T) {
		fx := newPipelineFixture(t)
		_, implID, _ := fx.addRun(gitLabSnapshot(false))
		fx.checks.matchingErr = errors.New("boom")
		fx.srv.ingestGitLabPipeline(context.Background(), gitLabPipelineEvent("acme/widgets", "gitlab:4242", body))
		if n := len(fx.checks.appendCalls); n != 0 {
			t.Fatalf("Append calls = %d, want 0", n)
		}
		if _, ok := fx.policyEvaluatedFor(t, implID); ok {
			t.Fatalf("policy_evaluated appended on a match error")
		}
	})
	t.Run("no_match", func(t *testing.T) {
		fx := newPipelineFixture(t)
		_, implID, _ := fx.addRun(gitLabSnapshot(false))
		fx.checks.gitlabMatches = nil
		fx.srv.ingestGitLabPipeline(context.Background(), gitLabPipelineEvent("acme/widgets", "gitlab:4242", body))
		if n := len(fx.checks.appendCalls); n != 0 {
			t.Fatalf("Append calls = %d, want 0", n)
		}
		if _, ok := fx.policyEvaluatedFor(t, implID); ok {
			t.Fatalf("policy_evaluated appended with no match")
		}
	})
}

// TestIngestGitLabPipeline_SkippedUsesRunSnapshotFlag pins the
// skipped-pipeline rule against the run's flag: on → pass row, off →
// skipped_not_allowed, nil snapshot → skipped_not_allowed (conservative),
// and a GetRun failure → skipped_not_allowed with the row still written.
func TestIngestGitLabPipeline_SkippedUsesRunSnapshotFlag(t *testing.T) {
	body := makeGitLabPipelinePayload(4242, "acme/widgets", 100, "abc123", "skipped", "", "", 7)
	cases := []struct {
		name      string
		snap      *run.RequiredChecksSnapshot
		getErr    error
		wantConcl string
	}{
		{"flag_on", gitLabSnapshot(true), nil, "skipped"},
		{"flag_off", gitLabSnapshot(false), nil, "skipped_not_allowed"},
		{"snapshot_nil", nil, nil, "skipped_not_allowed"},
		{"get_run_error", gitLabSnapshot(true), errors.New("db down"), "skipped_not_allowed"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fx := newPipelineFixture(t)
			fx.addRun(c.snap)
			fx.runs.getErr = c.getErr
			fx.srv.ingestGitLabPipeline(context.Background(), gitLabPipelineEvent("acme/widgets", "gitlab:4242", body))
			if n := len(fx.checks.appendCalls); n != 1 {
				t.Fatalf("Append calls = %d, want 1", n)
			}
			a := fx.checks.appendCalls[0]
			if a.Status != "completed" || a.Conclusion == nil || *a.Conclusion != c.wantConcl {
				t.Fatalf("row = %s/%v, want completed/%s", a.Status, a.Conclusion, c.wantConcl)
			}
		})
	}
}

// TestIngestGitLabPipeline_SkipsAppendForSupersededPipeline pins the
// write-avoidance optimisation: when the stage's latest gitlab/pipeline row
// carries a HIGHER id than the incoming event, Append is not called and no
// re-eval fires. An EQUAL or lower latest id still appends (status updates
// for the same pipeline must land).
func TestIngestGitLabPipeline_SkipsAppendForSupersededPipeline(t *testing.T) {
	seed := func(fx *pipelineFixture, reviewID uuid.UUID, latestID int64) {
		id := latestID
		fx.checks.seed(reviewID, &stagecheck.Check{
			StageID: reviewID, Name: webhook.GitLabPipelineCheckContext,
			State: stagecheck.StateFail, Status: "completed", Conclusion: ptrStr("failure"),
			GitLabPipelineID: &id,
		})
	}
	t.Run("older_incoming_skipped", func(t *testing.T) {
		fx := newPipelineFixture(t)
		_, implID, reviewID := fx.addRun(gitLabSnapshot(false))
		seed(fx, reviewID, 101)
		body := makeGitLabPipelinePayload(4242, "acme/widgets", 100, "abc123", "success", "", "", 7)
		fx.srv.ingestGitLabPipeline(context.Background(), gitLabPipelineEvent("acme/widgets", "gitlab:4242", body))
		if n := len(fx.checks.appendCalls); n != 0 {
			t.Fatalf("Append calls = %d, want 0 (pipeline 100 is superseded by 101)", n)
		}
		if _, ok := fx.policyEvaluatedFor(t, implID); ok {
			t.Fatalf("policy_evaluated appended for a superseded pipeline")
		}
	})
	t.Run("same_id_appends", func(t *testing.T) {
		fx := newPipelineFixture(t)
		_, _, reviewID := fx.addRun(gitLabSnapshot(false))
		seed(fx, reviewID, 100)
		body := makeGitLabPipelinePayload(4242, "acme/widgets", 100, "abc123", "success", "", "", 7)
		fx.srv.ingestGitLabPipeline(context.Background(), gitLabPipelineEvent("acme/widgets", "gitlab:4242", body))
		if n := len(fx.checks.appendCalls); n != 1 {
			t.Fatalf("Append calls = %d, want 1 (a status update for the same pipeline)", n)
		}
	})
	t.Run("newer_incoming_appends", func(t *testing.T) {
		fx := newPipelineFixture(t)
		_, _, reviewID := fx.addRun(gitLabSnapshot(false))
		seed(fx, reviewID, 100)
		body := makeGitLabPipelinePayload(4242, "acme/widgets", 101, "abc123", "success", "", "", 7)
		fx.srv.ingestGitLabPipeline(context.Background(), gitLabPipelineEvent("acme/widgets", "gitlab:4242", body))
		if n := len(fx.checks.appendCalls); n != 1 {
			t.Fatalf("Append calls = %d, want 1", n)
		}
	})
}

// TestIngestGitLabPipeline_ReevaluatesEveryMatchedRun: two StageRefs on
// two runs → a policy_evaluated row with ci_green=true for BOTH, not just
// the first match.
func TestIngestGitLabPipeline_ReevaluatesEveryMatchedRun(t *testing.T) {
	fx := newPipelineFixture(t)
	_, implA, _ := fx.addRun(gitLabSnapshot(false))
	_, implB, _ := fx.addRun(gitLabSnapshot(false))
	body := makeGitLabPipelinePayload(4242, "acme/widgets", 100, "abc123", "success", "", "", 7)

	fx.srv.ingestGitLabPipeline(context.Background(), gitLabPipelineEvent("acme/widgets", "gitlab:4242", body))

	if n := len(fx.checks.appendCalls); n != 2 {
		t.Fatalf("Append calls = %d, want 2", n)
	}
	for name, impl := range map[string]uuid.UUID{"A": implA, "B": implB} {
		g, ok := fx.policyEvaluatedFor(t, impl)
		if !ok {
			t.Fatalf("run %s: no policy_evaluated appended — every matched run must be re-evaluated", name)
		}
		if g == nil || !*g {
			t.Errorf("run %s: ci_green = %v, want &true", name, g)
		}
	}
}

// TestIngestGitLabPipeline_NonTerminalStatusNoReeval: a running pipeline
// writes the in_progress row but does NOT re-evaluate — matching the GitHub
// path, where only a `completed` check_run re-fires the evaluator. The
// prior evaluation is seeded ci_green=false (a failed pipeline) so that a
// re-eval WOULD emit (aggregate nil ≠ false); with the terminal-only guard
// in place no row is appended.
func TestIngestGitLabPipeline_NonTerminalStatusNoReeval(t *testing.T) {
	fx := newPipelineFixture(t)
	runID, implID, _ := fx.addRun(gitLabSnapshot(false))
	f := false
	fx.audit.seedPolicyEvaluated(runID, implID, policy.EvaluationPayload{
		StageType: "implement",
		Diff:      []policy.DiffEntry{{Path: "x.go", Status: policy.Status("modified")}},
		Applied:   policy.Constraints{RequiredOutcomes: []string{"ci_green"}, CIGreen: &f},
		Passed:    false,
	})
	body := makeGitLabPipelinePayload(4242, "acme/widgets", 101, "abc123", "running", "", "", 7)
	fx.srv.ingestGitLabPipeline(context.Background(), gitLabPipelineEvent("acme/widgets", "gitlab:4242", body))
	if n := len(fx.checks.appendCalls); n != 1 || fx.checks.appendCalls[0].Status != "in_progress" {
		t.Fatalf("Append calls = %+v, want one in_progress row", fx.checks.appendCalls)
	}
	if g, ok := fx.policyEvaluatedFor(t, implID); ok {
		t.Fatalf("policy_evaluated appended (ci_green=%v) for a non-terminal pipeline; only a terminal status re-fires the evaluator", g)
	}
}

// TestIngestGitLabPipeline_AppendErrorContinues: an Append failure on the
// first stage neither aborts the second nor re-evaluates the failed run.
func TestIngestGitLabPipeline_AppendErrorContinues(t *testing.T) {
	fx := newPipelineFixture(t)
	_, implA, reviewA := fx.addRun(gitLabSnapshot(false))
	_, implB, _ := fx.addRun(gitLabSnapshot(false))
	failing := &failFirstAppendRepo{pipelineCheckRepo: &pipelineCheckRepo{stageCheckRepoFake: fx.checks}, failStage: reviewA}
	fx.srv = New(Config{Addr: "127.0.0.1:0", RunRepo: fx.runs, AuditRepo: fx.audit, StageCheckRepo: failing})
	body := makeGitLabPipelinePayload(4242, "acme/widgets", 100, "abc123", "success", "", "", 7)

	fx.srv.ingestGitLabPipeline(context.Background(), gitLabPipelineEvent("acme/widgets", "gitlab:4242", body))

	if _, ok := fx.policyEvaluatedFor(t, implA); ok {
		t.Fatalf("run A re-evaluated although its Append failed")
	}
	if g, ok := fx.policyEvaluatedFor(t, implB); !ok || g == nil || !*g {
		t.Fatalf("run B: policy_evaluated = (%v, %v), want (&true, true) — a sibling's Append failure must not abort the loop", g, ok)
	}
}

// failFirstAppendRepo fails Append for one stage id and delegates the rest.
type failFirstAppendRepo struct {
	*pipelineCheckRepo
	failStage uuid.UUID
}

func (f *failFirstAppendRepo) Append(ctx context.Context, p stagecheck.AppendParams) (*stagecheck.Check, error) {
	if p.StageID == f.failStage {
		return nil, errors.New("append boom")
	}
	return f.pipelineCheckRepo.Append(ctx, p)
}
