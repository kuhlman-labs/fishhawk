// Package version exposes the fishhawk-runner build version.
//
// During development the value is the literal "dev". Release builds
// inject the real version via -ldflags at link time, the same way
// fishhawkd does:
//
//	go build -ldflags="-X github.com/kuhlman-labs/fishhawk/runner/internal/version.Version=v0.1.0" \
//	  ./cmd/fishhawk-runner
//
// The runner is published as a versioned GitHub Action
// (kuhlman-labs/fishhawk/runner@vX.Y) per MVP_SPEC §5.1.2; the value
// here matches the action tag at release time.
package version

// Version is the fishhawk-runner build version. Set at link time
// for releases.
var Version = "dev"

// GitSHA is the git commit SHA the binary was built from. Overridden via:
//
//	go build -ldflags="-X github.com/kuhlman-labs/fishhawk/runner/internal/version.GitSHA=abc1234" \
//	  ./cmd/fishhawk-runner
//
// scripts/dev stamps the short HEAD SHA (with a "-dirty" suffix on a dirty
// tree) into dev builds; "unknown" means the binary was built outside
// scripts/dev / without stamping.
var GitSHA = "unknown"
