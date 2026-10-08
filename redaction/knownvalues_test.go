package redaction_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/redaction"
)

// Every known value below is built at runtime and shaped to match NO
// DefaultPatterns regex, so a row turns red only on the known-value control
// it targets, never on a pattern hit (and the diff secrets check stays
// quiet on this file).

// knownSet compiles one binding and fails the test if it was dropped.
func knownSet(t *testing.T, name, value string) *redaction.KnownValues {
	t.Helper()
	kv, below := redaction.NewKnownValues(redaction.KnownValue{Name: name, Value: value})
	if len(below) != 0 || kv.Len() != 1 {
		t.Fatalf("fixture binding %q dropped: below=%v len=%d", name, below, kv.Len())
	}
	return kv
}

// TestKnownValues_RawValue: the prose rows' value carries a `"`, which
// every JSON and URL escaping rewrites, so only the raw needle matches it
// there. The JSON-string row needs an escape-free value to stay valid JSON.
func TestKnownValues_RawValue(t *testing.T) {
	v := `Kv9"mQ` + "2xZp7Lw"
	plain := "Pz8" + "nR4tWq6Ys"
	kv, _ := redaction.NewKnownValues(
		redaction.KnownValue{Name: "ACC_TOKEN", Value: v},
		redaction.KnownValue{Name: "ACC_TOKEN", Value: plain},
	)
	cases := []struct{ name, in, want string }{
		{"prose", "the token is " + v + " here", "the token is [REDACTED:credential:ACC_TOKEN] here"},
		{"twice", v + "," + v, "[REDACTED:credential:ACC_TOKEN],[REDACTED:credential:ACC_TOKEN]"},
		{"json string", `{"msg":"use ` + plain + `"}`, `{"msg":"use [REDACTED:credential:ACC_TOKEN]"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, hits := kv.Redact([]byte(tc.in))
			if string(out) != tc.want {
				t.Errorf("Redact = %q, want %q", out, tc.want)
			}
			if findHit(hits, "credential:ACC_TOKEN") == 0 {
				t.Errorf("hits = %+v, want credential:ACC_TOKEN", hits)
			}
		})
	}
}

// TestKnownValues_URLEscaped plants ONLY the escaped form; the value is
// chosen so every escaping changes bytes, so the raw needle cannot match.
func TestKnownValues_URLEscaped(t *testing.T) {
	v := "ab/cd+ef" + "#gh!ij:kl mn"
	kv := knownSet(t, "GIT_TOKEN", v)
	m := "[REDACTED:credential:GIT_TOKEN]"
	cases := []struct{ name, in, want string }{
		{
			"userinfo password",
			"https://x-access-token:" + "ab%2Fcd+ef%23gh%21ij%3Akl%20mn" + "@host/repo.git",
			"https://x-access-token:" + m + "@host/repo.git",
		},
		{
			"query",
			"https://host/cb?token=" + "ab%2Fcd%2Bef%23gh%21ij%3Akl+mn" + "&x=1",
			"https://host/cb?token=" + m + "&x=1",
		},
		{
			"path segment",
			"https://host/v1/" + "ab%2Fcd+ef%23gh%21ij:kl%20mn" + "/x",
			"https://host/v1/" + m + "/x",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if strings.Contains(tc.in, v) {
				t.Fatalf("fixture carries the raw value")
			}
			out, _ := kv.Redact([]byte(tc.in))
			if string(out) != tc.want {
				t.Errorf("Redact = %q, want %q", out, tc.want)
			}
		})
	}
}

// TestKnownValues_TargetURLUserinfo pins what happens to a grammar-
// constrained field (the acceptance verdict's target_url) carrying a bound
// value in its userinfo: the value becomes the marker, and the result is
// no longer a parseable URL, so a consumer that validates the field's
// grammar after redaction may refuse it. Recorded, not engineered around.
func TestKnownValues_TargetURLUserinfo(t *testing.T) {
	v := "Tp4" + "wLx9Qz2Rm"
	kv := knownSet(t, "TARGET_PASS", v)
	in := "https://user:" + v + "@target.example/app"
	if _, err := url.Parse(in); err != nil {
		t.Fatalf("fixture URL does not parse: %v", err)
	}
	out, _ := kv.Redact([]byte(in))
	want := "https://user:[REDACTED:credential:TARGET_PASS]@target.example/app"
	if string(out) != want {
		t.Fatalf("Redact = %q, want %q", out, want)
	}
	if _, err := url.Parse(string(out)); err == nil {
		t.Errorf("redacted target_url %q parses; the recorded behaviour is that the marker breaks the userinfo grammar", out)
	}
}

// TestKnownValues_JSONEscaped plants the value only through a JSON encoder,
// where `"`, `\` and `&` no longer occur raw.
func TestKnownValues_JSONEscaped(t *testing.T) {
	v := `pa"ss\wo` + `&rd<9>`
	kv := knownSet(t, "DB_PASS", v)
	type doc struct{ Note string }

	htmlEscaped, err := json.Marshal(doc{Note: v})
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(doc{Note: v}); err != nil {
		t.Fatal(err)
	}
	noHTML := bytes.TrimSuffix(buf.Bytes(), []byte("\n"))

	for name, in := range map[string][]byte{"html escaped": htmlEscaped, "no html escaping": noHTML} {
		t.Run(name, func(t *testing.T) {
			if bytes.Contains(in, []byte(v)) {
				t.Fatalf("fixture carries the raw value: %s", in)
			}
			out, _ := kv.Redact(in)
			if want := `{"Note":"[REDACTED:credential:DB_PASS]"}`; string(out) != want {
				t.Errorf("Redact = %s, want %s", out, want)
			}
			if !json.Valid(out) {
				t.Errorf("redacted JSON is invalid: %s", out)
			}
		})
	}
}

// TestKnownValues_StandaloneBase64 covers base64 of the value alone in both
// alphabets, padded and raw. The value is chosen so the two alphabets differ
// and the encoding is padded.
func TestKnownValues_StandaloneBase64(t *testing.T) {
	v := "Tm9~v>q?" + "W8xKz"
	kv := knownSet(t, "B64_TOKEN", v)
	std := base64.StdEncoding.EncodeToString([]byte(v))
	if std == base64.URLEncoding.EncodeToString([]byte(v)) || !strings.HasSuffix(std, "=") {
		t.Fatalf("fixture value does not separate the alphabets/padding: %s", std)
	}
	for name, enc := range map[string]*base64.Encoding{
		"std": base64.StdEncoding, "url": base64.URLEncoding,
		"raw std": base64.RawStdEncoding, "raw url": base64.RawURLEncoding,
	} {
		t.Run(name, func(t *testing.T) {
			in := "cred=" + enc.EncodeToString([]byte(v)) + " end"
			out, _ := kv.Redact([]byte(in))
			if want := "cred=[REDACTED:credential:B64_TOKEN] end"; string(out) != want {
				t.Errorf("Redact = %q, want %q", out, want)
			}
		})
	}
}

// base64Recoverable reports whether any run of base64 characters in b
// decodes, at any character offset, to bytes containing v.
func base64Recoverable(b []byte, v string) bool {
	isB64 := func(c byte) bool {
		return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '+' || c == '/'
	}
	for i := 0; i < len(b); {
		if !isB64(b[i]) {
			i++
			continue
		}
		j := i
		for j < len(b) && isB64(b[j]) {
			j++
		}
		run := b[i:j]
		for off := 0; off < 4 && off < len(run); off++ {
			s := run[off:]
			s = s[:len(s)-len(s)%4]
			if dec, err := base64.StdEncoding.DecodeString(string(s)); err == nil && bytes.Contains(dec, []byte(v)) {
				return true
			}
		}
		i = j
	}
	return false
}

// TestKnownValues_Base64CoreShifts embeds the value at byte offset 0, 1 and
// 2 inside a longer base64 blob with no `Basic` keyword, so on row k only
// the shift-k core occurs in the bytes. The value is 20 bytes (20 % 3 != 0)
// and the suffix starts with a byte whose high bits are set, so the encoding
// of the value alone never matches the embedded form.
func TestKnownValues_Base64CoreShifts(t *testing.T) {
	v := "Hq7Lm2" + "Wx9Pz4" + "Rk8Vn3Ty"
	kv := knownSet(t, "BLOB_TOKEN", v)
	for shift := 0; shift < 3; shift++ {
		t.Run(string(rune('0'+shift)), func(t *testing.T) {
			blob := strings.Repeat("P", shift) + v + "SUFFIX-bytes"
			in := []byte("blob " + base64.StdEncoding.EncodeToString([]byte(blob)) + " end")
			if !base64Recoverable(in, v) {
				t.Fatalf("fixture sanity: value not recoverable from the unredacted blob")
			}
			out, hits := kv.Redact(in)
			if base64Recoverable(out, v) {
				t.Errorf("value still decodes out of the redacted blob: %s", out)
			}
			if findHit(hits, "credential:BLOB_TOKEN") != 1 {
				t.Errorf("hits = %+v, want one credential:BLOB_TOKEN", hits)
			}
		})
	}
}

// TestKnownValues_BasicCredential asserts a Basic credential whose decoded
// bytes carry the value is replaced WHOLE, username included — a result the
// core needles alone cannot produce.
func TestKnownValues_BasicCredential(t *testing.T) {
	v := "Gt5Nw8" + "Lq3Zx7Vb"
	kv := knownSet(t, "GIT_TOKEN", v)
	for name, in := range map[string]string{
		"std":         "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+v)),
		"raw url":     "authorization: basic " + base64.RawURLEncoding.EncodeToString([]byte("x-access-token:"+v)),
		"value first": "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte(v+":x")),
	} {
		t.Run(name, func(t *testing.T) {
			out, hits := kv.Redact([]byte(in))
			want := in[:len("Authorization: Basic ")] + "[REDACTED:credential:GIT_TOKEN]"
			if string(out) != want {
				t.Errorf("Redact = %q, want %q", out, want)
			}
			if findHit(hits, "credential:GIT_TOKEN") != 1 {
				t.Errorf("hits = %+v, want one credential:GIT_TOKEN", hits)
			}
		})
	}

	t.Run("unrelated credential untouched", func(t *testing.T) {
		in := "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("alice:hunter2hunter2")) + " and basic usage"
		out, hits := kv.Redact([]byte(in))
		if string(out) != in || hits != nil {
			t.Errorf("Redact = %q hits=%+v, want input unchanged", out, hits)
		}
	})
}

// TestKnownValues_BasicCredentialAttributedToLongestValue: when the decoded
// credential carries two bound values, one a prefix of the other, the whole
// credential is attributed to the longer binding even though the shorter
// one was bound first.
func TestKnownValues_BasicCredentialAttributedToLongestValue(t *testing.T) {
	short := "Zq4Wn8" + "Rt2Pv"
	long := short + "TailX9Yz"
	kv, _ := redaction.NewKnownValues(
		redaction.KnownValue{Name: "SHORT_TOKEN", Value: short},
		redaction.KnownValue{Name: "LONG_TOKEN", Value: long},
	)
	in := "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("u:"+long))
	out, _ := kv.Redact([]byte(in))
	if want := "Authorization: Basic [REDACTED:credential:LONG_TOKEN]"; string(out) != want {
		t.Errorf("Redact = %q, want %q", out, want)
	}
}

func TestKnownValues_Floor(t *testing.T) {
	seven := "Ab3dE5" + "g"
	eight := "Hj8kL0" + "mN"
	kv, below := redaction.NewKnownValues(
		redaction.KnownValue{Name: "SHORT_PASS", Value: seven},
		redaction.KnownValue{Name: "OK_PASS", Value: eight},
	)
	if len(below) != 1 || below[0] != "SHORT_PASS" {
		t.Errorf("below-floor names = %q, want [SHORT_PASS]", below)
	}
	for _, n := range below {
		if strings.Contains(n, seven) {
			t.Errorf("below-floor report carries the value: %q", n)
		}
	}
	if kv.Len() != 1 {
		t.Errorf("Len = %d, want 1", kv.Len())
	}
	out, _ := kv.Redact([]byte("a " + seven + " b " + eight))
	if want := "a " + seven + " b [REDACTED:credential:OK_PASS]"; string(out) != want {
		t.Errorf("Redact = %q, want %q", out, want)
	}
	if redaction.MinKnownValueBytes != len(eight) {
		t.Errorf("MinKnownValueBytes = %d, fixture assumes %d", redaction.MinKnownValueBytes, len(eight))
	}
}

// TestKnownValues_EmptyValueReported: an empty value is under the floor
// too — dropped, reported by name, and the set stays empty.
func TestKnownValues_EmptyValueReported(t *testing.T) {
	kv, below := redaction.NewKnownValues(redaction.KnownValue{Name: "EMPTY_PASS", Value: ""})
	if len(below) != 1 || below[0] != "EMPTY_PASS" || kv.Len() != 0 {
		t.Errorf("below = %q len = %d, want [EMPTY_PASS] and 0", below, kv.Len())
	}
	if out, hits := kv.Redact([]byte("unchanged")); string(out) != "unchanged" || hits != nil {
		t.Errorf("empty set changed its input: %q %+v", out, hits)
	}
}

// TestKnownValues_LongestFirst binds the shorter value FIRST; without the
// longest-first needle order its needle would consume the longer value's
// prefix and leave the tail behind.
func TestKnownValues_LongestFirst(t *testing.T) {
	short := "Zq4Wn8" + "Rt2Pv"
	long := short + "TailX9Yz"
	kv, _ := redaction.NewKnownValues(
		redaction.KnownValue{Name: "SHORT_TOKEN", Value: short},
		redaction.KnownValue{Name: "LONG_TOKEN", Value: long},
	)
	out, hits := kv.Redact([]byte("x " + long + " y " + short))
	if want := "x [REDACTED:credential:LONG_TOKEN] y [REDACTED:credential:SHORT_TOKEN]"; string(out) != want {
		t.Errorf("Redact = %q, want %q", out, want)
	}
	if findHit(hits, "credential:LONG_TOKEN") != 1 || findHit(hits, "credential:SHORT_TOKEN") != 1 {
		t.Errorf("hits = %+v", hits)
	}
}

// TestRedactDefaultKnown_KnownValuesBeforePatterns: a value that begins
// with a pattern-shaped prefix is replaced whole; patterns-first would
// redact only the prefix and leave the tail.
func TestRedactDefaultKnown_KnownValuesBeforePatterns(t *testing.T) {
	v := "ghp_" + strings.Repeat("a", 36) + "TAILSECRET99"
	kv := knownSet(t, "PAT", v)
	out, hits := redaction.RedactDefaultKnown([]byte("k="+v+";"), kv)
	if want := "k=[REDACTED:credential:PAT];"; string(out) != want {
		t.Errorf("RedactDefaultKnown = %q, want %q", out, want)
	}
	if bytes.Contains(out, []byte("TAILSECRET")) {
		t.Errorf("tail survived: %s", out)
	}
	if findHit(hits, "credential:PAT") != 1 || findHit(hits, "github-pat-classic") != 0 {
		t.Errorf("hits = %+v", hits)
	}
}

// TestRedactDefaultKnown_MergesHits: known-value and pattern hits come back
// in one name-sorted slice.
func TestRedactDefaultKnown_MergesHits(t *testing.T) {
	v := "Mx3Qv8" + "Np2Kw7"
	kv := knownSet(t, "ACC_TOKEN", v)
	in := v + " and AKIA" + strings.Repeat("Q", 16)
	out, hits := redaction.RedactDefaultKnown([]byte(in), kv)
	if want := "[REDACTED:credential:ACC_TOKEN] and [REDACTED:aws-access-key-id]"; string(out) != want {
		t.Errorf("RedactDefaultKnown = %q, want %q", out, want)
	}
	if len(hits) != 2 || hits[0].Pattern != "aws-access-key-id" || hits[1].Pattern != "credential:ACC_TOKEN" {
		t.Errorf("hits = %+v, want [aws-access-key-id credential:ACC_TOKEN]", hits)
	}
}

// TestKnownValues_InvalidNameGetsGenericMarker: a binding name outside the
// grammar still redacts, under the generic marker, and cannot break JSON.
func TestKnownValues_InvalidNameGetsGenericMarker(t *testing.T) {
	v := "Wd6Hs1" + "Jc9Fb4"
	for _, name := range []string{`BAD"]NAME`, "9STARTS_WITH_DIGIT", strings.Repeat("N", 65), "", "has space"} {
		t.Run(name[:min(len(name), 12)], func(t *testing.T) {
			kv := knownSet(t, name, v)
			out, hits := kv.Redact([]byte(`{"x":"` + v + `"}`))
			if want := `{"x":"[REDACTED:credential]"}`; string(out) != want {
				t.Errorf("Redact = %s, want %s", out, want)
			}
			if !json.Valid(out) {
				t.Errorf("output is not valid JSON: %s", out)
			}
			if len(hits) != 1 || hits[0].Pattern != "credential" {
				t.Errorf("hits = %+v, want one generic credential hit", hits)
			}
		})
	}
	t.Run("64-char name is valid", func(t *testing.T) {
		name := strings.Repeat("N", 64)
		out, _ := knownSet(t, name, v).Redact([]byte(v))
		if want := "[REDACTED:credential:" + name + "]"; string(out) != want {
			t.Errorf("Redact = %s, want %s", out, want)
		}
	})
}

func TestKnownValues_NilAndEmptyAreNoOps(t *testing.T) {
	in := []byte("nothing Kv9mQ2xZp7Lw " + "Authorization: Basic dXNlcjpwYXNz")
	var nilSet *redaction.KnownValues
	empty, below := redaction.NewKnownValues()
	if below != nil || empty.Len() != 0 || nilSet.Len() != 0 {
		t.Fatalf("empty/nil set not empty: below=%v", below)
	}
	for name, kv := range map[string]*redaction.KnownValues{"nil": nilSet, "empty": empty} {
		t.Run(name, func(t *testing.T) {
			out, hits := kv.Redact(in)
			if !bytes.Equal(out, in) || hits != nil {
				t.Errorf("Redact = %q hits=%+v, want input unchanged and nil hits", out, hits)
			}
			wantOut, wantHits := redaction.RedactDefault(in)
			gotOut, gotHits := redaction.RedactDefaultKnown(in, kv)
			if !bytes.Equal(gotOut, wantOut) || len(gotHits) != len(wantHits) {
				t.Errorf("RedactDefaultKnown = %q %+v, want RedactDefault's %q %+v", gotOut, gotHits, wantOut, wantHits)
			}
		})
	}
	t.Run("empty input", func(t *testing.T) {
		kv := knownSet(t, "ACC_TOKEN", "Kv9mQ2xZp7Lw")
		if out, hits := kv.Redact(nil); out != nil || hits != nil {
			t.Errorf("Redact(nil) = %q %+v", out, hits)
		}
	})
}

// TestKnownValues_HitsNeverCarryTheValue: every hit is named by binding and
// counted; no hit field carries value bytes.
func TestKnownValues_HitsNeverCarryTheValue(t *testing.T) {
	v := "Rr5Tt6" + "Yy7Uu8"
	kv := knownSet(t, "ACC_TOKEN", v)
	in := v + " " + url.QueryEscape(v) + " " + base64.StdEncoding.EncodeToString([]byte(v))
	out, hits := kv.Redact([]byte(in))
	if bytes.Contains(out, []byte(v)) {
		t.Errorf("value survived: %s", out)
	}
	if len(hits) != 1 || hits[0].Pattern != "credential:ACC_TOKEN" || hits[0].Count != 3 {
		t.Fatalf("hits = %+v, want one credential:ACC_TOKEN x3", hits)
	}
	for _, h := range hits {
		if strings.Contains(h.Pattern, v) {
			t.Errorf("hit carries the value: %+v", h)
		}
	}
}
