package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/cli/internal/httpclient"
)

// releaseFake is a configurable backend for the release end-to-end tests. It
// serves the four release endpoints (GET preview → text/markdown, POST notes /
// cut / publish → JSON), capturing the last request per endpoint so a test can
// assert the wiring (method / path / query / body) the CLI issued.
type releaseFake struct {
	mu sync.Mutex

	// preview
	previewStatus  int
	previewErrCode string
	previewBody    string // markdown returned on success
	previewQuery   string // captured raw query string

	// prepare (POST /v0/releases/notes)
	prepareStatus  int
	prepareErrCode string
	prepareResp    httpclient.ReleaseNotesResult
	prepareReq     httpclient.PrepareReleaseNotesInput
	prepareHit     bool

	// cut (POST /v0/releases/cut)
	cutStatus  int
	cutErrCode string
	cutResp    httpclient.CutReleaseResult
	cutReq     httpclient.CutReleaseInput
	cutHit     bool

	// publish (POST /v0/releases/publish)
	publishStatus  int
	publishErrCode string
	publishResp    httpclient.PublishReleaseResult
	publishReq     httpclient.PublishReleaseInput
	publishHit     bool
}

func newReleaseFake(t *testing.T) (*releaseFake, *httptest.Server) {
	t.Helper()
	fb := &releaseFake{
		previewStatus: http.StatusOK,
		prepareStatus: http.StatusCreated,
		cutStatus:     http.StatusCreated,
		publishStatus: http.StatusOK,
	}
	mux := http.NewServeMux()

	mux.HandleFunc("GET /v0/releases/notes/preview", func(w http.ResponseWriter, r *http.Request) {
		fb.mu.Lock()
		fb.previewQuery = r.URL.RawQuery
		fb.mu.Unlock()
		if fb.previewStatus >= 400 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(fb.previewStatus)
			_ = json.NewEncoder(w).Encode(errEnvelope(orDefault(fb.previewErrCode, "authentication_required"), "nope"))
			return
		}
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, fb.previewBody)
	})

	mux.HandleFunc("POST /v0/releases/notes", func(w http.ResponseWriter, r *http.Request) {
		var in httpclient.PrepareReleaseNotesInput
		_ = json.NewDecoder(r.Body).Decode(&in)
		fb.mu.Lock()
		fb.prepareReq = in
		fb.prepareHit = true
		fb.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(fb.prepareStatus)
		if fb.prepareStatus >= 400 {
			_ = json.NewEncoder(w).Encode(errEnvelope(orDefault(fb.prepareErrCode, "validation_failed"), "nope"))
			return
		}
		_ = json.NewEncoder(w).Encode(fb.prepareResp)
	})

	mux.HandleFunc("POST /v0/releases/cut", func(w http.ResponseWriter, r *http.Request) {
		var in httpclient.CutReleaseInput
		_ = json.NewDecoder(r.Body).Decode(&in)
		fb.mu.Lock()
		fb.cutReq = in
		fb.cutHit = true
		fb.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(fb.cutStatus)
		if fb.cutStatus >= 400 {
			_ = json.NewEncoder(w).Encode(errEnvelope(orDefault(fb.cutErrCode, "validation_failed"), "nope"))
			return
		}
		_ = json.NewEncoder(w).Encode(fb.cutResp)
	})

	mux.HandleFunc("POST /v0/releases/publish", func(w http.ResponseWriter, r *http.Request) {
		var in httpclient.PublishReleaseInput
		_ = json.NewDecoder(r.Body).Decode(&in)
		fb.mu.Lock()
		fb.publishReq = in
		fb.publishHit = true
		fb.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(fb.publishStatus)
		if fb.publishStatus >= 400 {
			_ = json.NewEncoder(w).Encode(errEnvelope(orDefault(fb.publishErrCode, "validation_failed"), "nope"))
			return
		}
		_ = json.NewEncoder(w).Encode(fb.publishResp)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return fb, srv
}

// --- release preview ---

func TestReleasePreview_HappyPath(t *testing.T) {
	fb, srv := newReleaseFake(t)
	withBackend(t, srv)
	fb.previewBody = "# Release notes\n\nsuggested bump: minor (because a feature merged)\n"

	var stdout strings.Builder
	got := run([]string{"release", "preview", "--repo", "acme/app", "--from", "v1.0.0", "--to", "HEAD"}, &stdout, io.Discard)
	if got != exitOK {
		t.Fatalf("preview = %d, want exitOK", got)
	}
	if !strings.Contains(stdout.String(), "suggested bump: minor") {
		t.Errorf("stdout missing rendered markdown:\n%s", stdout.String())
	}
	// The three coordinates cross the CLI->HTTP seam as query params.
	for _, want := range []string{"repo=acme%2Fapp", "from=v1.0.0", "to=HEAD"} {
		if !strings.Contains(fb.previewQuery, want) {
			t.Errorf("query %q missing %q", fb.previewQuery, want)
		}
	}
}

func TestReleasePreview_JSONOutput(t *testing.T) {
	fb, srv := newReleaseFake(t)
	withBackend(t, srv)
	fb.previewBody = "# Notes\nbody\n"

	var stdout strings.Builder
	got := run([]string{"release", "preview", "--repo", "acme/app", "--from", "a", "--to", "b", "--output", "json"}, &stdout, io.Discard)
	if got != exitOK {
		t.Fatalf("preview = %d, want exitOK", got)
	}
	var decoded releasePreviewOutput
	if err := json.NewDecoder(strings.NewReader(stdout.String())).Decode(&decoded); err != nil {
		t.Fatalf("decode json: %v\nstdout: %s", err, stdout.String())
	}
	if decoded.Markdown != "# Notes\nbody\n" {
		t.Errorf("markdown mismatch: %q", decoded.Markdown)
	}
}

func TestReleasePreview_MissingFrom(t *testing.T) {
	fb, srv := newReleaseFake(t)
	withBackend(t, srv)

	var stderr strings.Builder
	got := run([]string{"release", "preview", "--repo", "acme/app", "--to", "HEAD"}, io.Discard, &stderr)
	if got != exitUsage {
		t.Errorf("preview = %d, want exitUsage", got)
	}
	if !strings.Contains(stderr.String(), "--from is required") {
		t.Errorf("stderr missing diagnostic: %s", stderr.String())
	}
	if fb.previewQuery != "" {
		t.Errorf("preview endpoint reached despite missing --from")
	}
}

func TestReleasePreview_BadOutputValue(t *testing.T) {
	_, srv := newReleaseFake(t)
	withBackend(t, srv)
	var stderr strings.Builder
	got := run([]string{"release", "preview", "--repo", "acme/app", "--from", "a", "--to", "b", "--output", "xml"}, io.Discard, &stderr)
	if got != exitUsage {
		t.Errorf("preview = %d, want exitUsage", got)
	}
	if !strings.Contains(stderr.String(), "invalid --output") {
		t.Errorf("stderr missing diagnostic: %s", stderr.String())
	}
}

func TestReleasePreview_APIError401(t *testing.T) {
	fb, srv := newReleaseFake(t)
	withBackend(t, srv)
	fb.previewStatus = http.StatusUnauthorized
	fb.previewErrCode = "authentication_required"

	var stderr strings.Builder
	got := run([]string{"release", "preview", "--repo", "acme/app", "--from", "a", "--to", "b"}, io.Discard, &stderr)
	if got != exitFailure {
		t.Errorf("preview = %d, want exitFailure", got)
	}
	if !strings.Contains(stderr.String(), "authentication_required") {
		t.Errorf("stderr missing api code: %s", stderr.String())
	}
}

// --- release prepare ---

func TestReleasePrepare_HappyPath(t *testing.T) {
	fb, srv := newReleaseFake(t)
	withBackend(t, srv)
	stageID := uuid.New()
	artifactID := uuid.New()
	fb.prepareResp = httpclient.ReleaseNotesResult{
		ArtifactID:  artifactID.String(),
		StageID:     stageID.String(),
		Repo:        "acme/app",
		From:        "v1.0.0",
		To:          "HEAD",
		ContentHash: "deadbeef",
		Markdown:    "# Notes\nbody\n",
	}

	var stdout strings.Builder
	got := run([]string{"release", "prepare", "--repo", "acme/app", "--from", "v1.0.0", "--to", "HEAD", "--stage-id", stageID.String()}, &stdout, io.Discard)
	if got != exitOK {
		t.Fatalf("prepare = %d, want exitOK", got)
	}
	if fb.prepareReq.Repo != "acme/app" || fb.prepareReq.From != "v1.0.0" || fb.prepareReq.To != "HEAD" || fb.prepareReq.StageID != stageID.String() {
		t.Errorf("request body mismatch: %+v", fb.prepareReq)
	}
	out := stdout.String()
	for _, want := range []string{artifactID.String(), stageID.String(), "deadbeef", "# Notes"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing %q:\n%s", want, out)
		}
	}
}

