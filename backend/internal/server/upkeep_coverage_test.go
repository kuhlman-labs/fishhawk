package server

// Tests for the upkeep-report Dependabot coverage adapter (#3750). Tests that
// swap the upkeepListOpenPulls / upkeepCoverageBudget package vars do not call
// t.Parallel, and restore the var in t.Cleanup.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/timescale"
	"github.com/kuhlman-labs/fishhawk/backend/internal/upkeep"
)

// The shipped advisory example's three findings.
const (
	ukAdvNet  = "advisory:GO-2024-2687:golang.org/x/net"
	ukAdvText = "advisory:GO-2022-1059:golang.org/x/text"
	ukAdvGHSA = "advisory:GHSA-q8v2-3m4c-7x9p:yaml-front-parser"
)

// upkeepAdvisoryExampleBody is the shipped advisory example: three advisory
// findings (one called with a caller frame, one imported, one npm with no
// fix) and a deprecation source degrade.
func upkeepAdvisoryExampleBody(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("../../../docs/spec/examples/upkeep-report-v1-advisory-example.json")
	if err != nil {
		t.Fatalf("read upkeep advisory example: %v", err)
	}
	return b
}

func upkeepAdvisoryExampleReport(t *testing.T) *plan.UpkeepReport {
	t.Helper()
	r, err := plan.ParseUpkeepReport(upkeepAdvisoryExampleBody(t))
	if err != nil {
		t.Fatalf("advisory example does not parse: %v", err)
	}
	return r
}

// swapUpkeepListOpenPulls installs fn as the listing seam for the test.
func swapUpkeepListOpenPulls(t *testing.T, fn func(context.Context, *Server, *run.Run, forge.RepoRef, int) ([]githubclient.OpenPullRequest, bool, error)) {
	t.Helper()
	prev := upkeepListOpenPulls
	upkeepListOpenPulls = fn
	t.Cleanup(func() { upkeepListOpenPulls = prev })
}

// ukPullsSeam returns a counting seam serving pulls.
func ukPullsSeam(calls *atomic.Int32, pulls ...githubclient.OpenPullRequest) func(context.Context, *Server, *run.Run, forge.RepoRef, int) ([]githubclient.OpenPullRequest, bool, error) {
	return func(context.Context, *Server, *run.Run, forge.RepoRef, int) ([]githubclient.OpenPullRequest, bool, error) {
		calls.Add(1)
		return append([]githubclient.OpenPullRequest(nil), pulls...), false, nil
	}
}

// ukDependabotPull is one dependabot[bot] go_modules pull request bumping pkg
// from -> to in dir (a Dependabot `/<dir>`), in this repo's real title shape,
// targeting the default branch.
func ukDependabotPull(n int, pkg, from, to, dir string) githubclient.OpenPullRequest {
	return githubclient.OpenPullRequest{
		Number:        n,
		HTMLURL:       fmt.Sprintf("https://github.com/kuhlman-labs/fishhawk/pull/%d", n),
		Title:         fmt.Sprintf("deps(backend)(deps): bump %s from %s to %s in %s", pkg, from, to, dir),
		UserLogin:     upkeep.DependabotAuthor,
		HeadRef:       "dependabot/go_modules/" + strings.TrimPrefix(dir, "/") + "/" + pkg + "-" + to,
		BaseRef:       "main",
		DefaultBranch: "main",
	}
}

func ukCovRun() *run.Run {
	inst := int64(4242)
	return &run.Run{ID: uuid.New(), Repo: "kuhlman-labs/fishhawk", InstallationID: &inst}
}

func ukCovServer(gh *githubclient.Client) *Server {
	return New(Config{Addr: "127.0.0.1:0", GitHub: gh, Logger: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))})
}

// ukGitHubClient is a real client against srv, the crew_work_request_test
// shape.
func ukGitHubClient(srv *httptest.Server) *githubclient.Client {
	return &githubclient.Client{BaseURL: srv.URL, Tokens: &fakeTokenProvider{tok: "t"},
		HTTP: &http.Client{Timeout: 5 * time.Second}, AppJWT: func() (string, error) { return "jwt", nil }}
}

