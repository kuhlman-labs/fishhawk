package server

// refinement_conventions_test.go pins the refinement surface's move onto the
// per-repo conventions loader (ADR-110 / ADR-114, #4626): repo-scoped sessions,
// the four resolution states on the session view's conventions_source block,
// the fail-closed 502 refinement_conventions_unavailable on every arm (with
// nothing recorded on the writing arms), the open-arm and read-side visibility
// gates, and the file arm's open-time repo pin. Tests that install the
// conventionsResolver / conventionsLoader seams mutate package-level state, so
// none calls t.Parallel and each restores the seam in t.Cleanup.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/account"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/refinement"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// repoDeclarationTarget is this repository's own coordinate: its committed
// declaration's project owner (kuhlman-labs) destination-authorizes it.
const repoDeclarationTarget = "kuhlman-labs/fishhawk"

// repoDeclarationBytes reads this repository's own work-management
// declaration READ-ONLY, anchored on pkgSrcDir (never runtime.Caller, #4179).
func repoDeclarationBytes(t *testing.T) []byte {
	t.Helper()
	path := filepath.Join(pkgSrcDir, "..", "..", "..", ".fishhawk", "work-management.yaml")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v — this repository's own declaration must exist", path, err)
	}
	return b
}

// refinementPreviewGolden is the refinement package's pinned {draft, preview}
// golden (captured from the pre-move RenderDraft path, slice 1).
type refinementPreviewGolden struct {
	Draft   refinement.EpicDraft `json:"draft"`
	Preview []workmgmt.WorkItem  `json:"preview"`
}

func readRefinementPreviewGolden(t *testing.T) ([]byte, refinementPreviewGolden) {
	t.Helper()
	path := filepath.Join(pkgSrcDir, "..", "refinement", "testdata", "repo-declaration-preview.golden.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v", path, err)
	}
	var g refinementPreviewGolden
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatalf("decode golden: %v", err)
	}
	return raw, g
}

// installRefinementLoader installs a REAL RepoConventionsLoader built from cfg
// as BOTH process-wide seams (as serve.go does, on one instance), restoring
// the defaults on cleanup.
func installRefinementLoader(t *testing.T, cfg RepoConventionsLoaderConfig) {
	t.Helper()
	l := NewRepoConventionsLoader(cfg)
	prevLoader, prevResolver := conventionsLoader, conventionsResolver
	SetConventionsLoader(l.Load)
	SetConventionsResolver(l.Resolve)
	t.Cleanup(func() {
		conventionsLoader, conventionsResolver = prevLoader, prevResolver
	})
}

// declaringLoaderConfig is a github-resolved loader whose fetcher serves body.
func declaringLoaderConfig(fetcher *fakeFileFetcher) RepoConventionsLoaderConfig {
	return RepoConventionsLoaderConfig{
		Resolver:      &fakeProviderResolver{provider: "github", found: true},
		GitHubFetcher: fetcher,
		GitHubScope:   githubScopeFixed("installation:42"),
	}
}

func declarationFetcher(body []byte) *fakeFileFetcher {
	return &fakeFileFetcher{fc: &forge.FileContent{Content: body, SHA: "sha-decl"}}
}

// openReq builds an authed POST /v0/refinement/sessions request.
func openReq(t *testing.T, brief, repo string) *http.Request {
	t.Helper()
	body := map[string]string{"brief": brief}
	if repo != "" {
		body["repo"] = repo
	}
	return refinementReq(http.MethodPost, "/v0/refinement/sessions", "", mustJSON(t, body))
}

func decodeSessionView(t *testing.T, rec *httptest.ResponseRecorder) refinementSessionView {
	t.Helper()
	var view refinementSessionView
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode session view: %v (body=%s)", err, rec.Body.String())
	}
	return view
}

