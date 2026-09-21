package diagnostics

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// fingerprintLength is the number of hex chars retained from the
// SHA-256 digest. 12 hex chars (48 bits) make a collision between two
// distinct (error code, surface, detail class, version family) tuples
// vanishingly unlikely while keeping the embedded marker short.
const fingerprintLength = 12

// Fingerprint is a stable short identifier for a class of failure, used
// to dedup upstream product reports (#1006). It hashes the tuple
// (error code, failing surface, detail class, version family) so two runs
// that hit the same failure in the same version family fingerprint
// identically, while a change in any component yields a different
// fingerprint.
//
// The detailClass component (a closed-enum normalization of the failing
// stage's free-text reason — see ClassifyFailureDetail) is included in the
// hash input ONLY when non-empty after normalization. This is the
// live-fingerprint backward-compatibility contract (#1962): an empty
// detail class reproduces the exact pre-change 3-component digest, so
// every currently-unclassified open report keeps deduping its true
// recurrences, and only newly-classified failure shapes (which previously
// conflated onto a shared surface) re-key and file separately.
//
// Components are normalized (trimmed, lowercased) and joined with a NUL
// separator that cannot appear in any component, so distinct tuples can
// never collide by concatenation ("a","bc" vs "ab","c"). The result is a
// lowercase hex string safe to embed in an issue body marker.
func Fingerprint(errorCode, failingSurface, detailClass, versionFamily string) string {
	parts := []string{errorCode, failingSurface}
	if dc := normalizeComponent(detailClass); dc != "" {
		parts = append(parts, detailClass)
	}
	parts = append(parts, versionFamily)
	return FingerprintOf(parts...)
}

// FingerprintOf is the generic fingerprint primitive: it normalizes each
// component (trim + lowercase, via normalizeComponent), NUL-joins them,
// SHA-256s the join, and returns the first 12 hex chars. The NUL separator
// cannot appear in any component, so distinct component tuples can never
// collide by concatenation ("a","bc" vs "ab","c"). Fingerprint delegates to
// it so the failure digest is byte-identical to the pre-change value; the
// report-shape keyings (ReportFingerprint) call it directly.
func FingerprintOf(components ...string) string {
	parts := make([]string, len(components))
	for i, c := range components {
		parts[i] = normalizeComponent(c)
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])[:fingerprintLength]
}

