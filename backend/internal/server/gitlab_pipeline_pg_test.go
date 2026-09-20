package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	forgegitlab "github.com/kuhlman-labs/fishhawk/backend/internal/forge/gitlab"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/policy"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/stagecheck"
	"github.com/kuhlman-labs/fishhawk/backend/internal/webhook"
)

// gitlab_pipeline_pg_test.go is the CROSS-BOUNDARY proof for E45.55 /
// #3490 — Projects API → forge adapter → webhook dispatcher → Postgres run
// row (snapshot round-trip) → Pipeline Hook ingester → real stagecheck SQL
// (the project-scoped match predicates AND the `gitlab_pipeline_id DESC
// NULLS LAST` ordering, not a fake) → deployCIGreenVerdict and the
// per-run policy re-eval. Every run row below is INSERTed by the real
// webhook.Dispatcher from an admitted GitLab issue trigger; nothing is
// hand-shaped.

const pgGitLabSpec = `version: "0.3"
roles:
  tech_lead:
    members: ["@kuhlman-labs"]
workflows:
  feature_change:
    description: Two-stage GitLab workflow
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
      - id: review
        type: review
        executor:
          agent: claude-code
`

type pgGitLabSpecFetcher struct{}

func (pgGitLabSpecFetcher) FetchFile(_ context.Context, _ forge.CredentialScope,
	_ forge.RepoRef, path, _ string) (*forge.FileContent, error) {
	return &forge.FileContent{Path: path, Content: []byte(pgGitLabSpec), SHA: "g1t1absha"}, nil
}

// pgGitLabRegistry vouches for an explicit set of (credential ref, project
// path) pairs so one dispatcher can mint the target run AND its decoys.
type pgGitLabRegistry struct{ pairs map[string]bool }

func (r pgGitLabRegistry) AuthorizedGitLabProject(_ context.Context, credentialRef, projectPath string) (bool, error) {
	return r.pairs[credentialRef+"|"+projectPath], nil
}

// pgStubCIReader is the stub forge.CIRequirementReader for the OFF / nil /
// error arms.
type pgStubCIReader struct {
	req *forge.CIRequirement
	err error
}

func (s pgStubCIReader) ReadCIRequirement(context.Context, forge.CredentialScope, forge.RepoRef) (*forge.CIRequirement, error) {
	return s.req, s.err
}

// pgProjectsForge builds the REAL *forgegitlab.Forge against an httptest
// server serving GET /api/v4/projects/:id with the two merge-requirement
// settings (approval condition 5). It records every request path so the
// test can prove the read addressed the run's project id.
type pgProjectsForge struct {
	f     *forgegitlab.Forge
	mu    sync.Mutex
	paths []string
}

func newPGProjectsForge(t *testing.T, succeeds, allowSkipped bool) *pgProjectsForge {
	t.Helper()
	pf := &pgProjectsForge{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v4/projects/{id}", func(w http.ResponseWriter, r *http.Request) {
		pf.mu.Lock()
		pf.paths = append(pf.paths, r.URL.Path)
		pf.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, fmt.Sprintf(
			`{"id":%s,"path_with_namespace":"acme/widgets","only_allow_merge_if_pipeline_succeeds":%t,"allow_merge_on_skipped_pipeline":%t}`,
			r.PathValue("id"), succeeds, allowSkipped))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	pf.f = forgegitlab.New(srv.URL, forgegitlab.NewStaticCredentialProvider("glpat-test"),
		forgegitlab.WithHTTPClient(srv.Client()))
	return pf
}

// gitLabPipelinePGFixture holds the real repositories, the dispatcher and
// the server under test.
type gitLabPipelinePGFixture struct {
	pool      *pgxpool.Pool
	runRepo   run.Repository
	artRepo   artifact.Repository
	checkRepo stagecheck.Repository
	auditRepo audit.Repository
	d         *webhook.Dispatcher
	s         *Server
}

