// Package version exposes the build version of the backend binaries
// (fishhawkd, fishhawk-mcp, fishhawk-mcp-shim).
//
// During development Version is the literal "dev". Every build stamps
// Version and GitSHA at link time with the -X pairs scripts/release-ldflags
// prints — the one stamping source for all five binaries (#4117):
//
//	go build -ldflags "$(scripts/release-ldflags fishhawkd v0.1.0 <git-sha>)" ./cmd/fishhawkd
package version

// Version is the build version. scripts/release-ldflags stamps it: the
// release version for releases, "dev" for scripts/dev builds.
var Version = "dev"

// GitSHA is the git commit SHA the binary was built from. Set at link time
// by scripts/release-ldflags: releases stamp the release commit, and
// scripts/dev stamps the short HEAD SHA (with a "-dirty" suffix on a dirty
// tree). "unknown" means the binary was built without stamping (a bare
// `go build` / `go install`).
var GitSHA = "unknown"

// MinRunnerVersion is the minimum fishhawk-runner version required to
// interoperate with this backend. Set at link time for releases; "dev"
// signals no enforcement (useful for local development where both sides
// are built from HEAD).
var MinRunnerVersion = "dev"

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
