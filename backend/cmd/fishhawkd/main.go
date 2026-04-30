// Command fishhawkd is the Fishhawk backend control plane.
//
// E3.1 (https://github.com/kuhlman-labs/fishhawk/issues/41) only scaffolds
// the module. The real HTTP server, state machine, and policy evaluator
// land in subsequent issues under epic E3 (#3).
package main

import (
	"fmt"

	"github.com/kuhlman-labs/fishhawk/backend/internal/version"
)

func main() {
	fmt.Printf("fishhawkd %s\n", version.Version)
}
