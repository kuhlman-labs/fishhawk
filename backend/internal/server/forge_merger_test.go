package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	forgegitlab "github.com/kuhlman-labs/fishhawk/backend/internal/forge/gitlab"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// recordingMerger is a GitHubMerger that records each run it was asked to merge,
// so a delegation test can prove ForgeMerger's github-family arm calls the leaf.
type recordingMerger struct {
	mu    sync.Mutex
	calls []uuid.UUID
	heads []string
	err   error
}

func (m *recordingMerger) MergePullRequest(_ context.Context, runRow *run.Run, head string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, runRow.ID)
	m.heads = append(m.heads, head)
	return m.err
}

func (m *recordingMerger) gotHeads() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.heads...)
}

func (m *recordingMerger) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.calls)
}

// fakeMergeForge is a forge.Forge recording EnableAutoMerge / MergePullRequest.
// The rest embeds a nil forge.Forge, unreachable in these tests.
type fakeMergeForge struct {
	forge.Forge
	name      string
	enableErr error
	mergeErr  error

	enableCalls int
	mergeCalls  int
	scope       forge.CredentialScope
	repo        forge.RepoRef
	number      int
	method      forge.MergeMethod
}

func (f *fakeMergeForge) Name() string { return f.name }

func (f *fakeMergeForge) EnableAutoMerge(_ context.Context, scope forge.CredentialScope,
	repo forge.RepoRef, number int, method forge.MergeMethod) error {
	f.enableCalls++
	f.scope, f.repo, f.number, f.method = scope, repo, number, method
	return f.enableErr
}

func (f *fakeMergeForge) MergePullRequest(_ context.Context, scope forge.CredentialScope,
	repo forge.RepoRef, number int, method forge.MergeMethod) error {
	f.mergeCalls++
	f.scope, f.repo, f.number, f.method = scope, repo, number, method
	return f.mergeErr
}

// realGitLabMergeForge builds a real *forgegitlab.Forge pointed at an httptest
// GitLab mux serving the single merge PUT the adapter performs. It records the
// request body so the cross-boundary test can assert the wire shape. (The
// realGitLabForge shape copied from merge_observation_test.go — a distinct mux
// keyed on the /merge endpoint, per the run's binding condition to not modify
// that file.)
func realGitLabMergeForge(t *testing.T, gotBody *string) *forgegitlab.Forge {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /api/v4/projects/5/merge_requests/7/merge", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		*gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"iid":7,"state":"merged"}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return forgegitlab.New(srv.URL, forgegitlab.NewStaticCredentialProvider("glpat-test"),
		forgegitlab.WithHTTPClient(srv.Client()))
}

// gitlabMergeRun builds a valid gitlab-family run row for the merge tests.
func gitlabMergeRun(prURL string) *run.Run {
	ref := "gitlab:5"
	return &run.Run{
		ID:              uuid.New(),
		Repo:            "acme/widgets",
		InstallationRef: &ref,
		PullRequestURL:  &prURL,
	}
}

const canonicalGitLabMRURL = "https://gitlab.example.com/acme/widgets/-/merge_requests/7"

func TestForgeMerger_GitHubFamily_DelegatesAndNeverConsultsResolver(t *testing.T) {
	merger := &recordingMerger{}
	m := ForgeMerger{
		GitHub:   merger,
		Resolver: func(string) (forge.Forge, error) { t.Fatal("resolver consulted for github family"); return nil, nil },
	}
	id42 := int64(42)
	// An installation-id run (nil ref => github family) and a nil-ref run both
	// delegate to the github leaf, never the resolver.
	runs := []*run.Run{
		{ID: uuid.New(), InstallationID: &id42},
		{ID: uuid.New()},
	}
	for _, rr := range runs {
		if err := m.MergePullRequest(context.Background(), rr, ""); err != nil {
			t.Fatalf("MergePullRequest(github family) error = %v, want nil", err)
		}
	}
	if merger.callCount() != 2 {
		t.Fatalf("github leaf calls = %d, want 2", merger.callCount())
	}
}

