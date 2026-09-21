package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/kuhlman-labs/fishhawk/runner/internal/acceptenv"
)

// Forge-writes gate (E72.13 / #3500, E72.16 / #3510). A runner that reaches
// the forge (push a branch, open a pull request, push a scenario corpus, post
// a status comment) from an environment or on behalf of a backend that has
// declared forge writes DENIED must refuse the stage BEFORE any agent is
// spawned and before any of those writes is reachable. Three deny sources,
// any alone sufficient, none dependent on WHICH credential the process holds:
//
//   - the process env carries FISHHAWK_FORGE_WRITES=deny — acceptenv injects
//     it into the acceptance agent's env so every descendant runner the agent
//     could spawn inherits it, and `scripts/dev preview` sets it on the
//     preview daemon's serve line so runners its embedded /mcp verbs spawn
//     inherit it through os.Environ();
//   - the fetched prompt response carries forge_writes: "deny" — stamped by a
//     dev-mode fishhawkd (FISHHAWKD_DEV_FIXTURES / FISHHAWKD_DEV_STUB_FORGE),
//     which is what the preview runs;
//   - the backend's /healthz advertises dev_mode: true — probed ONCE by the
//     runner itself at the top of run(), before the prompt-fetch block, so
//     it reaches BOTH launch paths: a --prompt-file launch fetches no prompt
//     (no backend signal) and may come from an env that scrubbed the
//     variable, which left the first two sources blind (#3510).
//
// The /healthz probe is FAIL-OPEN by design: a production fishhawkd omits
// the key (omitempty), so an unreachable, non-200, non-JSON or dev_mode-less
// answer must mean "proceed", never "deny". Only a complete JSON object whose
// dev_mode member is boolean true denies. The outcome is always logged
// (forge_writes_healthz_probe) so the fail-open is visible, never silent.
//
// The gate is evaluated pre-spawn and answers category C (non-retryable):
// re-dispatching under the same posture would refuse identically.
const (
	forgeWritesEnvVar = acceptenv.ForgeWritesVar
	forgeWritesDeny   = acceptenv.ForgeWritesDeny
)

// devModeProbe is the classified answer of one /healthz probe. devMode is
// true ONLY for outcome "dev_mode"; every other outcome carries false and a
// detail string naming why, so the caller can log the fail-open reason.
type devModeProbe struct {
	devMode bool
	outcome string
	detail  string
}

// Probe outcomes. Exactly one is a deny.
const (
	probeOutcomeDevMode      = "dev_mode"     // 200 + JSON object with dev_mode: true — DENY
	probeOutcomeProduction   = "production"   // 200 + JSON object, dev_mode absent, false or not a bool
	probeOutcomeUnverifiable = "unverifiable" // non-200, or a body that is not a single JSON object
	probeOutcomeUnreachable  = "unreachable"  // request build or transport error
)

// forgeWritesHealthzBodyCap bounds the /healthz body read. A body over the
// cap is unverifiable (fail-open), never truncated-then-decoded.
const forgeWritesHealthzBodyCap = 64 << 10

// forgeWritesProbeClient dials the /healthz dev_mode probe DIRECT (Proxy
// explicitly nil: the runner never routes its own probe through the agent's
// egress proxy and ignores any ambient proxy env). Dedicated to this gate —
// NOT shared with previewprobe.go's probeHTTPClient — so previewprobe_test.go's
// swap of that var can never interact with the forge-writes tests.
var forgeWritesProbeClient = &http.Client{
	Timeout:   5 * time.Second,
	Transport: &http.Transport{Proxy: nil},
}

// probeBackendDevMode is the seam run() calls. Production is the HTTP probe;
// runTestMain installs a no-dial stub so the package's ~130 run()-driving
// tests that name https://api.fishhawk.test never pay a DNS lookup or a 5s
// transport timeout, and withRealDevModeProbe restores the real one for the
// tests that exercise it.
var probeBackendDevMode = probeBackendDevModeHTTP

