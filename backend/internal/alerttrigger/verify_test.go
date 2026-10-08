package alerttrigger

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	testSourceID = "pager"
	testSecret   = "0123456789abcdef0123456789abcdef-pager" // 38 bytes
	testWindow   = 5 * time.Minute
)

var testNow = time.Unix(1_800_000_000, 0)

const testBody = `{"fingerprint":"db:conn-pool","title":"Pool exhausted","severity":"high"}`

func testSources(t *testing.T) Sources {
	t.Helper()
	doc := "version: 1\nsources:\n  - id: " + testSourceID + "\n    secret_env: PAGER_SECRET\n    repo: acme/shop\n"
	srcs, err := ParseSources([]byte(doc), envMap(map[string]string{"PAGER_SECRET": testSecret}))
	if err != nil {
		t.Fatalf("ParseSources: %v", err)
	}
	return srcs
}

func ts(t time.Time) string { return strconv.FormatInt(t.Unix(), 10) }

func TestSign_SigningStringIsTimestampDotBody(t *testing.T) {
	mac := hmac.New(sha256.New, []byte(testSecret))
	mac.Write([]byte("1800000000." + testBody))
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if got := Sign([]byte(testSecret), "1800000000", []byte(testBody)); got != want {
		t.Fatalf("Sign = %s, want %s", got, want)
	}
	// Known answer, also produced by the README's openssl sender sketch
	// (printf '%s.%s' "$ts" "$body" | openssl dgst -sha256 -hmac ...).
	const knownAnswer = "sha256=343929c06e4059eb42aa6f17198b754452b0897d997c10c454d59ccd8c1e9d97"
	if got := Sign([]byte(testSecret), "1800000000", []byte(testBody)); got != knownAnswer {
		t.Fatalf("Sign = %s, want the README sketch's %s", got, knownAnswer)
	}
}

func TestVerify_Valid(t *testing.T) {
	srcs := testSources(t)
	sig := Sign([]byte(testSecret), ts(testNow), []byte(testBody))
	src, mac, err := Verify(srcs, testSourceID, ts(testNow), sig, []byte(testBody), testNow, testWindow)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if src.ID != testSourceID || src.Repo != "acme/shop" {
		t.Fatalf("source = %+v, want id %q repo acme/shop", src, testSourceID)
	}
	if got := SignaturePrefix + hex.EncodeToString(mac); got != sig {
		t.Fatalf("returned MAC %s does not decode the signature %s", got, sig)
	}
}

// The nonce material is the DECODED MAC: an upper-cased hex resend of one
// signature verifies and yields byte-identical MAC bytes.
func TestVerify_UpperCaseHexYieldsSameMAC(t *testing.T) {
	srcs := testSources(t)
	sig := Sign([]byte(testSecret), ts(testNow), []byte(testBody))
	upper := SignaturePrefix + strings.ToUpper(strings.TrimPrefix(sig, SignaturePrefix))
	_, lower, err := Verify(srcs, testSourceID, ts(testNow), sig, []byte(testBody), testNow, testWindow)
	if err != nil {
		t.Fatal(err)
	}
	_, up, err := Verify(srcs, testSourceID, ts(testNow), upper, []byte(testBody), testNow, testWindow)
	if err != nil {
		t.Fatalf("upper-case hex: %v", err)
	}
	if hex.EncodeToString(lower) != hex.EncodeToString(up) {
		t.Fatalf("MAC differs by hex case: %x vs %x", lower, up)
	}
}

func TestVerify_SignatureMissing(t *testing.T) {
	srcs := testSources(t)
	valid := Sign([]byte(testSecret), ts(testNow), []byte(testBody))
	for name, sig := range map[string]string{
		"absent":         "",
		"no prefix":      strings.TrimPrefix(valid, SignaturePrefix),
		"wrong prefix":   "SHA256=" + strings.TrimPrefix(valid, SignaturePrefix),
		"empty digest":   SignaturePrefix,
		"non-hex digest": SignaturePrefix + "zz" + strings.TrimPrefix(valid, SignaturePrefix)[2:],
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := Verify(srcs, testSourceID, ts(testNow), sig, []byte(testBody), testNow, testWindow)
			if !errors.Is(err, ErrSignatureMissing) {
				t.Fatalf("err = %v, want ErrSignatureMissing", err)
			}
		})
	}
}

// Each malformed timestamp is signed with the real key over that exact
// string, so only the timestamp parse stands between it and acceptance.
func TestVerify_TimestampInvalid(t *testing.T) {
	srcs := testSources(t)
	for name, tsh := range map[string]string{
		"empty":       "",
		"non-digit":   "17000000a0",
		"13 digits":   "1800000000000",
		"negative":    "-1800000000",
		"plus sign":   "+1800000000",
		"whitespace":  " 1800000000",
		"fractional":  "1800000000.5",
		"hex literal": "0x6b49d200",
	} {
		t.Run(name, func(t *testing.T) {
			sig := Sign([]byte(testSecret), tsh, []byte(testBody))
			_, _, err := Verify(srcs, testSourceID, tsh, sig, []byte(testBody), testNow, testWindow)
			if !errors.Is(err, ErrTimestampInvalid) {
				t.Fatalf("err = %v, want ErrTimestampInvalid", err)
			}
		})
	}
}

