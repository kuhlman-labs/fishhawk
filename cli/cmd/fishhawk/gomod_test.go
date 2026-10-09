package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// credstoreModule is the in-repo module the CLI consumes by pseudo-version.
const credstoreModule = "github.com/kuhlman-labs/fishhawk/credstore"

// pseudoVersionRE matches a v0.0.0-<UTC yyyymmddhhmmss>-<12-hex commit>
// pseudo-version — the form `go get <module>@<sha>` records.
var pseudoVersionRE = regexp.MustCompile(`^v0\.0\.0-\d{14}-[0-9a-f]{12}$`)

// TestCLIModuleIsGoInstallable pins the precondition that makes
// `go install github.com/kuhlman-labs/fishhawk/cli/cmd/fishhawk@<version>`
// work (#4117): the CLI's go.mod must carry NO replace directive (go install
// refuses one — https://go.dev/ref/mod#go-install), its credstore require
// must be a resolvable pseudo-version, and go.sum must carry both hashes for
// exactly that version. A bare `go build` cannot catch a regression here:
// workspace mode (go.work's `use ./credstore`) ignores both the replace and
// the pin. Bump procedure: cli/README.md § "Install".
func TestCLIModuleIsGoInstallable(t *testing.T) {
	gomod, err := os.ReadFile("../../go.mod")
	if err != nil {
		t.Fatalf("read cli/go.mod: %v", err)
	}
	gosum, err := os.ReadFile("../../go.sum")
	if err != nil {
		t.Fatalf("read cli/go.sum: %v", err)
	}

	var pin string
	block := "" // the directive whose `( ... )` block the scan is inside
	for _, raw := range strings.Split(string(gomod), "\n") {
		line := raw
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if block != "" && fields[0] == ")" {
			block = ""
			continue
		}
		// `replace a => b` and the block form `replace (` both open with the
		// keyword once comments are stripped.
		if fields[0] == "replace" {
			t.Errorf("cli/go.mod carries a replace directive (%q): `go install ...cli/cmd/fishhawk@...` refuses a module with one; pin credstore by pseudo-version instead (cli/README.md § \"Install\")", strings.TrimSpace(raw))
		}
		if len(fields) == 2 && fields[1] == "(" {
			block = fields[0]
			continue
		}
		// A require entry, single-line (`require m v`) or inside a require
		// block (`m v`).
		switch {
		case fields[0] == "require":
			fields = fields[1:]
		case block != "require":
			continue
		}
		if len(fields) >= 2 && fields[0] == credstoreModule {
			pin = fields[1]
		}
	}

	if pin == "" {
		t.Fatalf("cli/go.mod does not require %s", credstoreModule)
	}
	if !pseudoVersionRE.MatchString(pin) {
		t.Errorf("cli/go.mod requires %s %s, want a pseudo-version matching %s (bump with `GOWORK=off go get %s@<merged-sha>`)", credstoreModule, pin, pseudoVersionRE, credstoreModule)
	}

	sum := string(gosum)
	for _, want := range []string{
		credstoreModule + " " + pin + " h1:",
		credstoreModule + " " + pin + "/go.mod h1:",
	} {
		if !strings.Contains(sum, "\n"+want) && !strings.HasPrefix(sum, want) {
			t.Errorf("cli/go.sum is missing the %q line for the pinned credstore; run `cd cli && GOWORK=off go mod tidy`", want)
		}
	}
}
