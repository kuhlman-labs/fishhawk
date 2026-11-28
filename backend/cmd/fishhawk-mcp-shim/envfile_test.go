//go:build unix

package main

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/mcpserver"
)

// requireZsh skips a test that needs the real evaluator when zsh is absent
// (CI's ubuntu Go legs). The fail-closed vehicles use a fake /bin/sh shell
// instead, so they run everywhere.
func requireZsh(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("zsh"); err != nil {
		t.Skipf("zsh not on PATH (%v); this test needs the real .env evaluator", err)
	}
}

// writeFakeShell writes a 0755 /bin/sh script standing in for zsh and
// returns its path. It ignores the -f -c <script> arguments readDotenv
// passes, so body alone decides what the "evaluator" prints and how it exits.
func writeFakeShell(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-zsh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil { //nolint:gosec // test fixture script
		t.Fatalf("write fake shell: %v", err)
	}
	return path
}

// writeDotenv writes an operator-shaped .env (0600, owned by this uid).
func writeDotenv(t *testing.T, dir, content string) string {
	t.Helper()
	path := filepath.Join(dir, ".env")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write .env: %v", err)
	}
	return path
}

// shFrame is a printf format printing a valid frame with both keys typed
// scalar-export. Values must not contain % or \.
func shFrame(label, capacity string) string {
	return "printf '" + dotenvFrameMagic + `\000scalar-export\000` + label + `\000scalar-export\000` + capacity + `\000'`
}

func fakeOpts(shell string, timeout time.Duration) dotenvOpts {
	return dotenvOpts{shell: shell, timeout: timeout, waitDelay: dotenvWaitDelay, owner: statOwner}
}

// assertRefused pins the fail-closed contract: a nil map, exactly one warning
// line naming the file and the reason, and none of the forbidden strings
// (fixture values, .env text) in the warning.
func assertRefused(t *testing.T, got map[string]string, warn, path, reason string, forbidden ...string) {
	t.Helper()
	if got != nil {
		t.Errorf("readDotenv = %v, want nil (source nothing)", got)
	}
	if n := strings.Count(warn, "\n"); n != 1 {
		t.Errorf("warning lines = %d, want exactly 1: %q", n, warn)
	}
	if !strings.Contains(warn, path) || !strings.Contains(warn, reason) {
		t.Errorf("warning %q does not name the file %q and the reason %q", warn, path, reason)
	}
	if !strings.Contains(warn, "sourcing nothing from this file") {
		t.Errorf("warning %q does not say nothing was sourced", warn)
	}
	for _, f := range forbidden {
		if strings.Contains(warn, f) {
			t.Errorf("warning %q leaks %q", warn, f)
		}
	}
}

func TestReadDotenv_SourcesExportedHostKeys(t *testing.T) {
	requireZsh(t)
	path := writeDotenv(t, t.TempDir(), "FISHHAWK_HOST_LABEL=dotenv-label\n"+
		"export FISHHAWK_HOST_CAPACITY=4\n"+
		"FISHHAWK_API_TOKEN=fhk_dotenv_sentinel\n"+
		"FISHHAWK_HOST_DISPATCH_STATE_DIR=/x\n")
	var warn bytes.Buffer
	got := readDotenv(path, defaultDotenvOpts, &warn)
	want := map[string]string{hostLabelKey: "dotenv-label", hostCapacityKey: "4"}
	if !mapsEqual(got, want) {
		t.Fatalf("readDotenv = %v, want exactly %v", got, want)
	}
	if warn.Len() != 0 {
		t.Fatalf("unexpected warning: %q", warn.String())
	}
}

