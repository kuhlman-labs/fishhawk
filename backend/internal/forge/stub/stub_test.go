package stub_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	forgegithub "github.com/kuhlman-labs/fishhawk/backend/internal/forge/github"
	forgegitlab "github.com/kuhlman-labs/fishhawk/backend/internal/forge/gitlab"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge/stub"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubapp"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/webhook"
)

// Every behavioural test here drives the stub ONLY through the REAL
// adapters — forgegithub over a genuine *githubclient.Client and
// forgegitlab over a genuine gitlabclient factory, both reaching the stub
// through the in-process transport — so the served subset is proven to be
// what the product calls, not what a hand-rolled fixture happens to send.
// Assertions read COMMITTED STATE on the stub after the call returns.
// The one raw-HTTP test (TestStubGitLab_PutIgnoresBareState) exists
// because no adapter produces the malformed PUT it pins.

const (
	ghRepo   = "stub/parent-close"
	glPath   = "stub/parent-close"
	glProjID = 7
)

var (
	ghScope = forge.FromGitHubInstallationID(4242)
	glScope = forge.FromRef("gitlab:7")
	repoRef = forge.RepoRef{Owner: "stub", Name: "parent-close"}
)

func githubForge(f *stub.Forge) *forgegithub.Forge {
	return forgegithub.New(&githubclient.Client{
		BaseURL: stub.GitHubBaseURL,
		Tokens:  stub.StaticTokens{Value: stub.InstallationToken},
		HTTP:    f.HTTPClient(),
	})
}

func gitlabForge(f *stub.Forge) *forgegitlab.Forge {
	return forgegitlab.New(stub.GitLabBaseURL,
		forgegitlab.NewStaticCredentialProvider(stub.InstallationToken),
		forgegitlab.WithHTTPClient(f.HTTPClient()))
}

func seedGitHubIssue(t *testing.T, f *stub.Forge, number int, state string, comments ...string) {
	t.Helper()
	if _, err := f.SeedIssue(stub.Issue{Forge: stub.ForgeGitHub, Repo: ghRepo, Number: number, Title: "parent", State: state, Comments: comments}); err != nil {
		t.Fatalf("seed github issue: %v", err)
	}
}

func seedGitLabIssue(t *testing.T, f *stub.Forge, iid int, state string, notes ...string) {
	t.Helper()
	if _, err := f.SeedIssue(stub.Issue{Forge: stub.ForgeGitLab, Repo: glPath, ProjectID: glProjID, Number: iid, Title: "parent", State: state, Comments: notes}); err != nil {
		t.Fatalf("seed gitlab issue: %v", err)
	}
}

func strp(s string) *string { return &s }

// --- GitHub through forgegithub -------------------------------------------

func TestStubGitHub_IssueRoundTripThroughRealAdapter(t *testing.T) {
	f := stub.New()
	seedGitHubIssue(t, f, 100, "open", "first")
	gh := githubForge(f)
	ctx := context.Background()

	is, err := gh.FetchIssue(ctx, ghScope, repoRef, 100)
	if err != nil {
		t.Fatalf("FetchIssue: %v", err)
	}
	if is.Number != 100 || is.State != "open" || is.StateReason != "" || is.Title != "parent" {
		t.Fatalf("FetchIssue = %+v, want open #100 titled parent", *is)
	}

	if err := gh.PostIssueComment(ctx, ghScope, repoRef, 100, "second"); err != nil {
		t.Fatalf("PostIssueComment: %v", err)
	}
	comments, err := gh.FetchIssueComments(ctx, ghScope, repoRef, 100)
	if err != nil {
		t.Fatalf("FetchIssueComments: %v", err)
	}
	var bodies []string
	for _, c := range comments {
		bodies = append(bodies, c.Body)
	}
	if !reflect.DeepEqual(bodies, []string{"first", "second"}) {
		t.Fatalf("comment bodies = %v, want [first second] in arrival order", bodies)
	}

	if err := gh.SetIssueState(ctx, ghScope, repoRef, 100, forge.IssueStateUpdate{State: strp("closed"), StateReason: strp("completed")}); err != nil {
		t.Fatalf("SetIssueState: %v", err)
	}
	got, ok := f.GetIssue(stub.ForgeGitHub, ghRepo, 0, 100)
	if !ok {
		t.Fatal("issue vanished")
	}
	if got.State != "closed" || got.StateReason != "completed" {
		t.Fatalf("committed state = %q/%q, want closed/completed", got.State, got.StateReason)
	}
	if len(got.Comments) != 2 {
		t.Fatalf("committed comments = %v, want 2", got.Comments)
	}

	// Request ordering: the comment landed BEFORE the close.
	reqs := f.Requests()
	post, patch := indexOf(reqs, stub.OpGitHubPostComment+" github:"+ghRepo+"#100"), indexOf(reqs, stub.OpGitHubPatchIssue+" github:"+ghRepo+"#100")
	if post < 0 || patch < 0 || post > patch {
		t.Fatalf("request log %v: want post_comment before patch_issue", reqs)
	}
}