// assertUkCoverageDegraded asserts a degraded result naming reason with a
// non-nil EMPTY covered set that serializes as a JSON array.
func assertUkCoverageDegraded(t *testing.T, res upkeepCoverageResult, reason string) {
	t.Helper()
	if !res.Degraded || res.DegradeReason != reason {
		t.Fatalf("result = %+v, want degraded %q", res, reason)
	}
	if res.Covered == nil || len(res.Covered) != 0 {
		t.Fatalf("covered = %#v, want a non-nil empty slice", res.Covered)
	}
	if b, _ := json.Marshal(res.Covered); string(b) != "[]" {
		t.Errorf("covered marshals as %s, want []", b)
	}
}

// TestUpkeepCoverage_NamedDegrades: one behavioural arm PER named degrade.
// Every arm uses the shipped advisory example, which carries two coverable
// findings (non-null fixed_version), so the early return cannot mask a
// degrade.
func TestUpkeepCoverage_NamedDegrades(t *testing.T) {
	report := upkeepAdvisoryExampleReport(t)

	t.Run("forge_unsupported", func(t *testing.T) {
		var calls atomic.Int32
		swapUpkeepListOpenPulls(t, ukPullsSeam(&calls, ukDependabotPull(1, "golang.org/x/net", "0.22.0", "0.23.0", "/backend")))
		rn := ukCovRun()
		ref := "gitlab:5"
		rn.InstallationRef = &ref
		res := ukCovServer(nil).upkeepCoverage(context.Background(), rn, report)
		assertUkCoverageDegraded(t, res, upkeepCoverageForgeUnsupported)
		if n := calls.Load(); n != 0 {
			t.Errorf("listing calls = %d on a GitLab run, want 0", n)
		}
	})

	t.Run("github_unwired", func(t *testing.T) {
		// The PRODUCTION seam with no GitHub client.
		res := ukCovServer(nil).upkeepCoverage(context.Background(), ukCovRun(), report)
		assertUkCoverageDegraded(t, res, upkeepCoverageGitHubUnwired)
	})

	t.Run("repo_malformed", func(t *testing.T) {
		var calls atomic.Int32
		swapUpkeepListOpenPulls(t, ukPullsSeam(&calls))
		rn := ukCovRun()
		rn.Repo = "no-slash"
		res := ukCovServer(nil).upkeepCoverage(context.Background(), rn, report)
		assertUkCoverageDegraded(t, res, upkeepCoverageRepoMalformed)
		if n := calls.Load(); n != 0 {
			t.Errorf("listing calls = %d on a malformed repo, want 0", n)
		}
	})

	for name, status := range map[string]int{"lookup_error": http.StatusInternalServerError, "not_installed": http.StatusNotFound} {
		t.Run("scope_unavailable/"+name, func(t *testing.T) {
			// The PRODUCTION seam: a run with no installation id resolves the
			// repository's installation, which fails (or reports none).
			var pullsHit atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/pulls") {
					pullsHit.Add(1)
				}
				w.WriteHeader(status)
			}))
			defer srv.Close()
			rn := ukCovRun()
			rn.InstallationID = nil
			res := ukCovServer(ukGitHubClient(srv)).upkeepCoverage(context.Background(), rn, report)
			assertUkCoverageDegraded(t, res, upkeepCoverageScopeUnavailable)
			if n := pullsHit.Load(); n != 0 {
				t.Errorf("pulls requests = %d without a scope, want 0", n)
			}
		})
	}

	t.Run("pull_list_failed", func(t *testing.T) {
		swapUpkeepListOpenPulls(t, func(context.Context, *Server, *run.Run, forge.RepoRef, int) ([]githubclient.OpenPullRequest, bool, error) {
			return nil, false, errors.New("github said no")
		})
		res := ukCovServer(nil).upkeepCoverage(context.Background(), ukCovRun(), report)
		assertUkCoverageDegraded(t, res, upkeepCoveragePullListFailed)
	})

	t.Run("budget_exceeded", func(t *testing.T) {
		budget := timescale.D(30 * time.Millisecond)
		prev := upkeepCoverageBudget
		upkeepCoverageBudget = budget
		t.Cleanup(func() { upkeepCoverageBudget = prev })
		// The seam honours ctx; a fallback timer far past the budget returns a
		// plain error, so with the budget's WithTimeout deleted the reason
		// reads pull_list_failed instead.
		swapUpkeepListOpenPulls(t, func(ctx context.Context, _ *Server, _ *run.Run, _ forge.RepoRef, _ int) ([]githubclient.OpenPullRequest, bool, error) {
			select {
			case <-ctx.Done():
				return nil, false, ctx.Err()
			case <-time.After(timescale.D(2 * time.Second)):
				return nil, false, errors.New("fallback: the coverage budget never fired")
			}
		})
		res := ukCovServer(nil).upkeepCoverage(context.Background(), ukCovRun(), report)
		assertUkCoverageDegraded(t, res, upkeepCoverageBudgetExceeded)
	})

	t.Run("coverage_panic", func(t *testing.T) {
		swapUpkeepListOpenPulls(t, func(context.Context, *Server, *run.Run, forge.RepoRef, int) ([]githubclient.OpenPullRequest, bool, error) {
			panic("listing exploded")
		})
		res := ukCovServer(nil).upkeepCoverage(context.Background(), ukCovRun(), report)
		assertUkCoverageDegraded(t, res, upkeepCoveragePanic)
	})
}