// assertConventionsUnavailable asserts a 502 refinement_conventions_unavailable
// whose details carry {repo} only (approval condition C2: the cause rides
// internalCauseKey to the operator log, never the caller).
func assertConventionsUnavailable(t *testing.T, rec *httptest.ResponseRecorder, repo string) {
	t.Helper()
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (body=%s)", rec.Code, rec.Body.String())
	}
	var env struct {
		Error struct {
			Code    string         `json:"code"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode error envelope: %v (body=%s)", err, rec.Body.String())
	}
	if env.Error.Code != "refinement_conventions_unavailable" {
		t.Errorf("code = %q, want refinement_conventions_unavailable", env.Error.Code)
	}
	if env.Error.Details["repo"] != repo {
		t.Errorf("details.repo = %v, want %q (details=%v)", env.Error.Details["repo"], repo, env.Error.Details)
	}
}

// TestRefinementOpen_RepoDeclarationPreviewMatchesGolden is the post-move half
// of the golden pin: the open arm, scoped to this repository and resolved
// through a REAL loader serving this repository's declaration, renders a
// preview byte-equal to the golden captured from the pre-move path.
func TestRefinementOpen_RepoDeclarationPreviewMatchesGolden(t *testing.T) {
	goldenBytes, golden := readRefinementPreviewGolden(t)
	fetcher := declarationFetcher(repoDeclarationBytes(t))
	installRefinementLoader(t, declaringLoaderConfig(fetcher))

	repo := refinement.NewPostgresRepository(pgtest.NewPool(t))
	drafter := &fakeRefinementDrafter{draft: golden.Draft, model: "m"}
	s := New(Config{RefinementRepo: repo, AuditRepo: okAuditRepo{}, RefinementDrafter: drafter})

	rec := httptest.NewRecorder()
	s.handleCreateRefinementSession(rec, openReq(t, "golden brief", repoDeclarationTarget))
	if rec.Code != http.StatusCreated {
		t.Fatalf("open status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}
	opened := decodeSessionView(t, rec)
	if opened.ConventionsSource.State != ConventionsStateDeclared || opened.ConventionsSource.Repo != repoDeclarationTarget {
		t.Errorf("open conventions_source = %+v, want declared for %s", opened.ConventionsSource, repoDeclarationTarget)
	}
	// The drafter received the repository's DECLARED conventions.
	if ft, ok := drafter.sawConv.Types["feature"]; !ok || strings.Join(ft.RequiredLabelNamespaces, ",") != "area,autonomy,phase" {
		t.Errorf("drafter conventions feature namespaces = %v, want this repository's area,autonomy,phase", drafter.sawConv.Types["feature"].RequiredLabelNamespaces)
	}

	view := getSessionView(t, s, opened.SessionID)
	if view.ConventionsSource.State != ConventionsStateDeclared || view.ConventionsSource.Fallback != "" {
		t.Errorf("GET conventions_source = %+v, want declared with no fallback", view.ConventionsSource)
	}
	got, err := json.MarshalIndent(refinementPreviewGolden{Draft: view.LatestDraft, Preview: view.Preview}, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got = append(got, '\n')
	if !bytes.Equal(got, goldenBytes) {
		t.Errorf("post-move preview diverged from the golden:\n--- got ---\n%s\n--- want ---\n%s", got, goldenBytes)
	}
	if fetcher.calls != 1 {
		t.Errorf("fetches = %d, want 1 (the GET reuses the cached parse)", fetcher.calls)
	}

	drafts, err := repo.ListForSession(context.Background(), opened.SessionID)
	if err != nil || len(drafts) != 1 || drafts[0].Repo != repoDeclarationTarget {
		t.Fatalf("persisted drafts = %+v, %v; want one revision carrying repo %s", drafts, err, repoDeclarationTarget)
	}
}

// TestRefinementConventions_PreFetchFallThroughDegradesByName runs the
// pre-fetch fall-throughs a well-formed repo can reach — the four FO-named
// ones plus provider_resolver_unconfigured (a loader installed with a nil
// Resolver) — through the REAL loader on the open arm and GET: each degrades
// by name with ZERO fetches, never a silent default. repo_malformed cannot
// reach this path (the open and file arms answer 400 on a non-owner/name repo
// first); TestRepoConventionsLoader_ResolveNamesEachFallThrough pins it at
// loader level.
func TestRefinementConventions_PreFetchFallThroughDegradesByName(t *testing.T) {
	cases := []struct {
		name   string
		cfg    func(f *fakeFileFetcher) RepoConventionsLoaderConfig
		reason string
	}{
		{"provider_resolver_unconfigured", func(f *fakeFileFetcher) RepoConventionsLoaderConfig {
			return RepoConventionsLoaderConfig{GitHubFetcher: f, GitHubScope: githubScopeFixed("installation:1")}
		}, ConventionsDegradedProviderResolverUnconfigured},
		{"no_provider_account", func(f *fakeFileFetcher) RepoConventionsLoaderConfig {
			return RepoConventionsLoaderConfig{Resolver: &fakeProviderResolver{found: false}, GitHubFetcher: f, GitHubScope: githubScopeFixed("installation:1")}
		}, ConventionsDegradedNoProviderAccount},
		{"forge_unregistered", func(*fakeFileFetcher) RepoConventionsLoaderConfig {
			return RepoConventionsLoaderConfig{Resolver: &fakeProviderResolver{provider: "github", found: true}, GitHubScope: githubScopeFixed("installation:1")}
		}, ConventionsDegradedForgeUnregistered},
		{"app_not_installed", func(f *fakeFileFetcher) RepoConventionsLoaderConfig {
			return RepoConventionsLoaderConfig{Resolver: &fakeProviderResolver{provider: "github", found: true}, GitHubFetcher: f,
				GitHubScope: func(context.Context, string, string) (forge.CredentialScope, error) {
					return forge.CredentialScope{}, nil
				}}
		}, ConventionsDegradedAppNotInstalled},
		{"forge_credentials_unconfigured", func(f *fakeFileFetcher) RepoConventionsLoaderConfig {
			return RepoConventionsLoaderConfig{Resolver: &fakeProviderResolver{provider: "gitlab", found: true}, GitLabFetcher: f}
		}, ConventionsDegradedForgeCredentialsUnconfigured},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fetcher := declarationFetcher(repoDeclarationBytes(t))
			installRefinementLoader(t, tc.cfg(fetcher))
			repo := &disconnectRefinementRepo{}
			s := New(Config{RefinementRepo: repo, AuditRepo: okAuditRepo{}, RefinementDrafter: &fakeRefinementDrafter{draft: refinementValidDraft()}})

			rec := httptest.NewRecorder()
			s.handleCreateRefinementSession(rec, openReq(t, "b", "acme/widgets"))
			if rec.Code != http.StatusCreated {
				t.Fatalf("open status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
			}
			want := refinementConventionsSourceView{Repo: "acme/widgets", State: ConventionsStateDegraded, DegradedReason: tc.reason, Fallback: ConventionsFallbackShippedDefault}
			if got := decodeSessionView(t, rec).ConventionsSource; got != want {
				t.Errorf("open conventions_source = %+v, want %+v", got, want)
			}
			if got := getSessionView(t, s, repo.drafts[0].SessionID).ConventionsSource; got != want {
				t.Errorf("GET conventions_source = %+v, want %+v", got, want)
			}
			if fetcher.calls != 0 {
				t.Errorf("fetches = %d, want 0 (a pre-fetch fall-through never fetches)", fetcher.calls)
			}
		})
	}
}

// TestRefinementOpen_NotDeclaredNamesFallback: a repo whose fetch SUCCEEDS
// with forge.ErrNotFound is not_declared, previewed with the shipped default.
func TestRefinementOpen_NotDeclaredNamesFallback(t *testing.T) {
	fetcher := &fakeFileFetcher{err: forge.ErrNotFound}
	installRefinementLoader(t, declaringLoaderConfig(fetcher))
	repo := &disconnectRefinementRepo{}
	s := New(Config{RefinementRepo: repo, AuditRepo: okAuditRepo{}, RefinementDrafter: &fakeRefinementDrafter{draft: refinementValidDraft()}})

	rec := httptest.NewRecorder()
	s.handleCreateRefinementSession(rec, openReq(t, "b", "acme/widgets"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("open status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}
	view := decodeSessionView(t, rec)
	want := refinementConventionsSourceView{Repo: "acme/widgets", State: ConventionsStateNotDeclared, Fallback: ConventionsFallbackShippedDefault}
	if view.ConventionsSource != want {
		t.Errorf("conventions_source = %+v, want %+v", view.ConventionsSource, want)
	}
	wantPreview, err := refinement.RenderDraft(refinementValidDraft(), refinement.RenderOptions{}, workmgmt.Default())
	if err != nil {
		t.Fatalf("RenderDraft: %v", err)
	}
	if mustJSON(t, view.Preview) != mustJSON(t, wantPreview) {
		t.Errorf("preview = %s, want the Default() render %s", mustJSON(t, view.Preview), mustJSON(t, wantPreview))
	}
	if fetcher.calls != 1 {
		t.Errorf("fetches = %d, want 1", fetcher.calls)
	}
}

// TestRefinementOpen_NoRepoUsesShippedDefault: a repo-less open is no_repo over
// the shipped default, persists no repo, and never consults the resolver.
func TestRefinementOpen_NoRepoUsesShippedDefault(t *testing.T) {
	prev := conventionsResolver
	SetConventionsResolver(func(context.Context, string) (ConventionsResolution, error) {
		t.Error("the resolver was consulted for a repo-less session")
		return ConventionsResolution{}, errors.New("unexpected")
	})
	t.Cleanup(func() { conventionsResolver = prev })

	repo := &disconnectRefinementRepo{}
	drafter := &fakeRefinementDrafter{draft: refinementValidDraft()}
	s := New(Config{RefinementRepo: repo, AuditRepo: okAuditRepo{}, RefinementDrafter: drafter})
	rec := httptest.NewRecorder()
	s.handleCreateRefinementSession(rec, openReq(t, "b", ""))
	if rec.Code != http.StatusCreated {
		t.Fatalf("open status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}
	want := refinementConventionsSourceView{State: ConventionsStateNoRepo, Fallback: ConventionsFallbackShippedDefault}
	if got := decodeSessionView(t, rec).ConventionsSource; got != want {
		t.Errorf("conventions_source = %+v, want %+v", got, want)
	}
	if len(repo.drafts) != 1 || repo.drafts[0].Repo != "" {
		t.Errorf("persisted drafts = %+v, want one with no repo", repo.drafts)
	}
	if drafter.sawConv.Provider != workmgmt.Default().Provider {
		t.Errorf("drafter conventions provider = %q, want the shipped default's", drafter.sawConv.Provider)
	}
}

// failingLoaderCases are the fail-closed resolutions: every one is an ERROR
// from the real loader, which must never be replaced by a fallback.
func failingLoaderCases(t *testing.T) map[string]RepoConventionsLoaderConfig {
	decl := repoDeclarationBytes(t)
	gh := &fakeProviderResolver{provider: "github", found: true}
	return map[string]RepoConventionsLoaderConfig{
		"fetch_error": {Resolver: gh, GitHubScope: githubScopeFixed("installation:1"),
			GitHubFetcher: &fakeFileFetcher{err: errors.New("forge 500")}},
		"unparseable_declaration": {Resolver: gh, GitHubScope: githubScopeFixed("installation:1"),
			GitHubFetcher: declarationFetcher([]byte("types: [not a map"))},
		// This repository's declaration names project owner kuhlman-labs, so
		// it does not destination-authorize a filing into acme/*.
		"destination_refused": {Resolver: gh, GitHubScope: githubScopeFixed("installation:1"),
			GitHubFetcher: declarationFetcher(decl)},
		"resolver_error": {Resolver: &fakeProviderResolver{err: errors.New("accounts down")},
			GitHubFetcher: declarationFetcher(decl), GitHubScope: githubScopeFixed("installation:1")},
		"scope_error": {Resolver: gh, GitHubFetcher: declarationFetcher(decl),
			GitHubScope: func(context.Context, string, string) (forge.CredentialScope, error) {
				return forge.CredentialScope{}, errors.New("installation lookup 502")
			}},
	}
}

// TestRefinementOpen_ConventionsUnavailableRecordsNothing: each resolution
// error answers 502 BEFORE the drafter runs, and nothing is persisted.
func TestRefinementOpen_ConventionsUnavailableRecordsNothing(t *testing.T) {
	for name, cfg := range failingLoaderCases(t) {
		t.Run(name, func(t *testing.T) {
			installRefinementLoader(t, cfg)
			repo := &disconnectRefinementRepo{}
			drafter := &fakeRefinementDrafter{draft: refinementValidDraft()}
			s := New(Config{RefinementRepo: repo, AuditRepo: okAuditRepo{}, RefinementDrafter: drafter})

			rec := httptest.NewRecorder()
			s.handleCreateRefinementSession(rec, openReq(t, "b", "acme/widgets"))
			assertConventionsUnavailable(t, rec, "acme/widgets")
			if len(repo.drafts) != 0 || drafter.calls != 0 {
				t.Errorf("committed state: %d revisions, %d drafter calls; want 0 and 0", len(repo.drafts), drafter.calls)
			}
		})
	}
}

// seedRepoScopedSession persists one revision opened for repoName (unfiled,
// undecided) and returns its session id.
func seedRepoScopedSession(t *testing.T, repo refinement.Repository, repoName string) uuid.UUID {
	t.Helper()
	sessionID := uuid.New()
	if _, err := repo.CreateDraft(context.Background(), refinement.CreateParams{
		SessionID: sessionID, Brief: "b", Draft: refinementValidDraft(), Origin: refinement.OriginBrief, Repo: repoName,
	}); err != nil {
		t.Fatalf("CreateDraft: %v", err)
	}
	return sessionID
}

func TestRefinementGet_ConventionsUnavailableIs502(t *testing.T) {
	installRefinementLoader(t, failingLoaderCases(t)["fetch_error"])
	repo := refinement.NewPostgresRepository(pgtest.NewPool(t))
	sessionID := seedRepoScopedSession(t, repo, "acme/widgets")
	s := New(Config{RefinementRepo: repo, AuditRepo: okAuditRepo{}})

	rec := httptest.NewRecorder()
	s.handleGetRefinementSession(rec, refinementReq(http.MethodGet, "/v0/refinement/sessions/"+sessionID.String(), sessionID.String(), ""))
	assertConventionsUnavailable(t, rec, "acme/widgets")
}

// TestRefinementPatch_ConventionsUnavailableRecordsNothing: both PATCH arms
// resolve BEFORE the audit append and the persist, so a 502 leaves the
// revision count and the global chain unchanged.
func TestRefinementPatch_ConventionsUnavailableRecordsNothing(t *testing.T) {
	arms := map[string]string{
		"amendment":   `{"brief_amendment":"add a child"}`,
		"direct_edit": mustJSON(t, map[string]any{"draft": refinementValidDraft()}),
	}
	for name, body := range arms {
		t.Run(name, func(t *testing.T) {
			installRefinementLoader(t, failingLoaderCases(t)["fetch_error"])
			pool := pgtest.NewPool(t)
			repo := refinement.NewPostgresRepository(pool)
			auditRepo := audit.NewPostgresRepository(pool)
			sessionID := seedRepoScopedSession(t, repo, "acme/widgets")
			drafter := &fakeRefinementDrafter{draft: refinementValidDraft()}
			s := New(Config{RefinementRepo: repo, AuditRepo: auditRepo, RefinementDrafter: drafter})

			rec := httptest.NewRecorder()
			s.handlePatchRefinementDraft(rec, refinementReq(http.MethodPatch,
				"/v0/refinement/sessions/"+sessionID.String()+"/draft", sessionID.String(), body))
			assertConventionsUnavailable(t, rec, "acme/widgets")

			drafts, err := repo.ListForSession(context.Background(), sessionID)
			if err != nil || len(drafts) != 1 {
				t.Errorf("revisions = %d (%v), want 1 (nothing persisted)", len(drafts), err)
			}
			if n := countGlobalCategory(t, auditRepo, "refinement_draft_edited"); n != 0 {
				t.Errorf("refinement_draft_edited entries = %d, want 0", n)
			}
			if drafter.calls != 0 {
				t.Errorf("drafter calls = %d, want 0", drafter.calls)
			}
		})
	}
}

// TestRefinementDecision_ConventionsUnavailableRecordsNothing: the decision arm
// resolves BEFORE its audit append and RecordDecision.
func TestRefinementDecision_ConventionsUnavailableRecordsNothing(t *testing.T) {
	installRefinementLoader(t, failingLoaderCases(t)["fetch_error"])
	pool := pgtest.NewPool(t)
	repo := refinement.NewPostgresRepository(pool)
	auditRepo := audit.NewPostgresRepository(pool)
	sessionID := seedRepoScopedSession(t, repo, "acme/widgets")
	s := New(Config{RefinementRepo: repo, AuditRepo: auditRepo})

	rec := httptest.NewRecorder()
	s.handleDecideRefinementSession(rec, refinementReq(http.MethodPost,
		"/v0/refinement/sessions/"+sessionID.String()+"/decision", sessionID.String(), `{"decision":"approved","reason":"ok"}`))
	assertConventionsUnavailable(t, rec, "acme/widgets")

	decisions, err := repo.ListDecisions(context.Background(), sessionID)
	if err != nil || len(decisions) != 0 {
		t.Errorf("decisions = %d (%v), want 0 (nothing recorded)", len(decisions), err)
	}
	if n := countGlobalCategory(t, auditRepo, "refinement_draft_approved"); n != 0 {
		t.Errorf("refinement_draft_approved entries = %d, want 0", n)
	}
}

// TestRefinementPatch_CarriesRepoForward: both PATCH arms carry the open-time
// repo onto the new revision, so the session stays repo-scoped (declared).
func TestRefinementPatch_CarriesRepoForward(t *testing.T) {
	arms := map[string]string{
		"amendment":   `{"brief_amendment":"add a child"}`,
		"direct_edit": mustJSON(t, map[string]any{"draft": refinementValidDraft()}),
	}
	for name, body := range arms {
		t.Run(name, func(t *testing.T) {
			installRefinementLoader(t, declaringLoaderConfig(declarationFetcher(repoDeclarationBytes(t))))
			pool := pgtest.NewPool(t)
			repo := refinement.NewPostgresRepository(pool)
			sessionID := seedRepoScopedSession(t, repo, repoDeclarationTarget)
			s := New(Config{RefinementRepo: repo, AuditRepo: audit.NewPostgresRepository(pool), RefinementDrafter: &fakeRefinementDrafter{draft: refinementValidDraft()}})

			rec := httptest.NewRecorder()
			s.handlePatchRefinementDraft(rec, refinementReq(http.MethodPatch,
				"/v0/refinement/sessions/"+sessionID.String()+"/draft", sessionID.String(), body))
			if rec.Code != http.StatusOK {
				t.Fatalf("PATCH status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
			}
			if cs := decodeSessionView(t, rec).ConventionsSource; cs.State != ConventionsStateDeclared || cs.Repo != repoDeclarationTarget {
				t.Errorf("conventions_source = %+v, want declared for %s", cs, repoDeclarationTarget)
			}
			drafts, err := repo.ListForSession(context.Background(), sessionID)
			if err != nil || len(drafts) != 2 {
				t.Fatalf("revisions = %d (%v), want 2", len(drafts), err)
			}
			if drafts[1].Repo != repoDeclarationTarget {
				t.Errorf("new revision repo = %q, want %q carried forward", drafts[1].Repo, repoDeclarationTarget)
			}
		})
	}
}

func TestRefinementOpen_MalformedRepoIs400(t *testing.T) {
	for _, bad := range []string{"no-slash", "a/b/c", "/name", "owner/"} {
		t.Run(bad, func(t *testing.T) {
			repo := &disconnectRefinementRepo{}
			drafter := &fakeRefinementDrafter{draft: refinementValidDraft()}
			s := New(Config{RefinementRepo: repo, AuditRepo: okAuditRepo{}, RefinementDrafter: drafter})
			rec := httptest.NewRecorder()
			s.handleCreateRefinementSession(rec, openReq(t, "b", bad))
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"field":"repo"`) {
				t.Fatalf("status = %d, want 400 naming field repo (body=%s)", rec.Code, rec.Body.String())
			}
			if len(repo.drafts) != 0 || drafter.calls != 0 {
				t.Errorf("committed state: %d revisions, %d drafter calls; want 0 and 0", len(repo.drafts), drafter.calls)
			}
		})
	}
}

