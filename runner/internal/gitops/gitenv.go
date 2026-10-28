package gitops

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// This file holds the environment primitives that make the runner's own git
// safe beside concurrent stages in sibling worktrees of one repository
// (ADR-115 D2, #4648). Linked worktrees share $GIT_COMMON_DIR — its config,
// its refs and their lockfiles, and its object store — so a runner git child
// must never persist per-run state into the shared config, must tolerate a
// sibling briefly holding a ref lock, and must not start a repository-wide
// auto-maintenance pass under a sibling's feet.

// maintenancePins are the command-scope config entries every runner git child
// carries: maintenance.auto=false stops porcelain commands (commit, fetch,
// merge, ...) from launching `git maintenance run --auto`, and gc.auto=0
// disables the `git gc --auto` heuristics (git-config(1)). They ride as
// GIT_CONFIG_COUNT/KEY_n/VALUE_n entries, which are command scope and so beat
// repository-local config, and are never written to any config file.
var maintenancePins = [][2]string{{"maintenance.auto", "false"}, {"gc.auto", "0"}}

// gitConfigCountVar is git's command-scope config entry count (git >= 2.31).
const gitConfigCountVar = "GIT_CONFIG_COUNT"

// ConfigEnv returns GIT_CONFIG_COUNT/GIT_CONFIG_KEY_n/GIT_CONFIG_VALUE_n
// entries carrying maintenancePins followed by extra, indexed from 0. Appended
// after an environ, the set REPLACES any inherited command-scope config (the
// child sees only these entries), which is what an authenticated remote op
// wants: its extraheader reset+set must not be shadowed by an inherited set.
func ConfigEnv(extra ...[2]string) []string {
	entries := make([][2]string, 0, len(maintenancePins)+len(extra))
	entries = append(append(entries, maintenancePins...), extra...)
	env := make([]string, 0, 1+2*len(entries))
	env = append(env, gitConfigCountVar+"="+strconv.Itoa(len(entries)))
	for i, kv := range entries {
		env = append(env,
			"GIT_CONFIG_KEY_"+strconv.Itoa(i)+"="+kv[0],
			"GIT_CONFIG_VALUE_"+strconv.Itoa(i)+"="+kv[1])
	}
	return env
}

// AppendConfigEnv extends environ's inherited command-scope config set with
// maintenancePins at indices n, n+1 (n = the inherited GIT_CONFIG_COUNT), so
// an operator's own GIT_CONFIG_* entries are preserved and the pins land after
// them. When the pins are already the trailing entries of the inherited set
// (a process InstallProcessGitConfig already ran in) environ is returned
// unchanged rather than duplicating them. A non-decimal or negative inherited
// count is an error naming the variable: git itself refuses to run under it.
//
// The effective value of a variable is its LAST occurrence in environ, the
// rule os/exec applies when it de-duplicates cmd.Env.
func AppendConfigEnv(environ []string) ([]string, error) {
	set, err := maintenancePinEntries(environ)
	if err != nil {
		return nil, err
	}
	if len(set) == 0 {
		return environ, nil
	}
	replaced := make(map[string]bool, len(set))
	for _, kv := range set {
		name, _, _ := strings.Cut(kv, "=")
		replaced[name] = true
	}
	out := make([]string, 0, len(environ)+len(set))
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if !replaced[name] {
			out = append(out, kv)
		}
	}
	return append(out, set...), nil
}

// InstallProcessGitConfig appends maintenancePins to the runner PROCESS
// environment (after any inherited GIT_CONFIG_COUNT set), so every later git
// child built from os.Environ() inherits them. It is idempotent: a process
// whose effective trailing entries are already the pins is left unchanged —
// the same check AppendConfigEnv uses. A malformed inherited count is
// returned as an error and nothing is changed.
func InstallProcessGitConfig() error {
	set, err := maintenancePinEntries(os.Environ())
	if err != nil {
		return err
	}
	// The entries are ordered KEY/VALUE pairs first and GIT_CONFIG_COUNT
	// last, so a partially-applied install never advertises a count whose
	// entries are absent.
	for _, kv := range set {
		name, value, _ := strings.Cut(kv, "=")
		if err := os.Setenv(name, value); err != nil {
			return fmt.Errorf("gitops: install process git config %s: %w", name, err)
		}
	}
	return nil
}

// maintenancePinEntries returns the NAME=value entries that append
// maintenancePins to environ's inherited command-scope config set — the
// KEY/VALUE pairs at indices n.. followed by the new GIT_CONFIG_COUNT — or nil
// when the pins are already its trailing entries.
func maintenancePinEntries(environ []string) ([]string, error) {
	n, err := inheritedConfigCount(environ)
	if err != nil {
		return nil, err
	}
	if maintenancePinsTrailing(environ, n) {
		return nil, nil
	}
	set := make([]string, 0, 1+2*len(maintenancePins))
	for j, kv := range maintenancePins {
		i := strconv.Itoa(n + j)
		set = append(set, "GIT_CONFIG_KEY_"+i+"="+kv[0], "GIT_CONFIG_VALUE_"+i+"="+kv[1])
	}
	return append(set, gitConfigCountVar+"="+strconv.Itoa(n+len(maintenancePins))), nil
}

