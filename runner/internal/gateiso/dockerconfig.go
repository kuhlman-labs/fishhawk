package gateiso

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// Runtime CLI credentials (E51.26 / #4046). Every runtime call the runner
// makes after selection is pinned (BindEndpointEnv) to a docker config the
// RUNNER chose, never the operator's interactive one: a credsStore such as
// Docker Desktop's `desktop` helper blocks every pull while the macOS screen
// is locked, even an anonymous pull of a public image, because the docker
// CLI consults the store for every registry it talks to. By default that
// config is a runner-owned temp dir with NO credsStore and NO credHelpers
// (NewAnonymousDockerConfig); FISHHAWK_GATE_DOCKER_CONFIG opts into an
// operator-prepared one (LoadOperatorDockerConfig), whose matching helpers
// the runner probes under a bound before any pull (ProbeCredentialHelper).

// Credentials is the credential posture of the runtime CLI calls, recorded on
// the selection (Selection.Credentials) and in the gate_isolation evidence.
type Credentials string

const (
	// CredentialsAnonymous is the runner-owned config, which carries no
	// credential and invokes no credential helper.
	CredentialsAnonymous Credentials = "anonymous"
	// CredentialsOperatorConfig is the operator's FISHHAWK_GATE_DOCKER_CONFIG.
	CredentialsOperatorConfig Credentials = "operator_config"
)

// dockerConfigFile is the file name the docker CLI reads inside DOCKER_CONFIG.
const dockerConfigFile = "config.json"

// dockerHubAuthKey is the key the docker CLI resolves Docker Hub credentials
// by (registry.IndexServer); every other registry is keyed by its host.
const dockerHubAuthKey = "https://index.docker.io/v1/"

// helperNamePattern bounds a credsStore / credHelpers value: it is spliced
// into the executable name docker-credential-<name>, so a path separator or
// any other shell-relevant byte is refused at startup.
var helperNamePattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// DockerConfig is the docker CLI config dir every post-selection runtime call
// is pinned to. Only a config the runner minted (owned) is ever removed.
type DockerConfig struct {
	// Dir is the absolute config dir (DOCKER_CONFIG).
	Dir string
	// Credentials is the posture the dir carries.
	Credentials Credentials
	// owned marks a runner-minted dir: Remove deletes it. An operator config
	// is never owned.
	owned bool
	// credsStore / credHelpers are the operator config's store selection,
	// read by ProbesFor. Both empty for the anonymous config.
	credsStore  string
	credHelpers map[string]string
}

// ConfigFile is the config.json inside Dir (podman's REGISTRY_AUTH_FILE).
func (c DockerConfig) ConfigFile() string { return filepath.Join(c.Dir, dockerConfigFile) }

// Owned reports whether the runner minted the dir (and Remove deletes it).
func (c DockerConfig) Owned() bool { return c.owned }

// Remove deletes a runner-minted dir and is a no-op for an operator config:
// the runner never deletes a directory it did not create.
func (c DockerConfig) Remove() error {
	if !c.owned || c.Dir == "" {
		return nil
	}
	return os.RemoveAll(c.Dir)
}

// anonymousConfig is the whole content of the runner-owned config.json: an
// empty auths map, plus the plugin dirs so `docker buildx` (a CLI plugin the
// docker CLI looks up under DOCKER_CONFIG/cli-plugins) still resolves.
type anonymousConfig struct {
	Auths               map[string]any `json:"auths"`
	CliPluginsExtraDirs []string       `json:"cliPluginsExtraDirs,omitempty"`
}

// NewAnonymousDockerConfig mints the runner-owned config: a fresh temp dir
// (mode 0700) holding a config.json (mode 0600) with an empty auths map, NO
// credsStore and NO credHelpers, and pluginDirs as cliPluginsExtraDirs. The
// caller removes it with Remove; a failed mint removes what it created.
func NewAnonymousDockerConfig(pluginDirs []string) (DockerConfig, error) {
	dir, err := os.MkdirTemp("", "fishhawk-gate-docker-config-*")
	if err != nil {
		return DockerConfig{}, fmt.Errorf("create runner docker config dir: %w", err)
	}
	cfg := DockerConfig{Dir: dir, Credentials: CredentialsAnonymous, owned: true}
	b, err := json.Marshal(anonymousConfig{Auths: map[string]any{}, CliPluginsExtraDirs: pluginDirs})
	if err == nil {
		err = os.Chmod(dir, 0o700)
	}
	if err == nil {
		err = os.WriteFile(cfg.ConfigFile(), b, 0o600)
	}
	if err != nil {
		_ = cfg.Remove()
		return DockerConfig{}, fmt.Errorf("write runner docker config: %w", err)
	}
	return cfg, nil
}

