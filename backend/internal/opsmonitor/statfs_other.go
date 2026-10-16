//go:build !(darwin || linux)

package opsmonitor

import "errors"

// statfsBytes is unsupported off darwin/linux; the disk probe reads unknown.
func statfsBytes(string) (avail, total uint64, err error) {
	return 0, 0, errors.New("statfs is not supported on this platform")
}
