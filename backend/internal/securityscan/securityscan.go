// Package securityscan owns the cross-slice contract for surfacing
// GitHub code-scanning (CodeQL/SAST) findings to the implement-review
// gate (#1096). It is the single home of the tokens and pure helpers
// that the webhook ingest, the merge gate, and the run-status surfaces
// all share, so a value drift is caught here once rather than at three
// call sites:
//
//   - Finding — the normalized shape of a single code-scanning alert,
//     decoded by githubclient and rendered by the surfaces.
//   - AuditCategorySecurityFindings — the audit-log category the webhook
//     records and the gate reads.
//   - GateMissingKind — the auditcomplete MissingKind string the merge
//     gate holds the merge on (a high-severity finding on the implement
//     diff is unresolved).
//   - FilterHighSeverity / FilterToDiffFiles — the two pure filters that
//     reduce a raw alert list to the high-severity findings intersecting
//     the implement diff.
//
// This package has no internal dependencies on purpose: it is the leaf
// contract that wave-0 ships and waves 1-2 import unchanged.
package securityscan

import "strings"

// AuditCategorySecurityFindings is the audit-log Category the webhook
// ingest records one idempotent entry under (#1096) and the merge gate
// reads. Defined ONCE here; importers must not redeclare the literal.
const AuditCategorySecurityFindings = "implement_security_findings"

// GateMissingKind is the auditcomplete MissingKind string value the merge
// gate uses when a high-severity code-scanning finding on the implement
// diff is unresolved. auditcomplete owns the MissingKind type and binds
// its MissingSecurityFindings constant to this string, so the value lives
// here once (the contract) and auditcomplete imports it unchanged rather
// than the literal being duplicated across packages.
const GateMissingKind = "security_findings_unresolved"

// Finding is the normalized shape of a single GitHub code-scanning alert
// Fishhawk consumes. It is decoded from the REST API by githubclient,
// reduced by the filters below, recorded in the securityscan audit entry,
// and rendered on the implement-review prompt and the run-status/REST
// surfaces. Fields are the subset the gate and surfaces need; the raw
// alert carries far more.
type Finding struct {
	// Number is the alert's per-repo identifier. Stable across rescans,
	// so the webhook keys idempotency/dedup on it.
	Number int `json:"number"`
	// RuleID is the analysis rule that fired (e.g. "js/sql-injection").
	RuleID string `json:"rule_id"`
	// Description is the human-facing rule description (or name).
	Description string `json:"description"`
	// Severity is the normalized security-severity level, lowercased:
	// "critical" | "high" | "medium" | "low" | "" (none). This is GitHub's
	// rule.security_severity_level, NOT the coarser rule.severity
	// (none/note/warning/error) — the security level is what the gate
	// thresholds on.
	Severity string `json:"severity"`
	// State is the alert state: "open" | "fixed" | "dismissed".
	State string `json:"state"`
	// Path is the repo-relative file the finding's most-recent instance
	// points at. FilterToDiffFiles intersects on this.
	Path string `json:"path"`
	// StartLine is the 1-based line of the most-recent instance, for
	// surfacing "path:line". Zero when GitHub omits a location.
	StartLine int `json:"start_line"`
	// CommitSHA is the commit the most-recent instance was observed on,
	// which the webhook matches against a run's recorded head SHA.
	CommitSHA string `json:"commit_sha"`
	// Ref is the git ref of the most-recent instance (e.g.
	// "refs/pull/12/merge" or "refs/heads/...").
	Ref string `json:"ref"`
	// Tool is the analysis tool name (e.g. "CodeQL").
	Tool string `json:"tool"`
	// HTMLURL links to the alert on GitHub, surfaced so a reviewer can
	// open it directly.
	HTMLURL string `json:"html_url"`
}

// Severity constants are the normalized values Finding.Severity carries.
// GitHub emits these lowercase in rule.security_severity_level.
const (
	SeverityCritical = "critical"
	SeverityHigh     = "high"
	SeverityMedium   = "medium"
	SeverityLow      = "low"
)

// IsHighSeverity reports whether s is a gating severity. "critical" gates
// alongside "high": a critical security finding is strictly more severe
// than a high one, so the high-severity gate must not let it through.
// Comparison is case-insensitive and tolerant of surrounding whitespace
// so a decode quirk can't silently drop a real finding.
func IsHighSeverity(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case SeverityHigh, SeverityCritical:
		return true
	default:
		return false
	}
}

// FilterHighSeverity returns the findings whose Severity gates (high or
// critical), dropping medium/low/none. It is pure: it allocates a new
// slice and never mutates the input. A nil input yields a nil slice.
func FilterHighSeverity(findings []Finding) []Finding {
	var out []Finding
	for _, f := range findings {
		if IsHighSeverity(f.Severity) {
			out = append(out, f)
		}
	}
	return out
}

// FilterToDiffFiles returns the findings whose Path is one of diffFiles —
// the files the implement stage actually changed. This is how a finding
// is attributed to THIS run's diff rather than pre-existing repo debt: an
// alert on an untouched file is real but not introduced here, so it does
// not gate. It is pure (no input mutation); a finding with an empty Path,
// or an empty/nil diffFiles set, intersects nothing and is dropped.
func FilterToDiffFiles(findings []Finding, diffFiles []string) []Finding {
	if len(findings) == 0 || len(diffFiles) == 0 {
		return nil
	}
	inDiff := make(map[string]struct{}, len(diffFiles))
	for _, p := range diffFiles {
		inDiff[p] = struct{}{}
	}
	var out []Finding
	for _, f := range findings {
		if f.Path == "" {
			continue
		}
		if _, ok := inDiff[f.Path]; ok {
			out = append(out, f)
		}
	}
	return out
}
