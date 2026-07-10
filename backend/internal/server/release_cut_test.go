package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
)

// cutHarness wires a Server over the fake artifact + audit repos with a seeded
// release_notes artifact, so the record-the-decision path (artifact load ->
// kind check -> audit append) runs entirely offline. It reuses the
// fakeArtifactRepo + fakeChainAuditRepo defined alongside the publish tests.
type cutHarness struct {
	t           *testing.T
	server      *Server
	artRepo     *fakeArtifactRepo
	auditRepo   *fakeChainAuditRepo
	artifactID  uuid.UUID
	runID       uuid.UUID
	contentHash string
}

func newCutHarness(t *testing.T) *cutHarness {
	t.Helper()
	artRepo := newFakeArtifactRepo()
	content, _ := json.Marshal(releaseNotesContent{
		Repo: "kuhlman-labs/fishhawk", From: "v0.1.0", To: "v0.2.0",
		Markdown: "# Release v0.2.0\n\n- cut this\n",
	})
	created, err := artRepo.Create(context.Background(), artifact.CreateParams{
		StageID:     uuid.New(),
		Kind:        artifact.KindReleaseNotes,
		Content:     content,
		ContentHash: sha256Hex(content),
	})
	if err != nil {
		t.Fatalf("seed artifact: %v", err)
	}
	auditRepo := &fakeChainAuditRepo{}
	s := New(Config{
		Addr:         "127.0.0.1:0",
		ArtifactRepo: artRepo,
		AuditRepo:    auditRepo,
	})
	return &cutHarness{
		t:           t,
		server:      s,
		artRepo:     artRepo,
		auditRepo:   auditRepo,
		artifactID:  created.ID,
		runID:       uuid.New(),
		contentHash: created.ContentHash,
	}
}

// post builds a cut request with the supplied body + identity decorator and
// runs the handler directly.
func (h *cutHarness) post(reqBody string, decorate func(*http.Request) *http.Request) *httptest.ResponseRecorder {
	h.t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v0/releases/cut", strings.NewReader(reqBody))
	rec := httptest.NewRecorder()
	h.server.handleReleaseCut(rec, decorate(req))
	return rec
}

func (h *cutHarness) validBody() string {
	b, _ := json.Marshal(releaseCutRequest{
		Repo: "kuhlman-labs/fishhawk", RunID: h.runID.String(),
		ArtifactID: h.artifactID.String(), Version: "v0.2.0", BumpLevel: "minor",
	})
	return string(b)
}

// TestReleaseCut_HappyPath crosses the record path: artifact load -> kind check
// -> audit append. It asserts a single release_cut audit row keyed to the run
// carries the version, artifact id, advisory bump level, and the artifact's own
// content hash, and that the response affirms recorded:true.
func TestReleaseCut_HappyPath(t *testing.T) {
	h := newCutHarness(t)
	rec := h.post(h.validBody(), withReleaseOperator)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", rec.Code, rec.Body.String())
	}
	var resp releaseCutResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !resp.Recorded || resp.Version != "v0.2.0" || resp.BumpLevel != "minor" {
		t.Errorf("resp = %+v, want recorded:true version:v0.2.0 bump:minor", resp)
	}
	if resp.ContentHash != h.contentHash {
		t.Errorf("content_hash = %q, want %q", resp.ContentHash, h.contentHash)
	}

	if len(h.auditRepo.appendSeen) != 1 {
		t.Fatalf("audit appends = %d, want 1", len(h.auditRepo.appendSeen))
	}
	got := h.auditRepo.appendSeen[0]
	if got.Category != CategoryReleaseCut {
		t.Errorf("category = %q, want %q", got.Category, CategoryReleaseCut)
	}
	if got.RunID != h.runID {
		t.Errorf("run_id = %s, want %s", got.RunID, h.runID)
	}
	if got.ActorKind == nil || *got.ActorKind != audit.ActorSystem {
		t.Errorf("actor kind = %v, want system", got.ActorKind)
	}
	var payload struct {
		Repo        string `json:"repo"`
		Version     string `json:"version"`
		ArtifactID  string `json:"artifact_id"`
		BumpLevel   string `json:"bump_level"`
		ContentHash string `json:"content_hash"`
	}
	if err := json.Unmarshal(got.Payload, &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if payload.Version != "v0.2.0" || payload.ArtifactID != h.artifactID.String() ||
		payload.BumpLevel != "minor" || payload.ContentHash != h.contentHash ||
		payload.Repo != "kuhlman-labs/fishhawk" {
		t.Errorf("payload = %+v", payload)
	}
}

