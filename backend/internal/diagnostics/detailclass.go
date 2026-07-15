package diagnostics

import "strings"

// detailClass entries pair a closed-enum failure-detail class with the
// lowercase substrings that identify it in a git stderr line. The table
// is ORDERED and matched top-to-bottom: the first class with any matching
// marker wins.
//
// Ordering is load-bearing. Git prefixes BOTH auth and network failures
// with "fatal: unable to access '<url>':", so "unable to access" is
// deliberately NOT a marker, and auth-401 is checked before
// target-unreachable — a line carrying both an access-failure prefix and
// a "401" suffix classifies as auth-401, not as a network fault.
var detailClassTable = []struct {
	class   string
	markers []string
}{
	{
		class: "auth-401",
		markers: []string{
			"the requested url returned error: 401",
			"authentication failed",
			"could not read username",
			"terminal prompts disabled",
			"http 401",
		},
	},
	{
		class: "bad-object-ref",
		markers: []string{
			"bad object",
			"couldn't find remote ref",
			"unknown revision or path",
			"not a valid object name",
			"reference is not a tree",
			"did not match any",
		},
	},
	{
		class: "target-unreachable",
		markers: []string{
			"could not resolve host",
			"connection refused",
			"connection timed out",
			"operation timed out",
			"no route to host",
		},
	},
}

// ClassifyFailureDetail reduces a failing stage's free-text FailureReason
// (which carries the wrapped git stderr) to a closed enum of failure-detail
// classes: "auth-401", "bad-object-ref", "target-unreachable", or ""
// (unclassified). It lowercases the input and matches it against the
// ordered detailClassTable, returning the first matching class.
//
// The function NEVER returns any part of its input — only a table-owned
// enum literal — so its output is a redaction-safe product fact by
// construction. Empty or unrecognized input returns "" (fail-open), so an
// unclassifiable failure keeps the pre-classification fingerprint.
func ClassifyFailureDetail(reason string) string {
	lower := strings.ToLower(reason)
	for _, dc := range detailClassTable {
		for _, m := range dc.markers {
			if strings.Contains(lower, m) {
				return dc.class
			}
		}
	}
	return ""
}
