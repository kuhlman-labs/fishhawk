package stagecheck_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/stagecheck"
)

func ptr[T any](v T) *T { return &v }

// derefID renders a nullable pipeline id for failure messages.
func derefID(p *int64) any {
	if p == nil {
		return "nil"
	}
	return *p
}

// seedStage creates a run + stage in one shot so the stage_checks
// FK to stages.id resolves.
func seedStage(t *testing.T, pool *pgxpool.Pool) (runID, stageID uuid.UUID) {
	t.Helper()
	repo := run.NewPostgresRepository(pool)
	r, err := repo.CreateRun(context.Background(), run.CreateRunParams{
		Repo:          "kuhlman-labs/fishhawk",
		WorkflowID:    "feature_change",
		WorkflowSHA:   "deadbeef",
		TriggerSource: run.TriggerCLI,
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	s, err := repo.CreateStage(context.Background(), run.CreateStageParams{
		RunID:        r.ID,
		Sequence:     0,
		Type:         run.StageTypeReview,
		ExecutorKind: run.ExecutorHuman,
		ExecutorRef:  "human",
		Gate: &run.Gate{
			Kind: run.GateKindApproval,
		},
	})
	if err != nil {
		t.Fatalf("create stage: %v", err)
	}
	return r.ID, s.ID
}

func TestAppend_LatestForStageAndName_RoundTrip(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := stagecheck.NewPostgresRepository(pool)
	_, stageID := seedStage(t, pool)

	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	if _, err := repo.Append(context.Background(), stagecheck.AppendParams{
		StageID:    stageID,
		Name:       "ci_pass",
		Status:     "completed",
		Conclusion: ptr("success"),
		HeadSHA:    "abc123",
		Timestamp:  now,
		Payload:    json.RawMessage(`{"foo":"bar"}`),
	}); err != nil {
		t.Fatal(err)
	}

	got, err := repo.LatestForStageAndName(context.Background(), stageID, "ci_pass")
	if err != nil {
		t.Fatalf("LatestForStageAndName: %v", err)
	}
	if got.State != stagecheck.StatePass {
		t.Errorf("State = %q, want pass", got.State)
	}
	if got.HeadSHA != "abc123" {
		t.Errorf("HeadSHA = %q", got.HeadSHA)
	}
	if !strings.Contains(string(got.Payload), `"foo"`) || !strings.Contains(string(got.Payload), `"bar"`) {
		t.Errorf("Payload not preserved: %s", got.Payload)
	}
}

func TestLatestForStageAndName_NotFound(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := stagecheck.NewPostgresRepository(pool)
	_, stageID := seedStage(t, pool)

	_, err := repo.LatestForStageAndName(context.Background(), stageID, "ci_pass")
	if !errors.Is(err, stagecheck.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestLatestForStageAndName_PicksMostRecent(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := stagecheck.NewPostgresRepository(pool)
	_, stageID := seedStage(t, pool)

	older := time.Date(2026, 5, 7, 11, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)

	// CI started in_progress, then completed as failure. Latest
	// row wins; the gate should refuse approval.
	if _, err := repo.Append(context.Background(), stagecheck.AppendParams{
		StageID: stageID, Name: "ci_pass",
		Status: "in_progress", HeadSHA: "abc", Timestamp: older,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Append(context.Background(), stagecheck.AppendParams{
		StageID: stageID, Name: "ci_pass",
		Status: "completed", Conclusion: ptr("failure"),
		HeadSHA: "abc", Timestamp: newer,
	}); err != nil {
		t.Fatal(err)
	}

	got, err := repo.LatestForStageAndName(context.Background(), stageID, "ci_pass")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != stagecheck.StateFail {
		t.Errorf("State = %q, want fail (latest row wins)", got.State)
	}
}

func TestLatestForStage_OneRowPerCheckName(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := stagecheck.NewPostgresRepository(pool)
	_, stageID := seedStage(t, pool)

	now := time.Now().UTC()
	for _, name := range []string{"ci_pass", "fishhawk_audit_complete"} {
		for i := 0; i < 3; i++ {
			if _, err := repo.Append(context.Background(), stagecheck.AppendParams{
				StageID: stageID, Name: name,
				Status:     "completed",
				Conclusion: ptr("success"),
				HeadSHA:    "abc",
				Timestamp:  now.Add(time.Duration(i) * time.Second),
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	out, err := repo.LatestForStage(context.Background(), stageID)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 {
		t.Errorf("len = %d, want 2 (ci_pass + fishhawk_audit_complete)", len(out))
	}
}

func TestFindMatchingStages_FiltersByPRAndCheck(t *testing.T) {
	pool := pgtest.NewPool(t)
	scRepo := stagecheck.NewPostgresRepository(pool)
	runRepo := run.NewPostgresRepository(pool)
	artRepo := artifact.NewPostgresRepository(pool)

	// Create a run with two stages: implement (carries the PR
	// artifact) + review (carries the gate).
	r, err := runRepo.CreateRun(context.Background(), run.CreateRunParams{
		Repo: "x/y", WorkflowID: "feature_change", WorkflowSHA: "deadbeef", TriggerSource: run.TriggerCLI,
	})
	if err != nil {
		t.Fatal(err)
	}
	implStage, err := runRepo.CreateStage(context.Background(), run.CreateStageParams{
		RunID: r.ID, Sequence: 0, Type: run.StageTypeImplement,
		ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code",
	})
	if err != nil {
		t.Fatal(err)
	}
	reviewStage, err := runRepo.CreateStage(context.Background(), run.CreateStageParams{
		RunID: r.ID, Sequence: 1, Type: run.StageTypeReview,
		ExecutorKind: run.ExecutorHuman, ExecutorRef: "human",
		Gate: &run.Gate{
			Kind: run.GateKindApproval,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	prBody, _ := json.Marshal(map[string]any{
		"pr_number":           42,
		"head_sha":            "abc123",
		"branch":              "feat",
		"base_sha":            "def456",
		"title":               "x",
		"files_changed_count": 1,
	})
	if _, err := artRepo.Create(context.Background(), artifact.CreateParams{
		StageID:     implStage.ID,
		Kind:        artifact.KindPlan, // see note below
		Content:     prBody,
		ContentHash: "hashplan",
	}); err != nil {
		// We expect this insert to be rejected if the kind is
		// validated; ignore the artifact-validation result and
		// proceed with the real (kind=pull_request) insert.
		_ = err
	}
	prArt, err := artRepo.Create(context.Background(), artifact.CreateParams{
		StageID:     implStage.ID,
		Kind:        artifact.KindPullRequest,
		Content:     prBody,
		ContentHash: "hashpr",
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = prArt

	// Match by (pr=42, sha=abc123, check=ci_pass) → review stage.
	stages, err := scRepo.FindMatchingStages(context.Background(), 42, "abc123", "ci_pass")
	if err != nil {
		t.Fatal(err)
	}
	if len(stages) != 1 || stages[0] != reviewStage.ID {
		t.Errorf("expected [%v], got %v", reviewStage.ID, stages)
	}

	// Wrong PR number: no match.
	stages, _ = scRepo.FindMatchingStages(context.Background(), 99, "abc123", "ci_pass")
	if len(stages) != 0 {
		t.Errorf("expected empty, got %v", stages)
	}
	// Wrong head_sha: no match.
	stages, _ = scRepo.FindMatchingStages(context.Background(), 42, "ffffff", "ci_pass")
	if len(stages) != 0 {
		t.Errorf("expected empty, got %v", stages)
	}
	// Empty check name is a no-op (the SQL guard against the empty
	// string from the migrated-away gate.blocking_checks shape).
	stages, _ = scRepo.FindMatchingStages(context.Background(), 42, "abc123", "")
	if len(stages) != 0 {
		t.Errorf("empty check_name should match nothing, got %v", stages)
	}

	// Post-#254 (ADR-017): the query matches by stage_type = 'review'
	// rather than the dropped gate.blocking_checks list. Any check
	// name reported against a review stage's PR head_sha records,
	// since branch protection is now the authority on which checks
	// matter — Fishhawk just records what GitHub tells us.
	stages, _ = scRepo.FindMatchingStages(context.Background(), 42, "abc123", "some_other")
	if len(stages) != 1 || stages[0] != reviewStage.ID {
		t.Errorf("post-#254 the check_name no longer filters; got %v want [%v]",
			stages, reviewStage.ID)
	}
}

// TestAppend_GitLabPipelineID_RoundTrip pins that the nullable
// gitlab_pipeline_id column (migration 0084, E45.55 / #3490) round-trips
// through Append → both readers in BOTH states: nil stays nil (a GitHub
// row) and a set id comes back verbatim. Two distinct check names so
// each row is the latest of its own name.
func TestAppend_GitLabPipelineID_RoundTrip(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := stagecheck.NewPostgresRepository(pool)
	_, stageID := seedStage(t, pool)
	ctx := context.Background()
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)

	appended, err := repo.Append(ctx, stagecheck.AppendParams{
		StageID: stageID, Name: "gitlab/pipeline",
		Status: "completed", Conclusion: ptr("success"),
		HeadSHA: "abc123", GitLabPipelineID: ptr(int64(4242)), Timestamp: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if appended.GitLabPipelineID == nil || *appended.GitLabPipelineID != 4242 {
		t.Errorf("Append returned GitLabPipelineID = %v, want 4242", derefID(appended.GitLabPipelineID))
	}
	if _, err := repo.Append(ctx, stagecheck.AppendParams{
		StageID: stageID, Name: "ci_pass",
		Status: "completed", Conclusion: ptr("success"),
		HeadSHA: "abc123", GitHubCheckRunID: ptr(int64(7)), Timestamp: now,
	}); err != nil {
		t.Fatal(err)
	}

	gl, err := repo.LatestForStageAndName(ctx, stageID, "gitlab/pipeline")
	if err != nil {
		t.Fatal(err)
	}
	if gl.GitLabPipelineID == nil || *gl.GitLabPipelineID != 4242 {
		t.Errorf("LatestForStageAndName GitLabPipelineID = %v, want 4242", derefID(gl.GitLabPipelineID))
	}
	gh, err := repo.LatestForStageAndName(ctx, stageID, "ci_pass")
	if err != nil {
		t.Fatal(err)
	}
	if gh.GitLabPipelineID != nil {
		t.Errorf("GitHub row GitLabPipelineID = %d, want nil", *gh.GitLabPipelineID)
	}
	if gh.GitHubCheckRunID == nil || *gh.GitHubCheckRunID != 7 {
		t.Errorf("GitHub row GitHubCheckRunID = %v, want 7 (unchanged by the new column)", gh.GitHubCheckRunID)
	}

	all, err := repo.LatestForStage(ctx, stageID)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]*stagecheck.Check{}
	for _, c := range all {
		byName[c.Name] = c
	}
	if c := byName["gitlab/pipeline"]; c == nil || c.GitLabPipelineID == nil || *c.GitLabPipelineID != 4242 {
		t.Errorf("LatestForStage gitlab/pipeline = %+v, want GitLabPipelineID 4242", c)
	}
	if c := byName["ci_pass"]; c == nil || c.GitLabPipelineID != nil {
		t.Errorf("LatestForStage ci_pass = %+v, want nil GitLabPipelineID", c)
	}
}

// TestLatestForStageAndName_GitLabPipelineIDPrecedence pins the SHIPPED
// ORDER BY on both latest-row readers (E45.55 / #3490):
// `gitlab_pipeline_id DESC NULLS LAST, ts DESC`. Within
// (stage, gitlab/pipeline) the HIGHEST pipeline id is the latest row
// regardless of ts or append order — the same head_sha can carry several
// pipelines (retry / manual re-run) and GitLab ids are monotonically
// increasing. Seeded adversarially: id 100 carries the LATER ts and is
// appended LAST, so a ts-ordered reader (the pre-#3490 clause) or an
// append-order reader would return 100; only the id key returns 101.
// GitHub rows carry NULL and sort last on that key, so a NULL-id check
// name is still pure-ts ordered — asserted on the same stage so both halves
// run against one ORDER BY. Runs against the Go query const in
// db/queries.sql.go (what executes), so editing only queries.sql reddens it.
func TestLatestForStageAndName_GitLabPipelineIDPrecedence(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := stagecheck.NewPostgresRepository(pool)
	_, stageID := seedStage(t, pool)
	ctx := context.Background()

	t1 := time.Date(2026, 9, 19, 11, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)

	// Pipeline 101 (the newer pipeline) failed, recorded FIRST with the
	// EARLIER ts; pipeline 100 succeeded, recorded LAST with the LATER
	// ts. Highest id must win → fail.
	if _, err := repo.Append(ctx, stagecheck.AppendParams{
		StageID: stageID, Name: "gitlab/pipeline",
		Status: "completed", Conclusion: ptr("failure"),
		HeadSHA: "abc", GitLabPipelineID: ptr(int64(101)), Timestamp: t1,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Append(ctx, stagecheck.AppendParams{
		StageID: stageID, Name: "gitlab/pipeline",
		Status: "completed", Conclusion: ptr("success"),
		HeadSHA: "abc", GitLabPipelineID: ptr(int64(100)), Timestamp: t2,
	}); err != nil {
		t.Fatal(err)
	}
	// GitHub rows under another name: two NULL-id rows, the later ts
	// appended FIRST so append order cannot masquerade as ts order.
	if _, err := repo.Append(ctx, stagecheck.AppendParams{
		StageID: stageID, Name: "ci_pass",
		Status: "completed", Conclusion: ptr("success"),
		HeadSHA: "abc", Timestamp: t2,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Append(ctx, stagecheck.AppendParams{
		StageID: stageID, Name: "ci_pass",
		Status: "completed", Conclusion: ptr("failure"),
		HeadSHA: "abc", Timestamp: t1,
	}); err != nil {
		t.Fatal(err)
	}

	// GetStageCheckLatest half.
	gl, err := repo.LatestForStageAndName(ctx, stageID, "gitlab/pipeline")
	if err != nil {
		t.Fatal(err)
	}
	if gl.GitLabPipelineID == nil || *gl.GitLabPipelineID != 101 {
		t.Errorf("LatestForStageAndName(gitlab/pipeline) GitLabPipelineID = %v, want 101 (highest id wins over later ts)", derefID(gl.GitLabPipelineID))
	}
	if gl.State != stagecheck.StateFail {
		t.Errorf("LatestForStageAndName(gitlab/pipeline) State = %q, want fail", gl.State)
	}
	gh, err := repo.LatestForStageAndName(ctx, stageID, "ci_pass")
	if err != nil {
		t.Fatal(err)
	}
	if gh.State != stagecheck.StatePass || !gh.Timestamp.Equal(t2) {
		t.Errorf("LatestForStageAndName(ci_pass) = state %q ts %v, want pass at %v (NULL-id rows stay ts-ordered)", gh.State, gh.Timestamp, t2)
	}

	// ListStageChecksLatest half — the DISTINCT ON reader.
	all, err := repo.LatestForStage(ctx, stageID)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("LatestForStage len = %d, want 2", len(all))
	}
	for _, c := range all {
		switch c.Name {
		case "gitlab/pipeline":
			if c.GitLabPipelineID == nil || *c.GitLabPipelineID != 101 || c.State != stagecheck.StateFail {
				t.Errorf("LatestForStage gitlab/pipeline = id %v state %q, want 101 fail", derefID(c.GitLabPipelineID), c.State)
			}
		case "ci_pass":
			if c.State != stagecheck.StatePass || !c.Timestamp.Equal(t2) {
				t.Errorf("LatestForStage ci_pass = state %q ts %v, want pass at %v", c.State, c.Timestamp, t2)
			}
		default:
			t.Errorf("unexpected check name %q", c.Name)
		}
	}
}

// seedGitLabRun creates a run pinned to (repo, installationRef) with an
// implement stage carrying a pull_request artifact (prNumber, headSHA)
// and a review stage. Returns the run id and the review stage id.
func seedGitLabRun(t *testing.T, pool *pgxpool.Pool, repoPath, installationRef, headSHA string, prNumber int) (runID, reviewStageID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	runRepo := run.NewPostgresRepository(pool)
	artRepo := artifact.NewPostgresRepository(pool)
	r, err := runRepo.CreateRun(ctx, run.CreateRunParams{
		Repo: repoPath, WorkflowID: "feature_change", WorkflowSHA: "deadbeef",
		TriggerSource: run.TriggerCLI, InstallationRef: ptr(installationRef),
	})
	if err != nil {
		t.Fatalf("create run %s/%s: %v", repoPath, installationRef, err)
	}
	implStage, err := runRepo.CreateStage(ctx, run.CreateStageParams{
		RunID: r.ID, Sequence: 0, Type: run.StageTypeImplement,
		ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code",
	})
	if err != nil {
		t.Fatal(err)
	}
	reviewStage, err := runRepo.CreateStage(ctx, run.CreateStageParams{
		RunID: r.ID, Sequence: 1, Type: run.StageTypeReview,
		ExecutorKind: run.ExecutorHuman, ExecutorRef: "human",
		Gate: &run.Gate{Kind: run.GateKindApproval},
	})
	if err != nil {
		t.Fatal(err)
	}
	prBody, _ := json.Marshal(map[string]any{
		"pr_number": prNumber, "head_sha": headSHA, "branch": "feat",
		"base_sha": "def456", "title": "x", "files_changed_count": 1,
	})
	if _, err := artRepo.Create(ctx, artifact.CreateParams{
		StageID: implStage.ID, Kind: artifact.KindPullRequest,
		Content: prBody, ContentHash: "hashpr-" + r.ID.String(),
	}); err != nil {
		t.Fatal(err)
	}
	return r.ID, reviewStage.ID
}

// TestFindMatchingStagesForGitLabPipeline_ProjectScoped pins that BOTH
// runs.repo AND runs.installation_ref are load-bearing predicates
// (E45.55 / #3490). Four runs share the SAME pull_request artifact
// (head_sha abc123, pr_number 7): the target (acme/widgets, gitlab:4242)
// and THREE decoys — same repo + different ref, different repo + SAME
// ref, and both different. Each predicate has a decoy only IT excludes:
// deleting r.repo admits the (fork/widgets, gitlab:4242) decoy; deleting
// r.installation_ref admits the (acme/widgets, gitlab:9999) decoy; the
// both-different decoy is excluded by either. The test reads the
// COMMITTED match set, so a predicate that fires and is then dropped
// cannot leave it green.
func TestFindMatchingStagesForGitLabPipeline_ProjectScoped(t *testing.T) {
	pool := pgtest.NewPool(t)
	scRepo := stagecheck.NewPostgresRepository(pool)
	ctx := context.Background()

	targetRun, targetReview := seedGitLabRun(t, pool, "acme/widgets", "gitlab:4242", "abc123", 7)
	_, sameRepoOtherRef := seedGitLabRun(t, pool, "acme/widgets", "gitlab:9999", "abc123", 7)
	_, otherRepoSameRef := seedGitLabRun(t, pool, "fork/widgets", "gitlab:4242", "abc123", 7)
	_, bothOther := seedGitLabRun(t, pool, "fork/widgets", "gitlab:9999", "abc123", 7)

	find := func(m stagecheck.GitLabPipelineMatch) []stagecheck.StageRef {
		t.Helper()
		refs, err := scRepo.FindMatchingStagesForGitLabPipeline(ctx, m)
		if err != nil {
			t.Fatalf("FindMatchingStagesForGitLabPipeline(%+v): %v", m, err)
		}
		return refs
	}
	decoys := map[uuid.UUID]string{
		sameRepoOtherRef: "same-repo-different-ref (only the r.installation_ref predicate excludes it)",
		otherRepoSameRef: "different-repo-same-ref (only the r.repo predicate excludes it)",
		bothOther:        "both-different",
	}
	assertOnlyTarget := func(label string, refs []stagecheck.StageRef) {
		t.Helper()
		sawTarget := false
		for _, ref := range refs {
			if ref.StageID == targetReview && ref.RunID == targetRun {
				sawTarget = true
				continue
			}
			if name, ok := decoys[ref.StageID]; ok {
				t.Errorf("%s: matched decoy %s", label, name)
				continue
			}
			t.Errorf("%s: unexpected ref %+v", label, ref)
		}
		if !sawTarget {
			t.Errorf("%s: target review stage %v missing from %+v", label, targetReview, refs)
		}
		if len(refs) != 1 {
			t.Errorf("%s: got %d refs, want exactly the target review stage", label, len(refs))
		}
	}

	// Full key with the MR iid.
	assertOnlyTarget("mr_iid 7", find(stagecheck.GitLabPipelineMatch{
		Repo: "acme/widgets", InstallationRef: "gitlab:4242", HeadSHA: "abc123", MergeRequestIID: 7,
	}))
	// mr_iid 0 disables the pr_number predicate; project scoping still
	// holds.
	assertOnlyTarget("mr_iid 0", find(stagecheck.GitLabPipelineMatch{
		Repo: "acme/widgets", InstallationRef: "gitlab:4242", HeadSHA: "abc123",
	}))

	// Each decoy key, asked for DIRECTLY, resolves to its own run only —
	// proves the decoys are real, matchable runs and not fixture noise.
	for _, c := range []struct {
		repo, ref string
		want      uuid.UUID
	}{
		{"acme/widgets", "gitlab:9999", sameRepoOtherRef},
		{"fork/widgets", "gitlab:4242", otherRepoSameRef},
		{"fork/widgets", "gitlab:9999", bothOther},
	} {
		refs := find(stagecheck.GitLabPipelineMatch{Repo: c.repo, InstallationRef: c.ref, HeadSHA: "abc123", MergeRequestIID: 7})
		if len(refs) != 1 || refs[0].StageID != c.want {
			t.Errorf("(%s, %s): got %+v, want only stage %v", c.repo, c.ref, refs, c.want)
		}
	}

	// Wrong iid, wrong sha, wrong repo, wrong ref: nothing.
	for _, m := range []stagecheck.GitLabPipelineMatch{
		{Repo: "acme/widgets", InstallationRef: "gitlab:4242", HeadSHA: "abc123", MergeRequestIID: 8},
		{Repo: "acme/widgets", InstallationRef: "gitlab:4242", HeadSHA: "ffffff", MergeRequestIID: 7},
		{Repo: "acme/gadgets", InstallationRef: "gitlab:4242", HeadSHA: "abc123", MergeRequestIID: 7},
		{Repo: "acme/widgets", InstallationRef: "gitlab:1", HeadSHA: "abc123", MergeRequestIID: 7},
		{Repo: "acme/widgets", InstallationRef: "", HeadSHA: "abc123", MergeRequestIID: 7},
	} {
		if refs := find(m); len(refs) != 0 {
			t.Errorf("%+v: got %+v, want none", m, refs)
		}
	}
}