func TestReleasePrepare_JSONOutput(t *testing.T) {
	fb, srv := newReleaseFake(t)
	withBackend(t, srv)
	stageID := uuid.New()
	fb.prepareResp = httpclient.ReleaseNotesResult{
		ArtifactID: uuid.New().String(), StageID: stageID.String(), ContentHash: "abc", Markdown: "x",
	}

	var stdout strings.Builder
	got := run([]string{"release", "prepare", "--repo", "acme/app", "--from", "a", "--to", "b", "--stage-id", stageID.String(), "--output", "json"}, &stdout, io.Discard)
	if got != exitOK {
		t.Fatalf("prepare = %d, want exitOK", got)
	}
	var decoded httpclient.ReleaseNotesResult
	if err := json.NewDecoder(strings.NewReader(stdout.String())).Decode(&decoded); err != nil {
		t.Fatalf("decode json: %v\nstdout: %s", err, stdout.String())
	}
	if decoded.StageID != stageID.String() || decoded.ContentHash != "abc" {
		t.Errorf("decoded mismatch: %+v", decoded)
	}
}

func TestReleasePrepare_MissingStageID(t *testing.T) {
	fb, srv := newReleaseFake(t)
	withBackend(t, srv)
	var stderr strings.Builder
	got := run([]string{"release", "prepare", "--repo", "acme/app", "--from", "a", "--to", "b"}, io.Discard, &stderr)
	if got != exitUsage {
		t.Errorf("prepare = %d, want exitUsage", got)
	}
	if !strings.Contains(stderr.String(), "--stage-id is required") {
		t.Errorf("stderr missing diagnostic: %s", stderr.String())
	}
	if fb.prepareHit {
		t.Errorf("notes endpoint reached despite missing --stage-id")
	}
}

