//go:build !unix

package procgroup

import "os/exec"

// setpgid is a no-op on non-unix GOOS: there is no portable process-group
// primitive. fishhawkd runs only on linux (CI + prod container) and darwin
// (dev); this stub exists so the package still compiles under `go build` on any
// GOOS without a syscall.Setpgid reference.
func setpgid(cmd *exec.Cmd) {}

// killGroup degrades to a direct single-process kill on non-unix GOOS. The
// group-reap guarantee is unavailable, but Harden's WaitDelay still bounds
// cmd.Wait so a pipe-holding grandchild cannot hang the call indefinitely. A
// nil Process (never started) is a no-op.
func killGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
