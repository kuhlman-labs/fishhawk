//go:build !unix

package main

import "os/exec"

// setPgid is a no-op on non-unix GOOS: there is no portable process-group
// primitive. The shim runs on linux (CI + prod container) and darwin (dev);
// this stub exists so the package still compiles under `go build` on any GOOS.
func setPgid(cmd *exec.Cmd) {}

// signalTerm degrades to no-op on non-unix GOOS — the SIGKILL escalation in
// signalKillGroup still bounds shutdown.
func signalTerm(cmd *exec.Cmd) {}

// signalKillGroup degrades to a direct single-process kill on non-unix GOOS.
// The group-reap guarantee is unavailable, but the child is still terminated.
func signalKillGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
}