func TestReleasePrepare_BadStageIDUUID(t *testing.T) {
	fb, srv := newReleaseFake(t)
	withBackend(t, srv)
	var stderr strings.Builder
	got := run([]string{"release", "prepare", "--repo", "acme/app", "--from", "a", "--to", "b", "--stage-id", "not-a-uuid"}, io.Discard, &stderr)
	if got != exitUsage {
		t.Errorf("prepare = %d, want exitUsage", got)
	}
	if !strings.Contains(stderr.String(), "not a UUID") {
		t.Errorf("stderr missing 'not a UUID': %s", stderr.String())
	}
	if fb.prepareHit {
		t.Errorf("notes endpoint reached despite local UUID validation failure")
	}
}

func TestReleasePrepare_APIError403(t *testing.T) {
	fb, srv := newReleaseFake(t)
	withBackend(t, srv)
	fb.prepareStatus = http.StatusForbidden
	fb.prepareErrCode = "insufficient_scope"

	var stderr strings.Builder
	got := run([]string{"release", "prepare", "--repo", "acme/app", "--from", "a", "--to", "b", "--stage-id", uuid.New().String()}, io.Discard, &stderr)
	if got != exitFailure {
		t.Errorf("prepare = %d, want exitFailure", got)
	}
	if !strings.Contains(stderr.String(), "insufficient_scope") {
		t.Errorf("stderr missing api code: %s", stderr.String())
	}
}