func TestVerify_TwelveDigitTimestampParses(t *testing.T) {
	srcs := testSources(t)
	far := time.Unix(100_000_000_000, 0) // 12 digits
	sig := Sign([]byte(testSecret), ts(far), []byte(testBody))
	if _, _, err := Verify(srcs, testSourceID, ts(far), sig, []byte(testBody), far, testWindow); err != nil {
		t.Fatalf("12-digit timestamp: %v", err)
	}
}

func TestVerify_UnrelatedKeyRefused(t *testing.T) {
	srcs := testSources(t)
	other := make([]byte, 32)
	if _, err := rand.Read(other); err != nil {
		t.Fatal(err)
	}
	sig := Sign(other, ts(testNow), []byte(testBody))
	src, mac, err := Verify(srcs, testSourceID, ts(testNow), sig, []byte(testBody), testNow, testWindow)
	if !errors.Is(err, ErrSignatureInvalid) {
		t.Fatalf("err = %v, want ErrSignatureInvalid", err)
	}
	if src.ID != "" || mac != nil {
		t.Fatalf("refusal leaked source %q / mac %x", src.ID, mac)
	}
}

// The request is signed with the DUMMY key the unknown-source path uses, so
// its MAC matches and only the unknown-source check refuses it.
func TestVerify_UnknownSourceRefusedLikeMisSign(t *testing.T) {
	srcs := testSources(t)
	sig := Sign(unknownSourceKey, ts(testNow), []byte(testBody))
	src, _, err := Verify(srcs, "not-configured", ts(testNow), sig, []byte(testBody), testNow, testWindow)
	if !errors.Is(err, ErrSignatureInvalid) {
		t.Fatalf("err = %v, want ErrSignatureInvalid (indistinguishable from a mis-sign)", err)
	}
	if src.ID != "" {
		t.Fatalf("unknown source resolved to %+v", src)
	}
	// And the real secret under an unknown id is refused the same way.
	real := Sign([]byte(testSecret), ts(testNow), []byte(testBody))
	if _, _, err := Verify(srcs, "not-configured", ts(testNow), real, []byte(testBody), testNow, testWindow); !errors.Is(err, ErrSignatureInvalid) {
		t.Fatalf("real-secret unknown source: err = %v, want ErrSignatureInvalid", err)
	}
}

func TestVerify_ZeroSourcesRefuses(t *testing.T) {
	sig := Sign(unknownSourceKey, ts(testNow), []byte(testBody))
	if _, _, err := Verify(Sources{}, testSourceID, ts(testNow), sig, []byte(testBody), testNow, testWindow); !errors.Is(err, ErrSignatureInvalid) {
		t.Fatalf("err = %v, want ErrSignatureInvalid", err)
	}
}

func TestVerify_BodyAlteredRefused(t *testing.T) {
	srcs := testSources(t)
	sig := Sign([]byte(testSecret), ts(testNow), []byte(testBody))
	altered := []byte(strings.Replace(testBody, "high", "info", 1))
	if _, _, err := Verify(srcs, testSourceID, ts(testNow), sig, altered, testNow, testWindow); !errors.Is(err, ErrSignatureInvalid) {
		t.Fatalf("err = %v, want ErrSignatureInvalid", err)
	}
}

// A timestamp moved after signing breaks the MAC: the timestamp is signed.
func TestVerify_TimestampMovedRefused(t *testing.T) {
	srcs := testSources(t)
	sig := Sign([]byte(testSecret), ts(testNow), []byte(testBody))
	moved := ts(testNow.Add(time.Second))
	if _, _, err := Verify(srcs, testSourceID, moved, sig, []byte(testBody), testNow, testWindow); !errors.Is(err, ErrSignatureInvalid) {
		t.Fatalf("err = %v, want ErrSignatureInvalid", err)
	}
}

// The MAC is VALID over each stale timestamp, so only the window comparison
// stands between it and acceptance; the future case pins the absolute value.
func TestVerify_StaleRefused(t *testing.T) {
	srcs := testSources(t)
	for name, at := range map[string]time.Time{
		"10m past":   testNow.Add(-10 * time.Minute),
		"10m future": testNow.Add(10 * time.Minute),
		"just past":  testNow.Add(-testWindow - time.Second),
		"just ahead": testNow.Add(testWindow + time.Second),
	} {
		t.Run(name, func(t *testing.T) {
			sig := Sign([]byte(testSecret), ts(at), []byte(testBody))
			src, mac, err := Verify(srcs, testSourceID, ts(at), sig, []byte(testBody), testNow, testWindow)
			if !errors.Is(err, ErrStale) {
				t.Fatalf("err = %v, want ErrStale", err)
			}
			if src.ID != "" || mac != nil {
				t.Fatalf("stale refusal leaked source %q / mac %x", src.ID, mac)
			}
		})
	}
}