// TestReadDotenv_EvaluatesLikeZsh pins that control flow, quoting and typing
// agree with what a child of `set -a; source .env` sees. The expectations
// were checked against printenv under `zsh -f -c 'set -a; source f'`.
func TestReadDotenv_EvaluatesLikeZsh(t *testing.T) {
	requireZsh(t)
	cases := []struct {
		name, content string
		want          map[string]string
	}{
		{"if block", "if true; then\n  FISHHAWK_HOST_LABEL=in-if\nfi\n",
			map[string]string{hostLabelKey: "in-if"}},
		{"double-quoted multi-line value", "FISHHAWK_HOST_CAPACITY=5\nNOTE=\"first\nFISHHAWK_HOST_CAPACITY=9\nlast\"\n",
			map[string]string{hostCapacityKey: "5"}},
		{"single-quoted multi-line value", "FISHHAWK_HOST_CAPACITY=5\nNOTE='first\nFISHHAWK_HOST_CAPACITY=9\nlast'\n",
			map[string]string{hostCapacityKey: "5"}},
		{"array is never exported", "FISHHAWK_HOST_LABEL=(a b)\nFISHHAWK_HOST_CAPACITY=4\n",
			map[string]string{hostCapacityKey: "4"}},
		{"typeset +x unexports", "FISHHAWK_HOST_LABEL=x\ntypeset +x FISHHAWK_HOST_LABEL\n",
			map[string]string{}},
		{"integer export", "typeset -i FISHHAWK_HOST_CAPACITY=5\n",
			map[string]string{hostCapacityKey: "5"}},
		{"last assignment wins", "FISHHAWK_HOST_LABEL=first\nFISHHAWK_HOST_LABEL=second\n",
			map[string]string{hostLabelKey: "second"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeDotenv(t, t.TempDir(), tc.content)
			var warn bytes.Buffer
			got := readDotenv(path, defaultDotenvOpts, &warn)
			if !mapsEqual(got, tc.want) {
				t.Fatalf("readDotenv = %v, want %v (warn %q)", got, tc.want, warn.String())
			}
		})
	}
}

// TestReadDotenv_CRLFPassesThrough pins D4: the shim does no CR or whitespace
// normalisation; fishhawk-mcp TrimSpaces downstream.
func TestReadDotenv_CRLFPassesThrough(t *testing.T) {
	requireZsh(t)
	path := writeDotenv(t, t.TempDir(), "FISHHAWK_HOST_LABEL=crlf\r\nFISHHAWK_HOST_CAPACITY=4\r\n")
	var warn bytes.Buffer
	got := readDotenv(path, defaultDotenvOpts, &warn)
	want := map[string]string{hostLabelKey: "crlf\r", hostCapacityKey: "4\r"}
	if !mapsEqual(got, want) {
		t.Fatalf("readDotenv = %q, want %q (warn %q)", got, want, warn.String())
	}
	env := mergeDotenv(nil, got, path, &warn)
	if v, _ := lookupLast(env, hostLabelKey); v != "crlf\r" {
		t.Errorf("merged label = %q, want it passed through", v)
	}
	if v, _ := lookupLast(env, hostCapacityKey); v != "4\r" {
		t.Errorf("merged capacity = %q, want it passed through", v)
	}
	if err := mcpserver.ValidateHostCapacityEnv(func(string) (string, bool) { return "4\r", true }); err != nil {
		t.Errorf("fishhawk-mcp refuses the passed-through capacity: %v", err)
	}
}

func TestReadDotenv_MissingFileIsSilent(t *testing.T) {
	var warn bytes.Buffer
	if got := readDotenv(filepath.Join(t.TempDir(), ".env"), defaultDotenvOpts, &warn); got != nil {
		t.Fatalf("readDotenv = %v, want nil", got)
	}
	if warn.Len() != 0 {
		t.Fatalf("a missing file must be silent, got %q", warn.String())
	}
}

// TestReadDotenv_StatErrorWarns: a stat failure other than not-exist (here
// ENOTDIR, a path through a regular file) warns once and sources nothing.
func TestReadDotenv_StatErrorWarns(t *testing.T) {
	file := writeDotenv(t, t.TempDir(), "FISHHAWK_HOST_LABEL=x\n")
	path := filepath.Join(file, ".env")
	var warn bytes.Buffer
	assertRefused(t, readDotenv(path, defaultDotenvOpts, &warn), warn.String(), path, "cannot stat it")
}

// TestReadDotenv_NonRegularFileIsSkipped pins D1a: a FIFO is never opened. With
// the guard gone zsh would block on the writer-less FIFO until the timeout,
// which still yields nil, so the warning text and the elapsed time carry it.
func TestReadDotenv_NonRegularFileIsSkipped(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "fifo.env")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	subdir := filepath.Join(dir, "dir.env")
	if err := os.Mkdir(subdir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for _, path := range []string{fifo, subdir} {
		var warn bytes.Buffer
		start := time.Now()
		got := readDotenv(path, defaultDotenvOpts, &warn)
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Errorf("%s: readDotenv took %s, want < 2s (a non-regular file must never be opened)", path, elapsed)
		}
		assertRefused(t, got, warn.String(), path, "not a regular file")
	}
}

