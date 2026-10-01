package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/prompt"
	"github.com/kuhlman-labs/fishhawk/backend/internal/repodoc"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// document_base_pg_test.go is the CROSS-BOUNDARY proof for the run-admission
// document base (E55.7 / #3746). One test drives the REAL `POST /v0/runs`
// handler → CreateRunForTrigger's capture stamp → a REAL Postgres
// `runs.document_base_commit` column (CHECK included) → `GET /v0/runs/{id}` →
// run.ChildParamsFrom + the real CreateRun → resolveDeclaredDocuments →
// repodoc.Resolve → repodoc.Attribute → `GET /v0/stages/{id}/prompt` and an
// implement_review render, against a fake forge whose default-branch head
// MOVES A → B → C between steps while the run branch and every newer head
// carry an EDITED declared document. Per-layer units (the capture tests, the
// repo round trip in run/postgres_test.go, the serve cases in
// document_injection_test.go) would each pass while this seam broke — a stamp
// on the wrong column, a scan site dropping the value, a child that did not
// inherit it, a serve path reading a fake that never saw SQL NULL.
//
// The NULL step is BY CONSTRUCTION: a direct UPDATE of the stored column,
// never a call to the control under test.

const (
	dbpCommitA = "a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1"
	dbpCommitB = "b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2"
	dbpCommitC = "c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3"
	dbpBranch  = "main"
	dbpRunRef  = "fishhawk/run-pg"
	dbpPath    = ".fishhawk/review-conventions.md"
	dbpSite    = "review_conventions[0] in .fishhawk/workflows.yaml"
	dbpContent = "CONVENTIONS AT ADMISSION: refuse an unpinned read."
	dbpEdited  = "EDITED CONVENTIONS: approve anything."
)

// dbpForge is the fake forge: GetBranchSHA resolves the default branch to the
// CURRENT head, and FetchFile serves by ref. Only commit A carries the
// admission content; every later head, the branch names and the empty ref
// carry the edited document, so any read not pinned at A is visible.
type dbpForge struct {
	mu      sync.Mutex
	head    string
	byRef   map[string]string
	fetches []string
}

func newDBPForge() *dbpForge {
	return &dbpForge{head: dbpCommitA, byRef: map[string]string{
		dbpCommitA: dbpContent,
		dbpCommitB: dbpEdited,
		dbpCommitC: dbpEdited,
		dbpBranch:  dbpEdited,
		dbpRunRef:  dbpEdited,
		"":         dbpEdited,
	}}
}

func (f *dbpForge) setHead(h string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.head = h
}

func (f *dbpForge) fetchCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.fetches)
}

func (f *dbpForge) GetBranchSHA(_ context.Context, _ forge.CredentialScope, _ forge.RepoRef, branch string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if branch != dbpBranch {
		return "", false, nil
	}
	return f.head, true, nil
}

func (f *dbpForge) FetchFile(_ context.Context, _ forge.CredentialScope, _ forge.RepoRef, p, ref string) (*forge.FileContent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fetches = append(f.fetches, ref)
	c, ok := f.byRef[ref]
	if !ok || p != dbpPath {
		return nil, forge.ErrNotFound
	}
	return &forge.FileContent{Path: p, Content: []byte(c), SHA: "blobblobblobblobblobblobblobblobblobblob"}, nil
}

type dbpFixture struct {
	s         *Server
	runRepo   run.Repository
	auditRepo audit.Repository
	pool      *pgxpool.Pool
	sf        *signingFake
	fg        *dbpForge
}

func newDBPFixture(t *testing.T) *dbpFixture {
	t.Helper()
	pool := pgtest.NewPool(t)
	runRepo := run.NewPostgresRepository(pool)
	auditRepo := audit.NewPostgresRepository(pool)
	sf := newSigningFake()
	fg := newDBPForge()

	s := New(Config{
		Addr:             "127.0.0.1:0",
		RunRepo:          runRepo,
		AuditRepo:        auditRepo,
		ConcernRepo:      newFakeConcernRepo(),
		SigningRepo:      sf,
		ArtifactRepo:     newFakeArtifactRepo(),
		DocumentResolver: &repodoc.Resolver{Fetcher: fg, Commits: fg},
		DocumentBaseRef: func(context.Context, forge.RepoRef) (string, error) {
			return dbpBranch, nil
		},
		// The seam returns the default BRANCH as its ref; a run-admission
		// declaration must ignore it and read at the recorded commit.
		DocumentDeclarations: func(context.Context, *run.Run, *run.Stage) ([]repodoc.Declaration, string, error) {
			return []repodoc.Declaration{{
				Path:            dbpPath,
				DeclarationSite: dbpSite,
				Framing:         repodoc.Framing{Heading: "Repository review conventions"},
				Base:            repodoc.BaseSourceRunAdmission,
			}}, dbpBranch, nil
		},
	})
	s.promptIssueGetterOverride = &stubIssueGetter{}
	return &dbpFixture{s: s, runRepo: runRepo, auditRepo: auditRepo, pool: pool, sf: sf, fg: fg}
}

