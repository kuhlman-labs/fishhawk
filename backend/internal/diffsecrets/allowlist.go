package diffsecrets

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
	"go.yaml.in/yaml/v3"

	"github.com/kuhlman-labs/fishhawk/redaction"
)

// The reviewed allowlist (ADR-084 A2, #5230). A version-1 YAML file at
// AllowlistPath declares two rule lists of the same shape:
//
//	version: 1
//	entries:            # suppress every hit of these classes on these paths
//	  - id: docs-example-pat
//	    paths: ["docs/examples/*.md"]
//	    patterns: ["github-pat-classic"]
//	    reason: "documented placeholder tokens"
//	markers:            # honour an in-line MarkerToken only on these paths
//	  - id: fixture-tests
//	    paths: ["backend/**/testdata/**"]
//	    patterns: ["github-pat-classic"]
//	    reason: "synthetic keys in test fixtures"
//
// The server reads the file only at the run's admission commit and fails
// closed on every failure; the file is itself the diff-secrets-allowlist
// permission-drift surface. Nothing derived from the file is ever rendered
// verbatim: a rule is attributed by SafeRuleID(id) plus RuleFingerprint, and a
// rule whose id, glob or reason matches the pattern set is refused.

// AllowlistPath is the repository-relative path of the allowlist file.
const AllowlistPath = ".fishhawk/diff-secrets-allowlist.yaml"

// The two rule lists, as named in the file and in every fingerprint.
const (
	ListEntries = "entries"
	ListMarkers = "markers"
)

// The Suppression.By values.
const (
	ByEntry  = "entry"
	ByMarker = "marker"
)

// The per-rule rejection reasons.
const (
	RejectBadID          = "bad_id"
	RejectDuplicateID    = "duplicate_id"
	RejectEmptyPaths     = "empty_paths"
	RejectBadPath        = "bad_path"
	RejectEmptyPatterns  = "empty_patterns"
	RejectUnknownPattern = "unknown_pattern"
	RejectEmptyReason    = "empty_reason"
	// RejectRedactionHit: the id, a glob or the reason matches the pattern
	// set. The rule is refused so its credential-shaped text can never be
	// fingerprinted, attributed or rendered.
	RejectRedactionHit = "redaction_hit"
)

// The AllowlistStatusRecord.Status values. Every status but StatusLoaded means
// the server suppressed nothing.
const (
	StatusLoaded      = "loaded"
	StatusAbsent      = "absent"
	StatusUnavailable = "unavailable"
	StatusFetchFailed = "fetch_failed"
	StatusParseError  = "parse_error"
)

// MaxRejectedRecords caps AllowlistStatusRecord.Rejected; RejectedCount
// carries the full count.
const MaxRejectedRecords = 50

// InvalidRuleID is what SafeRuleID renders for an id it will not echo.
const InvalidRuleID = "(invalid id)"

// ErrAllowlistInvalid wraps every whole-file parse failure. The wrapped text
// is a fixed structural class, never file content.
var ErrAllowlistInvalid = errors.New("diff-secrets allowlist: invalid file")

// ruleIDPattern is a well-formed rule id.
var ruleIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// fingerprintPattern is a well-formed fingerprint (RuleFingerprint, Hit.Fingerprint).
var fingerprintPattern = regexp.MustCompile(`^[0-9a-f]{16}$`)

// Allowlist is a parsed allowlist: only ACCEPTED rules, in file order.
type Allowlist struct {
	Entries []Rule
	Markers []Rule
}

// Rule is one accepted allowlist rule.
type Rule struct {
	ID       string
	Paths    []string
	Patterns []string
	Reason   string
}

// Rejection names one rule that was not accepted. RuleID is set only when the
// id is a well-formed slug with no redaction hit and the reason is neither
// bad_id nor redaction_hit, so a malformed or credential-bearing id is never
// echoed.
type Rejection struct {
	List   string
	Index  int
	Reason string
	RuleID string
}

type allowlistFile struct {
	Version *int            `yaml:"version"`
	Entries []allowlistRule `yaml:"entries"`
	Markers []allowlistRule `yaml:"markers"`
}

type allowlistRule struct {
	ID       string   `yaml:"id"`
	Paths    []string `yaml:"paths"`
	Patterns []string `yaml:"patterns"`
	Reason   string   `yaml:"reason"`
}

