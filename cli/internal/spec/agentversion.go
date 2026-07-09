package spec

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// agent_version compatibility ranges (E32.13 / #1743).
//
// An agent_version value is a semver comparator RANGE: a space-separated
// AND list of comparators, each an operator (>=, >, <=, <, =, ==) followed
// by a 1-to-3-part dotted version, e.g. ">=2.1 <2.2" or "=3.0.1". The
// range is matched against a probed CLI version — a free-form string like
// "2.1.5 (Claude Code)" that #1769 records from `<binary> --version`, from
// which the FIRST semver token is extracted before comparison.
//
// This file is duplicated byte-for-byte in cli/internal/spec/agentversion.go:
// the two Go modules cannot share a package, so the matcher lives in both.
// The backend consumes MatchAgentVersionRange (the reviewer-enforcement
// sibling slice); the runner has its OWN duplicate (runner cannot import
// backend/internal/spec). ValidAgentVersionRange is the syntactic gate both
// validators call.

// semverTokenRe extracts the first 1-to-3-part dotted numeric token from a
// free-form probe string. "2.1.5 (Claude Code)" -> "2.1.5"; "codex 0.30"
// -> "0.30"; "unknown" -> "" (no match, uncomparable).
var semverTokenRe = regexp.MustCompile(`[0-9]+(?:\.[0-9]+){0,2}`)

// comparator is one parsed range term: an operator and its 3-slot version.
type comparator struct {
	op  string
	ver [3]int
}

// validOps is the closed set of comparator operators, longest-first so the
// two-character operators are matched before their single-character
// prefixes (">=" before ">", "==" before "=").
var validOps = []string{">=", "<=", "==", ">", "<", "="}

// ValidAgentVersionRange reports whether r is a syntactically valid
// agent_version range: a non-empty, space-separated AND list of comparators,
// each a known operator followed by a 1-to-3-part dotted numeric version. It
// is the semantic-layer gate for both Executor.AgentVersion and
// AgentReviewer.AgentVersion; a malformed range is a spec authoring error,
// caught here rather than at dispatch. Returns nil on a valid range.
func ValidAgentVersionRange(r string) error {
	_, err := parseAgentVersionRange(r)
	return err
}

// parseAgentVersionRange parses r into its comparator list, returning a
// precise error on any malformed token. Shared by ValidAgentVersionRange
// (syntactic gate) and MatchAgentVersionRange (evaluation).
func parseAgentVersionRange(r string) ([]comparator, error) {
	fields := strings.Fields(r)
	if len(fields) == 0 {
		return nil, fmt.Errorf("agent_version range is empty; expected a comparator list like \">=2.1 <2.2\"")
	}
	comps := make([]comparator, 0, len(fields))
	for _, tok := range fields {
		op := ""
		for _, cand := range validOps {
			if strings.HasPrefix(tok, cand) {
				op = cand
				break
			}
		}
		if op == "" {
			return nil, fmt.Errorf("agent_version comparator %q must start with one of >=, >, <=, <, =, ==", tok)
		}
		verStr := strings.TrimPrefix(tok, op)
		ver, ok := parseVersionParts(verStr)
		if !ok {
			return nil, fmt.Errorf("agent_version comparator %q has a malformed version %q; expected a 1-to-3-part dotted number like 2, 2.1, or 2.1.5", tok, verStr)
		}
		comps = append(comps, comparator{op: op, ver: ver})
	}
	return comps, nil
}

// parseVersionParts parses a 1-to-3-part dotted numeric version into a
// 3-slot array, zero-padding missing components ("2.1" -> {2,1,0}). Returns
// ok=false on an empty string, more than three parts, or a non-numeric part.
func parseVersionParts(v string) ([3]int, bool) {
	var out [3]int
	if v == "" {
		return out, false
	}
	parts := strings.Split(v, ".")
	if len(parts) > 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

// MatchAgentVersionRange evaluates a probed CLI version against an
// agent_version range. It returns:
//
//   - matched: true when the extracted probed version satisfies EVERY
//     comparator in the range (only meaningful when comparable is true).
//   - comparable: false when no semver token can be extracted from probed
//     (e.g. the #1769 "unknown" sentinel, or a version string with no
//     digits) — callers degrade to PASS/proceed, mirroring the runner's
//     semverLT("dev") degrade. true otherwise.
//   - err: non-nil only when r itself is a malformed range (a defensive
//     re-check; callers validate at spec-parse time via
//     ValidAgentVersionRange, so this should not fire in practice).
//
// A comparable=false result reports matched=false, but callers keyed on
// comparable (not matched) proceed rather than block.
func MatchAgentVersionRange(r, probed string) (matched, comparable bool, err error) {
	comps, perr := parseAgentVersionRange(r)
	if perr != nil {
		return false, false, perr
	}
	token := semverTokenRe.FindString(probed)
	if token == "" {
		return false, false, nil
	}
	ver, ok := parseVersionParts(token)
	if !ok {
		// The regex guarantees a 1-to-3-part dotted number, so this is
		// unreachable in practice; treat it as uncomparable defensively.
		return false, false, nil
	}
	for _, c := range comps {
		if !satisfies(ver, c) {
			return false, true, nil
		}
	}
	return true, true, nil
}

// satisfies reports whether the 3-slot version ver satisfies comparator c.
func satisfies(ver [3]int, c comparator) bool {
	cmp := compareVersions(ver, c.ver)
	switch c.op {
	case ">":
		return cmp > 0
	case ">=":
		return cmp >= 0
	case "<":
		return cmp < 0
	case "<=":
		return cmp <= 0
	case "=", "==":
		return cmp == 0
	default:
		return false
	}
}

// compareVersions returns -1, 0, or 1 as a is less than, equal to, or
// greater than b, comparing the three components in order.
func compareVersions(a, b [3]int) int {
	for i := 0; i < 3; i++ {
		if a[i] < b[i] {
			return -1
		}
		if a[i] > b[i] {
			return 1
		}
	}
	return 0
}
