package main

import "regexp"

// goBuildCacheEntryVanishedRe matches a missing Go build-cache ENTRY (#3901):
// cmd/go's DiskCache.fileName shape — a two-hex subdirectory, then the 64-hex
// rendering of a 32-byte SHA-256 action/output ID, then the `-a` (action) or
// `-d` (output) suffix — immediately followed by the ENOENT rendering.
//
// The `/` after the subdirectory and the `-[ad]` suffix bound the hash on both
// sides, so a 63- or 65-hex name, uppercase hex, or a name without the suffix
// never matches. No `go-build` prefix is required, so the container gate's
// /gatecache/gocache path matches exactly like the host's
// ~/Library/Caches/go-build.
//
// backend/internal/delegation's hasInfraFlakeSignature carries the SAME
// pattern (the two live in different Go modules and cannot share it); each
// side pins it against the same verbatim corpus string.
var goBuildCacheEntryVanishedRe = regexp.MustCompile(`[0-9a-f]{2}/[0-9a-f]{64}-[ad]: no such file or directory`)

// isGoBuildCacheEntryVanished reports whether a failed verify's output names a
// Go build-cache entry that disappeared under the build (#3901). The class is
// DIFF-INDEPENDENT: the cache is host state shared by every worktree and run,
// and an entry vanishes when something OUTSIDE the tree under test deletes it
// mid-build — the observed instance is a `go clean -cache` that passed its
// "nothing is live" check one second before run 26ede2f1's implement stage
// dispatched, whose verify then failed with
//
//	could not load export data: open …/go-build/e5/e5470b5b…-d: no such file or directory (typecheck)
//
// and was recorded as category A (an agent failure). Matched here, the
// committed-tree gates re-run the verify once in place and a persistent
// signature classifies category C (infrastructure).
//
// The input is UNTRUSTED verify output, exactly as for every other
// isVerifyInfraFailure class: a test the agent authored can print this
// rendering and steer its own genuine failure to category C. The bound is the
// one isVerifyInfraFailure documents — retry churn and delayed parking, never
// a verified-tree bypass, because the push still requires an explicit
// "passed" verify outcome.
func isGoBuildCacheEntryVanished(output string) bool {
	return goBuildCacheEntryVanishedRe.MatchString(output)
}