func newGitLabPipelinePGFixture(t *testing.T, reader forge.CIRequirementReader) *gitLabPipelinePGFixture {
	t.Helper()
	pool := pgtest.NewPool(t)
	runRepo := run.NewPostgresRepository(pool)
	auditRepo := audit.NewPostgresRepository(pool)
	fx := &gitLabPipelinePGFixture{
		pool: pool, runRepo: runRepo,
		artRepo:   artifact.NewPostgresRepository(pool),
		checkRepo: stagecheck.NewPostgresRepository(pool),
		auditRepo: auditRepo,
	}
	fx.d = &webhook.Dispatcher{
		Runs:        runRepo,
		Audit:       auditRepo,
		GitLabFiles: pgGitLabSpecFetcher{},
		GitLabProjects: pgGitLabRegistry{pairs: map[string]bool{
			"gitlab:4242|acme/widgets": true, // the target project
			"gitlab:9999|acme/widgets": true, // decoy: same repo, different ref
			"gitlab:4242|fork/widgets": true, // decoy: different repo, SAME ref
			"gitlab:7777|fork/other":   true, // decoy: both different
		}},
		GitLabCIRequirements:   reader,
		PlanReviewerConfigured: true,
	}
	fx.s = New(Config{
		Addr:           "127.0.0.1:0",
		RunRepo:        runRepo,
		AuditRepo:      auditRepo,
		StageCheckRepo: fx.checkRepo,
	})
	return fx
}

// pgRun is one dispatcher-minted run with its two stages.
type pgRun struct {
	run           *run.Run
	implStageID   uuid.UUID
	reviewStageID uuid.UUID
}

// mintRun drives an admitted issue trigger for (repo, credentialRef) through
// the REAL dispatcher, reloads the run from Postgres, seeds the implement
// stage's pull_request artifact at (mrIID, headSHA) and a prior deferred
// policy_evaluated row, and returns the hydrated run.
func (fx *gitLabPipelinePGFixture) mintRun(t *testing.T, repo, credentialRef string, issueIID, mrIID int, headSHA string) pgRun {
	t.Helper()
	ctx := context.Background()
	before, err := fx.runRepo.ListRuns(ctx, run.ListRunsFilter{Repo: repo, Limit: 100})
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	seen := map[uuid.UUID]bool{}
	for _, r := range before {
		seen[r.ID] = true
	}
	ev := webhook.Event{
		Type:          "issue",
		DeliveryID:    "gitlab:" + uuid.NewString(),
		Repo:          repo,
		Sender:        "alice",
		Forge:         webhook.ForgeGitLab,
		CredentialRef: credentialRef,
		RawBody: []byte(fmt.Sprintf(`{"object_attributes":{"iid":%d,"action":"open"},`+
			`"labels":[{"title":"fishhawk"}]}`, issueIID)),
	}
	if err := fx.d.Handle(ctx, ev); err != nil {
		t.Fatalf("webhook Handle: %v", err)
	}
	after, err := fx.runRepo.ListRuns(ctx, run.ListRunsFilter{Repo: repo, Limit: 100})
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	var minted *run.Run
	for _, r := range after {
		if !seen[r.ID] {
			if minted != nil {
				t.Fatalf("trigger minted more than one run")
			}
			minted = r
		}
	}
	if minted == nil {
		t.Fatalf("trigger minted no run for %s / %s", repo, credentialRef)
	}
	if minted.InstallationRef == nil || *minted.InstallationRef != credentialRef {
		t.Fatalf("persisted installation_ref = %v, want %q", minted.InstallationRef, credentialRef)
	}
	stages, err := fx.runRepo.ListStagesForRun(ctx, minted.ID)
	if err != nil {
		t.Fatalf("list stages: %v", err)
	}
	if len(stages) != 2 || stages[0].Type != run.StageTypeImplement || stages[1].Type != run.StageTypeReview {
		t.Fatalf("persisted stages = %d, want implement + review", len(stages))
	}
	pr := pgRun{run: minted, implStageID: stages[0].ID, reviewStageID: stages[1].ID}

	prBody, _ := json.Marshal(map[string]any{
		"pr_number": mrIID, "head_sha": headSHA, "branch": "feat", "base_sha": "def456",
		"title": "x", "files_changed_count": 1,
	})
	if _, err := fx.artRepo.Create(ctx, artifact.CreateParams{
		StageID: pr.implStageID, Kind: artifact.KindPullRequest, Content: prBody, ContentHash: "hashpr-" + uuid.NewString(),
	}); err != nil {
		t.Fatalf("artifact Create: %v", err)
	}
	if _, err := policy.EmitEvaluation(ctx, fx.auditRepo, minted.ID, pr.implStageID, "implement",
		policy.Diff{ChangedFiles: []policy.ChangedFile{{Path: "x.go", Status: policy.Status("modified")}}},
		policy.Constraints{RequiredOutcomes: []string{"ci_green"}}, nil); err != nil {
		t.Fatalf("EmitEvaluation: %v", err)
	}
	return pr
}

