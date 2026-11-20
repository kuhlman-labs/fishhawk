package mcpserver

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
)

// hostCapacityEnv sizes the host capacity group (ADR-094 D4, #4241): every
// host-dispatched agent stage holds one slot of host:<label>:capacity. When it
// is set, this process sends its value as the host-dispatch marker's
// {"capacity"} on every request; when it is unset, nothing is sent and the
// server uses its default.
const hostCapacityEnv = "FISHHAWK_HOST_CAPACITY"

// Host-capacity sources, as fishhawk_doctor's host_capacity rung reports them.
const (
	hostCapacitySourceEnv     = "env"
	hostCapacitySourceDefault = "default"
	hostCapacitySourceInvalid = "invalid"
)

// defaultHostCapacity, maxHostCapacity, hostScopePrefix and capacityGroupName
// mirror backend/internal/concurrency's DefaultHostCapacity, MaxLimit, the
// host scope and CapacityGroupName. They are copies so the production MCP
// package gains no import edge onto the store package; TestHostCapacityParity
// pins them to the originals.
const (
	defaultHostCapacity = 3
	maxHostCapacity     = 64
	hostScopePrefix     = "host:"
	capacityGroupName   = "capacity"
)

// errInvalidHostCapacity is wrapped by every FISHHAWK_HOST_CAPACITY refusal.
var errInvalidHostCapacity = errors.New("invalid host capacity")

// hostCapacityResolution is this process's host capacity and where it came
// from. Capacity is the value the server admits against: the env value for
// source env, the server default for source default, and 0 for source
// invalid. Raw is the env value as set (source env and invalid only).
type hostCapacityResolution struct {
	Capacity int
	Source   string
	Raw      string
}

// declared reports the capacity to send on the marker: the env value for
// source env, and nothing otherwise, so a process with the variable unset is
// byte-identical on the wire to a client that predates it.
func (r hostCapacityResolution) declared() (int, bool) {
	return r.Capacity, r.Source == hostCapacitySourceEnv
}

// resolveHostCapacity reads FISHHAWK_HOST_CAPACITY through lookupEnv
// (os.LookupEnv in production). Unset is the server default (3), sent as
// nothing. A set value is trimmed and parsed with strconv.Atoi, so "+2" and
// " 2 " are 2; an integer in 1..64 is source env. Anything else, including a
// set-but-empty value, is an error wrapping errInvalidHostCapacity that names
// the variable, the value and the accepted range, with a source-invalid
// resolution for the doctor rung.
func resolveHostCapacity(lookupEnv func(string) (string, bool)) (hostCapacityResolution, error) {
	raw, ok := lookupEnv(hostCapacityEnv)
	if !ok {
		return hostCapacityResolution{Capacity: defaultHostCapacity, Source: hostCapacitySourceDefault}, nil
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n < 1 || n > maxHostCapacity {
		return hostCapacityResolution{Source: hostCapacitySourceInvalid, Raw: raw},
			fmt.Errorf("%w: %s=%q is not an integer in 1..%d (unset it for the default %d)",
				errInvalidHostCapacity, hostCapacityEnv, raw, maxHostCapacity, defaultHostCapacity)
	}
	return hostCapacityResolution{Capacity: n, Source: hostCapacitySourceEnv, Raw: raw}, nil
}

// ValidateHostCapacityEnv refuses an invalid FISHHAWK_HOST_CAPACITY. The
// fishhawk-mcp command calls it at startup with os.LookupEnv so a bad value
// fails the process before it serves anything; RunHostDispatcher calls it
// before taking its state-dir lock. fishhawkd's /mcp route does not: it reads
// the variable lazily (processHostCapacity), so a bad value there fails each
// host-dispatch marker closed instead.
func ValidateHostCapacityEnv(lookupEnv func(string) (string, bool)) error {
	_, err := resolveHostCapacity(lookupEnv)
	return err
}

// resolveProcessHostCapacity is the production host-capacity read:
// resolveHostCapacity over this process's real environment. It is a named
// function, not an inline closure, so TestResolveProcessHostCapacity runs the
// production read itself (the test binary's init pins processHostCapacity).
func resolveProcessHostCapacity() (hostCapacityResolution, error) {
	return resolveHostCapacity(os.LookupEnv)
}

// processHostCapacity is this process's host capacity, resolved lazily and
// once, on first use (the first host-dispatch marker or fishhawk_doctor call),
// like processHostLabel. Config.internal and RunHostDispatcher wire it.
var processHostCapacity = sync.OnceValues(resolveProcessHostCapacity)

// capacityGroupKeyFor is the host capacity group key for label:
// concurrency.CapacityGroupKey, mirrored. An empty label is the server's
// unknown host.
func capacityGroupKeyFor(label string) string {
	if label == "" {
		label = unknownHostLabel
	}
	return hostScopePrefix + label + ":" + capacityGroupName
}
