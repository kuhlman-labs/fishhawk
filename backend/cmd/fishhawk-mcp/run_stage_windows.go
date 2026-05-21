//go:build windows

package main

import (
	"os/exec"
	"syscall"
)

// runStageSetProcessGroup is a no-op on Windows. The runner is
// not currently supported on Windows; if/when it is, the
// equivalent is to spawn with `CREATE_NEW_PROCESS_GROUP` via
// SysProcAttr.CreationFlags. Keeping the stub here so the
// non-Unix build still compiles.
var runStageSetProcessGroup = func(_ *exec.Cmd) {}

// runStageSignalGroup falls back to signalling just the direct
// child on Windows. Same caveats as the setup stub.
var runStageSignalGroup = func(cmd *exec.Cmd, sig syscall.Signal) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Signal(sig)
}