// normalizeComponent lowercases and trims a fingerprint component so
// trivially-different spellings of the same fact fingerprint alike.
func normalizeComponent(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// DescriptionDigest is a SHA-256 (first 12 hex) of the operator description
// after whitespace-normalization: TrimSpace, ToLower, and collapsing every
// run of Unicode whitespace to a single space (strings.Fields + Join). Two
// descriptions that differ only in case or whitespace digest identically; an
// all-whitespace (or empty) input returns "". It is a DIGEST, never the text:
// the raw description is hashed away, so a healthy-run report can be keyed on
// "the same free text" without carrying that free text into the fingerprint.
// The caller passes the REDACTED description, so no secret-derived material
// reaches the digest input either.
func DescriptionDigest(text string) string {
	fields := strings.Fields(strings.ToLower(text))
	if len(fields) == 0 {
		return ""
	}
	return FingerprintOf(strings.Join(fields, " "))
}

// FingerprintBasis names WHAT a report fingerprint was computed over, so a
// response, audit payload, report body and occurrence comment can show an
// operator the keying instead of asking them to trust `action: occurrence`.
// Kind is one of the closed FingerprintKind* constants; Components lists the
// component NAMES actually hashed, in hash order; DedupSearched reports
// whether the handler searched the upstream repo for an existing report
// (false on the healthy_unique keying, which files fresh).
type FingerprintBasis struct {
	Kind          string   `json:"kind"`
	Components    []string `json:"components"`
	DedupSearched bool     `json:"dedup_searched"`
}

// The closed set of fingerprint keyings (#3233).
//   - FingerprintKindFailure: a failing-stage report, keyed on the failure
//     tuple (byte-identical to the pre-change digest).
//   - FingerprintKindHealthyDescription: a healthy run WITH consented free
//     text, keyed on (run_state, workflow_id, description_digest, version).
//   - FingerprintKindHealthyUnique: a healthy run with NO free text, keyed on
//     (run_state, workflow_id, run_id, version) and filed fresh (no search).
const (
	FingerprintKindFailure            = "failure"
	FingerprintKindHealthyDescription = "healthy_description"
	FingerprintKindHealthyUnique      = "healthy_unique"
)

// ReportFingerprint computes the dedup fingerprint for a product report and
// names what it was keyed on (#3233). It splits the keying by report SHAPE so
// a friction report on a healthy (no-failing-stage) run can no longer collide
// with an unrelated report that merely shares the run shape:
//
//   - When the bundle has a FailingStage, the fingerprint is the UNCHANGED
//     failure tuple (category, surface, optional detail class, version family)
//     with DedupSearched=true — the same failure on the same workflow must
//     still dedup, so the description is deliberately NOT hashed here, and the
//     digest is byte-identical to the pre-change Fingerprint value.
//   - When there is no failing stage but a non-empty redacted description,
//     the fingerprint keys on (run_state, workflow_id, description_digest,
//     version_family) with DedupSearched=true — two friction reports on the
//     same workflow describing the same thing dedup together.
//   - Otherwise (healthy run, no free text) the fingerprint keys on
//     (run_state, workflow_id, run_id, version_family) with
//     DedupSearched=false — the run id makes the marker unique per run, and
//     the handler skips the dedup search and files fresh.
//
// A failure tuple and a healthy tuple cannot collide by concatenation: the
// failure tuple's first component is a single-letter MVP_SPEC §6 category (or
// the run-state fallback only when no stage failed), while a healthy tuple's
// first component is always the run-state word, and every component is
// NUL-separated so no two distinct joins produce the same input.
func ReportFingerprint(b DiagnosticBundle, redactedDescription string) (string, FingerprintBasis) {
	versionFamily := VersionFamily(b.Versions.Fishhawkd.Version)
	if b.FailingStage != nil {
		components := []string{"failure_category", "failure_surface"}
		if normalizeComponent(b.FailingStage.FailureDetailClass) != "" {
			components = append(components, "failure_detail_class")
		}
		components = append(components, "version_family")
		fp := Fingerprint(b.FailingStage.FailureCategory, b.FailingStage.FailureSurface,
			b.FailingStage.FailureDetailClass, versionFamily)
		return fp, FingerprintBasis{Kind: FingerprintKindFailure, Components: components, DedupSearched: true}
	}
	if digest := DescriptionDigest(redactedDescription); digest != "" {
		fp := FingerprintOf(b.RunState, b.WorkflowID, digest, versionFamily)
		return fp, FingerprintBasis{
			Kind:          FingerprintKindHealthyDescription,
			Components:    []string{"run_state", "workflow_id", "description_digest", "version_family"},
			DedupSearched: true,
		}
	}
	fp := FingerprintOf(b.RunState, b.WorkflowID, b.RunID, versionFamily)
	return fp, FingerprintBasis{
		Kind:          FingerprintKindHealthyUnique,
		Components:    []string{"run_state", "workflow_id", "run_id", "version_family"},
		DedupSearched: false,
	}
}

// VersionFamily reduces a build version to its major.minor family — the
// granularity at which "the same defect" is the same for dedup purposes.
// A semver-ish "v0.4.2" becomes "v0.4"; "dev"/"unknown"/single-segment
// values degrade to the literal input (and an empty version to "dev")
// rather than failing, so an unstamped dev build still fingerprints
// deterministically.
func VersionFamily(version string) string {
	v := strings.TrimSpace(version)
	if v == "" {
		return "dev"
	}
	parts := strings.Split(v, ".")
	if len(parts) >= 2 {
		return parts[0] + "." + parts[1]
	}
	return v
}
