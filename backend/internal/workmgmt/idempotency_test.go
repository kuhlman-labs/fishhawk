package workmgmt

import (
	"strings"
	"testing"
)

// TestMintIdempotencyKey_Deterministic pins the property the whole
// query-before-file mechanism rests on: the same inputs re-derive a
// BYTE-IDENTICAL key on a later pass. If minting ever became clock- or
// randomness-dependent, a re-approval would mint a key that matches nothing on
// the forge and would re-file every child — the exact bug #2064 closes.
func TestMintIdempotencyKey_Deterministic(t *testing.T) {
	a := MintIdempotencyKey("split-child", "run-abc", "0")
	b := MintIdempotencyKey("split-child", "run-abc", "0")
	if a != b {
		t.Fatalf("re-derived key differs: %q vs %q", a, b)
	}
	if a == "" {
		t.Fatal("minted key is empty")
	}
	// A different ordinal is a different key.
	if other := MintIdempotencyKey("split-child", "run-abc", "1"); other == a {
		t.Errorf("ordinal 0 and 1 minted the SAME key %q", a)
	}
	// A different run is a different key.
	if other := MintIdempotencyKey("split-child", "run-xyz", "0"); other == a {
		t.Errorf("run-abc and run-xyz minted the SAME key %q", a)
	}
	// A different namespace is a different key.
	if other := MintIdempotencyKey("split-comment", "run-abc", "0"); other == a {
		t.Errorf("distinct namespaces minted the SAME key %q", a)
	}
	// The namespace is carried in the clear so a raw body names the mechanism.
	if !strings.HasPrefix(a, "split-child:") {
		t.Errorf("key %q does not carry its namespace in the clear", a)
	}
}

// TestMintIdempotencyKey_PartBoundariesAreNotForgeable proves the NUL-separated
// join: two different part splits that concatenate to the same string mint
// DIFFERENT keys, so ("run-ab", "1") cannot collide with ("run-a", "b1").
func TestMintIdempotencyKey_PartBoundariesAreNotForgeable(t *testing.T) {
	a := MintIdempotencyKey("ns", "run-ab", "1")
	b := MintIdempotencyKey("ns", "run-a", "b1")
	if a == b {
		t.Fatalf("part boundary forged: both splits minted %q", a)
	}
}

// TestMintIdempotencyKey_NamespaceSanitizedToMarkerSafeSet proves a namespace
// carrying a newline or the HTML-comment terminator cannot break the marker
// line it is embedded in — which would defeat the whole-line match outright.
func TestMintIdempotencyKey_NamespaceSanitizedToMarkerSafeSet(t *testing.T) {
	key := MintIdempotencyKey("bad ns\n-->", "x")
	if strings.ContainsAny(key, "\n\r<>") || strings.Contains(key, " ") {
		t.Fatalf("key %q carries a marker-breaking character", key)
	}
	// It still round-trips through stamp/detect.
	body := StampIdempotencyKey("## Summary\n", key)
	if !BodyHasIdempotencyKey(body, key) {
		t.Errorf("sanitized-namespace key did not round-trip: %q", body)
	}
}

// TestStampIdempotencyKey_StampOnce is the c4 counterfactual vehicle: stamping
// an already-stamped body must leave EXACTLY ONE marker. Deleting the
// stamp-once guard in StampIdempotencyKey reddens this on the marker count.
func TestStampIdempotencyKey_StampOnce(t *testing.T) {
	key := MintIdempotencyKey("split-child", "run-abc", "0")
	once := StampIdempotencyKey("## Summary\n\ndo the thing\n", key)
	twice := StampIdempotencyKey(once, key)

	if got := strings.Count(twice, idempotencyMarker(key)); got != 1 {
		t.Fatalf("double-stamped body carries %d markers, want exactly 1:\n%s", got, twice)
	}
	if twice != once {
		t.Errorf("re-stamping an already-keyed body changed it:\nonce:  %q\ntwice: %q", once, twice)
	}
	if !BodyHasIdempotencyKey(twice, key) {
		t.Errorf("stamped body does not carry its own key: %q", twice)
	}
	// The original prose survives the stamp.
	if !strings.Contains(twice, "do the thing") {
		t.Errorf("stamp destroyed the body prose: %q", twice)
	}
}

// TestStampIdempotencyKey_EmptyKeyIsNoOp pins the inert-by-default posture: a
// FilingRequest without an IdempotencyKey must render byte-identically to
// today's body.
func TestStampIdempotencyKey_EmptyKeyIsNoOp(t *testing.T) {
	body := "## Summary\n\ndo the thing\n"
	if got := StampIdempotencyKey(body, ""); got != body {
		t.Fatalf("empty key mutated the body: %q", got)
	}
	if BodyHasIdempotencyKey(body, "") {
		t.Errorf("empty key must never report a match")
	}
}

