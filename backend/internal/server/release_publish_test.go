package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
)

// --- fakes for the release-publish path (offline: artifact load -> GitHub
// round-trip -> audit append) ---

// fakePublisherAsset mirrors a stored GitHub Release asset with its bytes, so a
// test can assert the replaced asset carries the new content.
type fakePublisherAsset struct {
	id      int64
	name    string
	content []byte
}

// fakeReleasePublisher is the offline stand-in for the production GitHub client
// on the publish path. Pointer receiver so body/asset mutations persist across
// the two handler invocations an idempotency test drives.
type fakeReleasePublisher struct {
	mu sync.Mutex

	instID     int64
	installErr error
	getErr     error
	notFound   bool // GetReleaseByTag returns ErrNotFound

	releaseID int64
	htmlURL   string
	body      string
	assets    []fakePublisherAsset

	updateErr error
	deleteErr error
	uploadErr error

	patchCalls  int
	deleteCalls int
	uploadCalls int
	nextAssetID int64
}

func (f *fakeReleasePublisher) GetRepoInstallation(_ context.Context, _ githubclient.RepoRef) (int64, error) {
	if f.installErr != nil {
		return 0, f.installErr
	}
	return f.instID, nil
}

func (f *fakeReleasePublisher) GetReleaseByTag(_ context.Context, _ int64, _ githubclient.RepoRef, tag string) (*githubclient.Release, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	if f.notFound {
		return nil, githubclient.ErrNotFound
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	assets := make([]githubclient.ReleaseAsset, 0, len(f.assets))
	for _, a := range f.assets {
		assets = append(assets, githubclient.ReleaseAsset{ID: a.id, Name: a.name})
	}
	return &githubclient.Release{ID: f.releaseID, TagName: tag, Body: f.body, HTMLURL: f.htmlURL, Assets: assets}, nil
}

func (f *fakeReleasePublisher) UpdateReleaseBody(_ context.Context, _ int64, _ githubclient.RepoRef, _ int64, body string) error {
	if f.updateErr != nil {
		return f.updateErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.patchCalls++
	f.body = body
	return nil
}

func (f *fakeReleasePublisher) DeleteReleaseAsset(_ context.Context, _ int64, _ githubclient.RepoRef, assetID int64) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleteCalls++
	kept := f.assets[:0:0]
	for _, a := range f.assets {
		if a.id != assetID {
			kept = append(kept, a)
		}
	}
	f.assets = kept
	return nil
}

func (f *fakeReleasePublisher) UploadReleaseAsset(_ context.Context, _ int64, _ githubclient.RepoRef, _ int64, name, _ string, data []byte) error {
	if f.uploadErr != nil {
		return f.uploadErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.uploadCalls++
	f.nextAssetID++
	f.assets = append(f.assets, fakePublisherAsset{id: 1000 + f.nextAssetID, name: name, content: append([]byte(nil), data...)})
	return nil
}

// assetByName returns the stored asset with the given name (and whether found).
func (f *fakeReleasePublisher) assetByName(name string) (fakePublisherAsset, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, a := range f.assets {
		if a.name == name {
			return a, true
		}
	}
	return fakePublisherAsset{}, false
}

// fakeChainAuditRepo records AppendChained entries and returns them from
// ListForRun, so the content-hash idempotency read (lastReleasePublishedHash)
// resolves offline.
type fakeChainAuditRepo struct {
	audit.BaseFake
	mu         sync.Mutex
	entries    []*audit.Entry
	appendErr  error
	listErr    error
	appendSeen []audit.ChainAppendParams
}

func (f *fakeChainAuditRepo) AppendChained(_ context.Context, p audit.ChainAppendParams) (*audit.Entry, error) {
	if f.appendErr != nil {
		return nil, f.appendErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.appendSeen = append(f.appendSeen, p)
	e := &audit.Entry{
		ID:        uuid.New(),
		Sequence:  int64(len(f.entries) + 1),
		RunID:     &p.RunID,
		StageID:   p.StageID,
		Timestamp: p.Timestamp,
		Category:  p.Category,
		ActorKind: p.ActorKind,
		Payload:   p.Payload,
	}
	f.entries = append(f.entries, e)
	return e, nil
}

func (f *fakeChainAuditRepo) ListForRun(_ context.Context, runID uuid.UUID) ([]*audit.Entry, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*audit.Entry
	for _, e := range f.entries {
		if e.RunID != nil && *e.RunID == runID {
			out = append(out, e)
		}
	}
	return out, nil
}

// publishHarness wires a Server over the fake artifact + audit repos and the
// fake publisher, plus a seeded release_notes artifact.
type publishHarness struct {
	t          *testing.T
	server     *Server
	artRepo    *fakeArtifactRepo
	auditRepo  *fakeChainAuditRepo
	publisher  *fakeReleasePublisher
	artifactID uuid.UUID
	runID      uuid.UUID
	markdown   string
}

func newPublishHarness(t *testing.T, markdown string, pub *fakeReleasePublisher) *publishHarness {
	t.Helper()
	artRepo := newFakeArtifactRepo()
	content, _ := json.Marshal(releaseNotesContent{
		Repo: "kuhlman-labs/fishhawk", From: "v0.1.0", To: "v0.2.0", Markdown: markdown,
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
	s.releasePublisherOverride = pub
	return &publishHarness{
		t:          t,
		server:     s,
		artRepo:    artRepo,
		auditRepo:  auditRepo,
		publisher:  pub,
		artifactID: created.ID,
		runID:      uuid.New(),
		markdown:   markdown,
	}
}

// post builds a publish request with the harness's ids and the supplied identity
// decorator, and runs the handler.
func (h *publishHarness) post(reqBody string, decorate func(*http.Request) *http.Request) *httptest.ResponseRecorder {
	h.t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v0/releases/publish", strings.NewReader(reqBody))
	rec := httptest.NewRecorder()
	h.server.handleReleasePublish(rec, decorate(req))
	return rec
}

func (h *publishHarness) validBody() string {
	b, _ := json.Marshal(releasePublishRequest{
		Repo: "kuhlman-labs/fishhawk", Tag: "v0.2.0",
		RunID: h.runID.String(), ArtifactID: h.artifactID.String(),
	})
	return string(b)
}

// TestReleasePublish_HappyPath crosses every layer: artifact load -> GitHub
// round-trip -> audit append. It asserts the Release body is set to the notes
// markdown, the markdown asset is uploaded, and a release_published audit row
// carries the content_hash.
func TestReleasePublish_HappyPath(t *testing.T) {
	md := "# Release v0.2.0\n\n- assemble release evidence\n"
	pub := &fakeReleasePublisher{instID: 77, releaseID: 555, body: "stale body",
		htmlURL: "https://github.com/kuhlman-labs/fishhawk/releases/tag/v0.2.0"}
	h := newPublishHarness(t, md, pub)

	rec := h.post(h.validBody(), withReleaseOperator)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", rec.Code, rec.Body.String())
	}
	var resp releasePublishResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !resp.Published || resp.Idempotent {
		t.Errorf("flags = %+v, want published:true idempotent:false", resp)
	}
	if resp.ContentHash != sha256Hex([]byte(md)) {
		t.Errorf("content_hash = %q, want %q", resp.ContentHash, sha256Hex([]byte(md)))
	}
	if resp.ReleaseURL != pub.htmlURL {
		t.Errorf("release_url = %q", resp.ReleaseURL)
	}

	// Body set to the notes markdown.
	if pub.body != md {
		t.Errorf("release body = %q, want the notes markdown", pub.body)
	}
	// Markdown asset uploaded with the fixed name + markdown content.
	asset, ok := pub.assetByName(releaseNotesAssetName)
	if !ok {
		t.Fatalf("no %s asset uploaded", releaseNotesAssetName)
	}
	if string(asset.content) != md {
		t.Errorf("asset content = %q, want the notes markdown", asset.content)
	}
	// release_published audit row carrying the content_hash.
	if len(h.auditRepo.appendSeen) != 1 {
		t.Fatalf("audit appends = %d, want 1", len(h.auditRepo.appendSeen))
	}
	got := h.auditRepo.appendSeen[0]
	if got.Category != CategoryReleasePublished {
		t.Errorf("category = %q, want %q", got.Category, CategoryReleasePublished)
	}
	if got.RunID != h.runID {
		t.Errorf("run_id = %s, want %s", got.RunID, h.runID)
	}
	if got.ActorKind == nil || *got.ActorKind != audit.ActorSystem {
		t.Errorf("actor kind = %v, want system", got.ActorKind)
	}
	var payload struct {
		Tag         string `json:"tag"`
		ReleaseURL  string `json:"release_url"`
		ArtifactID  string `json:"artifact_id"`
		ContentHash string `json:"content_hash"`
	}
	if err := json.Unmarshal(got.Payload, &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if payload.ContentHash != sha256Hex([]byte(md)) || payload.Tag != "v0.2.0" ||
		payload.ArtifactID != h.artifactID.String() || payload.ReleaseURL != pub.htmlURL {
		t.Errorf("payload = %+v", payload)
	}
}

// TestReleasePublish_IdempotentReinvoke pins the content-hash no-op: after a
// first publish, a second identical call PATCHes/uploads nothing, returns
// idempotent:true, and appends no second audit row (the done-means test that a
// mere asset-name-presence check would wrongly let publish again).
func TestReleasePublish_IdempotentReinvoke(t *testing.T) {
	md := "# Release v0.2.0\n\nnotes\n"
	pub := &fakeReleasePublisher{instID: 77, releaseID: 555, body: "stale",
		htmlURL: "https://github.com/kuhlman-labs/fishhawk/releases/tag/v0.2.0"}
	h := newPublishHarness(t, md, pub)

	if rec := h.post(h.validBody(), withReleaseOperator); rec.Code != http.StatusOK {
		t.Fatalf("first publish status = %d:\n%s", rec.Code, rec.Body.String())
	}
	patchAfterFirst, uploadAfterFirst := pub.patchCalls, pub.uploadCalls

	rec := h.post(h.validBody(), withReleaseOperator)
	if rec.Code != http.StatusOK {
		t.Fatalf("reinvoke status = %d:\n%s", rec.Code, rec.Body.String())
	}
	var resp releasePublishResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Published || !resp.Idempotent {
		t.Errorf("flags = %+v, want published:false idempotent:true", resp)
	}
	if pub.patchCalls != patchAfterFirst {
		t.Errorf("patch calls = %d, want unchanged %d (no-op reinvoke)", pub.patchCalls, patchAfterFirst)
	}
	if pub.uploadCalls != uploadAfterFirst {
		t.Errorf("upload calls = %d, want unchanged %d (no-op reinvoke)", pub.uploadCalls, uploadAfterFirst)
	}
	if len(h.auditRepo.appendSeen) != 1 {
		t.Errorf("audit appends = %d, want 1 (no second row on no-op)", len(h.auditRepo.appendSeen))
	}
}

// TestReleasePublish_StaleAssetReplaced pins the binding stale-asset case: an
// existing asset with the fixed name but DIFFERENT content is replaced, so
// after publish the asset carries the new content and release_published records
// the new hash (body and asset can never diverge).
func TestReleasePublish_StaleAssetReplaced(t *testing.T) {
	md := "# Release v0.2.0\n\nfresh notes\n"
	pub := &fakeReleasePublisher{
		instID: 77, releaseID: 555, body: "old body that differs",
		htmlURL: "https://github.com/kuhlman-labs/fishhawk/releases/tag/v0.2.0",
		assets:  []fakePublisherAsset{{id: 9001, name: releaseNotesAssetName, content: []byte("STALE asset content")}},
	}
	h := newPublishHarness(t, md, pub)

	rec := h.post(h.validBody(), withReleaseOperator)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d:\n%s", rec.Code, rec.Body.String())
	}
	if pub.deleteCalls != 1 {
		t.Errorf("delete calls = %d, want 1 (stale asset deleted before re-upload)", pub.deleteCalls)
	}
	asset, ok := pub.assetByName(releaseNotesAssetName)
	if !ok {
		t.Fatalf("no %s asset after publish", releaseNotesAssetName)
	}
	if string(asset.content) != md {
		t.Errorf("asset content = %q, want the fresh markdown (stale content must be replaced)", asset.content)
	}
	if asset.id == 9001 {
		t.Errorf("asset id = 9001, want a new asset (delete+upload, not left stale)")
	}
	if pub.body != md {
		t.Errorf("body = %q, want fresh markdown", pub.body)
	}
	// release_published records the NEW hash.
	if len(h.auditRepo.appendSeen) != 1 {
		t.Fatalf("audit appends = %d, want 1", len(h.auditRepo.appendSeen))
	}
	var payload struct {
		ContentHash string `json:"content_hash"`
	}
	_ = json.Unmarshal(h.auditRepo.appendSeen[0].Payload, &payload)
	if payload.ContentHash != sha256Hex([]byte(md)) {
		t.Errorf("recorded content_hash = %q, want the fresh hash %q", payload.ContentHash, sha256Hex([]byte(md)))
	}
}

// --- fail-closed / defensive branches (one behavioral test per named mode) ---

func newAuthedPublisher() *fakeReleasePublisher {
	return &fakeReleasePublisher{instID: 77, releaseID: 555, htmlURL: "https://x/releases/tag/v0.2.0"}
}

// TestReleasePublish_Anonymous pins the 401 branch (via the routed mux, no
// identity).
func TestReleasePublish_Anonymous(t *testing.T) {
	h := newPublishHarness(t, "md", newAuthedPublisher())
	req := httptest.NewRequest(http.MethodPost, "/v0/releases/publish", strings.NewReader(h.validBody()))
	rec := httptest.NewRecorder()
	h.server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401:\n%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "authentication_required") {
		t.Errorf("body missing authentication_required:\n%s", rec.Body.String())
	}
}

// TestReleasePublish_MissingScope pins the 403 branch: an authenticated bearer
// token without write:runs cannot publish.
func TestReleasePublish_MissingScope(t *testing.T) {
	h := newPublishHarness(t, "md", newAuthedPublisher())
	rec := h.post(h.validBody(), withReleaseReader)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403:\n%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "insufficient_scope") {
		t.Errorf("body missing insufficient_scope:\n%s", rec.Body.String())
	}
}

// TestReleasePublish_Unconfigured pins the 503 nil-dependency branch: an
// authenticated caller on a zero-Config server clears the auth ladder then hits
// releasePublishConfigured.
func TestReleasePublish_Unconfigured(t *testing.T) {
	s := New(Config{})
	req := httptest.NewRequest(http.MethodPost, "/v0/releases/publish",
		strings.NewReader(`{"repo":"o/n","tag":"v1","run_id":"`+uuid.New().String()+`","artifact_id":"`+uuid.New().String()+`"}`))
	rec := httptest.NewRecorder()
	s.handleReleasePublish(rec, withReleaseOperator(req))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503:\n%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "release_publish_unconfigured") {
		t.Errorf("body missing release_publish_unconfigured:\n%s", rec.Body.String())
	}
}

// TestReleasePublish_BadBody pins the 400 branches: missing required field,
// malformed run_id, malformed artifact_id. Each field-level case additionally
// asserts the response NAMES the offending field in details.field — a generic
// validation error for every case must not satisfy [publish-validates-required-
// fields]. The unknown-field case is a JSON decode failure (no field name), so
// it only asserts the code.
func TestReleasePublish_BadBody(t *testing.T) {
	h := newPublishHarness(t, "md", newAuthedPublisher())
	cases := []struct {
		name      string
		body      string
		wantField string // "" when the case is a decode error with no field
	}{
		{"missing-repo", `{"tag":"v1","run_id":"` + h.runID.String() + `","artifact_id":"` + h.artifactID.String() + `"}`, "repo"},
		{"missing-tag", `{"repo":"o/n","run_id":"` + h.runID.String() + `","artifact_id":"` + h.artifactID.String() + `"}`, "tag"},
		{"missing-run", `{"repo":"o/n","tag":"v1","artifact_id":"` + h.artifactID.String() + `"}`, "run_id"},
		{"missing-artifact", `{"repo":"o/n","tag":"v1","run_id":"` + h.runID.String() + `"}`, "artifact_id"},
		{"bad-run", `{"repo":"o/n","tag":"v1","run_id":"nope","artifact_id":"` + h.artifactID.String() + `"}`, "run_id"},
		{"bad-artifact", `{"repo":"o/n","tag":"v1","run_id":"` + h.runID.String() + `","artifact_id":"nope"}`, "artifact_id"},
		{"unknown-field", `{"repo":"o/n","tag":"v1","run_id":"` + h.runID.String() + `","artifact_id":"` + h.artifactID.String() + `","x":1}`, ""},
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

// TestReleasePublish_ArtifactNotFound pins the 404 artifact_not_found branch.
func TestReleasePublish_ArtifactNotFound(t *testing.T) {
	h := newPublishHarness(t, "md", newAuthedPublisher())
	body, _ := json.Marshal(releasePublishRequest{
		Repo: "kuhlman-labs/fishhawk", Tag: "v0.2.0",
		RunID: h.runID.String(), ArtifactID: uuid.New().String(), // no such artifact
	})
	rec := h.post(string(body), withReleaseOperator)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404:\n%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "artifact_not_found") {
		t.Errorf("body missing artifact_not_found:\n%s", rec.Body.String())
	}
}

// TestReleasePublish_WrongKind pins the 409 branch: an artifact that is not a
// release_notes kind is rejected.
func TestReleasePublish_WrongKind(t *testing.T) {
	pub := newAuthedPublisher()
	h := newPublishHarness(t, "md", pub)
	// Seed a non-release_notes artifact and target it.
	planContent, _ := json.Marshal(map[string]any{"kind": "plan"})
	planArt, err := h.artRepo.Create(context.Background(), artifact.CreateParams{
		StageID: uuid.New(), Kind: artifact.KindPlan, Content: planContent, ContentHash: sha256Hex(planContent),
	})
	if err != nil {
		t.Fatalf("seed plan artifact: %v", err)
	}
	body, _ := json.Marshal(releasePublishRequest{
		Repo: "kuhlman-labs/fishhawk", Tag: "v0.2.0",
		RunID: h.runID.String(), ArtifactID: planArt.ID.String(),
	})
	rec := h.post(string(body), withReleaseOperator)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409:\n%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "artifact_wrong_kind") {
		t.Errorf("body missing artifact_wrong_kind:\n%s", rec.Body.String())
	}
}

// TestReleasePublish_ReleaseNotFound pins the 404 release_not_found branch.
func TestReleasePublish_ReleaseNotFound(t *testing.T) {
	pub := newAuthedPublisher()
	pub.notFound = true
	h := newPublishHarness(t, "md", pub)
	rec := h.post(h.validBody(), withReleaseOperator)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404:\n%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "release_not_found") {
		t.Errorf("body missing release_not_found:\n%s", rec.Body.String())
	}
}

