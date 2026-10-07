package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kuhlman-labs/fishhawk/runner/internal/gateiso"
	"github.com/kuhlman-labs/fishhawk/runner/internal/upload"
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
	// gate in when the stage declares no gate_container (E51.3 / #2136: a
	// declared image or build takes precedence). Empty with no declaration
	// means the container path is unavailable, so a default runner never pays
	// the per-exec cache cost (gateiso/cache.go).
	gateImageEnvVar = "FISHHAWK_GATE_IMAGE"
	// deploymentProfileEnvVar is the runner-DECLARED deployment profile
	// (local|self-hosted|hosted; empty = local). It is declared, not
	// detected: a hosted deployment that forgets it gets fallback rather
	// than refusal — a documented residual pinned by the selection tests.
	deploymentProfileEnvVar = "FISHHAWK_DEPLOYMENT_PROFILE"
	// gateServicesEnvVar lists the services the container path provisions
	// beside the gate (#2137; comma list, only member `postgres`, empty =
	// none). It is ignored off the container path (gate_services_ignored).
	gateServicesEnvVar = "FISHHAWK_GATE_SERVICES"
	// gatePostgresImageEnvVar overrides the Postgres service image (default
	// gateiso.DefaultPostgresImage; operators should pin it by digest).
	gatePostgresImageEnvVar = "FISHHAWK_GATE_POSTGRES_IMAGE"
	// gateImageAllowlistEnvVar is the operator image allowlist a declared
	// gate_container image — and every base of a declared build — must pass
	// (E51.3 / #2136; gateiso.ParseAllowlist grammar, empty = no allowlist).
	gateImageAllowlistEnvVar = "FISHHAWK_GATE_IMAGE_ALLOWLIST"
	// gateBuildEnvVar is the in-repo gate image build posture (allow|deny;
	// empty = the profile default: hosted deny, local/self-hosted allow).
	gateBuildEnvVar = "FISHHAWK_GATE_BUILD"
	// gateCacheEnvVar is the container path's build-cache posture
	// (process|off; empty = process, E51.18 / #3967): `process` mounts ONE
	// per-runner-process named volume for GOCACHE and the lint cache, `off`
	// keeps the per-exec cold caches (gateiso/cachevolume.go).
	gateCacheEnvVar = "FISHHAWK_GATE_CACHE"
	// gateDockerConfigEnvVar names an operator-prepared docker config dir the
	// container path's runtime CLI calls use for registry credentials
	// (E51.26 / #4046; empty = the runner-owned anonymous config, which
	// carries no credential and invokes no credential helper). Validated at
	// startup (gateiso.LoadOperatorDockerConfig) and never removed.
	gateDockerConfigEnvVar = "FISHHAWK_GATE_DOCKER_CONFIG"
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

// Container-path provisioning bounds (#2137 approval condition 9). The passwd
// read and every gate-service step run BEFORE the gate exec and — like the
// module-cache seed — do NOT count against the gate's own timeout: each step
// carries its own bound, and a step that exceeds it is a provisioning failure
// (gateUnavailable), never a gate verdict.
const (
	// gatePasswdReadTimeout bounds the image /etc/passwd read, including a
	// cold pull of the gate image.
	gatePasswdReadTimeout = 5 * time.Minute
	// gateServiceVolumeTimeout bounds `volume create`.
	gateServiceVolumeTimeout = time.Minute
	// gateServiceStartTimeout bounds `run -d`, including a cold pull of the
	// service image.
	gateServiceStartTimeout = 5 * time.Minute
	// gateServiceProbeTimeout bounds each readiness probe (`logs`,
	// pg_isready).
	gateServiceProbeTimeout = 15 * time.Second
	// gateServiceBootstrapTimeout bounds the least-privilege role bootstrap.
	gateServiceBootstrapTimeout = time.Minute
)

// Gate cache volume bounds (E51.18 / #3967). Like the provisioning bounds
// above, each step runs BEFORE the gate exec and outside the gate's own
// timeout; exceeding one DEGRADES to the per-exec caches, never a verdict.
const (
	// gateCacheVolumeTimeout bounds `volume create`.
	gateCacheVolumeTimeout = time.Minute
	// gateCachePrepareTimeout bounds the prepare helper, including a cold
	// pull of the gate image.
	gateCachePrepareTimeout = 5 * time.Minute
	// gateCacheCheckTimeout bounds the post-prepare write check.
	gateCacheCheckTimeout = time.Minute
)

// Declared gate_container resolution bounds (E51.3 / #2136). Like the
// provisioning bounds above, each runs BEFORE the gate exec and outside the
// gate's own timeout; exceeding one is gateUnavailable, never a verdict.
const (
	// gateImageInspectTimeout bounds one `image inspect`.
	gateImageInspectTimeout = 30 * time.Second
	// gateImagePullTimeout bounds the explicit pull of a declared image.
	gateImagePullTimeout = 10 * time.Minute
	// gateImageBuildTimeout bounds an in-repo build TWICE, separately: once
	// over reading and materializing its committed source from git, and again
	// over the build call itself (worst case about twice this value).
	gateImageBuildTimeout = 20 * time.Minute
)

// gateCredentialProbeTimeout bounds each credential helper probe (E51.26 /
// #4046): a helper that has not answered by then — a keychain behind a
// locked screen, or a slow network-backed helper — fails the exec
// gateUnavailable with container_credentials_blocked instead of hanging the
// pull it would serve. A package var SOLELY so a test can shorten it.
var gateCredentialProbeTimeout = 20 * time.Second

// gateBuildContextLimits bounds a materialized build context. A package var
// SOLELY so a test can lower it; production leaves the gateiso default.
var gateBuildContextLimits = gateiso.DefaultContextLimits

// gateServiceReadyTimeout / gateServiceReadyInterval bound the readiness
// poll. Package vars SOLELY so a test can shorten them.
var (
	gateServiceReadyTimeout  = 90 * time.Second
	gateServiceReadyInterval = 250 * time.Millisecond
)

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

	// services / postgresImage are the gate services the container path
	// provisions per exec (#2137) and the Postgres service image.
	services      []gateiso.Service
	postgresImage string

	// allowlist / buildAllowed are the operator image allowlist and the
	// in-repo build posture (E51.3 / #2136), parsed at startup.
	allowlist    gateiso.Allowlist
	buildAllowed bool

	// declared is the stage's effective gate_container from the fetched
	// prompt (declare); selDone marks that selection() has read it, after
	// which a late declaration is ignored. Both guarded by declMu.
	declMu   sync.Mutex
	declared *upload.GateContainerConfig
	selDone  bool

	// req / decision are the image request selection() built and the policy
	// verdict on it; written once inside once.Do, read by the container path.
	req      gateiso.ImageRequest
	decision gateiso.ImageDecision

	// resolvedMu guards the images declared gates resolved to: deciding is
	// the image of the LAST verify gate to reach the container path (nil when
	// that gate's resolution failed), decidingSeen whether any did, last the
	// most recent of any gate, identities every distinct image.
	resolvedMu   sync.Mutex
	deciding     *gateiso.ResolvedImage
	decidingSeen bool
	last         *gateiso.ResolvedImage
	identities   map[string]bool

	// cacheMode is the FISHHAWK_GATE_CACHE posture. Its ZERO value means off,
	// so a hand-built struct-literal state keeps the per-exec caches; only
	// configureGateIsolation selects process (the default). ownerRun /
	// ownerStage are the run and stage this process serves (bindOwner), which
	// every cache volume name embeds. cacheVol is the volume the next exec
	// reuses when it still BelongsTo the owner, cacheUses how many execs
	// mounted it, cacheMinted every volume minted (removed at cleanup with the
	// runtime and bound CLI env it was minted under). All guarded by cacheMu.
	cacheMode   gateiso.CacheMode
	cacheMu     sync.Mutex
	ownerRun    string
	ownerStage  string
	cacheVol    *gateiso.CacheVolume
	cacheUses   int
	cacheMinted []mintedCacheVolume

	// credentials is the runtime CLI's credential posture (E51.26 / #4046),
	// fixed at startup: operator_config when FISHHAWK_GATE_DOCKER_CONFIG named
	// a valid dir, else anonymous. dockerConfig is the config every
	// post-selection runtime call is pinned to — the operator's, set at
	// startup, or the runner-owned anonymous one, minted lazily on the first
	// container exec (gateDockerConfig) and removed at cleanup AFTER the
	// cache volumes. Guarded by dockerCfgMu.
	credentials  gateiso.Credentials
	dockerCfgMu  sync.Mutex
	dockerConfig *gateiso.DockerConfig

	// passwdByImage caches each gate image's /etc/passwd after a SUCCESSFUL
	// read (#2137 approval condition 7: a failed read is never cached, so a
	// transient failure is retried on a later exec).
	passwdMu      sync.Mutex
	passwdByImage map[string][]byte

	// detect / probeSandbox are the two host probes selection runs. Nil
	// selects gateiso.DetectRuntime / gateiso.ProbeSandbox; tests inject.
	detect       func(context.Context, gateiso.Probes) gateiso.Runtime
	probeSandbox func(context.Context) (bool, string)

	once sync.Once
	sel  gateiso.Selection
	uid  int
	gid  int

	// seamReached is set by markSeamReached at the gate-exec seam
	// (runBoundedGateArgvDisposed) — NOT inside selection()'s once.Do,
	// because selection() has a non-exec caller too (runVerifyCommittedTree
	// reads the path to decide its lock-path env). It is what makes the
	// selection RECORDED for the gate evidence (#2135): a gate reached the
	// seam, refusal included.
	seamReached atomic.Bool
}

