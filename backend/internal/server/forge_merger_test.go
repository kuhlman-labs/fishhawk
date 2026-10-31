package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
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

// TestClassifyMergeDispatchOutcome_HeadPinRefusalStaysUncertain (#4683) pins
// that a pre-forge head-pin refusal stays UNCERTAIN (dispatch_failed) on the
// merge-dispatch ledger. It is a deterministic capability refusal: releasing
// it would re-dispatch into the same refusal (a 502 retry loop inline, a
// re-claim every tick delegated, a step-8 re-claim every pump on merge-on-pass,
// ADR-093 D4). The second row is the exact error ForgeMerger returns for a
// pinned GitLab run, which wraps none of the four release sentinels. The
// expected outcome is hard-coded. Counterfactual: adding
// errors.Is(err, ErrHeadPinnedMergeUnsupported) to the release case of
// classifyMergeDispatchOutcome flips both rows to released, RED.
func TestClassifyMergeDispatchOutcome_HeadPinRefusalStaysUncertain(t *testing.T) {
	m := ForgeMerger{Resolver: func(string) (forge.Forge, error) { return &fakeMergeForge{name: "gitlab"}, nil }}
	refusal := m.MergePullRequest(context.Background(), gitlabMergeRun(canonicalGitLabMRURL), "2222222222222222222222222222222222222222")
	if !errors.Is(refusal, ErrHeadPinnedMergeUnsupported) {
		t.Fatalf("ForgeMerger pinned GitLab error = %v, want it to wrap ErrHeadPinnedMergeUnsupported", refusal)
	}
	for name, err := range map[string]error{
		"bare sentinel":              ErrHeadPinnedMergeUnsupported,
		"ForgeMerger pinned refusal": refusal,
	} {
		if got := classifyMergeDispatchOutcome(err); got != mergeDispatchUncertain {
			t.Errorf("%s: classifyMergeDispatchOutcome = %q, want %q (a deterministic refusal must not release)", name, got, mergeDispatchUncertain)
		}
	}
}

