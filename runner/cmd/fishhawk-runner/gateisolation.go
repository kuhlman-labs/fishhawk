package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/kuhlman-labs/fishhawk/runner/internal/gateiso"
)

// Gate isolation wiring (ADR-063 / #2134). This file resolves the operator's
// isolation configuration ONCE at startup, decides the execution path ONCE per
// process (lazily, on the first gate exec), and owns the runner-side pieces
// the gateiso package leaves to its caller: the gate DISPOSITION the verify
// gates classify on (gateDisposition — an out-of-band channel, never the
// gate's output text, #3448), the operator-facing refusal text, the ONE
// throwaway-checkout materializer both gate sites call, and the host-exec
// seam the container / sandbox paths route through. The exec-path switch
// itself lives on runBoundedGateArgvDisposed (main.go) so there is still
// exactly ONE containment implementation.

// Environment variables the runner reads at startup.
const (
	// gateIsolationModeEnvVar selects the isolation mode
	// (auto|container|clone-sandbox|clone; empty = auto).
	gateIsolationModeEnvVar = "FISHHAWK_GATE_ISOLATION"
	// gateImageEnvVar names the container image the container path runs the
	// gate in. Empty means the container path is unavailable, so a default
	// runner never pays the per-exec cache cost (gateiso/cache.go).
	gateImageEnvVar = "FISHHAWK_GATE_IMAGE"
	// deploymentProfileEnvVar is the runner-DECLARED deployment profile
	// (local|self-hosted|hosted; empty = local). It is declared, not
	// detected: a hosted deployment that forgets it gets fallback rather
	// than refusal — a documented residual pinned by the selection tests.
	deploymentProfileEnvVar = "FISHHAWK_DEPLOYMENT_PROFILE"
)

// gateIsolationRefusedSignature leads every refusal output. It is
// operator-facing TEXT only: since #3448 the verify gates classify a refusal
// on the out-of-band gateDisposition (gateRefused), never by matching this
// literal in the gate's output — verify output is untrusted, and a test that
// printed the literal must not be able to steer its own red tree to category
// C. It deliberately matches none of isVerifyInfraFailure's signatures so the
// text is never absorbed as a flake either.
const gateIsolationRefusedSignature = "gate isolation refused:"

// gateDisposition reports HOW a gate exec ended, OUT OF BAND of its output
// (#3448). The two classifying gate sites (runVerifyFixLoop,
// runVerifyGateCommitted) read it instead of matching a leading literal in
// the untrusted verify output. The values are documented by CLASSIFICATION;
// the zero value is gateExecuted so every `_`-receiving call site — and the
// tolerant tmp-dir / clone `skipped` branches of runVerifyCommittedTree — is
// safe by construction (never category C).
type gateDisposition int

const (
	// gateExecuted: the gate argv ran (or the tolerant pre-exec skip fired);
	// the output and exit code ARE the gate's verdict. Classified exactly as
	// before #3448: pass, red tree (category A/B), or an absorbed infra flake.
	gateExecuted gateDisposition = iota
	// gateCheckoutRefused: container path — the host-side seed refused the
	// checkout's OWN module metadata (errors.Is(err, gateiso.ErrSeedCheckout)).
	// That is TREE-attributable, so it is classified exactly like an executed
	// failure: the fix agent sees the message naming the refused file.
	gateCheckoutRefused
	// gateRefused: the isolation selection refused every path (a hosted
	// profile with no safe runtime or image, or an explicit mode whose path
	// is unavailable). A deployment-configuration outcome: category C, never
	// absorbed as a flake, never handed to the fix agent.
	gateRefused
	// gateUnavailable: container path — a pre-exec failure whose cause is the
	// HOST, not the tree: visible-cache creation, the lint-cache dir, the
	// mount-guard refusal, the host GOMODCACHE probe / `go mod download` (an
	// offline host), or endpoint binding. The gate never executed, so its
	// verdict says nothing about the tree: category C exactly like a refusal
	// (verify_gate_unavailable / errGateContainerUnavailable).
	gateUnavailable
	// gateTimedOut: the gate DID execute but the RUNNER's own per-exec
	// deadline (executor.verify.timeout) expired before it returned, so the
	// process group was SIGKILLed and NO verdict was reached (#3383). A
	// no-verdict outcome: category C at every gate, no infra absorb (a re-run
	// costs another full timeout), no fix agent (there is no failure to
	// hand it). neverExecutedInfra() stays FALSE — the gate ran; it simply
	// did not finish — so every site tests this value explicitly.
	gateTimedOut
)