// Binding fix A regression (#1601): the window comparison must not go
// through a signed Duration. A validly signed 999999999999 (~31,700 years
// ahead of the receiver) made now.Sub saturate to math.MinInt64, whose
// negation wraps to itself, so `|skew| > window` was false and the request
// authenticated. The far-past row does NOT overflow (~56 years is ~1.8e18 ns)
// and stays green under that bug — it is kept as a separate row so a fix that
// only handles one direction is still caught. Every row is validly signed, so
// only the window comparison stands between it and acceptance.
func TestVerify_FarFutureTimestampIsStale(t *testing.T) {
	srcs := testSources(t)
	now := time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, ts string
		wantErr  error
	}{
		{"far future 999999999999", "999999999999", ErrStale},
		{"far past 1", "1", ErrStale},
		{"far past 0", "0", ErrStale},
		{"exactly window behind", ts(now.Add(-testWindow)), nil},
		{"exactly window ahead", ts(now.Add(testWindow)), nil},
		{"window+1s behind", ts(now.Add(-testWindow - time.Second)), ErrStale},
		{"window+1s ahead", ts(now.Add(testWindow + time.Second)), ErrStale},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sig := Sign([]byte(testSecret), tc.ts, []byte(testBody))
			src, mac, err := Verify(srcs, testSourceID, tc.ts, sig, []byte(testBody), now, testWindow)
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("err = %v, want acceptance", err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if src.ID != "" || mac != nil {
				t.Fatalf("stale refusal leaked source %q / mac %x", src.ID, mac)
			}
		})
	}
}

// The largest window fishhawkd admits (15m) keeps the 12-digit far-future
// refusal: Add(±window) on time.Unix(999999999999, 0) is in range.
func TestVerify_FarFutureStaleAtMaxWindow(t *testing.T) {
	srcs := testSources(t)
	sig := Sign([]byte(testSecret), "999999999999", []byte(testBody))
	if _, _, err := Verify(srcs, testSourceID, "999999999999", sig, []byte(testBody), testNow, 15*time.Minute); !errors.Is(err, ErrStale) {
		t.Fatalf("err = %v, want ErrStale", err)
	}
}

func TestVerify_WindowEdgeAccepted(t *testing.T) {
	srcs := testSources(t)
	for _, at := range []time.Time{testNow.Add(-testWindow), testNow.Add(testWindow)} {
		sig := Sign([]byte(testSecret), ts(at), []byte(testBody))
		if _, _, err := Verify(srcs, testSourceID, ts(at), sig, []byte(testBody), testNow, testWindow); err != nil {
			t.Fatalf("at the window edge (%s): %v", at.Sub(testNow), err)
		}
	}
}

func TestVerify_NonPositiveWindowUsesDefault(t *testing.T) {
	srcs := testSources(t)
	inside := testNow.Add(-DefaultReplayWindow + time.Minute)
	outside := testNow.Add(-DefaultReplayWindow - time.Minute)
	for _, w := range []time.Duration{0, -time.Second} {
		sig := Sign([]byte(testSecret), ts(inside), []byte(testBody))
		if _, _, err := Verify(srcs, testSourceID, ts(inside), sig, []byte(testBody), testNow, w); err != nil {
			t.Fatalf("window %s, inside default: %v", w, err)
		}
		sig = Sign([]byte(testSecret), ts(outside), []byte(testBody))
		if _, _, err := Verify(srcs, testSourceID, ts(outside), sig, []byte(testBody), testNow, w); !errors.Is(err, ErrStale) {
			t.Fatalf("window %s, outside default: err = %v, want ErrStale", w, err)
		}
	}
}

// Order: a stale request that is ALSO mis-signed reports the signature, and
// an unsigned stale one reports the missing signature.
func TestVerify_SignatureCheckedBeforeWindow(t *testing.T) {
	srcs := testSources(t)
	stale := testNow.Add(-time.Hour)
	other := make([]byte, 32)
	if _, err := rand.Read(other); err != nil {
		t.Fatal(err)
	}
	misSigned := Sign(other, ts(stale), []byte(testBody))
	if _, _, err := Verify(srcs, testSourceID, ts(stale), misSigned, []byte(testBody), testNow, testWindow); !errors.Is(err, ErrSignatureInvalid) {
		t.Fatalf("stale+mis-signed: err = %v, want ErrSignatureInvalid", err)
	}
	if _, _, err := Verify(srcs, testSourceID, ts(stale), "", []byte(testBody), testNow, testWindow); !errors.Is(err, ErrSignatureMissing) {
		t.Fatalf("stale+unsigned: err = %v, want ErrSignatureMissing", err)
	}
}

func TestVerify_ErrorsAreDistinct(t *testing.T) {
	errs := []error{ErrSignatureMissing, ErrTimestampInvalid, ErrSignatureInvalid, ErrStale}
	for i, a := range errs {
		for j, b := range errs {
			if i != j && errors.Is(a, b) {
				t.Fatalf("%v matches %v", a, b)
			}
		}
	}
}
