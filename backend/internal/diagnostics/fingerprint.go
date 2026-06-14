package diagnostics

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// fingerprintLength is the number of hex chars retained from the
// SHA-256 digest. 12 hex chars (48 bits) make a collision between two
// distinct (error code, surface, version family) tuples vanishingly
// unlikely while keeping the embedded marker short.
const fingerprintLength = 12

// Fingerprint is a stable short identifier for a class of failure, used
// to dedup upstream product reports (#1006). It hashes the tuple
// (error code, failing surface, version family) so two runs that hit the
// same failure in the same version family fingerprint identically, while
// a change in any component yields a different fingerprint.
//
// Components are normalized (trimmed, lowercased) and joined with a NUL
// separator that cannot appear in any component, so distinct tuples can
// never collide by concatenation ("a","bc" vs "ab","c"). The result is a
// lowercase hex string safe to embed in an issue body marker.
func Fingerprint(errorCode, failingSurface, versionFamily string) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		normalizeComponent(errorCode),
		normalizeComponent(failingSurface),
		normalizeComponent(versionFamily),
	}, "\x00")))
	return hex.EncodeToString(sum[:])[:fingerprintLength]
}

// normalizeComponent lowercases and trims a fingerprint component so
// trivially-different spellings of the same fact fingerprint alike.
func normalizeComponent(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
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
