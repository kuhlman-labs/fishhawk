// Package version exposes the build version of the backend binaries
// (fishhawkd, fishhawk-mcp, fishhawk-mcp-shim).
//
// During development Version is the literal "dev". Every build stamps
// Version and GitSHA at link time with the -X pairs scripts/release-ldflags
// prints — the one stamping source for all five binaries (#4117):
//
//	go build -ldflags "$(scripts/release-ldflags fishhawkd v0.1.0 <git-sha>)" ./cmd/fishhawkd
//
// MinRunnerVersion is not one of those pairs: a release-stamped Version
// resolves it from the committed min-runner-version file (see init below).
package version

import (
	_ "embed"
	"regexp"
	"strings"
)

// Version is the build version. scripts/release-ldflags stamps it: the
// release version for releases, "dev" for scripts/dev builds.
var Version = "dev"

// GitSHA is the git commit SHA the binary was built from. Set at link time
// by scripts/release-ldflags: releases stamp the release commit, and
// scripts/dev stamps the short HEAD SHA (with a "-dirty" suffix on a dirty
// tree). "unknown" means the binary was built without stamping (a bare
// `go build` / `go install`).
var GitSHA = "unknown"

// minRunnerFloorFile is the committed runner floor (#4135): one
// vMAJOR.MINOR.PATCH line, bumped in the same PR as the backend change that
// needs newer runner behaviour.
//
//go:embed min-runner-version
var minRunnerFloorFile string

// MinRunnerVersion is the minimum fishhawk-runner version required to
// interoperate with this backend. A release build (Version is
// vMAJOR.MINOR.PATCH, optionally with a pre-release suffix) advertises the
// committed min-runner-version floor; every other build advertises "dev",
// which signals no enforcement (useful for local development where both
// sides are built from HEAD). An explicit -X stamp of this var wins over
// both; the constant initializer is what keeps that -X effective.
var MinRunnerVersion = "dev"

// releaseVersionRE matches a release-shaped Version: what a backend/v* tag
// stamps. dev, ci-<sha> and sha-<sha> builds do not match.
var releaseVersionRE = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$`)

// isReleaseVersion reports whether v is release-shaped.
func isReleaseVersion(v string) bool {
	return releaseVersionRE.MatchString(v)
}

// resolveMinRunnerVersion is the pure core of init: an explicit non-dev
// stamped value wins as-is; otherwise a release-shaped version resolves to the
// committed floor (when it is non-empty); everything else is "dev".
func resolveMinRunnerVersion(version, stamped, floor string) string {
	if stamped != "" && stamped != "dev" {
		return stamped
	}
	floor = strings.TrimSpace(floor)
	if isReleaseVersion(version) {
		if floor != "" {
			return floor
		}
	}
	return "dev"
}

// init resolves MinRunnerVersion after package-level variables, including the
// -X-stamped Version, are initialized (Go spec, "Package initialization").
// The floor is keyed on Version rather than stamped by scripts/release-ldflags
// because the released image is built by backend/Dockerfile, which cannot exec
// the helper (.dockerignore excludes scripts/), and the image job passes only
// VERSION and GIT_SHA build-args.
func init() {
	MinRunnerVersion = resolveMinRunnerVersion(Version, MinRunnerVersion, minRunnerFloorFile)
}

// String renders the build identity in the shape `fishhawk version` prints:
// "<Version> (<GitSHA>)", or the bare Version when no SHA was stamped. It is
// the --version / version output of fishhawkd, fishhawk-mcp and
// fishhawk-mcp-shim.
func String() string {
	return format(Version, GitSHA)
}

// format is String's pure core, so tests never mutate the package vars.
func format(v, sha string) string {
	if sha == "unknown" || sha == "" {
		return v
	}
	return v + " (" + sha + ")"
}
