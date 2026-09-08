package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/repodoc"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// charter_determination_pg_test.go is the CROSS-BOUNDARY proof for the
// persisted grooming determination (E54.13 / #2806): it drives the REAL
// `POST /v0/runs` handler → CreateRunForTrigger's stamp → a REAL Postgres
// `runs.requires_charter` column → `GET /v0/runs/{id}` → the prompt-serve
// path's stageRequiresCharter, in ONE test against a pgtest-provisioned
// database. Per-layer units (the stamp test in charter_gate_test.go, the
// repo round trip in run/postgres_test.go, the determination tests in
// charter_injection_test.go) would each pass while this seam broke — a stamp
// that fired on a different column, a scan site that dropped the value, a
// handler reading a fake that never saw SQL NULL.
//
// The corruption step is BY CONSTRUCTION: a direct UPDATE of the stored
// workflow_spec bytes (approval condition 5 names pgtest.NewPool as exposing
// the pool for exactly this), never a call to the control under test.

// cdPGFixture is a server wired for BOTH run creation (real run + audit repos,
// a fake concern repo) and prompt serving (a signing fake, a fake artifact
// repo, the wired charter seam).
type cdPGFixture struct {
	s       *Server
	runRepo run.Repository
	pool    *pgxpool.Pool
	sf      *signingFake
}

func newCDPGFixture(t *testing.T) *cdPGFixture {
	t.Helper()
	pool := pgtest.NewPool(t)
	runRepo := run.NewPostgresRepository(pool)
	auditRepo := audit.NewPostgresRepository(pool)
	sf := newSigningFake()

	ff := newCHFetcher()
	cfg := Config{
		Addr:             "127.0.0.1:0",
		RunRepo:          runRepo,
		AuditRepo:        auditRepo,
		ConcernRepo:      newFakeConcernRepo(),
		SigningRepo:      sf,
		ArtifactRepo:     newFakeArtifactRepo(),
		DocumentResolver: &repodoc.Resolver{Fetcher: ff, Commits: &chCommits{sha: chPinnedCommit}},
		DocumentBaseRef:  chDefaultBaseRef,
	}
	s := New(cfg)
	s.cfg.DocumentDeclarations = s.CharterDocumentDeclarations
	s.promptIssueGetterOverride = &stubIssueGetter{}

	return &cdPGFixture{s: s, runRepo: runRepo, pool: pool, sf: sf}
}

// createRun POSTs /v0/runs with an inline workflow_spec and returns the run id.
func (f *cdPGFixture) createRun(t *testing.T, workflowID, triggerSource, specYAML string) uuid.UUID {
	t.Helper()
	w := createRunViaHandler(t, f.s, map[string]any{
		"repo": "x/y", "workflow_id": workflowID, "workflow_sha": "abc",
		"trigger_source": triggerSource, "workflow_spec": specYAML,
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("POST /v0/runs status = %d, want 201:\n%s", w.Code, w.Body.String())
	}
	var resp struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode create response: %v\n%s", err, w.Body.String())
	}
	id, err := uuid.Parse(resp.ID)
	if err != nil {
		t.Fatalf("create response id %q: %v", resp.ID, err)
	}
	return id
}

// planStage returns the run's plan-typed stage, read from the real repo.
func (f *cdPGFixture) planStage(t *testing.T, runID uuid.UUID) *run.Stage {
	t.Helper()
	stages, err := f.runRepo.ListStagesForRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("list stages: %v", err)
	}
	for _, st := range stages {
		if st.Type == run.StageTypePlan {
			return st
		}
	}
	t.Fatalf("run %s carries no plan stage among %d stages", runID, len(stages))
	return nil
}

// getRunRequiresCharter drives GET /v0/runs/{id} and returns the RAW
// requires_charter wire value: (nil, false) when the key is absent.
func (f *cdPGFixture) getRunRequiresCharter(t *testing.T, runID uuid.UUID) (value bool, present bool) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v0/runs/"+runID.String(), nil)
	req.SetPathValue("run_id", runID.String())
	w := httptest.NewRecorder()
	f.s.handleGetRun(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET run status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode run response: %v\n%s", err, w.Body.String())
	}
	raw, ok := body["requires_charter"]
	if !ok {
		return false, false
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatalf("requires_charter is not a boolean: %s", string(raw))
	}
	return value, true
}

// corruptSpec overwrites the stored workflow_spec bytes DIRECTLY, bypassing
// every product write path.
func (f *cdPGFixture) corruptSpec(t *testing.T, runID uuid.UUID, corrupt string) {
	t.Helper()
	tag, err := f.pool.Exec(context.Background(),
		`UPDATE runs SET workflow_spec = $1 WHERE id = $2`, []byte(corrupt), runID)
	if err != nil {
		t.Fatalf("corrupt workflow_spec: %v", err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("corrupt workflow_spec touched %d rows, want 1", tag.RowsAffected())
	}
	// Read back through the repo: the corruption landed AND the persisted
	// determination survived it, which is the whole point.
	stored, err := f.runRepo.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("GetRun after corruption: %v", err)
	}
	if string(stored.WorkflowSpec) != corrupt {
		t.Fatalf("stored workflow_spec after corruption = %q, want the corrupt bytes", stored.WorkflowSpec)
	}
}

