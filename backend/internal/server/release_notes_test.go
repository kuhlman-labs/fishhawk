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

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/postgres"
	"github.com/kuhlman-labs/fishhawk/backend/internal/releaseevidence"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// fakeReleaseResolver injects a fixed merged-PR list so the release-notes
// endpoint runs offline — the test seam described in the plan. It stands in for
// the production releaseevidence.GitHubResolver (the commit walk), letting the
// cross-boundary integration test drive resolver -> assembler -> renderer ->
// HTTP without live GitHub.
type fakeReleaseResolver struct {
	prs []releaseevidence.MergedPR
	err error
}

func (f fakeReleaseResolver) MergedPRsInRange(context.Context, string, string, string) ([]releaseevidence.MergedPR, error) {
	return f.prs, f.err
}

// releaseNotesHarness wires a Server over pgtest-backed run/audit/concern/
// artifact repos plus the fake resolver override, and seeds evidence rows.
type releaseNotesHarness struct {
	t         *testing.T
	ctx       context.Context
	runRepo   run.Repository
	auditRepo audit.Repository
	concRepo  concern.Repository
	artRepo   artifact.Repository
	server    *Server
}

func newReleaseNotesHarness(t *testing.T, prs ...releaseevidence.MergedPR) *releaseNotesHarness {
	t.Helper()
	url := pgtest.NewURL(t)
	if err := postgres.MigrateUp(url); err != nil {
		t.Fatalf("MigrateUp: %v", err)
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)

	h := &releaseNotesHarness{
		t:         t,
		ctx:       context.Background(),
		runRepo:   run.NewPostgresRepository(pool),
		auditRepo: audit.NewPostgresRepository(pool),
		concRepo:  concern.NewPostgresRepository(pool),
		artRepo:   artifact.NewPostgresRepository(pool),
	}
	s := New(Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      h.runRepo,
		AuditRepo:    h.auditRepo,
		ConcernRepo:  h.concRepo,
		ArtifactRepo: h.artRepo,
	})
	s.releaseNotesResolverOverride = fakeReleaseResolver{prs: prs}
	h.server = s
	return h
}

// seedLoopRun creates a succeeded run mapped to prURL, carrying a plan stage
// with a standard_v1 plan artifact so the assembler resolves the plan summary
// and evidence links.
func (h *releaseNotesHarness) seedLoopRun(prURL, summary string) {
	h.t.Helper()
	r, err := h.runRepo.CreateRun(h.ctx, run.CreateRunParams{
		Repo:          "kuhlman-labs/fishhawk",
		WorkflowID:    "feature_change",
		WorkflowSHA:   "deadbeef",
		TriggerSource: run.TriggerCLI,
	})
	if err != nil {
		h.t.Fatalf("create run: %v", err)
	}
	if _, err := h.runRepo.SetRunPullRequestURL(h.ctx, r.ID, prURL); err != nil {
		h.t.Fatalf("set pr url: %v", err)
	}
	st, err := h.runRepo.CreateStage(h.ctx, run.CreateStageParams{
		RunID:        r.ID,
		Sequence:     1,
		Type:         run.StageTypePlan,
		ExecutorKind: run.ExecutorAgent,
		ExecutorRef:  "fishhawk/runner@v1",
	})
	if err != nil {
		h.t.Fatalf("create plan stage: %v", err)
	}
	content, _ := json.Marshal(map[string]any{"kind": "plan", "summary": summary})
	sv := "standard_v1"
	sum := sha256Hex(content)
	if _, err := h.artRepo.Create(h.ctx, artifact.CreateParams{
		StageID:       st.ID,
		Kind:          artifact.KindPlan,
		SchemaVersion: &sv,
		Content:       content,
		ContentHash:   sum,
	}); err != nil {
		h.t.Fatalf("create plan artifact: %v", err)
	}
	if _, err := h.runRepo.TransitionRun(h.ctx, r.ID, run.StateRunning); err != nil {
		h.t.Fatalf("transition running: %v", err)
	}
	if _, err := h.runRepo.TransitionRun(h.ctx, r.ID, run.StateSucceeded); err != nil {
		h.t.Fatalf("transition succeeded: %v", err)
	}
}

// seedStage creates a bare run + stage and returns the stage id, for use as a
// persist target (the caller-supplied stage_id the release_notes artifact keys
// to).
func (h *releaseNotesHarness) seedStage() uuid.UUID {
	h.t.Helper()
	r, err := h.runRepo.CreateRun(h.ctx, run.CreateRunParams{
		Repo:          "kuhlman-labs/fishhawk",
		WorkflowID:    "feature_change",
		WorkflowSHA:   "deadbeef",
		TriggerSource: run.TriggerCLI,
	})
	if err != nil {
		h.t.Fatalf("create run: %v", err)
	}
	st, err := h.runRepo.CreateStage(h.ctx, run.CreateStageParams{
		RunID:        r.ID,
		Sequence:     0,
		Type:         run.StageTypePlan,
		ExecutorKind: run.ExecutorAgent,
		ExecutorRef:  "fishhawk/runner@v1",
	})
	if err != nil {
		h.t.Fatalf("create stage: %v", err)
	}
	return st.ID
}