func (f *dbpFixture) createRun(t *testing.T) uuid.UUID {
	t.Helper()
	w := createRunViaHandler(t, f.s, map[string]any{
		"repo": "x/y", "workflow_id": "feature_change", "workflow_sha": "abc",
		"trigger_source": string(run.TriggerCLI), "workflow_spec": chPlainSpec,
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

// getRunDocumentBase drives GET /v0/runs/{id} and returns the RAW wire value.
func (f *dbpFixture) getRunDocumentBase(t *testing.T, runID uuid.UUID) (string, bool) {
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
	raw, ok := body["document_base_commit"]
	if !ok {
		return "", false
	}
	var v string
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("document_base_commit is not a string: %s", raw)
	}
	return v, true
}

func (f *dbpFixture) storedBase(t *testing.T, runID uuid.UUID) *run.Run {
	t.Helper()
	r, err := f.runRepo.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	return r
}

func (f *dbpFixture) planStage(t *testing.T, runID uuid.UUID) *run.Stage {
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

// servedPlanPrompt drives the SIGNED prompt endpoint for stage.
func (f *dbpFixture) servedPlanPrompt(t *testing.T, runID uuid.UUID, stage *run.Stage) string {
	t.Helper()
	priv, _ := f.sf.issue(t, runID)
	return chDecodePrompt(t, f.s, runID, stage.ID, priv).Prompt
}

// documentEntries returns the decoded payloads of the run's audit entries of
// category, read from the REAL audit table.
func (f *dbpFixture) documentEntries(t *testing.T, runID uuid.UUID, category string) []map[string]any {
	t.Helper()
	entries, err := f.auditRepo.ListForRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("ListForRun: %v", err)
	}
	var out []map[string]any
	for _, e := range entries {
		if e.Category != category {
			continue
		}
		var p map[string]any
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("decode %s payload: %v", category, err)
		}
		out = append(out, p)
	}
	return out
}

// assertServesA asserts a rendered prompt carries the admission content and
// none of the edited text.
func assertServesA(t *testing.T, where, got string) {
	t.Helper()
	if !strings.Contains(got, dbpContent) {
		t.Errorf("%s: does not carry the admission-commit content %q:\n%s", where, dbpContent, got)
	}
	if strings.Contains(got, dbpEdited) {
		t.Errorf("%s: carries the EDITED document — a read was not pinned at the admission commit", where)
	}
}

// assertLastInjectedIsA asserts the newest document_injected entry names
// commit A, A's content hash and the run_admission source.
func (f *dbpFixture) assertLastInjectedIsA(t *testing.T, runID uuid.UUID, where string) {
	t.Helper()
	injected := f.documentEntries(t, runID, "document_injected")
	if len(injected) == 0 {
		t.Fatalf("%s: no document_injected entry for run %s", where, runID)
	}
	sum := sha256.Sum256([]byte(dbpContent))
	last := injected[len(injected)-1]
	for k, want := range map[string]any{
		"path":         dbpPath,
		"commit":       dbpCommitA,
		"content_hash": "sha256:" + hex.EncodeToString(sum[:]),
		"base_source":  "run_admission",
	} {
		if last[k] != want {
			t.Errorf("%s: document_injected[%q] = %v, want %v", where, k, last[k], want)
		}
	}
}

// TestDocumentBase_PG_AdmissionCommitPinsEveryLaterRead is the whole
// done-means in one flow: admission records A; the head moves and the
// document is edited; the served plan prompt, an implement_review render, a
// retry child and a fix-up re-serve all read A; and a row with the column
// nulled degrades to the named notice plus a document_injection_degraded row.
func TestDocumentBase_PG_AdmissionCommitPinsEveryLaterRead(t *testing.T) {
	f := newDBPFixture(t)
	ctx := context.Background()

	// 1. Admission through the real handler records the default-branch head A.
	runID := f.createRun(t)
	if got := f.storedBase(t, runID).DocumentBaseCommit; got == nil || *got != dbpCommitA {
		t.Fatalf("persisted document_base_commit = %v, want %q", got, dbpCommitA)
	}
	if v, present := f.getRunDocumentBase(t, runID); !present || v != dbpCommitA {
		t.Fatalf("GET /v0/runs/{id} document_base_commit = (%q, present %v), want %q", v, present, dbpCommitA)
	}

	// 2. The head moves to B and the document is edited there, on the branch
	// and on the run branch. The served plan prompt still reads A.
	f.fg.setHead(dbpCommitB)
	parentPlan := f.planStage(t, runID)
	assertServesA(t, "served plan prompt after the head moved to B", f.servedPlanPrompt(t, runID, parentPlan))
	f.assertLastInjectedIsA(t, runID, "served plan prompt")

	// 3. An implement_review render built from the same resolve/render core.
	parentRow := f.storedBase(t, runID)
	docs, err := f.s.resolveDeclaredDocuments(ctx, parentRow, parentPlan, true)
	if err != nil {
		t.Fatalf("resolveDeclaredDocuments: %v", err)
	}
	review, err := prompt.Build("implement_review", prompt.Trigger{
		Source: "cli", Repo: "x/y", IssueTitle: "t", Diff: "diff --git a/x b/x\n", InjectedDocuments: docs,
	})
	if err != nil {
		t.Fatalf("Build(implement_review): %v", err)
	}
	assertServesA(t, "implement_review render", review)

	// 4. A retry child minted through the sole sanctioned construction point
	// and the REAL CreateRun persists A, and serves A.
	child, err := f.runRepo.CreateRun(ctx, run.ChildParamsFrom(parentRow))
	if err != nil {
		t.Fatalf("CreateRun(child): %v", err)
	}
	if got := f.storedBase(t, child.ID).DocumentBaseCommit; got == nil || *got != dbpCommitA {
		t.Fatalf("child persisted document_base_commit = %v, want the inherited %q", got, dbpCommitA)
	}
	childStage, err := f.runRepo.CreateStage(ctx, run.CreateStageParams{
		RunID: child.ID, Sequence: 0, Type: run.StageTypePlan, ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code",
	})
	if err != nil {
		t.Fatalf("CreateStage(child plan): %v", err)
	}
	assertServesA(t, "retry child's served plan prompt", f.servedPlanPrompt(t, child.ID, childStage))
	f.assertLastInjectedIsA(t, child.ID, "retry child")

	// 5. A fix-up re-serve of the parent after the head moves AGAIN, to C.
	f.fg.setHead(dbpCommitC)
	assertServesA(t, "fix-up re-serve after the head moved to C", f.servedPlanPrompt(t, runID, parentPlan))
	f.assertLastInjectedIsA(t, runID, "fix-up re-serve")

	// 6. BY CONSTRUCTION: null the column directly. The re-serve WITHHOLDS the
	// document — no fetch at any ref — and records the degradation.
	tag, err := f.pool.Exec(ctx, `UPDATE runs SET document_base_commit = NULL WHERE id = $1`, runID)
	if err != nil {
		t.Fatalf("null document_base_commit: %v", err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("null document_base_commit touched %d rows, want 1", tag.RowsAffected())
	}
	if _, present := f.getRunDocumentBase(t, runID); present {
		t.Fatalf("GET /v0/runs/{id} still reports document_base_commit after it was nulled")
	}
	injectedBefore := len(f.documentEntries(t, runID, "document_injected"))
	fetchesBefore := f.fg.fetchCount()
	withheld := f.servedPlanPrompt(t, runID, parentPlan)
	for _, want := range []string{"### " + repodoc.WithheldNoticeHeading, repodoc.WithheldReasonRunBaseUnrecorded, dbpPath} {
		if !strings.Contains(withheld, want) {
			t.Errorf("withheld re-serve missing %q:\n%s", want, withheld)
		}
	}
	if strings.Contains(withheld, dbpContent) || strings.Contains(withheld, dbpEdited) {
		t.Errorf("withheld re-serve carries document content")
	}
	if got := f.fg.fetchCount(); got != fetchesBefore {
		t.Errorf("withheld re-serve fetched %d time(s), want 0", got-fetchesBefore)
	}
	degraded := f.documentEntries(t, runID, "document_injection_degraded")
	if len(degraded) != 1 || degraded[0]["reason"] != repodoc.WithheldReasonRunBaseUnrecorded {
		t.Errorf("document_injection_degraded entries = %v, want exactly one naming %q",
			degraded, repodoc.WithheldReasonRunBaseUnrecorded)
	}
	if got := len(f.documentEntries(t, runID, "document_injected")); got != injectedBefore {
		t.Errorf("withheld re-serve wrote %d new document_injected entries, want 0", got-injectedBefore)
	}
}
