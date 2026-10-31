package gitops

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// lockRetryTestD scales a deadline-competing duration by the test time factor
// (CI auto-scales 5x; FISHHAWK_TEST_TIME_SCALE overrides), mirroring
// cmd/fishhawk-runner's scaledD: the runner module has no timescale package.
func lockRetryTestD(base time.Duration) time.Duration {
	factor := 1
	if v := os.Getenv("FISHHAWK_TEST_TIME_SCALE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 1000 {
			factor = n
		}
	} else if os.Getenv("CI") != "" {
		factor = 5
	}
	return base * time.Duration(factor)
}

// maskedMaintenanceRepo returns a repository whose LOCAL config turns
// auto-maintenance ON (maintenance.auto=true, gc.auto=6700). TestMain pins
// maintenance.auto=false through GIT_CONFIG_GLOBAL, which would otherwise mask
// a missing pin; repo-local config beats global, and command-scope config
// beats repo-local, so a child reads false/0 only when the pins reach it.
func maskedMaintenanceRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	mustGit(t, repo, "init", "-q", "--initial-branch=main")
	mustGit(t, repo, "config", "maintenance.auto", "true")
	mustGit(t, repo, "config", "gc.auto", "6700")
	return repo
}

// gitConfigGetEnv reads key in repo with cmd.Env = env (nil inherits the
// process environment).
func gitConfigGetEnv(t *testing.T, repo string, env []string, key string) string {
	t.Helper()
	cmd := exec.Command("git", "config", "--get", key)
	cmd.Dir = repo
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git config --get %s: %v\n%s", key, err, out)
	}
	return strings.TrimSpace(string(out))
}

// environWithoutGitConfig is os.Environ() minus every GIT_CONFIG_COUNT/KEY/VALUE
// entry, so a test controls the inherited command-scope set exactly.
func environWithoutGitConfig() []string {
	var out []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "GIT_CONFIG_COUNT=") || strings.HasPrefix(kv, "GIT_CONFIG_KEY_") || strings.HasPrefix(kv, "GIT_CONFIG_VALUE_") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// assertPinsTrailCount asserts env's effective GIT_CONFIG_COUNT equals want
