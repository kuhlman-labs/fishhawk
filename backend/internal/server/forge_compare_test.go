package server

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// fakeCompareForge is a forge.Forge whose only implemented method is
// ComparePatch: it returns a canned result and records the scope/base/head it
// was called with. The rest embeds a nil forge.Forge, unreachable in these
// tests. It guards its bookkeeping with a mutex because the trace.go
// implement-review sites call ComparePatch on a detached background goroutine
// (#3226: a fake reachable from a concurrent product path must be race-clean, or
// a -race counterfactual reddens on the fake, not the control).
type fakeCompareForge struct {
	forge.Forge
	name   string
	result *forge.ComparePatchResult
	err    error

	mu    sync.Mutex
	calls int
	scope forge.CredentialScope
	base  string
	head  string
}

func (f *fakeCompareForge) Name() string { return f.name }

func (f *fakeCompareForge) ComparePatch(_ context.Context, scope forge.CredentialScope,
	_ forge.RepoRef, base, head string) (*forge.ComparePatchResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.scope = scope
	f.base = base
	f.head = head
	if f.err != nil {
		return nil, f.err
	}
	return f.result, nil
}

func (f *fakeCompareForge) snapshot() (calls int, scope forge.CredentialScope, base, head string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls, f.scope, f.base, f.head
}

// oneFileCompareResult is the canned diff the gitlab-family compare fakes return.
func oneFileCompareResult() *forge.ComparePatchResult {
	return &forge.ComparePatchResult{
		HeadSHA: "headsha1",
		Patch:   "@@ -1 +1 @@\n-a\n+b",
		Files:   []forge.ComparePatchFile{{Path: "x.go", Status: "modified"}},
	}
}

// gitlabRunRow builds a minimal gitlab-family run row for the forge-resolved
// tests: InstallationRef "gitlab:5", nil InstallationID, the given repo.
func gitlabRunRow(repo string) *run.Run {
	ref := "gitlab:5"
	return &run.Run{ID: uuid.New(), Repo: repo, InstallationRef: &ref}
}

