package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/kuhlman-labs/fishhawk/runner/internal/acceptenv"
)

// Forge-writes gate (E72.13 / #3500). A runner that reaches the forge
// (push a branch, open a pull request, push a scenario corpus, post a status
// comment) from an environment or on behalf of a backend that has declared
// forge writes DENIED must refuse the stage BEFORE any agent is spawned and
// before any of those writes is reachable. Two deny sources, either alone
// sufficient, neither dependent on WHICH credential the process holds:
//
//   - the process env carries FISHHAWK_FORGE_WRITES=deny — acceptenv injects
//     it into the acceptance agent's env so every descendant runner the agent
//     could spawn inherits it, and `scripts/dev preview` sets it on the
//     preview daemon's serve line so runners its embedded /mcp verbs spawn
//     inherit it through os.Environ();
//   - the fetched prompt response carries forge_writes: "deny" — stamped by a
//     dev-mode fishhawkd (FISHHAWKD_DEV_FIXTURES / FISHHAWKD_DEV_STUB_FORGE),
//     which is what the preview runs.
//
// The gate is evaluated pre-spawn and answers category C (non-retryable):
// re-dispatching under the same posture would refuse identically.
const (
	forgeWritesEnvVar = acceptenv.ForgeWritesVar
	forgeWritesDeny   = acceptenv.ForgeWritesDeny
)

// forgeWritesDenied reports whether forge writes are denied for this process
// and names the source ("env", "backend" or "env+backend"). Only the exact
// value "deny" denies: an absent variable, an empty value, "allow", or any
// other spelling (mixed case included) proceeds — the deny is an explicit
// opt-in posture, never an accidental one.
func forgeWritesDenied(environ []string, backendSignal string) (denied bool, source string) {
	envDeny := false
	for _, kv := range environ {
		k, v, ok := strings.Cut(kv, "=")
		if ok && k == forgeWritesEnvVar && v == forgeWritesDeny {
			envDeny = true
			break
		}
	}
	backendDeny := backendSignal == forgeWritesDeny
	switch {
	case envDeny && backendDeny:
		return true, "env+backend"
	case envDeny:
		return true, "env"
	case backendDeny:
		return true, "backend"
	}
	return false, ""
}

// refuseIfForgeWritesDenied evaluates the gate against environ and the
// backend's prompt-response signal and, when denied, logs the terminal
// runner_failed line and returns true so the caller exits exitFailure
// without spawning an agent. Returns false (no log line) otherwise.
func refuseIfForgeWritesDenied(logSink io.Writer, environ []string, backendSignal string) bool {
	denied, source := forgeWritesDenied(environ, backendSignal)
	if !denied {
		return false
	}
	_, _ = fmt.Fprintf(logSink,
		`{"event":"runner_failed","reason":"forge_writes_denied","category":"C","source":%q,"detail":%q}`+"\n",
		source,
		fmt.Sprintf("forge writes are denied for this backend/environment (source=%s): the runner will not spawn an agent, push a branch, open a pull request or post to the forge (E72.13 / #3500)", source))
	return true
}