// mintedCacheVolume is one cache volume a state minted, with the runtime and
// bound CLI env its removal runs under.
type mintedCacheVolume struct {
	vol    gateiso.CacheVolume
	rt     gateiso.Runtime
	cliEnv []string
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

// execGateAuxArgvFn is the host-exec seam for the runtime CLI calls AROUND a
// container gate exec — the gate image's /etc/passwd read and the gate-service
// lifecycle (#2137) — kept apart from execBoundedHostArgvFn so that seam still
// carries exactly the gate's own `run` and its `rm -f` kill. A package var
// SOLELY so a test can script and record those calls; production leaves it
// execBoundedHostArgv.
var execGateAuxArgvFn = execBoundedHostArgv

// newAnonymousDockerConfigFn mints the runner-owned docker config
// (gateiso.NewAnonymousDockerConfig). A package var SOLELY so a test can make
// the mint fail and pin that branch's disposition.
var newAnonymousDockerConfigFn = gateiso.NewAnonymousDockerConfig

// probeCredentialHelperFn probes one credential helper
// (gateiso.ProbeCredentialHelper). A package var SOLELY so a test can count
// probes; production leaves it the real probe.
var probeCredentialHelperFn = gateiso.ProbeCredentialHelper

// writePasswdFileFn writes the per-exec passwd file (gateiso.WritePasswdFile).
// A package var SOLELY so a test can make the write fail and pin the degrade.
var writePasswdFileFn = gateiso.WritePasswdFile

// gateServiceObserver, when non-nil, is called once per provisioned gate
// service after readiness and the role bootstrap and BEFORE the gate exec,
// with the validated runtime and the bound CLI env — the end-to-end fixtures'
// host-side inspection point. Test-only: production leaves it nil.
var gateServiceObserver func(ctx context.Context, rt gateiso.Runtime, cliEnv []string, svc gateiso.PostgresService)

// dockerFixturesEligible / dockerFixturesRan are the end-to-end fixture
// sentinel (gateisolation_e2e_test.go increments Ran at the END of every
// docker-gated fixture; main_test.go's TestMain fails the binary when a
// runtime is present but no docker-gated fixture ran). They live here so the
// two test files compile independently.
var (
	dockerFixturesEligible bool
	dockerFixturesRan      int
)

// configureGateIsolation parses the nine variables through getenv and
// applies the startup rule ProfileForbidsFallback. A configuration error names
// the variable and the valid values (an allowlist error also names the
// offending entry); run() logs it as runner_failed
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
	services, err := gateiso.ParseServices(getenv(gateServicesEnvVar))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", gateServicesEnvVar, err)
	}
	pgImage := strings.TrimSpace(getenv(gatePostgresImageEnvVar))
	if pgImage == "" {
		pgImage = gateiso.DefaultPostgresImage
	}
	allow, err := gateiso.ParseAllowlist(getenv(gateImageAllowlistEnvVar))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", gateImageAllowlistEnvVar, err)
	}
	buildAllowed, err := gateiso.ParseBuildPolicy(getenv(gateBuildEnvVar), profile)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", gateBuildEnvVar, err)
	}
	cacheMode, err := gateiso.ParseCacheMode(getenv(gateCacheEnvVar))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", gateCacheEnvVar, err)
	}
	creds, dockerCfg := gateiso.CredentialsAnonymous, (*gateiso.DockerConfig)(nil)
	if dir := strings.TrimSpace(getenv(gateDockerConfigEnvVar)); dir != "" {
		cfg, err := gateiso.LoadOperatorDockerConfig(dir)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", gateDockerConfigEnvVar, err)
		}
		creds, dockerCfg = gateiso.CredentialsOperatorConfig, &cfg
	}
	st := &gateIsolationState{
		mode:          mode,
		profile:       profile,
		image:         strings.TrimSpace(getenv(gateImageEnvVar)),
		probes:        probes,
		logSink:       logSink,
		uid:           os.Getuid(),
		gid:           os.Getgid(),
		services:      services,
		postgresImage: pgImage,
		allowlist:     allow,
		buildAllowed:  buildAllowed,
		cacheMode:     cacheMode,
		credentials:   creds,
		dockerConfig:  dockerCfg,
	}
	build := gateiso.BuildPolicyDeny
	if buildAllowed {
		build = gateiso.BuildPolicyAllow
	}
	if logSink != nil {
		_, _ = fmt.Fprintf(logSink,
			`{"event":"gate_isolation_configured","mode":%q,"profile":%q,"image":%q,"credentials":%q,"services":%q,"postgres_image":%q,"allowlist_entries":%d,"build":%q,"cache":%q}`+"\n",
			mode, profile, st.image, creds, joinServices(services), pgImage, len(allow), build, cacheMode)
	}
	return st, nil
}

// declare records the stage's effective gate_container from the fetched
// prompt (E51.3 / #2136). run() calls it right after the fetch, before any
// gate; selection() reads it once. A declaration arriving after the
// selection was decided cannot change the path the earlier gates ran under,
// so it is ignored with a logged line. Nil receiver, nil config, or a config
// carrying neither a source nor a level: no-op (no declaration).
func (s *gateIsolationState) declare(gc *upload.GateContainerConfig) {
	if s == nil || gc == nil || (*gc == upload.GateContainerConfig{}) {
		return
	}
	s.declMu.Lock()
	defer s.declMu.Unlock()
	if s.selDone {
		s.logEvent(`{"event":"gate_container_declaration_ignored","source":%q,"reason":"the gate isolation selection was already decided"}`, gc.Source)
		return
	}
	d := *gc
	s.declared = &d
	s.logEvent(`{"event":"gate_container_declared","source":%q,"image":%q,"dockerfile":%q,"context":%q}`, d.Source, d.Image, d.Dockerfile, d.Context)
}

