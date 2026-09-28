//go:build windows

package procsweep

import "context"

// readTable is a refusing stub on Windows: there is no `ps`, and the runner is
// not currently supported there. Returning ErrUnsupported makes the caller take
// its named fail-open degrade (one printed reason, zero kills). Mirrors
// signalLockHolder's Windows stub in the runner command.
var readTable = func(context.Context) (Table, error) {
	return Table{}, ErrUnsupported
}