// and that its last len(maintenancePins) entries are the pins — presence plus
// count consistency, not fixed indices (approval condition C2).
func assertPinsTrailCount(t *testing.T, env []string, want int) {
	t.Helper()
	raw, ok := lookupEnv(env, "GIT_CONFIG_COUNT")
	if !ok {
		t.Fatalf("GIT_CONFIG_COUNT absent from env")
	}
	if raw != strconv.Itoa(want) {
		t.Fatalf("GIT_CONFIG_COUNT = %q, want %d", raw, want)
	}
	if !maintenancePinsTrailing(env, want) {
		t.Fatalf("maintenance pins are not the trailing entries of the count-%d set", want)
	}
	for i := 0; i < want; i++ {
		if _, ok := lookupEnv(env, "GIT_CONFIG_KEY_"+strconv.Itoa(i)); !ok {
			t.Fatalf("GIT_CONFIG_KEY_%d absent although GIT_CONFIG_COUNT=%d", i, want)
		}
	}
	var count int
	for _, kv := range env {
		if strings.HasPrefix(kv, "GIT_CONFIG_COUNT=") {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("env carries %d GIT_CONFIG_COUNT entries, want exactly 1", count)
	}
}

func TestConfigEnvPinsMaintenance(t *testing.T) {
	repo := maskedMaintenanceRepo(t)

	// Precondition: the fixture really masks a missing pin.
	if got := gitConfigGetEnv(t, repo, environWithoutGitConfig(), "maintenance.auto"); got != "true" {
		t.Fatalf("fixture maintenance.auto = %q without pins, want true (mask ineffective)", got)
	}

	env := append(environWithoutGitConfig(), ConfigEnv()...)
	if got := gitConfigGetEnv(t, repo, env, "maintenance.auto"); got != "false" {
		t.Errorf("maintenance.auto under ConfigEnv() = %q, want false", got)
	}
	if got := gitConfigGetEnv(t, repo, env, "gc.auto"); got != "0" {
		t.Errorf("gc.auto under ConfigEnv() = %q, want 0", got)
	}

	// Extra entries follow the pins, and the inherited set is REPLACED.
	inherited := append(environWithoutGitConfig(), "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=core.abbrev", "GIT_CONFIG_VALUE_0=9")
	env = append(inherited, ConfigEnv([2]string{"core.abbrev", "12"})...)
	if got := gitConfigGetEnv(t, repo, env, "core.abbrev"); got != "12" {
		t.Errorf("core.abbrev under ConfigEnv(extra) = %q, want 12", got)
	}
	if got := gitConfigGetEnv(t, repo, env, "maintenance.auto"); got != "false" {
		t.Errorf("maintenance.auto under ConfigEnv(extra) = %q, want false", got)
	}
}

func TestAppendConfigEnv_PreservesInherited(t *testing.T) {
	repo := maskedMaintenanceRepo(t)

	// A stale, uncounted KEY_1 must not survive as a duplicate.
	inherited := append(environWithoutGitConfig(),
		"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=core.abbrev", "GIT_CONFIG_VALUE_0=12",
		"GIT_CONFIG_KEY_1=stale.key")
	env, err := AppendConfigEnv(inherited)
	if err != nil {
		t.Fatalf("AppendConfigEnv: %v", err)
	}
	assertPinsTrailCount(t, env, 1+len(maintenancePins))
	if got := gitConfigGetEnv(t, repo, env, "core.abbrev"); got != "12" {
		t.Errorf("inherited core.abbrev = %q after AppendConfigEnv, want 12", got)
	}
	if got := gitConfigGetEnv(t, repo, env, "maintenance.auto"); got != "false" {
		t.Errorf("maintenance.auto = %q after AppendConfigEnv, want false", got)
	}
	if got := gitConfigGetEnv(t, repo, env, "gc.auto"); got != "0" {
		t.Errorf("gc.auto = %q after AppendConfigEnv, want 0", got)
	}
	var key1 int
	for _, kv := range env {
		if strings.HasPrefix(kv, "GIT_CONFIG_KEY_1=") {
			key1++
		}
	}
	if key1 != 1 {
		t.Errorf("env carries %d GIT_CONFIG_KEY_1 entries, want 1 (stale entry replaced)", key1)
	}

	// No inherited set: the pins start at index 0.
	env, err = AppendConfigEnv(environWithoutGitConfig())
	if err != nil {
		t.Fatalf("AppendConfigEnv(no inherited set): %v", err)
	}
	assertPinsTrailCount(t, env, len(maintenancePins))
	if got := gitConfigGetEnv(t, repo, env, "maintenance.auto"); got != "false" {
		t.Errorf("maintenance.auto = %q with no inherited set, want false", got)
	}
}

// TestAppendConfigEnv_SkipsWhenPinsTrailing pins approval condition C2: a
// second append over an env whose trailing entries are already the pins is a
// no-op, not a duplicate pair.
func TestAppendConfigEnv_SkipsWhenPinsTrailing(t *testing.T) {
	inherited := append(environWithoutGitConfig(), "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=core.abbrev", "GIT_CONFIG_VALUE_0=12")
	once, err := AppendConfigEnv(inherited)
	if err != nil {
		t.Fatalf("AppendConfigEnv: %v", err)
	}
	twice, err := AppendConfigEnv(once)
	if err != nil {
		t.Fatalf("AppendConfigEnv(again): %v", err)
	}
	assertPinsTrailCount(t, twice, 1+len(maintenancePins))
	if strings.Join(twice, "\n") != strings.Join(once, "\n") {
		t.Errorf("second AppendConfigEnv changed the env:\nonce:  %q\ntwice: %q", once, twice)
	}

	// Pins present but NOT trailing (an operator entry after them): append.
	notTrailing := append(environWithoutGitConfig(),
		"GIT_CONFIG_COUNT=3",
		"GIT_CONFIG_KEY_0=maintenance.auto", "GIT_CONFIG_VALUE_0=false",
		"GIT_CONFIG_KEY_1=gc.auto", "GIT_CONFIG_VALUE_1=0",
		"GIT_CONFIG_KEY_2=maintenance.auto", "GIT_CONFIG_VALUE_2=true")
	env, err := AppendConfigEnv(notTrailing)
	if err != nil {
		t.Fatalf("AppendConfigEnv(not trailing): %v", err)
	}
	assertPinsTrailCount(t, env, 3+len(maintenancePins))
}

func TestAppendConfigEnv_RefusesMalformedCount(t *testing.T) {
	for _, raw := range []string{"x", "-1", "+2", "1x", " 1"} {
		t.Run(raw, func(t *testing.T) {
			_, err := AppendConfigEnv(append(environWithoutGitConfig(), "GIT_CONFIG_COUNT="+raw))
			if err == nil {
				t.Fatalf("AppendConfigEnv(GIT_CONFIG_COUNT=%q) = nil error, want refusal", raw)
			}
			if !strings.Contains(err.Error(), "GIT_CONFIG_COUNT") {
				t.Errorf("error %q does not name GIT_CONFIG_COUNT", err)
			}
		})
	}
	// An empty count is git's zero, not an error.
	env, err := AppendConfigEnv(append(environWithoutGitConfig(), "GIT_CONFIG_COUNT="))
	if err != nil {
		t.Fatalf("AppendConfigEnv(GIT_CONFIG_COUNT=\"\"): %v", err)
	}
	assertPinsTrailCount(t, env, len(maintenancePins))
}

// isolateProcessGitConfig registers every GIT_CONFIG_* variable the install
// can touch with t.Setenv (so cleanup restores them) and then unsets them,
// leaving the process with no command-scope set.
func isolateProcessGitConfig(t *testing.T) {
	t.Helper()
	names := []string{"GIT_CONFIG_COUNT"}
	for i := 0; i < 8; i++ {
		names = append(names, "GIT_CONFIG_KEY_"+strconv.Itoa(i), "GIT_CONFIG_VALUE_"+strconv.Itoa(i))
	}
	for _, name := range names {
		t.Setenv(name, "")
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}
}

func TestInstallProcessGitConfig_InheritedChild(t *testing.T) {
	repo := maskedMaintenanceRepo(t)

	t.Run("no inherited set", func(t *testing.T) {
		isolateProcessGitConfig(t)
		if got := gitConfigGetEnv(t, repo, nil, "maintenance.auto"); got != "true" {
			t.Fatalf("precondition: inherited maintenance.auto = %q before install, want true", got)
		}
		if err := InstallProcessGitConfig(); err != nil {
			t.Fatalf("InstallProcessGitConfig: %v", err)
		}
		if got := gitConfigGetEnv(t, repo, nil, "maintenance.auto"); got != "false" {
			t.Errorf("inherited maintenance.auto = %q after install, want false", got)
		}
		if got := gitConfigGetEnv(t, repo, nil, "gc.auto"); got != "0" {
			t.Errorf("inherited gc.auto = %q after install, want 0", got)
		}
		// Idempotent: a second install appends nothing.
		if err := InstallProcessGitConfig(); err != nil {
			t.Fatalf("InstallProcessGitConfig(again): %v", err)
		}
		assertPinsTrailCount(t, os.Environ(), len(maintenancePins))
	})

	t.Run("operator set preserved", func(t *testing.T) {
		isolateProcessGitConfig(t)
		t.Setenv("GIT_CONFIG_COUNT", "1")
		t.Setenv("GIT_CONFIG_KEY_0", "core.abbrev")
		t.Setenv("GIT_CONFIG_VALUE_0", "12")
		if err := InstallProcessGitConfig(); err != nil {
			t.Fatalf("InstallProcessGitConfig: %v", err)
		}
		assertPinsTrailCount(t, os.Environ(), 1+len(maintenancePins))
		if got := gitConfigGetEnv(t, repo, nil, "core.abbrev"); got != "12" {
			t.Errorf("inherited core.abbrev = %q after install, want 12", got)
		}
		if got := gitConfigGetEnv(t, repo, nil, "maintenance.auto"); got != "false" {
			t.Errorf("inherited maintenance.auto = %q after install, want false", got)
		}
	})

	t.Run("malformed count refused, env unchanged", func(t *testing.T) {
		isolateProcessGitConfig(t)
		t.Setenv("GIT_CONFIG_COUNT", "bogus")
		err := InstallProcessGitConfig()
		if err == nil || !strings.Contains(err.Error(), "GIT_CONFIG_COUNT") {
			t.Fatalf("InstallProcessGitConfig() = %v, want an error naming GIT_CONFIG_COUNT", err)
		}
		if got := os.Getenv("GIT_CONFIG_COUNT"); got != "bogus" {
			t.Errorf("GIT_CONFIG_COUNT = %q after refused install, want unchanged %q", got, "bogus")
		}
		if _, ok := os.LookupEnv("GIT_CONFIG_KEY_0"); ok {
			t.Errorf("refused install still set GIT_CONFIG_KEY_0")
		}
	})
}

func TestIdentityEnv_OverridesRepoConfig(t *testing.T) {
	repo := t.TempDir()
	mustGit(t, repo, "init", "-q", "--initial-branch=main")
	mustGit(t, repo, "config", "user.name", "someone-else")
	mustGit(t, repo, "config", "user.email", "someone-else@example.com")
	mustGit(t, repo, "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("f\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "add", "-A")

	cmd := exec.Command("git", "commit", "-q", "-m", "identity")
	cmd.Dir = repo
	cmd.Env = append(os.Environ(), IdentityEnv("fh-bot", "fh-bot@example.com")...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v\n%s", err, out)
	}
	got := mustGitOut(t, repo, "log", "-1", "--format=%an|%ae|%cn|%ce")
	if want := "fh-bot|fh-bot@example.com|fh-bot|fh-bot@example.com"; got != want {
		t.Errorf("commit identity = %q, want %q", got, want)
	}
}

// lockRetryRepo returns a repository with one commit and refs/heads/x pointing at
// it, plus the path of the loose-ref lockfile for refs/heads/x.
func lockRetryRepo(t *testing.T) (repo, lockPath string) {
	t.Helper()
	repo = t.TempDir()
	mustGit(t, repo, "init", "-q", "--initial-branch=main")
	mustGit(t, repo, "-c", "user.name=init", "-c", "user.email=init@example.com", "commit", "-q", "--allow-empty", "-m", "init")
	mustGit(t, repo, "update-ref", "refs/heads/x", "HEAD")
	return repo, filepath.Join(repo, ".git", "refs", "heads", "x.lock")
}

// lockGitOp runs git in repo under LockRetryLocaleEnv, returning combined output
// and the error, as a RetryLockContention op.
func lockGitOp(ctx context.Context, repo string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = repo
	cmd.Env = append(os.Environ(), LockRetryLocaleEnv()...)
	return cmd.CombinedOutput()
}

func holdRefLock(t *testing.T, lockPath string) {
	t.Helper()
	if err := os.WriteFile(lockPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestIsLockContention(t *testing.T) {
	repo, lockPath := lockRetryRepo(t)
	ctx := context.Background()
	head := mustGitOut(t, repo, "rev-parse", "HEAD")

	holdRefLock(t, lockPath)
	lockOut, err := lockGitOp(ctx, repo, "update-ref", "refs/heads/x", head)
	if err == nil {
		t.Fatal("update-ref under a held lock succeeded")
	}
	if err := os.Remove(lockPath); err != nil {
		t.Fatal(err)
	}
	casOut, err := lockGitOp(ctx, repo, "update-ref", "refs/heads/x", head, strings.Repeat("1", len(head)))
	if err == nil {
		t.Fatal("CAS-mismatched update-ref succeeded")
	}
	dfOut, err := lockGitOp(ctx, repo, "update-ref", "refs/heads/x/y", head)
	if err == nil {
		t.Fatal("D/F-conflicting update-ref succeeded")
	}
	wt := filepath.Join(t.TempDir(), "wt")
	checkedOutOut, err := lockGitOp(ctx, repo, "worktree", "add", wt, "main")
	if err == nil {
		t.Fatal("worktree add of the checked-out branch succeeded")
	}

	for _, tc := range []struct {
		name string
		out  string
		want bool
	}{
		{"held ref lock", string(lockOut), true},
		{"CAS mismatch", string(casOut), false},
		{"D/F conflict", string(dfOut), false},
		{"already checked out", string(checkedOutOut), false},
		{"empty", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsLockContention(tc.out); got != tc.want {
				t.Errorf("IsLockContention(%q) = %v, want %v", tc.out, got, tc.want)
			}
		})
	}
}

func TestRetryLockContention_RetriesHeldRefLock(t *testing.T) {
	repo, lockPath := lockRetryRepo(t)
	head := mustGitOut(t, repo, "rev-parse", "HEAD")
	holdRefLock(t, lockPath)

	// The sibling's lock is released after the second contended attempt, so
	// the third succeeds — deterministic, no wall-clock race.
	attempts := 0
	out, err := RetryLockContention(context.Background(), func() ([]byte, error) {
		attempts++
		out, err := lockGitOp(context.Background(), repo, "update-ref", "refs/heads/x", head)
		if err != nil && attempts == 2 {
			if rmErr := os.Remove(lockPath); rmErr != nil {
				t.Errorf("release lock: %v", rmErr)
			}
		}
		return out, err
	})
	if err != nil {
		t.Fatalf("RetryLockContention = %v (output %q), want success after the lock is released", err, out)
	}
	if attempts != 3 {
		t.Errorf("attempts = %d, want 3 (two contended, one success)", attempts)
	}
}

func TestRetryLockContention_DoesNotRetryNonLockError(t *testing.T) {
	repo, _ := lockRetryRepo(t)
	head := mustGitOut(t, repo, "rev-parse", "HEAD")

	attempts := 0
	var firstErr error
	_, err := RetryLockContention(context.Background(), func() ([]byte, error) {
		attempts++
		out, err := lockGitOp(context.Background(), repo, "update-ref", "refs/heads/x", head, strings.Repeat("1", len(head)))
		if firstErr == nil {
			firstErr = err
		}
		return out, err
	})
	if err == nil {
		t.Fatal("CAS-mismatched update-ref through RetryLockContention succeeded")
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1 (a CAS mismatch is not lock contention)", attempts)
	}
	if !errors.Is(err, firstErr) || err.Error() != firstErr.Error() {
		t.Errorf("error = %v, want the original error %v unchanged", err, firstErr)
	}
}

func TestRetryLockContention_BoundedExhaustion(t *testing.T) {
	repo, lockPath := lockRetryRepo(t)
	head := mustGitOut(t, repo, "rev-parse", "HEAD")
	holdRefLock(t, lockPath)

	attempts := 0
	start := time.Now()
	_, err := RetryLockContention(context.Background(), func() ([]byte, error) {
		attempts++
		return lockGitOp(context.Background(), repo, "update-ref", "refs/heads/x", head)
	})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("RetryLockContention succeeded with the lock never released")
	}
	if attempts != LockRetryAttempts {
		t.Errorf("attempts = %d, want LockRetryAttempts = %d", attempts, LockRetryAttempts)
	}
	msg := err.Error()
	if !strings.Contains(msg, strconv.Itoa(LockRetryAttempts)+" attempts") || !strings.Contains(msg, "lock contention") {
		t.Errorf("error %q does not name the attempt bound and lock contention", msg)
	}
	if !IsLockContention(msg) {
		t.Errorf("final error %q lost the underlying lock message", msg)
	}

	// The stated bound: 50+100+200+400+800+1000+1000 ms of backoff.
	var sleep time.Duration
	for b, i := LockRetryInitialBackoff, 1; i < LockRetryAttempts; i++ {
		sleep += b
		b = min(2*b, LockRetryMaxBackoff)
	}
	if sleep != 3550*time.Millisecond {
		t.Fatalf("stated backoff sum = %v, want 3.55s", sleep)
	}
	if elapsed < sleep {
		t.Errorf("elapsed %v < the %v backoff schedule: the retry did not back off", elapsed, sleep)
	}
	// Worst case on top of the backoff: git's own 100 ms per-attempt loose-ref
	// lock wait plus process spawn, scaled for a loaded host.
	if limit := sleep + lockRetryTestD(time.Duration(LockRetryAttempts)*time.Second); elapsed > limit {
		t.Errorf("elapsed %v exceeds the stated worst case %v", elapsed, limit)
	}
}

func TestRetryLockContention_StopsOnContextDone(t *testing.T) {
	repo, lockPath := lockRetryRepo(t)
	head := mustGitOut(t, repo, "rev-parse", "HEAD")
	holdRefLock(t, lockPath)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	attempts := 0
	_, err := RetryLockContention(ctx, func() ([]byte, error) {
		attempts++
		out, err := lockGitOp(ctx, repo, "update-ref", "refs/heads/x", head)
		if attempts == 2 {
			cancel() // the ctx ends during the backoff that follows
		}
		return out, err
	})
	if err == nil {
		t.Fatal("RetryLockContention succeeded with the lock held")
	}
	if attempts != 2 {
		t.Errorf("attempts = %d, want 2 (stop in the backoff after ctx is cancelled)", attempts)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want it to wrap context.Canceled", err)
	}
	if !IsLockContention(err.Error()) {
		t.Errorf("error %q lost the last lock-contention failure", err)
	}
}