// --- release cut ---

func TestReleaseCut_HappyPath(t *testing.T) {
	fb, srv := newReleaseFake(t)
	withBackend(t, srv)
	runID := uuid.New()
	artifactID := uuid.New()
	stageID := uuid.New()
	fb.cutResp = httpclient.CutReleaseResult{
		Version: "v1.4.0", ArtifactID: artifactID.String(), ContentHash: "deadbeef",
		BumpLevel: "minor", Recorded: true,
	}

	var stdout strings.Builder
	got := run([]string{"release", "cut",
		"--repo", "acme/app", "--run-id", runID.String(), "--artifact-id", artifactID.String(),
		"--version", "v1.4.0", "--stage-id", stageID.String(), "--bump-level", "minor"}, &stdout, io.Discard)
	if got != exitOK {
		t.Fatalf("cut = %d, want exitOK", got)
	}
	if fb.cutReq.Repo != "acme/app" || fb.cutReq.RunID != runID.String() || fb.cutReq.ArtifactID != artifactID.String() ||
		fb.cutReq.Version != "v1.4.0" || fb.cutReq.StageID != stageID.String() || fb.cutReq.BumpLevel != "minor" {
		t.Errorf("request body mismatch: %+v", fb.cutReq)
	}
	out := stdout.String()
	for _, want := range []string{"v1.4.0", "deadbeef", "minor", "recorded:", "true"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing %q:\n%s", want, out)
		}
	}
	// Binding approval condition: cut records the decision only; the tag push
	// stays a human git action, and the text output must say so explicitly.
	if !strings.Contains(out, "push the git tag") {
		t.Errorf("stdout missing human-tag-push note:\n%s", out)
	}
}

func TestReleaseCut_JSONOutput(t *testing.T) {
	fb, srv := newReleaseFake(t)
	withBackend(t, srv)
	artifactID := uuid.New()
	fb.cutResp = httpclient.CutReleaseResult{Version: "v2.0.0", ArtifactID: artifactID.String(), ContentHash: "h", Recorded: true}

	var stdout strings.Builder
	got := run([]string{"release", "cut", "--repo", "acme/app", "--run-id", uuid.New().String(),
		"--artifact-id", artifactID.String(), "--version", "v2.0.0", "--output", "json"}, &stdout, io.Discard)
	if got != exitOK {
		t.Fatalf("cut = %d, want exitOK", got)
	}
	var decoded httpclient.CutReleaseResult
	if err := json.NewDecoder(strings.NewReader(stdout.String())).Decode(&decoded); err != nil {
		t.Fatalf("decode json: %v\nstdout: %s", err, stdout.String())
	}
	if decoded.Version != "v2.0.0" || !decoded.Recorded {
		t.Errorf("decoded mismatch: %+v", decoded)
	}
	// stage_id is optional and was omitted; the wire body must not carry it.
	if fb.cutReq.StageID != "" {
		t.Errorf("stage_id should be empty when omitted, got %q", fb.cutReq.StageID)
	}
}

func TestReleaseCut_MissingVersion(t *testing.T) {
	fb, srv := newReleaseFake(t)
	withBackend(t, srv)
	var stderr strings.Builder
	got := run([]string{"release", "cut", "--repo", "acme/app", "--run-id", uuid.New().String(), "--artifact-id", uuid.New().String()}, io.Discard, &stderr)
	if got != exitUsage {
		t.Errorf("cut = %d, want exitUsage", got)
	}
	if !strings.Contains(stderr.String(), "--version is required") {
		t.Errorf("stderr missing diagnostic: %s", stderr.String())
	}
	if fb.cutHit {
		t.Errorf("cut endpoint reached despite missing --version")
	}
}

