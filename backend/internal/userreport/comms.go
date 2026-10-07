package userreport

// The comms draft marker (E81.5 / #3775, phase 1 #4011): the single-line
// provenance marker a captain-approved user-report draft carries, naming the
// user reports it answers and the content hash each report had when the draft
// was proposed. A later scan reads it back to suppress re-proposing a report
// whose content has not materially changed.
//
// TRUST RULE. The marker is attacker-writable body text. It is trusted ONLY
// on an item Classify reports as fishhawk_filed; a Result with
// MarkerFromExternal set is never trusted (phase 4, #4014, enforces this at
// the consumer). Contract: README.md § "Comms draft marker".

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/kuhlman-labs/fishhawk/backend/internal/prompt"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// CommsMarkerName is the comms marker's name/version token, reported as the
// classification basis "marker:fishhawk-comms:v1".
const CommsMarkerName = "fishhawk-comms:v1"

// CommsMarkerPrefix is the exact opening every comms marker carries — a full
// HTML-comment opening, like intakegroom.MarkerPrefix, so prose that merely
// quotes the token does not match.
const CommsMarkerPrefix = "<!-- " + CommsMarkerName + " "

// commsMarkerSuffix closes a comms marker on the same line.
const commsMarkerSuffix = " -->"

// MarkedReport is one user report a draft answers: its canonical report id
// (prompt.UserReportID) and its ContentHash when the draft was proposed.
type MarkedReport struct {
	ID          string `json:"id"`
	ContentHash string `json:"content_hash"`
}

// commsMarkerPayload is the marker's JSON object. Reports is a pointer so a
// marker LACKING the key (or carrying it as null) is distinguishable from one
// carrying an empty list.
type commsMarkerPayload struct {
	Reports *[]MarkedReport `json:"reports"`
}

var contentHashRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// validMarkedReport holds when r's content hash is 64 lowercase hex digits and
// its id is canonical for a real report kind.
func validMarkedReport(r MarkedReport) bool {
	return contentHashRe.MatchString(r.ContentHash) && canonicalReportID(r.ID)
}

// canonicalReportID holds when id parses as UR-issue-<n> (n >= 1) or
// UR-comment-<n>-<c> (n, c >= 1) AND equals what prompt.UserReportID renders
// for those values — the round-trip rejects leading zeros, signs and spaces
// that strconv would accept. UR-unknown-* is refused: workmgmt.UserReportKind
// is closed to issue and comment, so no gathered report carries it.
func canonicalReportID(id string) bool {
	if rest, ok := strings.CutPrefix(id, "UR-issue-"); ok {
		n, err := strconv.Atoi(rest)
		return err == nil && n >= 1 && prompt.UserReportID(string(workmgmt.UserReportKindIssue), n, 0) == id
	}
	if rest, ok := strings.CutPrefix(id, "UR-comment-"); ok {
		ns, cs, ok := strings.Cut(rest, "-")
		if !ok {
			return false
		}
		n, nerr := strconv.Atoi(ns)
		c, cerr := strconv.ParseInt(cs, 10, 64)
		return nerr == nil && cerr == nil && n >= 1 && c >= 1 &&
			prompt.UserReportID(string(workmgmt.UserReportKindComment), n, c) == id
	}
	return false
}

// canonicalReports keeps only valid entries, dedupes on (ID, ContentHash) and
// sorts by ID then ContentHash. It returns nil when nothing survives.
func canonicalReports(reports []MarkedReport) []MarkedReport {
	seen := make(map[MarkedReport]struct{}, len(reports))
	var out []MarkedReport
	for _, r := range reports {
		if !validMarkedReport(r) {
			continue
		}
		if _, dup := seen[r]; dup {
			continue
		}
		seen[r] = struct{}{}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ID != out[j].ID {
			return out[i].ID < out[j].ID
		}
		return out[i].ContentHash < out[j].ContentHash
	})
	return out
}

// DraftMarker renders the comms marker for reports as ONE line:
// CommsMarkerPrefix + {"reports":[...]} + " -->". Invalid entries are dropped,
// duplicates removed and the rest sorted, so equal inputs render equal bytes.
// It returns "" when no valid entry remains. json.Marshal escapes <, > and &,
// and a valid entry holds only [A-Za-z0-9-] runes, so no payload can contain
// "-->" or a line break.
func DraftMarker(reports []MarkedReport) string {
	kept := canonicalReports(reports)
	if len(kept) == 0 {
		return ""
	}
	payload, err := json.Marshal(commsMarkerPayload{Reports: &kept})
	if err != nil {
		return ""
	}
	return CommsMarkerPrefix + string(payload) + commsMarkerSuffix
}

// ParseDraftMarkers walks EVERY occurrence of CommsMarkerPrefix in body and
// returns the deduped, sorted union of the valid entries (nil when there are
// none), plus how many markers it dropped whole as malformed.
//
// A whole marker is dropped (and counted) when it is unterminated, spans a
// line break before its " -->", fails strict decoding (unknown fields at any
// depth, a non-object payload), lacks the reports key or carries it as null,
// or carries a second JSON value or trailing data after the object. Within a
// well-formed marker each entry failing validation is dropped individually
// and NOT counted, so `{"reports":[]}` is well-formed with no entries.
func ParseDraftMarkers(body string) (reports []MarkedReport, malformed int) {
	var all []MarkedReport
	for i := 0; ; {
		at := strings.Index(body[i:], CommsMarkerPrefix)
		if at < 0 {
			break
		}
		start := i + at + len(CommsMarkerPrefix)
		i = start
		entries, ok := parseOneMarker(body[start:])
		if !ok {
			malformed++
			continue
		}
		all = append(all, entries...)
	}
	return canonicalReports(all), malformed
}

// parseOneMarker decodes the marker whose payload starts at rest (just past
// CommsMarkerPrefix). ok is false when the marker is malformed.
func parseOneMarker(rest string) (entries []MarkedReport, ok bool) {
	end := strings.Index(rest, commsMarkerSuffix)
	if end < 0 {
		return nil, false // unterminated
	}
	payload := rest[:end]
	if strings.ContainsAny(payload, "\r\n") {
		return nil, false // spans a line break
	}
	dec := json.NewDecoder(bytes.NewReader([]byte(payload)))
	dec.DisallowUnknownFields()
	var p commsMarkerPayload
	if err := dec.Decode(&p); err != nil {
		return nil, false
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, false // a second value or trailing data
	}
	if p.Reports == nil {
		return nil, false // no reports key
	}
	return *p.Reports, true
}

// ContentHash is the lowercase-hex sha256 of a report's normalized text, the
// value a MarkedReport carries. normalize converts CRLF to LF and trims
// surrounding whitespace. An issue hashes normalize(title) + "\n" +
// normalize(body); a comment (or any non-issue kind) hashes normalize(body)
// and ignores title. Phases 4 and 7 call this function rather than
// re-deriving it.
func ContentHash(kind workmgmt.UserReportKind, title, body string) string {
	text := normalizeReportText(body)
	if kind == workmgmt.UserReportKindIssue {
		text = normalizeReportText(title) + "\n" + text
	}
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

func normalizeReportText(s string) string {
	return strings.TrimSpace(strings.ReplaceAll(s, "\r\n", "\n"))
}
