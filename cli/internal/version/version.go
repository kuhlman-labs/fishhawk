// Package version exposes the build-time version string the CLI
// reports via `fishhawk --version`. Set via -ldflags at release
// time; defaults to "dev" for local builds.
package version

// Version is the CLI's reported version. Overridden via:
//
//	go build -ldflags "-X github.com/kuhlman-labs/fishhawk/cli/internal/version.Version=v0.1.0"
var Version = "dev"