func TestReleaseCut_BadRunIDUUID(t *testing.T) {
	fb, srv := newReleaseFake(t)
	withBackend(t, srv)
	var stderr strings.Builder
	got := run([]string{"release", "cut", "--repo", "acme/app", "--run-id", "not-a-uuid", "--artifact-id", uuid.New().String(), "--version", "v1.0.0"}, io.Discard, &stderr)
	if got != exitUsage {
		t.Errorf("cut = %d, want exitUsage", got)
	}
	if !strings.Contains(stderr.String(), "not a UUID") {
		t.Errorf("stderr missing 'not a UUID': %s", stderr.String())
	}
	if fb.cutHit {
		t.Errorf("cut endpoint reached despite local UUID validation failure")
	}
}

func TestReleaseCut_BadOptionalStageIDUUID(t *testing.T) {
	fb, srv := newReleaseFake(t)
	withBackend(t, srv)
	var stderr strings.Builder
	got := run([]string{"release", "cut", "--repo", "acme/app", "--run-id", uuid.New().String(),
		"--artifact-id", uuid.New().String(), "--version", "v1.0.0", "--stage-id", "not-a-uuid"}, io.Discard, &stderr)
	if got != exitUsage {
		t.Errorf("cut = %d, want exitUsage", got)
	}
	if !strings.Contains(stderr.String(), "not a UUID") {
		t.Errorf("stderr missing 'not a UUID': %s", stderr.String())
	}
	if fb.cutHit {
		t.Errorf("cut endpoint reached despite bad optional --stage-id")
	}
}

func TestReleaseCut_APIError409(t *testing.T) {
	fb, srv := newReleaseFake(t)
	withBackend(t, srv)
	fb.cutStatus = http.StatusConflict
	fb.cutErrCode = "artifact_wrong_kind"

	var stderr strings.Builder
	got := run([]string{"release", "cut", "--repo", "acme/app", "--run-id", uuid.New().String(),
		"--artifact-id", uuid.New().String(), "--version", "v1.0.0"}, io.Discard, &stderr)
	if got != exitFailure {
		t.Errorf("cut = %d, want exitFailure", got)
	}
	if !strings.Contains(stderr.String(), "artifact_wrong_kind") {
		t.Errorf("stderr missing api code: %s", stderr.String())
	}
}

// --- release publish ---

func TestReleasePublish_HappyPath(t *testing.T) {
	fb, srv := newReleaseFake(t)
	withBackend(t, srv)
	runID := uuid.New()
	artifactID := uuid.New()
	fb.publishResp = httpclient.PublishReleaseResult{
		ReleaseURL: "https://github.com/acme/app/releases/tag/v1.4.0", Tag: "v1.4.0",
		ArtifactID: artifactID.String(), ContentHash: "deadbeef", Published: true, Idempotent: false,
	}

	var stdout strings.Builder
	got := run([]string{"release", "publish", "--repo", "acme/app", "--tag", "v1.4.0",
		"--run-id", runID.String(), "--artifact-id", artifactID.String()}, &stdout, io.Discard)
	if got != exitOK {
		t.Fatalf("publish = %d, want exitOK", got)
	}
	if fb.publishReq.Repo != "acme/app" || fb.publishReq.Tag != "v1.4.0" ||
		fb.publishReq.RunID != runID.String() || fb.publishReq.ArtifactID != artifactID.String() {
		t.Errorf("request body mismatch: %+v", fb.publishReq)
	}
	out := stdout.String()
	for _, want := range []string{"https://github.com/acme/app/releases/tag/v1.4.0", "v1.4.0", "deadbeef", "published:", "true"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing %q:\n%s", want, out)
		}
	}
}

func TestReleasePublish_JSONOutput_Idempotent(t *testing.T) {
	fb, srv := newReleaseFake(t)
	withBackend(t, srv)
	fb.publishResp = httpclient.PublishReleaseResult{
		ReleaseURL: "https://x/y", Tag: "v1.0.0", ArtifactID: uuid.New().String(),
		ContentHash: "h", Published: false, Idempotent: true,
	}

	var stdout strings.Builder
	got := run([]string{"release", "publish", "--repo", "acme/app", "--tag", "v1.0.0",
		"--run-id", uuid.New().String(), "--artifact-id", uuid.New().String(), "--output", "json"}, &stdout, io.Discard)
	if got != exitOK {
		t.Fatalf("publish = %d, want exitOK", got)
	}
	var decoded httpclient.PublishReleaseResult
	if err := json.NewDecoder(strings.NewReader(stdout.String())).Decode(&decoded); err != nil {
		t.Fatalf("decode json: %v\nstdout: %s", err, stdout.String())
	}
	if decoded.Published || !decoded.Idempotent {
		t.Errorf("expected a no-op re-invoke (published=false idempotent=true): %+v", decoded)
	}
}

