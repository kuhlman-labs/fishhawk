package basedelta

import (
	"strings"

	"github.com/bmatcuk/doublestar/v4"
)

// SurfaceVersion versions GlobalSurface and SharedArtifactDirs together. Bump
// it whenever either set changes, so a recorded verdict names the set it was
// decided against.
const SurfaceVersion = 1

// GlobalSurface is the ADR-096 D3 set: root-anchored doublestar patterns for
// paths a verify leg reads as a whole-tree input, so a change to one on EITHER
// side forces a fresh verify pass. README.md carries one row per pattern
// naming the leg (or rationale) that puts it here, and surface_test.go keeps
// the two in sync. verifylegs_test.go pins every verify leg's declared inputs
// against this set.
var GlobalSurface = []string{
	// ADR-096 D3, as enumerated by #4292.
	"go.work",
	"**/go.mod",
	"**/go.sum",
	"scripts/**",
	".golangci.yml",
	"deploy/gate-image/**",
	".fishhawk/**",
	"docs/spec/**",
	"docs/api/**",
	"site/**",
	"docs/adr/**",
	"docs/ARCHITECTURE.md",
	"backend/internal/postgres/migrations/**",
	"backend/internal/audit/categories.go",
	"testdata/**",
	// Verify-leg inputs found by the leg survey.
	"deploy/helm/**",
	"backend/cmd/fishhawkd/**",
	"backend/Dockerfile",
	"Makefile",
	".github/workflows/**",
	"cli/internal/docgen/**",
	"cli/internal/cmdinfo/**",
	"cli/cmd/fishhawk-docgen/**",
	"backend/internal/version/**",
	"cli/internal/version/**",
	"runner/internal/version/**",
	// scripts/sync-schemas mirror destinations.
	"backend/internal/plan/schemas/**",
	"backend/internal/spec/schemas/**",
	"backend/internal/crewmessage/schemas/**",
	"backend/internal/specialistjob/schemas/**",
	"backend/internal/operatorrole/schemas/**",
	"backend/internal/workmgmt/schemas/**",
	"backend/internal/operatorrole/defaults/**",
	"backend/internal/workmgmt/defaults/**",
	"backend/internal/spec/presets/**",
	"cli/internal/spec/schemas/**",
	"cli/internal/spec/presets/**",
	"runner/internal/plan/schemas/**",
	"backend/internal/spec/testdata/predicate-fixtures.json",
	"backend/internal/spec/testdata/review-conventions-fixtures.json",
	"backend/internal/spec/testdata/tier-expansion-fixtures.json",
	"cli/internal/spec/testdata/predicate-fixtures.json",
	"cli/internal/spec/testdata/review-conventions-fixtures.json",
	"cli/internal/spec/testdata/tier-expansion-fixtures.json",
	"backend/internal/server/testdata/acceptance-verdict-fixtures.json",
	"runner/cmd/fishhawk-runner/testdata/acceptance-verdict-fixtures.json",
}

// SharedArtifactDirs are the ADR-096 D1(d) shared-artifact roots: a delta
// path and a PR path under the same entry are dependent. Each entry ends in
// "/".
var SharedArtifactDirs = []string{
	"docs/",
	"frontend/",
	"infra/",
	"acceptance/",
	"auth/",
	".github/",
	".agents/",
	".claude/",
	".githooks/",
	"deploy/",
}

// MatchGlobal reports the first GlobalSurface pattern p matches. Matching is
// anchored at the repository root: "testdata/**" matches testdata/wire/x.json
// but not backend/internal/x/testdata/y.json.
func MatchGlobal(p string) (string, bool) {
	for _, pat := range GlobalSurface {
		if ok, err := doublestar.Match(pat, p); err == nil && ok {
			return pat, true
		}
	}
	return "", false
}

// sharedDir reports the SharedArtifactDirs entry p sits under.
func sharedDir(p string) (string, bool) {
	for _, d := range SharedArtifactDirs {
		if strings.HasPrefix(p, d) {
			return d, true
		}
	}
	return "", false
}