// TestReadDotenv_ForeignOwnerIsSkipped pins S0-1: a file another uid owns is
// refused, through the owner seam.
func TestReadDotenv_ForeignOwnerIsSkipped(t *testing.T) {
	path := writeDotenv(t, t.TempDir(), "FISHHAWK_HOST_LABEL=foreign-label\n")
	shell := writeFakeShell(t, shFrame("foreign-label", "4"))
	o := fakeOpts(shell, 5*time.Second)
	o.owner = func(fs.FileInfo) (int, bool) { return os.Getuid() + 1, true }
	var warn bytes.Buffer
	assertRefused(t, readDotenv(path, o, &warn), warn.String(), path, "not owned by the current user", "foreign-label")

	o.owner = func(fs.FileInfo) (int, bool) { return 0, false }
	warn.Reset()
	assertRefused(t, readDotenv(path, o, &warn), warn.String(), path, "not owned by the current user", "foreign-label")
}

// TestReadDotenv_GroupOrWorldWritableIsSkipped pins S0-1: a file another user
// could have written is refused.
func TestReadDotenv_GroupOrWorldWritableIsSkipped(t *testing.T) {
	shell := writeFakeShell(t, shFrame("writable-label", "4"))
	for _, mode := range []fs.FileMode{0o620, 0o602} {
		path := writeDotenv(t, t.TempDir(), "FISHHAWK_HOST_LABEL=writable-label\n")
		if err := os.Chmod(path, mode); err != nil {
			t.Fatalf("chmod: %v", err)
		}
		var warn bytes.Buffer
		assertRefused(t, readDotenv(path, fakeOpts(shell, 5*time.Second), &warn), warn.String(), path,
			"group- or world-writable", "writable-label")
	}
}

// TestStatOwner_ReportsCurrentUser pins the production owner seam: a file this
// process created is reported as this uid, or no .env would ever be sourced.
func TestStatOwner_ReportsCurrentUser(t *testing.T) {
	path := writeDotenv(t, t.TempDir(), "")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if uid, ok := statOwner(info); !ok || uid != os.Getuid() {
		t.Fatalf("statOwner = (%d, %v), want (%d, true)", uid, ok, os.Getuid())
	}
}

// sysFileInfo is an fs.FileInfo whose Sys() is whatever the test gives it.
type sysFileInfo struct {
	fs.FileInfo
	sys any
}

func (i sysFileInfo) Sys() any { return i.sys }

// TestStatOwner_FallsClosedOnForeignSys pins the fallbacks a non-unix GOOS
// would reach: a Sys() with no readable unsigned Uid reports false, so the
// file is refused as not owned by the current user.
func TestStatOwner_FallsClosedOnForeignSys(t *testing.T) {
	type noUid struct{ Gid uint32 }
	type stringUid struct{ Uid string }
	cases := []struct {
		name string
		sys  any
	}{
		{"nil", nil},
		{"nil pointer", (*syscall.Stat_t)(nil)},
		{"non-struct", uint32(os.Getuid())},
		{"pointer to non-struct", new(int)},
		{"struct without Uid", noUid{Gid: 1}},
		{"pointer to struct without Uid", &noUid{Gid: 1}},
		{"non-unsigned Uid", stringUid{Uid: strconv.Itoa(os.Getuid())}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if uid, ok := statOwner(sysFileInfo{sys: tc.sys}); ok {
				t.Fatalf("statOwner(Sys() = %#v) = (%d, true), want false", tc.sys, uid)
			}
		})
	}
	// The happy shape through the same fake: a struct with an unsigned Uid.
	if uid, ok := statOwner(sysFileInfo{sys: &syscall.Stat_t{Uid: 4242}}); !ok || uid != 4242 {
		t.Fatalf("statOwner(&Stat_t{Uid: 4242}) = (%d, %v), want (4242, true)", uid, ok)
	}
}

// TestReadDotenv_NilOwnerUsesStatOwner pins the nil-seam default: a zero
// dotenvOpts.owner falls back to statOwner, so an operator's own file is
// sourced rather than refused.
func TestReadDotenv_NilOwnerUsesStatOwner(t *testing.T) {
	path := writeDotenv(t, t.TempDir(), "# fake\n")
	shell := writeFakeShell(t, shFrame("nil-owner", "4"))
	var warn bytes.Buffer
	got := readDotenv(path, dotenvOpts{shell: shell, timeout: 5 * time.Second, waitDelay: dotenvWaitDelay}, &warn)
	if want := map[string]string{hostLabelKey: "nil-owner", hostCapacityKey: "4"}; !mapsEqual(got, want) || warn.Len() != 0 {
		t.Fatalf("readDotenv with a nil owner = %v (warn %q), want %v", got, warn.String(), want)
	}
}

