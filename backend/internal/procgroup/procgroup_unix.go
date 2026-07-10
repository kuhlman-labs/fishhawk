//go:build unix

package procgroup

import (
	"errors"
	"os/exec"
	"syscall"
)

// setpgid makes the child its own process-group leader (pgid == pid) so a
// single kill(-pgid) reaches the child and every grandchild it forks that did
// not itself change process group. It preserves any SysProcAttr the caller
// already set.
func setpgid(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// killGroup SIGKILLs the whole process group led by the child. Signalling the
// NEGATIVE pid targets the group (pgid == the leader's pid, set by setpgid), so
// a grandchild that inherited the child's stdout write-end is reaped too —
// closing the pipe writer so cmd.Output()'s reader sees EOF.
//
// A nil Process (Harden'd cmd never started) is a no-op. ESRCH ("no such
// process") means the group is already gone — every member exited or escaped —
// which is success from the cancel's point of view, so it is reported as nil
// rather than surfacing as a Cancel error that would displace ctx.Err() on the
// returned error.
func killGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}