func TestForgeCompareFor_Rungs(t *testing.T) {
	fatalResolver := func(string) (forge.Forge, error) {
		t.Fatal("github-family run must NOT consult the forge resolver")
		return nil, nil
	}
	gh := cannedComparePatchClient(t, cannedCompareOneFile)

	githubRun := func(repo string, id *int64) *run.Run {
		return &run.Run{ID: uuid.New(), Repo: repo, InstallationID: id}
	}
	instID := int64(55)

	t.Run("github never consults resolver", func(t *testing.T) {
		s := New(Config{GitHub: gh, ForgeResolver: fatalResolver})
		c, scope, repo, reason := s.forgeCompareFor(githubRun("acme/widgets", &instID))
		if reason != "" {
			t.Fatalf("reason = %q, want empty", reason)
		}
		got, ok := c.(*githubclient.Client)
		if !ok || got != gh {
			t.Fatalf("comparer = %T, want cfg.GitHub", c)
		}
		if scope.Ref() != forge.FromGitHubInstallationID(instID).Ref() {
			t.Errorf("scope = %q, want github installation scope", scope.Ref())
		}
		if repo.Owner != "acme" || repo.Name != "widgets" {
			t.Errorf("repo = %+v, want acme/widgets", repo)
		}
	})

	t.Run("github nil client", func(t *testing.T) {
		s := New(Config{})
		if _, _, _, reason := s.forgeCompareFor(githubRun("acme/widgets", &instID)); reason != "github client not wired" {
			t.Fatalf("reason = %q, want 'github client not wired'", reason)
		}
	})

	t.Run("github nil installation id", func(t *testing.T) {
		s := New(Config{GitHub: gh})
		if _, _, _, reason := s.forgeCompareFor(githubRun("acme/widgets", nil)); reason != "no installation id" {
			t.Fatalf("reason = %q, want 'no installation id'", reason)
		}
	})

	t.Run("github zero installation id", func(t *testing.T) {
		s := New(Config{GitHub: gh})
		zero := int64(0)
		if _, _, _, reason := s.forgeCompareFor(githubRun("acme/widgets", &zero)); reason != "no installation id" {
			t.Fatalf("reason = %q, want 'no installation id'", reason)
		}
	})

	t.Run("github bad repo", func(t *testing.T) {
		s := New(Config{GitHub: gh})
		_, _, _, reason := s.forgeCompareFor(githubRun("noslash", &instID))
		if reason == "" || reason[:11] != "parse repo:" {
			t.Fatalf("reason = %q, want a 'parse repo:' reason", reason)
		}
	})

	t.Run("gitlab resolver error", func(t *testing.T) {
		s := New(Config{ForgeResolver: func(string) (forge.Forge, error) {
			return nil, errors.New("boom")
		}})
		if _, _, _, reason := s.forgeCompareFor(gitlabRunRow("acme/widgets")); reason != "forge gitlab unresolved" {
			t.Fatalf("reason = %q, want 'forge gitlab unresolved'", reason)
		}
	})

	t.Run("gitlab nil forge", func(t *testing.T) {
		s := New(Config{ForgeResolver: func(string) (forge.Forge, error) { return nil, nil }})
		if _, _, _, reason := s.forgeCompareFor(gitlabRunRow("acme/widgets")); reason != "forge gitlab unresolved" {
			t.Fatalf("reason = %q, want 'forge gitlab unresolved'", reason)
		}
	})

	t.Run("gitlab typed-nil forge", func(t *testing.T) {
		s := New(Config{ForgeResolver: func(string) (forge.Forge, error) {
			return (*fakeCompareForge)(nil), nil
		}})
		if _, _, _, reason := s.forgeCompareFor(gitlabRunRow("acme/widgets")); reason != "forge gitlab unresolved" {
			t.Fatalf("reason = %q, want 'forge gitlab unresolved' (isNilForge guard)", reason)
		}
	})

	t.Run("gitlab nested path ok", func(t *testing.T) {
		fake := &fakeCompareForge{name: "gitlab", result: oneFileCompareResult()}
		s := New(Config{ForgeResolver: func(string) (forge.Forge, error) { return fake, nil }})
		c, scope, repo, reason := s.forgeCompareFor(gitlabRunRow("group/sub/project"))
		if reason != "" {
			t.Fatalf("reason = %q, want empty for a nested gitlab path", reason)
		}
		if c != fake {
			t.Errorf("comparer = %T, want the resolved fake forge", c)
		}
		if scope.Ref() != "gitlab:5" {
			t.Errorf("scope = %q, want gitlab:5", scope.Ref())
		}
		if repo.Owner != "group/sub" || repo.Name != "project" {
			t.Errorf("repo = %+v, want group/sub + project (last-slash split)", repo)
		}
	})

	t.Run("gitlab nil resolver defaults to registry", func(t *testing.T) {
		// No ForgeResolver wired: forgeCompareFor falls back to forge.Get, which
		// resolves nothing for "gitlab" in the test process → unresolved.
		s := New(Config{})
		if _, _, _, reason := s.forgeCompareFor(gitlabRunRow("acme/widgets")); reason != "forge gitlab unresolved" {
			t.Fatalf("reason = %q, want 'forge gitlab unresolved' (nil resolver -> forge.Get)", reason)
		}
	})

	t.Run("gitlab unsplittable repo", func(t *testing.T) {
		fake := &fakeCompareForge{name: "gitlab", result: oneFileCompareResult()}
		s := New(Config{ForgeResolver: func(string) (forge.Forge, error) { return fake, nil }})
		if _, _, _, reason := s.forgeCompareFor(gitlabRunRow("noslash")); reason != "parse repo" {
			t.Fatalf("reason = %q, want 'parse repo' (splitParentRepoRef refuses)", reason)
		}
	})

	t.Run("unknown family gitea", func(t *testing.T) {
		s := New(Config{ForgeResolver: func(id string) (forge.Forge, error) {
			return nil, &forge.UnknownForgeError{ID: id}
		}})
		ref := "gitea:1"
		giteaRun := &run.Run{ID: uuid.New(), Repo: "acme/widgets", InstallationRef: &ref}
		if _, _, _, reason := s.forgeCompareFor(giteaRun); reason != "forge gitea unresolved" {
			t.Fatalf("reason = %q, want 'forge gitea unresolved'", reason)
		}
	})
}