func TestReleasePublish_MissingTag(t *testing.T) {
	fb, srv := newReleaseFake(t)
	withBackend(t, srv)
	var stderr strings.Builder
	got := run([]string{"release", "publish", "--repo", "acme/app", "--run-id", uuid.New().String(), "--artifact-id", uuid.New().String()}, io.Discard, &stderr)
	if got != exitUsage {
		t.Errorf("publish = %d, want exitUsage", got)
	}
	if !strings.Contains(stderr.String(), "--tag is required") {
		t.Errorf("stderr missing diagnostic: %s", stderr.String())
	}
	if fb.publishHit {
		t.Errorf("publish endpoint reached despite missing --tag")
	}
}

func TestReleasePublish_BadArtifactIDUUID(t *testing.T) {
	fb, srv := newReleaseFake(t)
	withBackend(t, srv)
	var stderr strings.Builder
	got := run([]string{"release", "publish", "--repo", "acme/app", "--tag", "v1", "--run-id", uuid.New().String(), "--artifact-id", "not-a-uuid"}, io.Discard, &stderr)
	if got != exitUsage {
		t.Errorf("publish = %d, want exitUsage", got)
	}
	if !strings.Contains(stderr.String(), "not a UUID") {
		t.Errorf("stderr missing 'not a UUID': %s", stderr.String())
	}
	if fb.publishHit {
		t.Errorf("publish endpoint reached despite local UUID validation failure")
	}
}

func TestReleasePublish_APIError404(t *testing.T) {
	fb, srv := newReleaseFake(t)
	withBackend(t, srv)
	fb.publishStatus = http.StatusNotFound
	fb.publishErrCode = "release_not_found"

	var stderr strings.Builder
	got := run([]string{"release", "publish", "--repo", "acme/app", "--tag", "v9.9.9",
		"--run-id", uuid.New().String(), "--artifact-id", uuid.New().String()}, io.Discard, &stderr)
	if got != exitFailure {
		t.Errorf("publish = %d, want exitFailure", got)
	}
	if !strings.Contains(stderr.String(), "release_not_found") {
		t.Errorf("stderr missing api code: %s", stderr.String())
	}
}

func TestReleasePublish_BadOutputValue(t *testing.T) {
	_, srv := newReleaseFake(t)
	withBackend(t, srv)
	var stderr strings.Builder
	got := run([]string{"release", "publish", "--repo", "acme/app", "--tag", "v1",
		"--run-id", uuid.New().String(), "--artifact-id", uuid.New().String(), "--output", "xml"}, io.Discard, &stderr)
	if got != exitUsage {
		t.Errorf("publish = %d, want exitUsage", got)
	}
	if !strings.Contains(stderr.String(), "invalid --output") {
		t.Errorf("stderr missing diagnostic: %s", stderr.String())
	}
}

// --- dispatcher ---

func TestRelease_NoSubcommand(t *testing.T) {
	var stderr strings.Builder
	got := run([]string{"release"}, io.Discard, &stderr)
	if got != exitUsage {
		t.Errorf("release = %d, want exitUsage", got)
	}
	if !strings.Contains(stderr.String(), "subcommand required") {
		t.Errorf("stderr missing diagnostic: %s", stderr.String())
	}
}

func TestRelease_UnknownSubcommand(t *testing.T) {
	var stderr strings.Builder
	got := run([]string{"release", "frobnicate"}, io.Discard, &stderr)
	if got != exitUsage {
		t.Errorf("release = %d, want exitUsage", got)
	}
	if !strings.Contains(stderr.String(), "unknown subcommand") {
		t.Errorf("stderr missing diagnostic: %s", stderr.String())
	}
}