// refinementCookieOpenReq is an open-arm request under a COOKIE identity — the
// only identity kind repo filtering applies to.
func refinementCookieOpenReq(t *testing.T, repo string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/v0/refinement/sessions",
		bytes.NewReader([]byte(mustJSON(t, map[string]string{"brief": "b", "repo": repo}))))
	return withIdentity(r, memberIdentity())
}

// TestRefinementOpen_RepoNotVisibleIs403: a cookie caller without read
// visibility of the named repo is refused BEFORE its declaration is fetched,
// and nothing is persisted (the ADR-114-ratified open-arm gate).
func TestRefinementOpen_RepoNotVisibleIs403(t *testing.T) {
	fetcher := declarationFetcher(repoDeclarationBytes(t))
	installRefinementLoader(t, RepoConventionsLoaderConfig{
		Resolver: mapProviderResolver{"acme/secret": "github"}, GitHubFetcher: fetcher, GitHubScope: githubScopeFixed("installation:1"),
		AllowedDestinations: nil,
	})
	repo := &disconnectRefinementRepo{}
	drafter := &fakeRefinementDrafter{draft: refinementValidDraft()}
	vis := newFakeRepoVisibility(map[string]bool{"acme/visible": true})
	s := New(Config{
		Addr: "127.0.0.1:0", RefinementRepo: repo, AuditRepo: okAuditRepo{}, RefinementDrafter: drafter,
		AccountRoles: fakeAccountRoles{role: account.RoleMember}, RepoVisibility: vis,
	})

	rec := httptest.NewRecorder()
	s.handleCreateRefinementSession(rec, refinementCookieOpenReq(t, "acme/secret"))
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "repo_forbidden") {
		t.Fatalf("status = %d, want 403 repo_forbidden (body=%s)", rec.Code, rec.Body.String())
	}
	if fetcher.calls != 0 || len(repo.drafts) != 0 || drafter.calls != 0 {
		t.Errorf("fetches=%d revisions=%d drafter calls=%d, want 0/0/0", fetcher.calls, len(repo.drafts), drafter.calls)
	}

	// A visible repo passes the gate (then resolves: not declared here).
	fetcher.err, fetcher.fc = forge.ErrNotFound, nil
	rec = httptest.NewRecorder()
	s.handleCreateRefinementSession(rec, refinementCookieOpenReq(t, "acme/visible"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("visible repo status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}
}

// TestRefinementGet_OpenTimeRepoVisibilityFiltered: an UNFILED repo-scoped
// session is visibility-filtered on read by its open-time repo (no filing
// session exists, so only StoredDraft.Repo can name it).
func TestRefinementGet_OpenTimeRepoVisibilityFiltered(t *testing.T) {
	sessionID := uuid.New()
	repo := &seededRefinementRepo{drafts: []*refinement.StoredDraft{{
		ID: uuid.New(), SessionID: sessionID, Brief: "b", Draft: refinementValidDraft(), Origin: refinement.OriginBrief, Repo: "acme/secret",
	}}}
	vis := newFakeRepoVisibility(map[string]bool{"acme/visible": true})
	s := New(Config{
		Addr: "127.0.0.1:0", RefinementRepo: repo, AuditRepo: okAuditRepo{},
		AccountRoles: fakeAccountRoles{role: account.RoleMember}, RepoVisibility: vis,
	})
	rec := httptest.NewRecorder()
	s.handleGetRefinementSession(rec, refinementCookieReq(http.MethodGet, "/v0/refinement/sessions/"+sessionID.String(), sessionID.String()))
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "repo_forbidden") {
		t.Fatalf("status = %d, want 403 repo_forbidden (body=%s)", rec.Code, rec.Body.String())
	}
}

// approvedRepoScopedSession seeds an approved, hash-pinned revision opened for
// repoName (no filing session).
func approvedRepoScopedSession(t *testing.T, repo refinement.Repository, repoName string) (uuid.UUID, uuid.UUID) {
	t.Helper()
	sessionID := uuid.New()
	d := refinementValidDraft()
	stored, err := repo.CreateDraft(context.Background(), refinement.CreateParams{SessionID: sessionID, Brief: "b", Draft: d, Repo: repoName})
	if err != nil {
		t.Fatalf("CreateDraft: %v", err)
	}
	hash, err := refinement.ContentHash(d)
	if err != nil {
		t.Fatalf("ContentHash: %v", err)
	}
	if _, err := repo.RecordDecision(context.Background(), refinement.DecisionParams{
		SessionID: sessionID, DraftID: stored.ID, Decision: refinement.DecisionApproved, Reason: "ok", DraftContentHash: hash,
	}); err != nil {
		t.Fatalf("RecordDecision: %v", err)
	}
	return sessionID, stored.ID
}

// TestRefinementFile_OpenTimeRepoMismatch409: the approved revision carries
// the open-time repo acme/a and NO filing session exists, isolating the
// open-time pin from the filing-session pin.
func TestRefinementFile_OpenTimeRepoMismatch409(t *testing.T) {
	repo := refinement.NewPostgresRepository(pgtest.NewPool(t))
	sessionID, draftID := approvedRepoScopedSession(t, repo, "acme/a")
	s := New(Config{RefinementRepo: repo, AuditRepo: okAuditRepo{}})
	t.Cleanup(func() { waitFilingBounded(t, s) })

	rec := postFile(s, sessionID, "acme/b")
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "refinement_filing_repo_mismatch") ||
		!strings.Contains(rec.Body.String(), `"pinned_repo":"acme/a"`) {
		t.Fatalf("status = %d, want 409 refinement_filing_repo_mismatch pinning acme/a (body=%s)", rec.Code, rec.Body.String())
	}
	if _, err := repo.GetFilingSession(context.Background(), draftID); !errors.Is(err, refinement.ErrNotFound) {
		t.Errorf("GetFilingSession err = %v, want ErrNotFound (no filing launched)", err)
	}
}

