// Package version exposes the build-time version string the CLI
// reports via `fishhawk version`.
//
// Precedence, highest first: an -ldflags stamp (Version / GitSHA below),
// then the identity Go recorded in the binary (debug.ReadBuildInfo), then
// the literal "dev" / "unknown" defaults. A binary carrying no stamp reports
// the module version a `go install ...@<ref>` build recorded, or the
// checkout commit a `go build` from a git clone recorded (#4139).
package version

import "runtime/debug"

// Version is the CLI's reported version. Overridden via:
//
//	go build -ldflags "-X github.com/kuhlman-labs/fishhawk/cli/internal/version.Version=v0.1.0"
var Version = "dev"

// GitSHA is the git commit SHA the binary was built from. Overridden via:
//
//	go build -ldflags "-X github.com/kuhlman-labs/fishhawk/cli/internal/version.GitSHA=abc1234"
//
// scripts/dev stamps the short HEAD SHA (with a "-dirty" suffix on a dirty
// tree) into dev builds; "unknown" means the binary was built outside
// scripts/dev / without stamping.
var GitSHA = "unknown"

const (
	unstampedVersion = "dev"
	unstampedGitSHA  = "unknown"
	// develVersion is the Main.Version Go records for a build from a local
	// checkout rather than a module-cache `go install`.
	develVersion = "(devel)"
)

// readBuildInfo is the seam tests swap; production reads the real build info.
var readBuildInfo = debug.ReadBuildInfo

// String renders the build identity `fishhawk version` prints:
// "<version> (<sha>)", or the bare version when no SHA is known.
func String() string {
	var info *debug.BuildInfo
	if bi, ok := readBuildInfo(); ok {
		info = bi
	}
	return format(resolve(Version, GitSHA, info))
}

// format is String's pure rendering core.
func format(v, sha string) string {
	if sha == unstampedGitSHA || sha == "" {
		return v
	}
	return v + " (" + sha + ")"
}

// resolve picks the reported version and SHA. A binary stamped by ldflags
// is returned verbatim; only a wholly unstamped binary (Version "dev" AND
// GitSHA "unknown") falls back to the build info Go recorded: the main
// module version (a pseudo-version for a `go install ...@<commit>`, the tag
// for a tagged ref) unless it is empty or "(devel)", and the vcs.revision
// build setting, suffixed "-dirty" when vcs.modified is true.
//
// The whole-build stamp rule is deliberate: ANY stamp means stamped.
// scripts/dev stamps Version "dev" with a real GitSHA through
// scripts/release-ldflags, and that build must keep reporting "dev" even
// if a toolchain records a VCS-derived module version.
//
// The "-dirty" suffix is this package's chosen convention, mirroring
// scripts/dev's _sha_with_dirty, not a requirement of the originating issue:
// a modified clone build must not claim a clean commit.
func resolve(stampedVersion, stampedSHA string, info *debug.BuildInfo) (string, string) {
	if stampedVersion != unstampedVersion || stampedSHA != unstampedGitSHA {
		return stampedVersion, stampedSHA
	}
	v, sha := unstampedVersion, unstampedGitSHA
	if info == nil {
		return v, sha
	}
	if mv := info.Main.Version; mv != "" && mv != develVersion {
		v = mv
	}
	var revision string
	var modified bool
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.modified":
			modified = s.Value == "true"
		}
	}
	if revision != "" {
		sha = revision
		if modified {
			sha += "-dirty"
		}
	}
	return v, sha
}