func TestReadDotenv_MissingShellWarnsAndSourcesNothing(t *testing.T) {
	path := writeDotenv(t, t.TempDir(), "FISHHAWK_HOST_LABEL=no-zsh-label\n")
	t.Setenv("PATH", t.TempDir())
	var warn bytes.Buffer
	assertRefused(t, readDotenv(path, defaultDotenvOpts, &warn), warn.String(), path, "zsh not found on PATH", "no-zsh-label")
}

// processGone polls until pid has exited. A killed orphan stays a zombie
// until its new parent reaps it, and a container's PID 1 may never reap, so a
// zombie counts as gone.
func processGone(pid int, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) || isZombie(pid) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// isZombie reports whether pid is an exited, unreaped process: from
// /proc/<pid>/stat on linux, else from ps.
func isZombie(pid int) bool {
	if b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat"); err == nil {
		if i := bytes.LastIndexByte(b, ')'); i >= 0 && i+2 < len(b) {
			return b[i+2] == 'Z'
		}
		return false
	}
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	return err == nil && strings.HasPrefix(strings.TrimSpace(string(out)), "Z")
}

// readPidFile reads the pid the fake shell recorded for its sleep grandchild
// and registers a cleanup that kills it, so a failing run leaks nothing.
func readPidFile(t *testing.T, path string) int {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the fake shell never recorded its grandchild pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		t.Fatalf("pid file %q: %v", b, err)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	return pid
}

// TestReadDotenv_TimeoutSourcesNothing pins the timeout and the group kill: the
// fake shell would print a valid frame after its sleep grandchild finishes, so
// only the timeout keeps it from being sourced, and the grandchild must be
// dead once readDotenv returns. WaitDelay is lengthened past the sleep, so a
// direct-process kill would leave the sleep holding stdout and Run would not
// return for 8s; only the timeout's group kill keeps the return near 1s (the
// reap after Run would still kill the sleep, so the elapsed bound carries it).
func TestReadDotenv_TimeoutSourcesNothing(t *testing.T) {
	path := writeDotenv(t, t.TempDir(), "FISHHAWK_HOST_LABEL=after-sleep\n")
	pidFile := filepath.Join(t.TempDir(), "sleep.pid")
	shell := writeFakeShell(t, "sleep 8 &\necho $! > '"+pidFile+"'\nwait\n"+shFrame("after-sleep", "4"))
	const timeout = time.Second
	o := fakeOpts(shell, timeout)
	o.waitDelay = 30 * time.Second
	var warn bytes.Buffer
	start := time.Now()
	got := readDotenv(path, o, &warn)
	elapsed := time.Since(start)
	assertRefused(t, got, warn.String(), path, "timed out after 1s", "after-sleep")
	if elapsed > 3*timeout {
		t.Errorf("readDotenv took %s, want < %s", elapsed, 3*timeout)
	}
	pid := readPidFile(t, pidFile)
	if !processGone(pid, 2*time.Second) {
		t.Errorf("sleep grandchild %d is still alive after readDotenv returned: the timeout must kill the whole process group", pid)
	}
}

// TestReadDotenv_WaitDelayBoundsHeldPipe pins WaitDelay: the fake shell prints
// a valid frame and exits 0, but leaves a sleep holding stdout. Without
// WaitDelay, Wait would block until the sleep ends and the frame would be
// sourced. The sleep must also be dead once readDotenv returns: the group is
// reaped on this refusal too, not only on the timeout.
func TestReadDotenv_WaitDelayBoundsHeldPipe(t *testing.T) {
	path := writeDotenv(t, t.TempDir(), "FISHHAWK_HOST_LABEL=held-pipe\n")
	pidFile := filepath.Join(t.TempDir(), "sleep.pid")
	shell := writeFakeShell(t, "sleep 10 &\necho $! > '"+pidFile+"'\n"+shFrame("held-pipe", "4")+"\nexit 0")
	var warn bytes.Buffer
	start := time.Now()
	got := readDotenv(path, fakeOpts(shell, 30*time.Second), &warn)
	elapsed := time.Since(start)
	pid := readPidFile(t, pidFile)
	assertRefused(t, got, warn.String(), path, "left a process holding its output open", "held-pipe")
	if bound := 3 * (dotenvWaitDelay + time.Second/3); elapsed > bound {
		t.Errorf("readDotenv took %s, want < %s (WaitDelay must bound a held pipe)", elapsed, bound)
	}
	if !processGone(pid, 2*time.Second) {
		t.Errorf("sleep grandchild %d is still alive after readDotenv returned: the group must be reaped on every exit path", pid)
	}
}