// ParseAllowlist parses an allowlist file (version 1). Decoding is STRICT:
// an unknown key anywhere, a missing or unsupported version, or YAML that
// does not parse is an ErrAllowlistInvalid error for the whole file, and the
// error text never quotes content. Empty or comment-only content is an empty
// allowlist. A rule is REJECTED — dropped, named in rejected, never fatal —
// for a redaction hit in its id, a glob or its reason (checked first, with
// the full patterns set), a malformed or duplicate id (ids are unique across
// both lists), empty paths, an invalid glob, empty patterns, a pattern name
// not in patterns, or an empty (whitespace-only) reason.
func ParseAllowlist(content []byte, patterns []redaction.Pattern) (*Allowlist, []Rejection, error) {
	al := &Allowlist{}
	if len(bytes.TrimSpace(content)) == 0 {
		return al, nil, nil
	}
	dec := yaml.NewDecoder(bytes.NewReader(content))
	dec.KnownFields(true)
	var f allowlistFile
	if err := dec.Decode(&f); err != nil {
		if errors.Is(err, io.EOF) { // a comment-only document
			return al, nil, nil
		}
		// The YAML error text can quote the offending value; never wrap it.
		return nil, nil, fmt.Errorf("%w: does not decode as a version-1 allowlist", ErrAllowlistInvalid)
	}
	if f.Version == nil {
		return nil, nil, fmt.Errorf("%w: version is required (want 1)", ErrAllowlistInvalid)
	}
	if *f.Version != 1 {
		return nil, nil, fmt.Errorf("%w: unsupported version (want 1)", ErrAllowlistInvalid)
	}
	known := map[string]bool{}
	for _, p := range patterns {
		known[p.Name] = true
	}
	seen := map[string]bool{}
	var rejected []Rejection
	parse := func(list string, in []allowlistRule) []Rule {
		var out []Rule
		for i, r := range in {
			if reason := ruleProblem(r, patterns, known, seen); reason != "" {
				rej := Rejection{List: list, Index: i, Reason: reason}
				if reason != RejectBadID && reason != RejectRedactionHit {
					rej.RuleID = r.ID
				}
				rejected = append(rejected, rej)
				continue
			}
			out = append(out, Rule{
				ID: r.ID, Paths: append([]string(nil), r.Paths...),
				Patterns: append([]string(nil), r.Patterns...), Reason: r.Reason,
			})
		}
		return out
	}
	al.Entries = parse(ListEntries, f.Entries)
	al.Markers = parse(ListMarkers, f.Markers)
	return al, rejected, nil
}

// ruleProblem returns the rejection reason for r, or "" when it is accepted.
// An accepted or duplicate-checked id is recorded in seen.
func ruleProblem(r allowlistRule, patterns []redaction.Pattern, known, seen map[string]bool) string {
	if redactionHit(r.ID, patterns) || redactionHit(r.Reason, patterns) {
		return RejectRedactionHit
	}
	for _, p := range r.Paths {
		if redactionHit(p, patterns) {
			return RejectRedactionHit
		}
	}
	if !ruleIDPattern.MatchString(r.ID) {
		return RejectBadID
	}
	if seen[r.ID] {
		return RejectDuplicateID
	}
	seen[r.ID] = true
	if len(r.Paths) == 0 {
		return RejectEmptyPaths
	}
	for _, p := range r.Paths {
		if p == "" || !doublestar.ValidatePattern(p) {
			return RejectBadPath
		}
	}
	if len(r.Patterns) == 0 {
		return RejectEmptyPatterns
	}
	for _, p := range r.Patterns {
		if !known[p] {
			return RejectUnknownPattern
		}
	}
	if strings.TrimSpace(r.Reason) == "" {
		return RejectEmptyReason
	}
	return ""
}

// redactionHit reports whether any of patterns matches s.
func redactionHit(s string, patterns []redaction.Pattern) bool {
	_, hits := redaction.Redact([]byte(s), patterns)
	return len(hits) > 0
}

// RuleFingerprint is the one identity every surface attributes a rule by:
// the first 16 hex of SHA-256 over list, glob and pattern, NUL-separated. The
// glob itself is never rendered.
func RuleFingerprint(list, glob, pattern string) string {
	sum := sha256.Sum256([]byte(list + "\x00" + glob + "\x00" + pattern))
	return hex.EncodeToString(sum[:])[:fingerprintHexLen]
}