// TestForgeMerger_GitHubFamilyThreadsPinnedHead pins (#4641) that the github
// arm hands the evaluated head to the github leaf unchanged: a pinned call
// records the head, an unpinned call records "".
func TestForgeMerger_GitHubFamilyThreadsPinnedHead(t *testing.T) {
	merger := &recordingMerger{}
	m := ForgeMerger{GitHub: merger}
	id42 := int64(42)
	rr := &run.Run{ID: uuid.New(), InstallationID: &id42}
	const h1 = "1111111111111111111111111111111111111111"
	if err := m.MergePullRequest(context.Background(), rr, h1); err != nil {
		t.Fatalf("MergePullRequest(pinned) error = %v, want nil", err)
	}
	if err := m.MergePullRequest(context.Background(), rr, ""); err != nil {
		t.Fatalf("MergePullRequest(unpinned) error = %v, want nil", err)
	}
	if got := merger.gotHeads(); len(got) != 2 || got[0] != h1 || got[1] != "" {
		t.Fatalf("github leaf heads = %q, want [%q \"\"]", got, h1)
	}
}

// TestForgeMerger_NonGitHubPinnedHeadRefused pins ruling R6 (#4641): a
// non-github family has no pinned auto-merge, so a non-empty pin is REFUSED
// with ErrHeadPinnedMergeUnsupported before the forge is resolved or called —
// never silently dropped onto the unpinned EnableAutoMerge ladder.
func TestForgeMerger_NonGitHubPinnedHeadRefused(t *testing.T) {
	fake := &fakeMergeForge{name: "gitlab"}
	resolved := 0
	m := ForgeMerger{Resolver: func(string) (forge.Forge, error) { resolved++; return fake, nil }}
	err := m.MergePullRequest(context.Background(), gitlabMergeRun(canonicalGitLabMRURL), "2222222222222222222222222222222222222222")
	if !errors.Is(err, ErrHeadPinnedMergeUnsupported) {
		t.Fatalf("MergePullRequest(gitlab, pinned) error = %v, want ErrHeadPinnedMergeUnsupported", err)
	}
	if fake.enableCalls != 0 || fake.mergeCalls != 0 {
		t.Fatalf("forge calls enable=%d merge=%d, want 0/0 (refused before any forge call)", fake.enableCalls, fake.mergeCalls)
	}
	if resolved != 0 {
		t.Fatalf("resolver calls = %d, want 0 (refused before resolution)", resolved)
	}
}

func TestForgeMerger_GitHubFamily_NilGitHub_FailsClosed(t *testing.T) {
	m := ForgeMerger{} // GitHub is a nil interface, Resolver nil.
	err := m.MergePullRequest(context.Background(), &run.Run{ID: uuid.New()}, "")
	if err == nil {
		t.Fatal("MergePullRequest with nil github leaf = nil, want an error (no panic)")
	}
}

func TestForgeMerger_GitLabFamily_MergesViaResolvedForge(t *testing.T) {
	fake := &fakeMergeForge{name: "gitlab"}
	m := ForgeMerger{Resolver: func(string) (forge.Forge, error) { return fake, nil }}

	if err := m.MergePullRequest(context.Background(), gitlabMergeRun(canonicalGitLabMRURL), ""); err != nil {
		t.Fatalf("MergePullRequest(gitlab) error = %v, want nil", err)
	}
	if fake.enableCalls != 1 {
		t.Fatalf("EnableAutoMerge calls = %d, want 1", fake.enableCalls)
	}
	if fake.mergeCalls != 0 {
		t.Errorf("MergePullRequest (synchronous) calls = %d, want 0 (no clean-status fallback)", fake.mergeCalls)
	}
	if fake.scope.Ref() != "gitlab:5" {
		t.Errorf("scope = %q, want gitlab:5", fake.scope.Ref())
	}
	if fake.number != 7 {
		t.Errorf("MR number = %d, want 7 (from the URL)", fake.number)
	}
	if fake.method != forge.MergeMethodSquash {
		t.Errorf("method = %v, want squash", fake.method)
	}
}

