//go:build windows

package main

import "syscall"

// signalLockHolder is a refusing stub on Windows: there is no portable
// process-group signal, and the runner is not currently supported there.
// Returning errSignalUnsupported makes the lineage-lock eviction degrade to
// its refuse branch (the lockfile is left intact and the acquire fails loud),
// which is the same fail-closed posture every other unprovable branch takes.
// Mirrors run_stage_windows.go on the MCP side.
var signalLockHolder = func(_ int, _ syscall.Signal) error {
	return errSignalUnsupported
}
