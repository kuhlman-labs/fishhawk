package splitfiling

import (
	"strconv"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

const testRunID = "11111111-2222-3333-4444-555555555555"

// keyedChild builds an EpicChild whose body bears key. Bodies are stamped by
// the SHIPPED stamper, so a fixture can never drift from the detector.
func keyedChild(number int, key string) workmgmt.EpicChild {
	return workmgmt.EpicChild{
		Number: number,
		Title:  "phase child",
		Body:   workmgmt.StampIdempotencyKey("## Summary\n\nphase child\n", key),
		// A GitHub Enterprise Server host, deliberately: the adopted URL must
		// come from the forge, never be composed from a github.com prefix.
		URL: "https://ghe.example.com/o/r/issues/" + strconv.Itoa(number),
	}
}

// TestSplitKeys_NamespacedAndDeterministic pins that the child and comment key
// families never collide, that each is deterministic, and that the two comment
// kinds are distinct — the acceptance carrier and the #2412 refusal arise from
// the same window on the same thread, so a shared key would let one suppress
// the other.
func TestSplitKeys_NamespacedAndDeterministic(t *testing.T) {
	child0 := SplitChildKey(testRunID, 0)
	if child0 != SplitChildKey(testRunID, 0) {
		t.Fatalf("SplitChildKey is not deterministic")
	}
	if child0 == SplitChildKey(testRunID, 1) {
		t.Errorf("ordinals 0 and 1 share a key %q", child0)
	}
	if child0 == SplitChildKey("99999999-2222-3333-4444-555555555555", 0) {
		t.Errorf("two runs share ordinal-0 key %q", child0)
	}

	accept := SplitCommentKey(testRunID, CommentKindAcceptance)
	refuse := SplitCommentKey(testRunID, CommentKindRefusal)
	if accept != SplitCommentKey(testRunID, CommentKindAcceptance) {
		t.Fatalf("SplitCommentKey is not deterministic")
	}
	if accept == refuse {
		t.Errorf("acceptance and refusal comments share key %q", accept)
	}
	// Namespaces do not cross: a child key never equals a comment key.
	if accept == child0 || refuse == child0 {
		t.Errorf("child and comment key namespaces collide")
	}
}

// TestFindAdoptableChild_Hit is the positive control.
func TestFindAdoptableChild_Hit(t *testing.T) {
	key := SplitChildKey(testRunID, 1)
	children := []workmgmt.EpicChild{
		{Number: 10, Body: "## Summary\n\nunrelated\n"},
		keyedChild(11, key),
		{Number: 12, Body: "## Summary\n\nalso unrelated\n"},
	}
	got, ok := FindAdoptableChild(children, key)
	if !ok {
		t.Fatalf("no adoptable child found for %q", key)
	}
	if got.Number != 11 {
		t.Errorf("adopted #%d, want #11", got.Number)
	}
	if got.URL == "" {
		t.Errorf("adopted child carries no URL")
	}
}

// TestFindAdoptableChild_Miss: a child keyed for a DIFFERENT ordinal (bad state
// seeded by construction, not by calling the detector) is not adopted.
func TestFindAdoptableChild_Miss(t *testing.T) {
	sought := SplitChildKey(testRunID, 0)
	children := []workmgmt.EpicChild{
		keyedChild(10, SplitChildKey(testRunID, 1)),
		keyedChild(11, SplitChildKey("99999999-2222-3333-4444-555555555555", 0)),
		{Number: 12, Body: ""},
	}
	if got, ok := FindAdoptableChild(children, sought); ok {
		t.Fatalf("adopted #%d for a key no child bears", got.Number)
	}
	// Self-paired control: each of those children IS adoptable under its OWN
	// key, so the miss above is a real discrimination and not a broken matcher.
	if _, ok := FindAdoptableChild(children, SplitChildKey(testRunID, 1)); !ok {
		t.Errorf("child keyed for ordinal 1 was not adoptable under its own key")
	}
}

// TestFindAdoptableChild_PrefixNearMiss is the m4 unit-layer case, covering
// near-miss keys in BOTH directions. #10 bears a key that is a strict PREFIX of
// the sought key; #11 bears one that strictly EXTENDS it.
//
// Both directions are needed for this to DISCRIMINATE against a loosened
// (substring) matcher: such a matcher, looking for the full key, finds nothing
// inside the SHORTER one — so a prefix-only fixture stays green under the
// loosening — but does find it inside the LONGER one and wrongly adopts #11.
func TestFindAdoptableChild_PrefixNearMiss(t *testing.T) {
	full := SplitChildKey(testRunID, 0)
	prefix := full[:len(full)-4]
	extension := full + "deadbeef"
	if prefix == full {
		t.Fatal("fixture error: prefix is not strictly shorter")
	}
	children := []workmgmt.EpicChild{keyedChild(10, prefix), keyedChild(11, extension)}

	if got, ok := FindAdoptableChild(children, full); ok {
		t.Fatalf("adopted #%d on a near-miss key", got.Number)
	}
	// Self-paired: each child IS adoptable under the key it actually bears, so
	// the negative above is real discrimination and not a broken matcher.
	if _, ok := FindAdoptableChild(children, prefix); !ok {
		t.Errorf("prefix-keyed child not adoptable under its own key")
	}
	if _, ok := FindAdoptableChild(children, extension); !ok {
		t.Errorf("extension-keyed child not adoptable under its own key")
	}
}

// TestFindAdoptableChild_ClosedChildIsAdoptable is the m5 unit-layer case: a
// child closed before the re-approval was still FILED, so re-filing it would
// duplicate it. State must not gate adoption.
func TestFindAdoptableChild_ClosedChildIsAdoptable(t *testing.T) {
	key := SplitChildKey(testRunID, 2)
	closed := keyedChild(42, key)
	closed.Complete = true
	got, ok := FindAdoptableChild([]workmgmt.EpicChild{closed}, key)
	if !ok {
		t.Fatalf("closed child bearing the key was not adopted")
	}
	if got.Number != 42 {
		t.Errorf("adopted #%d, want #42", got.Number)
	}
}

// TestFindAdoptableChild_DuplicateKeyTieBreak pins the INTENTIONAL tie-break:
// when a prior concurrent re-approval left two children bearing the same key,
// the LOWEST NUMBER wins, deterministically and regardless of the order the
// forge returns them in. The second child is not reconciled here.
func TestFindAdoptableChild_DuplicateKeyTieBreak(t *testing.T) {
	key := SplitChildKey(testRunID, 0)
	ascending := []workmgmt.EpicChild{keyedChild(20, key), keyedChild(31, key)}
	descending := []workmgmt.EpicChild{keyedChild(31, key), keyedChild(20, key)}

	for name, children := range map[string][]workmgmt.EpicChild{
		"ascending": ascending, "descending": descending,
	} {
		got, ok := FindAdoptableChild(children, key)
		if !ok {
			t.Fatalf("%s: no child adopted", name)
		}
		if got.Number != 20 {
			t.Errorf("%s: adopted #%d, want the lowest-numbered #20", name, got.Number)
		}
	}
}

// TestFindAdoptableChild_EmptyKeyNeverAdopts: an unminted key must never adopt
// anything (a child with an empty body would otherwise "match").
func TestFindAdoptableChild_EmptyKeyNeverAdopts(t *testing.T) {
	if _, ok := FindAdoptableChild([]workmgmt.EpicChild{{Number: 1, Body: ""}}, ""); ok {
		t.Fatal("empty key adopted a child")
	}
	if _, ok := FindAdoptableChild(nil, SplitChildKey(testRunID, 0)); ok {
		t.Fatal("adopted from a nil child set")
	}
}

// TestThreadHasComment_HitMissAndWholeSlice covers the comment-side lookup: hit,
// miss, and — the load-bearing one — a match in the LAST element, so a helper
// that inspected only the head goes red here as well as in the server-level
// page-2 test.
func TestThreadHasComment_HitMissAndWholeSlice(t *testing.T) {
	key := SplitCommentKey(testRunID, CommentKindAcceptance)
	stamped := StampComment("Fishhawk filed the phased children.", key)

	if !ThreadHasComment([]string{stamped}, key) {
		t.Fatalf("single-element thread did not match")
	}
	if ThreadHasComment([]string{"unrelated", "also unrelated"}, key) {
		t.Errorf("unkeyed thread reported a match")
	}
	// LAST-element match: a head-only reader misses this.
	last := []string{"unrelated 1", "unrelated 2", "unrelated 3", stamped}
	if !ThreadHasComment(last, key) {
		t.Errorf("match in the LAST element was not found: %v", last)
	}
	// A comment keyed for the REFUSAL kind does not satisfy the ACCEPTANCE key.
	refusal := StampComment("Fishhawk did not file the children.", SplitCommentKey(testRunID, CommentKindRefusal))
	if ThreadHasComment([]string{refusal}, key) {
		t.Errorf("refusal-keyed comment matched the acceptance key")
	}
	// ...and it DOES match its own key (self-paired control).
	if !ThreadHasComment([]string{refusal}, SplitCommentKey(testRunID, CommentKindRefusal)) {
		t.Errorf("refusal-keyed comment did not match its own key")
	}
	if ThreadHasComment([]string{stamped}, "") {
		t.Errorf("empty key reported a match")
	}
}

// TestThreadHasComment_CRLFBody: a comment round-tripped through the GitHub web
// interface can come back with \r\n line endings. The detector normalizes them,
// so the dedup still fires.
func TestThreadHasComment_CRLFBody(t *testing.T) {
	key := SplitCommentKey(testRunID, CommentKindAcceptance)
	// The trailing "\n" is load-bearing — see the workmgmt CRLF test: the marker
	// is the LAST line, so without it the conversion leaves the marker line with
	// no \r and the case would pass whether or not the normalization exists.
	lf := StampComment("Fishhawk filed the phased children.\n\nSecond paragraph.", key) + "\n"
	crlf := strings.ReplaceAll(lf, "\n", "\r\n")
	if !strings.HasSuffix(crlf, "-->\r\n") {
		t.Fatalf("fixture error: the marker line does not end \\r\\n: %q", crlf)
	}

	if !ThreadHasComment([]string{lf}, key) {
		t.Fatalf("LF fixture did not match (fixture broken)")
	}
	if !ThreadHasComment([]string{crlf}, key) {
		t.Errorf("CRLF-bodied comment did not match: %q", crlf)
	}
}

// TestStampComment_IsStampOnce: composing over the workmgmt stamper keeps the
// stamp-once property, so a re-rendered comment body never carries two markers.
func TestStampComment_IsStampOnce(t *testing.T) {
	key := SplitCommentKey(testRunID, CommentKindRefusal)
	once := StampComment("refused", key)
	if twice := StampComment(once, key); twice != once {
		t.Fatalf("re-stamping changed the body:\nonce:  %q\ntwice: %q", once, twice)
	}
}
