package redaction

// Known-value redaction (E72.41 / #3793).
//
// DefaultPatterns redacts credentials by SHAPE. A credential bound into an
// agent through a declared binding (ADR-086 decision 2) has no shape the
// pattern set can know, but its VALUE is known to the caller that bound it.
// KnownValues redacts by value: every occurrence of a bound value, in each
// encoding the value plausibly takes on its way into a trace, verdict or
// transcript, is replaced with a marker naming the binding, never the value.

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

// MinKnownValueBytes is the floor below which a bound value is NOT
// redacted. Replacing every occurrence of a very short string would shred
// unrelated evidence (a 3-byte value occurs all over a trace). A value under
// the floor is reported back to the caller BY BINDING NAME so the omission
// is visible; a real credential that short leaks.
const MinKnownValueBytes = 8

// KnownValue is one bound credential: Name is the binding name the marker
// carries, Value is the secret bytes to redact.
type KnownValue struct {
	Name  string
	Value string
}

// KnownValues is a compiled, immutable set of known values. The zero value
// and a nil pointer are both an empty set: Redact on either returns its
// input unchanged with nil hits, so a caller can thread the set
// unconditionally.
type KnownValues struct {
	// entries holds one row per retained value, in binding order.
	entries []knownEntry
	// needles holds every encoded form of every retained value,
	// de-duplicated and sorted longest first.
	needles []knownNeedle
}

type knownEntry struct {
	value  []byte
	marker []byte
	hit    string
}

type knownNeedle struct {
	b     []byte
	entry int
}

// knownValueName is the binding-name grammar the marker may carry. A name
// outside it gets the generic marker, so a malformed name never suppresses
// redaction and can never inject `"`, `]` or a newline into the output.
var knownValueName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)

// basicCredential matches an HTTP Basic credential. The capture is the
// base64 token alone, so the `Basic ` keyword survives the replacement.
var basicCredential = regexp.MustCompile(`(?i)\bbasic\s+([A-Za-z0-9+/_\-]+=*)`)

// NewKnownValues compiles bindings into a KnownValues set. It returns the
// set plus the NAMES (never the values) of the bindings it dropped because
// their value was empty or shorter than MinKnownValueBytes.
func NewKnownValues(bindings ...KnownValue) (*KnownValues, []string) {
	kv := &KnownValues{}
	var below []string
	for _, b := range bindings {
		if len(b.Value) < MinKnownValueBytes {
			below = append(below, b.Name)
			continue
		}
		hit, marker := "credential", "[REDACTED:credential]"
		if knownValueName.MatchString(b.Name) {
			hit = "credential:" + b.Name
			marker = "[REDACTED:" + hit + "]"
		}
		kv.entries = append(kv.entries, knownEntry{
			value:  []byte(b.Value),
			marker: []byte(marker),
			hit:    hit,
		})
	}

	seen := make(map[string]bool)
	for i, e := range kv.entries {
		for _, n := range encodedForms(string(e.value)) {
			if seen[n] {
				continue
			}
			seen[n] = true
			kv.needles = append(kv.needles, knownNeedle{b: []byte(n), entry: i})
		}
	}
	// Longest first across ALL values: a value that is a substring of
	// another, or a short encoding of a longer value, must not pre-empt
	// the longer needle and leave its tail behind.
	sort.SliceStable(kv.needles, func(i, j int) bool {
		return len(kv.needles[i].b) > len(kv.needles[j].b)
	})
	return kv, below
}

// Len reports how many values the set redacts (bindings under the floor
// are not counted). A nil set has length 0.
func (kv *KnownValues) Len() int {
	if kv == nil {
		return 0
	}
	return len(kv.entries)
}

// Redact replaces every known value in input, in every encoded form, with
// its binding's marker. A whole HTTP Basic credential whose decoded bytes
// contain a known value is replaced first, username included; then each
// needle is replaced longest first. Hits are named `credential:<NAME>` (or
// `credential` for a binding whose name failed the grammar) and sorted by
// name; they never carry the value.
func (kv *KnownValues) Redact(input []byte) ([]byte, []Hit) {
	if kv == nil || len(kv.entries) == 0 || len(input) == 0 {
		return input, nil
	}
	hitMap := make(map[string]int)
	out := kv.redactBasic(input, hitMap)
	for _, n := range kv.needles {
		c := bytes.Count(out, n.b)
		if c == 0 {
			continue
		}
		e := kv.entries[n.entry]
		hitMap[e.hit] += c
		out = bytes.ReplaceAll(out, n.b, e.marker)
	}
	return out, sortedHits(hitMap)
}