func TestRefinementFile_ConventionsUnavailableIs502(t *testing.T) {
	installRefinementLoader(t, failingLoaderCases(t)["fetch_error"])
	repo := refinement.NewPostgresRepository(pgtest.NewPool(t))
	sessionID, draftID := approvedRepoScopedSession(t, repo, "acme/widgets")
	s := New(Config{RefinementRepo: repo, AuditRepo: okAuditRepo{}})
	t.Cleanup(func() { waitFilingBounded(t, s) })

	rec := postFile(s, sessionID, "acme/widgets")
	assertConventionsUnavailable(t, rec, "acme/widgets")
	if _, err := repo.GetFilingSession(context.Background(), draftID); !errors.Is(err, refinement.ErrNotFound) {
		t.Errorf("GetFilingSession err = %v, want ErrNotFound (nothing filed)", err)
	}
}

// TestRefinementFile_DegradedReasonInResponse: a degraded filing names its
// reason in the file-arm response's conventions_source.
func TestRefinementFile_DegradedReasonInResponse(t *testing.T) {
	installRefinementLoader(t, RepoConventionsLoaderConfig{Resolver: &fakeProviderResolver{found: false}})
	pool := pgtest.NewPool(t)
	repo := refinement.NewPostgresRepository(pool)
	sessionID, _ := approvedRepoScopedSession(t, repo, "acme/widgets")
	s := New(Config{RefinementRepo: repo, AuditRepo: audit.NewPostgresRepository(pool)})
	t.Cleanup(func() { waitFilingBounded(t, s) })

	rec := postFile(s, sessionID, "acme/widgets")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body=%s)", rec.Code, rec.Body.String())
	}
	want := refinementConventionsSourceView{Repo: "acme/widgets", State: ConventionsStateDegraded,
		DegradedReason: ConventionsDegradedNoProviderAccount, Fallback: ConventionsFallbackShippedDefault}
	if got := decodeFileResp(t, rec).ConventionsSource; got != want {
		t.Errorf("file conventions_source = %+v, want %+v", got, want)
	}
}

