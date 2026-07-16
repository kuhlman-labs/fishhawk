// Package timescale is a test-support helper that derives a timing multiplier
// for wall-clock boundary-timeout tests (the #1805 pipe-leak group-kill family,
// #1984). Boundary-timeout tests pit a context deadline against a subprocess
// spawn/reap race; on a loaded CI runner the scheduling latency can slip past a
// tight raw millisecond bound and red-line an otherwise-correct test. Threading
// every deadline-competing duration through D(base) scales all of them by the
// same factor, so every discrimination ratio (bound/deadline, wedge/bound,
// long-grace/bound) is preserved by construction while the family gains headroom
// on CI-class hardware.
//
// Factor precedence:
//
//	FISHHAWK_TEST_TIME_SCALE (explicit positive integer) — wins over everything.
//	CI env var non-empty (GitHub Actions sets CI unconditionally)   — default 5.
//	neither                                                          — default 1.
//
// A set-but-invalid FISHHAWK_TEST_TIME_SCALE (non-integer, zero, negative, or
// above maxFactor) panics with a precise message rather than silently degrading
// to 1 — a misleading raw factor must fail loud, never produce a short or
// negative scaled duration. maxFactor caps the accepted value so D can never
// overflow time.Duration: with base durations bounded by the ~30s liveness
// waits, base*maxFactor stays far inside int64 nanoseconds.
//
// Helper re-exec processes compute the SAME factor as the driving test because
// all three test-helper builders (procgroup, claudecode, codex) append their
// helper env to os.Environ(), so FISHHAWK_TEST_TIME_SCALE and CI propagate to
// the fake-CLI and grandchild processes. That inheritance is the invariant that
// keeps a scaled wedge sleep above the scaled elapsed bound: if the helper
// computed a different factor its wedge sleep could fall below the driver's
// bound and spuriously fail the test.
package timescale

import (
	"os"
	"strconv"
	"time"
)

// maxFactor is the largest accepted FISHHAWK_TEST_TIME_SCALE. It caps D's output
// well inside time.Duration's int64 range (30s * 1000 ≈ 3e13 ns ≪ 9.2e18) so a
// huge override can never produce a wrapped negative or misleadingly short
// duration; a larger value panics, like every other invalid input.
const maxFactor = 1000

// scaleEnv is the explicit override variable; ciEnv is the CI presence signal.
const (
	scaleEnv = "FISHHAWK_TEST_TIME_SCALE"
	ciEnv    = "CI"
)

// Factor returns the timing multiplier per the precedence documented on the
// package. An explicit FISHHAWK_TEST_TIME_SCALE wins; otherwise a non-empty CI
// env var yields the CI default 5; otherwise 1. A set-but-invalid override
// panics.
func Factor() int {
	raw, ok := os.LookupEnv(scaleEnv)
	if ok && raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			panic(scaleEnv + "=" + strconv.Quote(raw) + ": must be a positive integer (1.." + strconv.Itoa(maxFactor) + ")")
		}
		if n <= 0 || n > maxFactor {
			panic(scaleEnv + "=" + strconv.Quote(raw) + ": out of range, must be a positive integer 1.." + strconv.Itoa(maxFactor))
		}
		return n
	}
	if os.Getenv(ciEnv) != "" {
		return 5
	}
	return 1
}

// D scales base by Factor(). Every deadline-competing duration in a
// boundary-timeout test derives from D so uniform scaling preserves the test's
// discrimination ratios at any factor.
func D(base time.Duration) time.Duration {
	return base * time.Duration(Factor())
}
