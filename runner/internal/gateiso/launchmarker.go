package gateiso

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
)

// The launch marker (#5408, ADR-102 Amendment A1) tells a container runtime's
// OWN failure apart from the verify command's exit status.
//
// `docker run` and `podman run` return the contained command's exit status for
// every code EXCEPT their own three: 125 when the runtime (daemon, image,
// create) itself errors, 126 when the contained command cannot be invoked,
// and 127 when it cannot be found
// (https://docs.docker.com/engine/containers/run/#exit-status; podman-run(1)
// "Exit Status"). Those three codes collide with "any other non-zero = the
// change failed": a pre-launch break would otherwise be read as a red tree.
//
// The code alone therefore never decides. A marked gate's argv runs behind a
// runner-owned `sh` wrapper that prints a fresh per-exec runner nonce on its
// own STDOUT line before it execs the verify command. A 125/126/127 exit with
// NO marker line means the command never started (a launch failure); the same
// codes AFTER the marker are the command's own status. The command cannot
// retract an already-printed marker, so it cannot forge a launch failure.

// LaunchMarkerArg0 is the wrapper script's $0.
const LaunchMarkerArg0 = "fishhawk-gate-launch"

// launchMarkerPrefix leads the marker line; the per-exec nonce follows it.
const launchMarkerPrefix = "fishhawk-gate-launched:"

// launchMarkerScript prints the marker line, then replaces itself with the
// original argv. A failed printf exits 125 with no marker, which is correctly
// a launch failure: the command never ran.
const launchMarkerScript = `printf 'fishhawk-gate-launched:%s\n' "$1" || exit 125; shift; exec "$@"`

// NewLaunchNonce returns a fresh 32-character lowercase hex nonce from 16
// bytes of crypto/rand. crypto/rand.Read never returns an error (Go 1.24+:
// it crashes the program instead), so there is no error path.
func NewLaunchNonce() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// WrapLaunchMarker returns `sh -c <script> fishhawk-gate-launch <nonce>
// <argv...>`: a fresh slice with the original argv verbatim at the tail. The
// input slice is never aliased.
func WrapLaunchMarker(argv []string, nonce string) []string {
	out := make([]string, 0, 5+len(argv))
	out = append(out, "sh", "-c", launchMarkerScript, LaunchMarkerArg0, nonce)
	return append(out, argv...)
}

// LaunchMarkerLine is the exact line (without terminator) the wrapper prints.
func LaunchMarkerLine(nonce string) string {
	return launchMarkerPrefix + nonce
}

// CutLaunchMarker finds the first WHOLE line of out equal to
// LaunchMarkerLine(nonce) — starting at index 0 or after '\n', ending at '\n',
// "\r\n" or end of string — and removes exactly that line and its terminator,
// returning (cleaned, true). A match inside a longer line does not count (a
// runtime echoing the argv prints the nonce, but never as the whole marker
// line). An empty nonce, or no whole-line match, returns (out, false).
func CutLaunchMarker(out, nonce string) (string, bool) {
	if nonce == "" {
		return out, false
	}
	line := LaunchMarkerLine(nonce)
	for from := 0; from <= len(out); {
		i := strings.Index(out[from:], line)
		if i < 0 {
			return out, false
		}
		start := from + i
		end := start + len(line)
		atLineStart := start == 0 || out[start-1] == '\n'
		switch {
		case atLineStart && end == len(out):
			return out[:start], true
		case atLineStart && out[end] == '\n':
			return out[:start] + out[end+1:], true
		case atLineStart && strings.HasPrefix(out[end:], "\r\n"):
			return out[:start] + out[end+2:], true
		}
		from = start + 1
	}
	return out, false
}

// IsRuntimeLaunchExit reports whether code is one of the container runtime's
// own exit statuses (125, 126, 127). It is only a necessary condition: the
// ABSENCE of the launch marker decides whether the command started.
func IsRuntimeLaunchExit(code int) bool {
	return code == 125 || code == 126 || code == 127
}