// probeBackendDevModeHTTP GETs <backendURL>/healthz and classifies the
// answer. It NEVER returns an error: every non-dev_mode outcome is
// devMode:false with a detail string. The body is read bounded and decoded
// with json.Unmarshal into a map so the COMPLETE body must be a single JSON
// object — a null, an array, a scalar, trailing garbage or an over-cap body
// is unverifiable, and only a dev_mode member that unmarshals to boolean
// true denies (a string "true" is production).
func probeBackendDevModeHTTP(ctx context.Context, backendURL string) devModeProbe {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	url := strings.TrimRight(backendURL, "/") + "/healthz"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return devModeProbe{outcome: probeOutcomeUnreachable, detail: "build request: " + err.Error()}
	}
	req.Header.Set("Accept", "application/json")
	resp, err := forgeWritesProbeClient.Do(req)
	if err != nil {
		return devModeProbe{outcome: probeOutcomeUnreachable, detail: "GET /healthz: " + err.Error()}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return devModeProbe{outcome: probeOutcomeUnverifiable, detail: fmt.Sprintf("GET /healthz: status %d", resp.StatusCode)}
	}
	// Read one byte past the cap so an over-cap body is distinguishable from
	// one that is exactly at it.
	body, err := io.ReadAll(io.LimitReader(resp.Body, forgeWritesHealthzBodyCap+1))
	if err != nil {
		return devModeProbe{outcome: probeOutcomeUnverifiable, detail: "read /healthz body: " + err.Error()}
	}
	if len(body) > forgeWritesHealthzBodyCap {
		return devModeProbe{outcome: probeOutcomeUnverifiable, detail: fmt.Sprintf("/healthz body exceeds %d bytes", forgeWritesHealthzBodyCap)}
	}
	// json.Unmarshal rejects trailing content and a non-object top level
	// (null, array, scalar) fails to unmarshal into a map — exactly the
	// "complete body is a single JSON object" check. A streaming Decoder
	// would accept a leading object and ignore the rest; not used.
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return devModeProbe{outcome: probeOutcomeUnverifiable, detail: "/healthz body is not a single JSON object: " + err.Error()}
	}
	if obj == nil {
		// `null` unmarshals into a nil map without error.
		return devModeProbe{outcome: probeOutcomeUnverifiable, detail: "/healthz body is JSON null, not an object"}
	}
	raw, ok := obj["dev_mode"]
	if !ok {
		return devModeProbe{outcome: probeOutcomeProduction, detail: "/healthz omits dev_mode"}
	}
	var devMode bool
	if err := json.Unmarshal(raw, &devMode); err != nil {
		return devModeProbe{outcome: probeOutcomeProduction, detail: "/healthz dev_mode is not a JSON boolean: " + err.Error()}
	}
	if !devMode {
		return devModeProbe{outcome: probeOutcomeProduction, detail: "/healthz dev_mode is false"}
	}
	return devModeProbe{devMode: true, outcome: probeOutcomeDevMode, detail: "/healthz advertises dev_mode: true"}
}

// forgeWritesDenied reports whether forge writes are denied for this process
// and names the source(s) that fired, joined in the FIXED order env, backend,
// healthz with "+" ("env", "backend", "healthz", "env+backend",
// "env+healthz", "backend+healthz", "env+backend+healthz"). Only the exact
// value "deny" denies for the first two: an absent variable, an empty value,
// "allow", or any other spelling (mixed case included) proceeds — the deny is
// an explicit opt-in posture, never an accidental one. healthzDevMode is the
// classified probe's devMode.
func forgeWritesDenied(environ []string, backendSignal string, healthzDevMode bool) (denied bool, source string) {
	var fired []string
	for _, kv := range environ {
		k, v, ok := strings.Cut(kv, "=")
		if ok && k == forgeWritesEnvVar && v == forgeWritesDeny {
			fired = append(fired, "env")
			break
		}
	}
	if backendSignal == forgeWritesDeny {
		fired = append(fired, "backend")
	}
	if healthzDevMode {
		fired = append(fired, "healthz")
	}
	if len(fired) == 0 {
		return false, ""
	}
	return true, strings.Join(fired, "+")
}

// refuseIfForgeWritesDenied evaluates the gate against environ, the
// backend's prompt-response signal and the /healthz dev_mode probe and, when
// denied, logs the terminal runner_failed line and returns true so the
// caller exits exitFailure without spawning an agent. Returns false (no log
// line) otherwise.
func refuseIfForgeWritesDenied(logSink io.Writer, environ []string, backendSignal string, healthzDevMode bool) bool {
	denied, source := forgeWritesDenied(environ, backendSignal, healthzDevMode)
	if !denied {
		return false
	}
	_, _ = fmt.Fprintf(logSink,
		`{"event":"runner_failed","reason":"forge_writes_denied","category":"C","source":%q,"detail":%q}`+"\n",
		source,
		fmt.Sprintf("forge writes are denied for this backend/environment (source=%s; sources are the FISHHAWK_FORGE_WRITES env, the prompt response's forge_writes, and the backend's /healthz dev_mode): the runner will not spawn an agent, push a branch, open a pull request or post to the forge (E72.13 / #3500, E72.16 / #3510)", source))
	return true
}
