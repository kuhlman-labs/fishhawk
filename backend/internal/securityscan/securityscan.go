// Package securityscan is the cross-slice contract for surfacing GitHub
// code-scanning (CodeQL/SAST) high-severity findings to the implement-review
// gate (#1096). It owns the small set of tokens and pure helpers every
// consuming slice shares, so they are defined ONCE here and imported
// unchanged downstream:
//
//   - Finding — the typed slice of a code-scanning alert Fishhawk surfaces.
//   - AuditCategorySecurityFindings — the dedicated audit category the
//     webhook handler records findings under (their own signal, distinct
//     from review-verdict design concerns).
//   - SecurityFindingsUnresolved — the auditcomplete gate MissingKind that
//     holds the merge gate while unresolved high-severity findings intersect
//     the implement diff.
//   - FilterToDiffFiles / FilterHighSeverity — the two pure filters the
//     orchestration slice composes to reduce raw alerts to the gating set.
//
// This package has no lifecycle wiring and no dependencies beyond the
// standard library: it is a leaf the webhook (read + record), merge gate
// (read + hold), and surface (render) slices all import without creating a
// cycle.
package securityscan

import "strings"

// AuditCategorySecurityFindings is the audit-log category under which the
// webhook handler records the recorded high-severity code-scanning findings
// for a run's implement diff. It is a SEPARATE signal from review-verdict
// concern entries (#1096): a finding here must not consume a design-concern
// fixup pass. Defined once; imported unchanged by the webhook and surface
// slices.
const AuditCategorySecurityFindings = "implement_security_findings"

// SecurityFindingsUnresolved is the auditcomplete gate MissingKind that holds
// the fishhawk_audit_complete merge gate while unresolved high-severity
// code-scanning findings intersect the implement diff. Defined here as an
// untyped string constant so the merge-gate slice can convert it to its
// MissingKind type without re-declaring the token. Defined once; imported
// unchanged downstream.
const SecurityFindingsUnresolved = "security_findings_unresolved"

// Finding is the typed slice of a GitHub code-scanning alert Fishhawk
// surfaces. It carries enough to gate on, render in the implement-review
// prompt, and expose on the MCP/REST run-status surfaces, without exposing
// the full GitHub alert shape. Fields are JSON-tagged because a Finding is
// serialized into the recorded audit entry and the run-status response.
type Finding struct {
	// Number is the GitHub code-scanning alert number (stable per repo),
	// the dedup/idempotency key for a recorded finding.
	Number int `json:"number"`
	// RuleID is the analysis rule identifier (e.g. "go/allocation-size-overflow").
	RuleID string `json:"rule_id"`
	// Severity is the alert's security severity level
	// ("critical"|"high"|"medium"|"low"|"none"). FilterHighSeverity keeps
	// only "high" and "critical".
	Severity string `json:"severity"`
	// Description is the rule's short human description, surfaced verbatim
	// in the review prompt and run-status.
	Description string `json:"description"`
	// Path is the repo-relative file the alert's most-recent instance points
	// at. FilterToDiffFiles intersects this against the implement diff's
	// changed-file list.
	Path string `json:"path"`
	// Line is the 1-based start line of the alert's most-recent instance,
	// or 0 when GitHub reported no location.
	Line int `json:"line"`
	// State is the alert state ("open"|"dismissed"|"fixed"). The read only
	// surfaces unresolved ("open") alerts, so this is "open" in practice; it
	// is retained for the recorded audit entry's auditability.
	State string `json:"state"`
	// URL is the alert's GitHub html_url, surfaced so an operator can open
	// the finding directly from run-status.
	URL string `json:"url"`
}

// FilterToDiffFiles returns the findings whose Path is one of changedFiles —
// the alerts that intersect the implement diff. A code-scanning analysis
// covers the whole repository, so an alert in an untouched file is not this
// run's concern and must not hold the gate; intersecting against the diff
// isolates the findings the run is responsible for.
//
// Matching is exact on the repo-relative path. Returns a nil slice when no
// finding intersects (len 0), never the input slice, so callers never alias
// the source.
func FilterToDiffFiles(findings []Finding, changedFiles []string) []Finding {
	if len(findings) == 0 || len(changedFiles) == 0 {
		return nil
	}
	inDiff := make(map[string]struct{}, len(changedFiles))
	for _, f := range changedFiles {
		inDiff[f] = struct{}{}
	}
	var out []Finding
	for _, fnd := range findings {
		if _, ok := inDiff[fnd.Path]; ok {
			out = append(out, fnd)
		}
	}
	return out
}

// FilterHighSeverity returns only the findings whose security severity is
// "high" or "critical" — the severities that hold the merge gate. Lower
// severities ("medium"/"low"/"none") are advisory and never block, so they
// are dropped here rather than recorded and then ignored downstream.
//
// Matching is case-insensitive on Severity. Returns a nil slice when none
// qualify (len 0).
func FilterHighSeverity(findings []Finding) []Finding {
	var out []Finding
	for _, f := range findings {
		switch strings.ToLower(strings.TrimSpace(f.Severity)) {
		case "high", "critical":
			out = append(out, f)
		}
	}
	return out
}