// TestReleasePublish_AppNotInstalled pins the 503 github_app_not_installed
// branch.
func TestReleasePublish_AppNotInstalled(t *testing.T) {
	pub := newAuthedPublisher()
	pub.installErr = githubclient.ErrNotInstalled
	h := newPublishHarness(t, "md", pub)
	rec := h.post(h.validBody(), withReleaseOperator)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503:\n%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "github_app_not_installed") {
		t.Errorf("body missing github_app_not_installed:\n%s", rec.Body.String())
	}
}

// TestReleasePublish_ReleaseLookupError pins the 502 release_lookup_failed
// branch: a non-ErrNotFound error from GetReleaseByTag fails closed.
func TestReleasePublish_ReleaseLookupError(t *testing.T) {
	pub := newAuthedPublisher()
	pub.getErr = context.DeadlineExceeded
	h := newPublishHarness(t, "md", pub)
	rec := h.post(h.validBody(), withReleaseOperator)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502:\n%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "release_lookup_failed") {
		t.Errorf("body missing release_lookup_failed:\n%s", rec.Body.String())
	}
}

// TestReleasePublish_UpdateBodyError pins the 502 release_publish_failed branch:
// a GitHub failure updating the Release body fails closed with no audit row.
func TestReleasePublish_UpdateBodyError(t *testing.T) {
	pub := &fakeReleasePublisher{instID: 77, releaseID: 555, body: "stale", htmlURL: "https://x/tag/v0.2.0"}
	pub.updateErr = context.DeadlineExceeded
	h := newPublishHarness(t, "notes", pub)
	rec := h.post(h.validBody(), withReleaseOperator)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502:\n%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "release_publish_failed") {
		t.Errorf("body missing release_publish_failed:\n%s", rec.Body.String())
	}
	if len(h.auditRepo.appendSeen) != 0 {
		t.Errorf("audit appends = %d, want 0 (no audit on a failed update)", len(h.auditRepo.appendSeen))
	}
}