func TestForgeMerger_GitLabFamily_CleanStatus_FallsBackToMerge(t *testing.T) {
	t.Run("clean status falls back", func(t *testing.T) {
		fake := &fakeMergeForge{name: "gitlab", enableErr: forge.ErrPullRequestCleanStatus}
		m := ForgeMerger{Resolver: func(string) (forge.Forge, error) { return fake, nil }}
		if err := m.MergePullRequest(context.Background(), gitlabMergeRun(canonicalGitLabMRURL), ""); err != nil {
			t.Fatalf("error = %v, want nil after fallback", err)
		}
		if fake.enableCalls != 1 || fake.mergeCalls != 1 {
			t.Fatalf("calls enable=%d merge=%d, want 1/1", fake.enableCalls, fake.mergeCalls)
		}
	})
	t.Run("unrelated error returned verbatim", func(t *testing.T) {
		sentinel := errors.New("boom")
		fake := &fakeMergeForge{name: "gitlab", enableErr: sentinel}
		m := ForgeMerger{Resolver: func(string) (forge.Forge, error) { return fake, nil }}
		err := m.MergePullRequest(context.Background(), gitlabMergeRun(canonicalGitLabMRURL), "")
		if !errors.Is(err, sentinel) {
			t.Fatalf("error = %v, want the sentinel returned verbatim", err)
		}
		if fake.mergeCalls != 0 {
			t.Errorf("MergePullRequest calls = %d, want 0 (no fallback on unrelated error)", fake.mergeCalls)
		}
	})
}

func TestForgeMerger_GitLabFamily_Rungs(t *testing.T) {
	fatalResolver := func(string) (forge.Forge, error) {
		t.Fatal("resolver consulted before target resolution")
		return nil, nil
	}
	okResolver := func(f forge.Forge) func(string) (forge.Forge, error) {
		return func(string) (forge.Forge, error) { return f, nil }
	}

	cases := []struct {
		name       string
		run        *run.Run
		resolver   func(string) (forge.Forge, error)
		wantErr    bool
		wantNoCall bool // resolver must NOT be reached
	}{
		{
			name:       "github-shaped url on gitlab ref",
			run:        gitlabMergeRun("https://github.com/acme/widgets/pull/7"),
			resolver:   fatalResolver,
			wantErr:    true,
			wantNoCall: true,
		},
		{
			name:       "malformed url",
			run:        gitlabMergeRun("https://gitlab.example.com/acme/widgets"),
			resolver:   fatalResolver,
			wantErr:    true,
			wantNoCall: true,
		},
		{
			name:       "repo/url project mismatch",
			run:        gitlabMergeRun("https://gitlab.example.com/other/project/-/merge_requests/7"),
			resolver:   fatalResolver,
			wantErr:    true,
			wantNoCall: true,
		},
		{
			name:       "unknown family gitea",
			run:        &run.Run{ID: uuid.New(), Repo: "acme/widgets", InstallationRef: ptrString("gitea:1"), PullRequestURL: ptrString(canonicalGitLabMRURL)},
			resolver:   fatalResolver,
			wantErr:    true,
			wantNoCall: true,
		},
		{
			name:     "resolver error",
			run:      gitlabMergeRun(canonicalGitLabMRURL),
			resolver: func(string) (forge.Forge, error) { return nil, errors.New("boom") },
			wantErr:  true,
		},
		{
			name:     "nil forge",
			run:      gitlabMergeRun(canonicalGitLabMRURL),
			resolver: func(string) (forge.Forge, error) { return nil, nil },
			wantErr:  true,
		},
		{
			name:     "typed-nil forge",
			run:      gitlabMergeRun(canonicalGitLabMRURL),
			resolver: okResolver((*fakeMergeForge)(nil)),
			wantErr:  true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := ForgeMerger{Resolver: tc.resolver}
			err := m.MergePullRequest(context.Background(), tc.run, "")
			if tc.wantErr && err == nil {
				t.Fatalf("error = nil, want an error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("error = %v, want nil", err)
			}
			_ = tc.wantNoCall // enforced by the fatalResolver t.Fatal
		})
	}
}