// TestHeadPinnedMergeUnsupported_UnreachableWithoutGitHubInstallation (#4683)
// pins why the inline and delegated paths never meet the head-pin refusal: a
// run with no GitHub installation (every GitLab-family run) gets an EMPTY gate
// head from mergeCandidateGate, so its dispatch is unpinned. The fixture is the
// production GitLab shape with a CONSISTENT repository and merge request
// (acme/widgets), so the observation target resolves; the isolation
// precondition shows the SAME run pinned to a non-empty head IS refused with
// zero enables, which proves only the empty gate head avoids the refusal.
// Counterfactual: returning a non-empty HeadSHA from mergeCandidateGate's
// no-installation arm reddens the gate-head assertion.
func TestHeadPinnedMergeUnsupported_UnreachableWithoutGitHubInstallation(t *testing.T) {
	g := newMCGateFixture(t, mcvUndelegatedSpecYAML)
	g.repo.mu.Lock()
	r := g.repo.runs[g.runID]
	r.InstallationID = nil
	r.InstallationRef = ptrString("gitlab:5")
	r.PullRequestURL = ptrString(canonicalGitLabMRURL)
	r.Repo = "acme/widgets"
	g.repo.mu.Unlock()
	ctx := context.Background()
	runRow, err := g.repo.GetRun(ctx, g.runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}

	gate := g.s.mergeCandidateGate(ctx, runRow)
	if gate.HeadSHA != "" {
		t.Fatalf("gate HeadSHA = %q, want empty (no GitHub installation: the gate cannot probe, so the dispatch is unpinned)", gate.HeadSHA)
	}
	g.gh.mu.Lock()
	compares := g.gh.compareCalls
	g.gh.mu.Unlock()
	if compares != 0 {
		t.Fatalf("GitHub compare calls = %d, want 0 (warnUngated returns before any GitHub read)", compares)
	}

	// Isolation precondition: the same run pinned to a non-empty head IS refused
	// before any forge call.
	probe := &fakeMergeForge{name: "gitlab"}
	if err := (ForgeMerger{Resolver: func(string) (forge.Forge, error) { return probe, nil }}).MergePullRequest(ctx, runRow, "h1"); !errors.Is(err, ErrHeadPinnedMergeUnsupported) {
		t.Fatalf("pinned dispatch error = %v, want ErrHeadPinnedMergeUnsupported (the fixture must reach the refusal branch)", err)
	}
	if probe.enableCalls != 0 {
		t.Fatalf("pinned dispatch enable calls = %d, want 0", probe.enableCalls)
	}

	// The gate's (empty) head dispatches unpinned and reaches the forge.
	fake := &fakeMergeForge{name: "gitlab"}
	merr := (ForgeMerger{Resolver: func(string) (forge.Forge, error) { return fake, nil }}).MergePullRequest(ctx, runRow, gate.HeadSHA)
	if merr != nil || errors.Is(merr, ErrHeadPinnedMergeUnsupported) {
		t.Fatalf("unpinned dispatch error = %v, want nil", merr)
	}
	if fake.enableCalls != 1 || fake.repo != (forge.RepoRef{Owner: "acme", Name: "widgets"}) || fake.number != 7 {
		t.Fatalf("forge enable calls = %d repo = %+v number = %d, want 1 acme/widgets #7", fake.enableCalls, fake.repo, fake.number)
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

// ---- E93.2 / #4658: merge method resolution, attribution, static refusal ----

// fmmSpec is a minimal non-Fishhawk workflow-v2 admission spec; a non-empty
// decl is appended as the top-level merge_method declaration.
func fmmSpec(decl string) []byte {
	doc := `version: "2"
workflows:
  feature_change:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
      - id: implement
        type: implement
        executor:
          agent: claude-code
        produces:
          - artifact: pull_request
`
	if decl != "" {
		doc += "merge_method: " + decl + "\n"
	}
	return []byte(doc)
}

// fmmUnparseableSpec is admission-spec bytes ParseBytes refuses.
var fmmUnparseableSpec = []byte("version: [\n")

// fmmSecretText is forge response text carrying a token-bearing URL; no
// merge-method surface or log line may carry it.
const fmmSecretText = "FORGE-SECRET-TEXT https://x-access-token:ghs_SENTINEL@github.com/acme/widgets.git"

func fmmGitHubRun(specBytes []byte) *run.Run {
	id := int64(42)
	return &run.Run{ID: uuid.New(), InstallationID: &id, WorkflowSpec: specBytes}
}

func fmmGitLabRun(specBytes []byte) *run.Run {
	r := gitlabMergeRun(canonicalGitLabMRURL)
	r.WorkflowSpec = specBytes
	return r
}

// fmmPolicyLeaf is a github-family leaf implementing GitHubMerger,
// HeadPinnedMerger and MergeMethodPolicyReader: it returns err from both merge
// entry points and answers the settings read with allowed / readErr, honouring
// ctx and recording the read's deadline.
type fmmPolicyLeaf struct {
	mu           sync.Mutex
	err          error
	allowed      forge.AllowedMergeMethods
	readErr      error
	reads        int
	readDeadline time.Duration
	mergeCalls   int
}

func (l *fmmPolicyLeaf) MergePullRequest(context.Context, *run.Run, string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.mergeCalls++
	return l.err
}

func (l *fmmPolicyLeaf) MergePullRequestAtHead(context.Context, *run.Run, string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.mergeCalls++
	return l.err
}

func (l *fmmPolicyLeaf) AllowedMergeMethods(ctx context.Context, _ *run.Run) (forge.AllowedMergeMethods, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.reads++
	if dl, ok := ctx.Deadline(); ok {
		l.readDeadline = time.Until(dl)
	}
	if err := ctx.Err(); err != nil {
		return forge.AllowedMergeMethods{}, err
	}
	return l.allowed, l.readErr
}

// fmmNotMergeable is a REST 405 refusal whose wrapped text carries the secret.
func fmmNotMergeable() error {
	return fmt.Errorf("%w: merge pull request #7: %s", forge.ErrPullRequestNotMergeable, fmmSecretText)
}

// fmmCapture returns a ForgeMerger logger writing JSON lines to the buffer.
func fmmCapture() (*slog.Logger, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})), buf
}