// OperatorPluginDirs returns the CLI plugin dirs the anonymous config carries
// forward from the operator's interactive docker config dir — the inherited
// DOCKER_CONFIG, else $HOME/.docker. It always includes that dir's
// cli-plugins subdirectory (where Docker Desktop installs docker-buildx),
// followed by the absolute entries of the cliPluginsExtraDirs key of that
// dir's config.json. The file is read once, read-only, and ONLY that key is
// decoded: no credential, credsStore or credHelpers value is read from it, so
// the interactive config is never used for credentials. An absent or
// malformed file yields just the cli-plugins dir; with neither variable set
// there is no operator dir and the result is nil.
func OperatorPluginDirs(getenv func(string) string) []string {
	dir := getenv("DOCKER_CONFIG")
	if dir == "" {
		home := getenv("HOME")
		if home == "" {
			return nil
		}
		dir = filepath.Join(home, ".docker")
	}
	dirs := []string{filepath.Join(dir, "cli-plugins")}
	b, err := os.ReadFile(filepath.Join(dir, dockerConfigFile))
	if err != nil {
		return dirs
	}
	var only struct {
		CliPluginsExtraDirs []string `json:"cliPluginsExtraDirs"`
	}
	if json.Unmarshal(b, &only) != nil {
		return dirs
	}
	for _, d := range only.CliPluginsExtraDirs {
		if filepath.IsAbs(d) {
			dirs = append(dirs, d)
		}
	}
	return dirs
}

// ErrDockerConfig is wrapped by LoadOperatorDockerConfig for every defect.
var ErrDockerConfig = errors.New("invalid gate docker config")

// LoadOperatorDockerConfig validates the operator-prepared config dir named by
// FISHHAWK_GATE_DOCKER_CONFIG: an absolute path to a directory holding a
// config.json whose top level is a JSON object, with a string credsStore and
// a string-valued credHelpers map whose helper names match
// ^[A-Za-z0-9._-]+$. Every defect wraps ErrDockerConfig. The returned config
// is never owned: Remove never deletes it.
func LoadOperatorDockerConfig(dir string) (DockerConfig, error) {
	if !filepath.IsAbs(dir) {
		return DockerConfig{}, fmt.Errorf("%w: %q is not an absolute path", ErrDockerConfig, dir)
	}
	info, err := os.Stat(dir)
	if err != nil {
		return DockerConfig{}, fmt.Errorf("%w: %v", ErrDockerConfig, err)
	}
	if !info.IsDir() {
		return DockerConfig{}, fmt.Errorf("%w: %q is not a directory", ErrDockerConfig, dir)
	}
	cfg := DockerConfig{Dir: dir, Credentials: CredentialsOperatorConfig}
	b, err := os.ReadFile(cfg.ConfigFile())
	if err != nil {
		return DockerConfig{}, fmt.Errorf("%w: %q has no readable %s: %v", ErrDockerConfig, dir, dockerConfigFile, err)
	}
	if !bytes.HasPrefix(bytes.TrimSpace(b), []byte("{")) {
		return DockerConfig{}, fmt.Errorf("%w: %s is not a JSON object", ErrDockerConfig, cfg.ConfigFile())
	}
	var parsed struct {
		CredsStore  string            `json:"credsStore"`
		CredHelpers map[string]string `json:"credHelpers"`
	}
	if err := json.Unmarshal(b, &parsed); err != nil {
		return DockerConfig{}, fmt.Errorf("%w: %s: %v", ErrDockerConfig, cfg.ConfigFile(), err)
	}
	if parsed.CredsStore != "" && !helperNamePattern.MatchString(parsed.CredsStore) {
		return DockerConfig{}, fmt.Errorf("%w: credsStore %q is not a helper name (%s)", ErrDockerConfig, parsed.CredsStore, helperNamePattern)
	}
	for server, helper := range parsed.CredHelpers {
		if helper != "" && !helperNamePattern.MatchString(helper) {
			return DockerConfig{}, fmt.Errorf("%w: credHelpers[%q] = %q is not a helper name (%s)", ErrDockerConfig, server, helper, helperNamePattern)
		}
	}
	cfg.credsStore, cfg.credHelpers = parsed.CredsStore, parsed.CredHelpers
	return cfg, nil
}

// CredentialProbe is one (helper, server) pair the docker CLI would consult
// for an image this exec pulls.
type CredentialProbe struct {
	// Helper is the store name: the executable is docker-credential-<Helper>.
	Helper string
	// Server is the auth key the CLI asks the helper for.
	Server string
}