func TestStubGitHub_PullRequestMergedFields(t *testing.T) {
	f := stub.New()
	mergedAt := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	if _, err := f.SeedPullRequest(stub.PullRequest{
		Forge: stub.ForgeGitHub, Repo: ghRepo, Number: 41, State: "closed", Merged: true,
		MergeCommitSHA: "abc123", MergedAt: &mergedAt, HeadSHA: "head456", HeadRef: "feature", BaseRef: "main",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	pr, err := githubForge(f).GetPullRequest(context.Background(), ghScope, repoRef, 41)
	if err != nil {
		t.Fatalf("GetPullRequest: %v", err)
	}
	if !pr.Merged || pr.MergeCommitSHA != "abc123" || pr.HeadSHA != "head456" || pr.State != "closed" || pr.HeadRef != "feature" || pr.BaseRef != "main" {
		t.Fatalf("GetPullRequest = %+v", *pr)
	}
	if pr.MergedAt == nil || !pr.MergedAt.Equal(mergedAt) {
		t.Fatalf("MergedAt = %v, want %v", pr.MergedAt, mergedAt)
	}

	// An unmerged PR reports merged_at as JSON null, never the zero time.
	if _, err := f.SeedPullRequest(stub.PullRequest{Forge: stub.ForgeGitHub, Repo: ghRepo, Number: 42, HeadSHA: "h"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	open, err := githubForge(f).GetPullRequest(context.Background(), ghScope, repoRef, 42)
	if err != nil {
		t.Fatalf("GetPullRequest open: %v", err)
	}
	if open.Merged || open.MergedAt != nil || open.State != "open" {
		t.Fatalf("open PR = %+v, want unmerged/open with nil MergedAt", *open)
	}
}

func TestStubGitHub_RepositoryDefaultBranch(t *testing.T) {
	f := stub.New()
	repo, err := githubForge(f).GetRepository(context.Background(), ghScope, repoRef)
	if err != nil {
		t.Fatalf("GetRepository: %v", err)
	}
	if repo.DefaultBranch != "main" {
		t.Fatalf("DefaultBranch = %q, want main", repo.DefaultBranch)
	}
}

// TestStubGitHub_AccessTokensMintsThroughGitHubAppClient proves a
// signer-backed githubapp client (the App-JWT path serve.go builds when an
// App is configured) can mint against the stub: the endpoint answers 201
// with the fixed InstallationToken and a future expires_at.
func TestStubGitHub_AccessTokensMintsThroughGitHubAppClient(t *testing.T) {
	f := stub.New()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	signer, err := githubapp.NewSignerFromPEM(12345, pemBytes)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	c := githubapp.NewClient(signer)
	c.BaseURL = stub.GitHubBaseURL
	c.HTTP = f.HTTPClient()

	tok, err := c.IssueInstallationToken(context.Background(), 4242)
	if err != nil {
		t.Fatalf("IssueInstallationToken: %v", err)
	}
	if tok.Token != stub.InstallationToken {
		t.Fatalf("token = %q, want %q", tok.Token, stub.InstallationToken)
	}
	if !tok.ExpiresAt.After(time.Now()) {
		t.Fatalf("expires_at %v is not in the future", tok.ExpiresAt)
	}
	if got := f.Requests(); len(got) != 1 || !strings.HasPrefix(got[0], stub.OpGitHubAccessToken+" ") {
		t.Fatalf("request log = %v, want one %s entry", got, stub.OpGitHubAccessToken)
	}
}

// --- GitLab through forgegitlab -------------------------------------------

func TestStubGitLab_IssueRoundTripNormalizesNativeState(t *testing.T) {
	f := stub.New()
	seedGitLabIssue(t, f, 100, "opened", "first")
	gl := gitlabForge(f)
	ctx := context.Background()

	is, err := gl.FetchIssue(ctx, glScope, repoRef, 100)
	if err != nil {
		t.Fatalf("FetchIssue: %v", err)
	}
	// The stub holds "opened"; the adapter normalizes onto "open".
	if is.State != "open" || is.Number != 100 {
		t.Fatalf("FetchIssue = %+v, want normalized open #100", *is)
	}

	if err := gl.PostIssueComment(ctx, glScope, repoRef, 100, "second"); err != nil {
		t.Fatalf("PostIssueComment: %v", err)
	}
	if err := gl.SetIssueState(ctx, glScope, repoRef, 100, forge.IssueStateUpdate{State: strp("closed")}); err != nil {
		t.Fatalf("SetIssueState closed: %v", err)
	}
	got, _ := f.GetIssue(stub.ForgeGitLab, "", glProjID, 100)
	if got.State != "closed" {
		t.Fatalf("committed native state = %q, want closed", got.State)
	}
	if !reflect.DeepEqual(got.Comments, []string{"first", "second"}) {
		t.Fatalf("committed notes = %v", got.Comments)
	}
	closed, err := gl.FetchIssue(ctx, glScope, repoRef, 100)
	if err != nil || closed.State != "closed" {
		t.Fatalf("FetchIssue after close = %+v, %v", closed, err)
	}

	if err := gl.SetIssueState(ctx, glScope, repoRef, 100, forge.IssueStateUpdate{State: strp("open")}); err != nil {
		t.Fatalf("SetIssueState open: %v", err)
	}
	got, _ = f.GetIssue(stub.ForgeGitLab, "", glProjID, 100)
	if got.State != "opened" {
		t.Fatalf("committed native state after reopen = %q, want opened", got.State)
	}

	reqs := f.Requests()
	post, put := indexOf(reqs, stub.OpGitLabPostNote+" gitlab:7#100"), indexOf(reqs, stub.OpGitLabPutIssue+" gitlab:7#100")
	if post < 0 || put < 0 || post > put {
		t.Fatalf("request log %v: want post_note before put_issue", reqs)
	}
}

// TestStubGitLab_PutIgnoresBareState pins that a PUT carrying `state`
// without `state_event` leaves the state UNCHANGED, as the real API does.
// No adapter produces this body, so it is sent raw through the stub's
// own client. Counterfactual: writing `state` verbatim in the PUT branch
// turns this RED.
func TestStubGitLab_PutIgnoresBareState(t *testing.T) {
	f := stub.New()
	seedGitLabIssue(t, f, 100, "opened")
	body, _ := json.Marshal(map[string]any{"state": "closed"})
	req, _ := http.NewRequest(http.MethodPut, stub.GitLabBaseURL+"/api/v4/projects/7/issues/100", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := f.HTTPClient().Do(req)
	if err != nil {
		t.Fatalf("PUT: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200", resp.StatusCode)
	}
	got, _ := f.GetIssue(stub.ForgeGitLab, "", glProjID, 100)
	if got.State != "opened" {
		t.Fatalf("bare `state` changed native state to %q; the real API ignores it", got.State)
	}

	// And an unknown state_event is refused with 400 without changing state.
	body, _ = json.Marshal(map[string]any{"state_event": "archive"})
	req, _ = http.NewRequest(http.MethodPut, stub.GitLabBaseURL+"/api/v4/projects/7/issues/100", bytes.NewReader(body))
	resp, err = f.HTTPClient().Do(req)
	if err != nil {
		t.Fatalf("PUT: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown state_event status = %d, want 400", resp.StatusCode)
	}
	if got, _ := f.GetIssue(stub.ForgeGitLab, "", glProjID, 100); got.State != "opened" {
		t.Fatalf("unknown state_event changed state to %q", got.State)
	}
}

// TestStubGitLab_NotesPaginateToExhaustion forces two-note pages over a
// five-note thread and asserts the REAL adapter walks every page via the
// rel="next" Link. Counterfactual: dropping the Link emission leaves only
// page one visible and turns this RED.
func TestStubGitLab_NotesPaginateToExhaustion(t *testing.T) {
	f := stub.New()
	seedGitLabIssue(t, f, 100, "opened", "n1", "n2", "n3", "n4", "n5")
	f.SetNotesPageSize(2)

	comments, err := gitlabForge(f).FetchIssueComments(context.Background(), glScope, repoRef, 100)
	if err != nil {
		t.Fatalf("FetchIssueComments: %v", err)
	}
	var bodies []string
	for _, c := range comments {
		bodies = append(bodies, c.Body)
	}
	if want := []string{"n1", "n2", "n3", "n4", "n5"}; !reflect.DeepEqual(bodies, want) {
		t.Fatalf("paged notes = %v, want %v", bodies, want)
	}
	if n := countPrefix(f.Requests(), stub.OpGitLabListNotes+" "); n != 3 {
		t.Fatalf("list_notes requests = %d, want 3 (pages of 2 over 5 notes)", n)
	}
}

func TestStubGitLab_MergeRequestMergedFields(t *testing.T) {
	f := stub.New()
	mergedAt := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	if _, err := f.SeedPullRequest(stub.PullRequest{
		Forge: stub.ForgeGitLab, ProjectID: glProjID, Number: 9, Merged: true,
		MergeCommitSHA: "mc1", MergedAt: &mergedAt, HeadSHA: "sha9", HeadRef: "feature", BaseRef: "main",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	pr, err := gitlabForge(f).GetPullRequest(context.Background(), glScope, repoRef, 9)
	if err != nil {
		t.Fatalf("GetPullRequest: %v", err)
	}
	if !pr.Merged || pr.State != "closed" || pr.MergeCommitSHA != "mc1" || pr.HeadSHA != "sha9" || pr.Number != 9 {
		t.Fatalf("GetPullRequest = %+v", *pr)
	}
	if pr.MergedAt == nil || !pr.MergedAt.Equal(mergedAt) {
		t.Fatalf("MergedAt = %v, want %v", pr.MergedAt, mergedAt)
	}
}

func TestStubGitLab_ProjectPathLookup(t *testing.T) {
	f := stub.New()
	seedGitLabIssue(t, f, 1, "opened")
	gl := gitlabForge(f)

	scope, err := gl.ResolveRepoScope(context.Background(), repoRef)
	if err != nil {
		t.Fatalf("ResolveRepoScope: %v", err)
	}
	if scope.Ref() != "gitlab:7" {
		t.Fatalf("scope ref = %q, want gitlab:7", scope.Ref())
	}
	_, err = gl.ResolveRepoScope(context.Background(), forge.RepoRef{Owner: "nobody", Name: "here"})
	if !errors.Is(err, forge.ErrNotInstalled) {
		t.Fatalf("unknown path err = %v, want ErrNotInstalled", err)
	}
}

// --- cross-family ---------------------------------------------------------

// TestStub_UnknownPathIs404 pins that a path the stub does not serve — the
// contents path the conventions loader fetches on the same issues.closed
// delivery — answers a JSON 404 that both adapters map to forge.ErrNotFound,
// never a panic or a 5xx.
func TestStub_UnknownPathIs404(t *testing.T) {
	f := stub.New()
	ctx := context.Background()
	if _, err := githubForge(f).FetchFile(ctx, ghScope, repoRef, ".fishhawk/conventions.yaml", "main"); !errors.Is(err, forge.ErrNotFound) {
		t.Fatalf("github contents path err = %v, want ErrNotFound", err)
	}
	if _, err := gitlabForge(f).FetchFile(ctx, glScope, repoRef, ".fishhawk/conventions.yaml", "main"); !errors.Is(err, forge.ErrNotFound) {
		t.Fatalf("gitlab files path err = %v, want ErrNotFound", err)
	}
	if _, err := githubForge(f).FetchIssue(ctx, ghScope, repoRef, 999); !errors.Is(err, forge.ErrNotFound) {
		t.Fatalf("unseeded github issue err = %v, want ErrNotFound", err)
	}
	if _, err := gitlabForge(f).FetchIssue(ctx, glScope, repoRef, 999); !errors.Is(err, forge.ErrNotFound) {
		t.Fatalf("unseeded gitlab issue err = %v, want ErrNotFound", err)
	}
	if _, err := githubForge(f).GetPullRequest(ctx, ghScope, repoRef, 999); !errors.Is(err, forge.ErrNotFound) {
		t.Fatalf("unseeded github pull err = %v, want ErrNotFound", err)
	}
	if _, err := gitlabForge(f).GetPullRequest(ctx, glScope, repoRef, 999); !errors.Is(err, forge.ErrNotFound) {
		t.Fatalf("unseeded gitlab mr err = %v, want ErrNotFound", err)
	}
}

func TestStub_HandlerRoutesByHost(t *testing.T) {
	f := stub.New()
	req, _ := http.NewRequest(http.MethodGet, "http://elsewhere.invalid/repos/o/r", nil)
	resp, err := f.HTTPClient().Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown host status = %d, want 404", resp.StatusCode)
	}
	if got := f.Requests(); len(got) != 0 {
		t.Fatalf("unknown host recorded requests %v", got)
	}
}

// TestStub_FaultsSurfaceThenClear sets each operation's fault, drives the
// operation through the real adapter, asserts a NON-not-found error
// surfaces, clears the fault, and asserts the same call succeeds — so a
// fault is proven transient, never terminal.
func TestStub_FaultsSurfaceThenClear(t *testing.T) {
	f := stub.New()
	seedGitHubIssue(t, f, 100, "open", "c")
	seedGitLabIssue(t, f, 100, "opened", "n")
	if _, err := f.SeedPullRequest(stub.PullRequest{Forge: stub.ForgeGitHub, Repo: ghRepo, Number: 5, HeadSHA: "h"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.SeedPullRequest(stub.PullRequest{Forge: stub.ForgeGitLab, ProjectID: glProjID, Number: 5, HeadSHA: "h"}); err != nil {
		t.Fatal(err)
	}
	gh, gl := githubForge(f), gitlabForge(f)
	ctx := context.Background()

	cases := []struct {
		op   string
		call func() error
	}{
		{stub.OpGitHubGetIssue, func() error { _, err := gh.FetchIssue(ctx, ghScope, repoRef, 100); return err }},
		{stub.OpGitHubListComments, func() error { _, err := gh.FetchIssueComments(ctx, ghScope, repoRef, 100); return err }},
		{stub.OpGitHubPostComment, func() error { return gh.PostIssueComment(ctx, ghScope, repoRef, 100, "x") }},
		{stub.OpGitHubPatchIssue, func() error {
			return gh.SetIssueState(ctx, ghScope, repoRef, 100, forge.IssueStateUpdate{State: strp("open")})
		}},
		{stub.OpGitHubGetPull, func() error { _, err := gh.GetPullRequest(ctx, ghScope, repoRef, 5); return err }},
		{stub.OpGitHubGetRepo, func() error { _, err := gh.GetRepository(ctx, ghScope, repoRef); return err }},
		{stub.OpGitLabGetProject, func() error { _, err := gl.ResolveRepoScope(ctx, repoRef); return err }},
		{stub.OpGitLabGetIssue, func() error { _, err := gl.FetchIssue(ctx, glScope, repoRef, 100); return err }},
		{stub.OpGitLabListNotes, func() error { _, err := gl.FetchIssueComments(ctx, glScope, repoRef, 100); return err }},
		{stub.OpGitLabPostNote, func() error { return gl.PostIssueComment(ctx, glScope, repoRef, 100, "x") }},
		{stub.OpGitLabPutIssue, func() error {
			return gl.SetIssueState(ctx, glScope, repoRef, 100, forge.IssueStateUpdate{State: strp("open")})
		}},
		{stub.OpGitLabGetMergeRequest, func() error { _, err := gl.GetPullRequest(ctx, glScope, repoRef, 5); return err }},
	}
	for _, tc := range cases {
		t.Run(tc.op, func(t *testing.T) {
			f.SetFault(tc.op, true)
			err := tc.call()
			if err == nil {
				t.Fatalf("%s: fault set but call succeeded", tc.op)
			}
			if errors.Is(err, forge.ErrNotFound) {
				t.Fatalf("%s: fault surfaced as ErrNotFound (%v); want a 500-class error", tc.op, err)
			}
			f.SetFault(tc.op, false)
			if err := tc.call(); err != nil {
				t.Fatalf("%s: fault cleared but call failed: %v", tc.op, err)
			}
		})
	}
}

func TestStub_ResetEmptiesState(t *testing.T) {
	f := stub.New()
	seedGitHubIssue(t, f, 100, "open")
	seedGitLabIssue(t, f, 100, "opened")
	if _, err := f.SeedPullRequest(stub.PullRequest{Forge: stub.ForgeGitHub, Repo: ghRepo, Number: 1}); err != nil {
		t.Fatal(err)
	}
	f.SetFault(stub.OpGitHubGetIssue, true)
	f.SetNotesPageSize(1)
	if _, err := githubForge(f).FetchIssue(context.Background(), ghScope, repoRef, 100); err == nil {
		t.Fatal("fault not active before reset")
	}

	f.Reset()

	snap := f.Snapshot()
	if len(snap.GitHub.Issues) != 0 || len(snap.GitLab.Issues) != 0 || len(snap.GitHub.Pulls) != 0 || len(snap.Requests) != 0 {
		t.Fatalf("snapshot after reset = %+v, want empty", snap)
	}
	if _, ok := f.GetIssue(stub.ForgeGitHub, ghRepo, 0, 100); ok {
		t.Fatal("github issue survived reset")
	}
	if _, err := gitlabForge(f).ResolveRepoScope(context.Background(), repoRef); !errors.Is(err, forge.ErrNotInstalled) {
		t.Fatalf("project path survived reset: %v", err)
	}
	// Faults are cleared too: re-seed and the previously faulted op succeeds.
	seedGitHubIssue(t, f, 100, "open")
	if _, err := githubForge(f).FetchIssue(context.Background(), ghScope, repoRef, 100); err != nil {
		t.Fatalf("fault survived reset: %v", err)
	}
}

func TestStub_SnapshotShape(t *testing.T) {
	f := stub.New()
	seedGitHubIssue(t, f, 100, "open", "c1")
	seedGitLabIssue(t, f, 100, "opened")
	if _, err := f.SeedPullRequest(stub.PullRequest{Forge: stub.ForgeGitLab, ProjectID: glProjID, Number: 3, Merged: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := githubForge(f).FetchIssue(context.Background(), ghScope, repoRef, 100); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(f.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	gh := got["github"].(map[string]any)
	gl := got["gitlab"].(map[string]any)
	if len(gh["issues"].([]any)) != 1 || len(gl["issues"].([]any)) != 1 {
		t.Fatalf("snapshot issues: %s", raw)
	}
	if _, ok := gh["pulls"]; ok {
		t.Fatalf("github.pulls present with no github pulls seeded: %s", raw)
	}
	mrs := gl["merge_requests"].([]any)
	if len(mrs) != 1 || mrs[0].(map[string]any)["state"] != "merged" {
		t.Fatalf("gitlab.merge_requests: %s", raw)
	}
	reqs := got["requests"].([]any)
	if len(reqs) != 1 || reqs[0] != stub.OpGitHubGetIssue+" github:"+ghRepo+"#100" {
		t.Fatalf("requests: %v", reqs)
	}
	issue := gh["issues"].([]any)[0].(map[string]any)
	for _, k := range []string{"forge", "repo", "number", "title", "body", "state", "comments"} {
		if _, ok := issue[k]; !ok {
			t.Fatalf("github issue JSON missing %q: %s", k, raw)
		}
	}
}

func TestStub_SeedValidation(t *testing.T) {
	f := stub.New()
	cases := []struct {
		name string
		is   stub.Issue
	}{
		{"unknown forge", stub.Issue{Forge: "bitbucket", Repo: "o/r", Number: 1}},
		{"non-positive number", stub.Issue{Forge: stub.ForgeGitHub, Repo: "o/r", Number: 0}},
		{"github without repo", stub.Issue{Forge: stub.ForgeGitHub, Number: 1}},
		{"gitlab without project_id", stub.Issue{Forge: stub.ForgeGitLab, Repo: "o/r", Number: 1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := f.SeedIssue(tc.is); !errors.Is(err, stub.ErrInvalid) {
				t.Fatalf("SeedIssue(%+v) err = %v, want ErrInvalid", tc.is, err)
			}
			pr := stub.PullRequest{Forge: tc.is.Forge, Repo: tc.is.Repo, ProjectID: tc.is.ProjectID, Number: tc.is.Number}
			if _, err := f.SeedPullRequest(pr); !errors.Is(err, stub.ErrInvalid) {
				t.Fatalf("SeedPullRequest(%+v) err = %v, want ErrInvalid", pr, err)
			}
		})
	}
	if snap := f.Snapshot(); len(snap.GitHub.Issues)+len(snap.GitLab.Issues) != 0 {
		t.Fatalf("invalid seeds were stored: %+v", snap)
	}

	// Defaults: empty state becomes the family's native open word.
	gh, err := f.SeedIssue(stub.Issue{Forge: stub.ForgeGitHub, Repo: "o/r", Number: 1})
	if err != nil || gh.State != "open" {
		t.Fatalf("github default state = %q, %v", gh.State, err)
	}
	gl, err := f.SeedIssue(stub.Issue{Forge: stub.ForgeGitLab, ProjectID: 3, Number: 1})
	if err != nil || gl.State != "opened" {
		t.Fatalf("gitlab default state = %q, %v", gl.State, err)
	}
}

// --- transport / tokens / signing -----------------------------------------

func TestStaticTokens_ReturnsValue(t *testing.T) {
	tokens := stub.StaticTokens{Value: "tok"}
	got, err := tokens.Token(context.Background(), 0)
	if err != nil || got != "tok" {
		t.Fatalf("Token = %q, %v; want tok, nil", got, err)
	}
	var provider githubapp.TokenProvider = tokens
	if got, _ := provider.Token(context.Background(), 99); got != "tok" {
		t.Fatalf("through the interface = %q", got)
	}
}

func TestSignGitHubDelivery_VerifiesWithReceiver(t *testing.T) {
	secret := []byte(stub.GitHubWebhookSecret)
	body := []byte(`{"action":"closed"}`)
	sig := stub.SignGitHubDelivery(secret, body)
	if !strings.HasPrefix(sig, "sha256=") {
		t.Fatalf("signature %q lacks sha256= prefix", sig)
	}
	if err := webhook.VerifySignature(secret, body, sig); err != nil {
		t.Fatalf("receiver rejected our signature: %v", err)
	}
	if err := webhook.VerifySignature([]byte("other"), body, sig); err == nil {
		t.Fatal("signature verified under the wrong secret")
	}
}

func TestTransport_HonoursCancelledContext(t *testing.T) {
	f := stub.New()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, stub.GitHubBaseURL+"/repos/o/r", nil)
	if _, err := stub.Transport(f.Handler()).RoundTrip(req); !errors.Is(err, context.Canceled) {
		t.Fatalf("RoundTrip err = %v, want context.Canceled", err)
	}
	if got := f.Requests(); len(got) != 0 {
		t.Fatalf("cancelled request reached the stub: %v", got)
	}
}

func TestTransport_ImplicitStatusIs200(t *testing.T) {
	rt := stub.Transport(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, "ok")
	}))
	req, _ := http.NewRequest(http.MethodGet, "http://any.invalid/", nil)
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK || resp.ContentLength != 2 {
		t.Fatalf("resp = %d/%d, want 200/2", resp.StatusCode, resp.ContentLength)
	}
}

func TestStubGitLab_RepositoryByProjectID(t *testing.T) {
	f := stub.New()
	seedGitLabIssue(t, f, 1, "opened") // registers glPath -> 7
	repo, err := gitlabForge(f).GetRepository(context.Background(), glScope, repoRef)
	if err != nil {
		t.Fatalf("GetRepository: %v", err)
	}
	if repo.DefaultBranch != "main" {
		t.Fatalf("DefaultBranch = %q, want main", repo.DefaultBranch)
	}
	// A project id with no registered path is not found.
	if _, err := gitlabForge(f).GetRepository(context.Background(), forge.FromRef("gitlab:8"), repoRef); !errors.Is(err, forge.ErrNotFound) {
		t.Fatalf("unregistered project id err = %v, want ErrNotFound", err)
	}
}

func TestStub_GetPullRequestByAddress(t *testing.T) {
	f := stub.New()
	if _, err := f.SeedPullRequest(stub.PullRequest{Forge: stub.ForgeGitHub, Repo: ghRepo, Number: 4, HeadSHA: "h4"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.SeedPullRequest(stub.PullRequest{Forge: stub.ForgeGitLab, Repo: glPath, ProjectID: glProjID, Number: 4, HeadSHA: "g4"}); err != nil {
		t.Fatal(err)
	}
	gh, ok := f.GetPullRequest(stub.ForgeGitHub, ghRepo, 0, 4)
	if !ok || gh.HeadSHA != "h4" || gh.State != "open" || gh.ProjectID != 0 {
		t.Fatalf("github pull = %+v, %v", gh, ok)
	}
	gl, ok := f.GetPullRequest(stub.ForgeGitLab, "", glProjID, 4)
	if !ok || gl.HeadSHA != "g4" || gl.State != "opened" {
		t.Fatalf("gitlab mr = %+v, %v", gl, ok)
	}
	if _, ok := f.GetPullRequest(stub.ForgeGitHub, ghRepo, 0, 5); ok {
		t.Fatal("unseeded pull found")
	}
	// Seeding a GitLab MR under a path registers it for lookups too.
	if scope, err := gitlabForge(f).ResolveRepoScope(context.Background(), repoRef); err != nil || scope.Ref() != "gitlab:7" {
		t.Fatalf("path registered by SeedPullRequest: %v, %v", scope.Ref(), err)
	}
}

// TestStub_RawEdgeResponses pins every defensive branch of the two
// handlers that no adapter reaches: unknown/incomplete paths and wrong
// methods answer 404, an undecodable body answers 4xx, and a faulted
// access_tokens mint answers 500 — never a panic. Each row also asserts
// the stub's state is untouched afterwards.
func TestStub_RawEdgeResponses(t *testing.T) {
	f := stub.New()
	seedGitHubIssue(t, f, 100, "open")
	seedGitLabIssue(t, f, 100, "opened")
	client := f.HTTPClient()
	gh, gl := stub.GitHubBaseURL, stub.GitLabBaseURL

	rows := []struct {
		name, method, url, body string
		fault                   string
		want                    int
	}{
		{"github root", http.MethodGet, gh + "/", "", "", 404},
		{"github unknown top-level", http.MethodGet, gh + "/user", "", "", 404},
		{"github access_tokens wrong method", http.MethodGet, gh + "/app/installations/1/access_tokens", "", "", 404},
		{"github access_tokens faulted", http.MethodPost, gh + "/app/installations/1/access_tokens", "", stub.OpGitHubAccessToken, 500},
		{"github repo wrong method", http.MethodDelete, gh + "/repos/stub/parent-close", "", "", 404},
		{"github repo short path", http.MethodGet, gh + "/repos/stub/parent-close/issues", "", "", 404},
		{"github non-numeric number", http.MethodGet, gh + "/repos/stub/parent-close/issues/abc", "", "", 404},
		{"github unknown sub-resource", http.MethodGet, gh + "/repos/stub/parent-close/issues/100/labels", "", "", 404},
		{"github contents path", http.MethodGet, gh + "/repos/stub/parent-close/contents/.fishhawk/workflows.yaml", "", "", 404},
		{"github pulls wrong method", http.MethodPatch, gh + "/repos/stub/parent-close/pulls/1", "{}", "", 404},
		{"github patch undecodable", http.MethodPatch, gh + "/repos/stub/parent-close/issues/100", "{not json", "", 422},
		{"github patch unseeded", http.MethodPatch, gh + "/repos/stub/parent-close/issues/999", `{"state":"closed"}`, "", 404},
		{"github comment undecodable", http.MethodPost, gh + "/repos/stub/parent-close/issues/100/comments", "{not json", "", 422},
		{"github comment unseeded", http.MethodPost, gh + "/repos/stub/parent-close/issues/999/comments", `{"body":"x"}`, "", 404},
		{"github list comments unseeded", http.MethodGet, gh + "/repos/stub/parent-close/issues/999/comments", "", "", 404},
		{"gitlab root", http.MethodGet, gl + "/", "", "", 404},
		{"gitlab non-v4", http.MethodGet, gl + "/api/v3/projects/7", "", "", 404},
		{"gitlab path lookup wrong method", http.MethodPost, gl + "/api/v4/projects/stub%2Fparent-close", "", "", 404},
		{"gitlab path lookup unknown", http.MethodGet, gl + "/api/v4/projects/nobody%2Fhere", "", "", 404},
		{"gitlab project by id wrong method", http.MethodPut, gl + "/api/v4/projects/7", "{}", "", 404},
		{"gitlab short path", http.MethodGet, gl + "/api/v4/projects/7/issues", "", "", 404},
		{"gitlab non-numeric iid", http.MethodGet, gl + "/api/v4/projects/7/issues/abc", "", "", 404},
		{"gitlab unknown sub-resource", http.MethodGet, gl + "/api/v4/projects/7/issues/100/links", "", "", 404},
		{"gitlab files path", http.MethodGet, gl + "/api/v4/projects/7/repository/files/x/raw", "", "", 404},
		{"gitlab put undecodable", http.MethodPut, gl + "/api/v4/projects/7/issues/100", "{not json", "", 400},
		{"gitlab put unseeded", http.MethodPut, gl + "/api/v4/projects/7/issues/999", `{"state_event":"close"}`, "", 404},
		{"gitlab note undecodable", http.MethodPost, gl + "/api/v4/projects/7/issues/100/notes", "{not json", "", 400},
		{"gitlab note unseeded", http.MethodPost, gl + "/api/v4/projects/7/issues/999/notes", `{"body":"x"}`, "", 404},
		{"gitlab list notes unseeded", http.MethodGet, gl + "/api/v4/projects/7/issues/999/notes", "", "", 404},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			if row.fault != "" {
				f.SetFault(row.fault, true)
				defer f.SetFault(row.fault, false)
			}
			req, _ := http.NewRequest(row.method, row.url, strings.NewReader(row.body))
			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("%s %s: %v", row.method, row.url, err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != row.want {
				t.Fatalf("%s %s = %d, want %d", row.method, row.url, resp.StatusCode, row.want)
			}
			if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
				t.Fatalf("Content-Type = %q, want application/json", ct)
			}
		})
	}
	ghIs, _ := f.GetIssue(stub.ForgeGitHub, ghRepo, 0, 100)
	glIs, _ := f.GetIssue(stub.ForgeGitLab, "", glProjID, 100)
	if ghIs.State != "open" || len(ghIs.Comments) != 0 || glIs.State != "opened" || len(glIs.Comments) != 0 {
		t.Fatalf("edge requests mutated state: github=%+v gitlab=%+v", ghIs, glIs)
	}
}

// TestStub_EditFieldsThroughPatchAndPut pins the non-state fields the edit
// endpoints accept (GitHub title/body, GitLab title/description) and that
// the GitLab notes list honours the client's per_page when no override is
// set.
func TestStub_EditFieldsThroughPatchAndPut(t *testing.T) {
	f := stub.New()
	seedGitHubIssue(t, f, 100, "open")
	seedGitLabIssue(t, f, 100, "opened", "a", "b", "c")
	client := f.HTTPClient()

	req, _ := http.NewRequest(http.MethodPatch, stub.GitHubBaseURL+"/repos/stub/parent-close/issues/100", strings.NewReader(`{"title":"T","body":"B"}`))
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if is, _ := f.GetIssue(stub.ForgeGitHub, ghRepo, 0, 100); is.Title != "T" || is.Body != "B" || is.State != "open" {
		t.Fatalf("github after PATCH = %+v", is)
	}

	req, _ = http.NewRequest(http.MethodPut, stub.GitLabBaseURL+"/api/v4/projects/7/issues/100", strings.NewReader(`{"title":"T","description":"D"}`))
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if is, _ := f.GetIssue(stub.ForgeGitLab, "", glProjID, 100); is.Title != "T" || is.Body != "D" || is.State != "opened" {
		t.Fatalf("gitlab after PUT = %+v", is)
	}

	// per_page=2 from the client, no override: two pages then exhaustion.
	req, _ = http.NewRequest(http.MethodGet, stub.GitLabBaseURL+"/api/v4/projects/7/issues/100/notes?per_page=2", nil)
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var page []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if len(page) != 2 || !strings.Contains(resp.Header.Get("Link"), `page=2>; rel="next"`) {
		t.Fatalf("page 1 = %v, Link = %q", page, resp.Header.Get("Link"))
	}
	// No per_page and no override: GitLab's default page size holds all three.
	req, _ = http.NewRequest(http.MethodGet, stub.GitLabBaseURL+"/api/v4/projects/7/issues/100/notes", nil)
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	page = nil
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if len(page) != 3 || resp.Header.Get("Link") != "" {
		t.Fatalf("default page = %v, Link = %q", page, resp.Header.Get("Link"))
	}
}

// --- helpers ----------------------------------------------------------------

func indexOf(list []string, want string) int {
	for i, s := range list {
		if s == want {
			return i
		}
	}
	return -1
}

func countPrefix(list []string, prefix string) int {
	n := 0
	for _, s := range list {
		if strings.HasPrefix(s, prefix) {
			n++
		}
	}
	return n
}