// neverExecutedInfra reports whether the disposition is one the gates
// classify category C without an absorb and without the fix agent: the gate
// never ran for a reason the tree cannot have caused.
func (d gateDisposition) neverExecutedInfra() bool {
	return d == gateRefused || d == gateUnavailable
}

// String renders the disposition for log lines and test failures.
func (d gateDisposition) String() string {
	switch d {
	case gateExecuted:
		return "executed"
	case gateCheckoutRefused:
		return "checkout_refused"
	case gateRefused:
		return "refused"
	case gateUnavailable:
		return "unavailable"
	case gateTimedOut:
		return "timed_out"
	}
	return fmt.Sprintf("gateDisposition(%d)", int(d))
}

// gateSeedTimeout bounds the host-side module-cache seed before a container
// exec (gateiso.SeedModCache); the gate's own timeout does not cover it.
const gateSeedTimeout = 5 * time.Minute

// gateIsolationState is the process-wide isolation configuration plus the
// lazily computed selection. The package var gateIsolation carries it; its
// NIL value selects the pre-#2134 host exec (path clone, reason
// "unconfigured: host exec") so every direct runBoundedGateCommand caller —
// and every existing test of it — is byte-unchanged.
type gateIsolationState struct {
	mode    gateiso.Mode
	profile gateiso.Profile
	image   string
	probes  gateiso.Probes
	logSink io.Writer

	// detect / probeSandbox are the two host probes selection runs. Nil
	// selects gateiso.DetectRuntime / gateiso.ProbeSandbox; tests inject.
	detect       func(context.Context, gateiso.Probes) gateiso.Runtime
	probeSandbox func(context.Context) (bool, string)

	once sync.Once
	sel  gateiso.Selection
	uid  int
	gid  int
}

// gateIsolation is the live state run() installs and clears (cleanup).
var gateIsolation *gateIsolationState

// execBoundedHostArgvFn is the host-exec seam every selected path routes
// through (container: the runtime CLI; clone-sandbox: the unshare wrapper;
// clone: the gate argv itself). Its third return value, timedOut, reports
// that the RUNNER's own per-exec deadline expired while the parent context
// was still live (#3383) — the out-of-band signal the callers map to
// gateTimedOut; a parent-context cancellation (a runner shutdown) keeps
// (-1, false). It is a package var SOLELY so a test can capture the argv,
// prove it was never reached, or script a timed-out result; production
// leaves it execBoundedHostArgv.
var execBoundedHostArgvFn = execBoundedHostArgv

// seedModCacheFn is the host-side module-cache seed the container path runs
// (gateiso.SeedModCache). A package var SOLELY so a test can capture the
// environment the seed is handed; production leaves it gateiso.SeedModCache.
var seedModCacheFn = gateiso.SeedModCache

// bindEndpointEnvFn binds the runtime CLI's environment to the validated
// socket (gateiso.Runtime.BindEndpointEnv). A package var SOLELY so a test
// can make the binder fail and pin that branch's disposition (#3448);
// production leaves it the method expression.
var bindEndpointEnvFn = gateiso.Runtime.BindEndpointEnv

// dockerFixturesEligible / dockerFixturesRan are the end-to-end fixture
// sentinel (gateisolation_e2e_test.go increments Ran at the END of every
// docker-gated fixture; main_test.go's TestMain fails the binary when a
// runtime is present but no docker-gated fixture ran). They live here so the
// two test files compile independently.
var (
	dockerFixturesEligible bool
	dockerFixturesRan      int
)

// configureGateIsolation parses the three variables through getenv and
// applies the startup rule ProfileForbidsFallback. A configuration error names
// the variable and the valid values; run() logs it as runner_failed
// reason=config and exits exitUsage BEFORE any backend contact. On success it
// emits gate_isolation_configured and returns the state for run() to install.
func configureGateIsolation(getenv func(string) string, probes gateiso.Probes, logSink io.Writer) (*gateIsolationState, error) {
	mode, err := gateiso.ParseMode(getenv(gateIsolationModeEnvVar))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", gateIsolationModeEnvVar, err)
	}
	profile, err := gateiso.ParseProfile(getenv(deploymentProfileEnvVar))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", deploymentProfileEnvVar, err)
	}
	if err := gateiso.ProfileForbidsFallback(profile, mode); err != nil {
		return nil, fmt.Errorf("%s=%s with %s=%s: %w", deploymentProfileEnvVar, profile, gateIsolationModeEnvVar, mode, err)
	}
	st := &gateIsolationState{
		mode:    mode,
		profile: profile,
		image:   strings.TrimSpace(getenv(gateImageEnvVar)),
		probes:  probes,
		logSink: logSink,
		uid:     os.Getuid(),
		gid:     os.Getgid(),
	}
	if logSink != nil {
		_, _ = fmt.Fprintf(logSink,
			`{"event":"gate_isolation_configured","mode":%q,"profile":%q,"image":%q}`+"\n",
			mode, profile, st.image)
	}
	return st, nil
}