func TestResolveRunMergeMethod(t *testing.T) {
	cases := []struct {
		name       string
		spec       []byte
		want       forge.MergeMethod
		wantSource string
	}{
		{"declared squash", fmmSpec("squash"), forge.MergeMethodSquash, MergeMethodSourceDeclared},
		{"declared merge", fmmSpec("merge"), forge.MergeMethodMerge, MergeMethodSourceDeclared},
		{"declared rebase", fmmSpec("rebase"), forge.MergeMethodRebase, MergeMethodSourceDeclared},
		{"no declaration", fmmSpec(""), forge.MergeMethodSquash, MergeMethodSourceDefault},
		{"no cached spec", nil, forge.MergeMethodSquash, MergeMethodSourceDefault},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mm, err := ResolveRunMergeMethod(&run.Run{ID: uuid.New(), WorkflowSpec: tc.spec})
			if err != nil {
				t.Fatalf("ResolveRunMergeMethod: %v", err)
			}
			if mm.Method != tc.want || mm.Source != tc.wantSource {
				t.Errorf("got %+v, want %s/%s", mm, tc.want, tc.wantSource)
			}
			if mm.Name() != forge.MergeMethodName(tc.want) {
				t.Errorf("Name() = %q", mm.Name())
			}
		})
	}
	declared, _ := ResolveRunMergeMethod(&run.Run{WorkflowSpec: fmmSpec("merge")})
	if !strings.Contains(declared.Citation(), ".fishhawk/workflows.yaml merge_method") {
		t.Errorf("declared citation = %q, want it to name the declaration", declared.Citation())
	}
	def, _ := ResolveRunMergeMethod(&run.Run{})
	if def.Citation() != "default (no merge_method declared)" {
		t.Errorf("default citation = %q", def.Citation())
	}
	if _, err := ResolveRunMergeMethod(&run.Run{ID: uuid.New(), WorkflowSpec: fmmUnparseableSpec}); !errors.Is(err, ErrMergeMethodUndeterminable) {
		t.Errorf("unparseable spec: err = %v, want ErrMergeMethodUndeterminable", err)
	}
}

// TestForgeMerger_MergeMethodThreadedPerFamily: the gitlab arm passes the
// resolved method to EnableAutoMerge, the clean-status MergePullRequest
// fallback and the pinned merge — never a hard-coded squash.
func TestForgeMerger_MergeMethodThreadedPerFamily(t *testing.T) {
	for _, tc := range []struct {
		name string
		spec []byte
		want forge.MergeMethod
	}{
		{"declared squash", fmmSpec("squash"), forge.MergeMethodSquash},
		{"declared merge", fmmSpec("merge"), forge.MergeMethodMerge},
		{"default", nil, forge.MergeMethodSquash},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Run("EnableAutoMerge", func(t *testing.T) {
				f := &fakeMergeForge{name: "gitlab"}
				m := ForgeMerger{Resolver: func(string) (forge.Forge, error) { return f, nil }}
				if err := m.MergePullRequest(context.Background(), fmmGitLabRun(tc.spec), ""); err != nil {
					t.Fatalf("MergePullRequest: %v", err)
				}
				if f.enableCalls != 1 || f.method != tc.want {
					t.Errorf("enable calls %d method %q, want 1 %q", f.enableCalls, f.method, tc.want)
				}
			})
			t.Run("clean-status fallback", func(t *testing.T) {
				f := &fakeMergeForge{name: "gitlab", enableErr: forge.ErrPullRequestCleanStatus}
				m := ForgeMerger{Resolver: func(string) (forge.Forge, error) { return f, nil }}
				if err := m.MergePullRequest(context.Background(), fmmGitLabRun(tc.spec), ""); err != nil {
					t.Fatalf("MergePullRequest: %v", err)
				}
				if f.mergeCalls != 1 || f.method != tc.want {
					t.Errorf("merge calls %d method %q, want 1 %q", f.mergeCalls, f.method, tc.want)
				}
			})
			t.Run("MergePullRequestAtHead", func(t *testing.T) {
				f := &pinnedMergeForge{fakeMergeForge: fakeMergeForge{name: "gitlab"}}
				m := ForgeMerger{Resolver: func(string) (forge.Forge, error) { return f, nil }}
				if err := m.MergePullRequestAtHead(context.Background(), fmmGitLabRun(tc.spec), "h1"); err != nil {
					t.Fatalf("MergePullRequestAtHead: %v", err)
				}
				if f.pinnedCalls != 1 || f.method != tc.want {
					t.Errorf("pinned calls %d method %q, want 1 %q", f.pinnedCalls, f.method, tc.want)
				}
			})
		})
	}
}