// TestCharterDetermination_PG_GroomingRun_CorruptSpecStillAnchored: a grooming
// run minted through the real handler carries requires_charter TRUE in the
// real row and on the wire; after its stored spec is corrupted to
// syntactically broken bytes, the plan prompt is refused for the CHARTER
// reason — never grooming_workflow_spec_unreadable, which would mean the
// prompt-serve path consulted the corrupt bytes instead of the fact.
func TestCharterDetermination_PG_GroomingRun_CorruptSpecStillAnchored(t *testing.T) {
	f := newCDPGFixture(t)
	installConventions(t, chConventions(chCharterPath), nil)

	runID := f.createRun(t, "backlog_grooming", string(run.TriggerOnDemand), chGroomingSpec)

	// COMMITTED STATE: the real row, read back through the repository.
	stored, err := f.runRepo.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if stored.RequiresCharter == nil || !*stored.RequiresCharter {
		t.Fatalf("persisted requires_charter = %v, want true", stored.RequiresCharter)
	}
	if v, present := f.getRunRequiresCharter(t, runID); !present || !v {
		t.Fatalf("GET /v0/runs/{id} requires_charter = (value %v, present %v), want true", v, present)
	}

	stage := f.planStage(t, runID)
	priv, _ := f.sf.issue(t, runID)

	// Control: with the spec intact the prompt is served WITH the charter.
	resp := chDecodePrompt(t, f.s, runID, stage.ID, priv)
	if !strings.Contains(resp.Prompt, "### "+charterFraming().Heading) {
		t.Fatalf("intact grooming run served a prompt with no charter block:\n%s", resp.Prompt)
	}

	// Corrupt the stored bytes, then take the charter away: the run is STILL
	// grooming (the fact decides) and is refused for the charter reason.
	f.corruptSpec(t, runID, chCorruptGroomingSpec)
	installConventions(t, chConventionsWithoutCharter(), nil)
	body := chRefusal(t, f.s, runID, stage.ID, priv)
	if got, _ := chDetails(t, body)["reason"].(string); got == reasonGroomingSpecUnreadable {
		t.Fatalf("a run carrying a persisted determination was refused for the SPEC (%s); the fact must decide", got)
	}
	chAssertReason(t, body, reasonCharterAbsent)

	// And with the charter declared again, the corrupt-spec grooming run is
	// served WITH its charter: the bytes are never read.
	installConventions(t, chConventions(chCharterPath), nil)
	resp = chDecodePrompt(t, f.s, runID, stage.ID, priv)
	if !strings.Contains(resp.Prompt, "### "+charterFraming().Heading) {
		t.Errorf("corrupt-spec grooming run served a prompt with no charter block:\n%s", resp.Prompt)
	}
}

// TestCharterDetermination_PG_OrdinaryRun_CorruptSpecWithTokenServed is the
// ordinary-workflow twin: requires_charter FALSE in the row and on the wire,
// and after the stored spec is corrupted to bytes carrying the grooming_report
// token in a comment and a scalar, the plan prompt is SERVED, byte-identical
// to the intact-spec prompt, with the conventions loader never consulted.
func TestCharterDetermination_PG_OrdinaryRun_CorruptSpecWithTokenServed(t *testing.T) {
	f := newCDPGFixture(t)
	calls := installConventions(t, chConventions(chCharterPath), nil)

	runID := f.createRun(t, "feature_change", string(run.TriggerCLI), chPlainSpec)

	stored, err := f.runRepo.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if stored.RequiresCharter == nil || *stored.RequiresCharter {
		t.Fatalf("persisted requires_charter = %v, want false", stored.RequiresCharter)
	}
	if v, present := f.getRunRequiresCharter(t, runID); !present || v {
		t.Fatalf("GET /v0/runs/{id} requires_charter = (value %v, present %v), want false", v, present)
	}

	stage := f.planStage(t, runID)
	priv, _ := f.sf.issue(t, runID)

	intact := chDecodePrompt(t, f.s, runID, stage.ID, priv)
	if strings.Contains(intact.Prompt, "### "+charterFraming().Heading) {
		t.Fatalf("ordinary run served a charter block:\n%s", intact.Prompt)
	}

	f.corruptSpec(t, runID, chCorruptPlainSpecIncidentalToken)
	corrupt := chDecodePrompt(t, f.s, runID, stage.ID, priv)
	if corrupt.Prompt != intact.Prompt {
		t.Errorf("corrupting the stored spec changed the served prompt of a persisted non-grooming run:\n--- intact ---\n%s\n--- corrupt ---\n%s",
			intact.Prompt, corrupt.Prompt)
	}
	if *calls != 0 {
		t.Errorf("the conventions loader was consulted %d times for an ordinary run, want 0", *calls)
	}
}