// TestReleasePublish_AuditFailure pins the durable-before-response 500 branch:
// an audit append error surfaces as release_publish_audit_failed rather than a
// false 200.
func TestReleasePublish_AuditFailure(t *testing.T) {
	pub := &fakeReleasePublisher{instID: 77, releaseID: 555, body: "stale", htmlURL: "https://x/tag/v0.2.0"}
	h := newPublishHarness(t, "notes", pub)
	h.auditRepo.appendErr = context.DeadlineExceeded
	rec := h.post(h.validBody(), withReleaseOperator)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500:\n%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "release_publish_audit_failed") {
		t.Errorf("body missing release_publish_audit_failed:\n%s", rec.Body.String())
	}
}

// TestReleasePublish_InstallResolutionError pins the 502
// installation_resolution_failed branch: a NON-ErrNotInstalled error from
// GetRepoInstallation fails closed as a bad-gateway (the ErrNotInstalled->503
// path is covered by TestReleasePublish_AppNotInstalled).
func TestReleasePublish_InstallResolutionError(t *testing.T) {
	pub := newAuthedPublisher()
	pub.installErr = context.DeadlineExceeded
	h := newPublishHarness(t, "md", pub)
	rec := h.post(h.validBody(), withReleaseOperator)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502:\n%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "installation_resolution_failed") {
		t.Errorf("body missing installation_resolution_failed:\n%s", rec.Body.String())
	}
}