// reload re-reads the run row so every assertion is on persisted state.
func (fx *gitLabPipelinePGFixture) reload(t *testing.T, id uuid.UUID) *run.Run {
	t.Helper()
	r, err := fx.runRepo.GetRun(context.Background(), id)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	return r
}

// rawSnapshot returns the JSONB text of runs.required_checks_snapshot so
// the explicit-false serialization can be asserted (approval condition 2).
func (fx *gitLabPipelinePGFixture) rawSnapshot(t *testing.T, id uuid.UUID) string {
	t.Helper()
	var raw *string
	if err := fx.pool.QueryRow(context.Background(),
		`SELECT required_checks_snapshot::text FROM runs WHERE id = $1`, id).Scan(&raw); err != nil {
		t.Fatalf("read raw snapshot: %v", err)
	}
	if raw == nil {
		return ""
	}
	return *raw
}

// deliver drives one Pipeline Hook through the real ingester under the
// given (repo, credentialRef).
func (fx *gitLabPipelinePGFixture) deliver(t *testing.T, repo, credentialRef string, pipelineID int64, sha, status, createdAt, finishedAt string, mrIID int) {
	t.Helper()
	body := makeGitLabPipelinePayload(4242, repo, pipelineID, sha, status, createdAt, finishedAt, mrIID)
	fx.s.ingestGitLabPipeline(context.Background(), gitLabPipelineEvent(repo, credentialRef, body))
}

func (fx *gitLabPipelinePGFixture) verdict(t *testing.T, id uuid.UUID) deployUpstreamVerdict {
	t.Helper()
	return fx.s.deployCIGreenVerdict(context.Background(), fx.reload(t, id))
}

func (fx *gitLabPipelinePGFixture) latestPipelineRow(t *testing.T, stageID uuid.UUID) *stagecheck.Check {
	t.Helper()
	c, err := fx.checkRepo.LatestForStageAndName(context.Background(), stageID, webhook.GitLabPipelineCheckContext)
	if err != nil {
		if errors.Is(err, stagecheck.ErrNotFound) {
			return nil
		}
		t.Fatalf("LatestForStageAndName: %v", err)
	}
	return c
}

// latestPolicyCIGreen returns the CIGreen of the newest policy_evaluated
// row anchored on the implement stage, and how many such rows exist.
func (fx *gitLabPipelinePGFixture) latestPolicyCIGreen(t *testing.T, runID, implStageID uuid.UUID) (*bool, int) {
	t.Helper()
	entries, err := fx.auditRepo.ListForRunByCategory(context.Background(), runID, policy.CategoryPolicyEvaluated)
	if err != nil {
		t.Fatalf("ListForRunByCategory: %v", err)
	}
	var last *policy.EvaluationPayload
	n := 0
	for _, e := range entries {
		if e.StageID == nil || *e.StageID != implStageID {
			continue
		}
		n++
		var pl policy.EvaluationPayload
		if err := json.Unmarshal(e.Payload, &pl); err != nil {
			t.Fatalf("decode policy_evaluated: %v", err)
		}
		last = &pl
	}
	if last == nil {
		return nil, 0
	}
	return last.Applied.CIGreen, n
}

