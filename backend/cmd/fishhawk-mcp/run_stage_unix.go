//go:build !windows

package main

import (
	"os/exec"
	"syscall"
)

// runStageSetProcessGroup attaches a fresh process group to cmd
// before Start. Unix semantics: SysProcAttr.Setpgid makes the
// child the leader of a new group with Pgid == its own PID, so
// `kill(-pgid, sig)` from `runStageSignalGroup` reaches the
// child and every descendant that hasn't broken away with its
// own setpgid.
//
// Test seam: exposed as `var` so the package-level setter in
// tests can no-op it (the fake subprocess for the unit tests
// shouldn't run in a separate group — the test parent needs to
// be able to observe the children directly).
var runStageSetProcessGroup = func(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// runStageSignalGroup sends sig to the process group cmd was
// started in. Falls back to signalling the direct child if the
// group wasn't set up (defensive — should never fire given the
// always-on setup in runStage).
//
// The minus on the PID is the POSIX convention for "signal the
// process group with that ID." Errors are swallowed because
// missing-process is normal (race with subprocess exit) and we
// can't recover from EPERM anyway.
var runStageSignalGroup = func(cmd *exec.Cmd, sig syscall.Signal) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		_ = cmd.Process.Signal(sig)
		return
	}
	_ = syscall.Kill(-pgid, sig)
}