// TestUpkeepCoverage_NoCoverableFindingNoForgeRead: a GitHub run WITH an
// installation id and a counting seam that would answer with a valid empty
// list. A report with no advisory finding, and a report whose only advisory
// finding has no fix, are healthy empty results with NO listing call. The
// early return is the only thing preventing the call: delete it and the seam
// counts 1.
func TestUpkeepCoverage_NoCoverableFindingNoForgeRead(t *testing.T) {
	noFix := upkeepAdvisoryExampleReport(t)
	var only []plan.UpkeepFinding
	for _, f := range noFix.Findings {
		if f.ID == ukAdvGHSA {
			only = append(only, f)
		}
	}
	noFix.Findings = only
	shipped, err := plan.ParseUpkeepReport(upkeepExampleBody(t))
	if err != nil {
		t.Fatal(err)
	}
	for name, report := range map[string]*plan.UpkeepReport{"no_advisory": shipped, "advisory_without_fix": noFix} {
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int32
			swapUpkeepListOpenPulls(t, ukPullsSeam(&calls))
			res := ukCovServer(nil).upkeepCoverage(context.Background(), ukCovRun(), report)
			if res.Degraded || res.Covered == nil || len(res.Covered) != 0 {
				t.Fatalf("result = %+v, want healthy and empty", res)
			}
			if n := calls.Load(); n != 0 {
				t.Errorf("listing calls = %d, want 0 (nothing coverable)", n)
			}
		})
	}
}

// TestUpkeepCoverage_RequiresEveryManifest (approval condition 1 of the prior
// run, crossing plan.UpkeepAdvisoryManifestDirs -> adapter ->
// upkeep.MarkCovered): a finding citing backend/go.mod, runner/go.mod and a
// call-site source file is NOT covered by a /backend pull request alone, and
// IS covered once a /runner one is open. Adapting the directories from every
// file ref instead of the manifests adds backend/internal/server, which no
// Dependabot pull request can name, so the two-pull arm stays uncovered.
func TestUpkeepCoverage_RequiresEveryManifest(t *testing.T) {
	fixed := "v0.23.0"
	line := 1
	f := plan.UpkeepFinding{
		ID: ukAdvNet, Source: plan.UpkeepSourceAdvisory, Subject: "GO-2024-2687:golang.org/x/net",
		Evidence: []plan.UpkeepEvidenceRef{
			{Kind: plan.UpkeepEvidenceKindFile, Path: "backend/go.mod", Line: &line},
			{Kind: plan.UpkeepEvidenceKindFile, Path: "runner/go.mod", Line: &line},
			{Kind: plan.UpkeepEvidenceKindFile, Path: "backend/internal/server/serve.go", Line: &line},
		},
		Advisory: &plan.UpkeepAdvisory{Ecosystem: "go", Package: "golang.org/x/net", Version: "v0.22.0",
			AdvisoryIDs: []string{"GO-2024-2687"}, FixedVersion: &fixed, Scanner: "govulncheck",
			Reachability: "called", Severity: "high"},
	}
	report := &plan.UpkeepReport{Findings: []plan.UpkeepFinding{f}}
	backendPull := ukDependabotPull(11, "golang.org/x/net", "0.22.0", "0.23.0", "/backend")
	runnerPull := ukDependabotPull(12, "golang.org/x/net", "0.22.0", "0.24.1", "/runner")

	t.Run("one_directory_not_covered", func(t *testing.T) {
		var calls atomic.Int32
		swapUpkeepListOpenPulls(t, ukPullsSeam(&calls, backendPull))
		res := ukCovServer(nil).upkeepCoverage(context.Background(), ukCovRun(), report)
		if res.Degraded || len(res.Covered) != 0 || res.ScannedPulls != 1 {
			t.Fatalf("result = %+v, want healthy, scanned 1, nothing covered", res)
		}
	})
	t.Run("every_directory_covered", func(t *testing.T) {
		var calls atomic.Int32
		swapUpkeepListOpenPulls(t, ukPullsSeam(&calls, backendPull, runnerPull))
		res := ukCovServer(nil).upkeepCoverage(context.Background(), ukCovRun(), report)
		if res.Degraded || len(res.Covered) != 1 {
			t.Fatalf("result = %+v, want one covered finding", res)
		}
		c := res.Covered[0]
		if c.FindingID != ukAdvNet || len(c.Pulls) != 2 || c.Pulls[0].Number != 11 || c.Pulls[1].Number != 12 ||
			c.Pulls[0].Directory != "backend" || c.Pulls[1].Directory != "runner" {
			t.Errorf("covered = %+v, want %s by #11 (backend) and #12 (runner)", c, ukAdvNet)
		}
		if c.Pulls[0].URL != backendPull.HTMLURL {
			t.Errorf("covering url = %q, want %q", c.Pulls[0].URL, backendPull.HTMLURL)
		}
	})
}