func assertBranch(t *testing.T, v deployUpstreamVerdict, want string) {
	t.Helper()
	if v.Satisfied {
		t.Fatalf("Satisfied = true, want refusal on branch %q", want)
	}
	if got := v.Details["branch"]; got != want {
		t.Fatalf("branch = %v, want %q (reason: %s)", got, want, v.Reason)
	}
}

// TestGitLabPipelinePG_SnapshotRoundTrip_ON is the ON + allowed arm through
// the REAL forge adapter and the Projects API boundary (condition 5), then
// cases (a) success → Satisfied, (b) failed → contexts_failed and (c) no
// pipeline → contexts_pending, plus the persisted row.
func TestGitLabPipelinePG_SnapshotRoundTrip_ON(t *testing.T) {
	pf := newPGProjectsForge(t, true, true)
	fx := newGitLabPipelinePGFixture(t, pf.f)

	pr := fx.mintRun(t, "acme/widgets", "gitlab:4242", 11, 7, "abc123")
	pf.mu.Lock()
	paths := append([]string(nil), pf.paths...)
	pf.mu.Unlock()
	if len(paths) != 1 || paths[0] != "/api/v4/projects/4242" {
		t.Fatalf("Projects API requests = %v, want exactly [/api/v4/projects/4242] — the project id must survive webhook → forge scope", paths)
	}
	r := fx.reload(t, pr.run.ID)
	if r.RequiredChecksSnapshot == nil {
		t.Fatalf("persisted snapshot = nil, want the ON snapshot")
	}
	if got := r.RequiredChecksSnapshot.Contexts; len(got) != 1 || got[0] != webhook.GitLabPipelineCheckContext {
		t.Errorf("Contexts = %v, want [%s]", got, webhook.GitLabPipelineCheckContext)
	}
	if got := r.RequiredChecksSnapshot.Sources; len(got) != 1 || got[0] != webhook.GitLabPipelineRequirementSource {
		t.Errorf("Sources = %v, want [%s]", got, webhook.GitLabPipelineRequirementSource)
	}
	if !r.RequiredChecksSnapshot.GitLabAllowSkippedPipeline {
		t.Errorf("GitLabAllowSkippedPipeline = false, want true (Projects API served allow_merge_on_skipped_pipeline:true)")
	}
	if raw := fx.rawSnapshot(t, pr.run.ID); !strings.Contains(raw, `"gitlab_allow_skipped_pipeline": true`) && !strings.Contains(raw, `"gitlab_allow_skipped_pipeline":true`) {
		t.Errorf("raw JSONB %s lacks an explicit gitlab_allow_skipped_pipeline:true", raw)
	}

	// (c) nothing delivered yet → contexts_pending.
	v := fx.verdict(t, pr.run.ID)
	assertBranch(t, v, "contexts_pending")
	if pc, _ := v.Details["pending_contexts"].([]string); len(pc) != 1 || pc[0] != webhook.GitLabPipelineCheckContext {
		t.Errorf("pending_contexts = %v, want [gitlab/pipeline]", v.Details["pending_contexts"])
	}

	// (b) failed → contexts_failed.
	fx.deliver(t, "acme/widgets", "gitlab:4242", 100, "abc123", "failed", "2026-09-19 10:00:00 UTC", "2026-09-19 10:05:00 UTC", 7)
	v = fx.verdict(t, pr.run.ID)
	assertBranch(t, v, "contexts_failed")
	if fc, _ := v.Details["failed_contexts"].([]string); len(fc) != 1 || fc[0] != webhook.GitLabPipelineCheckContext {
		t.Errorf("failed_contexts = %v, want [gitlab/pipeline]", v.Details["failed_contexts"])
	}
	if g, n := fx.latestPolicyCIGreen(t, pr.run.ID, pr.implStageID); n != 2 || g == nil || *g {
		t.Errorf("after failed: policy_evaluated rows = %d ci_green = %v, want 2 rows and &false", n, g)
	}

	// (a) a newer success → Satisfied; the persisted row carries the id.
	fx.deliver(t, "acme/widgets", "gitlab:4242", 101, "abc123", "success", "2026-09-19 10:10:00 UTC", "2026-09-19 10:15:00 UTC", 7)
	if v := fx.verdict(t, pr.run.ID); !v.Satisfied {
		t.Fatalf("after success: Satisfied = false (branch=%v reason=%q)", v.Details["branch"], v.Reason)
	}
	row := fx.latestPipelineRow(t, pr.reviewStageID)
	if row == nil || row.GitLabPipelineID == nil || *row.GitLabPipelineID != 101 || row.Conclusion == nil || *row.Conclusion != "success" {
		t.Fatalf("latest review-stage row = %+v, want pipeline 101 / success", row)
	}
	if !row.Timestamp.Equal(mustTime(t, "2026-09-19T10:15:00Z")) {
		t.Errorf("row ts = %v, want the payload's finished_at", row.Timestamp)
	}
	if implRows, err := fx.checkRepo.LatestForStage(context.Background(), pr.implStageID); err != nil || len(implRows) != 0 {
		t.Errorf("implement-stage rows = %d (err %v), want 0 — rows land on the review stage only", len(implRows), err)
	}
	if g, n := fx.latestPolicyCIGreen(t, pr.run.ID, pr.implStageID); n != 3 || g == nil || !*g {
		t.Errorf("after success: policy_evaluated rows = %d ci_green = %v, want 3 rows and &true", n, g)
	}
}

