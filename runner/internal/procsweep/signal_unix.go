//go:build !windows

package procsweep

import "syscall"

// getpgid is injectable so the wiring tests can pose a process as its own group
// leader or as a member of somebody else's group.
var getpgid = syscall.Getpgid

// SelfPGID returns pid's process-group id, or 0 when it cannot be determined.
// Exported because the runner command needs the group-leader test (pgid == pid)
// that arms Targets' group-leader guard, and syscall.Getpgid does not exist on
// every platform this package builds for.
func SelfPGID(pid int) int {
	if pg, err := getpgid(pid); err == nil {
		return pg
	}
	return 0
}

// killPID delivers SIGKILL to a single pid the caller has ALREADY authorised
// (Targets made the decision; this function makes none of its own). SIGKILL
// rather than SIGTERM because the class this reaps is a process that already
// escaped every graceful teardown the stage performs.
//
// Deliberately pid-addressed, never `kill(-pgid)`: the recorded pgid set is an
// ADMISSION credential, not a kill target — a group-addressed signal would
// reach members Targets never admitted and never fenced.
//
// Injectable: the selection and failure-mode tests replace it with a recorder
// that does NOT deliver the signal, so "the kill was attempted" is assertable
// without any real process dying.
var killPID = func(pid int) error {
	if pid <= 1 {
		return syscall.ESRCH
	}
	return syscall.Kill(pid, syscall.SIGKILL)
}