// TestStampIdempotencyKey_EmptyBodyYieldsBareMarker covers the degenerate body:
// stamping an empty body must not emit leading blank lines that a later
// whole-line match would still have to tolerate.
func TestStampIdempotencyKey_EmptyBodyYieldsBareMarker(t *testing.T) {
	key := MintIdempotencyKey("ns", "x")
	got := StampIdempotencyKey("   \n", key)
	if got != idempotencyMarker(key) {
		t.Fatalf("stamped empty body = %q, want the bare marker", got)
	}
	if !BodyHasIdempotencyKey(got, key) {
		t.Errorf("bare marker does not detect")
	}
}

// TestBodyHasIdempotencyKey_WholeLineMatching is the c3 counterfactual vehicle.
// Every case here pairs an input with the key that would match it under a
// LOOSENED (substring / prefix) matcher but must NOT match under the shipped
// whole-line matcher.
func TestBodyHasIdempotencyKey_WholeLineMatching(t *testing.T) {
	key := MintIdempotencyKey("split-child", "run-abc", "0")
	stamped := StampIdempotencyKey("## Summary\n", key)

	// The stamped key itself matches (the positive control — without it a
	// matcher that always returned false would pass the negatives below).
	if !BodyHasIdempotencyKey(stamped, key) {
		t.Fatalf("stamped key does not match its own body: %q", stamped)
	}

	// A key that is a STRICT PREFIX of the stamped key must NOT match. A
	// substring/prefix matcher adopts the wrong child here.
	prefix := key[:len(key)-4]
	if prefix == key {
		t.Fatal("fixture error: prefix is not strictly shorter than the key")
	}
	if BodyHasIdempotencyKey(stamped, prefix) {
		t.Errorf("strict-prefix key %q matched a body stamped with %q", prefix, key)
	}

	// A key that EXTENDS the stamped key must not match either.
	if BodyHasIdempotencyKey(stamped, key+"deadbeef") {
		t.Errorf("extended key matched a body stamped with %q", key)
	}

	// The key appearing inside PROSE, not as its own marker line, must not
	// match. Note the body is stamped with NOTHING — the only occurrence of the
	// key text is the prose mention.
	prose := "## Summary\n\nSee also the key " + key + " mentioned inline.\n"
	if BodyHasIdempotencyKey(prose, key) {
		t.Errorf("prose mention of the key matched: %q", prose)
	}

	// A marker line for a DIFFERENT key does not match this key.
	otherKey := MintIdempotencyKey("split-child", "run-abc", "1")
	if BodyHasIdempotencyKey(StampIdempotencyKey("## Summary\n", otherKey), key) {
		t.Errorf("a body stamped with %q matched %q", otherKey, key)
	}
	// ...and the other key DOES match its own body (self-paired control, so the
	// negative above is not passing because the matcher is simply broken).
	if !BodyHasIdempotencyKey(StampIdempotencyKey("## Summary\n", otherKey), otherKey) {
		t.Errorf("a body stamped with %q did not match itself", otherKey)
	}
}

// TestBodyHasIdempotencyKey_CRLFNormalization is the c6 counterfactual vehicle
// at the unit layer: a body whose marker line ends \r\n — the shape GitHub
// returns for content authored or edited through the web interface — must still
// match. Deleting the normalization reddens this.
func TestBodyHasIdempotencyKey_CRLFNormalization(t *testing.T) {
	key := MintIdempotencyKey("split-comment", "run-abc", "acceptance")
	// The trailing "\n" is load-bearing: the marker is the LAST line, so without
	// it the conversion below would leave the marker line with no \r at all and
	// the case would pass whether or not the normalization exists.
	lf := StampIdempotencyKey("## Summary\n\nprose\n", key) + "\n"
	crlf := strings.ReplaceAll(lf, "\n", "\r\n")
	if !strings.Contains(crlf, idempotencyMarker(key)+"\r\n") {
		t.Fatalf("fixture error: the marker line does not end \\r\\n: %q", crlf)
	}

	// Self-paired: the SAME body content, differing only in line endings.
	if !BodyHasIdempotencyKey(lf, key) {
		t.Fatalf("LF body did not match (fixture broken): %q", lf)
	}
	if !BodyHasIdempotencyKey(crlf, key) {
		t.Errorf("CRLF body did not match: %q", crlf)
	}
	// A bare-CR trailing byte on the marker line is absorbed too.
	if !BodyHasIdempotencyKey("## Summary\r\n\r\n"+idempotencyMarker(key)+"\r", key) {
		t.Errorf("bare-CR-terminated marker line did not match")
	}
}
