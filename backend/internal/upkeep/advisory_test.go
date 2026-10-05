package upkeep_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/upkeep"
)

func TestDependabotEcosystem(t *testing.T) {
	for ref, want := range map[string]string{
		"dependabot/go_modules/backend/github.com/aws/aws-sdk-go-v2-1.47.1": upkeep.EcosystemGo,
		"dependabot/npm_and_yarn/site/astro-65b6b13359":                     upkeep.EcosystemNPM,
		"dependabot/github_actions/actions/checkout-5":                      "",
		"feature/go_modules": "",
		"":                   "",
	} {
		if got := upkeep.DependabotEcosystem(ref); got != want {
			t.Errorf("DependabotEcosystem(%q) = %q, want %q", ref, got, want)
		}
	}
}

// TestDependabotBumps_RealShapes parses this repository's real Dependabot
// pull-request shapes (#3823, #3825, #3820) and the variants around them.
func TestDependabotBumps_RealShapes(t *testing.T) {
	for _, tc := range []struct {
		name  string
		title string
		body  string
		want  []upkeep.Bump
	}{
		{
			name:  "prefixed single bump (#3823)",
			title: "deps(backend)(deps): bump github.com/aws/aws-sdk-go-v2 from 1.47.0 to 1.47.1 in /backend",
			body:  "Bumps [github.com/aws/aws-sdk-go-v2](https://github.com/aws/aws-sdk-go-v2) from 1.47.0 to 1.47.1.\n",
			want:  []upkeep.Bump{{Package: "github.com/aws/aws-sdk-go-v2", From: "1.47.0", To: "1.47.1", Directory: "backend"}},
		},
		{
			name:  "unprefixed single bump at the root",
			title: "Bump golang.org/x/net from 0.22.0 to 0.23.0 in /",
			want:  []upkeep.Bump{{Package: "golang.org/x/net", From: "0.22.0", To: "0.23.0", Directory: "."}},
		},
		{
			name:  "single bump naming no directory",
			title: "Bump lodash from 4.17.20 to 4.17.21",
			want:  []upkeep.Bump{{Package: "lodash", From: "4.17.20", To: "4.17.21", Directory: ""}},
		},
		{
			name:  "security tag and nested directory",
			title: "[Security] Bump astro from 7.3.3 to 7.3.5 in /site/docs/",
			want:  []upkeep.Bump{{Package: "astro", From: "7.3.3", To: "7.3.5", Directory: "site/docs"}},
		},
		{
			name:  "group in one directory (#3825)",
			title: "deps(site)(deps): bump the astro group in /site with 2 updates",
			body: "Bumps the astro group in /site with 2 updates: [@astrojs/starlight](https://github.com/withastro/starlight/tree/HEAD/packages/starlight) and [astro](https://github.com/withastro/astro/tree/HEAD/packages/astro).\n\n" +
				"Updates `@astrojs/starlight` from 0.41.8 to 0.42.4\n<details>\n<summary>Release notes</summary>\n</details>\n\n" +
				"Updates `astro` from 7.3.3 to 7.3.5\n",
			want: []upkeep.Bump{
				{Package: "@astrojs/starlight", From: "0.41.8", To: "0.42.4", Directory: "site"},
				{Package: "astro", From: "7.3.3", To: "7.3.5", Directory: "site"},
			},
		},
		{
			name:  "group across one directory (#3820) names no single directory",
			title: "deps(frontend)(deps-dev): bump the vite group across 1 directory with 2 updates",
			body: "Bumps the vite group with 2 updates in the /frontend directory: [vite](https://github.com/vitejs/vite) and [vitest](https://github.com/vitest-dev/vitest).\n" +
				"Updates `vite` from 8.3.0 to 8.3.1\nUpdates `vitest` from 5.0.1 to 5.0.2\n",
			want: []upkeep.Bump{
				{Package: "vite", From: "8.3.0", To: "8.3.1", Directory: ""},
				{Package: "vitest", From: "5.0.1", To: "5.0.2", Directory: ""},
			},
		},
		{
			name:  "group across two directories",
			title: "bump the go-deps group across 2 directories with 3 updates",
			body:  "Updates `golang.org/x/net` from 0.22.0 to 0.23.0\n",
			want:  []upkeep.Bump{{Package: "golang.org/x/net", From: "0.22.0", To: "0.23.0", Directory: ""}},
		},
		{
			name:  "multi-dependency title reads the Bumps body line",
			title: "Bump vite and vitest in /frontend",
			body:  "Bumps [vite](https://github.com/vitejs/vite) from 8.3.0 to 8.3.1.\n",
			want:  []upkeep.Bump{{Package: "vite", From: "8.3.0", To: "8.3.1", Directory: "frontend"}},
		},
		{
			name:  "not a bump",
			title: "Update the README",
			body:  "Updates `astro` from 7.3.3 to 7.3.5\n",
			want:  []upkeep.Bump{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := upkeep.DependabotBumps(upkeep.PullRequest{Title: tc.title, Body: tc.body})
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("DependabotBumps = %#v, want %#v", got, tc.want)
			}
		})
	}
}