// TestGitLabPipelinePG_SkippedFlag is case (f): the same `skipped` pipeline
// is Satisfied when the persisted flag is on and contexts_failed
// (skipped_not_allowed) when it is off — and the OFF-flag snapshot
// serializes an EXPLICIT false (condition 2).
func TestGitLabPipelinePG_SkippedFlag(t *testing.T) {
	t.Run("flag_true", func(t *testing.T) {
		pf := newPGProjectsForge(t, true, true)
		fx := newGitLabPipelinePGFixture(t, pf.f)
		pr := fx.mintRun(t, "acme/widgets", "gitlab:4242", 12, 7, "abc123")
		fx.deliver(t, "acme/widgets", "gitlab:4242", 100, "abc123", "skipped", "", "", 7)
		if v := fx.verdict(t, pr.run.ID); !v.Satisfied {
			t.Fatalf("Satisfied = false (branch=%v reason=%q), want true with allow_merge_on_skipped_pipeline on", v.Details["branch"], v.Reason)
		}
		if row := fx.latestPipelineRow(t, pr.reviewStageID); row == nil || row.Conclusion == nil || *row.Conclusion != "skipped" {
			t.Fatalf("row = %+v, want conclusion skipped", row)
		}
	})
	t.Run("flag_false", func(t *testing.T) {
		pf := newPGProjectsForge(t, true, false)
		fx := newGitLabPipelinePGFixture(t, pf.f)
		pr := fx.mintRun(t, "acme/widgets", "gitlab:4242", 13, 7, "abc123")
		r := fx.reload(t, pr.run.ID)
		if r.RequiredChecksSnapshot == nil || r.RequiredChecksSnapshot.GitLabAllowSkippedPipeline {
			t.Fatalf("snapshot = %+v, want present with flag false", r.RequiredChecksSnapshot)
		}
		if raw := fx.rawSnapshot(t, pr.run.ID); !strings.Contains(raw, `"gitlab_allow_skipped_pipeline": false`) && !strings.Contains(raw, `"gitlab_allow_skipped_pipeline":false`) {
			t.Fatalf("raw JSONB %s lacks an EXPLICIT gitlab_allow_skipped_pipeline:false (condition 2: no omitempty)", raw)
		}
		fx.deliver(t, "acme/widgets", "gitlab:4242", 100, "abc123", "skipped", "", "", 7)
		assertBranch(t, fx.verdict(t, pr.run.ID), "contexts_failed")
		if row := fx.latestPipelineRow(t, pr.reviewStageID); row == nil || row.Conclusion == nil || *row.Conclusion != "skipped_not_allowed" {
			t.Fatalf("row = %+v, want conclusion skipped_not_allowed", row)
		}
	})
}