func TestForgeMerger_GitLabFamily_NilResolver_DefaultsToRegistry(t *testing.T) {
	t.Run("unregistered fails closed", func(t *testing.T) {
		snap := forge.SnapshotRegistry()
		t.Cleanup(func() { forge.RestoreRegistry(snap) })
		// No Resolver wired: MergePullRequest falls back to forge.Get, which
		// resolves nothing for "gitlab" in the test process → a resolve error,
		// not a panic.
		m := ForgeMerger{}
		err := m.MergePullRequest(context.Background(), gitlabMergeRun(canonicalGitLabMRURL), "")
		if err == nil {
			t.Fatal("MergePullRequest(nil resolver) = nil, want a forge-resolution error")
		}
	})

	t.Run("registered gitlab forge is dialed", func(t *testing.T) {
		snap := forge.SnapshotRegistry()
		t.Cleanup(func() { forge.RestoreRegistry(snap) })
		fake := &fakeMergeForge{name: "gitlab"}
		forge.Register(fake)

		// Resolver left nil → defaults to forge.Get, which finds the registered
		// fake.
		m := ForgeMerger{}
		if err := m.MergePullRequest(context.Background(), gitlabMergeRun(canonicalGitLabMRURL), ""); err != nil {
			t.Fatalf("MergePullRequest(registered gitlab forge) error = %v, want nil", err)
		}
		if fake.enableCalls != 1 {
			t.Fatalf("enableCalls = %d, want 1 (fake forge must be dialed via the process registry default)", fake.enableCalls)
		}
		if fake.scope.Ref() != "gitlab:5" {
			t.Errorf("scope = %q, want gitlab:5", fake.scope.Ref())
		}
		if fake.number != 7 {
			t.Errorf("number = %d, want 7", fake.number)
		}
	})
}

func TestForgeMerger_GitLabFamily_NoPullRequestURL_FailsClosed(t *testing.T) {
	// A gitlab-family run with no recorded PR url: resolveObservationTarget can
	// name no target, so the merge fails closed and prURLOf reads the nil url as
	// "" for the error message rather than panicking.
	ref := "gitlab:5"
	runRow := &run.Run{ID: uuid.New(), Repo: "acme/widgets", InstallationRef: &ref}
	m := ForgeMerger{Resolver: func(string) (forge.Forge, error) {
		t.Fatal("resolver consulted despite an unresolvable target")
		return nil, nil
	}}
	if err := m.MergePullRequest(context.Background(), runRow, ""); err == nil {
		t.Fatal("MergePullRequest(no pr url) = nil, want a fail-closed error")
	}
}

func TestForgeMerger_GitLab_RealAdapter_EndToEnd(t *testing.T) {
	var gotBody string
	realForge := realGitLabMergeForge(t, &gotBody)
	m := ForgeMerger{Resolver: func(string) (forge.Forge, error) { return realForge, nil }}

	if err := m.MergePullRequest(context.Background(), gitlabMergeRun(canonicalGitLabMRURL), ""); err != nil {
		t.Fatalf("MergePullRequest(real gitlab adapter) error = %v, want nil", err)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(gotBody), &body); err != nil {
		t.Fatalf("merge request body not JSON: %v (body=%q)", err, gotBody)
	}
	if body["merge_when_pipeline_succeeds"] != true {
		t.Errorf("body merge_when_pipeline_succeeds = %v, want true", body["merge_when_pipeline_succeeds"])
	}
	if body["squash"] != true {
		t.Errorf("body squash = %v, want true", body["squash"])
	}
}

// pinnedLeaf is a github-family leaf implementing BOTH GitHubMerger and
// HeadPinnedMerger, recording the pinned head and the call's deadline.
type pinnedLeaf struct {
	recordingMerger
	heads    []string
	deadline time.Duration
	err      error
}