// SafeRuleID is the ONLY renderer of an allowlist-derived string: the id when
// it is a well-formed slug with no redaction.DefaultPatterns hit, otherwise
// InvalidRuleID. Readers apply it again to every PERSISTED id (gate view, PR
// comment, notes), as defence in depth against a corrupted row.
func SafeRuleID(id string) string {
	if !ruleIDPattern.MatchString(id) || redactionHit(id, redaction.DefaultPatterns) {
		return InvalidRuleID
	}
	return id
}

// InvalidFingerprint is what SafeFingerprint renders for a value it will not
// echo.
const InvalidFingerprint = "(invalid fingerprint)"

// SafeFingerprint renders a PERSISTED fingerprint only when it is 16 lowercase
// hex, otherwise InvalidFingerprint: the reader-side twin of SafeRuleID.
func SafeFingerprint(fp string) string {
	if !fingerprintPattern.MatchString(fp) {
		return InvalidFingerprint
	}
	return fp
}

// Suppression is one hit the allowlist suppressed, with the rule and the
// exact (list, glob, pattern) fingerprint that matched it.
type Suppression struct {
	Hit             Hit
	By              string
	RuleID          string
	RuleFingerprint string
}

// Partition splits hits into those still raised (kept) and those the
// allowlist suppresses. A nil or empty allowlist keeps everything. A hit is
// NEVER suppressed when its path is UnresolvedPath, was redacted
// (PathRedacted), cannot be trusted (PathAmbiguous: a spoofable section
// header), or is AllowlistPath itself — a credential added to the allowlist
// file always raises. Otherwise entries are checked first, in file
// order (By=entry), and an entry suppresses a hit of any class; then, only for
// a Marked hit of a marker-suppressible class (markerSuppressible), marker
// scopes (By=marker). A marked hit of a low-entropy class not covered by an
// entry is KEPT, so it raises a high concern (#5462). A rule matches when one
// of its globs matches the path AND its pattern list names the hit's pattern.
func (a *Allowlist) Partition(hits []Hit) (kept []Hit, suppressed []Suppression) {
	if a == nil || (len(a.Entries) == 0 && len(a.Markers) == 0) {
		return hits, nil
	}
	for _, h := range hits {
		if h.Path == UnresolvedPath || h.PathRedacted || h.PathAmbiguous || h.Path == AllowlistPath {
			kept = append(kept, h)
			continue
		}
		if s, ok := matchRules(ListEntries, ByEntry, a.Entries, h); ok {
			suppressed = append(suppressed, s)
			continue
		}
		if h.Marked && markerSuppressible(h.Pattern) {
			if s, ok := matchRules(ListMarkers, ByMarker, a.Markers, h); ok {
				suppressed = append(suppressed, s)
				continue
			}
		}
		kept = append(kept, h)
	}
	return kept, suppressed
}

