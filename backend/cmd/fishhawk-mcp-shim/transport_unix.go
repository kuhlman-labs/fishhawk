//go:build unix

package main

import (
	"os/exec"
	"syscall"
)

// setPgid makes the child its own process-group leader (pgid == pid) so a
// single kill(-pgid) reaches the child and every grandchild it forks that did
// not itself change process group — the grandchild-holds-the-stdout-pipe class
// (mirrors backend/internal/procgroup). It preserves any SysProcAttr already set.
func setPgid(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// signalTerm SIGTERMs the direct child, giving it a chance to shut down cleanly
// before the group SIGKILL escalation.
func signalTerm(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
}

// signalKillGroup SIGKILLs the whole process group led by the child. Signalling
// the NEGATIVE pid targets the group; ESRCH ("no such process" — the group is
// already gone) is success and is swallowed.
func signalKillGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
