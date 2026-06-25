// Package securityscan is the dependency-free leaf contract for the
// CodeQL/SAST findings surface (#1096). It defines the single
// cross-slice domain type — Finding — that every downstream slice
// (2-5) imports, plus the audit-category and gate concern-kind
// constants those slices reference, and the two pure filters that
// reduce a raw finding set to the diff-relevant, high-severity subset
// the implement-review gate consumes.
//
// This package imports NOTHING from the rest of the backend on
// purpose: it is the acyclic leaf so the Finding seam lives entirely
// in one place. githubclient depends on it (decoding the live GitHub
// code-scanning REST shape directly into []Finding); the lifecycle
// wiring that consumes the constants + filters lands in later slices.
// As of slice 1/5 there are no consumers — the package is purely
// additive.
package securityscan

import "strings"

// Finding is the single cross-slice contract type: one code-scanning
// alert reduced to the fields the implement-review gate needs. The
// GitHub code-scanning REST client decodes alerts directly into this
// type (githubclient.ListCodeScanningAlerts), and the filters below
// operate on it, so the whole Finding seam is contained in this slice.
type Finding struct {
	// RuleID is the code-scanning rule identifier (alert `rule.id`),
	// e.g. "go/sql-injection".
	RuleID string
	// Severity is GitHub's security-severity level
	// (`rule.security_severity_level`): one of low|medium|high|critical,
	// or empty when the rule carries no security-severity (non-security
	// rules report null, which decodes to "").
	Severity string
	// Path is the repo-relative file path of the alert's most recent
	// instance (`most_recent_instance.location.path`).
	Path string
	// StartLine is the 1-based start line of the alert's most recent
	// instance (`most_recent_instance.location.start_line`).
	StartLine int
}

// AuditCategorySecurityFindings is the audit-log category slices 2-5
// stamp when recording security findings against a run. It is a
// cross-slice contract token: keep the value stable and exactly as
// named.
const AuditCategorySecurityFindings = "implement_security_findings"

// MissingKind is the implement-review gate concern-kind that slice 3
// raises when a high-severity finding lands on a changed file. It is a
// cross-slice contract token (the gate seam): the string is internal —
// not a wire/schema/API surface — but must stay stable for slice 3 to
// import.
const MissingKind = "security_findings"

// FilterToDiffFiles keeps only the findings whose Path is one of
// changedPaths, preserving the input order. It is nil-safe (nil or
// empty findings yields a non-nil empty slice; nil or empty
// changedPaths drops everything), never mutates the input slice, and
// always returns a fresh slice.
//
// Membership is resolved via a set built once for O(1) lookups, so the
// cost is linear in len(findings)+len(changedPaths).
func FilterToDiffFiles(findings []Finding, changedPaths []string) []Finding {
	changed := make(map[string]struct{}, len(changedPaths))
	for _, p := range changedPaths {
		changed[p] = struct{}{}
	}
	out := make([]Finding, 0, len(findings))
	for _, f := range findings {
		if _, ok := changed[f.Path]; ok {
			out = append(out, f)
		}
	}
	return out
}

// FilterHighSeverity keeps only the findings whose Severity is high or
// critical, comparing case-insensitively against the GitHub enum. It
// preserves the input order, is nil-safe (nil/empty input yields a
// non-nil empty slice), never mutates the input slice, and always
// returns a fresh slice.
func FilterHighSeverity(findings []Finding) []Finding {
	out := make([]Finding, 0, len(findings))
	for _, f := range findings {
		switch strings.ToLower(f.Severity) {
		case "high", "critical":
			out = append(out, f)
		}
	}
	return out
}
