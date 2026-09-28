//go:build !windows

package procsweep

import (
	"context"
	"fmt"
	"os/exec"
	"time"
)

// procTableReadTimeout bounds the `ps` exec, mirroring the bounded-exec shape
// the runner command's processArgv uses for its own ps probe.
const procTableReadTimeout = 5 * time.Second

// readTable reads the host process table via ps. Injectable: every selection
// test drives a synthetic Table through it, and the sweep-time re-read contract
// is pinned by a stub that returns DIFFERENT tables at sample time and at sweep
// time.
//
// The column set is exactly the four identity fields plus the command, with the
// `=` header suppressors so no header line has to be skipped:
//
//	1     0     1 Tue Aug 11 11:57:20 2026     /sbin/launchd
//
// i.e. three leading integers, lstart as five whitespace-separated tokens, then
// the command as the remainder. VERIFIED on Darwin 25.6; procps-ng renders
// lstart in the same ctime-style five-token form.
var readTable = func(ctx context.Context) (Table, error) {
	ctx, cancel := context.WithTimeout(ctx, procTableReadTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-axo", "pid=,ppid=,pgid=,lstart=,command=").Output()
	if err != nil {
		return Table{}, fmt.Errorf("procsweep: read process table: %w", err)
	}
	return parseTable(string(out)), nil
}
