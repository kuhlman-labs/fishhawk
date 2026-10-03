package permdrift

import (
	"reflect"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

func grant(key, value string, rank int) Entry {
	return Entry{Key: key, Value: value, Rank: rank, Polarity: Grant}
}

func restriction(key, value string, rank int) Entry {
	return Entry{Key: key, Value: value, Rank: rank, Polarity: Restriction}
}

func set(es ...Entry) Grants {
	g := Grants{}
	for _, e := range es {
		g.Put(e)
	}
	return g
}

// TestCompare pins every Compare branch. The wildcard rows are the
// subsumption control. COUNTERFACTUAL: make coveredBy return false (body
// mutation) — the row "base write-all, head {contents: write}" has a head-only
// key jobs.build.contents whose ONLY counterpart is the base wildcard
// jobs.build.*, so without subsumption it is reported Widened where the row
// asserts no widening, and the row goes RED (and symmetrically the
// "base {contents: write}, head write-all" row gains a spurious narrowing).
func TestCompare(t *testing.T) {
	cases := []struct {
		name       string
		base, head Grants
		want       []Change
	}{
		{"grant added", set(), set(grant("a", "write", 2)),
			[]Change{{Key: "a", Before: Absent, After: "write", Direction: Widened}}},
		{"grant removed", set(grant("a", "write", 2)), set(),
			[]Change{{Key: "a", Before: "write", After: Absent, Direction: Narrowed}}},
		{"restriction added", set(), set(restriction("r", Present, 1)),
			[]Change{{Key: "r", Before: Absent, After: Present, Direction: Narrowed}}},
		{"restriction removed", set(restriction("r", Present, 1)), set(),
			[]Change{{Key: "r", Before: Present, After: Absent, Direction: Widened}}},
		{"rank up", set(grant("a", "read", 1)), set(grant("a", "write", 2)),
			[]Change{{Key: "a", Before: "read", After: "write", Direction: Widened}}},
		{"rank down", set(grant("a", "write", 2)), set(grant("a", "read", 1)),
			[]Change{{Key: "a", Before: "write", After: "read", Direction: Narrowed}}},
		{"rank equal", set(grant("a", "write", 2)), set(grant("a", "write (other display)", 2)), nil},
		{"restriction rank up widens", set(restriction("c", "medium", 1)), set(restriction("c", "high", 2)),
			[]Change{{Key: "c", Before: "medium", After: "high", Direction: Widened}}},
		{"base write-all, head {contents: write}",
			set(grant("jobs.build.*", "write", 2)), set(grant("jobs.build.contents", "write", 2)),
			[]Change{{Key: "jobs.build.*", Before: "write", After: Absent, Direction: Narrowed}}},
		{"base read-all, head {contents: write} widens past the wildcard rank",
			set(grant("jobs.build.*", "read", 1)), set(grant("jobs.build.contents", "write", 2)),
			[]Change{
				{Key: "jobs.build.*", Before: "read", After: Absent, Direction: Narrowed},
				{Key: "jobs.build.contents", Before: Absent, After: "write", Direction: Widened},
			}},
		{"base {contents: write}, head write-all",
			set(grant("jobs.build.contents", "write", 2)), set(grant("jobs.build.*", "write", 2)),
			[]Change{{Key: "jobs.build.*", Before: Absent, After: "write", Direction: Widened}}},
		{"wildcard covers one segment only",
			set(grant("a.*", "write", 2)), set(grant("a.b.c", "write", 2)),
			[]Change{
				{Key: "a.*", Before: "write", After: Absent, Direction: Narrowed},
				{Key: "a.b.c", Before: Absent, After: "write", Direction: Widened},
			}},
		{"restriction wildcard never subsumes",
			set(restriction("a.*", Present, 1)), set(grant("a.b", Present, 1)),
			[]Change{
				{Key: "a.*", Before: Present, After: Absent, Direction: Widened},
				{Key: "a.b", Before: Absent, After: Present, Direction: Widened},
			}},
		// COUNTERFACTUAL (polarity change, item 1): mutate Compare's
		// `b.Polarity != h.Polarity` branch to fall through to the rank
		// comparison (body mutation). Both rows hold the key on both sides at
		// the SAME rank, so the rank comparison reports nothing and both rows
		// go RED (observed: `Compare = [] want [{Key:g ... widened}]`).
		{"restriction to grant at one key is widened",
			set(restriction("g", "not auto (gate override)", PresenceRank)), set(grant("g", "auto", PresenceRank)),
			[]Change{{Key: "g", Before: "not auto (gate override)", After: "auto", Direction: Widened}}},
		{"grant to restriction at one key is narrowed",
			set(grant("g", "auto", PresenceRank)), set(restriction("g", "not auto (gate override)", PresenceRank)),
			[]Change{{Key: "g", Before: "auto", After: "not auto (gate override)", Direction: Narrowed}}},
		{"polarity change wins over a rank move",
			set(restriction("g", "low", 0)), set(grant("g", "low", 0)),
			[]Change{{Key: "g", Before: "low", After: "low", Direction: Widened}}},
		{"deterministic order", set(), set(grant("z", "x", 1), grant("a", "x", 1), grant("m", "x", 1)),
			[]Change{
				{Key: "a", Before: Absent, After: "x", Direction: Widened},
				{Key: "m", Before: Absent, After: "x", Direction: Widened},
				{Key: "z", Before: Absent, After: "x", Direction: Widened},
			}},
		{"nil sets", nil, nil, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Compare(c.base, c.head)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("Compare =\n  %+v\nwant\n  %+v", got, c.want)
			}
		})
	}
}

