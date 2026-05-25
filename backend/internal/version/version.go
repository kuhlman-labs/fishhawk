// Package version exposes the fishhawkd build version.
//
// During development the value is the literal "dev". Release builds inject
// the real version via -ldflags at link time:
//
//	go build -ldflags="-X github.com/kuhlman-labs/fishhawk/backend/internal/version.Version=v0.1.0" ./cmd/fishhawkd
package version

// Version is the fishhawkd build version. Set at link time for releases.
var Version = "dev"

// GitSHA is the git commit SHA the binary was built from. Set at link time
// for releases; "unknown" in development builds.
var GitSHA = "unknown"

// MinRunnerVersion is the minimum fishhawk-runner version required to
// interoperate with this backend. Set at link time for releases; "dev"
// signals no enforcement (useful for local development where both sides
// are built from HEAD).
var MinRunnerVersion = "dev"