// withReleaseOperator injects an operator token identity carrying write:runs.
func withReleaseOperator(req *http.Request) *http.Request {
	return req.WithContext(context.WithValue(req.Context(), ctxKeyIdentity, Identity{
		Subject: "github:ops", TokenID: "tok-op", Scopes: []string{"write:runs"},
	}))
}

// withReleaseReader injects an authenticated read-only token (no write:runs).
func withReleaseReader(req *http.Request) *http.Request {
	return req.WithContext(context.WithValue(req.Context(), ctxKeyIdentity, Identity{
		Subject: "github:reader", TokenID: "tok-read", Scopes: []string{"read:runs"},
	}))
}

// mixedPRs returns a loop-merged PR (mapped to a seeded run) and a reduced PR
// (no run) so a single assembly exercises both evidence classes.
func mixedPRs() []releaseevidence.MergedPR {
	return []releaseevidence.MergedPR{
		{URL: "https://github.com/kuhlman-labs/fishhawk/pull/100", Number: 100, Title: "loop change"},
		{URL: "https://github.com/kuhlman-labs/fishhawk/pull/101", Number: 101, Title: "human-led change"},
	}
}

// TestReleaseNotesPreview_EndToEnd drives GET preview across resolver ->
// assembler -> renderer -> HTTP, asserting evidence links AND the reduced-
// evidence marker both appear.
func TestReleaseNotesPreview_EndToEnd(t *testing.T) {
	h := newReleaseNotesHarness(t, mixedPRs()...)
	h.seedLoopRun("https://github.com/kuhlman-labs/fishhawk/pull/100", "assemble release evidence")

	req := httptest.NewRequest(http.MethodGet,
		"/v0/releases/notes/preview?repo=kuhlman-labs/fishhawk&from=v0.1.0&to=main", nil)
	rec := httptest.NewRecorder()
	h.server.handleReleaseNotesPreview(rec, withReleaseOperator(req))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/markdown; charset=utf-8" {
		t.Errorf("content-type = %q, want text/markdown", ct)
	}
	body := rec.Body.String()
	// Loop-merged evidence: plan summary + working PR link.
	if !strings.Contains(body, "assemble release evidence") {
		t.Errorf("body missing plan summary:\n%s", body)
	}
	if !strings.Contains(body, "- Plan: https://github.com/kuhlman-labs/fishhawk/pull/100") {
		t.Errorf("body missing plan link:\n%s", body)
	}
	// Reduced-evidence marker for the unmapped PR.
	if !strings.Contains(body, "> **Reduced evidence.**") {
		t.Errorf("body missing reduced-evidence marker:\n%s", body)
	}
	// Header cost rollup present.
	if !strings.Contains(body, "Total cost: $") {
		t.Errorf("body missing cost rollup:\n%s", body)
	}
}

// TestReleaseNotesPersist_RoundTrips drives POST persist and asserts a
// release_notes artifact is created and round-trips through the kind CHECK
// (proving migration 0051 + the constant).
func TestReleaseNotesPersist_RoundTrips(t *testing.T) {
	h := newReleaseNotesHarness(t, mixedPRs()...)
	h.seedLoopRun("https://github.com/kuhlman-labs/fishhawk/pull/100", "assemble release evidence")
	stageID := h.seedStage()

	reqBody, _ := json.Marshal(releaseNotesPersistRequest{
		Repo: "kuhlman-labs/fishhawk", From: "v0.1.0", To: "main", StageID: stageID.String(),
	})
	req := httptest.NewRequest(http.MethodPost, "/v0/releases/notes", strings.NewReader(string(reqBody)))
	rec := httptest.NewRecorder()
	h.server.handleReleaseNotesPersist(rec, withReleaseOperator(req))

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", rec.Code, rec.Body.String())
	}
	var resp releaseNotesPersistResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.ArtifactID == "" || resp.StageID != stageID.String() {
		t.Fatalf("response = %+v", resp)
	}
	if !strings.Contains(resp.Markdown, "assemble release evidence") {
		t.Errorf("response markdown missing evidence:\n%s", resp.Markdown)
	}

	// The persisted artifact round-trips through the kind CHECK: Get returns
	// KindReleaseNotes (proving migration 0051 admitted it).
	artID, err := uuid.Parse(resp.ArtifactID)
	if err != nil {
		t.Fatalf("parse artifact id: %v", err)
	}
	got, err := h.artRepo.Get(h.ctx, artID)
	if err != nil {
		t.Fatalf("Get persisted artifact: %v", err)
	}
	if got.Kind != artifact.KindReleaseNotes {
		t.Errorf("persisted kind = %q, want release_notes", got.Kind)
	}
	var content releaseNotesContent
	if err := json.Unmarshal(got.Content, &content); err != nil {
		t.Fatalf("decode content: %v", err)
	}
	if content.Repo != "kuhlman-labs/fishhawk" || !strings.Contains(content.Markdown, "Release notes") {
		t.Errorf("content = %+v", content)
	}
}