func TestLevelsAndStrings(t *testing.T) {
	if r, ok := AppLevels.Rank("admin"); !ok || r != 3 {
		t.Errorf("AppLevels.Rank(admin) = %d, %v", r, ok)
	}
	if _, ok := ActionsLevels.Rank("admin"); ok {
		t.Errorf("ActionsLevels accepted admin")
	}
	if _, err := ShellLevels.mustRank("x", "bogus"); err == nil {
		t.Errorf("mustRank accepted an unknown level")
	}
	for _, c := range []struct{ got, want string }{
		{Widened.String(), "widened"}, {Narrowed.String(), "narrowed"}, {Direction(0).String(), "unknown"},
		{Grant.String(), "grant"}, {Restriction.String(), "restriction"},
	} {
		if c.got != c.want {
			t.Errorf("String() = %q, want %q", c.got, c.want)
		}
	}
}

// surfaceByID returns the product surface with id, failing the test when the
// list holds none.
func surfaceByID(t *testing.T, id string) Surface {
	t.Helper()
	for _, s := range DefaultSurfaces() {
		if s.ID == id {
			return s
		}
	}
	t.Fatalf("no product surface %q", id)
	return Surface{}
}

// TestKeySegment pins the file-derived segment escaping (item 10 wildcard
// half; approval condition 3's '.' clause).
//
// COUNTERFACTUAL: make keySegment return s unchanged (body mutation) — the
// "*" row keeps a bare '*' that ends in WildcardSuffix after a '.', the "a.b"
// row keeps a bare '.', the control-rune rows keep the rune, and the
// injectivity pairs collide; every arm goes RED (observed:
// `keySegment("*") = "*"`).
func TestKeySegment(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"contents", "contents"},
		{"write:runs", "write:runs"},
		{"*", "%2A"},
		{"a.b", "a%2Eb"},
		{"x.*", "x%2E%2A"},
		{"100%", "100%25"},
		{"%2A", "%252A"},
		{"a\nb", "a%0Ab"},
		{"rtl\u202eb", "rtl%E2%80%AEb"},
		{"ls\u2028x", "ls%E2%80%A8x"},
		{"zw\u200bx", "zw%E2%80%8Bx"},
		{"bad\xffbyte", "bad%FFbyte"},
		{"émoji✓", "émoji✓"},
	} {
		if got := keySegment(c.in); got != c.want {
			t.Errorf("keySegment(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// No escaped segment carries a '.', a control/format rune, or a wildcard
	// of its own making, and distinct inputs never share an escaping.
	inputs := []string{"*", ".*", "a.b", "a%2Eb", "a%2eb", "%", "%25", "x\n", "x%0A", "\u202e", "%E2%80%AE"}
	seen := map[string]string{}
	for _, in := range inputs {
		got := keySegment(in)
		if strings.ContainsRune(got, '.') || strings.HasSuffix("."+got, WildcardSuffix) || !utf8.ValidString(got) {
			t.Errorf("keySegment(%q) = %q carries a '.', a wildcard or invalid UTF-8", in, got)
		}
		for _, r := range got {
			if unicode.In(r, unicode.Cc, unicode.Cf, unicode.Zl, unicode.Zp) {
				t.Errorf("keySegment(%q) = %q carries control/format rune %U", in, got, r)
			}
		}
		if prev, dup := seen[got]; dup {
			t.Errorf("keySegment(%q) == keySegment(%q) == %q: not injective", in, prev, got)
		}
		seen[got] = in
	}
}