// inheritedConfigCount parses environ's effective GIT_CONFIG_COUNT. Absent or
// empty is 0 (git reads an empty count as zero entries); anything other than
// a non-negative decimal integer is an error.
func inheritedConfigCount(environ []string) (int, error) {
	raw, ok := lookupEnv(environ, gitConfigCountVar)
	if !ok || raw == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 || strings.ContainsAny(raw, "+-") {
		return 0, fmt.Errorf("gitops: inherited %s=%q is not a non-negative decimal count (git refuses to run under it)", gitConfigCountVar, raw)
	}
	return n, nil
}

// maintenancePinsTrailing reports whether the last len(maintenancePins)
// entries of environ's count-n command-scope set are exactly the pins, in
// order.
func maintenancePinsTrailing(environ []string, n int) bool {
	base := n - len(maintenancePins)
	if base < 0 {
		return false
	}
	for j, kv := range maintenancePins {
		i := strconv.Itoa(base + j)
		key, okK := lookupEnv(environ, "GIT_CONFIG_KEY_"+i)
		value, okV := lookupEnv(environ, "GIT_CONFIG_VALUE_"+i)
		if !okK || !okV || key != kv[0] || value != kv[1] {
			return false
		}
	}
	return true
}

// lookupEnv returns the value of the LAST name=... entry in environ.
func lookupEnv(environ []string, name string) (string, bool) {
	prefix := name + "="
	for i := len(environ) - 1; i >= 0; i-- {
		if strings.HasPrefix(environ[i], prefix) {
			return environ[i][len(prefix):], true
		}
	}
	return "", false
}

// IdentityEnv returns the per-command author and committer identity for a git
// child. GIT_AUTHOR_*/GIT_COMMITTER_* override user.name/user.email
// (git-commit-tree(1) "Commit Information"), so a commit carries the run's
// identity without `git config user.*`, which in a linked worktree writes the
// SHARED $GIT_COMMON_DIR/config every sibling worktree and the operator read.
func IdentityEnv(name, email string) []string {
	return []string{"GIT_AUTHOR_NAME=" + name, "GIT_AUTHOR_EMAIL=" + email, "GIT_COMMITTER_NAME=" + name, "GIT_COMMITTER_EMAIL=" + email}
}

// IsLockContention reports whether git output (stderr, or an error carrying
// it) is git's lockfile-contention message, `Unable to create '<path>.lock':
// File exists.` — for a ref wrapped as `cannot lock ref '<ref>': Unable to
// create ...`. It deliberately does NOT match a compare-and-swap mismatch
// (`cannot lock ref '<ref>': is at <sha> but expected <sha>`), a D/F conflict
// or any other failure: those are real outcomes a retry must not paper over.
// The message is English only under the C locale, so every op whose output is
// classified here must run with LockRetryLocaleEnv.
func IsLockContention(output string) bool {
	return strings.Contains(output, ".lock': File exists")
}

// LockRetryLocaleEnv is the locale pin every git invocation classified by
// IsLockContention carries, so the lockfile message is the untranslated
// English text the classifier matches regardless of the host's LANG.
func LockRetryLocaleEnv() []string {
	return []string{"LC_ALL=C", "LANG=C"}
}

// The RetryLockContention bound: at most LockRetryAttempts attempts, sleeping
// LockRetryInitialBackoff after the first and doubling up to
// LockRetryMaxBackoff — 50+100+200+400+800+1000+1000 ms, at most 3.55 s of
// added sleep per call on top of git's own per-attempt lock wait
// (core.filesRefLockTimeout, default 100 ms; core.packedRefsTimeout, default
// 1 s). git's native timeouts are deliberately not raised.
const (
	LockRetryAttempts       = 8
	LockRetryInitialBackoff = 50 * time.Millisecond
	LockRetryMaxBackoff     = time.Second
)

// RetryLockContention runs op, retrying ONLY while its failure is git lockfile
// contention (IsLockContention over op's output or error text) — a sibling
// worktree's git briefly holding a shared ref, packed-refs or worktree-registry
// lock. Any other error is returned immediately and unchanged, as is a
// success. Between attempts it sleeps with ctx-aware doubling backoff; ctx
// ending during a backoff returns promptly with the last error and ctx's.
// After LockRetryAttempts contended attempts the final error is wrapped naming
// the attempt count and `lock contention`. op must run git under
// LockRetryLocaleEnv.
func RetryLockContention(ctx context.Context, op func() ([]byte, error)) ([]byte, error) {
	backoff := LockRetryInitialBackoff
	for attempt := 1; ; attempt++ {
		out, err := op()
		if err == nil {
			return out, nil
		}
		if !IsLockContention(err.Error()) {
			if !IsLockContention(string(out)) {
				return out, err
			}
			// The lock message is only in the output (a bare exec error):
			// carry it in the error so a final wrap still names the lock.
			err = fmt.Errorf("%w (output: %s)", err, strings.TrimSpace(string(out)))
		}
		if attempt >= LockRetryAttempts {
			return out, fmt.Errorf("gitops: lock contention persisted after %d attempts: %w", attempt, err)
		}
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return out, fmt.Errorf("gitops: lock contention retry stopped after %d attempts: %w: %w", attempt, err, ctx.Err())
		case <-timer.C:
		}
		backoff = min(2*backoff, LockRetryMaxBackoff)
	}
}