// Executable is the helper's executable name.
func (p CredentialProbe) Executable() string { return "docker-credential-" + p.Helper }

// authKey is the key the docker CLI resolves an image's credentials by.
func authKey(ref ImageRef) string {
	if ref.Registry == defaultRegistry {
		return dockerHubAuthKey
	}
	return ref.Registry
}

// ProbesFor returns the (helper, server) pairs the docker CLI would invoke for
// images, mirroring its store choice: credHelpers[server] when the key exists
// (an empty value selects the plain file store, no helper), else credsStore.
// A pair whose server no image names is never returned, and an unparsable
// image is skipped (it fails on its own later). Deduplicated, in first-seen
// order. Always nil for the anonymous config.
func (c DockerConfig) ProbesFor(images []string) []CredentialProbe {
	var out []CredentialProbe
	seen := map[CredentialProbe]bool{}
	for _, img := range images {
		ref, err := ParseImageRef(img)
		if err != nil {
			continue
		}
		server := authKey(ref)
		helper, ok := c.credHelpers[server]
		if !ok {
			helper = c.credsStore
		}
		p := CredentialProbe{Helper: helper, Server: server}
		if helper == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

// ProbeOutcome classifies one credential helper probe.
type ProbeOutcome string

const (
	// ProbeAnswered means the helper exited within the bound (whatever its exit
	// code: "credentials not found" is an answer).
	ProbeAnswered ProbeOutcome = "answered"
	// ProbeMissing means the helper is not on the env's PATH, or would not start.
	// The pull reports its own error; the exec is not failed here.
	ProbeMissing ProbeOutcome = "missing"
	// ProbeBlocked means the helper did not answer within the bound; its whole
	// process group was killed.
	ProbeBlocked ProbeOutcome = "blocked"
)

// ProbeResult is one probe's verdict. It never carries the helper's output:
// a helper that answers prints a credential.
type ProbeResult struct {
	Probe   CredentialProbe
	Outcome ProbeOutcome
	// Detail names why a probe is missing ("" otherwise).
	Detail  string
	Bound   time.Duration
	Elapsed time.Duration
}

// BlockedReason is the operator-facing cause of a blocked probe.
func (r ProbeResult) BlockedReason() string {
	return fmt.Sprintf("container_credentials_blocked: credential helper %s did not answer within %s for %s (a locked keychain/screen, or a slow network-backed helper)",
		r.Probe.Executable(), r.Bound, r.Probe.Server)
}

// probeWaitDelay bounds how long Wait lingers on the stdin copy after the
// helper's group was killed.
const probeWaitDelay = 2 * time.Second

// lookPathIn resolves file on the PATH carried by env (not the process PATH):
// the probe must find the helper the runtime CLI, run under env, would find.
func lookPathIn(file string, env []string) (string, error) {
	path := ""
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			path = v
		}
	}
	for _, dir := range filepath.SplitList(path) {
		if dir == "" || !filepath.IsAbs(dir) {
			continue
		}
		p := filepath.Join(dir, file)
		if info, err := os.Stat(p); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			return p, nil
		}
	}
	return "", fmt.Errorf("%s not found on the runtime CLI's PATH", file)
}

// ProbeCredentialHelper runs `docker-credential-<helper> get` for the probe's
// server under env, bounded by bound. The helper runs in its own process
// group with stdout and stderr on the null device (its answer is a
// credential and is never read), and on expiry the WHOLE group is SIGKILLed —
// a helper's descendant cannot outlive the probe. It classifies the helper as
// answered, missing or blocked.
func ProbeCredentialHelper(ctx context.Context, p CredentialProbe, env []string, bound time.Duration) ProbeResult {
	res := ProbeResult{Probe: p, Bound: bound}
	bin, err := lookPathIn(p.Executable(), env)
	if err != nil {
		res.Outcome, res.Detail = ProbeMissing, err.Error()
		return res
	}
	pctx, cancel := context.WithTimeout(ctx, bound)
	defer cancel()
	cmd := exec.CommandContext(pctx, bin, "get")
	cmd.Env = env
	cmd.Stdin = strings.NewReader(p.Server + "\n")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = probeWaitDelay
	start := time.Now()
	if err := cmd.Start(); err != nil {
		res.Outcome, res.Detail = ProbeMissing, "start: "+err.Error()
		return res
	}
	_ = cmd.Wait()
	res.Elapsed = time.Since(start)
	if pctx.Err() != nil {
		res.Outcome = ProbeBlocked
		return res
	}
	res.Outcome = ProbeAnswered
	return res
}