// TestReleasePublish_DecodeContentError pins the 500 release_publish_failed
// branch on a release_notes artifact whose stored content is not valid JSON:
// the kind check passes but the releaseNotesContent unmarshal fails closed.
func TestReleasePublish_DecodeContentError(t *testing.T) {
	h := newPublishHarness(t, "md", newAuthedPublisher())
	bad, err := h.artRepo.Create(context.Background(), artifact.CreateParams{
		StageID:     uuid.New(),
		Kind:        artifact.KindReleaseNotes,
		Content:     []byte("this is not json"),
		ContentHash: sha256Hex([]byte("this is not json")),
	})
	if err != nil {
		t.Fatalf("seed bad-content artifact: %v", err)
	}
	body, _ := json.Marshal(releasePublishRequest{
		Repo: "kuhlman-labs/fishhawk", Tag: "v0.2.0",
		RunID: h.runID.String(), ArtifactID: bad.ID.String(),
	})
	rec := h.post(string(body), withReleaseOperator)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500:\n%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "release_publish_failed") {
		t.Errorf("body missing release_publish_failed:\n%s", rec.Body.String())
	}
	if len(h.auditRepo.appendSeen) != 0 {
		t.Errorf("audit appends = %d, want 0 (no audit on a decode failure)", len(h.auditRepo.appendSeen))
	}
}