// TestReleaseCut_OptionalBumpOmitted pins that bump_level is advisory: a cut
// with no bump_level still records and the response omits the empty field.
func TestReleaseCut_OptionalBumpOmitted(t *testing.T) {
	h := newCutHarness(t)
	body, _ := json.Marshal(releaseCutRequest{
		Repo: "kuhlman-labs/fishhawk", RunID: h.runID.String(),
		ArtifactID: h.artifactID.String(), Version: "v0.2.0",
	})
	rec := h.post(string(body), withReleaseOperator)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "bump_level") {
		t.Errorf("empty bump_level should be omitted from the response:\n%s", rec.Body.String())
	}
	if len(h.auditRepo.appendSeen) != 1 {
		t.Fatalf("audit appends = %d, want 1", len(h.auditRepo.appendSeen))
	}
}

// --- fail-closed / defensive branches (one behavioral test per named mode) ---

// TestReleaseCut_Anonymous pins the 401 branch via the routed mux (no identity).
func TestReleaseCut_Anonymous(t *testing.T) {
	h := newCutHarness(t)
	req := httptest.NewRequest(http.MethodPost, "/v0/releases/cut", strings.NewReader(h.validBody()))
	rec := httptest.NewRecorder()
	h.server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401:\n%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "authentication_required") {
		t.Errorf("body missing authentication_required:\n%s", rec.Body.String())
	}
}

// TestReleaseCut_MissingScope pins the 403 branch: an authenticated bearer
// token without write:runs cannot cut.
func TestReleaseCut_MissingScope(t *testing.T) {
	h := newCutHarness(t)
	rec := h.post(h.validBody(), withReleaseReader)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403:\n%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "insufficient_scope") {
		t.Errorf("body missing insufficient_scope:\n%s", rec.Body.String())
	}
}

// TestReleaseCut_Unconfigured pins the 503 nil-dependency branch: an
// authenticated caller on a zero-Config server clears the auth ladder then hits
// releaseCutConfigured.
func TestReleaseCut_Unconfigured(t *testing.T) {
	s := New(Config{})
	req := httptest.NewRequest(http.MethodPost, "/v0/releases/cut",
		strings.NewReader(`{"repo":"o/n","run_id":"`+uuid.New().String()+`","artifact_id":"`+uuid.New().String()+`","version":"v1"}`))
	rec := httptest.NewRecorder()
	s.handleReleaseCut(rec, withReleaseOperator(req))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503:\n%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "release_cut_unconfigured") {
		t.Errorf("body missing release_cut_unconfigured:\n%s", rec.Body.String())
	}
}