// TestRefinementResolveConventions_StubNamesLoaderUnconfigured: with no
// resolver installed, a repo-scoped session reports the seam stub's named
// degrade rather than a silent default.
func TestRefinementResolveConventions_StubNamesLoaderUnconfigured(t *testing.T) {
	s := New(Config{})
	res, err := s.resolveRefinementConventions(context.Background(), "acme/widgets")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if res.State != ConventionsStateDegraded || res.DegradedReason != ConventionsDegradedLoaderUnconfigured {
		t.Errorf("resolution = %+v, want degraded / %s", res, ConventionsDegradedLoaderUnconfigured)
	}
	if got := fmt.Sprint(conventionsSourceView(res).Repo); got != "acme/widgets" {
		t.Errorf("view repo = %q, want acme/widgets", got)
	}
	// The stub cannot know where its delegate's conventions came from: it
	// names no Fallback kind, and the session view carries none either.
	if res.Fallback != "" {
		t.Errorf("resolution Fallback = %q, want empty", res.Fallback)
	}
	if got := conventionsSourceView(res).Fallback; got != "" {
		t.Errorf("view Fallback = %q, want empty", got)
	}
}

// TestRefinementDrafter_ReceivesResolvedConventions isolates the wiring of the
// RESOLVED conventions into the drafter on both agent-backed arms: the
// installed resolution's feature type requires a "component" namespace that
// neither workmgmt.Default() nor this repository's declaration has, so a
// handler drafting with any other conventions value is caught.
func TestRefinementDrafter_ReceivesResolvedConventions(t *testing.T) {
	custom := workmgmt.Default()
	types := make(map[string]workmgmt.ItemType, len(custom.Types))
	for k, v := range custom.Types {
		types[k] = v
	}
	feature := types["feature"]
	feature.RequiredLabelNamespaces = []string{"component"}
	types["feature"] = feature
	custom.Types = types
	prev := conventionsResolver
	SetConventionsResolver(func(_ context.Context, repo string) (ConventionsResolution, error) {
		return ConventionsResolution{Conventions: custom, Repo: repo, State: ConventionsStateDeclared}, nil
	})
	t.Cleanup(func() { conventionsResolver = prev })

	repo := &disconnectRefinementRepo{}
	drafter := &fakeRefinementDrafter{draft: refinementValidDraft()}
	s := New(Config{RefinementRepo: repo, AuditRepo: okAuditRepo{}, RefinementDrafter: drafter})
	wantNS := func(arm string) {
		t.Helper()
		if got := drafter.sawConv.Types["feature"].RequiredLabelNamespaces; len(got) != 1 || got[0] != "component" {
			t.Errorf("%s: drafter feature namespaces = %v, want the resolved [component]", arm, got)
		}
	}

	rec := httptest.NewRecorder()
	s.handleCreateRefinementSession(rec, openReq(t, "b", "acme/widgets"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("open status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}
	wantNS("open")
	// The preview renders with the SAME resolved conventions: each child
	// (labels area + autonomy) is missing only the resolved "component".
	for _, item := range decodeSessionView(t, rec).Preview[1:] {
		if got := item.Classification.MissingLabelNamespaces; len(got) != 1 || got[0] != "component" {
			t.Errorf("preview child %q missing namespaces = %v, want [component] (rendered with the resolved conventions)", item.Title, got)
		}
	}

	drafter.sawConv = workmgmt.Conventions{}
	sessionID := repo.drafts[0].SessionID
	rec = httptest.NewRecorder()
	s.handlePatchRefinementDraft(rec, refinementReq(http.MethodPatch,
		"/v0/refinement/sessions/"+sessionID.String()+"/draft", sessionID.String(), `{"brief_amendment":"more"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	wantNS("amendment")
}
