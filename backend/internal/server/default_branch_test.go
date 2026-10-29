package server

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/defaultbranch"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// repoGetterFake is a counting defaultbranch.Getter for the server's
// default-branch tests: it answers branch (or err) and records every repo it
// was asked about. Wrap it in the PRODUCTION defaultbranch.New resolver so a
// test exercises the real classification and cache (ADR-110 3(a)).
type repoGetterFake struct {
	branch string
	err    error

	mu    sync.Mutex
	repos []forge.RepoRef
}

func (g *repoGetterFake) GetRepository(_ context.Context, _ forge.CredentialScope, repo forge.RepoRef) (*forge.Repository, error) {
	g.mu.Lock()
	g.repos = append(g.repos, repo)
	g.mu.Unlock()
	if g.err != nil {
		return nil, g.err
	}
	return &forge.Repository{DefaultBranch: g.branch}, nil
}

func (g *repoGetterFake) calls() []forge.RepoRef {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]forge.RepoRef(nil), g.repos...)
}

func TestResolveRunDefaultBranch(t *testing.T) {
	inst := int64(55)
	gitlabRef := "gitlab:77"
	cases := []struct {
		name       string
		repo       string
		inst       *int64
		instRef    *string
		runnerKind string
		source     func(g *repoGetterFake) defaultbranch.Source
		getterErr  error
		wantBranch string
		wantReason defaultbranch.Reason
		wantReads  int
	}{
		{
			name: "forge default branch", repo: "acme/widgets", inst: &inst,
			source:     func(g *repoGetterFake) defaultbranch.Source { return defaultbranch.New(g) },
			wantBranch: "trunk", wantReads: 1,
		},
		{
			name: "unparseable repo", repo: "widgets", inst: &inst,
			source:     func(g *repoGetterFake) defaultbranch.Source { return defaultbranch.New(g) },
			wantReason: defaultbranch.ReasonRepositoryInvalid,
		},
		{
			name: "gitlab run", repo: "acme/widgets", instRef: &gitlabRef,
			source:     func(g *repoGetterFake) defaultbranch.Source { return defaultbranch.New(g) },
			wantReason: defaultbranch.ReasonForgeUnsupported,
		},
		{
			name: "gitlab_ci runner kind", repo: "acme/widgets", inst: &inst, runnerKind: run.RunnerKindGitLabCI,
			source:     func(g *repoGetterFake) defaultbranch.Source { return defaultbranch.New(g) },
			wantReason: defaultbranch.ReasonForgeUnsupported,
		},
		{
			name: "no installation", repo: "acme/widgets",
			source:     func(g *repoGetterFake) defaultbranch.Source { return defaultbranch.New(g) },
			wantReason: defaultbranch.ReasonNoInstallation,
		},
		{
			name: "nil resolver", repo: "acme/widgets", inst: &inst,
			source:     func(*repoGetterFake) defaultbranch.Source { return nil },
			wantReason: defaultbranch.ReasonResolverUnconfigured,
		},
		{
			name: "forge not found", repo: "acme/widgets", inst: &inst, getterErr: forge.ErrNotFound,
			source:     func(g *repoGetterFake) defaultbranch.Source { return defaultbranch.New(g) },
			wantReason: defaultbranch.ReasonRepositoryNotVisible, wantReads: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := &repoGetterFake{branch: "trunk", err: tc.getterErr}
			s := &Server{cfg: Config{DefaultBranch: tc.source(g)}}
			r := &run.Run{Repo: tc.repo, InstallationID: tc.inst, InstallationRef: tc.instRef}
			r.RunnerKind = tc.runnerKind
			branch, err := s.resolveRunDefaultBranch(context.Background(), r)
			if tc.wantReason != "" {
				reason, ok := defaultbranch.ReasonOf(err)
				if !ok || reason != tc.wantReason {
					t.Fatalf("err = %v, want reason %s", err, tc.wantReason)
				}
				if branch != "" {
					t.Errorf("branch = %q, want empty on a refusal", branch)
				}
			} else if err != nil || branch != tc.wantBranch {
				t.Fatalf("resolveRunDefaultBranch = (%q, %v), want (%q, nil)", branch, err, tc.wantBranch)
			}
			if got := len(g.calls()); got != tc.wantReads {
				t.Errorf("forge reads = %d, want %d", got, tc.wantReads)
			}
			if tc.wantReads > 0 {
				if c := g.calls()[0]; c.Owner != "acme" || c.Name != "widgets" {
					t.Errorf("forge read repo = %s, want acme/widgets", c)
				}
			}
		})
	}
}

// TestResolveRunDefaultBranch_UnresolvedErrorNamesRepo: the refusal is the
// typed error, carrying the cause for errors.Is.
func TestResolveRunDefaultBranch_UnresolvedErrorNamesRepo(t *testing.T) {
	inst := int64(55)
	s := &Server{cfg: Config{DefaultBranch: defaultbranch.New(&repoGetterFake{err: forge.ErrForbidden})}}
	_, err := s.resolveRunDefaultBranch(context.Background(), &run.Run{Repo: "acme/widgets", InstallationID: &inst})
	var ue *defaultbranch.UnresolvedError
	if !errors.As(err, &ue) || ue.Reason != defaultbranch.ReasonForbidden || ue.Repo.String() != "acme/widgets" {
		t.Fatalf("err = %#v, want an UnresolvedError{acme/widgets, forbidden}", err)
	}
	if !errors.Is(err, forge.ErrForbidden) {
		t.Errorf("err = %v, want it to wrap forge.ErrForbidden", err)
	}
}
