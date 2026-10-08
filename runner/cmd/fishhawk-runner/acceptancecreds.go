package main

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"sort"

	"github.com/kuhlman-labs/fishhawk/runner/internal/acceptenv"
	"github.com/kuhlman-labs/fishhawk/runner/internal/netsandbox"
)

// acceptanceCredentialIsolationEnvVar is the operator policy for cutting the
// acceptance agent off from the runner host's ambient forge credentials
// (E72.40 / #3792): auto (default — isolate; with no env-carried model
// credential, degrade LOUDLY to the real HOME so the keychain subscription
// login keeps working), require (fail the stage category-C pre-spawn when no
// env-carried model credential exists), off (the kill switch: today's
// posture — real HOME, no git pins, no credential profile clauses, the
// home-family passthroughs honoured). Runner-process config, dropped from the
// agent env by acceptenv's default-deny allow-list.
const acceptanceCredentialIsolationEnvVar = "FISHHAWK_ACCEPTANCE_CREDENTIAL_ISOLATION"

// skipReasonModelCredentialNotEnvCarried is the home_isolation_skipped value
// when auto keeps the real HOME: a synthetic HOME would hide the keychain or
// ~/.claude subscription login and log the agent out.
const skipReasonModelCredentialNotEnvCarried = "model_credential_not_env_carried"

// acceptanceCredentialIsolation is the resolved per-stage decision.
type acceptanceCredentialIsolation struct {
	// mode is the parsed policy (auto|require|off).
	mode netsandbox.Mode
	// active is true for every mode but off: the git pins apply, the
	// credential-locator passthroughs are refused, and the Seatbelt profile
	// (when it applies) carries deny.
	active bool
	// homeIsolated is true when the agent gets a runner-created synthetic
	// HOME — only when an env-carried model credential exists.
	homeIsolated bool
	// skipReason is set when auto keeps the real HOME.
	skipReason string
	// deny is the Seatbelt credential denial; zero under off.
	deny netsandbox.CredentialDeny
}

// resolveAcceptanceCredentialIsolation decides the acceptance stage's
// credential isolation. A non-empty failReason is a category-C stage failure
// the caller reports BEFORE any spawn; the matching detail event is logged
// here.
//
// Branches (each asserted by acceptancecreds_test.go):
//   - invalid mode                -> acceptance_credential_isolation_config (fail)
//   - off                         -> zero decision, active:false
//   - auto|require + credential   -> synthetic HOME, keychain+files+ssh-agent denied
//   - auto + no credential        -> real HOME (skipReason), files+ssh-agent denied
//   - require + no credential     -> acceptance_credential_isolation_required (fail)
//
// homes is consulted only when isolation is active.
func resolveAcceptanceCredentialIsolation(getenv func(string) string, environ []string, agentID string, homes func() []string, logSink io.Writer) (iso acceptanceCredentialIsolation, failReason, failDetail string) {
	mode, err := netsandbox.ParseMode(getenv(acceptanceCredentialIsolationEnvVar))
	if err != nil {
		_, _ = fmt.Fprintf(logSink,
			`{"event":"acceptance_credential_isolation_config","var":%q,"detail":%q}`+"\n",
			acceptanceCredentialIsolationEnvVar, err.Error())
		return acceptanceCredentialIsolation{}, "acceptance_credential_isolation_config", err.Error()
	}
	if mode == netsandbox.ModeOff {
		return acceptanceCredentialIsolation{mode: mode}, "", ""
	}
	hasCredential := acceptenv.HasModelCredential(environ, agentID)
	if !hasCredential && mode == netsandbox.ModeRequire {
		detail := fmt.Sprintf("%s=require but no env-carried model credential is set for agent %q: "+
			"run `claude setup-token` and export the printed token as CLAUDE_CODE_OAUTH_TOKEN "+
			"(or set ANTHROPIC_API_KEY; OPENAI_API_KEY for codex) on the runner env",
			acceptanceCredentialIsolationEnvVar, agentID)
		_, _ = fmt.Fprintf(logSink,
			`{"event":"acceptance_credential_isolation_required","mode":%q,"detail":%q}`+"\n", mode, detail)
		return acceptanceCredentialIsolation{}, "acceptance_credential_isolation_required", detail
	}
	iso = acceptanceCredentialIsolation{
		mode:         mode,
		active:       true,
		homeIsolated: hasCredential,
		deny: netsandbox.CredentialDeny{
			Homes:        homes(),
			Files:        true,
			Keychain:     hasCredential,
			SSHAgent:     true,
			AgentSockets: resolveAgentSocket(getenv),
		},
	}
	if !hasCredential {
		iso.skipReason = skipReasonModelCredentialNotEnvCarried
	}
	return iso, "", ""
}

// realHomeDirs returns the runner host's home directories — $HOME
// (os.UserHomeDir) and the passwd home (user.Current), which OpenSSH reads
// regardless of $HOME — each symlink-resolved, de-duplicated and sorted. One
// that cannot be resolved, or resolves to a relative path, is skipped:
// netsandbox requires canonical homes.
func realHomeDirs() []string {
	var candidates []string
	if h, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, h)
	}
	if u, err := user.Current(); err == nil {
		candidates = append(candidates, u.HomeDir)
	}
	set := map[string]struct{}{}
	for _, c := range candidates {
		if c == "" {
			continue
		}
		r, err := filepath.EvalSymlinks(c)
		if err != nil {
			continue
		}
		if !filepath.IsAbs(r) {
			continue
		}
		set[r] = struct{}{}
	}
	out := make([]string, 0, len(set))
	for h := range set {
		out = append(out, h)
	}
	sort.Strings(out)
	return out
}

// resolveAgentSocket returns the runner's SSH_AUTH_SOCK, symlink-resolved
// (Seatbelt matches a socket by its resolved path, e.g. /private/tmp/...),
// falling back to the cleaned path for a stale socket. Unset or relative
// gives none.
func resolveAgentSocket(getenv func(string) string) []string {
	sock := getenv("SSH_AUTH_SOCK")
	if sock == "" || !filepath.IsAbs(sock) {
		return nil
	}
	if r, err := filepath.EvalSymlinks(sock); err == nil {
		return []string{r}
	}
	return []string{filepath.Clean(sock)}
}

// acceptanceHomeMkdirTemp creates the synthetic HOME; a package-level var so
// a test can fail ONLY the HOME creation (the working-dir MkdirTemp runs
// first and must stay real).
var acceptanceHomeMkdirTemp = os.MkdirTemp

// removeAllForce removes dir like os.RemoveAll and, when that fails, makes
// every directory under it owner-writable and retries — a synthetic HOME can
// hold a 0555 Go module cache ($HOME/go/pkg/mod) a plain RemoveAll cannot
// unlink.
func removeAllForce(dir string) error {
	if err := os.RemoveAll(dir); err == nil {
		return nil
	}
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(p, 0o700)
		}
		return nil
	})
	return os.RemoveAll(dir)
}

// cleanupAcceptanceDir is the deferred removal of an acceptance temp dir. A
// failure logs event and never changes the stage outcome.
func cleanupAcceptanceDir(dir, event string, logSink io.Writer) {
	if err := removeAllForce(dir); err != nil {
		_, _ = fmt.Fprintf(logSink, `{"event":%q,"dir":%q,"detail":%q}`+"\n", event, dir, err.Error())
	}
}