func (p *pinnedLeaf) MergePullRequestAtHead(ctx context.Context, _ *run.Run, headSHA string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.heads = append(p.heads, headSHA)
	if dl, ok := ctx.Deadline(); ok {
		p.deadline = time.Until(dl)
	}
	return p.err
}

// pinnedMergeForge is a fakeMergeForge that ALSO implements
// forge.HeadPinnedMerger.
type pinnedMergeForge struct {
	fakeMergeForge
	pinnedCalls int
	pinnedHead  string
	pinnedErr   error
}

func (f *pinnedMergeForge) MergePullRequestAtHead(_ context.Context, scope forge.CredentialScope,
	repo forge.RepoRef, number int, method forge.MergeMethod, head string) error {
	f.pinnedCalls++
	f.scope, f.repo, f.number, f.method, f.pinnedHead = scope, repo, number, method, head
	return f.pinnedErr
}

// TestForgeMerger_MergePullRequestAtHead_Routes pins the head-pinned arm's
// family routing (ADR-093 D3, E87.5 / #4296): github family -> the pinned leaf
// (never the resolver, never the unpinned MergePullRequest); a non-pinned leaf
// or forge -> ErrHeadPinnedMergeUnsupported with ZERO merge calls of any kind;
// gitlab family -> the resolved forge's pinned merge with squash; an empty
// head refuses before any call; every forge call carries a bounded deadline.
func TestForgeMerger_MergePullRequestAtHead_Routes(t *testing.T) {
	id42 := int64(42)
	ghRun := &run.Run{ID: uuid.New(), InstallationID: &id42}
	fatalResolver := func(string) (forge.Forge, error) { t.Fatal("resolver consulted for github family"); return nil, nil }

	t.Run("github pinned leaf", func(t *testing.T) {
		leaf := &pinnedLeaf{}
		m := ForgeMerger{GitHub: leaf, Resolver: fatalResolver}
		if err := m.MergePullRequestAtHead(context.Background(), ghRun, "head1"); err != nil {
			t.Fatalf("MergePullRequestAtHead: %v", err)
		}
		if len(leaf.heads) != 1 || leaf.heads[0] != "head1" {
			t.Fatalf("pinned heads = %v, want [head1]", leaf.heads)
		}
		if leaf.callCount() != 0 {
			t.Errorf("unpinned MergePullRequest calls = %d, want 0", leaf.callCount())
		}
		if leaf.deadline <= 0 || leaf.deadline > headPinnedMergeCallTimeout {
			t.Errorf("leaf ctx deadline remaining = %v, want in (0, %v]", leaf.deadline, headPinnedMergeCallTimeout)
		}
	})

	t.Run("github pinned leaf error surfaces", func(t *testing.T) {
		leaf := &pinnedLeaf{err: forge.ErrMergeConflict}
		m := ForgeMerger{GitHub: leaf}
		if err := m.MergePullRequestAtHead(context.Background(), ghRun, "head1"); !errors.Is(err, forge.ErrMergeConflict) {
			t.Fatalf("err = %v, want ErrMergeConflict", err)
		}
	})

	t.Run("github leaf not pinned", func(t *testing.T) {
		leaf := &recordingMerger{}
		m := ForgeMerger{GitHub: leaf, Resolver: fatalResolver}
		err := m.MergePullRequestAtHead(context.Background(), ghRun, "head1")
		if !errors.Is(err, ErrHeadPinnedMergeUnsupported) {
			t.Fatalf("err = %v, want ErrHeadPinnedMergeUnsupported", err)
		}
		if leaf.callCount() != 0 {
			t.Errorf("unpinned fallback taken: MergePullRequest calls = %d, want 0", leaf.callCount())
		}
	})

	t.Run("github nil leaf", func(t *testing.T) {
		m := ForgeMerger{Resolver: fatalResolver}
		if err := m.MergePullRequestAtHead(context.Background(), ghRun, "head1"); err == nil {
			t.Fatal("err = nil, want no-github-client refusal")
		}
	})

	t.Run("empty head refused before any call", func(t *testing.T) {
		leaf := &pinnedLeaf{}
		m := ForgeMerger{GitHub: leaf, Resolver: fatalResolver}
		for _, h := range []string{"", "  "} {
			if err := m.MergePullRequestAtHead(context.Background(), ghRun, h); err == nil {
				t.Errorf("head %q: err = nil, want refusal", h)
			}
		}
		if len(leaf.heads) != 0 {
			t.Errorf("pinned calls = %d, want 0", len(leaf.heads))
		}
	})

	t.Run("gitlab pinned forge", func(t *testing.T) {
		f := &pinnedMergeForge{fakeMergeForge: fakeMergeForge{name: "gitlab"}}
		m := ForgeMerger{Resolver: func(string) (forge.Forge, error) { return f, nil }}
		if err := m.MergePullRequestAtHead(context.Background(), gitlabMergeRun(canonicalGitLabMRURL), "head2"); err != nil {
			t.Fatalf("MergePullRequestAtHead: %v", err)
		}
		if f.pinnedCalls != 1 || f.pinnedHead != "head2" || f.number != 7 || f.method != forge.MergeMethodSquash {
			t.Fatalf("pinned call = (%d, %q, #%d, %q), want (1, head2, #7, squash)", f.pinnedCalls, f.pinnedHead, f.number, f.method)
		}
		if f.scope.Ref() != "gitlab:5" {
			t.Errorf("scope = %q, want gitlab:5", f.scope.Ref())
		}
		if f.enableCalls != 0 || f.mergeCalls != 0 {
			t.Errorf("unpinned calls enable=%d merge=%d, want 0/0", f.enableCalls, f.mergeCalls)
		}
	})

	t.Run("gitlab forge not pinned", func(t *testing.T) {
		f := &fakeMergeForge{name: "gitlab"}
		m := ForgeMerger{Resolver: func(string) (forge.Forge, error) { return f, nil }}
		err := m.MergePullRequestAtHead(context.Background(), gitlabMergeRun(canonicalGitLabMRURL), "head2")
		if !errors.Is(err, ErrHeadPinnedMergeUnsupported) {
			t.Fatalf("err = %v, want ErrHeadPinnedMergeUnsupported", err)
		}
		if f.enableCalls != 0 || f.mergeCalls != 0 {
			t.Errorf("unpinned fallback taken: enable=%d merge=%d, want 0/0", f.enableCalls, f.mergeCalls)
		}
	})

	t.Run("gitlab malformed url refuses before resolver", func(t *testing.T) {
		m := ForgeMerger{Resolver: func(string) (forge.Forge, error) {
			t.Fatal("resolver consulted before target resolution")
			return nil, nil
		}}
		if err := m.MergePullRequestAtHead(context.Background(), gitlabMergeRun("https://gitlab.example.com/acme/widgets"), "head2"); err == nil {
			t.Fatal("err = nil, want malformed-url refusal")
		}
	})

	t.Run("gitlab real adapter sends sha", func(t *testing.T) {
		var body string
		gl := realGitLabMergeForge(t, &body)
		m := ForgeMerger{Resolver: func(string) (forge.Forge, error) { return gl, nil }}
		if err := m.MergePullRequestAtHead(context.Background(), gitlabMergeRun(canonicalGitLabMRURL), "head3"); err != nil {
			t.Fatalf("MergePullRequestAtHead: %v", err)
		}
		var got map[string]any
		if err := json.Unmarshal([]byte(body), &got); err != nil {
			t.Fatalf("decode %q: %v", body, err)
		}
		if got["sha"] != "head3" {
			t.Errorf("sha = %v, want head3", got["sha"])
		}
		if _, ok := got["merge_when_pipeline_succeeds"]; ok {
			t.Errorf("pinned merge queued auto-merge: %s", body)
		}
	})
}
