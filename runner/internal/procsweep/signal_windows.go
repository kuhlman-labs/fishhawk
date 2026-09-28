//go:build windows

package procsweep

// killPID is a refusing stub on Windows, for the same reason readTable is: the
// runner is not supported there and there is no portable process-group signal.
var killPID = func(int) error {
	return ErrUnsupported
}

// SelfPGID has no meaning on Windows: there are no POSIX process groups. It
// reports 0, which never equals a real pid, so the runner's group-leader test
// reads as "not a leader" and the sweep takes its named degrade.
func SelfPGID(int) int { return 0 }