// --- fail-closed / defensive branches (one behavioral test per named mode) ---

// TestReleaseNotesPreview_MissingParam pins the 400 branch for each missing
// coordinate.
func TestReleaseNotesPreview_MissingParam(t *testing.T) {
	h := newReleaseNotesHarness(t)
	cases := map[string]string{
		"no-repo": "/v0/releases/notes/preview?from=a&to=b",
		"no-from": "/v0/releases/notes/preview?repo=o/n&to=b",
		"no-to":   "/v0/releases/notes/preview?repo=o/n&from=a",
	}
	for name, url := range cases {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, url, nil)
			rec := httptest.NewRecorder()
			h.server.handleReleaseNotesPreview(rec, withReleaseOperator(req))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400:\n%s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "validation_failed") {
				t.Errorf("body missing validation_failed:\n%s", rec.Body.String())
			}
		})
	}
}

// TestReleaseNotesPreview_Anonymous pins the 401 branch.
func TestReleaseNotesPreview_Anonymous(t *testing.T) {
	h := newReleaseNotesHarness(t)
	req := httptest.NewRequest(http.MethodGet,
		"/v0/releases/notes/preview?repo=o/n&from=a&to=b", nil)
	rec := httptest.NewRecorder()
	h.server.Handler().ServeHTTP(rec, req) // no identity
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401:\n%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "authentication_required") {
		t.Errorf("body missing authentication_required:\n%s", rec.Body.String())
	}
}

// The 503 nil-dependency branch for both routes is pinned by the
// *RouteRegistered tests in handlers_test.go (a zero-Config server reaches the
// handler and 503s rather than 404ing).

// TestReleaseNotesPersist_Anonymous pins the 401 branch on the write endpoint.
func TestReleaseNotesPersist_Anonymous(t *testing.T) {
	h := newReleaseNotesHarness(t)
	req := httptest.NewRequest(http.MethodPost, "/v0/releases/notes",
		strings.NewReader(`{"repo":"o/n","from":"a","to":"b","stage_id":"x"}`))
	rec := httptest.NewRecorder()
	h.server.Handler().ServeHTTP(rec, req) // no identity
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401:\n%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "authentication_required") {
		t.Errorf("body missing authentication_required:\n%s", rec.Body.String())
	}
}

// TestReleaseNotesPersist_MissingScope pins the 403 branch: an authenticated
// bearer token without write:runs cannot persist (binding condition 3).
func TestReleaseNotesPersist_MissingScope(t *testing.T) {
	h := newReleaseNotesHarness(t)
	req := httptest.NewRequest(http.MethodPost, "/v0/releases/notes",
		strings.NewReader(`{"repo":"kuhlman-labs/fishhawk","from":"a","to":"b","stage_id":"`+uuid.New().String()+`"}`))
	rec := httptest.NewRecorder()
	h.server.handleReleaseNotesPersist(rec, withReleaseReader(req))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403:\n%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "insufficient_scope") {
		t.Errorf("body missing insufficient_scope:\n%s", rec.Body.String())
	}
}

// TestReleaseNotesPersist_BadStageID pins the 400 branch for a malformed
// stage_id.
func TestReleaseNotesPersist_BadStageID(t *testing.T) {
	h := newReleaseNotesHarness(t)
	req := httptest.NewRequest(http.MethodPost, "/v0/releases/notes",
		strings.NewReader(`{"repo":"kuhlman-labs/fishhawk","from":"a","to":"b","stage_id":"not-a-uuid"}`))
	rec := httptest.NewRecorder()
	h.server.handleReleaseNotesPersist(rec, withReleaseOperator(req))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400:\n%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "validation_failed") {
		t.Errorf("body missing validation_failed:\n%s", rec.Body.String())
	}
}

// TestReleaseNotesPersist_ResolverError pins the 502 assembly-failure branch:
// a resolver error propagates fail-closed (MergedPRsInRange fails CLOSED), so
// no artifact is rendered from a partial walk.
func TestReleaseNotesPersist_ResolverError(t *testing.T) {
	h := newReleaseNotesHarness(t)
	h.server.releaseNotesResolverOverride = fakeReleaseResolver{err: context.DeadlineExceeded}
	stageID := h.seedStage()
	req := httptest.NewRequest(http.MethodPost, "/v0/releases/notes",
		strings.NewReader(`{"repo":"kuhlman-labs/fishhawk","from":"a","to":"b","stage_id":"`+stageID.String()+`"}`))
	rec := httptest.NewRecorder()
	h.server.handleReleaseNotesPersist(rec, withReleaseOperator(req))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502:\n%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "release_notes_assembly_failed") {
		t.Errorf("body missing release_notes_assembly_failed:\n%s", rec.Body.String())
	}
}
