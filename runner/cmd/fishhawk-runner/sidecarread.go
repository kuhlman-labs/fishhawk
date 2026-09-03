package main

import (
	"errors"
	"fmt"
	"io"
	"os"
)

// maxSidecarBytes is the ceiling on the read of ONE agent-authored sidecar
// (E64.12 / #3106), applied before the bytes are decoded. Every sidecar in the
// family — the PR-description handoff, the acceptance verdict, the fix-up /
// implement commit messages, the scope-justification and counterfactual
// self-reports — is written by an untrusted coding agent, so its size is
// agent-controlled input. 1 MiB is chosen with more than an order of magnitude
// of headroom over the largest LEGITIMATE member: a PR body (GitHub itself caps
// a PR body at 65536 characters) or an acceptance verdict carrying per-criterion
// evidence hashes, both well under 100 KiB. The risk of setting it too LOW is a
// silently dropped honest sidecar, which the per-loader `*_oversize` events make
// diagnosable rather than invisible; the risk of no ceiling is an agent
// materializing an arbitrarily large file into runner memory before any cap or
// validation applies.
//
// It is a plain const, NEVER a test-injectable var, deliberately: injecting a
// tiny ceiling would let the tests pass without ever exercising the SHIPPED
// value, the done-means-test failure mode this repo has been bitten by before.
// TestSidecarCeilingValue pins the shipped number, since a constant's value is
// not structurally enforced by compilation.
const maxSidecarBytes int64 = 1 << 20 // 1 MiB

// errSidecarTooLarge is the distinct sentinel readSidecarBounded returns when a
// sidecar exceeds maxSidecarBytes. Each caller branches on it with errors.Is and
// emits its own named `*_oversize` diagnostic event, so an operator can tell an
// oversize sidecar from a malformed one.
var errSidecarTooLarge = errors.New("agent-authored sidecar exceeds size ceiling")

// readSidecarBounded reads an agent-authored sidecar with a hard ceiling
// (E64.12 / #3106), replacing a bare os.ReadFile at every loader that reads a
// file the coding agent wrote. It preserves os.ReadFile's observable error
// identity so the callers' error ladders keep working unchanged:
//
//   - The os.Open error is returned UNWRAPPED. Every caller branches on
//     os.ErrNotExist (errors.Is) or os.IsNotExist and MUST keep seeing the same
//     error identity os.ReadFile produced — wrapping it would silently reroute
//     the common absent-sidecar no-op into a fail-closed or fail-loud branch.
//   - A read error is likewise returned UNWRAPPED: a directory at the path still
//     yields EISDIR from the read, exactly as os.ReadFile did, so every
//     present-but-unreadable branch is unchanged.
//   - On exceeding the ceiling it returns (nil, errSidecarTooLarge-wrapped) —
//     NIL bytes, never the truncated prefix, so no caller can partially decode.
//
// Reading maxSidecarBytes+1 through an io.LimitReader and comparing the result
// against maxSidecarBytes is what distinguishes an exactly-at-ceiling file
// (accepted) from a one-byte-over file (rejected); the LimitReader also bounds
// the allocation to ~maxSidecarBytes+1 regardless of the file's real size, so
// the ceiling is enforced before the allocation rather than after.
func readSidecarBounded(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	data, err := io.ReadAll(io.LimitReader(f, maxSidecarBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxSidecarBytes {
		return nil, fmt.Errorf("%w: %s is over %d bytes", errSidecarTooLarge, path, maxSidecarBytes)
	}
	return data, nil
}