// hostExecSelection is the nil-state selection: the pre-#2134 host exec.
func hostExecSelection() gateiso.Selection {
	return gateiso.Selection{
		Path:    gateiso.PathClone,
		Mode:    gateiso.ModeAuto,
		Profile: gateiso.ProfileLocal,
		Reason:  "unconfigured: host exec",
	}
}

// selection decides the execution path ONCE per process: runtime detection
// and the sandbox probe are paid on the first gate exec, then the recorded
// Selection (including Runtime.Endpoint) is logged as
// gate_isolation_selected and reused. A nil receiver is the host exec.
func (s *gateIsolationState) selection(ctx context.Context) gateiso.Selection {
	if s == nil {
		return hostExecSelection()
	}
	s.once.Do(func() {
		detect := s.detect
		if detect == nil {
			detect = gateiso.DetectRuntime
		}
		probe := s.probeSandbox
		if probe == nil {
			probe = gateiso.ProbeSandbox
		}
		rt := detect(ctx, s.probes)
		avail, reason := probe(ctx)
		s.sel = gateiso.Select(gateiso.Inputs{
			Mode:    s.mode,
			Profile: s.profile,
			Image:   s.image,
			Runtime: rt,
			Sandbox: gateiso.SandboxProbe{Available: avail, Reason: reason},
		})
		if s.logSink != nil {
			b, _ := json.Marshal(s.sel)
			_, _ = fmt.Fprintf(s.logSink, `{"event":"gate_isolation_selected","selection":%s}`+"\n", b)
		}
	})
	return s.sel
}

// cleanup releases the process-wide state. run() defers it so a later run()
// in the same process (the test binary) starts unconfigured.
func (s *gateIsolationState) cleanup() {
	if gateIsolation == s {
		gateIsolation = nil
	}
}

// gateRefusalMessage renders the operator-facing output a refused gate
// returns instead of executing. It always starts with
// gateIsolationRefusedSignature, but that lead is TEXT for the operator and
// the FailureReason — classification reads the gateRefused disposition
// runBoundedGateArgvDisposed returns beside it, not this string.
func gateRefusalMessage(sel gateiso.Selection) string {
	return fmt.Sprintf("%s %s (mode=%s profile=%s image=%q runtime=%s safe=%t)",
		gateIsolationRefusedSignature, sel.Reason, sel.Mode, sel.Profile, sel.Image, sel.Runtime.Kind, sel.Runtime.Safe)
}

// materializeGateCheckout is the ONE throwaway-checkout materializer both
// gate sites (runVerifyCommittedTree, measureDiffCoverage) call: an
// independent `--no-hardlinks` clone of repoDir at <parent>/tree with headSHA
// checked out detached (gateiso.MaterializeClone). Unlike the `git worktree
// add --detach` it replaces, the clone shares no objects, refs or hooks with
// the primary, so a ref or hook a gate plants stays in the throwaway tree.
// The caller removes parent (os.RemoveAll) — there is no worktree to
// unregister.
func materializeGateCheckout(ctx context.Context, repoDir, headSHA, parent string) (string, error) {
	wt := filepath.Join(parent, "tree")
	if _, err := gateiso.MaterializeClone(ctx, "git", repoDir, headSHA, wt); err != nil {
		return "", err
	}
	return wt, nil
}