// TestReleasePublish_DeleteAssetError pins the 502 release_publish_failed branch
// in the delete-stale-asset loop: a GitHub failure deleting the existing
// fixed-name asset fails closed with no upload and no audit row.
func TestReleasePublish_DeleteAssetError(t *testing.T) {
	md := "# Release v0.2.0\n\nfresh\n"
	pub := &fakeReleasePublisher{
		instID: 77, releaseID: 555, body: "old body that differs",
		htmlURL:   "https://x/tag/v0.2.0",
		assets:    []fakePublisherAsset{{id: 9001, name: releaseNotesAssetName, content: []byte("stale")}},
		deleteErr: context.DeadlineExceeded,
	}
	h := newPublishHarness(t, md, pub)
	rec := h.post(h.validBody(), withReleaseOperator)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502:\n%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "release_publish_failed") {
		t.Errorf("body missing release_publish_failed:\n%s", rec.Body.String())
	}
	if pub.uploadCalls != 0 {
		t.Errorf("upload calls = %d, want 0 (delete failed before upload)", pub.uploadCalls)
	}
	if len(h.auditRepo.appendSeen) != 0 {
		t.Errorf("audit appends = %d, want 0 (no audit on a failed delete)", len(h.auditRepo.appendSeen))
	}
}

// TestReleasePublish_UploadAssetError pins the 502 release_publish_failed branch
// on UploadReleaseAsset failure: after the body PATCH succeeds, a GitHub failure
// uploading the fixed-name asset fails closed with no audit row.
func TestReleasePublish_UploadAssetError(t *testing.T) {
	pub := &fakeReleasePublisher{instID: 77, releaseID: 555, body: "stale", htmlURL: "https://x/tag/v0.2.0"}
	pub.uploadErr = context.DeadlineExceeded
	h := newPublishHarness(t, "notes", pub)
	rec := h.post(h.validBody(), withReleaseOperator)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502:\n%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "release_publish_failed") {
		t.Errorf("body missing release_publish_failed:\n%s", rec.Body.String())
	}
	if len(h.auditRepo.appendSeen) != 0 {
		t.Errorf("audit appends = %d, want 0 (no audit on a failed upload)", len(h.auditRepo.appendSeen))
	}
}