// matchRules returns the first rule (in file order) matching h.
func matchRules(list, by string, rules []Rule, h Hit) (Suppression, bool) {
	for _, r := range rules {
		if !containsString(r.Patterns, h.Pattern) {
			continue
		}
		for _, g := range r.Paths {
			if ok, err := doublestar.Match(g, h.Path); err == nil && ok {
				return Suppression{Hit: h, By: by, RuleID: r.ID, RuleFingerprint: RuleFingerprint(list, g, h.Pattern)}, true
			}
		}
	}
	return Suppression{}, false
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// SuppressionRecord is one folded suppression on the diff_secrets_detected
// audit payload: every hit with the same (path, pattern, by, rule, fingerprint)
// and, for a marker record, the same per-hit fingerprint. CheckKey is set only
// on a marker record (the marker concern's MarkerCheckKey).
type SuppressionRecord struct {
	Path            string `json:"path"`
	Lines           []int  `json:"lines"`
	Pattern         string `json:"pattern"`
	By              string `json:"by"`
	RuleID          string `json:"rule_id"`
	RuleFingerprint string `json:"rule_fingerprint"`
	CheckKey        string `json:"check_key,omitempty"`
}

// RejectionRecord is a Rejection on the wire.
type RejectionRecord struct {
	List   string `json:"list"`
	Index  int    `json:"index"`
	Reason string `json:"reason"`
	RuleID string `json:"rule_id,omitempty"`
}

// AllowlistStatusRecord is the allowlist read's outcome on the wire. Ref is
// the admission commit the file was read at ("" when none was recorded);
// Detail is a fixed structural class, never error text.
type AllowlistStatusRecord struct {
	Path          string            `json:"path"`
	Ref           string            `json:"ref"`
	Status        string            `json:"status"`
	Detail        string            `json:"detail,omitempty"`
	RejectedCount int               `json:"rejected_count"`
	Rejected      []RejectionRecord `json:"rejected,omitempty"`
}

// SuppressionSummary is the suppression half of the diff_secrets_detected
// payload, shared by the server's writer and every reader (gate view, PR
// status comment). Allowlist is nil on an entry written before #5230.
type SuppressionSummary struct {
	Allowlist                *AllowlistStatusRecord `json:"allowlist,omitempty"`
	SuppressedHitCount       int                    `json:"suppressed_hit_count"`
	EntrySuppressedHitCount  int                    `json:"entry_suppressed_hit_count"`
	MarkerSuppressedHitCount int                    `json:"marker_suppressed_hit_count"`
	Suppressed               []SuppressionRecord    `json:"suppressed"`
}

// RejectionRecords maps rejections to the wire, capped at MaxRejectedRecords.
// RuleID passes through SafeRuleID's check: an id it would not render is
// dropped.
func RejectionRecords(rej []Rejection) []RejectionRecord {
	out := make([]RejectionRecord, 0, min(len(rej), MaxRejectedRecords))
	for i, r := range rej {
		if i == MaxRejectedRecords {
			break
		}
		rec := RejectionRecord{List: r.List, Index: r.Index, Reason: r.Reason}
		if r.RuleID != "" && SafeRuleID(r.RuleID) == r.RuleID {
			rec.RuleID = r.RuleID
		}
		out = append(out, rec)
	}
	return out
}

// NewSuppressionSummary builds the payload summary: the counts per By and
// the folded records (GroupSuppressions).
func NewSuppressionSummary(status *AllowlistStatusRecord, sup []Suppression) SuppressionSummary {
	s := SuppressionSummary{Allowlist: status, SuppressedHitCount: len(sup), Suppressed: GroupSuppressions(sup)}
	for _, x := range sup {
		if x.By == ByMarker {
			s.MarkerSuppressedHitCount++
		} else {
			s.EntrySuppressedHitCount++
		}
	}
	return s
}

// GroupSuppressions folds suppressions by (path, pattern, by, rule id, rule
// fingerprint) — and, for marker suppressions, the per-hit fingerprint, so
// each distinct marked hit keeps its own record and check key. Lines are
// sorted and de-duplicated; records are ordered by path, pattern, by, rule
// id, rule fingerprint, then check key. A marker record's CheckKey is its
// MarkerCheckKey. Never nil, so the payload carries "suppressed": [].
func GroupSuppressions(sup []Suppression) []SuppressionRecord {
	type key struct{ path, pattern, by, id, fp, hit string }
	idx := map[key]int{}
	out := []SuppressionRecord{}
	for _, s := range sup {
		k := key{s.Hit.Path, s.Hit.Pattern, s.By, s.RuleID, s.RuleFingerprint, ""}
		if s.By == ByMarker {
			k.hit = s.Hit.Fingerprint
		}
		i, ok := idx[k]
		if !ok {
			i = len(out)
			idx[k] = i
			rec := SuppressionRecord{Path: k.path, Pattern: k.pattern, By: k.by, RuleID: k.id, RuleFingerprint: k.fp}
			if s.By == ByMarker {
				rec.CheckKey = MarkerCheckKey(k.pattern, k.id, k.fp, k.hit, k.path)
			}
			out = append(out, rec)
		}
		out[i].Lines = append(out[i].Lines, s.Hit.Line)
	}
	for i := range out {
		sort.Ints(out[i].Lines)
		out[i].Lines = uniqueSorted(out[i].Lines)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		for _, c := range [][2]string{{a.Path, b.Path}, {a.Pattern, b.Pattern}, {a.By, b.By},
			{a.RuleID, b.RuleID}, {a.RuleFingerprint, b.RuleFingerprint}, {a.CheckKey, b.CheckKey}} {
			if c[0] != c[1] {
				return c[0] < c[1]
			}
		}
		return false
	})
	return out
}