// TestReadDotenv_ReapsGroupOnEveryExit pins the reap on the paths that hold
// no pipe: a sleep with its stdout on the null device outlives the fake shell
// unless readDotenv kills the group after Run returns, on a refused non-zero
// exit and on a clean, sourced exit alike.
func TestReadDotenv_ReapsGroupOnEveryExit(t *testing.T) {
	cases := []struct {
		name, tail string
		want       map[string]string
		reason     string
	}{
		{"non-zero exit", "exit 3", nil, "exited unsuccessfully (exit status 3)"},
		{"clean exit", "exit 0", map[string]string{hostLabelKey: "reaped", hostCapacityKey: "4"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeDotenv(t, t.TempDir(), "FISHHAWK_HOST_LABEL=reaped\n")
			pidFile := filepath.Join(t.TempDir(), "sleep.pid")
			shell := writeFakeShell(t, "sleep 10 >/dev/null 2>&1 &\necho $! > '"+pidFile+"'\n"+shFrame("reaped", "4")+"\n"+tc.tail)
			var warn bytes.Buffer
			got := readDotenv(path, fakeOpts(shell, 30*time.Second), &warn)
			pid := readPidFile(t, pidFile)
			if tc.want == nil {
				assertRefused(t, got, warn.String(), path, tc.reason, "reaped")
			} else if !mapsEqual(got, tc.want) || warn.Len() != 0 {
				t.Fatalf("readDotenv = %v (warn %q), want %v", got, warn.String(), tc.want)
			}
			if !processGone(pid, 2*time.Second) {
				t.Errorf("sleep grandchild %d is still alive after readDotenv returned: the group must be reaped on every exit path", pid)
			}
		})
	}
}

// TestReadDotenv_NonZeroExitSourcesNothing: the fake shell prints a VALID frame
// and then exits 3, so the exit check is the only guard.
func TestReadDotenv_NonZeroExitSourcesNothing(t *testing.T) {
	path := writeDotenv(t, t.TempDir(), "FISHHAWK_HOST_LABEL=faked\n")
	shell := writeFakeShell(t, shFrame("faked", "4")+"\nexit 3")
	var warn bytes.Buffer
	assertRefused(t, readDotenv(path, fakeOpts(shell, 5*time.Second), &warn), warn.String(), path,
		"exited unsuccessfully (exit status 3)", "faked")
}

// TestReadDotenv_ShellFailsToStartSourcesNothing: the shell resolves through
// LookPath (an executable regular file) but exec fails on its missing
// interpreter, so Run returns neither a timeout, ErrWaitDelay nor an
// *exec.ExitError. The generic refusal is the only guard; with it gone the
// empty output would be refused as a malformed frame instead.
func TestReadDotenv_ShellFailsToStartSourcesNothing(t *testing.T) {
	path := writeDotenv(t, t.TempDir(), "FISHHAWK_HOST_LABEL=never-started\n")
	shell := filepath.Join(t.TempDir(), "bad-interpreter-zsh")
	if err := os.WriteFile(shell, []byte("#!/nonexistent/fishhawk-no-such-interpreter\n"), 0o755); err != nil { //nolint:gosec // test fixture script
		t.Fatalf("write fake shell: %v", err)
	}
	if _, err := exec.LookPath(shell); err != nil {
		t.Fatalf("fixture: LookPath must find the fake shell: %v", err)
	}
	var warn bytes.Buffer
	assertRefused(t, readDotenv(path, fakeOpts(shell, 5*time.Second), &warn), warn.String(), path,
		"exited unsuccessfully; sourcing nothing", "never-started", "exit status")
}

// TestReadDotenv_ShellSeesMinimalEnv pins that the evaluator gets PATH and
// HOME only: no FISHHAWK_* variable of the shim's reaches it.
func TestReadDotenv_ShellSeesMinimalEnv(t *testing.T) {
	t.Setenv(hostLabelKey, "inherited")
	t.Setenv("FISHHAWK_DOTENV_ONLY_PROBE", "leaked")
	path := writeDotenv(t, t.TempDir(), "# empty\n")
	shell := writeFakeShell(t, "printf '"+dotenvFrameMagic+`\000scalar-export\000%s\000scalar-export\000%s\000' `+
		`"${FISHHAWK_DOTENV_ONLY_PROBE:-clean}" "${FISHHAWK_HOST_LABEL:-none}"`)
	var warn bytes.Buffer
	got := readDotenv(path, fakeOpts(shell, 5*time.Second), &warn)
	want := map[string]string{hostLabelKey: "clean", hostCapacityKey: "none"}
	if !mapsEqual(got, want) {
		t.Fatalf("readDotenv = %v, want %v: the evaluator saw the shim's own env (warn %q)", got, want, warn.String())
	}
}

