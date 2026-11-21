//go:build windows

package mcpserver

import (
	"fmt"
	"path/filepath"
)

// acquireHostDispatchLock fails closed on Windows: the state-dir lock is an
// flock(2), which Windows does not provide, so a dispatcher here would run
// unlocked. It opens and creates nothing. This stub keeps the package
// buildable on Windows; it is defence in depth, because a Windows start is
// already refused earlier by the state-dir owner check (state_dir_not_owned).
func acquireHostDispatchLock(dir string) (*hostDispatchLock, error) {
	return nil, fmt.Errorf("%s: host-dispatch locks %s with flock(2), which Windows does not provide; run host-dispatch on Linux or macOS",
		hdStateDirLocked, filepath.Join(dir, hostDispatchLockFile))
}