// TestForgeMerger_UnparseableSpecUndeterminable: both entry points on both
// families fail closed with ErrMergeMethodUndeterminable with ZERO leaf calls
// and ZERO resolver calls.
func TestForgeMerger_UnparseableSpecUndeterminable(t *testing.T) {
	resolverCalls := 0
	resolver := func(string) (forge.Forge, error) {
		resolverCalls++
		return &pinnedMergeForge{fakeMergeForge: fakeMergeForge{name: "gitlab"}}, nil
	}
	leaf := &fmmPolicyLeaf{}
	m := ForgeMerger{GitHub: leaf, Resolver: resolver}
	for _, rr := range []*run.Run{fmmGitHubRun(fmmUnparseableSpec), fmmGitLabRun(fmmUnparseableSpec)} {
		if err := m.MergePullRequest(context.Background(), rr, ""); !errors.Is(err, ErrMergeMethodUndeterminable) {
			t.Errorf("MergePullRequest err = %v, want ErrMergeMethodUndeterminable", err)
		}
		if err := m.MergePullRequestAtHead(context.Background(), rr, "h1"); !errors.Is(err, ErrMergeMethodUndeterminable) {
			t.Errorf("MergePullRequestAtHead err = %v, want ErrMergeMethodUndeterminable", err)
		}
	}
	if leaf.mergeCalls != 0 || resolverCalls != 0 {
		t.Errorf("leaf calls %d resolver calls %d, want 0/0", leaf.mergeCalls, resolverCalls)
	}
}