// TestGitLabPipelinePG_OFF is case (d): only_allow_merge_if_pipeline_succeeds
// off → a PRESENT-BUT-EMPTY snapshot (with an explicit flag false) and a
// Satisfied verdict with no rows at all.
func TestGitLabPipelinePG_OFF(t *testing.T) {
	fx := newGitLabPipelinePGFixture(t, pgStubCIReader{req: &forge.CIRequirement{PipelineMustSucceed: false}})
	pr := fx.mintRun(t, "acme/widgets", "gitlab:4242", 14, 7, "abc123")
	r := fx.reload(t, pr.run.ID)
	if r.RequiredChecksSnapshot == nil {
		t.Fatalf("snapshot = nil, want present-but-empty")
	}
	if len(r.RequiredChecksSnapshot.Contexts) != 0 || len(r.RequiredChecksSnapshot.Sources) != 0 {
		t.Fatalf("snapshot = %+v, want zero contexts and sources", r.RequiredChecksSnapshot)
	}
	if raw := fx.rawSnapshot(t, pr.run.ID); !strings.Contains(raw, `"contexts": []`) && !strings.Contains(raw, `"contexts":[]`) {
		t.Fatalf("raw JSONB %s, want contexts serialized as [] not null", raw)
	}
	if v := fx.verdict(t, pr.run.ID); !v.Satisfied {
		t.Fatalf("Satisfied = false (branch=%v reason=%q), want true: nothing is required", v.Details["branch"], v.Reason)
	}
}

// TestGitLabPipelinePG_NilSnapshot is case (e): an unwired reader and a
// failing reader both leave the snapshot NIL, the run is still minted, and
// the deploy gate refuses snapshot_absent — even after a success pipeline,
// whose `skipped` sibling records skipped_not_allowed.
func TestGitLabPipelinePG_NilSnapshot(t *testing.T) {
	for name, reader := range map[string]forge.CIRequirementReader{
		"reader_unwired": nil,
		"reader_error":   pgStubCIReader{err: errors.New("projects api 503")},
	} {
		t.Run(name, func(t *testing.T) {
			fx := newGitLabPipelinePGFixture(t, reader)
			pr := fx.mintRun(t, "acme/widgets", "gitlab:4242", 15, 7, "abc123")
			if r := fx.reload(t, pr.run.ID); r.RequiredChecksSnapshot != nil {
				t.Fatalf("snapshot = %+v, want nil", r.RequiredChecksSnapshot)
			}
			if raw := fx.rawSnapshot(t, pr.run.ID); raw != "" {
				t.Fatalf("raw JSONB = %s, want SQL NULL", raw)
			}
			assertBranch(t, fx.verdict(t, pr.run.ID), "snapshot_absent")
			fx.deliver(t, "acme/widgets", "gitlab:4242", 100, "abc123", "success", "", "", 7)
			assertBranch(t, fx.verdict(t, pr.run.ID), "snapshot_absent")
			fx.deliver(t, "acme/widgets", "gitlab:4242", 101, "abc123", "skipped", "", "", 7)
			if row := fx.latestPipelineRow(t, pr.reviewStageID); row == nil || row.Conclusion == nil || *row.Conclusion != "skipped_not_allowed" {
				t.Fatalf("row = %+v, want skipped_not_allowed on a nil-snapshot run", row)
			}
		})
	}
}