// TestReadDotenv_OversizedOutputSourcesNothing: the frame is VALID but 5000
// bytes of label put it over the 4 KiB cap, so the cap is the only guard.
func TestReadDotenv_OversizedOutputSourcesNothing(t *testing.T) {
	path := writeDotenv(t, t.TempDir(), "# big\n")
	big := strings.Repeat("x", 5000)
	shell := writeFakeShell(t, shFrame(big, "4"))
	var warn bytes.Buffer
	assertRefused(t, readDotenv(path, fakeOpts(shell, 5*time.Second), &warn), warn.String(), path,
		"printed more than 4 KiB", big[:64])
}

// TestReadDotenv_ExitInFileSourcesNothing: `exit 0` in the file ends zsh before
// the frame, so zsh exits 0 with no output.
func TestReadDotenv_ExitInFileSourcesNothing(t *testing.T) {
	requireZsh(t)
	path := writeDotenv(t, t.TempDir(), "FISHHAWK_HOST_LABEL=exit-label\nexit 0\n")
	var warn bytes.Buffer
	assertRefused(t, readDotenv(path, defaultDotenvOpts, &warn), warn.String(), path, "unexpected output from zsh", "exit-label")
}

// TestReadDotenv_NulInValueSourcesNothing: a NUL in a value grows the frame to
// 7 fields, and it is refused rather than truncated.
func TestReadDotenv_NulInValueSourcesNothing(t *testing.T) {
	requireZsh(t)
	path := writeDotenv(t, t.TempDir(), "FISHHAWK_HOST_LABEL=$'lab\\0el'\n")
	var warn bytes.Buffer
	assertRefused(t, readDotenv(path, defaultDotenvOpts, &warn), warn.String(), path, "unexpected output from zsh", "lab")
}

// TestReadDotenv_SkipsStartupFiles pins `zsh -f`: an EXPORTED assignment in
// $HOME/.zshenv is not sourced. (A non-exported one would be masked by
// dotenvExported, so the fixture exports it.)
func TestReadDotenv_SkipsStartupFiles(t *testing.T) {
	requireZsh(t)
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, ".zshenv"), []byte("export FISHHAWK_HOST_LABEL=from-zshenv\n"), 0o600); err != nil {
		t.Fatalf("write .zshenv: %v", err)
	}
	t.Setenv("HOME", home)
	path := writeDotenv(t, t.TempDir(), "FISHHAWK_HOST_CAPACITY=4\n")
	var warn bytes.Buffer
	got := readDotenv(path, defaultDotenvOpts, &warn)
	if want := map[string]string{hostCapacityKey: "4"}; !mapsEqual(got, want) {
		t.Fatalf("readDotenv = %v, want %v: zsh read a startup file", got, want)
	}
}