// TestForgeMerger_MergeMethodAttribution: one case per attribution mode, on
// both entry points of the github arm.
func TestForgeMerger_MergeMethodAttribution(t *testing.T) {
	allowAll := forge.AllowedMergeMethods{Squash: true, Merge: true, Rebase: true}
	noMerge := forge.AllowedMergeMethods{Squash: true, Rebase: true}
	entry := map[string]func(m ForgeMerger, ctx context.Context, r *run.Run) error{
		"MergePullRequest": func(m ForgeMerger, ctx context.Context, r *run.Run) error {
			return m.MergePullRequest(ctx, r, "")
		},
		"MergePullRequestAtHead": func(m ForgeMerger, ctx context.Context, r *run.Run) error {
			return m.MergePullRequestAtHead(ctx, r, "h1")
		},
	}
	for name, call := range entry {
		t.Run(name, func(t *testing.T) {
			t.Run("nil error", func(t *testing.T) {
				leaf := &fmmPolicyLeaf{allowed: noMerge}
				if err := call(ForgeMerger{GitHub: leaf}, context.Background(), fmmGitHubRun(fmmSpec("merge"))); err != nil {
					t.Fatalf("err = %v, want nil", err)
				}
				if leaf.reads != 0 {
					t.Errorf("reads = %d, want 0", leaf.reads)
				}
			})
			t.Run("non-candidate class: zero reads", func(t *testing.T) {
				leaf := &fmmPolicyLeaf{err: forge.ErrMergeConflict, allowed: noMerge}
				err := call(ForgeMerger{GitHub: leaf}, context.Background(), fmmGitHubRun(fmmSpec("merge")))
				if !errors.Is(err, forge.ErrMergeConflict) || errors.Is(err, forge.ErrMergeMethodNotAllowed) {
					t.Errorf("err = %v, want the conflict unchanged", err)
				}
				if leaf.reads != 0 {
					t.Errorf("reads = %d, want 0 for a non-candidate refusal", leaf.reads)
				}
			})
			t.Run("candidate with no reader", func(t *testing.T) {
				leaf := &pinnedLeaf{err: fmmNotMergeable()}
				leaf.recordingMerger.err = fmmNotMergeable()
				err := call(ForgeMerger{GitHub: leaf}, context.Background(), fmmGitHubRun(fmmSpec("merge")))
				if !errors.Is(err, forge.ErrPullRequestNotMergeable) || errors.Is(err, forge.ErrMergeMethodNotAllowed) {
					t.Errorf("err = %v, want the plain refusal", err)
				}
			})
			t.Run("read error leaves the refusal unchanged", func(t *testing.T) {
				leaf := &fmmPolicyLeaf{err: fmmNotMergeable(), readErr: errors.New("boom")}
				err := call(ForgeMerger{GitHub: leaf}, context.Background(), fmmGitHubRun(fmmSpec("merge")))
				if !errors.Is(err, forge.ErrPullRequestNotMergeable) || errors.Is(err, forge.ErrMergeMethodNotAllowed) {
					t.Errorf("err = %v, want the plain refusal", err)
				}
				if leaf.reads != 1 {
					t.Errorf("reads = %d, want 1", leaf.reads)
				}
				if leaf.readDeadline <= 0 || leaf.readDeadline > mergeMethodAttributionTimeout {
					t.Errorf("read deadline remaining = %v, want in (0, %v]", leaf.readDeadline, mergeMethodAttributionTimeout)
				}
			})
			t.Run("spent caller deadline leaves the refusal unchanged", func(t *testing.T) {
				leaf := &fmmPolicyLeaf{err: fmmNotMergeable(), allowed: noMerge}
				ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
				defer cancel()
				err := call(ForgeMerger{GitHub: leaf}, ctx, fmmGitHubRun(fmmSpec("merge")))
				if !errors.Is(err, forge.ErrPullRequestNotMergeable) || errors.Is(err, forge.ErrMergeMethodNotAllowed) {
					t.Errorf("err = %v, want the plain refusal", err)
				}
				if leaf.reads != 0 {
					t.Errorf("reads = %d, want 0 (no time left for the read)", leaf.reads)
				}
			})
			t.Run("allowed leaves the refusal unchanged", func(t *testing.T) {
				leaf := &fmmPolicyLeaf{err: fmmNotMergeable(), allowed: allowAll}
				err := call(ForgeMerger{GitHub: leaf}, context.Background(), fmmGitHubRun(fmmSpec("merge")))
				if !errors.Is(err, forge.ErrPullRequestNotMergeable) || errors.Is(err, forge.ErrMergeMethodNotAllowed) {
					t.Errorf("err = %v, want the plain refusal", err)
				}
				if leaf.reads != 1 {
					t.Errorf("reads = %d, want 1", leaf.reads)
				}
			})
			for _, dc := range []struct {
				name       string
				spec       []byte
				allowed    forge.AllowedMergeMethods
				wantMethod string
				wantSource string
				wantCite   string
			}{
				{"disallowed declared", fmmSpec("merge"), noMerge, "merge", MergeMethodSourceDeclared, ".fishhawk/workflows.yaml merge_method"},
				{"disallowed default", nil, forge.AllowedMergeMethods{Merge: true}, "squash", MergeMethodSourceDefault, "default (no merge_method declared)"},
			} {
				t.Run(dc.name, func(t *testing.T) {
					leaf := &fmmPolicyLeaf{err: fmmNotMergeable(), allowed: dc.allowed}
					err := call(ForgeMerger{GitHub: leaf}, context.Background(), fmmGitHubRun(dc.spec))
					if !errors.Is(err, forge.ErrMergeMethodNotAllowed) || !errors.Is(err, forge.ErrPullRequestNotMergeable) {
						t.Fatalf("err = %v, want ErrMergeMethodNotAllowed AND ErrPullRequestNotMergeable", err)
					}
					var ref *MergeMethodRefusalError
					if !errors.As(err, &ref) {
						t.Fatalf("err = %v, want a *MergeMethodRefusalError", err)
					}
					if ref.Method != dc.wantMethod || ref.Source != dc.wantSource || ref.Reason != MergeMethodRefusalForgeDisallows {
						t.Errorf("refusal = %+v", ref)
					}
					if !strings.Contains(ref.Citation(), dc.wantCite) || !strings.Contains(err.Error(), dc.wantCite) {
						t.Errorf("citation = %q / error %q, want %q", ref.Citation(), err.Error(), dc.wantCite)
					}
					if !strings.HasPrefix(err.Error(), "merge_method_refused:") {
						t.Errorf("Error() = %q, want the merge_method_refused: lead", err.Error())
					}
					if strings.Join(ref.Allowed, ",") != strings.Join(dc.allowed.Names(), ",") {
						t.Errorf("Allowed = %v, want %v", ref.Allowed, dc.allowed.Names())
					}
				})
			}
		})
	}
}