// TestGitLabPipelinePG_Precedence is cases (g) and (g'): the highest
// pipeline id is authoritative regardless of ts or delivery order.
//
//	(g)  101 failed at T1 delivered FIRST, then 100 success at T2 > T1 →
//	     still contexts_failed (the ingester's write-avoidance skip fires
//	     here, but the ORDER BY alone would decide the same way).
//	(g') 100 success recorded FIRST with ts T2, then 101 failed delivered
//	     LATER with ts T1 < T2 → contexts_failed and the latest row is 101.
//	     A ts-ordered reader would return 100's success; this passes ONLY
//	     through `gitlab_pipeline_id DESC NULLS LAST` — the skip never
//	     fires for a newer id.
func TestGitLabPipelinePG_Precedence(t *testing.T) {
	const t1c, t1f = "2026-09-19 10:00:00 UTC", "2026-09-19 10:05:00 UTC"
	const t2c, t2f = "2026-09-19 11:00:00 UTC", "2026-09-19 11:05:00 UTC"

	t.Run("g_delayed_older_pipeline", func(t *testing.T) {
		pf := newPGProjectsForge(t, true, true)
		fx := newGitLabPipelinePGFixture(t, pf.f)
		pr := fx.mintRun(t, "acme/widgets", "gitlab:4242", 16, 7, "abc123")
		fx.deliver(t, "acme/widgets", "gitlab:4242", 101, "abc123", "failed", t1c, t1f, 7)
		fx.deliver(t, "acme/widgets", "gitlab:4242", 100, "abc123", "success", t2c, t2f, 7)
		assertBranch(t, fx.verdict(t, pr.run.ID), "contexts_failed")
		if row := fx.latestPipelineRow(t, pr.reviewStageID); row == nil || row.GitLabPipelineID == nil || *row.GitLabPipelineID != 101 {
			t.Fatalf("latest row = %+v, want pipeline 101", row)
		}
	})
	t.Run("g_prime_structural_newer_id_earlier_ts", func(t *testing.T) {
		pf := newPGProjectsForge(t, true, true)
		fx := newGitLabPipelinePGFixture(t, pf.f)
		pr := fx.mintRun(t, "acme/widgets", "gitlab:4242", 17, 7, "abc123")
		fx.deliver(t, "acme/widgets", "gitlab:4242", 100, "abc123", "success", t2c, t2f, 7)
		if v := fx.verdict(t, pr.run.ID); !v.Satisfied {
			t.Fatalf("precondition: Satisfied = false after 100 success (branch=%v)", v.Details["branch"])
		}
		fx.deliver(t, "acme/widgets", "gitlab:4242", 101, "abc123", "failed", t1c, t1f, 7)
		assertBranch(t, fx.verdict(t, pr.run.ID), "contexts_failed")
		row := fx.latestPipelineRow(t, pr.reviewStageID)
		if row == nil || row.GitLabPipelineID == nil || *row.GitLabPipelineID != 101 {
			t.Fatalf("latest row = %+v, want pipeline 101 despite its EARLIER ts — precedence must be by id, not ts", row)
		}
		if !row.Timestamp.Equal(mustTime(t, "2026-09-19T10:05:00Z")) {
			t.Errorf("latest row ts = %v, want T1 (the row IS the earlier-stamped one)", row.Timestamp)
		}
		all, err := fx.checkRepo.LatestForStage(context.Background(), pr.reviewStageID)
		if err != nil || len(all) != 1 || all[0].GitLabPipelineID == nil || *all[0].GitLabPipelineID != 101 {
			t.Fatalf("LatestForStage = %+v (err %v), want the single gitlab/pipeline row for 101", all, err)
		}
		if g, _ := fx.latestPolicyCIGreen(t, pr.run.ID, pr.implStageID); g == nil || *g {
			t.Errorf("policy re-eval ci_green = %v, want &false after the newer failed pipeline", g)
		}
	})
}