func TestParseDotenvFrame(t *testing.T) {
	frame := func(fields ...string) []byte { return []byte(strings.Join(fields, "\x00")) }
	cases := []struct {
		name   string
		out    []byte
		want   map[string]string
		wantOK bool
	}{
		{"valid", frame(dotenvFrameMagic, "scalar-export", "lab", "integer-export", "4", ""),
			map[string]string{hostLabelKey: "lab", hostCapacityKey: "4"}, true},
		{"both unset", frame(dotenvFrameMagic, "", "", "", "", ""), map[string]string{}, true},
		{"non-exported types", frame(dotenvFrameMagic, "scalar", "lab", "array-export", "4", ""), map[string]string{}, true},
		{"wrong magic", frame("fishhawk-dotenv-v2", "scalar-export", "lab", "", "", ""), nil, false},
		{"seven fields (NUL in a value)", frame(dotenvFrameMagic, "scalar-export", "lab", "el", "", "", ""), nil, false},
		{"missing final NUL", frame(dotenvFrameMagic, "scalar-export", "lab", "scalar-export", "4x"), nil, false},
		{"empty output", nil, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseDotenvFrame(tc.out)
			if ok != tc.wantOK || !mapsEqual(got, tc.want) || (tc.want == nil) != (got == nil) {
				t.Fatalf("parseDotenvFrame = (%v, %v), want (%v, %v)", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func TestDotenvExported(t *testing.T) {
	for typ, want := range map[string]bool{
		"scalar-export":          true,
		"integer-export":         true,
		"float-export":           true,
		"scalar-readonly-export": true,
		"scalar":                 false,
		"array-export":           false,
		"association-export":     false,
		"":                       false,
	} {
		if got := dotenvExported(typ); got != want {
			t.Errorf("dotenvExported(%q) = %v, want %v", typ, got, want)
		}
	}
}

// lookupLast resolves key with the exec duplicate-key rule: the last entry
// wins.
func lookupLast(env []string, key string) (string, bool) {
	val, ok := "", false
	for _, kv := range env {
		if k, v, _ := strings.Cut(kv, "="); k == key {
			val, ok = v, true
		}
	}
	return val, ok
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}

func TestMergeDotenv_SourcesAllowlistedKeys(t *testing.T) {
	var warn bytes.Buffer
	env := mergeDotenv([]string{"PATH=/bin"}, map[string]string{hostLabelKey: "dotenv-label", hostCapacityKey: "4"}, "/c/.env", &warn)
	if v, ok := lookupLast(env, hostLabelKey); !ok || v != "dotenv-label" {
		t.Errorf("label = (%q, %v), want dotenv-label", v, ok)
	}
	if v, ok := lookupLast(env, hostCapacityKey); !ok || v != "4" {
		t.Errorf("capacity = (%q, %v), want 4", v, ok)
	}
	if v, _ := lookupLast(env, "PATH"); v != "/bin" {
		t.Errorf("base env lost: PATH = %q", v)
	}
}

// TestMergeDotenv_ExplicitEnvWins pins the presence rule across ABSENT, SET
// and SET-but-empty, resolved by last-wins lookup the way exec resolves it.
func TestMergeDotenv_ExplicitEnvWins(t *testing.T) {
	sourced := map[string]string{hostLabelKey: "dotenv-label"}
	cases := []struct {
		name    string
		base    []string
		want    string
		wantSet bool
	}{
		{"absent", []string{"PATH=/bin"}, "dotenv-label", true},
		{"set", []string{hostLabelKey + "=explicit-label"}, "explicit-label", true},
		{"set but empty", []string{hostLabelKey + "="}, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var warn bytes.Buffer
			env := mergeDotenv(tc.base, sourced, "/c/.env", &warn)
			if v, ok := lookupLast(env, hostLabelKey); v != tc.want || ok != tc.wantSet {
				t.Fatalf("label = (%q, %v), want (%q, %v); env %q", v, ok, tc.want, tc.wantSet, env)
			}
		})
	}
}

func TestMergeDotenv_SkipsEmptyAndInvalidCapacity(t *testing.T) {
	for _, bad := range []string{"abc", "0", "65", ""} {
		t.Run(strconv.Quote(bad), func(t *testing.T) {
			var warn bytes.Buffer
			env := mergeDotenv(nil, map[string]string{hostLabelKey: "dotenv-label", hostCapacityKey: bad}, "/c/.env", &warn)
			if v, ok := lookupLast(env, hostCapacityKey); ok {
				t.Fatalf("capacity %q was sourced as %q", bad, v)
			}
			if v, _ := lookupLast(env, hostLabelKey); v != "dotenv-label" {
				t.Fatalf("label = %q, want it still sourced", v)
			}
			if bad == "" {
				if strings.Contains(warn.String(), "not an integer") {
					t.Fatalf("an empty value is skipped silently, got %q", warn.String())
				}
				return
			}
			w := warn.String()
			if !strings.Contains(w, hostCapacityKey+" is not an integer in 1..64") {
				t.Fatalf("warning %q does not name the key and the range", w)
			}
			// Neither the fixed text nor /c/.env contains abc, 0 or 65, so
			// any occurrence of the value is a leak, in whatever framing.
			if strings.Contains(w, bad) {
				t.Fatalf("warning %q leaks the value %q", w, bad)
			}
		})
	}
}

func TestMergeDotenv_LogsKeysNotValues(t *testing.T) {
	var warn bytes.Buffer
	mergeDotenv(nil, map[string]string{hostLabelKey: "secret-looking-label", hostCapacityKey: "7"}, "/c/.env", &warn)
	w := warn.String()
	if strings.Count(w, "\n") != 1 || !strings.Contains(w, hostLabelKey) || !strings.Contains(w, hostCapacityKey) || !strings.Contains(w, "/c/.env") {
		t.Fatalf("summary %q must be one line naming both keys and the file", w)
	}
	if strings.Contains(w, "secret-looking-label") || strings.Contains(w, "=7") {
		t.Fatalf("summary %q leaks a value", w)
	}
}

// TestValidDotenvCapacity_ParityWithMcpserver pins that the shim never sources
// a capacity fishhawk-mcp would refuse, and never skips one it would accept.
func TestValidDotenvCapacity_ParityWithMcpserver(t *testing.T) {
	for _, in := range []string{"1", "3", "64", "0", "65", "-1", "abc", "", " ", "4\r", " 4 ", "+2", "4.0", "0x4"} {
		want := mcpserver.ValidateHostCapacityEnv(func(string) (string, bool) { return in, true }) == nil
		if got := validDotenvCapacity(in); got != want {
			t.Errorf("validDotenvCapacity(%q) = %v, fishhawk-mcp accepts it: %v", in, got, want)
		}
	}
}

func TestChildEnv_EmptyEnvFileDisables(t *testing.T) {
	base := []string{"PATH=/bin"}
	var warn bytes.Buffer
	shell := writeFakeShell(t, shFrame("never", "4"))
	got := childEnv(base, "", fakeOpts(shell, 5*time.Second), &warn)
	if len(got) != 1 || got[0] != "PATH=/bin" || warn.Len() != 0 {
		t.Fatalf("childEnv with no env file = %q (warn %q), want the base env untouched", got, warn.String())
	}
	// A configured file is read through the same path.
	path := writeDotenv(t, t.TempDir(), "# fake\n")
	got = childEnv(base, path, fakeOpts(shell, 5*time.Second), &warn)
	if v, _ := lookupLast(got, hostLabelKey); v != "never" {
		t.Fatalf("childEnv with an env file = %q, want the sourced label", got)
	}
}

func TestDefaultEnvFile_ResolvesCheckoutDotenv(t *testing.T) {
	got := defaultEnvFile(func() (string, error) { return "/repo/bin/fishhawk-mcp-shim", nil })
	if got != "/repo/.env" {
		t.Fatalf("defaultEnvFile = %q, want /repo/.env", got)
	}
	if got := defaultEnvFile(func() (string, error) { return "", errors.New("no exe") }); got != "" {
		t.Fatalf("defaultEnvFile on an executable error = %q, want empty", got)
	}
}

// TestCheckoutEnvFile_OnlyInsideACheckout pins S0-1: the default env file is
// sourced only when its directory has .git and a regular scripts/dev. A
// release layout (shim in <tmp>/bin, nothing else beside it) sources nothing
// and says so once, naming the path and the --env-file remedy.
func TestCheckoutEnvFile_OnlyInsideACheckout(t *testing.T) {
	mk := func(t *testing.T, git, dev bool, devIsDir bool) string {
		root := t.TempDir()
		if err := os.Mkdir(filepath.Join(root, "bin"), 0o700); err != nil {
			t.Fatal(err)
		}
		if git {
			if err := os.Mkdir(filepath.Join(root, ".git"), 0o700); err != nil {
				t.Fatal(err)
			}
		}
		if dev {
			devPath := filepath.Join(root, "scripts", "dev")
			if devIsDir {
				if err := os.MkdirAll(devPath, 0o700); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.MkdirAll(filepath.Dir(devPath), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(devPath, []byte("#!/bin/zsh\n"), 0o700); err != nil { //nolint:gosec // fixture
					t.Fatal(err)
				}
			}
		}
		writeDotenv(t, root, "FISHHAWK_HOST_LABEL=release-label\n")
		return defaultEnvFile(func() (string, error) { return filepath.Join(root, "bin", "fishhawk-mcp-shim"), nil })
	}
	cases := []struct {
		name               string
		git, dev, devIsDir bool
		wantSourced        bool
	}{
		{"checkout", true, true, false, true},
		{"release layout", false, false, false, false},
		{"no .git", false, true, false, false},
		{"no scripts/dev", true, false, false, false},
		{"scripts/dev is a directory", true, true, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := mk(t, tc.git, tc.dev, tc.devIsDir)
			var warn bytes.Buffer
			got := checkoutEnvFile(path, &warn)
			if tc.wantSourced {
				if got != path || warn.Len() != 0 {
					t.Fatalf("checkoutEnvFile = %q (warn %q), want %q silently", got, warn.String(), path)
				}
				return
			}
			w := warn.String()
			if got != "" {
				t.Fatalf("checkoutEnvFile = %q, want empty outside a checkout", got)
			}
			if strings.Count(w, "\n") != 1 || !strings.Contains(w, path) || !strings.Contains(w, "--env-file") {
				t.Fatalf("warning %q must be one line naming %q and --env-file", w, path)
			}
			if strings.Contains(w, "release-label") {
				t.Fatalf("warning %q leaks a value", w)
			}
		})
	}
	var warn bytes.Buffer
	if got := checkoutEnvFile("", &warn); got != "" || warn.Len() != 0 {
		t.Fatalf("checkoutEnvFile(\"\") = %q (warn %q), want empty and silent", got, warn.String())
	}
}