// TestForgeMerger_AttributionCandidateClasses pins which refusals trigger the
// settings re-read.
func TestForgeMerger_AttributionCandidateClasses(t *testing.T) {
	cases := []struct {
		name      string
		err       error
		wantReads int
	}{
		{"not mergeable", forge.ErrPullRequestNotMergeable, 1},
		{"unprocessable", forge.ErrMergeUnprocessable, 1},
		{"bare validation", forge.ErrValidation, 1},
		{"clean status", errors.Join(forge.ErrValidation, forge.ErrPullRequestCleanStatus), 0},
		{"unstable status", errors.Join(forge.ErrValidation, forge.ErrPullRequestUnstableStatus), 0},
		{"merge conflict", forge.ErrMergeConflict, 0},
		{"validation conflict", errors.Join(forge.ErrValidation, forge.ErrMergeConflict), 0},
		{"opaque", errors.New("502"), 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			leaf := &fmmPolicyLeaf{err: tc.err, allowed: forge.AllowedMergeMethods{Squash: true}}
			err := ForgeMerger{GitHub: leaf}.MergePullRequest(context.Background(), fmmGitHubRun(fmmSpec("merge")), "")
			if leaf.reads != tc.wantReads {
				t.Errorf("reads = %d, want %d", leaf.reads, tc.wantReads)
			}
			if got := errors.Is(err, forge.ErrMergeMethodNotAllowed); got != (tc.wantReads == 1) {
				t.Errorf("attributed = %v, want %v (err %v)", got, tc.wantReads == 1, err)
			}
		})
	}
}

// TestForgeMerger_AdapterUnsupportedAttributed: the GitLab adapter's
// defence-in-depth rebase refusal surfaces as a forge_unsupported refusal
// whose Allowed is the gitlab family table.
func TestForgeMerger_AdapterUnsupportedAttributed(t *testing.T) {
	adapterErr := fmt.Errorf("%w: %w: forge/gitlab: refused", forge.ErrMergeMethodUnsupported, forge.ErrUnsupported)
	for name, call := range map[string]func(m ForgeMerger) error{
		"MergePullRequest": func(m ForgeMerger) error {
			return m.MergePullRequest(context.Background(), fmmGitLabRun(fmmSpec("rebase")), "")
		},
		"MergePullRequestAtHead": func(m ForgeMerger) error {
			return m.MergePullRequestAtHead(context.Background(), fmmGitLabRun(fmmSpec("rebase")), "h1")
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := &pinnedMergeForge{fakeMergeForge: fakeMergeForge{name: "gitlab", enableErr: adapterErr}, pinnedErr: adapterErr}
			err := call(ForgeMerger{Resolver: func(string) (forge.Forge, error) { return f, nil }})
			var ref *MergeMethodRefusalError
			if !errors.As(err, &ref) || ref.Reason != MergeMethodRefusalForgeUnsupported {
				t.Fatalf("err = %v, want a forge_unsupported refusal", err)
			}
			if ref.Method != "rebase" || strings.Join(ref.Allowed, ",") != "squash,merge" {
				t.Errorf("refusal = %+v, want rebase with Allowed [squash merge]", ref)
			}
			if !errors.Is(err, forge.ErrMergeMethodUnsupported) || !errors.Is(err, forge.ErrUnsupported) {
				t.Errorf("err = %v, want ErrMergeMethodUnsupported and ErrUnsupported", err)
			}
		})
	}
}