// TestReleaseCut_BadBody pins the 400 branches: each missing required field and
// each malformed UUID names the offending field in details.field, and the
// unknown-field decode error asserts only the code.
func TestReleaseCut_BadBody(t *testing.T) {
	h := newCutHarness(t)
	rid, aid := h.runID.String(), h.artifactID.String()
	cases := []struct {
		name      string
		body      string
		wantField string // "" when the case is a decode error with no field
	}{
		{"missing-repo", `{"run_id":"` + rid + `","artifact_id":"` + aid + `","version":"v1"}`, "repo"},
		{"missing-run", `{"repo":"o/n","artifact_id":"` + aid + `","version":"v1"}`, "run_id"},
		{"missing-artifact", `{"repo":"o/n","run_id":"` + rid + `","version":"v1"}`, "artifact_id"},
		{"missing-version", `{"repo":"o/n","run_id":"` + rid + `","artifact_id":"` + aid + `"}`, "version"},
		{"bad-run", `{"repo":"o/n","run_id":"nope","artifact_id":"` + aid + `","version":"v1"}`, "run_id"},
		{"bad-artifact", `{"repo":"o/n","run_id":"` + rid + `","artifact_id":"nope","version":"v1"}`, "artifact_id"},
		{"bad-stage", `{"repo":"o/n","run_id":"` + rid + `","artifact_id":"` + aid + `","version":"v1","stage_id":"nope"}`, "stage_id"},
		{"unknown-field", `{"repo":"o/n","run_id":"` + rid + `","artifact_id":"` + aid + `","version":"v1","x":1}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := h.post(tc.body, withReleaseOperator)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400:\n%s", rec.Code, rec.Body.String())
			}
			var env errorEnvelope
			if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
				t.Fatalf("decode error envelope: %v\n%s", err, rec.Body.String())
			}
			if env.Error.Code != "validation_failed" {
				t.Errorf("code = %q, want validation_failed:\n%s", env.Error.Code, rec.Body.String())
			}
			if tc.wantField == "" {
				return
			}
			gotField, _ := env.Error.Details["field"].(string)
			if gotField != tc.wantField {
				t.Errorf("details.field = %q, want %q (a generic validation error must name the offending field)\n%s",
					gotField, tc.wantField, rec.Body.String())
			}
		})
	}
}

// TestReleaseCut_ArtifactNotFound pins the 404 artifact_not_found branch.
func TestReleaseCut_ArtifactNotFound(t *testing.T) {
	h := newCutHarness(t)
	body, _ := json.Marshal(releaseCutRequest{
		Repo: "kuhlman-labs/fishhawk", RunID: h.runID.String(),
		ArtifactID: uuid.New().String(), Version: "v0.2.0", // no such artifact
	})
	rec := h.post(string(body), withReleaseOperator)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404:\n%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "artifact_not_found") {
		t.Errorf("body missing artifact_not_found:\n%s", rec.Body.String())
	}
	if len(h.auditRepo.appendSeen) != 0 {
		t.Errorf("audit appends = %d, want 0 (no record on a missing artifact)", len(h.auditRepo.appendSeen))
	}
}

// TestReleaseCut_WrongKind pins the 409 branch: an artifact that is not a
// release_notes kind cannot be cut against.
func TestReleaseCut_WrongKind(t *testing.T) {
	h := newCutHarness(t)
	planContent, _ := json.Marshal(map[string]any{"kind": "plan"})
	planArt, err := h.artRepo.Create(context.Background(), artifact.CreateParams{
		StageID: uuid.New(), Kind: artifact.KindPlan, Content: planContent, ContentHash: sha256Hex(planContent),
	})
	if err != nil {
		t.Fatalf("seed plan artifact: %v", err)
	}
	body, _ := json.Marshal(releaseCutRequest{
		Repo: "kuhlman-labs/fishhawk", RunID: h.runID.String(),
		ArtifactID: planArt.ID.String(), Version: "v0.2.0",
	})
	rec := h.post(string(body), withReleaseOperator)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409:\n%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "artifact_wrong_kind") {
		t.Errorf("body missing artifact_wrong_kind:\n%s", rec.Body.String())
	}
	if len(h.auditRepo.appendSeen) != 0 {
		t.Errorf("audit appends = %d, want 0 (no record on a wrong-kind artifact)", len(h.auditRepo.appendSeen))
	}
}

// TestReleaseCut_ArtifactLoadError pins the 500 release_cut_failed branch: a
// non-NotFound error from ArtifactRepo.Get (a transient load failure, not a
// missing artifact) surfaces as release_cut_failed and records no audit entry.
func TestReleaseCut_ArtifactLoadError(t *testing.T) {
	h := newCutHarness(t)
	h.artRepo.getErr = context.DeadlineExceeded
	rec := h.post(h.validBody(), withReleaseOperator)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500:\n%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "release_cut_failed") {
		t.Errorf("body missing release_cut_failed:\n%s", rec.Body.String())
	}
	if len(h.auditRepo.appendSeen) != 0 {
		t.Errorf("audit appends = %d, want 0 (no record on an artifact load failure)", len(h.auditRepo.appendSeen))
	}
}

// TestReleaseCut_AuditFailure pins the durable-before-response 500 branch: an
// audit append error surfaces as release_cut_audit_failed rather than a false
// 201.
func TestReleaseCut_AuditFailure(t *testing.T) {
	h := newCutHarness(t)
	h.auditRepo.appendErr = context.DeadlineExceeded
	rec := h.post(h.validBody(), withReleaseOperator)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500:\n%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "release_cut_audit_failed") {
		t.Errorf("body missing release_cut_audit_failed:\n%s", rec.Body.String())
	}
}
