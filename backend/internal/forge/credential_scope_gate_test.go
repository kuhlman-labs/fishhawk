package forge

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// This file is the contract gate for the #1855 forge-credential split
// (phase 5/5, #2013). The staged migration replaced every cross-forge
// `installationID int64` seam with a forge.CredentialScope; this test
// keeps it replaced. It walks the committed Go source of all three
// modules and fails, naming file:line, when an installationID/
// InstallationID int64 DECLARATION appears outside the sanctioned
// survivor set below.
//
// The gate matches declarations (`InstallationID int64`, `installationID
// *int64`), not uses: `InstallationID: derefInt64(x)` passes an int64
// into an already-sanctioned struct field and is not itself a new
// cross-forge seam.

// scanModules are the Go modules registered in /go.work. A missing one
// means the workspace layout moved out from under repoRoot's parent
// hops — fail loudly rather than silently scanning nothing.
var scanModules = []string{"backend", "runner", "cli"}

// skipDirs are directory NAMES pruned anywhere in the walk.
//
// "db" mirrors the AGENTS.md coverage-gate convention for sqlc-generated
// packages: generated code models the installation_id column as a bare
// int64 and is not hand-written cross-forge surface. Skipping the
// */db/ DIRECTORY (rather than allowlisting each generated file) means a
// future sqlc package is auto-skipped with no edit here.
var skipDirs = map[string]bool{
	".git":         true,
	"node_modules": true,
	"frontend":     true,
	"testdata":     true,
	"db":           true,
}

// installationIDDecl matches an installationID/InstallationID declared
// with an int64 (or *int64) type: the identifier, whitespace, then the
// type. Case-insensitive so both the exported field and the unexported
// parameter spelling are caught.
var installationIDDecl = regexp.MustCompile(`(?i)\bInstallationID\b\s+\*?int64\b`)

// exportedClientInt64Method matches an EXPORTED *githubclient.Client
// method still taking a bare int64 installation id. sanctioned[] below
// allowlists the whole githubclient package for its unexported plumbing,
// so without this second, narrower assertion that broad entry could hide
// a reintroduced int64 method on the public surface — the exact
// regression this phase exists to prevent.
var exportedClientInt64Method = regexp.MustCompile(`func \(c \*Client\) [A-Z]\w*\(ctx context\.Context, installationID int64`)

// sanctioned maps a repo-relative path prefix to WHY that surface
// legitimately keeps a bare int64 installation id. Everything else is a
// gate failure. Keep the reasons — they are the contract, and a new
// entry should be hard to add without stating one.
var sanctioned = map[string]string{
	// The GitHub App token machinery is GitHub-by-definition: issuing and
	// caching an installation token has no forge-neutral shape.
	"backend/internal/githubapp/": "GitHub App TokenProvider internals",

	// githubclient's UNEXPORTED plumbing (buildRequest/doGraphQL/
	// fetchRulesetContexts) and the credentialTokens adapter resolve a
	// scope to an id exactly once at the exported boundary and pass the
	// int64 down. The exported surface is scope-taking and is pinned by
	// TestNoExportedGitHubClientMethodTakesInt64InstallationID.
	"backend/internal/githubclient/": "githubclient internal plumbing below the scope-taking boundary",

	// Webhook ingest decodes GitHub's wire payload verbatim; the id is a
	// GitHub-shaped input, converted to a scope at the consumer.
	"backend/internal/webhook/webhook.go":    "GitHub webhook ingest payload struct",
	"backend/internal/webhook/dispatcher.go": "GitHub webhook ingest payload struct",

	// Run persistence: installation_id is a nullable column on the run
	// row (*int64). ADR-057's installation_ref migration owns this
	// column's type; the Go mirrors follow it, not this phase.
	"backend/internal/run/run.go":        "run row persistence (installation_id column mirror)",
	"backend/internal/run/repository.go": "run row persistence (installation_id column mirror)",
	"backend/internal/server/runs.go":    "run row persistence (CreateRunParams InstallationID *int64)",

	// Onboarding reports what the GitHub installations API returned. The
	// three payload shapes must stay byte-identical across server, MCP,
	// and CLI, so they mirror the API's int64 verbatim.
	"backend/internal/server/onboarding.go": "onboarding payload mirror of the GitHub installations API",
	"backend/cmd/fishhawk-mcp/client.go":    "onboarding payload mirror of the GitHub installations API",
	"cli/cmd/fishhawk/doctor_onboarding.go": "onboarding payload mirror of the GitHub installations API",

	// DEFERRED, NOT SANCTIONED-FOREVER (#1861). runnerbackend.TriggerParams
	// .InstallationID is the one known remaining cross-forge int64 seam.
	// Migrating it here was adjudicated as deferred at the #2013 plan gate:
	// the gitlab_ci backend is the field's second consumer and is what
	// gives it its correct forge-neutral shape, so #1861 owns the flip
	// rather than this pass guessing it. Delete this entry when #1861
	// lands — the gate then holds the field to a scope.
	"backend/internal/runnerbackend/runnerbackend.go": "TriggerParams.InstallationID — deferred to #1861 (gitlab_ci backend shapes the flip)",
}