// runGateInContainer is the container branch of runBoundedGateArgv: fresh
// empty visible caches, argv build under the resolved-path mount guard
// (FIRST — a checkout the guard refuses is never handed to the host-side
// seed, so no go process runs against a checkout the container would not
// have been given), THEN the host-side module-cache seed run under the
// SANITIZED gate env (never the runner's inherited environment — no runner
// credential reaches the `go mod download`, and the checkout's module
// metadata is refused before any go process runs when it would reach outside
// the checkout), exec through the host seam with the RUNNER's inherited environment BOUND to the
// validated endpoint (the runtime CLI needs PATH and its config dir from the
// inherited env; DOCKER_HOST / DOCKER_CONTEXT / CONTAINER_HOST /
// CONTAINER_CONNECTION are dropped and the selection's socket re-pinned, and
// the argv carries the same binding as a global flag, so a docker-context
// switch between gates cannot redirect a bind-mount request to a daemon the
// selection never validated; the SANITIZED gate env crosses into the
// container via -e only), and `rm -f` — under the same binding — on a
// detached context when the exec returned -1 (killing the CLI does not stop
// the container). Every failure before the exec returns -1 WITHOUT
// executing, and the third value names WHY out of band (#3448): a HOST-caused
// pre-exec failure — visible-cache creation, the lint-cache dir, the
// mount-guard refusal, a seed failure NOT wrapping gateiso.ErrSeedCheckout
// (the host GOMODCACHE probe or `go mod download` on an offline host), or
// endpoint binding — is gateUnavailable (category C at the gates, like a
// refusal); a seed failure wrapping ErrSeedCheckout is the checkout's OWN
// metadata being refused, so it is gateCheckoutRefused (tree-attributable,
// classified as an executed failure); the exec path is gateExecuted whatever
// the exit code, EXCEPT that a seam result reporting the runner's own
// deadline expiry is gateTimedOut (#3383, no verdict). Deliberate residual: a legitimate tree whose `replace`
// target sits outside the checkout draws ErrSeedCheckout too and reaches the
// fix agent with the refusing message rather than parking category C.
func runGateInContainer(ctx context.Context, sel gateiso.Selection, argv []string, dir, lintCacheDir string, sanitizedEnv, extraEnv []string, timeout time.Duration) (string, int, gateDisposition) {
	st := gateIsolation
	vc, err := gateiso.NewVisibleCaches()
	if err != nil {
		return "gate container: " + err.Error(), -1, gateUnavailable
	}
	defer func() { _ = vc.Remove() }()
	if err := os.MkdirAll(lintCacheDir, 0o700); err != nil {
		return "gate container: create lint cache dir: " + err.Error(), -1, gateUnavailable
	}
	spec := gateiso.ContainerSpec{
		Runtime:    sel.Runtime,
		Image:      sel.Image,
		Name:       gateiso.NewContainerName(),
		Checkout:   dir,
		GoCache:    vc.GoCache,
		GoModCache: vc.GoModCache,
		LintCache:  lintCacheDir,
		Env:        gateiso.ContainerEnv(sanitizedEnv, extraEnv),
		Argv:       argv,
	}
	if st != nil {
		spec.UID, spec.GID = st.uid, st.gid
	} else {
		spec.UID, spec.GID = os.Getuid(), os.Getgid()
	}
	runArgv, err := spec.BuildArgv(gateiso.MountPolicy{
		Permitted:    []string{dir, vc.Root, lintCacheDir},
		DaemonSocket: sel.Runtime.SocketPath,
	})
	if err != nil {
		return "gate container: " + err.Error(), -1, gateUnavailable
	}
	// Seed only AFTER the mount guard accepted every source: the seed is the
	// one host-side process the container path runs against the checkout.
	if _, err := seedModCacheFn(ctx, nil, dir, "", vc, sanitizedEnv, gateSeedTimeout); err != nil {
		disp := gateUnavailable
		if errors.Is(err, gateiso.ErrSeedCheckout) {
			// The checkout's OWN metadata was refused: tree-attributable.
			disp = gateCheckoutRefused
		}
		return "gate container: seed module cache: " + err.Error(), -1, disp
	}
	// The runtime CLI's env is the runner's inherited environment with the
	// endpoint bound to the socket the selection validated (concern: a
	// context switch after selection must not redirect launch or cleanup).
	cliEnv, err := bindEndpointEnvFn(sel.Runtime, os.Environ())
	if err != nil {
		return "gate container: " + err.Error(), -1, gateUnavailable
	}
	out, code, timedOut := execBoundedHostArgvFn(ctx, runArgv, dir, cliEnv, timeout)
	if code == -1 {
		killCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), diffCoverageCleanupTimeout)
		defer cancel()
		_, _, _ = execBoundedHostArgvFn(killCtx, spec.KillArgv(), dir, cliEnv, diffCoverageCleanupTimeout)
	}
	if timedOut {
		// The runner's own deadline killed the runtime CLI's process group
		// (the `rm -f` above has already torn the container down): no
		// verdict, gateTimedOut (#3383).
		return out, code, gateTimedOut
	}
	return out, code, gateExecuted
}

// errGateIsolationRefused is joined (alongside gitops.ErrVerifyInfraFailure)
// into the single-shot gate's refusal error so a caller can distinguish a
// refusal from an ordinary infra failure with errors.Is.
var errGateIsolationRefused = errors.New("gate isolation refused")

// errGateContainerUnavailable is joined (alongside gitops.ErrVerifyInfraFailure)
// into the single-shot gate's error for a gateUnavailable disposition (#3448):
// the container path failed BEFORE exec for a host-caused reason, so the gate
// never executed. Distinguishable from a refusal (errGateIsolationRefused)
// and from an absorbed-then-persistent infra signature with errors.Is.
var errGateContainerUnavailable = errors.New("gate container unavailable")