// imageRequest is the gate image request in precedence order: the declared
// gate_container (stage beats workflow, resolved by the backend), else the
// operator's FISHHAWK_GATE_IMAGE, else none.
func (s *gateIsolationState) imageRequest(declared *upload.GateContainerConfig) gateiso.ImageRequest {
	if declared != nil {
		return gateiso.ImageRequest{Source: declared.Source, Image: declared.Image, Dockerfile: declared.Dockerfile, Context: declared.Context}
	}
	if s.image != "" {
		return gateiso.ImageRequest{Source: gateiso.ImageSourceEnv, Image: s.image}
	}
	return gateiso.ImageRequest{}
}

// joinServices renders a service list as its comma form for log lines.
func joinServices(svcs []gateiso.Service) string {
	parts := make([]string, len(svcs))
	for i, s := range svcs {
		parts[i] = string(s)
	}
	return strings.Join(parts, ",")
}

// logEvent writes one runner log line to the state's sink. Nil-safe.
func (s *gateIsolationState) logEvent(format string, args ...any) {
	if s == nil || s.logSink == nil {
		return
	}
	_, _ = fmt.Fprintf(s.logSink, format+"\n", args...)
}

// postgresServiceWanted reports whether the container path provisions the
// Postgres service for each exec.
func (s *gateIsolationState) postgresServiceWanted() bool {
	if s == nil {
		return false
	}
	for _, svc := range s.services {
		if svc == gateiso.ServicePostgres {
			return true
		}
	}
	return false
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
		s.declMu.Lock()
		s.selDone = true
		declared := s.declared
		s.declMu.Unlock()
		s.req = s.imageRequest(declared)
		s.decision = gateiso.EvaluateImagePolicy(s.profile, s.req, s.allowlist, s.buildAllowed)
		rt := detect(ctx, s.probes)
		avail, reason := probe(ctx)
		s.sel = gateiso.Select(gateiso.Inputs{
			Mode:          s.mode,
			Profile:       s.profile,
			Image:         s.req.Image,
			Runtime:       rt,
			Sandbox:       gateiso.SandboxProbe{Available: avail, Reason: reason},
			Build:         gateiso.DeclaredSource(s.req.Source) && (s.req.Dockerfile != "" || s.req.Context != ""),
			ImageSource:   s.req.Source,
			PolicyRefusal: s.decision.Refusal,
			PolicyWarning: s.decision.Warning,
		})
		if s.sel.Path == gateiso.PathContainer {
			// Only the container path makes runtime CLI calls, so only it
			// carries a credential posture (Select never stamps one).
			s.sel.Credentials = s.credentials
		}
		if s.logSink != nil {
			b, _ := json.Marshal(s.sel)
			_, _ = fmt.Fprintf(s.logSink, `{"event":"gate_isolation_selected","selection":%s}`+"\n", b)
		}
		if s.sel.PolicyWarning != "" {
			s.logEvent(`{"event":"gate_container_policy_warning","source":%q,"warning":%q}`, s.sel.ImageSource, s.sel.PolicyWarning)
		}
		if s.sel.DeclaredUnhonored != "" {
			s.logEvent(`{"event":"gate_container_unhonored","source":%q,"path":%q,"detail":%q}`, s.sel.ImageSource, s.sel.Path, s.sel.DeclaredUnhonored)
		}
		if len(s.services) > 0 && s.sel.Path != gateiso.PathContainer {
			// Only the container path provisions services; elsewhere the
			// gate's own tooling (pgtest's host testcontainers) is unchanged.
			s.logEvent(`{"event":"gate_services_ignored","services":%q,"path":%q}`, joinServices(s.services), s.sel.Path)
		}
	})
	return s.sel
}

// markSeamReached records that a gate reached the exec seam with the
// selection already decided. Called by runBoundedGateArgvDisposed right
// after selection(), so the Store follows the once.Do write of s.sel and a
// Load()==true reader observes the final selection. Nil receiver: no-op (the
// unconfigured host exec records nothing).
func (s *gateIsolationState) markSeamReached() {
	if s == nil {
		return
	}
	s.seamReached.Store(true)
}

// recordedSelection returns the selection a gate actually ran under, and
// false when no gate reached the seam (a plan stage, the working-tree
// runVerifyGate, a nil state). It never triggers detection. For a declared
// gate_container it also carries the image of the FINAL DECIDING gate — the
// last verify gate to reach the container path (nil when that gate's
// resolution failed), else the most recent gate's — and, when the stage's
// gates ran in more than one distinct image, that count.
func (s *gateIsolationState) recordedSelection() (gateiso.Selection, bool) {
	if s == nil || !s.seamReached.Load() {
		return gateiso.Selection{}, false
	}
	sel := s.sel
	s.resolvedMu.Lock()
	defer s.resolvedMu.Unlock()
	img := s.last
	if s.decidingSeen {
		img = s.deciding
	}
	if img != nil {
		c := *img
		sel.ResolvedImage = &c
	}
	if n := len(s.identities); n > 1 {
		sel.DistinctImagesCount = n
	}
	return sel, true
}

// decidingGateKey marks a gate exec's context as a VERIFY gate — the kind
// whose outcome decides the push or the failure (runVerifyCommittedTree) —
// as opposed to the diff-coverage measurement or the auto-format absorb.
type decidingGateKey struct{}

// withDecidingGate marks ctx as a deciding verify gate.
func withDecidingGate(ctx context.Context) context.Context {
	return context.WithValue(ctx, decidingGateKey{}, true)
}

// isDecidingGate reports whether ctx was marked by withDecidingGate.
func isDecidingGate(ctx context.Context) bool {
	v, _ := ctx.Value(decidingGateKey{}).(bool)
	return v
}

// noteDeclaredGate records that a declared-image gate started resolution: a
// deciding gate clears the deciding image, so a deciding gate whose
// resolution fails never inherits an earlier gate's image.
func (s *gateIsolationState) noteDeclaredGate(deciding bool) {
	if !deciding {
		return
	}
	s.resolvedMu.Lock()
	s.decidingSeen, s.deciding = true, nil
	s.resolvedMu.Unlock()
}

// recordResolved records the image one declared gate resolved to.
func (s *gateIsolationState) recordResolved(res gateiso.ResolvedImage, deciding bool) {
	s.resolvedMu.Lock()
	defer s.resolvedMu.Unlock()
	if s.identities == nil {
		s.identities = map[string]bool{}
	}
	s.identities[res.Identity()] = true
	c := res
	s.last = &c
	if deciding {
		s.deciding = &c
	}
}

// bindOwner records the run and stage this runner process serves (run()
// calls it once, right after installing the state). Every cache volume name
// embeds them, and an exec whose owner no longer matches the minted volume
// mints its own instead of reusing it (#3967 approval condition 2). Ids that
// are not UUIDs make every exec degrade to the per-exec caches. Nil-safe.
func (s *gateIsolationState) bindOwner(runID, stageID string) {
	if s == nil {
		return
	}
	s.cacheMu.Lock()
	s.ownerRun, s.ownerStage = runID, stageID
	s.cacheMu.Unlock()
}

// cacheUnavailable logs one degrade of the cache volume step and returns "".
func (s *gateIsolationState) cacheUnavailable(vol, step, reason string) string {
	s.logEvent(`{"event":"gate_cache_volume_unavailable","volume":%q,"step":%q,"reason":%q}`, vol, step, reason)
	return ""
}

