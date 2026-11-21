//go:build !windows

package mcpserver

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
)

// acquireHostDispatchLock takes the lock without blocking. A held lock refuses
// state_dir_locked, naming the pid the holder wrote (diagnostics only: the
// flock, not the file content, is the lock).
func acquireHostDispatchLock(dir string) (*hostDispatchLock, error) {
	path := filepath.Join(dir, hostDispatchLockFile)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o600) //nolint:gosec // operator state dir
	if err != nil {
		return nil, fmt.Errorf("%s: open %s: %w", hdStateDirLocked, path, err)
	}
	if ferr := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); ferr != nil { //nolint:gosec // an fd fits an int
		_ = f.Close()
		if errors.Is(ferr, syscall.EWOULDBLOCK) {
			holder := "unknown"
			if pid, rerr := readHostDispatchLock(path); rerr == nil {
				holder = strconv.Itoa(pid)
			}
			return nil, fmt.Errorf("%s: another dispatcher (pid %s) holds %s; stop it first", hdStateDirLocked, holder, path)
		}
		return nil, fmt.Errorf("%s: flock %s: %w", hdStateDirLocked, path, ferr)
	}
	return finishHostDispatchLock(f, path)
}