// redactBasic replaces the whole base64 token of every `Basic <token>`
// credential whose decoded bytes contain a known value. The needle pass
// alone would leave the username-derived leading characters and the
// alignment boundary characters of such a token in place.
func (kv *KnownValues) redactBasic(input []byte, hitMap map[string]int) []byte {
	locs := basicCredential.FindAllSubmatchIndex(input, -1)
	if len(locs) == 0 {
		return input
	}
	var out []byte
	last, replaced := 0, false
	for _, loc := range locs {
		start, end := loc[2], loc[3]
		e, ok := kv.decodedEntry(input[start:end])
		if !ok {
			continue
		}
		out = append(out, input[last:start]...)
		out = append(out, e.marker...)
		hitMap[e.hit]++
		last, replaced = end, true
	}
	if !replaced {
		return input
	}
	return append(out, input[last:]...)
}

// decodedEntry decodes token under each base64 variant and returns the
// LONGEST entry whose value the decoded bytes contain, so a credential
// carrying a value that has a shorter bound value as a substring is
// attributed to the longer binding.
func (kv *KnownValues) decodedEntry(token []byte) (knownEntry, bool) {
	encs := []*base64.Encoding{
		base64.StdEncoding, base64.URLEncoding,
		base64.RawStdEncoding, base64.RawURLEncoding,
	}
	for _, enc := range encs {
		dec, err := enc.DecodeString(string(token))
		if err != nil {
			continue
		}
		best := -1
		for i, e := range kv.entries {
			if bytes.Contains(dec, e.value) && (best < 0 || len(e.value) > len(kv.entries[best].value)) {
				best = i
			}
		}
		if best >= 0 {
			return kv.entries[best], true
		}
	}
	return knownEntry{}, false
}

// encodedForms returns every form of v the needle pass replaces: raw;
// JSON-string-escaped (HTML-escaping on and off, quotes stripped); URL
// query, path and userinfo-password escaped; base64 of v alone (Std and
// URL alphabets, padded and raw); and the alignment-independent base64
// cores of v at byte shifts 0, 1 and 2, which match v embedded anywhere
// inside a longer base64 string.
func encodedForms(v string) []string {
	forms := []string{v}

	// Encoding a Go string to JSON cannot fail (invalid UTF-8 becomes
	// U+FFFD), so the errors are discarded.
	j, _ := json.Marshal(v)
	forms = append(forms, string(j[1:len(j)-1]))
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	noHTML := strings.TrimSuffix(buf.String(), "\n")
	forms = append(forms, noHTML[1:len(noHTML)-1])

	forms = append(forms,
		url.QueryEscape(v),
		url.PathEscape(v),
		strings.TrimPrefix(url.UserPassword("", v).String(), ":"),
	)

	for _, e := range []*base64.Encoding{
		base64.StdEncoding, base64.URLEncoding,
		base64.RawStdEncoding, base64.RawURLEncoding,
	} {
		forms = append(forms, e.EncodeToString([]byte(v)))
	}

	for _, e := range []*base64.Encoding{base64.RawStdEncoding, base64.RawURLEncoding} {
		for shift := 0; shift < 3; shift++ {
			forms = append(forms, base64Core(e, v, shift))
		}
	}
	return forms
}

// base64Core encodes v preceded by shift zero bytes and keeps only the
// characters that depend on v's bits alone: it drops the leading
// characters that mix in prefix bits (0, 2 or 3 for shift 0, 1 or 2) and,
// when the encoded length is not a whole number of 3-byte groups, the final
// character, which mixes in whatever byte follows v. The same technique as
// the GitHub Actions runner's secret masker (Base64 shift encoders).
func base64Core(enc *base64.Encoding, v string, shift int) string {
	data := append(make([]byte, shift), v...)
	s := enc.EncodeToString(data)
	s = s[[]int{0, 2, 3}[shift]:]
	if len(data)%3 != 0 {
		s = s[:len(s)-1]
	}
	return s
}

// RedactDefaultKnown redacts kv's known values, THEN applies
// DefaultPatterns, and returns the merged hits sorted by name. Known values
// run first so a value that begins with a pattern-shaped prefix is replaced
// whole instead of leaving its non-pattern tail. With a nil or empty kv it
// is exactly RedactDefault.
func RedactDefaultKnown(input []byte, kv *KnownValues) ([]byte, []Hit) {
	out, known := kv.Redact(input)
	out, pattern := RedactDefault(out)
	if len(known) == 0 {
		return out, pattern
	}
	hitMap := make(map[string]int, len(known)+len(pattern))
	for _, h := range known {
		hitMap[h.Pattern] += h.Count
	}
	for _, h := range pattern {
		hitMap[h.Pattern] += h.Count
	}
	return out, sortedHits(hitMap)
}

// sortedHits flattens a name→count map into hits sorted by name, or nil
// when nothing fired.
func sortedHits(hitMap map[string]int) []Hit {
	if len(hitMap) == 0 {
		return nil
	}
	hits := make([]Hit, 0, len(hitMap))
	for name, n := range hitMap {
		hits = append(hits, Hit{Pattern: name, Count: n})
	}
	sort.Slice(hits, func(i, j int) bool {
		return hits[i].Pattern < hits[j].Pattern
	})
	return hits
}