// gateCacheVolume returns the cache volume the next container exec mounts,
// or "" for the per-exec caches: always "" for a nil state or mode off. In
// process mode it mints the volume ONCE per (state, owner) — recorded for
// cleanup BEFORE the create is attempted, so teardown covers a partial
// create — and re-mints when the minted volume no longer BelongsTo the bound
// owner (it is never reused across a (run, stage) pair). On EVERY call it
// then runs, through execGateAuxArgvFn under the bound CLI env, `volume
// create` (idempotent), the prepare helper (subdirectories owned by the gate
// uid) and the write check under the gate's own user pin (#3967 approval
// condition 1). Any render error, non-zero exit or timeout logs
// gate_cache_volume_unavailable naming the step and returns "" — a DEGRADE to
// the per-exec caches, never a refusal and never a gate verdict.
func (s *gateIsolationState) gateCacheVolume(ctx context.Context, rt gateiso.Runtime, image, root string, cliEnv []string, uid, gid int) string {
	if s == nil || s.cacheMode != gateiso.CacheModeProcess {
		return ""
	}
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	if s.cacheVol == nil || !s.cacheVol.BelongsTo(s.ownerRun, s.ownerStage) {
		v, err := gateiso.NewCacheVolume(s.ownerRun, s.ownerStage)
		if err != nil {
			return s.cacheUnavailable("", "mint", err.Error())
		}
		if s.cacheVol != nil {
			s.logEvent(`{"event":"gate_cache_volume_not_reused","volume":%q,"replacement":%q,"reason":"the volume belongs to a different run/stage"}`, s.cacheVol.Name, v.Name)
		}
		s.cacheVol, s.cacheUses = &v, 0
		s.cacheMinted = append(s.cacheMinted, mintedCacheVolume{vol: v, rt: rt, cliEnv: append([]string(nil), cliEnv...)})
	}
	vol := *s.cacheVol
	steps := []struct {
		name    string
		build   func() ([]string, error)
		timeout time.Duration
		failure string
	}{
		{"create", func() ([]string, error) { return vol.CreateArgv(rt) }, gateCacheVolumeTimeout, ""},
		{"prepare", func() ([]string, error) { return vol.PrepareArgv(rt, image, uid, gid) }, gateCachePrepareTimeout, ""},
		{"check", func() ([]string, error) { return vol.CheckArgv(rt, image, uid, gid) }, gateCacheCheckTimeout,
			fmt.Sprintf("ownership: the gate uid %d:%d cannot write %s / %s", uid, gid, gateiso.GateGoCache, gateiso.GateLintCache)},
	}
	for _, step := range steps {
		argv, err := step.build()
		if err != nil {
			return s.cacheUnavailable(vol.Name, step.name, "argv: "+err.Error())
		}
		out, code, timedOut := execGateAuxArgvFn(ctx, argv, root, cliEnv, step.timeout)
		if timedOut {
			return s.cacheUnavailable(vol.Name, step.name, fmt.Sprintf("timed out after %s", step.timeout))
		}
		if code != 0 {
			reason := fmt.Sprintf("exit %d: %s", code, gateOutputTail(out))
			if step.failure != "" {
				reason = step.failure + ": " + reason
			}
			return s.cacheUnavailable(vol.Name, step.name, reason)
		}
	}
	s.logEvent(`{"event":"gate_cache_volume_ready","volume":%q,"reused":%t}`, vol.Name, s.cacheUses > 0)
	s.cacheUses++
	return vol.Name
}

// removeCacheVolumes removes every cache volume the state minted, each on a
// fresh detached context bounded by diffCoverageCleanupTimeout under the
// runtime and bound CLI env it was minted with. A failure is logged
// (gate_cache_volume_cleanup_failed, naming the label a leak stays
// removable by) and never panics.
func (s *gateIsolationState) removeCacheVolumes() {
	s.cacheMu.Lock()
	minted := s.cacheMinted
	s.cacheMinted, s.cacheVol = nil, nil
	s.cacheMu.Unlock()
	for _, m := range minted {
		detail := ""
		if argv, err := m.vol.RemoveArgv(m.rt); err != nil {
			detail = "argv: " + err.Error()
		} else {
			ctx, cancel := context.WithTimeout(context.Background(), diffCoverageCleanupTimeout)
			out, code, _ := execGateAuxArgvFn(ctx, argv, os.TempDir(), m.cliEnv, diffCoverageCleanupTimeout)
			cancel()
			if code != 0 {
				detail = fmt.Sprintf("exit %d: %s", code, gateOutputTail(out))
			}
		}
		if detail != "" {
			s.logEvent(`{"event":"gate_cache_volume_cleanup_failed","volume":%q,"detail":%q,"label":%q}`, m.vol.Name, detail, gateiso.CacheVolumeLabel)
			continue
		}
		s.logEvent(`{"event":"gate_cache_volume_removed","volume":%q}`, m.vol.Name)
	}
}

// gateDockerConfig returns the docker config the container path's runtime
// calls are pinned to (E51.26 / #4046): the operator's
// FISHHAWK_GATE_DOCKER_CONFIG when configured, else the runner-owned
// anonymous config, minted ONCE per state on the first container exec — with
// the operator's CLI plugin dirs carried forward (gateiso.OperatorPluginDirs)
// — and reused by every later exec. A nil state has no cleanup to remove a
// minted dir, so it is an error.
func (s *gateIsolationState) gateDockerConfig() (gateiso.DockerConfig, error) {
	if s == nil {
		return gateiso.DockerConfig{}, errors.New("no gate isolation state to own a runner docker config")
	}
	s.dockerCfgMu.Lock()
	defer s.dockerCfgMu.Unlock()
	if s.dockerConfig == nil {
		cfg, err := newAnonymousDockerConfigFn(gateiso.OperatorPluginDirs(os.Getenv))
		if err != nil {
			return gateiso.DockerConfig{}, err
		}
		s.dockerConfig = &cfg
		s.logEvent(`{"event":"gate_docker_config_minted","dir":%q,"credentials":%q}`, cfg.Dir, cfg.Credentials)
	}
	return *s.dockerConfig, nil
}

// removeDockerConfig removes the runner-owned docker config dir, if one was
// minted; an operator config is never removed (DockerConfig.Remove is a
// no-op for it). A failure is logged and never panics.
func (s *gateIsolationState) removeDockerConfig() {
	s.dockerCfgMu.Lock()
	cfg := s.dockerConfig
	if cfg != nil && cfg.Owned() {
		s.dockerConfig = nil
	}
	s.dockerCfgMu.Unlock()
	if cfg == nil || !cfg.Owned() {
		return
	}
	if err := cfg.Remove(); err != nil {
		s.logEvent(`{"event":"gate_docker_config_cleanup_failed","dir":%q,"detail":%q}`, cfg.Dir, err.Error())
		return
	}
	s.logEvent(`{"event":"gate_docker_config_removed","dir":%q}`, cfg.Dir)
}

// cleanup releases the process-wide state: it first removes every cache
// volume the state minted (#3967) — under the bound CLI env, which still
// points at the runner-owned docker config — THEN that config dir (#4046),
// then clears the global. run() defers it so a later run() in the same
// process (the test binary) starts unconfigured. Nil-safe.
func (s *gateIsolationState) cleanup() {
	if s != nil {
		s.removeCacheVolumes()
		s.removeDockerConfig()
	}
	if gateIsolation == s {
		gateIsolation = nil
	}
}