// TestForgeMerger_AttributionLogsNoForgeText (approval condition C3): the
// attribution path's log lines — a settings read error, and a successful
// attribution of a forge refusal — carry method names and closed-set codes
// only: no ghs_ sentinel and no forge response text.
func TestForgeMerger_AttributionLogsNoForgeText(t *testing.T) {
	for _, tc := range []struct {
		name     string
		leaf     *fmmPolicyLeaf
		wantLine string
	}{
		{"read error", &fmmPolicyLeaf{err: fmmNotMergeable(), readErr: errors.New("read failed: " + fmmSecretText)}, "repository merge settings unreadable"},
		{"forge error attributed", &fmmPolicyLeaf{err: fmmNotMergeable(), allowed: forge.AllowedMergeMethods{Squash: true}}, "attributed to the declared merge method"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logger, buf := fmmCapture()
			err := ForgeMerger{GitHub: tc.leaf, Logger: logger}.MergePullRequest(context.Background(), fmmGitHubRun(fmmSpec("merge")), "")
			if err == nil {
				t.Fatal("err = nil")
			}
			out := buf.String()
			if !strings.Contains(out, tc.wantLine) || !strings.Contains(out, `"merge_method":"merge"`) {
				t.Fatalf("log = %q, want the %q line with the method attribute (non-vacuity)", out, tc.wantLine)
			}
			for _, bad := range []string{"ghs_SENTINEL", "FORGE-SECRET-TEXT", "read failed"} {
				if strings.Contains(out, bad) {
					t.Errorf("log carries %q: %s", bad, out)
				}
			}
			var ref *MergeMethodRefusalError
			if errors.As(err, &ref) {
				for _, s := range []string{ref.Error(), ref.FieldDetail()} {
					if strings.Contains(s, "ghs_SENTINEL") || strings.Contains(s, "FORGE-SECRET-TEXT") {
						t.Errorf("refusal rendering carries forge text: %q", s)
					}
				}
			}
		})
	}
}

func TestMergeMethodRefusalError_Rendering(t *testing.T) {
	e := &MergeMethodRefusalError{Method: "rebase", Source: MergeMethodSourceDeclared, Reason: MergeMethodRefusalForgeUnsupported, Cause: forge.ErrMergeMethodUnsupported}
	if got := e.Unwrap(); len(got) != 1 || got[0] != forge.ErrMergeMethodUnsupported {
		t.Errorf("Unwrap() = %v, want only the sentinel when Cause IS the sentinel", got)
	}
	if !strings.Contains(e.FieldDetail(), "allowed merge methods: none") {
		t.Errorf("FieldDetail() = %q, want 'none' for an empty allowed set", e.FieldDetail())
	}
	und := &MergeMethodRefusalError{Reason: MergeMethodRefusalUndeterminable, Allowed: []string{"squash"}, Cause: errors.New(fmmSecretText)}
	if !errors.Is(und, ErrMergeMethodUndeterminable) {
		t.Error("an undeterminable refusal must match ErrMergeMethodUndeterminable")
	}
	if strings.Contains(und.Error(), "ghs_SENTINEL") || !strings.Contains(und.FieldDetail(), "undeterminable") {
		t.Errorf("undeterminable rendering = %q", und.Error())
	}
}

// TestStaticMergeMethodRefusal pins the pre-claim predicate's rows, including
// approval condition C2: an unparseable admission spec FAILS CLOSED with the
// undeterminable reason, never nil.
func TestStaticMergeMethodRefusal(t *testing.T) {
	cases := []struct {
		name       string
		run        *run.Run
		wantReason string // "" = nil (no refusal)
	}{
		{"gitlab x rebase", fmmGitLabRun(fmmSpec("rebase")), MergeMethodRefusalForgeUnsupported},
		{"gitlab x squash", fmmGitLabRun(fmmSpec("squash")), ""},
		{"gitlab x merge", fmmGitLabRun(fmmSpec("merge")), ""},
		{"gitlab x default", fmmGitLabRun(nil), ""},
		{"github x rebase", fmmGitHubRun(fmmSpec("rebase")), ""},
		{"github nil spec", fmmGitHubRun(nil), ""},
		{"github unparseable spec", fmmGitHubRun(fmmUnparseableSpec), MergeMethodRefusalUndeterminable},
		{"gitlab unparseable spec", fmmGitLabRun(fmmUnparseableSpec), MergeMethodRefusalUndeterminable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := staticMergeMethodRefusal(tc.run)
			if tc.wantReason == "" {
				if got != nil {
					t.Fatalf("refusal = %+v, want nil", got)
				}
				return
			}
			if got == nil {
				t.Fatalf("refusal = nil, want reason %s", tc.wantReason)
			}
			if got.Reason != tc.wantReason {
				t.Errorf("reason = %q, want %q", got.Reason, tc.wantReason)
			}
			switch tc.wantReason {
			case MergeMethodRefusalForgeUnsupported:
				if got.Method != "rebase" || got.Source != MergeMethodSourceDeclared || strings.Join(got.Allowed, ",") != "squash,merge" {
					t.Errorf("refusal = %+v", got)
				}
				if !errors.Is(got, forge.ErrMergeMethodUnsupported) {
					t.Error("forge_unsupported refusal must match ErrMergeMethodUnsupported")
				}
			case MergeMethodRefusalUndeterminable:
				if !errors.Is(got, ErrMergeMethodUndeterminable) {
					t.Error("undeterminable refusal must match ErrMergeMethodUndeterminable")
				}
			}
		})
	}
}