const (
	netFinding = "advisory:GO-2024-2687:golang.org/x/net"
	netURL     = "https://github.com/x/y/pull/3900"
)

func netAdvisory(dirs ...string) upkeep.AdvisoryProposal {
	return upkeep.AdvisoryProposal{
		FindingID:    netFinding,
		Ecosystem:    upkeep.EcosystemGo,
		Package:      "golang.org/x/net",
		FixedVersion: "v0.23.0",
		Directories:  dirs,
	}
}

// netPull is a Dependabot go_modules pull request bumping golang.org/x/net
// to `to` in /<dir>.
func netPull(number int, to, dir string) upkeep.PullRequest {
	return upkeep.PullRequest{
		Number:  number,
		URL:     netURL,
		Title:   "deps(" + dir + ")(deps): bump golang.org/x/net from 0.22.0 to " + to + " in /" + dir,
		Author:  upkeep.DependabotAuthor,
		HeadRef: "dependabot/go_modules/" + dir + "/golang.org/x/net-" + to,
	}
}

func assertNotCovered(t *testing.T, got []upkeep.Covered) {
	t.Helper()
	if got == nil || len(got) != 0 {
		t.Errorf("MarkCovered = %#v, want a non-nil empty slice (not covered)", got)
	}
}

func TestMarkCovered_Covers(t *testing.T) {
	got := upkeep.MarkCovered([]upkeep.AdvisoryProposal{netAdvisory("backend")}, []upkeep.PullRequest{netPull(3900, "0.23.0", "backend")})
	want := []upkeep.Covered{{
		FindingID: netFinding,
		Package:   "golang.org/x/net",
		Pulls:     []upkeep.CoveringPull{{Number: 3900, URL: netURL, Directory: "backend", BumpsTo: "0.23.0"}},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("MarkCovered = %#v, want %#v", got, want)
	}
}

// TestMarkCovered_RequiresDependabotAuthor: a PR identical to a covering one
// except its author does not cover.
//
// Counterfactual: every other field matches, so deleting the author check
// covers the finding — RED.
func TestMarkCovered_RequiresDependabotAuthor(t *testing.T) {
	pr := netPull(3900, "0.23.0", "backend")
	pr.Author = "octocat"
	assertNotCovered(t, upkeep.MarkCovered([]upkeep.AdvisoryProposal{netAdvisory("backend")}, []upkeep.PullRequest{pr}))
}

// TestMarkCovered_RequiresEveryDirectory: a finding citing backend and runner
// manifests is not covered by a /backend PR alone; adding a /runner PR covers
// it, recording one pull per directory in the finding's order.
//
// Counterfactual: replacing the every-directory rule with any-directory makes
// the one-PR arm cover — RED.
func TestMarkCovered_RequiresEveryDirectory(t *testing.T) {
	a := []upkeep.AdvisoryProposal{netAdvisory("backend", "runner")}
	assertNotCovered(t, upkeep.MarkCovered(a, []upkeep.PullRequest{netPull(3900, "0.23.0", "backend")}))

	got := upkeep.MarkCovered(a, []upkeep.PullRequest{netPull(3901, "0.24.1", "runner"), netPull(3900, "0.23.0", "backend")})
	if len(got) != 1 {
		t.Fatalf("MarkCovered = %#v, want one covered finding", got)
	}
	want := []upkeep.CoveringPull{
		{Number: 3900, URL: netURL, Directory: "backend", BumpsTo: "0.23.0"},
		{Number: 3901, URL: netURL, Directory: "runner", BumpsTo: "0.24.1"},
	}
	if !reflect.DeepEqual(got[0].Pulls, want) {
		t.Errorf("Pulls = %#v, want %#v", got[0].Pulls, want)
	}
}

// TestMarkCovered_RequiresFixedVersion: a bump below the fixed version does
// not cover; the fixed version itself and a later one do.
//
// Counterfactual: neutering VersionAtLeast to (true, true) makes the 0.22.1
// bump cover — RED.
func TestMarkCovered_RequiresFixedVersion(t *testing.T) {
	a := []upkeep.AdvisoryProposal{netAdvisory("backend")}
	assertNotCovered(t, upkeep.MarkCovered(a, []upkeep.PullRequest{netPull(3900, "0.22.1", "backend")}))
	for _, to := range []string{"0.23.0", "0.24.1"} {
		if got := upkeep.MarkCovered(a, []upkeep.PullRequest{netPull(3900, to, "backend")}); len(got) != 1 {
			t.Errorf("bump to %s: MarkCovered = %#v, want covered", to, got)
		}
	}
}

// TestMarkCovered_RequiresEcosystem: an npm head ref bumping a same-named
// package does not cover a go finding.
//
// Counterfactual: deleting the ecosystem comparison covers it — RED.
func TestMarkCovered_RequiresEcosystem(t *testing.T) {
	pr := netPull(3900, "0.23.0", "backend")
	pr.HeadRef = "dependabot/npm_and_yarn/backend/golang.org/x/net-0.23.0"
	assertNotCovered(t, upkeep.MarkCovered([]upkeep.AdvisoryProposal{netAdvisory("backend")}, []upkeep.PullRequest{pr}))
}

// TestMarkCovered_UnknownHeadRefNeverCovers: a Dependabot PR whose head ref
// names no recognized ecosystem never covers, even a finding that (by a
// caller bug) carries no ecosystem either.
//
// Counterfactual: the finding's empty ecosystem equals the unknown ref's
// empty ecosystem, so deleting the unknown-ref skip covers it — RED.
func TestMarkCovered_UnknownHeadRefNeverCovers(t *testing.T) {
	a := netAdvisory("backend")
	a.Ecosystem = ""
	pr := netPull(3900, "0.23.0", "backend")
	pr.HeadRef = "dependabot/github_actions/backend/net"
	assertNotCovered(t, upkeep.MarkCovered([]upkeep.AdvisoryProposal{a}, []upkeep.PullRequest{pr}))
}

// TestMarkCovered_RequiresSamePackage: a bump of another package in the same
// directory past the fixed version does not cover.
//
// Counterfactual: deleting the package comparison covers it — RED.
func TestMarkCovered_RequiresSamePackage(t *testing.T) {
	pr := netPull(3900, "0.23.0", "backend")
	pr.Title = "deps(backend)(deps): bump golang.org/x/crypto from 0.22.0 to 0.23.0 in /backend"
	assertNotCovered(t, upkeep.MarkCovered([]upkeep.AdvisoryProposal{netAdvisory("backend")}, []upkeep.PullRequest{pr}))
}

// TestMarkCovered_NoDirectoriesNeverCovers: a finding citing no manifest is
// never covered.
//
// Counterfactual: with zero directories the every-directory loop is vacuously
// satisfied, so deleting the at-least-one rule covers it — RED.
func TestMarkCovered_NoDirectoriesNeverCovers(t *testing.T) {
	assertNotCovered(t, upkeep.MarkCovered([]upkeep.AdvisoryProposal{netAdvisory()}, []upkeep.PullRequest{netPull(3900, "0.23.0", "backend")}))
}

// TestMarkCovered_EmptyDirectoryNeverMatchesUnknown: a "" directory on the
// finding (a caller bug) never matches a bump whose directory is unknown.
//
// Counterfactual: an across-directories group yields bumps with directory "",
// so deleting the empty-directory stop lets "" == "" cover it — RED.
func TestMarkCovered_EmptyDirectoryNeverMatchesUnknown(t *testing.T) {
	pr := upkeep.PullRequest{
		Number:  3902,
		Title:   "bump the go-deps group across 2 directories with 2 updates",
		Body:    "Updates `golang.org/x/net` from 0.22.0 to 0.23.0\n",
		Author:  upkeep.DependabotAuthor,
		HeadRef: "dependabot/go_modules/go-deps-abc",
	}
	assertNotCovered(t, upkeep.MarkCovered([]upkeep.AdvisoryProposal{netAdvisory("")}, []upkeep.PullRequest{pr}))
}

// TestMarkCovered_ConservativeArms: arms whose control is masked by another
// rule (declared, asserted behaviourally only): no fixed version (masked by
// VersionAtLeast's unparseable empty want) and an unknown-directory bump
// against a named directory (masked by the directory comparison).
func TestMarkCovered_ConservativeArms(t *testing.T) {
	noFix := netAdvisory("backend")
	noFix.FixedVersion = ""
	assertNotCovered(t, upkeep.MarkCovered([]upkeep.AdvisoryProposal{noFix}, []upkeep.PullRequest{netPull(3900, "0.23.0", "backend")}))

	across := upkeep.PullRequest{
		Number:  3902,
		Title:   "bump the go-deps group across 1 directory with 1 update",
		Body:    "Updates `golang.org/x/net` from 0.22.0 to 0.23.0\n",
		Author:  upkeep.DependabotAuthor,
		HeadRef: "dependabot/go_modules/go-deps-abc",
	}
	assertNotCovered(t, upkeep.MarkCovered([]upkeep.AdvisoryProposal{netAdvisory("backend")}, []upkeep.PullRequest{across}))

	assertNotCovered(t, upkeep.MarkCovered(nil, nil))
}

// TestMarkCovered_InputOrderAndFirstPull: output follows the advisory input
// order, and the first covering PR in input order is recorded.
func TestMarkCovered_InputOrderAndFirstPull(t *testing.T) {
	crypto := upkeep.AdvisoryProposal{
		FindingID: "advisory:GO-2025-0001:golang.org/x/crypto", Ecosystem: upkeep.EcosystemGo,
		Package: "golang.org/x/crypto", FixedVersion: "v0.31.0", Directories: []string{"backend"},
	}
	cryptoPR := upkeep.PullRequest{
		Number: 3910, Title: "bump golang.org/x/crypto from 0.30.0 to 0.31.0 in /backend",
		Author: upkeep.DependabotAuthor, HeadRef: "dependabot/go_modules/backend/golang.org/x/crypto-0.31.0",
	}
	got := upkeep.MarkCovered(
		[]upkeep.AdvisoryProposal{crypto, netAdvisory("backend")},
		[]upkeep.PullRequest{netPull(3905, "0.23.0", "backend"), cryptoPR, netPull(3904, "0.24.0", "backend")},
	)
	if len(got) != 2 || got[0].FindingID != crypto.FindingID || got[1].FindingID != netFinding {
		t.Fatalf("MarkCovered = %#v, want crypto then net", got)
	}
	if got[1].Pulls[0].Number != 3905 {
		t.Errorf("net covered by #%d, want #3905 (first in input order)", got[1].Pulls[0].Number)
	}
}

func TestVersionAtLeast(t *testing.T) {
	for _, tc := range []struct {
		have, want          string
		atLeast, comparable bool
	}{
		{"v0.23.0", "v0.23.0", true, true},
		{"0.23.0", "v0.23.0", true, true},
		{"0.22.0", "v0.23.0", false, true},
		{"0.24.1", "v0.23.0", true, true},
		{"1.10.0", "1.9.0", true, true},
		{"1.9.0", "1.10.0", false, true},
		{"2.0.0", "1.99.99", true, true},
		// A prerelease is below its release; a release covers its own
		// prerelease fix.
		{"1.2.3-rc.1", "1.2.3", false, true},
		{"1.2.3", "1.2.3-rc.1", true, true},
		// Numeric prerelease identifiers compare numerically (condition 2).
		{"1.2.3-beta.2", "1.2.3-beta.10", false, true},
		{"1.2.3-beta.10", "1.2.3-beta.2", true, true},
		// Numeric < alphanumeric; alphanumerics compare in ASCII order.
		{"1.0.0-1", "1.0.0-alpha", false, true},
		{"1.0.0-alpha", "1.0.0-1", true, true},
		{"1.0.0-beta", "1.0.0-alpha", true, true},
		// The shorter identifier set is lower.
		{"1.0.0-alpha", "1.0.0-alpha.1", false, true},
		{"1.0.0-alpha.1", "1.0.0-alpha", true, true},
		// Build metadata is ignored.
		{"1.2.3+build.5", "1.2.3", true, true},
		{"1.2.3", "1.2.3+zzz", true, true},
		// Go pseudo-versions are semver prereleases (condition 4), against
		// the release fix v0.23.0. Form 1: vX.0.0-yyyymmddhhmmss-hash.
		{"v0.0.0-20240101123456-abcdef123456", "v0.23.0", false, true},
		// Form 2: vX.Y.Z-pre.0.yyyymmddhhmmss-hash.
		{"v0.23.0-rc.1.0.20240101123456-abcdef123456", "v0.23.0", false, true},
		{"v0.24.0-pre.0.20240101123456-abcdef123456", "v0.23.0", true, true},
		// Form 3: vX.Y.(Z+1)-0.yyyymmddhhmmss-hash.
		{"v0.23.1-0.20240101123456-abcdef123456", "v0.23.0", true, true},
		{"v0.23.0-0.20240101123456-abcdef123456", "v0.23.0", false, true},
		// Two pseudo-versions order by timestamp.
		{"v0.0.0-20240202000000-aaaaaaaaaaaa", "v0.0.0-20240101000000-bbbbbbbbbbbb", true, true},
		// Not comparable: ranges, partial versions, invalid identifiers.
		{"", "v0.23.0", false, false},
		{"0.23.0", "", false, false},
		{"^1.2.3", "1.2.3", false, false},
		{"1.2.3", ">=1.2.3", false, false},
		{"1.2", "1.2.0", false, false},
		{"1.2.3.4", "1.2.3", false, false},
		{"1.2.x", "1.2.0", false, false},
		{"1.2.3 - 2.0.0", "1.2.3", false, false},
		{"01.2.3", "1.2.3", false, false},
		{"1.2.3-", "1.2.3", false, false},
		{"1.2.3-a..b", "1.2.3", false, false},
		{"1.2.3-beta.01", "1.2.3", false, false},
		{"1.2.3-be_ta", "1.2.3", false, false},
		{"99999999999999999999.0.0", "1.0.0", false, false},
		{"latest", "1.0.0", false, false},
	} {
		atLeast, comparable := upkeep.VersionAtLeast(tc.have, tc.want)
		if atLeast != tc.atLeast || comparable != tc.comparable {
			t.Errorf("VersionAtLeast(%q, %q) = (%v, %v), want (%v, %v)", tc.have, tc.want, atLeast, comparable, tc.atLeast, tc.comparable)
		}
	}
}

// TestVersionAtLeast_SemverSpecChain walks semver §11.4's example chain:
// each version is strictly above its predecessor.
func TestVersionAtLeast_SemverSpecChain(t *testing.T) {
	chain := []string{"1.0.0-alpha", "1.0.0-alpha.1", "1.0.0-alpha.beta", "1.0.0-beta", "1.0.0-beta.2", "1.0.0-beta.11", "1.0.0-rc.1", "1.0.0"}
	for i := 1; i < len(chain); i++ {
		lo, hi := chain[i-1], chain[i]
		if up, ok := upkeep.VersionAtLeast(hi, lo); !up || !ok {
			t.Errorf("VersionAtLeast(%q, %q) = (%v, %v), want (true, true)", hi, lo, up, ok)
		}
		if down, ok := upkeep.VersionAtLeast(lo, hi); down || !ok {
			t.Errorf("VersionAtLeast(%q, %q) = (%v, %v), want (false, true)", lo, hi, down, ok)
		}
	}
}

func goFacts() upkeep.AdvisoryFacts {
	return upkeep.AdvisoryFacts{
		IDs:          []string{"GO-2024-2687", "CVE-2023-45288", "GHSA-4v7x-pqxf-cx7m"},
		Ecosystem:    "go",
		Package:      "golang.org/x/net",
		Version:      "v0.22.0",
		FixedVersion: "v0.23.0",
		Reachability: "called",
		Severity:     "high",
		Manifests:    []string{"backend/go.mod", "runner/go.mod"},
	}
}

func TestRenderAdvisoryFacts_Golden(t *testing.T) {
	want := "### Advisory facts (server-rendered)\n\n" +
		"- Advisory IDs: `GO-2024-2687`, `CVE-2023-45288`, `GHSA-4v7x-pqxf-cx7m`\n" +
		"- Ecosystem: `go`\n" +
		"- Package: `golang.org/x/net`\n" +
		"- In-use version: `v0.22.0`\n" +
		"- Fixed version: `v0.23.0`\n" +
		"- Reachability: `called`\n" +
		"- Severity: `high`\n" +
		"- Manifests: `backend/go.mod`, `runner/go.mod`\n"
	if got := upkeep.RenderAdvisoryFacts(goFacts()); got != want {
		t.Errorf("RenderAdvisoryFacts =\n%s\nwant\n%s", got, want)
	}
}

func TestRenderAdvisoryFacts_NoFixAndEmptyLists(t *testing.T) {
	f := goFacts()
	f.FixedVersion = ""
	f.IDs = nil
	f.Manifests = nil
	got := upkeep.RenderAdvisoryFacts(f)
	for _, line := range []string{"- Fixed version: no fix published\n", "- Advisory IDs: none\n", "- Manifests: none cited\n"} {
		if !strings.Contains(got, line) {
			t.Errorf("RenderAdvisoryFacts missing %q:\n%s", line, got)
		}
	}
}

// TestRenderAdvisory_WithholdsNonTokenValues: a structured value carrying
// prose, markdown or a call-path quote is withheld from both the title and
// the facts block.
//
// Counterfactual: making the token check pass every value lets the prose
// through — RED.
func TestRenderAdvisory_WithholdsNonTokenValues(t *testing.T) {
	f := goFacts()
	f.Version = "v0.22.0 reached via internal/fetch/pull.go"
	f.Package = "golang.org/x/net`](https://evil.example)"
	f.Manifests = []string{"backend/go.mod see @octocat"}
	f.IDs = []string{"GO-2024-2687 in fetch.Pull"}
	title := upkeep.RenderAdvisoryTitle(f)
	body := upkeep.RenderAdvisoryFacts(f)
	for _, leaked := range []string{"internal/fetch/pull.go", "evil.example", "@octocat", "fetch.Pull"} {
		if strings.Contains(title, leaked) || strings.Contains(body, leaked) {
			t.Errorf("rendered filing leaks %q:\ntitle=%s\nbody=%s", leaked, title, body)
		}
	}
	if !strings.Contains(body, "- In-use version: (withheld: not a plain token)\n") {
		t.Errorf("body does not mark the withheld version:\n%s", body)
	}
}

func TestRenderAdvisoryTitle(t *testing.T) {
	if got, want := upkeep.RenderAdvisoryTitle(goFacts()), "GO-2024-2687: golang.org/x/net v0.22.0 (high severity)"; got != want {
		t.Errorf("RenderAdvisoryTitle = %q, want %q", got, want)
	}
	f := goFacts()
	f.IDs = nil
	if got := upkeep.RenderAdvisoryTitle(f); !strings.HasPrefix(got, "advisory: golang.org/x/net ") {
		t.Errorf("RenderAdvisoryTitle with no ids = %q, want an `advisory:` primary", got)
	}
}

// TestRenderAdvisoryTitle_CappedAtGitHubLimit: a maximal package and version
// still render a title within GitHub's 256-character limit.
//
// Counterfactual: the uncapped title is 309 characters, so deleting the cap
// goes RED.
func TestRenderAdvisoryTitle_CappedAtGitHubLimit(t *testing.T) {
	f := goFacts()
	f.Package = "a" + strings.Repeat("b", 213)
	f.Version = "v" + strings.Repeat("1", 63)
	got := upkeep.RenderAdvisoryTitle(f)
	if len(got) != 256 || !strings.HasPrefix(got, "GO-2024-2687: abbb") {
		t.Errorf("len(title) = %d (%q), want exactly 256 with the primary id kept", len(got), got)
	}
}

// disclosureFrames: frame 0 is the vulnerable dependency symbol; frames 1-2
// are the repository's own callers.
func disclosureFrames() []upkeep.DisclosureFrame {
	return []upkeep.DisclosureFrame{
		{Package: "golang.org/x/net/http2", Function: "ReadFrame", Receiver: "*Framer", Filename: "http2/frame.go"},
		{Package: "github.com/kuhlman-labs/fishhawk/backend/internal/fetch", Function: "Pull", Filename: "internal/fetch/pull.go"},
		{Package: "github.com/kuhlman-labs/fishhawk/backend/internal/server", Function: "ServeHTTP", Receiver: "*Server"},
	}
}

// TestCallPathDisclosure_CallerFilename: a body quoting only a caller's
// filename is found.
//
// Counterfactual: the body carries no qualified name, so dropping the
// filename token leaves it unfound — RED.
func TestCallPathDisclosure_CallerFilename(t *testing.T) {
	tok, found := upkeep.CallPathDisclosure("Reached from internal/fetch/pull.go in the fetcher.", disclosureFrames())
	if !found || tok != "internal/fetch/pull.go" {
		t.Errorf("CallPathDisclosure = (%q, %v), want the caller filename", tok, found)
	}
}

func TestCallPathDisclosure_QualifiedNames(t *testing.T) {
	frames := disclosureFrames()
	for body, want := range map[string]string{
		"via github.com/kuhlman-labs/fishhawk/backend/internal/fetch.Pull": "github.com/kuhlman-labs/fishhawk/backend/internal/fetch.Pull",
		// The receiver token trims the leading `*`.
		"called from Server.ServeHTTP": "Server.ServeHTTP",
	} {
		if tok, found := upkeep.CallPathDisclosure(body, frames); !found || tok != want {
			t.Errorf("CallPathDisclosure(%q) = (%q, %v), want %q", body, tok, found, want)
		}
	}
}

// TestCallPathDisclosure_FrameZeroIsPublic: frame 0's tokens (the vulnerable
// dependency symbol) are not a disclosure.
//
// Counterfactual: frame 0's qualified names are the only tokens in the body,
// so starting the scan at index 0 finds one — RED.
func TestCallPathDisclosure_FrameZeroIsPublic(t *testing.T) {
	body := "golang.org/x/net/http2.ReadFrame and Framer.ReadFrame in http2/frame.go are vulnerable."
	if tok, found := upkeep.CallPathDisclosure(body, disclosureFrames()); found {
		t.Errorf("CallPathDisclosure = (%q, true), want frame 0 ignored", tok)
	}
}

// TestCallPathDisclosure_PartialFramesYieldNoToken: a frame with an empty
// filename, a package without a function, or a bare `*` receiver contributes
// no token.
//
// Counterfactual: each guard isolates one arm — without the empty-token
// guard "" matches every body; without the function requirement "pkg." and
// ".Run" become tokens the body contains — RED.
func TestCallPathDisclosure_PartialFramesYieldNoToken(t *testing.T) {
	frames := []upkeep.DisclosureFrame{
		{Package: "dep", Function: "Vuln"},
		{Package: "pkg"},
		{Receiver: "*", Function: "Run"},
	}
	if tok, found := upkeep.CallPathDisclosure("see pkg.Other and x.Run", frames); found {
		t.Errorf("CallPathDisclosure = (%q, true), want no token from partial frames", tok)
	}
}

// TestRenderedAdvisoryFilingCarriesNoCallerFrame: a filing rendered from an
// advisory's structured fields carries none of its caller frames, whatever
// the agent's own title and body said (condition 1). The renderers take no
// call path and no prose, so the agent text has no way in.
func TestRenderedAdvisoryFilingCarriesNoCallerFrame(t *testing.T) {
	agentBody := "Reached via internal/fetch/pull.go: github.com/kuhlman-labs/fishhawk/backend/internal/fetch.Pull -> Server.ServeHTTP"
	frames := disclosureFrames()
	if _, found := upkeep.CallPathDisclosure(agentBody, frames); !found {
		t.Fatal("fixture: the agent body must quote a caller frame")
	}
	filed := upkeep.RenderAdvisoryTitle(goFacts()) + "\n" + upkeep.RenderAdvisoryFacts(goFacts())
	if tok, found := upkeep.CallPathDisclosure(filed, frames); found {
		t.Errorf("server-rendered filing quotes caller token %q:\n%s", tok, filed)
	}
}