// TestGitLabPipelinePG_Collision is case (h) with the three decoys of
// approval condition 4: (same repo, different ref), (different repo, SAME
// ref), (both different). A success for the target's (sha, iid) delivered
// under the target's (repo, ref) lands ONLY on the target's review stage.
// Deleting r.repo from the SQL admits the same-ref decoy; deleting
// r.installation_ref admits the same-repo decoy — committed-state reads,
// so each deletion is RED on its own decoy.
func TestGitLabPipelinePG_Collision(t *testing.T) {
	pf := newPGProjectsForge(t, true, true)
	fx := newGitLabPipelinePGFixture(t, pf.f)
	target := fx.mintRun(t, "acme/widgets", "gitlab:4242", 18, 7, "abc123")
	decoySameRepo := fx.mintRun(t, "acme/widgets", "gitlab:9999", 19, 7, "abc123")
	decoySameRef := fx.mintRun(t, "fork/widgets", "gitlab:4242", 20, 7, "abc123")
	decoyBoth := fx.mintRun(t, "fork/other", "gitlab:7777", 21, 7, "abc123")

	fx.deliver(t, "acme/widgets", "gitlab:4242", 100, "abc123", "success", "", "", 7)

	if row := fx.latestPipelineRow(t, target.reviewStageID); row == nil || row.GitLabPipelineID == nil || *row.GitLabPipelineID != 100 {
		t.Fatalf("target row = %+v, want pipeline 100", row)
	}
	for name, d := range map[string]pgRun{
		"same_repo_different_ref": decoySameRepo,
		"different_repo_same_ref": decoySameRef,
		"both_different":          decoyBoth,
	} {
		if row := fx.latestPipelineRow(t, d.reviewStageID); row != nil {
			t.Errorf("decoy %s received a row %+v; the match must be scoped by BOTH runs.repo and runs.installation_ref", name, row)
		}
		if _, n := fx.latestPolicyCIGreen(t, d.run.ID, d.implStageID); n != 1 {
			t.Errorf("decoy %s: policy_evaluated rows = %d, want 1 (no re-eval)", name, n)
		}
	}
	if v := fx.verdict(t, target.run.ID); !v.Satisfied {
		t.Errorf("target verdict Satisfied = false (branch=%v)", v.Details["branch"])
	}
	assertBranch(t, fx.verdict(t, decoySameRef.run.ID), "contexts_pending")
}

// TestGitLabPipelinePG_ReevalReach is case (i): runs A (sha aaa) and B (sha
// bbb, newer) for MR 7 — a success for aaa delivered after B exists writes
// A's row and a new policy_evaluated for A with ci_green=true, and touches
// B not at all.
func TestGitLabPipelinePG_ReevalReach(t *testing.T) {
	pf := newPGProjectsForge(t, true, true)
	fx := newGitLabPipelinePGFixture(t, pf.f)
	a := fx.mintRun(t, "acme/widgets", "gitlab:4242", 22, 7, "aaa111")
	b := fx.mintRun(t, "acme/widgets", "gitlab:4242", 23, 7, "bbb222")

	fx.deliver(t, "acme/widgets", "gitlab:4242", 100, "aaa111", "success", "", "", 7)

	if row := fx.latestPipelineRow(t, a.reviewStageID); row == nil || row.HeadSHA != "aaa111" {
		t.Fatalf("run A row = %+v, want a success row at aaa111", row)
	}
	if g, n := fx.latestPolicyCIGreen(t, a.run.ID, a.implStageID); n != 2 || g == nil || !*g {
		t.Fatalf("run A: policy_evaluated rows = %d ci_green = %v, want 2 rows and &true", n, g)
	}
	if v := fx.verdict(t, a.run.ID); !v.Satisfied {
		t.Errorf("run A verdict Satisfied = false (branch=%v)", v.Details["branch"])
	}
	if row := fx.latestPipelineRow(t, b.reviewStageID); row != nil {
		t.Errorf("run B received a row %+v for a pipeline at a different sha", row)
	}
	if _, n := fx.latestPolicyCIGreen(t, b.run.ID, b.implStageID); n != 1 {
		t.Errorf("run B: policy_evaluated rows = %d, want 1 (untouched)", n)
	}
	assertBranch(t, fx.verdict(t, b.run.ID), "contexts_pending")
}

// mustTime parses an RFC 3339 instant or fails the test.
func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return ts
}