// repoRoot returns the workspace root by hopping three parents up from
// this file's directory (<root>/backend/internal/forge).
func repoRoot(t *testing.T) string {
	t.Helper()
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed: cannot locate the repo root")
	}
	root := filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(self))))
	for _, m := range scanModules {
		if _, err := os.Stat(filepath.Join(root, m)); err != nil {
			t.Fatalf("module %q not found under derived repo root %q: %v"+
				" (the workspace layout moved; fix repoRoot's parent hops"+
				" rather than letting the gate scan nothing)", m, root, err)
		}
	}
	return root
}

// walkGoSources calls fn(relPath, content) for every non-test .go file
// in the scanned modules.
func walkGoSources(t *testing.T, fn func(rel, content string)) {
	t.Helper()
	root := repoRoot(t)
	for _, mod := range scanModules {
		err := filepath.WalkDir(filepath.Join(root, mod), func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if skipDirs[d.Name()] {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			fn(filepath.ToSlash(rel), string(b))
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", mod, err)
		}
	}
}

func isSanctioned(rel string) bool {
	for prefix := range sanctioned {
		if strings.HasPrefix(rel, prefix) {
			return true
		}
	}
	return false
}

// TestNoUnsanctionedInt64InstallationIDSurface is the contract: after
// #2013 every cross-forge installation seam is a forge.CredentialScope.
// A new `installationID int64` declaration outside the sanctioned set is
// a new forge-coupled surface and fails here with its file:line.
func TestNoUnsanctionedInt64InstallationIDSurface(t *testing.T) {
	var offenders []string
	walkGoSources(t, func(rel, content string) {
		if isSanctioned(rel) {
			return
		}
		for i, line := range strings.Split(content, "\n") {
			if installationIDDecl.MatchString(line) {
				offenders = append(offenders, rel+":"+strconv.Itoa(i+1)+": "+strings.TrimSpace(line))
			}
		}
	})
	if len(offenders) > 0 {
		t.Fatalf("%d unsanctioned int64 installation-id declaration(s) — use forge.CredentialScope,"+
			" or add a sanctioned[] entry stating why this surface is forge-specific:\n\t%s",
			len(offenders), strings.Join(offenders, "\n\t"))
	}
}

// TestNoExportedGitHubClientMethodTakesInt64InstallationID narrows the
// whole-package githubclient sanction: its EXPORTED surface must stay
// scope-taking. Anchored to the githubclient package deliberately —
// githubapp.Client.IssueInstallationToken matches the same method shape
// and is a sanctioned survivor, so a package-blind pattern would fail on
// a legitimately GitHub-shaped API.
func TestNoExportedGitHubClientMethodTakesInt64InstallationID(t *testing.T) {
	const pkg = "backend/internal/githubclient/"
	var offenders []string
	scanned := 0
	walkGoSources(t, func(rel, content string) {
		if !strings.HasPrefix(rel, pkg) {
			return
		}
		scanned++
		for i, line := range strings.Split(content, "\n") {
			if exportedClientInt64Method.MatchString(line) {
				offenders = append(offenders, rel+":"+strconv.Itoa(i+1)+": "+strings.TrimSpace(line))
			}
		}
	})
	if scanned == 0 {
		t.Fatalf("scanned no files under %s — the assertion would pass vacuously", pkg)
	}
	if len(offenders) > 0 {
		t.Fatalf("%d exported *githubclient.Client method(s) take a bare int64 installation id;"+
			" the exported surface must take a forge.CredentialScope:\n\t%s",
			len(offenders), strings.Join(offenders, "\n\t"))
	}
}

// TestSanctionedSurfacesStillExist keeps the allowlist honest: an entry
// whose file no longer matches is dead weight that would silently
// re-sanction the path if code moved back into it.
func TestSanctionedSurfacesStillExist(t *testing.T) {
	matched := map[string]bool{}
	walkGoSources(t, func(rel, content string) {
		if !installationIDDecl.MatchString(content) {
			return
		}
		for prefix := range sanctioned {
			if strings.HasPrefix(rel, prefix) {
				matched[prefix] = true
			}
		}
	})
	for prefix, why := range sanctioned {
		if !matched[prefix] {
			t.Errorf("sanctioned entry %q (%s) no longer has an int64 installation-id declaration;"+
				" drop the entry", prefix, why)
		}
	}
}