// cachePosture names the cache posture one container exec ran under for the
// gate_container_timing line: process (the volume mounted), degraded (process
// mode, but a cache step failed) or off (mode off, or a nil state).
func (s *gateIsolationState) cachePosture(volume string) string {
	switch {
	case volume != "":
		return string(gateiso.CacheModeProcess)
	case s != nil && s.cacheMode == gateiso.CacheModeProcess:
		return "degraded"
	}
	return string(gateiso.CacheModeOff)
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

// runGateInContainer is the container branch of runBoundedGateArgv. In order:
// fresh empty visible caches; the lint-cache dir; the docker config the
// runtime calls are pinned to (gateDockerConfig, E51.26 / #4046: the
// runner-owned anonymous config, or the operator's FISHHAWK_GATE_DOCKER_CONFIG);
// the runtime CLI's env BOUND to the validated endpoint AND that config (the
// runner's inherited environment — the CLI needs PATH — with DOCKER_HOST /
// DOCKER_CONTEXT / CONTAINER_HOST / CONTAINER_CONNECTION / DOCKER_CONFIG /
// REGISTRY_AUTH_FILE dropped and the selection's socket and the chosen config
// re-pinned; every argv carries the same endpoint binding as a global flag,
// so a docker-context switch between gates cannot redirect any runtime call
// to a daemon the selection never validated, and no runtime call reaches the
// operator's interactive credential store); under an operator config the
// credential probe of every helper the gate and service images match
// (probeGateCredentials — a blocked helper is gateUnavailable with
// container_credentials_blocked, before any pull); for a DECLARED gate_container (E51.3
// / #2136) the image resolution (resolveDeclaredImage: explicit pull or
// in-repo build, a refused build source/Dockerfile/base gateRefused and any
// operational failure gateUnavailable, the resolved image recorded for the
// evidence) — the FISHHAWK_GATE_IMAGE path skips it; when FISHHAWK_GATE_SERVICES names
// postgres, a fresh per-exec gate service and its argv (#2137); the caller's
// passwd file (gatePasswdFile — a DEGRADE, never a refusal); under
// FISHHAWK_GATE_CACHE=process the per-process cache volume for the resolved
// image (gateCacheVolume: create + prepare + write check on every exec, a
// DEGRADE to the per-exec GOCACHE / lint-cache binds on any failure, #3967)
// with its env applied before the service env; the argv build
// under the resolved-path mount guard (BEFORE the seed — a checkout the guard
// refuses is never handed to the host-side seed); the host-side module-cache
// seed under the SANITIZED gate env (no runner credential reaches the
// `go mod download`, and the checkout's module metadata is refused before any
// go process runs when it would reach outside the checkout); the gate service
// provisioning (provisionGateService), whose teardown is DEFERRED from the
// moment `volume create` is attempted so it runs on EVERY later exit; the
// exec through the host seam (the sanitized gate env crosses into the
// container via -e only, with FISHHAWK_GATE_CONTAINER=1 pinned and the
// service DSN applied LAST); and `rm -f` — under the same binding — on a
// detached context when the exec returned -1 (killing the CLI does not stop
// the container). Every failure before the exec returns -1 WITHOUT executing
// the gate, and the third value names WHY out of band (#3448): a HOST-caused
// pre-exec failure — visible-cache creation, the lint-cache dir, the docker
// config, endpoint binding, a blocked credential helper, the gate-service argv, the mount-guard refusal, a seed failure NOT
// wrapping gateiso.ErrSeedCheckout (the host GOMODCACHE probe or `go mod
// download` on an offline host), or ANY gate-service provisioning step — is
// gateUnavailable (category C at the gates, like a refusal; never the fix
// agent); a seed failure wrapping ErrSeedCheckout is the checkout's OWN
// metadata being refused, so it is gateCheckoutRefused (tree-attributable,
// classified as an executed failure); the exec path is gateExecuted whatever
// the exit code, EXCEPT that a seam result reporting the runner's own
// deadline expiry is gateTimedOut (#3383, no verdict). A gate-service
// teardown failure is logged (gate_service_cleanup_failed) and never changes
// the gate's verdict. Every exec that reaches the seam logs one
// gate_container_timing line splitting cache / seed / service / exec time.
// Deliberate residual: a legitimate tree whose `replace`
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
	// The docker config every runtime call below is pinned to (#4046): never
	// the operator's interactive one, whose credsStore can block a pull
	// behind a locked screen.
	dockerCfg, err := st.gateDockerConfig()
	if err != nil {
		return "gate container: docker config: " + err.Error(), -1, gateUnavailable
	}
	// The runtime CLI's env is the runner's inherited environment with the
	// endpoint bound to the socket the selection validated (concern: a
	// context switch after selection must not redirect launch, the passwd
	// read, the service lifecycle or cleanup) and the config pinned to
	// dockerCfg. Bound FIRST: no runtime call below runs under an unbound env.
	cliEnv, err := bindEndpointEnvFn(sel.Runtime, os.Environ(), dockerCfg.Dir)
	if err != nil {
		return "gate container: " + err.Error(), -1, gateUnavailable
	}
	// Probe site (a): before the resolution, the passwd read, the cache
	// volume and the service — every one of which can pull — probe each
	// operator-config helper the gate image (by reference) and the postgres
	// service image match. The anonymous config matches none.
	var pulled []string
	if sel.Image != "" {
		pulled = append(pulled, sel.Image)
	}
	if st.postgresServiceWanted() {
		pulled = append(pulled, st.postgresImage)
	}
	if reason := st.probeGateCredentials(ctx, dockerCfg, pulled, cliEnv, "gate"); reason != "" {
		return "gate container: " + reason, -1, gateUnavailable
	}
	// A DECLARED gate_container (E51.3 / #2136) resolves to an image on the
	// host first — explicit pull or in-repo build, each under its own bound
	// and the bound CLI env (the pinned docker config's credentials; Fishhawk
	// passes no credential). The FISHHAWK_GATE_IMAGE path skips this and runs
	// sel.Image byte-unchanged (its implicit pull stays inside the gate run).
	image := sel.Image
	if gateiso.DeclaredSource(sel.ImageSource) {
		deciding := isDecidingGate(ctx)
		st.noteDeclaredGate(deciding)
		res, how, msg, disp := st.resolveDeclaredImage(ctx, sel.Runtime, dir, vc.Root, cliEnv, dockerCfg)
		if msg != "" {
			return msg, -1, disp
		}
		st.recordResolved(res, deciding)
		image = res.Ref
		st.logEvent(`{"event":"gate_container_resolved","source":%q,"how":%q,"ref":%q,"digest":%q,"image_id":%q,"context_digest":%q,"deciding":%t}`,
			sel.ImageSource, how, res.Ref, res.Digest, res.ImageID, res.ContextDigest, deciding)
	}
	uid, gid := os.Getuid(), os.Getgid()
	if st != nil {
		uid, gid = st.uid, st.gid
	}
	env := gateiso.ContainerEnv(sanitizedEnv, extraEnv)
	passwdFile := st.gatePasswdFile(ctx, sel.Runtime, image, vc.Root, cliEnv, uid, gid)
	// The per-process cache volume (#3967) for the RESOLVED image — a
	// declared gate_container's name@digest or FISHHAWK_GATE_IMAGE alike. ""
	// keeps the per-exec GOCACHE / lint-cache binds exactly as before.
	cacheStart := time.Now()
	cacheVolume := st.gateCacheVolume(ctx, sel.Runtime, image, vc.Root, cliEnv, uid, gid)
	cacheElapsed := time.Since(cacheStart)
	if cacheVolume != "" {
		// Cache env before the service env, which stays LAST.
		env = gateiso.WithCacheVolumeEnv(env)
	}
	var svc *gateiso.PostgresService
	var svcArgv gateServiceArgv
	if st.postgresServiceWanted() {
		s := gateiso.NewPostgresService(st.postgresImage)
		if svcArgv, err = buildGateServiceArgv(s, sel.Runtime); err != nil {
			return "gate container: provision postgres service: argv: " + err.Error(), -1, gateUnavailable
		}
		svc = &s
		// Service env LAST: neither the sanitized env nor extras can
		// redirect the DSN the runner provisioned.
		env = gateiso.WithServiceEnv(env, s.GateEnv())
	}
	spec := gateiso.ContainerSpec{
		Runtime:    sel.Runtime,
		Image:      image,
		Name:       gateiso.NewContainerName(),
		Checkout:   dir,
		GoModCache: vc.GoModCache,
		PasswdFile: passwdFile,
		Env:        env,
		Argv:       argv,
		UID:        uid,
		GID:        gid,
	}
	if cacheVolume != "" {
		spec.CacheVolume = cacheVolume
	} else {
		spec.GoCache, spec.LintCache = vc.GoCache, lintCacheDir
	}
	if svc != nil {
		spec.ServiceMounts = []gateiso.ServiceMount{svc.GateMount()}
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
	seedStart := time.Now()
	_, err = seedModCacheFn(ctx, nil, dir, "", vc, sanitizedEnv, gateSeedTimeout)
	seedElapsed := time.Since(seedStart)
	if err != nil {
		disp := gateUnavailable
		if errors.Is(err, gateiso.ErrSeedCheckout) {
			// The checkout's OWN metadata was refused: tree-attributable.
			disp = gateCheckoutRefused
		}
		return "gate container: seed module cache: " + err.Error(), -1, disp
	}
	var serviceElapsed time.Duration
	if svc != nil {
		// Registered BEFORE `volume create` is attempted, so a partial
		// provision, a failed gate and a timed-out gate all tear down.
		defer teardownGateService(ctx, st, svc.Name, svcArgv, vc.Root, cliEnv)
		serviceStart := time.Now()
		msg, ok := provisionGateService(ctx, svcArgv, vc.Root, cliEnv)
		serviceElapsed = time.Since(serviceStart)
		if !ok {
			return "gate container: provision postgres service: " + msg, -1, gateUnavailable
		}
		st.logEvent(`{"event":"gate_service_provisioned","service":%q,"volume":%q,"image":%q}`, svc.Name, svc.Name, svc.Image)
		if gateServiceObserver != nil {
			gateServiceObserver(ctx, sel.Runtime, cliEnv, *svc)
		}
	}
	execStart := time.Now()
	out, code, timedOut := execBoundedHostArgvFn(ctx, runArgv, dir, cliEnv, timeout)
	// One phase split per container exec (#3967): provisioning stays outside
	// the gate timeout, so exec_ms alone is what the gate's timeout bounds.
	st.logEvent(`{"event":"gate_container_timing","cache":%q,"cache_volume":%q,"cache_ms":%d,"seed_ms":%d,"service_ms":%d,"exec_ms":%d,"exit_code":%d}`,
		st.cachePosture(cacheVolume), cacheVolume, cacheElapsed.Milliseconds(), seedElapsed.Milliseconds(),
		serviceElapsed.Milliseconds(), time.Since(execStart).Milliseconds(), code)
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

// gateContainerUnavailable / gateContainerRefused render a declared
// gate_container resolution failure. A refusal leads with
// gateIsolationRefusedSignature like every selection refusal (operator TEXT;
// classification reads the disposition).
func gateContainerUnavailable(format string, args ...any) (gateiso.ResolvedImage, string, string, gateDisposition) {
	return gateiso.ResolvedImage{}, "", "gate container: gate_container unavailable: " + fmt.Sprintf(format, args...), gateUnavailable
}

func gateContainerRefused(format string, args ...any) (gateiso.ResolvedImage, string, string, gateDisposition) {
	return gateiso.ResolvedImage{}, "", gateIsolationRefusedSignature + " gate_container build refused: " + fmt.Sprintf(format, args...), gateRefused
}

// resolveDeclaredImage resolves the declared gate_container to the image the
// gate runs by. It returns the resolution, how it was reached
// (present|pulled|cache_hit|built), and — on failure — a non-empty message
// with its disposition: a build source, Dockerfile or base the runner refuses
// is gateRefused BEFORE any build call; every operational failure (inspect,
// pull, digest, git read, context size, build) is gateUnavailable. Neither
// reaches the fix agent, and neither substitutes FISHHAWK_GATE_IMAGE.
func (s *gateIsolationState) resolveDeclaredImage(ctx context.Context, rt gateiso.Runtime, dir, root string, cliEnv []string, dockerCfg gateiso.DockerConfig) (gateiso.ResolvedImage, string, string, gateDisposition) {
	if s.decision.Build {
		return s.resolveBuiltImage(ctx, rt, dir, root, cliEnv, dockerCfg)
	}
	return s.resolvePulledImage(ctx, rt, root, cliEnv)
}

// inspectImage runs `image inspect` on ref under gateImageInspectTimeout and
// parses it; ok is false when the runtime reports no such image (or failed).
func inspectImage(ctx context.Context, rt gateiso.Runtime, ref, root string, cliEnv []string) (insp gateiso.ImageInspect, ok bool, err error) {
	argv, err := rt.InspectArgv(ref)
	if err != nil {
		return gateiso.ImageInspect{}, false, err
	}
	out, code, _ := execGateAuxArgvFn(ctx, argv, root, cliEnv, gateImageInspectTimeout)
	if code != 0 {
		return gateiso.ImageInspect{}, false, fmt.Errorf("image inspect %s: exit %d: %s", ref, code, gateOutputTail(out))
	}
	insp, err = gateiso.ParseInspect(out)
	if err != nil {
		return gateiso.ImageInspect{}, false, err
	}
	return insp, true, nil
}

// resolvePulledImage resolves a declared `image:`. A digest-pinned ref is
// inspected and pulled only when absent; a tag-only ref is ALWAYS pulled.
// Either way the gate runs by name@<registry digest> from the inspected
// RepoDigests, so a concurrent retag cannot change what runs; a pinned ref
// must find its own digest there. A pull failure carries the named
// local-only remedy (gateiso.PullFailedReason) and, under the anonymous
// posture, the FISHHAWK_GATE_DOCKER_CONFIG remedy for a private registry.
func (s *gateIsolationState) resolvePulledImage(ctx context.Context, rt gateiso.Runtime, root string, cliEnv []string) (gateiso.ResolvedImage, string, string, gateDisposition) {
	ref := s.decision.Ref
	target := ref.String()
	if ref.Pinned() {
		target = ref.Name() + "@" + ref.Digest
	}
	how := "present"
	insp, ok, ierr := gateiso.ImageInspect{}, false, error(nil)
	if ref.Pinned() {
		insp, ok, _ = inspectImage(ctx, rt, target, root, cliEnv)
	}
	if !ok {
		how = "pulled"
		argv, err := rt.PullArgv(target)
		if err != nil {
			return gateContainerUnavailable("pull argv for %s: %v", ref, err)
		}
		out, code, timedOut := execGateAuxArgvFn(ctx, argv, root, cliEnv, gateImagePullTimeout)
		if code != 0 {
			cause := fmt.Errorf("exit %d: %s", code, gateOutputTail(out))
			if timedOut {
				cause = fmt.Errorf("timed out after %s", gateImagePullTimeout)
			}
			return gateContainerUnavailable("%s%s", gateiso.PullFailedReason(ref, cause), s.anonymousPullHint())
		}
		if insp, ok, ierr = inspectImage(ctx, rt, target, root, cliEnv); !ok {
			return gateContainerUnavailable("inspect %s after pull: %v", ref, ierr)
		}
	}
	if ref.Pinned() {
		if !hasRepoDigest(ref, insp.RepoDigests) {
			return gateContainerUnavailable("image %s is present but its registry digests %q do not include the pinned %s", ref, insp.RepoDigests, ref.Digest)
		}
		return gateiso.ResolvedImage{Ref: target, Digest: ref.Digest, ImageID: insp.ID}, how, "", gateExecuted
	}
	pinned, found := gateiso.RepoDigestFor(ref, insp.RepoDigests)
	if !found {
		return gateContainerUnavailable("image %s has no registry digest after the pull, so the gate cannot run it by digest: a declared `image:` must be pullable from a registry; for a local-only image declare `dockerfile` + `context`, or have the operator set FISHHAWK_GATE_IMAGE", ref)
	}
	_, digest, _ := strings.Cut(pinned, "@")
	return gateiso.ResolvedImage{Ref: pinned, Digest: digest, ImageID: insp.ID}, how, "", gateExecuted
}

// anonymousPullHint is appended to a pull failure under the anonymous
// posture (#4046 approval condition 1): the runner pulled with a config that
// carries no credential, so a private registry's image needs the operator
// opt-in. "" under an operator config, whose credentials were offered.
func (s *gateIsolationState) anonymousPullHint() string {
	if s.credentials != gateiso.CredentialsAnonymous {
		return ""
	}
	return "; the runner pulls with a credential-free docker config (no credsStore, no credHelpers), so an image on a private registry needs " +
		gateDockerConfigEnvVar + " set to a docker config dir holding that registry's credentials"
}

// probeGateCredentials probes, under the bound CLI env, every credential
// helper dockerCfg would invoke for images (E51.26 / #4046: the CLI's own
// store choice, gateiso.DockerConfig.ProbesFor — an unmatched helper is never
// run) and returns the blocked reason of the first that does not answer
// within gateCredentialProbeTimeout ("" when none blocks). The anonymous
// config matches no helper, so it probes nothing. Each probe logs one
// gate_credentials_probe line naming the site, helper, server and outcome —
// never the helper's output. Probes run on EVERY exec, uncached: a screen
// can lock between two gates.
func (s *gateIsolationState) probeGateCredentials(ctx context.Context, dockerCfg gateiso.DockerConfig, images, cliEnv []string, site string) string {
	for _, p := range dockerCfg.ProbesFor(images) {
		res := probeCredentialHelperFn(ctx, p, cliEnv, gateCredentialProbeTimeout)
		s.logEvent(`{"event":"gate_credentials_probe","site":%q,"helper":%q,"server":%q,"outcome":%q,"detail":%q,"elapsed_ms":%d}`,
			site, p.Executable(), p.Server, res.Outcome, res.Detail, res.Elapsed.Milliseconds())
		if res.Outcome == gateiso.ProbeBlocked {
			return res.BlockedReason()
		}
	}
	return ""
}

// hasRepoDigest reports whether any inspected RepoDigest names ref's
// repository at ref's own digest.
func hasRepoDigest(ref gateiso.ImageRef, digests []string) bool {
	for _, d := range digests {
		if r, err := gateiso.ParseImageRef(d); err == nil && r.Name() == ref.Name() && r.Digest == ref.Digest {
			return true
		}
	}
	return false
}

// resolveBuiltImage resolves a declared dockerfile/context build from the
// COMMITTED tree at the gate checkout's HEAD — never the working tree, so an
// uncommitted or untracked file cannot enter a gate image, and every gate
// kind on one commit computes the same content digest. In order: pin the
// source to git objects (gateiso.ResolveBuildSource), screen the committed
// Dockerfile bytes (parse refusals, the profile's cache-mount rule, the
// allowlist/pinning rule for every base) — every refusal before ANY build
// call — then inspect the content-addressed tag (a hit skips the build),
// else probe the operator-config credential helpers every Dockerfile base
// matches (probe site (b), #4046: a blocked helper is gateUnavailable BEFORE
// any build call), materialize the committed context into a throwaway dir
// and build it with --network=none.
func (s *gateIsolationState) resolveBuiltImage(ctx context.Context, rt gateiso.Runtime, dir, root string, cliEnv []string, dockerCfg gateiso.DockerConfig) (gateiso.ResolvedImage, string, string, gateDisposition) {
	bctx, cancel := context.WithTimeout(ctx, gateImageBuildTimeout)
	defer cancel()
	git := gateiso.ExecGit(dir)
	sourceFailure := func(what string, err error) (gateiso.ResolvedImage, string, string, gateDisposition) {
		if errors.Is(err, gateiso.ErrBuildSourceRefused) {
			return gateContainerRefused("%s: %v", what, err)
		}
		return gateContainerUnavailable("%s: %v", what, err)
	}
	src, err := gateiso.ResolveBuildSource(bctx, git, "HEAD", s.req.Dockerfile, s.req.Context)
	if err != nil {
		return sourceFailure("resolve build source", err)
	}
	content, err := gateiso.ReadDockerfile(bctx, git, src)
	if err != nil {
		return sourceFailure("read "+src.Dockerfile, err)
	}
	if err := gateiso.ScreenDockerfile(content, s.profile, s.allowlist, s.decision); err != nil {
		return gateContainerRefused("%s at %s: %v", src.Dockerfile, src.Commit, err)
	}
	res := gateiso.ResolvedImage{Ref: src.Tag(), BuildDockerfile: src.Dockerfile, BuildContext: src.Context, ContextDigest: src.Digest}
	if insp, ok, _ := inspectImage(ctx, rt, res.Ref, root, cliEnv); ok {
		res.ImageID = insp.ID
		return res, "cache_hit", "", gateExecuted
	}
	// ScreenDockerfile above parsed the same bytes, so this cannot fail.
	if df, err := gateiso.ParseDockerfile(content); err == nil {
		bases := make([]string, len(df.Bases))
		for i, b := range df.Bases {
			bases[i] = b.Ref
		}
		if reason := s.probeGateCredentials(ctx, dockerCfg, bases, cliEnv, "build"); reason != "" {
			return gateContainerUnavailable("%s", reason)
		}
	}
	tmp, err := os.MkdirTemp("", "fishhawk-gate-build-*")
	if err != nil {
		return gateContainerUnavailable("build dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	layout, err := gateiso.MaterializeBuildContext(bctx, git, src, filepath.Join(tmp, "src"), gateBuildContextLimits)
	if err != nil {
		return sourceFailure("materialize build context", err)
	}
	argv, err := rt.BuildArgv(gateiso.BuildSpec{Dockerfile: layout.Dockerfile, Context: layout.Context, Digest: src.Digest})
	if err != nil {
		return gateContainerUnavailable("build argv: %v", err)
	}
	out, code, timedOut := execGateAuxArgvFn(ctx, argv, root, cliEnv, gateImageBuildTimeout)
	if code != 0 {
		if timedOut {
			return gateContainerUnavailable("build of %s timed out after %s", src.Dockerfile, gateImageBuildTimeout)
		}
		return gateContainerUnavailable("build of %s failed: exit %d: %s", src.Dockerfile, code, gateOutputTail(out))
	}
	insp, ok, ierr := inspectImage(ctx, rt, res.Ref, root, cliEnv)
	if !ok {
		return gateContainerUnavailable("inspect %s after build: %v", res.Ref, ierr)
	}
	res.ImageID = insp.ID
	return res, "built", "", gateExecuted
}

// gateCallerName is the host login name the passwd entry carries
// (gateiso.BuildPasswd falls back to fishhawk-gate when it is unusable).
func gateCallerName() string {
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return ""
}

// gatePasswdFile returns a FRESH per-exec passwd file under root (#2137
// approval condition 7) naming the caller uid, mounted read-only at
// /etc/passwd on EVERY container exec so `id -un`, git and libpq resolve the
// caller. Any failure — the image read or the write — DEGRADES to "" (no
// passwd mount) with a gate_passwd_unavailable log line; it never refuses the
// gate. Only a successful image read is cached (per image, per process).
func (s *gateIsolationState) gatePasswdFile(ctx context.Context, rt gateiso.Runtime, image, root string, cliEnv []string, uid, gid int) string {
	content, err := s.imagePasswd(ctx, rt, image, root, cliEnv, uid, gid)
	if err == nil {
		var path string
		if path, err = writePasswdFileFn(root, gateiso.BuildPasswd(content, uid, gid, gateCallerName())); err == nil {
			return path
		}
	}
	s.logEvent(`{"event":"gate_passwd_unavailable","image":%q,"reason":%q}`, image, err.Error())
	return ""
}

// imagePasswd reads the gate image's /etc/passwd through the hardened,
// endpoint-bound gateiso.PasswdReadArgv (no network, all capabilities
// dropped, no-new-privileges, the caller's user pin), serving a cached
// SUCCESSFUL read when there is one. The seam returns the runtime CLI's
// COMBINED stdout and stderr, so only gateiso.WellFormedPasswd's entries are
// kept (a cold pull's progress lines exit 0 alongside the file); an output
// with no entry at all is a failed read. A failed read is not cached.
func (s *gateIsolationState) imagePasswd(ctx context.Context, rt gateiso.Runtime, image, root string, cliEnv []string, uid, gid int) ([]byte, error) {
	if s != nil {
		s.passwdMu.Lock()
		cached, ok := s.passwdByImage[image]
		s.passwdMu.Unlock()
		if ok {
			return cached, nil
		}
	}
	argv, err := gateiso.PasswdReadArgv(rt, image, uid, gid)
	if err != nil {
		return nil, err
	}
	out, code, _ := execGateAuxArgvFn(ctx, argv, root, cliEnv, gatePasswdReadTimeout)
	if code != 0 {
		return nil, fmt.Errorf("read %s /etc/passwd: exit %d: %s", image, code, gateOutputTail(out))
	}
	content := gateiso.WellFormedPasswd([]byte(out))
	if len(content) == 0 {
		return nil, fmt.Errorf("read %s /etc/passwd: no well-formed passwd entry in the output: %s", image, gateOutputTail(out))
	}
	if s != nil {
		s.passwdMu.Lock()
		if s.passwdByImage == nil {
			s.passwdByImage = map[string][]byte{}
		}
		s.passwdByImage[image] = content
		s.passwdMu.Unlock()
	}
	return content, nil
}

// gateServiceArgv is every runtime command line of one gate service's
// lifecycle, rendered (and validated) before any of them executes.
type gateServiceArgv struct {
	volumeCreate, run, logs, ready, bootstrap, remove, volumeRemove []string
}

// buildGateServiceArgv renders the lifecycle argv; any refusal (an unbound
// runtime, a malformed name, a flag-shaped image) renders none.
func buildGateServiceArgv(svc gateiso.PostgresService, rt gateiso.Runtime) (gateServiceArgv, error) {
	var a gateServiceArgv
	for _, b := range []struct {
		dst   *[]string
		build func(gateiso.Runtime) ([]string, error)
	}{
		{&a.volumeCreate, svc.VolumeCreateArgv},
		{&a.run, svc.RunArgv},
		{&a.logs, svc.LogsArgv},
		{&a.ready, svc.ReadyArgv},
		{&a.bootstrap, svc.BootstrapArgv},
		{&a.remove, svc.RemoveArgv},
		{&a.volumeRemove, svc.VolumeRemoveArgv},
	} {
		argv, err := b.build(rt)
		if err != nil {
			return gateServiceArgv{}, err
		}
		*b.dst = argv
	}
	return a, nil
}

// provisionGateService creates the socket volume, starts the service, waits
// for readiness and bootstraps the least-privilege gate role. It returns
// ("<step>: <detail>", false) on the first failing step. Readiness (#2137
// approval condition 3): each iteration reads the service logs FIRST and runs
// pg_isready only once the image's init-complete line was seen — the
// temporary init server answers pg_isready before init completes — and the
// service is ready only when pg_isready succeeds after that. A readiness
// failure carries the tail of the last service-log read (or its failure) as
// well as the last pg_isready output, because the deferred teardown removes
// the container and its logs with it. A parent cancellation ends the poll at
// once — also while it is parked on the interval timer — as a readiness
// failure, so provisioning never continues to the bootstrap or the gate.
func provisionGateService(ctx context.Context, a gateServiceArgv, root string, cliEnv []string) (string, bool) {
	if out, code, _ := execGateAuxArgvFn(ctx, a.volumeCreate, root, cliEnv, gateServiceVolumeTimeout); code != 0 {
		return fmt.Sprintf("volume create: exit %d: %s", code, gateOutputTail(out)), false
	}
	if out, code, _ := execGateAuxArgvFn(ctx, a.run, root, cliEnv, gateServiceStartTimeout); code != 0 {
		return fmt.Sprintf("run: exit %d: %s", code, gateOutputTail(out)), false
	}
	deadline := time.Now().Add(gateServiceReadyTimeout)
	initSeen, last, lastLogs := false, "", ""
	notReady := func() string {
		why := fmt.Sprintf("not ready within %s", gateServiceReadyTimeout)
		if err := ctx.Err(); err != nil {
			why = "cancelled before ready: " + err.Error()
		}
		ready := "pg_isready not run"
		if initSeen {
			ready = "last pg_isready: " + gateOutputTail(last)
		}
		return fmt.Sprintf("readiness: %s (init-complete line seen: %t): %s; last service logs: %s",
			why, initSeen, ready, gateOutputTail(lastLogs))
	}
	for {
		logs, code, _ := execGateAuxArgvFn(ctx, a.logs, root, cliEnv, gateServiceProbeTimeout)
		lastLogs = logs
		if code != 0 {
			lastLogs = fmt.Sprintf("logs exit %d: %s", code, logs)
		}
		if code == 0 && gateiso.PostgresInitComplete(logs) {
			initSeen = true
			out, rc, _ := execGateAuxArgvFn(ctx, a.ready, root, cliEnv, gateServiceProbeTimeout)
			if rc == 0 {
				break
			}
			last = out
		}
		if ctx.Err() != nil || !time.Now().Before(deadline) {
			return notReady(), false
		}
		t := time.NewTimer(gateServiceReadyInterval)
		select {
		case <-ctx.Done():
			// Cancelled while parked: stop here rather than run another
			// iteration whose probes could still answer.
			t.Stop()
			return notReady(), false
		case <-t.C:
		}
	}
	if out, code, _ := execGateAuxArgvFn(ctx, a.bootstrap, root, cliEnv, gateServiceBootstrapTimeout); code != 0 {
		return fmt.Sprintf("bootstrap: exit %d: %s", code, gateOutputTail(out)), false
	}
	return "", true
}

// teardownGateService removes the service container (with its anonymous
// volumes) and then the socket volume, on a detached context under the same
// endpoint binding, each bounded by diffCoverageCleanupTimeout. A failure is
// logged and never changes the gate's verdict; a leak stays removable by the
// gateiso.ServiceLabel label.
func teardownGateService(ctx context.Context, st *gateIsolationState, name string, a gateServiceArgv, root string, cliEnv []string) {
	cleanupCtx := context.WithoutCancel(ctx)
	var failed []string
	for _, step := range []struct {
		what string
		argv []string
	}{{"rm", a.remove}, {"volume rm", a.volumeRemove}} {
		if out, code, _ := execGateAuxArgvFn(cleanupCtx, step.argv, root, cliEnv, diffCoverageCleanupTimeout); code != 0 {
			failed = append(failed, fmt.Sprintf("%s: exit %d: %s", step.what, code, gateOutputTail(out)))
		}
	}
	if len(failed) > 0 {
		st.logEvent(`{"event":"gate_service_cleanup_failed","service":%q,"volume":%q,"detail":%q,"label":%q}`,
			name, name, strings.Join(failed, "; "), gateiso.ServiceLabel)
		return
	}
	st.logEvent(`{"event":"gate_service_removed","service":%q,"volume":%q}`, name, name)
}

// gateOutputTail bounds runtime CLI output carried into an error or log line.
func gateOutputTail(out string) string {
	const limit = 2048
	out = strings.TrimSpace(out)
	if len(out) > limit {
		out = "…" + out[len(out)-limit:]
	}
	return out
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
