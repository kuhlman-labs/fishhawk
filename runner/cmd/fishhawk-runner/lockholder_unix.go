//go:build !windows

package main

import "syscall"

// signalLockHolder delivers sig to a lineage-lock holder the caller has
// ALREADY authorized (evictTerminalLockHolder proved the pid's argv is a
// fishhawk-runner carrying the cancelled holder's run id — see its (e)
// branch). This function makes no authorization decision of its own.
//
// The group test is a SIGNAL SELECTOR, not a safety control: every runner this
// product spawns is started with SysProcAttr{Setpgid: true}, so it leads a new
// group whose pgid equals its own pid. When that holds, signalling the GROUP
// (`kill(-pgid, sig)`) also reaches the agent subprocesses the runner forked,
// which is what actually frees the lineage. When it does NOT hold — the pid is
// not a group leader — we fall back to the single pid rather than signal a
// group the holder merely belongs to (which on a non-leader pid would be
// somebody else's group, up to and including our own).
//
// Injectable: the survived-TERM+KILL test replaces it with a recorder that
// does NOT deliver the signals, so "the signals were sent" and "the holder is
// still alive" are both literally true in that test.
var signalLockHolder = func(pid int, sig syscall.Signal) error {
	if pid <= 0 {
		return syscall.ESRCH
	}
	if pgid, err := syscall.Getpgid(pid); err == nil && pgid == pid {
		return syscall.Kill(-pgid, sig)
	}
	return syscall.Kill(pid, sig)
}