// TestObservationForgeIDMatchesFamilyOfInstallationRef: the server's routing
// ladder and the forge package's twin (used by the orchestrator) agree.
func TestObservationForgeIDMatchesFamilyOfInstallationRef(t *testing.T) {
	ref := func(s string) *string { return &s }
	for _, r := range []*string{nil, ref(""), ref("12345"), ref("gitlab:7"), ref("github:9"), ref(":7"), ref("gitlab:"), ref("not a ref")} {
		if got, want := forge.FamilyOfInstallationRef(r), observationForgeID(r); got != want {
			label := "<nil>"
			if r != nil {
				label = *r
			}
			t.Errorf("ref %q: FamilyOfInstallationRef = %q, observationForgeID = %q", label, got, want)
		}
	}
}

// TestClassifyMergeDispatchOutcome_ReleaseSetUnchangedByMergeMethod pins that
// E93.2 / #4658 added NOTHING to classifyMergeDispatchOutcome's release set
// (moved here from merge_queue_pump_test.go per the approval): a bare
// ErrMergeMethodUnsupported is uncertain, and a MergeMethodRefusalError
// classifies exactly as its Cause does.
func TestClassifyMergeDispatchOutcome_ReleaseSetUnchangedByMergeMethod(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want mergeDispatchLedgerOutcome
	}{
		{"bare ErrMergeMethodUnsupported", forge.ErrMergeMethodUnsupported, mergeDispatchUncertain},
		{"adapter rebase refusal", fmt.Errorf("%w: %w: x", forge.ErrMergeMethodUnsupported, forge.ErrUnsupported), mergeDispatchUncertain},
		{"bare ErrMergeMethodNotAllowed", forge.ErrMergeMethodNotAllowed, mergeDispatchUncertain},
		{"refusal over REST 405", &MergeMethodRefusalError{Method: "merge", Reason: MergeMethodRefusalForgeDisallows, Cause: fmmNotMergeable()}, mergeDispatchRelease},
		{"refusal over GraphQL validation", &MergeMethodRefusalError{Method: "merge", Reason: MergeMethodRefusalForgeDisallows, Cause: fmt.Errorf("%w: graphql: x", forge.ErrValidation)}, mergeDispatchUncertain},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyMergeDispatchOutcome(tc.err); got != tc.want {
				t.Errorf("classifyMergeDispatchOutcome = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestResolveRunMergeMethod_ExistingMergePathFixtures confirms every existing
// merge-path fixture spec resolves to the squash default with no error (plan
// constraint 5c), so no existing merge test changes behaviour.
func TestResolveRunMergeMethod_ExistingMergePathFixtures(t *testing.T) {
	for name, doc := range map[string]string{
		"autoDriveAcceptanceSpecYAML": autoDriveAcceptanceSpecYAML,
		"mqeSpecYAML":                 mqeSpecYAML,
		"faeGateSpecYAML":             faeGateSpecYAML,
		"admqSpecYAML":                admqSpecYAML,
		"mcvDelegatedV2SpecYAML":      mcvDelegatedV2SpecYAML,
	} {
		mm, err := ResolveRunMergeMethod(&run.Run{ID: uuid.New(), WorkflowSpec: []byte(doc)})
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if mm.Method != forge.MergeMethodSquash || mm.Source != MergeMethodSourceDefault {
			t.Errorf("%s: got %+v, want squash/default", name, mm)
		}
	}
}