// TestUpkeepCoverage_ProductionListing drives the PRODUCTION seam against a
// fake GitHub: with the run's installation id, and with the installation
// resolved through the App. It pins the field mapping (user.login, head.ref,
// base.ref, base.repo.default_branch) and the truncation pass-through.
func TestUpkeepCoverage_ProductionListing(t *testing.T) {
	report := upkeepAdvisoryExampleReport(t)
	pull := map[string]any{
		"number": 41, "html_url": "https://github.com/kuhlman-labs/fishhawk/pull/41",
		"title": "deps(backend)(deps): bump golang.org/x/net from 0.22.0 to 0.23.0 in /backend",
		"body":  nil, "user": map[string]any{"login": upkeep.DependabotAuthor},
		"head": map[string]any{"ref": "dependabot/go_modules/backend/golang.org/x/net-0.23.0"},
		"base": map[string]any{"ref": "main", "repo": map[string]any{"default_branch": "main"}},
	}
	for _, withInstall := range []bool{true, false} {
		t.Run(fmt.Sprintf("installation_id=%v", withInstall), func(t *testing.T) {
			var installHits atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/installation"):
					installHits.Add(1)
					_, _ = w.Write([]byte(`{"id":4242}`))
				case strings.HasSuffix(r.URL.Path, "/repos/kuhlman-labs/fishhawk/pulls"):
					if r.URL.Query().Get("state") != "open" {
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					_ = json.NewEncoder(w).Encode([]any{pull})
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer srv.Close()
			rn := ukCovRun()
			if !withInstall {
				rn.InstallationID = nil
			}
			res := ukCovServer(ukGitHubClient(srv)).upkeepCoverage(context.Background(), rn, report)
			if res.Degraded || res.ScannedPulls != 1 || res.WindowTruncated {
				t.Fatalf("result = %+v, want healthy, scanned 1, not truncated", res)
			}
			if len(res.Covered) != 1 || res.Covered[0].FindingID != ukAdvNet || res.Covered[0].Pulls[0].Number != 41 {
				t.Errorf("covered = %+v, want %s by #41", res.Covered, ukAdvNet)
			}
			if want := int32(map[bool]int{true: 0, false: 1}[withInstall]); installHits.Load() != want {
				t.Errorf("installation lookups = %d, want %d", installHits.Load(), want)
			}
		})
	}

	t.Run("truncated_window_passes_through", func(t *testing.T) {
		var gotMax int
		swapUpkeepListOpenPulls(t, func(_ context.Context, _ *Server, _ *run.Run, _ forge.RepoRef, maxPulls int) ([]githubclient.OpenPullRequest, bool, error) {
			gotMax = maxPulls
			return []githubclient.OpenPullRequest{{Number: 1}, {Number: 2}}, true, nil
		})
		res := ukCovServer(nil).upkeepCoverage(context.Background(), ukCovRun(), report)
		if res.Degraded || res.ScannedPulls != 2 || !res.WindowTruncated || len(res.Covered) != 0 {
			t.Errorf("result = %+v, want healthy, scanned 2, truncated, nothing covered", res)
		}
		if gotMax != upkeepCoverageMaxPulls {
			t.Errorf("maxPulls = %d, want %d", gotMax, upkeepCoverageMaxPulls)
		}
	})
}
